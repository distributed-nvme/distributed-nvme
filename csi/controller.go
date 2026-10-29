package csi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The keys of a volume's context and publish context (csi.md, Attach and
// detach): what the node plugin needs to connect, hold and find a volume.
// The node plugin reads them and never talks to the gateway.
const (
	ctxNqn  = "nqn"
	ctxUuid = "uuid"
	ctxCdc  = "cdc"
	// cdcSep joins the cdc addresses into the one value a context carries.
	cdcSep = ","
)

// defaultVolumeSize is the size of a volume whose create names no capacity
// range at all, which the CSI spec leaves to the plugin.
const defaultVolumeSize = 1 << 30

// controller is the CSI controller service (csi.md, The volume model; Attach
// and detach; Capacity): a gRPC client of the gateway.
type controller struct {
	csi.UnimplementedControllerServer
	gw      pb.GatewayClient
	timeout time.Duration
	cdc     []string
}

func newController(gw pb.GatewayClient, cfg Config) *controller {
	timeout := cfg.GatewayTimeout
	if timeout <= 0 {
		timeout = DefaultGatewayTimeout
	}
	return &controller{gw: gw, timeout: timeout, cdc: cfg.CdcAddresses}
}

// gwCtx bounds one gateway call. The ctx carries the trace id the
// interceptor minted, which the shared client chain hands to the gateway.
func (c *controller) gwCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// fromGateway turns a gateway error into the CSI call's error: a gateway
// status keeps its code behind a message that names the call, and a
// connection failure is UNAVAILABLE, which every sidecar retries.
func fromGateway(op string, err error) error {
	if st, ok := status.FromError(err); ok {
		return status.Errorf(st.Code(), "gateway %s: %s", op, st.Message())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Errorf(codes.DeadlineExceeded,
			"gateway %s: no answer in time", op)
	}
	return status.Errorf(codes.Unavailable, "gateway %s: %v", op, err)
}

// codeOf is the gateway's code of err, or OK.
func codeOf(err error) codes.Code {
	return status.Code(err)
}

// checkCapability holds one volume capability to what the driver serves
// (csi.md, The volume model): a single-node access mode, and a block volume
// or a mount volume of ext4 or xfs, ext4 when no filesystem is named.
func checkCapability(cap *csi.VolumeCapability) error {
	if cap == nil {
		return errors.New("a volume capability is required")
	}
	switch cap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
	case csi.VolumeCapability_AccessMode_UNKNOWN:
		return errors.New("an access mode is required")
	default:
		return fmt.Errorf("access mode %s: only one node at a time may use a "+
			"volume", cap.GetAccessMode().GetMode())
	}
	switch t := cap.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
	case *csi.VolumeCapability_Mount:
		switch fs := t.Mount.GetFsType(); fs {
		case "", fsExt4, fsXfs:
		default:
			return fmt.Errorf("filesystem %q: the driver formats %s and %s",
				fs, fsExt4, fsXfs)
		}
	default:
		return errors.New("a block or a mount access type is required")
	}
	return nil
}

// The filesystems the node plugin formats.
const (
	fsExt4 = "ext4"
	fsXfs  = "xfs"
)

// checkCapabilities is checkCapability over a create's list, which must not
// be empty.
func checkCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument,
			"volume capabilities are required")
	}
	for _, cap := range caps {
		if err := checkCapability(cap); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return nil
}

// volumeSize is the thin device size a capacity range asks for, rounded up
// to the pool's unit (csi.md, The volume model): the required bytes rounded
// up, or the limit rounded down when only a limit is given, or the
// driver's default when neither is. A range no multiple of the unit fits is
// OUT_OF_RANGE.
func volumeSize(rng *csi.CapacityRange, unit uint64) (uint64, error) {
	required, limit := rng.GetRequiredBytes(), rng.GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, status.Error(codes.InvalidArgument,
			"a capacity range must not be negative")
	}
	var size uint64
	switch {
	case required > 0:
		size = (uint64(required) + unit - 1) / unit * unit
	case limit > 0:
		size = uint64(limit) / unit * unit
	default:
		size = (defaultVolumeSize + unit - 1) / unit * unit
	}
	if size == 0 || (limit > 0 && size > uint64(limit)) {
		return 0, status.Errorf(codes.OutOfRange,
			"no size from %d to %d bytes is a multiple of the pool's unit of "+
				"%d bytes", required, limit, unit)
	}
	return size, nil
}

// CreateVolume makes a volume's four objects in order, each step repeatable
// (csi.md, The volume model): the pool, the thin device, the subsystem and
// the namespace. A step that finds its object there reads it back and goes
// on when it matches the request; a thin device of another size is
// ALREADY_EXISTS, the spec's word for an incompatible volume of the same
// name. The create returns once the four records exist; the node plugin
// waits for the device.
func (c *controller) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume name is required")
	}
	// The volume's dnv name: the name as given when dnv takes it, else its
	// acceptable form (csi.md, The volume model).
	name := dnvNameOf(req.GetName())
	if err := checkCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}
	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(codes.InvalidArgument,
			"a volume from a snapshot or another volume is not supported")
	}
	p, err := parseParams(req.GetParameters())
	if err != nil {
		return nil, err
	}
	cc, err := c.clusterConf(ctx, p.cluster)
	if err != nil {
		return nil, err
	}
	extent := cc.GetDnBinConf().GetExtentSize()
	unit := cc.GetBdevConf().GetDmRaid0Conf().GetStripeSize()
	if extent == 0 || unit == 0 {
		return nil, status.Errorf(codes.Internal,
			"cluster %q stores no extent size or stripe size",
			clusterNameOf(p.cluster))
	}
	size, err := volumeSize(req.GetCapacityRange(), unit)
	if err != nil {
		return nil, err
	}

	// The pool, named as the volume: one slice, the class's redundancy and
	// cntlr count, the cluster's defaults for the rest, and a first data
	// group of one part in volumeSteps of the volume.
	gctx, cancel := c.gwCtx(ctx)
	_, err = c.gw.CreateStoragePool(gctx, &pb.CreateStoragePoolRequest{
		ClusterName: p.cluster,
		SpName:      name,
		BdevConf:    &pb.BdevConf{RedundConf: p.redundConf()},
		CntlrCnt:    p.cntlrs,
		SliceCnt:    1,
		InitExtCnt:  initExtCnt(size, extent),
	})
	cancel()
	if err != nil && codeOf(err) != codes.AlreadyExists {
		return nil, fromGateway("CreateStoragePool", err)
	}

	// The thin device, named as the volume and sized to the request.
	tdId, err := c.ensureThinDevice(ctx, p.cluster, name, size)
	if err != nil {
		return nil, err
	}

	// The subsystem with no host yet, and its one namespace.
	nqn := subsystemNqnOf(name)
	uuid := namespaceUuidOf(name)
	gctx, cancel = c.gwCtx(ctx)
	_, err = c.gw.CreateSubsystem(gctx, &pb.CreateSubsystemRequest{
		ClusterName: p.cluster,
		SpName:      name,
		Nqn:         nqn,
	})
	cancel()
	if err != nil && codeOf(err) != codes.AlreadyExists {
		return nil, fromGateway("CreateSubsystem", err)
	}
	if err := c.ensureNamespace(ctx, p.cluster, name, nqn, uuid, tdId); err != nil {
		return nil, err
	}

	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      volumeIdOf(p.cluster, name),
		CapacityBytes: int64(size),
		VolumeContext: map[string]string{ctxNqn: nqn, ctxUuid: uuid},
	}}, nil
}

// clusterConf reads the cluster a class names; a cluster that does not
// exist is the class's error.
func (c *controller) clusterConf(
	ctx context.Context,
	cluster string,
) (*pb.ClusterConf, error) {
	gctx, cancel := c.gwCtx(ctx)
	defer cancel()
	reply, err := c.gw.GetCluster(gctx, &pb.GetClusterRequest{
		ClusterName: cluster,
	})
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return nil, status.Errorf(codes.InvalidArgument,
				"parameter %s: cluster %q does not exist",
				paramCluster, clusterNameOf(cluster))
		}
		return nil, fromGateway("GetCluster", err)
	}
	return reply.GetClusterConf(), nil
}

// ensureThinDevice creates the volume's thin device or finds it, and
// returns its id. A thin device of another size is ALREADY_EXISTS; a pool
// that is being deleted is ABORTED, so the caller retries once the drain
// has removed the name.
func (c *controller) ensureThinDevice(
	ctx context.Context,
	cluster string,
	name string,
	size uint64,
) (uint64, error) {
	gctx, cancel := c.gwCtx(ctx)
	reply, err := c.gw.CreateThinDevice(gctx, &pb.CreateThinDeviceRequest{
		ClusterName: cluster,
		SpName:      name,
		TdName:      name,
		Size:        size,
	})
	cancel()
	switch codeOf(err) {
	case codes.OK:
		return reply.GetTdId(), nil
	case codes.AlreadyExists:
	case codes.FailedPrecondition:
		return 0, status.Errorf(codes.Aborted,
			"volume %q is still being deleted; retry later", name)
	default:
		return 0, fromGateway("CreateThinDevice", err)
	}
	gctx, cancel = c.gwCtx(ctx)
	list, err := c.gw.ListThinDevices(gctx, &pb.ListThinDevicesRequest{
		ClusterName: cluster,
		SpName:      name,
	})
	cancel()
	if err != nil {
		return 0, fromGateway("ListThinDevices", err)
	}
	td := list.GetNameToTd()[name]
	if td == nil {
		return 0, status.Errorf(codes.Aborted,
			"volume %q: its thin device vanished between two reads", name)
	}
	if td.GetSize() != size {
		return 0, status.Errorf(codes.AlreadyExists,
			"volume %q exists with %d bytes, not %d", name, td.GetSize(), size)
	}
	return td.GetTdId(), nil
}

// ensureNamespace creates the volume's one namespace or checks the one
// there: it must back the volume's thin device under the volume's uuid.
func (c *controller) ensureNamespace(
	ctx context.Context,
	cluster string,
	name string,
	nqn string,
	uuid string,
	tdId uint64,
) error {
	gctx, cancel := c.gwCtx(ctx)
	_, err := c.gw.CreateNamespace(gctx, &pb.CreateNamespaceRequest{
		ClusterName: cluster,
		SpName:      name,
		Nqn:         nqn,
		NsIdx:       nsIdx,
		DevUuid:     uuid,
		TdName:      name,
	})
	cancel()
	switch codeOf(err) {
	case codes.OK:
		return nil
	case codes.InvalidArgument, codes.AlreadyExists:
		// The index is taken: the namespace is there from an earlier
		// attempt, or it is not ours.
	default:
		return fromGateway("CreateNamespace", err)
	}
	ss, err := c.subsystem(ctx, cluster, name, nqn)
	if err != nil {
		return err
	}
	ns := namespaceOf(ss)
	if ns == nil || ns.GetTdId() != tdId ||
		!strings.EqualFold(ns.GetDevUuid(), uuid) {
		return status.Errorf(codes.AlreadyExists,
			"volume %q: its subsystem holds a namespace that is not the "+
				"volume's", name)
	}
	return nil
}

// subsystem reads a volume's subsystem record. A pool or a subsystem that
// is gone is NOT_FOUND.
func (c *controller) subsystem(
	ctx context.Context,
	cluster string,
	name string,
	nqn string,
) (*pb.Subsystem, error) {
	gctx, cancel := c.gwCtx(ctx)
	defer cancel()
	reply, err := c.gw.ListSubsystems(gctx, &pb.ListSubsystemsRequest{
		ClusterName: cluster,
		SpName:      name,
	})
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound,
				"volume %q does not exist", name)
		}
		return nil, fromGateway("ListSubsystems", err)
	}
	ss := reply.GetNqnToSubsystem()[nqn]
	if ss == nil {
		return nil, status.Errorf(codes.NotFound,
			"volume %q has no subsystem", name)
	}
	return ss, nil
}

// namespaceOf is the volume's one namespace of a subsystem, or nil.
func namespaceOf(ss *pb.Subsystem) *pb.Namespace {
	for _, ns := range ss.GetNsList() {
		if ns.GetNsIdx() == nsIdx {
			return ns
		}
	}
	return nil
}

// DeleteVolume removes the volume's objects in the reverse order of their
// creation, each step repeatable: the namespace, the subsystem and the thin
// device, then the pool, whose delete the gateway refuses while any
// user-created object remains and answers done when the pool is already
// deleting; the worker drains the rest. A pool, a subsystem or a thin
// device that is already gone is done, and so is an id that names no
// volume of this driver. A write refused because the pool is already
// deleting is done too: the drain is running.
func (c *controller) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest,
) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	done := &csi.DeleteVolumeResponse{}
	cluster, name, err := parseVolumeId(req.GetVolumeId())
	if err != nil {
		return done, nil
	}
	nqn := subsystemNqnOf(name)

	gctx, cancel := c.gwCtx(ctx)
	_, err = c.gw.DeleteNamespace(gctx, &pb.DeleteNamespaceRequest{
		ClusterName: cluster,
		SpName:      name,
		Nqn:         nqn,
		NsIdx:       nsIdx,
	})
	cancel()
	switch codeOf(err) {
	case codes.OK, codes.NotFound:
	case codes.FailedPrecondition:
		// The pool is already deleting: the drain is running.
		return done, nil
	default:
		return nil, fromGateway("DeleteNamespace", err)
	}

	gctx, cancel = c.gwCtx(ctx)
	_, err = c.gw.DeleteSubsystem(gctx, &pb.DeleteSubsystemRequest{
		ClusterName: cluster,
		SpName:      name,
		Nqn:         nqn,
	})
	cancel()
	switch codeOf(err) {
	case codes.OK, codes.NotFound:
	case codes.FailedPrecondition:
		return nil, status.Errorf(codes.Aborted,
			"volume %q: its subsystem still holds a namespace; retry later", name)
	default:
		return nil, fromGateway("DeleteSubsystem", err)
	}

	gctx, cancel = c.gwCtx(ctx)
	_, err = c.gw.DeleteThinDevice(gctx, &pb.DeleteThinDeviceRequest{
		ClusterName: cluster,
		SpName:      name,
		TdName:      name,
	})
	cancel()
	switch codeOf(err) {
	case codes.OK, codes.NotFound:
	case codes.FailedPrecondition:
		return nil, status.Errorf(codes.Aborted,
			"volume %q: its thin device is still in use; retry later", name)
	default:
		return nil, fromGateway("DeleteThinDevice", err)
	}

	gctx, cancel = c.gwCtx(ctx)
	defer cancel()
	_, err = c.gw.DeleteStoragePool(gctx, &pb.DeleteStoragePoolRequest{
		ClusterName: cluster,
		SpName:      name,
	})
	switch codeOf(err) {
	case codes.OK, codes.NotFound:
		return done, nil
	case codes.FailedPrecondition:
		// Something else was created in the volume's pool by hand.
		return nil, fromGateway("DeleteStoragePool", err)
	default:
		return nil, fromGateway("DeleteStoragePool", err)
	}
}

// ControllerPublishVolume attaches a volume to one node: it sets the
// subsystem's allowed hosts to the node's host NQN alone (csi.md, Attach
// and detach). The node already alone in the list is done; another node in
// it is FAILED_PRECONDITION, the spec's word for a volume published
// elsewhere under single-node access. The publish context carries what the
// node plugin needs: the subsystem NQN, the namespace uuid and the cdc
// addresses.
func (c *controller) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest,
) (*csi.ControllerPublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	node := req.GetNodeId()
	if node == "" {
		return nil, status.Error(codes.InvalidArgument, "the node id is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument,
			"the volume capability is required")
	}
	if err := checkCapability(req.GetVolumeCapability()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	cluster, name, err := parseVolumeId(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if !isNodeId(node) {
		return nil, status.Errorf(codes.NotFound,
			"node %q is unknown to this driver: a node id is a host NQN", node)
	}
	nqn := subsystemNqnOf(name)
	ss, err := c.subsystem(ctx, cluster, name, nqn)
	if err != nil {
		return nil, err
	}
	ns := namespaceOf(ss)
	if ns == nil {
		return nil, status.Errorf(codes.NotFound,
			"volume %q has no namespace", name)
	}
	hosts := ss.GetAllowedHosts()
	switch {
	case len(hosts) == 1 && hosts[0] == node:
	case len(hosts) == 0:
		gctx, cancel := c.gwCtx(ctx)
		_, err = c.gw.UpdateSubsystemHosts(gctx, &pb.UpdateSubsystemHostsRequest{
			ClusterName:  cluster,
			SpName:       name,
			Nqn:          nqn,
			AllowedHosts: []string{node},
		})
		cancel()
		if err != nil {
			if codeOf(err) == codes.NotFound {
				return nil, status.Errorf(codes.NotFound,
					"volume %q does not exist", name)
			}
			return nil, fromGateway("UpdateSubsystemHosts", err)
		}
	default:
		others := make([]string, 0, len(hosts))
		for _, h := range hosts {
			if h != node {
				others = append(others, h)
			}
		}
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %q is published to node %s", name,
			strings.Join(others, cdcSep))
	}
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			ctxNqn:  nqn,
			ctxUuid: ns.GetDevUuid(),
			ctxCdc:  strings.Join(c.cdc, cdcSep),
		},
	}, nil
}

// ControllerUnpublishVolume detaches a volume from a node, or from every
// node when none is named: it takes the node out of the subsystem's allowed
// hosts, which empties the list. A node not in the list, a subsystem or a
// pool that is gone, and an id that names no volume of this driver are all
// done.
func (c *controller) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest,
) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	done := &csi.ControllerUnpublishVolumeResponse{}
	cluster, name, err := parseVolumeId(req.GetVolumeId())
	if err != nil {
		return done, nil
	}
	nqn := subsystemNqnOf(name)
	ss, err := c.subsystem(ctx, cluster, name, nqn)
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return done, nil
		}
		return nil, err
	}
	node := req.GetNodeId()
	var hosts []string
	found := node == ""
	for _, h := range ss.GetAllowedHosts() {
		if h == node {
			found = true
			continue
		}
		if node != "" {
			hosts = append(hosts, h)
		}
	}
	if !found || len(hosts) == len(ss.GetAllowedHosts()) {
		return done, nil
	}
	gctx, cancel := c.gwCtx(ctx)
	defer cancel()
	_, err = c.gw.UpdateSubsystemHosts(gctx, &pb.UpdateSubsystemHostsRequest{
		ClusterName:  cluster,
		SpName:       name,
		Nqn:          nqn,
		AllowedHosts: hosts,
	})
	if err != nil && codeOf(err) != codes.NotFound {
		return nil, fromGateway("UpdateSubsystemHosts", err)
	}
	return done, nil
}

// ValidateVolumeCapabilities confirms the capabilities the driver serves
// for a volume that exists; a capability it does not serve leaves the reply
// unconfirmed with the reason.
func (c *controller) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"volume capabilities are required")
	}
	cluster, name, err := parseVolumeId(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	gctx, cancel := c.gwCtx(ctx)
	_, err = c.gw.GetStoragePool(gctx, &pb.GetStoragePoolRequest{
		ClusterName: cluster,
		SpName:      name,
	})
	cancel()
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound,
				"volume %q does not exist", name)
		}
		return nil, fromGateway("GetStoragePool", err)
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if err := checkCapability(cap); err != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: err.Error(),
			}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

// GetCapacity reports what the class's cluster can still promise (csi.md,
// Capacity), from the cluster capacity call's records.
func (c *controller) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest,
) (*csi.GetCapacityResponse, error) {
	p, err := parseParams(req.GetParameters())
	if err != nil {
		return nil, err
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if err := checkCapability(cap); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	gctx, cancel := c.gwCtx(ctx)
	defer cancel()
	reply, err := c.gw.GetClusterCapacity(gctx, &pb.GetClusterCapacityRequest{
		ClusterName: p.cluster,
	})
	if err != nil {
		if codeOf(err) == codes.NotFound {
			// A cluster that does not exist has nothing free (csi.md,
			// Capacity): Kubernetes asks on a timer, and a create is
			// where a class that names a wrong cluster fails.
			return &csi.GetCapacityResponse{}, nil
		}
		return nil, fromGateway("GetClusterCapacity", err)
	}
	available, maxVolume := capacityOf(reply, p.legs(), p.cntlrCnt())
	resp := &csi.GetCapacityResponse{AvailableCapacity: int64(available)}
	if maxVolume > 0 {
		resp.MaximumVolumeSize = wrapperspb.Int64(int64(maxVolume))
	}
	return resp, nil
}

// ControllerGetCapabilities advertises what the controller plugin does:
// create and delete, publish and unpublish, and get capacity.
func (c *controller) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest,
) (*csi.ControllerGetCapabilitiesResponse, error) {
	var caps []*csi.ControllerServiceCapability
	for _, t := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	} {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}
