package cnagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// §6.15 — the CN11 leg prober. The loop itself is time-driven; the tests drive
// one round at a time under a fake clock, which is what makes the single-flight
// and stall rules observable.

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func withClock(srv *CnAgentServer) *fakeClock {
	clock := &fakeClock{now: time.Unix(1<<30, 0)}
	srv.now = clock.Now
	return clock
}

// logCapture reads back the records the cn agent emits for itself — the
// prober is now the only cn code that logs its own block IO, so the msgs and
// attrs have to be pinned here (log.md §5.1), and the §7 conf refusal is
// pinned the same way in conf_test.go. The buffer is mutex-guarded
// because slog.Default is process-wide and other goroutines may be logging.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// msgRecords decodes every captured JSON line carrying one of the given msgs;
// every other record is ignored, so an unrelated goroutine cannot fail these
// tests.
func (c *logCapture) msgRecords(
	t *testing.T,
	msgs ...string,
) []map[string]any {
	t.Helper()
	c.mu.Lock()
	text := c.buf.String()
	c.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		rec := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %q: %v", line, err)
		}
		for _, msg := range msgs {
			if rec["msg"] == msg {
				out = append(out, rec)
				break
			}
		}
	}
	return out
}

// records is msgRecords over the two probe msgs.
func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	return c.msgRecords(t, "probe write block", "probe read block direct")
}

func (c *logCapture) onlyMsg(t *testing.T, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, rec := range c.records(t) {
		if rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %q record, got %d", msg, len(found))
	}
	return found[0]
}

// captureLogs installs a capturing logger as the process default for the
// duration of the test, using the production handler chain (log.md R2) so the
// trace id lands on the record exactly as it does in the agent.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	logger := slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(capture, nil),
	})
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return capture
}

// legState reaches the prober registry of one converged cntlr.
func legState(t *testing.T, srv *CnAgentServer) *cntlrState {
	t.Helper()
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		t.Fatalf("the cntlr has no state")
	}
	return st
}

func TestLegProberWritesTheHealthBlock(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	st := legState(t, srv)
	prober := st.probers[testDataLeg]
	if prober == nil {
		t.Fatalf("the primary started no prober for the data leg")
	}
	// §3.6: the health block is the last 4 KiB of the meta region, written
	// through the leg wrapper.
	wantOffset := 1*testBlockSize - common.LegHealthBlockSize
	if prober.offset != wantOffset {
		t.Fatalf("probe offset is %d, want %d", prober.offset, wantOffset)
	}

	node.Reset()
	srv.runLegProbe(context.Background(), prober, testCn)
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d len=%d",
			srv.nf.DmPath(legName(srv, testDataLeg)), wantOffset,
			common.LegHealthBlockSize),
		fmt.Sprintf("readblockdirect %s off=%d len=%d",
			srv.nf.DmPath(legName(srv, testDataLeg)), wantOffset,
			common.LegHealthBlockSize),
	)
	// CN11/acceptance 5: the read-back is O_DIRECT, never a buffered read.
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "readblock ") {
			t.Fatalf("the probe used a buffered read: %q", call)
		}
	}

	status, details := srv.legProbeOutcome(st, planLeg(t, srv, testDataLeg))
	if status != pb.ResStatus_RES_STATUS_OK || details != "" {
		t.Fatalf("a completed probe reports %v/%q", status, details)
	}
}

func planLeg(t *testing.T, srv *CnAgentServer, legId uint64) *legPlan {
	t.Helper()
	st := legState(t, srv)
	plan := newCntlrPlan(srv.nf, st.req)
	lp := plan.legById[legId]
	if lp == nil {
		t.Fatalf("no leg %d in the plan", legId)
	}
	return lp
}

func TestLegProberPendingAndStalled(t *testing.T) {
	srv, _ := newTestServer(t)
	clock := withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	st := legState(t, srv)
	lp := planLeg(t, srv, testDataLeg)
	prober := st.probers[testDataLeg]

	// Before any completion the leg is OK/"pending": the wrapper exists and
	// nothing has failed yet.
	status, details := srv.legProbeOutcome(st, lp)
	if status != pb.ResStatus_RES_STATUS_OK ||
		details != detailsProbePending {
		t.Fatalf("a fresh prober reports %v/%q", status, details)
	}

	// An attempt in flight past CnLegProbeStallSeconds is an error — the
	// case a blocked leg produces, where no completion will ever arrive.
	prober.begin(clock.Now())
	clock.advance(common.CnLegProbeStallSeconds * time.Second)
	if status, _ = srv.legProbeOutcome(st, lp); status !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("an in-flight probe inside the bound reports %v", status)
	}
	clock.advance(time.Second)
	status, details = srv.legProbeOutcome(st, lp)
	if status != pb.ResStatus_RES_STATUS_ERROR ||
		details != detailsProbeStalled {
		t.Fatalf("a stalled probe reports %v/%q", status, details)
	}

	// A completed failure carries the IO error.
	prober.finish(fmt.Errorf("input/output error"))
	status, details = srv.legProbeOutcome(st, lp)
	if status != pb.ResStatus_RES_STATUS_ERROR ||
		details != "input/output error" {
		t.Fatalf("a failed probe reports %v/%q", status, details)
	}
}

func TestStandbyStartsNoProber(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: false})
	st := legState(t, srv)
	if len(st.probers) != 0 {
		t.Fatalf("a standby started %d probers", len(st.probers))
	}
	// A standby reports transport liveness and ana_state instead.
	reply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	info := reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]
	assertOk(t, info, "leg")
	if !strings.Contains(info.GetDetails(), "live/") {
		t.Fatalf("standby leg details %q carry no transport state",
			info.GetDetails())
	}
	// And no block IO ever touched the leg.
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock ") ||
			strings.HasPrefix(call, "readblockdirect ") {
			t.Fatalf("a standby ran the block probe: %q", call)
		}
	}
}

// ---------------------------------------------------------------------------
// The standby's ana_state verdict (CN11)
// ---------------------------------------------------------------------------

// TestTransportHealthAnaState pins CN11 on the pure function: a leg with exactly
// one desired side fails its row when that side's path is live but
// unpromotable. `non-optimized` is the standby's healthy steady state and not
// a fault — the DN grants the optimized group to the primary CN alone, so
// every other CN's path sits in the non-optimized group over a dm-error
// backing, which is exactly the shape the redund lab case pins OK (leg rows
// OK, integtest/cnagent_test.sh:1742-1743; ana `non-optimized`, :1774-1779).
// `optimized` is the post-flip pre-promote window and passes too. A leg
// carrying two sides is mid-migration (CN10) and exempt.
func TestTransportHealthAnaState(t *testing.T) {
	oneSide := []*pb.Side{sideOf(testDataSide, testIp, testSvcId)}
	twoSides := []*pb.Side{
		sideOf(testDataSide, testIp, testSvcId),
		sideOf(testDataSide+0x100, testIp2, testSvcId2),
	}
	// name is what `nvme disconnect --device` takes; the report never reads
	// it, so any live-looking value does.
	ctrl := func(name, addr, svcId, state, ana string) ctrlView {
		return ctrlView{name: name, trAddr: addr, trSvcId: svcId,
			state: state, anaState: ana}
	}
	for _, tc := range []struct {
		name    string
		sides   []*pb.Side
		ctrls   []ctrlView
		want    pb.ResStatus
		details string
	}{
		{
			name:  "single-sided optimized",
			sides: oneSide,
			ctrls: []ctrlView{ctrl("nvme0", testIp, testSvcId, "live",
				agent.AnaStateOptimized)},
			want:    pb.ResStatus_RES_STATUS_OK,
			details: "live/optimized",
		},
		{
			name:  "single-sided non-optimized",
			sides: oneSide,
			ctrls: []ctrlView{ctrl("nvme0", testIp, testSvcId, "live",
				agent.AnaStateNonOptimized)},
			want:    pb.ResStatus_RES_STATUS_OK,
			details: "live/non-optimized",
		},
		{
			name:  "single-sided inaccessible",
			sides: oneSide,
			ctrls: []ctrlView{ctrl("nvme0", testIp, testSvcId, "live",
				agent.AnaStateInaccessible)},
			want:    pb.ResStatus_RES_STATUS_ERROR,
			details: "live/inaccessible unpromotable",
		},
		{
			// A state the DN never sets is unpromotable all the same.
			name:  "single-sided change",
			sides: oneSide,
			ctrls: []ctrlView{ctrl("nvme0", testIp, testSvcId, "live",
				"change")},
			want:    pb.ResStatus_RES_STATUS_ERROR,
			details: "live/change unpromotable",
		},
		{
			// CN10: the dst side is inaccessible until the cutover, and only
			// the DN knows that — the row keeps the live-only check.
			name:  "two-sided migration",
			sides: twoSides,
			ctrls: []ctrlView{
				ctrl("nvme0", testIp, testSvcId, "live",
					agent.AnaStateOptimized),
				ctrl("nvme1", testIp2, testSvcId2, "live",
					agent.AnaStateInaccessible),
			},
			want:    pb.ResStatus_RES_STATUS_OK,
			details: "live/inaccessible",
		},
		{
			name:  "not live",
			sides: oneSide,
			ctrls: []ctrlView{ctrl("nvme0", testIp, testSvcId, "connecting",
				agent.AnaStateOptimized)},
			want:    pb.ResStatus_RES_STATUS_ERROR,
			details: "connecting/optimized",
		},
		{
			name:    "missing controller",
			sides:   oneSide,
			ctrls:   nil,
			want:    pb.ResStatus_RES_STATUS_ERROR,
			details: testIp + ":" + testSvcId + " missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lp := &legPlan{legId: testDataLeg, nqn: testNqn, sides: tc.sides}
			view := &subsysView{found: true, ctrls: tc.ctrls}
			status, details := transportHealth(lp, view)
			if status != tc.want {
				t.Fatalf("the leg row is %v/%q, want %v",
					status, details, tc.want)
			}
			if !strings.Contains(details, tc.details) {
				t.Fatalf("the leg row details %q carry no %q",
					details, tc.details)
			}
			if status == pb.ResStatus_RES_STATUS_OK &&
				strings.Contains(details, "unpromotable") {
				t.Fatalf("an OK row calls a path unpromotable: %q", details)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The LegProbeIO dependency (osclient.md §4.5.1)
// ---------------------------------------------------------------------------

// TestFakeProbeIoDefaultsAndOverrides pins the double's FakeOsClient-style
// contract, the way common's TestFakeReadBlockDirect used to pin the OsClient
// fake's: unset halves succeed with zero values, set halves dispatch.
func TestFakeProbeIoDefaultsAndOverrides(t *testing.T) {
	var io LegProbeIO = &fakeProbeIO{}
	if err := io.Write(context.Background(), "/dev/mapper/x", 0,
		make([]byte, common.LegHealthBlockSize)); err != nil {
		t.Fatalf("an unset write half failed: %v", err)
	}
	data, err := io.ReadDirect(context.Background(), "/dev/mapper/x", 0,
		common.LegHealthBlockSize)
	if err != nil || data != nil {
		t.Fatalf("an unset read half returned (%v, %v)", data, err)
	}

	var gotPath string
	var gotOffset, gotLength uint64
	io = &fakeProbeIO{
		WriteFn: func(
			ctx context.Context, path string, offset uint64, data []byte,
		) error {
			gotPath, gotOffset = path, offset
			return fmt.Errorf("input/output error")
		},
		ReadDirectFn: func(
			ctx context.Context, path string, offset, length uint64,
		) ([]byte, error) {
			gotLength = length
			return []byte{1}, nil
		},
	}
	if err := io.Write(context.Background(), "/dev/mapper/y", 4096,
		nil); err == nil || err.Error() != "input/output error" {
		t.Fatalf("the write half did not dispatch: %v", err)
	}
	if gotPath != "/dev/mapper/y" || gotOffset != 4096 {
		t.Fatalf("the write half saw %s off=%d", gotPath, gotOffset)
	}
	if data, err = io.ReadDirect(context.Background(), "/dev/mapper/y", 0,
		common.LegHealthBlockSize); err != nil || len(data) != 1 {
		t.Fatalf("the read half returned (%v, %v)", data, err)
	}
	if gotLength != common.LegHealthBlockSize {
		t.Fatalf("the read half saw length %d", gotLength)
	}
}

// TestLegProbeUsesTheProbeIo: the round drives the LegProbeIO dependency and
// nothing else, and the outcome is whatever that dependency reported.
//
// The double is installed **before** the converge that starts the probers:
// `srv.probeIO` is read by every live prober goroutine, so assigning it after
// syncupBoth is an unsynchronised write to a field two goroutines are reading.
// The counters are mutex-guarded for the same reason — the
// leg's own loop may fire a round of its own on the fake clock.
func TestLegProbeUsesTheProbeIo(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	var mu sync.Mutex
	var wrote, read bool
	srv.probeIO = &fakeProbeIO{
		WriteFn: func(
			ctx context.Context, path string, offset uint64, data []byte,
		) error {
			mu.Lock()
			defer mu.Unlock()
			wrote = true
			return nil
		},
		ReadDirectFn: func(
			ctx context.Context, path string, offset, length uint64,
		) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			read = true
			return nil, fmt.Errorf("input/output error")
		},
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	st := legState(t, srv)
	prober := st.probers[testDataLeg]

	node.Reset()
	srv.runLegProbe(context.Background(), prober, testCn)
	mu.Lock()
	droveWrite, droveRead := wrote, read
	mu.Unlock()
	if !droveWrite || !droveRead {
		t.Fatalf("the round drove write=%v read=%v", droveWrite, droveRead)
	}
	// The OsClient is out of the path entirely (§7 acceptance 3).
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock ") ||
			strings.HasPrefix(call, "readblockdirect ") {
			t.Fatalf("the round went through the OsClient: %q", call)
		}
	}
	status, details := srv.legProbeOutcome(st, planLeg(t, srv, testDataLeg))
	if status != pb.ResStatus_RES_STATUS_ERROR ||
		details != "input/output error" {
		t.Fatalf("a failed read half reports %v/%q", status, details)
	}
}

// TestWedgedProbeDoesNotBlockAConverge is the central carve-out property (§6 test
// 15): a probe that never returns must not delay anything else on the node.
// A leg with no serving path queues IO forever (ctrl_loss_tmo = -1), so this
// is the ordinary failure mode, not an exotic one — and the design's answer is
// that a hung probe is harmless: its IO does not go through the process's
// DefaultOsClientLimit semaphore (osclient.md §4.5.1), and CN1 forbids holding
// any lock across it. This test pins the second half, which is the one a unit
// test can observe: a full SyncupCntlr — leg wrappers, arrays, pools, nvmet —
// completes while a probe of the same server's own leg is parked mid-write.
func TestWedgedProbeDoesNotBlockAConverge(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// Installed before the converge that starts the probers: srv.probeIO is
	// read by every prober goroutine.
	srv.probeIO = &fakeProbeIO{
		WriteFn: func(
			ctx context.Context, path string, offset uint64, data []byte,
		) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		},
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	st := legState(t, srv)
	prober := st.probers[testDataLeg]
	if prober == nil {
		t.Fatalf("the primary started no prober for the data leg")
	}

	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		srv.runLegProbe(context.Background(), prober, testCn)
	}()
	<-entered

	node.Reset()
	var syncErr error
	converged := make(chan struct{})
	go func() {
		defer close(converged)
		_, syncErr = srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: 3, primary: true}))
	}()
	select {
	case <-converged:
	case <-time.After(10 * time.Second):
		close(release)
		<-probeDone
		t.Fatalf("a converge waited on a wedged probe:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if syncErr != nil {
		close(release)
		<-probeDone
		t.Fatalf("SyncupCntlr: %v", syncErr)
	}
	// The converge really did drive the node while the probe was parked, and
	// the probe really was still parked: it never completed a round.
	if !node.hasCall("cmd dmsetup") {
		t.Fatalf("the converge issued no OS command:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if _, completed, _ := prober.snapshot(); completed {
		t.Fatalf("the probe completed instead of staying parked")
	}

	close(release)
	<-probeDone
	if _, completed, _ := prober.snapshot(); !completed {
		t.Fatalf("the released probe never completed")
	}
}

// TestDirectProbeIoLogsOneRecordPerHalf pins CN11's own log records: exactly
// one per half, msg `probe write block` / `probe read block direct`, carrying
// the attempt's trace id and the shape of the IO — never the data.
func TestDirectProbeIoLogsOneRecordPerHalf(t *testing.T) {
	capture := captureLogs(t)
	path := filepath.Join(t.TempDir(), "leg-wrapper")
	if err := os.WriteFile(path, make([]byte, common.LegHealthBlockSize),
		0o600); err != nil {
		t.Fatalf("stage the device stand-in: %v", err)
	}
	ctx := common.WithTraceId(context.Background(), "a1b2c3d4e5f60718")
	var io LegProbeIO = directLegProbeIO{}

	if err := io.Write(ctx, path, 0,
		healthBlockPayload(testCn, 1)); err != nil {
		t.Fatalf("write the health block: %v", err)
	}
	rec := capture.onlyMsg(t, "probe write block")
	if rec["path"] != path || rec["offset"] != float64(0) ||
		rec["length"] != float64(common.LegHealthBlockSize) {
		t.Fatalf("the write record is %v", rec)
	}
	if _, ok := rec["error"]; ok {
		t.Fatalf("a successful write logged an error: %v", rec)
	}
	if rec[common.TraceIdLogKey] != "a1b2c3d4e5f60718" {
		t.Fatalf("the write record carries no attempt trace id: %v", rec)
	}

	// A missing device fails both halves; the read half is asserted there
	// because O_DIRECT is not supported on every filesystem a test may run on.
	missing := filepath.Join(t.TempDir(), "no-such-device")
	if _, err := io.ReadDirect(ctx, missing, 0,
		common.LegHealthBlockSize); err == nil {
		t.Fatalf("reading a missing device succeeded")
	}
	rec = capture.onlyMsg(t, "probe read block direct")
	if rec["path"] != missing || rec["offset"] != float64(0) ||
		rec["length"] != float64(common.LegHealthBlockSize) {
		t.Fatalf("the read record is %v", rec)
	}
	if _, ok := rec["error"].(string); !ok {
		t.Fatalf("a failed read logged no error: %v", rec)
	}
	// The health block is device content: its shape is logged, never its bytes.
	for _, rec := range capture.records(t) {
		if _, ok := rec["data"]; ok {
			t.Fatalf("a probe record carries the data: %v", rec)
		}
	}
}

// TestDirectProbeIoShortCircuitsOnCancel: a cancelled prober starts no IO and
// logs nothing — the fast fail the OsClient's semaphore acquire used to give.
func TestDirectProbeIoShortCircuitsOnCancel(t *testing.T) {
	capture := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var io LegProbeIO = directLegProbeIO{}

	// The path exists, so only the cancellation can stop the IO.
	path := filepath.Join(t.TempDir(), "leg-wrapper")
	if err := os.WriteFile(path, make([]byte, common.LegHealthBlockSize),
		0o600); err != nil {
		t.Fatalf("stage the device stand-in: %v", err)
	}
	if err := io.Write(ctx, path, 0,
		healthBlockPayload(testCn, 1)); err != context.Canceled {
		t.Fatalf("a cancelled write returned %v", err)
	}
	if _, err := io.ReadDirect(ctx, path, 0,
		common.LegHealthBlockSize); err != context.Canceled {
		t.Fatalf("a cancelled read returned %v", err)
	}
	if recs := capture.records(t); len(recs) != 0 {
		t.Fatalf("an operation that never happened logged %d records: %v",
			len(recs), recs)
	}
}

// TestProbersStopOnDemotion: a primary→standby flip cancels every prober, and
// a re-promotion starts them again.
func TestProbersStopOnDemotion(t *testing.T) {
	srv, _ := newTestServer(t)
	withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	st := legState(t, srv)
	if len(st.probers) != 2 {
		t.Fatalf("want 2 probers, got %d", len(st.probers))
	}
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: false})); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if len(st.probers) != 0 {
		t.Fatalf("demotion left %d probers", len(st.probers))
	}
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4, primary: true})); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(st.probers) != 2 {
		t.Fatalf("re-promotion left %d probers", len(st.probers))
	}
}

// TestLegUnavailableWhenNotOptimized is the §11.1.1 availability test: a path
// that is live but `non-optimized` means the side exports dm-error, so the leg
// is not available and the array is not built from it.
func TestLegUnavailableWhenNotOptimized(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	node.anaOf[srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg, testCn)+
		"|"+testIp+":"+testSvcId] = "non-optimized"
	node.anaOf[srv.nf.SideToCnNqn(testCluster, testSp, testMetaLeg, testCn)+
		"|"+testIp+":"+testSvcId] = "non-optimized"

	reply := syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true})
	assertNoCall(t, node, "cmd mdadm --create")
	info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("a group with no available leg reports %v", info.GetStatus())
	}
	// SH15: the whole walk this test drives — subsysnqn, nsid,
	// address, state, ana_state — runs under the SH15 soft timeout. Reverting
	// the agent.CmdCtx in leg.go's readSysfs fails here.
	assertSysfsDeadlines(t, node)
}

// TestDeadPathDisconnectedByDevice is CN10: a controller of the leg NQN whose
// transport matches no desired side is dropped by device, never by NQN — the
// surviving side shares that NQN ([D1]).
func TestDeadPathDisconnectedByDevice(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	// A migrating leg: two sides on one NQN.
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, extraSide: true})
	nqn := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg, testCn)
	if got := len(node.subsystems[nqn].ctrls); got != 2 {
		t.Fatalf("the migrating leg has %d controllers, want 2", got)
	}

	node.Reset()
	// FinishMigration: the src side leaves side_list.
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: true})); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !node.hasCall("cmd nvme disconnect --device") {
		t.Fatalf("the dead path was not disconnected by device:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, "cmd nvme disconnect --nqn "+nqn)
	if got := len(node.subsystems[nqn].ctrls); got != 1 {
		t.Fatalf("the leg has %d controllers after the cutover, want 1", got)
	}
}
