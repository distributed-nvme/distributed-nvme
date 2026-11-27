package gateway

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Migrations / gateway.md, Migrations: the five
// RPCs that move one leg's side from one disk node to another while the volume
// keeps serving (architecture.md, Migration).
//
// The whole feature rests on one temporary shape: between CreateMigration and
// Finish/Cancel the leg owns TWO sides, the source and the destination, both
// exporting the same NQN so every CN sees them as two paths of one multipath
// namespace. Every rule below follows from that — the destination must land on
// a disk node no leg of the group already uses and, whenever the cluster has
// one to offer, in a failure domain the group does not already occupy either
// (a destination in the domain the migration was meant to leave repairs
// nothing; the two tiers of architecture.md, Per-operation allocation: the
// domain exclusion yields rather than refuse the migration altogether), it
// must take a cntlid slot the source does not hold (otherwise the two
// controllers a CN aggregates could pick the same CNTLID and the kernel would
// refuse the second path, architecture.md, cntlid slots), and exactly one
// of the two sides survives: Finish keeps the destination, Cancel keeps the
// source.

// The op names the ledger flushes and the bump helpers cite; they are the RPC
// names so a log record names something greppable. GetMigration writes
// nothing and therefore needs none.
const (
	opCreateMigration = "CreateMigration"
	opFinishMigration = "FinishMigration"
	opCancelMigration = "CancelMigration"
)

// whatMigrDst leads CreateMigration's capacity refusals, the scan's (pickDns)
// and the deciding charge's (dnLedger.chargeUnits) alike, so both name the
// object that found no room the same way.
const whatMigrDst = "migration destination"

// migrMaxLegSideCnt is how many sides one leg may own: its own, plus the
// destination of at most ONE migration (architecture.md, Migrations). A leg
// already at this count is mid-migration, so CreateMigration refuses it — two
// concurrent migrations of one leg would give the CN three paths to reconcile
// and leave no unambiguous survivor for FinishMigration to keep.
const migrMaxLegSideCnt = 2

// migrDstCntlidSlot is the destination side's cntlid slot (GW18): the first
// entry of the SP's cntlid_slot_list that differs from the source side's.
//
// architecture.md, cntlid slots, makes this the one slot constraint a side
// has. Sides of different legs may share slots freely — their subsystem NQNs
// differ — but the two sides of ONE leg are aggregated by every CN, so they
// must occupy different CNTLID ranges. An SP whose list offers no second value
// cannot migrate this leg at all; that is a standing property of its own
// configuration, hence FAILED_PRECONDITION rather than a candidate problem.
func migrDstCntlidSlot(conf *pb.SpConf, srcSlot uint32) (uint32, error) {
	for _, slot := range conf.GetCntlidSlotList() {
		if slot != srcSlot {
			return slot, nil
		}
	}
	return 0, errPrecondition(
		"cntlid_slot_list holds no slot other than the source side's %d",
		srcSlot)
}

// migrSrcLocation locates a CreateMigration source side and asserts its leg
// can still take a destination (the errors of architecture.md, Migrations,
// on the source side: not found, a side of a clone meta group, a side of a
// repair's bad leg, a leg with two sides). It also returns the group's open
// repair, nil when it has none, whose new legs belong to the group's DN set
// (grpDnAddrs).
//
// The same walk runs twice — once to plan the allocation and once inside the
// deciding transaction — and shares one implementation so the two can never
// disagree about what they refuse. findSide covers every leg of a slice's
// groups, since architecture.md, Migrations, takes a side of any leg of a
// slice. It covers no repair's new leg and no retired leg, so their sides are
// not found: a new leg is no leg of its group before its finish, and a retired
// leg is no leg (architecture.md, Repairs, Retired legs). A side of a repair's
// bad leg is refused FAILED_PRECONDITION: the repair replaces that leg
// (architecture.md, Repair).
//
// A side of a clone's clone meta group is refused FAILED_PRECONDITION
// (architecture.md, Migrations): a clone is a copy in flight, moving a leg of
// its group would need a clone-keyed twin of the whole migration path, and a
// leg of an md-raid1 clone meta group is replaced by a repair instead
// (architecture.md, Clones). The clones are read only when no slice holds the
// side, so the migration of a slice's side never depends on the clone keys.
func migrSrcLocation(
	stm etcdutil.STM,
	sc *spScope,
	slices []*pb.Slice,
	spName string,
	srcSideId uint64,
) (sliceLocation, *pb.Repair, error) {
	loc, ok := findSide(sc.Conf, slices, srcSideId)
	if !ok {
		clones, err := loadClones(stm, sc.Cid, sc.Conf)
		if err != nil {
			return sliceLocation{}, nil, err
		}
		if cloneLoc, ok := findCloneSide(
			sc.Conf, clones, srcSideId,
		); ok {
			return sliceLocation{}, nil, errPrecondition(
				"side %d belongs to the clone meta group of clone %q, "+
					"which no migration moves", srcSideId, cloneLoc.CloneName)
		}
		return sliceLocation{}, nil, errNotFound(
			"side %d is not in any leg of storage pool %q", srcSideId, spName)
	}
	rep, err := grpRepair(stm, sc, loc.Grp)
	if err != nil {
		return sliceLocation{}, nil, err
	}
	if containsId(rep.GetBadLegIdList(), loc.Leg.GetLegId()) {
		return sliceLocation{}, nil, errPrecondition(
			"side %d belongs to leg %d, the bad leg of the repair %q, "+
				"which replaces it", srcSideId, loc.Leg.GetLegId(),
			loc.Grp.GetRepName())
	}
	if len(loc.Leg.GetSideList()) >= migrMaxLegSideCnt {
		return sliceLocation{}, nil, errMigrRunning(loc.Leg)
	}
	return loc, rep, nil
}

// migrMetaUnitsOf is the metadata units a migration destination's dm-clone
// run takes in its disk node's metadata area (architecture.md, Metadata
// units): common.CloneMetaUnits of the moved side's bytes — the group's
// ext_cnt times the cluster's extent_size — and the SP's stored
// data_block_size, the inputs the dn agent sizes the run by. Both stored
// confs are validated first and refused ABORTED (GW11): a zero extent size or
// block size read back out of the store would size the run at one unit.
// CreateMigration charges the count to the destination and stores it on the
// Migration, where FinishMigration and CancelMigration read what to give
// back, so a release returns exactly what the create charged.
func migrMetaUnitsOf(
	cc *pb.ClusterConf,
	conf *pb.SpConf,
	grp *pb.Group,
) (uint64, error) {
	if err := model.ValidateClusterConf(cc); err != nil {
		return 0, errAborted("%v", err)
	}
	bdev := spBdevConf(conf)
	if err := model.ValidateBdevConf(bdev); err != nil {
		return 0, errAborted("%v", err)
	}
	return common.CloneMetaUnits(
		grp.GetExtCnt()*cc.GetDnBinConf().GetExtentSize(),
		model.PoolBlockSize(bdev),
	), nil
}

// errMigrRunning is the FAILED_PRECONDITION for a leg that owns more than its
// own side: a migration is running on it (architecture.md, Migrations).
// CreateMigration raises it for a source leg that cannot take a second
// destination and CreateRepair for a bad leg a migration holds
// (architecture.md, Repairs), so the two RPCs name the refusal the same way.
func errMigrRunning(leg *pb.Leg) error {
	return errPrecondition(
		"leg %d already has %d sides: a migration is running on it",
		leg.GetLegId(), len(leg.GetSideList()))
}

// migrCutoverStarted is FinishMigration's refusal of a migration whose
// cutover has not started, read from the record alone (architecture.md,
// Migrations): while cutover_pending is set the source side still serves the
// leg and the destination holds no copied byte, so a finish would drop the
// side that serves for one that holds nothing. force does not lift it, since
// force skips only the hydration proof; a cutover that never comes is ended
// by CancelMigration, which the message names. Both phases run it: phase 1
// so the refusal comes before any agent call, the deciding STM because a
// record cancelled and created again under the same name between the phases
// is pending again.
func migrCutoverStarted(migrName string, migr *pb.Migration) error {
	if migr.GetCutoverPending() {
		return errPrecondition(
			"migration %q: the cutover has not started; "+
				"cancel the migration instead", migrName)
	}
	return nil
}

// migrCancelAllowed is CancelMigration's refusal (architecture.md,
// Migrations): once the migration's cutover has started, the destination may
// serve and hold the only copy of the writes since, which a cancel throws
// away, the source then serving its content as of the cutover; only force
// cancels then.
func migrCancelAllowed(
	migrName string,
	migr *pb.Migration,
	force bool,
) error {
	if migr.GetCutoverPending() || force {
		return nil
	}
	return errPrecondition(
		"migration %q: the cutover has started, so the new side may hold "+
			"the only copy of the writes since; finish the migration, or "+
			"cancel with force to serve the old side as of the cutover",
		migrName)
}

// migrCntBelowCeiling is CreateMigration's count ceiling: a pool whose
// migr_name_list already holds MaxMigrCntPerSp migrations starts no other
// (architecture.md, Migrations). The planning read and the deciding STM both
// run it, so the two give one refusal the same words.
func migrCntBelowCeiling(conf *pb.SpConf, spName string) error {
	if len(conf.GetMigrNameList()) >= common.MaxMigrCntPerSp {
		return errExhausted(
			"storage pool %q already has %d migrations, the maximum is %d",
			spName, len(conf.GetMigrNameList()), common.MaxMigrCntPerSp)
	}
	return nil
}

// migrPoolChecks is what CreateMigration answers from the pool's own record,
// in its order, behind the resolution and the token check openSp makes: the
// MaxMigrCntPerSp ceiling, ahead of the name so a pool at the ceiling hears
// the ceiling (GW9); the name free, so a re-send of a created migration
// hears ALREADY_EXISTS, unless that migration filled the pool to the
// ceiling, when the ceiling answers first; and the pool's block within the
// block's bound of architecture.md, Common validation, FAILED_PRECONDITION
// otherwise. A pool made before that bound keeps its block, and a migration
// of one of its sides would make a dm-clone whose region is that block,
// which reports at least one region as its optimal IO size: the controller
// node would send the new side commands its disk node refuses. No call
// moves a healthy leg of such a pool in place — an operator's repair
// replaces only a leg with a health epoch or one whose row the primary's
// report marks of another identity, and a RedundNone pool has none — so the
// message names the way out, a move of the pool's volumes into a new pool by
// a transfer and a clone (architecture.md, Common validation, the block's
// bound). The planning read and the deciding STM both run it, ahead of the
// source side, so a refusal reached in either has the same words and the
// planning read answers each ahead of the scan.
func migrPoolChecks(conf *pb.SpConf, spName string, migrName string) error {
	if err := migrCntBelowCeiling(conf, spName); err != nil {
		return err
	}
	if containsName(conf.GetMigrNameList(), migrName) {
		return errExists("migration %q already exists", migrName)
	}
	if block := model.PoolBlockSize(spBdevConf(conf)); block >
		common.MaxNvmeTcpCmdBytes {
		return errPrecondition(
			"storage pool %q has a data_block_size of %d, over %d, the most "+
				"data one NVMe/TCP command may carry: a migration copies in "+
				"regions of one block and reports at least one block as its "+
				"optimal IO size, so the controller node would send the new "+
				"side commands its disk node refuses; move the pool's "+
				"volumes to a new pool instead, by a transfer and a clone",
			spName, block, common.MaxNvmeTcpCmdBytes)
	}
	return nil
}

// getMigration reads one migration of the SP by name inside the caller's
// transaction; an absent record is NOT_FOUND (GW7).
//
// The record and its migr_name_list entry are written and deleted in the same
// transaction, so either one answers "does it exist". The four RPCs that
// address an existing migration read the record because they need its fields;
// CreateMigration tests the list instead, because that is also what the
// MaxMigrCntPerSp ceiling counts.
func getMigration(
	stm etcdutil.STM,
	sc *spScope,
	migrName string,
) (*pb.Migration, error) {
	migr := &pb.Migration{}
	if !stm.Get(model.MigrationKey(sc.Cid, sc.SpId(), migrName), migr) {
		return nil, errNotFound("migration %q not found", migrName)
	}
	return migr, nil
}

// CreateMigration is the CreateMigration of architecture.md, Migrations: it
// allocates one destination disk node and hangs a second, unprovisioned Side
// off the source side's leg.
//
// The destination is written `provisioned: false` ([D15]) and the Migration
// `cutover_pending: true`, and nothing else happens yet: the dst DN runs only
// the zeroing protocol (architecture.md, Side provisioning protocol), and
// `migr_src_conf.dst_provisioned = false` keeps the source serving untouched
// meanwhile (architecture.md, Migration, Phase 0). The sp-worker's later
// provisioned flip has the destination build its exports and its
// clone-metadata wrapper and the primary connect to it; only the sp-worker's
// cutover, which clears the flag, takes the source off the leg and starts the
// copy (architecture.md, Migration). That is why this RPC allocates and
// writes but starts no data movement.
//
// Shape (gateway.md, Migrations): the GW9 candidate unit, each round of which
// plans from plain pre-reads, scans and decides. The plan — the group's
// ext_cnt, the destination's metadata units and the black list — is read
// through a read-only snapshot so the SpConf and the slices it indexes are
// coherent; nothing from it is trusted, since the deciding STM re-locates the
// side and re-verifies both the pick's capacity key and the plan the pick was
// scanned against: the group, as that STM reads it, must occupy no DN the
// plan's read of it did not. The same read answers the MaxMigrCntPerSp
// ceiling, the name, the pool's block (migrPoolChecks), the source side and
// the slot ahead of the scan, and the deciding STM answers each again (GW9).
//
// The destination's dm-clone run takes room in its DN's metadata area
// (migrMetaUnitsOf; architecture.md, Metadata units). The scan skips every DN
// without that room, and a round with no DN left for that reason is refused
// RESOURCE_EXHAUSTED with "metadata room" in its message (pickDns). The
// deciding STM re-checks the room on the pick's record (dnLedger.verifyPick)
// — a DN that filled up after the scan sends the round back as candidate
// changed — and then charges the units with the extents, under the DN's one
// DnRev bump, and stores them on the Migration.
func (s *Server) CreateMigration(
	ctx context.Context,
	req *pb.CreateMigrationRequest,
) (*pb.CreateMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	if err := validateNodeSelector(
		"dn_selector", req.GetDnSelector(),
	); err != nil {
		return nil, err
	}
	if err := validateDmCloneConf(
		"dm_clone_conf", req.GetDmCloneConf(),
	); err != nil {
		return nil, err
	}
	var migrId uint64
	err := candidateUnit(ctx, func() error {
		// The plan: which cluster to scan, how big the destination must be in
		// extents and in metadata units, and which disk nodes — and failure
		// domains — it must avoid. grpDnAddrs names every DN the group already
		// occupies through a leg or its repair's new leg and seeds the black
		// list; grpDnLocations turns the same set into the tier-1 exclusion of
		// architecture.md, Per-operation allocation, so the destination leaves
		// the failure domain the migration is meant to leave, and a cluster
		// with no other domain still places through tier 2 rather than
		// refusing. It is read afresh every round, never once for the unit:
		// the deciding STM refuses a round whose group has gained a DN since
		// this read, and a plan read only once would be refused that way every
		// round until the unit gave up.
		var (
			planCid   uint64
			planCc    *pb.ClusterConf
			planExt   uint64
			planUnits uint64
			planGrp   *pb.Group
			planRep   *pb.Repair
			planHost  string
		)
		err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
			// openSp, not openSpRead: this is a MUTATOR's planning read, so
			// GW6 wants the token — when one was sent — checked before any
			// other state check: a stale client must see ABORTED "stale
			// revision", never a NOT_FOUND or a FAILED_PRECONDITION computed
			// against a slice list it has not read. The deciding STM checks
			// it again (AG4); this one only fixes which refusal a stale
			// caller is given.
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev())
			if err != nil {
				return err
			}
			// The count ceiling, the name and the block before the scan
			// (GW9), right behind the token: a pool with every migration it
			// may run in flight is told so, not that no DN is free, and a
			// re-send of a created migration hears ALREADY_EXISTS. The
			// deciding STM checks them again for a write committed since.
			if err := migrPoolChecks(
				sc.Conf, req.GetSpName(), req.GetMigrName(),
			); err != nil {
				return err
			}
			slices, err := loadSlices(stm, sc.Cid, sc.Conf)
			if err != nil {
				return err
			}
			loc, rep, err := migrSrcLocation(
				stm, sc, slices, req.GetSpName(), req.GetSrcSideId())
			if err != nil {
				return err
			}
			// The slot too: a pool whose slot list offers no second slot
			// starts no migration, whatever its disk nodes (GW18).
			if _, err := migrDstCntlidSlot(
				sc.Conf, loc.Side.GetCntlidSlot(),
			); err != nil {
				return err
			}
			units, err := migrMetaUnitsOf(sc.Cc, sc.Conf, loc.Grp)
			if err != nil {
				return err
			}
			planCid = sc.Cid
			planCc = sc.Cc
			planExt = loc.Grp.GetExtCnt()
			planUnits = units
			planGrp = loc.Grp
			planRep = rep
			// The old side's machine, which the destination never shares
			// (sameHostDns); a disk node record that is gone names none.
			srcDn := &pb.DnConf{}
			if stm.Get(model.DnConfKey(sc.Cid, loc.Side.GetAddrPort()),
				srcDn) {
				planHost = srcDn.GetHostName()
			}
			return nil
		})
		if err != nil {
			return mapStmErr(err)
		}
		planBlack := grpDnAddrs(planGrp, planRep)
		// Outside the snapshot, at no cost in coherence: a location cannot
		// change (architecture.md, Disk nodes).
		planLocs, err := grpDnLocations(ctx, s.cli, planCid, planBlack)
		if err != nil {
			return err
		}
		// Then the old side's machine joins the black list, but not the
		// tier-1 locations: its other disk nodes are no failure domain the
		// group occupies, they are simply never the destination.
		sameHost, err := sameHostDns(ctx, s.cli, planCid, planHost)
		if err != nil {
			return err
		}
		picks, err := pickDns(
			ctx, s.cli, planCid, planCc,
			dnPickPlan{
				ExtCnt:      planExt,
				Legs:        1,
				ExcludeLocs: planLocs,
				MetaUnits:   planUnits,
			},
			req.GetDnSelector(), append(planBlack, sameHost...),
			whatMigrDst,
		)
		if err != nil {
			if len(sameHost) > 1 && status.Code(err) ==
				codes.ResourceExhausted {
				return errExhausted("%s; the disk nodes of host %q, the "+
					"old side's machine, are passed over, since two disk "+
					"nodes of one machine never hold the two sides of one "+
					"leg", status.Convert(err).Message(), planHost)
			}
			return err
		}
		cand := picks[0]
		return s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			migrId = 0
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev())
			if err != nil {
				return err
			}
			if err := migrPoolChecks(
				sc.Conf, req.GetSpName(), req.GetMigrName(),
			); err != nil {
				return err
			}
			// The topology is re-verified inside the transaction, not
			// carried over from the plan: a side that moved between the two
			// reads is the plain NOT_FOUND / FAILED_PRECONDITION of
			// architecture.md, Migrations, again, never GW9's
			// candidate-changed retry — re-scanning disk
			// nodes would not bring a side back.
			slices, err := loadSlices(stm, sc.Cid, sc.Conf)
			if err != nil {
				return err
			}
			loc, rep, err := migrSrcLocation(
				stm, sc, slices, req.GetSpName(), req.GetSrcSideId())
			if err != nil {
				return err
			}
			slot, err := migrDstCntlidSlot(
				sc.Conf, loc.Side.GetCntlidSlot())
			if err != nil {
				return err
			}
			// The size charged is the one this transaction read, not the one
			// the scan was run for: it is what the destination must actually
			// hold, and a pick too small for it fails the verify below. The
			// metadata units likewise.
			extCnt := loc.Grp.GetExtCnt()
			ledger, err := newDnLedger(stm, sc.Cid, sc.Cc)
			if err != nil {
				return err
			}
			units, err := migrMetaUnitsOf(sc.Cc, sc.Conf, loc.Grp)
			if err != nil {
				return err
			}
			// The pick was scanned against the group as the round's plan read
			// it: planBlack for the distinct-DN rule, and those DNs' locations
			// for tier 1 (architecture.md, Per-operation allocation). A side
			// hung off the group since — a migration of its other leg, a
			// repair's new leg — is missing from that plan, so the pick may
			// sit on that side's very DN, behind a capacity key verifyPick
			// still finds, or in its failure domain, and only the group as
			// this transaction reads it can tell. A group that occupies a DN
			// the plan's read of it did not is therefore a changed candidate,
			// not a refusal (GW9): the next round plans from the group as it
			// now stands. A pick on a DN the group occupies always fails this:
			// the scan excluded planBlack, so that DN is a gain. Only a gain
			// is checked: when the group has left a DN since the plan, the
			// pick is still off every DN the group occupies, and the room the
			// departure frees is like a DN that joins after the scan — a
			// candidate the round did not count on, which GW9 never re-scans
			// for. Only a token-less request gets here with a gained DN: the
			// new side's SpRev bump fails a sent token at openSp above.
			for _, addrPort := range grpDnAddrs(loc.Grp, rep) {
				if !containsName(planBlack, addrPort) {
					return errCandidateChanged
				}
			}
			dn, err := ledger.verifyPick(cand, extCnt, units)
			if err != nil {
				return err
			}
			// The pass-over decided again on the pick's own record: a disk
			// node created on the old side's machine after the plan listed
			// the machine's nodes, or deleted and created again at the
			// pick's address, is no destination. The old side's host name
			// cannot change, since its disk node holds the side.
			if planHost != "" && dn.GetHostName() == planHost {
				return errCandidateChanged
			}
			minter := newSpIdMinter(sc.Conf)
			migrId = minter.mint()
			dstSideId := minter.mint()
			if err := minter.commit(sc.Conf, opCreateMigration); err != nil {
				return err
			}
			loc.Leg.SideList = append(loc.Leg.GetSideList(), &pb.Side{
				SideId:      dstSideId,
				AddrPort:    cand.AddrPort,
				CntlidSlot:  slot,
				NvmeTrConf:  dn.GetNvmeTrConf(),
				ErrEpoch:    0,
				Provisioned: false,
			})
			ptr := &pb.SidePointer{
				SpId:   sc.SpId(),
				LegId:  loc.Leg.GetLegId(),
				SideId: dstSideId,
			}
			if err := ledger.charge(
				cand.AddrPort, ptr, extCnt,
			); err != nil {
				return err
			}
			// Defensive, as charge's own check is: verifyPick has already
			// answered a DN short of the units as a changed candidate.
			if err := ledger.chargeUnits(
				cand.AddrPort, units, whatMigrDst,
			); err != nil {
				return err
			}
			if err := ledger.flush(opCreateMigration); err != nil {
				return err
			}
			stm.Put(
				model.MigrationKey(
					sc.Cid, sc.SpId(), req.GetMigrName()),
				&pb.Migration{
					MigrId:         migrId,
					SrcSideId:      req.GetSrcSideId(),
					DstSideId:      dstSideId,
					DmCloneConf:    req.GetDmCloneConf(),
					MetaUnits:      units,
					CutoverPending: true,
				},
			)
			sc.Conf.MigrNameList = append(
				sc.Conf.GetMigrNameList(), req.GetMigrName())
			stm.Put(
				model.SliceKey(sc.Cid, sc.SpId(), loc.SliceId), loc.Slice)
			stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
			return bumpSp(stm, opCreateMigration, sc)
		})
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateMigrationReply{MigrId: migrId}, nil
}

// FinishMigration is the FinishMigration of architecture.md, Migrations: the
// destination side becomes the leg's only side and the source is released,
// and the destination's DN gets back the metadata units its dm-clone run held
// (architecture.md, Metadata units).
//
// It is two-phase (AG4) for the same reason DeleteClone is: `force == false`
// must PROVE the dm-clone has finished hydrating before the source disappears,
// and only the destination DN's agent knows that — so one read-only STM
// resolves what the call needs, the deleting gate included, and checks the
// token, the GetSideInfo happens strictly between the transactions (AG1), and
// the deciding STM re-runs the whole resolution plus the token check. Nothing
// from phase 1 is trusted in phase 2: any interleaved mutation bumped SpRev,
// so a token that was sent subsumes the staleness of everything phase 1 saw.
// A caller that sent none gets the re-resolution but not that subsumption —
// GW6 is presence-based, so an interleaved mutation stays invisible
// to it, save one: the deciding STM compares the pool's and the migration's
// ids with phase 1's whatever the token, and answers ABORTED when another
// migration took the name in between.
//
// The agent answers GetSideInfo from its latest observation of the side and
// reads nothing of the node (architecture.md, Live-state reporting). That is
// still proof: a copy only advances, so an observation that lags the node
// shows less of it, and the refusal it gives is one the caller repeats
// (common.HydrationComplete).
//
// An unreachable agent is FAILED_PRECONDITION, not ABORTED (AG3): the caller
// cannot prove hydration is done, which is exactly the precondition
// architecture.md, Migrations, states. `force == true` skips the whole
// judgement and finishes regardless.
//
// One refusal comes ahead of that judgement and holds with or without force:
// a migration whose record still carries cutover_pending is
// FAILED_PRECONDITION, decided from the record before any agent call
// (migrCutoverStarted).
func (s *Server) FinishMigration(
	ctx context.Context,
	req *pb.FinishMigrationRequest,
) (*pb.FinishMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	var (
		phaseCid  uint64
		phaseDnId uint64
		phaseAddr string
		phasePtr  *pb.SidePointer
		// phaseSpId and phaseMigrId name the pool and the migration phase
		// 1 read, whose copy the agent call proves whole; phase 2 ends that
		// migration or none, so a migration made again under the name in
		// between, after a forced cancel, another caller's finish or the
		// worker's own, is never ended on the old one's proof, as
		// DeleteClone compares its clone's id.
		phaseSpId   uint64
		phaseMigrId uint64
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		// openSp, not openSpRead, as in CreateMigration's planning read:
		// GW5's deleting gate, then GW6's token, before the migration lookup
		// and before any agent call. Without them a deleting pool or a stale
		// token could first meet the lookup's NOT_FOUND or, unforced, the
		// hydration verdict and its agent call. Phase 2 checks both again
		// (AG4).
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		if err := migrCutoverStarted(req.GetMigrName(), migr); err != nil {
			return err
		}
		phaseCid = sc.Cid
		phaseSpId = sc.SpId()
		phaseMigrId = migr.GetMigrId()
		if req.GetForce() {
			return nil
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, ok := findSide(sc.Conf, slices, migr.GetDstSideId())
		if !ok {
			// A live Migration always owns both sides: Cancel removes the
			// destination and the record together, Finish the source and the
			// record together. A destination that is gone while the record
			// stands is a lost invariant, which is an ABORTED
			// (architecture.md, UNEXPECTED_ERROR → `ABORTED`).
			return errAborted(
				"migration %q destination side %d is in no leg",
				req.GetMigrName(), migr.GetDstSideId())
		}
		dn := &pb.DnConf{}
		dnKey := model.DnConfKey(sc.Cid, loc.Side.GetAddrPort())
		if !stm.Get(dnKey, dn) {
			// GW7: the request named a migration, not this DN — the DN is
			// named by the destination side, and DeleteDiskNode refuses a DN
			// whose side_ptr_list is non-empty. Its absence is a lost
			// invariant key, which is an ABORTED (architecture.md,
			// UNEXPECTED_ERROR → `ABORTED`).
			return errAborted("dn_conf key %q is missing", dnKey)
		}
		phaseDnId = dn.GetDnId()
		phaseAddr = loc.Side.GetAddrPort()
		phasePtr = &pb.SidePointer{
			SpId:   sc.SpId(),
			LegId:  loc.Leg.GetLegId(),
			SideId: loc.Side.GetSideId(),
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	if !req.GetForce() {
		details := ""
		callErr := withDnAgent(ctx, phaseAddr,
			func(ctx context.Context, client pb.DiskNodeAgentClient) error {
				reply, err := client.GetSideInfo(ctx, &pb.GetSideInfoRequest{
					ClusterId:   phaseCid,
					DnId:        phaseDnId,
					SidePointer: phasePtr,
				})
				if err != nil {
					return err
				}
				details = reply.GetSideInfo().GetMigrDstInfo().
					GetDmCloneInfo().GetDetails()
				return nil
			})
		if callErr != nil {
			return nil, errPrecondition(
				"migration %q: disk node %q did not report hydration: %v",
				req.GetMigrName(), phaseAddr, callErr)
		}
		if !common.HydrationComplete(details) {
			return nil, errPrecondition(
				"migration %q has not finished hydrating", req.GetMigrName())
		}
	}
	var migrId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		migrId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		if err := migrCutoverStarted(req.GetMigrName(), migr); err != nil {
			return err
		}
		if sc.SpId() != phaseSpId || migr.GetMigrId() != phaseMigrId {
			return errAborted(
				"migration %q is another migration (pool %d, id %d) than "+
					"the one the first read found (pool %d, id %d): run "+
					"the finish again", req.GetMigrName(), sc.SpId(),
				migr.GetMigrId(), phaseSpId, phaseMigrId)
		}
		migrId = migr.GetMigrId()
		// The source's extents go back to its DN and its pointer disappears,
		// which is what makes the src agent tear the side down on its next
		// syncup; the destination becomes the leg's only side, and the units
		// CreateMigration charged on its DN come back without a bump of that
		// DN (model.EndMigrationIn; architecture.md, Metadata units). The
		// write is the one the worker's finish runs too (dnv-worker.md AR8).
		// Its errors are lost invariants, ABORTED (mapStmErr).
		_, _, err = model.EndMigrationIn(stm, opFinishMigration, sc.Cid,
			sc.Shard(), req.GetSpName(), sc.Conf, req.GetMigrName(), migr,
			false, false, sc.Cc)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.FinishMigrationReply{MigrId: migrId}, nil
}

// CancelMigration is the CancelMigration of architecture.md, Migrations: the
// mirror rollback of CreateMigration.
//
// One transaction decides it, as no agent has to prove anything for it:
// whatever the destination has copied is thrown away. Once the cutover has
// started the destination may serve and hold the only copy of the leg's
// newest writes, so the cancel is refused then unless forced
// (migrCancelAllowed). The source side returns to service on its next
// SyncupSide; the destination DN sees its pointer disappear and tears the
// stack down, including the migration's local bitmap files (dnagent.md SH7
// and DN6).
//
// A forced cancel after the cutover does more (architecture.md,
// Migrations, The forced cancel). Its write marks the leg to be copied
// again in an md-raid1 group of more than one leg (model.EndMigrationIn),
// as the old side then serves the leg's content as of the cutover. Before
// the write, outside any transaction, it reads the primary's report for
// the group's legs in sync (cancelLegsInSync), which only words the
// answer's loss (cancelLoss). After the write it waits until the primary
// uses the old side's path alone (waitOldSideServes), so a host write
// after the answer lands on the old side; at the bound it answers
// DEADLINE_EXCEEDED, the cancel committed.
func (s *Server) CancelMigration(
	ctx context.Context,
	req *pb.CancelMigrationRequest,
) (*pb.CancelMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	// Phase 1, for a forced cancel after the cutover: the leg, its group
	// and the primary, whose report says which legs of the group md holds
	// in sync. Phase 1's refusals are the decision's own, which the
	// transaction runs again.
	var (
		after       bool
		phaseMigrId uint64
		legId       uint64
		srcSideId   uint64
		grpId       uint64
		tgt         grpReportTarget
		hasPrimary  bool
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		after, hasPrimary, phaseMigrId = false, false, 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		if err := migrCancelAllowed(
			req.GetMigrName(), migr, req.GetForce()); err != nil {
			return err
		}
		phaseMigrId = migr.GetMigrId()
		if migr.GetCutoverPending() {
			return nil
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, ok := findSide(sc.Conf, slices, migr.GetSrcSideId())
		if !ok {
			return errAborted("migration %q: its source side %d is in no "+
				"leg", req.GetMigrName(), migr.GetSrcSideId())
		}
		after = true
		legId, grpId = loc.Leg.GetLegId(), loc.Grp.GetGrpId()
		srcSideId = migr.GetSrcSideId()
		tgt, hasPrimary, err = primaryTarget(stm, sc)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	inSync, known := cancelLegsInSync(ctx, after && hasPrimary, tgt, grpId)
	var (
		migrId   uint64
		revision uint64
		marked   bool
	)
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		migrId, revision, marked = 0, 0, false
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		migrId = migr.GetMigrId()
		if err := migrCancelAllowed(
			req.GetMigrName(), migr, req.GetForce()); err != nil {
			return err
		}
		// Phase 1 read the cutover started or pending; a migration that
		// changed since, the cutover in between or another migration made
		// under the name, is ABORTED, so the answer's words fit the write.
		if migr.GetCutoverPending() == after ||
			migr.GetMigrId() != phaseMigrId {
			return errAborted("migration %q changed while the cancel read "+
				"it; send the cancel again", req.GetMigrName())
		}
		// The destination's extents and its metadata units return to its DN
		// and its pointer goes, which is the whole rollback: the source side
		// was never touched (model.EndMigrationIn; architecture.md,
		// Migrations; Metadata units). The write is the one the worker's
		// cancel runs too (dnv-worker.md AR8). Its errors are lost
		// invariants, ABORTED (mapStmErr).
		revision, marked, err = model.EndMigrationIn(stm, opCancelMigration,
			sc.Cid, sc.Shard(), req.GetSpName(), sc.Conf, req.GetMigrName(),
			migr, true, after, sc.Cc)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	reply := &pb.CancelMigrationReply{MigrId: migrId, CopyAgain: marked}
	if !after {
		return reply, nil
	}
	reply.Loss = cancelLoss(marked, grpId, legId, inSync, known)
	if err := s.waitOldSideServes(ctx, req, oldSideWait{
		legId: legId, srcSideId: srcSideId, grpId: grpId,
		revision: revision, recopy: marked && reply.Loss == "",
		loss: reply.Loss,
	}); err != nil {
		return nil, err
	}
	return reply, nil
}

// cancelLegsInSync is a forced cancel's read of the primary's latest report
// (GetCntlrInfo answers from the agent's observation and reads nothing of
// the node): the legs of group grpId that md holds in sync
// (GrpInfo.in_sync_leg_id_list), and whether the report answered with the
// group's reading. ask false, a cancel before the cutover or a pool with no
// primary, reads nothing.
func cancelLegsInSync(
	ctx context.Context,
	ask bool,
	tgt grpReportTarget,
	grpId uint64,
) ([]uint64, bool) {
	if !ask {
		return nil, false
	}
	var (
		legs  []uint64
		known bool
	)
	_ = withCnAgent(ctx, tgt.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: tgt.ClusterId,
				CnId:      tgt.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    tgt.SpId,
					CntlrId: tgt.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			info, ok := reply.GetCntlrInfo().GetGrpIdToGrpInfo()[grpId]
			if ok {
				legs, known = info.GetInSyncLegIdList(), true
			}
			return nil
		})
	return legs, known
}

// cancelLoss is a forced cancel's answer about the writes since the cutover
// (CancelMigrationReply.loss): empty when it marked the leg to be copied
// again and the primary's report held another leg of the group in sync;
// otherwise why those writes are lost on the leg, or may be.
func cancelLoss(
	marked bool,
	grpId uint64,
	legId uint64,
	inSync []uint64,
	known bool,
) string {
	if !marked {
		return fmt.Sprintf("group %d has no other leg to copy leg %d "+
			"from: the writes since the cutover are lost on it", grpId,
			legId)
	}
	if !known {
		return fmt.Sprintf("the primary's report of group %d did not "+
			"answer, so whether another leg is in sync is not known: the "+
			"writes since the cutover are lost on leg %d unless one is",
			grpId, legId)
	}
	for _, id := range inSync {
		if id != legId {
			return ""
		}
	}
	return fmt.Sprintf("the primary's report held no other leg of group %d "+
		"in sync: leg %d is copied again only once one is, and the writes "+
		"since the cutover are lost on it unless one is", grpId, legId)
}

// oldSideWait is what a forced cancel's wait reads the primary's report
// for: the leg and its old side, the group, the revision the cancel
// committed, whether the primary is to copy the leg again beside another
// leg in sync (recopy), and the reply's loss, which the bound's error
// carries.
type oldSideWait struct {
	legId     uint64
	srcSideId uint64
	grpId     uint64
	revision  uint64
	recopy    bool
	loss      string
}

// waitOldSideServes is a forced cancel's wait after its write (architecture.md,
// Migrations, The forced cancel): until the pool's primary reports, at or
// past the cancel's revision, the leg's row as RES_STATUS_OK with no reason
// (leg_id_to_leg): the leg serves on the primary's node, and its one side,
// the old side, is the only path to it there, which
// RES_REASON_LEG_PATH_NOT_SOLE would deny (cnagent.md CN28), so the primary
// writes the leg to the old side only; and, where the primary is to copy
// the leg again beside another leg in sync, until the group's reading names
// the cancel's revision as the recopy the primary began
// (leg_id_to_recopy_revision, cnagent.md CN12), as md reads no more of the
// leg's old bytes from then on. Each try resolves the primary again, as a
// failover may move it, and reads its latest report; a pool with no primary,
// a report that does not answer and one that does not show it yet try again
// a second later, for at most Server.migrCancelWait. At the bound, or when
// the caller's context ends first, it answers DEADLINE_EXCEEDED: the cancel
// has committed, and the message names the revision and how to check the
// path before any write, and carries the reply's loss.
func (s *Server) waitOldSideServes(
	ctx context.Context,
	req *pb.CancelMigrationRequest,
	w oldSideWait,
) error {
	wctx, cancel := context.WithTimeout(ctx, s.migrCancelWait)
	defer cancel()
	for {
		if s.oldSideServes(wctx, req, w) {
			return nil
		}
		select {
		case <-wctx.Done():
			msg := fmt.Sprintf("migration %q is cancelled at revision %d, "+
				"but the primary does not yet use the old side %d of leg "+
				"%d alone: before any write to the pool, check with "+
				"`dnvctl cntlr inspect` that applied_revision is at least "+
				"%d and leg_id_to_leg reads leg %d RES_STATUS_OK with no "+
				"reason", req.GetMigrName(), w.revision, w.srcSideId,
				w.legId, w.revision, w.legId)
			if w.loss != "" {
				msg += "; " + w.loss
			}
			return status.Error(codes.DeadlineExceeded, msg)
		case <-time.After(time.Second):
		}
	}
}

// oldSideServes is one try of waitOldSideServes: the pool's primary, read
// in a snapshot, and its latest report.
func (s *Server) oldSideServes(
	ctx context.Context,
	req *pb.CancelMigrationRequest,
	w oldSideWait,
) bool {
	var (
		tgt grpReportTarget
		ok  bool
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		tgt, ok, err = primaryTarget(stm, sc)
		return err
	})
	if err != nil || !ok {
		return false
	}
	serves := false
	_ = withCnAgent(ctx, tgt.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: tgt.ClusterId,
				CnId:      tgt.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    tgt.SpId,
					CntlrId: tgt.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			info := reply.GetCntlrInfo()
			row := info.GetLegIdToLeg()[w.legId]
			serves = reply.GetRevision() >= w.revision &&
				row.GetStatus() == pb.ResStatus_RES_STATUS_OK &&
				row.GetReason() == pb.ResReason_RES_REASON_NONE
			if serves && w.recopy {
				serves = info.GetGrpIdToGrpInfo()[w.grpId].
					GetLegIdToRecopyRevision()[w.legId] == w.revision
			}
			return nil
		})
	return serves
}

// GetMigration is the GetMigration of architecture.md, Migrations: one
// read-only transaction (architecture.md, STM discipline), so the SpConf that
// resolves the key and the Migration it addresses are read at one store
// revision. It carries no token — a read never
// needs one — and the Migration is replied verbatim, cutover_pending as
// stored, which tells a caller whether the cutover has started and so whether
// a FinishMigration can succeed yet.
func (s *Server) GetMigration(
	ctx context.Context,
	req *pb.GetMigrationRequest,
) (*pb.GetMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	var migr *pb.Migration
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		migr, err = getMigration(stm, sc, req.GetMigrName())
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetMigrationReply{Migr: migr}, nil
}
