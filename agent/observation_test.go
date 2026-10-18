package agent

import (
	"context"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// SH28: an observation's time is strictly increasing for its object, so equal
// times mean the same observation, whatever the clock the writer read.
func TestObsSlotTimesStrictlyIncrease(t *testing.T) {
	var slot ObsSlot[*pb.DnInfo]
	ctx := context.Background()
	at := time.Unix(1000, 0)
	if !slot.WriteLocked(ctx, Observation[*pb.DnInfo]{Time: at, Revision: 1}) {
		t.Fatal("the first write stored nothing")
	}
	// The same clock reading twice, then one from before it.
	for i, when := range []time.Time{at, at.Add(-time.Second)} {
		prev := slot.LatestLocked().Time
		if !slot.WriteLocked(ctx, Observation[*pb.DnInfo]{Time: when, Revision: 2}) {
			t.Fatalf("write %d stored nothing", i)
		}
		got := slot.LatestLocked()
		if !got.Time.After(prev) {
			t.Fatalf("write %d: time %v is not after %v", i, got.Time, prev)
		}
		if got.Revision != 2 {
			t.Fatalf("write %d: revision %d, want 2", i, got.Revision)
		}
	}
	later := at.Add(time.Hour)
	slot.WriteLocked(ctx, Observation[*pb.DnInfo]{Time: later})
	if got := slot.LatestLocked().Time; !got.Equal(later) {
		t.Fatalf("a later time was moved: %v, want %v", got, later)
	}
}

// SH29: a cancelled read stores nothing, stays the running read until it ends,
// and ending it frees the slot for the next one; a read the caller ran itself
// ends nothing.
func TestObsSlotReadLife(t *testing.T) {
	var slot ObsSlot[*pb.SideInfo]
	if slot.ReadingLocked() {
		t.Fatal("a new slot reads as reading")
	}
	ctx := slot.BeginReadLocked(context.Background())
	if !slot.ReadingLocked() {
		t.Fatal("a begun read is not running")
	}
	slot.CancelReadLocked()
	if slot.WriteLocked(ctx, Observation[*pb.SideInfo]{Time: time.Unix(5, 0)}) {
		t.Fatal("a cancelled read stored its observation")
	}
	if !slot.LatestLocked().Time.IsZero() {
		t.Fatal("a cancelled read left an observation")
	}
	if !slot.ReadingLocked() {
		t.Fatal("a cancelled read stopped counting as running before it ended")
	}
	slot.EndReadLocked(context.Background())
	if !slot.ReadingLocked() {
		t.Fatal("another ctx ended the running read")
	}
	slot.EndReadLocked(ctx)
	if slot.ReadingLocked() {
		t.Fatal("an ended read still runs")
	}

	ctx = slot.BeginReadLocked(context.Background())
	if !slot.WriteLocked(ctx, Observation[*pb.SideInfo]{Time: time.Unix(6, 0)}) {
		t.Fatal("a read that ran to its end stored nothing")
	}
	slot.EndReadLocked(ctx)
	if ctx.Err() == nil {
		t.Fatal("ending a read left its ctx live")
	}
}

// SH30: a mark stands from the first begin to the last end of an object's
// converges and reports the first begin's time; a node-level mark makes every
// object unquiet.
func TestConvergeMarks(t *testing.T) {
	c := NewConvergeMarks()
	if !c.QuietLocked("a") || !c.SinceLocked("a").IsZero() {
		t.Fatal("a new set is not quiet")
	}
	t1, t2 := time.Unix(10, 0), time.Unix(20, 0)
	c.BeginLocked("a", t1)
	c.BeginLocked("a", t2)
	if c.QuietLocked("a") || !c.SinceLocked("a").Equal(t1) {
		t.Fatalf("two converges in flight: quiet %v since %v",
			c.QuietLocked("a"), c.SinceLocked("a"))
	}
	if !c.QuietLocked("b") {
		t.Fatal("another object's mark made b unquiet")
	}
	c.EndLocked("a")
	if c.QuietLocked("a") {
		t.Fatal("one end cleared two marks")
	}
	c.EndLocked("a")
	if !c.QuietLocked("a") || !c.SinceLocked("a").IsZero() {
		t.Fatal("the last end left a mark")
	}
	c.EndLocked("a")
	c.BeginNodeLocked()
	if c.QuietLocked("b") {
		t.Fatal("a node-level syncup left b quiet")
	}
	if !c.SinceLocked("b").IsZero() {
		t.Fatal("a node-level syncup reads as a converge of b")
	}
	c.EndNodeLocked()
	c.EndNodeLocked()
	if !c.QuietLocked("b") {
		t.Fatal("the node mark outlived its end")
	}
}

// The stamp leaves a zero time unset.
func TestObservationStamp(t *testing.T) {
	var o Observation[*pb.CnInfo]
	now := time.Unix(30, 0)
	st := o.Stamp(now, time.Time{})
	if st.GetObsTime() != nil || st.GetConvergeStart() != nil {
		t.Fatalf("zero times travel set: %v", st)
	}
	if !st.GetAnswerTime().AsTime().Equal(now) {
		t.Fatalf("answer time %v, want %v", st.GetAnswerTime().AsTime(), now)
	}
	o.Time, o.TraceId = time.Unix(29, 5), "tid"
	st = o.Stamp(now, time.Unix(28, 0))
	if !st.GetObsTime().AsTime().Equal(o.Time) || st.GetObsTraceId() != "tid" ||
		!st.GetConvergeStart().AsTime().Equal(time.Unix(28, 0)) {
		t.Fatalf("stamp %v", st)
	}
}
