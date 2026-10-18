package worker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker smoke cases (dnv-worker.md, Integration test plan): one
// whole worker over the harness's etcd and fakes converges a planted pool, and
// fails a disabled primary over. The harness's own tests are in
// harness_self_test.go.

// TestWholeWorkerConverges pins the convergence of a planted pool under one
// whole worker: every disk node, side, controller node and cntlr fake gets
// its syncup at the revision its key holds and its check rounds answer it
// (waitConverged), the sides are told the pool as it was planted before the
// provisioned flip moves it on (RW18), and the discovery record lists both
// cntlrs (architecture.md [D18]).
func TestWholeWorkerConverges(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	plantedSp := c.spRev(t)
	plantedDn := map[uint64]uint64{}
	for id := uint64(1); id <= uint64(c.spec.dnCnt); id++ {
		plantedDn[id] = c.dnRev(t, id)
	}
	plantedCn := map[uint64]uint64{}
	for id := uint64(1); id <= uint64(c.spec.cnCnt); id++ {
		plantedCn[id] = c.cnRev(t, id)
	}

	start := time.Now()
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	conv := c.waitConverged(t)
	t.Logf("converged %v after the worker started, at SpRev %d (planted %d)",
		conv.at.Sub(start).Round(time.Millisecond), conv.spRev, plantedSp)

	// No flip moves a node's revision: the nodes converged at the revisions
	// the plant left.
	for id, rev := range plantedDn {
		if conv.dnRevs[id] != rev {
			t.Errorf("dn %d converged at revision %d, planted at %d",
				id, conv.dnRevs[id], rev)
		}
	}
	for id, rev := range plantedCn {
		if conv.cnRevs[id] != rev {
			t.Errorf("cn %d converged at revision %d, planted at %d",
				id, conv.cnRevs[id], rev)
		}
	}
	// The provisioned flip needs every side's report of the planted pool, so
	// each side got a syncup at the planted revision first, and the flip
	// moved the pool past it.
	if conv.spRev <= plantedSp {
		t.Errorf("SpRev converged at %d, not past the planted %d",
			conv.spRev, plantedSp)
	}
	for _, side := range stdSides {
		agent, key := c.sideAgent(t, side.sideId)
		var revs []uint64
		for _, msg := range agent.Record(key).Syncups {
			revs = append(revs, msg.(*pb.SyncupSideRequest).GetRevision())
		}
		if len(revs) == 0 || revs[0] != plantedSp {
			t.Errorf("side %d: syncup revisions %v, want the first at the "+
				"planted %d", side.sideId, revs, plantedSp)
		}
	}
	// Both cntlrs are enabled and clean, so the discovery record lists both,
	// in cntlr_id_list order.
	state := c.load(t)
	var cntlrs []*pb.Cntlr
	for _, id := range state.Conf.GetCntlrIdList() {
		cntlrs = append(cntlrs, state.Cntlrs[id])
	}
	if got, want := c.cdcEntry(t).GetNvmeTrConfList(), model.CdcTrConfList(cntlrs); len(got) != 2 ||
		!trConfsEqual(got, want) {
		t.Errorf("CdcEntry lists %v, want both cntlrs %v", got, want)
	}
	if c.spOwner() != w || c.dnOwner(1) != w || c.cnOwner(1) != w {
		t.Errorf("the lone worker owns sp %v, dn 1 %v, cn 1 %v",
			c.spOwner(), c.dnOwner(1), c.cnOwner(1))
	}
	if len(h.records(w, msgSyncupResult)) == 0 {
		t.Errorf("no syncup result record is the worker's")
	}
}

// TestWholeWorkerFailsOverADisabledPrimary pins AR5's disabled trigger under a
// whole worker: once the primary is disabled, the next pass elects the clean
// standby, the pool's records name it primary, its fake is promoted, the old
// primary's fake demoted and every side told the new primary, the discovery
// record follows the listing rule (architecture.md [D18]), and the pool
// converges again under the new primary, in well under the 30 s a scenario
// case may take.
func TestWholeWorkerFailsOverADisabledPrimary(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	if got := c.primary(t); got != 1 {
		t.Fatalf("the converged pool's primary is cntlr %d, want 1", got)
	}
	newAgent, newKey := c.cntlrAgent(t, 2)
	oldAgent, oldKey := c.cntlrAgent(t, 1)
	newCnId := c.cnConf(t, 2).GetCnId()

	disabled := true
	disabledAt := time.Now()
	if _, _, err := plant.SetCntlr(context.Background(), h.cli, c.cl, c.sp.Ref, 1,
		plant.CntlrFlags{Disabled: &disabled}); err != nil {
		t.Fatalf("disable cntlr 1: %v", err)
	}

	var failRev uint64
	h.waitUntil(t, 20*time.Second, "the pool's records to name cntlr 2 primary",
		func() (bool, string) {
			state := c.load(t)
			if !state.Cntlrs[2].GetPrimary() || state.Cntlrs[1].GetPrimary() {
				return false, "cntlr 1 still primary"
			}
			failRev = state.SpRevision
			return true, ""
		})
	committedAt := time.Now()
	var promotedAt time.Time
	h.waitUntil(t, 10*time.Second, "the promotion of cntlr 2 on cn 2's fake",
		func() (bool, string) {
			for _, msg := range newAgent.Record(newKey).Syncups {
				req := msg.(*pb.SyncupCntlrRequest)
				if req.GetCntlr().GetPrimary() && req.GetRevision() >= failRev {
					promotedAt = time.Now()
					return true, ""
				}
			}
			return false, "no SyncupCntlr with primary set at the failover's revision"
		})
	took := promotedAt.Sub(disabledAt)
	t.Logf("disable to failover commit %v, to the promotion %v",
		committedAt.Sub(disabledAt).Round(time.Millisecond),
		took.Round(time.Millisecond))
	if took > 10*time.Second {
		t.Errorf("the promotion came %v after the disable, want well under 30 s", took)
	}

	demoted := false
	for _, msg := range oldAgent.Record(oldKey).Syncups {
		req := msg.(*pb.SyncupCntlrRequest)
		if !req.GetCntlr().GetPrimary() && req.GetRevision() >= failRev {
			demoted = true
		}
	}
	if !demoted {
		t.Errorf("cn 1's fake got no demotion of cntlr 1 at revision %d or later", failRev)
	}
	for _, side := range stdSides {
		agent, key := c.sideAgent(t, side.sideId)
		told := false
		for _, msg := range agent.Record(key).Syncups {
			req := msg.(*pb.SyncupSideRequest)
			if req.GetRevision() >= failRev &&
				req.GetSideConf().GetPrimaryCnId() == newCnId {
				told = true
			}
		}
		if !told {
			t.Errorf("side %d was never told cn %d is primary", side.sideId, newCnId)
		}
	}

	conv := c.waitConverged(t)
	state := c.load(t)
	if !state.Cntlrs[1].GetDisabled() || state.Cntlrs[1].GetErrEpoch() != 0 {
		t.Errorf("cntlr 1 after the failover: disabled %t, err_epoch %d; want "+
			"disabled with no epoch (no threshold was waited out)",
			state.Cntlrs[1].GetDisabled(), state.Cntlrs[1].GetErrEpoch())
	}
	var cntlrs []*pb.Cntlr
	for _, id := range state.Conf.GetCntlrIdList() {
		cntlrs = append(cntlrs, state.Cntlrs[id])
	}
	got := c.cdcEntry(t).GetNvmeTrConfList()
	if want := model.CdcTrConfList(cntlrs); !trConfsEqual(got, want) ||
		len(got) != 1 || !proto.Equal(got[0], state.Cntlrs[2].GetNvmeTrConf()) {
		t.Errorf("CdcEntry lists %v, want only the new primary %v",
			got, state.Cntlrs[2].GetNvmeTrConf())
	}
	failovers := 0
	for _, rec := range h.records(w, msgReactionApplied) {
		if rec.str("kind") == reactionFailover {
			failovers++
		}
	}
	if failovers != 1 {
		t.Errorf("%d failovers applied, want 1", failovers)
	}
	t.Logf("converged again at SpRev %d, %v after the disable",
		conv.spRev, conv.at.Sub(disabledAt).Round(time.Millisecond))
}

// trConfsEqual compares two transport lists in order.
func trConfsEqual(a, b []*pb.NvmeTrConf) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
