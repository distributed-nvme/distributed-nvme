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
// Shared stored-conf fixtures (architecture.md §7)
// ---------------------------------------------------------------------------
//
// Defaults are resolved once, on the write path, so a stored ClusterConf or
// bdev_conf is concrete in etcd and no reader substitutes a member of one. A
// fixture is therefore a STORED conf: spelled out, never left at the proto3
// zeros a reader would have had to fill in. These builders are what the whole
// package uses; a test that is about one member overwrites exactly that member
// on the result.

// testBinConf is the 0/4/8/12 shift ladder — bin levels 1 / 16 / 256 / 4096
// (§6.2) — with 1 GiB extents. It is written out rather than defaulted,
// because binLevels shifts what it is given: it is the same ladder
// ResolveDnBinConf picks for a request that named none, which keeps these
// fixtures representative of a real cluster's stored conf.
func testBinConf() *pb.DnBinConf {
	return &pb.DnBinConf{
		ExtentSize: common.DefaultDnExtSize,
		Bin0Shift:  0,
		Bin1Shift:  4,
		Bin2Shift:  8,
		Bin3Shift:  12,
	}
}

// testBdevConf is a stored BdevConf with dataBlockSize as its pool block size
// and no redund_conf, which is the redund_none choice (§8.4). Every member
// ValidateBdevConf requires is concrete.
func testBdevConf(dataBlockSize uint64) *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   dataBlockSize,
			LowWaterMarkPct: common.DefaultPoolLowWatermarkPct,
		},
		DmRaid0Conf: &pb.DmRaid0Conf{
			StripeSize: common.DefaultDmRaid0StripeSize,
		},
	}
}

// testRaid1BdevConf is testBdevConf plus the md-raid1 arm of the redund_conf
// oneof, whose bitmap_chunk_block_cnt is the one member ValidateBdevConf
// requires for that choice and only for it.
func testRaid1BdevConf(
	dataBlockSize uint64,
	chunkBlockCnt uint64,
) *pb.BdevConf {
	conf := testBdevConf(dataBlockSize)
	conf.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{
				BitmapChunkBlockCnt: chunkBlockCnt,
			},
		},
	}
	return conf
}

// testClusterConf is a ClusterConf in the shape CreateCluster stores one: the
// ladder above plus concrete alloc, health-check and bdev members, which is
// exactly what ValidateClusterConf accepts.
func testClusterConf() *pb.ClusterConf {
	return &pb.ClusterConf{
		BdevConf:  testBdevConf(common.DefaultDmPoolDataBlockSize),
		DnBinConf: testBinConf(),
		AllocConf: &pb.AllocConf{
			DnBatchSize: common.DefaultAllocDnBatchSize,
			CnBatchSize: common.DefaultAllocCnBatchSize,
		},
		HealthCheckConf: &pb.HealthCheckConf{
			DnInterval:    common.DefaultHealthCheckInterval,
			CnInterval:    common.DefaultHealthCheckInterval,
			SideInterval:  common.DefaultHealthCheckInterval,
			CntlrInterval: common.DefaultHealthCheckInterval,
		},
	}
}

// ---------------------------------------------------------------------------
// Write-path resolution (architecture.md §7)
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
	// testBinConf spells its ladder out; this is what keeps it the very conf
	// a request that named no shifts is stored as, so that a fixture cannot
	// drift away from the clusters these tests stand in for.
	if !proto.Equal(ResolveDnBinConf(nil), testBinConf()) {
		t.Errorf(
			"testBinConf = %v, want the resolved defaults %v",
			testBinConf(), ResolveDnBinConf(nil),
		)
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

// raid1Conf is the md-raid1 arm of the redund_conf oneof with one chunk count.
func raid1Conf(chunkBlockCnt uint64) *pb.RedundConf {
	return &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{
				BitmapChunkBlockCnt: chunkBlockCnt,
			},
		},
	}
}

// noneConf is the redund_none arm of the redund_conf oneof.
func noneConf() *pb.RedundConf {
	return &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundNone{RedundNone: &pb.RedundNone{}},
	}
}

// TestResolveBdevConf pins §7 member by member: a zero takes the constant, a
// named value is kept, and the two members that are NOT plain defaults — a
// low_water_mark_pct above 100, which means "never auto-grow this pool", and
// the redund_conf choice, which is a choice and not a default — are left
// exactly as given.
func TestResolveBdevConf(t *testing.T) {
	cases := []struct {
		name          string
		conf          *pb.BdevConf
		wantBlockSize uint64
		wantPct       uint32
		wantStripe    uint64
		// wantChunk is the resolved bitmap_chunk_block_cnt when the md-raid1
		// arm must be selected; 0 means it must not be.
		wantChunk uint64
		// wantNone is true when the redund_none arm must be selected.
		wantNone bool
	}{
		{
			name:          "nil",
			conf:          nil,
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    common.DefaultDmRaid0StripeSize,
		},
		{
			// An unset redund_conf already means redund_none (§8.4), so
			// nothing here may promote it to a bitmap-carrying md-raid1.
			name:          "empty: no redund kind is invented either",
			conf:          &pb.BdevConf{},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    common.DefaultDmRaid0StripeSize,
		},
		{
			name: "data_block_size alone",
			conf: &pb.BdevConf{
				DmPoolConf: &pb.DmPoolConf{DataBlockSize: 64 * 1024},
			},
			wantBlockSize: 64 * 1024,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    common.DefaultDmRaid0StripeSize,
		},
		{
			name: "low_water_mark_pct alone",
			conf: &pb.BdevConf{
				DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 80},
			},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       80,
			wantStripe:    common.DefaultDmRaid0StripeSize,
		},
		{
			// Above 100 is the §7 "auto-grow off" setting, never a value to
			// clamp or replace.
			name: "low_water_mark_pct above 100 survives",
			conf: &pb.BdevConf{
				DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 4096},
			},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       4096,
			wantStripe:    common.DefaultDmRaid0StripeSize,
		},
		{
			name: "stripe_size alone",
			conf: &pb.BdevConf{
				DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: 4 * 1024},
			},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    4 * 1024,
		},
		{
			name:          "redund_none stays redund_none",
			conf:          &pb.BdevConf{RedundConf: noneConf()},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    common.DefaultDmRaid0StripeSize,
			wantNone:      true,
		},
		{
			name:          "md-raid1 with no chunk count takes the default",
			conf:          &pb.BdevConf{RedundConf: raid1Conf(0)},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    common.DefaultDmRaid0StripeSize,
			wantChunk:     common.DefaultChunkBlockCnt,
		},
		{
			name:          "md-raid1 keeps a named chunk count",
			conf:          &pb.BdevConf{RedundConf: raid1Conf(7)},
			wantBlockSize: common.DefaultDmPoolDataBlockSize,
			wantPct:       common.DefaultPoolLowWatermarkPct,
			wantStripe:    common.DefaultDmRaid0StripeSize,
			wantChunk:     7,
		},
	}
	for _, tc := range cases {
		got := ResolveBdevConf(tc.conf)
		if got.GetDmPoolConf().GetDataBlockSize() != tc.wantBlockSize {
			t.Errorf(
				"%s: data_block_size = %d, want %d", tc.name,
				got.GetDmPoolConf().GetDataBlockSize(), tc.wantBlockSize,
			)
		}
		if got.GetDmPoolConf().GetLowWaterMarkPct() != tc.wantPct {
			t.Errorf(
				"%s: low_water_mark_pct = %d, want %d", tc.name,
				got.GetDmPoolConf().GetLowWaterMarkPct(), tc.wantPct,
			)
		}
		if got.GetDmRaid0Conf().GetStripeSize() != tc.wantStripe {
			t.Errorf(
				"%s: stripe_size = %d, want %d", tc.name,
				got.GetDmRaid0Conf().GetStripeSize(), tc.wantStripe,
			)
		}
		raid1 := got.GetRedundConf().GetRedundMdRaid1()
		if tc.wantChunk == 0 && raid1 != nil {
			t.Errorf("%s: md-raid1 was invented", tc.name)
		}
		if tc.wantChunk != 0 {
			if raid1 == nil {
				t.Errorf("%s: the md-raid1 choice was lost", tc.name)
			} else if raid1.GetBitmapChunkBlockCnt() != tc.wantChunk {
				t.Errorf(
					"%s: bitmap_chunk_block_cnt = %d, want %d", tc.name,
					raid1.GetBitmapChunkBlockCnt(), tc.wantChunk,
				)
			}
		}
		if gotNone := got.GetRedundConf().GetRedundNone() != nil; gotNone !=
			tc.wantNone {
			t.Errorf(
				"%s: redund_none selected = %v, want %v",
				tc.name, gotNone, tc.wantNone,
			)
		}
	}
	// The request message is never modified in place, and the result never
	// aliases the redund_conf the caller still owns.
	stored := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{},
		RedundConf: noneConf(),
	}
	resolved := ResolveBdevConf(stored)
	if stored.GetDmPoolConf().GetDataBlockSize() != 0 ||
		stored.GetDmPoolConf().GetLowWaterMarkPct() != 0 ||
		stored.GetDmRaid0Conf() != nil {
		t.Error("ResolveBdevConf mutated its argument")
	}
	if resolved.GetRedundConf() == stored.GetRedundConf() {
		t.Error("ResolveBdevConf aliased its argument's redund_conf")
	}
}

func TestResolveClusterConf(t *testing.T) {
	stored := &pb.ClusterConf{
		CreationEpoch: 1756000000000000000,
		QosRatio:      &pb.QosRatio{},
		// One bdev member named, the rest left at proto3 zero: the named one
		// must survive and the others must become concrete.
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{DataBlockSize: 64 * 1024},
		},
	}
	got := ResolveClusterConf(stored)
	if got.GetCreationEpoch() != stored.GetCreationEpoch() {
		t.Error("ResolveClusterConf changed creation_epoch")
	}
	// qos_ratio is not defaultable and stays the very message it was given;
	// bdev_conf, which now goes through ResolveBdevConf, must NOT — a stored
	// message may never alias the request the caller still owns.
	if got.GetQosRatio() != stored.GetQosRatio() {
		t.Error("ResolveClusterConf did not pass qos_ratio through as stored")
	}
	if got.GetBdevConf() == stored.GetBdevConf() {
		t.Error("ResolveClusterConf aliased bdev_conf instead of resolving it")
	}
	if got.GetBdevConf().GetDmPoolConf().GetDataBlockSize() != 64*1024 {
		t.Error("ResolveClusterConf changed a named data_block_size")
	}
	if got.GetBdevConf().GetDmPoolConf().GetLowWaterMarkPct() !=
		common.DefaultPoolLowWatermarkPct ||
		got.GetBdevConf().GetDmRaid0Conf().GetStripeSize() !=
			common.DefaultDmRaid0StripeSize {
		t.Errorf(
			"ResolveClusterConf did not resolve bdev_conf: %v",
			got.GetBdevConf(),
		)
	}
	if stored.GetBdevConf().GetDmRaid0Conf() != nil {
		t.Error("ResolveClusterConf mutated its argument's bdev_conf")
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
	// What CreateCluster stores is what its readers accept: the two families
	// are each other's mirror image (§7), so a resolved conf must validate.
	if err := ValidateClusterConf(ResolveClusterConf(nil)); err != nil {
		t.Errorf("a resolved ClusterConf must validate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Stored-conf validation (architecture.md §7)
// ---------------------------------------------------------------------------
//
// The error strings are part of the contract, not decoration: the gateway
// turns them into an ABORTED message, the worker logs them under
// msgInvalidStoredConf, and the agents carry a second copy of these rules with
// the SAME text (layout.md §3 forbids them importing model). So the tables
// below pin the whole string, prefix included.

func TestValidateBdevConf(t *testing.T) {
	// A stored conf as CreateStoragePool writes it; each case below changes
	// exactly the one member it is about.
	autoGrowOff := testBdevConf(common.DefaultDmPoolDataBlockSize)
	autoGrowOff.DmPoolConf.LowWaterMarkPct = 4096
	noBlockSize := testRaid1BdevConf(common.DefaultDmPoolDataBlockSize, 128)
	noBlockSize.DmPoolConf.DataBlockSize = 0
	noPct := testBdevConf(common.DefaultDmPoolDataBlockSize)
	noPct.DmPoolConf.LowWaterMarkPct = 0
	noStripe := testBdevConf(common.DefaultDmPoolDataBlockSize)
	noStripe.DmRaid0Conf.StripeSize = 0
	explicitNone := testBdevConf(common.DefaultDmPoolDataBlockSize)
	explicitNone.RedundConf = noneConf()
	cases := []struct {
		name string
		conf *pb.BdevConf
		// wantErr is "" when the conf must be accepted.
		wantErr string
	}{
		{
			// No redund_conf at all is the redund_none choice (§8.4), and a
			// redund_none pool has no bitmap: the missing chunk count is
			// correct, not an omission.
			name: "redund_none, no bitmap chunk count",
			conf: testBdevConf(common.DefaultDmPoolDataBlockSize),
		},
		{
			name: "an explicit redund_none, no bitmap chunk count",
			conf: explicitNone,
		},
		{
			name: "md-raid1 with a chunk count",
			conf: testRaid1BdevConf(common.DefaultDmPoolDataBlockSize, 128),
		},
		{
			// Above 100 means "never auto-grow" (§7) and is a value, not a
			// fault; ValidateBdevConf checks presence, never range.
			name: "low_water_mark_pct above 100",
			conf: autoGrowOff,
		},
		{
			name: "nil",
			conf: nil,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			name: "data_block_size zero",
			conf: noBlockSize,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			name: "low_water_mark_pct zero",
			conf: noPct,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.low_water_mark_pct is zero",
		},
		{
			name: "stripe_size zero",
			conf: noStripe,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_raid0_conf.stripe_size is zero",
		},
		{
			name: "md-raid1 without a chunk count",
			conf: testRaid1BdevConf(common.DefaultDmPoolDataBlockSize, 0),
			wantErr: "invalid stored conf: bdev_conf.redund_conf." +
				"redund_md_raid1.bitmap_chunk_block_cnt is zero",
		},
	}
	for _, tc := range cases {
		err := ValidateBdevConf(tc.conf)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: got %v, want accepted", tc.name, err)
			}
			continue
		}
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateClusterConf(t *testing.T) {
	if err := ValidateClusterConf(nil); err == nil ||
		err.Error() != "invalid stored conf: dn_bin_conf.extent_size is zero" {
		t.Errorf("nil: got %v, want the extent_size refusal", err)
	}
	cases := []struct {
		name string
		// tweak turns the valid stored conf into this case's; nil leaves it
		// valid.
		tweak func(cc *pb.ClusterConf)
		// wantErr is "" when the conf must be accepted.
		wantErr string
	}{
		{
			name: "as CreateCluster stores it",
		},
		{
			// A DN header is formatted with extent_size (§3.1), so a cluster
			// whose DNs are correctly formatted at a size §7 would reject on
			// the request must keep working: presence is checked, range never.
			name: "an extent_size outside the §7 request bounds",
			tweak: func(cc *pb.ClusterConf) {
				cc.DnBinConf.ExtentSize = 1024
			},
		},
		{
			name: "the 0/4/8/12 ladder",
			tweak: func(cc *pb.ClusterConf) {
				cc.DnBinConf.Bin0Shift = 0
				cc.DnBinConf.Bin1Shift = 4
				cc.DnBinConf.Bin2Shift = 8
				cc.DnBinConf.Bin3Shift = 12
			},
		},
		{
			name: "a low_water_mark_pct above 100 in bdev_conf",
			tweak: func(cc *pb.ClusterConf) {
				cc.BdevConf.DmPoolConf.LowWaterMarkPct = 4096
			},
		},
		{
			// Nothing reads a cluster's own bdev_conf for geometry — each SP
			// carries its own snapshot — so its absence is not fatal.
			name: "no bdev_conf at all",
			tweak: func(cc *pb.ClusterConf) {
				cc.BdevConf = nil
			},
		},
		{
			name: "extent_size zero",
			tweak: func(cc *pb.ClusterConf) {
				cc.DnBinConf.ExtentSize = 0
			},
			wantErr: "invalid stored conf: dn_bin_conf.extent_size is zero",
		},
		{
			// The all-zero set a DnBinConf written without shifts carries.
			// It is not a ladder — bin0_shift 0 is legal only as the bottom
			// of an increasing one — and no reader resolves it any more, so
			// it is refused rather than silently shifted into 1/1/1/1.
			name: "the all-zero shift ladder",
			tweak: func(cc *pb.ClusterConf) {
				cc.DnBinConf.Bin0Shift = 0
				cc.DnBinConf.Bin1Shift = 0
				cc.DnBinConf.Bin2Shift = 0
				cc.DnBinConf.Bin3Shift = 0
			},
			wantErr: "invalid stored conf: dn_bin_conf shifts 0/0/0/0 " +
				"are not a ladder 0 <= bin0 < bin1 < bin2 < bin3 <= 63",
		},
		{
			name: "a shift above 63",
			tweak: func(cc *pb.ClusterConf) {
				cc.DnBinConf.Bin3Shift = 64
			},
			wantErr: "invalid stored conf: dn_bin_conf shifts 0/4/8/64 " +
				"are not a ladder 0 <= bin0 < bin1 < bin2 < bin3 <= 63",
		},
		{
			name: "dn_batch_size zero",
			tweak: func(cc *pb.ClusterConf) {
				cc.AllocConf.DnBatchSize = 0
			},
			wantErr: "invalid stored conf: " +
				"alloc_conf.dn_batch_size 0 is outside [1, 1024]",
		},
		{
			name: "dn_batch_size past the max",
			tweak: func(cc *pb.ClusterConf) {
				cc.AllocConf.DnBatchSize = 1025
			},
			wantErr: "invalid stored conf: " +
				"alloc_conf.dn_batch_size 1025 is outside [1, 1024]",
		},
		{
			name: "cn_batch_size zero",
			tweak: func(cc *pb.ClusterConf) {
				cc.AllocConf.CnBatchSize = 0
			},
			wantErr: "invalid stored conf: " +
				"alloc_conf.cn_batch_size 0 is outside [1, 1024]",
		},
		{
			name: "an interval past the max",
			tweak: func(cc *pb.ClusterConf) {
				cc.HealthCheckConf.SideInterval = 3601
			},
			wantErr: "invalid stored conf: " +
				"health_check_conf.side_interval 3601 is outside [1, 3600]",
		},
		{
			name: "an interval of zero",
			tweak: func(cc *pb.ClusterConf) {
				cc.HealthCheckConf.CntlrInterval = 0
			},
			wantErr: "invalid stored conf: " +
				"health_check_conf.cntlr_interval 0 is outside [1, 3600]",
		},
		{
			name: "a bdev_conf that does not validate",
			tweak: func(cc *pb.ClusterConf) {
				cc.BdevConf.DmPoolConf.DataBlockSize = 0
			},
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
	}
	for _, tc := range cases {
		cc := testClusterConf()
		if tc.tweak != nil {
			tc.tweak(cc)
		}
		err := ValidateClusterConf(cc)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: got %v, want accepted", tc.name, err)
			}
			continue
		}
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// ---------------------------------------------------------------------------
// Bins (MD4, §6.2)
// ---------------------------------------------------------------------------

func TestDnBinIdx(t *testing.T) {
	ladder := testBinConf()
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
		// The stored 0/4/8/12 ladder: levels 1 / 16 / 256 / 4096.
		{"below level0", ladder, 0, 0, false},
		{"at level0", ladder, 1, 0, true},
		{"below level1", ladder, 15, 0, true},
		{"at level1", ladder, 16, 1, true},
		{"below level2", ladder, 255, 1, true},
		{"at level2", ladder, 256, 2, true},
		{"below level3", ladder, 4095, 2, true},
		{"at level3", ladder, 4096, 3, true},
		{"far above level3", ladder, 1 << 40, 3, true},
		// An unwritten dn_bin_conf is NOT resolved to that ladder any more
		// (§7): binLevels shifts the four zeros it is given, every level is
		// 1 << 0, and any free count at all lands in the top bin. An op that
		// reaches it without refusing an unusable conf first gets these
		// nonsense levels deliberately — a plausible default is what a stored
		// key could not be deleted under, and capacity.go says where the
		// refusals live — and these two cases pin the shift itself.
		{"an unwritten ladder shifts as stored", &pb.DnBinConf{}, 16, 3, true},
		{"an unwritten ladder still floors at level0", nil, 0, 0, false},
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
	// The stored 0/4/8/12 ladder, whose bin0 floor is 1 extent, against a
	// stored ladder that raises that floor to 16.
	ladder := testBinConf()
	bigBin0 := &pb.DnBinConf{
		Bin0Shift: 4, Bin1Shift: 5, Bin2Shift: 6, Bin3Shift: 7,
	}
	cases := []struct {
		name string
		dn   *pb.DnConf
		conf *pb.DnBinConf
		want bool
	}{
		{"healthy", healthyDn(), ladder, true},
		{"nil record", nil, ladder, false},
		{"side_ptr_list at the cap", full, ladder, false},
		{"side_ptr_list below the cap", almostFull, ladder, true},
		{"err_epoch set", unhealthy, ladder, false},
		{"disabled", disabled, ladder, false},
		{"no free extent", empty, ladder, false},
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
	cc := testClusterConf()
	// free 10 under the stored 0/4/8/12 ladder is bin 0 (levels
	// 1/16/256/4096); MaintainDnCapacity shifts that ladder as stored (§7).
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
