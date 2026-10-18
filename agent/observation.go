package agent

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Observation is the real state of one object as its agent last read it
// (dnagent.md SH28): the rows, the code that says whether the object is clean
// or what is left over, the revision the agent held when the reading started,
// the time by the node's clock when it ended, and the trace id of the call
// whose converge or read made it. Once stored it is never changed, so a reply
// hands out its info as it is.
type Observation[T proto.Message] struct {
	Info     T
	Reply    *pb.AgentReply
	Revision uint64
	Time     time.Time
	TraceId  string
}

// Stamp is the ObsStamp of a reply that answers from o (SH28): o's time and
// trace id, the agent's clock now, and convergeStart, the start of the
// object's converge that runs now, zero when none runs. A zero time is left
// unset.
func (o *Observation[T]) Stamp(now, convergeStart time.Time) *pb.ObsStamp {
	return &pb.ObsStamp{
		ObsTime:       Timestamp(o.Time),
		ObsTraceId:    o.TraceId,
		AnswerTime:    Timestamp(now),
		ConvergeStart: Timestamp(convergeStart),
	}
}

// Timestamp turns a time into its protobuf form, nil for the zero time, so an
// unknown time travels unset rather than as the Unix epoch.
func Timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// ObsSlot holds one object's latest observation and the background read of
// it that runs, if any (SH28, SH29). It has no lock of its own: the role
// server's leaf lock guards it, the same lock its ConvergeMarks live under,
// so that a round's test "no read and no converge of the object runs" and the
// start of its read are one step against a syncup's mark and its cancel of
// the read (SH30). Every method is called with that lock held.
type ObsSlot[T proto.Message] struct {
	obs Observation[T]
	// readCtx is the running read's ctx, nil when none runs; cancel cancels
	// it. A cancelled read stays the running one until it ends, so no second
	// read starts beside it.
	readCtx context.Context
	cancel  context.CancelFunc
}

// LatestLocked returns the latest observation, the zero one while none has
// been stored.
func (s *ObsSlot[T]) LatestLocked() Observation[T] {
	return s.obs
}

// ReadingLocked reports whether a read of the object runs.
func (s *ObsSlot[T]) ReadingLocked() bool {
	return s.readCtx != nil
}

// BeginReadLocked marks a read as running and returns its ctx, derived from
// parent; CancelReadLocked cancels it. The caller has found that no read runs
// (ReadingLocked) and ends the read with EndReadLocked on every path.
func (s *ObsSlot[T]) BeginReadLocked(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	s.readCtx, s.cancel = ctx, cancel
	return ctx
}

// CancelReadLocked cancels the running read, if any. The read ends at its
// current command and stores nothing (WriteLocked).
func (s *ObsSlot[T]) CancelReadLocked() {
	if s.cancel != nil {
		s.cancel()
	}
}

// EndReadLocked ends the read run under ctx: when it is the running read, the
// object has no read running any more. A read the caller ran itself, not
// begun by BeginReadLocked, ends nothing. The read stores its observation
// (WriteLocked) before it ends, in the same hold of the lock, since ending
// cancels its ctx.
func (s *ObsSlot[T]) EndReadLocked(ctx context.Context) {
	if s.readCtx == nil || s.readCtx != ctx {
		return
	}
	s.cancel()
	s.readCtx, s.cancel = nil, nil
}

// WriteLocked stores obs as the latest observation unless ctx, the ctx of the
// read or the converge that made it, is done: a cancelled read and a converge
// whose call was cut off store nothing (SH29, SH28). The time is kept
// strictly increasing for the object: a time that is not later than the
// stored one becomes one nanosecond after it, so two observations never share
// a time and equal times mean the same observation. It reports whether it
// stored obs.
func (s *ObsSlot[T]) WriteLocked(ctx context.Context, obs Observation[T]) bool {
	if ctx.Err() != nil {
		return false
	}
	// The wall clock alone: the stored times travel as timestamps, which
	// carry no monotonic reading to order them by.
	obs.Time = obs.Time.Round(0)
	if !obs.Time.After(s.obs.Time) {
		obs.Time = s.obs.Time.Add(time.Nanosecond)
	}
	s.obs = obs
	return true
}

// ConvergeMarks records the converges in flight (SH30): for each object key,
// how many syncups and background converges of the object have begun and not
// yet ended, and since when one has been in flight without a break; and how
// many node-level syncups that mark the node are in flight. A syncup marks
// itself when it arrives, before it cancels a read and waits for its locks,
// unless a gate refuses it on the request the agent holds; a background
// converge marks itself before it waits for the object lock. No read starts
// while a mark of its object, or of its node, stands. It has no lock of its
// own: the role server's leaf lock guards it, as it guards the ObsSlots.
type ConvergeMarks struct {
	objs map[string]*convergeMark
	node int
}

type convergeMark struct {
	n     int
	since time.Time
}

// NewConvergeMarks builds an empty set.
func NewConvergeMarks() *ConvergeMarks {
	return &ConvergeMarks{objs: make(map[string]*convergeMark)}
}

// BeginLocked marks one converge of key in flight from now on.
func (c *ConvergeMarks) BeginLocked(key string, now time.Time) {
	m := c.objs[key]
	if m == nil {
		m = &convergeMark{since: now}
		c.objs[key] = m
	}
	m.n++
}

// EndLocked ends one converge of key that BeginLocked marked.
func (c *ConvergeMarks) EndLocked(key string) {
	m := c.objs[key]
	if m == nil {
		return
	}
	m.n--
	if m.n <= 0 {
		delete(c.objs, key)
	}
}

// BeginNodeLocked marks one node-level syncup in flight.
func (c *ConvergeMarks) BeginNodeLocked() {
	c.node++
}

// EndNodeLocked ends one node-level syncup that BeginNodeLocked marked.
func (c *ConvergeMarks) EndNodeLocked() {
	if c.node > 0 {
		c.node--
	}
}

// SinceLocked is when the converges of key in flight began to be in flight
// without a break, the start a reply reports: the time since which no read of
// the object could start, zero when none is in flight.
func (c *ConvergeMarks) SinceLocked(key string) time.Time {
	if m := c.objs[key]; m != nil {
		return m.since
	}
	return time.Time{}
}

// QuietLocked reports whether no converge of key and no node-level syncup is
// in flight, the condition a round's read needs besides no read running.
func (c *ConvergeMarks) QuietLocked(key string) bool {
	return c.objs[key] == nil && c.node == 0
}
