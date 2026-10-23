package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Spare legs (gateway.md, Spare legs): the three
// RPCs that give an md-raid1 group its standby capacity. A spare leg is a Leg
// in the group's spare_leg_list. Every cntlr connects to its side and
// health-checks it (architecture.md, Primary cntlr, step 1), on a slice's
// group and on a clone meta group alike, but md never sees the leg until
// SwitchSpareLeg moves it into leg_list, so a spare is pre-connected capacity
// and nothing more.
//
// All three requests address a group by grp_id and carry NO slice id, so each
// handler must first find what holds the group: a slice, or, when no slice
// does, a clone, whose clone meta group is a group of the pool too
// (architecture.md, Terminology and object model). CreateSpareLeg and
// SwitchSpareLeg locate it in a plain snapshot (gateway.md, Spare legs) that
// plans but does not decide — everything it read is read again inside the
// model op's own STM, and CreateSpareLeg takes a fresh snapshot in every round
// of its candidate unit; DeleteSpareLeg has no snapshot at all — its locate
// runs directly inside the gateway's one deciding transaction. Either way a
// group that moved, a token that went stale or a spare list that filled up
// between plan and decision is caught where it matters.
//
// Two of the three are model ops (gateway.md, Scope and placement: reused,
// never duplicated), the ones the sp-worker's leg repair (dnv-worker.md AR8)
// already drives: on a slice's group the slice-keyed ops, and on a clone
// meta group their clone-keyed twins (dnv-worker.md MD6). DeleteSpareLeg is
// the gateway's own STM on either, because no worker reaction ever removes a
// spare — a parked leg stays parked until an operator frees the slot with
// DeleteSpareLeg (dnv-worker.md AR8).

// opDeleteSpareLeg is the op name DeleteSpareLeg's own STM gives its ledger
// flush and the bump helper; it is the RPC name so a log line names something
// greppable.
const opDeleteSpareLeg = "DeleteSpareLeg"

// openGrpForSpareLeg is the opening the three spare-leg RPCs share: resolve
// the cluster and the SP, check the token, then locate the group by id
// (locateGrp) across every slice of the SP (GW5 → GW6, in that order) and,
// only when no slice holds it, across the clones' clone meta groups, so a
// slice's group never depends on the clone keys. A clone being deleted
// refuses all three
// FAILED_PRECONDITION, as loadLiveClone refuses the clone RPCs that address
// one (dnv-worker.md CLD1): its group goes back to its DNs in the clone drain's
// final transaction, and a spare made, removed or switched in for it would
// change a group that is going away.
//
// The token is checked by openSp BEFORE the group is looked up, so a client
// that sent a stale one always sees ABORTED "stale revision" and never a
// NOT_FOUND computed against a slice list it has not read. tok is the token
// MESSAGE: GW6 is presence-based, so a nil tok is a client that deliberately
// sent none and openSp lets it through unchecked (GW6). That same absence
// then reaches model.checkSpRev as expectRev 0, which reads 0 as "skip the
// check entirely" (gateway.md GW6) — the two layers agree, because a live
// SpRev starts at 1 and a token that is merely present-with-0 has already been
// refused above.
func openGrpForSpareLeg(
	stm etcdutil.STM,
	clusterName string,
	spName string,
	tok *pb.SpRev,
	grpId uint64,
) (*spScope, grpLocation, error) {
	sc, err := openSp(stm, clusterName, spName, tok)
	if err != nil {
		return nil, grpLocation{}, err
	}
	loc, err := locateGrp(stm, sc, spName, grpId)
	if err != nil {
		return nil, grpLocation{}, err
	}
	if err := refuseDeletingClone(loc); err != nil {
		return nil, grpLocation{}, err
	}
	return sc, loc, nil
}

// snapshotGrpForSpareLeg runs openGrpForSpareLeg in a read-only snapshot: the
// plain pre-read the two model-backed RPCs plan from (gateway.md, Spare legs;
// architecture.md, STM discipline). Every read is served at one store
// revision, so the SpConf, the slice list or the clone and the
// group agree with each other — a plan assembled from a torn read would pick
// a DN for a group that no longer has that size.
//
// It is deliberately not the deciding read: the model op re-resolves the SP,
// re-reads the slice or the clone and re-checks the token inside its own
// STM, which is AG4's "nothing from phase 1 is trusted" applied to a
// pre-read.
func (s *Server) snapshotGrpForSpareLeg(
	ctx context.Context,
	clusterName string,
	spName string,
	tok *pb.SpRev,
	grpId uint64,
) (*spScope, grpLocation, error) {
	var sc *spScope
	var loc grpLocation
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		var err error
		sc, loc, err = openGrpForSpareLeg(
			stm, clusterName, spName, tok, grpId)
		return err
	})
	if err != nil {
		return nil, grpLocation{}, mapStmErr(err)
	}
	return sc, loc, nil
}

// spareLegOf is the group's spare leg with that id, or nil. Only
// spare_leg_list is searched, never the active list: DeleteSpareLeg and
// SwitchSpareLeg address the two lists by different arguments, and a
// group-wide search would let a mistyped id name an ACTIVE leg — the one leg
// that must never be released or promoted.
func spareLegOf(grp *pb.Group, legId uint64) *pb.Leg {
	for _, leg := range grp.GetSpareLegList() {
		if leg.GetLegId() == legId {
			return leg
		}
	}
	return nil
}

// activeLegOf is spareLegOf for the group's active leg_list, which is where
// SwitchSpareLeg's target_leg_id must be.
func activeLegOf(grp *pb.Group, legId uint64) *pb.Leg {
	for _, leg := range grp.GetLegList() {
		if leg.GetLegId() == legId {
			return leg
		}
	}
	return nil
}

// removeSpareLeg drops one leg from a spare list, keeping the order of the
// rest: the list order is what AR8's "ready spare with the smallest leg_id"
// scan and every reply that echoes the list read, so a delete must not
// reshuffle the spares it leaves behind.
func removeSpareLeg(list []*pb.Leg, legId uint64) []*pb.Leg {
	kept := make([]*pb.Leg, 0, len(list))
	for _, leg := range list {
		if leg.GetLegId() == legId {
			continue
		}
		kept = append(kept, leg)
	}
	return kept
}

// CreateSpareLeg is the CreateSpareLeg of architecture.md, Spare legs: one
// more Leg in the group's spare_leg_list, carrying one Side on a DN the group
// does not already occupy and, whenever the cluster has one to offer, in a
// failure domain it does not already occupy either — a spare in the domain of
// the leg it exists to replace dies with it (the two tiers of architecture.md,
// Per-operation allocation: the domain exclusion
// yields rather than refuse the spare altogether).
//
// It is a GW9 candidate unit around model.CreateSpareLeg, or, on a clone
// meta group, its twin model.CreateCloneSpareLeg (gateway.md, Scope and
// placement), each round of which plans, scans and decides, as
// CreateMigration's does. The plan — the group as a fresh snapshot reads it,
// its DNs as the black list and their locations as tier 1 — is read again in
// every round: the op's STM refuses a round whose group gained a DN since the
// plan, and a plan read once for the whole unit would be refused that way in
// every round until the unit gave up. The DN scan is a range query and
// therefore runs outside every transaction, and the pick is re-validated
// inside the op's STM. The op answers ReasonCandidateChanged for a pick whose
// capacity key moved, for a pick deleted, disabled or otherwise no longer
// allocatable, or short of extents, since the scan, and for a group that
// gained a DN outside the plan, which it receives as grpDnAddrs; each comes
// back as errCandidateChanged through
// mapModelErr and re-runs the whole unit, plan and scan included —
// re-running only the op would re-validate the same dead pick for ever.
//
// The three refusals below the snapshot are checked twice on purpose: the
// model op applies them again inside its STM, where they are authoritative,
// but a group that is RedundNone, has a repair or is already full would
// otherwise pay for a candidate scan whose result the op can only throw
// away. The pre-check is also what gives two of them the codes of
// architecture.md, Spare legs — INVALID_ARGUMENT and RESOURCE_EXHAUSTED —
// since model raises every one of its own preconditions as
// FAILED_PRECONDITION, which is the code of the group with a repair: a spare
// leg and the repair's new leg would race for one place in the group's
// array, so a group has one of the two at a time. The full list is the one the
// op's STM can still meet after the pre-check passed, when another create
// fills it in between, and its reason, model.ReasonSpareListFull, is answered
// RESOURCE_EXHAUSTED as well (GW7).
func (s *Server) CreateSpareLeg(
	ctx context.Context,
	req *pb.CreateSpareLegRequest,
) (*pb.CreateSpareLegReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateNodeSelector(
		"dn_selector", req.GetDnSelector(),
	); err != nil {
		return nil, err
	}
	what := fmt.Sprintf("spare leg for group %d", req.GetGrpId())
	var legId uint64
	err := candidateUnit(ctx, func() error {
		legId = 0
		sc, loc, err := s.snapshotGrpForSpareLeg(
			ctx, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev(), req.GetGrpId(),
		)
		if err != nil {
			return err
		}
		// Redundancy is an SP-wide property the group only inherits
		// (architecture.md, Storage pools), so the test is the SP's STORED
		// bdev_conf — the merge CreateStoragePool
		// wrote, never a re-resolution against ClusterConf, whose
		// redund_conf may have been meant for a different SP.
		if !isMdRaid1(spBdevConf(sc.Conf)) {
			return errInvalid(
				"group %d is RedundNone: there is no redundancy to repair",
				req.GetGrpId())
		}
		if repName := loc.Grp.GetRepName(); repName != "" {
			return errPrecondition(
				"group %d has the repair %q: a spare leg and the repair "+
					"would race for one place in its array",
				req.GetGrpId(), repName)
		}
		if len(loc.Grp.GetSpareLegList()) >= common.MaxSpareLegPerGrp {
			return spareListFull(req.GetGrpId())
		}
		// The spare joins an existing Group and inherits its geometry
		// unchanged (architecture.md, Spare legs: no on-leg layout —
		// architecture.md, Group on-leg layout: meta region, data region,
		// health block — is computed), so the candidate must have room for
		// exactly one more copy of the group's ext_cnt.
		extCnt := loc.Grp.GetExtCnt()
		// The plan: every DN the group occupies, which the scan black-lists
		// and the op checks the group against, and their failure DOMAINS,
		// tier 1 of architecture.md, Per-operation allocation, read outside
		// every transaction because a location is immutable
		// (architecture.md, Disk nodes).
		// The group has no repair here, the refusal above, so its DN set is
		// its legs' and its spares'.
		planBlack := grpDnAddrs(loc.Grp, nil)
		excludeLocs, err := grpDnLocations(ctx, s.cli, sc.Cid, planBlack)
		if err != nil {
			return err
		}
		picks, err := pickDns(
			ctx, s.cli, sc.Cid, sc.Cc,
			dnPickPlan{ExtCnt: extCnt, Legs: 1, ExcludeLocs: excludeLocs},
			req.GetDnSelector(), planBlack, what,
		)
		if err != nil {
			return err
		}
		// The Side the op writes is provisioned = false ([D15]), and a
		// switch takes only a provisioned spare (architecture.md, Spare
		// legs), which is why a fresh spare cannot be switched in at once.
		// Only the sp-worker flips it, once the dn agent reports the side's
		// zeroing done, through the clone flip on a clone meta group
		// (architecture.md, Side provisioning protocol; dnv-worker.md RW18).
		var newId uint64
		if loc.Clone != nil {
			newId, err = model.CreateCloneSpareLeg(
				ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
				req.GetSpRev().GetRevision(), loc.Clone.CloneName,
				loc.Clone.Clone.GetCloneId(), req.GetGrpId(), picks[0],
				planBlack, sc.Cc,
			)
		} else {
			newId, err = model.CreateSpareLeg(
				ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
				req.GetSpRev().GetRevision(), loc.Slice.SliceId,
				req.GetGrpId(), picks[0], planBlack, sc.Cc,
			)
		}
		if isSpareListFull(err) {
			// The op's own full-list check: a create committed since the
			// snapshot filled the list. It is the same ceiling the
			// pre-check above answers, so it gets the same refusal (GW7).
			return spareListFull(req.GetGrpId())
		}
		if err != nil {
			return mapModelErr(err)
		}
		legId = newId
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &pb.CreateSpareLegReply{LegId: legId}, nil
}

// spareListFull is CreateSpareLeg's refusal of a group whose spare_leg_list
// already holds MaxSpareLegPerGrp spares (architecture.md, Spare legs). The
// pre-check and the mapping of the op's own check both return it, so the two
// give one refusal the same words.
func spareListFull(grpId uint64) error {
	return errExhausted(
		"group %d already has the maximum of %d spare legs",
		grpId, common.MaxSpareLegPerGrp)
}

// isSpareListFull reports whether err is model.CreateSpareLeg's refusal, or
// its clone twin's, of a group whose spare list is already full.
func isSpareListFull(err error) bool {
	var pre *model.ErrPrecondition
	return errors.As(err, &pre) && pre.Reason == model.ReasonSpareListFull
}

// DeleteSpareLeg is the DeleteSpareLeg of architecture.md, Spare legs: the
// spare leaves spare_leg_list and its side gives the DN back what it took.
//
// This is the gateway's own STM — model exports no op for it — and the whole
// RPC is that one transaction (GW8): resolve, token, group and spare leg by
// id, no migration running on that leg, then the two writes that must land
// together, the Slice — or, for a clone meta group, the Clone — without the
// leg and the DN with its extents and its pointer back. A refusal returns
// before the first Put, so an unknown id or a spare under migration leaves
// the store untouched.
//
// No CN and no cntlr is touched, exactly as model.CreateSpareLeg touches
// none: the footprint a cntlr's CN reserves for the SP is Σ ext_cnt over
// GROUPS (architecture.md, Storage pools; architecture.md, Cntlrs), and a
// spare leg changes no group's ext_cnt, nor does a clone meta group count in
// it. The bumps are therefore one SpRev (the slice or the clone changed) and
// one DnRev per DN the ledger touched (architecture.md,
// Revision keys and the sync fan-out), the DnRev coming from the ledger's
// flush.
func (s *Server) DeleteSpareLeg(
	ctx context.Context,
	req *pb.DeleteSpareLegRequest,
) (*pb.DeleteSpareLegReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		sc, loc, err := openGrpForSpareLeg(
			stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev(), req.GetGrpId(),
		)
		if err != nil {
			return err
		}
		spare := spareLegOf(loc.Grp, req.GetLegId())
		if spare == nil {
			return errNotFound(
				"spare leg %d not found in group %d",
				req.GetLegId(), req.GetGrpId())
		}
		// The side of a slice's spare can be a migration source
		// (architecture.md, Migrations, takes a side of any leg of a slice,
		// spare legs included), and the destination then hangs off this very
		// leg. Releasing both would leave the Migration naming sides in no leg,
		// which neither FinishMigration nor CancelMigration can end — and an SP
		// whose migr_name_list never empties can never be deleted. A spare of a
		// clone meta group never has a second side, since no migration moves
		// one, so on that path the check only keeps the two alike.
		if len(spare.GetSideList()) > 1 {
			return errMigrRunning(spare)
		}
		// The side was charged the group's ext_cnt when it was created, so
		// that is what returns; the ledger reads its DN once and writes,
		// re-indexes and bumps it once.
		ledger, err := newDnLedger(stm, sc.Cid, sc.Cc)
		if err != nil {
			return err
		}
		for _, side := range spare.GetSideList() {
			err := ledger.release(
				side.GetAddrPort(), sc.SpId(), side.GetSideId(),
				loc.Grp.GetExtCnt(),
			)
			if err != nil {
				return err
			}
		}
		loc.Grp.SpareLegList = removeSpareLeg(
			loc.Grp.GetSpareLegList(), req.GetLegId())
		if loc.Clone != nil {
			stm.Put(model.CloneKey(
				sc.Cid, sc.SpId(), loc.Clone.CloneName), loc.Clone.Clone)
		} else {
			stm.Put(model.SliceKey(
				sc.Cid, sc.SpId(), loc.Slice.SliceId), loc.Slice.Slice)
		}
		if err := ledger.flush(opDeleteSpareLeg); err != nil {
			return err
		}
		return bumpSp(stm, opDeleteSpareLeg, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteSpareLegReply{LegId: req.GetLegId()}, nil
}

// SwitchSpareLeg is the SwitchSpareLeg of architecture.md, Spare legs, the
// only way a spare becomes active: the spare takes the target's POSITION in
// leg_list and the target is parked in spare_leg_list, never repaired again.
// That position is the md member slot the array is missing, and the parked
// leg stays connected and probed.
//
// The swap is model.SwitchSpareLeg, the same op the sp-worker's leg repair
// (dnv-worker.md AR8) calls (gateway.md, Scope and
// placement), or, on a clone meta group, its twin model.SwitchCloneSpareLeg,
// and its preconditions are this RPC's. The one that a caller
// meets in practice is that of architecture.md, Spare legs: the spare's side
// must be `provisioned`, so a side still zeroing never becomes an md member
// (architecture.md, Side provisioning protocol). It surfaces as
// FAILED_PRECONDITION, which is the code the faults case of gateway.md,
// Integration test plan, expects (GW7).
//
// The pre-read checks the two leg ids for membership as well as the group, so
// an id that is in neither list is the NOT_FOUND of architecture.md, Spare
// legs, rather than the FAILED_PRECONDITION model would raise for it. It
// deliberately does NOT look at `provisioned`: that verdict belongs to the
// deciding STM, where it cannot be raced.
//
// It does refuse either leg while a migration is running on it — two sides,
// architecture.md, Migrations — with CreateMigration's own message: a
// migration holds its leg until Finish or Cancel ends it, so a migrating leg
// is neither promoted nor parked (dnv-worker.md AR8 leaves one alone for the
// same reason). That refusal is a pre-read too; model.SwitchSpareLeg re-checks
// both legs inside its STM, with its own reasons. That re-check is what
// refuses a switch when a migration of either leg started after the leg was
// read: the worker's own AR8 switch, which passes no token, and this one when
// the request carried none. In that race a request that carried a token is
// refused first, by the STM's token check (ABORTED "stale revision"), because
// CreateMigration bumped SpRev.
func (s *Server) SwitchSpareLeg(
	ctx context.Context,
	req *pb.SwitchSpareLegRequest,
) (*pb.SwitchSpareLegReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	sc, loc, err := s.snapshotGrpForSpareLeg(
		ctx, req.GetClusterName(), req.GetSpName(),
		req.GetSpRev(), req.GetGrpId(),
	)
	if err != nil {
		return nil, err
	}
	spare := spareLegOf(loc.Grp, req.GetSpareLegId())
	if spare == nil {
		return nil, errNotFound(
			"spare leg %d not found in group %d",
			req.GetSpareLegId(), req.GetGrpId())
	}
	target := activeLegOf(loc.Grp, req.GetTargetLegId())
	if target == nil {
		return nil, errNotFound(
			"active leg %d not found in group %d",
			req.GetTargetLegId(), req.GetGrpId())
	}
	for _, leg := range []*pb.Leg{spare, target} {
		if len(leg.GetSideList()) > 1 {
			return nil, errMigrRunning(leg)
		}
	}
	if loc.Clone != nil {
		err = model.SwitchCloneSpareLeg(
			ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
			req.GetSpRev().GetRevision(), loc.Clone.CloneName,
			loc.Clone.Clone.GetCloneId(), req.GetGrpId(),
			req.GetSpareLegId(), req.GetTargetLegId(),
		)
	} else {
		err = model.SwitchSpareLeg(
			ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
			req.GetSpRev().GetRevision(), loc.Slice.SliceId,
			req.GetGrpId(), req.GetSpareLegId(), req.GetTargetLegId(),
		)
	}
	if err != nil {
		return nil, mapModelErr(err)
	}
	// After the swap the spare holds the target's position in leg_list and
	// the target is the one parked in spare_leg_list, so the reply is the two
	// request ids exchanged — read back from the request, not from a second
	// query, because only the committed STM's view is authoritative and it
	// has already told us it committed.
	return &pb.SwitchSpareLegReply{
		CurrActiveLegId: req.GetSpareLegId(),
		CurrSpareLegId:  req.GetTargetLegId(),
	}, nil
}
