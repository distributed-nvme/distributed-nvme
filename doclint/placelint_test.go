package doclint

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The checks of this file hold the markdown files to their places and the
// human docs to their readers (layout.md, Human docs and LLM docs):
//
//   - every markdown file is README.md at the root, a file under doc/human/
//     or a file under doc/llm/; the instruction files CLAUDE.md and
//     CLAUDE.local.md are not docs and are skipped by name;
//   - no two docs share a base name, since a citation names a doc by its
//     base name alone; the two README.md are the exception, and neither is
//     ever cited (citelint_test.go);
//   - the file part of every markdown link and image of every doc names a
//     file of the tree, read relative to the doc's own directory; a target
//     with a scheme is left alone and an anchor is not checked;
//   - a human doc refers to no LLM doc: no link or image whose target is
//     under doc/llm/, no LLM doc's file name as a word, bare or in backticks,
//     in prose or in a fence, where the bare name README.md is the root
//     README's too and passes while a path to the LLM one does not, and no
//     rule id in its prose.
//
// These checks take no exceptions.

var (
	// A markdown link or image target: what follows "](" up to the closing
	// parenthesis, in angle brackets or not, an optional title in double
	// quotes, single quotes or parentheses aside.
	linkRe   = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*\)`)
	schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// instructionFiles are the LLM's instruction files: not docs, and the local
// one is untracked but present.
var instructionFiles = map[string]bool{"CLAUDE.md": true, "CLAUDE.local.md": true}

func isPlaced(p string) bool {
	return p == "README.md" || strings.HasPrefix(p, "doc/human/") || strings.HasPrefix(p, "doc/llm/")
}

func TestMarkdownFilesArePlaced(t *testing.T) {
	var bad []string
	for _, f := range walkFiles(t, func(p string) bool { return strings.HasSuffix(p, ".md") }) {
		if instructionFiles[filepath.Base(f)] || isPlaced(f) {
			continue
		}
		bad = append(bad, f+": a markdown file is README.md at the root, under doc/human/ or under doc/llm/")
	}
	report(t, "placement", bad)
}

func TestDocBaseNamesAreUnique(t *testing.T) {
	byBase := map[string][]string{}
	for _, f := range docFiles(t) {
		byBase[filepath.Base(f)] = append(byBase[filepath.Base(f)], f)
	}
	var bad []string
	for base, paths := range byBase {
		if len(paths) > 1 && base != "README.md" {
			bad = append(bad, base+" is the base name of "+strings.Join(paths, " and "))
		}
	}
	sort.Strings(bad)
	report(t, "base names", bad)
}

// linkTargets yields the file part of every link and image target of a line
// as a path from the repository root, read relative to the doc's directory;
// a target with a scheme and a target that is an anchor alone are skipped.
func linkTargets(doc string, ln string) []string {
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(ln, -1) {
		target := m[1]
		if i := strings.Index(target, "#"); i >= 0 {
			target = target[:i]
		}
		if target == "" || schemeRe.MatchString(target) {
			continue
		}
		out = append(out, filepath.Clean(filepath.Join(filepath.Dir(doc), target)))
	}
	return out
}

func TestLinksResolve(t *testing.T) {
	var bad []string
	for _, f := range docFiles(t) {
		prose := docProse(readLines(t, f))
		for _, i := range sortedKeys(prose) {
			for _, target := range linkTargets(f, prose[i]) {
				if strings.HasPrefix(target, "..") {
					bad = append(bad, f+":"+itoa(i)+": the link "+target+" leaves the repository")
					continue
				}
				if _, err := os.Stat(filepath.Join(root, target)); err != nil {
					bad = append(bad, f+":"+itoa(i)+": the link target "+target+" does not exist")
				}
			}
		}
	}
	report(t, "links", bad)
}

func TestHumanDocsNameNoLlmDoc(t *testing.T) {
	// The LLM README is named by a path, never by its bare name, which is
	// the root README's too.
	nameRes := []*regexp.Regexp{regexp.MustCompile(`\bllm/README\.md\b`)}
	for _, f := range llmDocFiles(t) {
		if base := filepath.Base(f); base != "README.md" {
			nameRes = append(nameRes, regexp.MustCompile(`\b`+regexp.QuoteMeta(base)+`\b`))
		}
	}
	var bad []string
	for _, f := range humanDocFiles(t) {
		lines := readLines(t, f)
		prose := docProse(lines)
		for i, ln := range lines {
			where := f + ":" + itoa(i+1)
			for _, target := range linkTargets(f, ln) {
				if strings.HasPrefix(target, "doc/llm/") {
					bad = append(bad, where+": links to the LLM doc "+target)
				}
			}
			for _, re := range nameRes {
				if m := re.FindString(ln); m != "" {
					bad = append(bad, where+": names the LLM doc "+m)
				}
			}
			if _, isProse := prose[i+1]; !isProse {
				continue
			}
			if m := idCiteRe.FindString(ln); m != "" {
				bad = append(bad, where+": cites the rule id "+m)
			}
			if m := dCiteRe.FindString(ln); m != "" {
				bad = append(bad, where+": cites the rule id "+m)
			}
		}
	}
	report(t, "human docs", bad)
}
