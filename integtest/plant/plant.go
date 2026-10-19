// Package plant is the planting library: the writes that put a desired state
// into etcd directly, as the gateway's transaction would have written it,
// with the placement the caller chooses instead of the allocator's
// (dnv-worker.md, Integration test plan, The driver). It is the core of
// integtest/workerctl, which is a front over it for the shell suites, and the
// worker's Go tests may call it.
//
// Every write formats the keys of architecture.md, Key table, through model,
// marshals the pb messages and bumps the revision keys, in one
// etcdutil.RunSTM, so the state it leaves behind is exactly the state a real
// gateway STM would have produced — capacity keys per architecture.md,
// Capacity index keys; revision keys bumped in place per architecture.md,
// Revision keys and the sync fan-out; SpConf.next_id past every id the caller
// assigned. The list calls, PutDns, PutCns and PutTds, are the one exception:
// they commit many records per transaction, which leaves the records the same
// single calls would leave, save that a pool's revision moves once per
// transaction rather than once per thin device. It never dials an agent and
// never sleeps.
//
// Every call takes the caller's ctx, the etcdutil client and the Cluster it
// writes into, returns the ids it wrote — the ones its spec gave or, where a
// spec leaves an id 0, the ones it took — and returns an error rather than
// ending the process. A spec's Check refuses what no transaction could write,
// before any etcd call; every Put runs it first.
package plant

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// nvmeTrType / nvmeAdrFam are the nvme_tr_conf fields every DnConf/CnConf
	// this library writes carries; the fake agents never open an NVMe-oF
	// port, so they only have to be deterministic and plausible. nvmeTrSvcId
	// is only the FALLBACK tr_svc_id of an address with no port —
	// nvmeTrConfOf stores the node's own port instead — and 4420 is the IANA
	// nvme-tcp port, which is all that makes it the plausible fallback: a real
	// agent has no default here at all, since dnv-agent's --tr-svc-id is
	// required (dnagent.md CM2).
	nvmeTrType  = "tcp"
	nvmeAdrFam  = "ipv4"
	nvmeTrSvcId = "4420"

	// nodesPerTxn is how many nodes one transaction of PutDns or PutCns
	// writes. The commit of a serializable STM compares every key it read
	// and every key it wrote, and etcd refuses a transaction of more
	// compares than common.EtcdMaxTxnOps: one node reads its conf and its
	// revision key and writes those two plus at most a capacity key's delete
	// and put, six, and the global it shares with the other nodes of the
	// transaction is read and written once.
	nodesPerTxn = (common.EtcdMaxTxnOps - 2) / 6

	// tdsPerTxn is how many thin devices one transaction of PutTds writes:
	// each reads and writes its own key, two compares, and the transaction
	// reads and writes the pool's SpConf and SpRev once.
	tdsPerTxn = (common.EtcdMaxTxnOps - 4) / 2
)

// ---------------------------------------------------------------------------
// Clusters (architecture.md, Clusters and cluster_id derivation)
// ---------------------------------------------------------------------------

// Cluster is one planted cluster: its name, the cluster_id derived from the
// name and the stored creation_epoch, and the ClusterConf etcd stores for it.
// Every other call takes one, from PutCluster or LoadCluster.
type Cluster struct {
	Name string
	Id   uint64
	Conf *pb.ClusterConf
}

// ClusterSpec is what PutCluster stores. A zero member is resolved the way
// CreateCluster resolves it (model.ResolveClusterConf), so the zero spec with
// a name plants a cluster of the defaults; note that the default extent size
// is common.DefaultDnExtSize, not the common.MinDnExtSize workerctl's flag
// defaults to.
type ClusterSpec struct {
	Name string
	// The four round intervals of health_check_conf, in seconds.
	DnInterval    uint32
	CnInterval    uint32
	SideInterval  uint32
	CntlrInterval uint32
	// ExtentSize is dn_bin_conf.extent_size in bytes.
	ExtentSize uint64
	// Lwm is bdev_conf.dm_pool_conf.low_water_mark_pct.
	Lwm uint32
	// BlockSize is bdev_conf.dm_pool_conf.data_block_size, the geometry's
	// block size.
	BlockSize uint64
	// ChunkBlocks is redund_md_raid1.bitmap_chunk_block_cnt, the geometry's
	// bitmap chunk.
	ChunkBlocks uint64
	// DnBatch and CnBatch are alloc_conf's batch sizes.
	DnBatch uint32
	CnBatch uint32
}

// PutCluster implements CreateCluster (architecture.md, Clusters and
// cluster_id derivation): it stamps creation_epoch = time.Now().UnixNano()
// like the gateway, derives the cluster_id from the name and that epoch, and
// writes the three zeroed globals alongside the ClusterConf (architecture.md,
// Globals: id allocation + shard buckets).
//
// It refuses to overwrite an existing cluster: creation_epoch is immutable,
// and rewriting it would orphan every other key of the cluster
// (architecture.md, cluster_id derivation).
func PutCluster(
	ctx context.Context,
	cli *etcdutil.Client,
	spec ClusterSpec,
) (Cluster, error) {
	if spec.Name == "" {
		return Cluster{}, fmt.Errorf("the cluster name is empty")
	}
	// The epoch is stamped once, outside the transaction: a retried attempt
	// must derive the same cluster_id.
	epoch := uint64(time.Now().UnixNano())
	cid := model.ClusterId(spec.Name, epoch)
	cc := &pb.ClusterConf{
		CreationEpoch: epoch,
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{
				DataBlockSize:   spec.BlockSize,
				LowWaterMarkPct: spec.Lwm,
			},
			// PutSp inherits this bdev_conf and replaces the redund kind
			// with the one its groups name; the chunk count rides along,
			// which is how a case pins the group geometry (architecture.md,
			// Group on-leg layout: meta region, data region, health block).
			RedundConf: &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundMdRaid1{
					RedundMdRaid1: &pb.RedundMdRaid1{
						BitmapChunkBlockCnt: spec.ChunkBlocks,
					},
				},
			},
		},
		DnBinConf: &pb.DnBinConf{ExtentSize: spec.ExtentSize},
		AllocConf: &pb.AllocConf{
			DnBatchSize: spec.DnBatch,
			CnBatchSize: spec.CnBatch,
		},
		HealthCheckConf: &pb.HealthCheckConf{
			DnInterval:    spec.DnInterval,
			CnInterval:    spec.CnInterval,
			SideInterval:  spec.SideInterval,
			CntlrInterval: spec.CntlrInterval,
		},
	}
	// This library plays the gateway, so it must store what CreateCluster
	// stores: a FULLY RESOLVED conf (architecture.md, Common validation).
	// Every member left at zero — the batch sizes, the four intervals, the
	// bin shift ladder, dm_raid0_conf.stripe_size — becomes concrete here,
	// because the worker validates the stored conf and refuses to drive an
	// object whose geometry nobody chose.
	cc = model.ResolveClusterConf(cc)
	newGlobal := func() ([]uint32, uint64) {
		return make([]uint32, common.ShardBucketSize), 1
	}
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := model.ClusterConfKey(spec.Name)
		if s.Get(key, &pb.ClusterConf{}) {
			return fmt.Errorf(
				"cluster %q already exists; creation_epoch is immutable "+
					"(architecture.md, cluster_id derivation)", spec.Name,
			)
		}
		s.Put(key, cc)
		bucket, nextId := newGlobal()
		s.Put(model.DnGlobalKey(cid), &pb.DnGlobal{
			NextId: nextId, ShardBucket: bucket,
		})
		bucket, nextId = newGlobal()
		s.Put(model.CnGlobalKey(cid), &pb.CnGlobal{
			NextId: nextId, ShardBucket: bucket,
		})
		bucket, nextId = newGlobal()
		s.Put(model.SpGlobalKey(cid), &pb.SpGlobal{
			NextId: nextId, ShardBucket: bucket,
		})
		return nil
	})
	if err != nil {
		return Cluster{}, err
	}
	return Cluster{Name: spec.Name, Id: cid, Conf: cc}, nil
}

// LoadCluster reads a cluster's ClusterConf and derives its cluster_id from
// it, exactly as the gateway must (architecture.md, cluster_id derivation):
// cluster_id is not computable from a name alone.
func LoadCluster(
	ctx context.Context,
	cli *etcdutil.Client,
	name string,
) (Cluster, error) {
	cc := &pb.ClusterConf{}
	found, err := cli.Get(ctx, model.ClusterConfKey(name), cc)
	if err != nil {
		return Cluster{}, err
	}
	if !found {
		return Cluster{}, fmt.Errorf("cluster %q does not exist", name)
	}
	return Cluster{
		Name: name,
		Id:   model.ClusterId(name, cc.GetCreationEpoch()),
		Conf: cc,
	}, nil
}

// ---------------------------------------------------------------------------
// Disk nodes and controller nodes (architecture.md, Disk nodes; Controller
// nodes)
// ---------------------------------------------------------------------------

// DnSpec is one disk node to plant, with an explicit endpoint, shard and
// budget.
type DnSpec struct {
	// Id is the dn_id; 0 takes the cluster's next free one.
	Id    uint64
	Shard uint32
	// Addr is the dn agent's ip:port, which DnConf is keyed by.
	Addr     string
	Location string
	// TotalExt is total_ext_cnt; 0 means FreeExt, a node with nothing on it.
	TotalExt uint64
	FreeExt  uint64
	Disabled bool
	Sides    []*pb.SidePointer
}

// Check refuses a disk node no key could be written for.
func (spec DnSpec) Check() error {
	return checkNode(spec.Addr, spec.Shard)
}

// CnSpec is one controller node to plant: DnSpec's mirror.
type CnSpec struct {
	// Id is the cn_id; 0 takes the cluster's next free one.
	Id    uint64
	Shard uint32
	// Addr is the cn agent's ip:port, which CnConf is keyed by.
	Addr     string
	Location string
	// TotalExt is total_ext_cnt; 0 means FreeExt, a node with nothing on it.
	TotalExt uint64
	FreeExt  uint64
	Disabled bool
	Cntlrs   []*pb.CntlrPointer
}

// Check refuses a controller node no key could be written for.
func (spec CnSpec) Check() error {
	return checkNode(spec.Addr, spec.Shard)
}

// checkNode is the rule both node specs share: the endpoint is a key field,
// and a shard code has a two-digit spelling and a bucket slot only below
// common.ShardBucketSize (architecture.md, Globals: id allocation + shard
// buckets).
func checkNode(addr string, shard uint32) error {
	if addr == "" {
		return fmt.Errorf("the node's addr_port is empty")
	}
	if shard >= common.ShardBucketSize {
		return fmt.Errorf("shard code %#x is out of range (00..%02x)",
			shard, common.ShardBucketSize-1)
	}
	return nil
}

// Dn is one planted disk node: its endpoint, the DnConf as written and the
// revision its DnRev holds.
type Dn struct {
	Addr     string
	Conf     *pb.DnConf
	Revision uint64
}

// Cn is one planted controller node: Dn's mirror.
type Cn struct {
	Addr     string
	Conf     *pb.CnConf
	Revision uint64
}

// PutDn implements CreateDiskNode with an explicit id, shard and budget
// (architecture.md, Disk nodes): ONE STM writes DnConf, DnRev (revision =
// current + 1, or 1 when absent) and the DnCapacity key through
// model.MaintainDnCapacity, so the presence rule of architecture.md, Capacity
// index keys, holds the moment the transaction commits. Creating a node
// moves DnGlobal's next_id past its id and counts it in its shard bucket.
func PutDn(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	spec DnSpec,
) (Dn, error) {
	dns, err := PutDns(ctx, cli, cl, []DnSpec{spec})
	if err != nil {
		return Dn{}, err
	}
	return dns[0], nil
}

// PutDns is PutDn for a list, in order, nodesPerTxn nodes per transaction. A
// node a transaction writes is seen by the later nodes of the same
// transaction, so the list leaves what the same PutDn calls would. A
// failure leaves the transactions before it committed and returns the nodes
// they wrote.
func PutDns(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	specs []DnSpec,
) ([]Dn, error) {
	for i, spec := range specs {
		if err := spec.Check(); err != nil {
			return nil, fmt.Errorf("dn %d (%s): %w", i, spec.Addr, err)
		}
	}
	out := make([]Dn, 0, len(specs))
	for start := 0; start < len(specs); start += nodesPerTxn {
		batch := specs[start:min(start+nodesPerTxn, len(specs))]
		var written []Dn
		err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
			written = written[:0]
			for _, spec := range batch {
				dn, err := putDnIn(s, cl, spec)
				if err != nil {
					return err
				}
				written = append(written, dn)
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		out = append(out, written...)
	}
	return out, nil
}

// putDnIn is one PutDn inside a caller's STM.
func putDnIn(s etcdutil.STM, cl Cluster, spec DnSpec) (Dn, error) {
	totalExt := spec.TotalExt
	if totalExt == 0 {
		totalExt = spec.FreeExt
	}
	globalKey := model.DnGlobalKey(cl.Id)
	id := spec.Id
	if id == 0 {
		global := &pb.DnGlobal{}
		if !s.Get(globalKey, global) {
			return Dn{}, fmt.Errorf("%q not found, so no dn_id to take",
				globalKey)
		}
		id = max(global.GetNextId(), 1)
	}
	newDn := &pb.DnConf{
		DnId:        id,
		ShardCode:   spec.Shard,
		Disabled:    spec.Disabled,
		ErrEpoch:    0,
		NvmeTrConf:  nvmeTrConfOf(spec.Addr),
		Location:    spec.Location,
		SidePtrList: clonePtrs(spec.Sides),
		TotalExtCnt: totalExt,
		FreeExtCnt:  spec.FreeExt,
	}
	confKey := model.DnConfKey(cl.Id, spec.Addr)
	old := &pb.DnConf{}
	var oldDn *pb.DnConf
	if s.Get(confKey, old) {
		oldDn = old
	}
	revKey := model.DnRevKey(spec.Shard, cl.Id, id)
	rev := &pb.DnRev{}
	created := !s.Get(revKey, rev)
	revision := rev.GetRevision() + 1
	s.Put(confKey, newDn)
	model.MaintainDnCapacity(s, cl.Id, spec.Addr, cl.Conf, oldDn, newDn)
	s.Put(revKey, &pb.DnRev{AddrPort: spec.Addr, Revision: revision})
	if created {
		// Creating a node consumes an id and one bucket slot
		// (architecture.md, Globals: id allocation + shard buckets), so a
		// worker reaction that allocates later never reuses this id.
		global := &pb.DnGlobal{}
		if s.Get(globalKey, global) {
			if global.NextId <= id {
				global.NextId = id + 1
			}
			if int(spec.Shard) < len(global.ShardBucket) {
				global.ShardBucket[spec.Shard]++
			}
			s.Put(globalKey, global)
		}
	}
	return Dn{Addr: spec.Addr, Conf: newDn, Revision: revision}, nil
}

// PutCn is PutDn's mirror for a controller node (architecture.md,
// Controller nodes): CnConf, CnRev and the CnCapacity key through
// model.MaintainCnCapacity.
func PutCn(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	spec CnSpec,
) (Cn, error) {
	cns, err := PutCns(ctx, cli, cl, []CnSpec{spec})
	if err != nil {
		return Cn{}, err
	}
	return cns[0], nil
}

// PutCns is PutDns's mirror for controller nodes.
func PutCns(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	specs []CnSpec,
) ([]Cn, error) {
	for i, spec := range specs {
		if err := spec.Check(); err != nil {
			return nil, fmt.Errorf("cn %d (%s): %w", i, spec.Addr, err)
		}
	}
	out := make([]Cn, 0, len(specs))
	for start := 0; start < len(specs); start += nodesPerTxn {
		batch := specs[start:min(start+nodesPerTxn, len(specs))]
		var written []Cn
		err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
			written = written[:0]
			for _, spec := range batch {
				cn, err := putCnIn(s, cl, spec)
				if err != nil {
					return err
				}
				written = append(written, cn)
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		out = append(out, written...)
	}
	return out, nil
}

// putCnIn is one PutCn inside a caller's STM.
func putCnIn(s etcdutil.STM, cl Cluster, spec CnSpec) (Cn, error) {
	totalExt := spec.TotalExt
	if totalExt == 0 {
		totalExt = spec.FreeExt
	}
	globalKey := model.CnGlobalKey(cl.Id)
	id := spec.Id
	if id == 0 {
		global := &pb.CnGlobal{}
		if !s.Get(globalKey, global) {
			return Cn{}, fmt.Errorf("%q not found, so no cn_id to take",
				globalKey)
		}
		id = max(global.GetNextId(), 1)
	}
	newCn := &pb.CnConf{
		CnId:         id,
		ShardCode:    spec.Shard,
		Disabled:     spec.Disabled,
		ErrEpoch:     0,
		NvmeTrConf:   nvmeTrConfOf(spec.Addr),
		Location:     spec.Location,
		CntlrPtrList: clonePtrs(spec.Cntlrs),
		TotalExtCnt:  totalExt,
		FreeExtCnt:   spec.FreeExt,
	}
	confKey := model.CnConfKey(cl.Id, spec.Addr)
	old := &pb.CnConf{}
	var oldCn *pb.CnConf
	if s.Get(confKey, old) {
		oldCn = old
	}
	revKey := model.CnRevKey(spec.Shard, cl.Id, id)
	rev := &pb.CnRev{}
	created := !s.Get(revKey, rev)
	revision := rev.GetRevision() + 1
	s.Put(confKey, newCn)
	model.MaintainCnCapacity(s, cl.Id, spec.Addr, oldCn, newCn)
	s.Put(revKey, &pb.CnRev{AddrPort: spec.Addr, Revision: revision})
	if created {
		global := &pb.CnGlobal{}
		if s.Get(globalKey, global) {
			if global.NextId <= id {
				global.NextId = id + 1
			}
			if int(spec.Shard) < len(global.ShardBucket) {
				global.ShardBucket[spec.Shard]++
			}
			s.Put(globalKey, global)
		}
	}
	return Cn{Addr: spec.Addr, Conf: newCn, Revision: revision}, nil
}

// clonePtrs copies a pointer list, so that a stored record never aliases the
// caller's spec.
func clonePtrs[T proto.Message](ptrs []T) []T {
	if len(ptrs) == 0 {
		return nil
	}
	out := make([]T, 0, len(ptrs))
	for _, ptr := range ptrs {
		out = append(out, proto.Clone(ptr).(T))
	}
	return out
}

// DnAddr resolves a dn_id to the addr_port its record is keyed by.
func DnAddr(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	id uint64,
) (string, error) {
	return findNodeAddr(ctx, cli, cl.Id, "dn_conf", id)
}

// CnAddr resolves a cn_id to the addr_port its record is keyed by.
func CnAddr(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	id uint64,
) (string, error) {
	return findNodeAddr(ctx, cli, cl.Id, "cn_conf", id)
}

// findNodeAddr resolves a dn_id / cn_id to the addr_port its record is keyed
// by: DnConf and CnConf are keyed by endpoint, several calls take an id, so
// exactly one helper scans the cluster's conf prefix for the record that
// carries the id.
func findNodeAddr(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	kind string,
	id uint64,
) (string, error) {
	var prefix string
	switch kind {
	case "dn_conf":
		prefix = model.DnConfPrefix(cid)
	case "cn_conf":
		prefix = model.CnConfPrefix(cid)
	default:
		return "", fmt.Errorf("findNodeAddr: unknown kind %q", kind)
	}
	kvs, _, err := cli.Range(ctx, prefix)
	if err != nil {
		return "", err
	}
	found := ""
	for _, kv := range kvs {
		addrPort := strings.TrimPrefix(kv.Key, prefix)
		var stored uint64
		if kind == "dn_conf" {
			dn := &pb.DnConf{}
			if err := cli.Decode(ctx, kv, dn); err != nil {
				return "", err
			}
			stored = dn.GetDnId()
		} else {
			cn := &pb.CnConf{}
			if err := cli.Decode(ctx, kv, cn); err != nil {
				return "", err
			}
			stored = cn.GetCnId()
		}
		if stored != id {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("%s %#x is stored at both %q and %q",
				kind, id, found, addrPort)
		}
		found = addrPort
	}
	if found == "" {
		return "", fmt.Errorf("no %s with id %#x under prefix %q",
			kind, id, prefix)
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Revision keys (architecture.md, Revision keys and the sync fan-out)
// ---------------------------------------------------------------------------

// RevKind names which of the three revision keys BumpRev and DelRev address.
type RevKind string

const (
	RevDn RevKind = "dn"
	RevCn RevKind = "cn"
	RevSp RevKind = "sp"
)

// revKey is the key of one revision of kind, and a fresh message to read it
// into.
func revKey(
	kind RevKind,
	shard uint32,
	cid uint64,
	id uint64,
) (string, revMsg, error) {
	switch kind {
	case RevDn:
		return model.DnRevKey(shard, cid, id), &pb.DnRev{}, nil
	case RevCn:
		return model.CnRevKey(shard, cid, id), &pb.CnRev{}, nil
	case RevSp:
		return model.SpRevKey(shard, cid, id), &pb.SpRev{}, nil
	}
	return "", nil, fmt.Errorf("want a dn, cn or sp revision, got %q", kind)
}

// revMsg is what the three revision messages share.
type revMsg interface {
	proto.Message
	GetRevision() uint64
}

// setRevision writes a revision into one of the three revision messages.
func setRevision(msg revMsg, revision uint64) {
	switch rev := msg.(type) {
	case *pb.DnRev:
		rev.Revision = revision
	case *pb.CnRev:
		rev.Revision = revision
	case *pb.SpRev:
		rev.Revision = revision
	}
}

// BumpRev raises one revision key IN PLACE (architecture.md, Revision keys
// and the sync fan-out): the key is id-based and therefore stable, so a
// watcher sees one put — never a delete followed by a put, which would read
// as "node removed, then a different node added". It returns the new
// revision.
func BumpRev(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	kind RevKind,
	shard uint32,
	id uint64,
) (uint64, error) {
	key, rev, err := revKey(kind, shard, cl.Id, id)
	if err != nil {
		return 0, err
	}
	revision := uint64(0)
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		revision = 0
		if !s.Get(key, rev) {
			return fmt.Errorf("%q not found", key)
		}
		revision = rev.GetRevision() + 1
		setRevision(rev, revision)
		s.Put(key, rev)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

// DelRev deletes one revision key and nothing else — the "the object is gone
// as far as the watching worker is concerned" case of SW3. It returns the key
// and the revision it held.
func DelRev(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	kind RevKind,
	shard uint32,
	id uint64,
) (string, uint64, error) {
	key, rev, err := revKey(kind, shard, cl.Id, id)
	if err != nil {
		return "", 0, err
	}
	revision := uint64(0)
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		revision = 0
		if !s.Get(key, rev) {
			return fmt.Errorf("%q not found", key)
		}
		revision = rev.GetRevision()
		s.Del(key)
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return key, revision, nil
}

// MoveDn is the moved-node case of architecture.md, Revision keys and the
// sync fan-out: one STM rewrites DnRev.addr_port and bumps its revision, and
// moves DnConf and the DnCapacity key to the new endpoint. The rev key itself
// is rewritten, never deleted and re-created, so the watching worker sees a
// single put that says "the same DN now answers somewhere else".
//
// It rewrites nothing else: Sides already placed on the DN keep the old
// addr_port copy their Slice, or for a side of a clone meta group their Clone,
// holds, because the rev keys have no rename path and no RPC changes a node's
// endpoint. A case moves only a DN that carries no side. It returns the old
// endpoint and the new revision.
func MoveDn(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	id uint64,
	shard uint32,
	addr string,
) (string, uint64, error) {
	if addr == "" {
		return "", 0, fmt.Errorf("the new addr_port is empty")
	}
	oldAddr, err := DnAddr(ctx, cli, cl, id)
	if err != nil {
		return "", 0, err
	}
	if oldAddr == addr {
		return "", 0, fmt.Errorf("dn %#x is already at %q", id, addr)
	}
	cid := cl.Id
	revision := uint64(0)
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		revision = 0
		oldKey := model.DnConfKey(cid, oldAddr)
		oldDn := &pb.DnConf{}
		if !s.Get(oldKey, oldDn) {
			return fmt.Errorf("%q not found", oldKey)
		}
		if oldDn.GetDnId() != id {
			return fmt.Errorf(
				"%q holds dn %#x, want %#x", oldKey, oldDn.GetDnId(), id,
			)
		}
		newKey := model.DnConfKey(cid, addr)
		if s.Get(newKey, &pb.DnConf{}) {
			return fmt.Errorf("%q already exists", newKey)
		}
		newDn := proto.Clone(oldDn).(*pb.DnConf)
		newDn.NvmeTrConf = nvmeTrConfOf(addr)
		s.Del(oldKey)
		model.MaintainDnCapacity(s, cid, oldAddr, cl.Conf, oldDn, nil)
		s.Put(newKey, newDn)
		model.MaintainDnCapacity(s, cid, addr, cl.Conf, nil, newDn)
		revKey := model.DnRevKey(shard, cid, id)
		rev := &pb.DnRev{}
		if !s.Get(revKey, rev) {
			return fmt.Errorf("%q not found", revKey)
		}
		rev.AddrPort = addr
		rev.Revision++
		revision = rev.Revision
		s.Put(revKey, rev)
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return oldAddr, revision, nil
}

// bumpSpRev rewrites the SP's revision key with revision + 1 IN PLACE
// (architecture.md, Revision keys and the sync fan-out): the key is id-based
// and therefore stable, so a watcher must see one put, not a delete followed
// by a put. Every SpConf sub-object mutation calls it exactly once per
// transaction. It is model.BumpSpRev that also returns the revision.
func bumpSpRev(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) (uint64, error) {
	key := model.SpRevKey(conf.GetShardCode(), cid, conf.GetSpId())
	rev := &pb.SpRev{}
	if !s.Get(key, rev) {
		return 0, fmt.Errorf("sp rev key %q not found", key)
	}
	rev.Revision++
	s.Put(key, rev)
	return rev.Revision, nil
}

// bumpDnRev bumps one DN's revision key in place
// (architecture.md, Revision keys and the sync fan-out).
func bumpDnRev(s etcdutil.STM, cid uint64, dn *pb.DnConf) (uint64, error) {
	key := model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId())
	rev := &pb.DnRev{}
	if !s.Get(key, rev) {
		return 0, fmt.Errorf("dn rev key %q not found", key)
	}
	rev.Revision++
	s.Put(key, rev)
	return rev.Revision, nil
}

// bumpCnRev bumps one CN's revision key in place
// (architecture.md, Revision keys and the sync fan-out).
func bumpCnRev(s etcdutil.STM, cid uint64, cn *pb.CnConf) (uint64, error) {
	key := model.CnRevKey(cn.GetShardCode(), cid, cn.GetCnId())
	rev := &pb.CnRev{}
	if !s.Get(key, rev) {
		return 0, fmt.Errorf("cn rev key %q not found", key)
	}
	rev.Revision++
	s.Put(key, rev)
	return rev.Revision, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// nvmeTrConfOf builds the transport record of a node from its gRPC endpoint.
// The fakes never open an NVMe-oF port, so only the shape matters: these
// fields are copied into the Sides PutSp and PutMigr write, the Cntlrs PutSp
// writes and the CdcEntry PutSs builds; the worker sends a migration source
// side's as src_nvme_tr_conf.
func nvmeTrConfOf(addrPort string) *pb.NvmeTrConf {
	host, port := addrPort, nvmeTrSvcId
	if idx := strings.LastIndex(addrPort, ":"); idx >= 0 {
		host, port = addrPort[:idx], addrPort[idx+1:]
	}
	// tr_svc_id is the node's own port, not the fixed nvmeTrSvcId: the fake
	// agents never open an NVMe-oF port, so the value is inert, and every
	// node of the worker suite's topology shares one host address
	// (dnv-worker.md, Integration test plan, Topology). A constant here would
	// make every node's NvmeTrConf byte-identical, and a CdcEntry's
	// nvme_tr_conf_list would then be unattributable to a particular CN —
	// which is exactly what worker_test.sh case D step 3 has to assert
	// ("CdcEntry of ss0 lists cn 2 and cn 3, not cn 1").
	return &pb.NvmeTrConf{
		TrType:  nvmeTrType,
		AdrFam:  nvmeAdrFam,
		TrAddr:  host,
		TrSvcId: port,
	}
}

// nsIdentity derives a namespace's dev_uuid / dev_nguid from (cluster_id,
// sp_id, ns_id). The gateway generates a random v4 uuid and 16 random bytes
// (architecture.md, Subsystems, namespaces: CreateNamespace); a test library
// must instead be reproducible, so the same namespace of the same cluster
// always gets the same identity.
func nsIdentity(cid uint64, spId uint64, nsId uint64) (string, string) {
	var raw [16]byte
	high := fnv.New64a()
	fmt.Fprintf(high, "dnv-ns-uuid %016x %016x %016x", cid, spId, nsId)
	binary.BigEndian.PutUint64(raw[0:8], high.Sum64())
	low := fnv.New64a()
	fmt.Fprintf(low, "dnv-ns-nguid %016x %016x %016x", cid, spId, nsId)
	binary.BigEndian.PutUint64(raw[8:16], low.Sum64())
	// The RFC 4122 v4 shape: version 4, variant 10xx.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	nguid := hex.EncodeToString(raw[:])
	uuid := strings.Join([]string{
		nguid[0:8], nguid[8:12], nguid[12:16], nguid[16:20], nguid[20:32],
	}, "-")
	return uuid, nguid
}

// cloneBdevConf copies a BdevConf for mutation. proto.Clone of a nil message
// returns a read-only zero, so a cluster written without a bdev_conf needs a
// fresh one.
func cloneBdevConf(src *pb.BdevConf) *pb.BdevConf {
	if src == nil {
		return &pb.BdevConf{}
	}
	return proto.Clone(src).(*pb.BdevConf)
}

// RedundConf is the redund_conf PutSp stores for a pool of md-raid1 groups
// with chunkBlocks blocks per bitmap chunk, or of redund_none groups, which
// is how a caller computes a group's geometry the way PutSp does
// (model.GroupBlocks).
func RedundConf(raid1 bool, chunkBlocks uint64) *pb.RedundConf {
	if raid1 {
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: chunkBlocks,
				},
			},
		}
	}
	return &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundNone{
			RedundNone: &pb.RedundNone{},
		},
	}
}

// advanceNextId keeps SpConf.next_id past every id the caller assigned, so
// that a worker reaction allocates fresh ids ABOVE them (architecture.md,
// Globals: id allocation + shard buckets).
func advanceNextId(conf *pb.SpConf, ids ...uint64) {
	for _, id := range ids {
		if conf.NextId <= id {
			conf.NextId = id + 1
		}
	}
}

// takeId is a sub-object id: the spec's own, or, for 0, the pool's next free
// one, taken through model.TakeSpIds as every create takes it, so a counter
// with no id left refuses the call that would take one (architecture.md,
// Globals: id allocation + shard buckets). Either way next_id moves past it.
// op names the call in the refusal.
func takeId(conf *pb.SpConf, op string, id uint64) (uint64, error) {
	if id == 0 {
		return model.TakeSpIds(conf, op, 1)
	}
	advanceNextId(conf, id)
	return id, nil
}

// containsName reports whether a name list already holds name.
func containsName(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}
