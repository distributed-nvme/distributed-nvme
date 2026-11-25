package csi

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// node is the CSI node service (csi.md, The node plugin): it stages a
// volume by connecting the node to its subsystem through the follower and
// waiting for the device, formats a blank device and mounts it, publishes
// the staged volume to a pod as a filesystem or a block device, and reports
// the used space. It never talks to the gateway: the subsystem NQN and the
// namespace uuid come in the publish context, the rest it derives from the
// volume id, and the cdcs it follows come from its own configuration
// (csi.md CSI15).
type node struct {
	csi.UnimplementedNodeServer
	id         hostIdentity
	host       *nvmeHost
	follow     *follower
	mnt        *mounter
	deviceWait time.Duration
	// busy holds, by subsystem NQN, the volumes whose stage or unstage
	// runs, and which of the two, so that no second try of either runs
	// beside it (csi.md CSI16): a try whose command waits for IO keeps the
	// volume, and its next tries neither take another command slot nor drop
	// the paths it needs.
	busyMu sync.Mutex
	busy   map[string]string
}

// tryVolume claims a volume for one stage or unstage, op naming which;
// when one runs already, it returns false and what runs. The release must
// be called once the claim's call ends.
func (n *node) tryVolume(nqn string, op string) (func(), string, bool) {
	n.busyMu.Lock()
	defer n.busyMu.Unlock()
	if running, ok := n.busy[nqn]; ok {
		return nil, running, false
	}
	if n.busy == nil {
		n.busy = map[string]string{}
	}
	n.busy[nqn] = op
	return func() {
		n.busyMu.Lock()
		defer n.busyMu.Unlock()
		delete(n.busy, nqn)
	}, "", true
}

// opStage and opUnstage name the calls tryVolume claims for.
const (
	opStage   = "stage"
	opUnstage = "unstage"
)

// publishContext is what the node reads of the context the controller
// plugin handed it at attach: the subsystem NQN and the namespace uuid.
// The context's cdc addresses are not read: the node follows the cdcs of
// its own configuration alone (csi.md CSI15).
type publishContext struct {
	nqn  string
	uuid string
}

// readPublishContext reads the attach's context; a volume staged before a
// plugin restart carries it again on every call, so nothing is kept.
func readPublishContext(m map[string]string) (publishContext, error) {
	pc := publishContext{nqn: m[ctxNqn], uuid: strings.ToLower(m[ctxUuid])}
	if pc.nqn == "" || pc.uuid == "" {
		return publishContext{}, status.Error(codes.InvalidArgument,
			"the publish context must carry the subsystem NQN and the namespace uuid")
	}
	return pc, nil
}

// The one topology segment every node reports (csi.md, The node plugin). A
// dnv volume is reachable from every node, so the segment says nothing of
// its own; it exists because the provisioner sidecar deletes a capacity
// object whose node selector is empty as soon as it has made it, so a
// driver without a segment has no capacity objects at all.
const (
	topologyKey   = "topology.csi.dnv.io/segment"
	topologyValue = "all"
)

func (n *node) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: n.id.hostNqn,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{topologyKey: topologyValue},
		},
	}, nil
}

func (n *node) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	var caps []*csi.NodeServiceCapability
	for _, t := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	} {
		caps = append(caps, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: t},
			},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeStageVolume connects the node to the volume and, for a filesystem
// volume, formats a blank device and mounts it at the staging path (csi.md
// CSI14, CSI17). Every step reads the node first, so a repeat is done.
func (n *node) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument,
			"the staging target path is required")
	}
	cap := req.GetVolumeCapability()
	if cap == nil {
		return nil, status.Error(codes.InvalidArgument,
			"the volume capability is required")
	}
	if err := checkCapability(cap); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	pc, err := readPublishContext(req.GetPublishContext())
	if err != nil {
		return nil, err
	}
	release, running, ok := n.tryVolume(pc.nqn, opStage)
	if !ok {
		return nil, status.Errorf(codes.Aborted,
			"another %s of the volume of subsystem %q runs", running, pc.nqn)
	}
	defer release()

	// The connects, retried within the device wait until the node holds a
	// path to the subsystem: the cdc lists a subsystem before its cntlrs
	// have built it, and a cntlr that has not built it refuses the connect;
	// one still refusing then is the follower's loop's to retry, whose pass
	// that fails arms its retry timer (csi.md CSI14, CSI15).
	deadline := time.Now().Add(n.deviceWait)
	for {
		err := n.follow.stage(ctx, pc.nqn)
		if err == nil {
			break
		}
		if held, herr := n.follow.hasPath(ctx, pc.nqn); herr == nil && held {
			n.follow.trigger()
			break
		}
		if time.Now().After(deadline) {
			return nil, status.Errorf(codes.Unavailable,
				"connecting to %s: %v", pc.nqn, err)
		}
		select {
		case <-ctx.Done():
			return nil, status.Errorf(codes.Unavailable,
				"connecting to %s: %v", pc.nqn, ctx.Err())
		case <-time.After(connectRetry):
		}
	}
	remaining := time.Until(deadline)
	if remaining < connectRetry {
		remaining = connectRetry
	}
	dev, err := n.host.waitDevice(ctx, pc.uuid, remaining)
	if err != nil {
		// UNAVAILABLE: the kubelet stages the volume again or unstages it.
		return nil, err
	}
	n.follow.trigger()
	if err := n.stageDevice(ctx, staging, cap, dev); err != nil {
		n.forgetFailedStage(ctx, staging, pc.nqn, dev, err)
		return nil, err
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// stageDevice formats a blank device of a filesystem volume and mounts it
// at the staging path; a block volume is published straight from the
// device.
func (n *node) stageDevice(
	ctx context.Context,
	staging string,
	cap *csi.VolumeCapability,
	dev string,
) error {
	mnt, ok := cap.GetAccessType().(*csi.VolumeCapability_Mount)
	if !ok {
		return nil
	}
	fsType := mnt.Mount.GetFsType()
	if fsType == "" {
		fsType = fsExt4
	}
	entry, err := n.mnt.mountAt(ctx, staging)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if entry != nil {
		if entry.fsType != fsType {
			return status.Errorf(codes.AlreadyExists,
				"%s is staged with %s, not %s", staging, entry.fsType, fsType)
		}
		return nil
	}
	existing, ptType, err := n.mnt.probe(ctx, dev)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	switch {
	case existing == "" && ptType != "":
		return status.Errorf(codes.FailedPrecondition,
			"the device of the volume carries a %s partition table and no "+
				"filesystem; the driver never formats over it", ptType)
	case existing == "":
		if err := n.proveBlank(ctx, dev); err != nil {
			return err
		}
		if err := n.mnt.mkfs(ctx, fsType, dev); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	case existing == fsType:
	default:
		return status.Errorf(codes.FailedPrecondition,
			"the device of the volume carries %s, not %s; the driver never "+
				"reformats", existing, fsType)
	}
	if err := n.mnt.mkdir(ctx, staging); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if err := n.mnt.mount(ctx, dev, staging, fsType, mnt.Mount.GetMountFlags()); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

// proveBlank is CSI17's proof that a device blkid finds nothing on is blank:
// its start, read past the page cache, is all zero, as a thin device reads
// where nobody wrote. blkid gives the same silent answer when its read
// failed, so that answer alone formats nothing. A read that fails is
// UNAVAILABLE, a code after which the kubelet stages the volume again and
// the follower keeps its paths (CSI16); a byte that is not zero is
// FAILED_PRECONDITION, as any other content the driver will not format over.
func (n *node) proveBlank(ctx context.Context, dev string) error {
	start, err := n.mnt.readStart(ctx, dev)
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"reading the start of the device %s: %v; the driver formats only "+
				"a device whose start it has read", dev, err)
	}
	if len(start) == 0 {
		return status.Errorf(codes.FailedPrecondition,
			"the device %s is smaller than one 4 KiB block; the driver "+
				"formats no device so small", dev)
	}
	for i, b := range start {
		if b != 0 {
			return status.Errorf(codes.FailedPrecondition,
				"the device %s of the volume holds data at byte %d of its "+
					"first %d bytes and no filesystem or partition table "+
					"blkid knows; the driver never formats over it", dev, i,
				len(start))
		}
	}
	return nil
}

// forgetFailedStage undoes the follower's part of a stage that failed with a
// final code (csi.md CSI16): after one the kubelet counts the volume as not
// staged and never unstages it, so the subsystem leaves the staged set here,
// its paths with it. A code after which the stage may still be running
// keeps it, since the kubelet unstages such a volume itself, and so does a
// mount at the staging path, which an earlier stage made; a mount table
// that cannot be read keeps it too, and so does a device that something
// still holds, as an unstage's drop does (heldDevice).
func (n *node) forgetFailedStage(
	ctx context.Context,
	staging string,
	nqn string,
	dev string,
	err error,
) {
	switch status.Code(err) {
	case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded,
		codes.ResourceExhausted, codes.Aborted:
		return
	}
	entry, merr := n.mnt.mountAt(ctx, staging)
	if merr != nil || entry != nil {
		return
	}
	if herr := n.heldDevice(ctx, strings.TrimPrefix(nqn, nqnPrefix), dev); herr != nil {
		slog.InfoContext(ctx, msgFollowerSyncError,
			slog.String("nqn", nqn), slog.String("error", herr.Error()))
		return
	}
	if uerr := n.follow.unstage(ctx, nqn); uerr != nil {
		slog.InfoContext(ctx, msgFollowerSyncError,
			slog.String("nqn", nqn), slog.String("error", uerr.Error()))
	}
}

// heldDevice answers whether something still holds a volume's device, so
// that its paths must stay (csi.md CSI16): nil when the kernel grants an
// exclusive open of it, or the device is gone and has no path left to keep;
// otherwise the call's error. The kernel refuses that open while any mount
// of the device is left, the one an unmount still tears down while it
// writes the file system's data included, a mount another mount namespace
// copied, which no table this plugin reads shows, and any other holder,
// such as a device mapper, md or bcache device built over a raw block
// volume. A call that ended first answers its end, not a hold it never read.
func (n *node) heldDevice(ctx context.Context, name string, dev string) error {
	err := n.mnt.oc.OpenExclusive(ctx, dev)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if ctx.Err() != nil {
		return status.Errorf(status.FromContextError(ctx.Err()).Code(),
			"volume %q: the call ended before its device %s was read free, "+
				"so its paths stay: %v", name, dev, err)
	}
	return status.Errorf(codes.FailedPrecondition,
		"volume %q: its device %s is still held, by a mount in some mount "+
			"namespace, an unmount that still writes, or a device built "+
			"over it, so its paths stay: %v", name, dev, err)
}

// NodeUnstageVolume unmounts the staging path and, once nothing holds the
// volume's device, drops every path to the volume's subsystem and forgets
// it; one try of a volume runs at a time. The subsystem's NQN derives from
// the volume id, since an unstage carries no context.
func (n *node) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument,
			"the staging target path is required")
	}
	_, name, err := parseVolumeId(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	// One try at a time: a try whose unmount still writes the file system's
	// data, after the kubelet's deadline ended its call, holds the volume, so
	// a second try neither unmounts nor drops the paths that data needs; and
	// a stage of the volume that still runs holds it too.
	release, running, ok := n.tryVolume(subsystemNqnOf(name), opUnstage)
	if !ok {
		return nil, status.Errorf(codes.Aborted,
			"another %s of volume %q runs", running, name)
	}
	defer release()
	entry, err := n.mnt.mountAt(ctx, staging)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if entry != nil {
		if err := n.mnt.unmount(ctx, staging); err != nil {
			// The kubelet's deadline can end the call while the unmount
			// waits to write the file system's data; the try answers that
			// end, and the lock above keeps the next tries off until the
			// unmount is done.
			if ctx.Err() != nil {
				return nil, status.Errorf(
					status.FromContextError(ctx.Err()).Code(),
					"volume %q: the call ended while its unmount ran, so "+
						"its paths stay: %v", name, err)
			}
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	// The paths go only once nothing holds the device: an unmount takes the
	// mount out of the table before it has written the file system's data.
	if err := n.heldDevice(ctx, name, devicePath(namespaceUuidOf(name))); err != nil {
		return nil, err
	}
	if err := n.follow.unstage(ctx, subsystemNqnOf(name)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume gives a staged volume to a pod: a bind mount of the
// staging path for a filesystem volume, read-only when asked, and a bind
// mount of the device node onto a file for a block volume. A target already
// mounted is done, but for a read-only request over a read-write mount: a
// read-only publish binds first and remounts read-only after, so a remount
// that failed, or a plugin stopped between the two, left a read-write bind
// in the plugin's own view, which the repeat finishes, so that the answer
// matches what the plugin did. The pod's read-only access is Kubernetes'
// flag on the container's mount, not this remount (csi.md CSI17).
func (n *node) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is required")
	}
	cap := req.GetVolumeCapability()
	if cap == nil {
		return nil, status.Error(codes.InvalidArgument,
			"the volume capability is required")
	}
	if err := checkCapability(cap); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.FailedPrecondition,
			"the staging target path is required: the volume is staged first")
	}
	entry, err := n.mnt.mountAt(ctx, target)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if entry != nil {
		if req.GetReadonly() && !entry.readOnly {
			if err := n.mnt.remountReadOnly(ctx, target); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if _, ok := cap.GetAccessType().(*csi.VolumeCapability_Block); ok {
		pc, err := readPublishContext(req.GetPublishContext())
		if err != nil {
			return nil, err
		}
		dev := devicePath(pc.uuid)
		if _, err := n.host.oc.ReadDevNo(ctx, dev); err != nil {
			return nil, status.Errorf(codes.NotFound,
				"the device of the volume, %s, is not there: %v", dev, err)
		}
		if err := n.mnt.touch(ctx, target); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if err := n.mnt.bindMount(ctx, dev, target, req.GetReadonly()); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}
	staged, err := n.mnt.mountAt(ctx, staging)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if staged == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"nothing is staged at %s", staging)
	}
	if err := n.mnt.mkdir(ctx, target); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := n.mnt.bindMount(ctx, staging, target, req.GetReadonly()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the target and removes it; a target that is
// not mounted or gone is done.
func (n *node) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is required")
	}
	entry, err := n.mnt.mountAt(ctx, target)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if entry != nil {
		if err := n.mnt.unmount(ctx, target); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if err := n.mnt.remove(ctx, target); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats reports a mounted filesystem's totals, or a block
// volume's size alone.
func (n *node) NodeGetVolumeStats(
	ctx context.Context,
	req *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume path is required")
	}
	entry, err := n.mnt.mountAt(ctx, path)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "nothing is mounted at %s", path)
	}
	if _, err := n.host.oc.ReadDevNo(ctx, path); err == nil {
		size, err := n.mnt.blockSize(ctx, path)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{{
			Total: int64(size),
			Unit:  csi.VolumeUsage_BYTES,
		}}}, nil
	}
	st, err := n.mnt.statfs(ctx, path)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{
			Total:     int64(st.totalBytes),
			Used:      int64(st.totalBytes - st.freeBytes),
			Available: int64(st.availBytes),
			Unit:      csi.VolumeUsage_BYTES,
		},
		{
			Total:     int64(st.totalInodes),
			Used:      int64(st.totalInodes - st.freeInodes),
			Available: int64(st.availInodes),
			Unit:      csi.VolumeUsage_INODES,
		},
	}}, nil
}
