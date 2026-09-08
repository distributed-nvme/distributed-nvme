// Tests for the parsing and rendering surface of cdcctl (cdc.md §9.6). The
// driver writes straight into the etcd the whole suite reads back, and every
// field it gets wrong is invisible until a much later assertion fails on a
// host: a shard code read as decimal serves the entry from the wrong pair of
// instances, a reordered nvme_tr_conf_list breaks the DS5 index, and an
// allowed_hosts that vanishes from a `list` row cannot be compared with the
// §9.5 table. These cover exactly that surface.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

func TestParseId(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"0", 0, true},
		{"1", 1, true},
		{"15", 15, true},
		{"0xa", 0xa, true},
		{"0XA", 0xa, true},
		{" 0xcdc2 ", 0xcdc2, true},
		{"0x1cdc2", 0x1cdc2, true},
		{"18446744073709551615", 1<<64 - 1, true},
		{"", 0, false},
		{"-1", 0, false},
		{"0x", 0, false},
		{"ssa", 0, false},
		{"18446744073709551616", 0, false},
	}
	for _, tc := range cases {
		got, err := parseId(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("parseId(%q) err = %v, want ok = %v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseId(%q) = %#x, want %#x", tc.in, got, tc.want)
		}
	}
}

func TestHexUintFlag(t *testing.T) {
	var id hexUint
	if err := id.Set("0xcdc2"); err != nil {
		t.Fatalf("Set(0xcdc2) failed: %v", err)
	}
	if uint64(id) != 0xcdc2 {
		t.Fatalf("Set(0xcdc2) = %#x, want 0xcdc2", uint64(id))
	}
	if got := id.String(); got != "0xcdc2" {
		t.Fatalf("String() = %q, want 0xcdc2", got)
	}
	if err := id.Set("ss"); err == nil {
		t.Fatalf("Set(ss) succeeded, want an error")
	}
}

// TestShardFlag pins the §9.5 shard codes. "81" is the one that matters most:
// read as decimal it would become 0x51, which is in the LOW half and would be
// served by the wrong two instances of §9.3.
func TestShardFlag(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
	}{
		{"00", 0},
		{"0", 0},
		{"07", 0x07},
		{"08", 0x08},
		{"3c", 0x3c},
		{"3C", 0x3c},
		{"81", 0x81},
		{"ff", 0xff},
		{"0xff", 0xff},
		{" ff ", 0xff},
	}
	for _, tc := range cases {
		var shard shardFlag
		if err := shard.Set(tc.in); err != nil {
			t.Errorf("Set(%q) failed: %v", tc.in, err)
			continue
		}
		if !shard.set {
			t.Errorf("Set(%q) left the flag unset", tc.in)
		}
		if shard.code != tc.want {
			t.Errorf("Set(%q) = %#x, want %#x", tc.in, shard.code, tc.want)
		}
	}
	// 256 and up have no two-digit %02x spelling and no shard bucket slot.
	for _, bad := range []string{"", "100", "-1", "zz", "0x100"} {
		var shard shardFlag
		if err := shard.Set(bad); err == nil {
			t.Errorf("Set(%q) succeeded, want an error", bad)
		}
	}
}

// TestShardFlagRemembersUnset is the whole reason this flag is a struct: shard
// 00 is a code §9.5 uses (ssA), so "not given" has to be distinguishable from
// "given 00" by something other than the value.
func TestShardFlagRemembersUnset(t *testing.T) {
	var shard shardFlag
	if shard.set {
		t.Fatalf("a fresh shardFlag reports itself set")
	}
	if got := shard.String(); got != "unset" {
		t.Fatalf("String() = %q, want unset", got)
	}
	if err := shard.Set("00"); err != nil {
		t.Fatalf("Set(00) failed: %v", err)
	}
	if !shard.set || shard.code != 0 {
		t.Fatalf("Set(00) = {set:%v code:%#x}, want {true 0x0}", shard.set,
			shard.code)
	}
	if got := shard.String(); got != "00" {
		t.Fatalf("String() = %q, want 00", got)
	}
}

func TestParseTrConf(t *testing.T) {
	conf, err := parseTrConf("tcp,ipv4,10.0.0.2,14420")
	if err != nil {
		t.Fatalf("parseTrConf failed: %v", err)
	}
	want := &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "10.0.0.2",
		TrSvcId: "14420",
	}
	if !proto.Equal(conf, want) {
		t.Fatalf("parseTrConf = %v, want %v", conf, want)
	}
	// Comma separation exists so that an IPv6 tr_addr survives verbatim.
	conf, err = parseTrConf(" tcp , ipv6 , fd00::2 , 14421 ")
	if err != nil {
		t.Fatalf("parseTrConf(ipv6) failed: %v", err)
	}
	if conf.GetTrAddr() != "fd00::2" || conf.GetTrSvcId() != "14421" {
		t.Fatalf("parseTrConf(ipv6) = %v, want fd00::2 / 14421", conf)
	}
	// DS3 skips an element whose tr_type is not tcp and keeps serving the
	// rest of the entry, so the driver must be able to write one.
	if _, err := parseTrConf("rdma,ipv4,10.0.0.2,4420"); err != nil {
		t.Fatalf("parseTrConf(rdma) failed: %v", err)
	}
	for _, bad := range []string{
		"", "tcp", "tcp,ipv4,10.0.0.2", "tcp,ipv4,10.0.0.2,14420,x",
		"tcp,,10.0.0.2,14420", ",ipv4,10.0.0.2,14420",
		"tcp,ipv4, ,14420", "tcp,ipv4,10.0.0.2,",
	} {
		if _, err := parseTrConf(bad); err == nil {
			t.Errorf("parseTrConf(%q) succeeded, want an error", bad)
		}
	}
}

// TestTrConfListKeepsOrder is the DS5 index rule: §9.11 step 3 asserts ssD's
// two records by their trsvcids, in the order the case listed them.
func TestTrConfListKeepsOrder(t *testing.T) {
	var list trConfList
	for _, spec := range []string{
		"tcp,ipv4,10.0.0.2,14421",
		"tcp,ipv4,10.0.0.2,14420",
		"tcp,ipv4,10.0.0.2,14421",
	} {
		if err := list.Set(spec); err != nil {
			t.Fatalf("Set(%q) failed: %v", spec, err)
		}
	}
	got := make([]string, 0, len(list))
	for _, conf := range list {
		got = append(got, conf.GetTrSvcId())
	}
	want := []string{"14421", "14420", "14421"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tr svc ids = %v, want %v (order kept, duplicates kept)",
			got, want)
	}
	if err := list.Set("tcp,ipv4,10.0.0.2"); err == nil {
		t.Fatalf("Set of a 3-field spec succeeded, want an error")
	}
	if len(list) != 3 {
		t.Fatalf("a rejected --tr changed the list: %d entries", len(list))
	}
}

func TestStringListKeepsOrder(t *testing.T) {
	var list stringList
	for _, host := range []string{"nqn.h1", " nqn.h2 "} {
		if err := list.Set(host); err != nil {
			t.Fatalf("Set(%q) failed: %v", host, err)
		}
	}
	if strings.Join(list, ",") != "nqn.h1,nqn.h2" {
		t.Fatalf("allowed = %v, want [nqn.h1 nqn.h2]", []string(list))
	}
	// An empty --allowed would be a host nothing can ever match, and DS4
	// would filter every host out of an entry that was meant to be open.
	if err := list.Set("  "); err == nil {
		t.Fatalf("Set of an empty hostnqn succeeded, want an error")
	}
}

// TestEntryKeyGolden pins the key the whole suite addresses: the §9.5 ssF row
// (second cluster id, shard 81) in the MD2 spelling.
func TestEntryKeyGolden(t *testing.T) {
	got := model.CdcEntryKey(0x1cdc2, 0x81, 0x4, 0xf)
	want := "dnv cdc 000000000001cdc2 81 0000000000000004 " +
		"000000000000000f"
	if got != want {
		t.Fatalf("CdcEntryKey = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, model.CdcEntryPrefix()) {
		t.Fatalf("%q is not under %q", got, model.CdcEntryPrefix())
	}
}

// TestOpenEntryRendersEmptyAllowedHosts is why marshalOpts sets
// EmitUnpopulated: ssA is the §0 #5 entry visible to everyone, and its
// allowed_hosts must be readable as an explicit [] in a `list` row rather than
// be missing from the document.
func TestOpenEntryRendersEmptyAllowedHosts(t *testing.T) {
	entry := &pb.CdcEntry{
		Nqn: "nqn.2024-01.io.dnv-it:cdc:ssa",
		NvmeTrConfList: []*pb.NvmeTrConf{{
			TrType:  "tcp",
			AdrFam:  "ipv4",
			TrAddr:  "10.0.0.2",
			TrSvcId: "14420",
		}},
	}
	raw := mustJson(pbToAny(entry))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("re-parsing %s failed: %v", raw, err)
	}
	hosts, ok := doc["allowed_hosts"]
	if !ok {
		t.Fatalf("allowed_hosts is missing from %s", raw)
	}
	if list, isList := hosts.([]any); !isList || len(list) != 0 {
		t.Fatalf("allowed_hosts = %v, want an empty list", hosts)
	}
	// The proto field names of the assertions, not the JSON camelCase ones.
	if !strings.Contains(string(raw), `"nvme_tr_conf_list"`) {
		t.Fatalf("%s does not use the proto field names", raw)
	}
	// One line, so that a `list` row stays one line.
	if strings.ContainsAny(string(raw), "\n\r") {
		t.Fatalf("%s is not a single line", raw)
	}
}
