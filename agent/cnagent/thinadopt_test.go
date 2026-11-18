package cnagent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A new volume over a thin id that holds data is refused (CN14)
//
// When the thin create of a new plain volume fails, whatever the failure —
// the id exists, the pool takes no message, the message was killed — no
// device is there to read; the agent reads the slice pool's device headers
// (`thin_dump --metadata-snap --skip-mappings`), once per slice per pass, and
// adopts the id only while it maps no block or is not there.
// ---------------------------------------------------------------------------

// adoptTd and adoptTd2 are new plain volumes beside the fixture's created
// origin: the ids 2 and 3 a test plants in the pools first.
const (
	adoptTd  = testSnapTd
	adoptTd2 = uint64(0x31)
)

// adoptTds is the td_list of the tests: the created fixture td and the new
// plain volumes of ids.
func adoptTds(ids ...uint32) []*pb.ThinDevice {
	tds := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize, Created: true},
	}
	for i, id := range ids {
		tdId := adoptTd
		if i == 1 {
			tdId = adoptTd2
		}
		tds = append(tds, &pb.ThinDevice{TdId: tdId, DevId: id,
			Size: testTdSize})
	}
	return tds
}

// headerRead is the command line of one slice's header read.
func headerRead(srv *CnAgentServer, sliceId uint64) string {
	return "cmd thin_dump --metadata-snap --skip-mappings " +
		srv.nf.DmPath(names(srv).CnPoolMetaName(testCluster, testCn, testSp,
			sliceId))
}

// adoptSetup converges the origin alone at revision 2, then runs plant on
// the node and resets its calls: the ids planted after the origin's
// converge, whose activation sweep would delete them as strays.
func adoptSetup(
	t *testing.T,
	plant func(srv *CnAgentServer, node *fakeNode),
) (*CnAgentServer, *fakeNode) {
	t.Helper()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, twoSlices: true})
	plant(srv, node)
	node.Reset()
	return srv, node
}

// TestANewVolumeOverAThinIdWithDataIsRefused pins CN14's refusal: a slice
// whose pool holds the new volume's id mapping blocks reads ERROR with
// RES_REASON_THIN_ID_NOT_ADOPTED and the blocks in its details, and builds
// no thin volume over it, while a slice whose id maps nothing adopts it. The
// converge's verdict does not name the refused row; a read finds the volume
// absent and names it.
func TestANewVolumeOverAThinIdWithDataIsRefused(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		node.holdThinData(poolNameOf(srv, testSlice), 2, 7)
		node.holdThinIds(poolNameOf(srv, testSlice2), 2)
	})
	held := cntlrReq(reqOpts{revision: 3, primary: true, twoSlices: true,
		tds: adoptTds(2)})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2), leftoverOk: true})

	rows := reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()
	refused := rows[testSlice]
	if refused.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		refused.GetReason() != pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED ||
		!strings.Contains(refused.GetDetails(), "maps 7 blocks") {
		t.Fatalf("slice 0's row = %v, want ERROR, the reason and the blocks",
			refused)
	}
	assertOk(t, rows[testSlice2], "slice 1, its id empty")
	assertNoCall(t, node, "cmd dmsetup create "+
		thinNameOf(srv, adoptTd, testSlice))
	if !node.hasCall("cmd dmsetup create " +
		thinNameOf(srv, adoptTd, testSlice2)) {
		t.Fatalf("the empty id of slice 1 was not adopted")
	}
	thin := thinNameOf(srv, adoptTd, testSlice)
	assertRowNotNamed(t, reply.GetAgentReply(), "the converge", thin)

	round := rowRound(t, srv, 3)
	if !strings.Contains(round.GetAgentReply().GetDetails(),
		rowNamed(thin, "absent")) {
		t.Fatalf("the read's verdict %q does not name the absent volume",
			round.GetAgentReply().GetDetails())
	}
	// The re-send the name brings refuses again: the id still holds data.
	again := rowResend(t, srv, held, round)
	if again == nil {
		t.Fatal("the read's name brought no re-send")
	}
	if got := again.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice].GetReason(); got !=
		pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED {
		t.Fatalf("the re-send's row reason = %v, want the refusal", got)
	}
}

// TestANewVolumeOverAnEmptyThinIdIsAdopted pins CN14's adoption: an id that
// maps no block — a crash between the message and the create, or a message
// another primary sent — is built over, after one header read per slice; a
// re-apply over the devices then reads no header.
func TestANewVolumeOverAnEmptyThinIdIsAdopted(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		node.holdThinIds(poolNameOf(srv, testSlice), 2)
		node.holdThinIds(poolNameOf(srv, testSlice2), 2)
	})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2)})
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
			GetSliceIdToDmThin()[sliceId],
			fmt.Sprintf("slice %#x", sliceId))
		if n := len(node.callsMatching(headerRead(srv, sliceId))); n != 1 {
			t.Fatalf("slice %#x: %d header reads, want 1", sliceId, n)
		}
	}
	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2)})
	assertNoCall(t, node, "cmd thin_dump")
}

// TestOneThinHeaderReadPerSlice pins CN14's cost: the header read of a slice
// runs at the first new volume whose create fails and
// serves every other such volume of the slice in the pass, so a promotion
// over uncreated volumes runs no more reads than slices, and never a full
// dump of the mappings.
func TestOneThinHeaderReadPerSlice(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		for _, sliceId := range []uint64{testSlice, testSlice2} {
			node.holdThinIds(poolNameOf(srv, sliceId), 2, 3)
		}
	})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2, 3)})
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		if n := len(node.callsMatching(headerRead(srv, sliceId))); n != 1 {
			t.Fatalf("slice %#x: %d header reads for two volumes, want 1",
				sliceId, n)
		}
		for _, tdId := range []uint64{adoptTd, adoptTd2} {
			assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[tdId].
				GetSliceIdToDmThin()[sliceId],
				fmt.Sprintf("td %#x slice %#x", tdId, sliceId))
		}
	}
	if n := len(node.callsMatching("cmd thin_dump")); n != 2 {
		t.Fatalf("%d thin_dump runs, want the two header reads alone", n)
	}
}

// TestAThinHeaderReadThatWaitsIsNamed pins CN14's wait: a header read whose
// reserve is refused because a metadata snapshot is already held, after the
// release and retry, reads the volume MISSING with the refusal's text and no
// reason, and the converge's verdict names it, so the worker's re-send tries
// again once the holder releases.
func TestAThinHeaderReadThatWaitsIsNamed(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		node.holdThinIds(poolNameOf(srv, testSlice), 2)
		node.holdThinIds(poolNameOf(srv, testSlice2), 2)
		pool := poolNameOf(srv, testSlice)
		node.failCmdAlways["message "+pool+" 0 reserve_metadata_snap"] =
			msgIoctlFailed(pool, "Device or resource busy")
	})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2), leftoverOk: true})
	row := reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		row.GetReason() != pb.ResReason_RES_REASON_NONE ||
		!strings.Contains(row.GetDetails(), "held metadata snapshot") {
		t.Fatalf("the waiting row = %v, want MISSING with the wait's text",
			row)
	}
	thin := thinNameOf(srv, adoptTd, testSlice)
	assertNoCall(t, node, "cmd dmsetup create "+thin)
	if !strings.Contains(reply.GetAgentReply().GetDetails(),
		rowNamed(thin, "absent")) {
		t.Fatalf("the converge's verdict %q does not name the waiting "+
			"volume", reply.GetAgentReply().GetDetails())
	}
}

// TestAThinHeaderReadThatFailsRefusesUnnamed pins CN14's other failures: a
// header read that fails for any cause but a held metadata snapshot — here
// a tool that refuses the option — reads ERROR with the refusal's reason and
// the read's text, which the converge's verdict does not name, and the
// metadata snapshot is released.
func TestAThinHeaderReadThatFailsRefusesUnnamed(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		node.holdThinIds(poolNameOf(srv, testSlice), 2)
		node.holdThinIds(poolNameOf(srv, testSlice2), 2)
		node.failCmdAlways["thin_dump --metadata-snap --skip-mappings"] =
			"error: unexpected argument '--skip-mappings' found"
	})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2), leftoverOk: true})
	for _, sliceId := range []uint64{testSlice, testSlice2} {
		row := reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
			GetSliceIdToDmThin()[sliceId]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			row.GetReason() != pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED ||
			!strings.Contains(row.GetDetails(), "--skip-mappings") {
			t.Fatalf("slice %#x's row = %v, want ERROR with the reason and "+
				"the read's text", sliceId, row)
		}
		assertRowNotNamed(t, reply.GetAgentReply(), "the converge",
			thinNameOf(srv, adoptTd, sliceId))
		pool := poolNameOf(srv, sliceId)
		if n := len(node.callsMatching("cmd dmsetup message " + pool +
			" 0 release_metadata_snap")); n != 1 {
			t.Fatalf("slice %#x: %d releases after the failed read, want 1",
				sliceId, n)
		}
	}
}

// TestAnExistingIdTheHeadersLackIsNoRefusal pins the header read's absent
// id: a create that answers the id exists while the pool's headers do not
// list it — the id went between the two — is no refusal, and the thin
// table's create decides: dm-thin refuses it, an error with no reason.
func TestAnExistingIdTheHeadersLackIsNoRefusal(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		pool := poolNameOf(srv, testSlice)
		node.failCmdAlways["message "+pool+" 0 create_thin 2"] =
			msgIoctlFailed(pool, "File exists")
	})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2), leftoverOk: true})
	row := reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice]
	if n := len(node.callsMatching(headerRead(srv, testSlice))); n != 1 {
		t.Fatalf("%d header reads, want 1", n)
	}
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		row.GetReason() != pb.ResReason_RES_REASON_NONE {
		t.Fatalf("the row = %v, want the create's ERROR with no reason", row)
	}
}

// TestAHandMadeDeviceOverARefusedIdIsTaken pins the way that keeps the data
// of a refused id (CN14; operations.md, A new volume refused for a thin id
// that holds data): a thin device an operator makes by hand on the primary
// over the id, under the volume's own name, is a device already there, which
// the next converge takes with no create message and no header read.
func TestAHandMadeDeviceOverARefusedIdIsTaken(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		node.holdThinData(poolNameOf(srv, testSlice), 2, 7)
		node.holdThinIds(poolNameOf(srv, testSlice2), 2)
	})
	o := reqOpts{revision: 3, primary: true, twoSlices: true,
		tds: adoptTds(2), leftoverOk: true}
	reply := syncupCntlrAt(t, srv, o)
	if got := reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice].GetReason(); got !=
		pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED {
		t.Fatalf("fixture: slice 0's reason = %v, want the refusal", got)
	}

	pool, err := srv.dmDevNo(t.Context(), nil, poolNameOf(srv, testSlice))
	if err != nil {
		t.Fatalf("the pool's number: %v", err)
	}
	thin := thinNameOf(srv, adoptTd, testSlice)
	if err := srv.dm.Create(t.Context(), thin, dmTable("thin",
		testTdSize/2/512, []string{pool, "2"})); err != nil {
		t.Fatalf("the device made by hand: %v", err)
	}
	node.Reset()
	reply = syncupCntlrAt(t, srv, o)
	assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice], "slice 0 over the hand-made device")
	assertNoCall(t, node, "create_thin")
	assertNoCall(t, node, "cmd thin_dump")
	assertOk(t, reply.GetCntlrInfo().GetTdIdToRaid0()[adoptTd],
		"the volume's raid0")
}

// TestAKilledThinCreateIsFollowedByTheHeaders pins CN14's guard on a create
// message the soft timeout killed, which the kernel may have carried out:
// the slice's header read follows, once, and decides — an id that maps
// blocks is refused with the reason and no thin table loads over it; an id
// the killed message made, mapping nothing, is adopted; an id the message
// never made is not there, and the thin table's create decides: dm-thin
// refuses it, an error with no reason.
func TestAKilledThinCreateIsFollowedByTheHeaders(t *testing.T) {
	cases := []struct {
		name   string
		plant  func(srv *CnAgentServer, node *fakeNode, msg string)
		check  func(t *testing.T, row *pb.ResInfo)
		create bool
	}{
		{
			name: "an id with data",
			plant: func(srv *CnAgentServer, node *fakeNode, msg string) {
				node.holdThinData(poolNameOf(srv, testSlice), 2, 7)
				node.killCmdAlways[msg] = true
			},
			check: func(t *testing.T, row *pb.ResInfo) {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					row.GetReason() !=
						pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED ||
					!strings.Contains(row.GetDetails(), "maps 7 blocks") {
					t.Fatalf("the row = %v, want ERROR, the reason and "+
						"the blocks", row)
				}
			},
		},
		{
			name: "the id the killed message made",
			plant: func(srv *CnAgentServer, node *fakeNode, msg string) {
				node.killCmd[msg] = true
			},
			check: func(t *testing.T, row *pb.ResInfo) {
				assertOk(t, row, "slice 0 over its own empty id")
			},
			create: true,
		},
		{
			name: "no id made",
			plant: func(srv *CnAgentServer, node *fakeNode, msg string) {
				node.killCmdNoEffectAlways[msg] = true
			},
			check: func(t *testing.T, row *pb.ResInfo) {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					row.GetReason() != pb.ResReason_RES_REASON_NONE {
					t.Fatalf("the row = %v, want the create's ERROR with "+
						"no reason", row)
				}
			},
			create: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, node := adoptSetup(t, func(srv *CnAgentServer,
				node *fakeNode) {
				node.holdThinIds(poolNameOf(srv, testSlice2), 2)
				c.plant(srv, node,
					"message "+poolNameOf(srv, testSlice)+" 0 create_thin 2")
			})
			reply := syncupCntlrAt(t, srv, reqOpts{revision: 3,
				primary: true, twoSlices: true, tds: adoptTds(2),
				leftoverOk: true})
			if n := len(node.callsMatching(
				headerRead(srv, testSlice))); n != 1 {
				t.Fatalf("%d header reads after the killed message, want 1",
					n)
			}
			c.check(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
				GetSliceIdToDmThin()[testSlice])
			create := "cmd dmsetup create " +
				thinNameOf(srv, adoptTd, testSlice)
			if !c.create {
				assertNoCall(t, node, create)
			} else if !node.hasCall(create) {
				t.Fatalf("no thin table create after the header read")
			}
		})
	}
}

// TestAPoolThatTakesNoMessageRefusesANewVolume pins CN14's guard on a pool
// that takes no message — read-only, out of metadata space or under
// needs_check, where dm-thin answers every message EOPNOTSUPP but still
// loads a thin table over an id it holds: the header read follows, its
// reserve is refused the same way, and the read's failure refuses the id
// with the reason, so no thin table loads over an id that may hold another
// volume's data.
func TestAPoolThatTakesNoMessageRefusesANewVolume(t *testing.T) {
	srv, node := adoptSetup(t, func(srv *CnAgentServer, node *fakeNode) {
		pool := poolNameOf(srv, testSlice)
		node.holdThinData(pool, 2, 7)
		node.holdThinIds(poolNameOf(srv, testSlice2), 2)
		for _, msg := range []string{"create_thin 2",
			"reserve_metadata_snap"} {
			node.failCmdAlways["message "+pool+" 0 "+msg] =
				msgIoctlFailed(pool, "Operation not supported")
		}
	})
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		twoSlices: true, tds: adoptTds(2), leftoverOk: true})
	row := reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		row.GetReason() != pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED ||
		!strings.Contains(row.GetDetails(), "header read failed") ||
		!strings.Contains(row.GetDetails(), "Operation not supported") {
		t.Fatalf("the row = %v, want ERROR, the reason and the read's "+
			"failure", row)
	}
	thin := thinNameOf(srv, adoptTd, testSlice)
	assertNoCall(t, node, "cmd dmsetup create "+thin)
	assertRowNotNamed(t, reply.GetAgentReply(), "the converge", thin)
	assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[adoptTd].
		GetSliceIdToDmThin()[testSlice2], "slice 1, whose pool takes it")
}
