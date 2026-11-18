package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A snapshot is point-in-time across slices (CN14's quiesce)
// ---------------------------------------------------------------------------
//
// A td is a dm-striped raid0 across one thin volume per slice
// (architecture.md, Primary cntlr), so a
// snapshot needs one `create_snap` per slice. Sent independently, slice 0's
// snapshot and slice 1's snapshot date from different instants and any host
// write landing between them is in one and not the other — a torn snapshot,
// worse than crash-consistent. The primary therefore suspends the origin td's
// raid0 around the whole per-slice message sequence (CN14).

// snapTds is the quiesce fixture's td_list: a live origin (dev_id 1) and the
// snapshot taken of it. ori_id is the *origin's dev_id*, never its td_id.
//
// The origin carries `created` because that is the only shape the gateway can
// produce: `CreateThinDevice` refuses a snapshot whose origin is not
// materialized in every slice pool (architecture.md, Thin devices), so a
// td_list holding a snapshot always holds a created origin — or no origin at
// all. It changes nothing in the tests below, whose origin is built by the
// preceding revision-2 converge and therefore never messaged again either
// way; it is what keeps the fixture honest.
func snapTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize, Created: true},
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
// origin's stack live, which is the state the CN14 quiesce exists for.
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
	if !acceptedCode(reply.GetAgentReply(), o.leftoverOk) {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// syncupSnapshotHeld is syncupSnapshot with the snapshot's id, dev_id 2,
// already in the pools of the given slices when the snapshot joins td_list:
// what a crashed or refused earlier run leaves. The ids go in after the
// origin's converge, whose activation sweep would delete them as strays
// (CN14).
func syncupSnapshotHeld(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	o reqOpts,
	heldIn ...uint64,
) *pb.SyncupCntlrReply {
	t.Helper()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	for _, sliceId := range heldIn {
		node.holdThinIds(poolNameOf(srv, sliceId), 2)
	}
	node.Reset()
	return syncupCntlrAt(t, srv, o)
}

// TestSnapshotQuiescesOriginRaid0 pins CN14's quiesce: the origin's raid0 is
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
	// after the resume — which no path can now produce, because ensureThin
	// never messages a td with ori_id != 0 (CN14).
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
	// One run made every slice's id: the snapshot is whole (CN14).
	assertTorn(t, info, testSnapTd, false, "a run that made every id")
}

// TestSnapshotResumesRaid0AfterAFailedMessage pins CN14's resume ([D12]): a
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
		revision: 3, primary: true, twoSlices: true, tds: snapTds(),
		leftoverOk: true})

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

// TestSnapshotReapplySuspendsNothing is SH16's re-apply: every snap thin
// device is already present, so the pre-pass has nothing to do — no quiesce
// and no message. A suspend here would stall the origin's host IO on every
// converge round the worker drives.
func TestSnapshotReapplySuspendsNothing(t *testing.T) {
	srv, node := newTestServer(t)
	// The origin is built by the revision-2 converge syncupSnapshot runs and
	// only then does the snapshot join td_list — the two-pass shape every
	// other test here uses, and the only one the gateway produces
	// (architecture.md, Thin devices).
	o := reqOpts{revision: 3, primary: true, twoSlices: true, tds: snapTds()}
	syncupSnapshot(t, srv, node, o)

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertNoCall(t, node, "cmd dmsetup suspend")
	assertNoCall(t, node, "create_snap")
	assertOnlyPersisted(t, node)
	// A run that makes no id tears nothing (CN14).
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
		"a re-apply that made no id")
}

// TestPlainThinNeverQuiescesARaid0 is CN14's case 2: an ordinary td is
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

// TestSnapshotMessagesWithoutTheOriginDevice is the fresh-primary shape of
// CN14's pre-pass. The origin's ids are in every slice pool — the control
// plane says so with `created` — but *this* cntlr has just been rebuilt, so
// no dm device of either td exists when the pre-pass runs. It must message
// anyway: declining would lose the snapshot outright, since no other path
// messages an uncreated snapshot (CN14).
//
// Nothing is quiesced, because nothing is live yet, and no `create_thin` is
// sent for the created origin (CN14) — the bare `dmsetup create` re-attaches
// the id the CN21 teardown left in the pool metadata.
func TestSnapshotMessagesWithoutTheOriginDevice(t *testing.T) {
	// CN14, order independence: td_list order carries no meaning.
	// Both orders must
	// record the same messages, the same absence of suspends and the same
	// device creations. Not a literal call-for-call multiset: the fake hands
	// out minor numbers in creation order, so the two runs' raid0 tables
	// cite different ones.
	reversed := []*pb.ThinDevice{snapTds()[1], snapTds()[0]}
	for _, tc := range []struct {
		name string
		tds  []*pb.ThinDevice
	}{
		{"origin first", snapTds()},
		{"snapshot first", reversed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			// Revision 2 builds the origin alone; its create_thin is what
			// puts dev_id 1 in both pools.
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, twoSlices: true})
			// CN21: the teardown removes every device and sends no `delete`,
			// so the pool metadata on the DN legs keeps dev_id 1.
			cnSweepAssertCode(t, cnSweepSyncupSettled(t, srv, 3, false).
				GetAgentReply(), 0, "the teardown")
			cnSyncup(t, srv, 4, true)

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(
				reqOpts{revision: 4, primary: true, twoSlices: true,
					tds: tc.tds}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}

			assertNoCall(t, node, "cmd dmsetup suspend")
			assertNoCall(t, node, "0 create_thin ")
			for _, sliceId := range []uint64{testSlice, testSlice2} {
				if n := len(node.callsMatching(
					snapMessage(srv, sliceId))); n != 1 {
					t.Fatalf("slice %#x: %d create_snap calls, want exactly 1",
						sliceId, n)
				}
				for _, tdId := range []uint64{testTd, testSnapTd} {
					assertOrder(t, node, "cmd dmsetup create "+
						thinNameOf(srv, tdId, sliceId))
				}
			}
			info := reply.GetCntlrInfo()
			for _, tdId := range []uint64{testTd, testSnapTd} {
				for _, sliceId := range []uint64{testSlice, testSlice2} {
					assertOk(t, info.GetTdIdToThinInfo()[tdId].
						GetSliceIdToDmThin()[sliceId],
						fmt.Sprintf("thin td %#x slice %#x", tdId, sliceId))
				}
			}
		})
	}
}

// TestCreatedTdIsNeverMessaged is the plain-td half of CN14's case 1.
// `created` is the control plane's word that dev_id 1 is in every slice
// pool, so a fresh primary attaches it with a bare `dmsetup create` and sends
// nothing. The CN9 order test still sees create_thin because its
// tds are `created = false`.
func TestCreatedTdIsNeverMessaged(t *testing.T) {
	srv, node := newTestServer(t)
	node.holdThinIds(poolName(srv), 1)
	o := reqOpts{revision: 2, primary: true, tds: []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize, Created: true}}}
	reply := syncupBoth(t, srv, o)

	assertNoCall(t, node, "0 create_thin ")
	assertOrder(t, node, "cmd dmsetup create "+thinName(srv, testTd))
	assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice], "created thin")

	// SH16: an equal-revision re-apply of a fully built cntlr mutates nothing.
	node.Reset()
	if _, err := srv.SyncupCntlr(
		context.Background(), cntlrReq(o)); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertOnlyPersisted(t, node)
}

// TestCreatedSnapshotIsNeverMessaged is the same rule for a snapshot (CN14).
// Both ids are in the pool, so the pre-pass has nothing to claim — no
// `create_snap`, and therefore no quiesce of an origin that is not even
// carrying IO yet. The second case drops the origin from td_list entirely:
// the gateway (architecture.md, Thin devices, DeleteThinDevice) lets it be
// deleted once every snapshot of it is created, and that
// changes nothing here, because there is nothing to message and nothing to
// quiesce either way.
func TestCreatedSnapshotIsNeverMessaged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tds    []*pb.ThinDevice
		subsys map[string]*pb.Subsystem
		want   []uint64
	}{
		{
			name: "origin in td_list",
			tds: []*pb.ThinDevice{
				{TdId: testTd, DevId: 1, Size: testTdSize, Created: true},
				{TdId: testSnapTd, DevId: 2, OriId: 1, Size: testTdSize,
					Created: true},
			},
			want: []uint64{testTd, testSnapTd},
		},
		{
			name: "origin deleted",
			tds: []*pb.ThinDevice{
				{TdId: testSnapTd, DevId: 2, OriId: 1, Size: testTdSize,
					Created: true},
			},
			subsys: subsysForTd(testSnapTd),
			want:   []uint64{testSnapTd},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			node.holdThinIds(poolNameOf(srv, testSlice), 1, 2)
			node.holdThinIds(poolNameOf(srv, testSlice2), 1, 2)
			reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				twoSlices: true, tds: tc.tds, subsys: tc.subsys})

			assertNoCall(t, node, "0 create_snap ")
			assertNoCall(t, node, "0 create_thin ")
			assertNoCall(t, node, "cmd dmsetup suspend")
			info := reply.GetCntlrInfo()
			for _, tdId := range tc.want {
				for _, sliceId := range []uint64{testSlice, testSlice2} {
					assertOrder(t, node, "cmd dmsetup create "+
						thinNameOf(srv, tdId, sliceId))
					assertOk(t, info.GetTdIdToThinInfo()[tdId].
						GetSliceIdToDmThin()[sliceId],
						fmt.Sprintf("thin td %#x slice %#x", tdId, sliceId))
				}
			}
		})
	}
}

// TestCreatedTdWithAMissingIdIsAnError is the price CN14's "A created td is
// never messaged on any pass" deliberately pays. A created td whose id a pool
// no longer holds cannot be repaired by a
// message — a `create_thin` would hand the live dev_id a fresh, empty volume
// — so the failing `dmsetup create` is reported as it happened and left
// there. No converge, at any revision, self-heals it.
//
// The row names the cause in its reason, RES_REASON_THIN_ID_LOST, read from
// the refusal's errno (CN29), so the worker's class for a lost id reads a
// field and not the dmsetup words (dnv-worker.md HL2). Only a converge names
// it: a check round reads the absent volume MISSING with no reason. Once an
// operator restores the id, the row reads OK and the reason is gone.
func TestCreatedTdWithAMissingIdIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	o := reqOpts{revision: 2, primary: true, tds: []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize, Created: true}}}
	reply := syncupBoth(t, srv, o)

	assertNoCall(t, node, "0 create_thin ")
	row := reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice]
	assertErrorDetails(t, row, "No data available", "lost thin id")
	assertReason(t, row, pb.ResReason_RES_REASON_THIN_ID_LOST, "lost thin id")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertNoCall(t, node, "0 create_thin ")
	row = reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice]
	assertErrorDetails(t, row, "No data available",
		"lost thin id, second converge")
	assertReason(t, row, pb.ResReason_RES_REASON_THIN_ID_LOST,
		"lost thin id, second converge")

	probed := checkRound(t, srv, 2).GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice]
	assertMissing(t, probed, "lost thin id, check round")
	assertReason(t, probed, pb.ResReason_RES_REASON_NONE,
		"lost thin id, check round")

	node.holdThinIds(poolName(srv), 1)
	reply, err = srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	row = reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice]
	assertOk(t, row, "restored thin id")
	assertReason(t, row, pb.ResReason_RES_REASON_NONE, "restored thin id")
}

// TestCreatedTdOtherErrorHasNoReason bounds the reason to the errno that
// names it (CN29): a created td whose thin table the kernel refuses for any
// other cause reads ERROR with the dmsetup output in its details and no
// reason, so the worker treats it as this cntlr's own error.
func TestCreatedTdOtherErrorHasNoReason(t *testing.T) {
	srv, node := newTestServer(t)
	node.holdThinIds(poolName(srv), 1)
	node.failCmdAlways["dmsetup create "+thinName(srv, testTd)] =
		"device-mapper: reload ioctl on " + thinName(srv, testTd) +
			"  failed: Invalid argument"
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		tds: []*pb.ThinDevice{
			{TdId: testTd, DevId: 1, Size: testTdSize, Created: true}},
		leftoverOk: true})

	row := reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice]
	assertErrorDetails(t, row, "Invalid argument", "refused thin table")
	assertReason(t, row, pb.ResReason_RES_REASON_NONE, "refused thin table")
}

// TestUncreatedSnapshotRetriesWhenTheOriginIdIsMissing is CN14's "A violated
// precondition is left to dm-thin": the origin guarantee is the gateway's to
// keep and the agent does not re-check
// it. A `create_snap` whose origin the pool lacks fails at the message, the
// `dmsetup create` behind it fails too, and the row says so — with the td
// still `created = false`, which is exactly what makes the next converge try
// again.
func TestUncreatedSnapshotRetriesWhenTheOriginIdIsMissing(t *testing.T) {
	srv, node := newTestServer(t)
	o := reqOpts{revision: 2, primary: true, subsys: subsysForTd(testSnapTd),
		tds: []*pb.ThinDevice{
			{TdId: testSnapTd, DevId: 2, OriId: 7, Size: testTdSize}},
		leftoverOk: true}
	message := "cmd dmsetup message " + poolName(srv) + " 0 create_snap 2 7"
	reply := syncupBoth(t, srv, o)

	assertNoCall(t, node, "cmd dmsetup suspend")
	if n := len(node.callsMatching(message)); n != 1 {
		t.Fatalf("%d create_snap calls, want exactly 1", n)
	}
	row := reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice]
	assertErrorDetails(t, row, "No data available", "orphan snap")
	// An uncreated td's id has not been made yet: no lost id (CN14).
	assertReason(t, row, pb.ResReason_RES_REASON_NONE, "orphan snap")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if n := len(node.callsMatching(message)); n != 1 {
		t.Fatalf("the re-apply sent %d create_snap calls, want exactly 1", n)
	}
	row = reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice]
	assertErrorDetails(t, row, "No data available",
		"orphan snap, second converge")
	assertReason(t, row, pb.ResReason_RES_REASON_NONE,
		"orphan snap, second converge")
}

// TestSnapshotWithADeferredSliceStillMessagesTheReadyOne pins that the
// message loop is gated on poolReady and never on tdPlan.deferred, which is
// plan-global (anySliceDeferred, [D15]): with one slice still provisioning the
// ready slice's create_snap must still go out. Dropping it would lose the
// snapshot outright.
//
// CN14's pre-pass has no `origin.deferred` early return, and this fixture is
// where that shows: the origin's raid0 was built by the revision-2 converge
// and is still live, so it is quiesced around the one message it is possible
// to send. Quiescing the live raid0 is the CN14 quiesce applied honestly, so
// the assertion is the bracket, not its absence.
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
	origin := raid0Name(srv, testTd)
	assertOrder(t, node,
		"cmd dmsetup suspend "+origin,
		snapMessage(srv, testSlice2),
		"cmd dmsetup resume "+origin,
	)
	if node.dms[origin].suspended {
		t.Fatalf("a deferred slice left the origin raid0 suspended")
	}
}

// TestSnapshotClaimsASliceEvenWhenItsMessageFailed pins "exactly one
// create_snap per slice, none after the raid0 resume". It holds by
// construction, because ensureThin
// never messages a td with ori_id != 0 (CN14) and the pre-pass is the only
// caller of createSnapId. A re-send would put that slice's create_snap after
// the resume — dating its snapshot from after host IO restarted, which is the
// very tear the quiesce exists to prevent. The one-shot failCmd is what makes such a
// retry observable: failCmdAlways would fail it identically.
func TestSnapshotClaimsASliceEvenWhenItsMessageFailed(t *testing.T) {
	srv, node := newTestServer(t)
	node.failCmd["message "+poolNameOf(srv, testSlice2)+" 0 create_snap"] =
		"device-mapper: message ioctl failed: File exists"
	syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds(),
		leftoverOk: true})

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

// assertReason checks the reason a row names (architecture.md, Live-state
// reporting; CN29).
func assertReason(
	t *testing.T,
	info *pb.ResInfo,
	want pb.ResReason,
	label string,
) {
	t.Helper()
	if got := info.GetReason(); got != want {
		t.Fatalf("%s: reason %v, want %v (status %v, details %q)",
			label, got, want, info.GetStatus(), info.GetDetails())
	}
}

// assertTorn checks the torn mark one td's thin info carries (CN14).
func assertTorn(
	t *testing.T,
	info *pb.CntlrInfo,
	tdId uint64,
	want bool,
	label string,
) {
	t.Helper()
	thinInfo := info.GetTdIdToThinInfo()[tdId]
	if thinInfo == nil {
		t.Fatalf("%s: no thin info for td %#x", label, tdId)
	}
	if got := thinInfo.GetTorn(); got != want {
		t.Fatalf("%s: td %#x torn = %v, want %v", label, tdId, got, want)
	}
}

// tornMarks is the fixture cntlr's in-memory torn marks (CN14).
func tornMarks(t *testing.T, srv *CnAgentServer) map[uint64]bool {
	t.Helper()
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		t.Fatalf("the fixture cntlr has no state")
	}
	return st.tornTds
}

// TestSnapshotTornWhenASliceAlreadyHoldsItsId is CN14's torn mark from a
// message: slice 0's create_snap makes the snapshot's id while slice 1's pool
// already holds it — its message is refused with EEXIST — so the two slices'
// snapshots date from different instants. Both rows read OK, as the volumes
// are there; the torn mark says what the rows cannot, on the converge and on
// the check round alike.
func TestSnapshotTornWhenASliceAlreadyHoldsItsId(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupSnapshotHeld(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()},
		testSlice2)

	info := reply.GetCntlrInfo()
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		assertOk(t, info.GetTdIdToThinInfo()[testSnapTd].
			GetSliceIdToDmThin()[sliceId],
			fmt.Sprintf("snap thin slice %#x", sliceId))
	}
	assertTorn(t, info, testSnapTd, true, "converge")
	assertTorn(t, info, testTd, false, "the origin, converge")
	probed := checkRound(t, srv, 3)
	assertTorn(t, probed, testSnapTd, true, "check round")
	assertTorn(t, probed, testTd, false, "the origin, check round")
}

// TestSnapshotTornWhenASliceHasItsDevice is CN14's torn mark from a device: a
// run whose slice-0 message is refused for another cause counts that slice
// as neither made nor held, so it tears nothing; the next run finds slice 1's
// snapshot device there — its id from the earlier run — and makes slice 0's,
// which tears the snapshot.
func TestSnapshotTornWhenASliceHasItsDevice(t *testing.T) {
	srv, node := newTestServer(t)
	node.failCmd["message "+poolNameOf(srv, testSlice)+" 0 create_snap"] =
		msgIoctlFailed(poolNameOf(srv, testSlice), "Invalid argument")
	o := reqOpts{revision: 3, primary: true, twoSlices: true, tds: snapTds(),
		leftoverOk: true}
	reply := syncupSnapshot(t, srv, node, o)
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
		"a run that made one id and failed the other")
	assertErrorDetails(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice], "No data available",
		"the slice whose message failed")

	node.Reset()
	reply = syncupCntlrAt(t, srv, o)
	if n := len(node.callsMatching(snapMessage(srv, testSlice))); n != 1 {
		t.Fatalf("the re-apply sent %d create_snap calls to slice 0, want 1",
			n)
	}
	assertNoCall(t, node, snapMessage(srv, testSlice2))
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true,
		"a run that made slice 0's id beside slice 1's device")
}

// TestSnapshotTornBesideAPoolNotReady is CN14's torn mark from a device
// whose pool is not ready: a slice's snapshot device holds its id whatever
// its pool's state this pass. The first run makes slice 0's id while slice
// 1's pool is not ready, and the pass builds slice 0's device; the second
// makes slice 1's id while slice 0's pool is not ready, beside that device,
// which tears the snapshot. Both channels report the mark, and so does the
// pass that then finds every device there.
func TestSnapshotTornBesideAPoolNotReady(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	o := reqOpts{revision: 3, primary: true, twoSlices: true, tds: snapTds(),
		leftoverOk: true}
	// notReadyRun runs one pass with the pool of slice off not ready, and
	// checks the pass messaged only slice on.
	notReadyRun := func(off, on uint64) *pb.CntlrInfo {
		t.Helper()
		node.Reset()
		// Every status read of the pass fails: the converge reads the
		// pool's status before its row's (ensurePool, CN13).
		key := "dmsetup status --noflush " + poolNameOf(srv, off)
		node.failCmdAlways[key] = "device-mapper: status ioctl on " +
			poolNameOf(srv, off) + "  failed: Input/output error"
		info := syncupCntlrAt(t, srv, o).GetCntlrInfo()
		delete(node.failCmdAlways, key)
		assertMissingDetails(t, info.GetTdIdToThinInfo()[testSnapTd].
			GetSliceIdToDmThin()[off], "thin pool missing",
			fmt.Sprintf("snap thin slice %#x, its pool not ready", off))
		assertNoCall(t, node, snapMessage(srv, off))
		if n := len(node.callsMatching(snapMessage(srv, on))); n != 1 {
			t.Fatalf("slice %#x: %d create_snap calls, want 1", on, n)
		}
		return info
	}

	assertTorn(t, notReadyRun(testSlice2, testSlice), testSnapTd, false,
		"a run that made slice 0's id while slice 1's pool was not ready")
	if _, ok := node.dms[thinNameOf(srv, testSnapTd, testSlice)]; !ok {
		t.Fatal("fixture: the first run built no snapshot device in slice 0")
	}
	assertTorn(t, notReadyRun(testSlice, testSlice2), testSnapTd, true,
		"a run that made slice 1's id beside slice 0's device")
	assertTorn(t, checkRound(t, srv, 3), testSnapTd, true, "check round")

	node.Reset()
	reply := syncupCntlrAt(t, srv, o)
	assertNoCall(t, node, "create_snap")
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true,
		"the pass that finds every device there")
}

// TestSnapshotNotTornBesideAnUnreadSlice bounds the mark (CN14): a slice
// whose snapshot device the run cannot read counts as neither made nor held,
// so a run that makes the other slice's id beside it tears nothing.
func TestSnapshotNotTornBesideAnUnreadSlice(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	node.Reset()
	node.killCmdNoEffectAlways["dmsetup info --columns --noheadings -o attr "+
		thinNameOf(srv, testSnapTd, testSlice2)] = true
	reply := syncupCntlrAt(t, srv, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds(),
		leftoverOk: true})

	if n := len(node.callsMatching(snapMessage(srv, testSlice))); n != 1 {
		t.Fatalf("slice 0: %d create_snap calls, want 1", n)
	}
	assertNoCall(t, node, snapMessage(srv, testSlice2))
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
		"a run that made one id beside a slice it could not read")
}

// TestSnapshotNotTornWhenNoIdIsMade bounds the mark (CN14): a run in which
// every slice already holds the id — each message refused with EEXIST — made
// nothing, so it tore nothing, and the volumes attach the ids that are there.
func TestSnapshotNotTornWhenNoIdIsMade(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupSnapshotHeld(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()},
		testSlice, testSlice2)

	for _, sliceId := range []uint64{testSlice, testSlice2} {
		if n := len(node.callsMatching(snapMessage(srv, sliceId))); n != 1 {
			t.Fatalf("slice %#x: %d create_snap calls, want 1", sliceId, n)
		}
		assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
			GetSliceIdToDmThin()[sliceId],
			fmt.Sprintf("snap thin slice %#x", sliceId))
	}
	if !node.dms[poolNameOf(srv, testSlice)].thinIds[2] ||
		node.dms[poolNameOf(srv, testSlice)].snapshot {
		t.Fatalf("fixture: slice 0's message made the id after all")
	}
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
		"a run whose every message was refused with EEXIST")
}

// TestTornMarkLastsUntilTheRequestShowsIt pins the mark's life (CN14): a run
// sets it, a later run that makes no id — every snapshot device is there, so
// it sends no message and suspends nothing — leaves it set, and both
// channels report it until the request lists the td torn, which is when the
// worker has recorded it (dnv-worker.md RW19), or drops the td; a request
// that lists the td created and not torn keeps it, and its file goes with
// the last mark.
func TestTornMarkLastsUntilTheRequestShowsIt(t *testing.T) {
	tornSnapshot := func(t *testing.T) (*CnAgentServer, *fakeNode) {
		t.Helper()
		srv, node := newTestServer(t)
		reply := syncupSnapshotHeld(t, srv, node, reqOpts{
			revision: 3, primary: true, twoSlices: true, tds: snapTds()},
			testSlice2)
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true, "the torn run")
		return srv, node
	}

	t.Run("a re-apply keeps it", func(t *testing.T) {
		srv, node := tornSnapshot(t)
		node.Reset()
		reply := syncupCntlrAt(t, srv, reqOpts{
			revision: 3, primary: true, twoSlices: true, tds: snapTds()})
		assertNoCall(t, node, "create_snap")
		assertNoCall(t, node, "cmd dmsetup suspend")
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true, "re-apply")
		assertTorn(t, checkRound(t, srv, 3), testSnapTd, true,
			"check round after the re-apply")
	})

	t.Run("a request that lists it torn clears it", func(t *testing.T) {
		srv, node := tornSnapshot(t)
		tds := snapTds()
		tds[1].Created = true
		tds[1].Torn = true
		reply := syncupCntlrAt(t, srv, reqOpts{
			revision: 4, primary: true, twoSlices: true, tds: tds})
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
			"converge of the td the request lists torn")
		assertTorn(t, checkRound(t, srv, 4), testSnapTd, false,
			"check round of the td the request lists torn")
		if marks := tornMarks(t, srv); len(marks) != 0 {
			t.Fatalf("torn marks %v outlive the request's mark", marks)
		}
		if storeFileExists(node, tornPathOf(srv)) {
			t.Fatalf("the torn marks' file outlives the last mark")
		}
	})

	t.Run("a created td keeps it", func(t *testing.T) {
		srv, node := tornSnapshot(t)
		tds := snapTds()
		tds[1].Created = true
		reply := syncupCntlrAt(t, srv, reqOpts{
			revision: 4, primary: true, twoSlices: true, tds: tds})
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true,
			"converge of a td the request lists created, not torn")
		assertTorn(t, checkRound(t, srv, 4), testSnapTd, true,
			"check round of a td the request lists created, not torn")
		if !storeFileExists(node, tornPathOf(srv)) {
			t.Fatalf("the torn marks' file went with the created flip")
		}
	})

	t.Run("a check round reads torn before a converge drops it",
		func(t *testing.T) {
			srv, _ := tornSnapshot(t)
			tds := snapTds()
			tds[1].Created = true
			tds[1].Torn = true
			st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
				testCntlr))
			st.storeReq(cntlrReq(reqOpts{
				revision: 3, primary: true, twoSlices: true, tds: tds}))
			assertTorn(t, checkRound(t, srv, 3), testSnapTd, false,
				"check round of a td the request lists torn")
		})

	t.Run("dropping the td clears it", func(t *testing.T) {
		srv, node := tornSnapshot(t)
		syncupCntlrAt(t, srv, reqOpts{
			revision: 4, primary: true, twoSlices: true,
			tds: snapTds()[:1]})
		if marks := tornMarks(t, srv); len(marks) != 0 {
			t.Fatalf("torn marks %v outlive the td", marks)
		}
		if storeFileExists(node, tornPathOf(srv)) {
			t.Fatalf("the torn marks' file outlives the td")
		}
	})
}

// tornPathOf is the fixture cntlr's torn marks' file (CN14).
func tornPathOf(srv *CnAgentServer) string {
	return srv.nf.LocalTornPath(testCluster, testCn, testSp, testCntlr)
}

// storedTornMarks decodes the fixture cntlr's torn marks' file.
func storedTornMarks(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
) *pb.TornMarks {
	t.Helper()
	node.mu.Lock()
	raw, ok := node.protos[tornPathOf(srv)]
	node.mu.Unlock()
	if !ok {
		t.Fatalf("no torn marks' file at %s", tornPathOf(srv))
	}
	marks := &pb.TornMarks{}
	if err := proto.Unmarshal(raw, marks); err != nil {
		t.Fatalf("the torn marks' file does not decode: %v", err)
	}
	return marks
}

// TestTornMarkSurvivesARestart pins the saved mark (CN14, CN2): the run
// that tears the snapshot saves the mark in the local store before the thin
// loop, naming its cntlr, and the restarted agent, whose runs make no id —
// every snapshot device is there — loads it and reports it again.
func TestTornMarkSurvivesARestart(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupSnapshotHeld(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()},
		testSlice2)
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true, "the torn run")
	assertOrder(t, node, "writeproto "+tornPathOf(srv),
		"cmd dmsetup create "+thinNameOf(srv, testSnapTd, testSlice))
	marks := storedTornMarks(t, srv, node)
	if marks.GetClusterId() != testCluster || marks.GetCnId() != testCn ||
		marks.GetCntlrPointer().GetSpId() != testSp ||
		marks.GetCntlrPointer().GetCntlrId() != testCntlr ||
		len(marks.GetTdIdList()) != 1 ||
		marks.GetTdIdList()[0] != testSnapTd {
		t.Fatalf("saved torn marks = %v, want the fixture cntlr's td %#x",
			marks, testSnapTd)
	}

	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	assertTorn(t, checkRound(t, fresh, 3), testSnapTd, true,
		"check round after a restart")
	node.Reset()
	reply = syncupCntlrAt(t, fresh, reqOpts{
		revision: 4, primary: true, twoSlices: true, tds: snapTds()})
	assertNoCall(t, node, "create_snap")
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true,
		"the first converge after a restart")
}

// TestATornMarkWhoseSaveFailsStays pins a failed save of the mark (CN14):
// the converge logs it, the mark stays in memory and both channels report
// it, and the node's store row reads the disk's refusal (CN28).
func TestATornMarkWhoseSaveFailsStays(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	failSaves(srv, node, tornPathOf(srv))
	node.holdThinIds(poolNameOf(srv, testSlice2), 2)
	node.Reset()
	reply := syncupCntlrAt(t, srv, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()})
	if n := len(node.callsMatching(failedWrite(tornPathOf(srv)))); n != 1 {
		t.Fatalf("%d failed saves of the torn marks, want 1", n)
	}
	assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true, "the torn run")
	assertTorn(t, checkRound(t, srv, 3), testSnapTd, true,
		"check round after the failed save")
	if storeFileExists(node, tornPathOf(srv)) {
		t.Fatalf("fixture: the failed save wrote the file")
	}
	if _, refused := srv.store.Refusal(); !refused {
		t.Fatalf("the store does not read the refused save")
	}
}

// TestATornMarkIsReportedInAnyRole pins CN14's report in any role: a primary
// that tore a snapshot and is demoted, or whose agent restarts as a standby
// after the new primary's created flip — the td listed created and not torn
// — reports the mark in an entry with no rows, on both channels, so the
// worker reads a tear only the old primary saw (dnv-worker.md RW19).
func TestATornMarkIsReportedInAnyRole(t *testing.T) {
	tdsAt := func(created bool) []*pb.ThinDevice {
		tds := snapTds()
		tds[1].Created = created
		return tds
	}
	for _, tc := range []struct {
		name    string
		restart bool
		created bool
	}{
		{"demoted", false, false},
		{"back as a standby after the created flip", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupSnapshotHeld(t, srv, node, reqOpts{
				revision: 3, primary: true, twoSlices: true,
				tds: snapTds()}, testSlice2)
			if tc.restart {
				srv = newCnServer(node)
				reconcileForTest(t, srv)
			}
			reply := syncupCntlrAt(t, srv, reqOpts{
				revision: 4, twoSlices: true, tds: tdsAt(tc.created),
				leftoverOk: true})
			for label, info := range map[string]*pb.CntlrInfo{
				"converge":    reply.GetCntlrInfo(),
				"check round": checkRound(t, srv, 4),
			} {
				assertTorn(t, info, testSnapTd, true, label)
				if rows := info.GetTdIdToThinInfo()[testSnapTd].
					GetSliceIdToDmThin(); len(rows) != 0 {
					t.Fatalf("%s: a standby's entry carries rows %v",
						label, rows)
				}
				if _, ok := info.GetTdIdToThinInfo()[testTd]; ok {
					t.Fatalf("%s: a standby reports the unmarked origin",
						label)
				}
			}
		})
	}
}

// TestSnapshotTornWhenTheOriginCannotBeQuiesced pins CN14's failed quiesce
// (suspendSnapOrigin): when the origin's raid0 may be live and the run cannot
// suspend it — its info read does not answer, or its suspend fails — the
// messages still go, and a run that makes ids in two slices marks the
// snapshot torn; one that makes a single id does not.
func TestSnapshotTornWhenTheOriginCannotBeQuiesced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     func(srv *CnAgentServer, node *fakeNode)
		heldIn   []uint64
		wantTorn bool
	}{
		{"the suspend fails", func(srv *CnAgentServer, node *fakeNode) {
			node.failCmdAlways["dmsetup suspend "+raid0Name(srv, testTd)] =
				"device-mapper: suspend ioctl failed: Invalid argument"
		}, nil, true},
		{"the info read does not answer", func(srv *CnAgentServer,
			node *fakeNode) {
			node.killCmdNoEffectAlways["dmsetup info --columns --noheadings "+
				"-o attr "+raid0Name(srv, testTd)] = true
		}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				twoSlices: true})
			for _, sliceId := range tc.heldIn {
				node.holdThinIds(poolNameOf(srv, sliceId), 2)
			}
			tc.fail(srv, node)
			node.Reset()
			reply := syncupCntlrAt(t, srv, reqOpts{
				revision: 3, primary: true, twoSlices: true, tds: snapTds(),
				leftoverOk: true})
			assertNoCall(t, node, "cmd dmsetup resume "+raid0Name(srv, testTd))
			for _, sliceId := range []uint64{testSlice, testSlice2} {
				if n := len(node.callsMatching(
					snapMessage(srv, sliceId))); n != 1 {
					t.Fatalf("slice %#x: %d create_snap calls, want 1",
						sliceId, n)
				}
			}
			assertTorn(t, reply.GetCntlrInfo(), testSnapTd, tc.wantTorn,
				"the unquiesced run")
		})
	}

	t.Run("a single slice made unquiesced", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			twoSlices: true})
		node.failCmdAlways["dmsetup suspend "+raid0Name(srv, testTd)] =
			"device-mapper: suspend ioctl failed: Invalid argument"
		node.failCmdAlways["message "+poolNameOf(srv, testSlice2)+
			" 0 create_snap"] = msgIoctlFailed(poolNameOf(srv, testSlice2),
			"Invalid argument")
		node.Reset()
		reply := syncupCntlrAt(t, srv, reqOpts{
			revision: 3, primary: true, twoSlices: true, tds: snapTds(),
			leftoverOk: true})
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
			"an unquiesced run that made one id alone")
	})
}

// TestAKilledSnapMessageCountsAsMade pins CN14's count of a create_snap the
// soft timeout killed: the kernel may have made the id, and the thin loop of
// the same pass builds the device over it, after which no run messages the
// slice again, so the run counts the slice as made — beside a slice already
// held by an earlier run, and in an unquiesced run beside one more made id.
// A slice whose message never went — its origin thin volume's suspend was
// killed — counts as nothing.
func TestAKilledSnapMessageCountsAsMade(t *testing.T) {
	failRaid0Suspend := func(srv *CnAgentServer, node *fakeNode) {
		node.failCmdAlways["dmsetup suspend "+raid0Name(srv, testTd)] =
			"device-mapper: suspend ioctl failed: Invalid argument"
	}
	o := reqOpts{revision: 3, primary: true, twoSlices: true, tds: snapTds(),
		leftoverOk: true}
	key := func(srv *CnAgentServer) string {
		return snapMessage(srv, testSlice2)[len("cmd dmsetup "):]
	}

	t.Run("an unquiesced run", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			twoSlices: true})
		failRaid0Suspend(srv, node)
		node.killCmd[key(srv)] = true
		node.Reset()
		reply := syncupCntlrAt(t, srv, o)
		if _, ok := node.dms[thinNameOf(srv, testSnapTd, testSlice2)]; !ok {
			t.Fatal("fixture: the pass built no device over the id the " +
				"killed message made")
		}
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, true,
			"an unquiesced run that made one id and killed the other's "+
				"message")
		if !tornMarks(t, srv)[testSnapTd] {
			t.Fatal("the run kept no mark")
		}
	})

	t.Run("a later run", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			twoSlices: true})
		node.failCmd[key(srv)] = msgIoctlFailed(poolNameOf(srv, testSlice2),
			"Invalid argument")
		node.Reset()
		assertTorn(t, syncupCntlrAt(t, srv, o).GetCntlrInfo(), testSnapTd,
			false, "a run that made one id and failed the other")
		node.killCmd[key(srv)] = true
		assertTorn(t, syncupCntlrAt(t, srv, o).GetCntlrInfo(), testSnapTd,
			true, "a run whose killed message joined slice 0's device")
	})

	t.Run("a message never sent", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			twoSlices: true})
		failRaid0Suspend(srv, node)
		node.killCmdNoEffect["dmsetup suspend "+
			thinNameOf(srv, testTd, testSlice2)] = true
		node.Reset()
		reply := syncupCntlrAt(t, srv, o)
		assertNoCall(t, node, snapMessage(srv, testSlice2))
		assertTorn(t, reply.GetCntlrInfo(), testSnapTd, false,
			"an unquiesced run that made one id and sent no other message")
	})
}

// TestSnapshotResumesTheOriginThinOnACancelledCall pins the per-slice
// resume's ctx (CN14, createSnapId): a call cancelled while a slice's
// create_snap runs still resumes that slice's origin thin volume, as the
// raid0's resume does, so no origin is left suspended.
func TestSnapshotResumesTheOriginThinOnACancelledCall(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	node.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.cancelCmd[snapMessage(srv, testSlice)] = cancel
	_, _ = srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()}))
	ori := thinNameOf(srv, testTd, testSlice)
	assertOrder(t, node, "cmd dmsetup suspend "+ori,
		snapMessage(srv, testSlice), "cmd dmsetup resume "+ori)
	if node.dms[ori].suspended {
		t.Fatalf("a cancelled call left the origin thin volume suspended")
	}
	if node.dms[raid0Name(srv, testTd)].suspended {
		t.Fatalf("a cancelled call left the origin raid0 suspended")
	}
}

// TestTornMarksFileGoesWithItsCntlr pins the life of the torn marks' file
// (CN7, CN2): the drop of a cntlr whose pointer leaves its CN's list removes
// it with the cntlr's other files, and a startup reconcile removes one whose
// cntlr it does not load, as an orphan, while a file of a loaded cntlr is
// kept.
func TestTornMarksFileGoesWithItsCntlr(t *testing.T) {
	t.Run("the cntlr's drop", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupSnapshotHeld(t, srv, node, reqOpts{
			revision: 3, primary: true, twoSlices: true, tds: snapTds()},
			testSlice2)
		if !storeFileExists(node, tornPathOf(srv)) {
			t.Fatal("fixture: no torn marks' file")
		}
		if _, err := srv.SyncupCn(context.Background(),
			cnReq(2, false)); err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		if storeFileExists(node, tornPathOf(srv)) {
			t.Fatal("the torn marks' file outlives its cntlr's drop")
		}
	})

	t.Run("an orphan at startup", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupSnapshotHeld(t, srv, node, reqOpts{
			revision: 3, primary: true, twoSlices: true, tds: snapTds()},
			testSlice2)
		orphan := srv.nf.LocalTornPath(testCluster, testCn, testSp,
			testCntlr+0x40)
		raw, err := proto.Marshal(&pb.TornMarks{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: &pb.CntlrPointer{SpId: testSp,
				CntlrId: testCntlr + 0x40},
			TdIdList: []uint64{testSnapTd},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		node.mu.Lock()
		node.protos[orphan] = raw
		node.mu.Unlock()

		fresh := newCnServer(node)
		reconcileForTest(t, fresh)
		if storeFileExists(node, orphan) {
			t.Fatal("a torn marks' file of no loaded cntlr outlives the " +
				"startup reconcile")
		}
		if !storeFileExists(node, tornPathOf(srv)) {
			t.Fatal("the startup reconcile removed a loaded cntlr's file")
		}
	})
}
