package dnagent

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// dialAgent serves the DiskNodeAgent over bufconn with the grpc.md server
// interceptors, exactly as cmd/dnv-agent wires it.
func dialAgent(
	t *testing.T,
	srv *DnAgentServer,
) pb.DiskNodeAgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterDiskNodeAgentServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		grpcServer.Stop()
	})
	return pb.NewDiskNodeAgentClient(conn)
}

func TestGetDnSize(t *testing.T) {
	srv, node := newTestServer(t)
	client := dialAgent(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, err := client.GetDnSize(ctx, &pb.GetDnSizeRequest{
		ClusterId: testCluster, DnId: 0,
	})
	if err != nil {
		t.Fatalf("GetDnSize: %v", err)
	}
	// GetDnSize reports the *data area*, not the raw device: the fixed
	// [D13] prefix is already subtracted, so the CP does no further
	// subtraction (§6.1).
	want := node.devSize[testDisk] - common.DnDataOffset
	if reply.GetSize() != want {
		t.Errorf("size = %d, want %d", reply.GetSize(), want)
	}
	if want != 268435456 {
		t.Errorf("the 512 MiB test disk should leave 268435456 bytes, not %d",
			want)
	}
	// A device at or below DnDataOffset has no data area at all, and a bad
	// disk fails outright. Both report through the gRPC status: this RPC has
	// no AgentReply (DN3).
	node.devSize[testDisk] = common.DnDataOffset
	if _, err := client.GetDnSize(ctx, &pb.GetDnSizeRequest{
		ClusterId: testCluster,
	}); err == nil {
		t.Error("a disk with no data area did not produce a gRPC error")
	}
	delete(node.devSize, testDisk)
	if _, err := client.GetDnSize(ctx, &pb.GetDnSizeRequest{
		ClusterId: testCluster,
	}); err == nil {
		t.Error("a failing lsblk did not produce a gRPC error")
	}
}

// ---------------------------------------------------------------------------
// 8. CheckDn / CheckSide (SH24-SH26)
// ---------------------------------------------------------------------------

func TestCheckDnStream(t *testing.T) {
	srv, node := newTestServer(t)
	client := dialAgent(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	round := func(showInfo bool) *pb.CheckDnReply {
		t.Helper()
		if err := stream.Send(&pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn,
			Revision: 1, ShowInfo: showInfo,
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		return reply
	}

	// Unknown object: rejected, revision 0, no info — the stream stays open.
	reply := round(false)
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code = %d, want ReplyCodeUnknownObject",
			reply.GetAgentReply().GetCode())
	}
	if reply.GetRevision() != 0 || reply.GetDnInfo() != nil {
		t.Errorf("unknown-object reply carried revision %d / info %v",
			reply.GetRevision(), reply.GetDnInfo())
	}

	syncupBoth(t, srv, 1, testSide)

	// First reply on the stream always carries the full info.
	reply = round(false)
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	if reply.GetRevision() != 1 {
		t.Errorf("revision = %d, want the stored 1", reply.GetRevision())
	}
	if reply.GetDnInfo() == nil {
		t.Fatal("the first reply omitted the info")
	}

	// Unchanged round with show_info = false omits it.
	if got := round(false); got.GetDnInfo() != nil {
		t.Errorf("unchanged round carried the info: %v", got.GetDnInfo())
	}
	// show_info = true always fills it.
	if got := round(true); got.GetDnInfo() == nil {
		t.Error("show_info = true omitted the info")
	}
	// A probe flipped to error re-includes it: wipe the disk header behind
	// the agent's back and the meta probe must notice.
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))
	changed := round(false)
	if changed.GetDnInfo() == nil {
		t.Fatal("a changed probe did not re-include the info")
	}
	if changed.GetDnInfo().GetMetaInfo().GetStatus() ==
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("meta still reported OK: %v",
			changed.GetDnInfo().GetMetaInfo())
	}

	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Error("the handler did not end after CloseSend")
	}
}

func TestCheckSideStream(t *testing.T) {
	srv, node := newTestServer(t)
	client := dialAgent(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.CheckSide(ctx)
	if err != nil {
		t.Fatalf("CheckSide: %v", err)
	}
	round := func(showInfo bool) *pb.CheckSideReply {
		t.Helper()
		if err := stream.Send(&pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 1, ShowInfo: showInfo,
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		return reply
	}

	reply := round(false)
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code = %d, want ReplyCodeUnknownObject",
			reply.GetAgentReply().GetCode())
	}
	if reply.GetSideInfo() != nil {
		t.Error("unknown-object reply carried an info")
	}

	syncupBoth(t, srv, 1, testSide)

	reply = round(false)
	if reply.GetSideInfo() == nil {
		t.Fatal("the first reply omitted the info")
	}
	if reply.GetRevision() != 1 {
		t.Errorf("revision = %d, want 1", reply.GetRevision())
	}
	if got := round(false); got.GetSideInfo() != nil {
		t.Errorf("unchanged round carried the info: %v", got.GetSideInfo())
	}
	// Break the primary's dm-linear behind the agent's back.
	delete(node.dms, srv.nf.DnLinearName(
		testCluster, testDn, testSp, testSide, testCn0))
	changed := round(false)
	if changed.GetSideInfo() == nil {
		t.Fatal("a changed probe did not re-include the info")
	}
	if got := changed.GetSideInfo().GetCnIdToDmLinear()[testCn0].
		GetStatus(); got != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("dm-linear status = %v, want MISSING", got)
	}
}

// A Check round never mutates (DN16/SH25) — including the [D13] metadata:
// writeblock is not in readOnlyPrefixes, so Mutations() covers it.
func TestCheckRoundsNeverMutate(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	ctx := context.Background()

	node.Reset()
	srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, ShowInfo: true,
	}, nil)
	srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), ShowInfo: true,
	}, nil)
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a Check round mutated: %v", got)
	}
}
