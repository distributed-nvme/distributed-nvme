package model

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// The fence-last leg of a group (Group.fence_last_leg_id): the sp worker
// writes its reading of the primary's report, so that a worker that has none
// of its own, after a restart or a handoff, still fences that leg last
// (dnv-worker.md RW24). No agent reads it.

// The Op name of the write.
const opSetFenceLastLegs = "SetFenceLastLegs"

// FenceLastEdit is one group's new fence-last leg, zero for none.
type FenceLastEdit struct {
	Ref   GrpRef
	LegId uint64
}

// SetFenceLastLegs writes the fence-last leg of every group edits names in
// one STM, and returns how many groups it changed. It bumps no revision: no
// agent reads the field, and the worker that wrote it holds the reading in
// memory. It takes no expected revision and checks no level: the field only
// records what the primary reported, and a later report writes it again. A
// ref that is stale is skipped, never refused, as MarkGroupsArrayMade skips
// one: a parent or a group that is gone, and a clone being deleted.
func SetFenceLastLegs(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	edits []FenceLastEdit,
) (int, error) {
	set := 0
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		set = 0
		holders := make(map[GrpParent]*grpHolder)
		var changed []*grpHolder
		for _, edit := range edits {
			holder, seen := holders[edit.Ref.Parent]
			if !seen {
				var err error
				holder, err = loadGrpHolder(
					s, opSetFenceLastLegs, cid, spId, edit.Ref.Parent)
				if err != nil {
					holder = nil
				}
				holders[edit.Ref.Parent] = holder
			}
			if holder == nil || holder.deleting() {
				continue
			}
			grp := holder.group(edit.Ref.GrpId)
			if grp == nil || grp.GetFenceLastLegId() == edit.LegId {
				continue
			}
			grp.FenceLastLegId = edit.LegId
			set++
			if !containsHolder(changed, holder) {
				changed = append(changed, holder)
			}
		}
		for _, holder := range changed {
			holder.put(s)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return set, nil
}
