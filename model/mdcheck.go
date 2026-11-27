package model

import (
	"context"
	"slices"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The schedule of a group's md check (Group.md_check): the sp worker alone
// writes it (dnv-worker.md AR16), and the primary reads its request
// (cnagent.md CN12).

// The Op name of the schedule's write.
const opUpdateMdChecks = "UpdateMdChecks"

// MdCheckEdit is one change of one group's md check schedule: the group's
// stored schedule must still be From, the one the caller read, and becomes
// To. A nil From or To is the zero schedule.
type MdCheckEdit struct {
	Ref  GrpRef
	From *pb.MdCheck
	To   *pb.MdCheck
}

// UpdateMdChecks applies edits in one STM with one SpRev bump, and returns
// how many it applied and the revision its bump wrote, zero when it applied
// none and wrote nothing. Each edit is a compare and set: one whose group no
// longer holds From is skipped, so two owners of the pool (dnv-worker.md VW7)
// never both ask for a check or both end one, and so is a ref that is stale —
// a parent or a group that is gone, or a clone being deleted, as
// MarkGroupsArrayMade skips one. It takes no expected revision: each edit
// carries its own precondition. The schedule rides to every cntlr in the
// group's parent record, and the primary reads its request, so the write
// raises SpRev.
func UpdateMdChecks(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	edits []MdCheckEdit,
) (int, uint64, error) {
	applied, revision := 0, uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		applied, revision = 0, 0
		conf, err := loadSpConfAnyLevel(s, opUpdateMdChecks, cid, spName, spId)
		if err != nil {
			return err
		}
		holders := make(map[GrpParent]*grpHolder)
		holderOf := func(parent GrpParent) *grpHolder {
			holder, seen := holders[parent]
			if !seen {
				var err error
				holder, err = loadGrpHolder(
					s, opUpdateMdChecks, cid, spId, parent)
				if err != nil {
					holder = nil
				}
				holders[parent] = holder
			}
			return holder
		}
		// A new request goes in only while no group of the pool's slices
		// carries one, the edits before it applied: one check at a time
		// per pool, whatever two owners read.
		standing := func(conf *pb.SpConf) bool {
			for _, sliceId := range conf.GetSliceIdList() {
				holder := holderOf(GrpParent{SliceId: sliceId})
				if holder == nil || holder.slice == nil {
					continue
				}
				for _, grp := range append(
					slices.Clone(holder.slice.GetMetaGrpList()),
					holder.slice.GetDataGrpList()...) {
					if grp.GetMdCheck().GetRequestEpoch() != 0 {
						return true
					}
				}
			}
			return false
		}
		var changed []*grpHolder
		for _, edit := range edits {
			holder := holderOf(edit.Ref.Parent)
			if holder == nil || holder.deleting() {
				continue
			}
			grp := holder.group(edit.Ref.GrpId)
			if grp == nil || !SameMdCheck(grp.GetMdCheck(), edit.From) {
				continue
			}
			if edit.From.GetRequestEpoch() == 0 &&
				edit.To.GetRequestEpoch() != 0 && standing(conf) {
				continue
			}
			grp.MdCheck = &pb.MdCheck{
				NextEpoch:    edit.To.GetNextEpoch(),
				RequestEpoch: edit.To.GetRequestEpoch(),
				DoneEpoch:    edit.To.GetDoneEpoch(),
			}
			applied++
			if !containsHolder(changed, holder) {
				changed = append(changed, holder)
			}
		}
		if applied == 0 {
			return nil
		}
		for _, holder := range changed {
			holder.put(s)
		}
		revision, err = bumpSpRevTo(s, opUpdateMdChecks, shard, cid, spId)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return applied, revision, nil
}

// SameMdCheck reports whether two md check schedules are equal, an absent
// one reading as the zero schedule.
func SameMdCheck(a, b *pb.MdCheck) bool {
	return a.GetNextEpoch() == b.GetNextEpoch() &&
		a.GetRequestEpoch() == b.GetRequestEpoch() &&
		a.GetDoneEpoch() == b.GetDoneEpoch()
}
