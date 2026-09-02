package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The invocation from architecture.md §13.
var exampleDnArgs = []string{
	"--grpc-network", "tcp",
	"--grpc-address", "192.168.0.20:29528",
	"--tr-type", "tcp",
	"--adr-fam", "ipv4",
	"--tr-addr", "192.168.0.20",
	"--tr-svc-id", "4200",
	"--local-store", "/var/tmp",
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

// CM1: exactly two subcommands, and --disk belongs only to dn.
func TestSubcommands(t *testing.T) {
	root := newRootCmd()
	if root.Use != "dnv-agent" {
		t.Errorf("root command = %q", root.Use)
	}
	var names []string
	for _, cmd := range root.Commands() {
		names = append(names, cmd.Use)
	}
	if len(names) != 2 || !contains(names, "dn") || !contains(names, "cn") {
		t.Fatalf("subcommands = %v, want exactly dn and cn", names)
	}
	if subCmd(t, "dn").Flags().Lookup("disk") == nil {
		t.Error("dn has no --disk flag")
	}
	if subCmd(t, "cn").Flags().Lookup("disk") != nil {
		t.Error("cn has a --disk flag")
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

// CM2: the §13 example parses and every value arrives through viper.
func TestExampleInvocationParses(t *testing.T) {
	parse(t, "dn", exampleDnArgs)
	if err := requireValues(append(requiredCommon, "disk")...); err != nil {
		t.Fatalf("the §13 example was rejected: %v", err)
	}
	for key, want := range map[string]string{
		"grpc-network": "tcp",
		"grpc-address": "192.168.0.20:29528",
		"tr-type":      "tcp",
		"adr-fam":      "ipv4",
		"tr-addr":      "192.168.0.20",
		"tr-svc-id":    "4200",
		"local-store":  "/var/tmp",
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
	if err := requireValues(requiredCommon...); err != nil {
		t.Errorf("cn rejected a complete invocation: %v", err)
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
