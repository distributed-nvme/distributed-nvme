package dnagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The store row (DN18; agent.Store.Refusal, agent.Store.Probe)
// ---------------------------------------------------------------------------

// storeDisk is the disk of a local store that a test turns: while err is
// set every save fails with it, as a full disk fails them (a disk error) or
// as the soft timeout cuts them off (a ctx error), and every save's path is
// recorded.
type storeDisk struct {
	mu     sync.Mutex
	err    error
	writes []string
}

// useStoreDisk puts a new store over d on srv; the store's states start
// clear.
func useStoreDisk(srv *DnAgentServer, node *fakeNode) *storeDisk {
	d := &storeDisk{}
	oc := node.osClient()
	write := oc.WriteProtoFn
	oc.WriteProtoFn = func(
		ctx context.Context, p string, msg proto.Message,
	) error {
		d.mu.Lock()
		d.writes = append(d.writes, p)
		err := d.err
		d.mu.Unlock()
		if err != nil {
			return err
		}
		return write(ctx, p, msg)
	}
	srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
	return d
}

func (d *storeDisk) fail(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

// count is the number of saves to path so far.
func (d *storeDisk) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, p := range d.writes {
		if p == path {
			n++
		}
	}
	return n
}

// assertStoreRow checks a store row: its name, the store's directory, its
// status, and, for an error row, that its details carry text.
func assertStoreRow(
	t *testing.T,
	label string,
	row *pb.ResInfo,
	want pb.ResStatus,
	text string,
) {
	t.Helper()
	if row == nil {
		t.Errorf("%s: no store row", label)
		return
	}
	if row.GetResName() != common.DefaultLocalStorPrefix {
		t.Errorf("%s: the store row names %q, want the store's directory "+
			"%q", label, row.GetResName(), common.DefaultLocalStorPrefix)
	}
	if row.GetStatus() != want {
		t.Errorf("%s: the store row is %v (%q), want %v", label,
			row.GetStatus(), row.GetDetails(), want)
	}
	if !strings.Contains(row.GetDetails(), text) {
		t.Errorf("%s: the store row's details %q do not carry %q", label,
			row.GetDetails(), text)
	}
}

// TestTheStoreRowFollowsTheStoreDisk: the DN's store row reads OK after a
// converge and on a read while the store's disk takes writes, and the read
// writes no probe file then. A request refused because the disk refused its
// save, the DN's or a side's, turns the row ERROR on the next read, with the
// save's error; each read while it is ERROR saves the probe file once, and a
// refused probe keeps the row ERROR, its text refreshed and its epoch
// unmoved. A probe that holds turns the row OK in the same read with no
// syncup, and leaves the latest save failed; the reads after it write no
// probe. A save cut off by its ctx leaves the row OK. A SyncupDn whose save
// holds reads OK on its converge.
func TestTheStoreRowFollowsTheStoreDisk(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	d := useStoreDisk(srv, node)
	probe := srv.nf.LocalDnProbePath(testCluster, testDn)
	noSpaceErr := fmt.Errorf("write %s: %s",
		srv.nf.LocalDnPath(testCluster, testDn), noSpace)
	const ioText = "input/output error"
	ioErr := errors.New("write probe: " + ioText)
	ok, bad := pb.ResStatus_RES_STATUS_OK, pb.ResStatus_RES_STATUS_ERROR
	readRow := func() *pb.ResInfo {
		readDnNow(ctx, srv)
		return dnRound(srv, "round").GetDnInfo().GetStoreInfo()
	}

	assertStoreRow(t, "a read of a healthy store", readRow(), ok, "")
	readRow()
	if n := d.count(probe); n != 0 {
		t.Fatalf("%d probe writes on a healthy store, want none", n)
	}

	// The DN's own request, refused for a disk error.
	d.fail(noSpaceErr)
	reply, err := srv.SyncupDn(ctx, dnReq(2, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 1)
	first := readRow()
	assertStoreRow(t, "the read after the refusal", first, bad, noSpace)
	if n := d.count(probe); n != 1 {
		t.Errorf("%d probe writes after one read, want 1", n)
	}
	d.fail(ioErr)
	second := readRow()
	assertStoreRow(t, "a read whose probe the disk refused", second, bad,
		ioText)
	if second.GetEpoch() != first.GetEpoch() {
		t.Errorf("a refused probe moved the row's epoch from %d to %d",
			first.GetEpoch(), second.GetEpoch())
	}
	if n := d.count(probe); n != 2 {
		t.Errorf("%d probe writes after two reads, want 2", n)
	}

	// The disk takes writes again: the next read's probe heals the row.
	d.fail(nil)
	assertStoreRow(t, "the read whose probe held", readRow(), ok, "")
	// The probe clears the refusal alone: the latest save failed until a
	// save of a request or a chunk holds (dnagent.md SH30).
	if !srv.store.SaveFailing() {
		t.Error("the probe that held cleared the latest save failed")
	}
	node.mu.Lock()
	_, left := node.protos[probe]
	node.mu.Unlock()
	if left {
		t.Error("the probe that held left its file in the store")
	}
	readRow()
	if n := d.count(probe); n != 3 {
		t.Errorf("%d probe writes, want 3: none after the healing one", n)
	}

	// A save cut off by its ctx is no refusal of the disk.
	d.fail(fmt.Errorf("write: %w", context.DeadlineExceeded))
	reply, err = srv.SyncupDn(ctx, dnReq(2, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeSaveFailed {
		t.Errorf("the cut-off save answered code %d, want a refusal",
			reply.GetAgentReply().GetCode())
	}
	assertStoreRow(t, "the read after a cut-off save", readRow(), ok, "")
	if n := d.count(probe); n != 3 {
		t.Errorf("%d probe writes, want 3: none after a cut-off save", n)
	}

	// A side's request, refused for a disk error, marks the DN's row.
	d.fail(noSpaceErr)
	sideReply, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if sideReply.GetAgentReply().GetCode() != common.ReplyCodeSaveFailed {
		t.Errorf("the side's refused save answered code %d, want a "+
			"refusal", sideReply.GetAgentReply().GetCode())
	}
	assertStoreRow(t, "the read after a side's refusal", readRow(), bad,
		noSpace)

	// A SyncupDn whose save holds: its converge reads OK.
	d.fail(nil)
	reply, err = srv.SyncupDn(ctx, dnReq(2, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn answered %v", reply.GetAgentReply())
	}
	assertStoreRow(t, "the converge after a save that held",
		reply.GetDnInfo().GetStoreInfo(), ok, "")
}

// TestReconcileRemovesTheProbeFiles: the startup listing lists the probe
// kind (dnagent.md SH6) so that an interrupted probe write goes with the
// other temp leftovers, and a committed probe file a probe did not get to
// remove goes too: nothing reads either, and both hold room the store may
// need.
func TestReconcileRemovesTheProbeFiles(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	probe := srv.nf.LocalDnProbePath(testCluster, testDn)
	stray := probe + common.AtomicWriteTmpInfix + "1234567890"
	for _, path := range []string{probe, stray} {
		if err := node.writeProto(ctx, path,
			wrapperspb.Bytes([]byte("probe"))); err != nil {
			t.Fatalf("planting %s: %v", path, err)
		}
	}
	node.Reset()

	startTestServer(t, node)

	if node.hasCall("readproto " + probe) {
		t.Errorf("the restart read the probe file")
	}
	if node.hasCall("readproto " + stray) {
		t.Errorf("the restart read the probe's temp file")
	}
	node.mu.Lock()
	_, kept := node.protos[probe]
	_, left := node.protos[stray]
	node.mu.Unlock()
	if kept {
		t.Errorf("the committed probe file survived the restart")
	}
	if left {
		t.Errorf("the probe's temp file survived the restart")
	}
}
