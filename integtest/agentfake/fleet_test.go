package agentfake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The dial seam of the worker's connection cache takes exactly this shape.
var _ func(addrPort string) (*grpc.ClientConn, error) = (*Fleet)(nil).Dial

// newTestFleet builds a fleet whose connections retry fast, so that a
// connection to a stopped agent comes back within the test's waits once the
// agent starts again.
func newTestFleet(t *testing.T, quiet bool) *Fleet {
	t.Helper()
	fleet := NewFleet(FleetConfig{
		Quiet: quiet,
		DialOptions: []grpc.DialOption{grpc.WithConnectParams(
			grpc.ConnectParams{
				Backoff: backoff.Config{
					BaseDelay:  20 * time.Millisecond,
					Multiplier: 1.2,
					MaxDelay:   100 * time.Millisecond,
				},
				MinConnectTimeout: time.Second,
			})},
	})
	t.Cleanup(fleet.Close)
	return fleet
}

// addAgent adds one agent to the fleet and fails the test on an error.
func addAgent(
	t *testing.T, fleet *Fleet, addrPort string, kind Kind, cfg Config,
) *Agent {
	t.Helper()
	agent, err := fleet.Add(addrPort, kind, cfg)
	if err != nil {
		t.Fatalf("Add(%s): %v", addrPort, err)
	}
	return agent
}

// dialFleet dials addrPort through the fleet and closes it at the end.
func dialFleet(t *testing.T, fleet *Fleet, addrPort string) *grpc.ClientConn {
	t.Helper()
	conn, err := fleet.Dial(addrPort)
	if err != nil {
		t.Fatalf("Dial(%s): %v", addrPort, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// boundCtx is a context the test's waits bound.
func boundCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// eventually retries call until it succeeds or the wait runs out.
func eventually(t *testing.T, what string, call func() error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := call()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkDnRound opens a CheckDn stream once the connection serves again and
// sends one round on it, returning the channel the round's outcome lands on
// and the stream's cancel. A unary call must succeed first: after a restart
// the connection may still hold the transport the stop closed, and a stream
// opened on it would read the close instead of the agent's answer.
func checkDnRound(
	t *testing.T, ctx context.Context, client pb.DiskNodeAgentClient,
) (chan error, context.CancelFunc) {
	t.Helper()
	eventually(t, "the connection serves", func() error {
		_, err := client.GetDnSize(ctx,
			&pb.GetDnSizeRequest{ClusterId: 1, DnId: 1})
		return err
	})
	rctx, cancel := context.WithCancel(ctx)
	stream, err := client.CheckDn(rctx)
	if err == nil {
		err = stream.Send(&pb.CheckDnRequest{
			ClusterId: 1, DnId: 1, Revision: 4,
		})
	}
	if err != nil {
		cancel()
		t.Fatalf("a CheckDn round: %v", err)
	}
	round := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		round <- err
	}()
	return round, cancel
}

// TestFleetServesByAddrPort pins the in-process serving: each address
// reaches the agent added there, which serves only its own kind's service;
// the fleet's dial fits the worker's connection seam and its context dialer
// fits a client of one's own; and a dial to an address where no agent
// serves succeeds while every call on it fails as unavailable, as one to a
// closed TCP port does.
func TestFleetServesByAddrPort(t *testing.T) {
	fleet := newTestFleet(t, false)
	dn := addAgent(t, fleet, "dn0:9520", Dn, Config{})
	cn := addAgent(t, fleet, "cn0:9520", Cn, Config{})
	if fleet.Agent("dn0:9520") != dn || fleet.Agent("cn0:9520") != cn ||
		fleet.Agent("dn9:9520") != nil {
		t.Fatal("Agent does not return the agent added at an address")
	}
	if _, err := fleet.Add("dn0:9520", Dn, Config{}); err == nil {
		t.Fatal("a second Add at a taken address succeeded")
	}
	ctx := boundCtx(t)

	dnConn := dialFleet(t, fleet, "dn0:9520")
	if _, err := pb.NewDiskNodeAgentClient(dnConn).SyncupDn(ctx,
		&pb.SyncupDnRequest{ClusterId: 1, DnId: 1, Revision: 3}); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := dn.Record(DnKey).Syncups; len(got) != 1 {
		t.Fatalf("the dn agent recorded %d syncups, want 1", len(got))
	}
	if got := cn.Calls(MethodSyncupDn); got != 0 {
		t.Fatalf("the cn agent counted %d SyncupDn calls, want 0", got)
	}
	if _, err := pb.NewControllerNodeAgentClient(dnConn).SyncupCn(ctx,
		&pb.SyncupCnRequest{ClusterId: 1, CnId: 1, Revision: 1}); status.
		Code(err) != codes.Unimplemented {
		t.Fatalf("SyncupCn on a dn agent: %v, want Unimplemented", err)
	}

	cnConn, err := grpc.NewClient("passthrough:///cn0:9520",
		grpc.WithContextDialer(fleet.DialContext),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cnConn.Close()
	reply, err := pb.NewControllerNodeAgentClient(cnConn).SyncupCn(ctx,
		&pb.SyncupCnRequest{ClusterId: 1, CnId: 1, Revision: 2})
	if err != nil || reply.GetRevision() != 2 {
		t.Fatalf("SyncupCn over the context dialer = %v, %v", reply, err)
	}

	noneConn := dialFleet(t, fleet, "dn9:9520")
	_, err = pb.NewDiskNodeAgentClient(noneConn).GetDnSize(ctx,
		&pb.GetDnSizeRequest{ClusterId: 1, DnId: 1})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("a call where no agent serves: %v, want Unavailable", err)
	}
}

// TestFleetStopAndStart pins the stop and the restart of one fleet member:
// a stop ends every open stream and a held syncup, unapplied, and fails new
// calls as unavailable; a start that keeps the store answers from the
// stored state, over the caller's own connection once it reconnects, with
// the counted levers re-armed and the observations and the rows' epochs
// begun afresh, so an old-observation lever set across the restart repeats
// nothing from before it; a start that drops it answers as an agent started
// on empty files, its sides unknown until a node syncup lists them; and the
// records survive both.
func TestFleetStopAndStart(t *testing.T) {
	fleet := newTestFleet(t, false)
	agent := addAgent(t, fleet, "dn0:9520", Dn, Config{})
	client := pb.NewDiskNodeAgentClient(dialFleet(t, fleet, "dn0:9520"))
	ctx := boundCtx(t)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, client, 4, ptr)
	syncupSide(t, client, ptr, 4, 0)

	stream, err := client.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	if err := stream.Send(&pb.CheckDnRequest{
		ClusterId: 1, DnId: 1, Revision: 4,
	}); err != nil {
		t.Fatalf("CheckDn send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("CheckDn recv: %v", err)
	}
	// A round under its own trace id, then an age far longer than the test:
	// for the rest of this lifetime every answer about the dn repeats that
	// round's observation whole, its disk row's epoch included.
	before := dnRound(t, client, 4, "before")
	beforeObs, _ := obsTimes(before.GetObsStamp())
	beforeEpoch := before.GetDnInfo().GetDiskInfo().GetEpoch()
	if beforeObs.IsZero() || beforeEpoch == 0 {
		t.Fatalf("the round before the stop = %v, want an observation and "+
			"a disk row", before)
	}
	update(t, agent, DnKey, func(ob *Object) {
		ob.ObsAge = time.Hour
		ob.HangSyncup = true
	})
	held := make(chan error, 1)
	go func() {
		_, err := client.SyncupDn(ctx, &pb.SyncupDnRequest{
			ClusterId: 1, DnId: 1, Revision: 5,
			SidePointerList: []*pb.SidePointer{ptr},
		})
		held <- err
	}()
	wantHeld(t, "the held syncup", held)

	agent.Stop()
	if err := wantReturned(t, "the held syncup", held); status.Code(err) !=
		codes.Unavailable {
		t.Fatalf("the held syncup at the stop: %v, want Unavailable", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("the open stream at the stop: %v, want Unavailable", err)
	}
	if _, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	}); status.Code(err) != codes.Unavailable {
		t.Fatalf("a call while stopped: %v, want Unavailable", err)
	}
	agent.Stop()

	update(t, agent, DnKey, func(ob *Object) {
		ob.HangSyncup = false
		ob.HangRounds = 1
	})
	if err := agent.Start(KeepStore); err != nil {
		t.Fatalf("Start(KeepStore): %v", err)
	}
	if err := agent.Start(KeepStore); err == nil {
		t.Fatal("a second Start of a serving agent succeeded")
	}
	eventually(t, "GetSideInfo after the keeping start", func() error {
		reply, err := client.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr,
		})
		if err == nil && (reply.GetAgentReply().GetCode() != 0 ||
			reply.GetRevision() != 4) {
			t.Fatalf("GetSideInfo after the keeping start = %v, want the "+
				"stored revision 4", reply)
		}
		return err
	})
	// The held syncup the stop ended stays unapplied, though its lever was
	// cleared before the start. The first answer about the dn in the new
	// lifetime is a new observation, an hour old under the age, under its
	// own trace id, not the round before the stop repeated; and its disk
	// row's epoch, the time of the row's last status change, is one of the
	// new lifetime, for which the test waits out the second it began in.
	for time.Now().Unix() <= int64(beforeEpoch) {
		time.Sleep(10 * time.Millisecond)
	}
	reply, err := client.GetDnInfo(metadata.AppendToOutgoingContext(ctx,
		common.TraceIdMetadataKey, "after"), &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil || reply.GetRevision() != 4 {
		t.Fatalf("GetDnInfo after the keeping start = %v, %v; want the "+
			"stored revision 4, not the held syncup's 5", reply, err)
	}
	obs, answered := obsTimes(reply.GetObsStamp())
	if trace := reply.GetObsStamp().GetObsTraceId(); trace != "after" ||
		obs.Equal(beforeObs) || answered.Sub(obs) != time.Hour {
		t.Fatalf("the first dn answer after the start: obs %v %q answered "+
			"%v; want a new observation an hour before the answer, not the "+
			"one of %v %q", obs, trace, answered, beforeObs, "before")
	}
	if epoch := reply.GetDnInfo().GetDiskInfo().GetEpoch(); epoch <=
		beforeEpoch {
		t.Fatalf("the disk row's epoch after the start = %d, want one later "+
			"than the %d of the lifetime before", epoch, beforeEpoch)
	}
	update(t, agent, DnKey, func(ob *Object) { ob.ObsAge = 0 })
	if got := agent.Record(DnKey).Syncups; len(got) != 2 {
		t.Fatalf("the dn record holds %d syncups after the restart, want "+
			"both received", len(got))
	}
	// The armed round is spent on the first round after the start, and the
	// round after it is answered; a restart arms it again, as a restarted
	// program arms it from its behavior file.
	round, cancel := checkDnRound(t, ctx, client)
	wantHeld(t, "the round hang_rounds counts", round)
	cancel()
	wantReturned(t, "the counted round", round)
	round, cancel = checkDnRound(t, ctx, client)
	if err := wantReturned(t, "the round after it", round); err != nil {
		t.Fatalf("the round after the counted one: %v", err)
	}
	cancel()
	agent.Stop()
	if err := agent.Start(KeepStore); err != nil {
		t.Fatalf("Start(KeepStore): %v", err)
	}
	round, cancel = checkDnRound(t, ctx, client)
	wantHeld(t, "the round the restart re-armed", round)
	cancel()
	wantReturned(t, "the re-armed round", round)

	agent.Stop()
	if err := agent.Start(DropStore); err != nil {
		t.Fatalf("Start(DropStore): %v", err)
	}
	eventually(t, "GetDnInfo after the dropping start", func() error {
		reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
			ClusterId: 1, DnId: 1,
		})
		if err == nil && (reply.GetRevision() != 0 ||
			reply.GetDnInfo() != nil) {
			t.Fatalf("GetDnInfo after the dropping start = %v, want "+
				"revision 0 and no info", reply)
		}
		return err
	})
	if code := syncupSide(t, client, ptr, 4, 0).GetAgentReply().
		GetCode(); code != common.ReplyCodeUnknownObject {
		t.Fatalf("SyncupSide after the dropping start: code %d, want the "+
			"side unknown", code)
	}
}

// TestStopEndsACallInFlight pins that a call in flight at a Stop acts no
// more, as a killed agent's call ends: a SyncupDn that has reached the
// agent but not yet its apply when the agent stops — held here in the
// front's reload — applies nothing and is not recorded, though it goes on
// only after a Start, so the restarted agent answers from what it kept or,
// started on empty files, from nothing; and one whose handler runs only
// after the restart, with its caller still waiting, is refused all the
// same, as is a call whose caller has already given up.
func TestStopEndsACallInFlight(t *testing.T) {
	// inFlight adds an agent whose front's reload holds the next request
	// that arrives while armed, and returns it with a release.
	type inFlight struct {
		agent   *Agent
		client  pb.DiskNodeAgentClient
		arm     func()
		arrived chan struct{}
		release func()
	}
	newInFlight := func(t *testing.T) *inFlight {
		t.Helper()
		f := &inFlight{arrived: make(chan struct{})}
		gate := make(chan struct{})
		var mu sync.Mutex
		armed, held := false, false
		f.arm = func() {
			mu.Lock()
			defer mu.Unlock()
			armed = true
		}
		f.release = sync.OnceFunc(func() { close(gate) })
		fleet := newTestFleet(t, true)
		f.agent = addAgent(t, fleet, "dn0:9520", Dn, Config{
			Refresh: func(context.Context) {
				mu.Lock()
				hold := armed && !held
				held = held || hold
				mu.Unlock()
				if hold {
					close(f.arrived)
					<-gate
				}
			},
		})
		t.Cleanup(f.release)
		f.client = pb.NewDiskNodeAgentClient(dialFleet(t, fleet, "dn0:9520"))
		syncupDn(t, f.client, 1)
		return f
	}
	revisionAfter := func(t *testing.T, f *inFlight) uint64 {
		t.Helper()
		var rev uint64
		eventually(t, "GetDnInfo after the start", func() error {
			reply, err := f.client.GetDnInfo(boundCtx(t),
				&pb.GetDnInfoRequest{ClusterId: 1, DnId: 1})
			rev = reply.GetRevision()
			return err
		})
		return rev
	}

	for _, tc := range []struct {
		name  string
		store Store
		want  uint64
	}{{"DropStore", DropStore, 0}, {"KeepStore", KeepStore, 1}} {
		t.Run("over the wire, "+tc.name,
			func(t *testing.T) {
				f := newInFlight(t)
				f.arm()
				done := make(chan *pb.SyncupDnReply, 1)
				failed := make(chan error, 1)
				go func() {
					reply, err := f.client.SyncupDn(context.Background(),
						&pb.SyncupDnRequest{
							ClusterId: 1, DnId: 1, Revision: 9,
						})
					if err != nil {
						failed <- err
						return
					}
					done <- reply
				}()
				<-f.arrived
				f.agent.Stop()
				if err := f.agent.Start(tc.store); err != nil {
					t.Fatalf("Start: %v", err)
				}
				f.release()
				select {
				case err := <-failed:
					if status.Code(err) != codes.Unavailable {
						t.Fatalf("the call in flight at the stop: %v, "+
							"want Unavailable", err)
					}
				case reply := <-done:
					t.Fatalf("the call in flight at the stop was "+
						"answered: %v", reply)
				case <-time.After(5 * time.Second):
					t.Fatal("the call in flight at the stop did not end")
				}
				if got := revisionAfter(t, f); got != tc.want {
					t.Fatalf("revision after the start = %d, want %d: the "+
						"call in flight at the stop applied", got, tc.want)
				}
				if got := len(f.agent.Record(DnKey).Syncups); got != 1 {
					t.Fatalf("the dn record holds %d syncups, want only "+
						"the one before the stop", got)
				}
			})
	}

	t.Run("a handler that runs after the restart", func(t *testing.T) {
		f := newInFlight(t)
		f.arm()
		returned := make(chan error, 1)
		go func() {
			// The handler itself, as gRPC runs it, under a ctx no stop
			// cancels: only the lifetime it arrived in tells it the agent
			// was stopped.
			_, err := f.agent.SyncupDn(context.Background(),
				&pb.SyncupDnRequest{ClusterId: 1, DnId: 1, Revision: 9})
			returned <- err
		}()
		<-f.arrived
		f.agent.Stop()
		if err := f.agent.Start(KeepStore); err != nil {
			t.Fatalf("Start: %v", err)
		}
		f.release()
		if err := wantReturned(t, "the handler", returned); status.
			Code(err) != codes.Unavailable {
			t.Fatalf("the handler of the stopped lifetime: %v, want "+
				"Unavailable", err)
		}
		if got := revisionAfter(t, f); got != 1 {
			t.Fatalf("revision after the start = %d, want the kept 1", got)
		}
	})

	t.Run("a call whose caller gave up", func(t *testing.T) {
		f := newInFlight(t)
		ptr := sidePtr(1, 3, 5)
		syncupDn(t, f.client, 1, ptr)
		syncupSide(t, f.client, ptr, 1, 30)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// A syncup, which a lever could hold, and a push, which none can.
		_, err := f.agent.SyncupDn(ctx,
			&pb.SyncupDnRequest{ClusterId: 1, DnId: 1, Revision: 9})
		if status.Code(err) != codes.Canceled {
			t.Fatalf("a syncup whose caller gave up: %v, want Canceled", err)
		}
		_, err = f.agent.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr, MigrId: 30, BmIdx: 0,
			Bitmap: []byte{1},
		})
		if status.Code(err) != codes.Canceled {
			t.Fatalf("a push whose caller gave up: %v, want Canceled", err)
		}
		if got := revisionAfter(t, f); got != 1 {
			t.Fatalf("revision = %d, want 1: the syncup applied", got)
		}
		if got := syncupSide(t, f.client, ptr, 1, 30).GetBmInfo().
			GetBmIdxList(); len(got) != 0 {
			t.Fatalf("the applied set = %v, want none: the push applied", got)
		}
		rec := f.agent.Record(SideKey(1, 3, 5))
		if len(f.agent.Record(DnKey).Syncups) != 2 || len(rec.Pushes) != 0 {
			t.Fatalf("records: %d dn syncups and %d pushes, want 2 and 0: "+
				"a call whose caller gave up is not received",
				len(f.agent.Record(DnKey).Syncups), len(rec.Pushes))
		}
	})
}

// TestFleetStopAndStartWithAStateFile pins the restart of a member whose
// stored state lives in a state file, as the program's does: a start that
// keeps the store reads the file as it finds it, a hand edit made while the
// agent was stopped included, and one that drops it empties the file.
func TestFleetStopAndStartWithAStateFile(t *testing.T) {
	fleet := newTestFleet(t, true)
	dir := t.TempDir()
	agent := addAgent(t, fleet, "cn0:9520", Cn, Config{Dir: dir})
	client := pb.NewControllerNodeAgentClient(dialFleet(t, fleet, "cn0:9520"))
	ctx := boundCtx(t)
	if _, err := client.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 3,
	}); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	agent.Stop()
	writeStateFile(t, agent, `{"objects": {"cn": {"revision": 8}}}`)
	if err := agent.Start(KeepStore); err != nil {
		t.Fatalf("Start(KeepStore): %v", err)
	}
	eventually(t, "GetCnInfo after the keeping start", func() error {
		reply, err := client.GetCnInfo(ctx,
			&pb.GetCnInfoRequest{ClusterId: 1, CnId: 1})
		if err == nil && reply.GetRevision() != 8 {
			t.Fatalf("GetCnInfo after the keeping start: revision %d, "+
				"want the file's 8", reply.GetRevision())
		}
		return err
	})

	agent.Stop()
	if err := agent.Start(DropStore); err != nil {
		t.Fatalf("Start(DropStore): %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, StateFileName))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	parsed := &stateFile{}
	if err := json.Unmarshal(raw, parsed); err != nil ||
		len(parsed.Objects) != 0 {
		t.Fatalf("the state file after the dropping start = %s (%v), want "+
			"no object", raw, err)
	}
}

// TestRecords pins what an agent records of what it received: every
// syncup of an object, the refused and the held included, every round, info
// read and push, the Check streams opened and ended per object, and the
// calls per method.
func TestRecords(t *testing.T) {
	fleet := newTestFleet(t, true)
	agent := addAgent(t, fleet, "dn0:9520", Dn, Config{})
	client := pb.NewDiskNodeAgentClient(dialFleet(t, fleet, "dn0:9520"))
	ctx := boundCtx(t)
	ptr := sidePtr(1, 3, 5)
	sideKey := SideKey(1, 3, 5)

	syncupSide(t, client, ptr, 1, 30) // refused: the dn lists no side yet
	syncupDn(t, client, 2, ptr)
	syncupDn(t, client, 1, ptr) // refused: stale
	syncupSide(t, client, ptr, 1, 30)
	if _, err := client.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, MigrId: 30, BmIdx: 0,
		Bitmap: []byte{1},
	}); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if _, err := client.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr,
	}); err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	stream, err := client.CheckSide(ctx)
	if err != nil {
		t.Fatalf("CheckSide: %v", err)
	}
	for rev := range uint64(3) {
		if err := stream.Send(&pb.CheckSideRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: rev,
		}); err != nil {
			t.Fatalf("CheckSide send: %v", err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("CheckSide recv: %v", err)
		}
	}
	stream.CloseSend()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("CheckSide after CloseSend replied")
	}
	update(t, agent, DnKey, func(ob *Object) { ob.HangSyncup = true })
	hctx, hcancel := context.WithCancel(ctx)
	held := make(chan error, 1)
	go func() {
		_, err := client.SyncupDn(hctx, &pb.SyncupDnRequest{
			ClusterId: 1, DnId: 1, Revision: 3,
		})
		held <- err
	}()
	eventually(t, "the held syncup recorded", func() error {
		if got := len(agent.Record(DnKey).Syncups); got != 3 {
			return status.Errorf(codes.FailedPrecondition,
				"%d dn syncups recorded, want 3", got)
		}
		return nil
	})
	hcancel()
	wantReturned(t, "the held syncup", held)

	dn := agent.Record(DnKey)
	revs := make([]uint64, 0, len(dn.Syncups))
	for _, req := range dn.Syncups {
		revs = append(revs, req.(*pb.SyncupDnRequest).GetRevision())
	}
	if len(revs) != 3 || revs[0] != 2 || revs[1] != 1 || revs[2] != 3 {
		t.Fatalf("dn syncup revisions = %v, want [2 1 3]", revs)
	}
	side := agent.Record(sideKey)
	if len(side.Syncups) != 2 || len(side.Pushes) != 1 ||
		len(side.InfoReads) != 1 || len(side.Rounds) != 3 {
		t.Fatalf("side record: %d syncups, %d pushes, %d info reads, %d "+
			"rounds; want 2, 1, 1, 3", len(side.Syncups), len(side.Pushes),
			len(side.InfoReads), len(side.Rounds))
	}
	for i, req := range side.Rounds {
		if got := req.(*pb.CheckSideRequest).GetRevision(); got != uint64(i) {
			t.Fatalf("side round %d revision %d", i, got)
		}
	}
	eventually(t, "the stream end recorded", func() error {
		if got := agent.Record(sideKey); got.Streams != 1 ||
			got.StreamEnds != 1 {
			return status.Errorf(codes.FailedPrecondition,
				"side streams %d, ends %d; want 1 and 1", got.Streams,
				got.StreamEnds)
		}
		return nil
	})
	for method, want := range map[string]int{
		MethodSyncupDn: 3, MethodSyncupSide: 2, MethodPushMigrBitmap: 1,
		MethodGetSideInfo: 1, MethodCheckSide: 1, MethodCheckDn: 0,
	} {
		if got := agent.Calls(method); got != want {
			t.Errorf("Calls(%s) = %d, want %d", method, got, want)
		}
	}
	if got := agent.Record("side 9:9:9"); got.Streams != 0 ||
		len(got.Syncups) != 0 {
		t.Errorf("an object never named has a record: %+v", got)
	}
}

// TestStoredIsTheLastAcceptedRequest pins Stored beside Record: a refused
// syncup, a stale one or one under a forced code, joins the record and not
// what the agent keeps; a held one joins the record as it arrives and what
// the agent keeps only once the lever releases it; and after a start on
// empty files the agent keeps nothing while the record keeps every
// request.
func TestStoredIsTheLastAcceptedRequest(t *testing.T) {
	fleet := newTestFleet(t, true)
	agent := addAgent(t, fleet, "dn0:9520", Dn, Config{})
	client := pb.NewDiskNodeAgentClient(dialFleet(t, fleet, "dn0:9520"))
	ctx := boundCtx(t)

	recorded := func() []uint64 {
		var revs []uint64
		for _, req := range agent.Record(DnKey).Syncups {
			revs = append(revs, req.(*pb.SyncupDnRequest).GetRevision())
		}
		return revs
	}
	wantStored := func(what string, want uint64, wantRecorded ...uint64) {
		t.Helper()
		req, _ := agent.Stored(DnKey).(*pb.SyncupDnRequest)
		if req == nil || req.GetRevision() != want {
			t.Fatalf("%s: the agent keeps %v, want the syncup at revision %d",
				what, req, want)
		}
		if got := recorded(); !slices.Equal(got, wantRecorded) {
			t.Fatalf("%s: recorded syncup revisions %v, want %v", what, got,
				wantRecorded)
		}
	}

	if got := agent.Stored(DnKey); got != nil {
		t.Fatalf("a new agent keeps %v, want nothing", got)
	}
	syncupDn(t, client, 2)
	wantStored("an accepted syncup", 2, 2)
	syncupDn(t, client, 1)
	wantStored("a stale syncup", 2, 2, 1)
	update(t, agent, DnKey, func(ob *Object) {
		ob.ReplyCode = common.ReplyCodeInvalidConf
	})
	syncupDn(t, client, 3)
	wantStored("a syncup under a forced code", 2, 2, 1, 3)

	update(t, agent, DnKey, func(ob *Object) {
		ob.ReplyCode = 0
		ob.HangSyncup = true
	})
	held := make(chan error, 1)
	go func() {
		_, err := client.SyncupDn(ctx, &pb.SyncupDnRequest{
			ClusterId: 1, DnId: 1, Revision: 4,
		})
		held <- err
	}()
	eventually(t, "the held syncup recorded", func() error {
		if got := recorded(); len(got) != 4 {
			return status.Errorf(codes.FailedPrecondition,
				"recorded syncup revisions %v, want 4 of them", got)
		}
		return nil
	})
	wantHeld(t, "the held syncup", held)
	wantStored("a held syncup", 2, 2, 1, 3, 4)
	update(t, agent, DnKey, func(ob *Object) { ob.HangSyncup = false })
	if err := wantReturned(t, "the released syncup", held); err != nil {
		t.Fatalf("the released syncup: %v", err)
	}
	wantStored("a released syncup", 4, 2, 1, 3, 4)

	agent.Stop()
	if err := agent.Start(DropStore); err != nil {
		t.Fatalf("Start(DropStore): %v", err)
	}
	if got := agent.Stored(DnKey); got != nil {
		t.Fatalf("an agent started on empty files keeps %v, want nothing", got)
	}
	if got := recorded(); !slices.Equal(got, []uint64{2, 1, 3, 4}) {
		t.Fatalf("recorded syncup revisions after the restart %v, want "+
			"[2 1 3 4]", got)
	}
}

// TestQuietServerStampsTheTraceId pins that a quiet server, which installs no
// server interceptor, still stamps an observation with the caller's trace
// id, read from the call's metadata.
func TestQuietServerStampsTheTraceId(t *testing.T) {
	fleet := newTestFleet(t, true)
	addAgent(t, fleet, "cn0:9520", Cn, Config{})
	client := pb.NewControllerNodeAgentClient(dialFleet(t, fleet, "cn0:9520"))
	ctx := common.WithTraceId(boundCtx(t), "quiet-trace")
	reply, err := client.SyncupCn(ctx,
		&pb.SyncupCnRequest{ClusterId: 1, CnId: 1, Revision: 1})
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if got := reply.GetObsStamp().GetObsTraceId(); got != "quiet-trace" {
		t.Fatalf("obs_trace_id = %q, want the caller's", got)
	}
}
