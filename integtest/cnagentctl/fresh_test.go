package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// base is an arbitrary node time; at(ms) is base plus ms milliseconds.
var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func at(ms int) *timestamppb.Timestamp {
	return timestamppb.New(base.Add(time.Duration(ms) * time.Millisecond))
}

// reply builds a CheckCntlrReply from an observation answered at answer; obs
// or converge -1 leaves that time unset.
func reply(code uint32, obs, answer, converge int) *pb.CheckCntlrReply {
	stamp := &pb.ObsStamp{AnswerTime: at(answer), ObsTraceId: "t"}
	if obs >= 0 {
		stamp.ObsTime = at(obs)
	}
	if converge >= 0 {
		stamp.ConvergeStart = at(converge)
	}
	return &pb.CheckCntlrReply{
		AgentReply: &pb.AgentReply{Code: code},
		ObsStamp:   stamp,
	}
}

// boundedCtx ends a test's rounds in time, so a rule that never accepts fails
// the test instead of hanging it.
func boundedCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// scripted is a roundFn that hands out replies in order and then repeats
// the last one.
func scripted(replies ...*pb.CheckCntlrReply) (roundFn, *int) {
	n := 0
	return func() (stampedReply, error) {
		i := min(n, len(replies)-1)
		n++
		return replies[i], nil
	}, &n
}

func TestFreshRuleTakesTheSecondObservationAfterTheStart(t *testing.T) {
	steps := []struct {
		reply *pb.CheckCntlrReply
		want  bool
		why   string
	}{
		{reply(0, 50, 100, -1), false, "the first reply sets the start"},
		{reply(0, 105, 200, -1), false,
			"the first observation after the start may have begun before it"},
		{reply(0, 105, 300, -1), false, "the same observation again"},
		{reply(0, 350, 400, 360), false, "a converge of the object runs"},
		{reply(0, 450, 500, -1), true, "newer, and no converge runs"},
	}
	var rule freshRule
	for i, step := range steps {
		if got := rule.accept(step.reply); got != step.want {
			t.Fatalf("round %d (%s): accept = %v, want %v",
				i+1, step.why, got, step.want)
		}
	}
	if !rule.start.Equal(base.Add(100*time.Millisecond)) ||
		!rule.firstAfter.Equal(base.Add(105*time.Millisecond)) {
		t.Fatalf("start %v, first after %v", rule.start, rule.firstAfter)
	}
}

func TestFreshRuleCountsAnObservationStoredUnderAConvergeAsTheFirst(t *testing.T) {
	var rule freshRule
	for i, step := range []struct {
		reply *pb.CheckCntlrReply
		want  bool
	}{
		{reply(0, 50, 100, -1), false},
		// A converge stored it and its mark has not ended yet.
		{reply(0, 150, 200, 120), false},
		{reply(0, 250, 300, -1), true},
	} {
		if got := rule.accept(step.reply); got != step.want {
			t.Fatalf("round %d: accept = %v, want %v", i+1, got, step.want)
		}
	}
}

func TestFreshRuleNeverTakesAnObservationFromBeforeTheStart(t *testing.T) {
	var rule freshRule
	for i, r := range []*pb.CheckCntlrReply{
		reply(0, 50, 100, -1),
		reply(0, 60, 200, -1),
		reply(0, 100, 300, -1),
		reply(0, -1, 400, -1),
		reply(0, 100, 500, -1),
	} {
		if rule.accept(r) {
			t.Fatalf("round %d accepted an observation not after the start", i+1)
		}
	}
}

func TestFreshRuleWaitsOnAnObjectWithNoObservation(t *testing.T) {
	var rule freshRule
	for i, r := range []*pb.CheckCntlrReply{
		reply(0, -1, 100, -1),
		reply(0, -1, 200, -1),
		reply(0, 250, 300, -1),
		reply(0, 350, 400, -1),
	} {
		if got, want := rule.accept(r), i == 3; got != want {
			t.Fatalf("round %d: accept = %v, want %v", i+1, got, want)
		}
	}
}

func TestFreshRuleTakesAnUnknownObjectAtOnce(t *testing.T) {
	var rule freshRule
	if !rule.accept(reply(common.ReplyCodeUnknownObject, -1, 100, -1)) {
		t.Fatal("an unknown object's reply was not taken at once")
	}
}

func TestFreshRoundReturnsTheAcceptedReply(t *testing.T) {
	want := reply(4, 450, 500, -1)
	round, n := scripted(reply(0, 50, 100, -1), reply(0, 150, 200, -1),
		reply(0, 150, 300, 250), want, reply(0, 900, 1000, -1))
	var rule freshRule
	got, err := freshRound(boundedCtx(t), round, time.Millisecond, &rule)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || *n != 4 || rule.rounds != 4 {
		t.Fatalf("got %v after %d rounds (rule %d), want the 4th reply",
			got, *n, rule.rounds)
	}
}

func TestFreshRoundEndsAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(),
		50*time.Millisecond)
	defer cancel()
	// The observation never changes: a converge holds it back.
	round, n := scripted(reply(0, 50, 100, -1), reply(0, 150, 200, -1),
		reply(0, 150, 300, 250))
	var rule freshRule
	_, err := freshRound(ctx, round, time.Millisecond, &rule)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if *n < 3 || rule.last.GetConvergeStart() == nil {
		t.Fatalf("%d rounds, last stamp %v", *n, rule.last)
	}
}

func TestFreshRoundFailsOnAFailedRound(t *testing.T) {
	boom := errors.New("boom")
	var rule freshRule
	_, err := freshRound(boundedCtx(t), func() (stampedReply, error) {
		return nil, boom
	}, time.Millisecond, &rule)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the round's error", err)
	}
}

func TestSamplerTakesTheFreshReplyThenEachNewerObservation(t *testing.T) {
	round, n := scripted(
		reply(0, 50, 100, -1),
		reply(0, 150, 200, -1),
		reply(0, 250, 300, -1),  // the fresh one: sample 1
		reply(0, 250, 400, -1),  // the same observation: no sample
		reply(0, 450, 500, -1),  // sample 2
		reply(0, 550, 600, 520), // newer, though a converge runs: sample 3
	)
	s := &sampler{ctx: boundedCtx(t), round: round,
		interval: time.Millisecond, period: time.Millisecond}
	for i, want := range []int{300, 500, 600} {
		got, err := s.next()
		if err != nil {
			t.Fatal(err)
		}
		if answer := got.GetObsStamp().GetAnswerTime().AsTime(); !answer.Equal(
			base.Add(time.Duration(want) * time.Millisecond)) {
			t.Fatalf("sample %d answered at %v, want +%dms", i+1, answer, want)
		}
	}
	if *n != 6 {
		t.Fatalf("%d rounds, want 6", *n)
	}
}

func TestSamplerHandsOverAnUnknownObject(t *testing.T) {
	round, _ := scripted(
		reply(0, 50, 100, -1),
		reply(0, 150, 200, -1),
		reply(0, 250, 300, -1),
		reply(common.ReplyCodeUnknownObject, -1, 400, -1),
	)
	s := &sampler{ctx: boundedCtx(t), round: round,
		interval: time.Millisecond, period: time.Millisecond}
	if _, err := s.next(); err != nil {
		t.Fatal(err)
	}
	got, err := s.next()
	if err != nil {
		t.Fatal(err)
	}
	if got.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code %d, want unknown object", got.GetAgentReply().GetCode())
	}
}

func TestSamplerEndsAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(),
		50*time.Millisecond)
	defer cancel()
	round, _ := scripted(reply(0, 50, 100, -1), reply(0, 150, 200, -1),
		reply(0, 250, 300, -1))
	s := &sampler{ctx: ctx, round: round,
		interval: time.Millisecond, period: time.Millisecond}
	if _, err := s.next(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.next(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

// fakeStream records what each round sends and answers from scripted.
type fakeStream struct {
	grpc.ClientStream
	sent    []*pb.CheckCntlrRequest
	replies []*pb.CheckCntlrReply
}

func (f *fakeStream) Send(req *pb.CheckCntlrRequest) error {
	f.sent = append(f.sent, req)
	return nil
}

func (f *fakeStream) Recv() (*pb.CheckCntlrReply, error) {
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func TestStreamRoundsSendsOneRequestEveryRound(t *testing.T) {
	stream := &fakeStream{replies: []*pb.CheckCntlrReply{
		reply(0, 50, 100, -1), reply(0, 150, 200, -1)}}
	req := &pb.CheckCntlrRequest{ShowInfo: true, TraceId: "stage"}
	round := streamRounds[pb.CheckCntlrRequest, pb.CheckCntlrReply](stream, req)
	for i := range 2 {
		got, err := round()
		if err != nil {
			t.Fatal(err)
		}
		if got.GetObsStamp().GetAnswerTime().AsTime() !=
			base.Add(time.Duration(100*(i+1))*time.Millisecond) {
			t.Fatalf("round %d got reply %v", i+1, got)
		}
	}
	if len(stream.sent) != 2 || stream.sent[0] != req || stream.sent[1] != req {
		t.Fatalf("sent %v, want the one request twice", stream.sent)
	}
}

func TestObsAgeText(t *testing.T) {
	if got := obsAgeText(reply(0, -1, 100, -1).GetObsStamp()); got !=
		"no observation yet" {
		t.Fatalf("got %q", got)
	}
	if got := obsAgeText(reply(0, 100, 1600, 1200).GetObsStamp()); got !=
		`observation 1.5s old (trace "t"), converge running since `+
			"2026-01-02T03:04:06.2Z" {
		t.Fatalf("got %q", got)
	}
}

// scriptedAgent serves CheckCntlr from a script of replies, one per request,
// and records every request it got.
type scriptedAgent struct {
	pb.UnimplementedControllerNodeAgentServer
	replies []*pb.CheckCntlrReply
	got     []*pb.CheckCntlrRequest
}

func (a *scriptedAgent) CheckCntlr(
	stream grpc.BidiStreamingServer[pb.CheckCntlrRequest, pb.CheckCntlrReply],
) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		a.got = append(a.got, req)
		i := min(len(a.got), len(a.replies)) - 1
		if err := stream.Send(a.replies[i]); err != nil {
			return err
		}
	}
}

func TestFreshRoundOverACheckStream(t *testing.T) {
	agent := &scriptedAgent{replies: []*pb.CheckCntlrReply{
		reply(0, 50, 100, -1),
		reply(0, 150, 200, -1),
		reply(0, 150, 300, -1),
		reply(0, 350, 400, -1),
		reply(0, 450, 500, -1),
	}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterControllerNodeAgentServer(srv, agent)
	go srv.Serve(lis)
	defer srv.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := pb.NewControllerNodeAgentClient(conn).CheckCntlr(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rule freshRule
	got, err := freshRound(ctx, streamRounds(stream, &pb.CheckCntlrRequest{
		CnId: 7, ShowInfo: true, TraceId: "stage",
	}), time.Millisecond, &rule)
	if err != nil {
		t.Fatal(err)
	}
	if answer := got.GetObsStamp().GetAnswerTime().AsTime(); !answer.Equal(
		base.Add(400 * time.Millisecond)) {
		t.Fatalf("took the reply answered at %v, want the 4th", answer)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if len(agent.got) != 4 {
		t.Fatalf("the agent got %d rounds, want 4", len(agent.got))
	}
	for i, req := range agent.got {
		if req.GetTraceId() != "stage" || !req.GetShowInfo() ||
			req.GetCnId() != 7 {
			t.Fatalf("round %d sent %v", i+1, req)
		}
	}
}
