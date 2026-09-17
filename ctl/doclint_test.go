// Doc lint — rule-id definitions, citations and ranges across `doc/` and
// README.md.
//
// # Why this lives in ctl/
//
// It is a test over documents, not over `ctl`. It sits here because ctl/ is
// already where doc-versus-generated-code cross-checks live: completeness_test.go
// walks `pb.Gateway_ServiceDesc` against the §5 tables transcribed from
// dnvctl.md, and the 2026-09-16 review ledger puts the sibling "59 RPCs"
// literal scan here too, "next to the existing table tests". A `doclint`
// package of its own would
// exist only to hold one file, and would have to be added to doc/layout.md's
// package table — a new entry in the layout spec for no gain. Nothing in this
// file imports `ctl`; it reads the repository from `..`.
//
// # What drifts, and what this catches
//
// The docs tag normative rules with ids — `SPD14`, `GW6`, `CN18`, `[D13]` — and
// cite them from prose, from coverage matrices and from code comments all over
// the tree. Two kinds of rot no compiler and no test suite can see:
//
//   - a citation of an id that no longer exists (a rule renumbered, a rule
//     deleted, a typo: `CLD13` reads exactly like `CLD12`);
//   - a range, `SPD1-SPD14`, that reaches past the last rule of its family, or
//     across a rule that was removed from the middle of one.
//
// Both are checked below, for every definition form the docs actually use, and
// every violation is reported with its file and line — not just the first,
// because these arrive in families (one renumbering touches every carrier).
//
// # What this deliberately does NOT check
//
// It does not require the top of a cited range to be the family's highest
// defined id. That rule reads well and is false here: most cited ranges are
// SUB-ranges naming a group of rules inside a family, not a claim to cover it.
// `SH10-SH13` is the lock block of a 27-rule family (dnagent.md §2.6);
// `DS5-DS8` is four of eleven (cdc.md §0 item 5); dnv-worker.md's own §14.14
// coverage matrix splits RW across `RW1-RW12` and `RW13-RW21`, and stops at
// `CM1-CM6` and `EU1-EU6` because CM7 is a `go list -deps` wiring check and EU7
// is "`etcdutil_test.go` runs against a real etcd", neither of which the §14
// shell suite drives. Nor does opening at 1 mean "all of them": `SH1-SH3` is
// the bootstrap block of those same 27 rules, and §14.14's own `VW1-VW7`,
// `SW1-SW3`, `RW1-RW12`, `EU1-EU6` and `CM1-CM6` all start at 1 and stop
// short. There is no structural signal in the markdown that separates "these
// four" from "all of them", so a highest-id check would have to carry a
// hand-kept list of the ranges that mean "all" — a second place to maintain,
// of the kind this check exists to remove. Narrowing the check to the sections
// headed `[Cc]overage matrix` does not rescue it: §14.14 is one of those
// sections, and its CM and EU rows are the short ones.
//
// So of the two clauses of the 2026-09-16 ledger's §3.2 fix 3, "every cited Xk
// is defined" is implemented below and "every X1-Xn range cited anywhere in
// doc/ ends at the highest defined id" is NOT, which leaves the §3.1 risk it
// came from — "a new rule falls outside silently" — open. What the range check
// does catch are the loud halves: widening a citation to `SPD1-SPD15` before
// SPD15 is written, and deleting or renumbering SPD14 under a standing
// `SPD1-SPD14`. Adding SPD15 while `SPD1-SPD14` stands still passes here.
//
// Rule ids sit in ONE namespace, family prefix plus number, and `CM` is four
// independent per-binary families under that one prefix: cdc.md defines
// CM1-CM5, dnagent.md CM1-CM4 (marked `[shared]` by cnagent.md's preamble,
// which re-specifies none of them), dnv-worker.md CM1-CM7, gateway.md
// CM1-CM3. A CM citation therefore resolves against the union rather than
// against its own document, so cdc.md could lose its CM5 and cdc.md's §7 log
// rows would still resolve — against dnv-worker.md's unrelated `CM5.
// **Shutdown.**`. Splitting the namespace per document needs a signal the
// markdown does not carry either:
// architecture.md's IR rules are RE-stated as bold bullets in dnagent.md and
// cnagent.md, and a restatement of one rule is indistinguishable here from a
// second family's definition of another. The stop-gap is the pin table in
// TestDocScanFindsEveryDefinitionForm, which asserts the owning FILE, and it
// carries cdc.md's CM4 and CM5 for exactly this reason.
//
// Only `doc/*.md` and README.md are read. Code comments cite these ids too —
// `(SPD10, SPD11)` in worker/reaction.go and model/drain.go, `(SPD10/SPD11)`
// in gateway/storagepool.go, SPD10 in common/constants.go — and they are out
// of scope. This file is itself the reason to be careful about widening the
// walk: its header cites `CLD13` as an example of a typo and no such rule
// exists, so a scan of `.go` would have to open with an exception for the
// scanner.
//
// Single-letter families are out of scope except `[D<n>]`, and that one only in
// its bracketed spelling. Bare `D1`/`D2`/`D3` are dnv-worker.md's names for the
// three sp-drain steps (SPD8), `R4` is a row of ThinDeviceCreated.md's review
// table AND a rule of log.md, `T4` is grpc.md's trace-id rule and `L1` is its
// logging rule; a bare single letter plus a digit is not evidence of a citation
// in this tree. The brackets are, which is why `[D14]` is read and `D14` is not.
package ctl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docRoot is the repository root as seen from this package's directory.
const docRoot = ".."

// docSite is one file:line, for reporting.
type docSite struct {
	file string
	line int
}

func (s docSite) String() string { return fmt.Sprintf("%s:%d", s.file, s.line) }

// docRuleId is a family prefix plus its number, e.g. {"SPD", 14}.
type docRuleId struct {
	fam string
	num int
}

func (r docRuleId) String() string { return r.fam + strconv.Itoa(r.num) }

// docCitation is one id mentioned in running text.
type docCitation struct {
	id   docRuleId
	site docSite
	text string // the surrounding line, trimmed, for the failure message
}

// docRange is one `X<i>-X<j>` (any dash) mentioned in running text.
type docRange struct {
	lo, hi docRuleId
	raw    string
	site   docSite
	text   string
}

// docIndex is the whole scan: what is defined, what is declared absent, and
// everything that points at either.
type docIndex struct {
	defs  map[docRuleId][]docSite
	gaps  map[docRuleId][]docSite // "There is no GW13" — see below
	fams  map[string]bool
	cites []docCitation
	rngs  []docRange
	forms map[string]int // definition form -> how many lines matched it
}

var (
	// A fence opens or closes a code block, capturing the opener's info string.
	//
	// Fenced blocks are read for CITATIONS but never for definitions: a rule is
	// defined by the prose structure around it, while code and file trees cite
	// rules all the time — layout.md's whole package tree is one fence, and it
	// cites `SPD1-SPD14` and `CLD1-CLD12` from its two `worker/` drain lines.
	//
	// The exception is a ```mermaid block, which is skipped entirely: its node
	// identifiers are drawing labels, not ids. architecture.md §1 draws
	// `CN0["CN"]` and `DN0["DN"]` — there is no rule CN0, and the label says so
	// itself at §3.1: `CN0["cn0 (primary cntlr's CN)"]`.
	docFenceRe = regexp.MustCompile("^[\t ]*(?:```+|~~~+)[ \t]*([A-Za-z0-9_+-]*)")

	// Definition form 1, heading: `### 3.2 Errors and exit codes — CT5`.
	// dnvctl.md tags three of its sections this way instead of opening a
	// paragraph with the id. The `.*` is greedy, so the id must follow the LAST
	// dash of the heading and be the whole of its tail.
	docHeadDefRe = regexp.MustCompile(`^#{1,6} .*[\x{2013}\x{2014}] *([A-Z]{2,4})([0-9]+) *$`)

	// A leading list marker, stripped before the line-start forms are tried.
	docBulletRe = regexp.MustCompile(`^ {0,6}[-*] +`)

	// Definition form 2, bracketed: `* **[D13] The DN carries ...`
	// (architecture.md's decision record).
	docBrackDefRe = regexp.MustCompile(`^\[D([0-9]+)\]`)

	// Definition forms 3 and 4, line-start: `SPD14. **Tripwires.**` and
	// `* **GW6 — token check, presence-based.**`. What may follow the id is
	// spelled out in docDefTerminators.
	docPlainDefRe = regexp.MustCompile(`^([A-Z]{2,4})([0-9]+)`)

	// A DECLARED gap: the GW14 bullet of gateway.md §4 ("The handler pattern")
	// closes with "(There is no GW13: …)" in prose. The id is deliberately
	// absent, so a citation of it is not a dangling citation and a range across
	// it is not stale. Recognising the sentence keeps that fact in the document
	// that owns it rather than in an exception list here. Only the "there is no
	// <id>" clause is read; whatever justification the document gives after the
	// colon is the document's own, and nothing here reads or relies on it.
	docGapRe = regexp.MustCompile(`(?i:there is no )([A-Z]{2,4})([0-9]+)`)

	// A bracketed architecture-decision citation, `[D12]`.
	docDCiteRe = regexp.MustCompile(`\[D([0-9]+)\]`)

	// The separator of a range. The docs use ASCII `-` (`SPD1-SPD14`) and en
	// dash (`RW13–RW16`); em dash and the figure/non-breaking dashes are
	// accepted so a re-typeset range is still read. Exactly ONE dash character:
	// the integtest docs draw their VM topology with `----------` runs.
	docDashRe = regexp.MustCompile(`^ *[-\x{2010}\x{2011}\x{2012}\x{2013}\x{2014}\x{2015}] *$`)

	// A word run that is exactly a rule id.
	docTokenIdRe = regexp.MustCompile(`^([A-Z]{2,4})([0-9]+)$`)
)

// docDefTerminator is one thing that may follow the id on a definition line,
// with whether the id has to be BOLD for that terminator to count.
//
// Anything else — `SPD10/SPD11.`, `SH1-SH3`, `CN18's` — is a citation or a
// joint definition, not a definition of one id, and is not accepted. Rejecting
// the joint form is deliberate: it is what makes `grep '^SPD11\.'` find SPD11,
// and what stops a pair of rules sharing one definition line again.
//
// The bold column is what keeps prose citations out. Every line-start
// definition in doc/ today is either `.`-terminated (`SPD14. **Tripwires.**`)
// or bold (`* **GW6 — token check, presence-based.**`); the plain lines that
// open with an id and a dash or a paren are all in ThinDeviceCreated.md, and
// all three are citations — the review to-dos `* CN14 — rewrite the three
// paragraphs …` and `* CN21 — after "(no delete messages …`, and the wrapped
// sentence `CN21 (teardown deactivates, never deletes), §6 test 19; …`.
// Accepting those registered CN14 and CN21 as defined a second time, and that
// was enough to hide the rot this file exists to catch: renumbering both of
// them in cnagent.md left all three tests green, with every one of their
// citations across doc/, and the range `CN15-CN21`, still resolving against
// the review list.
type docDefTerminator struct {
	s    string
	bold bool // the id must carry a `**` opener for this terminator to define
}

// A colon is deliberately not in the table. No document defines a rule as
// `X<n>: …`, and an UNBOLDED colon would reopen the channel the bold column
// closes: a wrapped prose sentence that happens to begin a line with an id and
// a colon would register as a definition, which is enough to keep every
// dangling citation of a deleted rule — and every range across it — resolving.
var docDefTerminators = []docDefTerminator{
	{".", false},
	{"**", true}, {" —", true}, {" –", true}, {" (", true},
}

// docFiles lists the documents this lint owns: every doc/*.md plus README.md.
func docFiles(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(docRoot, "doc"))
	if err != nil {
		t.Fatalf("read doc/: %v", err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, filepath.Join("doc", e.Name()))
		}
	}
	sort.Strings(out)
	return append(out, "README.md")
}

// docLine is one line of a document and whether it sits inside a code fence.
type docLine struct {
	text string
	code bool
}

// docLines returns the numbered lines of one file. Fence markers themselves and
// whole ```mermaid blocks are dropped; every other line is kept, flagged with
// whether it is fenced.
func docLines(t *testing.T, rel string) map[int]docLine {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	out := map[int]docLine{}
	inFence, drawing := false, false
	for i, ln := range strings.Split(string(b), "\n") {
		if m := docFenceRe.FindStringSubmatch(ln); m != nil {
			if inFence {
				inFence, drawing = false, false
			} else {
				inFence, drawing = true, strings.EqualFold(m[1], "mermaid")
			}
			continue
		}
		if drawing {
			continue
		}
		out[i+1] = docLine{text: ln, code: inFence}
	}
	return out
}

// docDefOf reads one line as a rule-id definition, reporting which form matched
// so the scan can prove every form is still exercised.
func docDefOf(line string) (docRuleId, string, bool) {
	if m := docHeadDefRe.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[2])
		return docRuleId{m[1], n}, "heading", true
	}
	item := docBulletRe.ReplaceAllString(line, "")
	rest := strings.TrimPrefix(item, "**")
	bold := rest != item
	// The bracketed form must be BOLD. architecture.md's decision record opens
	// each decision with `* **[D13] …`, while its amendment lists open entries
	// with an unbolded `* [D14] LVM removal — …`, which points at the decision
	// rather than making one; and a wrapped sentence can begin with a bare
	// `[D13]` too (§11.2's "…[D13] clone-metadata area of the disk), so a DN
	// reboot resumes…"). Only the bold opener is a definition.
	if m := docBrackDefRe.FindStringSubmatch(rest); m != nil && bold {
		n, _ := strconv.Atoi(m[1])
		return docRuleId{"D", n}, "bracket", true
	}
	m := docPlainDefRe.FindStringSubmatch(rest)
	if m == nil {
		return docRuleId{}, "", false
	}
	tail := rest[len(m[0]):]
	for _, term := range docDefTerminators {
		if !strings.HasPrefix(tail, term.s) || (term.bold && !bold) {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		return docRuleId{m[1], n}, "line-start", true
	}
	return docRuleId{}, "", false
}

// docWord is one maximal run of [A-Za-z0-9_], with its byte offsets.
type docWord struct {
	s          string
	start, end int
}

func docWords(line string) []docWord {
	var out []docWord
	start := -1
	isWord := func(c byte) bool {
		return c == '_' || (c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	for i := 0; i < len(line); i++ {
		if isWord(line[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, docWord{line[start:i], start, i})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, docWord{line[start:], start, len(line)})
	}
	return out
}

// scanDocs builds the index. Definitions are collected from every file first,
// because a rule is routinely cited in one document and defined in another.
func scanDocs(t *testing.T) *docIndex {
	t.Helper()
	files := docFiles(t)
	idx := &docIndex{
		defs:  map[docRuleId][]docSite{},
		gaps:  map[docRuleId][]docSite{},
		fams:  map[string]bool{},
		forms: map[string]int{},
	}
	type numbered struct {
		rel   string
		lines map[int]docLine
		order []int
	}
	var docs []numbered
	for _, rel := range files {
		lines := docLines(t, rel)
		order := make([]int, 0, len(lines))
		for n := range lines {
			order = append(order, n)
		}
		sort.Ints(order)
		docs = append(docs, numbered{rel, lines, order})
	}
	for _, d := range docs {
		for _, n := range d.order {
			if d.lines[n].code {
				continue
			}
			line := d.lines[n].text
			site := docSite{d.rel, n}
			if id, form, ok := docDefOf(line); ok {
				idx.defs[id] = append(idx.defs[id], site)
				idx.fams[id.fam] = true
				idx.forms[form]++
			}
			for _, m := range docGapRe.FindAllStringSubmatch(line, -1) {
				num, _ := strconv.Atoi(m[2])
				id := docRuleId{m[1], num}
				idx.gaps[id] = append(idx.gaps[id], site)
				idx.forms["declared-gap"]++
			}
		}
	}
	for _, d := range docs {
		for _, n := range d.order {
			line := d.lines[n].text
			site := docSite{d.rel, n}
			text := strings.TrimSpace(line)
			words := docWords(line)
			ids := make([]*docRuleId, len(words))
			for i, w := range words {
				m := docTokenIdRe.FindStringSubmatch(w.s)
				if m == nil || !idx.fams[m[1]] {
					continue
				}
				num, _ := strconv.Atoi(m[2])
				id := docRuleId{m[1], num}
				ids[i] = &id
				idx.cites = append(idx.cites, docCitation{id, site, text})
				if d.lines[n].code {
					idx.forms["code-citation"]++
				}
			}
			for i := 0; i+1 < len(words); i++ {
				if ids[i] == nil || ids[i+1] == nil {
					continue
				}
				sep := line[words[i].end:words[i+1].start]
				if !docDashRe.MatchString(sep) {
					continue
				}
				idx.rngs = append(idx.rngs, docRange{
					lo:   *ids[i],
					hi:   *ids[i+1],
					raw:  line[words[i].start:words[i+1].end],
					site: site,
					text: text,
				})
			}
			if idx.fams["D"] {
				for _, m := range docDCiteRe.FindAllStringSubmatch(line, -1) {
					num, _ := strconv.Atoi(m[1])
					idx.cites = append(idx.cites, docCitation{
						docRuleId{"D", num}, site, text,
					})
				}
			}
		}
	}
	return idx
}

// exists reports whether an id is defined, or declared absent on purpose.
func (idx *docIndex) exists(id docRuleId) bool {
	if _, ok := idx.defs[id]; ok {
		return true
	}
	_, ok := idx.gaps[id]
	return ok
}

// TestDocScanFindsEveryDefinitionForm guards the lint itself. An extractor that
// silently stops matching — a form reworded, doc/ moved, the fence tracker
// eating the file — reports zero violations and passes both checks below while
// proving nothing, which is the failure mode this whole file exists to prevent.
// So: every definition form the docs use must still be found, every id in the
// table below must still be found in the document that owns it, and the range
// extractor — which feeds no form counter — must still be finding ranges.
func TestDocScanFindsEveryDefinitionForm(t *testing.T) {
	idx := scanDocs(t)
	for _, form := range []string{
		"heading", "bracket", "line-start", "declared-gap", "code-citation",
	} {
		if idx.forms[form] == 0 {
			t.Errorf("the %q part of the scan matched nothing; the extractor "+
				"is blind to it, so whatever it used to reach is now "+
				"unchecked", form)
		}
	}
	// The first three ids are reachable ONLY through their own definition
	// form: CT5 exists only as `### 3.2 Errors and exit codes — CT5`, D14 only
	// as `* **[D14] ...`, SPD11 only as `SPD11. **D2's slice-final half**`
	// (GW13, the declared-gap form, is checked separately below).
	//
	// The CM rows pin what the single id namespace cannot see (see the header):
	// three documents define a CM4 and two define a CM5, so re-joining cdc.md's
	// pair into one `* **CM4/CM5 — lifecycle logs.**` bullet defines NEITHER id
	// and its §7 log rows still resolve — against the strangers, dnv-worker.md's
	// `CM4. **Startup.**` and `CM5. **Shutdown.**` among them. Only the
	// owning-FILE assertion below catches that.
	for _, want := range []struct {
		id   docRuleId
		file string
		how  string
	}{
		{docRuleId{"CT", 5}, "doc/dnvctl.md", "a section heading ending in the id"},
		{docRuleId{"D", 14}, "doc/architecture.md", "a bracketed decision bullet"},
		{docRuleId{"SPD", 11}, "doc/dnv-worker.md", "a line-start `SPD11.` definition"},
		{docRuleId{"CM", 4}, "doc/cdc.md", "the bold `**CM4 — lifecycle logs, the start half.**` bullet"},
		{docRuleId{"CM", 5}, "doc/cdc.md", "the bold `**CM5 — lifecycle logs, the stop half**` bullet"},
	} {
		sites := idx.defs[want.id]
		if len(sites) == 0 {
			t.Errorf("%s is not defined anywhere; it is defined in %s by %s",
				want.id, want.file, want.how)
			continue
		}
		found := false
		for _, s := range sites {
			if s.file == want.file {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is defined at %v but not in %s, where %s carries it",
				want.id, sites, want.file, want.how)
		}
	}
	if len(idx.gaps[docRuleId{"GW", 13}]) == 0 {
		t.Errorf("gateway.md's \"There is no GW13\" is no longer recognised as " +
			"a declared gap; either the sentence changed or docGapRe did")
	}
	if n := len(idx.fams); n < 20 {
		t.Errorf("found %d rule families, want at least 20: the scan is not "+
			"reading doc/", n)
	}
	// Ranges have no form counter of their own, and the counters above cannot
	// stand in for one: `code-citation` is incremented per SINGLE id, so
	// layout.md's fenced `SPD1-SPD14` keeps it non-zero as two citations even
	// with docDashRe or the adjacency walk in scanDocs matching nothing. An
	// empty idx.rngs makes TestDocRuleRangesResolve pass over nothing, so pin
	// it here: dnv-worker.md §14.14's coverage matrix alone cites thirteen
	// ranges, which puts the floor far below what ordinary editing moves.
	if n := len(idx.rngs); n < 10 {
		t.Errorf("found %d cited rule-id ranges, want at least 10: the range "+
			"extractor is blind, so TestDocRuleRangesResolve checks nothing", n)
	}
}

// TestDocRuleCitationsAreDefined fails on any `X<k>` in doc/ or README.md whose
// family is one the docs define and whose number is not defined anywhere (and
// is not a declared gap).
func TestDocRuleCitationsAreDefined(t *testing.T) {
	idx := scanDocs(t)
	seen := map[string]bool{}
	var bad []string
	for _, c := range idx.cites {
		if idx.exists(c.id) {
			continue
		}
		key := c.site.String() + " " + c.id.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		bad = append(bad, fmt.Sprintf(
			"%s: cites %s, which no document defines\n        %s",
			c.site, c.id, c.text))
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s", b)
	}
	if len(bad) > 0 {
		t.Errorf("%d dangling rule-id citation(s); define the rule, fix the "+
			"citation, or — if the id is absent on purpose — say so in the "+
			"owning document as gateway.md does for GW13", len(bad))
	}
}

// TestDocRuleRangesResolve fails on any cited range whose endpoints are not both
// defined, that runs backwards, or that spans an id nothing defines. That is the
// half of "the range matches the family" a document cannot get wrong by
// accident: it fires the moment a range is widened past the last rule, and the
// moment a rule inside a cited range is deleted or renumbered.
func TestDocRuleRangesResolve(t *testing.T) {
	idx := scanDocs(t)
	var bad []string
	for _, r := range idx.rngs {
		report := func(why string) {
			bad = append(bad, fmt.Sprintf("%s: range %s %s\n        %s",
				r.site, r.raw, why, r.text))
		}
		if r.lo.fam != r.hi.fam {
			report(fmt.Sprintf("crosses families (%s then %s)", r.lo.fam, r.hi.fam))
			continue
		}
		if r.lo.num >= r.hi.num {
			report("does not ascend")
			continue
		}
		var missing []string
		for n := r.lo.num; n <= r.hi.num; n++ {
			id := docRuleId{r.lo.fam, n}
			if !idx.exists(id) {
				missing = append(missing, id.String())
			}
		}
		if len(missing) > 0 {
			report(fmt.Sprintf("covers %s, which no document defines",
				strings.Join(missing, ", ")))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s", b)
	}
	if len(bad) > 0 {
		t.Errorf("%d stale rule-id range(s) in doc/", len(bad))
	}
}
