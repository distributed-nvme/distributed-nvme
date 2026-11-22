package ctl

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/spf13/viper"
)

// Every key of dnvctl's shipped config example, deploy/dnv/dnvctl.yaml, set
// or offered by a comment, a line "# key: value" of one value alone, is a
// global flag that has a config carrier, but --config itself (CT9), and the
// example names the gateway, the one value dnvctl requires.
func TestShippedConfigMatchesGlobals(t *testing.T) {
	path := filepath.Join("..", "deploy", "dnv", "dnvctl.yaml")
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
	for _, m := range regexp.MustCompile(`(?m)^# ?([a-z][a-z0-9-]*): [^ ]+$`).
		FindAllStringSubmatch(string(data), -1) {
		keys = append(keys, m[1])
	}
	carried := map[string]bool{}
	for _, name := range envGlobals {
		carried[name] = name != "config"
	}
	for _, key := range keys {
		if !carried[key] {
			t.Errorf("dnvctl.yaml: %q is no global flag a config file sets",
				key)
		}
	}
	if v.GetString("gateway-address") == "" {
		t.Error("dnvctl.yaml names no gateway-address")
	}
}
