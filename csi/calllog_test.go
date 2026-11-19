package csi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// callLog is a capture of the call records, at Debug.
type callLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *callLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// levels lists "<msg> <level>" for every captured record, in order.
func (c *callLog) levels(t *testing.T) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, rec["msg"].(string)+" "+rec["level"].(string))
	}
	return out
}

func captureCallLog(t *testing.T) *callLog {
	t.Helper()
	capture := &callLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(capture,
			&slog.HandlerOptions{Level: slog.LevelDebug}),
	}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return capture
}

// csi.md, Log records: the calls Kubernetes repeats on a timer — the
// liveness probe, the provisioner's capacity poll and the kubelet's volume
// stats with the capability call that comes before it — log their request
// and their reply at DEBUG, and a failure of one at INFO; every other call
// logs both at INFO.
func TestCallRecordLevels(t *testing.T) {
	ok := func(context.Context, any) (any, error) { return "reply", nil }
	fail := func(context.Context, any) (any, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}
	for _, tc := range []struct {
		method  string
		handler grpc.UnaryHandler
		want    []string
	}{
		{"/csi.v1.Identity/Probe", ok,
			[]string{"csi request DEBUG", "csi reply DEBUG"}},
		{"/csi.v1.Controller/GetCapacity", ok,
			[]string{"csi request DEBUG", "csi reply DEBUG"}},
		{"/csi.v1.Node/NodeGetVolumeStats", ok,
			[]string{"csi request DEBUG", "csi reply DEBUG"}},
		{"/csi.v1.Node/NodeGetCapabilities", ok,
			[]string{"csi request DEBUG", "csi reply DEBUG"}},
		{"/csi.v1.Identity/Probe", fail,
			[]string{"csi request DEBUG", "csi reply INFO"}},
		{"/csi.v1.Controller/GetCapacity", fail,
			[]string{"csi request DEBUG", "csi reply INFO"}},
		{"/csi.v1.Controller/CreateVolume", ok,
			[]string{"csi request INFO", "csi reply INFO"}},
		{"/csi.v1.Node/NodeStageVolume", fail,
			[]string{"csi request INFO", "csi reply INFO"}},
	} {
		capture := captureCallLog(t)
		unaryInterceptor()(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: tc.method}, tc.handler)
		got := capture.levels(t)
		if strings.Join(got, ", ") != strings.Join(tc.want, ", ") {
			t.Errorf("%s: records %v, want %v", tc.method, got, tc.want)
		}
	}
}

// records decodes every captured record.
func (c *callLog) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

// syncErrorLevels lists "<key>=<value> <level>" for each `csi follower
// sync error` record, keyed by the cdc or the endpoint it names.
func syncErrorLevels(t *testing.T, c *callLog) []string {
	t.Helper()
	var out []string
	for _, rec := range c.records(t) {
		if rec["msg"] != msgFollowerSyncError {
			continue
		}
		key := "none"
		if cdc, ok := rec["cdc"].(string); ok {
			key = "cdc=" + cdc
		} else if ep, ok := rec["endpoint"].(string); ok {
			key = "endpoint=" + ep
		}
		out = append(out, key+" "+rec["level"].(string))
	}
	return out
}

// csi.md, Log records (log.md R6): a cdc the follower cannot reach, or whose
// log it cannot read, is a lost redundancy that holds until the cdc comes
// back or the operator fixes the list — the follower holds back every drop
// meanwhile — so its record, naming the cdc, is at WARN; a path connect a
// controller node refuses, which the next pass tries again, is at INFO.
func TestFollowerSyncErrorLevels(t *testing.T) {
	t.Run("a cdc it cannot reach", func(t *testing.T) {
		n, f := newTestNode(t)
		f.mu.Lock()
		// A staged volume's path, and no discovery connection to the cdc.
		f.addCtrl(subsystemNqnOf(testVolume), mustEndpoint(fakeCn0))
		f.discFail[fakeCdcA] = true
		f.mu.Unlock()
		if err := n.follow.recover(t.Context()); err != nil {
			t.Fatal(err)
		}
		capture := captureCallLog(t)
		if err := syncDrained(t.Context(), n); err == nil {
			t.Fatal("a sync that reached no cdc answered no error")
		}
		got := strings.Join(syncErrorLevels(t, capture), ", ")
		if want := "cdc=" + fakeCdcA + " WARN"; got != want {
			t.Errorf("records %q, want %q", got, want)
		}
	})
	t.Run("a cdc whose log it cannot read", func(t *testing.T) {
		n, f := newTestNode(t)
		stagedOnRestart(t, n, f)
		f.mu.Lock()
		f.logFail[fakeCdcA] = true
		f.mu.Unlock()
		capture := captureCallLog(t)
		if err := syncDrained(t.Context(), n); err == nil {
			t.Fatal("a sync whose log read failed answered no error")
		}
		got := strings.Join(syncErrorLevels(t, capture), ", ")
		if want := "cdc=" + fakeCdcA + " WARN"; got != want {
			t.Errorf("records %q, want %q", got, want)
		}
	})
	t.Run("a path connect a controller node refuses", func(t *testing.T) {
		n, f := newTestNode(t)
		stagedOnRestart(t, n, f)
		f.mu.Lock()
		f.connectErr[fakeCn1] = "Failed to write to /dev/nvme-fabrics: Connection refused"
		f.mu.Unlock()
		capture := captureCallLog(t)
		if err := syncDrained(t.Context(), n); err == nil {
			t.Fatal("a sync whose connect was refused answered no error")
		}
		got := strings.Join(syncErrorLevels(t, capture), ", ")
		if want := "endpoint=" + fakeCn1 + " INFO"; got != want {
			t.Errorf("records %q, want %q", got, want)
		}
	})
}
