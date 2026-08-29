package common

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A stub DiskNodeAgent wired through the real interceptor chain over bufconn.
// ---------------------------------------------------------------------------

type stubDnAgent struct {
	pb.UnimplementedDiskNodeAgentServer

	getDnInfoFn      func(ctx context.Context, req *pb.GetDnInfoRequest) (*pb.GetDnInfoReply, error)
	syncupDnFn       func(stream grpc.BidiStreamingServer[pb.SyncupDnRequest, pb.SyncupDnReply]) error
	pushMigrBitmapFn func(stream grpc.BidiStreamingServer[pb.PushMigrBitmapRequest, pb.PushMigrBitmapReply]) error
}

func (s *stubDnAgent) GetDnInfo(
	ctx context.Context, req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	if s.getDnInfoFn != nil {
		return s.getDnInfoFn(ctx, req)
	}
	return &pb.GetDnInfoReply{}, nil
}

func (s *stubDnAgent) SyncupDn(
	stream grpc.BidiStreamingServer[pb.SyncupDnRequest, pb.SyncupDnReply],
) error {
	if s.syncupDnFn != nil {
		return s.syncupDnFn(stream)
	}
	return nil
}

func (s *stubDnAgent) PushMigrBitmap(
	stream grpc.BidiStreamingServer[pb.PushMigrBitmapRequest, pb.PushMigrBitmapReply],
) error {
	if s.pushMigrBitmapFn != nil {
		return s.pushMigrBitmapFn(stream)
	}
	return nil
}

// startStub brings up the stub agent behind the §4 server interceptors and
// returns a client dialed with the §4 client interceptors.
func startStub(t *testing.T, stub *stubDnAgent) pb.DiskNodeAgentClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(GrpcStreamServerInterceptor()),
	)
	pb.RegisterDiskNodeAgentServer(server, stub)
	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("stub server stopped: %v", err)
		}
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(GrpcStreamClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		server.Stop()
		lis.Close()
	})
	return pb.NewDiskNodeAgentClient(conn)
}

// sideMsgs returns the captured msg strings of one side ("client"/"server"),
// in the order that side emitted them.
func (c *logCapture) sideMsgs(t *testing.T, side string) []string {
	t.Helper()
	prefix := "grpc " + side + " "
	var out []string
	for _, msg := range c.msgs(t) {
		if strings.HasPrefix(msg, prefix) {
			out = append(out, msg)
		}
	}
	return out
}

func wantMsgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("records = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Trace-id propagation (grpc.md §6.1, §6.2, §6.3)
// ---------------------------------------------------------------------------

func TestUnaryTracePropagation(t *testing.T) {
	type seen struct {
		md      []string
		traceId string
		ok      bool
	}
	got := make(chan seen, 1)
	client := startStub(t, &stubDnAgent{
		getDnInfoFn: func(ctx context.Context, _ *pb.GetDnInfoRequest) (*pb.GetDnInfoReply, error) {
			var s seen
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				s.md = md.Get(TraceIdMetadataKey)
			}
			s.traceId, s.ok = TraceIdFromCtx(ctx)
			got <- s
			return &pb.GetDnInfoReply{DnInfo: &pb.DnInfo{Revision: 9}}, nil
		},
	})

	ctx := WithTraceId(context.Background(), "t-123")
	reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{ClusterId: 1, DnId: 3})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if reply.GetDnInfo().GetRevision() != 9 {
		t.Errorf("reply = %v", reply)
	}

	s := <-got
	if len(s.md) != 1 || s.md[0] != "t-123" {
		t.Errorf("incoming metadata %s = %v, want [t-123]", TraceIdMetadataKey, s.md)
	}
	if !s.ok || s.traceId != "t-123" {
		t.Errorf("TraceIdFromCtx in handler = (%q, %v), want (t-123, true)", s.traceId, s.ok)
	}
}

// The interceptors never invent a trace id (T1).
func TestNoTraceIdIsMinted(t *testing.T) {
	got := make(chan []string, 1)
	client := startStub(t, &stubDnAgent{
		getDnInfoFn: func(ctx context.Context, _ *pb.GetDnInfoRequest) (*pb.GetDnInfoReply, error) {
			var vals []string
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				vals = md.Get(TraceIdMetadataKey)
			}
			if _, ok := TraceIdFromCtx(ctx); ok {
				t.Error("handler ctx carries a trace id that nobody minted")
			}
			got <- vals
			return &pb.GetDnInfoReply{}, nil
		},
	})

	if _, err := client.GetDnInfo(context.Background(), &pb.GetDnInfoRequest{}); err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if vals := <-got; len(vals) != 0 {
		t.Errorf("metadata %s = %v, want none", TraceIdMetadataKey, vals)
	}
}

// The wrapped ServerStream must hand the trace-enriched ctx to the handler
// (T2, the Context() override).
func TestStreamTracePropagation(t *testing.T) {
	got := make(chan string, 1)
	client := startStub(t, &stubDnAgent{
		syncupDnFn: func(stream grpc.BidiStreamingServer[pb.SyncupDnRequest, pb.SyncupDnReply]) error {
			traceId, ok := TraceIdFromCtx(stream.Context())
			if !ok {
				traceId = "<none>"
			}
			got <- traceId
			for {
				req, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := stream.Send(&pb.SyncupDnReply{
					DnInfo: &pb.DnInfo{Revision: req.GetRevision()},
				}); err != nil {
					return err
				}
			}
		},
	})

	ctx := WithTraceId(context.Background(), "stream-trace")
	stream, err := client.SyncupDn(ctx)
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if err := stream.Send(&pb.SyncupDnRequest{ClusterId: 1, DnId: 3, Revision: 7}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if reply.GetDnInfo().GetRevision() != 7 {
		t.Errorf("reply = %v", reply)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	select {
	case traceId := <-got:
		if traceId != "stream-trace" {
			t.Errorf("stream.Context() trace id = %q, want stream-trace", traceId)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}

// ---------------------------------------------------------------------------
// Message logging (grpc.md §6.4, §6.5, L2, L3, L4)
// ---------------------------------------------------------------------------

func TestUnaryLogRecords(t *testing.T) {
	capture := captureLogs(t)
	client := startStub(t, &stubDnAgent{
		getDnInfoFn: func(_ context.Context, _ *pb.GetDnInfoRequest) (*pb.GetDnInfoReply, error) {
			return &pb.GetDnInfoReply{DnInfo: &pb.DnInfo{Revision: 9}}, nil
		},
	})

	ctx := WithTraceId(context.Background(), "unary-trace")
	if _, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{ClusterId: 1, DnId: 3}); err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}

	wantMsgs(t, capture.sideMsgs(t, "client"),
		[]string{"grpc client request", "grpc client reply"})
	wantMsgs(t, capture.sideMsgs(t, "server"),
		[]string{"grpc server request", "grpc server reply"})

	for _, msg := range []string{
		"grpc client request", "grpc server request",
		"grpc server reply", "grpc client reply",
	} {
		rec := capture.onlyMsg(t, msg)
		if rec["method"] != "/DiskNodeAgent/GetDnInfo" {
			t.Errorf("%s: method = %v", msg, rec["method"])
		}
		if rec[TraceIdLogKey] != "unary-trace" {
			t.Errorf("%s: trace_id = %v", msg, rec[TraceIdLogKey])
		}
		if _, ok := rec["data"].(map[string]any); !ok {
			t.Errorf("%s: data = %v, want a rendered message", msg, rec["data"])
		}
	}

	req := capture.onlyMsg(t, "grpc client request")["data"].(map[string]any)
	if req["cluster_id"] != float64(1) || req["dn_id"] != float64(3) {
		t.Errorf("request data = %v", req)
	}
	reply := capture.onlyMsg(t, "grpc server reply")["data"].(map[string]any)
	dnInfo, ok := reply["dn_info"].(map[string]any)
	if !ok || dnInfo["revision"] != float64(9) {
		t.Errorf("reply data = %v", reply)
	}
}

func TestStreamLogRecordsAndBytesRedaction(t *testing.T) {
	capture := captureLogs(t)
	client := startStub(t, &stubDnAgent{
		pushMigrBitmapFn: func(stream grpc.BidiStreamingServer[pb.PushMigrBitmapRequest, pb.PushMigrBitmapReply]) error {
			for {
				req, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := stream.Send(&pb.PushMigrBitmapReply{
					AgentReply: &pb.AgentReply{Code: 0, Details: req.GetSidePointer().String()},
				}); err != nil {
					return err
				}
			}
		},
	})

	ctx := WithTraceId(context.Background(), "bm-trace")
	stream, err := client.PushMigrBitmap(ctx)
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	for bmIdx := uint32(1); bmIdx <= 2; bmIdx++ {
		if err := stream.Send(&pb.PushMigrBitmapRequest{
			ClusterId:   16981786240730056190,
			DnId:        3,
			SidePointer: &pb.SidePointer{SpId: 17, LegId: 21, SideId: 22},
			Revision:    9,
			MigrId:      30,
			BmIdx:       bmIdx,
			Bitmap:      []byte{1, 2, 3, 4},
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	// The terminating io.EOF must not be logged (L4).
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Recv = %v, want io.EOF", err)
	}

	wantMsgs(t, capture.sideMsgs(t, "client"), []string{
		"grpc client stream open",
		"grpc client send", "grpc client recv",
		"grpc client send", "grpc client recv",
	})
	wantMsgs(t, capture.sideMsgs(t, "server"), []string{
		"grpc server stream open",
		"grpc server recv", "grpc server send",
		"grpc server recv", "grpc server send",
		"grpc server stream close",
	})

	for _, msg := range []string{"grpc client stream open", "grpc server stream open",
		"grpc server stream close"} {
		rec := capture.onlyMsg(t, msg)
		if rec["method"] != "/DiskNodeAgent/PushMigrBitmap" {
			t.Errorf("%s: method = %v", msg, rec["method"])
		}
		if _, ok := rec["data"]; ok {
			t.Errorf("%s: carries a data attr: %v", msg, rec)
		}
	}

	// Bytes redaction on both sides (L1).
	for _, msg := range []string{"grpc client send", "grpc server recv"} {
		for i, rec := range capture.withMsg(t, msg) {
			if rec[TraceIdLogKey] != "bm-trace" {
				t.Errorf("%s[%d]: trace_id = %v", msg, i, rec[TraceIdLogKey])
			}
			data, ok := rec["data"].(map[string]any)
			if !ok {
				t.Fatalf("%s[%d]: data = %v", msg, i, rec["data"])
			}
			if data["bitmap"] != "<4 bytes>" {
				t.Errorf("%s[%d]: bitmap = %v, want <4 bytes>", msg, i, data["bitmap"])
			}
			if data["bm_idx"] != float64(i+1) {
				t.Errorf("%s[%d]: bm_idx = %v", msg, i, data["bm_idx"])
			}
		}
	}
	if strings.Contains(capture.buf.String(), "AQIDBA") { // base64 of the payload
		t.Error("raw bitmap payload leaked into the log")
	}
}

// ---------------------------------------------------------------------------
// Error path (grpc.md §6.6, L3)
// ---------------------------------------------------------------------------

func TestUnaryErrorRecords(t *testing.T) {
	capture := captureLogs(t)
	client := startStub(t, &stubDnAgent{
		getDnInfoFn: func(_ context.Context, _ *pb.GetDnInfoRequest) (*pb.GetDnInfoReply, error) {
			return nil, status.Error(codes.Aborted, "boom")
		},
	})

	_, err := client.GetDnInfo(
		WithTraceId(context.Background(), "err-trace"),
		&pb.GetDnInfoRequest{ClusterId: 1},
	)
	if status.Code(err) != codes.Aborted {
		t.Fatalf("GetDnInfo err = %v, want Aborted", err)
	}

	for _, msg := range []string{"grpc server reply", "grpc client reply"} {
		rec := capture.onlyMsg(t, msg)
		if rec["method"] != "/DiskNodeAgent/GetDnInfo" {
			t.Errorf("%s: method = %v", msg, rec["method"])
		}
		errAttr, ok := rec["error"].(string)
		if !ok || !strings.Contains(errAttr, "boom") {
			t.Errorf("%s: error = %v, want it to mention boom", msg, rec["error"])
		}
		if _, ok := rec["data"]; ok {
			t.Errorf("%s: failed call logged a data attr: %v", msg, rec)
		}
		if rec[TraceIdLogKey] != "err-trace" {
			t.Errorf("%s: trace_id = %v", msg, rec[TraceIdLogKey])
		}
	}
}

func TestStreamErrorRecords(t *testing.T) {
	capture := captureLogs(t)
	client := startStub(t, &stubDnAgent{
		syncupDnFn: func(stream grpc.BidiStreamingServer[pb.SyncupDnRequest, pb.SyncupDnReply]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return status.Error(codes.Aborted, "handler boom")
		},
	})

	stream, err := client.SyncupDn(WithTraceId(context.Background(), "stream-err"))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if err := stream.Send(&pb.SyncupDnRequest{ClusterId: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Aborted {
		t.Fatalf("Recv err = %v, want Aborted", err)
	}

	closeRec := capture.onlyMsg(t, "grpc server stream close")
	errAttr, ok := closeRec["error"].(string)
	if !ok || !strings.Contains(errAttr, "handler boom") {
		t.Errorf("stream close error = %v", closeRec["error"])
	}
	// The client's failing Recv is logged with the error, not as data.
	recvRec := capture.onlyMsg(t, "grpc client recv")
	if _, ok := recvRec["error"].(string); !ok {
		t.Errorf("client recv error attr missing: %v", recvRec)
	}
	if _, ok := recvRec["data"]; ok {
		t.Errorf("failed client recv logged data: %v", recvRec)
	}
}
