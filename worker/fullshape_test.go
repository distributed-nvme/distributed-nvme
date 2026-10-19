package worker

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker full-shape cases (dnv-worker.md, Integration test plan):
// one dimension at its maximum at a time (architecture.md, Cardinality
// limits), driven through one whole worker on the harness of
// harness_test.go. They assert what the worker leaves in etcd and what each
// fake received, never how long it took, and they are skipped under -short.

// The full-shape timers. A check round that misses sets a health epoch at
// once (HL1), so a case whose fakes share the process with the race detector
// gives each round a timeout of several seconds, the round's own interval;
// the vote timers are lengthened past the harness's so that a stalled
// machine does not make the worker fence itself (VW8), and the syncup
// timeout so that a cntlr syncup that carries a whole wide pool is not cut
// off. None of them is a bound a case asserts.
const (
	fullShapeVoteInterval = time.Second
	fullShapeGraceTime    = 3 * time.Second
	// fullShapeNodeInterval is the dn and cn check interval of the most
	// nodes case, in seconds.
	fullShapeNodeInterval = 5
	// fullShapePoll is how often a full-shape wait asks its condition: each
	// ask reads thousands of fakes' records.
	fullShapePoll = 250 * time.Millisecond
)

// fullShapeOpts are the harness options of a full-shape case.
func fullShapeOpts() harnessOpts {
	return harnessOpts{
		voteInterval: fullShapeVoteInterval,
		graceTime:    fullShapeGraceTime,
		tmo: &timeouts{
			syncup:       10 * time.Second,
			push:         10 * time.Second,
			demotionHold: time.Second,
			etcdOp:       2 * time.Second,
		},
	}
}

// skipFullShape skips a full-shape case under -short (plan decision 13).
func skipFullShape(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("a full-shape case: skipped under -short")
	}
}

// fullShapeWait is waitUntil at fullShapePoll: it asks cond until it holds and
// fails the case once timeout has passed, with cond's last word.
func fullShapeWait(
	t *testing.T,
	timeout time.Duration,
	what string,
	cond func() (bool, string),
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(fullShapePoll)
	defer ticker.Stop()
	for {
		ok, why := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s: %s", timeout, what, why)
		}
		<-ticker.C
	}
}

// fullShapeFailedRoundMsgs are the records of a check round that failed (RW4 step 4,
// HL7): a stream that could not be opened, a send that failed, and a reply
// that did not come in time, ended the stream or answered from an
// observation too old.
var fullShapeFailedRoundMsgs = []string{
	"check stream open failed",
	"check stream send failed",
	"check round failed",
}

// assertNoFailedRoundOf fails the case for every failed-round record of w and
// for every syncup result that is not an accepted code-zero reply.
func assertNoFailedRoundOf(t *testing.T, h *workerHarness, w *harnessWorker) {
	t.Helper()
	for _, msg := range fullShapeFailedRoundMsgs {
		if recs := h.records(w, msg); len(recs) != 0 {
			t.Errorf("%d %q records, want none; the first: %v",
				len(recs), msg, recs[0].attrs)
		}
	}
	for _, rec := range h.records(w, msgSyncupResult) {
		if rec.num("code") != 0 || rec.str("error") != "" {
			t.Errorf("a syncup result with code %d, error %q: %v",
				rec.num("code"), rec.str("error"), rec.attrs)
			break
		}
	}
}

// assertNoEpochSetBy fails the case for every health change record of w that
// set an epoch (HL1, HL2): with no epoch ever set, each such record is an
// object's first verdict, clean, which HL3 writes whatever the record holds
// and which names the reason initial.
func assertNoEpochSetBy(t *testing.T, h *workerHarness, w *harnessWorker) {
	t.Helper()
	set := 0
	for _, rec := range h.records(w, msgHealthChanged) {
		if rec.num("err_epoch") != 0 {
			if set == 0 {
				t.Errorf("a health epoch was set: %v", rec.attrs)
			}
			set++
		}
	}
	if set > 1 {
		t.Errorf("%d health epochs were set in all", set)
	}
}

// ---------------------------------------------------------------------------
// The most nodes
// ---------------------------------------------------------------------------

func mostDnAddr(i int) string { return fmt.Sprintf("10.2.%d.%d:29600", i/256, i%256) }
func mostCnAddr(i int) string { return fmt.Sprintf("10.3.%d.%d:29700", i/256, i%256) }

// mostNodeWatch is one node's fake and what the case has seen of it: the
// counts of its syncups and rounds when its syncup at its planted revision
// first read right, and whether two more rounds have come since. Once a node
// is done, check reads it no more; assertMostNoSecondSyncup reads every node
// once more at the end.
type mostNodeWatch struct {
	name     string
	agent    *agentfake.Agent
	key      string
	revision uint64
	seen     bool
	syncups  int
	rounds   int
	done     bool
}

// check reads the node's fake once (TestFullShapeMostNodes). It returns
// whether the node is done and, when not, why.
func (n *mostNodeWatch) check(t *testing.T) (bool, string) {
	t.Helper()
	if n.done {
		return true, ""
	}
	rec := n.agent.Record(n.key)
	if !n.seen {
		if len(rec.Syncups) == 0 {
			return false, n.name + ": no syncup yet"
		}
		var revision uint64
		var listed int
		switch req := rec.Syncups[len(rec.Syncups)-1].(type) {
		case *pb.SyncupDnRequest:
			revision, listed = req.GetRevision(), len(req.GetSidePointerList())
		case *pb.SyncupCnRequest:
			revision, listed = req.GetRevision(), len(req.GetCntlrPointerList())
		}
		if revision != n.revision || listed != 0 {
			return false, fmt.Sprintf("%s: last syncup at revision %d listing %d, "+
				"want revision %d listing nothing", n.name, revision, listed, n.revision)
		}
		// Every fake starts empty, so the syncup that reads right is the
		// node's first and only one.
		if len(rec.Syncups) != 1 {
			t.Fatalf("%s got %d syncups before one read right: %v",
				n.name, len(rec.Syncups), rec.Syncups)
		}
		n.seen, n.syncups, n.rounds = true, len(rec.Syncups), len(rec.Rounds)
	}
	if len(rec.Syncups) != n.syncups {
		t.Fatalf("%s got syncup %d after its syncup at revision %d: %v",
			n.name, len(rec.Syncups), n.revision, rec.Syncups[len(rec.Syncups)-1])
	}
	if len(rec.Rounds) < n.rounds+2 {
		return false, fmt.Sprintf("%s: %d rounds since its syncup, of 2",
			n.name, len(rec.Rounds)-n.rounds)
	}
	n.done = true
	return true, ""
}

// TestFullShapeMostNodes pins the per-cluster node maximums under one whole
// worker: a cluster of MaxDnCntPerCluster disk nodes and MaxCnCntPerCluster
// controller nodes, each a fake agent, spread over every shard. Every node's
// fake gets its syncup at the revision its key holds, with the empty list its
// record holds, and then at least two more rounds and no second syncup, so
// every round answers the revision the worker wants (RW4 step 5); no node
// ever gets a health epoch (HL1); and no round of the worker fails and every
// syncup is accepted.
func TestFullShapeMostNodes(t *testing.T) {
	t.Parallel()
	skipFullShape(t)
	ctx := context.Background()
	h := newWorkerHarness(t, fullShapeOpts())
	dnCnt, cnCnt := common.MaxDnCntPerCluster, common.MaxCnCntPerCluster

	start := time.Now()
	for i := 0; i < dnCnt; i++ {
		if _, err := h.fleet.Add(mostDnAddr(i), agentfake.Dn, agentfake.Config{}); err != nil {
			t.Fatalf("fake dn %d: %v", i, err)
		}
	}
	for i := 0; i < cnCnt; i++ {
		if _, err := h.fleet.Add(mostCnAddr(i), agentfake.Cn, agentfake.Config{}); err != nil {
			t.Fatalf("fake cn %d: %v", i, err)
		}
	}
	cl := h.putCluster(t, plant.ClusterSpec{
		ExtentSize: common.MinDnExtSize,
		DnInterval: fullShapeNodeInterval,
		CnInterval: fullShapeNodeInterval,
	})
	dnSpecs := make([]plant.DnSpec, 0, dnCnt)
	for i := 0; i < dnCnt; i++ {
		dnSpecs = append(dnSpecs, plant.DnSpec{
			Shard:    uint32(i % common.ShardBucketSize),
			Addr:     mostDnAddr(i),
			Location: fmt.Sprintf("dn-rack%d", i),
			FreeExt:  64,
		})
	}
	dns, err := plant.PutDns(ctx, h.cli, cl, dnSpecs)
	if err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	cnSpecs := make([]plant.CnSpec, 0, cnCnt)
	for i := 0; i < cnCnt; i++ {
		cnSpecs = append(cnSpecs, plant.CnSpec{
			Shard:    uint32(i % common.ShardBucketSize),
			Addr:     mostCnAddr(i),
			Location: fmt.Sprintf("cn-rack%d", i),
			FreeExt:  64,
		})
	}
	cns, err := plant.PutCns(ctx, h.cli, cl, cnSpecs)
	if err != nil {
		t.Fatalf("PutCns: %v", err)
	}
	planted := time.Now()
	t.Logf("%d fakes added and %d dns, %d cns planted in %v", dnCnt+cnCnt,
		len(dns), len(cns), planted.Sub(start).Round(time.Millisecond))

	nodes := make([]*mostNodeWatch, 0, len(dns)+len(cns))
	dnIds := make(map[uint64]bool, len(dns))
	for _, dn := range dns {
		dnIds[dn.Conf.GetDnId()] = true
		nodes = append(nodes, &mostNodeWatch{
			name:     fmt.Sprintf("dn %d", dn.Conf.GetDnId()),
			agent:    h.fleet.Agent(dn.Addr),
			key:      agentfake.DnKey,
			revision: dn.Revision,
		})
	}
	cnIds := make(map[uint64]bool, len(cns))
	for _, cn := range cns {
		cnIds[cn.Conf.GetCnId()] = true
		nodes = append(nodes, &mostNodeWatch{
			name:     fmt.Sprintf("cn %d", cn.Conf.GetCnId()),
			agent:    h.fleet.Agent(cn.Addr),
			key:      agentfake.CnKey,
			revision: cn.Revision,
		})
	}
	// The plant took one id per node, so every node is a node of its own.
	if len(dnIds) != dnCnt || len(cnIds) != cnCnt {
		t.Fatalf("the plant took %d dn ids and %d cn ids, want %d and %d",
			len(dnIds), len(cnIds), dnCnt, cnCnt)
	}

	w := h.startWorker(t, "w1", common.WorkerRoleDn, common.WorkerRoleCn)
	h.waitOwnsAll(t, w)
	// Two intervals after the last node's syncup, which the first round of
	// each node's stream brings, plus the start of 2048 streams.
	fullShapeWait(t, 90*time.Second, "every node's syncup and two rounds after it",
		func() (bool, string) {
			pending, why := 0, ""
			for _, n := range nodes {
				if ok, nwhy := n.check(t); !ok {
					if pending == 0 {
						why = nwhy
					}
					pending++
				}
			}
			if pending != 0 {
				return false, fmt.Sprintf("%d nodes pending; %s", pending, why)
			}
			return true, ""
		})
	t.Logf("every node synced and answered two rounds %v after the worker started",
		time.Since(planted).Round(time.Millisecond))

	// No node's record holds a health epoch, and none ever did: the worker
	// logs every health transition (HL1), and a monitor writes its first
	// verdict whatever the record holds (HL3), so each node's first clean
	// round logs one with no epoch and the reason initial, not recovered.
	assertMostNodeEpochs(t, h, cl, dnCnt, cnCnt)
	assertNoEpochSetBy(t, h, w)
	assertNoFailedRoundOf(t, h, w)
	if late := h.records(w, msgLateObservation); len(late) != 0 {
		t.Logf("%d late observation records", len(late))
	}
	// Nothing moved a node's revision: every node converged at the revision
	// its plant left.
	assertMostNodeRevisions(t, h, cl, dns, cns)
	// A node that was done early was not read while the others finished: no
	// node got a second syncup up to the end of the case.
	assertMostNoSecondSyncup(t, nodes)
}

// assertMostNoSecondSyncup fails the case for every node whose fake holds
// another syncup count than the one the case saw when its syncup first read
// right.
func assertMostNoSecondSyncup(t *testing.T, nodes []*mostNodeWatch) {
	t.Helper()
	for _, n := range nodes {
		if got := len(n.agent.Record(n.key).Syncups); got != n.syncups {
			t.Errorf("%s holds %d syncups at the end, %d when its syncup read right",
				n.name, got, n.syncups)
		}
	}
}

// assertMostNodeEpochs reads every DnConf and CnConf of the cluster and
// fails the case for each that holds a health epoch, and when the counts are
// not the planted ones.
func assertMostNodeEpochs(
	t *testing.T,
	h *workerHarness,
	cl plant.Cluster,
	dnCnt, cnCnt int,
) {
	t.Helper()
	ctx := context.Background()
	kvs, _, err := h.cli.Range(ctx, model.DnConfPrefix(cl.Id))
	if err != nil {
		t.Fatalf("range dn confs: %v", err)
	}
	if len(kvs) != dnCnt {
		t.Errorf("%d dn confs, want %d", len(kvs), dnCnt)
	}
	for _, kv := range kvs {
		conf := &pb.DnConf{}
		if err := proto.Unmarshal(kv.Value, conf); err != nil {
			t.Fatalf("decode %s: %v", kv.Key, err)
		}
		if conf.GetErrEpoch() != 0 {
			t.Errorf("dn %d: err_epoch %d", conf.GetDnId(), conf.GetErrEpoch())
		}
	}
	kvs, _, err = h.cli.Range(ctx, model.CnConfPrefix(cl.Id))
	if err != nil {
		t.Fatalf("range cn confs: %v", err)
	}
	if len(kvs) != cnCnt {
		t.Errorf("%d cn confs, want %d", len(kvs), cnCnt)
	}
	for _, kv := range kvs {
		conf := &pb.CnConf{}
		if err := proto.Unmarshal(kv.Value, conf); err != nil {
			t.Fatalf("decode %s: %v", kv.Key, err)
		}
		if conf.GetErrEpoch() != 0 {
			t.Errorf("cn %d: err_epoch %d", conf.GetCnId(), conf.GetErrEpoch())
		}
	}
}

// assertMostNodeRevisions fails the case for every node whose revision key
// no longer holds the revision its plant wrote.
func assertMostNodeRevisions(
	t *testing.T,
	h *workerHarness,
	cl plant.Cluster,
	dns []plant.Dn,
	cns []plant.Cn,
) {
	t.Helper()
	ctx := context.Background()
	for _, dn := range dns {
		rev := &pb.DnRev{}
		key := model.DnRevKey(dn.Conf.GetShardCode(), cl.Id, dn.Conf.GetDnId())
		if found, err := h.cli.Get(ctx, key, rev); err != nil || !found {
			t.Fatalf("get %s: found %t, %v", key, found, err)
		}
		if rev.GetRevision() != dn.Revision {
			t.Errorf("dn %d: revision %d, planted %d",
				dn.Conf.GetDnId(), rev.GetRevision(), dn.Revision)
		}
	}
	for _, cn := range cns {
		rev := &pb.CnRev{}
		key := model.CnRevKey(cn.Conf.GetShardCode(), cl.Id, cn.Conf.GetCnId())
		if found, err := h.cli.Get(ctx, key, rev); err != nil || !found {
			t.Fatalf("get %s: found %t, %v", key, found, err)
		}
		if rev.GetRevision() != cn.Revision {
			t.Errorf("cn %d: revision %d, planted %d",
				cn.Conf.GetCnId(), rev.GetRevision(), cn.Revision)
		}
	}
}

// ---------------------------------------------------------------------------
// The widest pool
// ---------------------------------------------------------------------------

// The widest pool's shape (architecture.md, Cardinality limits): every
// per-pool maximum at once, with one meta group and one data group per
// slice as CreateStoragePool builds a pool. Every group's legs sit on
// wideGrpDnCnt disk nodes, one leg of each group on each, because PutSp
// writes the pool in one transaction and each disk node costs it six
// operations; each migration's new side goes to a disk node of its own past
// them.
const (
	wideLegCnt   = common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp
	wideGrpDnCnt = wideLegCnt
	wideDnCnt    = wideGrpDnCnt + common.MaxMigrCntPerSp
	wideCnCnt    = common.MaxCntlrCntPerSp
	// wideNodeFree is every node's free extent count before the pool.
	wideNodeFree = 1024
	// wideTdSize is every thin device's size.
	wideTdSize = 1 << 30
	// wideInterval is every check interval of the widest pool, in seconds:
	// a cntlr's round derives a row for every thin device in every slice.
	wideInterval = 2
)

// wideThreshold is the widest pool's event_threshold: long enough that a
// round missed under load starts no reaction, so the one reaction of the
// case is the failover its disable starts, which waits for no threshold
// (AR5); the primary's at least twice the cntlr interval and the leg's past
// the side's, as the gateway requires of a stored pool.
func wideThreshold() *pb.EventThreshold {
	return &pb.EventThreshold{
		PrimaryUnhealthy: 60,
		CntlrUnhealthy:   60,
		SideUnhealthy:    60,
		LegUnhealthy:     120,
	}
}

// wideMigr is one migration of the widest pool and its new side's path-row
// lever (pathRowKey) on every cntlr's fake.
type wideMigr struct {
	plant.Migr
	pathRowKey string
}

// widePool is the planted widest pool: the standard cluster's readers over it
// (it sits at the standard ids, addresses and shards, so they serve it), and
// what its plant wrote.
type widePool struct {
	*stdCluster
	tds    []plant.Td
	sss    []plant.Ss
	clones []plant.Clone
	xfers  []plant.Xfer
	migrs  []wideMigr
	// before is every key etcd held before the pool was planted.
	before map[string]bool
}

// plantWide adds the fakes and plants the widest pool: the cluster, the
// nodes, the pool with the first spare of every group provisioned, every
// thin device, subsystem, clone, transfer and migration, and the levers:
// thin rows reported OK on every cntlr while it is primary, and the path row
// of every migration's new side held at PROVISIONING on every cntlr, so no
// cutover starts (RW18).
func plantWide(t *testing.T, h *workerHarness) *widePool {
	t.Helper()
	ctx := context.Background()
	for id := uint64(1); id <= wideDnCnt; id++ {
		if _, err := h.fleet.Add(stdDnAddr(id), agentfake.Dn, agentfake.Config{}); err != nil {
			t.Fatalf("fake dn %d: %v", id, err)
		}
	}
	for id := uint64(1); id <= wideCnCnt; id++ {
		if _, err := h.fleet.Add(stdCnAddr(id), agentfake.Cn, agentfake.Config{}); err != nil {
			t.Fatalf("fake cn %d: %v", id, err)
		}
	}
	cl := h.putCluster(t, plant.ClusterSpec{
		ExtentSize:    common.MinDnExtSize,
		DnInterval:    wideInterval,
		CnInterval:    wideInterval,
		SideInterval:  wideInterval,
		CntlrInterval: wideInterval,
	})
	wp := &widePool{stdCluster: &stdCluster{
		h:    h,
		spec: stdSpec{dnCnt: wideDnCnt, cnCnt: wideCnCnt},
		cl:   cl,
	}}
	var dnSpecs []plant.DnSpec
	for id := uint64(1); id <= wideDnCnt; id++ {
		dnSpecs = append(dnSpecs, plant.DnSpec{
			Id: id, Shard: stdDnShard(id), Addr: stdDnAddr(id),
			Location: fmt.Sprintf("dn-rack%d", id), FreeExt: wideNodeFree,
		})
	}
	var err error
	if wp.dns, err = plant.PutDns(ctx, h.cli, cl, dnSpecs); err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	var cnSpecs []plant.CnSpec
	for id := uint64(1); id <= wideCnCnt; id++ {
		cnSpecs = append(cnSpecs, plant.CnSpec{
			Id: id, Shard: stdCnShard(id), Addr: stdCnAddr(id),
			Location: fmt.Sprintf("cn-rack%d", id), FreeExt: wideNodeFree,
		})
	}
	if wp.cns, err = plant.PutCns(ctx, h.cli, cl, cnSpecs); err != nil {
		t.Fatalf("PutCns: %v", err)
	}
	wp.before = wideKeys(t, h)

	// The pool's ids come from one counter, as a gateway draws them from
	// the pool's next_id: cntlr i is cntlr_id i on cn i, the first primary.
	spSpec := plant.SpSpec{
		Name:      "sp0",
		Id:        stdSpId,
		Shard:     stdSpShard,
		Threshold: wideThreshold(),
	}
	next := uint64(1)
	take := func() uint64 { next++; return next - 1 }
	for i := uint64(1); i <= wideCnCnt; i++ {
		spSpec.Cntlrs = append(spSpec.Cntlrs, plant.CntlrSpec{
			CntlrId: take(), CnId: i, Slot: uint32(i - 1), Primary: i == 1,
		})
	}
	var firstSpares []model.SideRef
	// dataLeg0Sides[i] is the side of slice i's first data leg.
	var dataLeg0Sides []uint64
	for idx := uint32(0); idx < common.MaxSliceCntPerSp; idx++ {
		sliceId := take()
		spSpec.Slices = append(spSpec.Slices, plant.SliceSpec{SliceId: sliceId, SliceIdx: idx})
		for _, meta := range []bool{true, false} {
			grpId := take()
			extCnt := uint64(2)
			if meta {
				extCnt = 1
			}
			spSpec.Groups = append(spSpec.Groups, plant.GroupSpec{
				SliceId: sliceId, GrpId: grpId, Meta: meta, ExtCnt: extCnt, Raid1: true,
			})
			for legIdx := uint32(0); legIdx < wideLegCnt; legIdx++ {
				legId, sideId := take(), take()
				spare := legIdx >= common.MaxAllocLegPerGrp
				spSpec.Legs = append(spSpec.Legs, plant.LegSpec{
					GrpId: grpId, LegId: legId, LegIdx: legIdx, Spare: spare,
				})
				spSpec.Sides = append(spSpec.Sides, plant.SideSpec{
					LegId: legId, SideId: sideId, DnId: uint64(legIdx) + 1,
				})
				if legIdx == common.MaxAllocLegPerGrp {
					firstSpares = append(firstSpares, model.SideRef{
						SliceId: sliceId, LegId: legId, SideId: sideId,
					})
				}
				if !meta && legIdx == 0 {
					dataLeg0Sides = append(dataLeg0Sides, sideId)
				}
			}
		}
	}
	if wp.sp, err = plant.PutSp(ctx, h.cli, cl, spSpec); err != nil {
		t.Fatalf("PutSp: %v", err)
	}
	// Every spare of a group but one provisioned: the gateway never leaves
	// two unprovisioned spares in one group (LegSpec), and the worker
	// provisions the other.
	for _, ref := range firstSpares {
		if ok, _, err := plant.SetProvisioned(ctx, h.cli, cl, wp.sp.Ref, ref); err != nil || !ok {
			t.Fatalf("provision spare side %d: flipped %t, %v", ref.SideId, ok, err)
		}
	}

	tdSpecs := make([]plant.TdSpec, 0, common.MaxTdCntPerSp)
	for i := 0; i < common.MaxTdCntPerSp; i++ {
		tdSpecs = append(tdSpecs, plant.TdSpec{Name: fmt.Sprintf("td%04d", i), Size: wideTdSize})
	}
	if wp.tds, err = plant.PutTds(ctx, h.cli, cl, wp.sp.Ref, tdSpecs); err != nil {
		t.Fatalf("PutTds: %v", err)
	}
	// The subsystems' namespaces take the first thin devices, the clones the
	// next ones as their destinations.
	tdIdx := 0
	for i := 0; i < common.MaxSsCntPerSp; i++ {
		spec := plant.SsSpec{
			Nqn:    fmt.Sprintf("nqn.2026-10.io.dnv-fs:ss%d", i),
			Serial: fmt.Sprintf("fs%d", i),
		}
		for n := uint32(1); n <= common.MaxNsCntPerSs; n++ {
			spec.Namespaces = append(spec.Namespaces, plant.NsSpec{
				NsIdx: n, TdId: wp.tds[tdIdx].Id,
			})
			tdIdx++
		}
		ss, err := plant.PutSs(ctx, h.cli, cl, wp.sp.Ref, spec)
		if err != nil {
			t.Fatalf("PutSs %d: %v", i, err)
		}
		wp.sss = append(wp.sss, ss)
	}
	for i := 0; i < common.MaxCloneCntPerSp; i++ {
		clone, err := plant.PutClone(ctx, h.cli, cl, wp.sp.Ref, plant.CloneSpec{
			Name:          fmt.Sprintf("clone%02d", i),
			DstTd:         wp.tds[tdIdx].Name,
			SrcSliceCnt:   1,
			SrcNqn:        fmt.Sprintf("nqn.2026-10.io.dnv-fs:src%02d", i),
			SrcNsIdx:      1,
			SrcStripeSize: 64 << 10,
			SrcBlockSize:  64 << 10,
		})
		if err != nil {
			t.Fatalf("PutClone %d: %v", i, err)
		}
		tdIdx++
		wp.clones = append(wp.clones, clone)
	}
	for i := 0; i < common.MaxXferCntPerSp; i++ {
		xfer, err := plant.PutXfer(ctx, h.cli, cl, wp.sp.Ref, plant.XferSpec{
			Name:     fmt.Sprintf("xfer%d", i),
			OriNqn:   wp.sss[i].Nqn,
			OriNsIdx: 1,
		})
		if err != nil {
			t.Fatalf("PutXfer %d: %v", i, err)
		}
		wp.xfers = append(wp.xfers, xfer)
	}
	// Migration i moves the first leg of slice i's data group to disk node
	// wideGrpDnCnt + 1 + i, its new side on a second cntlid slot.
	for i := 0; i < common.MaxMigrCntPerSp; i++ {
		migr, err := plant.PutMigr(ctx, h.cli, cl, wp.sp.Ref, plant.MigrSpec{
			Name:           fmt.Sprintf("migr%d", i),
			SrcSide:        dataLeg0Sides[i],
			DstDn:          uint64(wideGrpDnCnt + 1 + i),
			DstSlot:        1,
			CutoverPending: true,
		})
		if err != nil {
			t.Fatalf("PutMigr %d: %v", i, err)
		}
		wp.migrs = append(wp.migrs, wideMigr{
			Migr:       migr,
			pathRowKey: fmt.Sprintf("side_id_to_path.%d", migr.DstSide),
		})
	}

	held := pb.ResStatus_RES_STATUS_PROVISIONING
	for _, cntlr := range spSpec.Cntlrs {
		err := wp.cnAgent(cntlr.CnId).Update(agentfake.CntlrKey(stdSpId, cntlr.CntlrId),
			func(o *agentfake.Object) {
				o.ThinOk = true
				if o.Rows == nil {
					o.Rows = make(map[string]agentfake.Row)
				}
				for _, migr := range wp.migrs {
					o.Rows[migr.pathRowKey] = agentfake.Row{Status: &held}
				}
			})
		if err != nil {
			t.Fatalf("levers of cntlr %d: %v", cntlr.CntlrId, err)
		}
	}
	return wp
}

// wideKeys is every key etcd holds but the worker registrations, which come
// and go with the workers (VW2).
func wideKeys(t *testing.T, h *workerHarness) map[string]bool {
	t.Helper()
	keys, _, err := h.cli.RangeKeys(context.Background(), common.DnvPrefix)
	if err != nil {
		t.Fatalf("range every key: %v", err)
	}
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		if _, _, ok := model.ParseWorkerRegKey(key.Key); ok {
			continue
		}
		out[key.Key] = true
	}
	return out
}

// assertWideShape fails the case unless the loaded pool holds every
// per-pool maximum the plant wrote (architecture.md, Cardinality limits).
func (wp *widePool) assertWideShape(t *testing.T) {
	t.Helper()
	state := wp.load(t)
	nsCnt := 0
	for nqn, ss := range state.Subsystems {
		if n := len(ss.GetNsList()); n != common.MaxNsCntPerSs {
			t.Errorf("subsystem %s holds %d namespaces, want %d",
				nqn, n, common.MaxNsCntPerSs)
		}
		nsCnt += len(ss.GetNsList())
	}
	counts := []struct {
		what      string
		got, want int
	}{
		{"cntlrs", len(state.Cntlrs), common.MaxCntlrCntPerSp},
		{"slices", len(state.Slices), common.MaxSliceCntPerSp},
		{"thin devices", len(state.Tds), common.MaxTdCntPerSp},
		{"subsystems", len(state.Subsystems), common.MaxSsCntPerSp},
		{"namespaces", nsCnt, common.MaxSsCntPerSp * common.MaxNsCntPerSs},
		{"clones", len(state.Clones), common.MaxCloneCntPerSp},
		{"transfers", len(state.Xfers), common.MaxXferCntPerSp},
		{"migrations", len(state.Migrs), common.MaxMigrCntPerSp},
	}
	for _, c := range counts {
		if c.got != c.want {
			t.Errorf("the pool holds %d %s, want %d", c.got, c.what, c.want)
		}
	}
	for _, slice := range state.Slices {
		grps := append(append([]*pb.Group(nil), slice.GetMetaGrpList()...), slice.GetDataGrpList()...)
		if len(grps) != 2 {
			t.Errorf("slice %d holds %d groups, want a meta and a data group",
				slice.GetSliceIdx(), len(grps))
		}
		for _, grp := range grps {
			if len(grp.GetLegList()) != common.MaxAllocLegPerGrp ||
				len(grp.GetSpareLegList()) != common.MaxSpareLegPerGrp {
				t.Errorf("group %d holds %d legs and %d spares, want %d and %d",
					grp.GetGrpId(), len(grp.GetLegList()), len(grp.GetSpareLegList()),
					common.MaxAllocLegPerGrp, common.MaxSpareLegPerGrp)
			}
		}
	}
}

// pendingMigrs are the names of the migrations whose record still has its
// cutover pending.
func (wp *widePool) pendingMigrs(t *testing.T) []string {
	t.Helper()
	state := wp.load(t)
	var out []string
	for _, migr := range wp.migrs {
		if state.Migrs[migr.Name].GetCutoverPending() {
			out = append(out, migr.Name)
		}
	}
	return out
}

// waitWideConverged is waitConverged with deadlines for the widest pool: the
// same convergence (convergedNow), then two more rounds of every object and
// no syncup, and neither SpRev nor any node's revision moved meanwhile.
func (wp *widePool) waitWideConverged(t *testing.T, what string) convergence {
	t.Helper()
	var conv convergence
	fullShapeWait(t, 90*time.Second, what, func() (bool, string) {
		ok, why, now := wp.convergedNow(t)
		conv = now
		return ok, why
	})
	conv.at = time.Now()
	fullShapeWait(t, 10*wideInterval*time.Second, "two more rounds of every object",
		func() (bool, string) {
			for _, obj := range conv.objs {
				rec := obj.agent.Record(obj.key)
				if len(rec.Syncups) != obj.syncups {
					t.Fatalf("%s got syncup %d after the pool converged",
						obj.name, len(rec.Syncups))
				}
				if len(rec.Rounds) < obj.rounds+2 {
					return false, fmt.Sprintf("%s: %d rounds since, of 2",
						obj.name, len(rec.Rounds)-obj.rounds)
				}
			}
			return true, ""
		})
	ok, why, after := wp.convergedNow(t)
	if !ok {
		t.Fatalf("the pool stopped reading converged: %s", why)
	}
	if after.spRev != conv.spRev {
		t.Fatalf("SpRev moved from %d to %d after the pool converged",
			conv.spRev, after.spRev)
	}
	for id, rev := range conv.dnRevs {
		if after.dnRevs[id] != rev {
			t.Fatalf("dn %d's revision moved from %d to %d after the pool converged",
				id, rev, after.dnRevs[id])
		}
	}
	for id, rev := range conv.cnRevs {
		if after.cnRevs[id] != rev {
			t.Fatalf("cn %d's revision moved from %d to %d after the pool converged",
				id, rev, after.cnRevs[id])
		}
	}
	return conv
}

// TestFullShapeWidestPool pins the per-pool maximums under one whole worker:
// a pool of MaxSliceCntPerSp slices, MaxCntlrCntPerSp cntlrs, RAID1 groups of
// MaxAllocLegPerGrp legs and MaxSpareLegPerGrp spares, MaxSsCntPerSp
// subsystems of MaxNsCntPerSs namespaces, MaxTdCntPerSp thin devices,
// MaxCloneCntPerSp clones, MaxXferCntPerSp transfers and MaxMigrCntPerSp
// migrations. It converges (waitConverged's meaning); a disabled primary
// fails over to a standby (AR5); one migration runs to its cutover by the
// worker's own path once the new primary reports a live path to its new
// side (RW18), and the others stay pending; and, emptied as the gateway
// requires before a delete (SPD4) — the clones by the worker's own clone
// drain (CLD7, CLD9) — the pool drains to its end (SPD8 to SPD12): every key
// of it is gone, every node's free extent count and metadata units are back,
// and every node's fake is told it holds nothing (SPD13).
func TestFullShapeWidestPool(t *testing.T) {
	t.Parallel()
	skipFullShape(t)
	ctx := context.Background()
	h := newWorkerHarness(t, fullShapeOpts())
	start := time.Now()
	wp := plantWide(t, h)
	planted := time.Now()
	t.Logf("the widest pool planted in %v", planted.Sub(start).Round(time.Millisecond))
	wp.assertWideShape(t)

	// Converged.
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	conv := wp.waitWideConverged(t, "the widest pool to converge")
	t.Logf("converged %v after the worker started, at SpRev %d",
		conv.at.Sub(planted).Round(time.Millisecond), conv.spRev)
	if pending := wp.pendingMigrs(t); len(pending) != common.MaxMigrCntPerSp {
		t.Fatalf("migrations pending their cutover: %v, want all %d held",
			pending, common.MaxMigrCntPerSp)
	}

	// Failed over: cntlr 1, the primary, is disabled, and the next pass
	// elects a standby.
	disabled := true
	disabledAt := time.Now()
	if _, _, err := plant.SetCntlr(ctx, h.cli, wp.cl, wp.sp.Ref, 1,
		plant.CntlrFlags{Disabled: &disabled}); err != nil {
		t.Fatalf("disable cntlr 1: %v", err)
	}
	var newPrimary, failRev uint64
	fullShapeWait(t, 60*time.Second, "a standby elected primary", func() (bool, string) {
		state := wp.load(t)
		if state.Cntlrs[1].GetPrimary() {
			return false, "cntlr 1 still primary"
		}
		for _, id := range state.Conf.GetCntlrIdList() {
			if state.Cntlrs[id].GetPrimary() {
				newPrimary, failRev = id, state.SpRevision
				return true, ""
			}
		}
		return false, "no primary"
	})
	newAgent, newKey := wp.cntlrAgent(t, newPrimary)
	newAddr := wp.cntlr(t, newPrimary).GetAddrPort()
	fullShapeWait(t, 60*time.Second, "the new primary's promotion", func() (bool, string) {
		for _, msg := range newAgent.Record(newKey).Syncups {
			req := msg.(*pb.SyncupCntlrRequest)
			if req.GetCntlr().GetPrimary() && req.GetRevision() >= failRev {
				return true, ""
			}
		}
		return false, fmt.Sprintf("no promotion of cntlr %d at revision %d", newPrimary, failRev)
	})
	conv = wp.waitWideConverged(t, "the widest pool to converge after the failover")
	t.Logf("cntlr %d (on %s) elected; converged again %v after the disable",
		newPrimary, newAddr, conv.at.Sub(disabledAt).Round(time.Millisecond))
	failovers := 0
	for _, rec := range h.records(w, msgReactionApplied) {
		if rec.str("kind") == reactionFailover {
			failovers++
		} else {
			t.Errorf("a reaction other than the failover: %v", rec.attrs)
		}
	}
	if failovers != 1 {
		t.Errorf("%d failovers applied, want 1", failovers)
	}
	wp.assertSidesTold(t, newAddr, failRev)
	wp.assertCdcEntries(t)

	// Migrated on one leg: the new primary reports a live path to the first
	// migration's new side, and the worker starts its cutover. The old
	// primary, a disabled standby since the failover committed, reports a
	// live path to the second migration's new side, which starts no cutover:
	// a standby's path counts for nothing (RW18).
	cut, notCut := wp.migrs[0], wp.migrs[1]
	if err := newAgent.Update(newKey, func(o *agentfake.Object) {
		delete(o.Rows, cut.pathRowKey)
	}); err != nil {
		t.Fatalf("release the path of side %d: %v", cut.DstSide, err)
	}
	oldAgent, oldKey := wp.cntlrAgent(t, 1)
	if err := oldAgent.Update(oldKey, func(o *agentfake.Object) {
		delete(o.Rows, notCut.pathRowKey)
	}); err != nil {
		t.Fatalf("release the path of side %d: %v", notCut.DstSide, err)
	}
	fullShapeWait(t, 60*time.Second, "the cutover of "+cut.Name, func() (bool, string) {
		pending := wp.pendingMigrs(t)
		if len(pending) == common.MaxMigrCntPerSp {
			return false, "every cutover still pending"
		}
		return true, ""
	})
	if pending := wp.pendingMigrs(t); len(pending) != common.MaxMigrCntPerSp-1 ||
		slices.Contains(pending, cut.Name) {
		t.Fatalf("migrations pending after the cutover: %v, want all but %s",
			pending, cut.Name)
	}
	cutovers := 0
	for _, rec := range h.records(w, msgFlipApplied) {
		if rec.str("kind") != flipKindCutover {
			continue
		}
		cutovers++
		if rec.str("migr_name") != cut.Name || rec.num("cntlr_id") != newPrimary {
			t.Errorf("a cutover of %s on cntlr %d, want %s on the new primary %d",
				rec.str("migr_name"), rec.num("cntlr_id"), cut.Name, newPrimary)
		}
	}
	if cutovers != 1 {
		t.Errorf("%d cutovers applied, want 1", cutovers)
	}
	// Converged again, so every cntlr, the old primary too, has answered two
	// more rounds since: the second migration's cutover still waits on the
	// standby's path.
	wp.waitWideConverged(t, "the widest pool to converge after the cutover")
	wp.assertCutoverTold(t, cut)
	if pending := wp.pendingMigrs(t); !slices.Contains(pending, notCut.Name) {
		t.Errorf("%s cut over on a standby's path, the disabled old primary's, pending %v",
			notCut.Name, pending)
	}

	// Drained: emptied as the gateway requires before it latches a pool
	// (SPD4), latched, and taken apart by the worker's drain.
	drainStart := time.Now()
	wp.drainClones(t)
	wp.dropChildren(t, cut.Name)
	if _, err := plant.SetDeleting(ctx, h.cli, wp.cl, wp.sp.Ref); err != nil {
		t.Fatalf("latch the pool: %v", err)
	}
	fullShapeWait(t, 60*time.Second, "the pool's drain to its end", func() (bool, string) {
		found, err := h.cli.Get(ctx, model.SpConfKey(wp.cl.Id, wp.sp.Ref.Name), &pb.SpConf{})
		if err != nil {
			return false, err.Error()
		}
		if found {
			return false, "the SpConf is still there"
		}
		return true, ""
	})
	t.Logf("emptied and drained in %v", time.Since(drainStart).Round(time.Millisecond))
	wp.assertDrained(t, w)
}

// wideLegSide is one side of a load and the leg that carries it.
type wideLegSide struct {
	leg  *pb.Leg
	side *pb.Side
}

// wideSides lists every side of a load, spare legs' included.
func wideSides(state *model.SpState) []wideLegSide {
	var out []wideLegSide
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice := state.Slices[sliceId]
		for _, grp := range append(slices.Clone(slice.GetMetaGrpList()), slice.GetDataGrpList()...) {
			for _, leg := range append(slices.Clone(grp.GetLegList()), grp.GetSpareLegList()...) {
				for _, side := range leg.GetSideList() {
					out = append(out, wideLegSide{leg, side})
				}
			}
		}
	}
	return out
}

// lastSideSyncup is the newest SyncupSide a side's fake got.
func (wp *widePool) lastSideSyncup(legId, sideId uint64, addr string) *pb.SyncupSideRequest {
	msg, _, _ := lastSyncup(wp.h.fleet.Agent(addr), agentfake.SideKey(stdSpId, legId, sideId))
	req, _ := msg.(*pb.SyncupSideRequest)
	return req
}

// assertSidesTold fails the case for every side whose fake was never told,
// at the failover's revision or later, that the CN at newAddr holds the
// primary (RW15).
func (wp *widePool) assertSidesTold(t *testing.T, newAddr string, failRev uint64) {
	t.Helper()
	cnId := wp.cnConfAt(t, newAddr).GetCnId()
	for _, ls := range wideSides(wp.load(t)) {
		told := false
		agent := wp.h.fleet.Agent(ls.side.GetAddrPort())
		key := agentfake.SideKey(stdSpId, ls.leg.GetLegId(), ls.side.GetSideId())
		for _, msg := range agent.Record(key).Syncups {
			req := msg.(*pb.SyncupSideRequest)
			if req.GetRevision() >= failRev && req.GetSideConf().GetPrimaryCnId() == cnId {
				told = true
				break
			}
		}
		if !told {
			t.Errorf("side %d was never told cn %d is primary", ls.side.GetSideId(), cnId)
		}
	}
}

// assertCdcEntries fails the case unless every subsystem's discovery record
// lists the cntlrs the listing rule gives (architecture.md [D18]).
func (wp *widePool) assertCdcEntries(t *testing.T) {
	t.Helper()
	state := wp.load(t)
	var cntlrs []*pb.Cntlr
	for _, id := range state.Conf.GetCntlrIdList() {
		cntlrs = append(cntlrs, state.Cntlrs[id])
	}
	want := model.CdcTrConfList(cntlrs)
	for _, ss := range wp.sss {
		entry := &pb.CdcEntry{}
		wp.mustGet(t, ss.CdcKey, entry)
		if !trConfsEqual(entry.GetNvmeTrConfList(), want) {
			t.Errorf("%s's discovery record lists %v, want %v",
				ss.Nqn, entry.GetNvmeTrConfList(), want)
		}
	}
	if len(want) != wideCnCnt-1 {
		t.Errorf("the listing rule lists %d cntlrs, want every one but the disabled",
			len(want))
	}
}

// cnConfAt reads the CnConf at addr.
func (wp *widePool) cnConfAt(t *testing.T, addr string) *pb.CnConf {
	t.Helper()
	conf := &pb.CnConf{}
	wp.mustGet(t, model.CnConfKey(wp.cl.Id, addr), conf)
	return conf
}

// assertCutoverTold fails the case unless both sides of the cut-over
// migration were last told its cutover is no longer pending, the source
// side also that the new side is provisioned (RW15).
func (wp *widePool) assertCutoverTold(t *testing.T, cut wideMigr) {
	t.Helper()
	// A migration's source side is the first leg's, which sits on dn 1.
	src := wp.lastSideSyncup(cut.LegId, cut.SrcSide, stdDnAddr(1))
	dst := wp.lastSideSyncup(cut.LegId, cut.DstSide, cut.Addr)
	switch {
	case src == nil || dst == nil:
		t.Errorf("%s: no syncup of a side, src %v, dst %v", cut.Name, src, dst)
	case src.GetMigrSrcConf().GetCutoverPending() || !src.GetMigrSrcConf().GetDstProvisioned():
		t.Errorf("%s: the source side was last told %v", cut.Name, src.GetMigrSrcConf())
	case dst.GetMigrDstConf().GetCutoverPending():
		t.Errorf("%s: the new side was last told %v", cut.Name, dst.GetMigrDstConf())
	}
}

// drainClones latches every clone as DeleteClone does (CLD4) and waits for
// the worker's clone drain to remove each (CLD9): the clone records and
// names go, and every CN gets the clones' metadata units back.
func (wp *widePool) drainClones(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, clone := range wp.clones {
		if _, err := plant.SetCloneDeleting(ctx, wp.h.cli, wp.cl, wp.sp.Ref, clone.Name); err != nil {
			t.Fatalf("latch %s: %v", clone.Name, err)
		}
	}
	fullShapeWait(t, 60*time.Second, "every clone drained", func() (bool, string) {
		if n := len(wp.load(t).Conf.GetCloneNameList()); n != 0 {
			return false, fmt.Sprintf("%d clones left", n)
		}
		return true, ""
	})
	for id := uint64(1); id <= wideCnCnt; id++ {
		if units := wp.cnConf(t, id).GetUsedMetaUnits(); units != 0 {
			t.Errorf("cn %d holds %d metadata units after the clone drain", id, units)
		}
	}
}

// wideTdsPerTxn is how many thin devices one transaction of dropChildren
// deletes: half etcd's cap on a transaction (EtcdMaxTxnOps). Each delete is
// one operation of the commit and, since RunSTM's serializable snapshot
// compares every key it writes, one compare; the pool's SpConf and SpRev,
// read and written, add a few of each.
const wideTdsPerTxn = common.EtcdMaxTxnOps / 2

// dropChildren removes the transfers, the subsystems with their namespaces
// and discovery records, the thin devices and the migrations — the one whose
// cutover started finished, the others cancelled — writing what the
// gateway's DeleteTransfer, DeleteNamespace and DeleteSubsystem,
// DeleteThinDevice, FinishMigration and CancelMigration write
// (architecture.md, Transfers; Subsystems, namespaces; Thin devices;
// Migrations): one transaction per kind, the thin devices in batches and the
// migrations one each, each bumping SpRev once. The worker's tests do not
// import the gateway, and no worker path removes these.
func (wp *widePool) dropChildren(t *testing.T, finished string) {
	t.Helper()
	ref, cid := wp.sp.Ref, wp.cl.Id
	inPool := func(what string, f func(s etcdutil.STM, conf *pb.SpConf) error) {
		t.Helper()
		err := wp.h.cli.RunSTM(context.Background(), func(s etcdutil.STM) error {
			conf := &pb.SpConf{}
			if !s.Get(model.SpConfKey(cid, ref.Name), conf) {
				return fmt.Errorf("no SpConf")
			}
			if err := f(s, conf); err != nil {
				return err
			}
			s.Put(model.SpConfKey(cid, ref.Name), conf)
			return model.BumpSpRev(s, "fullshape "+what, ref.Shard, cid, ref.Id)
		})
		if err != nil {
			t.Fatalf("delete %s: %v", what, err)
		}
	}
	inPool("transfers", func(s etcdutil.STM, conf *pb.SpConf) error {
		for _, name := range conf.GetXferNameList() {
			s.Del(model.TransferKey(cid, ref.Id, name))
		}
		conf.XferNameList = nil
		return nil
	})
	inPool("subsystems", func(s etcdutil.STM, conf *pb.SpConf) error {
		for _, nqn := range conf.GetNqnList() {
			ss := &pb.Subsystem{}
			if !s.Get(model.SubsystemKey(cid, ref.Id, nqn), ss) {
				return fmt.Errorf("no subsystem %s", nqn)
			}
			s.Del(model.SubsystemKey(cid, ref.Id, nqn))
			s.Del(model.CdcEntryKey(cid, ref.Shard, ref.Id, ss.GetSsId()))
		}
		conf.NqnList = nil
		return nil
	})
	for len(wp.load(t).Conf.GetTdNameList()) != 0 {
		inPool("thin devices", func(s etcdutil.STM, conf *pb.SpConf) error {
			names := conf.GetTdNameList()
			batch := names[:min(wideTdsPerTxn, len(names))]
			for _, name := range batch {
				s.Del(model.ThinDeviceKey(cid, ref.Id, name))
			}
			conf.TdNameList = slices.Clone(names[len(batch):])
			return nil
		})
	}
	for _, migr := range wp.migrs {
		inPool("migration "+migr.Name, func(s etcdutil.STM, conf *pb.SpConf) error {
			return wp.endMigration(s, conf, migr, migr.Name == finished)
		})
	}
}

// endMigration is FinishMigration (finish) or CancelMigration inside
// dropChildren's STM: the source side, or the new one, leaves the leg, its
// disk node gets its extents back and loses its pointer, with its capacity
// key and revision moved; the new side's disk node gets the migration's
// metadata units back; and the record and its name go (architecture.md,
// Migrations; Metadata units).
func (wp *widePool) endMigration(
	s etcdutil.STM,
	conf *pb.SpConf,
	migr wideMigr,
	finish bool,
) error {
	cid := wp.cl.Id
	record := &pb.Migration{}
	if !s.Get(model.MigrationKey(cid, wp.sp.Ref.Id, migr.Name), record) {
		return fmt.Errorf("no migration %s", migr.Name)
	}
	sliceKey := model.SliceKey(cid, wp.sp.Ref.Id, migr.SliceId)
	slice := &pb.Slice{}
	if !s.Get(sliceKey, slice) {
		return fmt.Errorf("no slice %d", migr.SliceId)
	}
	dropped := record.GetDstSideId()
	if finish {
		dropped = record.GetSrcSideId()
	}
	var droppedAddr, dstAddr string
	var extCnt uint64
	for _, grp := range append(slices.Clone(slice.GetMetaGrpList()), slice.GetDataGrpList()...) {
		for _, leg := range grp.GetLegList() {
			if leg.GetLegId() != migr.LegId {
				continue
			}
			extCnt = grp.GetExtCnt()
			var kept []*pb.Side
			for _, side := range leg.GetSideList() {
				if side.GetSideId() == record.GetDstSideId() {
					dstAddr = side.GetAddrPort()
				}
				if side.GetSideId() == dropped {
					droppedAddr = side.GetAddrPort()
					continue
				}
				kept = append(kept, side)
			}
			leg.SideList = kept
		}
	}
	if droppedAddr == "" || dstAddr == "" {
		return fmt.Errorf("migration %s: sides not on leg %d", migr.Name, migr.LegId)
	}
	dnOf := func(addr string) (*pb.DnConf, *pb.DnConf, error) {
		dn := &pb.DnConf{}
		if !s.Get(model.DnConfKey(cid, addr), dn) {
			return nil, nil, fmt.Errorf("no dn at %s", addr)
		}
		return dn, proto.Clone(dn).(*pb.DnConf), nil
	}
	oldDn, newDn, err := dnOf(droppedAddr)
	if err != nil {
		return err
	}
	newDn.FreeExtCnt += extCnt
	var ptrs []*pb.SidePointer
	for _, ptr := range newDn.GetSidePtrList() {
		if ptr.GetSpId() != wp.sp.Ref.Id || ptr.GetSideId() != dropped {
			ptrs = append(ptrs, ptr)
		}
	}
	newDn.SidePtrList = ptrs
	if !finish {
		newDn.UsedMetaUnits = model.ReleaseMetaUnits(newDn.GetUsedMetaUnits(), record.GetMetaUnits())
	}
	s.Put(model.DnConfKey(cid, droppedAddr), newDn)
	model.MaintainDnCapacity(s, cid, droppedAddr, wp.cl.Conf, oldDn, newDn)
	if err := model.BumpDnRev(s, "fullshape migration", cid, newDn); err != nil {
		return err
	}
	if finish {
		oldDst, newDst, err := dnOf(dstAddr)
		if err != nil {
			return err
		}
		newDst.UsedMetaUnits = model.ReleaseMetaUnits(newDst.GetUsedMetaUnits(), record.GetMetaUnits())
		s.Put(model.DnConfKey(cid, dstAddr), newDst)
		model.MaintainDnCapacity(s, cid, dstAddr, wp.cl.Conf, oldDst, newDst)
	}
	s.Put(sliceKey, slice)
	s.Del(model.MigrationKey(cid, wp.sp.Ref.Id, migr.Name))
	conf.MigrNameList = slices.DeleteFunc(slices.Clone(conf.GetMigrNameList()),
		func(name string) bool { return name == migr.Name })
	return nil
}

// assertDrained fails the case unless the drain left nothing of the pool
// (SPD9 to SPD13): the keys etcd holds are the keys it held before the pool
// was planted, every node record is back at its planted free extent count
// with no pointer, no metadata unit and no health epoch, every node's fake
// was told at its latest revision that it holds nothing, and the drain
// removed the cntlrs in one step and each slice in a step of its own.
func (wp *widePool) assertDrained(t *testing.T, w *harnessWorker) {
	t.Helper()
	h := wp.h
	after := wideKeys(t, h)
	var extra, missing []string
	for key := range after {
		if !wp.before[key] {
			extra = append(extra, key)
		}
	}
	for key := range wp.before {
		if !after[key] {
			missing = append(missing, key)
		}
	}
	slices.Sort(extra)
	slices.Sort(missing)
	if len(extra) != 0 || len(missing) != 0 {
		t.Errorf("after the drain etcd holds %d keys it did not before the pool, "+
			"the first %q, and lacks %d it held, the first %q",
			len(extra), extra[:min(5, len(extra))], len(missing), missing[:min(5, len(missing))])
	}
	for id := uint64(1); id <= wideDnCnt; id++ {
		conf := wp.dnConf(t, id)
		if conf.GetFreeExtCnt() != wideNodeFree || len(conf.GetSidePtrList()) != 0 ||
			conf.GetUsedMetaUnits() != 0 || conf.GetErrEpoch() != 0 {
			t.Errorf("dn %d after the drain: free %d of %d, %d sides, %d units, err_epoch %d",
				id, conf.GetFreeExtCnt(), wideNodeFree, len(conf.GetSidePtrList()),
				conf.GetUsedMetaUnits(), conf.GetErrEpoch())
		}
	}
	for id := uint64(1); id <= wideCnCnt; id++ {
		conf := wp.cnConf(t, id)
		if conf.GetFreeExtCnt() != wideNodeFree || len(conf.GetCntlrPtrList()) != 0 ||
			conf.GetUsedMetaUnits() != 0 || conf.GetErrEpoch() != 0 {
			t.Errorf("cn %d after the drain: free %d of %d, %d cntlrs, %d units, err_epoch %d",
				id, conf.GetFreeExtCnt(), wideNodeFree, len(conf.GetCntlrPtrList()),
				conf.GetUsedMetaUnits(), conf.GetErrEpoch())
		}
	}
	fullShapeWait(t, 30*time.Second, "every node's fake told it holds nothing",
		func() (bool, string) {
			for id := uint64(1); id <= wideDnCnt; id++ {
				msg, _, _ := lastSyncup(wp.dnAgent(id), agentfake.DnKey)
				req, _ := msg.(*pb.SyncupDnRequest)
				if rev := wp.dnRev(t, id); req == nil || req.GetRevision() != rev ||
					len(req.GetSidePointerList()) != 0 {
					return false, fmt.Sprintf("dn %d: last syncup %v, revision %d", id, req != nil, rev)
				}
			}
			for id := uint64(1); id <= wideCnCnt; id++ {
				msg, _, _ := lastSyncup(wp.cnAgent(id), agentfake.CnKey)
				req, _ := msg.(*pb.SyncupCnRequest)
				if rev := wp.cnRev(t, id); req == nil || req.GetRevision() != rev ||
					len(req.GetCntlrPointerList()) != 0 {
					return false, fmt.Sprintf("cn %d: last syncup %v, revision %d", id, req != nil, rev)
				}
			}
			return true, ""
		})
	t.Logf("the drain: %d step records, %d failed attempts, %d end records",
		len(h.records(w, msgSpDrainStep)), len(h.records(w, msgSpDrainFailed)),
		len(h.records(w, msgSpDrainDone)))
	// The drain logs its end exactly once and no failed step, the last one
	// included (SPD12).
	h.waitRecord(t, 10*time.Second, w, msgSpDrainDone, nil)
	if done := h.records(w, msgSpDrainDone); len(done) != 1 {
		t.Errorf("%d %q records, want exactly 1", len(done), msgSpDrainDone)
	}
	if failed := h.records(w, msgSpDrainFailed); len(failed) != 0 {
		t.Errorf("%d %q records, want none: %v", len(failed), msgSpDrainFailed,
			failed[0].attrs)
	}
	cntlrSteps := 0
	sliceSteps := map[uint64]int{}
	for _, rec := range h.records(w, msgSpDrainStep) {
		switch rec.str("phase") {
		case drainPhaseCntlrs:
			cntlrSteps++
			if rec.num("cntlr_cnt") != wideCnCnt {
				t.Errorf("a cntlrs step removed %d cntlrs, want all %d",
					rec.num("cntlr_cnt"), wideCnCnt)
			}
		case drainPhaseSlice:
			sliceSteps[rec.num("slice_id")]++
			if rec.str("slice_done") != "true" || rec.num("grp_cnt") != 2 {
				t.Errorf("a slice step: %v, want both groups of the slice and the slice done",
					rec.attrs)
			}
		}
	}
	if cntlrSteps != 1 || len(sliceSteps) != common.MaxSliceCntPerSp {
		t.Errorf("%d cntlrs steps and steps on %d slices, want 1 and %d",
			cntlrSteps, len(sliceSteps), common.MaxSliceCntPerSp)
	}
	for sliceId, n := range sliceSteps {
		if n != 1 {
			t.Errorf("slice %d took %d steps, want 1", sliceId, n)
		}
	}
}
