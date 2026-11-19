package ctl

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// processLevel is a slog.Leveler that reads the process log level each
// time, as the handler common's init builds does.
type processLevel struct{}

func (processLevel) Level() slog.Level { return common.LogLevel() }

// levelLogging installs a logger over a buffer that honours the process log
// level, and restores the process default and its level afterwards.
func levelLogging(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(
			&buf, &slog.HandlerOptions{Level: processLevel{}}),
	}))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		common.SetLogLevel(slog.LevelWarn)
	})
	return &buf
}

// dnvctl.md CT7, CT9 (log.md R6): --log-level, env-backed as
// DNVCTL_LOG_LEVEL, sets dnvctl's level, warn by default, so --log-level
// info shows the client chain's records on stderr; any other word than
// debug, info or warn is a usage error that issues no RPC.
func TestLogLevelFlag(t *testing.T) {
	srv, seam := serveTrace(t)
	logs := levelLogging(t)

	common.SetLogLevel(slog.LevelWarn)
	if res := runCLIWithDial(t, seam, globalArgv("cluster", "list")...); res.code != 0 {
		t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
	}
	if got := common.LogLevel(); got != slog.LevelWarn || logs.Len() != 0 {
		t.Errorf("by default: level %v, logged %q, want WARN and nothing",
			got, logs.String())
	}

	if res := runCLIWithDial(t, seam,
		globalArgv("--log-level", "info", "cluster", "list")...); res.code != 0 {
		t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
	}
	if got := common.LogLevel(); got != slog.LevelInfo ||
		!strings.Contains(logs.String(), `"msg":"grpc client request"`) {
		t.Errorf("--log-level info: level %v, logged %q, want INFO and the "+
			"client chain's request record", got, logs.String())
	}

	common.SetLogLevel(slog.LevelWarn)
	t.Setenv("DNVCTL_LOG_LEVEL", "debug")
	if res := runCLIWithDial(t, seam, globalArgv("cluster", "list")...); res.code != 0 {
		t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
	}
	if got := common.LogLevel(); got != slog.LevelDebug {
		t.Errorf("DNVCTL_LOG_LEVEL=debug: level %v", got)
	}

	calls := len(srv.seen())
	res := runCLIWithDial(t, seam,
		globalArgv("--log-level", "error", "cluster", "list")...)
	want := `dnvctl: --log-level must be debug, info or warn, got "error"` + "\n"
	if res.code != 2 || res.stderr != want || res.stdout != "" {
		t.Errorf("--log-level error: exit %d, stdout %q, stderr %q, want 2 "+
			"and %q", res.code, res.stdout, res.stderr, want)
	}
	if got := len(srv.seen()); got != calls {
		t.Errorf("a bad --log-level issued %d RPCs", got-calls)
	}
}
