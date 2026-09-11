package model

import (
	"context"
	"reflect"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// putDnCap writes one DN capacity key. The allocator reads nothing else: the
// index alone carries free_ext_cnt (key) and location (value), which is what
// makes a scan need no point reads (§5.6, [D5]).
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

// putCnCap writes one CN capacity key.
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

// TestFindDnCandidatesBinWalk exercises the §6.3 walk end to end (MD5, MD9):
// the too-small bin is skipped, each bin is descending, a bin is abandoned at
// the first DN below candExt, and the remaining bins are still walked.
func TestFindDnCandidatesBinWalk(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	// Default bins: levels 1 / 16 / 256 / 4096.
	putDnCap(t, cli, cid, 0, 10, "dn-a:9000", "r0")
	putDnCap(t, cli, cid, 1, 30, "dn-c:9000", "r2")
	putDnCap(t, cli, cid, 1, 20, "dn-b:9000", "r1")
	putDnCap(t, cli, cid, 1, 18, "dn-d:9000", "r3")
	putDnCap(t, cli, cid, 2, 300, "dn-e:9000", "r4")
	putDnCap(t, cli, cid, 3, 5000, "dn-f:9000", "r5")

	cc := &pb.ClusterConf{}
	cands, err := FindDnCandidates(ctx, cli, cid, cc, 20, 10, nil, nil, nil)
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
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 20, 2, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-c:9000", "dn-b:9000"},
	) {
		t.Errorf("candCnt = 2 gave %v", addrsOf(cands))
	}

	// A small request starts at bin 0 and sees everything.
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 1, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if len(cands) != 6 || cands[0].AddrPort != "dn-a:9000" {
		t.Errorf("candExt = 1 gave %v", addrsOf(cands))
	}

	// Nothing is large enough: the caller gets an empty list, not an error
	// (§6.5 turns that into RESOURCE_EXHAUSTED).
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 100000, 1, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("oversized request gave %v", addrsOf(cands))
	}
}

// TestFindDnCandidatesLocationDedupe checks the §6.3 LocList rule: one
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
		ctx, cli, cid, &pb.ClusterConf{}, 20, 10, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	want := []string{"dn-a:9000", "dn-c:9000"}
	if !equalStrings(addrsOf(cands), want) {
		t.Errorf("candidates = %v, want %v", addrsOf(cands), want)
	}
}

// TestFindDnCandidatesLists checks the two NodeSelector rules of §6.3: the
// black list always excludes, and a non-empty white list excludes everything
// it does not name.
func TestFindDnCandidatesLists(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "r0")
	putDnCap(t, cli, cid, 1, 30, "dn-b:9000", "r1")
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "r2")
	cc := &pb.ClusterConf{}

	cands, err := FindDnCandidates(
		ctx, cli, cid, cc, 20, 10, []string{"dn-a:9000"}, nil, nil,
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
		ctx, cli, cid, cc, 20, 10, nil,
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
		ctx, cli, cid, cc, 20, 10,
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
	cands, err = FindDnCandidates(ctx, cli, cid, cc, 20, 0, nil, nil, nil)
	if err != nil || len(cands) != 0 {
		t.Errorf("candCnt = 0 gave %v, %v", addrsOf(cands), err)
	}
}

// antiAffineFixture is the three-DN cluster the two-tier tests use: the
// group's own DN (dn-a) and a second DN in its failure domain (dn-b), plus one
// DN in another domain (dn-c). It is the shape §6.5 tier 1 exists for — a
// black list of addr_ports alone leaves dn-b eligible.
func antiAffineFixture(t *testing.T, cli *etcdutil.Client, cid uint64) {
	t.Helper()
	putDnCap(t, cli, cid, 1, 40, "dn-a:9000", "rack0")
	putDnCap(t, cli, cid, 1, 30, "dn-b:9000", "rack0")
	putDnCap(t, cli, cid, 1, 20, "dn-c:9000", "rack1")
}

// TestFindDnCandidatesExcludeLocs pins §6.5 tier 1: the caller's failure
// domains are in the location set before the walk starts, so a DN that merely
// shares a domain with a black-listed one is skipped too — which black-listing
// the addr_port alone never did, because a black-listed DN is skipped before
// its location is recorded.
func TestFindDnCandidatesExcludeLocs(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	antiAffineFixture(t, cli, cid)
	cc := &pb.ClusterConf{}
	black := []string{"dn-a:9000"}

	cands, err := FindDnCandidates(ctx, cli, cid, cc, 20, 10, black, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(
		addrsOf(cands), []string{"dn-b:9000", "dn-c:9000"},
	) {
		t.Errorf("no exclusion gave %v", addrsOf(cands))
	}

	cands, err = FindDnCandidates(
		ctx, cli, cid, cc, 20, 10, black, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !equalStrings(addrsOf(cands), []string{"dn-c:9000"}) {
		t.Errorf("excludeLocs gave %v", addrsOf(cands))
	}

	// A nil exclusion changes nothing at all, which is what keeps
	// CreateStoragePool, GrowSlice and the AR6 grow on today's behavior.
	plain, err := FindDnCandidates(ctx, cli, cid, cc, 20, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	empty, err := FindDnCandidates(
		ctx, cli, cid, cc, 20, 10, nil, nil, []string{},
	)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	if !reflect.DeepEqual(plain, empty) {
		t.Errorf("empty excludeLocs = %v, want %v", empty, plain)
	}
}

// TestFindDnCandidatesAntiAffine pins the two tiers of §6.5: tier 1 alone when
// it has enough, a tier-2 rescan without the location exclusion when it has
// not — the DN black list applying to both — and a nil exclusion that is the
// plain scan with no second scan at all.
func TestFindDnCandidatesAntiAffine(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	antiAffineFixture(t, cli, cid)
	cc := &pb.ClusterConf{}
	black := []string{"dn-a:9000"}

	// Tier 1 has what was asked for: the other-domain DN, and no fallback.
	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 1, 1, black, nil, []string{"rack0"},
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
		ctx, cli, cid, cc, 20, 10, 1, black, nil, []string{"rack0", "rack1"},
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
	plain, err := FindDnCandidates(ctx, cli, cid, cc, 20, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("FindDnCandidates: %v", err)
	}
	cands, tier2, err = FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 10, 1, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if tier2 || !reflect.DeepEqual(cands, plain) {
		t.Errorf("nil excludeLocs = %v (tier2 %v), want %v",
			cands, tier2, plain)
	}
}

// TestFindDnCandidatesAntiAffineTriggerIsRequiredCnt is the §6.5 trigger
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
	cc := &pb.ClusterConf{}

	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 10, 1,
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
		ctx, cli, cid, cc, 20, 10, 2,
		[]string{"dn-a:9000"}, nil, []string{"rack0"},
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if !tier2 {
		t.Errorf("tier 2 did not run for requiredCnt = 2: %v", addrsOf(cands))
	}
}

// TestFindDnCandidatesAntiAffineMergesTier1 pins the merge of §6.5: tier 2
// walks free-count-descending and stops at candCnt, and the excluded domains
// usually hold the fullest DNs — so a tier 2 that REPLACED tier 1 could drop a
// distinct-domain DN tier 1 had found. Tier 1's entries stay at the head and
// tier 2 only appends what is not already there.
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
	cc := &pb.ClusterConf{}

	cands, tier2, err := FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 20, 2, 2,
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

// TestFindCnCandidates exercises the §6.4 scan: no bins, descending, the same
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
		ctx, cli, cid, 20, 10, nil, nil, []string{"cn-b:9000"},
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
		nil,
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

// TestPickRandom checks the §6.5 pick: n distinct entries drawn from the
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
	// is what spreads concurrent allocations (§6.5).
	seen := make(map[string]struct{})
	for i := 0; i < 500; i++ {
		seen[PickRandom(cands, 1)[0].AddrPort] = struct{}{}
	}
	if len(seen) != len(cands) {
		t.Errorf("PickRandom never picked %d of the candidates first",
			len(cands)-len(seen))
	}
}
