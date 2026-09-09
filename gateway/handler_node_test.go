package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md §9.3 for architecture.md §8.1 (the four cluster
// RPCs), §8.2 (the six disk-node RPCs) and §8.3 (the six controller-node
// RPCs), driven against the real etcd of §9.1 through a Server built by
// newTestServer.
//
// Every happy path asserts the EXACT keys the RPC is specified to write and
// nothing else: the whole stored message through proto.Equal, the capacity key
// as a key STRING built from the numbers §5.6 puts in it, and the globals'
// next_id and shard_bucket. A handler that wrote the right fields under a
// slightly wrong key would pass a getter-by-getter test and fail here, which
// is the point — the key layout is the contract every worker and the allocator
// read the store through, and no other test in the package pins it.
//
// The refusal cases all assert the same second half: nothing was written. GW6
// says a mutator returns its error before any Put and an error out of the STM
// closure aborts the transaction uncommitted, so "nothing written" is checked
// as an unchanged mod_revision rather than as an unchanged value — a Put of
// the identical bytes still moves the revision, and that is a write.

// hnodeName is a name no other test in the package can collide with: the "hn-"
// prefix is this file's, and testSeq is the run-wide counter that keeps two
// iterations of the same test under -count=2 apart. Cluster names are keys
// (§5.2) and the etcd server is shared, so a fixed literal would make a test
// depend on whether an earlier one had run.
func hnodeName(kind string) string {
	return fmt.Sprintf("hn-%s-%d", kind, testSeq.Add(1))
}

// hnodeGet reads one key into msg, failing when it is absent.
func hnodeGet(t *testing.T, s *Server, key string, msg proto.Message) {
	t.Helper()
	found, err := s.cli.Get(context.Background(), key, msg)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	if !found {
		t.Fatalf("Get %s: not found", key)
	}
}

// hnodeModRev is one key's mod_revision, 0 when the key does not exist.
//
// It is a keys-only range rather than a point read because it must work on a
// key whose message type the caller has no reason to know — a capacity key, a
// global, a conf — and because the mod_revision is what "untouched" means: two
// assertions in this file (§0 #17's idempotent no-write and every "nothing was
// written" refusal) are exactly the statement that this number did not move.
func hnodeModRev(t *testing.T, s *Server, key string) int64 {
	t.Helper()
	keys, _, err := s.cli.RangeKeys(context.Background(), key)
	if err != nil {
		t.Fatalf("RangeKeys %s: %v", key, err)
	}
	for _, entry := range keys {
		if entry.Key == key {
			return entry.ModRev
		}
	}
	return 0
}

// hnodeExists reports whether a key is present.
func hnodeExists(t *testing.T, s *Server, key string) bool {
	t.Helper()
	return hnodeModRev(t, s, key) != 0
}

// hnodeWantMsg asserts that a refusal explains itself with the sentence its
// spec gives it. The integration suite greps these strings, so a handler that
// returned the right code with a different reason would still break §10.
func hnodeWantMsg(t *testing.T, err error, want string, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got no error, want one saying %q", label, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %v does not mention %q", label, err, want)
	}
}

// hnodeWantProto asserts one stored message field for field.
func hnodeWantProto(
	t *testing.T,
	got proto.Message,
	want proto.Message,
	label string,
) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Errorf("%s:\n got %v\nwant %v", label, got, want)
	}
}

// hnodeZeroBucket is the shard_bucket a global carries while the cluster holds
// no object of that kind: ShardBucketSize zeros (§5.4).
func hnodeZeroBucket() []uint32 {
	return make([]uint32, common.ShardBucketSize)
}

// hnodeBucket is hnodeZeroBucket with one entry per shard code listed, which is
// what a cluster that has minted exactly those ids looks like (GW12).
func hnodeBucket(shards ...uint32) []uint32 {
	bucket := hnodeZeroBucket()
	for _, shard := range shards {
		bucket[shard]++
	}
	return bucket
}

// hnodeDnCapacityKeys is every DN capacity key of one cluster, over all four
// bins of §6.2.
//
// The §5.6 rule is a presence rule — the key exists if and only if the node is
// allocatable — so the SET of keys is what a create or an update has to be
// checked against. A point read of the key the test expects would miss the
// interesting failure: a key written under the wrong bin or the wrong
// free_ext_cnt is invisible to the allocator's walk and to that read alike.
func hnodeDnCapacityKeys(t *testing.T, s *Server, cid uint64) []string {
	t.Helper()
	keys := []string{}
	for bin := uint32(0); bin < 4; bin++ {
		found, _, err := s.cli.RangeKeys(
			context.Background(), model.DnCapacityPrefix(cid, bin))
		if err != nil {
			t.Fatalf("RangeKeys dn_capacity bin %d: %v", bin, err)
		}
		for _, entry := range found {
			keys = append(keys, entry.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// hnodeCnCapacityKeys is hnodeDnCapacityKeys for a CN, which needs one scan
// because CN capacity keys carry no bin index (§6.4).
func hnodeCnCapacityKeys(t *testing.T, s *Server, cid uint64) []string {
	t.Helper()
	keys := []string{}
	found, _, err := s.cli.RangeKeys(
		context.Background(), model.CnCapacityPrefix(cid))
	if err != nil {
		t.Fatalf("RangeKeys cn_capacity: %v", err)
	}
	for _, entry := range found {
		keys = append(keys, entry.Key)
	}
	sort.Strings(keys)
	return keys
}

// hnodeWantKeys asserts a whole key set, sorted.
func hnodeWantKeys(t *testing.T, got []string, want []string, label string) {
	t.Helper()
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s:\n got %v\nwant %v", label, got, want)
	}
}

// ---------------------------------------------------------------------------
// §8.1 CreateCluster
// ---------------------------------------------------------------------------

// TestCreateClusterWritesConfAndThreeGlobals pins §8.1's write set and §5.2's
// derived id: one ClusterConf under the name plus the three globals under the
// id, and cluster_id = fnv64a(name ‖ creation_epoch) of the epoch the handler
// stamped — the reply's id must be exactly the one recomputable from what was
// stored, or no later RPC could address the cluster's keys at all.
//
// The five sub-messages are asserted VERBATIM (GW11): ClusterConf is
// write-once and defaults are resolved at use time, so a handler that helpfully
// filled in DefaultDnExtSize here would freeze a default into every DN's disk
// header for the life of the cluster.
func TestCreateClusterWritesConfAndThreeGlobals(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := hnodeName("cluster")
	req := &pb.CreateClusterRequest{
		ClusterName: name,
		QosRatio: &pb.QosRatio{
			Strict:       true,
			BytesPerIops: 4096,
			BytesPerBps:  8192,
		},
		BdevConf: &pb.BdevConf{
			DmPoolConf:  &pb.DmPoolConf{DataBlockSize: 2 * 1024 * 1024},
			DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: 128 * 1024},
		},
		DnBinConf:       &pb.DnBinConf{ExtentSize: 2 << 30},
		AllocConf:       &pb.AllocConf{DnBatchSize: 8, CnBatchSize: 4},
		HealthCheckConf: &pb.HealthCheckConf{DnInterval: 11, CntlrInterval: 14},
	}
	before := uint64(time.Now().UnixNano())
	reply, err := s.CreateCluster(ctx, req)
	after := uint64(time.Now().UnixNano())
	wantCode(t, err, codes.OK, "CreateCluster")

	cc := &pb.ClusterConf{}
	hnodeGet(t, s, model.ClusterConfKey(name), cc)
	epoch := cc.GetCreationEpoch()
	if epoch < before || epoch > after {
		t.Errorf(
			"creation_epoch %d is outside the call [%d, %d]",
			epoch, before, after)
	}
	cid := model.ClusterId(name, epoch)
	if reply.GetClusterId() != cid {
		t.Errorf(
			"cluster_id: replied %#016x, want %#016x = fnv64a(name ‖ epoch)",
			reply.GetClusterId(), cid)
	}
	hnodeWantProto(t, cc, &pb.ClusterConf{
		CreationEpoch:   epoch,
		QosRatio:        req.GetQosRatio(),
		BdevConf:        req.GetBdevConf(),
		DnBinConf:       req.GetDnBinConf(),
		AllocConf:       req.GetAllocConf(),
		HealthCheckConf: req.GetHealthCheckConf(),
	}, "stored cluster_conf")

	// next_id starts at 1 and the bucket is ShardBucketSize zeros (§5.4). A
	// fresh slice per global: they are three independent counters and an
	// aliased bucket would make one kind's mint move another kind's shard.
	dnGlobal := &pb.DnGlobal{}
	hnodeGet(t, s, model.DnGlobalKey(cid), dnGlobal)
	hnodeWantProto(t, dnGlobal, &pb.DnGlobal{
		NextId:      1,
		ShardBucket: hnodeZeroBucket(),
	}, "dn_global")
	cnGlobal := &pb.CnGlobal{}
	hnodeGet(t, s, model.CnGlobalKey(cid), cnGlobal)
	hnodeWantProto(t, cnGlobal, &pb.CnGlobal{
		NextId:      1,
		ShardBucket: hnodeZeroBucket(),
	}, "cn_global")
	spGlobal := &pb.SpGlobal{}
	hnodeGet(t, s, model.SpGlobalKey(cid), spGlobal)
	hnodeWantProto(t, spGlobal, &pb.SpGlobal{
		NextId:      1,
		ShardBucket: hnodeZeroBucket(),
	}, "sp_global")
}

// TestCreateClusterNameCollisionWritesNothing pins the ALREADY_EXISTS of §8.1
// and the rule behind every refusal in this file: the closure returns before
// its first Put, so the stored ClusterConf is not merely still correct, it was
// not rewritten — its mod_revision has not moved.
func TestCreateClusterNameCollisionWritesNothing(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := hnodeName("dup")
	cid := mustCluster(t, s, name)
	confKey := model.ClusterConfKey(name)
	confRev := hnodeModRev(t, s, confKey)
	globalRev := hnodeModRev(t, s, model.DnGlobalKey(cid))

	_, err := s.CreateCluster(ctx, &pb.CreateClusterRequest{
		ClusterName: name,
		QosRatio:    &pb.QosRatio{Strict: true},
	})
	wantCode(t, err, codes.AlreadyExists, "CreateCluster of an existing name")
	hnodeWantMsg(t, err, "already exists", "CreateCluster of an existing name")

	if got := hnodeModRev(t, s, confKey); got != confRev {
		t.Errorf("cluster_conf mod_revision moved: %d -> %d", confRev, got)
	}
	if got := hnodeModRev(t, s, model.DnGlobalKey(cid)); got != globalRev {
		t.Errorf("dn_global mod_revision moved: %d -> %d", globalRev, got)
	}
	cc := &pb.ClusterConf{}
	hnodeGet(t, s, confKey, cc)
	if cc.GetQosRatio().GetStrict() {
		t.Errorf("the refused request's qos_ratio reached the store")
	}
}

// The knobs of TestCreateClusterRefusesAClusterIdInUse. hnodeIdWindow is how
// many consecutive nanosecond epochs one attempt pre-claims, hnodeIdLead how
// far below the calibrated lag the window starts, and hnodeIdAttempts how many
// windows the test gets before it gives up. The window is ~30 µs wide around a
// lag measured on the same machine microseconds earlier, which covers the
// scheduling jitter of the handler's own prologue several times over; a miss
// doubles it, so the loop converges instead of just retrying.
const (
	hnodeIdWindow   = 32768
	hnodeIdLead     = 8192
	hnodeIdAttempts = 6
)

// hnodeStampLag measures how long after a busy-wait exit CreateCluster stamps
// its creation_epoch, under exactly the protocol the attempt loop uses: spin to
// a deadline, call, read back what was stored. It is the smallest of a few
// samples, because the loop wants the FLOOR of the lag — the window extends
// upwards from it, and an outlier taken as the centre would put the whole
// window above every ordinary attempt.
//
// The clusters it creates are deleted again, so it leaves the store exactly as
// it found it and the caller can reuse the name.
func hnodeStampLag(t *testing.T, s *Server, name string) int64 {
	t.Helper()
	ctx := context.Background()
	lag := int64(0)
	for sample := 0; sample < 3; sample++ {
		open := time.Now().Add(20 * time.Millisecond).UnixNano()
		for time.Now().UnixNano() < open {
		}
		if _, err := s.CreateCluster(
			ctx, &pb.CreateClusterRequest{ClusterName: name},
		); err != nil {
			t.Fatalf("CreateCluster while calibrating: %v", err)
		}
		got := int64(testClusterEpoch(t, s, name)) - open
		if sample == 0 || got < lag {
			lag = got
		}
		if _, err := s.DeleteCluster(
			ctx, &pb.DeleteClusterRequest{ClusterName: name},
		); err != nil {
			t.Fatalf("DeleteCluster while calibrating: %v", err)
		}
	}
	return lag
}

// hnodeIdKeys is the DnGlobal key that `name` would take for each of the
// `count` epochs starting at `base` — the set of cluster_ids one attempt
// claims.
func hnodeIdKeys(name string, base int64, count int) []string {
	keys := make([]string, 0, count)
	for i := 0; i < count; i++ {
		keys = append(
			keys,
			model.DnGlobalKey(model.ClusterId(name, uint64(base+int64(i)))),
		)
	}
	return keys
}

// hnodeApplyIds writes, or deletes, every key of a claim.
//
// One STM per batch and several batches in flight, because a claim has to
// finish inside the setup budget of its attempt: tens of thousands of point
// writes would not, and neither would the same number of sequential
// transactions. The batch size stays under etcd's default 128-op transaction
// limit.
func hnodeApplyIds(t *testing.T, s *Server, keys []string, remove bool) {
	t.Helper()
	const batch = 100
	const lanes = 8
	jobs := make(chan []string)
	errs := make(chan error, lanes)
	var wg sync.WaitGroup
	for lane := 0; lane < lanes; lane++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range jobs {
				err := s.cli.RunSTM(
					context.Background(),
					func(stm etcdutil.STM) error {
						for _, key := range chunk {
							if remove {
								stm.Del(key)
								continue
							}
							stm.Put(key, &pb.DnGlobal{NextId: 1})
						}
						return nil
					})
				if err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
	}
	for start := 0; start < len(keys); start += batch {
		jobs <- keys[start:min(start+batch, len(keys))]
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("apply %d claimed cluster_ids: %v", len(keys), err)
	default:
	}
}

// TestCreateClusterRefusesAClusterIdInUse pins §8.1's hash-collision guard: a
// cluster whose derived cluster_id is already carrying a global is refused with
// ALREADY_EXISTS and writes nothing, because the alternative is two clusters
// silently sharing every key prefix in the store.
//
// The branch cannot be reached the way the other refusals are. cluster_id is
// fnv64a(name ‖ creation_epoch) and creation_epoch is a clock read INSIDE the
// handler, so no request field influences which id a create will take, and the
// name that would collide with a live cluster cannot be computed either — with
// the same epoch a name collision needs a 64-bit fnv1a collision, and with a
// different epoch no name collides at all.
//
// So the test claims the id of every epoch in a window: it writes the DnGlobal
// key `name` would map to for each nanosecond of a ~30 µs span, spins until the
// span opens and calls the handler, which stamps its epoch inside it. The span
// is placed by measuring the same handler's lag first, so it is calibrated to
// the machine rather than guessed. An attempt whose epoch missed is not a
// failure: the cluster it created says exactly what the lag was, so the next
// window is re-centred on the measured value and widened.
func TestCreateClusterRefusesAClusterIdInUse(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := hnodeName("idclash")
	window := hnodeIdWindow
	lag := hnodeStampLag(t, s, name) - hnodeIdLead
	budget := 400 * time.Millisecond
	for attempt := 0; attempt < hnodeIdAttempts; attempt++ {
		open := time.Now().Add(budget).UnixNano()
		keys := hnodeIdKeys(name, open+lag, window)
		hnodeApplyIds(t, s, keys, false)
		t.Cleanup(func() { hnodeApplyIds(t, s, keys, true) })
		if time.Now().UnixNano() > open-int64(10*time.Millisecond) {
			// Claiming the window took longer than the budget it was placed
			// after, so the window is already in the past. Nothing has been
			// learned; give the next attempt more room.
			budget *= 3
			continue
		}
		for time.Now().UnixNano() < open {
		}
		_, err := s.CreateCluster(
			ctx, &pb.CreateClusterRequest{ClusterName: name})
		if err != nil {
			wantCode(t, err, codes.AlreadyExists, "CreateCluster onto a live id")
			hnodeWantMsg(
				t, err, "already in use", "CreateCluster onto a live id")
			if hnodeExists(t, s, model.ClusterConfKey(name)) {
				t.Errorf("the refused CreateCluster wrote a cluster_conf")
			}
			return
		}
		// The stamped epoch fell outside the claim. Re-centre on it, widen,
		// and drop the cluster the miss created so the retry starts from the
		// state this attempt did.
		lag = int64(testClusterEpoch(t, s, name)) - open - int64(window/2)
		window *= 2
		if _, err := s.DeleteCluster(
			ctx, &pb.DeleteClusterRequest{ClusterName: name},
		); err != nil {
			t.Fatalf("DeleteCluster after a missed window: %v", err)
		}
	}
	t.Skipf(
		"the stamped creation_epoch never landed in %d attempts: the guard "+
			"branch was not exercised on this machine",
		hnodeIdAttempts)
}

// ---------------------------------------------------------------------------
// §8.1 DeleteCluster, GetCluster, ListClusters
// ---------------------------------------------------------------------------

// TestDeleteClusterRemovesConfAndGlobals pins §8.1's delete: the ClusterConf
// and all three globals go in one transaction, and the reply carries the id
// that has just stopped existing — it is derived, not stored, so a client that
// did not keep it could never name it again.
func TestDeleteClusterRemovesConfAndGlobals(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := hnodeName("del")
	cid := mustCluster(t, s, name)

	reply, err := s.DeleteCluster(
		ctx, &pb.DeleteClusterRequest{ClusterName: name})
	wantCode(t, err, codes.OK, "DeleteCluster")
	if reply.GetClusterId() != cid {
		t.Errorf(
			"cluster_id: got %#016x, want %#016x",
			reply.GetClusterId(), cid)
	}
	for _, key := range []string{
		model.ClusterConfKey(name),
		model.DnGlobalKey(cid),
		model.CnGlobalKey(cid),
		model.SpGlobalKey(cid),
	} {
		if hnodeExists(t, s, key) {
			t.Errorf("key %q survived DeleteCluster", key)
		}
	}
	_, err = s.GetCluster(ctx, &pb.GetClusterRequest{ClusterName: name})
	wantCode(t, err, codes.NotFound, "GetCluster after DeleteCluster")
}

// TestDeleteClusterBucketSumGate pins the emptiness precondition of §8.1: it is
// sum(shard_bucket) over all THREE globals, not a range read, because §5.4
// makes that sum the cluster's live object count by construction. Each row
// claims one bucket entry of one global and asserts the refusal names that
// kind, writes nothing, and stops naming it once the bucket is empty again.
func TestDeleteClusterBucketSumGate(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cli := newTestClient(t)
	for _, tc := range []struct {
		label string
		key   func(cid uint64) string
		held  func(bucket []uint32) proto.Message
		want  string
	}{
		{
			label: "a disk node",
			key:   model.DnGlobalKey,
			held: func(bucket []uint32) proto.Message {
				return &pb.DnGlobal{NextId: 2, ShardBucket: bucket}
			},
			want: "1 disk nodes, 0 controller nodes and 0 storage pools",
		},
		{
			label: "a controller node",
			key:   model.CnGlobalKey,
			held: func(bucket []uint32) proto.Message {
				return &pb.CnGlobal{NextId: 2, ShardBucket: bucket}
			},
			want: "0 disk nodes, 1 controller nodes and 0 storage pools",
		},
		{
			label: "a storage pool",
			key:   model.SpGlobalKey,
			held: func(bucket []uint32) proto.Message {
				return &pb.SpGlobal{NextId: 2, ShardBucket: bucket}
			},
			want: "0 disk nodes, 0 controller nodes and 1 storage pools",
		},
	} {
		name := hnodeName("gate")
		cid := mustCluster(t, s, name)
		key := tc.key(cid)
		mustPut(t, cli, key, tc.held(hnodeBucket(9)))
		confRev := hnodeModRev(t, s, model.ClusterConfKey(name))
		globalRev := hnodeModRev(t, s, key)

		_, err := s.DeleteCluster(
			ctx, &pb.DeleteClusterRequest{ClusterName: name})
		wantCode(t, err, codes.FailedPrecondition, tc.label)
		hnodeWantMsg(t, err, tc.want, tc.label)
		if got := hnodeModRev(
			t, s, model.ClusterConfKey(name)); got != confRev {
			t.Errorf("%s: cluster_conf was rewritten", tc.label)
		}
		if got := hnodeModRev(t, s, key); got != globalRev {
			t.Errorf("%s: %s was rewritten", tc.label, key)
		}

		// Releasing the bucket entry is all it takes: the gate reads the sum
		// and nothing else.
		mustPut(t, cli, key, tc.held(hnodeZeroBucket()))
		_, err = s.DeleteCluster(
			ctx, &pb.DeleteClusterRequest{ClusterName: name})
		wantCode(t, err, codes.OK, tc.label+" once the bucket is empty")
		if hnodeExists(t, s, model.ClusterConfKey(name)) {
			t.Errorf("%s: cluster_conf survived the delete", tc.label)
		}
	}
}

// TestGetClusterReadsConfAndGlobals pins §8.1's read: the reply carries the
// name, the derived id and all four messages at ONE store revision, and the
// globals show the mints that have happened — which is what makes GetCluster
// the only way a client learns a cluster's object counts.
func TestGetClusterReadsConfAndGlobals(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := hnodeName("get")
	cid := mustCluster(t, s, name)
	dnAddr := fakeAddrPort(t, "dn")
	mustDn(t, s, name, dnAddr, "", 16<<30)

	reply, err := s.GetCluster(ctx, &pb.GetClusterRequest{ClusterName: name})
	wantCode(t, err, codes.OK, "GetCluster")
	if reply.GetClusterName() != name {
		t.Errorf(
			"cluster_name: got %q, want %q", reply.GetClusterName(), name)
	}
	if reply.GetClusterId() != cid {
		t.Errorf(
			"cluster_id: got %#016x, want %#016x", reply.GetClusterId(), cid)
	}
	cc := &pb.ClusterConf{}
	hnodeGet(t, s, model.ClusterConfKey(name), cc)
	hnodeWantProto(t, reply.GetClusterConf(), cc, "cluster_conf")
	// One DN has been minted: next_id has moved on and the first shard bucket
	// carries it. The other two globals are still untouched (§5.4).
	hnodeWantProto(t, reply.GetDnGlobal(), &pb.DnGlobal{
		NextId:      2,
		ShardBucket: hnodeBucket(0),
	}, "dn_global")
	hnodeWantProto(t, reply.GetCnGlobal(), &pb.CnGlobal{
		NextId:      1,
		ShardBucket: hnodeZeroBucket(),
	}, "cn_global")
	hnodeWantProto(t, reply.GetSpGlobal(), &pb.SpGlobal{
		NextId:      1,
		ShardBucket: hnodeZeroBucket(),
	}, "sp_global")
}

// TestListClustersPagesAndRejectsABadToken pins GW10/§5.7 on the one list that
// resolves no cluster at all: a page is the key suffixes under the ClusterConf
// prefix, the next token is the last key of the page, and a token that is not
// base64 is INVALID_ARGUMENT rather than an empty first page.
//
// The etcd server is shared and the ClusterConf prefix is the whole store, so
// the page is scoped by a token this test synthesises: the key of the name its
// own three clusters are suffixes of, which sorts immediately before them. That
// makes the first page exact without assuming the store holds nothing else.
func TestListClustersPagesAndRejectsABadToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	prefix := hnodeName("list")
	names := []string{prefix + "-a", prefix + "-b", prefix + "-c"}
	for _, name := range names {
		mustCluster(t, s, name)
	}
	start := encodePageToken(model.ClusterConfKey(prefix))

	reply, err := s.ListClusters(ctx, &pb.ListClustersRequest{
		Count:     3,
		PageToken: start,
	})
	wantCode(t, err, codes.OK, "ListClusters")
	if got := fmt.Sprint(reply.GetClusterName()); got != fmt.Sprint(names) {
		t.Errorf("page: got %v, want %v", reply.GetClusterName(), names)
	}
	// A FULL page always carries a token, even when it happens to be the last
	// one: the client learns it has finished only from a short page.
	wantToken := encodePageToken(model.ClusterConfKey(names[2]))
	if reply.GetPageToken() != wantToken {
		t.Errorf(
			"page_token: got %q, want %q", reply.GetPageToken(), wantToken)
	}
	next, err := s.ListClusters(ctx, &pb.ListClustersRequest{
		Count:     common.MaxListCnt,
		PageToken: reply.GetPageToken(),
	})
	wantCode(t, err, codes.OK, "ListClusters second page")
	for _, got := range next.GetClusterName() {
		if strings.HasPrefix(got, prefix) {
			t.Errorf("the second page repeated %q", got)
		}
	}
	// The end-of-listing rule as an implication, because this prefix is the
	// whole store and the test cannot know how many clusters follow its own:
	// a page shorter than count is the last one. ListDiskNodes pins the same
	// rule exactly, on a prefix that does belong to one test.
	if len(next.GetClusterName()) < common.MaxListCnt &&
		next.GetPageToken() != "" {
		t.Errorf(
			"a page shorter than count must end the listing, got token %q",
			next.GetPageToken())
	}

	_, err = s.ListClusters(
		ctx, &pb.ListClustersRequest{PageToken: "!!not base64!!"})
	wantCode(t, err, codes.InvalidArgument, "ListClusters with a bad token")
	hnodeWantMsg(t, err, "malformed page_token", "ListClusters with a bad token")
	_, err = s.ListClusters(
		ctx, &pb.ListClustersRequest{Count: common.MaxListCnt + 1})
	wantCode(t, err, codes.InvalidArgument, "ListClusters with count over max")
}

// ---------------------------------------------------------------------------
// §8.2 disk nodes
// ---------------------------------------------------------------------------

// TestCreateDiskNodeWritesFourKeys pins §8.2's whole write set, row by row: the
// DnConf, the DnRev the owning dn-worker watches, the §5.6 capacity key and the
// cluster's DnGlobal.
//
// The rows are chosen to pin four separate rules at once: total_ext_cnt is the
// reported size divided by the cluster's extent_size and ROUNDS DOWN (§6.1); an
// omitted location defaults to the node's own endpoint (§8.2 Defaults); the
// shard code is the index of the smallest bucket entry, first index on ties, so
// three creates land on shards 0, 1 and 2 (GW12); and a node created disabled
// gets no capacity key at all, in any bin — it is invisible to the allocator
// from its first instant (§5.6).
func TestCreateDiskNodeWritesFourKeys(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dn")
	cid := mustCluster(t, s, cluster)

	wantCapKeys := []string{}
	for _, tc := range []struct {
		label     string
		location  string
		size      uint64
		disabled  bool
		wantId    uint64
		wantShard uint32
		wantExt   uint64
		wantBin   uint32
	}{
		{
			// 16.5 GiB against the default 1 GiB extent: §6.1 floors it to 16,
			// which is exactly bin 1's level (1 << DefaultDnBin1Shift).
			label:     "default location",
			size:      16<<30 + 512<<20,
			wantId:    1,
			wantShard: 0,
			wantExt:   16,
			wantBin:   1,
		},
		{
			label:     "explicit location",
			location:  "rack-2",
			size:      300 << 30,
			wantId:    2,
			wantShard: 1,
			wantExt:   300,
			wantBin:   2,
		},
		{
			label:     "created disabled",
			location:  "rack-3",
			size:      8 << 30,
			disabled:  true,
			wantId:    3,
			wantShard: 2,
			wantExt:   8,
		},
	} {
		addr := fakeAddrPort(t, "dn")
		startFakeAgent(t, addr, tc.size)
		reply, err := s.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  testTrConf(addr),
			Location:    tc.location,
			Disabled:    tc.disabled,
		})
		wantCode(t, err, codes.OK, tc.label)
		if reply.GetDnId() != tc.wantId {
			t.Errorf(
				"%s: dn_id %d, want %d", tc.label, reply.GetDnId(), tc.wantId)
		}
		location := tc.location
		if location == "" {
			location = addr
		}
		// err_epoch 0, an empty side_ptr_list and free == total are the state
		// of a node that is healthy and hosts nothing yet (§8.2).
		dn := &pb.DnConf{}
		hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
		hnodeWantProto(t, dn, &pb.DnConf{
			DnId:        tc.wantId,
			ShardCode:   tc.wantShard,
			Disabled:    tc.disabled,
			NvmeTrConf:  testTrConf(addr),
			Location:    location,
			TotalExtCnt: tc.wantExt,
			FreeExtCnt:  tc.wantExt,
		}, tc.label+": dn_conf")
		rev := &pb.DnRev{}
		hnodeGet(t, s, model.DnRevKey(tc.wantShard, cid, tc.wantId), rev)
		hnodeWantProto(t, rev, &pb.DnRev{
			AddrPort: addr,
			Revision: 1,
		}, tc.label+": dn_rev")

		if !tc.disabled {
			capKey := model.DnCapacityKey(cid, tc.wantBin, tc.wantExt, addr)
			wantCapKeys = append(wantCapKeys, capKey)
			capacity := &pb.DnCapacity{}
			hnodeGet(t, s, capKey, capacity)
			hnodeWantProto(t, capacity, &pb.DnCapacity{
				Location: location,
			}, tc.label+": dn_capacity")
		}
	}

	// The capacity index is asserted as a whole key SET: the bin and the free
	// count are key fields the §6.3 walk scans by, so a right-valued key under
	// a wrong bin would be invisible to the allocator — and to a point read of
	// the key the test expected. The disabled node contributes nothing (§5.6).
	hnodeWantKeys(
		t, hnodeDnCapacityKeys(t, s, cid), wantCapKeys, "dn capacity index")

	// Three mints: next_id has advanced past every id it handed out and the
	// three shard codes each carry one node.
	global := &pb.DnGlobal{}
	hnodeGet(t, s, model.DnGlobalKey(cid), global)
	hnodeWantProto(t, global, &pb.DnGlobal{
		NextId:      4,
		ShardBucket: hnodeBucket(0, 1, 2),
	}, "dn_global after three creates")
}

// TestCreateDiskNodeRefusalsWriteNothing pins the two §8.2 refusals that can
// happen after the cluster has been resolved: a second create under the same
// addr_port is ALREADY_EXISTS, and a node whose reported size does not cover
// one extent is INVALID_ARGUMENT. Neither may leave a trace — in particular
// neither may consume a dn_id, because ids are never reused (§5.4) and a
// refused create that burned one would leave a permanent hole.
func TestCreateDiskNodeRefusalsWriteNothing(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dnrefuse")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	mustDn(t, s, cluster, addr, "rack-1", 16<<30)
	confRev := hnodeModRev(t, s, model.DnConfKey(cid, addr))
	globalRev := hnodeModRev(t, s, model.DnGlobalKey(cid))

	_, err := s.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		NvmeTrConf:  testTrConf(addr),
		Location:    "rack-9",
	})
	wantCode(t, err, codes.AlreadyExists, "CreateDiskNode twice")
	hnodeWantMsg(t, err, "already exists", "CreateDiskNode twice")
	if got := hnodeModRev(t, s, model.DnConfKey(cid, addr)); got != confRev {
		t.Errorf("dn_conf mod_revision moved: %d -> %d", confRev, got)
	}
	dn := &pb.DnConf{}
	hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
	if dn.GetLocation() != "rack-1" {
		t.Errorf("location: got %q, want the original %q",
			dn.GetLocation(), "rack-1")
	}

	// Half an extent: §6.1 floors total_ext_cnt to 0 and a node that can hold
	// nothing must not be registered at all.
	tiny := fakeAddrPort(t, "tiny")
	startFakeAgent(t, tiny, 512<<20)
	_, err = s.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    tiny,
		NvmeTrConf:  testTrConf(tiny),
	})
	wantCode(t, err, codes.InvalidArgument, "CreateDiskNode under one extent")
	if hnodeExists(t, s, model.DnConfKey(cid, tiny)) {
		t.Errorf("the refused CreateDiskNode wrote a dn_conf")
	}
	if got := hnodeModRev(t, s, model.DnGlobalKey(cid)); got != globalRev {
		t.Errorf("dn_global mod_revision moved: %d -> %d", globalRev, got)
	}
}

// TestGetDiskNodeReturnsConfAndToken pins §8.2's read: the reply is the stored
// DnConf plus the DnRev that is the client's token for the next mutator, both
// from one store revision, and the addr_port it was asked for.
func TestGetDiskNodeReturnsConfAndToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dnget")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	mustDn(t, s, cluster, addr, "rack-1", 64<<30)

	reply, err := s.GetDiskNode(ctx, &pb.GetDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
	})
	wantCode(t, err, codes.OK, "GetDiskNode")
	if reply.GetAddrPort() != addr {
		t.Errorf("addr_port: got %q, want %q", reply.GetAddrPort(), addr)
	}
	stored := &pb.DnConf{}
	hnodeGet(t, s, model.DnConfKey(cid, addr), stored)
	hnodeWantProto(t, reply.GetDnConf(), stored, "dn_conf")
	hnodeWantProto(t, reply.GetDnRev(), &pb.DnRev{
		AddrPort: addr,
		Revision: 1,
	}, "dn_rev")

	_, err = s.GetDiskNode(ctx, &pb.GetDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr + "-nope",
	})
	wantCode(t, err, codes.NotFound, "GetDiskNode of an unknown node")
}

// TestListDiskNodesPagesAndRejectsABadToken pins GW10 on a cluster-scoped list:
// the names are the addr_port suffixes under the cluster's prefix in key order,
// a full page carries the token that continues it, and a short page ends the
// listing. The whole prefix belongs to this test's cluster, so unlike
// ListClusters the pages are asserted exactly.
func TestListDiskNodesPagesAndRejectsABadToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dnlist")
	mustCluster(t, s, cluster)
	var addrs []string
	for i := 0; i < 3; i++ {
		addr := fakeAddrPort(t, "dn")
		mustDn(t, s, cluster, addr, "", 16<<30)
		addrs = append(addrs, addr)
	}
	// A page is key order, and an addr_port is a path whose lexical order is
	// not its creation order, so the expectation is sorted rather than assumed.
	sort.Strings(addrs)

	first, err := s.ListDiskNodes(ctx, &pb.ListDiskNodesRequest{
		ClusterName: cluster,
		Count:       2,
	})
	wantCode(t, err, codes.OK, "ListDiskNodes first page")
	if fmt.Sprint(first.GetAddrPort()) != fmt.Sprint(addrs[:2]) {
		t.Errorf(
			"first page: got %v, want %v", first.GetAddrPort(), addrs[:2])
	}
	if first.GetPageToken() == "" {
		t.Fatalf("a full page must carry a continuation token")
	}
	second, err := s.ListDiskNodes(ctx, &pb.ListDiskNodesRequest{
		ClusterName: cluster,
		Count:       2,
		PageToken:   first.GetPageToken(),
	})
	wantCode(t, err, codes.OK, "ListDiskNodes second page")
	if fmt.Sprint(second.GetAddrPort()) != fmt.Sprint(addrs[2:]) {
		t.Errorf(
			"second page: got %v, want %v", second.GetAddrPort(), addrs[2:])
	}
	if second.GetPageToken() != "" {
		t.Errorf(
			"a short page must end the listing, got token %q",
			second.GetPageToken())
	}

	_, err = s.ListDiskNodes(ctx, &pb.ListDiskNodesRequest{
		ClusterName: cluster,
		PageToken:   "!!not base64!!",
	})
	wantCode(t, err, codes.InvalidArgument, "ListDiskNodes with a bad token")
	_, err = s.ListDiskNodes(ctx, &pb.ListDiskNodesRequest{
		ClusterName: cluster + "-nope",
	})
	wantCode(t, err, codes.NotFound, "ListDiskNodes of an unknown cluster")
}

// TestUpdateDiskNodeDisabledMovesOnlyTheCapacityKey pins §8.2's one post-create
// writer. `disabled` decides whether the allocator can see the node and nothing
// else, so the RPC writes the DnConf and maintains the §5.6 capacity key, bumps
// NO revision — the agent is never told, and the sides the node already hosts
// keep running — and makes no agent call.
//
// The second half is §0 #17: a request that asks for the flag the node already
// carries writes NOTHING. The token is still checked first, so the no-op is not
// a way for a stale client to get an OK; what it must not do is move a single
// mod_revision, and both the rev key and the capacity key are asserted for that.
func TestUpdateDiskNodeDisabledMovesOnlyTheCapacityKey(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dndis")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	dnId := mustDn(t, s, cluster, addr, "rack-1", 16<<30)
	confKey := model.DnConfKey(cid, addr)
	revKey := model.DnRevKey(0, cid, dnId)
	capKey := model.DnCapacityKey(cid, 1, 16, addr)
	revBefore := hnodeModRev(t, s, revKey)

	reply, err := s.UpdateDiskNodeDisabled(
		ctx, &pb.UpdateDiskNodeDisabledRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, addr)},
			Disabled:    true,
		})
	wantCode(t, err, codes.OK, "UpdateDiskNodeDisabled true")
	if reply.GetDnId() != dnId {
		t.Errorf("dn_id: got %d, want %d", reply.GetDnId(), dnId)
	}
	dn := &pb.DnConf{}
	hnodeGet(t, s, confKey, dn)
	if !dn.GetDisabled() {
		t.Errorf("disabled was not stored")
	}
	hnodeWantKeys(
		t, hnodeDnCapacityKeys(t, s, cid), nil,
		"the capacity index of a cluster whose only DN is disabled")
	if got := hnodeModRev(t, s, revKey); got != revBefore {
		t.Errorf(
			"dn_rev was bumped: %d -> %d; §8.2 exempts this RPC",
			revBefore, got)
	}
	if got := dnTok(t, s, cluster, addr); got != 1 {
		t.Errorf("dn_rev revision: got %d, want 1", got)
	}

	// §0 #17: the same request again writes nothing at all.
	confRev := hnodeModRev(t, s, confKey)
	_, err = s.UpdateDiskNodeDisabled(ctx, &pb.UpdateDiskNodeDisabledRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, addr)},
		Disabled:    true,
	})
	wantCode(t, err, codes.OK, "UpdateDiskNodeDisabled true again")
	if got := hnodeModRev(t, s, confKey); got != confRev {
		t.Errorf("the idempotent call rewrote dn_conf: %d -> %d", confRev, got)
	}
	if got := hnodeModRev(t, s, revKey); got != revBefore {
		t.Errorf("the idempotent call moved dn_rev: %d -> %d", revBefore, got)
	}
	if hnodeExists(t, s, capKey) {
		t.Errorf("the idempotent call resurrected the capacity key")
	}

	// Re-enabling puts back exactly the key the create wrote: same bin, same
	// free count, same location.
	_, err = s.UpdateDiskNodeDisabled(ctx, &pb.UpdateDiskNodeDisabledRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, addr)},
		Disabled:    false,
	})
	wantCode(t, err, codes.OK, "UpdateDiskNodeDisabled false")
	capacity := &pb.DnCapacity{}
	hnodeGet(t, s, capKey, capacity)
	hnodeWantProto(
		t, capacity, &pb.DnCapacity{Location: "rack-1"}, "dn_capacity")

	// And the enabled side is idempotent too: the capacity key must not be
	// rewritten by a call that changes nothing.
	capRev := hnodeModRev(t, s, capKey)
	confRev = hnodeModRev(t, s, confKey)
	_, err = s.UpdateDiskNodeDisabled(ctx, &pb.UpdateDiskNodeDisabledRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, addr)},
		Disabled:    false,
	})
	wantCode(t, err, codes.OK, "UpdateDiskNodeDisabled false again")
	if got := hnodeModRev(t, s, capKey); got != capRev {
		t.Errorf("the idempotent call rewrote the capacity key: %d -> %d",
			capRev, got)
	}
	if got := hnodeModRev(t, s, confKey); got != confRev {
		t.Errorf("the idempotent call rewrote dn_conf: %d -> %d", confRev, got)
	}
	if got := hnodeModRev(t, s, revKey); got != revBefore {
		t.Errorf("the idempotent call moved dn_rev: %d -> %d", revBefore, got)
	}
}

// TestDeleteDiskNodeReleasesTheShard pins §8.2's delete: the DnRev key whose
// disappearance stops the owning dn-worker, the DnConf and the capacity key go
// in one commit; the global's bucket entry is released but next_id keeps
// growing, because a dn_id is never reused.
//
// It also pins the ORDER of §8.2's occupancy gate against GW6: a node that
// still hosts a side is refused, but a stale token is refused FIRST — an
// operator working from an out-of-date GetDiskNode must be told its view is
// stale, not told about sides it never saw.
func TestDeleteDiskNodeReleasesTheShard(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dndel")
	cid := mustCluster(t, s, cluster)
	cli := newTestClient(t)
	first := fakeAddrPort(t, "dn")
	second := fakeAddrPort(t, "dn")
	mustDn(t, s, cluster, first, "", 16<<30)
	mustDn(t, s, cluster, second, "", 16<<30)

	reply, err := s.DeleteDiskNode(ctx, &pb.DeleteDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    first,
		DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, first)},
	})
	wantCode(t, err, codes.OK, "DeleteDiskNode")
	if reply.GetDnId() != 1 {
		t.Errorf("dn_id: got %d, want 1", reply.GetDnId())
	}
	for _, key := range []string{
		model.DnConfKey(cid, first),
		model.DnRevKey(0, cid, 1),
		model.DnCapacityKey(cid, 1, 16, first),
	} {
		if hnodeExists(t, s, key) {
			t.Errorf("key %q survived DeleteDiskNode", key)
		}
	}
	global := &pb.DnGlobal{}
	hnodeGet(t, s, model.DnGlobalKey(cid), global)
	hnodeWantProto(t, global, &pb.DnGlobal{
		NextId:      3,
		ShardBucket: hnodeBucket(1),
	}, "dn_global after a delete")

	// The survivor now hosts a side. §8.2 refuses to delete it, and GW6 puts
	// the token check ahead of that gate.
	dn := &pb.DnConf{}
	hnodeGet(t, s, model.DnConfKey(cid, second), dn)
	dn.SidePtrList = []*pb.SidePointer{{SpId: 7, LegId: 8, SideId: 9}}
	mustPut(t, cli, model.DnConfKey(cid, second), dn)
	confRev := hnodeModRev(t, s, model.DnConfKey(cid, second))

	_, err = s.DeleteDiskNode(ctx, &pb.DeleteDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    second,
		DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, second) + 1},
	})
	wantCode(t, err, codes.Aborted, "DeleteDiskNode of a busy node, stale token")
	hnodeWantMsg(
		t, err, msgStaleRevision, "DeleteDiskNode of a busy node, stale token")

	_, err = s.DeleteDiskNode(ctx, &pb.DeleteDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    second,
		DnRev:       &pb.DnRev{Revision: dnTok(t, s, cluster, second)},
	})
	wantCode(t, err, codes.FailedPrecondition, "DeleteDiskNode of a busy node")
	hnodeWantMsg(t, err, "still hosts 1 sides", "DeleteDiskNode of a busy node")
	if got := hnodeModRev(t, s, model.DnConfKey(cid, second)); got != confRev {
		t.Errorf("the refused delete rewrote dn_conf: %d -> %d", confRev, got)
	}
	if !hnodeExists(t, s, model.DnRevKey(1, cid, 2)) {
		t.Errorf("the refused delete removed dn_rev")
	}
}

// TestInspectDiskNodeRepliesTheAppliedRevision pins update_04.md U2: the
// reply's `applied_revision` is the one the agent's GetDnInfo reply carries —
// its last applied revision — NOT the DnRev stored in etcd, which is what
// architecture.md §8.2 and gateway.md §5.2 specify. The fake answers with a
// value no bump sequence reaches, so a handler that regressed to the stored
// revision would be unmistakable.
func TestInspectDiskNodeRepliesTheAppliedRevision(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("dnins")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	agent := startFakeAgent(t, addr, 16<<30)
	if _, err := s.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		NvmeTrConf:  testTrConf(addr),
	}); err != nil {
		t.Fatalf("CreateDiskNode: %v", err)
	}
	info := &pb.DnInfo{
		DiskInfo: &pb.ResInfo{
			ResName: "disk",
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "1 extent",
			Epoch:   3,
		},
	}
	agent.setInfo(info, nil, nil, nil)

	reply, err := s.InspectDiskNode(ctx, &pb.InspectDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
	})
	wantCode(t, err, codes.OK, "InspectDiskNode")
	if reply.GetAppliedRevision() != fakeAgentRevision {
		t.Errorf(
			"applied_revision: got %#x, want the agent's %#x (not the stored DnRev)",
			reply.GetAppliedRevision(), fakeAgentRevision)
	}
	hnodeWantProto(t, reply.GetDnInfo(), info, "dn_info")
	if got := agent.callCount("GetDnInfo"); got != 1 {
		t.Errorf("GetDnInfo call count: got %d, want 1", got)
	}
	// The create that set this node up probed the agent for its size exactly
	// once, before its transaction (AG1, §5.8).
	if got := agent.callCount("GetDnSize"); got != 1 {
		t.Errorf("GetDnSize call count: got %d, want 1", got)
	}

	// A bumped rev key must not move the reply: the store is not the source
	// (U2), the agent's reply is.
	mustPut(t, newTestClient(t), model.DnRevKey(0, cid, 1), &pb.DnRev{
		AddrPort: addr,
		Revision: 5,
	})
	reply, err = s.InspectDiskNode(ctx, &pb.InspectDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
	})
	wantCode(t, err, codes.OK, "InspectDiskNode after a bump")
	if reply.GetAppliedRevision() != fakeAgentRevision {
		t.Errorf("applied_revision after a bump: got %#x, want %#x",
			reply.GetAppliedRevision(), fakeAgentRevision)
	}
}

// ---------------------------------------------------------------------------
// §8.3 controller nodes
// ---------------------------------------------------------------------------

// TestCreateControllerNodeAppliesTheBudgetRule pins the one place §8.3 is not a
// character-for-character mirror of §8.2: a DN reports a disk it has measured
// and the gateway believes it, while a CN reports how much working space the
// operator lets dnv use — an opinion, so §6.1 reads it with a floor and a
// ceiling before dividing by extent_size.
//
// The rows are the three arms of that rule plus its collision with §6.1's
// floor: MinCnCap itself is a legal budget that yields less than one extent,
// and a node that can hold nothing must not be registered.
func TestCreateControllerNodeAppliesTheBudgetRule(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cn")
	cid := mustCluster(t, s, cluster)

	wantCapKeys := []string{}
	for _, tc := range []struct {
		label     string
		size      uint64
		wantId    uint64
		wantShard uint32
		wantExt   uint64
		wantCode  codes.Code
	}{
		{
			label:     "no opinion takes DefaultCnCap",
			size:      0,
			wantId:    1,
			wantShard: 0,
			wantExt:   common.DefaultCnCap / common.DefaultDnExtSize,
			wantCode:  codes.OK,
		},
		{
			label:     "a budget inside the window is used as reported",
			size:      8 << 30,
			wantId:    2,
			wantShard: 1,
			wantExt:   8,
			wantCode:  codes.OK,
		},
		{
			label:     "a budget over MaxCnCap is clamped to it",
			size:      common.MaxCnCap + 1,
			wantId:    3,
			wantShard: 2,
			wantExt:   common.MaxCnCap / common.DefaultDnExtSize,
			wantCode:  codes.OK,
		},
		{
			label:    "a budget under one extent is refused",
			size:     common.MinCnCap,
			wantCode: codes.InvalidArgument,
		},
	} {
		addr := fakeAddrPort(t, "cn")
		startFakeAgent(t, addr, tc.size)
		reply, err := s.CreateControllerNode(
			ctx, &pb.CreateControllerNodeRequest{
				ClusterName: cluster,
				AddrPort:    addr,
				NvmeTrConf:  testTrConf(addr),
				Location:    "rack-1",
			})
		wantCode(t, err, tc.wantCode, tc.label)
		if tc.wantCode != codes.OK {
			if hnodeExists(t, s, model.CnConfKey(cid, addr)) {
				t.Errorf("%s: a cn_conf was written anyway", tc.label)
			}
			continue
		}
		if reply.GetCnId() != tc.wantId {
			t.Errorf(
				"%s: cn_id %d, want %d", tc.label, reply.GetCnId(), tc.wantId)
		}
		cn := &pb.CnConf{}
		hnodeGet(t, s, model.CnConfKey(cid, addr), cn)
		hnodeWantProto(t, cn, &pb.CnConf{
			CnId:        tc.wantId,
			ShardCode:   tc.wantShard,
			NvmeTrConf:  testTrConf(addr),
			Location:    "rack-1",
			TotalExtCnt: tc.wantExt,
			FreeExtCnt:  tc.wantExt,
		}, tc.label+": cn_conf")
		rev := &pb.CnRev{}
		hnodeGet(t, s, model.CnRevKey(tc.wantShard, cid, tc.wantId), rev)
		hnodeWantProto(t, rev, &pb.CnRev{
			AddrPort: addr,
			Revision: 1,
		}, tc.label+": cn_rev")
		// A CN capacity key carries no bin index (§6.4): the whole key is the
		// cluster, the free count and the address.
		capKey := model.CnCapacityKey(cid, tc.wantExt, addr)
		wantCapKeys = append(wantCapKeys, capKey)
		capacity := &pb.CnCapacity{}
		hnodeGet(t, s, capKey, capacity)
		hnodeWantProto(t, capacity, &pb.CnCapacity{
			Location: "rack-1",
		}, tc.label+": cn_capacity")
	}

	hnodeWantKeys(
		t, hnodeCnCapacityKeys(t, s, cid), wantCapKeys, "cn capacity index")

	// The refused row minted nothing: next_id counts the three that committed.
	global := &pb.CnGlobal{}
	hnodeGet(t, s, model.CnGlobalKey(cid), global)
	hnodeWantProto(t, global, &pb.CnGlobal{
		NextId:      4,
		ShardBucket: hnodeBucket(0, 1, 2),
	}, "cn_global after three creates and one refusal")
}

// TestCreateControllerNodeCollisionWritesNothing is §8.2's ALREADY_EXISTS
// mirrored onto §8.3: a second create under the same addr_port is refused and
// consumes no cn_id.
func TestCreateControllerNodeCollisionWritesNothing(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cndup")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "cn")
	mustCn(t, s, cluster, addr, "rack-1", 1<<40)
	confRev := hnodeModRev(t, s, model.CnConfKey(cid, addr))
	globalRev := hnodeModRev(t, s, model.CnGlobalKey(cid))

	_, err := s.CreateControllerNode(ctx, &pb.CreateControllerNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		NvmeTrConf:  testTrConf(addr),
		Location:    "rack-9",
	})
	wantCode(t, err, codes.AlreadyExists, "CreateControllerNode twice")
	if got := hnodeModRev(t, s, model.CnConfKey(cid, addr)); got != confRev {
		t.Errorf("cn_conf mod_revision moved: %d -> %d", confRev, got)
	}
	if got := hnodeModRev(t, s, model.CnGlobalKey(cid)); got != globalRev {
		t.Errorf("cn_global mod_revision moved: %d -> %d", globalRev, got)
	}
}

// TestGetControllerNodeReturnsConfAndToken is §8.2's read mirrored: the stored
// CnConf plus the CnRev that is the client's token, from one store revision.
func TestGetControllerNodeReturnsConfAndToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cnget")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "cn")
	mustCn(t, s, cluster, addr, "rack-1", 1<<40)

	reply, err := s.GetControllerNode(ctx, &pb.GetControllerNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
	})
	wantCode(t, err, codes.OK, "GetControllerNode")
	if reply.GetAddrPort() != addr {
		t.Errorf("addr_port: got %q, want %q", reply.GetAddrPort(), addr)
	}
	stored := &pb.CnConf{}
	hnodeGet(t, s, model.CnConfKey(cid, addr), stored)
	hnodeWantProto(t, reply.GetCnConf(), stored, "cn_conf")
	hnodeWantProto(t, reply.GetCnRev(), &pb.CnRev{
		AddrPort: addr,
		Revision: 1,
	}, "cn_rev")

	_, err = s.GetControllerNode(ctx, &pb.GetControllerNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr + "-nope",
	})
	wantCode(t, err, codes.NotFound, "GetControllerNode of an unknown node")
}

// TestListControllerNodesPagesAndRejectsABadToken is ListDiskNodes' assertion
// on the CN prefix: same GW10 rules, a different key kind.
func TestListControllerNodesPagesAndRejectsABadToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cnlist")
	mustCluster(t, s, cluster)
	var addrs []string
	for i := 0; i < 3; i++ {
		addr := fakeAddrPort(t, "cn")
		mustCn(t, s, cluster, addr, "", 1<<40)
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)

	first, err := s.ListControllerNodes(
		ctx, &pb.ListControllerNodesRequest{
			ClusterName: cluster,
			Count:       2,
		})
	wantCode(t, err, codes.OK, "ListControllerNodes first page")
	if fmt.Sprint(first.GetAddrPort()) != fmt.Sprint(addrs[:2]) {
		t.Errorf(
			"first page: got %v, want %v", first.GetAddrPort(), addrs[:2])
	}
	second, err := s.ListControllerNodes(
		ctx, &pb.ListControllerNodesRequest{
			ClusterName: cluster,
			Count:       2,
			PageToken:   first.GetPageToken(),
		})
	wantCode(t, err, codes.OK, "ListControllerNodes second page")
	if fmt.Sprint(second.GetAddrPort()) != fmt.Sprint(addrs[2:]) {
		t.Errorf(
			"second page: got %v, want %v", second.GetAddrPort(), addrs[2:])
	}
	if second.GetPageToken() != "" {
		t.Errorf(
			"a short page must end the listing, got token %q",
			second.GetPageToken())
	}

	_, err = s.ListControllerNodes(ctx, &pb.ListControllerNodesRequest{
		ClusterName: cluster,
		PageToken:   "!!not base64!!",
	})
	wantCode(
		t, err, codes.InvalidArgument, "ListControllerNodes with a bad token")
}

// TestUpdateControllerNodeDisabledMovesOnlyTheCapacityKey is §8.2's
// UpdateDiskNodeDisabled mirrored onto §8.3, including §0 #17's idempotent
// no-write: neither the rev key nor the capacity key may be touched by a call
// that asks for the flag the node already carries.
func TestUpdateControllerNodeDisabledMovesOnlyTheCapacityKey(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cndis")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "cn")
	cnId := mustCn(t, s, cluster, addr, "rack-1", 8<<30)
	confKey := model.CnConfKey(cid, addr)
	revKey := model.CnRevKey(0, cid, cnId)
	capKey := model.CnCapacityKey(cid, 8, addr)
	revBefore := hnodeModRev(t, s, revKey)

	reply, err := s.UpdateControllerNodeDisabled(
		ctx, &pb.UpdateControllerNodeDisabledRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, addr)},
			Disabled:    true,
		})
	wantCode(t, err, codes.OK, "UpdateControllerNodeDisabled true")
	if reply.GetCnId() != cnId {
		t.Errorf("cn_id: got %d, want %d", reply.GetCnId(), cnId)
	}
	cn := &pb.CnConf{}
	hnodeGet(t, s, confKey, cn)
	if !cn.GetDisabled() {
		t.Errorf("disabled was not stored")
	}
	hnodeWantKeys(
		t, hnodeCnCapacityKeys(t, s, cid), nil,
		"the capacity index of a cluster whose only CN is disabled")
	if got := hnodeModRev(t, s, revKey); got != revBefore {
		t.Errorf(
			"cn_rev was bumped: %d -> %d; §8.3 exempts this RPC",
			revBefore, got)
	}

	confRev := hnodeModRev(t, s, confKey)
	_, err = s.UpdateControllerNodeDisabled(
		ctx, &pb.UpdateControllerNodeDisabledRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, addr)},
			Disabled:    true,
		})
	wantCode(t, err, codes.OK, "UpdateControllerNodeDisabled true again")
	if got := hnodeModRev(t, s, confKey); got != confRev {
		t.Errorf("the idempotent call rewrote cn_conf: %d -> %d", confRev, got)
	}
	if got := hnodeModRev(t, s, revKey); got != revBefore {
		t.Errorf("the idempotent call moved cn_rev: %d -> %d", revBefore, got)
	}
	if hnodeExists(t, s, capKey) {
		t.Errorf("the idempotent call resurrected the capacity key")
	}

	_, err = s.UpdateControllerNodeDisabled(
		ctx, &pb.UpdateControllerNodeDisabledRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, addr)},
			Disabled:    false,
		})
	wantCode(t, err, codes.OK, "UpdateControllerNodeDisabled false")
	capacity := &pb.CnCapacity{}
	hnodeGet(t, s, capKey, capacity)
	hnodeWantProto(
		t, capacity, &pb.CnCapacity{Location: "rack-1"}, "cn_capacity")
	capRev := hnodeModRev(t, s, capKey)
	_, err = s.UpdateControllerNodeDisabled(
		ctx, &pb.UpdateControllerNodeDisabledRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, addr)},
			Disabled:    false,
		})
	wantCode(t, err, codes.OK, "UpdateControllerNodeDisabled false again")
	if got := hnodeModRev(t, s, capKey); got != capRev {
		t.Errorf("the idempotent call rewrote the capacity key: %d -> %d",
			capRev, got)
	}
}

// TestDeleteControllerNodeReleasesTheShard is §8.2's delete mirrored onto §8.3,
// where the occupancy gate is `cntlr_ptr_list` instead of `side_ptr_list`, and
// the same GW6 ordering applies: a stale token beats the gate.
func TestDeleteControllerNodeReleasesTheShard(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cndel")
	cid := mustCluster(t, s, cluster)
	cli := newTestClient(t)
	first := fakeAddrPort(t, "cn")
	second := fakeAddrPort(t, "cn")
	mustCn(t, s, cluster, first, "", 8<<30)
	mustCn(t, s, cluster, second, "", 8<<30)

	reply, err := s.DeleteControllerNode(
		ctx, &pb.DeleteControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    first,
			CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, first)},
		})
	wantCode(t, err, codes.OK, "DeleteControllerNode")
	if reply.GetCnId() != 1 {
		t.Errorf("cn_id: got %d, want 1", reply.GetCnId())
	}
	for _, key := range []string{
		model.CnConfKey(cid, first),
		model.CnRevKey(0, cid, 1),
		model.CnCapacityKey(cid, 8, first),
	} {
		if hnodeExists(t, s, key) {
			t.Errorf("key %q survived DeleteControllerNode", key)
		}
	}
	global := &pb.CnGlobal{}
	hnodeGet(t, s, model.CnGlobalKey(cid), global)
	hnodeWantProto(t, global, &pb.CnGlobal{
		NextId:      3,
		ShardBucket: hnodeBucket(1),
	}, "cn_global after a delete")

	cn := &pb.CnConf{}
	hnodeGet(t, s, model.CnConfKey(cid, second), cn)
	cn.CntlrPtrList = []*pb.CntlrPointer{{SpId: 7, CntlrId: 8}}
	mustPut(t, cli, model.CnConfKey(cid, second), cn)
	confRev := hnodeModRev(t, s, model.CnConfKey(cid, second))

	_, err = s.DeleteControllerNode(ctx, &pb.DeleteControllerNodeRequest{
		ClusterName: cluster,
		AddrPort:    second,
		CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, second) + 1},
	})
	wantCode(
		t, err, codes.Aborted,
		"DeleteControllerNode of a busy node, stale token")

	_, err = s.DeleteControllerNode(ctx, &pb.DeleteControllerNodeRequest{
		ClusterName: cluster,
		AddrPort:    second,
		CnRev:       &pb.CnRev{Revision: cnTok(t, s, cluster, second)},
	})
	wantCode(
		t, err, codes.FailedPrecondition, "DeleteControllerNode of a busy node")
	hnodeWantMsg(
		t, err, "still hosts 1 cntlrs", "DeleteControllerNode of a busy node")
	if got := hnodeModRev(t, s, model.CnConfKey(cid, second)); got != confRev {
		t.Errorf("the refused delete rewrote cn_conf: %d -> %d", confRev, got)
	}
}

// TestInspectControllerNodeRepliesTheAppliedRevision is update_04.md U3 on
// the CN side: the reply's `applied_revision` is the one the agent's
// GetCnInfo reply carries, never the CnRev the store holds.
func TestInspectControllerNodeRepliesTheAppliedRevision(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("cnins")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "cn")
	agent := startFakeAgent(t, addr, 8<<30)
	if _, err := s.CreateControllerNode(
		ctx, &pb.CreateControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  testTrConf(addr),
		}); err != nil {
		t.Fatalf("CreateControllerNode: %v", err)
	}
	info := &pb.CnInfo{
		PortInfo: &pb.ResInfo{
			ResName: "port",
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "4420",
		},
		LoopDevInfo: &pb.ResInfo{ResName: "/dev/loop0"},
	}
	agent.setInfo(nil, nil, info, nil)

	reply, err := s.InspectControllerNode(
		ctx, &pb.InspectControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
		})
	wantCode(t, err, codes.OK, "InspectControllerNode")
	if reply.GetAppliedRevision() != fakeAgentRevision {
		t.Errorf(
			"applied_revision: got %#x, want the agent's %#x (not the stored CnRev)",
			reply.GetAppliedRevision(), fakeAgentRevision)
	}
	hnodeWantProto(t, reply.GetCnInfo(), info, "cn_info")
	if got := agent.callCount("GetCnInfo"); got != 1 {
		t.Errorf("GetCnInfo call count: got %d, want 1", got)
	}
	if got := agent.callCount("GetCnSize"); got != 1 {
		t.Errorf("GetCnSize call count: got %d, want 1", got)
	}

	// A bumped rev key must not move the reply: the store is not the source
	// (U3), the agent's reply is.
	mustPut(t, newTestClient(t), model.CnRevKey(0, cid, 1), &pb.CnRev{
		AddrPort: addr,
		Revision: 5,
	})
	reply, err = s.InspectControllerNode(
		ctx, &pb.InspectControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
		})
	wantCode(t, err, codes.OK, "InspectControllerNode after a bump")
	if reply.GetAppliedRevision() != fakeAgentRevision {
		t.Errorf("applied_revision after a bump: got %#x, want %#x",
			reply.GetAppliedRevision(), fakeAgentRevision)
	}
}

// ---------------------------------------------------------------------------
// GW6 across both node kinds
// ---------------------------------------------------------------------------

// TestNodeMutatorsRefuseAStaleOrMissingToken pins GW6 and §0 #7 on all four
// token-taking node RPCs at once: the token is read as
// req.GetXRev().GetRevision(), so an omitted message reads 0 — and since a
// stored revision starts at 1 and only grows, 0 can never match. Both rows are
// therefore the same refusal, which is exactly the property that makes the
// nil-token case safe: there is no way to reach a mutation without a token.
func TestNodeMutatorsRefuseAStaleOrMissingToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("tok")
	cid := mustCluster(t, s, cluster)
	dnAddr := fakeAddrPort(t, "dn")
	cnAddr := fakeAddrPort(t, "cn")
	mustDn(t, s, cluster, dnAddr, "rack-1", 16<<30)
	mustCn(t, s, cluster, cnAddr, "rack-1", 8<<30)
	dnConfRev := hnodeModRev(t, s, model.DnConfKey(cid, dnAddr))
	cnConfRev := hnodeModRev(t, s, model.CnConfKey(cid, cnAddr))

	for _, tc := range []struct {
		label string
		call  func(dnRev *pb.DnRev, cnRev *pb.CnRev) error
	}{
		{
			label: "DeleteDiskNode",
			call: func(dnRev *pb.DnRev, _ *pb.CnRev) error {
				_, err := s.DeleteDiskNode(ctx, &pb.DeleteDiskNodeRequest{
					ClusterName: cluster,
					AddrPort:    dnAddr,
					DnRev:       dnRev,
				})
				return err
			},
		},
		{
			label: "UpdateDiskNodeDisabled",
			call: func(dnRev *pb.DnRev, _ *pb.CnRev) error {
				_, err := s.UpdateDiskNodeDisabled(
					ctx, &pb.UpdateDiskNodeDisabledRequest{
						ClusterName: cluster,
						AddrPort:    dnAddr,
						DnRev:       dnRev,
						Disabled:    true,
					})
				return err
			},
		},
		{
			label: "DeleteControllerNode",
			call: func(_ *pb.DnRev, cnRev *pb.CnRev) error {
				_, err := s.DeleteControllerNode(
					ctx, &pb.DeleteControllerNodeRequest{
						ClusterName: cluster,
						AddrPort:    cnAddr,
						CnRev:       cnRev,
					})
				return err
			},
		},
		{
			label: "UpdateControllerNodeDisabled",
			call: func(_ *pb.DnRev, cnRev *pb.CnRev) error {
				_, err := s.UpdateControllerNodeDisabled(
					ctx, &pb.UpdateControllerNodeDisabledRequest{
						ClusterName: cluster,
						AddrPort:    cnAddr,
						CnRev:       cnRev,
						Disabled:    true,
					})
				return err
			},
		},
	} {
		for _, token := range []struct {
			label string
			dnRev *pb.DnRev
			cnRev *pb.CnRev
		}{
			{label: "no token at all"},
			{
				label: "a token from the future",
				dnRev: &pb.DnRev{Revision: 2},
				cnRev: &pb.CnRev{Revision: 2},
			},
			{
				// The addr_port a client echoes back inside the token message
				// is ignored: only `revision` participates, so a message that
				// carries everything BUT the revision reads 0 like a nil one.
				label: "a token message with no revision",
				dnRev: &pb.DnRev{AddrPort: dnAddr},
				cnRev: &pb.CnRev{AddrPort: cnAddr},
			},
		} {
			label := tc.label + " with " + token.label
			err := tc.call(token.dnRev, token.cnRev)
			wantCode(t, err, codes.Aborted, label)
			hnodeWantMsg(t, err, msgStaleRevision, label)
		}
	}

	// Nothing above wrote: both nodes are still there, still enabled, still on
	// revision 1.
	if got := hnodeModRev(t, s, model.DnConfKey(cid, dnAddr)); got != dnConfRev {
		t.Errorf("dn_conf was rewritten by a refused mutator")
	}
	if got := hnodeModRev(t, s, model.CnConfKey(cid, cnAddr)); got != cnConfRev {
		t.Errorf("cn_conf was rewritten by a refused mutator")
	}
	if got := dnTok(t, s, cluster, dnAddr); got != 1 {
		t.Errorf("dn_rev: got %d, want 1", got)
	}
	if got := cnTok(t, s, cluster, cnAddr); got != 1 {
		t.Errorf("cn_rev: got %d, want 1", got)
	}
}
