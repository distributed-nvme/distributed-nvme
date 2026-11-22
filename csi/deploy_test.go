package csi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The shipped modules-load file of a host, deploy/modules-load.d/dnv-host.conf,
// names the one module that the node plugin's start check tells a node with
// no NVMe over Fabrics device to load and to name in a modules-load file
// (csi.md CSI13), and no other.
func TestShippedHostModulesLoadMatchesTheFix(t *testing.T) {
	f := newFakeNode()
	delete(f.files, fabricsDevice)
	err := checkHostKernel(t.Context(), f.client())
	if err == nil {
		t.Fatal("checkHostKernel took a node with no fabrics device")
	}
	m := regexp.MustCompile("run `modprobe ([a-z0-9_-]+)`").
		FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("the fix names no module to load: %v", err)
	}
	want := strings.ReplaceAll(m[1], "-", "_")
	path := filepath.Join("..", "deploy", "modules-load.d", "dnv-host.conf")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		got = append(got, strings.ReplaceAll(strings.Fields(line)[0], "-", "_"))
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("dnv-host.conf names %v, want only %s, the module the "+
			"start check's fix names", got, want)
	}
}
