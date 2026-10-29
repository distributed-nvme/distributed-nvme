package csi

import (
	"context"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// node is the CSI node service (csi.md, The node plugin): it stages a
// volume by connecting the node to its subsystem through the follower and
// waiting for the device, formats a blank device and mounts it, publishes
// the staged volume to a pod as a filesystem or a block device, and reports
// the used space. It never talks to the gateway: everything it needs comes
// in the publish context, and the rest it derives from the volume id.
type node struct {
	csi.UnimplementedNodeServer
	id         hostIdentity
	host       *nvmeHost
	follow     *follower
	mnt        *mounter
	deviceWait time.Duration
}

// publishContext is what the controller plugin handed the node at attach.
type publishContext struct {
	nqn  string
	uuid string
	cdcs []endpoint
}

// readPublishContext reads the attach's context; a volume staged before a
// plugin restart carries it again on every call, so nothing is kept.
func readPublishContext(m map[string]string) (publishContext, error) {
	pc := publishContext{nqn: m[ctxNqn], uuid: strings.ToLower(m[ctxUuid])}
	if pc.nqn == "" || pc.uuid == "" {
		return publishContext{}, status.Error(codes.InvalidArgument,
			"the publish context must carry the subsystem NQN and the namespace uuid")
	}
	for _, raw := range strings.Split(m[ctxCdc], cdcSep) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		ep, err := parseEndpoint(raw)
		if err != nil {
			return publishContext{}, status.Errorf(codes.InvalidArgument,
				"the publish context's cdc address %q is not ip:port", raw)
		}
		pc.cdcs = append(pc.cdcs, ep)
	}
	if len(pc.cdcs) == 0 {
		return publishContext{}, status.Error(codes.InvalidArgument,
			"the publish context names no cdc address")
	}
	return pc, nil
}

func (n *node) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: n.id.hostNqn}, nil
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

	// The connects, retried within the device wait: the cdc lists a
	// subsystem before its cntlrs have built it, and a port that does not
	// listen yet refuses the connect.
	deadline := time.Now().Add(n.deviceWait)
	for {
		err := n.follow.stage(ctx, pc.nqn, pc.cdcs)
		if err == nil {
			break
		}
		if held, herr := n.follow.hasPath(ctx, pc.nqn); herr == nil && held {
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
		return nil, err
	}
	n.follow.trigger()

	mnt, ok := cap.GetAccessType().(*csi.VolumeCapability_Mount)
	if !ok {
		// A block volume is published straight from the device.
		return &csi.NodeStageVolumeResponse{}, nil
	}
	fsType := mnt.Mount.GetFsType()
	if fsType == "" {
		fsType = fsExt4
	}
	entry, err := n.mnt.mountAt(ctx, staging)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if entry != nil {
		if entry.fsType != fsType {
			return nil, status.Errorf(codes.AlreadyExists,
				"%s is staged with %s, not %s", staging, entry.fsType, fsType)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}
	existing, err := n.mnt.fsType(ctx, dev)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	switch existing {
	case "":
		if err := n.mnt.mkfs(ctx, fsType, dev); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	case fsType:
	default:
		return nil, status.Errorf(codes.FailedPrecondition,
			"the device of the volume carries %s, not %s; the driver never "+
				"reformats", existing, fsType)
	}
	if err := n.mnt.mkdir(ctx, staging); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := n.mnt.mount(ctx, dev, staging, fsType, mnt.Mount.GetMountFlags()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts the staging path, drops every path to the
// volume's subsystem and forgets it. The subsystem's NQN derives from the
// volume id, since an unstage carries no context.
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
	entry, err := n.mnt.mountAt(ctx, staging)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if entry != nil {
		if err := n.mnt.unmount(ctx, staging); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if err := n.follow.unstage(ctx, subsystemNqnOf(name)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume gives a staged volume to a pod: a bind mount of the
// staging path for a filesystem volume, read-only when asked, and a bind
// mount of the device node onto a file for a block volume.
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
