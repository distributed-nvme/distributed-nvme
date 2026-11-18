package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// DN3's block size rule: a disk node takes a disk of 512-byte logical
// blocks alone (common.DnLogicalBlockSize). GetDnSize refuses any other,
// naming the size and the fix; the DN converge reports the same refusal as
// its disk row and formats no blank disk of it, while a disk formatted
// already converges as it is (DN5); and the DN's read reports the converge's
// disk row. A partition's block size is its disk's, which sysfs keeps in the
// disk's directory alone.

// testWholeDisk is the disk testDisk is a partition of, when shapeDisk makes
// it one.
const testWholeDisk = "/dev/fake-whole-disk"

// shapeDisk makes testDisk a whole disk of lbs-byte logical blocks or, with
// partition set, a partition of such a disk: then testDisk's own sysfs
// directory holds a "partition" file and no queue/, and only its disk's
// directory, reached through "..", holds the size (sysBlockAttr).
func shapeDisk(node *fakeNode, lbs uint64, partition bool) {
	node.mu.Lock()
	defer node.mu.Unlock()
	if !partition {
		delete(node.partOf, testDisk)
		node.devLbs[testDisk] = lbs
		return
	}
	delete(node.devLbs, testDisk)
	node.partOf[testDisk] = testWholeDisk
	node.devNo[testWholeDisk] = "7:0"
	node.devLbs[testWholeDisk] = lbs
}

// diskShapes are the four disks the tests run over.
var diskShapes = []struct {
	name      string
	lbs       uint64
	partition bool
}{
	{"a disk of 512-byte blocks", 512, false},
	{"a disk of 4096-byte blocks", 4096, false},
	{"a partition of a disk of 512-byte blocks", 512, true},
	{"a partition of a disk of 4096-byte blocks", 4096, true},
}

// wantBlockRefusal checks that text is DN3's refusal of a disk of 4096-byte
// blocks: the size, the one size taken, and the fixes: an nvme format, or a
// loop device with no -b and no direct IO, whose block size would then be
// the backing file's direct-IO alignment, or with -b 512.
func wantBlockRefusal(t *testing.T, what string, text string) {
	t.Helper()
	for _, part := range []string{
		"disk " + testDisk + " has a logical block size of 4096 bytes",
		"not 512", "nvme format",
		"losetup without -b and without --direct-io", "or with -b 512",
	} {
		if !strings.Contains(text, part) {
			t.Errorf("%s %q does not say %q", what, text, part)
		}
	}
}

func TestGetDnSizeTakesOnlyADiskOf512ByteBlocks(t *testing.T) {
	for _, tc := range diskShapes {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			shapeDisk(node, tc.lbs, tc.partition)
			reply, err := srv.GetDnSize(context.Background(),
				&pb.GetDnSizeRequest{ClusterId: testCluster})
			if tc.lbs == common.DnLogicalBlockSize {
				if err != nil {
					t.Fatalf("GetDnSize: %v", err)
				}
				want := node.devSize[testDisk] - common.DnDataOffset
				if reply.GetSize() != want {
					t.Errorf("size = %d, want %d", reply.GetSize(), want)
				}
				return
			}
			if status.Code(err) != codes.Internal {
				t.Fatalf("GetDnSize = (%v, %v), want an Internal refusal",
					reply, err)
			}
			wantBlockRefusal(t, "the refusal",
				status.Convert(err).Message())
			if got := node.Mutations(); len(got) != 0 {
				t.Errorf("GetDnSize changed the node: %q", got)
			}
		})
	}
}

func TestABlankDiskOfAnotherBlockSizeIsNotFormatted(t *testing.T) {
	ctx := context.Background()
	for _, tc := range diskShapes {
		if tc.lbs == common.DnLogicalBlockSize {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			shapeDisk(node, tc.lbs, tc.partition)
			reply, err := srv.SyncupDn(ctx, dnReq(1))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			info := reply.GetDnInfo()
			disk := info.GetDiskInfo()
			if disk.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("disk_info = %v, want DN3's refusal", disk)
			}
			wantBlockRefusal(t, "disk_info", disk.GetDetails())
			meta := info.GetMetaInfo()
			if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.HasPrefix(meta.GetDetails(), refusingFormat) {
				t.Fatalf("meta_info = %v, want the format refused", meta)
			}
			wantBlockRefusal(t, "meta_info", meta.GetDetails())
			if got := node.callsMatching("writeblock " + testDisk); len(
				got) != 0 {
				t.Fatalf("the converge wrote the disk: %q", got)
			}
			// The disk stays blank, so the record step names it and the
			// worker sends the syncup again, as under any refused format.
			if got := reply.GetAgentReply().GetCode(); got !=
				common.ReplyCodeLeftover {
				t.Errorf("code = %d, want %d", got, common.ReplyCodeLeftover)
			}
			read := dnInfoOf(t, srv).GetDiskInfo()
			if read.GetStatus() != disk.GetStatus() ||
				read.GetDetails() != disk.GetDetails() ||
				read.GetEpoch() != disk.GetEpoch() {
				t.Errorf("the read's disk row %v, the converge's %v", read,
					disk)
			}

			// Set up again at 512, the disk is formatted by the next
			// converge.
			shapeDisk(node, common.DnLogicalBlockSize, tc.partition)
			reply, err = srv.SyncupDn(ctx, dnReq(2))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			for what, row := range map[string]*pb.ResInfo{
				"disk_info": reply.GetDnInfo().GetDiskInfo(),
				"meta_info": reply.GetDnInfo().GetMetaInfo(),
			} {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("at 512, %s = %v, want OK", what, row)
				}
			}
			if !headerWritten(node) {
				t.Error("the disk at 512 was not formatted")
			}
		})
	}
}

// TestABlankDiskIsNotFormattedWhileItsBlockSizeIsNotRead: a read of the
// block size that did not answer proves no 512, so a blank disk stays blank,
// while a size read that did not answer alone formats as before (the counts
// test's "no size read has answered"); the next converge whose read answers
// 512 formats.
func TestABlankDiskIsNotFormattedWhileItsBlockSizeIsNotRead(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	lbsRead := "/sys/dev/block/" + node.devNo[testDisk] +
		"/queue/logical_block_size"
	node.mu.Lock()
	node.failReadAlways[lbsRead] = true
	node.mu.Unlock()
	reply, err := srv.SyncupDn(ctx, dnReq(1))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if disk := reply.GetDnInfo().GetDiskInfo(); disk.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(disk.GetDetails(), lbsRead) {
		t.Fatalf("disk_info = %v, want the failed read of %s", disk, lbsRead)
	}
	if meta := reply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.HasPrefix(meta.GetDetails(), refusingFormat) ||
		!strings.Contains(meta.GetDetails(), lbsRead) {
		t.Fatalf("meta_info = %v, want the format refused for the read",
			meta)
	}
	if headerWritten(node) {
		t.Fatal("a disk of a block size not read was formatted")
	}
	node.mu.Lock()
	delete(node.failReadAlways, lbsRead)
	node.mu.Unlock()
	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if !headerWritten(node) {
		t.Error("the disk was not formatted once its read answered 512")
	}
}

// TestAFormattedDiskThatReadsAnotherBlockSizeConvergesAsItIs: a disk
// formatted at 512 that reads 4096 later, as a loop device set up again with
// -b over the same file does, still holds its table and the data of its
// sides. Its converge removes nothing and writes nothing, so the table, the
// sides' devices and their data stay for the operator's fix and a by-hand
// recovery; its disk row reads DN3's refusal, in the converge and in the
// read alike. The controller node's md cannot use the disk's legs until the
// device reads 512 again (dnagent.md, Known limits), which no unit test
// here models.
func TestAFormattedDiskThatReadsAnotherBlockSizeConvergesAsItIs(
	t *testing.T,
) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)

	shapeDisk(node, 4096, false)
	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(2, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("code = %d (%s), want 0", got,
			reply.GetAgentReply().GetDetails())
	}
	disk := reply.GetDnInfo().GetDiskInfo()
	if disk.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("disk_info = %v, want DN3's refusal", disk)
	}
	wantBlockRefusal(t, "disk_info", disk.GetDetails())
	if meta := reply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("meta_info = %v, want the formatted disk OK", meta)
	}
	sideReply, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if row := sideReply.GetSideInfo().GetSideDevInfo(); row.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev_info = %v, want the side kept", row)
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock "+testDisk) ||
			strings.Contains(call, "dmsetup remove") {
			t.Errorf("the converge changed the disk or its devices: %q",
				call)
		}
	}
	node.mu.Lock()
	_, kept := node.dms[sideDev]
	node.mu.Unlock()
	if !kept {
		t.Errorf("the side device %s is gone", sideDev)
	}
	read := dnInfoOf(t, srv).GetDiskInfo()
	if read.GetStatus() != disk.GetStatus() ||
		read.GetDetails() != disk.GetDetails() ||
		read.GetEpoch() != disk.GetEpoch() {
		t.Errorf("the read's disk row %v, the converge's %v", read, disk)
	}
}

// TestAPartitionIsReadThroughItsDisk pins where the read of a partition's
// block size goes: its own sysfs directory, which the kernel's link from the
// device's number names, then "..", joined as given. A path cleaned as text
// reads /sys/dev/block/queue, which no kernel has.
func TestAPartitionIsReadThroughItsDisk(t *testing.T) {
	srv, node := newTestServer(t)
	shapeDisk(node, common.DnLogicalBlockSize, true)
	if _, err := srv.GetDnSize(context.Background(),
		&pb.GetDnSizeRequest{ClusterId: testCluster}); err != nil {
		t.Fatalf("GetDnSize: %v", err)
	}
	dir := "/sys/dev/block/" + node.devNo[testDisk]
	for _, want := range []string{
		fmt.Sprintf("read %s/partition", dir),
		fmt.Sprintf("read %s/../queue/logical_block_size", dir),
	} {
		if !node.hasCall(want) {
			t.Errorf("no %q among %q", want, node.Calls())
		}
	}
	if node.hasCall("read " + dir + "/queue/") {
		t.Errorf("the partition's own queue/ was read: %q", node.Calls())
	}
}
