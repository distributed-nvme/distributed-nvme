package dnagent

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The comparison of the fake agent with the dn agent (dnv-worker.md,
// Integration test plan, The fake against the real agents): the same
// syncups and rounds go to a real dn agent on this package's fake node and
// to the fake agent of integtest/agentfake, and what each answers is
// compared, as the cn package's TestFakeAgentMatchesTheCnAgent compares the
// cn role. After every syncup both must answer the same code and revision;
// once the real agent's rows have settled, a round of each must carry the
// same rows, by key, with the same status and reason, save the differences
// agentfake.RealDiffs names, each with the exact status and reason of each
// row and the states of the side's request it holds in.

// dnTwin is one real dn agent and one fake, each behind a fake worker.
type dnTwin struct {
	t    *testing.T
	srv  *DnAgentServer
	node *fakeNode
	real *fakeWorker
	fake *fakeWorker
	seen map[int]int
	// held is the last request each agent accepted for a side, by its side
	// id: the request whose states (agentfake.SideConds) an allowed
	// difference holds in.
	held map[uint64]*pb.SyncupSideRequest
}

// newDnTwin starts both agents, the real one with its rounds starting their
// reads, as in production (SH29).
func newDnTwin(t *testing.T, seen map[int]int) *dnTwin {
	t.Helper()
	srv, node := newTestServer(t)
	srv.roundReads = true
	fleet := agentfake.NewFleet(agentfake.FleetConfig{Quiet: true})
	t.Cleanup(fleet.Close)
	const addr = "fake-dn:29528"
	if _, err := fleet.Add(addr, agentfake.Dn, agentfake.Config{}); err != nil {
		t.Fatalf("fake dn: %v", err)
	}
	conn, err := fleet.Dial(addr)
	if err != nil {
		t.Fatalf("dial the fake dn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &dnTwin{
		t:    t,
		srv:  srv,
		node: node,
		real: newFakeWorker(t, srv),
		fake: &fakeWorker{
			t:      t,
			client: pb.NewDiskNodeAgentClient(conn),
			sides:  make(map[uint64]pb.DiskNodeAgent_CheckSideClient),
			ctx:    ctx,
			cancel: cancel,
		},
		seen: seen,
		held: make(map[uint64]*pb.SyncupSideRequest),
	}
}

// sameReply fails the step unless both replies carry one code and one
// revision.
func (tw *dnTwin) sameReply(step string, real, fake interface {
	GetAgentReply() *pb.AgentReply
	GetRevision() uint64
}) {
	tw.t.Helper()
	rc, fc := real.GetAgentReply().GetCode(), fake.GetAgentReply().GetCode()
	if rc != fc || real.GetRevision() != fake.GetRevision() {
		tw.t.Errorf("%s: the dn agent answers code %d (%s) at revision %d, "+
			"the fake code %d (%s) at revision %d", step, rc,
			real.GetAgentReply().GetDetails(), real.GetRevision(), fc,
			fake.GetAgentReply().GetDetails(), fake.GetRevision())
	}
}

func (tw *dnTwin) syncupDn(step string, req *pb.SyncupDnRequest) {
	tw.t.Helper()
	tw.sameReply(step, tw.real.syncupDn(req), tw.fake.syncupDn(req))
}

func (tw *dnTwin) syncupSide(step string, req *pb.SyncupSideRequest) {
	tw.t.Helper()
	real := tw.real.syncupSide(req)
	tw.sameReply(step, real, tw.fake.syncupSide(req))
	if code := real.GetAgentReply().GetCode(); code == 0 ||
		code == common.ReplyCodeLeftover {
		tw.held[req.GetSidePointer().GetSideId()] = req
	}
}

// settledSide plays rounds of the real side until two in a row carry the
// same rows at revision, joining the read each starts: a round answers the
// read the round before it started (SH29).
func (tw *dnTwin) settledSide(ptr *pb.SidePointer, revision uint64) *pb.CheckSideReply {
	tw.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last map[string]string
	for {
		reply := tw.real.sideRound(ptr, revision)
		joinReads(tw.t, tw.srv)
		rows := rowStates(reply.GetSideInfo())
		if reply.GetRevision() == revision && maps.Equal(rows, last) {
			return reply
		}
		last = rows
		if time.Now().After(deadline) {
			tw.t.Fatalf("side %s never settled at revision %d: code %d rev %d, "+
				"rows %v", sidePointerText(ptr), revision,
				reply.GetAgentReply().GetCode(), reply.GetRevision(), rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// compareSide compares a settled round of the real side with a round of
// the fake's.
func (tw *dnTwin) compareSide(step string, ptr *pb.SidePointer, revision uint64) {
	tw.t.Helper()
	real := tw.settledSide(ptr, revision)
	fake := tw.fake.sideRound(ptr, revision)
	tw.sameReply(step, real, fake)
	tw.sameRows(step, "SideInfo", agentfake.SideConds(tw.held[ptr.GetSideId()]),
		real.GetSideInfo(), fake.GetSideInfo())
}

// compareDn compares a round of the DN on both, the real one after a read.
func (tw *dnTwin) compareDn(step string, revision uint64) {
	tw.t.Helper()
	tw.real.dnRound(revision)
	joinReads(tw.t, tw.srv)
	real := tw.real.dnRound(revision)
	fake := tw.fake.dnRound(revision)
	tw.sameReply(step, real, fake)
	tw.sameRows(step, "DnInfo", agentfake.Conds{}, real.GetDnInfo(), fake.GetDnInfo())
}

func (tw *dnTwin) sameRows(step, infoName string, conds agentfake.Conds, real, fake any) {
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

// unflipped is a side's request before the sp worker's provisioned flip
// (architecture.md, Side provisioning protocol).
func unflipped(req *pb.SyncupSideRequest) *pb.SyncupSideRequest {
	out := proto.Clone(req).(*pb.SyncupSideRequest)
	out.SideConf.Provisioned = false
	return out
}

// TestFakeAgentMatchesTheDnAgent drives both agents through a side that
// provisions and is exported to its primary and its standby, a failover
// that trades the two, a repair's new leg, a second side provisioned on the
// same DN, a migration's destination and its source through the cutover,
// and the refusals of a side the DN does not list and of stale syncups,
// comparing them at every step. A DN knows no clone: a clone meta group's
// side is a side like the others.
func TestFakeAgentMatchesTheDnAgent(t *testing.T) {
	seen := make(map[int]int)
	tw := newDnTwin(t, seen)
	tw.syncupDn("the DN", legDnReq(1, testSide))
	side := legSideReq(1, testSide)
	tw.syncupSide("the side before its flip", unflipped(side))
	waitZeroed(t, tw.srv, testSide)
	tw.compareSide("the zeroed side", legPtr(testSide), 1)
	tw.syncupSide("the side's flip", side)
	tw.compareSide("the side", legPtr(testSide), 1)
	tw.compareDn("the DN", 1)

	// A side the DN's syncup does not list is refused, and so is its round
	// (DN8, SH9, SH25).
	tw.syncupSide("a side the DN does not list", legSideReq(1, testSide2))
	tw.sameReply("a round of a side the DN does not list",
		tw.real.sideRound(legPtr(testSide2), 1),
		tw.fake.sideRound(legPtr(testSide2), 1))

	tw.syncupSide("the failover", failoverSideReq(2, testSide))
	tw.compareSide("the failed-over side", legPtr(testSide), 2)
	// A syncup below the revision the side holds is refused as stale (SH8).
	tw.syncupSide("a stale syncup", legSideReq(1, testSide))

	tw.syncupDn("the new leg", legDnReq(2, testSide, testSide2))
	newLeg := legSideReq(2, testSide2)
	tw.syncupSide("the new leg before its flip", unflipped(newLeg))
	waitZeroed(t, tw.srv, testSide2)
	tw.syncupSide("the new leg's flip", newLeg)
	tw.compareSide("the new leg", legPtr(testSide2), 2)
	tw.compareDn("the DN with two sides", 2)
	tw.syncupDn("a stale DN syncup", legDnReq(1, testSide, testSide2))

	// A migration's destination, on a DN of its own.
	mig := newDnTwin(t, seen)
	mig.syncupDn("the destination's DN", dnReq(1, testSide))
	dst := pendingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	mig.syncupSide("the destination before its flip", unflipped(dst))
	waitZeroed(t, mig.srv, testSide)
	mig.compareSide("the zeroed destination", sidePtr(testSide), 1)
	mig.syncupSide("the destination's flip", dst)
	mig.compareSide("the destination waiting for the cutover", sidePtr(testSide), 1)
	mig.syncupSide("the cutover", migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	mig.compareSide("the destination after the cutover", sidePtr(testSide), 2)

	// The migration's source, on a DN of its own: a side that serves, told
	// its destination waits for the cutover, then that the cutover started.
	src := newDnTwin(t, seen)
	src.syncupDn("the source's DN", dnReq(1, testSide))
	serving := sideReq(1, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	src.syncupSide("the source before its flip", unflipped(serving))
	waitZeroed(t, src.srv, testSide)
	src.syncupSide("the source's flip", serving)
	src.compareSide("the source", sidePtr(testSide), 1)
	waiting := migrSrcReq(2)
	waiting.MigrSrcConf.DstProvisioned = false
	waiting.MigrSrcConf.CutoverPending = true
	src.syncupSide("the source waiting for the cutover", waiting)
	src.compareSide("the source waiting for the cutover", sidePtr(testSide), 2)
	src.syncupSide("the source's cutover", migrSrcReq(3))
	src.compareSide("the source after the cutover", sidePtr(testSide), 3)

	for i, d := range agentfake.RealDiffs {
		if !slices.Contains([]string{"SideInfo", "DnInfo"}, d.Info) {
			continue
		}
		if seen[i] == 0 {
			t.Errorf("agentfake.RealDiffs[%d] (%s %v, code %d) names no "+
				"difference this comparison meets", i, d.Info, d.Rows, d.Code)
		}
	}
}
