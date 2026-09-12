package model

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Conf defaults, resolved at WRITE time (architecture.md §7)
// ---------------------------------------------------------------------------
//
// Every Resolve* below turns an accepted request into the CONCRETE message the
// gateway stores. They run once, on the write path — `CreateCluster` for a
// ClusterConf, `CreateStoragePool` for an SP's bdev_conf — and never again: a
// conf read back out of etcd already carries its values, so no reader
// substitutes a member of one. Readers use the Validate* helpers further down
// instead, which refuse a zero rather than guessing around it. (§7's two
// deliberate exemptions are stored as sent and resolved elsewhere:
// event_threshold in ops.go, and a migration's DmCloneConf in worker/sprole.go
// — neither is geometry.)
//
// Two things resolve-at-read cost, and this is why they live here: a consumer
// that forgets to resolve (the cn agent did) silently computes with zeros, and
// a stored zero pins geometry to whatever common.Default* the RUNNING binary
// carries, so changing a constant would re-geometry live storage pools.
// Concrete stored values make an SP's geometry genuinely immutable.
//
// Nothing else may re-implement these rules.

// clampU32 keeps value inside [min, max]; a proto3 zero has already been
// replaced by its default before this is called.
func clampU32(value uint32, min uint32, max uint32) uint32 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// binLadderOk is §6.2's rule about the four shifts, in one place so the
// resolver and the validator below cannot drift apart:
// 0 <= bin0 < bin1 < bin2 < bin3 <= 63. The all-zero set a DnBinConf written
// without shifts carries is NOT a ladder — bin0_shift 0 is legitimate, but
// only as the bottom of an increasing one.
func binLadderOk(conf *pb.DnBinConf) bool {
	return conf.GetBin0Shift() < conf.GetBin1Shift() &&
		conf.GetBin1Shift() < conf.GetBin2Shift() &&
		conf.GetBin2Shift() < conf.GetBin3Shift() &&
		conf.GetBin3Shift() <= 63
}

// ResolveDnBinConf returns a copy of conf with its defaults applied
// (architecture.md §6.2). A nil conf resolves to the pure defaults.
//
//   - extent_size: 0 => common.DefaultDnExtSize. The §7 bounds are enforced on
//     the REQUEST, not here: a stored size is what every DN's disk header was
//     formatted with (§3.1) and must never be silently changed afterwards.
//   - the four shifts: a set that is not a ladder — the all-zero one a
//     ClusterConf written without a dn_bin_conf carries above all — falls back
//     to the 0/4/8/12 defaults as a whole, never shift by shift, so that the
//     resolved bins are always consistent. The create RPC refuses an
//     explicitly-invalid ladder before this is reached, so in practice only
//     the all-zero set takes that arm; it stays as robustness.
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
	if !binLadderOk(resolved) {
		resolved.Bin0Shift = common.DefaultDnBin0Shift
		resolved.Bin1Shift = common.DefaultDnBin1Shift
		resolved.Bin2Shift = common.DefaultDnBin2Shift
		resolved.Bin3Shift = common.DefaultDnBin3Shift
	}
	return resolved
}

// ResolveAllocConf returns a copy of conf with its defaults applied
// (architecture.md §6.5, §7): each batch size 0 => 16, clamped to [1, 1024].
// The clamp is unreachable for an accepted request — validateAllocConf refuses
// a non-zero value outside the range first — and is kept as robustness.
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

// resolveInterval applies the §7 rule to one health-check interval: 0 => 5
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
// resolved (§7): each is the object kind's round timeout (§8.1), so none of
// them may ever be zero.
func ResolveHealthCheckConf(conf *pb.HealthCheckConf) *pb.HealthCheckConf {
	return &pb.HealthCheckConf{
		DnInterval:    resolveInterval(conf.GetDnInterval()),
		CnInterval:    resolveInterval(conf.GetCnInterval()),
		SideInterval:  resolveInterval(conf.GetSideInterval()),
		CntlrInterval: resolveInterval(conf.GetCntlrInterval()),
	}
}

// ResolveBdevConf returns a copy of conf with every defaultable member
// concrete (§7): data_block_size 0 => 1 MiB, low_water_mark_pct 0 => 50,
// stripe_size 0 => 64 KiB, and — only when the redund_conf oneof actually
// selects md-raid1 — bitmap_chunk_block_cnt 0 => 128. A nil conf resolves to
// the pure defaults.
//
// Two members are deliberately left alone. low_water_mark_pct above 100 is a
// MEANING, not an error ("never grow this pool automatically", §7), so it is
// carried through as written and never clamped. And redund_conf is a choice,
// not a default: an unset oneof already means redund_none (§8.4), so nothing
// here invents a kind — only the chunk count INSIDE an md-raid1 choice is
// filled in.
func ResolveBdevConf(conf *pb.BdevConf) *pb.BdevConf {
	resolved := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   conf.GetDmPoolConf().GetDataBlockSize(),
			LowWaterMarkPct: conf.GetDmPoolConf().GetLowWaterMarkPct(),
		},
		DmRaid0Conf: &pb.DmRaid0Conf{
			StripeSize: conf.GetDmRaid0Conf().GetStripeSize(),
		},
	}
	// §7 refuses a non-empty bdev_feature_list on both the cluster and the SP,
	// so this list is always empty today. It is copied element by element
	// anyway, for the same reason redund_conf is cloned below: the result ends
	// up inside a stored message and must not alias a request the caller still
	// owns.
	for _, feature := range conf.GetBdevFeatureList() {
		resolved.BdevFeatureList = append(
			resolved.BdevFeatureList,
			proto.Clone(feature).(*pb.BdevFeature),
		)
	}
	if resolved.DmPoolConf.DataBlockSize == 0 {
		resolved.DmPoolConf.DataBlockSize = common.DefaultDmPoolDataBlockSize
	}
	if resolved.DmPoolConf.LowWaterMarkPct == 0 {
		resolved.DmPoolConf.LowWaterMarkPct = common.DefaultPoolLowWatermarkPct
	}
	if resolved.DmRaid0Conf.StripeSize == 0 {
		resolved.DmRaid0Conf.StripeSize = common.DefaultDmRaid0StripeSize
	}
	if raid1 := conf.GetRedundConf().GetRedundMdRaid1(); raid1 != nil {
		chunkBlockCnt := raid1.GetBitmapChunkBlockCnt()
		if chunkBlockCnt == 0 {
			chunkBlockCnt = common.DefaultChunkBlockCnt
		}
		resolved.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: chunkBlockCnt,
				},
			},
		}
	} else if conf.GetRedundConf() != nil {
		// redund_none, or an empty message: cloned rather than aliased,
		// because the result ends up inside a stored message and must not
		// alias a request the caller still owns.
		resolved.RedundConf = proto.Clone(conf.GetRedundConf()).(*pb.RedundConf)
	}
	return resolved
}

// ResolveClusterConf is what CreateCluster STORES: a copy of cc with
// bdev_conf, dn_bin_conf, alloc_conf and health_check_conf fully concrete, and
// creation_epoch and qos_ratio exactly as given (neither is defaultable). A
// nil cc resolves to the pure defaults.
//
// ClusterConf is write-once (§8.1: no UpdateCluster* RPC exists), so this is
// the only chance a cluster's conf ever gets to become concrete — which is why
// it covers every sub-message rather than staying sparse.
func ResolveClusterConf(cc *pb.ClusterConf) *pb.ClusterConf {
	return &pb.ClusterConf{
		CreationEpoch:   cc.GetCreationEpoch(),
		QosRatio:        cc.GetQosRatio(),
		BdevConf:        ResolveBdevConf(cc.GetBdevConf()),
		DnBinConf:       ResolveDnBinConf(cc.GetDnBinConf()),
		AllocConf:       ResolveAllocConf(cc.GetAllocConf()),
		HealthCheckConf: ResolveHealthCheckConf(cc.GetHealthCheckConf()),
	}
}

// ---------------------------------------------------------------------------
// Stored-conf validation (architecture.md §7)
// ---------------------------------------------------------------------------
//
// The mirror image of the Resolve* family above: every conf in etcd is
// concrete, so a reader that finds a zero has found corruption or foreign
// data, not an omission. It refuses — one Error record and the object's pass
// is skipped — rather than substituting, because silently guessing a geometry
// is how a storage pool ends up formatted one way and addressed another.
//
// The error text always begins "invalid stored conf: " and names the proto
// field; that prefix is what an operator and the acceptance checklist grep
// for.

// invalidConf builds the one error shape both validators use.
func invalidConf(format string, args ...any) error {
	return fmt.Errorf("invalid stored conf: "+format, args...)
}

// validateStoredRange refuses a stored value outside [min, max]. Unlike the
// gateway's request-side bound check, a zero has no escape hatch here: it is
// simply below every minimum.
func validateStoredRange(
	field string,
	value uint64,
	min uint64,
	max uint64,
) error {
	if value < min || value > max {
		return invalidConf("%s %d is outside [%d, %d]", field, value, min, max)
	}
	return nil
}

// ValidateBdevConf refuses a stored BdevConf whose geometry is not concrete.
//
// Only the four defaultable members are checked, and only for presence: the §7
// ranges are enforced on the request, and re-enforcing them here would make
// every RPC on a pool whose stored value is out of range fail rather than the
// one RPC that set it. low_water_mark_pct is checked for zero ONLY — a value
// above 100 is the legal "auto-grow off" setting (§7).
func ValidateBdevConf(conf *pb.BdevConf) error {
	if conf.GetDmPoolConf().GetDataBlockSize() == 0 {
		return invalidConf("bdev_conf.dm_pool_conf.data_block_size is zero")
	}
	if conf.GetDmPoolConf().GetLowWaterMarkPct() == 0 {
		return invalidConf("bdev_conf.dm_pool_conf.low_water_mark_pct is zero")
	}
	if conf.GetDmRaid0Conf().GetStripeSize() == 0 {
		return invalidConf("bdev_conf.dm_raid0_conf.stripe_size is zero")
	}
	// The chunk count only exists when the oneof chose md-raid1; a
	// redund_none pool has no bitmap and must not be asked for one (§8.4).
	if raid1 := conf.GetRedundConf().GetRedundMdRaid1(); raid1 != nil &&
		raid1.GetBitmapChunkBlockCnt() == 0 {
		return invalidConf(
			"bdev_conf.redund_conf.redund_md_raid1." +
				"bitmap_chunk_block_cnt is zero")
	}
	return nil
}

// ValidateClusterConf refuses a stored ClusterConf that CreateCluster could
// not have written.
//
// extent_size is checked for zero only, never against [Min, Max]: it is what
// every DN's disk header was formatted with (§3.1), so a cluster whose DNs are
// correctly formatted at an unusual size must keep working. The batch sizes
// and intervals ARE range-checked, because the resolver clamps them into those
// ranges and anything outside therefore cannot have come from a write.
// bdev_conf is validated when present; a cluster never reads its own
// bdev_conf for geometry (each SP carries its own snapshot), so its absence is
// not by itself fatal.
func ValidateClusterConf(cc *pb.ClusterConf) error {
	bin := cc.GetDnBinConf()
	if bin.GetExtentSize() == 0 {
		return invalidConf("dn_bin_conf.extent_size is zero")
	}
	if !binLadderOk(bin) {
		return invalidConf(
			"dn_bin_conf shifts %d/%d/%d/%d are not a ladder "+
				"0 <= bin0 < bin1 < bin2 < bin3 <= 63",
			bin.GetBin0Shift(), bin.GetBin1Shift(),
			bin.GetBin2Shift(), bin.GetBin3Shift())
	}
	alloc := cc.GetAllocConf()
	if err := validateStoredRange(
		"alloc_conf.dn_batch_size", uint64(alloc.GetDnBatchSize()),
		common.MinAllocDnBatchSize, common.MaxAllocDnBatchSize,
	); err != nil {
		return err
	}
	if err := validateStoredRange(
		"alloc_conf.cn_batch_size", uint64(alloc.GetCnBatchSize()),
		common.MinAllocCnBatchSize, common.MaxAllocCnBatchSize,
	); err != nil {
		return err
	}
	hc := cc.GetHealthCheckConf()
	intervals := []struct {
		field string
		value uint32
	}{
		{"health_check_conf.dn_interval", hc.GetDnInterval()},
		{"health_check_conf.cn_interval", hc.GetCnInterval()},
		{"health_check_conf.side_interval", hc.GetSideInterval()},
		{"health_check_conf.cntlr_interval", hc.GetCntlrInterval()},
	}
	for _, interval := range intervals {
		if err := validateStoredRange(
			interval.field, uint64(interval.value),
			common.MinHealthCheckInterval, common.MaxHealthCheckInterval,
		); err != nil {
			return err
		}
	}
	if cc.GetBdevConf() != nil {
		return ValidateBdevConf(cc.GetBdevConf())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bins (MD4, architecture.md §6.2)
// ---------------------------------------------------------------------------

// binLevels returns the four bin levels level_i = 1 << bin_i_shift of a STORED
// DnBinConf (the 0/4/8/12 ladder gives 1/16/256/4096).
//
// It shifts what it is given and resolves nothing: one ladder must hold for
// the whole life of a cluster, because a capacity key embeds the bin it was
// written under and can only be deleted by a transaction that computes the
// same index again. A ladder that moved — which is what a default substituted
// from the running binary would do across an upgrade — would strand every key
// already in the store. An op that reaches this without having refused an
// unusable conf first gets nonsense levels, deliberately, rather than a
// plausible default; MaintainDnCapacity below says where that refusal lives.
func binLevels(conf *pb.DnBinConf) [4]uint64 {
	return [4]uint64{
		uint64(1) << conf.GetBin0Shift(),
		uint64(1) << conf.GetBin1Shift(),
		uint64(1) << conf.GetBin2Shift(),
		uint64(1) << conf.GetBin3Shift(),
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
	// The stored ladder, used as stored: CreateCluster resolves dn_bin_conf
	// before it writes it (§7), so a cluster this control plane created
	// carries a concrete one. Nothing is resolved or guessed here, so an op
	// that reaches this must have refused an unusable cc before it staged its
	// first write: a key embeds the bin it was written under, so a different
	// ladder names a key nothing ever wrote and leaves the live one behind.
	// The gates that do that are, in the gateway, CreateDiskNode,
	// DeleteDiskNode, UpdateDiskNodeDisabled and newDnLedger (the constructor
	// the six handlers that keep their own DN bookkeeping share); in model,
	// GrowSlice's own gate and, for CreateSpareLeg, its two callers'; in the
	// worker, newDnMonitor's per-write re-validation. The one caller with no
	// gate is integtest/workerctl, which plants records on purpose.
	conf := cc.GetDnBinConf()
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
