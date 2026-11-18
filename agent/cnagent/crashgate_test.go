package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// One agent process over the fake node, and its end (the crash walks)
// ---------------------------------------------------------------------------

// crashGate stands between one agent process and the fake node. Every call
// the cn agent makes to the node goes through the one common.OsClient
// NewCnAgentServer hands to the store, dm, nvmet, the nvme host and md, or
// through the leg probers' LegProbeIO (CN11), and the gate is both, so it
// sees them all. Once it has ended it refuses every call, as a dead process
// makes none.
//
// Armed with a trace id, it counts the calls of one piece of work under that
// id: the work's own calls, which carry crashOwnKey (crashOwnCtx), or, with
// background set, its background calls, which carry the id and not the
// mark — the sweep's `nvme disconnect`s, which run off the pass under the
// pass's trace id from the root context (startDisconnect, CN10). It ends at
// the cutAt-th counted call: before it, the call is refused; after it, the
// call runs and every later one is refused. The work's background calls are
// held until release, or refused once the gate has ended, so the work's own
// calls run in one order whatever the scheduler does; an ordered release
// lets them through one at a time in the order of their lines, once all the
// expected ones have arrived, so they too run in one order. A held
// disconnect keeps the slot it took (disconnectSlots), so a shape may set
// going no more of them than there are slots.
type crashGate struct {
	osc   common.OsClient
	probe LegProbeIO

	mu       sync.Mutex
	cond     *sync.Cond
	ended    bool
	cut      chan struct{} // closed when the gate ends
	inflight int

	traceId    string
	background bool
	cutAt      int
	after      bool
	n          int
	lines      []string // the counted calls, in order
	// released lets the held background calls through, one at a time in
	// the order of their lines when ordered, once expect of them have
	// arrived; waiting are the lines of those held now, busy says one runs,
	// and done counts those that ran. heldLines are those let through, in
	// the order they ran.
	released  bool
	ordered   bool
	expect    int
	waiting   []string
	busy      bool
	done      int
	heldLines []string
	// pauseReached and pauseResume hold the cut call at the cut, before the
	// gate ends (pauseAtCut).
	pauseReached chan struct{}
	pauseResume  chan struct{}
	// byId is the calls let through, per trace id.
	byId map[string][]string
	// stopped is set once the process's end has cancelled its root context
	// and its calls in flight have returned; late counts the calls refused
	// after that on a context that was not cancelled: work that does not
	// hang off the process's root context, and so outlives its end.
	stopped bool
	late    int
}

func newCrashGate(osc common.OsClient, probe LegProbeIO) *crashGate {
	g := &crashGate{
		osc:   osc,
		probe: probe,
		cut:   make(chan struct{}),
		byId:  make(map[string][]string),
	}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// crashOwnKey marks the context of the work a gate counts (crashOwnCtx).
type crashOwnKey struct{}

// crashOwnCtx is parent under traceId with the gate's mark: the context the
// walk gives a converge, or a startup pass, whose calls it counts.
func crashOwnCtx(parent context.Context, traceId string) context.Context {
	return common.WithTraceId(context.WithValue(parent, crashOwnKey{}, true),
		traceId)
}

// arm starts counting the calls of the work under traceId, from zero: its
// own calls, or its background calls when background is set. cutAt 0 counts
// and never cuts.
func (g *crashGate) arm(traceId string, background bool, cutAt int,
	after bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.traceId, g.background, g.cutAt, g.after = traceId, background, cutAt,
		after
	g.n, g.lines = 0, nil
	g.released, g.ordered, g.expect = false, false, 0
	g.waiting, g.busy, g.done, g.heldLines = nil, false, 0, nil
}

// pauseAtCut makes the cut call wait at the cut, before the gate ends and
// before the call runs or is refused, until resume is closed; reached is
// closed when it waits. Meanwhile the process lives on, so a test can see
// what it answers at that point.
func (g *crashGate) pauseAtCut() (reached <-chan struct{},
	resume chan<- struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pauseReached = make(chan struct{})
	g.pauseResume = make(chan struct{})
	return g.pauseReached, g.pauseResume
}

// release lets the held background calls through: at once, or, when
// ordered, one at a time in the order of their lines once expect of them
// have arrived.
func (g *crashGate) release(ordered bool, expect int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.released, g.ordered, g.expect = true, ordered, expect
	g.cond.Broadcast()
}

// counted is the calls counted so far.
func (g *crashGate) counted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.lines)
}

// backgroundLines is the held background calls let through, sorted.
func (g *crashGate) backgroundLines() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := slices.Clone(g.heldLines)
	slices.Sort(out)
	return out
}

// linesUnder is the calls let through under traceId, in order.
func (g *crashGate) linesUnder(traceId string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.byId[traceId])
}

// end refuses every later call.
func (g *crashGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.endLocked()
}

func (g *crashGate) endLocked() {
	if !g.ended {
		g.ended = true
		close(g.cut)
		g.cond.Broadcast()
	}
}

// waitInflight waits until no call the gate let through runs.
func (g *crashGate) waitInflight() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.inflight > 0 {
		g.cond.Wait()
	}
}

// stop marks the process's end done (crashInc.end).
func (g *crashGate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
}

// lateCalls is how many calls were refused after stop on a context that
// was not cancelled.
func (g *crashGate) lateCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.late
}

// refuseLocked counts one refused call.
func (g *crashGate) refuseLocked(ctx context.Context) {
	if g.stopped && ctx.Err() == nil {
		g.late++
	}
}

// turnLocked reports whether a held background call whose line is line may
// go now.
func (g *crashGate) turnLocked(line string) bool {
	if !g.released {
		return false
	}
	if !g.ordered {
		return true
	}
	return !g.busy && len(g.waiting)+g.done >= g.expect &&
		line == slices.Min(g.waiting)
}

// enter decides one call: admitted lets it through, and the caller then runs
// it and calls leave with held.
func (g *crashGate) enter(ctx context.Context, line string) (
	admitted bool, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		g.refuseLocked(ctx)
		return false, false
	}
	traceId, _ := common.TraceIdFromCtx(ctx)
	armed := g.traceId != "" && traceId == g.traceId
	own := ctx.Value(crashOwnKey{}) != nil
	held = armed && !own
	if held {
		g.waiting = append(g.waiting, line)
		// An arrival can be the one the call whose turn it is waits for.
		g.cond.Broadcast()
		for !g.ended && !g.turnLocked(line) {
			g.cond.Wait()
		}
		g.waiting = slices.Delete(g.waiting,
			slices.Index(g.waiting, line), slices.Index(g.waiting, line)+1)
		if g.ended {
			g.refuseLocked(ctx)
			g.cond.Broadcast()
			return false, false
		}
	}
	if armed && own != g.background {
		g.n++
		g.lines = append(g.lines, line)
		if g.n == g.cutAt && g.pauseReached != nil {
			reached, resume := g.pauseReached, g.pauseResume
			g.pauseReached = nil
			close(reached)
			g.mu.Unlock()
			<-resume
			g.mu.Lock()
		}
		if g.n == g.cutAt {
			g.endLocked()
			if !g.after {
				g.refuseLocked(ctx)
				return false, false
			}
		}
	}
	if held {
		g.busy = g.ordered
		g.heldLines = append(g.heldLines, line)
	}
	g.byId[traceId] = append(g.byId[traceId], line)
	g.inflight++
	return true, held
}

// leave marks a call enter let through done.
func (g *crashGate) leave(held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight--
	if held {
		g.busy = false
		g.done++
	}
	g.cond.Broadcast()
}

// errCrashEnded is what a refused call answers: the process that made it is
// gone. RunCommand pairs it with exit code -1, the "did not answer" of
// agent.Reported, and nothing reads it as a file that is absent.
var errCrashEnded = context.Canceled

func (g *crashGate) RunCommand(
	ctx context.Context, name string, args []string, stdin string,
) (string, string, int, error) {
	ok, held := g.enter(ctx, "cmd "+name+" "+strings.Join(args, " "))
	if !ok {
		return "", "", -1, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.RunCommand(ctx, name, args, stdin)
}

func (g *crashGate) ReadFile(ctx context.Context, path string) (string, error) {
	ok, held := g.enter(ctx, "read "+path)
	if !ok {
		return "", errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadFile(ctx, path)
}

func (g *crashGate) ReadDir(ctx context.Context, path string) ([]string, error) {
	ok, held := g.enter(ctx, "list "+path)
	if !ok {
		return nil, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadDir(ctx, path)
}

func (g *crashGate) ReadMtime(
	ctx context.Context, path string,
) (time.Time, error) {
	ok, held := g.enter(ctx, "mtime "+path)
	if !ok {
		return time.Time{}, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadMtime(ctx, path)
}

func (g *crashGate) ReadDevNo(ctx context.Context, path string) (string, error) {
	ok, held := g.enter(ctx, "devno "+path)
	if !ok {
		return "", errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadDevNo(ctx, path)
}

func (g *crashGate) WriteFile(ctx context.Context, path, data string) error {
	ok, held := g.enter(ctx, "write "+path+"="+data)
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.osc.WriteFile(ctx, path, data)
}

func (g *crashGate) WriteFileDirect(
	ctx context.Context, path, data string,
) error {
	ok, held := g.enter(ctx, "writedirect "+path+"="+data)
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.osc.WriteFileDirect(ctx, path, data)
}

func (g *crashGate) ReadBlock(
	ctx context.Context, path string, off, n uint64,
) ([]byte, error) {
	ok, held := g.enter(ctx, fmt.Sprintf("readblock %s off=%d len=%d", path,
		off, n))
	if !ok {
		return nil, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadBlock(ctx, path, off, n)
}

// ReadBlockDirect carries md's member superblock reads, which the fake
// records as "readsb".
func (g *crashGate) ReadBlockDirect(
	ctx context.Context, path string, off, n uint64,
) ([]byte, error) {
	ok, held := g.enter(ctx, fmt.Sprintf("readsb %s off=%d len=%d", path,
		off, n))
	if !ok {
		return nil, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadBlockDirect(ctx, path, off, n)
}

func (g *crashGate) WriteBlock(
	ctx context.Context, path string, off uint64, data []byte,
) error {
	ok, held := g.enter(ctx, fmt.Sprintf("writeblock %s off=%d len=%d", path,
		off, len(data)))
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.osc.WriteBlock(ctx, path, off, data)
}

func (g *crashGate) DiscardRanges(
	ctx context.Context, path string, ranges []common.DiscardRange,
) (int, error) {
	ok, held := g.enter(ctx, fmt.Sprintf("discard %s ranges=%v", path,
		ranges))
	if !ok {
		return 0, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.DiscardRanges(ctx, path, ranges)
}

func (g *crashGate) DialTcp(
	ctx context.Context, address string, timeout time.Duration,
) error {
	ok, held := g.enter(ctx, "dial "+address)
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.osc.DialTcp(ctx, address, timeout)
}

func (g *crashGate) StartDetached(
	ctx context.Context, name string, args []string, dir string,
) (*common.Detached, error) {
	ok, held := g.enter(ctx, "detach "+name+" "+strings.Join(args, " "))
	if !ok {
		return nil, errCrashEnded
	}
	defer g.leave(held)
	return g.osc.StartDetached(ctx, name, args, dir)
}

func (g *crashGate) ReadProto(
	ctx context.Context, path string, m proto.Message,
) error {
	ok, held := g.enter(ctx, "readproto "+path)
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.osc.ReadProto(ctx, path, m)
}

func (g *crashGate) WriteProto(
	ctx context.Context, path string, m proto.Message,
) error {
	ok, held := g.enter(ctx, "writeproto "+path)
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.osc.WriteProto(ctx, path, m)
}

// Write and ReadDirect are the leg probers' LegProbeIO.
func (g *crashGate) Write(
	ctx context.Context, path string, off uint64, data []byte,
) error {
	ok, held := g.enter(ctx, fmt.Sprintf("probewrite %s off=%d len=%d", path,
		off, len(data)))
	if !ok {
		return errCrashEnded
	}
	defer g.leave(held)
	return g.probe.Write(ctx, path, off, data)
}

func (g *crashGate) ReadDirect(
	ctx context.Context, path string, off, n uint64,
) ([]byte, error) {
	ok, held := g.enter(ctx, fmt.Sprintf("probereaddirect %s off=%d len=%d",
		path, off, n))
	if !ok {
		return nil, errCrashEnded
	}
	defer g.leave(held)
	return g.probe.ReadDirect(ctx, path, off, n)
}

var (
	_ common.OsClient = (*crashGate)(nil)
	_ LegProbeIO      = (*crashGate)(nil)
)

// crashProbeInterval is the probers' and the path watchers' interval in the
// walks: short, so a restarted agent's legs leave "health probe pending"
// (CN11) within a few milliseconds.
const crashProbeInterval = 2 * time.Millisecond

// crashInc is one agent process over a node.
type crashInc struct {
	srv    *CnAgentServer
	gate   *crashGate
	root   context.Context
	cancel context.CancelFunc
	ended  bool
}

// startCrashInc starts an agent process the way agent.Serve does, up to its
// startup pass: Prepare, after which the agent answers "starting"
// (dnagent.md SH33). The settings are newCnServer's, with the probers and
// the rounds' reads running as production runs them (probing). Nothing
// registers a cleanup that holds the server: a walk starts thousands, and
// each is ended by end.
func startCrashInc(node *fakeNode) *crashInc {
	gate := newCrashGate(node.osClient(), node.probeIO())
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewCnAgentServer(gate, nf, common.DefaultLocalStorPrefix,
		testCapacity, testTrConf(), common.NvmetPortId, 0)
	srv.probeIO = gate
	srv.sleep = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	probing(srv, crashProbeInterval)
	root, cancel := context.WithCancel(context.Background())
	srv.Prepare(root)
	return &crashInc{srv: srv, gate: gate, root: root, cancel: cancel}
}

// reconcile runs the startup pass under traceId, on the root context as
// agent.Serve runs it, so that ending the process reaches a pass in flight,
// and with the gate's mark, so that a gate armed with traceId counts it.
func (inc *crashInc) reconcile(traceId string) error {
	return inc.srv.Reconcile(crashOwnCtx(inc.root, traceId))
}

// end plays the process's death: the gate refuses every later call, the
// root context ends — the probers, the path watchers, the background
// disconnects and the rounds' reads all hang off it (CN10, CN11, dnagent.md
// SH29) — the calls in flight return, and the reads are joined. The cn joins
// none of its other background work (CN11): from here a call the gate
// refuses on a context that is not cancelled is late (crashGate.late). A
// second end does nothing.
func (inc *crashInc) end() {
	if inc.ended {
		return
	}
	inc.ended = true
	inc.gate.end()
	inc.cancel()
	inc.gate.waitInflight()
	inc.srv.reads.Wait()
	inc.gate.stop()
}

// crashAwaitDisconnects waits until CN10's disconnect registry of srv is
// empty, as a worker's next round comes well after a pass's disconnects have
// returned, and reports whether that happened within a bound.
func crashAwaitDisconnects(srv *CnAgentServer) bool {
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		left := len(srv.disconnecting)
		srv.mu.Unlock()
		if left == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}
