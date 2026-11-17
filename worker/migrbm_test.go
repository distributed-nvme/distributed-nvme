package worker

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// BM7 — the destination side's child reads the migration's bitmap from the
// primary and pushes its pieces (and BM2, BM4, BM6 for a migration)
// ---------------------------------------------------------------------------

// testCloneStatus is a dm-clone status line as dmsetup prints it.
const testCloneStatus = "0 2048 clone 8 1/1024 8 128/256 0 1 " +
	"no_discard_passdown 4 hydration_threshold 1 hydration_batch_size 1 rw"

// cloneShownInfo is a dst side's report once its dm-clone stands: both
// migration rows OK, the dm-clone row's details its status line.
func cloneShownInfo() *pb.SideInfo {
	return &pb.SideInfo{
		MigrDstInfo: &pb.SideInfo_MigrDstInfo{
			TargetInfo: resOk("target"),
			DmCloneInfo: &pb.ResInfo{
				Status:  pb.ResStatus_RES_STATUS_OK,
				Details: testCloneStatus,
			},
		},
	}
}

// migrBmFixture is spFixture with the migration's dst side provisioned and
// its cutover_pending as pending.
func migrBmFixture(pending bool) *model.SpState {
	state := spFixture()
	state.Migrs[spMigrName].CutoverPending = pending
	dstSide(state).Provisioned = true
	return state
}

// migrBmAgents is the dst DN's applied set for the fixture's migration, which
// its SyncupSide replies report as bm_info, and the bitmap the primary's CN
// answers GetLegBm with. The dst DN holds the revision of its last SyncupSide,
// as an agent does, and answers its rounds with it, so a worker meets a
// SyncupSide only while the agent lacks the desired revision.
type migrBmAgents struct {
	mu      sync.Mutex
	applied []uint32
	heldRev uint64
	// saveFails makes every SyncupSide of the dst DN fail its save
	// (ReplyCodeSaveFailed), so the agent never holds the desired revision.
	saveFails bool
	legBm     []byte
	// failReads is how many GetLegBm calls, first, fail with failCode
	// (UNAVAILABLE when unset) before one answers.
	failReads int
	failCode  codes.Code
	// hold, when set, keeps every GetLegBm waiting until it is closed.
	hold chan struct{}
}

func (m *migrBmAgents) setApplied(applied ...uint32) {
	m.mu.Lock()
	m.applied = applied
	m.mu.Unlock()
}

// restartDst is the dst DN's agent restarting: it holds no revision until
// its next SyncupSide.
func (m *migrBmAgents) restartDst() {
	m.mu.Lock()
	m.heldRev = 0
	m.mu.Unlock()
}

// serveMigrBm wires the stubs of h as serveCutover does, with the dst side's
// SyncupSide replies carrying the applied set and the primary's CN answering
// GetLegBm.
func serveMigrBm(h *spHarness, a *cutoverAgents, m *migrBmAgents) {
	serveCutover(h, a)
	h.sides[spDnC].syncupReply = func(
		req *pb.SyncupSideRequest,
	) *pb.SyncupSideReply {
		m.mu.Lock()
		defer m.mu.Unlock()
		applied := slices.Clone(m.applied)
		if m.saveFails {
			return &pb.SyncupSideReply{
				AgentReply: &pb.AgentReply{
					Code: common.ReplyCodeSaveFailed, Details: "disk full",
				},
				Revision: m.heldRev, SideInfo: a.sideInfo(),
			}
		}
		m.heldRev = req.GetRevision()
		return &pb.SyncupSideReply{
			Revision: req.GetRevision(), SideInfo: a.sideInfo(),
			BmInfo: &pb.BitmapInfo{ResId: spMigrId, BmIdxList: applied},
		}
	}
	h.sides[spDnC].checkReply = func(
		req *pb.CheckSideRequest,
	) *pb.CheckSideReply {
		a.rounds.add(spDnC)
		m.mu.Lock()
		held := m.heldRev
		m.mu.Unlock()
		return &pb.CheckSideReply{Revision: held, SideInfo: a.sideInfo()}
	}
	for _, stub := range h.cntlrs {
		stub.legBm = func(*pb.GetLegBmRequest) (*pb.GetLegBmReply, error) {
			m.mu.Lock()
			hold := m.hold
			m.mu.Unlock()
			if hold != nil {
				<-hold
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.failReads > 0 {
				m.failReads--
				code := m.failCode
				if code == codes.OK {
					code = codes.Unavailable
				}
				return nil, status.Error(code, "the read failed")
			}
			return &pb.GetLegBmReply{Bitmap: slices.Clone(m.legBm)}, nil
		}
	}
}

// smallPieces cuts a migration's bitmap into pieces of n bytes for the test.
func smallPieces(t *testing.T, n int) {
	t.Helper()
	old := migrPieceBytes
	migrPieceBytes = n
	t.Cleanup(func() { migrPieceBytes = old })
}

// migrBmHarness is the fixture with the migration's cutover as pending, the
// dst side reporting info, the primary a live path to the dst side, and the
// primary's CN answering m.
func migrBmHarness(
	t *testing.T,
	pending bool,
	info *pb.SideInfo,
	m *migrBmAgents,
) (*spHarness, *cutoverAgents) {
	t.Helper()
	h := newSpHarness(t)
	h.addFixtureAgents()
	h.ops.setState(migrBmFixture(pending))
	agents := newCutoverAgents()
	agents.setSide(info)
	agents.setPaths(spCnA, pathInfo(spSideSrc, spSideDst))
	serveMigrBm(h, agents, m)
	return h, agents
}

// TestMigrBmReadWaitsForItsTrigger pins BM7's trigger: the dst side's child
// reads the bitmap only when the migration's cutover_pending is clear, its
// last reply is at the desired revision and shows the dm-clone, and the
// primary reports a live path to it. Each case drops one condition and reads
// nothing over three rounds; the first holds them all.
func TestMigrBmReadWaitsForItsTrigger(t *testing.T) {
	notShown := cloneShownInfo()
	notShown.MigrDstInfo.DmCloneInfo = resStatus("dm-clone",
		pb.ResStatus_RES_STATUS_PROVISIONING)
	noStatusLine := cloneShownInfo()
	noStatusLine.MigrDstInfo.DmCloneInfo.Details = ""
	cases := []struct {
		name    string
		pending bool
		info    *pb.SideInfo
		// standbyOnly moves the live path from the primary to a standby.
		standbyOnly bool
		// saveFails keeps the dst agent behind the desired revision.
		saveFails bool
		wantRead  bool
	}{
		{"every condition holds", false, cloneShownInfo(), false, false, true},
		{"the cutover pending", true, cloneShownInfo(), false, false, false},
		{"the dm-clone row not OK", false, notShown, false, false, false},
		{"an OK dm-clone row with no status line", false, noStatusLine,
			false, false, false},
		{"a standby's live path only", false, cloneShownInfo(), true, false,
			false},
		{"the dst agent behind the desired revision", false,
			cloneShownInfo(), false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, inBubble(func(t *testing.T) {
			m := &migrBmAgents{legBm: []byte{0xff}, saveFails: tc.saveFails}
			h, agents := migrBmHarness(t, tc.pending, tc.info, m)
			if tc.standbyOnly {
				agents.setPaths(spCnA, pathInfo(spSideSrc))
				agents.setPaths(spCnB, pathInfo(spSideSrc, spSideDst))
			}
			h.start()
			if tc.wantRead {
				h.advanceUntil("the read", roundInterval, func() bool {
					return len(h.cntlrs[spCnA].legBmCalls()) > 0
				})
				return
			}
			h.advanceUntil("three rounds", roundInterval, func() bool {
				return agents.rounds.atLeast(3, spCnA, spCnB, spDnC)
			})
			for _, addr := range []string{spCnA, spCnB, spCnC} {
				if n := len(h.cntlrs[addr].legBmCalls()); n != 0 {
					t.Fatalf("%d GetLegBm calls to %s", n, addr)
				}
			}
		}))
	}
}

// TestMigrBmPiecesReachTheDestination pins BM7 end to end: one GetLegBm of
// the whole leg to the primary's CN, the bitmap cut into pieces, and every
// piece pushed right after the read, in order, whatever the dst agent's
// applied set lists (here piece 0), to the dst side's DN and to no other
// (BM4). Later rounds read nothing again and push nothing again; a later
// SyncupSide reply whose applied set lacks piece 1 pushes it again from the
// child's own read, and piece 2 after it, which the agent takes out of its
// set when it saves piece 1 (BM2, BM6; dnagent.md DN15).
func TestMigrBmPiecesReachTheDestination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{
			applied: []uint32{0},
			legBm:   []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		w := h.start()
		dst := h.sides[spDnC]
		h.advanceUntil("three pushes and three rounds", roundInterval,
			func() bool {
				return len(dst.pushes()) >= 3 &&
					agents.rounds.atLeast(3, spCnA, spDnC)
			})

		reads := h.cntlrs[spCnA].legBmCalls()
		wantReq := &pb.GetLegBmRequest{
			ClusterId: testCid, CnId: spCnIdA, SpId: testSpId,
			CntlrId: spCntlrPrimary, LegId: spLegMigr,
		}
		if len(reads) != 1 || !proto.Equal(reads[0], wantReq) {
			t.Fatalf("GetLegBm calls = %v, want one %v", reads, wantReq)
		}
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 2)
		for _, addr := range []string{spDnA, spDnB, spDnD} {
			if n := len(h.sides[addr].pushes()); n != 0 {
				t.Fatalf("%d pushes to %s, not the dst side's DN", n, addr)
			}
		}
		read := h.logs.withMsg(msgMigrBitmapRead)
		if len(read) != 1 || read[0]["piece_cnt"] != float64(3) ||
			read[0]["leg_id"] != float64(spLegMigr) {
			t.Fatalf("%s records = %v, want one of 3 pieces",
				msgMigrBitmapRead, read)
		}

		// A bump brings a SyncupSide whose applied set lacks piece 1.
		m.setApplied(0, 2)
		before := len(dst.syncups())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the re-push of pieces 1 and 2", roundInterval,
			func() bool {
				return len(dst.syncups()) > before && len(dst.pushes()) >= 5
			})
		wantPieces(t, dst.pushes(), all, 0, 1, 2, 1, 2)
		if n := len(h.cntlrs[spCnA].legBmCalls()); n != 1 {
			t.Fatalf("%d GetLegBm calls, want the one read", n)
		}
	})
}

// wantPieces fails unless pushes are, in order, the pieces at idxs of the
// fixture's migration, addressed to the dst side, each carrying its bytes.
func wantPieces(
	t *testing.T,
	pushes []*pb.PushMigrBitmapRequest,
	bytes map[uint32][]byte,
	idxs ...uint32,
) {
	t.Helper()
	if len(pushes) != len(idxs) {
		t.Fatalf("%d pushes, want %d: %v", len(pushes), len(idxs), pushes)
	}
	for i, push := range pushes {
		if push.GetMigrId() != spMigrId || push.GetBmIdx() != idxs[i] ||
			push.GetDnId() != spDnIdC ||
			push.GetSidePointer().GetSideId() != spSideDst ||
			!slices.Equal(push.GetBitmap(), bytes[idxs[i]]) {
			t.Fatalf("push %d = %v, want piece %d %v of migration %d to "+
				"side %d", i, push, idxs[i], bytes[idxs[i]], spMigrId,
				spSideDst)
		}
	}
}

// TestMigrBmRestartedChildPushesEveryPiece: the pieces live in the child's
// memory only, so a new child reads the bitmap again and pushes every piece
// again, which the dn agent takes without applying a piece it holds byte
// for byte (dnagent.md DN15): with its agent at the desired revision it
// meets no SyncupSide, and with an agent restarted it meets one whose
// applied set lists pieces it has not pushed itself, which may be an older
// build's chunks (BM2, BM7).
func TestMigrBmRestartedChildPushesEveryPiece(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
		h, _ := migrBmHarness(t, false, cloneShownInfo(), m)
		w := h.start()
		dst := h.sides[spDnC]
		h.advanceUntil("three pushes", roundInterval, func() bool {
			return len(dst.pushes()) >= 3
		})
		w.stop()
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 2)
		syncups := len(dst.syncups())

		// The agent stayed at its revision: no SyncupSide, every piece.
		m.setApplied(0, 1, 2)
		w = h.start()
		h.advanceUntil("a second read and three pushes", roundInterval,
			func() bool {
				return len(h.cntlrs[spCnA].legBmCalls()) >= 2 &&
					len(dst.pushes()) >= 6
			})
		w.stop()
		wantPieces(t, dst.pushes(), all, 0, 1, 2, 0, 1, 2)
		if n := len(dst.syncups()); n != syncups {
			t.Fatalf("%d SyncupSides of an agent at its revision", n-syncups)
		}

		// The agent restarted with pieces 0 and 2: a SyncupSide gives the
		// set, which holds back none of the pieces.
		m.setApplied(0, 2)
		m.restartDst()
		h.start()
		h.advanceUntil("a third read and three pushes", roundInterval,
			func() bool {
				return len(h.cntlrs[spCnA].legBmCalls()) >= 3 &&
					len(dst.pushes()) >= 9
			})
		wantPieces(t, dst.pushes(), all, 0, 1, 2, 0, 1, 2, 0, 1, 2)
		if n := len(dst.syncups()); n != syncups+1 {
			t.Fatalf("%d SyncupSides of the restarted agent, want 1",
				n-syncups)
		}
	})
}

// TestMigrBmAnOlderAppliedSetHoldsBackNoPiece: after an upgrade the dst
// agent's applied set lists the chunks an older build left, at the indexes
// the child's pieces take. Right after its read the child pushes every
// piece all the same; a later SyncupSide reply that lists every piece the
// child delivered pushes none again (BM2, BM7).
func TestMigrBmAnOlderAppliedSetHoldsBackNoPiece(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{
			applied: []uint32{0, 1, 2, 3},
			legBm:   []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		w := h.start()
		dst := h.sides[spDnC]
		h.advanceUntil("the read and three rounds", roundInterval,
			func() bool {
				return len(h.cntlrs[spCnA].legBmCalls()) >= 1 &&
					agents.rounds.atLeast(3, spCnA, spDnC)
			})
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 2)

		m.setApplied(0, 1, 2)
		before := len(dst.syncups())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the SyncupSide and three more rounds",
			roundInterval, func() bool {
				return len(dst.syncups()) > before &&
					agents.rounds.atLeast(6, spCnA, spDnC)
			})
		wantPieces(t, dst.pushes(), all, 0, 1, 2)
	})
}

// TestMigrBmAFailedPushIsFollowedByEveryLaterPiece: a push of piece 1 that
// fails ends the plan; the next offer pushes piece 1 again and every piece
// after it, though the applied set, an older build's, lists them all: a
// piece counts as delivered only once this child pushed it with a code-zero
// reply (BM2, BM6, BM7).
func TestMigrBmAFailedPushIsFollowedByEveryLaterPiece(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{
			applied: []uint32{0, 1, 2},
			legBm:   []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		dst := h.sides[spDnC]
		dst.mu.Lock()
		dst.refuse = map[uint32]int{1: 1}
		dst.mu.Unlock()
		h.start()
		h.advanceUntil("the read and three rounds", roundInterval,
			func() bool {
				return len(h.cntlrs[spCnA].legBmCalls()) >= 1 &&
					agents.rounds.atLeast(3, spCnA, spDnC)
			})
		syncups := len(dst.syncups())
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(base+3, spCnA)
		})
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 1, 2)
		if n := len(dst.syncups()); n != syncups {
			t.Fatalf("%d SyncupSides re-planned the push, want none: the "+
				"offer must", n-syncups)
		}
	})
}

// TestMigrBmARePushForgetsThePiecesAboveIt: a SyncupSide reply whose applied
// set lacks piece 1 pushes pieces 1 and 2 again. The agent takes piece 2 out
// of its set when it saves piece 1 (dnagent.md DN15), so the push of piece
// 1 makes the child forget it delivered piece 2, whatever its reply
// (forgetFrom); when the push of piece 2 then fails, the next offer pushes
// it again, with no SyncupSide in between (BM5, BM6, BM7).
func TestMigrBmARePushForgetsThePiecesAboveIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		w := h.start()
		dst := h.sides[spDnC]
		h.advanceUntil("three pushes", roundInterval, func() bool {
			return len(dst.pushes()) >= 3 &&
				agents.rounds.atLeast(3, spCnA, spDnC)
		})
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 2)

		dst.mu.Lock()
		dst.refuse = map[uint32]int{2: 1}
		dst.mu.Unlock()
		m.setApplied(0, 2)
		before := len(dst.syncups())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the SyncupSide and the re-push", roundInterval,
			func() bool {
				return len(dst.syncups()) > before && len(dst.pushes()) >= 5
			})
		syncups := len(dst.syncups())
		h.advanceUntil("piece 2 pushed again", roundInterval, func() bool {
			return len(dst.pushes()) >= 6
		})
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(base+3, spCnA)
		})
		wantPieces(t, dst.pushes(), all, 0, 1, 2, 1, 2, 2)
		if n := len(dst.syncups()); n != syncups {
			t.Fatalf("%d SyncupSides re-planned the push, want none: the "+
				"offer must", n-syncups)
		}
	})
}

// TestMigrBmARefusedRePushIsOfferedAgain: a SyncupSide reply whose applied
// set lacks pieces 1 and 2, which the child delivered, pushes piece 1 again,
// and the agent refuses that push. The push takes pieces 1 and 2 out of the
// child's memo whatever its reply, so the next offer pushes both again, with
// no SyncupSide in between (BM5, BM6, BM7).
func TestMigrBmARefusedRePushIsOfferedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		w := h.start()
		dst := h.sides[spDnC]
		h.advanceUntil("three pushes", roundInterval, func() bool {
			return len(dst.pushes()) >= 3 &&
				agents.rounds.atLeast(3, spCnA, spDnC)
		})
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 2)

		dst.mu.Lock()
		dst.refuse = map[uint32]int{1: 1}
		dst.mu.Unlock()
		m.setApplied(0)
		before := len(dst.syncups())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the SyncupSide and the refused re-push",
			roundInterval, func() bool {
				return len(dst.syncups()) > before && len(dst.pushes()) >= 4
			})
		syncups := len(dst.syncups())
		h.advanceUntil("pieces 1 and 2 pushed again", roundInterval,
			func() bool { return len(dst.pushes()) >= 6 })
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(base+3, spCnA)
		})
		wantPieces(t, dst.pushes(), all, 0, 1, 2, 1, 1, 2)
		if n := len(dst.syncups()); n != syncups {
			t.Fatalf("%d SyncupSides re-planned the push, want none: the "+
				"offer must", n-syncups)
		}
	})
}

// TestMigrBmAReplyPushesWhatTheMemoLacks: the agent refuses the push of
// piece 1, and then the primary shows no live path to the dst side, so no
// offer comes. A SyncupSide reply whose applied set, an older build's, lists
// every piece pushes piece 1 again and piece 2 after it: the reply's plan
// starts at the first piece the memo of BM5 does not list, though the set
// lists it (BM2).
func TestMigrBmAReplyPushesWhatTheMemoLacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{
			applied: []uint32{0, 1, 2},
			legBm:   []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		dst := h.sides[spDnC]
		hold := make(chan struct{})
		dst.mu.Lock()
		dst.refuse = map[uint32]int{1: 1}
		dst.pushHold = hold
		dst.mu.Unlock()
		w := h.start()
		h.advanceUntil("the first push held", roundInterval, func() bool {
			return len(dst.pushes()) >= 1
		})
		agents.setPaths(spCnA, pathInfo(spSideSrc))
		dst.mu.Lock()
		dst.pushHold = nil
		dst.mu.Unlock()
		close(hold)
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three rounds with no path", roundInterval,
			func() bool { return agents.rounds.atLeast(base+3, spCnA) })
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1)

		before := len(dst.syncups())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the SyncupSide and three more rounds",
			roundInterval, func() bool {
				return len(dst.syncups()) > before &&
					agents.rounds.atLeast(base+6, spCnA)
			})
		wantPieces(t, dst.pushes(), all, 0, 1, 1, 2)
	})
}

// TestMigrBmAnOfferWaitsForTheRunningPlan: the offers made while a plan of
// the migration runs plan nothing, though the pieces of that plan are not
// in the child's memo while it runs: the plan pushes every piece to the
// last, and only an offer after it ends plans from what it left (BM7).
func TestMigrBmAnOfferWaitsForTheRunningPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		dst := h.sides[spDnC]
		hold := make(chan struct{})
		dst.mu.Lock()
		dst.pushHold = hold
		dst.mu.Unlock()
		h.start()
		h.advanceUntil("the first push held", roundInterval, func() bool {
			return len(dst.pushes()) >= 1
		})
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three rounds with the push held", roundInterval,
			func() bool { return agents.rounds.atLeast(base+3, spCnA) })
		dst.mu.Lock()
		dst.pushHold = nil
		dst.mu.Unlock()
		close(hold)
		base = agents.rounds.get(spCnA)
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(base+3, spCnA)
		})
		all := map[uint32][]byte{0: {1, 2, 3, 4}, 1: {5, 6, 7, 8}, 2: {9, 10}}
		wantPieces(t, dst.pushes(), all, 0, 1, 2)
	})
}

// TestMigrBmFailedReadIsRetried: a GetLegBm that fails leaves the child no
// bitmap and logs `migration bitmap read failed` (dnv-worker.md, Log
// records); a later round reads again. A call that finds the agent
// unreachable counts for nothing; after migrBmReadTries reads the agent
// refused, or one bitmap of more than MaxMigrBmCnt pieces, the child gives
// the bitmap up: it reads no more and pushes nothing (BM7).
func TestMigrBmFailedReadIsRetried(t *testing.T) {
	t.Run("unreachable, not counted", inBubble(func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2},
			failReads: migrBmReadTries + 1}
		h, _ := migrBmHarness(t, false, cloneShownInfo(), m)
		h.start()
		dst := h.sides[spDnC]
		h.advanceUntil("the push after the failed reads", roundInterval,
			func() bool { return len(dst.pushes()) >= 1 })
		if n := len(h.cntlrs[spCnA].legBmCalls()); n != migrBmReadTries+2 {
			t.Fatalf("%d GetLegBm calls, want the %d failed ones and one more",
				n, migrBmReadTries+1)
		}
		failed := h.logs.withMsg(msgMigrBitmapReadFailed)
		if len(failed) != migrBmReadTries+1 || failed[0]["error"] == nil ||
			failed[len(failed)-1]["given_up"] != false {
			t.Fatalf("%s records = %v, want %d with their errors, none "+
				"giving up", msgMigrBitmapReadFailed, failed,
				migrBmReadTries+1)
		}
		wantPieces(t, dst.pushes(), map[uint32][]byte{0: {1, 2}}, 0)
	}))
	t.Run("refused by the agent, given up", inBubble(func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2}, failReads: migrBmReadTries,
			failCode: codes.Internal}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		h.start()
		h.advanceUntil("the reads given up", roundInterval, func() bool {
			return len(h.logs.withMsg(msgMigrBitmapReadFailed)) >=
				migrBmReadTries
		})
		calls := len(h.cntlrs[spCnA].legBmCalls())
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(calls+3, spCnA)
		})
		if n := len(h.cntlrs[spCnA].legBmCalls()); n != migrBmReadTries {
			t.Fatalf("%d GetLegBm calls, want the %d refused ones and no "+
				"more", n, migrBmReadTries)
		}
		failed := h.logs.withMsg(msgMigrBitmapReadFailed)
		if failed[len(failed)-1]["given_up"] != true ||
			failed[0]["given_up"] != false {
			t.Fatalf("%s records = %v, want the last one giving up",
				msgMigrBitmapReadFailed, failed)
		}
		if n := len(h.sides[spDnC].pushes()); n != 0 {
			t.Fatalf("%d pushes of a bitmap given up", n)
		}
	}))
	t.Run("too many pieces, given up", inBubble(func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: make([]byte, 4*common.MaxMigrBmCnt+1)}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		h.start()
		h.advanceUntil("the failed read", roundInterval, func() bool {
			return len(h.logs.withMsg(msgMigrBitmapReadFailed)) >= 1
		})
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(4, spCnA)
		})
		failed := h.logs.withMsg(msgMigrBitmapReadFailed)
		if n := len(h.cntlrs[spCnA].legBmCalls()); n != 1 ||
			len(failed) != 1 || failed[0]["given_up"] != true {
			t.Fatalf("%d GetLegBm calls and %s records %v, want one read "+
				"given up", n, msgMigrBitmapReadFailed, failed)
		}
		if n := len(h.sides[spDnC].pushes()); n != 0 {
			t.Fatalf("%d pushes of a bitmap the read refused", n)
		}
	}))
	t.Run("MaxMigrBmCnt pieces, pushed whole", inBubble(func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: make([]byte, 4*common.MaxMigrBmCnt)}
		h, _ := migrBmHarness(t, false, cloneShownInfo(), m)
		h.start()
		h.advanceUntil("every piece pushed", roundInterval, func() bool {
			return len(h.sides[spDnC].pushes()) >= common.MaxMigrBmCnt
		})
	}))
}

// TestMigrBmEmptyBitmapIsReadOnce: an empty leg bitmap is a read that
// ended, with no piece to push; no later round reads it again (BM7).
func TestMigrBmEmptyBitmapIsReadOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &migrBmAgents{}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		h.start()
		h.advanceUntil("the read and four more rounds", roundInterval,
			func() bool {
				return len(h.cntlrs[spCnA].legBmCalls()) >= 1 &&
					agents.rounds.atLeast(5, spCnA)
			})
		if n := len(h.cntlrs[spCnA].legBmCalls()); n != 1 {
			t.Fatalf("%d GetLegBm calls of an empty bitmap, want 1", n)
		}
		if n := len(h.sides[spDnC].pushes()); n != 0 {
			t.Fatalf("%d pushes of an empty bitmap", n)
		}
	})
}

// TestMigrBmFailedPushIsOfferedAgain: a piece whose push failed is pushed
// again on a later offer, with no SyncupSide in between, as no SyncupSide
// need follow the read; a piece the agent acknowledged is not (BM6, BM7).
func TestMigrBmFailedPushIsOfferedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		m := &migrBmAgents{legBm: []byte{1, 2, 3, 4, 5, 6}}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		dst := h.sides[spDnC]
		dst.mu.Lock()
		dst.pushCode = common.ReplyCodeUnknownObject
		dst.mu.Unlock()
		h.start()
		h.advanceUntil("a refused push", roundInterval, func() bool {
			return len(dst.pushes()) >= 1
		})
		syncups := len(dst.syncups())
		dst.mu.Lock()
		dst.pushCode = 0
		dst.mu.Unlock()
		h.advanceUntil("both pieces acknowledged", roundInterval, func() bool {
			pushes := dst.pushes()
			return len(pushes) >= 3 && pushes[len(pushes)-1].GetBmIdx() == 1
		})
		before := len(dst.pushes())
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(base+3, spCnA)
		})
		pushes := dst.pushes()
		if pushes[0].GetBmIdx() != 0 || pushes[1].GetBmIdx() != 0 {
			t.Fatalf("pushes = %v, want piece 0 refused, then pushed again",
				pushes)
		}
		if len(pushes) != before {
			t.Fatalf("%d pushes after both pieces were acknowledged, want "+
				"none", len(pushes)-before)
		}
		if n := len(dst.syncups()); n != syncups {
			t.Fatalf("%d SyncupSides re-planned the push, want none: the "+
				"offer must", n-syncups)
		}
	})
}

// TestMigrBmGoesWithTheMigration: a read that ends after its migration left
// the plan, at its finish, keeps nothing and pushes nothing (BM7).
func TestMigrBmGoesWithTheMigration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		smallPieces(t, 4)
		hold := make(chan struct{})
		m := &migrBmAgents{legBm: []byte{1, 2}, hold: hold}
		h, agents := migrBmHarness(t, false, cloneShownInfo(), m)
		w := h.start()
		h.advanceUntil("the read under way", roundInterval, func() bool {
			return len(h.cntlrs[spCnA].legBmCalls()) >= 1
		})
		// The migration finishes: the record and the source side are gone
		// from the next load, and the dst side keeps its child as the leg's
		// one side.
		state := migrBmFixture(false)
		delete(state.Migrs, spMigrName)
		state.Conf.MigrNameList = nil
		leg := state.Slices[spSliceA].DataGrpList[0].LegList[0]
		leg.SideList = leg.SideList[1:]
		h.ops.setState(state)
		dst := h.sides[spDnC]
		before := len(dst.syncups())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the dst side synced without its migration",
			roundInterval, func() bool {
				for _, req := range dst.syncups()[before:] {
					if req.GetMigrDstConf() == nil {
						return true
					}
				}
				return false
			})
		close(hold)
		h.advanceUntil("the read's end", roundInterval, func() bool {
			return len(h.logs.withMsg(msgMigrBitmapRead)) >= 1
		})
		base := agents.rounds.get(spCnA)
		h.advanceUntil("three more rounds", roundInterval, func() bool {
			return agents.rounds.atLeast(base+3, spCnA)
		})
		if n := len(dst.pushes()); n != 0 {
			t.Fatalf("%d pushes of a migration that left the plan", n)
		}
	})
}

// get is the rounds one endpoint answered so far.
func (c *cutoverRounds) get(addr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[addr]
}
