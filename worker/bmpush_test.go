package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A pusher over recording closures
// ---------------------------------------------------------------------------

// pushRecorder stands in for one agent: it records every delivered chunk, in
// order, and tracks how many deliveries are in flight at once so that BM3's
// "one in flight per migration/clone" can be asserted.
type pushRecorder struct {
	mu       sync.Mutex
	parts    []bmPart
	inFlight map[uint64]int
	maxOne   bool
	peak     int
	// gate, when non-nil, blocks every delivery until it is closed.
	gate chan struct{}
	// entered is signalled once per delivery, before the gate.
	entered chan bmPart
	// reply is the AgentReply code of the next delivery.
	code uint32
	err  error
}

func newPushRecorder() *pushRecorder {
	return &pushRecorder{
		inFlight: make(map[uint64]int),
		maxOne:   true,
		entered:  make(chan bmPart, 32),
	}
}

func (r *pushRecorder) deliver(
	ctx context.Context,
	conn *grpc.ClientConn,
	part bmPart,
) (uint32, string, error) {
	r.mu.Lock()
	r.inFlight[part.resId]++
	if r.inFlight[part.resId] > 1 {
		r.maxOne = false
	}
	total := 0
	for _, n := range r.inFlight {
		total += n
	}
	if total > r.peak {
		r.peak = total
	}
	gate := r.gate
	code, err := r.code, r.err
	r.mu.Unlock()

	select {
	case r.entered <- part:
	default:
	}
	if gate != nil {
		<-gate
	}

	r.mu.Lock()
	r.inFlight[part.resId]--
	if err == nil {
		r.parts = append(r.parts, part)
	}
	r.mu.Unlock()
	if err != nil {
		return 0, "", err
	}
	return code, "rejected", nil
}

func (r *pushRecorder) delivered() []bmPart {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bmPart(nil), r.parts...)
}

func (r *pushRecorder) sequential() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxOne
}

func (r *pushRecorder) peakInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

func (r *pushRecorder) setCode(code uint32) {
	r.mu.Lock()
	r.code = code
	r.mu.Unlock()
}

func (r *pushRecorder) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// bmTestDeps is a deps whose connection cache dials nothing real: the pusher's
// deliver closure is the test's, so the connection is only ever passed along.
func bmTestDeps(t *testing.T) *deps {
	t.Helper()
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	d.conns.dial = func(addrPort string) (*grpc.ClientConn, error) {
		return grpc.NewClient(
			"passthrough:///"+addrPort,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}
	return d
}

// newTestPusher builds a clone pusher over the recorder, with a fetch that
// serves one byte per chunk index.
func newTestPusher(
	t *testing.T,
	d *deps,
	rec *pushRecorder,
) *bmPusher {
	t.Helper()
	p := newBmPusher(bmPusherParams{
		deps:     d,
		seed:     seedOf(4),
		kind:     bmKindClone,
		addrPort: spCnA,
		idAttr:   "clone_id",
		ids:      []slog.Attr{slog.Uint64("cn_id", spCnIdA)},
		fetch: func(
			ctx context.Context, name string, bmIdx uint32,
		) ([]byte, bool, error) {
			return []byte{byte(bmIdx)}, true, nil
		},
		deliver: rec.deliver,
	})
	t.Cleanup(p.stop)
	return p
}

// ---------------------------------------------------------------------------
// BM2 / BM5 — the diff and the mod_revision memo
// ---------------------------------------------------------------------------

// TestBmMissingDiff checks BM2: the chunks etcd holds minus the ones the agent
// acknowledges, ascending by bm_idx; a clone absent from the reply has
// everything missing.
func TestBmMissingDiff(t *testing.T) {
	captureLogs(t)
	p := newTestPusher(t, bmTestDeps(t), newPushRecorder())
	chunks := []model.BmChunk{
		{Idx: 2, ModRev: 12},
		{Idx: 0, ModRev: 10},
		{Idx: 1, ModRev: 11},
	}
	got := p.missing(spCloneId, chunks, nil)
	if len(got) != 3 || got[0].Idx != 0 || got[1].Idx != 1 ||
		got[2].Idx != 2 {
		t.Fatalf("missing = %v, want 0,1,2 ascending", got)
	}
	got = p.missing(spCloneId, chunks, []uint32{0, 2})
	if len(got) != 1 || got[0].Idx != 1 {
		t.Fatalf("missing = %v, want only 1", got)
	}
	got = p.missing(spCloneId, chunks, []uint32{0, 1, 2})
	if len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}

// TestBmGrownChunkMemo checks BM5: a chunk whose etcd mod_revision advanced
// past the one this worker last pushed is re-pushed even though the agent
// acknowledges its index — while a chunk this worker never pushed (a handoff:
// the memo is in-memory) is left alone.
func TestBmGrownChunkMemo(t *testing.T) {
	captureLogs(t)
	rec := newPushRecorder()
	p := newTestPusher(t, bmTestDeps(t), rec)
	chunks := []model.BmChunk{{Idx: 0, ModRev: 10}, {Idx: 1, ModRev: 11}}
	p.submit(&bmPlan{
		resId:    spCloneId,
		name:     spCloneNm,
		revision: 9,
		parts:    p.missing(spCloneId, chunks, nil),
	})
	waitFor(t, "both chunks pushed", func() bool {
		return len(rec.delivered()) == 2
	})

	// Acknowledged and unchanged: nothing to do.
	if got := p.missing(spCloneId, chunks, []uint32{0, 1}); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
	// AppendCloneBitmap grew chunk 1 (BM5).
	grown := []model.BmChunk{{Idx: 0, ModRev: 10}, {Idx: 1, ModRev: 19}}
	got := p.missing(spCloneId, grown, []uint32{0, 1})
	if len(got) != 1 || got[0].Idx != 1 || got[0].ModRev != 19 {
		t.Fatalf("missing = %v, want the grown chunk 1", got)
	}
	// A chunk this worker never pushed stays acknowledged.
	other := []model.BmChunk{{Idx: 3, ModRev: 30}}
	if got := p.missing(spCloneId, other, []uint32{3}); len(got) != 0 {
		t.Fatalf("missing = %v, want none after a handoff", got)
	}
}

// ---------------------------------------------------------------------------
// BM3 — ordering and one in flight
// ---------------------------------------------------------------------------

// TestBmAscendingOneInFlight checks BM3: the parts of one object go out in
// ascending bm_idx, one at a time, each carrying the object's synced revision;
// a plan submitted while one is running replaces the pending one instead of
// starting a second runner.
func TestBmAscendingOneInFlight(t *testing.T) {
	captureLogs(t)
	rec := newPushRecorder()
	rec.gate = make(chan struct{})
	p := newTestPusher(t, bmTestDeps(t), rec)
	parts := []model.BmChunk{
		{Idx: 0, ModRev: 10}, {Idx: 1, ModRev: 11}, {Idx: 2, ModRev: 12},
	}
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, revision: 7, parts: parts,
	})
	// The first delivery is in flight; a second submit must not start a
	// second runner.
	<-rec.entered
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, revision: 7,
		parts: []model.BmChunk{{Idx: 3, ModRev: 13}},
	})
	close(rec.gate)

	waitFor(t, "every part pushed", func() bool {
		return len(rec.delivered()) == 4
	})
	if !rec.sequential() {
		t.Fatalf("two pushes of one clone were in flight at once")
	}
	for i, part := range rec.delivered() {
		if part.bmIdx != uint32(i) {
			t.Fatalf("delivered %v, want ascending bm_idx",
				rec.delivered())
		}
		if part.revision != 7 || part.resId != spCloneId {
			t.Fatalf("part = %+v", part)
		}
		if len(part.bitmap) != 1 || part.bitmap[0] != byte(i) {
			t.Fatalf("part %d carried %v", i, part.bitmap)
		}
	}
}

// TestBmObjectsPushIndependently checks BM3/§9.6 step 4: different clones push
// independently and may be in flight toward one agent at the same time.
func TestBmObjectsPushIndependently(t *testing.T) {
	captureLogs(t)
	rec := newPushRecorder()
	rec.gate = make(chan struct{})
	p := newTestPusher(t, bmTestDeps(t), rec)
	for _, resId := range []uint64{spCloneId, spCloneId + 1} {
		p.submit(&bmPlan{
			resId: resId, name: spCloneNm, revision: 7,
			parts: []model.BmChunk{{Idx: 0, ModRev: 10}},
		})
	}
	<-rec.entered
	<-rec.entered
	if got := rec.peakInFlight(); got != 2 {
		t.Fatalf("peak in flight = %d, want the two clones", got)
	}
	close(rec.gate)
	waitFor(t, "both pushed", func() bool { return len(rec.delivered()) == 2 })
}

// ---------------------------------------------------------------------------
// BM6 — failure
// ---------------------------------------------------------------------------

// TestBmRejectedPushSetsResync checks BM6: a rejected push (code != 0) raises
// the resync flag the child turns into an equal-revision Syncup*, and the rest
// of the plan is abandoned — the next reply's diff decides again.
func TestBmRejectedPushSetsResync(t *testing.T) {
	logs := captureLogs(t)
	rec := newPushRecorder()
	rec.setCode(common.ReplyCodeStaleRevision)
	p := newTestPusher(t, bmTestDeps(t), rec)
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, revision: 7,
		parts: []model.BmChunk{{Idx: 0, ModRev: 10}, {Idx: 1, ModRev: 11}},
	})
	waitFor(t, "the resync flag", func() bool { return p.takeFailed() })
	if got := len(rec.delivered()); got != 1 {
		t.Fatalf("%d parts delivered, want the plan abandoned after the "+
			"rejected one", got)
	}
	// takeFailed clears the flag: one resync per failure (BM6).
	if p.takeFailed() {
		t.Fatalf("the resync flag was not cleared")
	}
	records := logs.withMsg(msgBitmapPushed)
	if len(records) != 1 {
		t.Fatalf("%d bitmap pushed records, want one", len(records))
	}
	if records[0]["kind"] != bmKindClone {
		t.Fatalf("kind = %v", records[0]["kind"])
	}
	if code, _ := records[0]["code"].(float64); uint32(code) !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("code = %v", records[0]["code"])
	}
	if idx, _ := records[0]["bm_idx"].(float64); uint32(idx) != 0 {
		t.Fatalf("bm_idx = %v", records[0]["bm_idx"])
	}
}

// TestBmFailedPushSetsResync checks BM6 for a transport failure: no
// AgentReply, so no "bitmap pushed" record, and the flag is raised all the
// same.
func TestBmFailedPushSetsResync(t *testing.T) {
	logs := captureLogs(t)
	rec := newPushRecorder()
	rec.setErr(errors.New("connection refused"))
	p := newTestPusher(t, bmTestDeps(t), rec)
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, revision: 7,
		parts: []model.BmChunk{{Idx: 0, ModRev: 10}},
	})
	waitFor(t, "the resync flag", func() bool { return p.takeFailed() })
	if got := len(logs.withMsg(msgBitmapPushed)); got != 0 {
		t.Fatalf("%d bitmap pushed records without a reply", got)
	}
	waitFor(t, "the failure record", func() bool {
		return len(logs.withMsg(msgBitmapPushFailed)) > 0
	})
}

// TestBmPushRecordsCarryATraceId checks RW10 for §10: every push runs under
// common.WithTraceId(ctx, seed[:8] + "-" + NewTraceId()), so the §12
// "bitmap pushed" record — and the failure record of a push that never
// reached a chunk — names the worker incarnation that sent it. It is the
// evidence the §14 suite matches trace_id | split("-")[0] against.
func TestBmPushRecordsCarryATraceId(t *testing.T) {
	logs := captureLogs(t)
	rec := newPushRecorder()
	p := newTestPusher(t, bmTestDeps(t), rec)
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, revision: 7,
		parts: []model.BmChunk{{Idx: 0, ModRev: 10}, {Idx: 1, ModRev: 11}},
	})
	waitFor(t, "both chunks pushed", func() bool {
		return len(logs.withMsg(msgBitmapPushed)) == 2
	})
	want := seedPrefix(seedOf(4))
	seen := make(map[string]bool)
	for _, record := range logs.withMsg(msgBitmapPushed) {
		traceId, _ := record["trace_id"].(string)
		if !strings.HasPrefix(traceId, want+"-") || len(traceId) <= len(want)+1 {
			t.Fatalf("trace_id = %q, want %q + \"-\" + a fresh id",
				traceId, want)
		}
		seen[traceId] = true
	}
	if len(seen) != 2 {
		t.Fatalf("%d distinct trace ids for two pushes", len(seen))
	}

	// A push that cannot even take its connection is still a push (RW10).
	d := bmTestDeps(t)
	d.conns.dial = func(addrPort string) (*grpc.ClientConn, error) {
		return nil, errors.New("no route to host")
	}
	failing := newTestPusher(t, d, newPushRecorder())
	failing.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, revision: 7,
		parts: []model.BmChunk{{Idx: 0, ModRev: 10}},
	})
	waitFor(t, "the connect failure record", func() bool {
		return len(logs.withMsg(msgBitmapPushFailed)) > 0
	})
	record := logs.withMsg(msgBitmapPushFailed)[0]
	traceId, _ := record["trace_id"].(string)
	if !strings.HasPrefix(traceId, want+"-") {
		t.Fatalf("connect failure trace_id = %q, want the %q prefix",
			traceId, want)
	}
}

// TestBmMissingChunkSetsResync checks BM6's other abort: a chunk value that
// cannot be read raises the flag too, because a diff only ever comes from a
// Syncup* reply and nothing else would ever retry.
func TestBmMissingChunkSetsResync(t *testing.T) {
	captureLogs(t)
	rec := newPushRecorder()
	d := bmTestDeps(t)
	p := newBmPusher(bmPusherParams{
		deps:     d,
		seed:     seedOf(4),
		kind:     bmKindMigr,
		addrPort: spDnC,
		idAttr:   "migr_id",
		ids:      []slog.Attr{slog.Uint64("dn_id", spDnIdC)},
		fetch: func(
			ctx context.Context, name string, bmIdx uint32,
		) ([]byte, bool, error) {
			return nil, false, nil
		},
		deliver: rec.deliver,
	})
	t.Cleanup(p.stop)
	p.submit(&bmPlan{
		resId: spMigrId, name: spMigrName, revision: 7,
		parts: []model.BmChunk{{Idx: 0, ModRev: 10}},
	})
	waitFor(t, "the resync flag", func() bool { return p.takeFailed() })
	if got := len(rec.delivered()); got != 0 {
		t.Fatalf("%d parts delivered without a chunk value", got)
	}
}

// ---------------------------------------------------------------------------
// BM1 / BM4 — sources and targets, end to end through the sp children
// ---------------------------------------------------------------------------

// TestBmMigrTargetIsDestinationDn checks BM1/BM4: a migration's chunks are
// read from the MigrBitmap keys and pushed only to the DESTINATION side's DN,
// and only when the reply's bm_info is about that migration.
func TestBmMigrTargetIsDestinationDn(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	for idx := uint32(0); idx < 2; idx++ {
		h.store.seed(t,
			model.MigrBitmapKey(testCid, testSpId, spMigrName, idx),
			&pb.MigrBitmap{Bitmap: []byte{byte(idx), 0xff}},
		)
	}
	// The destination agent holds chunk 0 already.
	h.sides[spDnC].syncupReply = func(
		req *pb.SyncupSideRequest,
	) *pb.SyncupSideReply {
		return &pb.SyncupSideReply{
			Revision: req.GetRevision(),
			BmInfo: &pb.BitmapInfo{
				ResId:     spMigrId,
				BmIdxList: []uint32{0},
			},
		}
	}
	// The source agent reports the same set; it must push nothing (BM4).
	h.sides[spDnB].syncupReply = func(
		req *pb.SyncupSideRequest,
	) *pb.SyncupSideReply {
		return &pb.SyncupSideReply{
			Revision: req.GetRevision(),
			BmInfo:   &pb.BitmapInfo{ResId: spMigrId},
		}
	}
	h.start()

	waitFor(t, "the missing chunk pushed", func() bool {
		return len(h.sides[spDnC].pushes()) > 0
	})
	push := h.sides[spDnC].pushes()[0]
	if push.GetMigrId() != spMigrId || push.GetBmIdx() != 1 {
		t.Fatalf("push = %v, want the missing chunk 1", push)
	}
	if push.GetDnId() != spDnIdC ||
		push.GetSidePointer().GetSideId() != spSideDst {
		t.Fatalf("push addressed %v", push)
	}
	if push.GetRevision() != testSpRev {
		t.Fatalf("push revision = %d, want the synced one",
			push.GetRevision())
	}
	if len(push.GetBitmap()) != 2 || push.GetBitmap()[0] != 1 {
		t.Fatalf("push bitmap = %v", push.GetBitmap())
	}
	if got := len(h.sides[spDnB].pushes()); got != 0 {
		t.Fatalf("%d chunks pushed to the SOURCE side's DN", got)
	}
	for _, addr := range []string{spDnA, spDnD} {
		if got := len(h.sides[addr].pushes()); got != 0 {
			t.Fatalf("%d chunks pushed to an unrelated side", got)
		}
	}
}

// TestBmCloneTargetIsPrimaryCn checks BM1/BM4: a clone's chunks go only to the
// CN hosting the PRIMARY cntlr, and a standby's bm_info_list is ignored.
func TestBmCloneTargetIsPrimaryCn(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	for idx := uint32(0); idx < 2; idx++ {
		h.store.seed(t,
			model.CloneBitmapKey(testCid, testSpId, spCloneNm, idx),
			&pb.CloneBitmap{Bitmap: []byte{byte(idx)}},
		)
	}
	// Neither the primary nor the standby holds a chunk: a clone absent from
	// the list has everything missing (BM2).
	empty := func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply {
		return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
	}
	h.cntlrs[spCnA].syncupReply = empty
	h.cntlrs[spCnB].syncupReply = empty
	h.cntlrs[spCnC].syncupReply = empty
	h.start()

	waitFor(t, "both clone chunks pushed", func() bool {
		return len(h.cntlrs[spCnA].pushes()) >= 2
	})
	pushes := h.cntlrs[spCnA].pushes()
	for i, push := range pushes[:2] {
		if push.GetCloneId() != spCloneId || push.GetBmIdx() != uint32(i) {
			t.Fatalf("push %d = %v", i, push)
		}
		if push.GetCnId() != spCnIdA ||
			push.GetCntlrPointer().GetCntlrId() != spCntlrPrimary {
			t.Fatalf("push addressed %v", push)
		}
	}
	for _, addr := range []string{spCnB, spCnC} {
		if got := len(h.cntlrs[addr].pushes()); got != 0 {
			t.Fatalf("%d chunks pushed to a STANDBY's CN", got)
		}
	}
}
