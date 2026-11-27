package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The primary's discard of a deleted thin volume (cnagent.md CN14, CN21)
//
// A pool's `delete` message frees a thin id's blocks in the pool's metadata
// alone, so the disk nodes under the pool never learn that the blocks are
// free. L7 therefore discards each volume whose id it is about to delete,
// before its removal and before the delete, and nowhere else: a volume
// removed with its pool unwanted holds data the next primary attaches. The
// discard is best effort and bounded: it skips a slice where it could wait in
// the kernel under the cntlr's locks, the pass sends at most
// CnThinDiscardPassChunks chunks, and nothing that becomes of it stops the
// removal or the delete.
// ---------------------------------------------------------------------------

// discardLine is the fake node's record of one in-process discard of a dm
// device (fakeNode.discardRanges).
func discardLine(name string, offset, length uint64) string {
	return fmt.Sprintf("discard --offset %d --length %d /dev/mapper/%s",
		offset, length, name)
}

// discardsOf are the recorded discards of one dm device.
func discardsOf(node *fakeNode, name string) []string {
	var out []string
	for _, call := range node.callsMatching("discard --offset ") {
		if strings.HasSuffix(call, " /dev/mapper/"+name) {
			out = append(out, call)
		}
	}
	return out
}

// syncupCntlrAny drives one SyncupCntlr and returns its reply whatever its
// code: a pass whose leg is dead or whose pool refuses its messages names
// rows the tests below do not judge.
func syncupCntlrAny(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	return reply
}

// dropTestTd is the request that deletes testTd and keeps testSnapTd, with
// the fixture's namespace moved onto testSnapTd (TestThinDeleteOnlyUnderWantedPool).
func dropTestTd(revision uint64) reqOpts {
	return reqOpts{
		revision: revision, primary: true,
		tds:    []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2, Size: testTdSize}},
		subsys: subsysForTd(testSnapTd),
	}
}

// assertVolumeGone is the rest of a deletion: the volume is removed and its
// id deleted from its pool, whatever its discard did.
func assertVolumeGone(
	t *testing.T,
	node *fakeNode,
	thin, pool string,
	devId uint32,
) {
	t.Helper()
	if _, ok := node.dms[thin]; ok {
		t.Fatalf("the thin volume %s survived its td's deletion", thin)
	}
	if !node.hasCall(deleteMsg(pool, devId)) {
		t.Fatalf("no delete of id %d was sent to %s:\n%s", devId, pool,
			strings.Join(node.Calls(), "\n"))
	}
	if node.thinPools[pool][devId] {
		t.Fatalf("the pool %s still holds id %d", pool, devId)
	}
}

// TestADeletedVolumeIsDiscardedInEachSlice pins the discard itself: a td that
// leaves td_list while its pools stay has each slice's volume discarded from
// its first sector to its highest mapped one, before the volume's removal and
// before its id's delete message, in each slice.
func TestADeletedVolumeIsDiscardedInEachSlice(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, tds: twoTds(), twoSlices: true})
	// Four blocks of 1 MiB written in each slice's volume of testTd, and
	// two in testSnapTd's, which stays.
	for _, slice := range []uint64{testSlice, testSlice2} {
		node.mapThinBlocks(poolNameOf(srv, slice), 1, 4)
		node.mapThinBlocks(poolNameOf(srv, slice), 2, 2)
	}

	node.Reset()
	o := dropTestTd(3)
	o.twoSlices = true
	syncupCntlrAt(t, srv, o)

	for _, slice := range []uint64{testSlice, testSlice2} {
		thin := thinNameOf(srv, testTd, slice)
		pool := poolNameOf(srv, slice)
		assertOrder(t, node,
			discardLine(thin, 0, 4*testBlockSize),
			"cmd dmsetup remove "+thin,
			deleteMsg(pool, 1))
		if got := discardsOf(node, thin); len(got) != 1 {
			t.Fatalf("slice %x: want one discard of %s, got %v",
				slice, thin, got)
		}
		assertVolumeGone(t, node, thin, pool, 1)
		if got := discardsOf(node, thinNameOf(srv, testSnapTd, slice)); len(got) != 0 {
			t.Fatalf("slice %x: the volume that stays was discarded: %v",
				slice, got)
		}
	}
}

// TestNoDiscardWhereTheVolumeIsOnlyDeactivated pins the gate: a volume
// removed while its pool is unwanted is the data the next primary attaches,
// so neither a demotion nor a level that drops the pool discards it, even in
// the request that also deletes a td.
func TestNoDiscardWhereTheVolumeIsOnlyDeactivated(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    reqOpts
	}{
		{"a demotion", reqOpts{
			revision: 3, primary: false,
			tds:    []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2, Size: testTdSize}},
			subsys: subsysForTd(testSnapTd),
		}},
		{"a level that drops the pool", reqOpts{
			revision: 3, primary: true,
			tds:    []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2, Size: testTdSize}},
			subsys: subsysForTd(testSnapTd),
			level:  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, tds: twoTds()})
			pool := poolName(srv)
			node.mapThinBlocks(pool, 1, 4)
			node.mapThinBlocks(pool, 2, 4)

			node.Reset()
			syncupCntlrAny(t, srv, tc.o)
			for _, td := range []uint64{testTd, testSnapTd} {
				if _, ok := node.dms[thinName(srv, td)]; ok {
					t.Fatalf("the thin volume of td %x survived", td)
				}
			}
			assertNoCall(t, node, "discard --offset")
			assertNoCall(t, node, "0 delete ")
		})
	}
}

// TestNoDiscardWhereItCouldWait pins the skips: a discard into a suspended
// device, or through a leg whose path is not live, waits in the kernel under
// the cntlr's locks, and one to a pool that does not read read-write unmaps
// nothing. Each sends no discard, and the volume is removed and its id
// deleted all the same where the pool takes the message.
func TestNoDiscardWhereItCouldWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		// deletes says the pool still takes the delete message.
		deletes bool
		set     func(srv *CnAgentServer, node *fakeNode)
	}{
		{"a suspended volume", true, func(srv *CnAgentServer, node *fakeNode) {
			node.dms[thinName(srv, testTd)].suspended = true
		}},
		{"a suspended pool", true, func(srv *CnAgentServer, node *fakeNode) {
			node.dms[poolName(srv)].suspended = true
		}},
		{"a suspended leg wrapper", true, func(srv *CnAgentServer, node *fakeNode) {
			node.dms[legName(srv, testDataLeg)].suspended = true
		}},
		{"a read-only pool", false, func(srv *CnAgentServer, node *fakeNode) {
			node.dms[poolName(srv)].poolMode = "ro"
		}},
		{"a data leg that is not live", true, func(srv *CnAgentServer, node *fakeNode) {
			node.setCtrlState(legNqn(srv, testDataLeg), testIp, testSvcId,
				"connecting")
		}},
		{"a meta leg that is not live", true, func(srv *CnAgentServer, node *fakeNode) {
			node.setCtrlState(legNqn(srv, testMetaLeg), testIp, testSvcId,
				"connecting")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, tds: twoTds()})
			pool := poolName(srv)
			node.mapThinBlocks(pool, 1, 4)
			tc.set(srv, node)

			node.Reset()
			syncupCntlrAny(t, srv, dropTestTd(3))
			assertNoCall(t, node, "discard --offset")
			if !tc.deletes {
				return
			}
			assertVolumeGone(t, node, thinName(srv, testTd), pool, 1)
		})
	}
}

// TestNoDiscardWhileALegHasTwoSides: while a leg of a slice has two sides,
// a migration's, the slice's pool passes no discard down to its data device
// (CN13), so the delete sends its volume there no discard, which would free
// the blocks in the pool alone, as the delete does anyway; the other
// slice's volume is discarded, and both are deleted.
func TestNoDiscardWhileALegHasTwoSides(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds(),
		twoSlices: true, extraSide: true})
	for _, slice := range []uint64{testSlice, testSlice2} {
		node.mapThinBlocks(poolNameOf(srv, slice), 1, 4)
	}

	node.Reset()
	o := dropTestTd(3)
	o.twoSlices, o.extraSide = true, true
	syncupCntlrAt(t, srv, o)

	if got := discardsOf(node, thinNameOf(srv, testTd, testSlice)); len(got) != 0 {
		t.Fatalf("the volume of the slice whose leg has two sides was "+
			"discarded: %v", got)
	}
	if got := discardsOf(node, thinNameOf(srv, testTd, testSlice2)); len(got) != 1 {
		t.Fatalf("want one discard of the other slice's volume, got %v", got)
	}
	for _, slice := range []uint64{testSlice, testSlice2} {
		assertVolumeGone(t, node, thinNameOf(srv, testTd, slice),
			poolNameOf(srv, slice), 1)
	}
}

// TestAFailedDiscardStopsNothing pins best effort: a discard the kernel
// refuses is logged, and the volume is removed and its id deleted in the
// same pass.
func TestAFailedDiscardStopsNothing(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds()})
	pool := poolName(srv)
	thin := thinName(srv, testTd)
	node.mapThinBlocks(pool, 1, 4)
	node.failCmdAlways["discard --offset 0 --length 4194304 /dev/mapper/"+
		thin] = "refused"

	node.Reset()
	syncupCntlrAt(t, srv, dropTestTd(3))
	assertOrder(t, node,
		discardLine(thin, 0, 4*testBlockSize),
		"cmd dmsetup remove "+thin,
		deleteMsg(pool, 1))
	assertVolumeGone(t, node, thin, pool, 1)
}

// TestTheDiscardBudgetEndsThePassesDiscards pins the bound: a pass sends at
// most CnThinDiscardPassChunks chunks of CnThinDiscardChunkBytes over all
// its volumes. The first volume, mapped past the budget, takes all of it,
// top chunk first from the end of its highest mapped block down; the second
// gets none; both are removed and their ids deleted.
func TestTheDiscardBudgetEndsThePassesDiscards(t *testing.T) {
	const bigTdSize = uint64(32 << 30)
	chunk := uint64(common.CnThinDiscardChunkBytes)
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		tds: []*pb.ThinDevice{
			{TdId: testTd, DevId: 1, Size: bigTdSize},
			{TdId: testSnapTd, DevId: 2, Size: testTdSize},
		}})
	pool := poolName(srv)
	mapped := (common.CnThinDiscardPassChunks + 4) * chunk / testBlockSize
	node.mapThinBlocks(pool, 1, mapped)
	node.mapThinBlocks(pool, 2, 4)

	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{
		revision: 3, primary: true,
		tds:    []*pb.ThinDevice{},
		subsys: map[string]*pb.Subsystem{},
	})
	big, small := thinName(srv, testTd), thinName(srv, testSnapTd)
	got := discardsOf(node, big)
	if len(got) != common.CnThinDiscardPassChunks {
		t.Fatalf("want %d discards of the first volume, got %d: %v",
			common.CnThinDiscardPassChunks, len(got), got)
	}
	top := uint64(common.CnThinDiscardPassChunks + 4)
	for i, call := range got {
		want := discardLine(big, (top-1-uint64(i))*chunk, chunk)
		if call != want {
			t.Fatalf("discard %d is %q, want %q", i, call, want)
		}
	}
	if got := discardsOf(node, small); len(got) != 0 {
		t.Fatalf("the second volume was discarded past the budget: %v", got)
	}
	if node.emptyDiscards != 0 {
		t.Fatalf("%d discard calls carried no range", node.emptyDiscards)
	}
	assertVolumeGone(t, node, big, pool, 1)
	assertVolumeGone(t, node, small, pool, 2)
}

// TestAReSendGoesOnBelowACutDiscard pins the top-first order: a volume mapped
// past the budget has its top chunks discarded first, so when its removal
// then fails — as one does that meets udev's probe after the discard's close
// — the re-send's pass reads a lower highest mapped sector and discards what
// is left below it, not the same chunks again.
func TestAReSendGoesOnBelowACutDiscard(t *testing.T) {
	const bigTdSize = uint64(32 << 30)
	chunk := uint64(common.CnThinDiscardChunkBytes)
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		tds: []*pb.ThinDevice{
			{TdId: testTd, DevId: 1, Size: bigTdSize},
			{TdId: testSnapTd, DevId: 2, Size: testTdSize},
		}})
	pool := poolName(srv)
	big := thinName(srv, testTd)
	extra := uint64(4)
	node.mapThinBlocks(pool, 1,
		(common.CnThinDiscardPassChunks+extra)*chunk/testBlockSize)

	node.Reset()
	node.failCmd["dmsetup remove "+big] = "device-mapper: remove ioctl on " +
		big + " failed: Device or resource busy"
	drop := dropTestTd(3)
	drop.leftoverOk = true
	syncupCntlrAt(t, srv, drop)
	if _, ok := node.dms[big]; !ok {
		t.Fatalf("the volume was removed in the pass whose removal failed")
	}
	first := discardsOf(node, big)
	if len(first) != common.CnThinDiscardPassChunks ||
		first[len(first)-1] != discardLine(big, extra*chunk, chunk) {
		t.Fatalf("the cut pass sent %v, want %d chunks down to %d GiB",
			first, common.CnThinDiscardPassChunks, extra)
	}

	node.Reset()
	syncupCntlrAt(t, srv, drop)
	second := discardsOf(node, big)
	var want []string
	for i := extra; i > 0; i-- {
		want = append(want, discardLine(big, (i-1)*chunk, chunk))
	}
	if strings.Join(second, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the re-send sent %v, want %v", second, want)
	}
	assertVolumeGone(t, node, big, pool, 1)
}

// TestAPartialLastBlockIsNotDiscardedAgain pins the part-block rule: a
// volume that ends inside a pool block keeps that block once written, as
// dm-thin unmaps only blocks a discard covers whole. The discard stops at
// the last whole block, and a volume whose status reads only that block
// mapped gets none: a re-send after a removal that failed opens the volume
// for nothing otherwise, and makes udev probe it again.
func TestAPartialLastBlockIsNotDiscardedAgain(t *testing.T) {
	// One slice, so the volume is the td's size: 64 MiB and half a block.
	const partTdSize = testTdSize + testBlockSize/2
	wholeBlocks := testTdSize / testBlockSize
	for _, tc := range []struct {
		name string
		// lo and blocks are the run the volume maps, in blocks; the last
		// block of the run is the part-block.
		lo, blocks uint64
		// first is the first pass's discards.
		first []string
	}{
		{"only the part-block mapped", wholeBlocks, 1, nil},
		{"the whole volume mapped", 0, wholeBlocks + 1, []string{
			"discard --offset 0 --length 67108864 /dev/mapper/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				tds: []*pb.ThinDevice{
					{TdId: testTd, DevId: 1, Size: partTdSize},
					{TdId: testSnapTd, DevId: 2, Size: testTdSize},
				}})
			pool := poolName(srv)
			thin := thinName(srv, testTd)
			node.mapThinRun(pool, 1, tc.lo, tc.blocks)

			node.Reset()
			node.failCmd["dmsetup remove "+thin] = "device-mapper: " +
				"remove ioctl on " + thin + " failed: Device or resource busy"
			drop := dropTestTd(3)
			drop.leftoverOk = true
			syncupCntlrAt(t, srv, drop)
			var want []string
			for _, line := range tc.first {
				want = append(want, line+thin)
			}
			if got := discardsOf(node, thin); strings.Join(got, "\n") !=
				strings.Join(want, "\n") {
				t.Fatalf("the first pass sent %v, want %v", got, want)
			}
			if _, ok := node.dms[thin]; !ok {
				t.Fatalf("the volume was removed in the pass whose " +
					"removal failed")
			}

			node.Reset()
			syncupCntlrAt(t, srv, drop)
			assertNoCall(t, node, "discard --offset")
			if node.emptyDiscards != 0 {
				t.Fatalf("%d discard calls carried no range",
					node.emptyDiscards)
			}
			assertVolumeGone(t, node, thin, pool, 1)
		})
	}
}
