package agentfake

import (
	"slices"
	"strconv"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Row resolution (dnv-worker.md, Integration test plan, The fake agent;
// architecture.md, Live-state reporting)
// ---------------------------------------------------------------------------

// resolveRow builds one ResInfo. The status and details come from the
// default entry, then the object's own status/details, then its row
// overrides, applied in the order the keys are given (the last key is the
// most specific and becomes res_name). epoch is the unix second of the last
// status change, so it only moves when the status actually moves.
//
// It serves the dn, cn and side objects, whose rows cannot carry
// when_primary (validate), so no override is gated; a cntlr's rows go through
// resolveCntlrRow.
func (a *Agent) resolveRow(objKey string, keys ...string) *pb.ResInfo {
	return a.resolveCntlrRow(objKey, true, keys...)
}

// resolveCntlrRow is resolveRow for a cntlr object: a row override marked
// when_primary is skipped while primary — the cntlr.primary of the cntlr's
// last accepted request — is false (dnv-worker.md, Integration test plan, The
// fake agent).
func (a *Agent) resolveCntlrRow(
	objKey string, primary bool, keys ...string,
) *pb.ResInfo {
	return a.resolveDerivedRow(objKey, primary, pb.ResStatus_RES_STATUS_OK,
		"", pb.ResReason_RES_REASON_NONE, keys...)
}

// resolveDerivedRow is resolveCntlrRow over a row whose state the fake
// derives from the request instead of defaulting to OK with no details:
// baseStatus, baseDetails and baseReason stand where the default OK stands,
// and every lever applies over them in the usual order. A row lever's reason
// is the row's reason; without one, the base reason stays only while the
// resolved status is still the base status, as the real agent sets a reason
// on that status alone.
func (a *Agent) resolveDerivedRow(
	objKey string,
	primary bool,
	baseStatus pb.ResStatus,
	baseDetails string,
	baseReason pb.ResReason,
	keys ...string,
) *pb.ResInfo {
	resStatus := baseStatus
	details := baseDetails
	var reason *pb.ResReason
	if def := a.beh.Default; def != nil {
		if def.Status != nil {
			resStatus = *def.Status
		}
		if def.Details != nil {
			details = *def.Details
		}
	}
	if ob := a.objLocked(objKey); ob != nil {
		if ob.Status != nil {
			resStatus = *ob.Status
		}
		if ob.Details != nil {
			details = *ob.Details
		}
		for _, key := range keys {
			row, ok := ob.Rows[key]
			if !ok || (row.WhenPrimary && !primary) {
				continue
			}
			if row.Status != nil {
				resStatus = *row.Status
			}
			if row.Details != nil {
				details = *row.Details
			}
			if row.Reason != nil {
				reason = row.Reason
			}
		}
	}
	resReason := pb.ResReason_RES_REASON_NONE
	switch {
	case reason != nil:
		resReason = *reason
	case resStatus == baseStatus:
		resReason = baseReason
	}
	resName := keys[len(keys)-1]
	return &pb.ResInfo{
		ResName: resName,
		Status:  resStatus,
		Details: details,
		Epoch:   a.epochLocked(objKey, resName, resStatus),
		Reason:  resReason,
	}
}

// epochLocked returns the unix seconds of resName's last status change.
func (a *Agent) epochLocked(
	objKey, resName string, resStatus pb.ResStatus,
) uint64 {
	rows := a.epochs[objKey]
	if rows == nil {
		rows = make(map[string]*epochEntry)
		a.epochs[objKey] = rows
	}
	entry := rows[resName]
	if entry == nil || entry.status != resStatus {
		entry = &epochEntry{
			status: resStatus,
			epoch:  uint64(time.Now().Unix()),
		}
		rows[resName] = entry
	}
	return entry.epoch
}

// ---------------------------------------------------------------------------
// *Info derivation from the last accepted request
// (dnv-worker.md, Integration test plan, The fake agent)
// ---------------------------------------------------------------------------

// dnInfoLocked reports the three DnInfo rows; nil until a SyncupDn has been
// applied, so the worker's revision check re-issues the syncup. The disk
// counts ride beside the rows when the levers give them and the meta row
// reads OK, as the dn agent sends them only beside an OK meta row
// (dnagent.md DN18).
func (a *Agent) dnInfoLocked() *pb.DnInfo {
	req, _ := a.requestLocked(DnKey).(*pb.SyncupDnRequest)
	if req == nil {
		return nil
	}
	info := &pb.DnInfo{
		DiskInfo: a.resolveRow(DnKey, "disk_info"),
		MetaInfo: a.resolveRow(DnKey, "meta_info"),
		PortInfo: a.resolveRow(DnKey, "port_info"),
	}
	ob := a.objLocked(DnKey)
	if ob == nil || ob.DiskCounts == nil ||
		info.GetMetaInfo().GetStatus() != pb.ResStatus_RES_STATUS_OK {
		return info
	}
	counts := ob.DiskCounts
	sideRecCnt := uint64(len(req.GetSidePointerList()))
	if counts.SideRecCnt != nil {
		sideRecCnt = *counts.SideRecCnt
	}
	match := true
	if counts.SideRecsMatch != nil {
		match = *counts.SideRecsMatch
	}
	info.DiskCounts = &pb.DnInfo_DiskCounts{
		TotalExtCnt:   counts.TotalExtCnt,
		FreeExtCnt:    counts.FreeExtCnt,
		SideRecCnt:    sideRecCnt,
		FreeMetaUnits: counts.FreeMetaUnits,
		SideRecsMatch: match,
	}
	return info
}

// cnInfoLocked reports CnInfo's one row, its port's
// (architecture.md, Controller node, common; cnagent.md CN5).
func (a *Agent) cnInfoLocked() *pb.CnInfo {
	if a.requestLocked(CnKey) == nil {
		return nil
	}
	return &pb.CnInfo{PortInfo: a.resolveRow(CnKey, "port_info")}
}

// sideInfoLocked derives SideInfo from the side's last accepted request: one
// row per per-CN export stack (primary_cn_id when non-zero plus every
// standby_id_list entry), the migration roles only when the request carried
// their conf, and the provisioning counters — zero_bytes = the request's
// side_conf.zero_bytes and zeroed_bytes = zero_bytes by default (instant
// provisioning), either overridable per object (architecture.md, Side
// provisioning protocol).
//
// A migration destination whose request carries side_conf.provisioned and
// migr_dst_conf.cutover_pending reports what the real dn agent reports once
// its metadata wrapper stands and it waits for the cutover (dnagent.md DN13,
// DN18): both migr_dst_info rows PROVISIONING "waiting for the cutover" with
// the reason RES_REASON_AWAITING_CUTOVER, which the worker's cutover reads
// (dnv-worker.md RW18). A lever that overrides such a row keeps the reason
// only while the row still reads PROVISIONING, so a case holds the cutover
// back on the side by moving the target row to any other status. Before the
// side is provisioned the rows default to OK, as every row the fake does not
// derive.
func (a *Agent) sideInfoLocked(key string) *pb.SideInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupSideRequest)
	if req == nil {
		return nil
	}
	conf := req.GetSideConf()
	info := &pb.SideInfo{
		SideDevInfo:    a.resolveRow(key, "side_dev_info"),
		CnIdToDmError:  make(map[uint64]*pb.ResInfo),
		CnIdToDmLinear: make(map[uint64]*pb.ResInfo),
		CnIdToNvmeof:   make(map[uint64]*pb.ResInfo),
	}
	cnIdList := make([]uint64, 0, len(conf.GetStandbyIdList())+1)
	if conf.GetPrimaryCnId() != 0 {
		cnIdList = append(cnIdList, conf.GetPrimaryCnId())
	}
	cnIdList = append(cnIdList, conf.GetStandbyIdList()...)
	for _, cnId := range cnIdList {
		info.CnIdToDmError[cnId] = a.resolveRow(
			key, rowKey("cn_id_to_dm_error", cnId))
		info.CnIdToDmLinear[cnId] = a.resolveRow(
			key, rowKey("cn_id_to_dm_linear", cnId))
		info.CnIdToNvmeof[cnId] = a.resolveRow(
			key, rowKey("cn_id_to_nvmeof", cnId))
	}
	if req.GetMigrSrcConf() != nil {
		info.MigrSrcInfo = &pb.SideInfo_MigrSrcInfo{
			DmLinearInfo: a.resolveRow(key, "migr_src_info.dm_linear_info"),
			NvmeofInfo:   a.resolveRow(key, "migr_src_info.nvmeof_info"),
		}
	}
	if dst := req.GetMigrDstConf(); dst != nil {
		awaiting := dst.GetCutoverPending() && conf.GetProvisioned()
		info.MigrDstInfo = &pb.SideInfo_MigrDstInfo{
			TargetInfo: a.migrDstRowLocked(
				key, awaiting, "migr_dst_info.target_info"),
			DmCloneInfo: a.migrDstRowLocked(
				key, awaiting, "migr_dst_info.dm_clone_info"),
		}
	}
	zeroBytes := conf.GetZeroBytes()
	ob := a.objLocked(key)
	if ob != nil && ob.ZeroBytes != nil {
		zeroBytes = *ob.ZeroBytes
	}
	zeroedBytes := zeroBytes
	if ob != nil && ob.ZeroedBytes != nil {
		zeroedBytes = *ob.ZeroedBytes
	}
	info.ZeroBytes = zeroBytes
	info.ZeroedBytes = zeroedBytes
	return info
}

// migrDstRowLocked resolves one of a migration destination's two rows: the
// default OK, or — while awaiting, the side provisioned and its cutover
// pending — PROVISIONING "waiting for the cutover", carrying the reason
// RES_REASON_AWAITING_CUTOVER whenever the resolved status is still
// PROVISIONING (sideInfoLocked).
func (a *Agent) migrDstRowLocked(
	key string, awaiting bool, rowName string,
) *pb.ResInfo {
	if !awaiting {
		return a.resolveRow(key, rowName)
	}
	return a.resolveDerivedRow(key, true,
		pb.ResStatus_RES_STATUS_PROVISIONING, detailsAwaitingCutover,
		pb.ResReason_RES_REASON_AWAITING_CUTOVER, rowName)
}

// sideBmInfoLocked reports the applied migration-bitmap indexes of the side's
// destination role (architecture.md, Bitmap push protocol: BitmapInfo.res_id
// = migr_id, the set derived from the recorded chunks unless a lever
// overrides it).
func (a *Agent) sideBmInfoLocked(key string) *pb.BitmapInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupSideRequest)
	if req == nil || req.GetMigrDstConf() == nil {
		return nil
	}
	migrId := req.GetMigrDstConf().GetMigrId()
	return &pb.BitmapInfo{
		ResId:     migrId,
		BmIdxList: a.bmIdxListLocked(key, migrId),
	}
}

// bmIdxListLocked derives one migration's applied index set, honouring the
// bm_idx_list override.
func (a *Agent) bmIdxListLocked(key string, resId uint64) []uint32 {
	if ob := a.objLocked(key); ob != nil && ob.BmIdxList != nil {
		return slices.Clone(*ob.BmIdxList)
	}
	return a.state[key].bmIdxList(resId)
}

// chunkIdListLocked derives one clone's applied chunk set, honouring the
// chunk_id_list override — the pair-addressed counterpart of bm_idx_list, by
// which a case forces a clone's applied set (an empty list forces "nothing
// applied"). The entries are built afresh, so a reply the grpc codec touches
// cannot reach back into the levers.
func (a *Agent) chunkIdListLocked(
	key string, resId uint64,
) []*pb.BmChunkId {
	if ob := a.objLocked(key); ob != nil && ob.ChunkIdList != nil {
		forced := make([]*pb.BmChunkId, 0, len(*ob.ChunkIdList))
		for _, chunkId := range *ob.ChunkIdList {
			forced = append(forced, &pb.BmChunkId{
				SrcSliceIdx: chunkId.SrcSliceIdx,
				BmIdx:       chunkId.BmIdx,
			})
		}
		return forced
	}
	return a.state[key].chunkIdList(resId)
}

// sliceIdList returns the sorted slice ids of a cntlr's last request.
// id_to_slice is keyed by common.IdKeyFmt ("%016x").
func sliceIdList(req *pb.SyncupCntlrRequest) []uint64 {
	idList := make([]uint64, 0, len(req.GetIdToSlice()))
	for key := range req.GetIdToSlice() {
		sliceId, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			continue
		}
		idList = append(idList, sliceId)
	}
	slices.Sort(idList)
	return idList
}

// cntlrInfoLocked derives CntlrInfo from the cntlr's last accepted request:
// one row per object it named — per slice, per group (meta and data), per leg
// AND per spare leg of every group, per td, per subsystem, per namespace, per
// clone and per xfer, a clone's being its target row and its dm-clone row,
// the kinds of row the cn agent reports for a clone (cnagent.md CN28) — and,
// for each clone of its clone_list, a group row for the clone's clone meta
// group and a leg row per leg and per spare leg of it, as for a slice's group
// (dnv-worker.md, Integration test plan, The fake agent). td_id_to_thin_info
// is filled only when the request's cntlr.primary is true and the thin_ok
// lever says so (architecture.md, sp role; cnagent CN14 "primary only"),
// minus any thin_missing_slices — the created-flip negative of the worker
// suite's smoke case (RW19). The same
// cntlr.primary gates every when_primary row override (resolveCntlrRow).
//
// side_id_to_path holds one row per side of every leg of a slice's group with
// two sides — the shape a migration gives a leg; a leg of a clone meta group
// never has a second side, since no migration moves it — on a primary and a
// standby alike, as the real cn agent reports its paths (cnagent.md CN28): OK
// "live/optimized" for a provisioned side, which is what the worker's cutover
// waits for on the primary (dnv-worker.md RW18), and PROVISIONING
// "provisioning" for one that is not, the side the real agent connects to
// only once it is provisioned. A
// row override, keyed "side_id_to_path.<side_id>", holds a path down.
//
// A cntlr also reports a leg row per new leg of every repair its request
// carries in name_to_repair, as the cn agent connects, wraps and probes a
// new leg as it does a spare (architecture.md, Repair). While its request
// makes it the primary it reports, for every group of the request, a
// reading in grp_id_to_grp_info — whole, with every leg of the group's
// leg_list in sync, unless the grps lever says otherwise — and, for each
// repair of the request the repairs lever names, that lever's verdict in
// rep_id_to_repair_info under the repair's rep_id. A standby reports
// neither, as the real cn agent reads no array as a standby (cnagent.md
// CN12).
func (a *Agent) cntlrInfoLocked(key string) *pb.CntlrInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupCntlrRequest)
	if req == nil {
		return nil
	}
	primary := req.GetCntlr().GetPrimary()
	info := &pb.CntlrInfo{
		SsIdToSubsystem:   make(map[uint64]*pb.ResInfo),
		NsIdToNamespace:   make(map[uint64]*pb.ResInfo),
		NsIdToDmLinear:    make(map[uint64]*pb.ResInfo),
		TdIdToRaid0:       make(map[uint64]*pb.ResInfo),
		TdIdToDmError:     make(map[uint64]*pb.ResInfo),
		TdIdToThinInfo:    make(map[uint64]*pb.CntlrInfo_ThinInfo),
		SliceIdToDmPool:   make(map[uint64]*pb.ResInfo),
		SliceIdToMeta:     make(map[uint64]*pb.ResInfo),
		SliceIdToData:     make(map[uint64]*pb.ResInfo),
		GrpIdToMdRaid:     make(map[uint64]*pb.ResInfo),
		LegIdToLeg:        make(map[uint64]*pb.ResInfo),
		XferIdToDmLinear:  make(map[uint64]*pb.ResInfo),
		XferIdToSubsystem: make(map[uint64]*pb.ResInfo),
		XferIdToNamespace: make(map[uint64]*pb.ResInfo),
		CloneIdToTarget:   make(map[uint64]*pb.ResInfo),
		CloneIdToDmClone:  make(map[uint64]*pb.ResInfo),
		SideIdToPath:      make(map[uint64]*pb.ResInfo),
	}
	if primary {
		info.RepIdToRepairInfo = make(map[uint64]*pb.CntlrInfo_RepairInfo)
		info.GrpIdToGrpInfo = make(map[uint64]*pb.CntlrInfo_GrpInfo)
	}
	sliceIdList := sliceIdList(req)
	for _, sliceId := range sliceIdList {
		info.SliceIdToDmPool[sliceId] = a.resolveCntlrRow(
			key, primary, rowKey("slice_id_to_dm_pool", sliceId))
		info.SliceIdToMeta[sliceId] = a.resolveCntlrRow(
			key, primary, rowKey("slice_id_to_meta", sliceId))
		info.SliceIdToData[sliceId] = a.resolveCntlrRow(
			key, primary, rowKey("slice_id_to_data", sliceId))
	}
	for _, slice := range req.GetIdToSlice() {
		grpList := make([]*pb.Group, 0,
			len(slice.GetMetaGrpList())+len(slice.GetDataGrpList()))
		grpList = append(grpList, slice.GetMetaGrpList()...)
		grpList = append(grpList, slice.GetDataGrpList()...)
		for _, grp := range grpList {
			a.grpRowsLocked(info, key, primary, grp, true)
		}
	}
	for _, clone := range req.GetCloneList() {
		if grp := clone.GetMetaGrp(); grp != nil {
			a.grpRowsLocked(info, key, primary, grp, false)
		}
	}
	a.repairRowsLocked(info, key, primary, req)
	thinOk := false
	var thinMissing map[uint64]bool
	if ob := a.objLocked(key); ob != nil {
		thinOk = ob.ThinOk
		thinMissing = make(map[uint64]bool, len(ob.ThinMissingSlices))
		for _, sliceId := range ob.ThinMissingSlices {
			thinMissing[sliceId] = true
		}
	}
	for _, td := range req.GetTdList() {
		tdId := td.GetTdId()
		info.TdIdToRaid0[tdId] = a.resolveCntlrRow(
			key, primary, rowKey("td_id_to_raid0", tdId))
		info.TdIdToDmError[tdId] = a.resolveCntlrRow(
			key, primary, rowKey("td_id_to_dm_error", tdId))
		if !primary || !thinOk {
			continue
		}
		thinKey := rowKey("td_id_to_thin_info", tdId)
		thin := &pb.CntlrInfo_ThinInfo{
			SliceIdToDmThin: make(map[uint64]*pb.ResInfo),
		}
		for _, sliceId := range sliceIdList {
			if thinMissing[sliceId] {
				continue
			}
			thin.SliceIdToDmThin[sliceId] = a.resolveCntlrRow(
				key, primary, thinKey,
				thinKey+"."+strconv.FormatUint(sliceId, 10))
		}
		info.TdIdToThinInfo[tdId] = thin
	}
	for _, subsystem := range req.GetNqnToSubsystem() {
		info.SsIdToSubsystem[subsystem.GetSsId()] = a.resolveCntlrRow(
			key, primary, rowKey("ss_id_to_subsystem", subsystem.GetSsId()))
		for _, ns := range subsystem.GetNsList() {
			info.NsIdToNamespace[ns.GetNsId()] = a.resolveCntlrRow(
				key, primary, rowKey("ns_id_to_namespace", ns.GetNsId()))
			info.NsIdToDmLinear[ns.GetNsId()] = a.resolveCntlrRow(
				key, primary, rowKey("ns_id_to_dm_linear", ns.GetNsId()))
		}
	}
	for _, clone := range req.GetCloneList() {
		cloneId := clone.GetCloneId()
		info.CloneIdToTarget[cloneId] = a.resolveCntlrRow(
			key, primary, rowKey("clone_id_to_target", cloneId))
		info.CloneIdToDmClone[cloneId] = a.resolveCntlrRow(
			key, primary, rowKey("clone_id_to_dm_clone", cloneId))
	}
	for _, xfer := range req.GetXferList() {
		xferId := xfer.GetXferId()
		info.XferIdToDmLinear[xferId] = a.resolveCntlrRow(
			key, primary, rowKey("xfer_id_to_dm_linear", xferId))
		info.XferIdToSubsystem[xferId] = a.resolveCntlrRow(
			key, primary, rowKey("xfer_id_to_subsystem", xferId))
		info.XferIdToNamespace[xferId] = a.resolveCntlrRow(
			key, primary, rowKey("xfer_id_to_namespace", xferId))
	}
	return info
}

// grpRowsLocked derives one group's rows into info (cntlrInfoLocked): its
// group row and a leg row per leg and per spare leg, and, with paths, the
// path rows of every leg with two sides. A clone meta group is walked without
// paths: no migration gives its legs a second side. On a primary it adds the
// group's reading (grpReadingLocked).
func (a *Agent) grpRowsLocked(
	info *pb.CntlrInfo, key string, primary bool, grp *pb.Group, paths bool,
) {
	info.GrpIdToMdRaid[grp.GetGrpId()] = a.resolveCntlrRow(
		key, primary, rowKey("grp_id_to_md_raid", grp.GetGrpId()))
	if primary {
		info.GrpIdToGrpInfo[grp.GetGrpId()] = a.grpReadingLocked(key, grp)
	}
	legList := make([]*pb.Leg, 0,
		len(grp.GetLegList())+len(grp.GetSpareLegList()))
	legList = append(legList, grp.GetLegList()...)
	legList = append(legList, grp.GetSpareLegList()...)
	for _, leg := range legList {
		info.LegIdToLeg[leg.GetLegId()] = a.resolveCntlrRow(
			key, primary, rowKey("leg_id_to_leg", leg.GetLegId()))
		if !paths || len(leg.GetSideList()) != 2 {
			continue
		}
		for _, side := range leg.GetSideList() {
			info.SideIdToPath[side.GetSideId()] =
				a.pathRowLocked(key, primary, side)
		}
	}
}

// grpReadingLocked is a primary's reading of one group (cntlrInfoLocked):
// whole, with every leg of its leg_list in sync, each half overridden by the
// grps lever's entry for the group. The list is built afresh, so a reply the
// grpc codec touches cannot reach back into the levers.
func (a *Agent) grpReadingLocked(key string, grp *pb.Group) *pb.CntlrInfo_GrpInfo {
	reading := &pb.CntlrInfo_GrpInfo{Whole: true}
	for _, leg := range grp.GetLegList() {
		reading.InSyncLegIdList = append(
			reading.InSyncLegIdList, leg.GetLegId())
	}
	ob := a.objLocked(key)
	if ob == nil {
		return reading
	}
	lever, ok := ob.Grps[grp.GetGrpId()]
	if !ok {
		return reading
	}
	if lever.Whole != nil {
		reading.Whole = *lever.Whole
	}
	if lever.InSync != nil {
		reading.InSyncLegIdList = slices.Clone(*lever.InSync)
	}
	return reading
}

// repairRowsLocked derives what a cntlr reports of the repairs of its request
// (cntlrInfoLocked): a leg row per new leg of every repair, on a primary and
// a standby alike, and, on a primary, the verdict the repairs lever gives a
// repair, keyed by the repair's rep_id.
func (a *Agent) repairRowsLocked(
	info *pb.CntlrInfo, key string, primary bool, req *pb.SyncupCntlrRequest,
) {
	ob := a.objLocked(key)
	for repName, rep := range req.GetNameToRepair() {
		for _, leg := range rep.GetNewLegList() {
			info.LegIdToLeg[leg.GetLegId()] = a.resolveCntlrRow(
				key, primary, rowKey("leg_id_to_leg", leg.GetLegId()))
		}
		if !primary || ob == nil {
			continue
		}
		verdict, ok := ob.Repairs[repName]
		if !ok {
			continue
		}
		info.RepIdToRepairInfo[rep.GetRepId()] = &pb.CntlrInfo_RepairInfo{
			Started:        verdict.Started,
			AbortConfirmed: verdict.AbortConfirmed,
		}
	}
}

// pathRowLocked resolves one side's path row (cntlrInfoLocked): live for a
// provisioned side, held at provisioning for one that is not, under every
// lever.
func (a *Agent) pathRowLocked(
	key string, primary bool, side *pb.Side,
) *pb.ResInfo {
	resStatus := pb.ResStatus_RES_STATUS_OK
	details := detailsPathLive
	if !side.GetProvisioned() {
		resStatus = pb.ResStatus_RES_STATUS_PROVISIONING
		details = detailsPathProvisioning
	}
	return a.resolveDerivedRow(key, primary, resStatus, details,
		pb.ResReason_RES_REASON_NONE,
		rowKey("side_id_to_path", side.GetSideId()))
}

// cntlrBmInfoListLocked reports one BitmapInfo per clone of the last request
// (architecture.md, Bitmap push protocol: SyncupCntlrReply.bm_info_list,
// res_id = clone_id). A clone's applied set rides in chunk_id_list, the
// pair-addressed field; bm_idx_list stays unset, it carries migration chunks
// only.
func (a *Agent) cntlrBmInfoListLocked(key string) []*pb.BitmapInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupCntlrRequest)
	if req == nil {
		return nil
	}
	bmInfoList := make([]*pb.BitmapInfo, 0, len(req.GetCloneList()))
	for _, clone := range req.GetCloneList() {
		bmInfoList = append(bmInfoList, &pb.BitmapInfo{
			ResId:       clone.GetCloneId(),
			ChunkIdList: a.chunkIdListLocked(key, clone.GetCloneId()),
		})
	}
	if len(bmInfoList) == 0 {
		return nil
	}
	return bmInfoList
}
