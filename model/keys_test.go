package model

import (
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// goldenCid and goldenSpId are the architecture.md §5.1 example's inputs.
const (
	goldenCid  = uint64(0xebada5168620c5fe)
	goldenSpId = uint64(17)
)

const (
	goldenDnAddr = "192.168.0.17:9000"
	goldenCnAddr = "192.168.0.18:9000"
)

// TestSpNameKeyIsTheSpecExample pins the one key architecture.md §5.1 spells
// out in full (MD2, MD9). If this ever changes, every stored key of every
// existing cluster has changed with it.
func TestSpNameKeyIsTheSpecExample(t *testing.T) {
	got := SpNameKey(goldenCid, goldenSpId)
	want := "dnv sp_id_to_name ebada5168620c5fe 0000000000000011"
	if got != want {
		t.Fatalf("SpNameKey = %q, want %q", got, want)
	}
}

// TestKeyGoldenStrings pins one golden string per §5.3 key (MD9).
func TestKeyGoldenStrings(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			"ClusterConfKey",
			ClusterConfKey("cluster0"),
			"dnv cluster_conf cluster0",
		},
		{
			"DnGlobalKey",
			DnGlobalKey(goldenCid),
			"dnv dn_global ebada5168620c5fe",
		},
		{
			"CnGlobalKey",
			CnGlobalKey(goldenCid),
			"dnv cn_global ebada5168620c5fe",
		},
		{
			"SpGlobalKey",
			SpGlobalKey(goldenCid),
			"dnv sp_global ebada5168620c5fe",
		},
		{
			"DnRevKey",
			DnRevKey(4, goldenCid, 18),
			"dnv dn_rev 04 ebada5168620c5fe 0000000000000012",
		},
		{
			"CnRevKey",
			CnRevKey(255, goldenCid, 1),
			"dnv cn_rev ff ebada5168620c5fe 0000000000000001",
		},
		{
			"SpRevKey",
			SpRevKey(4, goldenCid, goldenSpId),
			"dnv sp_rev 04 ebada5168620c5fe 0000000000000011",
		},
		{
			"DnConfKey",
			DnConfKey(goldenCid, goldenDnAddr),
			"dnv dn_conf ebada5168620c5fe 192.168.0.17:9000",
		},
		{
			"CnConfKey",
			CnConfKey(goldenCid, goldenCnAddr),
			"dnv cn_conf ebada5168620c5fe 192.168.0.18:9000",
		},
		{
			"DnCapacityKey",
			DnCapacityKey(goldenCid, 2, 4096, goldenDnAddr),
			"dnv dn_capacity ebada5168620c5fe 2 0000000000001000 " +
				"192.168.0.17:9000",
		},
		{
			"CnCapacityKey",
			CnCapacityKey(goldenCid, 15, goldenCnAddr),
			"dnv cn_capacity ebada5168620c5fe 000000000000000f " +
				"192.168.0.18:9000",
		},
		{
			"CdcEntryKey",
			CdcEntryKey(goldenCid, 4, goldenSpId, 5),
			"dnv cdc ebada5168620c5fe 04 0000000000000011 " +
				"0000000000000005",
		},
		{
			"SpConfKey",
			SpConfKey(goldenCid, "pool1"),
			"dnv sp_conf ebada5168620c5fe pool1",
		},
		{
			"CntlrKey",
			CntlrKey(goldenCid, goldenSpId, 21),
			"dnv cntlr ebada5168620c5fe 0000000000000011 " +
				"0000000000000015",
		},
		{
			"SliceKey",
			SliceKey(goldenCid, goldenSpId, 22),
			"dnv slice ebada5168620c5fe 0000000000000011 " +
				"0000000000000016",
		},
		{
			"ThinDeviceKey",
			ThinDeviceKey(goldenCid, goldenSpId, "td0"),
			"dnv thin_device ebada5168620c5fe 0000000000000011 td0",
		},
		{
			"SubsystemKey",
			SubsystemKey(goldenCid, goldenSpId, "nqn.2024-01.io.dnv:s0"),
			"dnv subsystem ebada5168620c5fe 0000000000000011 " +
				"nqn.2024-01.io.dnv:s0",
		},
		{
			"CloneKey",
			CloneKey(goldenCid, goldenSpId, "clone0"),
			"dnv clone ebada5168620c5fe 0000000000000011 clone0",
		},
		{
			"CloneBitmapKey",
			CloneBitmapKey(goldenCid, goldenSpId, "clone0", 3),
			"dnv clone_bitmap ebada5168620c5fe 0000000000000011 " +
				"clone0 03",
		},
		{
			"TransferKey",
			TransferKey(goldenCid, goldenSpId, "xfer0"),
			"dnv transfer ebada5168620c5fe 0000000000000011 xfer0",
		},
		{
			"MigrationKey",
			MigrationKey(goldenCid, goldenSpId, "migr0"),
			"dnv migration ebada5168620c5fe 0000000000000011 migr0",
		},
		{
			"MigrBitmapKey",
			MigrBitmapKey(goldenCid, goldenSpId, "migr0", 1),
			"dnv migration_bitmap ebada5168620c5fe 0000000000000011 " +
				"migr0 01",
		},
		{
			"WorkerRegKey",
			WorkerRegKey(common.WorkerRoleSp, "a1b2c3d4"),
			"dnv worker sp a1b2c3d4",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
		if strings.HasPrefix(tc.got, " ") || strings.HasSuffix(tc.got, " ") {
			t.Errorf("%s = %q: a key has no outer space", tc.name, tc.got)
		}
		if strings.Contains(tc.got, "  ") {
			t.Errorf("%s = %q: fields join with ONE space", tc.name, tc.got)
		}
	}
}

// TestPrefixesEndInOneSpace checks the §5.1 invariant every scan and watch
// depends on (MD2, MD9): a prefix ends in exactly one space, so that it can
// never match a longer sibling field.
func TestPrefixesEndInOneSpace(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ClusterConfPrefix", ClusterConfPrefix(), "dnv cluster_conf "},
		{"DnRevPrefix", DnRevPrefix(4), "dnv dn_rev 04 "},
		{"CnRevPrefix", CnRevPrefix(255), "dnv cn_rev ff "},
		{"SpRevPrefix", SpRevPrefix(0), "dnv sp_rev 00 "},
		{
			"DnCapacityPrefix",
			DnCapacityPrefix(goldenCid, 2),
			"dnv dn_capacity ebada5168620c5fe 2 ",
		},
		{
			"CnCapacityPrefix",
			CnCapacityPrefix(goldenCid),
			"dnv cn_capacity ebada5168620c5fe ",
		},
		{"CdcEntryPrefix", CdcEntryPrefix(), "dnv cdc "},
		{
			"CloneBitmapPrefix",
			CloneBitmapPrefix(goldenCid, goldenSpId, "clone0"),
			"dnv clone_bitmap ebada5168620c5fe 0000000000000011 clone0 ",
		},
		{
			"MigrBitmapPrefix",
			MigrBitmapPrefix(goldenCid, goldenSpId, "migr0"),
			"dnv migration_bitmap ebada5168620c5fe 0000000000000011 " +
				"migr0 ",
		},
		{
			"WorkerRegPrefix",
			WorkerRegPrefix(common.WorkerRoleDn),
			"dnv worker dn ",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
		if !strings.HasSuffix(tc.got, " ") {
			t.Errorf("%s = %q: a prefix must end in a space", tc.name, tc.got)
		}
		if strings.HasSuffix(tc.got, "  ") {
			t.Errorf("%s = %q: exactly one trailing space", tc.name, tc.got)
		}
	}
}

// TestPrefixIsPrefixOfKey checks that the prefix a worker scans really covers
// the keys it is meant to find (MD2).
func TestPrefixIsPrefixOfKey(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		key    string
	}{
		{
			"dn_rev",
			DnRevPrefix(7),
			DnRevKey(7, goldenCid, 3),
		},
		{
			"cn_rev",
			CnRevPrefix(7),
			CnRevKey(7, goldenCid, 3),
		},
		{
			"sp_rev",
			SpRevPrefix(7),
			SpRevKey(7, goldenCid, 3),
		},
		{
			"dn_capacity",
			DnCapacityPrefix(goldenCid, 1),
			DnCapacityKey(goldenCid, 1, 20, goldenDnAddr),
		},
		{
			"cn_capacity",
			CnCapacityPrefix(goldenCid),
			CnCapacityKey(goldenCid, 20, goldenCnAddr),
		},
		{
			"clone_bitmap",
			CloneBitmapPrefix(goldenCid, goldenSpId, "clone0"),
			CloneBitmapKey(goldenCid, goldenSpId, "clone0", 2),
		},
		{
			"migration_bitmap",
			MigrBitmapPrefix(goldenCid, goldenSpId, "migr0"),
			MigrBitmapKey(goldenCid, goldenSpId, "migr0", 2),
		},
		{
			"worker",
			WorkerRegPrefix(common.WorkerRoleCn),
			WorkerRegKey(common.WorkerRoleCn, "seed0"),
		},
		{
			"cluster_conf",
			ClusterConfPrefix(),
			ClusterConfKey("cluster0"),
		},
		{
			"cdc",
			CdcEntryPrefix(),
			CdcEntryKey(goldenCid, 4, goldenSpId, 5),
		},
	}
	for _, tc := range cases {
		if !strings.HasPrefix(tc.key, tc.prefix) {
			t.Errorf(
				"%s: %q is not a prefix of %q", tc.name, tc.prefix, tc.key,
			)
		}
	}
}

// TestClusterId pins the §5.2 derivation against recorded vectors (MD9).
// Changing any of these orphans every key of every existing cluster.
func TestClusterId(t *testing.T) {
	cases := []struct {
		name  string
		epoch uint64
		want  uint64
	}{
		{"default", 1756000000000000000, 0x5e205a57a5548ded},
		{"default", 0, 0xe37ab2ef8796f1be},
		{"", 0, 0xa8c7f832281a39c5},
		{"cluster0", 1756000000000000000, 0x142b4646c6e427f8},
	}
	for _, tc := range cases {
		got := ClusterId(tc.name, tc.epoch)
		if got != tc.want {
			t.Errorf(
				"ClusterId(%q, %d) = %#016x, want %#016x",
				tc.name, tc.epoch, got, tc.want,
			)
		}
	}
	// The epoch is folded in as raw bytes, so the same name with two epochs
	// is two clusters (§5.2: delete + recreate never inherits a key space).
	if ClusterId("c", 1) == ClusterId("c", 2) {
		t.Error("ClusterId ignores creation_epoch")
	}
	// ... and it is not a text concatenation either.
	if ClusterId("c1", 0) == ClusterId("c", 0x3100000000000000) {
		t.Error("ClusterId folds the epoch in as text")
	}
}

// TestParseRevKeyRoundTrip round-trips the three rev-key parsers over the
// edges of every field (MD2, MD9).
func TestParseRevKeyRoundTrip(t *testing.T) {
	type builder struct {
		name  string
		build func(uint32, uint64, uint64) string
		parse func(string) (uint32, uint64, uint64, bool)
	}
	builders := []builder{
		{"dn_rev", DnRevKey, ParseDnRevKey},
		{"cn_rev", CnRevKey, ParseCnRevKey},
		{"sp_rev", SpRevKey, ParseSpRevKey},
	}
	values := []struct {
		shard uint32
		cid   uint64
		id    uint64
	}{
		{0, 0, 0},
		{4, goldenCid, goldenSpId},
		{255, 0xffffffffffffffff, 0xffffffffffffffff},
		{1, 1, 1},
	}
	for _, b := range builders {
		for _, v := range values {
			key := b.build(v.shard, v.cid, v.id)
			shard, cid, id, ok := b.parse(key)
			if !ok {
				t.Errorf("%s: parse(%q) not ok", b.name, key)
				continue
			}
			if shard != v.shard || cid != v.cid || id != v.id {
				t.Errorf(
					"%s: parse(%q) = (%d, %#x, %#x), want (%d, %#x, %#x)",
					b.name, key, shard, cid, id, v.shard, v.cid, v.id,
				)
			}
		}
	}
	// The three kinds never accept each other's keys.
	if _, _, _, ok := ParseDnRevKey(CnRevKey(1, 2, 3)); ok {
		t.Error("ParseDnRevKey accepted a cn_rev key")
	}
	if _, _, _, ok := ParseCnRevKey(SpRevKey(1, 2, 3)); ok {
		t.Error("ParseCnRevKey accepted an sp_rev key")
	}
	if _, _, _, ok := ParseSpRevKey(DnRevKey(1, 2, 3)); ok {
		t.Error("ParseSpRevKey accepted a dn_rev key")
	}
}

// TestParseRevKeyRejectsMalformed checks that a bad key is skipped, never
// mis-parsed and never a panic (MD2: SW2 logs and skips).
func TestParseRevKeyRejectsMalformed(t *testing.T) {
	good := DnRevKey(4, goldenCid, goldenSpId)
	bad := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"prefix only", "dnv "},
		{"too few fields", "dnv dn_rev 04 ebada5168620c5fe"},
		{"too many fields", good + " extra"},
		{"wrong prefix", "xxx dn_rev 04 ebada5168620c5fe " +
			"0000000000000011"},
		{"wrong kind", "dnv dn_revs 04 ebada5168620c5fe " +
			"0000000000000011"},
		{"unpadded shard", "dnv dn_rev 4 ebada5168620c5fe " +
			"0000000000000011"},
		{"shard out of range", "dnv dn_rev 100 ebada5168620c5fe " +
			"0000000000000011"},
		{"upper-case cid", "dnv dn_rev 04 EBADA5168620C5FE " +
			"0000000000000011"},
		{"non-hex cid", "dnv dn_rev 04 zzzzzzzzzzzzzzzz " +
			"0000000000000011"},
		{"short cid", "dnv dn_rev 04 ebada5168620c5f " +
			"0000000000000011"},
		{"unpadded id", "dnv dn_rev 04 ebada5168620c5fe 11"},
		{"empty id", "dnv dn_rev 04 ebada5168620c5fe "},
		{"0x id", "dnv dn_rev 04 ebada5168620c5fe 0x00000000000011"},
		{"tab separated", strings.ReplaceAll(good, " ", "\t")},
	}
	for _, tc := range bad {
		if _, _, _, ok := ParseDnRevKey(tc.key); ok {
			t.Errorf("%s: ParseDnRevKey(%q) accepted it", tc.name, tc.key)
		}
	}
	if _, _, _, ok := ParseDnRevKey(good); !ok {
		t.Errorf("ParseDnRevKey(%q) rejected a good key", good)
	}
}

// TestParseWorkerRegKey round-trips the registration parser and checks its
// rejections (MD2, VW3).
func TestParseWorkerRegKey(t *testing.T) {
	roles := []string{
		common.WorkerRoleDn,
		common.WorkerRoleCn,
		common.WorkerRoleSp,
	}
	seed := "6f0a1e2b-1c3d-4e5f-8a9b-0c1d2e3f4a5b"
	for _, want := range roles {
		key := WorkerRegKey(want, seed)
		role, got, ok := ParseWorkerRegKey(key)
		if !ok || role != want || got != seed {
			t.Errorf(
				"ParseWorkerRegKey(%q) = (%q, %q, %v)",
				key, role, got, ok,
			)
		}
	}
	bad := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"too few fields", "dnv worker sp"},
		{"too many fields", "dnv worker sp seed extra"},
		{"wrong prefix", "xxx worker sp seed"},
		{"wrong kind", "dnv workers sp seed"},
		{"unknown role", "dnv worker xx seed"},
		{"empty role", "dnv worker  seed"},
		{"empty seed", "dnv worker sp "},
	}
	for _, tc := range bad {
		if _, _, ok := ParseWorkerRegKey(tc.key); ok {
			t.Errorf(
				"%s: ParseWorkerRegKey(%q) accepted it", tc.name, tc.key,
			)
		}
	}
}

// TestParseBmIdx round-trips the bitmap-chunk parser over both bitmap kinds
// and checks its rejections (MD2, MD3).
func TestParseBmIdx(t *testing.T) {
	for _, bmIdx := range []uint32{0, 1, 15, 16, 255} {
		cloneKey := CloneBitmapKey(goldenCid, goldenSpId, "clone0", bmIdx)
		got, ok := ParseBmIdx(cloneKey)
		if !ok || got != bmIdx {
			t.Errorf("ParseBmIdx(%q) = (%d, %v)", cloneKey, got, ok)
		}
		migrKey := MigrBitmapKey(goldenCid, goldenSpId, "migr0", bmIdx)
		got, ok = ParseBmIdx(migrKey)
		if !ok || got != bmIdx {
			t.Errorf("ParseBmIdx(%q) = (%d, %v)", migrKey, got, ok)
		}
	}
	head := "dnv clone_bitmap ebada5168620c5fe 0000000000000011 clone0"
	bad := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"too few fields", head},
		{"too many fields", head + " 03 extra"},
		{"wrong prefix", "xxx clone_bitmap ebada5168620c5fe " +
			"0000000000000011 clone0 03"},
		{"not a bitmap kind", CloneKey(goldenCid, goldenSpId, "clone0") +
			" 03"},
		{"unpadded index", head + " 3"},
		{"empty index", head + " "},
		{"non-hex index", head + " zz"},
		{"empty name", "dnv clone_bitmap ebada5168620c5fe " +
			"0000000000000011  03"},
		{"bad cid", "dnv clone_bitmap zzzz 0000000000000011 clone0 03"},
		{"bad sp id", "dnv clone_bitmap ebada5168620c5fe 11 clone0 03"},
	}
	for _, tc := range bad {
		if _, ok := ParseBmIdx(tc.key); ok {
			t.Errorf("%s: ParseBmIdx(%q) accepted it", tc.name, tc.key)
		}
	}
}

// TestParseCapacityKeys round-trips the two capacity-key parsers the §6
// allocator turns a scan back into candidates with (MD2, MD5).
func TestParseCapacityKeys(t *testing.T) {
	for binIdx := uint32(0); binIdx <= 3; binIdx++ {
		for _, freeExt := range []uint64{0, 1, 4096, 0xffffffffffffffff} {
			key := DnCapacityKey(goldenCid, binIdx, freeExt, goldenDnAddr)
			gotBin, gotFree, gotAddr, ok := ParseDnCapacityKey(key)
			if !ok {
				t.Errorf("ParseDnCapacityKey(%q) not ok", key)
				continue
			}
			if gotBin != binIdx || gotFree != freeExt ||
				gotAddr != goldenDnAddr {
				t.Errorf(
					"ParseDnCapacityKey(%q) = (%d, %d, %q)",
					key, gotBin, gotFree, gotAddr,
				)
			}
		}
	}
	for _, freeExt := range []uint64{0, 1, 4096, 0xffffffffffffffff} {
		key := CnCapacityKey(goldenCid, freeExt, goldenCnAddr)
		gotFree, gotAddr, ok := ParseCnCapacityKey(key)
		if !ok || gotFree != freeExt || gotAddr != goldenCnAddr {
			t.Errorf(
				"ParseCnCapacityKey(%q) = (%d, %q, %v)",
				key, gotFree, gotAddr, ok,
			)
		}
	}
	dnHead := "dnv dn_capacity ebada5168620c5fe"
	badDn := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"too few fields", dnHead + " 2 0000000000001000"},
		{"too many fields", dnHead + " 2 0000000000001000 a:1 extra"},
		{"wrong kind", "dnv cn_capacity ebada5168620c5fe 2 " +
			"0000000000001000 a:1"},
		{"two-digit bin", dnHead + " 02 0000000000001000 a:1"},
		{"non-hex bin", dnHead + " z 0000000000001000 a:1"},
		{"unpadded free", dnHead + " 2 1000 a:1"},
		{"upper-case free", dnHead + " 2 0000000000001A00 a:1"},
		{"empty addr_port", dnHead + " 2 0000000000001000 "},
		{"bad cid", "dnv dn_capacity zz 2 0000000000001000 a:1"},
	}
	for _, tc := range badDn {
		if _, _, _, ok := ParseDnCapacityKey(tc.key); ok {
			t.Errorf(
				"%s: ParseDnCapacityKey(%q) accepted it", tc.name, tc.key,
			)
		}
	}
	cnHead := "dnv cn_capacity ebada5168620c5fe"
	badCn := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"too few fields", cnHead + " 0000000000001000"},
		{"too many fields", cnHead + " 0000000000001000 a:1 extra"},
		{"wrong kind", dnHead + " 0000000000001000 a:1"},
		{"unpadded free", cnHead + " 1000 a:1"},
		{"empty addr_port", cnHead + " 0000000000001000 "},
	}
	for _, tc := range badCn {
		if _, _, ok := ParseCnCapacityKey(tc.key); ok {
			t.Errorf(
				"%s: ParseCnCapacityKey(%q) accepted it", tc.name, tc.key,
			)
		}
	}
}

// TestCapacityKeyOrderIsNumeric checks the §5.6 claim the whole allocator
// rests on: the zero-padded FreeSpaceFmt makes lexical key order equal
// numeric free-space order, so one descending range returns nodes
// largest-free first.
func TestCapacityKeyOrderIsNumeric(t *testing.T) {
	free := []uint64{0, 1, 9, 10, 15, 16, 255, 256, 4096, 1 << 40}
	for i := 1; i < len(free); i++ {
		lower := DnCapacityKey(goldenCid, 0, free[i-1], goldenDnAddr)
		higher := DnCapacityKey(goldenCid, 0, free[i], goldenDnAddr)
		if !(lower < higher) {
			t.Errorf(
				"free %d key %q does not sort before free %d key %q",
				free[i-1], lower, free[i], higher,
			)
		}
	}
}
