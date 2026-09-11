package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A snapshot is point-in-time across slices (CN14's quiesce)
// ---------------------------------------------------------------------------
//
// A td is a dm-striped raid0 across one thin volume per slice (§3.3), so a
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
// materialized in every slice pool (ThinDeviceCreated.md U2-S1), so a
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
	// after the resume — which no path can now produce, because ensureThin
	// never messages a td with ori_id != 0 (U4-S1).
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
	// The origin is built by the revision-2 converge syncupSnapshot runs and
	// only then does the snapshot join td_list — the two-pass shape every
	// other test here uses, and the only one the gateway produces (U2-S1).
	o := reqOpts{revision: 3, primary: true, twoSlices: true, tds: snapTds()}
	syncupSnapshot(t, srv, node, o)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(o)); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertNoCall(t, node, "cmd dmsetup suspend")
	assertNoCall(t, node, "create_snap")
	assertOnlyPersisted(t, node)
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

// TestSnapshotMessagesWithoutTheOriginDevice is U4-T1, the fresh-primary
// shape and the reason the pre-pass's origin-device filter could go. The
// origin's ids are in every slice pool — the control plane says so with
// `created` — but *this* cntlr has just been rebuilt, so no dm device of
// either td exists when the pre-pass runs. It must message anyway: declining
// here (as the pre-U4 filter did) would hand the slice to a lazy path that no
// longer exists and lose the snapshot outright.
//
// Nothing is quiesced, because nothing is live yet, and no `create_thin` is
// sent for the created origin (U4-S2) — the bare `dmsetup create` re-attaches
// the id the CN21 teardown left in the pool metadata.
func TestSnapshotMessagesWithoutTheOriginDevice(t *testing.T) {
	// U4-S4: td_list order carries no meaning any more. Both orders must
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
			cnSyncup(t, srv, 3, false)
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

// TestCreatedTdIsNeverMessaged is U4-T2: the plain-td half of U4-S1's first
// row. `created` is the control plane's word that dev_id 1 is in every slice
// pool, so a fresh primary attaches it with a bare `dmsetup create` and sends
// nothing. The CN9 order test (§6 test 5) still sees create_thin because its
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

// TestCreatedSnapshotIsNeverMessaged is U4-T3: the same rule for a snapshot.
// Both ids are in the pool, so the pre-pass has nothing to claim — no
// `create_snap`, and therefore no quiesce of an origin that is not even
// carrying IO yet. The second case drops the origin from td_list entirely:
// U2 lets it be deleted once every snapshot of it is created, and that
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

// TestCreatedTdWithAMissingIdIsAnError is U4-T4, the price U4-S2 deliberately
// pays. A created td whose id a pool no longer holds cannot be repaired by a
// message — a `create_thin` would hand the live dev_id a fresh, empty volume
// — so the failing `dmsetup create` is reported as it happened and left
// there. No converge, at any revision, self-heals it.
func TestCreatedTdWithAMissingIdIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	o := reqOpts{revision: 2, primary: true, tds: []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize, Created: true}}}
	reply := syncupBoth(t, srv, o)

	assertNoCall(t, node, "0 create_thin ")
	assertErrorDetails(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice], "No data available", "lost thin id")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertNoCall(t, node, "0 create_thin ")
	assertErrorDetails(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice], "No data available",
		"lost thin id, second converge")
}

// TestUncreatedSnapshotRetriesWhenTheOriginIdIsMissing is U4-T5, R11: the
// origin guarantee is the gateway's to keep and the agent does not re-check
// it. A `create_snap` whose origin the pool lacks fails at the message, the
// `dmsetup create` behind it fails too, and the row says so — with the td
// still `created = false`, which is exactly what makes the next converge try
// again.
func TestUncreatedSnapshotRetriesWhenTheOriginIdIsMissing(t *testing.T) {
	srv, node := newTestServer(t)
	o := reqOpts{revision: 2, primary: true, subsys: subsysForTd(testSnapTd),
		tds: []*pb.ThinDevice{
			{TdId: testSnapTd, DevId: 2, OriId: 7, Size: testTdSize}}}
	message := "cmd dmsetup message " + poolName(srv) + " 0 create_snap 2 7"
	reply := syncupBoth(t, srv, o)

	assertNoCall(t, node, "cmd dmsetup suspend")
	if n := len(node.callsMatching(message)); n != 1 {
		t.Fatalf("%d create_snap calls, want exactly 1", n)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice], "No data available", "orphan snap")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if n := len(node.callsMatching(message)); n != 1 {
		t.Fatalf("the retry sent %d create_snap calls, want exactly 1", n)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testSnapTd].
		GetSliceIdToDmThin()[testSlice], "No data available",
		"orphan snap, second converge")
}

// TestSnapshotWithADeferredSliceStillMessagesTheReadyOne pins that the
// message loop is gated on poolReady and never on tdPlan.deferred, which is
// plan-global (anySliceDeferred, [D15]): with one slice still provisioning the
// ready slice's create_snap must still go out. Dropping it would lose the
// snapshot outright.
//
// U4-S3 also drops the pre-pass's `origin.deferred` early return, and this
// fixture is where that shows: the origin's raid0 was built by the
// revision-2 converge and is still live, so it is quiesced around the one
// message it is possible to send. Before U4 the deferred plan sent the
// caller down ensureThin's lazy path, which bracketed only the per-slice
// origin thin. Quiescing the live raid0 is the CN14 quiesce applied honestly, so
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
// create_snap per slice, none after the raid0 resume". Before U4 a handoff
// map carried that property; it now holds by construction, because ensureThin
// never messages a td with ori_id != 0 (U4-S1) and the pre-pass is the only
// caller of createSnapId. A re-send would put that slice's create_snap after
// the resume — dating its snapshot from after host IO restarted, which is the
// very tear the quiesce exists to prevent. The one-shot failCmd is what makes such a
// retry observable: failCmdAlways would fail it identically.
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
