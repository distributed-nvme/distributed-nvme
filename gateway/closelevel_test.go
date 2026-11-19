package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// log.md R6 (GW2): a failed close of the etcd client at the stop is an event
// the program cannot act on, at Info, as in the worker and the cdc. The
// listener cannot open, so Run returns at once, and the client was closed
// before, so the close Run defers fails.
func TestFailedCloseAtStopIsInfo(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cli, err := etcdutil.New(context.Background(), []string{"127.0.0.1:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cli, Config{
		GrpcNetwork: "tcp", GrpcAddress: "256.0.0.1:1",
	}); err == nil {
		t.Fatal("Run served on an address that cannot be listened on")
	}
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if rec["msg"] == "etcd client close failed" {
			levels = append(levels, rec["level"].(string))
		}
	}
	if strings.Join(levels, " ") != "INFO" {
		t.Errorf("close records at %v, want one at INFO", levels)
	}
}
