package model

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The end of a migration (architecture.md, Migrations): a cancel drops the
// destination side and a finish the source side, each with the migration's
// record, in one transaction. The gateway runs the write after its own
// checks (gateway.md, Handlers by resource group), and the sp worker's leg
// repair runs it, through CancelMigration and FinishMigration below, on the
// migrating leg it ends before it repairs it (dnv-worker.md AR8).

// The Op names of the migration ends the worker runs.
const (
	opCancelMigration = "CancelMigration"
	opFinishMigration = "FinishMigration"
)

// The reasons of the worker's migration ends' refusals, beside the pool's
// and the record's own.
const (
	// ReasonCutoverStarted is a cancel of a migration whose cutover has
	// started: the destination may hold the only copy of the writes since.
	ReasonCutoverStarted = "cutover started"
	// ReasonCutoverPending is a finish of a migration whose cutover has not
	// started: the destination holds no copied byte.
	ReasonCutoverPending = "cutover pending"
	// ReasonMigrationGone is a migration whose record is gone, or another
	// one now under its name.
	ReasonMigrationGone = "migration gone"
)

// EndMigrationIn is the write that ends the migration migrName, whose
// record the caller read as migr from conf's pool, in the caller's STM: a
// cancel (cancel true) drops the destination side from the leg, its extents
// and the metadata units the migration charged going back to its disk node,
// which is bumped; a finish drops the source side, its extents going back
// to its disk node, which is bumped, and the migration's units to the
// destination's, which is written and re-indexed but not bumped, nothing it
// is sent depending on the count (architecture.md, Metadata units). Either
// way the migration's record and its name in migr_name_list go, and with
// them the keys an older build kept the migration's bitmap under, at every
// index below LegacyMigrBmCnt (architecture.md, Upgrade in lockstep); the
// slice and the SpConf are written, and SpRev is bumped. The caller has
// made its checks; the errors here are the lost invariants of a live
// migration — a slice, a side or a disk node record missing, two sides not
// in one leg, or a stored cluster conf that fails its validation — which
// the gateway answers ABORTED.
//
// A cancel after the migration's cutover (recopy, the gateway's forced
// cancel) also marks the leg, Leg.recopy_revision set to the SpRev revision
// this write commits: the old side then serves the leg's content as of the
// cutover, and the primary reports whether that side alone serves the leg,
// and, when the pool is md-raid1 and the leg's group holds another leg,
// copies the leg whole from a member in sync (architecture.md, Migrations;
// cnagent.md CN12, CN28). It returns the SpRev revision the write commits
// and whether the mark asks for that copy.
func EndMigrationIn(
	s etcdutil.STM,
	op string,
	cid uint64,
	shard uint32,
	spName string,
	conf *pb.SpConf,
	migrName string,
	migr *pb.Migration,
	cancel bool,
	recopy bool,
	cc *pb.ClusterConf,
) (uint64, bool, error) {
	if err := ValidateClusterConf(cc); err != nil {
		return 0, false, err
	}
	spId := conf.GetSpId()
	var (
		sliceId uint64
		slice   *pb.Slice
		grp     *pb.Group
		leg     *pb.Leg
	)
	findSide := func(sideId uint64) (bool, error) {
		for _, id := range conf.GetSliceIdList() {
			key := SliceKey(cid, spId, id)
			cand := &pb.Slice{}
			if !s.Get(key, cand) {
				return false, fmt.Errorf("slice key %q is missing", key)
			}
			for _, g := range slices.Concat(cand.GetMetaGrpList(),
				cand.GetDataGrpList()) {
				for _, l := range g.GetLegList() {
					for _, side := range l.GetSideList() {
						if side.GetSideId() == sideId {
							sliceId, slice, grp, leg = id, cand, g, l
							return true, nil
						}
					}
				}
			}
		}
		return false, nil
	}
	dropId, keepId := migr.GetSrcSideId(), migr.GetDstSideId()
	what := "source"
	if cancel {
		dropId, keepId = keepId, dropId
		what = "destination"
	}
	found, err := findSide(dropId)
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, fmt.Errorf("migration %q %s side %d is in no leg",
			migrName, what, dropId)
	}
	var dropped, kept *pb.Side
	for _, side := range leg.GetSideList() {
		switch side.GetSideId() {
		case dropId:
			dropped = side
		case keepId:
			kept = side
		}
	}
	// A finish keeps the destination in the leg it drops the source from:
	// the two sides export one NQN, which the cntlrs aggregate as one
	// namespace's paths. A cancel keeps whatever the leg holds besides.
	dstAddr := dropped.GetAddrPort()
	if !cancel {
		if kept == nil {
			return 0, false, fmt.Errorf(
				"migration %q sides %d and %d are not in one leg",
				migrName, migr.GetSrcSideId(), migr.GetDstSideId())
		}
		dstAddr = kept.GetAddrPort()
	}
	ledger := newMigrLedger(s, cid, cc)
	if err := ledger.release(dropped.GetAddrPort(), spId,
		dropped.GetSideId(), grp.GetExtCnt()); err != nil {
		return 0, false, err
	}
	if err := ledger.releaseUnits(dstAddr, migr.GetMetaUnits()); err != nil {
		return 0, false, err
	}
	if err := ledger.flush(op); err != nil {
		return 0, false, err
	}
	sides := make([]*pb.Side, 0, len(leg.GetSideList()))
	for _, side := range leg.GetSideList() {
		if side.GetSideId() != dropId {
			sides = append(sides, side)
		}
	}
	leg.SideList = sides
	// STM.Put marshals at the call, so the revision goes into the mark
	// before the slice is staged.
	revision, err := bumpSpRevTo(s, op, shard, cid, spId)
	if err != nil {
		return 0, false, err
	}
	if cancel && recopy {
		leg.RecopyRevision = revision
	}
	copyAgain := cancel && recopy && isMdRaid1(conf) &&
		len(grp.GetLegList()) > 1
	s.Del(MigrationKey(cid, spId, migrName))
	for bmIdx := range uint32(LegacyMigrBmCnt) {
		s.Del(LegacyMigrBitmapKey(cid, spId, migrName, bmIdx))
	}
	conf.MigrNameList = removeName(conf.GetMigrNameList(), migrName)
	s.Put(SliceKey(cid, spId, sliceId), slice)
	s.Put(SpConfKey(cid, spName), conf)
	return revision, copyAgain, nil
}

// CancelMigration is the sp worker's cancel of the migration migrName of a
// bad leg (dnv-worker.md AR8): it requires the pool live at a level where
// reactions run, the record still the one the worker read (migrId) and its
// cutover still pending, the one direction in which a cancel loses nothing,
// since the source holds every write and the destination serves nothing;
// then EndMigrationIn. It takes no expected revision, as the worker's
// repair ops do (MD6).
func CancelMigration(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	migrName string,
	migrId uint64,
	cc *pb.ClusterConf,
) error {
	return endMigration(ctx, cli, opCancelMigration, cid, shard, spId,
		spName, migrName, migrId, true, cc)
}

// FinishMigration is the sp worker's finish of the migration migrName of a
// bad leg (dnv-worker.md AR8): it requires the pool live at a level where
// reactions run, the record still the one the worker read (migrId) and its
// cutover started; the worker has read the destination's copy whole from
// the destination's latest report first, as the gateway's finish reads it
// from the disk node, since no transaction can; then EndMigrationIn.
func FinishMigration(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	migrName string,
	migrId uint64,
	cc *pb.ClusterConf,
) error {
	return endMigration(ctx, cli, opFinishMigration, cid, shard, spId,
		spName, migrName, migrId, false, cc)
}

// endMigration is CancelMigration's and FinishMigration's transaction.
func endMigration(
	ctx context.Context,
	cli *etcdutil.Client,
	op string,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	migrName string,
	migrId uint64,
	cancel bool,
	cc *pb.ClusterConf,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := loadSpConfForOp(s, op, cid, spName, spId)
		if err != nil {
			return err
		}
		migr := &pb.Migration{}
		if !s.Get(MigrationKey(cid, spId, migrName), migr) ||
			migr.GetMigrId() != migrId {
			return fail(op, ReasonMigrationGone)
		}
		switch {
		case cancel && !migr.GetCutoverPending():
			return fail(op, ReasonCutoverStarted)
		case !cancel && migr.GetCutoverPending():
			return fail(op, ReasonCutoverPending)
		}
		// The worker cancels only while the cutover is pending, which marks
		// nothing (recopy false).
		_, _, err = EndMigrationIn(s, op, cid, shard, spName, conf,
			migrName, migr, cancel, false, cc)
		return err
	})
}

// migrLedger is the DN bookkeeping of a migration's end: each disk node it
// touches is read once and written once, with its capacity key, and bumped
// once when a side's extents went back to it; one whose only change is its
// used metadata units is not bumped (architecture.md, Metadata units). A
// disk node record that is missing is a lost invariant, since a stored side
// names it.
type migrLedger struct {
	s     etcdutil.STM
	cid   uint64
	cc    *pb.ClusterConf
	old   map[string]*pb.DnConf
	cur   map[string]*pb.DnConf
	bump  map[string]bool
	order []string
}

func newMigrLedger(
	s etcdutil.STM,
	cid uint64,
	cc *pb.ClusterConf,
) *migrLedger {
	return &migrLedger{s: s, cid: cid, cc: cc,
		old:  make(map[string]*pb.DnConf),
		cur:  make(map[string]*pb.DnConf),
		bump: make(map[string]bool)}
}

func (l *migrLedger) get(addrPort string) (*pb.DnConf, error) {
	if cur, ok := l.cur[addrPort]; ok {
		return cur, nil
	}
	dn := &pb.DnConf{}
	if !l.s.Get(DnConfKey(l.cid, addrPort), dn) {
		return nil, fmt.Errorf("dn_conf for %q is missing", addrPort)
	}
	l.old[addrPort] = dn
	l.cur[addrPort] = proto.Clone(dn).(*pb.DnConf)
	l.order = append(l.order, addrPort)
	return l.cur[addrPort], nil
}

func (l *migrLedger) release(
	addrPort string,
	spId uint64,
	sideId uint64,
	extCnt uint64,
) error {
	dn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	dn.SidePtrList = removeSidePtr(dn.GetSidePtrList(), spId, sideId)
	dn.FreeExtCnt += extCnt
	l.bump[addrPort] = true
	return nil
}

func (l *migrLedger) releaseUnits(addrPort string, units uint64) error {
	if units == 0 {
		return nil
	}
	dn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	dn.UsedMetaUnits = ReleaseMetaUnits(dn.GetUsedMetaUnits(), units)
	return nil
}

func (l *migrLedger) flush(op string) error {
	for _, addrPort := range l.order {
		cur := l.cur[addrPort]
		l.s.Put(DnConfKey(l.cid, addrPort), cur)
		MaintainDnCapacity(l.s, l.cid, addrPort, l.cc, l.old[addrPort], cur)
		if !l.bump[addrPort] {
			continue
		}
		if err := BumpDnRev(l.s, op, l.cid, cur); err != nil {
			return err
		}
	}
	return nil
}

// opClearRecopy is the Op name of the sp worker's clear of a recopy mark.
const opClearRecopy = "ClearRecopy"

// RecopyRef names the recopy mark the sp worker clears (ClearRecopy): the
// leg by its group's parent, the group's id and its own id, and the mark by
// the revision it carried, which proves that the mark is still the one the
// primary's report answered.
type RecopyRef struct {
	Parent   GrpParent
	GrpId    uint64
	LegId    uint64
	Revision uint64
}

// ClearRecopy clears the recopy mark ref names (Leg.recopy_revision) once
// the primary's report shows the recopy done (dnv-worker.md AR13): for a
// leg the primary copies again, the group's reading names the mark's
// revision as the recopy the primary ran and holds the leg in sync; for any
// other leg, its row reads RES_STATUS_OK with no reason. It reports whether
// it wrote and the revision its STM committed, 0 when it wrote nothing, and
// raises SpRev when it writes, so every cntlr is handed the leg without the
// mark: one that stayed in an md-raid1 group of more than one leg would
// copy the leg again at the next agent restart of the primary.
//
// It takes no expected revision and a pool at any level that is not being
// deleted, as ClearForcedStart does. A stale ref is skipped, never refused:
// a parent, a group or a leg that is gone, a clone being deleted, and a mark
// that is gone or of another revision.
func ClearRecopy(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	ref RecopyRef,
) (bool, uint64, error) {
	cleared, revision := false, uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		cleared, revision = false, 0
		if _, err := loadSpConfAnyLevel(
			s, opClearRecopy, cid, spName, spId,
		); err != nil {
			return err
		}
		holder, err := loadGrpHolder(s, opClearRecopy, cid, spId, ref.Parent)
		if err != nil {
			return nil
		}
		grp := holder.group(ref.GrpId)
		if grp == nil || holder.deleting() {
			return nil
		}
		var leg *pb.Leg
		for _, l := range grp.GetLegList() {
			if l.GetLegId() == ref.LegId {
				leg = l
			}
		}
		if leg == nil || ref.Revision == 0 ||
			leg.GetRecopyRevision() != ref.Revision {
			return nil
		}
		leg.RecopyRevision = 0
		holder.put(s)
		revision, err = bumpSpRevTo(s, opClearRecopy, shard, cid, spId)
		cleared = err == nil
		return err
	})
	if err != nil {
		return false, 0, err
	}
	return cleared, revision, nil
}
