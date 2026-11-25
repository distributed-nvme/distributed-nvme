package gateway

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Disk nodes / gateway.md, Disk nodes: the six
// RPCs that own a disk node's four keys — `dn_conf` (authoritative, keyed by
// addr_port), `dn_capacity` (the allocator index, present iff the node is
// allocatable per architecture.md, Capacity index keys), `dn_rev` (the
// id-keyed handle a dn-worker watches, architecture.md, Revision keys and the
// sync fan-out) and the cluster's `DnGlobal` (id + shard minting,
// architecture.md, Globals: id allocation + shard buckets).
//
// None of the six bumps a revision. A DN's desired state is its `DnConf`, and
// the only RPC here that writes one after creation is UpdateDiskNodeDisabled,
// which architecture.md, Disk nodes, explicitly exempts: `disabled` gates CP
// scheduling only, exactly like `err_epoch` and capacity-key maintenance, so
// no agent has to be told about it and sides already hosted by the node keep
// running. That is why no op-name constant is declared in this file — no bump
// helper and no model op that names an op is called from it.
//
// Three of the six leave etcd (AG1): CreateDiskNode needs the node's disk
// size before it can compute `total_ext_cnt`, and the host name of its
// machine for a node created with no location; DeleteDiskNode without force
// needs its agent's proof that the node is clean; and InspectDiskNode needs
// the agent's latest observation of the node. Each call sits strictly outside
// a transaction: before the create's, between the delete's planning snapshot
// and its deciding STM, and after the inspect's.

// CreateDiskNode is the CreateDiskNode of architecture.md, Disk nodes.
//
// The disk size comes from the node itself, so the handler is a pre-STM agent
// call followed by one STM (AG1; architecture.md, STM discipline). The size
// probe needs a `cluster_id`, which, per architecture.md, cluster_id
// derivation, depends on `ClusterConf.creation_epoch`, so a plain Get of
// ClusterConf supplies it for the request only: the in-STM read stays
// authoritative and it is that ClusterConf's `extent_size` that
// `total_ext_cnt` is computed from, so a cluster deleted and recreated between
// the probe and the transaction cannot leave a DN sized against a stale conf.
// The size and the host name are facts of the node, not of the store, so
// carrying them into the closure keeps the closure a pure function of what
// it reads (GW8).
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
		// architecture.md, Disk nodes, refuses an empty transport conf
		// outright: a DN with no transport can never be connected to by a
		// cntlr, so the record would describe a node no SP could ever use.
		return nil, errInvalid("nvme_tr_conf must not be empty")
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
	var hostName string
	// dn_id 0: the node has not been given one yet and the agent needs none —
	// it uses the id for logging only (architecture.md, Disk nodes).
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
			hostName = reply.GetHostName()
			return nil
		})
	if agentErr != nil {
		// AG3: a transport failure or a non-OK status is an ABORTED
		// (architecture.md, UNEXPECTED_ERROR → `ABORTED`). The
		// node is not registered, so there is nothing to roll back.
		return nil, errAborted(
			"get dn size from %q: %v", req.GetAddrPort(), agentErr)
	}
	// The location default of architecture.md, Disk nodes: a node
	// created with no location takes the host name its agent reported, so
	// every node of one machine is one failure domain until an operator
	// names another; a name that is no valid location refuses the create
	// before anything is written (hostNameLocation).
	location := req.GetLocation()
	if location == "" {
		location, err = hostNameLocation(req.GetAddrPort(), hostName)
		if err != nil {
			return nil, err
		}
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
		// architecture.md, Size → extents: counts round down, and the agent
		// already reported the data area only, so nothing further is
		// subtracted here. extent_size is read exactly as CreateCluster stored
		// it (architecture.md, Common validation) — it is what this
		// node's disk header will be formatted with — so the stored conf is
		// validated first rather than divided by.
		if err := model.ValidateClusterConf(cc); err != nil {
			return errAborted("%v", err)
		}
		extentSize := cc.GetDnBinConf().GetExtentSize()
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
			// CreateCluster writes is gone, which is an ABORTED
			// (architecture.md, UNEXPECTED_ERROR → `ABORTED`) and never
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
		// the same reason (architecture.md, Disk nodes).
		newDn := &pb.DnConf{
			DnId:        drawn.Id,
			ShardCode:   drawn.Shard,
			Disabled:    req.GetDisabled(),
			NvmeTrConf:  req.GetNvmeTrConf(),
			Location:    location,
			TotalExtCnt: totalExtCnt,
			FreeExtCnt:  totalExtCnt,
			// The machine's name, kept so that a migration never puts the
			// two sides of one leg on two disk nodes of one machine
			// (architecture.md, Migrations); empty when the read failed,
			// and for a name every unnamed machine reports, which names no
			// machine (storedHostName).
			HostName: storedHostName(hostName),
		}
		stm.Put(confKey, newDn)
		// The capacity key is written iff the new node is allocatable, so a
		// node created with disabled = true is invisible to the allocator
		// from its first instant (architecture.md, Capacity index keys).
		model.MaintainDnCapacity(stm, cid, req.GetAddrPort(), cc, nil, newDn)
		// The DnRev key appearing is what makes the owning dn-worker start
		// syncing and health-checking the node (architecture.md, Revision keys
		// and the sync fan-out). It is written in the
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

// DeleteDiskNode is the DeleteDiskNode of architecture.md, Disk nodes.
//
// Without force it is two-phase (AG4): a planning snapshot makes every check
// the deciding STM makes (dnDeleteChecks), so a refusal about the request or
// the store comes before the agent is asked; checkDnClean then asks the
// node's agent, outside any STM (AG1), for its latest observation of the
// node, which must prove the node clean; and the deciding STM makes the
// checks again and requires the cluster id, the dn_id and the DnRev
// revision the planning snapshot read (sameNode), so a node whose desired
// state moved, or that was deleted and created again at the address, during
// the agent call is refused rather than deleted on an answer about another
// state. With force the delete is one STM and asks no agent: the node goes
// with what its agent still holds. Removing the DnRev key stops the
// dn-worker, which sends no last syncup, and the agent keeps the node's
// request in its local store (architecture.md, Disk nodes; Known limits).
//
// The occupancy check is `side_ptr_list`, with force too, and it runs AFTER
// the token check (GW6) so an operator working from a stale GetDiskNode is
// told its view is stale, not told about sides it never saw. An operator who
// sent no token has opted out of that ordering (GW6 is presence-based) and
// is told about the sides directly.
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
	var plan *nodePlan
	if !req.GetForce() {
		err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
			plan = nil
			read, err := dnDeleteChecks(stm, req)
			if err != nil {
				return err
			}
			plan = &nodePlan{
				Cid:      read.cid,
				NodeId:   read.dn.GetDnId(),
				Revision: read.rev.GetRevision(),
			}
			return nil
		})
		if err != nil {
			return nil, mapStmErr(err)
		}
		if err := checkDnClean(ctx, req.GetAddrPort(), plan); err != nil {
			return nil, err
		}
	}
	var dnId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		dnId = 0
		read, err := dnDeleteChecks(stm, req)
		if err != nil {
			return err
		}
		if err := plan.sameNode(
			"disk node", req.GetAddrPort(), read.cid, read.dn.GetDnId(),
			read.rev.GetRevision(),
		); err != nil {
			return err
		}
		dn := read.dn
		// Deleting the rev key is the event that stops the owning dn-worker
		// health-checking the node (architecture.md, Revision keys and the
		// sync fan-out); DnConf and the capacity key go
		// with it in one atomic commit, so no worker ever sees a node whose
		// conf is gone but whose rev key survives.
		stm.Del(model.DnRevKey(dn.GetShardCode(), read.cid, dn.GetDnId()))
		stm.Del(model.DnConfKey(read.cid, req.GetAddrPort()))
		// dn is the record THIS transaction read, which is what makes the
		// capacity-key delete exact: the key embeds free_ext_cnt
		// (architecture.md, Capacity index keys).
		model.MaintainDnCapacity(
			stm, read.cid, req.GetAddrPort(), read.cc, dn, nil)
		// Only the bucket is decremented; next_id keeps growing, because ids
		// are never reused and an agent may assume, an etcd restore aside,
		// that a deleted node never comes back under the same id
		// (architecture.md, Globals: id allocation + shard buckets).
		read.global.ShardBucket = releaseShard(
			read.global.GetShardBucket(), dn.GetShardCode())
		stm.Put(model.DnGlobalKey(read.cid), read.global)
		dnId = dn.GetDnId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteDiskNodeReply{DnId: dnId}, nil
}

// dnDeleteRead is what dnDeleteChecks read: the resolved cluster, the node's
// record, its DnRev and the cluster's DnGlobal.
type dnDeleteRead struct {
	cid    uint64
	cc     *pb.ClusterConf
	dn     *pb.DnConf
	rev    *pb.DnRev
	global *pb.DnGlobal
}

// dnDeleteChecks makes DeleteDiskNode's checks in their order, the same in
// the planning snapshot and in the deciding STM: resolve; the DnConf by
// address (NOT_FOUND); the token (GW6), whose read of the DnRev key also
// gives the revision the agent's answer is held to; a non-empty
// side_ptr_list, with force too (FAILED_PRECONDITION, and its message does
// not offer force); DnGlobal present; and the stored ClusterConf.
func dnDeleteChecks(
	stm etcdutil.STM,
	req *pb.DeleteDiskNodeRequest,
) (*dnDeleteRead, error) {
	cid, cc, err := resolveCluster(stm, req.GetClusterName())
	if err != nil {
		return nil, err
	}
	dn := &pb.DnConf{}
	if !stm.Get(model.DnConfKey(cid, req.GetAddrPort()), dn) {
		return nil, errNotFound("disk node %q not found", req.GetAddrPort())
	}
	rev, err := checkDnToken(stm, cid, dn, req.GetDnRev())
	if err != nil {
		return nil, err
	}
	if len(dn.GetSidePtrList()) != 0 {
		return nil, errPrecondition(
			"disk node %q still hosts %d sides: move each leg off it by a "+
				"migration or a repair, or delete its storage pool or clone, "+
				"and delete each retired leg (DeleteRetiredLeg; dnvctl "+
				"retired-leg delete), first",
			req.GetAddrPort(), len(dn.GetSidePtrList()))
	}
	globalKey := model.DnGlobalKey(cid)
	global := &pb.DnGlobal{}
	if !stm.Get(globalKey, global) {
		return nil, errAborted("dn_global key %q is missing", globalKey)
	}
	// The last check of the request and the store, before the deciding
	// transaction's first write, so it can still return having staged
	// nothing. MaintainDnCapacity names the key to delete by shifting the
	// STORED ladder (architecture.md, Common validation), so against a
	// conf CreateCluster could not have written it names a key nothing
	// ever wrote: the delete would miss, the node's real capacity key
	// would outlive its DnConf, and once the conf was repaired the scan of
	// architecture.md, Per-operation allocation, would keep offering free
	// space on a node that is gone. The check runs after the token and the
	// side_ptr_list check so an operator still hears about a stale view
	// or about the sides first — those are answers about the request,
	// this one is about the store.
	if err := model.ValidateClusterConf(cc); err != nil {
		return nil, errAborted("%v", err)
	}
	return &dnDeleteRead{cid: cid, cc: cc, dn: dn, rev: rev, global: global}, nil
}

// nodePlan is what a node delete's planning snapshot read and its agent's
// answer was judged against: the cluster id, the node's id and its stored
// revision. A nil plan is a forced delete, which asked no agent.
type nodePlan struct {
	Cid      uint64
	NodeId   uint64
	Revision uint64
}

// sameNode is a node delete's check in its deciding STM that the node is
// the one its agent answered about, at the same desired state: the same
// cluster id, the same node id and the same stored revision as the planning
// snapshot read, whatever the token (AG4). A token-less request would not
// see a side added and dropped again during the agent call, nor a node
// deleted and created again at the address, whose new id and first
// revision can match the old ones' numbers only together with a new
// cluster. Each is ABORTED: the caller runs the delete again. A nil plan
// (force) checks nothing.
func (p *nodePlan) sameNode(
	kind string,
	addrPort string,
	cid uint64,
	nodeId uint64,
	revision uint64,
) error {
	switch {
	case p == nil:
		return nil
	case cid != p.Cid:
		return errAborted(
			"%s %q: its cluster was created again during the check of "+
				"the node; run the delete again", kind, addrPort)
	case nodeId != p.NodeId:
		return errAborted(
			"%s %q is node %d, not node %d the check asked about: it was "+
				"created again during the check; run the delete again",
			kind, addrPort, nodeId, p.NodeId)
	case revision != p.Revision:
		return errAborted(
			"%s %q changed during the check: its revision is %d, the "+
				"check proved %d; run the delete again",
			kind, addrPort, revision, p.Revision)
	}
	return nil
}

// checkDnClean is DeleteDiskNode's agent call between its two phases (AG1,
// AG3): one GetDnInfo, which answers from the agent's latest observation of
// the node and reads nothing of it (architecture.md, Live-state reporting),
// with no read_io_stats. The answer proves the node clean, or the delete is
// refused (nodeObsRefusal, then the disk counts). An agent that does not
// answer is FAILED_PRECONDITION, as the hydration checks answer one (AG3):
// its silence proves nothing about what the node still holds.
func checkDnClean(ctx context.Context, addrPort string, plan *nodePlan) error {
	var reply *pb.GetDnInfoReply
	callErr := withDnAgent(ctx, addrPort,
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			got, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
				ClusterId: plan.Cid,
				DnId:      plan.NodeId,
			})
			reply = got
			return err
		})
	if callErr != nil {
		return errNodeUnproven("disk node", addrPort,
			"its agent did not answer: %v", callErr)
	}
	if err := nodeObsRefusal("disk node", addrPort, plan,
		reply.GetAgentReply(), reply.GetRevision(), reply.GetObsStamp(),
	); err != nil {
		return err
	}
	return dnCountsRefusal(addrPort, reply.GetDnInfo().GetDiskCounts())
}

// nodeObsRefusal judges an agent's answer about its node (GetDnInfo,
// GetCnInfo) for a node delete: the observation's code must be zero, an
// observation must exist, and it must be of the plan's revision. A non-zero
// code is a leftover the node's latest sweep or read names, a failure, a
// refused conf, an unknown node or an agent that still starts (dnagent.md
// SH33), and its details pass on cut to the row text cap
// (common.CutRowText; architecture.md, Live-state reporting). A revision
// below the stored one is a node whose latest desired state the agent has
// not applied, so the sweep that removes what the state no longer names may
// not have run; one above it is an agent
// that holds a newer state than etcd, as after an etcd restore.
func nodeObsRefusal(
	kind string,
	addrPort string,
	plan *nodePlan,
	agentReply *pb.AgentReply,
	revision uint64,
	stamp *pb.ObsStamp,
) error {
	switch {
	case agentReply.GetCode() != 0:
		return errNodeUnproven(kind, addrPort,
			"its agent's latest observation of the node answers code %d: %s",
			agentReply.GetCode(), common.CutRowText(agentReply.GetDetails()))
	case stamp.GetObsTime() == nil:
		return errNodeUnproven(kind, addrPort,
			"its agent holds no observation of the node yet")
	case revision < plan.Revision:
		return errNodeUnproven(kind, addrPort,
			"its agent's latest observation is of revision %d, the node's "+
				"is %d: the agent has not applied the node's latest "+
				"desired state yet", revision, plan.Revision)
	case revision > plan.Revision:
		return errNodeUnproven(kind, addrPort,
			"its agent's latest observation is of revision %d, above the "+
				"node's %d in etcd: the agent holds a newer desired state "+
				"than etcd, as after an etcd restore",
			revision, plan.Revision)
	}
	return nil
}

// dnCountsRefusal is the disk node's half of the proof: an empty volume
// table. The clean code alone proves no such table: while an export of a
// pool reads young, the agent's sweep keeps a dropped side's device and
// record and names neither (dnagent.md DN6, An sp waits for its exports).
// So the counts must be there, which the agent sends only beside an OK meta
// row from a table it has confirmed as this node's (dnagent.md DN18), and
// they must show no side record, every extent free and every
// clone-metadata unit free.
func dnCountsRefusal(addrPort string, counts *pb.DnInfo_DiskCounts) error {
	switch {
	case counts == nil:
		return errNodeUnproven("disk node", addrPort,
			"its agent reports no counts of the disk's volume table, "+
				"which it sends only from a table it has confirmed as "+
				"this node's")
	case counts.GetSideRecCnt() != 0:
		return errNodeUnproven("disk node", addrPort,
			"the disk's volume table still holds %d side records",
			counts.GetSideRecCnt())
	case counts.GetFreeExtCnt() != counts.GetTotalExtCnt():
		return errNodeUnproven("disk node", addrPort,
			"%d of the disk's %d extents are free",
			counts.GetFreeExtCnt(), counts.GetTotalExtCnt())
	case counts.GetFreeMetaUnits() != common.DnCloneMetaUnitCnt:
		return errNodeUnproven("disk node", addrPort,
			"%d of the disk's %d clone-metadata units are free",
			counts.GetFreeMetaUnits(), uint64(common.DnCloneMetaUnitCnt))
	}
	return nil
}

// errNodeUnproven is a node delete's refusal for a node its agent does not
// prove clean: what failed, then the two ways on.
func errNodeUnproven(
	kind string,
	addrPort string,
	format string,
	args ...any,
) error {
	return errPrecondition(
		"%s %q is not proven clean: %s; retry once its agent has removed "+
			"what the node no longer needs, or delete with force, which "+
			"leaves on the node what it still holds",
		kind, addrPort, fmt.Sprintf(format, args...))
}

// GetDiskNode is the GetDiskNode of architecture.md, Disk nodes.
//
// It is a Snapshot, not a RunSTM: both keys must come from one store revision,
// because the reply's DnRev is the client's optimistic-concurrency token for
// the next mutator (architecture.md, Revision keys and the sync fan-out) and a
// token read at a different revision than the
// conf it describes would let a client act on a view that never existed. The
// request names the node by addr_port, so DnConf must be read first — the
// id-keyed DnRev key cannot be formed before its dn_id and shard_code are
// known (architecture.md, Disk nodes).
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
			// (architecture.md, UNEXPECTED_ERROR → `ABORTED`), not a node that
			// has yet to be registered.
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

// ListDiskNodes is the ListDiskNodes of architecture.md, Disk nodes.
//
// architecture.md, page_token, keeps the list RPCs out of the STM entirely — a
// transaction cannot range — so the cluster is resolved with one plain Get
// instead: the reply is a page of names, not a consistent view of anything,
// and a node created or deleted while the page is being read is exactly the
// kind of change a paged listing is allowed to straddle. The names returned
// are the `addr_port` key suffixes under the prefix (GW10).
func (s *Server) ListDiskNodes(
	ctx context.Context,
	req *pb.ListDiskNodesRequest,
) (*pb.ListDiskNodesReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	// GW4: the two page arguments are pure checks of architecture.md, Common
	// validation, so they run before the ClusterConf read the prefix needs — a
	// bad count or page_token must be INVALID_ARGUMENT, not the NOT_FOUND a
	// missing cluster would otherwise answer first.
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

// UpdateDiskNodeDisabled is the UpdateDiskNodeDisabled of architecture.md,
// Disk nodes.
//
// `disabled` is a scheduling flag and nothing else: it decides whether the
// allocator may see the node, i.e. whether the capacity key of
// architecture.md, Capacity index keys, exists, and
// it is invisible to the agent. That is why this handler bumps no revision
// and makes no agent call — sides the node already hosts keep running — and
// why the whole mutation is one DnConf write plus MaintainDnCapacity.
//
// When the stored flag already equals the requested one the handler writes
// nothing at all (GW6): a token that was sent has still been checked first,
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
		// Below the no-op return above, because a request that changes
		// nothing reaches no capacity-key maintenance and so has nothing to
		// be wrong about, and above the Put, because a refusal must stage no
		// write. Flipping the flag moves a capacity key in both directions —
		// disabling deletes the key the STORED ladder names, enabling writes
		// it back (architecture.md, Capacity index keys) — so a conf
		// CreateCluster could not have written
		// would leave a disabled node in the index, or index an enabled one
		// under a bin its free count does not belong to.
		if err := model.ValidateClusterConf(cc); err != nil {
			return errAborted("%v", err)
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

// InspectDiskNode is the InspectDiskNode of architecture.md, Disk nodes: the
// node as its agent last observed it, for diagnostics.
//
// The reply is the agent's, whole: `applied_revision` and `dn_info` both
// come from the GetDnInfo reply (architecture.md, Disk nodes, deliberately not
// the stored rev key), which answers from the agent's latest observation of
// the node (architecture.md, Live-state reporting). The
// pair is that one observation: `applied_revision` is the revision the agent
// held when the reading started, which can trail the last SyncupDn it
// accepted, as it does while that syncup's converge runs, and a caller diffs
// it against the desired-state token GetDiskNode hands out. The reply's stamp
// passes through beside them with the observation's age (obsAge), so a
// caller sees how old the observation is and whether a converge of the node
// is in flight. The call asks the agent for the disk's counters too
// (read_io_stats), which the agent reads at the call (dnagent.md DN16), and
// passes them through: InspectDiskNode serves a person, and is the only
// caller that asks.
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
	var stamp *pb.ObsStamp
	var ioStats *pb.DevIoStats
	agentErr := withDnAgent(ctx, req.GetAddrPort(),
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
				ClusterId:   cid,
				DnId:        dnId,
				ReadIoStats: true,
			})
			if err != nil {
				return err
			}
			appliedRevision = reply.GetRevision()
			info = reply.GetDnInfo()
			stamp = reply.GetObsStamp()
			ioStats = reply.GetDiskIoStats()
			return nil
		})
	if agentErr != nil {
		return nil, errAborted(
			"get dn info from %q: %v", req.GetAddrPort(), agentErr)
	}
	return &pb.InspectDiskNodeReply{
		AppliedRevision: appliedRevision,
		DnInfo:          info,
		ObsStamp:        stamp,
		ObsAge:          obsAge(stamp),
		DiskIoStats:     ioStats,
	}, nil
}
