package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// The shipped config example and unit of the gateway (architecture.md,
// Components: invocation reference).
var (
	shippedConfig = filepath.Join("..", "..", "deploy", "dnv", "dnv-gateway.yaml")
	shippedUnit   = filepath.Join("..", "..", "deploy", "systemd",
		"dnv-gateway.service")
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

// Every key of the shipped config example is a flag of the gateway, but
// --version, which no config key sets, and --config; the example alone
// gives the gateway every value it requires; and the shipped unit starts
// the gateway with that file at its place under /etc/dnv.
func TestShippedConfigMatchesFlags(t *testing.T) {
	flags := newRootCmd().Flags()
	for _, key := range configKeys(t, shippedConfig) {
		if key == "version" || key == "config" || flags.Lookup(key) == nil {
			t.Errorf("dnv-gateway.yaml: %q is no flag a config file sets",
				key)
		}
	}
	parse(t, []string{"--config", shippedConfig})
	if _, err := optionsFromViper(); err != nil {
		t.Errorf("dnv-gateway.yaml alone: %v", err)
	}
	want := []string{"/usr/local/bin/dnv-gateway", "--config",
		"/etc/dnv/dnv-gateway.yaml"}
	if got := unitExec(t, shippedUnit); !reflect.DeepEqual(got, want) {
		t.Errorf("dnv-gateway.service starts %q, want %q", got, want)
	}
}
