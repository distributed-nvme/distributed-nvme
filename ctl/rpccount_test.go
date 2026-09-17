// Prose lint — the size of `service Gateway`, hand-copied into documents and
// comments.
//
// # Who owns what
//
// The RPC count of `service Gateway` is asserted in two places, and they check
// different things:
//
//   - completeness_test.go (CT-T1) pins it as a Go literal, on
//     `pb.Gateway_ServiceDesc.Methods`, on the `rpcToCmd` and `leafFlags`
//     tables transcribed from dnvctl.md §5, and on the RPC leaves of the cobra
//     tree NewRootCmd builds. It is what fails when an RPC is added to the
//     service and not to the CLI. It says nothing about any document.
//
//   - this file owns the PROSE carriers: the same number written out in
//     README.md, in `doc/*.md` and in Go comments and help strings. It states
//     no count of its own — it reads `len(pb.Gateway_ServiceDesc.Methods)` and
//     compares every carrier against that — so it cannot itself become one
//     more hand-copied literal. `Methods` is not by itself a synonym for "the
//     RPC surface": grpc.ServiceDesc files streaming RPCs under `Streams`
//     instead, as DiskNodeAgent's `Check*` are filed. `service Gateway` has an
//     empty `Streams` today, and TestProseRpcCountMatchesService fails on that
//     assumption directly, so the first streaming Gateway RPC names the
//     decision to make rather than reporting every freshly-corrected sentence
//     in the tree as stale.
//
// Neither subsumes the other. Renaming one RPC, or deleting one and adding
// another, leaves every count right and only CT-T1 notices. Adding an RPC does
// fail CT-T1 — on its own literal — and updating that literal and its tables
// makes it green again with every sentence in the tree still stale; the
// sentences are what this file is for.
//
// It lives in ctl/ for the reason doclint_test.go's header gives at length: this
// is where doc-versus-generated-code cross-checks already live, and the review
// ledger asked for the scan "next to the existing table tests". It reads no
// `ctl` identifier, and reads the repository from `..`. It enumerates its own
// corpus rather than sharing doclint_test.go's `docFiles` helper, because the
// corpus here also covers Go files and because the two lints should stay
// independent: narrowing one must not silently narrow the other.
//
// # What counts as a carrier
//
// A carrier is a decimal number that the text presents as the size of the whole
// Gateway RPC surface. Three shapes are recognised, in a window of the line
// plus the one after it, so that a phrase broken across two lines still matches
// (the carrier is reported at the line holding the number):
//
//	<n> [qualifiers] RPCs | methods | calls      "all 59 RPCs of `service Gateway`"
//	<n>-RPC | -method | -call                    "a 59-RPC sweep"
//	<n> [qualifiers] RPC | method | call <head>  "a map of all 59 RPC names"
//
// The qualifiers are a closed set — `service`, `Gateway`, `unary` and a `§`
// section reference, with or without backticks — and <head> is a closed set of
// plural head nouns. The closed qualifier set is what makes a match mean THIS
// service: another service's count is written with that service's name against
// the noun, as dnagent_integtest.md §1 writes "All 8 `DiskNodeAgent` RPCs", and
// no qualifier admits such a name. The number must also not be preceded by a
// word character, `§`, `#`, `.`, `-` or `/`, which is what keeps `§8 RPC`,
// `§10.11 steps` and `it-sweep-59` out.
//
// Markdown bold is removed from the window before matching (rpcCountStrip), so
// dnvctl.md §7.5's "registers **all 59** `service Gateway` methods" is a
// carrier exactly like its unbolded twin in integtest/fakegateway. The tally
// line "4+6+…+3 = **59**." stays quiet: dropping the `**` leaves a number with
// no noun after it, which is not a carrier for the reason below.
//
// Any number in one of those shapes is a carrier, not only the current count:
// a stale `58 RPCs` fires, which is the whole point. Because the shapes are
// generic, a sentence that states a DIFFERENT count next to one of those nouns
// is a carrier too and must be reworded rather than left to fail — gateway.md
// §6's complement of the agent-call matrix was one, and now names no number.
//
// # What this deliberately does not check
//
// A bare literal with no RPC noun beside it is not a carrier: a row index in
// the §7.10 sweep table (`| 59 | spare switch …`), a tally line
// (`4+6+…+3 = **59**`), `len == 59` quoted from a test. Neither is a subset
// count, nor the total a ratio is taken over: `58 of 59` and `55 of the 58
// requests` differ only in which of the two numbers is the whole surface, and
// nothing in either sentence says which. That is why the noun is required
// rather than nearness to the word RPC — without it the check would have to
// know which number in a sentence is the total, and it cannot.
//
// Spelled-out counts ("the ten agent calls", "the four interceptors") are not
// scanned. The remedy for those is the enumeration beside the number, not a
// lint: a number whose list is next to it cannot drift on its own.
//
// Shell suites are out of scope, and two of them state the count:
// `integtest/dnvctl_test.sh` in its header (:22) and again in the §7.10 audit
// stage, which pins it (:1193-1209), and `integtest/gateway_test.sh` in its
// header (:21, "every one of the 59 RPCs"). A shell suite cannot import `pb`,
// so a check that derives the number cannot be written there — it would have
// to read it out of a driver.
//
// This file's own samples below would otherwise be carriers, so the scan skips
// this one file. The generated `pb/*.pb.go` are skipped too: protoc copies
// schema.proto's comments into them verbatim — the `// Just a placeholder`
// above `message BdevFeatureNone` stands word for word in schema.pb.go, and a
// comment above `service Gateway` would land twice in schema_grpc.pb.go, once
// per generated interface — so scanning them would report the proto's own
// sentence again at lines no one can edit.
// `pb/schema.proto` itself IS scanned: it is where `service Gateway` is
// defined, which makes it the likeliest new home for such a sentence, and it
// carries none today. Beyond those, the corpus is README.md, `doc/*.md` (the
// glob is flat; `doc/` has no subdirectory) and every other `.go` file in the
// tree, with `.git/`, `bin/`, `integtest/bin/` and `tmp_doc/` not walked.
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

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// rpcCountRoot is the repository root as seen from the ctl/ package directory,
// where `go test` runs.
const rpcCountRoot = ".."

// rpcCountSelf is excluded from the corpus: the sample tables below hold
// deliberately stale phrases.
const rpcCountSelf = "ctl/rpccount_test.go"

// The three carrier shapes. rpcCountLead is what may precede the number: the
// start of the window, or any character that is not a word character, `§`,
// `#`, `.`, `-` or `/`. That is what keeps a section reference (`§8 RPCs`), a
// hyphenated id (`it-sweep-59`) and the tail of a longer number out of the
// capture. rpcCountQual is the closed qualifier set that may sit between the
// number and the noun.
const (
	rpcCountLead = `(?:^|[^0-9A-Za-z_§#./-])`
	rpcCountQual = "(?:`?(?:service|Gateway|unary|§[0-9]+(?:\\.[0-9]+)*)`? )"
	rpcCountHead = `(?:names|leaves|rows|steps|commands|invocations|requests)`
)

var rpcCountShapes = []*regexp.Regexp{
	// "59 RPCs", "59 `Gateway` methods", "59 unary calls".
	regexp.MustCompile(rpcCountLead + `([0-9]+)[ -]` + rpcCountQual +
		`{0,3}(?:RPCs|methods|calls)\b`),
	// "a 59-RPC sweep".
	regexp.MustCompile(rpcCountLead + `([0-9]+)-(?:RPC|method|call)\b`),
	// "all 59 RPC names", "the 59 RPC leaves".
	regexp.MustCompile(rpcCountLead + `([0-9]+) ` + rpcCountQual +
		`{0,3}(?:RPC|method|call) ` + rpcCountHead + `\b`),
}

// rpcCountSite is one carrier: where it is, what it says, and the phrase to
// quote back so the failure can be found without opening the file.
type rpcCountSite struct {
	file string
	line int
	n    int
	text string
}

func (s rpcCountSite) String() string {
	return fmt.Sprintf("%s:%d: %q", s.file, s.line, s.text)
}

// rpcCountStrip removes the leading indentation, the Go comment marker and
// markdown bold markers, so that a wrapped phrase reads as one line, a number
// at the start of a comment line is still preceded by nothing, and a bolded
// number is read as its digits.
//
// Bold is the only emphasis removed: `**` is what hid dnvctl.md §7.5's
// `**all 59**` from shape 1. Single `*` and `_` are left alone, because `*` is
// a pointer marker and `_` a word character in the Go half of the corpus, so
// removing them would corrupt identifiers inside the window.
func rpcCountStrip(line string) string {
	line = strings.TrimLeft(line, " \t")
	line = strings.TrimPrefix(line, "//")
	line = strings.ReplaceAll(line, "**", "")
	return strings.TrimLeft(line, " \t")
}

// rpcCountMatch is one carrier found in a two-line window, positioned by its
// byte offset in that window.
type rpcCountMatch struct {
	col  int
	n    int
	text string
}

// rpcCountMatches reports every carrier whose number lies in cur. next is the
// following line, joined so a wrapped phrase matches; a carrier whose number
// lies in next is left for next's own window, so nothing is reported twice.
func rpcCountMatches(cur, next string) []rpcCountMatch {
	cur = rpcCountStrip(cur)
	window := cur
	if next != "" {
		window = cur + " " + rpcCountStrip(next)
	}
	seen := map[int]bool{}
	var out []rpcCountMatch
	for _, re := range rpcCountShapes {
		for _, m := range re.FindAllStringSubmatchIndex(window, -1) {
			if m[2] >= len(cur) || seen[m[2]] {
				continue
			}
			seen[m[2]] = true
			n, err := strconv.Atoi(window[m[2]:m[3]])
			if err != nil {
				continue
			}
			out = append(out, rpcCountMatch{
				col:  m[2],
				n:    n,
				text: window[m[2]:m[1]],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].col < out[j].col })
	return out
}

// rpcCountCorpus lists every file scanned, repo-relative: README.md, every
// doc/*.md, pb/schema.proto, and every non-generated Go file in the tree.
// Build output, the review scratch directory and .git are skipped, as are the
// generated `.pb.go` and this file — see the header for why each is out.
func rpcCountCorpus(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(rpcCountRoot,
		func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(rpcCountRoot, path)
			if relErr != nil {
				return relErr
			}
			if d.IsDir() {
				switch rel {
				case ".git", "bin", "doc", "tmp_doc", "integtest/bin":
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case rel == rpcCountSelf:
			case rel == "README.md":
				out = append(out, rel)
			case strings.HasSuffix(rel, ".pb.go"):
			case strings.HasSuffix(rel, ".go"):
				out = append(out, rel)
			case strings.HasSuffix(rel, ".proto"):
				out = append(out, rel)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("walk %s: %v", rpcCountRoot, err)
	}
	docs, err := filepath.Glob(filepath.Join(rpcCountRoot, "doc", "*.md"))
	if err != nil {
		t.Fatalf("glob doc/*.md: %v", err)
	}
	for _, path := range docs {
		out = append(out, filepath.Join("doc", filepath.Base(path)))
	}
	sort.Strings(out)
	return out
}

// rpcCountScan returns every carrier in the corpus, in file and line order.
func rpcCountScan(t *testing.T) []rpcCountSite {
	t.Helper()
	var out []rpcCountSite
	for _, rel := range rpcCountCorpus(t) {
		b, err := os.ReadFile(filepath.Join(rpcCountRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			next := ""
			if i+1 < len(lines) {
				next = lines[i+1]
			}
			for _, m := range rpcCountMatches(line, next) {
				out = append(out, rpcCountSite{
					file: rel, line: i + 1, n: m.n, text: m.text,
				})
			}
		}
	}
	return out
}

// TestProseRpcCountMatchesService is the tripwire: every sentence that states
// how many RPCs `service Gateway` has must state the number the generated
// descriptor actually has. Every stale carrier is reported, not just the first
// — they arrive in families, one per document that describes the surface.
func TestProseRpcCountMatchesService(t *testing.T) {
	// `Methods` holds the unary RPCs only; a streaming one is filed under
	// `Streams` instead, as DiskNodeAgent's `Check*` are. `service Gateway`
	// has an empty `Streams` today, so `Methods` is its whole surface — but
	// only today. Fail on that assumption here, because on the day it breaks
	// a carrier reading "RPCs" needs the new total while one qualified
	// "unary" keeps the old one, and the loop below cannot tell them apart.
	if n := len(pb.Gateway_ServiceDesc.Streams); n != 0 {
		t.Fatalf("service Gateway gained %d streaming RPC(s): `want` below "+
			"must become Methods+Streams, and every carrier that qualifies "+
			"the count with `unary` (grep the corpus) has to be reread "+
			"before it is renumbered", n)
	}
	want := len(pb.Gateway_ServiceDesc.Methods)
	for _, site := range rpcCountScan(t) {
		if site.n != want {
			t.Errorf("%s says %d, but service Gateway has %d methods",
				site, site.n, want)
		}
	}
}

// TestRpcCountScanReachesItsAnchors is the guard against a vacuous scan. A
// matcher that stopped matching — a reworded shape, a corpus that lost a
// directory — would leave TestProseRpcCountMatchesService green over nothing.
// These files describe the RPC surface as their subject matter and each carry
// the count; if one of them legitimately stops naming it, delete it from this
// list in the same commit.
//
// integtest/fakegateway/main.go is on the list although grpc.md §4 says the
// integtest drivers are not dnv components: it is the densest carrier file in
// the tree, and it sits outside the "README.md and doc/*.md" the review ledger
// asked for, so a later narrowing of the corpus would drop it and its
// neighbours silently. Anchoring it makes that narrowing fail here.
func TestRpcCountScanReachesItsAnchors(t *testing.T) {
	anchors := []string{
		"README.md",
		"doc/dnvctl.md",
		"doc/gateway.md",
		"cmd/dnv-gateway/main.go",
		"ctl/root.go",
		"gateway/server.go",
		"integtest/fakegateway/main.go",
	}
	found := map[string]int{}
	for _, site := range rpcCountScan(t) {
		found[site.file]++
	}
	for _, rel := range anchors {
		if found[rel] == 0 {
			t.Errorf("%s carries no RPC count any more: either the matcher "+
				"stopped seeing it or the file stopped stating it", rel)
		}
	}
}

// rpcCountSample is one line of the tree, quoted verbatim, with the file it
// was taken from so a reader can check the quote. The two stale positives are
// the exception and are marked as invented: the tree does say 58 — `58 of 59`,
// `58 requests`, quoted on the quiet side below — but the scan reports no 58
// CARRIER anywhere in the corpus, so a fires row reading 58 can only have been
// written here.
type rpcCountSample struct {
	name string
	from string
	cur  string
	next string
	want int
}

// TestRpcCountMatcherIsPrecise pins what "in RPC context" means, in both
// directions, on real lines. The positives are the shapes the tree uses —
// including two stale ones, because a wrong count next to the noun has to fire
// or the tripwire would only ever find counts that are already right. The
// negatives are numbers that sit near the word RPC without claiming to be the
// size of the service.
func TestRpcCountMatcherIsPrecise(t *testing.T) {
	fires := []rpcCountSample{
		{"plain", "README.md",
			"  `doc/gateway.md`: all 59 RPCs of `service Gateway` over " +
				"`etcdutil`'s STM", "", 59},
		{"backticked service", "doc/dnvctl.md",
			"3. **Surface = exactly the 59 `service Gateway` RPCs.** One " +
				"command per RPC,", "", 59},
		{"backticked service name", "doc/grpc.md",
			"  for the dnvctl suite: it serves all 59 `Gateway` methods " +
				"behind both server", "", 59},
		{"bare methods", "gateway/server.go",
			"// carries no state. Every one of the 59 methods is overridden " +
				"below, so it", "", 59},
		{"section qualifier", "doc/architecture.md",
			"in `dnvctl.md` (a noun-grouped tree over the 59 §8 RPCs; the " +
				"§11.4 userspace", "", 59},
		{"unary calls, wrapped", "gateway/traceid.go",
			"// `service Gateway` has no streaming RPC today — schema.proto " +
				"declares 59",
			"// unary calls and not one `stream`, as dnvctl.md §2.2 states " +
				"from the client", 59},
		{"compound", "doc/dnvctl.md",
			"14. **Coverage: a 59-RPC sweep** asserting every request proto " +
				"on the wire,", "", 59},
		{"headed noun", "doc/dnvctl.md",
			"* **CT-T1 — completeness.** An `rpcToCmd` map of all 59 RPC " +
				"names →", "", 59},
		{"bolded count, wrapped", "doc/dnvctl.md",
			"`fakegateway --grpc-address <ip:port> --dir <dir>` — registers " +
				"**all 59**",
			"`service Gateway` methods (all unary) behind the mandatory " +
				"`grpc.md` §4 server", 59},
		{"wrapped over two lines", "ctl/root.go",
			"// Package ctl is the dnv operator CLI (dnvctl.md). It exposes " +
				"exactly the 59",
			"// RPCs of `service Gateway`, one command per RPC, grouped by " +
				"noun:", 59},
		{"stale count still fires", "invented",
			"  `doc/gateway.md`: all 58 RPCs of `service Gateway` over " +
				"`etcdutil`'s STM", "", 58},
		{"stale compound still fires", "invented",
			"14. **Coverage: a 58-RPC sweep** asserting every request proto " +
				"on the wire,", "", 58},
	}
	for _, tc := range fires {
		t.Run("fires/"+tc.name, func(t *testing.T) {
			got := rpcCountMatches(tc.cur, tc.next)
			if len(got) != 1 {
				t.Fatalf("%s: matched %d carriers, want 1: %+v",
					tc.from, len(got), got)
			}
			if got[0].n != tc.want {
				t.Errorf("%s: read %d, want %d (from %q)",
					tc.from, got[0].n, tc.want, got[0].text)
			}
		})
	}
	quiet := []rpcCountSample{
		{"table row index", "doc/dnvctl.md",
			"| 59 | `spare switch --grp 1 --spare 6 --target 4 --rev 7` | " +
				"`spare_leg_id == \"6\"`, `target_leg_id == \"4\"` |", "", 0},
		// The tally line is the bold case on the quiet side: rpcCountStrip
		// drops its `**`, and what is left is a number with no noun after
		// it — unlike §7.5's `**all 59**` above, whose noun is on the next
		// line.
		{"tally line, bolded", "doc/dnvctl.md",
			"Tally: 4+6+6+9+4+5+4+4+5+4+5+3 = **59**.", "", 0},
		{"step count", "doc/dnvctl.md",
			"59 steps, trace ids `it-sweep-01`…`it-sweep-59`, one per §5 " +
				"row, in §5 order.", "", 0},
		{"quoted pin", "doc/dnvctl.md",
			"  in both directions, with `len == 59` pinned (the gatewayctl " +
				"test, ported).", "", 0},
		{"subset count", "doc/dnvctl.md",
			"   `cluster_name` appears in 58 requests and `sp_name` in 41.",
			"", 0},
		{"subset over a subset total", "ctl/root.go",
			"// clusterOf fills `cluster_name`. It is the global for 55 of " +
				"the 58 requests",
			"// that carry the field; only the `cluster` group's own three " +
				"commands", 0},
		{"ratio in a flag table", "doc/dnvctl.md",
			"| `--cluster` | string | `\"\"` | `cluster_name` of every " +
				"request that has one (58 of 59) |", "", 0},
		{"another service's surface", "doc/dnagent_integtest.md",
			"machine. All 8 `DiskNodeAgent` RPCs are exercised (§18). " +
				"Happy-path", "", 0},
		{"section reference", "doc/dnvctl.md",
			"build-out order. Division of authority: architecture.md §8 " +
				"keeps RPC semantics", "", 0},
		{"exit-code list", "cmd/dnvctl/main.go",
			"//  2. runs the command tree and exits with its code: 0 OK, " +
				"1 RPC or",
			"//     connection failure, 2 usage error (dnvctl.md §3.2).", 0},
		{"per-request limit", "doc/gateway.md",
			"  `DefaultListCnt` 64 per request, and `validatePageArgs` runs " +
				"it —", "", 0},
		{"call sites are not calls", "integtest/fakegateway/main.go",
			"// the method name at 59 call sites, is exactly the drift this " +
				"avoids.", "", 0},
		{"numbered step reference", "doc/gateway.md",
			"worker at exactly the steps a precondition demands it (§10.11 " +
				"steps 9 and",
			"15, and the live `sp0` §10.14 stands up) — and the drains " +
				"`drain-sp` and", 0},
	}
	for _, tc := range quiet {
		t.Run("quiet/"+tc.name, func(t *testing.T) {
			if got := rpcCountMatches(tc.cur, tc.next); len(got) != 0 {
				t.Errorf("%s: matched %+v, want nothing", tc.from, got)
			}
		})
	}
}
