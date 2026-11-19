package gateway

import (
	"math/bits"
	"regexp"
	"strings"

	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Common validation, as pure functions (GW4): no
// I/O, no etcd, no clock. Every handler runs its validation FIRST, before any
// read, so a malformed request is refused without touching the store.
// Validation that depends on stored state — a cntlid slot already in use, an
// ns_idx already taken, a size that must divide the SP's stripe — is not here;
// it happens inside the RPC's STM. CreateStoragePool also runs checks that
// depend on the cluster it reads: validateBdevConf and the stripe's product
// (validateMergedBdevConf) on the bdev_conf it merges over the cluster's, and
// validatePrimaryThreshold against the cluster's cntlr_interval — each once
// over the conf its plain pre-read returned, before its scans, and once more
// inside its STM. The geometry rules of architecture.md, Common validation,
// must hold for what the SP stores, and only the merge shows what an omitted
// member becomes.
//
// The rule for bounded numerics is that of architecture.md, Common validation:
// a proto3 zero means "unset" and asks for the default, so zero is always
// accepted here and only a non-zero value outside [Min, Max] is refused.
// Nothing in this file rewrites a request — the
// handler resolves an accepted request into the concrete message it stores
// (model.ResolveClusterConf, model.ResolveSpBdevConf), which is why validation
// must run FIRST: after resolution every member is non-zero and every bound
// check below would be a tautology.

var (
	// validStr is common.ValidStrPattern: the character set every dnv name
	// and endpoint must be drawn from.
	validStr = regexp.MustCompile(common.ValidStrPattern)
	// validNqn is common.ValidNqnPattern. It requires a ':' after the domain
	// part, so the well-known discovery NQN can never validate — no separate
	// rejection for it exists anywhere (architecture.md, Common validation).
	validNqn = regexp.MustCompile(common.ValidNqnPattern)
	// validUuid is the canonical RFC 4122 dashed form CreateNamespace
	// generates and therefore also the only form it accepts.
	validUuid = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-` +
			`[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// validNguid is 16 bytes as 32 hex characters.
	validNguid = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// validateName checks one required name of architecture.md, Common validation:
// non-empty, at most MaxStrSize bytes, drawn from ValidStrPattern. field names
// the request field so the message points a caller at what to fix.
func validateName(field string, value string) error {
	if value == "" {
		return errInvalid("%s must not be empty", field)
	}
	return validateOptionalName(field, value)
}

// validateOptionalName is validateName for a field whose empty value is legal
// (cluster_name, which defaults; location, which defaults to the host name
// the node's agent reports, judged by hostNameLocation; CreateThinDevice's
// ori_name, which is optional).
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

// hostNameLocation is the location of a node created with none: the host
// name its agent reported in the size reply (architecture.md, Disk nodes and
// Controller nodes). A name that is no valid location — empty, as an agent
// older than the field or a failed read reports it, too long, or outside
// ValidStrPattern, as the kernel's "(none)" of a machine never named is —
// refuses the create FAILED_PRECONDITION with the advice to pass a location:
// the request is well formed and the node is what refuses it. No fall back
// to addr_port: that would give each agent a failure domain of its own, in
// silence.
func hostNameLocation(addrPort string, hostName string) (string, error) {
	if hostName == "" {
		return "", errPrecondition("the agent at %q reports no host name "+
			"(an agent older than the gateway reports none); pass a "+
			"location", addrPort)
	}
	if err := validateOptionalName("host name", hostName); err != nil {
		return "", errPrecondition("the agent at %q reports a host name "+
			"that is no valid location: %s; pass a location",
			addrPort, status.Convert(err).Message())
	}
	return hostName, nil
}

// validateRepName is CreateRepair's rep_name rule (architecture.md, Common
// validation): a name as validateName checks one, which does not start with
// common.RepNameReservedPrefix, the prefix of the sp worker's own repairs,
// so that no operator's repair carries it. The other repair calls address an
// existing repair, whatever its name, the worker's included, and check the
// name with validateName alone.
func validateRepName(value string) error {
	if err := validateName("rep_name", value); err != nil {
		return err
	}
	if common.HasRepNameReservedPrefix(value) {
		return errInvalid("rep_name %q starts with the reserved prefix %q",
			value, common.RepNameReservedPrefix)
	}
	return nil
}

// validateNqn checks one NQN against architecture.md, Common validation: at
// most MaxNqnLength bytes, ValidNqnPattern (which keeps out "/" and
// whitespace) and no "..".
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
	if strings.Contains(value, "..") {
		return errInvalid("%s %q contains \"..\"", field, value)
	}
	return nil
}

// validateHostFacingNqn is validateNqn plus the dnv-namespace rule of
// architecture.md, Common validation, for the
// two fields that carry it: CreateSubsystem's nqn, where a user picks a
// subsystem's NQN, and CreateTransfer's ori_nqn, where a transfer is built on
// one. The agents attribute a subsystem by parsing its NQN (common.ParseNqn,
// common.IsDnvNqn): a user NQN of a dnv kind would read as one dnv minted, and
// one that only carries the prefix is never swept. Host NQNs (a transfer's
// allowed_hosts holds CnHostNqns) and the nqn of UpdateSubsystemHosts,
// CreateNamespace, UpdateNamespaceDev and UpdateNamespaceSuspended keep plain
// validateNqn, so a subsystem stored in the dnv namespace can still be
// updated when its NQN passes validateNqn; a clone's src_nqn takes
// validateCloneSrcNqn, and DeleteNamespace and DeleteSubsystem take
// validateExistingNqn.
func validateHostFacingNqn(field string, value string) error {
	if err := validateNqn(field, value); err != nil {
		return err
	}
	if common.IsDnvNqn(value) {
		return errInvalid("%s %q is in the dnv namespace %q, which only "+
			"the NQNs dnv mints may use", field, value, common.NqnPrefix+":")
	}
	return nil
}

// validateCloneSrcNqn is validateNqn plus the dnv namespace rule for a
// clone's source, CreateClone's src_nqn (architecture.md, Common
// validation): one in the dnv namespace is taken only when it decodes as a
// transfer NQN (common.IsCloneSrcNqn). The cn agent removes a clone source
// once its clone has gone only when it is a transfer of this cluster, judged
// by its NQN, or a transfer of another cluster or one in no dnv namespace,
// judged at node level by its controllers' host NQN (cnagent.md CN21), so
// any other source in the dnv namespace would stay connected on every
// controller node that read it, for good.
func validateCloneSrcNqn(field string, value string) error {
	if err := validateNqn(field, value); err != nil {
		return err
	}
	if !common.IsCloneSrcNqn(value) {
		return errInvalid("%s %q is in the dnv namespace %q and is no "+
			"transfer NQN", field, value, common.NqnPrefix+":")
	}
	return nil
}

// validateExistingNqn is the length check of architecture.md, Common
// validation, alone, for the nqn of the two RPCs that empty and delete a
// subsystem: DeleteNamespace and DeleteSubsystem. A subsystem stored under an
// NQN those rules refuse can still be emptied and deleted, and its SP after
// it. Nothing more is needed here: the nqn only ever names an exact subsystem
// key, and a string no subsystem is stored under is NOT_FOUND there.
func validateExistingNqn(field string, value string) error {
	if value == "" {
		return errInvalid("%s must not be empty", field)
	}
	if len(value) > common.MaxNqnLength {
		return errInvalid("%s is %d bytes, the maximum is %d",
			field, len(value), common.MaxNqnLength)
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
// proto3 "unset" that asks for the default (architecture.md, Common
// validation); the handler substitutes it once, at write time, and the stored
// value is never zero. model's stored-conf validators deliberately have no
// such escape hatch.
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
// MaxStrSize / ValidStrPattern rule of architecture.md, Common validation,
// and tr_svc_id against MaxTrSvcIdLength as well: a CN's transport conf is
// copied verbatim into the discovery log, whose TRSVCID field holds no more
// (cdc.md DS3), and the one rule holds for every transport conf. The pattern
// needs a character, so a message with any member set must have all four:
// a partly blank address is stored for good, since no call changes a node's
// address. tr_type is NvmeTrTypeTcp and adr_fam NvmeAdrFamIpv4 or
// NvmeAdrFamIpv6, in every conf alike: an agent hands a DN's, a CN's or a
// clone source's tr_type to the kernel, which matches the name exactly, and
// the cdc renders no other words. An entirely empty message is accepted
// here; the RPCs that require one say so themselves.
func validateTrConf(field string, conf *pb.NvmeTrConf) error {
	if trConfEmpty(conf) {
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
		if err := validateName(
			field+"."+pair.name, pair.value,
		); err != nil {
			return err
		}
	}
	if conf.GetTrType() != common.NvmeTrTypeTcp {
		return errInvalid("%s.tr_type %q is not %s: dnv serves NVMe/TCP "+
			"only", field, conf.GetTrType(), common.NvmeTrTypeTcp)
	}
	if fam := conf.GetAdrFam(); fam != common.NvmeAdrFamIpv4 &&
		fam != common.NvmeAdrFamIpv6 {
		return errInvalid("%s.adr_fam %q is neither %s nor %s", field, fam,
			common.NvmeAdrFamIpv4, common.NvmeAdrFamIpv6)
	}
	if len(conf.GetTrSvcId()) > common.MaxTrSvcIdLength {
		return errInvalid("%s.tr_svc_id is %d bytes, the maximum is %d",
			field, len(conf.GetTrSvcId()), common.MaxTrSvcIdLength)
	}
	return nil
}

// trConfEmpty reports whether an NvmeTrConf carries nothing at all, which is
// what CreateDiskNode / CreateControllerNode refuse (architecture.md, Disk
// nodes).
func trConfEmpty(conf *pb.NvmeTrConf) bool {
	return conf.GetTrType() == "" && conf.GetAdrFam() == "" &&
		conf.GetTrAddr() == "" && conf.GetTrSvcId() == ""
}

// validateTrConfList checks a whole transport list. Both users (CreateClone,
// UpdateCloneTrConf) require at least one entry and at most MaxCntlrCntPerSp,
// the most cntlrs a source pool has, since every cntlr syncup of the
// destination pool carries the list (architecture.md, Common validation).
func validateTrConfList(field string, list []*pb.NvmeTrConf) error {
	if len(list) == 0 {
		return errInvalid("%s must not be empty", field)
	}
	if len(list) > common.MaxCntlrCntPerSp {
		return errInvalid("%s has %d entries, the maximum is %d",
			field, len(list), common.MaxCntlrCntPerSp)
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

// validateDnBinConf checks a DnBinConf against architecture.md, Common
// validation: extent_size's bounds and its unit, and the shift ladder of
// architecture.md, DN bins.
//
// An extent size is a whole number of DnExtSizeUnit: a side's runs are each
// floored to sectors on their own (sideRunSectors, agent/dnagent) while the
// side is sized whole, so at a size that is not a whole number of 512 bytes
// the runs of a side of two or more runs can sum a sector or more short of
// the side, as the size and the runs' extent counts fall, and the kernel
// refuses its upper tables; at a whole number of 512 bytes that is not one
// of 4096 every side builds, but its runs do not sit on 4 KiB physical
// blocks. A cluster's extent size never changes, so nothing judges a stored
// one.
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
	if size := conf.GetExtentSize(); size%common.DnExtSizeUnit != 0 {
		return errInvalid(
			"dn_bin_conf.extent_size %d is not a multiple of %d",
			size, common.DnExtSizeUnit)
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

// validateQosRatio refuses a qos_ratio that is not all zero: strict set, or a
// non-zero bytes_per_iops or bytes_per_bps (architecture.md, Common
// validation). Nothing enforces QoS (architecture.md, Controller node,
// common), so a stored non-zero ratio would only mislead whoever reads it
// back. An absent ratio and an all-zero one pass, and CreateCluster stores
// them as sent.
//
// model.ValidateClusterConf does not repeat the rule on the read side: the
// worker copies whatever ratio a cluster stores into every SyncupCn, and a
// stored conf that validator refuses stops every pass of that cluster's
// nodes and pools.
func validateQosRatio(ratio *pb.QosRatio) error {
	if ratio.GetStrict() {
		return errInvalid(
			"qos_ratio.strict must be false: QoS is not enforced")
	}
	if v := ratio.GetBytesPerIops(); v != 0 {
		return errInvalid(
			"qos_ratio.bytes_per_iops %d must be zero: QoS is not "+
				"enforced", v)
	}
	if v := ratio.GetBytesPerBps(); v != 0 {
		return errInvalid(
			"qos_ratio.bytes_per_bps %d must be zero: QoS is not "+
				"enforced", v)
	}
	return nil
}

// validateDmCloneConf checks a hydration knob pair. Both clone and migration
// share one bound table (architecture.md, Common validation).
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

// validateBdevConf checks the bounds of a BdevConf (architecture.md, Common
// validation), its four geometry
// rules, the block's bound, and the two structural rules: bdev_feature_list
// must be empty, and
// RedundConf accepts only redund_none and redund_md_raid1 (the
// proto oneof has no third case, so an unset oneof is the only other shape and
// means redund_none).
//
// The geometry rules refuse here what would otherwise be refused only once the
// pool exists — by dm-thin, dm-clone or mdadm as it is built, by CreateClone
// as it is named a source — and then for ever, because a stored geometry never
// changes (architecture.md, Storage pools):
//
//   - data_block_size is a power of two. Its 64 KiB minimum then makes it a
//     multiple of dm-thin's unit ("Invalid block size" otherwise), and it is
//     the dm-clone region of every clone into the pool and of every migration
//     of one of its sides, which dm-clone refuses unless it is a power of two
//     ("Region size is not a power of 2").
//   - data_block_size is at most common.MaxNvmeTcpCmdBytes, the block's
//     bound: a migration's dm-clone reports at least one region as its
//     optimal IO size, so the controller node's path to the new side would
//     send commands up to a block, which the disk node's kernel refuses over
//     TCP above that bound. MaxDmPoolDataBlockSize stays the bounds table's
//     maximum, the one a clone source is held to (validateCloneGeometry).
//   - stripe_size is a multiple of 4 KiB and at most 1 MiB by its bound, and
//     data_block_size is a multiple of it: CreateClone's source rules
//     (architecture.md, raid0 bitmap math),
//     which every pool created under them meets, so that any such pool can be
//     a clone source.
//   - Under md-raid1, the bitmap chunk of bitmap_chunk_block_cnt ×
//     data_block_size bytes is a power of two (mdadm's "invalid bitmap
//     chunksize") and at most maxMdBitmapChunk.
//
// A zero member is "unset" and takes part in no rule, so a rule between two
// members is judged only when both are non-zero. The bdev_conf a create RPC
// stores therefore comes through here a second time, resolved:
// validateClusterConfInput resolves CreateCluster's against the constants and,
// for the stripe, the block, and CreateStoragePool judges its merge over the
// cluster's conf, where an omitted member takes its real value.
func validateBdevConf(conf *pb.BdevConf) error {
	if conf == nil {
		return nil
	}
	if len(conf.GetBdevFeatureList()) != 0 {
		return errInvalid("bdev_conf.bdev_feature_list must be empty")
	}
	blockSize := conf.GetDmPoolConf().GetDataBlockSize()
	if err := validateBound(
		"bdev_conf.dm_pool_conf.data_block_size", blockSize,
		common.MinDmPoolDataBlockSize, common.MaxDmPoolDataBlockSize,
	); err != nil {
		return err
	}
	// Above the minimum, a power of two is also a whole number of dm-thin's
	// 64 KiB (128-sector) units.
	if blockSize != 0 && bits.OnesCount64(blockSize) != 1 {
		return errInvalid(
			"bdev_conf.dm_pool_conf.data_block_size %d is not a power of two",
			blockSize)
	}
	if blockSize > common.MaxNvmeTcpCmdBytes {
		return errInvalid(
			"bdev_conf.dm_pool_conf.data_block_size %d is over %d, the "+
				"most data one NVMe/TCP command may carry: a migration of "+
				"one of the pool's sides copies in regions of one block and "+
				"reports at least one block as its optimal IO size, and the "+
				"controller node would send the new side commands its disk "+
				"node refuses",
			blockSize, common.MaxNvmeTcpCmdBytes)
	}
	if lwm := conf.GetDmPoolConf().GetLowWaterMarkPct(); lwm != 0 && lwm < 1 {
		// Unreachable for a uint32, kept as the explicit statement of the
		// row of architecture.md, Common validation: 0 selects
		// DefaultPoolLowWatermarkPct and values above 100
		// are accepted and switch auto-grow off, so nothing is refused.
		return errInvalid(
			"bdev_conf.dm_pool_conf.low_water_mark_pct %d is invalid", lwm)
	}
	stripeSize := conf.GetDmRaid0Conf().GetStripeSize()
	if err := validateBound(
		"bdev_conf.dm_raid0_conf.stripe_size", stripeSize,
		common.MinDmRaid0StripeSize, common.MaxDmRaid0StripeSize,
	); err != nil {
		return err
	}
	// The minimum is also the stripe's unit: i × 4 KiB (architecture.md, raid0
	// bitmap math).
	if stripeSize%common.MinDmRaid0StripeSize != 0 {
		return errInvalid(
			"bdev_conf.dm_raid0_conf.stripe_size %d is not a multiple of %d",
			stripeSize, common.MinDmRaid0StripeSize)
	}
	if blockSize != 0 && stripeSize != 0 && blockSize%stripeSize != 0 {
		return errInvalid(
			"bdev_conf.dm_pool_conf.data_block_size %d is not a multiple "+
				"of bdev_conf.dm_raid0_conf.stripe_size %d",
			blockSize, stripeSize)
	}
	if raid1 := conf.GetRedundConf().GetRedundMdRaid1(); raid1 != nil {
		chunkBlockCnt := raid1.GetBitmapChunkBlockCnt()
		if err := validateBound(
			"bdev_conf.redund_conf.redund_md_raid1.bitmap_chunk_block_cnt",
			chunkBlockCnt,
			common.MinChunkBlockCnt, common.MaxChunkBlockCnt,
		); err != nil {
			return err
		}
		// Both factors are bounded by now, so the product is at most
		// 1024 × 4 MiB and cannot overflow; it is zero while either is unset.
		chunk := chunkBlockCnt * blockSize
		if chunk != 0 && bits.OnesCount64(chunk) != 1 {
			return errInvalid(
				"bdev_conf.redund_conf.redund_md_raid1."+
					"bitmap_chunk_block_cnt %d x "+
					"bdev_conf.dm_pool_conf.data_block_size %d = %d bytes "+
					"is not a power of two",
				chunkBlockCnt, blockSize, chunk)
		}
		if chunk > maxMdBitmapChunk {
			return errInvalid(
				"bdev_conf.redund_conf.redund_md_raid1."+
					"bitmap_chunk_block_cnt %d x "+
					"bdev_conf.dm_pool_conf.data_block_size %d = %d bytes "+
					"is above %d, the largest md bitmap chunk",
				chunkBlockCnt, blockSize, chunk, maxMdBitmapChunk)
		}
	}
	return nil
}

// maxMdBitmapChunk is the largest md bitmap chunk, in bytes, a pool may ask
// mdadm for (architecture.md, Common validation). mdadm keeps --bitmap-chunk
// in a signed int and turns it into bytes in that int, so 1 GiB is the largest
// chunk it computes without overflowing: a 4 GiB one wraps to zero and is
// refused, and md stores the chunk as a 32-bit byte count anyway.
const maxMdBitmapChunk = uint64(1) << 30

// validateStripeProduct is the bound of architecture.md, Common validation,
// on a pool's stripe times its slice count: that product is the optimal IO
// size the pool's volumes report, a host lets one request grow to it when it
// is over common.MaxNvmeTcpCmdBytes, and the controller node's kernel refuses
// a command that carries more. A zero stripe is unset and takes part in no
// rule. The message names the largest stripe that fits, which model.FitStripe
// gives a stripe the request does not name. It has two callers:
// CreateStoragePool on the stripe its request names, before any read, and
// validateMergedBdevConf (gateway/storagepool.go) on the conf the pool would
// store. Both run it after validateBdevConf and the slice_cnt bound, so the
// product cannot overflow.
func validateStripeProduct(stripe uint64, sliceCnt int) error {
	if stripe == 0 || sliceCnt < 1 {
		return nil
	}
	product := stripe * uint64(sliceCnt)
	if product <= common.MaxNvmeTcpCmdBytes {
		return nil
	}
	return errInvalid(
		"bdev_conf.dm_raid0_conf.stripe_size %d times slice_cnt %d is %d "+
			"bytes, over %d, the most data one NVMe/TCP command may carry "+
			"to a controller node: the pool's volumes would report it as "+
			"their optimal IO size, and hosts would send commands the "+
			"controller node refuses; the largest stripe_size that fits "+
			"is %d",
		stripe, sliceCnt, product, common.MaxNvmeTcpCmdBytes,
		model.FitStripe(stripe, uint32(sliceCnt)))
}

// validateEventThreshold checks the thresholds by the rules that read the
// request alone (architecture.md, Common validation; the one that reads the
// cluster is validatePrimaryThreshold). leg_unhealthy MUST exceed
// side_unhealthy AFTER the defaults are resolved, because the leg repair of
// dnv-worker.md AR8 fires on the side threshold when the DN looks dead and on
// the leg threshold when only the cntlr's path is bad. A leg_fast_io_fail_tmo
// that is set lies from common.MinLegFastIoFailTmo to
// common.DefaultNvmeFastIoFailTmo, whatever the cluster's intervals: a
// longer one would hold a write at a dead leg past the time a host's own IO
// timeout may fire, and a zero is no value set, judged by nothing.
func validateEventThreshold(threshold *pb.EventThreshold) error {
	resolved := model.ResolveEventThreshold(threshold)
	if resolved.GetLegUnhealthy() <= resolved.GetSideUnhealthy() {
		return errInvalid(
			"event_threshold.leg_unhealthy %d must exceed side_unhealthy %d",
			resolved.GetLegUnhealthy(), resolved.GetSideUnhealthy())
	}
	if tmo := threshold.GetLegFastIoFailTmo(); tmo != 0 &&
		(tmo < common.MinLegFastIoFailTmo ||
			tmo > common.DefaultNvmeFastIoFailTmo) {
		return errInvalid(
			"event_threshold.leg_fast_io_fail_tmo %d is outside [%d, %d]",
			tmo, common.MinLegFastIoFailTmo, common.DefaultNvmeFastIoFailTmo)
	}
	return nil
}

// minPrimaryRounds is how many of its cluster's cntlr_interval a pool's
// primary_unhealthy must cover at least (validatePrimaryThreshold).
const minPrimaryRounds = 2

// validatePrimaryThreshold is the rule of architecture.md, Common validation,
// that ties a pool's primary threshold to its cluster: primary_unhealthy, after
// its default is resolved, must cover at least two of the cluster's
// cntlr_interval. The err_epoch one missed check round stamps lives about one
// interval, until the next round clears it, and the rule leaves a second
// interval of margin; a failover also needs two unhealthy verdicts in a row
// from the primary's rounds (dnv-worker.md AR10), which one missed round
// followed by a clean answer never gives. It reads stored state, the cluster's
// health_check_conf, so it is one of the checks CreateStoragePool runs where it
// runs validateMergedBdevConf: on its plain pre-read, before the scans, and
// once more inside its STM.
func validatePrimaryThreshold(
	threshold *pb.EventThreshold,
	hc *pb.HealthCheckConf,
) error {
	primary := uint64(model.ResolveEventThreshold(threshold).GetPrimaryUnhealthy())
	floor := minPrimaryRounds * uint64(hc.GetCntlrInterval())
	if primary < floor {
		return errInvalid(
			"event_threshold.primary_unhealthy %d must be at least %d, "+
				"twice the cluster's health_check_conf.cntlr_interval %d",
			primary, floor, hc.GetCntlrInterval())
	}
	return nil
}

// validateClusterConfInput checks every ClusterConf member a
// CreateClusterRequest carries (architecture.md, Clusters: they are
// write-once, so this is their only validation point).
func validateClusterConfInput(req *pb.CreateClusterRequest) error {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return err
	}
	if err := validateBdevConf(req.GetBdevConf()); err != nil {
		return err
	}
	// Once more on the bdev_conf CreateCluster STORES: a member the request
	// left unset becomes its default: its constant or, for the stripe, the
	// stripe its block gives. Only the resolved message shows whether that
	// default meets the members the request did set — 1000 chunk blocks of the
	// default 1 MiB block is a bitmap chunk mdadm refuses, and a 192 KiB
	// stripe does not divide that block. The message says so, because the
	// value it names may be one this request never sent.
	if err := validateBdevConf(
		model.ResolveBdevConf(req.GetBdevConf()),
	); err != nil {
		return errInvalid("bdev_conf with its defaults filled in: %s",
			status.Convert(err).Message())
	}
	if err := validateDnBinConf(req.GetDnBinConf()); err != nil {
		return err
	}
	if err := validateAllocConf(req.GetAllocConf()); err != nil {
		return err
	}
	if err := validateHealthCheckConf(req.GetHealthCheckConf()); err != nil {
		return err
	}
	if days := req.GetMdCheckConf().GetIntervalDays(); days >
		common.MaxMdCheckDays {
		return errInvalid("md_check_conf.interval_days %d is above %d",
			days, common.MaxMdCheckDays)
	}
	// Absent takes the default hold and zero turns it off (dnv-worker.md
	// HL8); only a value above the bound is refused.
	if seconds := req.GetStartHoldConf().GetSeconds(); seconds >
		common.MaxStartHoldSeconds {
		return errInvalid("start_hold_conf.seconds %d is above %d",
			seconds, common.MaxStartHoldSeconds)
	}
	return validateQosRatio(req.GetQosRatio())
}

// validateCntlidSlotList checks a cntlid_slot_list (architecture.md, Storage
// pools; architecture.md, cntlid slots): every
// value below CnCntlidSlotCnt and no duplicates. An empty list is legal
// here and defaults to every slot below CnCntlidSlotCnt at
// CreateStoragePool; UpdateStoragePoolCntlidSlotList
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

// validateOverrideList refuses a forced start's list of overrides
// (architecture.md, Repairs, ForceStartGroup) that holds a kind naming no
// refusal, NONE or a number the enum does not declare (GW7's "bad enum"
// row), or that names one kind twice.
func validateOverrideList(field string, list []pb.ForcedStartOverride) error {
	seen := make(map[pb.ForcedStartOverride]bool, len(list))
	for i, override := range list {
		if _, ok := pb.ForcedStartOverride_name[int32(override)]; !ok ||
			override == pb.ForcedStartOverride_FORCED_START_OVERRIDE_NONE {
			return errInvalid("%s[%d] names no refusal: %d", field, i,
				int32(override))
		}
		if seen[override] {
			return errInvalid("%s names %s twice", field, override)
		}
		seen[override] = true
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

// validateDevIdentity checks a supplied namespace identity (architecture.md,
// Subsystems, namespaces): an empty value is generated by the handler, a
// supplied one must be well-formed.
func validateDevIdentity(uuid string, nguid string) error {
	if uuid != "" && !validUuid.MatchString(uuid) {
		return errInvalid("dev_uuid %q is not a canonical RFC 4122 uuid", uuid)
	}
	if nguid != "" && !validNguid.MatchString(nguid) {
		return errInvalid("dev_nguid %q is not 32 hex characters", nguid)
	}
	return nil
}

// validateCloneGeometry checks the source-geometry bounds (architecture.md,
// Clones; architecture.md, raid0 bitmap math) of a
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
	// The bounds are the pool's own (validateBdevConf), by name: a pool this
	// cluster accepts is then a legal clone source by construction.
	const stripeUnit = uint64(common.MinDmRaid0StripeSize)
	const stripeMax = uint64(common.MaxDmRaid0StripeSize)
	if stripeSize == 0 || stripeSize%stripeUnit != 0 ||
		stripeSize > stripeMax {
		return errInvalid(
			"src_stripe_size %d must be a positive multiple of %d, "+
				"at most %d", stripeSize, stripeUnit, stripeMax)
	}
	const blockUnit = uint64(common.MinDmPoolDataBlockSize)
	const blockMax = uint64(common.MaxDmPoolDataBlockSize)
	if blockSize == 0 || blockSize%blockUnit != 0 || blockSize > blockMax {
		return errInvalid(
			"src_block_size %d must be a positive multiple of %d, "+
				"at most %d", blockSize, blockUnit, blockMax)
	}
	if blockSize%stripeSize != 0 {
		return errInvalid(
			"src_block_size %d is not a multiple of src_stripe_size %d",
			blockSize, stripeSize)
	}
	return nil
}

// validateBitmap refuses an empty AppendCloneBitmap payload (architecture.md,
// Clones).
func validateBitmap(bitmap []byte) error {
	if len(bitmap) == 0 {
		return errInvalid("bitmap must not be empty")
	}
	return nil
}

// validateGrowExclusivity is the rule of architecture.md, GrowSlice, that
// decides which of ext_cnt and is_meta a GrowSlice may carry: a data grow
// states a non-zero ext_cnt, a meta grow states none because meta sizes come
// from the ladder.
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
