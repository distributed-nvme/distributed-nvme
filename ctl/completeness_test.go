// Completeness of the command tree (dnvctl.md CT1).
//
// The CLI surface is exactly the RPCs of `service Gateway`: one command per
// RPC, no convenience verbs, no compound commands, nothing missing. Three
// things have to agree for that to be true, and this file cross-checks all
// three against each other:
//
//  1. rpcToCmd below, the RPC-to-command table;
//  2. pb.Gateway_ServiceDesc.Methods, the generated truth about the service;
//  3. the leaves of the cobra tree NewRootCmd actually builds.
//
// Checking 1 against 2 alone is the trap: it proves the table knows about
// every RPC, and says nothing about whether the command was ever wired into
// the tree. A group file whose registerX forgot one leaf, or a leaf added to
// the wrong group, passes a ServiceDesc-only check and fails the walk in
// TestCommandTreeMatchesTable. The tables here are the test's own; the doc
// states the rules (dnvctl.md, Conventions), not the tables.
package ctl

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// rpcToCmd maps every RPC of `service Gateway` to the `<group> <verb>` that
// drives it, group by group.
var rpcToCmd = map[string]string{
	// cluster
	"CreateCluster": "cluster create",
	"DeleteCluster": "cluster delete",
	"GetCluster":    "cluster get",
	"ListClusters":  "cluster list",
	// dn
	"CreateDiskNode":         "dn create",
	"DeleteDiskNode":         "dn delete",
	"GetDiskNode":            "dn get",
	"ListDiskNodes":          "dn list",
	"UpdateDiskNodeDisabled": "dn set-disabled",
	"InspectDiskNode":        "dn inspect",
	// cn
	"CreateControllerNode":         "cn create",
	"DeleteControllerNode":         "cn delete",
	"GetControllerNode":            "cn get",
	"ListControllerNodes":          "cn list",
	"UpdateControllerNodeDisabled": "cn set-disabled",
	"InspectControllerNode":        "cn inspect",
	// sp
	"CreateStoragePool":               "sp create",
	"DeleteStoragePool":               "sp delete",
	"GetStoragePool":                  "sp get",
	"ListStoragePools":                "sp list",
	"UpdateStoragePoolCntlidSlotList": "sp set-cntlid-slots",
	"UpdateStoragePoolLevel":          "sp set-level",
	"FindStoragePoolNames":            "sp find-names",
	"GrowSlice":                       "sp grow-slice",
	"InspectSide":                     "sp inspect-side",
	// cntlr
	"CreateCntlr":        "cntlr create",
	"DeleteCntlr":        "cntlr delete",
	"UpdateCntlrEnabled": "cntlr set-enabled",
	"InspectCntlr":       "cntlr inspect",
	// td
	"CreateThinDevice":    "td create",
	"DeleteThinDevice":    "td delete",
	"ListThinDevices":     "td list",
	"GetThinDeviceBitmap": "td get-bm",
	"GetLegBitmap":        "td get-leg-bm",
	// ss
	"CreateSubsystem":      "ss create",
	"DeleteSubsystem":      "ss delete",
	"ListSubsystems":       "ss list",
	"UpdateSubsystemHosts": "ss set-hosts",
	// ns
	"CreateNamespace":          "ns create",
	"DeleteNamespace":          "ns delete",
	"UpdateNamespaceDev":       "ns set-dev",
	"UpdateNamespaceSuspended": "ns set-suspended",
	// clone
	"CreateClone":       "clone create",
	"DeleteClone":       "clone delete",
	"GetClone":          "clone get",
	"UpdateCloneTrConf": "clone set-tr",
	"AppendCloneBitmap": "clone append-bm",
	// xfer
	"CreateTransfer":      "xfer create",
	"DeleteTransfer":      "xfer delete",
	"GetTransfer":         "xfer get",
	"UpdateTransferHosts": "xfer set-hosts",
	// migr
	"CreateMigration":       "migr create",
	"FinishMigration":       "migr finish",
	"CancelMigration":       "migr cancel",
	"GetMigration":          "migr get",
	"AppendMigrationBitmap": "migr append-bm",
	// spare
	"CreateSpareLeg": "spare create",
	"DeleteSpareLeg": "spare delete",
	"SwitchSpareLeg": "spare switch",
}

// TestRpcToCmdMatchesServiceDesc is the two-way check of CT1: every RPC the
// generated descriptor declares has a command, every command in the table
// names a real RPC, and the count is pinned at 59 so that a *pair* of
// compensating edits — an RPC removed from the service and a row removed from
// the table — still fails.
func TestRpcToCmdMatchesServiceDesc(t *testing.T) {
	methods := make(map[string]bool, len(pb.Gateway_ServiceDesc.Methods))
	for _, method := range pb.Gateway_ServiceDesc.Methods {
		methods[method.MethodName] = true
	}
	if len(methods) != 59 {
		t.Errorf("service Gateway has %d methods, want 59", len(methods))
	}
	if len(rpcToCmd) != 59 {
		t.Errorf("rpcToCmd has %d rows, want 59", len(rpcToCmd))
	}
	for name := range methods {
		if _, ok := rpcToCmd[name]; !ok {
			t.Errorf("RPC %s has no dnvctl command", name)
		}
	}
	for name := range rpcToCmd {
		if !methods[name] {
			t.Errorf("rpcToCmd names %s, which service Gateway does not "+
				"declare", name)
		}
	}
	// The commands must be distinct too: two RPCs mapped to one command
	// would pass both loops above while leaving an RPC undrivable.
	seen := make(map[string]string, len(rpcToCmd))
	for name, cmd := range rpcToCmd {
		if other, ok := seen[cmd]; ok {
			t.Errorf("command %q drives both %s and %s", cmd, other, name)
		}
		seen[cmd] = name
	}
}

// TestCommandTreeMatchesTable walks the tree NewRootCmd really builds and
// compares its leaf set with the table. This is the half a ServiceDesc-only
// check cannot do: a command that exists in rpcToCmd but was never
// added to its group — or was added to the wrong one — is invisible to the
// descriptor and fails here.
func TestCommandTreeMatchesTable(t *testing.T) {
	leaves := leafPaths(t, NewRootCmd())

	want := make(map[string]bool, len(rpcToCmd))
	for _, cmd := range rpcToCmd {
		want[cmd] = true
	}
	got := make(map[string]bool, len(leaves))
	for _, path := range leaves {
		got[path] = true
	}
	for path := range got {
		if !want[path] {
			t.Errorf("the tree has a leaf %q that no RPC maps to", path)
		}
	}
	for path := range want {
		if !got[path] {
			t.Errorf("rpcToCmd names %q, which the tree does not have", path)
		}
	}
	if len(leaves) != 59 {
		t.Errorf("the tree has %d RPC leaves, want 59: %v",
			len(leaves), leaves)
	}
}

// TestGroupsLeafCounts pins the leaf count of every group, so a leaf that moved between two groups (which keeps
// the total at 59) names the two groups it moved between in the failure.
func TestGroupsLeafCounts(t *testing.T) {
	want := map[string]int{
		"cluster": 4, "dn": 6, "cn": 6, "sp": 9, "cntlr": 4, "td": 5,
		"ss": 4, "ns": 4, "clone": 5, "xfer": 4, "migr": 5, "spare": 3,
	}
	got := map[string]int{}
	for _, path := range leafPaths(t, NewRootCmd()) {
		got[strings.SplitN(path, " ", 2)[0]]++
	}
	for name, count := range want {
		if got[name] != count {
			t.Errorf("group %q has %d commands, want %d",
				name, got[name], count)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("the tree has an unexpected group %q", name)
		}
	}
}

// TestEveryLeafIsRunnable guards the quiet failure mode of a cobra tree: a
// leaf with no RunE is not a usage error, it prints its own help and exits 0.
// A test that only checked exit codes would call that a pass.
func TestEveryLeafIsRunnable(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if len(cmd.Commands()) > 0 {
			for _, child := range cmd.Commands() {
				walk(child)
			}
			return
		}
		if stockPath(cmd) {
			return
		}
		if cmd.RunE == nil {
			t.Errorf("leaf %q has no RunE", commandPath(cmd))
		}
		if cmd.Args == nil {
			t.Errorf("leaf %q accepts positional arguments", commandPath(cmd))
		}
	}
	walk(NewRootCmd())
}

// leafFlags is every command's flags beyond the globals, listed command by
// command. The shared helpers are expanded into the flags they really
// declare, because that expansion is itself part of the surface: trConfFlags
// puts four flags on a command, under a prefix that differs between `dn
// create` (unprefixed) and `clone create` (`src-`), and reading the wrong
// prefix is a mistake the sweep can miss whenever the two would produce the
// same defaults.
var leafFlags = map[string][]string{
	// cluster
	"cluster create": {"name", "extent-size"},
	"cluster delete": {"name"},
	"cluster get":    {"name"},
	"cluster list":   {"count", "page-token"},
	// dn — trConfFlags("") expands to the four unprefixed transport flags.
	"dn create": {"addr", "location", "disabled",
		"tr-type", "adr-fam", "tr-addr", "tr-svc-id"},
	"dn delete":       {"addr"},
	"dn get":          {"addr"},
	"dn list":         {"count", "page-token"},
	"dn set-disabled": {"addr", "disabled"},
	"dn inspect":      {"addr"},
	// cn — the exact mirror of dn, asserted literally.
	"cn create": {"addr", "location", "disabled",
		"tr-type", "adr-fam", "tr-addr", "tr-svc-id"},
	"cn delete":       {"addr"},
	"cn get":          {"addr"},
	"cn list":         {"count", "page-token"},
	"cn set-disabled": {"addr", "disabled"},
	"cn inspect":      {"addr"},
	// sp
	"sp create": {"cntlr-cnt", "slice-cnt", "init-ext-cnt", "slots",
		"redund", "bitmap-chunk-blocks", "stripe-size", "block-size",
		"low-water-mark-pct",
		"thr-primary", "thr-cntlr", "thr-side", "thr-leg",
		"dn-black", "dn-white", "cn-black", "cn-white"},
	"sp delete":           {},
	"sp get":              {},
	"sp list":             {"count", "page-token"},
	"sp set-cntlid-slots": {"slots"},
	"sp set-level":        {"level"},
	"sp find-names":       {"ids"},
	"sp grow-slice":       {"slice", "ext", "meta", "dn-black", "dn-white"},
	"sp inspect-side":     {"id"},
	// cntlr
	"cntlr create":      {"slot", "cn-black", "cn-white"},
	"cntlr delete":      {"id"},
	"cntlr set-enabled": {"id", "enabled"},
	"cntlr inspect":     {"id"},
	// td
	"td create":     {"name", "ori", "size"},
	"td delete":     {"name"},
	"td list":       {},
	"td get-bm":     {"name", "slice-idx", "start", "cnt"},
	"td get-leg-bm": {"leg", "start", "cnt"},
	// ss
	"ss create":    {"nqn", "hosts"},
	"ss delete":    {"nqn"},
	"ss list":      {},
	"ss set-hosts": {"nqn", "hosts"},
	// ns
	"ns create": {"nqn", "idx", "td", "uuid", "nguid", "suspended"},
	"ns delete": {"nqn", "idx"},
	"ns set-dev": {"nqn", "idx",
		"td"},
	"ns set-suspended": {"nqn", "idx", "suspended"},
	// clone — trConfFlags("src-") and dmCloneConfFlags expanded.
	"clone create": {"name", "dst-td", "src-nqn", "src-idx", "src-slices",
		"src-stripe", "src-block", "src-tr-type", "src-adr-fam",
		"src-tr-addr", "src-tr-svc-id", "auto-resume",
		"hyd-threshold", "hyd-batch"},
	"clone delete": {"name", "force"},
	"clone get":    {"name"},
	"clone set-tr": {"name", "src-tr-type", "src-adr-fam", "src-tr-addr",
		"src-tr-svc-id"},
	"clone append-bm": {"name", "src-slice-idx", "bm-idx", "bm-hex"},
	// xfer
	"xfer create": {"name", "ori-nqn", "ori-idx", "hosts", "auto-suspend"},
	"xfer delete": {"name", "force"},
	"xfer get":    {"name"},
	"xfer set-hosts": {"name",
		"hosts"},
	// migr
	"migr create": {"name", "src-side", "dn-black", "dn-white",
		"hyd-threshold", "hyd-batch"},
	"migr finish":    {"name", "force"},
	"migr cancel":    {"name"},
	"migr get":       {"name"},
	"migr append-bm": {"name", "bm-hex"},
	// spare
	"spare create": {"grp", "dn-black", "dn-white"},
	"spare delete": {"grp", "leg"},
	"spare switch": {"grp", "spare", "target"},
}

// TestLeafFlagsMatchTable pins the flag surface of every command. The
// missing half is caught by the sweep — a flag the code forgot to declare
// makes its argv row an exit-2 "unknown flag" — but the EXTRA half is not: a
// flag left behind on a command, or one silently added, changes what an
// operator can type and no request-level test can see it.
func TestLeafFlagsMatchTable(t *testing.T) {
	root := NewRootCmd()
	for _, path := range leafPaths(t, root) {
		want, ok := leafFlags[path]
		if !ok {
			t.Errorf("no leafFlags row for %q", path)
			continue
		}
		cmd := findLeaf(t, root, path)
		var got []string
		// A fresh tree's Flags() holds the command's own flags only: cobra
		// merges the inherited persistent ones in ParseFlags, which has not
		// run here.
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			got = append(got, f.Name)
		})
		sort.Strings(got)
		wantSorted := append([]string(nil), want...)
		sort.Strings(wantSorted)
		if !slices.Equal(got, wantSorted) {
			t.Errorf("%q declares %v, want %v", path, got, wantSorted)
		}
	}
	if len(leafFlags) != 59 {
		t.Errorf("leafFlags has %d rows, want 59", len(leafFlags))
	}
}

// TestRootPersistentFlags pins the globals (dnvctl.md, Global flags, env, config), and nothing
// else promoted to the root by accident. A local flag that drifted onto the
// root would be silently accepted on every one of the 59 commands.
func TestRootPersistentFlags(t *testing.T) {
	want := []string{"cluster", "config", "gateway-address", "rev", "sp",
		"timeout", "trace-id"}
	var got []string
	defaults := map[string]string{}
	NewRootCmd().PersistentFlags().VisitAll(func(f *pflag.Flag) {
		got = append(got, f.Name)
		defaults[f.Name] = f.DefValue
	})
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("the root declares %v, want %v", got, want)
	}

	// CT9's env-backed set is every global but --rev (CT9), so a global
	// added to the root has to be placed on one side or the other here.
	envBacked := slices.DeleteFunc(slices.Clone(want),
		func(name string) bool { return name == "rev" })
	gotEnv := slices.Sorted(slices.Values(envGlobals))
	if !slices.Equal(gotEnv, envBacked) {
		t.Errorf("the env-backed globals are %v, want %v", gotEnv, envBacked)
	}

	// The globals' defaults. --timeout is the only one that is not empty.
	// --rev's empty default is not what makes "not given" (CT3) — that is
	// pflag's Changed bit, read in revToken — but a string flag keeps the
	// value's base-0 parse, and its usage error, next to that rule.
	for name, want := range map[string]string{
		"gateway-address": "",
		"cluster":         "",
		"sp":              "",
		"rev":             "",
		"trace-id":        "",
		"config":          "",
		"timeout":         "10",
	} {
		if defaults[name] != want {
			t.Errorf("--%s defaults to %q, want %q",
				name, defaults[name], want)
		}
	}
}

// findLeaf resolves a `<group> <verb>` path back to its command.
func findLeaf(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	cmd, _, err := root.Find(strings.Split(path, " "))
	if err != nil || cmd == nil {
		t.Fatalf("Find(%q): %v", path, err)
	}
	return cmd
}

// leafPaths returns every runnable `<group> <verb>` of a tree, sorted, with
// cobra's stock help/completion subcommands filtered out (CT1 allows those
// two and nothing else).
//
// The two stock commands are added here explicitly. cobra grafts them on
// during ExecuteC, so a tree that has never been executed does not have them
// and the filter below would never run — leaving the one thing it exists for
// untested, and leaving "help and completion are the ONLY non-RPC
// subcommands" unasserted.
func leafPaths(t *testing.T, root *cobra.Command) []string {
	t.Helper()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var out []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if len(cmd.Commands()) > 0 {
			for _, child := range cmd.Commands() {
				walk(child)
			}
			return
		}
		if cmd == root || stockPath(cmd) {
			return
		}
		out = append(out, commandPath(cmd))
	}
	walk(root)
	sort.Strings(out)
	return out
}

// commandPath is the `<group> <verb>` spelling, i.e. cobra's CommandPath
// without the program name.
func commandPath(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// stockPath reports whether a command is one of cobra's own (`help`,
// `completion` and completion's per-shell children), which CT1 exempts.
func stockPath(cmd *cobra.Command) bool {
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		if !c.Parent().HasParent() {
			return c.Name() == "help" || c.Name() == "completion"
		}
	}
	return false
}
