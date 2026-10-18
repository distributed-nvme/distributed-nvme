package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// msgDnFreeCountDisagrees is the dn role's count record (dnv-worker.md, Log
// records; RW13). The string is normative, so it is a constant and never
// formatted.
const msgDnFreeCountDisagrees = "dn free count disagrees"

// dnCountsRun is how many round compares in a row must find a disk node's own
// free count other than its record's before the role logs it (RW13), a
// sibling of AR10's failoverRun. One compare can disagree with no fault
// anywhere: while two owners overlap (VW7), the other one's syncup can hand
// the agent this revision with a side pointer list read after this role's
// own read of the record, until the DnRev watch brings the newer revision.
const dnCountsRun = 2

// dnDriver is the dn role's half of the per-object loop (RW13). One
// per DN: cluster_id and dn_id come from the DnRev key, addr_port and
// revision from its value.
type dnDriver struct {
	deps *deps
	cid  uint64
	dnId uint64

	addr     string
	revision uint64

	// mu guards lastInfo, the latest DnInfo the agent reported (RW2), and
	// infoRev, the revision of the reply that carried it. Health is
	// evaluated on lastInfo, not only on the info a given reply carried
	// (HL5).
	//
	// A CheckDn reply is decoded by fold on the STREAM'S PUMP goroutine
	// (dnCheckStream.recv) and a SyncupDn reply by fold on the LOOP's, while
	// the loop reads the field in observe and rewrites the message's rows in
	// place in unreachable (architecture.md, Live-state reporting). They
	// overlap on the RW4 step 4 path: dropStream does not join the pump, so a
	// late reply can still be inside fold while fail() reports the object
	// unreachable. Unguarded, that loses the round's ERROR rows and HL1 never
	// sets err_epoch.
	mu       sync.Mutex
	lastInfo *pb.DnInfo
	infoRev  uint64

	health *healthMonitor

	// The count compare of RW13, owned by the loop goroutine (RW1) like the
	// health monitor that feeds it. rec is a cache of the record: its extent
	// counts and the DnRev revision read with them, as the last read of the
	// pair found them (noteRecord), nil while that read found no record of
	// this dn. disagree is the run of round compares in a row that found the
	// agent's free count other than the record's, and logged whether that
	// run has been logged; both are memos of observations (RW2).
	rec      *dnRecordCounts
	disagree int
	logged   bool
}

// dnRecordCounts is a DnConf's extent counts with the revision of the DnRev
// read in the same snapshot (readDnRecord). Every transaction that changes a
// DN's free_ext_cnt also changes its side_ptr_list and bumps its DnRev
// (architecture.md, Per-operation allocation), so one revision of a DN has
// one free count.
type dnRecordCounts struct {
	free     uint64
	total    uint64
	revision uint64
}

// storeInfo remembers the latest DnInfo (HL5) and the revision of the reply
// that carried it; info and infoAt return them. See the mu comment above for
// why they are guarded.
func (d *dnDriver) storeInfo(info *pb.DnInfo, revision uint64) {
	d.mu.Lock()
	d.lastInfo = info
	d.infoRev = revision
	d.mu.Unlock()
}

func (d *dnDriver) info() *pb.DnInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastInfo
}

func (d *dnDriver) infoAt() (*pb.DnInfo, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastInfo, d.infoRev
}

// markInfoUnknown is the "no answer from the node" of architecture.md,
// Live-state reporting, applied in place to the last known info, under the lock
// a concurrent fold takes (HL1).
func (d *dnDriver) markInfoUnknown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	markDnUnknown(d.lastInfo)
}

// newDnDriver builds the dn driver of one revision worker (RW13). host is the
// generic loop this driver belongs to; a DN drives no bitmap pushes and no
// flips, so it never calls back into it (unlike the sp children, RW14-RW20).
func newDnDriver(p revWorkerParams, host *revWorker) objDriver {
	d := &dnDriver{
		deps:     p.deps,
		cid:      p.cid,
		dnId:     p.id,
		addr:     p.desired.handle,
		revision: p.desired.revision,
	}
	d.health = newDnMonitor(p.deps, p.cid, p.id, d.addrPort, d.noteRecord)
	return d
}

// logAttrs are the DN's ids as the records of dnv-worker.md, Log records, carry
// them.
func (d *dnDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("dn_id", d.dnId),
	}
}

// childAttrs is empty: only the sp children carry a pointer attribute.
func (d *dnDriver) childAttrs() []slog.Attr {
	return nil
}

// addrPort is the DN agent's endpoint (RW13).
func (d *dnDriver) addrPort() string {
	return d.addr
}

// interval is the DN round period, health_check_conf.dn_interval of the
// resolved cluster conf (RW9).
func (d *dnDriver) interval(cc *pb.ClusterConf) time.Duration {
	return roundPeriod(cc.GetHealthCheckConf().GetDnInterval())
}

// setDesired installs a new revision and endpoint (RW3). A put whose only
// change is addr_port re-syncs the node at its new endpoint: the loop's
// connect drops the stream and the connection reference and continues there
// (RW13; architecture.md, dn / cn roles). No delete ever reaches the agent.
func (d *dnDriver) setDesired(next desiredState) {
	d.revision = next.revision
	d.addr = next.handle
}

// openStream opens the DN's CheckDn stream (RW4 step 1, architecture.md, Check
// streams).
func (d *dnDriver) openStream(
	ctx context.Context,
	conn *grpc.ClientConn,
) (checkStream, error) {
	stream, err := pb.NewDiskNodeAgentClient(conn).CheckDn(ctx)
	if err != nil {
		return nil, fmt.Errorf("check dn stream: %w", err)
	}
	return &dnCheckStream{driver: d, stream: stream}, nil
}

// syncup issues one SyncupDn (RW5, RW13). Per syncup it reads the DN's
// DnConf at DnConfKey(cluster_id, addr_port) together with its DnRev, both at
// one store revision (readDnRecord); a missing record is logged and skipped,
// and the next round retries. A read that fails fails the syncup with its
// error, which the loop logs as the syncup's result and leaves to the next
// round, as it does a failed call. The read is a load of the DN's record, so
// it re-seeds the health memo (HL3): an err_epoch another observer wrote, or
// cleared, since this monitor's last write makes the verdict on this
// syncup's reply a transition when the two disagree. It also replaces the
// record the count compare holds (noteRecord). A quiet DN never syncs; its
// monitor's refresh reads the record instead.
func (d *dnDriver) syncup(
	ctx context.Context,
	conn *grpc.ClientConn,
	cc *pb.ClusterConf,
) (*replyState, error) {
	dnConf, revision, found, revFound, err := readDnRecord(
		ctx, d.deps.store, d.cid, d.addr,
	)
	if err != nil {
		// No SyncupDn was issued, but the syncup failed all the same: its
		// result names the read's error (RW13), and the next round retries
		// (RW12).
		return nil, fmt.Errorf("read dn record: %w", err)
	}
	d.noteRecord(dnConf, revision, found, revFound)
	if !found {
		slog.InfoContext(ctx, "dn conf missing",
			slog.Uint64("cluster_id", d.cid),
			slog.Uint64("dn_id", d.dnId),
			slog.String("addr_port", d.addr),
		)
		return nil, nil
	}
	d.health.seedRecord(dnConf.GetErrEpoch())
	reply, err := pb.NewDiskNodeAgentClient(conn).SyncupDn(
		ctx, dnSyncupRequest(d.cid, d.dnId, d.revision, dnConf, cc),
	)
	if err != nil {
		return nil, fmt.Errorf("syncup dn: %w", err)
	}
	return d.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetDnInfo(),
		reply.GetObsStamp(), reply.GetSaveTime(),
	), nil
}

// observe folds one CheckDn/SyncupDn reply into the DN's health (HL1, HL4),
// then into the count compare (RW13). The compare comes second because the
// health verdict can re-read the record first (refresh), and a re-read
// replaces the record the compare holds.
func (d *dnDriver) observe(ctx context.Context, r *replyState) {
	obs, res := dnObservation(r.code, d.info())
	d.health.observe(ctx, obs, res)
	d.compareCounts(ctx, r)
}

// readDnRecord reads the DnConf at DnConfKey(cid, addrPort) and, when there
// is one, the DnRev of the dn it names, in one snapshot, so the revision
// belongs to the record (RW13). found says whether the DnConf exists and
// revFound whether its DnRev does. Both the syncup's read and the health
// monitor's refresh of a DN go through it.
func readDnRecord(
	ctx context.Context,
	store etcdStore,
	cid uint64,
	addrPort string,
) (conf *pb.DnConf, revision uint64, found bool, revFound bool, err error) {
	_, err = store.SnapshotRev(ctx, func(s etcdutil.STM) error {
		conf, revision, found, revFound = &pb.DnConf{}, 0, false, false
		found = s.Get(model.DnConfKey(cid, addrPort), conf)
		if !found {
			return nil
		}
		rev := &pb.DnRev{}
		revFound = s.Get(
			model.DnRevKey(conf.GetShardCode(), cid, conf.GetDnId()), rev,
		)
		revision = rev.GetRevision()
		return nil
	})
	if err != nil {
		return nil, 0, false, false, err
	}
	return conf, revision, found, revFound, nil
}

// noteRecord replaces the record the count compare holds with what one read
// of the DnConf and its DnRev found (readDnRecord): its counts, or nil when
// the read found no DnConf of this dn or no DnRev with it. Only a read that
// answered gets here, so a failed read leaves the cache as it is. It runs on
// the loop goroutine, from the syncup and from the health monitor's refresh.
func (d *dnDriver) noteRecord(
	conf *pb.DnConf,
	revision uint64,
	found bool,
	revFound bool,
) {
	if !found || !revFound || conf.GetDnId() != d.dnId {
		d.rec = nil
		return
	}
	d.rec = &dnRecordCounts{
		free:     conf.GetFreeExtCnt(),
		total:    conf.GetTotalExtCnt(),
		revision: revision,
	}
}

// compareCounts is RW13's count compare, run on a round's accepted reply
// that answers from an observation new to this loop (RW23): the free extents
// of the disk counts in the latest known info (HL5) against the record's
// free_ext_cnt. It compares only when the counts are present and say the side
// records match the side pointer list, and when the revision of the reply
// that carried them, this round's reply revision and the revision read with
// the record are one: at one revision a DN has one list and one free count
// (dnRecordCounts). Any other reply neither counts nor resets the run — a
// round repeating an observation already seen among them, since counting it
// again would count one look twice. A compare that agrees ends the run; the
// dnCountsRun-th disagreement in a row is logged once, until a compare
// agrees. Nothing is written.
func (d *dnDriver) compareCounts(ctx context.Context, r *replyState) {
	if !r.fromRound || !r.newObs || !accepted(r.code) {
		return
	}
	info, infoRev := d.infoAt()
	counts := info.GetDiskCounts()
	rec := d.rec
	if counts == nil || !counts.GetSideRecsMatch() || rec == nil ||
		infoRev != rec.revision || r.revision != rec.revision {
		return
	}
	if counts.GetFreeExtCnt() == rec.free {
		d.disagree = 0
		d.logged = false
		return
	}
	d.disagree++
	if d.disagree < dnCountsRun || d.logged {
		return
	}
	d.logged = true
	slog.ErrorContext(ctx, msgDnFreeCountDisagrees,
		slog.String("role", common.WorkerRoleDn),
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("dn_id", d.dnId),
		slog.String("addr_port", d.addr),
		slog.Uint64("revision", rec.revision),
		slog.Uint64("free_ext_cnt", rec.free),
		slog.Uint64("agent_free_ext_cnt", counts.GetFreeExtCnt()),
		slog.Uint64("total_ext_cnt", rec.total),
		slog.Uint64("agent_total_ext_cnt", counts.GetTotalExtCnt()),
		slog.Uint64("side_rec_cnt", counts.GetSideRecCnt()),
	)
}

// unreachable folds a broken stream, a missed reply or a reply whose
// observation is too old (HL7) into the DN's health (HL1). The in-memory info
// is marked RES_STATUS_UNKNOWN — what the worker records itself while the
// stream is dead (architecture.md, Live-state reporting) — and never written
// to etcd.
func (d *dnDriver) unreachable(ctx context.Context) {
	d.markInfoUnknown()
	d.health.observe(ctx, healthUnreachable, "")
}

// fold turns one reply into a replyState, remembering the DnInfo it carried
// with the reply's revision (HL5: a show_info = false reply carries one only
// when something changed; RW13 pairs the counts with that revision). stamp
// is the reply's observation stamp and saveTime a SyncupDn reply's save time,
// nil on a round's reply.
func (d *dnDriver) fold(
	agentReply *pb.AgentReply,
	revision uint64,
	info *pb.DnInfo,
	stamp *pb.ObsStamp,
	saveTime *timestamppb.Timestamp,
) *replyState {
	if info != nil {
		d.storeInfo(info, revision)
	}
	return newReplyState(agentReply, revision, info != nil, stamp, saveTime)
}

// dnCheckStream adapts the generated CheckDn stream to checkStream (RW4).
type dnCheckStream struct {
	driver *dnDriver
	stream grpc.BidiStreamingClient[pb.CheckDnRequest, pb.CheckDnReply]
}

func (s *dnCheckStream) send(
	traceId string, revision uint64, showInfo bool,
) error {
	req := dnCheckRequest(s.driver.cid, s.driver.dnId, revision, showInfo)
	req.TraceId = traceId
	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("check dn send: %w", err)
	}
	return nil
}

func (s *dnCheckStream) recv() (*replyState, error) {
	reply, err := s.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("check dn recv: %w", err)
	}
	return s.driver.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetDnInfo(),
		reply.GetObsStamp(), nil,
	), nil
}

func (s *dnCheckStream) closeSend() error {
	return s.stream.CloseSend()
}

// dnSyncupRequest builds the SyncupDn request of RW13: the side pointer list
// is DnConf's, authoritative and complete (architecture.md, Common agent
// rules), and extent_size is the STORED dn_bin_conf's, validated by the loop's
// pass gate before this is ever built (architecture.md, Common validation).
// Nothing here substitutes a default, and it matters more here than anywhere:
// the dn agent formats every disk header with this number (architecture.md,
// Disk node), so shipping a zero — or a constant this binary happens to carry —
// would be a geometry the rest of the cluster does not share.
func dnSyncupRequest(
	cid uint64,
	dnId uint64,
	revision uint64,
	dnConf *pb.DnConf,
	cc *pb.ClusterConf,
) *pb.SyncupDnRequest {
	return &pb.SyncupDnRequest{
		ClusterId:       cid,
		DnId:            dnId,
		Revision:        revision,
		SidePointerList: dnConf.GetSidePtrList(),
		ExtentSize:      cc.GetDnBinConf().GetExtentSize(),
	}
}

// dnCheckRequest builds one CheckDn round request (RW4 step 2, RW13).
func dnCheckRequest(
	cid uint64,
	dnId uint64,
	revision uint64,
	showInfo bool,
) *pb.CheckDnRequest {
	return &pb.CheckDnRequest{
		ClusterId: cid,
		DnId:      dnId,
		Revision:  revision,
		ShowInfo:  showInfo,
	}
}
