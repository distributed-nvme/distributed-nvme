package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The shipped config examples and units of the two agents (architecture.md,
// Components: invocation reference): one disk node agent for each disk, a
// unit template whose instance names the disk's config file and store, and
// one controller node agent.
var (
	deployDir     = filepath.Join("..", "..", "deploy")
	shippedDnCfg  = filepath.Join(deployDir, "dnv", "dnv-agent-dn-0.yaml")
	shippedCnCfg  = filepath.Join(deployDir, "dnv", "dnv-agent-cn.yaml")
	shippedDnUnit = filepath.Join(deployDir, "systemd",
		"dnv-agent-dn@.service")
	shippedCnUnit = filepath.Join(deployDir, "systemd",
		"dnv-agent-cn.service")
)

// offeredKeyRe is a key a config example's comment offers, a line "# key:
// value" of one value alone.
var offeredKeyRe = regexp.MustCompile(`(?m)^# ?([a-z][a-z0-9-]*): [^ ]+$`)

// configKeys are the keys a config example sets and the keys its comments
// offer.
func configKeys(t *testing.T, path string) []string {
	t.Helper()
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := v.AllKeys()
	for _, m := range offeredKeyRe.FindAllStringSubmatch(string(data), -1) {
		keys = append(keys, m[1])
	}
	return keys
}

// unitText is a unit's text with its continuation lines joined.
func unitText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "\\\n", " ")
}

// unitSetting is the value of one setting of a unit, "" when it has none.
func unitSetting(text, name string) string {
	m := regexp.MustCompile(`(?m)^` + name + `=(.*)$`).FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// Every key of each agent's shipped config example is a flag of its role,
// but --version, which no config key sets, --config, and --local-store,
// which the unit sets, since a flag wins over the file; and each example
// alone gives its role every value it requires and passes its role's checks
// of the values (dnagent.md CM3).
func TestShippedConfigsMatchFlags(t *testing.T) {
	for _, tc := range []struct {
		path, role string
		check      func() error
	}{
		{shippedDnCfg, "dn", func() error {
			return requireValues(append(requiredCommon, "disk")...)
		}},
		{shippedCnCfg, "cn", func() error {
			if err := requireValues(requiredCommon...); err != nil {
				return err
			}
			_, err := capacityFromViper()
			return err
		}},
	} {
		name := filepath.Base(tc.path)
		flags := subCmd(t, tc.role).Flags()
		for _, key := range configKeys(t, tc.path) {
			switch {
			case key == "config" || key == "local-store":
				t.Errorf("%s: %q is the unit's to set", name, key)
			case flags.Lookup(key) == nil:
				t.Errorf("%s: %q is no flag of dnv-agent %s", name, key,
					tc.role)
			}
		}
		parse(t, tc.role, []string{"--config", tc.path})
		if err := tc.check(); err != nil {
			t.Errorf("%s alone: %v", name, err)
		}
		if _, err := nvmetPortIdFromViper(); err != nil {
			t.Errorf("%s alone: %v", name, err)
		}
		if _, err := nvmetQidMaxFromViper(); err != nil {
			t.Errorf("%s alone: %v", name, err)
		}
		if _, err := common.ParseLogLevel(viper.GetString("log-level")); err != nil {
			t.Errorf("%s alone: %v", name, err)
		}
	}
}

// Each agent's shipped unit starts its role with its config file under
// /etc/dnv and its own local store, the folder the unit makes as its
// StateDirectory=: the controller node agent's store is the default, and a
// disk node agent's is one folder for each instance of the template, outside
// the default store, whose listing takes any name that starts as a store
// kind does for a state file, a folder's too (dnagent.md SH6).
func TestShippedUnitsKeepStoresApart(t *testing.T) {
	for _, tc := range []struct {
		unit, role, config string
	}{
		{shippedDnUnit, "dn", "/etc/dnv/dnv-agent-dn-%i.yaml"},
		{shippedCnUnit, "cn", "/etc/dnv/dnv-agent-cn.yaml"},
	} {
		name := filepath.Base(tc.unit)
		text := unitText(t, tc.unit)
		exec := strings.Fields(unitSetting(text, "ExecStart"))
		want := []string{"/usr/local/bin/dnv-agent", tc.role, "--config",
			tc.config, "--local-store"}
		if len(exec) != len(want)+1 || !reflect.DeepEqual(exec[:len(want)], want) {
			t.Errorf("%s starts %q, want %q and a store", name, exec, want)
			continue
		}
		store := exec[len(want)]
		if made := "/var/lib/" + unitSetting(text, "StateDirectory"); made != store {
			t.Errorf("%s makes %s, not its store %s", name, made, store)
		}
		switch tc.role {
		case "cn":
			if store != common.DefaultLocalStorPrefix {
				t.Errorf("%s keeps its store in %s, not in the default %s",
					name, store, common.DefaultLocalStorPrefix)
			}
		case "dn":
			one := strings.ReplaceAll(store, "%i", "0")
			if one == store || one == common.DefaultLocalStorPrefix ||
				strings.HasPrefix(one, common.DefaultLocalStorPrefix+"/") {
				t.Errorf("%s keeps an instance's store in %s, not in a "+
					"folder of its own outside the default store %s", name,
					store, common.DefaultLocalStorPrefix)
			}
		}
		cfg := strings.ReplaceAll(tc.config, "%i", "0")
		if shipped := filepath.Join(deployDir, "dnv", filepath.Base(cfg)); !fileExists(shipped) {
			t.Errorf("%s reads %s, which has no example %s", name, cfg,
				shipped)
		}
	}
}

// Each agent's shipped unit sends the stop signal to the agent's main
// process alone, and what it does with the rest of the unit once that
// process has exited follows what the role joins (dnagent.md SH27): the dn's
// stop waits for its background tasks that run children, a fence timer's
// callback aside, so its unit kills what is left (KillMode=mixed); the cn
// joins none of its background tasks, and its sweep's background
// "nvme disconnect" may outlive it, so its unit kills nothing else
// (KillMode=process), lest a stop wait for that disconnect inside the
// kernel and hold a restart of a node that holds a primary.
func TestShippedAgentUnitsKillAsTheRoleJoins(t *testing.T) {
	for _, tc := range []struct{ unit, want string }{
		{shippedDnUnit, "mixed"},
		{shippedCnUnit, "process"},
	} {
		got := unitSetting(unitText(t, tc.unit), "KillMode")
		if got != tc.want {
			t.Errorf("%s: KillMode=%s, want KillMode=%s",
				filepath.Base(tc.unit), got, tc.want)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
