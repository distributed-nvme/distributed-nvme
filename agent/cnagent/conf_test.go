package cnagent

import (
	"context"
	"strings"
	"testing"

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
			if st == nil || st.applied == nil {
				t.Fatalf("the fixture converge left no applied plan")
			}
			appliedBefore, reqBefore := st.applied, st.req
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
			if st.req != reqBefore || st.req.GetRevision() != 2 {
				t.Errorf("the refused request became desired state")
			}
			if st.applied != appliedBefore {
				t.Errorf("the refusal replaced the applied plan")
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
		// cntlr, and every object a cntlr converge would build or retire is
		// named by one of these: its dm devices, its md arrays, its leg
		// connections and everything under the nvmet subsystems tree, whose
		// `ana_grpid` writes are the retire phase's first step.
		for _, fragment := range []string{
			"cmd dmsetup create", "cmd dmsetup reload", "cmd dmsetup remove",
			"cmd dmsetup suspend", "cmd dmsetup message", "cmd mdadm",
			"cmd nvme connect", "cmd nvme disconnect",
			agent.NvmetRoot + "/subsystems",
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
		if st.applied != nil {
			t.Errorf("a refused Reconcile left an applied plan")
		}
		if len(st.probers) != 0 {
			t.Errorf("a refused Reconcile started %d probers",
				len(st.probers))
		}
		assertRefusalRecord(t, capture, msgNoBlockSize)
	})

	// The CN10/CN18 connect-retry loop re-enters with the request it already
	// holds. Here the cntlr HAS an applied plan, so "left alone" is a real
	// claim: the gate returns before newCntlrPlan, and a later teardown still
	// plans from the shape this agent built.
	t.Run("connect retry", func(t *testing.T) {
		srv, node := newTestServer(t)
		capture := captureLogs(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		key := cntlrKey(testCluster, testCn, testSp, testCntlr)
		st := srv.getCntlr(key)
		if st == nil || st.applied == nil {
			t.Fatalf("the fixture converge left no applied plan")
		}
		appliedBefore := st.applied
		// What a Reconcile-loaded zero, or a request the agent kept across a
		// downgrade, leaves in the state the retry loop re-enters with.
		st.req = zeroConfReq(reqOpts{revision: 2, primary: true},
			func(conf *pb.BdevConf) { conf.DmPoolConf.LowWaterMarkPct = 0 })
		node.Reset()

		srv.reconvergeCntlr(context.Background(), key, st)

		if mutations := node.Mutations(); len(mutations) != 0 {
			t.Fatalf("a refused reconverge mutated:\n%s",
				strings.Join(mutations, "\n"))
		}
		if st.applied != appliedBefore {
			t.Errorf("the refusal replaced the applied plan")
		}
		assertRefusalRecord(t, capture, msgNoWaterMark)
	})
}
