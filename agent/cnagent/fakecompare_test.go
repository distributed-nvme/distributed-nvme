package cnagent

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The comparison of the fake agent with the cn agent (dnv-worker.md,
// Integration test plan, The fake against the real agents): the same
// syncups and rounds go to a real cn agent on this package's fake node and
// to the fake agent of integtest/agentfake, and what each answers is
// compared. After every syncup both must answer the same code and revision;
// once the real agent's rows have settled, a round of each must carry the
// same rows, by key, with the same status and reason. A difference passes
// only when agentfake.RealDiffs names it, with the exact status and reason
// of each row, the states of the cntlr's request it holds in and the rule
// that keeps the worker's tests on the fake true.

// cnTwin is one real cn agent and one fake, each behind a fake worker.
type cnTwin struct {
	t    *testing.T
	srv  *CnAgentServer
	node *fakeNode
	real *fakeWorker
	fake *fakeWorker
	// seen counts the uses of each entry of agentfake.RealDiffs.
	seen map[int]int
	// held is the last request each agent accepted for a cntlr, by its sp
	// id: the request whose states (agentfake.CntlrConds) an allowed
	// difference holds in.
	held map[uint64]*pb.SyncupCntlrRequest
}

// newCnTwin starts both agents: the real one with its probers on a short
// interval and its rounds starting their reads, as in production.
func newCnTwin(t *testing.T) *cnTwin {
	t.Helper()
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv, _ := startCnServer(t, node, 20*time.Millisecond)
	fleet := agentfake.NewFleet(agentfake.FleetConfig{Quiet: true})
	t.Cleanup(fleet.Close)
	const addr = "fake-cn:29529"
	if _, err := fleet.Add(addr, agentfake.Cn, agentfake.Config{}); err != nil {
		t.Fatalf("fake cn: %v", err)
	}
	conn, err := fleet.Dial(addr)
	if err != nil {
		t.Fatalf("dial the fake cn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &cnTwin{
		t:    t,
		srv:  srv,
		node: node,
		real: newFakeWorker(t, srv),
		fake: newFakeWorkerOn(t, pb.NewControllerNodeAgentClient(conn)),
		seen: make(map[int]int),
		held: make(map[uint64]*pb.SyncupCntlrRequest),
	}
}

// newFakeWorkerOn is newFakeWorker over a client of another server.
func newFakeWorkerOn(t *testing.T, client pb.ControllerNodeAgentClient) *fakeWorker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &fakeWorker{
		t:           t,
		client:      client,
		cntlrs:      make(map[uint64]pb.ControllerNodeAgent_CheckCntlrClient),
		ctx:         ctx,
		cancel:      cancel,
		callTimeout: scenarioCallTimeout,
	}
}

// sameReply fails the step unless both replies carry one code and one
// revision.
func (tw *cnTwin) sameReply(step string, real, fake interface {
	GetAgentReply() *pb.AgentReply
	GetRevision() uint64
}) {
	tw.t.Helper()
	rc, fc := real.GetAgentReply().GetCode(), fake.GetAgentReply().GetCode()
	if rc != fc || real.GetRevision() != fake.GetRevision() {
		tw.t.Errorf("%s: the cn agent answers code %d (%s) at revision %d, "+
			"the fake code %d (%s) at revision %d", step, rc,
			real.GetAgentReply().GetDetails(), real.GetRevision(), fc,
			fake.GetAgentReply().GetDetails(), fake.GetRevision())
	}
}

// syncupCn sends one SyncupCn to both.
func (tw *cnTwin) syncupCn(step string, req *pb.SyncupCnRequest) {
	tw.t.Helper()
	tw.sameReply(step, tw.real.syncupCn(req), tw.fake.syncupCn(req))
}

// syncupCntlr sends one SyncupCntlr to both. A leftover the cn agent names
// is a difference agentfake.RealDiffs allows: the twin then sends the
// syncup to the cn agent again, as the worker does (dnv-worker.md RW5), once
// the disconnects it started have ended (CN10), until it answers what the
// fake did.
func (tw *cnTwin) syncupCntlr(step string, req *pb.SyncupCntlrRequest) {
	tw.t.Helper()
	real, fake := tw.real.syncupCntlr(req), tw.fake.syncupCntlr(req)
	for range 10 {
		code := real.GetAgentReply().GetCode()
		d, differ := agentfake.CompareCode("SyncupCntlrReply", code,
			fake.GetAgentReply().GetCode())
		if !differ || d.Allowed < 0 {
			break
		}
		tw.seen[d.Allowed]++
		awaitDisconnects(tw.t, tw.srv)
		real = tw.real.syncupCntlr(req)
	}
	tw.sameReply(step, real, fake)
	if code := real.GetAgentReply().GetCode(); code == 0 ||
		code == common.ReplyCodeLeftover {
		tw.held[req.GetCntlrPointer().GetSpId()] = req
	}
}

// pushClone pushes the one-slice fixture's clone chunk to both.
func (tw *cnTwin) pushClone(step string, bitmap []byte) {
	tw.t.Helper()
	req := &pb.PushCloneBitmapRequest{
		ClusterId:    testCluster,
		CnId:         testCn,
		CntlrPointer: cntlrPtr(),
		CloneId:      testClone,
		SrcSliceIdx:  0,
		BmIdx:        0,
		Bitmap:       bitmap,
	}
	push := func(w *fakeWorker) *pb.PushCloneBitmapReply {
		ctx, cancel := w.callCtx()
		defer cancel()
		reply, err := w.client.PushCloneBitmap(ctx, req)
		if err != nil {
			tw.t.Fatalf("%s: PushCloneBitmap: %v", step, err)
		}
		return reply
	}
	real, fake := push(tw.real), push(tw.fake)
	if rc, fc := real.GetAgentReply().GetCode(), fake.GetAgentReply().GetCode(); rc != fc {
		tw.t.Errorf("%s: the cn agent answers code %d (%s), the fake code %d (%s)",
			step, rc, real.GetAgentReply().GetDetails(), fc,
			fake.GetAgentReply().GetDetails())
	}
}

// settledCntlr plays rounds of the real cntlr until two in a row carry the
// same rows, none PENDING, at revision: its leg rows read PENDING until its
// probers have run (CN11), and a round answers the read the round before it
// started (dnagent.md SH29).
func (tw *cnTwin) settledCntlr(ptr *pb.CntlrPointer, revision uint64) *pb.CheckCntlrReply {
	tw.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last map[string]string
	for {
		reply := tw.real.cntlrRound(ptr, revision, "")
		joinReads(tw.t, tw.srv)
		rows := rowStates(reply.GetCntlrInfo())
		if reply.GetRevision() == revision && maps.Equal(rows, last) &&
			!slices.ContainsFunc(slices.Collect(maps.Values(rows)),
				func(s string) bool { return strings.HasPrefix(s, "RES_STATUS_PENDING") }) {
			return reply
		}
		last = rows
		if time.Now().After(deadline) {
			tw.t.Fatalf("cntlr %s never settled at revision %d: code %d rev %d, "+
				"rows %v", cntlrPointerText(ptr), revision,
				reply.GetAgentReply().GetCode(), reply.GetRevision(), rows)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// compareCntlr compares a settled round of the real cntlr with a round of
// the fake's.
func (tw *cnTwin) compareCntlr(step string, ptr *pb.CntlrPointer, revision uint64) {
	tw.t.Helper()
	real := tw.settledCntlr(ptr, revision)
	fake := tw.fake.cntlrRound(ptr, revision, "")
	tw.sameReply(step, real, fake)
	tw.sameRows(step, "CntlrInfo", agentfake.CntlrConds(tw.held[ptr.GetSpId()]),
		real.GetCntlrInfo(), fake.GetCntlrInfo())
}

// compareCn compares a round of the CN on both, the real one after a read.
func (tw *cnTwin) compareCn(step string, revision uint64) {
	tw.t.Helper()
	tw.real.cnRound(revision)
	joinReads(tw.t, tw.srv)
	real := tw.real.cnRound(revision)
	fake := tw.fake.cnRound(revision)
	tw.sameReply(step, real, fake)
	tw.sameRows(step, "CnInfo", agentfake.Conds{}, real.GetCnInfo(), fake.GetCnInfo())
}

// sameRows fails the step on every row the two infos differ in that
// agentfake.RealDiffs does not name.
func (tw *cnTwin) sameRows(step, infoName string, conds agentfake.Conds, real, fake any) {
	tw.t.Helper()
	for _, d := range agentfake.CompareRows(infoName, conds, real, fake) {
		if d.Allowed >= 0 {
			tw.seen[d.Allowed]++
			continue
		}
		tw.t.Errorf("%s: %s", step, d.Text)
	}
}

// rowStates is every row of info by key, as "STATUS REASON".
func rowStates(info any) map[string]string {
	out := make(map[string]string)
	for key, row := range agentfake.RowsOf(info) {
		out[key] = row.GetStatus().String() + " " + row.GetReason().String()
	}
	return out
}

// standbyReq is the second cntlr's request, a cntlr of a pool of its own on
// this CN: the fixture's raid1 shape, primary or not, with clones.
func standbyReq(revision uint64, primary bool, clones ...*pb.Clone) *pb.SyncupCntlrRequest {
	req := cntlrReq(reqOpts{revision: revision, primary: primary, raid1: true,
		twoLegs: true, clones: clones})
	full := fullCntlrReq(revision, 1)
	req.CntlrPointer = full.CntlrPointer
	req.NqnToSubsystem = full.NqnToSubsystem
	return req
}

// TestFakeAgentMatchesTheCnAgent drives both agents through a primary and a
// standby that holds a clone, the demotion of the one and the promotion of
// the other, a repair with a new leg, a clone held by the level, its bitmap
// pushed and the clone enabled, and a migration of the meta leg and one data
// leg through the destination's flip and the finish, comparing them at every
// step.
func TestFakeAgentMatchesTheCnAgent(t *testing.T) {
	tw := newCnTwin(t)
	tw.syncupCn("the CN", twoCntlrCnReq(1, true, true))
	tw.syncupCntlr("the primary", noRepairReq(1))
	tw.syncupCntlr("the standby", standbyReq(1, false, cloneOf()))
	tw.compareCntlr("the primary", cntlrPtr(), 1)
	tw.compareCntlr("the standby", otherCntlrPtr(), 1)
	tw.compareCn("the CN", 1)

	// A cntlr the CN's syncup does not list is refused, and so is its
	// round (CN8, CN23).
	unlisted := standbyReq(1, false)
	unlisted.CntlrPointer = fullCntlrPtr(2)
	tw.syncupCntlr("a cntlr the CN does not list", unlisted)
	tw.sameReply("a round of a cntlr the CN does not list",
		tw.real.cntlrRound(fullCntlrPtr(2), 1, ""),
		tw.fake.cntlrRound(fullCntlrPtr(2), 1, ""))

	// The failover: the primary is demoted, the standby promoted.
	demoted := noRepairReq(2)
	demoted.Cntlr.Primary = false
	tw.syncupCntlr("the demotion", demoted)
	tw.syncupCntlr("the promotion", standbyReq(2, true))
	tw.compareCntlr("the demoted cntlr", cntlrPtr(), 2)
	tw.compareCntlr("the promoted cntlr", otherCntlrPtr(), 2)

	// A syncup below the revision the cntlr holds is refused as stale, and
	// so is the CN's (CN4; dnagent.md SH8).
	tw.syncupCntlr("a stale syncup", noRepairReq(1))
	tw.syncupCn("the CN at revision 2", twoCntlrCnReq(2, true, true))
	tw.syncupCn("a stale CN syncup", twoCntlrCnReq(1, true, true))

	// A repair with a new leg on the fixture cntlr, primary again.
	tw.syncupCntlr("the repair's pool", noRepairReq(3))
	tw.compareCntlr("primary again", cntlrPtr(), 3)
	tw.syncupCntlr("the repair", repairReq(4, pb.RepStage_REP_STAGE_CREATED, false))
	tw.compareCntlr("the repair", cntlrPtr(), 4)

	// A clone: declared at the level that holds it, its bitmap pushed, then
	// enabled (CN18, CN19).
	clone := func(revision uint64, level pb.SpLevel) *pb.SyncupCntlrRequest {
		return cntlrReq(reqOpts{revision: revision, primary: true, raid1: true,
			twoLegs: true, level: level, clones: []*pb.Clone{cloneOf()}})
	}
	tw.syncupCntlr("the held clone", clone(5, pb.SpLevel_SP_LEVEL_NO_CLONE))
	tw.compareCntlr("the held clone", cntlrPtr(), 5)
	tw.pushClone("the clone's bitmap", hexBytes(t, testSkipHex))
	tw.syncupCntlr("the clone", clone(6, pb.SpLevel_SP_LEVEL_READWRITE))
	tw.compareCntlr("the clone", cntlrPtr(), 6)
	tw.syncupCntlr("the clone gone", noRepairReq(7))
	tw.compareCntlr("the clone gone", cntlrPtr(), 7)

	// A migration of the meta leg and one data leg, the other data leg
	// serving beside them: the destination side before its flip, then
	// provisioned, so that the cutover can start, then the source dropped
	// at the finish.
	migr := func(revision uint64, provisioned bool) *pb.SyncupCntlrRequest {
		return cntlrReq(reqOpts{revision: revision, primary: true, raid1: true,
			twoLegs: true, extraSide: true, unprovisionedExtraSide: !provisioned})
	}
	tw.syncupCntlr("the migration", migr(8, false))
	tw.compareCntlr("the migration", cntlrPtr(), 8)
	tw.syncupCntlr("the destination's flip", migr(9, true))
	tw.compareCntlr("the destination's flip", cntlrPtr(), 9)
	finished := migr(10, true)
	for _, slice := range finished.GetIdToSlice() {
		for _, grp := range slices.Concat(slice.GetMetaGrpList(), slice.GetDataGrpList()) {
			for _, leg := range grp.GetLegList() {
				if len(leg.GetSideList()) == 2 {
					leg.SideList = leg.SideList[1:]
				}
			}
		}
	}
	tw.syncupCntlr("the migration's finish", finished)
	tw.compareCntlr("the migration's finish", cntlrPtr(), 10)

	for i, d := range agentfake.RealDiffs {
		if !slices.Contains([]string{"CntlrInfo", "CnInfo", "SyncupCntlrReply"}, d.Info) {
			continue
		}
		if tw.seen[i] == 0 {
			t.Errorf("agentfake.RealDiffs[%d] (%s %v, code %d) names no "+
				"difference this comparison meets", i, d.Info, d.Rows, d.Code)
		}
	}
}
