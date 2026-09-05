package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The "record" attribute of the §12 "health changed" record (HL1/HL2).
const (
	healthRecordDn    = "dn"
	healthRecordCn    = "cn"
	healthRecordCntlr = "cntlr"
	healthRecordLeg   = "leg"
	healthRecordSide  = "side"
)

// The "reason" attribute of the §12 "health changed" record.
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
	// object's health. Two things produce it — agent_reply.code != 0 (HL1's
	// last row and HL2's trailer), which triggers a re-sync instead (RW4), and
	// a leg probe row that is neither ERROR nor OK (HL2's Leg row clears on an
	// explicit RES_STATUS_OK and on nothing else, legObservation below).
	//
	// RES_STATUS_PROVISIONING and MISSING rows are deliberately NOT healthNone
	// for a node, a side or a cntlr: they are simply not ERROR rows, so a
	// reply carrying them is a clean round (HL1 row 3, "no ERROR row in the
	// latest known info"). architecture.md §9.5 makes PROVISIONING healthy
	// ([D15]), and all HL1 row 4 and HL2's trailer forbid is SETTING an
	// err_epoch — which healthClean never does.
	healthNone healthObs = iota
	// healthUnreachable is a stream that cannot be opened, breaks, or misses
	// its reply within the round timeout.
	healthUnreachable
	// healthErrorRow is a RES_STATUS_ERROR row in the object's latest known
	// info.
	healthErrorRow
	// healthClean is a clean round: reply in time, code == 0, no ERROR row in
	// the latest known info (HL5).
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
) error {
	return model.SetCntlrErrEpoch(ctx, w.cli, cid, spId, cntlrId, epoch)
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

// healthMonitor is one object's health bookkeeping (HL3): it remembers the
// last state it WROTE and issues an etcd write only on an observed
// transition. Two owners of the same object (the accepted overlap of §0
// item 4) therefore write at most once each, and the model op re-reads the
// record inside its STM so the second write is a no-op — the §11 threshold
// clock never restarts.
//
// A monitor is owned by one object goroutine (RW1) and needs no locking.
type healthMonitor struct {
	deps   *deps
	role   string
	record string
	cid    uint64
	// attrs are the object's ids as the §12 "health changed" record carries
	// them, between cluster_id and record.
	attrs []slog.Attr
	// write performs the MD6 op for this record kind.
	write func(ctx context.Context, epoch uint64) error

	known     bool
	unhealthy bool
}

// observe folds one observation into the object's health (HL1/HL2, HL3).
func (m *healthMonitor) observe(
	ctx context.Context,
	obs healthObs,
	resName string,
) {
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
		// HL1/HL2: PROVISIONING, MISSING and code != 0 neither set nor clear.
		return
	}
	if m.known && m.unhealthy == unhealthy {
		return
	}
	var epoch uint64
	if unhealthy {
		epoch = m.deps.clk.nowUnix()
	}
	if err := m.write(ctx, epoch); err != nil {
		// Left for the next round (RW12): nothing is remembered, so the
		// transition is retried. The etcdutil records carry the details.
		slog.ErrorContext(ctx, "health write failed",
			slog.String("role", m.role),
			slog.Uint64("cluster_id", m.cid),
			slog.String("record", m.record),
			slog.String("error", err.Error()),
		)
		return
	}
	m.known = true
	m.unhealthy = unhealthy

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
}

// ---------------------------------------------------------------------------
// HL1 — the dn and cn node tables
// ---------------------------------------------------------------------------

// errNoClusterConf is why a DN health write is skipped while its cluster is
// absent from the RW21 cache. MD4 derives the DnCapacity key's bin index from
// dn_bin_conf, so a write with no conf would resolve the DEFAULT 0/4/8/12
// shifts: the real key would never be deleted and a duplicate would be written
// at the wrong bin, which §6.3's bin scan would then hand out as an allocation
// candidate for a DN HL1 has just flagged unhealthy.
var errNoClusterConf = errors.New("cluster conf missing")

// newDnMonitor builds the health monitor of one DN (HL1). The resolved cluster
// conf the capacity key maintenance needs (MD4) is read per write rather than
// captured, because a cluster conf change must reach the next write — and a
// conf that has been deleted between the loop's RW9 gate and this write makes
// the write wait for the next round (RW12) rather than guess defaults.
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
		write: func(ctx context.Context, epoch uint64) error {
			cc, ok := d.conf.get(cid)
			if !ok {
				return fmt.Errorf(
					"dn err_epoch cluster %016x: %w", cid, errNoClusterConf,
				)
			}
			return d.health.setDnErrEpoch(ctx, cid, addrPort(), epoch, cc)
		},
	}
}

// newCnMonitor builds the health monitor of one CN (HL1). Unlike the DN's, it
// needs no ClusterConf: CN capacity keys carry no bin index, so nothing about
// them depends on dn_bin_conf (MD4, §6.4).
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
		write: func(ctx context.Context, epoch uint64) error {
			return d.health.setCnErrEpoch(ctx, cid, addrPort(), epoch)
		},
	}
}

// dnObservation applies the HL1 table to one CheckDn/SyncupDn reply (HL5:
// info is the LATEST KNOWN DnInfo, not necessarily this reply's). The ERROR
// sources are disk_info, meta_info and port_info — including meta_info's
// "disk lacks Write Zeroes" (§9.4), which is a plain ERROR.
func dnObservation(code uint32, info *pb.DnInfo) (healthObs, string) {
	if code != 0 {
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
	if code != 0 {
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
// stream is dead (HL1, §9.5). It is never written to etcd; it exists so that
// the ERROR rows of a stale info do not outlive the stream that reported
// them — the first reply on a fresh stream carries the full info again
// (§9.7).
func markDnUnknown(info *pb.DnInfo) {
	if info == nil {
		return
	}
	markUnknown(info.GetDiskInfo(), info.GetMetaInfo(), info.GetPortInfo())
}

// markCnUnknown is markDnUnknown for a CN (HL1, §9.5).
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
// HL2 — the sp-object tables (used by the sp role, §8.4)
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
		write: func(ctx context.Context, epoch uint64) error {
			return d.health.setCntlrErrEpoch(ctx, cid, spId, cntlrId, epoch)
		},
	}
}

// newLegMonitor builds the health monitor of one leg (HL2). A leg's health is
// reported by the PRIMARY cntlr's §3.6 probe, never by a standby, so the
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
		write: func(ctx context.Context, epoch uint64) error {
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
		write: func(ctx context.Context, epoch uint64) error {
			return d.health.setSideErrEpoch(
				ctx, cid, spId, sliceId, sideId, epoch,
			)
		},
	}
}

// cntlrObservation applies the HL2 cntlr row to one CheckCntlr/SyncupCntlr
// reply: any RES_STATUS_ERROR row of the latest known CntlrInfo OTHER than
// leg_id_to_leg, whose rows belong to the legs (below).
func cntlrObservation(code uint32, info *pb.CntlrInfo) (healthObs, string) {
	if code != 0 {
		return healthNone, ""
	}
	maps := []struct {
		label string
		rows  map[uint64]*pb.ResInfo
	}{
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
	for _, m := range maps {
		if resName, bad := firstErrorInMap(m.label, m.rows); bad {
			return healthErrorRow, resName
		}
	}
	for _, tdId := range sortedKeys(info.GetTdIdToThinInfo()) {
		thin := info.GetTdIdToThinInfo()[tdId]
		if resName, bad := firstErrorInMap(
			"slice_id_to_dm_thin", thin.GetSliceIdToDmThin(),
		); bad {
			return healthErrorRow, resName
		}
	}
	return healthClean, ""
}

// sideObservation applies the HL2 side row to one CheckSide/SyncupSide reply:
// side_dev_info, any cn_id_to_dm_error / cn_id_to_dm_linear / cn_id_to_nvmeof
// row, and the migr_src_info / migr_dst_info rows.
func sideObservation(code uint32, info *pb.SideInfo) (healthObs, string) {
	if code != 0 {
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

// legObservation applies the HL2 leg row: the PRIMARY cntlr's §3.6 probe of
// one leg (spares included). ERROR sets, OK clears, everything else — and a
// leg the primary did not report at all — neither sets nor clears. A
// STANDBY's leg row is logged by its caller, never passed in here (HL2).
func legObservation(
	code uint32,
	info *pb.CntlrInfo,
	legId uint64,
) (healthObs, string) {
	if code != 0 {
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
// while its stream is dead (§9.5).
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
// its stream is dead (§9.5).
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

// resName is the "res_name" attribute of the §12 "health changed" record: the
// agent's own res_name, or the field label when it is empty.
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

// markUnknown sets every given row to RES_STATUS_UNKNOWN (§9.5).
func markUnknown(rows ...*pb.ResInfo) {
	for _, res := range rows {
		if res == nil {
			continue
		}
		res.Status = pb.ResStatus_RES_STATUS_UNKNOWN
	}
}
