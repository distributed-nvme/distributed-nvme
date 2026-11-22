package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The shipped config example and unit of the worker (architecture.md,
// Components: invocation reference).
var (
	shippedConfig = filepath.Join("..", "..", "deploy", "dnv", "dnv-worker.yaml")
	shippedUnit   = filepath.Join("..", "..", "deploy", "systemd",
		"dnv-worker.service")
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

// unitExec is a unit's ExecStart= split into words, its continuation lines
// joined.
func unitExec(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\\\n", " ")
	m := regexp.MustCompile(`(?m)^ExecStart=(.*)$`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("%s has no ExecStart=", path)
	}
	return strings.Fields(m[1])
}

// Every key of the shipped config example is a flag of the worker, but
// --version, which no config key sets, and --config; the example alone
// gives the worker every value it requires; and the shipped unit starts
// the worker with that file at its place under /etc/dnv.
func TestShippedConfigMatchesFlags(t *testing.T) {
	flags := newRootCmd().Flags()
	for _, key := range configKeys(t, shippedConfig) {
		if key == "version" || key == "config" || flags.Lookup(key) == nil {
			t.Errorf("dnv-worker.yaml: %q is no flag a config file sets",
				key)
		}
	}
	parse(t, []string{"--config", shippedConfig})
	if _, err := optionsFromViper(); err != nil {
		t.Errorf("dnv-worker.yaml alone: %v", err)
	}
	want := []string{"/usr/local/bin/dnv-worker", "--config",
		"/etc/dnv/dnv-worker.yaml"}
	if got := unitExec(t, shippedUnit); !reflect.DeepEqual(got, want) {
		t.Errorf("dnv-worker.service starts %q, want %q", got, want)
	}
}

// The shipped unit waits longer on a stop than the worker's longest drain
// (dnv-worker.md CM5), since systemd kills a unit whose stop outlasts its
// TimeoutStopSec=: the heartbeat loop's last put, then one delete per role
// of the default --roles, each bounded by common.DefaultEtcdOpTimeout, then
// the join of the shard workers, bounded by common.DefaultWorkerSyncupTimeout
// plus common.DefaultWorkerPushTimeout.
func TestShippedUnitOutwaitsTheDrain(t *testing.T) {
	data, err := os.ReadFile(shippedUnit)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^TimeoutStopSec=([0-9]+)s?$`).
		FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("dnv-worker.service sets no TimeoutStopSec= in seconds")
	}
	stop, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	drain := (1+len(workerRoles))*common.DefaultEtcdOpTimeout +
		common.DefaultWorkerSyncupTimeout + common.DefaultWorkerPushTimeout
	if stop <= drain {
		t.Errorf("dnv-worker.service waits %d s on a stop, not more than "+
			"the worker's longest drain of %d s", stop, drain)
	}
}
