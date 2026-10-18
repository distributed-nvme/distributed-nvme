package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A check round and an info read answer from the agent's latest observation
// of the object (dnagent.md SH28), and only a check round starts a background
// read of it, when no read and no converge of it runs (dnagent.md SH29). This
// suite runs no worker, so nothing else refreshes an observation: a stage
// that must see the node as it is now sends rounds of its own until one
// carries an observation read after the command began. That is the fresh mode
// of check-cn and check-cntlr, and the first sample of wait-hydrated.

// freshPeriod is the gap between two rounds of a fresh command.
const freshPeriod = 100 * time.Millisecond

// stampedReply is what every check reply carries: its code and the stamp of
// the observation it answers from.
type stampedReply interface {
	proto.Message
	GetAgentReply() *pb.AgentReply
	GetObsStamp() *pb.ObsStamp
}

// roundFn sends one round on the command's stream and returns its reply.
type roundFn func() (stampedReply, error)

// streamRounds sends req as every round of stream: one trace id and one
// revision for the whole command.
func streamRounds[Req, Rep any](
	stream grpc.BidiStreamingClient[Req, Rep], req *Req) roundFn {
	return func() (stampedReply, error) {
		if err := stream.Send(req); err != nil {
			return nil, fmt.Errorf("send: %w", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		return any(reply).(stampedReply), nil
	}
}

// stampTime is a stamp's time, the zero time when it is unset.
func stampTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// freshRule decides which reply of a fresh command is the fresh one. The
// command's start is the agent's answer time on its first reply, by the
// node's clock as every observation time is, never this machine's. An
// observation's time is when its reading ended, so the first one to end after
// the start may have begun before it — a read the previous command left
// running, say, which began before a stage's fault. One read of an object runs
// at a time, and a converge of it takes the object's locks only once a read
// has ended or, cancelled, stored nothing (dnagent.md SH29, SH30), so an
// observation newer than that first one was read entirely after the start:
// the rule takes the first reply whose observation is newer than the first
// observation that ended after the start, while no converge of the object
// runs. The cost is about one read more. An unknown object holds no
// observation to wait for, so its reply is taken at once.
type freshRule struct {
	rounds int
	// start is the first reply's answer time.
	start time.Time
	// firstAfter is the time of the first observation seen that ended after
	// start, zero until one is seen.
	firstAfter time.Time
	// last is the stamp of the latest reply.
	last *pb.ObsStamp
}

// accept takes one reply of the command, in order, and reports whether it is
// the fresh one.
func (f *freshRule) accept(reply stampedReply) bool {
	f.rounds++
	stamp := reply.GetObsStamp()
	f.last = stamp
	if reply.GetAgentReply().GetCode() == common.ReplyCodeUnknownObject {
		return true
	}
	if f.rounds == 1 {
		f.start = stampTime(stamp.GetAnswerTime())
	}
	// An unset observation time is the zero time, never after the start.
	obsTime := stampTime(stamp.GetObsTime())
	switch {
	case !obsTime.After(f.start):
		return false
	case f.firstAfter.IsZero():
		f.firstAfter = obsTime
		return false
	case !obsTime.After(f.firstAfter):
		return false
	}
	return stamp.GetConvergeStart() == nil
}

// describe names where the rule stands, for a failure message.
func (f *freshRule) describe() string {
	return fmt.Sprintf("%d rounds, start %s, first observation after it %s, "+
		"last stamp %s", f.rounds, timeText(f.start), timeText(f.firstAfter),
		stampText(f.last))
}

// timeText prints a node time, "unset" for the zero time.
func timeText(t time.Time) string {
	if t.IsZero() {
		return "unset"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// stampText prints a stamp's four fields.
func stampText(stamp *pb.ObsStamp) string {
	if stamp == nil {
		return "none"
	}
	return fmt.Sprintf("{obs_time %s, obs_trace_id %q, answer_time %s, "+
		"converge_start %s}",
		timeText(stampTime(stamp.GetObsTime())), stamp.GetObsTraceId(),
		timeText(stampTime(stamp.GetAnswerTime())),
		timeText(stampTime(stamp.GetConvergeStart())))
}

// obsAgeText says how old the observation a reply answers from was when the
// agent answered, by the node's clock, and whether a converge of the object
// ran then.
func obsAgeText(stamp *pb.ObsStamp) string {
	obsTime := stampTime(stamp.GetObsTime())
	if obsTime.IsZero() {
		return "no observation yet"
	}
	text := fmt.Sprintf("observation %s old (trace %q)",
		stampTime(stamp.GetAnswerTime()).Sub(obsTime), stamp.GetObsTraceId())
	if start := stamp.GetConvergeStart(); start != nil {
		text += ", converge running since " + timeText(start.AsTime())
	}
	return text
}

// noteStamp prints a reply's observation age as one human line on stderr:
// stdout stays exactly one protojson line, which callers parse with jq and
// the restart case diffs.
func noteStamp(name string, stamp *pb.ObsStamp) {
	fmt.Fprintf(os.Stderr, "cnagentctl: %s: %s\n", name, obsAgeText(stamp))
}

// pause waits d, or less when ctx ends first.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// freshRound sends a round every period until rule accepts a reply, and
// returns it. It fails when a round fails, which the stream's deadline, the
// command's --timeout, ends as well.
func freshRound(ctx context.Context, round roundFn, period time.Duration,
	rule *freshRule) (stampedReply, error) {
	for {
		reply, err := round()
		if err != nil {
			return nil, err
		}
		if rule.accept(reply) {
			return reply, nil
		}
		if err := pause(ctx, period); err != nil {
			return nil, err
		}
	}
}

// sampler hands a poller its samples of one object, from rounds on one
// stream that ask for the info every time: first the fresh reply, then each
// reply that carries a newer observation than the sample before it, so
// every sample is a reading of the node made after the poll began. Rounds
// start the reads (dnagent.md SH29); the kernel's progress — a hydration, an
// md resync — reaches no observation otherwise.
type sampler struct {
	ctx      context.Context
	round    roundFn
	interval time.Duration
	period   time.Duration
	rule     freshRule
	started  bool
	last     time.Time
}

// next returns the next sample: a reply with a newer observation, or an
// unknown object's reply.
func (s *sampler) next() (stampedReply, error) {
	if !s.started {
		reply, err := freshRound(s.ctx, s.round, s.period, &s.rule)
		if err != nil {
			return nil, err
		}
		s.started = true
		s.last = stampTime(reply.GetObsStamp().GetObsTime())
		return reply, nil
	}
	for {
		if err := pause(s.ctx, s.interval); err != nil {
			return nil, err
		}
		reply, err := s.round()
		if err != nil {
			return nil, err
		}
		s.rule.last = reply.GetObsStamp()
		if reply.GetAgentReply().GetCode() == common.ReplyCodeUnknownObject {
			return reply, nil
		}
		if obsTime := stampTime(reply.GetObsStamp().GetObsTime()); obsTime.After(s.last) {
			s.last = obsTime
			return reply, nil
		}
	}
}

// runCheck answers a check command: one round, or with fresh the fresh reply.
// It prints the reply, notes its observation's age on stderr, and checks its
// code — the fresh reply's alone, since the rounds before it carry older
// observations whose code may differ.
func (g *globals) runCheck(ctx context.Context, name string,
	round roundFn, closeSend func() error, fresh bool) {
	var reply stampedReply
	if fresh {
		var rule freshRule
		var err error
		reply, err = freshRound(ctx, round, freshPeriod, &rule)
		if err != nil {
			if ctx.Err() != nil {
				die("%s --fresh: no reply from an observation read after the "+
					"command began within %gs (%s)",
					name, g.timeout, rule.describe())
			}
			die("%s --fresh: round failed: %v (%s)",
				name, err, rule.describe())
		}
		fmt.Fprintf(os.Stderr, "cnagentctl: %s: fresh after %d rounds\n",
			name, rule.rounds)
	} else {
		var err error
		reply, err = round()
		if err != nil {
			die("%s failed: %v", name, err)
		}
	}
	if err := closeSend(); err != nil {
		die("%s close failed: %v", name, err)
	}
	emit(reply)
	noteStamp(name, reply.GetObsStamp())
	g.checkReply(reply.GetAgentReply())
}
