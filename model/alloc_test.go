package model

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// putDnCap writes one DN capacity key. The allocator reads nothing else: the
// index alone carries free_ext_cnt (key) and location and used metadata units
// (value), which is what makes a scan need no point reads (architecture.md,
// Capacity index keys; Metadata units; [D5]). The value it writes has no
// used_meta_units, which reads as nothing used.
func putDnCap(
	t *testing.T,
	cli *etcdutil.Client,
	cid uint64,
	binIdx uint32,
	freeExt uint64,
	addrPort string,
	location string,
) {
	t.Helper()
	mustPut(
		t, cli,
		DnCapacityKey(cid, binIdx, freeExt, addrPort),
		&pb.DnCapacity{Location: location},
	)
}

// putDnCapUsed is putDnCap with a value whose used_meta_units is used, built
// the way MaintainDnCapacity builds it.
func putDnCapUsed(
	t *testing.T,
	cli *etcdutil.Client,
	cid uint64,
	binIdx uint32,
	freeExt uint64,
	addrPort string,
	location string,
	used uint64,
) {
	t.Helper()
	mustPut(
		t, cli,
		DnCapacityKey(cid, binIdx, freeExt, addrPort),
		DnCapacityValue(&pb.DnConf{Location: location, UsedMetaUnits: used}),
	)
}

// putCnCap writes one CN capacity key, with the location as its value.
func putCnCap(
	t *testing.T,
	cli *etcdutil.Client,
	cid uint64,
	freeExt uint64,
	addrPort string,
	location string,
) {
	t.Helper()
	mustPut(
		t, cli,
		CnCapacityKey(cid, freeExt, addrPort),
		&pb.CnCapacity{Location: location},
	)
}

// addrsOf renders a candidate list for comparison.
func addrsOf(cands []Cand) []string {
	addrs := make([]string, 0, len(cands))
	for _, cand := range cands {
		addrs = append(addrs, cand.AddrPort)
	}
	return addrs
}

// TestFindDnCandidatesBinWalk exercises the DN candidate walk end to end
// (MD5, MD9; architecture.md, Finding DN candidates):
// the too-small bin is skipped, each bin is descending, a bin is abandoned at
// the first DN below candExt, and the remaining bins are still walked.
func TestFindDnCandidatesBinWalk(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	// The stored 0/4/8/12 ladder gives levels 1 / 16 / 256 / 4096, which is
	// what the bin indexes below are computed under.
	putDnCap(t, cli, cid, 0, 10, "dn-a:9000", "r0")
	putDnCap(t, cli, cid, 1, 30, "dn-c:9000", "r2")
	putDnCap(t, cli, cid, 1, 20, "dn-b:9000", "r1")
	putDnCap(t, cli, cid, 1, 18, "dn-d:9000", "r3")
	putDnCap(t, cli, cid, 2, 300, "dn-e:9000", "r4")
	putDnCap(t, cli, cid, 3, 5000, "dn-f:9000", "r5")

	cc := testClusterConf()
	cands, err := FindDnCandidates(ctx, cli, cid, cc, 20, 0, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	// bin 0 is skipped (level1 = 16 <= 20); bin 1 runs 30, 20 and stops at
	// 18; bins 2 and 3 follow.
	want := []string{"dn-c:9000", "dn-b:9000", "dn-e:9000", "dn-f:9000"}
	if !equalStrings(addrsOf(cands), want) {
		t.Errorf("candidates = %v, want %v", addrsOf(cands), want)
	}
	if cands[0].FreeExt != 30 || cands[0].BinIdx != 1 ||
		cands[0].Location != "r2" {
		t.Errorf("first candidate = %+v", cands[0])
	}
	if cands[3].FreeExt != 5000 || cands[3].BinIdx != 3 {
		t.Errorf("last candidate = %+v", cands[3])
	}

	// The walk returns as soon as it has enough.
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 20, 0, 2, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-c:9000", "dn-b:9000"},
	) {
		t.Errorf("candCnt = 2 gave %v", addrsOf(cands))
	}

	// A small request starts at bin 0 and sees everything.
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 1, 0, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if len(cands) != 6 || cands[0].AddrPort != "dn-a:9000" {
		t.Errorf("candExt = 1 gave %v", addrsOf(cands))
	}

	// Nothing is large enough: the caller gets an empty list, not an error
	// (architecture.md, Per-operation allocation, turns that into
	// RESOURCE_EXHAUSTED).
	cands, err = FindDnCandidates(
		ctx, cli, cid, cc, 100000, 0, 1, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("oversized request gave %v", addrsOf(cands))
	}
}

// TestFindDnCandidatesLocationDedupe checks the taken-locations rule of
// architecture.md, Finding DN candidates: one
// allocation round never returns two DNs from the same failure domain.
func TestFindDnCandidatesLocationDedupe(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "rack0")
	putDnCap(t, cli, cid, 1, 30, "dn-b:9000", "rack0")
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "rack1")
	putDnCap(t, cli, cid, 2, 300, "dn-d:9000", "rack0")

	cands, err := FindDnCandidates(
		ctx, cli, cid, testClusterConf(), 20, 0, 10, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	want := []string{"dn-a:9000", "dn-c:9000"}
	if !equalStrings(addrsOf(cands), want) {
		t.Errorf("candidates = %v, want %v", addrsOf(cands), want)
	}
}

// TestFindDnCandidatesLists checks the two NodeSelector rules of
// architecture.md, Finding DN candidates: the
// black list always excludes, and a non-empty white list excludes everything
// it does not name.
func TestFindDnCandidatesLists(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "r0")
	putDnCap(t, cli, cid, 1, 30, "dn-b:9000", "r1")
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "r2")
	cc := testClusterConf()

	cands, err := FindDnCandidates(
		ctx, cli, cid, cc, 20, 0, 10, []string{"dn-a:9000"}, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-b:9000", "dn-c:9000"},
	) {
		t.Errorf("black list gave %v", addrsOf(cands))
	}

	cands, err = FindDnCandidates(
		ctx, cli, cid, cc, 20, 0, 10, nil,
		[]string{"dn-c:9000", "dn-a:9000"}, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-a:9000", "dn-c:9000"},
	) {
		t.Errorf("white list gave %v", addrsOf(cands))
	}

	// The black list wins over the white list.
	cands, err = FindDnCandidates(
		ctx, cli, cid, cc, 20, 0, 10,
		[]string{"dn-a:9000"},
		[]string{"dn-a:9000", "dn-b:9000"},
		nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(addrsOf(cands), []string{"dn-b:9000"}) {
		t.Errorf("black over white gave %v", addrsOf(cands))
	}

	// candCnt <= 0 asks for nothing and scans nothing.
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 20, 0, 0, nil, nil, nil)
	if err != nil || len(cands) != 0 {
		t.Errorf("candCnt = 0 gave %v, %v", addrsOf(cands), err)
	}
}

// antiAffineFixture is the three-DN cluster the two-tier tests use: the
// group's own DN (dn-a) and a second DN in its failure domain (dn-b), plus one
// DN in another domain (dn-c). It is the shape tier 1 of architecture.md,
// Per-operation allocation, exists for — a
// black list of addr_ports alone leaves dn-b eligible. No two of its DNs tie
// on free count, so no scan of it draws, and two scans of it compare equal.
func antiAffineFixture(t *testing.T, cli *etcdutil.Client, cid uint64) {
	t.Helper()
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "rack0")
	putDnCap(t, cli, cid, 1, 30, "dn-b:9000", "rack0")
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "rack1")
}

// TestFindDnCandidatesExcludeLocs pins tier 1 of architecture.md,
// Per-operation allocation: the caller's failure
// domains are in the location set before the walk starts, so a DN that merely
// shares a domain with a black-listed one is skipped too — which black-listing
// the addr_port alone never did, because a black-listed DN is skipped before
// its location is recorded.
func TestFindDnCandidatesExcludeLocs(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	antiAffineFixture(t, cli, cid)
	cc := testClusterConf()
	black := []string{"dn-a:9000"}

	cands, err := FindDnCandidates(
		ctx, cli, cid, cc, 20, 0, 10, black, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-b:9000", "dn-c:9000"},
	) {
		t.Errorf("no exclusion gave %v", addrsOf(cands))
	}

	cands, err = FindDnCandidates(
		ctx, cli, cid, cc, 20, 0, 10, black, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(addrsOf(cands), []string{"dn-c:9000"}) {
		t.Errorf("excludeLocs gave %v", addrsOf(cands))
	}

	// A nil exclusion changes nothing at all, which is what
	// CreateStoragePool, GrowSlice and the AR6 grow rely on.
	plain, err := FindDnCandidates(ctx, cli, cid, cc, 20, 0, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	empty, err := FindDnCandidates(
		ctx, cli, cid, cc, 20, 0, 10, nil, nil, []string{},
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !reflect.DeepEqual(plain, empty) {
		t.Errorf("empty excludeLocs = %v, want %v", empty, plain)
	}
}

// TestFindDnCandidatesAntiAffine pins the two tiers of architecture.md,
// Per-operation allocation: tier 1 alone when
// it has enough, a tier-2 rescan without the location exclusion when it has
// not — the DN black list applying to both — and a nil exclusion that is the
// plain scan with no second scan at all.
func TestFindDnCandidatesAntiAffine(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	antiAffineFixture(t, cli, cid)
	cc := testClusterConf()
	black := []string{"dn-a:9000"}

	// Tier 1 has what was asked for: the other-domain DN, and no fallback.
	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 0, 1, 1, black, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if tier2 || !equalStrings(addrsOf(cands), []string{"dn-c:9000"}) {
		t.Errorf("tier 1 = %v, tier2 = %v", addrsOf(cands), tier2)
	}

	// Every domain of the cluster is excluded, so tier 1 comes up empty and
	// tier 2 places anyway — on a DN of the group's own domain, but never on
	// a black-listed one.
	cands, tier2, err = FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 0, 10, 1, black, nil, []string{"rack0", "rack1"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if !tier2 {
		t.Errorf("tier 2 did not run: %v", addrsOf(cands))
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-b:9000", "dn-c:9000"},
	) {
		t.Errorf("tier 2 = %v", addrsOf(cands))
	}

	// No exclusion: tier 1 is the plain scan, byte for byte, and short of
	// candCnt it is still the answer — a second identical scan would find
	// nothing new.
	plain, err := FindDnCandidates(ctx, cli, cid, cc, 20, 0, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	cands, tier2, err = FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 0, 10, 1, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if tier2 || !reflect.DeepEqual(cands, plain) {
		t.Errorf("nil excludeLocs = %v (tier2 %v), want %v",
			cands, tier2, plain)
	}
}

// TestFindDnCandidatesAntiAffineTriggerIsRequiredCnt is the tier trigger of
// architecture.md, Per-operation allocation,
// itself: tier 2 fires on requiredCnt — the DNs the caller must actually place
// — and NEVER on candCnt, the oversampled scan width (RequiredCnt ×
// dn_batch_size, default 16). A scan returns at most one candidate per
// location, so under the candCnt reading tier 1 would be discarded in every
// cluster with fewer than a batch of out-of-domain domains and the exclusion
// would be inert exactly where it is needed.
//
// The case below is that shape in miniature: tier 1 returns one candidate for
// a candCnt of 10, which is short of candCnt and enough for the caller.
func TestFindDnCandidatesAntiAffineTriggerIsRequiredCnt(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	antiAffineFixture(t, cli, cid)
	cc := testClusterConf()

	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 0, 10, 1,
		[]string{"dn-a:9000"}, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if tier2 {
		t.Errorf("tier 2 ran on a tier 1 that had the one DN asked for")
	}
	if !equalStrings(addrsOf(cands), []string{"dn-c:9000"}) {
		t.Errorf("candidates = %v, want only the other-domain DN "+
			"(dn-b shares rack0)", addrsOf(cands))
	}

	// One more than tier 1 can offer, and tier 2 does run: the trigger is a
	// comparison against requiredCnt, not a constant false.
	cands, tier2, err = FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 0, 10, 2,
		[]string{"dn-a:9000"}, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if !tier2 {
		t.Errorf("tier 2 did not run for requiredCnt = 2: %v", addrsOf(cands))
	}
}

// TestFindDnCandidatesAntiAffineMergesTier1 pins the merge of architecture.md,
// Per-operation allocation: tier 2
// walks free-count-descending and stops at candCnt, and the excluded domains
// usually hold the fullest DNs — so a tier 2 that REPLACED tier 1 could drop a
// distinct-domain DN tier 1 had found. Tier 1's entries stay at the head and
// tier 2 only appends DNs of locations not already there.
func TestFindDnCandidatesAntiAffineMergesTier1(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	// rack1's only DN is the emptiest in the cluster, so tier 2's two fullest
	// candidates (dn-e, dn-b) crowd it out of a candCnt of 2.
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "rack0")
	putDnCap(t, cli, cid, 1, 35, "dn-e:9000", "rack2")
	putDnCap(t, cli, cid, 1, 30, "dn-b:9000", "rack0")
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "rack1")
	cc := testClusterConf()

	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 0, 2, 2,
		[]string{"dn-a:9000"}, nil, []string{"rack0", "rack2"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if !tier2 {
		t.Fatalf("tier 2 did not run: %v", addrsOf(cands))
	}
	// dn-c is tier 1's find and heads the list; dn-e is tier 2's fullest
	// addition; dn-b is past candCnt and dn-a is black-listed in both tiers.
	if !equalStrings(addrsOf(cands), []string{"dn-c:9000", "dn-e:9000"}) {
		t.Errorf("merged = %v, want tier 1's dn-c kept at the head",
			addrsOf(cands))
	}
}

// TestFindCnCandidates exercises the scan of architecture.md, Finding CN
// candidates: no bins, descending, the same
// list and location rules, plus the per-SP exclusion (MD5, MD9).
func TestFindCnCandidates(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putCnCap(t, cli, cid, 50, "cn-a:9000", "r0")
	putCnCap(t, cli, cid, 40, "cn-b:9000", "r1")
	putCnCap(t, cli, cid, 30, "cn-c:9000", "r0")
	putCnCap(t, cli, cid, 20, "cn-d:9000", "r3")
	putCnCap(t, cli, cid, 10, "cn-e:9000", "r4")

	// cn-b already hosts a cntlr of this SP; cn-c repeats cn-a's location;
	// cn-e is below candExt and ends the scan.
	cands, err := FindCnCandidates(
		ctx, cli, cid, 20, 10, nil, nil, []string{"cn-b:9000"}, nil,
	)
	if err != nil {
		t.Fatalf("FindCnCandidates: %v", err)
	}
	want := []string{"cn-a:9000", "cn-d:9000"}
	if !equalStrings(addrsOf(cands), want) {
		t.Errorf("candidates = %v, want %v", addrsOf(cands), want)
	}
	if cands[0].FreeExt != 50 || cands[0].BinIdx != 0 {
		t.Errorf("first candidate = %+v (CNs have no bins)", cands[0])
	}

	cands, err = FindCnCandidates(
		ctx, cli, cid, 10, 10,
		[]string{"cn-a:9000"},
		[]string{"cn-a:9000", "cn-b:9000", "cn-e:9000"},
		nil, nil,
	)
	if err != nil {
		t.Fatalf("FindCnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"cn-b:9000", "cn-e:9000"},
	) {
		t.Errorf("lists gave %v", addrsOf(cands))
	}
}

// TestFindCnCandidatesAntiAffine pins the cntlr half of the two tiers of
// architecture.md, Per-operation allocation:
// tier 1 seeds the SP's cntlr locations, so a CN that merely shares a domain
// with one of the SP's CNs is skipped; tier 2 rescans without them only when
// tier 1 found no CN at all — the one CN every cntlr pick places — and it
// relaxes the domain and nothing else: the black list, the white list and
// the SP's own CNs bind both tiers. A nil exclusion is the plain scan, and an
// empty plain scan is not rescanned.
func TestFindCnCandidatesAntiAffine(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	// cn-a hosts a cntlr of the SP; cn-b shares its domain; cn-c and cn-d are
	// the other domain.
	putCnCap(t, cli, cid, 50, "cn-a:9000", "rack0")
	putCnCap(t, cli, cid, 40, "cn-b:9000", "rack0")
	putCnCap(t, cli, cid, 30, "cn-c:9000", "rack1")
	putCnCap(t, cli, cid, 20, "cn-d:9000", "rack1")
	spCn := []string{"cn-a:9000"}

	plain, err := FindCnCandidates(
		ctx, cli, cid, 20, 10, nil, nil, spCn, nil,
	)
	if err != nil {
		t.Fatalf("FindCnCandidates: %v", err)
	}
	if !equalStrings(addrsOf(plain), []string{"cn-b:9000", "cn-c:9000"}) {
		t.Errorf("no exclusion gave %v", addrsOf(plain))
	}

	// Tier 1 has the one CN a pick needs: the other domain's, alone.
	cands, tier2, err := FindCnCandidatesAntiAffine(
		ctx, cli, cid, 20, 10, nil, nil, spCn, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindCnCandidatesAntiAffine: %v", err)
	}
	if tier2 || !equalStrings(addrsOf(cands), []string{"cn-c:9000"}) {
		t.Errorf("tier 1 = %v, tier2 = %v", addrsOf(cands), tier2)
	}

	// The SP already has a cntlr in each domain, so tier 1 is empty and tier
	// 2 places anyway — in a domain the SP uses, but never on one of its CNs
	// and never on a black-listed one.
	cands, tier2, err = FindCnCandidatesAntiAffine(
		ctx, cli, cid, 20, 10,
		[]string{"cn-d:9000"}, nil,
		[]string{"cn-a:9000", "cn-c:9000"}, []string{"rack0", "rack1"},
	)
	if err != nil {
		t.Fatalf("FindCnCandidatesAntiAffine: %v", err)
	}
	if !tier2 || !equalStrings(addrsOf(cands), []string{"cn-b:9000"}) {
		t.Errorf("tier 2 = %v, tier2 = %v", addrsOf(cands), tier2)
	}

	// The white list binds tier 2 too: cn-c is the fuller CN of rack1 but is
	// not white-listed, so the rescan reaches past it to cn-d. A tier 2 that
	// dropped the white list would hand back cn-c — a cntlr placed outside
	// the operator's cn_selector because every CN inside it sat in an
	// excluded domain.
	cands, tier2, err = FindCnCandidatesAntiAffine(
		ctx, cli, cid, 20, 10,
		nil, []string{"cn-a:9000", "cn-b:9000", "cn-d:9000"},
		spCn, []string{"rack0", "rack1"},
	)
	if err != nil {
		t.Fatalf("FindCnCandidatesAntiAffine: %v", err)
	}
	if !tier2 ||
		!equalStrings(addrsOf(cands), []string{"cn-b:9000", "cn-d:9000"}) {
		t.Errorf("white-listed tier 2 = %v, tier2 = %v, want "+
			"[cn-b:9000 cn-d:9000] from tier 2", addrsOf(cands), tier2)
	}

	// No exclusion: tier 1 is the plain scan, byte for byte, and an empty one
	// stays empty — a second identical scan would find nothing new.
	cands, tier2, err = FindCnCandidatesAntiAffine(
		ctx, cli, cid, 20, 10, nil, nil, spCn, nil,
	)
	if err != nil {
		t.Fatalf("FindCnCandidatesAntiAffine: %v", err)
	}
	if tier2 || !reflect.DeepEqual(cands, plain) {
		t.Errorf("nil excludeLocs = %v (tier2 %v), want %v",
			cands, tier2, plain)
	}
	cands, tier2, err = FindCnCandidatesAntiAffine(
		ctx, cli, cid, 100, 10, nil, nil, spCn, nil,
	)
	if err != nil {
		t.Fatalf("FindCnCandidatesAntiAffine: %v", err)
	}
	if tier2 || len(cands) != 0 {
		t.Errorf("an empty plain scan = %v (tier2 %v), want nothing and "+
			"no rescan", addrsOf(cands), tier2)
	}
}

// TestFindDnCandidatesMetaUnits pins the metadata-room skip of
// architecture.md, Metadata units, on the DN walk. A DN whose capacity value
// leaves it fewer than candUnits free units is skipped with a continue, so
// the next DN of the same bin is still returned. It is skipped before its
// location is recorded, so a DN of the same location that has room is still
// taken. A value with no used_meta_units counts as nothing used, and a
// candUnits of zero skips nothing.
func TestFindDnCandidatesMetaUnits(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	const ceiling = common.DnCloneMetaUnitCnt
	// All in bin 1, fullest first: dn-a has no free unit, dn-b has five, and
	// dn-c's value carries no count at all.
	putDnCapUsed(t, cli, cid, 1, 40, "dn-a:9000", "rack0", ceiling)
	putDnCapUsed(t, cli, cid, 1, 30, "dn-b:9000", "rack0", ceiling-5)
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "rack1")
	cc := testClusterConf()

	cases := []struct {
		name      string
		candUnits uint64
		want      []string
	}{
		// dn-b shares dn-a's location, which dn-a takes first.
		{"no units asked", 0, []string{"dn-a:9000", "dn-c:9000"}},
		// dn-a is skipped before it takes rack0, and the walk goes on to
		// the next DN of its bin.
		{"one unit", 1, []string{"dn-b:9000", "dn-c:9000"}},
		{"exactly dn-b's room", 5, []string{"dn-b:9000", "dn-c:9000"}},
		{"one more than dn-b's room", 6, []string{"dn-c:9000"}},
		// A value with no count reads as a whole free area.
		{"a whole area", ceiling, []string{"dn-c:9000"}},
		{"more than an area", ceiling + 1, nil},
	}
	for _, tc := range cases {
		cands, err := FindDnCandidates(
			ctx, cli, cid, cc, 20, tc.candUnits, 10, nil, nil, nil,
		)
		if err != nil {
			t.Fatalf("%s: FindDnCandidates: %v", tc.name, err)
		}
		if !equalStrings(addrsOf(cands), tc.want) {
			t.Errorf("%s: candidates = %v, want %v",
				tc.name, addrsOf(cands), tc.want)
		}
	}
}

// TestFindDnCandidatesAntiAffineKeepsTheUnitFloor pins candUnits on both
// tiers of architecture.md, Per-operation allocation: tier 2 relaxes the
// failure domain and nothing else. dn-a is the group's own DN, dn-b and dn-c
// have no free unit, and dn-d, in the group's own domain, is the one DN with
// room. Tier 1 finds nothing, since dn-c is full and dn-d sits in the
// excluded domain, and tier 2 finds dn-d. A tier 1 without the floor would
// answer dn-c alone, and a tier 2 without it dn-b and dn-c.
func TestFindDnCandidatesAntiAffineKeepsTheUnitFloor(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	const ceiling = common.DnCloneMetaUnitCnt
	putDnCap(t, cli, cid, 1, 50, "dn-a:9000", "rack0")
	putDnCapUsed(t, cli, cid, 1, 40, "dn-b:9000", "rack0", ceiling)
	putDnCapUsed(t, cli, cid, 1, 30, "dn-c:9000", "rack1", ceiling)
	putDnCap(t, cli, cid, 1, 20, "dn-d:9000", "rack0")

	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, testClusterConf(), 20, 1, 10, 1,
		[]string{"dn-a:9000"}, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if !tier2 || !equalStrings(addrsOf(cands), []string{"dn-d:9000"}) {
		t.Errorf("candidates = %v (tier2 %v), want [dn-d:9000] from tier 2",
			addrsOf(cands), tier2)
	}
}

// TestScansTakeCandExtBeforeCandUnits pins the order of the two uint64
// arguments the two DN scans take, candExt and then candUnits: a call that
// swapped them would compile and pick by the wrong numbers. dn-a has the
// extents asked for and too few units for the swapped reading; dn-b has the
// units and too few extents for the right one.
func TestScansTakeCandExtBeforeCandUnits(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putDnCapUsed(t, cli, cid, 1, 40, "dn-a:9000", "rack0",
		common.DnCloneMetaUnitCnt-5)
	putDnCap(t, cli, cid, 1, 20, "dn-b:9000", "rack1")
	cc := testClusterConf()
	// 30 extents and 3 units: only dn-a qualifies. Read the other way round,
	// 3 extents and 30 units, only dn-b would.
	const ext, units = 30, 3
	check := func(what string, cands []Cand, err error, want string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !equalStrings(addrsOf(cands), []string{want}) {
			t.Errorf("%s = %v, want [%s]", what, addrsOf(cands), want)
		}
	}

	cands, err := FindDnCandidates(
		ctx, cli, cid, cc, ext, units, 10, nil, nil, nil,
	)
	check("FindDnCandidates", cands, err, "dn-a:9000")
	cands, _, err = FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, ext, units, 10, 1, nil, nil, nil,
	)
	check("FindDnCandidatesAntiAffine", cands, err, "dn-a:9000")

	// The fixture tells the two readings apart: swapped, the scan answers
	// dn-b.
	cands, err = FindDnCandidates(
		ctx, cli, cid, cc, units, ext, 10, nil, nil, nil,
	)
	check("swapped FindDnCandidates", cands, err, "dn-b:9000")
}

// tieDraws is how many scans a test of the draw among equals runs against one
// fixture. A walk that offers a DN it must not with chance one half per scan
// passes every scan with chance 2^-64, and a right walk leaves one of two
// equals unseen with about the same chance.
const tieDraws = 64

// TestFindDnCandidatesDrawsAmongEquals pins the draw of architecture.md,
// Finding DN candidates: the DNs of one location that tie on the free count
// of the first DN the walk would take there, and that the lists and the unit
// floor let through, are drawn among uniformly; the location is taken at the
// end of their run, and the candidates keep the order the walk met their
// locations. In bin 1 at free 40, in key order, rack1 has dn-y (no free
// unit), dn-x (black-listed) and dn-c, then rack0 has dn-b, then rack1 has
// dn-a; dn-d (bin 1, free 30) and dn-e (bin 2) are rack1 DNs of other free
// counts.
func TestFindDnCandidatesDrawsAmongEquals(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putDnCapUsed(t, cli, cid, 1, 40, "dn-y:9000", "rack1",
		common.DnCloneMetaUnitCnt)
	putDnCap(t, cli, cid, 1, 40, "dn-x:9000", "rack1")
	putDnCap(t, cli, cid, 1, 40, "dn-c:9000", "rack1")
	putDnCap(t, cli, cid, 1, 40, "dn-b:9000", "rack0")
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "rack1")
	putDnCap(t, cli, cid, 1, 30, "dn-d:9000", "rack1")
	putDnCap(t, cli, cid, 2, 300, "dn-e:9000", "rack1")
	cc := testClusterConf()
	equals := []string{"dn-a:9000", "dn-c:9000"}
	second := Cand{
		AddrPort: "dn-b:9000", Location: "rack0", FreeExt: 40, BinIdx: 1,
	}

	seen := map[string]int{}
	for i := 0; i < tieDraws; i++ {
		cands, err := FindDnCandidates(
			ctx, cli, cid, cc, 20, 1, 10, []string{"dn-x:9000"}, nil, nil,
		)
		if err != nil {
			t.Fatalf("FindDnCandidates: %v", err)
		}
		if len(cands) != 2 {
			t.Fatalf("scan %d: candidates = %v, want one rack1 DN and "+
				"dn-b", i, addrsOf(cands))
		}
		first := cands[0]
		if !slices.Contains(equals, first.AddrPort) ||
			first.Location != "rack1" ||
			first.FreeExt != 40 || first.BinIdx != 1 {
			t.Fatalf("scan %d: first candidate = %+v, want dn-a or dn-c, "+
				"rack1's equals at 40 in bin 1", i, first)
		}
		if cands[1] != second {
			t.Fatalf("scan %d: second candidate = %+v, want %+v",
				i, cands[1], second)
		}
		seen[first.AddrPort]++
	}
	if len(seen) != len(equals) {
		t.Errorf("%d scans offered only %v of rack1's equals %v",
			tieDraws, seen, equals)
	}
}

// TestFindDnCandidatesDrawEndsWithTheRun pins where a run ends and is drawn
// (architecture.md, Finding DN candidates): at the first key below candExt,
// whose break must not lose the run before it; at the end of a bin; and at
// candCnt, which is counted only when the run that reaches it ends, so a
// candCnt of one still draws among the first location's equals.
func TestFindDnCandidatesDrawEndsWithTheRun(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cc := testClusterConf()
	cases := []struct {
		name    string
		put     func(cid uint64)
		candCnt int
		// equals heads every answer and each of them heads some answer;
		// rest follows it in every answer.
		equals []string
		rest   []string
	}{
		{"the break ends the run", func(cid uint64) {
			putDnCap(t, cli, cid, 1, 25, "dn-q:9000", "rack0")
			putDnCap(t, cli, cid, 1, 25, "dn-p:9000", "rack0")
			putDnCap(t, cli, cid, 1, 18, "dn-r:9000", "rack1")
		}, 10, []string{"dn-p:9000", "dn-q:9000"}, nil},
		{"the end of a bin ends the run", func(cid uint64) {
			putDnCap(t, cli, cid, 1, 25, "dn-q:9000", "rack0")
			putDnCap(t, cli, cid, 1, 25, "dn-p:9000", "rack0")
			putDnCap(t, cli, cid, 2, 300, "dn-s:9000", "rack1")
		}, 10, []string{"dn-p:9000", "dn-q:9000"}, []string{"dn-s:9000"}},
		{"candCnt waits for the end of the run", func(cid uint64) {
			putDnCap(t, cli, cid, 1, 40, "dn-c:9000", "rack0")
			putDnCap(t, cli, cid, 1, 40, "dn-b:9000", "rack1")
			putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "rack0")
		}, 1, []string{"dn-a:9000", "dn-c:9000"}, nil},
	}
	for _, tc := range cases {
		cid := testCid(t)
		tc.put(cid)
		seen := map[string]int{}
		for i := 0; i < tieDraws; i++ {
			cands, err := FindDnCandidates(
				ctx, cli, cid, cc, 20, 0, tc.candCnt, nil, nil, nil,
			)
			if err != nil {
				t.Fatalf("%s: FindDnCandidates: %v", tc.name, err)
			}
			if len(cands) != 1+len(tc.rest) ||
				!slices.Contains(tc.equals, cands[0].AddrPort) ||
				!equalStrings(addrsOf(cands[1:]), tc.rest) {
				t.Fatalf("%s: scan %d: candidates = %v, want one of %v "+
					"and then %v", tc.name, i, addrsOf(cands), tc.equals,
					tc.rest)
			}
			seen[cands[0].AddrPort]++
		}
		if len(seen) != len(tc.equals) {
			t.Errorf("%s: %d scans offered only %v of the equals %v",
				tc.name, tieDraws, seen, tc.equals)
		}
	}
}

// TestFindDnCandidatesAntiAffineMergesOnePerLocation pins the merge of
// architecture.md, Per-operation allocation, under the draw: tier 1 and
// tier 2 each draw among rack1's equals on their own, so the merge leaves
// out a tier-2 DN whose location tier 1 offers, and the list keeps one DN
// per location whichever DN each tier drew.
func TestFindDnCandidatesAntiAffineMergesOnePerLocation(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putDnCap(t, cli, cid, 1, 50, "dn-r:9000", "rack0")
	putDnCap(t, cli, cid, 1, 40, "dn-q:9000", "rack1")
	putDnCap(t, cli, cid, 1, 40, "dn-p:9000", "rack1")
	cc := testClusterConf()
	equals := []string{"dn-p:9000", "dn-q:9000"}

	seen := map[string]int{}
	for i := 0; i < tieDraws; i++ {
		cands, tier2, err := FindDnCandidatesAntiAffine(
			ctx, cli, cid, cc, 20, 0, 10, 2, nil, nil, []string{"rack0"},
		)
		if err != nil {
			t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
		}
		if !tier2 {
			t.Fatalf("scan %d: tier 2 did not run: %v", i, addrsOf(cands))
		}
		if len(cands) != 2 || !slices.Contains(equals, cands[0].AddrPort) ||
			cands[1].AddrPort != "dn-r:9000" {
			t.Fatalf("scan %d: merged = %v, want tier 1's rack1 DN and "+
				"then dn-r, one DN per location", i, addrsOf(cands))
		}
		seen[cands[0].AddrPort]++
	}
	if len(seen) != len(equals) {
		t.Errorf("%d scans put only %v of rack1's equals %v at the head",
			tieDraws, seen, equals)
	}
}

// TestFindCnCandidatesKeepsKeyOrderAmongEquals pins that the CN walk does not
// draw (architecture.md, Finding CN candidates): of two CNs of one location
// at one free count it always takes the first it meets, cn-c, and never
// cn-a.
func TestFindCnCandidatesKeepsKeyOrderAmongEquals(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putCnCap(t, cli, cid, 40, "cn-c:9000", "rack0")
	putCnCap(t, cli, cid, 40, "cn-b:9000", "rack1")
	putCnCap(t, cli, cid, 40, "cn-a:9000", "rack0")
	want := []string{"cn-c:9000", "cn-b:9000"}

	for i := 0; i < tieDraws; i++ {
		cands, err := FindCnCandidates(
			ctx, cli, cid, 20, 10, nil, nil, nil, nil,
		)
		if err != nil {
			t.Fatalf("FindCnCandidates: %v", err)
		}
		if !equalStrings(addrsOf(cands), want) {
			t.Fatalf("scan %d: candidates = %v, want %v",
				i, addrsOf(cands), want)
		}
	}
}

// TestPickRandom checks the pick of architecture.md, Per-operation allocation:
// n distinct entries drawn from the
// batch, the input untouched, and the degenerate bounds.
func TestPickRandom(t *testing.T) {
	cands := []Cand{
		{AddrPort: "dn-a:9000"},
		{AddrPort: "dn-b:9000"},
		{AddrPort: "dn-c:9000"},
		{AddrPort: "dn-d:9000"},
	}
	original := append([]Cand(nil), cands...)
	for i := 0; i < 100; i++ {
		picked := PickRandom(cands, 2)
		if len(picked) != 2 {
			t.Fatalf("PickRandom(_, 2) returned %d entries", len(picked))
		}
		if picked[0].AddrPort == picked[1].AddrPort {
			t.Fatalf("PickRandom picked %q twice", picked[0].AddrPort)
		}
	}
	if !equalStrings(addrsOf(cands), addrsOf(original)) {
		t.Errorf("PickRandom mutated its input: %v", addrsOf(cands))
	}
	if got := PickRandom(cands, 0); len(got) != 0 {
		t.Errorf("PickRandom(_, 0) = %v", addrsOf(got))
	}
	if got := PickRandom(cands, -1); len(got) != 0 {
		t.Errorf("PickRandom(_, -1) = %v", addrsOf(got))
	}
	if got := PickRandom(nil, 3); len(got) != 0 {
		t.Errorf("PickRandom(nil, 3) = %v", addrsOf(got))
	}
	if got := PickRandom(cands, 10); len(got) != len(cands) {
		t.Errorf("PickRandom(_, 10) = %v", addrsOf(got))
	}
	// Over many draws every candidate is eventually picked first: the pick
	// is what spreads concurrent allocations (architecture.md, Per-operation
	// allocation).
	seen := make(map[string]struct{})
	for i := 0; i < 500; i++ {
		seen[PickRandom(cands, 1)[0].AddrPort] = struct{}{}
	}
	if len(seen) != len(cands) {
		t.Errorf("PickRandom never picked %d of the candidates first",
			len(cands)-len(seen))
	}
}
