package cnagent

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// §6.27 — a zero conf member is refused (CN8, dnagent.md §2.1)
// ---------------------------------------------------------------------------
//
// The control plane resolves every defaultable member when it writes the
// conf, so a zero reaching this agent is a conf nobody chose. The refusal is
// total: the request never becomes desired state, nothing is converged and
// nothing is persisted, so the next Reconcile cannot replay it either.
//
// The four messages below are asserted verbatim and are the same literals
// model/capacity_test.go asserts of model.ValidateBdevConf, which is what
// keeps the two deliberate copies of the rule in step (dnagent.md §2.1).

const (
	msgNoBlockSize = "invalid stored conf: " +
		"bdev_conf.dm_pool_conf.data_block_size is zero"
	msgNoWaterMark = "invalid stored conf: " +
		"bdev_conf.dm_pool_conf.low_water_mark_pct is zero"
	msgNoStripeSize = "invalid stored conf: " +
		"bdev_conf.dm_raid0_conf.stripe_size is zero"
	msgNoChunkCnt = "invalid stored conf: bdev_conf.redund_conf." +
		"redund_md_raid1.bitmap_chunk_block_cnt is zero"
)

// zeroConfCases are the four members a stored bdev_conf must carry, one case
// each. raid1 picks the redund_conf arm the case needs: the chunk count only
// exists under md-raid1.
var zeroConfCases = []struct {
	name  string
	raid1 bool
	zero  func(conf *pb.BdevConf)
	want  string
}{
	{
		name: "data_block_size",
		zero: func(conf *pb.BdevConf) { conf.DmPoolConf.DataBlockSize = 0 },
		want: msgNoBlockSize,
	},
	{
		name: "low_water_mark_pct",
		zero: func(conf *pb.BdevConf) { conf.DmPoolConf.LowWaterMarkPct = 0 },
		want: msgNoWaterMark,
	},
	{
		name: "stripe_size",
		zero: func(conf *pb.BdevConf) { conf.DmRaid0Conf.StripeSize = 0 },
		want: msgNoStripeSize,
	},
	{
		name:  "bitmap_chunk_block_cnt",
		raid1: true,
		zero: func(conf *pb.BdevConf) {
			conf.GetRedundConf().GetRedundMdRaid1().BitmapChunkBlockCnt = 0
		},
		want: msgNoChunkCnt,
	},
}

// zeroConfReq is the fixture request with exactly one bdev_conf member
// cleared — the shape a pre-change cluster's stored conf has.
func zeroConfReq(o reqOpts, zero func(conf *pb.BdevConf)) *pb.SyncupCntlrRequest {
	req := cntlrReq(o)
	zero(req.BdevConf)
	return req
}

// assertRefusalRecord is the one Error record §7 asks for: msg
// msgInvalidStoredConf, the validator's own text under "error", and the ids
// that name the cntlr an operator has to go look at.
func assertRefusalRecord(
	t *testing.T,
	capture *logCapture,
	wantDetails string,
) {
	t.Helper()
	recs := capture.msgRecords(t, msgInvalidStoredConf)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1", len(recs), msgInvalidStoredConf)
	}
	rec := recs[0]
	if rec["error"] != wantDetails {
		t.Errorf("record error %v, want %q", rec["error"], wantDetails)
	}
	if rec["level"] != "ERROR" {
		t.Errorf("record level %v, want ERROR", rec["level"])
	}
	for _, key := range []string{"cluster_id", "cn_id", "sp_id", "cntlr_id"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("the refusal record names no %s", key)
		}
	}
}

// TestSyncupCntlrRefusesAZeroConfMember drives the refusal from the RPC
// entrance: a cntlr converged at revision 2, then a revision-3 request whose
// bdev_conf lost one member.
func TestSyncupCntlrRefusesAZeroConfMember(t *testing.T) {
	for _, tc := range zeroConfCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			capture := captureLogs(t)
			syncupBoth(t, srv,
				reqOpts{revision: 2, primary: true, raid1: tc.raid1})
			key := cntlrKey(testCluster, testCn, testSp, testCntlr)
			st := srv.getCntlr(key)
			if st == nil {
				t.Fatalf("the fixture converge left no cntlr state")
			}
			reqBefore := st.loadReq()
			path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp,
				testCntlr)
			storedBefore := node.protos[path]
			if len(storedBefore) == 0 {
				t.Fatalf("the fixture converge persisted nothing")
			}
			node.Reset()

			reply, err := srv.SyncupCntlr(context.Background(), zeroConfReq(
				reqOpts{revision: 3, primary: true, raid1: tc.raid1},
				tc.zero))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}

			// The reply: the refusal code, the validator's own text, and the
			// STORED revision — the request's 3 was never accepted, and
			// echoing it would tell the worker the opposite.
			if reply.GetAgentReply().GetCode() !=
				common.ReplyCodeInvalidConf {
				t.Fatalf("code %d, want %d",
					reply.GetAgentReply().GetCode(),
					common.ReplyCodeInvalidConf)
			}
			if reply.GetAgentReply().GetDetails() != tc.want {
				t.Errorf("details %q, want %q",
					reply.GetAgentReply().GetDetails(), tc.want)
			}
			if reply.GetRevision() != 2 {
				t.Errorf("reply revision %d, want the stored 2",
					reply.GetRevision())
			}
			if reply.GetCntlrInfo() != nil {
				t.Errorf("a refused request reported converge info")
			}

			// The node: nothing was driven at all. Mutations() is every
			// recorded call that is not a probe, so this one assertion
			// covers the dm, md, nvme and configfs writes a converge makes
			// and the local-store WriteProto that follows one.
			if mutations := node.Mutations(); len(mutations) != 0 {
				t.Fatalf("a refused SyncupCntlr mutated:\n%s",
					strings.Join(mutations, "\n"))
			}
			// Not even a probe: the gate sits before the converge, so this
			// is the point with literally zero side effects.
			if calls := node.Calls(); len(calls) != 0 {
				t.Errorf("a refused SyncupCntlr touched the node:\n%s",
					strings.Join(calls, "\n"))
			}
			if string(node.protos[path]) != string(storedBefore) {
				t.Errorf("the cntlr state file was rewritten")
			}
			if stored := storedProto(t, node, path); stored.GetRevision() !=
				2 {
				t.Errorf("the local store carries revision %d, want 2",
					stored.GetRevision())
			}

			// The agent: the desired state and the applied plan are the ones
			// revision 2 left, so a later teardown still plans from the shape
			// this agent actually built. The state is re-read rather than
			// reused, so a refusal that swapped the whole cntlrState in the
			// map cannot pass by leaving the old object behind.
			st = srv.getCntlr(key)
			if st == nil {
				t.Fatalf("the cntlr state vanished")
			}
			if req := st.loadReq(); req != reqBefore ||
				req.GetRevision() != 2 {
				t.Errorf("the refused request became desired state")
			}
			assertRefusalRecord(t, capture, tc.want)
		})
	}
}

// storedProto decodes one local-store file, so a test can compare what is on
// disk rather than the bytes' identity alone.
func storedProto(
	t *testing.T,
	node *fakeNode,
	path string,
) *pb.SyncupCntlrRequest {
	t.Helper()
	req := &pb.SyncupCntlrRequest{}
	raw, ok := node.protos[path]
	if !ok {
		t.Fatalf("no store file at %s", path)
	}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return req
}

// A redund_none conf with no bitmap chunk count is ACCEPTED: that member
// exists only under the md-raid1 arm of the oneof (§8.4), so its absence is
// correct rather than a missing default. The fixture's default redund_conf is
// redund_none, so the plain converge is the assertion — but the cn's own
// validator is checked directly too, because a fixture that happened to stop
// converging for another reason would hide the difference.
func TestRedundNoneNeedsNoBitmapChunkCount(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("a redund_none conf was refused: %v", reply.GetAgentReply())
	}
	req := cntlrReq(reqOpts{revision: 2, primary: true})
	if raid1 := req.GetBdevConf().GetRedundConf().GetRedundMdRaid1(); raid1 !=
		nil {
		t.Fatalf("the fixture is not redund_none: %v", raid1)
	}
	if err := agent.ValidateBdevConf(req.GetBdevConf()); err != nil {
		t.Fatalf("a redund_none conf must validate: %v", err)
	}
	if !node.hasCall("cmd dmsetup create " + poolName(srv)) {
		t.Fatalf("the accepted conf converged nothing")
	}
}

// TestConvergeCntlrRefusesAZeroConfMember covers convergeCntlr's own gate —
// the two entrances that do not come through syncupCntlr.
func TestConvergeCntlrRefusesAZeroConfMember(t *testing.T) {
	// The startup Reconcile: an older build persisted the zero, and this one
	// must skip it exactly as syncupCntlr would have.
	t.Run("reconcile", func(t *testing.T) {
		ctx := context.Background()
		node := newFakeNode()
		node.dirs[agent.NvmetRoot] = true
		srv := newCnServer(node)
		capture := captureLogs(t)
		if err := node.writeProto(ctx,
			srv.nf.LocalCnPath(testCluster, testCn),
			cnReq(2, true)); err != nil {
			t.Fatalf("seeding the cn state file: %v", err)
		}
		if err := node.writeProto(ctx,
			srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr),
			zeroConfReq(reqOpts{revision: 2, primary: true},
				func(conf *pb.BdevConf) {
					conf.DmPoolConf.DataBlockSize = 0
				})); err != nil {
			t.Fatalf("seeding the cntlr state file: %v", err)
		}
		node.Reset()
		reconcileForTest(t, srv)

		// The CN base state (§3.2) still converges — the mount, the arena
		// file, the loop device and the port. The refusal is scoped to the
		// cntlr, and every object a cntlr converge would build or sweep is
		// named by one of these: its dm devices, its md arrays, its leg
		// connections and everything under the nvmet subsystems tree, whose
		// `ana_grpid` writes are the sweep's first pre-step.
		//
		// The node-level sweep does ENUMERATE the nvmet tree — it must, to
		// find what an sp whose pointer left the list has behind it — so the
		// claim is about writes, not reads: nothing under subsystems/ is
		// created, removed, linked or written.
		for _, fragment := range []string{
			"cmd dmsetup create", "cmd dmsetup reload", "cmd dmsetup remove",
			"cmd dmsetup suspend", "cmd dmsetup message", "cmd mdadm",
			"cmd nvme connect", "cmd nvme disconnect",
			"cmd mkdir -p " + agent.NvmetRoot + "/subsystems",
			"cmd rmdir " + agent.NvmetRoot + "/subsystems",
			"cmd ln -s " + agent.NvmetRoot + "/subsystems",
			"cmd rm -f " + agent.NvmetRoot,
			"writedirect " + agent.NvmetRoot + "/subsystems",
		} {
			if node.hasCall(fragment) {
				t.Errorf("a refused Reconcile ran %q: %v", fragment,
					node.callsMatching(fragment))
			}
		}
		st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
		if st == nil {
			t.Fatalf("the persisted cntlr was not loaded at all")
		}
		if len(st.probers) != 0 {
			t.Errorf("a refused Reconcile started %d probers",
				len(st.probers))
		}
		assertRefusalRecord(t, capture, msgNoBlockSize)
	})

	// The CN10/CN18 connect-retry loop re-enters with the request it already
	// holds. Here the cntlr has already been converged once, so "left alone"
	// is a real claim: the gate returns before newCntlrPlan, and the sweep
	// that would otherwise run never enumerates anything.
	t.Run("connect retry", func(t *testing.T) {
		srv, node := newTestServer(t)
		capture := captureLogs(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		key := cntlrKey(testCluster, testCn, testSp, testCntlr)
		st := srv.getCntlr(key)
		if st == nil {
			t.Fatalf("the fixture converge left no cntlr state")
		}
		reqBefore := st.loadReq()
		// What a Reconcile-loaded zero, or a request the agent kept across a
		// downgrade, leaves in the state the retry loop re-enters with.
		st.storeReq(zeroConfReq(reqOpts{revision: 2, primary: true},
			func(conf *pb.BdevConf) { conf.DmPoolConf.LowWaterMarkPct = 0 }))
		node.Reset()

		srv.reconvergeCntlr(context.Background(), key, st)

		if mutations := node.Mutations(); len(mutations) != 0 {
			t.Fatalf("a refused reconverge mutated:\n%s",
				strings.Join(mutations, "\n"))
		}
		if st.loadReq() == reqBefore {
			t.Fatalf("the fixture did not install the zeroed request")
		}
		assertRefusalRecord(t, capture, msgNoWaterMark)
	})
}

// ---------------------------------------------------------------------------
// §6.33 — an unreadable cn file deletes nothing it might own (CN2)
// ---------------------------------------------------------------------------

// seedCntlrFiles writes the fixture cntlr's state file — a primary with one
// clone — and one bitmap chunk of that clone into the store of a node that
// holds nothing else: what a restart finds of a cntlr whose clone has
// received a push, with none of it loaded yet. It returns the two paths.
func seedCntlrFiles(
	t *testing.T,
	node *fakeNode,
	nf *common.NameFmt,
) (string, string) {
	t.Helper()
	ctx := context.Background()
	cntlrPath := nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	chunkPath := nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0, 0)
	if err := node.writeProto(ctx, cntlrPath, cntlrReq(reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()},
	})); err != nil {
		t.Fatalf("seeding the cntlr state file: %v", err)
	}
	if err := node.writeProto(ctx, chunkPath, &pb.PushCloneBitmapRequest{
		ClusterId:    testCluster,
		CnId:         testCn,
		CntlrPointer: cntlrPtr(),
		CloneId:      testClone,
		Bitmap:       []byte{0x05},
	}); err != nil {
		t.Fatalf("seeding the bitmap chunk file: %v", err)
	}
	return cntlrPath, chunkPath
}

// seedUndecodable puts a store file at path that does not decode as msg: a
// varint cut off after one byte.
func seedUndecodable(
	t *testing.T,
	node *fakeNode,
	path string,
	msg proto.Message,
) {
	t.Helper()
	node.mu.Lock()
	node.protos[path] = []byte{0xff}
	node.mu.Unlock()
	if proto.Unmarshal([]byte{0xff}, msg) == nil {
		t.Fatalf("the fixture's %s decodes", path)
	}
}

// storedFiles copies the fake's local store, so a test can compare it byte
// for byte after a restart.
func storedFiles(node *fakeNode) map[string][]byte {
	node.mu.Lock()
	defer node.mu.Unlock()
	out := make(map[string][]byte, len(node.protos))
	for path, raw := range node.protos {
		out[path] = append([]byte(nil), raw...)
	}
	return out
}

// rmCallsNaming counts the `rm -f` commands whose arguments name path.
func rmCallsNaming(node *fakeNode, path string) int {
	n := 0
	for _, call := range node.callsMatching("cmd rm -f ") {
		if slices.Contains(strings.Fields(call), path) {
			n++
		}
	}
	return n
}

// TestReconcileKeepsTheCntlrsOfAnUnreadableCnFile pins the cn-* file CN2
// cannot use because it does not decode. There is no request to load, so the
// CN is skipped — and the skip used to send every cntlr of it down the
// pointer-absent branch, which deleted each cntlr's state file and its clone
// bitmap chunks for want of a list that could not be read. Now the cntlrs
// are skipped with their CN: neither loaded nor deleted, and nothing of them
// converged, the node left exactly as the restart found it. Nor are they left
// looking healthy: the CN and the cntlr are unknown to the Check rounds — the
// cntlr still after the re-sent SyncupCn — and an unknown object is what the
// worker re-sends its Syncup* for (dnv-worker.md RW4). The re-sent
// SyncupCntlr then rebuilds the cntlr from the request it carries, and its
// reply acknowledges no chunk, so the worker pushes the chunk again.
func TestReconcileKeepsTheCntlrsOfAnUnreadableCnFile(t *testing.T) {
	ctx := context.Background()
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServer(node)
	// The rebuild below starts the primary's leg probers; none of their
	// rounds is wanted inside the test.
	srv.probeInterval = time.Hour
	cnPath := srv.nf.LocalCnPath(testCluster, testCn)
	cntlrPath, chunkPath := seedCntlrFiles(t, node, srv.nf)
	seedUndecodable(t, node, cnPath, &pb.SyncupCnRequest{})
	seeded := storedFiles(node)
	node.Reset()

	reconcileForTest(t, srv)

	// Nothing but reads. This is the regression guard: the skip used to
	// `rm -f` the cntlr's state file and its chunk right here.
	if mutations := node.Mutations(); len(mutations) != 0 {
		t.Fatalf("a cn state file that did not load tore its cntlrs' "+
			"state down:\n%s", strings.Join(mutations, "\n"))
	}
	for path, raw := range seeded {
		if got, ok := storedFiles(node)[path]; !ok {
			t.Errorf("%s was removed", path)
		} else if !bytes.Equal(got, raw) {
			t.Errorf("%s was rewritten", path)
		}
	}

	// Unknown, not healthy: the reply the worker answers with a re-sync.
	checkCntlr := func() *pb.AgentReply {
		reply, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
		return reply.GetAgentReply()
	}
	cnReply, _ := srv.checkCnRound(ctx, &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 2,
	}, nil)
	if got := cnReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Errorf("CheckCn code %d, want %d", got,
			common.ReplyCodeUnknownObject)
	}
	if got := checkCntlr().GetCode(); got != common.ReplyCodeUnknownObject {
		t.Errorf("CheckCntlr code %d, want %d", got,
			common.ReplyCodeUnknownObject)
	}

	// The re-sent SyncupCn is accepted and rewrites the file, which decodes
	// again. The cntlr is still unknown after it: nothing loaded it, so its
	// own Check round keeps asking for the SyncupCntlr that rebuilds its
	// state.
	reply, err := srv.SyncupCn(ctx, cnReq(2, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn rejected: %v", reply.GetAgentReply())
	}
	stored := &pb.SyncupCnRequest{}
	if err := proto.Unmarshal(storedFiles(node)[cnPath], stored); err != nil {
		t.Fatalf("the re-sent SyncupCn left the cn state file "+
			"unreadable: %v", err)
	}
	if stored.GetRevision() != 2 {
		t.Errorf("cn state file revision %d, want 2", stored.GetRevision())
	}
	if got := checkCntlr().GetCode(); got != common.ReplyCodeUnknownObject {
		t.Errorf("CheckCntlr code after the SyncupCn %d, want %d", got,
			common.ReplyCodeUnknownObject)
	}
	for _, path := range []string{cntlrPath, chunkPath} {
		if got, ok := storedFiles(node)[path]; !ok ||
			!bytes.Equal(got, seeded[path]) {
			t.Errorf("%s did not survive the re-sent SyncupCn", path)
		}
	}

	// The re-sent SyncupCntlr rebuilds the cntlr from the request it carries
	// and rewrites its file. The chunk file the reload skipped stays on
	// disk, neither loaded nor deleted, so the reply acknowledges no chunk
	// of the clone and the worker pushes it again.
	node.Reset()
	cntlrReply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()},
	}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if cntlrReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr rejected: %v", cntlrReply.GetAgentReply())
	}
	bmInfo := cntlrReply.GetBmInfoList()
	if len(bmInfo) != 1 || bmInfo[0].GetResId() != testClone {
		t.Fatalf("bm_info_list %v, want one entry for clone %d",
			bmInfo, testClone)
	}
	if got := chunkIds(bmInfo[0]); len(got) != 0 {
		t.Errorf("the rebuilt cntlr acknowledged chunks %v the reload "+
			"never loaded", got)
	}
	if !node.hasCall("writeproto " + cntlrPath) {
		t.Errorf("the rebuild did not rewrite the cntlr state file")
	}
	if got := storedProto(t, node, cntlrPath).GetRevision(); got != 2 {
		t.Errorf("cntlr state file revision %d, want 2", got)
	}
	if got, ok := storedFiles(node)[chunkPath]; !ok ||
		!bytes.Equal(got, seeded[chunkPath]) {
		t.Errorf("%s did not survive the rebuild", chunkPath)
	}
	if got := checkCntlr(); got.GetCode() != 0 {
		t.Errorf("CheckCntlr after the rebuild = %v, want code 0", got)
	}
}

// seedStore is a node holding nothing but the store: the cntlr and chunk
// files of seedCntlrFiles, the cn file cnReq(2, listed) unless cnFile is
// false, and an undecodable file of a second CN when stray is set. It returns
// the cntlr and chunk paths beside the server and the node.
func seedStore(
	t *testing.T, cnFile bool, listed bool, stray bool,
) (*CnAgentServer, *fakeNode, string, string) {
	t.Helper()
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServer(node)
	cntlrPath, chunkPath := seedCntlrFiles(t, node, srv.nf)
	if cnFile {
		if err := node.writeProto(context.Background(),
			srv.nf.LocalCnPath(testCluster, testCn),
			cnReq(2, listed)); err != nil {
			t.Fatalf("seeding the cn state file: %v", err)
		}
	}
	if stray {
		seedUndecodable(t, node, strayCnPath(srv), &pb.SyncupCnRequest{})
	}
	node.Reset()
	return srv, node, cntlrPath, chunkPath
}

// strayCnPath is the file of the second CN that seedStore leaves undecodable.
func strayCnPath(srv *CnAgentServer) string {
	return srv.nf.LocalCnPath(testCluster, testCn2)
}

// assertStrayRead fails unless the reload read the undecodable file of the
// second CN: a case that means to run beside it proves nothing about it
// otherwise.
func assertStrayRead(t *testing.T, srv *CnAgentServer, node *fakeNode) {
	t.Helper()
	if stray := strayCnPath(srv); !node.hasCall("readproto " + stray) {
		t.Fatalf("the undecodable file %s was never read:\n%s", stray,
			strings.Join(node.Calls(), "\n"))
	}
}

// TestReconcileSkipsOnlyTheCntlrsOfAnUnloadedCn bounds the skip above from
// both sides. A cn-* file that does not load names no CN, so it keeps out
// only the cntlrs and chunks whose CN is not loaded: beside it, a CN whose
// own file loads has its cntlrs loaded and converged as on any restart, and a
// cntlr whose pointer has left that CN's list is dropped as ever. With no
// cn-* file left unread there is nothing to skip for: a CN with no file at
// all reads as a list that names no cntlr, so a cntlr of it has its request
// and its chunks deleted on the spot — a skip that outlived its reason would
// keep them on disk for ever.
func TestReconcileSkipsOnlyTheCntlrsOfAnUnloadedCn(t *testing.T) {
	t.Run("an undecodable file of another cn", func(t *testing.T) {
		ctx := context.Background()
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})
		seedUndecodable(t, node, strayCnPath(srv), &pb.SyncupCnRequest{})
		node.Reset()

		restarted := newCnServer(node)
		reconcileForTest(t, restarted)

		assertStrayRead(t, restarted, node)
		if restarted.getCntlr(cntlrKey(
			testCluster, testCn, testSp, testCntlr)) == nil {
			t.Fatalf("a cn file that did not load skipped a cntlr of a " +
				"loaded cn")
		}
		reply, _ := restarted.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
		if got := reply.GetAgentReply(); got.GetCode() != 0 ||
			reply.GetRevision() != 2 {
			t.Errorf("CheckCntlr after the restart = %v at revision %d, "+
				"want 0 at 2", got, reply.GetRevision())
		}
	})

	t.Run("no cn file at all", func(t *testing.T) {
		srv, node, cntlrPath, chunkPath := seedStore(t, false, false, false)

		reconcileForTest(t, srv)

		if got := node.callsMatching("cmd rm -f "); len(got) != 1 ||
			rmCallsNaming(node, cntlrPath) != 1 ||
			rmCallsNaming(node, chunkPath) != 1 {
			t.Errorf("rm calls = %q, want one naming the cntlr's state "+
				"file and its chunk", got)
		}
		for _, path := range []string{cntlrPath, chunkPath} {
			if _, ok := storedFiles(node)[path]; ok {
				t.Errorf("%s survived a restart that found no cn file",
					path)
			}
		}
	})

	t.Run("a pointer that left a loaded cn's list", func(t *testing.T) {
		srv, node, cntlrPath, chunkPath := seedStore(t, true, false, true)

		reconcileForTest(t, srv)

		assertStrayRead(t, srv, node)
		for _, path := range []string{cntlrPath, chunkPath} {
			if n := rmCallsNaming(node, path); n != 1 {
				t.Errorf("%s was deleted %d times, want 1", path, n)
			}
			if _, ok := storedFiles(node)[path]; ok {
				t.Errorf("%s of a cntlr its loaded cn no longer lists "+
					"survived the restart", path)
			}
		}
	})
}

// TestReconcileKeepsTheChunksOfAnUnreadableCntlrFile is the same rule one
// level down, the disk node's rule for a side-* file (dnagent.md DN2). A
// cntlr-* file that does not decode names no cntlr, so a chunk whose cntlr
// did not load may be that file's — and while the chunk's loaded CN still
// names its cntlr, nothing read here proves the cntlr gone: the chunk is
// skipped, neither loaded nor deleted, and the cntlr, which nothing loaded,
// is unknown to its Check round, which is what the worker re-sends its
// SyncupCntlr for. It used to be deleted as an orphan, so a read failure of
// one file destroyed another; here that runs beside an undecodable file of
// another CN, which keeps out only that CN's cntlrs and chunks. The skip is
// bounded both ways: a chunk whose loaded CN no longer names its cntlr is an
// orphan whatever cntlr-* file failed to decode, and with no cntlr-* file
// left unread a chunk whose cntlr has no state file is an orphan as before.
func TestReconcileKeepsTheChunksOfAnUnreadableCntlrFile(t *testing.T) {
	t.Run("its cn still names the cntlr", func(t *testing.T) {
		ctx := context.Background()
		srv, node, cntlrPath, chunkPath := seedStore(t, true, true, true)
		// The cntlr's own file does not decode; its CN's does and lists it.
		seedUndecodable(t, node, cntlrPath, &pb.SyncupCntlrRequest{})
		seeded := storedFiles(node)[chunkPath]
		node.Reset()

		reconcileForTest(t, srv)

		assertStrayRead(t, srv, node)
		if n := rmCallsNaming(node, chunkPath); n != 0 {
			t.Errorf("the chunk was deleted %d times beside a cntlr file "+
				"that did not decode:\n%s", n,
				strings.Join(node.callsMatching("cmd rm -f "), "\n"))
		}
		if got, ok := storedFiles(node)[chunkPath]; !ok ||
			!bytes.Equal(got, seeded) {
			t.Errorf("%s did not survive the restart byte for byte",
				chunkPath)
		}
		if got := storedFiles(node)[cntlrPath]; !bytes.Equal(got,
			[]byte{0xff}) {
			t.Errorf("the undecodable cntlr state file was touched")
		}
		// Not hidden either: the cntlr is unknown to its Check round.
		reply, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
		if got := reply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("CheckCntlr code %d, want %d", got,
				common.ReplyCodeUnknownObject)
		}
	})

	t.Run("its cn no longer names the cntlr", func(t *testing.T) {
		srv, node, cntlrPath, chunkPath := seedStore(t, true, false, false)
		seedUndecodable(t, node, cntlrPath, &pb.SyncupCntlrRequest{})
		node.Reset()

		reconcileForTest(t, srv)

		if !node.hasCall("readproto " + cntlrPath) {
			t.Fatalf("the undecodable cntlr state file was never read")
		}
		if n := rmCallsNaming(node, chunkPath); n != 1 {
			t.Errorf("the orphan chunk was deleted %d times, want 1", n)
		}
		if _, ok := storedFiles(node)[chunkPath]; ok {
			t.Errorf("the chunk of a cntlr its cn no longer names " +
				"survived the restart")
		}
	})

	t.Run("no cntlr file left unread", func(t *testing.T) {
		srv, node, cntlrPath, chunkPath := seedStore(t, true, true, false)
		// Its CN lists the cntlr, which has no state file at all.
		node.mu.Lock()
		delete(node.protos, cntlrPath)
		node.mu.Unlock()

		reconcileForTest(t, srv)

		if n := rmCallsNaming(node, chunkPath); n != 1 {
			t.Errorf("the orphan chunk was deleted %d times, want 1", n)
		}
		if _, ok := storedFiles(node)[chunkPath]; ok {
			t.Errorf("the chunk of a cntlr with no state file survived " +
				"the restart")
		}
	})
}
