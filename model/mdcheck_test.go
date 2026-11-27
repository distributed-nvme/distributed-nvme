package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestUpdateMdChecks: the op applies each edit whose group still holds its
// From, in one STM with one SpRev bump, and returns the count and the
// revision; an edit whose From no longer matches — another owner's write
// came first — is skipped, as is a ref to a group or a parent that is gone,
// and a call that applies nothing writes nothing at all.
func TestUpdateMdChecks(t *testing.T) {
	env := newOpsEnv(t)
	meta := GrpRef{Parent: opsSliceParent, GrpId: opsMetaGrpId}
	data := GrpRef{Parent: opsSliceParent, GrpId: opsDataGrpId}
	stamp := &pb.MdCheck{NextEpoch: 1000}
	before := env.spRev()
	applied, revision, err := UpdateMdChecks(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, []MdCheckEdit{
			{Ref: meta, From: nil, To: stamp},
			{Ref: data, From: nil, To: stamp},
			{Ref: GrpRef{Parent: opsSliceParent, GrpId: 999}, To: stamp},
			{Ref: GrpRef{Parent: GrpParent{SliceId: 999},
				GrpId: opsDataGrpId}, To: stamp},
		})
	if err != nil || applied != 2 || revision != before+1 ||
		env.spRev() != before+1 {
		t.Fatalf("the stamp: applied %d, revision %d, err %v, SpRev %d; "+
			"want 2 at %d", applied, revision, err, env.spRev(), before+1)
	}
	for _, grpId := range []uint64{opsMetaGrpId, opsDataGrpId} {
		if got := findGroup(env.slice(), grpId).GetMdCheck(); !SameMdCheck(
			got, stamp) {
			t.Errorf("group %d schedule %v, want %v", grpId, got, stamp)
		}
	}

	// A request from the read schedule applies; a second owner's request
	// from the same read finds the schedule moved and writes nothing.
	request := &pb.MdCheck{NextEpoch: 1000, RequestEpoch: 2000}
	edit := []MdCheckEdit{{Ref: data, From: stamp, To: request}}
	applied, _, err = UpdateMdChecks(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, edit)
	if err != nil || applied != 1 {
		t.Fatalf("the request: applied %d, err %v; want 1", applied, err)
	}
	store := env.storeRev()
	applied, revision, err = UpdateMdChecks(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, edit)
	if err != nil || applied != 0 || revision != 0 ||
		env.storeRev() != store {
		t.Fatalf("the second owner's request: applied %d, revision %d, "+
			"err %v; want no write", applied, revision, err)
	}
	if got := findGroup(env.slice(), opsDataGrpId).GetMdCheck(); !SameMdCheck(
		got, request) {
		t.Errorf("the data group's schedule %v, want %v", got, request)
	}

	// One check at a time per pool: a second owner's request of another
	// group, from a read that saw no request, is skipped while the data
	// group's stands, and goes in once it is ended in the same call.
	second := &pb.MdCheck{NextEpoch: 1000, RequestEpoch: 3000}
	store = env.storeRev()
	applied, _, err = UpdateMdChecks(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []MdCheckEdit{{Ref: meta, From: stamp,
			To: second}})
	if err != nil || applied != 0 || env.storeRev() != store {
		t.Fatalf("a second request: applied %d, err %v; want no write",
			applied, err)
	}
	applied, _, err = UpdateMdChecks(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []MdCheckEdit{
			{Ref: data, From: request, To: &pb.MdCheck{NextEpoch: 5000}},
			{Ref: meta, From: stamp, To: second},
		})
	if err != nil || applied != 2 {
		t.Fatalf("the end and the next request: applied %d, err %v; want 2",
			applied, err)
	}
}

// TestUpdateMdChecksKeepsTheDoneTime: the write stores done_epoch, the time
// the sp worker ended a request the primary reported done (dnv-worker.md
// AR16), and an edit whose from-schedule differs from the stored one in
// done_epoch alone writes nothing, so two owners cannot both record an end.
func TestUpdateMdChecksKeepsTheDoneTime(t *testing.T) {
	env := newOpsEnv(t)
	data := GrpRef{Parent: opsSliceParent, GrpId: opsDataGrpId}
	request := &pb.MdCheck{NextEpoch: 1000, RequestEpoch: 2000}
	ended := &pb.MdCheck{NextEpoch: 5000, DoneEpoch: 3000}
	for _, edit := range [][]MdCheckEdit{
		{{Ref: data, From: nil, To: request}},
		{{Ref: data, From: request, To: ended}},
	} {
		if applied, _, err := UpdateMdChecks(env.ctx, env.cli, env.cid,
			opsShard, opsSpId, opsSpName, edit); err != nil || applied != 1 {
			t.Fatalf("edit %v: applied %d, err %v; want 1", edit, applied,
				err)
		}
	}
	got := findGroup(env.slice(), opsDataGrpId).GetMdCheck()
	if got.GetDoneEpoch() != 3000 || !SameMdCheck(got, ended) {
		t.Fatalf("the schedule %v, want %v", got, ended)
	}
	stale := &pb.MdCheck{NextEpoch: 5000}
	store := env.storeRev()
	applied, _, err := UpdateMdChecks(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []MdCheckEdit{{Ref: data, From: stale,
			To: &pb.MdCheck{NextEpoch: 5000, RequestEpoch: 6000}}})
	if err != nil || applied != 0 || env.storeRev() != store {
		t.Fatalf("an edit from a schedule without the done time: applied "+
			"%d, err %v; want no write", applied, err)
	}
}
