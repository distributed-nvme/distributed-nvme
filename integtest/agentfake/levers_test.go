package agentfake

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The levers set by Go calls (dnv-worker.md, Integration test plan, The fake
// agent). Each test plays one lever through Update or SetBehavior; the
// program's tests play the same levers through its behavior file.
// ---------------------------------------------------------------------------

// update edits one object's levers and fails the test on a refusal.
func update(t *testing.T, agent *Agent, key string, fn func(*Object)) {
	t.Helper()
	if err := agent.Update(key, fn); err != nil {
		t.Fatalf("Update(%s): %v", key, err)
	}
}

// dnRound runs one CheckDn round on a fresh stream under traceId.
func dnRound(
	t *testing.T, client pb.DiskNodeAgentClient, rev uint64, traceId string,
) *pb.CheckDnReply {
	t.Helper()
	return oneRound(t, client.CheckDn, &pb.CheckDnRequest{
		ClusterId: 1, DnId: 1, Revision: rev, TraceId: traceId,
	})
}

// dnInfoRead runs one GetDnInfo under traceId.
func dnInfoRead(
	t *testing.T, client pb.DiskNodeAgentClient, traceId string,
) *pb.GetDnInfoReply {
	t.Helper()
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		common.TraceIdMetadataKey, traceId)
	reply, err := client.GetDnInfo(ctx,
		&pb.GetDnInfoRequest{ClusterId: 1, DnId: 1})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	return reply
}

// TestObsAgeLever is the old observation (ObsAge): a round and an info read
// answer from an observation that ended the lever's age before the answer,
// under the call's trace id; while that age would reach back past the
// latest observation the fake gave, the answer repeats that observation, its
// time and its trace id; a Syncup* reply keeps a new observation after its
// save time; and once the lever is cleared a round answers from a new one.
func TestObsAgeLever(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	syncupDn(t, dnClient, 1)
	fresh := dnRound(t, dnClient, 1, "fresh")
	freshObs, _ := obsTimes(fresh.GetObsStamp())

	// An age far longer than the test: every answer repeats the latest
	// observation, the fresh round's, and its age grows with the answers.
	update(t, agent, DnKey, func(ob *Object) { ob.ObsAge = time.Hour })
	for _, call := range []struct {
		what  string
		stamp *pb.ObsStamp
	}{
		{"a round", dnRound(t, dnClient, 1, "stalled").GetObsStamp()},
		{"an info read", dnInfoRead(t, dnClient, "stalled").GetObsStamp()},
	} {
		obs, answer := obsTimes(call.stamp)
		if !obs.Equal(freshObs) || call.stamp.GetObsTraceId() != "fresh" {
			t.Fatalf("%s under a stalled read: obs %v %q, want the latest "+
				"observation %v \"fresh\"", call.what, obs,
				call.stamp.GetObsTraceId(), freshObs)
		}
		if !answer.After(obs) {
			t.Fatalf("%s: answer %v not after the repeated obs %v",
				call.what, answer, obs)
		}
	}

	// A short age, once it has passed: each answer is a new observation
	// that ended exactly that long before it, under the call's trace id.
	const age = 100 * time.Millisecond
	update(t, agent, DnKey, func(ob *Object) { ob.ObsAge = age })
	time.Sleep(2 * age)
	var last time.Time
	for i, stamp := range []*pb.ObsStamp{
		dnRound(t, dnClient, 1, "aged").GetObsStamp(),
		dnInfoRead(t, dnClient, "aged").GetObsStamp(),
	} {
		obs, answer := obsTimes(stamp)
		if got := answer.Sub(obs); got != age {
			t.Fatalf("aged answer %d: answer - obs = %v, want %v", i, got, age)
		}
		if stamp.GetObsTraceId() != "aged" || !obs.After(last) ||
			!obs.After(freshObs) {
			t.Fatalf("aged answer %d: obs %v %q, want a new one after %v",
				i, obs, stamp.GetObsTraceId(), last)
		}
		last = obs
	}

	// A Syncup*'s converge makes a new observation whatever the lever says:
	// it ends after the save.
	saved := syncupDn(t, dnClient, 2)
	obs, _ := obsTimes(saved.GetObsStamp())
	if saved.GetSaveTime() == nil || !obs.After(saved.GetSaveTime().AsTime()) {
		t.Fatalf("a Syncup under the lever: obs %v, save_time %v; want the "+
			"observation after the save", obs, saved.GetSaveTime())
	}

	update(t, agent, DnKey, func(ob *Object) { ob.ObsAge = 0 })
	before := time.Now()
	cleared := dnRound(t, dnClient, 2, "cleared").GetObsStamp()
	if obs, _ := obsTimes(cleared); obs.Before(before) ||
		cleared.GetObsTraceId() != "cleared" {
		t.Fatalf("a round after the clear: obs %v %q, want a new one made "+
			"after %v", obs, cleared.GetObsTraceId(), before)
	}
}

// TestObsAgeRepeatsTheWholeObservation is the stalled read of ObsAge: while
// the age reaches back past the latest observation, a round and an info read
// repeat that observation whole — its code, revision and rows with its time
// and trace id — whatever the levers and the stored state say meanwhile, and
// a round on a repeat spends no leftover round; once the lever is cleared
// the next round is a new observation that reads them all.
func TestObsAgeRepeatsTheWholeObservation(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	syncupDn(t, dnClient, 1)
	fresh := dnRound(t, dnClient, 1, "fresh")
	freshObs, _ := obsTimes(fresh.GetObsStamp())
	const (
		ok       = pb.ResStatus_RES_STATUS_OK
		errState = pb.ResStatus_RES_STATUS_ERROR
	)

	update(t, agent, DnKey, func(ob *Object) {
		ob.ObsAge = time.Hour
		ob.LeftoverRounds = 1
		ob.Rows = map[string]Row{"meta_info": {Status: ptrTo(errState)}}
	})
	writeStateFile(t, agent, `{"objects": {"dn": {"revision": 7,
	  "request": {"cluster_id": 1, "dn_id": 1, "revision": 7}}}}`)
	type reading struct {
		what  string
		code  uint32
		rev   uint64
		info  *pb.DnInfo
		stamp *pb.ObsStamp
	}
	for range 2 {
		r := dnRound(t, dnClient, 7, "stalled")
		i := dnInfoRead(t, dnClient, "stalled")
		for _, call := range []reading{
			{"a round", r.GetAgentReply().GetCode(), r.GetRevision(),
				r.GetDnInfo(), r.GetObsStamp()},
			{"an info read", i.GetAgentReply().GetCode(), i.GetRevision(),
				i.GetDnInfo(), i.GetObsStamp()},
		} {
			obs, _ := obsTimes(call.stamp)
			if call.code != 0 || call.rev != 1 ||
				call.info.GetMetaInfo().GetStatus() != ok ||
				!obs.Equal(freshObs) ||
				call.stamp.GetObsTraceId() != "fresh" {
				t.Fatalf("%s under a stalled read: code %d revision %d "+
					"meta_info %v obs %v %q; want the fresh round's code "+
					"0, revision 1, meta_info OK, obs %v \"fresh\"",
					call.what, call.code, call.rev, call.info.GetMetaInfo(),
					obs, call.stamp.GetObsTraceId(), freshObs)
			}
		}
	}

	update(t, agent, DnKey, func(ob *Object) { ob.ObsAge = 0 })
	read := dnRound(t, dnClient, 7, "read")
	if obs, _ := obsTimes(read.GetObsStamp()); read.GetAgentReply().
		GetCode() != common.ReplyCodeLeftover || read.GetRevision() != 7 ||
		read.GetDnInfo().GetMetaInfo().GetStatus() != errState ||
		!obs.After(freshObs) || read.GetObsStamp().GetObsTraceId() != "read" {
		t.Fatalf("the round after the clear = %v; want a new observation: "+
			"the leftover none of the repeats spent, revision 7 and "+
			"meta_info ERROR", read)
	}
	if code := dnRound(t, dnClient, 7, "next").GetAgentReply().
		GetCode(); code != 0 {
		t.Fatalf("the round after it: code %d, want the leftover spent", code)
	}
}

// TestObsNoneLever is the absent observation (ObsNone): a round and an info
// read of an object the fake knows answer as an agent that holds the object
// and no observation of it yet — code zero, revision zero, no *Info and the
// answer time alone (dnagent.md SH28) — even while a leftover stands, which
// spends no round of it; a Check stream's change-only rule passes such a
// round by; and a Syncup* reply still carries its observation and save
// time. Cleared, a round answers from a new observation again.
func TestObsNoneLever(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)
	syncupSide(t, dnClient, ptr, 1, 0)

	stream, err := dnClient.CheckDn(context.Background())
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	defer stream.CloseSend()
	round := func(rev uint64, traceId string) *pb.CheckDnReply {
		t.Helper()
		if err := stream.Send(&pb.CheckDnRequest{
			ClusterId: 1, DnId: 1, Revision: rev, TraceId: traceId,
		}); err != nil {
			t.Fatalf("CheckDn send: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("CheckDn recv: %v", err)
		}
		return reply
	}
	if round(1, "before").GetDnInfo() == nil {
		t.Fatal("the stream's first round carries no dn_info")
	}

	update(t, agent, DnKey, func(ob *Object) {
		ob.ObsNone = true
		ob.LeftoverRounds = 1
	})
	update(t, agent, SideKey(1, 3, 5), func(ob *Object) { ob.ObsNone = true })
	type got struct {
		code  uint32
		rev   uint64
		info  bool
		stamp *pb.ObsStamp
	}
	dnStream := round(1, "r")
	dnRead := dnInfoRead(t, dnClient, "r")
	sideRound := oneRound(t, dnClient.CheckSide, &pb.CheckSideRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 1, TraceId: "r",
	})
	sideRead, err := dnClient.GetSideInfo(context.Background(),
		&pb.GetSideInfoRequest{ClusterId: 1, DnId: 1, SidePointer: ptr})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	for what, reply := range map[string]got{
		"a dn round": {dnStream.GetAgentReply().GetCode(),
			dnStream.GetRevision(), dnStream.GetDnInfo() != nil,
			dnStream.GetObsStamp()},
		"a dn info read": {dnRead.GetAgentReply().GetCode(),
			dnRead.GetRevision(), dnRead.GetDnInfo() != nil,
			dnRead.GetObsStamp()},
		"a side round": {sideRound.GetAgentReply().GetCode(),
			sideRound.GetRevision(), sideRound.GetSideInfo() != nil,
			sideRound.GetObsStamp()},
		"a side info read": {sideRead.GetAgentReply().GetCode(),
			sideRead.GetRevision(), sideRead.GetSideInfo() != nil,
			sideRead.GetObsStamp()},
	} {
		obs, answer := obsTimes(reply.stamp)
		if reply.code != 0 || reply.rev != 0 || reply.info ||
			!obs.IsZero() || reply.stamp.GetObsTraceId() != "" ||
			answer.IsZero() {
			t.Errorf("%s under obs_none: code %d revision %d info %v "+
				"stamp %v; want code 0, revision 0, no info and an answer "+
				"time alone", what, reply.code, reply.rev, reply.info,
				reply.stamp)
		}
	}

	saved := syncupDn(t, dnClient, 2, ptr)
	if obs, _ := obsTimes(saved.GetObsStamp()); obs.IsZero() ||
		saved.GetSaveTime() == nil || saved.GetRevision() != 2 {
		t.Fatalf("a Syncup under obs_none: revision %d stamp %v, save_time "+
			"%v; want revision 2, an observation and a save time",
			saved.GetRevision(), saved.GetObsStamp(), saved.GetSaveTime())
	}

	update(t, agent, DnKey, func(ob *Object) { ob.ObsNone = false })
	after := round(2, "after")
	if obs, _ := obsTimes(after.GetObsStamp()); obs.IsZero() ||
		after.GetObsStamp().GetObsTraceId() != "after" ||
		after.GetRevision() != 2 ||
		after.GetAgentReply().GetCode() != common.ReplyCodeLeftover {
		t.Fatalf("the round after the clear = %v; want a new observation "+
			"at revision 2 with the leftover no round under obs_none spent",
			after)
	}
	if info := after.GetDnInfo(); info != nil {
		t.Fatalf("the round after the clear carries %v; want none: it is "+
			"the info the stream last sent", info)
	}
}

// TestRowReasonLever is the row reason: a row lever's reason is the row's
// reason, over a derived row's reason too, and a row with no reason lever
// keeps the derived one only while it reads the derived status.
func TestRowReasonLever(t *testing.T) {
	const (
		errStatus = pb.ResStatus_RES_STATUS_ERROR
		prov      = pb.ResStatus_RES_STATUS_PROVISIONING
		lost      = pb.ResReason_RES_REASON_THIN_ID_LOST
		none      = pb.ResReason_RES_REASON_NONE
		await     = pb.ResReason_RES_REASON_AWAITING_CUTOVER
	)
	agent := newTestAgent(t)
	dnClient, cnClient := startAgent(t, agent)

	// The cn agent's thin row whose pool lost the thin id (cnagent.md CN14).
	update(t, agent, CntlrKey(1, 1), func(ob *Object) {
		ob.ThinOk = true
		ob.Rows = map[string]Row{"td_id_to_thin_info.7.1": {
			Status: ptrTo(errStatus), Details: ptrTo("ENODATA"),
			Reason: ptrTo(lost),
		}}
	})
	info := syncupCntlrFixture(t, cnClient, true).GetCntlrInfo()
	wantRow(t, "td_id_to_thin_info.7.1",
		info.GetTdIdToThinInfo()[7].GetSliceIdToDmThin()[1],
		errStatus, "ENODATA", lost)
	wantRow(t, "td_id_to_thin_info.8.1",
		info.GetTdIdToThinInfo()[8].GetSliceIdToDmThin()[1],
		pb.ResStatus_RES_STATUS_OK, "", none)

	// A migration destination waiting for the cutover: a reason lever wins
	// over the derived reason while the row stays PROVISIONING.
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)
	update(t, agent, SideKey(1, 3, 5), func(ob *Object) {
		ob.Rows = map[string]Row{
			"migr_dst_info.target_info": {Reason: ptrTo(none)},
		}
	})
	dst := syncupMigrDst(t, dnClient, ptr, 1, true, true).GetSideInfo().
		GetMigrDstInfo()
	wantRow(t, "migr_dst_info.target_info", dst.GetTargetInfo(),
		prov, detailsAwaitingCutover, none)
	wantRow(t, "migr_dst_info.dm_clone_info", dst.GetDmCloneInfo(),
		prov, detailsAwaitingCutover, await)
}

// ptrTo is a pointer to a copy of v.
func ptrTo[T any](v T) *T { return &v }

// TestSyncupRevisionLever is the reply revision lever (SyncupRevision): an
// accepted Syncup* reply, code zero or a leftover_rounds leftover, carries
// the lever's revision while the fake stores the request's, which its
// rounds and info reads report; a refused reply carries the stored one.
func TestSyncupRevisionLever(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)
	update(t, agent, DnKey, func(ob *Object) { ob.SyncupRevision = ptrTo[uint64](7) })
	update(t, agent, SideKey(1, 3, 5), func(ob *Object) {
		ob.SyncupRevision = ptrTo[uint64](0)
	})

	reply := syncupDn(t, dnClient, 3, ptr)
	if code := reply.GetAgentReply().GetCode(); code != 0 ||
		reply.GetRevision() != 7 {
		t.Fatalf("an accepted SyncupDn: code %d revision %d, want 0 and the "+
			"lever's 7", code, reply.GetRevision())
	}
	if got := dnRound(t, dnClient, 3, "r").GetRevision(); got != 3 {
		t.Fatalf("a round after the lever's reply reports revision %d, want "+
			"the stored 3", got)
	}
	if got := dnInfoRead(t, dnClient, "i").GetRevision(); got != 3 {
		t.Fatalf("an info read reports revision %d, want the stored 3", got)
	}
	if side := syncupSide(t, dnClient, ptr, 4, 0); side.GetAgentReply().
		GetCode() != 0 || side.GetRevision() != 0 {
		t.Fatalf("an accepted SyncupSide = code %d revision %d, want 0 and "+
			"the lever's 0", side.GetAgentReply().GetCode(), side.GetRevision())
	}

	stale := syncupDn(t, dnClient, 2, ptr)
	if code := stale.GetAgentReply().GetCode(); code !=
		common.ReplyCodeStaleRevision || stale.GetRevision() != 3 {
		t.Fatalf("a stale SyncupDn: code %d revision %d, want stale and the "+
			"stored 3", code, stale.GetRevision())
	}

	update(t, agent, DnKey, func(ob *Object) { ob.LeftoverRounds = 1 })
	left := syncupDn(t, dnClient, 3, ptr)
	if code := left.GetAgentReply().GetCode(); code !=
		common.ReplyCodeLeftover || left.GetRevision() != 7 {
		t.Fatalf("a leftover SyncupDn: code %d revision %d, want the "+
			"leftover and the lever's 7", code, left.GetRevision())
	}
}

// TestLeftoverRoundsLever is the leftover that clears (LeftoverRounds): while
// it stands a Syncup* is applied and saved and answered with
// ReplyCodeLeftover, an info read answers it without spending a round, and
// each Check round answered spends one; after the last the object answers
// code zero again. A new write re-arms it, and a forced reply_code wins over
// it.
func TestLeftoverRoundsLever(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	syncupDn(t, dnClient, 1)
	update(t, agent, DnKey, func(ob *Object) { ob.LeftoverRounds = 2 })

	saved := syncupDn(t, dnClient, 2)
	if code := saved.GetAgentReply().GetCode(); code !=
		common.ReplyCodeLeftover {
		t.Fatalf("a SyncupDn under a leftover: code %d, want leftover", code)
	}
	if obs, _ := obsTimes(saved.GetObsStamp()); obs.IsZero() ||
		saved.GetSaveTime() == nil {
		t.Fatalf("a leftover SyncupDn: stamp %v, save_time %v; want both: "+
			"it saved the request", saved.GetObsStamp(), saved.GetSaveTime())
	}
	wantRound := func(what string, code uint32) {
		t.Helper()
		round := dnRound(t, dnClient, 2, "r")
		if got := round.GetAgentReply().GetCode(); got != code ||
			round.GetRevision() != 2 {
			t.Fatalf("%s: code %d revision %d, want %d and the applied 2",
				what, got, round.GetRevision(), code)
		}
	}
	wantRound("the first round", common.ReplyCodeLeftover)
	for range 2 {
		if code := dnInfoRead(t, dnClient, "i").GetAgentReply().
			GetCode(); code != common.ReplyCodeLeftover {
			t.Fatalf("an info read under a leftover: code %d", code)
		}
	}
	wantRound("the second round", common.ReplyCodeLeftover)
	wantRound("the round after the leftover cleared", 0)
	if code := syncupDn(t, dnClient, 2).GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("a SyncupDn after the leftover cleared: code %d", code)
	}

	update(t, agent, DnKey, func(ob *Object) { ob.LeftoverRounds = 1 })
	wantRound("the round after the re-arm", common.ReplyCodeLeftover)
	wantRound("the round after it", 0)

	update(t, agent, DnKey, func(ob *Object) {
		ob.LeftoverRounds = 2
		ob.ReplyCode = common.ReplyCodeInvalidConf
	})
	refused := syncupDn(t, dnClient, 3)
	if code := refused.GetAgentReply().GetCode(); code !=
		common.ReplyCodeInvalidConf || refused.GetRevision() != 2 {
		t.Fatalf("a forced code under a leftover: code %d revision %d, want "+
			"the forced code and nothing applied", code, refused.GetRevision())
	}
}

// TestUpdateReArmsOnlyAChangedCount pins how an Update treats the counted
// levers (HangRounds, LeftoverRounds): an Update that leaves a count's value
// as it was, editing another lever of the object, leaves the rounds the
// count has left as they are, a spent count included; one that changes the
// value counts afresh; and SetBehavior counts every one afresh.
func TestUpdateReArmsOnlyAChangedCount(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	syncupDn(t, dnClient, 1)
	editRow := func(details string) {
		t.Helper()
		update(t, agent, DnKey, func(ob *Object) {
			ob.Rows = map[string]Row{"port_info": {Details: ptrTo(details)}}
		})
	}
	wantCodes := func(what string, codes ...uint32) {
		t.Helper()
		for i, want := range codes {
			if got := dnRound(t, dnClient, 1, "r").GetAgentReply().
				GetCode(); got != want {
				t.Fatalf("%s, round %d: code %d, want %d", what, i, got, want)
			}
		}
	}

	update(t, agent, DnKey, func(ob *Object) { ob.LeftoverRounds = 2 })
	wantCodes("the first of two leftover rounds", common.ReplyCodeLeftover)
	editRow("a")
	wantCodes("after an edit of a row", common.ReplyCodeLeftover, 0)
	editRow("b")
	wantCodes("after an edit of a row once the count is spent", 0)
	update(t, agent, DnKey, func(ob *Object) { ob.LeftoverRounds = 1 })
	wantCodes("after a change of the count", common.ReplyCodeLeftover, 0)
	if err := agent.SetBehavior(agent.Behavior()); err != nil {
		t.Fatalf("SetBehavior: %v", err)
	}
	wantCodes("after SetBehavior", common.ReplyCodeLeftover, 0)

	// The same for hang_rounds: an edit of a row leaves the one held round
	// armed, and once that round is spent leaves it spent.
	ctx := boundCtx(t)
	update(t, agent, DnKey, func(ob *Object) { ob.HangRounds = 1 })
	editRow("c")
	round, cancel := checkDnRound(t, ctx, dnClient)
	wantHeld(t, "the round hang_rounds counts", round)
	cancel()
	wantReturned(t, "the counted round", round)
	editRow("d")
	round, cancel = checkDnRound(t, ctx, dnClient)
	defer cancel()
	if err := wantReturned(t, "the round after an edit of a row",
		round); err != nil {
		t.Fatalf("the round after an edit of a row: %v, want it answered: "+
			"the count is spent", err)
	}
}

// TestDiskCountsLever is the disk counts (DiskCounts) of the dn: reported
// beside the rows only while the meta row reads OK, the side records counted
// from the last accepted SyncupDn and matching it unless the lever says
// otherwise, and a change of the counts alone is a change a Check stream
// sends.
func TestDiskCountsLever(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	syncupDn(t, dnClient, 1, sidePtr(1, 3, 5), sidePtr(1, 3, 6))
	if counts := dnInfoRead(t, dnClient, "i").GetDnInfo().
		GetDiskCounts(); counts != nil {
		t.Fatalf("disk counts without the lever = %v, want none", counts)
	}

	update(t, agent, DnKey, func(ob *Object) {
		ob.DiskCounts = &DiskCounts{
			TotalExtCnt: 100, FreeExtCnt: 40, FreeMetaUnits: 9,
		}
	})
	want := func(what string, got *pb.DnInfo_DiskCounts,
		total, free, recs, units uint64, match bool) {
		t.Helper()
		if got.GetTotalExtCnt() != total || got.GetFreeExtCnt() != free ||
			got.GetSideRecCnt() != recs || got.GetFreeMetaUnits() != units ||
			got.GetSideRecsMatch() != match || got == nil {
			t.Fatalf("%s: disk counts %v, want %d %d %d %d %v", what, got,
				total, free, recs, units, match)
		}
	}
	want("the info read", dnInfoRead(t, dnClient, "i").GetDnInfo().
		GetDiskCounts(), 100, 40, 2, 9, true)

	stream, err := dnClient.CheckDn(context.Background())
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	defer stream.CloseSend()
	round := func() *pb.CheckDnReply {
		t.Helper()
		if err := stream.Send(&pb.CheckDnRequest{
			ClusterId: 1, DnId: 1, Revision: 1,
		}); err != nil {
			t.Fatalf("CheckDn send: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("CheckDn recv: %v", err)
		}
		return reply
	}
	want("the first round", round().GetDnInfo().GetDiskCounts(),
		100, 40, 2, 9, true)
	if info := round().GetDnInfo(); info != nil {
		t.Fatalf("an unchanged round carries %v", info)
	}
	update(t, agent, DnKey, func(ob *Object) {
		ob.DiskCounts.FreeExtCnt = 39
		ob.DiskCounts.SideRecCnt = ptrTo[uint64](5)
		ob.DiskCounts.SideRecsMatch = ptrTo(false)
	})
	want("the round after the counts changed", round().GetDnInfo().
		GetDiskCounts(), 100, 39, 5, 9, false)

	update(t, agent, DnKey, func(ob *Object) {
		ob.Rows = map[string]Row{"meta_info": {
			Status: ptrTo(pb.ResStatus_RES_STATUS_ERROR),
		}}
	})
	if counts := dnInfoRead(t, dnClient, "i").GetDnInfo().
		GetDiskCounts(); counts != nil {
		t.Fatalf("disk counts beside an ERROR meta row = %v, want none",
			counts)
	}
}

// TestParseBehaviorNewLevers pins the behavior file's keys of the levers the
// Go front sets too: each parses into the same lever, reason names in both
// spellings, and a lever placed where it cannot act is refused.
func TestParseBehaviorNewLevers(t *testing.T) {
	parsed, err := ParseBehavior([]byte(`{"objects": {
	  "dn": {"obs_age_ms": 1500, "syncup_revision": 0, "leftover_rounds": 3,
	         "disk_counts": {"total_ext_cnt": 10, "free_ext_cnt": 4,
	                         "free_meta_units": 2, "side_rec_cnt": 1,
	                         "side_recs_match": false}},
	  "cn": {"obs_none": true, "disk_counts": null},
	  "cntlr 1:1": {"rows": {
	    "td_id_to_thin_info.7.1": {"status": "ERROR", "reason": "thin_id_lost"},
	    "leg_id_to_leg.3": {"reason": "RES_REASON_AWAITING_CUTOVER"},
	    "clone_id_to_target.48": {"status": "MISSING",
	                              "reason": "SOURCE_FAULT", "when_primary": true},
	    "slice_id_to_dm_pool.1": {"status": "MISSING",
	                              "reason": "RES_REASON_SP_LEVEL"}}}}}`))
	if err != nil {
		t.Fatalf("ParseBehavior: %v", err)
	}
	dn := parsed.Objects[DnKey]
	if dn.ObsAge != 1500*time.Millisecond || dn.SyncupRevision == nil ||
		*dn.SyncupRevision != 0 || dn.LeftoverRounds != 3 {
		t.Errorf("dn levers = %+v", dn)
	}
	counts := dn.DiskCounts
	if counts == nil || counts.TotalExtCnt != 10 || counts.FreeExtCnt != 4 ||
		counts.FreeMetaUnits != 2 || counts.SideRecCnt == nil ||
		*counts.SideRecCnt != 1 || counts.SideRecsMatch == nil ||
		*counts.SideRecsMatch {
		t.Errorf("dn disk counts = %+v", counts)
	}
	if cn := parsed.Objects[CnKey]; !cn.ObsNone || cn.DiskCounts != nil {
		t.Errorf("cn levers = %+v", cn)
	}
	rows := parsed.Objects[CntlrKey(1, 1)].Rows
	if r := rows["td_id_to_thin_info.7.1"].Reason; r == nil ||
		*r != pb.ResReason_RES_REASON_THIN_ID_LOST {
		t.Errorf("thin row reason = %v", r)
	}
	if r := rows["leg_id_to_leg.3"].Reason; r == nil ||
		*r != pb.ResReason_RES_REASON_AWAITING_CUTOVER {
		t.Errorf("leg row reason = %v", r)
	}
	if row := rows["clone_id_to_target.48"]; row.Reason == nil ||
		*row.Reason != pb.ResReason_RES_REASON_SOURCE_FAULT ||
		row.Status == nil || *row.Status != pb.ResStatus_RES_STATUS_MISSING ||
		!row.WhenPrimary {
		t.Errorf("clone target row = %+v", row)
	}
	if r := rows["slice_id_to_dm_pool.1"].Reason; r == nil ||
		*r != pb.ResReason_RES_REASON_SP_LEVEL {
		t.Errorf("pool row reason = %v", r)
	}

	for _, tc := range []struct {
		name string
		data string
	}{
		{"unknown reason",
			`{"objects": {"dn": {"rows": {"disk_info": {"reason": "nope"}}}}}`},
		{"disk counts on a side",
			`{"objects": {"side 1:3:5": {"disk_counts": {"free_ext_cnt": 1}}}}`},
		{"disk counts on the cn",
			`{"objects": {"cn": {"disk_counts": {"free_ext_cnt": 1}}}}`},
		{"disk counts in default", `{"default": {"disk_counts": {}}}`},
		{"obs_age_ms in default", `{"default": {"obs_age_ms": 5}}`},
		{"obs_none in default", `{"default": {"obs_none": true}}`},
		{"syncup_revision in default", `{"default": {"syncup_revision": 1}}`},
		{"leftover_rounds in default", `{"default": {"leftover_rounds": 1}}`},
		{"unknown disk counts key",
			`{"objects": {"dn": {"disk_counts": {"free": 1}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBehavior([]byte(tc.data)); err == nil {
				t.Fatalf("ParseBehavior(%s) succeeded, want an error", tc.data)
			}
		})
	}
}

// nonZeroOf is a value of type typ that is not its zero value, for a guard
// that sets every field of a struct in turn.
func nonZeroOf(t *testing.T, field string, typ reflect.Type) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.Pointer:
		return reflect.New(typ.Elem())
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(reflect.Zero(typ.Key()), reflect.Zero(typ.Elem()))
		return m
	case reflect.Slice:
		return reflect.MakeSlice(typ, 1, 1)
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(typ)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64:
		return reflect.ValueOf(1).Convert(typ)
	case reflect.String:
		return reflect.ValueOf("x").Convert(typ)
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return reflect.ValueOf(time.Now())
	}
	t.Fatalf("Object.%s: no non-zero value of kind %s for the guard",
		field, typ.Kind())
	return reflect.Value{}
}

// TestDefaultEntryTakesStatusAndDetailsAlone guards the default entry
// (dnv-worker.md, Integration test plan, The fake agent): it is read for its
// status and details alone, so SetBehavior refuses it with any other field
// of Object set, each field in turn, a lever added to Object later
// included, and takes it with the two it reads.
func TestDefaultEntryTakesStatusAndDetailsAlone(t *testing.T) {
	agent := newTestAgent(t)
	typ := reflect.TypeOf(Object{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		ob := &Object{}
		reflect.ValueOf(ob).Elem().Field(i).Set(
			nonZeroOf(t, field.Name, field.Type))
		err := agent.SetBehavior(&Behavior{Default: ob})
		switch field.Name {
		case "Status", "Details":
			if err != nil {
				t.Errorf("the default entry with %s alone refused: %v",
					field.Name, err)
			}
		default:
			if err == nil {
				t.Errorf("the default entry with %s set accepted, want it "+
					"refused", field.Name)
			}
		}
	}
	if _, err := ParseBehavior([]byte(
		`{"default": {"status": "ERROR", "details": "d"}}`)); err != nil {
		t.Errorf("ParseBehavior refused a default status and details: %v",
			err)
	}
	for _, data := range []string{
		`{"default": {"rows": {"disk_info": {"status": "ERROR"}}}}`,
		`{"default": {"reply_code": 2}}`,
		`{"default": {"hang": true}}`,
		`{"default": {"thin_ok": true}}`,
	} {
		if _, err := ParseBehavior([]byte(data)); err == nil {
			t.Errorf("ParseBehavior(%s) accepted, want the default entry's "+
				"lever refused", data)
		}
	}
}

// TestLeverChangeWakesWaiters pins that a waiting lever waits on the lever
// change itself and not on the next request: a held round, a held info read
// and a held syncup each end as soon as Update or SetBehavior clears their
// lever, with no other call made.
func TestLeverChangeWakesWaiters(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, agent *Agent)
	}{
		{"Update", func(t *testing.T, agent *Agent) {
			update(t, agent, CntlrKey(1, 1), func(ob *Object) {
				ob.Hang, ob.HangSyncup = false, false
			})
			update(t, agent, CnKey, func(ob *Object) { ob.Hang = false })
		}},
		{"SetBehavior", func(t *testing.T, agent *Agent) {
			if err := agent.SetBehavior(nil); err != nil {
				t.Fatalf("SetBehavior: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent(t)
			_, cnClient := startAgent(t, agent)
			syncupCntlrFixture(t, cnClient, true)
			update(t, agent, CntlrKey(1, 1), func(ob *Object) {
				ob.Hang, ob.HangSyncup = true, true
			})
			update(t, agent, CnKey, func(ob *Object) { ob.Hang = true })

			round, cancel := cntlrRound(t, cnClient)
			defer cancel()
			info := make(chan error, 1)
			go func() {
				_, err := cnClient.GetCnInfo(context.Background(),
					&pb.GetCnInfoRequest{ClusterId: 1, CnId: 1})
				info <- err
			}()
			syncup := make(chan error, 1)
			go func() {
				req := cntlrFixture(true)
				req.Revision = 6
				_, err := cnClient.SyncupCntlr(context.Background(), req)
				syncup <- err
			}()
			for what, done := range map[string]chan error{
				"the round": round, "the info read": info,
				"the syncup": syncup,
			} {
				wantHeld(t, what, done)
			}
			tc.do(t, agent)
			for what, done := range map[string]chan error{
				"the round": round, "the info read": info,
				"the syncup": syncup,
			} {
				if err := wantReturned(t, what, done); err != nil {
					t.Fatalf("%s after the clear: %v", what, err)
				}
			}
		})
	}
}

// TestWaitsNeverPoll pins that a wait that holds a call wakes on the lever
// change alone and never on a clock. It runs the waits of a held round, a
// held unary call and a held syncup in a testing/synctest bubble, whose
// clock moves only while every goroutine of the bubble is blocked and no
// synctest.Wait is pending: once Update clears their levers, each must have
// returned with no time passed, where a wait that re-read the levers on a
// timer would still sit on that timer.
func TestWaitsNeverPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent, err := New(context.Background(), Config{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		update(t, agent, DnKey, func(ob *Object) {
			ob.Hang, ob.HangSyncup = true, true
		})
		start := time.Now()
		ctx := context.Background()
		waits := []struct {
			what string
			wait func() error
			done chan error
		}{
			{"the round", func() error {
				_, err := agent.waitForRound(ctx, DnKey)
				return err
			}, make(chan error, 1)},
			{"the unary call", func() error {
				return agent.hangUnary(ctx, DnKey)
			}, make(chan error, 1)},
			{"the syncup", func() error {
				return agent.hangSyncup(ctx, DnKey)
			}, make(chan error, 1)},
		}
		for _, w := range waits {
			go func() { w.done <- w.wait() }()
		}
		synctest.Wait()
		for _, w := range waits {
			select {
			case err := <-w.done:
				t.Fatalf("%s returned while held: %v", w.what, err)
			default:
			}
		}
		update(t, agent, DnKey, func(ob *Object) {
			ob.Hang, ob.HangSyncup = false, false
		})
		synctest.Wait()
		for _, w := range waits {
			select {
			case err := <-w.done:
				if err != nil {
					t.Fatalf("%s after the clear: %v", w.what, err)
				}
			default:
				t.Fatalf("%s is still held after the clear, with no time "+
					"passed: its wait does not wake on the lever change",
					w.what)
			}
		}
		if passed := time.Since(start); passed != 0 {
			t.Fatalf("%v passed in the bubble, want none", passed)
		}
	})
}

// TestLeversSafeUnderConcurrentUse plays every Go call of the agent from
// several goroutines while rounds, syncups and info reads are served, which
// the race detector checks: the levers are set and cleared while the agent
// serves.
func TestLeversSafeUnderConcurrentUse(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)
	syncupSide(t, dnClient, ptr, 1, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	serve := func(fn func(i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ctx.Err() == nil && i < 50; i++ {
				if err := fn(i); err != nil && ctx.Err() == nil {
					t.Errorf("%v", err)
					return
				}
			}
		}()
	}
	serve(func(i int) error {
		_, err := dnClient.SyncupSide(ctx, &pb.SyncupSideRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 1,
			SideConf: &pb.SyncupSideRequest_SideConf{ExtCnt: 2},
		})
		return err
	})
	serve(func(i int) error {
		_, err := dnClient.GetDnInfo(ctx,
			&pb.GetDnInfoRequest{ClusterId: 1, DnId: 1})
		return err
	})
	serve(func(i int) error {
		stream, err := dnClient.CheckSide(ctx)
		if err != nil {
			return err
		}
		defer stream.CloseSend()
		if err := stream.Send(&pb.CheckSideRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 1,
		}); err != nil {
			return err
		}
		_, err = stream.Recv()
		return err
	})
	serve(func(i int) error {
		return agent.Update(SideKey(1, 3, 5), func(ob *Object) {
			ob.ObsAge = time.Duration(i%3) * time.Millisecond
			ob.LeftoverRounds = uint32(i % 2)
			ob.Rows = map[string]Row{"side_dev_info": {
				Details: ptrTo(fmt.Sprint(i)),
			}}
		})
	})
	serve(func(i int) error {
		b := agent.Behavior()
		b.Size = uint64(i)
		if err := agent.SetBehavior(b); err != nil {
			return err
		}
		_ = agent.Record(SideKey(1, 3, 5))
		_ = agent.Calls(MethodCheckSide)
		return nil
	})
	wg.Wait()
}

// TestCntlrInfoRepairsAndReadings pins what a cntlr reports of a repair and
// of a group (dnv-worker.md, Integration test plan, The fake agent;
// architecture.md, Repair): a leg row per new leg of every repair its
// request carries, on a primary and a standby alike; on a primary, the
// repairs lever's verdict on a repair by its rep_name, keyed by the repair's
// rep_id, its refusal text included, and no verdict for a repair the lever
// does not name; and on a primary, a reading of every group of the request,
// whole with every leg_list leg in sync and naming the group's
// forced-start mark as the one it acted on, unless the grps lever says
// otherwise, part by part. A standby reports neither verdicts nor
// readings.
func TestCntlrInfoRepairsAndReadings(t *testing.T) {
	const behavior = `{"objects": {"cntlr 1:1": {
	  "repairs": {"r0": {"started": true, "held": [105]},
	              "gone": {"abort_confirmed": true},
	              "r2": {"refusal": "leg 0x2ca holds newer data"}},
	  "grps": {"12": {"whole": false}, "0x15": {"in_sync": []},
	           "71": {"whole": false, "in_sync": [712], "acted": 0}}}}}`
	for _, shape := range []struct {
		name     string
		primary  bool
		disabled bool
	}{
		{"primary", true, false},
		{"standby", false, false},
		// A disabled primary converges the standby shape (cnagent.md CN9):
		// no group readings and no repair verdicts.
		{"disabled primary", true, true},
	} {
		primary := shape.primary && !shape.disabled
		t.Run(shape.name, func(t *testing.T) {
			agent := newTestAgent(t)
			_, cnClient := startAgent(t, agent)
			setBehavior(t, agent, behavior)
			ctx := context.Background()
			if _, err := cnClient.SyncupCn(ctx, &pb.SyncupCnRequest{
				ClusterId: 1, CnId: 1, Revision: 1,
				CntlrPointerList: []*pb.CntlrPointer{cntlrPtr(1, 1)},
			}); err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
			req := cntlrFixture(shape.primary)
			req.GetCntlr().Disabled = shape.disabled
			req.NameToRepair = map[string]*pb.Repair{
				"r0": {RepId: 9, GrpId: 12, NewLegList: []*pb.Leg{
					{LegId: 105},
				}},
				"r1": {RepId: 10, GrpId: 21, NewLegList: []*pb.Leg{
					{LegId: 203},
				}},
				"r2": {RepId: 11, GrpId: 71, NewLegList: []*pb.Leg{
					{LegId: 714},
				}},
			}
			// Marks on groups 11 and 71: the default names 11's, and the
			// lever says 71's was never acted on.
			for _, slice := range req.GetIdToSlice() {
				for _, grp := range append(slice.GetMetaGrpList(),
					slice.GetDataGrpList()...) {
					if grp.GetGrpId() == 11 || grp.GetGrpId() == 71 {
						grp.ForcedStart = &pb.ForcedStart{
							LegId:    grp.GetLegList()[0].GetLegId(),
							Revision: 7,
						}
					}
				}
			}
			reply, err := cnClient.SyncupCntlr(ctx, req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			info := reply.GetCntlrInfo()
			wantKeys(t, "leg_id_to_leg", info.GetLegIdToLeg(),
				101, 102, 103, 104, 105, 201, 202, 203, 711, 712, 713, 714)
			if !primary {
				wantKeys(t, "rep_id_to_repair_info",
					info.GetRepIdToRepairInfo())
				wantKeys(t, "grp_id_to_grp_info", info.GetGrpIdToGrpInfo())
				return
			}
			wantKeys(t, "rep_id_to_repair_info",
				info.GetRepIdToRepairInfo(), 9, 11)
			if v := info.GetRepIdToRepairInfo()[9]; !v.GetStarted() ||
				v.GetAbortConfirmed() || v.GetRefusal() != "" ||
				fmt.Sprint(v.GetHeldNewLegIdList()) != "[105]" {
				t.Errorf("verdict on rep 9 = %v, want started, leg 105 held",
					v)
			}
			if v := info.GetRepIdToRepairInfo()[11]; v.GetStarted() ||
				v.GetAbortConfirmed() ||
				v.GetRefusal() != "leg 0x2ca holds newer data" {
				t.Errorf("verdict on rep 11 = %v, want the refusal alone", v)
			}
			wantKeys(t, "grp_id_to_grp_info", info.GetGrpIdToGrpInfo(),
				11, 12, 21, 71)
			for _, tc := range []struct {
				grpId  uint64
				whole  bool
				inSync []uint64
				acted  uint64
			}{
				{11, true, []uint64{101, 102}, 7},
				{12, false, []uint64{103, 104}, 0},
				{21, true, nil, 0},
				{71, false, []uint64{712}, 0},
			} {
				got := info.GetGrpIdToGrpInfo()[tc.grpId]
				if got.GetWhole() != tc.whole ||
					fmt.Sprint(got.GetInSyncLegIdList()) !=
						fmt.Sprint(tc.inSync) ||
					got.GetForcedStartRevision() != tc.acted {
					t.Errorf("group %d reading = %v, want whole %v in sync "+
						"%v acted %d", tc.grpId, got, tc.whole, tc.inSync,
						tc.acted)
				}
			}
		})
	}

	for _, tc := range []struct {
		name string
		data string
	}{
		{"repairs on a side",
			`{"objects": {"side 1:3:5": {"repairs": {"r0": {}}}}}`},
		{"grps on the dn", `{"objects": {"dn": {"grps": {"1": {}}}}}`},
		{"repairs in default", `{"default": {"repairs": {"r0": {}}}}`},
		{"grps in default", `{"default": {"grps": {"1": {}}}}`},
		{"a grp key that is no id",
			`{"objects": {"cntlr 1:1": {"grps": {"g1": {}}}}}`},
		{"an unknown verdict key",
			`{"objects": {"cntlr 1:1": {"repairs": {"r0": {"done": true}}}}}`},
		{"a refusal that is no text",
			`{"objects": {"cntlr 1:1": {"repairs": {"r0": {"refusal": true}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBehavior([]byte(tc.data)); err == nil {
				t.Fatalf("ParseBehavior(%s) succeeded, want an error", tc.data)
			}
		})
	}
	err := newTestAgent(t).Update(SideKey(1, 3, 5), func(o *Object) {
		o.Grps = map[uint64]GrpReading{1: {}}
	})
	if err == nil {
		t.Fatalf("Update set grps on a side")
	}
}

// TestReadLeftoverLever is the leftover only reads name (ReadLeftover): each
// round answers from the latest observation and starts a read the next round
// answers from (dnagent.md SH29, SH30). A read names the leftover and finds
// its read rows; an accepted SyncupCntlr answers code zero with the rows the
// other levers give, cancels the read the round before it started and spends
// one re-send, so the round after it answers from its observation; a refused
// one cancels nothing. Once the re-sends are spent the reads answer code
// zero. The lever is refused where it cannot act.
func TestReadLeftoverLever(t *testing.T) {
	agent := newTestAgent(t)
	_, cnClient := startAgent(t, agent)
	fixture := syncupCntlrFixture(t, cnClient, true)
	key := CntlrKey(1, 1)
	missing := pb.ResStatus_RES_STATUS_MISSING
	update(t, agent, key, func(ob *Object) {
		ob.ReadLeftover = 2
		ob.ReadRows = map[string]Row{
			"grp_id_to_md_raid.12": {Status: &missing, WhenPrimary: true},
		}
	})
	round := func(traceId string) *pb.CheckCntlrReply {
		t.Helper()
		return oneRound(t, cnClient.CheckCntlr, &pb.CheckCntlrRequest{
			ClusterId: 1, CnId: 1, CntlrPointer: cntlrPtr(1, 1), Revision: 5,
			ShowInfo: true, TraceId: traceId,
		})
	}
	resend := func(traceId string, revision uint64) *pb.SyncupCntlrReply {
		t.Helper()
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			common.TraceIdMetadataKey, traceId)
		req := cntlrFixture(true)
		req.Revision = revision
		reply, err := cnClient.SyncupCntlr(ctx, req)
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		return reply
	}
	// want checks that a round answered from the observation made under
	// obsTrace at obsAt (zero: any time), with code and the data group's
	// status.
	want := func(
		what string, reply *pb.CheckCntlrReply, code uint32,
		obsTrace string, obsAt time.Time, grp pb.ResStatus,
	) time.Time {
		t.Helper()
		stamp := reply.GetObsStamp()
		obs, _ := obsTimes(stamp)
		got := reply.GetCntlrInfo().GetGrpIdToMdRaid()[12].GetStatus()
		if reply.GetAgentReply().GetCode() != code ||
			stamp.GetObsTraceId() != obsTrace ||
			(!obsAt.IsZero() && !obs.Equal(obsAt)) || got != grp {
			t.Fatalf("%s: code %d, observation %q at %v, group %v; want "+
				"code %d, observation %q at %v, group %v", what,
				reply.GetAgentReply().GetCode(), stamp.GetObsTraceId(), obs,
				got, code, obsTrace, obsAt, grp)
		}
		return obs
	}
	fixtureObs, _ := obsTimes(fixture.GetObsStamp())
	ok := pb.ResStatus_RES_STATUS_OK
	want("the first round", round("r1"), 0,
		fixture.GetObsStamp().GetObsTraceId(), fixtureObs, ok)
	want("the round after it", round("r2"), common.ReplyCodeLeftover, "r1",
		time.Time{}, missing)

	first := resend("r1", 5)
	if first.GetAgentReply().GetCode() != 0 || first.GetCntlrInfo().
		GetGrpIdToMdRaid()[12].GetStatus() != ok {
		t.Fatalf("the re-send: %v, group %v; want code 0 and the group as "+
			"the converge builds it", first.GetAgentReply(), first.
			GetCntlrInfo().GetGrpIdToMdRaid()[12])
	}
	firstObs, _ := obsTimes(first.GetObsStamp())
	want("the round after the re-send", round("r3"), 0, "r1", firstObs, ok)
	want("the next round", round("r4"), common.ReplyCodeLeftover, "r3",
		time.Time{}, missing)

	// A refused syncup cancels no read: the next round answers from r4's.
	if code := resend("stale", 4).GetAgentReply().GetCode(); code !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("a stale SyncupCntlr: code %d", code)
	}
	want("the round after a refused syncup", round("r5"),
		common.ReplyCodeLeftover, "r4", time.Time{}, missing)

	second := resend("r4", 5)
	secondObs, _ := obsTimes(second.GetObsStamp())
	want("the round after the last re-send", round("r6"), 0, "r4", secondObs,
		ok)
	want("a read after the leftover was spent", round("r7"), 0, "r6",
		time.Time{}, ok)
	want("the read after it", round("r8"), 0, "r7", time.Time{}, ok)

	for _, tc := range []struct {
		name string
		fn   func(*Object)
	}{
		{"read rows with no read leftover", func(ob *Object) {
			ob.ReadLeftover = 0
		}},
		{"a read leftover beside leftover rounds", func(ob *Object) {
			ob.LeftoverRounds = 1
		}},
		{"a read leftover beside an old observation", func(ob *Object) {
			ob.ObsAge = time.Second
		}},
	} {
		if err := agent.Update(key, tc.fn); err == nil {
			t.Errorf("Update accepted %s", tc.name)
		}
	}
	if err := agent.Update(DnKey, func(ob *Object) {
		ob.ReadLeftover = 1
		ob.ReadRows = map[string]Row{"meta": {Status: &missing,
			WhenPrimary: true}}
	}); err == nil {
		t.Errorf("Update accepted a when_primary read row on the dn")
	}
	if _, err := ParseBehavior([]byte(
		`{"default": {"read_leftover": 1}}`)); err == nil {
		t.Errorf("ParseBehavior accepted read_leftover on the default entry")
	}
	parsed, err := ParseBehavior([]byte(`{"objects": {"cntlr 1:1": {
	  "read_leftover": 3,
	  "read_rows": {"grp_id_to_md_raid.12": {"status": "MISSING",
	                                         "when_primary": true}}}}}`))
	if err != nil {
		t.Fatalf("ParseBehavior: %v", err)
	}
	ob := parsed.Objects[key]
	if row := ob.ReadRows["grp_id_to_md_raid.12"]; ob.ReadLeftover != 3 ||
		row.Status == nil || *row.Status != missing || !row.WhenPrimary {
		t.Errorf("parsed read leftover = %d, read rows %+v", ob.ReadLeftover,
			ob.ReadRows)
	}
}

// TestGetCnStoreAnswersTheStoredRequests pins the fake's GetCnStore
// (dnv-worker.md, Integration test plan, The fake agent): it answers its
// node's last accepted SyncupCnRequest and a summary of each cntlr the
// pointer list names that holds a request, as the cn agent does
// (cnagent.md CN32): the thin devices, the NQNs in order, and the largest
// side id and the count of the sides of the slices' groups, the clone's
// meta group, the repair's new leg and each retired leg a group lists, by
// the side id the request gives it, one with none counted and marking the
// summary as missing one; another cluster or node is an unknown object; the
// Starting and reply_code levers set the code.
func TestGetCnStoreAnswersTheStoredRequests(t *testing.T) {
	agent := newTestAgent(t)
	_, cnClient := startAgent(t, agent)
	ctx := context.Background()
	store := func(clusterId, cnId uint64) *pb.GetCnStoreReply {
		t.Helper()
		reply, err := cnClient.GetCnStore(ctx, &pb.GetCnStoreRequest{
			ClusterId: clusterId, CnId: cnId})
		if err != nil {
			t.Fatalf("GetCnStore: %v", err)
		}
		return reply
	}
	if code := store(1, 0).GetAgentReply().GetCode(); code !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("a fake that holds nothing: code %d, want unknown", code)
	}

	req := cntlrFixture(true)
	req.TdList = []*pb.ThinDevice{{TdId: 7, DevId: 3, Size: 1 << 30}}
	req.NqnToSubsystem = map[string]*pb.Subsystem{"nqn.b": {}, "nqn.a": {}}
	slice := req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, 1)]
	slice.GetMetaGrpList()[0].GetLegList()[0].SideList =
		[]*pb.Side{{SideId: 501}}
	slice.GetDataGrpList()[0].GetLegList()[1].SideList =
		[]*pb.Side{{SideId: 502}, {SideId: 509}}
	slice.GetDataGrpList()[0].RetiredLegIdList = []uint64{105, 107}
	req.RetiredLegIdToSideId = map[uint64]uint64{105: 9000}
	req.NameToRepair = map[string]*pb.Repair{"r": {NewLegList: []*pb.Leg{
		{LegId: 106, SideList: []*pb.Side{{SideId: 520}}}}}}
	syncupCntlrRequest(t, cnClient, req)
	if _, err := cnClient.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 2,
		CntlrPointerList: []*pb.CntlrPointer{cntlrPtr(1, 1), cntlrPtr(2, 1)},
	}); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	want := &pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:     1,
			Revision: 2,
			CntlrPointerList: []*pb.CntlrPointer{
				cntlrPtr(1, 1), cntlrPtr(2, 1)},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{{
				CntlrPointer: cntlrPtr(1, 1),
				Revision:     5,
				Primary:      true,
				TdList:       req.GetTdList(),
				NqnList:      []string{"nqn.a", "nqn.b"},
				// The clone's meta group sides 7111 to 7131 count; the
				// slices hold 501, 502 and 509 and the retired legs 105
				// (side 9000) and 107 (no side id in the request), and the
				// repair its new leg's 520.
				MaxSideId: 9000,
				SideCnt:   9,

				RetiredSideIdMissing: true,
			}},
		}},
	}
	for _, cnId := range []uint64{0, 1} {
		if got := store(1, cnId); !proto.Equal(got, want) {
			t.Errorf("cn_id %d:\n got %v\nwant %v", cnId, got, want)
		}
	}
	for _, ask := range [][2]uint64{{2, 0}, {1, 2}} {
		if code := store(ask[0], ask[1]).GetAgentReply().GetCode(); code !=
			common.ReplyCodeUnknownObject {
			t.Errorf("cluster %d, cn %d: code %d, want unknown", ask[0],
				ask[1], code)
		}
	}

	update(t, agent, CnKey, func(ob *Object) { ob.ReplyCode = 4 })
	if got := store(1, 0); got.GetAgentReply().GetCode() != 4 ||
		len(got.GetCnList()) != 0 {
		t.Errorf("the reply_code lever: %v, want code 4 and no node", got)
	}
	update(t, agent, CnKey, func(ob *Object) {
		ob.ReplyCode = 0
		ob.Starting = time.Now()
	})
	if code := store(1, 0).GetAgentReply().GetCode(); code !=
		common.ReplyCodeStarting {
		t.Errorf("the Starting lever: code %d, want starting", code)
	}
	if got := agent.Calls(MethodGetCnStore); got != 7 {
		t.Errorf("GetCnStore counted %d calls, want 7", got)
	}
}
