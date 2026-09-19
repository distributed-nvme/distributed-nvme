package common

import (
	"reflect"
	"testing"
)

// The parsers are the sweep's attribution: a name that decodes wrongly sends
// a removal at the wrong object, and a name that fails to decode leaves a
// leak behind for ever. Every formatter is round-tripped here so a new kind
// or a reordered Sprintf cannot land without its dmKindIdCnt row.

func TestParseDmNameRoundTrip(t *testing.T) {
	nf := NewNameFmt("")
	cases := []struct {
		what string
		name string
		kind DmKind
		ids  []uint64
	}{
		{"DnErrorName", nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn),
			DmKindDnError, []uint64{testSp, testSide, testCn}},
		{"DnLinearName", nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn),
			DmKindDnLinear, []uint64{testSp, testSide, testCn}},
		{"DnMigrSrcName", nf.DnMigrSrcName(testCluster, testDn, testSp, testMigr),
			DmKindDnMigrSrc, []uint64{testSp, testMigr}},
		{"DnMigrFinalName", nf.DnMigrFinalName(testCluster, testDn, testSp, testMigr),
			DmKindDnMigrFinal, []uint64{testSp, testMigr}},
		{"DnSideName", nf.DnSideName(testCluster, testDn, testSp, testSide),
			DmKindDnSide, []uint64{testSp, testSide}},
		{"DnMigrMetaDmName", nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigr),
			DmKindDnMigrMeta, []uint64{testSp, testMigr}},

		{"CnPoolMetaName", nf.CnPoolMetaName(testCluster, testCn, testSp, testSlice),
			DmKindCnPoolMeta, []uint64{testSp, testSlice}},
		{"CnPoolDataName", nf.CnPoolDataName(testCluster, testCn, testSp, testSlice),
			DmKindCnPoolData, []uint64{testSp, testSlice}},
		{"CnPoolFinalName", nf.CnPoolFinalName(testCluster, testCn, testSp, testSlice),
			DmKindCnPoolFinal, []uint64{testSp, testSlice}},
		{"CnThinDevName", nf.CnThinDevName(testCluster, testCn, testSp, testTd, testSlice),
			DmKindCnThinDev, []uint64{testSp, testTd, testSlice}},
		{"CnRaid0Name", nf.CnRaid0Name(testCluster, testCn, testSp, testTd),
			DmKindCnRaid0, []uint64{testSp, testTd}},
		{"CnErrorName", nf.CnErrorName(testCluster, testCn, testSp, testTd),
			DmKindCnError, []uint64{testSp, testTd}},
		{"CnNsDevName", nf.CnNsDevName(testCluster, testCn, testSp, testNs),
			DmKindCnNsDev, []uint64{testSp, testNs}},
		{"CnCloneFinalName", nf.CnCloneFinalName(testCluster, testCn, testSp, testClone),
			DmKindCnCloneFinal, []uint64{testSp, testClone}},
		{"CnXferFinalName", nf.CnXferFinalName(testCluster, testCn, testSp, testXfer),
			DmKindCnXferFinal, []uint64{testSp, testXfer}},
		{"CnLegName", nf.CnLegName(testCluster, testCn, testSp, testLeg),
			DmKindCnLeg, []uint64{testSp, testLeg}},
		{"CnGrpName", nf.CnGrpName(testCluster, testCn, testSp, testGrp),
			DmKindCnGrp, []uint64{testSp, testGrp}},
		{"CnCloneMetaDmName", nf.CnCloneMetaDmName(testCluster, testCn, testSp, testClone),
			DmKindCnCloneMeta, []uint64{testSp, testClone}},
	}
	for _, c := range cases {
		dn, ok := ParseDmName(c.name)
		if !ok {
			t.Errorf("%s: %q did not parse", c.what, c.name)
			continue
		}
		if dn.ClusterId != testCluster {
			t.Errorf("%s: cluster = %#x, want %#x", c.what, dn.ClusterId, testCluster)
		}
		wantNode := testCn
		wantRole := byte(DmRoleCn)
		if c.kind[0] == DmRoleDn {
			wantNode = testDn
			wantRole = DmRoleDn
		}
		if dn.NodeId != wantNode {
			t.Errorf("%s: node = %#x, want %#x", c.what, dn.NodeId, wantNode)
		}
		if dn.Role() != wantRole {
			t.Errorf("%s: role = %q, want %q", c.what, dn.Role(), wantRole)
		}
		if dn.Kind != c.kind {
			t.Errorf("%s: kind = %q, want %q", c.what, dn.Kind, c.kind)
		}
		if !reflect.DeepEqual(dn.Ids, c.ids) {
			t.Errorf("%s: ids = %v, want %v", c.what, dn.Ids, c.ids)
		}
		if dn.SpId() != testSp {
			t.Errorf("%s: SpId = %#x, want %#x", c.what, dn.SpId(), testSp)
		}
	}
	// Every kind of both roles is covered: a new kind without a case here is
	// a kind the sweep would enumerate and never attribute.
	if len(cases) != len(dmKindIdCnt) {
		t.Errorf("%d round-trip cases for %d kinds", len(cases), len(dmKindIdCnt))
	}
}

// A name that is not ours, or is ours but malformed, must be rejected whole:
// the caller's next move on a "true" is a removal.
func TestParseDmNameRejects(t *testing.T) {
	nf := NewNameFmt("")
	leg := nf.CnLegName(testCluster, testCn, testSp, testLeg)
	bad := []struct {
		what string
		name string
	}{
		{"empty", ""},
		{"foreign prefix", "vg-" + leg[4:]},
		{"md array name", nf.CnMdArrayName(testSp, 2, 1, false)},
		{"md dev name", nf.CnMdDevName(testCluster, testCn, testSp, 2, 1, false)},
		{"lvm-ish", "dnv--sp--vg-dnv--lv"},
		{"old digit kind", "dnv-ebada5168620c5fe-0000000000000005-9-" +
			"0000000000000011-0000000000000015"},
		{"unknown kind", "dnv-ebada5168620c5fe-0000000000000005-cf-" +
			"0000000000000011-0000000000000015"},
		{"unknown role", "dnv-ebada5168620c5fe-0000000000000005-x9-" +
			"0000000000000011-0000000000000015"},
		{"too few ids", "dnv-ebada5168620c5fe-0000000000000005-c9-0000000000000011"},
		{"too many ids", leg + "-0000000000000099"},
		{"short id field", "dnv-ebada5168620c5fe-0000000000000005-c9-11-15"},
		{"upper-case id", "dnv-EBADA5168620C5FE-0000000000000005-c9-" +
			"0000000000000011-0000000000000015"},
		{"trailing dash", leg + "-"},
		{"clone-meta prefix alone", nf.CnCloneMetaDmPrefix(testCluster, testCn)},
	}
	for _, c := range bad {
		if dn, ok := ParseDmName(c.name); ok {
			t.Errorf("%s: %q parsed as %+v, want rejected", c.what, c.name, dn)
		}
	}
}

func TestParseNqnRoundTrip(t *testing.T) {
	nf := NewNameFmt("")
	cases := []struct {
		what string
		nqn  string
		kind NqnKind
		ids  []uint64
	}{
		{"DnHostNqn", nf.DnHostNqn(testCluster, testDn),
			NqnKindDnHost, []uint64{testCluster, testDn}},
		{"CnHostNqn", nf.CnHostNqn(testCluster, testCn),
			NqnKindCnHost, []uint64{testCluster, testCn}},
		{"SideToCnNqn", nf.SideToCnNqn(testCluster, testSp, testLeg, testCn),
			NqnKindSideToCn, []uint64{testCluster, testSp, testLeg, testCn}},
		{"MigrSrcNqn", nf.MigrSrcNqn(testCluster, testDn, testSp, testMigr),
			NqnKindMigrSrc, []uint64{testCluster, testDn, testSp, testMigr}},
		{"XferNqn", nf.XferNqn(testCluster, testSp, testXfer),
			NqnKindXfer, []uint64{testCluster, testSp, testXfer}},
	}
	for _, c := range cases {
		parts, ok := ParseNqn(c.nqn)
		if !ok {
			t.Errorf("%s: %q did not parse", c.what, c.nqn)
			continue
		}
		if parts.Kind != c.kind {
			t.Errorf("%s: kind = %d, want %d", c.what, parts.Kind, c.kind)
		}
		if !reflect.DeepEqual(parts.Ids, c.ids) {
			t.Errorf("%s: ids = %v, want %v", c.what, parts.Ids, c.ids)
		}
	}
	if len(cases) != len(nqnKindIdCnt) {
		t.Errorf("%d round-trip cases for %d nqn kinds", len(cases), len(nqnKindIdCnt))
	}
}

// A host-facing subsystem NQN is user-chosen; "not dnv-format" is the signal
// the sweep attributes it by its namespaces' backing device instead ([D15]).
func TestParseNqnRejects(t *testing.T) {
	bad := []string{
		"",
		"nqn.2014-08.org.nvmexpress.discovery",
		"nqn.2016-06.io.spdk:cnode1",
		NqnPrefix,
		NqnPrefix + ":",
		NqnPrefix + ":2",
		NqnPrefix + "x:2:0000000000000001:0000000000000002:" +
			"0000000000000003:0000000000000004",
		NqnPrefix + ":9:0000000000000001:0000000000000002",
		NqnPrefix + ":2:0000000000000001:0000000000000002",
		NqnPrefix + ":0:0000000000000001:0000000000000002:0000000000000003",
		NqnPrefix + ":0:1:2",
	}
	for _, nqn := range bad {
		if parts, ok := ParseNqn(nqn); ok {
			t.Errorf("%q parsed as %+v, want rejected", nqn, parts)
		}
	}
}
