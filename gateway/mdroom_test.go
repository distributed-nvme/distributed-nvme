package gateway

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file pins the md bitmap's room of architecture.md, Common
// validation: under md-raid1, CreateStoragePool refuses a bitmap chunk that
// mdadm would refuse for one of the groups the pool can ever make, the
// groups of its metadata's growth included, so that no grow of the pool fails
// at its md create.

const mrKib = uint64(1) << 10

// mrRaid1 is a request's md-raid1 bdev_conf of the given block and chunk.
func mrRaid1(block uint64, chunkBlocks uint64) *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{DataBlockSize: block},
		RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: chunkBlocks,
				},
			},
		},
	}
}

// mrEnv is the stripe fixture's cluster with the given extent size, and the
// cluster's stripe set to 64 KiB so that a 64 KiB block takes it.
func mrEnv(t *testing.T, extentSize uint64) *sptEnv {
	t.Helper()
	env := stbEnv(t, 64*mrKib)
	env.cc.DnBinConf.ExtentSize = extentSize
	mustPut(t, env.cli, model.ClusterConfKey(env.name), env.cc)
	return env
}

// TestCreateStoragePoolRefusesAnMdChunkTooSmallForAGroup is the example of
// the plan's item: a 64 KiB block with one block for each chunk, on 1 GiB
// extents. The first metadata group, of one extent, fits, but the metadata
// grows by doubling to groups of 2, 4 and 8 extents, and mdadm gives a group
// of up to 8 GiB only 4 KiB for its record, so those groups need a larger
// chunk: the pool is refused INVALID_ARGUMENT, nothing written, the message
// naming the group and the smallest chunk count that fits every group, 8
// (512 KiB). A chunk of 4 blocks is still refused; 8 blocks are stored.
func TestCreateStoragePoolRefusesAnMdChunkTooSmallForAGroup(t *testing.T) {
	env := mrEnv(t, 1<<30)
	for _, cnt := range []uint64{1, 4} {
		req := sptSpec{
			name: fmt.Sprintf("small%d", cnt), cntlrCnt: 1, sliceCnt: 1,
			initExt: 1,
		}.req(env.name)
		req.BdevConf = mrRaid1(64*mrKib, cnt)
		before := env.dumpAll()
		_, err := env.srv.CreateStoragePool(env.ctx, req)
		sptWantCode(t, err, codes.InvalidArgument)
		msg := status.Convert(err).Message()
		for _, want := range []string{
			fmt.Sprintf("bitmap_chunk_block_cnt %d", cnt),
			"metadata group of 8 extents",
			"mdadm",
			"name a bitmap_chunk_block_cnt of at least 8",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("chunk of %d blocks: message %q does not say %q",
					cnt, msg, want)
			}
		}
		if changed := sptChangedKeys(before, env.dumpAll()); len(
			changed) != 0 {
			t.Errorf("chunk of %d blocks: a refusal wrote %v", cnt, changed)
		}
	}
	spec := sptSpec{name: "fits", cntlrCnt: 1, sliceCnt: 1, initExt: 1}
	req := spec.req(env.name)
	req.BdevConf = mrRaid1(64*mrKib, 8)
	if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
		t.Fatalf("CreateStoragePool with a chunk of 8 blocks: %v", err)
	}
	if got := env.spConf("fits").GetBdevConf().GetRedundConf().
		GetRedundMdRaid1().GetBitmapChunkBlockCnt(); got != 8 {
		t.Errorf("stored chunk count %d, want 8", got)
	}
}

// TestCreateStoragePoolJudgesEveryGroupNotTheLargest pins that every group
// is judged: on 1 GiB extents a data group of 9 extents, the largest group,
// has the 64 KiB room and fits a 64 KiB chunk, but the metadata group of 8
// extents that the metadata grows to has the 4 KiB room and needs 512 KiB,
// so a 64 KiB chunk is refused, the message saying the chunk count is the
// cluster's when the request names none. The refusal comes before the node
// scans: a cluster of one disk node, too few for two legs, hears it too.
func TestCreateStoragePoolJudgesEveryGroupNotTheLargest(t *testing.T) {
	env := mrEnv(t, 1<<30)
	few := sptNewEnv(t, 1, 1, 1024)
	few.cc.DnBinConf.ExtentSize = 1 << 30
	few.cc.BdevConf.DmRaid0Conf.StripeSize = 64 * mrKib
	few.cc.BdevConf.DmPoolConf.DataBlockSize = 64 * mrKib
	few.cc.BdevConf.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{BitmapChunkBlockCnt: 1},
		},
	}
	mustPut(t, few.cli, model.ClusterConfKey(few.name), few.cc)
	for _, tc := range []struct {
		name  string
		env   *sptEnv
		bdev  *pb.BdevConf
		frags []string
	}{
		{"the largest group fits, a smaller one does not", env,
			mrRaid1(64*mrKib, 1), []string{
				"bdev_conf merged over the cluster's: ",
				"metadata group of 8 extents",
				"name a bitmap_chunk_block_cnt of at least 8",
			}},
		{"a cluster too short of nodes, the count the cluster's", few,
			nil, []string{
				"metadata group of 8 extents",
				"the bitmap_chunk_block_cnt is the cluster's; name a " +
					"bitmap_chunk_block_cnt of at least 8",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sptSpec{
				name: "every", cntlrCnt: 1, sliceCnt: 1, initExt: 9,
			}.req(tc.env.name)
			req.BdevConf = tc.bdev
			before := tc.env.dumpAll()
			_, err := tc.env.srv.CreateStoragePool(tc.env.ctx, req)
			sptWantCode(t, err, codes.InvalidArgument)
			msg := status.Convert(err).Message()
			for _, want := range tc.frags {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not say %q", msg, want)
				}
			}
			if changed := sptChangedKeys(before, tc.env.dumpAll()); len(
				changed) != 0 {
				t.Errorf("a refusal wrote %v", changed)
			}
		})
	}
}

// TestCreateStoragePoolJudgesTheDataGroupsMdRoom pins the data group's
// half. On 16 GiB extents the first metadata group already reaches dm-thin's
// metadata ceiling, so the metadata never grows, and a group of 16 GiB has
// 64 KiB of room: a 64 KiB chunk fits it. A data group of 13 extents, 208
// GiB, has 128 KiB of room and needs a chunk of 256 KiB: refused, the message
// naming the data group; a data group of one extent is created.
func TestCreateStoragePoolJudgesTheDataGroupsMdRoom(t *testing.T) {
	env := mrEnv(t, 16<<30)
	req := sptSpec{
		name: "wide", cntlrCnt: 1, sliceCnt: 1, initExt: 13,
	}.req(env.name)
	req.BdevConf = mrRaid1(64*mrKib, 1)
	before := env.dumpAll()
	_, err := env.srv.CreateStoragePool(env.ctx, req)
	sptWantCode(t, err, codes.InvalidArgument)
	msg := status.Convert(err).Message()
	for _, want := range []string{
		"data group of 13 extents",
		"name a bitmap_chunk_block_cnt of at least 4",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	if changed := sptChangedKeys(before, env.dumpAll()); len(changed) != 0 {
		t.Errorf("a refusal wrote %v", changed)
	}
	req = sptSpec{
		name: "narrow", cntlrCnt: 1, sliceCnt: 1, initExt: 1,
	}.req(env.name)
	req.BdevConf = mrRaid1(64*mrKib, 1)
	if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
		t.Fatalf("CreateStoragePool with a one-extent data group: %v", err)
	}
}

// TestMdBitmapMinChunkFollowsMdadm pins mdBitmapMinChunk, mdadm 4.5's
// arithmetic, at the edges of its room. The cases of 30720 x 64 KiB, of
// 8 GiB, of 8 GiB + 64 KiB, of 200 GiB and of 200 GiB + 64 KiB, and a
// 64 KiB chunk at 30719 x 64 KiB, were run with mdadm --create on a lab guest
// with dnv's options: the chunk given here was taken and the next smaller one
// refused with "Given bitmap chunk size not supported" (at 30719 x 64 KiB,
// the chunk alone was run). The other cases are computed. The room grows in
// steps, past 8 GiB and from 200 GiB on, so the chunk a group needs falls at
// each step.
func TestMdBitmapMinChunkFollowsMdadm(t *testing.T) {
	const gib = uint64(1) << 30
	for _, tc := range []struct {
		name string
		data uint64
		want uint64
	}{
		{"under 64 KiB there is no room", 32 * mrKib, 0},
		{"64 KiB", 64 * mrKib, 4 * mrKib},
		{"the item's 1 GiB group", gib, 64 * mrKib},
		{"30719 x 64 KiB, the last size a 64 KiB chunk fits",
			30719 * 64 * mrKib, 64 * mrKib},
		{"30720 x 64 KiB", 30720 * 64 * mrKib, 128 * mrKib},
		{"the item's 2 GiB group", 2 * gib, 128 * mrKib},
		{"8 GiB, the last size of the 4 KiB room", 8 * gib, 512 * mrKib},
		{"8 GiB + 64 KiB, the 64 KiB room", 8*gib + 64*mrKib, 32 * mrKib},
		{"200 GiB, the last size of the 64 KiB room", 200 * gib,
			512 * mrKib},
		{"200 GiB + 64 KiB, the 128 KiB room", 200*gib + 64*mrKib,
			256 * mrKib},
	} {
		if got := mdBitmapMinChunk(tc.data); got != tc.want {
			t.Errorf("%s: mdBitmapMinChunk(%d) = %d, want %d", tc.name,
				tc.data, got, tc.want)
		}
	}
}
