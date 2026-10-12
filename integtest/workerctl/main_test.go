// Tests for the parsing surface of workerctl (dnv-worker.md,
// Integration test plan, The driver). The driver writes straight into the etcd
// the whole suite reads back, so a malformed test invocation must fail loudly
// here rather than land garbage in a key: these cover every tuple form of its
// flags, the 0x hex ids, the bitmap payload, the key kind → message mapping of
// `get` (architecture.md, Key table), the put-sp wiring validation that
// joins the placement flags on their ids, and the metadata units the planted
// copies cost (architecture.md, Metadata units): the units put-clone and
// put-migr size them by and, over an in-memory STM, put-clone's charge to the
// CN of every cntlr of the pool and put-migr's charge to its destination DN.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
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
		{"17", 17, true},
		{"0x11", 17, true},
		{"0X11", 17, true},
		{" 0x11 ", 17, true},
		{"0xebada5168620c5fe", 0xebada5168620c5fe, true},
		{"18446744073709551615", 1<<64 - 1, true},
		{"0b101", 5, true},
		{"", 0, false},
		{"-1", 0, false},
		{"0x", 0, false},
		{"sp0", 0, false},
		{"18446744073709551616", 0, false},
	}
	for _, tc := range cases {
		got, err := parseId(tc.in)
		if tc.ok && err != nil {
			t.Errorf("parseId(%q) failed: %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("parseId(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("parseId(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestHexUintFlag(t *testing.T) {
	var id hexUint
	if err := id.Set("0x2a"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if uint64(id) != 42 {
		t.Fatalf("Set(0x2a) = %d, want 42", uint64(id))
	}
	if got := id.String(); got != "0x2a" {
		t.Fatalf("String() = %q, want 0x2a", got)
	}
	if err := id.Set("nope"); err == nil {
		t.Fatalf("Set(nope) succeeded, want an error")
	}
}

func TestTriBool(t *testing.T) {
	var flag triBool
	if flag.set {
		t.Fatalf("a fresh triBool is set")
	}
	if got := flag.String(); got != "unset" {
		t.Fatalf("String() = %q, want unset", got)
	}
	if err := flag.Set("false"); err != nil {
		t.Fatalf("Set(false) failed: %v", err)
	}
	// "given false" must differ from "not given": set-cntlr changes only the
	// flags the caller named.
	if !flag.set || flag.value {
		t.Fatalf("Set(false) gave set=%v value=%v", flag.set, flag.value)
	}
	if err := flag.Set("1"); err != nil {
		t.Fatalf("Set(1) failed: %v", err)
	}
	if !flag.value {
		t.Fatalf("Set(1) did not set the value")
	}
	if err := flag.Set("maybe"); err == nil {
		t.Fatalf("Set(maybe) succeeded, want an error")
	}
	if !flag.IsBoolFlag() {
		t.Fatalf("IsBoolFlag() = false, want true so --primary alone works")
	}
}

func TestParseSidePointer(t *testing.T) {
	ptr, err := parseSidePointer("1:0x3:5")
	if err != nil {
		t.Fatalf("parseSidePointer failed: %v", err)
	}
	want := &pb.SidePointer{SpId: 1, LegId: 3, SideId: 5}
	if !proto.Equal(ptr, want) {
		t.Fatalf("parseSidePointer = %v, want %v", ptr, want)
	}
	for _, bad := range []string{"", "1:2", "1:2:3:4", "1:x:3", "1::3"} {
		if _, err := parseSidePointer(bad); err == nil {
			t.Errorf("parseSidePointer(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseCntlrPointer(t *testing.T) {
	ptr, err := parseCntlrPointer("0x1:2")
	if err != nil {
		t.Fatalf("parseCntlrPointer failed: %v", err)
	}
	want := &pb.CntlrPointer{SpId: 1, CntlrId: 2}
	if !proto.Equal(ptr, want) {
		t.Fatalf("parseCntlrPointer = %v, want %v", ptr, want)
	}
	for _, bad := range []string{"", "1", "1:2:3", "a:2"} {
		if _, err := parseCntlrPointer(bad); err == nil {
			t.Errorf("parseCntlrPointer(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseCntlrSpec(t *testing.T) {
	got, err := parseCntlrSpec("0x1:2:3:true")
	if err != nil {
		t.Fatalf("parseCntlrSpec failed: %v", err)
	}
	want := cntlrSpec{cntlrId: 1, cnId: 2, slot: 3, primary: true}
	if got != want {
		t.Fatalf("parseCntlrSpec = %+v, want %+v", got, want)
	}
	got, err = parseCntlrSpec("2:2:1:0")
	if err != nil {
		t.Fatalf("parseCntlrSpec failed: %v", err)
	}
	if got.primary {
		t.Fatalf("parseCntlrSpec(...:0) is primary")
	}
	for _, bad := range []string{
		"", "1:2:3", "1:2:3:4:5", "1:2:3:yesplease", "1:2:0x1ffffffff:true",
	} {
		if _, err := parseCntlrSpec(bad); err == nil {
			t.Errorf("parseCntlrSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseSliceSpec(t *testing.T) {
	got, err := parseSliceSpec("3:0")
	if err != nil {
		t.Fatalf("parseSliceSpec failed: %v", err)
	}
	if got != (sliceSpec{sliceId: 3, sliceIdx: 0}) {
		t.Fatalf("parseSliceSpec = %+v", got)
	}
	for _, bad := range []string{"", "3", "3:0:1", "x:0"} {
		if _, err := parseSliceSpec(bad); err == nil {
			t.Errorf("parseSliceSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseGroupSpec(t *testing.T) {
	got, err := parseGroupSpec("1:2:meta:1:raid1")
	if err != nil {
		t.Fatalf("parseGroupSpec failed: %v", err)
	}
	want := groupSpec{sliceId: 1, grpId: 2, isMeta: true, extCnt: 1, raid1: true}
	if got != want {
		t.Fatalf("parseGroupSpec = %+v, want %+v", got, want)
	}
	got, err = parseGroupSpec("1:0x4:data:2:none")
	if err != nil {
		t.Fatalf("parseGroupSpec failed: %v", err)
	}
	want = groupSpec{sliceId: 1, grpId: 4, isMeta: false, extCnt: 2, raid1: false}
	if got != want {
		t.Fatalf("parseGroupSpec = %+v, want %+v", got, want)
	}
	for _, bad := range []string{
		"",
		"1:2:meta:1",
		"1:2:mirror:1:raid1",
		"1:2:meta:1:raid5",
		// ext_cnt 0 has no group geometry at all (architecture.md, Group
		// on-leg layout: meta region, data region, health block).
		"1:2:meta:0:raid1",
	} {
		if _, err := parseGroupSpec(bad); err == nil {
			t.Errorf("parseGroupSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseLegSpec(t *testing.T) {
	got, err := parseLegSpec("0x2:3:1")
	if err != nil {
		t.Fatalf("parseLegSpec failed: %v", err)
	}
	if got != (legSpec{grpId: 2, legId: 3, legIdx: 1}) {
		t.Fatalf("parseLegSpec = %+v", got)
	}
	for _, bad := range []string{"", "2:3", "2:3:1:0", "2:3:x"} {
		if _, err := parseLegSpec(bad); err == nil {
			t.Errorf("parseLegSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseSideSpec(t *testing.T) {
	got, err := parseSideSpec("3:0x4:1:0")
	if err != nil {
		t.Fatalf("parseSideSpec failed: %v", err)
	}
	if got != (sideSpec{legId: 3, sideId: 4, dnId: 1, slot: 0}) {
		t.Fatalf("parseSideSpec = %+v", got)
	}
	for _, bad := range []string{"", "3:4:1", "3:4:1:0:9", "3:4:1:x"} {
		if _, err := parseSideSpec(bad); err == nil {
			t.Errorf("parseSideSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseMigrSpec(t *testing.T) {
	got, err := parseMigrSpec("m0:3:1:0x9")
	if err != nil {
		t.Fatalf("parseMigrSpec failed: %v", err)
	}
	want := migrSpec{name: "m0", migrId: 3, srcSide: 1, dstSide: 9}
	if got != want {
		t.Fatalf("parseMigrSpec = %+v, want %+v", got, want)
	}
	for _, bad := range []string{"", "m0:3:1", "m0:3:1:9:0", ":3:1:9", "m0:x:1:9"} {
		if _, err := parseMigrSpec(bad); err == nil {
			t.Errorf("parseMigrSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseNsSpec(t *testing.T) {
	got, err := parseNsSpec("5:1:0x7")
	if err != nil {
		t.Fatalf("parseNsSpec failed: %v", err)
	}
	if got != (nsSpec{nsId: 5, nsIdx: 1, tdId: 7}) {
		t.Fatalf("parseNsSpec = %+v", got)
	}
	for _, bad := range []string{"", "5:1", "5:1:7:9", "5:x:7"} {
		if _, err := parseNsSpec(bad); err == nil {
			t.Errorf("parseNsSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseU32List(t *testing.T) {
	got, err := parseU32List("0,1,0x2")
	if err != nil {
		t.Fatalf("parseU32List failed: %v", err)
	}
	if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("parseU32List = %v", got)
	}
	if got, err := parseU32List("  "); err != nil || got != nil {
		t.Fatalf("parseU32List(blank) = %v, %v", got, err)
	}
	for _, bad := range []string{"0,,1", "0,x", "0,4294967296"} {
		if _, err := parseU32List(bad); err == nil {
			t.Errorf("parseU32List(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseHexBitmap(t *testing.T) {
	data, err := parseHexBitmap("0xdeadbeef")
	if err != nil {
		t.Fatalf("parseHexBitmap failed: %v", err)
	}
	if len(data) != 4 || data[0] != 0xde || data[3] != 0xef {
		t.Fatalf("parseHexBitmap = %x", data)
	}
	if data, err = parseHexBitmap("00ff"); err != nil {
		t.Fatalf("parseHexBitmap failed: %v", err)
	}
	if len(data) != 2 || data[1] != 0xff {
		t.Fatalf("parseHexBitmap = %x", data)
	}
	for _, bad := range []string{"", "0x", "abc", "zz", "0xzz"} {
		if _, err := parseHexBitmap(bad); err == nil {
			t.Errorf("parseHexBitmap(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseSpLevel(t *testing.T) {
	cases := []struct {
		in   string
		want pb.SpLevel
	}{
		{"0", pb.SpLevel_SP_LEVEL_READWRITE},
		{"48", pb.SpLevel_SP_LEVEL_NO_THINPOOL},
		{"0x30", pb.SpLevel_SP_LEVEL_NO_THINPOOL},
		{"no_thinpool", pb.SpLevel_SP_LEVEL_NO_THINPOOL},
		{"SP_LEVEL_READONLY", pb.SpLevel_SP_LEVEL_READONLY},
	}
	for _, tc := range cases {
		got, err := parseSpLevel(tc.in)
		if err != nil {
			t.Errorf("parseSpLevel(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSpLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// 47 is between two defined levels: storing it would leave a level no rule
	// of architecture.md, Common validation, knows.
	for _, bad := range []string{"", "47", "-1", "nonesuch"} {
		if _, err := parseSpLevel(bad); err == nil {
			t.Errorf("parseSpLevel(%q) succeeded, want an error", bad)
		}
	}
}

// TestMessageForKey pins the mapping `get` uses (architecture.md, Key table):
// the SECOND field of a key names the message its value decodes into.
func TestMessageForKey(t *testing.T) {
	cases := []struct {
		key  string
		want proto.Message
	}{
		{"dnv cluster_conf it-smoke", &pb.ClusterConf{}},
		{"dnv dn_global ebada5168620c5fe", &pb.DnGlobal{}},
		{"dnv cn_global ebada5168620c5fe", &pb.CnGlobal{}},
		{"dnv sp_global ebada5168620c5fe", &pb.SpGlobal{}},
		{"dnv dn_rev 00 ebada5168620c5fe 0000000000000001", &pb.DnRev{}},
		{"dnv cn_rev 00 ebada5168620c5fe 0000000000000001", &pb.CnRev{}},
		{"dnv sp_rev 00 ebada5168620c5fe 0000000000000001", &pb.SpRev{}},
		{"dnv dn_conf ebada5168620c5fe 10.0.0.1:29600", &pb.DnConf{}},
		{"dnv cn_conf ebada5168620c5fe 10.0.0.1:29700", &pb.CnConf{}},
		{
			"dnv dn_capacity ebada5168620c5fe 0 0000000000000008 " +
				"10.0.0.1:29600",
			&pb.DnCapacity{},
		},
		{
			"dnv cn_capacity ebada5168620c5fe 0000000000000040 " +
				"10.0.0.1:29700",
			&pb.CnCapacity{},
		},
		{
			"dnv cdc ebada5168620c5fe 00 0000000000000001 " +
				"0000000000000002",
			&pb.CdcEntry{},
		},
		{"dnv sp_conf ebada5168620c5fe sp0", &pb.SpConf{}},
		{
			"dnv cntlr ebada5168620c5fe 0000000000000001 " +
				"0000000000000001",
			&pb.Cntlr{},
		},
		{
			"dnv slice ebada5168620c5fe 0000000000000001 " +
				"0000000000000001",
			&pb.Slice{},
		},
		{
			"dnv thin_device ebada5168620c5fe 0000000000000001 td0",
			&pb.ThinDevice{},
		},
		{
			"dnv subsystem ebada5168620c5fe 0000000000000001 " +
				"nqn.2024-01.io.dnv-it:ss0",
			&pb.Subsystem{},
		},
		{"dnv clone ebada5168620c5fe 0000000000000001 c0", &pb.Clone{}},
		{
			// Seven fields: a clone chunk carries the src_slice_idx and
			// the bm_idx it is addressed by, both as two hex digits.
			"dnv clone_bitmap ebada5168620c5fe 0000000000000001 c0 02 01",
			&pb.CloneBitmap{},
		},
		{"dnv transfer ebada5168620c5fe 0000000000000001 x0", &pb.Transfer{}},
		{"dnv migration ebada5168620c5fe 0000000000000001 m0", &pb.Migration{}},
		{
			"dnv migration_bitmap ebada5168620c5fe 0000000000000001 m0 01",
			&pb.MigrBitmap{},
		},
		{
			"dnv sp_id_to_name ebada5168620c5fe 0000000000000001",
			&pb.SpName{},
		},
		{"dnv worker dn 0123456789abcdef", &pb.WorkerReg{}},
	}
	for _, tc := range cases {
		got, err := messageForKey(tc.key)
		if err != nil {
			t.Errorf("messageForKey(%q) failed: %v", tc.key, err)
			continue
		}
		gotName := got.ProtoReflect().Descriptor().FullName()
		wantName := tc.want.ProtoReflect().Descriptor().FullName()
		if gotName != wantName {
			t.Errorf("messageForKey(%q) = %s, want %s",
				tc.key, gotName, wantName)
		}
	}
	for _, bad := range []string{
		"",
		"dnv",
		"dnv sp_conf",
		"dnv nonesuch ebada5168620c5fe",
		"other sp_conf ebada5168620c5fe sp0",
	} {
		if _, err := messageForKey(bad); err == nil {
			t.Errorf("messageForKey(%q) succeeded, want an error", bad)
		}
	}
}

// TestKindsCoverKeyTable checks that every kind `get` can decode also knows
// whether its key is cluster-scoped, which is what list-keys needs, and that
// the table has not grown a kind whose message builder is nil.
func TestKindsCoverKeyTable(t *testing.T) {
	for name, info := range kinds {
		if info.newMsg == nil {
			t.Errorf("kind %q has no message builder", name)
			continue
		}
		if info.newMsg() == nil {
			t.Errorf("kind %q builds a nil message", name)
		}
	}
	// The three rev kinds carry {shard_code} before {cluster_id}, so they can
	// never be scoped to a cluster by prefix.
	for _, name := range []string{"dn_rev", "cn_rev", "sp_rev", "worker",
		"cluster_conf"} {
		if kinds[name].clusterScoped {
			t.Errorf("kind %q must not be cluster scoped", name)
		}
	}
	for _, name := range []string{"dn_conf", "cn_conf", "dn_capacity",
		"cn_capacity", "sp_conf", "cntlr", "slice", "cdc"} {
		if !kinds[name].clusterScoped {
			t.Errorf("kind %q must be cluster scoped", name)
		}
	}
}

func TestKeyPrefix(t *testing.T) {
	got := keyPrefix(common.DnvPrefix, "dn_conf", idHex(0xebada5168620c5fe))
	want := "dnv dn_conf ebada5168620c5fe "
	if got != want {
		t.Fatalf("keyPrefix = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, " ") {
		t.Fatalf("a scan prefix must end in the field separator")
	}
}

// TestConstantsEmitsWhatTheSuitesRead pins `constants`, the channel through
// which the shell suites read a Go constant (`geometry` and `meta-units` are
// the others, for computed values). It prints eighteen keys, and the suites
// read fifteen of them at preflight and stop there when one is missing.
// Every suite that starts an etcd — the worker, gateway, cdc and e2e suites —
// launches it with EtcdMaxTxnOps and EtcdMaxRequestBytes. The e2e suite
// cross-checks MaxAllocLegPerGrp. The gateway suite creates its stage-13
// clone at MaxSliceCntPerSp, checks the health intervals a new cluster stores
// against DefaultHealthCheckInterval, fills the metadata areas in its ceiling
// case to CnCloneMetaUnitCnt and DnCloneMetaUnitCnt in units of
// CnCloneMetaUnit, which it requires DnCloneMetaUnit to equal, and checks the
// counters the id step moves against IdStepStride and ThinIdStepStride. The
// worker and e2e suites read DefaultHealthCheckInterval,
// DefaultPrimaryUnhealthy and DemotionHoldTimeout for their failover cases.
// The cdc suite's flood case fills the pending cap, CdcMaxPendingConnCnt, and
// waits by the pre-Connect deadline, DefaultCdcPendingTmoMs. One of those
// fifteen dropped or renamed here would otherwise surface only when a suite
// is run, at its preflight. No suite reads the other three, MaxDelGrpPerTxn,
// MaxSpareLegPerGrp and MaxDelBmPerTxn; they are pinned as well, because a
// case may size itself against them.
func TestConstantsEmitsWhatTheSuitesRead(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = write
	g := newGlobals()
	cmdConstants(&g, nil)
	os.Stdout = saved
	write.Close()
	out, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("reading the constants output: %v", err)
	}
	var got map[string]uint64
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("constants printed %q, not one JSON object: %v", out, err)
	}
	want := map[string]uint64{
		"EtcdMaxTxnOps":       common.EtcdMaxTxnOps,
		"EtcdMaxRequestBytes": common.EtcdMaxRequestBytes,

		"MaxDelGrpPerTxn":   common.MaxDelGrpPerTxn,
		"MaxAllocLegPerGrp": common.MaxAllocLegPerGrp,
		"MaxSpareLegPerGrp": common.MaxSpareLegPerGrp,
		"MaxDelBmPerTxn":    common.MaxDelBmPerTxn,
		"MaxSliceCntPerSp":  common.MaxSliceCntPerSp,

		"DefaultHealthCheckInterval": common.DefaultHealthCheckInterval,
		"DefaultPrimaryUnhealthy":    common.DefaultPrimaryUnhealthy,
		"DemotionHoldTimeout":        common.DemotionHoldTimeout,

		"CnCloneMetaUnitCnt": common.CnCloneMetaUnitCnt,
		"DnCloneMetaUnitCnt": common.DnCloneMetaUnitCnt,
		"CnCloneMetaUnit":    common.CnCloneMetaUnit,
		"DnCloneMetaUnit":    common.DnCloneMetaUnit,

		"IdStepStride":     common.IdStepStride,
		"ThinIdStepStride": common.ThinIdStepStride,

		"CdcMaxPendingConnCnt":   common.CdcMaxPendingConnCnt,
		"DefaultCdcPendingTmoMs": common.DefaultCdcPendingTmoMs,
	}
	for key, value := range want {
		if have, ok := got[key]; !ok || have != value {
			t.Errorf("constants %s = %d (present %t), want %d",
				key, have, ok, value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("constants printed %d keys, want %d: %s",
			len(got), len(want), out)
	}
}

// TestGeometryEmitsWhatTheSuitesRead pins `geometry`, through which the worker
// suite reads a group's block counts and its sides' lengths to zero
// (architecture.md, Side provisioning protocol; dnv-worker.md RW15) instead of
// hand-copying either formula: a meta group's side is zeroed over the whole
// leg, which at an extent size that is a multiple of the block size is the
// whole side, and a data group's over its meta region and first data block.
func TestGeometryEmitsWhatTheSuitesRead(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want map[string]uint64
	}{
		{
			// md superblock, one bitmap block and the health block.
			name: "md-raid1",
			args: []string{"--raid1", "--ext-cnt", "2",
				"--extent-size", "67108864", "--block-size", "1048576",
				"--chunk-blocks", "128"},
			want: map[string]uint64{
				"ext_cnt":         2,
				"meta_blocks":     3,
				"data_blocks":     125,
				"meta_zero_bytes": 128 << 20,
				"data_zero_bytes": 4 << 20,
			},
		},
		{
			// The health block alone.
			name: "redund_none",
			args: []string{"--ext-cnt", "1",
				"--extent-size", "67108864", "--block-size", "1048576"},
			want: map[string]uint64{
				"ext_cnt":         1,
				"meta_blocks":     1,
				"data_blocks":     63,
				"meta_zero_bytes": 64 << 20,
				"data_zero_bytes": 2 << 20,
			},
		},
	}
	for _, tc := range cases {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		saved := os.Stdout
		os.Stdout = write
		g := newGlobals()
		cmdGeometry(&g, tc.args)
		os.Stdout = saved
		write.Close()
		out, err := io.ReadAll(read)
		if err != nil {
			t.Fatalf("%s: reading the geometry output: %v", tc.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: geometry printed %q, not one JSON object: %v",
				tc.name, out, err)
		}
		for key, value := range tc.want {
			have, ok := got[key].(float64)
			if !ok || uint64(have) != value {
				t.Errorf("%s: geometry %s = %v, want %d",
					tc.name, key, got[key], value)
			}
		}
	}
}

// captureStdout runs one subcommand and returns what it printed.
func captureStdout(
	t *testing.T,
	cmd func(*globals, []string),
	args []string,
) []byte {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = write
	g := newGlobals()
	cmd(&g, args)
	os.Stdout = saved
	write.Close()
	out, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("reading the output: %v", err)
	}
	return out
}

// TestMetaUnitsEmitsWhatTheSuitesRead pins `meta-units`, through which the
// suites read the units a copy costs (architecture.md, Metadata units)
// instead of re-deriving the formula: the two agents' own examples, a copy
// under one block, and every copy the gateway suite's ceiling case, its smoke
// case and the worker suite's drain case size by it. --block-size defaults to
// common.DefaultDmPoolDataBlockSize, and a zero for either input is refused.
func TestMetaUnitsEmitsWhatTheSuitesRead(t *testing.T) {
	const tib = uint64(1) << 40
	cases := []struct {
		name  string
		args  []string
		bytes uint64
		block uint64
		units uint64
	}{
		{"the cn agent's example",
			[]string{"--bytes", "1099511627776", "--block-size", "65536"},
			tib, 64 << 10, 5},
		{"the dn agent's example",
			[]string{"--bytes", "1099511627776", "--block-size", "1048576"},
			tib, 1 << 20, 2},
		{"a copy under one block",
			[]string{"--bytes", "65536", "--block-size", "1048576"},
			64 << 10, 1 << 20, 1},
		{"the ceiling case's clone too big for any CN",
			[]string{"--bytes", "70368744177664", "--block-size", "65536"},
			70368744177664, 64 << 10, 257},
		{"the ceiling case's migration that fills a DN",
			[]string{"--bytes", "12919261626368", "--block-size", "65536"},
			12919261626368, 64 << 10, 48},
		{"the ceiling case's clone of a quarter of a CN",
			[]string{"--bytes", "17317308137472", "--block-size", "65536"},
			17317308137472, 64 << 10, 64},
		{"the ceiling case's two-unit clone",
			[]string{"--bytes", "274877906944", "--block-size", "65536"},
			274877906944, 64 << 10, 2},
		{"the ceiling case's probe migration of one extent",
			[]string{"--bytes", "1073741824", "--block-size", "65536"},
			1 << 30, 64 << 10, 2},
		{"the smoke case's clone",
			[]string{"--bytes", "67108864", "--block-size", "1048576"},
			64 << 20, 1 << 20, 2},
		{"the smoke case's migration of two extents",
			[]string{"--bytes", "2147483648", "--block-size", "1048576"},
			2 << 30, 1 << 20, 2},
		{"the worker suite's drain-case clone",
			[]string{"--bytes", "10737418240", "--block-size", "1048576"},
			10 << 30, 1 << 20, 2},
		{"the default block size",
			[]string{"--bytes", "1099511627776"},
			tib, common.DefaultDmPoolDataBlockSize, 2},
	}
	for _, tc := range cases {
		out := captureStdout(t, cmdMetaUnits, tc.args)
		var got map[string]uint64
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: meta-units printed %q, not one JSON object: %v",
				tc.name, out, err)
		}
		want := map[string]uint64{
			"bytes": tc.bytes, "block_size": tc.block, "units": tc.units,
		}
		for key, value := range want {
			if have, ok := got[key]; !ok || have != value {
				t.Errorf("%s: meta-units %s = %d (present %t), want %d",
					tc.name, key, have, ok, value)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: meta-units printed %d keys, want %d: %s",
				tc.name, len(got), len(want), out)
		}
		// The subcommand is the formula and nothing else.
		if formula := common.CloneMetaUnits(tc.bytes, tc.block); formula !=
			tc.units {
			t.Errorf("%s: common.CloneMetaUnits = %d, the table says %d",
				tc.name, formula, tc.units)
		}
	}
	for _, bad := range []struct{ bytes, block uint64 }{
		{0, 1 << 20}, {1 << 30, 0}, {0, 0},
	} {
		if units, err := metaUnitsOf(bad.bytes, bad.block); err == nil {
			t.Errorf("meta-units --bytes %d --block-size %d = %d, "+
				"want a refusal", bad.bytes, bad.block, units)
		}
	}
}

// memStm is an in-memory etcdutil.STM over marshaled values, so a value read
// back is a fresh copy, as it is from etcd, and a staged write is seen by a
// later read. It counts the reads and the writes of every key: "one write
// per node" and "zero units read nothing" are counts.
type memStm struct {
	vals map[string][]byte
	gets map[string]int
	puts map[string]int
}

func newMemStm() *memStm {
	return &memStm{
		vals: make(map[string][]byte),
		gets: make(map[string]int),
		puts: make(map[string]int),
	}
}

func (m *memStm) Get(key string, msg proto.Message) bool {
	m.gets[key]++
	return m.peek(key, msg)
}

func (m *memStm) Put(key string, msg proto.Message) {
	m.seed(key, msg)
	m.puts[key]++
}

func (m *memStm) Del(key string) { delete(m.vals, key) }

func (m *memStm) Rev(key string) int64 { return 0 }

// seed stores a fixture value without counting a write.
func (m *memStm) seed(key string, msg proto.Message) {
	data, err := proto.Marshal(msg)
	if err != nil {
		panic(fmt.Sprintf("memStm: marshaling %q: %v", key, err))
	}
	m.vals[key] = data
}

// peek reads a value for an assertion without counting a read.
func (m *memStm) peek(key string, msg proto.Message) bool {
	data, ok := m.vals[key]
	if !ok {
		return false
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		panic(fmt.Sprintf("memStm: decoding %q: %v", key, err))
	}
	return true
}

// memStm must stay a valid etcdutil.STM.
var _ etcdutil.STM = (*memStm)(nil)

// testPoolBdev is a stored pool BdevConf with blockSize as its block size:
// every member model.ValidateBdevConf requires is concrete, and no
// redund_conf is the redund_none choice.
func testPoolBdev(blockSize uint64) *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   blockSize,
			LowWaterMarkPct: common.DefaultPoolLowWatermarkPct,
		},
		DmRaid0Conf: &pb.DmRaid0Conf{
			StripeSize: common.DefaultDmRaid0StripeSize,
		},
	}
}

// TestCloneChargePerCn pins put-clone's charge (architecture.md, Metadata
// units): the clone's units once per cntlr on the CN its record names, one
// write per CN, the capacity value following the record, no CnRev, and no
// refusal for room — the charge model.FinishCloneDelete gives back.
func TestCloneChargePerCn(t *testing.T) {
	const (
		cid    = uint64(0xc1d)
		spId   = uint64(7)
		cnA    = "10.0.0.1:29700"
		cnB    = "10.0.0.1:29701"
		cnGone = "10.0.0.1:29702"
		free   = uint64(64)
		units  = uint64(4)
	)
	// fixture plants a pool whose cntlr id i+1 sits on cntlrCns[i] (an empty
	// addr is a listed cntlr with no key) and the CNs cnA and cnB, cnA with
	// usedA units already used. cnGone has no CnConf.
	fixture := func(usedA uint64, cntlrCns ...string) (*memStm, *pb.SpConf) {
		m := newMemStm()
		conf := &pb.SpConf{SpId: spId}
		for i, addrPort := range cntlrCns {
			cntlrId := uint64(i + 1)
			conf.CntlrIdList = append(conf.CntlrIdList, cntlrId)
			if addrPort == "" {
				continue
			}
			m.seed(model.CntlrKey(cid, spId, cntlrId),
				&pb.Cntlr{AddrPort: addrPort})
		}
		m.seed(model.CnConfKey(cid, cnA), &pb.CnConf{
			CnId: 1, Location: "rack0", TotalExtCnt: free,
			FreeExtCnt: free, UsedMetaUnits: usedA,
		})
		m.seed(model.CnConfKey(cid, cnB), &pb.CnConf{
			CnId: 2, Location: "rack1", TotalExtCnt: free,
			FreeExtCnt: free,
		})
		return m, conf
	}
	// used reads a CN's count from its record and from its capacity value,
	// failing when the two disagree or the key is gone.
	used := func(t *testing.T, m *memStm, addrPort string) uint64 {
		t.Helper()
		cn := &pb.CnConf{}
		if !m.peek(model.CnConfKey(cid, addrPort), cn) {
			t.Fatalf("%s: no CnConf", addrPort)
		}
		value := &pb.CnCapacity{}
		if !m.peek(model.CnCapacityKey(cid, free, addrPort), value) {
			t.Fatalf("%s: the capacity key is gone", addrPort)
		}
		if value.GetUsedMetaUnits() != cn.GetUsedMetaUnits() {
			t.Fatalf("%s: capacity value carries %d used units, the "+
				"record %d", addrPort, value.GetUsedMetaUnits(),
				cn.GetUsedMetaUnits())
		}
		if value.GetLocation() != cn.GetLocation() {
			t.Fatalf("%s: capacity value location %q, the record %q",
				addrPort, value.GetLocation(), cn.GetLocation())
		}
		return cn.GetUsedMetaUnits()
	}
	// onlyNodeWrites fails on any write but a CnConf or a capacity key, and
	// on a second write of one key: a units-only charge bumps no CnRev.
	onlyNodeWrites := func(t *testing.T, m *memStm) {
		t.Helper()
		for key, cnt := range m.puts {
			if !strings.HasPrefix(key, common.DnvPrefix+" cn_conf ") &&
				!strings.HasPrefix(key, common.DnvPrefix+" cn_capacity ") {
				t.Errorf("the charge wrote %q", key)
			}
			if cnt != 1 {
				t.Errorf("the charge wrote %q %d times, want once", key, cnt)
			}
		}
	}

	t.Run("two cntlrs on two CNs charge each CN once", func(t *testing.T) {
		m, conf := fixture(10, cnA, cnB)
		charged, err := chargeCloneCns(m, cid, conf, units)
		if err != nil {
			t.Fatalf("chargeCloneCns: %v", err)
		}
		if got := used(t, m, cnA); got != 10+units {
			t.Errorf("cnA used %d, want %d", got, 10+units)
		}
		if got := used(t, m, cnB); got != units {
			t.Errorf("cnB used %d, want %d", got, units)
		}
		if len(charged) != 2 || charged[cnA] != units ||
			charged[cnB] != units {
			t.Errorf("charged %v, want %d on each of %s and %s",
				charged, units, cnA, cnB)
		}
		onlyNodeWrites(t, m)
	})
	t.Run("two cntlrs on one CN charge it twice in one write",
		func(t *testing.T) {
			m, conf := fixture(0, cnA, cnB, cnA)
			charged, err := chargeCloneCns(m, cid, conf, units)
			if err != nil {
				t.Fatalf("chargeCloneCns: %v", err)
			}
			if got := used(t, m, cnA); got != 2*units {
				t.Errorf("cnA used %d, want %d", got, 2*units)
			}
			if got := used(t, m, cnB); got != units {
				t.Errorf("cnB used %d, want %d", got, units)
			}
			if charged[cnA] != 2*units {
				t.Errorf("charged %v, want %d on %s", charged, 2*units, cnA)
			}
			onlyNodeWrites(t, m)
		})
	t.Run("no cntlr charges nothing", func(t *testing.T) {
		m, conf := fixture(0)
		charged, err := chargeCloneCns(m, cid, conf, units)
		if err != nil || len(charged) != 0 || len(m.puts) != 0 {
			t.Errorf("charged %v, err %v, writes %v; want none",
				charged, err, m.puts)
		}
	})
	t.Run("zero units read nothing", func(t *testing.T) {
		m, conf := fixture(0, cnA, cnB)
		charged, err := chargeCloneCns(m, cid, conf, 0)
		if err != nil || len(charged) != 0 {
			t.Errorf("charged %v, err %v; want nothing", charged, err)
		}
		if len(m.gets) != 0 || len(m.puts) != 0 {
			t.Errorf("zero units read %v and wrote %v", m.gets, m.puts)
		}
	})
	t.Run("a CN past its ceiling is charged all the same",
		func(t *testing.T) {
			m, conf := fixture(common.CnCloneMetaUnitCnt-1, cnA)
			if _, err := chargeCloneCns(m, cid, conf, units); err != nil {
				t.Fatalf("chargeCloneCns refused a full CN: %v", err)
			}
			want := uint64(common.CnCloneMetaUnitCnt - 1 + units)
			if got := used(t, m, cnA); got != want {
				t.Errorf("cnA used %d, want %d", got, want)
			}
			cn := &pb.CnConf{}
			m.peek(model.CnConfKey(cid, cnA), cn)
			if free := model.CnFreeMetaUnits(cn); free != 0 {
				t.Errorf("cnA past its ceiling has %d free units, want 0",
					free)
			}
		})
	t.Run("a listed cntlr with no key refuses before any write",
		func(t *testing.T) {
			m, conf := fixture(0, cnA, "", cnB)
			_, err := chargeCloneCns(m, cid, conf, units)
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("chargeCloneCns = %v, want a \"not found\" "+
					"refusal", err)
			}
			if len(m.puts) != 0 {
				t.Errorf("the refusal wrote %v", m.puts)
			}
		})
	t.Run("a CN that is gone is skipped", func(t *testing.T) {
		m, conf := fixture(0, cnA, cnGone)
		charged, err := chargeCloneCns(m, cid, conf, units)
		if err != nil {
			t.Fatalf("chargeCloneCns: %v", err)
		}
		if _, ok := charged[cnGone]; ok {
			t.Errorf("charged %v names the CN that is gone", charged)
		}
		if m.puts[model.CnConfKey(cid, cnGone)] != 0 {
			t.Errorf("the charge wrote the CnConf of the CN that is gone")
		}
		if got := used(t, m, cnA); got != units {
			t.Errorf("cnA used %d, want %d", got, units)
		}
		onlyNodeWrites(t, m)
	})
}

// TestMigrDstCharge pins put-migr's charge of its destination DN
// (architecture.md, Metadata units): the side pointer, the extents and the
// migration's units in the one write of the record, the capacity value
// following the record, one DnRev bump, no refusal past the DN's metadata
// area, and a DN short of extents refused before any write — the charge
// FinishMigration and CancelMigration give back.
func TestMigrDstCharge(t *testing.T) {
	const (
		cid    = uint64(0xc1d)
		spId   = uint64(7)
		dnId   = uint64(0x21)
		shard  = uint32(3)
		addr   = "10.0.0.2:29600"
		total  = uint64(100)
		free   = uint64(64)
		extCnt = uint64(2)
		units  = uint64(3)
		rev    = uint64(41)
	)
	cc := &pb.ClusterConf{DnBinConf: model.ResolveDnBinConf(nil)}
	ptr := &pb.SidePointer{SpId: spId, LegId: 0x31, SideId: 0x32}
	held := &pb.SidePointer{SpId: spId, LegId: 0x11, SideId: 0x12}
	capKey := func(t *testing.T, freeExt uint64) string {
		t.Helper()
		binIdx, ok := model.DnBinIdx(freeExt, cc.GetDnBinConf())
		if !ok {
			t.Fatalf("%d free extents have no bin", freeExt)
		}
		return model.DnCapacityKey(cid, binIdx, freeExt, addr)
	}
	// fixture plants the DN with freeExt free extents, used units already
	// used and one side it already holds, its capacity key as
	// model.MaintainDnCapacity writes it, and its DnRev at rev; it returns
	// the record as an STM reads it.
	fixture := func(t *testing.T, freeExt, used uint64) (*memStm, *pb.DnConf) {
		t.Helper()
		m := newMemStm()
		dn := &pb.DnConf{
			DnId: dnId, ShardCode: shard, Location: "rack2",
			TotalExtCnt: total, FreeExtCnt: freeExt, UsedMetaUnits: used,
			SidePtrList: []*pb.SidePointer{held},
		}
		m.seed(model.DnConfKey(cid, addr), dn)
		m.seed(capKey(t, freeExt), model.DnCapacityValue(dn))
		m.seed(model.DnRevKey(shard, cid, dnId), &pb.DnRev{Revision: rev})
		read := &pb.DnConf{}
		if !m.Get(model.DnConfKey(cid, addr), read) {
			t.Fatalf("no DnConf at %q", addr)
		}
		return m, read
	}
	// charged checks one successful charge: the record and its capacity
	// value carry the extents and the units, the key moved with the free
	// count, the revision went up once, and nothing else was written.
	charged := func(t *testing.T, m *memStm, dnRev, used uint64) {
		t.Helper()
		dn := &pb.DnConf{}
		if !m.peek(model.DnConfKey(cid, addr), dn) {
			t.Fatalf("the charge removed the DnConf")
		}
		if dn.GetFreeExtCnt() != free-extCnt {
			t.Errorf("free_ext_cnt %d, want %d", dn.GetFreeExtCnt(),
				free-extCnt)
		}
		if dn.GetUsedMetaUnits() != used+units {
			t.Errorf("used_meta_units %d, want %d", dn.GetUsedMetaUnits(),
				used+units)
		}
		ptrs := dn.GetSidePtrList()
		if len(ptrs) != 2 || !proto.Equal(ptrs[0], held) ||
			!proto.Equal(ptrs[1], ptr) {
			t.Errorf("side_ptr_list %v, want %v then %v", ptrs, held, ptr)
		}
		if m.peek(capKey(t, free), &pb.DnCapacity{}) {
			t.Errorf("the capacity key of %d free extents is still there",
				free)
		}
		value := &pb.DnCapacity{}
		if !m.peek(capKey(t, free-extCnt), value) {
			t.Fatalf("no capacity key for %d free extents", free-extCnt)
		}
		if !proto.Equal(value, model.DnCapacityValue(dn)) {
			t.Errorf("capacity value %v, the record implies %v", value,
				model.DnCapacityValue(dn))
		}
		got := &pb.DnRev{}
		m.peek(model.DnRevKey(shard, cid, dnId), got)
		if got.GetRevision() != rev+1 || dnRev != rev+1 {
			t.Errorf("DnRev %d, returned %d; want %d once",
				got.GetRevision(), dnRev, rev+1)
		}
		for key, cnt := range m.puts {
			if key != model.DnConfKey(cid, addr) &&
				key != capKey(t, free-extCnt) &&
				key != model.DnRevKey(shard, cid, dnId) {
				t.Errorf("the charge wrote %q", key)
			}
			if cnt != 1 {
				t.Errorf("the charge wrote %q %d times, want once", key, cnt)
			}
		}
	}

	t.Run("the units ride the extents' write", func(t *testing.T) {
		m, dn := fixture(t, free, 10)
		dnRev, err := chargeMigrDst(m, cid, cc, addr, dn, ptr, extCnt, units)
		if err != nil {
			t.Fatalf("chargeMigrDst: %v", err)
		}
		charged(t, m, dnRev, 10)
	})
	t.Run("a DN past its metadata area is charged all the same",
		func(t *testing.T) {
			used := uint64(common.DnCloneMetaUnitCnt - 1)
			m, dn := fixture(t, free, used)
			dnRev, err := chargeMigrDst(
				m, cid, cc, addr, dn, ptr, extCnt, units)
			if err != nil {
				t.Fatalf("chargeMigrDst refused a full DN: %v", err)
			}
			charged(t, m, dnRev, used)
			after := &pb.DnConf{}
			m.peek(model.DnConfKey(cid, addr), after)
			if room := model.DnFreeMetaUnits(after); room != 0 {
				t.Errorf("a DN past its area has %d free units, want 0",
					room)
			}
		})
	t.Run("a DN short of extents refuses before any write",
		func(t *testing.T) {
			m, dn := fixture(t, extCnt-1, 0)
			_, err := chargeMigrDst(m, cid, cc, addr, dn, ptr, extCnt, units)
			if err == nil || !strings.Contains(err.Error(), "free_ext_cnt") {
				t.Fatalf("chargeMigrDst = %v, want a free_ext_cnt refusal",
					err)
			}
			if len(m.puts) != 0 {
				t.Errorf("the refusal wrote %v", m.puts)
			}
		})
}

// TestPlantedCloneTd pins how put-clone's --dst-td finds the td it sizes the
// clone by: a name first, then a td_id among the pool's tds, and a td_id no
// td carries planted unsized.
func TestPlantedCloneTd(t *testing.T) {
	const cid, spId = uint64(0xc1d), uint64(7)
	m := newMemStm()
	conf := &pb.SpConf{SpId: spId, TdNameList: []string{"td0", "5"}}
	m.seed(model.ThinDeviceKey(cid, spId, "td0"),
		&pb.ThinDevice{TdId: 0x10, Size: 10 << 30})
	// A td NAMED like an id: the name is tried first, so "5" is this td and
	// never td_id 5.
	m.seed(model.ThinDeviceKey(cid, spId, "5"),
		&pb.ThinDevice{TdId: 0x11, Size: 1 << 30})
	cases := []struct {
		dstTd string
		id    uint64
		size  uint64
		found bool
	}{
		{"td0", 0x10, 10 << 30, true},
		{"0x10", 0x10, 10 << 30, true},
		{"16", 0x10, 10 << 30, true},
		{"5", 0x11, 1 << 30, true},
		{"0x99", 0x99, 0, false},
	}
	for _, tc := range cases {
		id, td, err := plantedCloneTd(m, cid, conf, tc.dstTd)
		if err != nil {
			t.Errorf("plantedCloneTd(%q): %v", tc.dstTd, err)
			continue
		}
		if id != tc.id || (td != nil) != tc.found ||
			td.GetSize() != tc.size {
			t.Errorf("plantedCloneTd(%q) = %#x, %v; want %#x, size %d, "+
				"found %t", tc.dstTd, id, td, tc.id, tc.size, tc.found)
		}
	}
	if _, _, err := plantedCloneTd(m, cid, conf, "nope"); err == nil {
		t.Errorf("plantedCloneTd(\"nope\") succeeded, want an error")
	}
}

// TestPlantedUnits pins the units put-clone and put-migr store and charge
// (architecture.md, Metadata units): the formula at the pool's block size,
// behind the stored conf's gate, and none for a clone with no td to size.
func TestPlantedUnits(t *testing.T) {
	small := &pb.SpConf{BdevConf: testPoolBdev(64 << 10)}
	large := &pb.SpConf{BdevConf: testPoolBdev(1 << 20)}
	broken := &pb.SpConf{BdevConf: testPoolBdev(0)}

	cloneCases := []struct {
		name  string
		conf  *pb.SpConf
		td    *pb.ThinDevice
		units uint64
	}{
		{"the worker suite's drain-case clone", large,
			&pb.ThinDevice{Size: 10 << 30}, 2},
		{"the cn agent's example", small,
			&pb.ThinDevice{Size: 1 << 40}, 5},
		{"no td to size", small, nil, 0},
		{"no td to size, whatever the conf", broken, nil, 0},
	}
	for _, tc := range cloneCases {
		units, err := plantedCloneUnits(tc.conf, tc.td)
		if err != nil || units != tc.units {
			t.Errorf("plantedCloneUnits, %s = %d, %v; want %d",
				tc.name, units, err, tc.units)
		}
	}
	if units, err := plantedCloneUnits(
		broken, &pb.ThinDevice{Size: 10 << 30}); err == nil {
		t.Errorf("plantedCloneUnits on a zero block size = %d, want a "+
			"refusal", units)
	}

	migrCases := []struct {
		name       string
		conf       *pb.SpConf
		extCnt     uint64
		extentSize uint64
		units      uint64
	}{
		{"the worker suite's bitmap-case migration", large,
			2, common.MinDnExtSize, 2},
		{"the gateway suite's smoke migration", large,
			2, 1 << 30, 2},
		{"the ceiling case's migration that fills a DN", small,
			12032, 1 << 30, common.DnCloneMetaUnitCnt},
	}
	for _, tc := range migrCases {
		units, err := plantedMigrUnits(tc.conf, tc.extCnt, tc.extentSize)
		if err != nil || units != tc.units {
			t.Errorf("plantedMigrUnits, %s = %d, %v; want %d",
				tc.name, units, err, tc.units)
		}
	}
	if units, err := plantedMigrUnits(broken, 2, 1<<30); err == nil {
		t.Errorf("plantedMigrUnits on a zero block size = %d, want a "+
			"refusal", units)
	}
}

func TestNvmeTrConfOf(t *testing.T) {
	conf := nvmeTrConfOf("192.168.10.20:29600")
	if conf.GetTrAddr() != "192.168.10.20" {
		t.Fatalf("tr_addr = %q", conf.GetTrAddr())
	}
	// The node's own port, so two nodes on one host get distinct
	// NvmeTrConfs and a CdcEntry's transport list stays attributable
	// (worker_test.sh case D step 3).
	if conf.GetTrSvcId() != "29600" {
		t.Fatalf("tr_svc_id = %q, want %q", conf.GetTrSvcId(), "29600")
	}
	if other := nvmeTrConfOf("192.168.10.20:29700"); other.GetTrSvcId() ==
		conf.GetTrSvcId() {
		t.Fatalf("two nodes on one host share an NvmeTrConf")
	}
	// An address with no port still yields a usable default.
	if bare := nvmeTrConfOf("192.168.10.20"); bare.GetTrSvcId() != nvmeTrSvcId {
		t.Fatalf("bare addr tr_svc_id = %q", bare.GetTrSvcId())
	}
	if conf.GetTrType() != "tcp" || conf.GetAdrFam() != "ipv4" {
		t.Fatalf("tr_type/adr_fam = %q/%q",
			conf.GetTrType(), conf.GetAdrFam())
	}
}

func TestNsIdentityIsDeterministic(t *testing.T) {
	uuid, nguid := nsIdentity(1, 2, 3)
	again, againNguid := nsIdentity(1, 2, 3)
	if uuid != again || nguid != againNguid {
		t.Fatalf("nsIdentity is not deterministic")
	}
	other, _ := nsIdentity(1, 2, 4)
	if uuid == other {
		t.Fatalf("two namespaces share one uuid")
	}
	if len(nguid) != 32 {
		t.Fatalf("nguid %q is not 16 bytes of hex", nguid)
	}
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 {
		t.Fatalf("uuid %q is not RFC 4122 shaped", uuid)
	}
	if uuid[14] != '4' {
		t.Fatalf("uuid %q is not version 4", uuid)
	}
}

// smokePlan is worker_test.sh's case S placement: two cntlrs, one slice, a
// raid1 meta group of 1 extent and a raid1 data group of 2, four sides on two
// DNs.
func smokePlan() (cntlrs, slices, groups, legs, sides []string) {
	return []string{"1:1:0:true", "2:2:1:false"},
		[]string{"1:0"},
		[]string{"1:2:meta:1:raid1", "1:5:data:2:raid1"},
		[]string{"2:3:0", "2:4:1", "5:6:0", "5:7:1"},
		[]string{"3:8:1:0", "4:9:2:0", "6:10:1:0", "7:11:2:0"}
}

func TestBuildSpPlan(t *testing.T) {
	cntlrs, slices, groups, legs, sides := smokePlan()
	plan, err := buildSpPlan(
		cntlrs, slices, groups, legs, sides, []uint32{0, 1, 2},
	)
	if err != nil {
		t.Fatalf("buildSpPlan failed: %v", err)
	}
	if !plan.raid1 {
		t.Fatalf("plan.raid1 = false")
	}
	// Σ ext_cnt over ALL groups is what one cntlr's CN reserves
	// (architecture.md, Storage pools).
	if plan.footprint != 3 {
		t.Fatalf("footprint = %d, want 3", plan.footprint)
	}
	// next_id must be past every id the script assigned.
	if plan.nextId != 12 {
		t.Fatalf("next_id = %d, want 12", plan.nextId)
	}
	if plan.grpOfLeg[6] != 5 || plan.grpOfLeg[3] != 2 {
		t.Fatalf("grpOfLeg = %v", plan.grpOfLeg)
	}
	// A side of a data-group leg costs the data group's 2 extents.
	if plan.extOfLeg[6] != 2 || plan.extOfLeg[3] != 1 {
		t.Fatalf("extOfLeg = %v", plan.extOfLeg)
	}
	if len(plan.sides) != 4 || len(plan.legs) != 4 {
		t.Fatalf("plan has %d sides / %d legs", len(plan.sides), len(plan.legs))
	}
}

func TestBuildSpPlanRejectsBadWiring(t *testing.T) {
	slots := []uint32{0, 1, 2}
	cases := []struct {
		name   string
		mutate func(c, s, g, l, si *[]string)
		want   string
	}{
		{
			name: "dangling group slice",
			mutate: func(c, s, g, l, si *[]string) {
				(*g)[0] = "9:2:meta:1:raid1"
			},
			want: "no --slice with id",
		},
		{
			name: "dangling leg group",
			mutate: func(c, s, g, l, si *[]string) {
				(*l)[0] = "9:3:0"
			},
			want: "no --group with id",
		},
		{
			name: "dangling side leg",
			mutate: func(c, s, g, l, si *[]string) {
				(*si)[0] = "9:8:1:0"
			},
			want: "no --leg with id",
		},
		{
			name: "duplicate side id",
			mutate: func(c, s, g, l, si *[]string) {
				(*si)[1] = "4:8:2:0"
			},
			want: "side id 0x8 is used twice",
		},
		{
			name: "duplicate leg id",
			mutate: func(c, s, g, l, si *[]string) {
				(*l)[1] = "2:3:1"
			},
			want: "leg id 0x3 is used twice",
		},
		{
			name: "duplicate cntlr id",
			mutate: func(c, s, g, l, si *[]string) {
				(*c)[1] = "1:2:1:false"
			},
			want: "cntlr id 0x1 is used twice",
		},
		{
			name: "id zero",
			mutate: func(c, s, g, l, si *[]string) {
				(*s)[0] = "0:0"
			},
			want: "never valid",
		},
		{
			name: "two primaries",
			mutate: func(c, s, g, l, si *[]string) {
				(*c)[1] = "2:2:1:true"
			},
			want: "exactly one --cntlr must be primary",
		},
		{
			name: "no primary",
			mutate: func(c, s, g, l, si *[]string) {
				(*c)[0] = "1:1:0:false"
			},
			want: "exactly one --cntlr must be primary",
		},
		{
			name: "shared cntlid slot",
			mutate: func(c, s, g, l, si *[]string) {
				(*c)[1] = "2:2:0:false"
			},
			want: "already taken by cntlr",
		},
		{
			name: "slot outside --slots",
			mutate: func(c, s, g, l, si *[]string) {
				(*c)[1] = "2:2:7:false"
			},
			want: "is not in --slots",
		},
		{
			name: "two cntlrs on one cn",
			mutate: func(c, s, g, l, si *[]string) {
				(*c)[1] = "2:1:1:false"
			},
			want: "already hosts a cntlr of this sp",
		},
		{
			name: "mixed redundancy",
			mutate: func(c, s, g, l, si *[]string) {
				(*g)[1] = "1:5:data:2:none"
			},
			want: "same redundancy",
		},
		{
			name: "two legs of one group on one dn",
			mutate: func(c, s, g, l, si *[]string) {
				(*si)[1] = "4:9:1:0"
			},
			want: "already carries side",
		},
		{
			name: "duplicate leg_idx",
			mutate: func(c, s, g, l, si *[]string) {
				(*l)[1] = "2:4:0"
			},
			want: "already used by leg",
		},
		{
			name: "duplicate slice_idx",
			mutate: func(c, s, g, l, si *[]string) {
				*s = append(*s, "12:0")
			},
			want: "already used by slice",
		},
		{
			name: "group with no leg",
			mutate: func(c, s, g, l, si *[]string) {
				*g = append(*g, "1:20:data:2:raid1")
			},
			want: "has no --leg",
		},
		{
			name: "leg with no side",
			mutate: func(c, s, g, l, si *[]string) {
				*l = append(*l, "2:20:2")
			},
			want: "has no --side",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s, g, l, si := smokePlan()
			tc.mutate(&c, &s, &g, &l, &si)
			_, err := buildSpPlan(c, s, g, l, si, slots)
			if err == nil {
				t.Fatalf("buildSpPlan succeeded, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestBuildSpPlanRequiresEveryPart(t *testing.T) {
	c, s, g, l, si := smokePlan()
	slots := []uint32{0, 1, 2}
	if _, err := buildSpPlan(nil, s, g, l, si, slots); err == nil {
		t.Errorf("a plan with no --cntlr was accepted")
	}
	if _, err := buildSpPlan(c, nil, g, l, si, slots); err == nil {
		t.Errorf("a plan with no --slice was accepted")
	}
	if _, err := buildSpPlan(c, s, nil, l, si, slots); err == nil {
		t.Errorf("a plan with no --group was accepted")
	}
	// Fewer slots than cntlrs cannot give every cntlr a distinct one
	// (architecture.md, cntlid slots).
	if _, err := buildSpPlan(c, s, g, l, si, []uint32{0}); err == nil {
		t.Errorf("a plan with fewer slots than cntlrs was accepted")
	}
}

func TestContainsNameAndAdvanceNextId(t *testing.T) {
	if !containsName([]string{"a", "b"}, "b") {
		t.Fatalf("containsName missed an entry")
	}
	if containsName([]string{"a", "b"}, "c") {
		t.Fatalf("containsName invented an entry")
	}
	conf := &pb.SpConf{NextId: 5}
	advanceNextId(conf, 3)
	if conf.NextId != 5 {
		t.Fatalf("next_id went backwards to %d", conf.NextId)
	}
	advanceNextId(conf, 5, 9)
	if conf.NextId != 10 {
		t.Fatalf("next_id = %d, want 10", conf.NextId)
	}
}

// TestShardFlag pins the one place where a decimal reading would be silently
// wrong: worker_test.sh's case E spreads DNs over the shard codes "00", "55",
// "aa" and "ff", which are the common.ShardCodeFmt spelling and therefore hex.
func TestShardFlag(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
	}{
		{"00", 0},
		{"0", 0},
		{"55", 0x55},
		{"aa", 0xaa},
		{"AA", 0xaa},
		{"ff", 0xff},
		{"0x55", 0x55},
	}
	for _, tc := range cases {
		var shard shardFlag
		if err := shard.Set(tc.in); err != nil {
			t.Errorf("Set(%q) failed: %v", tc.in, err)
			continue
		}
		if uint32(shard) != tc.want {
			t.Errorf("Set(%q) = %#x, want %#x", tc.in, uint32(shard), tc.want)
		}
	}
	var shard shardFlag
	if err := shard.Set("ff"); err != nil {
		t.Fatalf("Set(ff) failed: %v", err)
	}
	if got := shard.String(); got != "ff" {
		t.Fatalf("String() = %q, want ff", got)
	}
	// 256 and up have no two-digit %02x spelling and no bucket slot
	// (architecture.md, Globals: id allocation + shard buckets).
	for _, bad := range []string{"", "100", "-1", "zz", "0x100"} {
		var shard shardFlag
		if err := shard.Set(bad); err == nil {
			t.Errorf("Set(%q) succeeded, want an error", bad)
		}
	}
}

// TestPlantedEpoch pins set-epoch's value: --age seconds before now, or 0
// for --clear, exactly one of them, and never an age that would land on 0,
// which reads as healthy rather than as an epoch planted long ago.
func TestPlantedEpoch(t *testing.T) {
	const now = 1_000_000
	cases := []struct {
		age        uint64
		ageGiven   bool
		clearEpoch bool
		want       uint64
		ok         bool
	}{
		{30, true, false, now - 30, true},
		{0, true, false, now, true},
		{now - 1, true, false, 1, true},
		{0, false, true, 0, true},
		{now, true, false, 0, false},
		{now + 1, true, false, 0, false},
		{30, true, true, 0, false},
		{0, false, false, 0, false},
	}
	for _, tc := range cases {
		got, err := plantedEpoch(now, tc.age, tc.ageGiven, tc.clearEpoch)
		if !tc.ok {
			if err == nil {
				t.Errorf("plantedEpoch(age %d given %v clear %v) = %d, "+
					"want an error", tc.age, tc.ageGiven, tc.clearEpoch, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("plantedEpoch(age %d given %v clear %v) = %d, %v; "+
				"want %d", tc.age, tc.ageGiven, tc.clearEpoch, got, err,
				tc.want)
		}
	}
}

// TestSetSliceEpoch pins where set-epoch finds a leg or a side: in either
// group list and on an active or a spare leg, as the health ops do, touching
// that one object and nothing else.
func TestSetSliceEpoch(t *testing.T) {
	newSlice := func() *pb.Slice {
		return &pb.Slice{
			MetaGrpList: []*pb.Group{{
				GrpId: 1,
				LegList: []*pb.Leg{{
					LegId: 1, SideList: []*pb.Side{{SideId: 11}},
				}},
				SpareLegList: []*pb.Leg{{
					LegId: 2, ErrEpoch: 7,
					SideList: []*pb.Side{{SideId: 12, ErrEpoch: 8}},
				}},
			}},
			DataGrpList: []*pb.Group{{
				GrpId: 2,
				LegList: []*pb.Leg{{
					LegId: 3, SideList: []*pb.Side{{SideId: 13}},
				}},
			}},
		}
	}
	cases := []struct {
		kind  string
		id    uint64
		old   uint64
		found bool
		// read returns the epoch the case set, from a fresh walk.
		read func(*pb.Slice) uint64
	}{
		{"leg", 2, 7, true, func(s *pb.Slice) uint64 {
			return s.GetMetaGrpList()[0].GetSpareLegList()[0].GetErrEpoch()
		}},
		{"leg", 3, 0, true, func(s *pb.Slice) uint64 {
			return s.GetDataGrpList()[0].GetLegList()[0].GetErrEpoch()
		}},
		{"side", 12, 8, true, func(s *pb.Slice) uint64 {
			return s.GetMetaGrpList()[0].GetSpareLegList()[0].
				GetSideList()[0].GetErrEpoch()
		}},
		{"side", 13, 0, true, func(s *pb.Slice) uint64 {
			return s.GetDataGrpList()[0].GetLegList()[0].
				GetSideList()[0].GetErrEpoch()
		}},
		{"leg", 11, 0, false, nil},
		{"side", 3, 0, false, nil},
		{"cntlr", 1, 0, false, nil},
	}
	for _, tc := range cases {
		slice := newSlice()
		old, found := setSliceEpoch(slice, tc.kind, tc.id, 42)
		if found != tc.found || old != tc.old {
			t.Errorf("setSliceEpoch(%s %d) = %d, %v; want %d, %v",
				tc.kind, tc.id, old, found, tc.old, tc.found)
			continue
		}
		if !found {
			if !proto.Equal(slice, newSlice()) {
				t.Errorf("setSliceEpoch(%s %d) found nothing and still "+
					"changed the slice: %v", tc.kind, tc.id, slice)
			}
			continue
		}
		if got := tc.read(slice); got != 42 {
			t.Errorf("setSliceEpoch(%s %d) left %d, want 42",
				tc.kind, tc.id, got)
		}
		// Undo the one write: what remains must be the fixture.
		setSliceEpoch(slice, tc.kind, tc.id, tc.old)
		if !proto.Equal(slice, newSlice()) {
			t.Errorf("setSliceEpoch(%s %d) changed more than its object: %v",
				tc.kind, tc.id, slice)
		}
	}
}
