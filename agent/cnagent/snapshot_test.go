package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// U1 — a snapshot is point-in-time across slices (update_02.md U1-T2)
// ---------------------------------------------------------------------------
//
// A td is a dm-striped raid0 across one thin volume per slice (§3.3), so a
// snapshot needs one `create_snap` per slice. Sent independently, slice 0's
// snapshot and slice 1's snapshot date from different instants and any host
// write landing between them is in one and not the other — a torn snapshot,
// worse than crash-consistent. The primary therefore suspends the origin td's
// raid0 around the whole per-slice message sequence (CN14).

// snapTds is the U1 fixture's td_list: a live origin (dev_id 1) and the
// snapshot taken of it. ori_id is the *origin's dev_id*, never its td_id.
func snapTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, OriId: 1, Size: testTdSize},
	}
}

// snapMessage is one slice's create_snap, exactly as the agent records it.
func snapMessage(srv *CnAgentServer, sliceId uint64) string {
	return "cmd dmsetup message " + poolNameOf(srv, sliceId) +
		" 0 create_snap 2 1"
}

// syncupSnapshot converges the origin alone, then applies o with the snapshot
// added. The two passes are the point: only after the first one is the
// origin's stack live, which is the state the U1 quiesce exists for.
func syncupSnapshot(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// TestSnapshotQuiescesOriginRaid0 is U1-T2 case 1: the origin's raid0 is
// suspended across *every* slice's create_snap, each of which keeps its own
// nested per-slice origin-thin suspend, and the snap thin devices are created
// only after the raid0 resumes — their content was fixed at message time.
func TestSnapshotQuiescesOriginRaid0(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()})

	origin := raid0Name(srv, testTd)
	assertOrder(t, node,
		"cmd dmsetup suspend "+origin,
		"cmd dmsetup suspend "+thinNameOf(srv, testTd, testSlice),
		snapMessage(srv, testSlice),
		"cmd dmsetup resume "+thinNameOf(srv, testTd, testSlice),
		"cmd dmsetup suspend "+thinNameOf(srv, testTd, testSlice2),
		snapMessage(srv, testSlice2),
		"cmd dmsetup resume "+thinNameOf(srv, testTd, testSlice2),
		"cmd dmsetup resume "+origin,
		"cmd dmsetup create "+thinNameOf(srv, testSnapTd, testSlice),
		"cmd dmsetup create "+thinNameOf(srv, testSnapTd, testSlice2),
	)
	// assertOrder is a subsequence check, so pin the bracket exactly: both
	// messages sit strictly between the one suspend and the one resume.
	suspend := node.indexOfCall("cmd dmsetup suspend " + origin)
	resume := node.indexOfCall("cmd dmsetup resume " + origin)
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		if at := node.indexOfCall(snapMessage(srv, sliceId)); at < suspend ||
			at > resume {
			t.Fatalf("create_snap of slice %#x at %d is outside the raid0 "+
				"bracket [%d, %d]", sliceId, at, suspend, resume)
		}
	}
	// Exactly one message per slice. indexOfCall and assertOrder both stop at
	// the first match, so only a count catches a second create_snap emitted
	// after the resume — which is what a broken snapDone handoff produces.
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		if n := len(node.callsMatching(snapMessage(srv, sliceId))); n != 1 {
			t.Fatalf("slice %#x: %d create_snap calls, want exactly 1",
				sliceId, n)
		}
	}
	if node.dms[origin].suspended {
		t.Fatalf("the origin raid0 is still suspended")
	}

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice], "snap thin slice 0")
	assertOk(t, info.GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice2], "snap thin slice 1")
	assertOk(t, info.GetTdIdToRaid0()[testSnapTd], "snap raid0")
}

// TestSnapshotResumesRaid0AfterAFailedMessage is U1-T2 case 2 ([D12]): a
// scripted failure of the *second* slice's create_snap still resumes the
// origin's raid0. No path out of the sequence may leave a device suspended —
// a suspended raid0 stalls the td's host IO and wedges any scanner that opens
// it, unkillably, until the next converge.
func TestSnapshotResumesRaid0AfterAFailedMessage(t *testing.T) {
	srv, node := newTestServer(t)
	// The key carries the full pool name *and* create_snap, so it can never
	// catch slice 0's message, a create_thin or a dmsetup create. failCmd
	// would be consumed by the first line matching it; failCmdAlways is
	// persistent.
	node.failCmdAlways["message "+poolNameOf(srv, testSlice2)+
		" 0 create_snap"] = "device-mapper: message ioctl failed: File exists"
	syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()})

	origin := raid0Name(srv, testTd)
	assertOrder(t, node,
		"cmd dmsetup suspend "+origin,
		snapMessage(srv, testSlice2),
		"cmd dmsetup resume "+origin,
	)
	if node.dms[origin].suspended {
		t.Fatalf("a failed create_snap left the origin raid0 suspended")
	}
	if node.dms[thinNameOf(srv, testTd, testSlice2)].suspended {
		t.Fatalf("a failed create_snap left the origin thin suspended")
	}
}

// TestSnapshotReapplySuspendsNothing is U1-T2 case 3 (SH16): every snap thin
// device is already present, so the pre-pass has nothing to do — no quiesce
// and no message. A suspend here would stall the origin's host IO on every
// converge round the worker drives.
func TestSnapshotReapplySuspendsNothing(t *testing.T) {
	srv, node := newTestServer(t)
	o := reqOpts{revision: 2, primary: true, twoSlices: true, tds: snapTds()}
	syncupBoth(t, srv, o)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(o)); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertNoCall(t, node, "cmd dmsetup suspend")
	assertNoCall(t, node, "create_snap")
	for _, call := range node.Mutations() {
		// Persisting the request is the one write SH5 mandates.
		if strings.HasPrefix(call, "writeproto ") {
			continue
		}
		t.Fatalf("an equal-revision re-apply mutated: %q", call)
	}
}

// TestSnapshotWithoutAnOriginStillMessages is U1-T2 case 4: ori_id names a
// dev_id no td of the plan carries — the origin was deleted from td_list.
// There is no dnv IO path to quiesce, so no raid0 is suspended, and the
// messages still go out exactly as they did before U1.
func TestSnapshotWithoutAnOriginStillMessages(t *testing.T) {
	srv, node := newTestServer(t)
	orphan := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, OriId: 7, Size: testTdSize},
	}
	reply := syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: orphan})

	assertNoCall(t, node, "cmd dmsetup suspend")
	assertOrder(t, node,
		"cmd dmsetup message "+poolNameOf(srv, testSlice)+
			" 0 create_snap 2 7",
		"cmd dmsetup message "+poolNameOf(srv, testSlice2)+
			" 0 create_snap 2 7",
	)
	assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice], "orphan snap thin slice 0")
}

// TestPlainThinNeverQuiescesARaid0 is U1-T2 case 5: an ordinary td is
// create_thin and quiesces nothing. The second converge is what makes it
// meaningful — by then the first td's raid0 is live, so an implementation
// that bracketed every td and not only snapshots would be caught here.
func TestPlainThinNeverQuiescesARaid0(t *testing.T) {
	srv, node := newTestServer(t)
	plain := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
	syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: plain})

	assertOrder(t, node,
		"cmd dmsetup message "+poolNameOf(srv, testSlice)+" 0 create_thin 2",
		"cmd dmsetup message "+poolNameOf(srv, testSlice2)+" 0 create_thin 2",
	)
	assertNoCall(t, node, "cmd dmsetup suspend")
	assertNoCall(t, node, "create_snap")
}

// TestFreshSnapshotMessagesAfterTheOriginsCreateThin pins the one thing the
// recorded-call fake cannot check for itself: dm-thin rejects a `create_snap`
// whose origin dev_id the pool does not hold. On a pass that creates the
// origin *and* its snapshot, the origin's `create_thin` is still ahead in the
// td loop, so the pre-pass must decline those slices and leave them to the
// lazy path — which is what keeps the two messages in the only order the
// kernel accepts. Nothing is quiesced: the origin's stack does not exist yet,
// so there is no live IO to tear.
func TestFreshSnapshotMessagesAfterTheOriginsCreateThin(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, twoSlices: true, tds: snapTds()})

	for _, sliceId := range []uint64{testSlice, testSlice2} {
		assertOrder(t, node,
			"cmd dmsetup message "+poolNameOf(srv, sliceId)+
				" 0 create_thin 1",
			snapMessage(srv, sliceId),
		)
	}
	assertNoCall(t, node, "cmd dmsetup suspend "+raid0Name(srv, testTd))
}

// TestSnapshotWithADeferredSliceStillMessagesTheReadyOne pins that the
// message loop is gated on poolReady and never on tdPlan.deferred, which is
// plan-global (anySliceDeferred, U4): with one slice still provisioning the
// origin has no raid0 to quiesce, but the ready slice's create_snap must
// still go out. Dropping it would lose the snapshot outright.
func TestSnapshotWithADeferredSliceStillMessagesTheReadyOne(t *testing.T) {
	srv, node := newTestServer(t)
	// unprovisionedDataLeg defers the *first* slice only; the second one
	// stays ready and its thin volumes are already live from revision 2.
	syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true,
		unprovisionedDataLeg: true, tds: snapTds()})

	if !node.hasCall(snapMessage(srv, testSlice2)) {
		t.Fatalf("the ready slice's create_snap was dropped\ncalls:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, snapMessage(srv, testSlice))
	assertNoCall(t, node, "cmd dmsetup suspend "+raid0Name(srv, testTd))
}

// TestSnapshotClaimsASliceEvenWhenItsMessageFailed pins the snapDone handoff
// (U1 step 5): the pre-pass claims a slice *before* sending, so a message
// that failed is not re-sent by ensureThin's lazy path later in the same
// pass. Re-sending would put that slice's create_snap after the raid0 resume
// — dating its snapshot from after host IO restarted, which is the very tear
// U1 exists to prevent. The one-shot failCmd is what makes the retry
// observable: failCmdAlways would fail the retry identically.
func TestSnapshotClaimsASliceEvenWhenItsMessageFailed(t *testing.T) {
	srv, node := newTestServer(t)
	node.failCmd["message "+poolNameOf(srv, testSlice2)+" 0 create_snap"] =
		"device-mapper: message ioctl failed: File exists"
	syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()})

	resume := node.indexOfCall("cmd dmsetup resume " + raid0Name(srv, testTd))
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		msgs := node.callsMatching(snapMessage(srv, sliceId))
		if len(msgs) != 1 {
			t.Fatalf("slice %#x: %d create_snap calls, want exactly 1",
				sliceId, len(msgs))
		}
		if at := node.indexOfCall(snapMessage(srv, sliceId)); at > resume {
			t.Fatalf("slice %#x: create_snap at %d is after the raid0 "+
				"resume at %d", sliceId, at, resume)
		}
	}
}
