package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/agent/nodecheck"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// roleFlags is an invocation both roles accept, with no disk.
func roleFlags(t *testing.T) []string {
	return []string{"--grpc-address", "127.0.0.1:1", "--tr-type", "tcp",
		"--adr-fam", "ipv4", "--tr-addr", "127.0.0.1", "--tr-svc-id", "4420",
		"--local-store", t.TempDir()}
}

// log.md R6, R13 (dnagent.md CM2, CM4): both roles take --log-level, from
// the flag or DNV_AGENT_LOG_LEVEL, set it before they serve, and change it
// while they serve on SIGUSR1 and SIGUSR2; a word other than debug, info or
// warn is refused before the start.
func TestRolesTakeTheLogLevel(t *testing.T) {
	oldServe := serveAgent
	kernelCuts(t)
	t.Cleanup(func() {
		serveAgent = oldServe
		viper.Reset()
		common.SetLogLevel(slog.LevelInfo)
	})
	for _, role := range []string{"dn", "cn"} {
		args := append([]string{role}, roleFlags(t)...)
		if role == "dn" {
			args = append(args, "--disk", "/dev/null")
		}
		common.SetLogLevel(slog.LevelInfo)
		var atServe, afterUsr1 slog.Level
		serveAgent = func(context.Context, string, string,
			func(context.Context), func(context.Context) error,
			func(*grpc.Server), func()) error {
			atServe = common.LogLevel()
			if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
				return err
			}
			deadline := time.Now().Add(5 * time.Second)
			for common.LogLevel() != slog.LevelDebug &&
				time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			afterUsr1 = common.LogLevel()
			return nil
		}
		var printed strings.Builder
		if err := executeRoot(append(args, "--log-level", "warn"),
			&printed); err != nil {
			t.Fatalf("%s: %v\n%s", role, err, printed.String())
		}
		if atServe != slog.LevelWarn || afterUsr1 != slog.LevelDebug {
			t.Errorf("%s --log-level warn: level %v at serve, %v after "+
				"SIGUSR1, want WARN, DEBUG", role, atServe, afterUsr1)
		}

		common.SetLogLevel(slog.LevelInfo)
		t.Setenv("DNV_AGENT_LOG_LEVEL", "debug")
		serveAgent = func(context.Context, string, string,
			func(context.Context), func(context.Context) error,
			func(*grpc.Server), func()) error {
			atServe = common.LogLevel()
			return nil
		}
		if err := executeRoot(args, &printed); err != nil {
			t.Fatalf("%s: %v\n%s", role, err, printed.String())
		}
		if atServe != slog.LevelDebug {
			t.Errorf("%s with DNV_AGENT_LOG_LEVEL=debug: level %v at serve",
				role, atServe)
		}
		os.Unsetenv("DNV_AGENT_LOG_LEVEL")

		served := false
		serveAgent = func(context.Context, string, string,
			func(context.Context), func(context.Context) error,
			func(*grpc.Server), func()) error {
			served = true
			return nil
		}
		err := executeRoot(append(args, "--log-level", "error"), &printed)
		want := `--log-level must be debug, info or warn, got "error"`
		if err == nil || err.Error() != want || served {
			t.Errorf("%s --log-level error: %v, served %v, want %q", role,
				err, served, want)
		}
	}
}

// dnagent.md CM5: the node check takes the agent's --log-level, so an
// agent's command line serves its check, and still logs at Warn.
func TestCheckTakesTheLogLevelAndStaysAtWarn(t *testing.T) {
	oldEnv := nodeEnv
	t.Cleanup(func() {
		nodeEnv = oldEnv
		viper.Reset()
		common.SetLogLevel(slog.LevelInfo)
	})
	nodeEnv = func() nodecheck.Env { return nodeTestEnv(t, map[string]string{}) }
	for _, args := range [][]string{
		{"check", "dn", "--disk", "/dev/loop0", "--log-level", "debug"},
		{"check", "cn", "--log-level", "debug", "--output", "json"},
	} {
		common.SetLogLevel(slog.LevelInfo)
		var printed strings.Builder
		err := executeRoot(args, &printed)
		var exit *nodecheck.ExitError
		if err != nil && (!errors.As(err, &exit) ||
			exit.Code == nodecheck.ExitNotRun) {
			t.Errorf("%v: %v\n%s", args, err, printed.String())
		}
		if got := common.LogLevel(); got != slog.LevelWarn {
			t.Errorf("%v ran at %v, want WARN", args, got)
		}
	}
}
