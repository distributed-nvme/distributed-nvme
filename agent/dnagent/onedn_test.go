package dnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestADiskTableServesOnlyTheDiskNodeItsHeaderNames: an agent that holds
// two disk nodes — disk node A, whose disk this is, and disk node B — has
// each converge ask for its own identity, and every use of the table is made
// for one disk node. A side of A is served and a side of B refused at every
// use, whichever of the two converged last (DN5).
func TestADiskTableServesOnlyTheDiskNodeItsHeaderNames(t *testing.T) {
	ctx := context.Background()
	meta, _ := formatted(t)
	other := testDn + 1
	if _, err := meta.AllocSide(ctx, testCluster, testDn, testSp, testSide, 1,
		metaExtentSize); err != nil {
		t.Fatalf("a side of A: %v", err)
	}
	if _, err := meta.AllocCloneMeta(ctx, testCluster, testDn, testSp,
		testMigrId, 1); err != nil {
		t.Fatalf("a clone-metadata run of A: %v", err)
	}
	type use struct {
		name string
		call func(dnId uint64) error
	}
	uses := []use{
		{"AllocSide", func(dnId uint64) error {
			_, err := meta.AllocSide(ctx, testCluster, dnId, testSp,
				testSide, 1, metaExtentSize)
			return err
		}},
		{"LookupConfirmedSide", func(dnId uint64) error {
			_, _, err := meta.LookupConfirmedSide(ctx, testCluster, dnId,
				testSp, testSide)
			return err
		}},
		{"SideAllocRefusal", func(dnId uint64) error {
			return meta.SideAllocRefusal(ctx, testCluster, dnId, testSp,
				testSide, 1, metaExtentSize)
		}},
		{"SetSideZeroed", func(dnId uint64) error {
			return meta.SetSideZeroed(ctx, testCluster, dnId, testSp,
				testSide, 0, 0)
		}},
		{"AllocCloneMeta", func(dnId uint64) error {
			_, err := meta.AllocCloneMeta(ctx, testCluster, dnId, testSp,
				testMigrId, 1)
			return err
		}},
		{"HeldCloneMeta", func(dnId uint64) error {
			_, err := meta.HeldCloneMeta(ctx, testCluster, dnId, testSp,
				testMigrId, 1)
			return err
		}},
	}
	// The frees of the records A holds, refused for B and so writing
	// nothing; an absent record's free writes nothing for anyone.
	frees := []use{
		{"FreeSide", func(dnId uint64) error {
			return meta.FreeSide(ctx, testCluster, dnId, testSp, testSide)
		}},
		{"FreeCloneMeta", func(dnId uint64) error {
			return meta.FreeCloneMeta(ctx, testCluster, dnId, testSp,
				testMigrId)
		}},
	}
	for _, last := range []uint64{other, testDn} {
		// Both converge; last is the one that converges last.
		first := testDn
		if last == testDn {
			first = other
		}
		for _, dnId := range []uint64{first, last} {
			_ = meta.EnsureFormatted(ctx, testCluster, dnId, metaExtentSize,
				mayFormat)
		}
		for _, u := range uses {
			if err := u.call(testDn); err != nil {
				t.Errorf("%s for A, %d converged last: %v", u.name, last, err)
			}
		}
		for _, u := range append(uses, frees...) {
			if err := u.call(other); err == nil ||
				!strings.Contains(err.Error(), "foreign disk") {
				t.Errorf("%s for B, %d converged last = %v, want the "+
					"foreign disk", u.name, last, err)
			}
		}
		if c, n, ok := meta.Identity(); !ok || c != testCluster || n != testDn {
			t.Errorf("Identity, %d converged last = %d/%d %v, want A",
				last, c, n, ok)
		}
		if meta.Counts(testCluster, other, nil) != nil {
			t.Errorf("Counts for B, %d converged last, answered", last)
		}
	}
	for _, u := range frees {
		if err := u.call(testDn); err != nil {
			t.Errorf("%s for A: %v", u.name, err)
		}
	}
}

// TestASecondDiskNodeReadsItsDiskAsForeign: an agent that holds disk node
// A, whose disk this is, takes a SyncupDn for disk node B, an operator's
// mistake; B's converge reads the disk as foreign, a meta row in error that
// marks B unhealthy in its cluster, and a side of B is refused the table,
// while a side of A is served after B's converge as before it (DN5).
func TestASecondDiskNodeReadsItsDiskAsForeign(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	if reply, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn of A: %v, %v", reply.GetAgentReply(), err)
	}
	provisionSide(t, srv, 1, testSide)
	second := dnReq(1, testSide)
	second.DnId = testDn + 1
	reply, err := srv.SyncupDn(ctx, second)
	if err != nil {
		t.Fatalf("SyncupDn of B: %v", err)
	}
	meta := reply.GetDnInfo().GetMetaInfo()
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "foreign disk") {
		t.Fatalf("B's meta row is %v, want the foreign disk", meta)
	}
	sideOfB := sideReq(2, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	sideOfB.DnId = testDn + 1
	reply2, err := srv.SyncupSide(ctx, sideOfB)
	if err != nil {
		t.Fatalf("SyncupSide of B: %v", err)
	}
	if row := reply2.GetSideInfo().GetSideDevInfo(); row.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), "foreign disk") {
		t.Fatalf("B's side device row is %v, want the foreign disk", row)
	}
	reply3, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide of A: %v", err)
	}
	if row := reply3.GetSideInfo().GetSideDevInfo(); row.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("A's side device row after B's converge is %v", row)
	}
}
