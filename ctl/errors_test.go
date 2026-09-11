// CT-T4 — the error surface (dnvctl.md §6, CT5/§3.2).
//
// §3.2 is a three-row table and every row is a promise an operator's script
// depends on:
//
//	0  the §3.1 document on stdout, stderr empty
//	1  RPC or connection failure: stdout EMPTY, one line on stderr
//	2  usage error: stdout empty, and NO RPC WAS ISSUED
//
// The third row is the one a careless test gets wrong. Asserting the exit
// code alone proves nothing about whether a request went out — a command that
// dialed, sent, got an answer and then failed to parse a flag would exit 2
// just the same. So every usage case here asserts the recording client's CALL
// COUNT, which is the only direct evidence that nothing reached the gateway.
package ctl

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAbortedStaleRevision is §3.2's worked example and §7.12's step c2: the
// failure an operator meets when their --rev is stale. The whole line is
// asserted, not a substring, because each part of it is load-bearing — the
// `dnvctl: ` prefix marks the line as the CLI's own, the UPPER_SNAKE code is
// what a script greps for, the message is the gateway's verbatim, and the
// trace id is the jq key for the gateway and agent logs.
func TestAbortedStaleRevision(t *testing.T) {
	client := &recordingClient{
		want: "DeleteThinDevice",
		err:  status.Error(codes.Aborted, "stale revision"),
	}
	res := runCLI(t, client, globalArgv(
		"--trace-id", "it-errors-2",
		"td", "delete", "--name", "t0", "--rev", "7")...)

	if res.code != 1 {
		t.Errorf("exit code = %d, want 1", res.code)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
	want := "dnvctl: ABORTED: stale revision (trace_id it-errors-2)\n"
	if res.stderr != want {
		t.Errorf("stderr\n got: %q\nwant: %q", res.stderr, want)
	}
	// The failure is reported for an RPC that really was issued.
	if client.calls != 1 {
		t.Errorf("the client saw %d calls, want 1", client.calls)
	}
}

// TestRpcFailureLines walks the codes an operator actually meets (§7.12's
// four injections plus the two transport ones) through the same line.
func TestRpcFailureLines(t *testing.T) {
	cases := []struct {
		code    codes.Code
		message string
		want    string
	}{
		{codes.NotFound, "sp sp0 not found", "NOT_FOUND: sp sp0 not found"},
		{codes.AlreadyExists, "cluster c1 exists",
			"ALREADY_EXISTS: cluster c1 exists"},
		{codes.InvalidArgument, "cntlr_cnt must not be 0",
			"INVALID_ARGUMENT: cntlr_cnt must not be 0"},
		{codes.FailedPrecondition, "cntlr is primary",
			"FAILED_PRECONDITION: cntlr is primary"},
		{codes.DeadlineExceeded, "context deadline exceeded",
			"DEADLINE_EXCEEDED: context deadline exceeded"},
		{codes.Unavailable, "connection refused",
			"UNAVAILABLE: connection refused"},
		{codes.ResourceExhausted, "no spare slot",
			"RESOURCE_EXHAUSTED: no spare slot"},
		// An empty message still produces a well-formed line rather than a
		// truncated one.
		{codes.Internal, "", "INTERNAL: "},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			client := &recordingClient{
				want: "GetStoragePool",
				err:  status.Error(tc.code, tc.message),
			}
			res := runCLI(t, client, globalArgv(
				"--trace-id", "it-errors-1", "sp", "get")...)
			if res.code != 1 {
				t.Errorf("exit code = %d, want 1", res.code)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			want := "dnvctl: " + tc.want + " (trace_id it-errors-1)\n"
			if res.stderr != want {
				t.Errorf("stderr\n got: %q\nwant: %q", res.stderr, want)
			}
		})
	}
}

// TestDialFailureIsExitOne is §7.13's d1 in miniature: a connection that
// cannot be made is an RPC failure (exit 1), not a usage error, and dnvctl
// adds no translation layer over the code gRPC reports.
func TestDialFailureIsExitOne(t *testing.T) {
	t.Run("status error", func(t *testing.T) {
		res := runCLIWithDial(t, failingDial(
			status.Error(codes.Unavailable, "connection refused")),
			globalArgv("--trace-id", "it-transport-1", "cluster", "list")...)
		if res.code != 1 {
			t.Errorf("exit code = %d, want 1", res.code)
		}
		want := "dnvctl: UNAVAILABLE: connection refused " +
			"(trace_id it-transport-1)\n"
		if res.stderr != want {
			t.Errorf("stderr\n got: %q\nwant: %q", res.stderr, want)
		}
		if res.stdout != "" {
			t.Errorf("stdout = %q, want empty", res.stdout)
		}
	})

	// A dial error that is not a gRPC status still exits 1 — the class is
	// "the RPC or the connection failed", not "the error had a code" — and
	// renders as UNKNOWN with the error's own text.
	t.Run("plain error", func(t *testing.T) {
		res := runCLIWithDial(t, failingDial(fmt.Errorf("bad address")),
			globalArgv("--trace-id", "it-transport-2", "cluster", "list")...)
		if res.code != 1 {
			t.Errorf("exit code = %d, want 1", res.code)
		}
		want := "dnvctl: UNKNOWN: bad address (trace_id it-transport-2)\n"
		if res.stderr != want {
			t.Errorf("stderr\n got: %q\nwant: %q", res.stderr, want)
		}
	})
}

// failingDial is a dial seam that always fails.
func failingDial(err error) dialFunc {
	return func(_ context.Context, _ string) (
		pb.GatewayClient, func() error, error,
	) {
		return nil, nil, err
	}
}

// TestUsageErrorsIssueNoRpc is §3.2's third row and §7.12's c5/c6. Each case
// asserts the call count, not just the exit code: the count is the only thing
// that distinguishes "rejected before dialing" from "sent, then rejected".
func TestUsageErrorsIssueNoRpc(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"unknown flag",
			[]string{"td", "create", "--no-such-flag"}},
		// An unknown VERB under a known group has its own test below: it is
		// the one row of §3.2 the implementation does not satisfy.
		{"unknown group",
			[]string{"nosuchgroup", "list"}},
		{"positional argument",
			[]string{"td", "list", "stray"}},
		// The §5.0 booleans: `--enabled false` leaves `false` as a
		// positional argument, which cobra.NoArgs rejects. That is why the
		// spec insists on the `=` spelling.
		{"bool without =",
			[]string{"cntlr", "set-enabled", "--id", "3", "--enabled",
				"false"}},
		{"malformed --rev",
			[]string{"td", "create", "--name", "t0", "--rev", "zz"}},
		{"negative --rev",
			[]string{"td", "create", "--name", "t0", "--rev", "-1"}},
		{"malformed --bm-hex",
			[]string{"clone", "append-bm", "--name", "cl0",
				"--bm-hex", "zz", "--rev", "7"}},
		{"odd-length --bm-hex",
			[]string{"migr", "append-bm", "--name", "m0", "--bm-hex", "a5a"}},
		{"malformed --slots",
			[]string{"sp", "set-cntlid-slots", "--slots", "0,x"}},
		{"out-of-range --slots",
			[]string{"sp", "set-cntlid-slots", "--slots", "4294967296"}},
		{"malformed --ids",
			[]string{"sp", "find-names", "--ids", "nope"}},
		{"malformed --id",
			[]string{"cntlr", "delete", "--id", "cntlr0"}},
		{"malformed --grp",
			[]string{"spare", "create", "--grp", "grp0"}},
		{"malformed --level",
			[]string{"sp", "set-level", "--level", "bogus"}},
		{"malformed --redund",
			[]string{"sp", "create", "--redund", "raid5"}},
		{"malformed --count",
			[]string{"cluster", "list", "--count", "-1"}},
		{"malformed --timeout",
			[]string{"cluster", "list", "--timeout", "soon"}},
		// --gateway-address has no default, and its absence is a usage error
		// rather than a dial to nowhere.
		{"missing --gateway-address", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingClient{}
			argv := globalArgv(tc.argv...)
			if tc.argv == nil {
				argv = []string{"--cluster", itCluster, "cluster", "list"}
			}
			res := runCLI(t, client, argv...)
			if res.code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %q)",
					res.code, res.stderr)
			}
			if client.calls != 0 {
				t.Errorf("%d RPCs were issued, want 0 — a usage error must "+
					"never reach the gateway", client.calls)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			if res.stderr == "" {
				t.Errorf("stderr is empty, want cobra's message")
			}
		})
	}
}

// TestUnknownVerbIsAUsageError pins §3.2's "unknown command" row on the shape
// an operator actually types it in: a typo'd verb under a real group.
//
// FAILING, and deliberately not weakened — this exposes a defect in
// ctl/root.go's group() helper. A group command has no Run/RunE, and cobra's
// (*Command).execute returns flag.ErrHelp for any command that is not
// Runnable BEFORE it ever calls ValidateArgs (cobra@v1.10.2 command.go:955,
// above the ValidateArgs call at :968). ExecuteC treats flag.ErrHelp as
// success — "always show help if requested, even if SilenceErrors is in
// effect", command.go:1152 — printing the group's help and returning nil,
// which Execute then maps to exit 0. So `dnvctl td lst`
// exits 0 with a page of help text on STDOUT — which breaks two promises at
// once, §3.2's "unknown command ⇒ exit 2, stdout empty, no RPC issued" and
// §3.1's "nothing else is ever printed to stdout". A script that mistypes a
// verb is told it succeeded, and `dnvctl td lst | jq .` fails on the help
// text rather than on the exit code.
//
// The group's `Args: cobra.NoArgs` does not save it: cobra reaches the
// not-Runnable branch first, and Find's legacyArgs check only fires for the
// ROOT command (args.go:35, `!cmd.HasParent()`), which is why the sibling
// case `dnvctl nosuchgroup list` DOES exit 2.
//
// The same path makes a bare `dnvctl td` print help on stdout and exit 0.
// That one is arguably cobra's stock help behaviour (§0 #3 permits `help`),
// so it is described here rather than asserted; the typo'd verb is not
// arguable.
func TestUnknownVerbIsAUsageError(t *testing.T) {
	for _, argv := range [][]string{
		{"td", "no-such-verb"},
		{"cluster", "lst"},
		{"sp", "grow"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			client := &recordingClient{}
			res := runCLI(t, client, globalArgv(argv...)...)
			if res.code != 2 {
				t.Errorf("exit code = %d, want 2", res.code)
			}
			if res.stdout != "" {
				t.Errorf("stdout got %d bytes, want empty",
					len(res.stdout))
			}
			if client.calls != 0 {
				t.Errorf("%d RPCs were issued, want 0", client.calls)
			}
		})
	}
}

// TestCodeNamesCoverEveryCode is the table's own completeness check, done
// against gRPC's canonical spellings rather than against a second hand-written
// list: codes.Code.UnmarshalJSON accepts exactly the UPPER_SNAKE names the
// wire uses, so a name that round-trips back to its own code is provably the
// wire spelling. That is what catches the one entry nobody would think to
// question — Canceled's wire name is CANCELLED, with two Ls, which no
// mechanical CamelCase-to-UPPER_SNAKE rule would produce.
func TestCodeNamesCoverEveryCode(t *testing.T) {
	for code, name := range codeNames {
		var round codes.Code
		if err := round.UnmarshalJSON([]byte(`"` + name + `"`)); err != nil {
			t.Errorf("codeNames[%v] = %q, which gRPC does not know: %v",
				code, name, err)
			continue
		}
		if round != code {
			t.Errorf("codeNames[%v] = %q, which is gRPC's name for %v",
				code, name, round)
		}
		if name != strings.ToUpper(name) {
			t.Errorf("codeNames[%v] = %q, want UPPER_SNAKE", code, name)
		}
	}

	// Every code the linked gRPC declares must have a row. String() returns
	// the "Code(n)" placeholder for everything it does not declare, which is
	// how the loop finds the end of the enum without importing its private
	// _maxCode.
	declared := 0
	for i := 0; i < 256; i++ {
		code := codes.Code(i)
		if strings.HasPrefix(code.String(), "Code(") {
			continue
		}
		declared++
		if _, ok := codeNames[code]; !ok {
			t.Errorf("gRPC declares %v (%d), which codeNames does not spell",
				code, i)
		}
	}
	if len(codeNames) != declared {
		t.Errorf("codeNames has %d rows, but gRPC declares %d codes",
			len(codeNames), declared)
	}
}

// TestCodeNameFallback covers the branch a future gRPC would take: a code no
// version of the table knows still produces a usable line rather than an
// empty one.
func TestCodeNameFallback(t *testing.T) {
	if got := codeName(codes.Code(99)); got != "CODE_99" {
		t.Errorf("codeName(99) = %q, want CODE_99", got)
	}
	if got := codeName(codes.Aborted); got != "ABORTED" {
		t.Errorf("codeName(Aborted) = %q, want ABORTED", got)
	}
}

// mintedTraceId is the shape of common.NewTraceId's output: 8 random bytes as
// lowercase hex.
var mintedTraceId = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestFailureLineCarriesMintedTraceId closes the §2.3 loop for the failure
// path: with no --trace-id the line still hands the operator an id, and it is
// the minted one rather than an empty parenthesis. CT-T5 proves the same id
// is what the server saw.
func TestFailureLineCarriesMintedTraceId(t *testing.T) {
	line := regexp.MustCompile(
		`^dnvctl: NOT_FOUND: nope \(trace_id ([^)]*)\)\n$`)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		client := &recordingClient{
			want: "GetStoragePool",
			err:  status.Error(codes.NotFound, "nope"),
		}
		res := runCLI(t, client, globalArgv("sp", "get")...)
		match := line.FindStringSubmatch(res.stderr)
		if match == nil {
			t.Fatalf("stderr = %q, want the §3.2 line", res.stderr)
		}
		if !mintedTraceId.MatchString(match[1]) {
			t.Errorf("trace id %q is not a minted id", match[1])
		}
		seen[match[1]] = true
	}
	if len(seen) != 2 {
		t.Errorf("two invocations reported the same trace id %v", seen)
	}
}
