package gateway

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.12 (gateway.md §5.11): the three RPCs that
// give an md-raid1 group its standby capacity. A spare leg is a Leg in the
// group's spare_leg_list — every cntlr connects to its side and health-checks
// it (§3.3 step 1), but md never sees the leg until SwitchSpareLeg moves it
// into leg_list, so a spare is pre-connected capacity and nothing more.
//
// All three requests address a group by grp_id and carry NO slice id, so each
// handler must first find the slice that holds the group. CreateSpareLeg and
// SwitchSpareLeg locate it in a plain snapshot (§5.11) that plans but does
// not decide — everything it read is read again inside the model op's own
// STM; DeleteSpareLeg has no snapshot at all — its locate runs directly
// inside the gateway's one deciding transaction. Either way a group that
// moved, a token that went stale or a spare list that filled up between plan
// and decision is caught where it matters.
//
// Two of the three are model ops the sp-worker's §10.4 leg repair already
// drives (§0 #4: reused, never duplicated); DeleteSpareLeg is the gateway's
// own STM, because no worker reaction ever removes a spare — a parked leg
// stays parked until an operator frees the slot (§0 item 17).

// The op names the model helpers put into their error messages and the bump
// helpers cite; they are the RPC names so a log line names something
// greppable.
const (
	opCreateSpareLeg = "CreateSpareLeg"
	opDeleteSpareLeg = "DeleteSpareLeg"
	opSwitchSpareLeg = "SwitchSpareLeg"
)

// openGrpForSpareLeg is the opening the three spare-leg RPCs share: resolve
// the cluster and the SP, check the token, then locate the group by id across
// every slice of the SP (GW5 → GW6, in that order).
//
// The token is checked by openSp BEFORE the group is looked up, so a client
// that sent a stale one always sees ABORTED "stale revision" and never a
// NOT_FOUND computed against a slice list it has not read. tok is the token
// MESSAGE: GW6 is presence-based, so a nil tok is a client that deliberately
// sent none and openSp lets it through unchecked (§0 #7). That same absence
// then reaches model.checkSpRev as expectRev 0, which reads 0 as "skip the
// check entirely" (gateway.md §2.2 #3) — the two layers agree, because a live
// SpRev starts at 1 and a token that is merely present-with-0 has already been
// refused above.
func openGrpForSpareLeg(
	stm etcdutil.STM,
	clusterName string,
	spName string,
	tok *pb.SpRev,
	grpId uint64,
) (*spScope, sliceLocation, error) {
	sc, err := openSp(stm, clusterName, spName, tok)
	if err != nil {
		return nil, sliceLocation{}, err
	}
	slices, err := loadSlices(stm, sc.Cid, sc.Conf)
	if err != nil {
		return nil, sliceLocation{}, err
	}
	loc, ok := findGrp(sc.Conf, slices, grpId)
	if !ok {
		return nil, sliceLocation{}, errNotFound(
			"group %d not found in storage pool %q", grpId, spName)
	}
	return sc, loc, nil
}

// snapshotGrpForSpareLeg runs openGrpForSpareLeg in a read-only snapshot: the
// plain pre-read the two model-backed RPCs plan from (§5.11, §5.8). Every
// read is served at one store revision, so the SpConf, the slice list and the
// group agree with each other — a plan assembled from a torn read would pick
// a DN for a group that no longer has that size.
//
// It is deliberately not the deciding read: the model op re-resolves the SP,
// re-reads the slice and re-checks the token inside its own STM, which is
// AG4's "nothing from phase 1 is trusted" applied to a pre-read.
func (s *Server) snapshotGrpForSpareLeg(
	ctx context.Context,
	clusterName string,
	spName string,
	tok *pb.SpRev,
	grpId uint64,
) (*spScope, sliceLocation, error) {
	var sc *spScope
	var loc sliceLocation
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		var err error
		sc, loc, err = openGrpForSpareLeg(
			stm, clusterName, spName, tok, grpId)
		return err
	})
	if err != nil {
		return nil, sliceLocation{}, mapStmErr(err)
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

// CreateSpareLeg is architecture.md §8.12's CreateSpareLeg: one more Leg in
// the group's spare_leg_list, carrying one Side on a DN the group does not
// already occupy and, whenever the cluster has one to offer, in a failure
// domain it does not already occupy either — a spare in the domain of the leg
// it exists to replace dies with it (§6.5's two tiers: the domain exclusion
// yields rather than refuse the spare altogether).
//
// It is a GW9 candidate unit around model.CreateSpareLeg (§0 #4): the DN scan
// is a range query and therefore runs outside every transaction, and the pick
// is re-validated against its exact capacity key inside the op's STM. A pick
// whose key moved comes back as errCandidateChanged through mapModelErr and
// re-runs the whole unit, scan included — re-running only the op would
// re-validate the same dead pick for ever.
//
// The three refusals below are checked twice on purpose: the model op applies
// them again inside its STM, where they are authoritative, but a group that
// is RedundNone or already full would otherwise pay for a candidate scan
// whose result the op can only throw away. The pre-check is also what gives
// them §8.12's codes — INVALID_ARGUMENT and RESOURCE_EXHAUSTED — since model
// raises every one of its own preconditions as FAILED_PRECONDITION.
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
	sc, loc, err := s.snapshotGrpForSpareLeg(
		ctx, req.GetClusterName(), req.GetSpName(),
		req.GetSpRev(), req.GetGrpId(),
	)
	if err != nil {
		return nil, err
	}
	// Redundancy is an SP-wide property the group only inherits (§8.4), so
	// the test is the SP's STORED bdev_conf — the merge CreateStoragePool
	// wrote, never a re-resolution against ClusterConf, whose redund_conf may
	// have been meant for a different SP.
	if !isMdRaid1(spBdevConf(sc.Conf)) {
		return nil, errInvalid(
			"group %d is RedundNone: there is no redundancy to repair",
			req.GetGrpId())
	}
	if len(loc.Grp.GetSpareLegList()) >= common.MaxSpareLegPerGrp {
		return nil, errExhausted(
			"group %d has %d spare legs, the maximum is %d",
			req.GetGrpId(), len(loc.Grp.GetSpareLegList()),
			common.MaxSpareLegPerGrp)
	}
	// The spare joins an existing Group and inherits its geometry unchanged
	// (§8.12: no §3.6 layout is computed), so the candidate must have room
	// for exactly one more copy of the group's ext_cnt.
	extCnt := loc.Grp.GetExtCnt()
	what := fmt.Sprintf("spare leg for group %d", req.GetGrpId())
	// §6.5 tier 1: the group's failure DOMAINS, not merely its DNs. Read once
	// for the whole candidate unit, outside every transaction, because a
	// location is immutable in v1 (§8.2).
	excludeLocs, err := grpDnLocations(ctx, s.cli, sc.Cid, loc.Grp)
	if err != nil {
		return nil, err
	}
	var legId uint64
	err = candidateUnit(ctx, func() error {
		legId = 0
		picks, err := pickDns(
			ctx, s.cli, sc.Cid, sc.Cc,
			dnPickPlan{ExtCnt: extCnt, Legs: 1, ExcludeLocs: excludeLocs},
			req.GetDnSelector(), grpDnAddrs(loc.Grp), what,
		)
		if err != nil {
			return err
		}
		// The Side the op writes is provisioned = false ([D15]); only the
		// sp-worker flips it, after the dn agent has zeroed the whole side
		// (§9.4), which is why a fresh spare cannot be switched in at once.
		newId, err := model.CreateSpareLeg(
			ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
			req.GetSpRev().GetRevision(), loc.SliceId, req.GetGrpId(),
			picks[0], sc.Cc,
		)
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

// DeleteSpareLeg is architecture.md §8.12's DeleteSpareLeg: the spare leaves
// spare_leg_list and its side gives the DN back what it took.
//
// This is the gateway's own STM — model exports no op for it — and the whole
// RPC is that one transaction (GW8): resolve, token, group and spare leg by
// id, then the two writes that must land together, the Slice without the leg
// and the DN with its extents and its pointer back. A refusal returns before
// the first Put, so an unknown id leaves the store untouched.
//
// No CN and no cntlr is touched, exactly as model.CreateSpareLeg touches
// none: the footprint a cntlr's CN reserves for the SP is Σ ext_cnt over
// GROUPS (§8.4, §8.6), and a spare leg changes no group's ext_cnt. The bumps
// are therefore one SpRev (the slice changed) and one DnRev per DN the ledger
// touched (§5.5), the DnRev coming from the ledger's flush.
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
		// The side was charged the group's ext_cnt when it was created, so
		// that is what returns; the ledger reads each DN once and writes,
		// re-indexes and bumps it once however many sides a leg carries.
		ledger := newDnLedger(stm, sc.Cid, sc.Cc)
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
		stm.Put(model.SliceKey(sc.Cid, sc.SpId(), loc.SliceId), loc.Slice)
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

// SwitchSpareLeg is architecture.md §8.12's SwitchSpareLeg, the only way a
// spare becomes active: the spare takes the target's POSITION in leg_list —
// the md member slot the array is missing — and the target is parked in
// spare_leg_list, still connected and probed but never repaired again.
//
// The swap is model.SwitchSpareLeg, the same op the sp-worker's §10.4 leg
// repair calls (§0 #4), and its preconditions are this RPC's. The one that a
// caller meets in practice is §9.4's: the spare's side must be `provisioned`,
// because switching to a side that has not finished zeroing would put an
// unwritten member into the array. It surfaces as FAILED_PRECONDITION, which
// is the code §10.14 step 3 expects.
//
// The pre-read checks the two leg ids for membership as well as the group, so
// an id that is in neither list is §8.12's NOT_FOUND rather than the
// FAILED_PRECONDITION model would raise for it. It deliberately does NOT look
// at `provisioned`: that verdict belongs to the deciding STM, where it cannot
// be raced.
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
	if spareLegOf(loc.Grp, req.GetSpareLegId()) == nil {
		return nil, errNotFound(
			"spare leg %d not found in group %d",
			req.GetSpareLegId(), req.GetGrpId())
	}
	if activeLegOf(loc.Grp, req.GetTargetLegId()) == nil {
		return nil, errNotFound(
			"active leg %d not found in group %d",
			req.GetTargetLegId(), req.GetGrpId())
	}
	err = model.SwitchSpareLeg(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), loc.SliceId, req.GetGrpId(),
		req.GetSpareLegId(), req.GetTargetLegId(),
	)
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
