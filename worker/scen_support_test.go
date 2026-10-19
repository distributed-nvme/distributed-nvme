package worker

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The helpers of the whole-worker scenario cases of the reactions, the
// migration, the drains and the agent restarts (scen_reactions_test.go,
// scen_migration_test.go, scen_drains_test.go, scen_restarts_test.go): a
// planting of the standard cluster with the shapes those cases need beside
// it, readers of the worker's records, and a hold of one RPC at a fake.

// scenShape is the standard cluster (plantStd) with what a scenario adds to
// it: fakes built with a config of their own, disk nodes past the standard
// three, spare legs in the data group, and a pool without its thin device
// and subsystem, as a pool DeleteStoragePool latches holds none.
type scenShape struct {
	// edit changes the standard spec first, as plantStd's edit does.
	edit func(*stdSpec)
	// extraDns are disk nodes planted past the spec's dnCnt, numbered on
	// from it; they hold nothing unless a spare names them.
	extraDns int
	// dnCfg and cnCfg are the fake configs of some nodes, by node id.
	dnCfg map[uint64]agentfake.Config
	cnCfg map[uint64]agentfake.Config
	// spares are spare legs of the data group: leg, side and the dn holding
	// the side, planted unprovisioned (plant.LegSpec).
	spares []struct{ legId, sideId, dnId uint64 }
	// noTdSs leaves the thin device and the subsystem out.
	noTdSs bool
}

// plantScen adds the fakes of shape and plants it; the returned cluster's
// spec counts every disk node planted, so waitConverged covers them all.
func (h *workerHarness) plantScen(t *testing.T, shape scenShape) *stdCluster {
	t.Helper()
	ctx := context.Background()
	spec := defaultStdSpec()
	if shape.edit != nil {
		shape.edit(&spec)
	}
	spec.dnCnt += shape.extraDns
	c := &stdCluster{h: h, spec: spec}
	for id := uint64(1); id <= uint64(spec.dnCnt); id++ {
		if _, err := h.fleet.Add(stdDnAddr(id), agentfake.Dn,
			shape.dnCfg[id]); err != nil {
			t.Fatalf("fake dn %d: %v", id, err)
		}
	}
	for id := uint64(1); id <= uint64(spec.cnCnt); id++ {
		if _, err := h.fleet.Add(stdCnAddr(id), agentfake.Cn,
			shape.cnCfg[id]); err != nil {
			t.Fatalf("fake cn %d: %v", id, err)
		}
	}
	cl := h.putCluster(t, plant.ClusterSpec{ExtentSize: common.MinDnExtSize})
	c.cl = cl
	var err error
	var dnSpecs []plant.DnSpec
	for id := uint64(1); id <= uint64(spec.dnCnt); id++ {
		dnSpecs = append(dnSpecs, plant.DnSpec{
			Id: id, Shard: stdDnShard(id), Addr: stdDnAddr(id),
			Location: fmt.Sprintf("dn-rack%d", id), FreeExt: spec.nodeFree,
		})
	}
	if c.dns, err = plant.PutDns(ctx, h.cli, cl, dnSpecs); err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	var cnSpecs []plant.CnSpec
	for id := uint64(1); id <= uint64(spec.cnCnt); id++ {
		cnSpecs = append(cnSpecs, plant.CnSpec{
			Id: id, Shard: stdCnShard(id), Addr: stdCnAddr(id),
			Location: fmt.Sprintf("cn-rack%d", id), FreeExt: spec.nodeFree,
		})
	}
	if c.cns, err = plant.PutCns(ctx, h.cli, cl, cnSpecs); err != nil {
		t.Fatalf("PutCns: %v", err)
	}
	spSpec := plant.SpSpec{
		Name:      "sp0",
		Id:        stdSpId,
		Shard:     stdSpShard,
		Slots:     []uint32{0, 1, 2},
		Threshold: spec.threshold,
		Cntlrs: []plant.CntlrSpec{
			{CntlrId: 1, CnId: 1, Slot: 0, Primary: true},
			{CntlrId: 2, CnId: 2, Slot: 1},
		},
		Slices: []plant.SliceSpec{{SliceId: stdSliceId, SliceIdx: 0}},
		Groups: []plant.GroupSpec{
			{SliceId: stdSliceId, GrpId: stdMetaGrpId, Meta: true, ExtCnt: 1, Raid1: true},
			{SliceId: stdSliceId, GrpId: stdDataGrpId, ExtCnt: 2, Raid1: true},
		},
		Legs: []plant.LegSpec{
			{GrpId: stdMetaGrpId, LegId: 3, LegIdx: 0},
			{GrpId: stdMetaGrpId, LegId: 4, LegIdx: 1},
			{GrpId: stdDataGrpId, LegId: 6, LegIdx: 0},
			{GrpId: stdDataGrpId, LegId: 7, LegIdx: 1},
		},
	}
	for _, side := range stdSides {
		spSpec.Sides = append(spSpec.Sides, plant.SideSpec{
			LegId: side.legId, SideId: side.sideId, DnId: side.dnId,
		})
	}
	for idx, spare := range shape.spares {
		spSpec.Legs = append(spSpec.Legs, plant.LegSpec{
			GrpId: stdDataGrpId, LegId: spare.legId,
			LegIdx: uint32(2 + idx), Spare: true,
		})
		spSpec.Sides = append(spSpec.Sides, plant.SideSpec{
			LegId: spare.legId, SideId: spare.sideId, DnId: spare.dnId,
		})
	}
	if c.sp, err = plant.PutSp(ctx, h.cli, cl, spSpec); err != nil {
		t.Fatalf("PutSp: %v", err)
	}
	if spec.thinOk {
		for cntlrId, cnId := range map[uint64]uint64{1: 1, 2: 2} {
			setLever(t, c.cnAgent(cnId), agentfake.CntlrKey(stdSpId, cntlrId),
				func(o *agentfake.Object) { o.ThinOk = true })
		}
	}
	if shape.noTdSs {
		return c
	}
	if c.td, err = plant.PutTd(ctx, h.cli, cl, c.sp.Ref, plant.TdSpec{
		Name: "td0", Size: 1 << 30,
	}); err != nil {
		t.Fatalf("PutTd: %v", err)
	}
	if c.ss, err = plant.PutSs(ctx, h.cli, cl, c.sp.Ref, plant.SsSpec{
		Nqn:        "nqn.2026-10.io.dnv-wh:ss0",
		Serial:     "s0",
		Namespaces: []plant.NsSpec{{NsIdx: 1, TdId: c.td.Id}},
	}); err != nil {
		t.Fatalf("PutSs: %v", err)
	}
	return c
}

// setLever edits one object's levers on a fake and fails the case when the
// fake refuses the edit.
func setLever(
	t *testing.T,
	agent *agentfake.Agent,
	key string,
	edit func(o *agentfake.Object),
) {
	t.Helper()
	if err := agent.Update(key, edit); err != nil {
		t.Fatalf("lever on %s: %v", key, err)
	}
}

// rowLever is a row override of a cntlr that applies while it is primary,
// or always.
func rowLever(status pb.ResStatus, details string, whenPrimary bool) agentfake.Row {
	row := agentfake.Row{Status: &status, WhenPrimary: whenPrimary}
	if details != "" {
		row.Details = &details
	}
	return row
}

// setRow sets one row override of an object, keeping its others.
func setRow(t *testing.T, agent *agentfake.Agent, key, rowKey string, row agentfake.Row) {
	t.Helper()
	setLever(t, agent, key, func(o *agentfake.Object) {
		if o.Rows == nil {
			o.Rows = make(map[string]agentfake.Row)
		}
		o.Rows[rowKey] = row
	})
}

// clearRow drops one row override of an object.
func clearRow(t *testing.T, agent *agentfake.Agent, key, rowKey string) {
	t.Helper()
	setLever(t, agent, key, func(o *agentfake.Object) { delete(o.Rows, rowKey) })
}

// statusPtr is a lever's status.
func statusPtr(status pb.ResStatus) *pb.ResStatus { return &status }

// uint64Ptr is a lever's count.
func uint64Ptr(v uint64) *uint64 { return &v }

// nextSpId is the pool's next free sub-object id (model.SpNextId), which the
// next reaction that creates an object takes first.
func (c *stdCluster) nextSpId(t *testing.T) uint64 {
	t.Helper()
	return model.SpNextId(c.load(t).Conf)
}

// sideOf finds a side of the pool's slices by its id: its leg, whether that
// leg is a spare, and the side; ok is false when no slice holds such a side.
func (c *stdCluster) sideOf(t *testing.T, sideId uint64) (*pb.Leg, bool, *pb.Side, bool) {
	t.Helper()
	for _, slice := range c.load(t).Slices {
		for _, grp := range append(slices.Clone(slice.GetMetaGrpList()), slice.GetDataGrpList()...) {
			for _, leg := range grp.GetLegList() {
				for _, side := range leg.GetSideList() {
					if side.GetSideId() == sideId {
						return leg, false, side, true
					}
				}
			}
			for _, leg := range grp.GetSpareLegList() {
				for _, side := range leg.GetSideList() {
					if side.GetSideId() == sideId {
						return leg, true, side, true
					}
				}
			}
		}
	}
	return nil, false, nil, false
}

// dataGrp is the standard data group as the pool's record holds it now.
func (c *stdCluster) dataGrp(t *testing.T) *pb.Group {
	t.Helper()
	for _, grp := range c.load(t).Slices[stdSliceId].GetDataGrpList() {
		if grp.GetGrpId() == stdDataGrpId {
			return grp
		}
	}
	t.Fatalf("no data group %d in the pool", stdDataGrpId)
	return nil
}

// legIds are the leg ids of a leg list.
func legIds(legs []*pb.Leg) []uint64 {
	var ids []uint64
	for _, leg := range legs {
		ids = append(ids, leg.GetLegId())
	}
	slices.Sort(ids)
	return ids
}

// pointerId reads one id field out of a record's side_pointer or
// cntlr_pointer attribute.
func pointerId(rec logRecord, attr, field string) (uint64, bool) {
	v, ok := rec.attrs[attr]
	if !ok {
		return 0, false
	}
	ptr, ok := v.Any().(map[string]any)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseUint(fmt.Sprint(ptr[field]), 10, 64)
	return id, err == nil
}

// cntlrIdOf is the cntlr_id of a cntlr child's record, read from its
// cntlr_pointer.
func cntlrIdOf(rec logRecord) (uint64, bool) {
	return pointerId(rec, "cntlr_pointer", "cntlr_id")
}

// isNodeRec reports whether a record is the dn role's (or the cn role's)
// own about one node, not one of a side or a cntlr on it.
func isNodeRec(rec logRecord, idAttr string, id uint64) bool {
	_, side := rec.attrs["side_pointer"]
	_, cntlr := rec.attrs["cntlr_pointer"]
	_, has := rec.attrs[idAttr]
	return has && !side && !cntlr && rec.num(idAttr) == id
}

// countRecs is how many records of w (nil: any) with msg match accepts.
func (h *workerHarness) countRecs(
	w *harnessWorker,
	msg string,
	match func(logRecord) bool,
) int {
	n := 0
	for _, rec := range h.records(w, msg) {
		if match == nil || match(rec) {
			n++
		}
	}
	return n
}

// reactionIs matches a reaction record of one kind, and of one reason when
// reason is not empty.
func reactionIs(kind, reason string) func(logRecord) bool {
	return func(rec logRecord) bool {
		return rec.str("kind") == kind && (reason == "" || rec.str("reason") == reason)
	}
}

// and matches a record both functions accept.
func and(a, b func(logRecord) bool) func(logRecord) bool {
	return func(rec logRecord) bool { return a(rec) && b(rec) }
}

// numIs matches a record whose attribute key holds want.
func numIs(key string, want uint64) func(logRecord) bool {
	return func(rec logRecord) bool { return rec.num(key) == want }
}

// waitCount waits until at least n records of w with msg match accepts, and
// returns them.
func (h *workerHarness) waitCount(
	t *testing.T,
	timeout time.Duration,
	w *harnessWorker,
	msg string,
	n int,
	match func(logRecord) bool,
) []logRecord {
	t.Helper()
	var found []logRecord
	h.waitUntil(t, timeout, fmt.Sprintf("%d %q records", n, msg),
		func() (bool, string) {
			found = found[:0]
			for _, rec := range h.records(w, msg) {
				if match == nil || match(rec) {
					found = append(found, rec)
				}
			}
			return len(found) >= n, fmt.Sprintf("%d so far", len(found))
		})
	return found
}

// waitRounds waits until the fake got n more check rounds about each of
// keys than it had when the wait began: the case's clock for "for a while",
// each round one interval of its object.
func (h *workerHarness) waitRounds(
	t *testing.T,
	agent *agentfake.Agent,
	n int,
	keys ...string,
) {
	t.Helper()
	from := make(map[string]int, len(keys))
	for _, key := range keys {
		from[key] = len(agent.Record(key).Rounds)
	}
	h.waitUntil(t, time.Duration(n)*5*time.Second+10*time.Second,
		fmt.Sprintf("%d more rounds of %v", n, keys),
		func() (bool, string) {
			for _, key := range keys {
				if got := len(agent.Record(key).Rounds) - from[key]; got < n {
					return false, fmt.Sprintf("%s: %d of %d", key, got, n)
				}
			}
			return true, ""
		})
}

// putStdClone plants a clone of the standard pool, spec with its clone meta
// group placed as CreateClone places one in this RAID1 pool: two legs of one
// side each, on dn 1 and dn 2, which sit in two locations, every id the
// pool's next; dn 3 holds none of it.
func (c *stdCluster) putStdClone(t *testing.T, spec plant.CloneSpec) plant.Clone {
	t.Helper()
	spec.Legs = []plant.CloneLegSpec{
		{LegIdx: 0, DnId: 1},
		{LegIdx: 1, DnId: 2},
	}
	clone, err := plant.PutClone(context.Background(), c.h.cli, c.cl, c.sp.Ref, spec)
	if err != nil {
		t.Fatalf("PutClone %s: %v", spec.Name, err)
	}
	return clone
}

// dnLedger is one disk node's charge as etcd holds it: its free extents, its
// side pointers (sidePtrSet), whether the capacity key of that free count is
// there, and its revision.
type dnLedger struct {
	free  uint64
	ptrs  string
	keyed bool
	rev   uint64
}

// dnLedgerOf reads dn id's ledger.
func (c *stdCluster) dnLedgerOf(t *testing.T, id uint64) dnLedger {
	t.Helper()
	conf := c.dnConf(t, id)
	return dnLedger{
		free:  conf.GetFreeExtCnt(),
		ptrs:  sidePtrSet(conf.GetSidePtrList()),
		keyed: c.dnCapacityKeyed(t, id),
		rev:   c.dnRev(t, id),
	}
}

// dnFree and cnFree read a node's free extent count from its record.
func (c *stdCluster) dnFree(t *testing.T, id uint64) uint64 {
	t.Helper()
	return c.dnConf(t, id).GetFreeExtCnt()
}

func (c *stdCluster) cnFree(t *testing.T, id uint64) uint64 {
	t.Helper()
	return c.cnConf(t, id).GetFreeExtCnt()
}

// callHold holds one RPC at a fake, through the fake's request hook
// (agentfake.Config.Refresh): while it is armed, every call of the method
// waits at its arrival until the hold is released or the caller's deadline
// ends it, as an agent that takes a request and does not answer. A call
// that ends by its deadline applies nothing and is not in the fake's
// Record, which notes a request only once it gets past the hook.
type callHold struct {
	method string

	mu      sync.Mutex
	armed   bool
	arrived []time.Time
	release chan struct{}
}

// newCallHold arms a hold of method, the gRPC method's last path element
// (agentfake.MethodPushMigrBitmap, say).
func newCallHold(method string) *callHold {
	return &callHold{method: method, armed: true, release: make(chan struct{})}
}

// config is the fake config whose request hook is the hold.
func (c *callHold) config() agentfake.Config {
	return agentfake.Config{Refresh: c.refresh}
}

func (c *callHold) refresh(ctx context.Context) {
	full, ok := grpc.Method(ctx)
	if !ok || !strings.HasSuffix(full, "/"+c.method) {
		return
	}
	c.mu.Lock()
	c.arrived = append(c.arrived, time.Now())
	armed, release := c.armed, c.release
	c.mu.Unlock()
	if !armed {
		return
	}
	select {
	case <-release:
	case <-ctx.Done():
	}
}

// open releases every held call and lets every later one through.
func (c *callHold) open() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.armed {
		c.armed = false
		close(c.release)
	}
}

// arrivals are the times the method's calls reached the fake, held or not.
func (c *callHold) arrivals() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.arrived)
}
