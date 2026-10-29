package worker

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// reactCoveredBlocks is what the fixture's one data group holds: the blocks
// of a two-extent group, which is the need a thin device of exactly that
// many blocks per slice has in the fixture's one slice.
func reactCoveredBlocks(t *testing.T) uint64 {
	t.Helper()
	return reactDataBlocks(t, 2)
}

// TestReactionDataGrowStopsWhenCovered pins AR6's covered rule: a data grow
// is skipped once the slice's data groups hold what the pool's thin devices
// can use in it, with no scan and with the pass going on to the other
// reactions, as past a full group list; one block of need beyond that and
// the grow runs; a thin device the state lists without a record holds the
// rule off; a snapshot counts in full; and a covered slice still grows its
// metadata.
func TestReactionDataGrowStopsWhenCovered(t *testing.T) {
	t.Run("covered: skipped, no scan, the pass goes on", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		held := reactCoveredBlocks(t)
		h.state.Tds[0].Size = held * reactBlockSize
		// Above the mark, and not pending: the one group's blocks are the
		// whole total, so the covered rule is the only thing holding it.
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, held*90/100, held))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_repair = %+v, want the data group", calls[0])
		}
		// The covered grow ran no scan: the one scan is the repair's.
		if queries := h.rops.allQueries(); len(queries) != 1 ||
			queries[0].requiredCnt != 1 {
			t.Fatalf("queries = %+v, want only the repair's scan", queries)
		}
		h.wantSkippedBy(reactionGrowData, reasonDataCovered, "")
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("one block of need past the held blocks grows", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		held := reactCoveredBlocks(t)
		h.state.Tds[0].Size = (held + 1) * reactBlockSize
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, held*90/100, held))
		h.pass()
		calls := h.wantOps("grow")
		if calls[0].isMeta || calls[0].sliceId != reactSliceId {
			t.Fatalf("grow = %+v, want a data grow of the fixture's slice",
				calls[0])
		}
		h.wantApplied(reactionGrowData)
	})

	t.Run("a listed thin device without a record holds the rule off",
		func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.dnCands(reactDnC, reactDnD)
			held := reactCoveredBlocks(t)
			h.state.Tds[0].Size = held * reactBlockSize
			// td1 is listed but its record is missing from the state: a need
			// computed from td0 alone would undercount, so the grow runs.
			h.state.Conf.TdNameList = append(h.state.Conf.TdNameList, "td1")
			h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
				poolLine(1, 1000, held*90/100, held))
			h.pass()
			h.wantOps("grow")
			h.wantApplied(reactionGrowData)
		})

	t.Run("a snapshot counts in full", func(t *testing.T) {
		held := reactCoveredBlocks(t)
		// td0 and its snapshot td1 together need exactly the held blocks:
		// covered. One more block on the snapshot: not covered.
		for _, tc := range []struct {
			name     string
			snapshot uint64
			covered  bool
		}{
			{"origin and snapshot fill the groups", held - 60, true},
			{"the snapshot needs one block more", held - 59, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newReactHarness(t, reactFixture(t))
				h.dnCands(reactDnC, reactDnD)
				h.state.Conf.TdNameList = []string{"td0", "td1"}
				h.state.TdNames = []string{"td0", "td1"}
				h.state.Tds = []*pb.ThinDevice{
					{TdId: 500, DevId: 1, Size: 60 * reactBlockSize},
					{TdId: 501, DevId: 2, OriId: 1,
						Size: tc.snapshot * reactBlockSize},
				}
				h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
					poolLine(1, 1000, held*90/100, held))
				h.pass()
				if tc.covered {
					h.wantOps()
					h.wantSkippedBy(reactionGrowData, reasonDataCovered, "")
					return
				}
				h.wantOps("grow")
				h.wantApplied(reactionGrowData)
			})
		}
	})

	t.Run("a covered slice still grows its metadata", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		held := reactCoveredBlocks(t)
		h.state.Tds[0].Size = held * reactBlockSize
		// Both kinds breach the mark: the data grow is covered, the metadata
		// grow is not AR6's covered rule's business and runs.
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(900, 1000, held*90/100, held))
		h.pass()
		calls := h.wantOps("grow")
		if !calls[0].isMeta {
			t.Fatalf("grow = %+v, want a meta grow", calls[0])
		}
		h.wantSkippedBy(reactionGrowData, reasonDataCovered, "")
		h.wantApplied(reactionGrowMeta)
	})
}
