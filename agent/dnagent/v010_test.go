package dnagent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The v0.1.0 samples of one disk node: the local store v0.1.0's dn agent
// wrote and its disk's header block and table slots (pb/samples_test.go says
// how they were made). Its disk was the data offset and 64 extents long.
const (
	v010Dir        = "../../pb/testdata/v0.1.0"
	v010DiskExtCnt = 64
)

// v010Store is the dn sample's store: each file's name and bytes, the dn
// file's request and each side file's.
type v010Store struct {
	files map[string][]byte
	dn    *pb.SyncupDnRequest
	sides []*pb.SyncupSideRequest
	bms   []*pb.PushMigrBitmapRequest
}

// readV010DnStore reads the dn sample's store, each file by v0.1.0's
// prefix of its kind, not by the store's names for them today.
func readV010DnStore(t *testing.T) v010Store {
	t.Helper()
	dir := filepath.Join(v010Dir, "store", "dn")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := v010Store{files: map[string][]byte{}}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		st.files[e.Name()] = raw
		var msg proto.Message
		switch {
		case strings.HasPrefix(e.Name(), "dn-"):
			st.dn = &pb.SyncupDnRequest{}
			msg = st.dn
		case strings.HasPrefix(e.Name(), "side-"):
			req := &pb.SyncupSideRequest{}
			st.sides = append(st.sides, req)
			msg = req
		case strings.HasPrefix(e.Name(), "migr-bm-"):
			req := &pb.PushMigrBitmapRequest{}
			st.bms = append(st.bms, req)
			msg = req
		default:
			t.Fatalf("%s: no store kind of a disk node", e.Name())
		}
		if err := proto.Unmarshal(raw, msg); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
	}
	if st.dn == nil || len(st.sides) == 0 || len(st.bms) == 0 {
		t.Fatalf("the dn sample lacks a dn file, a side or a chunk")
	}
	return st
}

// v010DiskNode is a fake node whose disk holds the v0.1.0 disk sample, at
// the sample's size.
func v010DiskNode(t *testing.T, extentSize uint64) (*fakeNode, uint64) {
	t.Helper()
	node := newFakeNode()
	size := common.DnDataOffset + v010DiskExtCnt*extentSize
	node.devSize[testDisk] = size
	node.devNo[testDisk] = "253:0"
	for name, off := range map[string]uint64{
		"header.bin": common.DnHeaderOffset,
		"slot-a.bin": common.DnTableSlotAOffset,
		"slot-b.bin": common.DnTableSlotBOffset,
	} {
		raw, err := os.ReadFile(filepath.Join(v010Dir, "disk", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := node.writeBlock(context.Background(), testDisk, off, raw); err != nil {
			t.Fatal(err)
		}
	}
	node.Reset()
	return node, size
}

// TestDiskMetaReadsTheV010Disk reads a disk v0.1.0's dn agent formatted and
// wrote (dnagent.md DN5; architecture.md, [D13]): the format check passes
// with the identity the dn file names and writes nothing, and every side the
// store names has its record, of its length, done zeroing, beside the
// migration's metadata record.
func TestDiskMetaReadsTheV010Disk(t *testing.T) {
	st := readV010DnStore(t)
	node, size := v010DiskNode(t, st.dn.GetExtentSize())
	ctx := context.Background()
	meta := NewDiskMeta(node.osClient(), testDisk)
	meta.SetDiskSize(size)
	refuse := func(context.Context) error {
		t.Error("the v0.1.0 disk reads as blank")
		return nil
	}
	if err := meta.EnsureFormatted(ctx, st.dn.GetClusterId(), st.dn.GetDnId(),
		st.dn.GetExtentSize(), refuse); err != nil {
		t.Fatalf("EnsureFormatted over the v0.1.0 disk: %v", err)
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("reading the v0.1.0 disk wrote %q", got)
	}
	if got, want := meta.Describe(), v010DiskDescribe; !strings.HasPrefix(got, want) {
		t.Errorf("Describe = %q, want it to start %q", got, want)
	}
	for _, req := range st.sides {
		ptr := req.GetSidePointer()
		rec, ok, err := meta.LookupSide(ctx, ptr.GetSpId(), ptr.GetSideId())
		if err != nil || !ok {
			t.Errorf("side %d: no record (%v)", ptr.GetSideId(), err)
			continue
		}
		var exts uint64
		for _, run := range rec.GetRunList() {
			exts += run.GetCount()
		}
		if exts != req.GetSideConf().GetExtCnt() {
			t.Errorf("side %d: its record holds %d extents, want %d",
				ptr.GetSideId(), exts, req.GetSideConf().GetExtCnt())
		}
		if rec.GetZeroBytes() != req.GetSideConf().GetZeroBytes() || !sideFullyZeroed(rec) {
			t.Errorf("side %d: its record zeroes %d of %d, want %d done",
				ptr.GetSideId(), rec.GetZeroedBytes(), rec.GetZeroBytes(),
				req.GetSideConf().GetZeroBytes())
		}
	}
	for _, bm := range st.bms {
		if _, ok, err := meta.LookupCloneMeta(ctx, bm.GetSidePointer().GetSpId(),
			bm.GetMigrId()); err != nil || !ok {
			t.Errorf("migration %d: no metadata record (%v)", bm.GetMigrId(), err)
		}
	}
}

// TestDnAgentStartsOverTheV010Store starts today's dn agent over the store
// and the disk v0.1.0's dn agent left (architecture.md, Upgrade in
// lockstep): its startup pass loads the dn file, every side file and every
// bitmap chunk, removes none of them, and takes the disk over as it is: it
// writes no block of the disk, and the volume table keeps its sequence and
// every side and clone-metadata record as v0.1.0 wrote them, so no record is
// freed and made again, which would zero a side or a migration's metadata;
// and neither conf gate refuses what it loaded.
func TestDnAgentStartsOverTheV010Store(t *testing.T) {
	st := readV010DnStore(t)
	node, size := v010DiskNode(t, st.dn.GetExtentSize())
	before := readV010Records(t, node, size)
	node.dirs[agent.NvmetRoot] = true
	for name, raw := range st.files {
		node.protos[common.DefaultLocalStorPrefix+"/"+name] = raw
	}
	srv := startTestServer(t, node)

	cid, dnId := st.dn.GetClusterId(), st.dn.GetDnId()
	if srv.getDn(dnKey(cid, dnId)) == nil {
		t.Fatalf("the dn file did not load")
	}
	if err := agent.ValidateExtentSize(st.dn.GetExtentSize()); err != nil {
		t.Errorf("the dn conf gate refuses the v0.1.0 dn file: %v", err)
	}
	for _, req := range st.sides {
		if err := validateZeroBytes(req.GetSideConf(), st.dn.GetExtentSize()); err != nil {
			t.Errorf("the side conf gate refuses side %d: %v",
				req.GetSidePointer().GetSideId(), err)
		}
	}
	for _, req := range st.sides {
		ptr := req.GetSidePointer()
		side := srv.getSide(sideKey(cid, dnId, ptr.GetSpId(), ptr.GetSideId()))
		if side == nil {
			t.Errorf("side %d did not load", ptr.GetSideId())
			continue
		}
		var want []uint32
		for _, bm := range st.bms {
			if bm.GetSidePointer().GetSideId() == ptr.GetSideId() {
				want = append(want, bm.GetBmIdx())
			}
		}
		slices.Sort(want)
		if got := side.chunks.Indexes(); !slices.Equal(got, want) {
			t.Errorf("side %d holds the chunks %v, want %v", ptr.GetSideId(), got, want)
		}
	}
	for name := range st.files {
		if _, ok := node.protos[common.DefaultLocalStorPrefix+"/"+name]; !ok {
			t.Errorf("the startup pass removed %s", name)
		}
	}
	if gotCid, gotDn, ok := srv.meta.Identity(); !ok || gotCid != cid || gotDn != dnId {
		t.Errorf("the disk's identity reads %x/%d/%v, want %x/%d", gotCid, gotDn, ok, cid, dnId)
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock ") {
			t.Errorf("the startup pass wrote the disk: %s", call)
		}
	}
	if got, want := srv.meta.Describe(), v010DiskDescribe; !strings.HasPrefix(got, want) {
		t.Errorf("after the startup pass Describe = %q, want it to start %q", got, want)
	}
	ctx := context.Background()
	sides, err := srv.meta.SideRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := srv.meta.CloneMetaRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !recordsEqual(sides, before.sides) || !recordsEqual(metas, before.metas) {
		t.Errorf("the startup pass changed the volume table:\nsides %v\nwas   %v\nclone metadata %v\nwas            %v",
			sides, before.sides, metas, before.metas)
	}
}

// v010DiskDescribe is how DiskMeta.Describe starts on the v0.1.0 disk: its
// table's sequence and record counts.
const v010DiskDescribe = "seq=14 sides=3 clone_metas=1 "

// v010Records are the side and clone-metadata records of a disk's volume
// table.
type v010Records struct {
	sides []*pb.DnDiskTable_SideRecord
	metas []*pb.DnDiskTable_CloneMetaRecord
}

// readV010Records reads the volume table of the v0.1.0 disk on node through
// a DiskMeta of its own, writing nothing.
func readV010Records(t *testing.T, node *fakeNode, size uint64) v010Records {
	t.Helper()
	ctx := context.Background()
	meta := NewDiskMeta(node.osClient(), testDisk)
	meta.SetDiskSize(size)
	sides, err := meta.SideRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := meta.CloneMetaRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sides) == 0 || len(metas) == 0 {
		t.Fatalf("the v0.1.0 disk holds %d side and %d clone-metadata records", len(sides), len(metas))
	}
	return v010Records{sides: sides, metas: metas}
}

// recordsEqual reports whether two record lists hold equal records in the
// same order.
func recordsEqual[M proto.Message](a, b []M) bool {
	return slices.EqualFunc(a, b, func(x, y M) bool { return proto.Equal(x, y) })
}
