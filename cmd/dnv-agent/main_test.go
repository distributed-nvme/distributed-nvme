package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/agent/nodecheck"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// An example dn invocation, with the flags of dnagent.md CM2.
var exampleDnArgs = []string{
	"--grpc-network", "tcp",
	"--grpc-address", "192.168.0.20:29528",
	"--tr-type", "tcp",
	"--adr-fam", "ipv4",
	"--tr-addr", "192.168.0.20",
	"--tr-svc-id", "4200",
	"--local-store", "/var/lib/dnv",
	"--disk", "/dev/disk/by-uuid/4425c6a8-dc27-40a3-9fd5-0cc41f534360",
}

func subCmd(t *testing.T, use string) *cobra.Command {
	t.Helper()
	for _, cmd := range newRootCmd().Commands() {
		if cmd.Use == use {
			return cmd
		}
	}
	t.Fatalf("no %q subcommand", use)
	return nil
}

// parse binds one subcommand's flags into a fresh viper.
func parse(t *testing.T, use string, args []string) *cobra.Command {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := subCmd(t, use)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if err := bindViper(cmd); err != nil {
		t.Fatalf("bindViper: %v", err)
	}
	return cmd
}

// CM1: the command tree. dn and cn serve the agents; check dn and check cn
// are the node check with their roles' flags; collect is the gather.
// --disk belongs to the dn role's two commands alone, --capacity to the cn
// role's, --md-replay to check cn, and --log-level to the roles and their
// checks, not to the gather.
func TestSubcommands(t *testing.T) {
	root := newRootCmd()
	if root.Use != "dnv-agent" {
		t.Errorf("root command = %q", root.Use)
	}
	tree := map[string][]string{}
	var walk func(prefix string, cmd *cobra.Command)
	walk = func(prefix string, cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			tree[prefix] = append(tree[prefix], sub.Use)
			walk(strings.TrimSpace(prefix+" "+sub.Use), sub)
		}
	}
	walk("", root)
	want := map[string][]string{
		"":      {"check", "cn", "collect", "dn"},
		"check": {"cn", "dn"},
	}
	for prefix, names := range want {
		got := append([]string{}, tree[prefix]...)
		sort.Strings(got)
		if strings.Join(got, " ") != strings.Join(names, " ") {
			t.Errorf("subcommands of %q = %v, want %v", prefix, got, names)
		}
	}
	if len(tree) != len(want) {
		t.Errorf("command tree = %v, want %v", tree, want)
	}
	flags := map[string]map[string]bool{
		"dn": {"disk": true, "capacity": false, "md-replay": false,
			"log-level": true},
		"cn": {"disk": false, "capacity": true, "md-replay": false,
			"log-level": true},
		"check dn": {"disk": true, "capacity": false, "md-replay": false,
			"log-level": true},
		"check cn": {"disk": false, "capacity": true, "md-replay": true,
			"log-level": true},
		"collect": {"disk": false, "log-file": true, "journal-unit": true,
			"log-level": false},
	}
	for path, want := range flags {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatalf("find %q: %v", path, err)
		}
		for flag, has := range want {
			if (cmd.Flags().Lookup(flag) != nil) != has {
				t.Errorf("%s: --%s present = %v, want %v", path, flag,
					!has, has)
			}
		}
	}
	// The two check commands take every flag of their role, so an agent's
	// own --config serves its check.
	for _, role := range []string{"dn", "cn"} {
		agentCmd, _, _ := root.Find([]string{role})
		checkCmd, _, _ := root.Find([]string{"check", role})
		agentCmd.Flags().VisitAll(func(f *pflag.Flag) {
			if checkCmd.Flags().Lookup(f.Name) == nil {
				t.Errorf("check %s lacks --%s", role, f.Name)
			}
		})
	}
}

// nodeTestEnv is a node whose every read answers empty, as root, for the
// command tree's tests: no directory is there, so the gather's --out is no
// directory; the gather's archive is moved from its private directory to
// its path.
func nodeTestEnv(t *testing.T, written map[string]string) nodecheck.Env {
	t.Helper()
	return nodecheck.Env{
		Oc: &common.FakeOsClient{
			ReadDirFn: func(_ context.Context, path string) ([]string,
				error) {
				return nil, &fs.PathError{Op: "open", Path: path,
					Err: fs.ErrNotExist}
			},
			RunCommandFn: func(_ context.Context, name string,
				args []string, _ string) (string, string, int, error) {
				switch name {
				case "mktemp":
					return strings.Replace(args[len(args)-1], "XXXXXX",
						"x", 1), "", 0, nil
				case "mv":
					from, to := args[len(args)-2], args[len(args)-1]
					written[to] = written[from]
					delete(written, from)
				}
				return "", "", 0, nil
			},
			WriteFileFn: func(_ context.Context, path, data string) error {
				written[path] = data
				return nil
			},
			StartDetachedFn: func(context.Context, string, []string,
				string) (*common.Detached, error) {
				child, end := common.NewDetached(1, "/tmp/out")
				end(0, nil)
				return child, nil
			},
		},
		Euid: func() int { return 0 },
		Now:  time.Now,
	}
}

// executeRoot runs the command tree with args, as main does, its output to
// out.
func executeRoot(args []string, out *strings.Builder) error {
	viper.Reset()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(out)
	return root.Execute()
}

// Neither the node check nor the gather starts a server: each runs to its
// end, never calls the agent's serve, and leaves the gRPC address it was
// given unused.
func TestCheckAndCollectStartNoServer(t *testing.T) {
	served := false
	oldServe, oldEnv := serveAgent, nodeEnv
	t.Cleanup(func() {
		serveAgent, nodeEnv = oldServe, oldEnv
		viper.Reset()
	})
	serveAgent = func(context.Context, string, string,
		func(context.Context), func(context.Context) error,
		func(*grpc.Server), func()) error {
		served = true
		return nil
	}
	written := map[string]string{}
	nodeEnv = func() nodecheck.Env { return nodeTestEnv(t, written) }
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	out := filepath.Join(t.TempDir(), "c.tar.gz")
	for _, args := range [][]string{
		{"check", "dn", "--grpc-address", addr, "--disk", "/dev/loop0"},
		{"check", "cn", "--grpc-address", addr, "--output", "json"},
		{"collect", "--out", out, "--deadline", "100ms"},
	} {
		var printed strings.Builder
		err := executeRoot(args, &printed)
		var exit *nodecheck.ExitError
		if err != nil && (!errors.As(err, &exit) ||
			exit.Code == nodecheck.ExitNotRun) {
			t.Errorf("%v: %v\n%s", args, err, printed.String())
		}
		if printed.Len() == 0 {
			t.Errorf("%v printed nothing", args)
		}
		if served {
			t.Fatalf("%v started a server", args)
		}
		again, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("%v holds %s: %v", args, addr, err)
		} else {
			again.Close()
		}
	}
	if written[out] == "" {
		t.Errorf("collect wrote no archive: %v", written)
	}
}

// The node check's exit statuses reach the command line: a flag it cannot
// parse, and a run not as root, end it with ExitNotRun.
func TestCheckCannotRun(t *testing.T) {
	oldEnv := nodeEnv
	t.Cleanup(func() { nodeEnv = oldEnv; viper.Reset() })
	nodeEnv = func() nodecheck.Env {
		env := nodeTestEnv(t, map[string]string{})
		env.Euid = func() int { return 1000 }
		return env
	}
	for _, args := range [][]string{
		{"check", "cn"},
		{"check", "dn", "--no-such-flag"},
		{"collect"},
		{"collect", "--deadline", "soon"},
	} {
		var printed strings.Builder
		err := executeRoot(args, &printed)
		var exit *nodecheck.ExitError
		if !errors.As(err, &exit) || exit.Code != nodecheck.ExitNotRun {
			t.Errorf("%v: %v, want exit %d", args, err, nodecheck.ExitNotRun)
		}
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// CM2: the example dn invocation parses and every value arrives through viper.
func TestExampleInvocationParses(t *testing.T) {
	parse(t, "dn", exampleDnArgs)
	if err := requireValues(append(requiredCommon, "disk")...); err != nil {
		t.Fatalf("the example dn invocation was rejected: %v", err)
	}
	for key, want := range map[string]string{
		"grpc-network": "tcp",
		"grpc-address": "192.168.0.20:29528",
		"tr-type":      "tcp",
		"adr-fam":      "ipv4",
		"tr-addr":      "192.168.0.20",
		"tr-svc-id":    "4200",
		"local-store":  "/var/lib/dnv",
		"disk": "/dev/disk/by-uuid/" +
			"4425c6a8-dc27-40a3-9fd5-0cc41f534360",
	} {
		if got := viper.GetString(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	trConf := trConfFromViper()
	if trConf.GetTrType() != "tcp" || trConf.GetTrSvcId() != "4200" {
		t.Errorf("tr conf = %v", trConf)
	}
}

func TestDefaults(t *testing.T) {
	parse(t, "cn", []string{
		"--grpc-address", "192.168.0.20:29529",
		"--tr-type", "tcp", "--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.20", "--tr-svc-id", "4200",
	})
	if got := viper.GetString("grpc-network"); got != "tcp" {
		t.Errorf("grpc-network default = %q, want tcp", got)
	}
	if got := viper.GetString("local-store"); got !=
		common.DefaultLocalStorPrefix {
		t.Errorf("local-store default = %q, want %q",
			got, common.DefaultLocalStorPrefix)
	}
	if got := viper.GetInt("nvmet-port-id"); got != common.NvmetPortId {
		t.Errorf("nvmet-port-id default = %d, want common.NvmetPortId (%d)",
			got, common.NvmetPortId)
	}
	if got, err := nvmetPortIdFromViper(); err != nil ||
		got != common.NvmetPortId {
		t.Errorf("nvmetPortIdFromViper() = %d, %v; want %d, nil",
			got, err, common.NvmetPortId)
	}
	if err := requireValues(requiredCommon...); err != nil {
		t.Errorf("cn rejected a complete invocation: %v", err)
	}
}

// CM2: --nvmet-port-id belongs to both roles (a dn VM runs many agents, each
// converging its own configfs port), it defaults to common.NvmetPortId, and
// anything below 1 is refused — configfs has no ports/0.
func TestNvmetPortIdIsOnBothRolesAndBounded(t *testing.T) {
	for _, use := range []string{"dn", "cn"} {
		if subCmd(t, use).Flags().Lookup("nvmet-port-id") == nil {
			t.Fatalf("%s has no --nvmet-port-id flag", use)
		}
	}
	parse(t, "dn", append([]string{
		"--nvmet-port-id", "7",
	}, exampleDnArgs...))
	if got, err := nvmetPortIdFromViper(); err != nil || got != 7 {
		t.Errorf("nvmetPortIdFromViper() = %d, %v; want 7, nil", got, err)
	}

	for _, bad := range []string{"0", "-1"} {
		parse(t, "cn", append([]string{
			"--nvmet-port-id", bad,
		}, exampleCnArgs...))
		got, err := nvmetPortIdFromViper()
		if err == nil {
			t.Fatalf("--nvmet-port-id %s was accepted as %d", bad, got)
		}
		if !strings.Contains(err.Error(), "--nvmet-port-id") {
			t.Errorf("error %q does not name --nvmet-port-id", err)
		}
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("error %q does not name the rejected value %s", err, bad)
		}
	}
}

// CM2: --nvmet-qid-max belongs to both roles, defaults to common.NvmetQidMax,
// takes 0 (write none) up to nvmet's own bound, and refuses anything else,
// a value that is not a number included: viper would read that as 0, which
// would turn the cap off without a word (CM3).
func TestNvmetQidMaxIsOnBothRolesAndBounded(t *testing.T) {
	for _, use := range []string{"dn", "cn"} {
		if subCmd(t, use).Flags().Lookup("nvmet-qid-max") == nil {
			t.Fatalf("%s has no --nvmet-qid-max flag", use)
		}
	}
	parse(t, "cn", exampleCnArgs)
	if got, err := nvmetQidMaxFromViper(); err != nil ||
		got != common.NvmetQidMax {
		t.Errorf("nvmetQidMaxFromViper() = %d, %v; want the default %d, nil",
			got, err, common.NvmetQidMax)
	}
	for _, good := range []int{0, 1, common.NvmetQidMaxLimit} {
		parse(t, "dn", append([]string{
			"--nvmet-qid-max", strconv.Itoa(good),
		}, exampleDnArgs...))
		if got, err := nvmetQidMaxFromViper(); err != nil || got != good {
			t.Errorf("nvmetQidMaxFromViper() = %d, %v; want %d, nil",
				got, err, good)
		}
	}
	for _, bad := range []string{"-1",
		strconv.Itoa(common.NvmetQidMaxLimit + 1)} {
		parse(t, "cn", append([]string{
			"--nvmet-qid-max", bad,
		}, exampleCnArgs...))
		got, err := nvmetQidMaxFromViper()
		if err == nil {
			t.Fatalf("--nvmet-qid-max %s was accepted as %d", bad, got)
		}
		if !strings.Contains(err.Error(), "--nvmet-qid-max") ||
			!strings.Contains(err.Error(), bad) {
			t.Errorf("error %q does not name the flag and the value %s",
				err, bad)
		}
	}

	t.Setenv("DNV_AGENT_NVMET_QID_MAX", "2")
	parse(t, "dn", exampleDnArgs)
	if got, err := nvmetQidMaxFromViper(); err != nil || got != 2 {
		t.Errorf("nvmetQidMaxFromViper() = %d, %v; want the environment's "+
			"2, nil", got, err)
	}
	t.Setenv("DNV_AGENT_NVMET_QID_MAX", "eight")
	parse(t, "dn", exampleDnArgs)
	if got, err := nvmetQidMaxFromViper(); err == nil {
		t.Errorf("a non-numeric env value was accepted as %d", got)
	}
}

// CM3: the environment reaches --nvmet-port-id like every other flag, which
// is how the e2e suite gives each of a VM's agents its own port.
func TestNvmetPortIdFromEnv(t *testing.T) {
	t.Setenv("DNV_AGENT_NVMET_PORT_ID", "9")
	parse(t, "dn", exampleDnArgs)
	if got := viper.GetInt("nvmet-port-id"); got != 9 {
		t.Errorf("nvmet-port-id = %d, want the environment value 9", got)
	}
	if got, err := nvmetPortIdFromViper(); err != nil || got != 9 {
		t.Errorf("nvmetPortIdFromViper() = %d, %v; want 9, nil", got, err)
	}

	// An explicit flag still wins over the environment.
	parse(t, "dn", append([]string{
		"--nvmet-port-id", "3",
	}, exampleDnArgs...))
	if got, err := nvmetPortIdFromViper(); err != nil || got != 3 {
		t.Errorf("nvmetPortIdFromViper() = %d, %v; want the flag's 3, nil",
			got, err)
	}

	// A value the environment cannot parse as an int reads back as 0 and is
	// refused rather than silently converging ports/0.
	t.Setenv("DNV_AGENT_NVMET_PORT_ID", "not-a-number")
	parse(t, "dn", exampleDnArgs)
	if got, err := nvmetPortIdFromViper(); err == nil {
		t.Errorf("a non-numeric env value was accepted as %d", got)
	}
}

// CM2: --disk is required for dn.
func TestDiskRequiredForDn(t *testing.T) {
	parse(t, "dn", exampleDnArgs[:len(exampleDnArgs)-2])
	err := requireValues(append(requiredCommon, "disk")...)
	if err == nil {
		t.Fatal("a dn invocation without --disk was accepted")
	}
	if !strings.Contains(err.Error(), "--disk") {
		t.Errorf("error %q does not name --disk", err)
	}
}

func TestRequiredValuesReported(t *testing.T) {
	parse(t, "dn", []string{"--disk", "/dev/sdb"})
	err := requireValues(append(requiredCommon, "disk")...)
	if err == nil {
		t.Fatal("an invocation missing every endpoint flag was accepted")
	}
	for _, name := range requiredCommon {
		if !strings.Contains(err.Error(), "--"+name) {
			t.Errorf("error %q does not name --%s", err, name)
		}
	}
}

// CM3: environment beats the flag default.
func TestEnvOverridesFlagDefault(t *testing.T) {
	t.Setenv("DNV_AGENT_GRPC_ADDRESS", "10.0.0.9:29528")
	t.Setenv("DNV_AGENT_GRPC_NETWORK", "unix")
	parse(t, "dn", []string{
		"--tr-type", "tcp", "--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.20", "--tr-svc-id", "4200",
		"--disk", "/dev/sdb",
	})
	if got := viper.GetString("grpc-address"); got != "10.0.0.9:29528" {
		t.Errorf("grpc-address = %q, want the environment value", got)
	}
	if got := viper.GetString("grpc-network"); got != "unix" {
		t.Errorf("grpc-network = %q, want the environment value", got)
	}
	if err := requireValues(append(requiredCommon, "disk")...); err != nil {
		t.Errorf("an env-supplied required value was rejected: %v", err)
	}

	// An explicit flag still wins over the environment.
	parse(t, "dn", append([]string{
		"--grpc-address", "127.0.0.1:1",
	}, exampleDnArgs[2:]...))
	if got := viper.GetString("grpc-address"); got != "192.168.0.20:29528" {
		t.Errorf("grpc-address = %q, want the explicit flag value", got)
	}
}

// CM3: --config is read, and file values fill in what flags leave unset.
func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/agent.yaml"
	if err := writeFileForTest(path,
		"grpc-address: 10.1.2.3:29528\ntr-svc-id: \"4420\"\n"); err != nil {
		t.Fatalf("write config: %v", err)
	}
	parse(t, "dn", []string{
		"--config", path,
		"--tr-type", "tcp", "--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.20",
		"--disk", "/dev/sdb",
	})
	if got := viper.GetString("grpc-address"); got != "10.1.2.3:29528" {
		t.Errorf("grpc-address = %q, want the config value", got)
	}
	if got := viper.GetString("tr-svc-id"); got != "4420" {
		t.Errorf("tr-svc-id = %q, want the config value", got)
	}
	if err := requireValues(append(requiredCommon, "disk")...); err != nil {
		t.Errorf("config-supplied values were rejected: %v", err)
	}
}

func writeFileForTest(path string, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}

// ---------------------------------------------------------------------------
// The cn role (cnagent.md CN-CM1/CN-CM2, dnagent.md CM3)
// ---------------------------------------------------------------------------

// An example cn invocation: the flags of dnagent.md CM2, plus the cn-only
// --capacity of CN-CM1.
var exampleCnArgs = []string{
	"--grpc-network", "tcp",
	"--grpc-address", "192.168.0.20:29529",
	"--tr-type", "tcp",
	"--adr-fam", "ipv4",
	"--tr-addr", "192.168.0.20",
	"--tr-svc-id", "4200",
	"--local-store", "/var/lib/dnv",
	"--capacity", "1099511627776",
}

func TestCnExampleInvocationParses(t *testing.T) {
	parse(t, "cn", exampleCnArgs)
	if err := requireValues(requiredCommon...); err != nil {
		t.Fatalf("the example cn invocation was rejected: %v", err)
	}
	if got := viper.GetString("grpc-address"); got !=
		"192.168.0.20:29529" {
		t.Errorf("grpc-address = %q", got)
	}
	if got := viper.GetUint64("capacity"); got != 1099511627776 {
		t.Errorf("capacity = %d, want 1099511627776", got)
	}
}

// CN-CM1: --capacity is cn-only and optional; 0 means "use the CP default".
func TestCapacityIsCnOnlyAndOptional(t *testing.T) {
	if subCmd(t, "dn").Flags().Lookup("capacity") != nil {
		t.Error("dn has a --capacity flag")
	}
	if subCmd(t, "cn").Flags().Lookup("capacity") == nil {
		t.Fatal("cn has no --capacity flag")
	}
	parse(t, "cn", []string{
		"--grpc-address", "192.168.0.20:29529",
		"--tr-type", "tcp", "--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.20", "--tr-svc-id", "4200",
	})
	if err := requireValues(requiredCommon...); err != nil {
		t.Fatalf("cn without --capacity was rejected: %v", err)
	}
	if got := viper.GetUint64("capacity"); got != 0 {
		t.Errorf("capacity default = %d, want 0", got)
	}
}

// CM3: the environment reaches --capacity like every other flag.
func TestCapacityFromEnv(t *testing.T) {
	t.Setenv("DNV_AGENT_CAPACITY", "4294967296")
	parse(t, "cn", []string{
		"--grpc-address", "192.168.0.20:29529",
		"--tr-type", "tcp", "--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.20", "--tr-svc-id", "4200",
	})
	if got := viper.GetUint64("capacity"); got != 4294967296 {
		t.Errorf("capacity = %d, want the environment value", got)
	}
}

// CM3: one rule for --capacity from the flag, the environment and a config
// file's string: a plain unsigned decimal number that fits 64 bits, with no
// leading zero but in "0" itself, is taken, and anything else is refused,
// naming the flag and the value as given, where viper.GetUint64 would read
// it as 0 and pflag's uint64 parse would take Go integer syntax from the
// flag alone. The default is 0. A config file's YAML number is taken by the
// value YAML decodes.
func TestCapacityRefusesWhatIsNotANumber(t *testing.T) {
	cnArgs := []string{
		"--grpc-address", "192.168.0.20:29529",
		"--tr-type", "tcp", "--adr-fam", "ipv4",
		"--tr-addr", "192.168.0.20", "--tr-svc-id", "4200",
	}
	parse(t, "cn", cnArgs)
	if got, err := capacityFromViper(); err != nil || got != 0 {
		t.Errorf("capacityFromViper() = %d, %v; want the default 0, nil",
			got, err)
	}
	conf := t.TempDir() + "/agent.yaml"
	// sources each parse cn's flags with the value v given by one source.
	sources := map[string]func(t *testing.T, v string){
		"the flag": func(t *testing.T, v string) {
			parse(t, "cn", append([]string{"--capacity", v}, cnArgs...))
		},
		"the environment": func(t *testing.T, v string) {
			t.Setenv("DNV_AGENT_CAPACITY", v)
			parse(t, "cn", cnArgs)
		},
		"a config file's string": func(t *testing.T, v string) {
			if err := os.WriteFile(conf, []byte("capacity: \""+v+"\"\n"),
				0o600); err != nil {
				t.Fatalf("writing the config file: %v", err)
			}
			parse(t, "cn", append([]string{"--config", conf}, cnArgs...))
		},
	}
	for name, from := range sources {
		t.Run(name, func(t *testing.T) {
			for raw, want := range map[string]uint64{
				"4294967296": 4294967296, "0": 0,
			} {
				from(t, raw)
				if got, err := capacityFromViper(); err != nil ||
					got != want {
					t.Errorf("capacityFromViper() of %q = %d, %v; want "+
						"%d, nil", raw, got, err, want)
				}
			}
			for _, bad := range []string{"500GiB", "1e12", "-1", "1.5",
				"0x100", "1_000", "+5", "0100", "00",
				"18446744073709551616"} {
				from(t, bad)
				got, err := capacityFromViper()
				if err == nil {
					t.Fatalf("a capacity of %q was taken as %d", bad, got)
				}
				if !strings.Contains(err.Error(), "--capacity") ||
					!strings.Contains(err.Error(), bad) {
					t.Errorf("error %q does not name the flag and the "+
						"value %s", err, bad)
				}
			}
		})
	}
	t.Run("a config file's number", func(t *testing.T) {
		for raw, want := range map[string]uint64{
			"1e12": 1000000000000, "0x100": 256, "0100": 64,
			"4294967296": 4294967296,
		} {
			if err := os.WriteFile(conf, []byte("capacity: "+raw+"\n"),
				0o600); err != nil {
				t.Fatalf("writing the config file: %v", err)
			}
			parse(t, "cn", append([]string{"--config", conf}, cnArgs...))
			if got, err := capacityFromViper(); err != nil || got != want {
				t.Errorf("capacity: %s = %d, %v; want %d, nil", raw, got,
					err, want)
			}
		}
		if err := os.WriteFile(conf, []byte("capacity: 1.5\n"),
			0o600); err != nil {
			t.Fatalf("writing the config file: %v", err)
		}
		parse(t, "cn", append([]string{"--config", conf}, cnArgs...))
		if got, err := capacityFromViper(); err == nil {
			t.Fatalf("a config file's capacity 1.5 was taken as %d", got)
		}
	})
}

// CM4: the first signal cancels the context, which starts the bounded stop,
// and a second one exits with status 1.
func TestWatchSignalsSecondSignalExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSignals(ctx, cancel, sigCh, func(code int) { exited <- code })
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the first signal did not cancel ctx")
	}

	sigCh <- syscall.SIGINT
	select {
	case code := <-exited:
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second signal did not exit")
	}
	<-done
}

// CM4: a stop with no signal leaves no watcher behind and never exits.
func TestWatchSignalsReturnsWhenCtxEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSignals(ctx, cancel, sigCh, func(int) {
			t.Error("exit was called without any signal")
		})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchSignals did not return when ctx ended")
	}
}

// Each role logs its start record before it serves: the kernel release,
// and for the cn the mdadm version too. A read that fails is in the record,
// and the start goes on to serve.
func TestRolesLogTheirStartRecord(t *testing.T) {
	oldServe := serveAgent
	t.Cleanup(func() {
		serveAgent = oldServe
		viper.Reset()
		common.SetLogLevel(slog.LevelInfo)
	})
	common.SetLogLevel(slog.LevelInfo)
	var buf bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	flags := []string{"--grpc-address", "127.0.0.1:1", "--tr-type", "tcp",
		"--adr-fam", "ipv4", "--tr-addr", "127.0.0.1", "--tr-svc-id", "4420",
		"--local-store", t.TempDir()}
	for _, role := range []string{"dn", "cn"} {
		buf.Reset()
		var recordBeforeServe bool
		serveAgent = func(context.Context, string, string,
			func(context.Context), func(context.Context) error,
			func(*grpc.Server), func()) error {
			recordBeforeServe = strings.Contains(buf.String(),
				`"msg":"agent node versions"`)
			return nil
		}
		args := append([]string{role}, flags...)
		if role == "dn" {
			args = append(args, "--disk", "/dev/null")
		}
		var printed strings.Builder
		if err := executeRoot(args, &printed); err != nil {
			t.Fatalf("%s: %v\n%s", role, err, printed.String())
		}
		if !recordBeforeServe {
			t.Errorf("%s: no start record before serving:\n%s", role,
				buf.String())
		}
		hasMdadm := strings.Contains(buf.String(), `"mdadm_version"`)
		if hasMdadm != (role == "cn") {
			t.Errorf("%s: mdadm_version in the record = %v:\n%s", role,
				hasMdadm, buf.String())
		}
	}
}

// The node check's and the gather's first signal, a hang-up or a quit
// among them, cancels their context instead of ending the process, and a
// later one is ignored, so the md replay's teardown runs to its end (CM5).
func TestToolSignalsStopWithoutEnding(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGTERM} {
		ctx, stop := toolSignalContext()
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			stop()
			t.Fatalf("%v did not cancel the tool's context", sig)
		}
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		stop()
	}
}
