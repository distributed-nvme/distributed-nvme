package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCloneSampleReadsTheMetadataMode pins the sample wait-hydrated takes
// from a clone's dm row: a count only from a dm-clone whose metadata is in
// read-write mode, no sample while the details hold no status line, and an
// error naming the mode for every other dm-clone (cnagent.md CN18).
func TestCloneSampleReadsTheMetadataMode(t *testing.T) {
	const lists = "0 1 no_discard_passdown 4 hydration_threshold 1 " +
		"hydration_batch_size 1"
	line := func(hydrated string, mode string) string {
		raw := "0 131072 clone 8 72/32768 2048 " + hydrated + " " + lists
		if mode != "" {
			raw += " " + mode
		}
		return raw
	}
	cases := []struct {
		name     string
		raw      string
		ok       bool
		hydrated uint64
		total    uint64
		err      string // a substring of the error; "" for none
	}{
		{"read-write", line("40/64", "rw"), true, 40, 64, ""},
		{"read-write behind a row's own text",
			"hydration not enabled\n" + line("32/64", "rw"),
			true, 32, 64, ""},
		{"no status line yet", "metadata group not ready", false, 0, 0, ""},
		{"no details at all", "", false, 0, 0, ""},
		{"read-only", line("40/64", "ro"), false, 0, 0, `"ro" mode`},
		{"fail mode", "0 131072 clone Fail", false, 0, 0, `"Fail" mode`},
		{"fail mode behind a row's own text",
			"metadata failed\n0 131072 clone Fail", false, 0, 0,
			`"Fail" mode`},
		{"no mode on the line", line("40/64", ""), false, 0, 0,
			"no metadata mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok, err := cloneSample(c.raw)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("cloneSample(%q) error %v, want one holding %q",
						c.raw, err, c.err)
				}
				if ok || status != nil {
					t.Fatalf("cloneSample(%q) = %v, %v beside its error",
						c.raw, status, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("cloneSample(%q): %v", c.raw, err)
			}
			if ok != c.ok {
				t.Fatalf("cloneSample(%q) ok %v, want %v", c.raw, ok, c.ok)
			}
			if !ok {
				return
			}
			if status.HydratedRegions != c.hydrated ||
				status.TotalRegions != c.total {
				t.Fatalf("cloneSample(%q) = %d/%d, want %d/%d", c.raw,
					status.HydratedRegions, status.TotalRegions,
					c.hydrated, c.total)
			}
		})
	}
}

// TestStrayWordIsRefused pins parseArgs, the one parse of every subcommand's
// flags: a word left after them is refused, naming it — a separate `false`
// after a bool flag, whose flags after it would go unread, and any other
// word — while the bare and the equals forms of a bool parse.
func TestStrayWordIsRefused(t *testing.T) {
	newSet := func() (*flag.FlagSet, *bool, *string) {
		fs := flag.NewFlagSet("check", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fresh := fs.Bool("fresh", false, "")
		addr := fs.String("addr", "", "")
		return fs, fresh, addr
	}
	for _, tc := range []struct {
		argv []string
		word string
	}{
		{[]string{"--fresh", "false", "--addr", "a:1"}, "false"},
		{[]string{"--addr", "a:1", "--fresh", "false"}, "false"},
		{[]string{"--addr", "a:1", "extra"}, "extra"},
	} {
		fs, _, _ := newSet()
		err := parseArgs(fs, tc.argv)
		if err == nil ||
			!strings.Contains(err.Error(), fmt.Sprintf("%q", tc.word)) {
			t.Errorf("%v: %v, want the word %q refused", tc.argv, err,
				tc.word)
		}
	}
	for _, tc := range []struct {
		argv  []string
		fresh bool
	}{
		{[]string{"--fresh=false", "--addr", "a:1"}, false},
		{[]string{"--addr", "a:1", "--fresh"}, true},
	} {
		fs, fresh, addr := newSet()
		if err := parseArgs(fs, tc.argv); err != nil ||
			*fresh != tc.fresh || *addr != "a:1" {
			t.Errorf("%v: %v, fresh %v addr %q; want it parsed with fresh "+
				"%v", tc.argv, err, *fresh, *addr, tc.fresh)
		}
	}
}

// parseCallsOutsideParseArgs reads the driver's code in dir and returns where
// a call of a method named Parse, or of flag.Parse, sits outside parseArgs, a
// top.Parse in main excepted, which this driver does not have, since each
// subcommand's flag set carries the global flags (newFlagSet), and how many
// such calls parseArgs holds. A call on another imported package's name,
// such as time.Parse, is no flag set's and is skipped, but flag.Parse, which
// parses flag.CommandLine, counts as any other; a call inside a function
// literal of a top-level table counts as outside.
func parseCallsOutsideParseArgs(dir string) ([]string, int, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, 0, err
	}
	var outside []string
	inside := 0
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return nil, 0, err
		}
		pkgs := map[string]bool{}
		// flagName is the name the file gives package flag, whose Parse
		// parses flag.CommandLine and so counts.
		flagName := ""
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			pkgName := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
			pkgs[pkgName] = true
			if path == "flag" {
				flagName = pkgName
			}
		}
		for _, decl := range f.Decls {
			fn := "a top-level declaration"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Parse" {
					return true
				}
				recv, _ := sel.X.(*ast.Ident)
				switch {
				case recv != nil && pkgs[recv.Name] && recv.Name != flagName:
				case fn == "parseArgs":
					inside++
				case fn == "main" && recv != nil && recv.Name == "top":
				default:
					outside = append(outside, fmt.Sprintf("%s in %s",
						fset.Position(call.Pos()), fn))
				}
				return true
			})
		}
	}
	return outside, inside, nil
}

// TestEveryParseGoesThroughParseArgs guards the refusal's reach: every call
// of a Parse method or of flag.Parse in the driver's code sits inside
// parseArgs, which holds exactly one; main parses no flags, each
// subcommand's flag set carrying the global ones (newFlagSet), so a
// subcommand added later that calls one cannot parse its flags past the
// refusal; a method value such as `f := fs.Parse` is not seen.
func TestEveryParseGoesThroughParseArgs(t *testing.T) {
	outside, inside, err := parseCallsOutsideParseArgs(".")
	if err != nil {
		t.Fatalf("reading the driver's code: %v", err)
	}
	for _, at := range outside {
		t.Errorf("a parse outside parseArgs: %s", at)
	}
	if inside != 1 {
		t.Errorf("parseArgs holds %d parse calls, want one", inside)
	}
}

// TestTheParseGuardSeesASkip: the guard reports a parse that skips parseArgs
// in each shape a subcommand could give it — a subcommand's own fs.Parse, a
// flag set of another name, flag.Parse of the process's flag set, and one
// inside a function literal of a table — and passes main's top.Parse and a
// call on another imported package's name.
func TestTheParseGuardSeesASkip(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import (
	"flag"
	"time"
)

func parseArgs(fs *flag.FlagSet, args []string) error { return fs.Parse(args) }

func runOne(fs *flag.FlagSet, args []string) { _ = fs.Parse(args) }

func runRace(jobFs *flag.FlagSet, argv []string) { _ = jobFs.Parse(argv) }

func runGlobal() { flag.Parse() }

var table = []func(*flag.FlagSet, []string){
	func(set *flag.FlagSet, args []string) { _ = set.Parse(args) },
}

func when() { _, _ = time.Parse(time.RFC3339, "") }

func main() {
	top := flag.NewFlagSet("x", flag.ExitOnError)
	_ = top.Parse(nil)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src),
		0o644); err != nil {
		t.Fatalf("writing the sample: %v", err)
	}
	outside, inside, err := parseCallsOutsideParseArgs(dir)
	if err != nil {
		t.Fatalf("reading the sample: %v", err)
	}
	if inside != 1 {
		t.Errorf("the sample's parseArgs holds %d parse calls, want one",
			inside)
	}
	want := []string{" in runOne", " in runRace", " in runGlobal",
		" in a top-level declaration"}
	if len(outside) != len(want) {
		t.Fatalf("the guard reports %v, want %d parses outside parseArgs",
			outside, len(want))
	}
	for i, w := range want {
		if !strings.HasSuffix(outside[i], w) {
			t.Errorf("report %d is %q, want one ending %q", i, outside[i], w)
		}
	}
}

// mainChildEnv names the environment variable that carries runMain's
// arguments to its child, joined by the unit separator.
const mainChildEnv = "DNV_DRIVER_TEST_MAIN_ARGV"

// runMain runs the driver's own main in a child process of the test binary,
// with argv as its arguments (TestMainChild), and returns its exit code and
// its stderr.
func runMain(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainChild$")
	cmd.Env = append(os.Environ(),
		mainChildEnv+"="+strings.Join(argv, "\x1f"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	}
	t.Fatalf("running main %v: %v", argv, err)
	return 0, ""
}

// TestMainChild is runMain's child: it runs main with the arguments its
// parent put in the environment, and skips in any other run.
func TestMainChild(t *testing.T) {
	argv, ok := os.LookupEnv(mainChildEnv)
	if !ok {
		t.Skip("only runMain's child runs main")
	}
	os.Args = append([]string{"cnagentctl"}, strings.Split(argv, "\x1f")...)
	main()
	os.Exit(0)
}

// TestMainRefusesAStrayWord runs the driver's main (runMain), so that the
// wrapper every subcommand's parse goes through is what refuses: a stray
// word exits 2, naming it, before any call leaves the driver.
func TestMainRefusesAStrayWord(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		word string
	}{
		{[]string{"get-cn-size", "--addr", "127.0.0.1:1", "--timeout", "1",
			"stray"}, "stray"},
	} {
		code, stderr := runMain(t, tc.argv...)
		if code != 2 ||
			!strings.Contains(stderr, fmt.Sprintf("%q", tc.word)) {
			t.Errorf("%v: exit %d, stderr %q; want exit 2 naming %q",
				tc.argv, code, stderr, tc.word)
		}
	}
}
