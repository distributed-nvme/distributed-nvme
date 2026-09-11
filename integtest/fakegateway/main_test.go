package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CT-T6 (dnvctl.md §6): the fakegateway obligations are default success on
// every RPC, behavior.json code/message injection, `hang` released by ctx
// cancel and by lever clear, `reply` injection, state.json count/last_request
// writing and the atomic-rename property, malformed behavior keeping the
// previous behavior, and unknown behavior keys rejected.

// ---------------------------------------------------------------------------
// Harness: one fake gateway behind the real §4 server interceptors on
// bufconn, exactly as main() wires it (the fakeagent house pattern).
// ---------------------------------------------------------------------------

func newTestGateway(t *testing.T) *fakeGateway {
	t.Helper()
	fake, err := newFakeGateway(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("newFakeGateway: %v", err)
	}
	return fake
}

// startGateway serves fake on a bufconn and returns both a typed client and
// the raw connection — the connection is what lets a test drive all 59
// methods in one loop through grpc's generic Invoke.
func startGateway(
	t *testing.T, fake *fakeGateway,
) (pb.GatewayClient, *grpc.ClientConn) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterGatewayServer(server, fake)
	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("fake gateway server stopped: %v", err)
		}
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
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
		conn.Close()
		server.Stop()
		lis.Close()
	})
	return pb.NewGatewayClient(conn), conn
}

// writeFile writes one of the fake's two files and stamps a strictly newer
// mtime, so the reload rules ("mtime or size changed" for behavior.json,
// "newer than the fake's own last write" for state.json) fire deterministically
// however coarse the filesystem's timestamps are.
func writeFile(t *testing.T, fake *fakeGateway, name, content string) {
	t.Helper()
	writeFileAt(t, fake, name, content, time.Now().Add(2*time.Second))
}

// writeFileAt writes one of the fake's two files and stamps an explicit
// mtime, so the reload rules ("mtime or size changed" for behavior.json,
// "newer than the fake's own last write" for state.json) fire
// deterministically however coarse the filesystem's timestamps are — and so
// one test can hold the mtime still to exercise the size half of the guard.
func writeFileAt(
	t *testing.T, fake *fakeGateway, name, content string, mtime time.Time,
) {
	t.Helper()
	path := filepath.Join(fake.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

func setBehavior(t *testing.T, fake *fakeGateway, content string) {
	t.Helper()
	writeFile(t, fake, behaviorFileName, content)
}

// readState returns the parsed state.json, as the suite's jq does.
func readState(t *testing.T, fake *fakeGateway) *stateFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fake.dir, stateFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", stateFileName, err)
	}
	parsed := &stateFile{}
	if err := json.Unmarshal(data, parsed); err != nil {
		t.Fatalf("parsing %s: %v", stateFileName, err)
	}
	return parsed
}

// lastRequest returns one method's stored last_request as a generic JSON
// object, which is how §7.7's assertions look at it.
func lastRequest(t *testing.T, fake *fakeGateway, method string) map[string]any {
	t.Helper()
	entry := readState(t, fake).Methods[method]
	if entry == nil {
		t.Fatalf("state.json has no entry for %s", method)
	}
	if len(entry.LastRequest) == 0 {
		t.Fatalf("state.json has no last_request for %s", method)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(entry.LastRequest, &decoded); err != nil {
		t.Fatalf("parsing %s last_request: %v", method, err)
	}
	return decoded
}

func wantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("%s = %v (code %v), want code %v",
			what, err, status.Code(err), want)
	}
}

func fullMethod(method string) string {
	return "/" + pb.Gateway_ServiceDesc.ServiceName + "/" + method
}

// ---------------------------------------------------------------------------
// The method type registry
// ---------------------------------------------------------------------------

// TestMethodTypeRegistryMatchesServiceDesc pins the registry against
// pb.Gateway_ServiceDesc in both directions, with the 59 of §0 #3 spelled
// out: the registry is what decodes behavior.json's `reply`, so a method it
// misses would silently answer with the wrong message type.
func TestMethodTypeRegistryMatchesServiceDesc(t *testing.T) {
	if len(pb.Gateway_ServiceDesc.Methods) != 59 {
		t.Fatalf("service Gateway has %d methods, want 59",
			len(pb.Gateway_ServiceDesc.Methods))
	}
	if len(gatewayMethodTypes) != 59 {
		t.Fatalf("the registry has %d methods, want 59",
			len(gatewayMethodTypes))
	}
	declared := make(map[string]bool, len(pb.Gateway_ServiceDesc.Methods))
	for _, method := range pb.Gateway_ServiceDesc.Methods {
		declared[method.MethodName] = true
		types, ok := gatewayMethodTypes[method.MethodName]
		if !ok {
			t.Errorf("the registry misses %s", method.MethodName)
			continue
		}
		request := string(types.request.Descriptor().FullName())
		if want := method.MethodName + "Request"; request != want {
			t.Errorf("%s request type = %s, want %s",
				method.MethodName, request, want)
		}
		reply := string(types.reply.Descriptor().FullName())
		if want := method.MethodName + "Reply"; reply != want {
			t.Errorf("%s reply type = %s, want %s",
				method.MethodName, reply, want)
		}
		if newRequest(method.MethodName) == nil ||
			newReply(method.MethodName) == nil {
			t.Errorf("%s does not construct", method.MethodName)
		}
	}
	for name := range gatewayMethodTypes {
		if !declared[name] {
			t.Errorf("the registry has %s, which service Gateway does not",
				name)
		}
	}
	if newReply("NoSuchMethod") != nil || newRequest("NoSuchMethod") != nil {
		t.Errorf("an unknown method constructed a message")
	}
}

// ---------------------------------------------------------------------------
// Default success (CT-T6 "default success on every RPC")
// ---------------------------------------------------------------------------

// TestDefaultSuccessOnEveryRpc drives all 59 methods through the wire with no
// behavior.json at all. Going through grpc's generic Invoke (rather than 59
// typed calls) is deliberate: it also proves every handler passes its OWN
// method name to serve, because state.json is keyed by that string and the
// loop asserts the key the ServiceDesc declares.
func TestDefaultSuccessOnEveryRpc(t *testing.T) {
	fake := newTestGateway(t)
	_, conn := startGateway(t, fake)
	ctx := context.Background()

	for _, method := range pb.Gateway_ServiceDesc.Methods {
		name := method.MethodName
		reply := newReply(name)
		err := conn.Invoke(ctx, fullMethod(name), newRequest(name), reply)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// §7.5: the canned default is an EMPTY reply message; dnvctl's
		// EmitUnpopulated rendering fills the shape client-side.
		if !proto.Equal(reply, newReply(name)) {
			t.Errorf("%s replied %v, want the empty message", name, reply)
		}
	}

	state := readState(t, fake)
	if len(state.Methods) != 59 {
		t.Fatalf("state.json records %d methods, want 59",
			len(state.Methods))
	}
	for _, method := range pb.Gateway_ServiceDesc.Methods {
		entry := state.Methods[method.MethodName]
		if entry == nil {
			t.Errorf("state.json has no entry for %s", method.MethodName)
			continue
		}
		if entry.Count != 1 {
			t.Errorf("%s count = %d, want 1", method.MethodName, entry.Count)
		}
		if len(entry.LastRequest) == 0 {
			t.Errorf("%s has no last_request", method.MethodName)
		}
	}
}

// ---------------------------------------------------------------------------
// state.json (CT-T6 "count/last_request writing")
// ---------------------------------------------------------------------------

// TestStateCountsAndLastRequest pins the count bump and the protojson
// rendering of the §4 token trio — the assertion §7.11 b1/b2/b3 is built on.
// UseProtoNames WITHOUT EmitUnpopulated is what makes an absent token an
// ABSENT KEY and a present-but-zero token `{}`; emitting unpopulated fields
// would render both as `{}` and quietly destroy the distinction.
func TestStateCountsAndLastRequest(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	// b1: no token at all.
	_, err := client.CreateThinDevice(ctx, &pb.CreateThinDeviceRequest{
		ClusterName: "itctl",
		SpName:      "sp0",
		TdName:      "t0",
		Size:        67108864,
	})
	if err != nil {
		t.Fatalf("CreateThinDevice: %v", err)
	}
	req := lastRequest(t, fake, "CreateThinDevice")
	if _, ok := req["sp_rev"]; ok {
		t.Errorf("a token-less request stored sp_rev = %v, want no key",
			req["sp_rev"])
	}
	if req["cluster_name"] != "itctl" || req["sp_name"] != "sp0" {
		t.Errorf("stored globals = %v/%v, want itctl/sp0",
			req["cluster_name"], req["sp_name"])
	}
	if req["td_name"] != "t0" {
		t.Errorf("stored td_name = %v, want t0", req["td_name"])
	}
	// §7.7: protojson renders uint64 as a string.
	if req["size"] != "67108864" {
		t.Errorf("stored size = %#v, want the string \"67108864\"",
			req["size"])
	}
	if _, ok := req["ori_name"]; ok {
		t.Errorf("an unset string stored ori_name = %v, want no key",
			req["ori_name"])
	}

	// b2: --rev 0, a present message with revision 0.
	_, err = client.CreateThinDevice(ctx, &pb.CreateThinDeviceRequest{
		ClusterName: "itctl",
		SpName:      "sp0",
		SpRev:       &pb.SpRev{},
		TdName:      "t0",
	})
	if err != nil {
		t.Fatalf("CreateThinDevice --rev 0: %v", err)
	}
	req = lastRequest(t, fake, "CreateThinDevice")
	rev, ok := req["sp_rev"]
	if !ok {
		t.Fatalf("a present zero token stored no sp_rev key")
	}
	if fields, isMap := rev.(map[string]any); !isMap || len(fields) != 0 {
		t.Errorf("a present zero token stored sp_rev = %#v, want {}", rev)
	}

	// b3: a real revision, as a string.
	_, err = client.CreateThinDevice(ctx, &pb.CreateThinDeviceRequest{
		ClusterName: "itctl",
		SpName:      "sp0",
		SpRev:       &pb.SpRev{Revision: 31},
		TdName:      "t0",
	})
	if err != nil {
		t.Fatalf("CreateThinDevice --rev 0x1f: %v", err)
	}
	req = lastRequest(t, fake, "CreateThinDevice")
	fields, isMap := req["sp_rev"].(map[string]any)
	if !isMap || fields["revision"] != "31" {
		t.Errorf("stored sp_rev = %#v, want {\"revision\": \"31\"}",
			req["sp_rev"])
	}

	// Three calls, one method, one entry.
	state := readState(t, fake)
	if len(state.Methods) != 1 {
		t.Errorf("state.json records %d methods, want 1", len(state.Methods))
	}
	if got := state.Methods["CreateThinDevice"].Count; got != 3 {
		t.Errorf("CreateThinDevice count = %d, want 3", got)
	}
}

// TestRecordingPrecedesBehavior pins §7.5's "Request recording (always
// first)": a call the behaviour refuses is still counted and still stores its
// request. §7.12's error steps assert both the stderr line and the counts,
// and a fake that recorded after the gate would report zero.
func TestRecordingPrecedesBehavior(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	setBehavior(t, fake, `{"methods": {"GetStoragePool": {"code": "NOT_FOUND"}}}`)
	_, err := client.GetStoragePool(ctx, &pb.GetStoragePoolRequest{
		ClusterName: "itctl",
		SpName:      "sp0",
	})
	wantCode(t, "GetStoragePool", err, codes.NotFound)

	entry := readState(t, fake).Methods["GetStoragePool"]
	if entry == nil || entry.Count != 1 {
		t.Fatalf("a refused call recorded %v, want count 1", entry)
	}
	req := lastRequest(t, fake, "GetStoragePool")
	if req["sp_name"] != "sp0" {
		t.Errorf("a refused call stored sp_name = %v, want sp0", req["sp_name"])
	}
}

// TestStateSurvivesRestart proves the counts are loaded at start, so a fake
// that is killed and restarted mid-suite keeps counting where it left off.
func TestStateSurvivesRestart(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := client.ListClusters(ctx, &pb.ListClustersRequest{
			Count: 2,
		}); err != nil {
			t.Fatalf("ListClusters: %v", err)
		}
	}

	restarted, err := newFakeGateway(context.Background(), fake.dir)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	client, _ = startGateway(t, restarted)
	if _, err := client.ListClusters(ctx, &pb.ListClustersRequest{
		Count: 2,
	}); err != nil {
		t.Fatalf("ListClusters after restart: %v", err)
	}
	if got := readState(t, restarted).Methods["ListClusters"].Count; got != 3 {
		t.Errorf("ListClusters count after restart = %d, want 3", got)
	}
}

// TestStateHandEditIsPickedUp covers the own-write mtime guard from the other
// side: the fake must NOT re-read its own writes (or every call would reload
// what it just wrote), but a file strictly newer than its last write is an
// operator edit and must win — the way a suite resets a count between cases.
func TestStateHandEditIsPickedUp(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	if _, err := client.ListClusters(ctx, &pb.ListClustersRequest{}); err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	writeFile(t, fake, stateFileName,
		`{"methods": {"ListClusters": {"count": 41}}}`)
	if _, err := client.ListClusters(ctx, &pb.ListClustersRequest{}); err != nil {
		t.Fatalf("ListClusters after the edit: %v", err)
	}
	if got := readState(t, fake).Methods["ListClusters"].Count; got != 42 {
		t.Errorf("ListClusters count = %d, want 42 (41 + this call)", got)
	}
}

// TestStateWriteIsAtomic is CT-T6's atomic-rename property. Concurrent
// callers hammer the fake while a reader reads state.json in a tight loop:
// with the temp-file + rename write every read sees a whole file, and no
// temp file is left behind. A plain os.WriteFile would fail this within a
// handful of iterations.
func TestStateWriteIsAtomic(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()
	path := filepath.Join(fake.dir, stateFileName)

	const callers = 8
	const calls = 25
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < calls; j++ {
				_, err := client.CreateThinDevice(ctx,
					&pb.CreateThinDeviceRequest{
						TdName: fmt.Sprintf("t%d-%d", id, j),
					})
				if err != nil {
					t.Errorf("CreateThinDevice: %v", err)
					return
				}
			}
		}(i)
	}

	stop := make(chan struct{})
	readerDone := make(chan struct{})
	reads, torn := 0, 0
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Errorf("reading %s: %v", stateFileName, err)
				return
			}
			reads++
			parsed := &stateFile{}
			if err := json.Unmarshal(data, parsed); err != nil {
				torn++
				t.Errorf("read %d of %s is torn: %v",
					reads, stateFileName, err)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-readerDone

	if torn != 0 {
		t.Errorf("%d of %d reads were torn", torn, reads)
	}
	if reads == 0 {
		t.Errorf("the reader never read the file")
	}
	if got := readState(t, fake).Methods["CreateThinDevice"].Count; got !=
		callers*calls {
		t.Errorf("CreateThinDevice count = %d, want %d", got, callers*calls)
	}
	entries, err := os.ReadDir(fake.dir)
	if err != nil {
		t.Fatalf("reading the dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stateFileName+".tmp") {
			t.Errorf("the write left %s behind", entry.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// behavior.json parsing
// ---------------------------------------------------------------------------

// TestParseBehaviorEveryLever checks that every §7.5 key lands, including the
// per-method reply decoded against that method's own reply type.
func TestParseBehaviorEveryLever(t *testing.T) {
	parsed, err := parseBehavior([]byte(`{
		"default": {"code": "unavailable", "message": "down", "hang": false},
		"methods": {
			"GetStoragePool": {
				"code": "NOT_FOUND",
				"reply": {"sp_name": "sp0", "sp_rev": {"revision": "7"}}
			},
			"ListClusters": {"hang": true}
		}
	}`))
	if err != nil {
		t.Fatalf("parseBehavior: %v", err)
	}
	if parsed.Default.code != codes.Unavailable {
		t.Errorf("default code = %v, want Unavailable", parsed.Default.code)
	}
	if *parsed.Default.Message != "down" {
		t.Errorf("default message = %q, want down", *parsed.Default.Message)
	}
	if *parsed.Default.Hang {
		t.Errorf("default hang = true, want false")
	}
	sp := parsed.Methods["GetStoragePool"]
	if sp.code != codes.NotFound {
		t.Errorf("GetStoragePool code = %v, want NotFound", sp.code)
	}
	reply, ok := sp.reply.(*pb.GetStoragePoolReply)
	if !ok {
		t.Fatalf("GetStoragePool reply is %T, want *pb.GetStoragePoolReply",
			sp.reply)
	}
	if reply.GetSpName() != "sp0" || reply.GetSpRev().GetRevision() != 7 {
		t.Errorf("GetStoragePool reply = %v, want sp0/7", reply)
	}
	if !*parsed.Methods["ListClusters"].Hang {
		t.Errorf("ListClusters hang = false, want true")
	}
}

// TestParseBehaviorMalformed is CT-T6's "unknown behavior keys rejected",
// widened to every way the file can be wrong. Each of these must fail the
// whole file: a rejection that is logged and ignored keeps the previous
// behaviour, whereas a silently dropped key would fail a test far from its
// cause.
func TestParseBehaviorMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"broken json", `{"default": {`},
		{"unknown top-level field", `{"objects": {}}`},
		{"unknown entry field", `{"methods": {"GetCluster": {"hang_it": true}}}`},
		{"unknown method", `{"methods": {"GetClusters": {"code": "OK"}}}`},
		{"unknown code", `{"default": {"code": "NOPE"}}`},
		{"camel-case code", `{"default": {"code": "NotFound"}}`},
		{"wrong type", `{"default": {"hang": "yes"}}`},
		{"reply under default", `{"default": {"reply": {}}}`},
		{"unknown reply field",
			`{"methods": {"GetCluster": {"reply": {"nope": 1}}}}`},
		{"reply of the wrong shape",
			`{"methods": {"GetCluster": {"reply": {"cluster_name": 7}}}}`},
		{"reply is not an object",
			`{"methods": {"GetCluster": {"reply": "x"}}}`},
		{"trailing data", `{} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBehavior([]byte(tc.data)); err == nil {
				t.Errorf("parseBehavior(%s) succeeded, want an error",
					tc.data)
			}
		})
	}
}

// TestParseCodeSpellings pins the UPPER_SNAKE vocabulary of §7.5 against the
// table dnvctl's §3.2 error line renders from: an operator who copies a code
// out of an error line must be able to paste it into behavior.json.
func TestParseCodeSpellings(t *testing.T) {
	for code, name := range codeNames {
		got, err := parseCode(name)
		if err != nil {
			t.Errorf("parseCode(%q): %v", name, err)
			continue
		}
		if got != code {
			t.Errorf("parseCode(%q) = %v, want %v", name, got, code)
		}
		if back := codeName(code); back != name {
			t.Errorf("codeName(%v) = %q, want %q", code, back, name)
		}
	}
	// codes.Canceled has two accepted spellings and no other code does.
	for _, name := range []string{"CANCELLED", "CANCELED", " cancelled "} {
		if got, err := parseCode(name); err != nil || got != codes.Canceled {
			t.Errorf("parseCode(%q) = %v, %v, want Canceled", name, got, err)
		}
	}
	if _, err := parseCode("NO_SUCH_CODE"); err == nil {
		t.Errorf("parseCode(NO_SUCH_CODE) succeeded, want an error")
	}
	if got := codeName(codes.Code(99)); got != "CODE_99" {
		t.Errorf("codeName(99) = %q, want CODE_99", got)
	}
}

// ---------------------------------------------------------------------------
// behavior.json injection (CT-T6 "code/message injection", "reply injection")
// ---------------------------------------------------------------------------

func TestBehaviorCodeAndMessageInjection(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	// A code with an explicit message — §7.12 c2, the §4 failure an operator
	// actually meets.
	setBehavior(t, fake, `{"methods": {"DeleteThinDevice": {
		"code": "ABORTED", "message": "stale revision"}}}`)
	_, err := client.DeleteThinDevice(ctx, &pb.DeleteThinDeviceRequest{
		TdName: "t0",
	})
	wantCode(t, "DeleteThinDevice", err, codes.Aborted)
	if got := status.Convert(err).Message(); got != "stale revision" {
		t.Errorf("message = %q, want %q", got, "stale revision")
	}

	// No message: §7.5's default, "behavior.json <code>".
	setBehavior(t, fake,
		`{"methods": {"CreateCluster": {"code": "ALREADY_EXISTS"}}}`)
	_, err = client.CreateCluster(ctx, &pb.CreateClusterRequest{
		ClusterName: "c1",
	})
	wantCode(t, "CreateCluster", err, codes.AlreadyExists)
	if got := status.Convert(err).Message(); got != "behavior.json ALREADY_EXISTS" {
		t.Errorf("message = %q, want %q", got, "behavior.json ALREADY_EXISTS")
	}

	// An untouched method still succeeds.
	if _, err := client.GetCluster(ctx, &pb.GetClusterRequest{}); err != nil {
		t.Errorf("GetCluster: %v", err)
	}
}

// TestBehaviorMergeIsKeyByKey pins §7.5's "most specific wins KEY BY KEY":
// a method entry that sets only the message keeps the default's code, and a
// method entry may switch a default lever back off.
func TestBehaviorMergeIsKeyByKey(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	setBehavior(t, fake, `{
		"default": {"code": "UNAVAILABLE", "message": "gateway down"},
		"methods": {
			"GetCluster": {"message": "only the message"},
			"ListClusters": {"code": "OK"}
		}
	}`)

	// Neither key overridden: the default applies whole.
	_, err := client.GetStoragePool(ctx, &pb.GetStoragePoolRequest{})
	wantCode(t, "GetStoragePool", err, codes.Unavailable)
	if got := status.Convert(err).Message(); got != "gateway down" {
		t.Errorf("GetStoragePool message = %q, want %q", got, "gateway down")
	}

	// Only the message overridden: the default's code survives.
	_, err = client.GetCluster(ctx, &pb.GetClusterRequest{})
	wantCode(t, "GetCluster", err, codes.Unavailable)
	if got := status.Convert(err).Message(); got != "only the message" {
		t.Errorf("GetCluster message = %q, want %q", got, "only the message")
	}

	// Only the code overridden, back to OK: the call succeeds.
	if _, err := client.ListClusters(ctx, &pb.ListClustersRequest{}); err != nil {
		t.Errorf("ListClusters = %v, want success", err)
	}
}

// TestBehaviorReplyInjection covers the reply lever and the two rules around
// it: an injected reply is what the caller gets, and it is CLONED, so a
// caller that mutates its reply cannot poison the next call.
func TestBehaviorReplyInjection(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	setBehavior(t, fake, `{"methods": {"GetStoragePool": {"reply": {
		"sp_name": "sp0", "sp_rev": {"revision": "7"}}}}}`)
	reply, err := client.GetStoragePool(ctx, &pb.GetStoragePoolRequest{})
	if err != nil {
		t.Fatalf("GetStoragePool: %v", err)
	}
	if reply.GetSpName() != "sp0" || reply.GetSpRev().GetRevision() != 7 {
		t.Fatalf("GetStoragePool = %v, want sp0/7", reply)
	}

	// A method without an injected reply keeps the canned empty one.
	td, err := client.ListThinDevices(ctx, &pb.ListThinDevicesRequest{})
	if err != nil {
		t.Fatalf("ListThinDevices: %v", err)
	}
	if len(td.GetNameToTd()) != 0 {
		t.Errorf("ListThinDevices = %v, want the empty message", td)
	}

	// The second call must see the injected reply again, unchanged.
	reply, err = client.GetStoragePool(ctx, &pb.GetStoragePoolRequest{})
	if err != nil {
		t.Fatalf("GetStoragePool again: %v", err)
	}
	if reply.GetSpRev().GetRevision() != 7 {
		t.Errorf("the second GetStoragePool = %v, want sp0/7", reply)
	}
}

// TestBehaviorMalformedKeepsPrevious is CT-T6's "malformed behavior keeps the
// previous behavior", plus the file-gone reset. The script writes
// behavior.json atomically for exactly this reason (§7.7); a half-written
// file that was parsed, rejected and ignored would otherwise silently keep
// the previous case's behaviour.
func TestBehaviorMalformedKeepsPrevious(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	setBehavior(t, fake, `{"methods": {"GetCluster": {"code": "NOT_FOUND"}}}`)
	_, err := client.GetCluster(ctx, &pb.GetClusterRequest{})
	wantCode(t, "GetCluster", err, codes.NotFound)

	// Malformed: logged and ignored, the previous behaviour still answers.
	setBehavior(t, fake, `{"methods": {"GetCluster": {`)
	_, err = client.GetCluster(ctx, &pb.GetClusterRequest{})
	wantCode(t, "GetCluster after a malformed file", err, codes.NotFound)

	// An unknown key is malformed too, by the same rule.
	setBehavior(t, fake, `{"methods": {"GetCluster": {"cod": "OK"}}}`)
	_, err = client.GetCluster(ctx, &pb.GetClusterRequest{})
	wantCode(t, "GetCluster after an unknown key", err, codes.NotFound)

	// File gone: back to the built-in defaults.
	if err := os.Remove(filepath.Join(fake.dir, behaviorFileName)); err != nil {
		t.Fatalf("removing %s: %v", behaviorFileName, err)
	}
	if _, err := client.GetCluster(ctx, &pb.GetClusterRequest{}); err != nil {
		t.Errorf("GetCluster after the file went = %v, want success", err)
	}

	// And `{}` is the same as absent (the suite's case_reset).
	setBehavior(t, fake, `{}`)
	if _, err := client.GetCluster(ctx, &pb.GetClusterRequest{}); err != nil {
		t.Errorf("GetCluster under {} = %v, want success", err)
	}
}

// TestBehaviorReloadNoticesSizeChange pins the "OR size" half of §7.5's
// reload guard by holding the mtime still across the rewrite: on a coarse
// filesystem clock a same-second rewrite of a different length is the real
// case, and an mtime-only guard would keep serving the previous behaviour.
func TestBehaviorReloadNoticesSizeChange(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()
	frozen := time.Now().Add(2 * time.Second)

	writeFileAt(t, fake, behaviorFileName,
		`{"methods": {"GetCluster": {"code": "NOT_FOUND"}}}`, frozen)
	_, err := client.GetCluster(ctx, &pb.GetClusterRequest{})
	wantCode(t, "GetCluster", err, codes.NotFound)

	// Same mtime, different length.
	writeFileAt(t, fake, behaviorFileName,
		`{"methods": {"GetCluster": {"code": "DATA_LOSS", "hang": false}}}`,
		frozen)
	_, err = client.GetCluster(ctx, &pb.GetClusterRequest{})
	wantCode(t, "GetCluster after a same-mtime rewrite", err, codes.DataLoss)
}

// ---------------------------------------------------------------------------
// The hang lever (CT-T6 "hang released by ctx cancel and by lever clear")
// ---------------------------------------------------------------------------

// hangingCall starts one call in a goroutine and proves it has NOT answered
// within two poll intervals, returning the channel its result will arrive on.
func hangingCall(
	t *testing.T, ctx context.Context, client pb.GatewayClient,
) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := client.ListClusters(ctx, &pb.ListClustersRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a hanging call answered: %v", err)
	case <-time.After(2 * hangPollInterval):
	}
	return done
}

// TestHangReleasedByContextCancel is how §7.13 case D step 2 manufactures a
// DEADLINE_EXCEEDED: dnvctl's --timeout cancels the call's context and the
// fake must turn that into a proper gRPC code rather than hanging forever.
func TestHangReleasedByContextCancel(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)

	setBehavior(t, fake, `{"methods": {"ListClusters": {"hang": true}}}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := hangingCall(t, ctx, client)

	cancel()
	select {
	case err := <-done:
		wantCode(t, "the cancelled call", err, codes.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatalf("the cancelled call never returned")
	}

	// A deadline is the shape dnvctl actually uses.
	setBehavior(t, fake, `{"default": {"hang": true}}`)
	deadlineCtx, deadlineCancel := context.WithTimeout(
		context.Background(), 3*hangPollInterval)
	defer deadlineCancel()
	_, err := client.GetCluster(deadlineCtx, &pb.GetClusterRequest{})
	wantCode(t, "the expired call", err, codes.DeadlineExceeded)
}

// TestHangReleasedByClearingLever covers the other release path and the rule
// that makes it possible: the mutex is never held while waiting, so a hung
// call neither wedges the process nor blocks the write that frees it.
func TestHangReleasedByClearingLever(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	setBehavior(t, fake, `{"methods": {"ListClusters": {"hang": true}}}`)
	done := hangingCall(t, ctx, client)

	// Another method answers while the first one hangs.
	if _, err := client.GetCluster(ctx, &pb.GetClusterRequest{}); err != nil {
		t.Fatalf("GetCluster while ListClusters hangs: %v", err)
	}
	// And the hung call was recorded before it hung (§7.5).
	if entry := readState(t, fake).Methods["ListClusters"]; entry == nil ||
		entry.Count != 1 {
		t.Errorf("the hung call recorded %v, want count 1", entry)
	}

	setBehavior(t, fake, `{}`)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the released call = %v, want success", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("clearing the lever did not release the call")
	}

	// §7.13 d2's recovery step: the method works again afterwards.
	if _, err := client.ListClusters(ctx, &pb.ListClustersRequest{}); err != nil {
		t.Errorf("ListClusters after the lever cleared: %v", err)
	}
}

// TestHangAndCodeOrder pins that `hang` is applied BEFORE `code`: the
// transport case needs a call that never answers, not one that is refused
// after a delay.
func TestHangAndCodeOrder(t *testing.T) {
	fake := newTestGateway(t)
	client, _ := startGateway(t, fake)
	ctx := context.Background()

	setBehavior(t, fake, `{"methods": {"ListClusters": {
		"hang": true, "code": "NOT_FOUND"}}}`)
	done := hangingCall(t, ctx, client)

	setBehavior(t, fake, `{"methods": {"ListClusters": {"code": "NOT_FOUND"}}}`)
	select {
	case err := <-done:
		wantCode(t, "the released call", err, codes.NotFound)
	case <-time.After(5 * time.Second):
		t.Fatalf("clearing the lever did not release the call")
	}
}
