// The invocation model (dnvctl.md §2): the parts of CT2/CT9 that are neither
// a request field nor a rendered document — the per-invocation deadline, the
// one connection and its close, the viper config file, and the stderr logger
// cmd/dnvctl/main.go installs.
//
// These sit under CT-T2/CT-T5 rather than having a tag of their own, but they
// are where a defect would be invisible to every other test in the package:
// a deadline that is never applied, a connection that is never closed and a
// --config that is silently ignored all leave the request and the rendering
// exactly as they should be.
package ctl

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestTimeoutIsApplied is §2.2's "every RPC runs under
// context.WithTimeout(ctx, timeout seconds)", proved the only way it can be:
// against a server that never answers. It is §7.13's d2 without the VM.
func TestTimeoutIsApplied(t *testing.T) {
	srv := &traceServer{hang: make(chan struct{})}
	seam := serveTraceWith(t, srv)
	quietLogging(t)

	start := time.Now()
	res := runCLIWithDial(t, seam, globalArgv(
		"--timeout", "0.3", "--trace-id", "it-transport-3",
		"cluster", "list")...)
	elapsed := time.Since(start)

	if res.code != 1 {
		t.Errorf("exit code = %d, want 1", res.code)
	}
	if !strings.HasPrefix(res.stderr, "dnvctl: DEADLINE_EXCEEDED: ") {
		t.Errorf("stderr = %q, want a DEADLINE_EXCEEDED line", res.stderr)
	}
	if !strings.HasSuffix(res.stderr, "(trace_id it-transport-3)\n") {
		t.Errorf("stderr = %q, want it to end with the trace id", res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
	// The deadline must be the one that was asked for: too early would mean
	// some other timeout is in charge, and the default 10 s would mean the
	// flag is not read at all.
	if elapsed < 250*time.Millisecond {
		t.Errorf("gave up after %v, want at least the 0.3 s asked for",
			elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("gave up after %v, want ~0.3 s: --timeout was not applied",
			elapsed)
	}
	close(srv.hang)
}

// TestConnectionIsClosed is the rest of §2.2: ONE connection per invocation,
// closed on exit — on the success path and on the failure path alike, since a
// CLI that leaked its connection on every error would still pass every other
// test here.
func TestConnectionIsClosed(t *testing.T) {
	cases := []struct {
		name   string
		client *recordingClient
		code   int
	}{
		{"success", &recordingClient{want: "ListClusters"}, 0},
		{"rpc failure", &recordingClient{
			want: "ListClusters",
			err:  context.DeadlineExceeded,
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dials, closes := 0, 0
			seam := func(_ context.Context, address string) (
				pb.GatewayClient, func() error, error,
			) {
				if address != gatewayAddress {
					t.Errorf("dialed %q, want %q", address, gatewayAddress)
				}
				dials++
				return tc.client, func() error { closes++; return nil }, nil
			}
			res := runCLIWithDial(t, seam, globalArgv("cluster", "list")...)
			if res.code != tc.code {
				t.Errorf("exit code = %d, want %d", res.code, tc.code)
			}
			if dials != 1 {
				t.Errorf("%d dials, want exactly 1", dials)
			}
			if closes != 1 {
				t.Errorf("%d closes, want exactly 1 — the connection is "+
					"closed on exit (§2.2)", closes)
			}
		})
	}
}

// TestConfigFile covers the last leg of CT9's precedence chain. --config is
// the only one of the four sources with a failure mode of its own: an
// unreadable file must stop the invocation rather than be ignored, because a
// silently skipped config would send requests scoped to the wrong cluster.
func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dnvctl.json")
	body := `{"gateway-address": "` + gatewayAddress + `",
	          "cluster": "cfgclu", "sp": "cfgsp"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Run("config supplies the globals", func(t *testing.T) {
		req := runArgvFull(t, "GetStoragePool",
			"--config", path, "sp", "get").(*pb.GetStoragePoolRequest)
		wantRequest(t, req, &pb.GetStoragePoolRequest{
			ClusterName: "cfgclu", SpName: "cfgsp"})
	})

	t.Run("a flag beats the config", func(t *testing.T) {
		req := runArgvFull(t, "GetStoragePool",
			"--config", path, "--cluster", "flagclu", "sp", "get")
		wantRequest(t, req, &pb.GetStoragePoolRequest{
			ClusterName: "flagclu", SpName: "cfgsp"})
	})

	t.Run("the environment beats the config", func(t *testing.T) {
		t.Setenv("DNVCTL_CLUSTER", "envclu")
		req := runArgvFull(t, "GetStoragePool",
			"--config", path, "sp", "get").(*pb.GetStoragePoolRequest)
		wantRequest(t, req, &pb.GetStoragePoolRequest{
			ClusterName: "envclu", SpName: "cfgsp"})
	})

	t.Run("a missing config is an error", func(t *testing.T) {
		client := &recordingClient{}
		res := runCLI(t, client, globalArgv(
			"--config", filepath.Join(dir, "nope.json"), "sp", "get")...)
		if res.code != 2 {
			t.Errorf("exit code = %d, want 2", res.code)
		}
		if client.calls != 0 {
			t.Errorf("%d RPCs were issued, want 0", client.calls)
		}
		if res.stdout != "" {
			t.Errorf("stdout = %q, want empty", res.stdout)
		}
	})
}

// TestInstallStderrLogging is CT7: what survives the level goes to STDERR,
// because stdout is reserved for the one result document, and the level
// travels explicitly — building the handler with a nil options argument would
// drop common's LevelVar and leave the process logging at Info, which is the
// latent bug root.go's comment says the three integtest drivers have.
func TestInstallStderrLogging(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	stdout, stderr := captureOutput(t, func() {
		InstallStderrLogging(slog.LevelWarn)
		slog.Info("info record")
		slog.WarnContext(
			common.WithTraceId(context.Background(), "trace-abc"),
			"warn record")
	})

	if stdout != "" {
		t.Errorf("logging wrote %q to stdout, which is reserved for the "+
			"result document (CT7)", stdout)
	}
	if strings.Contains(stderr, "info record") {
		t.Errorf("an Info record survived level Warn: %q", stderr)
	}
	if !strings.Contains(stderr, "warn record") {
		t.Errorf("stderr = %q, want the Warn record", stderr)
	}
	// common.TraceIdHandler must wrap the JSON handler, or a dnvctl warning
	// would be unjoinable to the gateway's log for the same invocation.
	if !strings.Contains(stderr, `"trace_id":"trace-abc"`) {
		t.Errorf("stderr = %q, want the trace id attribute", stderr)
	}
}
