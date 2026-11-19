package model

import (
	"math/rand/v2"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// spaceExt is the extent size of the space fixtures: 64 MiB.
const spaceExt = uint64(64) << 20

// spaceCc is a stored ClusterConf as the fixtures need it: the extent size
// and a bin ladder whose smallest bin is 1<<bin0 extents.
func spaceCc(bin0 uint32) *pb.ClusterConf {
	return &pb.ClusterConf{DnBinConf: &pb.DnBinConf{
		ExtentSize: spaceExt,
		Bin0Shift:  bin0, Bin1Shift: bin0 + 4,
		Bin2Shift: bin0 + 8, Bin3Shift: bin0 + 12,
	}}
}

func spaceDn(location string, free uint64) *pb.DnConf {
	return &pb.DnConf{Location: location, TotalExtCnt: 1024, FreeExtCnt: free}
}

func spaceCn(location string, free uint64) *pb.CnConf {
	return &pb.CnConf{Location: location, TotalExtCnt: 10240, FreeExtCnt: free}
}

// spaceWant is the computed part of one answer, in extents.
type spaceWant struct {
	limit     pb.CapacityLimit
	placeable uint64
	largest   uint64
}

func checkSpace(t *testing.T, label string, got *pb.ClusterSpace, want spaceWant) {
	t.Helper()
	if got.GetLimit() != want.limit ||
		got.GetPlaceableBytes() != want.placeable*spaceExt ||
		got.GetLargestInitExtCnt() != want.largest {
		t.Errorf("%s: limit %v, placeable %d extents, largest %d; "+
			"want %v, %d, %d", label, got.GetLimit(),
			got.GetPlaceableBytes()/spaceExt, got.GetLargestInitExtCnt(),
			want.limit, want.placeable, want.largest)
	}
}

// TestClusterSpaceOf pins the space answer of architecture.md, Clusters,
// GetClusterCapacity: which nodes count, the order of the limits, the space
// capped per location on the disk side and per node on the controller side,
// and the largest first data group a new pool is sure to get.
func TestClusterSpaceOf(t *testing.T) {
	cns := []*pb.CnConf{
		spaceCn("cx", 10000), spaceCn("cy", 10000),
		// At its cntlr ceiling: counts in raw and charged, never in free.
		{Location: "cz", TotalExtCnt: 10240, FreeExtCnt: 10240,
			CntlrPtrList: make([]*pb.CntlrPointer, common.MaxCntlrCntPerCn)},
	}
	dns := []*pb.DnConf{
		spaceDn("la", 500), spaceDn("lb", 30), spaceDn("lc", 10),
		spaceDn("le", 0),
		{Location: "lf", TotalExtCnt: 1024, FreeExtCnt: 1024,
			SidePtrList: make([]*pb.SidePointer, common.MaxSideCntPerDn)},
		{Location: "lg", TotalExtCnt: 1024, FreeExtCnt: 1024, Disabled: true},
		{Location: "lh", TotalExtCnt: 1024, FreeExtCnt: 1024, ErrEpoch: 1},
	}
	cc := spaceCc(0)

	// Raw is every record, charged what its pointers hold, free only the
	// nodes a scan can offer; the counts are echoed.
	got := ClusterSpaceOf(cc, dns, cns, 7, 2, 2, 1)
	if got.GetDnRawBytes() != 7*1024*spaceExt ||
		got.GetDnChargedBytes() != (524+994+1014+1024)*spaceExt ||
		got.GetDnFreeBytes() != 540*spaceExt {
		t.Errorf("disk sums: raw %d charged %d free %d (extents)",
			got.GetDnRawBytes()/spaceExt, got.GetDnChargedBytes()/spaceExt,
			got.GetDnFreeBytes()/spaceExt)
	}
	if got.GetCnRawBytes() != 3*10240*spaceExt ||
		got.GetCnChargedBytes() != 2*240*spaceExt ||
		got.GetCnFreeBytes() != 20000*spaceExt {
		t.Errorf("controller sums: raw %d charged %d free %d (extents)",
			got.GetCnRawBytes()/spaceExt, got.GetCnChargedBytes()/spaceExt,
			got.GetCnFreeBytes()/spaceExt)
	}
	if got.GetLegCnt() != 2 || got.GetCntlrCnt() != 2 ||
		got.GetSliceCnt() != 1 || got.GetPoolCnt() != 7 {
		t.Errorf("counts echoed as %d/%d/%d, pools %d", got.GetLegCnt(),
			got.GetCntlrCnt(), got.GetSliceCnt(), got.GetPoolCnt())
	}
	// RAID1 puts four legs on four disk nodes, and three count.
	checkSpace(t, "three disk nodes for four legs", got,
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_COUNT, 0, 0})

	// With a fourth: 560 free over four locations, but la's 500 can carry
	// one leg of each group only, so the other three bound the space: 60.
	// The meta group may take the two fullest, so the data group is sure of
	// the fourth largest, 10.
	dns = append(dns, spaceDn("ld", 20))
	checkSpace(t, "four disk nodes, one far fuller", ClusterSpaceOf(cc, dns, cns, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE, 60, 10})
	// No redundancy, one cntlr: all 560, and the second largest, 30.
	checkSpace(t, "without redundancy", ClusterSpaceOf(cc, dns, cns, 0, 1, 1, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE, 560, 30})
	// A smallest bin of 16 extents: lc (10) is offered to no scan.
	checkSpace(t, "a bin floor of 16", ClusterSpaceOf(spaceCc(4), dns, cns, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_COUNT, 0, 0})
	// Two slices take eight disk nodes.
	checkSpace(t, "two slices", ClusterSpaceOf(cc, dns, cns, 0, 2, 2, 2),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_COUNT, 0, 0})
	// The pool ceiling comes first, whatever else is short.
	checkSpace(t, "the pool ceiling", ClusterSpaceOf(cc, dns, cns,
		common.MaxSpCntPerCluster, 2, 3, 2),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_POOL_CEILING, 0, 0})
	// Controller nodes before disk nodes: three cntlrs over two that count.
	checkSpace(t, "three cntlrs", ClusterSpaceOf(cc, dns, cns, 0, 2, 3, 2),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_CN_COUNT, 0, 0})
	checkSpace(t, "nothing at all", ClusterSpaceOf(cc, nil, nil, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_CN_COUNT, 0, 0})

	// The controller node that would carry the last cntlr bounds the first
	// data group: 6 free, less the meta group's extent. Each cntlr sits on a
	// node of its own, so cy's 6 bound the space too: the budget limit.
	tight := []*pb.CnConf{spaceCn("cx", 10000), spaceCn("cy", 6)}
	checkSpace(t, "a nearly full controller node", ClusterSpaceOf(cc, dns, tight, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_CN_BUDGET, 6, 5})
	// At two slices the footprint is twice one plus the data group:
	// floor(1000/2) - 1.
	eight := []*pb.DnConf{}
	for _, loc := range []string{"l0", "l1", "l2", "l3", "l4", "l5", "l6", "l7"} {
		eight = append(eight, spaceDn(loc, 1000))
	}
	two := []*pb.CnConf{spaceCn("cx", 1000), spaceCn("cy", 5000)}
	checkSpace(t, "two slices under the budget", ClusterSpaceOf(cc, eight, two, 0, 2, 2, 2),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_CN_BUDGET, 1000, 499})
	// No controller node holds even the smallest pool of two slices, two
	// meta extents and two data extents: nothing is placeable, whatever
	// the shares, and the budgets are the side that has no room.
	small := []*pb.DnConf{}
	for _, loc := range []string{"l0", "l1", "l2", "l3", "l4", "l5", "l6", "l7"} {
		small = append(small, spaceDn(loc, 2))
	}
	six := []*pb.CnConf{}
	for _, loc := range []string{"c0", "c1", "c2", "c3", "c4", "c5"} {
		six = append(six, spaceCn(loc, 3))
	}
	checkSpace(t, "no controller node holds the smallest pool", ClusterSpaceOf(cc, small, six, 0, 2, 1, 2),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_CN_BUDGET, 0, 0})
	// A tie goes to the disk side: 4000 over two legs, 2000 a node on two
	// controller nodes.
	even := []*pb.CnConf{spaceCn("cx", 2000), spaceCn("cy", 2000)}
	checkSpace(t, "a tie", ClusterSpaceOf(cc, eight[:4], even, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE, 2000, 1000})

	// Four disk nodes of one machine: enough nodes, one location.
	oneLoc := []*pb.DnConf{spaceDn("m", 100), spaceDn("m", 100),
		spaceDn("m", 100), spaceDn("m", 100)}
	checkSpace(t, "one location", ClusterSpaceOf(cc, oneLoc, cns, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_LOCATIONS, 0, 0})
	// Two locations of two: enough of both for one slice, not for two,
	// though the plain count of eight would be.
	twoLoc := []*pb.DnConf{spaceDn("m", 100), spaceDn("m", 100),
		spaceDn("n", 100), spaceDn("n", 100)}
	checkSpace(t, "two locations of two", ClusterSpaceOf(cc, twoLoc, cns, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE, 200, 100})
	twoLoc = append(twoLoc, spaceDn("m", 100), spaceDn("m", 100),
		spaceDn("m", 100), spaceDn("m", 100))
	checkSpace(t, "two locations, two slices", ClusterSpaceOf(cc, twoLoc, cns, 0, 2, 2, 2),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_LOCATIONS, 0, 0})

	// Uneven machines: A holds far more than B and C, but a group puts at
	// most one leg on each, so the space is what B and C pair with A: 2000.
	uneven := []*pb.DnConf{spaceDn("A", 4000), spaceDn("A", 3000),
		spaceDn("A", 3000), spaceDn("B", 1000), spaceDn("C", 1000)}
	checkSpace(t, "uneven machines", ClusterSpaceOf(cc, uneven, cns, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE, 2000, 1000})
	// Locations bound the first data group too: only A holds large disk
	// nodes, and every group needs a second location, so one extent is the
	// most a data group can get, where a count that ignores locations would
	// offer the fourth largest, 100.
	oneLarge := []*pb.DnConf{spaceDn("A", 100), spaceDn("A", 100),
		spaceDn("A", 100), spaceDn("A", 100), spaceDn("B", 1), spaceDn("C", 1)}
	checkSpace(t, "one machine holds the large disk nodes", ClusterSpaceOf(cc, oneLarge, cns, 0, 2, 2, 1),
		spaceWant{pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE, 2, 1})
}

// TestSpreadIsTheLargestEvenShare checks spread against a brute force on
// small random inputs: the largest x with sum(min(v, x)) >= k*x.
func TestSpreadIsTheLargestEvenShare(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		values := make([]uint64, rng.IntN(6))
		for i := range values {
			values[i] = uint64(rng.IntN(40))
		}
		k := 1 + rng.IntN(4)
		want := uint64(0)
		for x := uint64(1); x <= 200; x++ {
			sum := uint64(0)
			for _, v := range values {
				sum += min(v, x)
			}
			if sum >= uint64(k)*x {
				want = x
			}
		}
		if got := spread(slices.Clone(values), k); got != want {
			t.Fatalf("spread(%v, %d) = %d, want %d", values, k, got, want)
		}
	}
}

// TestPoolSpaceOf pins the space answer of architecture.md, Storage pools,
// InspectStoragePool: each slice's data space from its data groups' blocks,
// its thin pool's sizes and use from the primary's row, a row that does not
// parse leaving its slice unread while the records' half stays, and the
// pool's sums, overcommit and footprint.
func TestPoolSpaceOf(t *testing.T) {
	const block = uint64(2) << 20
	conf := &pb.SpConf{
		SliceIdList: []uint64{11, 12},
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{DataBlockSize: block},
		},
	}
	slices := []*pb.Slice{
		{
			SliceIdx:    0,
			MetaGrpList: []*pb.Group{{ExtCnt: 1, DataBlocks: 509}},
			DataGrpList: []*pb.Group{
				{ExtCnt: 4, DataBlocks: 2045},
				{ExtCnt: 2, DataBlocks: 1021},
			},
		},
		{
			SliceIdx:    1,
			MetaGrpList: []*pb.Group{{ExtCnt: 1, DataBlocks: 509}},
			DataGrpList: []*pb.Group{{ExtCnt: 4, DataBlocks: 2045}},
		},
	}
	tds := []*pb.ThinDevice{{Size: 10 << 30}, {Size: 10 << 30}, {Size: 4 << 30}}
	info := &pb.CntlrInfo{SliceIdToDmPool: map[uint64]*pb.ResInfo{
		11: {Status: pb.ResStatus_RES_STATUS_OK,
			Details: "0 25116672 thin-pool 3 300/129024 1000/3066 - rw " +
				"discard_passdown queue_if_no_space - 1024"},
		12: {Status: pb.ResStatus_RES_STATUS_ERROR,
			Details: "0 16752640 thin-pool Fail"},
	}}
	got := PoolSpaceOf(conf, slices, tds, info)
	want := &pb.PoolSpace{
		SliceList: []*pb.SliceSpace{
			{
				SliceId: 11, SliceIdx: 0, DataGrpCnt: 2,
				DataBytes:     (2045 + 1021) * block,
				Read:          true,
				PoolDataBytes: 3066 * block,
				UsedDataBytes: 1000 * block,
				PoolMetaBytes: 129024 * common.ThinMetaBlockSize,
				UsedMetaBytes: 300 * common.ThinMetaBlockSize,
			},
			// A failed pool's line does not parse: the slice is unread,
			// its data space still the records'.
			{SliceId: 12, SliceIdx: 1, DataGrpCnt: 1, DataBytes: 2045 * block},
		},
		TdCnt:           3,
		VirtualBytes:    24 << 30,
		DataBytes:       (2045 + 1021 + 2045) * block,
		UsedDataBytes:   1000 * block,
		OvercommitPct:   (24 << 30) * 100 / ((2045 + 1021 + 2045) * block),
		FootprintExtCnt: 1 + 4 + 2 + 1 + 4,
	}
	if !proto.Equal(got, want) {
		t.Errorf("PoolSpaceOf:\n got %v\nwant %v", got, want)
	}

	// No report at all: every slice unread, the records' half whole.
	got = PoolSpaceOf(conf, slices, tds, nil)
	if got.GetUsedDataBytes() != 0 || got.GetSliceList()[0].GetRead() ||
		got.GetDataBytes() != want.GetDataBytes() ||
		got.GetFootprintExtCnt() != want.GetFootprintExtCnt() {
		t.Errorf("with no report: %v", got)
	}
	// No data space: no overcommit, rather than a division by zero.
	if got := PoolSpaceOf(conf, nil, tds, nil); got.GetOvercommitPct() != 0 ||
		got.GetVirtualBytes() != 24<<30 {
		t.Errorf("with no slice: %v", got)
	}
}
