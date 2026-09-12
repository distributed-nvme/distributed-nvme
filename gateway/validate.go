package gateway

import (
	"regexp"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §7 as pure functions (GW4): no I/O, no etcd,
// no clock. Every handler runs its validation FIRST, before any read, so a
// malformed request is refused without touching the store. Validation that
// depends on stored state — a cntlid slot already in use, an ns_idx already
// taken, a size that must divide the SP's stripe — is not here; it happens
// inside the RPC's STM.
//
// The rule for bounded numerics is §7's: a proto3 zero means "unset" and asks
// for the default, so zero is always accepted here and only a non-zero value
// outside [Min, Max] is refused. Nothing in this file rewrites a request — the
// handler resolves an accepted request into the concrete message it stores
// (model.ResolveClusterConf, model.ResolveBdevConf), which is why validation
// must run FIRST: after resolution every member is non-zero and every bound
// check below would be a tautology.

var (
	// validStr is common.ValidStrPattern: the character set every dnv name
	// and endpoint must be drawn from.
	validStr = regexp.MustCompile(common.ValidStrPattern)
	// validNqn is common.ValidNqnPattern. It requires a ':' after the domain
	// part, so the well-known discovery NQN can never validate — no separate
	// rejection for it exists anywhere (§7).
	validNqn = regexp.MustCompile(common.ValidNqnPattern)
	// validUuid is the canonical RFC 4122 dashed form CreateNamespace
	// generates and therefore also the only form it accepts.
	validUuid = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-` +
			`[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// validNguid is 16 bytes as 32 hex characters.
	validNguid = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// validateName checks one required §7 name: non-empty, at most MaxStrSize
// bytes, drawn from ValidStrPattern. field names the request field so the
// message points a caller at what to fix.
func validateName(field string, value string) error {
	if value == "" {
		return errInvalid("%s must not be empty", field)
	}
	return validateOptionalName(field, value)
}

// validateOptionalName is validateName for a field whose empty value is legal
// (cluster_name, which defaults; location, which defaults to addr_port).
func validateOptionalName(field string, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > common.MaxStrSize {
		return errInvalid("%s is %d bytes, the maximum is %d",
			field, len(value), common.MaxStrSize)
	}
	if !validStr.MatchString(value) {
		return errInvalid("%s %q does not match %s",
			field, value, common.ValidStrPattern)
	}
	return nil
}

// validateNqn checks one NQN against §7: at most MaxNqnLength bytes and
// ValidNqnPattern.
func validateNqn(field string, value string) error {
	if value == "" {
		return errInvalid("%s must not be empty", field)
	}
	if len(value) > common.MaxNqnLength {
		return errInvalid("%s is %d bytes, the maximum is %d",
			field, len(value), common.MaxNqnLength)
	}
	if !validNqn.MatchString(value) {
		return errInvalid("%s %q is not a valid NQN", field, value)
	}
	return nil
}

// validateHosts checks an allowed_hosts list: at most MaxHostCntPerSs
// entries, each one a valid host NQN.
func validateHosts(field string, hosts []string) error {
	if len(hosts) > common.MaxHostCntPerSs {
		return errInvalid("%s has %d entries, the maximum is %d",
			field, len(hosts), common.MaxHostCntPerSs)
	}
	for _, host := range hosts {
		if err := validateNqn(field, host); err != nil {
			return err
		}
	}
	return nil
}

// validateBound refuses a non-zero value outside [min, max]. A zero is the
// proto3 "unset" that asks for the default (§7); the handler substitutes it
// once, at write time, and the stored value is never zero. model's
// stored-conf validators deliberately have no such escape hatch.
func validateBound(field string, value uint64, min uint64, max uint64) error {
	if value == 0 {
		return nil
	}
	if value < min || value > max {
		return errInvalid("%s %d is outside [%d, %d]", field, value, min, max)
	}
	return nil
}

// validateTrConf checks the four members of an NvmeTrConf against the
// MaxStrSize / ValidStrPattern rule of §7. An entirely empty message is
// accepted here; the RPCs that require one say so themselves.
func validateTrConf(field string, conf *pb.NvmeTrConf) error {
	if conf == nil {
		return nil
	}
	pairs := []struct {
		name  string
		value string
	}{
		{"tr_type", conf.GetTrType()},
		{"adr_fam", conf.GetAdrFam()},
		{"tr_addr", conf.GetTrAddr()},
		{"tr_svc_id", conf.GetTrSvcId()},
	}
	for _, pair := range pairs {
		if err := validateOptionalName(
			field+"."+pair.name, pair.value,
		); err != nil {
			return err
		}
	}
	return nil
}

// trConfEmpty reports whether an NvmeTrConf carries nothing at all, which is
// what CreateDiskNode / CreateControllerNode refuse (§8.2).
func trConfEmpty(conf *pb.NvmeTrConf) bool {
	return conf.GetTrType() == "" && conf.GetAdrFam() == "" &&
		conf.GetTrAddr() == "" && conf.GetTrSvcId() == ""
}

// validateTrConfList checks a whole transport list, refusing an empty one:
// both users (CreateClone, UpdateCloneTrConf) require at least one entry.
func validateTrConfList(field string, list []*pb.NvmeTrConf) error {
	if len(list) == 0 {
		return errInvalid("%s must not be empty", field)
	}
	for _, conf := range list {
		if err := validateTrConf(field, conf); err != nil {
			return err
		}
		if trConfEmpty(conf) {
			return errInvalid("%s has an empty entry", field)
		}
	}
	return nil
}

// validateDnBinConf checks a DnBinConf against §7: extent_size's bounds, and
// the §6.2 shift ladder.
//
// The four shifts are all-or-nothing. All four zero is the proto3 "unset" that
// asks for the 0/4/8/12 default, and is accepted. Any other set must already
// BE a ladder — 0 <= bin0 < bin1 < bin2 < bin3 <= 63 — because the stored
// ladder is what every capacity key in the cluster is written under, for the
// life of the cluster, and quietly replacing an operator's ladder with the
// default would give them a cluster binned differently from the one they
// asked for. This is the only place a REQUEST's ladder can be refused:
// model.ResolveDnBinConf replaces any non-ladder with 0/4/8/12 as a whole, so
// a bad set that got past here would be STORED as the default, and
// model.ValidateClusterConf — which refuses a non-ladder on the read side —
// would never see it.
func validateDnBinConf(conf *pb.DnBinConf) error {
	if err := validateBound(
		"dn_bin_conf.extent_size", conf.GetExtentSize(),
		common.MinDnExtSize, common.MaxDnExtSize,
	); err != nil {
		return err
	}
	bin0, bin1 := conf.GetBin0Shift(), conf.GetBin1Shift()
	bin2, bin3 := conf.GetBin2Shift(), conf.GetBin3Shift()
	if bin0 == 0 && bin1 == 0 && bin2 == 0 && bin3 == 0 {
		return nil
	}
	if !(bin0 < bin1 && bin1 < bin2 && bin2 < bin3 && bin3 <= 63) {
		return errInvalid(
			"dn_bin_conf shifts %d/%d/%d/%d are not a ladder "+
				"0 <= bin0_shift < bin1_shift < bin2_shift < bin3_shift <= 63",
			bin0, bin1, bin2, bin3)
	}
	return nil
}

// validateAllocConf checks the two batch sizes.
func validateAllocConf(conf *pb.AllocConf) error {
	if err := validateBound(
		"alloc_conf.dn_batch_size", uint64(conf.GetDnBatchSize()),
		common.MinAllocDnBatchSize, common.MaxAllocDnBatchSize,
	); err != nil {
		return err
	}
	return validateBound(
		"alloc_conf.cn_batch_size", uint64(conf.GetCnBatchSize()),
		common.MinAllocCnBatchSize, common.MaxAllocCnBatchSize)
}

// validateHealthCheckConf checks the four round intervals.
func validateHealthCheckConf(conf *pb.HealthCheckConf) error {
	intervals := []struct {
		name  string
		value uint32
	}{
		{"dn_interval", conf.GetDnInterval()},
		{"cn_interval", conf.GetCnInterval()},
		{"side_interval", conf.GetSideInterval()},
		{"cntlr_interval", conf.GetCntlrInterval()},
	}
	for _, interval := range intervals {
		if err := validateBound(
			"health_check_conf."+interval.name, uint64(interval.value),
			common.MinHealthCheckInterval, common.MaxHealthCheckInterval,
		); err != nil {
			return err
		}
	}
	return nil
}

// validateDmCloneConf checks a hydration knob pair. Both clone and migration
// share one bound table (§7).
func validateDmCloneConf(field string, conf *pb.DmCloneConf) error {
	if err := validateBound(
		field+".hydration_threshold", uint64(conf.GetHydrationThreshold()),
		1, common.MaxCloneThreshold,
	); err != nil {
		return err
	}
	return validateBound(
		field+".hydration_batch_size", uint64(conf.GetHydrationBatchSize()),
		1, common.MaxCloneBatchSize)
}

// validateBdevConf checks the §7 bounds of a BdevConf plus the two structural
// rules: bdev_feature_list MUST be empty in this version, and RedundConf
// accepts only redund_none and redund_md_raid1 (the proto oneof has no third
// case, so an unset oneof is the only other shape and means redund_none).
func validateBdevConf(conf *pb.BdevConf) error {
	if conf == nil {
		return nil
	}
	if len(conf.GetBdevFeatureList()) != 0 {
		return errInvalid("bdev_conf.bdev_feature_list must be empty")
	}
	if err := validateBound(
		"bdev_conf.dm_pool_conf.data_block_size",
		conf.GetDmPoolConf().GetDataBlockSize(),
		common.MinDmPoolDataBlockSize, common.MaxDmPoolDataBlockSize,
	); err != nil {
		return err
	}
	if lwm := conf.GetDmPoolConf().GetLowWaterMarkPct(); lwm != 0 && lwm < 1 {
		// Unreachable for a uint32, kept as the explicit statement of the
		// §7 row: 0 selects DefaultPoolLowWatermarkPct and values above 100
		// are accepted and switch auto-grow off, so nothing is refused.
		return errInvalid(
			"bdev_conf.dm_pool_conf.low_water_mark_pct %d is invalid", lwm)
	}
	if err := validateBound(
		"bdev_conf.dm_raid0_conf.stripe_size",
		conf.GetDmRaid0Conf().GetStripeSize(),
		common.MinDmRaid0StripeSize, common.MaxDmRaid0StripeSize,
	); err != nil {
		return err
	}
	if raid1 := conf.GetRedundConf().GetRedundMdRaid1(); raid1 != nil {
		if err := validateBound(
			"bdev_conf.redund_conf.redund_md_raid1.bitmap_chunk_block_cnt",
			raid1.GetBitmapChunkBlockCnt(),
			common.MinChunkBlockCnt, common.MaxChunkBlockCnt,
		); err != nil {
			return err
		}
	}
	return nil
}

// validateEventThreshold checks the four thresholds and the one cross-field
// rule of §7: leg_unhealthy MUST exceed side_unhealthy AFTER the defaults are
// resolved, because the §10.4 leg repair fires on the side threshold when the
// DN looks dead and on the leg threshold when only the cntlr's path is bad.
func validateEventThreshold(threshold *pb.EventThreshold) error {
	resolved := model.ResolveEventThreshold(threshold)
	if resolved.GetLegUnhealthy() <= resolved.GetSideUnhealthy() {
		return errInvalid(
			"event_threshold.leg_unhealthy %d must exceed side_unhealthy %d",
			resolved.GetLegUnhealthy(), resolved.GetSideUnhealthy())
	}
	return nil
}

// validateClusterConfInput checks every ClusterConf member a
// CreateClusterRequest carries (§8.1: they are write-once, so this is their
// only validation point).
func validateClusterConfInput(req *pb.CreateClusterRequest) error {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return err
	}
	if err := validateBdevConf(req.GetBdevConf()); err != nil {
		return err
	}
	if err := validateDnBinConf(req.GetDnBinConf()); err != nil {
		return err
	}
	if err := validateAllocConf(req.GetAllocConf()); err != nil {
		return err
	}
	return validateHealthCheckConf(req.GetHealthCheckConf())
}

// validateCntlidSlotList checks a cntlid_slot_list against §8.4/§11.8: every
// value below CnCntlidSlotCnt (8) and no duplicates. An empty list is legal
// here and defaults to [0..7] at CreateStoragePool; UpdateStoragePoolCntlidSlotList
// refuses one, because an SP with no slots can produce no side.
func validateCntlidSlotList(slots []uint32, allowEmpty bool) error {
	if len(slots) == 0 {
		if allowEmpty {
			return nil
		}
		return errInvalid("cntlid_slot_list must not be empty")
	}
	seen := make(map[uint32]bool, len(slots))
	for _, slot := range slots {
		if slot >= common.CnCntlidSlotCnt {
			return errInvalid("cntlid_slot_list value %d is not below %d",
				slot, common.CnCntlidSlotCnt)
		}
		if seen[slot] {
			return errInvalid("cntlid_slot_list value %d is duplicated", slot)
		}
		seen[slot] = true
	}
	return nil
}

// validateSpLevel refuses an sp_level that is not one of the declared enum
// values (GW7's "bad enum" row).
func validateSpLevel(level pb.SpLevel) error {
	if _, ok := pb.SpLevel_name[int32(level)]; !ok {
		return errInvalid("sp_level %d is not a known level", int32(level))
	}
	return nil
}

// validateNodeSelector checks the two address lists of a NodeSelector.
func validateNodeSelector(field string, selector *pb.NodeSelector) error {
	if selector == nil {
		return nil
	}
	for _, addr := range selector.GetBlackList() {
		if err := validateName(field+".black_list", addr); err != nil {
			return err
		}
	}
	for _, addr := range selector.GetWhiteList() {
		if err := validateName(field+".white_list", addr); err != nil {
			return err
		}
	}
	return nil
}

// validateDevIdentity checks a supplied namespace identity (§8.8): an empty
// value is generated by the handler, a supplied one must be well-formed.
func validateDevIdentity(uuid string, nguid string) error {
	if uuid != "" && !validUuid.MatchString(uuid) {
		return errInvalid("dev_uuid %q is not a canonical RFC 4122 uuid", uuid)
	}
	if nguid != "" && !validNguid.MatchString(nguid) {
		return errInvalid("dev_nguid %q is not 32 hex characters", nguid)
	}
	return nil
}

// validateCloneGeometry checks the §8.9 / §11.4 source-geometry bounds of a
// CreateClone request.
func validateCloneGeometry(
	sliceCnt uint32,
	stripeSize uint64,
	blockSize uint64,
) error {
	if sliceCnt < 1 || sliceCnt > common.MaxSliceCntPerSp {
		return errInvalid("src_slice_cnt %d is outside [1, %d]",
			sliceCnt, common.MaxSliceCntPerSp)
	}
	const stripeUnit = uint64(4 * 1024)
	const stripeMax = 256 * stripeUnit
	if stripeSize == 0 || stripeSize%stripeUnit != 0 ||
		stripeSize > stripeMax {
		return errInvalid(
			"src_stripe_size %d must be i x 4KiB with 1 <= i <= 256",
			stripeSize)
	}
	const blockUnit = uint64(64 * 1024)
	const blockMax = 16384 * blockUnit
	if blockSize == 0 || blockSize%blockUnit != 0 || blockSize > blockMax {
		return errInvalid(
			"src_block_size %d must be j x 64KiB with 1 <= j <= 16384",
			blockSize)
	}
	if blockSize%stripeSize != 0 {
		return errInvalid(
			"src_block_size %d is not a multiple of src_stripe_size %d",
			blockSize, stripeSize)
	}
	return nil
}

// validateBitmap refuses an empty Append*Bitmap payload (§8.9, §8.11).
func validateBitmap(bitmap []byte) error {
	if len(bitmap) == 0 {
		return errInvalid("bitmap must not be empty")
	}
	return nil
}

// validateGrowExclusivity is the §8.5 rule that decides which of ext_cnt and
// is_meta a GrowSlice may carry: a data grow states a non-zero ext_cnt, a meta
// grow states none because meta sizes come from the ladder.
func validateGrowExclusivity(isMeta bool, extCnt uint64) error {
	if isMeta && extCnt != 0 {
		return errInvalid(
			"ext_cnt must be 0 when is_meta is true: meta group sizes " +
				"follow the ladder")
	}
	if !isMeta && extCnt == 0 {
		return errInvalid("ext_cnt must be non-zero when is_meta is false")
	}
	return nil
}
