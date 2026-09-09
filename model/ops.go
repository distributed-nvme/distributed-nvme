package model

import (
	"context"
	"fmt"
	"math"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// ErrPrecondition (MD7)
// ---------------------------------------------------------------------------

// ErrPrecondition is what an op returns from inside its STM callback when one
// of the preconditions it re-validates does not hold (MD7). Op names the
// mutation, Reason says which precondition failed.
//
// Because Unwrap returns etcdutil.ErrNoCommit, returning it aborts the
// transaction WITHOUT commit and WITHOUT retry, and etcdutil.RunSTM hands it
// back to the caller unchanged (EU4), so a caller type-asserts it with
// errors.As. An op that raises it has written nothing at all: the staged
// writes of the aborted attempt never reach etcd.
//
// It is not a failure. The worker logs `reaction skipped` (LG) and
// re-evaluates on its next pass (AR2); nothing sleeps and nothing retries
// here.
type ErrPrecondition struct {
	Op     string
	Reason string
}

// Error renders the op and the precondition that failed (MD7).
func (e *ErrPrecondition) Error() string {
	return fmt.Sprintf("%s: precondition failed: %s", e.Op, e.Reason)
}

// Unwrap returns etcdutil.ErrNoCommit, which is what makes an ErrPrecondition
// a deliberate abort rather than a failure (MD7, EU4).
func (e *ErrPrecondition) Unwrap() error {
	return etcdutil.ErrNoCommit
}

// The Op names of the MD6 mutations. They are the exported function names, so
// that an error message names the op a reader can grep for.
const (
	opSetDnErrEpoch    = "SetDnErrEpoch"
	opSetCnErrEpoch    = "SetCnErrEpoch"
	opSetCntlrErrEpoch = "SetCntlrErrEpoch"
	opSetLegErrEpoch   = "SetLegErrEpoch"
	opSetSideErrEpoch  = "SetSideErrEpoch"
	opFlipProvisioned  = "FlipProvisioned"
	opFlipCreated      = "FlipCreated"
	opFailover         = "Failover"
	opGrowSlice        = "GrowSlice"
	opReplaceCntlr     = "ReplaceCntlr"
	opCreateSpareLeg   = "CreateSpareLeg"
	opSwitchSpareLeg   = "SwitchSpareLeg"
)

// ReasonCandidateChanged is the one reason string MD5 pins: a pick whose
// capacity key is no longer the one the scan saw. The caller rescans and
// retries the scan + STM as one unit (architecture.md §8.4 step 2).
//
// It is exported for the same reason ReasonStaleRevision is: the gateway calls
// GrowSlice and CreateSpareLeg from inside its own candidate unit (gateway.md
// GW9) and must tell "rescan and retry" apart from every other
// ErrPrecondition, which is a FAILED_PRECONDITION for the client.
const ReasonCandidateChanged = "candidate changed"

// ReasonGrowPending is the Reason a GrowSlice carries when the AR6 pending
// rule holds inside its STM. It is exported because worker/reaction.go logs
// the SAME `reaction skipped reason=` string from its own pre-check, and the
// §14 suite greps for one string, not two.
const ReasonGrowPending = "grow_pending"

// ReasonStaleRevision is the Reason the three ops the gateway shares with the
// worker carry when their expectRev argument does not match the stored
// SpRev.revision (gateway.md §2.2 #3). The gateway maps exactly this reason to
// ABORTED "stale revision" (GW7); every other ErrPrecondition reason is a
// FAILED_PRECONDITION, so the two must be distinguishable by string.
const ReasonStaleRevision = "stale revision"

// fail builds one precondition failure (MD7).
func fail(op string, reason string) error {
	return &ErrPrecondition{Op: op, Reason: reason}
}

// ---------------------------------------------------------------------------
// Per-SP id allocation (architecture.md §5.4)
// ---------------------------------------------------------------------------

// SpFirstId is the lowest per-SP sub-object id. Every one of them — cntlr_id,
// slice_id, grp_id, leg_id, side_id, ss_id, ns_id, td_id, clone_id, xfer_id,
// migr_id — comes from the single SpConf.next_id counter, which
// architecture.md §5.4 says "starts at 1".
//
// 0 is therefore not a legal id, and the control plane relies on that: it is
// the reserved "none" sentinel of every id-valued result. failoverCandidate
// returns 0 for "no candidate" and worker/reaction.go tests the plan's
// candidate against 0 the same way, so a cntlr that really carried id 0 would
// be a healthy, enabled, non-primary candidate that AR5 cannot see — and AR7
// would replace the primary in place while a failover target existed.
const SpFirstId = uint64(1)

// SpNextId is SpConf.next_id with 0 reserved (SpFirstId). next_id is a proto3
// uint64, so an SpConf written without it — or one whose counter was reset —
// reads back as the zero value; clamping here is what keeps a minted id from
// colliding with the "none" sentinel. Every id an op mints goes through it.
func SpNextId(conf *pb.SpConf) uint64 {
	if id := conf.GetNextId(); id >= SpFirstId {
		return id
	}
	return SpFirstId
}

// ---------------------------------------------------------------------------
// Thresholds (MD6, AR4, architecture.md §7)
// ---------------------------------------------------------------------------

// ResolveEventThreshold returns a copy of threshold with the §7 defaults
// applied: every 0-valued field falls back to its constant (AR4). It is
// exported because the ops resolve the SP's thresholds inside their STM and
// worker/reaction.go must resolve them exactly the same way when it decides
// whether to call one; nothing may re-implement the rule.
//
// A nil threshold — an SP written without one — resolves to the pure
// defaults. No upper bound applies: §7 only requires each value to be >= 1,
// which is what a resolved zero already is.
func ResolveEventThreshold(threshold *pb.EventThreshold) *pb.EventThreshold {
	resolved := &pb.EventThreshold{
		PrimaryUnhealthy: threshold.GetPrimaryUnhealthy(),
		CntlrUnhealthy:   threshold.GetCntlrUnhealthy(),
		SideUnhealthy:    threshold.GetSideUnhealthy(),
		LegUnhealthy:     threshold.GetLegUnhealthy(),
	}
	if resolved.PrimaryUnhealthy == 0 {
		resolved.PrimaryUnhealthy = common.DefaultPrimaryUnhealthy
	}
	if resolved.CntlrUnhealthy == 0 {
		resolved.CntlrUnhealthy = common.DefaultCntlrUnhealthy
	}
	if resolved.SideUnhealthy == 0 {
		resolved.SideUnhealthy = common.DefaultSideUnhealthy
	}
	if resolved.LegUnhealthy == 0 {
		resolved.LegUnhealthy = common.DefaultLegUnhealthy
	}
	return resolved
}

// thresholdReached is the AR4 comparison `now - err_epoch >= T`, written so
// that it can never underflow: a zero epoch is healthy, and a clock that has
// gone backwards (now < epoch) has simply not reached the threshold yet.
func thresholdReached(now uint64, errEpoch uint64, threshold uint64) bool {
	if errEpoch == 0 {
		return false
	}
	if now < errEpoch {
		return false
	}
	return now-errEpoch >= threshold
}

// ---------------------------------------------------------------------------
// Group geometry (architecture.md §3.6)
// ---------------------------------------------------------------------------

// ceilDiv is ceil(a / b) on unsigned integers; b is never 0 at any call site.
func ceilDiv(a uint64, b uint64) uint64 {
	return (a + b - 1) / b
}

// GroupBlocks computes one group's meta_blocks and data_blocks
// (architecture.md §3.6). Both counts are derived, never configured, and are
// stored in the Group by whoever creates it — CreateStoragePool and GrowSlice
// alike, which is why this is exported.
//
//	total_group_blocks = ext_cnt × extent_size / block_size
//	bitmap_chunk       = bitmap_chunk_block_cnt × block_size
//	bitmap_bits        = ceil(ext_cnt × extent_size / bitmap_chunk)
//	bitmap_bytes       = 256 + ceil(bitmap_bits / 8)   // 256 = md superblock
//	bitmap_blocks      = ceil(bitmap_bytes / block_size)
//	RedundMdRaid1: meta_blocks = 1 + bitmap_blocks + 1 // md sb, bitmap, health
//	RedundNone:    meta_blocks = 1                     // health block only
//	data_blocks        = total_group_blocks − meta_blocks
//
// Defaults are resolved here (§7): extentSize 0 => DefaultDnExtSize,
// dm_pool_conf.data_block_size 0 => DefaultDmPoolDataBlockSize,
// redund_md_raid1.bitmap_chunk_block_cnt 0 => DefaultChunkBlockCnt. A group
// whose data region would be empty is an error, not a zero-sized group.
func GroupBlocks(
	extCnt uint64,
	extentSize uint64,
	bdevConf *pb.BdevConf,
) (uint64, uint64, error) {
	if extCnt == 0 {
		return 0, 0, fmt.Errorf("group blocks: ext_cnt is zero")
	}
	if extentSize == 0 {
		extentSize = common.DefaultDnExtSize
	}
	if extCnt > math.MaxUint64/extentSize {
		return 0, 0, fmt.Errorf(
			"group blocks: ext_cnt %d × extent_size %d overflows",
			extCnt, extentSize,
		)
	}
	blockSize := bdevConf.GetDmPoolConf().GetDataBlockSize()
	if blockSize == 0 {
		blockSize = common.DefaultDmPoolDataBlockSize
	}
	groupSize := extCnt * extentSize
	totalBlocks := groupSize / blockSize
	// The health block of §3.6 is the only meta a RedundNone group needs.
	metaBlocks := uint64(1)
	if raid1 := bdevConf.GetRedundConf().GetRedundMdRaid1(); raid1 != nil {
		chunkBlockCnt := raid1.GetBitmapChunkBlockCnt()
		if chunkBlockCnt == 0 {
			chunkBlockCnt = common.DefaultChunkBlockCnt
		}
		bitmapBits := ceilDiv(groupSize, chunkBlockCnt*blockSize)
		bitmapBytes := 256 + ceilDiv(bitmapBits, 8)
		bitmapBlocks := ceilDiv(bitmapBytes, blockSize)
		metaBlocks = 1 + bitmapBlocks + 1
	}
	if totalBlocks <= metaBlocks {
		return 0, 0, fmt.Errorf(
			"group blocks: %d blocks cannot hold %d meta blocks",
			totalBlocks, metaBlocks,
		)
	}
	return metaBlocks, totalBlocks - metaBlocks, nil
}

// MetaLadderExtCnt is the §8.5 meta ladder: the ext_cnt of the meta group a
// meta GrowSlice would append to a slice whose meta groups currently total
// currentTotal extents. The first meta group (created with the SP) is 1
// extent, and every further meta grow adds the slice's current meta total, so
// the totals run 1 → 2 → 4 → 8 → 16 …
//
// ok is false once the total has reached the 16 GiB dm-thin metadata ceiling,
// which is where §8.5 refuses to grow further; the cap is expressed in bytes,
// so it holds for any extent size (with 1 TiB extents the very first meta
// group is already past it and no meta grow is ever allowed). A slice with no
// meta group at all also reports false: the ladder has nothing to double.
func MetaLadderExtCnt(currentTotal uint64, extentSize uint64) (uint64, bool) {
	if currentTotal == 0 {
		return 0, false
	}
	if extentSize == 0 {
		extentSize = common.DefaultDnExtSize
	}
	if currentTotal > math.MaxUint64/extentSize {
		return 0, false
	}
	if currentTotal*extentSize >= metaSizeCap {
		return 0, false
	}
	return currentTotal, true
}

// metaSizeCap is the 16 GiB dm-thin metadata ceiling of §8.5.
const metaSizeCap = uint64(16) * 1024 * 1024 * 1024

// ---------------------------------------------------------------------------
// Shared STM helpers
// ---------------------------------------------------------------------------

// loadSpConfForOp reads an SP's configuration and applies the three checks
// every SP mutation of MD6 shares (AR3): the SP exists and is the one the
// caller addressed, it is not being deleted, and its sp_level has not reached
// SP_LEVEL_NO_THINPOOL — the disaster-recovery levels where an operator is in
// charge.
//
// The sp_id check exists because the SpConf is name-keyed: an SP deleted and
// re-created under the same name between the caller's snapshot and this STM
// is a different SP, and none of the ids the caller carries mean anything in
// it.
func loadSpConfForOp(
	s etcdutil.STM,
	op string,
	cid uint64,
	spName string,
	spId uint64,
) (*pb.SpConf, error) {
	conf := &pb.SpConf{}
	if !s.Get(SpConfKey(cid, spName), conf) {
		return nil, fail(op, "sp not found")
	}
	if conf.GetSpId() != spId {
		return nil, fail(op, "sp id changed")
	}
	if conf.GetDeleting() {
		return nil, fail(op, "sp deleting")
	}
	if conf.GetSpLevel() >= pb.SpLevel_SP_LEVEL_NO_THINPOOL {
		return nil, fail(op, "sp level suppresses reactions")
	}
	return conf, nil
}

// checkSpRev is the optimistic-concurrency gate the gateway asks the three
// shared ops to apply before they touch anything (gateway.md §2.2 #3, §5.5):
// the stored SpRev.revision MUST equal expectRev. expectRev 0 skips the check
// entirely, which is what the worker's own internal calls pass — the worker
// converges on what etcd holds and carries no client token. The gateway never
// passes 0: a nil or zero request token is short-circuited to ABORTED by the
// handler itself (GW6), so model never sees one.
//
// It runs FIRST inside the op's STM, before every other precondition, so that
// a stale caller always sees "stale revision" rather than a precondition
// computed against state it has not read.
func checkSpRev(
	s etcdutil.STM,
	op string,
	shard uint32,
	cid uint64,
	spId uint64,
	expectRev uint64,
) error {
	if expectRev == 0 {
		return nil
	}
	rev := &pb.SpRev{}
	if !s.Get(SpRevKey(shard, cid, spId), rev) {
		return fail(op, "sp rev not found")
	}
	if rev.GetRevision() != expectRev {
		return fail(op, ReasonStaleRevision)
	}
	return nil
}

// BumpSpRev writes the SP's revision key with revision + 1 and the sp_name it
// already holds (§5.5): the key is id-based and therefore stable, so it is
// rewritten in place and never deleted and re-created — a watcher must see one
// put, not a delete followed by a put.
func BumpSpRev(
	s etcdutil.STM,
	op string,
	shard uint32,
	cid uint64,
	spId uint64,
) error {
	key := SpRevKey(shard, cid, spId)
	rev := &pb.SpRev{}
	if !s.Get(key, rev) {
		return fail(op, "sp rev not found")
	}
	rev.Revision++
	s.Put(key, rev)
	return nil
}

// BumpDnRev bumps one DN's revision key in place (§5.5). The key is addressed
// by the shard code and dn_id the DnConf carries.
func BumpDnRev(
	s etcdutil.STM,
	op string,
	cid uint64,
	dn *pb.DnConf,
) error {
	key := DnRevKey(dn.GetShardCode(), cid, dn.GetDnId())
	rev := &pb.DnRev{}
	if !s.Get(key, rev) {
		return fail(op, "dn rev not found")
	}
	rev.Revision++
	s.Put(key, rev)
	return nil
}

// BumpCnRev bumps one CN's revision key in place (§5.5).
func BumpCnRev(
	s etcdutil.STM,
	op string,
	cid uint64,
	cn *pb.CnConf,
) error {
	key := CnRevKey(cn.GetShardCode(), cid, cn.GetCnId())
	rev := &pb.CnRev{}
	if !s.Get(key, rev) {
		return fail(op, "cn rev not found")
	}
	rev.Revision++
	s.Put(key, rev)
	return nil
}

// applyErrEpoch is the set/clear rule every Set*ErrEpoch shares (MD6, HL3):
// a nonzero epoch is written only onto a stored 0 — the threshold clock of §11
// never restarts — and a zero epoch always clears. It returns the value to
// store and whether that differs from what is stored, so that a caller writes
// nothing when nothing changed.
func applyErrEpoch(stored uint64, epoch uint64) (uint64, bool) {
	if epoch == 0 {
		return 0, stored != 0
	}
	if stored != 0 {
		return stored, false
	}
	return epoch, true
}

// allLegs returns every leg of a slice — both group lists, and both the active
// and the spare list of every group — in a stable order. The MD6 err_epoch ops
// look a leg or a side up in all of them: a spare's leg and side are
// health-checked exactly like an active one (§8.12).
func allLegs(slice *pb.Slice) []*pb.Leg {
	grpLists := [][]*pb.Group{
		slice.GetMetaGrpList(),
		slice.GetDataGrpList(),
	}
	var legs []*pb.Leg
	for _, grpList := range grpLists {
		for _, grp := range grpList {
			legs = append(legs, grp.GetLegList()...)
			legs = append(legs, grp.GetSpareLegList()...)
		}
	}
	return legs
}

// findLeg returns the leg with legId anywhere in the slice, or nil.
func findLeg(slice *pb.Slice, legId uint64) *pb.Leg {
	for _, leg := range allLegs(slice) {
		if leg.GetLegId() == legId {
			return leg
		}
	}
	return nil
}

// findSide returns the side with sideId anywhere in the slice, or nil. Side
// ids come from SpConf.next_id and are therefore unique within the SP, so the
// leg id is not needed to address one.
func findSide(slice *pb.Slice, sideId uint64) *pb.Side {
	for _, leg := range allLegs(slice) {
		for _, side := range leg.GetSideList() {
			if side.GetSideId() == sideId {
				return side
			}
		}
	}
	return nil
}

// findSideOfLeg returns the side with sideId inside the leg with legId, or
// nil. RW18 reports a side by its whole side pointer, and checking both ids
// keeps a stale pointer from flipping an unrelated side.
func findSideOfLeg(slice *pb.Slice, legId uint64, sideId uint64) *pb.Side {
	leg := findLeg(slice, legId)
	if leg == nil {
		return nil
	}
	for _, side := range leg.GetSideList() {
		if side.GetSideId() == sideId {
			return side
		}
	}
	return nil
}

// findGroup returns the group with grpId, searching the meta list and then the
// data list of the slice (§8.12: a spare may be created on either kind).
func findGroup(slice *pb.Slice, grpId uint64) *pb.Group {
	grpLists := [][]*pb.Group{
		slice.GetMetaGrpList(),
		slice.GetDataGrpList(),
	}
	for _, grpList := range grpLists {
		for _, grp := range grpList {
			if grp.GetGrpId() == grpId {
				return grp
			}
		}
	}
	return nil
}

// isMdRaid1 reports whether the SP's groups are md-raid1 groups. Redundancy is
// an SP-wide property (SpConf.bdev_conf.redund_conf); a group only inherits
// it.
func isMdRaid1(conf *pb.SpConf) bool {
	return conf.GetBdevConf().GetRedundConf().GetRedundMdRaid1() != nil
}

// legCntOf is how many legs one group of the SP has: 2 for RedundMdRaid1, 1
// for RedundNone (§11.3).
func legCntOf(conf *pb.SpConf) int {
	if isMdRaid1(conf) {
		return 2
	}
	return 1
}

// firstCntlidSlot is the slot every new SIDE is written with:
// cntlid_slot_list[0] (§8.4). Sides of different legs may share slots freely
// (§11.8), so no search for an unused one is needed — but an SpConf with an
// empty list cannot produce a side at all.
func firstCntlidSlot(conf *pb.SpConf, op string) (uint32, error) {
	slots := conf.GetCntlidSlotList()
	if len(slots) == 0 {
		return 0, fail(op, "empty cntlid_slot_list")
	}
	return slots[0], nil
}

// checkDnPick re-validates one allocator pick inside the caller's STM (MD5):
// the DnConf still exists and is allocatable, it still has room for extCnt
// extents, and — the exact test — the capacity key the scan saw is still
// there. That key embeds free_ext_cnt, so its presence proves the DN has not
// been touched since the scan; its absence is `candidate changed` and the
// caller rescans.
func checkDnPick(
	s etcdutil.STM,
	op string,
	cid uint64,
	cc *pb.ClusterConf,
	cand Cand,
	extCnt uint64,
) (*pb.DnConf, error) {
	dn := &pb.DnConf{}
	if !s.Get(DnConfKey(cid, cand.AddrPort), dn) {
		return nil, fail(op, "dn not found")
	}
	if !DnAllocatable(dn, cc.GetDnBinConf()) {
		return nil, fail(op, "dn not allocatable")
	}
	if dn.GetFreeExtCnt() < extCnt {
		return nil, fail(op, "dn free_ext_cnt too low")
	}
	capKey := DnCapacityKey(cid, cand.BinIdx, cand.FreeExt, cand.AddrPort)
	if !s.Get(capKey, &pb.DnCapacity{}) {
		return nil, fail(op, ReasonCandidateChanged)
	}
	return dn, nil
}

// chargeDn is the DN bookkeeping every side allocation performs (§5.6, §8.4):
// the side pointer goes in, extCnt leaves free_ext_cnt, the capacity key
// follows the §5.6 presence rule, and the DN's revision is bumped once.
func chargeDn(
	s etcdutil.STM,
	op string,
	cid uint64,
	cc *pb.ClusterConf,
	addrPort string,
	dn *pb.DnConf,
	ptr *pb.SidePointer,
	extCnt uint64,
) error {
	newDn := proto.Clone(dn).(*pb.DnConf)
	newDn.SidePtrList = append(newDn.SidePtrList, ptr)
	newDn.FreeExtCnt -= extCnt
	s.Put(DnConfKey(cid, addrPort), newDn)
	MaintainDnCapacity(s, cid, addrPort, cc, dn, newDn)
	return BumpDnRev(s, op, cid, newDn)
}

// spFootprint is the Σ ext_cnt over ALL groups of ALL slices of the SP — meta
// and data alike — which is what one cntlr's CN reserves for the SP (§8.4,
// §8.6). Slices are read through the caller's STM; a listed slice that does
// not exist is a precondition failure, since a footprint computed from a
// partial slice list would undercharge the CN.
func spFootprint(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
) (uint64, error) {
	total := uint64(0)
	for _, sliceId := range conf.GetSliceIdList() {
		key := SliceKey(cid, conf.GetSpId(), sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return 0, fail(op, "slice not found")
		}
		grpLists := [][]*pb.Group{
			slice.GetMetaGrpList(),
			slice.GetDataGrpList(),
		}
		for _, grpList := range grpLists {
			for _, grp := range grpList {
				total += grp.GetExtCnt()
			}
		}
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// err_epoch maintenance (MD6, HL1/HL2/HL3)
// ---------------------------------------------------------------------------

// SetDnErrEpoch sets or clears a DN's err_epoch (MD6, HL1). A nonzero epoch is
// written only when the stored value is 0 — the §11 threshold clock never
// restarts — and epoch 0 always clears; a record that already holds what is
// wanted is not written at all, so two owners observing the same transition
// write once (HL3).
//
// The capacity key follows in the same transaction (MD4, §5.6): err_epoch is
// an input of the presence rule, and the old record read HERE is what makes
// the delete of the old key exact. Nothing bumps a revision — err_epoch only
// gates control-plane scheduling (§5.5).
func SetDnErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	addrPort string,
	epoch uint64,
	cc *pb.ClusterConf,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := DnConfKey(cid, addrPort)
		dn := &pb.DnConf{}
		if !s.Get(key, dn) {
			return fail(opSetDnErrEpoch, "dn not found")
		}
		value, changed := applyErrEpoch(dn.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		newDn := proto.Clone(dn).(*pb.DnConf)
		newDn.ErrEpoch = value
		s.Put(key, newDn)
		MaintainDnCapacity(s, cid, addrPort, cc, dn, newDn)
		return nil
	})
}

// SetCnErrEpoch sets or clears a CN's err_epoch (MD6, HL1) under the same
// rule as SetDnErrEpoch, maintaining the CN capacity key in the same STM and
// bumping no revision. It takes no ClusterConf: CN capacity keys carry no bin
// index, so nothing about them depends on dn_bin_conf (§6.4).
func SetCnErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	addrPort string,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := CnConfKey(cid, addrPort)
		cn := &pb.CnConf{}
		if !s.Get(key, cn) {
			return fail(opSetCnErrEpoch, "cn not found")
		}
		value, changed := applyErrEpoch(cn.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		newCn := proto.Clone(cn).(*pb.CnConf)
		newCn.ErrEpoch = value
		s.Put(key, newCn)
		MaintainCnCapacity(s, cid, addrPort, cn, newCn)
		return nil
	})
}

// SetCntlrErrEpoch sets or clears a cntlr's err_epoch (MD6, HL2) under the
// same set/clear rule. A Cntlr has no capacity key and health never bumps a
// revision, so this is the whole mutation.
func SetCntlrErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	cntlrId uint64,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := CntlrKey(cid, spId, cntlrId)
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			return fail(opSetCntlrErrEpoch, "cntlr not found")
		}
		value, changed := applyErrEpoch(cntlr.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		cntlr.ErrEpoch = value
		s.Put(key, cntlr)
		return nil
	})
}

// SetLegErrEpoch sets or clears one leg's err_epoch (MD6, HL2). The leg is
// looked up in EVERY group of the slice — meta and data — and in both the
// leg_list and the spare_leg_list, because the primary probes a spare's leg
// exactly like an active one (§8.12). Legs are embedded in the Slice, so the
// whole Slice is rewritten; nothing bumps a revision.
func SetLegErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	sliceId uint64,
	legId uint64,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return fail(opSetLegErrEpoch, "slice not found")
		}
		leg := findLeg(slice, legId)
		if leg == nil {
			return fail(opSetLegErrEpoch, "leg not found")
		}
		value, changed := applyErrEpoch(leg.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		leg.ErrEpoch = value
		s.Put(key, slice)
		return nil
	})
}

// SetSideErrEpoch sets or clears one side's err_epoch (MD6, HL2), looking the
// side up in every group and in both leg lists like SetLegErrEpoch. Side ids
// come from SpConf.next_id and are unique within the SP, so the slice id and
// the side id address it.
func SetSideErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	sliceId uint64,
	sideId uint64,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return fail(opSetSideErrEpoch, "slice not found")
		}
		side := findSide(slice, sideId)
		if side == nil {
			return fail(opSetSideErrEpoch, "side not found")
		}
		value, changed := applyErrEpoch(side.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		side.ErrEpoch = value
		s.Put(key, slice)
		return nil
	})
}

// ---------------------------------------------------------------------------
// The two flips (MD6, §10.3)
// ---------------------------------------------------------------------------

// SideRef names one side of an SP for FlipProvisioned (MD6): the slice that
// holds it plus the side pointer RW18 reports.
type SideRef struct {
	SliceId uint64
	LegId   uint64
	SideId  uint64
}

// TdRef names one thin device for FlipCreated (MD6): the name that keys the
// record and the td_id RW19 observed complete. Both are needed — the name
// addresses the key, and the id is what proves the record is still the one
// that was observed (§10.3).
type TdRef struct {
	Name string
	TdId uint64
}

// FlipProvisioned sets Side.provisioned on every listed side that is still
// false and bumps SpRev exactly once if at least one was flipped (MD6, RW18,
// §10.3). It returns the sides it ACTUALLY wrote, in the order they were
// listed; the MD6 count is len() of that slice.
//
// The refs and not a bare count, because the caller logs one §12
// "flip applied" record per side and that record names the side (`ids`): a
// candidate this STM skipped was never flipped by this worker, and logging it
// would attribute a write — and a revision — to an owner that did not cause
// either.
//
// A side that has already been flipped — by another owner, or by an earlier
// round — is skipped, which is what makes the flip idempotent and lets the
// bump happen at most once per call. A listed side that is not in its slice
// any more is skipped too: RW18 reports what a round observed, and a
// disappeared side is a stale observation, not a failure (the MD6 row lists
// only "slice exists" as a precondition). A listed SLICE that is gone IS a
// precondition failure: the whole batch is dropped and the next round
// re-reports whatever is still unflipped.
func FlipProvisioned(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	sides []SideRef,
) ([]SideRef, error) {
	var flipped []SideRef
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		// A retried attempt (EU4) re-reads everything, so the report of the
		// abandoned one is dropped with it.
		flipped = nil
		// Every slice is read once and written at most once, however many
		// of its sides the batch names.
		slices := make(map[uint64]*pb.Slice)
		var touched []uint64
		for _, ref := range sides {
			slice, ok := slices[ref.SliceId]
			if !ok {
				slice = &pb.Slice{}
				key := SliceKey(cid, spId, ref.SliceId)
				if !s.Get(key, slice) {
					return fail(opFlipProvisioned, "slice not found")
				}
				slices[ref.SliceId] = slice
			}
			side := findSideOfLeg(slice, ref.LegId, ref.SideId)
			if side == nil || side.GetProvisioned() {
				continue
			}
			side.Provisioned = true
			flipped = append(flipped, ref)
			if !containsId(touched, ref.SliceId) {
				touched = append(touched, ref.SliceId)
			}
		}
		if len(flipped) == 0 {
			return nil
		}
		for _, sliceId := range touched {
			s.Put(SliceKey(cid, spId, sliceId), slices[sliceId])
		}
		return BumpSpRev(s, opFlipProvisioned, shard, cid, spId)
	})
	if err != nil {
		return nil, err
	}
	return flipped, nil
}

// containsId reports whether ids already holds id. The lists it guards hold at
// most MaxSliceCntPerSp entries, so a linear scan is the right shape.
func containsId(ids []uint64, id uint64) bool {
	for _, item := range ids {
		if item == id {
			return true
		}
	}
	return false
}

// FlipCreated sets ThinDevice.created on every listed candidate that still
// needs it and bumps SpRev exactly once if at least one was written (MD6,
// RW19, §10.3 / ThinDeviceCreated.md U3). It returns the candidates it
// ACTUALLY wrote, in the order they were listed; the MD6 count is len() of
// that slice.
//
// The refs and not a bare count, for FlipProvisioned's reason: the caller logs
// one §12 "flip applied" record per td, naming it, and a skipped candidate was
// never created by this worker.
//
// A candidate is skipped — never an error — when its key is absent (the td was
// deleted meanwhile), when its td_id differs (deleted and re-created under the
// same name), or when created is already true (another worker or a concurrent
// reply got there first). Nothing written ⇒ no bump, and a call whose whole
// batch is skipped leaves etcd untouched.
func FlipCreated(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	cands []TdRef,
) ([]TdRef, error) {
	var created []TdRef
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		created = nil
		for _, cand := range cands {
			key := ThinDeviceKey(cid, spId, cand.Name)
			td := &pb.ThinDevice{}
			if !s.Get(key, td) {
				continue
			}
			if td.GetTdId() != cand.TdId || td.GetCreated() {
				continue
			}
			td.Created = true
			s.Put(key, td)
			created = append(created, cand)
		}
		if len(created) == 0 {
			return nil
		}
		return BumpSpRev(s, opFlipCreated, shard, cid, spId)
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ---------------------------------------------------------------------------
// Failover (MD6, AR5, §10.4, §11.1)
// ---------------------------------------------------------------------------

// failoverCandidate is the AR5 election run inside an STM: the cntlr with the
// smallest cntlr_id among those that are not primary, not disabled and
// healthy. It returns 0 when there is none — 0 is the reserved "none" sentinel
// of every id-valued result here, which is exactly why no id is ever minted
// below SpFirstId (SpNextId). A listed cntlr whose key is missing simply cannot
// be elected.
func failoverCandidate(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) uint64 {
	best := uint64(0)
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr := &pb.Cntlr{}
		if !s.Get(CntlrKey(cid, conf.GetSpId(), cntlrId), cntlr) {
			continue
		}
		if cntlr.GetPrimary() || cntlr.GetDisabled() {
			continue
		}
		if cntlr.GetErrEpoch() != 0 {
			continue
		}
		if best == 0 || cntlrId < best {
			best = cntlrId
		}
	}
	return best
}

// Failover moves the primary role from oldId to newId (MD6, AR5, §10.4): the
// old primary has been unhealthy for primary_unhealthy seconds and the new one
// is the healthy, enabled, non-primary cntlr with the smallest cntlr_id. Both
// primary booleans flip in one STM and SpRev is bumped once; the data-plane
// choreography is §11.1's and belongs to the agents.
//
// Every precondition is re-validated here, election included: two owners
// overlapping on one SP (§0 item 4) cannot both apply it, because the second
// finds the old cntlr no longer primary.
func Failover(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newId uint64,
	now uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := loadSpConfForOp(s, opFailover, cid, spName, spId)
		if err != nil {
			return err
		}
		if oldId == newId {
			return fail(opFailover, "old and new cntlr are the same")
		}
		threshold := ResolveEventThreshold(conf.GetEventThreshold())
		oldKey := CntlrKey(cid, spId, oldId)
		old := &pb.Cntlr{}
		if !s.Get(oldKey, old) {
			return fail(opFailover, "old cntlr not found")
		}
		if !old.GetPrimary() {
			return fail(opFailover, "old cntlr is not primary")
		}
		if old.GetErrEpoch() == 0 {
			return fail(opFailover, "old cntlr is healthy")
		}
		if !thresholdReached(
			now, old.GetErrEpoch(),
			uint64(threshold.GetPrimaryUnhealthy()),
		) {
			return fail(opFailover, "primary_unhealthy not reached")
		}
		newKey := CntlrKey(cid, spId, newId)
		fresh := &pb.Cntlr{}
		if !s.Get(newKey, fresh) {
			return fail(opFailover, "new cntlr not found")
		}
		if fresh.GetPrimary() {
			return fail(opFailover, "new cntlr is already primary")
		}
		if fresh.GetDisabled() {
			return fail(opFailover, "new cntlr is disabled")
		}
		if fresh.GetErrEpoch() != 0 {
			return fail(opFailover, "new cntlr is unhealthy")
		}
		// The election is part of the transaction: a healthier, smaller
		// candidate that appeared since the pass read its snapshot wins.
		if failoverCandidate(s, cid, conf) != newId {
			return fail(opFailover, "new cntlr is not the smallest candidate")
		}
		old.Primary = false
		fresh.Primary = true
		s.Put(oldKey, old)
		s.Put(newKey, fresh)
		return BumpSpRev(s, opFailover, shard, cid, spId)
	})
}

// ---------------------------------------------------------------------------
// GrowSlice (MD6, AR6, §8.5)
// ---------------------------------------------------------------------------

// GrowSlice appends one new group to a slice (MD6, §8.5) and returns its
// grp_id. legs carries one allocator pick per leg of the new group — 2 for a
// RedundMdRaid1 SP, 1 for RedundNone — and every one of them is re-validated
// inside the STM (MD5).
//
// The new group's size is not the caller's to choose: for a data grow it is
// the slice's FIRST data group's ext_cnt (grow by the original allocation
// unit), for a meta grow the §8.5 ladder value, which doubles the slice's meta
// total and is refused once that total reaches 16 GiB. meta_blocks and
// data_blocks follow §3.6 from the SP's own bdev_conf and the cluster's
// extent_size.
//
// poolTotal is the total the primary reported for the pool of THIS kind —
// total_data for a data grow, total_meta for a meta one (AR6) — and is what
// makes the AR6 pending rule a precondition of the transaction: the caller has
// already found the grow not pending against its own snapshot, and this
// re-runs the same rule against the slice as it is NOW. Without it AR6 would
// be the one reaction whose second attempt cannot fail (AR2): during an
// accepted shard-handoff overlap (§0 item 4) two owners evaluating the same
// pre-grow snapshot both find the grow not pending, and the second would
// append a second group for one breach — twice the DN extents and twice the CN
// footprint, undoable only by an operator. Their allocator picks are drawn at
// random, so the MD5 capacity guard does not cover this. The gateway passes
// `math.MaxUint64` — a user-driven grow is not gated on the reported usage
// (architecture.md §8.5, gateway.md §5.4).
//
// Effects, all in the one transaction: the Group with one Leg and one
// unprovisioned Side per pick ([D15]); each picked DN's side pointer, budget,
// capacity key and DnRev; the budget, capacity key and CnRev of the CN of
// EVERY cntlr of the SP — a new group is stacked by all of them, so all of
// them reserve it; the Slice, the SpConf whose next_id fed the ids, and one
// SpRev bump.
func GrowSlice(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	sliceId uint64,
	isMeta bool,
	poolTotal uint64,
	cc *pb.ClusterConf,
	legs []Cand,
) (uint64, error) {
	grpId := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		grpId = 0
		if err := checkSpRev(
			s, opGrowSlice, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfForOp(s, opGrowSlice, cid, spName, spId)
		if err != nil {
			return err
		}
		if !containsId(conf.GetSliceIdList(), sliceId) {
			return fail(opGrowSlice, "slice not in sp")
		}
		sliceKey := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(sliceKey, slice) {
			return fail(opGrowSlice, "slice not found")
		}
		// AR6 re-validated on the slice this transaction read (AR2): a grow
		// another owner has already appended makes this one pending.
		if GrowPending(
			slice, isMeta, poolTotal, PoolBlockSize(conf.GetBdevConf()),
		) {
			return fail(opGrowSlice, ReasonGrowPending)
		}
		extentSize := ResolveDnBinConf(cc.GetDnBinConf()).GetExtentSize()
		extCnt, err := growExtCnt(slice, isMeta, extentSize)
		if err != nil {
			return err
		}
		if len(legs) != legCntOf(conf) {
			return fail(opGrowSlice, "wrong leg count")
		}
		if hasDuplicateAddr(legs) {
			return fail(opGrowSlice, "duplicate candidate")
		}
		slot, err := firstCntlidSlot(conf, opGrowSlice)
		if err != nil {
			return err
		}
		metaBlocks, dataBlocks, err := GroupBlocks(
			extCnt, extentSize, conf.GetBdevConf(),
		)
		if err != nil {
			return fail(opGrowSlice, err.Error())
		}
		// Ids are consumed in one run so that a retried attempt, which
		// re-reads next_id, produces exactly the same ones.
		nextId := SpNextId(conf)
		grpId = nextId
		nextId++
		grp := &pb.Group{
			GrpId:      grpId,
			ExtCnt:     extCnt,
			MetaBlocks: metaBlocks,
			DataBlocks: dataBlocks,
		}
		for legIdx, cand := range legs {
			dn, err := checkDnPick(s, opGrowSlice, cid, cc, cand, extCnt)
			if err != nil {
				return err
			}
			legId := nextId
			nextId++
			sideId := nextId
			nextId++
			grp.LegList = append(grp.LegList, &pb.Leg{
				LegId:  legId,
				LegIdx: uint32(legIdx),
				SideList: []*pb.Side{{
					SideId:      sideId,
					AddrPort:    cand.AddrPort,
					CntlidSlot:  slot,
					NvmeTrConf:  dn.GetNvmeTrConf(),
					ErrEpoch:    0,
					Provisioned: false,
				}},
			})
			ptr := &pb.SidePointer{
				SpId:   spId,
				LegId:  legId,
				SideId: sideId,
			}
			err = chargeDn(
				s, opGrowSlice, cid, cc, cand.AddrPort, dn, ptr, extCnt,
			)
			if err != nil {
				return err
			}
		}
		if err := chargeSpCns(s, opGrowSlice, cid, conf, extCnt); err != nil {
			return err
		}
		if isMeta {
			slice.MetaGrpList = append(slice.MetaGrpList, grp)
		} else {
			slice.DataGrpList = append(slice.DataGrpList, grp)
		}
		conf.NextId = nextId
		s.Put(sliceKey, slice)
		s.Put(SpConfKey(cid, spName), conf)
		return BumpSpRev(s, opGrowSlice, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return grpId, nil
}

// growExtCnt is the size of the group a GrowSlice appends (§8.5): the slice's
// first data group's ext_cnt for a data grow — the original allocation unit —
// and the meta ladder value for a meta grow, which is refused at the 16 GiB
// dm-thin metadata ceiling.
func growExtCnt(
	slice *pb.Slice,
	isMeta bool,
	extentSize uint64,
) (uint64, error) {
	if !isMeta {
		if len(slice.GetDataGrpList()) == 0 {
			return 0, fail(opGrowSlice, "slice has no data group")
		}
		extCnt := slice.GetDataGrpList()[0].GetExtCnt()
		if extCnt == 0 {
			return 0, fail(opGrowSlice, "first data group is empty")
		}
		return extCnt, nil
	}
	total := uint64(0)
	for _, grp := range slice.GetMetaGrpList() {
		total += grp.GetExtCnt()
	}
	extCnt, ok := MetaLadderExtCnt(total, extentSize)
	if !ok {
		return 0, fail(opGrowSlice, "meta ladder at the 16 GiB cap")
	}
	return extCnt, nil
}

// thinMetaBlockSize is dm-thin's FIXED metadata block size (AR6): a thin
// pool's metadata used/total counts are in these 4 KiB blocks, never in the
// pool's data_block_size.
const thinMetaBlockSize = uint64(4096)

// PoolBlockSize resolves the §7 default of a pool's data block size: the unit
// the thin pool's DATA counts are expressed in, and the one that converts a
// meta group's data region into dm-thin metadata blocks (AR6, §3.6).
func PoolBlockSize(bdevConf *pb.BdevConf) uint64 {
	size := bdevConf.GetDmPoolConf().GetDataBlockSize()
	if size == 0 {
		size = common.DefaultDmPoolDataBlockSize
	}
	return size
}

// GrowPending is AR6's stateless pending rule: a grow of a kind is pending
// while the pool's REPORTED total of that kind is no larger than the total
// implied by the slice's groups of that kind excluding the newest one.
//
//	data: total_data ≤ Σ data_blocks over all data groups but the last
//	meta: total_meta ≤ Σ data_blocks × block_size / 4096 over all meta groups
//	      but the last
//
// It is a memo reconstructed from facts (§0 item 14), so a worker restart or a
// shard handoff cannot issue a second grow, and a grow deferred on the CN
// ([D15]) stays pending the same way because its totals have not moved.
//
// With zero or one group of a kind the sum is over an empty set and nothing is
// pending — the first grow of a slice must never be held back by its own
// (as yet unreported) group.
//
// It lives here, not in worker/reaction.go, because both sides of AR6 apply
// it: the pass as its cheap pre-check on the snapshot it holds, and GrowSlice
// as the precondition AR2 requires it to re-validate inside its STM. One rule,
// one implementation.
func GrowPending(
	slice *pb.Slice,
	isMeta bool,
	reportedTotal uint64,
	blockSize uint64,
) bool {
	grps := slice.GetDataGrpList()
	if isMeta {
		grps = slice.GetMetaGrpList()
	}
	if len(grps) < 2 {
		return false
	}
	total := uint64(0)
	for _, grp := range grps[:len(grps)-1] {
		blocks := grp.GetDataBlocks()
		if isMeta {
			// The pool's metadata device is the concat of the meta groups'
			// DATA regions, addressed in 4 KiB metadata blocks.
			blocks = blocks * blockSize / thinMetaBlockSize
		}
		total += blocks
	}
	return reportedTotal <= total
}

// hasDuplicateAddr reports whether two picks name the same node. Two legs of
// one group on one DN would defeat the redundancy the group exists for, and
// one node charged twice in one STM would leave its capacity key wrong.
func hasDuplicateAddr(cands []Cand) bool {
	seen := make(map[string]struct{}, len(cands))
	for _, cand := range cands {
		if _, ok := seen[cand.AddrPort]; ok {
			return true
		}
		seen[cand.AddrPort] = struct{}{}
	}
	return false
}

// chargeSpCns takes extCnt extents off the CN of every cntlr of the SP, one
// budget update, one capacity-key maintenance and one CnRev bump per CN
// (§8.5). A CN hosting two cntlrs of one SP is not supposed to exist (§6.4),
// but if one did it would reserve the group twice, so the charge is
// accumulated per CN and applied once.
func chargeSpCns(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
	extCnt uint64,
) error {
	var order []string
	cns := make(map[string]*pb.CnConf)
	charges := make(map[string]uint64)
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr := &pb.Cntlr{}
		if !s.Get(CntlrKey(cid, conf.GetSpId(), cntlrId), cntlr) {
			return fail(op, "cntlr not found")
		}
		addrPort := cntlr.GetAddrPort()
		if _, ok := cns[addrPort]; !ok {
			cn := &pb.CnConf{}
			if !s.Get(CnConfKey(cid, addrPort), cn) {
				return fail(op, "cn not found")
			}
			cns[addrPort] = cn
			order = append(order, addrPort)
		}
		charges[addrPort] += extCnt
	}
	for _, addrPort := range order {
		cn := cns[addrPort]
		charge := charges[addrPort]
		if cn.GetFreeExtCnt() < charge {
			return fail(op, "cn free_ext_cnt too low")
		}
		newCn := proto.Clone(cn).(*pb.CnConf)
		newCn.FreeExtCnt -= charge
		s.Put(CnConfKey(cid, addrPort), newCn)
		MaintainCnCapacity(s, cid, addrPort, cn, newCn)
		if err := BumpCnRev(s, op, cid, newCn); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ReplaceCntlr (MD6, AR7, §8.6 ×2 in one STM)
// ---------------------------------------------------------------------------

// ReplaceCntlr deletes a cntlr that has been unhealthy for cntlr_unhealthy
// seconds and creates its replacement on newCn with the SAME cntlid_slot (MD6,
// AR7, §10.4). It returns the new cntlr_id. This is §8.6's DeleteCntlr and
// CreateCntlr in one transaction — the SP is never left with fewer cntlrs than
// it had.
//
// asPrimary carries the sole-primary variant of AR7: the primary of an SP with
// no failover candidate is replaced by a new primary. It must therefore be
// true when the old cntlr is the primary (demoting it silently would leave the
// SP without one), and it may not be true while another cntlr still holds the
// role.
//
// Effects: the old Cntlr key is deleted and, if its CN record still exists,
// that CN gets its pointer removed, the SP footprint back, its capacity key
// maintained and its CnRev bumped; the new Cntlr is written with
// cntlr_id = SpConf.next_id++ and its CN charged the same way; every CdcEntry
// of the SP loses the old nvme_tr_conf and gains the new one; SpConf is
// rewritten and SpRev bumped once.
func ReplaceCntlr(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newCn Cand,
	asPrimary bool,
	now uint64,
) (uint64, error) {
	newId := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		newId = 0
		conf, err := loadSpConfForOp(s, opReplaceCntlr, cid, spName, spId)
		if err != nil {
			return err
		}
		threshold := ResolveEventThreshold(conf.GetEventThreshold())
		oldKey := CntlrKey(cid, spId, oldId)
		old := &pb.Cntlr{}
		if !s.Get(oldKey, old) {
			return fail(opReplaceCntlr, "old cntlr not found")
		}
		if old.GetErrEpoch() == 0 {
			return fail(opReplaceCntlr, "old cntlr is healthy")
		}
		if !thresholdReached(
			now, old.GetErrEpoch(),
			uint64(threshold.GetCntlrUnhealthy()),
		) {
			return fail(opReplaceCntlr, "cntlr_unhealthy not reached")
		}
		if old.GetDisabled() {
			// AR3: disabling is the operator's hands-off signal.
			return fail(opReplaceCntlr, "old cntlr is disabled")
		}
		if old.GetPrimary() {
			if !asPrimary {
				return fail(
					opReplaceCntlr,
					"primary replacement must stay primary",
				)
			}
			if failoverCandidate(s, cid, conf) != 0 {
				return fail(opReplaceCntlr, "failover candidate exists")
			}
		}
		// Reading every surviving cntlr once answers both the "is the CN
		// already hosting one of this SP's cntlrs" rule (§6.4) and the
		// "exactly one primary" invariant.
		for _, cntlrId := range conf.GetCntlrIdList() {
			cntlr := &pb.Cntlr{}
			if !s.Get(CntlrKey(cid, spId, cntlrId), cntlr) {
				return fail(opReplaceCntlr, "cntlr not found")
			}
			if cntlr.GetAddrPort() == newCn.AddrPort {
				return fail(opReplaceCntlr, "cn already hosts a cntlr")
			}
			if cntlrId != oldId && asPrimary && cntlr.GetPrimary() {
				return fail(opReplaceCntlr, "sp already has a primary")
			}
		}
		footprint, err := spFootprint(s, opReplaceCntlr, cid, conf)
		if err != nil {
			return err
		}
		cn := &pb.CnConf{}
		if !s.Get(CnConfKey(cid, newCn.AddrPort), cn) {
			return fail(opReplaceCntlr, "cn not found")
		}
		if !CnAllocatable(cn) {
			return fail(opReplaceCntlr, "cn not allocatable")
		}
		if cn.GetFreeExtCnt() < footprint {
			return fail(opReplaceCntlr, "cn free_ext_cnt too low")
		}
		capKey := CnCapacityKey(cid, newCn.FreeExt, newCn.AddrPort)
		if !s.Get(capKey, &pb.CnCapacity{}) {
			return fail(opReplaceCntlr, ReasonCandidateChanged)
		}
		// --- effects ---
		s.Del(oldKey)
		err = releaseCn(
			s, opReplaceCntlr, cid, spId, oldId,
			old.GetAddrPort(), footprint,
		)
		if err != nil {
			return err
		}
		newId = SpNextId(conf)
		conf.NextId = newId + 1
		s.Put(CntlrKey(cid, spId, newId), &pb.Cntlr{
			AddrPort:   newCn.AddrPort,
			NvmeTrConf: cn.GetNvmeTrConf(),
			CntlidSlot: old.GetCntlidSlot(),
			Primary:    asPrimary,
			Disabled:   false,
			ErrEpoch:   0,
		})
		newCnConf := proto.Clone(cn).(*pb.CnConf)
		newCnConf.CntlrPtrList = append(
			newCnConf.CntlrPtrList,
			&pb.CntlrPointer{SpId: spId, CntlrId: newId},
		)
		newCnConf.FreeExtCnt -= footprint
		s.Put(CnConfKey(cid, newCn.AddrPort), newCnConf)
		MaintainCnCapacity(s, cid, newCn.AddrPort, cn, newCnConf)
		if err := BumpCnRev(s, opReplaceCntlr, cid, newCnConf); err != nil {
			return err
		}
		conf.CntlrIdList = append(
			removeId(conf.GetCntlrIdList(), oldId), newId,
		)
		err = rewriteCdcEntries(
			s, opReplaceCntlr, cid, shard, conf,
			old.GetNvmeTrConf(), cn.GetNvmeTrConf(),
		)
		if err != nil {
			return err
		}
		s.Put(SpConfKey(cid, spName), conf)
		return BumpSpRev(s, opReplaceCntlr, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return newId, nil
}

// releaseCn returns one cntlr's reservation to its CN: the pointer comes out
// of cntlr_ptr_list, the footprint goes back to free_ext_cnt, the capacity key
// follows §5.6 and CnRev is bumped once (§8.6 DeleteCntlr).
//
// A CN record that does not exist any more is not an error: the cntlr key is
// deleted either way, and there is nothing left to give the extents back to.
func releaseCn(
	s etcdutil.STM,
	op string,
	cid uint64,
	spId uint64,
	cntlrId uint64,
	addrPort string,
	footprint uint64,
) error {
	key := CnConfKey(cid, addrPort)
	cn := &pb.CnConf{}
	if !s.Get(key, cn) {
		return nil
	}
	newCn := proto.Clone(cn).(*pb.CnConf)
	newCn.CntlrPtrList = removeCntlrPtr(newCn.GetCntlrPtrList(), spId, cntlrId)
	newCn.FreeExtCnt += footprint
	s.Put(key, newCn)
	MaintainCnCapacity(s, cid, addrPort, cn, newCn)
	return BumpCnRev(s, op, cid, newCn)
}

// removeCntlrPtr drops one (sp_id, cntlr_id) pointer from a CN's list,
// preserving the order of the rest.
func removeCntlrPtr(
	list []*pb.CntlrPointer,
	spId uint64,
	cntlrId uint64,
) []*pb.CntlrPointer {
	kept := make([]*pb.CntlrPointer, 0, len(list))
	for _, ptr := range list {
		if ptr.GetSpId() == spId && ptr.GetCntlrId() == cntlrId {
			continue
		}
		kept = append(kept, ptr)
	}
	return kept
}

// removeId drops one id from a list, preserving the order of the rest.
func removeId(ids []uint64, id uint64) []uint64 {
	kept := make([]uint64, 0, len(ids))
	for _, item := range ids {
		if item == id {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// rewriteCdcEntries swaps one cntlr's transport address for another in every
// discovery entry of the SP (§8.6): one CdcEntry per Subsystem of nqn_list,
// whose key needs the subsystem's ss_id — so each Subsystem is read first.
//
// A listed Subsystem that is missing aborts the op: its CdcEntry key cannot be
// formed, so the entry would keep advertising a controller that no longer
// exists, and an op never writes half of what it owes. A Subsystem whose
// CdcEntry has not been written yet is skipped: there is nothing to rewrite,
// and inventing one here would guess at fields only CreateSubsystem knows.
func rewriteCdcEntries(
	s etcdutil.STM,
	op string,
	cid uint64,
	shard uint32,
	conf *pb.SpConf,
	oldTr *pb.NvmeTrConf,
	newTr *pb.NvmeTrConf,
) error {
	for _, nqn := range conf.GetNqnList() {
		subsystem := &pb.Subsystem{}
		if !s.Get(SubsystemKey(cid, conf.GetSpId(), nqn), subsystem) {
			return fail(op, "subsystem not found")
		}
		key := CdcEntryKey(cid, shard, conf.GetSpId(), subsystem.GetSsId())
		entry := &pb.CdcEntry{}
		if !s.Get(key, entry) {
			continue
		}
		entry.NvmeTrConfList = appendTrConf(
			removeTrConf(entry.GetNvmeTrConfList(), oldTr),
			newTr,
		)
		s.Put(key, entry)
	}
	return nil
}

// removeTrConf drops every entry equal to target from a transport list.
func removeTrConf(
	list []*pb.NvmeTrConf,
	target *pb.NvmeTrConf,
) []*pb.NvmeTrConf {
	kept := make([]*pb.NvmeTrConf, 0, len(list))
	for _, item := range list {
		if proto.Equal(item, target) {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// appendTrConf adds one transport to a list unless an equal one is already
// there, which keeps the rewrite idempotent.
func appendTrConf(
	list []*pb.NvmeTrConf,
	target *pb.NvmeTrConf,
) []*pb.NvmeTrConf {
	if target == nil {
		return list
	}
	for _, item := range list {
		if proto.Equal(item, target) {
			return list
		}
	}
	return append(list, target)
}

// ---------------------------------------------------------------------------
// Spare legs (MD6, AR8, §8.12)
// ---------------------------------------------------------------------------

// CreateSpareLeg appends one spare leg to a group and returns its leg_id (MD6,
// §8.12, AR8 step 3). The spare is pre-connected standby capacity: every cntlr
// connects to and probes its side, but md never sees it until SwitchSpareLeg
// puts it in the active list.
//
// The group is addressed by grp_id across BOTH the meta and the data list of
// the slice, and it must be a RedundMdRaid1 group — a RedundNone group has no
// redundancy to repair (AR8). The DN must not already carry a leg or a spare
// of this group, or the spare would share the failure domain it is meant to
// replace. No §3.6 geometry is computed: the spare joins an existing Group and
// inherits its ext_cnt, meta_blocks and data_blocks unchanged.
func CreateSpareLeg(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	sliceId uint64,
	grpId uint64,
	dn Cand,
	cc *pb.ClusterConf,
) (uint64, error) {
	legId := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		legId = 0
		if err := checkSpRev(
			s, opCreateSpareLeg, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfForOp(s, opCreateSpareLeg, cid, spName, spId)
		if err != nil {
			return err
		}
		if !isMdRaid1(conf) {
			return fail(opCreateSpareLeg, "group is not md-raid1")
		}
		sliceKey := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(sliceKey, slice) {
			return fail(opCreateSpareLeg, "slice not found")
		}
		grp := findGroup(slice, grpId)
		if grp == nil {
			return fail(opCreateSpareLeg, "group not found")
		}
		if len(grp.GetSpareLegList()) >= common.MaxSpareLegPerGrp {
			return fail(opCreateSpareLeg, "spare list full")
		}
		if grpHostsAddr(grp, dn.AddrPort) {
			return fail(opCreateSpareLeg, "dn already in the group")
		}
		slot, err := firstCntlidSlot(conf, opCreateSpareLeg)
		if err != nil {
			return err
		}
		extCnt := grp.GetExtCnt()
		if extCnt == 0 {
			return fail(opCreateSpareLeg, "group is empty")
		}
		dnConf, err := checkDnPick(s, opCreateSpareLeg, cid, cc, dn, extCnt)
		if err != nil {
			return err
		}
		nextId := SpNextId(conf)
		legId = nextId
		nextId++
		sideId := nextId
		nextId++
		grp.SpareLegList = append(grp.SpareLegList, &pb.Leg{
			LegId:  legId,
			LegIdx: nextLegIdx(grp),
			SideList: []*pb.Side{{
				SideId:      sideId,
				AddrPort:    dn.AddrPort,
				CntlidSlot:  slot,
				NvmeTrConf:  dnConf.GetNvmeTrConf(),
				ErrEpoch:    0,
				Provisioned: false,
			}},
		})
		ptr := &pb.SidePointer{SpId: spId, LegId: legId, SideId: sideId}
		err = chargeDn(
			s, opCreateSpareLeg, cid, cc, dn.AddrPort, dnConf, ptr, extCnt,
		)
		if err != nil {
			return err
		}
		conf.NextId = nextId
		s.Put(sliceKey, slice)
		s.Put(SpConfKey(cid, spName), conf)
		return BumpSpRev(s, opCreateSpareLeg, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return legId, nil
}

// grpHostsAddr reports whether the group already has a leg or a spare on that
// DN.
func grpHostsAddr(grp *pb.Group, addrPort string) bool {
	legLists := [][]*pb.Leg{grp.GetLegList(), grp.GetSpareLegList()}
	for _, legList := range legLists {
		for _, leg := range legList {
			for _, side := range leg.GetSideList() {
				if side.GetAddrPort() == addrPort {
					return true
				}
			}
		}
	}
	return false
}

// nextLegIdx is 1 + the largest leg_idx over BOTH lists of the group (§8.12:
// "the next unused idx in the group"), so an active leg and a spare never
// share one — the idx names the md member slot.
func nextLegIdx(grp *pb.Group) uint32 {
	legLists := [][]*pb.Leg{grp.GetLegList(), grp.GetSpareLegList()}
	maxIdx := uint32(0)
	found := false
	for _, legList := range legLists {
		for _, leg := range legList {
			if !found || leg.GetLegIdx() > maxIdx {
				maxIdx = leg.GetLegIdx()
				found = true
			}
		}
	}
	if !found {
		return 0
	}
	return maxIdx + 1
}

// SwitchSpareLeg makes a ready spare active and parks the leg it replaces
// (MD6, §8.12, AR8 step 1). The spare takes the target's POSITION in leg_list
// — the md member slot the array is missing — and inherits nothing else: it
// keeps its own leg_id, leg_idx, side and err_epoch. The target is appended to
// spare_leg_list keeping its err_epoch, where it stays parked for the operator
// (§0 item 17): still connected and probed, never repaired again.
//
// The spare's single side must be provisioned (§9.4): switching to a side that
// has not finished zeroing would put an unwritten member into the array.
func SwitchSpareLeg(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	sliceId uint64,
	grpId uint64,
	spareLegId uint64,
	targetLegId uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		if err := checkSpRev(
			s, opSwitchSpareLeg, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		if _, err := loadSpConfForOp(
			s, opSwitchSpareLeg, cid, spName, spId,
		); err != nil {
			return err
		}
		sliceKey := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(sliceKey, slice) {
			return fail(opSwitchSpareLeg, "slice not found")
		}
		grp := findGroup(slice, grpId)
		if grp == nil {
			return fail(opSwitchSpareLeg, "group not found")
		}
		spareIdx := legIdxIn(grp.GetSpareLegList(), spareLegId)
		if spareIdx < 0 {
			return fail(opSwitchSpareLeg, "spare leg not found")
		}
		targetIdx := legIdxIn(grp.GetLegList(), targetLegId)
		if targetIdx < 0 {
			return fail(opSwitchSpareLeg, "target leg not found")
		}
		spare := grp.GetSpareLegList()[spareIdx]
		if len(spare.GetSideList()) != 1 {
			return fail(opSwitchSpareLeg, "spare leg has no single side")
		}
		if !spare.GetSideList()[0].GetProvisioned() {
			return fail(opSwitchSpareLeg, "spare side is not provisioned")
		}
		target := grp.GetLegList()[targetIdx]
		grp.LegList[targetIdx] = spare
		grp.SpareLegList = append(
			append(
				grp.SpareLegList[:spareIdx:spareIdx],
				grp.SpareLegList[spareIdx+1:]...,
			),
			target,
		)
		s.Put(sliceKey, slice)
		return BumpSpRev(s, opSwitchSpareLeg, shard, cid, spId)
	})
}

// legIdxIn is the position of legId in the list, or -1.
func legIdxIn(legList []*pb.Leg, legId uint64) int {
	for idx, leg := range legList {
		if leg.GetLegId() == legId {
			return idx
		}
	}
	return -1
}
