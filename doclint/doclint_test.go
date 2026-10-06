// Package doclint holds the tests that hold doc/*.md and README.md to the
// documentation rules: a doc states the current design and never restates
// what the code holds. It reads the repository from "..". The checks:
//
//   - no section numbers anywhere: no section sign in a doc or a code comment, no
//     numbered headings;
//   - every rule id cited in a doc, a Go file or a shell suite is defined in a
//     doc (ids are permanent anchors the code relies on);
//   - every backticked code identifier and every path a doc names exists in
//     the tree;
//   - no numeric literal beside a named constant (the value lives in the code);
//   - no dates in a doc (history lives in git); history words are reported
//     with -v only;
//   - in a code file: no "path.ext:N" line pointer into a Go, shell, proto or
//     Markdown file, no ISO date, and no citation of a note kept outside the
//     repository in the words outsideNoteRe matches (cleanlint_test.go);
//   - an "identifier (path/file.ext)" pointer resolves, and what follows a
//     document's name and a comma is a rule id or a heading text of that
//     document (citelint_test.go).
//
// Exceptions live in allowlist.txt beside this file, one per line:
// "<check> <token>", where check is one of ids, ident, path, value, history,
// identptr, heading, pointer, note.
package doclint

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const root = ".."

// skipDirs are never walked: vendored trees, build output, the untracked
// working notes, and git itself.
var skipDirs = map[string]bool{
	".git": true, "bin": true, "tmp_doc": true, ".claude": true,
}

func walkFiles(t *testing.T, match func(path string) bool) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			if skipDirs[info.Name()] || rel == "integtest/bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if match(rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

func docFiles(t *testing.T) []string {
	return walkFiles(t, func(p string) bool {
		return p == "README.md" || (strings.HasPrefix(p, "doc/") && strings.HasSuffix(p, ".md"))
	})
}

func codeFiles(t *testing.T) []string {
	return walkFiles(t, func(p string) bool {
		if strings.HasSuffix(p, ".pb.go") {
			return false
		}
		return strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".sh") ||
			strings.HasSuffix(p, ".proto") || filepath.Base(p) == "Makefile"
	})
}

func readLines(t *testing.T, rel string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.Split(string(b), "\n")
}

// allow returns the allowlisted tokens of one check.
func allow(t *testing.T, check string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	f, err := os.Open("allowlist.txt")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == check {
			out[fields[1]] = true
		}
	}
	return out
}

var fenceRe = regexp.MustCompile("^[\t ]*(```+|~~~+)")

// docProse yields the lines of a doc outside fenced code blocks, keeping
// line numbers; fenced blocks are returned by docFences.
func docProse(lines []string) map[int]string {
	out := map[int]string{}
	in := false
	for i, ln := range lines {
		if fenceRe.MatchString(ln) {
			in = !in
			continue
		}
		if !in {
			out[i+1] = ln
		}
	}
	return out
}

func sortedKeys(m map[int]string) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// ---- section numbers -------------------------------------------------------

// sectionSign is written as an escape so this file does not flag itself.
const sectionSign = "\u00a7"

var numberedHeadingRe = regexp.MustCompile(`^#{1,6}\s+(\d+(\.\d+)*\.?|Appendix\s+[A-Z])(\s|$)`)

func TestNoSectionNumbers(t *testing.T) {
	var bad []string
	for _, f := range docFiles(t) {
		for i, ln := range readLines(t, f) {
			if strings.Contains(ln, sectionSign) {
				bad = append(bad, f+":"+itoa(i+1)+": section sign")
			}
			if numberedHeadingRe.MatchString(ln) {
				bad = append(bad, f+":"+itoa(i+1)+": numbered heading")
			}
		}
	}
	for _, f := range codeFiles(t) {
		for i, ln := range readLines(t, f) {
			if strings.Contains(ln, sectionSign) {
				bad = append(bad, f+":"+itoa(i+1)+": section sign in code")
			}
		}
	}
	report(t, "section numbers", bad)
}

// ---- rule ids --------------------------------------------------------------

// idPrefixes are every rule-id family the docs have ever defined. A family
// that is deleted stays here so that a code comment still citing it fails.
var idPrefixes = []string{
	"SPD", "CLD", "E2E", "CN", "SH", "RW", "DN", "CM", "NP", "GW", "VW", "DS",
	"AR", "MD", "IR", "CT", "EU", "WV", "SW", "HL", "BM", "AG", "LG",
	"L", "R", "N", "T", "U",
}

var (
	// Definition forms: a heading ending in "— ID", a bold bullet "**[Dn]",
	// and a line-start "ID." / "ID ".
	headDefRe  = regexp.MustCompile(`^#{1,6} .*[\x{2013}\x{2014}] *([A-Z][A-Z0-9]{0,3})([0-9]+)[a-z]? *$`)
	bulletRe   = regexp.MustCompile(`^ {0,6}[-*] +`)
	brackDefRe = regexp.MustCompile(`^\**\[D([0-9]+)\]`)
	plainDefRe = regexp.MustCompile(`^\**([A-Z][A-Z0-9]{0,3})([0-9]+)[a-z]?\**[. ]`)
	dCiteRe    = regexp.MustCompile(`\[D([0-9]+)\]`)
	idCiteRe   = regexp.MustCompile(`\b(` + strings.Join(idPrefixes, "|") + `)([0-9]+)[a-z]?\b`)
)

func definedIds(t *testing.T) map[string]bool {
	defs := map[string]bool{}
	for _, f := range docFiles(t) {
		for _, ln := range docProse(readLines(t, f)) {
			if m := headDefRe.FindStringSubmatch(ln); m != nil {
				defs[m[1]+m[2]] = true
			}
			s := bulletRe.ReplaceAllString(ln, "")
			if m := brackDefRe.FindStringSubmatch(s); m != nil {
				defs["[D"+m[1]+"]"] = true
			}
			if m := plainDefRe.FindStringSubmatch(s); m != nil {
				defs[m[1]+m[2]] = true
			}
		}
	}
	return defs
}

func TestCitedRuleIdsAreDefined(t *testing.T) {
	defs := definedIds(t)
	if len(defs) == 0 {
		t.Fatal("no rule ids defined in doc/")
	}
	ok := allow(t, "ids")
	var bad []string
	check := func(f string, i int, ln string) {
		for _, m := range dCiteRe.FindAllStringSubmatch(ln, -1) {
			id := "[D" + m[1] + "]"
			if !defs[id] && !ok[id] {
				bad = append(bad, f+":"+itoa(i)+": "+id+" is not defined")
			}
		}
		for _, m := range idCiteRe.FindAllStringSubmatch(ln, -1) {
			id := m[1] + m[2]
			if !defs[id] && !ok[id] && !ok[m[1]] {
				bad = append(bad, f+":"+itoa(i)+": "+id+" is not defined")
			}
		}
	}
	for _, f := range docFiles(t) {
		for i, ln := range docProse(readLines(t, f)) {
			check(f, i, ln)
		}
	}
	for _, f := range codeFiles(t) {
		for i, ln := range readLines(t, f) {
			check(f, i+1, ln)
		}
	}
	report(t, "rule ids", bad)
}

// ---- identifiers and paths -------------------------------------------------

var (
	tickRe   = regexp.MustCompile("`([^`\n]+)`")
	pathRe   = regexp.MustCompile(`^[A-Za-z0-9_.\-]+(/[A-Za-z0-9_.\-]+)+$`)
	identRe  = regexp.MustCompile(`^-{0,2}[A-Za-z_][A-Za-z0-9_.]*$`)
	wordRe   = regexp.MustCompile(`[A-Za-z0-9_]+`)
	hasUpper = regexp.MustCompile(`[A-Z]`)
)

func corpus(t *testing.T) string {
	var sb strings.Builder
	for _, f := range walkFiles(t, func(p string) bool {
		return strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".sh") ||
			strings.HasSuffix(p, ".proto") || filepath.Base(p) == "Makefile" ||
			strings.HasSuffix(p, ".json")
	}) {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// isCodeName says whether a backticked token is a claim about the tree: a
// Go/proto identifier (an uppercase letter or an underscore in it), a dotted
// member, or a long flag. Plain lowercase words are tool names and prose.
func isCodeName(tok string) bool {
	if !identRe.MatchString(tok) {
		return false
	}
	body := strings.TrimLeft(tok, "-")
	if strings.HasPrefix(tok, "--") {
		return len(body) >= 3
	}
	if len(body) < 3 {
		return false
	}
	return hasUpper.MatchString(body) || strings.Contains(body, "_") || strings.Contains(body, ".")
}

func TestDocIdentifiersAndPathsExist(t *testing.T) {
	code := corpus(t)
	words := map[string]bool{}
	for _, w := range wordRe.FindAllString(code, -1) {
		words[w] = true
	}
	okIdent, okPath := allow(t, "ident"), allow(t, "path")
	// A bare file name (`osclient_fake.go`) is a claim that the file exists
	// somewhere in the tree, not that its name appears in some source.
	baseNames := map[string]bool{}
	for _, f := range walkFiles(t, func(string) bool { return true }) {
		baseNames[filepath.Base(f)] = true
	}
	var bad []string
	seen := map[string]bool{}
	for _, f := range docFiles(t) {
		prose := docProse(readLines(t, f))
		for _, i := range sortedKeys(prose) {
			ln := prose[i]
			for _, m := range tickRe.FindAllStringSubmatch(ln, -1) {
				tok := strings.TrimSpace(m[1])
				if seen[f+"|"+tok] {
					continue
				}
				switch {
				case pathRe.MatchString(tok) && strings.Contains(filepath.Base(tok), "."):
					if okPath[tok] {
						continue
					}
					if _, err := os.Stat(filepath.Join(root, tok)); err != nil {
						seen[f+"|"+tok] = true
						bad = append(bad, f+":"+itoa(i)+": path `"+tok+"` does not exist")
					}
				case isCodeName(tok):
					if okIdent[tok] {
						continue
					}
					body := strings.TrimLeft(tok, "-")
					// A flag is matched as text (its dashes split words); an
					// identifier must exist as a whole word, and a dotted name
					// as whole words part by part, so `InspectDn` is not found
					// inside `InspectDnNode`.
					var found bool
					switch {
					case strings.HasPrefix(tok, "--"):
						found = strings.Contains(code, body)
					case baseNames[body]:
						found = true
					default:
						found = true
						for _, w := range wordRe.FindAllString(body, -1) {
							if !words[w] {
								found = false
							}
						}
					}
					if !found {
						seen[f+"|"+tok] = true
						bad = append(bad, f+":"+itoa(i)+": `"+tok+"` is in no Go, proto, shell or Makefile source")
					}
				}
			}
		}
	}
	report(t, "identifiers and paths", bad)
}

// ---- values beside constants -----------------------------------------------

var valueRe = regexp.MustCompile("`[A-Z][A-Za-z0-9]*`\\s*(=|==|:=|:|\\(|is|of|=\\s*)\\s*[0-9]|`[A-Z][A-Za-z0-9]*`\\s+[0-9]+[^0-9a-z]")

func TestNoValuesBesideConstants(t *testing.T) {
	ok := allow(t, "value")
	var bad []string
	for _, f := range docFiles(t) {
		prose := docProse(readLines(t, f))
		for _, i := range sortedKeys(prose) {
			if m := valueRe.FindString(prose[i]); m != "" {
				name := strings.Trim(tickRe.FindString(m), "`")
				if ok[name] {
					continue
				}
				bad = append(bad, f+":"+itoa(i)+": a value beside `"+name+"`; the value lives in the code")
			}
		}
	}
	report(t, "values", bad)
}

// ---- history ---------------------------------------------------------------

var (
	dateRe    = regexp.MustCompile(`\b20[0-9]{2}-[01][0-9]-[0-3][0-9]\b`)
	historyRe = regexp.MustCompile(`(?i)\b(amended|previously|formerly|superseded|retired|used to|re-scoped)\b`)
)

func TestNoDatesInDocs(t *testing.T) {
	ok := allow(t, "history")
	var bad []string
	for _, f := range docFiles(t) {
		prose := docProse(readLines(t, f))
		for _, i := range sortedKeys(prose) {
			if d := dateRe.FindString(prose[i]); d != "" && !ok[d] {
				bad = append(bad, f+":"+itoa(i)+": date "+d)
			}
			if w := historyRe.FindString(prose[i]); w != "" {
				t.Logf("%s:%d: history word %q", f, i, w)
			}
		}
	}
	report(t, "dates", bad)
}

// ---- helpers ---------------------------------------------------------------

func itoa(i int) string { return strconv.Itoa(i) }

func report(t *testing.T, what string, bad []string) {
	t.Helper()
	if len(bad) == 0 {
		return
	}
	const max = 60
	shown := bad
	if len(shown) > max {
		shown = shown[:max]
	}
	t.Errorf("%s: %d problems\n%s", what, len(bad), strings.Join(shown, "\n"))
	if len(bad) > max {
		t.Errorf("... and %d more", len(bad)-max)
	}
}

// ---- layout ----------------------------------------------------------------

// TestLayoutNamesEveryPackage holds layout.md's directory tree to the Go
// packages of the repository: every directory that holds a Go file is named
// there by its path with a trailing slash (`agent/cnagent/`). Which file holds
// what is the code's; the package boundaries are the doc's.
func TestLayoutNamesEveryPackage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "doc", "layout.md"))
	if err != nil {
		t.Fatalf("read doc/layout.md: %v", err)
	}
	text := string(b)
	dirs := map[string]bool{}
	for _, f := range walkFiles(t, func(p string) bool { return strings.HasSuffix(p, ".go") }) {
		dirs[filepath.Dir(f)] = true
	}
	var bad []string
	for d := range dirs {
		if d == "." {
			continue
		}
		if !strings.Contains(text, "`"+d+"/`") {
			bad = append(bad, "`"+d+"/` is a Go package directory that doc/layout.md does not name")
		}
	}
	sort.Strings(bad)
	report(t, "layout", bad)
}
