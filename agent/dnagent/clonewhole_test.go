package dnagent

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAWholeDestinationReadsWithTheFlush pins SH17 on a migration
// destination: a dm-clone status read asks for no flush, and one that finds
// the copy whole reads once more with the flush, whose line the
// dm_clone_info row carries, in the converge and in a check round alike; a
// flushing read that fails gives the row ERROR, so no whole copy is reported
// from a read whose metadata was not committed.
func TestAWholeDestinationReadsWithTheFlush(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	names := newMigrDstNames()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	waits := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	waits.MigrDstConf.CutoverPending = true
	if _, err := srv.SyncupSide(ctx, waits); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	req := migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmPresent(node, names.clone) {
		t.Fatal("fixture is wrong: the cutover built no dm-clone")
	}
	noFlush := "cmd dmsetup status --noflush " + names.clone
	flush := "cmd dmsetup status " + names.clone
	// reads counts the pass's no-flush reads and its flushing reads of the
	// dm-clone's status since the last Reset.
	reads := func() (int, int) {
		return len(node.callsMatching(noFlush)),
			len(node.callsMatching(flush))
	}

	// A copy that is not whole: no-flush reads alone.
	node.Reset()
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if nf, f := reads(); nf == 0 || f != 0 {
		t.Fatalf("a converge over a copy that is not whole read %d "+
			"no-flush and %d flushing statuses, want some and none", nf, f)
	}

	// A whole copy: each no-flush read that finds it whole is followed by
	// one flushing read, and the row carries a whole line.
	node.setCloneHydrated(names.clone, 1<<40)
	for _, channel := range []string{"converge", "check round"} {
		node.Reset()
		var row *pb.ResInfo
		if channel == "converge" {
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			row = reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo()
		} else {
			row = checkRoundInfo(t, srv, 3).GetMigrDstInfo().GetDmCloneInfo()
		}
		nf, f := reads()
		if nf == 0 || f != nf {
			t.Errorf("%s over a whole copy read %d no-flush and %d "+
				"flushing statuses, want one flushing read after each "+
				"no-flush read", channel, nf, f)
		}
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			!common.HydrationComplete(row.GetDetails()) {
			t.Errorf("%s: dm_clone_info = %v/%q, want OK and whole",
				channel, row.GetStatus(), row.GetDetails())
		}
	}

	// A flushing read that fails: the row reads ERROR, not whole.
	node.failCmdAlways["dmsetup status "+names.clone] = "refused"
	row := checkRoundInfo(t, srv, 3).GetMigrDstInfo().GetDmCloneInfo()
	delete(node.failCmdAlways, "dmsetup status "+names.clone)
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		common.HydrationComplete(row.GetDetails()) {
		t.Errorf("a failed flushing read: dm_clone_info = %v/%q, want "+
			"ERROR and not whole", row.GetStatus(), row.GetDetails())
	}
}
