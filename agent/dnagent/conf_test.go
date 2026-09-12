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
// The obvious implementation — skip the file, like an unreadable one — is
// DESTRUCTIVE: Reconcile's side loop reads a missing DN record as "this side
// left its parent's list" and calls teardownSide on every side of that DN,
// removing its nvmet exports, its dm devices and its local state file. A conf
// fault must not delete resources. So the record is LOADED, convergeDn refuses
// it once, and every side of that DN is left exactly as the restart found it.
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
	// with the refusal written as a skip, teardownSide removed this file.
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
