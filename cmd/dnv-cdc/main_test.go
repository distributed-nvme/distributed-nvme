package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
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

	"github.com/distributed-nvme/distributed-nvme/cdc"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// The invocation from cdc.md §6 (architecture.md §13, amended per §10).
var exampleArgs = []string{
	"--etcd-endpoints", "192.168.0.10:2379,192.168.0.11:2379",
	"--range", "0,1,2,3,4,5,6,7",
	"--tr-type", "tcp",
	"--adr-fam", "ipv4",
	"--tr-addr", "192.168.0.10",
	"--tr-svc-id", "8009",
}

// cm1Flags is the CM1 flag set, verbatim.
var cm1Flags = []string{
	"adr-fam",
	"config",
	"etcd-dial-timeout",
	"etcd-endpoints",
	"range",
	"tr-addr",
	"tr-svc-id",
	"tr-type",
}

// minArgs is the shortest invocation CM2 accepts: the two required flags,
// everything else defaulted.
var minArgs = []string{
	"--etcd-endpoints", "127.0.0.1:2379", "--tr-addr", "127.0.0.1",
}

// parse binds one invocation's flags into a fresh viper, as the dnv-worker
// tests do (CM1). viper is process-global, so every case resets it.
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

// mustOptions parses an invocation that CM2 must accept.
func mustOptions(t *testing.T, args []string) *options {
	t.Helper()
	parse(t, args)
	opts, err := optionsFromViper()
	if err != nil {
		t.Fatalf("a valid invocation was rejected: %v", err)
	}
	return opts
}

// wantError parses an invocation CM2 must refuse and returns the message.
func wantError(t *testing.T, args []string, want string) {
	t.Helper()
	parse(t, args)
	opts, err := optionsFromViper()
	if err == nil {
		t.Fatalf("accepted %v as %+v", args, opts)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

// ---------------------------------------------------------------------------
// CM1 — the command and its flags
// ---------------------------------------------------------------------------

// CM1: one root command, no subcommands.
func TestRootCommandHasNoSubcommands(t *testing.T) {
	root := newRootCmd()
	if root.Use != "dnv-cdc" {
		t.Errorf("root command = %q, want dnv-cdc", root.Use)
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

// CM1: exactly the eight flags of the table, and no --etcd-op-timeout (EU5).
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
// CM1 — defaults
// ---------------------------------------------------------------------------

// CM1: every default is the constant the table names, and the --range
// default is CdcRangeAll: all sixteen digits, in order, so a single instance
// with no --range owns the whole space (DS2).
func TestDefaults(t *testing.T) {
	opts := mustOptions(t, minArgs)
	want := []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if !reflect.DeepEqual(opts.ranges, want) {
		t.Errorf("range default = %v, want %v", opts.ranges, want)
	}
	if opts.trType != common.DefaultCdcTrType {
		t.Errorf("tr-type default = %q, want %q",
			opts.trType, common.DefaultCdcTrType)
	}
	if opts.adrFam != common.DefaultCdcAdrFam {
		t.Errorf("adr-fam default = %q, want %q",
			opts.adrFam, common.DefaultCdcAdrFam)
	}
	if opts.trSvcId != common.DefaultCdcTrSvcId {
		t.Errorf("tr-svc-id default = %q, want %q",
			opts.trSvcId, common.DefaultCdcTrSvcId)
	}
	if want := common.DefaultEtcdDialTimeout * time.Second; opts.dialTimeout !=
		want {
		t.Errorf("etcd-dial-timeout default = %v, want %v",
			opts.dialTimeout, want)
	}
}

// CM1: the --range default is spelled exactly CdcRangeAll, so an operator
// reading --help sees the digits the constant names.
func TestRangeFlagDefaultIsCdcRangeAll(t *testing.T) {
	root := newRootCmd()
	flag := root.Flags().Lookup("range")
	if flag == nil {
		t.Fatal("--range does not exist")
	}
	if flag.DefValue != common.CdcRangeAll {
		t.Errorf("--range default = %q, want %q",
			flag.DefValue, common.CdcRangeAll)
	}
}

// ---------------------------------------------------------------------------
// CM2 — --range parsing
// ---------------------------------------------------------------------------

// CM2: --range is a non-empty, duplicate-free list of single lower-case hex
// digits, kept in the order given (DS2).
func TestParseRanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []uint32
	}{
		{name: "one digit", raw: "0", want: []uint32{0}},
		{name: "top digit", raw: "f", want: []uint32{15}},
		{name: "subset", raw: "0,1,2,3,4,5,6,7",
			want: []uint32{0, 1, 2, 3, 4, 5, 6, 7}},
		{name: "order preserved", raw: "f,0,a",
			want: []uint32{15, 0, 10}},
		{name: "whitespace trimmed", raw: " 3 , c ",
			want: []uint32{3, 12}},
		{name: "trailing comma", raw: "1,2,", want: []uint32{1, 2}},
		{name: "all sixteen", raw: common.CdcRangeAll,
			want: []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14,
				15}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRanges(tc.raw)
			if err != nil {
				t.Fatalf("parseRanges(%q): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseRanges(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// CM2: every --range spelling that is not a single lower-case hex digit is a
// refusal, because a mis-parsed digit is a silent coverage gap (§0 #3).
func TestParseRangesRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: "must name at least one hex digit"},
		{name: "blank", raw: " , ",
			want: "must name at least one hex digit"},
		{name: "two digits 0f", raw: "0f", want: `"0f" is not a single hex`},
		{name: "two digits 10", raw: "10", want: `"10" is not a single hex`},
		{name: "upper case", raw: "F", want: `"F" is not a single hex`},
		{name: "upper case in list", raw: "0,A",
			want: `"A" is not a single hex`},
		{name: "not hex", raw: "g", want: `"g" is not a single hex`},
		{name: "hex prefix", raw: "0x1", want: `"0x1" is not a single hex`},
		{name: "duplicate", raw: "1,2,1", want: `duplicate digit "1"`},
		{name: "duplicate of the default", raw: common.CdcRangeAll + ",0",
			want: `duplicate digit "0"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRanges(tc.raw)
			if err == nil {
				t.Fatalf("parseRanges(%q) = %v, want an error", tc.raw, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// CM2: the same refusals arrive through the flag, not only through the
// helper.
func TestRangeFlagRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: "--range must name at least one"},
		{name: "upper case", raw: "A", want: "--range:"},
		{name: "duplicate", raw: "3,3", want: "--range: duplicate digit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, append(append([]string{}, minArgs...),
				"--range", tc.raw), tc.want)
		})
	}
}

// ---------------------------------------------------------------------------
// CM2 — the rest of the validation
// ---------------------------------------------------------------------------

// CM2: every rejection of the endpoint and the etcd flags, one case each,
// with the exact phrase the operator sees.
func TestValidationRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no endpoints",
			args: []string{"--tr-addr", "127.0.0.1"},
			want: "--etcd-endpoints",
		},
		{
			name: "blank endpoints",
			args: []string{
				"--etcd-endpoints", " , ", "--tr-addr", "127.0.0.1",
			},
			want: "--etcd-endpoints",
		},
		{
			name: "rdma tr-type",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-type", "rdma",
			},
			want: `--tr-type must be "tcp", got "rdma"`,
		},
		{
			name: "upper case tr-type",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-type", "TCP",
			},
			want: `--tr-type must be "tcp"`,
		},
		{
			name: "unknown adr-fam",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--adr-fam", "fc",
			},
			want: `--adr-fam must be "ipv4" or "ipv6", got "fc"`,
		},
		{
			name: "missing tr-addr",
			args: []string{"--etcd-endpoints", "127.0.0.1:2379"},
			want: "missing required flag: --tr-addr",
		},
		{
			name: "blank tr-addr",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379", "--tr-addr", "  ",
			},
			want: "missing required flag: --tr-addr",
		},
		{
			name: "tr-addr is a name",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "cdc.example.com",
			},
			want: `--tr-addr: "cdc.example.com" is not an IP address`,
		},
		{
			name: "tr-addr carries a port",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "192.168.0.10:8009",
			},
			want: "is not an IP address",
		},
		{
			name: "ipv6 address under adr-fam ipv4",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--adr-fam", "ipv4", "--tr-addr", "fd00::1",
			},
			want: `--tr-addr: "fd00::1" is not an ipv4 address`,
		},
		{
			name: "ipv4 address under adr-fam ipv6",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--adr-fam", "ipv6", "--tr-addr", "192.168.0.10",
			},
			want: `--tr-addr: "192.168.0.10" is not an ipv6 address`,
		},
		{
			name: "tr-svc-id zero",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-svc-id", "0",
			},
			want: `--tr-svc-id: "0" is not a tcp port in [1, 65535]`,
		},
		{
			name: "tr-svc-id above 65535",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-svc-id", "65536",
			},
			want: `--tr-svc-id: "65536" is not a tcp port in [1, 65535]`,
		},
		{
			name: "tr-svc-id negative",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-svc-id", "-1",
			},
			want: `--tr-svc-id: "-1" is not a tcp port in [1, 65535]`,
		},
		{
			name: "tr-svc-id is a service name",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-svc-id", "nvme-disc",
			},
			want: "is not a tcp port in [1, 65535]",
		},
		{
			name: "empty tr-svc-id",
			args: []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--tr-addr", "127.0.0.1", "--tr-svc-id", "",
			},
			want: "--tr-svc-id must not be empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, tc.args, tc.want)
		})
	}
}

// CM2: both address families are accepted with a literal of their own kind,
// and the port bounds are inclusive.
func TestEndpointAcceptances(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adrFam  string
		trAddr  string
		trSvcId string
	}{
		{name: "ipv4", adrFam: "ipv4", trAddr: "192.168.0.10",
			trSvcId: "8009"},
		{name: "ipv4 any", adrFam: "ipv4", trAddr: "0.0.0.0",
			trSvcId: "1"},
		{name: "ipv6", adrFam: "ipv6", trAddr: "fd00::1",
			trSvcId: "65535"},
		{name: "ipv6 loopback", adrFam: "ipv6", trAddr: "::1",
			trSvcId: "4420"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := mustOptions(t, []string{
				"--etcd-endpoints", "127.0.0.1:2379",
				"--adr-fam", tc.adrFam,
				"--tr-addr", tc.trAddr,
				"--tr-svc-id", tc.trSvcId,
			})
			if opts.adrFam != tc.adrFam || opts.trAddr != tc.trAddr ||
				opts.trSvcId != tc.trSvcId {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					opts.adrFam, opts.trAddr, opts.trSvcId,
					tc.adrFam, tc.trAddr, tc.trSvcId)
			}
		})
	}
}

// EU5: a non-positive dial timeout is passed through to etcdutil.New, which
// substitutes the default; it is not a configuration error.
func TestZeroDialTimeoutIsPassedThrough(t *testing.T) {
	opts := mustOptions(t, append(append([]string{}, minArgs...),
		"--etcd-dial-timeout", "0"))
	if opts.dialTimeout != 0 {
		t.Errorf("dial timeout = %v, want 0 (etcdutil substitutes the default)",
			opts.dialTimeout)
	}
}

// ---------------------------------------------------------------------------
// CM2/CM3 — the happy path and the cdc.Config it builds
// ---------------------------------------------------------------------------

// CM2/CM3: the §6 example parses, every value arrives through viper, and the
// options become exactly the cdc.Config of the CM1 table — the endpoints
// travel along for the `cdc starting` record (LG, CM4).
func TestExampleInvocationBuildsCdcConfig(t *testing.T) {
	opts := mustOptions(t, exampleArgs)
	got := cdc.Config{
		Ranges:    opts.ranges,
		TrType:    opts.trType,
		AdrFam:    opts.adrFam,
		TrAddr:    opts.trAddr,
		TrSvcId:   opts.trSvcId,
		Endpoints: opts.endpoints,
	}
	want := cdc.Config{
		Ranges:  []uint32{0, 1, 2, 3, 4, 5, 6, 7},
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "192.168.0.10",
		TrSvcId: "8009",
		Endpoints: []string{
			"192.168.0.10:2379", "192.168.0.11:2379",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cdc.Config = %+v, want %+v", got, want)
	}
	// CM1 owns no rescan override: WV5's cadence is the package default.
	if got.RescanInterval != 0 {
		t.Errorf("RescanInterval = %v, want 0 (only the §8 tests set it)",
			got.RescanInterval)
	}
}

// captureRecords redirects the default slog logger into a buffer for the
// duration of one test and restores it afterwards, so the CM4 records cdc.Run
// emits can be read back. The tests of this package never run concurrently,
// so a plain buffer is safe.
func captureRecords(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// reservePort binds an ephemeral loopback port and holds it for the test, so
// a second bind of that exact address is guaranteed to fail. Nothing here
// hard-codes a port number.
func reservePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", ln.Addr(), err)
	}
	return port
}

// startingRecord is the `cdc starting` record of §7 as CM3 hands its fields
// over: every value below comes from the cdc.Config run built.
type startingRecord struct {
	Msg       string   `json:"msg"`
	Ranges    []string `json:"ranges"`
	Endpoints []string `json:"endpoints"`
	TrAddr    string   `json:"tr_addr"`
	TrSvcId   string   `json:"tr_svc_id"`
}

// CM3/CM4: run itself binds viper, validates and hands the built cdc.Config
// to cdc.Run — the fields must arrive in the right places, which asserting on
// the options struct alone cannot prove. The listen port is one this test
// already holds, so the hand-off fails at bind and run returns instead of
// serving forever; `cdc starting` is emitted first, before that bind, and
// carries the ranges, the etcd endpoints and the listen endpoint.
func TestRunHandsTheOptionsToCdcRun(t *testing.T) {
	port := reservePort(t)
	buf := captureRecords(t)
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newRootCmd()
	args := []string{
		"--etcd-endpoints", "192.168.0.10:2379,192.168.0.11:2379",
		"--range", "3,1",
		"--tr-addr", "127.0.0.1",
		"--tr-svc-id", port,
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	// run is called on its own goroutine only so that a bind which somehow
	// succeeds fails this test instead of serving until the suite times out;
	// the channel receive orders every write run made before its return.
	errCh := make(chan error, 1)
	go func() { errCh <- run(cmd, nil) }()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return: the reserved port was not in use")
	}
	if err == nil {
		t.Fatal("run returned nil though its listen address was taken")
	}
	if want := "127.0.0.1:" + port; !strings.Contains(err.Error(), want) {
		t.Errorf("run failed with %q, want the listen endpoint %q", err, want)
	}

	line, _, _ := strings.Cut(buf.String(), "\n")
	var got startingRecord
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("first record %q: %v", line, err)
	}
	want := startingRecord{
		Msg:    "cdc starting",
		Ranges: []string{"3", "1"},
		Endpoints: []string{
			"192.168.0.10:2379", "192.168.0.11:2379",
		},
		TrAddr:  "127.0.0.1",
		TrSvcId: port,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("first record = %+v, want %+v", got, want)
	}
}

// CM2: run refuses to start on a rejected invocation — it returns the
// validation error and never reaches cdc.Run, so no `cdc starting` record is
// written and no socket is opened.
func TestRunRefusesToStartOnAnInvalidInvocation(t *testing.T) {
	buf := captureRecords(t)
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newRootCmd()
	if err := cmd.ParseFlags([]string{
		"--etcd-endpoints", "127.0.0.1:2379",
		"--tr-addr", "127.0.0.1",
		"--tr-type", "rdma",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	err := run(cmd, nil)
	if err == nil {
		t.Fatal("run accepted --tr-type rdma")
	}
	if want := `--tr-type must be "tcp"`; !strings.Contains(err.Error(), want) {
		t.Errorf("run failed with %q, want %q", err, want)
	}
	if strings.Contains(buf.String(), "cdc starting") {
		t.Errorf("a rejected invocation still started cdc: %s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// CM1 — environment, config file and precedence
// ---------------------------------------------------------------------------

// CM1: DNV_CDC_RANGE is honoured, and an explicit --range still wins.
func TestRangeFromEnvironment(t *testing.T) {
	t.Setenv("DNV_CDC_RANGE", "8,9,a")
	opts := mustOptions(t, minArgs)
	if want := []uint32{8, 9, 10}; !reflect.DeepEqual(opts.ranges, want) {
		t.Errorf("ranges = %v, want %v from the environment", opts.ranges, want)
	}

	opts = mustOptions(t, append(append([]string{}, minArgs...),
		"--range", "0,1"))
	if want := []uint32{0, 1}; !reflect.DeepEqual(opts.ranges, want) {
		t.Errorf("ranges = %v, want %v: a flag beats the environment",
			opts.ranges, want)
	}
}

// CM1: every value reaches viper from the environment too, including the
// required --etcd-endpoints and --tr-addr (checked after binding, never by
// cobra).
func TestEnvironmentSuppliesEveryValue(t *testing.T) {
	t.Setenv("DNV_CDC_ETCD_ENDPOINTS", "10.0.0.1:2379,10.0.0.2:2379")
	t.Setenv("DNV_CDC_ETCD_DIAL_TIMEOUT", "2")
	t.Setenv("DNV_CDC_RANGE", "c,d")
	t.Setenv("DNV_CDC_TR_TYPE", "tcp")
	t.Setenv("DNV_CDC_ADR_FAM", "ipv6")
	t.Setenv("DNV_CDC_TR_ADDR", "fd00::2")
	t.Setenv("DNV_CDC_TR_SVC_ID", "4420")
	opts := mustOptions(t, nil)
	if want := []string{"10.0.0.1:2379", "10.0.0.2:2379"}; !reflect.DeepEqual(
		opts.endpoints, want) {
		t.Errorf("endpoints = %v, want %v", opts.endpoints, want)
	}
	if opts.dialTimeout != 2*time.Second {
		t.Errorf("etcd-dial-timeout = %v, want 2s", opts.dialTimeout)
	}
	if want := []uint32{12, 13}; !reflect.DeepEqual(opts.ranges, want) {
		t.Errorf("ranges = %v, want %v", opts.ranges, want)
	}
	if opts.trType != "tcp" || opts.adrFam != "ipv6" ||
		opts.trAddr != "fd00::2" || opts.trSvcId != "4420" {
		t.Errorf("endpoint = (%q, %q, %q, %q), want (tcp, ipv6, fd00::2, 4420)",
			opts.trType, opts.adrFam, opts.trAddr, opts.trSvcId)
	}
}

// CM1: a flag beats the environment for every flag of the table, not only
// --range.
func TestFlagBeatsEnvironment(t *testing.T) {
	t.Setenv("DNV_CDC_ETCD_ENDPOINTS", "10.0.0.1:2379")
	t.Setenv("DNV_CDC_ETCD_DIAL_TIMEOUT", "2")
	t.Setenv("DNV_CDC_ADR_FAM", "ipv6")
	t.Setenv("DNV_CDC_TR_ADDR", "fd00::2")
	t.Setenv("DNV_CDC_TR_SVC_ID", "4420")
	opts := mustOptions(t, []string{
		"--etcd-endpoints", "10.9.9.9:2379",
		"--etcd-dial-timeout", "7",
		"--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.10",
		"--tr-svc-id", "8009",
	})
	if want := []string{"10.9.9.9:2379"}; !reflect.DeepEqual(
		opts.endpoints, want) {
		t.Errorf("endpoints = %v, want %v", opts.endpoints, want)
	}
	if opts.dialTimeout != 7*time.Second {
		t.Errorf("etcd-dial-timeout = %v, want 7s", opts.dialTimeout)
	}
	if opts.adrFam != "ipv4" || opts.trAddr != "192.168.0.10" ||
		opts.trSvcId != "8009" {
		t.Errorf("endpoint = (%q, %q, %q), want (ipv4, 192.168.0.10, 8009)",
			opts.adrFam, opts.trAddr, opts.trSvcId)
	}
}

// CM1: --config is read, and a file value satisfies a required flag and
// overrides a flag default just as an explicit flag does.
func TestConfigFile(t *testing.T) {
	path := t.TempDir() + "/cdc.yaml"
	body := "etcd-endpoints: 10.1.2.3:2379\n" +
		"tr-addr: 10.1.2.3\n" +
		"range: 4,5\n" +
		"tr-svc-id: \"4420\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	opts := mustOptions(t, []string{"--config", path})
	if want := []string{"10.1.2.3:2379"}; !reflect.DeepEqual(
		opts.endpoints, want) {
		t.Errorf("endpoints = %v, want %v", opts.endpoints, want)
	}
	if opts.trAddr != "10.1.2.3" {
		t.Errorf("tr-addr = %q, want 10.1.2.3 from the file", opts.trAddr)
	}
	if want := []uint32{4, 5}; !reflect.DeepEqual(opts.ranges, want) {
		t.Errorf("ranges = %v, want %v from the file", opts.ranges, want)
	}
	if opts.trSvcId != "4420" {
		t.Errorf("tr-svc-id = %q, want 4420 from the file", opts.trSvcId)
	}
}

// CM1: the precedence is flag > environment > config file > default.
func TestConfigFilePrecedence(t *testing.T) {
	path := t.TempDir() + "/cdc.yaml"
	body := "etcd-endpoints: 10.1.2.3:2379\n" +
		"tr-addr: 10.1.2.3\n" +
		"range: 4,5\n" +
		"tr-svc-id: \"4420\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("DNV_CDC_RANGE", "6,7")
	opts := mustOptions(t, []string{
		"--config", path, "--tr-svc-id", "9009",
	})
	if want := []uint32{6, 7}; !reflect.DeepEqual(opts.ranges, want) {
		t.Errorf("ranges = %v, want %v: the environment beats the file",
			opts.ranges, want)
	}
	if opts.trSvcId != "9009" {
		t.Errorf("tr-svc-id = %q, want 9009: the flag beats the file",
			opts.trSvcId)
	}
	if opts.trAddr != "10.1.2.3" {
		t.Errorf("tr-addr = %q, want 10.1.2.3: the file beats the default",
			opts.trAddr)
	}
}

// CM1: a --config path that does not exist is a startup error, not a silent
// fallback to the defaults.
func TestMissingConfigFileIsAnError(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newRootCmd()
	args := []string{"--config", t.TempDir() + "/absent.yaml"}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if err := bindViper(cmd); err == nil {
		t.Error("bindViper accepted a --config path that does not exist")
	}
}

// CM2: a value that arrives from the environment is validated exactly like a
// flag value.
func TestEnvironmentValueIsValidated(t *testing.T) {
	t.Setenv("DNV_CDC_RANGE", "0,0")
	wantError(t, minArgs, `--range: duplicate digit "0"`)
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
