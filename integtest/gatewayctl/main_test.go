// Tests for the parsing surface of gatewayctl (gateway.md §10.8). The driver
// is what the whole gateway suite asserts through, so a malformed invocation
// or a mis-built request must fail loudly here rather than turn into a wrong
// verdict about the gateway: these cover the flag value types, the
// UPPER_SNAKE code names --expect is matched against, the JSON-params-to-argv
// conversion `race` reuses the command line through, the completeness of the
// subcommand table against the generated service descriptor, and a round trip
// of one representative request per resource group.
package main

import (
	"context"
	"flag"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

func TestHexUintFlag(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"0", 0, true},
		{"17", 17, true},
		{"0x11", 17, true},
		{"0X11", 17, true},
		{" 0x11 ", 17, true},
		{"18446744073709551615", 1<<64 - 1, true},
		{"", 0, false},
		{"-1", 0, false},
		{"sp0", 0, false},
	}
	for _, tc := range cases {
		var id hexUint
		err := id.Set(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("hexUint.Set(%q) failed: %v", tc.in, err)
				continue
			}
			if uint64(id) != tc.want {
				t.Errorf("hexUint.Set(%q) = %d, want %d",
					tc.in, uint64(id), tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("hexUint.Set(%q) = %d, want an error", tc.in, uint64(id))
		}
	}
}

// TestListFlagsReplace pins the one property `race` depends on: a list flag
// REPLACES its value on every Set, so a value coming from a job's params
// overrides the default instead of appending to it.
func TestListFlagsReplace(t *testing.T) {
	var hosts stringList
	if err := hosts.Set("a,b"); err != nil {
		t.Fatalf("stringList.Set: %v", err)
	}
	if err := hosts.Set(" c , d ,"); err != nil {
		t.Fatalf("stringList.Set: %v", err)
	}
	if got := []string(hosts); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Errorf("stringList = %v, want [c d]", got)
	}

	var slots u32List
	if err := slots.Set("0,1,2"); err != nil {
		t.Fatalf("u32List.Set: %v", err)
	}
	if got := []uint32(slots); !reflect.DeepEqual(got, []uint32{0, 1, 2}) {
		t.Errorf("u32List = %v, want [0 1 2]", got)
	}
	if err := slots.Set("0,nope"); err == nil {
		t.Errorf("u32List.Set(0,nope) succeeded, want an error")
	}

	var ids idList
	if err := ids.Set("1,0x63"); err != nil {
		t.Fatalf("idList.Set: %v", err)
	}
	if got := []uint64(ids); !reflect.DeepEqual(got, []uint64{1, 0x63}) {
		t.Errorf("idList = %v, want [1 99]", got)
	}
}

func TestParseHexBitmap(t *testing.T) {
	got, err := parseHexBitmap("0xff00ff")
	if err != nil {
		t.Fatalf("parseHexBitmap: %v", err)
	}
	if !reflect.DeepEqual(got, []byte{0xff, 0x00, 0xff}) {
		t.Errorf("parseHexBitmap = %v, want [255 0 255]", got)
	}
	if _, err := parseHexBitmap(""); err == nil {
		t.Errorf("parseHexBitmap(\"\") succeeded, want an error")
	}
	if _, err := parseHexBitmap("zz"); err == nil {
		t.Errorf("parseHexBitmap(\"zz\") succeeded, want an error")
	}
}

func TestParseSpLevel(t *testing.T) {
	cases := []struct {
		in   string
		want pb.SpLevel
		ok   bool
	}{
		{"READWRITE", pb.SpLevel_SP_LEVEL_READWRITE, true},
		{"readonly", pb.SpLevel_SP_LEVEL_READONLY, true},
		{"SP_LEVEL_NO_THINPOOL", pb.SpLevel_SP_LEVEL_NO_THINPOOL, true},
		{"112", pb.SpLevel_SP_LEVEL_DISABLE, true},
		// A number the enum does not declare is accepted here on purpose, so
		// the §10.14 validation battery can watch the GATEWAY refuse it.
		{"7", pb.SpLevel(7), true},
		{"nope", 0, false},
	}
	for _, tc := range cases {
		got, err := parseSpLevel(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("parseSpLevel(%q) failed: %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("parseSpLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseSpLevel(%q) = %v, want an error", tc.in, got)
		}
	}
}

// TestCodeNamesAreUpperSnake is a regression guard: codes.Code.String()
// renders CamelCase, so upper-casing it yields ALREADYEXISTS and no --expect
// on a multi-word code would ever match. Every name must be UPPER_SNAKE, and
// every code the gateway can return must be in the table.
func TestCodeNamesAreUpperSnake(t *testing.T) {
	for code, name := range codeNames {
		if name != strings.ToUpper(name) {
			t.Errorf("codeName(%v) = %q, want upper case", code, name)
		}
		if strings.ContainsAny(name, " -") {
			t.Errorf("codeName(%v) = %q, want UPPER_SNAKE", code, name)
		}
	}
	want := map[codes.Code]string{
		codes.OK:                 "OK",
		codes.InvalidArgument:    "INVALID_ARGUMENT",
		codes.NotFound:           "NOT_FOUND",
		codes.AlreadyExists:      "ALREADY_EXISTS",
		codes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
		codes.FailedPrecondition: "FAILED_PRECONDITION",
		codes.Aborted:            "ABORTED",
		codes.Unavailable:        "UNAVAILABLE",
	}
	for code, name := range want {
		if got := codeName(code); got != name {
			t.Errorf("codeName(%v) = %q, want %q", code, got, name)
		}
	}
	if got := codeName(codes.Code(99)); got != "CODE_99" {
		t.Errorf("codeName(99) = %q, want CODE_99", got)
	}
}

// TestParamArgs pins the JSON-object-to-argv conversion `race` builds every
// job's request through, so a race job and a command line reach the same
// request builder with the same values.
func TestParamArgs(t *testing.T) {
	got, err := paramArgs(map[string]any{
		"sp":       "sp0",
		"rev":      float64(7),
		"force":    true,
		"disabled": false,
		"hosts":    []any{"nqn.a", "nqn.b"},
		"slots":    []any{float64(0), float64(1)},
		"skipme":   nil,
	})
	if err != nil {
		t.Fatalf("paramArgs: %v", err)
	}
	// Keys are sorted, so the argv is reproducible.
	want := []string{
		"--disabled=false",
		"--force=true",
		"--hosts=nqn.a,nqn.b",
		"--rev=7",
		"--slots=0,1",
		"--sp=sp0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paramArgs =\n  %v\nwant\n  %v", got, want)
	}
	if _, err := paramArgs(map[string]any{
		"bad": map[string]any{"x": 1},
	}); err == nil {
		t.Errorf("paramArgs accepted a nested object, want an error")
	}
}

// TestParamArgsIntegersAreNotScientific guards the trap encoding/json sets:
// every number decodes as float64, and %v on a large one prints
// "6.8719476736e+10", which no uint64 flag can parse.
func TestParamArgsIntegersAreNotScientific(t *testing.T) {
	got, err := paramArgs(map[string]any{"size": float64(68719476736)})
	if err != nil {
		t.Fatalf("paramArgs: %v", err)
	}
	if got[0] != "--size=68719476736" {
		t.Errorf("paramArgs size = %q, want --size=68719476736", got[0])
	}
}

// TestCommandsCoverEveryRpc is the §10.8 completeness check: every method of
// the generated Gateway service descriptor must have a subcommand, so a new
// RPC cannot be added to the proto without the suite gaining a way to drive
// it. It also fails on a subcommand whose setup is nil.
func TestCommandsCoverEveryRpc(t *testing.T) {
	// The subcommand name of an RPC: CreateStoragePool -> create-storage-pool
	// is NOT the spelling §10.8 chose (it abbreviates: create-sp), so the
	// mapping is explicit and this table is what pins it.
	rpcToCmd := map[string]string{
		"CreateCluster": "create-cluster", "DeleteCluster": "delete-cluster",
		"GetCluster": "get-cluster", "ListClusters": "list-clusters",
		"CreateDiskNode": "create-dn", "DeleteDiskNode": "delete-dn",
		"GetDiskNode": "get-dn", "ListDiskNodes": "list-dns",
		"UpdateDiskNodeDisabled":          "set-dn-disabled",
		"InspectDiskNode":                 "inspect-dn",
		"CreateControllerNode":            "create-cn",
		"DeleteControllerNode":            "delete-cn",
		"GetControllerNode":               "get-cn",
		"ListControllerNodes":             "list-cns",
		"UpdateControllerNodeDisabled":    "set-cn-disabled",
		"InspectControllerNode":           "inspect-cn",
		"CreateStoragePool":               "create-sp",
		"DeleteStoragePool":               "delete-sp",
		"GetStoragePool":                  "get-sp",
		"ListStoragePools":                "list-sps",
		"UpdateStoragePoolCntlidSlotList": "set-cntlid-slots",
		"UpdateStoragePoolLevel":          "set-sp-level",
		"FindStoragePoolNames":            "find-sp-names",
		"GrowSlice":                       "grow-slice",
		"CreateCntlr":                     "create-cntlr",
		"DeleteCntlr":                     "delete-cntlr",
		"UpdateCntlrEnabled":              "set-cntlr-enabled",
		"InspectCntlr":                    "inspect-cntlr",
		"InspectSide":                     "inspect-side",
		"CreateThinDevice":                "create-td",
		"DeleteThinDevice":                "delete-td",
		"ListThinDevices":                 "list-tds",
		"CreateSubsystem":                 "create-ss",
		"DeleteSubsystem":                 "delete-ss",
		"ListSubsystems":                  "list-sss",
		"UpdateSubsystemHosts":            "set-ss-hosts",
		"CreateNamespace":                 "create-ns",
		"DeleteNamespace":                 "delete-ns",
		"UpdateNamespaceDev":              "set-ns-dev",
		"UpdateNamespaceSuspended":        "set-ns-suspended",
		"CreateClone":                     "create-clone",
		"DeleteClone":                     "delete-clone",
		"GetClone":                        "get-clone",
		"UpdateCloneTrConf":               "set-clone-tr",
		"AppendCloneBitmap":               "append-clone-bm",
		"CreateTransfer":                  "create-xfer",
		"DeleteTransfer":                  "delete-xfer",
		"GetTransfer":                     "get-xfer",
		"UpdateTransferHosts":             "set-xfer-hosts",
		"CreateMigration":                 "create-migr",
		"FinishMigration":                 "finish-migr",
		"CancelMigration":                 "cancel-migr",
		"GetMigration":                    "get-migr",
		"AppendMigrationBitmap":           "append-migr-bm",
		"CreateSpareLeg":                  "create-spare",
		"DeleteSpareLeg":                  "delete-spare",
		"SwitchSpareLeg":                  "switch-spare",
		"GetThinDeviceBitmap":             "get-td-bm",
		"GetLegBitmap":                    "get-leg-bm",
	}
	if len(pb.Gateway_ServiceDesc.Methods) != 59 {
		t.Fatalf("service Gateway has %d methods, want 59",
			len(pb.Gateway_ServiceDesc.Methods))
	}
	for _, method := range pb.Gateway_ServiceDesc.Methods {
		name, ok := rpcToCmd[method.MethodName]
		if !ok {
			t.Errorf("RPC %s has no subcommand mapping", method.MethodName)
			continue
		}
		if _, ok := lookup(name); !ok {
			t.Errorf("RPC %s maps to subcommand %q, which is not in the table",
				method.MethodName, name)
		}
	}
	for _, cmd := range commands {
		if cmd.setup == nil {
			t.Errorf("subcommand %q has a nil setup", cmd.name)
		}
	}
	// ping is the only subcommand that is not an RPC of its own.
	if len(commands) != len(rpcToCmd)+1 {
		t.Errorf("commands holds %d entries, want %d RPCs + ping",
			len(commands), len(rpcToCmd))
	}
}

// ---------------------------------------------------------------------------
// Request round trips
// ---------------------------------------------------------------------------

// recordingClient implements pb.GatewayClient by embedding the interface and
// overriding only the methods a test drives; anything else panics, which is
// exactly what a test that reached the wrong method wants.
type recordingClient struct {
	pb.GatewayClient
	req proto.Message
}

func (c *recordingClient) CreateStoragePool(
	ctx context.Context, in *pb.CreateStoragePoolRequest, _ ...grpc.CallOption,
) (*pb.CreateStoragePoolReply, error) {
	c.req = in
	return &pb.CreateStoragePoolReply{SpId: 1}, nil
}

func (c *recordingClient) CreateThinDevice(
	ctx context.Context, in *pb.CreateThinDeviceRequest, _ ...grpc.CallOption,
) (*pb.CreateThinDeviceReply, error) {
	c.req = in
	return &pb.CreateThinDeviceReply{TdId: 1, DevId: 1}, nil
}

func (c *recordingClient) AppendCloneBitmap(
	ctx context.Context, in *pb.AppendCloneBitmapRequest, _ ...grpc.CallOption,
) (*pb.AppendCloneBitmapReply, error) {
	c.req = in
	return &pb.AppendCloneBitmapReply{CloneId: 1}, nil
}

func (c *recordingClient) UpdateDiskNodeDisabled(
	ctx context.Context, in *pb.UpdateDiskNodeDisabledRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateDiskNodeDisabledReply, error) {
	c.req = in
	return &pb.UpdateDiskNodeDisabledReply{DnId: 1}, nil
}

// runArgv parses argv through one subcommand's flag set and runs its job
// against a recording client, returning the request that was built. It is the
// same path both the command line and `race` take.
func runArgv(t *testing.T, name string, argv ...string) proto.Message {
	t.Helper()
	cmd, ok := lookup(name)
	if !ok {
		t.Fatalf("no subcommand %q", name)
	}
	g := newGlobals()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	g.bind(fs)
	run := cmd.setup(fs)
	if err := fs.Parse(argv); err != nil {
		t.Fatalf("parsing %v: %v", argv, err)
	}
	client := &recordingClient{}
	if _, err := run(context.Background(), client, &g); err != nil {
		t.Fatalf("running %s: %v", name, err)
	}
	if client.req == nil {
		t.Fatalf("%s built no request", name)
	}
	return client.req
}

// TestCreateSpRequest pins the §10.6 shape the whole suite creates its SPs
// with, including the --raid1 lever that selects RedundMdRaid1 — a RedundNone
// SP would have one leg per group and every leg assertion of §10.11 would be
// wrong.
func TestCreateSpRequest(t *testing.T) {
	req := runArgv(t, "create-sp",
		"--sp", "sp0", "--cntlr-cnt", "2", "--slice-cnt", "1",
		"--init-ext-cnt", "2", "--raid1", "--slots", "0,1,2",
	).(*pb.CreateStoragePoolRequest)
	if req.GetClusterName() != defaultCluster {
		t.Errorf("cluster_name = %q, want %q",
			req.GetClusterName(), defaultCluster)
	}
	if req.GetSpName() != "sp0" {
		t.Errorf("sp_name = %q, want sp0", req.GetSpName())
	}
	if req.GetCntlrCnt() != 2 || req.GetSliceCnt() != 1 ||
		req.GetInitExtCnt() != 2 {
		t.Errorf("shape = %d/%d/%d, want 2/1/2",
			req.GetCntlrCnt(), req.GetSliceCnt(), req.GetInitExtCnt())
	}
	if !reflect.DeepEqual(req.GetCntlidSlotList(), []uint32{0, 1, 2}) {
		t.Errorf("cntlid_slot_list = %v, want [0 1 2]",
			req.GetCntlidSlotList())
	}
	if req.GetBdevConf().GetRedundConf().GetRedundMdRaid1() == nil {
		t.Errorf("--raid1 did not select redund_md_raid1")
	}
}

// TestSpRevTokenIsAlwaysPresent pins §0 #7 as the driver sees it: an omitted
// --rev sends the ZERO token, not an absent message, and zero can never match
// a stored revision that starts at 1. Case B step 4 asserts exactly that.
func TestSpRevTokenIsAlwaysPresent(t *testing.T) {
	req := runArgv(t, "create-td",
		"--sp", "sp0", "--name", "t0", "--size", "67108864",
	).(*pb.CreateThinDeviceRequest)
	if req.GetSpRev() == nil {
		t.Fatalf("sp_rev is nil; an omitted --rev must send the zero token")
	}
	if req.GetSpRev().GetRevision() != 0 {
		t.Errorf("sp_rev.revision = %d, want 0",
			req.GetSpRev().GetRevision())
	}
	req = runArgv(t, "create-td",
		"--sp", "sp0", "--rev", "0x11", "--name", "t0", "--size", "1",
	).(*pb.CreateThinDeviceRequest)
	if req.GetSpRev().GetRevision() != 17 {
		t.Errorf("sp_rev.revision = %d, want 17 (0x11)",
			req.GetSpRev().GetRevision())
	}
}

// TestBitmapIsVerbatim pins GW14 on the driver side: --bm-hex reaches the
// request as raw bytes, so what the suite appends is what etcd stores.
func TestBitmapIsVerbatim(t *testing.T) {
	req := runArgv(t, "append-clone-bm",
		"--sp", "sp0", "--rev", "3", "--name", "cl0",
		"--slice-idx", "0", "--bm-hex", "ff00ff",
	).(*pb.AppendCloneBitmapRequest)
	if !reflect.DeepEqual(req.GetBitmap(), []byte{0xff, 0x00, 0xff}) {
		t.Errorf("bitmap = %v, want [255 0 255]", req.GetBitmap())
	}
	if req.GetSliceIdx() != 0 {
		t.Errorf("slice_idx = %d, want 0", req.GetSliceIdx())
	}
}

// TestBoolFlagsNeedTheEqualsForm documents the trap every case function has to
// respect: Go's flag package never consumes the next argument for a bool, so
// `--disabled false` leaves `false` as a positional and the flag reads TRUE.
// The suite must always write `--disabled=false`.
func TestBoolFlagsNeedTheEqualsForm(t *testing.T) {
	req := runArgv(t, "set-dn-disabled",
		"--addr", "127.0.0.1:29820", "--rev", "1", "--disabled=false",
	).(*pb.UpdateDiskNodeDisabledRequest)
	if req.GetDisabled() {
		t.Errorf("--disabled=false produced disabled = true")
	}
	req = runArgv(t, "set-dn-disabled",
		"--addr", "127.0.0.1:29820", "--rev", "1", "--disabled",
	).(*pb.UpdateDiskNodeDisabledRequest)
	if !req.GetDisabled() {
		t.Errorf("--disabled produced disabled = false")
	}
}
