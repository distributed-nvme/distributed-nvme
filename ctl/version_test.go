package ctl

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestVersionFlag pins dnvctl's --version (architecture.md, Components:
// invocation reference; dnvctl.md CT4, CT5): the root prints "dnvctl" and
// the build version on stdout, exits 0 and dials nothing; a write of that
// line that fails exits 2 with two lines on stderr; the flag has no
// shorthand, and a group or a leaf refuses it as a usage error.
func TestVersionFlag(t *testing.T) {
	noDial := func(context.Context, string) (pb.GatewayClient, func() error, error) {
		t.Error("--version dialed the gateway")
		return nil, nil, context.Canceled
	}
	res := runCLIWithDial(t, noDial, "--version")
	if want := "dnvctl " + common.BuildVersion() + "\n"; res.code != 0 ||
		res.stdout != want || res.stderr != "" {
		t.Errorf("--version: exit %d, stdout %q, stderr %q; want 0, %q, nothing",
			res.code, res.stdout, res.stderr, want)
	}
	if NewRootCmd().Flags().ShorthandLookup("v") != nil {
		t.Error("--version has the shorthand -v")
	}
	// cobra checks the version line's write, unlike help's: a write that
	// fails prints cobra's line of the error, then dnvctl's, and is CT5's
	// exit 2, no RPC having been issued.
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	res = runCLIOnStdout(t, noDial, full, "--version")
	lines := strings.Split(strings.TrimSuffix(res.stderr, "\n"), "\n")
	if res.code != 2 || len(lines) != 2 || !strings.HasPrefix(lines[1], "dnvctl: ") ||
		!strings.Contains(lines[0], "no space left on device") {
		t.Errorf("--version onto a full device: exit %d, stderr %q; want 2, "+
			"cobra's line of the error, then dnvctl's", res.code, res.stderr)
	}
	for _, argv := range [][]string{{"cluster", "--version"}, {"cluster", "get", "--version"}} {
		res := runCLIWithDial(t, noDial, argv...)
		if res.code != 2 || res.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q; want the usage error 2 and nothing",
				argv, res.code, res.stdout)
		}
	}
}
