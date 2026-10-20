// Tests of the bookkeeping PutSp, PutClone and PutMigr write inside their
// transactions, over an in-memory STM: the metadata units a planted
// migration costs (architecture.md, Metadata units), the DN charge the three
// share (chargeDn) — a pool's sides, a clone meta group's and a migration's
// destination — and how PutClone resolves its destination td.
package plant

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// memStm is an in-memory etcdutil.STM over marshaled values, so a value read
// back is a fresh copy, as it is from etcd, and a staged write is seen by a
// later read. It counts the reads and the writes of every key: "one write
// per node" and "zero units read nothing" are counts.
type memStm struct {
	vals map[string][]byte
	gets map[string]int
	puts map[string]int
}

func newMemStm() *memStm {
	return &memStm{
		vals: make(map[string][]byte),
		gets: make(map[string]int),
		puts: make(map[string]int),
	}
}

func (m *memStm) Get(key string, msg proto.Message) bool {
	m.gets[key]++
	return m.peek(key, msg)
}

func (m *memStm) Put(key string, msg proto.Message) {
	m.seed(key, msg)
	m.puts[key]++
}

func (m *memStm) Del(key string) { delete(m.vals, key) }

func (m *memStm) Rev(key string) int64 { return 0 }

// seed stores a fixture value without counting a write.
func (m *memStm) seed(key string, msg proto.Message) {
	data, err := proto.Marshal(msg)
	if err != nil {
		panic(fmt.Sprintf("memStm: marshaling %q: %v", key, err))
	}
	m.vals[key] = data
}

// peek reads a value for an assertion without counting a read.
func (m *memStm) peek(key string, msg proto.Message) bool {
	data, ok := m.vals[key]
	if !ok {
		return false
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		panic(fmt.Sprintf("memStm: decoding %q: %v", key, err))
	}
	return true
}

// memStm must stay a valid etcdutil.STM.
var _ etcdutil.STM = (*memStm)(nil)

// testPoolBdev is a stored pool BdevConf with blockSize as its block size:
// every member model.ValidateBdevConf requires is concrete, and no
// redund_conf is the redund_none choice.
func testPoolBdev(blockSize uint64) *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   blockSize,
			LowWaterMarkPct: common.DefaultPoolLowWatermarkPct,
		},
		DmRaid0Conf: &pb.DmRaid0Conf{
			StripeSize: common.DefaultDmRaid0StripeSize,
		},
	}
}

// TestDnCharge pins chargeDn, the DN charge of PutSp, PutClone and PutMigr
// (architecture.md, Capacity index keys; Metadata units): the side pointers,
// the extents and the units in the one write of the record, the capacity
// value following the record, one DnRev bump however many pointers, no
// refusal past the DN's metadata area, and a DN short of extents refused
// before any write — the charge a migration's finish or cancel and the two
// drains give back. A migration's destination brings units; a pool's sides
// and a clone meta group's bring none.
func TestDnCharge(t *testing.T) {
	const (
		cid    = uint64(0xc1d)
		spId   = uint64(7)
		dnId   = uint64(0x21)
		shard  = uint32(3)
		addr   = "10.0.0.2:29600"
		total  = uint64(100)
		free   = uint64(64)
		extCnt = uint64(2)
		units  = uint64(3)
		rev    = uint64(41)
	)
	cc := &pb.ClusterConf{DnBinConf: model.ResolveDnBinConf(nil)}
	ptr := &pb.SidePointer{SpId: spId, LegId: 0x31, SideId: 0x32}
	second := &pb.SidePointer{SpId: spId, LegId: 0x33, SideId: 0x34}
	held := &pb.SidePointer{SpId: spId, LegId: 0x11, SideId: 0x12}
	capKey := func(t *testing.T, freeExt uint64) string {
		t.Helper()
		binIdx, ok := model.DnBinIdx(freeExt, cc.GetDnBinConf())
		if !ok {
			t.Fatalf("%d free extents have no bin", freeExt)
		}
		return model.DnCapacityKey(cid, binIdx, freeExt, addr)
	}
	// fixture plants the DN with freeExt free extents, used units already
	// used and one side it already holds, its capacity key as
	// model.MaintainDnCapacity writes it, and its DnRev at rev; it returns
	// the record as an STM reads it.
	fixture := func(t *testing.T, freeExt, used uint64) (*memStm, *pb.DnConf) {
		t.Helper()
		m := newMemStm()
		dn := &pb.DnConf{
			DnId: dnId, ShardCode: shard, Location: "rack2",
			TotalExtCnt: total, FreeExtCnt: freeExt, UsedMetaUnits: used,
			SidePtrList: []*pb.SidePointer{held},
		}
		m.seed(model.DnConfKey(cid, addr), dn)
		m.seed(capKey(t, freeExt), model.DnCapacityValue(dn))
		m.seed(model.DnRevKey(shard, cid, dnId), &pb.DnRev{Revision: rev})
		read := &pb.DnConf{}
		if !m.Get(model.DnConfKey(cid, addr), read) {
			t.Fatalf("no DnConf at %q", addr)
		}
		return m, read
	}
	// charged checks one successful charge of extCnt extents, units units
	// and the pointers ptrs: the record and its capacity value carry them,
	// the key moved with the free count, the revision went up once, and
	// nothing else was written.
	charged := func(t *testing.T, m *memStm, dnRev, used, extCnt, units uint64,
		ptrs ...*pb.SidePointer) {
		t.Helper()
		dn := &pb.DnConf{}
		if !m.peek(model.DnConfKey(cid, addr), dn) {
			t.Fatalf("the charge removed the DnConf")
		}
		if dn.GetFreeExtCnt() != free-extCnt {
			t.Errorf("free_ext_cnt %d, want %d", dn.GetFreeExtCnt(),
				free-extCnt)
		}
		if dn.GetUsedMetaUnits() != used+units {
			t.Errorf("used_meta_units %d, want %d", dn.GetUsedMetaUnits(),
				used+units)
		}
		want := append([]*pb.SidePointer{held}, ptrs...)
		if !equalPtrs(dn.GetSidePtrList(), want) {
			t.Errorf("side_ptr_list %v, want %v", dn.GetSidePtrList(), want)
		}
		if m.peek(capKey(t, free), &pb.DnCapacity{}) {
			t.Errorf("the capacity key of %d free extents is still there",
				free)
		}
		value := &pb.DnCapacity{}
		if !m.peek(capKey(t, free-extCnt), value) {
			t.Fatalf("no capacity key for %d free extents", free-extCnt)
		}
		if !proto.Equal(value, model.DnCapacityValue(dn)) {
			t.Errorf("capacity value %v, the record implies %v", value,
				model.DnCapacityValue(dn))
		}
		got := &pb.DnRev{}
		m.peek(model.DnRevKey(shard, cid, dnId), got)
		if got.GetRevision() != rev+1 || dnRev != rev+1 {
			t.Errorf("DnRev %d, returned %d; want %d once",
				got.GetRevision(), dnRev, rev+1)
		}
		for key, cnt := range m.puts {
			if key != model.DnConfKey(cid, addr) &&
				key != capKey(t, free-extCnt) &&
				key != model.DnRevKey(shard, cid, dnId) {
				t.Errorf("the charge wrote %q", key)
			}
			if cnt != 1 {
				t.Errorf("the charge wrote %q %d times, want once", key, cnt)
			}
		}
	}

	t.Run("the units ride the extents' write", func(t *testing.T) {
		m, dn := fixture(t, free, 10)
		dnRev, err := chargeDn(m, cid, cc, addr, dn,
			[]*pb.SidePointer{ptr}, extCnt, units)
		if err != nil {
			t.Fatalf("chargeDn: %v", err)
		}
		charged(t, m, dnRev, 10, extCnt, units, ptr)
	})
	t.Run("two pointers and no unit, one write and one bump",
		func(t *testing.T) {
			m, dn := fixture(t, free, 10)
			dnRev, err := chargeDn(m, cid, cc, addr, dn,
				[]*pb.SidePointer{ptr, second}, extCnt, 0)
			if err != nil {
				t.Fatalf("chargeDn: %v", err)
			}
			charged(t, m, dnRev, 10, extCnt, 0, ptr, second)
		})
	t.Run("a DN past its metadata area is charged all the same",
		func(t *testing.T) {
			used := uint64(common.DnCloneMetaUnitCnt - 1)
			m, dn := fixture(t, free, used)
			dnRev, err := chargeDn(m, cid, cc, addr, dn,
				[]*pb.SidePointer{ptr}, extCnt, units)
			if err != nil {
				t.Fatalf("chargeDn refused a full DN: %v", err)
			}
			charged(t, m, dnRev, used, extCnt, units, ptr)
			after := &pb.DnConf{}
			m.peek(model.DnConfKey(cid, addr), after)
			if room := model.DnFreeMetaUnits(after); room != 0 {
				t.Errorf("a DN past its area has %d free units, want 0",
					room)
			}
		})
	t.Run("a DN short of extents refuses before any write",
		func(t *testing.T) {
			m, dn := fixture(t, extCnt-1, 0)
			_, err := chargeDn(m, cid, cc, addr, dn,
				[]*pb.SidePointer{ptr}, extCnt, units)
			if err == nil || !strings.Contains(err.Error(), "free_ext_cnt") {
				t.Fatalf("chargeDn = %v, want a free_ext_cnt refusal", err)
			}
			if len(m.puts) != 0 {
				t.Errorf("the refusal wrote %v", m.puts)
			}
		})
}

// TestPlantedCloneTd pins how PutClone's DstTd resolves to a td_id: a name
// first, so a td named like an id is that td, then the text as a td_id, which
// need not name a td of the pool.
func TestPlantedCloneTd(t *testing.T) {
	const cid, spId = uint64(0xc1d), uint64(7)
	m := newMemStm()
	conf := &pb.SpConf{SpId: spId, TdNameList: []string{"td0", "5"}}
	m.seed(model.ThinDeviceKey(cid, spId, "td0"),
		&pb.ThinDevice{TdId: 0x10, Size: 10 << 30})
	// A td NAMED like an id: the name is tried first, so "5" is this td and
	// never td_id 5.
	m.seed(model.ThinDeviceKey(cid, spId, "5"),
		&pb.ThinDevice{TdId: 0x11, Size: 1 << 30})
	cases := []struct {
		dstTd string
		id    uint64
	}{
		{"td0", 0x10},
		{"0x10", 0x10},
		{"16", 0x10},
		{"5", 0x11},
		{"0x99", 0x99},
	}
	for _, tc := range cases {
		id, err := plantedCloneTd(m, cid, conf, tc.dstTd)
		if err != nil {
			t.Errorf("plantedCloneTd(%q): %v", tc.dstTd, err)
			continue
		}
		if id != tc.id {
			t.Errorf("plantedCloneTd(%q) = %#x, want %#x", tc.dstTd, id,
				tc.id)
		}
	}
	if _, err := plantedCloneTd(m, cid, conf, "nope"); err == nil {
		t.Errorf("plantedCloneTd(\"nope\") succeeded, want an error")
	}
}

// TestPlantedUnits pins the units PutMigr stores and charges
// (architecture.md, Metadata units): the formula at the pool's block size,
// behind the stored conf's gate.
func TestPlantedUnits(t *testing.T) {
	small := &pb.SpConf{BdevConf: testPoolBdev(64 << 10)}
	large := &pb.SpConf{BdevConf: testPoolBdev(1 << 20)}
	broken := &pb.SpConf{BdevConf: testPoolBdev(0)}

	migrCases := []struct {
		name       string
		conf       *pb.SpConf
		extCnt     uint64
		extentSize uint64
		units      uint64
	}{
		{"the worker suite's bitmap-case migration", large,
			2, common.MinDnExtSize, 2},
		{"the gateway suite's smoke migration", large,
			2, 1 << 30, 2},
		{"the ceiling case's migration that fills a DN", small,
			12032, 1 << 30, common.DnCloneMetaUnitCnt},
	}
	for _, tc := range migrCases {
		units, err := plantedMigrUnits(tc.conf, tc.extCnt, tc.extentSize)
		if err != nil || units != tc.units {
			t.Errorf("plantedMigrUnits, %s = %d, %v; want %d",
				tc.name, units, err, tc.units)
		}
	}
	if units, err := plantedMigrUnits(broken, 2, 1<<30); err == nil {
		t.Errorf("plantedMigrUnits on a zero block size = %d, want a "+
			"refusal", units)
	}
}
