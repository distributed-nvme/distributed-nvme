package common

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The shipped example unit of etcd, deploy/systemd/dnv-etcd.service, starts
// etcd with the two request limits every etcd serving dnv must have, at
// EtcdMaxTxnOps and EtcdMaxRequestBytes, with automatic compaction of its
// history, and with no watch progress notification interval of its own, so
// that etcd's default, which the etcd client's silence bound rests on, holds
// (architecture.md, Components: invocation reference).
func TestShippedEtcdUnitCarriesTheRequestLimits(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "deploy", "systemd",
		"dnv-etcd.service"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\\\n", " ")
	m := regexp.MustCompile(`(?m)^ExecStart=(.*)$`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("dnv-etcd.service has no ExecStart=")
	}
	flags := map[string]string{}
	for _, word := range strings.Fields(m[1]) {
		name, value, _ := strings.Cut(strings.TrimPrefix(word, "--"), "=")
		flags[name] = value
	}
	for name, want := range map[string]int{
		"max-txn-ops":       EtcdMaxTxnOps,
		"max-request-bytes": EtcdMaxRequestBytes,
	} {
		got, err := strconv.Atoi(flags[name])
		if err != nil || got != want {
			t.Errorf("dnv-etcd.service: --%s=%q, want --%s=%d", name,
				flags[name], name, want)
		}
	}
	if flags["auto-compaction-retention"] == "" {
		t.Error("dnv-etcd.service: no --auto-compaction-retention=")
	}
	for name := range flags {
		if strings.Contains(name, "watch-progress-notify-interval") {
			t.Errorf("dnv-etcd.service sets --%s", name)
		}
	}
}
