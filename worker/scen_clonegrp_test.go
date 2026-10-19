package worker

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenario cases of a clone's clone meta group
// (architecture.md, Terminology and object model): the sp coordinator drives
// its sides as a slice's (dnv-worker.md RW14, RW15, RW18), leg repair runs on
// it with its precondition of its own (AR8) and skips a clone being deleted,
// and the clone drain's final step gives back the sides the coordinator
// drove (CLD9). The clone is planted as CreateClone places one in the
// standard RAID1 pool (putStdClone): its two legs on dn 1 and dn 2, dn 3
// left for a spare. The fakes derive the rows of the clone's group and legs
// on every cntlr whose request carries the clone, as for a slice's group
// (dnv-worker.md, Integration test plan, The fake agent).

// stdCloneSpec is the clone every case of this file plants, onto the
// standard pool's thin device.
func stdCloneSpec(name string) plant.CloneSpec {
	return plant.CloneSpec{
		Name: name, DstTd: "td0", SrcSliceCnt: 1,
		SrcNqn: "nqn.2026-10.io.dnv-wh:src", SrcNsIdx: 1,
		SrcStripeSize: 1 << 20, SrcBlockSize: 1 << 16,
	}
}

// cloneGrp is a clone's clone meta group as the pool's record holds it now.
func (c *stdCluster) cloneGrp(t *testing.T, name string) *pb.Group {
	t.Helper()
	clone, ok := c.load(t).Clones[name]
	if !ok || clone.GetMetaGrp() == nil {
		t.Fatalf("clone %s has no clone meta group in the pool's record", name)
	}
	return clone.GetMetaGrp()
}

// failLegs makes the primary report several legs' rows ERROR in one lever
// edit, so that their health epochs are set in the same round (AR8's case 1
// trigger once leg_unhealthy has run out).
func (c *stdCluster) failLegs(t *testing.T, legIds ...uint64) {
	t.Helper()
	agent, key := c.cntlrAgent(t, 1)
	setLever(t, agent, key, func(o *agentfake.Object) {
		if o.Rows == nil {
			o.Rows = make(map[string]agentfake.Row)
		}
		for _, legId := range legIds {
			o.Rows[fmt.Sprintf("leg_id_to_leg.%d", legId)] =
				rowLever(pb.ResStatus_RES_STATUS_ERROR, "", true)
		}
	})
}

// healLeg drops a leg's ERROR row on the primary, so its next round reads the
// row OK and clears the leg's health epoch (HL2).
func (c *stdCluster) healLeg(t *testing.T, legId uint64) {
	t.Helper()
	agent, key := c.cntlrAgent(t, 1)
	clearRow(t, agent, key, fmt.Sprintf("leg_id_to_leg.%d", legId))
}

// ofClone matches a record that names the clone by its name and clone_id and
// names no slice.
func ofClone(clone plant.Clone) func(logRecord) bool {
	return func(rec logRecord) bool {
		_, slice := rec.attrs["slice_id"]
		return !slice && rec.str("clone_name") == clone.Name &&
			rec.num("clone_id") == clone.Id
	}
}

// TestScenCloneMetaGroupDrivenAndFlipped pins RW14, RW15 and RW18 on a
// clone's clone meta group: once the clone is planted, each of its sides'
// fakes is sent a SyncupSide, the first with the side not provisioned, every
// one with the group's length to zero, the data-group length and not the
// whole leg span, and no migration conf; the fake's zeroing done, the
// coordinator flips each side through the clone flip, whose record names
// the clone and no slice; the next fan-out tells each side it is
// provisioned; the cntlrs' requests carry the clone with its group and its
// sides provisioned; and the pool converges with no health epoch set, the
// primary's leg rows of the group reading OK.
func TestScenCloneMetaGroupDrivenAndFlipped(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	clone := c.putStdClone(t, stdCloneSpec("c0"))
	conv := c.waitConverged(t)

	state := c.load(t)
	grp := state.Clones["c0"].GetMetaGrp()
	blockSize := state.Conf.GetBdevConf().GetDmPoolConf().GetDataBlockSize()
	wantZero := (grp.GetMetaBlocks() + 1) * blockSize
	if span := (grp.GetMetaBlocks() + grp.GetDataBlocks()) * blockSize; wantZero == span {
		t.Fatalf("the group's data-group length %d equals its leg span: "+
			"the case cannot tell the two apart", wantZero)
	}
	flips := h.records(w, msgFlipApplied)
	for i, sideId := range clone.SideIds {
		legId, dnId := clone.LegIds[i], uint64(i+1)
		key := agentfake.SideKey(stdSpId, legId, sideId)
		syncups := c.dnAgent(dnId).Record(key).Syncups
		if len(syncups) == 0 {
			t.Fatalf("side %d on dn %d got no SyncupSide", sideId, dnId)
		}
		first := syncups[0].(*pb.SyncupSideRequest)
		if first.GetSideConf().GetProvisioned() {
			t.Errorf("side %d's first SyncupSide says provisioned", sideId)
		}
		for _, msg := range syncups {
			req := msg.(*pb.SyncupSideRequest)
			if got := req.GetSideConf().GetZeroBytes(); got != wantZero {
				t.Errorf("side %d told zero_bytes %d at revision %d, want "+
					"the data-group length %d", sideId, got,
					req.GetRevision(), wantZero)
			}
			if req.GetMigrSrcConf() != nil || req.GetMigrDstConf() != nil {
				t.Errorf("side %d told a migration conf: %v", sideId, req)
			}
		}
		last := syncups[len(syncups)-1].(*pb.SyncupSideRequest)
		if !last.GetSideConf().GetProvisioned() ||
			last.GetRevision() != conv.spRev {
			t.Errorf("side %d's last SyncupSide: provisioned %t at revision "+
				"%d, want provisioned at %d", sideId,
				last.GetSideConf().GetProvisioned(), last.GetRevision(),
				conv.spRev)
		}
		n := 0
		for _, rec := range flips {
			if rec.str("kind") != flipKindProvisioned ||
				rec.num("side_id") != sideId {
				continue
			}
			n++
			if !ofClone(clone)(rec) || rec.num("leg_id") != legId ||
				rec.num("revision") == 0 || rec.num("revision") > conv.spRev {
				t.Errorf("side %d's flip record %v, want clone %s (%d), "+
					"leg %d, a revision up to %d", sideId, rec.attrs,
					clone.Name, clone.Id, legId, conv.spRev)
			}
		}
		if n != 1 {
			t.Errorf("%d flip records of side %d, want 1", n, sideId)
		}
	}
	for _, cntlrId := range []uint64{1, 2} {
		agent, key := c.cntlrAgent(t, cntlrId)
		msg, _, _ := lastSyncup(agent, key)
		clones := msg.(*pb.SyncupCntlrRequest).GetCloneList()
		if len(clones) != 1 || clones[0].GetMetaGrp().GetGrpId() != clone.GrpId {
			t.Fatalf("cntlr %d's last syncup carries clones %v, want c0 "+
				"with group %d", cntlrId, clones, clone.GrpId)
		}
		for _, leg := range clones[0].GetMetaGrp().GetLegList() {
			if side := singleSide(leg); side == nil || !side.GetProvisioned() {
				t.Errorf("cntlr %d told leg %d's side %v, want provisioned",
					cntlrId, leg.GetLegId(), side)
			}
		}
	}
	// The primary's rows of the group's legs, which its fake derives from
	// the clone its request carries: the rows HL2 judges, read OK here, and
	// so no leg of the group has a health epoch.
	primaryId := c.primary(t)
	agent, _ := c.cntlrAgent(t, primaryId)
	reply, err := agent.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{
			ClusterId: c.cl.Id,
			CntlrPointer: &pb.CntlrPointer{
				SpId: stdSpId, CntlrId: primaryId,
			},
		})
	if err != nil || reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("the primary's GetCntlrInfo: %v, %v",
			reply.GetAgentReply(), err)
	}
	rows := reply.GetCntlrInfo().GetLegIdToLeg()
	for _, legId := range clone.LegIds {
		if got := rows[legId].GetStatus(); got != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("the primary's row of the group's leg %d reads %v, "+
				"want OK", legId, got)
		}
	}
	if n := h.countRecs(w, msgReactionApplied, nil); n != 0 {
		t.Errorf("%d reactions applied, want none", n)
	}
}

// TestScenCloneLegRepair pins AR8 on a clone meta group: a leg of the clone's
// group the primary reports ERROR past leg_unhealthy gets a spare on dn 3,
// the one DN outside the group, through the clone-keyed create, which
// charges dn 3 one extent and its pointer; the coordinator drives the
// spare's side to provisioned and flips it through the clone flip; the next
// pass switches the spare in through the clone-keyed switch and parks the
// leg; every record names the clone and no slice. Once the parked leg reads
// OK again the pool converges.
func TestScenCloneLegRepair(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	clone := c.putStdClone(t, stdCloneSpec("c0"))
	c.waitConverged(t)

	before := c.dnLedgerOf(t, 3)
	spareLeg, spareSide := c.spareIds(t)
	legA := clone.LegIds[0]
	c.failLegs(t, legA)
	created := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareCreate, ""), numIs("leg_id", legA)))
	if !ofClone(clone)(created) || created.num("grp_id") != clone.GrpId ||
		created.num("spare_leg_id") != spareLeg ||
		created.str("addr_port") != stdDnAddr(3) {
		t.Fatalf("the spare create %v, want spare %d of clone %s's group %d "+
			"on dn 3", created.attrs, spareLeg, clone.Name, clone.GrpId)
	}
	switched := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareSwitch, ""), numIs("leg_id", legA)))
	if !ofClone(clone)(switched) ||
		switched.num("spare_leg_id") != spareLeg {
		t.Fatalf("the switch %v, want spare %d of clone %s", switched.attrs,
			spareLeg, clone.Name)
	}
	grp := c.cloneGrp(t, "c0")
	if !slices.Contains(legIds(grp.GetLegList()), spareLeg) ||
		!slices.Contains(legIds(grp.GetSpareLegList()), legA) {
		t.Fatalf("after the switch the group has legs %v and spares %v, "+
			"want leg %d in and leg %d parked", legIds(grp.GetLegList()),
			legIds(grp.GetSpareLegList()), spareLeg, legA)
	}
	flipped := h.countRecs(w, msgFlipApplied, and(ofClone(clone),
		numIs("side_id", spareSide)))
	if flipped != 1 {
		t.Errorf("%d clone flip records of the spare's side %d, want 1",
			flipped, spareSide)
	}
	after := c.dnLedgerOf(t, 3)
	if after.free != before.free-1 || after.ptrs == before.ptrs ||
		!after.keyed || after.rev != before.rev+1 {
		t.Errorf("dn 3 after the spare: %+v, before %+v; want one extent "+
			"and one pointer more, keyed, one bump", after, before)
	}
	for _, rec := range h.records(w, msgReactionApplied) {
		if rec.str("kind") != reactionSpareCreate &&
			rec.str("kind") != reactionSpareSwitch {
			t.Errorf("a reaction other than the repair: %v", rec.attrs)
		}
	}
	if n := h.countRecs(w, msgReactionApplied,
		reactionIs(reactionSpareCreate, "")); n != 1 {
		t.Errorf("%d spares created for one repair, want 1", n)
	}

	c.healLeg(t, legA)
	c.waitConverged(t)
}

// TestScenCloneLegRepairNeedsAHealthyLeg pins AR8's precondition of its own on
// a clone meta group: with both legs of the clone's group reported ERROR past
// leg_unhealthy, every pass logs `reaction skipped` (no_healthy_leg) for each
// of them, naming the clone, and neither creates nor switches a spare, so dn
// 3 is never charged; once one leg reads OK again, the other is repaired, a
// spare created on dn 3 and switched in.
func TestScenCloneLegRepairNeedsAHealthyLeg(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	clone := c.putStdClone(t, stdCloneSpec("c0"))
	c.waitConverged(t)

	before := c.dnLedgerOf(t, 3)
	legA, legB := clone.LegIds[0], clone.LegIds[1]
	c.failLegs(t, legA, legB)
	for _, legId := range []uint64{legA, legB} {
		h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 3,
			and(and(reactionIs(reactionSpareCreate, reasonNoHealthyLeg),
				numIs("leg_id", legId)), ofClone(clone)))
	}
	if n := h.countRecs(w, msgReactionApplied, nil); n != 0 {
		t.Fatalf("%d reactions applied with every leg of the group "+
			"unhealthy, want 0", n)
	}
	if got := c.dnLedgerOf(t, 3); got != before {
		t.Fatalf("dn 3 while the group has no healthy leg: %+v, want %+v",
			got, before)
	}

	spareLeg, _ := c.spareIds(t)
	c.healLeg(t, legB)
	created := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareCreate, ""), numIs("leg_id", legA)))
	if !ofClone(clone)(created) || created.num("spare_leg_id") != spareLeg ||
		created.str("addr_port") != stdDnAddr(3) {
		t.Fatalf("the spare create %v, want spare %d of clone %s on dn 3",
			created.attrs, spareLeg, clone.Name)
	}
	h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(and(reactionIs(reactionSpareSwitch, ""), numIs("leg_id", legA)),
			numIs("spare_leg_id", spareLeg)))
}

// TestScenDeletingCloneIsNotRepaired pins AR8's skip of a clone being deleted
// (CLD9 gives its group back): a leg of the clone's group is reported ERROR,
// its health epoch is set and judged, and then the clone is latched, which
// takes the clone, and so its leg rows, out of the cntlrs' requests (CLD5)
// while the leg keeps its epoch and its verdict. The drain's final step is
// held for the case by taking dn 2's revision key away, which its
// transaction refuses ("dn rev not found"), so every pass logs `clone drain
// failed` and the clone stays latched past leg_unhealthy: no pass creates or
// switches a spare for it, no record of a reaction names it, and dn 3 is not
// charged. With the key back, the drain gives the group back and the pool
// converges. The leg's threshold is longer than the standard one, so that the
// latch lands well inside it on a loaded machine.
func TestScenDeletingCloneIsNotRepaired(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, func(spec *stdSpec) { spec.threshold.LegUnhealthy = 4 })
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()
	before := map[uint64]dnLedger{}
	for id := uint64(1); id <= 3; id++ {
		before[id] = c.dnLedgerOf(t, id)
	}
	clone := c.putStdClone(t, stdCloneSpec("c0"))
	c.waitConverged(t)

	legA := clone.LegIds[0]
	c.failLegs(t, legA)
	var epoch uint64
	h.waitUntil(t, 20*time.Second, "leg A's health epoch", func() (bool, string) {
		for _, leg := range c.cloneGrp(t, "c0").GetLegList() {
			if leg.GetLegId() == legA {
				epoch = leg.GetErrEpoch()
			}
		}
		return epoch != 0, "not set"
	})
	// The whole value goes back later, its handle, dn 2's addr_port, with
	// its revision.
	revKey := model.DnRevKey(stdDnShard(2), c.cl.Id, 2)
	saved := &pb.DnRev{}
	c.mustGet(t, revKey, saved)
	if _, _, err := plant.DelRev(ctx, h.cli, c.cl, plant.RevDn,
		stdDnShard(2), 2); err != nil {
		t.Fatalf("take dn 2's revision key away: %v", err)
	}
	if _, err := plant.SetCloneDeleting(ctx, h.cli, c.cl, c.sp.Ref, "c0"); err != nil {
		t.Fatalf("SetCloneDeleting: %v", err)
	}
	h.waitRecord(t, 20*time.Second, w, msgCloneDrainFailed, ofClone(clone))
	if epoch+uint64(c.spec.threshold.GetLegUnhealthy()) > uint64(time.Now().Unix()) {
		h.waitUntil(t, 20*time.Second, "leg_unhealthy to run out on leg A",
			func() (bool, string) {
				now := uint64(time.Now().Unix())
				return reached(now, epoch, c.spec.threshold.GetLegUnhealthy()),
					fmt.Sprintf("now %d, epoch %d", now, epoch)
			})
	}
	// Three passes with the leg due and the clone still latched.
	from := h.countRecs(w, msgCloneDrainFailed, ofClone(clone))
	h.waitCount(t, 20*time.Second, w, msgCloneDrainFailed, from+3, ofClone(clone))
	if got := c.cloneGrp(t, "c0").GetLegList()[0]; got.GetLegId() != legA ||
		got.GetErrEpoch() == 0 {
		t.Fatalf("leg A of the latched clone: %v, want it in its place with "+
			"its health epoch", got)
	}
	for _, msg := range []string{msgReactionApplied, msgReactionSkipped} {
		if recs := h.records(w, msg); len(recs) != 0 {
			t.Errorf("%d %q records, want none; the first: %v", len(recs), msg,
				recs[0].attrs)
		}
	}
	if got := c.dnLedgerOf(t, 3); got != before[3] {
		t.Errorf("dn 3 while the latched clone's leg is due: %+v, want %+v",
			got, before[3])
	}

	if err := h.cli.RunSTM(ctx, func(s etcdutil.STM) error {
		s.Put(revKey, saved)
		return nil
	}); err != nil {
		t.Fatalf("put dn 2's revision key back: %v", err)
	}
	h.waitRecord(t, 20*time.Second, w, msgCloneDrainDone, ofClone(clone))
	c.healLeg(t, legA)
	c.waitConverged(t)
	for id := uint64(1); id <= 3; id++ {
		want := before[id]
		if id <= 2 {
			want.rev += 2
		}
		if got := c.dnLedgerOf(t, id); got != want {
			t.Errorf("dn %d after the clone's drain: %+v, want %+v", id, got,
				want)
		}
	}
}

// TestScenCloneDrainGivesBackDrivenSides pins CLD9 on the sides the
// coordinator drove: the clone's sides are driven to provisioned and
// flipped (RW18), the clone is latched and drained, and the final step
// gives dn 1 and dn 2 their extents, pointers and capacity keys back with one
// DnRev bump each; each DN's fake is then told a side list without the
// clone's side, the coordinator stops the clone's side children, so their
// fakes get no further round, and the pool converges without them.
func TestScenCloneDrainGivesBackDrivenSides(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()
	before := map[uint64]dnLedger{}
	for id := uint64(1); id <= 3; id++ {
		before[id] = c.dnLedgerOf(t, id)
	}
	clone := c.putStdClone(t, stdCloneSpec("c0"))
	c.waitConverged(t)
	keys := make([]string, len(clone.SideIds))
	for i, sideId := range clone.SideIds {
		keys[i] = agentfake.SideKey(stdSpId, clone.LegIds[i], sideId)
		_, _, side, ok := c.sideOf(t, sideId)
		if !ok || !side.GetProvisioned() {
			t.Fatalf("clone side %d before the latch: found %t, %v, want "+
				"provisioned", sideId, ok, side)
		}
		if n := h.countRecs(w, msgFlipApplied, and(ofClone(clone),
			numIs("side_id", sideId))); n != 1 {
			t.Fatalf("%d clone flip records of side %d, want 1", n, sideId)
		}
	}

	if _, err := plant.SetCloneDeleting(ctx, h.cli, c.cl, c.sp.Ref, "c0"); err != nil {
		t.Fatalf("SetCloneDeleting: %v", err)
	}
	h.waitRecord(t, 20*time.Second, w, msgCloneDrainDone, ofClone(clone))
	c.waitConverged(t)
	for id := uint64(1); id <= 3; id++ {
		want := before[id]
		if id <= 2 {
			want.rev += 2
		}
		if got := c.dnLedgerOf(t, id); got != want {
			t.Errorf("dn %d after the clone's drain: %+v, want %+v", id, got,
				want)
		}
	}
	rounds := make([]int, len(keys))
	for i, key := range keys {
		dn := c.dnAgent(uint64(i + 1))
		msg, _, _ := storedSyncup(dn, agentfake.DnKey)
		for _, ptr := range msg.(*pb.SyncupDnRequest).GetSidePointerList() {
			if ptr.GetSideId() == clone.SideIds[i] {
				t.Errorf("dn %d's fake still keeps clone side %d", i+1,
					clone.SideIds[i])
			}
		}
		rounds[i] = len(dn.Record(key).Rounds)
	}
	// Two more rounds of a slice's side on each DN: a running child of a
	// clone side would have sent one meanwhile.
	h.waitRounds(t, c.dnAgent(1), 2, agentfake.SideKey(stdSpId, 6, 10))
	h.waitRounds(t, c.dnAgent(2), 2, agentfake.SideKey(stdSpId, 7, 11))
	for i, key := range keys {
		if got := len(c.dnAgent(uint64(i + 1)).Record(key).Rounds); got != rounds[i] {
			t.Errorf("clone side %d got %d rounds after the drain, want "+
				"none", clone.SideIds[i], got-rounds[i])
		}
	}
	if names := c.load(t).Conf.GetCloneNameList(); len(names) != 0 {
		t.Errorf("the pool still lists clones %v", names)
	}
}
