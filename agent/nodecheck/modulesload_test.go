package nodecheck

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// deployModulesLoadDir is where the shipped modules-load files live in the
// tree (architecture.md, Components: invocation reference).
var deployModulesLoadDir = filepath.Join("..", "..", "deploy", "modules-load.d")

// shippedModulesLoad are the shipped modules-load files of the node check's
// roles, by file name.
var shippedModulesLoad = map[string]string{
	"dnv-dn.conf": RoleDn,
	"dnv-cn.conf": RoleCn,
}

// hostModulesLoadName is the shipped modules-load file of a host, which the
// CSI driver's test holds to the node plugin's start check (csi.md CSI13).
const hostModulesLoadName = "dnv-host.conf"

// Each shipped modules-load file of a role names, read as the node check
// reads a modules-load file, every module the check lists for that role and
// no other (dnagent.md CM5), each once, in two runs of module lines that a
// comment or a blank line separates: first those that must be loaded before
// the agent starts, then those the kernel loads when the agent first needs
// them; and every file there is a role's or the host's. The test reads none
// of the files' comments.
func TestShippedModulesLoadMatchRoles(t *testing.T) {
	for name, role := range shippedModulesLoad {
		data, err := os.ReadFile(filepath.Join(deployModulesLoadDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		loadedNow, onDemand := (&checker{opts: CheckOptions{Role: role}}).
			roleModules()
		seen := map[string]bool{}
		for _, module := range modulesLoadNames(string(data)) {
			if seen[module] {
				t.Errorf("%s names %s twice", name, module)
			}
			seen[module] = true
		}
		runs := moduleRuns(string(data))
		if len(runs) != 2 {
			t.Errorf("%s holds %d runs of module lines, want two: those "+
				"loaded before the agent starts, then those loaded on "+
				"first use", name, len(runs))
			continue
		}
		for i, group := range []struct {
			what string
			want []string
		}{
			{"must be loaded before the agent starts", loadedNow},
			{"the kernel loads on first use", onDemand},
		} {
			want := map[string]bool{}
			for _, module := range group.want {
				want[module] = true
			}
			got := map[string]bool{}
			for _, module := range runs[i] {
				got[module] = true
			}
			for _, module := range sortedKeys(want) {
				if !got[module] {
					t.Errorf("%s names no %s among the modules that %s, "+
						"which the node check lists so for %s", name,
						module, group.what, role)
				}
			}
			for _, module := range sortedKeys(got) {
				if !want[module] {
					t.Errorf("%s names %s among the modules that %s, which "+
						"the node check does not list so for %s", name,
						module, group.what, role)
				}
			}
		}
	}
	entries, err := os.ReadDir(deployModulesLoadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, ok := shippedModulesLoad[entry.Name()]; !ok &&
			entry.Name() != hostModulesLoadName {
			t.Errorf("deploy/modules-load.d/%s is no role's file",
				entry.Name())
		}
	}
}

// moduleRuns are the runs of module lines of a modules-load file, each
// ended by a comment or a blank line, each module read as modulesLoadNames
// reads it.
func moduleRuns(text string) [][]string {
	var runs [][]string
	var run []string
	for _, line := range strings.Split(text, "\n") {
		names := modulesLoadNames(line)
		if len(names) == 0 {
			if len(run) > 0 {
				runs = append(runs, run)
			}
			run = nil
			continue
		}
		run = append(run, names...)
	}
	if len(run) > 0 {
		runs = append(runs, run)
	}
	return runs
}

// sortedKeys are a set's members in order, so that failures read the same
// on every run.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
