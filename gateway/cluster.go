package gateway

import (
	"context"
	"time"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.1 / gateway.md §5.1: the four cluster RPCs.
//
// The cluster is the one resource whose identity is derived rather than
// stored: `ClusterConf` is the only name-keyed message and
// `cluster_id = fnv64a(name ‖ creation_epoch)` (§5.2) prefixes every other key
// in the store. That makes this file the two ends of GW5 — `CreateCluster` is
// the only mutator that does not resolve a cluster (it mints the epoch the
// resolution would read), and `ListClusters` is the only list that needs no
// `cluster_id` at all.
//
// Nothing here bumps a revision: `DnRev`/`CnRev`/`SpRev` are per-object keys
// that do not exist yet at `CreateCluster` and can no longer exist at
// `DeleteCluster` (its emptiness gate is exactly the statement that none do),
// so §5.5's fan-out has nothing to reach and no op name is cited.

// CreateCluster is architecture.md §8.1's CreateCluster: the one RPC that
// computes cluster_id without reading ClusterConf first, because it is the RPC
// that mints creation_epoch.
//
// The epoch is stamped ONCE, outside the STM (GW8): an internal retry of this
// attempt re-runs the closure against the same epoch and therefore rewrites
// exactly the same three keys, which is what makes the retry idempotent. A
// client that retries after a failure stamps a NEW epoch and so targets a
// different cluster_id — which is precisely why the hash-collision guard below
// is re-evaluated inside every attempt rather than hoisted out.
//
// ClusterConf is write-once (§8.1: no UpdateCluster* RPC exists, deliberately),
// so this is the only chance its members ever get to be made concrete, and
// both halves happen here in this order: validateClusterConfInput bounds the
// RAW request — where a proto3 zero still means "give me the default" and a
// non-zero value outside its range is refused — and model.ResolveClusterConf
// then turns the accepted request into the concrete message that is stored.
// The order is load-bearing: resolving first would replace every omitted
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
		// The hash-collision guard of §8.1. A 64-bit fnv1a of name ‖ epoch
		// colliding with a live cluster is vanishingly unlikely, but the
		// consequence would be two clusters silently sharing every key
		// prefix, so it is checked rather than assumed. The three globals are
		// distinct proto types, hence three reads instead of a loop; the
		// short circuit is safe because any hit aborts the transaction.
		if stm.Get(model.DnGlobalKey(cid), &pb.DnGlobal{}) ||
			stm.Get(model.CnGlobalKey(cid), &pb.CnGlobal{}) ||
			stm.Get(model.SpGlobalKey(cid), &pb.SpGlobal{}) {
			return errExists(
				"cluster %q would take cluster_id %#016x, which is already "+
					"in use", name, cid)
		}
		stm.Put(confKey, conf)
		// next_id starts at 1 and shard_bucket is ShardBucketSize zeros
		// (§5.4). A fresh bucket per global rather than one shared slice:
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

// DeleteCluster is architecture.md §8.1's DeleteCluster.
//
// The emptiness precondition is sum(shard_bucket) == 0 on ALL THREE globals,
// not a range read: §5.4 makes the sum the cluster's live object count by
// construction (every create increments a bucket, every delete decrements
// one), so the check is three point reads the STM already needs for the
// deletes. A missing global is §5.9's ABORTED — the cluster's invariant keys
// are gone and the count cannot be established, and an RPC never guesses at a
// precondition it cannot read.
//
// The reply carries the deleted cluster_id because it is derived, not stored:
// a later CreateCluster with the same name stamps a new epoch and therefore
// reports a different one (§8.1).
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

// GetCluster is architecture.md §8.1's GetCluster.
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

// ListClusters is architecture.md §8.1's ListClusters: the one list — indeed
// the one RPC other than CreateCluster — that resolves no cluster at all (GW5).
//
// It needs none: ClusterConf is the only name-keyed message, so its prefix can
// be ranged without a cluster_id, and the names it returns are the key
// suffixes. Turning a listed name into an id is GetCluster's job. Like every
// paged list it uses plain reads and no transaction (§5.7) — a transaction
// cannot range, and a page is a snapshot of names, not an invariant.
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
