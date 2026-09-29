package dnagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// §6.23 — a zero conf member is refused (DN4, §2.1)
// ---------------------------------------------------------------------------
//
// extent_size is what this disk's header was formatted with and what every
// side's run is carved out of, so a zero is refused rather than replaced with
// a constant the rest of the cluster does not share. The refusal sits after
// the revision gate and before the request becomes desired state, so nothing
// is converged, the disk is not even read for identity, and no local-store
// file is touched — a zero cannot be replayed by the next Reconcile either.
//
// The message is asserted verbatim and is the same literal
// model/capacity_test.go asserts of model.ValidateClusterConf, which is what
// keeps the two deliberate copies of the rule in step (§2.1).

const msgNoExtentSize = "invalid stored conf: dn_bin_conf.extent_size is zero"

// logCapture reads back the records the dn agent emits for itself. The buffer
// is mutex-guarded because slog.Default is process-wide and the §9.4 zeroing
// goroutines may be logging.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// msgRecords decodes every captured JSON line carrying the given msg; every
// other record is ignored, so an unrelated goroutine cannot fail these tests.
func (c *logCapture) msgRecords(t *testing.T, msg string) []map[string]any {
	t.Helper()
	c.mu.Lock()
	text := c.buf.String()
	c.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		rec := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// captureLogs installs a capturing logger as the process default for the
// duration of the test, using the production handler chain (log.md R2) so the
// trace id lands on the record exactly as it does in the agent.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	logger := slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(capture, nil),
	})
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return capture
}

// assertRefusalRecord is the one Error record §7 asks for: msg
// msgInvalidStoredConf carrying the validator's own text and the ids that
// name what an operator has to go look at.
func assertRefusalRecord(
	t *testing.T,
	capture *logCapture,
	wantKeys ...string,
) {
	t.Helper()
	recs := capture.msgRecords(t, msgInvalidStoredConf)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1", len(recs), msgInvalidStoredConf)
	}
	rec := recs[0]
	if rec["error"] != msgNoExtentSize {
		t.Errorf("record error %v, want %q", rec["error"], msgNoExtentSize)
	}
	if rec["level"] != "ERROR" {
		t.Errorf("record level %v, want ERROR", rec["level"])
	}
	for _, key := range wantKeys {
		if _, ok := rec[key]; !ok {
			t.Errorf("the refusal record names no %s", key)
		}
	}
}

// diskImage renders the fake disk's bytes as the ordered log of the
// WriteBlock segments that produced them — offset, length and content hash of
// each, in write order. writeBlock is the only thing in the fake that changes
// a disk byte (corruptBlock aside, which only a test calls), so two equal
// renderings mean the image is byte-identical: same header, same volume-table
// slots, same extent area.
func diskImage(node *fakeNode, path string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	var out strings.Builder
	for _, seg := range node.blocks[path] {
		fmt.Fprintf(&out, "off=%d len=%d sha=%x\n",
			seg.off, len(seg.data), sha256.Sum256(seg.data))
	}
	return out.String()
}

// TestSyncupDnRefusesAZeroExtentSize drives the refusal from the RPC
// entrance: a DN converged and formatted at revision 2, then a revision-3
// request whose extent_size is 0.
func TestSyncupDnRefusesAZeroExtentSize(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	capture := captureLogs(t)
	if reply, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	} else if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn rejected: %v", reply.GetAgentReply())
	}
	imageBefore := diskImage(node, testDisk)
	if imageBefore == "" {
		t.Fatalf("the fixture converge formatted nothing")
	}
	path := srv.nf.LocalDnPath(testCluster, testDn)
	storedBefore := node.protos[path]
	if len(storedBefore) == 0 {
		t.Fatalf("the fixture converge persisted nothing")
	}
	node.Reset()

	req := dnReq(3)
	req.ExtentSize = 0
	reply, err := srv.SyncupDn(ctx, req)
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	// The reply: the refusal code, the validator's own text, and the STORED
	// revision — the request's 3 was never accepted.
	if reply.GetAgentReply().GetCode() != common.ReplyCodeInvalidConf {
		t.Fatalf("code %d, want %d", reply.GetAgentReply().GetCode(),
			common.ReplyCodeInvalidConf)
	}
	if reply.GetAgentReply().GetDetails() != msgNoExtentSize {
		t.Errorf("details %q, want %q", reply.GetAgentReply().GetDetails(),
			msgNoExtentSize)
	}
	if reply.GetRevision() != 2 {
		t.Errorf("reply revision %d, want the stored 2", reply.GetRevision())
	}
	if reply.GetDnInfo() != nil {
		t.Errorf("a refused request reported converge info")
	}

	// The node: nothing ran at all. Mutations() is every recorded call that
	// is not a probe, so this covers the writeblock of a header or a volume
	// table, every dmsetup and configfs write, and the local-store
	// WriteProto that a converge is followed by.
	if mutations := node.Mutations(); len(mutations) != 0 {
		t.Fatalf("a refused SyncupDn mutated:\n%s",
			strings.Join(mutations, "\n"))
	}
	// Not even a read: the disk is never opened for an identity check
	// against a guessed extent size, and no dmsetup or configfs probe runs
	// either — the gate sits before the converge, so this is the point with
	// literally zero side effects.
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("a refused SyncupDn touched the node:\n%s",
			strings.Join(calls, "\n"))
	}
	if got := diskImage(node, testDisk); got != imageBefore {
		t.Errorf("the disk image changed:\nbefore:\n%s\nafter:\n%s",
			imageBefore, got)
	}

	// The agent and its store: the desired state is still revision 2's, and
	// the zero was not persisted for the next Reconcile to converge.
	if string(node.protos[path]) != string(storedBefore) {
		t.Errorf("the dn state file was rewritten")
	}
	st := srv.getDn(dnKey(testCluster, testDn))
	if st == nil {
		t.Fatalf("the dn state vanished")
	}
	if st.req.GetRevision() != 2 ||
		st.req.GetExtentSize() != testExtentSize {
		t.Errorf("the refused request became desired state: revision %d, "+
			"extent_size %d", st.req.GetRevision(), st.req.GetExtentSize())
	}
	assertRefusalRecord(t, capture, "cluster_id", "dn_id")
}

// TestReconcileRefusesAZeroExtentSizeWithoutTearingSidesDown is the startup
// half of DN2's §7 refusal, and the shape of it is the whole point.
//
// The obvious implementation — skip the file — is DESTRUCTIVE: with no dn-*
// file left unread, Reconcile's side loop reads a missing DN record as "this
// side left its parent's list" and drops the state of every side of that DN,
// its local state file included. A conf fault must not delete the desired
// state. So the record is LOADED, convergeDn refuses it once, and every side
// of that DN is left exactly as the restart found it.
func TestReconcileRefusesAZeroExtentSizeWithoutTearingSidesDown(
	t *testing.T,
) {
	ctx := context.Background()
	node := newFakeNode()
	node.devSize[testDisk] = 512 << 20
	node.devNo[testDisk] = "253:0"
	node.dirs[agent.NvmetRoot] = true

	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	// The side pointer is LISTED on the DN: that is what makes this side one
	// the DN still owns, so a teardown here could only come from the conf
	// refusal itself and not from DN6's ordinary orphan rule.
	req := dnReq(2, testSide)
	req.ExtentSize = 0
	if err := node.writeProto(ctx, nf.LocalDnPath(testCluster, testDn),
		req); err != nil {
		t.Fatalf("seeding the dn state file: %v", err)
	}
	// A side of that DN, persisted by the build that formatted the disk. It
	// is what the destructive shape would have torn down.
	sidePath := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	sideRequest := sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	if err := node.writeProto(ctx, sidePath, sideRequest); err != nil {
		t.Fatalf("seeding the side state file: %v", err)
	}
	node.Reset()
	capture := captureLogs(t)

	srv := startTestServer(t, node)

	// Loaded, not dropped — this is what keeps the side's parent present.
	st := srv.getDn(dnKey(testCluster, testDn))
	if st == nil {
		t.Fatalf("the dn record was dropped; its sides are now parentless " +
			"and the next pass would tear them down")
	}
	if st.req.GetExtentSize() != 0 {
		t.Errorf("extent_size %d, want the stored 0 kept as read",
			st.req.GetExtentSize())
	}
	// Refused: nothing converged, nothing removed, nothing written.
	if mutations := node.Mutations(); len(mutations) != 0 {
		t.Fatalf("a refused dn state file still mutated the node:\n%s",
			strings.Join(mutations, "\n"))
	}
	if calls := node.callsMatching(testDisk); len(calls) != 0 {
		t.Errorf("a refused dn state file touched the disk:\n%s",
			strings.Join(calls, "\n"))
	}
	if image := diskImage(node, testDisk); image != "" {
		t.Errorf("the disk was written:\n%s", image)
	}
	// The side survives, state file and all. This is the regression guard:
	// with the refusal written as a skip, the side's state was dropped and
	// this file removed.
	if _, ok := node.protos[sidePath]; !ok {
		t.Errorf("the side state file was torn down by a conf refusal")
	}
	if srv.getSide(sideKey(testCluster, testDn, testSp, testSide)) == nil {
		t.Errorf("the side state was dropped by a conf refusal")
	}
	// Exactly one record, from convergeDn — not one per loop that notices.
	assertRefusalRecord(t, capture, "cluster_id", "dn_id")
	if _, ok := node.protos[nf.LocalDnPath(testCluster, testDn)]; !ok {
		t.Errorf("the refused dn state file was removed")
	}
}

// The accept side of the same gate, so the refusal above cannot pass by
// refusing everything: the fixture's own extent size validates and converges.
func TestSyncupDnAcceptsAConcreteExtentSize(t *testing.T) {
	srv, node := newTestServer(t)
	reply, err := srv.SyncupDn(context.Background(), dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("a concrete extent_size was refused: %v",
			reply.GetAgentReply())
	}
	if err := agent.ValidateExtentSize(dnReq(2).GetExtentSize()); err != nil {
		t.Fatalf("the fixture extent size must validate: %v", err)
	}
	if !node.hasCall(fmt.Sprintf("writeblock %s off=%d", testDisk,
		common.DnHeaderOffset)) {
		t.Fatalf("the accepted request formatted nothing:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	stored := &pb.SyncupDnRequest{}
	raw, ok := node.protos[srv.nf.LocalDnPath(testCluster, testDn)]
	if !ok {
		t.Fatalf("the accepted request was not persisted")
	}
	if err := proto.Unmarshal(raw, stored); err != nil {
		t.Fatalf("decoding the dn state file: %v", err)
	}
	if stored.GetExtentSize() != testExtentSize {
		t.Errorf("persisted extent_size %d, want %d", stored.GetExtentSize(),
			testExtentSize)
	}
}

// ---------------------------------------------------------------------------
// §6.24 — an unreadable store file deletes nothing it might own (DN2)
// ---------------------------------------------------------------------------

// newDiskNode is a fake node holding the --disk device and the nvmet root and
// nothing else: the node a restart finds when the test seeds the store itself.
func newDiskNode() *fakeNode {
	node := newFakeNode()
	node.devSize[testDisk] = 512 << 20
	node.devNo[testDisk] = "253:0"
	node.dirs[agent.NvmetRoot] = true
	return node
}

// TestReconcileKeepsTheSidesOfAnUnreadableDnFile pins the other dn-* file
// DN2 cannot use, beside §6.23's zero conf: one that does not decode. There
// is no request to load, so the DN is skipped — and the skip used to send
// every side of it down the pointer-absent branch, which deleted each side's
// state file and bitmap chunks for want of a list that could not be read. Now
// the sides are skipped with their DN: neither loaded nor deleted, the node
// left exactly as the restart found it. Nor are they left looking healthy:
// the DN and the side are unknown to the Check rounds — the side still after
// the re-sent SyncupDn — and an unknown object is what the worker re-sends
// its Syncup* for (RW4).
func TestReconcileKeepsTheSidesOfAnUnreadableDnFile(t *testing.T) {
	ctx := context.Background()
	node := newDiskNode()

	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	dnPath := nf.LocalDnPath(testCluster, testDn)
	sidePath := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	chunkPath := nf.LocalMigrBmPath(
		testCluster, testDn, testSp, testMigrId, 0)
	// A migration destination with one received chunk: the side owns a state
	// file and a bitmap chunk file, and the skip deleted both.
	if err := node.writeProto(ctx, sidePath,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("seeding the side state file: %v", err)
	}
	if err := node.writeProto(ctx, chunkPath,
		pushReq(0, testMigrId, []byte{0x05})); err != nil {
		t.Fatalf("seeding the bitmap chunk file: %v", err)
	}
	// The dn state file does not decode: a varint cut off after one byte.
	node.protos[dnPath] = []byte{0xff}
	if proto.Unmarshal(node.protos[dnPath], &pb.SyncupDnRequest{}) == nil {
		t.Fatalf("the fixture's dn state file decodes")
	}
	seeded := make(map[string][]byte, len(node.protos))
	for path, raw := range node.protos {
		seeded[path] = raw
	}
	node.Reset()

	srv := startTestServer(t, node)

	// Nothing but reads. This is the regression guard: the skip used to
	// `rm -f` the side's state file and its chunk right here.
	if mutations := node.Mutations(); len(mutations) != 0 {
		t.Fatalf("a dn state file that did not load tore its sides' "+
			"state down:\n%s", strings.Join(mutations, "\n"))
	}
	for path, raw := range seeded {
		if got, ok := node.protos[path]; !ok {
			t.Errorf("%s was removed", path)
		} else if !bytes.Equal(got, raw) {
			t.Errorf("%s was rewritten", path)
		}
	}

	// Unknown, not healthy: the reply the worker answers with a re-sync.
	checkSide := func() *pb.AgentReply {
		reply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 2,
		}, nil)
		return reply.GetAgentReply()
	}
	dnReply, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 1,
	}, nil)
	if got := dnReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Errorf("CheckDn code %d, want %d", got,
			common.ReplyCodeUnknownObject)
	}
	if got := checkSide().GetCode(); got != common.ReplyCodeUnknownObject {
		t.Errorf("CheckSide code %d, want %d", got,
			common.ReplyCodeUnknownObject)
	}

	// The re-sent SyncupDn is accepted and rewrites the file, which decodes
	// again. The side is still unknown after it: nothing loaded it, so its
	// own Check round keeps asking for the SyncupSide that rebuilds its
	// state. A side kept in memory instead would answer that round from a
	// request no converge had run since the restart.
	reply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn rejected: %v", reply.GetAgentReply())
	}
	stored := &pb.SyncupDnRequest{}
	if err := proto.Unmarshal(node.protos[dnPath], stored); err != nil {
		t.Fatalf("the re-sent SyncupDn left the dn state file "+
			"unreadable: %v", err)
	}
	if stored.GetRevision() != 1 {
		t.Errorf("dn state file revision %d, want 1", stored.GetRevision())
	}
	if got := checkSide().GetCode(); got != common.ReplyCodeUnknownObject {
		t.Errorf("CheckSide code after the SyncupDn %d, want %d", got,
			common.ReplyCodeUnknownObject)
	}
	for _, path := range []string{sidePath, chunkPath} {
		if got, ok := node.protos[path]; !ok ||
			!bytes.Equal(got, seeded[path]) {
			t.Errorf("%s did not survive the re-sent SyncupDn", path)
		}
	}
}

// TestReconcileSkipsOnlyTheSidesOfAnUnloadedDn bounds the skip above from
// both sides. A dn-* file that does not load keeps out only the sides whose DN
// is not loaded: the store's listing also returns the `.tmp-*` file an atomic
// replace leaves behind when the process dies before its rename — under the
// dn- prefix and, when half written, possibly undecodable. Beside such a file
// the real one loads, and that DN's sides must be loaded and converged as on
// any restart; skipping them too would take every side of a healthy DN off
// the agent's books until the worker re-sent it. And with no dn-* file left
// unread there is nothing to skip for: a DN with no file at all reads as a
// list that names no side, so a side of it has its request and its chunks
// deleted on the spot as before — a skip that outlived its reason would keep
// them on disk for ever.
func TestReconcileSkipsOnlyTheSidesOfAnUnloadedDn(t *testing.T) {
	t.Run("an undecodable temporary file beside a readable dn file",
		func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			stray := srv.nf.LocalDnPath(testCluster, testDn) +
				".tmp-1234567890"
			node.mu.Lock()
			node.protos[stray] = []byte{0xff}
			node.mu.Unlock()
			node.Reset()

			restarted := startTestServer(t, node)

			if !node.hasCall("readproto " + stray) {
				t.Fatalf("the stray file was never read:\n%s",
					strings.Join(node.Calls(), "\n"))
			}
			if restarted.getSide(
				sideKey(testCluster, testDn, testSp, testSide)) == nil {
				t.Fatalf("a stray dn file that did not load skipped a " +
					"side of a loaded dn")
			}
			reply, _ := restarted.checkSideRound(ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: sidePtr(testSide), Revision: 1,
			}, nil)
			if got := reply.GetAgentReply(); got.GetCode() != 0 ||
				reply.GetRevision() != 1 {
				t.Errorf("CheckSide after the restart = %v at revision "+
					"%d, want 0 at 1", got, reply.GetRevision())
			}
		})

	t.Run("no dn file at all", func(t *testing.T) {
		ctx := context.Background()
		node := newDiskNode()
		nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
		sidePath := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
		chunkPath := nf.LocalMigrBmPath(
			testCluster, testDn, testSp, testMigrId, 0)
		if err := node.writeProto(ctx, sidePath,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("seeding the side state file: %v", err)
		}
		if err := node.writeProto(ctx, chunkPath,
			pushReq(0, testMigrId, []byte{0x05})); err != nil {
			t.Fatalf("seeding the bitmap chunk file: %v", err)
		}
		node.Reset()

		startTestServer(t, node)

		if got := node.callsMatching("cmd rm -f "); len(got) != 1 ||
			!strings.Contains(got[0], " "+sidePath) ||
			!strings.Contains(got[0], " "+chunkPath) {
			t.Errorf("rm calls = %q, want one naming the side's state "+
				"file and its chunk", got)
		}
		for _, path := range []string{sidePath, chunkPath} {
			if _, ok := node.protos[path]; ok {
				t.Errorf("%s survived a restart that found no dn file",
					path)
			}
		}
	})
}

// TestReconcileKeepsTheChunksOfAnUnreadableSideFile is the same rule one
// level down. A side-* file that does not decode names no side, so a chunk
// whose side did not load may be that file's — and while the chunk's DN still
// names its side, nothing read here proves the side gone: the chunk is
// skipped, neither loaded nor deleted. It used to be deleted as an orphan, so
// a read failure of one file destroyed another. The skip is bounded both
// ways: a chunk whose DN no longer names its side is an orphan whatever
// failed to decode, and so is one whose DN has no file (with no dn-* file
// left unread); with no side-* file left unread a chunk whose side is not in
// the store is an orphan as before; and a chunk of a loaded side is loaded as
// ever, whatever side-* file failed to decode.
func TestReconcileKeepsTheChunksOfAnUnreadableSideFile(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	dnPath := nf.LocalDnPath(testCluster, testDn)
	sidePath := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	chunkPath := nf.LocalMigrBmPath(
		testCluster, testDn, testSp, testMigrId, 0)
	// seed writes the dn state file listing sideIds, one bitmap chunk of
	// testSide's migration and, when undecodable is set, a side state file
	// that does not decode in testSide's place.
	seed := func(
		t *testing.T, undecodable bool, sideIds ...uint64,
	) *fakeNode {
		t.Helper()
		ctx := context.Background()
		node := newDiskNode()
		if err := node.writeProto(ctx, dnPath,
			dnReq(1, sideIds...)); err != nil {
			t.Fatalf("seeding the dn state file: %v", err)
		}
		if err := node.writeProto(ctx, chunkPath,
			pushReq(0, testMigrId, []byte{0x05})); err != nil {
			t.Fatalf("seeding the bitmap chunk file: %v", err)
		}
		if undecodable {
			node.protos[sidePath] = []byte{0xff}
			if proto.Unmarshal(node.protos[sidePath],
				&pb.SyncupSideRequest{}) == nil {
				t.Fatalf("the fixture's side state file decodes")
			}
		}
		node.Reset()
		return node
	}
	chunkRms := func(node *fakeNode) int {
		n := 0
		for _, call := range node.callsMatching("cmd rm -f ") {
			if strings.Contains(call, " "+chunkPath) {
				n++
			}
		}
		return n
	}

	t.Run("its dn still names the side", func(t *testing.T) {
		ctx := context.Background()
		node := seed(t, true, testSide)
		seeded := node.protos[chunkPath]

		srv := startTestServer(t, node)

		if n := chunkRms(node); n != 0 {
			t.Errorf("the chunk was deleted %d times beside a side "+
				"file that did not decode:\n%s", n,
				strings.Join(node.callsMatching("cmd rm -f "), "\n"))
		}
		if got, ok := node.protos[chunkPath]; !ok ||
			!bytes.Equal(got, seeded) {
			t.Errorf("%s did not survive the restart", chunkPath)
		}
		if got := node.protos[sidePath]; !bytes.Equal(got, []byte{0xff}) {
			t.Errorf("the undecodable side state file was touched")
		}
		// Not hidden either: the side is unknown to its Check round, which
		// is what the worker re-sends the SyncupSide for.
		reply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 2,
		}, nil)
		if got := reply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("CheckSide code %d, want %d", got,
				common.ReplyCodeUnknownObject)
		}
	})

	t.Run("its dn no longer names the side", func(t *testing.T) {
		node := seed(t, true)

		startTestServer(t, node)

		if n := chunkRms(node); n != 1 {
			t.Errorf("the orphan chunk was deleted %d times, want 1", n)
		}
		if _, ok := node.protos[chunkPath]; ok {
			t.Errorf("the chunk of a side its dn no longer names " +
				"survived the restart")
		}
	})

	t.Run("no side file left unread", func(t *testing.T) {
		node := seed(t, false, testSide)

		startTestServer(t, node)

		if n := chunkRms(node); n != 1 {
			t.Errorf("the orphan chunk was deleted %d times, want 1", n)
		}
		if _, ok := node.protos[chunkPath]; ok {
			t.Errorf("the chunk of a side with no state file survived " +
				"the restart")
		}
	})

	t.Run("its dn has no file", func(t *testing.T) {
		ctx := context.Background()
		node := newDiskNode()
		if err := node.writeProto(ctx, chunkPath,
			pushReq(0, testMigrId, []byte{0x05})); err != nil {
			t.Fatalf("seeding the bitmap chunk file: %v", err)
		}
		node.protos[sidePath] = []byte{0xff}
		node.Reset()

		startTestServer(t, node)

		if n := chunkRms(node); n != 1 {
			t.Errorf("the chunk of a side whose dn has no file was "+
				"deleted %d times, want 1", n)
		}
		if _, ok := node.protos[chunkPath]; ok {
			t.Errorf("the chunk of a side whose dn has no file " +
				"survived the restart")
		}
	})

	// The stray an interrupted atomic write leaves beside a side file sorts
	// under the side- prefix and, half written, does not decode. It names no
	// side, but the side it sits beside is loaded, and that side's chunk is
	// loaded with it: skipping it would take it out of the applied set, and
	// the worker would push the whole migration again after every restart
	// for as long as the stray is there.
	t.Run("a temporary side file beside a loaded side", func(t *testing.T) {
		ctx := context.Background()
		node := seed(t, false, testSide)
		if err := node.writeProto(ctx, sidePath,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("seeding the side state file: %v", err)
		}
		stray := sidePath + ".tmp-1234567890"
		node.protos[stray] = []byte{0xff}
		node.Reset()

		srv := startTestServer(t, node)

		if !node.hasCall("readproto " + stray) {
			t.Fatalf("the stray file was never read:\n%s",
				strings.Join(node.Calls(), "\n"))
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		if st == nil {
			t.Fatalf("a stray side file that did not load skipped the " +
				"side it sits beside")
		}
		if n := st.chunks.Len(); n != 1 {
			t.Errorf("chunk set has %d chunks after the restart, want 1", n)
		}
		if got := srv.bitmapInfo(st).GetBmIdxList(); len(got) != 1 ||
			got[0] != 0 {
			t.Errorf("bm_idx_list %v after the restart, want [0]", got)
		}
		if n := chunkRms(node); n != 0 {
			t.Errorf("the chunk of a loaded side was deleted %d times", n)
		}
	})
}
