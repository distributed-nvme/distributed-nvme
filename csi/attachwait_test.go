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

// The tests of an attach's wait for the primary's cut (csi.md CSI10): an
// attach that puts its node on the empty list first waits for a report of
// the pool's primary cntlr made at exactly the revision its attempt read
// whose row of the volume's subsystem reads OK, so that no host taken off
// the list still holds an nvmet connection when the new one is let in; a
// poll or a report past that revision ends the attempt, which reads and
// waits again. Kubernetes'
// forced detach attaches the next node once a detach call has failed,
// while the detach's own wait still runs.

func publish(
	ctx context.Context, c *controller, id string, node string,
) error {
	_, err := c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: node,
		VolumeCapability: mountCap(fsExt4, rwo),
	})
	return err
}

// detachedVolume is attachedVolume detached from node A again, with the
// fake's record of inspects cleared.
func detachedVolume(
	t *testing.T, c *controller, g *fakeGateway,
) (string, uint64, *fakePool) {
	t.Helper()
	id, ssId, p := attachedVolume(t, c, g)
	wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK, "detach A")
	g.mu.Lock()
	g.inspected = nil
	g.mu.Unlock()
	return id, ssId, p
}

// The attach after a detach whose cut has not held waits before it writes:
// a report older than the revision it read, then a pending row at it, are
// polled past while the list stays empty, and the ok row lets the write
// through.
func TestAnAttachWaitsForTheCutBeforeItWrites(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := detachedVolume(t, c, g)
	nqn := subsystemNqnOf(testVolume)
	g.mu.Lock()
	read := p.rev
	steps := 0
	var listed []int
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		steps++
		listed = append(listed, len(p.ss[nqn].GetAllowedHosts()))
		switch steps {
		case 1:
			return report(read-1, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
		case 2:
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	wantCode(t, publish(t.Context(), c, id, testNodeB), codes.OK, "attach B")
	g.mu.Lock()
	defer g.mu.Unlock()
	if steps != 3 {
		t.Fatalf("%d polls of the primary before the attach's write, want 3: "+
			"an older report, a pending row, the ok row", steps)
	}
	if !slices.Equal(listed, []int{0, 0, 0}) {
		t.Errorf("hosts on the list at each poll %v, want none before the "+
			"ok row", listed)
	}
	if got := p.ss[nqn].GetAllowedHosts(); !slices.Equal(got, []string{testNodeB}) {
		t.Errorf("allowed hosts %v after the attach, want [%s]", got, testNodeB)
	}
	if p.rev != read+1 {
		t.Errorf("pool revision %d after the attach, want %d", p.rev, read+1)
	}
}

// An attach that writes nothing, its node already alone on the list, does
// not wait: it lets no new host in.
func TestAnAttachThatWritesNothingDoesNotWait(t *testing.T) {
	c, g := newTestController(t)
	id, _, _ := attachedVolume(t, c, g)
	before := g.methodCalls("InspectCntlr")
	wantCode(t, publish(t.Context(), c, id, testNodeA), codes.OK,
		"attach A again")
	if n := g.methodCalls("InspectCntlr") - before; n != 0 {
		t.Errorf("%d polls of the primary for an attach that writes "+
			"nothing, want 0", n)
	}
}

// An attach whose cut never holds fails at its bound, or at the call's
// deadline, and writes nothing: the list stays empty, and the error names
// the volume, the node and the pending row.
func TestAnAttachFailsAtItsBoundAndWritesNothing(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := detachedVolume(t, c, g)
	nqn := subsystemNqnOf(testVolume)
	c.detachWait = 200 * time.Millisecond
	c.detachPoll = 20 * time.Millisecond
	g.mu.Lock()
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
			pendingDetails), nil
	}
	read := p.rev
	g.mu.Unlock()
	start := time.Now()
	err := publish(t.Context(), c, id, testNodeB)
	took := time.Since(start)
	wantCode(t, err, codes.DeadlineExceeded, "a cut that never holds")
	if took < c.detachWait {
		t.Errorf("the attach failed after %v, before its bound of %v", took,
			c.detachWait)
	}
	for _, want := range []string{
		strconv.Quote(testVolume), "attach to node " + testNodeB,
		"pending", pendingDetails,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	// The call's own deadline ends the wait first.
	c.detachWait = 10 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	err = publish(ctx, c, id, testNodeB)
	took = time.Since(start)
	wantCode(t, err, codes.DeadlineExceeded, "the call's deadline")
	if took > 5*time.Second {
		t.Errorf("the attach took %v past a deadline of 200ms", took)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if got := p.ss[nqn].GetAllowedHosts(); len(got) != 0 {
		t.Errorf("allowed hosts %v after attaches that ran out of time, "+
			"want none", got)
	}
	if p.rev != read {
		t.Errorf("pool revision %d after attaches that wrote nothing, want %d",
			p.rev, read)
	}
}

// tokenCheck records, under the fake's lock, the revision of the last OK
// report the fake made and, at every host write, the write's token and
// that revision, so a test can see that each write came after an OK
// report at its own token's revision.
type tokenCheck struct {
	okAt   uint64
	writes [][2]uint64
}

func (k *tokenCheck) hook(g *fakeGateway) {
	g.hostWrite = func(p *fakePool, req *pb.UpdateSubsystemHostsRequest) {
		k.writes = append(k.writes, [2]uint64{req.GetSpRev().GetRevision(), k.okAt})
	}
}

func (k *tokenCheck) verify(t *testing.T) {
	t.Helper()
	if len(k.writes) == 0 {
		t.Fatal("no host write")
	}
	for i, w := range k.writes {
		if w[0] != w[1] {
			t.Errorf("write %d carries token %d after the last OK report at "+
				"revision %d, want an OK report at its own token's revision",
				i+1, w[0], w[1])
		}
	}
}

// The worker writes the pool after a report of the primary, several times
// while it builds a pool: here after each of the first five reports, each
// OK at the revision it was made at. The attach rides out every move: each
// stale write reads and waits again, past hostsAttempts, and the attach
// ends OK with every write after an OK report at its own token's revision.
func TestAnAttachRidesOutTheWorkersRevisionMoves(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := detachedVolume(t, c, g)
	var k tokenCheck
	g.mu.Lock()
	k.hook(g)
	bumps := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		rep := report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, "")
		k.okAt = p.rev
		if bumps < hostsAttempts+1 {
			bumps++
			p.rev++
		}
		return rep, nil
	}
	g.mu.Unlock()
	wantCode(t, publish(t.Context(), c, id, testNodeB), codes.OK, "attach B")
	g.mu.Lock()
	defer g.mu.Unlock()
	if bumps != hostsAttempts+1 {
		t.Errorf("%d moves of the pool's revision, want %d", bumps,
			hostsAttempts+1)
	}
	if got := p.ss[subsystemNqnOf(testVolume)].GetAllowedHosts(); !slices.Equal(got, []string{testNodeB}) {
		t.Errorf("allowed hosts %v, want [%s]", got, testNodeB)
	}
	k.verify(t)
}

// A first write made stale by another node's attach and detach, whose cut
// has not held at the new revision: the second attempt waits again, past
// the pending row there, before it writes.
func TestAStaleAttachWaitsAgain(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := detachedVolume(t, c, g)
	var k tokenCheck
	g.mu.Lock()
	k.hook(g)
	first := p.rev
	polls := map[uint64]int{}
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		polls[p.rev]++
		switch {
		case p.rev == first:
			// OK at the first read; then node C is attached and detached.
			k.okAt = p.rev
			rep := report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, "")
			p.rev += 2
			return rep, nil
		case polls[p.rev] == 1:
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		k.okAt = p.rev
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	wantCode(t, publish(t.Context(), c, id, testNodeB), codes.OK, "attach B")
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(k.writes) != 2 {
		t.Fatalf("%d host writes, want 2: one stale, one after the second "+
			"wait", len(k.writes))
	}
	if n := polls[first+2]; n != 2 {
		t.Errorf("%d polls at the second attempt's revision, want 2: the "+
			"pending row, then the OK one", n)
	}
	k.verify(t)
}

// A caller that ends its call answers CANCELED, a detach's and an attach's
// alike, as the attacher sidecar does at its own deadline (csi.md CSI10).
func TestACancelledCallAnswersCanceled(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := attachedVolume(t, c, g)
	g.mu.Lock()
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
			pendingDetails), nil
	}
	g.mu.Unlock()
	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(100*time.Millisecond, cancel)
		return ctx
	}
	wantCode(t, unpublish(cancelled(), c, id, testNodeA), codes.Canceled,
		"a detach its caller cancels")
	wantCode(t, publish(cancelled(), c, id, testNodeB), codes.Canceled,
		"an attach its caller cancels")
}

// A call's attempts share one bound: an attach whose every attempt waits
// most of the bound before its write turns stale ends at the bound, not
// after a bound for each attempt.
func TestACallsAttemptsShareOneBound(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := detachedVolume(t, c, g)
	c.detachWait = 300 * time.Millisecond
	c.detachPoll = 20 * time.Millisecond
	g.mu.Lock()
	polls := map[uint64]int{}
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		// Ten polls, about 200 ms, pending at each revision, then OK, and
		// the worker moves the pool right after.
		if polls[p.rev]++; polls[p.rev] <= 10 {
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		rep := report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, "")
		p.rev++
		return rep, nil
	}
	g.mu.Unlock()
	start := time.Now()
	err := publish(t.Context(), c, id, testNodeB)
	took := time.Since(start)
	wantCode(t, err, codes.DeadlineExceeded, "attempts past the bound")
	if took > 2*c.detachWait {
		t.Errorf("the attach took %v, past its one bound of %v", took,
			c.detachWait)
	}
}

// The pool moves while the attach's wait reads pending, more times than
// hostsAttempts, and then the row reads OK: each move ends the attempt,
// which counts against no limit, and the attach ends OK with one write,
// after an OK report at its own token's revision (csi.md CSI10).
func TestAnAttachRidesOutMovesWhileItWaits(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, p := detachedVolume(t, c, g)
	var k tokenCheck
	g.mu.Lock()
	k.hook(g)
	moves := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		if moves < hostsAttempts+1 {
			rep := report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails)
			moves++
			p.rev++
			return rep, nil
		}
		k.okAt = p.rev
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	before := g.methodCalls("UpdateSubsystemHosts")
	wantCode(t, publish(t.Context(), c, id, testNodeB), codes.OK, "attach B")
	if n := g.methodCalls("UpdateSubsystemHosts") - before; n != 1 {
		t.Errorf("%d host writes, want 1: no write before the OK report", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if moves <= hostsAttempts {
		t.Errorf("%d moves of the pool, want more than %d", moves, hostsAttempts)
	}
	if got := p.ss[subsystemNqnOf(testVolume)].GetAllowedHosts(); !slices.Equal(got, []string{testNodeB}) {
		t.Errorf("allowed hosts %v, want [%s]", got, testNodeB)
	}
	k.verify(t)
}

// A report past the revision the attempt read, while the pool read in the
// same poll still shows that revision, ends the attempt at once: the attach
// reads the subsystem again before its next poll, and writes under no token
// without an OK report at it.
func TestAReportPastTheAttemptsRevisionEndsTheAttempt(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := detachedVolume(t, c, g)
	var k tokenCheck
	g.mu.Lock()
	k.hook(g)
	first := true
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		rev := p.rev
		if first {
			// A report one revision ahead of the pool's.
			first = false
			rev++
		}
		k.okAt = rev
		return report(rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	lists := g.methodCalls("ListSubsystems")
	writes := g.methodCalls("UpdateSubsystemHosts")
	wantCode(t, publish(t.Context(), c, id, testNodeB), codes.OK, "attach B")
	if n := g.methodCalls("ListSubsystems") - lists; n != 2 {
		t.Errorf("%d subsystem reads, want 2: the report past the revision "+
			"ends the first attempt", n)
	}
	if n := g.methodCalls("UpdateSubsystemHosts") - writes; n != 1 {
		t.Errorf("%d host writes, want 1", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k.verify(t)
}

// A primary one step behind: the pool moves past the attempt's revision
// while the wait reads pending, and the next report is OK at the old
// revision. The poll that reads the pool past it ends the attempt, so no
// write goes out under the old token: one write, at the new revision.
func TestAPoolPastTheAttemptsRevisionEndsTheAttempt(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := detachedVolume(t, c, g)
	var k tokenCheck
	g.mu.Lock()
	k.hook(g)
	var old uint64
	step := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		step++
		switch step {
		case 1:
			old = p.rev
			p.rev++
			return report(old, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		case 2:
			k.okAt = old
			return report(old, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
		}
		k.okAt = p.rev
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	writes := g.methodCalls("UpdateSubsystemHosts")
	wantCode(t, publish(t.Context(), c, id, testNodeB), codes.OK, "attach B")
	if n := g.methodCalls("UpdateSubsystemHosts") - writes; n != 1 {
		t.Errorf("%d host writes, want 1: none under the old token", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k.verify(t)
}

// An attach's exact wait that finds the pool gone, the pool deleting or
// its subsystem gone writes nothing: the next attempt's own reads answer
// NOT_FOUND.
func TestAnAttachWaitEndsWithNoWriteForAGoneOrDeletingPool(t *testing.T) {
	nqn := subsystemNqnOf(testVolume)
	for _, tc := range []struct {
		name string
		end  func(g *fakeGateway, p *fakePool)
	}{
		{"deleting", func(g *fakeGateway, p *fakePool) {
			// A deleting pool holds no subsystem (csi.md CSI9).
			p.deleting = true
			delete(p.ss, nqn)
		}},
		{"pool gone", func(g *fakeGateway, p *fakePool) {
			delete(g.clusters[fakeClusterOne].pools, testVolume)
		}},
		{"subsystem gone", func(g *fakeGateway, p *fakePool) {
			delete(p.ss, nqn)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, g := newTestController(t)
			id, ssId, _ := detachedVolume(t, c, g)
			g.mu.Lock()
			g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
				tc.end(g, p)
				return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
					pendingDetails), nil
			}
			g.mu.Unlock()
			before := g.methodCalls("UpdateSubsystemHosts")
			wantCode(t, publish(t.Context(), c, id, testNodeB),
				codes.NotFound, "attach B")
			if n := g.methodCalls("UpdateSubsystemHosts") - before; n != 0 {
				t.Errorf("%d host writes, want none", n)
			}
		})
	}
}

// The polls of one call keep the poll period apart across its attempts: a
// primary whose report is always past the pool's revision ends every
// attempt at its first poll, and the attach still makes at most one poll a
// period until its bound, with no write.
func TestAnAttachsPollsKeepThePeriodAcrossAttempts(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := detachedVolume(t, c, g)
	c.detachWait = 300 * time.Millisecond
	c.detachPoll = 20 * time.Millisecond
	g.mu.Lock()
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		return report(p.rev+1, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.mu.Unlock()
	beforeInspects := g.methodCalls("InspectCntlr")
	beforeWrites := g.methodCalls("UpdateSubsystemHosts")
	err := publish(t.Context(), c, id, testNodeB)
	wantCode(t, err, codes.DeadlineExceeded, "a primary always ahead")
	for _, want := range []string{
		strconv.Quote(testVolume), "attach to node " + testNodeB, "past the attempt's",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	n := g.methodCalls("InspectCntlr") - beforeInspects
	if most := int(c.detachWait/c.detachPoll) + 2; n > most {
		t.Errorf("%d polls within the bound, want at most %d", n, most)
	}
	if n < 3 {
		t.Errorf("%d polls within the bound, want several", n)
	}
	if w := g.methodCalls("UpdateSubsystemHosts") - beforeWrites; w != 0 {
		t.Errorf("%d host writes, want none", w)
	}
}

// pastDeadline is a context whose deadline has passed while its Err still
// reads nil, as for a moment after a gateway call fails at the deadline.
type pastDeadline struct{ context.Context }

func (pastDeadline) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Millisecond), true
}

func (pastDeadline) Err() error { return nil }

// callErr turns an error that ended the call into the bound's own once the
// deadline has passed, even before the context says so, naming the
// volume, the call and the last state; before the deadline it passes the
// error on as it is.
func TestCallErrReadsTheDeadline(t *testing.T) {
	c, _ := newTestController(t)
	gwErr := status.Error(codes.DeadlineExceeded,
		"gateway ListSubsystems: context deadline exceeded")
	st := &cutState{answer: "primary cntlr 101 at revision 3 reads the " +
		"subsystem pending: " + pendingDetails}
	what := "the attach to node " + testNodeB
	err := c.callErr(pastDeadline{t.Context()}, testVolume, what, st, gwErr)
	wantCode(t, err, codes.DeadlineExceeded, "a passed deadline")
	for _, want := range []string{
		strconv.Quote(testVolume), what, st.answer, "gateway ListSubsystems",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if got := c.callErr(ctx, testVolume, what, &cutState{}, gwErr); got != gwErr {
		t.Errorf("an error before the deadline came back as %v, want it "+
			"as it is", got)
	}
}

// A call's first poll runs at once: a detach whose primary reports the
// cut at once returns well inside one poll period.
func TestACallsFirstPollRunsAtOnce(t *testing.T) {
	c, g := newTestController(t)
	id, _, _ := attachedVolume(t, c, g)
	c.detachPoll = time.Second
	start := time.Now()
	wantCode(t, unpublish(t.Context(), c, id, testNodeA), codes.OK, "detach A")
	if took := time.Since(start); took > c.detachPoll/2 {
		t.Errorf("the detach took %v, want its first poll at once, well "+
			"inside a period of %v", took, c.detachPoll)
	}
}

// The bound ends a call inside a gateway call of an attempt as it ends a
// wait: DEADLINE_EXCEEDED naming the volume, the call and the gateway call
// that the bound cut short, for an attach and a detach.
func TestTheBoundInAGatewayCallNamesTheCall(t *testing.T) {
	c, g := newTestController(t)
	id, _, _ := attachedVolume(t, c, g)
	c.detachWait = 100 * time.Millisecond
	for _, tc := range []struct {
		what string
		call func() error
	}{
		{"detach from node " + testNodeA, func() error {
			return unpublish(t.Context(), c, id, testNodeA)
		}},
		{"attach to node " + testNodeB, func() error {
			return publish(t.Context(), c, id, testNodeB)
		}},
	} {
		g.mu.Lock()
		g.afterList = func() { time.Sleep(300 * time.Millisecond) }
		g.mu.Unlock()
		err := tc.call()
		wantCode(t, err, codes.DeadlineExceeded, tc.what)
		for _, want := range []string{
			strconv.Quote(testVolume), tc.what, "gateway ListSubsystems",
		} {
			if err != nil && !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the error does not name %q: %v", tc.what, want, err)
			}
		}
	}
}

// The bound's error names the last poll, an OK one included: an attach
// whose wait saw a pending row and then the OK one, and whose write the
// bound then cuts short, names no pending row.
func TestTheBoundsErrorNamesTheLastPoll(t *testing.T) {
	c, g := newTestController(t)
	id, ssId, _ := detachedVolume(t, c, g)
	c.detachWait = 100 * time.Millisecond
	g.mu.Lock()
	polls := 0
	g.inspect = func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error) {
		if polls++; polls == 1 {
			return report(p.rev, ssId, pb.ResStatus_RES_STATUS_PENDING,
				pendingDetails), nil
		}
		return report(p.rev, ssId, pb.ResStatus_RES_STATUS_OK, ""), nil
	}
	g.hostWrite = func(p *fakePool, req *pb.UpdateSubsystemHostsRequest) {
		time.Sleep(300 * time.Millisecond)
	}
	g.mu.Unlock()
	err := publish(t.Context(), c, id, testNodeB)
	wantCode(t, err, codes.DeadlineExceeded, "a write the bound cuts short")
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "pending") {
		t.Errorf("the error names the pending row the OK one followed: %v", err)
	}
	if !strings.Contains(err.Error(), "reads the subsystem ok") {
		t.Errorf("the error does not name the OK poll: %v", err)
	}
}
