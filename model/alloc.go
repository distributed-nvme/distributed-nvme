package model

import (
	"context"
	"math/rand/v2"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Cand is one node the allocator offers (MD5): everything the caller needs to
// pick it and to re-validate the pick later. AddrPort and FreeExt come out of
// the capacity key itself, Location out of its value ([D5]).
//
// FreeExt is load-bearing beyond the pick: it is part of the capacity key the
// scan saw, so every MD6 op re-reads exactly DnCapacityKey(cid, BinIdx,
// FreeExt, AddrPort) inside its STM and raises ErrPrecondition{"candidate
// changed"} when that key is gone — which is how a scan outside the
// transaction is made safe (architecture.md, Storage pools, CreateStoragePool
// step 2).
//
// The key proves only that the node's free extents have not moved since the
// scan. A write that changes only a DN's used metadata units rewrites the
// value under the same key, so a pick that needs units is re-checked against
// the free units of the record its transaction reads (DnFreeMetaUnits;
// architecture.md, Metadata units). Cand itself carries no units.
type Cand struct {
	AddrPort string
	Location string
	FreeExt  uint64
	BinIdx   uint32
}

// strSet turns a black/white/exclusion list into a lookup set.
func strSet(list []string) map[string]struct{} {
	if len(list) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(list))
	for _, item := range list {
		set[item] = struct{}{}
	}
	return set
}

// excluded applies the two list rules of architecture.md, Finding DN
// candidates, to one addr_port: the black list
// always excludes, and a non-empty white list excludes everything outside it.
func excluded(
	addrPort string,
	black map[string]struct{},
	white map[string]struct{},
) bool {
	if _, ok := black[addrPort]; ok {
		return true
	}
	if white != nil {
		if _, ok := white[addrPort]; !ok {
			return true
		}
	}
	return false
}

// FindDnCandidates walks the DN capacity index for candCnt DNs with at least
// candExt free extents and candUnits free metadata units each (MD5;
// architecture.md, Finding DN candidates, verbatim).
//
// The walk starts at the smallest bin whose range can still hold candExt —
// bins with level_{b+1} <= candExt are skipped, since every DN in them is too
// small by construction — and then runs bins b…3 in order. Each bin is one
// DESCENDING range over its prefix, so DNs come largest-free first and the bin
// is abandoned as soon as a DN's free count drops below candExt. A DN is
// skipped when it is black-listed, when a non-empty white list does not name
// it, or when its location is already represented in the result or in
// excludeLocs — the caller's already-occupied failure domains (tier 1 of
// architecture.md, Per-operation allocation);
// the location rule is what gives one allocation round failure-domain
// anti-affinity for free. The walk returns as soon as candCnt candidates are
// collected, and after bin 3 returns whatever it has — the caller decides
// whether that is enough (RESOURCE_EXHAUSTED otherwise, architecture.md,
// Per-operation allocation).
//
// Among DNs that tie on free count the walk draws. The keys of one free count
// lie next to each other in a bin, a run (freeRun), ordered by addr_port, so
// taking the first DN of a location the walk meets would take a machine's
// disk nodes, which share its location, in one fixed order. The walk instead
// collects the whole run — per location not taken before the run, the DNs
// the lists and the unit floor let through — and at its end, in the order
// the walk met the locations, takes each location and appends one of its DNs
// drawn uniformly. The break and candCnt can only fall where a run ends, so
// the locations offered, their order and their free counts are those of a
// walk without the draw; only which of a location's equals is offered
// changes, and the draw never trades free space for spread.
//
// Health, flags, fullness and the side-count cap need no filtering here: a
// node that fails any of them has no capacity key at all (architecture.md,
// Capacity index keys).
//
// Metadata room is the second filter the key's value decides, beside the
// location (architecture.md, Metadata units): a DN whose value leaves it
// fewer than candUnits free units (DnCapacityFreeMetaUnits) is skipped. It
// is a continue and never a break, because the key order says nothing about
// units, and a DN it skips never joins a run, so a full DN neither takes its
// failure domain from a DN of the same location that has room nor is drawn.
// A candUnits of zero skips nothing.
//
// The scans are plain Ranges OUTSIDE any STM (MD5): a transaction cannot
// range, and holding one across a full index walk would serialize every
// allocation in the cluster. Each bin is read whole rather than in pages —
// common.MaxDnCntPerCluster bounds the whole index, and a limit could truncate
// the scan just before the DNs the filters would have accepted.
func FindDnCandidates(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	cc *pb.ClusterConf,
	candExt uint64,
	candUnits uint64,
	candCnt int,
	black []string,
	white []string,
	excludeLocs []string,
) ([]Cand, error) {
	if candCnt <= 0 {
		return nil, nil
	}
	levels := binLevels(cc.GetDnBinConf())
	startBin := uint32(0)
	for startBin < 3 && levels[startBin+1] <= candExt {
		startBin++
	}
	blackSet := strSet(black)
	whiteSet := strSet(white)
	locSet := make(map[string]struct{})
	// Seeding the exclusion into the SAME set the walk dedupes with is the
	// whole of tier 1 (architecture.md, Per-operation allocation): a DN whose
	// location is already a failure domain of
	// the group is skipped exactly like a second DN of one location. It has to
	// happen here rather than as another test inside the loop, because a
	// black-listed addr_port is skipped BEFORE its location is recorded — so
	// black-listing a group's DNs never excluded their domains.
	for _, location := range excludeLocs {
		locSet[location] = struct{}{}
	}
	cands := make([]Cand, 0, candCnt)
	for binIdx := startBin; binIdx <= 3; binIdx++ {
		kvs, _, err := cli.RangeDesc(ctx, DnCapacityPrefix(cid, binIdx), 0)
		if err != nil {
			return nil, err
		}
		var run freeRun
		for _, kv := range kvs {
			keyBin, freeExt, addrPort, ok := ParseDnCapacityKey(kv.Key)
			if !ok {
				// A key nothing in dnv writes. Skipping it keeps one
				// corrupt index entry from failing every allocation.
				continue
			}
			if freeExt != run.freeExt {
				// A new free count ends the run before it, which is
				// drawn first: the break below falls only on such a
				// boundary, so no run is cut short.
				cands = run.take(cands, locSet, candCnt)
				if len(cands) >= candCnt {
					return cands, nil
				}
				run.freeExt = freeExt
			}
			if freeExt < candExt {
				// Descending key order is descending free order inside one
				// bin (architecture.md, Capacity index keys), so nothing further in this
				// bin can qualify.
				break
			}
			if excluded(addrPort, blackSet, whiteSet) {
				continue
			}
			capacity := &pb.DnCapacity{}
			if err := cli.Decode(ctx, kv, capacity); err != nil {
				return nil, err
			}
			if DnCapacityFreeMetaUnits(capacity) < candUnits {
				// Never a break: the next DN of the bin may have room.
				// It joins no run, so this DN neither shuts a
				// same-location DN with room out nor is drawn.
				continue
			}
			location := capacity.GetLocation()
			if _, ok := locSet[location]; ok {
				// Taken before this run: by excludeLocs or by an
				// earlier run's draw.
				continue
			}
			run.add(Cand{
				AddrPort: addrPort,
				Location: location,
				FreeExt:  freeExt,
				BinIdx:   keyBin,
			})
		}
		// The bin's last run; after the break it is already empty.
		cands = run.take(cands, locSet, candCnt)
		if len(cands) >= candCnt {
			return cands, nil
		}
	}
	return cands, nil
}

// freeRun is one run of FindDnCandidates' walk: the keys of one bin that carry
// one free count, which lie next to each other in the bin's descending order
// (architecture.md, Finding DN candidates). Per location not taken before the
// run it holds the DNs of that count the lists and the unit floor let
// through, and it keeps the locations in the order the walk met them.
type freeRun struct {
	freeExt uint64
	locs    []string
	byLoc   map[string][]Cand
}

// add puts a DN that passed every filter into the run, under its location.
func (r *freeRun) add(cand Cand) {
	if r.byLoc == nil {
		r.byLoc = make(map[string][]Cand)
	}
	if _, ok := r.byLoc[cand.Location]; !ok {
		r.locs = append(r.locs, cand.Location)
	}
	r.byLoc[cand.Location] = append(r.byLoc[cand.Location], cand)
}

// take ends the run into cands: per location, in the order met, it marks the
// location taken and appends one of the location's DNs, drawn uniformly, and
// it stops once cands holds candCnt. The run is empty on return. Marking a
// location only here, never when the walk meets it, is what lets every DN of
// the run's count in that location into the draw.
func (r *freeRun) take(
	cands []Cand,
	locSet map[string]struct{},
	candCnt int,
) []Cand {
	locs, byLoc := r.locs, r.byLoc
	r.locs, r.byLoc = nil, nil
	for _, location := range locs {
		if len(cands) >= candCnt {
			break
		}
		equals := byLoc[location]
		locSet[location] = struct{}{}
		cands = append(cands, equals[rand.IntN(len(equals))])
	}
	return cands
}

// FindDnCandidatesAntiAffine is FindDnCandidates with the two-tier
// location rule of architecture.md, Per-operation allocation: tier 1
// excludes excludeLocs; when it yields
// fewer than requiredCnt — the DNs the caller must actually place, not
// the oversampled scan width candCnt — tier 2 rescans without the
// location exclusion and its candidates are merged behind tier 1's. The
// bool reports whether tier 2 was used.
//
// The trigger is requiredCnt and never candCnt: candCnt is RequiredCnt ×
// dn_batch_size (architecture.md, Per-operation allocation), and one scan
// returns at most one candidate per
// location, so a cluster with fewer distinct domains than one batch could
// never fill tier 1 and the anti-affinity would be inert everywhere.
//
// Tier 2 MERGES rather than replaces: it walks free-count-descending and stops
// at candCnt, and the caller's own (excluded) domains usually hold the fullest
// DNs, so a plain replace can drop a distinct-domain candidate tier 1 DID
// find. Tier 1's entries therefore stay at the head and only tier-2 entries
// whose location tier 1 does not already offer are appended: each scan makes
// its own draw among a location's equals (FindDnCandidates), so tier 2 can
// offer another DN of a location tier 1 offers, and the merged list keeps one
// DN per location, as each scan's does. An addr_port has one location, so no
// caller ever sees one DN twice either. The black and white lists and
// candUnits apply to both tiers: tier 2 relaxes the failure domain and
// nothing else. An empty excludeLocs makes tier 1 the plain scan, and tier 2
// — which would offer the same locations again — is skipped.
//
// In a default deployment location defaults to addr_port (architecture.md,
// Disk nodes), so a group's
// locations ARE the addr_ports its caller already black-lists: tier 1 is
// byte-identical to the plain scan, and tier 2 — reached only where that scan
// was already short — re-walks the whole index for a result that cannot
// differ from tier 1's. Unchanged picks and refusals are not a free ride:
// what such a cluster pays is the second walk, on a path that was failing
// anyway.
func FindDnCandidatesAntiAffine(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	cc *pb.ClusterConf,
	candExt uint64,
	candUnits uint64,
	candCnt int,
	requiredCnt int,
	black []string,
	white []string,
	excludeLocs []string,
) ([]Cand, bool, error) {
	cands, err := FindDnCandidates(
		ctx, cli, cid, cc, candExt, candUnits, candCnt,
		black, white, excludeLocs,
	)
	if err != nil {
		return nil, false, err
	}
	if len(excludeLocs) == 0 || len(cands) >= requiredCnt {
		return cands, false, nil
	}
	// Tier 2 is what keeps a two-rack cluster able to place a repair's new
	// leg at all (architecture.md, Per-operation allocation). The resulting same-domain
	// placement is visible in the stored
	// topology and is not logged separately.
	relaxed, err := FindDnCandidates(
		ctx, cli, cid, cc, candExt, candUnits, candCnt, black, white, nil,
	)
	if err != nil {
		return nil, false, err
	}
	seen := make(map[string]struct{}, len(cands))
	for _, cand := range cands {
		seen[cand.Location] = struct{}{}
	}
	for _, cand := range relaxed {
		if len(cands) >= candCnt {
			break
		}
		if _, ok := seen[cand.Location]; ok {
			continue
		}
		seen[cand.Location] = struct{}{}
		cands = append(cands, cand)
	}
	return cands, true, nil
}

// FindCnCandidates walks the CN capacity index for candCnt CNs with at least
// candExt free extents each (MD5; architecture.md, Finding CN candidates,
// verbatim).
//
// It is the DN walk (architecture.md, Finding DN candidates) without bins,
// without the draw among equals and without the metadata-room skip: one
// descending range over the single cn_capacity prefix that takes the first
// CN it meets in each location. The draw spreads a machine's disks, and a
// controller node has none; a CN counts no metadata units. The walk adds one
// filter: spCnAddrs names the CNs that already host a cntlr of the SP being
// served, and a second cntlr of one SP never lands on the same CN. excludeLocs seeds the
// location set before the walk exactly as in FindDnCandidates; for a cntlr
// pick it is the failure domains of the SP's other cntlrs — for a
// replacement, the surviving ones (tier 1 of architecture.md, Per-operation
// allocation). The returned Cand.BinIdx is always 0: CN capacity keys carry
// no bin field, and a caller re-validating a pick rebuilds
// CnCapacityKey(cid, FreeExt, AddrPort) from the other two fields.
func FindCnCandidates(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	white []string,
	spCnAddrs []string,
	excludeLocs []string,
) ([]Cand, error) {
	if candCnt <= 0 {
		return nil, nil
	}
	blackSet := strSet(black)
	whiteSet := strSet(white)
	spSet := strSet(spCnAddrs)
	locSet := make(map[string]struct{})
	// Seeded, not tested inside the loop, for FindDnCandidates' reason: an
	// excluded addr_port is skipped before its location is recorded, so
	// excluding the SP's CNs never excluded their domains.
	for _, location := range excludeLocs {
		locSet[location] = struct{}{}
	}
	cands := make([]Cand, 0, candCnt)
	kvs, _, err := cli.RangeDesc(ctx, CnCapacityPrefix(cid), 0)
	if err != nil {
		return nil, err
	}
	for _, kv := range kvs {
		freeExt, addrPort, ok := ParseCnCapacityKey(kv.Key)
		if !ok {
			continue
		}
		if freeExt < candExt {
			break
		}
		if excluded(addrPort, blackSet, whiteSet) {
			continue
		}
		if _, ok := spSet[addrPort]; ok {
			continue
		}
		capacity := &pb.CnCapacity{}
		if err := cli.Decode(ctx, kv, capacity); err != nil {
			return nil, err
		}
		location := capacity.GetLocation()
		if _, ok := locSet[location]; ok {
			continue
		}
		locSet[location] = struct{}{}
		cands = append(cands, Cand{
			AddrPort: addrPort,
			Location: location,
			FreeExt:  freeExt,
		})
		if len(cands) >= candCnt {
			return cands, nil
		}
	}
	return cands, nil
}

// FindCnCandidatesAntiAffine is FindCnCandidates with the two-tier location
// rule of architecture.md, Per-operation allocation, for a cntlr pick: tier 1
// excludes excludeLocs; when it finds
// no CN at all, tier 2 rescans without the location exclusion. The bool
// reports whether tier 2 was used.
//
// It is FindDnCandidatesAntiAffine with requiredCnt fixed at one — every
// cntlr pick places exactly one CN — so "short" means "empty", and tier 2 has
// no tier-1 entry to merge behind. The black and white lists and spCnAddrs
// apply to both tiers: tier 2 relaxes the failure domain and nothing else, so
// two cntlrs of one SP still never share a CN. An empty excludeLocs
// makes tier 1 the plain scan, and tier 2 — which would repeat it exactly — is
// skipped.
//
// In a default deployment location defaults to addr_port (architecture.md,
// Controller nodes), so the SP's
// locations ARE the addr_ports of the CNs its caller already excludes by
// address: tier 1 is the plain scan, and tier 2 — reached only where that
// scan was empty — re-walks the index for the same empty answer.
func FindCnCandidatesAntiAffine(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	white []string,
	spCnAddrs []string,
	excludeLocs []string,
) ([]Cand, bool, error) {
	cands, err := FindCnCandidates(
		ctx, cli, cid, candExt, candCnt,
		black, white, spCnAddrs, excludeLocs,
	)
	if err != nil {
		return nil, false, err
	}
	if len(excludeLocs) == 0 || len(cands) > 0 {
		return cands, false, nil
	}
	// Tier 2 is what keeps a cluster with fewer failure domains than the SP
	// has cntlrs able to place them at all (architecture.md, Per-operation
	// allocation). The same-domain placement
	// is visible in the stored topology and is not logged separately.
	cands, err = FindCnCandidates(
		ctx, cli, cid, candExt, candCnt,
		black, white, spCnAddrs, nil,
	)
	if err != nil {
		return nil, false, err
	}
	return cands, true, nil
}

// PickRandom draws n distinct candidates uniformly at random (MD5;
// architecture.md, Per-operation allocation). The scan hands back the
// emptiest-first order of the
// index; picking randomly out of a batch candCnt times larger than what is
// needed is what keeps concurrent allocations from all landing on the same few
// nodes.
//
// It never mutates cands, returns at most len(cands) entries, and returns
// nothing for n <= 0.
func PickRandom(cands []Cand, n int) []Cand {
	if n <= 0 || len(cands) == 0 {
		return nil
	}
	if n > len(cands) {
		n = len(cands)
	}
	shuffled := make([]Cand, len(cands))
	copy(shuffled, cands)
	// Partial Fisher-Yates: only the first n slots have to be settled.
	for i := 0; i < n; i++ {
		j := i + rand.IntN(len(shuffled)-i)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	return shuffled[:n]
}
