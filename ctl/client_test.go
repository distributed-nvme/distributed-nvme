// client_test.go is the other half of CT-T2's recordingClient: one method per
// RPC of `service Gateway`, all 59 of them, each a single call into rpcCall.
//
// They are uniform on purpose. A method that did anything of its own would be
// a place for a test to accidentally assert against the stub instead of
// against dnvctl, and the whole value of the recorder is that it records and
// nothing else. The list is also a second, independent census of the service:
// if pb ever grows a 60th RPC, this type stops satisfying pb.GatewayClient
// through its own methods and falls through to the embedded nil — which
// CT-T1's len(Methods) == 59 pin catches first, at the same commit.
package ctl

import (
	"context"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

func (c *recordingClient) CreateCluster(
	_ context.Context,
	in *pb.CreateClusterRequest,
	_ ...grpc.CallOption,
) (*pb.CreateClusterReply, error) {
	return rpcCall(
		c, "CreateCluster", in,
		&pb.CreateClusterReply{})
}

func (c *recordingClient) DeleteCluster(
	_ context.Context,
	in *pb.DeleteClusterRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteClusterReply, error) {
	return rpcCall(
		c, "DeleteCluster", in,
		&pb.DeleteClusterReply{})
}

func (c *recordingClient) GetCluster(
	_ context.Context,
	in *pb.GetClusterRequest,
	_ ...grpc.CallOption,
) (*pb.GetClusterReply, error) {
	return rpcCall(
		c, "GetCluster", in,
		&pb.GetClusterReply{})
}

func (c *recordingClient) ListClusters(
	_ context.Context,
	in *pb.ListClustersRequest,
	_ ...grpc.CallOption,
) (*pb.ListClustersReply, error) {
	return rpcCall(
		c, "ListClusters", in,
		&pb.ListClustersReply{})
}

func (c *recordingClient) CreateDiskNode(
	_ context.Context,
	in *pb.CreateDiskNodeRequest,
	_ ...grpc.CallOption,
) (*pb.CreateDiskNodeReply, error) {
	return rpcCall(
		c, "CreateDiskNode", in,
		&pb.CreateDiskNodeReply{})
}

func (c *recordingClient) DeleteDiskNode(
	_ context.Context,
	in *pb.DeleteDiskNodeRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteDiskNodeReply, error) {
	return rpcCall(
		c, "DeleteDiskNode", in,
		&pb.DeleteDiskNodeReply{})
}

func (c *recordingClient) GetDiskNode(
	_ context.Context,
	in *pb.GetDiskNodeRequest,
	_ ...grpc.CallOption,
) (*pb.GetDiskNodeReply, error) {
	return rpcCall(
		c, "GetDiskNode", in,
		&pb.GetDiskNodeReply{})
}

func (c *recordingClient) ListDiskNodes(
	_ context.Context,
	in *pb.ListDiskNodesRequest,
	_ ...grpc.CallOption,
) (*pb.ListDiskNodesReply, error) {
	return rpcCall(
		c, "ListDiskNodes", in,
		&pb.ListDiskNodesReply{})
}

func (c *recordingClient) UpdateDiskNodeDisabled(
	_ context.Context,
	in *pb.UpdateDiskNodeDisabledRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateDiskNodeDisabledReply, error) {
	return rpcCall(
		c, "UpdateDiskNodeDisabled", in,
		&pb.UpdateDiskNodeDisabledReply{})
}

func (c *recordingClient) InspectDiskNode(
	_ context.Context,
	in *pb.InspectDiskNodeRequest,
	_ ...grpc.CallOption,
) (*pb.InspectDiskNodeReply, error) {
	return rpcCall(
		c, "InspectDiskNode", in,
		&pb.InspectDiskNodeReply{})
}

func (c *recordingClient) CreateControllerNode(
	_ context.Context,
	in *pb.CreateControllerNodeRequest,
	_ ...grpc.CallOption,
) (*pb.CreateControllerNodeReply, error) {
	return rpcCall(
		c, "CreateControllerNode", in,
		&pb.CreateControllerNodeReply{})
}

func (c *recordingClient) DeleteControllerNode(
	_ context.Context,
	in *pb.DeleteControllerNodeRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteControllerNodeReply, error) {
	return rpcCall(
		c, "DeleteControllerNode", in,
		&pb.DeleteControllerNodeReply{})
}

func (c *recordingClient) GetControllerNode(
	_ context.Context,
	in *pb.GetControllerNodeRequest,
	_ ...grpc.CallOption,
) (*pb.GetControllerNodeReply, error) {
	return rpcCall(
		c, "GetControllerNode", in,
		&pb.GetControllerNodeReply{})
}

func (c *recordingClient) ListControllerNodes(
	_ context.Context,
	in *pb.ListControllerNodesRequest,
	_ ...grpc.CallOption,
) (*pb.ListControllerNodesReply, error) {
	return rpcCall(
		c, "ListControllerNodes", in,
		&pb.ListControllerNodesReply{})
}

func (c *recordingClient) UpdateControllerNodeDisabled(
	_ context.Context,
	in *pb.UpdateControllerNodeDisabledRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateControllerNodeDisabledReply, error) {
	return rpcCall(
		c, "UpdateControllerNodeDisabled", in,
		&pb.UpdateControllerNodeDisabledReply{})
}

func (c *recordingClient) InspectControllerNode(
	_ context.Context,
	in *pb.InspectControllerNodeRequest,
	_ ...grpc.CallOption,
) (*pb.InspectControllerNodeReply, error) {
	return rpcCall(
		c, "InspectControllerNode", in,
		&pb.InspectControllerNodeReply{})
}

func (c *recordingClient) CreateStoragePool(
	_ context.Context,
	in *pb.CreateStoragePoolRequest,
	_ ...grpc.CallOption,
) (*pb.CreateStoragePoolReply, error) {
	return rpcCall(
		c, "CreateStoragePool", in,
		&pb.CreateStoragePoolReply{})
}

func (c *recordingClient) DeleteStoragePool(
	_ context.Context,
	in *pb.DeleteStoragePoolRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteStoragePoolReply, error) {
	return rpcCall(
		c, "DeleteStoragePool", in,
		&pb.DeleteStoragePoolReply{})
}

func (c *recordingClient) GetStoragePool(
	_ context.Context,
	in *pb.GetStoragePoolRequest,
	_ ...grpc.CallOption,
) (*pb.GetStoragePoolReply, error) {
	return rpcCall(
		c, "GetStoragePool", in,
		&pb.GetStoragePoolReply{})
}

func (c *recordingClient) ListStoragePools(
	_ context.Context,
	in *pb.ListStoragePoolsRequest,
	_ ...grpc.CallOption,
) (*pb.ListStoragePoolsReply, error) {
	return rpcCall(
		c, "ListStoragePools", in,
		&pb.ListStoragePoolsReply{})
}

func (c *recordingClient) UpdateStoragePoolCntlidSlotList(
	_ context.Context,
	in *pb.UpdateStoragePoolCntlidSlotListRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateStoragePoolCntlidSlotListReply, error) {
	return rpcCall(
		c, "UpdateStoragePoolCntlidSlotList", in,
		&pb.UpdateStoragePoolCntlidSlotListReply{})
}

func (c *recordingClient) UpdateStoragePoolLevel(
	_ context.Context,
	in *pb.UpdateStoragePoolLevelRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateStoragePoolLevelReply, error) {
	return rpcCall(
		c, "UpdateStoragePoolLevel", in,
		&pb.UpdateStoragePoolLevelReply{})
}

func (c *recordingClient) FindStoragePoolNames(
	_ context.Context,
	in *pb.FindStoragePoolNamesRequest,
	_ ...grpc.CallOption,
) (*pb.FindStoragePoolNamesReply, error) {
	return rpcCall(
		c, "FindStoragePoolNames", in,
		&pb.FindStoragePoolNamesReply{})
}

func (c *recordingClient) GrowSlice(
	_ context.Context,
	in *pb.GrowSliceRequest,
	_ ...grpc.CallOption,
) (*pb.GrowSliceReply, error) {
	return rpcCall(
		c, "GrowSlice", in,
		&pb.GrowSliceReply{})
}

func (c *recordingClient) CreateCntlr(
	_ context.Context,
	in *pb.CreateCntlrRequest,
	_ ...grpc.CallOption,
) (*pb.CreateCntlrReply, error) {
	return rpcCall(
		c, "CreateCntlr", in,
		&pb.CreateCntlrReply{})
}

func (c *recordingClient) DeleteCntlr(
	_ context.Context,
	in *pb.DeleteCntlrRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteCntlrReply, error) {
	return rpcCall(
		c, "DeleteCntlr", in,
		&pb.DeleteCntlrReply{})
}

func (c *recordingClient) UpdateCntlrEnabled(
	_ context.Context,
	in *pb.UpdateCntlrEnabledRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateCntlrEnabledReply, error) {
	return rpcCall(
		c, "UpdateCntlrEnabled", in,
		&pb.UpdateCntlrEnabledReply{})
}

func (c *recordingClient) InspectCntlr(
	_ context.Context,
	in *pb.InspectCntlrRequest,
	_ ...grpc.CallOption,
) (*pb.InspectCntlrReply, error) {
	return rpcCall(
		c, "InspectCntlr", in,
		&pb.InspectCntlrReply{})
}

func (c *recordingClient) InspectSide(
	_ context.Context,
	in *pb.InspectSideRequest,
	_ ...grpc.CallOption,
) (*pb.InspectSideReply, error) {
	return rpcCall(
		c, "InspectSide", in,
		&pb.InspectSideReply{})
}

func (c *recordingClient) CreateThinDevice(
	_ context.Context,
	in *pb.CreateThinDeviceRequest,
	_ ...grpc.CallOption,
) (*pb.CreateThinDeviceReply, error) {
	return rpcCall(
		c, "CreateThinDevice", in,
		&pb.CreateThinDeviceReply{})
}

func (c *recordingClient) DeleteThinDevice(
	_ context.Context,
	in *pb.DeleteThinDeviceRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteThinDeviceReply, error) {
	return rpcCall(
		c, "DeleteThinDevice", in,
		&pb.DeleteThinDeviceReply{})
}

func (c *recordingClient) ListThinDevices(
	_ context.Context,
	in *pb.ListThinDevicesRequest,
	_ ...grpc.CallOption,
) (*pb.ListThinDevicesReply, error) {
	return rpcCall(
		c, "ListThinDevices", in,
		&pb.ListThinDevicesReply{})
}

func (c *recordingClient) CreateSubsystem(
	_ context.Context,
	in *pb.CreateSubsystemRequest,
	_ ...grpc.CallOption,
) (*pb.CreateSubsystemReply, error) {
	return rpcCall(
		c, "CreateSubsystem", in,
		&pb.CreateSubsystemReply{})
}

func (c *recordingClient) DeleteSubsystem(
	_ context.Context,
	in *pb.DeleteSubsystemRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteSubsystemReply, error) {
	return rpcCall(
		c, "DeleteSubsystem", in,
		&pb.DeleteSubsystemReply{})
}

func (c *recordingClient) ListSubsystems(
	_ context.Context,
	in *pb.ListSubsystemsRequest,
	_ ...grpc.CallOption,
) (*pb.ListSubsystemsReply, error) {
	return rpcCall(
		c, "ListSubsystems", in,
		&pb.ListSubsystemsReply{})
}

func (c *recordingClient) UpdateSubsystemHosts(
	_ context.Context,
	in *pb.UpdateSubsystemHostsRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateSubsystemHostsReply, error) {
	return rpcCall(
		c, "UpdateSubsystemHosts", in,
		&pb.UpdateSubsystemHostsReply{})
}

func (c *recordingClient) CreateNamespace(
	_ context.Context,
	in *pb.CreateNamespaceRequest,
	_ ...grpc.CallOption,
) (*pb.CreateNamespaceReply, error) {
	return rpcCall(
		c, "CreateNamespace", in,
		&pb.CreateNamespaceReply{})
}

func (c *recordingClient) DeleteNamespace(
	_ context.Context,
	in *pb.DeleteNamespaceRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteNamespaceReply, error) {
	return rpcCall(
		c, "DeleteNamespace", in,
		&pb.DeleteNamespaceReply{})
}

func (c *recordingClient) UpdateNamespaceDev(
	_ context.Context,
	in *pb.UpdateNamespaceDevRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateNamespaceDevReply, error) {
	return rpcCall(
		c, "UpdateNamespaceDev", in,
		&pb.UpdateNamespaceDevReply{})
}

func (c *recordingClient) UpdateNamespaceSuspended(
	_ context.Context,
	in *pb.UpdateNamespaceSuspendedRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateNamespaceSuspendedReply, error) {
	return rpcCall(
		c, "UpdateNamespaceSuspended", in,
		&pb.UpdateNamespaceSuspendedReply{})
}

func (c *recordingClient) CreateClone(
	_ context.Context,
	in *pb.CreateCloneRequest,
	_ ...grpc.CallOption,
) (*pb.CreateCloneReply, error) {
	return rpcCall(
		c, "CreateClone", in,
		&pb.CreateCloneReply{})
}

func (c *recordingClient) DeleteClone(
	_ context.Context,
	in *pb.DeleteCloneRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteCloneReply, error) {
	return rpcCall(
		c, "DeleteClone", in,
		&pb.DeleteCloneReply{})
}

func (c *recordingClient) GetClone(
	_ context.Context,
	in *pb.GetCloneRequest,
	_ ...grpc.CallOption,
) (*pb.GetCloneReply, error) {
	return rpcCall(
		c, "GetClone", in,
		&pb.GetCloneReply{})
}

func (c *recordingClient) UpdateCloneTrConf(
	_ context.Context,
	in *pb.UpdateCloneTrConfRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateCloneTrConfReply, error) {
	return rpcCall(
		c, "UpdateCloneTrConf", in,
		&pb.UpdateCloneTrConfReply{})
}

func (c *recordingClient) AppendCloneBitmap(
	_ context.Context,
	in *pb.AppendCloneBitmapRequest,
	_ ...grpc.CallOption,
) (*pb.AppendCloneBitmapReply, error) {
	return rpcCall(
		c, "AppendCloneBitmap", in,
		&pb.AppendCloneBitmapReply{})
}

func (c *recordingClient) CreateTransfer(
	_ context.Context,
	in *pb.CreateTransferRequest,
	_ ...grpc.CallOption,
) (*pb.CreateTransferReply, error) {
	return rpcCall(
		c, "CreateTransfer", in,
		&pb.CreateTransferReply{})
}

func (c *recordingClient) DeleteTransfer(
	_ context.Context,
	in *pb.DeleteTransferRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteTransferReply, error) {
	return rpcCall(
		c, "DeleteTransfer", in,
		&pb.DeleteTransferReply{})
}

func (c *recordingClient) GetTransfer(
	_ context.Context,
	in *pb.GetTransferRequest,
	_ ...grpc.CallOption,
) (*pb.GetTransferReply, error) {
	return rpcCall(
		c, "GetTransfer", in,
		&pb.GetTransferReply{})
}

func (c *recordingClient) UpdateTransferHosts(
	_ context.Context,
	in *pb.UpdateTransferHostsRequest,
	_ ...grpc.CallOption,
) (*pb.UpdateTransferHostsReply, error) {
	return rpcCall(
		c, "UpdateTransferHosts", in,
		&pb.UpdateTransferHostsReply{})
}

func (c *recordingClient) CreateMigration(
	_ context.Context,
	in *pb.CreateMigrationRequest,
	_ ...grpc.CallOption,
) (*pb.CreateMigrationReply, error) {
	return rpcCall(
		c, "CreateMigration", in,
		&pb.CreateMigrationReply{})
}

func (c *recordingClient) FinishMigration(
	_ context.Context,
	in *pb.FinishMigrationRequest,
	_ ...grpc.CallOption,
) (*pb.FinishMigrationReply, error) {
	return rpcCall(
		c, "FinishMigration", in,
		&pb.FinishMigrationReply{})
}

func (c *recordingClient) CancelMigration(
	_ context.Context,
	in *pb.CancelMigrationRequest,
	_ ...grpc.CallOption,
) (*pb.CancelMigrationReply, error) {
	return rpcCall(
		c, "CancelMigration", in,
		&pb.CancelMigrationReply{})
}

func (c *recordingClient) GetMigration(
	_ context.Context,
	in *pb.GetMigrationRequest,
	_ ...grpc.CallOption,
) (*pb.GetMigrationReply, error) {
	return rpcCall(
		c, "GetMigration", in,
		&pb.GetMigrationReply{})
}

func (c *recordingClient) AppendMigrationBitmap(
	_ context.Context,
	in *pb.AppendMigrationBitmapRequest,
	_ ...grpc.CallOption,
) (*pb.AppendMigrationBitmapReply, error) {
	return rpcCall(
		c, "AppendMigrationBitmap", in,
		&pb.AppendMigrationBitmapReply{})
}

func (c *recordingClient) CreateSpareLeg(
	_ context.Context,
	in *pb.CreateSpareLegRequest,
	_ ...grpc.CallOption,
) (*pb.CreateSpareLegReply, error) {
	return rpcCall(
		c, "CreateSpareLeg", in,
		&pb.CreateSpareLegReply{})
}

func (c *recordingClient) DeleteSpareLeg(
	_ context.Context,
	in *pb.DeleteSpareLegRequest,
	_ ...grpc.CallOption,
) (*pb.DeleteSpareLegReply, error) {
	return rpcCall(
		c, "DeleteSpareLeg", in,
		&pb.DeleteSpareLegReply{})
}

func (c *recordingClient) SwitchSpareLeg(
	_ context.Context,
	in *pb.SwitchSpareLegRequest,
	_ ...grpc.CallOption,
) (*pb.SwitchSpareLegReply, error) {
	return rpcCall(
		c, "SwitchSpareLeg", in,
		&pb.SwitchSpareLegReply{})
}

func (c *recordingClient) GetThinDeviceBitmap(
	_ context.Context,
	in *pb.GetThinDeviceBitmapRequest,
	_ ...grpc.CallOption,
) (*pb.GetThinDeviceBitmapReply, error) {
	return rpcCall(
		c, "GetThinDeviceBitmap", in,
		&pb.GetThinDeviceBitmapReply{})
}

func (c *recordingClient) GetLegBitmap(
	_ context.Context,
	in *pb.GetLegBitmapRequest,
	_ ...grpc.CallOption,
) (*pb.GetLegBitmapReply, error) {
	return rpcCall(
		c, "GetLegBitmap", in,
		&pb.GetLegBitmapReply{})
}
