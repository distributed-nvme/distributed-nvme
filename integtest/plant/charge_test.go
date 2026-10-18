// Tests of the bookkeeping PutClone and PutMigr write inside their
// transactions, over an in-memory STM: the metadata units a planted copy
// costs (architecture.md, Metadata units), PutClone's charge to the CN of
// every cntlr of the pool and PutMigr's charge to its destination DN, and how
// PutClone finds the td it sizes a clone by.
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

// TestCloneChargePerCn pins PutClone's charge (architecture.md, Metadata
// units): the clone's units once per cntlr on the CN its record names, one
// write per CN, the capacity value following the record, no CnRev, and no
// refusal for room — the charge model.FinishCloneDelete gives back.
func TestCloneChargePerCn(t *testing.T) {
	const (
		cid    = uint64(0xc1d)
		spId   = uint64(7)
		cnA    = "10.0.0.1:29700"
		cnB    = "10.0.0.1:29701"
		cnGone = "10.0.0.1:29702"
		free   = uint64(64)
		units  = uint64(4)
	)
	// fixture plants a pool whose cntlr id i+1 sits on cntlrCns[i] (an empty
	// addr is a listed cntlr with no key) and the CNs cnA and cnB, cnA with
	// usedA units already used. cnGone has no CnConf.
	fixture := func(usedA uint64, cntlrCns ...string) (*memStm, *pb.SpConf) {
		m := newMemStm()
		conf := &pb.SpConf{SpId: spId}
		for i, addrPort := range cntlrCns {
			cntlrId := uint64(i + 1)
			conf.CntlrIdList = append(conf.CntlrIdList, cntlrId)
			if addrPort == "" {
				continue
			}
			m.seed(model.CntlrKey(cid, spId, cntlrId),
				&pb.Cntlr{AddrPort: addrPort})
		}
		m.seed(model.CnConfKey(cid, cnA), &pb.CnConf{
			CnId: 1, Location: "rack0", TotalExtCnt: free,
			FreeExtCnt: free, UsedMetaUnits: usedA,
		})
		m.seed(model.CnConfKey(cid, cnB), &pb.CnConf{
			CnId: 2, Location: "rack1", TotalExtCnt: free,
			FreeExtCnt: free,
		})
		return m, conf
	}
	// used reads a CN's count from its record and from its capacity value,
	// failing when the two disagree or the key is gone.
	used := func(t *testing.T, m *memStm, addrPort string) uint64 {
		t.Helper()
		cn := &pb.CnConf{}
		if !m.peek(model.CnConfKey(cid, addrPort), cn) {
			t.Fatalf("%s: no CnConf", addrPort)
		}
		value := &pb.CnCapacity{}
		if !m.peek(model.CnCapacityKey(cid, free, addrPort), value) {
			t.Fatalf("%s: the capacity key is gone", addrPort)
		}
		if value.GetUsedMetaUnits() != cn.GetUsedMetaUnits() {
			t.Fatalf("%s: capacity value carries %d used units, the "+
				"record %d", addrPort, value.GetUsedMetaUnits(),
				cn.GetUsedMetaUnits())
		}
		if value.GetLocation() != cn.GetLocation() {
			t.Fatalf("%s: capacity value location %q, the record %q",
				addrPort, value.GetLocation(), cn.GetLocation())
		}
		return cn.GetUsedMetaUnits()
	}
	// onlyNodeWrites fails on any write but a CnConf or a capacity key, and
	// on a second write of one key: a units-only charge bumps no CnRev.
	onlyNodeWrites := func(t *testing.T, m *memStm) {
		t.Helper()
		for key, cnt := range m.puts {
			if !strings.HasPrefix(key, common.DnvPrefix+" cn_conf ") &&
				!strings.HasPrefix(key, common.DnvPrefix+" cn_capacity ") {
				t.Errorf("the charge wrote %q", key)
			}
			if cnt != 1 {
				t.Errorf("the charge wrote %q %d times, want once", key, cnt)
			}
		}
	}

	t.Run("two cntlrs on two CNs charge each CN once", func(t *testing.T) {
		m, conf := fixture(10, cnA, cnB)
		charged, err := chargeCloneCns(m, cid, conf, units)
		if err != nil {
			t.Fatalf("chargeCloneCns: %v", err)
		}
		if got := used(t, m, cnA); got != 10+units {
			t.Errorf("cnA used %d, want %d", got, 10+units)
		}
		if got := used(t, m, cnB); got != units {
			t.Errorf("cnB used %d, want %d", got, units)
		}
		if len(charged) != 2 || charged[cnA] != units ||
			charged[cnB] != units {
			t.Errorf("charged %v, want %d on each of %s and %s",
				charged, units, cnA, cnB)
		}
		onlyNodeWrites(t, m)
	})
	t.Run("two cntlrs on one CN charge it twice in one write",
		func(t *testing.T) {
			m, conf := fixture(0, cnA, cnB, cnA)
			charged, err := chargeCloneCns(m, cid, conf, units)
			if err != nil {
				t.Fatalf("chargeCloneCns: %v", err)
			}
			if got := used(t, m, cnA); got != 2*units {
				t.Errorf("cnA used %d, want %d", got, 2*units)
			}
			if got := used(t, m, cnB); got != units {
				t.Errorf("cnB used %d, want %d", got, units)
			}
			if charged[cnA] != 2*units {
				t.Errorf("charged %v, want %d on %s", charged, 2*units, cnA)
			}
			onlyNodeWrites(t, m)
		})
	t.Run("no cntlr charges nothing", func(t *testing.T) {
		m, conf := fixture(0)
		charged, err := chargeCloneCns(m, cid, conf, units)
		if err != nil || len(charged) != 0 || len(m.puts) != 0 {
			t.Errorf("charged %v, err %v, writes %v; want none",
				charged, err, m.puts)
		}
	})
	t.Run("zero units read nothing", func(t *testing.T) {
		m, conf := fixture(0, cnA, cnB)
		charged, err := chargeCloneCns(m, cid, conf, 0)
		if err != nil || len(charged) != 0 {
			t.Errorf("charged %v, err %v; want nothing", charged, err)
		}
		if len(m.gets) != 0 || len(m.puts) != 0 {
			t.Errorf("zero units read %v and wrote %v", m.gets, m.puts)
		}
	})
	t.Run("a CN past its ceiling is charged all the same",
		func(t *testing.T) {
			m, conf := fixture(common.CnCloneMetaUnitCnt-1, cnA)
			if _, err := chargeCloneCns(m, cid, conf, units); err != nil {
				t.Fatalf("chargeCloneCns refused a full CN: %v", err)
			}
			want := uint64(common.CnCloneMetaUnitCnt - 1 + units)
			if got := used(t, m, cnA); got != want {
				t.Errorf("cnA used %d, want %d", got, want)
			}
			cn := &pb.CnConf{}
			m.peek(model.CnConfKey(cid, cnA), cn)
			if free := model.CnFreeMetaUnits(cn); free != 0 {
				t.Errorf("cnA past its ceiling has %d free units, want 0",
					free)
			}
		})
	t.Run("a listed cntlr with no key refuses before any write",
		func(t *testing.T) {
			m, conf := fixture(0, cnA, "", cnB)
			_, err := chargeCloneCns(m, cid, conf, units)
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("chargeCloneCns = %v, want a \"not found\" "+
					"refusal", err)
			}
			if len(m.puts) != 0 {
				t.Errorf("the refusal wrote %v", m.puts)
			}
		})
	t.Run("a CN that is gone is skipped", func(t *testing.T) {
		m, conf := fixture(0, cnA, cnGone)
		charged, err := chargeCloneCns(m, cid, conf, units)
		if err != nil {
			t.Fatalf("chargeCloneCns: %v", err)
		}
		if _, ok := charged[cnGone]; ok {
			t.Errorf("charged %v names the CN that is gone", charged)
		}
		if m.puts[model.CnConfKey(cid, cnGone)] != 0 {
			t.Errorf("the charge wrote the CnConf of the CN that is gone")
		}
		if got := used(t, m, cnA); got != units {
			t.Errorf("cnA used %d, want %d", got, units)
		}
		onlyNodeWrites(t, m)
	})
}

// TestMigrDstCharge pins PutMigr's charge of its destination DN
// (architecture.md, Metadata units): the side pointer, the extents and the
// migration's units in the one write of the record, the capacity value
// following the record, one DnRev bump, no refusal past the DN's metadata
// area, and a DN short of extents refused before any write — the charge
// FinishMigration and CancelMigration give back.
func TestMigrDstCharge(t *testing.T) {
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
	// charged checks one successful charge: the record and its capacity
	// value carry the extents and the units, the key moved with the free
	// count, the revision went up once, and nothing else was written.
	charged := func(t *testing.T, m *memStm, dnRev, used uint64) {
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
		ptrs := dn.GetSidePtrList()
		if len(ptrs) != 2 || !proto.Equal(ptrs[0], held) ||
			!proto.Equal(ptrs[1], ptr) {
			t.Errorf("side_ptr_list %v, want %v then %v", ptrs, held, ptr)
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
		dnRev, err := chargeMigrDst(m, cid, cc, addr, dn, ptr, extCnt, units)
		if err != nil {
			t.Fatalf("chargeMigrDst: %v", err)
		}
		charged(t, m, dnRev, 10)
	})
	t.Run("a DN past its metadata area is charged all the same",
		func(t *testing.T) {
			used := uint64(common.DnCloneMetaUnitCnt - 1)
			m, dn := fixture(t, free, used)
			dnRev, err := chargeMigrDst(
				m, cid, cc, addr, dn, ptr, extCnt, units)
			if err != nil {
				t.Fatalf("chargeMigrDst refused a full DN: %v", err)
			}
			charged(t, m, dnRev, used)
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
			_, err := chargeMigrDst(m, cid, cc, addr, dn, ptr, extCnt, units)
			if err == nil || !strings.Contains(err.Error(), "free_ext_cnt") {
				t.Fatalf("chargeMigrDst = %v, want a free_ext_cnt refusal",
					err)
			}
			if len(m.puts) != 0 {
				t.Errorf("the refusal wrote %v", m.puts)
			}
		})
}

// TestPlantedCloneTd pins how PutClone's DstTd finds the td it sizes the
// clone by: a name first, then a td_id among the pool's tds, and a td_id no
// td carries planted unsized.
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
		size  uint64
		found bool
	}{
		{"td0", 0x10, 10 << 30, true},
		{"0x10", 0x10, 10 << 30, true},
		{"16", 0x10, 10 << 30, true},
		{"5", 0x11, 1 << 30, true},
		{"0x99", 0x99, 0, false},
	}
	for _, tc := range cases {
		id, td, err := plantedCloneTd(m, cid, conf, tc.dstTd)
		if err != nil {
			t.Errorf("plantedCloneTd(%q): %v", tc.dstTd, err)
			continue
		}
		if id != tc.id || (td != nil) != tc.found ||
			td.GetSize() != tc.size {
			t.Errorf("plantedCloneTd(%q) = %#x, %v; want %#x, size %d, "+
				"found %t", tc.dstTd, id, td, tc.id, tc.size, tc.found)
		}
	}
	if _, _, err := plantedCloneTd(m, cid, conf, "nope"); err == nil {
		t.Errorf("plantedCloneTd(\"nope\") succeeded, want an error")
	}
}

// TestPlantedUnits pins the units PutClone and PutMigr store and charge
// (architecture.md, Metadata units): the formula at the pool's block size,
// behind the stored conf's gate, and none for a clone with no td to size.
func TestPlantedUnits(t *testing.T) {
	small := &pb.SpConf{BdevConf: testPoolBdev(64 << 10)}
	large := &pb.SpConf{BdevConf: testPoolBdev(1 << 20)}
	broken := &pb.SpConf{BdevConf: testPoolBdev(0)}

	cloneCases := []struct {
		name  string
		conf  *pb.SpConf
		td    *pb.ThinDevice
		units uint64
	}{
		{"the worker suite's drain-case clone", large,
			&pb.ThinDevice{Size: 10 << 30}, 2},
		{"the cn agent's example", small,
			&pb.ThinDevice{Size: 1 << 40}, 5},
		{"no td to size", small, nil, 0},
		{"no td to size, whatever the conf", broken, nil, 0},
	}
	for _, tc := range cloneCases {
		units, err := plantedCloneUnits(tc.conf, tc.td)
		if err != nil || units != tc.units {
			t.Errorf("plantedCloneUnits, %s = %d, %v; want %d",
				tc.name, units, err, tc.units)
		}
	}
	if units, err := plantedCloneUnits(
		broken, &pb.ThinDevice{Size: 10 << 30}); err == nil {
		t.Errorf("plantedCloneUnits on a zero block size = %d, want a "+
			"refusal", units)
	}

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
