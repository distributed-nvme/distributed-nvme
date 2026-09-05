package model

import (
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// ClusterConf defaults (RW21, architecture.md §7)
// ---------------------------------------------------------------------------
//
// These live here, next to the one rule that consumes them (§6.2 bins), so
// that the §8.5 ClusterConf cache and every model op resolve a stored
// ClusterConf exactly the same way. Nothing else may re-implement them
// (dnv-worker.md RW21).

// clampU32 keeps value inside [min, max]; a stored proto3 zero has already
// been replaced by its default before this is called.
func clampU32(value uint32, min uint32, max uint32) uint32 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// ResolveDnBinConf returns a copy of conf with its defaults applied (RW21,
// architecture.md §6.2). A nil conf resolves to the pure defaults.
//
//   - extent_size: 0 => common.DefaultDnExtSize. The §7 bounds are enforced
//     when the value is written, not here: a stored size is what every DN's
//     disk header was formatted with (§3.1) and must never be silently
//     changed on read.
//   - the four shifts: §6.2 requires 0 <= bin0 < bin1 < bin2 < bin3 <= 63.
//     Any other set — the all-zero one a ClusterConf written without a
//     dn_bin_conf carries above all — falls back to the 0/4/8/12 defaults as
//     a whole, never shift by shift, so that the resolved bins are always a
//     consistent ladder.
func ResolveDnBinConf(conf *pb.DnBinConf) *pb.DnBinConf {
	resolved := &pb.DnBinConf{
		ExtentSize: conf.GetExtentSize(),
		Bin0Shift:  conf.GetBin0Shift(),
		Bin1Shift:  conf.GetBin1Shift(),
		Bin2Shift:  conf.GetBin2Shift(),
		Bin3Shift:  conf.GetBin3Shift(),
	}
	if resolved.ExtentSize == 0 {
		resolved.ExtentSize = common.DefaultDnExtSize
	}
	increasing := resolved.Bin0Shift < resolved.Bin1Shift &&
		resolved.Bin1Shift < resolved.Bin2Shift &&
		resolved.Bin2Shift < resolved.Bin3Shift
	if !increasing || resolved.Bin3Shift > 63 {
		resolved.Bin0Shift = common.DefaultDnBin0Shift
		resolved.Bin1Shift = common.DefaultDnBin1Shift
		resolved.Bin2Shift = common.DefaultDnBin2Shift
		resolved.Bin3Shift = common.DefaultDnBin3Shift
	}
	return resolved
}

// ResolveAllocConf returns a copy of conf with its defaults applied
// (architecture.md §6.5, §7): each batch size 0 => 16, clamped to [1, 1024].
func ResolveAllocConf(conf *pb.AllocConf) *pb.AllocConf {
	resolved := &pb.AllocConf{
		DnBatchSize: conf.GetDnBatchSize(),
		CnBatchSize: conf.GetCnBatchSize(),
	}
	if resolved.DnBatchSize == 0 {
		resolved.DnBatchSize = common.DefaultAllocDnBatchSize
	}
	if resolved.CnBatchSize == 0 {
		resolved.CnBatchSize = common.DefaultAllocCnBatchSize
	}
	resolved.DnBatchSize = clampU32(
		resolved.DnBatchSize,
		common.MinAllocDnBatchSize,
		common.MaxAllocDnBatchSize,
	)
	resolved.CnBatchSize = clampU32(
		resolved.CnBatchSize,
		common.MinAllocCnBatchSize,
		common.MaxAllocCnBatchSize,
	)
	return resolved
}

// resolveInterval applies the RW21 rule to one health-check interval: 0 => 5
// seconds, then clamped to [1, 3600].
func resolveInterval(interval uint32) uint32 {
	if interval == 0 {
		interval = common.DefaultHealthCheckInterval
	}
	return clampU32(
		interval,
		common.MinHealthCheckInterval,
		common.MaxHealthCheckInterval,
	)
}

// ResolveHealthCheckConf returns a copy of conf with the four round intervals
// resolved (RW21): each is the object kind's round timeout (§8.1), so none of
// them may ever be zero.
func ResolveHealthCheckConf(conf *pb.HealthCheckConf) *pb.HealthCheckConf {
	return &pb.HealthCheckConf{
		DnInterval:    resolveInterval(conf.GetDnInterval()),
		CnInterval:    resolveInterval(conf.GetCnInterval()),
		SideInterval:  resolveInterval(conf.GetSideInterval()),
		CntlrInterval: resolveInterval(conf.GetCntlrInterval()),
	}
}

// ResolveClusterConf returns the snapshot RW21 hands to a reader of the §8.5
// cache: a copy of cc with dn_bin_conf, alloc_conf and health_check_conf
// resolved, and creation_epoch, qos_ratio and bdev_conf exactly as stored. A
// nil cc resolves to the pure defaults, which is what a cluster written before
// any of these sub-messages existed reads back as.
func ResolveClusterConf(cc *pb.ClusterConf) *pb.ClusterConf {
	return &pb.ClusterConf{
		CreationEpoch:   cc.GetCreationEpoch(),
		QosRatio:        cc.GetQosRatio(),
		BdevConf:        cc.GetBdevConf(),
		DnBinConf:       ResolveDnBinConf(cc.GetDnBinConf()),
		AllocConf:       ResolveAllocConf(cc.GetAllocConf()),
		HealthCheckConf: ResolveHealthCheckConf(cc.GetHealthCheckConf()),
	}
}

// ---------------------------------------------------------------------------
// Bins (MD4, architecture.md §6.2)
// ---------------------------------------------------------------------------

// binLevels returns the four bin levels level_i = 1 << bin_i_shift of a
// resolved DnBinConf (defaults 1/16/256/4096).
func binLevels(conf *pb.DnBinConf) [4]uint64 {
	resolved := ResolveDnBinConf(conf)
	return [4]uint64{
		uint64(1) << resolved.GetBin0Shift(),
		uint64(1) << resolved.GetBin1Shift(),
		uint64(1) << resolved.GetBin2Shift(),
		uint64(1) << resolved.GetBin3Shift(),
	}
}

// DnBinIdx is the bin a DN with freeExt free extents sits in (MD4, §6.2): bin
// b is where level_b <= f < level_{b+1}, bin 3 is unbounded above, and ok is
// false below level_0 — a DN with less free space than the smallest bin holds
// has no bin and therefore no capacity key at all (§5.6).
func DnBinIdx(freeExt uint64, conf *pb.DnBinConf) (uint32, bool) {
	levels := binLevels(conf)
	if freeExt < levels[0] {
		return 0, false
	}
	for binIdx := uint32(len(levels)) - 1; binIdx > 0; binIdx-- {
		if freeExt >= levels[binIdx] {
			return binIdx, true
		}
	}
	return 0, true
}

// ---------------------------------------------------------------------------
// The §5.6 presence rule (MD4)
// ---------------------------------------------------------------------------

// DnAllocatable is the §5.6 presence rule for a DN (MD4): its capacity key
// exists in etcd if and only if this returns true. A nil dn — a record that
// was just deleted, or one that never existed — is not allocatable.
func DnAllocatable(dn *pb.DnConf, conf *pb.DnBinConf) bool {
	if dn == nil {
		return false
	}
	if len(dn.GetSidePtrList()) >= common.MaxSideCntPerDn {
		return false
	}
	if dn.GetErrEpoch() != 0 {
		return false
	}
	if dn.GetDisabled() {
		return false
	}
	// The free floor is exactly "has a bin" (§6.2): free_ext_cnt < 1 <<
	// bin0_shift.
	_, ok := DnBinIdx(dn.GetFreeExtCnt(), conf)
	return ok
}

// CnAllocatable is the §5.6 presence rule for a CN (MD4). CNs have no bins, so
// the free floor is simply a nonzero budget.
func CnAllocatable(cn *pb.CnConf) bool {
	if cn == nil {
		return false
	}
	if len(cn.GetCntlrPtrList()) >= common.MaxCntlrCntPerCn {
		return false
	}
	if cn.GetErrEpoch() != 0 {
		return false
	}
	if cn.GetDisabled() {
		return false
	}
	return cn.GetFreeExtCnt() != 0
}

// ---------------------------------------------------------------------------
// Capacity-key maintenance (MD4)
// ---------------------------------------------------------------------------

// dnCapacityKeyOf is the capacity key a DnConf implies, or "" when the record
// is not allocatable and therefore implies no key at all (§5.6).
func dnCapacityKeyOf(
	cid uint64,
	addrPort string,
	conf *pb.DnBinConf,
	dn *pb.DnConf,
) string {
	if !DnAllocatable(dn, conf) {
		return ""
	}
	binIdx, ok := DnBinIdx(dn.GetFreeExtCnt(), conf)
	if !ok {
		return ""
	}
	return DnCapacityKey(cid, binIdx, dn.GetFreeExtCnt(), addrPort)
}

// cnCapacityKeyOf is the capacity key a CnConf implies, or "" when the record
// is not allocatable (§5.6).
func cnCapacityKeyOf(cid uint64, addrPort string, cn *pb.CnConf) string {
	if !CnAllocatable(cn) {
		return ""
	}
	return CnCapacityKey(cid, cn.GetFreeExtCnt(), addrPort)
}

// MaintainDnCapacity brings a DN's capacity key in line with the §5.6 presence
// rule inside the caller's STM (MD4): it deletes the key oldDn implied, if
// oldDn was allocatable, and writes the key newDn implies, if newDn is. A nil
// oldDn means "the record did not exist before" (a create), a nil newDn means
// "it does not exist any more" (a delete).
//
// oldDn MUST be the record as read in this very STM, which is what makes the
// delete target exact: a capacity key embeds free_ext_cnt, so it can only be
// removed by the transaction that still knows the count it was written with.
// The two records share one addrPort — no v001 path renames a node (§5.5) —
// which is why it is a parameter rather than a field: DnConf is keyed by
// addr_port and does not carry it.
//
// The call is idempotent and safe when nothing moved: when both records imply
// the same key only the put is issued, so an unchanged DN produces no spurious
// delete record in the log. It is called by every op that changes an input of
// the rule, in that op's STM, and never bumps a revision (§5.5).
func MaintainDnCapacity(
	s etcdutil.STM,
	cid uint64,
	addrPort string,
	cc *pb.ClusterConf,
	oldDn *pb.DnConf,
	newDn *pb.DnConf,
) {
	conf := ResolveDnBinConf(cc.GetDnBinConf())
	oldKey := dnCapacityKeyOf(cid, addrPort, conf, oldDn)
	newKey := dnCapacityKeyOf(cid, addrPort, conf, newDn)
	if oldKey != "" && oldKey != newKey {
		s.Del(oldKey)
	}
	if newKey != "" {
		s.Put(newKey, &pb.DnCapacity{Location: newDn.GetLocation()})
	}
}

// MaintainCnCapacity is MaintainDnCapacity for a CN (MD4). It takes no
// ClusterConf: CN capacity keys carry no bin index, so nothing about them
// depends on dn_bin_conf (§6.4).
func MaintainCnCapacity(
	s etcdutil.STM,
	cid uint64,
	addrPort string,
	oldCn *pb.CnConf,
	newCn *pb.CnConf,
) {
	oldKey := cnCapacityKeyOf(cid, addrPort, oldCn)
	newKey := cnCapacityKeyOf(cid, addrPort, newCn)
	if oldKey != "" && oldKey != newKey {
		s.Del(oldKey)
	}
	if newKey != "" {
		s.Put(newKey, &pb.CnCapacity{Location: newCn.GetLocation()})
	}
}
