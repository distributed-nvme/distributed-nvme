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

// TestShippedFilterUnitLoadsAndRemovesDnvsTableAlone pins the kit's
// firewall unit, deploy/systemd/dnv-filter.service (the operations doc, The
// networks): it loads the table file the operations doc installs, before
// network-pre.target and from sysinit.target, so the table stands before
// the system's network services set the interfaces up and before any dnv
// program starts, and its stop deletes the table the example file names
// and no other, never the whole rule set.
func TestShippedFilterUnitLoadsAndRemovesDnvsTableAlone(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "deploy", "systemd",
		"dnv-filter.service"))
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	table, err := os.ReadFile(filepath.Join("..", "deploy", "nftables",
		"dnv-filter.nft"))
	if err != nil {
		t.Fatalf("read the table: %v", err)
	}
	if !strings.Contains(string(table), "\ntable inet dnv_filter {") {
		t.Fatalf("deploy/nftables/dnv-filter.nft names no table inet " +
			"dnv_filter")
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(string(unit), "\n") {
		if !strings.HasPrefix(line, "#") {
			lines[strings.TrimSpace(line)] = true
		}
	}
	for _, want := range []string{
		"DefaultDependencies=no",
		"Wants=network-pre.target",
		"Before=network-pre.target",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"After=nftables.service",
		"ExecStart=/usr/sbin/nft -f /etc/dnv/dnv-filter.nft",
		"ExecReload=/usr/sbin/nft -f /etc/dnv/dnv-filter.nft",
		"ExecStop=-/usr/sbin/nft delete table inet dnv_filter",
		"WantedBy=sysinit.target",
	} {
		if !lines[want] {
			t.Errorf("dnv-filter.service lacks the line %q", want)
		}
	}
	// Nothing stops the unit at shutdown, so the table stands until the
	// machine is off; and its stop never empties the whole rule set.
	for _, banned := range []string{
		"Conflicts=shutdown.target", "Before=shutdown.target",
	} {
		if lines[banned] {
			t.Errorf("dnv-filter.service has the line %q", banned)
		}
	}
	for line := range lines {
		if strings.Contains(line, "flush ruleset") {
			t.Errorf("dnv-filter.service flushes the whole rule set: %q",
				line)
		}
	}
}
