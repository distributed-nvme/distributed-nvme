package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// inspect is InspectGrp of the fixture's data group, with a dry run from
// legId when it is not zero, passing the kinds of refusal overrides names.
func (f *repFix) inspect(
	t *testing.T,
	legId uint64,
	overrides ...pb.ForcedStartOverride,
) *pb.InspectGrpReply {
	t.Helper()
	reply, err := f.srv.InspectGrp(f.ctx, &pb.InspectGrpRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, GrpId: testDataGrp, LegId: legId,
		OverrideList: overrides,
	})
	if err != nil {
		t.Fatalf("InspectGrp: %v", err)
	}
	return reply
}

// memberOf is the reply's member for legId, or nil.
func memberOf(reply *pb.InspectGrpReply, legId uint64) *pb.InspectedMember {
	for _, member := range reply.GetMemberList() {
		if member.GetLegId() == legId {
			return member
		}
	}
	return nil
}

// TestInspectGrpReadsEveryMember pins InspectGrp's read of a running array
// (cnagent.md CN31): every leg_list leg and the repair's new leg,
// each with whether it serves, its superblock read and md's view of it,
// and the array's state; it runs no mdadm command and changes nothing; a
// dry run over the running array is refused, as the primary leaves a mark
// waiting while the array runs.
func TestInspectGrpReadsEveryMember(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	before := f.members()
	f.node.Reset()
	reply := f.inspect(t, 0)
	if !strings.Contains(reply.GetArrayState(), "clean") &&
		!strings.Contains(reply.GetArrayState(), "active") {
		t.Errorf("array state %q, want the running array's",
			reply.GetArrayState())
	}
	if reply.GetDryRun() != nil {
		t.Errorf("a dry run with no leg named: %v", reply.GetDryRun())
	}
	if got := len(reply.GetMemberList()); got != 3 {
		t.Fatalf("%d members, want both legs and the new leg: %v", got,
			reply.GetMemberList())
	}
	// Leg 1 is the repair's bad leg, leg 2 a plain leg of the group.
	for legId, kind := range map[uint64]string{
		testDataLeg: inspectKindBadLeg, testDataLeg2: inspectKindLeg,
	} {
		m := memberOf(reply, legId)
		if m.GetKind() != kind || !m.GetAvailable() ||
			!m.GetHasSuperblock() || m.GetUuid() == "" ||
			!strings.Contains(m.GetMdState(), "in_sync") {
			t.Errorf("leg %d reads %v, want a serving in-sync %s with "+
				"a superblock", legId, m, kind)
		}
	}
	if m := memberOf(reply, testDataLeg); m.GetUuid() !=
		memberOf(reply, testDataLeg2).GetUuid() {
		t.Errorf("the two legs read identities %q and %q, want one",
			m.GetUuid(), memberOf(reply, testDataLeg2).GetUuid())
	}
	if m := memberOf(reply, repNewLeg); m.GetKind() != inspectKindNewLeg ||
		m.GetMdState() != "" {
		t.Errorf("the new leg reads %v, want a new leg md does not hold", m)
	}
	assertNoCall(t, f.node, "cmd mdadm")
	if after := f.members(); !slices.Equal(after, before) {
		t.Errorf("the inspect changed the array's members to %v", after)
	}
	dry := f.inspect(t, testDataLeg2).GetDryRun()
	if !strings.Contains(dry.GetRefusal(), "runs") ||
		len(dry.GetZeroLegIdList()) != 0 {
		t.Errorf("the dry run over the running array: %v, want refused", dry)
	}
}

// TestInspectGrpDryRunMatchesTheStart pins the dry run (cnagent.md
// CN31): with no array of the group there, a dry run from a full
// member names the members the start would zero and its update, with no
// mdadm command, and the real start from the same mark then zeroes exactly
// those; a dry run from a leg the start refuses carries the start's own
// refusal.
func TestInspectGrpDryRunMatchesTheStart(t *testing.T) {
	f := downNoRepair(t)
	// A first mark the start refuses, naming a half-copied leg 2, makes the
	// cntlr primary with no array of the group and nothing zeroed.
	f.node.setSuperblock(f.leg2, fakeSuperblock{
		role: "Active device 1", recovery: "45056 sectors"})
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	refusal := info.GetGrpIdToMdRaid()[testDataGrp].GetDetails()
	if !strings.Contains(refusal, "not a full member") {
		t.Fatalf("the first start: %q, want refused", refusal)
	}
	f.node.Reset()
	dry := f.inspect(t, testDataLeg2).GetDryRun()
	if dry.GetRefusal() == "" ||
		!strings.Contains(refusal, dry.GetRefusal()) {
		t.Errorf("the dry run from leg 2 refuses %q, want the start's own "+
			"refusal %q", dry.GetRefusal(), refusal)
	}
	dry = f.inspect(t, testDataLeg).GetDryRun()
	if dry.GetRefusal() != "" ||
		!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg2}) ||
		dry.GetUpdate() != mdUpdateUuid || dry.GetStopsArray() {
		t.Fatalf("the dry run from leg 1: %v, want leg 2 zeroed under %s",
			dry, mdUpdateUuid)
	}
	assertNoCall(t, f.node, "cmd mdadm")
	f.node.Reset()
	f.syncup(t, withMark(quietReq(5), testDataLeg))
	assertOrder(t, f.node, zeroOf(f.leg2), f.forced(f.leg1, mdUpdateUuid))
	assertNoCall(t, f.node, zeroOf(f.leg1))
}

// TestInspectGrpDryRunCarriesTheOverrides pins the dry run's overrides
// (cnagent.md CN31): a named leg whose change count is below that of a full
// member of its identity the start would zero is refused without the
// override older-leg, as the real start refuses it, and with it the dry run
// names that member zeroed.
func TestInspectGrpDryRunCarriesTheOverrides(t *testing.T) {
	f := downNoRepair(t)
	f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
	f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
	// The real start refuses the older leg, which leaves the cntlr primary
	// with no array of the group.
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	refusal := info.GetGrpIdToMdRaid()[testDataGrp].GetDetails()
	if !strings.Contains(refusal, "is a full member with md change count") {
		t.Fatalf("the start: %q, want the older leg refused", refusal)
	}
	f.node.Reset()
	dry := f.inspect(t, testDataLeg2).GetDryRun()
	if dry.GetRefusal() == "" || !strings.Contains(refusal, dry.GetRefusal()) {
		t.Errorf("the dry run with no override refuses %q, want the start's %q",
			dry.GetRefusal(), refusal)
	}
	dry = f.inspect(t, testDataLeg2,
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_OLDER_LEG).GetDryRun()
	if dry.GetRefusal() != "" ||
		!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg}) {
		t.Errorf("the dry run with the override older-leg: %v, want leg 1 "+
			"zeroed", dry)
	}
	assertNoCall(t, f.node, "cmd mdadm")
}

// TestInspectGrpDryRunOverAnArrayThatDoesNotRun pins the dry run over an
// array of the group that is there and does not run (cnagent.md CN31): it
// says the start stops it first, and refuses an array that holds a member
// that is no leg wrapper of this pool, as the real start refuses to stop
// one, word for word; it stops nothing itself.
func TestInspectGrpDryRunOverAnArrayThatDoesNotRun(t *testing.T) {
	t.Run("ours, broken", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setArrayState(f.mdDev, "broken")
		f.node.Reset()
		dry := f.inspect(t, testDataLeg2).GetDryRun()
		if dry.GetRefusal() != "" || !dry.GetStopsArray() ||
			!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg}) {
			t.Fatalf("dry run %v, want the stop first and leg 1 zeroed", dry)
		}
		assertNoCall(t, f.node, "cmd mdadm")
	})
	t.Run("another sp's leg in it", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, levelReq(3, pb.SpLevel_SP_LEVEL_NO_THINPOOL))
		f.node.mu.Lock()
		array := f.node.arrays[f.mdDev]
		held := append(append([]string(nil), array.members...),
			"/dev/mapper/"+names(f.srv).CnLegName(
				testCluster, testCn, testSp+1, testDataLeg))
		name := array.name
		f.node.mu.Unlock()
		f.node.seedArray(f.mdDev, name, held...)
		f.node.setArrayState(f.mdDev, "inactive")
		f.node.Reset()
		dry := f.inspect(t, testDataLeg2).GetDryRun()
		if !strings.Contains(dry.GetRefusal(), "which is no leg of this sp") {
			t.Fatalf("dry run %v, want the real start's refusal to stop "+
				"the array", dry)
		}
		assertNoCall(t, f.node, "cmd mdadm")
	})
}

// TestInspectGrpLeavesAnAbortedRepairsLegOut: the new leg of an ABORTED
// repair is no member of the group, so the inspect does not list it.
func TestInspectGrpLeavesAnAbortedRepairsLegOut(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_ABORTED, true))
	reply := f.inspect(t, 0)
	if m := memberOf(reply, repNewLeg); m != nil {
		t.Fatalf("the aborted repair's new leg is listed: %v", m)
	}
}

// TestInspectGrpEndsAtItsDeadline pins InspectGrp's locks (cnagent.md
// CN31): a member read that stalls holds neither the node lock nor the
// cntlr's object lock, and the call answers DEADLINE_EXCEEDED at its
// deadline while the read goes on.
func TestInspectGrpEndsAtItsDeadline(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	key := "readsb " + f.leg2
	f.node.blockCmd(key)
	t.Cleanup(func() { f.node.releaseCmd(key) })
	ctx, cancel := context.WithTimeout(f.ctx, 200*time.Millisecond)
	defer cancel()
	_, err := f.srv.InspectGrp(ctx, &pb.InspectGrpRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, GrpId: testDataGrp,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("InspectGrp over a stalled read: %v, want DEADLINE_EXCEEDED",
			err)
	}
	node := f.srv.locks.Node()
	if !node.TryLock() {
		t.Fatalf("the stalled read holds the node lock")
	}
	node.Unlock()
	obj := f.srv.locks.Obj(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if !obj.TryLock() {
		t.Fatalf("the stalled read holds the cntlr's object lock")
	}
	obj.Unlock()
}

// TestInspectGrpReadsMembersOverAFailedArrayRead pins a failed read of the
// group's array (cnagent.md CN31): the members are read all the same, the
// array's state carries the read's error, and the dry run is refused with
// it, as the real start ends on it.
func TestInspectGrpReadsMembersOverAFailedArrayRead(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.mu.Lock()
	f.node.failReadAlways["/md/array_state"] = true
	f.node.mu.Unlock()
	f.node.Reset()
	reply := f.inspect(t, testDataLeg2)
	if !strings.HasPrefix(reply.GetArrayState(), "not read: ") {
		t.Errorf("array state %q, want the read's error",
			reply.GetArrayState())
	}
	if m := memberOf(reply, testDataLeg2); !m.GetHasSuperblock() {
		t.Errorf("leg 2 reads %v, want its superblock read", m)
	}
	if dry := reply.GetDryRun(); dry.GetRefusal() == "" ||
		!strings.Contains(reply.GetArrayState(), dry.GetRefusal()) {
		t.Errorf("dry run %v, want the array read's error as the refusal",
			dry)
	}
	assertNoCall(t, f.node, "cmd mdadm")
}

// TestInspectGrpReadsNoLegThatDoesNotServe pins the serving test of
// InspectGrp (cnagent.md CN31): a member whose leg does not serve, here
// the new leg whose disk node refuses the connect, is reported as not
// serving, with no read of it.
func TestInspectGrpReadsNoLegThatDoesNotServe(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.newLegUnreachable()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.node.Reset()
	m := memberOf(f.inspect(t, 0), repNewLeg)
	if m.GetAvailable() || m.GetHasSuperblock() ||
		!strings.Contains(m.GetReadError(), "does not serve") {
		t.Errorf("the unreachable new leg reads %v, want not serving", m)
	}
	assertNoCall(t, f.node, "readsb "+f.newLegDev)
}

// ---------------------------------------------------------------------------
// CN23's exception: the counters and probe times an inspect call reads
// ---------------------------------------------------------------------------

// statText is a sysfs "stat" file whose seventeen counters are base, base+1,
// ..., base+16, padded as the kernel pads them.
func statText(base uint64) string {
	var sb strings.Builder
	for i := uint64(0); i < 17; i++ {
		fmt.Fprintf(&sb, " %8d", base+i)
	}
	return sb.String() + "\n"
}

// plantStat gives the dm device name a "stat" file under its number whose
// counters start at base.
func plantStat(t *testing.T, node *fakeNode, name string, base uint64) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	devNo := node.devNo["/dev/mapper/"+name]
	if devNo == "" {
		t.Fatalf("%s has no device number", name)
	}
	node.files["/sys/dev/block/"+devNo+"/stat"] = statText(base)
}

// wantStat checks that an entry read the counters plantStat planted at base.
func wantStat(t *testing.T, io *pb.DevIoStats, name string, base uint64) {
	t.Helper()
	if io.GetStat() == nil || io.GetReadError() != "" {
		t.Fatalf("entry %v, want the counters of %s", io, name)
	}
	stat := io.GetStat()
	if stat.GetReadIos() != base || stat.GetWriteIos() != base+4 ||
		stat.GetInFlight() != base+8 || stat.GetFlushTicksMs() != base+16 {
		t.Fatalf("%s's counters %v, want base %d in the kernel's order",
			name, stat, base)
	}
}

// statReads are the calls of a test's node that read a device's number or a
// "stat" file.
func statReads(node *fakeNode) []string {
	var out []string
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "devno ") ||
			strings.HasSuffix(call, "/stat") {
			out = append(out, call)
		}
	}
	return out
}

// TestInspectReadsProbeTimesAndCounters pins the inspect call's read (CN11,
// CN23): with read_io_stats, GetCntlrInfo carries each leg's last probe
// round — its start, its write half and its read half, the end of the last
// round that succeeded — and each leg wrapper's and the ns-dev's counters,
// read at the call. A round whose write fails reports its write half alone
// and keeps the earlier success; a leg no round has probed carries no times;
// a device whose counters do not read carries the read's error and no
// counters, never zeros. The read takes no CN1 lock: it answers while the
// test holds the node's write lock.
func TestInspectReadsProbeTimesAndCounters(t *testing.T) {
	srv, node := newTestServer(t)
	clock := withClock(srv)
	srv.probeInterval = time.Hour
	failWrite := false
	srv.probeIO = &fakeProbeIO{
		WriteFn: func(context.Context, string, uint64, []byte) error {
			clock.advance(30 * time.Millisecond)
			if failWrite {
				return fmt.Errorf("input/output error")
			}
			return nil
		},
		ReadDirectFn: func(context.Context, string, uint64,
			uint64) ([]byte, error) {
			clock.advance(20 * time.Millisecond)
			return nil, nil
		},
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	st := legState(t, srv)
	srv.mu.Lock()
	prober := st.probers[testDataLeg]
	srv.mu.Unlock()
	first := clock.Now()
	srv.runLegProbe(context.Background(), prober, testCn)
	succeeded := clock.Now()
	clock.advance(time.Second)
	failWrite = true
	second := clock.Now()
	srv.runLegProbe(context.Background(), prober, testCn)
	plantStat(t, node, legName(srv, testDataLeg), 100)
	plantStat(t, node, nsDevName(srv, testNs), 300)

	node.Reset()
	srv.locks.Node().Lock()
	got := make(chan *pb.GetCntlrInfoReply, 1)
	go func() {
		reply, err := srv.GetCntlrInfo(context.Background(),
			&pb.GetCntlrInfoRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr(), ReadIoStats: true,
			})
		if err != nil {
			t.Errorf("GetCntlrInfo: %v", err)
		}
		got <- reply
	}()
	var reply *pb.GetCntlrInfoReply
	select {
	case reply = <-got:
	case <-time.After(5 * time.Second):
		srv.locks.Node().Unlock()
		t.Fatalf("GetCntlrInfo with read_io_stats waited on the node lock")
	}
	srv.locks.Node().Unlock()

	io := reply.GetIoStats()
	data := io.GetLegIdToIo()[testDataLeg]
	wantStat(t, data.GetDev(), legName(srv, testDataLeg), 100)
	// The ages count back from the answer, at the failed round's end.
	times := data.GetProbeTimes()
	if times.GetRoundAgeMs() != 30 || times.GetWriteMs() != 30 ||
		times.ReadMs != nil || times.LastSuccessAgeMs == nil ||
		times.GetLastSuccessAgeMs() != uint32(clock.Now().Sub(succeeded)/
			time.Millisecond) {
		t.Fatalf("data leg's probe times %v, want the failed round begun "+
			"30 ms before the answer (write 30 ms, no read) and the success "+
			"that ended %v before it", times, clock.Now().Sub(succeeded))
	}
	if second.Sub(succeeded) != time.Second {
		t.Fatalf("the rounds are %v apart", second.Sub(succeeded))
	}
	if succeeded.Sub(first) != 50*time.Millisecond {
		t.Fatalf("the first round took %v", succeeded.Sub(first))
	}
	meta := io.GetLegIdToIo()[testMetaLeg]
	if meta.GetProbeTimes() != nil {
		t.Fatalf("the meta leg, which no round probed, carries times %v",
			meta.GetProbeTimes())
	}
	if meta.GetDev().GetStat() != nil ||
		!strings.Contains(meta.GetDev().GetReadError(), "is not there") {
		t.Fatalf("the meta leg's wrapper has no stat file, and its entry "+
			"reads %v", meta.GetDev())
	}
	wantStat(t, io.GetNsIdToIo()[testNs], nsDevName(srv, testNs), 300)
	if len(io.GetLegIdToIo()) != 2 || len(io.GetNsIdToIo()) != 1 {
		t.Fatalf("io stats %v, want two legs and one ns-dev", io)
	}

	// The times of a round that succeeded: both halves. The clock moves on
	// a millisecond at every read of it during the call, so the ages show
	// which read they count from: the answer time the stamp carries.
	failWrite = false
	clock.advance(time.Second)
	third := clock.Now()
	srv.runLegProbe(context.Background(), prober, testCn)
	ended := clock.Now()
	srv.now = func() time.Time {
		clock.advance(time.Millisecond)
		return clock.Now()
	}
	reply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), ReadIoStats: true})
	srv.now = clock.Now
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	answered := reply.GetObsStamp().GetAnswerTime().AsTime()
	times = reply.GetIoStats().GetLegIdToIo()[testDataLeg].GetProbeTimes()
	if times.GetRoundAgeMs() != uint32(answered.Sub(third)/
		time.Millisecond) || times.GetWriteMs() != 30 ||
		times.ReadMs == nil || times.GetReadMs() != 20 ||
		times.LastSuccessAgeMs == nil ||
		times.GetLastSuccessAgeMs() != uint32(answered.Sub(ended)/
			time.Millisecond) {
		t.Fatalf("a round that succeeded reports %v, want its ages counted "+
			"back from the answer time %v (round began %v, ended %v)",
			times, answered, third, ended)
	}
}

// TestInfoReadsAndRoundsReadNoCounters pins the reach of CN23's exception:
// a GetCntlrInfo without read_io_stats, a cntlr's read and its check rounds
// read no device number and no "stat" file, and carry no counters, so the
// rounds' replies stay equal while the counters move and the worker sees no
// change; only the flagged call reads them.
func TestInfoReadsAndRoundsReadNoCounters(t *testing.T) {
	srv, node := newTestServer(t)
	srv.probeInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		plantStat(t, node, legName(srv, legId), 10)
	}
	plantStat(t, node, nsDevName(srv, testNs), 10)

	node.Reset()
	plain, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	if plain.GetIoStats() != nil {
		t.Fatalf("an info read without the flag carries %v",
			plain.GetIoStats())
	}
	before := rowRound(t, srv, 2)
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		plantStat(t, node, legName(srv, legId), 5000)
	}
	plantStat(t, node, nsDevName(srv, testNs), 5000)
	after := rowRound(t, srv, 2)
	if reads := statReads(node); len(reads) != 0 {
		t.Fatalf("an info read, two reads and two rounds read counters: %v",
			reads)
	}
	if !proto.Equal(before.GetCntlrInfo(), after.GetCntlrInfo()) ||
		before.GetAgentReply().GetCode() != after.GetAgentReply().GetCode() {
		t.Fatalf("the rounds' replies moved with the counters:\n%v\n%v",
			before, after)
	}

	flagged, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), ReadIoStats: true})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	if len(statReads(node)) == 0 {
		t.Fatalf("the flagged call read no counters")
	}
	wantStat(t, flagged.GetIoStats().GetNsIdToIo()[testNs],
		nsDevName(srv, testNs), 5000)
}
