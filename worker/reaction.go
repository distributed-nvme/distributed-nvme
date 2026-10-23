// The automatic reactions of dnv-worker.md (AR1-AR10), run by the sp
// coordinator (RW14) as one PASS per SP per cntlr_interval (AR1), with the
// two steps of the pass that follow a repair, its stage moves (AR11) and the
// retire timer (AR12).
//
// A pass is stateless by construction, but for two records: it starts from a
// fresh model.LoadSp snapshot, the in-memory CntlrInfo the PRIMARY cntlr's
// child last reported and a copy of AR10's memo of this coordinator's own
// verdicts (verdictMemo), decides ONE action (AR2), and
// forgets everything again — everything but AR5's record of the last failover
// it applied (failoverMemo) and AR7's of the last replacement of a primary
// (replaceMemo), which say why the role moved or the primary was replaced,
// facts no etcd record keeps.
// Two owners overlapping on one SP therefore cannot apply an action twice — every
// action is a model op that re-validates its own preconditions inside its STM
// (MD6/MD7), and the loser gets model.ErrPrecondition back — save in one
// narrow case of a spare create (AR2). A group may hold several spares, so
// its full list alone does not keep two creates apart; the loser of a spare
// create is refused by whichever of model.CreateSpareLeg's checks, or its
// clone-keyed twin's, comes first: "spare list full" when the winner's spare
// filled the list; otherwise spare_unprovisioned while that spare is still
// unprovisioned, and once RW18 has flipped it, `candidate changed`, when the
// loser's plan
// — the group's DNs its scan black-listed — does not hold the DN of the
// winner's spare. It does hold it when the loser's snapshot holds a DN that
// is out of the group when the winner's create commits and the winner's
// spare lands on that DN: the DN leaves the group, through an operator's
// call, before the winner plans, or joins after and leaves the same way
// before the winner's create. The loser's create can then commit a second
// spare for one repair (dnv-worker.md, Known limits).
//
// Which skips end the pass and which move the scan on is AR2's list, and AR8
// takes the smallest leg_id first. One order the doc leaves to the code: AR7
// does not say which of several eligible cntlrs is replaced first, and
// replaceTarget takes the smallest cntlr_id, so a pass is deterministic and
// two overlapping owners choose the same target.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The records of a pass (dnv-worker.md, Log records). The strings are normative
// — the worker suite (dnv-worker.md, Integration test plan) greps
// them — so they are constants and never formatted.
const (
	msgReactionApplied = "reaction applied"
	msgReactionSkipped = "reaction skipped"
)

// The "kind" attribute of the two records (AR2; dnv-worker.md, Log records).
// Every reaction the worker can run is one of these six; nothing else is ever
// logged as a kind.
const (
	reactionFailover     = "failover"
	reactionGrowData     = "grow_data"
	reactionGrowMeta     = "grow_meta"
	reactionReplaceCntlr = "replace_cntlr"
	reactionSpareCreate  = "spare_create"
	reactionSpareSwitch  = "spare_switch"
)

// The "reason" attribute of `reaction skipped`. A model.ErrPrecondition
// contributes its own Reason instead of one of these (MD7).
const (
	// reasonNoCandidate is AR5's and the allocator's "nothing to pick".
	reasonNoCandidate = "no candidate"
	// reasonSharedState is AR5's first refusal, and AR7's first: the
	// primary's every ERROR row is HL2's shared-state class, which neither a
	// failover nor a replacement can escape.
	reasonSharedState = "shared_state"
	// reasonSameError is AR5's second refusal, and AR7's second: the primary
	// fails only on rows the candidate failed on when the last failover took
	// the role from it or, when it is the replacement the last replacement of
	// a primary created, only on rows the primary it replaced failed on. The
	// record's kind tells the two apart.
	reasonSameError = "same_error"
	// reasonGrowPending is AR6's stateless pending rule. It is model's
	// string: the same reason reaches this record from the pass's pre-check
	// and from a GrowSlice whose STM found the grow pending, whose record
	// also names the op (AR2).
	reasonGrowPending = model.ReasonGrowPending
	// reasonSparePending is AR8 step 2: a spare is on its way.
	reasonSparePending = "spare_pending"
	// reasonSpareListFull is AR8 step 4: only an operator frees a slot.
	reasonSpareListFull = "spare_list_full"
	// reasonSpareUnprovisioned is AR8 step 3's hold: a spare of the group
	// still has an unprovisioned side, which model.CreateSpareLeg and its
	// clone-keyed twin model.CreateCloneSpareLeg refuse. It is model's
	// string, for the reason reasonGrowPending is.
	reasonSpareUnprovisioned = model.ReasonSpareUnprovisioned
	// reasonRedundNone is AR8's "RedundNone groups only log" (AR9).
	reasonRedundNone = "redund_none"
	// reasonTwoSides is AR8's "a leg with two sides has a user migration in
	// flight and is left alone".
	reasonTwoSides = "leg_has_two_sides"
	// reasonNoHealthyLeg is AR8's healthy-leg precondition, on every group:
	// no other leg of the group's leg_list is free of a health epoch and
	// held in sync by the current primary's latest report, so leg repair
	// neither switches nor creates a spare for it.
	reasonNoHealthyLeg = "no_healthy_leg"
	// The grp_row_ reasons are AR8 step 1's gate, which the retire timer
	// reads as well (AR12): the group's row in the primary's latest report
	// (grp_id_to_md_raid) does not read RES_STATUS_OK, the one status that
	// says the group's array runs (cnagent.md CN28), and the switch, or the
	// timer's delete, waits. One
	// reason per status of the row, and grp_row_absent for a report that
	// holds no row of the group — a standby's report, which the pass reads
	// from a promotion's hand-over until its reply (primaryInfo), holds
	// none (cnagent.md CN12).
	reasonGrpRowError        = "grp_row_error"
	reasonGrpRowMissing      = "grp_row_missing"
	reasonGrpRowUnknown      = "grp_row_unknown"
	reasonGrpRowProvisioning = "grp_row_provisioning"
	reasonGrpRowPending      = "grp_row_pending"
	reasonGrpRowAbsent       = "grp_row_absent"
	// reasonGrpRowStale is the same gate on a row that reads OK in a report
	// from an observation older than the snapshot: the report's revision is
	// below the snapshot's SpRevision, so the row may be from before the
	// last change of the group's leg_list, and the switch, or the timer's
	// delete, waits for the primary's report of an observation made at
	// that revision.
	reasonGrpRowStale = "grp_row_stale"
	// reasonMetaLadderCap is the dm-thin metadata ceiling of architecture.md,
	// GrowSlice (model's metaSizeCap), reached before model.GrowSlice is even
	// called because the allocator has to search for the ladder's size first.
	reasonMetaLadderCap = "meta_ladder_cap"
	// reasonGrpListFull is the group ceiling of architecture.md, GrowSlice: the
	// slice's list of that kind already holds common.MaxGrpCntPerSlice groups,
	// which model.GrowSlice refuses. It is model's string, for the reason
	// reasonGrowPending is.
	reasonGrpListFull = model.ReasonGrpListFull
	// reasonNoDataGroup is a slice with no data group to size a data grow by.
	reasonNoDataGroup = "no_data_group"
	// reasonOpFailed is a transient failure — an etcd error, a scan that did
	// not complete — as opposed to a precondition that did not hold. The
	// record then carries an "error" attribute as well.
	reasonOpFailed = "op_failed"
	// reasonGrpHasRepair is AR8's skip of a group that has a repair: a spare
	// and the repair's new leg would race for one place in the group's array
	// (architecture.md, Repair). It is model's string, for the reason
	// reasonGrowPending is: the spare creates refuse such a group with it.
	reasonGrpHasRepair = model.ReasonGrpHasRepair
)

// The records of the two pass steps of a repair (dnv-worker.md, Repairs; Log
// records): a stage move of a repair, and the retire timer's delete of a
// retired leg. Neither is a reaction, so neither is logged as one.
const (
	msgRepairStageMoved   = "repair stage moved"
	msgRepairStageSkipped = "repair stage skipped"
	msgRetiredLegDeleted  = "retired leg deleted"
	msgRetiredLegKept     = "retired leg kept"
)

// The "reason" attribute of `repair stage skipped` and `retired leg kept`,
// beside a model.ErrPrecondition's own Reason with the op that refused, and
// reasonOpFailed for any other failure of the op.
const (
	// reasonCloneDeleting is a repair or a retired leg of a deleting
	// clone's group, which the clone drain's final transaction gives back
	// with the group (CLD9) and every repair op refuses (MD6).
	reasonCloneDeleting = "clone_deleting"
	// reasonAbortFlagClear is a verdict of abort confirmed on a repair whose
	// stored abort flag is clear, which SetRepairStage refuses (MD6).
	reasonAbortFlagClear = "abort_flag_clear"
	// reasonGrpNotFound is a retired leg whose group the pass's snapshot
	// does not hold in the parent the record names.
	reasonGrpNotFound = "grp_not_found"
	// reasonGrpNotWhole is the retire timer's hold on a group whose whole
	// flag the current primary's latest report does not set.
	reasonGrpNotWhole = "grp_not_whole"
	// reasonGrpLegUnhealthy is the retire timer's hold on a group a leg of
	// whose leg_list, or that leg's side, has an err_epoch.
	reasonGrpLegUnhealthy = "grp_leg_unhealthy"
)

// Non-normative records of the pass. The strings above (dnv-worker.md, Log
// records) are the ones the worker suite (dnv-worker.md, Integration test plan)
// greps; these exist so an operator can see why an SP is not being
// reacted on at all.
const (
	// msgReactionSuppressed is AR3, logged on the transition only: an SP at
	// sp_level >= SP_LEVEL_NO_THINPOOL would otherwise produce one record per
	// cntlr_interval forever. A `deleting` SP drains (SPD6, drain.go) and is
	// not suppressed here, so this record has no such case.
	msgReactionSuppressed = "reaction suppressed"
	// msgPoolStatusUnparsable is AR6's "an unparsable line is skipped and
	// logged once per change".
	msgPoolStatusUnparsable = "pool status unparsable"
)

// ---------------------------------------------------------------------------
// The model surface (MD5, MD6)
// ---------------------------------------------------------------------------

// reactionOps is everything a pass allocates and mutates through model. It is
// an interface for the same reason spOps is: the unit tests drive a real
// coordinator over a fixture SpState without etcd. Production is
// modelReactionOps, which does nothing but call model.
type reactionOps interface {
	// findDnCandidates is MD5 for a side allocation (AR6 step, AR8 step 3).
	// excludeLocs is the tier-1 exclusion of architecture.md, Per-operation
	// allocation: the failure domains AR8 keeps the spare out of, empty for the
	// AR6 grow. requiredCnt is how many DNs the caller must actually place —
	// that section's tier-2 trigger, and never the oversampled candCnt.
	findDnCandidates(
		ctx context.Context,
		cid uint64,
		cc *pb.ClusterConf,
		candExt uint64,
		candCnt int,
		requiredCnt int,
		black []string,
		excludeLocs []string,
	) ([]model.Cand, error)
	// findCnCandidates is MD5 for a cntlr allocation (AR7). excludeLocs is
	// the tier-1 exclusion of architecture.md, Per-operation allocation: the
	// failure domains of the SP's other cntlrs, which model drops again when
	// tier 1 finds no CN.
	findCnCandidates(
		ctx context.Context,
		cid uint64,
		candExt uint64,
		candCnt int,
		black []string,
		spCnAddrs []string,
		excludeLocs []string,
	) ([]model.Cand, error)
	// failover is AR5.
	failover(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		oldId uint64,
		newId uint64,
		now uint64,
	) error
	// growSlice is AR6; it returns the new group's grp_id. poolTotal is the
	// total the primary reported for the pool of this kind, which the op
	// re-runs the AR6 pending rule against inside its STM (AR2).
	growSlice(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		isMeta bool,
		poolTotal uint64,
		cc *pb.ClusterConf,
		legs []model.Cand,
	) (uint64, error)
	// replaceCntlr is AR7; it returns the new cntlr_id. spCnAddrs is the
	// exclusion the scan that found newCn was given, which the op holds the
	// SP's surviving cntlrs against.
	replaceCntlr(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		oldId uint64,
		newCn model.Cand,
		spCnAddrs []string,
		asPrimary bool,
		now uint64,
	) (uint64, error)
	// createSpareLeg is AR8 step 3; it returns the new leg_id. grpDnAddrs is
	// the plan the scan that found dn ran against — the group's DNs, its
	// black list — which the op holds the group's current DNs against inside
	// its STM: a DN the group gained since answers `candidate changed`, and
	// the next pass plans again.
	createSpareLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		grpId uint64,
		dn model.Cand,
		grpDnAddrs []string,
		cc *pb.ClusterConf,
	) (uint64, error)
	// switchSpareLeg is AR8 step 1.
	switchSpareLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		grpId uint64,
		spareLegId uint64,
		targetLegId uint64,
	) error
	// createCloneSpareLeg and switchCloneSpareLeg are the two above on a
	// clone's clone meta group, the clone named by its name and proved by
	// its clone_id (MD6's clone-keyed twins).
	createCloneSpareLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		cloneName string,
		cloneId uint64,
		grpId uint64,
		dn model.Cand,
		grpDnAddrs []string,
		cc *pb.ClusterConf,
	) (uint64, error)
	switchCloneSpareLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		cloneName string,
		cloneId uint64,
		grpId uint64,
		spareLegId uint64,
		targetLegId uint64,
	) error
	// setRepairStage is the stage move of a repair (architecture.md, Repair;
	// MD6's SetRepairStage): the CREATED repair repName that still carries
	// repId, the rep_id the primary's verdict named, moves to stage, STARTED
	// or ABORTED.
	setRepairStage(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		repName string,
		repId uint64,
		stage pb.RepStage,
	) error
	// deleteRetiredLeg is the retire timer's delete (MD6's DeleteRetiredLeg)
	// of the retired leg legId, which the op refuses unless the pool's
	// SpRev still holds expectRev, the revision the pass's snapshot read.
	deleteRetiredLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		expectRev uint64,
		legId uint64,
		cc *pb.ClusterConf,
	) error
	// drainSpCntlrs is the sp drain's D1 (SPD9): every cntlr of a latched SP
	// in one STM. It returns how many it removed.
	drainSpCntlrs(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
	) (int, error)
	// drainSpRepairs is one batch of the sp drain's repair phase (SPD15):
	// the repairs repNames, of one parent, which the caller cut from its
	// snapshot (model.SpDrainRepairBatch). It returns how many it deleted.
	drainSpRepairs(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		repNames []string,
		cc *pb.ClusterConf,
	) (int, error)
	// drainSpRetiredLegs is one batch of the retired-leg phase (SPD15), cut
	// by model.SpDrainRetiredLegBatch. It returns how many it deleted.
	drainSpRetiredLegs(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		legIds []uint64,
		cc *pb.ClusterConf,
	) (int, error)
	// drainSpSlice is one D2 batch (SPD10, SPD11): up to MaxDelGrpPerTxn
	// groups of ONE slice. It returns how many groups it removed and whether
	// the slice is gone.
	drainSpSlice(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		cc *pb.ClusterConf,
	) (int, bool, error)
	// finishSpDelete is D3 (SPD12): the SP's last keys and the shard bucket.
	finishSpDelete(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
	) error
	// drainCloneBm is one clone-drain batch (CLD8): at most
	// MaxDelBmPerTxn chunk keys, named by the caller's snapshot scan, and no
	// SpRev read or bump. It returns the SIZE of that batch, not a count of
	// keys that were still there — a Del is an idempotent pop and the op
	// never reads them.
	drainCloneBm(
		ctx context.Context,
		cid uint64,
		spId uint64,
		spName string,
		cloneName string,
		cloneId uint64,
		chunks []model.BmChunk,
	) (int, error)
	// finishCloneDelete is the clone drain's final STM (CLD9): the Clone key
	// and its clone_name_list entry, together, and the sides of its clone
	// meta group given back to their DNs, under the cluster's stored conf cc,
	// which the STM refuses when it is unusable (CLD7).
	finishCloneDelete(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		cloneName string,
		cloneId uint64,
		cc *pb.ClusterConf,
	) error
}

// modelReactionOps is the production reactionOps.
type modelReactionOps struct {
	cli *etcdutil.Client
}

func (o *modelReactionOps) findDnCandidates(
	ctx context.Context,
	cid uint64,
	cc *pb.ClusterConf,
	candExt uint64,
	candCnt int,
	requiredCnt int,
	black []string,
	excludeLocs []string,
) ([]model.Cand, error) {
	// The tier bool is dropped: a tier-2 placement is visible in the stored
	// topology and the records of dnv-worker.md, Log records, gain none for it.
	// An empty excludeLocs — the AR6 grow — makes this the plain scan. It
	// asks for no metadata units: a grown group's sides and a spare's side
	// hold none of a DN's metadata area, which only a migration's destination
	// uses (architecture.md, Metadata units).
	cands, _, err := model.FindDnCandidatesAntiAffine(
		ctx, o.cli, cid, cc, candExt, 0, candCnt, requiredCnt,
		black, nil, excludeLocs,
	)
	return cands, err
}

func (o *modelReactionOps) findCnCandidates(
	ctx context.Context,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	spCnAddrs []string,
	excludeLocs []string,
) ([]model.Cand, error) {
	// The tier bool is dropped, as in findDnCandidates.
	cands, _, err := model.FindCnCandidatesAntiAffine(
		ctx, o.cli, cid, candExt, candCnt,
		black, nil, spCnAddrs, excludeLocs,
	)
	return cands, err
}

func (o *modelReactionOps) failover(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newId uint64,
	now uint64,
) error {
	return model.Failover(
		ctx, o.cli, cid, shard, spId, spName, oldId, newId, now,
	)
}

func (o *modelReactionOps) growSlice(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	isMeta bool,
	poolTotal uint64,
	cc *pb.ClusterConf,
	legs []model.Cand,
) (uint64, error) {
	// expectRev 0: the worker holds no client token and converges on what
	// etcd holds (gateway.md GW6, MD6). The gateway passes a real one only
	// when its caller sent one — GW6 is presence-based — and 0 otherwise.
	return model.GrowSlice(
		ctx, o.cli, cid, shard, spId, spName, 0, sliceId, isMeta,
		poolTotal, cc, legs,
	)
}

func (o *modelReactionOps) replaceCntlr(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newCn model.Cand,
	spCnAddrs []string,
	asPrimary bool,
	now uint64,
) (uint64, error) {
	return model.ReplaceCntlr(
		ctx, o.cli, cid, shard, spId, spName, oldId, newCn, spCnAddrs,
		asPrimary, now,
	)
}

func (o *modelReactionOps) createSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	grpId uint64,
	dn model.Cand,
	grpDnAddrs []string,
	cc *pb.ClusterConf,
) (uint64, error) {
	return model.CreateSpareLeg(
		ctx, o.cli, cid, shard, spId, spName, 0, sliceId, grpId, dn,
		grpDnAddrs, cc,
	)
}

func (o *modelReactionOps) switchSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	grpId uint64,
	spareLegId uint64,
	targetLegId uint64,
) error {
	return model.SwitchSpareLeg(
		ctx, o.cli, cid, shard, spId, spName, 0, sliceId, grpId,
		spareLegId, targetLegId,
	)
}

func (o *modelReactionOps) createCloneSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	grpId uint64,
	dn model.Cand,
	grpDnAddrs []string,
	cc *pb.ClusterConf,
) (uint64, error) {
	return model.CreateCloneSpareLeg(
		ctx, o.cli, cid, shard, spId, spName, 0, cloneName, cloneId, grpId,
		dn, grpDnAddrs, cc,
	)
}

func (o *modelReactionOps) switchCloneSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	grpId uint64,
	spareLegId uint64,
	targetLegId uint64,
) error {
	return model.SwitchCloneSpareLeg(
		ctx, o.cli, cid, shard, spId, spName, 0, cloneName, cloneId, grpId,
		spareLegId, targetLegId,
	)
}

func (o *modelReactionOps) setRepairStage(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
	repId uint64,
	stage pb.RepStage,
) error {
	return model.SetRepairStage(
		ctx, o.cli, cid, shard, spId, spName, repName, repId, stage,
	)
}

func (o *modelReactionOps) deleteRetiredLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	legId uint64,
	cc *pb.ClusterConf,
) error {
	return model.DeleteRetiredLeg(
		ctx, o.cli, cid, shard, spId, spName, expectRev, legId, cc,
	)
}

func (o *modelReactionOps) drainSpCntlrs(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) (int, error) {
	return model.DrainSpCntlrs(ctx, o.cli, cid, shard, spId, spName)
}

func (o *modelReactionOps) drainSpRepairs(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repNames []string,
	cc *pb.ClusterConf,
) (int, error) {
	return model.DrainSpRepairs(
		ctx, o.cli, cid, shard, spId, spName, repNames, cc,
	)
}

func (o *modelReactionOps) drainSpRetiredLegs(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	legIds []uint64,
	cc *pb.ClusterConf,
) (int, error) {
	return model.DrainSpRetiredLegs(
		ctx, o.cli, cid, shard, spId, spName, legIds, cc,
	)
}

func (o *modelReactionOps) drainSpSlice(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	cc *pb.ClusterConf,
) (int, bool, error) {
	return model.DrainSpSlice(
		ctx, o.cli, cid, shard, spId, spName, sliceId, cc,
	)
}

func (o *modelReactionOps) finishSpDelete(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) error {
	return model.FinishSpDelete(ctx, o.cli, cid, shard, spId, spName)
}

func (o *modelReactionOps) drainCloneBm(
	ctx context.Context,
	cid uint64,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	chunks []model.BmChunk,
) (int, error) {
	return model.DrainCloneBm(
		ctx, o.cli, cid, spId, spName, cloneName, cloneId, chunks,
	)
}

func (o *modelReactionOps) finishCloneDelete(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	cc *pb.ClusterConf,
) error {
	return model.FinishCloneDelete(
		ctx, o.cli, cid, shard, spId, spName, cloneName, cloneId, cc,
	)
}

// ---------------------------------------------------------------------------
// The reactor: the little state a stateless pass still keeps
// ---------------------------------------------------------------------------

// reactor is the sp coordinator's reaction half (AR1-AR12): the model surface,
// plus the two memos that exist only to keep the log honest and the two records
// AR5 and AR7 decide by. Apart from AR10's memo of the coordinator's own
// verdicts (verdictMemo), which gates every threshold reaction, no other
// decision is taken from a memo — AR6's pending rule and AR8's spare rules are
// reconstructed from etcd and the status line on every pass (AR6).
type reactor struct {
	ops reactionOps
	// badLine is the last unparsable pool status line per slice_id, so a
	// steady bad line is logged once per CHANGE and not once per pass (AR6).
	badLine map[uint64]string
	// suppressed is the last AR3 verdict, so msgReactionSuppressed is emitted
	// on the transition only.
	suppressed bool
	// lastFailover is AR5's record of the last failover this coordinator
	// applied (errorFollowedRole); nil before the first.
	lastFailover *failoverMemo
	// lastReplace is AR7's record of the last replacement of a primary this
	// coordinator applied (errorFollowedReplacement); nil before the first.
	lastReplace *replaceMemo
}

// newReactor builds a reactor over one model surface.
func newReactor(ops reactionOps) *reactor {
	return &reactor{ops: ops, badLine: make(map[uint64]string)}
}

// reactor returns the coordinator's reaction state, building the production one
// from its deps on first use. A coordinator assembled by hand — the plan-only
// test fixture — therefore still runs a well-formed pass.
func (w *spWorker) reactor() *reactor {
	if w.react == nil {
		w.react = newReactor(&modelReactionOps{cli: w.deps.cli})
	}
	return w.react
}

// ---------------------------------------------------------------------------
// The own-verdict memo (AR10)
// ---------------------------------------------------------------------------

// failoverRun is the run of unhealthy health verdicts in a row, from the
// primary's Check* rounds alone, that AR5's threshold trigger needs besides
// its threshold (AR10): one round missed or slow, followed by a clean answer
// of either kind, fails no primary over, while a primary that answers no
// round, or answers every round from an observation too old (HL7), gives its
// second such verdict one round later. A round that answers from an
// observation its child's loop has already seen adds nothing (RW23), nor
// does one that answers from an observation that ended before the loop's
// first round after a pause (HL7).
const failoverRun = 2

// verdictMemo is AR10's memo: the objects of the SP — cntlrs, legs and sides
// — whose latest health verdict by this coordinator's own monitors is
// unhealthy, and each cntlr's run of unhealthy verdicts from its rounds. A
// threshold reaction fires only for an object it holds, so an err_epoch an
// earlier owner left, or another observer wrote, fires nothing before this
// coordinator has judged the object itself, while the clock still runs from
// the stored epoch (AR4). The cntlr and side monitors note their
// verdicts from the children's goroutines, while the leg monitors and the pass
// work on the coordinator's, so it is locked. It lives with the coordinator,
// from its start to its stop, which is the coordinator's tenure of the SP: a
// restart or a handoff begins with an empty memo. A nil memo holds nothing.
type verdictMemo struct {
	mu     sync.Mutex
	cntlrs map[uint64]bool
	// runs counts, for each cntlr, the unhealthy verdicts its Check* rounds
	// gave in a row, each judged by the round's own report (noteRound) — an
	// ERROR row in an observation new to the child's loop (RW23), or no
	// reply in time, or one whose observation is too old (HL7) — since its
	// last clean answer of either kind. A Syncup* reply with an ERROR row
	// neither starts a run nor adds to one: a converge still waiting for a
	// member reports its groups ERROR, and the next primary would wait too.
	// The run outlives the child, and no look of a child's counts again by
	// its rows under the child restarted after it (RW14, RW23): the new
	// loop's first round starts the age's count, and every observation the
	// old child counted or saw ended before that round, so it gives no
	// verdict until it is too old, when a round on it counts as a missed
	// round does, by its age (HL7).
	runs  map[uint64]int
	legs  map[uint64]bool
	sides map[sideKey]bool
}

// newVerdictMemo builds an empty memo.
func newVerdictMemo() *verdictMemo {
	return &verdictMemo{
		cntlrs: make(map[uint64]bool),
		runs:   make(map[uint64]int),
		legs:   make(map[uint64]bool),
		sides:  make(map[sideKey]bool),
	}
}

// noteCntlr records one verdict on a cntlr, whichever answer gave it:
// unhealthy adds it, clean drops it and ends its run.
func (m *verdictMemo) noteCntlr(cntlrId uint64, unhealthy bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	noteVerdict(m.cntlrs, cntlrId, unhealthy)
	if !unhealthy {
		delete(m.runs, cntlrId)
	}
}

// noteRound records what one Check* round of a cntlr reported, judged by that
// round's own report (cntlrDriver.roundInfo): unhealthy — an ERROR row, or no
// reply in time, or an observation too old (HL7) — adds one to the cntlr's
// run, clean ends it. A round that answers from an observation its child's
// loop has seen already is not noted at all (RW23), nor is one that gives
// no verdict (HL7).
func (m *verdictMemo) noteRound(cntlrId uint64, unhealthy bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if unhealthy {
		m.runs[cntlrId]++
		return
	}
	delete(m.runs, cntlrId)
}

// noteLeg records one verdict on a leg.
func (m *verdictMemo) noteLeg(legId uint64, unhealthy bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	noteVerdict(m.legs, legId, unhealthy)
}

// noteSide records one verdict on a side.
func (m *verdictMemo) noteSide(key sideKey, unhealthy bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	noteVerdict(m.sides, key, unhealthy)
}

// noteVerdict is the one update every kind shares.
func noteVerdict[K comparable](set map[K]bool, key K, unhealthy bool) {
	if unhealthy {
		set[key] = true
		return
	}
	delete(set, key)
}

// judgedSet is one pass's copy of the memo (AR10): the cntlrs, legs and
// sides this coordinator last judged unhealthy, and each cntlr's run.
type judgedSet struct {
	cntlrs map[uint64]bool
	runs   map[uint64]int
	legs   map[uint64]bool
	sides  map[sideKey]bool
}

// snapshot copies the memo for one pass, after it has dropped every object
// the pass's snapshot no longer lists — the legs and sides of its slices and
// of its clones' clone meta groups, the new legs of its repairs with their
// sides and its retired sides are the ones it lists (judgedObjs): ids are
// never reused within an SP, so such an entry could only grow the memo.
func (m *verdictMemo) snapshot(state *model.SpState) judgedSet {
	out := judgedSet{
		cntlrs: make(map[uint64]bool),
		runs:   make(map[uint64]int),
		legs:   make(map[uint64]bool),
		sides:  make(map[sideKey]bool),
	}
	if m == nil {
		return out
	}
	legs, sides := judgedObjs(state)
	m.mu.Lock()
	defer m.mu.Unlock()
	for cntlrId := range m.cntlrs {
		if _, ok := state.Cntlrs[cntlrId]; !ok {
			delete(m.cntlrs, cntlrId)
			continue
		}
		out.cntlrs[cntlrId] = true
	}
	for cntlrId, run := range m.runs {
		if _, ok := state.Cntlrs[cntlrId]; !ok {
			delete(m.runs, cntlrId)
			continue
		}
		out.runs[cntlrId] = run
	}
	for legId := range m.legs {
		if _, ok := legs[legId]; !ok {
			delete(m.legs, legId)
			continue
		}
		out.legs[legId] = true
	}
	for key := range m.sides {
		if _, ok := sides[key]; !ok {
			delete(m.sides, key)
			continue
		}
		out.sides[key] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// The pass (AR1, AR2)
// ---------------------------------------------------------------------------

// spPass is one evaluation of the reactions for one SP (AR1): the fresh
// snapshot, the cluster conf, the resolved thresholds, the primary cntlr with
// the latest CntlrInfo its child reported, the copy of AR10's memo, and now in
// unix seconds.
type spPass struct {
	state *model.SpState
	cc    *pb.ClusterConf
	th    *pb.EventThreshold
	now   uint64

	// primaryId and primary are the SP's primary cntlr as the SNAPSHOT sees
	// it; primary is nil for an SP that momentarily has none.
	primaryId uint64
	primary   *pb.Cntlr
	// info is the primary's latest CntlrInfo (pool usage for AR6, spare
	// readiness and the group rows for AR8, HL2's row classes for AR5 and
	// AR7); nil when its child has never reported one. infoRev is the
	// revision of the observation it reports, which AR8 step 1's gate and
	// the retire timer compare with the snapshot's SpRevision (grpRowHold).
	info    *pb.CntlrInfo
	infoRev uint64
	// failoverCand is AR5's election, computed once because AR7's
	// sole-primary variant is defined as "AR5 found none". 0 = none.
	failoverCand uint64
	// judged is AR10's memo as the pass began: the objects this coordinator
	// last judged unhealthy, which alone a threshold may fire on, and each
	// cntlr's run of unhealthy verdicts from its rounds.
	judged judgedSet
}

// reactionPass is RW20's hook: one automatic-reaction pass for this SP, on the
// coordinator's own cntlr_interval ticker (AR1, AR2).
//
// The snapshot is taken fresh here rather than reused from the fan-out: a pass
// must see the err_epochs this same worker wrote since (HL3), and model.LoadSp
// is one Snapshot (EU4) even though it loads more than a pass reads.
//
// A drain step's wake-up runs the drain half of this pass alone, in the same
// order and behind the same gates (drainTick): the load, the clone drain, and
// for a latched SP the cluster-conf gate and the sp drain's step. The load
// and the gate are the helpers both call (loadPassState, passClusterConf).
func (w *spWorker) reactionPass(ctx context.Context) {
	seqs := w.healthSeqs()
	state, ok := w.loadPassState(ctx)
	if !ok {
		return
	}
	// HL3: the health memos are a cache of the records this load read. A
	// health write bumps no revision (architecture.md, Revision keys and the
	// sync fan-out), so no fan-out ever reloads for one; this pass, every
	// cntlr_interval, is the load that sees an epoch another observer wrote or
	// cleared, whatever the gates below decide.
	// The children's write counts were read before the load (healthSeqs).
	w.reseedHealth(state, seqs)
	// CLD7: the clone drain runs ALONGSIDE the reactions rather than instead of
	// them — the SP is healthy and its other children must keep converging —
	// and ahead of EVERY gate below, because none of them applies to it. It
	// reads no SP geometry and no sp_level, and its batches no cluster conf;
	// its final step reads the cluster conf from the cache itself and fails
	// alone, logged, when there is no usable one (drainCloneStep). A gate
	// that could stop every step would strand a latched clone permanently,
	// the latch being one-way.
	//
	// (The design words the order as "the normal fan-out first, then one batch
	// per deleting clone". The fan-out is a different function here, driven by
	// the watch rather than by this pass; what the wording is about is that
	// the living children keep converging, which they do, and what matters for
	// the drain is that nothing can skip it.)
	w.drainClones(ctx, state)
	cc, ok := w.passClusterConf(ctx)
	if !ok {
		return
	}
	p := w.newPass(state, cc)
	if state.Conf.GetDeleting() {
		// SPD6: `deleting` is no AR3 suppression. A LATCHED SP runs exactly
		// one drain step and nothing else — regardless of sp_level
		// suppression, because a doomed SP must drain at any level. drain.go
		// owns the rest.
		//
		// It also runs ahead of the SP's OWN two gates below, its bdev_conf
		// and its groups' lengths to zero, deliberately: the drain reads no
		// geometry at all — no block size, no stripe, no chunk count, no
		// group's block counts — and an SP whose stored geometry cannot be
		// read is exactly the one an operator most wants to be able to
		// delete. Gating the drain on it would make such an SP permanently
		// undeletable, since the latch is one-way (SPD5) and nothing else
		// ever removes the keys.
		w.confRefusal.cc = ""
		w.drainStep(ctx, state, cc)
		return
	}
	// The SP's own stored geometry, for the reactions: tryGrow reads
	// low_water_mark_pct and data_block_size straight off it.
	if err := model.ValidateBdevConf(state.Conf.GetBdevConf()); err != nil {
		w.refuseReactionConf(ctx, err)
		return
	}
	// RW14's group gate as well: while a group's stored counts give its sides
	// no length to zero (RW15), the fan-out builds no request, so the pass
	// must not commit a reaction the fan-out cannot deliver (AR1). A
	// failover would rewrite the SP's discovery records (architecture.md
	// [D18]) while neither cntlr is sent its new role, a replacement would
	// release the old cntlr from its CN while the new one is never built,
	// and a grow or a spare would take DN capacity for sides no request
	// reaches. The refusal comes before the memo is cleared, so a steady bad
	// group costs one record.
	if err := checkSideZeroBytes(state); err != nil {
		w.refuseReactionConf(ctx, err)
		return
	}
	w.confRefusal.cc = ""
	// dnv-worker.md AR11: the stage moves are a step of the pass, not
	// reactions. They run after the gates and before AR3's suppression, so
	// at every sp_level, since a move only follows the primary's report and
	// an operator may need to finish a repair at a level where the operator
	// is in charge (AR3); none of them is the pass's one action (AR2).
	w.moveRepairStages(ctx, p)
	if w.reactionSuppressed(ctx, p) {
		return
	}
	// The retire timer runs only where reactions run (AR3), and is not the
	// pass's one action either.
	w.retireOldLeg(ctx, p)
	// AR2: the first applicable reaction runs and the pass ends.
	switch {
	case w.tryFailover(ctx, p):
	case w.tryGrow(ctx, p):
	case w.tryReplaceCntlr(ctx, p):
	default:
		w.tryLegRepair(ctx, p)
	}
}

// loadPassState is the load of a pass (AR1) and of a drain's wake-up
// (drainTick): one fresh snapshot of the SP, or false when there is nothing
// to act on. An SP that is gone (model.ErrNotFound) is silent here: the
// fan-out records it (RW14), and the SpRev delete that follows stops this
// coordinator (SW3). Any other failure is recorded as RW14 records a
// fan-out's, and the pass simply runs again on the next tick (RW12).
func (w *spWorker) loadPassState(ctx context.Context) (*model.SpState, bool) {
	state, err := w.ops.loadSp(ctx, w.cid, w.desired.handle)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			slog.InfoContext(ctx, msgSpLoadFailed,
				slog.Uint64("cluster_id", w.cid),
				slog.Uint64("sp_id", w.spId),
				slog.String("sp_name", w.desired.handle),
				slog.String("error", err.Error()),
			)
		}
		return nil, false
	}
	return state, true
}

// passClusterConf is the cluster-conf gate of a pass, and of a latched SP's
// drain wake-up (drainTick): the cluster's stored conf, or false when there is
// none the pass may use.
//
// A cluster missing from the cache drives nothing (RW9): every allocating
// reaction needs its extent_size and batch sizes, and the children already
// log `cluster conf missing` for the idle period, so the miss is silent here.
//
// The CLUSTER's stored conf is checked before the pass is built
// (architecture.md, Common validation), so an unusable ladder produces no
// candidate scan and no model op. The sp drain needs it too — every repair,
// retired-leg and D2 batch maintains a DN capacity key, whose bin index
// comes from that ladder — so
// the gate sits ahead of both of the pass's branches. A conf that does not
// validate is refused through refuseReactionConf. The callers clear that
// refusal's memo once every gate of theirs has passed, and this gate never
// does: a usable cluster conf clearing it would wipe a live SP's memo of its
// own bdev_conf or group refusal, and that refusal would then be logged on
// every pass instead of once.
func (w *spWorker) passClusterConf(ctx context.Context) (*pb.ClusterConf, bool) {
	cc, ok := w.deps.conf.get(w.cid)
	if !ok {
		return nil, false
	}
	if err := model.ValidateClusterConf(cc); err != nil {
		w.refuseReactionConf(ctx, err)
		return nil, false
	}
	return cc, true
}

// refuseReactionConf records the pass gate's refusal, once per distinct error.
// It shares the coordinator's memo with the fan-out's refusal but keeps its
// own half, so an SP whose bdev_conf is bad, or whose group gives its sides
// no length to zero, reports both the fan-out it did not build and the pass
// it did not run — once each, not once per tick.
func (w *spWorker) refuseReactionConf(ctx context.Context, err error) {
	if w.confRefusal.cc == err.Error() {
		return
	}
	w.confRefusal.cc = err.Error()
	slog.ErrorContext(ctx, msgInvalidStoredConf,
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("sp_name", w.desired.handle),
		slog.String("error", err.Error()),
	)
}

// newPass indexes one snapshot into everything the reactions read (AR1):
// failover, thin-pool auto-grow, cntlr replacement, leg repair (AR5-AR8).
func (w *spWorker) newPass(state *model.SpState, cc *pb.ClusterConf) *spPass {
	p := &spPass{
		state:  state,
		cc:     cc,
		th:     model.ResolveEventThreshold(state.Conf.GetEventThreshold()),
		now:    w.deps.clk.nowUnix(),
		judged: w.verdicts.snapshot(state),
	}
	for _, cntlrId := range sortedIds(state.Conf.GetCntlrIdList()) {
		cntlr, ok := state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		if cntlr.GetPrimary() && p.primary == nil {
			p.primaryId, p.primary = cntlrId, cntlr
		}
		if failoverEligible(cntlr) && p.failoverCand == 0 {
			// AR5: the smallest cntlr_id among the healthy, enabled,
			// non-primary cntlrs; the ids are walked ascending.
			p.failoverCand = cntlrId
		}
	}
	if p.primary != nil {
		p.info, p.infoRev = w.primaryInfo(p.primaryId)
	}
	return p
}

// failoverEligible is AR5's candidate test on one cntlr: not the primary, not
// disabled (AR3: disabling is the operator's hands-off signal) and healthy.
func failoverEligible(cntlr *pb.Cntlr) bool {
	return !cntlr.GetPrimary() &&
		!cntlr.GetDisabled() &&
		cntlr.GetErrEpoch() == 0
}

// primaryInfo is the latest CntlrInfo the PRIMARY cntlr's child reported
// (AR1), with the revision of the observation it reports. AR6 reads the pool
// usage out of it and AR8 the spare readiness and the group rows, with that
// revision for step 1's gate; a cntlr whose child has not reported yet — or
// that has no child at all, because its CN could not be resolved or a hold
// (RW22's demotion hold, then RW14's sides-first barrier) has not started it
// yet, or whose child still drives the standby plan because that hold has
// not handed it the promotion yet — yields nil, and both reactions then wait
// rather than guess.
//
// The plan test keeps a held promotion out of the pass. The snapshot the pass
// loads names the new primary as soon as the failover commits, while that
// cntlr's child drives its standby plan until RW14's release, and a standby's
// leg row is transport liveness and ana_state, not the block probe
// (architecture.md, Group on-leg layout: meta region, data region, health
// block) AR8 step 1 reads as a spare's readiness. The test is on the plan the
// child drives, not on its info: from the hand-over until the reply to the
// promotion, the info is still the standby's last report — the promotion's
// own round trip. That report holds no group row and no reading of a group,
// a standby having no group (cnagent.md CN12), so AR8's healthy-leg test
// (otherLegHealthy) holds the switch through that window too, ahead of step
// 1's gate (grpRowHold), whatever the standby's leg row of a spare reads;
// and from the failover's commit on, the old primary's last report, whose
// group rows and readings may still say the array runs, is never the one
// the pass reads.
//
// It is a snapshot, not the live message: the child keeps folding replies into
// its info on its own goroutine while the pass runs on the coordinator's.
func (w *spWorker) primaryInfo(cntlrId uint64) (*pb.CntlrInfo, uint64) {
	child, ok := w.cntlrs[cntlrId]
	if !ok || child.driver == nil || child.plan == nil || !child.plan.primary {
		return nil, 0
	}
	return child.driver.infoSnapshot()
}

// reactionSuppressed is AR3: no reaction runs at
// sp_level >= SP_LEVEL_NO_THINPOOL, where an operator is in charge
// (architecture.md, SpLevel). The record is emitted on the transition only.
//
// A `deleting` SP is not suppressed, it drains (SPD6): reactionPass routes it
// to drainStep before reaching this gate, so this function is never called
// with `deleting` set and the sp_level test below is the whole of it.
func (w *spWorker) reactionSuppressed(ctx context.Context, p *spPass) bool {
	conf := p.state.Conf
	reason := ""
	if conf.GetSpLevel() >= pb.SpLevel_SP_LEVEL_NO_THINPOOL {
		reason = "sp_level"
	}
	r := w.reactor()
	if reason == "" {
		r.suppressed = false
		return false
	}
	if !r.suppressed {
		r.suppressed = true
		slog.InfoContext(ctx, msgReactionSuppressed,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("reason", reason),
			slog.String("sp_level", conf.GetSpLevel().String()),
		)
	}
	return true
}

// ---------------------------------------------------------------------------
// AR5 — primary failover
// ---------------------------------------------------------------------------

// tryFailover is AR5. It reports whether the pass ends here.
//
// The no-candidate case and the two refusals (shared_state, same_error)
// deliberately do NOT end the pass (AR2): they are AR5's skips that let the
// next reaction run.
func (w *spWorker) tryFailover(ctx context.Context, p *spPass) bool {
	if p.primary == nil {
		return false
	}
	// AR5 has two triggers. A `disabled` primary is one in its own right
	// (architecture.md, Cntlrs: "disabling the current primary triggers the
	// primary re-election of Automatic reactions") and fires immediately —
	// disabling is explicit operator intent, and the disabled primary has
	// normally stopped serving by then (its disable request is handed over one
	// RW14 sides-first hold after the bump; a pass inside that hold fails it
	// over before it has) — so no threshold is waited out. Only an enabled
	// primary has to have been unhealthy for a threshold: primary_unhealthy,
	// or, while it is settling, the longer of that and cntlr_unhealthy (below).
	// The candidate rule is unchanged (failoverEligible): a disabled cntlr is
	// never elected.
	threshold := p.th.GetPrimaryUnhealthy()
	if p.primary.GetSettling() && p.th.GetCntlrUnhealthy() > threshold {
		// AR5: a primary that has not yet reported its stack built and clean
		// in that role (HL2, primaryShapeBuilt) is given cntlr_unhealthy, the
		// threshold the worker already gives a cntlr before giving up on it.
		// The hold lasts until that report — the promotion's or, while a new
		// SP's sides are still being zeroed, the end of its first build — and
		// a primary that never makes it is failed over at the same threshold
		// AR7 would replace the cntlr at, unless AR5's refusals (below) hold
		// it. Only when it is the longer wait: nothing orders the two
		// thresholds (AR4), and the hold must never shorten the settled one.
		// model.Failover re-validates the same selection inside its STM
		// (MD6/MD7).
		threshold = p.th.GetCntlrUnhealthy()
	}
	if !p.primary.GetDisabled() {
		// AR10: the threshold fires only on a primary this coordinator has
		// judged unhealthy itself, on failoverRun of its rounds in a row;
		// the disabled trigger needs no verdict.
		if !reached(p.now, p.primary.GetErrEpoch(), threshold) ||
			!p.judged.cntlrs[p.primaryId] ||
			p.judged.runs[p.primaryId] < failoverRun {
			return false
		}
		// AR5's first refusal: a report whose every ERROR row belongs to the
		// stack of a created td whose thin id the pool no longer holds (HL2's
		// shared-state rows) is no trigger. The pool lives on the legs, so the
		// next primary reads the same rows: a failover moves the host paths
		// and nothing else, and the promoted cntlr, never settling, lost the
		// role again at cntlr_unhealthy — to the peer its promotion had made
		// clean — once per window, for as long as the td stayed lost. The
		// err_epoch stays set, and every pass says why AR5 moves nothing, as
		// AR8 does for what only an operator can repair, and goes on (AR2) —
		// to AR7, which declines to replace a primary with no candidate on
		// the same report (replaceTarget).
		if tdIds := sharedStateTds(p.info, p.state); len(tdIds) != 0 {
			w.reactionSkipped(ctx, reactionFailover, reasonSharedState,
				slog.Uint64("cntlr_id", p.primaryId),
				slog.Uint64("td_id", tdIds[0]),
			)
			return false
		}
	}
	if p.failoverCand == 0 {
		w.reactionSkipped(ctx, reactionFailover, reasonNoCandidate,
			slog.Uint64("cntlr_id", p.primaryId),
		)
		return false
	}
	ids := []slog.Attr{
		slog.Uint64("old_cntlr_id", p.primaryId),
		slog.Uint64("new_cntlr_id", p.failoverCand),
	}
	if !p.primary.GetDisabled() && w.reactor().errorFollowedRole(p) {
		// AR5's second refusal, and the pass goes on (AR2).
		w.reactionSkipped(ctx, reactionFailover, reasonSameError, ids...)
		return false
	}
	err := w.reactor().ops.failover(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		p.primaryId, p.failoverCand, p.now,
	)
	if err != nil {
		w.reactionFailed(ctx, reactionFailover, err, ids...)
		return true
	}
	w.reactor().lastFailover = &failoverMemo{
		lost:      p.primaryId,
		errorMemo: newErrorMemo(p.now, p.info),
	}
	w.reactionApplied(ctx, reactionFailover, ids...)
	return true
}

// errorMemo is what a same_error refusal decides by (AR5's second refusal and
// AR7's): the pass's now when the coordinator applied the reaction, and the
// ERROR rows of the latest report of the cntlr it acted on — why it acted,
// which no etcd record keeps.
type errorMemo struct {
	at   uint64
	rows map[cntlrRowId]bool
}

// newErrorMemo records a reaction applied at now on a cntlr whose latest
// report is info.
func newErrorMemo(now uint64, info *pb.CntlrInfo) errorMemo {
	return errorMemo{at: now, rows: rowSet(cntlrErrorRows(info))}
}

// followed is the comparison both same_error refusals make: the error of a
// cntlr whose err_epoch is errEpoch and whose latest report is info is
// presumed to have followed the reaction the memo records — the err_epoch
// was set less than cntlr_unhealthy after it, and every ERROR row of the
// report is one the memo holds, the same map and key, and the same td for a
// thin row. Rows are compared, not causes (dnv-worker.md, Known limits). A
// report with no ERROR row — a cntlr read unreachable has its rows UNKNOWN —
// is presumed nothing, and so is every report against a memo taken over such
// a report, or over none: a reaction applied on a report with no ERROR row
// holds nothing after it.
func (m *errorMemo) followed(
	errEpoch uint64,
	info *pb.CntlrInfo,
	th *pb.EventThreshold,
) bool {
	if errEpoch >= m.at+uint64(th.GetCntlrUnhealthy()) {
		return false
	}
	rows := cntlrErrorRows(info)
	if len(rows) == 0 {
		return false
	}
	for _, row := range rows {
		if !m.rows[row] {
			return false
		}
	}
	return true
}

// failoverMemo is AR5's record of a failover the coordinator applied: the
// cntlr that lost the role, the pass's now, and the ERROR rows of that
// cntlr's latest report — why the role moved.
type failoverMemo struct {
	lost uint64
	errorMemo
}

// errorFollowedRole is AR5's second refusal. The last failover took the role
// from the candidate, and the primary's error is presumed to have followed
// the role (errorMemo.followed): handing the role back would move it again
// over the same error — once per cntlr_unhealthy while the primary never
// settles, sooner once it has. An enabled primary whose report HL2's classes
// call shared state is not failed over at all (the first refusal); this keeps
// an error they do not name — a background read's report of a lost thin id,
// which carries no reason (sharedStateTds), among them — from sending the
// role back to the cntlr it last left while the new primary fails no row the
// old one did not: in an SP of two cntlrs it moves the role once, and in a
// larger one it can first move it on to a cntlr that has not held it. A
// report with any other ERROR row is taken for a fault of the primary's own,
// and an err_epoch set later for a new error: the role moves as for any, and
// that failover records the report's rows in turn. The record goes with the
// coordinator: after a restart or a handoff, one failover more records it
// again.
func (r *reactor) errorFollowedRole(p *spPass) bool {
	m := r.lastFailover
	if m == nil || m.lost != p.failoverCand {
		return false
	}
	return m.followed(p.primary.GetErrEpoch(), p.info, p.th)
}

// rowSet is a list of rows as a set.
func rowSet(rows []cntlrRowId) map[cntlrRowId]bool {
	set := make(map[cntlrRowId]bool, len(rows))
	for _, row := range rows {
		set[row] = true
	}
	return set
}

// ---------------------------------------------------------------------------
// AR6 — thin-pool auto-grow
// ---------------------------------------------------------------------------

// poolUsage is one thin pool's used/total counts as `dmsetup status` reports
// them (AR6): the metadata pair in dm-thin's fixed metadata blocks
// (common.ThinMetaBlockSize), the data pair in the pool's data_block_size.
type poolUsage struct {
	usedMeta  uint64
	totalMeta uint64
	usedData  uint64
	totalData uint64
}

// parseThinPoolStatus parses one raw `dmsetup status` line of a thin pool
// (AR6). The fields after the `thin-pool` token are
//
//	<transaction_id> <used_meta>/<total_meta> <used_data>/<total_data> …
//
// so the token is located rather than a fixed column counted: the line may or
// may not be prefixed with the device name and the start/length pair, and
// everything after the two ratios (held metadata root, the rw/ro and discard
// flags, needs_check, the low watermark) is of no interest here.
//
// A pool that has failed reports `thin-pool Fail` and parses as garbage, which
// is exactly right: a failed pool is not grown, it is reported.
func parseThinPoolStatus(details string) (poolUsage, bool) {
	fields := strings.Fields(details)
	token := -1
	for idx, field := range fields {
		if field == "thin-pool" {
			token = idx
			break
		}
	}
	if token < 0 || len(fields) < token+4 {
		return poolUsage{}, false
	}
	usedMeta, totalMeta, ok := parseRatio(fields[token+2])
	if !ok {
		return poolUsage{}, false
	}
	usedData, totalData, ok := parseRatio(fields[token+3])
	if !ok {
		return poolUsage{}, false
	}
	return poolUsage{
		usedMeta:  usedMeta,
		totalMeta: totalMeta,
		usedData:  usedData,
		totalData: totalData,
	}, true
}

// parseRatio parses one `<used>/<total>` field of a thin-pool status line.
func parseRatio(field string) (uint64, uint64, bool) {
	used, total, found := strings.Cut(field, "/")
	if !found {
		return 0, 0, false
	}
	usedVal, err := strconv.ParseUint(used, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	totalVal, err := strconv.ParseUint(total, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return usedVal, totalVal, true
}

// parsePoolStatus is parseThinPoolStatus plus AR6's "an unparsable line is
// skipped and logged once per change": the memo is per slice and is dropped
// again as soon as the slice reports a line that parses, so a line that goes
// bad, good and bad again is reported each time it turns.
func (w *spWorker) parsePoolStatus(
	ctx context.Context,
	sliceId uint64,
	details string,
) (poolUsage, bool) {
	usage, ok := parseThinPoolStatus(details)
	r := w.reactor()
	if ok {
		delete(r.badLine, sliceId)
		return usage, true
	}
	if last, seen := r.badLine[sliceId]; seen && last == details {
		return poolUsage{}, false
	}
	r.badLine[sliceId] = details
	slog.InfoContext(ctx, msgPoolStatusUnparsable,
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.Uint64("slice_id", sliceId),
		slog.String("details", details),
	)
	return poolUsage{}, false
}

// tryGrow is AR6. It reports whether the pass ends here.
//
// Slices are walked in SpConf.slice_id_list order and DATA is checked before
// metadata, so a pool breaching both grows its data first — the one that runs
// out with user IO on it.
//
// A breach the worker cannot turn into a grow is NOT an action, and does not
// end the pass. AR6 scopes its pending rule to "no grow OF THAT KIND starts",
// so a pending data grow leaves the same slice's metadata — and every other
// slice — free to grow; and a grow that is pending on the CN ([D15];
// architecture.md, Automatic reactions) or capped by a ceiling of
// architecture.md, GrowSlice, can stay that way indefinitely, so ending
// the pass on it would disable AR7 and AR8 for the whole SP for exactly as
// long. Only an APPLIED grow, or one of the two skips AR2 makes pass-ending
// (ErrPrecondition, no candidate), ends the pass.
func (w *spWorker) tryGrow(ctx context.Context, p *spPass) bool {
	// The SP's stored percentage, used as stored (architecture.md, Common
	// validation): a zero is invalid and the pass gate already refused it, so
	// there is no default arm here.
	lwm := uint64(
		p.state.Conf.GetBdevConf().GetDmPoolConf().GetLowWaterMarkPct(),
	)
	if lwm > 100 {
		// architecture.md, Common validation: values above 100 switch the
		// automation off; operators grow manually.
		return false
	}
	if p.info == nil {
		return false
	}
	rows := p.info.GetSliceIdToDmPool()
	for _, sliceId := range p.state.Conf.GetSliceIdList() {
		slice, ok := p.state.Slices[sliceId]
		if !ok {
			continue
		}
		row, ok := rows[sliceId]
		if !ok || row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			// AR6: an ERROR / PROVISIONING / absent pool is never grown.
			continue
		}
		usage, ok := w.parsePoolStatus(ctx, sliceId, row.GetDetails())
		if !ok {
			continue
		}
		// Each kind is tried on its own: AR6's pending rule is per kind,
		// so a data grow the CN has not acted on yet must not shadow a
		// metadata breach of the same pool — metadata filling up puts the
		// pool into needs_check and the whole SP read-only.
		if usage.usedData*100 > lwm*usage.totalData &&
			w.runGrow(ctx, p, sliceId, slice, false, usage) {
			return true
		}
		if usage.usedMeta*100 > lwm*usage.totalMeta &&
			w.runGrow(ctx, p, sliceId, slice, true, usage) {
			return true
		}
	}
	return false
}

// runGrow runs one AR6 grow of one kind on one slice. It reports whether the
// pass ends here, which distinguishes the two shapes of "no grow ran":
//
//   - the grow is not APPLICABLE — pending (AR6), the group ceiling or ladder
//     cap of architecture.md, GrowSlice, no data group to size a data grow by:
//     the record is emitted and false is returned, so the walk goes on to the
//     other kind, the next slice and finally to AR7 and AR8;
//   - the grow WAS applicable and did not complete — no candidate, an
//     ErrPrecondition, a failed scan or op: AR2 ends the pass, so that a pass
//     that has already touched etcd (or may have) applies nothing else.
func (w *spWorker) runGrow(
	ctx context.Context,
	p *spPass,
	sliceId uint64,
	slice *pb.Slice,
	isMeta bool,
	usage poolUsage,
) bool {
	kind := reactionGrowData
	if isMeta {
		kind = reactionGrowMeta
	}
	sliceAttr := slog.Uint64("slice_id", sliceId)
	blockSize := dataBlockSize(p.state.Conf.GetBdevConf())
	if growPending(slice, isMeta, usage, blockSize) {
		// AR6: while pending, no grow of THAT KIND starts. Another kind, and
		// every other reaction, is untouched.
		w.reactionSkipped(ctx, kind, reasonGrowPending, sliceAttr)
		return false
	}
	// The cluster's stored extent size — the same value model.GrowSlice reads
	// inside its STM, which is what makes this pre-check ask the allocator
	// for the ext_cnt the transaction will then demand.
	extentSize := p.cc.GetDnBinConf().GetExtentSize()
	extCnt, reason := growExtCnt(slice, isMeta, extentSize)
	if reason != "" {
		// grp_list_full, meta_ladder_cap and no_data_group are all
		// permanent for this slice and this kind: there is no grow to run,
		// ever, and the pass must go on to the reactions that still can do
		// something.
		w.reactionSkipped(ctx, kind, reason, sliceAttr)
		return false
	}
	legs := legCnt(p.state.Conf)
	batch := int(p.cc.GetAllocConf().GetDnBatchSize())
	// AR6: the black list starts empty — a new group may perfectly well land
	// on a DN that already carries another group of this SP — and grows as
	// the picks are drawn, so the legs of the ONE new group land on distinct
	// DNs. The location exclusion is empty for the same reason
	// (architecture.md, Per-operation allocation, leaves the grow on the plain
	// scan).
	cands, err := w.reactor().ops.findDnCandidates(
		ctx, w.cid, p.cc, extCnt, legs*batch, legs, nil, nil,
	)
	if err != nil {
		w.reactionFailed(ctx, kind, err, sliceAttr)
		return true
	}
	picks := pickDistinct(cands, legs)
	if len(picks) < legs {
		w.reactionSkipped(ctx, kind, reasonNoCandidate, sliceAttr)
		return true
	}
	grpId, err := w.reactor().ops.growSlice(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		sliceId, isMeta, poolTotal(usage, isMeta), p.cc, picks,
	)
	if err != nil {
		w.reactionFailed(ctx, kind, err, sliceAttr)
		return true
	}
	w.reactionApplied(ctx, kind,
		sliceAttr,
		slog.Uint64("grp_id", grpId),
		slog.Uint64("ext_cnt", extCnt),
		slog.Any("addr_port_list", candAddrs(picks)),
	)
	return true
}

// poolTotal is the total the primary reported for the pool of one kind
// (AR6): the metadata total for a meta grow, the data total for a data one.
func poolTotal(usage poolUsage, isMeta bool) uint64 {
	if isMeta {
		return usage.totalMeta
	}
	return usage.totalData
}

// growPending is AR6's stateless pending rule as the pass's cheap pre-check:
// it saves a candidate scan for a grow that cannot run. The rule itself is
// model.GrowPending, which model.GrowSlice also applies inside its STM as the
// precondition AR2 requires — one rule, one implementation, so the pass and
// the transaction can never disagree about what "pending" means.
func growPending(
	slice *pb.Slice,
	isMeta bool,
	usage poolUsage,
	blockSize uint64,
) bool {
	return model.GrowPending(slice, isMeta, poolTotal(usage, isMeta), blockSize)
}

// growExtCnt is the ext_cnt of the group a grow would append (architecture.md,
// GrowSlice). The worker computes it because the allocator must search for
// exactly that size BEFORE model.GrowSlice recomputes it inside its own STM: a
// data grow uses the slice's first data group's ext_cnt — the original
// allocation unit — and a meta grow the ladder value, which doubles the slice's
// meta total. A list already at the group ceiling of architecture.md,
// GrowSlice, has no group to size at all, and is checked first, as
// model.GrowSlice checks it ahead of its own sizing.
//
// The second return value is a `reaction skipped` reason, empty on success.
func growExtCnt(
	slice *pb.Slice,
	isMeta bool,
	extentSize uint64,
) (uint64, string) {
	if model.GrpListFull(slice, isMeta) {
		return 0, reasonGrpListFull
	}
	if !isMeta {
		grps := slice.GetDataGrpList()
		if len(grps) == 0 || grps[0].GetExtCnt() == 0 {
			return 0, reasonNoDataGroup
		}
		return grps[0].GetExtCnt(), ""
	}
	total := uint64(0)
	for _, grp := range slice.GetMetaGrpList() {
		total += grp.GetExtCnt()
	}
	extCnt, ok := model.MetaLadderExtCnt(total, extentSize)
	if !ok {
		return 0, reasonMetaLadderCap
	}
	return extCnt, ""
}

// ---------------------------------------------------------------------------
// AR7 — cntlr replacement
// ---------------------------------------------------------------------------

// tryReplaceCntlr is AR7. It reports whether the pass ends here.
//
// A primary its two refusals hold (replaceTarget) does NOT end the pass
// (AR2).
func (w *spWorker) tryReplaceCntlr(ctx context.Context, p *spPass) bool {
	oldId, old := w.replaceTarget(ctx, p)
	if old == nil {
		return false
	}
	ids := []slog.Attr{slog.Uint64("old_cntlr_id", oldId)}
	// The footprint is what one cntlr's CN reserves for the whole SP
	// (architecture.md, Cntlrs), and it is what model.ReplaceCntlr re-computes
	// inside its STM: the scan has to ask for the same number or the pick would
	// fail there.
	footprint := spFootprint(p.state)
	batch := int(p.cc.GetAllocConf().GetCnBatchSize())
	// AR7: the old CN is black-listed even when the node itself is healthy —
	// its cntlr is what failed. The SP's other cntlrs' CNs are excluded by
	// the rule of architecture.md, Finding CN candidates, instead, which is
	// spCnAddrs, and their locations by the tier 1 of architecture.md,
	// Per-operation allocation. Both come from this pass's snapshot, so the
	// same spCnAddrs goes to model.ReplaceCntlr, which refuses the pick as
	// `candidate changed` when the SP has gained a cntlr the snapshot did not
	// hold; the next pass plans from a snapshot that holds it.
	spCnAddrs := otherCntlrAddrs(p, oldId)
	cands, err := w.reactor().ops.findCnCandidates(
		ctx, w.cid, footprint, batch,
		[]string{old.GetAddrPort()}, spCnAddrs,
		otherCntlrLocations(p, oldId),
	)
	if err != nil {
		w.reactionFailed(ctx, reactionReplaceCntlr, err, ids...)
		return true
	}
	picks := model.PickRandom(cands, 1)
	if len(picks) == 0 {
		w.reactionSkipped(ctx, reactionReplaceCntlr, reasonNoCandidate, ids...)
		return true
	}
	newId, err := w.reactor().ops.replaceCntlr(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		oldId, picks[0], spCnAddrs, old.GetPrimary(), p.now,
	)
	if err != nil {
		w.reactionFailed(ctx, reactionReplaceCntlr, err, ids...)
		return true
	}
	if oldId == p.primaryId {
		// AR7's record: the pass holds the primary's report alone (AR1), so
		// a standby's replacement records nothing and leaves it as it is.
		w.reactor().lastReplace = &replaceMemo{
			fresh:     newId,
			errorMemo: newErrorMemo(p.now, p.info),
		}
	}
	w.reactionApplied(ctx, reactionReplaceCntlr,
		slog.Uint64("old_cntlr_id", oldId),
		slog.Uint64("new_cntlr_id", newId),
		slog.String("addr_port", picks[0].AddrPort),
		slog.Bool("primary", old.GetPrimary()),
	)
	return true
}

// replaceTarget is AR7's trigger: the cntlr with the smallest cntlr_id that
// has been unhealthy for cntlr_unhealthy, whose latest verdict by this
// coordinator is unhealthy (AR10), is not disabled (AR3), and is either not
// the primary or is the primary of an SP with no failover
// candidate — the sole-cntlr SP of AR7 — unless one of AR7's two
// refusals holds that primary (below): it is recorded and passed over, and
// the scan goes on.
func (w *spWorker) replaceTarget(
	ctx context.Context,
	p *spPass,
) (uint64, *pb.Cntlr) {
	for _, cntlrId := range sortedIds(p.state.Conf.GetCntlrIdList()) {
		cntlr, ok := p.state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		if cntlr.GetDisabled() {
			continue
		}
		// AR10: only a cntlr this coordinator has judged unhealthy itself.
		if !reached(p.now, cntlr.GetErrEpoch(), p.th.GetCntlrUnhealthy()) ||
			!p.judged.cntlrs[cntlrId] {
			continue
		}
		if cntlr.GetPrimary() && p.failoverCand != 0 {
			// AR5 moves the role away first, or holds it where a failover
			// cannot help or is presumed not to (its shared_state and
			// same_error); replacing it as primary would fail
			// model.ReplaceCntlr's own precondition anyway.
			continue
		}
		// AR7's two refusals, both judged on the report the pass holds, the
		// primary's latest (AR1); no standby's is read, and none carries a
		// thin row (cnagent.md CN14), so a standby's ERROR rows are its own.
		//
		// The first is AR5's first (sharedStateTds): a primary whose every
		// ERROR row belongs to the stack of a created td whose thin id the
		// pool no longer holds is not replaced. The replacement would read
		// the same rows from the same pool. The report carries the lost-id
		// reason only while it is a converge's observation (dnv-worker.md,
		// Known limits).
		//
		// The second is AR5's second, for a replacement
		// (errorFollowedReplacement): a background read's observation of the
		// same loss carries no reason, so the first lets the primary be
		// replaced on a round answering from it, and the replacement,
		// reading the same rows, would be replaced in turn once per
		// cntlr_unhealthy, for as long as the td stayed lost.
		if cntlrId == p.primaryId {
			if tdIds := sharedStateTds(p.info, p.state); len(tdIds) != 0 {
				w.reactionSkipped(ctx, reactionReplaceCntlr, reasonSharedState,
					slog.Uint64("old_cntlr_id", cntlrId),
					slog.Uint64("td_id", tdIds[0]),
				)
				continue
			}
			if w.reactor().errorFollowedReplacement(p, cntlrId, cntlr) {
				w.reactionSkipped(ctx, reactionReplaceCntlr, reasonSameError,
					slog.Uint64("old_cntlr_id", cntlrId),
				)
				continue
			}
		}
		return cntlrId, cntlr
	}
	return 0, nil
}

// replaceMemo is AR7's record of the last replacement of a primary the
// coordinator applied: the replacement it created, the pass's now, and the
// ERROR rows of the replaced primary's latest report — why it was replaced.
type replaceMemo struct {
	fresh uint64
	errorMemo
}

// errorFollowedReplacement is AR7's second refusal, the twin of AR5's. The
// primary cntlr is the replacement the last replacement of a primary created,
// and its error is presumed to have followed the replacement
// (errorMemo.followed): replacing it would put the same rows on a third cntlr,
// once per cntlr_unhealthy. A replacement whose report has a row of its own
// beside those rows, or whose err_epoch was set later, is replaced as any, and
// that replacement records its rows in turn. The record goes with the
// coordinator: after a restart or a handoff, one replacement more records it
// again.
func (r *reactor) errorFollowedReplacement(
	p *spPass,
	cntlrId uint64,
	cntlr *pb.Cntlr,
) bool {
	m := r.lastReplace
	if m == nil || m.fresh != cntlrId {
		return false
	}
	return m.followed(cntlr.GetErrEpoch(), p.info, p.th)
}

// otherCntlrAddrs are the endpoints of every cntlr of the SP except the one
// being replaced: architecture.md, Finding CN candidates, forbids two cntlrs of
// one SP on one CN, and MD5 takes that list as spCnAddrs.
func otherCntlrAddrs(p *spPass, oldId uint64) []string {
	addrs := make([]string, 0, len(p.state.Cntlrs))
	for _, cntlrId := range sortedIds(p.state.Conf.GetCntlrIdList()) {
		if cntlrId == oldId {
			continue
		}
		cntlr, ok := p.state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		addrs = append(addrs, cntlr.GetAddrPort())
	}
	return addrs
}

// otherCntlrLocations is AR7's tier-1 exclusion (architecture.md, Per-operation
// allocation): the failure domain of every CN otherCntlrAddrs names, resolved
// through the pass's OWN snapshot of the node records — MD3 reads one CnConf
// per distinct Cntlr.addr_port in the same transaction as the cntlrs — so it
// costs no further read. A CN missing from that snapshot contributes no
// location; it is excluded by address anyway. The old cntlr adds no location of
// its own, on purpose: it is the one leaving, and the replacement is kept off
// the domains the SP keeps — so the old cntlr's domain is excluded only when a
// survivor shares it.
//
// With the default `location = addr_port` this excludes exactly the CNs
// spCnAddrs already does.
func otherCntlrLocations(p *spPass, oldId uint64) []string {
	var locs []string
	for _, addrPort := range otherCntlrAddrs(p, oldId) {
		if cn, ok := p.state.CnByAddr[addrPort]; ok {
			locs = append(locs, cn.GetLocation())
		}
	}
	return locs
}

// spFootprint is the Σ ext_cnt over ALL groups of ALL slices of the SP, meta
// and data alike — the worker-side twin of model's, which recomputes it inside
// the STM (architecture.md, Cntlrs).
func spFootprint(state *model.SpState) uint64 {
	total := uint64(0)
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range allGroups(slice) {
			total += grp.GetExtCnt()
		}
	}
	return total
}

// ---------------------------------------------------------------------------
// AR8 — leg repair
// ---------------------------------------------------------------------------

// repairTarget is one leg the pass may repair, with everything the ops need to
// address it: the slice whose record holds the leg or, for a leg of a clone
// meta group, the clone whose record does (clone set, sliceId zero), which
// the clone-keyed ops take instead (MD6).
type repairTarget struct {
	sliceId uint64
	clone   *cloneOwner
	grp     *pb.Group
	leg     *pb.Leg
}

// ids are the attributes that name the target in the pass's records: its
// slice, or its clone by name and clone_id, then its group and its leg.
func (t *repairTarget) ids() []slog.Attr {
	owner := []slog.Attr{slog.Uint64("slice_id", t.sliceId)}
	if t.clone != nil {
		owner = []slog.Attr{
			slog.String("clone_name", t.clone.name),
			slog.Uint64("clone_id", t.clone.id),
		}
	}
	return withAttr(owner,
		slog.Uint64("grp_id", t.grp.GetGrpId()),
		slog.Uint64("leg_id", t.leg.GetLegId()),
	)
}

// tryLegRepair is AR8: one step per pass on the group of the unhealthy leg
// with the smallest leg_id, across the slices' groups and the clone meta
// groups alike. It reports whether the pass ends here.
//
// AR8's seven holds — "the leg has exactly one side" (this leg), a group
// that has a repair, a group with no other leg of its leg_list free of a
// health epoch and held in sync, step 1's gate on the
// group's row, step 2's wait for a pending spare, step 4's full spare list
// and step 3's spare still unprovisioned (this group) — are part of the
// candidate test, not reasons to end the pass. Each is a property of ONE leg
// or ONE group, and each can hold for a very long time: a two-sided leg has
// a user migration in flight (hours), a group with an operator's repair waits
// for the operator to finish or cancel it, a group whose other leg_list legs
// are unhealthy or not in sync stays so until one of them answers again and
// md holds it in sync, a group whose row
// does not read OK stays so until it does — its array stopped or broken
// until its legs come back or an operator acts, or held by cnagent.md CN12's
// in-sync guard — since the worker switches no spare into it, a row from a
// report older than the snapshot waits for the primary's report of the
// syncup the snapshot's revision brings (grpRowHold), a spare that
// cannot be connected stays pending until its leg has been unhealthy for
// leg_unhealthy (and one the primary never reports stays pending for good),
// only DeleteSpareLeg by an operator frees a spare slot (AR8 step 4), and a
// spare whose DN failed while it zeroed stays unprovisioned until that DN
// finishes zeroing it or an operator deletes it. Ending the pass on any of
// them would leave every OTHER group of the SP degraded on a single md-raid1
// member for exactly as long, so a further failure there is data loss. Each
// is still recorded per leg for visibility (AR8 step 4; the worker suite's
// reaction case greps `reason=spare_list_full`) and the scan moves on.
func (w *spWorker) tryLegRepair(ctx context.Context, p *spPass) bool {
	for _, target := range repairCandidates(p) {
		ids := target.ids()
		if !isMdRaid1(p.state.Conf) {
			// AR8/AR9: a RedundNone group has no redundancy to re-home, so
			// the worker only ever logs it. An operator moves the data.
			// Redundancy is an SP-wide property (architecture.md, GrowSlice),
			// so no later candidate can be any different and the pass ends
			// here.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonRedundNone, ids...,
			)
			return true
		}
		if len(target.leg.GetSideList()) != 1 {
			// AR8: two sides means a user migration is in flight on this
			// leg. The next candidate may still be repairable.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonTwoSides, ids...,
			)
			continue
		}
		if repName := target.grp.GetRepName(); repName != "" {
			// AR8: a group that has a repair is left to it, on every pass,
			// which is the wait of an operator's open repair: a spare and
			// the repair's new leg would race for one place in the array,
			// so the group holds one of the two at a time (architecture.md,
			// Repair), and the spare creates refuse it too (MD6). The worker
			// drives no repair of its own here (AR11). The next candidate,
			// in another group, may still be repairable.
			w.reactionSkipped(ctx, reactionSpareCreate, reasonGrpHasRepair,
				withAttr(ids, slog.String("rep_name", repName))...,
			)
			continue
		}
		if !otherLegHealthy(p, target) {
			// AR8's healthy-leg precondition, on every group: every other
			// leg of the group's leg_list has a health epoch or is not held
			// in sync by the current primary's latest report, so a switch or
			// a create would start replacing the last legs that may still
			// hold what was written on the group with fresh spares holding
			// none of it. A leg with no health epoch can still be half
			// copied — a spare switched in, a new leg finished while md
			// copies — and switching out the only leg in sync loses the
			// group at the next failover. md learns that a member is dead
			// only from IO to it, so an idle array whose legs are all dead
			// still runs and its row reads OK, which step 1's gate lets
			// through; what the cn agent then does with such a switch is
			// cnagent.md CN12's in-sync guard and its Known limits. The leg
			// epochs come from the primary's probe of each leg, which runs
			// with or without IO, so this test sees what the row does not;
			// and a primary whose own network fails, which reads every leg
			// unhealthy at once, leaves the group as it is too. The group
			// waits until one of those legs answers again and md holds it
			// in sync; the next candidate, in another group, may still be
			// repairable.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonNoHealthyLeg, ids...,
			)
			continue
		}
		if spare := readySpare(target.grp, p.info); spare != nil {
			if reason := grpRowHold(p, target.grp.GetGrpId()); reason != "" {
				// AR8 step 1's gate: the switch runs only while the group's
				// row in the primary's latest report reads OK. An OK row
				// means its array runs, whole or degraded (cnagent.md
				// CN28); a running array can still read ERROR — a read of
				// it that did not answer, a foreign member or a second
				// array holding the group's legs, a member change the
				// converge could not make, or CN12's in-sync guard — and
				// no other status says the array runs, so every status
				// but OK holds, and so does an OK row in a report older
				// than the snapshot (grpRowHold). A switch into a group
				// whose array does not run is where the hazards start —
				// two legs dead together, both switched out, and a fresh
				// array created over legs with no superblock (cnagent.md
				// CN12); a leg that dies during the copy onto a
				// switched-in spare and is switched out in its turn,
				// leaving the half-copied spare the only leg md can
				// assemble from — so a stopped array is left for its legs
				// to come back or for an operator. The hold is THIS
				// group's (AR2) and lasts as long as the row stays so, so
				// the scan goes on; the group keeps its ready
				// spare, so it gets no create here (step 3). The kind is
				// the switch's, the step held, as step 2's. No STM sees a
				// report, so the gate is the pass's alone: the operator's
				// SwitchSpareLeg is not held by it.
				w.reactionSkipped(ctx, reactionSpareSwitch, reason,
					withAttr(ids, slog.Uint64("spare_leg_id", spare.GetLegId()))...,
				)
				continue
			}
			return w.switchSpare(ctx, target, spare, ids)
		}
		if spare := pendingSpare(p, target.grp); spare != nil {
			// The kind names the step of the AR8 procedure that is being
			// deferred: here the switch, which runs as soon as the spare is
			// ready, as in step 1's gate above. Every other AR8 skip names
			// the create — the step the procedure would otherwise have
			// started with.
			//
			// The wait holds THIS group only (AR2): a spare whose leg keeps
			// reading ERROR stays pending for up to leg_unhealthy, and ending
			// the pass would hold every other group's repair that long.
			// A second due leg of the same group never gets here: its
			// partner then carries an err_epoch too, so the healthy-leg test
			// above stops both legs with no_healthy_leg, and the scan cannot
			// create a second spare for the group here either.
			w.reactionSkipped(ctx, reactionSpareSwitch, reasonSparePending,
				withAttr(ids, slog.Uint64("spare_leg_id", spare.GetLegId()))...,
			)
			continue
		}
		if len(target.grp.GetSpareLegList()) >= common.MaxSpareLegPerGrp {
			// AR8 step 4: the worker never deletes a parked leg; only
			// DeleteSpareLeg by an operator frees a slot. That is an
			// operator event on THIS group, so the scan goes on to the next
			// candidate rather than stranding the rest of the SP behind it.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonSpareListFull, ids...,
			)
			continue
		}
		if spare := model.UnprovisionedSpare(target.grp); spare != nil {
			// model.CreateSpareLeg, and its clone-keyed twin
			// model.CreateCloneSpareLeg, refuses while a spare of the group
			// has a side still unprovisioned (MD6). The unprovisioned spares
			// that get here are the ones step 2 does not wait for — a side
			// with an err_epoch, its DN unreachable or reporting ERROR while
			// it zeroed, say, or a second side a migration gave the spare —
			// and the first kind stays unprovisioned until its DN finishes
			// zeroing it or an operator deletes it. That is an event on THIS
			// group, so the scan goes on to the next candidate rather than
			// ending the pass on the op's refusal, and runs no candidate scan
			// the op could only throw away.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonSpareUnprovisioned,
				withAttr(ids, slog.Uint64("spare_leg_id", spare.GetLegId()))...,
			)
			continue
		}
		return w.createSpare(ctx, p, target, ids)
	}
	return false
}

// repairCandidates is AR8's trigger scan over BOTH group lists of every slice
// and over the clone meta group of every clone: every leg_list leg that needs
// repair, in ascending leg_id order across the whole SP — AR8's "several
// unhealthy legs ⇒ the smallest leg_id first". Spare legs are never
// scanned — a parked leg keeps its err_epoch and is never repaired again
// (AR8 steps 1 and 4). A clone being deleted is passed over: its group goes
// back to its DNs in the clone drain's final transaction (CLD9), so a repair
// would spend an extent for nothing, and the clone-keyed spare ops refuse it
// anyway (MD6). A clone with no group has no leg.
//
// The whole ordered list is returned rather than only its head because AR8's
// per-leg preconditions are applied by tryLegRepair as it walks it: the
// smallest-leg_id ordering has to run over the legs that are actually
// repairable, or one leg an operator has to unblock would block them all.
func repairCandidates(p *spPass) []*repairTarget {
	var found []*repairTarget
	for _, sliceId := range p.state.Conf.GetSliceIdList() {
		slice, ok := p.state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range allGroups(slice) {
			for _, leg := range grp.GetLegList() {
				if !legNeedsRepair(p, leg) {
					continue
				}
				found = append(found, &repairTarget{
					sliceId: sliceId, grp: grp, leg: leg,
				})
			}
		}
	}
	for _, cloneName := range p.state.Conf.GetCloneNameList() {
		clone, ok := p.state.Clones[cloneName]
		if !ok || clone.GetDeleting() || clone.GetMetaGrp() == nil {
			continue
		}
		owner := &cloneOwner{name: cloneName, id: clone.GetCloneId()}
		grp := clone.GetMetaGrp()
		for _, leg := range grp.GetLegList() {
			if !legNeedsRepair(p, leg) {
				continue
			}
			found = append(found, &repairTarget{
				clone: owner, grp: grp, leg: leg,
			})
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].leg.GetLegId() < found[j].leg.GetLegId()
	})
	return found
}

// otherLegHealthy is AR8's healthy-leg precondition, on a slice's group and
// a clone meta group alike: a leg of the target's group's leg_list other
// than the target has no health epoch and the current primary's latest
// report holds it in sync (grp_id_to_grp_info's in_sync_leg_id_list;
// architecture.md, Repair). Spare legs do not count: nothing written on the
// group reaches a leg while it is a spare. A report that holds no reading
// of the group — none at all, a standby's, or one of a group whose array
// does not run — holds no leg in sync, so the group waits.
func otherLegHealthy(p *spPass, target *repairTarget) bool {
	inSync := p.info.GetGrpIdToGrpInfo()[target.grp.GetGrpId()].
		GetInSyncLegIdList()
	for _, leg := range target.grp.GetLegList() {
		if leg.GetLegId() != target.leg.GetLegId() &&
			leg.GetErrEpoch() == 0 &&
			slices.Contains(inSync, leg.GetLegId()) {
			return true
		}
	}
	return false
}

// legNeedsRepair is AR8's two triggers. Both require Leg.err_epoch != 0: a
// side the worker cannot reach while the primary still sees the leg healthy
// triggers nothing. Both also require this coordinator's own verdict (AR10):
// on the leg, and in case 2 on the side whose clock fires as well.
//
//	Case 1 — the leg has been unhealthy for leg_unhealthy: the primary has no
//	         healthy path to it, possibly a CN↔DN problem, hence the long wait.
//	Case 2 — the leg is unhealthy AND its side has been unhealthy for
//	         side_unhealthy: the DN itself is probably dead, hence the short
//	         wait.
func legNeedsRepair(p *spPass, leg *pb.Leg) bool {
	if leg.GetErrEpoch() == 0 || !p.judged.legs[leg.GetLegId()] {
		return false
	}
	if reached(p.now, leg.GetErrEpoch(), p.th.GetLegUnhealthy()) {
		return true
	}
	for _, side := range leg.GetSideList() {
		key := sideKey{legId: leg.GetLegId(), sideId: side.GetSideId()}
		if p.judged.sides[key] &&
			reached(p.now, side.GetErrEpoch(), p.th.GetSideUnhealthy()) {
			return true
		}
	}
	return false
}

// spareReady is AR8 step 1's readiness test on one spare: its single side is
// provisioned (architecture.md, Side provisioning protocol) and the PRIMARY's
// latest report has its leg RES_STATUS_OK — connected and probed
// (architecture.md, Spare legs). A leg whose prober on the primary has not
// completed a round reads RES_STATUS_PENDING (cnagent.md CN11), so a fresh
// spare is not ready before its first probe.
//
// Leg.err_epoch is deliberately NOT part of it: a leg the primary reports OK
// has had its err_epoch cleared by HL2 already, so a parked leg that recovers
// becomes usable spare capacity again, which is exactly what parking it was
// for.
func spareReady(spare *pb.Leg, info *pb.CntlrInfo) bool {
	side := singleSide(spare)
	if side == nil || !side.GetProvisioned() {
		return false
	}
	row, ok := info.GetLegIdToLeg()[spare.GetLegId()]
	return ok && row.GetStatus() == pb.ResStatus_RES_STATUS_OK
}

// readySpare is AR8 step 1: the ready spare with the smallest leg_id. A
// user-created ready spare qualifies the same way; that is what spares are
// for.
func readySpare(grp *pb.Group, info *pb.CntlrInfo) *pb.Leg {
	for _, spare := range sortedLegs(grp.GetSpareLegList()) {
		if spareReady(spare, info) {
			return spare
		}
	}
	return nil
}

// grpRowHold is AR8 step 1's gate on the target's group, which the retire
// timer reads as well (retireHold): "" when the group's row
// (grp_id_to_md_raid) in p.info — the current primary's latest report, the
// pass's only one (primaryInfo) — reads RES_STATUS_OK, so its array runs,
// whole or degraded (cnagent.md CN28), and that report is of an observation
// made at the snapshot's revision or later; otherwise the reason the switch,
// or the timer's delete, waits, naming the row's status, grp_row_absent for
// a report that holds no row of the group, such as a standby's (cnagent.md
// CN12), or grp_row_stale for an OK row of an older observation. The row is
// read with its presence, since an absent row's status would read
// RES_STATUS_UNKNOWN. A group id is unique in the SP, a clone meta group's
// included (architecture.md, Terminology and object model), so one map
// serves both kinds of group.
//
// The revision test keeps a second switch off a group before the primary
// has reported a converge of the first. Every switch bumps SpRev (AR2), and
// a report of an older observation describes the array from before it. The
// agent can make such a report after the switch: while the switch's syncup
// waits for the cntlr's lock, a converge of the old leg_list can hold it,
// as the CN10 retry of a late member does (cnagent.md CN10), and its
// observation reads the array running with the old legs. A second switch
// read off that row could take the group's last leg with an md superblock
// out of leg_list before the primary has added the first spare to the
// array, and a new primary can then create a fresh empty array over legs
// none of which holds the data (cnagent.md CN12, Known limits). The test
// compares with the snapshot's SpRevision, not with the group's own last
// change, which no record keeps, so every group waits after any bump until
// the primary reports an observation made at that revision, which the
// syncup the bump's fan-out sends brings (RW14, RW6). The retire timer
// waits for the same report: the whole flag of an older one may be the
// old leg set's, read before a spare took a leg's place and while md has
// not yet started to copy onto it (AR12).
func grpRowHold(p *spPass, grpId uint64) string {
	row, ok := p.info.GetGrpIdToMdRaid()[grpId]
	if !ok {
		return reasonGrpRowAbsent
	}
	switch row.GetStatus() {
	case pb.ResStatus_RES_STATUS_OK:
		if p.infoRev < p.state.SpRevision {
			return reasonGrpRowStale
		}
		return ""
	case pb.ResStatus_RES_STATUS_ERROR:
		return reasonGrpRowError
	case pb.ResStatus_RES_STATUS_MISSING:
		return reasonGrpRowMissing
	case pb.ResStatus_RES_STATUS_PROVISIONING:
		return reasonGrpRowProvisioning
	case pb.ResStatus_RES_STATUS_PENDING:
		return reasonGrpRowPending
	default:
		// RES_STATUS_UNKNOWN — an unreachable primary's rows, which its
		// driver marks so (HL1) — and any status this build does not know.
		return reasonGrpRowUnknown
	}
}

// pendingSpare is AR8 step 2: a spare that is on its way — not ready yet,
// either still provisioning or not yet reported OK by the primary — and not
// dead. A DEAD spare is never pending — it can still be READY, since
// spareReady ignores every err_epoch, and step 1 has then switched it in
// already — and a PARKED old leg is the commonest one, which is what makes a
// second repair of the same group create a second spare rather than wait for
// one that will never arrive. A spare is dead when its side has an err_epoch —
// the worker cannot reach its DN or the side reports an ERROR row (AR8 case
// 2's condition), which a leg parked by case 2 still carries — or when its
// leg has been unhealthy for leg_unhealthy, which a leg parked by case 1 has
// already reached. Dead is a current state, not an identity: a parked leg
// whose DN comes back, or that recovers and fails again, is pending again by
// the same tests, and holds its group's next repair until it reads OK or
// times out.
// A dead spare that is still unprovisioned — its DN failed while it zeroed —
// makes no room for another: step 3 holds the group instead (tryLegRepair,
// model.UnprovisionedSpare).
//
// The leg's err_epoch is held to AR8 case 1's threshold (legNeedsRepair)
// rather than read as a verdict, because HL2 probes spares too: the moment a
// fresh spare's side is provisioned the primary starts connecting to it and
// may report the leg ERROR until the connect completes, which stamps the
// leg's err_epoch at once, with no grace (healthMonitor.observe). Read bare,
// that transient would turn the very spare this step waits for into a dead
// one, and a pass landing inside it would create a SECOND spare for one repair
// — contrary to step 2, and holding an extent and a connection per cntlr for a
// spare no failure asked for.
//
// A spare whose leg keeps reading ERROR is dead after leg_unhealthy — the wait
// AR8 already gives an active leg before it repairs one — and step 3 replaces
// it while the group has a free slot. The wait holds only this group
// (tryLegRepair). A spare that never gets a leg err_epoch at all — never
// provisioned, or never reported — has no such bound and stays pending.
func pendingSpare(p *spPass, grp *pb.Group) *pb.Leg {
	for _, spare := range sortedLegs(grp.GetSpareLegList()) {
		if reached(p.now, spare.GetErrEpoch(), p.th.GetLegUnhealthy()) {
			continue
		}
		side := singleSide(spare)
		if side == nil || side.GetErrEpoch() != 0 {
			continue
		}
		if spareReady(spare, p.info) {
			continue
		}
		return spare
	}
	return nil
}

// switchSpare runs AR8 step 1, through the clone-keyed op on a clone meta
// group.
func (w *spWorker) switchSpare(
	ctx context.Context,
	target *repairTarget,
	spare *pb.Leg,
	ids []slog.Attr,
) bool {
	ids = withAttr(ids, slog.Uint64("spare_leg_id", spare.GetLegId()))
	var err error
	if c := target.clone; c != nil {
		err = w.reactor().ops.switchCloneSpareLeg(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			c.name, c.id, target.grp.GetGrpId(),
			spare.GetLegId(), target.leg.GetLegId(),
		)
	} else {
		err = w.reactor().ops.switchSpareLeg(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			target.sliceId, target.grp.GetGrpId(),
			spare.GetLegId(), target.leg.GetLegId(),
		)
	}
	if err != nil {
		w.reactionFailed(ctx, reactionSpareSwitch, err, ids...)
		return true
	}
	w.reactionApplied(ctx, reactionSpareSwitch, ids...)
	return true
}

// createSpare runs AR8 step 3: a spare on a FRESH DN, black-listing the DNs of
// every leg and every spare of the group and, as the tier 1 of architecture.md,
// Per-operation allocation, excluding their LOCATIONS too, so the replacement
// does not share the failure domain it exists to replace. Tier 2 — model's
// rescan without the location exclusion when tier 1 yields fewer than the ONE
// DN this step places — is what still repairs a group in a cluster with no
// second domain to offer.
//
// The black list is also the plan the op is handed: model.CreateSpareLeg
// refuses the pick as `candidate changed` when the group holds a DN outside
// it — one that joined after this pass's snapshot, whose domain the scan
// could not exclude — or when the pick itself is gone, disabled or short of
// extents since the scan (MD6). Like every ErrPrecondition, that ends the pass
// with `reaction skipped` (AR2), and the next pass plans from a snapshot that
// holds what changed. On a clone meta group the scan is the same, with the
// group's ext_cnt, and the create is the clone-keyed op.
func (w *spWorker) createSpare(
	ctx context.Context,
	p *spPass,
	target *repairTarget,
	ids []slog.Attr,
) bool {
	batch := int(p.cc.GetAllocConf().GetDnBatchSize())
	plan := grpAddrs(target.grp)
	cands, err := w.reactor().ops.findDnCandidates(
		ctx, w.cid, p.cc, target.grp.GetExtCnt(), batch, 1,
		plan, grpLocations(p.state, target.grp),
	)
	if err != nil {
		w.reactionFailed(ctx, reactionSpareCreate, err, ids...)
		return true
	}
	picks := model.PickRandom(cands, 1)
	if len(picks) == 0 {
		w.reactionSkipped(ctx, reactionSpareCreate, reasonNoCandidate, ids...)
		return true
	}
	var legId uint64
	if c := target.clone; c != nil {
		legId, err = w.reactor().ops.createCloneSpareLeg(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			c.name, c.id, target.grp.GetGrpId(), picks[0], plan, p.cc,
		)
	} else {
		legId, err = w.reactor().ops.createSpareLeg(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			target.sliceId, target.grp.GetGrpId(), picks[0], plan, p.cc,
		)
	}
	if err != nil {
		w.reactionFailed(ctx, reactionSpareCreate, err, ids...)
		return true
	}
	w.reactionApplied(ctx, reactionSpareCreate,
		withAttr(ids,
			slog.Uint64("spare_leg_id", legId),
			slog.String("addr_port", picks[0].AddrPort),
		)...,
	)
	return true
}

// grpLocations is AR8's tier-1 exclusion (architecture.md, Per-operation
// allocation): the failure domain of every DN grpAddrs names, resolved through
// the pass's OWN snapshot of the node records — MD3 reads one DnConf per side
// addr_port of the SP, spare legs included, in the same transaction as the
// slices — so the locations come out of the state the pass already decided on
// and cost no further read. A DN missing from that snapshot contributes no
// location; it is black-listed by address anyway.
//
// With the default `location = addr_port` this excludes exactly the
// black-listed DNs' own domains.
func grpLocations(state *model.SpState, grp *pb.Group) []string {
	var locs []string
	for _, addrPort := range grpAddrs(grp) {
		if dn, ok := state.DnByAddr[addrPort]; ok {
			locs = append(locs, dn.GetLocation())
		}
	}
	return locs
}

// grpAddrs is AR8's black list: the DN of every side of every leg and every
// spare of the group. It is also the plan createSpare hands
// model.CreateSpareLeg, which walks the same sides of the group it reads.
func grpAddrs(grp *pb.Group) []string {
	var addrs []string
	for _, legList := range [][]*pb.Leg{
		grp.GetLegList(), grp.GetSpareLegList(),
	} {
		for _, leg := range legList {
			for _, side := range leg.GetSideList() {
				addrs = append(addrs, side.GetAddrPort())
			}
		}
	}
	return addrs
}

// ---------------------------------------------------------------------------
// The pass steps of a repair (dnv-worker.md, Repairs)
// ---------------------------------------------------------------------------

// repairIds are the attributes that name a repair in the records of its
// stage moves: its name, its rep_id and its group.
func repairIds(repName string, rep *pb.Repair) []slog.Attr {
	return []slog.Attr{
		slog.String("rep_name", repName),
		slog.Uint64("rep_id", rep.GetRepId()),
		slog.Uint64("grp_id", rep.GetGrpId()),
	}
}

// moveRepairStages is the stage move of architecture.md, Repair: each
// CREATED repair, in repair_name_list order, whose rep_id keys a verdict in
// the current primary's latest report (rep_id_to_repair_info) moves to
// STARTED when that verdict says started, whatever the abort flag says, and
// otherwise to ABORTED when it says abort confirmed and the snapshot's
// record has its flag set. Reason: the primary may add the new leg in the
// round the flag was set, so the flag alone cannot tell which side of that
// race the repair is on, and the verdict can; and the rep_id key ties the
// verdict to one record, so a stale verdict of a repair finished or
// cancelled and created again under the name moves nothing.
// model.SetRepairStage checks the stage, the rep_id and the flag again in its
// STM. The repair of a deleting clone's group, or of a clone the snapshot
// does not hold, is skipped: the clone drain's final transaction gives it
// back (CLD9). Each move logs `repair stage moved`; each verdict on a
// CREATED repair that such a clone or a clear abort flag keeps from moving,
// and each op that refuses or fails, logs `repair stage skipped` with its
// reason; a verdict on a repair past CREATED, one that says neither, or one
// under a rep_id no repair carries moves nothing and logs nothing. No move is the pass's one action (AR2), and a move's
// SpRev bump re-fans the pool (RW14).
func (w *spWorker) moveRepairStages(ctx context.Context, p *spPass) {
	verdicts := p.info.GetRepIdToRepairInfo()
	if len(verdicts) == 0 {
		return
	}
	for _, repName := range p.state.Conf.GetRepairNameList() {
		rep, ok := p.state.Repairs[repName]
		if !ok || rep.GetRepStage() != pb.RepStage_REP_STAGE_CREATED {
			continue
		}
		verdict, ok := verdicts[rep.GetRepId()]
		if !ok {
			continue
		}
		var stage pb.RepStage
		switch {
		case verdict.GetStarted():
			stage = pb.RepStage_REP_STAGE_STARTED
		case verdict.GetAbortConfirmed():
			stage = pb.RepStage_REP_STAGE_ABORTED
		default:
			continue
		}
		ids := withAttr(repairIds(repName, rep),
			slog.String("stage", stage.String()),
			slog.Uint64("cntlr_id", p.primaryId),
			slog.Uint64("report_revision", p.infoRev),
		)
		if repairCloneDeleting(p.state, rep) {
			w.repairStageSkipped(ctx, reasonCloneDeleting, ids...)
			continue
		}
		if stage == pb.RepStage_REP_STAGE_ABORTED && !rep.GetTryToAbort() {
			w.repairStageSkipped(ctx, reasonAbortFlagClear, ids...)
			continue
		}
		err := w.reactor().ops.setRepairStage(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			repName, rep.GetRepId(), stage,
		)
		if err != nil {
			w.stepFailed(ctx, msgRepairStageSkipped, err, ids...)
			continue
		}
		w.stepApplied(ctx, msgRepairStageMoved, ids...)
	}
}

// repairCloneDeleting reports whether a repair's group is a clone meta group
// whose clone is being deleted or is missing from the snapshot, which
// every repair op refuses (MD6).
func repairCloneDeleting(state *model.SpState, rep *pb.Repair) bool {
	cloneName := rep.GetCloneName()
	if cloneName == "" {
		return false
	}
	clone, ok := state.Clones[cloneName]
	return !ok || clone.GetDeleting()
}

// retireOldLeg is the retire timer (AR12), a step of the pass where
// reactions run (AR3): at most one retired leg a pass, the oldest first by
// retired_epoch, then by leg id, is deleted (model.DeleteRetiredLeg) when it
// is at least common.RetiredLegMaxAge old, whoever's repair retired it, and
// retireHold finds nothing to keep it. Its delete carries the
// snapshot's SpRevision as its expected revision, so a pool changed since the
// snapshot that judged the leg refuses it and the next pass judges again. A
// leg younger than the age is passed over silently; an old one that a hold
// keeps logs `retired leg kept` with the hold's reason on every pass. The
// delete is not the pass's one action (AR2), and the data of the leg it
// deletes is held twice elsewhere, so it is no reaction (AR9).
func (w *spWorker) retireOldLeg(ctx context.Context, p *spPass) {
	legIds := sortedIds(p.state.Conf.GetRetiredLegIdList())
	sort.SliceStable(legIds, func(i, j int) bool {
		return p.state.RetiredLegs[legIds[i]].GetRetiredEpoch() <
			p.state.RetiredLegs[legIds[j]].GetRetiredEpoch()
	})
	for _, legId := range legIds {
		retired, ok := p.state.RetiredLegs[legId]
		if !ok || !reached(
			p.now, retired.GetRetiredEpoch(), common.RetiredLegMaxAge,
		) {
			continue
		}
		ids := []slog.Attr{
			slog.Uint64("leg_id", legId),
			slog.Uint64("grp_id", retired.GetGrpId()),
			slog.Uint64("retired_epoch", retired.GetRetiredEpoch()),
		}
		if reason := retireHold(p, retired); reason != "" {
			w.stepSkipped(ctx, msgRetiredLegKept, reason, ids...)
			continue
		}
		err := w.reactor().ops.deleteRetiredLeg(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			p.state.SpRevision, legId, p.cc,
		)
		if err != nil {
			w.stepFailed(ctx, msgRetiredLegKept, err, ids...)
			return
		}
		w.stepApplied(ctx, msgRetiredLegDeleted, ids...)
		return
	}
}

// retireHold is the retire timer's test of one old retired leg: "" when it
// may be deleted, else the reason it is kept. Reason (AR12): its data
// exists twice elsewhere only while the group is whole, so a group waiting
// for its legs keeps its retired copies; md learns that a member is dead
// only from IO, so an idle array over a dead leg still reads whole, while
// the legs' and sides' err_epochs come from probes that run without IO; and
// a row turns UNKNOWN when the primary's stream dies, while a plain field
// such as the whole flag does not. So the leg is kept while
// its group is not found or is a deleting clone's, which DeleteRetiredLeg
// refuses; while the group has a repair (reasonGrpHasRepair); while the
// group's row in the current primary's latest report reads anything but OK,
// or that report is of an observation older than the snapshot, since it
// then describes the group from before a change to its legs (AR8 step 1's
// gate, grpRowHold, grp_row_stale included); while that report does not set
// the group's whole flag; and while a leg of the group's leg_list or its
// side has an err_epoch.
func retireHold(p *spPass, retired *pb.RetiredLeg) string {
	var grp *pb.Group
	switch cloneName := retired.GetCloneName(); {
	case cloneName != "":
		clone, ok := p.state.Clones[cloneName]
		if !ok || clone.GetMetaGrp().GetGrpId() != retired.GetGrpId() {
			return reasonGrpNotFound
		}
		if clone.GetDeleting() {
			return reasonCloneDeleting
		}
		grp = clone.GetMetaGrp()
	default:
		grp = findSliceGroup(p.state, retired.GetSliceId(), retired.GetGrpId())
		if grp == nil {
			return reasonGrpNotFound
		}
	}
	if grp.GetRepName() != "" {
		return reasonGrpHasRepair
	}
	grpId := grp.GetGrpId()
	if reason := grpRowHold(p, grpId); reason != "" {
		return reason
	}
	if !p.info.GetGrpIdToGrpInfo()[grpId].GetWhole() {
		return reasonGrpNotWhole
	}
	for _, leg := range grp.GetLegList() {
		if leg.GetErrEpoch() != 0 {
			return reasonGrpLegUnhealthy
		}
		for _, side := range leg.GetSideList() {
			if side.GetErrEpoch() != 0 {
				return reasonGrpLegUnhealthy
			}
		}
	}
	return ""
}

// findSliceGroup is the group grpId of the slice sliceId in one snapshot,
// or nil.
func findSliceGroup(
	state *model.SpState,
	sliceId uint64,
	grpId uint64,
) *pb.Group {
	slice, ok := state.Slices[sliceId]
	if !ok {
		return nil
	}
	for _, grp := range allGroups(slice) {
		if grp.GetGrpId() == grpId {
			return grp
		}
	}
	return nil
}

// stepApplied logs the record of a pass step that wrote (msg is
// msgRepairStageMoved or msgRetiredLegDeleted), with the SpRev its op
// bumped, read back as reactionApplied reads it.
func (w *spWorker) stepApplied(
	ctx context.Context,
	msg string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	attrs = append(attrs, slog.Uint64("revision", w.currentRevision(ctx)))
	slog.InfoContext(ctx, msg, attrs...)
}

// stepSkipped logs the record of a pass step that did not write (msg is
// msgRepairStageSkipped or msgRetiredLegKept), with its reason.
func (w *spWorker) stepSkipped(
	ctx context.Context,
	msg string,
	reason string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("reason", reason),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	slog.InfoContext(ctx, msg, attrs...)
}

// repairStageSkipped is stepSkipped for a stage move.
func (w *spWorker) repairStageSkipped(
	ctx context.Context,
	reason string,
	ids ...slog.Attr,
) {
	w.stepSkipped(ctx, msgRepairStageSkipped, reason, ids...)
}

// stepFailed turns one op error of a pass step into its skip record, as
// reactionFailed does for a reaction: a model.ErrPrecondition contributes
// its Reason and the op that refused, anything else reasonOpFailed and the
// error text.
func (w *spWorker) stepFailed(
	ctx context.Context,
	msg string,
	err error,
	ids ...slog.Attr,
) {
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		w.stepSkipped(ctx, msg, pre.Reason,
			withAttr(ids, slog.String("op", pre.Op))...,
		)
		return
	}
	w.stepSkipped(ctx, msg, reasonOpFailed,
		withAttr(ids, slog.String("error", err.Error()))...,
	)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// reached is AR4's threshold comparison `now − err_epoch ≥ T`, written so that
// it can never underflow: a zero epoch is healthy, and a clock that has gone
// backwards has simply not reached the threshold yet. It is the worker-side
// twin of model's, which re-applies it inside every op's STM.
func reached(now uint64, errEpoch uint64, threshold uint32) bool {
	if errEpoch == 0 || now < errEpoch {
		return false
	}
	return now-errEpoch >= uint64(threshold)
}

// isMdRaid1 reports whether the SP's groups are md-raid1 groups (AR8).
// Redundancy is an SP-wide property; a group only inherits it.
func isMdRaid1(conf *pb.SpConf) bool {
	return conf.GetBdevConf().GetRedundConf().GetRedundMdRaid1() != nil
}

// legCnt is how many legs one new group of the SP has (AR6): 2 for
// RedundMdRaid1, 1 for RedundNone. The md-raid1 arm cites
// common.MaxAllocLegPerGrp for gateway/alloc.go legCntOf's reason (SPD1).
func legCnt(conf *pb.SpConf) int {
	if isMdRaid1(conf) {
		return common.MaxAllocLegPerGrp
	}
	return 1
}

// allGroups is a slice's meta groups followed by its data groups.
func allGroups(slice *pb.Slice) []*pb.Group {
	grps := make([]*pb.Group, 0,
		len(slice.GetMetaGrpList())+len(slice.GetDataGrpList()))
	grps = append(grps, slice.GetMetaGrpList()...)
	return append(grps, slice.GetDataGrpList()...)
}

// singleSide returns a leg's one side, or nil when it does not have exactly
// one (AR8: a two-sided leg is a migration in flight).
func singleSide(leg *pb.Leg) *pb.Side {
	if len(leg.GetSideList()) != 1 {
		return nil
	}
	return leg.GetSideList()[0]
}

// sortedLegs returns a leg list in ascending leg_id order, leaving the stored
// list — whose ORDER is the md member order (architecture.md, Spare legs) —
// untouched.
func sortedLegs(legList []*pb.Leg) []*pb.Leg {
	sorted := append([]*pb.Leg(nil), legList...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].GetLegId() < sorted[j].GetLegId()
	})
	return sorted
}

// sortedIds returns a copy of an id list in ascending order, so "the smallest
// id" is well defined however the list happens to be stored.
func sortedIds(ids []uint64) []uint64 {
	sorted := append([]uint64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}

// withAttr returns ids plus extra as a NEW slice, so a caller that hands the
// same id list to two records cannot have the first one overwrite the second's
// backing array.
func withAttr(ids []slog.Attr, extra ...slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(ids)+len(extra))
	out = append(out, ids...)
	return append(out, extra...)
}

// candAddrs are the endpoints of a pick set, for the `reaction applied`
// record.
func candAddrs(cands []model.Cand) []string {
	addrs := make([]string, 0, len(cands))
	for _, cand := range cands {
		addrs = append(addrs, cand.AddrPort)
	}
	return addrs
}

// pickDistinct draws n candidates at random with a GROWING black list (AR6),
// so the legs of one new group land on distinct DNs. The scan already returns
// one candidate per node and per location, so the black list only has to hold
// what has been drawn; it returns fewer than n when the scan did.
func pickDistinct(cands []model.Cand, n int) []model.Cand {
	picked := make([]model.Cand, 0, n)
	remaining := append([]model.Cand(nil), cands...)
	for len(picked) < n && len(remaining) > 0 {
		one := model.PickRandom(remaining, 1)
		if len(one) == 0 {
			return picked
		}
		pick := one[0]
		picked = append(picked, pick)
		kept := remaining[:0]
		for _, cand := range remaining {
			if cand.AddrPort != pick.AddrPort {
				kept = append(kept, cand)
			}
		}
		remaining = kept
	}
	return picked
}

// ---------------------------------------------------------------------------
// The records of a pass (AR2; dnv-worker.md, Log records)
// ---------------------------------------------------------------------------

// reactionApplied logs the `reaction applied` record (dnv-worker.md, Log
// records). The revision is the SpRev the op's STM bumped, read back the way
// the created flip reads it (RW19): the ops report their new ids, not the
// revision, and a concurrent bump would make this report a slightly newer one
// — a log detail, not a decision.
func (w *spWorker) reactionApplied(
	ctx context.Context,
	kind string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("kind", kind),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	attrs = append(attrs, slog.Uint64("revision", w.currentRevision(ctx)))
	slog.InfoContext(ctx, msgReactionApplied, attrs...)
}

// reactionSkipped logs the `reaction skipped` record (dnv-worker.md, Log
// records): the reaction was applicable but did not run.
func (w *spWorker) reactionSkipped(
	ctx context.Context,
	kind string,
	reason string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("kind", kind),
		slog.String("reason", reason),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	slog.InfoContext(ctx, msgReactionSkipped, attrs...)
}

// reactionFailed turns one op error into a `reaction skipped` record (AR2): a
// model.ErrPrecondition contributes the precondition that did not hold, which
// is not a failure at all — the next pass re-evaluates — and the op that
// refused, whose name tells its refusal, which ends the pass, from the same
// reason found by the pass's own check; anything else is a transient failure
// that additionally carries the error text.
func (w *spWorker) reactionFailed(
	ctx context.Context,
	kind string,
	err error,
	ids ...slog.Attr,
) {
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		w.reactionSkipped(ctx, kind, pre.Reason,
			withAttr(ids, slog.String("op", pre.Op))...,
		)
		return
	}
	w.reactionSkipped(ctx, kind, reasonOpFailed,
		withAttr(ids, slog.String("error", err.Error()))...,
	)
}
