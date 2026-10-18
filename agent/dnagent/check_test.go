package dnagent

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

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
	// subtraction (architecture.md, Size → extents).
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
		t.Error("a disk whose size cannot be read did not produce a " +
			"gRPC error")
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

	// Unchanged round with show_info = false omits it. Two reads of the
	// unchanged DN come first: a round answers from the observation (SH28),
	// so only an observation from a read of its own, compared with one from
	// the read before, proves that two reads of an unchanged DN give equal
	// infos (SH26). The round after the first read may carry the info, since
	// the read's info may differ from the converge's.
	readDnNow(ctx, srv)
	round(false)
	readDnNow(ctx, srv)
	if got := round(false); got.GetDnInfo() != nil {
		t.Errorf("unchanged round carried the info: %v", got.GetDnInfo())
	}
	// show_info = true always fills it.
	if got := round(true); got.GetDnInfo() == nil {
		t.Error("show_info = true omitted the info")
	}
	// A probe flipped to error re-includes it: wipe the disk header behind
	// the agent's back and the meta probe of the DN's next read must notice
	// (SH29). Until that read has run, a round answers the observation it
	// had, which it sent already.
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))
	if got := round(false); got.GetDnInfo() != nil {
		t.Errorf("a round before the read carried the info: %v",
			got.GetDnInfo())
	}
	readDnNow(ctx, srv)
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
	// Unchanged round omits it. Two reads of the unchanged side come first,
	// as in TestCheckDnStream: only an observation from a read of its own,
	// compared with one from the read before, proves that two reads of an
	// unchanged side give equal infos (SH26).
	readSideNow(ctx, srv, sidePtr(testSide))
	round(false)
	readSideNow(ctx, srv, sidePtr(testSide))
	if got := round(false); got.GetSideInfo() != nil {
		t.Errorf("unchanged round carried the info: %v", got.GetSideInfo())
	}
	// Break the primary's dm-linear behind the agent's back: the side's next
	// read notices (SH29), and until it has run a round answers the
	// observation it had, which it sent already.
	delete(node.dms, srv.nf.DnLinearName(
		testCluster, testDn, testSp, testSide, testCn0))
	if got := round(false); got.GetSideInfo() != nil {
		t.Errorf("a round before the read carried the info: %v",
			got.GetSideInfo())
	}
	readSideNow(ctx, srv, sidePtr(testSide))
	changed := round(false)
	if changed.GetSideInfo() == nil {
		t.Fatal("a changed probe did not re-include the info")
	}
	if got := changed.GetSideInfo().GetCnIdToDmLinear()[testCn0].
		GetStatus(); got != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("dm-linear status = %v, want MISSING", got)
	}
}

// TestCheckRoundsCarryTheRequestTraceId pins SH24's per-round trace id: the
// stream's metadata names only the worker round that opened it, so the read
// each round starts (SH29) runs every command and read under the request's
// trace_id, and the read of a round that names none keeps the stream's.
func TestCheckRoundsCarryTheRequestTraceId(t *testing.T) {
	srv, node := newTestServer(t)
	client := dialAgent(t, srv)
	syncupBoth(t, srv, 1, testSide)
	srv.roundReads = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dnStream, err := client.CheckDn(metadata.AppendToOutgoingContext(
		ctx, common.TraceIdMetadataKey, "dn-stream"))
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	sideStream, err := client.CheckSide(metadata.AppendToOutgoingContext(
		ctx, common.TraceIdMetadataKey, "side-stream"))
	if err != nil {
		t.Fatalf("CheckSide: %v", err)
	}
	// Each round's read is joined before the next round, so every round
	// starts one: a round starts none while a read of its object runs.
	dnRound := func(traceId string) {
		t.Helper()
		if err := dnStream.Send(&pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: 1,
			TraceId: traceId,
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		if _, err := dnStream.Recv(); err != nil {
			t.Fatalf("recv: %v", err)
		}
		joinReads(t, srv)
	}
	sideRound := func(traceId string) {
		t.Helper()
		if err := sideStream.Send(&pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 1, TraceId: traceId,
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		if _, err := sideStream.Recv(); err != nil {
			t.Fatalf("recv: %v", err)
		}
		joinReads(t, srv)
	}

	dnRound("dn-round-1")
	dnRound("dn-round-2")
	sideRound("side-round-1")
	sideRound("side-round-2")
	for _, id := range []string{
		"dn-round-1", "dn-round-2", "side-round-1", "side-round-2",
	} {
		if len(node.Traced(id)) == 0 {
			t.Errorf("no command or read ran under %s", id)
		}
	}
	for _, id := range []string{"dn-stream", "side-stream"} {
		if got := node.Traced(id); len(got) != 0 {
			t.Errorf("a round naming its own id ran %d calls under the "+
				"stream's %s, the first %q", len(got), id, got[0])
		}
	}
	// An empty trace_id keeps the stream's id.
	dnRound("")
	sideRound("")
	for _, id := range []string{"dn-stream", "side-stream"} {
		if len(node.Traced(id)) == 0 {
			t.Errorf("a round naming no id ran nothing under the stream's %s",
				id)
		}
	}
}

// A check round and an info read run no command and read nothing of the
// node: they answer from the observation (SH28). The read a round starts
// issues no mutating command (DN16, SH29) — including the [D13] metadata:
// writeblock is not in readOnlyPrefixes, so Mutations() covers it.
func TestCheckRoundsNeverMutate(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	ctx := context.Background()

	node.Reset()
	dnRound(srv, "dn-round")
	sideRound(srv, "side-round")
	if _, err := srv.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn,
	}); err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if _, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	}); err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := node.Calls(); len(got) != 0 {
		t.Errorf("a round or an info read ran calls of its own: %q", got)
	}

	srv.roundReads = true
	node.Reset()
	dnRound(srv, "dn-round")
	sideRound(srv, "side-round")
	joinReads(t, srv)
	for _, id := range []string{"dn-round", "side-round"} {
		if len(node.Traced(id)) == 0 {
			t.Fatalf("fixture is wrong: %s started no read", id)
		}
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a check round's read mutated: %v", got)
	}
}

// GetSideInfo answers as a check round does, from the side's observation
// (SH28): the same code, revision, info and observation, and neither runs a
// call. The side's read that made the observation probes first and takes the
// verdict second (DN16, SH29), so it runs the probe's calls and then the
// verdict's: what the probe reads can move the agent's own view the verdict
// reads.
func TestGetSideInfoAnswersAsACheckRound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, srv *DnAgentServer)
	}{
		{"a side", func(t *testing.T, srv *DnAgentServer) {
			syncupBoth(t, srv, 1, testSide)
		}},
		{"a migration destination", func(t *testing.T, srv *DnAgentServer) {
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(context.Background(), migrDstReq(2,
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			tc.setup(t, srv)
			key := sideKey(testCluster, testDn, testSp, testSide)
			st := srv.getSide(key)

			node.Reset()
			srv.probeSide(ctx, st)
			want := node.Calls()
			node.Reset()
			srv.sideVerdict(ctx, st)
			want = append(want, node.Calls()...)
			node.Reset()
			srv.refreshSide(ctx, key, st)
			read := node.Calls()
			if len(read) == 0 {
				t.Fatal("fixture is wrong: the read ran nothing")
			}
			for i := range max(len(read), len(want)) {
				if i >= len(read) || i >= len(want) || read[i] != want[i] {
					t.Fatalf("the read ran %d calls, the probe and then the "+
						"verdict %d; the first difference at %d:\n%q\n%q",
						len(read), len(want), i, read[min(i, len(read)-1)],
						want[min(i, len(want)-1)])
				}
			}

			node.Reset()
			round, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: sidePtr(testSide), ShowInfo: true,
			}, nil)
			get, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: sidePtr(testSide),
			})
			if err != nil {
				t.Fatalf("GetSideInfo: %v", err)
			}
			if calls := node.Calls(); len(calls) != 0 {
				t.Errorf("the round and GetSideInfo ran %q", calls)
			}
			obs := sideObs(t, srv)
			if !proto.Equal(get.GetAgentReply(), round.GetAgentReply()) ||
				get.GetRevision() != round.GetRevision() ||
				!proto.Equal(get.GetSideInfo(), round.GetSideInfo()) ||
				!proto.Equal(get.GetSideInfo(), obs.Info) ||
				!get.GetObsStamp().GetObsTime().AsTime().Equal(obs.Time) ||
				!round.GetObsStamp().GetObsTime().AsTime().Equal(obs.Time) {
				t.Errorf("GetSideInfo = %v\nround = %v\nwant both the "+
					"read's observation", get, round)
			}
		})
	}
}
