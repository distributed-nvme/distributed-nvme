package common

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"testing"
)

// The ids used across the tables of architecture.md §4.
const (
	testCluster = uint64(0xebada5168620c5fe)
	testDn      = uint64(3)
	testCn      = uint64(5)
	testSp      = uint64(0x11)
	testSide    = uint64(0x16)
	testLeg     = uint64(0x15)
	testSlice   = uint64(0x21)
	testTd      = uint64(0x31)
	testMigr    = uint64(0x1e)
	testClone   = uint64(0x41)
	testXfer    = uint64(0x51)
	testNs      = uint64(0x32)
)

func checkName(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

// §4.2 dm device names.
func TestDmNames(t *testing.T) {
	nf := NewNameFmt("")
	const c = "ebada5168620c5fe"

	checkName(t, "DnErrorName",
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn),
		"dnv-"+c+"-0000000000000003-0-0000000000000011-0000000000000016-0000000000000005")
	checkName(t, "DnLinearName",
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn),
		"dnv-"+c+"-0000000000000003-1-0000000000000011-0000000000000016-0000000000000005")
	checkName(t, "DnMigrSrcName",
		nf.DnMigrSrcName(testCluster, testDn, testSp, testMigr),
		"dnv-"+c+"-0000000000000003-2-0000000000000011-000000000000001e")
	checkName(t, "DnMigrFinalName",
		nf.DnMigrFinalName(testCluster, testDn, testSp, testMigr),
		"dnv-"+c+"-0000000000000003-3-0000000000000011-000000000000001e")

	checkName(t, "CnPoolMetaName",
		nf.CnPoolMetaName(testCluster, testCn, testSp, testSlice),
		"dnv-"+c+"-0000000000000005-0-0000000000000011-0000000000000021")
	checkName(t, "CnPoolDataName",
		nf.CnPoolDataName(testCluster, testCn, testSp, testSlice),
		"dnv-"+c+"-0000000000000005-1-0000000000000011-0000000000000021")
	checkName(t, "CnPoolFinalName",
		nf.CnPoolFinalName(testCluster, testCn, testSp, testSlice),
		"dnv-"+c+"-0000000000000005-2-0000000000000011-0000000000000021")
	checkName(t, "CnThinDevName",
		nf.CnThinDevName(testCluster, testCn, testSp, testTd, testSlice),
		"dnv-"+c+"-0000000000000005-3-0000000000000011-0000000000000031-0000000000000021")
	checkName(t, "CnRaid0Name",
		nf.CnRaid0Name(testCluster, testCn, testSp, testTd),
		"dnv-"+c+"-0000000000000005-4-0000000000000011-0000000000000031")
	checkName(t, "CnErrorName",
		nf.CnErrorName(testCluster, testCn, testSp, testTd),
		"dnv-"+c+"-0000000000000005-5-0000000000000011-0000000000000031")
	checkName(t, "CnNsDevName",
		nf.CnNsDevName(testCluster, testCn, testSp, testNs),
		"dnv-"+c+"-0000000000000005-6-0000000000000011-0000000000000032")
	checkName(t, "CnCloneFinalName",
		nf.CnCloneFinalName(testCluster, testCn, testSp, testClone),
		"dnv-"+c+"-0000000000000005-7-0000000000000011-0000000000000041")
	checkName(t, "CnXferFinalName",
		nf.CnXferFinalName(testCluster, testCn, testSp, testXfer),
		"dnv-"+c+"-0000000000000005-8-0000000000000011-0000000000000051")

	checkName(t, "DmPath", nf.DmPath("dnv-x"), "/dev/mapper/dnv-x")
	checkName(t, "MdPath", nf.MdPath("dnv-x"), "/dev/md/dnv-x")
}

// §4.3 md names.
func TestMdNames(t *testing.T) {
	nf := NewNameFmt("")

	// shortId = uint32(fnv64a("%016x%016x")) — the hash's low 32 bits.
	h := fnv.New64a()
	fmt.Fprintf(h, "%016x%016x", testCluster, testCn)
	shortId := uint32(h.Sum64())

	dataName := nf.CnMdDevName(testCluster, testCn, testSp, 2, 1, false)
	metaName := nf.CnMdDevName(testCluster, testCn, testSp, 2, 1, true)

	checkName(t, "CnMdDevName(data)", dataName,
		fmt.Sprintf("%08x%016x%02x%02x", shortId, testSp, 2, 1))
	checkName(t, "CnMdDevName(meta)", metaName,
		fmt.Sprintf("%08x%016x%02x%02x", shortId, testSp, 2|0x80, 1))

	// 28 hex chars, no separators: "md_" + 28 = 31 stays within the kernel
	// DISK_NAME_LEN (32) even as a named array's kernel disk name.
	hex28 := regexp.MustCompile(`^[0-9a-f]{28}$`)
	for _, name := range []string{dataName, metaName} {
		if !hex28.MatchString(name) {
			t.Errorf("md dev name %q is not 28 hex chars", name)
		}
	}
	if other := nf.CnMdDevName(testCluster, testCn+1, testSp, 2, 1, false); other == dataName {
		t.Error("md dev names of two cns collide")
	}

	checkName(t, "CnMdArrayName(data)",
		nf.CnMdArrayName(testSp, 2, 1, false), "dnv-0000000000000011-02-01")
	checkName(t, "CnMdArrayName(meta)",
		nf.CnMdArrayName(testSp, 2, 1, true), "dnv-0000000000000011-82-01")
}

// §4.4 NQNs.
func TestNqns(t *testing.T) {
	nf := NewNameFmt("")
	const p = "nqn.2024-01.io.dnv"
	const c = "ebada5168620c5fe"

	checkName(t, "DnHostNqn", nf.DnHostNqn(testCluster, testDn),
		p+":0:"+c+":0000000000000003")
	checkName(t, "CnHostNqn", nf.CnHostNqn(testCluster, testCn),
		p+":1:"+c+":0000000000000005")
	// A side subsystem is keyed by leg_id and carries no dn_id, so the two
	// sides of a migrating leg export the same NQN (architecture.md §4.4).
	checkName(t, "SideToCnNqn",
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn),
		p+":2:"+c+":0000000000000011:0000000000000015:0000000000000005")
	checkName(t, "MigrSrcNqn",
		nf.MigrSrcNqn(testCluster, testDn, testSp, testMigr),
		p+":3:"+c+":0000000000000003:0000000000000011:000000000000001e")
	// A transfer NQN carries no node id: every enabled cntlr of the SP
	// exports the identical subsystem (§8.10).
	checkName(t, "XferNqn", nf.XferNqn(testCluster, testSp, testXfer),
		p+":4:"+c+":0000000000000011:0000000000000051")

	// No dn component: the src and dst sides of a migrating leg live on two
	// DNs and must still produce the identical string, which is what lets the
	// CN aggregate them into one multipath namespace (§11.2).
	sideNqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn)
	if n := strings.Count(sideNqn, ":"); n != 5 {
		t.Errorf("SideToCnNqn %q has %d ':' separators, want 5 "+
			"(prefix, kind, cluster, sp, leg, cn)", sideNqn, n)
	}
	for _, dnId := range []uint64{testDn, testDn + 1} {
		if strings.Contains(sideNqn, fmt.Sprintf("%016x", dnId)) {
			t.Errorf("SideToCnNqn %q carries dn %016x", sideNqn, dnId)
		}
	}

	for _, nqn := range []string{
		nf.DnHostNqn(testCluster, testDn),
		nf.CnHostNqn(testCluster, testCn),
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn),
		nf.MigrSrcNqn(testCluster, testDn, testSp, testMigr),
		nf.XferNqn(testCluster, testSp, testXfer),
	} {
		if len(nqn) > MaxNqnLength {
			t.Errorf("nqn %q is %d chars, over the %d limit", nqn, len(nqn), MaxNqnLength)
		}
		if !regexp.MustCompile(ValidNqnPattern).MatchString(nqn) {
			t.Errorf("nqn %q does not match ValidNqnPattern", nqn)
		}
	}
}

// §4.5 LVM / tmpfs / file names.
func TestLvmAndTmpfsNames(t *testing.T) {
	nf := NewNameFmt("")
	const c = "ebada5168620c5fe"

	checkName(t, "DnVgName", nf.DnVgName(testCluster, testDn),
		"dnv-dn-"+c+"-0000000000000003")
	checkName(t, "DnLvName", nf.DnLvName(testSp, testSide),
		"0000000000000011-0000000000000016")
	checkName(t, "DnLvPath", nf.DnLvPath(testCluster, testDn, testSp, testSide),
		"/dev/dnv-dn-"+c+"-0000000000000003/0000000000000011-0000000000000016")

	checkName(t, "DnMigrPvName", nf.DnMigrPvName(), "migr-pv")
	checkName(t, "DnMigrPvPath", nf.DnMigrPvPath(testCluster, testDn),
		"/dev/dnv-dn-"+c+"-0000000000000003/migr-pv")
	checkName(t, "DnMigrVgName", nf.DnMigrVgName(testCluster, testDn),
		"dnv-migr-"+c+"-0000000000000003")
	checkName(t, "DnMigrMetaName", nf.DnMigrMetaName(testSp, testMigr),
		"0000000000000011-000000000000001e")
	checkName(t, "DnMigrMetaPath", nf.DnMigrMetaPath(testCluster, testDn, testSp, testMigr),
		"/dev/dnv-migr-"+c+"-0000000000000003/0000000000000011-000000000000001e")

	checkName(t, "CnCloneVgName", nf.CnCloneVgName(testCluster, testCn),
		"dnv-clone-vg-"+c+"-0000000000000005")
	checkName(t, "CnCloneMetaName", nf.CnCloneMetaName(testSp, testClone),
		"0000000000000011-0000000000000041")
	checkName(t, "CnCloneMetaPath", nf.CnCloneMetaPath(testCluster, testCn, testSp, testClone),
		"/dev/dnv-clone-vg-"+c+"-0000000000000005/0000000000000011-0000000000000041")

	checkName(t, "CnTmpfsPath", nf.CnTmpfsPath(testCluster, testCn),
		DefaultTmpfsPrefix+"/"+c+"-0000000000000005")
	checkName(t, "CnTmpFileName", nf.CnTmpFileName(), "tmp-file")
	checkName(t, "CnTmpFilePath", nf.CnTmpFilePath(testCluster, testCn),
		DefaultTmpfsPrefix+"/"+c+"-0000000000000005/tmp-file")
}

// §4.6 agent local-store paths.
func TestLocalStorePaths(t *testing.T) {
	nf := NewNameFmt("")
	const c = "ebada5168620c5fe"

	checkName(t, "LocalDnPath", nf.LocalDnPath(testCluster, testDn),
		"/var/tmp/dn-"+c+"-0000000000000003")
	checkName(t, "LocalSidePath", nf.LocalSidePath(testCluster, testDn, testSp, testSide),
		"/var/tmp/side-"+c+"-0000000000000003-0000000000000011-0000000000000016")
	checkName(t, "LocalCnPath", nf.LocalCnPath(testCluster, testCn),
		"/var/tmp/cn-"+c+"-0000000000000005")
	checkName(t, "LocalCntlrPath", nf.LocalCntlrPath(testCluster, testCn, testSp, 0x61),
		"/var/tmp/cntlr-"+c+"-0000000000000005-0000000000000011-0000000000000061")
	checkName(t, "LocalMigrBmPath",
		nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigr, 3),
		"/var/tmp/migr-bm-"+c+"-0000000000000003-0000000000000011-000000000000001e-03")
	checkName(t, "LocalCloneBmPath",
		nf.LocalCloneBmPath(testCluster, testCn, testSp, testClone, 0x0a),
		"/var/tmp/clone-bm-"+c+"-0000000000000005-0000000000000011-0000000000000041-0a")

	// --local-store overrides the prefix (architecture.md §13).
	custom := NewNameFmt("/srv/dnv")
	if got := custom.LocalDnPath(testCluster, testDn); !strings.HasPrefix(got, "/srv/dnv/dn-") {
		t.Errorf("custom local store prefix ignored: %q", got)
	}
}

// The whole point of folding creation_epoch into cluster_id (§4): a recreated
// cluster produces a disjoint set of names.
func TestNamesAreClusterScoped(t *testing.T) {
	nf := NewNameFmt("")
	for _, pair := range [][2]string{
		{nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn),
			nf.DnLinearName(testCluster+1, testDn, testSp, testSide, testCn)},
		{nf.DnVgName(testCluster, testDn), nf.DnVgName(testCluster+1, testDn)},
		{nf.CnHostNqn(testCluster, testCn), nf.CnHostNqn(testCluster+1, testCn)},
		{nf.LocalCnPath(testCluster, testCn), nf.LocalCnPath(testCluster+1, testCn)},
	} {
		if pair[0] == pair[1] {
			t.Errorf("two clusters share the name %q", pair[0])
		}
	}
}
