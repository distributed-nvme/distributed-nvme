package model

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// opWithdrawGrow is WithdrawGrow's Op name (MD6).
const opWithdrawGrow = "WithdrawGrow"

// WithdrawGrow withdraws the newest group of one kind of a slice, a grown
// group no array was ever built from (MD6; dnv-worker.md AR6), whichever grow
// added it, an operator's included: it pops the group off the tail of its
// list, gives every side of it back to its disk node and the CN of every
// current cntlr of the SP the extents the group costs it, and bumps SpRev,
// all in one transaction. It returns the grp_id it withdrew.
//
// The guards make the group one no array was ever built from, so withdrawing
// it loses nothing:
//
//   - it is the newest group of its kind in the slice, grpId, the only group
//     a grow appends and the only one whose removal keeps every other
//     group's index, which its md names carry;
//   - every leg has one side, so no migration is in flight on the group (a
//     migration gives a leg its second side, and owns both until its finish
//     or cancel), and one leg's only side is still not provisioned: a cntlr
//     builds a group only once no leg has every side unprovisioned, so the
//     group was never assembled (cnagent.md CN9), and a provisioned flip that
//     commits first makes this transaction refuse;
//   - the grow is still pending against poolTotal, the total of that kind the
//     primary reported (GrowPending): the pool does not count the group;
//   - the group has no repair, no retired leg, no forced-start mark and no
//     operator in charge: an operator or a repair acts on it, so it is left
//     to them.
//
// cc is the cluster's stored conf, for the bins of the capacity keys the
// release maintains.
func WithdrawGrow(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	sliceId uint64,
	isMeta bool,
	grpId uint64,
	poolTotal uint64,
	cc *pb.ClusterConf,
) (uint64, error) {
	done := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		done = 0
		if err := checkSpRev(
			s, opWithdrawGrow, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfForOp(s, opWithdrawGrow, cid, spName, spId)
		if err != nil {
			return err
		}
		if err := ValidateBdevConf(conf.GetBdevConf()); err != nil {
			return fail(opWithdrawGrow, err.Error())
		}
		if err := ValidateClusterConf(cc); err != nil {
			return fail(opWithdrawGrow, err.Error())
		}
		if !containsId(conf.GetSliceIdList(), sliceId) {
			return fail(opWithdrawGrow, "slice not in sp")
		}
		sliceKey := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(sliceKey, slice) {
			return fail(opWithdrawGrow, "slice not found")
		}
		grps := slice.GetDataGrpList()
		if isMeta {
			grps = slice.GetMetaGrpList()
		}
		if len(grps) < 2 {
			return fail(opWithdrawGrow, "no grown group")
		}
		grp := grps[len(grps)-1]
		if grp.GetGrpId() != grpId {
			return fail(opWithdrawGrow, "not the newest group")
		}
		if !GrowPending(
			slice, isMeta, poolTotal, PoolBlockSize(conf.GetBdevConf()),
		) {
			return fail(opWithdrawGrow, "grow not pending")
		}
		if grpMigrating(grp) {
			return fail(opWithdrawGrow, "a leg has two sides")
		}
		if !grpProvisioning(grp) {
			return fail(opWithdrawGrow, "no leg provisioning")
		}
		switch {
		case grp.GetRepName() != "":
			return fail(opWithdrawGrow, ReasonGrpHasRepair)
		case len(grp.GetRetiredLegIdList()) > 0:
			return fail(opWithdrawGrow, "group has a retired leg")
		case grp.GetForcedStart() != nil:
			return fail(opWithdrawGrow, "group has a forced-start mark")
		case grp.GetOperatorInCharge():
			return fail(opWithdrawGrow, ReasonGrpOperatorInCharge)
		}
		rel := newDnReleaser(s, cid, cc)
		for _, leg := range grp.GetLegList() {
			releaseLeg(rel, spId, leg, grp.GetExtCnt())
		}
		if err := rel.flush(opWithdrawGrow); err != nil {
			return err
		}
		if err := creditSpCns(
			s, opWithdrawGrow, cid, conf, grp.GetExtCnt(),
		); err != nil {
			return err
		}
		if isMeta {
			slice.MetaGrpList = grps[:len(grps)-1]
		} else {
			slice.DataGrpList = grps[:len(grps)-1]
		}
		s.Put(sliceKey, slice)
		done = grpId
		return BumpSpRev(s, opWithdrawGrow, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return done, nil
}

// grpProvisioning reports whether a leg of grp has every side not
// provisioned yet, which keeps every cntlr from building the group
// (cnagent.md CN9).
func grpProvisioning(grp *pb.Group) bool {
	for _, leg := range grp.GetLegList() {
		provisioning := len(leg.GetSideList()) > 0
		for _, side := range leg.GetSideList() {
			if side.GetProvisioned() {
				provisioning = false
			}
		}
		if provisioning {
			return true
		}
	}
	return false
}

// grpMigrating reports whether a leg of grp has more than one side, which a
// migration in flight gives it.
func grpMigrating(grp *pb.Group) bool {
	for _, leg := range grp.GetLegList() {
		if len(leg.GetSideList()) > 1 {
			return true
		}
	}
	return false
}

// creditSpCns is chargeSpCns undone: it gives the CN of every current cntlr of
// the SP back the extents SpCnCharges says a group of extCnt costs, one budget
// update, one capacity-key maintenance and one CnRev bump per CN. A cntlr
// created or replaced since the grow was charged the SP's whole footprint,
// the group included, so the current cntlrs are the ones that hold the
// group's charge. A cntlr or a CN record that is gone is skipped, as
// releaseCn skips one: there is nothing left to give the extents back to.
func creditSpCns(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
	extCnt uint64,
) error {
	cntlrs := make(map[uint64]*pb.Cntlr)
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr := &pb.Cntlr{}
		if s.Get(CntlrKey(cid, conf.GetSpId(), cntlrId), cntlr) {
			cntlrs[cntlrId] = cntlr
		}
	}
	order, charges := SpCnCharges(conf, cntlrs, extCnt)
	for _, addrPort := range order {
		cn := &pb.CnConf{}
		if !s.Get(CnConfKey(cid, addrPort), cn) {
			continue
		}
		newCn := proto.Clone(cn).(*pb.CnConf)
		newCn.FreeExtCnt += charges[addrPort]
		s.Put(CnConfKey(cid, addrPort), newCn)
		MaintainCnCapacity(s, cid, addrPort, cn, newCn)
		if err := BumpCnRev(s, op, cid, newCn); err != nil {
			return err
		}
	}
	return nil
}
