package csi

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The tests of a detach's wait for the primary's cut (csi.md CSI10): a
// detach returns once a report of the pool's primary cntlr made at the
// detach's revision or later reads the volume's subsystem row OK.

// attachedVolume creates the test volume and attaches it to node A, whose
// write waits for one ok report of the primary (attachwait_test.go), and
// then clears the fake's record of inspects, so that a test counts its own
// detach's. It returns the volume id, the subsystem's ss_id and the pool.
func attachedVolume(
	t *testing.T, c *controller, g *fakeGateway,
) (string, uint64, *fakePool) {
	t.Helper()
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	_, err = c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: testNodeA,
		VolumeCapability: mountCap(fsExt4, rwo),
	})
	wantCode(t, err, codes.OK, "attach to A")
	p := g.pool(fakeClusterOne, testVolume)
	g.mu.Lock()
	defer g.mu.Unlock()
	ssId := p.ss[subsystemNqnOf(testVolume)].GetSsId()
	if len(g.inspected) != 1 {
		t.Fatalf("the attach to A polled the primary %d times, want 1",
			len(g.inspected))
	}
	g.inspected = nil
	g.calls = slices.DeleteFunc(g.calls, func(m string) bool {
		return m == "InspectCntlr"
	})
	return id, ssId, p
}

// report is a primary's report at a revision with one subsystem row.
func report(
	rev uint64, ssId uint64, st pb.ResStatus, details string,
) *pb.InspectCntlrReply {
	return &pb.InspectCntlrReply{
		AppliedRevision: rev,
		CntlrInfo: &pb.CntlrInfo{
			SsIdToSubsystem: map[uint64]*pb.ResInfo{
				ssId: {Status: st, Details: details},
			},
		},
	}
}

// pendingDetails is the text of a row whose cut has not held.
const pendingDetails = "host " + testNodeA + " still connected"

func unpublish(
	ctx context.Context, c *controller, id string, node string,
) error {
	_, err := c.ControllerUnpublishVolume(ctx,
		&csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: node})
	return err
}

// The detach waits after its write: a report older than the write's
// revision, then a pending row at it, are polled past, and the ok row at it
// ends the wait.
func TestTheDetachWaitsForTheCutAtItsRevision(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := attachedVolume(t, c, g)
	g.mu.Lock()
	before := p.rev
	steps := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		steps++
		switch steps {
		case 1:
			return report(before, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
		case 2:
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK, "detach A")
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.rev != before+1 {
		t.Fatalf("pool revision %d after the detach's write, want %d",
			p.rev, before+1)
	}
	if steps != 3 {
		t.Errorf("%d polls of the primary, want 3: an older report, a "+
			"pending row, the ok row", steps)
	}
	want := []uint64{fakeCntlrIdBase + 1, fakeCntlrIdBase + 1, fakeCntlrIdBase + 1}
	if !slices.Equal(g.inspected, want) {
		t.Errorf("inspected cntlrs %v, want the primary's %v", g.inspected, want)
	}
}

// Each poll that does not see the ok row at the revision or later polls
// again: a pool with no primary, an inspect the gateway refuses (NOT_FOUND
// for a cntlr replaced, or any other code), an agent that answers no rows, a
// report at the revision without the subsystem's row, a row in error, a
// report older than the revision. The next poll's ok row ends the wait. An ok
// row at a revision past the detach's ends it at once: the worker sends the
// primary the latest revision alone (csi.md CSI10).
func TestTheDetachPollsPastWhatIsNotTheCut(t *testing.T) {
	primary, replacement := fakeCntlrIdBase+1, fakeCntlrIdBase+9
	cases := []struct {
		name string
		// first answers the first poll's inspect; nil leaves it to the
		// fake's ok report.
		first func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error)
		// noPrimary takes the pool's primary away for the first poll.
		noPrimary bool
		// inspects is the count of inspects the wait makes; inspected,
		// when set, the cntlr ids they ask, in order.
		inspects  int
		inspected []uint64
	}{
		{name: "ok at once", inspects: 1},
		{
			name: "ok at a later revision",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return report(p.rev+1, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
			},
			inspects: 1,
		},
		{name: "no primary", noPrimary: true, inspects: 1},
		{
			// The primary is replaced while the inspect is on its way,
			// as the worker's replacement does it
			// (model.ReplaceCntlrPassingOver; dnv-worker.md AR7): one
			// write takes the old id out of the pool's cntlr list, adds
			// the replacement at the end with the primary flag, and
			// raises the revision, and the gateway answers NOT_FOUND for
			// the id the pool no longer lists. The next poll finds the
			// replacement by its primary flag, not by its place in the
			// list: the standby now sits where the primary was.
			name: "cntlr replaced",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				p.cntlrIds = append(slices.DeleteFunc(slices.Clone(p.cntlrIds),
					func(id uint64) bool { return id == primary }), replacement)
				p.primary = len(p.cntlrIds) - 1
				p.rev++
				return nil, status.Errorf(codes.NotFound,
					"cntlr %d not found", primary)
			},
			inspects:  2,
			inspected: []uint64{primary, replacement},
		},
		{
			name: "agent unreachable",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return nil, status.Error(codes.Aborted,
					"InspectCntlr: controller node: unavailable")
			},
			inspects: 2,
		},
		{
			name: "no rows",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return &pb.InspectCntlrReply{}, nil
			},
			inspects: 2,
		},
		{
			name: "no row of the subsystem",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return report(p.rev, ssId+1, pb.ResStatus_RES_STATUS_OK, ""), nil
			},
			inspects: 2,
		},
		{
			name: "row in error",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return report(p.rev, ssId, pb.ResStatus_RES_STATUS_ERROR,
					"port link failed"), nil
			},
			inspects: 2,
		},
		{
			name: "pending row",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
					pendingDetails), nil
			},
			inspects: 2,
		},
		{
			name: "older report",
			first: func(p *fakePool, ssId uint64) (*pb.InspectCntlrReply, error) {
				return report(p.rev-1, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
			},
			inspects: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g := newTestController(t)
			id, ssId, _ := attachedVolume(t, c, g)
			c.detachWait = 2 * time.Second
			c.detachPoll = 5 * time.Millisecond
			g.mu.Lock()
			reads, inspects := 0, 0
			// The detach's own write reads the pool once before the
			// wait; the wait's first poll is the second read.
			g.poolRead = func(p *fakePool) {
				reads++
				switch {
				case tc.noPrimary && reads == 2:
					p.primary = -1
				case tc.noPrimary && reads == 3:
					p.primary = 0
				}
			}
			g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
				inspects++
				if inspects == 1 && tc.first != nil {
					return tc.first(p, ssId)
				}
				return okReport(p), nil
			}
			g.mu.Unlock()
			wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK,
				"detach A")
			g.mu.Lock()
			defer g.mu.Unlock()
			if inspects != tc.inspects {
				t.Errorf("%d inspects, want %d", inspects, tc.inspects)
			}
			if tc.noPrimary && reads != 3 {
				t.Errorf("%d pool reads, want 3: the write's, a poll "+
					"with no primary, a poll with one", reads)
			}
			if tc.inspected != nil && !slices.Equal(g.inspected, tc.inspected) {
				t.Errorf("inspected cntlrs %v, want %v", g.inspected,
					tc.inspected)
			}
		})
	}
}

// A wait that never sees the ok row fails at the driver's bound, or at the
// call's deadline when that comes first, with DEADLINE_EXCEEDED naming the
// volume, the node and the last state seen: the last report, and the
// gateway call that failed after it, so that polls the gateway stopped
// answering do not hide what the primary last said. A node id that is no
// host NQN and an id of another form wait for nothing.
func TestTheDetachFailsAtItsBound(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := attachedVolume(t, c, g)
	c.detachWait = 300 * time.Millisecond
	c.detachPoll = 20 * time.Millisecond
	g.mu.Lock()
	pending := func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
			pendingDetails), nil
	}
	answered := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		// From the third report on, the gateway stops answering the
		// pool read.
		if answered++; answered == 3 {
			g.fail["GetStoragePool"] = codes.Unavailable
		}
		return pending(p, req)
	}
	g.mu.Unlock()

	readsBefore := g.methodCalls("GetStoragePool")
	start := time.Now()
	err := unpublish(t.Context(), c, id, testNodeA)
	took := time.Since(start)
	wantCode(t, err, codes.DeadlineExceeded, "a cut that never holds")
	if took < c.detachWait {
		t.Errorf("the detach failed after %v, before its bound of %v",
			took, c.detachWait)
	}
	p := g.pool(fakeClusterOne, testVolume)
	g.mu.Lock()
	rev := p.rev
	g.mu.Unlock()
	for _, want := range []string{
		strconv.Quote(testVolume), testNodeA, "pending", pendingDetails,
		strconv.FormatUint(fakeCntlrIdBase+1, 10),
		"revision " + strconv.FormatUint(rev, 10),
		// The failed read after the last report: the injected refusal,
		// or the wait's own deadline when the bound ends that read.
		"; then rpc error: code = ", "desc = gateway GetStoragePool",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	// The polls pause between them: at most one read per period past the
	// write's own read and the first poll, which runs at once, and one
	// more of slack. Timers never fire early, so a slow machine makes
	// fewer reads, never more.
	reads := g.methodCalls("GetStoragePool") - readsBefore
	if most := int(c.detachWait/c.detachPoll) + 3; reads > most {
		t.Errorf("%d pool reads within the bound, want at most %d", reads,
			most)
	}
	if reads < 5 {
		t.Errorf("%d pool reads within the bound, want several", reads)
	}
	g.mu.Lock()
	delete(g.fail, "GetStoragePool")
	g.inspect = pending
	g.mu.Unlock()

	// The call's own deadline ends the wait first.
	c.detachWait = 10 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	err = unpublish(ctx, c, id, testNodeA)
	took = time.Since(start)
	wantCode(t, err, codes.DeadlineExceeded, "the call's deadline")
	if took > 5*time.Second {
		t.Errorf("the detach took %v past a deadline of 200ms", took)
	}

	// Neither of these names a host this driver attached.
	before := g.methodCalls("InspectCntlr")
	wantCode(t, unpublish(t.Context(), c, id, "some-fake-node"), codes.OK,
		"a node id that is no host NQN")
	wantCode(t, unpublish(t.Context(), c, "some-vol-id", testNodeA), codes.OK,
		"an id of another form")
	if n := g.methodCalls("InspectCntlr") - before; n != 0 {
		t.Errorf("%d polls for ids that name nothing of this driver, want 0", n)
	}
}

// A detach repeated after its node left the list writes nothing and waits
// again, for the pool's revision its first poll reads; so does a detach
// from every node.
func TestARepeatedDetachWaitsAgain(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := attachedVolume(t, c, g)
	wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK, "detach A")
	for _, node := range []string{testNodeA, ""} {
		g.mu.Lock()
		writes := 0
		for _, m := range g.calls {
			if m == "UpdateSubsystemHosts" {
				writes++
			}
		}
		steps := 0
		g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
			steps++
			if steps == 1 {
				return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
					pendingDetails), nil
			}
			return okReport(p), nil
		}
		g.mu.Unlock()
		wantCode(t, unpublish(t.Context(), c, id, node), codes.OK,
			"the repeated detach from "+strconv.Quote(node))
		if n := g.methodCalls("UpdateSubsystemHosts"); n != writes {
			t.Errorf("the repeated detach wrote %d times, want 0", n-writes)
		}
		g.mu.Lock()
		got := steps
		g.mu.Unlock()
		if got != 2 {
			t.Errorf("the repeated detach from %q polled %d times, want 2",
				node, got)
		}
	}
}

// A detach that writes nothing waits for the revision of its first poll,
// which reads the pool after the subsystem read: another change that lands
// after the detach's own pool read, as another detach would, is in the
// revision it waits for, so a report from before that change does not end
// it.
func TestADetachThatWritesNothingWaitsForTheFreshRevision(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := attachedVolume(t, c, g)
	g.mu.Lock()
	old := p.rev
	steps := 0
	// After the subsystem read, another change of the pool commits.
	g.afterList = func() {
		g.mu.Lock()
		p.rev++
		g.mu.Unlock()
	}
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		steps++
		if steps == 1 {
			return report(old, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
		}
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	writes := g.methodCalls("UpdateSubsystemHosts")
	wantCode(t, unpublish(t.Context(), c, id, testNodeB), codes.OK,
		"detach B, which is not on the list")
	if n := g.methodCalls("UpdateSubsystemHosts"); n != writes {
		t.Fatalf("the detach wrote %d times, want 0", n-writes)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if steps != 2 {
		t.Errorf("%d polls, want 2: the report from before the change does "+
			"not end the wait", steps)
	}
}

// A pool that is being deleted or is gone, and a subsystem that is gone,
// end the wait at once as done: what is gone takes no attach, and the
// gateway marks a pool deleting only once it holds no subsystem; the cn
// agents' sweep takes down a subsystem whose record is gone, its namespaces
// at the cntlr's next converge (cnagent.md CN21), and a host still connected
// can keep its IO until then (csi.md CSI10). The fake lets
// the test mark a pool deleting with the subsystem still there, which the
// rule covers too.
func TestTheDetachWaitEndsForAGoneOrDeletingPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(g *fakeGateway, p *fakePool)
	}{
		{"deleting", func(g *fakeGateway, p *fakePool) { p.deleting = true }},
		{"pool gone", func(g *fakeGateway, p *fakePool) {
			delete(g.clusters[fakeClusterOne].pools, testVolume)
		}},
		{"subsystem gone", func(g *fakeGateway, p *fakePool) {
			delete(p.ss, subsystemNqnOf(testVolume))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, g := newTestController(t)
			id, ssId, _ := attachedVolume(t, c, g)
			c.detachWait = 2 * time.Second
			c.detachPoll = 5 * time.Millisecond
			g.mu.Lock()
			g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
				tc.end(g, p)
				return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
					pendingDetails), nil
			}
			g.mu.Unlock()
			wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK,
				"detach A")
			if n := g.methodCalls("InspectCntlr"); n != 1 {
				t.Errorf("%d polls of the primary, want 1: the next poll "+
					"reads the pool's end", n)
			}
		})
	}
}

// A failover during the wait moves the primary: each poll reads the pool's
// primary afresh, and the new primary's ok row ends the wait. The failover
// raises the pool's revision in the write that moves the primary, as the
// worker's failover does (model.FailoverPassingOver raises it in the
// transaction that moves the primary; dnv-worker.md AR5), so the new primary
// reports a revision past the detach's, which counts.
func TestTheDetachFollowsAFailover(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := attachedVolume(t, c, g)
	c.detachWait = 2 * time.Second
	c.detachPoll = 5 * time.Millisecond
	oldPrimary, newPrimary := fakeCntlrIdBase+1, fakeCntlrIdBase+2
	g.mu.Lock()
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		if req.GetCntlrId() == oldPrimary {
			// The failover lands while the old primary still reports
			// the host connected.
			if p.primary == 0 {
				p.primary = 1
				p.rev++
			}
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		return okReport(p), nil
	}
	g.mu.Unlock()
	wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK, "detach A")
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := []uint64{oldPrimary, newPrimary}; !slices.Equal(g.inspected, want) {
		t.Errorf("inspected cntlrs %v, want %v", g.inspected, want)
	}
}

// The detach's revision is fixed once it is known (csi.md CSI10): the one
// its write made, or, when it writes nothing, the one its first poll reads.
// A pool whose revision moves on during the wait, as with another change of
// the pool, does not move it, so a report at that revision ends the wait
// while the pool's own revision is already past it.
func TestTheDetachsRevisionStaysFixedOnceKnown(t *testing.T) {
	for _, tc := range []struct {
		name string
		node string
		// bumpFrom is the first pool read that raises the revision; the
		// first read is the detach's own, before the wait.
		bumpFrom int
		// wrote says the detach writes.
		wrote bool
	}{
		// The write makes the revision; every poll then reads a newer one.
		{name: "written", node: testNodeA, bumpFrom: 2, wrote: true},
		// The first poll's read makes the revision; every later poll
		// reads a newer one.
		{name: "not written", node: testNodeB, bumpFrom: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, g := newTestController(t)
			id, ssId, p := attachedVolume(t, c, g)
			c.detachWait = 500 * time.Millisecond
			c.detachPoll = 5 * time.Millisecond
			g.mu.Lock()
			before := p.rev
			// want is the revision the wait must hold to.
			want := before + 1
			if !tc.wrote {
				want = before
			}
			reads, inspects := 0, 0
			g.poolRead = func(p *fakePool) {
				if reads++; reads >= tc.bumpFrom {
					p.rev++
				}
			}
			g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
				if inspects++; inspects == 1 {
					return report(want, ssId, pb.ResStatus_RES_STATUS_PENDING,
						pendingDetails), nil
				}
				return report(want, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
			}
			g.mu.Unlock()
			writes := g.methodCalls("UpdateSubsystemHosts")
			wantCode(t, unpublish(t.Context(), c, id, tc.node), codes.OK,
				"the detach of "+tc.node)
			wrote := g.methodCalls("UpdateSubsystemHosts") - writes
			g.mu.Lock()
			defer g.mu.Unlock()
			if (wrote == 1) != tc.wrote || wrote > 1 {
				t.Fatalf("%d writes, want the detach to write: %v", wrote,
					tc.wrote)
			}
			if inspects != 2 {
				t.Errorf("%d inspects, want 2: a pending row, then the ok "+
					"row at revision %d", inspects, want)
			}
			if p.rev <= want {
				t.Errorf("pool revision %d, want it past %d", p.rev, want)
			}
		})
	}
}

// A detach whose first write is refused as stale writes again under the
// revision its next attempt reads, and waits for the revision that write
// made. A report at the first token plus one is at the revision of the
// change that ran in between: it is made after that change and before the
// detach's own write, and at that revision the node is still on the list,
// so it does not end the wait.
func TestARetriedDetachWaitsForItsOwnWrite(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := attachedVolume(t, c, g)
	g.mu.Lock()
	first := p.rev
	// Another change of the pool commits between the first attempt's
	// subsystem read and its write.
	g.afterList = func() {
		g.mu.Lock()
		p.rev++
		g.mu.Unlock()
	}
	inspects := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		if inspects++; inspects == 1 {
			return report(first+1, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
		}
		return okReport(p), nil
	}
	g.mu.Unlock()
	writes := g.methodCalls("UpdateSubsystemHosts")
	wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK, "detach A")
	if n := g.methodCalls("UpdateSubsystemHosts") - writes; n != 2 {
		t.Fatalf("%d writes, want 2: a stale one, then the one that holds", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.rev != first+2 {
		t.Fatalf("pool revision %d, want %d", p.rev, first+2)
	}
	if inspects != 2 {
		t.Errorf("%d inspects, want 2: the report at revision %d comes "+
			"before the detach's write", inspects, first+1)
	}
}

// The controller the driver runs, as newController builds it with no test's
// override, holds a detach's wait to the driver's own bound and polls once a
// period of one second (csi.md CSI7, CSI10). Every other test of a detach
// sets both fields short, so this one reads them: the fields carry the
// constants, and one detach on such a controller, its primary pending at the
// first poll and ok at the next, returns OK with its two inspects a period
// apart.
func TestTheDriversOwnBoundAndPollPeriod(t *testing.T) {
	built := newController(nil, Config{})
	if built.detachWait != detachWaitMax || detachWaitMax <= 0 {
		t.Errorf("the wait's bound %v, want detachWaitMax %v, which is "+
			"above zero", built.detachWait, detachWaitMax)
	}
	if built.detachPoll != detachPollPeriod || detachPollPeriod != time.Second {
		t.Errorf("the poll period %v, want detachPollPeriod %v, one second",
			built.detachPoll, detachPollPeriod)
	}

	g := newFakeGateway()
	c := newController(g.serve(t), Config{
		CdcAddresses:   testCdc,
		GatewayTimeout: 2 * time.Second,
	})
	id, ssId, _ := attachedVolume(t, c, g)
	var at []time.Time
	g.mu.Lock()
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		if at = append(at, time.Now()); len(at) == 1 {
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		return okReport(p), nil
	}
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	wantCode(t, unpublish(ctx, c, id, testNodeA), codes.OK, "detach A")
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(at) != 2 {
		t.Fatalf("%d inspects, want 2", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < detachPollPeriod {
		t.Errorf("the two inspects %v apart, want at least %v", gap,
			detachPollPeriod)
	}
}
