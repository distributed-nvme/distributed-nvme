package worker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenarios of check rounds, syncups and several agents
// (dnv-worker.md, Integration test plan, What the Go cases prove). Each case
// plays one agent, or several, answering late, not at all, from an old
// observation, with a refusal or with a leftover, under one whole worker on
// the standard cluster, and checks what the worker does against the rules of
// the per-object loop (RW4 to RW8, RW10, RW13), of the health bookkeeping
// (HL1 to HL4, HL7) and of the reactions (AR2, AR5, AR7, RW14).
//
// The loop is one piece of code for the four object kinds (RW1, RW9), so a
// case whose rule is the same for every kind drives dn 3, which holds nothing
// of the pool: no hold and no reaction depends on what it does. A case whose
// rule differs by kind drives each kind it differs for.

// roundObj is one object of the standard cluster a scenario case drives: its
// kind as the health records name it, its id, the fake that answers for it
// and its object key there.
type roundObj struct {
	c     *stdCluster
	kind  string
	id    uint64
	agent *agentfake.Agent
	key   string
}

func roundDn(c *stdCluster, id uint64) roundObj {
	return roundObj{c, healthRecordDn, id, c.dnAgent(id), agentfake.DnKey}
}

func roundCn(c *stdCluster, id uint64) roundObj {
	return roundObj{c, healthRecordCn, id, c.cnAgent(id), agentfake.CnKey}
}

func roundSide(t *testing.T, c *stdCluster, sideId uint64) roundObj {
	t.Helper()
	agent, key := c.sideAgent(t, sideId)
	return roundObj{c, healthRecordSide, sideId, agent, key}
}

func roundCntlr(t *testing.T, c *stdCluster, cntlrId uint64) roundObj {
	t.Helper()
	agent, key := c.cntlrAgent(t, cntlrId)
	return roundObj{c, healthRecordCntlr, cntlrId, agent, key}
}

func (o roundObj) String() string { return fmt.Sprintf("%s %d", o.kind, o.id) }

// set edits the object's levers on its fake.
func (o roundObj) set(t *testing.T, fn func(*agentfake.Object)) {
	t.Helper()
	if err := o.agent.Update(o.key, fn); err != nil {
		t.Fatalf("levers of %s: %v", o, err)
	}
}

// record is what the object's fake received about it.
func (o roundObj) record() agentfake.Record { return o.agent.Record(o.key) }

// about reports whether a record of the worker is about o: a health changed
// record by the record kind and the id it names, any other record of the loop
// by the ids the loop logs (a side's and a cntlr's name their node too,
// beside their pointer, so a node's record is one without a pointer).
func (o roundObj) about(rec logRecord) bool {
	if rec.msg == msgHealthChanged {
		if rec.str("record") != o.kind {
			return false
		}
		switch o.kind {
		case healthRecordDn:
			return rec.num("dn_id") == o.id
		case healthRecordCn:
			return rec.num("cn_id") == o.id
		case healthRecordSide:
			return rec.num("side_id") == o.id
		case healthRecordCntlr:
			return rec.num("cntlr_id") == o.id
		}
		return false
	}
	_, side := rec.attrs["side_pointer"]
	_, cntlr := rec.attrs["cntlr_pointer"]
	switch o.kind {
	case healthRecordDn:
		return !side && rec.num("dn_id") == o.id
	case healthRecordCn:
		return !cntlr && rec.num("cn_id") == o.id
	case healthRecordSide:
		id, ok := sideIdOf(rec)
		return ok && id == o.id
	case healthRecordCntlr:
		id, ok := roundCntlrIdOf(rec)
		return ok && id == o.id
	}
	return false
}

// roundCntlrIdOf is the cntlr_id of a cntlr child's record, read from its
// cntlr_pointer, as sideIdOf reads a side's.
func roundCntlrIdOf(rec logRecord) (uint64, bool) {
	v, ok := rec.attrs["cntlr_pointer"]
	if !ok || v.Kind() != slog.KindAny {
		return 0, false
	}
	ptr, ok := v.Any().(map[string]any)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseUint(fmt.Sprint(ptr["cntlr_id"]), 10, 64)
	return id, err == nil
}

// epoch reads the object's health epoch from its record.
func (o roundObj) epoch(t *testing.T) uint64 {
	t.Helper()
	switch o.kind {
	case healthRecordDn:
		return o.c.dnConf(t, o.id).GetErrEpoch()
	case healthRecordCn:
		return o.c.cnConf(t, o.id).GetErrEpoch()
	case healthRecordCntlr:
		return o.c.cntlr(t, o.id).GetErrEpoch()
	case healthRecordSide:
		for _, slice := range o.c.load(t).Slices {
			grps := append(slices.Clone(slice.GetMetaGrpList()), slice.GetDataGrpList()...)
			for _, grp := range grps {
				legs := append(slices.Clone(grp.GetLegList()), grp.GetSpareLegList()...)
				for _, leg := range legs {
					for _, side := range leg.GetSideList() {
						if side.GetSideId() == o.id {
							return side.GetErrEpoch()
						}
					}
				}
			}
		}
	}
	t.Fatalf("no record of %s", o)
	return 0
}

// roundTraceIds are the trace ids of the check rounds o's fake was asked:
// every round puts its own id in its request (RW4 step 2, RW10).
func (o roundObj) roundTraceIds() map[string]bool {
	ids := make(map[string]bool)
	for _, msg := range o.record().Rounds {
		if req, ok := msg.(interface{ GetTraceId() string }); ok {
			ids[req.GetTraceId()] = true
		}
	}
	return ids
}

// waitRounds waits until o's fake has been asked n more check rounds than it
// had when called. A fake records a round as it arrives, a held one too.
func (o roundObj) waitRounds(t *testing.T, h *workerHarness, n int) {
	t.Helper()
	from := len(o.record().Rounds)
	h.waitUntil(t, time.Duration(n)*4*time.Second+10*time.Second,
		fmt.Sprintf("%d more rounds of %s", n, o), func() (bool, string) {
			got := len(o.record().Rounds) - from
			return got >= n, fmt.Sprintf("%d of %d", got, n)
		})
}

// noSyncupFor waits until o's fake has been asked n more check rounds and
// fails the case if o got a syncup meanwhile: n rounds whose replies must
// issue none (RW4 step 5). A window of two rounds or more means it: a round
// that asked for a syncup sends it before the next round starts (RW1).
func (o roundObj) noSyncupFor(t *testing.T, h *workerHarness, n int, why string) {
	t.Helper()
	before := len(o.record().Syncups)
	o.waitRounds(t, h, n)
	if got := len(o.record().Syncups); got != before {
		t.Fatalf("%s got %d syncups in %d rounds, want none: %s",
			o, got-before, n, why)
	}
}

// roundRecords are w's records with msg about o, logged after since, in
// arrival order.
func (h *workerHarness) roundRecords(
	w *harnessWorker,
	msg string,
	o roundObj,
	since time.Time,
) []logRecord {
	var out []logRecord
	for _, rec := range h.records(w, msg) {
		if rec.at.After(since) && o.about(rec) {
			out = append(out, rec)
		}
	}
	return out
}

// waitRoundRecord waits for the first record of w with msg about o, logged
// after since, that match accepts (nil: any).
func (h *workerHarness) waitRoundRecord(
	t *testing.T,
	timeout time.Duration,
	w *harnessWorker,
	msg string,
	o roundObj,
	since time.Time,
	match func(logRecord) bool,
) logRecord {
	t.Helper()
	return h.waitRecord(t, timeout, w, msg, func(rec logRecord) bool {
		return rec.at.After(since) && o.about(rec) && (match == nil || match(rec))
	})
}

// roundSet and roundClear tell the two kinds of health changed record apart.
func roundSet(rec logRecord) bool   { return rec.num("err_epoch") != 0 }
func roundClear(rec logRecord) bool { return rec.num("err_epoch") == 0 }

// roundsNoise are the records a failed case's dump leaves out: the
// membership, ownership and lifecycle records, which fill the harness's own
// dump of its last records once the workers stop.
var roundsNoise = map[string]bool{
	msgShardOwned:            true,
	msgShardReleased:         true,
	msgMembershipObserved:    true,
	msgMembershipCommitted:   true,
	msgWorkerRegistered:      true,
	msgRevisionWorkerStarted: true,
	msgRevisionWorkerStopped: true,
}

// roundsDumpCnt is how many records a failed case's dump logs.
const roundsDumpCnt = 250

// roundsHarness starts a harness whose failed case logs, before the workers
// stop, its last records of rounds, syncups, health and reactions.
func roundsHarness(t *testing.T) *workerHarness {
	t.Helper()
	h := newWorkerHarness(t, harnessOpts{})
	// Registered after the harness's cleanup, so it runs first, while the
	// workers still run.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		var recs []logRecord
		for _, rec := range h.records(nil, "") {
			if !roundsNoise[rec.msg] {
				recs = append(recs, rec)
			}
		}
		if len(recs) > roundsDumpCnt {
			recs = recs[len(recs)-roundsDumpCnt:]
		}
		t.Logf("the last %d records of rounds, syncups, health and reactions:",
			len(recs))
		for _, rec := range recs {
			keys := slices.Sorted(maps.Keys(rec.attrs))
			var attrs strings.Builder
			for _, key := range keys {
				fmt.Fprintf(&attrs, " %s=%s", key, rec.attrs[key])
			}
			t.Logf("  %s %s %q trace=%s%s", rec.at.Format("15:04:05.000"),
				rec.workerName(), rec.msg, rec.traceId, attrs.String())
		}
	})
	return h
}

// roundsCase starts a harness with the standard cluster, shaped by edit, and
// one whole worker on every role that owns every shard, and waits for the
// cluster to converge.
func roundsCase(
	t *testing.T,
	edit func(*stdSpec),
) (*workerHarness, *stdCluster, *harnessWorker) {
	t.Helper()
	h := roundsHarness(t)
	c := h.plantStd(t, edit)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	return h, c, w
}

// roundsLongCntlrThreshold sets the pool's cntlr threshold to ten minutes,
// so that a standby a case keeps unhealthy for seconds is not replaced in the
// middle of a case about something else (AR7).
func roundsLongCntlrThreshold(s *stdSpec) {
	th := proto.Clone(s.threshold).(*pb.EventThreshold)
	th.CntlrUnhealthy = 600
	s.threshold = th
}

// dnCapacityKeyed and cnCapacityKeyed report whether a node's capacity key,
// the record that makes it a candidate for allocation, is in etcd: a node
// whose health epoch is set has none (MD4).
func (c *stdCluster) dnCapacityKeyed(t *testing.T, id uint64) bool {
	t.Helper()
	conf := c.dnConf(t, id)
	bin, ok := model.DnBinIdx(conf.GetFreeExtCnt(), c.cl.Conf.GetDnBinConf())
	if !ok {
		t.Fatalf("dn %d's %d free extents fall in no bin", id, conf.GetFreeExtCnt())
	}
	found, err := c.h.cli.Get(context.Background(),
		model.DnCapacityKey(c.cl.Id, bin, conf.GetFreeExtCnt(), stdDnAddr(id)),
		&pb.DnCapacity{})
	if err != nil {
		t.Fatalf("dn %d capacity key: %v", id, err)
	}
	return found
}

func (c *stdCluster) cnCapacityKeyed(t *testing.T, id uint64) bool {
	t.Helper()
	conf := c.cnConf(t, id)
	found, err := c.h.cli.Get(context.Background(),
		model.CnCapacityKey(c.cl.Id, conf.GetFreeExtCnt(), stdCnAddr(id)),
		&pb.CnCapacity{})
	if err != nil {
		t.Fatalf("cn %d capacity key: %v", id, err)
	}
	return found
}

// cdcLists reports whether the subsystem's discovery record lists the cntlr.
func (c *stdCluster) cdcLists(t *testing.T, cntlrId uint64) bool {
	t.Helper()
	want := c.cntlr(t, cntlrId).GetNvmeTrConf()
	for _, tr := range c.cdcEntry(t).GetNvmeTrConfList() {
		if proto.Equal(tr, want) {
			return true
		}
	}
	return false
}

// setDnRevision writes a revision into dn id's revision key as it stands, as
// an etcd restore that hands the key back an older value does
// (architecture.md, etcd backup and restore).
func (c *stdCluster) setDnRevision(t *testing.T, id uint64, revision uint64) {
	t.Helper()
	key := model.DnRevKey(stdDnShard(id), c.cl.Id, id)
	rev := &pb.DnRev{}
	c.mustGet(t, key, rev)
	rev.Revision = revision
	if err := c.h.cli.Put(context.Background(), key, rev); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// bumpDn raises dn id's revision by one, as a change of its desired state
// does, and returns the new revision.
func (c *stdCluster) bumpDn(t *testing.T, id uint64) uint64 {
	t.Helper()
	rev, err := plant.BumpRev(context.Background(), c.h.cli, c.cl,
		plant.RevDn, stdDnShard(id), id)
	if err != nil {
		t.Fatalf("bump dn %d: %v", id, err)
	}
	return rev
}

// roundSilence sets, or clears, the levers that keep every object of a node
// from answering: the node's own and those of its sides or cntlrs, each with
// its check rounds and its syncups held (Hang, HangSyncup), as a node that
// keeps its connections and never answers. A held syncup ends at the syncup
// timeout of deps (RW5).
func roundSilence(t *testing.T, on bool, objs ...roundObj) {
	t.Helper()
	for _, o := range objs {
		o.set(t, func(ob *agentfake.Object) {
			ob.Hang = on
			ob.HangSyncup = on
		})
	}
}

// roundDnObjs and roundCnObjs are a node of the standard cluster with every
// object its fake answers for: dn id with the sides it holds, cn id with the
// cntlrs it holds.
func roundDnObjs(t *testing.T, c *stdCluster, id uint64) []roundObj {
	t.Helper()
	objs := []roundObj{roundDn(c, id)}
	for _, side := range stdSides {
		if side.dnId == id {
			objs = append(objs, roundSide(t, c, side.sideId))
		}
	}
	return objs
}

func roundCnObjs(t *testing.T, c *stdCluster, id uint64) []roundObj {
	t.Helper()
	objs := []roundObj{roundCn(c, id)}
	for _, cntlrId := range c.load(t).Conf.GetCntlrIdList() {
		if c.cntlr(t, cntlrId).GetAddrPort() == stdCnAddr(id) {
			objs = append(objs, roundCntlr(t, c, cntlrId))
		}
	}
	return objs
}

// ---------------------------------------------------------------------------
// Check rounds
// ---------------------------------------------------------------------------

// TestWholeWorkerRoundLateReplyInTimeSetsNoEpoch pins RW8: a check round
// waits one interval for its reply, and a reply that comes later than usual
// but inside that wait is judged as any other. The primary's fake answers
// every round a third of an interval late: no round fails, no health epoch is
// set, no syncup is sent and the primary is not failed over. An agent silent
// for a whole round misses it, which sets the epoch (HL1, HL2); that case
// follows. The primary is the object here because its health is what a late
// agent would cost most: a failover.
func TestWholeWorkerRoundLateReplyInTimeSetsNoEpoch(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundCntlr(t, c, 1)
	since := time.Now()
	syncups := len(o.record().Syncups)
	delay := harnessInterval * time.Second / 3
	o.set(t, func(ob *agentfake.Object) { ob.DelayRounds = delay })
	// Four rounds asked, so three of them were answered late: a round is
	// asked only after the reply to the one before (RW1, RW8).
	o.waitRounds(t, h, 4)
	o.set(t, func(ob *agentfake.Object) { ob.DelayRounds = 0 })

	if recs := h.roundRecords(w, "check round failed", o, since); len(recs) != 0 {
		t.Errorf("%d rounds of %s failed although each reply came %v into "+
			"its %ds wait; the first: %s", len(recs), o, delay,
			harnessInterval, recs[0].str("error"))
	}
	if recs := h.roundRecords(w, msgHealthChanged, o, since); len(recs) != 0 {
		t.Errorf("%s's health changed %d times: err_epoch %d", o, len(recs),
			recs[0].num("err_epoch"))
	}
	if e := o.epoch(t); e != 0 {
		t.Errorf("%s has err_epoch %d, want none", o, e)
	}
	if got := len(o.record().Syncups); got != syncups {
		t.Errorf("%s got %d syncups, want none", o, got-syncups)
	}
	if got := c.primary(t); got != 1 {
		t.Errorf("the primary is cntlr %d, want cntlr 1 still", got)
	}
}

// TestWholeWorkerRoundMissedRoundsSetTheEpochOnce pins HL1, HL2 and HL3 on
// each object kind: a round with no reply in time sets the object's health
// epoch at once, the rounds it misses after that write nothing more, so the
// threshold clock never restarts, and the first round answered in time clears
// it. What the write does beside the epoch differs by kind, and is checked
// with it: a disk node's or a controller node's write removes the node's
// capacity key while the epoch is set and puts it back once cleared (MD4); a
// standby cntlr's takes it out of the subsystem's discovery record and lists
// it again (HL2, architecture.md [D18]); a side's writes the epoch alone.
// Each fake holds its object's next three rounds.
func TestWholeWorkerRoundMissedRoundsSetTheEpochOnce(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, roundsLongCntlrThreshold)
	objs := []roundObj{
		roundDn(c, 3), roundCn(c, 3), roundSide(t, c, 8), roundCntlr(t, c, 2),
	}
	// effect is the kind's write beside the epoch, as it reads with the epoch
	// set (true) or clear.
	effect := func(o roundObj, set bool) (bool, string) {
		switch o.kind {
		case healthRecordDn:
			keyed := c.dnCapacityKeyed(t, o.id)
			return keyed != set, fmt.Sprintf("capacity key there %t", keyed)
		case healthRecordCn:
			keyed := c.cnCapacityKeyed(t, o.id)
			return keyed != set, fmt.Sprintf("capacity key there %t", keyed)
		case healthRecordCntlr:
			listed := c.cdcLists(t, o.id)
			return listed != set, fmt.Sprintf("discovery record lists it %t", listed)
		}
		return true, ""
	}
	for _, o := range objs {
		if ok, why := effect(o, false); !ok {
			t.Fatalf("%s before the case: %s", o, why)
		}
	}
	const held = 3
	since := time.Now()
	for _, o := range objs {
		o.set(t, func(ob *agentfake.Object) { ob.HangRounds = held })
	}
	setEpoch := make(map[string]uint64)
	h.waitUntil(t, 30*time.Second, "every object's epoch set, with its kind's write",
		func() (bool, string) {
			for _, o := range objs {
				if _, seen := setEpoch[o.String()]; seen {
					continue
				}
				if e := o.epoch(t); e != 0 {
					if ok, _ := effect(o, true); ok {
						setEpoch[o.String()] = e
					}
				}
			}
			for _, o := range objs {
				if _, seen := setEpoch[o.String()]; !seen {
					_, why := effect(o, true)
					return false, fmt.Sprintf("%s: err_epoch %d, %s", o, o.epoch(t), why)
				}
			}
			return true, ""
		})
	cleared := make(map[string]bool)
	h.waitUntil(t, 30*time.Second, "every object's epoch cleared, with its kind's write",
		func() (bool, string) {
			for _, o := range objs {
				if !cleared[o.String()] && o.epoch(t) == 0 {
					if ok, _ := effect(o, false); ok {
						cleared[o.String()] = true
					}
				}
			}
			for _, o := range objs {
				if !cleared[o.String()] {
					_, why := effect(o, false)
					return false, fmt.Sprintf("%s: err_epoch %d, %s", o, o.epoch(t), why)
				}
			}
			return true, ""
		})
	// Two rounds more, answered in time: the objects stay healthy.
	for _, o := range objs {
		o.waitRounds(t, h, 2)
	}

	for _, o := range objs {
		recs := h.roundRecords(w, msgHealthChanged, o, since)
		if len(recs) != 2 || !roundSet(recs[0]) || !roundClear(recs[1]) {
			var got []string
			for _, rec := range recs {
				got = append(got, fmt.Sprintf("err_epoch %d (%s)",
					rec.num("err_epoch"), rec.str("reason")))
			}
			t.Errorf("%s's health changed %v, want one set, then one clear", o, got)
			continue
		}
		if r := recs[0].str("reason"); r != reasonUnreachable {
			t.Errorf("%s's epoch was set for %q, want %q", o, r, reasonUnreachable)
		}
		if got, want := recs[0].num("err_epoch"), setEpoch[o.String()]; got != want {
			t.Errorf("%s's record held err_epoch %d while set, the write %d", o, want, got)
		}
		missed := 0
		for _, rec := range h.roundRecords(w, "check round failed", o, since) {
			if rec.at.Before(recs[1].at) && strings.Contains(rec.str("error"),
				errRoundTimeout.Error()) {
				missed++
			}
		}
		if missed < held {
			t.Errorf("%s: %d rounds timed out before the clear, want the %d "+
				"its fake held", o, missed, held)
		}
		if !o.roundTraceIds()[recs[1].traceId] {
			t.Errorf("%s's epoch was cleared under trace id %s, no round's",
				o, recs[1].traceId)
		}
	}
}

// TestWholeWorkerRoundAgentDownWhenTheStreamOpens pins RW4 step 1: a round
// that cannot open its stream counts as a broken stream, so the agent is
// unreachable and its health epoch is set (HL1); with no reply, no syncup is
// sent (RW4 step 5). dn 3's agent is down from the worker's start, so its
// stream is never opened and the monitor's first verdict is that one, written
// whatever the record holds (HL3). Once the agent is back, the next round
// opens the stream, its reply at no revision brings the first syncup, and the
// clean round clears the epoch.
func TestWholeWorkerRoundAgentDownWhenTheStreamOpens(t *testing.T) {
	t.Parallel()
	h := roundsHarness(t)
	c := h.plantStd(t, nil)
	o := roundDn(c, 3)
	o.agent.Stop()
	since := time.Now()
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)

	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, since, roundSet)
	if r := set.str("reason"); r != reasonUnreachable {
		t.Errorf("dn 3's epoch was set for %q, want %q", r, reasonUnreachable)
	}
	opened := false
	for _, rec := range h.roundRecords(w, "check stream open failed", o, since) {
		if rec.traceId == set.traceId {
			opened = true
		}
	}
	if !opened {
		t.Errorf("dn 3's epoch was set under trace id %s, no round whose "+
			"stream failed to open", set.traceId)
	}
	// Two rounds more of a stream that does not open; the round records
	// tell them, since a stopped fake records nothing.
	h.waitUntil(t, 20*time.Second, "two more rounds of dn 3 with no stream",
		func() (bool, string) {
			n := len(h.roundRecords(w, "check stream open failed", o, set.at))
			return n >= 2, fmt.Sprintf("%d", n)
		})
	if recs := h.roundRecords(w, msgSyncupResult, o, since); len(recs) != 0 {
		t.Fatalf("%d syncups of dn 3 while no stream opened, want none", len(recs))
	}
	if more := h.roundRecords(w, msgHealthChanged, o, set.at); len(more) != 0 {
		t.Errorf("dn 3's health changed again while down: err_epoch %d",
			more[0].num("err_epoch"))
	}

	if err := o.agent.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("start dn 3: %v", err)
	}
	clear := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, set.at, roundClear)
	if !o.roundTraceIds()[clear.traceId] {
		t.Errorf("dn 3's epoch was cleared under trace id %s, no round's", clear.traceId)
	}
	h.waitUntil(t, 20*time.Second, "dn 3's first syncup", func() (bool, string) {
		syncups := o.record().Syncups
		if len(syncups) == 0 {
			return false, "none yet"
		}
		rev := syncups[0].(*pb.SyncupDnRequest).GetRevision()
		return rev == c.dnRev(t, 3), fmt.Sprintf("at revision %d", rev)
	})
}

// TestWholeWorkerRoundAgentEndsTheStreamWithAnError pins RW4 steps 3 and 4: a
// stream the agent ends with an error is a broken stream. The round fails
// with the stream's error, its stream is closed, the next round opens a fresh
// one, the agent is unreachable and its health epoch set (HL1), and no syncup
// is sent while no reply comes. cn 3's fake ends every stream as its first
// round arrives; once it stops, the next round is clean and clears the epoch,
// and still sends no syncup, since its fake holds the revision the worker
// wants. The rule is one for every kind; a controller node is driven here.
func TestWholeWorkerRoundAgentEndsTheStreamWithAnError(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundCn(c, 3)
	since := time.Now()
	streams := o.record().Streams
	syncups := len(o.record().Syncups)
	o.set(t, func(ob *agentfake.Object) { ob.DropStream = true })

	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, since, roundSet)
	if r := set.str("reason"); r != reasonUnreachable {
		t.Errorf("cn 3's epoch was set for %q, want %q", r, reasonUnreachable)
	}
	dropped := func() int {
		n := 0
		for _, rec := range h.roundRecords(w, "check round failed", o, since) {
			if strings.Contains(rec.str("error"), "drop_stream") {
				n++
			}
		}
		return n
	}
	h.waitUntil(t, 20*time.Second, "three rounds of cn 3 failed on its stream's error",
		func() (bool, string) {
			n := dropped()
			return n >= 3, fmt.Sprintf("%d", n)
		})
	// The first failed round may be on the stream opened before the lever;
	// each one after it is on a stream of its own. Streams only grow, so
	// they are read after the failures.
	failures := dropped()
	rec := o.record()
	if opened := rec.Streams - streams; opened < failures-1 {
		t.Errorf("cn 3's fake was opened %d streams for %d failed rounds, "+
			"want a fresh one for each round after the first", opened, failures)
	}
	if got := len(rec.Syncups); got != syncups {
		t.Errorf("cn 3 got %d syncups while no reply came, want none", got-syncups)
	}
	if more := h.roundRecords(w, msgHealthChanged, o, set.at); len(more) != 0 {
		t.Errorf("cn 3's health changed again while its streams broke: "+
			"err_epoch %d", more[0].num("err_epoch"))
	}

	o.set(t, func(ob *agentfake.Object) { ob.DropStream = false })
	h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, set.at, roundClear)
	o.noSyncupFor(t, h, 2, "cn 3 holds the revision the worker wants")
	if got := len(o.record().Syncups); got != syncups {
		t.Errorf("cn 3 got %d syncups, want none", got-syncups)
	}
}

// TestWholeWorkerRoundUnknownReplyCodeIsARefusal pins RW5 and HL1 on a reply
// code the worker does not know: it is a refusal, logged at Info, which
// neither sets nor clears the health epoch and brings the syncup again on
// every round, each refused alike. dn 3's fake misses one round, which sets
// the epoch, and then answers every reply with code 77: the epoch stays as
// that round set it, and only the first round after the code is gone clears
// it.
func TestWholeWorkerRoundUnknownReplyCodeIsARefusal(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundDn(c, 3)
	const code = 77
	since := time.Now()
	o.set(t, func(ob *agentfake.Object) {
		ob.HangRounds = 1
		ob.ReplyCode = code
	})
	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, since, roundSet)
	h.waitUntil(t, 30*time.Second, "three refused syncups of dn 3", func() (bool, string) {
		n := len(h.roundRecords(w, msgSyncupRejected, o, set.at))
		return n >= 3, fmt.Sprintf("%d", n)
	})
	o.set(t, func(ob *agentfake.Object) { ob.ReplyCode = 0 })
	liftedAt := time.Now()

	for _, rec := range h.roundRecords(w, msgSyncupRejected, o, since) {
		if rec.at.After(liftedAt) {
			continue
		}
		if rec.num("code") != code || rec.level != slog.LevelInfo {
			t.Errorf("dn 3's syncup refused with code %d at %v, want code %d at Info",
				rec.num("code"), rec.level, code)
		}
	}
	clear := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, set.at, nil)
	if !roundClear(clear) {
		t.Fatalf("dn 3's health changed to err_epoch %d after its epoch was set, "+
			"want only the clear", clear.num("err_epoch"))
	}
	if clear.at.Before(liftedAt) {
		t.Errorf("dn 3's epoch was cleared %v before the refusals stopped",
			liftedAt.Sub(clear.at).Round(time.Millisecond))
	}
	o.noSyncupFor(t, h, 2, "dn 3 answers code zero at the revision the worker wants")
}

// TestWholeWorkerRoundTooOldObservationIsAMissedRound pins HL7 and RW4 step
// 4: a round whose observation is older than ObsMaxAgeIntervals of the
// interval is a missed round in every way, which sets the health epoch and
// sends no syncup whatever the reply's revision. dn 3's fake first answers
// every round from its last observation, which ages until the rounds fail
// on it. It then restarts on empty files and answers from an observation an
// hour old at no revision: still no syncup, round after round. Once its
// observations are fresh again, the next round brings the syncup and clears
// the epoch.
func TestWholeWorkerRoundTooOldObservationIsAMissedRound(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundDn(c, 3)
	since := time.Now()
	syncups := len(o.record().Syncups)
	o.set(t, func(ob *agentfake.Object) { ob.ObsAge = time.Hour })
	stale := func(rec logRecord) bool {
		return rec.str("error") == errStaleObservation.Error()
	}
	first := h.waitRoundRecord(t, 30*time.Second, w, "check round failed", o, since, stale)
	set := h.waitRoundRecord(t, 10*time.Second, w, msgHealthChanged, o, since, roundSet)
	if set.traceId != first.traceId || set.str("reason") != reasonUnreachable {
		t.Errorf("dn 3's epoch was set for %q under trace id %s, want %q under "+
			"the stale round's %s", set.str("reason"), set.traceId,
			reasonUnreachable, first.traceId)
	}
	late := false
	for _, rec := range h.roundRecords(w, msgLateObservation, o, since) {
		if rec.traceId == first.traceId && rec.str("stale") == "true" {
			late = true
		}
	}
	if !late {
		t.Errorf("no stale late observation record of dn 3's first stale round")
	}

	// An agent restarted on empty files holds no revision, so a fresh
	// observation of it would bring a syncup at once.
	o.agent.Stop()
	if err := o.agent.Start(agentfake.DropStore); err != nil {
		t.Fatalf("start dn 3: %v", err)
	}
	restartedAt := time.Now()
	h.waitUntil(t, 30*time.Second, "three stale rounds of dn 3 after its restart",
		func() (bool, string) {
			n := 0
			for _, rec := range h.roundRecords(w, "check round failed", o, restartedAt) {
				if stale(rec) {
					n++
				}
			}
			return n >= 3, fmt.Sprintf("%d", n)
		})
	if got := len(o.record().Syncups); got != syncups {
		t.Fatalf("dn 3 got %d syncups while its observations were too old, "+
			"want none whatever their revision", got-syncups)
	}
	if more := h.roundRecords(w, msgHealthChanged, o, set.at); len(more) != 0 {
		t.Errorf("dn 3's health changed again while its observations were "+
			"too old: err_epoch %d", more[0].num("err_epoch"))
	}

	o.set(t, func(ob *agentfake.Object) { ob.ObsAge = 0 })
	h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, set.at, roundClear)
	h.waitUntil(t, 20*time.Second, "dn 3's syncup once its observations are fresh",
		func() (bool, string) {
			got := o.record().Syncups[syncups:]
			if len(got) == 0 {
				return false, "none yet"
			}
			rev := got[0].(*pb.SyncupDnRequest).GetRevision()
			return rev == c.dnRev(t, 3), fmt.Sprintf("at revision %d", rev)
		})
}

// TestWholeWorkerRoundObservationWithNoTime pins HL7 for a reply that carries
// no observation time: its age counts from the first round after a pause
// alone. dn 3's fake answers every round with no observation from the
// worker's start, at no revision. The first rounds after the loop's start
// give no verdict and each brings the syncup; once the first round lies
// ObsMaxAgeIntervals of the interval behind, every round is a missed round:
// it is logged late with no observation, sets the epoch and sends no syncup.
// Once the fake answers with observations again, the next round clears the
// epoch.
func TestWholeWorkerRoundObservationWithNoTime(t *testing.T) {
	t.Parallel()
	h := roundsHarness(t)
	c := h.plantStd(t, nil)
	o := roundDn(c, 3)
	o.set(t, func(ob *agentfake.Object) { ob.ObsNone = true })
	since := time.Now()
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)

	stale := func(rec logRecord) bool {
		return rec.str("error") == errStaleObservation.Error()
	}
	first := h.waitRoundRecord(t, 30*time.Second, w, "check round failed", o, since, stale)
	before := 0
	for _, rec := range h.roundRecords(w, msgSyncupResult, o, since) {
		if rec.at.Before(first.at) {
			before++
		}
	}
	if before < 2 {
		t.Errorf("%d syncups of dn 3 before its first stale round, want one "+
			"from each round of the first %d intervals", before,
			common.ObsMaxAgeIntervals)
	}
	for _, rec := range h.roundRecords(w, msgHealthChanged, o, since) {
		if rec.at.Before(first.at) && roundSet(rec) {
			t.Errorf("dn 3's epoch was set for %q before its first stale "+
				"round: a round with no observation gives no verdict until then",
				rec.str("reason"))
		}
	}
	late := false
	for _, rec := range h.roundRecords(w, msgLateObservation, o, since) {
		if rec.traceId == first.traceId && rec.str("stale") == "true" &&
			rec.str("no_obs") == "true" {
			late = true
		}
	}
	if !late {
		t.Errorf("no stale late observation record with no_obs of dn 3's " +
			"first stale round")
	}
	h.waitRoundRecord(t, 10*time.Second, w, msgHealthChanged, o, since, roundSet)
	o.noSyncupFor(t, h, 2, "every round of dn 3 is stale")

	settled := time.Now()
	o.set(t, func(ob *agentfake.Object) { ob.ObsNone = false })
	h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, settled, roundClear)
}

// ---------------------------------------------------------------------------
// Syncups
// ---------------------------------------------------------------------------

// TestWholeWorkerSyncupHungGivesNoVerdict pins HL4 and RW5: a syncup that its
// agent never answers fails at the syncup timeout of deps with no health
// judgment of its own, and the next round gives one. dn 3's fake holds its
// syncups. Once the syncup of a revision change has reached it, so that the
// loop is inside the call and runs no round, the fake's rows turn to error:
// nothing sets the epoch until the call has failed, and then the next round
// sets it on those rows.
func TestWholeWorkerSyncupHungGivesNoVerdict(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundDn(c, 3)
	o.set(t, func(ob *agentfake.Object) { ob.HangSyncup = true })
	t.Cleanup(func() {
		_ = o.agent.Update(o.key, func(ob *agentfake.Object) {
			ob.HangSyncup = false
			ob.Status = nil
		})
	})
	since := time.Now()
	syncups := len(o.record().Syncups)
	rev := c.bumpDn(t, 3)
	h.waitUntil(t, 10*time.Second, "dn 3's syncup of the change", func() (bool, string) {
		return len(o.record().Syncups) > syncups, "none yet"
	})
	heldAt := time.Now()
	errStatus := pb.ResStatus_RES_STATUS_ERROR
	o.set(t, func(ob *agentfake.Object) { ob.Status = &errStatus })

	failed := h.waitRoundRecord(t, 3*h.tmo.syncup+10*time.Second, w, msgSyncupResult,
		o, since, func(rec logRecord) bool {
			return rec.num("revision") == rev && rec.str("error") != ""
		})
	if took := failed.at.Sub(heldAt); took < h.tmo.syncup-time.Second {
		t.Errorf("the hung syncup failed %v after it reached the fake, want "+
			"about the syncup timeout %v", took.Round(time.Millisecond), h.tmo.syncup)
	}
	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, since, nil)
	if !roundSet(set) || set.str("reason") != reasonErrorRow {
		t.Errorf("dn 3's first health change after the syncup: err_epoch %d "+
			"for %q, want a set for %q", set.num("err_epoch"), set.str("reason"),
			reasonErrorRow)
	}
	if set.traceId == failed.traceId || set.at.Before(failed.at) {
		t.Errorf("dn 3's health changed under the hung syncup's trace id or "+
			"before it failed (%v before)", failed.at.Sub(set.at).Round(time.Millisecond))
	}
	if !o.roundTraceIds()[set.traceId] {
		t.Errorf("dn 3's epoch was set under trace id %s, no round's", set.traceId)
	}
}

// TestWholeWorkerSyncupRefusalsAreResentByRounds pins RW5's refusals: a
// syncup refused as stale, as unknown or as invalid is logged as refused, at
// Error for a stale revision and at Info otherwise, nothing is armed, and the
// re-send comes from the next round only, whose own reply repeats the
// refusal's cause (RW4 step 5, RW2). Each refusal is played on a kind whose
// syncup can return it: stale on dn 3, whose revision key goes back below
// what its agent holds, as an etcd restore does (architecture.md, etcd backup
// and restore); unknown on side 8 and invalid on cntlr 2, by the reply-code
// lever, which forces the code on the rounds too, as an agent that stores a
// refusal reports it on them. Every re-sent syncup carries the trace id of a
// round of its object, one per round (RW10). Once the cause is gone, no
// syncup is sent any more.
func TestWholeWorkerSyncupRefusalsAreResentByRounds(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, roundsLongCntlrThreshold)
	cases := []struct {
		o     roundObj
		code  uint64
		level slog.Level
	}{
		{roundDn(c, 3), common.ReplyCodeStaleRevision, slog.LevelError},
		{roundSide(t, c, 8), common.ReplyCodeUnknownObject, slog.LevelInfo},
		{roundCntlr(t, c, 2), common.ReplyCodeInvalidConf, slog.LevelInfo},
	}
	since := time.Now()
	held := c.dnRev(t, 3)
	c.setDnRevision(t, 3, held-1)
	cases[1].o.set(t, func(ob *agentfake.Object) {
		ob.ReplyCode = common.ReplyCodeUnknownObject
	})
	cases[2].o.set(t, func(ob *agentfake.Object) {
		ob.ReplyCode = common.ReplyCodeInvalidConf
	})
	h.waitUntil(t, 30*time.Second, "four refused syncups of each object",
		func() (bool, string) {
			for _, tc := range cases {
				if n := len(h.roundRecords(w, msgSyncupRejected, tc.o, since)); n < 4 {
					return false, fmt.Sprintf("%s: %d", tc.o, n)
				}
			}
			return true, ""
		})
	liftedAt := time.Now()
	c.setDnRevision(t, 3, held+1)
	for _, tc := range cases[1:] {
		tc.o.set(t, func(ob *agentfake.Object) { ob.ReplyCode = 0 })
	}

	for _, tc := range cases {
		rounds := tc.o.roundTraceIds()
		perTrace := make(map[string]int)
		fromRounds := 0
		var immediate []logRecord
		for _, rec := range h.roundRecords(w, msgSyncupRejected, tc.o, since) {
			if rec.at.After(liftedAt) {
				continue
			}
			if rec.num("code") != tc.code || rec.level != tc.level {
				t.Errorf("%s's syncup refused with code %d at %v, want code %d at %v",
					tc.o, rec.num("code"), rec.level, tc.code, tc.level)
			}
			perTrace[rec.traceId]++
			if rounds[rec.traceId] {
				fromRounds++
			} else {
				immediate = append(immediate, rec)
			}
		}
		for traceId, n := range perTrace {
			if n > 1 {
				t.Errorf("%s: %d refused syncups under one trace id %s, want "+
					"one per round", tc.o, n, traceId)
			}
		}
		// The dn's first syncup is the one its revision change sends at
		// once (RW6), under a trace id of its own; every other one is a
		// round's.
		wantImmediate := 0
		if tc.o.kind == healthRecordDn {
			wantImmediate = 1
		}
		if len(immediate) != wantImmediate {
			t.Errorf("%s: %d refused syncups under no round's trace id, want %d",
				tc.o, len(immediate), wantImmediate)
		}
		if fromRounds < 3 {
			t.Errorf("%s: %d refused syncups re-sent by rounds, want one per round",
				tc.o, fromRounds)
		}
	}
	// The dn's change of revision sends its syncup at once, accepted now.
	h.waitRoundRecord(t, 20*time.Second, w, msgSyncupResult, cases[0].o, liftedAt,
		func(rec logRecord) bool {
			return rec.num("revision") == held+1 && rec.num("code") == 0 &&
				rec.str("error") == ""
		})
	for _, tc := range cases {
		// One round lets a syncup that a round asked for before the lift
		// land; after it, nothing more.
		tc.o.waitRounds(t, h, 1)
		tc.o.noSyncupFor(t, h, 2, "its refusal is gone")
	}
}

// TestWholeWorkerSyncupLeftoverResentEveryRound pins RW4 step 5, RW5 and HL1
// on a leftover: a round whose reply carries ReplyCodeLeftover is judged as a
// clean one, so it clears a health epoch, and brings the syncup again, every
// round for as long as the code lasts; each such syncup is accepted, applied
// and logged as a leftover beside its result; once the agent reports no
// leftover, no syncup is sent any more. cn 3's fake misses one round, which
// sets the epoch, and then reports a leftover on its next three rounds. Cn
// teardowns are where a leftover is routine (RW5); the rule is one for every
// kind.
func TestWholeWorkerSyncupLeftoverResentEveryRound(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundCn(c, 3)
	const leftover = 3
	since := time.Now()
	o.set(t, func(ob *agentfake.Object) {
		ob.HangRounds = 1
		ob.LeftoverRounds = leftover
	})
	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, since, roundSet)
	clean := h.waitRoundRecord(t, 30*time.Second, w, msgSyncupResult, o, set.at,
		func(rec logRecord) bool { return rec.num("code") == 0 })
	o.noSyncupFor(t, h, 2, "cn 3 reports no leftover any more")

	results := h.roundRecords(w, msgSyncupResult, o, set.at)
	leftovers := h.roundRecords(w, msgSyncupLeftover, o, set.at)
	rounds := o.roundTraceIds()
	seen := make(map[string]bool)
	var codes []uint64
	for _, rec := range results {
		codes = append(codes, rec.num("code"))
		if !rounds[rec.traceId] || seen[rec.traceId] {
			t.Errorf("cn 3's syncup under trace id %s: want each re-sent by a "+
				"round of its own", rec.traceId)
		}
		seen[rec.traceId] = true
	}
	if len(results) != leftover || results[len(results)-1].traceId != clean.traceId {
		t.Errorf("cn 3's syncups after its missed round answered codes %v, want "+
			"one per round of its %d leftover rounds, the last clean", codes, leftover)
	}
	var withLeftover []string
	for _, rec := range results {
		if rec.num("code") == common.ReplyCodeLeftover {
			withLeftover = append(withLeftover, rec.traceId)
		}
	}
	var logged []string
	for _, rec := range leftovers {
		logged = append(logged, rec.traceId)
	}
	if len(withLeftover) < leftover-1 || strings.Join(withLeftover, ",") != strings.Join(logged, ",") {
		t.Errorf("cn 3's syncups answered a leftover under %v, the syncup "+
			"leftover records are under %v; want one record for each", withLeftover, logged)
	}
	clear := h.waitRoundRecord(t, 10*time.Second, w, msgHealthChanged, o, set.at, nil)
	if !roundClear(clear) || len(results) == 0 || clear.traceId != results[0].traceId {
		t.Errorf("cn 3's epoch was cleared under trace id %s, want the first "+
			"leftover round's, which re-sent the syncup under %s", clear.traceId,
			func() string {
				if len(results) == 0 {
					return "(none)"
				}
				return results[0].traceId
			}())
	}
}

// TestWholeWorkerSyncupAcceptedReplyEchoesAnotherRevision pins how an
// accepted syncup reply that carries another revision than its request's is
// read. The loop does not compare it with the request: dn 3's fake answers
// the syncup of a revision change with an unrelated revision, the syncup is
// accepted and logged as such, and no syncup follows, since the next rounds
// answer the revision the agent stored, the request's (RW4 step 5, RW5). A
// cntlr has a rule of its own (HL2): a clean reply of a cntlr its child
// drives as a standby, at a revision below the one the child drives, neither
// sets nor clears its health epoch. Cntlr 2's fake holds its rounds, so its
// epoch is set, and answers the syncup of a pool revision change with the
// revision before it: the epoch stays as the missed rounds set it, and the
// first round answered after the fake lets its rounds go clears it.
func TestWholeWorkerSyncupAcceptedReplyEchoesAnotherRevision(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, roundsLongCntlrThreshold)

	dn := roundDn(c, 3)
	echo := uint64(1000)
	dn.set(t, func(ob *agentfake.Object) { ob.SyncupRevision = &echo })
	since := time.Now()
	syncups := len(dn.record().Syncups)
	rev := c.bumpDn(t, 3)
	h.waitRoundRecord(t, 20*time.Second, w, msgSyncupResult, dn, since,
		func(rec logRecord) bool {
			return rec.num("revision") == rev && rec.num("code") == 0 &&
				rec.str("error") == ""
		})
	dn.noSyncupFor(t, h, 3, "dn 3's rounds answer the revision it stored")
	if got := len(dn.record().Syncups); got != syncups+1 {
		t.Errorf("dn 3 got %d syncups for one revision change, want 1", got-syncups)
	}
	if recs := h.roundRecords(w, msgSyncupRejected, dn, since); len(recs) != 0 {
		t.Errorf("dn 3's syncup echoing revision %d was refused: code %d", echo,
			recs[0].num("code"))
	}

	ct := roundCntlr(t, c, 2)
	since = time.Now()
	ct.set(t, func(ob *agentfake.Object) { ob.Hang = true })
	t.Cleanup(func() {
		_ = ct.agent.Update(ct.key, func(ob *agentfake.Object) { ob.Hang = false })
	})
	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, ct, since, roundSet)
	before := c.spRev(t)
	ct.set(t, func(ob *agentfake.Object) { ob.SyncupRevision = &before })
	spRev, err := plant.BumpRev(context.Background(), h.cli, c.cl, plant.RevSp,
		stdSpShard, stdSpId)
	if err != nil {
		t.Fatalf("bump the pool: %v", err)
	}
	h.waitRoundRecord(t, 20*time.Second, w, msgSyncupResult, ct, set.at,
		func(rec logRecord) bool {
			return rec.num("revision") == spRev && rec.num("code") == 0 &&
				rec.str("error") == ""
		})
	// Two rounds more, held: they keep the epoch set, and a clear by the
	// syncup's reply would have shown as a clear and a new set.
	ct.waitRounds(t, h, 2)
	if more := h.roundRecords(w, msgHealthChanged, ct, set.at); len(more) != 0 {
		t.Errorf("cntlr 2's health changed to err_epoch %d after its syncup "+
			"reply at revision %d, below the driven %d", more[0].num("err_epoch"),
			before, spRev)
	}
	if e := ct.epoch(t); e != set.num("err_epoch") {
		t.Errorf("cntlr 2 has err_epoch %d, the missed round set %d", e,
			set.num("err_epoch"))
	}

	ct.set(t, func(ob *agentfake.Object) { ob.Hang = false })
	clear := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, ct, set.at, nil)
	if !roundClear(clear) || !ct.roundTraceIds()[clear.traceId] {
		t.Errorf("cntlr 2's next health change: err_epoch %d under trace id %s, "+
			"want a clear by a round", clear.num("err_epoch"), clear.traceId)
	}
	h.waitUntil(t, 10*time.Second, "cntlr 2 back in the discovery record",
		func() (bool, string) { return c.cdcLists(t, 2), "not listed" })
}

// TestWholeWorkerSyncupAgentDownAtSyncupTime pins HL4 and RW5 on an agent that
// is down when a syncup is sent: the call fails and is logged as the
// syncup's result, it gives no health judgment of its own, and a round gives
// the "unreachable" verdict. dn 3's fake stops right after a round, and its
// revision changes at once, so the syncup of the change runs before the next
// round. Once the agent is back with its files, the next round finds it at
// the old revision, sends the syncup again and clears the epoch.
func TestWholeWorkerSyncupAgentDownAtSyncupTime(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	o := roundDn(c, 3)
	// The stop lands right after a round, whose reply the fake sends at once,
	// so the loop waits out its interval and the syncup of the change runs
	// before the next round, on a connection that has just gone down.
	o.waitRounds(t, h, 1)
	o.agent.Stop()
	since := time.Now()
	rev := c.bumpDn(t, 3)

	failed := h.waitRoundRecord(t, 20*time.Second, w, msgSyncupResult, o, since,
		func(rec logRecord) bool {
			return rec.num("revision") == rev && rec.str("error") != ""
		})
	set := h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, since, nil)
	if !roundSet(set) || set.str("reason") != reasonUnreachable {
		t.Errorf("dn 3's first health change after the stop: err_epoch %d for "+
			"%q, want a set for %q", set.num("err_epoch"), set.str("reason"),
			reasonUnreachable)
	}
	if set.traceId == failed.traceId {
		t.Errorf("dn 3's epoch was set under the failed syncup's trace id %s",
			failed.traceId)
	}

	if err := o.agent.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("start dn 3: %v", err)
	}
	resent := h.waitRoundRecord(t, 20*time.Second, w, msgSyncupResult, o, failed.at,
		func(rec logRecord) bool {
			return rec.num("revision") == rev && rec.num("code") == 0 &&
				rec.str("error") == ""
		})
	if !o.roundTraceIds()[resent.traceId] {
		t.Errorf("dn 3's syncup at revision %d after its restart came under "+
			"trace id %s, no round's", rev, resent.traceId)
	}
	h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, o, set.at, roundClear)
}

// ---------------------------------------------------------------------------
// Several agents
// ---------------------------------------------------------------------------

// TestWholeWorkerSilentLocation pins HL1, HL2 and AR5 on a whole location gone
// silent: dn 1 and cn 1 share rack 1, and every object their fakes answer for
// stops answering — dn 1 with sides 8 and 10, cn 1 with the primary cntlr 1.
// Every one of them gets its health epoch, no object elsewhere does, the
// primary fails over to the standby in rack 2 and the discovery record lists
// the new primary alone (architecture.md [D18]). Once rack 1 answers again,
// every epoch clears, the old primary takes its demotion, and the pool
// converges with both cntlrs listed.
func TestWholeWorkerSilentLocation(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, func(s *stdSpec) {
		s.sharedRacks = true
		roundsLongCntlrThreshold(s)
	})
	rack := append(roundDnObjs(t, c, 1), roundCnObjs(t, c, 1)...)
	var others []roundObj
	for id := uint64(2); id <= 3; id++ {
		others = append(others, roundDnObjs(t, c, id)...)
		others = append(others, roundCnObjs(t, c, id)...)
	}
	since := time.Now()
	roundSilence(t, true, rack...)
	t.Cleanup(func() {
		for _, o := range rack {
			_ = o.agent.Update(o.key, func(ob *agentfake.Object) {
				ob.Hang, ob.HangSyncup = false, false
			})
		}
	})

	h.waitUntil(t, 30*time.Second, "every object of rack 1 unhealthy, and the "+
		"failover", func() (bool, string) {
		for _, o := range rack {
			if o.epoch(t) == 0 {
				return false, fmt.Sprintf("%s: no err_epoch", o)
			}
		}
		if got := c.primary(t); got != 2 {
			return false, fmt.Sprintf("cntlr %d primary", got)
		}
		return true, ""
	})
	for _, o := range others {
		if e := o.epoch(t); e != 0 {
			t.Errorf("%s, outside rack 1, has err_epoch %d", o, e)
		}
		for _, rec := range h.roundRecords(w, msgHealthChanged, o, since) {
			if roundSet(rec) {
				t.Errorf("%s, outside rack 1, had its epoch set for %q", o,
					rec.str("reason"))
			}
		}
	}
	applied := h.waitRecord(t, 10*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool { return rec.str("kind") == reactionFailover })
	failRev := applied.num("revision")
	newAgent, newKey := c.cntlrAgent(t, 2)
	h.waitUntil(t, 20*time.Second, "cntlr 2's promotion", func() (bool, string) {
		for _, msg := range newAgent.Record(newKey).Syncups {
			req := msg.(*pb.SyncupCntlrRequest)
			if req.GetCntlr().GetPrimary() && req.GetRevision() >= failRev {
				return true, ""
			}
		}
		return false, "no SyncupCntlr with primary set at the failover's revision"
	})
	if !c.cdcLists(t, 2) || c.cdcLists(t, 1) {
		t.Errorf("the discovery record lists cntlr 1 %t, cntlr 2 %t; want the "+
			"new primary alone", c.cdcLists(t, 1), c.cdcLists(t, 2))
	}

	roundSilence(t, false, rack...)
	h.waitUntil(t, 30*time.Second, "every object of rack 1 healthy again",
		func() (bool, string) {
			for _, o := range rack {
				if e := o.epoch(t); e != 0 {
					return false, fmt.Sprintf("%s: err_epoch %d", o, e)
				}
			}
			return true, ""
		})
	c.waitConverged(t)
	if !c.cdcLists(t, 1) || !c.cdcLists(t, 2) {
		t.Errorf("the discovery record lists cntlr 1 %t, cntlr 2 %t; want both",
			c.cdcLists(t, 1), c.cdcLists(t, 2))
	}
}

// TestWholeWorkerSilentDiskNodesFailoverWaitsForNoSide pins RW14 and RW22: a
// failover's sides-first hold waits only for the sides that were healthy in
// its load. Every disk node's fake stops answering, for its node and its
// sides, so every side's health epoch is set; then the primary is disabled.
// The old primary reports its demotion at once, which ends the demotion hold,
// and the hold that follows waits for no side: the new primary is promoted
// with neither hold ended by its timer. Once the disk nodes answer again, the
// pool converges under the new primary.
func TestWholeWorkerSilentDiskNodesFailoverWaitsForNoSide(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, nil)
	var dark []roundObj
	for id := uint64(1); id <= uint64(c.spec.dnCnt); id++ {
		dark = append(dark, roundDnObjs(t, c, id)...)
	}
	roundSilence(t, true, dark...)
	t.Cleanup(func() {
		for _, o := range dark {
			_ = o.agent.Update(o.key, func(ob *agentfake.Object) {
				ob.Hang, ob.HangSyncup = false, false
			})
		}
	})
	h.waitUntil(t, 30*time.Second, "every disk node and side unhealthy",
		func() (bool, string) {
			for _, o := range dark {
				if o.epoch(t) == 0 {
					return false, fmt.Sprintf("%s: no err_epoch", o)
				}
			}
			return true, ""
		})

	since := time.Now()
	disabled := true
	if _, _, err := plant.SetCntlr(context.Background(), h.cli, c.cl, c.sp.Ref, 1,
		plant.CntlrFlags{Disabled: &disabled}); err != nil {
		t.Fatalf("disable cntlr 1: %v", err)
	}
	applied := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.at.After(since) && rec.str("kind") == reactionFailover
		})
	failRev := applied.num("revision")
	newAgent, newKey := c.cntlrAgent(t, 2)
	var promotedAt time.Time
	h.waitUntil(t, 20*time.Second, "cntlr 2's promotion", func() (bool, string) {
		for _, msg := range newAgent.Record(newKey).Syncups {
			req := msg.(*pb.SyncupCntlrRequest)
			if req.GetCntlr().GetPrimary() && req.GetRevision() >= failRev {
				promotedAt = time.Now()
				return true, ""
			}
		}
		return false, "no SyncupCntlr with primary set at the failover's revision"
	})
	t.Logf("cntlr 2 promoted %v after the failover's commit",
		promotedAt.Sub(applied.at).Round(time.Millisecond))
	// A hold ended by its timer logs its record before it hands the cntlrs
	// their plans, so by the promotion it would be there.
	for _, msg := range []string{msgSpDemotionUnsynced, msgSpSidesUnsynced} {
		for _, rec := range h.records(w, msg) {
			if rec.at.After(since) {
				t.Errorf("%q at revision %d: the hold waited for what it "+
					"should not", msg, rec.num("revision"))
			}
		}
	}

	roundSilence(t, false, dark...)
	c.waitConverged(t)
	if got := c.primary(t); got != 2 {
		t.Errorf("the pool converged with cntlr %d primary, want 2", got)
	}
}

// TestWholeWorkerSilentControllerNodesReplaceThePrimary pins AR2, AR5 and
// AR7's replacement of a primary with no standby to fail over to: every
// controller node that holds a cntlr stops answering, for its node and its
// cntlr, so both cntlrs are unhealthy. The failover finds no candidate and
// the pass goes on to replace the primary itself, in the same pass, on the
// one controller node left, outside the standby's location: the new cntlr is
// created primary, its controller node is told of it and it is promoted,
// and the discovery record lists it. The third controller node stays
// healthy because the replacement needs a candidate: a silent controller
// node has its health epoch set, so it has no capacity key (MD4) and AR7
// would find no node to replace the primary on.
//
// The order is made certain. AR7 replaces the smallest cntlr_id that is due,
// and a standby replaced first would hand the failover a candidate, so the
// standby's controller node goes silent only once the primary's health epoch
// is set: the standby's epoch, in whole seconds, is then a second later at
// least, and its replacement can never be due before the primary's. The
// primary's threshold is long enough that the standby is unhealthy before the
// failover could pick it, and the cntlr threshold longer still, so the
// failover's trigger holds, and logs that it has no candidate, in the pass
// that replaces the primary.
func TestWholeWorkerSilentControllerNodesReplaceThePrimary(t *testing.T) {
	t.Parallel()
	h, c, w := roundsCase(t, func(s *stdSpec) {
		th := proto.Clone(s.threshold).(*pb.EventThreshold)
		th.PrimaryUnhealthy = 6
		th.CntlrUnhealthy = 10
		s.threshold = th
	})
	first, second := roundCnObjs(t, c, 1), roundCnObjs(t, c, 2)
	dark := append(slices.Clone(first), second...)
	t.Cleanup(func() {
		for _, o := range dark {
			_ = o.agent.Update(o.key, func(ob *agentfake.Object) {
				ob.Hang, ob.HangSyncup = false, false
			})
		}
	})
	since := time.Now()
	roundSilence(t, true, first...)
	h.waitRoundRecord(t, 20*time.Second, w, msgHealthChanged, roundCntlr(t, c, 1),
		since, roundSet)
	roundSilence(t, true, second...)

	applied := h.waitRecord(t, 40*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.at.After(since) && rec.str("kind") == reactionReplaceCntlr &&
				rec.num("old_cntlr_id") == 1
		})
	newId := applied.num("new_cntlr_id")
	skipped := false
	for _, rec := range h.records(w, msgReactionSkipped) {
		if rec.traceId == applied.traceId && rec.str("kind") == reactionFailover &&
			rec.str("reason") == reasonNoCandidate {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("the pass that replaced cntlr 1 logged no failover skipped " +
			"for want of a candidate before it")
	}
	for _, rec := range h.records(w, msgReactionApplied) {
		if rec.at.After(since) && rec.str("kind") == reactionFailover {
			t.Errorf("a failover was applied at revision %d with every "+
				"standby unhealthy", rec.num("revision"))
		}
	}
	state := c.load(t)
	if _, ok := state.Cntlrs[1]; ok {
		t.Errorf("cntlr 1 is still in the pool after its replacement")
	}
	repl := c.cntlr(t, newId)
	if repl.GetAddrPort() != stdCnAddr(3) || !repl.GetPrimary() {
		t.Fatalf("the replacement cntlr %d is at %s, primary %t; want the "+
			"primary on cn 3", newId, repl.GetAddrPort(), repl.GetPrimary())
	}
	cn3 := c.cnAgent(3)
	newKey := agentfake.CntlrKey(stdSpId, newId)
	h.waitUntil(t, 20*time.Second, "cn 3 told of the replacement and its "+
		"promotion applied", func() (bool, string) {
		listed := false
		for _, msg := range cn3.Record(agentfake.CnKey).Syncups {
			for _, ptr := range msg.(*pb.SyncupCnRequest).GetCntlrPointerList() {
				if ptr.GetCntlrId() == newId {
					listed = true
				}
			}
		}
		if !listed {
			return false, "no SyncupCn of cn 3 lists it"
		}
		promoted := false
		for _, msg := range cn3.Record(newKey).Syncups {
			if msg.(*pb.SyncupCntlrRequest).GetCntlr().GetPrimary() {
				promoted = true
			}
		}
		if !promoted {
			return false, "no SyncupCntlr with primary set"
		}
		for _, rec := range h.records(w, msgSyncupResult) {
			id, ok := roundCntlrIdOf(rec)
			if ok && id == newId && rec.num("code") == 0 && rec.str("error") == "" {
				return true, ""
			}
		}
		return false, "no accepted syncup of it"
	})
	h.waitUntil(t, 10*time.Second, "the discovery record listing the "+
		"replacement", func() (bool, string) {
		return c.cdcLists(t, newId), "not listed"
	})
}
