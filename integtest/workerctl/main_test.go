// Tests for the parsing surface of workerctl (dnv-worker.md §14.8). The
// driver writes straight into the etcd the whole suite reads back, so a
// malformed test invocation must fail loudly here rather than land garbage in
// a key: these cover every tuple form of the §14.8 flag table, the 0x hex ids,
// the bitmap payload, the §5.3 key kind → message mapping of `get`, and the
// put-sp wiring validation that joins the placement flags on their ids.
package main

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
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
	// flags the caller named (§14.8).
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
		// ext_cnt 0 has no §3.6 geometry at all.
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
	// 47 is between two defined levels: storing it would leave a level no §7
	// rule knows.
	for _, bad := range []string{"", "47", "-1", "nonesuch"} {
		if _, err := parseSpLevel(bad); err == nil {
			t.Errorf("parseSpLevel(%q) succeeded, want an error", bad)
		}
	}
}

// TestMessageForKey pins the §5.3 mapping `get` uses: the SECOND field of a
// key names the message its value decodes into.
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
			"dnv clone_bitmap ebada5168620c5fe 0000000000000001 c0 00",
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

func TestNvmeTrConfOf(t *testing.T) {
	conf := nvmeTrConfOf("192.168.10.20:29600")
	if conf.GetTrAddr() != "192.168.10.20" {
		t.Fatalf("tr_addr = %q", conf.GetTrAddr())
	}
	// The node's own port, so two nodes on one host get distinct
	// NvmeTrConfs and a CdcEntry's transport list stays attributable
	// (§14.11 case D step 3).
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

// smokePlan is the §14.11 case S placement: two cntlrs, one slice, a raid1
// meta group of 1 extent and a raid1 data group of 2, four sides on two DNs.
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
	// Σ ext_cnt over ALL groups is what one cntlr's CN reserves (§8.4).
	if plan.footprint != 3 {
		t.Fatalf("footprint = %d, want 3", plan.footprint)
	}
	// next_id must be past every id the script assigned (§14.5).
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
	// Fewer slots than cntlrs cannot give every cntlr a distinct one (§11.8).
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
// wrong: §14.11 case E spreads DNs over the shard codes "00", "55", "aa" and
// "ff", which are the common.ShardCodeFmt spelling and therefore hex.
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
	// 256 and up have no two-digit %02x spelling and no bucket slot (§5.4).
	for _, bad := range []string{"", "100", "-1", "zz", "0x100"} {
		var shard shardFlag
		if err := shard.Set(bad); err == nil {
			t.Errorf("Set(%q) succeeded, want an error", bad)
		}
	}
}
