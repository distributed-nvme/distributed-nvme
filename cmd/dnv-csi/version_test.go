package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TestVersionFlag pins --version (architecture.md, Components: invocation
// reference): the root prints its name and the build version on stdout,
// nothing on stderr, and returns no error, before it binds a flag or runs;
// the flag has no shorthand. A subcommand refuses it.
func TestVersionFlag(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if want := "dnv-csi " + common.BuildVersion() + "\n"; out.String() != want {
		t.Errorf("--version prints %q, want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("--version writes %q on stderr", errOut.String())
	}
	if root.Flags().ShorthandLookup("v") != nil {
		t.Error("--version has the shorthand -v")
	}
	// Never inherited: a subcommand refuses the flag, so it never starts
	// what the subcommand starts.
	for _, sub := range []string{"controller", "node"} {
		viper.Reset()
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{sub, "--version"})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "unknown flag: --version") {
			t.Errorf("%s --version: %v, want the unknown flag refused", sub, err)
		}
	}
}
