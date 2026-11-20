package csi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/distributed-nvme/distributed-nvme/common"
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

// The detach's and the attach's wait for the primary's cut (csi.md CSI10,
// CSI7).
const (
	// detachWaitMax is the one bound of a detach or an attach, from the
	// call's start, when the caller's deadline is later or absent: every
	// attempt and every wait of the call share it. Kubernetes' attacher
	// gives every call a deadline of its own, far shorter, so this bound is
	// for a caller with none, a tool or the conformance test, whose attach
	// right after a create waits for the primary's first converge at the
	// pool's revision, and reads and waits again at each move the worker
	// makes to that revision while it builds the pool.
	detachWaitMax = 2 * time.Minute
	// detachPollPeriod is the least time between two polls of one call,
	// whatever ended its attempts. A poll is one GetStoragePool and at most
	// one InspectCntlr, of the primary the pool names, when that read does
	// not end the wait (the pool gone or deleting, the subsystem off its
	// list, or the pool past an attach's revision): the inspect makes the
	// primary's agent read its devices' counters, and each whole reply is
	// logged at Info, in the driver and in the gateway.
	detachPollPeriod = time.Second
)

// controller is the CSI controller service (csi.md, The volume model; Attach
// and detach; Capacity): a gRPC client of the gateway.
type controller struct {
	csi.UnimplementedControllerServer
	gw      pb.GatewayClient
	timeout time.Duration
	cdc     []string
	// detachWait and detachPoll are detachWaitMax and detachPollPeriod,
	// which tests set short.
	detachWait time.Duration
	detachPoll time.Duration
}

func newController(gw pb.GatewayClient, cfg Config) *controller {
	timeout := cfg.GatewayTimeout
	if timeout <= 0 {
		timeout = DefaultGatewayTimeout
	}
	return &controller{
		gw:         gw,
		timeout:    timeout,
		cdc:        cfg.CdcAddresses,
		detachWait: detachWaitMax,
		detachPoll: detachPollPeriod,
	}
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
	// The cluster's stripe is the pool's: the pool below has one slice and
	// names no stripe, and CreateStoragePool narrows a cluster's stripe only
	// for a pool whose stripe times its slice count would pass
	// common.MaxNvmeTcpCmdBytes (model.FitStripe), which one slice of at most
	// MaxDmRaid0StripeSize never does.
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
	// cntlr count, a first data group of one part in volumeSteps of the
	// volume, dnv's default auto-grow mark whatever the cluster stores,
	// since the volume model needs auto-grow on, and the cluster's defaults
	// for the rest.
	gctx, cancel := c.gwCtx(ctx)
	_, err = c.gw.CreateStoragePool(gctx, &pb.CreateStoragePoolRequest{
		ClusterName: p.cluster,
		SpName:      name,
		BdevConf: &pb.BdevConf{
			RedundConf: p.redundConf(),
			DmPoolConf: &pb.DmPoolConf{
				LowWaterMarkPct: common.DefaultPoolLowWatermarkPct,
			},
		},
		CntlrCnt:   p.cntlrs,
		SliceCnt:   1,
		InitExtCnt: initExtCnt(size, extent),
	})
	cancel()
	if err != nil && codeOf(err) != codes.AlreadyExists {
		return nil, fromGateway("CreateStoragePool", err)
	}
	if err != nil {
		if err := c.checkPool(ctx, p, name, unit); err != nil {
			return nil, err
		}
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

// checkPool reads back the pool a repeated create found under the volume's
// name (csi.md CSI9): one still being deleted is ABORTED, so the caller
// retries once the drain has removed the name, and one of another
// redundancy, of a slice count other than one or of a stripe other than
// the cluster's, unit, is ALREADY_EXISTS: the volume's size is counted in
// units of one slice's stripe. Its cntlr count is not compared, since an
// operator may add or delete cntlrs.
func (c *controller) checkPool(ctx context.Context, p params, name string, unit uint64) error {
	gctx, cancel := c.gwCtx(ctx)
	reply, err := c.gw.GetStoragePool(gctx, &pb.GetStoragePoolRequest{
		ClusterName: p.cluster,
		SpName:      name,
	})
	cancel()
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return status.Errorf(codes.Aborted,
				"volume %q went away under the create; retry", name)
		}
		return fromGateway("GetStoragePool", err)
	}
	if reply.GetSpConf().GetDeleting() {
		return status.Errorf(codes.Aborted,
			"volume %q is still being deleted; retry later", name)
	}
	conf := reply.GetSpConf()
	_, none := conf.GetBdevConf().GetRedundConf().GetRedunKind().(*pb.RedundConf_RedundNone)
	if none != (p.redundancy == redundancyNone) {
		return status.Errorf(codes.AlreadyExists,
			"volume %q exists with another redundancy", name)
	}
	if n := len(conf.GetSliceIdList()); n != 1 {
		return status.Errorf(codes.AlreadyExists,
			"volume %q exists with %d slices, not 1", name, n)
	}
	if stripe := conf.GetBdevConf().GetDmRaid0Conf().GetStripeSize(); stripe != unit {
		return status.Errorf(codes.AlreadyExists,
			"volume %q exists with a stripe of %d bytes, not the cluster's %d",
			name, stripe, unit)
	}
	return nil
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
// volume of this driver. A namespace delete the gateway refuses is done
// when the pool is already deleting or gone, since the drain is running;
// otherwise a transfer names the namespace, and the refusal is passed on.
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
		// Either the pool is already deleting, and the drain is running, or
		// a transfer names the namespace as its origin, which is the
		// operator's to end first (csi.md CSI9). Only the pool tells which.
		deleting, perr := c.poolDeletingOrGone(ctx, cluster, name)
		if perr != nil {
			return nil, perr
		}
		if deleting {
			return done, nil
		}
		return nil, fromGateway("DeleteNamespace", err)
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

// poolDeletingOrGone reads the volume's pool: true when it is already
// deleting or no longer there.
func (c *controller) poolDeletingOrGone(
	ctx context.Context,
	cluster string,
	name string,
) (bool, error) {
	gctx, cancel := c.gwCtx(ctx)
	defer cancel()
	reply, err := c.gw.GetStoragePool(gctx, &pb.GetStoragePoolRequest{
		ClusterName: cluster,
		SpName:      name,
	})
	switch codeOf(err) {
	case codes.OK:
		return reply.GetSpConf().GetDeleting(), nil
	case codes.NotFound:
		return true, nil
	default:
		return false, fromGateway("GetStoragePool", err)
	}
}

// ControllerPublishVolume attaches a volume to one node: it sets the
// subsystem's allowed hosts to the node's host NQN alone (csi.md, Attach
// and detach). The node already alone in the list is done; another node in
// it is FAILED_PRECONDITION, the spec's word for a volume published
// elsewhere under single-node access; an empty list is written once the
// primary's cut at the revision read has held (awaitCut). The publish
// context carries what the node plugin needs, the subsystem NQN and the
// namespace uuid, and the cdc addresses, which the node plugin does not
// read (csi.md CSI10).
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
	// One bound for the whole call, every attempt and every wait of it.
	ctx, cancel := context.WithTimeout(ctx, c.detachWait)
	defer cancel()
	// The write that lets the node in waits for the primary's cut at the
	// revision its attempt read: a host the list held before, which a
	// detach took off, may still hold an nvmet connection, and Kubernetes'
	// forced detach attaches the next node once a detach call has failed,
	// while that detach's own wait still runs (csi.md CSI10).
	what := "the attach to node " + node
	st := &cutState{}
	beforeWrite := func(ss *pb.Subsystem, rev uint64) error {
		return c.awaitCut(ctx, cluster, name, nqn, what,
			cutWait{ssId: ss.GetSsId(), rev: rev, revKnown: true, exact: true}, st)
	}
	res, err := c.updateHosts(ctx, cluster, name, nqn, beforeWrite,
		func(ss *pb.Subsystem) ([]string, bool, error) {
			if namespaceOf(ss) == nil {
				return nil, false, status.Errorf(codes.NotFound,
					"volume %q has no namespace", name)
			}
			hosts := ss.GetAllowedHosts()
			switch {
			case len(hosts) == 1 && hosts[0] == node:
				return nil, false, nil
			case len(hosts) == 0:
				return []string{node}, true, nil
			}
			others := make([]string, 0, len(hosts))
			for _, h := range hosts {
				if h != node {
					others = append(others, h)
				}
			}
			return nil, false, status.Errorf(codes.FailedPrecondition,
				"volume %q is published to node %s", name,
				strings.Join(others, cdcSep))
		})
	if err != nil {
		return nil, c.callErr(ctx, name, what, st, err)
	}
	ns := namespaceOf(res.ss)
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
// hosts, which empties the list, and then waits for the primary's cut
// (awaitCut). A node not in the list writes nothing and waits all the same.
// A subsystem or a pool that is gone, an id that names no volume of this
// driver and a node id that is no host NQN are done at once.
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
	node := req.GetNodeId()
	// One bound for the whole call, its write's attempts and its wait.
	ctx, cancel := context.WithTimeout(ctx, c.detachWait)
	defer cancel()
	what := "the detach from node " + node
	if node == "" {
		what = "the detach from every node"
	}
	st := &cutState{}
	res, err := c.updateHosts(ctx, cluster, name, nqn, nil,
		func(ss *pb.Subsystem) ([]string, bool, error) {
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
				return nil, false, nil
			}
			return hosts, true, nil
		})
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return done, nil
		}
		return nil, c.callErr(ctx, name, what, st, err)
	}
	if node != "" && !isNodeId(node) {
		// No attach of this driver put such an id on a list, so no
		// connection of it waits for a cut.
		return done, nil
	}
	w := cutWait{ssId: res.ss.GetSsId(), rev: res.rev, revKnown: res.wrote}
	if err := c.awaitCut(ctx, cluster, name, nqn, what, w, st); err != nil {
		return nil, c.callErr(ctx, name, what, st, err)
	}
	return done, nil
}

// awaitCut is a detach's or an attach's wait for the primary's cut (csi.md
// CSI10; cnagent.md CN33): it polls until a report of the pool's primary
// cntlr reads the subsystem's row OK at the wait's revision, which an agent
// that cuts answers once no host off the list keeps an nvmet connection to
// the subsystem. A detach's revision is the one its write made, and a
// report at a later one counts too, since the worker sends the primary the
// latest revision alone (dnv-worker.md RW3); a detach that wrote nothing
// takes the pool's revision its first poll reads, after the subsystem read,
// so a change that landed between the two reads, another detach of the
// node among them, is in it. A pool or a subsystem that is gone and a pool
// being deleted, which holds no subsystem, end a detach's wait as done. An
// attach's revision is the one its attempt read before its write, whose
// list is empty, and its wait is exact: a poll that reads the pool, or a
// report, past that revision, or the pool or its subsystem gone, or the
// pool deleting, ends the wait as errPoolMoved, since the write that would
// follow has no OK report at its token's revision; the attach's next
// attempt reads and decides again. Polls of one call keep detachPoll apart
// across its attempts (st). Anything else polls again, until ctx ends,
// which is the call's one bound (the caller's deadline or detachWait from
// the call's start, whichever comes first): errBoundEnded, which the call
// answers as callErr does. Standbys are not asked: they serve no IO, and a
// standby that becomes the primary cuts before it serves.
func (c *controller) awaitCut(
	ctx context.Context,
	cluster string,
	name string,
	nqn string,
	what string,
	w cutWait,
	st *cutState,
) error {
	timer := time.NewTimer(st.untilNextPoll(c.detachPoll))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return errBoundEnded
		case <-timer.C:
		}
		st.lastPoll = time.Now()
		res, seen, failed := c.pollCut(ctx, cluster, name, nqn, &w)
		if failed {
			st.failure = seen
		} else if seen != "" {
			st.answer, st.failure = seen, ""
		}
		switch res {
		case pollDone:
			return nil
		case pollMoved:
			return errPoolMoved
		}
		timer.Reset(c.detachPoll)
	}
}

// cutState is what the waits of one call carry from attempt to attempt:
// what the last poll the gateway answered saw, and the gateway call that
// failed after it, if one did, for the bound's error, and the time of the
// last poll, so that no two polls of the call come closer than the poll
// period, however its attempts end.
type cutState struct {
	answer   string
	failure  string
	lastPoll time.Time
}

// untilNextPoll is the pause before a wait's first poll: none for the
// call's first poll, else what is left of the poll period since the last.
func (st *cutState) untilNextPoll(period time.Duration) time.Duration {
	if st.lastPoll.IsZero() {
		return 0
	}
	return max(0, period-time.Since(st.lastPoll))
}

// last is the state the bound's error names: the last answer and, after
// it, a gateway call that failed; the wait's own end can cut the last
// poll's call short, and the state before it is the one worth naming.
func (st *cutState) last() string {
	switch {
	case st.answer == "" && st.failure == "":
		return "no poll ran"
	case st.answer == "":
		return st.failure
	case st.failure != "":
		return st.answer + "; then " + st.failure
	}
	return st.answer
}

// errBoundEnded is a wait that its call's bound ended.
var errBoundEnded = errors.New("the call's bound ended the wait")

// callErr is a detach's or an attach's answer for an error of its write or
// its wait. Once the call's bound has ended, on every path, it is the
// bound's own: DEADLINE_EXCEEDED, or CANCELED when the caller ended the
// call, naming the volume, what waited (what, as "the detach from node X")
// and the last state the polls saw, and after it any error that ended the
// call; before the bound, any other error passes as it is.
func (c *controller) callErr(
	ctx context.Context,
	name string,
	what string,
	st *cutState,
	err error,
) error {
	// A gateway call can fail at the deadline a moment before ctx says so.
	d, ok := ctx.Deadline()
	ended := ctx.Err() != nil || (ok && !time.Now().Before(d))
	if !ended && !errors.Is(err, errBoundEnded) {
		return err
	}
	if !errors.Is(err, errBoundEnded) {
		st.failure = err.Error()
	}
	code := codes.DeadlineExceeded
	if ctx.Err() != nil {
		code = status.FromContextError(ctx.Err()).Code()
	}
	return status.Errorf(code, "volume %q: %s ran out of time; last seen: %s",
		name, what, st.last())
}

// errPoolMoved ends an attach's exact wait that cannot end OK at its
// attempt's revision: the pool or a report past it, or the pool or its
// subsystem gone, or the pool deleting; the attach reads and decides
// again.
var errPoolMoved = errors.New("the pool moved past the attempt's revision")

// cutWait is what a detach's or an attach's wait polls for: the
// subsystem's row and the revision a report must be at or past, known once
// the call wrote or read the pool before its write, or else at its first
// poll's read of the pool; exact, an attach's, ends the wait as moved when
// it cannot end OK at that revision: the pool or a report past it, or the
// pool or its subsystem gone, or the pool deleting.
type cutWait struct {
	ssId     uint64
	rev      uint64
	revKnown bool
	exact    bool
}

// ended is what a poll that finds the pool or the subsystem gone, or the
// pool deleting, answers: a detach's wait is done, as what is gone serves
// no host, and an attach's exact wait has moved, so that its next attempt
// reads again and no write goes out with no OK report at its token.
func (w *cutWait) ended() pollResult {
	if w.exact {
		return pollMoved
	}
	return pollDone
}

// pollResult is what one poll of awaitCut found: poll again, the wait is
// done, or an exact wait has moved (cutWait).
type pollResult int

const (
	pollAgain pollResult = iota
	pollDone
	pollMoved
)

// pollCut is one poll of awaitCut: the pool, its primary, and the
// primary's report. It answers what it found, the state it saw, and
// whether that is a gateway call that failed. An exact wait, an attach's,
// answers pollMoved where a detach's is done (the pool or the subsystem
// gone, the pool deleting), and for a pool or a report past its revision.
func (c *controller) pollCut(
	ctx context.Context,
	cluster string,
	name string,
	nqn string,
	w *cutWait,
) (pollResult, string, bool) {
	gctx, cancel := c.gwCtx(ctx)
	pool, err := c.gw.GetStoragePool(gctx, &pb.GetStoragePoolRequest{
		ClusterName: cluster,
		SpName:      name,
	})
	cancel()
	if err != nil {
		if codeOf(err) == codes.NotFound {
			return w.ended(), "the pool is gone", false
		}
		return pollAgain, fromGateway("GetStoragePool", err).Error(), true
	}
	conf := pool.GetSpConf()
	if conf.GetDeleting() {
		return w.ended(), "the pool is being deleted", false
	}
	if !slices.Contains(conf.GetNqnList(), nqn) {
		return w.ended(), "the pool holds no such subsystem", false
	}
	if !w.revKnown {
		w.rev, w.revKnown = pool.GetSpRev().GetRevision(), true
	}
	if w.exact && pool.GetSpRev().GetRevision() > w.rev {
		return pollMoved, fmt.Sprintf("the pool moved from revision %d to "+
			"%d", w.rev, pool.GetSpRev().GetRevision()), false
	}
	id, ok := primaryOf(pool)
	if !ok {
		return pollAgain, fmt.Sprintf("the pool has no primary cntlr; the "+
			"wait is for revision %d", w.rev), false
	}
	gctx, cancel = c.gwCtx(ctx)
	reply, err := c.gw.InspectCntlr(gctx, &pb.InspectCntlrRequest{
		ClusterName: cluster,
		SpName:      name,
		CntlrId:     id,
	})
	cancel()
	if err != nil {
		return pollAgain, fmt.Sprintf("primary cntlr %d: %v; the wait is "+
			"for revision %d", id, fromGateway("InspectCntlr", err),
			w.rev), true
	}
	got := reply.GetAppliedRevision()
	if w.exact && got > w.rev {
		return pollMoved, fmt.Sprintf("primary cntlr %d reported revision "+
			"%d, past the attempt's %d", id, got, w.rev), false
	}
	if got < w.rev {
		return pollAgain, fmt.Sprintf("primary cntlr %d reported revision %d; "+
			"the wait is for revision %d", id, got, w.rev), false
	}
	row := reply.GetCntlrInfo().GetSsIdToSubsystem()[w.ssId]
	if row == nil {
		return pollAgain, fmt.Sprintf("primary cntlr %d reported no row of the "+
			"subsystem at revision %d", id, got), false
	}
	if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		return pollAgain, fmt.Sprintf("primary cntlr %d at revision %d reads "+
			"the subsystem %s: %s", id, got, statusWord(row.GetStatus()),
			row.GetDetails()), false
	}
	return pollDone, fmt.Sprintf("primary cntlr %d at revision %d reads the "+
		"subsystem ok", id, got), false
}

// primaryOf is the id of a pool's primary cntlr: its cntlr list paired by
// index with the conf's cntlr id list, as the gateway pairs them
// (primaryCntlr, gateway/common.go).
func primaryOf(pool *pb.GetStoragePoolReply) (uint64, bool) {
	ids := pool.GetSpConf().GetCntlrIdList()
	for i, cntlr := range pool.GetCntlrList() {
		if cntlr.GetPrimary() && i < len(ids) {
			return ids[i], true
		}
	}
	return 0, false
}

// statusWord is a row status as dnvctl prints it: "pending" for
// RES_STATUS_PENDING.
func statusWord(st pb.ResStatus) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "RES_STATUS_"))
}

// hostsAttempts bounds updateHosts' attempts of a write that waits for
// nothing: a pool that moves under every one of them is ABORTED, which the
// caller retries. An attach whose write waits for the cut is bounded by its
// call's one bound instead: its attempts end on every move of the pool's
// revision, which the worker makes several times during a pool's first
// build.
const hostsAttempts = 4

// gwStaleRevision is the gateway's fixed message for a token that no longer
// matches the pool's revision (gateway.md GW6), the one refusal updateHosts
// runs again; any other ABORTED is passed on with its own message.
const gwStaleRevision = "stale revision"

// hostsResult is what updateHosts did: the subsystem the deciding attempt
// read and, when it wrote, the pool revision the write made, which is the
// token's plus one: a write the token check let through found the stored
// revision equal to the token, and the write bumps it once
// (architecture.md, Revision keys and the sync fan-out).
type hostsResult struct {
	ss    *pb.Subsystem
	wrote bool
	rev   uint64
}

// updateHosts is one attach's or detach's change of a subsystem's allowed
// hosts under the pool's revision token (csi.md CSI10; gateway.md GW6):
// each attempt reads the pool's revision, then the subsystem, then decide
// says the list to write, if any, beforeWrite, when set, runs with the
// subsystem and the revision read and may refuse, and the write carries
// that revision. A write the gateway refuses as stale ran over a change
// made after that read, another attach or detach among them, so the
// attempt runs again and decides afresh on what that change left; any
// other refusal is passed on. With beforeWrite set, a stale refusal and a
// beforeWrite that answers errPoolMoved start a new attempt that counts
// against no limit, so ctx alone bounds the call; without it, the attempts
// are hostsAttempts. A pool or subsystem that is gone is NOT_FOUND.
func (c *controller) updateHosts(
	ctx context.Context,
	cluster string,
	name string,
	nqn string,
	beforeWrite func(ss *pb.Subsystem, rev uint64) error,
	decide func(ss *pb.Subsystem) ([]string, bool, error),
) (hostsResult, error) {
	for attempt := 0; attempt < hostsAttempts; {
		gctx, cancel := c.gwCtx(ctx)
		pool, err := c.gw.GetStoragePool(gctx, &pb.GetStoragePoolRequest{
			ClusterName: cluster,
			SpName:      name,
		})
		cancel()
		if err != nil {
			if codeOf(err) == codes.NotFound {
				return hostsResult{}, status.Errorf(codes.NotFound,
					"volume %q does not exist", name)
			}
			return hostsResult{}, fromGateway("GetStoragePool", err)
		}
		ss, err := c.subsystem(ctx, cluster, name, nqn)
		if err != nil {
			return hostsResult{}, err
		}
		hosts, write, err := decide(ss)
		if err != nil {
			return hostsResult{}, err
		}
		if !write {
			return hostsResult{ss: ss}, nil
		}
		if beforeWrite != nil {
			err := beforeWrite(ss, pool.GetSpRev().GetRevision())
			if errors.Is(err, errPoolMoved) {
				continue
			}
			if err != nil {
				return hostsResult{}, err
			}
		}
		gctx, cancel = c.gwCtx(ctx)
		_, err = c.gw.UpdateSubsystemHosts(gctx, &pb.UpdateSubsystemHostsRequest{
			ClusterName:  cluster,
			SpName:       name,
			SpRev:        pool.GetSpRev(),
			Nqn:          nqn,
			AllowedHosts: hosts,
		})
		cancel()
		switch codeOf(err) {
		case codes.OK:
			return hostsResult{
				ss:    ss,
				wrote: true,
				rev:   pool.GetSpRev().GetRevision() + 1,
			}, nil
		case codes.Aborted:
			if status.Convert(err).Message() == gwStaleRevision {
				if beforeWrite == nil {
					attempt++
				}
				continue
			}
			return hostsResult{}, fromGateway("UpdateSubsystemHosts", err)
		case codes.NotFound:
			return hostsResult{}, status.Errorf(codes.NotFound,
				"volume %q does not exist", name)
		default:
			return hostsResult{}, fromGateway("UpdateSubsystemHosts", err)
		}
	}
	return hostsResult{}, status.Errorf(codes.Aborted,
		"volume %q: its pool changed under every one of %d attempts; retry",
		name, hostsAttempts)
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
// Capacity): the gateway's space answer for a pool of the class's shape, its
// legs, its cntlr count as the class states it and one slice, converted to
// the volume model.
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
		LegCnt:      uint32(p.legs()),
		CntlrCnt:    p.cntlrs,
		SliceCnt:    1,
	})
	if err != nil {
		// A cluster that does not exist has nothing free (csi.md,
		// Capacity): Kubernetes asks on a timer, and a create is where a
		// class that names a wrong cluster fails.
		if codeOf(err) == codes.NotFound {
			return &csi.GetCapacityResponse{
				MaximumVolumeSize: wrapperspb.Int64(0),
			}, nil
		}
		return nil, fromGateway("GetClusterCapacity", err)
	}
	// A gateway that gives no space answer is older than the driver: an
	// error, so Kubernetes keeps what it last read, where a zero would
	// stop every claim of the class.
	if reply.GetSpace() == nil {
		return nil, status.Error(codes.Unavailable,
			"gateway GetClusterCapacity: the reply carries no space "+
				"answer; is the gateway older than the driver?")
	}
	// The largest volume is always set, zero included: an unset one means
	// no bound per volume.
	available, maxVolume := capacityOf(reply.GetSpace(), reply.GetExtentSize())
	return &csi.GetCapacityResponse{
		AvailableCapacity: int64(available),
		MaximumVolumeSize: wrapperspb.Int64(int64(maxVolume)),
	}, nil
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
