package model

import (
	"context"
	"errors"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The fixture one SP is built from. Ids are arbitrary but distinct, so that a
// wrong key shows up as a missing object rather than as a lucky hit.
const (
	fixtureSpId    = uint64(17)
	fixtureSpName  = "pool1"
	fixtureNqn     = "nqn.2024-01.io.dnv:pool1"
	fixtureDnAddrA = "dn-a:9000"
	fixtureDnAddrB = "dn-b:9000"
	fixtureDnAddrC = "dn-c:9000"
	fixtureCnAddrA = "cn-a:9000"
	fixtureCnAddrB = "cn-b:9000"
)

// fixtureSpConf is the SpConf every sub-object below is listed in.
func fixtureSpConf() *pb.SpConf {
	return &pb.SpConf{
		SpId:          fixtureSpId,
		ShardCode:     4,
		NextId:        100,
		NextDevId:     5,
		CntlrIdList:   []uint64{21, 22},
		SliceIdList:   []uint64{31},
		TdNameList:    []string{"td0", "td1"},
		NqnList:       []string{fixtureNqn},
		CloneNameList: []string{"clone0"},
		XferNameList:  []string{"xfer0"},
		MigrNameList:  []string{"migr0"},
	}
}

// writeSp writes the whole fixture: one slice with a meta group, a data group
// and a spare leg, so that the loader's side walk has to reach a spare's side
// to find dn-c.
func writeSp(t *testing.T, cli *etcdutil.Client, cid uint64) {
	t.Helper()
	conf := fixtureSpConf()
	mustPut(t, cli, SpConfKey(cid, fixtureSpName), conf)
	mustPut(t, cli, CntlrKey(cid, fixtureSpId, 21), &pb.Cntlr{
		AddrPort: fixtureCnAddrA,
		Primary:  true,
	})
	mustPut(t, cli, CntlrKey(cid, fixtureSpId, 22), &pb.Cntlr{
		AddrPort: fixtureCnAddrB,
	})
	mustPut(t, cli, SliceKey(cid, fixtureSpId, 31), &pb.Slice{
		SliceIdx: 0,
		MetaGrpList: []*pb.Group{{
			GrpId:  41,
			ExtCnt: 1,
			LegList: []*pb.Leg{{
				LegId: 51,
				SideList: []*pb.Side{{
					SideId:   61,
					AddrPort: fixtureDnAddrA,
				}},
			}},
		}},
		DataGrpList: []*pb.Group{{
			GrpId:  42,
			ExtCnt: 8,
			LegList: []*pb.Leg{{
				LegId: 52,
				SideList: []*pb.Side{{
					SideId:   62,
					AddrPort: fixtureDnAddrB,
				}},
			}},
			SpareLegList: []*pb.Leg{{
				LegId:  53,
				LegIdx: 1,
				SideList: []*pb.Side{{
					SideId:   63,
					AddrPort: fixtureDnAddrC,
				}},
			}},
		}},
	})
	mustPut(t, cli, ThinDeviceKey(cid, fixtureSpId, "td0"), &pb.ThinDevice{
		TdId: 71, DevId: 1, Size: 1 << 30, Created: true,
	})
	mustPut(t, cli, ThinDeviceKey(cid, fixtureSpId, "td1"), &pb.ThinDevice{
		TdId: 72, DevId: 2, Size: 1 << 30,
	})
	mustPut(t, cli, SubsystemKey(cid, fixtureSpId, fixtureNqn), &pb.Subsystem{
		SsId: 81, Serial: "s0", Model: "m0",
	})
	mustPut(t, cli, CloneKey(cid, fixtureSpId, "clone0"), &pb.Clone{
		CloneId: 91,
	})
	mustPut(t, cli, TransferKey(cid, fixtureSpId, "xfer0"), &pb.Transfer{
		XferId: 92,
	})
	mustPut(t, cli, MigrationKey(cid, fixtureSpId, "migr0"), &pb.Migration{
		MigrId: 93, SrcSideId: 62, DstSideId: 63, BmCnt: 2,
	})
	// Two chunks of source slice 0 and one of source slice 1: the fixture's
	// (1, 0) and (0, 0) share a bm_idx and are still two chunks, because a
	// clone chunk is addressed by the pair (architecture.md §9.6).
	for _, pair := range [][2]uint32{{0, 0}, {0, 1}, {1, 0}} {
		mustPut(
			t, cli,
			CloneBitmapKey(cid, fixtureSpId, "clone0", pair[0], pair[1]),
			&pb.CloneBitmap{Bitmap: []byte{byte(pair[0]), byte(pair[1])}},
		)
	}
	for bmIdx := uint32(0); bmIdx < 2; bmIdx++ {
		mustPut(
			t, cli,
			MigrBitmapKey(cid, fixtureSpId, "migr0", bmIdx),
			&pb.MigrBitmap{Bitmap: []byte{byte(bmIdx)}},
		)
	}
	for i, addrPort := range []string{
		fixtureDnAddrA, fixtureDnAddrB, fixtureDnAddrC,
	} {
		mustPut(t, cli, DnConfKey(cid, addrPort), &pb.DnConf{
			DnId:       uint64(i + 1),
			Location:   "rack" + string(rune('0'+i)),
			FreeExtCnt: 100,
		})
	}
	for i, addrPort := range []string{fixtureCnAddrA, fixtureCnAddrB} {
		mustPut(t, cli, CnConfKey(cid, addrPort), &pb.CnConf{
			CnId:       uint64(i + 1),
			Location:   "rack" + string(rune('0'+i)),
			FreeExtCnt: 50,
		})
	}
}

// bmIndexes renders a migration chunk list for comparison; a migration chunk
// names no slice, so its index is its whole address.
func bmIndexes(chunks []BmChunk) []uint32 {
	out := make([]uint32, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, chunk.Idx)
	}
	return out
}

// bmPairs renders a clone chunk list as the (src_slice_idx, bm_idx) addresses
// it carries (architecture.md §9.6).
func bmPairs(chunks []BmChunk) [][2]uint32 {
	out := make([][2]uint32, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, [2]uint32{chunk.SliceIdx, chunk.Idx})
	}
	return out
}

func equalPairs(got [][2]uint32, want [][2]uint32) bool {
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

func equalUint32s(got []uint32, want []uint32) bool {
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

// TestLoadSpHappyPath loads the whole fixture and checks every field MD3
// promises (MD9).
func TestLoadSpHappyPath(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	writeSp(t, cli, cid)

	state, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if state.Rev <= 0 {
		t.Errorf("Rev = %d, want the snapshot's store revision", state.Rev)
	}
	if state.Conf.GetSpId() != fixtureSpId {
		t.Errorf("Conf.sp_id = %d", state.Conf.GetSpId())
	}
	if len(state.Missing) != 0 {
		t.Errorf("Missing = %v, want none", state.Missing)
	}
	if len(state.Cntlrs) != 2 ||
		state.Cntlrs[21].GetAddrPort() != fixtureCnAddrA ||
		!state.Cntlrs[21].GetPrimary() ||
		state.Cntlrs[22].GetAddrPort() != fixtureCnAddrB {
		t.Errorf("Cntlrs = %v", state.Cntlrs)
	}
	if len(state.Slices) != 1 ||
		state.Slices[31].GetDataGrpList()[0].GetGrpId() != 42 {
		t.Errorf("Slices = %v", state.Slices)
	}
	if !equalStrings(state.TdNames, []string{"td0", "td1"}) {
		t.Errorf("TdNames = %v", state.TdNames)
	}
	if len(state.Tds) != 2 || state.Tds[0].GetTdId() != 71 ||
		!state.Tds[0].GetCreated() || state.Tds[1].GetTdId() != 72 ||
		state.Tds[1].GetCreated() {
		t.Errorf("Tds = %v", state.Tds)
	}
	if state.Subsystems[fixtureNqn].GetSsId() != 81 {
		t.Errorf("Subsystems = %v", state.Subsystems)
	}
	if state.Clones["clone0"].GetCloneId() != 91 ||
		state.Xfers["xfer0"].GetXferId() != 92 ||
		state.Migrs["migr0"].GetMigrId() != 93 {
		t.Errorf(
			"Clones/Xfers/Migrs = %v %v %v",
			state.Clones, state.Xfers, state.Migrs,
		)
	}
	// Three distinct chunks, ascending by pair: (1, 0) is not (0, 0) with
	// another name, it is the first chunk of another source slice.
	if !equalPairs(
		bmPairs(state.CloneBmIdx["clone0"]),
		[][2]uint32{{0, 0}, {0, 1}, {1, 0}},
	) {
		t.Errorf("CloneBmIdx = %v", state.CloneBmIdx)
	}
	if !equalUint32s(bmIndexes(state.MigrBmIdx["migr0"]), []uint32{0, 1}) {
		t.Errorf("MigrBmIdx = %v", state.MigrBmIdx)
	}
	for _, chunk := range state.MigrBmIdx["migr0"] {
		if chunk.SliceIdx != 0 {
			t.Errorf("migration chunk %d has slice_idx %d, want 0",
				chunk.Idx, chunk.SliceIdx)
		}
	}
	for _, chunk := range state.CloneBmIdx["clone0"] {
		if chunk.ModRev <= 0 {
			t.Errorf("chunk (%d, %d) has no mod_revision (BM5 memoizes it)",
				chunk.SliceIdx, chunk.Idx)
		}
	}
	// The spare leg's side counts: dn-c is only reachable through
	// spare_leg_list.
	if len(state.DnByAddr) != 3 ||
		state.DnByAddr[fixtureDnAddrC].GetDnId() != 3 {
		t.Errorf("DnByAddr = %v", state.DnByAddr)
	}
	if len(state.CnByAddr) != 2 ||
		state.CnByAddr[fixtureCnAddrB].GetCnId() != 2 {
		t.Errorf("CnByAddr = %v", state.CnByAddr)
	}
}

// TestLoadSpNotFound checks the MD3 sentinel: a deleted SP is not an error the
// worker retries, it is "nothing left to drive".
func TestLoadSpNotFound(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)

	state, err := LoadSp(ctx, cli, cid, "no-such-pool")
	if state != nil {
		t.Errorf("LoadSp returned a state for a missing SpConf")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadSp err = %v, want ErrNotFound", err)
	}
}

// TestLoadSpMissing checks that a listed-but-absent sub-object is reported and
// does not fail the load (MD3).
func TestLoadSpMissing(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	conf := fixtureSpConf()
	// Nothing but the SpConf and one cntlr exists.
	mustPut(t, cli, SpConfKey(cid, fixtureSpName), conf)
	mustPut(t, cli, CntlrKey(cid, fixtureSpId, 21), &pb.Cntlr{
		AddrPort: fixtureCnAddrA,
	})

	state, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	want := []string{
		CntlrKey(cid, fixtureSpId, 22),
		SliceKey(cid, fixtureSpId, 31),
		ThinDeviceKey(cid, fixtureSpId, "td0"),
		ThinDeviceKey(cid, fixtureSpId, "td1"),
		SubsystemKey(cid, fixtureSpId, fixtureNqn),
		CloneKey(cid, fixtureSpId, "clone0"),
		TransferKey(cid, fixtureSpId, "xfer0"),
		MigrationKey(cid, fixtureSpId, "migr0"),
		CnConfKey(cid, fixtureCnAddrA),
	}
	if !equalStrings(state.Missing, want) {
		t.Errorf("Missing = %v, want %v", state.Missing, want)
	}
	if len(state.Cntlrs) != 1 || state.Cntlrs[21] == nil {
		t.Errorf("Cntlrs = %v, want only the one that exists", state.Cntlrs)
	}
	if len(state.Tds) != 0 || len(state.TdNames) != 0 {
		t.Errorf("Tds/TdNames = %v %v", state.Tds, state.TdNames)
	}
	// A clone whose record is gone gets no bitmap scan at all.
	if _, ok := state.CloneBmIdx["clone0"]; ok {
		t.Error("CloneBmIdx has an entry for a clone that does not exist")
	}
}

// TestLoadSpTdAlignment checks that Tds and TdNames stay index-aligned when
// one listed td is missing (MD3).
func TestLoadSpTdAlignment(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	conf := &pb.SpConf{
		SpId:       fixtureSpId,
		TdNameList: []string{"td0", "gone", "td2"},
	}
	mustPut(t, cli, SpConfKey(cid, fixtureSpName), conf)
	mustPut(t, cli, ThinDeviceKey(cid, fixtureSpId, "td0"),
		&pb.ThinDevice{TdId: 1})
	mustPut(t, cli, ThinDeviceKey(cid, fixtureSpId, "td2"),
		&pb.ThinDevice{TdId: 3})

	state, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if !equalStrings(state.TdNames, []string{"td0", "td2"}) {
		t.Fatalf("TdNames = %v", state.TdNames)
	}
	if len(state.Tds) != 2 || state.Tds[0].GetTdId() != 1 ||
		state.Tds[1].GetTdId() != 3 {
		t.Fatalf("Tds = %v", state.Tds)
	}
	if !equalStrings(
		state.Missing,
		[]string{ThinDeviceKey(cid, fixtureSpId, "gone")},
	) {
		t.Errorf("Missing = %v", state.Missing)
	}
}

// TestLoadSpEmptyBitmapIndex checks that a clone with no chunk yet still gets
// an entry, so that a caller can tell "no chunks" from "not loaded" (MD3).
func TestLoadSpEmptyBitmapIndex(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	mustPut(t, cli, SpConfKey(cid, fixtureSpName), &pb.SpConf{
		SpId:          fixtureSpId,
		CloneNameList: []string{"clone0"},
	})
	mustPut(t, cli, CloneKey(cid, fixtureSpId, "clone0"), &pb.Clone{
		CloneId: 1,
	})

	state, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	chunks, ok := state.CloneBmIdx["clone0"]
	if !ok {
		t.Fatal("CloneBmIdx has no entry for a loaded clone")
	}
	if len(chunks) != 0 {
		t.Errorf("CloneBmIdx[clone0] = %v, want empty", chunks)
	}
}

// TestLoadSpSkipsOldFormatCloneBmKeys is MD3's whole compatibility story under
// test: a key of the superseded one-value-per-source-slice format sits under
// the very prefix MD3 scans, and the load drops it rather than failing or
// inventing an address for it. Nothing tolerates it, nothing deletes it.
func TestLoadSpSkipsOldFormatCloneBmKeys(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	mustPut(t, cli, SpConfKey(cid, fixtureSpName), &pb.SpConf{
		SpId:          fixtureSpId,
		CloneNameList: []string{"clone0"},
	})
	mustPut(t, cli, CloneKey(cid, fixtureSpId, "clone0"), &pb.Clone{
		CloneId: 1,
	})
	mustPut(
		t, cli,
		CloneBitmapKey(cid, fixtureSpId, "clone0", 3, 0),
		&pb.CloneBitmap{Bitmap: []byte{0xff}},
	)
	// The superseded key: clone_name followed by ONE index field.
	mustPut(
		t, cli,
		joinKey(
			common.DnvPrefix, kindCloneBitmap,
			idField(cid), idField(fixtureSpId), "clone0", bmIdxField(3),
		),
		&pb.CloneBitmap{Bitmap: []byte{0xff}},
	)

	state, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if !equalPairs(
		bmPairs(state.CloneBmIdx["clone0"]), [][2]uint32{{3, 0}},
	) {
		t.Errorf(
			"CloneBmIdx = %v, want only the pair-addressed chunk",
			state.CloneBmIdx,
		)
	}
}

// TestLoadSpRevAdvances checks that the reported revision is the store's, and
// that a later write is picked up by a later load: the bitmap scans are pinned
// to SpState.Rev, so a stale revision would freeze a bitmap index forever.
func TestLoadSpRevAdvances(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	mustPut(t, cli, SpConfKey(cid, fixtureSpName), &pb.SpConf{
		SpId:         fixtureSpId,
		MigrNameList: []string{"migr0"},
	})
	mustPut(t, cli, MigrationKey(cid, fixtureSpId, "migr0"), &pb.Migration{
		MigrId: 1,
	})
	mustPut(t, cli, MigrBitmapKey(cid, fixtureSpId, "migr0", 0),
		&pb.MigrBitmap{Bitmap: []byte{1}})

	first, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if !equalUint32s(bmIndexes(first.MigrBmIdx["migr0"]), []uint32{0}) {
		t.Fatalf("first MigrBmIdx = %v", first.MigrBmIdx)
	}

	mustPut(t, cli, MigrBitmapKey(cid, fixtureSpId, "migr0", 1),
		&pb.MigrBitmap{Bitmap: []byte{2}})

	second, err := LoadSp(ctx, cli, cid, fixtureSpName)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if second.Rev <= first.Rev {
		t.Errorf("Rev did not advance: %d then %d", first.Rev, second.Rev)
	}
	if !equalUint32s(bmIndexes(second.MigrBmIdx["migr0"]), []uint32{0, 1}) {
		t.Errorf("second MigrBmIdx = %v", second.MigrBmIdx)
	}
	// The first load's view is unchanged by the later write: it was read at
	// one revision.
	if !equalUint32s(bmIndexes(first.MigrBmIdx["migr0"]), []uint32{0}) {
		t.Errorf("first load's index changed under it: %v", first.MigrBmIdx)
	}
}
