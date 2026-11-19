package common

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// levelsOf maps each record name to the levels its records carry, in order.
func levelsOf(t *testing.T, capture *logCapture) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, rec := range capture.records(t) {
		msg, _ := rec["msg"].(string)
		level, _ := rec["level"].(string)
		out[msg] = append(out[msg], level)
	}
	return out
}

// wantLevels fails unless every record of each named msg carries level.
func wantLevels(t *testing.T, got map[string][]string, level string,
	msgs ...string) {
	t.Helper()
	for _, msg := range msgs {
		if len(got[msg]) == 0 {
			t.Errorf("no %q record", msg)
		}
		for i, l := range got[msg] {
			if l != level {
				t.Errorf("%s[%d] at %s, want %s", msg, i, l, level)
			}
		}
	}
}

// grpc.md L1, L6: a check round's stream messages are reads, at DEBUG on
// both ends; the stream's open and close, and a unary call's request and
// reply, stay at INFO.
func TestInterceptorRecordLevels(t *testing.T) {
	capture := captureLogs(t)
	client := startStub(t, &stubDnAgent{
		checkDnFn: checkDnEcho,
		getDnInfoFn: func(context.Context, *pb.GetDnInfoRequest) (
			*pb.GetDnInfoReply, error) {
			return &pb.GetDnInfoReply{Revision: 9}, nil
		},
	})
	ctx := WithTraceId(context.Background(), "level-trace")
	stream, err := client.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	if err := stream.Send(&pb.CheckDnRequest{ClusterId: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	stream.CloseSend()
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Recv = %v, want io.EOF", err)
	}
	if _, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{}); err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	waitFor(t, func() bool {
		return len(capture.withMsg(t, "grpc server stream close")) == 1
	})
	got := levelsOf(t, capture)
	wantLevels(t, got, "DEBUG", "grpc client send", "grpc client recv",
		"grpc server recv", "grpc server send")
	wantLevels(t, got, "INFO", "grpc client stream open",
		"grpc server stream open", "grpc server stream close",
		"grpc client request", "grpc client reply",
		"grpc server request", "grpc server reply")
}

// grpc.md L1: a stream message that failed is at INFO with its error, and so
// is a failed unary reply; at the default level a round that succeeds logs
// no message at all.
func TestInterceptorFailedMessagesAreInfo(t *testing.T) {
	capture := captureLogs(t)
	client := startStub(t, &stubDnAgent{
		checkDnFn: func(stream grpc.BidiStreamingServer[pb.CheckDnRequest,
			pb.CheckDnReply]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return status.Error(codes.Aborted, "handler boom")
		},
		getDnInfoFn: func(context.Context, *pb.GetDnInfoRequest) (
			*pb.GetDnInfoReply, error) {
			return nil, status.Error(codes.Aborted, "boom")
		},
	})
	ctx := WithTraceId(context.Background(), "fail-trace")
	stream, err := client.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	if err := stream.Send(&pb.CheckDnRequest{ClusterId: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Aborted {
		t.Fatalf("Recv err = %v, want Aborted", err)
	}
	client.GetDnInfo(ctx, &pb.GetDnInfoRequest{})
	got := levelsOf(t, capture)
	wantLevels(t, got, "INFO", "grpc client recv", "grpc server stream close",
		"grpc server reply", "grpc client reply")
	if rec := capture.onlyMsg(t, "grpc client recv"); rec["error"] == nil {
		t.Errorf("the failed recv carries no error: %v", rec)
	}

	// At Info, a round that succeeds logs nothing but the stream's open.
	capture = captureLogs(t)
	SetLogLevel(slog.LevelInfo)
	echo := startStub(t, &stubDnAgent{checkDnFn: checkDnEcho})
	stream, err = echo.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	if err := stream.Send(&pb.CheckDnRequest{ClusterId: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	for _, msg := range []string{"grpc client send", "grpc client recv",
		"grpc server recv", "grpc server send"} {
		if recs := capture.withMsg(t, msg); len(recs) != 0 {
			t.Errorf("at Info a round logged %s: %v", msg, recs)
		}
	}
}
