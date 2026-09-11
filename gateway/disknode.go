package gateway

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.2 / gateway.md §5.2: the six RPCs that own a
// disk node's four keys — `dn_conf` (authoritative, keyed by addr_port),
// `dn_capacity` (the allocator index, present iff the node is allocatable per
// §5.6), `dn_rev` (the id-keyed handle a dn-worker watches, §5.5) and the
// cluster's `DnGlobal` (id + shard minting, §5.4).
//
// None of the six bumps a revision. A DN's desired state is its `DnConf`, and
// the only RPC here that writes one after creation is UpdateDiskNodeDisabled,
// which §8.2 explicitly exempts: `disabled` gates CP scheduling only, exactly
// like `err_epoch` and capacity-key maintenance, so no agent has to be told
// about it and sides already hosted by the node keep running. That is why no
// op-name constant is declared in this file — no bump helper and no model op
// that names an op is called from it.
//
// Two of the six leave etcd (AG1): CreateDiskNode needs the node's disk size
// before it can compute `total_ext_cnt`, and InspectDiskNode needs the agent's
// live view. Both calls sit strictly outside the transaction, one before it
// and one after it.

// CreateDiskNode is architecture.md §8.2's CreateDiskNode.
//
// The disk size comes from the node itself, so the handler is a pre-STM agent
// call followed by one STM (AG1, §5.8). The size probe needs a `cluster_id`,
// which since §5.2 depends on `ClusterConf.creation_epoch`, so a plain Get of
// ClusterConf supplies it for the request only: the in-STM read stays
// authoritative and it is that ClusterConf's `extent_size` that
// `total_ext_cnt` is computed from, so a cluster deleted and recreated between
// the probe and the transaction cannot leave a DN sized against a stale conf.
// The size itself is a fact of the node, not of the store, so carrying it into
// the closure keeps the closure a pure function of what it reads (GW8).
func (s *Server) CreateDiskNode(
	ctx context.Context,
	req *pb.CreateDiskNodeRequest,
) (*pb.CreateDiskNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	if err := validateOptionalName("location", req.GetLocation()); err != nil {
		return nil, err
	}
	if err := validateTrConf("nvme_tr_conf", req.GetNvmeTrConf()); err != nil {
		return nil, err
	}
	if trConfEmpty(req.GetNvmeTrConf()) {
		// §8.2 refuses an empty transport conf outright: a DN with no
		// transport can never be connected to by a cntlr, so the record
		// would describe a node no SP could ever use.
		return nil, errInvalid("nvme_tr_conf must not be empty")
	}
	// §8.2 Defaults: an omitted location is the node's own endpoint, which
	// makes every node its own failure domain until an operator groups them.
	location := req.GetLocation()
	if location == "" {
		location = req.GetAddrPort()
	}

	clusterName := clusterNameOf(req.GetClusterName())
	probeConf := &pb.ClusterConf{}
	found, err := s.cli.Get(ctx, model.ClusterConfKey(clusterName), probeConf)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	if !found {
		return nil, errNotFound("cluster %q not found", clusterName)
	}
	probeCid := model.ClusterId(clusterName, probeConf.GetCreationEpoch())
	var dnSize uint64
	// dn_id 0: the node has not been given one yet and the agent needs none —
	// it uses the id for logging only (§8.2).
	agentErr := withDnAgent(ctx, req.GetAddrPort(),
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			reply, err := client.GetDnSize(ctx, &pb.GetDnSizeRequest{
				ClusterId: probeCid,
				DnId:      0,
			})
			if err != nil {
				return err
			}
			dnSize = reply.GetSize()
			return nil
		})
	if agentErr != nil {
		// AG3: a transport failure or a non-OK status is §5.9's ABORTED. The
		// node is not registered, so there is nothing to roll back.
		return nil, errAborted(
			"get dn size from %q: %v", req.GetAddrPort(), agentErr)
	}

	var dnId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		dnId = 0
		cid, cc, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		confKey := model.DnConfKey(cid, req.GetAddrPort())
		if stm.Get(confKey, &pb.DnConf{}) {
			return errExists("disk node %q already exists", req.GetAddrPort())
		}
		// §6.1: counts round down, and the agent already reported the data
		// area only, so nothing further is subtracted here.
		extentSize := model.ResolveDnBinConf(cc.GetDnBinConf()).GetExtentSize()
		totalExtCnt := dnSize / extentSize
		if totalExtCnt < 1 {
			return errInvalid(
				"disk node %q reports %d bytes, less than one extent of %d",
				req.GetAddrPort(), dnSize, extentSize)
		}
		globalKey := model.DnGlobalKey(cid)
		global := &pb.DnGlobal{}
		if !stm.Get(globalKey, global) {
			// The cluster exists but its DnGlobal does not: an invariant key
			// CreateCluster writes is gone, which is §5.9's ABORTED and never
			// a reason to invent a global here — minting from a fresh one
			// would hand out ids the cluster has already used.
			return errAborted("dn_global key %q is missing", globalKey)
		}
		drawn, err := mintClusterId(
			global.GetNextId(), global.GetShardBucket(),
			common.MaxDnCntPerCluster, "disk node")
		if err != nil {
			return err
		}
		// err_epoch 0 and an empty side_ptr_list are the proto3 zeros a fresh
		// node carries: it is healthy and hosts no side yet. free = total for
		// the same reason (§8.2).
		newDn := &pb.DnConf{
			DnId:        drawn.Id,
			ShardCode:   drawn.Shard,
			Disabled:    req.GetDisabled(),
			NvmeTrConf:  req.GetNvmeTrConf(),
			Location:    location,
			TotalExtCnt: totalExtCnt,
			FreeExtCnt:  totalExtCnt,
		}
		stm.Put(confKey, newDn)
		// The capacity key is written iff the new node is allocatable, so a
		// node created with disabled = true is invisible to the allocator
		// from its first instant (§5.6).
		model.MaintainDnCapacity(stm, cid, req.GetAddrPort(), cc, nil, newDn)
		// The DnRev key appearing is what makes the owning dn-worker start
		// syncing and health-checking the node (§5.5). It is written in the
		// same transaction as the DnConf that worker will read, and the
		// commit is atomic, so the watch can never fire on a node whose
		// record is not there yet.
		stm.Put(model.DnRevKey(drawn.Shard, cid, drawn.Id), &pb.DnRev{
			AddrPort: req.GetAddrPort(),
			Revision: 1,
		})
		global.NextId = drawn.NextId
		global.ShardBucket = drawn.Bucket
		stm.Put(globalKey, global)
		dnId = drawn.Id
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateDiskNodeReply{DnId: dnId}, nil
}

// DeleteDiskNode is architecture.md §8.2's DeleteDiskNode.
//
// It reaches no agent by design: removing the DnRev key stops the dn-worker,
// and once no desired state describes the node its agent process can simply
// be stopped (§8.2). The occupancy check is `side_ptr_list`, and it runs
// AFTER the token check (GW6) so an operator working from a stale
// GetDiskNode is told its view is stale, not told about sides it never saw.
// An operator who sent no token has opted out of that ordering (GW6 is
// presence-based, §0 #7) and is told about the sides directly.
func (s *Server) DeleteDiskNode(
	ctx context.Context,
	req *pb.DeleteDiskNodeRequest,
) (*pb.DeleteDiskNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var dnId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		dnId = 0
		cid, cc, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		confKey := model.DnConfKey(cid, req.GetAddrPort())
		dn := &pb.DnConf{}
		if !stm.Get(confKey, dn) {
			return errNotFound("disk node %q not found", req.GetAddrPort())
		}
		if _, err := checkDnToken(
			stm, cid, dn, req.GetDnRev(),
		); err != nil {
			return err
		}
		if len(dn.GetSidePtrList()) != 0 {
			return errPrecondition(
				"disk node %q still hosts %d sides: delete or migrate the "+
					"owning storage pools first",
				req.GetAddrPort(), len(dn.GetSidePtrList()))
		}
		globalKey := model.DnGlobalKey(cid)
		global := &pb.DnGlobal{}
		if !stm.Get(globalKey, global) {
			return errAborted("dn_global key %q is missing", globalKey)
		}
		// Deleting the rev key is the event that stops the owning dn-worker
		// health-checking the node (§5.5); DnConf and the capacity key go
		// with it in one atomic commit, so no worker ever sees a node whose
		// conf is gone but whose rev key survives.
		stm.Del(model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId()))
		stm.Del(confKey)
		// dn is the record THIS transaction read, which is what makes the
		// capacity-key delete exact: the key embeds free_ext_cnt (§5.6).
		model.MaintainDnCapacity(stm, cid, req.GetAddrPort(), cc, dn, nil)
		// Only the bucket is decremented; next_id keeps growing, because ids
		// are never reused and an agent may assume a deleted node never comes
		// back under the same id (§5.4).
		global.ShardBucket = releaseShard(
			global.GetShardBucket(), dn.GetShardCode())
		stm.Put(globalKey, global)
		dnId = dn.GetDnId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteDiskNodeReply{DnId: dnId}, nil
}

// GetDiskNode is architecture.md §8.2's GetDiskNode.
//
// It is a Snapshot, not a RunSTM: both keys must come from one store revision,
// because the reply's DnRev is the client's optimistic-concurrency token for
// the next mutator (§5.5) and a token read at a different revision than the
// conf it describes would let a client act on a view that never existed. The
// request names the node by addr_port, so DnConf must be read first — the
// id-keyed DnRev key cannot be formed before its dn_id and shard_code are
// known (§8.2).
func (s *Server) GetDiskNode(
	ctx context.Context,
	req *pb.GetDiskNodeRequest,
) (*pb.GetDiskNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var conf *pb.DnConf
	var rev *pb.DnRev
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		conf, rev = nil, nil
		cid, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		dn := &pb.DnConf{}
		if !stm.Get(model.DnConfKey(cid, req.GetAddrPort()), dn) {
			return errNotFound("disk node %q not found", req.GetAddrPort())
		}
		revKey := model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId())
		dnRev := &pb.DnRev{}
		if !stm.Get(revKey, dnRev) {
			// Every existing DnConf has a DnRev — CreateDiskNode writes both
			// in one transaction — so a missing one is a lost invariant key
			// (§5.9), not a node that has yet to be registered.
			return errAborted("dn_rev key %q is missing", revKey)
		}
		conf, rev = dn, dnRev
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetDiskNodeReply{
		AddrPort: req.GetAddrPort(),
		DnConf:   conf,
		DnRev:    rev,
	}, nil
}

// ListDiskNodes is architecture.md §8.2's ListDiskNodes.
//
// §5.7 keeps the list RPCs out of the STM entirely — a transaction cannot
// range — so the cluster is resolved with one plain Get instead: the reply is
// a page of names, not a consistent view of anything, and a node created or
// deleted while the page is being read is exactly the kind of change a paged
// listing is allowed to straddle. The names returned are the `addr_port` key
// suffixes under the prefix (GW10).
func (s *Server) ListDiskNodes(
	ctx context.Context,
	req *pb.ListDiskNodesRequest,
) (*pb.ListDiskNodesReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	// GW4: the two page arguments are pure §7 checks, so they run
	// before the ClusterConf read the prefix needs — a bad count or
	// page_token must be INVALID_ARGUMENT, not the NOT_FOUND a missing
	// cluster would otherwise answer first.
	if err := validatePageArgs(
		req.GetCount(), req.GetPageToken(),
	); err != nil {
		return nil, err
	}
	clusterName := clusterNameOf(req.GetClusterName())
	cc := &pb.ClusterConf{}
	found, err := s.cli.Get(ctx, model.ClusterConfKey(clusterName), cc)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	if !found {
		return nil, errNotFound("cluster %q not found", clusterName)
	}
	cid := model.ClusterId(clusterName, cc.GetCreationEpoch())
	names, next, err := pageNames(
		ctx, s.cli, model.DnConfPrefix(cid),
		req.GetCount(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListDiskNodesReply{AddrPort: names, PageToken: next}, nil
}

// UpdateDiskNodeDisabled is architecture.md §8.2's UpdateDiskNodeDisabled.
//
// `disabled` is a scheduling flag and nothing else: it decides whether the
// allocator may see the node, i.e. whether the §5.6 capacity key exists, and
// it is invisible to the agent. That is why this handler bumps no revision
// and makes no agent call — sides the node already hosts keep running — and
// why the whole mutation is one DnConf write plus MaintainDnCapacity.
//
// When the stored flag already equals the requested one the handler writes
// nothing at all (§0 #17): a token that was sent has still been checked first,
// so a stale client is refused rather than silently told its no-op succeeded,
// and a genuine repeat costs one empty transaction.
func (s *Server) UpdateDiskNodeDisabled(
	ctx context.Context,
	req *pb.UpdateDiskNodeDisabledRequest,
) (*pb.UpdateDiskNodeDisabledReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var dnId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		dnId = 0
		cid, cc, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		confKey := model.DnConfKey(cid, req.GetAddrPort())
		dn := &pb.DnConf{}
		if !stm.Get(confKey, dn) {
			return errNotFound("disk node %q not found", req.GetAddrPort())
		}
		if _, err := checkDnToken(
			stm, cid, dn, req.GetDnRev(),
		); err != nil {
			return err
		}
		dnId = dn.GetDnId()
		if dn.GetDisabled() == req.GetDisabled() {
			return nil
		}
		// dn stays the record as read so MaintainDnCapacity can delete the
		// exact key it implied; the clone carries the new flag.
		updated := proto.Clone(dn).(*pb.DnConf)
		updated.Disabled = req.GetDisabled()
		stm.Put(confKey, updated)
		model.MaintainDnCapacity(stm, cid, req.GetAddrPort(), cc, dn, updated)
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateDiskNodeDisabledReply{DnId: dnId}, nil
}

// InspectDiskNode is architecture.md §8.2's InspectDiskNode: the node as its
// agent currently sees it, for diagnostics.
//
// The reply is the agent's, whole: `applied_revision` and `dn_info` both
// come from the GetDnInfo reply (architecture.md §8.2, deliberately not the
// stored rev key), so the pair is one coherent agent snapshot and a caller
// can diff `applied_revision` against the desired-state token GetDiskNode
// hands out.
//
// The Snapshot resolves the cluster itself, so the agent request's cluster_id
// comes from it rather than from a second plain pre-read: the read is already
// finished when the call is made, and one read of ClusterConf cannot be less
// authoritative than two.
func (s *Server) InspectDiskNode(
	ctx context.Context,
	req *pb.InspectDiskNodeRequest,
) (*pb.InspectDiskNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var cid uint64
	var dnId uint64
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		cid, dnId = 0, 0
		clusterId, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		dn := &pb.DnConf{}
		if !stm.Get(model.DnConfKey(clusterId, req.GetAddrPort()), dn) {
			return errNotFound("disk node %q not found", req.GetAddrPort())
		}
		cid, dnId = clusterId, dn.GetDnId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var appliedRevision uint64
	var info *pb.DnInfo
	agentErr := withDnAgent(ctx, req.GetAddrPort(),
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
				ClusterId: cid,
				DnId:      dnId,
			})
			if err != nil {
				return err
			}
			appliedRevision = reply.GetRevision()
			info = reply.GetDnInfo()
			return nil
		})
	if agentErr != nil {
		return nil, errAborted(
			"get dn info from %q: %v", req.GetAddrPort(), agentErr)
	}
	return &pb.InspectDiskNodeReply{
		AppliedRevision: appliedRevision,
		DnInfo:          info,
	}, nil
}
