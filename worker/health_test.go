package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// HL1 — the dn and cn node tables, row by row
// ---------------------------------------------------------------------------

// TestHealthDnTable walks the HL1 table for a DN: an ERROR row in disk_info,
// meta_info or port_info sets; a clean reply clears; PROVISIONING and MISSING
// never set, and a reply with them and no ERROR row is a clean one; a
// rejection code neither sets nor clears.
func TestHealthDnTable(t *testing.T) {
	cases := []struct {
		name    string
		code    uint32
		info    *pb.DnInfo
		want    healthObs
		wantRes string
	}{
		{
			name: "clean",
			info: &pb.DnInfo{
				DiskInfo: resOk("disk"),
				MetaInfo: resOk("meta"),
				PortInfo: resOk("port"),
			},
			want: healthClean,
		},
		{
			name: "no info at all is a clean round (HL5)",
			info: nil,
			want: healthClean,
		},
		{
			name: "disk error",
			info: &pb.DnInfo{
				DiskInfo: resErr("disk", "io error"),
				MetaInfo: resOk("meta"),
			},
			want:    healthErrorRow,
			wantRes: "disk",
		},
		{
			name: "meta error: disk lacks Write Zeroes is a plain ERROR",
			info: &pb.DnInfo{
				DiskInfo: resOk("disk"),
				MetaInfo: resErr("meta", "disk lacks Write Zeroes"),
			},
			want:    healthErrorRow,
			wantRes: "meta",
		},
		{
			name: "port error",
			info: &pb.DnInfo{
				DiskInfo: resOk("disk"),
				PortInfo: resErr("port", "nvmet"),
			},
			want:    healthErrorRow,
			wantRes: "port",
		},
		{
			// PROVISIONING and MISSING are not ERROR rows, so the round is
			// HL1 row 3's clean one: it CLEARS a set err_epoch and never sets
			// one. That is the reading HL1 row 4 and architecture.md §9.5
			// require — "never sets err_epoch, never counts as bad for the
			// §5.6 capacity keys" ([D15]) — and healthNone here would instead
			// freeze a stale epoch on a healthy node.
			name: "provisioning and missing are not ERROR rows: a clean round",
			info: &pb.DnInfo{
				DiskInfo: resStatus(
					"disk", pb.ResStatus_RES_STATUS_PROVISIONING,
				),
				MetaInfo: resStatus("meta", pb.ResStatus_RES_STATUS_MISSING),
			},
			want: healthClean,
		},
		{
			name: "code != 0",
			code: common.ReplyCodeUnknownObject,
			info: &pb.DnInfo{DiskInfo: resErr("disk", "io error")},
			want: healthNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, res := dnObservation(tc.code, tc.info)
			if obs != tc.want {
				t.Fatalf("obs = %v, want %v", obs, tc.want)
			}
			if res != tc.wantRes {
				t.Fatalf("res_name = %q, want %q", res, tc.wantRes)
			}
		})
	}
}

// TestHealthCnTable walks the HL1 table for a CN: port_info, tmpfs_info,
// tmp_file_info and loop_dev_info are the ERROR sources.
func TestHealthCnTable(t *testing.T) {
	cases := []struct {
		name    string
		code    uint32
		info    *pb.CnInfo
		want    healthObs
		wantRes string
	}{
		{
			name: "clean",
			info: &pb.CnInfo{PortInfo: resOk("port")},
			want: healthClean,
		},
		{
			name:    "port error",
			info:    &pb.CnInfo{PortInfo: resErr("port", "x")},
			want:    healthErrorRow,
			wantRes: "port",
		},
		{
			name:    "tmpfs error",
			info:    &pb.CnInfo{TmpfsInfo: resErr("tmpfs", "x")},
			want:    healthErrorRow,
			wantRes: "tmpfs",
		},
		{
			name:    "tmp file error",
			info:    &pb.CnInfo{TmpFileInfo: resErr("tmp_file", "x")},
			want:    healthErrorRow,
			wantRes: "tmp_file",
		},
		{
			name:    "loop dev error",
			info:    &pb.CnInfo{LoopDevInfo: resErr("loop", "x")},
			want:    healthErrorRow,
			wantRes: "loop",
		},
		{
			name: "provisioning is not an ERROR row: a clean round",
			info: &pb.CnInfo{
				PortInfo: resStatus(
					"port", pb.ResStatus_RES_STATUS_PROVISIONING,
				),
			},
			want: healthClean,
		},
		{
			name: "code != 0",
			code: common.ReplyCodeStaleRevision,
			info: &pb.CnInfo{PortInfo: resErr("port", "x")},
			want: healthNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, res := cnObservation(tc.code, tc.info)
			if obs != tc.want {
				t.Fatalf("obs = %v, want %v", obs, tc.want)
			}
			if res != tc.wantRes {
				t.Fatalf("res_name = %q, want %q", res, tc.wantRes)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HL2 — the sp-object tables
// ---------------------------------------------------------------------------

// TestHealthCntlrTable checks HL2's cntlr row: any ERROR row other than
// leg_id_to_leg, which belongs to the legs.
func TestHealthCntlrTable(t *testing.T) {
	clean := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resOk("pool")},
		LegIdToLeg:      map[uint64]*pb.ResInfo{5: resOk("leg")},
	}
	if obs, _ := cntlrObservation(0, clean); obs != healthClean {
		t.Fatalf("clean cntlr = %v", obs)
	}
	legOnly := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resOk("pool")},
		LegIdToLeg:      map[uint64]*pb.ResInfo{5: resErr("leg", "io")},
	}
	if obs, _ := cntlrObservation(0, legOnly); obs != healthClean {
		t.Fatalf("a leg error must not set Cntlr.err_epoch, got %v", obs)
	}
	bad := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resErr("pool", "x")},
	}
	obs, res := cntlrObservation(0, bad)
	if obs != healthErrorRow || res != "pool" {
		t.Fatalf("obs = %v, res = %q", obs, res)
	}
	// A thin row is a cntlr row too.
	thin := &pb.CntlrInfo{
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			9: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				1: resErr("thin", "x"),
			}},
		},
	}
	if obs, res := cntlrObservation(0, thin); obs != healthErrorRow ||
		res != "thin" {
		t.Fatalf("thin obs = %v, res = %q", obs, res)
	}
	if obs, _ := cntlrObservation(1, bad); obs != healthNone {
		t.Fatalf("code != 0 must neither set nor clear, got %v", obs)
	}
}

// TestHealthSideTable checks HL2's side row.
func TestHealthSideTable(t *testing.T) {
	clean := &pb.SideInfo{SideDevInfo: resOk("side")}
	if obs, _ := sideObservation(0, clean); obs != healthClean {
		t.Fatalf("clean side = %v", obs)
	}
	cases := []struct {
		name string
		info *pb.SideInfo
		res  string
	}{
		{
			name: "side dev",
			info: &pb.SideInfo{SideDevInfo: resErr("side", "x")},
			res:  "side",
		},
		{
			name: "dm error row",
			info: &pb.SideInfo{
				CnIdToDmError: map[uint64]*pb.ResInfo{2: resErr("dmerr", "x")},
			},
			res: "dmerr",
		},
		{
			name: "dm linear row",
			info: &pb.SideInfo{
				CnIdToDmLinear: map[uint64]*pb.ResInfo{2: resErr("lin", "x")},
			},
			res: "lin",
		},
		{
			name: "nvmeof row",
			info: &pb.SideInfo{
				CnIdToNvmeof: map[uint64]*pb.ResInfo{2: resErr("nvmeof", "x")},
			},
			res: "nvmeof",
		},
		{
			name: "migration source row",
			info: &pb.SideInfo{
				MigrSrcInfo: &pb.SideInfo_MigrSrcInfo{
					NvmeofInfo: resErr("migrsrc", "x"),
				},
			},
			res: "migrsrc",
		},
		{
			name: "migration destination row",
			info: &pb.SideInfo{
				MigrDstInfo: &pb.SideInfo_MigrDstInfo{
					DmCloneInfo: resErr("migrdst", "x"),
				},
			},
			res: "migrdst",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, res := sideObservation(0, tc.info)
			if obs != healthErrorRow || res != tc.res {
				t.Fatalf("obs = %v, res = %q, want error_row/%s",
					obs, res, tc.res)
			}
		})
	}
	provisioning := &pb.SideInfo{
		SideDevInfo: resStatus(
			"side", pb.ResStatus_RES_STATUS_PROVISIONING,
		),
	}
	if obs, _ := sideObservation(0, provisioning); obs != healthClean {
		t.Fatalf("PROVISIONING must not set Side.err_epoch, got %v", obs)
	}
}

// TestHealthLegTable checks HL2's leg row: only the primary's probe result
// matters, ERROR sets, OK clears, everything else neither.
func TestHealthLegTable(t *testing.T) {
	info := &pb.CntlrInfo{
		LegIdToLeg: map[uint64]*pb.ResInfo{
			1: resErr("leg1", "io"),
			2: resOk("leg2"),
			3: resStatus("leg3", pb.ResStatus_RES_STATUS_PROVISIONING),
			5: resStatus("leg5", pb.ResStatus_RES_STATUS_PENDING),
		},
	}
	if obs, res := legObservation(0, info, 1); obs != healthErrorRow ||
		res != "leg1" {
		t.Fatalf("leg 1 obs = %v, res = %q", obs, res)
	}
	if obs, _ := legObservation(0, info, 2); obs != healthClean {
		t.Fatalf("leg 2 obs = %v, want clean", obs)
	}
	if obs, _ := legObservation(0, info, 3); obs != healthNone {
		t.Fatalf("leg 3 obs = %v, want none", obs)
	}
	// A leg whose prober has not completed a round (cnagent.md CN11): it
	// must not clear, or every promotion's fresh probers would wipe a dead
	// leg's err_epoch and restart AR8's leg_unhealthy clock.
	if obs, _ := legObservation(0, info, 5); obs != healthNone {
		t.Fatalf("PENDING leg obs = %v, want none", obs)
	}
	// A leg the reply does not mention at all.
	if obs, _ := legObservation(0, info, 4); obs != healthNone {
		t.Fatalf("unreported leg obs = %v, want none", obs)
	}
	if obs, _ := legObservation(1, info, 1); obs != healthNone {
		t.Fatalf("code != 0 obs = %v, want none", obs)
	}
}

// ---------------------------------------------------------------------------
// HL3 — transitions only
// ---------------------------------------------------------------------------

func healthTestDeps(t *testing.T) (*deps, *fakeHealthWriter) {
	t.Helper()
	writer := &fakeHealthWriter{}
	d := newTestDeps(
		testConfig(common.WorkerRoleDn), newFakeStore(), newFakeClock(),
	)
	d.health = writer
	return d, writer
}

// seedClusterConf installs a usable stored ClusterConf in the RW21 cache.
// Every DN health write needs one: MD4 derives the DnCapacity key's bin index
// from dn_bin_conf, so newDnMonitor refuses to write without one it can use
// (HL1, §7).
func seedClusterConf(d *deps, cid uint64) {
	setCachedConf(d, cid, testClusterConf())
}

// setCachedConf installs one conf in the RW21 cache exactly as given, the way
// the cache itself stores it (§7) — the only way to hand a reader a conf the
// gateway could not have written.
func setCachedConf(d *deps, cid uint64, cc *pb.ClusterConf) {
	d.conf.mu.Lock()
	d.conf.entries[cid] = cc
	d.conf.mu.Unlock()
}

// TestHealthTransitionsOnly checks HL3: a record is written only when the
// observed health changes, and the observations that neither set nor clear
// write nothing at all.
func TestHealthTransitionsOnly(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	seedClusterConf(d, 7)
	monitor := newDnMonitor(d, 7, 11, func() string { return "dn0:9520" })
	ctx := context.Background()

	// Unhealthy once.
	monitor.observe(ctx, healthUnreachable, "")
	monitor.observe(ctx, healthUnreachable, "")
	monitor.observe(ctx, healthErrorRow, "disk")
	if got := len(writer.all()); got != 1 {
		t.Fatalf("%d writes for one healthy -> unhealthy transition", got)
	}
	// Neither sets nor clears.
	monitor.observe(ctx, healthNone, "")
	if got := len(writer.all()); got != 1 {
		t.Fatalf("healthNone wrote something: %v", writer.all())
	}
	// Recovery, then more clean rounds.
	monitor.observe(ctx, healthClean, "")
	monitor.observe(ctx, healthClean, "")
	writes := writer.all()
	if len(writes) != 2 {
		t.Fatalf("writes = %v, want set then clear", writes)
	}
	if writes[0].record != healthRecordDn || writes[0].epoch == 0 {
		t.Fatalf("first write = %+v, want a nonzero dn epoch", writes[0])
	}
	if writes[1].epoch != 0 || writes[1].addr != "dn0:9520" {
		t.Fatalf("second write = %+v, want a clear on dn0:9520", writes[1])
	}
	recs := logs.withMsg(msgHealthChanged)
	if len(recs) != 2 {
		t.Fatalf("%d health changed records, want 2", len(recs))
	}
	first := recs[0]
	if first["role"] != common.WorkerRoleDn ||
		first["record"] != healthRecordDn ||
		first["reason"] != reasonUnreachable {
		t.Fatalf("first record = %v", first)
	}
	if cid, _ := first["cluster_id"].(float64); uint64(cid) != 7 {
		t.Fatalf("cluster_id = %v", first["cluster_id"])
	}
	if dnId, _ := first["dn_id"].(float64); uint64(dnId) != 11 {
		t.Fatalf("dn_id = %v", first["dn_id"])
	}
	if _, has := first["res_name"]; has {
		t.Fatalf("an unreachable record carries a res_name: %v", first)
	}
	if recs[1]["reason"] != reasonRecovered {
		t.Fatalf("second reason = %v", recs[1]["reason"])
	}
	if epoch, _ := recs[1]["err_epoch"].(float64); epoch != 0 {
		t.Fatalf("recovery err_epoch = %v, want 0", recs[1]["err_epoch"])
	}
}

// TestHealthOfferPredatingOwnWriteIsDropped pins offerRecord's guard (HL3).
// The sp coordinator reads a side or cntlr child's write count before the load
// whose record it offers, and the child drops the offer when it has written
// since: that load may predate the write, and folding it in would hand the
// memo back the state the write replaced. In both orders of the race the next
// verdict reverses the write the load missed, and it must still be written;
// with the stale offer folded in, the memo already equalled that verdict and
// nothing was written. An offer whose count still matches is folded in, and a
// newer offer replaces one not folded in yet.
func TestHealthOfferPredatingOwnWriteIsDropped(t *testing.T) {
	ctx := context.Background()
	epochs := func(writer *fakeHealthWriter) []uint64 {
		var out []uint64
		for _, write := range writer.all() {
			out = append(out, write.epoch)
		}
		return out
	}

	t.Run("the clear after one bad round", func(t *testing.T) {
		d, writer := healthTestDeps(t)
		monitor := newCntlrMonitor(d, 7, 1, 2)
		monitor.observe(ctx, healthClean, "")
		// A pass reads the count and loads the record, still clear; the
		// child's unreachable round writes an epoch before the pass offers
		// what its load found.
		seq := monitor.loadSeq()
		monitor.observe(ctx, healthUnreachable, "")
		monitor.offerRecord(0, seq)
		monitor.observe(ctx, healthClean, "")
		got := epochs(writer)
		if len(got) != 3 || got[0] != 0 || got[1] == 0 || got[2] != 0 {
			t.Fatalf("epoch writes = %v, want clear, set, clear: the stale "+
				"offer suppressed the clear", got)
		}
	})

	t.Run("the set after one clean round", func(t *testing.T) {
		d, writer := healthTestDeps(t)
		monitor := newSideMonitor(d, 7, 1, 2, 3)
		monitor.observe(ctx, healthErrorRow, "side")
		stamped := epochs(writer)[0]
		seq := monitor.loadSeq()
		monitor.observe(ctx, healthClean, "")
		monitor.offerRecord(stamped, seq)
		monitor.observe(ctx, healthErrorRow, "side")
		got := epochs(writer)
		if len(got) != 3 || got[0] == 0 || got[1] != 0 || got[2] == 0 {
			t.Fatalf("epoch writes = %v, want set, clear, set: the stale "+
				"offer suppressed the set", got)
		}
	})

	t.Run("an offer after the last write is folded in", func(t *testing.T) {
		d, writer := healthTestDeps(t)
		monitor := newCntlrMonitor(d, 7, 1, 2)
		monitor.observe(ctx, healthClean, "")
		// Another observer stamps the record; the next pass loads it.
		monitor.offerRecord(d.clk.nowUnix(), monitor.loadSeq())
		monitor.observe(ctx, healthClean, "")
		if got := epochs(writer); len(got) != 2 || got[1] != 0 {
			t.Fatalf("epoch writes = %v, want the stamp cleared", got)
		}
		// The record read stamped, then clear again: the newer offer
		// replaces the older one, and a clean verdict writes nothing.
		monitor.offerRecord(d.clk.nowUnix(), monitor.loadSeq())
		monitor.offerRecord(0, monitor.loadSeq())
		monitor.observe(ctx, healthClean, "")
		if got := epochs(writer); len(got) != 2 {
			t.Fatalf("epoch writes = %v, want 2: a replaced offer was "+
				"folded in", got)
		}
	})
}

// recordReads counts the reads of one node's record through the store the
// loop and its monitor use — the syncup's and the monitor's refresh alike
// (HL3) — with the fake time of each.
type recordReads struct {
	etcdStore
	clk *fakeClock
	key string

	mu    sync.Mutex
	times []time.Time
}

func (s *recordReads) Get(
	ctx context.Context,
	key string,
	msg proto.Message,
) (bool, error) {
	if key == s.key {
		s.mu.Lock()
		s.times = append(s.times, s.clk.now())
		s.mu.Unlock()
	}
	return s.etcdStore.Get(ctx, key, msg)
}

func (s *recordReads) all() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

// nodeStoreHealthWriter is a fake health writer whose dn and cn writes also
// land in the stored DnConf/CnConf under MD6's set/clear rule — a nonzero
// epoch only onto a stored 0, a zero always — so the monitor's next read of
// the record finds what it wrote: the etcd of a node refresh test. As in
// spStateHealthWriter, a write lands in the store before it is recorded, so
// that a stamp the test lays once it has seen a write is never overwritten by
// that write; a write the fake fails lands nowhere.
type nodeStoreHealthWriter struct {
	*fakeHealthWriter
	store *fakeStore
}

func (w *nodeStoreHealthWriter) setDnErrEpoch(
	ctx context.Context,
	cid uint64,
	addrPort string,
	epoch uint64,
	cc *pb.ClusterConf,
) error {
	if err := w.refusal(); err != nil {
		return err
	}
	err := w.put(ctx, model.DnConfKey(cid, addrPort), &pb.DnConf{}, epoch)
	if err != nil {
		return err
	}
	return w.fakeHealthWriter.setDnErrEpoch(ctx, cid, addrPort, epoch, cc)
}

func (w *nodeStoreHealthWriter) setCnErrEpoch(
	ctx context.Context,
	cid uint64,
	addrPort string,
	epoch uint64,
) error {
	if err := w.refusal(); err != nil {
		return err
	}
	err := w.put(ctx, model.CnConfKey(cid, addrPort), &pb.CnConf{}, epoch)
	if err != nil {
		return err
	}
	return w.fakeHealthWriter.setCnErrEpoch(ctx, cid, addrPort, epoch)
}

// put rewrites the err_epoch of the conf stored at key, straight in the fake
// store: no read is counted and no watch event is sent.
func (w *nodeStoreHealthWriter) put(
	ctx context.Context,
	key string,
	conf proto.Message,
	epoch uint64,
) error {
	found, err := w.store.Get(ctx, key, conf)
	if err != nil || !found {
		return err
	}
	var stored *uint64
	switch typed := conf.(type) {
	case *pb.DnConf:
		stored = &typed.ErrEpoch
	case *pb.CnConf:
		stored = &typed.ErrEpoch
	default:
		return fmt.Errorf("no err_epoch in %T", conf)
	}
	if epoch != 0 && *stored != 0 {
		return nil
	}
	*stored = epoch
	data, err := proto.Marshal(conf)
	if err != nil {
		return err
	}
	w.store.mu.Lock()
	w.store.rev++
	w.store.kvs[key] = data
	w.store.mu.Unlock()
	return nil
}

// TestHealthNodeRecordRefresh pins a node monitor's refresh (HL3) on the
// monitor alone: a verdict re-reads the record only once a minute has passed
// since the monitor last read or wrote it, and that read re-seeds the memo, so
// an epoch another observer set over a clean verdict is cleared at the first
// verdict a minute after the monitor's own write and not before. A read that
// fails re-seeds nothing and is retried at the next verdict; an absent record
// re-seeds nothing and counts as read. A monitor that has written nothing yet
// reads nothing, however long it has run.
func TestHealthNodeRecordRefresh(t *testing.T) {
	ctx := context.Background()
	d, writer := healthTestDeps(t)
	seedClusterConf(d, testCid)
	store := d.store.(*fakeStore)
	clk := d.clk.(*fakeClock)
	key := model.DnConfKey(testCid, testAddr)
	reads := &recordReads{etcdStore: store, clk: clk, key: key}
	d.store = reads
	stamp := func(epoch uint64) {
		conf := dnTestConf()
		conf.ErrEpoch = epoch
		store.seed(t, key, conf)
	}
	stamp(0)
	monitor := newDnMonitor(d, testCid, testDnId, func() string {
		return testAddr
	})
	check := func(what string, wantReads int, wantWrites []uint64) {
		t.Helper()
		if got := len(reads.all()); got != wantReads {
			t.Fatalf("%s: %d reads of the record, want %d",
				what, got, wantReads)
		}
		var got []uint64
		for _, write := range writer.all() {
			got = append(got, write.epoch)
		}
		if fmt.Sprint(got) != fmt.Sprint(wantWrites) {
			t.Fatalf("%s: epoch writes = %v, want %v", what, got, wantWrites)
		}
	}

	// HL3 says a minute; the steps below are written in minutes rather than
	// in nodeRecordMaxAge, so they pin its value too.
	minute := time.Minute

	// An empty memo writes its first verdict without reading, however late.
	clk.advance(2 * minute)
	monitor.observe(ctx, healthClean, "")
	check("the first verdict", 0, []uint64{0})

	// Another observer stamps the record. Short of the minute nothing reads
	// it; at the minute the verdict reads it first and clears the stamp.
	stamp(clk.nowUnix())
	clk.advance(minute - time.Second)
	monitor.observe(ctx, healthClean, "")
	check("short of the minute", 0, []uint64{0})
	clk.advance(time.Second)
	monitor.observe(ctx, healthClean, "")
	check("at the minute", 1, []uint64{0, 0})
	monitor.observe(ctx, healthClean, "")
	check("right after the read", 1, []uint64{0, 0})

	// A read that fails re-seeds nothing, and the next verdict reads again.
	stamp(clk.nowUnix())
	clk.advance(minute)
	store.mu.Lock()
	store.getErr = errors.New("etcd unavailable")
	store.mu.Unlock()
	monitor.observe(ctx, healthClean, "")
	check("a failed read", 2, []uint64{0, 0})
	store.mu.Lock()
	store.getErr = nil
	store.mu.Unlock()
	monitor.observe(ctx, healthClean, "")
	check("the read after it", 3, []uint64{0, 0, 0})

	// An absent record re-seeds nothing, and counts as read.
	store.mu.Lock()
	delete(store.kvs, key)
	store.mu.Unlock()
	clk.advance(minute)
	monitor.observe(ctx, healthClean, "")
	monitor.observe(ctx, healthClean, "")
	check("an absent record", 4, []uint64{0, 0, 0})
}

// orphanNode is one node role under TestOrphanedNodeEpochIsClearedByTheOwner:
// its running revision worker and the knobs of its agent and its record.
type orphanNode struct {
	w      *revWorker
	addr   string
	record string
	// checks counts the Check requests the agent received.
	checks func() int
	// syncupRevs is the revision of every Syncup* the agent received.
	syncupRevs func() []uint64
	// setBad switches the agent's Check replies between clean and one ERROR
	// row.
	setBad func(bad bool)
	// putEpoch rewrites the err_epoch of the stored DnConf/CnConf, as another
	// observer's write does.
	putEpoch func(epoch uint64)
}

func startOrphanDn(t *testing.T, h *revHarness) *orphanNode {
	var bad atomic.Bool
	stub := &stubDnAgent{}
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		reply := &pb.CheckDnReply{Revision: req.GetRevision()}
		if bad.Load() {
			reply.DnInfo = &pb.DnInfo{DiskInfo: resErr("disk", "io error")}
		}
		return reply
	})
	h.fleet.addDn(t, testAddr, stub)
	h.seedDnConf(testAddr, dnTestConf())
	return &orphanNode{
		w:      h.startDn(testAddr, 1),
		addr:   testAddr,
		record: healthRecordDn,
		checks: stub.checkCount,
		syncupRevs: func() []uint64 {
			var out []uint64
			for _, req := range stub.syncups() {
				out = append(out, req.GetRevision())
			}
			return out
		},
		setBad: bad.Store,
		putEpoch: func(epoch uint64) {
			conf := dnTestConf()
			conf.ErrEpoch = epoch
			h.seedDnConf(testAddr, conf)
		},
	}
}

func startOrphanCn(t *testing.T, h *revHarness) *orphanNode {
	var bad atomic.Bool
	stub := &stubCnAgent{
		checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
			reply := &pb.CheckCnReply{Revision: req.GetRevision()}
			if bad.Load() {
				reply.CnInfo = &pb.CnInfo{
					TmpfsInfo: resErr("tmpfs", "no space"),
				}
			}
			return reply
		},
	}
	h.fleet.addCn(t, testCnAddr, stub)
	key := model.CnConfKey(testCid, testCnAddr)
	h.store.seed(t, key, cnTestConf())
	return &orphanNode{
		w:      h.startCn(testCnAddr, 1),
		addr:   testCnAddr,
		record: healthRecordCn,
		checks: func() int {
			stub.mu.Lock()
			defer stub.mu.Unlock()
			return len(stub.checkReqs)
		},
		syncupRevs: func() []uint64 {
			var out []uint64
			for _, req := range stub.syncups() {
				out = append(out, req.GetRevision())
			}
			return out
		},
		setBad: bad.Store,
		putEpoch: func(epoch uint64) {
			conf := cnTestConf()
			conf.ErrEpoch = epoch
			h.store.seed(t, key, conf)
		},
	}
}

// TestOrphanedNodeEpochIsClearedByTheOwner is the node twin of
// TestOrphanedEpochIsClearedByTheOwner (HL3), through the real dn and cn
// loops: the memo is a cache of the record, so an owner whose DnConf/CnConf
// another observer rewrote after the owner's own write puts the record back
// at its first verdict after it next reads it. The monitor used to compare
// every verdict with the last one it had written, found no transition and
// wrote nothing, so a DN kept an orphaned epoch — and no capacity key (MD4) —
// until it genuinely flapped.
//
// First a quiet node: its revision does not move, so it never syncs, and the
// monitor reads the record again only once a minute has passed since it last
// read or wrote it. An epoch set over the owner's clean verdict is cleared at
// the first verdict past that minute and not before, by one read and one
// write, and the next read, a minute on, finds the record agreeing and writes
// nothing. Then through a syncup's read (RW13), both ways: an epoch set over
// the owner's clean verdict is cleared, and one cleared under its unhealthy
// verdict is set again, each by one read and one write.
func TestOrphanedNodeEpochIsClearedByTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		start func(t *testing.T, h *revHarness) *orphanNode
	}{
		{"dn", model.DnConfKey(testCid, testAddr), startOrphanDn},
		{"cn", model.CnConfKey(testCid, testCnAddr), startOrphanCn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRevHarness(t)
			h.defaultConf()
			reads := &recordReads{etcdStore: h.store, clk: h.clk, key: tc.key}
			h.deps.store = reads
			h.deps.health = &nodeStoreHealthWriter{
				fakeHealthWriter: h.hw,
				store:            h.store,
			}
			node := tc.start(t, h)
			epochs := func() []uint64 {
				var out []uint64
				for _, write := range h.hw.all() {
					if write.record == node.record {
						out = append(out, write.epoch)
					}
				}
				return out
			}
			wantReads := func(what string, want int) []time.Time {
				t.Helper()
				got := reads.all()
				if len(got) != want {
					t.Fatalf("%s: %d reads of the record %v, want %d",
						what, len(got), got, want)
				}
				return got
			}
			// round lets one more Check round reach the agent, the clock
			// moving a second at a time so that a round's reply is read long
			// before the round's own timeout can fire (RW4 step 3). The agent
			// sees a Check only once the loop has observed everything before
			// it — the previous round and any syncup since.
			round := func() {
				t.Helper()
				checks := node.checks()
				h.advanceUntil("a round", time.Second, func() bool {
					return node.checks() > checks
				})
			}
			// quietUntil runs rounds until one reaches the agent at or after
			// the given time, whose verdict then follows at that time: the
			// clock does not move again until the next round.
			quietUntil := func(at time.Time) {
				t.Helper()
				for h.clk.now().Before(at) {
					round()
				}
			}
			// resync bumps the revision; the syncup reads the record, and
			// its reply is the first verdict after that read.
			resync := func(revision uint64) {
				t.Helper()
				node.w.update(desiredState{
					revision: revision,
					handle:   node.addr,
				})
				waitFor(t, "the syncup", func() bool {
					revs := node.syncupRevs()
					return len(revs) > 0 && revs[len(revs)-1] == revision
				})
				round()
			}

			// The owner's first verdict: clean, and written, because a
			// monitor that has written nothing yet has an empty memo. The
			// agent reports the revision driven, so there is no syncup, and
			// nothing has read the record.
			waitFor(t, "the owner's first verdict", func() bool {
				return len(epochs()) == 1
			})
			written := h.clk.now()
			wantReads("the first verdict", 0)

			// Another observer stamps the record unhealthy, and the node
			// stays quiet. The first verdict a minute after the owner's write
			// reads the record and clears the stamp; none before it reads.
			node.putEpoch(h.clk.nowUnix())
			minute := written.Add(nodeRecordMaxAge)
			quietUntil(minute)
			waitFor(t, "the quiet node's clear", func() bool {
				return len(epochs()) == 2
			})
			if got := epochs(); got[0] != 0 || got[1] != 0 {
				t.Fatalf("epoch writes = %v, want the first verdict's "+
					"clear and the orphaned epoch's", got)
			}
			read := wantReads("the quiet node's clear", 1)[0]
			// The minute's exact edge is TestHealthNodeRecordRefresh's: a
			// loaded loop can fall clock steps behind its timer here, and a
			// refresh a round late never lands, the clock being frozen while
			// the clear is awaited.
			if read.Before(minute) {
				t.Fatalf("the record was read at %v, before the minute "+
					"at %v", read, minute)
			}
			// A minute on, the next read finds the record agreeing: no write.
			quietUntil(read.Add(nodeRecordMaxAge))
			waitFor(t, "the next read", func() bool {
				return len(reads.all()) == 2
			})
			round()
			if got := epochs(); len(got) != 2 {
				t.Fatalf("epoch writes = %v, want 2: a read that found "+
					"the record agreeing wrote", got)
			}
			if again := wantReads("the next read", 2)[1]; again.Sub(read) <
				nodeRecordMaxAge {
				t.Fatalf("the record was read at %v and again at %v, "+
					"less than %v apart", read, again, nodeRecordMaxAge)
			}

			// Another observer stamps the record again, and a revision bump
			// makes the owner sync: the syncup's read is the load.
			node.putEpoch(h.clk.nowUnix())
			resync(2)
			if got := epochs(); len(got) != 3 || got[2] != 0 {
				t.Fatalf("epoch writes = %v, want the syncup's clear of "+
					"the stamp", got)
			}
			wantReads("the syncup", 3)
			// Once: clean rounds after it write nothing (HL3).
			round()
			round()
			if got := epochs(); len(got) != 3 {
				t.Fatalf("epoch writes = %v, want 3: a clean round "+
					"re-wrote the clear", got)
			}

			// The other way round: the node turns bad, the owner sets the
			// epoch, and another observer clears it.
			node.setBad(true)
			h.advanceUntil("the owner's set", time.Second, func() bool {
				return len(epochs()) == 4
			})
			if got := epochs(); got[3] == 0 {
				t.Fatalf("epoch writes = %v, want a set last", got)
			}
			node.putEpoch(0)
			// The syncup's reply carries no info, so its verdict is the
			// latest known one's (HL5): still the ERROR row.
			resync(3)
			if got := epochs(); len(got) != 5 || got[4] == 0 {
				t.Fatalf("epoch writes = %v, want the owner's set again "+
					"over the other observer's clear", got)
			}
			round()
			if got := epochs(); len(got) != 5 {
				t.Fatalf("epoch writes = %v, want 5: an unhealthy round "+
					"re-wrote the set", got)
			}
			wantReads("the second syncup", 4)
		})
	}
}

// TestHealthErrorRowCarriesResName checks the res_name attribute of the §12
// record.
func TestHealthErrorRowCarriesResName(t *testing.T) {
	logs := captureLogs(t)
	d, _ := healthTestDeps(t)
	monitor := newCnMonitor(d, 3, 4, func() string { return "cn0:9620" })
	monitor.observe(context.Background(), healthErrorRow, "tmpfs")
	recs := logs.withMsg(msgHealthChanged)
	if len(recs) != 1 || recs[0]["res_name"] != "tmpfs" ||
		recs[0]["reason"] != reasonErrorRow {
		t.Fatalf("record = %v", recs)
	}
	if recs[0]["record"] != healthRecordCn {
		t.Fatalf("record kind = %v", recs[0]["record"])
	}
}

// TestHealthWriteFailureIsRetried checks that a failed write leaves the
// in-memory state untouched, so the next round retries the transition.
func TestHealthWriteFailureIsRetried(t *testing.T) {
	captureLogs(t)
	d, writer := healthTestDeps(t)
	seedClusterConf(d, 1)
	writer.err = errors.New("etcd down")
	monitor := newDnMonitor(d, 1, 2, func() string { return "dn0:9520" })
	monitor.observe(context.Background(), healthUnreachable, "")
	writer.err = nil
	monitor.observe(context.Background(), healthUnreachable, "")
	if got := len(writer.all()); got != 1 {
		t.Fatalf("%d writes, want the retry to land exactly once", got)
	}
}

// TestHealthSettle pins HL2's settle half of the cntlr monitor: a clean
// observation the driver judges to prove the primary role (canSettle) clears
// the record's settling flag once, in the same write as any err_epoch clear —
// and with a write of its own when there is no health transition to ride on,
// which HL3's transitions-only rule would otherwise swallow.
func TestHealthSettle(t *testing.T) {
	ctx := context.Background()
	settleWrite := healthWrite{
		record: healthRecordCntlr, cid: 7, spId: 1, objId: 2,
		epoch: 0, settle: true,
	}
	newMonitor := func(t *testing.T) (*healthMonitor, *fakeHealthWriter) {
		t.Helper()
		d, writer := healthTestDeps(t)
		monitor := newCntlrMonitor(d, 7, 1, 2)
		monitor.settlePending = true
		return monitor, writer
	}

	t.Run("settle without a transition", func(t *testing.T) {
		logs := captureLogs(t)
		monitor, writer := newMonitor(t)
		// Known healthy first, through the plain observe: never a settle.
		monitor.observe(ctx, healthClean, "")
		monitor.observe(ctx, healthClean, "")
		// Clean, but not a report of the primary shape: nothing to write.
		if monitor.observeSettle(ctx, healthClean, "", false) {
			t.Fatalf("settled without canSettle")
		}
		if got := writer.all(); len(got) != 1 || got[0].settle {
			t.Fatalf("writes = %+v, want the one recovery write only", got)
		}
		if !monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("a clean canSettle observation did not settle")
		}
		writes := writer.all()
		if len(writes) != 2 || writes[1] != settleWrite {
			t.Fatalf("writes = %+v, want the settle write %+v last",
				writes, settleWrite)
		}
		if monitor.settlePending {
			t.Fatalf("the memo survived the settle write")
		}
		// The memo is cleared: a second clean observation writes nothing.
		if monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("settled twice")
		}
		if got := len(writer.all()); got != 2 {
			t.Fatalf("%d writes after the memo cleared, want 2", got)
		}
		// The settle is not a health transition: one record, the recovery.
		if got := len(logs.withMsg(msgHealthChanged)); got != 1 {
			t.Fatalf("%d health changed records, want 1", got)
		}
	})

	t.Run("first observation settles with the recovery write", func(t *testing.T) {
		monitor, writer := newMonitor(t)
		if !monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("did not settle")
		}
		if got := writer.all(); len(got) != 1 || got[0] != settleWrite {
			t.Fatalf("writes = %+v, want one combined write %+v",
				got, settleWrite)
		}
	})

	t.Run("unhealthy keeps the memo", func(t *testing.T) {
		monitor, writer := newMonitor(t)
		if monitor.observeSettle(ctx, healthErrorRow, "pool", true) {
			t.Fatalf("an ERROR row settled the cntlr")
		}
		writes := writer.all()
		if len(writes) != 1 || writes[0].epoch == 0 || writes[0].settle {
			t.Fatalf("writes = %+v, want the epoch write alone", writes)
		}
		if !monitor.settlePending {
			t.Fatalf("an unhealthy observation cleared the memo")
		}
		// The next clean one clears err_epoch and settles in one write.
		if !monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("the recovery did not settle")
		}
		writes = writer.all()
		if len(writes) != 2 || writes[1] != settleWrite {
			t.Fatalf("writes = %+v, want %+v last", writes, settleWrite)
		}
	})

	t.Run("neither set nor clear never settles", func(t *testing.T) {
		monitor, writer := newMonitor(t)
		if monitor.observeSettle(ctx, healthNone, "", true) {
			t.Fatalf("a rejected reply settled the cntlr")
		}
		if got := writer.all(); len(got) != 0 {
			t.Fatalf("writes = %+v, want none", got)
		}
		if !monitor.settlePending {
			t.Fatalf("a rejected reply cleared the memo")
		}
		// The next clean one settles, in the first write of all.
		if !monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("the next observation did not settle")
		}
		if got := writer.all(); len(got) != 1 || got[0] != settleWrite {
			t.Fatalf("writes = %+v, want the settle %+v", got, settleWrite)
		}
	})

	t.Run("a failed write keeps the memo", func(t *testing.T) {
		captureLogs(t)
		monitor, writer := newMonitor(t)
		monitor.observe(ctx, healthClean, "")
		writer.err = errors.New("etcd down")
		if monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("a failed write reported a settle")
		}
		if !monitor.settlePending {
			t.Fatalf("a failed write cleared the memo")
		}
		writer.err = nil
		if !monitor.observeSettle(ctx, healthClean, "", true) {
			t.Fatalf("the next observation did not retry the settle")
		}
		writes := writer.all()
		if len(writes) != 2 || writes[1] != settleWrite {
			t.Fatalf("writes = %+v, want the retried settle last", writes)
		}
	})

	t.Run("no memo never settles", func(t *testing.T) {
		monitor, writer := newMonitor(t)
		monitor.settlePending = false
		monitor.observeSettle(ctx, healthClean, "", true)
		monitor.observeSettle(ctx, healthErrorRow, "pool", true)
		monitor.observeSettle(ctx, healthClean, "", true)
		for _, write := range writer.all() {
			if write.settle {
				t.Fatalf("writes = %+v, want no settle", writer.all())
			}
		}
		if got := len(writer.all()); got != 3 {
			t.Fatalf("%d writes, want the three transitions", got)
		}
	})
}

// TestHealthDnWriteNeedsClusterConf checks HL1 against MD6's SetDnErrEpoch
// row: the op maintains the DnCapacity key in the same STM, and MD4 computes
// that key's bin index from the cluster's dn_bin_conf. A cluster deleted from
// the RW21 cache between the loop's RW9 gate and this write must NOT be
// papered over with an invented 0/4/8/12 ladder — that leaves the real key
// undeleted and writes a duplicate at the wrong bin, which §6.3's bin scan
// then hands out as an allocation candidate for a DN this very write is
// flagging unhealthy. The write is skipped and retried next round (RW12).
func TestHealthDnWriteNeedsClusterConf(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	monitor := newDnMonitor(d, 7, 11, func() string { return "dn0:9520" })
	ctx := context.Background()

	monitor.observe(ctx, healthErrorRow, "disk")
	if got := writer.all(); len(got) != 0 {
		t.Fatalf("wrote %v with no cluster conf in the cache", got)
	}
	if got := len(logs.withMsg("health write failed")); got != 1 {
		t.Fatalf("%d health write failed records, want 1", got)
	}
	if got := len(logs.withMsg(msgHealthChanged)); got != 0 {
		t.Fatalf("a skipped write logged a health change")
	}
	// Nothing was remembered (HL3), so the next round retries the transition
	// once the conf is back.
	seedClusterConf(d, 7)
	monitor.observe(ctx, healthErrorRow, "disk")
	writes := writer.all()
	if len(writes) != 1 || writes[0].record != healthRecordDn ||
		writes[0].epoch == 0 {
		t.Fatalf("writes after the conf came back = %v", writes)
	}
}

// TestHealthDnWriteRefusesAnInvalidConf is the §7 twin of the test above, for
// the conf that is PRESENT but unusable. The two are the same failure: MD4
// needs a bin ladder, and a cluster whose stored dn_bin_conf has none leaves
// the monitor with nothing to compute the DnCapacity key from. Guessing
// the ladder would write the capacity key at a bin the rest of the cluster
// does not address — the same duplicate-key damage the test above describes,
// from a cluster that is not even deleted — so the write fails instead.
//
// This is not the only worker STM write that takes a ClusterConf — the sp
// role's model.GrowSlice, model.CreateSpareLeg and model.DrainSpSlice take one
// too (reaction.go, drain.go) — but each of those is handed the p.cc the
// reaction pass validated before it built the pass, and GrowSlice (both confs)
// and DrainSpSlice (the cluster one) re-check at the top of their own STMs
// besides. The monitor's write re-reads the conf from the RW21 cache per
// write instead of using the snapshot the revision loop validated, so the
// loop's gate does not cover the value this write uses; that, not uniqueness,
// is why it validates again (health.go).
func TestHealthDnWriteRefusesAnInvalidConf(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	// Present in the cache, but with the bin ladder CreateCluster always
	// writes missing: proto3 gives back all-zero shifts, which §6.2 does not
	// accept as a ladder.
	setCachedConf(d, 7, testClusterConf(func(cc *pb.ClusterConf) {
		cc.DnBinConf = nil
	}))
	monitor := newDnMonitor(d, 7, 11, func() string { return "dn0:9520" })
	ctx := context.Background()

	monitor.observe(ctx, healthErrorRow, "disk")
	if got := writer.all(); len(got) != 0 {
		t.Fatalf("wrote %v under an unusable stored conf", got)
	}
	recs := logs.withMsg("health write failed")
	if len(recs) != 1 {
		t.Fatalf("%d health write failed records, want 1", len(recs))
	}
	if err, _ := recs[0]["error"].(string); !strings.Contains(
		err, "invalid stored conf",
	) {
		t.Fatalf("error = %q, want the stored-conf refusal", err)
	}
	if got := len(logs.withMsg(msgHealthChanged)); got != 0 {
		t.Fatalf("a refused write logged a health change")
	}
	// Nothing was remembered (HL3): a repaired conf makes the next round
	// write the transition it owed.
	seedClusterConf(d, 7)
	monitor.observe(ctx, healthErrorRow, "disk")
	writes := writer.all()
	if len(writes) != 1 || writes[0].record != healthRecordDn ||
		writes[0].epoch == 0 {
		t.Fatalf("writes after the conf was repaired = %v", writes)
	}
}

// TestHealthSpMonitorsCarryTheirIds pins the HL2 monitors' log identities,
// which the sp role (§8.4) reuses.
func TestHealthSpMonitorsCarryTheirIds(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	ctx := context.Background()

	newCntlrMonitor(d, 1, 2, 3).observe(ctx, healthUnreachable, "")
	newLegMonitor(d, 1, 2, 4, 5).observe(ctx, healthErrorRow, "leg")
	newSideMonitor(d, 1, 2, 4, 6).observe(ctx, healthErrorRow, "side")

	writes := writer.all()
	if len(writes) != 3 {
		t.Fatalf("writes = %v", writes)
	}
	if writes[0].record != healthRecordCntlr || writes[0].objId != 3 {
		t.Fatalf("cntlr write = %+v", writes[0])
	}
	if writes[1].record != healthRecordLeg || writes[1].sliceId != 4 ||
		writes[1].objId != 5 {
		t.Fatalf("leg write = %+v", writes[1])
	}
	if writes[2].record != healthRecordSide || writes[2].objId != 6 {
		t.Fatalf("side write = %+v", writes[2])
	}
	recs := logs.withMsg(msgHealthChanged)
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3", len(recs))
	}
	for _, rec := range recs {
		if rec["role"] != common.WorkerRoleSp {
			t.Fatalf("record role = %v, want sp (HL6)", rec["role"])
		}
		if _, has := rec["sp_id"]; !has {
			t.Fatalf("record without sp_id: %v", rec)
		}
	}
}

// TestHealthMarkUnknownClearsStaleErrors checks §9.5: while a stream is dead
// the worker records RES_STATUS_UNKNOWN on its own copy of the info, so a
// stale ERROR row does not outlive the stream that reported it.
func TestHealthMarkUnknownClearsStaleErrors(t *testing.T) {
	dn := &pb.DnInfo{DiskInfo: resErr("disk", "io")}
	markDnUnknown(dn)
	if dn.GetDiskInfo().GetStatus() != pb.ResStatus_RES_STATUS_UNKNOWN {
		t.Fatalf("disk status = %v", dn.GetDiskInfo().GetStatus())
	}
	if obs, _ := dnObservation(0, dn); obs != healthClean {
		t.Fatalf("obs after markUnknown = %v, want clean", obs)
	}
	cn := &pb.CnInfo{TmpfsInfo: resErr("tmpfs", "x")}
	markCnUnknown(cn)
	if obs, _ := cnObservation(0, cn); obs != healthClean {
		t.Fatalf("cn obs after markUnknown = %v", obs)
	}
	side := &pb.SideInfo{
		SideDevInfo:   resErr("side", "x"),
		CnIdToNvmeof:  map[uint64]*pb.ResInfo{1: resErr("nvmeof", "x")},
		CnIdToDmError: map[uint64]*pb.ResInfo{1: resErr("dmerr", "x")},
	}
	markSideUnknown(side)
	if obs, _ := sideObservation(0, side); obs != healthClean {
		t.Fatalf("side obs after markUnknown = %v", obs)
	}
	cntlr := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resErr("pool", "x")},
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			2: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				1: resErr("thin", "x"),
			}},
		},
	}
	markCntlrUnknown(cntlr)
	if obs, _ := cntlrObservation(0, cntlr); obs != healthClean {
		t.Fatalf("cntlr obs after markUnknown = %v", obs)
	}
}

// TestHealthMonitorAttrsAreStable guards the §12 attribute names the
// integration suite greps.
func TestHealthMonitorAttrsAreStable(t *testing.T) {
	d, _ := healthTestDeps(t)
	monitor := newDnMonitor(d, 1, 2, func() string { return "dn0:9520" })
	want := []string{"dn_id"}
	if len(monitor.attrs) != len(want) {
		t.Fatalf("attrs = %v", monitor.attrs)
	}
	for i, attr := range monitor.attrs {
		if attr.Key != want[i] {
			t.Fatalf("attr %d = %s, want %s", i, attr.Key, want[i])
		}
	}
	var _ slog.Attr = monitor.attrs[0]
}

// cntlrRowField is one map of ResInfo rows in a CntlrInfo, found by
// reflection: a top-level map, or one nested in td_id_to_thin_info's per-td
// message. name is the proto name of the map that holds the rows.
type cntlrRowField struct {
	name string
	// put stores res under key in this map: under td tdId for a nested map,
	// which a top-level map ignores. The row is stored, not copied.
	put func(info *pb.CntlrInfo, tdId uint64, key uint64, res *pb.ResInfo)
}

// cntlrRowFields walks CntlrInfo's descriptor rather than a hand-kept list,
// so a map added to the proto is covered by the tests below the day it
// lands: every field must be a uint64-keyed map of ResInfo, or of a message
// whose every field is one.
func cntlrRowFields(t *testing.T) []cntlrRowField {
	t.Helper()
	resInfo := (&pb.ResInfo{}).ProtoReflect().Descriptor().FullName()
	uint64Map := func(fd protoreflect.FieldDescriptor) bool {
		return fd.IsMap() && fd.MapKey().Kind() == protoreflect.Uint64Kind &&
			fd.MapValue().Message() != nil
	}
	rowMap := func(fd protoreflect.FieldDescriptor) bool {
		return uint64Map(fd) && fd.MapValue().Message().FullName() == resInfo
	}
	key := func(id uint64) protoreflect.MapKey {
		return protoreflect.ValueOfUint64(id).MapKey()
	}
	row := func(res *pb.ResInfo) protoreflect.Value {
		return protoreflect.ValueOfMessage(res.ProtoReflect())
	}
	var out []cntlrRowField
	fields := (&pb.CntlrInfo{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if rowMap(fd) {
			out = append(out, cntlrRowField{
				name: string(fd.Name()),
				put: func(
					info *pb.CntlrInfo, _ uint64, id uint64, res *pb.ResInfo,
				) {
					info.ProtoReflect().Mutable(fd).Map().Set(key(id), row(res))
				},
			})
			continue
		}
		if !uint64Map(fd) {
			t.Fatalf("CntlrInfo.%s is not a map of rows: teach this test",
				fd.Name())
		}
		nested := fd.MapValue().Message().Fields()
		for j := 0; j < nested.Len(); j++ {
			nfd := nested.Get(j)
			if !rowMap(nfd) {
				t.Fatalf("CntlrInfo.%s.%s is not a map of rows: teach this "+
					"test", fd.Name(), nfd.Name())
			}
			out = append(out, cntlrRowField{
				name: string(nfd.Name()),
				put: func(
					info *pb.CntlrInfo, tdId uint64, id uint64, res *pb.ResInfo,
				) {
					info.ProtoReflect().Mutable(fd).Map().Mutable(key(tdId)).
						Message().Mutable(nfd).Map().Set(key(id), row(res))
				},
			})
		}
	}
	return out
}

// TestHealthCntlrEveryMap pins the maps HL2 judges a cntlr by
// (cntlrHealthMaps) against CntlrInfo itself: an ERROR row in any map but
// leg_id_to_leg, the per-td thin maps included, is an error row, and a row
// with no res_name of its own is named by its map — so a map dropped from the
// list, or mislabelled, fails here, as does a map added to the proto and not
// to the list. The row is the second of its map, in the second td for a thin
// map: every row and every td is judged. A leg row never is (HL2's leg row).
func TestHealthCntlrEveryMap(t *testing.T) {
	for _, f := range cntlrRowFields(t) {
		info := &pb.CntlrInfo{}
		f.put(info, 3, 1, resOk("first"))
		f.put(info, 5, 2, &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_ERROR})
		obs, res := cntlrObservation(0, info)
		if f.name == "leg_id_to_leg" {
			if obs != healthClean {
				t.Errorf("%s: obs = %v, want clean", f.name, obs)
			}
			continue
		}
		if obs != healthErrorRow || res != f.name {
			t.Errorf("%s: obs = %v, res = %q, want an error row named %q",
				f.name, obs, res, f.name)
		}
	}
}

// TestHealthCntlrFirstErrorOrder pins which ERROR row HL2's verdict names
// when several are, and so the res_name of the §12 "health changed" record:
// the maps in CntlrInfo's field order, then the per-td thin maps by ascending
// td id, the lowest key first within a map. Each named row is cleared in turn
// and the next one read; map order is random, so the walk is repeated.
func TestHealthCntlrFirstErrorOrder(t *testing.T) {
	for round := 0; round < 3; round++ {
		info := &pb.CntlrInfo{}
		rows := make(map[string]*pb.ResInfo)
		var want []string
		put := func(f cntlrRowField, tdId uint64, id uint64, name string) {
			rows[name] = resErr(name, "x")
			f.put(info, tdId, id, rows[name])
		}
		var thin []cntlrRowField
		for _, f := range cntlrRowFields(t) {
			switch f.name {
			case "leg_id_to_leg":
				continue
			case "slice_id_to_dm_thin":
				thin = append(thin, f)
				continue
			}
			put(f, 0, 7, f.name+" 7")
			put(f, 0, 2, f.name+" 2")
			want = append(want, f.name+" 2", f.name+" 7")
		}
		if len(thin) != 1 {
			t.Fatalf("%d thin maps", len(thin))
		}
		for _, tdId := range []uint64{12, 3, 9, 5} {
			put(thin[0], tdId, 1, fmt.Sprintf("thin %d", tdId))
		}
		for _, tdId := range []uint64{3, 5, 9, 12} {
			want = append(want, fmt.Sprintf("thin %d", tdId))
		}
		for i, name := range want {
			obs, res := cntlrObservation(0, info)
			if obs != healthErrorRow || res != name {
				t.Fatalf("round %d, step %d: obs = %v, res = %q, want %q",
					round, i, obs, res, name)
			}
			rows[name].Status = pb.ResStatus_RES_STATUS_OK
		}
		if obs, res := cntlrObservation(0, info); obs != healthClean {
			t.Fatalf("round %d: obs = %v (%q) with every row cleared", round,
				obs, res)
		}
	}
}

// TestHealthMarkCntlrUnknownCoversEveryMap pins markCntlrUnknown's list
// against CntlrInfo itself: a dead stream leaves no row of any map, leg rows
// and every td's thin rows included, at its last reported status (§9.5).
func TestHealthMarkCntlrUnknownCoversEveryMap(t *testing.T) {
	for _, f := range cntlrRowFields(t) {
		info := &pb.CntlrInfo{}
		first, second := resErr("first", "x"), resErr("second", "x")
		f.put(info, 3, 1, first)
		f.put(info, 5, 2, second)
		markCntlrUnknown(info)
		for _, res := range []*pb.ResInfo{first, second} {
			if res.GetStatus() != pb.ResStatus_RES_STATUS_UNKNOWN {
				t.Errorf("%s: %s reads %v after markCntlrUnknown", f.name,
					res.GetResName(), res.GetStatus())
			}
		}
	}
}

// TestPrimaryShapeBuilt pins which rows keep HL2's settle waiting, against
// CntlrInfo itself: a PROVISIONING row, or a MISSING row whose details are
// not CN19's "sp_level", in any map HL2 judges a cntlr by, the per-td thin
// maps included, except grp_id_to_md_raid, where a grow's group reads
// PROVISIONING beside a serving pool; never a leg row (a zeroing spare); and
// never a row of any other status, or a MISSING "sp_level" one, in any map.
func TestPrimaryShapeBuilt(t *testing.T) {
	fields := cntlrRowFields(t)
	notJudged := map[string]bool{
		"leg_id_to_leg":     true,
		"grp_id_to_md_raid": true,
	}
	status := func(s pb.ResStatus) *pb.ResInfo { return &pb.ResInfo{Status: s} }
	missing := func(details string) *pb.ResInfo {
		return &pb.ResInfo{
			Status: pb.ResStatus_RES_STATUS_MISSING, Details: details,
		}
	}
	ok := pb.ResStatus_RES_STATUS_OK
	prov := pb.ResStatus_RES_STATUS_PROVISIONING
	if !primaryShapeBuilt(&pb.CntlrInfo{}) {
		t.Errorf("empty: not built")
	}
	// Every other status, in every map at once: built. MISSING is read with
	// CN19's details, the one MISSING that says a row must not exist.
	values := prov.Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		s := pb.ResStatus(values.Get(i).Number())
		if s == prov {
			continue
		}
		info := &pb.CntlrInfo{}
		for _, f := range fields {
			res := func() *pb.ResInfo { return status(s) }
			if s == pb.ResStatus_RES_STATUS_MISSING {
				res = func() *pb.ResInfo { return missing("sp_level") }
			}
			f.put(info, 3, 1, res())
			f.put(info, 5, 2, res())
		}
		if !primaryShapeBuilt(info) {
			t.Errorf("%v in every map: not built", s)
		}
	}
	// One holding row in each map in turn, the third of four keys — in the
	// second td for a thin map — beside OK rows in every map: PROVISIONING,
	// the probe's MISSING "" for a wanted device it finds absent, and a
	// converge's MISSING with details of its own. Map order is random, so
	// each reading is taken several times.
	holds := []struct {
		label string
		res   func() *pb.ResInfo
	}{
		{"PROVISIONING", func() *pb.ResInfo { return status(prov) }},
		{`MISSING ""`, func() *pb.ResInfo { return missing("") }},
		{`MISSING "thin pool missing"`, func() *pb.ResInfo {
			return missing("thin pool missing")
		}},
	}
	for _, hold := range holds {
		for _, f := range fields {
			info := &pb.CntlrInfo{}
			for _, other := range fields {
				other.put(info, 3, 1, status(ok))
			}
			for id := uint64(1); id <= 4; id++ {
				if id == 3 {
					f.put(info, 5, id, hold.res())
				} else {
					f.put(info, 5, id, status(ok))
				}
			}
			want := notJudged[f.name]
			for i := 0; i < 8; i++ {
				if got := primaryShapeBuilt(info); got != want {
					t.Fatalf("%s %s: primaryShapeBuilt = %v, want %v",
						f.name, hold.label, got, want)
				}
			}
		}
	}
	// The two shapes the group exclusion tells apart: a new SP's group,
	// whose slice is deferred with it, and a grow's, beside a serving pool.
	newSp := &pb.CntlrInfo{
		GrpIdToMdRaid:   map[uint64]*pb.ResInfo{1: status(prov)},
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: status(prov)},
	}
	if primaryShapeBuilt(newSp) {
		t.Errorf("a new SP's deferred slice reads built")
	}
	grow := &pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{
			1: status(ok), 2: status(prov),
		},
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: status(ok)},
	}
	if !primaryShapeBuilt(grow) {
		t.Errorf("a grow's zeroing group holds the settle")
	}
	// The two MISSING shapes: a td-less primary probed between a converge
	// that left its members unavailable and the retry — groups, concats and
	// pool absent, no ERROR row anywhere — and a primary whose sp_level
	// suppresses its pools.
	unbuilt := &pb.CntlrInfo{
		GrpIdToMdRaid:   map[uint64]*pb.ResInfo{1: missing(""), 2: missing("")},
		SliceIdToMeta:   map[uint64]*pb.ResInfo{1: missing("")},
		SliceIdToData:   map[uint64]*pb.ResInfo{1: missing("")},
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: missing("")},
		SsIdToSubsystem: map[uint64]*pb.ResInfo{1: status(ok)},
	}
	if primaryShapeBuilt(unbuilt) {
		t.Errorf("a td-less primary's absent pools read built")
	}
	suppressed := &pb.CntlrInfo{
		GrpIdToMdRaid:   map[uint64]*pb.ResInfo{1: missing("sp_level")},
		SliceIdToMeta:   map[uint64]*pb.ResInfo{1: missing("sp_level")},
		SliceIdToData:   map[uint64]*pb.ResInfo{1: missing("sp_level")},
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: missing("sp_level")},
		SsIdToSubsystem: map[uint64]*pb.ResInfo{1: status(ok)},
	}
	if !primaryShapeBuilt(suppressed) {
		t.Errorf("a primary whose sp_level suppresses its pools reads " +
			"unbuilt")
	}
}
