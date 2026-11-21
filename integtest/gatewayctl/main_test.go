// Tests for the parsing surface of gatewayctl
// (gateway.md, Integration test plan, The driver, `gatewayctl`). The driver
// is what the whole gateway suite asserts through, so a malformed invocation
// or a mis-built request must fail loudly here rather than turn into a wrong
// verdict about the gateway: these cover the flag value types, the
// UPPER_SNAKE code names --expect is matched against, the JSON-params-to-argv
// conversion `race` reuses the command line through, the completeness of the
// subcommand table against the generated service descriptor, and a round trip
// of one representative request per resource group.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
		// A number the enum does not declare is accepted here on purpose:
		// refusing it is the gateway's job (validateSpLevel). No suite stage
		// sends one, and TestValidateSpLevel pins that refusal.
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

// TestCodeNamesAreUpperSnake pins that every name is UPPER_SNAKE:
// codes.Code.String() renders CamelCase, so upper-casing it yields
// ALREADYEXISTS and no --expect on a multi-word code would ever match. Every
// code the gateway can return must be in the table.
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

// TestCommandsCoverEveryRpc is the driver's completeness check: every method of
// the generated Gateway service descriptor must have a subcommand, so a new
// RPC cannot be added to the proto without the suite gaining a way to drive
// it. It also fails on a subcommand whose setup is nil.
func TestCommandsCoverEveryRpc(t *testing.T) {
	// The subcommand name of an RPC: CreateStoragePool -> create-storage-pool
	// is NOT the spelling the driver chose (it abbreviates: create-sp), so the
	// mapping is explicit and this table is what pins it.
	rpcToCmd := map[string]string{
		"CreateCluster": "create-cluster", "DeleteCluster": "delete-cluster",
		"GetCluster": "get-cluster", "ListClusters": "list-clusters",
		"GetClusterCapacity": "get-cluster-capacity",
		"GetClusterStatus":   "get-cluster-status",
		"AdvanceClusterIds":  "advance-cluster-ids",
		"ListWorkers":        "list-workers",
		"CreateDiskNode":     "create-dn", "DeleteDiskNode": "delete-dn",
		"GetDiskNode": "get-dn", "ListDiskNodes": "list-dns",
		"UpdateDiskNodeDisabled":          "set-dn-disabled",
		"InspectDiskNode":                 "inspect-dn",
		"CreateControllerNode":            "create-cn",
		"DeleteControllerNode":            "delete-cn",
		"GetControllerNode":               "get-cn",
		"ListControllerNodes":             "list-cns",
		"UpdateControllerNodeDisabled":    "set-cn-disabled",
		"InspectControllerNode":           "inspect-cn",
		"CheckControllerNodeRestore":      "check-restore",
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
		"InspectStoragePool":              "inspect-sp",
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
		"CreateRepair":                    "create-repair",
		"FinishRepair":                    "finish-repair",
		"CancelRepair":                    "cancel-repair",
		"TryToAbortRepair":                "abort-repair",
		"GetRepair":                       "get-repair",
		"ListRepairs":                     "list-repairs",
		"ListRetiredLegs":                 "list-retired-legs",
		"DeleteRetiredLeg":                "delete-retired-leg",
		"UpdateRetiredLegKeep":            "keep-retired-leg",
		"ForceStartGroup":                 "force-start-grp",
		"InspectGroup":                    "inspect-grp",
		"UpdateGroupOperatorInCharge":     "set-grp-operator-in-charge",
		"GetThinDeviceBitmap":             "get-td-bm",
	}
	if len(pb.Gateway_ServiceDesc.Methods) != 72 {
		t.Fatalf("service Gateway has %d methods, want 72",
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

func (c *recordingClient) CreateCluster(
	ctx context.Context, in *pb.CreateClusterRequest, _ ...grpc.CallOption,
) (*pb.CreateClusterReply, error) {
	c.req = in
	return &pb.CreateClusterReply{ClusterId: 1}, nil
}

func (c *recordingClient) DeleteDiskNode(
	ctx context.Context, in *pb.DeleteDiskNodeRequest, _ ...grpc.CallOption,
) (*pb.DeleteDiskNodeReply, error) {
	c.req = in
	return &pb.DeleteDiskNodeReply{DnId: 1}, nil
}

func (c *recordingClient) DeleteControllerNode(
	ctx context.Context, in *pb.DeleteControllerNodeRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteControllerNodeReply, error) {
	c.req = in
	return &pb.DeleteControllerNodeReply{CnId: 1}, nil
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

func (c *recordingClient) CreateRepair(
	ctx context.Context, in *pb.CreateRepairRequest, _ ...grpc.CallOption,
) (*pb.CreateRepairReply, error) {
	c.req = in
	return &pb.CreateRepairReply{RepId: 1, NewLegId: 2}, nil
}

func (c *recordingClient) ForceStartGroup(
	ctx context.Context, in *pb.ForceStartGroupRequest, _ ...grpc.CallOption,
) (*pb.ForceStartGroupReply, error) {
	c.req = in
	return &pb.ForceStartGroupReply{GrpId: in.GetGrpId(), Revision: 3}, nil
}

func (c *recordingClient) InspectGroup(
	ctx context.Context, in *pb.InspectGroupRequest,
	_ ...grpc.CallOption,
) (*pb.InspectGroupReply, error) {
	c.req = in
	return &pb.InspectGroupReply{GrpId: in.GetGrpId()}, nil
}

func (c *recordingClient) UpdateRetiredLegKeep(
	ctx context.Context, in *pb.UpdateRetiredLegKeepRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateRetiredLegKeepReply, error) {
	c.req = in
	return &pb.UpdateRetiredLegKeepReply{LegId: in.GetLegId()}, nil
}

func (c *recordingClient) UpdateGroupOperatorInCharge(
	ctx context.Context, in *pb.UpdateGroupOperatorInChargeRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateGroupOperatorInChargeReply, error) {
	c.req = in
	return &pb.UpdateGroupOperatorInChargeReply{GrpId: in.GetGrpId()}, nil
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
	if err := parseArgs(fs, argv); err != nil {
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

// TestCreateRepairRequest pins the request create-repair builds: the bad leg
// list as typed, the retired leg id, the selector, the two force flags each
// sent only when typed, and a token message that is present, a zero one when
// --rev is not given (GW6's always-stale probe).
func TestCreateRepairRequest(t *testing.T) {
	req := runArgv(t, "create-repair",
		"--sp", "sp0", "--rev", "7", "--name", "r0", "--grp", "0x11",
		"--bad-legs", "3,4", "--retired-leg", "5", "--dn-white", "a:1",
	).(*pb.CreateRepairRequest)
	want := &pb.CreateRepairRequest{
		ClusterName:  defaultCluster,
		SpName:       "sp0",
		SpRev:        &pb.SpRev{Revision: 7},
		RepName:      "r0",
		GrpId:        0x11,
		BadLegIdList: []uint64{3, 4},
		RetiredLegId: 5,
		DnSelector:   &pb.NodeSelector{WhiteList: []string{"a:1"}},
	}
	if !proto.Equal(req, want) {
		t.Errorf("request:\n got %v\nwant %v", req, want)
	}
	bare := runArgv(t, "create-repair", "--sp", "sp0", "--name", "r0").(*pb.CreateRepairRequest)
	if bare.SpRev == nil || bare.GetSpRev().GetRevision() != 0 {
		t.Errorf("no --rev sent sp_rev %v, want a present zero token",
			bare.SpRev)
	}
	if bare.GetForce() || bare.GetForceUndecided() {
		t.Errorf("a create with neither flag sent %v", bare)
	}
	forced := runArgv(t, "create-repair", "--sp", "sp0", "--name", "r0",
		"--force", "--force-undecided").(*pb.CreateRepairRequest)
	if !forced.GetForce() || !forced.GetForceUndecided() {
		t.Errorf("--force --force-undecided sent %v, want both set", forced)
	}
}

// TestDeleteNodeRequests pins the requests delete-dn and delete-cn build:
// the address, a token message that is present, and force sent only when
// typed.
func TestDeleteNodeRequests(t *testing.T) {
	dn := runArgv(t, "delete-dn", "--addr", "a:1", "--rev", "7",
		"--force").(*pb.DeleteDiskNodeRequest)
	wantDn := &pb.DeleteDiskNodeRequest{
		ClusterName: defaultCluster,
		AddrPort:    "a:1",
		DnRev:       &pb.DnRev{Revision: 7},
		Force:       true,
	}
	if !proto.Equal(dn, wantDn) {
		t.Errorf("delete-dn request:\n got %v\nwant %v", dn, wantDn)
	}
	cn := runArgv(t, "delete-cn", "--addr", "a:2",
		"--force").(*pb.DeleteControllerNodeRequest)
	wantCn := &pb.DeleteControllerNodeRequest{
		ClusterName: defaultCluster,
		AddrPort:    "a:2",
		CnRev:       &pb.CnRev{},
		Force:       true,
	}
	if !proto.Equal(cn, wantCn) {
		t.Errorf("delete-cn request:\n got %v\nwant %v", cn, wantCn)
	}
	for _, name := range []string{"delete-dn", "delete-cn"} {
		bare := runArgv(t, name, "--addr", "a:3")
		if bare.ProtoReflect().Get(bare.ProtoReflect().Descriptor().
			Fields().ByName("force")).Bool() {
			t.Errorf("a bare %s sent force", name)
		}
	}
}

// TestForceStartGrpRequest pins the request force-start-grp builds: the
// group, the leg, the cancel flag, and a token message that is present, a
// zero one when --rev is not given; a cancel carries the leg as typed, so
// the suite can send the pair the gateway refuses; --override sends each
// word as its kind and a number as typed, so the suite can send a value
// that names no refusal, on force-start-grp and inspect-grp alike.
func TestForceStartGrpRequest(t *testing.T) {
	req := runArgv(t, "force-start-grp",
		"--sp", "sp0", "--rev", "7", "--grp", "0x11", "--leg", "0x12",
	).(*pb.ForceStartGroupRequest)
	want := &pb.ForceStartGroupRequest{
		ClusterName: defaultCluster,
		SpName:      "sp0",
		SpRev:       &pb.SpRev{Revision: 7},
		GrpId:       0x11,
		LegId:       0x12,
	}
	if !proto.Equal(req, want) {
		t.Errorf("request:\n got %v\nwant %v", req, want)
	}
	cancel := runArgv(t, "force-start-grp",
		"--sp", "sp0", "--grp", "0x11", "--leg", "5", "--cancel",
	).(*pb.ForceStartGroupRequest)
	if !cancel.GetCancel() || cancel.GetLegId() != 5 {
		t.Errorf("--cancel --leg 5 built %v, want cancel with leg 5", cancel)
	}
	if cancel.SpRev == nil || cancel.GetSpRev().GetRevision() != 0 {
		t.Errorf("no --rev sent sp_rev %v, want a present zero token",
			cancel.SpRev)
	}
	overridden := runArgv(t, "force-start-grp", "--sp", "sp0",
		"--grp", "0x11", "--leg", "5", "--override", "older-leg, partial,0,9",
	).(*pb.ForceStartGroupRequest)
	if want := []pb.ForcedStartOverride{
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_OLDER_LEG,
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_PARTIAL,
		0, 9,
	}; !reflect.DeepEqual(overridden.GetOverrideList(), want) {
		t.Errorf("--override sent %v, want %v",
			overridden.GetOverrideList(), want)
	}
	dry := runArgv(t, "inspect-grp", "--sp", "sp0", "--grp", "0x11",
		"--leg", "5", "--override", "bad-blocks",
	).(*pb.InspectGroupRequest)
	if got := dry.GetOverrideList(); len(got) != 1 || got[0] !=
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_BAD_BLOCKS {
		t.Errorf("inspect-grp --override sent %v, want bad-blocks", got)
	}
	var list overrideList
	if err := list.Set("older-leg,everything"); err == nil {
		t.Errorf("--override took an unknown word: %v", list)
	}
}

// TestKeepRetiredLegRequest pins the request keep-retired-leg builds: the
// leg, keep true for the bare command and false with --clear, and a token
// message that is present, a zero one when --rev is not given.
func TestKeepRetiredLegRequest(t *testing.T) {
	req := runArgv(t, "keep-retired-leg",
		"--sp", "sp0", "--rev", "7", "--leg", "0x12",
	).(*pb.UpdateRetiredLegKeepRequest)
	want := &pb.UpdateRetiredLegKeepRequest{
		ClusterName: defaultCluster,
		SpName:      "sp0",
		SpRev:       &pb.SpRev{Revision: 7},
		LegId:       0x12,
		Keep:        true,
	}
	if !proto.Equal(req, want) {
		t.Errorf("request:\n got %v\nwant %v", req, want)
	}
	clear := runArgv(t, "keep-retired-leg",
		"--sp", "sp0", "--leg", "0x12", "--clear",
	).(*pb.UpdateRetiredLegKeepRequest)
	if clear.GetKeep() {
		t.Errorf("--clear built %v, want keep false", clear)
	}
	if clear.SpRev == nil || clear.GetSpRev().GetRevision() != 0 {
		t.Errorf("no --rev sent sp_rev %v, want a present zero token",
			clear.SpRev)
	}
}

// TestSetGrpOperatorInChargeRequest pins the request
// set-grp-operator-in-charge builds: the group, the flag, true when the bare
// flag or no flag is given and false only in the equals form, and a token
// message that is present, a zero one when --rev is not given.
func TestSetGrpOperatorInChargeRequest(t *testing.T) {
	req := runArgv(t, "set-grp-operator-in-charge",
		"--sp", "sp0", "--rev", "7", "--grp", "0x11",
	).(*pb.UpdateGroupOperatorInChargeRequest)
	want := &pb.UpdateGroupOperatorInChargeRequest{
		ClusterName:      defaultCluster,
		SpName:           "sp0",
		SpRev:            &pb.SpRev{Revision: 7},
		GrpId:            0x11,
		OperatorInCharge: true,
	}
	if !proto.Equal(req, want) {
		t.Errorf("request:\n got %v\nwant %v", req, want)
	}
	set := runArgv(t, "set-grp-operator-in-charge",
		"--sp", "sp0", "--grp", "0x11", "--operator-in-charge",
	).(*pb.UpdateGroupOperatorInChargeRequest)
	if !set.GetOperatorInCharge() {
		t.Errorf("--operator-in-charge built %v, want the flag set", set)
	}
	if set.SpRev == nil || set.GetSpRev().GetRevision() != 0 {
		t.Errorf("no --rev sent sp_rev %v, want a present zero token",
			set.SpRev)
	}
	off := runArgv(t, "set-grp-operator-in-charge",
		"--sp", "sp0", "--grp", "0x11", "--operator-in-charge=false",
	).(*pb.UpdateGroupOperatorInChargeRequest)
	if off.GetOperatorInCharge() {
		t.Errorf("--operator-in-charge=false built %v, want the flag clear",
			off)
	}
}

// TestCreateClusterRequest pins the request create-cluster builds: a bare
// one carries no conf at all, the pure-defaults request every cluster of the
// suite is made with, and --extent-size and --block-size each send only
// their own member.
func TestCreateClusterRequest(t *testing.T) {
	bare := runArgv(t, "create-cluster").(*pb.CreateClusterRequest)
	if !proto.Equal(bare, &pb.CreateClusterRequest{
		ClusterName: defaultCluster,
	}) {
		t.Errorf("bare create-cluster built %v, want the name alone", bare)
	}
	req := runArgv(t, "create-cluster",
		"--extent-size", "100000000", "--block-size", "8388608",
	).(*pb.CreateClusterRequest)
	want := &pb.CreateClusterRequest{
		ClusterName: defaultCluster,
		DnBinConf:   &pb.DnBinConf{ExtentSize: 100000000},
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{DataBlockSize: 8388608},
		},
	}
	if !proto.Equal(req, want) {
		t.Errorf("request:\n got %v\nwant %v", req, want)
	}
}

// TestCreateSpRequest pins the test-time shape the whole suite creates its SPs
// with, including the --raid1 lever that selects RedundMdRaid1 — a RedundNone
// SP would have one leg per group and every leg assertion of the smoke case
// would be wrong.
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

// TestSpRevTokenIsAlwaysPresent pins GW6 as the driver sees it: an omitted
// --rev sends the ZERO token, not an absent message, and a PRESENT zero can
// never match a stored revision that starts at 1. GW6 is presence-based, so
// this is what keeps case B step 4 a refusal — an absent
// message would be waved through — so this test guards the whole B4 stage,
// not just a marshalling detail.
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
// request as raw bytes, so what the suite appends is what etcd stores. The two
// index flags are pinned with it, and with DIFFERENT values, because a chunk is
// addressed by the (src_slice_idx, bm_idx) pair and a driver that fed one flag
// to both fields would append to the wrong chunk.
func TestBitmapIsVerbatim(t *testing.T) {
	req := runArgv(t, "append-clone-bm",
		"--sp", "sp0", "--rev", "3", "--name", "cl0",
		"--src-slice-idx", "2", "--bm-idx", "1", "--bm-hex", "ff00ff",
	).(*pb.AppendCloneBitmapRequest)
	if !reflect.DeepEqual(req.GetBitmap(), []byte{0xff, 0x00, 0xff}) {
		t.Errorf("bitmap = %v, want [255 0 255]", req.GetBitmap())
	}
	if req.GetSrcSliceIdx() != 2 {
		t.Errorf("src_slice_idx = %d, want 2", req.GetSrcSliceIdx())
	}
	if req.GetBmIdx() != 1 {
		t.Errorf("bm_idx = %d, want 1", req.GetBmIdx())
	}
}

// TestBitmapPositionIsSentOnlyWhenGiven pins the two position flags of the
// append drivers: absent unless given, and a given 0 is a present 0, so the
// suite can test both the gateway's blind append and its placed one.
func TestBitmapPositionIsSentOnlyWhenGiven(t *testing.T) {
	blind := runArgv(t, "append-clone-bm",
		"--sp", "sp0", "--rev", "3", "--name", "cl0", "--bm-hex", "ff",
	).(*pb.AppendCloneBitmapRequest)
	if blind.Offset != nil {
		t.Errorf("no --offset: offset = %d, want absent", blind.GetOffset())
	}
	placed := runArgv(t, "append-clone-bm",
		"--sp", "sp0", "--rev", "3", "--name", "cl0", "--bm-hex", "ff",
		"--offset", "0",
	).(*pb.AppendCloneBitmapRequest)
	if placed.Offset == nil || placed.GetOffset() != 0 {
		t.Errorf("--offset 0: offset = %v, want a present 0", placed.Offset)
	}
}

// TestBoolFlagsNeedTheEqualsForm pins the spelling of a false bool: Go's flag
// package never consumes the next argument for a bool, so `--disabled false`
// would leave `false` as a positional and read the flag TRUE; parseArgs
// refuses that word, so only `--disabled=false` clears the flag.
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

// TestStrayWordIsRefused pins parseArgs, the one parse of every subcommand's
// flags: a word left after them is refused, naming the word — a separate
// `false` after a bool flag, a default-false flag's `--force false`, and a
// word before more flags, whose flags would go unread — while the bare and
// the equals forms of a bool parse.
func TestStrayWordIsRefused(t *testing.T) {
	parse := func(name string, argv ...string) error {
		t.Helper()
		cmd, ok := lookup(name)
		if !ok {
			t.Fatalf("no subcommand %q", name)
		}
		g := newGlobals()
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		g.bind(fs)
		cmd.setup(fs)
		return parseArgs(fs, argv)
	}
	for _, tc := range []struct {
		name string
		argv []string
		word string
	}{
		{"set-dn-disabled", []string{"--addr", "a:1", "--rev", "1",
			"--disabled", "false"}, "false"},
		{"set-cntlr-enabled", []string{"--sp", "sp0", "--rev", "1",
			"--id", "1", "--enabled", "false"}, "false"},
		{"delete-dn", []string{"--addr", "a:1", "--rev", "1", "--force",
			"false"}, "false"},
		{"set-ns-suspended", []string{"--sp", "sp0", "--suspended", "false",
			"--nqn", "n", "--idx", "1"}, "false"},
	} {
		err := parse(tc.name, tc.argv...)
		if err == nil ||
			!strings.Contains(err.Error(), fmt.Sprintf("%q", tc.word)) {
			t.Errorf("%s %v: %v, want the word %q refused", tc.name,
				tc.argv, err, tc.word)
		}
	}
	for _, argv := range [][]string{
		{"--addr", "a:1", "--rev", "1", "--disabled=false"},
		{"--addr", "a:1", "--rev", "1", "--disabled"},
		{"--addr", "a:1", "--rev", "1"},
	} {
		if err := parse("set-dn-disabled", argv...); err != nil {
			t.Errorf("set-dn-disabled %v: %v, want it parsed", argv, err)
		}
	}
}

// parseCallsOutsideParseArgs reads the driver's code in dir and returns where
// a call of a method named Parse sits outside parseArgs, but main's parse of
// the global flags (top.Parse), and how many such calls parseArgs holds. A
// call on an imported package's name, such as time.Parse, is no flag set's
// and is skipped; a call inside a function literal of a top-level table
// counts as outside.
func parseCallsOutsideParseArgs(dir string) ([]string, int, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, 0, err
	}
	var outside []string
	inside := 0
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return nil, 0, err
		}
		pkgs := map[string]bool{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			pkgName := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
			pkgs[pkgName] = true
		}
		for _, decl := range f.Decls {
			fn := "a top-level declaration"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Parse" {
					return true
				}
				recv, _ := sel.X.(*ast.Ident)
				switch {
				case recv != nil && pkgs[recv.Name]:
				case fn == "parseArgs":
					inside++
				case fn == "main" && recv != nil && recv.Name == "top":
				default:
					outside = append(outside, fmt.Sprintf("%s in %s",
						fset.Position(call.Pos()), fn))
				}
				return true
			})
		}
	}
	return outside, inside, nil
}

// TestEveryParseGoesThroughParseArgs guards the refusal's reach: every call
// of a Parse method in the driver's code sits inside parseArgs, which holds
// exactly one, but main's parse of the global flags, so a subcommand added
// later cannot parse its flags past the refusal.
func TestEveryParseGoesThroughParseArgs(t *testing.T) {
	outside, inside, err := parseCallsOutsideParseArgs(".")
	if err != nil {
		t.Fatalf("reading the driver's code: %v", err)
	}
	for _, at := range outside {
		t.Errorf("a parse outside parseArgs: %s", at)
	}
	if inside != 1 {
		t.Errorf("parseArgs holds %d parse calls, want one", inside)
	}
}

// TestTheParseGuardSeesASkip: the guard reports a parse that skips parseArgs
// in each shape a subcommand could give it — a subcommand's own fs.Parse, a
// flag set of another name, and one inside a function literal of a table —
// and passes main's top.Parse and a call on an imported package's name.
func TestTheParseGuardSeesASkip(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import (
	"flag"
	"time"
)

func parseArgs(fs *flag.FlagSet, args []string) error { return fs.Parse(args) }

func runOne(fs *flag.FlagSet, args []string) { _ = fs.Parse(args) }

func runRace(jobFs *flag.FlagSet, argv []string) { _ = jobFs.Parse(argv) }

var table = []func(*flag.FlagSet, []string){
	func(set *flag.FlagSet, args []string) { _ = set.Parse(args) },
}

func when() { _, _ = time.Parse(time.RFC3339, "") }

func main() {
	top := flag.NewFlagSet("x", flag.ExitOnError)
	_ = top.Parse(nil)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src),
		0o644); err != nil {
		t.Fatalf("writing the sample: %v", err)
	}
	outside, inside, err := parseCallsOutsideParseArgs(dir)
	if err != nil {
		t.Fatalf("reading the sample: %v", err)
	}
	if inside != 1 {
		t.Errorf("the sample's parseArgs holds %d parse calls, want one",
			inside)
	}
	want := []string{" in runOne", " in runRace",
		" in a top-level declaration"}
	if len(outside) != len(want) {
		t.Fatalf("the guard reports %v, want %d parses outside parseArgs",
			outside, len(want))
	}
	for i, w := range want {
		if !strings.HasSuffix(outside[i], w) {
			t.Errorf("report %d is %q, want one ending %q", i, outside[i], w)
		}
	}
}

// mainChildEnv names the environment variable that carries runMain's
// arguments to its child, joined by the unit separator.
const mainChildEnv = "DNV_DRIVER_TEST_MAIN_ARGV"

// runMain runs the driver's own main in a child process of the test binary,
// with argv as its arguments (TestMainChild), and returns its exit code and
// its stderr.
func runMain(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainChild$")
	cmd.Env = append(os.Environ(),
		mainChildEnv+"="+strings.Join(argv, "\x1f"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	}
	t.Fatalf("running main %v: %v", argv, err)
	return 0, ""
}

// TestMainChild is runMain's child: it runs main with the arguments its
// parent put in the environment, and skips in any other run.
func TestMainChild(t *testing.T) {
	argv, ok := os.LookupEnv(mainChildEnv)
	if !ok {
		t.Skip("only runMain's child runs main")
	}
	os.Args = append([]string{"gatewayctl"}, strings.Split(argv, "\x1f")...)
	main()
	os.Exit(0)
}

// TestMainRefusesAStrayWord runs the driver's main (runMain), so that the
// wrapper every subcommand's parse goes through is what refuses: a stray
// word exits 2, naming it, before any call leaves the driver.
func TestMainRefusesAStrayWord(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		word string
	}{
		{[]string{"--gateway", "127.0.0.1:1", "--timeout", "1", "create-sp",
			"--sp", "sp0", "--raid1", "false"}, "false"},
		{[]string{"--gateway", "127.0.0.1:1", "--timeout", "1", "get-sp",
			"--sp", "sp0", "stray"}, "stray"},
		{[]string{"--gateway", "127.0.0.1:1", "--timeout", "1", "race",
			"false"}, "false"},
	} {
		code, stderr := runMain(t, tc.argv...)
		if code != 2 ||
			!strings.Contains(stderr, fmt.Sprintf("%q", tc.word)) {
			t.Errorf("%v: exit %d, stderr %q; want exit 2 naming %q",
				tc.argv, code, stderr, tc.word)
		}
	}
	// The same call well formed passes the parse and fails at the call,
	// with no gateway to answer it: exit 1, not 2.
	if code, stderr := runMain(t, "--gateway", "127.0.0.1:1", "--timeout",
		"1", "get-sp", "--sp", "sp0"); code != 1 {
		t.Errorf("get-sp with no gateway: exit %d, stderr %q; want 1",
			code, stderr)
	}
}
