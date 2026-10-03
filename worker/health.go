package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The "record" attribute of the "health changed" record (HL1/HL2;
// dnv-worker.md, Log records).
const (
	healthRecordDn    = "dn"
	healthRecordCn    = "cn"
	healthRecordCntlr = "cntlr"
	healthRecordLeg   = "leg"
	healthRecordSide  = "side"
)

// The "reason" attribute of the "health changed" record (dnv-worker.md, Log
// records).
const (
	reasonUnreachable = "unreachable"
	reasonErrorRow    = "error_row"
	reasonRecovered   = "recovered"
)

// healthObs is one health observation about one object, i.e. one row of the
// HL1/HL2 tables.
type healthObs int

const (
	// healthNone is "neither set nor clear": the reply said nothing about the
	// object's health. Two things produce it — a REJECTED agent_reply (HL1's
	// last row and HL2's trailer), which triggers a re-sync instead (RW4), and
	// a leg probe row that is neither ERROR nor OK, or absent (HL2's Leg row
	// clears on an explicit RES_STATUS_OK and on nothing else, legObservation
	// below). Among them RES_STATUS_PENDING, a leg whose prober has not
	// completed a round yet (cnagent.md CN11), which is what a just-promoted
	// primary reports for its wrapped legs: clearing on it would wipe a dead
	// leg's err_epoch at every promotion.
	//
	// RES_STATUS_PROVISIONING and MISSING rows are deliberately NOT healthNone
	// for a node, a side or a cntlr: they are simply not ERROR rows, so a
	// reply carrying them is a clean round (HL1 row 3, "no ERROR row in the
	// latest known info"). architecture.md, Live-state reporting, makes
	// PROVISIONING healthy ([D15]), and all HL1 row 4 and HL2's trailer forbid
	// is SETTING an err_epoch — which healthClean never does.
	healthNone healthObs = iota
	// healthUnreachable is a stream that cannot be opened, breaks, or misses
	// its reply within the round timeout.
	healthUnreachable
	// healthErrorRow is a RES_STATUS_ERROR row in the object's latest known
	// info.
	healthErrorRow
	// healthClean is a clean round: reply in time, an accepted code, no
	// ERROR row in the latest known info (HL5).
	healthClean
)

// ---------------------------------------------------------------------------
// The etcd half (HL3, MD6)
// ---------------------------------------------------------------------------

// healthWriter is the model surface health.go writes through. It exists as an
// interface so the HL1/HL2 tables can be unit-tested without etcd; the
// production implementation is modelHealthWriter and does nothing but call
// model, whose ops re-read the record inside their own STM (HL3).
type healthWriter interface {
	setDnErrEpoch(
		ctx context.Context,
		cid uint64,
		addrPort string,
		epoch uint64,
		cc *pb.ClusterConf,
	) error
	setCnErrEpoch(
		ctx context.Context,
		cid uint64,
		addrPort string,
		epoch uint64,
	) error
	setCntlrErrEpoch(
		ctx context.Context,
		cid uint64,
		spId uint64,
		cntlrId uint64,
		epoch uint64,
		settle bool,
	) error
	setLegErrEpoch(
		ctx context.Context,
		cid uint64,
		spId uint64,
		sliceId uint64,
		legId uint64,
		epoch uint64,
	) error
	setSideErrEpoch(
		ctx context.Context,
		cid uint64,
		spId uint64,
		sliceId uint64,
		sideId uint64,
		epoch uint64,
	) error
}

// modelHealthWriter is the production healthWriter (HL1/HL2 -> MD6).
type modelHealthWriter struct {
	cli *etcdutil.Client
}

func (w *modelHealthWriter) setDnErrEpoch(
	ctx context.Context,
	cid uint64,
	addrPort string,
	epoch uint64,
	cc *pb.ClusterConf,
) error {
	return model.SetDnErrEpoch(ctx, w.cli, cid, addrPort, epoch, cc)
}

func (w *modelHealthWriter) setCnErrEpoch(
	ctx context.Context,
	cid uint64,
	addrPort string,
	epoch uint64,
) error {
	return model.SetCnErrEpoch(ctx, w.cli, cid, addrPort, epoch)
}

func (w *modelHealthWriter) setCntlrErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	cntlrId uint64,
	epoch uint64,
	settle bool,
) error {
	return model.SetCntlrErrEpoch(
		ctx, w.cli, cid, spId, cntlrId, epoch, settle,
	)
}

func (w *modelHealthWriter) setLegErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	sliceId uint64,
	legId uint64,
	epoch uint64,
) error {
	return model.SetLegErrEpoch(ctx, w.cli, cid, spId, sliceId, legId, epoch)
}

func (w *modelHealthWriter) setSideErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	sliceId uint64,
	sideId uint64,
	epoch uint64,
) error {
	return model.SetSideErrEpoch(ctx, w.cli, cid, spId, sliceId, sideId, epoch)
}

// ---------------------------------------------------------------------------
// The transitions-only monitor (HL3)
// ---------------------------------------------------------------------------

// healthMonitor is one object's health bookkeeping (HL3). Its memo is a cache
// of the object's record: the state it last wrote, re-seeded from the record
// by the loads HL3 names (seedRecord, offerRecord, refresh). It issues an etcd
// write only on an observed transition from that memo — or, for a cntlr, for
// HL2's settle, once per memo (see observeSettle). Two owners of the same
// object (the accepted overlap of VW7) that observe the same transition
// therefore write at most once each, and the model op re-reads the record
// inside its STM so the second write is a no-op — the threshold clock of the
// automatic reactions (AR4) never restarts. An epoch written by an owner that
// saw something else, an observer cut off from the agent say, is put right by
// the owner that remains, at its first verdict after its next load.
//
// A monitor is owned by one object goroutine (RW1) and needs no locking but
// for the offered record and the count of its own writes, which the sp
// coordinator fills and reads from its own.
type healthMonitor struct {
	deps   *deps
	role   string
	record string
	cid    uint64
	// attrs are the object's ids as the "health changed" record (dnv-worker.md,
	// Log records) carries them, between cluster_id and record.
	attrs []slog.Attr
	// write performs the MD6 op for this record kind. settle asks a cntlr's
	// op to clear the record's settling flag as well (HL2); the other kinds
	// have no such flag and ignore it.
	write func(ctx context.Context, epoch uint64, settle bool) error
	// load reads the object's record for a dn or cn monitor (refresh). Its
	// only other load is the syncup's read, and a node whose revision stays
	// put and whose agent keeps answering code 0 never syncs. nil for the sp
	// kinds, which the sp coordinator's passes re-seed.
	load func(ctx context.Context) (errEpoch uint64, found bool, err error)

	known     bool
	unhealthy bool
	// settlePending is a cntlr's memo of its record's settling flag (HL2):
	// seeded from every plan the driver takes, cleared by the settle write.
	// Always false for the other kinds.
	settlePending bool
	// loaded is when the memo last took the record's state: a read of the
	// record (seedRecord) or a write of its own, after which the record holds
	// what the memo does. refresh reads the record again once it is
	// nodeRecordMaxAge old.
	loaded time.Time

	// offerMu guards the four fields below. offered is the record's err_epoch
	// as the sp coordinator's latest pass loaded it (offerRecord), which the
	// owning goroutine folds into the memo before its next observation
	// (takeOffer). writeSeq counts the monitor's successful writes; the pass
	// reads it before its load (loadSeq) and hands it back as offerSeq, so an
	// offer whose load may predate the monitor's own latest write is dropped
	// instead of folded in.
	offerMu  sync.Mutex
	offered  uint64
	offerSeq uint64
	hasOffer bool
	writeSeq uint64
}

// nodeRecordMaxAge is how long a dn or cn monitor's memo may go without
// reading or writing the node's record before a verdict re-reads it (HL3,
// refresh). The syncup's read covers a node whose revision moves; this one
// covers a quiet node, which never syncs — a DN another observer marked
// unhealthy loses its capacity key with the mark (MD4), so nothing is
// allocated on it and its revision stays put.
const nodeRecordMaxAge = time.Minute

// seedRecord re-seeds the memo from the object's stored record (HL3). The memo
// caches the record rather than only remembering the monitor's own writes: a
// load that finds the record holding what this monitor did not write — another
// observer's err_epoch, or its clear — makes the next verdict that disagrees
// with it a transition, and that write puts the record right. An empty memo
// stays empty: a monitor that has written nothing yet writes its first verdict
// whatever the record holds. The load must not predate the monitor's own
// latest write. One on the monitor's own goroutine cannot — the syncup's read,
// refresh's, the sp coordinator's pass for the leg monitors it keeps — and
// offerRecord guards one on another goroutine.
func (m *healthMonitor) seedRecord(errEpoch uint64) {
	m.loaded = m.deps.clk.now()
	if !m.known {
		return
	}
	m.unhealthy = errEpoch != 0
}

// loadSeq is the count of the monitor's own writes, which the sp coordinator
// reads before the load whose record it then offers (offerRecord).
func (m *healthMonitor) loadSeq() uint64 {
	m.offerMu.Lock()
	defer m.offerMu.Unlock()
	return m.writeSeq
}

// offerRecord is seedRecord for a caller on another goroutine: the sp
// coordinator hands every side and cntlr child its record's err_epoch as each
// reaction pass loads it, with the loadSeq it read before that load, and the
// child folds it in before it judges its next reply or missed round. A newer
// offer replaces one not yet folded in. An offer whose load may predate the
// monitor's own latest write — the count moved since — is dropped: folded in,
// it would hand back the state that write replaced, so a next verdict
// reversing the write would write nothing and one repeating it would write
// again. The next pass offers again.
func (m *healthMonitor) offerRecord(errEpoch uint64, seq uint64) {
	m.offerMu.Lock()
	m.offered, m.offerSeq, m.hasOffer = errEpoch, seq, true
	m.offerMu.Unlock()
}

// takeOffer folds the record last offered into the memo, on the owning
// goroutine (RW1), before an observation is compared with it.
func (m *healthMonitor) takeOffer() {
	m.offerMu.Lock()
	errEpoch := m.offered
	ok := m.hasOffer && m.offerSeq == m.writeSeq
	m.hasOffer = false
	m.offerMu.Unlock()
	if ok {
		m.seedRecord(errEpoch)
	}
}

// refresh re-reads a dn or cn monitor's record before a verdict once the memo
// has gone nodeRecordMaxAge without reading or writing it (HL3). A monitor
// that has written nothing yet needs none: its first verdict is written
// whatever the record holds. A read that fails leaves the memo as it is, and
// the next verdict retries (RW12); an absent record re-seeds nothing.
func (m *healthMonitor) refresh(ctx context.Context) {
	if m.load == nil || !m.known ||
		m.deps.clk.now().Sub(m.loaded) < nodeRecordMaxAge {
		return
	}
	errEpoch, found, err := m.load(ctx)
	if err != nil {
		// The "etcd get" record carries the error.
		return
	}
	if !found {
		m.loaded = m.deps.clk.now()
		return
	}
	m.seedRecord(errEpoch)
}

// observe folds one observation into the object's health (HL1/HL2, HL3).
func (m *healthMonitor) observe(
	ctx context.Context,
	obs healthObs,
	resName string,
) {
	m.observeSettle(ctx, obs, resName, false)
}

// observeSettle folds one observation into the object's health (HL1/HL2,
// HL3) and, for a cntlr, clears its settling flag on a clean observation
// that proves the primary role: canSettle is the driver's judgement that the
// reply describes the PRIMARY shape, built (primaryShapeBuilt), at the
// driven revision. It reports whether the flag was cleared. A settle is
// written even without a health transition, because a standby that was
// clean, is promoted and reports clean at once has no edge for HL3 to write
// on.
func (m *healthMonitor) observeSettle(
	ctx context.Context,
	obs healthObs,
	resName string,
	canSettle bool,
) bool {
	m.takeOffer()
	var unhealthy bool
	var reason string
	switch obs {
	case healthUnreachable:
		unhealthy, reason = true, reasonUnreachable
	case healthErrorRow:
		unhealthy, reason = true, reasonErrorRow
	case healthClean:
		unhealthy, reason = false, reasonRecovered
	default:
		// HL1/HL2 (healthNone): a rejected code, and a leg row that is
		// neither ERROR nor OK or is absent (legObservation), neither set
		// nor clear.
		return false
	}
	m.refresh(ctx)
	transition := !(m.known && m.unhealthy == unhealthy)
	doSettle := canSettle && !unhealthy && m.settlePending
	if !transition && !doSettle {
		return false
	}
	var epoch uint64
	if unhealthy {
		epoch = m.deps.clk.nowUnix()
	}
	if err := m.write(ctx, epoch, doSettle); err != nil {
		// Left for the next round (RW12): nothing is remembered, so the
		// transition — and the settle — is retried. The etcdutil records
		// carry the details.
		slog.ErrorContext(ctx, "health write failed",
			slog.String("role", m.role),
			slog.Uint64("cluster_id", m.cid),
			slog.String("record", m.record),
			slog.String("error", err.Error()),
		)
		return false
	}
	// The count moves only once the write has landed, under the lock loadSeq
	// takes: a pass that reads it after this began its load after the write,
	// so its record holds the write, and an offer from one that read it
	// before is dropped (takeOffer).
	m.offerMu.Lock()
	m.writeSeq++
	m.offerMu.Unlock()
	m.known = true
	m.unhealthy = unhealthy
	m.loaded = m.deps.clk.now()
	if doSettle {
		m.settlePending = false
	}
	if !transition {
		return doSettle
	}

	attrs := make([]any, 0, len(m.attrs)+6)
	attrs = append(attrs,
		slog.String("role", m.role),
		slog.Uint64("cluster_id", m.cid),
	)
	for _, attr := range m.attrs {
		attrs = append(attrs, attr)
	}
	attrs = append(attrs,
		slog.String("record", m.record),
		slog.Uint64("err_epoch", epoch),
		slog.String("reason", reason),
	)
	if resName != "" && unhealthy {
		attrs = append(attrs, slog.String("res_name", resName))
	}
	slog.InfoContext(ctx, msgHealthChanged, attrs...)
	return doSettle
}

// ---------------------------------------------------------------------------
// HL1 — the dn and cn node tables
// ---------------------------------------------------------------------------

// errNoClusterConf is why a DN health write is skipped while its cluster is
// absent from the RW21 cache. MD4 derives the DnCapacity key's bin index from
// dn_bin_conf, so a write with no usable conf would have to invent a ladder:
// the real key would never be deleted and a duplicate would be written at the
// wrong bin, which the bin scan of architecture.md, Finding DN candidates,
// would then hand out as an allocation candidate for a DN HL1 has just flagged
// unhealthy. An UNUSABLE stored conf is refused the same way and for exactly
// the same reason (architecture.md, Common validation).
var errNoClusterConf = errors.New("cluster conf missing")

// newDnMonitor builds the health monitor of one DN (HL1). The cluster conf the
// capacity key maintenance needs (MD4) is read per write rather than captured,
// because a cluster conf change must reach the next write — and a conf that
// has been deleted, or that cannot be used, between the loop's RW9 gate and
// this write makes the write wait for the next round (RW12) rather than guess.
// It re-validates rather than trusting the loop's gate because it is not
// COVERED by one: the gate checked the conf the round was built from, while
// this closure uses a conf re-read from the cache per write, which the watch
// goroutine may have replaced in between. Its refresh reads the DnConf the
// syncup reads (RW13).
func newDnMonitor(
	d *deps,
	cid uint64,
	dnId uint64,
	addrPort func() string,
) *healthMonitor {
	return &healthMonitor{
		deps:   d,
		role:   common.WorkerRoleDn,
		record: healthRecordDn,
		cid:    cid,
		attrs:  []slog.Attr{slog.Uint64("dn_id", dnId)},
		write: func(ctx context.Context, epoch uint64, _ bool) error {
			cc, ok := d.conf.get(cid)
			if !ok {
				return fmt.Errorf(
					"dn err_epoch cluster %016x: %w", cid, errNoClusterConf,
				)
			}
			if err := model.ValidateClusterConf(cc); err != nil {
				return fmt.Errorf(
					"dn err_epoch cluster %016x: %w", cid, err,
				)
			}
			return d.health.setDnErrEpoch(ctx, cid, addrPort(), epoch, cc)
		},
		load: func(ctx context.Context) (uint64, bool, error) {
			conf := &pb.DnConf{}
			found, err := d.store.Get(
				ctx, model.DnConfKey(cid, addrPort()), conf,
			)
			return conf.GetErrEpoch(), found, err
		},
	}
}

// newCnMonitor builds the health monitor of one CN (HL1). Unlike the DN's, it
// needs no ClusterConf: CN capacity keys carry no bin index, so nothing about
// them depends on dn_bin_conf (MD4; architecture.md, Finding CN candidates).
// Its refresh reads the CnConf the syncup reads (dnv-worker.md, cn role —
// `worker/cnrole.go`).
func newCnMonitor(
	d *deps,
	cid uint64,
	cnId uint64,
	addrPort func() string,
) *healthMonitor {
	return &healthMonitor{
		deps:   d,
		role:   common.WorkerRoleCn,
		record: healthRecordCn,
		cid:    cid,
		attrs:  []slog.Attr{slog.Uint64("cn_id", cnId)},
		write: func(ctx context.Context, epoch uint64, _ bool) error {
			return d.health.setCnErrEpoch(ctx, cid, addrPort(), epoch)
		},
		load: func(ctx context.Context) (uint64, bool, error) {
			conf := &pb.CnConf{}
			found, err := d.store.Get(
				ctx, model.CnConfKey(cid, addrPort()), conf,
			)
			return conf.GetErrEpoch(), found, err
		},
	}
}

// accepted says whether an agent_reply's code is one the worker may read
// rows out of. Code 0 and common.ReplyCodeLeftover are: a leftover reply is
// an ACCEPTED request whose desired state is stored and whose wanted objects
// were all converged — the node simply still holds something the desired
// state does not want. Its *Info rows are a full probe of the wanted objects,
// so they are evaluated exactly as for code 0; treating the code as a
// rejection would freeze health, push planning and RW19 reporting for as long
// as one leftover survived.
//
// The three rejection codes (stale revision, unknown object, invalid conf)
// mean the request was NOT applied, so their replies carry no verdict.
func accepted(code uint32) bool {
	return code == 0 || code == common.ReplyCodeLeftover
}

// dnObservation applies the HL1 table to one CheckDn/SyncupDn reply (HL5:
// info is the LATEST KNOWN DnInfo, not necessarily this reply's). The ERROR
// sources are disk_info, meta_info and port_info — including meta_info's
// "disk lacks Write Zeroes" (architecture.md, Side provisioning protocol),
// which is a plain ERROR.
func dnObservation(code uint32, info *pb.DnInfo) (healthObs, string) {
	if !accepted(code) {
		return healthNone, ""
	}
	if resName, bad := firstErrorRow(
		row{"disk_info", info.GetDiskInfo()},
		row{"meta_info", info.GetMetaInfo()},
		row{"port_info", info.GetPortInfo()},
	); bad {
		return healthErrorRow, resName
	}
	return healthClean, ""
}

// cnObservation applies the HL1 table to one CheckCn/SyncupCn reply. The
// ERROR sources are port_info, tmpfs_info, tmp_file_info and loop_dev_info.
func cnObservation(code uint32, info *pb.CnInfo) (healthObs, string) {
	if !accepted(code) {
		return healthNone, ""
	}
	if resName, bad := firstErrorRow(
		row{"port_info", info.GetPortInfo()},
		row{"tmpfs_info", info.GetTmpfsInfo()},
		row{"tmp_file_info", info.GetTmpFileInfo()},
		row{"loop_dev_info", info.GetLoopDevInfo()},
	); bad {
		return healthErrorRow, resName
	}
	return healthClean, ""
}

// markDnUnknown records RES_STATUS_UNKNOWN on a DN's in-memory info while its
// stream is dead (HL1; architecture.md, Live-state reporting). It is never
// written to etcd; it exists so that the ERROR rows of a stale info do not
// outlive the stream that reported them — the first reply on a fresh stream
// carries the full info again (architecture.md, Check streams).
func markDnUnknown(info *pb.DnInfo) {
	if info == nil {
		return
	}
	markUnknown(info.GetDiskInfo(), info.GetMetaInfo(), info.GetPortInfo())
}

// markCnUnknown is markDnUnknown for a CN (HL1; architecture.md, Live-state
// reporting).
func markCnUnknown(info *pb.CnInfo) {
	if info == nil {
		return
	}
	markUnknown(
		info.GetPortInfo(),
		info.GetTmpfsInfo(),
		info.GetTmpFileInfo(),
		info.GetLoopDevInfo(),
	)
}

// ---------------------------------------------------------------------------
// HL2 — the sp-object tables (used by the sp role, RW14-RW20)
// ---------------------------------------------------------------------------

// newCntlrMonitor builds the health monitor of one cntlr (HL2).
func newCntlrMonitor(
	d *deps,
	cid uint64,
	spId uint64,
	cntlrId uint64,
) *healthMonitor {
	return &healthMonitor{
		deps:   d,
		role:   common.WorkerRoleSp,
		record: healthRecordCntlr,
		cid:    cid,
		attrs: []slog.Attr{
			slog.Uint64("sp_id", spId),
			slog.Uint64("cntlr_id", cntlrId),
		},
		write: func(ctx context.Context, epoch uint64, settle bool) error {
			return d.health.setCntlrErrEpoch(
				ctx, cid, spId, cntlrId, epoch, settle,
			)
		},
	}
}

// newLegMonitor builds the health monitor of one leg (HL2). A leg's health is
// reported by the PRIMARY cntlr's probe (architecture.md, Group on-leg layout:
// meta region, data region, health block), never by a standby, so the
// monitor lives on the sp coordinator rather than on a cntlr child.
func newLegMonitor(
	d *deps,
	cid uint64,
	spId uint64,
	sliceId uint64,
	legId uint64,
) *healthMonitor {
	return &healthMonitor{
		deps:   d,
		role:   common.WorkerRoleSp,
		record: healthRecordLeg,
		cid:    cid,
		attrs: []slog.Attr{
			slog.Uint64("sp_id", spId),
			slog.Uint64("slice_id", sliceId),
			slog.Uint64("leg_id", legId),
		},
		write: func(ctx context.Context, epoch uint64, _ bool) error {
			return d.health.setLegErrEpoch(ctx, cid, spId, sliceId, legId, epoch)
		},
	}
}

// newSideMonitor builds the health monitor of one side (HL2).
func newSideMonitor(
	d *deps,
	cid uint64,
	spId uint64,
	sliceId uint64,
	sideId uint64,
) *healthMonitor {
	return &healthMonitor{
		deps:   d,
		role:   common.WorkerRoleSp,
		record: healthRecordSide,
		cid:    cid,
		attrs: []slog.Attr{
			slog.Uint64("sp_id", spId),
			slog.Uint64("slice_id", sliceId),
			slog.Uint64("side_id", sideId),
		},
		write: func(ctx context.Context, epoch uint64, _ bool) error {
			return d.health.setSideErrEpoch(
				ctx, cid, spId, sliceId, sideId, epoch,
			)
		},
	}
}

// cntlrRowMap is one labelled map of a CntlrInfo.
type cntlrRowMap struct {
	label string
	rows  map[uint64]*pb.ResInfo
}

// cntlrHealthMaps are the CntlrInfo maps HL2 judges a cntlr by: every map but
// leg_id_to_leg, whose rows belong to the legs (below), the per-td thin maps
// included.
func cntlrHealthMaps(info *pb.CntlrInfo) []cntlrRowMap {
	maps := []cntlrRowMap{
		{"ss_id_to_subsystem", info.GetSsIdToSubsystem()},
		{"ns_id_to_namespace", info.GetNsIdToNamespace()},
		{"ns_id_to_dm_linear", info.GetNsIdToDmLinear()},
		{"td_id_to_raid0", info.GetTdIdToRaid0()},
		{"td_id_to_dm_error", info.GetTdIdToDmError()},
		{"slice_id_to_dm_pool", info.GetSliceIdToDmPool()},
		{"slice_id_to_meta", info.GetSliceIdToMeta()},
		{"slice_id_to_data", info.GetSliceIdToData()},
		{"grp_id_to_md_raid", info.GetGrpIdToMdRaid()},
		{"xfer_id_to_dm_linear", info.GetXferIdToDmLinear()},
		{"xfer_id_to_subsystem", info.GetXferIdToSubsystem()},
		{"xfer_id_to_namespace", info.GetXferIdToNamespace()},
		{"clone_id_to_target", info.GetCloneIdToTarget()},
		{"clone_id_to_dm_clone", info.GetCloneIdToDmClone()},
		{"clone_id_to_meta", info.GetCloneIdToMeta()},
	}
	for _, tdId := range sortedKeys(info.GetTdIdToThinInfo()) {
		maps = append(maps, cntlrRowMap{
			thinMapLabel,
			info.GetTdIdToThinInfo()[tdId].GetSliceIdToDmThin(),
		})
	}
	return maps
}

// thinMapLabel labels the per-td thin maps of a CntlrInfo, keyed by slice.
const thinMapLabel = "slice_id_to_dm_thin"

// cntlrObservation applies the HL2 cntlr row to one CheckCntlr/SyncupCntlr
// reply: any RES_STATUS_ERROR row of the latest known CntlrInfo OTHER than
// leg_id_to_leg, whose rows belong to the legs (below). A row of either of
// HL2's classes counts (sharedStateTds): the class steers AR5 and AR7, not
// the verdict, so the row stays visible and the epoch set.
func cntlrObservation(code uint32, info *pb.CntlrInfo) (healthObs, string) {
	if !accepted(code) {
		return healthNone, ""
	}
	for _, m := range cntlrHealthMaps(info) {
		if resName, bad := firstErrorInMap(m.label, m.rows); bad {
			return healthErrorRow, resName
		}
	}
	return healthClean, ""
}

// thinIdMissing is how a created td's thin row names the thin id missing from
// its slice's pool, the mark of HL2's shared-state rows (sharedStateTds). The
// cn agent attaches a created td's volume with a bare `dmsetup create` of its
// thin table and never messages the id back into existence (cnagent.md CN14);
// dm-thin refuses the table of a dev_id the pool's metadata does not hold with
// ENODATA, which dmsetup prints as this, and the agent carries the output in
// the row's details (cnagent.md CN29 error capture).
// Only a converge's report names it: a Check round's probe finds the volume
// absent and reads it MISSING.
const thinIdMissing = "No data available"

// cntlrRowId names one row of a map HL2 judges a cntlr by: the map's label,
// the td of a per-td thin map (0 for any other map) and the row's key.
type cntlrRowId struct {
	label string
	tdId  uint64
	key   uint64
}

// cntlrErrorRows lists every RES_STATUS_ERROR row HL2 judges a cntlr by, in
// cntlrObservation's order; cntlrObservation stops at the first.
func cntlrErrorRows(info *pb.CntlrInfo) []cntlrRowId {
	var rows []cntlrRowId
	add := func(label string, tdId uint64, m map[uint64]*pb.ResInfo) {
		for _, key := range sortedKeys(m) {
			if m[key].GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
				rows = append(rows, cntlrRowId{label, tdId, key})
			}
		}
	}
	for _, m := range cntlrHealthMaps(info) {
		if m.label != thinMapLabel {
			add(m.label, 0, m.rows)
		}
	}
	// The thin maps carry their td, which cntlrHealthMaps leaves out.
	for _, tdId := range sortedKeys(info.GetTdIdToThinInfo()) {
		add(thinMapLabel, tdId,
			info.GetTdIdToThinInfo()[tdId].GetSliceIdToDmThin())
	}
	return rows
}

// sharedStateTds applies HL2's two row classes to one CntlrInfo. A row is of
// the shared-state class when it belongs to the stack of a created td (state)
// whose thin row, in some slice, reads RES_STATUS_ERROR naming the thin id
// missing (thinIdMissing): the td's own rows — its thin rows, its raid0 and
// its dm-error — and every row of an object that exists for it alone — the
// ns-dev and the nvmet namespace of each of its namespaces, the three rows of
// a transfer out of one of them, the three of a clone onto it. The pool lives
// on the SP's legs, so whichever cntlr holds the primary role reads the same
// rows, and no failover or replacement brings the td, or anything over it,
// back: an operator does (architecture.md, v1 assumptions and known limits).
// Every other ERROR row is the cntlr's own.
// It returns those tds, ascending, when the info carries ERROR rows and every
// one of them is of the shared-state class, and nil when one is the cntlr's
// own or there is none. Both classes set Cntlr.err_epoch alike
// (cntlrObservation): the class steers AR5 and AR7 alone.
func sharedStateTds(info *pb.CntlrInfo, state *model.SpState) []uint64 {
	lost := make(map[uint64]bool)
	for _, td := range state.Tds {
		if !td.GetCreated() {
			continue
		}
		thin := info.GetTdIdToThinInfo()[td.GetTdId()]
		for _, res := range thin.GetSliceIdToDmThin() {
			if res.GetStatus() == pb.ResStatus_RES_STATUS_ERROR &&
				strings.Contains(res.GetDetails(), thinIdMissing) {
				lost[td.GetTdId()] = true
			}
		}
	}
	if len(lost) == 0 {
		return nil
	}
	stacks := newTdStacks(state)
	for _, row := range cntlrErrorRows(info) {
		tdId, ok := stacks.owner(row)
		if !ok || !lost[tdId] {
			return nil
		}
	}
	return sortedKeys(lost)
}

// tdStacks maps the rows of the objects that exist for one td alone to that
// td (sharedStateTds), from the SP's desired state: a namespace to its td, a
// transfer to the td of the namespace it exports, a clone to its destination.
type tdStacks struct {
	ns    map[uint64]uint64
	xfer  map[uint64]uint64
	clone map[uint64]uint64
}

func newTdStacks(state *model.SpState) *tdStacks {
	s := &tdStacks{
		ns:    make(map[uint64]uint64),
		xfer:  make(map[uint64]uint64),
		clone: make(map[uint64]uint64),
	}
	byIdx := make(map[string]map[uint32]uint64, len(state.Subsystems))
	for nqn, ss := range state.Subsystems {
		byIdx[nqn] = make(map[uint32]uint64, len(ss.GetNsList()))
		for _, ns := range ss.GetNsList() {
			s.ns[ns.GetNsId()] = ns.GetTdId()
			byIdx[nqn][ns.GetNsIdx()] = ns.GetTdId()
		}
	}
	for _, xfer := range state.Xfers {
		if tdId, ok := byIdx[xfer.GetOriNqn()][xfer.GetOriNsIdx()]; ok {
			s.xfer[xfer.GetXferId()] = tdId
		}
	}
	for _, clone := range state.Clones {
		s.clone[clone.GetCloneId()] = clone.GetDstTdId()
	}
	return s
}

// owner is the td whose stack holds row, if one does.
func (s *tdStacks) owner(row cntlrRowId) (uint64, bool) {
	var ids map[uint64]uint64
	switch row.label {
	case thinMapLabel:
		return row.tdId, true
	case "td_id_to_raid0", "td_id_to_dm_error":
		return row.key, true
	case "ns_id_to_namespace", "ns_id_to_dm_linear":
		ids = s.ns
	case "xfer_id_to_dm_linear", "xfer_id_to_subsystem",
		"xfer_id_to_namespace":
		ids = s.xfer
	case "clone_id_to_target", "clone_id_to_dm_clone", "clone_id_to_meta":
		ids = s.clone
	default:
		return 0, false
	}
	tdId, ok := ids[row.key]
	return tdId, ok
}

// primaryShapeBuilt reports whether a primary's reply shows its stack built,
// the rows its sp_level suppresses aside: no row of the maps HL2 judges a
// cntlr by, grp_id_to_md_raid aside, reads RES_STATUS_PROVISIONING, or
// RES_STATUS_MISSING with details other than CN19's "sp_level"
// (common.ResDetailsSpLevel, the cn agent's own value). HL2's settle requires
// it. A new SP's primary reports a slice's pool rows,
// and the thin volumes in that pool, PROVISIONING until every leg of the
// groups under it has a provisioned side, and its raid0s — with the ns-devs,
// namespaces, clones and transfers over them — until no slice is deferred
// (cnagent.md CN9, [D15]; the ns-devs, on the td's dm-error, their namespaces
// and a transfer's device, subsystem and namespace are built meanwhile but
// read PROVISIONING all the same): a clean reply that says nothing about the
// build still to come, and a settle on it would leave that build to be judged
// by primary_unhealthy, which at 32 slices failed the building primary over
// (e2e_integtest.md, Known limits). MISSING is the other half. A converge that
// finds a member not available (a promotion ahead of the sides' ANA flips, a
// provisioned flip ahead of the side's export) reports the groups and pools it
// could not build ERROR and leaves them to the CN10 retry, whose first pass
// comes 5 s later; a Check round in between probes the absent devices and
// reports them MISSING "", not ERROR, and in an SP with no td, as a new SP is
// until one is created, that reply has no ERROR row outside leg_id_to_leg (a
// td's raid0 row reads ERROR while its thins are absent; a leg row reads the
// CN11 prober, which fails on a path still non-optimized). Every other MISSING
// the cn agent reports is likewise a device not built, or a clone whose
// source is not connected or could not be read this pass (CN18), so a
// primary showing one stays settling, held to cntlr_unhealthy, until it
// clears — for as long as a clone's source stays unconnected or unread. Leg
// rows are left out: a spare whose side is still zeroing reads PROVISIONING
// there for as long as it zeroes. So are group rows, for a grow: its new
// groups are appended to their lists and stay out of the live concat while
// their sides zero (CN9's prefix cut), so their group rows alone read
// PROVISIONING, beside a serving pool, for minutes.
// Nothing else needs them: a new SP has one group per list (the gateway's
// planSpGroups), so a group of its that is still provisioning defers its
// whole slice, whose pool rows say so, and a group a converge could not
// assemble leaves the pool rows over it ERROR or MISSING. A leg that a forced
// FinishMigration left with an unprovisioned destination as its only side is,
// to the agent, a leg still zeroing: in the first group of its list it defers
// the whole slice and every td with it, as a new SP's first zeroing does, so
// a primary that takes the role meanwhile stays settling until that side's
// RW18 flip, and indefinitely if the side never finishes; in a later group,
// one the pool's concat already spans, the concat under the thin-pool cannot
// shrink and a pool row reads ERROR. A row the sp_level suppresses reads
// MISSING "sp_level" (CN19) and holds nothing, so a primary does not wait for
// the layers its level suppresses. Yet a primary whose level suppresses its
// pools settles before the zeroing ends only at SP_LEVEL_DISABLE, or with no
// namespace and no transfer: [D15]'s deferral does not depend on the level,
// and its ns-devs, namespaces and transfers read PROVISIONING while a slice
// is deferred.
func primaryShapeBuilt(info *pb.CntlrInfo) bool {
	for _, m := range cntlrHealthMaps(info) {
		if m.label == "grp_id_to_md_raid" {
			continue
		}
		for _, res := range m.rows {
			switch res.GetStatus() {
			case pb.ResStatus_RES_STATUS_PROVISIONING:
				return false
			case pb.ResStatus_RES_STATUS_MISSING:
				if res.GetDetails() != common.ResDetailsSpLevel {
					return false
				}
			}
		}
	}
	return true
}

// sideObservation applies the HL2 side row to one CheckSide/SyncupSide reply:
// side_dev_info, any cn_id_to_dm_error / cn_id_to_dm_linear / cn_id_to_nvmeof
// row, and the migr_src_info / migr_dst_info rows.
func sideObservation(code uint32, info *pb.SideInfo) (healthObs, string) {
	if !accepted(code) {
		return healthNone, ""
	}
	if resName, bad := firstErrorRow(
		row{"side_dev_info", info.GetSideDevInfo()},
	); bad {
		return healthErrorRow, resName
	}
	maps := []struct {
		label string
		rows  map[uint64]*pb.ResInfo
	}{
		{"cn_id_to_dm_error", info.GetCnIdToDmError()},
		{"cn_id_to_dm_linear", info.GetCnIdToDmLinear()},
		{"cn_id_to_nvmeof", info.GetCnIdToNvmeof()},
	}
	for _, m := range maps {
		if resName, bad := firstErrorInMap(m.label, m.rows); bad {
			return healthErrorRow, resName
		}
	}
	if resName, bad := firstErrorRow(
		row{"migr_src_dm_linear", info.GetMigrSrcInfo().GetDmLinearInfo()},
		row{"migr_src_nvmeof", info.GetMigrSrcInfo().GetNvmeofInfo()},
		row{"migr_dst_target", info.GetMigrDstInfo().GetTargetInfo()},
		row{"migr_dst_dm_clone", info.GetMigrDstInfo().GetDmCloneInfo()},
	); bad {
		return healthErrorRow, resName
	}
	return healthClean, ""
}

// legObservation applies the HL2 leg row: the PRIMARY cntlr's probe
// (architecture.md, Group on-leg layout: meta region, data region, health
// block) of one leg (spares included). ERROR sets, OK clears, everything else —
// and a leg the primary did not report at all — neither sets nor clears. A
// STANDBY's leg row is logged by its caller, never passed in here (HL2).
func legObservation(
	code uint32,
	info *pb.CntlrInfo,
	legId uint64,
) (healthObs, string) {
	if !accepted(code) {
		return healthNone, ""
	}
	res, ok := info.GetLegIdToLeg()[legId]
	if !ok {
		return healthNone, ""
	}
	switch res.GetStatus() {
	case pb.ResStatus_RES_STATUS_ERROR:
		return healthErrorRow, resName("leg_id_to_leg", res)
	case pb.ResStatus_RES_STATUS_OK:
		return healthClean, ""
	default:
		return healthNone, ""
	}
}

// markCntlrUnknown records RES_STATUS_UNKNOWN on a cntlr's in-memory info
// while its stream is dead (architecture.md, Live-state reporting).
func markCntlrUnknown(info *pb.CntlrInfo) {
	if info == nil {
		return
	}
	maps := []map[uint64]*pb.ResInfo{
		info.GetSsIdToSubsystem(),
		info.GetNsIdToNamespace(),
		info.GetNsIdToDmLinear(),
		info.GetTdIdToRaid0(),
		info.GetTdIdToDmError(),
		info.GetSliceIdToDmPool(),
		info.GetSliceIdToMeta(),
		info.GetSliceIdToData(),
		info.GetGrpIdToMdRaid(),
		info.GetLegIdToLeg(),
		info.GetXferIdToDmLinear(),
		info.GetXferIdToSubsystem(),
		info.GetXferIdToNamespace(),
		info.GetCloneIdToTarget(),
		info.GetCloneIdToDmClone(),
		info.GetCloneIdToMeta(),
	}
	for _, rows := range maps {
		for _, res := range rows {
			markUnknown(res)
		}
	}
	for _, thin := range info.GetTdIdToThinInfo() {
		for _, res := range thin.GetSliceIdToDmThin() {
			markUnknown(res)
		}
	}
}

// markSideUnknown records RES_STATUS_UNKNOWN on a side's in-memory info while
// its stream is dead (architecture.md, Live-state reporting).
func markSideUnknown(info *pb.SideInfo) {
	if info == nil {
		return
	}
	markUnknown(info.GetSideDevInfo())
	for _, rows := range []map[uint64]*pb.ResInfo{
		info.GetCnIdToDmError(),
		info.GetCnIdToDmLinear(),
		info.GetCnIdToNvmeof(),
	} {
		for _, res := range rows {
			markUnknown(res)
		}
	}
	markUnknown(
		info.GetMigrSrcInfo().GetDmLinearInfo(),
		info.GetMigrSrcInfo().GetNvmeofInfo(),
		info.GetMigrDstInfo().GetTargetInfo(),
		info.GetMigrDstInfo().GetDmCloneInfo(),
	)
}

// ---------------------------------------------------------------------------
// ResInfo helpers
// ---------------------------------------------------------------------------

// row pairs a ResInfo with the field name it came from, used as the res_name
// fallback when the agent left ResInfo.res_name empty.
type row struct {
	label string
	info  *pb.ResInfo
}

// resName is the "res_name" attribute of the "health changed" record
// (dnv-worker.md, Log records): the agent's own res_name, or the field label
// when it is empty.
func resName(label string, info *pb.ResInfo) string {
	if name := info.GetResName(); name != "" {
		return name
	}
	return label
}

// firstErrorRow returns the res_name of the first RES_STATUS_ERROR row, in the
// listed order. An absent (nil) row is not an error: it is simply not
// reported.
func firstErrorRow(rows ...row) (string, bool) {
	for _, r := range rows {
		if r.info == nil {
			continue
		}
		if r.info.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
			return resName(r.label, r.info), true
		}
	}
	return "", false
}

// firstErrorInMap returns the res_name of the first RES_STATUS_ERROR row of a
// map, scanned in ascending key order so the reported row is deterministic.
func firstErrorInMap(
	label string,
	rows map[uint64]*pb.ResInfo,
) (string, bool) {
	for _, key := range sortedKeys(rows) {
		res := rows[key]
		if res.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
			return resName(label, res), true
		}
	}
	return "", false
}

// sortedKeys returns a map's uint64 keys in ascending order.
func sortedKeys[V any](m map[uint64]V) []uint64 {
	keys := make([]uint64, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// markUnknown sets every given row to RES_STATUS_UNKNOWN (architecture.md,
// Live-state reporting).
func markUnknown(rows ...*pb.ResInfo) {
	for _, res := range rows {
		if res == nil {
			continue
		}
		res.Status = pb.ResStatus_RES_STATUS_UNKNOWN
	}
}
