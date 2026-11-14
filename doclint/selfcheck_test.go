package doclint

import (
	"sort"
	"strings"
	"testing"
)

// The tests of this file feed the checks lines and tokens made up for the
// purpose, so that a gap in a check shows even where the docs of the tree
// happen not to hit it.

// TestIdDefinitionsAreParagraphStarts: an id defines a rule only at the start
// of a paragraph or of a list item, as an id with a dot in plain or bold type
// or as a bold id, or at the end of a heading; a citation that wraps to the
// start of a line defines nothing.
func TestIdDefinitionsAreParagraphStarts(t *testing.T) {
	// The ids are of a family no doc uses, XQ, so that the rule-id check,
	// which reads this file too, finds no citation in them; a design
	// decision's bracket is joined at run time for the same reason.
	d := func(n string) string { return "[" + "D" + n + "]" }
	lines := strings.Split(`# A doc

XQ1. **Lock mapping.** The first rule.

- XQ2. Enumerate the store.
- **XQ3.** A bold definition.

### A heading — XQ4

**`+d("5")+`** A design decision.

**XQ6** — A bold glossary term.

The rule cited at the end of
XQ7 says something, and so does
`+d("8")+` on this line, and
XQ9. ends a sentence at the start of a line.

XQ10 and XQ11 take the same step.

- XQ12 removes the device.
  XQ13. sits in a list item's continuation.
- An item.
XQ15. continues the item above.

`+d("16")+` of another doc opens a paragraph and cites it.

`+"```"+`
XQ14. inside a fence
`+"```"+`
XQ17a. follows a fence and has a suffix letter.

### Another heading
**XQ18a.** follows a heading.

### A heading with a suffix — XQ19b
`, "\n")
	got := idDefinitions(lines)
	var ids []string
	for id := range got {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	want := "XQ1 XQ17 XQ18 XQ19 XQ2 XQ3 XQ4 XQ6 " + d("5")
	if strings.Join(ids, " ") != want {
		t.Errorf("defined ids: got %q, want %q", strings.Join(ids, " "), want)
	}
}

// TestBareFileNamesExist: a bare file name in an LLM doc names a file of the
// tree, an underscore-led name names a suffix of one, and the allowlist of
// the check path takes the rest; identifiers and flags keep their rules.
func TestBareFileNamesExist(t *testing.T) {
	x := &treeIndex{
		code:      "func InspectDn() {} // rebuild go md --capacity",
		words:     map[string]bool{"func": true, "InspectDn": true, "rebuild": true, "go": true, "md": true, "capacity": true, "agent": true, "log": true, "CnAgentServer": true, "schema": true, "proto": true, "missing": true, "yaml": true, "conf": true},
		baseNames: map[string]bool{"md.go": true, "cnagent_test.go": true, "dnv-worker.md": true, "runner.conf": true},
		okIdent:   map[string]bool{},
		okPath:    map[string]bool{"cdc_host.sh": true},
	}
	cases := []struct {
		tok  string
		llm  bool
		fail bool
	}{
		{"md.go", true, false},
		{"rebuild.go", true, true},
		{"dnv-worker.md", true, false},
		{"dnv-agent.md", true, true},
		{"_test.go", true, false},
		{"_bench.go", true, true},
		{"cdc_host.sh", true, false},
		{"cdc_target.sh", true, true},
		{"InspectDn", true, false},
		{"InspectDnNode", true, true},
		{"--capacity", true, false},
		{"Clone.deleting", true, true},
		{"agent.log", true, false},
		{"rebuild.go", false, false},
		{"schema.proto", true, true},
		{"missing.yaml", true, true},
		{"runner.conf", true, false},
		{"missing.conf", true, true},
		{"CnAgentServer.md", true, false},
	}
	for _, c := range cases {
		p := x.tokenProblem(c.tok, c.llm)
		if (p != "") != c.fail {
			t.Errorf("token %q (llm doc %v): problem %q, want a problem: %v", c.tok, c.llm, p, c.fail)
		}
	}
}
