// Tests for cmd/dnv-gateway's CM1-CM3 surface (gateway.md §7): the flag set,
// the viper binding that makes a flag, a config file and an environment
// variable interchangeable, the validation of the two required values, and the
// two-signal handling. Nothing here touches etcd or opens a listener — the
// options path is deliberately I/O-free so every branch is drivable directly.
package main

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The invocation gateway.md §10.3 launches every instance of the integration
// suite with, plus the flag that suite leaves at its default.
var exampleArgs = []string{
	"--grpc-network", "tcp",
	"--grpc-address", "127.0.0.1:29810",
	"--etcd-endpoints", "127.0.0.1:15379",
}

// cm2Flags is the CM2 flag set, verbatim. There is deliberately no
// --etcd-op-timeout (EU5) and no default gRPC port (§0 #10).
var cm2Flags = []string{
	"config",
	"etcd-dial-timeout",
	"etcd-endpoints",
	"grpc-address",
	"grpc-network",
}

// parse binds one invocation's flags into a fresh viper, exactly as
// cmd/dnv-worker's tests do (CM1).
func parse(t *testing.T, args []string) *cobra.Command {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newRootCmd()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if err := bindViper(cmd); err != nil {
		t.Fatalf("bindViper: %v", err)
	}
	return cmd
}

func mustOptions(t *testing.T, args []string) *options {
	t.Helper()
	parse(t, args)
	opts, err := optionsFromViper()
	if err != nil {
		t.Fatalf("optionsFromViper(%v): %v", args, err)
	}
	return opts
}

// TestRootCommandHasNoSubcommands pins CM1: one root command, no subcommand
// tree, no positional arguments — every input is a flag, a config key or an
// environment variable.
func TestRootCommandHasNoSubcommands(t *testing.T) {
	cmd := newRootCmd()
	if len(cmd.Commands()) != 0 {
		t.Errorf("root has %d subcommands, want 0", len(cmd.Commands()))
	}
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Errorf("SilenceUsage/SilenceErrors = %v/%v, want true/true",
			cmd.SilenceUsage, cmd.SilenceErrors)
	}
	if err := cmd.Args(cmd, []string{"stray"}); err == nil {
		t.Errorf("a positional argument was accepted, want cobra.NoArgs")
	}
}

// TestFlagSetIsExactlyCM2 fails both ways: a missing flag and an undocumented
// extra one. It also pins the two deliberate absences of §0 #10 and EU5.
func TestFlagSetIsExactlyCM2(t *testing.T) {
	root := newRootCmd()
	got := longFlagNames(root.Flags().FlagUsages())
	if !reflect.DeepEqual(got, cm2Flags) {
		t.Errorf("flags = %v, want %v", got, cm2Flags)
	}
	if root.Flags().Lookup("etcd-op-timeout") != nil {
		t.Error("--etcd-op-timeout exists; EU5 says it deliberately does not")
	}
	if got := root.Flags().Lookup("grpc-address").DefValue; got != "" {
		t.Errorf("--grpc-address defaults to %q; §0 #10 says it has no "+
			"default port", got)
	}
}

var longFlagRe = regexp.MustCompile(`--([a-z0-9-]+)`)

// longFlagNames collects every --long-name mentioned in usage text, sorted and
// deduplicated. No flag usage string of this command names another flag, so
// over a help dump the result is exactly the flag set.
func longFlagNames(usage string) []string {
	seen := map[string]bool{}
	for _, match := range longFlagRe.FindAllStringSubmatch(usage, -1) {
		if match[1] != "help" {
			seen[match[1]] = true
		}
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestHelpListsExactlyTheCM2Flags keeps the help text and the flag set from
// drifting apart.
func TestHelpListsExactlyTheCM2Flags(t *testing.T) {
	cmd := newRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	got := longFlagNames(buf.String())
	if !reflect.DeepEqual(got, cm2Flags) {
		t.Errorf("help lists %v, want %v", got, cm2Flags)
	}
}

// TestDefaults pins the two CM2 defaults and the two values with none: a
// gateway that is given no address must fail rather than pick a port (§0 #10).
func TestDefaults(t *testing.T) {
	opts := mustOptions(t, exampleArgs)
	if opts.grpcNetwork != "tcp" {
		t.Errorf("grpc-network = %q, want tcp", opts.grpcNetwork)
	}
	want := time.Duration(common.DefaultEtcdDialTimeout) * time.Second
	if opts.dialTimeout != want {
		t.Errorf("etcd-dial-timeout = %v, want %v", opts.dialTimeout, want)
	}
	if !reflect.DeepEqual(opts.endpoints, []string{"127.0.0.1:15379"}) {
		t.Errorf("endpoints = %v, want [127.0.0.1:15379]", opts.endpoints)
	}
	if opts.grpcAddress != "127.0.0.1:29810" {
		t.Errorf("grpc-address = %q, want 127.0.0.1:29810", opts.grpcAddress)
	}
}

// TestEnvironmentSuppliesEveryValue pins CM1's env prefix: a value from the
// environment satisfies a required flag exactly as the flag does.
func TestEnvironmentSuppliesEveryValue(t *testing.T) {
	t.Setenv("DNV_GATEWAY_GRPC_ADDRESS", "10.0.0.9:29527")
	t.Setenv("DNV_GATEWAY_GRPC_NETWORK", "tcp")
	t.Setenv("DNV_GATEWAY_ETCD_ENDPOINTS", "a:2379,b:2379")
	t.Setenv("DNV_GATEWAY_ETCD_DIAL_TIMEOUT", "9")
	opts := mustOptions(t, nil)
	if opts.grpcAddress != "10.0.0.9:29527" {
		t.Errorf("grpc-address = %q, want 10.0.0.9:29527", opts.grpcAddress)
	}
	if !reflect.DeepEqual(opts.endpoints, []string{"a:2379", "b:2379"}) {
		t.Errorf("endpoints = %v, want [a:2379 b:2379]", opts.endpoints)
	}
	if opts.dialTimeout != 9*time.Second {
		t.Errorf("dial timeout = %v, want 9s", opts.dialTimeout)
	}
}

// TestConfigFile pins the optional --config path.
func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gw.yaml"
	body := "grpc-address: 127.0.0.1:1234\netcd-endpoints: e1:2379\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	opts := mustOptions(t, []string{"--config", path})
	if opts.grpcAddress != "127.0.0.1:1234" {
		t.Errorf("grpc-address = %q, want 127.0.0.1:1234", opts.grpcAddress)
	}
	if !reflect.DeepEqual(opts.endpoints, []string{"e1:2379"}) {
		t.Errorf("endpoints = %v, want [e1:2379]", opts.endpoints)
	}
}

// TestValidationRejections covers every branch that returns an error before
// anything is dialed or listened on.
func TestValidationRejections(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no grpc address",
			args: []string{"--etcd-endpoints", "e:2379"},
			want: "--grpc-address",
		},
		{
			name: "blank grpc address",
			args: []string{"--grpc-address", "   ",
				"--etcd-endpoints", "e:2379"},
			want: "--grpc-address",
		},
		{
			name: "empty grpc network",
			args: []string{"--grpc-address", "a:1", "--grpc-network", "",
				"--etcd-endpoints", "e:2379"},
			want: "--grpc-network",
		},
		{
			name: "no etcd endpoints",
			args: []string{"--grpc-address", "a:1"},
			want: "--etcd-endpoints",
		},
		{
			name: "blank etcd endpoints",
			args: []string{"--grpc-address", "a:1",
				"--etcd-endpoints", " , ,"},
			want: "--etcd-endpoints",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parse(t, tc.args)
			_, err := optionsFromViper()
			if err == nil {
				t.Fatalf("optionsFromViper(%v) succeeded, want an error",
					tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestZeroDialTimeoutIsPassedThrough pins EU5's contract: a non-positive dial
// timeout reaches etcdutil.New, which substitutes the default itself, rather
// than being silently rewritten here.
func TestZeroDialTimeoutIsPassedThrough(t *testing.T) {
	opts := mustOptions(t, append(append([]string(nil), exampleArgs...),
		"--etcd-dial-timeout", "0"))
	if opts.dialTimeout != 0 {
		t.Errorf("dial timeout = %v, want 0", opts.dialTimeout)
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ,", []string{"a", "b"}},
		{",,", nil},
	}
	for _, tc := range cases {
		if got := splitList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestWatchSignalsSecondSignalExits pins CM3: the first signal cancels the
// context, which makes gateway.Run call GracefulStop; a second one during that
// window abandons the drain with status 1, because a handler waiting on a hung
// agent may hold the process for DefaultGatewayAgentTimeout.
func TestWatchSignalsSecondSignalExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	canceled := make(chan struct{})
	exited := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		watchSignals(ctx, func() { close(canceled) }, sigCh,
			func(code int) { exited <- code })
		close(done)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("the first signal did not cancel the context")
	}
	sigCh <- syscall.SIGINT
	select {
	case code := <-exited:
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second signal did not exit")
	}
	<-done
}

// TestWatchSignalsReturnsWhenCtxEnds pins the other exit: a shutdown that was
// not started by a signal must not leave the watcher goroutine parked.
func TestWatchSignalsReturnsWhenCtxEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 2)
	done := make(chan struct{})
	go func() {
		watchSignals(ctx, cancel, sigCh, func(int) {
			t.Error("exit was called without a second signal")
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchSignals did not return when ctx ended")
	}
}
