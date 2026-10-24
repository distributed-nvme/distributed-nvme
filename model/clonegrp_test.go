package model

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone-keyed twins of the slice-keyed ops (dnv-worker.md MD6): the epoch
// writes and the provisioned flip on a clone's clone meta group. Each case is
// its slice sibling's of ops_test.go on the clone's group, plus the checks
// the clone adds: its name addresses the record, its clone_id proves it, a
// clone with no group is refused, and a deleting clone is skipped by the
// flip.

// The fixture clone's ids: below opsNextId like every id of the fixture, and
// apart from the slice's, so a twin that looked in the slice would find
// nothing.
const (
	opsCloneName  = "clone-grp"
	opsCloneId    = uint64(911)
	opsCloneGrpId = uint64(912)
	opsCloneLegA  = uint64(913)
	opsCloneSideA = uint64(914)
	opsCloneLegB  = uint64(915)
	opsCloneSideB = uint64(916)
)

// cloneGrpAt builds one clone meta group of CloneMetaGrpExtCnt extents with
// one single-sided leg per address: grp_id base, then a leg_id and a side_id
// per leg. Every side is provisioned or none.
func cloneGrpAt(base uint64, provisioned bool, addrs ...string) *pb.Group {
	grp := &pb.Group{
		GrpId:      base,
		ExtCnt:     common.CloneMetaGrpExtCnt,
		MetaBlocks: 3,
		DataBlocks: 1021,
	}
	for idx, addrPort := range addrs {
		legId := base + 1 + 2*uint64(idx)
		grp.LegList = append(grp.LegList, &pb.Leg{
			LegId:    legId,
			LegIdx:   uint32(idx),
			SideList: []*pb.Side{opsSide(legId+1, addrPort, provisioned)},
		})
	}
	return grp
}

// opsCloneGrp is the fixture clone's group: an md-raid1 pair on dn-c and
// dn-d, the two DNs no side of the fixture's slice uses.
func opsCloneGrp(provisioned bool) *pb.Group {
	return cloneGrpAt(opsCloneGrpId, provisioned, opsDnC, opsDnD)
}

// putGrpClone plants one clone of the fixture SP carrying grp, latched or
// not, and lists its name in clone_name_list. No DN is charged here; the
// cases that need the charge put it with chargeGrp.
func (e *opsEnv) putGrpClone(
	name string,
	cloneId uint64,
	grp *pb.Group,
	deleting bool,
) {
	e.t.Helper()
	conf := e.spConf()
	if !containsName(conf.GetCloneNameList(), name) {
		conf.CloneNameList = append(conf.GetCloneNameList(), name)
		mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	}
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, name), &pb.Clone{
		CloneId:  cloneId,
		SrcNqn:   "nqn.2024-01.io.dnv:src",
		DstTdId:  601,
		Deleting: deleting,
		MetaGrp:  grp,
	})
}

// setCloneGrp gives a planted clone its group, as CreateClone writes it.
func (e *opsEnv) setCloneGrp(name string, grp *pb.Group) {
	e.t.Helper()
	clone := e.clone(name)
	clone.MetaGrp = grp
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, name), clone)
}

// chargeGrp charges every side of grp's legs to its DN as the create that
// placed it would have: the pointer appended to side_ptr_list,
// the group's ext_cnt out of free_ext_cnt and the capacity key moved with
// the free count it embeds (architecture.md, Capacity index keys).
func (e *opsEnv) chargeGrp(grp *pb.Group) {
	e.t.Helper()
	for _, leg := range grp.GetLegList() {
		for _, side := range leg.GetSideList() {
			addrPort := side.GetAddrPort()
			dn := e.dn(addrPort)
			oldFree := dn.GetFreeExtCnt()
			e.delKey(DnCapacityKey(
				e.cid, mustBin(e.t, e, oldFree), oldFree, addrPort))
			dn.SidePtrList = append(dn.GetSidePtrList(), &pb.SidePointer{
				SpId:   opsSpId,
				LegId:  leg.GetLegId(),
				SideId: side.GetSideId(),
			})
			dn.FreeExtCnt = oldFree - grp.GetExtCnt()
			mustPut(e.t, e.cli, DnConfKey(e.cid, addrPort), dn)
			newFree := dn.GetFreeExtCnt()
			mustPut(e.t, e.cli,
				DnCapacityKey(e.cid, mustBin(e.t, e, newFree), newFree,
					addrPort),
				&pb.DnCapacity{Location: addrPort})
		}
	}
}

// cloneGrpDnAddrs is the plan a caller of CreateRepair scans a fresh new leg
// of the fixture clone's group against, read now: the DN of every side of
// every leg of that group.
func (e *opsEnv) cloneGrpDnAddrs() []string {
	e.t.Helper()
	var addrs []string
	for _, leg := range cloneLegs(e.clone(opsCloneName)) {
		for _, side := range leg.GetSideList() {
			addrs = append(addrs, side.GetAddrPort())
		}
	}
	return addrs
}

// TestCloneMetaSideZeroBytes pins the length to zero of a clone meta side
// (architecture.md, Side provisioning protocol): a data group's, its meta
// region and the first block of its data region, and not the whole side a
// meta group of a slice is zeroed over, which "meta" in the group's name
// might suggest. The refusals are SideZeroBytes's.
func TestCloneMetaSideZeroBytes(t *testing.T) {
	groupOf := func(conf *pb.BdevConf) *pb.Group {
		t.Helper()
		metaBlocks, dataBlocks, err := GroupBlocks(
			common.CloneMetaGrpExtCnt, opsExtSize, conf)
		if err != nil {
			t.Fatalf("GroupBlocks: %v", err)
		}
		return &pb.Group{
			GrpId:      7,
			ExtCnt:     common.CloneMetaGrpExtCnt,
			MetaBlocks: metaBlocks,
			DataBlocks: dataBlocks,
		}
	}
	for _, tc := range []struct {
		name string
		grp  *pb.Group
		want uint64
	}{
		{"md-raid1: three meta blocks and one data block",
			groupOf(testRaid1BdevConf(opsBlockSize, 128)), 4 * opsBlockSize},
		{"redund_none: the health block and one data block",
			groupOf(testBdevConf(opsBlockSize)), 2 * opsBlockSize},
		{"stored counts", &pb.Group{MetaBlocks: 5, DataBlocks: 9},
			6 * opsBlockSize},
	} {
		got, err := CloneMetaSideZeroBytes(tc.grp, opsBlockSize)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d (%v), want %d", tc.name, got, err, tc.want)
		}
		whole, err := SideZeroBytes(tc.grp, true, opsBlockSize)
		if err != nil || whole == got {
			t.Errorf("%s: the meta group length %d (%v) must differ from "+
				"the clone meta side's %d", tc.name, whole, err, got)
		}
	}
	_, err := CloneMetaSideZeroBytes(
		&pb.Group{GrpId: 7, MetaBlocks: 0, DataBlocks: 9}, opsBlockSize)
	if err == nil ||
		err.Error() != "invalid stored conf: group 7 meta_blocks is zero" {
		t.Errorf("a group with no meta_blocks: got %v, want the refusal "+
			"SideZeroBytes gives", err)
	}
}

func TestSetCloneLegErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	sliceBefore := env.slice()
	revBefore := env.spRev()
	set := func(legId uint64, epoch uint64) error {
		return SetCloneLegErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
			opsCloneName, opsCloneId, legId, epoch)
	}
	epochOf := func(legId uint64) uint64 {
		return findCloneLeg(env.clone(opsCloneName), legId).GetErrEpoch()
	}
	if err := set(opsCloneLegA, 1000); err != nil {
		t.Fatalf("SetCloneLegErrEpoch: %v", err)
	}
	if got := epochOf(opsCloneLegA); got != 1000 {
		t.Errorf("leg err_epoch: got %d, want 1000", got)
	}
	// No restart, then clear.
	if err := set(opsCloneLegA, 9000); err != nil {
		t.Fatalf("SetCloneLegErrEpoch again: %v", err)
	}
	if got := epochOf(opsCloneLegA); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := set(opsCloneLegA, 0); err != nil {
		t.Fatalf("SetCloneLegErrEpoch clear: %v", err)
	}
	if got := epochOf(opsCloneLegA); got != 0 {
		t.Errorf("leg err_epoch: got %d, want 0", got)
	}
	// A deleting clone's legs take a health epoch until the drain removes
	// them.
	clone := env.clone(opsCloneName)
	clone.Deleting = true
	mustPut(t, env.cli, CloneKey(env.cid, opsSpId, opsCloneName), clone)
	if err := set(opsCloneLegB, 1300); err != nil {
		t.Fatalf("SetCloneLegErrEpoch on a deleting clone: %v", err)
	}
	if got := epochOf(opsCloneLegB); got != 1300 {
		t.Errorf("deleting clone's leg err_epoch: got %d, want 1300", got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d", got,
			revBefore)
	}
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("a clone leg's write touched the slice: %v", got)
	}
	// A slice's leg id is no leg of the clone's group.
	precondition := wantPrecondition(t, set(opsDataLegA, 1),
		opSetCloneLegErrEpoch)
	if precondition.Reason != "leg not found" {
		t.Errorf("Reason: got %q, want %q", precondition.Reason,
			"leg not found")
	}
	wantCloneTwinRefusals(t, opSetCloneLegErrEpoch,
		func(env *opsEnv) error {
			return SetCloneLegErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsCloneName, opsCloneId, opsCloneLegA, 1)
		})
}

func TestSetCloneSideErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	sliceBefore := env.slice()
	revBefore := env.spRev()
	set := func(sideId uint64, epoch uint64) error {
		return SetCloneSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
			opsCloneName, opsCloneId, sideId, epoch)
	}
	epochOf := func(sideId uint64) uint64 {
		return findCloneSide(env.clone(opsCloneName), sideId).GetErrEpoch()
	}
	if err := set(opsCloneSideB, 1000); err != nil {
		t.Fatalf("SetCloneSideErrEpoch: %v", err)
	}
	if got := epochOf(opsCloneSideB); got != 1000 {
		t.Errorf("side err_epoch: got %d, want 1000", got)
	}
	// The group's other side is found too.
	if err := set(opsCloneSideA, 1100); err != nil {
		t.Fatalf("SetCloneSideErrEpoch side A: %v", err)
	}
	if got := epochOf(opsCloneSideA); got != 1100 {
		t.Errorf("side A err_epoch: got %d, want 1100", got)
	}
	if err := set(opsCloneSideB, 9000); err != nil {
		t.Fatalf("SetCloneSideErrEpoch again: %v", err)
	}
	if got := epochOf(opsCloneSideB); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := set(opsCloneSideB, 0); err != nil {
		t.Fatalf("SetCloneSideErrEpoch clear: %v", err)
	}
	if got := epochOf(opsCloneSideB); got != 0 {
		t.Errorf("side err_epoch: got %d, want 0", got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d", got,
			revBefore)
	}
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("a clone side's write touched the slice: %v", got)
	}
	precondition := wantPrecondition(t, set(opsDataSideA, 1),
		opSetCloneSideErrEpoch)
	if precondition.Reason != "side not found" {
		t.Errorf("Reason: got %q, want %q", precondition.Reason,
			"side not found")
	}
	wantCloneTwinRefusals(t, opSetCloneSideErrEpoch,
		func(env *opsEnv) error {
			return SetCloneSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsCloneName, opsCloneId, opsCloneSideA, 1)
		})
}

// wantCloneTwinRefusals runs one clone-keyed op against the three clones
// loadCloneForTwin refuses — gone, re-created under its name with another
// clone_id, and with no group — each in a fresh fixture, and asserts the
// refusal and that the op wrote nothing. The control row, the fixture
// clone as it is, commits, so the guard is not "refuse everything".
func wantCloneTwinRefusals(
	t *testing.T,
	op string,
	run func(env *opsEnv) error,
) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		break_ func(env *opsEnv)
		reason string
	}{
		{"clone not found", func(env *opsEnv) {
			env.delKey(CloneKey(env.cid, opsSpId, opsCloneName))
		}, "clone not found"},
		{"clone id changed", func(env *opsEnv) {
			clone := env.clone(opsCloneName)
			clone.CloneId = opsCloneId + 100
			mustPut(env.t, env.cli,
				CloneKey(env.cid, opsSpId, opsCloneName), clone)
		}, "clone id changed"},
		{"clone has no group", func(env *opsEnv) {
			env.setCloneGrp(opsCloneName, nil)
		}, "clone has no group"},
		{"control", func(env *opsEnv) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true),
				false)
			tc.break_(env)
			before := env.storeRev()
			err := run(env)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("%s on the fixture clone: %v", op, err)
				}
				return
			}
			precondition := wantPrecondition(t, err, op)
			if precondition.Reason != tc.reason {
				t.Errorf("Reason: got %q, want %q", precondition.Reason,
					tc.reason)
			}
			if got := env.storeRev(); got != before {
				t.Errorf("the refusal wrote: store revision %d, want %d",
					got, before)
			}
		})
	}
}

// TestFlipCloneProvisioned is TestFlipProvisioned on clone meta groups, and
// the skips the clone adds: a clone that is gone, re-created under its name,
// being deleted or without a group is skipped with its sides, and the other
// clones of the batch still flip.
func TestFlipCloneProvisioned(t *testing.T) {
	const otherName = "clone-other"
	const otherId = uint64(921)
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(false), false)
	other := cloneGrpAt(922, false, opsDnA, opsDnB)
	env.putGrpClone(otherName, otherId, other, false)
	otherLeg := other.GetLegList()[0]
	otherSide := otherLeg.GetSideList()[0]
	sliceBefore := env.slice()
	revBefore := env.spRev()
	sides := []CloneSideRef{
		{CloneName: opsCloneName, CloneId: opsCloneId,
			LegId: opsCloneLegA, SideId: opsCloneSideA},
		{CloneName: otherName, CloneId: otherId,
			LegId: otherLeg.GetLegId(), SideId: otherSide.GetSideId()},
		{CloneName: opsCloneName, CloneId: opsCloneId,
			LegId: opsCloneLegB, SideId: opsCloneSideB},
	}
	flip := func(refs []CloneSideRef) ([]CloneSideRef, uint64) {
		t.Helper()
		flipped, revision, err := FlipCloneProvisioned(
			env.ctx, env.cli, env.cid, opsShard, opsSpId, refs)
		if err != nil {
			t.Fatalf("FlipCloneProvisioned: %v", err)
		}
		return flipped, revision
	}
	flipped, revision := flip(sides)
	if !slicesEqualRefs(flipped, sides) {
		t.Errorf("flipped: got %v, want %v", flipped, sides)
	}
	if revision != revBefore+1 {
		t.Errorf("revision: got %d, want %d, the one the STM committed",
			revision, revBefore+1)
	}
	clone := env.clone(opsCloneName)
	if !findCloneSide(clone, opsCloneSideA).GetProvisioned() ||
		!findCloneSide(clone, opsCloneSideB).GetProvisioned() {
		t.Errorf("both listed sides of %s must be provisioned", opsCloneName)
	}
	otherClone := env.clone(otherName)
	if !findCloneSide(otherClone, otherSide.GetSideId()).GetProvisioned() {
		t.Errorf("the listed side of %s must be provisioned", otherName)
	}
	if findCloneSide(otherClone, otherSide.GetSideId()+2).GetProvisioned() {
		t.Errorf("an unlisted side must not be touched")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)", got,
			revBefore+1)
	}
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("a clone flip touched the slice: %v", got)
	}
	// Idempotent: a repeated batch writes nothing and bumps nothing.
	if flipped, revision := flip(sides); len(flipped) != 0 || revision != 0 {
		t.Errorf("flipped: got %v at revision %d, want none at 0", flipped,
			revision)
	}
	// A stale pointer is skipped: an unknown side, a side of another leg,
	// and a slice's side named under the clone.
	env.setCloneGrp(opsCloneName, opsCloneGrp(false))
	stale := []CloneSideRef{
		{CloneName: opsCloneName, CloneId: opsCloneId,
			LegId: opsCloneLegA, SideId: 9999},
		{CloneName: opsCloneName, CloneId: opsCloneId,
			LegId: opsCloneLegA, SideId: opsCloneSideB},
		{CloneName: opsCloneName, CloneId: opsCloneId,
			LegId: opsDataLegA, SideId: opsDataSideA},
	}
	if flipped, _ := flip(stale); len(flipped) != 0 {
		t.Errorf("a stale pointer must flip nothing: got %v", flipped)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev must not move: got %d", got)
	}

	// Each clone the flip skips, beside one it flips in the same batch.
	for _, tc := range []struct {
		name   string
		break_ func(env *opsEnv)
	}{
		{"a gone clone", func(env *opsEnv) {
			env.delKey(CloneKey(env.cid, opsSpId, otherName))
		}},
		{"a clone re-created under its name", func(env *opsEnv) {
			clone := env.clone(otherName)
			clone.CloneId = otherId + 100
			mustPut(env.t, env.cli,
				CloneKey(env.cid, opsSpId, otherName), clone)
		}},
		{"a deleting clone", func(env *opsEnv) {
			clone := env.clone(otherName)
			clone.Deleting = true
			mustPut(env.t, env.cli,
				CloneKey(env.cid, opsSpId, otherName), clone)
		}},
		{"a clone with no group", func(env *opsEnv) {
			env.setCloneGrp(otherName, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(false),
				false)
			env.putGrpClone(otherName, otherId,
				cloneGrpAt(922, false, opsDnA, opsDnB), false)
			tc.break_(env)
			otherKey := CloneKey(env.cid, opsSpId, otherName)
			var otherMod int64
			if env.exists(otherKey) {
				otherMod = env.modRev(otherKey)
			}
			revBefore := env.spRev()
			refs := []CloneSideRef{sides[1], sides[0]}
			flipped, _, err := FlipCloneProvisioned(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, refs)
			if err != nil {
				t.Fatalf("FlipCloneProvisioned: %v", err)
			}
			if !slicesEqualRefs(flipped, refs[1:]) {
				t.Errorf("flipped: got %v, want only %v", flipped, refs[1:])
			}
			if !findCloneSide(env.clone(opsCloneName),
				opsCloneSideA).GetProvisioned() {
				t.Errorf("the other clone's side must still flip")
			}
			if env.exists(otherKey) {
				if got := env.modRev(otherKey); got != otherMod {
					t.Errorf("the skipped clone was rewritten")
				}
			}
			if got := env.spRev(); got != revBefore+1 {
				t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
			}
		})
	}
}

// slicesEqualRefs compares what a clone flip REPORTED with what it was
// expected to write, order included.
func slicesEqualRefs(got []CloneSideRef, want []CloneSideRef) bool {
	if len(got) != len(want) {
		return false
	}
	for idx := range got {
		if got[idx] != want[idx] {
			return false
		}
	}
	return true
}

// TestFlipCloneProvisionedAtTheCloneCeiling commits the clone flip at its
// widest shape against the real etcd this package runs with
// --max-txn-ops=common.EtcdMaxTxnOps: one side of each of MaxCloneCntPerSp
// clones, every Clone key read and written in one transaction with the
// SpRev key. That is over etcd's default transaction cap, which makes the
// clone flip one of the transactions the deployment flag exists for
// (dnv-worker.md, Constants this document owns).
func TestFlipCloneProvisionedAtTheCloneCeiling(t *testing.T) {
	const etcdDefaultTxnOps = 128
	cloneCnt := common.MaxCloneCntPerSp
	// The fixture stays a reproduction: its one transaction is over etcd's
	// default and within EtcdMaxTxnOps.
	if compares := 2*cloneCnt + 2; compares <= etcdDefaultTxnOps ||
		compares > common.EtcdMaxTxnOps {
		t.Fatalf("%d clones no longer reproduce a flip over etcd's "+
			"default: %d compares against the default %d and "+
			"EtcdMaxTxnOps %d", cloneCnt, compares, etcdDefaultTxnOps,
			common.EtcdMaxTxnOps)
	}
	env := newOpsEnv(t)
	var refs []CloneSideRef
	for idx := 0; idx < cloneCnt; idx++ {
		name := fmt.Sprintf("ceil-clone-%02d", idx)
		cloneId := 2000 + 10*uint64(idx)
		grp := cloneGrpAt(cloneId+1, false, opsDnC, opsDnD)
		env.putGrpClone(name, cloneId, grp, false)
		leg := grp.GetLegList()[1]
		refs = append(refs, CloneSideRef{
			CloneName: name, CloneId: cloneId,
			LegId: leg.GetLegId(), SideId: leg.GetSideList()[0].GetSideId(),
		})
	}
	revBefore := env.spRev()

	flipped, revision, err := FlipCloneProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, refs)
	if err != nil {
		t.Fatalf("FlipCloneProvisioned over %d clones: %v (etcd's \"too "+
			"many operations in txn request\" here means one transaction "+
			"carried more of them than common.EtcdMaxTxnOps admits)",
			cloneCnt, err)
	}
	if !slicesEqualRefs(flipped, refs) {
		t.Errorf("flipped %d sides, want all %d in list order",
			len(flipped), len(refs))
	}
	if revision != revBefore+1 || env.spRev() != revBefore+1 {
		t.Errorf("SpRev: committed %d, stored %d, want %d (one bump)",
			revision, env.spRev(), revBefore+1)
	}
	// One transaction wrote every clone: they share one mod revision.
	want := env.modRev(CloneKey(env.cid, opsSpId, refs[0].CloneName))
	for _, ref := range refs {
		key := CloneKey(env.cid, opsSpId, ref.CloneName)
		if got := env.modRev(key); got != want {
			t.Errorf("%s at mod revision %d, want %d: the flip took more "+
				"than one transaction", ref.CloneName, got, want)
		}
		if !findCloneSide(env.clone(ref.CloneName),
			ref.SideId).GetProvisioned() {
			t.Errorf("%s: side %d was not flipped", ref.CloneName,
				ref.SideId)
		}
	}
}
