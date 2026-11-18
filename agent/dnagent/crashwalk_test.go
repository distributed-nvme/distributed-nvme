package dnagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A crash at every step of a converge.
//
// Each walk drives one converge against the fake node, ends the agent at
// every call that converge makes — before the call, and again after it —
// starts a new agent over the same node and the same local files, and
// drives it as the worker does (dnv-worker.md RW4, RW5, HL8, BM2): the held
// request is sent again on a non-zero code or another revision, nothing is
// sent while the agent answers "starting", and a migration piece the agent
// does not hold is pushed again. A cut passes when a pass of rounds sends
// nothing, every held object then answers code 0 at its revision, its rows
// read as in a run with no cut, and the node holds what that run leaves —
// or, for a state a restart forgets by design, the outcome its doc names:
//
//   - a cut before the request's save: the next agent holds the older
//     request, and the re-send brings the new one (SH5);
//   - a fence window lives in memory: a restart adopts a suspended per-CN
//     linear as past its window (DN12);
//   - an export a removal left without its namespace is left alone, not
//     named, until it is older than DnExportOrphanGrace, then named and
//     removed (DN6): the walk moves the clock past the grace;
//   - a lost migration piece waits for the next push (DN15).
//
// The next agent's startup pass converges every held object again (DN2), so
// the worker has to send something again only for the first and the last
// of those, or where the pass itself leaves a leftover, named, for a round
// (crashResendExpected); a cut that needs it anywhere else fails, for the
// worker's re-send would hide a startup pass that left its work undone.
//
// An end state can hide what happened on the way, so a cut also fails when
// the startup pass leaves a device suspended where it found one suspended
// (DN12: a restart takes it as past its window); when a migration source's
// suspended per-CN linear releases its held IO onto anything but its
// dm-error (crashBadReleases); and when work of the ended agent tries the
// node after its end joined it, the fence timer's converge already running
// aside. The snapshot holds what zeroing covered and whether each
// clone-metadata slot's head reads as zeros, over a disk whose metadata
// area starts stale; and a restart with no cut must keep every device under
// the number it had and make no call that changes the node, but DN2's
// re-apply of the migration pieces it holds (crashShape.startupChanges).
//
// The cut counts the converge's own calls by its trace id. Every call the dn
// agent makes to the node goes through the one common.OsClient
// NewDnAgentServer hands to the store, the disk metadata, dm, nvmet and the
// nvme host, so one crashGate per agent process sees them all, and once it
// has ended it refuses every call, as a dead process makes none.

// crashGate stands between one agent process and the fake node. It counts
// the calls count accepts, and ends at the cutAt-th of them: before it, the
// call is refused; after it, the call runs and every later one is refused.
type crashGate struct {
	inner    common.OsClient
	mu       sync.Mutex
	ended    bool
	cut      chan struct{} // closed when the gate ends
	inflight sync.WaitGroup
	count    func(traceId string) bool
	cutAt    int
	after    bool
	n        int
	lines    []string // the counted calls, in order
	cutLine  string
	// byId is the calls let through, per trace id.
	byId map[string][]string
	// stopped is set once the process's end has joined its background
	// work; late counts the calls refused after that, which only work that
	// outlives the join can make.
	stopped bool
	late    int
}

func newCrashGate(inner common.OsClient) *crashGate {
	return &crashGate{
		inner: inner,
		cut:   make(chan struct{}),
		byId:  make(map[string][]string),
	}
}

// arm starts counting the calls count accepts, from zero; cutAt 0 counts
// and never cuts.
func (g *crashGate) arm(count func(string) bool, cutAt int, after bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.count, g.cutAt, g.after = count, cutAt, after
	g.n, g.lines = 0, nil
}

// counted is the calls counted so far.
func (g *crashGate) counted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.lines)
}

// stop marks the process's background work joined (crashInc.end).
func (g *crashGate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
}

// lateCalls is how many calls were refused after stop.
func (g *crashGate) lateCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.late
}

// seenIds is every trace id a call has been let through under so far, and
// the ones given.
func (g *crashGate) seenIds(more ...string) map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make(map[string]bool, len(g.byId)+len(more))
	for id := range g.byId {
		ids[id] = true
	}
	for _, id := range more {
		ids[id] = true
	}
	return ids
}

// linesUnder is the calls let through under traceId, in order.
func (g *crashGate) linesUnder(traceId string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.byId[traceId])
}

func (g *crashGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.ended {
		g.ended = true
		close(g.cut)
	}
}

// enter decides one call: true lets it through, and the caller then runs it
// and marks it done.
func (g *crashGate) enter(ctx context.Context, line string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		if g.stopped {
			g.late++
		}
		return false
	}
	traceId, _ := common.TraceIdFromCtx(ctx)
	if g.count != nil && g.count(traceId) {
		g.n++
		g.lines = append(g.lines, line)
		if g.n == g.cutAt {
			g.ended = true
			g.cutLine = line
			close(g.cut)
			if !g.after {
				return false
			}
		}
	}
	g.byId[traceId] = append(g.byId[traceId], line)
	g.inflight.Add(1)
	return true
}

// errCrashEnded is what a refused call answers: the process that made it is
// gone.
var errCrashEnded = context.Canceled

func (g *crashGate) RunCommand(
	ctx context.Context, name string, args []string, stdin string,
) (string, string, int, error) {
	if !g.enter(ctx, "cmd "+strings.Join(append([]string{name}, args...),
		" ")) {
		return "", "", -1, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.RunCommand(ctx, name, args, stdin)
}

func (g *crashGate) ReadFile(ctx context.Context, path string) (string, error) {
	if !g.enter(ctx, "read "+path) {
		return "", errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadFile(ctx, path)
}

func (g *crashGate) ReadDir(ctx context.Context, path string) ([]string, error) {
	if !g.enter(ctx, "list "+path) {
		return nil, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadDir(ctx, path)
}

func (g *crashGate) ReadMtime(
	ctx context.Context, path string,
) (time.Time, error) {
	if !g.enter(ctx, "mtime "+path) {
		return time.Time{}, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadMtime(ctx, path)
}

func (g *crashGate) ReadDevNo(ctx context.Context, path string) (string, error) {
	if !g.enter(ctx, "devno "+path) {
		return "", errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadDevNo(ctx, path)
}

func (g *crashGate) WriteFile(ctx context.Context, path, data string) error {
	if !g.enter(ctx, "write "+path+"="+data) {
		return errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.WriteFile(ctx, path, data)
}

func (g *crashGate) WriteFileDirect(
	ctx context.Context, path, data string,
) error {
	if !g.enter(ctx, "writedirect "+path+"="+data) {
		return errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.WriteFileDirect(ctx, path, data)
}

func (g *crashGate) ReadBlock(
	ctx context.Context, path string, off, n uint64,
) ([]byte, error) {
	if !g.enter(ctx, fmt.Sprintf("readblock %s off=%d len=%d", path, off,
		n)) {
		return nil, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadBlock(ctx, path, off, n)
}

func (g *crashGate) ReadBlockDirect(
	ctx context.Context, path string, off, n uint64,
) ([]byte, error) {
	if !g.enter(ctx, fmt.Sprintf("readblockdirect %s off=%d len=%d", path,
		off, n)) {
		return nil, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadBlockDirect(ctx, path, off, n)
}

func (g *crashGate) WriteBlock(
	ctx context.Context, path string, off uint64, data []byte,
) error {
	if !g.enter(ctx, fmt.Sprintf("writeblock %s off=%d len=%d", path, off,
		len(data))) {
		return errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.WriteBlock(ctx, path, off, data)
}

func (g *crashGate) DiscardRanges(
	ctx context.Context, path string, ranges []common.DiscardRange,
) (int, error) {
	if !g.enter(ctx, fmt.Sprintf("discard %s ranges=%d", path,
		len(ranges))) {
		return 0, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.DiscardRanges(ctx, path, ranges)
}

func (g *crashGate) DialTcp(
	ctx context.Context, address string, timeout time.Duration,
) error {
	if !g.enter(ctx, "dial "+address) {
		return errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.DialTcp(ctx, address, timeout)
}

func (g *crashGate) StartDetached(
	ctx context.Context, name string, args []string, dir string,
) (*common.Detached, error) {
	if !g.enter(ctx, "detach "+name+" "+strings.Join(args, " ")) {
		return nil, errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.StartDetached(ctx, name, args, dir)
}

func (g *crashGate) ReadProto(
	ctx context.Context, path string, m proto.Message,
) error {
	if !g.enter(ctx, "readproto "+path) {
		return errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.ReadProto(ctx, path, m)
}

func (g *crashGate) WriteProto(
	ctx context.Context, path string, m proto.Message,
) error {
	if !g.enter(ctx, "writeproto "+path) {
		return errCrashEnded
	}
	defer g.inflight.Done()
	return g.inner.WriteProto(ctx, path, m)
}

var _ common.OsClient = (*crashGate)(nil)

// crashById counts the calls under one trace id.
func crashById(traceId string) func(string) bool {
	return func(id string) bool { return id == traceId }
}

// crashByUnknownId counts the calls under every trace id not in known: the
// ids an agent makes itself, as the DN12 fence timer's converge does.
func crashByUnknownId(known map[string]bool) func(string) bool {
	return func(id string) bool { return !known[id] }
}

// newCrashNode is newTestServerOnPort's node, on a fake clock of its own
// that the node's configfs stamps and every agent over it share. Its disk's
// clone-metadata area holds stale bytes, as a disk used before does: a slot
// is zeroed at its head before its record is saved (AllocCloneMeta), and on
// a disk of zeros a restart that skipped that would look the same.
func newCrashNode() (*fakeNode, *dnClock) {
	node := newFakeNode()
	node.devSize[testDisk] = 512 << 20
	node.devNo[testDisk] = "253:0"
	node.dirs[agent.NvmetRoot] = true
	stale := make([]byte, 64<<10)
	for i := range stale {
		stale[i] = 0xa5
	}
	node.corruptBlock(testDisk, common.DnCloneMetaOffset, stale)
	clock := &dnClock{now: time.Unix(1<<30, 0)}
	node.clock = clock.Now
	return node, clock
}

// crashInc is one agent process over a node.
type crashInc struct {
	srv    *DnAgentServer
	gate   *crashGate
	root   context.Context
	cancel context.CancelFunc
	ended  bool
}

// startCrashInc starts an agent process the way agent.Serve does, up to its
// startup pass: Prepare, after which the agent answers "starting" (SH33).
// The settings are startTestServerOnPort's, and the clock is the node's.
// Nothing registers a cleanup that holds the server: a walk starts
// thousands, and each is ended by end.
func startCrashInc(
	node *fakeNode,
	clock *dnClock,
	fenceWait time.Duration,
) *crashInc {
	gate := newCrashGate(node.osClient())
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewDnAgentServer(gate, nf, common.DefaultLocalStorPrefix,
		testDisk, testTrConf(), common.NvmetPortId, 0)
	srv.fenceWait = fenceWait
	srv.zeroRetryInterval = time.Millisecond
	srv.zeroBatchMax = testZeroBatchMax
	srv.zeroBatchMin = testZeroBatchMin
	srv.roundReads = false
	srv.now = clock.Now
	srv.sleep = clock.Sleep
	root, cancel := context.WithCancel(context.Background())
	srv.Prepare(root)
	return &crashInc{srv: srv, gate: gate, root: root, cancel: cancel}
}

// reconcile runs the startup pass on the root context under traceId, as
// Serve does, so that ending the process reaches a pass in flight too.
func (inc *crashInc) reconcile(traceId string) error {
	return inc.srv.Reconcile(common.WithTraceId(inc.root, traceId))
}

// end plays the process's death: the gate refuses every later call, the root
// context ends, the calls in flight return, and the background goroutines
// the server enrolls are joined (stopTestServer). A fence timer's converge
// already running is not enrolled (WaitBackground's carve-out): it runs on,
// every call of it refused, and the gate counts those calls as late. A
// second end does nothing.
func (inc *crashInc) end(t *testing.T) {
	if inc.ended {
		return
	}
	inc.ended = true
	inc.gate.end()
	inc.cancel()
	inc.gate.inflight.Wait()
	stopTestServer(t, inc.srv)
	inc.gate.stop()
}

// crashHeld is what the worker holds for the node: the DN's request, its
// sides' requests, and the migration pieces it has pushed.
type crashHeld struct {
	dn     *pb.SyncupDnRequest
	sides  []*pb.SyncupSideRequest
	pushes []*pb.PushMigrBitmapRequest
}

// crashDriveBound is the most passes a drive takes before it gives up.
const crashDriveBound = 8

// crashDrive plays the worker against srv until a whole pass sends nothing,
// and returns that pass's number and the rows its rounds read, or why no
// pass within crashDriveBound did. Each round answers what the node holds
// now (checkDnNow, checkSideNow). A round that answers "starting" gives no
// verdict and sends nothing (dnv-worker.md HL8), but the pass is not quiet;
// any other non-zero code, or another revision, sends the held request
// again (dnv-worker.md RW4). A piece of a held migration the side does not
// list is pushed again, as the next accepted SyncupSide reply's bm_info
// would have the worker do (dnv-worker.md BM2).
func crashDrive(
	srv *DnAgentServer,
	held crashHeld,
	prefix string,
) (int, string, string) {
	for pass := 1; pass <= crashDriveBound; pass++ {
		ctx := traced(fmt.Sprintf("%s-%d", prefix, pass))
		quiet := true
		var rows []string
		round, info := checkDnNow(srv, ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn,
			Revision: held.dn.GetRevision(), ShowInfo: true,
		}, nil)
		switch code := round.GetAgentReply().GetCode(); {
		case code == common.ReplyCodeStarting:
			quiet = false
		case code != 0 || round.GetRevision() != held.dn.GetRevision():
			quiet = false
			_, _ = srv.SyncupDn(ctx,
				proto.Clone(held.dn).(*pb.SyncupDnRequest))
		default:
			for _, row := range notOkRows(info) {
				rows = append(rows, "dn"+row)
			}
		}
		for _, side := range held.sides {
			if !crashWaitZeroed(srv, side) {
				return pass, "", fmt.Sprintf("side %d never finished "+
					"zeroing", side.GetSidePointer().GetSideId())
			}
			round, info := checkSideNow(srv, ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: side.GetSidePointer(),
				Revision:    side.GetRevision(), ShowInfo: true,
			}, nil)
			switch code := round.GetAgentReply().GetCode(); {
			case code == common.ReplyCodeStarting:
				quiet = false
			case code != 0 || round.GetRevision() != side.GetRevision():
				quiet = false
				_, _ = srv.SyncupSide(ctx,
					proto.Clone(side).(*pb.SyncupSideRequest))
			default:
				for _, row := range notOkRows(info) {
					rows = append(rows, fmt.Sprintf("side %d%s",
						side.GetSidePointer().GetSideId(), row))
				}
			}
		}
		for _, push := range held.pushes {
			if !crashHoldsPiece(srv, push) {
				quiet = false
				_, _ = srv.PushMigrBitmap(ctx,
					proto.Clone(push).(*pb.PushMigrBitmapRequest))
			}
		}
		if quiet {
			sort.Strings(rows)
			return pass, strings.Join(rows, "\n"), ""
		}
	}
	return -1, "", fmt.Sprintf("no pass of %d sent nothing", crashDriveBound)
}

// crashHoldsPiece reports whether the side a push addresses lists its piece
// of the push's migration: what the agent's SyncupSide reply carries in
// bm_info (bitmapInfo).
func crashHoldsPiece(
	srv *DnAgentServer,
	push *pb.PushMigrBitmapRequest,
) bool {
	ptr := push.GetSidePointer()
	st := srv.getSide(sideKey(push.GetClusterId(), push.GetDnId(),
		ptr.GetSpId(), ptr.GetSideId()))
	if st == nil {
		return false
	}
	info := srv.bitmapInfo(st)
	return info.GetResId() == push.GetMigrId() &&
		slices.Contains(info.GetBmIdxList(), push.GetBmIdx())
}

// crashZeroingBound is how long a drive waits for a side's zeroing: many
// times what the fake's few batches take, even under the race detector.
const crashZeroingBound = 2 * time.Second

// crashWaitZeroed waits until the side's record says its zeroing is done, as
// a worker waits for the flip (DN9) before it judges a side at provisioned =
// false, and reports whether it was done within crashZeroingBound. It
// returns at once for a side with no record.
func crashWaitZeroed(srv *DnAgentServer, side *pb.SyncupSideRequest) bool {
	deadline := time.Now().Add(crashZeroingBound)
	for {
		rec, ok, err := srv.meta.LookupSide(context.Background(),
			side.GetSidePointer().GetSpId(),
			side.GetSidePointer().GetSideId())
		if err != nil || !ok || sideFullyZeroed(rec) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// crashSettleFence waits until no fence window of srv is open: no per-CN
// linear on the node is suspended and no fence timer is armed, and the
// timer's converge, which holds the node read lock, has ended. It reports
// whether that happened within a bound.
func crashSettleFence(
	srv *DnAgentServer,
	node *fakeNode,
	window time.Duration,
) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !crashAnySuspended(node) {
			srv.locks.Node().Lock()
			srv.mu.Lock()
			armed := false
			for _, st := range srv.sides {
				if st.fenceTimer != nil {
					armed = true
				}
			}
			srv.mu.Unlock()
			srv.locks.Node().Unlock()
			if !armed {
				return true
			}
		}
		time.Sleep(window / 2)
	}
	return false
}

func crashAnySuspended(node *fakeNode) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, dm := range node.dms {
		if dm.suspended {
			return true
		}
	}
	return false
}

var crashDevNoRe = regexp.MustCompile(`\b253:\d+\b`)

// crashSnapshot is what the node holds, in lines: the dm devices with their
// tables (device numbers named by their devices), states and dm-clone knobs,
// the ranges a device was discarded at, as a set — a dm-clone's hydration
// marks, which a restart's apply makes again (DN2) — and the bytes its
// zeroouts covered, merged, which a restart's zeroing must neither skip nor
// claim (DN9) whatever size it gives its batches; the nvmet configfs tree;
// the nvme host connections; the local store, byte for byte; the volume
// table's records, and whether each clone-metadata slot's head reads as
// zeros. Left out: configfs times, the zeroout batches themselves, and the
// devices' own numbers, which the startup walk compares on its own
// (crashDevNos). It reads the disk through NewDiskMeta, which only loads,
// and the fake's blocks; its reads land in the node's call log.
func crashSnapshot(node *fakeNode) string {
	var out []string
	node.mu.Lock()
	byNo := make(map[string]string)
	for path, no := range node.devNo {
		byNo[no] = path
	}
	norm := func(s string) string {
		return crashDevNoRe.ReplaceAllStringFunc(s, func(no string) string {
			if path, ok := byNo[no]; ok {
				return path
			}
			return no
		})
	}
	for name, dm := range node.dms {
		discarded := slices.Clone(dm.discards)
		slices.Sort(discarded)
		out = append(out, fmt.Sprintf("dm %s suspended=%t ro=%t "+
			"waiting=%q nohydration=%t nodiscardpassdown=%t threshold=%d "+
			"batch=%d metamode=%q table=%s discarded=%q", name,
			dm.suspended, dm.readOnly, norm(dm.waitingTable()),
			dm.noHydration, dm.noDiscardPassdown, dm.threshold,
			dm.batchSize, dm.metaMode, norm(dm.table),
			slices.Compact(discarded)))
		if covered := crashZeroCoverage(dm.zeroouts); covered != "" {
			out = append(out, "zeroed "+name+" "+covered)
		}
	}
	for dir, ok := range node.dirs {
		if ok && strings.HasPrefix(dir, agent.NvmetRoot) {
			out = append(out, "dir "+dir)
		}
	}
	for path, value := range node.files {
		if strings.HasPrefix(path, agent.NvmetRoot) {
			out = append(out, fmt.Sprintf("file %s=%q", path,
				strings.TrimSpace(norm(value))))
		}
	}
	for link, target := range node.links {
		out = append(out, "link "+link+" -> "+target)
	}
	for nqn, conn := range node.conns {
		var ctrls []string
		for _, ctrl := range conn.ctrls {
			ctrls = append(ctrls, fmt.Sprintf("%s:%s:%s %s host=%s",
				ctrl.trType, ctrl.trAddr, ctrl.trSvcId, ctrl.state,
				ctrl.hostNqn))
		}
		sort.Strings(ctrls)
		out = append(out, fmt.Sprintf("conn %s kept=%t ctrls=%v", nqn,
			conn.kept, ctrls))
	}
	for path, raw := range node.protos {
		out = append(out, fmt.Sprintf("store %s %x", path, raw))
	}
	node.mu.Unlock()
	meta := NewDiskMeta(node.osClient(), testDisk)
	if recs, err := meta.SideRecords(context.Background()); err != nil {
		out = append(out, "side records: "+err.Error())
	} else {
		for _, rec := range recs {
			out = append(out, "side record "+
				prototext.MarshalOptions{}.Format(rec))
		}
	}
	if recs, err := meta.CloneMetaRecords(context.Background()); err != nil {
		out = append(out, "clone metadata records: "+err.Error())
	} else {
		for _, rec := range recs {
			out = append(out, "clone metadata record "+
				prototext.MarshalOptions{}.Format(rec))
			head, err := node.osClient().ReadBlock(context.Background(),
				testDisk, common.DnCloneMetaOffset+
					rec.GetUnitStart()*common.DnCloneMetaUnit, 8<<10)
			zero := err == nil
			for _, b := range head {
				zero = zero && b == 0
			}
			out = append(out, fmt.Sprintf("clone metadata head %d zero=%t",
				rec.GetUnitStart(), zero))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// crashZeroCoverage is the byte ranges a device's `blkdiscard --zeroout`
// runs covered, merged: which bytes were zeroed, whatever the batches.
func crashZeroCoverage(zeroouts []string) string {
	type span struct{ from, to uint64 }
	var spans []span
	for _, zeroout := range zeroouts {
		fields := strings.Fields(zeroout)
		var off, n uint64
		for i := 0; i+1 < len(fields); i++ {
			switch fields[i] {
			case "--offset":
				_, _ = fmt.Sscan(fields[i+1], &off)
			case "--length":
				_, _ = fmt.Sscan(fields[i+1], &n)
			}
		}
		spans = append(spans, span{off, off + n})
	}
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].from < spans[j].from
	})
	var merged []span
	for _, sp := range spans {
		if last := len(merged) - 1; last >= 0 && sp.from <= merged[last].to {
			merged[last].to = max(merged[last].to, sp.to)
			continue
		}
		merged = append(merged, sp)
	}
	if len(merged) == 0 {
		return ""
	}
	return fmt.Sprint(merged)
}

// crashDevNos is every dm device's own number: a restart that finds a device
// right keeps it, and so its number.
func crashDevNos(node *fakeNode) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	var out []string
	for name := range node.dms {
		out = append(out, name+" "+node.devNo["/dev/mapper/"+name])
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// crashNodeChanges is the calls of lines that change the node: all but the
// probes of readOnlyPrefixes.
func crashNodeChanges(lines []string) []string {
	var out []string
	for _, line := range lines {
		if !slices.ContainsFunc(readOnlyPrefixes, func(prefix string) bool {
			return strings.HasPrefix(line, prefix)
		}) {
			out = append(out, line)
		}
	}
	return out
}

// crashBadReleases lists, for a migration source's shape, every resume of a
// suspended per-CN linear (-d1-) that released the IO it held onto anything
// but that linear's own dm-error (-d0-): the IO a fence window holds must
// fail, and must never reach the side's data (DN12). Another shape resumes
// a linear onto the side for a reload that is no fence: a failover's flip, a
// finish's repoint.
func crashBadReleases(node *fakeNode, source bool) []string {
	if !source {
		return nil
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	byNo := make(map[string]string)
	for path, no := range node.devNo {
		byNo[no] = path
	}
	var bad []string
	for name, tables := range node.releasedOnto {
		if !strings.Contains(name, "-d1-") {
			continue
		}
		want := "/dev/mapper/" + strings.Replace(name, "-d1-", "-d0-", 1)
		for _, table := range tables {
			fields := strings.Fields(table)
			if len(fields) < 4 || byNo[fields[3]] != want {
				bad = append(bad, fmt.Sprintf("%s onto %q (%s)", name,
					table, byNo[fields[min(3, len(fields)-1)]]))
			}
		}
	}
	sort.Strings(bad)
	return bad
}

// crashForgetReleases drops what the resumes so far released onto: a shape's
// setup is no part of what the walk judges.
func crashForgetReleases(node *fakeNode) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.releasedOnto = make(map[string][]string)
}

// crashDiff is the lines one snapshot holds and the other does not.
func crashDiff(want, got string) string {
	wantSet := make(map[string]bool)
	for _, line := range strings.Split(want, "\n") {
		wantSet[line] = true
	}
	gotSet := make(map[string]bool)
	for _, line := range strings.Split(got, "\n") {
		gotSet[line] = true
	}
	var out []string
	for line := range wantSet {
		if !gotSet[line] {
			out = append(out, "  - "+crashTrunc(line))
		}
	}
	for line := range gotSet {
		if !wantSet[line] {
			out = append(out, "  + "+crashTrunc(line))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func crashTrunc(line string) string {
	if len(line) > 300 {
		return line[:300] + "..."
	}
	return line
}

// crashLeavesNsLess reports whether the first ran calls of a converge include
// the `rmdir` of a side export's namespace and not the `rmdir` of that
// export's own directory after it: the cut leaves the export without its
// namespace, which DN6's age rule leaves alone until it is older than the
// grace. A migration source's export names its disk node, so a sweep takes it
// by its name, whatever its age.
func crashLeavesNsLess(lines []string, ran int) bool {
	prefix := "cmd rmdir " + agent.NvmetRoot + "/subsystems/"
	for i, line := range lines[:min(ran, len(lines))] {
		subsys, ok := strings.CutSuffix(line, "/namespaces/1")
		nqn, under := strings.CutPrefix(subsys, prefix)
		if !ok || !under {
			continue
		}
		if parts, ok := common.ParseNqn(nqn); !ok ||
			parts.Kind != common.NqnKindSideToCn {
			continue
		}
		own := slices.Index(lines[i+1:], subsys)
		if own < 0 || i+1+own+1 > ran {
			return true
		}
	}
	return false
}

var (
	crashRequestSave = regexp.MustCompile(`^writeproto \S*/(dn|side)-`)
	crashPieceSave   = regexp.MustCompile(`^writeproto \S*/migr-bm-`)
	crashLinearHold  = regexp.MustCompile(`^cmd dmsetup suspend (\S*-d1-\S*)$`)
)

// crashResendExpected names why a cut may leave the worker something to
// send once the next agent's startup pass has run, and is "" when nothing
// may: the pass converges every held object again (DN2), so the worker
// sends again only what a restart forgets by design, or what the pass
// itself leaves, named, for a round. ran is how many of the converge's
// calls, lines, ran.
func crashResendExpected(lines []string, ran int) string {
	done := lines[:min(ran, len(lines))]
	for _, line := range lines[len(done):] {
		switch {
		case crashRequestSave.MatchString(line):
			return "the cut fell before the request's save, so the next " +
				"agent holds the request before it (SH5)"
		case crashPieceSave.MatchString(line):
			return "the cut fell before the piece's save, so it waits " +
				"for the next push (DN15)"
		}
	}
	for i, line := range done {
		m := crashLinearHold.FindStringSubmatch(line)
		if m == nil ||
			slices.Contains(done[i+1:], "cmd dmsetup resume "+m[1]) {
			continue
		}
		return "the cut left a per-CN linear suspended, which the sweep " +
			"leaves to the build phase, so a device it maps waits a " +
			"round, named (DN6, DN13)"
	}
	return ""
}

// crashNsLessExports lists the nvmet subsystems on the node that have no
// namespace one.
func crashNsLessExports(node *fakeNode) []string {
	node.mu.Lock()
	defer node.mu.Unlock()
	prefix := agent.NvmetRoot + "/subsystems/"
	var out []string
	for dir, ok := range node.dirs {
		rest, found := strings.CutPrefix(dir, prefix)
		if !ok || !found || rest == "" || strings.Contains(rest, "/") {
			continue
		}
		if !node.dirs[dir+"/namespaces/1"] {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

// crashSampled reports whether the walk runs the i-th cut: every one, or
// every third under the race detector and under -short, which keeps the
// package's test time usable there.
func crashSampled(i int) bool {
	if raceEnabled || testing.Short() {
		return i%3 == 0
	}
	return true
}

// crashQuietLogs sends the agent's log records nowhere for the rest of the
// test: thousands of agent processes would bury a failure's message. No
// other test of the package runs meanwhile, none being parallel.
func crashQuietLogs(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// crashOk fails the test unless a setup step's call answered code 0.
func crashOk(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
	err error,
) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("%s: code %d (%s)", what, got.GetCode(), got.GetDetails())
	}
}

// crashShape is one converge to crash through. setup brings a fresh node,
// on the first agent, to the state before it, and conv is the converge;
// held is what the worker holds once conv has been sent. minCalls is the
// fewest calls the converge makes under its trace id: a change that quietly
// shortens it shrinks the walk, and this says so.
type crashShape struct {
	name      string
	fenceWait time.Duration
	// zeroing says the converge starts a side's zeroing loop (DN9), whose
	// batches run under trace ids of their own; the walk cuts every call of
	// them too, as it cuts the fence timer's converge.
	zeroing bool
	// source says the side is a migration source's, whose per-CN linears
	// are suspended only by the fence (crashBadReleases).
	source   bool
	setup    func(t *testing.T, srv *DnAgentServer)
	conv     func(ctx context.Context, srv *DnAgentServer)
	held     crashHeld
	minCalls int
	// minBackgroundCalls is the fewest calls of the work the converge
	// leaves running — the fence timer's converge (DN12 phase 2), or the
	// zeroing loop; minStartupCalls, the fewest the startup pass after the
	// shape makes.
	minBackgroundCalls int
	minStartupCalls    int
	// startupChanges is how many calls of that startup pass, with no cut,
	// change the node: none, for a pass that finds every object right
	// leaves it as it is (SH16) — a needless reload of a serving device
	// stalls its host IO — save DN2's re-apply of the migration pieces a
	// destination side holds (SH21 to SH23), which every start makes.
	startupChanges int
}

// background names the work a shape's converge leaves running, "" for none.
func (sh crashShape) background() string {
	switch {
	case sh.fenceWait > 0:
		return "the fence timer's converge"
	case sh.zeroing:
		return "the zeroing loop"
	}
	return ""
}

// crashWindow is the fence window of the source cutover's walk with one: as
// short as it can be and still be open when the cutover's converge ends.
const crashWindow = 10 * time.Millisecond

func crashShapes() []crashShape {
	ctx := context.Background()
	rw := pb.SpLevel_SP_LEVEL_READWRITE
	plain := func(revision uint64) *pb.SyncupSideRequest {
		return sideReq(revision, testSide, testCn0, []uint64{testCn1}, rw)
	}
	unprovisioned := func(revision uint64) *pb.SyncupSideRequest {
		return unprovisionedSideReq(revision, testSide, testCn0,
			[]uint64{testCn1}, rw)
	}
	dstProvision := func() *pb.SyncupSideRequest {
		req := pendingDstReq(1, rw)
		req.SideConf.Provisioned = false
		return req
	}
	push := func() *pb.PushMigrBitmapRequest {
		return pushReq(0, testMigrId, []byte{0x05})
	}
	withDn := func(t *testing.T, srv *DnAgentServer) {
		reply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
		crashOk(t, "SyncupDn", reply, err)
	}
	built := func(t *testing.T, srv *DnAgentServer) {
		withDn(t, srv)
		syncupSideTwoPhase(t, srv, plain(1))
	}
	// A destination's life as the worker drives it: provisioned with its
	// migration's conf and the cutover pending, the cutover, a piece of
	// the bitmap, the finish. Each step's setup runs the ones before it.
	dstSteps := []func(t *testing.T, srv *DnAgentServer){
		withDn,
		func(t *testing.T, srv *DnAgentServer) {
			reply, err := srv.SyncupSide(ctx, dstProvision())
			crashOk(t, "SyncupSide (provisioning)", reply, err)
			waitZeroed(t, srv, testSide)
		},
		func(t *testing.T, srv *DnAgentServer) {
			reply, err := srv.SyncupSide(ctx, pendingDstReq(2, rw))
			crashOk(t, "SyncupSide (cutover pending)", reply, err)
		},
		func(t *testing.T, srv *DnAgentServer) {
			reply, err := srv.SyncupSide(ctx, migrDstReq(3, rw))
			crashOk(t, "SyncupSide (cutover)", reply, err)
		},
		func(t *testing.T, srv *DnAgentServer) {
			reply, err := srv.PushMigrBitmap(ctx, push())
			crashOk(t, "PushMigrBitmap", reply, err)
		},
	}
	dstUpTo := func(n int) func(t *testing.T, srv *DnAgentServer) {
		return func(t *testing.T, srv *DnAgentServer) {
			for _, step := range dstSteps[:n] {
				step(t, srv)
			}
		}
	}
	sendDn := func(req *pb.SyncupDnRequest) func(
		context.Context, *DnAgentServer) {
		return func(ctx context.Context, srv *DnAgentServer) {
			_, _ = srv.SyncupDn(ctx, req)
		}
	}
	sendSide := func(req *pb.SyncupSideRequest) func(
		context.Context, *DnAgentServer) {
		return func(ctx context.Context, srv *DnAgentServer) {
			_, _ = srv.SyncupSide(ctx, req)
		}
	}
	return []crashShape{{
		name:            "a fresh disk node",
		setup:           func(*testing.T, *DnAgentServer) {},
		conv:            sendDn(dnReq(1, testSide)),
		held:            crashHeld{dn: dnReq(1, testSide)},
		minCalls:        42,
		minStartupCalls: 18,
	}, {
		name:    "a provisioning side",
		zeroing: true,
		setup:   withDn,
		conv:    sendSide(unprovisioned(1)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{unprovisioned(1)}},
		minCalls:           10,
		minStartupCalls:    32,
		minBackgroundCalls: 6,
	}, {
		name: "an exporting side",
		setup: func(t *testing.T, srv *DnAgentServer) {
			withDn(t, srv)
			provisionSide(t, srv, 1, testSide)
		},
		conv: sendSide(plain(2)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{plain(2)}},
		minCalls:        84,
		minStartupCalls: 78,
	}, {
		name:  "a failover",
		setup: built,
		conv:  sendSide(failoverReq(2)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{failoverReq(2)}},
		minCalls:        73,
		minStartupCalls: 78,
	}, {
		name: "a failover sent again",
		setup: func(t *testing.T, srv *DnAgentServer) {
			built(t, srv)
			reply, err := srv.SyncupSide(ctx, failoverReq(2))
			crashOk(t, "SyncupSide (failover)", reply, err)
		},
		conv: sendSide(failoverReq(2)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{failoverReq(2)}},
		minCalls:        58,
		minStartupCalls: 78,
	}, {
		name: "a destination built at once",
		setup: func(t *testing.T, srv *DnAgentServer) {
			withDn(t, srv)
			provisionSide(t, srv, 1, testSide)
		},
		conv: sendSide(migrDstReq(2, rw)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{migrDstReq(2, rw)}},
		minCalls:        107,
		minStartupCalls: 98,
	}, {
		name:    "a destination provisioning",
		zeroing: true,
		setup:   dstUpTo(1),
		conv:    sendSide(dstProvision()),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{dstProvision()}},
		minCalls:           12,
		minStartupCalls:    32,
		minBackgroundCalls: 6,
	}, {
		name:  "a destination waiting for its cutover",
		setup: dstUpTo(2),
		conv:  sendSide(pendingDstReq(2, rw)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{pendingDstReq(2, rw)}},
		minCalls:        82,
		minStartupCalls: 82,
	}, {
		name:  "a destination's cutover",
		setup: dstUpTo(3),
		conv:  sendSide(migrDstReq(3, rw)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{migrDstReq(3, rw)}},
		minCalls:        91,
		minStartupCalls: 98,
	}, {
		name:  "a destination's bitmap piece",
		setup: dstUpTo(4),
		conv: func(ctx context.Context, srv *DnAgentServer) {
			_, _ = srv.PushMigrBitmap(ctx, push())
		},
		held: crashHeld{dn: dnReq(1, testSide),
			sides:  []*pb.SyncupSideRequest{migrDstReq(3, rw)},
			pushes: []*pb.PushMigrBitmapRequest{push()}},
		minCalls:        16,
		minStartupCalls: 113,
		// The re-apply of the held piece: hydration off, the per-CN
		// linear suspended, the discard, the clear, the resume, hydration
		// on.
		startupChanges: 6,
	}, {
		name:  "a destination's finish",
		setup: dstUpTo(5),
		conv:  sendSide(plain(4)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{plain(4)}},
		minCalls:        85,
		minStartupCalls: 78,
	}, {
		name:   "a source's cutover",
		source: true,
		setup:  built,
		conv:   sendSide(migrSrcReq(2)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{migrSrcReq(2)}},
		minCalls:        91,
		minStartupCalls: 100,
	}, {
		name:      "a source's cutover with a fence window",
		fenceWait: crashWindow,
		source:    true,
		setup:     built,
		conv:      sendSide(migrSrcReq(2)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{migrSrcReq(2)}},
		minCalls:           78,
		minBackgroundCalls: 82,
	}, {
		name: "a source's end",
		setup: func(t *testing.T, srv *DnAgentServer) {
			built(t, srv)
			reply, err := srv.SyncupSide(ctx, migrSrcReq(2))
			crashOk(t, "SyncupSide (source)", reply, err)
		},
		conv: sendSide(plain(3)),
		held: crashHeld{dn: dnReq(1, testSide),
			sides: []*pb.SyncupSideRequest{plain(3)}},
		minCalls:        89,
		minStartupCalls: 78,
	}, {
		name: "one side of two dropped",
		setup: func(t *testing.T, srv *DnAgentServer) {
			reply, err := srv.SyncupDn(ctx,
				legDnReq(1, testSide, testSide2))
			crashOk(t, "SyncupDn", reply, err)
			syncupSideTwoPhase(t, srv, legSideReq(1, testSide))
			syncupSideTwoPhase(t, srv, legSideReq(1, testSide2))
		},
		conv: sendDn(legDnReq(2, testSide2)),
		held: crashHeld{dn: legDnReq(2, testSide2),
			sides: []*pb.SyncupSideRequest{legSideReq(1, testSide2)}},
		minCalls:        63,
		minStartupCalls: 78,
	}, {
		name:            "the only side dropped",
		setup:           built,
		conv:            sendDn(dnReq(2)),
		held:            crashHeld{dn: dnReq(2)},
		minCalls:        63,
		minStartupCalls: 18,
	}}
}

// crashRef is what a run with no cut leaves: the node, and the rows of the
// quiet pass.
type crashRef struct {
	snap string
	rows string
}

// settle drives srv to a quiet pass and, for a shape with a fence window,
// waits for the window to end and drives again. It returns the first
// drive's passes and the last drive's rows, or a failure.
func (sh crashShape) settle(
	srv *DnAgentServer,
	node *fakeNode,
	prefix string,
) (int, string, string) {
	passes, rows, failure := crashDrive(srv, sh.held, prefix)
	if failure != "" || sh.fenceWait == 0 {
		return passes, rows, failure
	}
	if !crashSettleFence(srv, node, sh.fenceWait) {
		return passes, rows, "a fence window never ended"
	}
	_, rows, failure = crashDrive(srv, sh.held, prefix+"-after-window")
	if failure != "" {
		failure = "after the window: " + failure
	}
	return passes, rows, failure
}

// crashCut is one cut: which call, and on which side of it.
type crashCut struct {
	phase string // which work: the shape's converge, its background, a pass
	k     int
	after bool
	line  string
}

func (c crashCut) String() string {
	when := "before"
	if c.after {
		when = "after"
	}
	return fmt.Sprintf("%s, %s call %d (%s)", c.phase, when, c.k, c.line)
}

// crashOutcome is what a cut came to.
type crashOutcome struct {
	failure string
	passes  int
	aged    bool
}

// restartAndJudge is everything after the cut. ended is the gate of the agent
// the cut ended. A new agent starts over the same node; it answers
// "starting" and touches nothing until its startup pass. The pass runs, and
// it must have ended every suspension it found (DN12: a restart takes a
// suspended per-CN linear as past its window, and retires it at once). The
// worker then drives it to a quiet pass — after any fence window it opened
// has ended, as a window runs on real time — and the node and the rows are
// compared with the reference. ran is how many of the cut work's calls ran;
// refLines are its calls.
func (sh crashShape) restartAndJudge(
	t *testing.T,
	node *fakeNode,
	clock *dnClock,
	ended *crashGate,
	ref crashRef,
	refLines []string,
	ran int,
	startupId string,
) crashOutcome {
	t.Helper()
	suspended := crashAnySuspended(node)
	before := len(node.Calls())
	next := startCrashInc(node, clock, sh.fenceWait)
	defer next.end(t)
	round, _ := next.srv.checkDnRound(traced("starting-round"),
		&pb.CheckDnRequest{ClusterId: testCluster, DnId: testDn,
			Revision: sh.held.dn.GetRevision()}, nil)
	syncup, _ := next.srv.SyncupDn(traced("starting-syncup"),
		proto.Clone(sh.held.dn).(*pb.SyncupDnRequest))
	if round.GetAgentReply().GetCode() != common.ReplyCodeStarting ||
		syncup.GetAgentReply().GetCode() != common.ReplyCodeStarting {
		return crashOutcome{failure: fmt.Sprintf("before its startup pass "+
			"the agent answered a round %d and a syncup %d, not starting",
			round.GetAgentReply().GetCode(),
			syncup.GetAgentReply().GetCode())}
	}
	if after := len(node.Calls()); after != before {
		return crashOutcome{failure: fmt.Sprintf("the starting agent made "+
			"%d calls before its startup pass", after-before)}
	}
	if err := next.reconcile(startupId); err != nil {
		return crashOutcome{failure: "the startup pass failed: " +
			err.Error()}
	}
	if suspended && crashAnySuspended(node) {
		return crashOutcome{failure: "the startup pass left a device " +
			"suspended, and it found one suspended"}
	}
	if sh.fenceWait > 0 && !crashSettleFence(next.srv, node, sh.fenceWait) {
		return crashOutcome{failure: "a fence window the startup pass " +
			"opened never ended"}
	}
	passes, rows, failure := sh.settle(next.srv, node, "drive")
	out := crashOutcome{passes: passes, aged: crashLeavesNsLess(refLines,
		ran)}
	if failure != "" {
		out.failure = failure
		return out
	}
	if passes > 1 && crashResendExpected(refLines, ran) == "" {
		out.failure = fmt.Sprintf("the startup pass left the worker "+
			"something to send: %d passes", passes)
		return out
	}
	if out.aged {
		// DN6: an export without its namespace is left alone, and not
		// named, while it is younger than the grace.
		if len(crashNsLessExports(node)) == 0 {
			out.failure = "the cut left an export without its namespace, " +
				"and the node holds none"
			return out
		}
		clock.mu.Lock()
		clock.now = clock.now.Add(common.DnExportOrphanGrace + time.Second)
		clock.mu.Unlock()
		var aged int
		aged, rows, failure = sh.settle(next.srv, node, "drive-aged")
		if failure != "" {
			out.failure = "past the grace: " + failure
			return out
		}
		if aged < 2 {
			out.failure = "past the grace, no round named the export " +
				"left without its namespace"
			return out
		}
	}
	node.mu.Lock()
	wedged := slices.Clone(node.wedgedDisables)
	node.mu.Unlock()
	next.end(t)
	if late := ended.lateCalls(); late > 0 && sh.fenceWait == 0 {
		// Only a fence timer's converge already running outlives the
		// join (crashInc.end), and this shape arms no timer.
		out.failure = fmt.Sprintf("the ended agent tried %d calls after "+
			"its background work was joined", late)
		return out
	}
	if len(wedged) > 0 {
		out.failure = fmt.Sprintf("a namespace was disabled over a "+
			"suspended device: %v", wedged)
		return out
	}
	if bad := crashBadReleases(node, sh.source); len(bad) > 0 {
		out.failure = fmt.Sprintf("a suspended per-CN linear released its "+
			"held IO onto a table other than its dm-error: %v", bad)
		return out
	}
	if rows != ref.rows {
		out.failure = fmt.Sprintf("the rows read\n%s\nwant\n%s", rows,
			ref.rows)
		return out
	}
	if got := crashSnapshot(node); got != ref.snap {
		out.failure = "the node differs from a run with no cut:\n" +
			crashDiff(ref.snap, got)
	}
	return out
}

// crashTally collects a walk's outcomes and reports the first failures.
type crashTally struct {
	cuts, fails, aged int
	passes            map[int]int
}

func (tl *crashTally) add(t *testing.T, cut crashCut, out crashOutcome) {
	t.Helper()
	tl.cuts++
	if tl.passes == nil {
		tl.passes = make(map[int]int)
	}
	tl.passes[out.passes]++
	if out.aged {
		tl.aged++
	}
	if out.failure != "" {
		tl.fails++
		if tl.fails <= 5 {
			t.Errorf("cut %s: %s", cut, out.failure)
		}
	}
}

func (tl *crashTally) report(t *testing.T, what string, start time.Time) {
	t.Helper()
	if tl.fails > 5 {
		t.Errorf("%d more cuts failed", tl.fails-5)
	}
	t.Logf("%s: %d cuts in %v; %d needed the clock past the grace; passes "+
		"to a quiet one %v", what, tl.cuts, time.Since(start).Round(
		time.Millisecond), tl.aged, tl.passes)
}

// TestACrashAtEveryStepOfAConverge ends the agent at every call of each
// shape's converge, before it and after it, and starts it again over the
// same node. A shape whose converge leaves work running under trace ids of
// its own — the fence timer's converge (DN12 phase 2), a zeroing loop
// (DN9) — also has every call of that work cut.
func TestACrashAtEveryStepOfAConverge(t *testing.T) {
	crashQuietLogs(t)
	for _, sh := range crashShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			ref, lines, bgLines := sh.reference(t)
			t.Logf("%s: the converge makes %d calls", sh.name, len(lines))
			if bg := sh.background(); bg != "" {
				t.Logf("%s: %s makes %d calls", sh.name, bg, len(bgLines))
			}
			if len(lines) < sh.minCalls {
				t.Errorf("the converge made %d calls, fewer than the %d "+
					"it makes at least", len(lines), sh.minCalls)
			}
			if len(bgLines) < sh.minBackgroundCalls {
				t.Errorf("%s made %d calls, fewer than the %d it makes "+
					"at least", sh.background(), len(bgLines),
					sh.minBackgroundCalls)
			}
			start := time.Now()
			var tally crashTally
			i := 0
			for k := 1; k <= len(lines); k++ {
				for _, after := range []bool{false, true} {
					i++
					if !crashSampled(i - 1) {
						continue
					}
					cut := crashCut{phase: "the converge", k: k,
						after: after, line: lines[k-1]}
					tally.add(t, cut, sh.cutConverge(t, ref, lines, cut))
				}
			}
			for k := 1; k <= len(bgLines); k++ {
				for _, after := range []bool{false, true} {
					i++
					if !crashSampled(i - 1) {
						continue
					}
					cut := crashCut{phase: sh.background(), k: k,
						after: after, line: bgLines[k-1]}
					tally.add(t, cut, sh.cutBackground(t, ref, bgLines,
						cut))
				}
			}
			tally.report(t, sh.name, start)
		})
	}
}

// reference runs the shape with no cut and returns what it leaves, the
// converge's calls, and the calls of the work it leaves running, in order.
func (sh crashShape) reference(t *testing.T) (crashRef, []string, []string) {
	t.Helper()
	node, clock := newCrashNode()
	inc := startCrashInc(node, clock, sh.fenceWait)
	defer inc.end(t)
	if err := inc.reconcile("startup-0"); err != nil {
		t.Fatalf("startup pass: %v", err)
	}
	sh.setup(t, inc.srv)
	crashForgetReleases(node)
	// Every call under an id the test did not give is the background's,
	// counted in the order it runs.
	inc.gate.arm(crashByUnknownId(inc.gate.seenIds("cut")), 0, false)
	sh.conv(traced("cut"), inc.srv)
	lines := inc.gate.linesUnder("cut")
	var bgLines []string
	switch {
	case sh.fenceWait > 0:
		if !crashSettleFence(inc.srv, node, sh.fenceWait) {
			t.Fatal("the fence window never ended")
		}
		bgLines = inc.gate.counted()
	case sh.zeroing:
		for _, side := range sh.held.sides {
			if !crashWaitZeroed(inc.srv, side) {
				t.Fatal("the zeroing never finished")
			}
		}
		bgLines = inc.gate.counted()
	}
	passes, rows, failure := sh.settle(inc.srv, node, "drive")
	if failure != "" {
		t.Fatalf("with no cut: %s", failure)
	}
	if passes != 1 {
		t.Fatalf("with no cut, the first pass sent something")
	}
	inc.end(t)
	if bad := crashBadReleases(node, sh.source); len(bad) > 0 {
		t.Fatalf("with no cut, a linear released onto the side: %v", bad)
	}
	return crashRef{snap: crashSnapshot(node), rows: rows}, lines, bgLines
}

// cutConverge builds the shape's state on a fresh node, cuts its converge,
// and judges what the next agent makes of it.
func (sh crashShape) cutConverge(
	t *testing.T,
	ref crashRef,
	refLines []string,
	cut crashCut,
) crashOutcome {
	t.Helper()
	node, clock := newCrashNode()
	inc := startCrashInc(node, clock, sh.fenceWait)
	defer inc.end(t)
	if err := inc.reconcile("startup-0"); err != nil {
		t.Fatalf("startup pass: %v", err)
	}
	sh.setup(t, inc.srv)
	crashForgetReleases(node)
	inc.gate.arm(crashById("cut"), cut.k, cut.after)
	sh.conv(traced("cut"), inc.srv)
	lines := inc.gate.counted()
	inc.end(t)
	if failure := crashSameCalls(lines, refLines, cut.k); failure != "" {
		return crashOutcome{failure: failure}
	}
	return sh.restartAndJudge(t, node, clock, inc.gate, ref, refLines,
		crashRan(cut), "startup-1")
}

// cutBackground runs the shape's converge whole, cuts the work it leaves
// running — the fence timer's converge at the end of the window, or the
// zeroing loop — and judges what the next agent makes of it.
func (sh crashShape) cutBackground(
	t *testing.T,
	ref crashRef,
	refLines []string,
	cut crashCut,
) crashOutcome {
	t.Helper()
	node, clock := newCrashNode()
	inc := startCrashInc(node, clock, sh.fenceWait)
	defer inc.end(t)
	if err := inc.reconcile("startup-0"); err != nil {
		t.Fatalf("startup pass: %v", err)
	}
	sh.setup(t, inc.srv)
	crashForgetReleases(node)
	// Armed before the converge: the work may start before it returns.
	inc.gate.arm(crashByUnknownId(inc.gate.seenIds("cut")), cut.k,
		cut.after)
	sh.conv(traced("cut"), inc.srv)
	select {
	case <-inc.gate.cut:
	case <-time.After(5 * time.Second):
		return crashOutcome{failure: sh.background() + " never reached " +
			"the cut"}
	}
	lines := inc.gate.counted()
	inc.end(t)
	if failure := crashSameCalls(lines, refLines, cut.k); failure != "" {
		return crashOutcome{failure: failure}
	}
	return sh.restartAndJudge(t, node, clock, inc.gate, ref, refLines,
		crashRan(cut), "startup-1")
}

// crashRan is how many of the cut work's calls ran.
func crashRan(cut crashCut) int {
	if cut.after {
		return cut.k
	}
	return cut.k - 1
}

// crashSameCalls fails unless the cut work's first k calls are the
// reference's: the walk cuts the call it names.
func crashSameCalls(lines, refLines []string, k int) string {
	if len(lines) < k {
		return fmt.Sprintf("the work made %d calls, short of the cut",
			len(lines))
	}
	for i := range k {
		if lines[i] != refLines[i] {
			return fmt.Sprintf("call %d was %q, not %q as with no cut",
				i+1, lines[i], refLines[i])
		}
	}
	return ""
}

// TestACrashAtEveryStepOfTheStartupPass builds each shape's state whole,
// then ends the next agent at every call of its startup pass, before it and
// after it, and starts a third over the same node: the pass converges every
// held object again (DN2), so a pass cut short is the restart's own
// half-finished work. A restart with no cut must leave the built node as it
// found it, each device under the number it had, and make no call that
// changes the node but the shape's startupChanges: one that removed and
// rebuilt a device it found right would show in the numbers and in its
// calls, and one that suspended, reloaded and resumed it only in its calls.
func TestACrashAtEveryStepOfTheStartupPass(t *testing.T) {
	crashQuietLogs(t)
	for _, sh := range crashShapes() {
		if sh.fenceWait > 0 {
			// Once its window has ended, the same state as the shape with
			// none.
			continue
		}
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			build := func() (*fakeNode, *dnClock) {
				node, clock := newCrashNode()
				inc := startCrashInc(node, clock, 0)
				defer inc.end(t)
				if err := inc.reconcile("startup-0"); err != nil {
					t.Fatalf("startup pass: %v", err)
				}
				sh.setup(t, inc.srv)
				sh.conv(traced("conv"), inc.srv)
				if passes, _, failure := sh.settle(inc.srv, node,
					"build"); failure != "" {
					t.Fatalf("building: %s (%d passes)", failure, passes)
				}
				inc.end(t)
				crashForgetReleases(node)
				return node, clock
			}
			node, clock := build()
			built := crashSnapshot(node)
			builtNos := crashDevNos(node)
			probe := startCrashInc(node, clock, 0)
			defer probe.end(t)
			probe.gate.arm(crashById("startup-1"), 0, false)
			if err := probe.reconcile("startup-1"); err != nil {
				t.Fatalf("startup pass: %v", err)
			}
			lines := probe.gate.counted()
			passes, rows, failure := sh.settle(probe.srv, node, "drive")
			probe.end(t)
			if failure != "" || passes != 1 {
				t.Fatalf("a restart with no cut: %s (%d passes)", failure,
					passes)
			}
			ref := crashRef{snap: crashSnapshot(node), rows: rows}
			if ref.snap != built {
				t.Fatalf("a restart with no cut changed the node:\n%s",
					crashDiff(built, ref.snap))
			}
			if got := crashDevNos(node); got != builtNos {
				t.Fatalf("a restart with no cut rebuilt a device:\n%s",
					crashDiff(builtNos, got))
			}
			if changes := crashNodeChanges(lines); len(changes) !=
				sh.startupChanges {
				t.Fatalf("a restart with no cut made %d calls that change "+
					"the node, want %d (SH16):\n%s", len(changes),
					sh.startupChanges, strings.Join(changes, "\n"))
			}
			t.Logf("%s: the startup pass makes %d calls", sh.name,
				len(lines))
			if len(lines) < sh.minStartupCalls {
				t.Errorf("the startup pass made %d calls, fewer than the "+
					"%d it makes at least", len(lines), sh.minStartupCalls)
			}
			start := time.Now()
			var tally crashTally
			i := 0
			for k := 1; k <= len(lines); k++ {
				for _, after := range []bool{false, true} {
					i++
					if !crashSampled(i - 1) {
						continue
					}
					cut := crashCut{phase: "the startup pass", k: k,
						after: after, line: lines[k-1]}
					node, clock := build()
					inc := startCrashInc(node, clock, 0)
					inc.gate.arm(crashById("startup-1"), k, after)
					// A pass cut at its store listing fails as SH3 says;
					// the process would exit, and this one is ended.
					_ = inc.reconcile("startup-1")
					got := inc.gate.counted()
					inc.end(t)
					out := crashOutcome{failure: crashSameCalls(got, lines,
						k)}
					if out.failure == "" {
						out = sh.restartAndJudge(t, node, clock, inc.gate,
							ref, lines, crashRan(cut), "startup-2")
					}
					tally.add(t, cut, out)
				}
			}
			tally.report(t, sh.name, start)
		})
	}
}
