package gateway

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestGetClusterCapacityCopiesEveryRecord pins the read of architecture.md,
// Clusters, GetClusterCapacity: one entry per node of the cluster, every
// field the record's own, the pointer lists reduced to their counts, and
// nothing computed — a disabled or unhealthy node is reported as its record
// says, not left out.
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
