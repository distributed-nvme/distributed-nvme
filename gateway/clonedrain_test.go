package gateway

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The gateway half of the clone drain (gateway.md §5.8, dnv-worker.md §11.7):
// DeleteClone latches, and the chunk sweep that used to sit in its deciding STM
// is the worker's.
//
// As for the sp drain, the END-state assertions did not move with it, so the
// tests that own them run the drain here through volDrainClone, exactly as the
// sp coordinator would.

// volDrainCloneSteps bounds volDrainClone. A max-shape drain is
// ceil(MaxSliceCntPerSp*MaxCloneBmCnt / MaxDelBmPerTxn) + 1 = 5 steps; 64 is
// slack by an order of magnitude and turns a stalled drain into a failure
// rather than a hang.
const volDrainCloneSteps = 64

// volChunks is the keys-only scan MD3 performs, in ascending
// (src_slice_idx, bm_idx) order — the surviving chunk set a drain step derives
// its position from (CLD7).
func volChunks(env *volEnv, cloneName string) []model.BmChunk {
	env.t.Helper()
	prefix := model.CloneBitmapPrefix(env.cid, volSpId, cloneName)
	keys, _, err := env.cli.RangeKeys(env.ctx, prefix)
	if err != nil {
		env.t.Fatalf("RangeKeys %q: %v", prefix, err)
	}
	out := make([]model.BmChunk, 0, len(keys))
	for _, entry := range keys {
		sliceIdx, bmIdx, ok := model.ParseCloneBmKey(entry.Key)
		if !ok {
			continue
		}
		out = append(out, model.BmChunk{
			SliceIdx: sliceIdx, Idx: bmIdx, ModRev: entry.ModRev,
		})
	}
	return out
}

// volDrainClone runs one latched clone's drain to completion: CLD7's
// derivation in a loop, with no pass and no timer.
func volDrainClone(env *volEnv, cloneName string) {
	env.t.Helper()
	for step := 0; step < volDrainCloneSteps; step++ {
		clone := &pb.Clone{}
		if !env.exists(model.CloneKey(env.cid, volSpId, cloneName), clone) {
			// The final STM committed.
			return
		}
		env.get(model.CloneKey(env.cid, volSpId, cloneName), clone)
		if !clone.GetDeleting() {
			env.t.Fatalf("volDrainClone: %q is not latched", cloneName)
		}
		chunks := volChunks(env, cloneName)
		var err error
		if len(chunks) == 0 {
			err = model.FinishCloneDelete(
				env.ctx, env.cli, env.cid, volShard, volSpId, volSpName,
				cloneName, clone.GetCloneId())
		} else {
			batch := chunks
			if len(batch) > common.MaxDelBmPerTxn {
				batch = batch[:common.MaxDelBmPerTxn]
			}
			_, err = model.DrainCloneBm(
				env.ctx, env.cli, env.cid, volShard, volSpId, volSpName,
				cloneName, clone.GetCloneId(), batch)
		}
		if err != nil {
			env.t.Fatalf("volDrainClone %q step %d: %v", cloneName, step, err)
		}
	}
	env.t.Fatalf("volDrainClone: %q did not finish in %d steps",
		cloneName, volDrainCloneSteps)
}

// volCloneKeyCnt counts the surviving chunk keys of one clone.
func volCloneKeyCnt(env *volEnv, cloneName string) int {
	return len(volChunks(env, cloneName))
}

// TestDeleteCloneLatchesOnly is CLD4's whole write set: the Clone put with
// `deleting = true`, the destination namespaces resumed, one SpRev bump — and
// nothing else. No chunk key is touched and the name stays in
// `clone_name_list`.
//
// The name is the load-bearing half: model.LoadSp fetches clones by iterating
// that list, so a latch that removed the name would break every subsequent
// load of the SP — the sp drain's slice-final rule, applying verbatim.
func TestDeleteCloneLatchesOnly(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{
		{NsId: 601, NsIdx: 1, TdId: 900, Suspended: true},
	})
	reply := volCreateClone(env, "clone-a", "dst")
	for _, pair := range [][2]uint32{{0, 0}, {0, 2}, {3, 1}} {
		volAppendBm(env, "clone-a", pair[0], pair[1])
	}
	before := env.clone("clone-a")
	beforeRev := env.spRev()

	got, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		CloneName:   "clone-a",
		Force:       true,
	})
	if err != nil {
		t.Fatalf("DeleteClone: %v", err)
	}
	if got.GetCloneId() != reply.GetCloneId() {
		t.Errorf("clone_id: got %d, want %d",
			got.GetCloneId(), reply.GetCloneId())
	}
	clone := env.clone("clone-a")
	if !clone.GetDeleting() {
		t.Errorf("deleting must be true after the latch")
	}
	// Every other field is untouched — the latch is a flag, not a rewrite.
	// proto.Equal against the pre-latch record with `deleting` set is a
	// stronger statement than any field-by-field compare: it fails on a field
	// the latch had no business touching, including one added later.
	wantAfter := proto.Clone(before).(*pb.Clone)
	wantAfter.Deleting = true
	if !proto.Equal(clone, wantAfter) {
		t.Errorf("the latch rewrote the clone: got %v, want %v",
			clone, wantAfter)
	}
	if n := volCloneKeyCnt(env, "clone-a"); n != 3 {
		t.Errorf("%d chunk keys survive the latch, want 3", n)
	}
	if got := env.spConf().GetCloneNameList(); len(got) != 1 ||
		got[0] != "clone-a" {
		t.Errorf("clone_name_list: got %v, want the name to survive", got)
	}
	if got := env.spRev(); got != beforeRev+1 {
		t.Errorf("sp_rev: got %d, want %d (exactly one bump)",
			got, beforeRev+1)
	}
	// CLD4: the unsuspend rides the LATCH, not the final STM. Otherwise CN16's
	// auto_resume override vanishes when the clone leaves the plan while etcd
	// still says suspended, and the destination namespace goes dark for the
	// whole drain — a host-visible outage the one-shot never had.
	if findNs(env.subsystem(volNqn), 1).GetSuspended() {
		t.Errorf("the dst namespace must resume with the latch, not later")
	}

	volDrainClone(env, "clone-a")
	if env.exists(model.CloneKey(env.cid, volSpId, "clone-a"), &pb.Clone{}) {
		t.Errorf("the clone key survived the drain")
	}
	if n := volCloneKeyCnt(env, "clone-a"); n != 0 {
		t.Errorf("%d chunk keys survived the drain", n)
	}
	if got := env.spConf().GetCloneNameList(); len(got) != 0 {
		t.Errorf("clone_name_list after the drain: got %v, want empty", got)
	}
}

// volAppendBm appends one byte to a clone's chunk (s, b).
func volAppendBm(env *volEnv, name string, sliceIdx uint32, bmIdx uint32) {
	env.t.Helper()
	_, err := env.srv.AppendCloneBitmap(env.ctx, &pb.AppendCloneBitmapRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		CloneName:   name,
		SrcSliceIdx: sliceIdx,
		BmIdx:       bmIdx,
		Bitmap:      []byte{0xff},
	})
	if err != nil {
		env.t.Fatalf("AppendCloneBitmap (%d, %d): %v", sliceIdx, bmIdx, err)
	}
}

// TestDeleteCloneRepeatIsANoOp is CLD3, pinned in four directions: the first
// delete bumps, the repeat does not, the repeat answers the SAME clone_id, and
// the repeat makes NO agent call.
//
// The call count is the one that matters most and is easiest to leave out. The
// short-circuit sits BEFORE the between-phase `GetCntlrInfo` precisely because
// after the latch the CN has retired the stack, so the hydration check would
// find no dm-clone and wedge every repeat delete in FAILED_PRECONDITION for
// ever. A short-circuit placed in phase 2 instead would pass every assertion
// here except the call count.
func TestDeleteCloneRepeatIsANoOp(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	// The primary moves to a fake agent, so an agent call is observable at
	// all: volEnv's own cntlr addresses are not dialable.
	addr := fakeAddrPort(t, "cn")
	agent := startFakeAgent(t, addr, 1<<40)
	env.putCn(addr, 802, 2)
	mustPut(t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrA), &pb.Cntlr{
		AddrPort:   addr,
		NvmeTrConf: volTrConf(addr),
		Primary:    true,
	})
	reply := volCreateClone(env, "clone-a", "dst")
	// A complete dm-clone status, so the FIRST (unforced) delete is allowed
	// through: this test is about the repeat, not about the gate.
	agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
		CloneIdToDmClone: map[uint64]*pb.ResInfo{
			reply.GetCloneId(): {Details: "0 2097152 clone 253:0 253:1 " +
				"253:2 1024/1024 0 1 no_hydration"},
		},
	})
	beforeRev := env.spRev()

	if _, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		CloneName:   "clone-a",
	}); err != nil {
		t.Fatalf("DeleteClone: %v", err)
	}
	if got := env.spRev(); got != beforeRev+1 {
		t.Fatalf("the first delete must bump: sp_rev %d, want %d",
			got, beforeRev+1)
	}
	if got := agent.callCount("GetCntlrInfo"); got != 1 {
		t.Fatalf("GetCntlrInfo calls for the first delete = %d, want 1", got)
	}

	before := env.spConf()
	latchRev := env.spRev()
	for _, force := range []bool{false, true} {
		got, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: latchRev},
			CloneName:   "clone-a",
			Force:       force,
		})
		if err != nil {
			t.Fatalf("repeat DeleteClone (force=%v): %v", force, err)
		}
		// The no-op answers the SAME reply as the delete that latched. It is
		// the only thing the short-circuit returns, and a caller that reads
		// `clone_id` out of a retry — the shape a retrying script has — must
		// not get a zero from the second attempt.
		if got.GetCloneId() != reply.GetCloneId() {
			t.Errorf("repeat DeleteClone (force=%v) clone_id: got %d, want %d",
				force, got.GetCloneId(), reply.GetCloneId())
		}
	}
	if got := agent.callCount("GetCntlrInfo"); got != 1 {
		t.Errorf("a repeat delete called GetCntlrInfo: %d calls, want 1", got)
	}
	if got := env.spRev(); got != latchRev {
		t.Errorf("a repeat delete bumped sp_rev to %d, want %d",
			got, latchRev)
	}
	env.wantUntouched(before, latchRev)
	// A stale token still ABORTs on a latched clone: §5.8's table order puts
	// GW6 ahead of the `deleting` row, and the short-circuit runs the check
	// itself because phase 1 is where it decides.
	_, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: latchRev + 99},
		CloneName:   "clone-a",
	})
	volWantCode(t, err, codes.Aborted)
	if got := agent.callCount("GetCntlrInfo"); got != 1 {
		t.Errorf("a stale-token delete called GetCntlrInfo: %d calls", got)
	}
}

// TestDeleteCloneRacedLatchIsANoOp is CLD3's RACED variant, the branch the
// repeat test above cannot reach: the flag goes up after phase 1 read the
// clone live, so the deciding STM is the first place this call can see it.
//
// The window is not an instant — it is the whole between-phases
// `GetCntlrInfo` round trip — and it is exactly what two operators, or an
// operator and a retrying script, produce. Without the phase-2 check the loser
// would re-latch: a second `SpRev` bump invalidating every client's token, a
// second `resumeCloneDstNs` on namespaces the winner already resumed, and a
// Clone put racing the drain's own batches.
//
// The loser sends NO `sp_rev`, because GW6 is presence-based: with a token it
// would be ABORTED on the winner's bump and never reach the branch at all.
// That is also the ordinary dnvctl shape, `--rev` omitted.
func TestDeleteCloneRacedLatchIsANoOp(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	addr := fakeAddrPort(t, "cn")
	agent := startFakeAgent(t, addr, 1<<40)
	env.putCn(addr, 802, 2)
	mustPut(t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrA), &pb.Cntlr{
		AddrPort:   addr,
		NvmeTrConf: volTrConf(addr),
		Primary:    true,
	})
	reply := volCreateClone(env, "clone-a", "dst")
	agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
		CloneIdToDmClone: map[uint64]*pb.ResInfo{
			reply.GetCloneId(): {Details: "0 2097152 clone 253:0 253:1 " +
				"253:2 1024/1024 0 1 no_hydration"},
		},
	})

	// Hold the loser inside the agent call, which is where the window is.
	unblock := agent.setBlock()
	type answer struct {
		reply *pb.DeleteCloneReply
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			CloneName:   "clone-a",
		})
		done <- answer{got, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for agent.callCount("GetCntlrInfo") == 0 {
		if time.Now().After(deadline) {
			unblock()
			t.Fatalf("the loser never reached the agent call")
		}
		time.Sleep(time.Millisecond)
	}
	// The winner latches while the loser is parked between its phases.
	volLatchClone(env, "clone-a")
	latchRev := env.spRev()
	before := env.spConf()
	clone := env.clone("clone-a")
	unblock()

	got := <-done
	if got.err != nil {
		t.Fatalf("the raced delete must be an OK no-op: %v", got.err)
	}
	if got.reply.GetCloneId() != reply.GetCloneId() {
		t.Errorf("clone_id: got %d, want %d",
			got.reply.GetCloneId(), reply.GetCloneId())
	}
	if rev := env.spRev(); rev != latchRev {
		t.Errorf("the loser bumped sp_rev to %d, want %d (no writes)",
			rev, latchRev)
	}
	env.wantUntouched(before, latchRev)
	if after := env.clone("clone-a"); !proto.Equal(after, clone) {
		t.Errorf("the loser rewrote the clone: got %v, want %v", after, clone)
	}
}

// TestCloneMutatorsRefuseADeletingClone is CLD1, mutation-tested in BOTH
// directions: every clone mutator but DeleteClone refuses a latched clone, and
// the same call on a live one succeeds.
//
// The AppendCloneBitmap half is not cosmetic. Without it a racing append could
// write a chunk key behind the drain, and CLD9's emptiness guard — which rests
// on "after the latch no chunk key can ever appear again" — would stop being
// stable, leaving an orphaned chunk key under a clone name that no longer
// exists.
func TestCloneMutatorsRefuseADeletingClone(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(env *volEnv) error
	}{
		{"AppendCloneBitmap", func(env *volEnv) error {
			_, err := env.srv.AppendCloneBitmap(
				env.ctx, &pb.AppendCloneBitmapRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       env.token(),
					CloneName:   "clone-a",
					SrcSliceIdx: 0,
					BmIdx:       0,
					Bitmap:      []byte{0xff},
				})
			return err
		}},
		{"UpdateCloneTrConf", func(env *volEnv) error {
			_, err := env.srv.UpdateCloneTrConf(
				env.ctx, &pb.UpdateCloneTrConfRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       env.token(),
					CloneName:   "clone-a",
					SrcTrConf:   []*pb.NvmeTrConf{volTrConf(volCnB)},
				})
			return err
		}},
	} {
		t.Run(tc.name+"/on a live clone", func(t *testing.T) {
			env := newVolEnv(t)
			env.putTd("dst", 900, 7, 0, true)
			volCreateClone(env, "clone-a", "dst")
			if err := tc.call(env); err != nil {
				t.Fatalf("%s on a live clone: %v", tc.name, err)
			}
		})
		t.Run(tc.name+"/on a latched clone", func(t *testing.T) {
			env := newVolEnv(t)
			env.putTd("dst", 900, 7, 0, true)
			volCreateClone(env, "clone-a", "dst")
			volLatchClone(env, "clone-a")
			before := env.spConf()
			beforeRev := env.spRev()
			err := tc.call(env)
			msg := volWantCode(t, err, codes.FailedPrecondition)
			if msg != `clone "clone-a" is being deleted` {
				t.Errorf("message: got %q", msg)
			}
			env.wantUntouched(before, beforeRev)
		})
	}
}

// volLatchClone latches one clone through the RPC, with force so that no agent
// call is needed.
func volLatchClone(env *volEnv, name string) {
	env.t.Helper()
	_, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		CloneName:   name,
		Force:       true,
	})
	if err != nil {
		env.t.Fatalf("DeleteClone %q: %v", name, err)
	}
}

// TestCloneDrainConsequences pins what §5.8 states for the record — the things
// an operator or a script trips over, ALL of which follow from the Clone key
// and its `clone_name_list` entry surviving until the final STM:
//
//   - the name is NOT reusable until then;
//   - `DeleteStoragePool` keeps refusing while any clone drains, with the
//     count it still holds (`still holds 1 clones`);
//   - the DESTINATION thin device is held too: `DeleteThinDevice` of it
//     refuses, which the one-shot delete never did — so abandoning a clone is
//     delete-clone, poll until gone, then delete-td / delete-sp;
//   - and every one of them resumes the instant the drain finishes.
//
// The td half is the one a reader is most likely to miss, and the one with the
// worst shape: deleting the half-hydrated destination is the natural next step
// after abandoning a clone, and it is now the step that fails first.
func TestCloneDrainConsequences(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	createClone := func() error {
		_, err := env.srv.CreateClone(env.ctx, &pb.CreateCloneRequest{
			ClusterName:   env.cluster,
			SpName:        volSpName,
			SpRev:         env.token(),
			CloneName:     "clone-a",
			SrcTrConf:     []*pb.NvmeTrConf{volTrConf(volCnA)},
			SrcNqn:        volSrcNqn,
			SrcNsIdx:      1,
			SrcSliceCnt:   1,
			SrcStripeSize: 64 * 1024,
			SrcBlockSize:  1024 * 1024,
			DstTdName:     "dst",
			DmCloneConf:   &pb.DmCloneConf{HydrationThreshold: 2},
		})
		return err
	}
	deleteTd := func() error {
		_, err := env.srv.DeleteThinDevice(
			env.ctx, &pb.DeleteThinDeviceRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				TdName:      "dst",
			})
		return err
	}
	deleteSp := func() error {
		_, err := env.srv.DeleteStoragePool(
			env.ctx, &pb.DeleteStoragePoolRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
			})
		return err
	}
	volCreateClone(env, "clone-a", "dst")
	volAppendBm(env, "clone-a", 0, 0)
	volLatchClone(env, "clone-a")

	volWantCode(t, createClone(), codes.AlreadyExists)
	msg := volWantCode(t, deleteTd(), codes.FailedPrecondition)
	if msg != "thin device dst is the destination of clone clone-a" {
		t.Errorf("the td refusal must name the draining clone: got %q", msg)
	}

	// The five-list precondition is unchanged; what changed is how long the
	// name stays. It returns on the FIRST non-empty list, and the destination
	// td is one, so the td name is lifted out for this one call — otherwise
	// the assertion below would pass just as happily with the clones row
	// deleted from the check outright.
	conf := env.spConf()
	held := conf.GetTdNameList()
	conf.TdNameList = nil
	env.putSpConf(conf)
	msg = volWantCode(t, deleteSp(), codes.FailedPrecondition)
	if want := `storage pool "` + volSpName + `" still holds 1 clones`; msg !=
		want {
		t.Errorf("sp refusal: got %q, want %q", msg, want)
	}
	conf = env.spConf()
	conf.TdNameList = held
	env.putSpConf(conf)

	volDrainClone(env, "clone-a")

	// All three resume together, because all three read the same list.
	if err := createClone(); err != nil {
		t.Fatalf("CreateClone after the drain: %v", err)
	}
	volLatchClone(env, "clone-a")
	volDrainClone(env, "clone-a")
	if err := deleteTd(); err != nil {
		t.Fatalf("DeleteThinDevice after the drain: %v", err)
	}
	if err := deleteSp(); err != nil {
		t.Fatalf("DeleteStoragePool after the drain: %v", err)
	}
}

// TestCloneDrainAtTheChunkCeiling is CLD11's PROOF and the repurposed half of
// the ceiling test it replaced: the whole MaxSliceCntPerSp x MaxCloneBmCnt
// rectangle is filled — every chunk key the old one-shot could ever have had
// to sweep — and drained through the real batches, against the real etcd this
// package runs with --max-txn-ops=common.EtcdMaxTxnOps.
//
// What it proves is the property the arithmetic cannot: the 256 keys leave in
// ceil(256 / MaxDelBmPerTxn) transactions of a constant size, not in one whose
// size is the rectangle.
func TestCloneDrainAtTheChunkCeiling(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	if _, err := env.srv.CreateClone(env.ctx, &pb.CreateCloneRequest{
		ClusterName:   env.cluster,
		SpName:        volSpName,
		SpRev:         env.token(),
		CloneName:     "clone-max",
		SrcTrConf:     []*pb.NvmeTrConf{volTrConf(volCnA)},
		SrcNqn:        volSrcNqn,
		SrcNsIdx:      1,
		SrcSliceCnt:   common.MaxSliceCntPerSp,
		SrcStripeSize: 64 * 1024,
		SrcBlockSize:  1024 * 1024,
		DstTdName:     "dst",
		DmCloneConf:   &pb.DmCloneConf{HydrationThreshold: 2},
		AutoResume:    true,
	}); err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	// One byte per chunk: this is about transaction COUNT and op count, not
	// byte size.
	for sliceIdx := uint32(0); sliceIdx < common.MaxSliceCntPerSp; sliceIdx++ {
		for bmIdx := uint32(0); bmIdx < common.MaxCloneBmCnt; bmIdx++ {
			volAppendBm(env, "clone-max", sliceIdx, bmIdx)
		}
	}
	const total = common.MaxSliceCntPerSp * common.MaxCloneBmCnt
	if n := volCloneKeyCnt(env, "clone-max"); n != total {
		t.Fatalf("%d chunk keys planted, want %d", n, total)
	}
	volLatchClone(env, "clone-max")

	// The batches, counted: the rectangle leaves in exactly
	// ceil(total / MaxDelBmPerTxn) of them, each one MaxDelBmPerTxn deletes
	// except the last.
	clone := env.clone("clone-max")
	batches := 0
	for volCloneKeyCnt(env, "clone-max") > 0 {
		chunks := volChunks(env, "clone-max")
		want := len(chunks)
		if want > common.MaxDelBmPerTxn {
			want = common.MaxDelBmPerTxn
		}
		removed, err := model.DrainCloneBm(
			env.ctx, env.cli, env.cid, volShard, volSpId, volSpName,
			"clone-max", clone.GetCloneId(), chunks[:want])
		if err != nil {
			t.Fatalf("batch %d: %v (a \"too many operations in txn request\" "+
				"here means MaxDelBmPerTxn no longer fits EtcdMaxTxnOps)",
				batches, err)
		}
		if removed != want {
			t.Fatalf("batch %d removed %d, want %d", batches, removed, want)
		}
		batches++
		if batches > volDrainCloneSteps {
			t.Fatalf("the rectangle did not drain in %d batches",
				volDrainCloneSteps)
		}
	}
	wantBatches := (total + common.MaxDelBmPerTxn - 1) / common.MaxDelBmPerTxn
	if batches != wantBatches {
		t.Errorf("%d batches, want %d (%d chunks / %d per batch)",
			batches, wantBatches, total, common.MaxDelBmPerTxn)
	}
	// The record itself is untouched by every batch (CLD8): the drain derives
	// its position from the surviving keys, and writing anything into the
	// record would be a second copy of a truth those keys already carry.
	if got := env.clone("clone-max"); !proto.Equal(got, clone) {
		t.Errorf("a batch rewrote the Clone: got %v, want %v", got, clone)
	}
	if err := model.FinishCloneDelete(
		env.ctx, env.cli, env.cid, volShard, volSpId, volSpName,
		"clone-max", clone.GetCloneId(),
	); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	if env.exists(model.CloneKey(env.cid, volSpId, "clone-max"), &pb.Clone{}) {
		t.Errorf("the clone key survived")
	}
}
