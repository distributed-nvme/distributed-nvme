package doclint

import (
	"regexp"
	"testing"
)

// The checks of this file hold the comments and message strings of the code
// files to the present:
//
//   - code is named by identifier, never by a line number, which goes stale
//     with the next edit above it;
//   - no dates: history lives in git;
//   - no citation of a note kept outside the repository.
//
// Each check matches the one form the tree used: a "path.ext:N" pointer into
// a .go, .sh, .proto or .md file, an ISO date, the words outsideNoteRe
// matches. A bare ":N", a pointer into other source, or a note cited by its
// path passes them.
//
// Exceptions go in allowlist.txt under the checks pointer, history and note.

var (
	// A file name, a colon and a line number.
	linePointerRe = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|sh|proto|md):[0-9]+`)
	outsideNoteRe = regexp.MustCompile(`(?i)\b(?:memory|lab) notes?\b`)
)

func TestNoLinePointersInCode(t *testing.T) {
	ok := allow(t, "pointer")
	var bad []string
	for _, f := range codeFiles(t) {
		for i, ln := range readLines(t, f) {
			if m := linePointerRe.FindString(ln); m != "" && !ok[m] {
				bad = append(bad, f+":"+itoa(i+1)+": line pointer "+m+
					"; name the identifier instead")
			}
		}
	}
	report(t, "line pointers in code", bad)
}

func TestNoDatesInCode(t *testing.T) {
	ok := allow(t, "history")
	var bad []string
	for _, f := range codeFiles(t) {
		for i, ln := range readLines(t, f) {
			if d := dateRe.FindString(ln); d != "" && !ok[d] {
				bad = append(bad, f+":"+itoa(i+1)+": date "+d)
			}
		}
	}
	report(t, "dates in code", bad)
}

func TestNoOutsideCitationsInCode(t *testing.T) {
	ok := allow(t, "note")
	var bad []string
	for _, f := range codeFiles(t) {
		for i, ln := range readLines(t, f) {
			if m := outsideNoteRe.FindString(ln); m != "" && !ok[m] {
				bad = append(bad, f+":"+itoa(i+1)+
					": cites a note that is not in the repository")
			}
		}
	}
	report(t, "citations of notes outside the repository", bad)
}
