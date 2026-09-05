package model

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A fake STM, so that the MD4 maintenance rule is testable without etcd
// ---------------------------------------------------------------------------

// fakeStm records what a caller staged, in order. It is enough for
// Maintain*Capacity, which only ever puts and deletes.
type fakeStm struct {
	puts []string
	dels []string
	vals map[string]proto.Message
}

func newFakeStm() *fakeStm {
	return &fakeStm{vals: make(map[string]proto.Message)}
}

func (f *fakeStm) Get(key string, msg proto.Message) bool { return false }

func (f *fakeStm) Put(key string, msg proto.Message) {
	f.puts = append(f.puts, key)
	f.vals[key] = msg
}

func (f *fakeStm) Del(key string) { f.dels = append(f.dels, key) }

func (f *fakeStm) Rev(key string) int64 { return 0 }

// fakeStm must stay a valid etcdutil.STM.
var _ etcdutil.STM = (*fakeStm)(nil)

// ---------------------------------------------------------------------------
// Defaults (RW21)
// ---------------------------------------------------------------------------

func TestResolveDnBinConf(t *testing.T) {
	defaults := [4]uint32{
		common.DefaultDnBin0Shift,
		common.DefaultDnBin1Shift,
		common.DefaultDnBin2Shift,
		common.DefaultDnBin3Shift,
	}
	cases := []struct {
		name       string
		conf       *pb.DnBinConf
		wantShifts [4]uint32
		wantSize   uint64
	}{
		{"nil", nil, defaults, common.DefaultDnExtSize},
		{"empty", &pb.DnBinConf{}, defaults, common.DefaultDnExtSize},
		{
			"stored size kept",
			&pb.DnBinConf{ExtentSize: 64 * 1024 * 1024},
			defaults,
			64 * 1024 * 1024,
		},
		{
			"increasing shifts kept",
			&pb.DnBinConf{
				Bin0Shift: 2, Bin1Shift: 3, Bin2Shift: 4, Bin3Shift: 5,
			},
			[4]uint32{2, 3, 4, 5},
			common.DefaultDnExtSize,
		},
		{
			"equal shifts fall back",
			&pb.DnBinConf{
				Bin0Shift: 5, Bin1Shift: 5, Bin2Shift: 6, Bin3Shift: 7,
			},
			defaults,
			common.DefaultDnExtSize,
		},
		{
			"decreasing shifts fall back",
			&pb.DnBinConf{
				Bin0Shift: 8, Bin1Shift: 4, Bin2Shift: 2, Bin3Shift: 1,
			},
			defaults,
			common.DefaultDnExtSize,
		},
		{
			"shift above 63 falls back",
			&pb.DnBinConf{
				Bin0Shift: 0, Bin1Shift: 4, Bin2Shift: 8, Bin3Shift: 64,
			},
			defaults,
			common.DefaultDnExtSize,
		},
		{
			"the whole ladder falls back, never one shift",
			&pb.DnBinConf{
				Bin0Shift: 9, Bin1Shift: 9, Bin2Shift: 10, Bin3Shift: 11,
			},
			defaults,
			common.DefaultDnExtSize,
		},
	}
	for _, tc := range cases {
		got := ResolveDnBinConf(tc.conf)
		gotShifts := [4]uint32{
			got.GetBin0Shift(),
			got.GetBin1Shift(),
			got.GetBin2Shift(),
			got.GetBin3Shift(),
		}
		if gotShifts != tc.wantShifts {
			t.Errorf(
				"%s: shifts = %v, want %v",
				tc.name, gotShifts, tc.wantShifts,
			)
		}
		if got.GetExtentSize() != tc.wantSize {
			t.Errorf(
				"%s: extent_size = %d, want %d",
				tc.name, got.GetExtentSize(), tc.wantSize,
			)
		}
	}
	// The stored message is never modified in place.
	stored := &pb.DnBinConf{}
	ResolveDnBinConf(stored)
	if stored.GetExtentSize() != 0 || stored.GetBin1Shift() != 0 {
		t.Error("ResolveDnBinConf mutated its argument")
	}
}

func TestResolveHealthCheckConf(t *testing.T) {
	cases := []struct {
		name string
		conf *pb.HealthCheckConf
		want [4]uint32
	}{
		{
			"nil",
			nil,
			[4]uint32{
				common.DefaultHealthCheckInterval,
				common.DefaultHealthCheckInterval,
				common.DefaultHealthCheckInterval,
				common.DefaultHealthCheckInterval,
			},
		},
		{
			"zeros become the default",
			&pb.HealthCheckConf{DnInterval: 30},
			[4]uint32{
				30,
				common.DefaultHealthCheckInterval,
				common.DefaultHealthCheckInterval,
				common.DefaultHealthCheckInterval,
			},
		},
		{
			"above the max is clamped",
			&pb.HealthCheckConf{
				DnInterval:    common.MaxHealthCheckInterval + 1,
				CnInterval:    common.MaxHealthCheckInterval,
				SideInterval:  1,
				CntlrInterval: 7,
			},
			[4]uint32{
				common.MaxHealthCheckInterval,
				common.MaxHealthCheckInterval,
				1,
				7,
			},
		},
	}
	for _, tc := range cases {
		got := ResolveHealthCheckConf(tc.conf)
		gotAll := [4]uint32{
			got.GetDnInterval(),
			got.GetCnInterval(),
			got.GetSideInterval(),
			got.GetCntlrInterval(),
		}
		if gotAll != tc.want {
			t.Errorf("%s: intervals = %v, want %v", tc.name, gotAll, tc.want)
		}
	}
}

func TestResolveAllocConf(t *testing.T) {
	got := ResolveAllocConf(nil)
	if got.GetDnBatchSize() != common.DefaultAllocDnBatchSize ||
		got.GetCnBatchSize() != common.DefaultAllocCnBatchSize {
		t.Errorf("ResolveAllocConf(nil) = %v", got)
	}
	got = ResolveAllocConf(&pb.AllocConf{
		DnBatchSize: common.MaxAllocDnBatchSize + 1,
		CnBatchSize: 3,
	})
	if got.GetDnBatchSize() != common.MaxAllocDnBatchSize ||
		got.GetCnBatchSize() != 3 {
		t.Errorf("ResolveAllocConf clamp = %v", got)
	}
}

func TestResolveClusterConf(t *testing.T) {
	stored := &pb.ClusterConf{
		CreationEpoch: 1756000000000000000,
		QosRatio:      &pb.QosRatio{},
	}
	got := ResolveClusterConf(stored)
	if got.GetCreationEpoch() != stored.GetCreationEpoch() {
		t.Error("ResolveClusterConf changed creation_epoch")
	}
	if got.GetQosRatio() != stored.GetQosRatio() {
		t.Error("ResolveClusterConf did not pass qos_ratio through as stored")
	}
	if got.GetDnBinConf().GetExtentSize() != common.DefaultDnExtSize {
		t.Error("ResolveClusterConf did not resolve dn_bin_conf")
	}
	if got.GetHealthCheckConf().GetSideInterval() !=
		common.DefaultHealthCheckInterval {
		t.Error("ResolveClusterConf did not resolve health_check_conf")
	}
	if got.GetAllocConf().GetDnBatchSize() !=
		common.DefaultAllocDnBatchSize {
		t.Error("ResolveClusterConf did not resolve alloc_conf")
	}
	if ResolveClusterConf(nil).GetDnBinConf().GetBin3Shift() !=
		common.DefaultDnBin3Shift {
		t.Error("ResolveClusterConf(nil) is not the pure defaults")
	}
}

// ---------------------------------------------------------------------------
// Bins (MD4, §6.2)
// ---------------------------------------------------------------------------

func TestDnBinIdx(t *testing.T) {
	custom := &pb.DnBinConf{
		Bin0Shift: 2, Bin1Shift: 3, Bin2Shift: 4, Bin3Shift: 5,
	}
	cases := []struct {
		name    string
		conf    *pb.DnBinConf
		freeExt uint64
		wantBin uint32
		wantOk  bool
	}{
		// Defaults: levels 1 / 16 / 256 / 4096.
		{"default below level0", nil, 0, 0, false},
		{"default at level0", nil, 1, 0, true},
		{"default below level1", nil, 15, 0, true},
		{"default at level1", nil, 16, 1, true},
		{"default below level2", nil, 255, 1, true},
		{"default at level2", nil, 256, 2, true},
		{"default below level3", nil, 4095, 2, true},
		{"default at level3", nil, 4096, 3, true},
		{"default far above level3", nil, 1 << 40, 3, true},
		{"empty conf is the defaults", &pb.DnBinConf{}, 16, 1, true},
		// Custom: levels 4 / 8 / 16 / 32.
		{"custom below level0", custom, 3, 0, false},
		{"custom at level0", custom, 4, 0, true},
		{"custom below level1", custom, 7, 0, true},
		{"custom at level1", custom, 8, 1, true},
		{"custom below level2", custom, 15, 1, true},
		{"custom at level2", custom, 16, 2, true},
		{"custom below level3", custom, 31, 2, true},
		{"custom at level3", custom, 32, 3, true},
	}
	for _, tc := range cases {
		gotBin, gotOk := DnBinIdx(tc.freeExt, tc.conf)
		if gotOk != tc.wantOk || (gotOk && gotBin != tc.wantBin) {
			t.Errorf(
				"%s: DnBinIdx(%d) = (%d, %v), want (%d, %v)",
				tc.name, tc.freeExt, gotBin, gotOk, tc.wantBin, tc.wantOk,
			)
		}
	}
}

// ---------------------------------------------------------------------------
// The §5.6 presence rule (MD4)
// ---------------------------------------------------------------------------

// healthyDn is an allocatable DN; each case below breaks exactly one clause.
func healthyDn() *pb.DnConf {
	return &pb.DnConf{
		DnId:        1,
		Location:    "rack0",
		FreeExtCnt:  10,
		TotalExtCnt: 100,
	}
}

func TestDnAllocatable(t *testing.T) {
	full := healthyDn()
	full.SidePtrList = make([]*pb.SidePointer, common.MaxSideCntPerDn)
	almostFull := healthyDn()
	almostFull.SidePtrList = make([]*pb.SidePointer, common.MaxSideCntPerDn-1)
	unhealthy := healthyDn()
	unhealthy.ErrEpoch = 1756000000
	disabled := healthyDn()
	disabled.Disabled = true
	empty := healthyDn()
	empty.FreeExtCnt = 0
	small := healthyDn()
	small.FreeExtCnt = 10
	bigBin0 := &pb.DnBinConf{
		Bin0Shift: 4, Bin1Shift: 5, Bin2Shift: 6, Bin3Shift: 7,
	}
	cases := []struct {
		name string
		dn   *pb.DnConf
		conf *pb.DnBinConf
		want bool
	}{
		{"healthy", healthyDn(), nil, true},
		{"nil record", nil, nil, false},
		{"side_ptr_list at the cap", full, nil, false},
		{"side_ptr_list below the cap", almostFull, nil, true},
		{"err_epoch set", unhealthy, nil, false},
		{"disabled", disabled, nil, false},
		{"no free extent", empty, nil, false},
		{"below a raised bin0 floor", small, bigBin0, false},
		{"at a raised bin0 floor", &pb.DnConf{FreeExtCnt: 16}, bigBin0, true},
	}
	for _, tc := range cases {
		if got := DnAllocatable(tc.dn, tc.conf); got != tc.want {
			t.Errorf("%s: DnAllocatable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCnAllocatable(t *testing.T) {
	healthy := func() *pb.CnConf {
		return &pb.CnConf{CnId: 1, Location: "rack0", FreeExtCnt: 1}
	}
	full := healthy()
	full.CntlrPtrList = make([]*pb.CntlrPointer, common.MaxCntlrCntPerCn)
	almostFull := healthy()
	almostFull.CntlrPtrList = make(
		[]*pb.CntlrPointer, common.MaxCntlrCntPerCn-1,
	)
	unhealthy := healthy()
	unhealthy.ErrEpoch = 1756000000
	disabled := healthy()
	disabled.Disabled = true
	empty := healthy()
	empty.FreeExtCnt = 0
	cases := []struct {
		name string
		cn   *pb.CnConf
		want bool
	}{
		{"healthy", healthy(), true},
		{"nil record", nil, false},
		{"cntlr_ptr_list at the cap", full, false},
		{"cntlr_ptr_list below the cap", almostFull, true},
		{"err_epoch set", unhealthy, false},
		{"disabled", disabled, false},
		{"no free extent", empty, false},
	}
	for _, tc := range cases {
		if got := CnAllocatable(tc.cn); got != tc.want {
			t.Errorf("%s: CnAllocatable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Capacity-key maintenance (MD4)
// ---------------------------------------------------------------------------

func TestMaintainDnCapacity(t *testing.T) {
	const addrPort = "192.168.0.17:9000"
	cid := ClusterId(t.Name(), 0)
	cc := &pb.ClusterConf{}
	// free 10 with the default bins is bin 0 (levels 1/16/256/4096).
	key10 := DnCapacityKey(cid, 0, 10, addrPort)
	key20 := DnCapacityKey(cid, 1, 20, addrPort)

	withFree := func(free uint64) *pb.DnConf {
		dn := healthyDn()
		dn.FreeExtCnt = free
		return dn
	}
	disabled := withFree(10)
	disabled.Disabled = true

	cases := []struct {
		name     string
		old      *pb.DnConf
		new      *pb.DnConf
		wantDels []string
		wantPuts []string
	}{
		{
			"create",
			nil,
			withFree(10),
			nil,
			[]string{key10},
		},
		{
			"free count moved: exact delete, then put",
			withFree(10),
			withFree(20),
			[]string{key10},
			[]string{key20},
		},
		{
			"nothing moved: put only, no spurious delete",
			withFree(10),
			withFree(10),
			nil,
			[]string{key10},
		},
		{
			"delete",
			withFree(10),
			nil,
			[]string{key10},
			nil,
		},
		{
			"became unallocatable",
			withFree(10),
			disabled,
			[]string{key10},
			nil,
		},
		{
			"became allocatable",
			disabled,
			withFree(10),
			nil,
			[]string{key10},
		},
		{
			"never allocatable",
			disabled,
			disabled,
			nil,
			nil,
		},
		{
			"absent both ways",
			nil,
			nil,
			nil,
			nil,
		},
	}
	for _, tc := range cases {
		s := newFakeStm()
		MaintainDnCapacity(s, cid, addrPort, cc, tc.old, tc.new)
		if !equalStrings(s.dels, tc.wantDels) {
			t.Errorf("%s: dels = %v, want %v", tc.name, s.dels, tc.wantDels)
		}
		if !equalStrings(s.puts, tc.wantPuts) {
			t.Errorf("%s: puts = %v, want %v", tc.name, s.puts, tc.wantPuts)
		}
		for _, key := range s.puts {
			value, ok := s.vals[key].(*pb.DnCapacity)
			if !ok || value.GetLocation() != "rack0" {
				t.Errorf(
					"%s: value of %q = %v, want the DN's location",
					tc.name, key, s.vals[key],
				)
			}
		}
	}
}

func TestMaintainCnCapacity(t *testing.T) {
	const addrPort = "192.168.0.18:9000"
	cid := ClusterId(t.Name(), 0)
	withFree := func(free uint64) *pb.CnConf {
		return &pb.CnConf{CnId: 2, Location: "rack1", FreeExtCnt: free}
	}
	key5 := CnCapacityKey(cid, 5, addrPort)
	key4 := CnCapacityKey(cid, 4, addrPort)

	s := newFakeStm()
	MaintainCnCapacity(s, cid, addrPort, nil, withFree(5))
	if !equalStrings(s.puts, []string{key5}) || len(s.dels) != 0 {
		t.Errorf("create: puts = %v, dels = %v", s.puts, s.dels)
	}

	s = newFakeStm()
	MaintainCnCapacity(s, cid, addrPort, withFree(5), withFree(4))
	if !equalStrings(s.dels, []string{key5}) ||
		!equalStrings(s.puts, []string{key4}) {
		t.Errorf("move: puts = %v, dels = %v", s.puts, s.dels)
	}

	s = newFakeStm()
	MaintainCnCapacity(s, cid, addrPort, withFree(5), withFree(5))
	if len(s.dels) != 0 || !equalStrings(s.puts, []string{key5}) {
		t.Errorf("unchanged: puts = %v, dels = %v", s.puts, s.dels)
	}

	// A budget that ran out removes the key: CnAllocatable requires a
	// nonzero free_ext_cnt (§5.6).
	s = newFakeStm()
	MaintainCnCapacity(s, cid, addrPort, withFree(5), withFree(0))
	if !equalStrings(s.dels, []string{key5}) || len(s.puts) != 0 {
		t.Errorf("exhausted: puts = %v, dels = %v", s.puts, s.dels)
	}
}

// equalStrings compares two key lists, treating nil and empty as equal.
func equalStrings(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
