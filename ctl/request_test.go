// argv → request (dnvctl.md, Conventions, and the group sections; CT3, CT8,
// CT9).
//
// This is the heart of the unit suite: what dnvctl puts on the wire for a
// given command line. Every row drives the REAL cobra tree through
// Execute — the same path cmd/dnvctl/main.go takes — with the recording
// client behind the dialer seam, and compares the captured request against a
// whole expected proto with proto.Equal. Comparing whole messages rather than
// picking fields is deliberate: a field the row forgot to mention is still
// asserted, at its default, so a value that starts leaking into a request
// nobody listed fails here.
//
// The sweep table is the sweep case's (dnvctl.md, Integration test plan),
// argv for argv. The integration suite asserts the same 59 invocations
// against a real wire, so a row changed here and not there — or the other way
// round — shows up as a disagreement between the two suites rather than as a
// quiet gap.
package ctl

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The integration suite's fixture values (integtest/dnvctl_test.sh). Payload
// only — nothing here is ever dialed.
const (
	itDnAddr   = "127.0.0.1:29901"
	itCnAddr   = "127.0.0.1:29902"
	itNqn      = "nqn.2025-01.io.dnv:itctl:sp0:ss0"
	itHostNqn  = "nqn.2025-01.io.dnv:host0"
	itSrcNqn   = "nqn.2025-01.io.dnv:src:ss0"
	itUuid     = "6f7d0f3e-0dd6-4f22-9a34-5e0f1a2b3c4d"
	itNguid    = "00112233445566778899aabbccddeeff"
	itTdSize   = 67108864
	itRevValue = 7
)

// wantSpRev / wantDnRev / wantCnRev build the expected token message. They
// are functions rather than package vars so no row can share — and mutate —
// another row's expectation. Only `revision` is ever set: CT3 says the echo
// fields stay empty because the gateway compares nothing else.
func wantSpRev(revision uint64) *pb.SpRev { return &pb.SpRev{Revision: revision} }
func wantDnRev(revision uint64) *pb.DnRev { return &pb.DnRev{Revision: revision} }
func wantCnRev(revision uint64) *pb.CnRev { return &pb.CnRev{Revision: revision} }

// wantTrConf is trConfFlags' default transport (dnvctl.md, Conventions): the
// conf a command sends when the operator touches none of the four flags.
func wantTrConf() *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "127.0.0.1",
		TrSvcId: "4420",
	}
}

// sweepRow is one step of the sweep case (dnvctl.md, Integration test plan):
// the argv after the global prefix, the RPC it must drive, and the request it
// must produce.
type sweepRow struct {
	step int
	rpc  string
	argv []string
	want proto.Message
}

// sweepRows is the sweep case's 59-step table, in the order of
// dnvctl.md, The command tree. Together with the global prefix (--cluster
// itctl --sp sp0) it is the complete argv → request corpus: every command
// covered at least once, every field of every request asserted.
var sweepRows = []sweepRow{
	// ---- cluster ----
	{1, "CreateCluster",
		[]string{"cluster", "create", "--name", "c1",
			"--extent-size", "67108864"},
		// --name WINS over the global --cluster. --extent-size fills
		// dn_bin_conf.extent_size and NOTHING else: proto.Equal compares
		// the whole message, so the four bin shifts are asserted absent
		// here (the gateway resolves them as the 0/4/8/12 set).
		&pb.CreateClusterRequest{
			ClusterName: "c1",
			DnBinConf:   &pb.DnBinConf{ExtentSize: 67108864}}},
	{2, "DeleteCluster",
		[]string{"cluster", "delete", "--name", "c1"},
		&pb.DeleteClusterRequest{ClusterName: "c1"}},
	{3, "GetCluster",
		[]string{"cluster", "get"},
		// No --name: the group falls back to the global --cluster
		// (clusterNameOf).
		&pb.GetClusterRequest{ClusterName: itCluster}},
	{4, "ListClusters",
		[]string{"cluster", "list", "--count", "2", "--page-token", "pt0"},
		// The one request with no cluster_name field at all.
		&pb.ListClustersRequest{Count: 2, PageToken: "pt0"}},

	// ---- dn ----
	{5, "CreateDiskNode",
		[]string{"dn", "create", "--addr", itDnAddr, "--location", "rack0"},
		&pb.CreateDiskNodeRequest{
			ClusterName: itCluster,
			AddrPort:    itDnAddr,
			NvmeTrConf:  wantTrConf(),
			Location:    "rack0",
		}},
	{6, "DeleteDiskNode",
		[]string{"dn", "delete", "--addr", itDnAddr, "--rev", "7"},
		&pb.DeleteDiskNodeRequest{
			ClusterName: itCluster,
			AddrPort:    itDnAddr,
			DnRev:       wantDnRev(itRevValue),
		}},
	{7, "GetDiskNode",
		[]string{"dn", "get", "--addr", itDnAddr},
		&pb.GetDiskNodeRequest{ClusterName: itCluster, AddrPort: itDnAddr}},
	{8, "ListDiskNodes",
		[]string{"dn", "list", "--count", "8"},
		&pb.ListDiskNodesRequest{ClusterName: itCluster, Count: 8}},
	{9, "UpdateDiskNodeDisabled",
		[]string{"dn", "set-disabled", "--addr", itDnAddr,
			"--disabled", "--rev", "7"},
		&pb.UpdateDiskNodeDisabledRequest{
			ClusterName: itCluster,
			AddrPort:    itDnAddr,
			DnRev:       wantDnRev(itRevValue),
			Disabled:    true,
		}},
	{10, "InspectDiskNode",
		[]string{"dn", "inspect", "--addr", itDnAddr},
		&pb.InspectDiskNodeRequest{
			ClusterName: itCluster, AddrPort: itDnAddr}},

	// ---- cn: the six dn mirrors ----
	{11, "CreateControllerNode",
		[]string{"cn", "create", "--addr", itCnAddr, "--location", "rack1"},
		&pb.CreateControllerNodeRequest{
			ClusterName: itCluster,
			AddrPort:    itCnAddr,
			NvmeTrConf:  wantTrConf(),
			Location:    "rack1",
		}},
	{12, "DeleteControllerNode",
		[]string{"cn", "delete", "--addr", itCnAddr, "--rev", "7"},
		&pb.DeleteControllerNodeRequest{
			ClusterName: itCluster,
			AddrPort:    itCnAddr,
			CnRev:       wantCnRev(itRevValue),
		}},
	{13, "GetControllerNode",
		[]string{"cn", "get", "--addr", itCnAddr},
		&pb.GetControllerNodeRequest{
			ClusterName: itCluster, AddrPort: itCnAddr}},
	{14, "ListControllerNodes",
		[]string{"cn", "list", "--count", "8"},
		&pb.ListControllerNodesRequest{ClusterName: itCluster, Count: 8}},
	{15, "UpdateControllerNodeDisabled",
		[]string{"cn", "set-disabled", "--addr", itCnAddr,
			"--disabled", "--rev", "7"},
		&pb.UpdateControllerNodeDisabledRequest{
			ClusterName: itCluster,
			AddrPort:    itCnAddr,
			CnRev:       wantCnRev(itRevValue),
			Disabled:    true,
		}},
	{16, "InspectControllerNode",
		[]string{"cn", "inspect", "--addr", itCnAddr},
		&pb.InspectControllerNodeRequest{
			ClusterName: itCluster, AddrPort: itCnAddr}},

	// ---- sp ----
	{17, "CreateStoragePool",
		[]string{"sp", "create", "--cntlr-cnt", "2", "--slice-cnt", "1",
			"--init-ext-cnt", "2", "--slots", "0,1",
			"--low-water-mark-pct", "30"},
		// bdev_conf is ALWAYS sent with a redund_conf
		// (dnvctl.md, `sp` — `ctl/sp.go`); no --rev, which the request has no
		// token for (CT3); dm_pool_conf carries the mark alone, --block-size
		// being untyped; dm_raid0_conf and event_threshold stay absent.
		&pb.CreateStoragePoolRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			BdevConf: &pb.BdevConf{
				DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 30},
				RedundConf: &pb.RedundConf{
					RedunKind: &pb.RedundConf_RedundMdRaid1{
						RedundMdRaid1: &pb.RedundMdRaid1{},
					},
				},
			},
			CntlidSlotList: []uint32{0, 1},
			CntlrCnt:       2,
			SliceCnt:       1,
			InitExtCnt:     2,
		}},
	{18, "DeleteStoragePool",
		[]string{"sp", "delete", "--rev", "7"},
		&pb.DeleteStoragePoolRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
		}},
	{19, "GetStoragePool",
		[]string{"sp", "get"},
		&pb.GetStoragePoolRequest{ClusterName: itCluster, SpName: itSp}},
	{20, "ListStoragePools",
		[]string{"sp", "list", "--count", "4"},
		// No sp_name field: the global --sp is ignored.
		&pb.ListStoragePoolsRequest{ClusterName: itCluster, Count: 4}},
	{21, "UpdateStoragePoolCntlidSlotList",
		[]string{"sp", "set-cntlid-slots", "--slots", "0,1,2", "--rev", "7"},
		&pb.UpdateStoragePoolCntlidSlotListRequest{
			ClusterName:    itCluster,
			SpName:         itSp,
			SpRev:          wantSpRev(itRevValue),
			CntlidSlotList: []uint32{0, 1, 2},
		}},
	{22, "UpdateStoragePoolLevel",
		[]string{"sp", "set-level", "--level", "READONLY", "--rev", "7"},
		&pb.UpdateStoragePoolLevelRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			SpLevel:     pb.SpLevel_SP_LEVEL_READONLY,
		}},
	{23, "FindStoragePoolNames",
		[]string{"sp", "find-names", "--ids", "1,2"},
		&pb.FindStoragePoolNamesRequest{
			ClusterName: itCluster,
			SpIdList:    []uint64{1, 2},
		}},
	{24, "GrowSlice",
		[]string{"sp", "grow-slice", "--slice", "0", "--ext", "2",
			"--dn-white", itDnAddr, "--rev", "7"},
		&pb.GrowSliceRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			ExtCnt:      2,
			DnSelector:  &pb.NodeSelector{WhiteList: []string{itDnAddr}},
		}},
	{25, "InspectSide",
		[]string{"sp", "inspect-side", "--id", "5"},
		&pb.InspectSideRequest{
			ClusterName: itCluster, SpName: itSp, SideId: 5}},

	// ---- cntlr ----
	{26, "CreateCntlr",
		[]string{"cntlr", "create", "--slot", "1", "--rev", "7"},
		&pb.CreateCntlrRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			CntlidSlot:  1,
		}},
	{27, "DeleteCntlr",
		[]string{"cntlr", "delete", "--id", "3", "--rev", "7"},
		&pb.DeleteCntlrRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			CntlrId:     3,
		}},
	{28, "UpdateCntlrEnabled",
		[]string{"cntlr", "set-enabled", "--id", "3", "--enabled=false",
			"--rev", "7"},
		// The `=` spelling is the only one that turns a bool off (dnvctl.md,
		// Conventions).
		&pb.UpdateCntlrEnabledRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			CntlrId:     3,
			Enabled:     false,
		}},
	{29, "InspectCntlr",
		[]string{"cntlr", "inspect", "--id", "3"},
		&pb.InspectCntlrRequest{
			ClusterName: itCluster, SpName: itSp, CntlrId: 3}},

	// ---- td ----
	{30, "CreateThinDevice",
		[]string{"td", "create", "--name", "t0", "--size", "67108864",
			"--rev", "7"},
		&pb.CreateThinDeviceRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			TdName:      "t0",
			Size:        itTdSize,
		}},
	{31, "DeleteThinDevice",
		[]string{"td", "delete", "--name", "t0", "--rev", "7"},
		&pb.DeleteThinDeviceRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			TdName:      "t0",
		}},
	{32, "ListThinDevices",
		[]string{"td", "list"},
		&pb.ListThinDevicesRequest{ClusterName: itCluster, SpName: itSp}},
	{33, "GetThinDeviceBitmap",
		[]string{"td", "get-bm", "--name", "t0", "--slice-idx", "0",
			"--start", "0", "--cnt", "64"},
		&pb.GetThinDeviceBitmapRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			TdName:      "t0",
			BlockCnt:    64,
		}},
	{34, "GetLegBitmap",
		[]string{"td", "get-leg-bm", "--leg", "9", "--start", "0",
			"--cnt", "64"},
		&pb.GetLegBitmapRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			LegId:       9,
			BlockCnt:    64,
		}},

	// ---- ss ----
	{35, "CreateSubsystem",
		[]string{"ss", "create", "--nqn", itNqn, "--hosts", itHostNqn,
			"--rev", "7"},
		&pb.CreateSubsystemRequest{
			ClusterName:  itCluster,
			SpName:       itSp,
			SpRev:        wantSpRev(itRevValue),
			Nqn:          itNqn,
			AllowedHosts: []string{itHostNqn},
		}},
	{36, "DeleteSubsystem",
		[]string{"ss", "delete", "--nqn", itNqn, "--rev", "7"},
		&pb.DeleteSubsystemRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			Nqn:         itNqn,
		}},
	{37, "ListSubsystems",
		[]string{"ss", "list"},
		&pb.ListSubsystemsRequest{ClusterName: itCluster, SpName: itSp}},
	{38, "UpdateSubsystemHosts",
		[]string{"ss", "set-hosts", "--nqn", itNqn, "--hosts", "a,b",
			"--rev", "7"},
		&pb.UpdateSubsystemHostsRequest{
			ClusterName:  itCluster,
			SpName:       itSp,
			SpRev:        wantSpRev(itRevValue),
			Nqn:          itNqn,
			AllowedHosts: []string{"a", "b"},
		}},

	// ---- ns ----
	{39, "CreateNamespace",
		[]string{"ns", "create", "--nqn", itNqn, "--idx", "1", "--td", "t0",
			"--uuid", itUuid, "--nguid", itNguid, "--rev", "7"},
		&pb.CreateNamespaceRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			Nqn:         itNqn,
			NsIdx:       1,
			DevUuid:     itUuid,
			DevNguid:    itNguid,
			TdName:      "t0",
		}},
	{40, "DeleteNamespace",
		[]string{"ns", "delete", "--nqn", itNqn, "--idx", "1", "--rev", "7"},
		&pb.DeleteNamespaceRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			Nqn:         itNqn,
			NsIdx:       1,
		}},
	{41, "UpdateNamespaceDev",
		[]string{"ns", "set-dev", "--nqn", itNqn, "--idx", "1", "--td", "t1",
			"--rev", "7"},
		&pb.UpdateNamespaceDevRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			Nqn:         itNqn,
			NsIdx:       1,
			TdName:      "t1",
		}},
	{42, "UpdateNamespaceSuspended",
		[]string{"ns", "set-suspended", "--nqn", itNqn, "--idx", "1",
			"--rev", "7"},
		// --suspended defaults to TRUE on this command alone
		// (dnvctl.md, `ns` — `ctl/ns.go`).
		&pb.UpdateNamespaceSuspendedRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			Nqn:         itNqn,
			NsIdx:       1,
			Suspended:   true,
		}},

	// ---- clone ----
	{43, "CreateClone",
		[]string{"clone", "create", "--name", "cl0", "--dst-td", "t1",
			"--src-nqn", itSrcNqn, "--src-idx", "0", "--src-slices", "1",
			"--src-stripe", "16384", "--src-block", "1048576", "--rev", "7"},
		&pb.CreateCloneRequest{
			ClusterName:   itCluster,
			SpName:        itSp,
			SpRev:         wantSpRev(itRevValue),
			CloneName:     "cl0",
			SrcTrConf:     []*pb.NvmeTrConf{wantTrConf()},
			SrcNqn:        itSrcNqn,
			SrcSliceCnt:   1,
			SrcStripeSize: 16384,
			SrcBlockSize:  1048576,
			DstTdName:     "t1",
		}},
	{44, "DeleteClone",
		[]string{"clone", "delete", "--name", "cl0", "--force", "--rev", "7"},
		&pb.DeleteCloneRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			CloneName:   "cl0",
			Force:       true,
		}},
	{45, "GetClone",
		[]string{"clone", "get", "--name", "cl0"},
		&pb.GetCloneRequest{
			ClusterName: itCluster, SpName: itSp, CloneName: "cl0"}},
	{46, "UpdateCloneTrConf",
		[]string{"clone", "set-tr", "--name", "cl0",
			"--src-tr-addr", "127.0.0.1", "--rev", "7"},
		&pb.UpdateCloneTrConfRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			CloneName:   "cl0",
			SrcTrConf:   []*pb.NvmeTrConf{wantTrConf()},
		}},
	// The two indexes are given DISTINCT non-zero values: a chunk is
	// addressed by the pair, and equal values would let a command that wired
	// --bm-idx to src_slice_idx (or the reverse) pass.
	{47, "AppendCloneBitmap",
		[]string{"clone", "append-bm", "--name", "cl0",
			"--src-slice-idx", "1", "--bm-idx", "2",
			"--bm-hex", "a5", "--rev", "7"},
		&pb.AppendCloneBitmapRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			CloneName:   "cl0",
			SrcSliceIdx: 1,
			BmIdx:       2,
			Bitmap:      []byte{0xa5},
		}},

	// ---- xfer ----
	{48, "CreateTransfer",
		[]string{"xfer", "create", "--name", "x0", "--ori-nqn", itNqn,
			"--ori-idx", "1", "--hosts", itHostNqn, "--auto-suspend",
			"--rev", "7"},
		&pb.CreateTransferRequest{
			ClusterName:  itCluster,
			SpName:       itSp,
			SpRev:        wantSpRev(itRevValue),
			XferName:     "x0",
			OriNqn:       itNqn,
			OriNsIdx:     1,
			AllowedHosts: []string{itHostNqn},
			AutoSuspend:  true,
		}},
	{49, "DeleteTransfer",
		[]string{"xfer", "delete", "--name", "x0", "--rev", "7"},
		// No --force: the abort path is not taken.
		&pb.DeleteTransferRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			XferName:    "x0",
		}},
	{50, "GetTransfer",
		[]string{"xfer", "get", "--name", "x0"},
		&pb.GetTransferRequest{
			ClusterName: itCluster, SpName: itSp, XferName: "x0"}},
	{51, "UpdateTransferHosts",
		[]string{"xfer", "set-hosts", "--name", "x0", "--hosts", "c",
			"--rev", "7"},
		&pb.UpdateTransferHostsRequest{
			ClusterName:  itCluster,
			SpName:       itSp,
			SpRev:        wantSpRev(itRevValue),
			XferName:     "x0",
			AllowedHosts: []string{"c"},
		}},

	// ---- migr ----
	{52, "CreateMigration",
		[]string{"migr", "create", "--name", "m0", "--src-side", "5",
			"--hyd-threshold", "8", "--hyd-batch", "4", "--rev", "7"},
		&pb.CreateMigrationRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			MigrName:    "m0",
			SrcSideId:   5,
			DmCloneConf: &pb.DmCloneConf{
				HydrationThreshold: 8,
				HydrationBatchSize: 4,
			},
		}},
	{53, "FinishMigration",
		[]string{"migr", "finish", "--name", "m0", "--rev", "7"},
		&pb.FinishMigrationRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			MigrName:    "m0",
		}},
	{54, "CancelMigration",
		[]string{"migr", "cancel", "--name", "m0", "--rev", "7"},
		&pb.CancelMigrationRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			MigrName:    "m0",
		}},
	{55, "GetMigration",
		[]string{"migr", "get", "--name", "m0"},
		&pb.GetMigrationRequest{
			ClusterName: itCluster, SpName: itSp, MigrName: "m0"}},
	{56, "AppendMigrationBitmap",
		[]string{"migr", "append-bm", "--name", "m0", "--bm-hex", "a5a5",
			"--rev", "7"},
		&pb.AppendMigrationBitmapRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			MigrName:    "m0",
			Bitmap:      []byte{0xa5, 0xa5},
		}},

	// ---- spare ----
	{57, "CreateSpareLeg",
		[]string{"spare", "create", "--grp", "1", "--rev", "7"},
		&pb.CreateSpareLegRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			GrpId:       1,
		}},
	{58, "DeleteSpareLeg",
		[]string{"spare", "delete", "--grp", "1", "--leg", "2", "--rev", "7"},
		&pb.DeleteSpareLegRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			GrpId:       1,
			LegId:       2,
		}},
	{59, "SwitchSpareLeg",
		[]string{"spare", "switch", "--grp", "1", "--spare", "6",
			"--target", "4", "--rev", "7"},
		&pb.SwitchSpareLegRequest{
			ClusterName: itCluster,
			SpName:      itSp,
			SpRev:       wantSpRev(itRevValue),
			GrpId:       1,
			SpareLegId:  6,
			TargetLegId: 4,
		}},
}

// TestSweepArgvToRequest runs the sweep case's 59 steps against the recording
// client.
func TestSweepArgvToRequest(t *testing.T) {
	for _, row := range sweepRows {
		t.Run(rpcToCmd[row.rpc], func(t *testing.T) {
			got := runArgv(t, row.rpc, row.argv...)
			wantRequest(t, got, row.want)
		})
	}
}

// TestSweepCoversEveryCommand keeps the table honest: 59 rows, one per RPC,
// no duplicates. Without it a row could be deleted and the sweep would still
// be green over the 58 that remain.
func TestSweepCoversEveryCommand(t *testing.T) {
	if len(sweepRows) != 59 {
		t.Errorf("sweepRows has %d rows, want 59", len(sweepRows))
	}
	seen := map[string]bool{}
	for _, row := range sweepRows {
		if _, ok := rpcToCmd[row.rpc]; !ok {
			t.Errorf("step %d names %s, which is not an RPC",
				row.step, row.rpc)
		}
		if seen[row.rpc] {
			t.Errorf("step %d repeats %s", row.step, row.rpc)
		}
		seen[row.rpc] = true
		if row.step != len(seen) {
			t.Errorf("step %d is out of order (it is entry %d)",
				row.step, len(seen))
		}
	}
	for rpc := range rpcToCmd {
		if !seen[rpc] {
			t.Errorf("the sweep never drives %s", rpc)
		}
	}
}

// TestGlobalsFillEveryRequestThatHasThem pins the two counts behind
// dnvctl.md, Global flags, env, config — cluster_name in 58 of the 59
// requests, sp_name in 41 — by reading the field off every request type
// rather than by trusting the table. A command that stopped filling a field
// from its global flag would show up as a mismatch in the sweep; a command
// whose REQUEST stopped carrying the field shows up here.
func TestGlobalsFillEveryRequestThatHasThem(t *testing.T) {
	clusterFields, spFields := 0, 0
	for _, row := range sweepRows {
		fields := row.want.ProtoReflect().Descriptor().Fields()
		if hasStringField(fields, "cluster_name") {
			clusterFields++
		}
		if hasStringField(fields, "sp_name") {
			spFields++
			if got := stringField(row.want, "sp_name"); got != itSp {
				t.Errorf("step %d (%s) expects sp_name %q, want the "+
					"global --sp %q", row.step, row.rpc, got, itSp)
			}
		}
	}
	if clusterFields != 58 {
		t.Errorf("%d requests carry cluster_name, want 58", clusterFields)
	}
	if spFields != 41 {
		t.Errorf("%d requests carry sp_name, want 41", spFields)
	}
}

func hasStringField(fields protoreflect.FieldDescriptors, name string) bool {
	field := fields.ByName(protoreflect.Name(name))
	return field != nil && field.Kind() == protoreflect.StringKind
}

func stringField(msg proto.Message, name string) string {
	reflected := msg.ProtoReflect()
	field := reflected.Descriptor().Fields().ByName(protoreflect.Name(name))
	if field == nil {
		return ""
	}
	return reflected.Get(field).String()
}

// ---------------------------------------------------------------------------
// The CT3 token trio
// ---------------------------------------------------------------------------

// TestTokenPresenceTrio is the CT3 rule, on one command of each token family.
// The three cases are genuinely different wire content, not three spellings
// of one: absent means the gateway skips its GW6 check entirely, present-zero
// is the deliberate always-stale probe, and present-N is the ordinary
// optimistic-concurrency gate.
func TestTokenPresenceTrio(t *testing.T) {
	t.Run("SpRev", func(t *testing.T) {
		base := []string{"td", "create", "--name", "t0"}

		absent := runArgv(t, "CreateThinDevice", base...).(*pb.CreateThinDeviceRequest)
		if absent.SpRev != nil {
			t.Errorf("no --rev sent sp_rev %v, want a nil message",
				absent.SpRev)
		}

		zero := runArgv(t, "CreateThinDevice",
			append(base, "--rev", "0")...).(*pb.CreateThinDeviceRequest)
		if zero.SpRev == nil {
			t.Fatalf("--rev 0 sent no sp_rev, want a present message")
		}
		if zero.SpRev.Revision != 0 {
			t.Errorf("--rev 0 sent revision %d, want 0", zero.SpRev.Revision)
		}
		if zero.SpRev.SpName != "" {
			t.Errorf("--rev 0 echoed sp_name %q, want it unset",
				zero.SpRev.SpName)
		}

		hex := runArgv(t, "CreateThinDevice",
			append(base, "--rev", "0x1f")...).(*pb.CreateThinDeviceRequest)
		if hex.GetSpRev().GetRevision() != 31 {
			t.Errorf("--rev 0x1f sent revision %d, want 31",
				hex.GetSpRev().GetRevision())
		}
	})

	t.Run("DnRev", func(t *testing.T) {
		base := []string{"dn", "delete", "--addr", itDnAddr}

		absent := runArgv(t, "DeleteDiskNode", base...).(*pb.DeleteDiskNodeRequest)
		if absent.DnRev != nil {
			t.Errorf("no --rev sent dn_rev %v, want a nil message",
				absent.DnRev)
		}

		zero := runArgv(t, "DeleteDiskNode",
			append(base, "--rev", "0")...).(*pb.DeleteDiskNodeRequest)
		if zero.DnRev == nil || zero.DnRev.Revision != 0 {
			t.Errorf("--rev 0 sent dn_rev %v, want a present zero", zero.DnRev)
		}
		if zero.GetDnRev().GetAddrPort() != "" {
			t.Errorf("--rev 0 echoed addr_port %q, want it unset",
				zero.GetDnRev().GetAddrPort())
		}

		hex := runArgv(t, "DeleteDiskNode",
			append(base, "--rev", "0x1f")...).(*pb.DeleteDiskNodeRequest)
		if hex.GetDnRev().GetRevision() != 31 {
			t.Errorf("--rev 0x1f sent revision %d, want 31",
				hex.GetDnRev().GetRevision())
		}
	})

	t.Run("CnRev", func(t *testing.T) {
		base := []string{"cn", "set-disabled", "--addr", itCnAddr}

		absent := runArgv(t, "UpdateControllerNodeDisabled", base...).(*pb.UpdateControllerNodeDisabledRequest)
		if absent.CnRev != nil {
			t.Errorf("no --rev sent cn_rev %v, want a nil message",
				absent.CnRev)
		}

		zero := runArgv(t, "UpdateControllerNodeDisabled",
			append(base, "--rev", "0")...).(*pb.UpdateControllerNodeDisabledRequest)
		if zero.CnRev == nil || zero.CnRev.Revision != 0 {
			t.Errorf("--rev 0 sent cn_rev %v, want a present zero", zero.CnRev)
		}

		hex := runArgv(t, "UpdateControllerNodeDisabled",
			append(base, "--rev", "0x1f")...).(*pb.UpdateControllerNodeDisabledRequest)
		if hex.GetCnRev().GetRevision() != 31 {
			t.Errorf("--rev 0x1f sent revision %d, want 31",
				hex.GetCnRev().GetRevision())
		}
	})
}

// TestTokenCarriersMatchCT3 pins the other half of CT3: WHICH commands
// carry a token. It drives each of the 34 sweep rows whose request has a
// token field twice — once with the row's --rev 7 and once without — and
// asserts the token is sent exactly when --rev is typed; the count pins the
// 34. A token quietly added to a read, or dropped from a mutator, is
// invisible to the sweep table (which fixes both argv and expectation
// together) and shows up here. The other 25 refuse --rev outright
// (TestRevOnATokenlessCommandIsAUsageError).
func TestTokenCarriersMatchCT3(t *testing.T) {
	carriers := 0
	for _, row := range sweepRows {
		tokenField := revTokenField(row.want.ProtoReflect().Descriptor())
		if tokenField == nil {
			continue
		}
		carriers++
		t.Run(rpcToCmd[row.rpc], func(t *testing.T) {
			// The sweep argv already carries --rev 7 on every token
			// carrier; if a row does not, the spec and the table disagree.
			got := runArgv(t, row.rpc, row.argv...)
			if !got.ProtoReflect().Has(tokenField) {
				t.Errorf("--rev 7 sent no %s", tokenField.Name())
			}
			bare := stripRev(row.argv)
			got = runArgv(t, row.rpc, bare...)
			if got.ProtoReflect().Has(tokenField) {
				t.Errorf("a bare invocation sent %s anyway",
					tokenField.Name())
			}
		})
	}
	if carriers != 34 {
		t.Errorf("%d requests carry a revision token, want 34", carriers)
	}
}

// TestRevOnATokenlessCommandIsAUsageError is the complement of the test
// above: on the 25 commands whose request has no token field, a typed --rev
// is a usage error — exit 2, CT5's refusal line on stderr word for word, no
// RPC issued. It used to be dropped unread, so `cluster delete --rev 7`
// deleted with no gate while looking gated, and even `--rev zz`, which every
// carrier refuses, exited 0; each row is driven with a parsable, an
// unparsable and an empty value. The empty one is `--rev "$REV"` with REV
// unset: a refusal keyed on the value rather than on the flag being typed
// would let it through, and the request would go out ungated.
func TestRevOnATokenlessCommandIsAUsageError(t *testing.T) {
	// A carrier's --rev first: the token read by one invocation must not
	// stand in for the next one's in the same process.
	runArgv(t, "CreateThinDevice", "td", "create", "--name", "t0",
		"--rev", "7")
	tokenless := 0
	for _, row := range sweepRows {
		if revTokenField(row.want.ProtoReflect().Descriptor()) != nil {
			continue
		}
		tokenless++
		t.Run(rpcToCmd[row.rpc], func(t *testing.T) {
			for _, rev := range []string{"7", "zz", ""} {
				client := &recordingClient{}
				res := runCLI(t, client, globalArgv(
					append(stripRev(row.argv), "--rev", rev)...)...)
				if res.code != 2 {
					t.Errorf("--rev %q: exit code = %d, want 2 (stderr %q)",
						rev, res.code, res.stderr)
				}
				if client.calls != 0 {
					t.Errorf("--rev %q: %d RPCs were issued, want 0 — a "+
						"usage error must never reach the gateway",
						rev, client.calls)
				}
				if res.stdout != "" {
					t.Errorf("--rev %q: stdout = %q, want empty",
						rev, res.stdout)
				}
				want := "dnvctl: invalid --rev: " +
					strconv.Quote("dnvctl "+rpcToCmd[row.rpc]) +
					" takes no revision token\n"
				if res.stderr != want {
					t.Errorf("--rev %q: stderr %q, want %q",
						rev, res.stderr, want)
				}
			}
		})
	}
	if tokenless != 25 {
		t.Errorf("%d requests carry no revision token, want 25", tokenless)
	}
}

// revTokenField is a request's CT3 token field — sp_rev, dn_rev or cn_rev —
// or nil when the request carries none.
func revTokenField(
	desc protoreflect.MessageDescriptor,
) protoreflect.FieldDescriptor {
	for _, name := range []protoreflect.Name{"sp_rev", "dn_rev", "cn_rev"} {
		if field := desc.Fields().ByName(name); field != nil {
			return field
		}
	}
	return nil
}

// stripRev drops a `--rev <value>` pair from an argv.
func stripRev(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--rev" {
			i++
			continue
		}
		out = append(out, argv[i])
	}
	return out
}

// ---------------------------------------------------------------------------
// The nil rules of the shared flag helpers (dnvctl.md, Conventions)
// ---------------------------------------------------------------------------

// TestTrConfNilRule covers both halves of trConfOf's contract: the four flags
// left alone produce the default conf, and all four emptied produce a NIL
// conf — which is how an operator reaches the gateway's "nvme_tr_conf must
// not be empty" refusal (CT8).
func TestTrConfNilRule(t *testing.T) {
	empty := []string{
		"--tr-type=", "--adr-fam=", "--tr-addr=", "--tr-svc-id=",
	}
	req := runArgv(t, "CreateDiskNode", append(
		[]string{"dn", "create", "--addr", itDnAddr}, empty...)...).(*pb.CreateDiskNodeRequest)
	if req.NvmeTrConf != nil {
		t.Errorf("four empty transport flags sent %v, want a nil conf",
			req.NvmeTrConf)
	}

	// Emptying only SOME of them still sends a conf, with the blank carried
	// through as typed: dnvctl substitutes nothing (CT8).
	req = runArgv(t, "CreateDiskNode",
		"dn", "create", "--addr", itDnAddr, "--tr-addr=").(*pb.CreateDiskNodeRequest)
	wantRequest(t, req.NvmeTrConf, &pb.NvmeTrConf{
		TrType: "tcp", AdrFam: "ipv4", TrAddr: "", TrSvcId: "4420"})
}

// TestTrConfFlagsMapOneToOne gives all four transport flags DISTINCT
// non-default values, under both prefixes trConfFlags is used with. Two
// mistakes hide behind the defaults and only show up here: a pair of fields
// swapped in trConfOf, and a command reading the wrong PREFIX — `clone set-tr`
// reaching for the unprefixed flags, say, which would still produce a
// plausible conf on any command line that happens to use the defaults.
func TestTrConfFlagsMapOneToOne(t *testing.T) {
	custom := &pb.NvmeTrConf{
		TrType:  "rdma",
		AdrFam:  "ipv6",
		TrAddr:  "fd00::1",
		TrSvcId: "4421",
	}

	plain := []string{
		"--tr-type", "rdma", "--adr-fam", "ipv6",
		"--tr-addr", "fd00::1", "--tr-svc-id", "4421",
	}
	prefixed := []string{
		"--src-tr-type", "rdma", "--src-adr-fam", "ipv6",
		"--src-tr-addr", "fd00::1", "--src-tr-svc-id", "4421",
	}

	dn := runArgv(t, "CreateDiskNode", append(
		[]string{"dn", "create", "--addr", itDnAddr}, plain...)...)
	wantRequest(t, dn.(*pb.CreateDiskNodeRequest).NvmeTrConf, custom)

	cn := runArgv(t, "CreateControllerNode", append(
		[]string{"cn", "create", "--addr", itCnAddr}, plain...)...)
	wantRequest(t, cn.(*pb.CreateControllerNodeRequest).NvmeTrConf, custom)

	create := runArgv(t, "CreateClone", append(
		[]string{"clone", "create", "--name", "cl0"}, prefixed...)...)
	createList := create.(*pb.CreateCloneRequest).SrcTrConf
	if len(createList) != 1 {
		t.Fatalf("clone create sent %d src_tr_conf entries, want 1",
			len(createList))
	}
	wantRequest(t, createList[0], custom)

	setTr := runArgv(t, "UpdateCloneTrConf", append(
		[]string{"clone", "set-tr", "--name", "cl0"}, prefixed...)...)
	setTrList := setTr.(*pb.UpdateCloneTrConfRequest).SrcTrConf
	if len(setTrList) != 1 {
		t.Fatalf("clone set-tr sent %d src_tr_conf entries, want 1",
			len(setTrList))
	}
	wantRequest(t, setTrList[0], custom)
}

// TestEventThresholdFieldsMapOneToOne gives the four --thr-* flags distinct
// values at once, because each alone only proves that SOME field was set.
func TestEventThresholdFieldsMapOneToOne(t *testing.T) {
	req := runArgv(t, "CreateStoragePool", "sp", "create",
		"--thr-primary", "1", "--thr-cntlr", "2",
		"--thr-side", "3", "--thr-leg", "4").(*pb.CreateStoragePoolRequest)
	wantRequest(t, req.EventThreshold, &pb.EventThreshold{
		PrimaryUnhealthy: 1,
		CntlrUnhealthy:   2,
		SideUnhealthy:    3,
		LegUnhealthy:     4,
	})
}

// TestCloneSrcTrConfEmptyList is the clone group's deliberate departure from
// the nil rule (dnvctl.md, `clone` — `ctl/clone.go`): with all four `src-`
// flags emptied the list must be EMPTY rather than nil, because the
// gateway's "src_tr_conf must not be empty" refusal has to stay reachable
// from the CLI.
//
// proto.Equal cannot see the difference — an empty repeated field and an
// absent one are the same message — so this asserts on the Go slice itself.
func TestCloneSrcTrConfEmptyList(t *testing.T) {
	empty := []string{
		"--src-tr-type=", "--src-adr-fam=", "--src-tr-addr=",
		"--src-tr-svc-id=",
	}
	create := runArgv(t, "CreateClone", append(
		[]string{"clone", "create", "--name", "cl0"}, empty...)...).(*pb.CreateCloneRequest)
	if create.SrcTrConf == nil {
		t.Errorf("clone create sent a nil src_tr_conf, want an empty list")
	}
	if len(create.SrcTrConf) != 0 {
		t.Errorf("clone create sent %d src_tr_conf entries, want 0",
			len(create.SrcTrConf))
	}

	setTr := runArgv(t, "UpdateCloneTrConf", append(
		[]string{"clone", "set-tr", "--name", "cl0"}, empty...)...).(*pb.UpdateCloneTrConfRequest)
	if setTr.SrcTrConf == nil || len(setTr.SrcTrConf) != 0 {
		t.Errorf("clone set-tr sent src_tr_conf %v, want an empty list",
			setTr.SrcTrConf)
	}
}

// TestSelectorNilRule covers selectorOf: both lists empty ⇒ nil, either list
// set ⇒ a selector carrying exactly what was typed.
func TestSelectorNilRule(t *testing.T) {
	bare := runArgv(t, "GrowSlice",
		"sp", "grow-slice", "--slice", "0", "--ext", "2").(*pb.GrowSliceRequest)
	if bare.DnSelector != nil {
		t.Errorf("no --dn-* flags sent %v, want a nil selector",
			bare.DnSelector)
	}

	both := runArgv(t, "GrowSlice", "sp", "grow-slice", "--slice", "0",
		"--dn-black", "a,b", "--dn-white", "c").(*pb.GrowSliceRequest)
	wantRequest(t, both.DnSelector, &pb.NodeSelector{
		BlackList: []string{"a", "b"},
		WhiteList: []string{"c"},
	})

	// sp create carries two independent selectors; naming one must leave the
	// other nil.
	sp := runArgv(t, "CreateStoragePool",
		"sp", "create", "--cn-white", "cn0").(*pb.CreateStoragePoolRequest)
	if sp.DnSelector != nil {
		t.Errorf("--cn-white filled dn_selector %v, want nil", sp.DnSelector)
	}
	wantRequest(t, sp.CnSelector, &pb.NodeSelector{WhiteList: []string{"cn0"}})
}

// TestDmCloneConfNilRule covers dmCloneConfOf's GW11 "not given" convention:
// both zero ⇒ nil, either non-zero ⇒ a conf carrying the other as 0.
func TestDmCloneConfNilRule(t *testing.T) {
	bare := runArgv(t, "CreateMigration",
		"migr", "create", "--name", "m0").(*pb.CreateMigrationRequest)
	if bare.DmCloneConf != nil {
		t.Errorf("no --hyd-* flags sent %v, want a nil conf", bare.DmCloneConf)
	}

	one := runArgv(t, "CreateMigration", "migr", "create", "--name", "m0",
		"--hyd-threshold", "8").(*pb.CreateMigrationRequest)
	wantRequest(t, one.DmCloneConf, &pb.DmCloneConf{HydrationThreshold: 8})

	other := runArgv(t, "CreateClone", "clone", "create", "--name", "cl0",
		"--hyd-batch", "4").(*pb.CreateCloneRequest)
	wantRequest(t, other.DmCloneConf, &pb.DmCloneConf{HydrationBatchSize: 4})
}

// TestListFlagsReplaceOnSet pins the list rule of dnvctl.md, Conventions: a
// repeated occurrence REPLACES, empty items are dropped, and an empty value
// clears the list rather than leaving the previous one in place.
func TestListFlagsReplaceOnSet(t *testing.T) {
	replaced := runArgv(t, "UpdateSubsystemHosts", "ss", "set-hosts",
		"--nqn", itNqn, "--hosts", "a,b", "--hosts", "c,d").(*pb.UpdateSubsystemHostsRequest)
	wantRequest(t, replaced, &pb.UpdateSubsystemHostsRequest{
		ClusterName:  itCluster,
		SpName:       itSp,
		Nqn:          itNqn,
		AllowedHosts: []string{"c", "d"},
	})

	trimmed := runArgv(t, "UpdateSubsystemHosts", "ss", "set-hosts",
		"--nqn", itNqn, "--hosts", " a , , b ").(*pb.UpdateSubsystemHostsRequest)
	if len(trimmed.AllowedHosts) != 2 ||
		trimmed.AllowedHosts[0] != "a" || trimmed.AllowedHosts[1] != "b" {
		t.Errorf("--hosts %q gave %v, want [a b]",
			" a , , b ", trimmed.AllowedHosts)
	}

	cleared := runArgv(t, "UpdateSubsystemHosts", "ss", "set-hosts",
		"--nqn", itNqn, "--hosts=").(*pb.UpdateSubsystemHostsRequest)
	if len(cleared.AllowedHosts) != 0 {
		t.Errorf("--hosts= gave %v, want an empty list", cleared.AllowedHosts)
	}

	// The numeric lists follow the same rule.
	slots := runArgv(t, "UpdateStoragePoolCntlidSlotList",
		"sp", "set-cntlid-slots", "--slots", "0,1", "--slots", "5,0x6").(*pb.UpdateStoragePoolCntlidSlotListRequest)
	wantRequest(t, slots, &pb.UpdateStoragePoolCntlidSlotListRequest{
		ClusterName:    itCluster,
		SpName:         itSp,
		CntlidSlotList: []uint32{5, 6},
	})

	ids := runArgv(t, "FindStoragePoolNames",
		"sp", "find-names", "--ids", "1,0x63").(*pb.FindStoragePoolNamesRequest)
	wantRequest(t, ids, &pb.FindStoragePoolNamesRequest{
		ClusterName: itCluster,
		SpIdList:    []uint64{1, 0x63},
	})
}

// TestSpCreateAlwaysSendsRedundConf pins the default of
// dnvctl.md, `sp` — `ctl/sp.go`: `sp create` sends a bdev_conf with a
// redund_conf for BOTH --redund values, and the default is raid1 even though
// the flag was never typed.
func TestSpCreateAlwaysSendsRedundConf(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want *pb.RedundConf
	}{
		{"default", nil, &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{}}}},
		{"raid1", []string{"--redund", "raid1"}, &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{}}}},
		{"raid1 with chunk", []string{
			"--redund", "raid1", "--bitmap-chunk-blocks", "64"},
			&pb.RedundConf{RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: 64}}}},
		{"none", []string{"--redund", "none"}, &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{}}}},
		{"case insensitive", []string{"--redund", "NONE"}, &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{}}}},
		// The chunk size belongs to the raid1 arm and is quietly unused under
		// `none`; cross-checking the two flags would be the client-side
		// validation CT8 forbids.
		{"chunk under none", []string{
			"--redund", "none", "--bitmap-chunk-blocks", "64"},
			&pb.RedundConf{RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := runArgv(t, "CreateStoragePool",
				append([]string{"sp", "create"}, tc.argv...)...).(*pb.CreateStoragePoolRequest)
			if req.BdevConf == nil {
				t.Fatalf("sp create sent no bdev_conf")
			}
			wantRequest(t, req.BdevConf.RedundConf, tc.want)
			if req.BdevConf.DmRaid0Conf != nil {
				t.Errorf("sp create sent dm_raid0_conf %v, want it absent",
					req.BdevConf.DmRaid0Conf)
			}
			if req.BdevConf.DmPoolConf != nil {
				t.Errorf("sp create sent dm_pool_conf %v, want it absent",
					req.BdevConf.DmPoolConf)
			}
			if req.EventThreshold != nil {
				t.Errorf("sp create sent event_threshold %v, want it absent",
					req.EventThreshold)
			}
			if len(req.BdevConf.BdevFeatureList) != 0 {
				t.Errorf("sp create sent bdev_feature_list %v, want none "+
					"(dnvctl.md, In scope / out of scope)",
					req.BdevConf.BdevFeatureList)
			}
		})
	}
}

// TestSpCreateOptionalConfs covers the other half of
// dnvctl.md, `sp` — `ctl/sp.go`: dm_raid0_conf appears only when
// --stripe-size is non-zero, dm_pool_conf only when --block-size or
// --low-water-mark-pct is (the mark's half is TestSpCreateLowWaterMark's),
// and event_threshold only when at least one --thr-* is.
func TestSpCreateOptionalConfs(t *testing.T) {
	req := runArgv(t, "CreateStoragePool", "sp", "create",
		"--stripe-size", "16384", "--block-size", "1048576",
		"--thr-side", "30").(*pb.CreateStoragePoolRequest)
	wantRequest(t, req.BdevConf.DmRaid0Conf, &pb.DmRaid0Conf{StripeSize: 16384})
	wantRequest(t, req.BdevConf.DmPoolConf,
		&pb.DmPoolConf{DataBlockSize: 1048576})
	wantRequest(t, req.EventThreshold, &pb.EventThreshold{SideUnhealthy: 30})

	// Each --thr-* alone is enough to build the message, and the other three
	// travel as zeros rather than being omitted.
	for _, flag := range []string{
		"--thr-primary", "--thr-cntlr", "--thr-side", "--thr-leg",
	} {
		req := runArgv(t, "CreateStoragePool",
			"sp", "create", flag, "5").(*pb.CreateStoragePoolRequest)
		if req.EventThreshold == nil {
			t.Errorf("%s 5 sent no event_threshold", flag)
		}
	}
}

// TestSpCreateLowWaterMark pins --low-water-mark-pct, the dm_pool_conf member
// that had no flag, so every pool dnvctl created took its cluster's mark —
// the default 50 on any cluster dnvctl created — and nothing driven through
// dnvctl could move a pool's grow point or switch its auto-grow off. The
// flag builds dm_pool_conf on its own, travels beside --block-size when both
// are given, and a zero is "not given" like the message's other member. A
// mark above 100 switches auto-grow off and is forwarded as typed (CT8).
func TestSpCreateLowWaterMark(t *testing.T) {
	alone := runArgv(t, "CreateStoragePool", "sp", "create",
		"--low-water-mark-pct", "30").(*pb.CreateStoragePoolRequest)
	wantRequest(t, alone.GetBdevConf().GetDmPoolConf(),
		&pb.DmPoolConf{LowWaterMarkPct: 30})

	both := runArgv(t, "CreateStoragePool", "sp", "create",
		"--block-size", "1048576",
		"--low-water-mark-pct", "30").(*pb.CreateStoragePoolRequest)
	wantRequest(t, both.GetBdevConf().GetDmPoolConf(),
		&pb.DmPoolConf{DataBlockSize: 1048576, LowWaterMarkPct: 30})

	off := runArgv(t, "CreateStoragePool", "sp", "create",
		"--low-water-mark-pct", "101").(*pb.CreateStoragePoolRequest)
	wantRequest(t, off.GetBdevConf().GetDmPoolConf(),
		&pb.DmPoolConf{LowWaterMarkPct: 101})

	zero := runArgv(t, "CreateStoragePool", "sp", "create",
		"--low-water-mark-pct", "0").(*pb.CreateStoragePoolRequest)
	if zero.GetBdevConf().GetDmPoolConf() != nil {
		t.Errorf("--low-water-mark-pct 0 sent dm_pool_conf %v, want it absent",
			zero.GetBdevConf().GetDmPoolConf())
	}
}

// TestSpLevelSpellings covers spParseLevel's three accepted forms. The raw
// number is CT8, not a convenience: an enum value the schema does not declare
// is forwarded as typed so the gateway's validateSpLevel is the one that
// refuses it.
func TestSpLevelSpellings(t *testing.T) {
	cases := []struct {
		spec string
		want pb.SpLevel
	}{
		{"READWRITE", pb.SpLevel_SP_LEVEL_READWRITE},
		{"readonly", pb.SpLevel_SP_LEVEL_READONLY},
		{"SP_LEVEL_NO_THINPOOL", pb.SpLevel_SP_LEVEL_NO_THINPOOL},
		{"sp_level_no_clone", pb.SpLevel_SP_LEVEL_NO_CLONE},
		{"16", pb.SpLevel_SP_LEVEL_READONLY},
		{"0x10", pb.SpLevel_SP_LEVEL_READONLY},
		{"17", pb.SpLevel(17)},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			req := runArgv(t, "UpdateStoragePoolLevel",
				"sp", "set-level", "--level", tc.spec).(*pb.UpdateStoragePoolLevelRequest)
			if req.SpLevel != tc.want {
				t.Errorf("--level %q sent %v, want %v",
					tc.spec, req.SpLevel, tc.want)
			}
		})
	}

	// The default is the enum's zero value, so omitting the flag sends no
	// surprise (dnvctl.md, `sp` — `ctl/sp.go`).
	req := runArgv(t, "UpdateStoragePoolLevel", "sp", "set-level").(*pb.UpdateStoragePoolLevelRequest)
	if req.SpLevel != pb.SpLevel_SP_LEVEL_READWRITE {
		t.Errorf("a bare sp set-level sent %v, want READWRITE", req.SpLevel)
	}
}

// TestClusterCreateExtentSize states what sweep step 1 cannot. That row now
// TYPES the flag, and the table is pinned at 59 rows, one per RPC, so no
// second `cluster create` row can exist. What the row stopped saying is
// asserted here — a bare `cluster create` sends no dn_bin_conf — together
// with the three things it never could: an explicit --extent-size 0 sends
// none either, an out-of-range value still travels, and the command line is
// the flag's only carrier (CT9). The row's argv is not repeated here; it
// asserts its value against a whole CreateClusterRequest, which is strictly
// more than this test does.
//
// The shifts: DnBinConf's four are proto3 scalars, so a shift dnvctl sent as
// a literal 0 would be indistinguishable on the wire from one it did not send
// — and harmless, since model.ResolveDnBinConf reads the all-zero set as "no
// ladder" either way. What the whole-message comparisons below do catch is
// the ladder dnvctl must never invent: 4, 8 or 12 travelling alongside the
// size — the non-zero members of the default set. Nothing downstream would
// catch it for us, because {0,4,8,12} IS a valid ladder and the gateway takes
// it; the client would simply have decided a value the gateway resolves.
func TestClusterCreateExtentSize(t *testing.T) {
	bare := runArgv(t, "CreateCluster",
		"cluster", "create", "--name", "c1").(*pb.CreateClusterRequest)
	wantRequest(t, bare, &pb.CreateClusterRequest{ClusterName: "c1"})
	if bare.DnBinConf != nil {
		t.Errorf("a bare cluster create sent dn_bin_conf %v, want it absent",
			bare.DnBinConf)
	}

	// An explicitly typed zero is the same as not typing the flag at all —
	// the "not given" convention of dnvctl.md, Conventions, for an optional
	// sub-message, the same rule as `sp create`'s --stripe-size. What CT8
	// forbids is the other move: translating a 0 into common.DefaultDnExtSize
	// here rather than leaving the gateway to resolve it.
	zero := runArgv(t, "CreateCluster", "cluster", "create",
		"--name", "c1", "--extent-size", "0").(*pb.CreateClusterRequest)
	if zero.DnBinConf != nil {
		t.Errorf("--extent-size 0 sent dn_bin_conf %v, want it absent",
			zero.DnBinConf)
	}

	// A non-zero value fills extent_size and nothing else in dn_bin_conf, and
	// it travels unchecked: 1 is far below MinDnExtSize (67108864, which is
	// what step 1 sends) and dnvctl forwards it anyway, because refusing it
	// is the gateway's job (CT8).
	tiny := runArgv(t, "CreateCluster", "cluster", "create",
		"--name", "c1", "--extent-size", "1").(*pb.CreateClusterRequest)
	wantRequest(t, tiny.DnBinConf, &pb.DnBinConf{ExtentSize: 1})

	// The command line is this flag's only carrier (CT9), and a
	// ClusterConf is write-once, so a second one would do its worst here: a
	// stale DNVCTL_EXTENT_SIZE would size every cluster created under it,
	// and text viper could not cast used to read back as 0 — the gateway
	// default, permanently — where pflag refuses the same text below.
	t.Run("the environment is not a carrier", func(t *testing.T) {
		for _, value := range []string{"67108864", "-1"} {
			t.Setenv("DNVCTL_EXTENT_SIZE", value)
			fromEnv := runArgv(t, "CreateCluster", "cluster", "create",
				"--name", "c1").(*pb.CreateClusterRequest)
			if fromEnv.DnBinConf != nil {
				t.Errorf("DNVCTL_EXTENT_SIZE=%s sent dn_bin_conf %v, want "+
					"it absent", value, fromEnv.DnBinConf)
			}
		}
	})

	t.Run("the flag refuses the same text", func(t *testing.T) {
		client := &recordingClient{want: "CreateCluster"}
		res := runCLI(t, client, globalArgv("cluster", "create",
			"--name", "c1", "--extent-size", "-1")...)
		if res.code != 2 {
			t.Errorf("--extent-size -1 exited %d, want 2 (stderr %q)",
				res.code, res.stderr)
		}
		if client.calls != 0 {
			t.Errorf("--extent-size -1 issued %d RPCs, want 0", client.calls)
		}
	})
}

// TestClusterNameFallback is the first field→flag exception of dnvctl.md,
// Conventions, in all three states: --name wins, an empty --name falls back
// to the global --cluster, and `cluster list` — the one request with no
// cluster_name — declares no --name at all, so naming one is a usage error
// rather than a silently dropped value.
func TestClusterNameFallback(t *testing.T) {
	named := runArgv(t, "GetCluster", "cluster", "get", "--name", "other").(*pb.GetClusterRequest)
	if named.ClusterName != "other" {
		t.Errorf("--name other sent %q, want other", named.ClusterName)
	}

	fallback := runArgv(t, "GetCluster", "cluster", "get").(*pb.GetClusterRequest)
	if fallback.ClusterName != itCluster {
		t.Errorf("a bare cluster get sent %q, want the global --cluster %q",
			fallback.ClusterName, itCluster)
	}

	// An explicitly emptied --name falls back too: clusterNameOf keys on the
	// value, not on whether the flag was typed.
	emptied := runArgv(t, "GetCluster", "cluster", "get", "--name=").(*pb.GetClusterRequest)
	if emptied.ClusterName != itCluster {
		t.Errorf("--name= sent %q, want the global --cluster %q",
			emptied.ClusterName, itCluster)
	}

	// Both empty sends an empty cluster_name; the gateway substitutes its own
	// default and dnvctl substitutes nothing (CT8).
	bare := runArgvFull(t, "GetCluster",
		"--gateway-address", gatewayAddress, "cluster", "get").(*pb.GetClusterRequest)
	if bare.ClusterName != "" {
		t.Errorf("no --cluster and no --name sent %q, want empty",
			bare.ClusterName)
	}

	res := runCLI(t, &recordingClient{},
		globalArgv("cluster", "list", "--name", "x")...)
	if res.code != 2 {
		t.Errorf("cluster list --name x exited %d, want 2", res.code)
	}
}

// TestGlobalsAreIgnoredWhereTheFieldIsAbsent is the other half of dnvctl.md,
// Global flags, env, config: a --cluster or --sp left empty is sent empty,
// and a command whose request lacks the field simply ignores it (CT8). The
// proof that `cluster list`, `sp list` and `sp find-names` ignore --sp is
// structural — their requests have no sp_name field — so what is asserted
// here is that naming the global flags does not turn into some OTHER field
// of those requests.
func TestGlobalsAreIgnoredWhereTheFieldIsAbsent(t *testing.T) {
	list := runArgv(t, "ListClusters", "cluster", "list").(*pb.ListClustersRequest)
	wantRequest(t, list, &pb.ListClustersRequest{})

	spList := runArgv(t, "ListStoragePools", "sp", "list").(*pb.ListStoragePoolsRequest)
	wantRequest(t, spList, &pb.ListStoragePoolsRequest{
		ClusterName: itCluster})

	names := runArgv(t, "FindStoragePoolNames", "sp", "find-names").(*pb.FindStoragePoolNamesRequest)
	wantRequest(t, names, &pb.FindStoragePoolNamesRequest{
		ClusterName: itCluster})

	// A --cluster and --sp left empty travel as empty strings, not as
	// anything invented.
	empty := runArgvFull(t, "GetStoragePool",
		"--gateway-address", gatewayAddress, "sp", "get").(*pb.GetStoragePoolRequest)
	wantRequest(t, empty, &pb.GetStoragePoolRequest{})
}

// TestEnvBinding is CT9's precedence, and the reason every test in this
// package resets viper: every global flag but --rev is env-backed through
// viper's AutomaticEnv, so DNVCTL_CLUSTER is ambient context for every
// invocation and an explicit flag beats it.
func TestEnvBinding(t *testing.T) {
	t.Setenv("DNVCTL_CLUSTER", "envclu")
	t.Setenv("DNVCTL_SP", "envsp")
	t.Setenv("DNVCTL_GATEWAY_ADDRESS", gatewayAddress)

	fromEnv := runArgvFull(t, "GetStoragePool", "sp", "get").(*pb.GetStoragePoolRequest)
	wantRequest(t, fromEnv, &pb.GetStoragePoolRequest{
		ClusterName: "envclu", SpName: "envsp"})

	flagWins := runArgvFull(t, "GetStoragePool",
		"--cluster", "flagclu", "sp", "get").(*pb.GetStoragePoolRequest)
	wantRequest(t, flagWins, &pb.GetStoragePoolRequest{
		ClusterName: "flagclu", SpName: "envsp"})
}

// TestLeafFlagsIgnoreTheEnvironment is the other half of CT9: only
// the env-backed global flags have an environment or config carrier. --rev
// and every leaf flag are read off the command line alone, so a variable
// exported for one command, or a key sitting in a config file, cannot put a
// token, a name, a boolean such as a force, a count, an id, a list or a
// bitmap into a request nobody typed it for. Each case leaves the flag
// untyped, which is exactly when a second carrier would be consulted, and the
// cases span every reader a flag reaches — string (--rev, --name), bool
// (--force, --enabled), uint32 (--count) and the five built on strOf's text
// (hexOf for --id, idListOf for --ids, u32ListOf for --slots, hexBytesOf for
// --bm-hex, strListOf for --hosts); TestClusterCreateExtentSize holds the
// uint64 one — because each reader could grow a fallback of its own, and
// each of those five read viper itself before CT9.
func TestLeafFlagsIgnoreTheEnvironment(t *testing.T) {
	t.Run("DNVCTL_REV sends no token", func(t *testing.T) {
		t.Setenv("DNVCTL_REV", "12")
		req := runArgv(t, "CreateThinDevice",
			"td", "create", "--name", "t0").(*pb.CreateThinDeviceRequest)
		if req.SpRev != nil {
			t.Errorf("DNVCTL_REV=12 sent sp_rev %v, want no token message",
				req.SpRev)
		}
	})

	t.Run("DNVCTL_FORCE forces nothing", func(t *testing.T) {
		t.Setenv("DNVCTL_FORCE", "true")
		req := runArgv(t, "DeleteClone",
			"clone", "delete", "--name", "cl0").(*pb.DeleteCloneRequest)
		if req.Force {
			t.Errorf("DNVCTL_FORCE=true sent force=true, want false")
		}
	})

	// `cluster get` with no --name falls back to the global --cluster; a
	// DNVCTL_NAME must not step in between and redirect it.
	t.Run("DNVCTL_NAME names nothing", func(t *testing.T) {
		t.Setenv("DNVCTL_NAME", "other")
		req := runArgv(t, "GetCluster",
			"cluster", "get").(*pb.GetClusterRequest)
		if req.ClusterName != itCluster {
			t.Errorf("DNVCTL_NAME=other sent cluster_name %q, want the "+
				"global --cluster %q", req.ClusterName, itCluster)
		}
	})

	// --enabled defaults to true (dnvctl.md, `cntlr` — `ctl/cntlr.go`); were
	// the environment its carrier, an exported DNVCTL_ENABLED=false would
	// disable a controller on every `set-enabled` typed without the flag.
	t.Run("DNVCTL_ENABLED leaves --enabled at its default", func(t *testing.T) {
		t.Setenv("DNVCTL_ENABLED", "false")
		req := runArgv(t, "UpdateCntlrEnabled",
			"cntlr", "set-enabled", "--id", "3").(*pb.UpdateCntlrEnabledRequest)
		if !req.Enabled {
			t.Errorf("DNVCTL_ENABLED=false sent enabled=false, want the " +
				"flag default true")
		}
	})

	t.Run("DNVCTL_COUNT pages nothing", func(t *testing.T) {
		t.Setenv("DNVCTL_COUNT", "4")
		req := runArgv(t, "ListStoragePools",
			"sp", "list").(*pb.ListStoragePoolsRequest)
		if req.Count != 0 {
			t.Errorf("DNVCTL_COUNT=4 sent count %d, want 0", req.Count)
		}
	})

	// hexOf (the id flags), idListOf, u32ListOf, hexBytesOf and strListOf
	// parse strOf's text, and each read viper itself before CT9, so each
	// is pinned on its own.
	t.Run("the id, list and hex readers take nothing from the environment",
		func(t *testing.T) {
			t.Setenv("DNVCTL_ID", "3")
			t.Setenv("DNVCTL_IDS", "4,5")
			t.Setenv("DNVCTL_SLOTS", "0,1")
			t.Setenv("DNVCTL_BM_HEX", "a5")
			t.Setenv("DNVCTL_HOSTS", "h0")
			wantRequest(t, runArgv(t, "DeleteCntlr", "cntlr", "delete"),
				&pb.DeleteCntlrRequest{ClusterName: itCluster, SpName: itSp})
			wantRequest(t, runArgv(t, "FindStoragePoolNames",
				"sp", "find-names"),
				&pb.FindStoragePoolNamesRequest{ClusterName: itCluster})
			wantRequest(t, runArgv(t, "UpdateStoragePoolCntlidSlotList",
				"sp", "set-cntlid-slots"),
				&pb.UpdateStoragePoolCntlidSlotListRequest{
					ClusterName: itCluster, SpName: itSp})
			wantRequest(t, runArgv(t, "AppendCloneBitmap",
				"clone", "append-bm", "--name", "cl0"),
				&pb.AppendCloneBitmapRequest{
					ClusterName: itCluster, SpName: itSp, CloneName: "cl0"})
			wantRequest(t, runArgv(t, "UpdateSubsystemHosts",
				"ss", "set-hosts", "--nqn", "n0"),
				&pb.UpdateSubsystemHostsRequest{
					ClusterName: itCluster, SpName: itSp, Nqn: "n0"})
		})

	t.Run("a config file supplies no leaf", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dnvctl.json")
		if err := os.WriteFile(path,
			[]byte(`{"rev": "12", "force": true}`), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		req := runArgv(t, "DeleteClone", "--config", path,
			"clone", "delete", "--name", "cl0").(*pb.DeleteCloneRequest)
		wantRequest(t, req, &pb.DeleteCloneRequest{
			ClusterName: itCluster, SpName: itSp, CloneName: "cl0"})
	})
}

// TestBadNumericEnvIsAUsageError is the refusal half of CT9: an env-backed
// global flag keeps its environment and config carriers, so text there that
// is not a number is what the same text as a flag argument is — a usage error
// before any dial — never a cast that reads back as zero. --timeout is the
// one numeric env-backed global flag, and a zero there is a deadline that has
// already passed. NaN and ±Inf parse on every carrier, the flag's included,
// and name no deadline, so each is refused from each; and a positive value
// past time.Duration's range is saturated, not converted into a deadline
// that has already passed. The dial seam records how often it was reached
// and how far away the deadline was.
func TestBadNumericEnvIsAUsageError(t *testing.T) {
	type dialRecord struct {
		dials  int
		budget time.Duration
	}
	seam := func(rec *dialRecord, client pb.GatewayClient) dialFunc {
		return func(ctx context.Context, _ string) (
			pb.GatewayClient, func() error, error,
		) {
			rec.dials++
			if deadline, ok := ctx.Deadline(); ok {
				rec.budget = time.Until(deadline)
			}
			return client, func() error { return nil }, nil
		}
	}
	refused := func(t *testing.T, what string, argv ...string) {
		t.Helper()
		var rec dialRecord
		client := &recordingClient{}
		res := runCLIWithDial(t, seam(&rec, client),
			globalArgv(append(argv, "cluster", "list")...)...)
		if res.code != 2 {
			t.Errorf("%s exited %d with a deadline %v away, want 2 "+
				"(stderr %q)", what, res.code, rec.budget, res.stderr)
		}
		if rec.dials != 0 || client.calls != 0 {
			t.Errorf("%s dialed %d times and issued %d RPCs, want 0 and 0",
				what, rec.dials, client.calls)
		}
		if !strings.Contains(res.stderr, "--timeout") {
			t.Errorf("%s: stderr %q does not name --timeout", what, res.stderr)
		}
	}

	t.Run("DNVCTL_TIMEOUT=abc", func(t *testing.T) {
		t.Setenv("DNVCTL_TIMEOUT", "abc")
		refused(t, "DNVCTL_TIMEOUT=abc")
	})

	config := func(t *testing.T, timeout string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "dnvctl.json")
		if err := os.WriteFile(path,
			[]byte(`{"timeout": "`+timeout+`"}`), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	t.Run("the same text in a config file", func(t *testing.T) {
		refused(t, `a config "timeout": "abc"`,
			"--config", config(t, "abc"))
	})

	// strconv.ParseFloat, which pflag's Float64 flag calls too, takes these
	// three, so the flag lets them through and only timeoutOf refuses them.
	for _, text := range []string{"NaN", "Inf", "-Inf"} {
		t.Run(text+" from every carrier", func(t *testing.T) {
			refused(t, "--timeout "+text, "--timeout", text)
			refused(t, `a config "timeout": "`+text+`"`,
				"--config", config(t, text))
			t.Setenv("DNVCTL_TIMEOUT", text)
			refused(t, "DNVCTL_TIMEOUT="+text)
		})
	}

	// The carrier itself stays: a parsable DNVCTL_TIMEOUT is the deadline.
	t.Run("a number still applies", func(t *testing.T) {
		t.Setenv("DNVCTL_TIMEOUT", "2.5")
		var rec dialRecord
		client := &recordingClient{want: "ListClusters"}
		res := runCLIWithDial(t, seam(&rec, client),
			globalArgv("cluster", "list")...)
		if res.code != 0 {
			t.Fatalf("DNVCTL_TIMEOUT=2.5 exited %d, want 0 (stderr %q)",
				res.code, res.stderr)
		}
		if rec.budget <= 2*time.Second || rec.budget > 2500*time.Millisecond {
			t.Errorf("DNVCTL_TIMEOUT=2.5 gave a deadline %v away, want "+
				"just under 2.5s", rec.budget)
		}
	})

	// 1e10 s is past time.Duration's reach (math.MaxInt64 ns, about 292
	// years). Converted rather than saturated, it became math.MinInt64 on
	// amd64: a deadline that had already passed.
	t.Run("a value past time.Duration's range is saturated", func(t *testing.T) {
		var rec dialRecord
		client := &recordingClient{want: "ListClusters"}
		res := runCLIWithDial(t, seam(&rec, client),
			globalArgv("--timeout", "1e10", "cluster", "list")...)
		if res.code != 0 {
			t.Fatalf("--timeout 1e10 exited %d, want 0 (stderr %q)",
				res.code, res.stderr)
		}
		if rec.dials != 1 || rec.budget < 290*365*24*time.Hour {
			t.Errorf("--timeout 1e10 dialed %d times with a deadline %v "+
				"away, want 1 and about 292 years", rec.dials, rec.budget)
		}
	})
}

// TestIdFlagsAcceptBase0 pins the id rule of dnvctl.md, Conventions, across
// the groups that declare one: 17 and 0x11 name the same object.
func TestIdFlagsAcceptBase0(t *testing.T) {
	cntlr := runArgv(t, "DeleteCntlr",
		"cntlr", "delete", "--id", "0x11").(*pb.DeleteCntlrRequest)
	if cntlr.CntlrId != 17 {
		t.Errorf("--id 0x11 sent %d, want 17", cntlr.CntlrId)
	}

	side := runArgv(t, "InspectSide",
		"sp", "inspect-side", "--id", "0x11").(*pb.InspectSideRequest)
	if side.SideId != 17 {
		t.Errorf("--id 0x11 sent %d, want 17", side.SideId)
	}

	leg := runArgv(t, "GetLegBitmap",
		"td", "get-leg-bm", "--leg", "0xff").(*pb.GetLegBitmapRequest)
	if leg.LegId != 255 {
		t.Errorf("--leg 0xff sent %d, want 255", leg.LegId)
	}

	sw := runArgv(t, "SwitchSpareLeg", "spare", "switch", "--grp", "0x1",
		"--spare", "0x6", "--target", "4").(*pb.SwitchSpareLegRequest)
	wantRequest(t, sw, &pb.SwitchSpareLegRequest{
		ClusterName: itCluster,
		SpName:      itSp,
		GrpId:       1,
		SpareLegId:  6,
		TargetLegId: 4,
	})

	migr := runArgv(t, "CreateMigration", "migr", "create", "--name", "m0",
		"--src-side", "0x1f").(*pb.CreateMigrationRequest)
	if migr.SrcSideId != 31 {
		t.Errorf("--src-side 0x1f sent %d, want 31", migr.SrcSideId)
	}

	slice := runArgv(t, "GrowSlice", "sp", "grow-slice", "--slice", "0x20").(*pb.GrowSliceRequest)
	if slice.SliceId != 32 {
		t.Errorf("--slice 0x20 sent %d, want 32", slice.SliceId)
	}
}

// TestBitmapHexToBytes covers hexBytesOf's two accepted inputs: an EMPTY
// value sends an empty bitmap on purpose (so the gateway's "must not be
// empty" refusal stays reachable), and a well-formed value is decoded as
// typed. The malformed case is a usage error and lives in the CT5 tests.
func TestBitmapHexToBytes(t *testing.T) {
	empty := runArgv(t, "AppendCloneBitmap",
		"clone", "append-bm", "--name", "cl0").(*pb.AppendCloneBitmapRequest)
	if len(empty.Bitmap) != 0 {
		t.Errorf("no --bm-hex sent %v, want an empty bitmap", empty.Bitmap)
	}

	upper := runArgv(t, "AppendMigrationBitmap",
		"migr", "append-bm", "--name", "m0", "--bm-hex", "FF00A5").(*pb.AppendMigrationBitmapRequest)
	if string(upper.Bitmap) != string([]byte{0xff, 0x00, 0xa5}) {
		t.Errorf("--bm-hex FF00A5 sent %v, want [255 0 165]", upper.Bitmap)
	}
}

// TestBoolFlagDefaults pins the four booleans whose defaults differ from the
// proto3 zero, because those are the ones an operator gets wrong: --enabled
// and --suspended (on `ns set-suspended` only) default TRUE, while --disabled
// and --suspended (on `ns create`) default FALSE.
func TestBoolFlagDefaults(t *testing.T) {
	enabled := runArgv(t, "UpdateCntlrEnabled",
		"cntlr", "set-enabled", "--id", "3").(*pb.UpdateCntlrEnabledRequest)
	if !enabled.Enabled {
		t.Errorf("a bare cntlr set-enabled sent enabled=false, want true")
	}

	suspended := runArgv(t, "UpdateNamespaceSuspended",
		"ns", "set-suspended", "--nqn", itNqn, "--idx", "1").(*pb.UpdateNamespaceSuspendedRequest)
	if !suspended.Suspended {
		t.Errorf("a bare ns set-suspended sent suspended=false, want true")
	}
	resumed := runArgv(t, "UpdateNamespaceSuspended", "ns", "set-suspended",
		"--nqn", itNqn, "--idx", "1", "--suspended=false").(*pb.UpdateNamespaceSuspendedRequest)
	if resumed.Suspended {
		t.Errorf("--suspended=false sent suspended=true, want false")
	}

	created := runArgv(t, "CreateNamespace",
		"ns", "create", "--nqn", itNqn, "--idx", "1").(*pb.CreateNamespaceRequest)
	if created.Suspended {
		t.Errorf("a bare ns create sent suspended=true, want false")
	}

	node := runArgv(t, "CreateDiskNode",
		"dn", "create", "--addr", itDnAddr).(*pb.CreateDiskNodeRequest)
	if node.Disabled {
		t.Errorf("a bare dn create sent disabled=true, want false")
	}
}
