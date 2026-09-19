package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
	// attempts counts every delivery ENTERED, including the ones that fail:
	// "the plan ended" is a statement about what was attempted, and a failed
	// delivery leaves no part behind in parts.
	attempts int
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
	r.attempts++
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

func (r *pushRecorder) attemptCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
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
// serves the chunk's own (src_slice_idx, bm_idx) pair as its two bytes, so a
// delivery that read the wrong key is visible in the value it carried.
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
			ctx context.Context, name string, sliceIdx uint32, bmIdx uint32,
		) ([]byte, bool, error) {
			return []byte{byte(sliceIdx), byte(bmIdx)}, true, nil
		},
		deliver: rec.deliver,
	})
	t.Cleanup(p.stop)
	return p
}

// bmAddrs are the delivered chunks' addresses, for the order assertions.
func bmAddrs(parts []bmPart) [][2]uint32 {
	out := make([][2]uint32, 0, len(parts))
	for _, part := range parts {
		out = append(out, [2]uint32{part.sliceIdx, part.bmIdx})
	}
	return out
}

// ---------------------------------------------------------------------------
// BM2 / BM5 — the diff and the mod_revision memo
// ---------------------------------------------------------------------------

// TestBmMissingDiff checks BM2: the chunks etcd holds minus the ones the agent
// acknowledges, ascending (src_slice_idx, bm_idx); a clone absent from the
// reply has everything missing. The diff is over the PAIR — one bm_idx names a
// different chunk on every source slice (MD2) — so an acknowledged (0, 1) never
// satisfies the etcd chunk (2, 1).
func TestBmMissingDiff(t *testing.T) {
	captureLogs(t)
	p := newTestPusher(t, bmTestDeps(t), newPushRecorder())
	chunks := []model.BmChunk{
		{SliceIdx: 2, Idx: 1, ModRev: 14},
		{SliceIdx: 0, Idx: 1, ModRev: 11},
		{SliceIdx: 1, Idx: 0, ModRev: 12},
		{SliceIdx: 0, Idx: 0, ModRev: 10},
	}
	want := [][2]uint32{{0, 0}, {0, 1}, {1, 0}, {2, 1}}
	got := p.missing(spCloneId, chunks, nil)
	if !equalBmAddrs(chunkAddrs(got), want) {
		t.Fatalf("missing = %v, want %v ascending", chunkAddrs(got), want)
	}
	applied := []model.BmChunk{{SliceIdx: 0, Idx: 0}, {SliceIdx: 0, Idx: 1}}
	got = p.missing(spCloneId, chunks, applied)
	want = [][2]uint32{{1, 0}, {2, 1}}
	if !equalBmAddrs(chunkAddrs(got), want) {
		t.Fatalf("missing = %v, want %v", chunkAddrs(got), want)
	}
	// The whole set acknowledged, pair by pair.
	got = p.missing(spCloneId, chunks, []model.BmChunk{
		{SliceIdx: 0, Idx: 0}, {SliceIdx: 0, Idx: 1},
		{SliceIdx: 1, Idx: 0}, {SliceIdx: 2, Idx: 1},
	})
	if len(got) != 0 {
		t.Fatalf("missing = %v, want none", chunkAddrs(got))
	}
	// A bm_idx-keyed diff would read (0, 1) as covering (2, 1) and never send
	// source slice 2's chunk at all.
	got = p.missing(
		spCloneId,
		[]model.BmChunk{{SliceIdx: 2, Idx: 1, ModRev: 14}},
		[]model.BmChunk{{SliceIdx: 0, Idx: 1}},
	)
	want = [][2]uint32{{2, 1}}
	if !equalBmAddrs(chunkAddrs(got), want) {
		t.Fatalf("missing = %v, want %v: bm_idx 1 on slice 0 is not the "+
			"chunk (2, 1)", chunkAddrs(got), want)
	}
}

// chunkAddrs are the diff's chunk addresses, for the assertions above.
func chunkAddrs(chunks []model.BmChunk) [][2]uint32 {
	out := make([][2]uint32, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, [2]uint32{chunk.SliceIdx, chunk.Idx})
	}
	return out
}

func equalBmAddrs(got [][2]uint32, want [][2]uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i, addr := range got {
		if addr != want[i] {
			return false
		}
	}
	return true
}

// TestBmGrownChunkMemo checks BM5: a chunk whose etcd mod_revision advanced
// past the one this worker last pushed is re-pushed even though the agent
// acknowledges it — while a chunk this worker never pushed (a handoff: the
// memo is in-memory) is left alone. The memo is per (clone_id,
// src_slice_idx, bm_idx), so growth at one pair moves that pair alone and a
// growth is judged against what was pushed AT THAT PAIR.
func TestBmGrownChunkMemo(t *testing.T) {
	captureLogs(t)
	rec := newPushRecorder()
	p := newTestPusher(t, bmTestDeps(t), rec)
	// Two chunks that share bm_idx 1 on different source slices, pushed in
	// that order: the second's mod_revision is the higher one.
	chunks := []model.BmChunk{
		{SliceIdx: 0, Idx: 1, ModRev: 10},
		{SliceIdx: 2, Idx: 1, ModRev: 20},
	}
	p.submit(&bmPlan{
		resId: spCloneId,
		name:  spCloneNm,
		parts: p.missing(spCloneId, chunks, nil),
	})
	waitFor(t, "both chunks pushed", func() bool {
		return len(rec.delivered()) == 2
	})
	applied := []model.BmChunk{{SliceIdx: 0, Idx: 1}, {SliceIdx: 2, Idx: 1}}

	// Acknowledged and unchanged: nothing to do.
	if got := p.missing(spCloneId, chunks, applied); len(got) != 0 {
		t.Fatalf("missing = %v, want none", chunkAddrs(got))
	}
	// AppendCloneBitmap grew the chunk at (2, 1) (BM5): that pair alone
	// is re-pushed, and (0, 1) — the same bm_idx on another slice — is not.
	grown := []model.BmChunk{
		{SliceIdx: 0, Idx: 1, ModRev: 10},
		{SliceIdx: 2, Idx: 1, ModRev: 21},
	}
	got := p.missing(spCloneId, grown, applied)
	if len(got) != 1 || got[0].SliceIdx != 2 || got[0].Idx != 1 ||
		got[0].ModRev != 21 {
		t.Fatalf("missing = %v, want only the grown (2, 1)", chunkAddrs(got))
	}
	// The other direction: (0, 1) grows to a revision still BELOW the one
	// pushed at (2, 1). A memo keyed on bm_idx alone would compare it with
	// (2, 1)'s 20 and drop a growth the agent never received.
	grown = []model.BmChunk{
		{SliceIdx: 0, Idx: 1, ModRev: 15},
		{SliceIdx: 2, Idx: 1, ModRev: 20},
	}
	got = p.missing(spCloneId, grown, applied)
	if len(got) != 1 || got[0].SliceIdx != 0 || got[0].Idx != 1 ||
		got[0].ModRev != 15 {
		t.Fatalf("missing = %v, want only the grown (0, 1)", chunkAddrs(got))
	}
	// A chunk this worker never pushed stays acknowledged.
	other := []model.BmChunk{{SliceIdx: 1, Idx: 3, ModRev: 30}}
	got = p.missing(spCloneId, other, []model.BmChunk{{SliceIdx: 1, Idx: 3}})
	if len(got) != 0 {
		t.Fatalf("missing = %v, want none after a handoff", chunkAddrs(got))
	}
}

// ---------------------------------------------------------------------------
// BM3 — ordering and one in flight
// ---------------------------------------------------------------------------

// TestBmAscendingOneInFlight checks BM3: the parts of one object go out in
// ascending (src_slice_idx, bm_idx) — lexicographic ACROSS source slices, not
// by bm_idx alone — one at a time; a plan submitted while one is running
// replaces the pending one instead of starting a second runner.
func TestBmAscendingOneInFlight(t *testing.T) {
	captureLogs(t)
	rec := newPushRecorder()
	rec.gate = make(chan struct{})
	p := newTestPusher(t, bmTestDeps(t), rec)
	parts := p.missing(spCloneId, []model.BmChunk{
		{SliceIdx: 1, Idx: 0, ModRev: 12},
		{SliceIdx: 0, Idx: 1, ModRev: 11},
		{SliceIdx: 0, Idx: 0, ModRev: 10},
	}, nil)
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm, parts: parts,
	})
	// The first delivery is in flight; a second submit must not start a
	// second runner.
	<-rec.entered
	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm,
		parts: []model.BmChunk{{SliceIdx: 2, Idx: 0, ModRev: 13}},
	})
	close(rec.gate)

	waitFor(t, "every part pushed", func() bool {
		return len(rec.delivered()) == 4
	})
	if !rec.sequential() {
		t.Fatalf("two pushes of one clone were in flight at once")
	}
	want := [][2]uint32{{0, 0}, {0, 1}, {1, 0}, {2, 0}}
	if !equalBmAddrs(bmAddrs(rec.delivered()), want) {
		t.Fatalf("delivered %v, want %v", bmAddrs(rec.delivered()), want)
	}
	for i, part := range rec.delivered() {
		if part.resId != spCloneId {
			t.Fatalf("part = %+v", part)
		}
		// The fetch stub serves the pair it was asked for (BM1): a part whose
		// value does not carry its own address was read at the wrong key.
		if len(part.bitmap) != 2 || part.bitmap[0] != byte(want[i][0]) ||
			part.bitmap[1] != byte(want[i][1]) {
			t.Fatalf("part %v carried %v", want[i], part.bitmap)
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
			resId: resId, name: spCloneNm,
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
// [D13] — a failed push is logged, and nothing else
// ---------------------------------------------------------------------------

// TestPushFailureIsLoggedOnly pins what replaced BM6: a push that fails — for
// any of the three reasons there are — is LOGGED, ends the rest of that plan,
// and arms nothing. There is no flag left to raise: the diff a push comes from
// is computed from the agent's own acknowledged set in the next Syncup* reply,
// so a chunk that did not land is simply missing again next round and is
// re-planned there. BM6's extra equal-revision re-sync only hastened that by
// one round and cost a full re-apply of the object to do it.
//
// The sub-cases are the three failure paths of pushOne, plus the end-to-end
// negative: nothing the push path does makes the child issue a Syncup*.
func TestPushFailureIsLoggedOnly(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		logs := captureLogs(t)
		rec := newPushRecorder()
		rec.setErr(errors.New("connection refused"))
		p := newTestPusher(t, bmTestDeps(t), rec)
		p.submit(&bmPlan{
			resId: spCloneId, name: spCloneNm,
			parts: []model.BmChunk{
				{SliceIdx: 2, Idx: 0, ModRev: 10},
				{SliceIdx: 2, Idx: 1, ModRev: 11},
			},
		})
		waitFor(t, "the failure record", func() bool {
			return len(logs.withMsg(msgBitmapPushFailed)) > 0
		})
		// No AgentReply, so no "bitmap pushed" record to carry a code.
		if got := len(logs.withMsg(msgBitmapPushed)); got != 0 {
			t.Fatalf("%d bitmap pushed records without a reply", got)
		}
		rec0 := logs.withMsg(msgBitmapPushFailed)[0]
		if rec0["error"] != "connection refused" {
			t.Fatalf("error = %v, want the transport error", rec0["error"])
		}
		if rec0["kind"] != bmKindClone {
			t.Fatalf("kind = %v", rec0["kind"])
		}
		if got := rec.attemptCount(); got != 1 {
			t.Fatalf("%d deliveries attempted, want the plan to end after "+
				"the failed one", got)
		}
	})

	t.Run("chunk not found", func(t *testing.T) {
		logs := captureLogs(t)
		rec := newPushRecorder()
		p := newBmPusher(bmPusherParams{
			deps:     bmTestDeps(t),
			seed:     seedOf(4),
			kind:     bmKindMigr,
			addrPort: spDnC,
			idAttr:   "migr_id",
			ids:      []slog.Attr{slog.Uint64("dn_id", spDnIdC)},
			fetch: func(
				ctx context.Context, name string, sliceIdx uint32, bmIdx uint32,
			) ([]byte, bool, error) {
				return nil, false, nil
			},
			deliver: rec.deliver,
		})
		t.Cleanup(p.stop)
		p.submit(&bmPlan{
			resId: spMigrId, name: spMigrName,
			parts: []model.BmChunk{
				{Idx: 0, ModRev: 10}, {Idx: 1, ModRev: 11},
			},
		})
		waitFor(t, "the failure record", func() bool {
			return len(logs.withMsg(msgBitmapPushFailed)) > 0
		})
		if got := rec.attemptCount(); got != 0 {
			t.Fatalf("%d deliveries attempted without a chunk value", got)
		}
		rec0 := logs.withMsg(msgBitmapPushFailed)[0]
		if rec0["error"] != "chunk not found" {
			t.Fatalf("error = %v, want \"chunk not found\"", rec0["error"])
		}
		if rec0["migr_id"] == nil {
			t.Fatalf("the record does not name the migration: %v", rec0)
		}
	})

	t.Run("non-zero reply code", func(t *testing.T) {
		logs := captureLogs(t)
		rec := newPushRecorder()
		rec.setCode(common.ReplyCodeUnknownObject)
		p := newTestPusher(t, bmTestDeps(t), rec)
		p.submit(&bmPlan{
			resId: spCloneId, name: spCloneNm,
			parts: []model.BmChunk{
				{SliceIdx: 2, Idx: 0, ModRev: 10},
				{SliceIdx: 2, Idx: 1, ModRev: 11},
			},
		})
		waitFor(t, "the failure record", func() bool {
			return len(logs.withMsg(msgBitmapPushFailed)) > 0
		})
		if got := rec.attemptCount(); got != 1 {
			t.Fatalf("%d deliveries attempted, want the plan to end after "+
				"the refused one", got)
		}
		// The §12 "bitmap pushed" record carries the code and the chunk's
		// whole address (BM3); the failure record next to it carries the
		// AGENT's own explanation, which is the only thing that says WHY.
		pushed := logs.withMsg(msgBitmapPushed)
		if len(pushed) != 1 {
			t.Fatalf("%d bitmap pushed records, want one", len(pushed))
		}
		if code, _ := pushed[0]["code"].(float64); uint32(code) !=
			common.ReplyCodeUnknownObject {
			t.Fatalf("code = %v", pushed[0]["code"])
		}
		if idx, _ := pushed[0]["src_slice_idx"].(float64); uint32(idx) != 2 {
			t.Fatalf("src_slice_idx = %v", pushed[0]["src_slice_idx"])
		}
		if idx, _ := pushed[0]["bm_idx"].(float64); uint32(idx) != 0 {
			t.Fatalf("bm_idx = %v", pushed[0]["bm_idx"])
		}
		failed := logs.withMsg(msgBitmapPushFailed)[0]
		if failed["details"] != "rejected" {
			t.Fatalf("details = %v, want the agent's own explanation",
				failed["details"])
		}
		if code, _ := failed["code"].(float64); uint32(code) !=
			common.ReplyCodeUnknownObject {
			t.Fatalf("failure code = %v", failed["code"])
		}
	})

	// The end-to-end negative, through a real cntlr child: a refused push must
	// not make the child re-sync. The agent is one revision behind for its
	// FIRST round only, so exactly one SyncupCntlr is owed — the one that
	// plans the push — and every later round matches. The desired state never
	// changes here either (RW3/RW6 are the only other source of a Syncup*), so
	// a second one could only be something the push path re-armed.
	t.Run("no re-sync follows", func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		for _, chunk := range spCloneChunks {
			h.store.seed(t,
				model.CloneBitmapKey(
					testCid, testSpId, spCloneNm, chunk.SliceIdx, chunk.Idx,
				),
				&pb.CloneBitmap{Bitmap: []byte{1}},
			)
		}
		var rounds atomic.Int64
		h.cntlrs[spCnA].checkReply = func(
			req *pb.CheckCntlrRequest,
		) *pb.CheckCntlrReply {
			if rounds.Add(1) == 1 {
				return &pb.CheckCntlrReply{Revision: req.GetRevision() - 1}
			}
			return &pb.CheckCntlrReply{Revision: req.GetRevision()}
		}
		// No bm_info_list: every chunk of the fixture clone is missing (BM2).
		h.cntlrs[spCnA].syncupReply = func(
			req *pb.SyncupCntlrRequest,
		) *pb.SyncupCntlrReply {
			return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
		}
		h.cntlrs[spCnA].pushCode = common.ReplyCodeUnknownObject
		h.start()

		waitFor(t, "the refused push", func() bool {
			return len(h.cntlrs[spCnA].pushes()) > 0
		})
		waitFor(t, "the failure record", func() bool {
			return len(h.logs.withMsg(msgBitmapPushFailed)) > 0
		})
		h.advanceUntil("three more check rounds", roundInterval, func() bool {
			return rounds.Load() >= 4
		})
		if got := len(h.cntlrs[spCnA].syncups()); got != 1 {
			t.Fatalf("%d SyncupCntlr calls, want only the one that planned "+
				"the push: a refused push re-arms nothing", got)
		}
		if got := len(h.cntlrs[spCnA].pushes()); got != 1 {
			t.Fatalf("%d pushes, want the rest of the plan abandoned", got)
		}
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
		resId: spCloneId, name: spCloneNm,
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
		resId: spCloneId, name: spCloneNm,
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
	for _, chunk := range spCloneChunks {
		h.store.seed(t,
			model.CloneBitmapKey(
				testCid, testSpId, spCloneNm, chunk.SliceIdx, chunk.Idx,
			),
			&pb.CloneBitmap{Bitmap: []byte{
				byte(chunk.SliceIdx), byte(chunk.Idx),
			}},
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
		chunk := spCloneChunks[i]
		if push.GetCloneId() != spCloneId ||
			push.GetSrcSliceIdx() != chunk.SliceIdx ||
			push.GetBmIdx() != chunk.Idx {
			t.Fatalf("push %d = %v, want the chunk (%d, %d)",
				i, push, chunk.SliceIdx, chunk.Idx)
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
