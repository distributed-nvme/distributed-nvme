// harness_test.go is the shared machinery of the dnvctl.md §6 unit tests
// (CT-T1…CT-T5): the recording client, the argv driver, and the output
// capture every one of them needs.
//
// The one rule that shapes this file is that dnvctl's configuration lives in
// a GLOBAL: viper is a process-wide singleton, bindViper binds the invoked
// command's whole flag set into it, and AutomaticEnv reads the live
// environment. A flag bound by one test is therefore visible to the next one
// unless the singleton is cleared, and a test that passes because of a
// leftover binding is a lie rather than a pass. So every entry point here
// resets viper before AND after the run (runCLI, through resetViper), no test
// may reach the cobra tree by any other path, and `go test -count=3 ./ctl/`
// is what proves the isolation holds when the same test runs repeatedly in
// one process.
package ctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestMain clears the DNVCTL_* environment before any test runs. The CLI
// reads every value through viper's AutomaticEnv (CT9), so a developer with
// DNVCTL_CLUSTER or DNVCTL_GATEWAY_ADDRESS exported in their shell would
// otherwise get a different request out of every table row — and on the
// machine where the variable happens to hold the value a row expects, a green
// run that proves nothing. The prefix comes from root.go's own envPrefix so
// the scrub cannot drift from what bindViper binds.
//
// Tests that WANT an ambient variable set it themselves with t.Setenv, which
// runs after this and is undone at the end of that test.
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, envPrefix+"_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// The recording client (CT-T2)
// ---------------------------------------------------------------------------

// recordingClient implements pb.GatewayClient for the tests. It embeds the
// interface — the gatewayctl precedent — but every one of the 59 methods is
// overridden below, so the embedded nil is unreachable today and the panic a
// mis-driven test gets is rpcCall's named one rather than a bare nil
// dereference.
//
// `want` is what makes a wrong RPC loud: a command that drives a method other
// than the one the row names PANICS instead of quietly recording a request
// nobody asserts on. `calls` counts every method entry, including the
// mismatched one, so CT-T4 can prove that a usage error issued NO RPC at all
// rather than merely returning the right exit code.
type recordingClient struct {
	pb.GatewayClient

	// want is the only RPC this client may be asked for; empty accepts any.
	want string
	// method is the RPC that was actually driven.
	method string
	// req is the request the driven RPC received.
	req proto.Message
	// calls counts every RPC entry.
	calls int
	// err, when set, is returned instead of a reply (CT-T4).
	err error
	// reply, when set, is returned instead of the empty canned reply. Its
	// type must be the driven method's reply type (CT-T3).
	reply proto.Message
}

// rpcCall is the body every one of the 59 methods shares.
func rpcCall[Req proto.Message, Reply proto.Message](
	c *recordingClient, name string, in Req, canned Reply,
) (Reply, error) {
	c.calls++
	if c.want != "" && name != c.want {
		panic(fmt.Sprintf(
			"dnvctl drove %s, but this test drives %s", name, c.want))
	}
	c.method = name
	c.req = in
	var zero Reply
	if c.err != nil {
		return zero, c.err
	}
	if c.reply != nil {
		typed, ok := c.reply.(Reply)
		if !ok {
			panic(fmt.Sprintf(
				"injected reply %T is not %s's reply type %T",
				c.reply, name, canned))
		}
		return typed, nil
	}
	return canned, nil
}

// ---------------------------------------------------------------------------
// Driving the real command tree
// ---------------------------------------------------------------------------

// cliResult is one whole invocation: the process exit code Execute would hand
// os.Exit, plus the two streams §3.2 makes promises about.
type cliResult struct {
	code   int
	stdout string
	stderr string
}

// resetViper clears the global singleton now and again when the test ends.
// Both halves matter: the "now" protects this test from whatever ran before
// it in the same process, and the "later" protects the next test from this
// one.
func resetViper(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}

// runCLI runs one complete dnvctl invocation through Execute — the real entry
// point cmd/dnvctl/main.go calls — with `client` behind the dialer seam.
//
// argv is the full command line WITHOUT the program name. A nil client makes
// the dial itself fail, which is how a test proves a command never got as far
// as dialing.
func runCLI(t *testing.T, client pb.GatewayClient, argv ...string) cliResult {
	t.Helper()
	return runCLIWithDial(t, func(_ context.Context, _ string) (
		pb.GatewayClient, func() error, error,
	) {
		if client == nil {
			return nil, nil, fmt.Errorf("no client in this test")
		}
		return client, func() error { return nil }, nil
	}, argv...)
}

// runCLIWithDial is runCLI with the dial seam itself supplied by the caller:
// CT-T4 needs a dial that FAILS (the exit-1 connection branch) and CT-T5
// needs one that reaches a real gRPC server.
func runCLIWithDial(t *testing.T, d dialFunc, argv ...string) cliResult {
	t.Helper()
	resetViper(t)

	prevDialer := dialer
	dialer = d
	t.Cleanup(func() { dialer = prevDialer })

	prevArgs := os.Args
	os.Args = append([]string{"dnvctl"}, argv...)
	t.Cleanup(func() { os.Args = prevArgs })

	var res cliResult
	res.stdout, res.stderr = captureOutput(t, func() { res.code = Execute() })
	return res
}

// gatewayAddress is any dialable-looking address: the dialer seam never
// resolves it, and CT-T5 is the one test that needs a real one.
const gatewayAddress = "127.0.0.1:29840"

// The §7.6 identity plan, so the unit tests and the integration suite assert
// the same requests.
const (
	itCluster = "itctl"
	itSp      = "sp0"
)

// globalArgv prepends the §7.6 global prefix that every sweep step uses.
func globalArgv(argv ...string) []string {
	return append([]string{
		"--gateway-address", gatewayAddress,
		"--cluster", itCluster,
		"--sp", itSp,
	}, argv...)
}

// runArgv is CT-T2's driver: it parses argv (after the §7.6 global prefix)
// through the real cobra tree and returns the request the named RPC received.
// Anything that is not a clean exit-0 invocation of exactly that RPC fails the
// test here, so a row's assertions never run against a request that was not
// actually sent.
func runArgv(t *testing.T, rpc string, argv ...string) proto.Message {
	t.Helper()
	return runArgvFull(t, rpc, globalArgv(argv...)...)
}

// runArgvFull is runArgv without the global prefix, for the tests that own
// the globals themselves — the §2.1 env precedence cases, and the ones that
// prove a command works with a global left empty.
func runArgvFull(t *testing.T, rpc string, argv ...string) proto.Message {
	t.Helper()
	client := &recordingClient{want: rpc}
	res := runCLI(t, client, argv...)
	if res.code != 0 {
		t.Fatalf("dnvctl %v exited %d, stderr %q",
			argv, res.code, res.stderr)
	}
	if client.calls != 1 {
		t.Fatalf("dnvctl %v issued %d RPCs, want exactly 1",
			argv, client.calls)
	}
	if client.method != rpc {
		t.Fatalf("dnvctl %v drove %s, want %s", argv, client.method, rpc)
	}
	if client.req == nil {
		t.Fatalf("dnvctl %v recorded no request", argv)
	}
	return client.req
}

// wantRequest compares a captured request against the expected proto FIELD BY
// FIELD — proto.Equal walks every field of both, so a row that forgets to
// mention a field is still asserting that the field is at its default.
func wantRequest(t *testing.T, got, want proto.Message) {
	t.Helper()
	if proto.Equal(got, want) {
		return
	}
	t.Errorf("request mismatch\n got: %s\nwant: %s",
		prototext.MarshalOptions{Multiline: false}.Format(got),
		prototext.MarshalOptions{Multiline: false}.Format(want))
}

// TestRecordingClientGuardsTheMethod is the harness's own test. The "anything
// else panics" rule of CT-T2 is what stops a row from passing while dnvctl
// drives some OTHER RPC — the wrong request would simply never be compared —
// so the guard has to be known to fire rather than assumed to.
func TestRecordingClientGuardsTheMethod(t *testing.T) {
	client := &recordingClient{want: "ListClusters"}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Errorf("driving GetCluster on a ListClusters recorder did " +
					"not panic")
			}
		}()
		_, _ = client.GetCluster(
			context.Background(), &pb.GetClusterRequest{})
	}()
	// The mis-driven call is still counted, so CT-T4's "no RPC was issued"
	// assertions cannot be fooled by one.
	if client.calls != 1 {
		t.Errorf("calls = %d after a mis-driven RPC, want 1", client.calls)
	}

	// An empty `want` accepts anything, which is what the CT-T4 cases that
	// only count calls rely on.
	open := &recordingClient{}
	if _, err := open.GetCluster(
		context.Background(), &pb.GetClusterRequest{ClusterName: "c"},
	); err != nil {
		t.Errorf("an unbound recorder returned %v", err)
	}
	if open.method != "GetCluster" || open.calls != 1 {
		t.Errorf("an unbound recorder recorded %q/%d, want GetCluster/1",
			open.method, open.calls)
	}
}

// ---------------------------------------------------------------------------
// Output capture
// ---------------------------------------------------------------------------

// captureOutput redirects the process's stdout and stderr around fn. The two
// streams are the CLI's contract (§3.2), and emit writes to os.Stdout
// directly, so a pipe over the file descriptors is the only faithful way to
// read what an operator would see.
//
// Each pipe is drained by a goroutine, so a document larger than the pipe
// buffer cannot deadlock the run.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	outCh := drain(outR)
	errCh := drain(errR)

	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = prevOut, prevErr
		_ = outW.Close()
		_ = errW.Close()
		stdout, stderr = <-outCh, <-errCh
	}()
	fn()
	return
}

// drain reads one pipe end to EOF in the background.
func drain(r *os.File) <-chan string {
	ch := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		_ = r.Close()
		ch <- string(data)
	}()
	return ch
}
