package common

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The message limit (grpc.md, Wiring)
// ---------------------------------------------------------------------------

// overGrpcDefault is above gRPC's own receive default of 4 MiB and far under
// GrpcMaxMsgSize: a message of this size crosses a connection only where the
// receiving end carries the limit.
const overGrpcDefault = 5 << 20

// startStubWithMsgLimit is startStub with the message limit on both ends:
// GrpcServerMsgOptions on the server, GrpcClientMsgOption on the client
// connection.
func startStubWithMsgLimit(t *testing.T, stub *stubDnAgent) pb.DiskNodeAgentClient {
	t.Helper()
	return startStubOpts(t, stub, GrpcServerMsgOptions(),
		[]grpc.DialOption{GrpcClientMsgOption()})
}

// bigDnInfo is a DnInfo whose disk row carries n bytes of text.
func bigDnInfo(n int) *pb.DnInfo {
	return &pb.DnInfo{DiskInfo: &pb.ResInfo{
		ResName: "disk", Details: strings.Repeat("x", n),
	}}
}

// checkDnBig answers every CheckDnRequest with a reply of overGrpcDefault
// bytes and more.
func checkDnBig(stream grpc.BidiStreamingServer[pb.CheckDnRequest, pb.CheckDnReply]) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&pb.CheckDnReply{
			Revision: req.GetRevision(),
			DnInfo:   bigDnInfo(overGrpcDefault),
		}); err != nil {
			return err
		}
	}
}

// msgLimitStub is the stub agent the message-limit cases call: its GetDnInfo
// reply and its CheckDn replies are each above gRPC's receive default, and
// its PushMigrBitmap takes whatever arrives.
func msgLimitStub() *stubDnAgent {
	return &stubDnAgent{
		getDnInfoFn: func(_ context.Context, _ *pb.GetDnInfoRequest) (*pb.GetDnInfoReply, error) {
			return &pb.GetDnInfoReply{DnInfo: bigDnInfo(overGrpcDefault)}, nil
		},
		checkDnFn: checkDnBig,
	}
}

// msgLimitCase is one direction of a message above gRPC's receive default.
type msgLimitCase struct {
	name string
	// stream is set for the case whose server side logs its last record,
	// the stream close, from the handler's goroutine after the call returns.
	stream bool
	call   func(ctx context.Context, client pb.DiskNodeAgentClient) error
}

// msgLimitCases are the three directions, each run over a connection whose
// ends carry the limit and over one whose ends do not: the first must
// succeed, the second must fail with RESOURCE_EXHAUSTED, which is what shows
// the size really is above the default.
func msgLimitCases() []msgLimitCase {
	return []msgLimitCase{
		{
			// The server's receive limit.
			name: "a PushMigrBitmap request",
			call: func(ctx context.Context, client pb.DiskNodeAgentClient) error {
				_, err := client.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
					Bitmap: make([]byte, overGrpcDefault),
				})
				return err
			},
		},
		{
			// The client's receive limit, on a unary call.
			name: "a GetDnInfo reply",
			call: func(ctx context.Context, client pb.DiskNodeAgentClient) error {
				reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{})
				if err == nil && len(reply.GetDnInfo().GetDiskInfo().GetDetails()) != overGrpcDefault {
					return errors.New("the reply lost its text")
				}
				return err
			},
		},
		{
			// The client's receive limit, on a stream.
			name:   "a CheckDn reply",
			stream: true,
			call: func(ctx context.Context, client pb.DiskNodeAgentClient) error {
				stream, err := client.CheckDn(ctx)
				if err != nil {
					return err
				}
				if err := stream.Send(&pb.CheckDnRequest{Revision: 7}); err != nil {
					return err
				}
				reply, err := stream.Recv()
				if err != nil {
					return err
				}
				if reply.GetRevision() != 7 ||
					len(reply.GetDnInfo().GetDiskInfo().GetDetails()) != overGrpcDefault {
					return errors.New("the reply lost its revision or its text")
				}
				return stream.CloseSend()
			},
		},
	}
}

// waitStreamClosed waits until the server has logged the stream close of the
// call under traceId, the last record a stream's server side logs, so no
// record of that call reaches the logger a later test installs.
func waitStreamClosed(t *testing.T, capture *logCapture, traceId string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range strings.Split(capture.buf.String(), "\n") {
			if strings.Contains(line, `"msg":"grpc server stream close"`) &&
				strings.Contains(line, `"`+TraceIdLogKey+`":"`+traceId+`"`) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream under trace id %s never logged its close", traceId)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGrpcMsgOptionsCarryGrpcMaxMsgSize(t *testing.T) {
	// The interceptors log every message whole but its bytes fields, so the
	// replies' text goes to a buffer here instead of the test's stderr.
	capture := captureLogs(t)

	limited := startStubWithMsgLimit(t, msgLimitStub())
	bare := startStub(t, msgLimitStub())
	run := func(c msgLimitCase, client pb.DiskNodeAgentClient, traceId string) error {
		ctx, cancel := context.WithCancel(
			WithTraceId(context.Background(), traceId))
		// Cancelling ends a stream the failed case leaves open, so its
		// server side logs its close.
		defer cancel()
		return c.call(ctx, client)
	}
	for i, c := range msgLimitCases() {
		withId := fmt.Sprintf("msglimit-%d-limited", i)
		if err := run(c, limited, withId); err != nil {
			t.Errorf("%s of %d bytes with the message limit: %v",
				c.name, overGrpcDefault, err)
		}
		withoutId := fmt.Sprintf("msglimit-%d-bare", i)
		if err := run(c, bare, withoutId); status.Code(err) != codes.ResourceExhausted {
			t.Errorf("%s of %d bytes without the message limit = %v, "+
				"want RESOURCE_EXHAUSTED: the size no longer passes gRPC's "+
				"default, so the case above proves nothing",
				c.name, overGrpcDefault, err)
		}
		if c.stream {
			waitStreamClosed(t, capture, withId)
			waitStreamClosed(t, capture, withoutId)
		}
	}

	// The limit is a limit: a request above it is refused.
	_, err := limited.PushMigrBitmap(context.Background(), &pb.PushMigrBitmapRequest{
		Bitmap: make([]byte, GrpcMaxMsgSize+1),
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("PushMigrBitmap above GrpcMaxMsgSize = %v, want RESOURCE_EXHAUSTED",
			err)
	}
}

// ---------------------------------------------------------------------------
// Every dnv gRPC endpoint carries the limit (grpc.md, Wiring; Drivers and
// fakes)
// ---------------------------------------------------------------------------

// msgLimitSkipDirs are the directories the endpoint walk never enters: git
// itself, build output, the untracked working notes and the local settings,
// as doclint skips them.
var msgLimitSkipDirs = map[string]bool{
	".git": true, "bin": true, "tmp_doc": true, ".claude": true,
}

// grpcCallCounts counts, in one parsed file, the calls of grpc.NewServer and
// grpc.NewClient and the calls of the two helpers, by the local names the
// file imports grpc and common under (the helpers' bare names inside package
// common itself).
type grpcCallCounts struct {
	newServer, newClient, serverOpts, clientOpt int
}

func countGrpcCalls(file *ast.File) grpcCallCounts {
	grpcName, commonName := "", ""
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		local := ""
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch path {
		case "google.golang.org/grpc":
			grpcName = "grpc"
			if local != "" {
				grpcName = local
			}
		case "github.com/distributed-nvme/distributed-nvme/common":
			commonName = "common"
			if local != "" {
				commonName = local
			}
		}
	}
	inCommon := file.Name.Name == "common"
	var n grpcCallCounts
	helper := func(fun ast.Expr) string {
		switch f := fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := f.X.(*ast.Ident); ok && commonName != "" && x.Name == commonName {
				return f.Sel.Name
			}
		case *ast.Ident:
			if inCommon {
				return f.Name
			}
		}
		return ""
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && grpcName != "" && x.Name == grpcName {
				switch sel.Sel.Name {
				case "NewServer":
					n.newServer++
				case "NewClient":
					n.newClient++
				}
			}
		}
		switch helper(call.Fun) {
		case "GrpcServerMsgOptions":
			n.serverOpts++
		case "GrpcClientMsgOption":
			n.clientOpt++
		}
		return true
	})
	return n
}

// TestEveryGrpcEndpointSetsTheMessageLimit walks the module's production Go
// files and holds every file that builds a gRPC server or client connection
// to as many calls of the matching helper: a server built without
// GrpcServerMsgOptions, or a connection dialed without GrpcClientMsgOption,
// fails here, by its file, and a new endpoint is held to it the day it is
// written. Test files build bufconn servers of their own and are left out.
func TestEveryGrpcEndpointSetsTheMessageLimit(t *testing.T) {
	const root = ".."
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			if msgLimitSkipDirs[info.Name()] || rel == "integtest/bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") &&
			!strings.HasSuffix(rel, ".pb.go") {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(files)

	var servers, clients []string
	fset := token.NewFileSet()
	for _, rel := range files {
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		n := countGrpcCalls(file)
		if n.newServer > 0 {
			servers = append(servers, rel)
		}
		if n.newClient > 0 {
			clients = append(clients, rel)
		}
		if n.serverOpts < n.newServer {
			t.Errorf("%s: %d grpc.NewServer calls but %d of "+
				"common.GrpcServerMsgOptions: a server without the message "+
				"limit (grpc.md, Wiring)", rel, n.newServer, n.serverOpts)
		}
		if n.clientOpt < n.newClient {
			t.Errorf("%s: %d grpc.NewClient calls but %d of "+
				"common.GrpcClientMsgOption: a connection without the message "+
				"limit (grpc.md, Wiring)", rel, n.newClient, n.clientOpt)
		}
	}
	// A walk that finds no endpoint holds nothing to the rule: the tree's
	// servers are the gateway's, the agents', fakegateway's and fakeagent's,
	// and its connections are the gateway's and the worker's to the agents,
	// dnvctl's and the three drivers'.
	if len(servers) == 0 || len(clients) == 0 {
		t.Fatalf("the walk found servers in %v and connections in %v: it "+
			"did not see the tree", servers, clients)
	}
	t.Logf("servers in %v; client connections in %v", servers, clients)
}
