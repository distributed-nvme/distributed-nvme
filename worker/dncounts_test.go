package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The dn role's count compare (RW13): the free extents of a disk node's own
// counts (dnagent.md DN18) against its record's free_ext_cnt, at one
// revision, on round replies only, logged on the second disagreement in a
// row. dnTestConf's record holds 90 free extents of 100. A case that pins a
// gate disagrees with it, at 80 free extents, or leaves the compare no counts
// at all, so a gate and not agreement keeps it from logging; a case that
// agrees reports the record's own 90.

// countsInfo is a clean DnInfo whose disk counts say free of total extents
// free, with side_recs_match match.
func countsInfo(free uint64, total uint64, match bool) *pb.DnInfo {
	return &pb.DnInfo{
		DiskInfo: resOk("disk"),
		MetaInfo: resOk("meta"),
		PortInfo: resOk("port"),
		DiskCounts: &pb.DnInfo_DiskCounts{
			TotalExtCnt:   total,
			FreeExtCnt:    free,
			SideRecCnt:    2,
			FreeMetaUnits: 64,
			SideRecsMatch: match,
		},
	}
}

// countsDriver is a dn driver outside any loop, for the compare alone. Its
// record is what a read of dnTestConf with a DnRev at revision 42 leaves.
func countsDriver(t *testing.T) (*dnDriver, *logCapture) {
	t.Helper()
	h := newRevHarness(t)
	h.defaultConf()
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleDn,
		shard:   testShard,
		cid:     testCid,
		id:      testDnId,
		seed:    seedOf(1),
		desired: desiredState{revision: 42, handle: testAddr},
	}
	d := newDnDriver(params, nil).(*dnDriver)
	d.noteRecord(dnTestConf(), 42, true, true)
	return d, h.logs
}

// roundReply folds one round's reply as the loop does: the pump decodes it
// (fold) and the round marks it as a round's before observe sees it.
func roundReply(
	d *dnDriver,
	code uint32,
	revision uint64,
	info *pb.DnInfo,
) *replyState {
	r := d.fold(&pb.AgentReply{Code: code}, revision, info)
	r.fromRound = true
	return r
}

// TestDnCountsCompareGates: a reply is compared only when it is a round's
// accepted reply whose latest known info carries counts that say the side
// records match, and when the revision of the reply that carried them, the
// round's reply revision and the revision read with the record are one. Each
// case makes three replies, and a run logs on its second disagreeing compare
// (dnCountsRun), so a case whose replies passed every gate would log within
// them, as the two controls do.
func TestDnCountsCompareGates(t *testing.T) {
	ctx := context.Background()
	disagree := countsInfo(80, 100, true)
	for _, tc := range []struct {
		name string
		// setup runs once the record is set; reply then makes the three
		// replies, each folded and compared.
		setup func(d *dnDriver)
		reply func(d *dnDriver) *replyState
		want  int
	}{
		{name: "the control: a round at the record's revision", want: 1,
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, disagree)
			}},
		{name: "a round with leftovers is accepted too", want: 1,
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, common.ReplyCodeLeftover, 42, disagree)
			}},
		{name: "a syncup's reply",
			reply: func(d *dnDriver) *replyState {
				return d.fold(&pb.AgentReply{}, 42, disagree)
			}},
		{name: "a rejected round",
			reply: func(d *dnDriver) *replyState {
				return roundReply(
					d, common.ReplyCodeStaleRevision, 42, disagree)
			}},
		{name: "no counts",
			reply: func(d *dnDriver) *replyState {
				info := countsInfo(80, 100, true)
				info.DiskCounts = nil
				return roundReply(d, 0, 42, info)
			}},
		{name: "side records that do not match",
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, countsInfo(80, 100, false))
			}},
		{name: "no record",
			setup: func(d *dnDriver) { d.noteRecord(nil, 0, false, false) },
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, disagree)
			}},
		{name: "a record without its DnRev",
			setup: func(d *dnDriver) {
				d.noteRecord(dnTestConf(), 42, true, false)
			},
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, disagree)
			}},
		{name: "another dn's record at this address",
			setup: func(d *dnDriver) {
				conf := dnTestConf()
				conf.DnId = testDnId + 1
				d.noteRecord(conf, 42, true, true)
			},
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, disagree)
			}},
		{name: "counts a reply at another revision carried",
			setup: func(d *dnDriver) {
				d.fold(&pb.AgentReply{}, 41, disagree)
			},
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, nil)
			}},
		{name: "a round at another revision than the counts and record",
			setup: func(d *dnDriver) {
				d.fold(&pb.AgentReply{}, 42, disagree)
			},
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 43, nil)
			}},
		{name: "a record read at another revision",
			setup: func(d *dnDriver) {
				d.noteRecord(dnTestConf(), 43, true, true)
			},
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, disagree)
			}},
		{name: "counts a dead stream reported",
			setup: func(d *dnDriver) {
				d.fold(&pb.AgentReply{}, 42, disagree)
				d.markInfoUnknown()
			},
			reply: func(d *dnDriver) *replyState {
				return roundReply(d, 0, 42, nil)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, logs := countsDriver(t)
			if tc.setup != nil {
				tc.setup(d)
			}
			for range 3 {
				d.compareCounts(ctx, tc.reply(d))
			}
			if got := len(logs.withMsg(msgDnFreeCountDisagrees)); got !=
				tc.want {
				t.Errorf("%d %q records, want %d",
					got, msgDnFreeCountDisagrees, tc.want)
			}
		})
	}
}

// TestDnCountsCompareRuns: the second disagreeing compare in a row logs, once
// per run, whichever way the counts disagree; an agreeing compare ends the
// run; nothing else extends or ends it (RW13). rev is the revision of the
// record the compare holds, 42 until a B, and a step's replies come at rev
// unless it says otherwise. D and U are rounds that disagree, the agent
// counting fewer free extents than the record and more, and A is one that
// agrees. Every other step neither extends nor ends a run, and those that
// carry counts carry D's. S is a round whose side records do not match, N a
// round whose latest info has no counts, V a round at rev+1 with its counts,
// O a round whose latest counts came on a syncup's reply at rev-1, L a round
// at rev+1 whose latest counts came on a syncup's reply at rev, X a round
// while the last read of the record found none, after which a read finds it
// again, Y a SyncupDn's reply, R a rejected round, M a missed round, F a
// syncup whose read of the record fails, which leaves the record the compare
// holds as it is (RW2), and B a read that finds the record at rev+1, which
// becomes rev. A step that carries counts builds a fresh info, because a
// missed round marks the latest one unknown in place.
func TestDnCountsCompareRuns(t *testing.T) {
	ctx := context.Background()
	play := func(t *testing.T, d *dnDriver, rev *uint64, step rune) {
		t.Helper()
		round := func(revision uint64, info *pb.DnInfo) {
			d.compareCounts(ctx, roundReply(d, 0, revision, info))
		}
		switch step {
		case 'D':
			round(*rev, countsInfo(80, 100, true))
		case 'U':
			round(*rev, countsInfo(95, 100, true))
		case 'A':
			round(*rev, countsInfo(90, 100, true))
		case 'S':
			round(*rev, countsInfo(80, 100, false))
		case 'N':
			info := countsInfo(80, 100, true)
			info.DiskCounts = nil
			round(*rev, info)
		case 'V':
			round(*rev+1, countsInfo(80, 100, true))
		case 'O':
			d.compareCounts(ctx,
				d.fold(&pb.AgentReply{}, *rev-1, countsInfo(80, 100, true)))
			round(*rev, nil)
		case 'L':
			d.compareCounts(ctx,
				d.fold(&pb.AgentReply{}, *rev, countsInfo(80, 100, true)))
			round(*rev+1, nil)
		case 'X':
			d.noteRecord(nil, 0, false, false)
			round(*rev, countsInfo(80, 100, true))
			d.noteRecord(dnTestConf(), *rev, true, true)
		case 'B':
			*rev++
			d.noteRecord(dnTestConf(), *rev, true, true)
		case 'Y':
			d.compareCounts(ctx,
				d.fold(&pb.AgentReply{}, *rev, countsInfo(80, 100, true)))
		case 'R':
			d.compareCounts(ctx, roundReply(d, common.ReplyCodeStaleRevision,
				*rev, countsInfo(80, 100, true)))
		case 'M':
			d.unreachable(ctx)
		case 'F':
			// The read fails before the syncup uses its connection.
			store := d.deps.store.(*fakeStore)
			store.mu.Lock()
			store.getErr = errors.New("etcd unavailable")
			store.mu.Unlock()
			r, err := d.syncup(ctx, nil, testClusterConf())
			store.mu.Lock()
			store.getErr = nil
			store.mu.Unlock()
			if r != nil || err != nil {
				t.Fatalf("a syncup whose read failed = %v, %v, want neither",
					r, err)
			}
		default:
			t.Fatalf("unknown step %q", step)
		}
	}
	for _, tc := range []struct {
		steps string
		want  int
	}{
		{"D", 0},
		{"DD", 1},
		{"DDD", 1},
		{"DDADD", 2},
		{"DADA", 0},
		{"UU", 1},
		{"DU", 1},
		{"UD", 1},
		{"DS", 0},
		{"SSD", 0},
		{"DSD", 1},
		{"DDSDD", 1},
		{"DDSAD", 1},
		{"DDADDDAD", 2},
		{"DN", 0},
		{"NND", 0},
		{"DND", 1},
		{"DDNDD", 1},
		{"DV", 0},
		{"VVD", 0},
		{"DVD", 1},
		{"DDVDD", 1},
		{"DO", 0},
		{"OOD", 0},
		{"DOD", 1},
		{"DDODD", 1},
		{"DL", 0},
		{"LLD", 0},
		{"DLD", 1},
		{"DDLDD", 1},
		{"DX", 0},
		{"XXD", 0},
		{"DXD", 1},
		{"DDXDD", 1},
		{"DY", 0},
		{"YYD", 0},
		{"DYD", 1},
		{"DDYDD", 1},
		{"DR", 0},
		{"RRD", 0},
		{"DRD", 1},
		{"DDRDD", 1},
		{"DM", 0},
		{"MMD", 0},
		{"DMD", 1},
		{"DDMDD", 1},
		{"DFD", 1},
		{"DBD", 1},
		{"DDBDD", 1},
	} {
		t.Run(tc.steps, func(t *testing.T) {
			d, logs := countsDriver(t)
			rev := uint64(42)
			for _, step := range tc.steps {
				play(t, d, &rev, step)
			}
			if got := len(logs.withMsg(msgDnFreeCountDisagrees)); got !=
				tc.want {
				t.Errorf("%d %q records, want %d",
					got, msgDnFreeCountDisagrees, tc.want)
			}
		})
	}
}

// TestDnCountsRecord pins the record of dnv-worker.md, Log records: an Error
// with the node's ids, the revision both counts belong to, the record's two
// extent counts beside the agent's, and the agent's side records.
func TestDnCountsRecord(t *testing.T) {
	ctx := context.Background()
	d, logs := countsDriver(t)
	for range 2 {
		d.compareCounts(ctx, roundReply(d, 0, 42, countsInfo(80, 120, true)))
	}
	recs := logs.withMsg(msgDnFreeCountDisagrees)
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	for key, want := range map[string]any{
		"level":               "ERROR",
		"role":                common.WorkerRoleDn,
		"cluster_id":          float64(testCid),
		"dn_id":               float64(testDnId),
		"addr_port":           testAddr,
		"revision":            float64(42),
		"free_ext_cnt":        float64(90),
		"agent_free_ext_cnt":  float64(80),
		"total_ext_cnt":       float64(100),
		"agent_total_ext_cnt": float64(120),
		"side_rec_cnt":        float64(2),
	} {
		if got := recs[0][key]; got != want {
			t.Errorf("%s = %v (%T), want %v", key, got, got, want)
		}
	}
}

// dnCountsCase is one agent of TestDnRoleComparesAtTheRecordRevision.
type dnCountsCase struct {
	name string
	// info is what the agent's rounds report once it has synced.
	info *pb.DnInfo
	// dnId is the dn the DnConf at the address names, and dnRev the
	// revision of that dn's DnRev; the loop drives testDnId at 42.
	dnId  uint64
	dnRev uint64
	// want is the count of records after each of four compares.
	want [4]int
	// errorRow says info holds an ERROR row, which sets err_epoch.
	errorRow bool
}

// TestDnRoleComparesAtTheRecordRevision runs the compare through the real
// loop: the agent answers revision 0 until the syncup has handed it the
// revision driven, 42, so the syncup's read of the record is the one the
// rounds after it are compared with. A record whose DnRev reads another
// revision than the one the agent holds is never compared, though the loop
// drives that very revision, so a compare against the revision driven instead
// of the one read with the record would log there. The counts are no row and
// a compare writes nothing (HL1, RW13): in every case the health writes are
// the ones the rows alone call for, the clear of the first verdict, which the
// round before the syncup reaches, and, beside an ERROR row, the set that
// follows it, which counts that agree leave in place; and nothing is written
// to the store.
func TestDnRoleComparesAtTheRecordRevision(t *testing.T) {
	noCounts := countsInfo(80, 100, true)
	noCounts.DiskCounts = nil
	portError := countsInfo(90, 100, true)
	portError.PortInfo = resErr("port", "nvmet")
	for _, tc := range []dnCountsCase{
		{name: "agreeing", info: countsInfo(90, 100, true),
			dnId: testDnId, dnRev: 42},
		{name: "an ERROR row beside agreeing counts", info: portError,
			dnId: testDnId, dnRev: 42, errorRow: true},
		{name: "disagreeing", info: countsInfo(80, 100, true),
			dnId: testDnId, dnRev: 42, want: [4]int{0, 1, 1, 1}},
		{name: "side records that do not match",
			info: countsInfo(80, 100, false), dnId: testDnId, dnRev: 42},
		{name: "no counts", info: noCounts, dnId: testDnId, dnRev: 42},
		{name: "another dn's record", info: countsInfo(80, 100, true),
			dnId: testDnId + 1, dnRev: 42},
		{name: "a DnRev past the revision driven",
			info: countsInfo(80, 100, true), dnId: testDnId, dnRev: 43},
	} {
		t.Run(tc.name, inBubble(func(t *testing.T) {
			h := newRevHarness(t)
			h.defaultConf()
			conf := dnTestConf()
			conf.DnId = tc.dnId
			h.seedDnConf(testAddr, conf)
			h.store.seed(t, model.DnRevKey(testShard, testCid, tc.dnId),
				&pb.DnRev{AddrPort: testAddr, Revision: tc.dnRev})
			stub := &stubDnAgent{}
			stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				if len(stub.syncups()) == 0 {
					return &pb.CheckDnReply{}
				}
				return &pb.CheckDnReply{
					Revision: req.GetRevision(),
					DnInfo:   tc.info,
				}
			})
			h.fleet.addDn(t, testAddr, stub)
			h.startDn(testAddr, 42)

			h.advanceUntil("the syncup", roundInterval, func() bool {
				return len(stub.syncups()) == 1
			})
			for i, want := range tc.want {
				checks := i + 2
				h.advanceUntil(fmt.Sprintf("compare %d", i+1), roundInterval,
					func() bool { return stub.checkCount() >= checks })
				got := len(h.logs.withMsg(msgDnFreeCountDisagrees))
				if got != want {
					t.Fatalf("after compare %d: %d records, want %d",
						i+1, got, want)
				}
			}
			if got := len(stub.syncups()); got != 1 {
				t.Fatalf("%d syncups, want the one", got)
			}
			wantSets := []bool{false}
			if tc.errorRow {
				wantSets = append(wantSets, true)
			}
			var gotSets []bool
			for _, write := range h.hw.all() {
				gotSets = append(gotSets, write.epoch != 0)
			}
			if !slices.Equal(gotSets, wantSets) {
				t.Fatalf("health writes setting err_epoch = %v, want %v: %+v",
					gotSets, wantSets, h.hw.all())
			}
			puts, dels := h.store.putKeys(), h.store.deletedKeys()
			if len(puts) != 0 || len(dels) != 0 {
				t.Fatalf("store writes: puts %v, deletes %v, want none",
					puts, dels)
			}
		}))
	}
}

// TestDnRoleRefreshFeedsTheCountCompare: a quiet node never syncs, so the only
// read of its record is the health monitor's refresh (HL3), a minute after
// the monitor's first write, and that read is what the compare holds. Until
// it, nothing is compared; the round whose verdict reads the record makes the
// first compare, and the round after it logs.
func TestDnRoleRefreshFeedsTheCountCompare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newRevHarness(t)
		h.defaultConf()
		h.seedDnConf(testAddr, dnTestConf())
		h.store.seed(t, model.DnRevKey(testShard, testCid, testDnId),
			&pb.DnRev{AddrPort: testAddr, Revision: 42})
		stub := &stubDnAgent{}
		stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{
				Revision: req.GetRevision(),
				DnInfo:   countsInfo(80, 100, true),
			}
		})
		h.fleet.addDn(t, testAddr, stub)
		h.startDn(testAddr, 42)

		h.advanceUntil("the first verdict", roundInterval, func() bool {
			return len(h.hw.all()) == 1
		})
		refreshAt := h.clk.now().Add(nodeRecordMaxAge)
		records := func() int {
			return len(h.logs.withMsg(msgDnFreeCountDisagrees))
		}
		h.advanceUntil("the last round short of the refresh", roundInterval,
			func() bool {
				return !h.clk.now().Add(roundInterval).Before(refreshAt)
			})
		if got := records(); got != 0 {
			t.Fatalf("%d records before the record was ever read", got)
		}
		checks := stub.checkCount()
		h.advanceUntil("the round that refreshes", roundInterval,
			func() bool { return stub.checkCount() > checks })
		if got := records(); got != 0 {
			t.Fatalf("%d records after the first compare, want 0", got)
		}
		h.advanceUntil("the second compare", roundInterval,
			func() bool { return stub.checkCount() > checks+1 })
		if got := records(); got != 1 {
			t.Fatalf("%d records after the second compare, want 1", got)
		}
		if got := len(stub.syncups()); got != 0 {
			t.Fatalf("%d syncups of a quiet node", got)
		}
	})
}
