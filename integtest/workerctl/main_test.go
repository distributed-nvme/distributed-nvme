// Tests for the parsing surface of workerctl (dnv-worker.md,
// Integration test plan, The driver). The driver writes straight into the etcd
// the whole suite reads back, so a malformed test invocation must fail loudly
// here rather than land garbage in a key: these cover every tuple form of its
// flags and the typed spec of the planting library each one becomes, the 0x
// hex ids, the bitmap payload, the key kind → message mapping of `get`
// (architecture.md, Key table), the three printers that touch no etcd,
// set-epoch's value, and set-repair-stage's stage and create-repair's look-up
// of its group and its plan. What the library does with a spec is the
// library's own tests' (integtest/plant), and what a model operation does
// with its arguments the model's.
package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
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
	// The library's form: nil when not given, the value when given.
	var unset triBool
	if unset.ptr() != nil {
		t.Fatalf("ptr() of an unset triBool is not nil")
	}
	if err := flag.Set("false"); err != nil {
		t.Fatalf("Set(false) failed: %v", err)
	}
	if got := flag.ptr(); got == nil || *got {
		t.Fatalf("ptr() after Set(false) = %v, want a pointer to false", got)
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
	want := plant.CntlrSpec{CntlrId: 1, CnId: 2, Slot: 3, Primary: true}
	if got != want {
		t.Fatalf("parseCntlrSpec = %+v, want %+v", got, want)
	}
	got, err = parseCntlrSpec("2:2:1:0")
	if err != nil {
		t.Fatalf("parseCntlrSpec failed: %v", err)
	}
	if got.Primary {
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
	if got != (plant.SliceSpec{SliceId: 3, SliceIdx: 0}) {
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
	want := plant.GroupSpec{SliceId: 1, GrpId: 2, Meta: true, ExtCnt: 1, Raid1: true}
	if got != want {
		t.Fatalf("parseGroupSpec = %+v, want %+v", got, want)
	}
	got, err = parseGroupSpec("1:0x4:data:2:none")
	if err != nil {
		t.Fatalf("parseGroupSpec failed: %v", err)
	}
	want = plant.GroupSpec{SliceId: 1, GrpId: 4, Meta: false, ExtCnt: 2, Raid1: false}
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
	if got != (plant.LegSpec{GrpId: 2, LegId: 3, LegIdx: 1}) {
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
	if got != (plant.SideSpec{LegId: 3, SideId: 4, DnId: 1, Slot: 0}) {
		t.Fatalf("parseSideSpec = %+v", got)
	}
	for _, bad := range []string{"", "3:4:1", "3:4:1:0:9", "3:4:1:x"} {
		if _, err := parseSideSpec(bad); err == nil {
			t.Errorf("parseSideSpec(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseCloneLegSpec(t *testing.T) {
	got, err := parseCloneLegSpec("0x31:1")
	if err != nil {
		t.Fatalf("parseCloneLegSpec failed: %v", err)
	}
	if got != (plant.CloneLegSpec{LegId: 0x31, LegIdx: 1}) {
		t.Fatalf("parseCloneLegSpec = %+v", got)
	}
	for _, bad := range []string{"", "31", "31:1:0", "31:x", "31:0x100000000"} {
		if _, err := parseCloneLegSpec(bad); err == nil {
			t.Errorf("parseCloneLegSpec(%q) succeeded, want an error", bad)
		}
	}
}

// TestCloneLegsOf pins how put-clone joins its --leg and --side flags: each
// leg takes the one side that names it, in the order of the --leg flags, and
// every unmatched, doubled or zero id is refused.
func TestCloneLegsOf(t *testing.T) {
	got, err := cloneLegsOf(
		[]string{"0x32:0", "0x34:1"},
		[]string{"0x34:0x35:2:1", "0x32:0x33:1:0"},
	)
	if err != nil {
		t.Fatalf("cloneLegsOf failed: %v", err)
	}
	want := []plant.CloneLegSpec{
		{LegId: 0x32, LegIdx: 0, SideId: 0x33, DnId: 1, Slot: 0},
		{LegId: 0x34, LegIdx: 1, SideId: 0x35, DnId: 2, Slot: 1},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cloneLegsOf = %+v, want %+v", got, want)
	}
	bad := []struct {
		legs, sides []string
		want        string
	}{
		{[]string{"0x32:0"}, nil, "has no --side"},
		{[]string{"0x32:0"}, []string{"0x99:0x33:1:0"}, "no --leg names"},
		{[]string{"0x32:0"},
			[]string{"0x32:0x33:1:0", "0x32:0x35:2:0"}, "already has a side"},
		{[]string{"0x32:0", "0x32:1"}, nil, "named twice"},
		{[]string{"0:0"}, nil, "leg id 0"},
		{[]string{"0x32:0"}, []string{"0x32:0:1:0"}, "side id 0"},
		{[]string{"0x32"}, nil, "--leg"},
		{[]string{"0x32:0"}, []string{"0x32:0x33:1"}, "--side"},
	}
	for _, tc := range bad {
		_, err := cloneLegsOf(tc.legs, tc.sides)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("cloneLegsOf(%q, %q) = %v, want an error naming %q",
				tc.legs, tc.sides, err, tc.want)
		}
	}
}

// TestEpochHolder pins where set-epoch looks for its object: a cntlr needs
// neither --slice nor --clone, a leg or a side exactly one of them.
func TestEpochHolder(t *testing.T) {
	cases := []struct {
		kind  string
		slice uint64
		clone string
		ok    bool
	}{
		{"cntlr", 0, "", true},
		{"cntlr", 1, "", false},
		{"cntlr", 0, "c0", false},
		{"leg", 1, "", true},
		{"leg", 0, "c0", true},
		{"leg", 0, "", false},
		{"side", 1, "c0", false},
		{"side", 0, "c0", true},
	}
	for _, tc := range cases {
		err := epochHolder(tc.kind, tc.slice, tc.clone)
		if (err == nil) != tc.ok {
			t.Errorf("epochHolder(%s, %d, %q) = %v, want ok %t",
				tc.kind, tc.slice, tc.clone, err, tc.ok)
		}
	}
}

func TestParseMigrSpec(t *testing.T) {
	got, err := parseMigrSpec("m0:3:1:0x9")
	if err != nil {
		t.Fatalf("parseMigrSpec failed: %v", err)
	}
	want := plant.MigrSpec{Name: "m0", Id: 3, SrcSide: 1, DstSide: 9}
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
	if got != (plant.NsSpec{NsId: 5, NsIdx: 1, TdId: 7}) {
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
		{"dnv repair ebada5168620c5fe 0000000000000001 r0", &pb.Repair{}},
		{
			"dnv retired_leg ebada5168620c5fe 0000000000000001 " +
				"0000000000000002",
			&pb.RetiredLeg{},
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
		"cn_capacity", "sp_conf", "cntlr", "slice", "cdc", "repair",
		"retired_leg"} {
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
// the others, for computed values). It prints sixteen keys, and the suites
// read fourteen of them and stop when one is missing: thirteen at preflight,
// and RetiredLegMaxAge, which the worker suite's repair case reads.
// Every suite that starts an etcd — the worker, gateway, cdc and e2e suites —
// launches it with EtcdMaxTxnOps and EtcdMaxRequestBytes. The e2e suite
// cross-checks MaxAllocLegPerGrp. The gateway suite creates its stage-13
// clone at MaxSliceCntPerSp, checks the health intervals a new cluster stores
// against DefaultHealthCheckInterval, fills a DN's metadata area in its
// ceiling case to DnCloneMetaUnitCnt in units of DnCloneMetaUnit, and checks
// the counters the id step moves against IdStepStride and ThinIdStepStride. The
// worker and e2e suites read DefaultHealthCheckInterval,
// DefaultPrimaryUnhealthy and DemotionHoldTimeout for their failover cases.
// The cdc suite's flood case fills the pending cap, CdcMaxPendingConnCnt, and
// waits by the pre-Connect deadline, DefaultCdcPendingTmoMs. One of those
// fourteen dropped or renamed here would otherwise surface only when a suite
// is run, at its preflight or in that case. No suite reads the other two,
// MaxDelGrpPerTxn and MaxDelBmPerTxn; they are pinned as well, because a case
// may size itself against them.
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
		"MaxDelBmPerTxn":    common.MaxDelBmPerTxn,
		"MaxSliceCntPerSp":  common.MaxSliceCntPerSp,

		"DefaultHealthCheckInterval": common.DefaultHealthCheckInterval,
		"DefaultPrimaryUnhealthy":    common.DefaultPrimaryUnhealthy,
		"DemotionHoldTimeout":        common.DemotionHoldTimeout,
		"RetiredLegMaxAge":           common.RetiredLegMaxAge,

		"DnCloneMetaUnitCnt": common.DnCloneMetaUnitCnt,
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
// suites read the units a migration costs (architecture.md, Metadata units)
// instead of re-deriving the formula: the dn agent's own example, a copy
// under one block, and every migration the gateway suite's ceiling case and
// its smoke case size by it. --block-size defaults to
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
		{"the dn agent's example",
			[]string{"--bytes", "1099511627776", "--block-size", "1048576"},
			tib, 1 << 20, 2},
		{"a copy under one block",
			[]string{"--bytes", "65536", "--block-size", "1048576"},
			64 << 10, 1 << 20, 1},
		{"the ceiling case's migration that fills a DN",
			[]string{"--bytes", "12919261626368", "--block-size", "65536"},
			12919261626368, 64 << 10, 48},
		{"the ceiling case's probe migration of one extent",
			[]string{"--bytes", "1073741824", "--block-size", "65536"},
			1 << 30, 64 << 10, 2},
		{"the smoke case's migration of two extents",
			[]string{"--bytes", "2147483648", "--block-size", "1048576"},
			2 << 30, 1 << 20, 2},
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

// TestParseRepStage pins set-repair-stage's --stage: the two stages a stage
// move reaches (dnv-worker.md AR11), by either spelling, and nothing else —
// CREATED is no move.
func TestParseRepStage(t *testing.T) {
	cases := []struct {
		in   string
		want pb.RepStage
	}{
		{"started", pb.RepStage_REP_STAGE_STARTED},
		{"ABORTED", pb.RepStage_REP_STAGE_ABORTED},
		{" REP_STAGE_STARTED ", pb.RepStage_REP_STAGE_STARTED},
		{"rep_stage_aborted", pb.RepStage_REP_STAGE_ABORTED},
	}
	for _, tc := range cases {
		got, err := parseRepStage(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseRepStage(%q) = %v, %v; want %v", tc.in, got, err,
				tc.want)
		}
	}
	for _, bad := range []string{"", "created", "REP_STAGE_CREATED", "1"} {
		if _, err := parseRepStage(bad); err == nil {
			t.Errorf("parseRepStage(%q) succeeded, want an error", bad)
		}
	}
}

// TestFindRepairGrp pins create-repair's look-up of its group: the slices
// first, either list, then the clones' clone meta groups (architecture.md,
// Repairs), and the plan of a fresh new leg, the disk node of every side of
// the group's legs.
func TestFindRepairGrp(t *testing.T) {
	side := func(addr string) []*pb.Side {
		return []*pb.Side{{AddrPort: addr}}
	}
	cloneGrp := &pb.Group{GrpId: 30, LegList: []*pb.Leg{
		{LegId: 31, SideList: side("dn3")},
	}}
	state := &model.SpState{
		Conf: &pb.SpConf{
			SliceIdList:   []uint64{1},
			CloneNameList: []string{"c0", "bare"},
		},
		Slices: map[uint64]*pb.Slice{1: {
			MetaGrpList: []*pb.Group{{GrpId: 10}},
			DataGrpList: []*pb.Group{{
				GrpId: 20,
				LegList: []*pb.Leg{
					{LegId: 21, SideList: side("dn1")},
					{LegId: 22, SideList: side("dn2")},
				},
			}},
		}},
		Clones: map[string]*pb.Clone{
			"c0":   {MetaGrp: cloneGrp},
			"bare": {},
		},
	}
	for _, tc := range []struct {
		grpId  uint64
		parent model.GrpParent
	}{
		{10, model.GrpParent{SliceId: 1}},
		{20, model.GrpParent{SliceId: 1}},
		{30, model.GrpParent{CloneName: "c0"}},
	} {
		parent, grp, ok := findRepairGrp(state, tc.grpId)
		if !ok || parent != tc.parent || grp.GetGrpId() != tc.grpId {
			t.Errorf("findRepairGrp(%d) = %+v, %v, %v; want %+v", tc.grpId,
				parent, grp, ok, tc.parent)
		}
	}
	if _, _, ok := findRepairGrp(state, 99); ok {
		t.Errorf("findRepairGrp found a group no slice or clone holds")
	}
	_, grp, _ := findRepairGrp(state, 20)
	got := strings.Join(repairGrpDnAddrs(grp), ",")
	if got != "dn1,dn2" {
		t.Errorf("plan = %s, want every side of the legs", got)
	}
}

// TestForceStartIds pins force-start's check of its ids: a leg for a mark
// and none for a cancel, the gateway's argument check of a ForceStartGroup
// request (gateway.md GW4), and a group always, the driver's own.
func TestForceStartIds(t *testing.T) {
	for _, tc := range []struct {
		grpId, legId uint64
		cancel       bool
		ok           bool
	}{
		{5, 7, false, true},
		{5, 0, true, true},
		{0, 7, false, false},
		{0, 0, true, false},
		{5, 0, false, false},
		{5, 7, true, false},
	} {
		err := forceStartIds(tc.grpId, tc.legId, tc.cancel)
		if (err == nil) != tc.ok {
			t.Errorf("forceStartIds(%d, %d, %t) = %v, want ok %t", tc.grpId,
				tc.legId, tc.cancel, err, tc.ok)
		}
	}
}
