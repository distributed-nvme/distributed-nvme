package gateway

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.3 / gateway.md §5.3: the six RPCs that own a
// controller node's four keys — `cn_conf` (authoritative, keyed by addr_port),
// `cn_capacity` (the allocator index, present iff the node is allocatable per
// §5.6), `cn_rev` (the id-keyed handle a cn-worker watches, §5.5) and the
// cluster's `CnGlobal` (id + shard minting, §5.4).
//
// §8.3 defines them as §8.2's disk-node RPCs mirrored, so this file is kept
// step for step the same as disknode.go and only these five points differ:
//
//   - the pre-STM size call is `ControllerNodeAgent.GetCnSize`, and its reply
//     is a budget read through §6.1's CN rule (cnCapBudget) rather than a
//     measured disk size used verbatim;
//   - the per-cluster ceiling is `common.MaxCnCntPerCluster`;
//   - delete's occupancy precondition is `cntlr_ptr_list`;
//   - capacity maintenance is `model.MaintainCnCapacity`, which takes no
//     ClusterConf because a CN capacity key carries no bin index (§6.4);
//   - InspectControllerNode calls `GetCnInfo`.
//
// None of the six bumps a revision, and no op-name constant is therefore
// declared: no bump helper and no model op that names an op is called from
// here. UpdateControllerNodeDisabled is the only RPC that rewrites a CnConf
// after creation, and §8.3 exempts it exactly as §8.2 exempts its DN twin —
// `disabled` gates CP scheduling only, so no agent has to be told and the
// cntlrs the node already hosts keep running.
//
// Two of the six leave etcd (AG1): CreateControllerNode needs the node's
// capacity budget before it can compute `total_ext_cnt`, and
// InspectControllerNode needs the agent's live view. Both calls sit strictly
// outside the transaction, one before it and one after it.

// cnCapBudget maps a GetCnSize reply onto the capacity budget `total_ext_cnt`
// is computed from (§6.1). This is the one place the CN flow is not a
// character-for-character mirror of the DN one: a DN reports a disk it has
// measured and the gateway believes it, while a CN reports how much working
// space for clones and migrations the operator lets dnv use — an opinion, and
// therefore one read with a floor and a ceiling.
//
// 0 means "no opinion" and takes DefaultCnCap (4 TiB); a NON-ZERO reply below
// MinCnCap is treated exactly like 0 rather than accepted as a budget too
// small to allocate anything from; anything above MaxCnCap (64 TiB) is clamped
// to it. Whether the resulting budget yields at least one extent is decided by
// the caller against the cluster's extent_size, which only the STM knows.
func cnCapBudget(size uint64) uint64 {
	if size < common.MinCnCap {
		return common.DefaultCnCap
	}
	if size > common.MaxCnCap {
		return common.MaxCnCap
	}
	return size
}

// CreateControllerNode is architecture.md §8.3's CreateControllerNode.
//
// The budget comes from the node itself, so the handler is a pre-STM agent
// call followed by one STM (AG1, §5.8). The size probe needs a `cluster_id`,
// which since §5.2 depends on `ClusterConf.creation_epoch`, so a plain Get of
// ClusterConf supplies it for the request only: the in-STM read stays
// authoritative and it is that ClusterConf's `extent_size` that
// `total_ext_cnt` is computed from, so a cluster deleted and recreated between
// the probe and the transaction cannot leave a CN sized against a stale conf.
// The budget itself is a fact of the node, not of the store, so carrying it
// into the closure keeps the closure a pure function of what it reads (GW8).
func (s *Server) CreateControllerNode(
	ctx context.Context,
	req *pb.CreateControllerNodeRequest,
) (*pb.CreateControllerNodeReply, error) {
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
		// §8.2's rule, mirrored: a CN with no transport conf can advertise no
		// address to a host, so the record would describe a node no subsystem
		// could ever be reached through — §8.8 builds a CdcEntry out of
		// exactly this message.
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
	var cnSize uint64
	// cn_id 0: the node has not been given one yet and the agent needs none —
	// it uses the id for logging only (§8.2).
	agentErr := withCnAgent(ctx, req.GetAddrPort(),
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCnSize(ctx, &pb.GetCnSizeRequest{
				ClusterId: probeCid,
				CnId:      0,
			})
			if err != nil {
				return err
			}
			cnSize = reply.GetSize()
			return nil
		})
	if agentErr != nil {
		// AG3: a transport failure or a non-OK status is §5.9's ABORTED. The
		// node is not registered, so there is nothing to roll back.
		return nil, errAborted(
			"get cn size from %q: %v", req.GetAddrPort(), agentErr)
	}
	budget := cnCapBudget(cnSize)

	var cnId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cnId = 0
		cid, cc, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		confKey := model.CnConfKey(cid, req.GetAddrPort())
		if stm.Get(confKey, &pb.CnConf{}) {
			return errExists(
				"controller node %q already exists", req.GetAddrPort())
		}
		// §6.1: counts round down. extent_size is cluster-wide and shared
		// with the DNs, which is what makes a cntlr's reservation on this
		// node comparable to the group ext_cnts it covers (§8.4, §8.6).
		extentSize := model.ResolveDnBinConf(cc.GetDnBinConf()).GetExtentSize()
		totalExtCnt := budget / extentSize
		if totalExtCnt < 1 {
			return errInvalid(
				"controller node %q yields a budget of %d bytes, less than "+
					"one extent of %d",
				req.GetAddrPort(), budget, extentSize)
		}
		globalKey := model.CnGlobalKey(cid)
		global := &pb.CnGlobal{}
		if !stm.Get(globalKey, global) {
			// The cluster exists but its CnGlobal does not: an invariant key
			// CreateCluster writes is gone, which is §5.9's ABORTED and never
			// a reason to invent a global here — minting from a fresh one
			// would hand out ids the cluster has already used.
			return errAborted("cn_global key %q is missing", globalKey)
		}
		drawn, err := mintClusterId(
			global.GetNextId(), global.GetShardBucket(),
			common.MaxCnCntPerCluster, "controller node")
		if err != nil {
			return err
		}
		// err_epoch 0 and an empty cntlr_ptr_list are the proto3 zeros a
		// fresh node carries: it is healthy and hosts no cntlr yet. free =
		// total for the same reason (§8.2).
		newCn := &pb.CnConf{
			CnId:        drawn.Id,
			ShardCode:   drawn.Shard,
			Disabled:    req.GetDisabled(),
			NvmeTrConf:  req.GetNvmeTrConf(),
			Location:    location,
			TotalExtCnt: totalExtCnt,
			FreeExtCnt:  totalExtCnt,
		}
		stm.Put(confKey, newCn)
		// The capacity key is written iff the new node is allocatable, so a
		// node created with disabled = true is invisible to the allocator
		// from its first instant (§5.6). No ClusterConf is passed: a CN
		// capacity key carries no bin index (§6.4).
		model.MaintainCnCapacity(stm, cid, req.GetAddrPort(), nil, newCn)
		// The CnRev key appearing is what makes the owning cn-worker start
		// syncing and health-checking the node (§5.5). It is written in the
		// same transaction as the CnConf that worker will read, and the
		// commit is atomic, so the watch can never fire on a node whose
		// record is not there yet.
		stm.Put(model.CnRevKey(drawn.Shard, cid, drawn.Id), &pb.CnRev{
			AddrPort: req.GetAddrPort(),
			Revision: 1,
		})
		global.NextId = drawn.NextId
		global.ShardBucket = drawn.Bucket
		stm.Put(globalKey, global)
		cnId = drawn.Id
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateControllerNodeReply{CnId: cnId}, nil
}

// DeleteControllerNode is architecture.md §8.3's DeleteControllerNode.
//
// It reaches no agent by design: removing the CnRev key stops the cn-worker,
// and once no desired state describes the node its agent process can simply
// be stopped (§8.2). The occupancy check is `cntlr_ptr_list` — the CN mirror
// of §8.2's `side_ptr_list` — and it runs AFTER the token check (GW6) so an
// operator working from a stale GetControllerNode is told its view is stale,
// not told about cntlrs it never saw. An operator who sent no token has opted
// out of that ordering (GW6 is presence-based, §0 #7) and is told about the
// cntlrs directly.
func (s *Server) DeleteControllerNode(
	ctx context.Context,
	req *pb.DeleteControllerNodeRequest,
) (*pb.DeleteControllerNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var cnId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cnId = 0
		cid, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		confKey := model.CnConfKey(cid, req.GetAddrPort())
		cn := &pb.CnConf{}
		if !stm.Get(confKey, cn) {
			return errNotFound(
				"controller node %q not found", req.GetAddrPort())
		}
		if _, err := checkCnToken(
			stm, cid, cn, req.GetCnRev(),
		); err != nil {
			return err
		}
		if len(cn.GetCntlrPtrList()) != 0 {
			return errPrecondition(
				"controller node %q still hosts %d cntlrs: delete or migrate "+
					"the owning storage pools first",
				req.GetAddrPort(), len(cn.GetCntlrPtrList()))
		}
		globalKey := model.CnGlobalKey(cid)
		global := &pb.CnGlobal{}
		if !stm.Get(globalKey, global) {
			return errAborted("cn_global key %q is missing", globalKey)
		}
		// Deleting the rev key is the event that stops the owning cn-worker
		// health-checking the node (§5.5); CnConf and the capacity key go
		// with it in one atomic commit, so no worker ever sees a node whose
		// conf is gone but whose rev key survives.
		stm.Del(model.CnRevKey(cn.GetShardCode(), cid, cn.GetCnId()))
		stm.Del(confKey)
		// cn is the record THIS transaction read, which is what makes the
		// capacity-key delete exact: the key embeds free_ext_cnt (§5.6).
		model.MaintainCnCapacity(stm, cid, req.GetAddrPort(), cn, nil)
		// Only the bucket is decremented; next_id keeps growing, because ids
		// are never reused and an agent may assume a deleted node never comes
		// back under the same id (§5.4).
		global.ShardBucket = releaseShard(
			global.GetShardBucket(), cn.GetShardCode())
		stm.Put(globalKey, global)
		cnId = cn.GetCnId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteControllerNodeReply{CnId: cnId}, nil
}

// GetControllerNode is architecture.md §8.3's GetControllerNode.
//
// It is a Snapshot, not a RunSTM: both keys must come from one store revision,
// because the reply's CnRev is the client's optimistic-concurrency token for
// the next mutator (§5.5) and a token read at a different revision than the
// conf it describes would let a client act on a view that never existed. The
// request names the node by addr_port, so CnConf must be read first — the
// id-keyed CnRev key cannot be formed before its cn_id and shard_code are
// known (§8.2).
func (s *Server) GetControllerNode(
	ctx context.Context,
	req *pb.GetControllerNodeRequest,
) (*pb.GetControllerNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var conf *pb.CnConf
	var rev *pb.CnRev
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		conf, rev = nil, nil
		cid, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		cn := &pb.CnConf{}
		if !stm.Get(model.CnConfKey(cid, req.GetAddrPort()), cn) {
			return errNotFound(
				"controller node %q not found", req.GetAddrPort())
		}
		revKey := model.CnRevKey(cn.GetShardCode(), cid, cn.GetCnId())
		cnRev := &pb.CnRev{}
		if !stm.Get(revKey, cnRev) {
			// Every existing CnConf has a CnRev — CreateControllerNode writes
			// both in one transaction — so a missing one is a lost invariant
			// key (§5.9), not a node that has yet to be registered.
			return errAborted("cn_rev key %q is missing", revKey)
		}
		conf, rev = cn, cnRev
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetControllerNodeReply{
		AddrPort: req.GetAddrPort(),
		CnConf:   conf,
		CnRev:    rev,
	}, nil
}

// ListControllerNodes is architecture.md §8.3's ListControllerNodes.
//
// §5.7 keeps the list RPCs out of the STM entirely — a transaction cannot
// range — so the cluster is resolved with one plain Get instead: the reply is
// a page of names, not a consistent view of anything, and a node created or
// deleted while the page is being read is exactly the kind of change a paged
// listing is allowed to straddle. The names returned are the `addr_port` key
// suffixes under the prefix (GW10).
func (s *Server) ListControllerNodes(
	ctx context.Context,
	req *pb.ListControllerNodesRequest,
) (*pb.ListControllerNodesReply, error) {
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
		ctx, s.cli, model.CnConfPrefix(cid),
		req.GetCount(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListControllerNodesReply{AddrPort: names, PageToken: next}, nil
}

// UpdateControllerNodeDisabled is architecture.md §8.3's
// UpdateControllerNodeDisabled.
//
// `disabled` is a scheduling flag and nothing else: it decides whether the
// allocator may see the node, i.e. whether the §5.6 capacity key exists, and
// it is invisible to the agent. That is why this handler bumps no revision
// and makes no agent call — cntlrs the node already hosts keep running — and
// why the whole mutation is one CnConf write plus MaintainCnCapacity.
//
// When the stored flag already equals the requested one the handler writes
// nothing at all (§0 #17): a token that was sent has still been checked first,
// so a stale client is refused rather than silently told its no-op succeeded,
// and a genuine repeat costs one empty transaction.
func (s *Server) UpdateControllerNodeDisabled(
	ctx context.Context,
	req *pb.UpdateControllerNodeDisabledRequest,
) (*pb.UpdateControllerNodeDisabledReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var cnId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cnId = 0
		cid, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		confKey := model.CnConfKey(cid, req.GetAddrPort())
		cn := &pb.CnConf{}
		if !stm.Get(confKey, cn) {
			return errNotFound(
				"controller node %q not found", req.GetAddrPort())
		}
		if _, err := checkCnToken(
			stm, cid, cn, req.GetCnRev(),
		); err != nil {
			return err
		}
		cnId = cn.GetCnId()
		if cn.GetDisabled() == req.GetDisabled() {
			return nil
		}
		// cn stays the record as read so MaintainCnCapacity can delete the
		// exact key it implied; the clone carries the new flag.
		updated := proto.Clone(cn).(*pb.CnConf)
		updated.Disabled = req.GetDisabled()
		stm.Put(confKey, updated)
		model.MaintainCnCapacity(stm, cid, req.GetAddrPort(), cn, updated)
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateControllerNodeDisabledReply{CnId: cnId}, nil
}

// InspectControllerNode is architecture.md §8.3's InspectControllerNode: the
// node as its agent currently sees it — port, tmpfs, tmp file and loop device
// (§9.3) — for diagnostics.
//
// The reply is the agent's, whole: `applied_revision` and `cn_info` both
// come from the GetCnInfo reply (architecture.md §8.3, deliberately not the
// stored rev key), so the pair is one coherent agent snapshot and a caller
// can diff `applied_revision` against the desired-state token
// GetControllerNode hands out.
//
// The Snapshot resolves the cluster itself, so the agent request's cluster_id
// comes from it rather than from a second plain pre-read: the read is already
// finished when the call is made, and one read of ClusterConf cannot be less
// authoritative than two. Nothing is written, so no token is taken — a client
// that wants the desired-state token calls GetControllerNode.
func (s *Server) InspectControllerNode(
	ctx context.Context,
	req *pb.InspectControllerNodeRequest,
) (*pb.InspectControllerNodeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var cid uint64
	var cnId uint64
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		cid, cnId = 0, 0
		clusterId, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		cn := &pb.CnConf{}
		if !stm.Get(model.CnConfKey(clusterId, req.GetAddrPort()), cn) {
			return errNotFound(
				"controller node %q not found", req.GetAddrPort())
		}
		cid, cnId = clusterId, cn.GetCnId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var appliedRevision uint64
	var info *pb.CnInfo
	agentErr := withCnAgent(ctx, req.GetAddrPort(),
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCnInfo(ctx, &pb.GetCnInfoRequest{
				ClusterId: cid,
				CnId:      cnId,
			})
			if err != nil {
				return err
			}
			appliedRevision = reply.GetRevision()
			info = reply.GetCnInfo()
			return nil
		})
	if agentErr != nil {
		// AG3 again. GetCnInfoReply's AgentReply is deliberately not
		// inspected: that convention does not apply to the Get*Info reads
		// beyond what their replies define.
		return nil, errAborted(
			"get cn info from %q: %v", req.GetAddrPort(), agentErr)
	}
	return &pb.InspectControllerNodeReply{
		AppliedRevision: appliedRevision,
		CnInfo:          info,
	}, nil
}
