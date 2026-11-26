package model

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A health epoch later than the clock of the worker that reads it holds every
// threshold that counts from it until that clock catches up (thresholdReached).
// The sp worker's reaction pass writes its own time over such an epoch, so
// that its thresholds count from its own clock (dnv-worker.md HL3, AR4). It is
// the one write that replaces a stored non-zero epoch with another
// (applyErrEpoch).

// LowerRecordsPerTxn is how many records one transaction of the write reads
// at most: each costs a compare and a put, well inside common.EtcdMaxTxnOps,
// so the widest pool's cntlrs, slices and clones fit in the first.
const LowerRecordsPerTxn = 256

// EpochRecord names the kind of record an EpochRecordRef points at.
type EpochRecord int

const (
	// EpochRecordCntlr is a Cntlr, Id its cntlr_id.
	EpochRecordCntlr EpochRecord = iota
	// EpochRecordSlice is a Slice, Id its slice_id: the legs and sides of
	// both its group lists.
	EpochRecordSlice
	// EpochRecordClone is a Clone, Name its name: the legs and sides of its
	// clone meta group.
	EpochRecordClone
	// EpochRecordRepair is a Repair, Name its rep_name: its new legs and
	// their sides.
	EpochRecordRepair
)

// EpochRecordRef is one record of an SP whose health epochs the write lowers.
type EpochRecordRef struct {
	Record EpochRecord
	Id     uint64
	Name   string
}

// lowerErrEpoch is the rule of the write: an epoch later than now becomes
// now, and any other, a zero included, stays. It returns the value to store
// and whether that differs from what is stored.
func lowerErrEpoch(stored uint64, now uint64) (uint64, bool) {
	if stored > now {
		return now, true
	}
	return stored, false
}

// lowerLeg lowers a leg's epoch and those of its sides, and returns how many
// it changed.
func lowerLeg(leg *pb.Leg, now uint64) int {
	if leg == nil {
		return 0
	}
	cnt := 0
	if value, changed := lowerErrEpoch(leg.GetErrEpoch(), now); changed {
		leg.ErrEpoch = value
		cnt++
	}
	for _, side := range leg.GetSideList() {
		cnt += lowerSide(side, now)
	}
	return cnt
}

// lowerSide lowers one side's epoch, and returns 1 when it changed it.
func lowerSide(side *pb.Side, now uint64) int {
	if side == nil {
		return 0
	}
	if value, changed := lowerErrEpoch(side.GetErrEpoch(), now); changed {
		side.ErrEpoch = value
		return 1
	}
	return 0
}

// lowerGroups lowers the epochs of every leg and side of grps.
func lowerGroups(grps []*pb.Group, now uint64) int {
	cnt := 0
	for _, grp := range grps {
		for _, leg := range grp.GetLegList() {
			cnt += lowerLeg(leg, now)
		}
	}
	return cnt
}

// LowerFutureErrEpochs writes now over every health epoch later than now in
// the records refs names, LowerRecordsPerTxn records to an STM in refs'
// order, and returns how many epochs it lowered; on an error, those the
// STMs before it lowered. It reads each epoch again in its STM, so an epoch
// cleared or lowered since the caller's load is left as it is, and a zero is
// never set. A record that is gone is skipped. It bumps no revision, as no
// health write does (architecture.md, Revision keys and the sync fan-out),
// and checks no level: it changes only when a threshold starts to count.
func LowerFutureErrEpochs(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	now uint64,
	refs []EpochRecordRef,
) (int, error) {
	var unique []EpochRecordRef
	seen := make(map[EpochRecordRef]bool)
	for _, ref := range refs {
		if !seen[ref] {
			seen[ref] = true
			unique = append(unique, ref)
		}
	}
	total := 0
	for chunk := range slices.Chunk(unique, LowerRecordsPerTxn) {
		lowered := 0
		err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
			lowered = 0
			for _, ref := range chunk {
				lowered += lowerRecord(s, cid, spId, now, ref)
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		total += lowered
	}
	return total, nil
}

// lowerRecord lowers the epochs of one record in the STM and puts it back
// when it changed any; it returns how many it changed.
func lowerRecord(
	s etcdutil.STM,
	cid uint64,
	spId uint64,
	now uint64,
	ref EpochRecordRef,
) int {
	switch ref.Record {
	case EpochRecordCntlr:
		key := CntlrKey(cid, spId, ref.Id)
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			return 0
		}
		value, changed := lowerErrEpoch(cntlr.GetErrEpoch(), now)
		if !changed {
			return 0
		}
		cntlr.ErrEpoch = value
		s.Put(key, cntlr)
		return 1
	case EpochRecordSlice:
		key := SliceKey(cid, spId, ref.Id)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return 0
		}
		cnt := lowerGroups(slice.GetMetaGrpList(), now) +
			lowerGroups(slice.GetDataGrpList(), now)
		if cnt > 0 {
			s.Put(key, slice)
		}
		return cnt
	case EpochRecordClone:
		key := CloneKey(cid, spId, ref.Name)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return 0
		}
		var grps []*pb.Group
		if clone.GetMetaGrp() != nil {
			grps = append(grps, clone.GetMetaGrp())
		}
		cnt := lowerGroups(grps, now)
		if cnt > 0 {
			s.Put(key, clone)
		}
		return cnt
	case EpochRecordRepair:
		key := RepairKey(cid, spId, ref.Name)
		rep := &pb.Repair{}
		if !s.Get(key, rep) {
			return 0
		}
		cnt := 0
		for _, leg := range rep.GetNewLegList() {
			cnt += lowerLeg(leg, now)
		}
		if cnt > 0 {
			s.Put(key, rep)
		}
		return cnt
	}
	return 0
}

// sortedKeys returns m's keys in ascending order.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}

// FutureEpochRecords returns a ref to every record of state that holds a
// health epoch later than now that a threshold reads, in a stable order: the
// cntlrs, the slices, the clones and the repairs, each by id or name. A
// node record's epoch and a retired side's are no part of it: every reader
// reads those as set or not.
// The second result is the largest step, the latest such epoch minus now.
func FutureEpochRecords(
	state *SpState,
	now uint64,
) ([]EpochRecordRef, uint64) {
	var refs []EpochRecordRef
	var step uint64
	note := func(epoch uint64) bool {
		if epoch <= now {
			return false
		}
		step = max(step, epoch-now)
		return true
	}
	legAhead := func(leg *pb.Leg) bool {
		ahead := false
		if leg == nil {
			return false
		}
		if note(leg.GetErrEpoch()) {
			ahead = true
		}
		for _, side := range leg.GetSideList() {
			if note(side.GetErrEpoch()) {
				ahead = true
			}
		}
		return ahead
	}
	grpsAhead := func(grps ...[]*pb.Group) bool {
		ahead := false
		for _, list := range grps {
			for _, grp := range list {
				for _, leg := range grp.GetLegList() {
					if legAhead(leg) {
						ahead = true
					}
				}
			}
		}
		return ahead
	}
	for _, id := range sortedKeys(state.Cntlrs) {
		if note(state.Cntlrs[id].GetErrEpoch()) {
			refs = append(refs, EpochRecordRef{Record: EpochRecordCntlr, Id: id})
		}
	}
	for _, id := range sortedKeys(state.Slices) {
		slice := state.Slices[id]
		if grpsAhead(slice.GetMetaGrpList(), slice.GetDataGrpList()) {
			refs = append(refs, EpochRecordRef{Record: EpochRecordSlice, Id: id})
		}
	}
	for _, name := range sortedKeys(state.Clones) {
		grp := state.Clones[name].GetMetaGrp()
		if grp != nil && grpsAhead([]*pb.Group{grp}) {
			refs = append(refs,
				EpochRecordRef{Record: EpochRecordClone, Name: name})
		}
	}
	for _, name := range sortedKeys(state.Repairs) {
		ahead := false
		for _, leg := range state.Repairs[name].GetNewLegList() {
			if legAhead(leg) {
				ahead = true
			}
		}
		if ahead {
			refs = append(refs,
				EpochRecordRef{Record: EpochRecordRepair, Name: name})
		}
	}
	return refs, step
}
