package worker

import (
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// startingReply is a DN round's answer from an agent whose startup pass
// runs (dnagent.md SH33): the starting code, no observation, and the
// agent's start beside the answer time.
func startingReply(start, at time.Time) *pb.CheckDnReply {
	return &pb.CheckDnReply{
		AgentReply: &pb.AgentReply{Code: common.ReplyCodeStarting},
		ObsStamp: &pb.ObsStamp{
			AnswerTime:     timestamppb.New(at),
			AgentStartTime: timestamppb.New(start),
		},
	}
}

// pastHold are the rounds a DN's loop failed as a start past its hold.
func pastHold(logs *logCapture) []map[string]any {
	var out []map[string]any
	for _, rec := range logs.withMsg("check round failed") {
		if rec["error"] == errStartingPastHold.Error() {
			out = append(out, rec)
		}
	}
	return out
}

// TestAStartIsAPause pins HL8 on a DN's loop, and HL7's pause it makes. A
// round the agent answers starting gives no verdict and begins a start
// series, logged once. The first round it answers otherwise ends the series,
// is logged with the start's length, and, as the first round after a pause,
// judges no observation the pass made, however old, rather than failing on
// its age; the round after it judges a new one. A series past the cluster's
// hold, a hold of zero, and an answer before the agent's start (the node's
// clock stepped back) each make the starting round a missed one, and a
// series past its hold still logs its end. A round whose agent start differs
// from the one the rounds last carried logs the agent's restart.
func TestAStartIsAPause(t *testing.T) {
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	t0 := testNodeBase
	start := t0.Add(roundInterval - ms)
	pass := t0.Add(2 * roundInterval) // an observation the pass made
	ended := t0.Add(3*roundInterval + bound + ms)
	for _, c := range []struct {
		name string
		// hold is the cluster's start hold in seconds.
		hold uint32
		// stepBack answers the starting rounds before the agent's start.
		stepBack bool
		// missed is how many starting rounds are missed rounds.
		missed int
	}{
		{name: "within the hold", hold: 120},
		{name: "past the hold", hold: 1, missed: 1},
		{name: "a hold of zero", hold: 0, missed: 2},
		{name: "a clock stepped back", hold: 120, stepBack: true, missed: 2},
		{name: "a hold of zero, the clock stepped back", hold: 0,
			stepBack: true, missed: 2},
	} {
		t.Run(c.name, inBubble(func(t *testing.T) {
			h := newRevHarness(t)
			var rounds atomic.Int64
			stub := &stubDnAgent{}
			stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				// Every reply stamps the agent's start, as a real agent's
				// does: the first round the process before the restart.
				ok := func(obs, at, began time.Time) *pb.CheckDnReply {
					stamp := stampAt(obs, at, "read")
					stamp.AgentStartTime = timestamppb.New(began)
					return &pb.CheckDnReply{
						Revision: req.GetRevision(),
						DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
						ObsStamp: stamp,
					}
				}
				began := start
				if c.stepBack {
					began = t0.Add(3 * roundInterval)
				}
				n := rounds.Add(1)
				switch n {
				case 1:
					return ok(t0.Add(-ms), t0, t0.Add(-time.Hour))
				case 2, 3:
					at := t0.Add(time.Duration(n-1) * roundInterval)
					return startingReply(began, at)
				case 4:
					return ok(pass, ended, began)
				default:
					at := ended.Add(time.Duration(n-4) * roundInterval)
					return ok(at.Add(-ms), at, began)
				}
			})
			h.fleet.addDn(t, testAddr, stub)
			h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
				cc.StartHoldConf = &pb.StartHoldConf{Seconds: c.hold}
			}))
			h.seedDnConf(testAddr, dnTestConf())
			_, rec, _ := h.startRecordedDn(testAddr, 5)

			h.advanceUntil("the start's end", roundInterval,
				func() bool { return rounds.Load() >= 4 })
			if got := len(pastHold(h.logs)); got != c.missed {
				t.Fatalf("%d starting rounds missed, want %d", got, c.missed)
			}
			if got := tooOld(h.logs); len(got) != 0 {
				t.Fatalf("the round after the start failed on its "+
					"observation's age: %v", got)
			}
			if got := len(h.logs.withMsg(msgAgentStarting)); got != 1 {
				t.Fatalf("%d %q records, want one", got, msgAgentStarting)
			}
			if got := len(h.logs.withMsg(msgAgentStarted)); got != 1 {
				t.Fatalf("%d %q records, want one", got, msgAgentStarted)
			}
			if got := rec.holdsEnded(); got != 0 {
				t.Fatalf("the hold ended %d times at the start's end, want "+
					"none before the round after it", got)
			}
			// The loop's first round is a pause's first round too (HL7).
			if got := roundsSeen(rec); got != 0 {
				t.Fatalf("observe saw %d rounds, want none: the round "+
					"after the start judges no observation of the pass", got)
			}
			h.advanceUntil("the round after", roundInterval,
				func() bool { return rounds.Load() >= 5 })
			h.advanceUntil("its verdict", roundInterval,
				func() bool { return roundsSeen(rec) >= 1 })
			if got := rec.holdsEnded(); got != 1 {
				t.Fatalf("the hold ended %d times by the round after the "+
					"start's end, want once", got)
			}
		}))
	}

	t.Run("a restart", inBubble(func(t *testing.T) {
		h := newRevHarness(t)
		var rounds atomic.Int64
		stub := &stubDnAgent{}
		stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			n := rounds.Add(1)
			at := t0.Add(time.Duration(n) * roundInterval)
			stamp := stampAt(at.Add(-ms), at, "read")
			stamp.AgentStartTime = timestamppb.New(t0)
			if n >= 3 {
				stamp.AgentStartTime = timestamppb.New(t0.Add(time.Hour))
			}
			return &pb.CheckDnReply{
				Revision: req.GetRevision(),
				DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
				ObsStamp: stamp,
			}
		})
		h.fleet.addDn(t, testAddr, stub)
		h.defaultConf()
		h.seedDnConf(testAddr, dnTestConf())
		h.startRecordedDn(testAddr, 5)
		h.advanceUntil("the fourth round", roundInterval,
			func() bool { return rounds.Load() >= 4 })
		if got := len(h.logs.withMsg(msgAgentRestarted)); got != 1 {
			t.Fatalf("%d %q records, want one", got, msgAgentRestarted)
		}
	}))
}

// TestAJudgedRoundClosesTheStartSeries pins the end of a start series
// (HL8): once a round after a start is judged, a later start begins a new
// series, held from its own start, so a planned restart long after an
// earlier one is not judged by that earlier start.
func TestAJudgedRoundClosesTheStartSeries(t *testing.T) {
	inBubble(func(t *testing.T) {
		ms := time.Millisecond
		t0 := testNodeBase
		first := t0.Add(roundInterval - ms)
		second := t0.Add(time.Hour) // far past the hold from the first
		h := newRevHarness(t)
		var rounds atomic.Int64
		stub := &stubDnAgent{}
		stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			ok := func(obs, at, began time.Time) *pb.CheckDnReply {
				stamp := stampAt(obs, at, "read")
				stamp.AgentStartTime = timestamppb.New(began)
				return &pb.CheckDnReply{
					Revision: req.GetRevision(),
					DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
					ObsStamp: stamp,
				}
			}
			n := rounds.Add(1)
			at := t0.Add(time.Duration(n-1) * roundInterval)
			switch n {
			case 1:
				return ok(at.Add(-ms), at, t0.Add(-time.Hour))
			case 2:
				return startingReply(first, at)
			case 3:
				// The start ends: an observation of the pass, not judged.
				return ok(at.Add(-roundInterval), at, first)
			case 4, 5:
				// Judged rounds: fresh observations.
				return ok(at.Add(-ms), at, first)
			case 6:
				return startingReply(second, second.Add(ms))
			default:
				later := second.Add(time.Duration(n-6) * roundInterval)
				return ok(later.Add(-ms), later, second)
			}
		})
		h.fleet.addDn(t, testAddr, stub)
		h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
			cc.StartHoldConf = &pb.StartHoldConf{Seconds: 10}
		}))
		h.seedDnConf(testAddr, dnTestConf())
		h.startRecordedDn(testAddr, 5)
		h.advanceUntil("the second start", roundInterval,
			func() bool { return rounds.Load() >= 6 })
		if got := len(pastHold(h.logs)); got != 0 {
			t.Fatalf("%d starting rounds missed: the second start was "+
				"judged from the first", got)
		}
		if got := len(h.logs.withMsg(msgAgentStarting)); got != 2 {
			t.Fatalf("%d %q records, want one per series", got,
				msgAgentStarting)
		}
	})(t)
}
