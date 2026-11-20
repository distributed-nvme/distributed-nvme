package gateway

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestGetClusterCapacityCopiesEveryRecord pins the read of architecture.md,
// Clusters, GetClusterCapacity: one entry per node of the cluster, every
// field the record's own, the pointer lists reduced to their counts, and no
// entry field computed — a disabled or unhealthy node is reported as its
// record says, not left out.
func TestGetClusterCapacityCopiesEveryRecord(t *testing.T) {
	env := sptNewEnv(t, 3, 2, 500)

	// dn-01 is disabled and unhealthy, sits in another location and holds
	// two sides; the others are the planted defaults.
	dnKey := model.DnConfKey(env.cid, env.dnAddrs[1])
	dn := &pb.DnConf{}
	if found, err := env.cli.Get(env.ctx, dnKey, dn); err != nil || !found {
		t.Fatalf("reading %s: found %v, err %v", dnKey, found, err)
	}
	dn.Disabled = true
	dn.ErrEpoch = 7
	dn.Location = "rack-b"
	dn.FreeExtCnt = 400
	dn.SidePtrList = []*pb.SidePointer{
		{SpId: 1, LegId: 2, SideId: 3},
		{SpId: 1, LegId: 4, SideId: 5},
	}
	mustPut(t, env.cli, dnKey, dn)

	// cn-01 is unhealthy and hosts one cntlr.
	cnKey := model.CnConfKey(env.cid, env.cnAddrs[1])
	cn := &pb.CnConf{}
	if found, err := env.cli.Get(env.ctx, cnKey, cn); err != nil || !found {
		t.Fatalf("reading %s: found %v, err %v", cnKey, found, err)
	}
	cn.ErrEpoch = 9
	cn.FreeExtCnt = 123
	cn.CntlrPtrList = []*pb.CntlrPointer{{SpId: 1, CntlrId: 6}}
	mustPut(t, env.cli, cnKey, cn)

	reply, err := env.srv.GetClusterCapacity(
		env.ctx, &pb.GetClusterCapacityRequest{ClusterName: env.name})
	wantCode(t, err, codes.OK, "GetClusterCapacity")
	if reply.GetClusterName() != env.name {
		t.Errorf("cluster_name: got %q, want %q",
			reply.GetClusterName(), env.name)
	}
	if reply.GetExtentSize() != env.cc.GetDnBinConf().GetExtentSize() {
		t.Errorf("extent_size: got %d, want %d",
			reply.GetExtentSize(), env.cc.GetDnBinConf().GetExtentSize())
	}
	if len(reply.GetAddrPortToDn()) != 3 {
		t.Errorf("%d disk node entries, want 3", len(reply.GetAddrPortToDn()))
	}
	if len(reply.GetAddrPortToCn()) != 2 {
		t.Errorf("%d controller node entries, want 2",
			len(reply.GetAddrPortToCn()))
	}
	hnodeWantProto(t, reply.GetAddrPortToDn()[env.dnAddrs[0]],
		&pb.DnCapacityEntry{
			Location:    env.dnAddrs[0],
			TotalExtCnt: 1024,
			FreeExtCnt:  sptDnFree,
		}, "the untouched disk node's entry")
	hnodeWantProto(t, reply.GetAddrPortToDn()[env.dnAddrs[1]],
		&pb.DnCapacityEntry{
			Location:    "rack-b",
			Disabled:    true,
			ErrEpoch:    7,
			TotalExtCnt: 1024,
			FreeExtCnt:  400,
			SideCnt:     2,
		}, "the disabled, unhealthy disk node's entry")
	hnodeWantProto(t, reply.GetAddrPortToCn()[env.cnAddrs[0]],
		&pb.CnCapacityEntry{
			Location:    env.cnAddrs[0],
			TotalExtCnt: 1024,
			FreeExtCnt:  500,
		}, "the untouched controller node's entry")
	hnodeWantProto(t, reply.GetAddrPortToCn()[env.cnAddrs[1]],
		&pb.CnCapacityEntry{
			Location:    env.cnAddrs[1],
			ErrEpoch:    9,
			TotalExtCnt: 1024,
			FreeExtCnt:  123,
			CntlrCnt:    1,
		}, "the unhealthy controller node's entry")
}

// TestGetClusterCapacityResolvesTheCluster pins the two refusals shared with
// every cluster-scoped read: a name that fails validation is
// INVALID_ARGUMENT before any read (GW4), and a cluster that does not exist
// is NOT_FOUND. A fresh cluster with no node answers two empty maps.
func TestGetClusterCapacityResolvesTheCluster(t *testing.T) {
	s := newTestServer(t)
	ctx := t.Context()
	_, err := s.GetClusterCapacity(ctx,
		&pb.GetClusterCapacityRequest{ClusterName: "bad name"})
	wantCode(t, err, codes.InvalidArgument, "a name with a blank")
	_, err = s.GetClusterCapacity(ctx,
		&pb.GetClusterCapacityRequest{ClusterName: hnodeName("cap-none")})
	wantCode(t, err, codes.NotFound, "a cluster that does not exist")

	name := hnodeName("cap-empty")
	mustCluster(t, s, name)
	reply, err := s.GetClusterCapacity(ctx,
		&pb.GetClusterCapacityRequest{ClusterName: name})
	wantCode(t, err, codes.OK, "a cluster with no node")
	if reply.GetClusterName() != name {
		t.Errorf("cluster_name: got %q, want %q", reply.GetClusterName(), name)
	}
	if reply.GetExtentSize() == 0 {
		t.Errorf("extent_size is zero; the stored conf resolves a default")
	}
	if len(reply.GetAddrPortToDn()) != 0 || len(reply.GetAddrPortToCn()) != 0 {
		t.Errorf("a cluster with no node answered %d disk and %d controller "+
			"node entries, want none",
			len(reply.GetAddrPortToDn()), len(reply.GetAddrPortToCn()))
	}
}

// TestGetClusterCapacitySpace pins the space answer of architecture.md,
// Clusters, GetClusterCapacity, on planted records: the pool shape the
// request names, its zeros resolved as CreateStoragePool resolves them, the
// shape's bounds judged before any read, the pool count read off SpGlobal,
// and the arithmetic, which is model.ClusterSpaceOf's and pinned there.
func TestGetClusterCapacitySpace(t *testing.T) {
	env := sptNewEnv(t, 4, 2, 500)
	ext := env.cc.GetDnBinConf().GetExtentSize()
	capacity := func(legs, cntlrs, slices uint32) *pb.ClusterSpace {
		t.Helper()
		reply, err := env.srv.GetClusterCapacity(env.ctx,
			&pb.GetClusterCapacityRequest{
				ClusterName: env.name,
				LegCnt:      legs,
				CntlrCnt:    cntlrs,
				SliceCnt:    slices,
			})
		wantCode(t, err, codes.OK, "GetClusterCapacity")
		if reply.GetSpace() == nil {
			t.Fatalf("legs %d, cntlrs %d, slices %d: no space answer",
				legs, cntlrs, slices)
		}
		return reply.GetSpace()
	}

	// Four disk nodes of 64 free extents, each its own location, and two
	// controller nodes of 500: a RAID1 pool of one slice can place 128
	// extents of groups, and its first data group is sure of 64.
	hnodeWantProto(t, capacity(2, 2, 1), &pb.ClusterSpace{
		LegCnt:            2,
		CntlrCnt:          2,
		SliceCnt:          1,
		DnRawBytes:        4 * 1024 * ext,
		DnChargedBytes:    4 * (1024 - sptDnFree) * ext,
		DnFreeBytes:       4 * sptDnFree * ext,
		CnRawBytes:        2 * 1024 * ext,
		CnChargedBytes:    2 * (1024 - 500) * ext,
		CnFreeBytes:       2 * 500 * ext,
		PlaceableBytes:    2 * sptDnFree * ext,
		LargestInitExtCnt: sptDnFree,
		Limit:             pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE,
	}, "a RAID1 pool of two cntlrs and one slice")

	// Zero counts take the legs of the cluster's stored bdev_conf (none
	// here: one leg) and the default cntlr and slice counts.
	zero := capacity(0, 0, 0)
	if zero.GetLegCnt() != 1 ||
		zero.GetCntlrCnt() != common.DefaultCntlrCntPerSp ||
		zero.GetSliceCnt() != common.DefaultSliceCntPerSp {
		t.Errorf("zero counts resolve to %d/%d/%d, want 1/%d/%d",
			zero.GetLegCnt(), zero.GetCntlrCnt(), zero.GetSliceCnt(),
			common.DefaultCntlrCntPerSp, common.DefaultSliceCntPerSp)
	}
	raid1 := proto.Clone(env.cc).(*pb.ClusterConf)
	raid1.BdevConf.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{
				BitmapChunkBlockCnt: common.DefaultChunkBlockCnt,
			},
		},
	}
	mustPut(t, env.cli, model.ClusterConfKey(env.name), raid1)
	if legs := capacity(0, 1, 1).GetLegCnt(); legs != common.MaxAllocLegPerGrp {
		t.Errorf("a RAID1 cluster's zero leg count resolves to %d, want %d",
			legs, common.MaxAllocLegPerGrp)
	}

	// The bounds themselves are shapes a pool create takes: answered, each
	// with the limit this small cluster reaches for it.
	for _, edge := range []struct {
		legs, cntlrs, slices uint32
		limit                pb.CapacityLimit
	}{
		{common.MaxAllocLegPerGrp, 1, 1, pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE},
		{1, common.MaxCntlrCntPerSp, 1, pb.CapacityLimit_CAPACITY_LIMIT_CN_COUNT},
		{1, 1, common.MaxSliceCntPerSp, pb.CapacityLimit_CAPACITY_LIMIT_DN_COUNT},
	} {
		got := capacity(edge.legs, edge.cntlrs, edge.slices)
		if got.GetLimit() != edge.limit {
			t.Errorf("shape %d/%d/%d: limit %v, want %v", edge.legs,
				edge.cntlrs, edge.slices, got.GetLimit(), edge.limit)
		}
	}

	// The pool count is SpGlobal's bucket sum; at the ceiling nothing is
	// placeable.
	mustPut(t, env.cli, model.SpGlobalKey(env.cid), &pb.SpGlobal{
		NextId:      common.MaxSpCntPerCluster + 1,
		ShardBucket: []uint32{common.MaxSpCntPerCluster - 1, 1},
	})
	full := capacity(2, 2, 1)
	if full.GetPoolCnt() != common.MaxSpCntPerCluster ||
		full.GetLimit() != pb.CapacityLimit_CAPACITY_LIMIT_POOL_CEILING ||
		full.GetPlaceableBytes() != 0 || full.GetLargestInitExtCnt() != 0 {
		t.Errorf("a cluster at its pool ceiling answers %v", full)
	}
	if err := env.cli.Delete(env.ctx, model.SpGlobalKey(env.cid)); err != nil {
		t.Fatalf("deleting the sp_global key: %v", err)
	}
	_, err := env.srv.GetClusterCapacity(env.ctx,
		&pb.GetClusterCapacityRequest{ClusterName: env.name})
	wantCode(t, err, codes.Aborted, "a cluster with no sp_global key")

	// Shapes no pool create takes are refused before any read: a cluster
	// that does not exist answers INVALID_ARGUMENT, not NOT_FOUND.
	for _, bad := range []*pb.GetClusterCapacityRequest{
		{LegCnt: common.MaxAllocLegPerGrp + 1},
		{CntlrCnt: common.MaxCntlrCntPerSp + 1},
		{SliceCnt: common.MaxSliceCntPerSp + 1},
	} {
		bad.ClusterName = hnodeName("cap-none")
		_, err := env.srv.GetClusterCapacity(env.ctx, bad)
		wantCode(t, err, codes.InvalidArgument, bad.String())
	}
}
