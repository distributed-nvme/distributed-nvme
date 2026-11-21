// Tests for the parsing surface of dnagentctl (dnagent_integtest.md, The
// driver: `dnagentctl`): its subcommands refuse a word left after their flags.
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
// a call of a method named Parse sits outside parseArgs, but main's parse of
// the global flags (top.Parse), and how many such calls parseArgs holds. A
// call on an imported package's name, such as time.Parse, is no flag set's
// and is skipped; a call inside a function literal of a top-level table
// counts as outside.
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
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			pkgName := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
			pkgs[pkgName] = true
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
				case recv != nil && pkgs[recv.Name]:
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
// of a Parse method in the driver's code sits inside parseArgs, which holds
// exactly one, but main's parse of the global flags, so a subcommand added
// later cannot parse its flags past the refusal.
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
// flag set of another name, and one inside a function literal of a table —
// and passes main's top.Parse and a call on an imported package's name.
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
	want := []string{" in runOne", " in runRace",
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
	os.Args = append([]string{"dnagentctl"}, strings.Split(argv, "\x1f")...)
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
		{[]string{"get-dn-size", "--addr", "127.0.0.1:1", "--timeout", "1",
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
