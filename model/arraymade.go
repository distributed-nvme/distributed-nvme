package model

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// The flag that a group's array has run (Group.array_made): the sp worker
// sets it from the primary's report (dnv-worker.md AR15), and the primary
// never creates an array for a group that carries it (cnagent.md CN12, case
// 1).

// The Op name of the flag's write.
const opMarkGroupsArrayMade = "MarkGroupsArrayMade"

// GrpRef names one group by its parent and its id.
type GrpRef struct {
	Parent GrpParent
	GrpId  uint64
}

// MarkGroupsArrayMade sets array_made on every group refs names that does
// not carry it yet, in one STM with one SpRev bump, and returns how many it
// set and the revision its bump wrote, zero when it set none and wrote
// nothing. It takes no expected revision: the flag only records that the
// array ran, which no later write of the pool makes false, so a pool that
// changed since the caller's snapshot is no reason to refuse. A ref that is
// stale is skipped, never refused, as ClearForcedStart skips one: a parent or
// a group that is gone, and a clone being deleted, whose group goes back in
// the clone drain's final transaction (dnv-worker.md CLD9). The flag rides
// to every cntlr in the group's parent record, so the write raises SpRev.
func MarkGroupsArrayMade(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	refs []GrpRef,
) (int, uint64, error) {
	set, revision := 0, uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		set, revision = 0, 0
		if _, err := loadSpConfAnyLevel(
			s, opMarkGroupsArrayMade, cid, spName, spId,
		); err != nil {
			return err
		}
		holders := make(map[GrpParent]*grpHolder)
		var changed []*grpHolder
		for _, ref := range refs {
			holder, seen := holders[ref.Parent]
			if !seen {
				var err error
				holder, err = loadGrpHolder(
					s, opMarkGroupsArrayMade, cid, spId, ref.Parent)
				if err != nil {
					holder = nil
				}
				holders[ref.Parent] = holder
			}
			if holder == nil || holder.deleting() {
				continue
			}
			grp := holder.group(ref.GrpId)
			if grp == nil || grp.GetArrayMade() {
				continue
			}
			grp.ArrayMade = true
			set++
			if !containsHolder(changed, holder) {
				changed = append(changed, holder)
			}
		}
		if set == 0 {
			return nil
		}
		for _, holder := range changed {
			holder.put(s)
		}
		var err error
		revision, err = bumpSpRevTo(
			s, opMarkGroupsArrayMade, shard, cid, spId)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return set, revision, nil
}

// containsHolder reports whether holders holds h.
func containsHolder(holders []*grpHolder, h *grpHolder) bool {
	for _, have := range holders {
		if have == h {
			return true
		}
	}
	return false
}
