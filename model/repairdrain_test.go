package model

import (
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The drains' half of repairs and retired legs (dnv-worker.md, The sp drain;
// The clone drain): the sp drain's repair and retired-leg phases, their
// batches, their order before the slices, and the clone drain's final step
// giving a clone meta group's repair and retired legs back.

// loadFixture is LoadSp of the fixture SP.
func (e *opsEnv) loadFixture() *SpState {
	e.t.Helper()
	state, err := LoadSp(e.ctx, e.cli, e.cid, opsSpName)
	if err != nil {
		e.t.Fatalf("LoadSp: %v", err)
	}
	return state
}

// TestDrainSpRepairs: one batch gives every new leg's side back, clears the
// group's rep_name, deletes the Repair key with its list entry and bumps
// SpRev once; the loser of an overlap finds nothing and writes nothing
// (dnv-worker.md SPD15).
func TestDrainSpRepairs(t *testing.T) {
	env := newOpsEnv(t)
	env.createRepair(func() RepairNewLeg { return env.freshLeg(opsDnC) })
	env.latchForDrain()
	batch := SpDrainRepairBatch(env.loadFixture())
	if !equalStrings(batch, []string{opsRepName}) {
		t.Fatalf("batch: got %v, want [%s]", batch, opsRepName)
	}
	dnCRev, revBefore := env.dnRev(opsDnC), env.spRev()
	removed, err := DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, batch, env.cc)
	if err != nil || removed != 1 {
		t.Fatalf("DrainSpRepairs = %d, %v, want 1", removed, err)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
	if got := env.dnRev(opsDnC); got != dnCRev+1 {
		t.Errorf("dn-c rev: got %d, want %d", got, dnCRev+1)
	}
	if env.exists(RepairKey(env.cid, opsSpId, opsRepName)) ||
		len(env.spConf().GetRepairNameList()) != 0 {
		t.Errorf("the repair or its name survived the batch")
	}
	if got := findGroup(env.slice(), opsDataGrpId).GetRepName(); got != "" {
		t.Errorf("rep_name: got %q, want it cleared", got)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	before := env.storeRev()
	removed, err = DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, batch, env.cc)
	if err != nil || removed != 0 {
		t.Errorf("the loser's batch = %d, %v, want 0", removed, err)
	}
	if got := env.storeRev(); got != before {
		t.Errorf("the loser's batch wrote")
	}
}

// TestDrainSpRepairsCountsANameOnce: a batch that names one repair twice
// deletes it once and gives its new leg's side back once, so the DN's free
// extents come back to exactly what they were, never above.
func TestDrainSpRepairsCountsANameOnce(t *testing.T) {
	env := newOpsEnv(t)
	env.createRepair(func() RepairNewLeg { return env.freshLeg(opsDnC) })
	env.latchForDrain()
	removed, err := DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []string{opsRepName, opsRepName}, env.cc)
	if err != nil || removed != 1 {
		t.Fatalf("DrainSpRepairs = %d, %v, want 1", removed, err)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want exactly %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
}

// TestDrainSpRepairsGivesAReusedLegBack: a new leg taken from a retired leg
// goes back to its DN with the pool, never back into a retired leg.
func TestDrainSpRepairsGivesAReusedLegBack(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	env.createRepair(func() RepairNewLeg {
		return RepairNewLeg{RetiredLegId: opsRetiredLeg}
	})
	env.latchForDrain()
	if _, err := DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []string{opsRepName}, env.cc); err != nil {
		t.Fatalf("DrainSpRepairs: %v", err)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
	if env.exists(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg)) ||
		len(env.spConf().GetRetiredLegIdList()) != 0 {
		t.Errorf("the reused leg came back as a retired leg")
	}
}

// putCloneRepair plants a repair of the fixture clone's group, a fresh new
// leg on dn-a, and its name on the group and in the pool's list.
func (e *opsEnv) putCloneRepair(name string, repId uint64) {
	e.t.Helper()
	clone := e.clone(opsCloneName)
	clone.MetaGrp.RepName = name
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, opsCloneName), clone)
	conf := e.spConf()
	conf.RepairNameList = append(conf.RepairNameList, name)
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	mustPut(e.t, e.cli, RepairKey(e.cid, opsSpId, name), &pb.Repair{
		RepId: repId,
		GrpId: opsCloneGrpId,
		NewLegList: []*pb.Leg{{
			LegId:    repId + 1,
			LegIdx:   2,
			SideList: []*pb.Side{opsSide(repId+2, opsDnA, true)},
		}},
		BadLegIdList: []uint64{opsCloneLegB},
		CloneName:    opsCloneName,
	})
}

// TestDrainSpRepairsRefusals is every refusal of the repair phase, each with
// nothing written: the batch is the fixture's one repair of the data group.
func TestDrainSpRepairsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv) []string
		reason string
	}{
		{"cntlrs not drained", func(env *opsEnv) []string {
			conf := env.spConf()
			conf.CntlrIdList = []uint64{opsCntlrA}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			return []string{opsRepName}
		}, "cntlrs not drained"},
		{"batch too large", func(env *opsEnv) []string {
			names := make([]string, common.MaxDelRepPerTxn+1)
			for idx := range names {
				names[idx] = opsRepName
			}
			return names
		}, "batch too large"},
		{"repair record missing", func(env *opsEnv) []string {
			env.delKey(RepairKey(env.cid, opsSpId, opsRepName))
			return []string{opsRepName}
		}, "repair record missing"},
		{"a group the parent does not hold", func(env *opsEnv) []string {
			rep := env.repair(opsRepName)
			rep.GrpId = 9999
			mustPut(env.t, env.cli, RepairKey(env.cid, opsSpId, opsRepName),
				rep)
			return []string{opsRepName}
		}, "group not found"},
		// A lost parent key wedges the batch rather than letting it report
		// success with nothing removed (dnv-worker.md, The sp drain).
		{"parent not found", func(env *opsEnv) []string {
			env.delKey(SliceKey(env.cid, opsSpId, opsSliceId))
			return []string{opsRepName}
		}, "slice not found"},
		{"batch spans two parents", func(env *opsEnv) []string {
			env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true),
				false)
			env.putCloneRepair("rep1", 2000)
			return []string{opsRepName, "rep1"}
		}, "batch spans two parents"},
		{"an invalid stored conf", func(env *opsEnv) []string {
			env.cc.DnBinConf.ExtentSize = 0
			return []string{opsRepName}
		}, "invalid stored conf: dn_bin_conf.extent_size is zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			env.latchForDrain()
			batch := tc.setup(env)
			before := env.storeRev()
			_, err := DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, batch, env.cc)
			env.wantNoWrite(err, opDrainSpRepairs, tc.reason, before)
		})
	}
}

// TestDrainSpRetiredLegs: one batch of the group's two retired legs gives
// both sides back, deletes both records with their entries in the group's
// and the pool's lists, and bumps SpRev once; the loser of an overlap finds
// nothing and writes nothing (dnv-worker.md SPD15).
func TestDrainSpRetiredLegs(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	env.putRetired(opsDataGrpId, opsRetiredLeg2, opsRetiredSide2, opsDnD, 1,
		opsRetiredEpoch)
	env.latchForDrain()
	batch := SpDrainRetiredLegBatch(env.loadFixture())
	if !equalIds(batch, []uint64{opsRetiredLeg, opsRetiredLeg2}) {
		t.Fatalf("batch: got %v", batch)
	}
	revBefore := env.spRev()
	removed, err := DrainSpRetiredLegs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, batch, env.cc)
	if err != nil || removed != 2 {
		t.Fatalf("DrainSpRetiredLegs = %d, %v, want 2", removed, err)
	}
	for _, addrPort := range []string{opsDnC, opsDnD} {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("%s: free %d with pointers %v, want %d and none",
				addrPort, dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
		}
	}
	for _, legId := range batch {
		if env.exists(RetiredLegKey(env.cid, opsSpId, legId)) {
			t.Errorf("retired leg %d survived the batch", legId)
		}
	}
	if len(env.spConf().GetRetiredLegIdList()) != 0 ||
		len(findGroup(env.slice(), opsDataGrpId).GetRetiredLegIdList()) != 0 {
		t.Errorf("the retired-leg lists survived the batch")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	before := env.storeRev()
	removed, err = DrainSpRetiredLegs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, batch, env.cc)
	if err != nil || removed != 0 {
		t.Errorf("the loser's batch = %d, %v, want 0", removed, err)
	}
	if got := env.storeRev(); got != before {
		t.Errorf("the loser's batch wrote")
	}
}

// TestDrainSpRetiredLegsCountsAnIdOnce: a batch that names one retired leg
// twice deletes it once and gives its side back once, so the DN's free
// extents come back to exactly what they were, never above.
func TestDrainSpRetiredLegsCountsAnIdOnce(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	env.latchForDrain()
	removed, err := DrainSpRetiredLegs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []uint64{opsRetiredLeg, opsRetiredLeg}, env.cc)
	if err != nil || removed != 1 {
		t.Fatalf("DrainSpRetiredLegs = %d, %v, want 1", removed, err)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want exactly %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
}

// TestDrainSpRetiredLegsRefusals is every refusal of the retired-leg phase,
// each with nothing written.
func TestDrainSpRetiredLegsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv) []uint64
		reason string
	}{
		{"cntlrs not drained", func(env *opsEnv) []uint64 {
			conf := env.spConf()
			conf.CntlrIdList = []uint64{opsCntlrA}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			return []uint64{opsRetiredLeg}
		}, "cntlrs not drained"},
		{"repairs not drained", func(env *opsEnv) []uint64 {
			conf := env.spConf()
			conf.RepairNameList = []string{opsRepName}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			return []uint64{opsRetiredLeg}
		}, "repairs not drained"},
		{"batch too large", func(env *opsEnv) []uint64 {
			ids := make([]uint64, common.MaxDelRetiredLegPerTxn+1)
			for idx := range ids {
				ids[idx] = opsRetiredLeg
			}
			return ids
		}, "batch too large"},
		{"retired leg record missing", func(env *opsEnv) []uint64 {
			env.delKey(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg))
			return []uint64{opsRetiredLeg}
		}, "retired leg record missing"},
		{"a group the parent does not hold", func(env *opsEnv) []uint64 {
			retired := env.retired(opsRetiredLeg)
			retired.GrpId = 9999
			mustPut(env.t, env.cli,
				RetiredLegKey(env.cid, opsSpId, opsRetiredLeg), retired)
			return []uint64{opsRetiredLeg}
		}, "group not found"},
		// A lost parent key wedges the batch rather than letting it report
		// success with nothing removed (dnv-worker.md, The sp drain).
		{"parent not found", func(env *opsEnv) []uint64 {
			retired := env.retired(opsRetiredLeg)
			retired.SliceId = 0
			retired.CloneName = "gone"
			mustPut(env.t, env.cli,
				RetiredLegKey(env.cid, opsSpId, opsRetiredLeg), retired)
			return []uint64{opsRetiredLeg}
		}, "clone not found"},
		{"batch spans two parents", func(env *opsEnv) []uint64 {
			retired := env.retired(opsRetiredLeg2)
			retired.SliceId = 0
			retired.CloneName = opsCloneName
			mustPut(env.t, env.cli,
				RetiredLegKey(env.cid, opsSpId, opsRetiredLeg2), retired)
			return []uint64{opsRetiredLeg, opsRetiredLeg2}
		}, "batch spans two parents"},
		{"an invalid stored conf", func(env *opsEnv) []uint64 {
			env.cc.DnBinConf.ExtentSize = 0
			return []uint64{opsRetiredLeg}
		}, "invalid stored conf: dn_bin_conf.extent_size is zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			env.putRetired(opsDataGrpId, opsRetiredLeg2, opsRetiredSide2,
				opsDnD, 1, opsRetiredEpoch)
			env.latchForDrain()
			batch := tc.setup(env)
			before := env.storeRev()
			_, err := DrainSpRetiredLegs(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, batch, env.cc)
			env.wantNoWrite(err, opDrainSpRetiredLegs, tc.reason, before)
		})
	}
}

// TestDrainSpSliceRefusesWhileRepairsOrRetiredLegsRemain is SPD8's order
// made a precondition: a slice batch pops groups, and the repairs' and the
// retired legs' sides go back by their groups' ext_cnt.
func TestDrainSpSliceRefusesWhileRepairsOrRetiredLegsRemain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		reason string
	}{
		{"repairs", func(env *opsEnv) {
			env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
		}, "repairs not drained"},
		{"retired legs", func(env *opsEnv) {
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
		}, "retired legs not drained"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			tc.setup(env)
			env.latchForDrain()
			before := env.storeRev()
			_, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, opsSliceId, env.cc)
			env.wantNoWrite(err, opDrainSpSlice, tc.reason, before)
		})
	}
}

// TestSpDrainBatches pins the derivation of one batch from a snapshot: the
// records of the first listed one's parent, in list order, at most the
// batch constant of them; a first listed record the snapshot lacks is a
// batch of its own, so the drain names its lost key.
func TestSpDrainBatches(t *testing.T) {
	slice := GrpParent{SliceId: 7}
	clone := GrpParent{CloneName: "c"}
	state := newSpState()
	state.Conf = &pb.SpConf{}
	add := func(name string, parent GrpParent) {
		state.Conf.RepairNameList = append(state.Conf.RepairNameList, name)
		state.Repairs[name] = &pb.Repair{
			SliceId: parent.SliceId, CloneName: parent.CloneName,
		}
	}
	addRetired := func(legId uint64, parent GrpParent) {
		state.Conf.RetiredLegIdList = append(
			state.Conf.RetiredLegIdList, legId)
		state.RetiredLegs[legId] = &pb.RetiredLeg{
			SliceId: parent.SliceId, CloneName: parent.CloneName,
		}
	}
	if SpDrainRepairBatch(state) != nil || SpDrainRetiredLegBatch(state) != nil {
		t.Fatalf("an empty pool has a batch")
	}
	add("a", clone)
	add("b", slice)
	add("c", clone)
	addRetired(3, slice)
	addRetired(4, clone)
	addRetired(5, slice)
	if got := SpDrainRepairBatch(state); !equalStrings(got,
		[]string{"a", "c"}) {
		t.Errorf("repair batch: got %v, want [a c]", got)
	}
	if got := SpDrainRetiredLegBatch(state); !equalIds(got,
		[]uint64{3, 5}) {
		t.Errorf("retired batch: got %v, want [3 5]", got)
	}
	delete(state.Repairs, "a")
	delete(state.RetiredLegs, 3)
	if got := SpDrainRepairBatch(state); !equalStrings(got, []string{"a"}) {
		t.Errorf("a lost first repair: got %v, want [a]", got)
	}
	if got := SpDrainRetiredLegBatch(state); !equalIds(got, []uint64{3}) {
		t.Errorf("a lost first retired leg: got %v, want [3]", got)
	}
	full := newSpState()
	full.Conf = &pb.SpConf{}
	for idx := 0; idx < common.MaxDelRepPerTxn+3; idx++ {
		name := fmt.Sprintf("r%03d", idx)
		full.Conf.RepairNameList = append(full.Conf.RepairNameList, name)
		full.Repairs[name] = &pb.Repair{SliceId: 7}
	}
	for idx := 0; idx < common.MaxDelRetiredLegPerTxn+3; idx++ {
		legId := uint64(100 + idx)
		full.Conf.RetiredLegIdList = append(full.Conf.RetiredLegIdList, legId)
		full.RetiredLegs[legId] = &pb.RetiredLeg{SliceId: 7}
	}
	if got := len(SpDrainRepairBatch(full)); got != common.MaxDelRepPerTxn {
		t.Errorf("a full repair batch holds %d, want %d", got,
			common.MaxDelRepPerTxn)
	}
	if got := len(SpDrainRetiredLegBatch(full)); got !=
		common.MaxDelRetiredLegPerTxn {
		t.Errorf("a full retired batch holds %d, want %d", got,
			common.MaxDelRetiredLegPerTxn)
	}
}

// TestDrainSpRunsRepairsAndRetiredLegsBeforeTheSlices drives a latched pool
// with a repair and a retired leg through the phases in SPD8's order, each
// batch derived from a fresh load, to the final step, and checks that every
// DN got back every extent the pool held.
func TestDrainSpRunsRepairsAndRetiredLegsBeforeTheSlices(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.putRetired(opsMetaGrpId, opsRetiredLeg, opsRetiredSide, opsDnD, 1,
		opsRetiredEpoch)
	env.createRepair(func() RepairNewLeg { return env.freshLeg(opsDnC) })
	env.setDeleting()
	var phases []string
	for step := 0; step < 10; step++ {
		state := env.loadFixture()
		conf := state.Conf
		var err error
		switch {
		case len(conf.GetCntlrIdList()) > 0:
			phases = append(phases, "cntlrs")
			_, err = DrainSpCntlrs(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName)
		case len(conf.GetRepairNameList()) > 0:
			phases = append(phases, "repairs")
			_, err = DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, SpDrainRepairBatch(state), env.cc)
		case len(conf.GetRetiredLegIdList()) > 0:
			phases = append(phases, "retired legs")
			_, err = DrainSpRetiredLegs(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, SpDrainRetiredLegBatch(state), env.cc)
		case len(conf.GetSliceIdList()) > 0:
			phases = append(phases, "slice")
			_, _, err = DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, conf.GetSliceIdList()[0], env.cc)
		default:
			phases = append(phases, "final")
			err = FinishSpDelete(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName)
		}
		if err != nil {
			t.Fatalf("phase %s: %v", phases[len(phases)-1], err)
		}
		if phases[len(phases)-1] == "final" {
			break
		}
	}
	want := []string{"cntlrs", "repairs", "retired legs", "slice", "final"}
	if !equalStrings(phases, want) {
		t.Errorf("phases: got %v, want %v", phases, want)
	}
	for _, addrPort := range []string{opsDnA, opsDnB, opsDnC, opsDnD} {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("%s: free %d with pointers %v, want %d and none",
				addrPort, dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
		}
	}
}

// TestDrainSpRepairsAtTheCeiling is the proof of the repair phase's budget
// (SPD14's real-etcd half): one batch of MaxDelRepPerTxn repairs of one
// slice, each new leg's side on a DN of its own, committed against the real
// etcd this package runs with --max-txn-ops=common.EtcdMaxTxnOps.
func TestDrainSpRepairsAtTheCeiling(t *testing.T) {
	env := newOpsEnv(t)
	env.latchForDrain()
	conf := env.spConf()
	slice := &pb.Slice{SliceIdx: 0}
	var names, addrs []string
	for idx := 0; idx < common.MaxDelRepPerTxn; idx++ {
		sideId := drainDataSideBase + uint64(idx)
		grp := drainGrp(drainDataGrpBase+uint64(idx), sideId, opsDnA)
		name := fmt.Sprintf("rep-%03d", idx)
		grp.RepName = name
		slice.DataGrpList = append(slice.DataGrpList, grp)
		addrPort := fmt.Sprintf("rep-dn-%03d:9000", idx)
		env.putDn(addrPort, uint64(2000+idx), uint32(idx%256), opsDnFree)
		legId := uint64(20000 + 2*idx)
		env.chargeSide(addrPort, legId, legId+1, drainExtCnt)
		mustPut(t, env.cli, RepairKey(env.cid, opsSpId, name), &pb.Repair{
			RepId: uint64(30000 + idx),
			GrpId: grp.GetGrpId(),
			NewLegList: []*pb.Leg{{
				LegId:    legId,
				LegIdx:   1,
				SideList: []*pb.Side{opsSide(legId+1, addrPort, true)},
			}},
			BadLegIdList: []uint64{drainLegBase + sideId},
			SliceId:      opsSliceId,
		})
		names = append(names, name)
		addrs = append(addrs, addrPort)
	}
	conf.RepairNameList = names
	mustPut(t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)

	batch := SpDrainRepairBatch(env.loadFixture())
	if len(batch) != common.MaxDelRepPerTxn {
		t.Fatalf("batch of %d, want %d", len(batch), common.MaxDelRepPerTxn)
	}
	removed, err := DrainSpRepairs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, batch, env.cc)
	if err != nil {
		t.Fatalf("a maximum-shape repair batch (%d repairs over %d disk "+
			"nodes) did not commit: %v", len(batch), len(addrs), err)
	}
	if removed != common.MaxDelRepPerTxn {
		t.Fatalf("removed %d, want %d", removed, common.MaxDelRepPerTxn)
	}
	for _, addrPort := range addrs {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Fatalf("%s: free %d with pointers %v", addrPort,
				dn.GetFreeExtCnt(), dn.GetSidePtrList())
		}
	}
}

// TestDrainSpRetiredLegsAtTheCeiling is the same proof for the retired-leg
// phase: MaxDelRetiredLegPerTxn retired legs of one slice, MaxRetiredLegPerGrp
// per group, each side on a DN of its own.
func TestDrainSpRetiredLegsAtTheCeiling(t *testing.T) {
	env := newOpsEnv(t)
	env.latchForDrain()
	conf := env.spConf()
	slice := &pb.Slice{SliceIdx: 0}
	var ids []uint64
	var addrs []string
	for grpIdx := 0; len(ids) < common.MaxDelRetiredLegPerTxn; grpIdx++ {
		sideId := drainDataSideBase + uint64(grpIdx)
		grp := drainGrp(drainDataGrpBase+uint64(grpIdx), sideId, opsDnA)
		for legIdx := 0; legIdx < common.MaxRetiredLegPerGrp &&
			len(ids) < common.MaxDelRetiredLegPerTxn; legIdx++ {
			idx := len(ids)
			addrPort := fmt.Sprintf("ret-dn-%03d:9000", idx)
			env.putDn(addrPort, uint64(2000+idx), uint32(idx%256), opsDnFree)
			legId := uint64(20000 + 2*idx)
			env.chargeSide(addrPort, legId, legId+1, drainExtCnt)
			mustPut(t, env.cli, RetiredLegKey(env.cid, opsSpId, legId),
				&pb.RetiredLeg{
					GrpId:        grp.GetGrpId(),
					LegIdx:       1,
					Side:         opsSide(legId+1, addrPort, true),
					RetiredEpoch: opsRetiredEpoch,
					SliceId:      opsSliceId,
				})
			grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
			ids = append(ids, legId)
			addrs = append(addrs, addrPort)
		}
		slice.DataGrpList = append(slice.DataGrpList, grp)
	}
	conf.RetiredLegIdList = ids
	mustPut(t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)

	batch := SpDrainRetiredLegBatch(env.loadFixture())
	if len(batch) != common.MaxDelRetiredLegPerTxn {
		t.Fatalf("batch of %d, want %d", len(batch),
			common.MaxDelRetiredLegPerTxn)
	}
	removed, err := DrainSpRetiredLegs(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, batch, env.cc)
	if err != nil {
		t.Fatalf("a maximum-shape retired-leg batch (%d retired legs over "+
			"%d disk nodes) did not commit: %v", len(batch), len(addrs), err)
	}
	if removed != common.MaxDelRetiredLegPerTxn {
		t.Fatalf("removed %d, want %d", removed,
			common.MaxDelRetiredLegPerTxn)
	}
	for _, addrPort := range addrs {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Fatalf("%s: free %d with pointers %v", addrPort,
				dn.GetFreeExtCnt(), dn.GetSidePtrList())
		}
	}
}

// putCloneRetired plants one retired leg of the drain clone's group,
// charged to its DN, listed in the group and in the pool.
func (e *opsEnv) putCloneRetired(
	grp *pb.Group,
	legId uint64,
	addrPort string,
) {
	e.t.Helper()
	mustPut(e.t, e.cli, RetiredLegKey(e.cid, opsSpId, legId), &pb.RetiredLeg{
		GrpId:        grp.GetGrpId(),
		LegIdx:       1,
		Side:         opsSide(legId+1, addrPort, true),
		RetiredEpoch: opsRetiredEpoch,
		CloneName:    drainCloneName,
	})
	grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
	conf := e.spConf()
	conf.RetiredLegIdList = append(conf.RetiredLegIdList, legId)
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	e.chargeSide(addrPort, legId, legId+1, grp.GetExtCnt())
}

// putCloneRepairOf plants a repair of the drain clone's group with one fresh
// new leg on addrPort, charged, named on the group and listed in the pool.
func (e *opsEnv) putCloneRepairOf(
	grp *pb.Group,
	name string,
	legId uint64,
	addrPort string,
) {
	e.t.Helper()
	grp.RepName = name
	mustPut(e.t, e.cli, RepairKey(e.cid, opsSpId, name), &pb.Repair{
		RepId: legId - 1,
		GrpId: grp.GetGrpId(),
		NewLegList: []*pb.Leg{{
			LegId:    legId,
			LegIdx:   2,
			SideList: []*pb.Side{opsSide(legId+1, addrPort, false)},
		}},
		BadLegIdList: []uint64{grp.GetLegList()[1].GetLegId()},
		CloneName:    drainCloneName,
	})
	conf := e.spConf()
	conf.RepairNameList = append(conf.RepairNameList, name)
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	e.chargeSide(addrPort, legId, legId+1, grp.GetExtCnt())
}

// TestFinishCloneDeleteGivesTheRepairAndRetiredLegsBack: the clone drain's
// final step deletes the clone meta group's repair and its retired legs with
// the group, every side back to its DN and every list entry gone, in the one
// transaction that removes the clone.
func TestFinishCloneDeleteGivesTheRepairAndRetiredLegsBack(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	env.putDn("dn-e:9000", 704, 4, opsDnFree)
	env.putDn("dn-f:9000", 705, 5, opsDnFree)
	grp := opsCloneGrp(true)
	env.chargeGrp(grp)
	env.putCloneRepairOf(grp, "rep1", 2001, opsDnA)
	env.putCloneRetired(grp, 2011, "dn-e:9000")
	env.putCloneRetired(grp, 2013, "dn-f:9000")
	env.setCloneGrp(drainCloneName, grp)

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	for _, addrPort := range []string{
		opsDnA, opsDnC, opsDnD, "dn-e:9000", "dn-f:9000",
	} {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("%s: free %d with pointers %v, want %d and none",
				addrPort, dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
		}
	}
	for _, key := range []string{
		RepairKey(env.cid, opsSpId, "rep1"),
		RetiredLegKey(env.cid, opsSpId, 2011),
		RetiredLegKey(env.cid, opsSpId, 2013),
	} {
		if env.exists(key) {
			t.Errorf("%q survived the clone", key)
		}
	}
	conf := env.spConf()
	if len(conf.GetRepairNameList()) != 0 ||
		len(conf.GetRetiredLegIdList()) != 0 {
		t.Errorf("the pool's lists: repairs %v, retired legs %v",
			conf.GetRepairNameList(), conf.GetRetiredLegIdList())
	}
}

// TestFinishCloneDeleteRefusesALostRecord: a repair or a retired leg the
// clone's group names whose key is missing is refused with nothing written:
// the record is what says which side to give back.
func TestFinishCloneDeleteRefusesALostRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    func(env *opsEnv) string
		reason string
	}{
		{"repair", func(env *opsEnv) string {
			return RepairKey(env.cid, opsSpId, "rep1")
		}, "repair record missing"},
		{"retired leg", func(env *opsEnv) string {
			return RetiredLegKey(env.cid, opsSpId, 2011)
		}, "retired leg record missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putDrainClone(true, nil)
			env.putDn("dn-e:9000", 704, 4, opsDnFree)
			grp := opsCloneGrp(true)
			env.chargeGrp(grp)
			env.putCloneRepairOf(grp, "rep1", 2001, opsDnA)
			env.putCloneRetired(grp, 2011, "dn-e:9000")
			env.setCloneGrp(drainCloneName, grp)
			env.delKey(tc.key(env))
			before := env.storeRev()
			env.wantNoWrite(env.finishDrainClone(), opFinishCloneDelete,
				tc.reason, before)
		})
	}
}
