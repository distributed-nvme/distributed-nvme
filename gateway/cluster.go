package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Clusters / gateway.md, Clusters: the five
// cluster RPCs.
//
// The cluster is the one resource whose identity is derived rather than
// stored: `ClusterConf` is the only name-keyed message and
// `cluster_id = fnv64a(name ‖ creation_epoch)` (architecture.md, cluster_id
// derivation) prefixes every other key
// in the store. That makes this file the two ends of GW5 — `CreateCluster` is
// the only mutator that does not resolve a cluster (it mints the epoch the
// resolution would read), and `ListClusters` is the only list that needs no
// `cluster_id` at all.
//
// Nothing here bumps a revision: `DnRev`/`CnRev`/`SpRev` are per-object keys
// that do not exist yet at `CreateCluster` and can no longer exist at
// `DeleteCluster` (its emptiness gate is exactly the statement that none do),
// so the fan-out of architecture.md, Revision keys and the sync fan-out, has
// nothing to reach and no op name is cited; and `AdvanceClusterIds` writes
// only id counters, which no agent reads (GW19).

// CreateCluster is the CreateCluster of architecture.md, Clusters: the one RPC
// that computes cluster_id without reading ClusterConf first, because it is
// the RPC that mints creation_epoch.
//
// The epoch is stamped ONCE, outside the STM (GW8): an internal retry of this
// attempt re-runs the closure against the same epoch and therefore rewrites
// exactly the same three keys, which is what makes the retry idempotent. A
// client that retries after a failure stamps a NEW epoch and so targets a
// different cluster_id — which is precisely why the hash-collision guard below
// is re-evaluated inside every attempt rather than hoisted out.
//
// ClusterConf is write-once (architecture.md, Clusters: no UpdateCluster* RPC
// exists, deliberately), so this is the only chance its members ever get to be
// made concrete, and both halves happen here in this order:
// validateClusterConfInput bounds the RAW request — where a proto3 zero still
// means "give me the default" and a non-zero value outside its range is
// refused, and then judges the geometry rules of architecture.md, Common
// validation, once more on a resolved copy of
// its bdev_conf, and refuses a qos_ratio that is not all zero — and
// model.ResolveClusterConf then turns the accepted request
// into the concrete message that is stored. The order is load-bearing:
// resolving first would replace every omitted
// member with a constant and make the bound checks tautologies.
//
// Nothing downstream resolves again. A zero read back out of this key is
// corruption, and the worker and the agents refuse it rather than guessing —
// which is what makes a cluster's extent size and bin ladder immutable
// facts rather than whatever the reading binary's constants happen to say.
func (s *Server) CreateCluster(
	ctx context.Context,
	req *pb.CreateClusterRequest,
) (*pb.CreateClusterReply, error) {
	if err := validateClusterConfInput(req); err != nil {
		return nil, err
	}
	name := clusterNameOf(req.GetClusterName())
	creationEpoch := uint64(time.Now().UnixNano())
	// Built once, outside the STM, like creation_epoch above (GW8): an
	// internal retry of this attempt must rewrite byte-identical keys.
	conf := model.ResolveClusterConf(&pb.ClusterConf{
		CreationEpoch:   creationEpoch,
		QosRatio:        req.GetQosRatio(),
		BdevConf:        req.GetBdevConf(),
		DnBinConf:       req.GetDnBinConf(),
		AllocConf:       req.GetAllocConf(),
		HealthCheckConf: req.GetHealthCheckConf(),
	})
	var cid uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cid = 0
		confKey := model.ClusterConfKey(name)
		if stm.Get(confKey, &pb.ClusterConf{}) {
			return errExists("cluster %q already exists", name)
		}
		cid = model.ClusterId(name, creationEpoch)
		// The hash-collision guard of architecture.md, Clusters. A 64-bit
		// fnv1a of name ‖ epoch colliding with a live cluster is vanishingly
		// unlikely, but the consequence would be two clusters silently sharing
		// every key prefix, so it is checked rather than assumed. The three
		// globals are distinct proto types, hence three reads instead of a
		// loop; the short circuit is safe because any hit aborts the
		// transaction.
		if stm.Get(model.DnGlobalKey(cid), &pb.DnGlobal{}) ||
			stm.Get(model.CnGlobalKey(cid), &pb.CnGlobal{}) ||
			stm.Get(model.SpGlobalKey(cid), &pb.SpGlobal{}) {
			return errExists(
				"cluster %q would take cluster_id %#016x, which is already "+
					"in use", name, cid)
		}
		stm.Put(confKey, conf)
		// next_id starts at 1 and shard_bucket is ShardBucketSize zeros
		// (architecture.md, Globals: id allocation + shard buckets). A fresh
		// bucket per global rather than one shared slice:
		// they are three independent counters and must never alias.
		stm.Put(model.DnGlobalKey(cid), &pb.DnGlobal{
			NextId:      1,
			ShardBucket: zeroBucket(),
		})
		stm.Put(model.CnGlobalKey(cid), &pb.CnGlobal{
			NextId:      1,
			ShardBucket: zeroBucket(),
		})
		stm.Put(model.SpGlobalKey(cid), &pb.SpGlobal{
			NextId:      1,
			ShardBucket: zeroBucket(),
		})
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateClusterReply{ClusterId: cid}, nil
}

// DeleteCluster is the DeleteCluster of architecture.md, Clusters.
//
// The emptiness precondition is sum(shard_bucket) == 0 on ALL THREE globals,
// not a range read: architecture.md, Globals: id allocation + shard buckets,
// makes the sum the cluster's live object count by construction (every create
// increments a bucket, every delete decrements one), so the check is three
// point reads the STM already needs for the deletes. A missing global is an
// ABORTED (architecture.md, UNEXPECTED_ERROR → `ABORTED`) — the cluster's
// invariant keys are gone and the count cannot be established, and an RPC
// never guesses at a precondition it cannot read.
//
// The reply carries the deleted cluster_id because it is derived, not stored:
// a later CreateCluster with the same name stamps a new epoch and therefore
// reports a different one (architecture.md, Clusters).
func (s *Server) DeleteCluster(
	ctx context.Context,
	req *pb.DeleteClusterRequest,
) (*pb.DeleteClusterReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	name := clusterNameOf(req.GetClusterName())
	var cid uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cid = 0
		resolved, _, err := resolveCluster(stm, name)
		if err != nil {
			return err
		}
		cid = resolved
		dnKey := model.DnGlobalKey(cid)
		dnGlobal := &pb.DnGlobal{}
		if !stm.Get(dnKey, dnGlobal) {
			return errAborted("dn_global key %q is missing", dnKey)
		}
		cnKey := model.CnGlobalKey(cid)
		cnGlobal := &pb.CnGlobal{}
		if !stm.Get(cnKey, cnGlobal) {
			return errAborted("cn_global key %q is missing", cnKey)
		}
		spKey := model.SpGlobalKey(cid)
		spGlobal := &pb.SpGlobal{}
		if !stm.Get(spKey, spGlobal) {
			return errAborted("sp_global key %q is missing", spKey)
		}
		dnCnt := bucketSum(dnGlobal.GetShardBucket())
		cnCnt := bucketSum(cnGlobal.GetShardBucket())
		spCnt := bucketSum(spGlobal.GetShardBucket())
		if dnCnt != 0 || cnCnt != 0 || spCnt != 0 {
			return errPrecondition(
				"cluster %q still holds %d disk nodes, %d controller nodes "+
					"and %d storage pools", name, dnCnt, cnCnt, spCnt)
		}
		stm.Del(model.ClusterConfKey(name))
		stm.Del(dnKey)
		stm.Del(cnKey)
		stm.Del(spKey)
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteClusterReply{ClusterId: cid}, nil
}

// GetCluster is the GetCluster of architecture.md, Clusters.
//
// It is a Snapshot, not a RunSTM (GW5): the four messages are read at one
// store revision and nothing is written, so a concurrent CreateDiskNode can
// never make the reply show a ClusterConf from before it and a DnGlobal from
// after it. A missing global is ABORTED for the same reason as in
// DeleteCluster: the cluster's invariant keys are incomplete.
//
// cluster_name is echoed DEFAULTED, so a caller that sent "" learns which
// cluster it actually read.
func (s *Server) GetCluster(
	ctx context.Context,
	req *pb.GetClusterRequest,
) (*pb.GetClusterReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	name := clusterNameOf(req.GetClusterName())
	var reply *pb.GetClusterReply
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		reply = nil
		cid, conf, err := resolveCluster(stm, name)
		if err != nil {
			return err
		}
		dnKey := model.DnGlobalKey(cid)
		dnGlobal := &pb.DnGlobal{}
		if !stm.Get(dnKey, dnGlobal) {
			return errAborted("dn_global key %q is missing", dnKey)
		}
		cnKey := model.CnGlobalKey(cid)
		cnGlobal := &pb.CnGlobal{}
		if !stm.Get(cnKey, cnGlobal) {
			return errAborted("cn_global key %q is missing", cnKey)
		}
		spKey := model.SpGlobalKey(cid)
		spGlobal := &pb.SpGlobal{}
		if !stm.Get(spKey, spGlobal) {
			return errAborted("sp_global key %q is missing", spKey)
		}
		reply = &pb.GetClusterReply{
			ClusterName: name,
			ClusterId:   cid,
			ClusterConf: conf,
			DnGlobal:    dnGlobal,
			CnGlobal:    cnGlobal,
			SpGlobal:    spGlobal,
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return reply, nil
}

// ListClusters is the ListClusters of architecture.md, Clusters: the one list
// — indeed the one RPC other than CreateCluster — that resolves no cluster at
// all (GW5).
//
// It needs none: ClusterConf is the only name-keyed message, so its prefix can
// be ranged without a cluster_id, and the names it returns are the key
// suffixes. Turning a listed name into an id is GetCluster's job. Like every
// paged list it uses plain reads and no transaction (architecture.md,
// page_token) — a transaction cannot range, and a page is a snapshot of names,
// not an invariant.
func (s *Server) ListClusters(
	ctx context.Context,
	req *pb.ListClustersRequest,
) (*pb.ListClustersReply, error) {
	names, nextToken, err := pageNames(
		ctx, s.cli, model.ClusterConfPrefix(),
		req.GetCount(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListClustersReply{
		ClusterName: names,
		PageToken:   nextToken,
	}, nil
}

// idStepRanged runs in AdvanceClusterIds between the range that lists the
// pools and the first pool transaction, and does nothing in production. It is
// the seam a test deletes and creates pools through, in exactly the window
// the range opens.
var idStepRanged = func() {}

// AdvanceClusterIds is the AdvanceClusterIds of architecture.md, Clusters:
// the id step (GW19), run after an etcd restore (architecture.md, etcd backup
// and restore).
//
// One STM resolves the cluster and moves the next_id of its three globals
// with advanceNextId. A keys-only range of the cluster's SpConf prefix then
// lists the pools, outside any STM, and one STM per listed pool moves the
// pool's next_id with advanceNextId and its next_dev_id with advanceDevId. A
// pool whose SpConf is gone by its transaction is skipped: nothing is put for
// it and the reply's sp_cnt does not count it. A pool latched for deletion
// moves like any other.
//
// The globals go first, so a pool created while the step runs draws an sp_id
// above every old one and needs no step of its own. The pool transactions
// read no ClusterConf: the first transaction's cluster id names their keys,
// and a pool of a recreated cluster lives under another id. One transaction
// per pool keeps each to one SpConf whatever the pool count, where one over
// every pool of a cluster at MaxSpCntPerCluster would pass EtcdMaxTxnOps. No
// transaction reads or bumps a rev key: no agent reads a counter. An error in
// any transaction ends the RPC there, the pools before it moved.
func (s *Server) AdvanceClusterIds(
	ctx context.Context,
	req *pb.AdvanceClusterIdsRequest,
) (*pb.AdvanceClusterIdsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	name := clusterNameOf(req.GetClusterName())
	var cid uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cid = 0
		resolved, _, err := resolveCluster(stm, name)
		if err != nil {
			return err
		}
		dnKey := model.DnGlobalKey(resolved)
		dnGlobal := &pb.DnGlobal{}
		if !stm.Get(dnKey, dnGlobal) {
			return errAborted("dn_global key %q is missing", dnKey)
		}
		cnKey := model.CnGlobalKey(resolved)
		cnGlobal := &pb.CnGlobal{}
		if !stm.Get(cnKey, cnGlobal) {
			return errAborted("cn_global key %q is missing", cnKey)
		}
		spKey := model.SpGlobalKey(resolved)
		spGlobal := &pb.SpGlobal{}
		if !stm.Get(spKey, spGlobal) {
			return errAborted("sp_global key %q is missing", spKey)
		}
		if dnGlobal.NextId, err = advanceNextId(
			dnGlobal.GetNextId(), "dn_global"); err != nil {
			return err
		}
		if cnGlobal.NextId, err = advanceNextId(
			cnGlobal.GetNextId(), "cn_global"); err != nil {
			return err
		}
		if spGlobal.NextId, err = advanceNextId(
			spGlobal.GetNextId(), "sp_global"); err != nil {
			return err
		}
		stm.Put(dnKey, dnGlobal)
		stm.Put(cnKey, cnGlobal)
		stm.Put(spKey, spGlobal)
		cid = resolved
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	prefix := model.SpConfPrefix(cid)
	keys, _, err := s.cli.RangeKeys(ctx, prefix)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	idStepRanged()
	spCnt := uint32(0)
	for _, entry := range keys {
		spName := strings.TrimPrefix(entry.Key, prefix)
		stepped := false
		err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			stepped = false
			key := model.SpConfKey(cid, spName)
			conf := &pb.SpConf{}
			if !stm.Get(key, conf) {
				// Deleted since the range: it hands out nothing more.
				return nil
			}
			next, err := advanceNextId(model.SpNextId(conf),
				fmt.Sprintf("storage pool %q", spName))
			if err != nil {
				return err
			}
			conf.NextId = next
			conf.NextDevId = advanceDevId(conf.GetNextDevId())
			stm.Put(key, conf)
			stepped = true
			return nil
		})
		if err != nil {
			return nil, mapStmErr(err)
		}
		if stepped {
			spCnt++
		}
	}
	return &pb.AdvanceClusterIdsReply{ClusterId: cid, SpCnt: spCnt}, nil
}
