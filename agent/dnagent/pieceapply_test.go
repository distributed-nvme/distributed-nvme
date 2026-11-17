package dnagent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestPushOntoAServingDmClonePausesItsHolders is DN15 on a dm-clone that
// serves: the primary's per-CN dm-linear maps it, so the piece's discards go
// only once the background copy is stopped and that dm-linear is suspended,
// and both are restored after them (agent.ApplySkipRanges). The standby's
// dm-linear, on its own dm-error, is not touched.
func TestPushOntoAServingDmClonePausesItsHolders(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	primaryLin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		testCn0)
	standbyLin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		testCn1)
	cloneNo := node.devNo[nf.DmPath(cloneName)]
	if cloneNo == "" ||
		!strings.Contains(node.dms[primaryLin].table, cloneNo) {
		t.Fatalf("precondition: the primary's dm-linear %q does not map "+
			"the dm-clone %s", node.dms[primaryLin].table, cloneNo)
	}
	if node.dms[cloneName].noHydration {
		t.Fatal("precondition: the dm-clone's copy is off")
	}

	node.Reset()
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil || reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("PushMigrBitmap: %v, %v", err, reply.GetAgentReply())
	}
	assertOrder(t, node,
		"cmd dmsetup message "+cloneName+" 0 disable_hydration",
		"cmd dmsetup suspend "+primaryLin,
		"discard --offset 3145728 --length 1048576 "+nf.DmPath(cloneName),
		"cmd dmsetup resume "+primaryLin,
		"cmd dmsetup message "+cloneName+" 0 enable_hydration",
	)
	if node.hasCall("cmd dmsetup suspend " + standbyLin) {
		t.Error("the standby's dm-linear, which maps its dm-error, was paused")
	}
	if node.dms[primaryLin].suspended || node.dms[cloneName].noHydration {
		t.Fatal("the push left the dm-linear suspended or the copy off")
	}
}

// TestPushOntoASuspendedDmCloneWaitsForItsResume: a piece for a suspended
// dm-clone sends no discard, which would wait for the
// resume with the side's lock held, and still counts as applied; the
// converge that resumes the dm-clone applies it then (DN15).
func TestPushOntoASuspendedDmCloneWaitsForItsResume(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	node.dms[cloneName].suspended = true

	node.Reset()
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil || reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("PushMigrBitmap: %v, %v", err, reply.GetAgentReply())
	}
	if node.hasCall("discard --offset") ||
		node.hasCall("cmd dmsetup suspend") ||
		node.hasCall("disable_hydration") {
		t.Fatalf("a suspended dm-clone was touched: %q", node.Calls())
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if got := srv.bitmapInfo(st).GetBmIdxList(); len(got) != 1 ||
		got[0] != 0 {
		t.Fatalf("applied set = %v, want the piece counted", got)
	}

	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup resume "+cloneName,
		"discard --offset 3145728 --length 1048576 "+nf.DmPath(cloneName),
	)
}

// TestAReloadedDmCloneGetsItsPiecesAgain is DN15's re-apply on a reload: a
// converge that finds the dm-clone's table differing reloads it, and applies
// the stored pieces again after the reload.
func TestAReloadedDmCloneGetsItsPiecesAgain(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, err := srv.PushMigrBitmap(ctx,
		pushReq(0, testMigrId, []byte{0x05})); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	// The table the kernel holds no longer has the wanted region size, so
	// the converge reloads it (ensureDmClone).
	node.mu.Lock()
	dm := node.dms[cloneName]
	fields := strings.Fields(dm.table)
	fields[6] = "16"
	dm.table = strings.Join(fields, " ")
	node.mu.Unlock()

	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup reload "+cloneName,
		"discard --offset 3145728 --length 1048576 "+nf.DmPath(cloneName),
	)
}

// TestARepeatedPieceIsNotAppliedAgain is DN15's repeat: a piece the applied
// set holds byte for byte is answered OK with no removal, no save and no
// apply, and the chunk above it keeps its place, files and set alike, so a
// worker that pushes every piece again after its read does not save and
// apply every piece above it again, each apply pausing the volume; a piece
// at the same index with other bytes is saved and applied.
func TestARepeatedPieceIsNotAppliedAgain(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	push := func(bmIdx uint32, bitmap []byte) {
		t.Helper()
		reply, err := srv.PushMigrBitmap(ctx,
			pushReq(bmIdx, testMigrId, bitmap))
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("PushMigrBitmap %d: %v, %v", bmIdx, err,
				reply.GetAgentReply())
		}
	}
	push(0, []byte{0x05})
	push(1, []byte{0x00})

	node.Reset()
	push(0, []byte{0x05})
	if node.hasCall("discard --offset") ||
		node.hasCall("cmd dmsetup suspend") ||
		node.hasCall("disable_hydration") {
		t.Fatalf("a repeated piece was applied again: %q", node.Calls())
	}
	piecePath := nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigrId, 0)
	if node.hasCall("writeproto " + piecePath) {
		t.Fatalf("a repeated piece was saved again: %q", node.Calls())
	}
	if got := node.callsMatching("cmd rm -f "); len(got) != 0 {
		t.Fatalf("a repeated piece removed chunk files: %q", got)
	}
	key := sideKey(testCluster, testDn, testSp, testSide)
	if got := srv.bitmapInfo(srv.getSide(key)).GetBmIdxList(); !slices.Equal(
		got, []uint32{0, 1}) {
		t.Fatalf("applied set = %v, want [0 1]: the chunk above a repeated "+
			"piece keeps its place", got)
	}
	chunk1 := nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigrId, 1)
	node.mu.Lock()
	_, kept := node.protos[chunk1]
	node.mu.Unlock()
	if !kept {
		t.Fatal("a repeated piece removed the chunk file above it")
	}

	node.Reset()
	push(0, []byte{0x07})
	assertOrder(t, node,
		"writeproto "+piecePath,
		"cmd dmsetup message "+cloneName+" 0 disable_hydration",
		"discard --offset 3145728 --length 3145728 "+nf.DmPath(cloneName),
		"cmd dmsetup message "+cloneName+" 0 enable_hydration",
	)
}

// TestASavedPieceDropsTheOlderChunksAboveIt is DN15's drop: a chunk an older
// build left at index 1, behind a chunk 0 of another length, and that the
// restart reloaded (DN2), goes when a new piece 0 is saved, so it never joins
// the new piece at the wrong place. The applied set is [0], and no discard
// lands past the new piece's regions. A chunk file at index 2 that the
// reload never loaded goes too, as the removal is by path, so no later
// restart can bring it into the set.
func TestASavedPieceDropsTheOlderChunksAboveIt(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	push := func(srv *DnAgentServer, bmIdx uint32, bitmap []byte) {
		t.Helper()
		reply, err := srv.PushMigrBitmap(ctx,
			pushReq(bmIdx, testMigrId, bitmap))
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("PushMigrBitmap %d: %v, %v", bmIdx, err,
				reply.GetAgentReply())
		}
	}
	// What an older build left: a chunk 0 of one byte, and a chunk 1 behind
	// it whose every bit is skippable.
	push(srv, 0, []byte{0x00})
	push(srv, 1, []byte{0xff})
	stopTestServer(t, srv)
	node.Reset()
	srv = startTestServer(t, node)
	key := sideKey(testCluster, testDn, testSp, testSide)
	if got := srv.bitmapInfo(srv.getSide(key)).GetBmIdxList(); !slices.Equal(
		got, []uint32{0, 1}) {
		t.Fatalf("precondition: applied set after the restart = %v, want "+
			"[0 1]", got)
	}

	chunk0 := nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigrId, 0)
	chunk1 := nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigrId, 1)
	chunk2 := nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigrId, 2)
	unloaded, err := proto.Marshal(pushReq(2, testMigrId, []byte{0xff}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	node.mu.Lock()
	node.protos[chunk2] = unloaded
	node.dms[cloneName].discards = nil
	node.mu.Unlock()
	node.Reset()
	// The new piece 0 is two bytes long: its regions are the leg's first 16
	// data regions, and only the first is skippable.
	push(srv, 0, []byte{0x01, 0x00})
	node.mu.Lock()
	_, kept := node.protos[chunk1]
	_, keptUnloaded := node.protos[chunk2]
	discards := slices.Clone(node.dms[cloneName].discards)
	node.mu.Unlock()
	if want := []string{"--offset 3145728 --length 1048576"}; !slices.Equal(
		discards, want) {
		t.Errorf("discards = %v, want %v: the new piece's one skippable "+
			"region and nothing past its own", discards, want)
	}
	if got := srv.bitmapInfo(srv.getSide(key)).GetBmIdxList(); !slices.Equal(
		got, []uint32{0}) {
		t.Errorf("applied set = %v, want [0]", got)
	}
	if kept {
		t.Errorf("the older chunk 1 is still in the store")
	}
	if keptUnloaded {
		t.Errorf("the chunk 2 the reload left unloaded is still in the store")
	}
	// The removal comes before the save (DN15).
	assertOrder(t, node,
		"cmd rm -f "+chunk0+" "+chunk1,
		"writeproto "+chunk0,
	)
}

// TestAPieceNotSavedLeavesNoOlderChunkBelowALaterOne is DN15's failure rules
// with the drop. A removal of the stored chunks that fails and a save that
// fails after the removal both refuse the push (ReplyCodeSaveFailed) and
// apply nothing, so the worker's plan ends before it saves a later piece
// over a chunk the removal may have left, or over one a crash brings back
// because no save synced the directory after the removal. A removal that
// fails leaves the applied set as it was; a save that fails leaves the set
// without a chunk at its index or above, so no older chunk at that index
// joins a later piece.
func TestAPieceNotSavedLeavesNoOlderChunkBelowALaterOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    func(srv *DnAgentServer, node *fakeNode, chunk0 string)
		wantSet []uint32
	}{
		{"the removal fails", func(_ *DnAgentServer, node *fakeNode,
			_ string) {
			node.mu.Lock()
			node.failCmd["cmd rm -f "] = "rm: Input/output error"
			node.mu.Unlock()
		}, []uint32{0, 1}},
		{"the save fails", func(srv *DnAgentServer, node *fakeNode,
			chunk0 string) {
			failSaves(srv, node, onlyPath(chunk0))
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			for idx, bitmap := range [][]byte{{0x00}, {0xff}} {
				if _, err := srv.PushMigrBitmap(ctx, pushReq(uint32(idx),
					testMigrId, bitmap)); err != nil {
					t.Fatalf("PushMigrBitmap %d: %v", idx, err)
				}
			}
			chunk0 := nf.LocalMigrBmPath(testCluster, testDn, testSp,
				testMigrId, 0)
			cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp,
				testMigrId)
			tc.fail(srv, node, chunk0)
			node.mu.Lock()
			node.dms[cloneName].discards = nil
			node.mu.Unlock()
			node.Reset()
			reply, err := srv.PushMigrBitmap(ctx,
				pushReq(0, testMigrId, []byte{0x01, 0x00}))
			if err != nil || reply.GetAgentReply().GetCode() !=
				common.ReplyCodeSaveFailed {
				t.Fatalf("PushMigrBitmap: %v, %v, want ReplyCodeSaveFailed",
					err, reply.GetAgentReply())
			}
			st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
			if got := srv.bitmapInfo(st).GetBmIdxList(); !slices.Equal(
				got, tc.wantSet) {
				t.Errorf("applied set = %v, want %v", got, tc.wantSet)
			}
			node.mu.Lock()
			discards := slices.Clone(node.dms[cloneName].discards)
			node.mu.Unlock()
			if len(discards) != 0 || node.hasCall("disable_hydration") {
				t.Errorf("a piece not saved was applied: %v, %q", discards,
					node.Calls())
			}
			if node.hasCall("writeproto") {
				t.Errorf("a refused piece was saved: %q", node.Calls())
			}
		})
	}
}
