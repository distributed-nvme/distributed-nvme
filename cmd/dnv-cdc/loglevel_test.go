package main

import (
	"context"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/cdc"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// log.md R6: --log-level takes debug, info or warn, from the flag, the
// environment or a config file, with info by default; any other word is
// refused before the start, naming the words.
func TestLogLevel(t *testing.T) {
	base := []string{"--etcd-endpoints", "127.0.0.1:2379", "--tr-addr", "127.0.0.1"}
	if got := mustOptions(t, base).logLevel; got != slog.LevelInfo {
		t.Errorf("default log level %v, want INFO", got)
	}
	if got := mustOptions(t, append(base, "--log-level", "debug")).logLevel; got != slog.LevelDebug {
		t.Errorf("--log-level debug gave %v", got)
	}
	t.Setenv("DNV_CDC_LOG_LEVEL", "WARN")
	if got := mustOptions(t, base).logLevel; got != slog.LevelWarn {
		t.Errorf("DNV_CDC_LOG_LEVEL=WARN gave %v", got)
	}
	if got := mustOptions(t, append(base, "--log-level", "info")).logLevel; got != slog.LevelInfo {
		t.Errorf("the flag did not beat the environment: %v", got)
	}
	os.Unsetenv("DNV_CDC_LOG_LEVEL")
	path := t.TempDir() + "/conf.yaml"
	if err := os.WriteFile(path, []byte("log-level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustOptions(t, append(base, "--config", path)).logLevel; got != slog.LevelDebug {
		t.Errorf("a config file's log-level: debug gave %v", got)
	}
	for _, bad := range []string{"error", "trace", "1"} {
		parse(t, append(base, "--log-level", bad))
		_, err := optionsFromViper()
		want := `--log-level must be debug, info or warn, got "` + bad + `"`
		if err == nil || err.Error() != want {
			t.Errorf("--log-level %s: error %v, want %q", bad, err, want)
		}
	}
}

// signalAndWait sends sig to this process and waits up to five seconds for
// the process log level to read want, returning what it reads then.
func signalAndWait(t *testing.T, sig syscall.Signal, want slog.Level) slog.Level {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatalf("kill %v: %v", sig, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for common.LogLevel() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return common.LogLevel()
}

// log.md R6, R13: run sets the --log-level before it hands off, and the
// process takes SIGUSR1 and SIGUSR2 while the hand-off runs: SIGUSR1 sets
// Debug, SIGUSR2 the level it started with.
func TestRunSetsTheLogLevelAndWatchesTheSignals(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	common.SetLogLevel(slog.LevelInfo)
	t.Cleanup(func() { common.SetLogLevel(slog.LevelInfo) })
	old := runCdc
	t.Cleanup(func() { runCdc = old })
	var atRun, afterUsr1, afterUsr2 slog.Level
	runCdc = func(_ context.Context, cli *etcdutil.Client, _ cdc.Config) error {
		defer cli.Close()
		atRun = common.LogLevel()
		afterUsr1 = signalAndWait(t, syscall.SIGUSR1, slog.LevelDebug)
		afterUsr2 = signalAndWait(t, syscall.SIGUSR2, slog.LevelWarn)
		return nil
	}
	cmd := newRootCmd()
	if err := cmd.ParseFlags([]string{"--etcd-endpoints", "127.0.0.1:1", "--tr-addr", "127.0.0.1", "--log-level", "warn"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if err := run(cmd, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if atRun != slog.LevelWarn || afterUsr1 != slog.LevelDebug ||
		afterUsr2 != slog.LevelWarn {
		t.Errorf("level %v at the hand-off, %v after SIGUSR1, %v after "+
			"SIGUSR2, want WARN, DEBUG, WARN", atRun, afterUsr1, afterUsr2)
	}
}
