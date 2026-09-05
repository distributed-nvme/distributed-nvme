package main

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The invocation from architecture.md §13.
var exampleArgs = []string{
	"--etcd-endpoints",
	"192.168.0.10:2379,192.168.0.11:2379,192.168.0.12:2379",
	"--roles", "dn,cn,sp",
	"--vote-interval", "10",
	"--vote-grace-time", "60",
	"--etcd-dial-timeout", "5",
}

// cm1Flags is the CM1 flag set, verbatim.
var cm1Flags = []string{
	"config",
	"etcd-dial-timeout",
	"etcd-endpoints",
	"roles",
	"vote-grace-time",
	"vote-interval",
}

// parse binds one invocation's flags into a fresh viper, as the dnv-agent
// tests do (CM2).
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

// mustOptions parses an invocation that CM3 must accept.
func mustOptions(t *testing.T, args []string) *options {
	t.Helper()
	parse(t, args)
	opts, err := optionsFromViper()
	if err != nil {
		t.Fatalf("a valid invocation was rejected: %v", err)
	}
	return opts
}

// ---------------------------------------------------------------------------
// CM1 — the command and its flags
// ---------------------------------------------------------------------------

// CM1: one root command, no subcommands.
func TestRootCommandHasNoSubcommands(t *testing.T) {
	root := newRootCmd()
	if root.Use != "dnv-worker" {
		t.Errorf("root command = %q, want dnv-worker", root.Use)
	}
	if got := root.Commands(); len(got) != 0 {
		var names []string
		for _, cmd := range got {
			names = append(names, cmd.Use)
		}
		t.Errorf("subcommands = %v, want none", names)
	}
	if root.RunE == nil {
		t.Error("the root command has no RunE")
	}
}

// CM1: exactly the six flags of the table, and no --etcd-op-timeout (EU5).
func TestFlagSetIsExactlyCM1(t *testing.T) {
	root := newRootCmd()
	got := longFlagNames(root.Flags().FlagUsages())
	if !reflect.DeepEqual(got, cm1Flags) {
		t.Errorf("flags = %v, want %v", got, cm1Flags)
	}
	if root.Flags().Lookup("etcd-op-timeout") != nil {
		t.Error("--etcd-op-timeout exists; EU5 says it deliberately does not")
	}
}

// longFlagNames collects every --long-name mentioned in usage text, sorted
// and deduplicated. No flag usage string of this command names another flag,
// so over a help dump the result is exactly the flag set.
func longFlagNames(usage string) []string {
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`--([a-z0-9-]+)`).
		FindAllStringSubmatch(usage, -1) {
		seen[m[1]] = true
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CM1: --help lists exactly the CM1 flags (plus cobra's own --help).
func TestHelpListsExactlyTheCM1Flags(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	got := longFlagNames(out.String())
	want := append(append([]string{}, cm1Flags...), "help")
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--help lists %v, want %v\n%s", got, want, out.String())
	}
}

// ---------------------------------------------------------------------------
// CM1/CM2 — defaults and binding
// ---------------------------------------------------------------------------

// CM1: every default is the constant the table names.
func TestDefaults(t *testing.T) {
	opts := mustOptions(t, []string{"--etcd-endpoints", "127.0.0.1:2379"})
	if want := []string{"dn", "cn", "sp"}; !reflect.DeepEqual(opts.roles, want) {
		t.Errorf("roles default = %v, want %v", opts.roles, want)
	}
	if want := common.DefaultVoteWorkerInterval * time.Second; opts.voteInterval !=
		want {
		t.Errorf("vote-interval default = %v, want %v", opts.voteInterval, want)
	}
	if want := common.DefaultVoteWorkerGraceTime * time.Second; opts.graceTime !=
		want {
		t.Errorf("vote-grace-time default = %v, want %v", opts.graceTime, want)
	}
	if want := common.DefaultEtcdDialTimeout * time.Second; opts.dialTimeout !=
		want {
		t.Errorf("etcd-dial-timeout default = %v, want %v",
			opts.dialTimeout, want)
	}
	if opts.graceTooShort {
		t.Error("the default timers tripped the CM3 grace warning")
	}
}

// CM2: the §13 example parses and every value arrives through viper.
func TestExampleInvocationParses(t *testing.T) {
	opts := mustOptions(t, exampleArgs)
	want := []string{
		"192.168.0.10:2379", "192.168.0.11:2379", "192.168.0.12:2379",
	}
	if !reflect.DeepEqual(opts.endpoints, want) {
		t.Errorf("endpoints = %v, want %v", opts.endpoints, want)
	}
	if !reflect.DeepEqual(opts.roles, []string{"dn", "cn", "sp"}) {
		t.Errorf("roles = %v", opts.roles)
	}
}

// CM2: DNV_WORKER_ROLES=dn is honoured, and an explicit flag still wins.
func TestRolesFromEnvironment(t *testing.T) {
	t.Setenv("DNV_WORKER_ROLES", "dn")
	opts := mustOptions(t, []string{"--etcd-endpoints", "127.0.0.1:2379"})
	if !reflect.DeepEqual(opts.roles, []string{"dn"}) {
		t.Errorf("roles = %v, want [dn] from the environment", opts.roles)
	}

	opts = mustOptions(t, []string{
		"--etcd-endpoints", "127.0.0.1:2379", "--roles", "cn,sp",
	})
	if !reflect.DeepEqual(opts.roles, []string{"cn", "sp"}) {
		t.Errorf("roles = %v, want the explicit flag value", opts.roles)
	}
}

// CM2: every other value reaches viper from the environment too, including
// the required --etcd-endpoints (checked after binding, never by cobra).
func TestEnvironmentSuppliesEveryValue(t *testing.T) {
	t.Setenv("DNV_WORKER_ETCD_ENDPOINTS", "10.0.0.1:2379,10.0.0.2:2379")
	t.Setenv("DNV_WORKER_VOTE_INTERVAL", "1")
	t.Setenv("DNV_WORKER_VOTE_GRACE_TIME", "3")
	t.Setenv("DNV_WORKER_ETCD_DIAL_TIMEOUT", "2")
	opts := mustOptions(t, nil)
	if want := []string{"10.0.0.1:2379", "10.0.0.2:2379"}; !reflect.DeepEqual(
		opts.endpoints, want) {
		t.Errorf("endpoints = %v, want %v", opts.endpoints, want)
	}
	if opts.voteInterval != time.Second {
		t.Errorf("vote-interval = %v, want 1s", opts.voteInterval)
	}
	if opts.graceTime != 3*time.Second {
		t.Errorf("vote-grace-time = %v, want 3s", opts.graceTime)
	}
	if opts.dialTimeout != 2*time.Second {
		t.Errorf("etcd-dial-timeout = %v, want 2s", opts.dialTimeout)
	}
}

// CM2: --config is read, and file values fill in what flags leave unset.
func TestConfigFile(t *testing.T) {
	path := t.TempDir() + "/worker.yaml"
	err := os.WriteFile(path,
		[]byte("etcd-endpoints: 10.1.2.3:2379\nroles: sp\n"), 0o600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}
	opts := mustOptions(t, []string{"--config", path})
	if want := []string{"10.1.2.3:2379"}; !reflect.DeepEqual(
		opts.endpoints, want) {
		t.Errorf("endpoints = %v, want %v", opts.endpoints, want)
	}
	if !reflect.DeepEqual(opts.roles, []string{"sp"}) {
		t.Errorf("roles = %v, want [sp]", opts.roles)
	}
}

// ---------------------------------------------------------------------------
// CM3 — validation
// ---------------------------------------------------------------------------

// CM3: every rejection, and the exact phrase the operator sees.
func TestValidationRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no endpoints",
			args: []string{"--roles", "dn"},
			want: "--etcd-endpoints",
		},
		{
			name: "blank endpoints",
			args: []string{"--etcd-endpoints", " , "},
			want: "--etcd-endpoints",
		},
		{
			name: "empty roles",
			args: []string{"--etcd-endpoints", "127.0.0.1:2379", "--roles", ""},
			want: "--roles must name at least one",
		},
		{
			name: "unknown role",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379", "--roles", "dn,gw",
			},
			want: `unknown role "gw"`,
		},
		{
			name: "duplicate role",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379", "--roles", "dn,cn,dn",
			},
			want: `duplicate role "dn"`,
		},
		{
			name: "vote interval below one",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379", "--vote-interval", "0",
			},
			want: "--vote-interval must be at least 1 second",
		},
		{
			name: "negative vote interval",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379", "--vote-interval", "-1",
			},
			want: "--vote-interval must be at least 1 second",
		},
		{
			name: "grace time below one",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379", "--vote-grace-time", "0",
			},
			want: "--vote-grace-time must be at least 1 second",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parse(t, tc.args)
			opts, err := optionsFromViper()
			if err == nil {
				t.Fatalf("accepted %v as %+v", tc.args, opts)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// CM3: any subset of the three roles is legal, in any order.
func TestEveryRoleSubsetIsAccepted(t *testing.T) {
	for _, raw := range []string{
		"dn", "cn", "sp", "sp,dn", "cn,sp", "dn, cn , sp", "sp,cn,dn",
	} {
		opts := mustOptions(t, []string{
			"--etcd-endpoints", "127.0.0.1:2379", "--roles", raw,
		})
		if len(opts.roles) != len(splitList(raw)) {
			t.Errorf("--roles %q parsed to %v", raw, opts.roles)
		}
	}
}

// CM3: a grace window that does not exceed the dead threshold is a warning,
// never a rejection.
func TestShortGraceTimeWarnsButIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		interval, grace int
		wantWarn        bool
	}{
		{interval: 10, grace: 60, wantWarn: false},
		{interval: 10, grace: 21, wantWarn: false},
		{interval: 10, grace: 20, wantWarn: true},
		{interval: 10, grace: 5, wantWarn: true},
		{interval: 1, grace: 3, wantWarn: false},
		{interval: 1, grace: 2, wantWarn: true},
	} {
		opts := mustOptions(t, []string{
			"--etcd-endpoints", "127.0.0.1:2379",
			"--vote-interval", strconv.Itoa(tc.interval),
			"--vote-grace-time", strconv.Itoa(tc.grace),
		})
		if opts.graceTooShort != tc.wantWarn {
			t.Errorf("interval=%d grace=%d: warn = %v, want %v",
				tc.interval, tc.grace, opts.graceTooShort, tc.wantWarn)
		}
	}
}

// EU5: a non-positive dial timeout is passed through to etcdutil.New, which
// substitutes the default; it is not a configuration error.
func TestZeroDialTimeoutIsPassedThrough(t *testing.T) {
	opts := mustOptions(t, []string{
		"--etcd-endpoints", "127.0.0.1:2379", "--etcd-dial-timeout", "0",
	})
	if opts.dialTimeout != 0 {
		t.Errorf("dial timeout = %v, want 0 (etcdutil substitutes the default)",
			opts.dialTimeout)
	}
}

func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{raw: "", want: nil},
		{raw: "  ", want: nil},
		{raw: ",,", want: nil},
		{raw: "a", want: []string{"a"}},
		{raw: " a , b ,", want: []string{"a", "b"}},
	} {
		if got := splitList(tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// CM5 — signals
// ---------------------------------------------------------------------------

// CM5: the first signal cancels ctx, the second exits with status 1.
func TestWatchSignalsSecondSignalExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSignals(ctx, cancel, sigCh, func(code int) { exited <- code })
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the first signal did not cancel ctx")
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

// CM5: a clean shutdown (Run returns, ctx ends) leaves no signal watcher
// behind and never exits with status 1.
func TestWatchSignalsReturnsWhenCtxEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSignals(ctx, cancel, sigCh, func(int) {
			t.Error("exit was called without any signal")
		})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchSignals did not return when ctx ended")
	}
}
