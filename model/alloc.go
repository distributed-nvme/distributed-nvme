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
// transaction is made safe (architecture.md §8.4 step 2).
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

// excluded applies the two list rules of §6.3 to one addr_port: the black list
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
// candExt free extents each (MD5, architecture.md §6.3 verbatim).
//
// The walk starts at the smallest bin whose range can still hold candExt —
// bins with level_{b+1} <= candExt are skipped, since every DN in them is too
// small by construction — and then runs bins b…3 in order. Each bin is one
// DESCENDING range over its prefix, so DNs come largest-free first and the bin
// is abandoned as soon as a DN's free count drops below candExt. A DN is
// skipped when it is black-listed, when a non-empty white list does not name
// it, or when its location is already represented in the result; the location
// rule is what gives one allocation round failure-domain anti-affinity for
// free. The walk returns as soon as candCnt candidates are collected, and
// after bin 3 returns whatever it has — the caller decides whether that is
// enough (RESOURCE_EXHAUSTED otherwise, §6.5).
//
// Health, flags, fullness and the side-count cap need no filtering here: a
// node that fails any of them has no capacity key at all (§5.6).
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
	candCnt int,
	black []string,
	white []string,
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
	cands := make([]Cand, 0, candCnt)
	for binIdx := startBin; binIdx <= 3; binIdx++ {
		kvs, _, err := cli.RangeDesc(ctx, DnCapacityPrefix(cid, binIdx), 0)
		if err != nil {
			return nil, err
		}
		for _, kv := range kvs {
			keyBin, freeExt, addrPort, ok := ParseDnCapacityKey(kv.Key)
			if !ok {
				// A key nothing in dnv writes. Skipping it keeps one
				// corrupt index entry from failing every allocation.
				continue
			}
			if freeExt < candExt {
				// Descending key order is descending free order inside one
				// bin (§5.6), so nothing further in this bin can qualify.
				break
			}
			if excluded(addrPort, blackSet, whiteSet) {
				continue
			}
			capacity := &pb.DnCapacity{}
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
				BinIdx:   keyBin,
			})
			if len(cands) >= candCnt {
				return cands, nil
			}
		}
	}
	return cands, nil
}

// FindCnCandidates walks the CN capacity index for candCnt CNs with at least
// candExt free extents each (MD5, architecture.md §6.4 verbatim).
//
// It is §6.3 without bins — one descending range over the single cn_capacity
// prefix — plus one extra filter: spCnAddrs names the CNs that already host a
// cntlr of the SP being served, and a second cntlr of one SP never lands on
// the same CN. The returned Cand.BinIdx is always 0: CN capacity keys carry no
// bin field, and a caller re-validating a pick rebuilds CnCapacityKey(cid,
// FreeExt, AddrPort) from the other two fields.
func FindCnCandidates(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	white []string,
	spCnAddrs []string,
) ([]Cand, error) {
	if candCnt <= 0 {
		return nil, nil
	}
	blackSet := strSet(black)
	whiteSet := strSet(white)
	spSet := strSet(spCnAddrs)
	locSet := make(map[string]struct{})
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

// PickRandom draws n distinct candidates uniformly at random (MD5,
// architecture.md §6.5). The scan hands back the emptiest-first order of the
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
