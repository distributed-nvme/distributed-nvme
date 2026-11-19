package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// TestStartRecordCarriesTheBuildVersion pins the build version in the
// "gateway starting" record (GW2): the build of the process that runs,
// which --version, a read of the file on disk, cannot give
// (architecture.md, Upgrade in lockstep). The listener cannot open, so Run
// returns right after the record.
func TestStartRecordCarriesTheBuildVersion(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cli, err := etcdutil.New(context.Background(), []string{"127.0.0.1:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cli, Config{
		GrpcNetwork: "tcp", GrpcAddress: "256.0.0.1:1",
	}); err == nil {
		t.Fatal("Run served on an address that cannot be listened on")
	}
	line, _, _ := strings.Cut(buf.String(), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("first record %q: %v", line, err)
	}
	if rec["msg"] != msgGatewayStarting || rec["version"] != common.BuildVersion() {
		t.Errorf("first record = %v, want %q with version %q",
			rec, msgGatewayStarting, common.BuildVersion())
	}
}
