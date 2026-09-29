// CT-T1 — completeness (dnvctl.md §6, CT1).
//
// §0 #3 makes the CLI surface exactly the 59 RPCs of `service Gateway`: one
// command per RPC, no convenience verbs, no compound commands, nothing
// missing. Three things have to agree for that to be true, and this file
// cross-checks all three against each other:
//
//  1. rpcToCmd below, the §5 tables transcribed;
//  2. pb.Gateway_ServiceDesc.Methods, the generated truth about the service;
//  3. the leaves of the cobra tree NewRootCmd actually builds.
//
// Checking 1 against 2 alone is the trap: it proves the doc knows about every
// RPC, and says nothing about whether the command was ever wired into the
// tree. A group file whose registerX forgot one leaf, or a leaf added to the
// wrong group, passes a ServiceDesc-only check and fails the walk in
// TestCommandTreeMatchesTable.
//
// A transcription can also go stale against what it transcribes: a flag
// renamed in the code and in these tables, but not in §5, leaves every check
// above green. TestDocSection5MatchesTables reads §5 itself — its rows,
// §5.3's mirror sentence and §5.0's bullet spelling out the shared flag
// helpers' flags — and holds rpcToCmd, leafFlags and those helpers to it. It
// reads no other prose: a flag named in a notes column, or in another §5.0
// bullet, can still go stale with everything green.
package ctl

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// rpcToCmd maps every RPC of `service Gateway` to the `<group> <verb>` that
// drives it (§5.1-§5.12, in §5 order).
var rpcToCmd = map[string]string{
	// §5.1 cluster
	"CreateCluster": "cluster create",
	"DeleteCluster": "cluster delete",
	"GetCluster":    "cluster get",
	"ListClusters":  "cluster list",
	// §5.2 dn
	"CreateDiskNode":         "dn create",
	"DeleteDiskNode":         "dn delete",
	"GetDiskNode":            "dn get",
	"ListDiskNodes":          "dn list",
	"UpdateDiskNodeDisabled": "dn set-disabled",
	"InspectDiskNode":        "dn inspect",
	// §5.3 cn
	"CreateControllerNode":         "cn create",
	"DeleteControllerNode":         "cn delete",
	"GetControllerNode":            "cn get",
	"ListControllerNodes":          "cn list",
	"UpdateControllerNodeDisabled": "cn set-disabled",
	"InspectControllerNode":        "cn inspect",
	// §5.4 sp
	"CreateStoragePool":               "sp create",
	"DeleteStoragePool":               "sp delete",
	"GetStoragePool":                  "sp get",
	"ListStoragePools":                "sp list",
	"UpdateStoragePoolCntlidSlotList": "sp set-cntlid-slots",
	"UpdateStoragePoolLevel":          "sp set-level",
	"FindStoragePoolNames":            "sp find-names",
	"GrowSlice":                       "sp grow-slice",
	"InspectSide":                     "sp inspect-side",
	// §5.5 cntlr
	"CreateCntlr":        "cntlr create",
	"DeleteCntlr":        "cntlr delete",
	"UpdateCntlrEnabled": "cntlr set-enabled",
	"InspectCntlr":       "cntlr inspect",
	// §5.6 td
	"CreateThinDevice":    "td create",
	"DeleteThinDevice":    "td delete",
	"ListThinDevices":     "td list",
	"GetThinDeviceBitmap": "td get-bm",
	"GetLegBitmap":        "td get-leg-bm",
	// §5.7 ss
	"CreateSubsystem":      "ss create",
	"DeleteSubsystem":      "ss delete",
	"ListSubsystems":       "ss list",
	"UpdateSubsystemHosts": "ss set-hosts",
	// §5.8 ns
	"CreateNamespace":          "ns create",
	"DeleteNamespace":          "ns delete",
	"UpdateNamespaceDev":       "ns set-dev",
	"UpdateNamespaceSuspended": "ns set-suspended",
	// §5.9 clone
	"CreateClone":       "clone create",
	"DeleteClone":       "clone delete",
	"GetClone":          "clone get",
	"UpdateCloneTrConf": "clone set-tr",
	"AppendCloneBitmap": "clone append-bm",
	// §5.10 xfer
	"CreateTransfer":      "xfer create",
	"DeleteTransfer":      "xfer delete",
	"GetTransfer":         "xfer get",
	"UpdateTransferHosts": "xfer set-hosts",
	// §5.11 migr
	"CreateMigration":       "migr create",
	"FinishMigration":       "migr finish",
	"CancelMigration":       "migr cancel",
	"GetMigration":          "migr get",
	"AppendMigrationBitmap": "migr append-bm",
	// §5.12 spare
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
// check cannot do: a command that exists in §5 and in rpcToCmd but was never
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

// TestGroupsMatchSection5 pins the §5 tally line — 4+6+6+9+4+5+4+4+5+4+5+3 =
// 59 — group by group, so a leaf that moved between two groups (which keeps
// the total at 59) names the two groups it moved between in the failure.
func TestGroupsMatchSection5(t *testing.T) {
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

// leafFlags is §5's "flags beyond globals" column, transcribed command by
// command. The shared helpers are expanded into the flags they really
// declare, because that expansion is itself part of the surface: trConfFlags
// puts four flags on a command, under a prefix that differs between `dn
// create` (unprefixed) and `clone create` (`src-`), and reading the wrong
// prefix is a mistake the sweep can miss whenever the two would produce the
// same defaults.
var leafFlags = map[string][]string{
	// §5.1
	"cluster create": {"name", "extent-size"},
	"cluster delete": {"name"},
	"cluster get":    {"name"},
	"cluster list":   {"count", "page-token"},
	// §5.2 — trConfFlags("") expands to the four unprefixed transport flags.
	"dn create": {"addr", "location", "disabled",
		"tr-type", "adr-fam", "tr-addr", "tr-svc-id"},
	"dn delete":       {"addr"},
	"dn get":          {"addr"},
	"dn list":         {"count", "page-token"},
	"dn set-disabled": {"addr", "disabled"},
	"dn inspect":      {"addr"},
	// §5.3 — "exact dn mirrors ... same flags", asserted literally.
	"cn create": {"addr", "location", "disabled",
		"tr-type", "adr-fam", "tr-addr", "tr-svc-id"},
	"cn delete":       {"addr"},
	"cn get":          {"addr"},
	"cn list":         {"count", "page-token"},
	"cn set-disabled": {"addr", "disabled"},
	"cn inspect":      {"addr"},
	// §5.4
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
	// §5.5
	"cntlr create":      {"slot", "cn-black", "cn-white"},
	"cntlr delete":      {"id"},
	"cntlr set-enabled": {"id", "enabled"},
	"cntlr inspect":     {"id"},
	// §5.6
	"td create":     {"name", "ori", "size"},
	"td delete":     {"name"},
	"td list":       {},
	"td get-bm":     {"name", "slice-idx", "start", "cnt"},
	"td get-leg-bm": {"leg", "start", "cnt"},
	// §5.7
	"ss create":    {"nqn", "hosts"},
	"ss delete":    {"nqn"},
	"ss list":      {},
	"ss set-hosts": {"nqn", "hosts"},
	// §5.8
	"ns create": {"nqn", "idx", "td", "uuid", "nguid", "suspended"},
	"ns delete": {"nqn", "idx"},
	"ns set-dev": {"nqn", "idx",
		"td"},
	"ns set-suspended": {"nqn", "idx", "suspended"},
	// §5.9 — trConfFlags("src-") and dmCloneConfFlags expanded.
	"clone create": {"name", "dst-td", "src-nqn", "src-idx", "src-slices",
		"src-stripe", "src-block", "src-tr-type", "src-adr-fam",
		"src-tr-addr", "src-tr-svc-id", "auto-resume",
		"hyd-threshold", "hyd-batch"},
	"clone delete": {"name", "force"},
	"clone get":    {"name"},
	"clone set-tr": {"name", "src-tr-type", "src-adr-fam", "src-tr-addr",
		"src-tr-svc-id"},
	"clone append-bm": {"name", "src-slice-idx", "bm-idx", "bm-hex"},
	// §5.10
	"xfer create": {"name", "ori-nqn", "ori-idx", "hosts", "auto-suspend"},
	"xfer delete": {"name", "force"},
	"xfer get":    {"name"},
	"xfer set-hosts": {"name",
		"hosts"},
	// §5.11
	"migr create": {"name", "src-side", "dn-black", "dn-white",
		"hyd-threshold", "hyd-batch"},
	"migr finish":    {"name", "force"},
	"migr cancel":    {"name"},
	"migr get":       {"name"},
	"migr append-bm": {"name", "bm-hex"},
	// §5.12
	"spare create": {"grp", "dn-black", "dn-white"},
	"spare delete": {"grp", "leg"},
	"spare switch": {"grp", "spare", "target"},
}

// TestLeafFlagsMatchSection5 pins the flag surface of every command. The
// missing half is caught by the sweep — a flag the code forgot to declare
// makes its argv row an exit-2 "unknown flag" — but the EXTRA half is not: a
// flag left behind on a command, or one silently added, changes what an
// operator can type and no request-level test can see it.
func TestLeafFlagsMatchSection5(t *testing.T) {
	root := NewRootCmd()
	for _, path := range leafPaths(t, root) {
		want, ok := leafFlags[path]
		if !ok {
			t.Errorf("no §5 flag row for %q", path)
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

// TestDocSection5MatchesTables holds the two transcriptions above to what
// they transcribe. rpcToCmd and leafFlags are checked against the service
// and the tree, but nothing read §5 itself, so a flag renamed in the code and
// in both tables left a stale doc row with everything green. This reads every
// §5 row, and §5.3's mirror sentence (the one group stated in prose), and
// diffs each command against them: its RPC against rpcToCmd, its flags column
// against leafFlags — the shared helpers expanded through the real
// trConfFlags, selectorFlags and dmCloneConfFlags, so `trConfFlags("src-")`
// in the doc means the flags that call declares — and whether it carries a
// token marker, "(+ `--rev`)" or §5.3's "(+`cn_rev`)", against whether its
// request has a token field (§4). That expansion is also why a row cannot
// see a flag renamed inside a helper: the call changes with the helper.
// §5.0's shared-helpers bullet spells those flags out, so it is read as well
// (section5CheckHelpers). A marker counts only for being there: the --rev it
// spells is matched against this file's own literal, not against the flag
// root.go declares, and the field it names is not compared with the
// request's token field, so renaming the global, or naming the wrong field,
// leaves this test green.
func TestDocSection5MatchesTables(t *testing.T) {
	doc := readSection5(t)
	rows := section5Rows(t, doc)
	gateway := pb.File_pb_schema_proto.Services().ByName(
		protoreflect.Name(pb.Gateway_ServiceDesc.ServiceName))
	if gateway == nil {
		t.Fatalf("schema.proto declares no service %s",
			pb.Gateway_ServiceDesc.ServiceName)
	}
	for cmd, row := range rows {
		want, ok := rpcToCmd[row.rpc]
		if !ok {
			t.Errorf("%s: §5 maps %q to %s, which rpcToCmd does not name",
				row.site, cmd, row.rpc)
			continue
		}
		if want != cmd {
			t.Errorf("%s: §5 maps %q to %s, rpcToCmd maps %s to %q",
				row.site, cmd, row.rpc, row.rpc, want)
			continue
		}
		wantFlags := slices.Sorted(slices.Values(leafFlags[cmd]))
		if !slices.Equal(row.flags, wantFlags) {
			t.Errorf("%s: §5 gives %q the flags %v, leafFlags %v",
				row.site, cmd, row.flags, wantFlags)
		}
		method := gateway.Methods().ByName(protoreflect.Name(row.rpc))
		if method == nil {
			t.Errorf("%s: service Gateway declares no %s", row.site, row.rpc)
			continue
		}
		switch carries := revTokenField(method.Input()) != nil; {
		case row.rev && !carries:
			t.Errorf("%s: §5 marks %q (+ --rev), but the %s request "+
				"carries no token", row.site, cmd, row.rpc)
		case !row.rev && carries:
			t.Errorf("%s: §5 does not mark %q (+ --rev), but the %s "+
				"request carries a token", row.site, cmd, row.rpc)
		}
	}
	for rpc, cmd := range rpcToCmd {
		if _, ok := rows[cmd]; !ok {
			t.Errorf("rpcToCmd maps %s to %q, which §5 has no row for",
				rpc, cmd)
		}
	}
	if len(rows) != 59 {
		t.Errorf("§5 states %d commands, want 59", len(rows))
	}
	section5CheckHelpers(t, doc)
}

// section5Doc is dnvctl.md split into lines, §5 being lines[start:end].
type section5Doc struct {
	rel        string
	lines      []string
	start, end int
}

// readSection5 reads dnvctl.md and finds §5 in it.
func readSection5(t *testing.T) section5Doc {
	t.Helper()
	const rel = "doc/dnvctl.md"
	raw, err := os.ReadFile(filepath.Join(docRoot, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	lines := strings.Split(string(raw), "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		if start < 0 {
			if strings.HasPrefix(line, "## 5. ") {
				start = i
			}
		} else if strings.HasPrefix(line, "## ") {
			end = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s has no \"## 5. \" section", rel)
	}
	return section5Doc{rel: rel, lines: lines, start: start, end: end}
}

// section5Row is one command as dnvctl.md §5 states it.
type section5Row struct {
	rpc   string
	flags []string // sorted, the helpers expanded
	rev   bool     // the token marker of §4's 34 carriers
	site  docSite
}

var (
	// section5Rev is a flags column's token marker: "(+ `--rev`)", or
	// "(+ `--rev` ⇒ `dn_rev`)".
	section5Rev = regexp.MustCompile("\\(\\+ ?`--rev`[^)]*\\)")
	// section5Flag is one backticked flag in a flags column.
	section5Flag = regexp.MustCompile("`--([a-z0-9-]+)`")
	// section5Helper is a shared flag helper as §5.0 spells it.
	section5Helper = regexp.MustCompile(
		`(trConfFlags|selectorFlags)\("([a-z-]*)"\)|dmCloneConfFlags`)
	// section5Mirror is §5.3's sentence: "Exact `dn` mirrors against the
	// CN RPCs: `cn create`, … ↔ CreateControllerNode, …. Same flags."
	section5Mirror = regexp.MustCompile(
		"Exact `([a-z]+)` mirrors [^:]*: ([^↔]*) ↔ ([^.]*)\\. Same flags\\.")
	// section5MirrorRev is the mirror sentence's token marker, "(+`cn_rev`)".
	section5MirrorRev = regexp.MustCompile("\\(\\+ ?`[a-z]+_rev`\\)")
	section5Quoted    = regexp.MustCompile("`([^`]+)`")
)

// section5Rows reads every command dnvctl.md §5 states: the table rows, and
// the commands of §5.3's mirror sentence, which pair by position with its
// RPCs and take the flags of the mirrored group's command with the same verb
// ("Same flags.").
func section5Rows(t *testing.T, doc section5Doc) map[string]section5Row {
	t.Helper()
	rel, lines, start, end := doc.rel, doc.lines, doc.start, doc.end
	rows := map[string]section5Row{}
	add := func(cmd string, row section5Row) {
		if prev, ok := rows[cmd]; ok {
			t.Errorf("%s: §5 states %q a second time (first at %s)",
				row.site, cmd, prev.site)
		}
		rows[cmd] = row
	}
	var mirrorSites []docSite
	for i := start; i < end; i++ {
		site := docSite{rel, i + 1}
		if strings.Contains(lines[i], "↔") {
			mirrorSites = append(mirrorSites, site)
		}
		if !strings.HasPrefix(lines[i], "| `") {
			continue
		}
		cells := section5Cells(lines[i])
		if len(cells) < 3 {
			t.Errorf("%s: a §5 row with %d cells", site, len(cells))
			continue
		}
		flags, rev := section5Flags(cells[2])
		add(strings.Trim(cells[0], "`"), section5Row{
			rpc: cells[1], flags: flags, rev: rev, site: site,
		})
	}

	// The mirror sentence spans lines, so it is matched on §5 joined into
	// one; every ↔ must belong to a sentence read here, or a reworded one
	// would silently drop its group from the check.
	mirrors := section5Mirror.FindAllStringSubmatch(
		strings.Join(lines[start:end], " "), -1)
	if len(mirrors) != len(mirrorSites) {
		t.Fatalf("§5 has %d ↔ lines (%v) and %d mirror sentences this "+
			"lint can read", len(mirrorSites), mirrorSites, len(mirrors))
	}
	for k, m := range mirrors {
		cmds := section5Quoted.FindAllStringSubmatch(m[2], -1)
		rpcs := strings.Split(m[3], ",")
		if len(cmds) != len(rpcs) {
			t.Errorf("%s: %d commands ↔ %d RPCs", mirrorSites[k],
				len(cmds), len(rpcs))
			continue
		}
		for j, quoted := range cmds {
			cmd := quoted[1]
			_, verb, _ := strings.Cut(cmd, " ")
			base, ok := rows[m[1]+" "+verb]
			if !ok {
				t.Errorf("%s: %q mirrors %q, which §5 has no row for",
					mirrorSites[k], cmd, m[1]+" "+verb)
				continue
			}
			item := strings.TrimSpace(rpcs[j])
			rpc, _, _ := strings.Cut(item, " ")
			add(cmd, section5Row{
				rpc:   rpc,
				flags: base.flags,
				rev:   section5MirrorRev.MatchString(item),
				site:  mirrorSites[k],
			})
		}
	}
	return rows
}

// section5Cells splits one table row into its trimmed cells. `\|` is a pipe
// inside a cell (`sp create`'s `raid1`\|`none`), not a boundary.
func section5Cells(line string) []string {
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	cells = append(cells, strings.TrimSpace(cell.String()))
	// The row's outer pipes leave an empty cell at each end.
	if len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	return cells
}

// section5Flags reads one flags column: its backticked flags, its shared
// helpers expanded by calling them on a scratch flag set, and its token
// marker. Anything else in the column — `—`, a "(default `raid1`)" note, the
// `/` between the four `--thr-*` — names no flag.
func section5Flags(cell string) ([]string, bool) {
	rev := section5Rev.MatchString(cell)
	cell = section5Rev.ReplaceAllString(cell, "")
	var flags []string
	for _, m := range section5Flag.FindAllStringSubmatch(cell, -1) {
		flags = append(flags, m[1])
	}
	for _, m := range section5Helper.FindAllStringSubmatch(cell, -1) {
		scratch := pflag.NewFlagSet(m[0], pflag.ContinueOnError)
		switch m[1] {
		case "trConfFlags":
			trConfFlags(scratch, m[2])
		case "selectorFlags":
			selectorFlags(scratch, m[2])
		default:
			dmCloneConfFlags(scratch)
		}
		scratch.VisitAll(func(f *pflag.Flag) {
			flags = append(flags, f.Name)
		})
	}
	sort.Strings(flags)
	return flags, rev
}

// section5Helpers is §5.0's shared-helpers bullet as section5CheckHelpers
// reads it: each helper, the words that introduce its flags there, and a call
// declaring them. The bullet writes a prefixed flag with the placeholder
// `<prefix>` (`--<prefix>tr-type`), so the two prefixed helpers are called
// with that placeholder as their prefix and compared as spelled.
var section5Helpers = []struct {
	name    string
	head    string
	declare func(*pflag.FlagSet)
}{
	{"trConfFlags", "`trConfFlags(prefix)` →",
		func(f *pflag.FlagSet) { trConfFlags(f, "<prefix>") }},
	{"selectorFlags", "`selectorFlags(prefix)` →",
		func(f *pflag.FlagSet) { selectorFlags(f, "<prefix>") }},
	{"dmCloneConfFlags", "`dmCloneConfFlags` →", dmCloneConfFlags},
	{"pageFlags", "page flags", pageFlags},
}

// section5Spelled is one backticked flag in the shared-helpers bullet, the
// `<prefix>` placeholder included.
var section5Spelled = regexp.MustCompile("`--([^`]+)`")

// section5CheckHelpers holds §5.0's "Shared flag helpers" bullet to the
// helpers it describes. A row that names a helper is expanded by calling it,
// so a flag renamed inside the helper is renamed in that row's expansion too
// and no row goes stale, while the bullet, which spells the helper's flags
// out, does. For each helper the flags it spells are the backticked `--`
// names after the helper's head, up to the first `,` or `⇒` (the defaults
// and the nil rule follow), and they must be exactly the flags the helper
// declares.
func section5CheckHelpers(t *testing.T, doc section5Doc) {
	t.Helper()
	first := -1
	for i := doc.start; i < doc.end; i++ {
		if strings.HasPrefix(doc.lines[i], "* **Shared flag helpers**") {
			first = i
			break
		}
	}
	if first < 0 {
		t.Errorf("%s: §5 has no \"* **Shared flag helpers**\" bullet",
			doc.rel)
		return
	}
	last := first + 1
	for last < doc.end && doc.lines[last] != "" &&
		!strings.HasPrefix(doc.lines[last], "* ") &&
		!strings.HasPrefix(doc.lines[last], "#") {
		last++
	}
	bullet := strings.Join(
		strings.Fields(strings.Join(doc.lines[first:last], " ")), " ")
	for _, helper := range section5Helpers {
		// The failure names the line holding the head, or the bullet's
		// first line when the head is broken across two.
		site := docSite{doc.rel, first + 1}
		for i := first; i < last; i++ {
			if strings.Contains(doc.lines[i], helper.head) {
				site.line = i + 1
				break
			}
		}
		_, spelling, ok := strings.Cut(bullet, helper.head)
		if !ok {
			t.Errorf("%s: the shared-helpers bullet has no %q, so the "+
				"flags it spells for %s cannot be found", site, helper.head,
				helper.name)
			continue
		}
		if cut := strings.IndexAny(spelling, ",⇒"); cut >= 0 {
			spelling = spelling[:cut]
		}
		var spelled []string
		for _, m := range section5Spelled.FindAllStringSubmatch(
			spelling, -1) {
			spelled = append(spelled, m[1])
		}
		sort.Strings(spelled)
		scratch := pflag.NewFlagSet(helper.name, pflag.ContinueOnError)
		helper.declare(scratch)
		var declared []string
		scratch.VisitAll(func(f *pflag.Flag) {
			declared = append(declared, f.Name)
		})
		sort.Strings(declared)
		if !slices.Equal(spelled, declared) {
			t.Errorf("%s: §5.0 spells the flags of %s as %v, but it "+
				"declares %v", site, helper.name, spelled, declared)
		}
	}
}

// TestRootPersistentFlags pins §2.1's table: the seven globals, and nothing
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

	// CT9's env-backed set is every global but --rev (§0 #15), so a global
	// added to the root has to be placed on one side or the other here.
	envBacked := slices.DeleteFunc(slices.Clone(want),
		func(name string) bool { return name == "rev" })
	gotEnv := slices.Sorted(slices.Values(envGlobals))
	if !slices.Equal(gotEnv, envBacked) {
		t.Errorf("the env-backed globals are %v, want %v", gotEnv, envBacked)
	}

	// §2.1's default column. --timeout is the only one that is not empty.
	// --rev's empty default is not what makes "not given" (§4) — that is
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
// cobra's stock help/completion subcommands filtered out (§0 #3 allows those
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
// `completion` and completion's per-shell children), which §0 #3 exempts.
func stockPath(cmd *cobra.Command) bool {
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		if !c.Parent().HasParent() {
			return c.Name() == "help" || c.Name() == "completion"
		}
	}
	return false
}
