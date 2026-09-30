// Layout lint — the doc/layout.md §2 directory tree against the Go files that
// exist.
//
// # Why this lives in ctl/
//
// For the reason doclint_test.go gives for itself: ctl/ is where the
// doc-versus-code cross-checks live, and a package of its own would have to
// enter the very tree this file checks. Nothing here imports `ctl`; it reads
// the repository from `..` (docRoot).
//
// # What drifts, and what this catches
//
// layout.md §2 names every non-test Go file of the repository, and until
// this file no test held that tree against the files that exist (doclint_test.go
// reads it only for rule ids), so a file that arrived with a change and was not
// added to the tree went unnoticed: common/name_parse.go, the three sweep.go
// files and agent/waitbudget.go all did, while §7 item 5 went on counting six
// common/ files against seven. The check is two-way over non-test Go files: every one
// the repository holds must be named by the tree, and every one the tree names
// must exist. Every mismatch is reported, not just the first. Non-Go entries
// (the docs, the shell suites, go.mod, bin/) are not checked, and neither are
// `_test.go` files, which the tree never lists (§2's closing prose: they are
// colocated inside each package).
//
// # How the tree is read
//
// The tree is the first fenced block under the "## 2." heading, and its first
// line is the repository root. Every other line is either an entry, which
// carries a connector ("├── " or "└── "), or a continuation of the entry
// above it: only "│" and spaces, then a "#" comment. An entry's depth is its
// connector's column / 4, and a directory entry (one name ending in "/") is
// the parent of the deeper entries that follow it. The name column —
// everything before an entry's "#" — holds one name, a path through a
// subdirectory ("workerctl/main.go") or a comma-separated list ("cluster.go,
// dn.go, …"). A directory entry may instead enumerate its files in its
// comment, on the entry line and on the continuation lines under it
// (agent/dnagent/, agent/cnagent/, integtest/gatewayctl/): there every bare
// "name.go" is a file of that directory, while a "dir/name.go" names some
// other directory's file and is skipped. A file entry's comment names nothing.
// A line of any other shape fails the test rather than being skipped, so a new
// way of writing the tree cannot hide the files it lists from this check.
//
// A directory named with no file under it or in its comment claims no Go
// files, so whatever Go files it holds are reported as missing: a driver whose
// files the tree does not enumerate is written as a path ("dnagentctl/main.go"),
// never as a bare "dnagentctl/".
//
// # The repository side
//
// A walk from the root that skips what the go tool skips — directories whose
// names begin with "." or "_", and testdata — plus every bin/ directory: the
// build outputs and the etcd download cache that .gitignore keeps out of the
// repository.
package ctl

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// layoutTreeHeading opens the section whose first fenced block is the tree.
const layoutTreeHeading = "## 2. "

// layoutConnectors mark an entry line.
var layoutConnectors = []string{"├── ", "└── "}

// layoutCommentGoRe finds a bare Go file name in a directory entry's comment.
// The leading class rejects a match inside a path ("agent/dm.go" belongs to
// another directory) or inside a longer word.
var layoutCommentGoRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])([A-Za-z0-9_]+\.go)\b`)

// layoutTree is what the §2 tree names: each Go file's slash-separated
// repository path, mapped to the layout.md line that names it.
type layoutTree struct {
	files map[string]int
	bad   []string // one message per line the reader could not place
}

// layoutTreeBlock returns the lines of the tree and the 1-based layout.md
// line number of its first line.
func layoutTreeBlock(layout string) ([]string, int, error) {
	lines := strings.Split(layout, "\n")
	sec := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, layoutTreeHeading) {
			sec = i
			break
		}
	}
	if sec < 0 {
		return nil, 0, fmt.Errorf("no %q heading", layoutTreeHeading)
	}
	open := -1
	for i := sec + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			break
		}
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		if open < 0 {
			open = i
			continue
		}
		return lines[open+1 : i], open + 2, nil
	}
	return nil, 0, fmt.Errorf("no closed fenced block under %q", layoutTreeHeading)
}

// readLayoutTree reads the §2 tree out of the text of layout.md.
func readLayoutTree(layout string) (layoutTree, error) {
	block, first, err := layoutTreeBlock(layout)
	if err != nil {
		return layoutTree{}, err
	}
	tr := layoutTree{files: map[string]int{}}
	badf := func(no int, why, ln string) {
		tr.bad = append(tr.bad, fmt.Sprintf("layout.md:%d: %s: %q", no, why, ln))
	}
	add := func(no int, path string) {
		if prev, dup := tr.files[path]; dup {
			badf(no, fmt.Sprintf("%s is already named at line %d", path, prev), path)
			return
		}
		tr.files[path] = no
	}
	// dirs[d] is the directory entry open at depth d, with its trailing "/";
	// commentDir is the directory whose continuation lines may list files
	// ("" after a file entry).
	var dirs []string
	commentDir := ""
	addComment := func(no int, comment string) {
		if commentDir == "" {
			return
		}
		for _, m := range layoutCommentGoRe.FindAllStringSubmatch(comment, -1) {
			add(no, commentDir+m[1])
		}
	}
	for i, ln := range block {
		no := first + i
		if i == 0 {
			name, _, _ := strings.Cut(ln, "#")
			if name = strings.TrimSpace(name); !strings.HasSuffix(name, "/") {
				badf(no, "the tree does not open with the root directory", ln)
			}
			continue
		}
		at, conn := -1, ""
		for _, c := range layoutConnectors {
			if j := strings.Index(ln, c); j >= 0 && (at < 0 || j < at) {
				at, conn = j, c
			}
		}
		if at < 0 {
			rest := strings.TrimLeft(ln, "│ ")
			if !strings.HasPrefix(rest, "#") {
				badf(no, "neither an entry nor a comment continuation", ln)
				continue
			}
			addComment(no, rest)
			continue
		}
		if strings.Trim(ln[:at], "│ ") != "" {
			badf(no, "text before the connector", ln)
			continue
		}
		col := utf8.RuneCountInString(ln[:at])
		if col%4 != 0 || col/4 > len(dirs) {
			badf(no, "connector at a column no open directory explains", ln)
			continue
		}
		dirs = dirs[:col/4]
		parent := strings.Join(dirs, "")
		name, comment, _ := strings.Cut(ln[at+len(conn):], "#")
		name = strings.TrimSpace(name)
		commentDir = ""
		if !strings.Contains(name, ",") && strings.HasSuffix(name, "/") {
			dirs = append(dirs, name)
			commentDir = parent + name
			addComment(no, comment)
			continue
		}
		for _, item := range strings.Split(name, ",") {
			item = strings.TrimSpace(item)
			if item == "" || strings.ContainsAny(item, " \t") {
				badf(no, "not a list of names", ln)
				break
			}
			if strings.HasSuffix(item, ".go") {
				add(no, parent+item)
			}
		}
	}
	return tr, nil
}

// layoutRepoGoFiles lists the repository's non-test Go files under root as
// slash-separated paths relative to it.
func layoutRepoGoFiles(root string) (map[string]bool, error) {
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") ||
				strings.HasPrefix(name, "_") || name == "testdata" || name == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	return out, err
}

// layoutDiff returns, sorted, the repository's Go files the tree does not
// name and the tree's Go files the repository does not hold.
func layoutDiff(tr layoutTree, repo map[string]bool) (missing, phantom []string) {
	for p := range repo {
		if _, ok := tr.files[p]; !ok {
			missing = append(missing, p)
		}
	}
	for p := range tr.files {
		if !repo[p] {
			phantom = append(phantom, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(phantom)
	return missing, phantom
}

// checkLayoutTree reports every disagreement between a layout.md text and
// the repository's Go files as one message each; none means they agree.
func checkLayoutTree(layout string, repo map[string]bool) ([]string, error) {
	tr, err := readLayoutTree(layout)
	if err != nil {
		return nil, err
	}
	msgs := append([]string(nil), tr.bad...)
	missing, phantom := layoutDiff(tr, repo)
	for _, p := range missing {
		msgs = append(msgs, fmt.Sprintf("%s exists but layout.md §2 does not name it", p))
	}
	for _, p := range phantom {
		msgs = append(msgs, fmt.Sprintf("layout.md:%d names %s, which does not exist", tr.files[p], p))
	}
	return msgs, nil
}

func readLayoutDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docRoot, "doc", "layout.md"))
	if err != nil {
		t.Fatalf("read doc/layout.md: %v", err)
	}
	return string(b)
}

func repoGoFiles(t *testing.T) map[string]bool {
	t.Helper()
	repo, err := layoutRepoGoFiles(docRoot)
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	if len(repo) == 0 {
		t.Fatalf("the walk from %s found no Go file", docRoot)
	}
	return repo
}

// TestLayoutTreeMatchesGoFiles is the lint: layout.md §2 names exactly the
// repository's non-test Go files.
func TestLayoutTreeMatchesGoFiles(t *testing.T) {
	msgs, err := checkLayoutTree(readLayoutDoc(t), repoGoFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		t.Error(m)
	}
}

// TestLayoutTreeReaderForms pins every form the reader accepts on a small tree
// of its own, so a reader that silently stopped seeing one of them cannot
// pass the lint by naming fewer files on both sides of nothing.
func TestLayoutTreeReaderForms(t *testing.T) {
	doc := "# x\n\n" + layoutTreeHeading + "Directory tree\n\n```\n" + strings.Join([]string{
		"root/                    # the root",
		"├── go.mod",
		"├── a/                   # a directory whose comment lists nothing",
		"│   ├── one.go           # a file entry's comment names nothing: two.go",
		"│   ├── x.go, y.go",
		"│                        # a file entry's continuation names nothing: z.go",
		"│   ├── sub/main.go      # a path through a subdirectory",
		"│   └── deep/            # files in the comment: p.go (not agent/q.go), r.go,",
		"│                        # s.go",
		"├── bin/                 # build outputs",
		"└── cmd/",
		"    └── tool/main.go",
	}, "\n") + "\n```\n\n## 3. Next\n\n```\n└── not/read.go\n```\n"
	tr, err := readLayoutTree(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range tr.bad {
		t.Errorf("unexpected: %s", m)
	}
	want := []string{
		"a/deep/p.go", "a/deep/r.go", "a/deep/s.go", "a/one.go", "a/sub/main.go",
		"a/x.go", "a/y.go", "cmd/tool/main.go",
	}
	var got []string
	for p := range tr.files {
		got = append(got, p)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files read:\n got %v\nwant %v", got, want)
	}
	if tr.files["a/one.go"] != 9 || tr.files["a/deep/s.go"] != 14 {
		t.Errorf("line numbers: a/one.go at %d (want 9), a/deep/s.go at %d (want 14)",
			tr.files["a/one.go"], tr.files["a/deep/s.go"])
	}

	// A stray prose line inside the tree is refused, not skipped.
	tr, err = readLayoutTree(strings.Replace(doc, "├── go.mod", "some prose", 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.bad) != 1 || !strings.Contains(tr.bad[0], "layout.md:7:") {
		t.Errorf("a prose line in the tree: got %q, want one complaint at line 7", tr.bad)
	}
}

// TestLayoutTreeLintCatchesStaleCopy runs the lint on deliberately stale
// copies of the real layout.md: one that lost an entry and one that names a
// file nobody wrote. Each must come back with exactly its one complaint.
func TestLayoutTreeLintCatchesStaleCopy(t *testing.T) {
	doc, repo := readLayoutDoc(t), repoGoFiles(t)
	if !repo["common/name_parse.go"] {
		t.Fatal("common/name_parse.go is gone: pick another file for this test")
	}
	block, _, err := layoutTreeBlock(doc)
	if err != nil {
		t.Fatal(err)
	}
	var entry string
	for _, ln := range block {
		if strings.Contains(ln, "── name_parse.go ") {
			entry = ln
		}
	}
	if entry == "" {
		t.Fatal("the tree has no name_parse.go entry line to drop")
	}
	cases := []struct {
		name  string
		stale string
		want  string
	}{
		{"lost entry", strings.Replace(doc, entry+"\n", "", 1),
			"common/name_parse.go exists but layout.md §2 does not name it"},
		{"phantom entry", strings.Replace(doc, entry, entry+"\n│   ├── ghost.go", 1),
			"names common/ghost.go, which does not exist"},
	}
	for _, c := range cases {
		msgs, err := checkLayoutTree(c.stale, repo)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(msgs) != 1 || !strings.Contains(msgs[0], c.want) {
			t.Errorf("%s: got %q, want exactly one message containing %q", c.name, msgs, c.want)
		}
	}
	if _, err := checkLayoutTree(strings.Replace(doc, layoutTreeHeading, "## 9. ", 1), repo); err == nil {
		t.Error("a layout.md without its §2 heading was read without error")
	}
}
