package doclint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The checks of this file hold the pointers and citations of the code files
// and the docs to what a reader can follow in the tree:
//
//   - a pointer of the form "identifier (path/file.ext)" names a file of the
//     tree that holds the identifier;
//   - what follows a document's name and a comma is a rule id or a heading
//     text of that document.
//
// Exceptions go in allowlist.txt under the checks identptr and heading.

var (
	identPointerRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.]*) \(([A-Za-z0-9_./-]+\.(?:go|sh|proto))\)`)
	docCiteRe      = regexp.MustCompile("`?\\b([A-Za-z_][A-Za-z0-9_-]*\\.md)`?,\\s+")
	docNameRe      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*\.md\b`)
	headingRe      = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*$`)
	spaceRe        = regexp.MustCompile(`\s+`)
)

// TestIdentifierPointersResolve: "ProbeSubsystem (agent/nvmet.go)" must name a
// file of the tree — by its path, or by a base name only one file carries —
// that holds the identifier as a whole word; of a dotted name, its last part.
// Only tokens that read as code are checked (isCodeName), so prose in front of
// a parenthesised file name is left alone.
func TestIdentifierPointersResolve(t *testing.T) {
	ok := allow(t, "identptr")
	byBase := map[string][]string{}
	isFile := map[string]bool{}
	for _, f := range walkFiles(t, func(string) bool { return true }) {
		isFile[f] = true
		byBase[filepath.Base(f)] = append(byBase[filepath.Base(f)], f)
	}
	words := map[string]map[string]bool{}
	wordsOf := func(f string) map[string]bool {
		if w, done := words[f]; done {
			return w
		}
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		w := map[string]bool{}
		for _, tok := range wordRe.FindAllString(string(b), -1) {
			w[tok] = true
		}
		words[f] = w
		return w
	}
	var bad []string
	for _, f := range codeFiles(t) {
		for i, ln := range readLines(t, f) {
			for _, m := range identPointerRe.FindAllStringSubmatch(ln, -1) {
				ident, path := m[1], m[2]
				if !isCodeName(ident) || ok[ident] {
					continue
				}
				var cands []string
				switch {
				case isFile[path]:
					cands = []string{path}
				case !strings.Contains(path, "/") && len(byBase[path]) == 1:
					cands = byBase[path]
				}
				where := f + ":" + itoa(i+1) + ": " + ident + " (" + path + ")"
				if len(cands) == 0 {
					bad = append(bad, where+": names no one file of the tree")
					continue
				}
				last := ident[strings.LastIndex(ident, ".")+1:]
				if !wordsOf(cands[0])[last] {
					bad = append(bad, where+": the file does not hold "+last)
				}
			}
		}
	}
	report(t, "identifier pointers", bad)
}

// normCite drops the backticks and double quotes a citation or a heading may
// carry and collapses its white space.
func normCite(s string) string {
	s = strings.NewReplacer("`", "", "\"", "", "“", "", "”", "").Replace(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// commentBlocks joins each run of consecutive comment lines of a code file
// into one text, so a citation wrapped across lines reads whole. The key is
// the block's first line.
func commentBlocks(lines []string, goStyle bool) map[int]string {
	out := map[int]string{}
	start, cur := 0, []string(nil)
	flush := func() {
		if cur != nil {
			out[start] = strings.Join(cur, " ")
			cur = nil
		}
	}
	for i, ln := range lines {
		s := strings.TrimSpace(ln)
		var body string
		switch {
		case strings.HasPrefix(s, "//"):
			body = s[2:]
		case !goStyle && strings.HasPrefix(s, "#"):
			body = s[1:]
		default:
			flush()
			continue
		}
		if cur == nil {
			start = i + 1
		}
		cur = append(cur, strings.TrimSpace(body))
	}
	flush()
	return out
}

// TestCitedHeadingsExist: in a comment of a code file and in a doc, what
// follows a document's name and a comma is a rule id (TestCitedRuleIdsAreDefined
// checks that it is defined) or a heading text of that document; a bold
// lead-in of a paragraph under the heading may follow and is not checked. In a
// doc, a list of document names and a lower-case continuation are prose, not
// citations.
func TestCitedHeadingsExist(t *testing.T) {
	ok := allow(t, "heading")
	heads := map[string][]string{}
	for _, f := range docFiles(t) {
		base := filepath.Base(f)
		heads[base] = nil
		for _, ln := range docProse(readLines(t, f)) {
			if m := headingRe.FindStringSubmatch(ln); m != nil {
				heads[base] = append(heads[base], normCite(m[1]))
			}
		}
	}
	var bad []string
	check := func(where string, text string, inDoc bool) {
		for _, loc := range docCiteRe.FindAllStringSubmatchIndex(text, -1) {
			doc := text[loc[2]:loc[3]]
			rest := normCite(text[loc[1]:])
			if rest == "" || ok[doc] {
				continue
			}
			if inDoc && (docNameRe.MatchString(rest) ||
				unicode.IsLower([]rune(rest)[0])) {
				continue
			}
			hs, known := heads[doc]
			if !known {
				bad = append(bad, where+": "+doc+" is not a document")
				continue
			}
			if loc := idCiteRe.FindStringIndex(rest); loc != nil && loc[0] == 0 {
				continue
			}
			if strings.HasPrefix(rest, "[D") {
				continue
			}
			found := false
			for _, h := range hs {
				if h != "" && strings.HasPrefix(rest, h) {
					found = true
					break
				}
			}
			if !found {
				if len(rest) > 48 {
					rest = rest[:48]
				}
				bad = append(bad, where+": \""+rest+"\" starts with no heading of "+doc)
			}
		}
	}
	for _, f := range codeFiles(t) {
		blocks := commentBlocks(readLines(t, f), strings.HasSuffix(f, ".go"))
		for start, text := range blocks {
			check(f+":"+itoa(start), text, false)
		}
	}
	for _, f := range docFiles(t) {
		prose := docProse(readLines(t, f))
		start, cur := 0, []string(nil)
		flush := func() {
			if cur != nil {
				check(f+":"+itoa(start), strings.Join(cur, " "), true)
				cur = nil
			}
		}
		for _, i := range sortedKeys(prose) {
			ln := strings.TrimSpace(prose[i])
			if ln == "" {
				flush()
				continue
			}
			if cur == nil {
				start = i
			}
			cur = append(cur, ln)
		}
		flush()
	}
	report(t, "cited headings", bad)
}
