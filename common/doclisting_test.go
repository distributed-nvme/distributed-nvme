package common

// Reference listings — the two docs whose listing IS the file.
//
// grpc.md §3 lists `common/interceptor.go` and log.md §4 lists `common/log.go`,
// each as the complete file, and other carriers lean on that being exact:
// osclient.md §5 and dnagent.md's constant listing say the byte-identity of
// these two is pinned (theirs is not), and gateway/traceid.go keeps the
// gateway's trace-id mint out of interceptor.go so that the file stays the
// grpc.md §3 reference verbatim. This test is that pin: the first ```go block
// of each section must equal the committed file byte for byte, so an edit to
// either side alone fails here.
//
// osclient.md §5's listing of `common/osclient.go` is deliberately not pinned:
// that section says it is semantically complete only, and that the file is
// authoritative for comment text and declaration order.
//
// It lives in common/ rather than beside ctl/doclint_test.go because the
// pinned files are this package's; like that lint it reads the repository from
// `..`.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refListing names one pinned pair: the doc, the source file its section lists,
// and the section heading that introduces the listing.
type refListing struct {
	doc     string // repository-relative doc path
	src     string // repository-relative source path
	heading string // the exact `## ` heading line of the listing's section
}

var refListings = []refListing{
	{"doc/grpc.md", "common/interceptor.go",
		"## 3. Reference implementation — `common/interceptor.go` (complete)"},
	{"doc/log.md", "common/log.go",
		"## 4. Reference implementation — `common/log.go` (complete file)"},
}

// extractRefListing returns the body of the first ```go block in the section
// that `heading` opens, with a trailing newline, exactly as a Go file holding
// it would read. The heading must occur exactly once, and the block must open
// and close before the next `## ` heading.
func extractRefListing(doc, heading string) (string, error) {
	lines := strings.Split(doc, "\n")
	at := -1
	for i, ln := range lines {
		if ln == heading {
			if at >= 0 {
				return "", fmt.Errorf("heading %q occurs twice (lines %d and %d)",
					heading, at+1, i+1)
			}
			at = i
		}
	}
	if at < 0 {
		return "", fmt.Errorf("heading %q not found", heading)
	}
	open := -1
	for i := at + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			break
		}
		if lines[i] == "```go" {
			open = i
			break
		}
	}
	if open < 0 {
		return "", fmt.Errorf("no ```go block under heading %q (line %d)",
			heading, at+1)
	}
	for i := open + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			break
		}
		if lines[i] == "```" {
			return strings.Join(lines[open+1:i], "\n") + "\n", nil
		}
	}
	return "", fmt.Errorf("```go block opened at line %d is not closed "+
		"inside its section", open+1)
}

// firstDiff describes where want and got first part, by 1-based line, or
// returns "" when they are equal.
func firstDiff(want, got string) string {
	if want == got {
		return ""
	}
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return fmt.Sprintf("line %d:\n  listing: %q\n  file:    %q",
				i+1, w[i], g[i])
		}
	}
	return fmt.Sprintf("the end: the listing is %d bytes, the file %d",
		len(want), len(got))
}

// TestRefListingsVerbatim pins grpc.md §3 to common/interceptor.go and log.md
// §4 to common/log.go, byte for byte.
func TestRefListingsVerbatim(t *testing.T) {
	for _, rl := range refListings {
		doc, err := os.ReadFile(filepath.Join("..", rl.doc))
		if err != nil {
			t.Fatalf("read %s: %v", rl.doc, err)
		}
		src, err := os.ReadFile(filepath.Join("..", rl.src))
		if err != nil {
			t.Fatalf("read %s: %v", rl.src, err)
		}
		listing, err := extractRefListing(string(doc), rl.heading)
		if err != nil {
			t.Errorf("%s: %v", rl.doc, err)
			continue
		}
		if d := firstDiff(listing, string(src)); d != "" {
			t.Errorf("%s's listing and %s differ at %s", rl.doc, rl.src, d)
		}
	}
}

// TestExtractRefListing holds the extractor to the cases a doc edit can
// produce, so that a pin that silently compared nothing cannot pass above.
func TestExtractRefListing(t *testing.T) {
	const h = "## 3. Reference implementation — `x.go`"
	cases := []struct {
		name, doc, want, errSub string
	}{
		{"found", "# t\n\n" + h + "\n\ntext\n\n```go\npackage x\n\nfunc F() {}\n```\n\n## 4. Next\n",
			"package x\n\nfunc F() {}\n", ""},
		{"first go block only", h + "\n```go\na\n```\n```go\nb\n```\n", "a\n", ""},
		{"heading missing", "## 3. Other\n```go\na\n```\n", "", "not found"},
		{"heading twice", h + "\n```go\na\n```\n" + h + "\n", "", "occurs twice"},
		{"block in the next section", h + "\ntext\n## 4. Next\n```go\na\n```\n", "",
			"no ```go block"},
		{"untyped fence is not the listing", h + "\n```\na\n```\n", "", "no ```go block"},
		{"unclosed", h + "\n```go\na\n", "", "not closed"},
		{"closed only after the next heading", h + "\n```go\na\n## 4. Next\n```\n", "",
			"not closed"},
	}
	for _, c := range cases {
		got, err := extractRefListing(c.doc, h)
		switch {
		case c.errSub != "":
			if err == nil || !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.errSub)
			}
		case err != nil:
			t.Errorf("%s: err = %v", c.name, err)
		case got != c.want:
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if d := firstDiff("a\nb\n", "a\nc\n"); !strings.Contains(d, "line 2") {
		t.Errorf("firstDiff on a changed line = %q, want line 2", d)
	}
	if d := firstDiff("a\n", "a\nb\n"); !strings.Contains(d, "line 2") {
		t.Errorf("firstDiff on an extra line = %q, want line 2", d)
	}
	if d := firstDiff("a\n", "a"); !strings.Contains(d, "2 bytes, the file 1") {
		t.Errorf("firstDiff on a lost trailing newline = %q", d)
	}
	if d := firstDiff("a\n", "a\n"); d != "" {
		t.Errorf("firstDiff on equal input = %q, want empty", d)
	}
}
