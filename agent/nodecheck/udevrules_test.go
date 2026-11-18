package nodecheck

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// deployUdevDir is where the shipped rules live in the tree.
var deployUdevDir = filepath.Join("..", "..", "deploy", "udev")

// Each copy of a shipped rule is its file under deploy/udev/, byte for
// byte, and every file there has its copy.
func TestShippedRulesMatchDeploy(t *testing.T) {
	for name, text := range shippedRules {
		data, err := os.ReadFile(filepath.Join(deployUdevDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(data) != text {
			t.Errorf("%s: the copy in udevrules.go is not the file's text",
				name)
		}
	}
	entries, err := os.ReadDir(deployUdevDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, ok := shippedRules[entry.Name()]; !ok {
			t.Errorf("deploy/udev/%s has no copy in udevrules.go",
				entry.Name())
		}
	}
}

// The shipped md rules match a dnv array's name in its exact form, bare and
// after a homehost, and leave alone a node's own array named after a host
// called dnv-..., which mdadm stores as "dnv-cn0:0": the mask would mark its
// members not ready, and the array would not start at boot.
func TestShippedMdRulesMatchDnvNamesExactly(t *testing.T) {
	nameMatch := regexp.MustCompile(`ENV\{MD_NAME\}[!=]="([^"]*)"`)
	dnvName := common.NewNameFmt("").CnMdArrayName(0x3f1, 0, 0, false)
	for _, name := range []string{mdMaskRuleName, mdArrayRuleName} {
		m := nameMatch.FindStringSubmatch(shippedRules[name])
		if m == nil {
			t.Errorf("%s: no MD_NAME match", name)
			continue
		}
		if m[1] != common.CnMdArrayNameGlob {
			t.Errorf("%s matches MD_NAME by %q, not %q", name, m[1],
				common.CnMdArrayNameGlob)
		}
		for _, v := range []string{dnvName, "any:" + dnvName} {
			if !globMatch(m[1], v) {
				t.Errorf("%s: %q does not match %q", name, m[1], v)
			}
		}
		for _, v := range []string{"dnv-cn0:0", "dnv-cn0:root"} {
			if globMatch(m[1], v) {
				t.Errorf("%s: %q matches a node's own array %q", name,
					m[1], v)
			}
		}
	}
}
