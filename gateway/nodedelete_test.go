package gateway

// The node deletes ask the node's agent (architecture.md, Disk nodes and
// Controller nodes; gateway.md, Disk nodes and Controller nodes, AG1, AG3,
// AG4): without force, a delete is refused unless the agent's latest
// observation of the node proves it clean at the node's stored revision, and
// on a disk node shows an empty volume table; the deciding STM requires the
// node the planning snapshot read; force asks no agent and still refuses a
// node that hosts a side or a cntlr.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ndelDn is one disk node made for a delete test: its fake, its keys and the
// stored revision its agent's answer is held to.
type ndelDn struct {
	cluster string
	cid     uint64
	addr    string
	dnId    uint64
	total   uint64
	rev     uint64
	agent   *fakeAgent
	keys    []string
}

// ndelMakeDn makes one disk node in a cluster of its own, its fake answering
// clean at the node's stored revision.
func ndelMakeDn(t *testing.T, s *Server, kind string) *ndelDn {
	t.Helper()
	cluster := hnodeName(kind)
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	dnId := mustDn(t, s, cluster, addr, "rack-1", 16<<30)
	plantCleanDn(t, s, cluster, addr)
	dn := &pb.DnConf{}
	hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
	return &ndelDn{
		cluster: cluster,
		cid:     cid,
		addr:    addr,
		dnId:    dnId,
		total:   dn.GetTotalExtCnt(),
		rev:     dnTok(t, s, cluster, addr),
		agent:   fakeAgentOf(t, addr),
		keys: []string{
			model.DnConfKey(cid, addr),
			model.DnRevKey(dn.GetShardCode(), cid, dnId),
			model.DnGlobalKey(cid),
		},
	}
}

// ndelCn is ndelDn for a controller node.
type ndelCn struct {
	cluster string
	cid     uint64
	addr    string
	cnId    uint64
	rev     uint64
	agent   *fakeAgent
	keys    []string
}

func ndelMakeCn(t *testing.T, s *Server, kind string) *ndelCn {
	t.Helper()
	cluster := hnodeName(kind)
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "cn")
	cnId := mustCn(t, s, cluster, addr, "rack-1", 8<<30)
	plantCleanCn(t, s, cluster, addr)
	cn := &pb.CnConf{}
	hnodeGet(t, s, model.CnConfKey(cid, addr), cn)
	return &ndelCn{
		cluster: cluster,
		cid:     cid,
		addr:    addr,
		cnId:    cnId,
		rev:     cnTok(t, s, cluster, addr),
		agent:   fakeAgentOf(t, addr),
		keys: []string{
			model.CnConfKey(cid, addr),
			model.CnRevKey(cn.GetShardCode(), cid, cnId),
			model.CnGlobalKey(cid),
		},
	}
}

// ndelModRevs is the mod revision of each key, so a refusal can be shown
// to have written none of them.
func ndelModRevs(t *testing.T, s *Server, keys []string) []int64 {
	t.Helper()
	revs := make([]int64, len(keys))
	for i, key := range keys {
		revs[i] = hnodeModRev(t, s, key)
		if revs[i] == 0 {
			t.Fatalf("key %q is missing", key)
		}
	}
	return revs
}

// ndelWantUntouched fails unless every key still stands at the mod revision
// before.
func ndelWantUntouched(
	t *testing.T,
	s *Server,
	keys []string,
	before []int64,
	label string,
) {
	t.Helper()
	for i, key := range keys {
		if got := hnodeModRev(t, s, key); got != before[i] {
			t.Errorf("%s: key %q moved from %d to %d", label, key, before[i], got)
		}
	}
}

// ndelDelDn and ndelDelCn send one delete with no token.
func ndelDelDn(t *testing.T, s *Server, n *ndelDn, force bool) error {
	t.Helper()
	_, err := s.DeleteDiskNode(context.Background(), &pb.DeleteDiskNodeRequest{
		ClusterName: n.cluster,
		AddrPort:    n.addr,
		Force:       force,
	})
	return err
}

func ndelDelCn(t *testing.T, s *Server, n *ndelCn, force bool) error {
	t.Helper()
	_, err := s.DeleteControllerNode(context.Background(),
		&pb.DeleteControllerNodeRequest{
			ClusterName: n.cluster,
			AddrPort:    n.addr,
			Force:       force,
		})
	return err
}

// ndelWantRefused asserts a FAILED_PRECONDITION that carries every piece of
// want and names force as the way on.
func ndelWantRefused(t *testing.T, err error, label string, want ...string) {
	t.Helper()
	wantCode(t, err, codes.FailedPrecondition, label)
	for _, piece := range append(want, "force") {
		hnodeWantMsg(t, err, piece, label)
	}
}

// TestDeleteDiskNodeRefusesWhileTheAgentReportsLeftovers pins the code arm
// of the proof: an agent whose latest observation of the node names a
// leftover does not prove the node clean, the refusal passes the agent's
// details on and names force, and nothing is written.
func TestDeleteDiskNodeRefusesWhileTheAgentReportsLeftovers(t *testing.T) {
	s := newTestServer(t)
	n := ndelMakeDn(t, s, "dnleft")
	n.agent.setNodeAnswer(&pb.AgentReply{
		Code:    common.ReplyCodeLeftover,
		Details: "leftover(1): dm:dnv-x",
	}, n.rev)
	before := ndelModRevs(t, s, n.keys)

	err := ndelDelDn(t, s, n, false)
	ndelWantRefused(t, err, "a leftover", "leftover(1): dm:dnv-x", "code 4")
	ndelWantUntouched(t, s, n.keys, before, "a leftover")
}

// TestDeleteDiskNodeRefusesAnAgentNotAtTheNodesRevision pins the revision
// arm: an observation below the stored revision is of a desired state the
// agent has not applied, whose sweep may not have run; one above it is an
// agent that holds a newer state than etcd.
func TestDeleteDiskNodeRefusesAnAgentNotAtTheNodesRevision(t *testing.T) {
	s := newTestServer(t)
	n := ndelMakeDn(t, s, "dnrev")
	// The stored revision moves to 2, so the revision below is not zero:
	// a disabled flip bumps nothing, so the rev key is put by hand.
	revKey := n.keys[1]
	mustPut(t, newTestClient(t), revKey,
		&pb.DnRev{AddrPort: n.addr, Revision: 2})
	before := ndelModRevs(t, s, n.keys)
	for _, tc := range []struct {
		label    string
		revision uint64
		want     string
	}{
		{"an agent behind the node", 1, "has not applied"},
		{"an agent ahead of etcd", 3, "newer desired state than etcd"},
	} {
		n.agent.setNodeAnswer(nil, tc.revision)
		err := ndelDelDn(t, s, n, false)
		ndelWantRefused(t, err, tc.label, tc.want)
		ndelWantUntouched(t, s, n.keys, before, tc.label)
	}
}

// TestDeleteDiskNodeRefusesUntilTheTableIsEmpty pins the disk node's half of
// the proof: a clean code at the node's revision proves no empty volume
// table, so the counts must be there and show no side record, every extent
// free and every clone-metadata unit free. Counts that are absent are what
// an agent sends when the meta row is not OK: a disk it has not confirmed as
// this node's, or a conf it refuses.
func TestDeleteDiskNodeRefusesUntilTheTableIsEmpty(t *testing.T) {
	s := newTestServer(t)
	n := ndelMakeDn(t, s, "dncounts")
	before := ndelModRevs(t, s, n.keys)
	for _, tc := range []struct {
		label  string
		counts func(c *pb.DnInfo_DiskCounts) *pb.DnInfo_DiskCounts
		want   string
	}{
		{
			"no counts",
			func(*pb.DnInfo_DiskCounts) *pb.DnInfo_DiskCounts { return nil },
			"no counts",
		},
		{
			"a side record left",
			func(c *pb.DnInfo_DiskCounts) *pb.DnInfo_DiskCounts {
				c.SideRecCnt = 1
				return c
			},
			"1 side records",
		},
		{
			"an extent held",
			func(c *pb.DnInfo_DiskCounts) *pb.DnInfo_DiskCounts {
				c.FreeExtCnt--
				return c
			},
			"extents are free",
		},
		{
			"a clone-metadata unit held",
			func(c *pb.DnInfo_DiskCounts) *pb.DnInfo_DiskCounts {
				c.FreeMetaUnits--
				return c
			},
			"clone-metadata units are free",
		},
	} {
		n.agent.setNodeAnswer(nil, n.rev)
		n.agent.mu.Lock()
		n.agent.dnInfo = &pb.DnInfo{
			DiskCounts: tc.counts(cleanDiskCounts(n.total)),
		}
		n.agent.mu.Unlock()
		err := ndelDelDn(t, s, n, false)
		ndelWantRefused(t, err, tc.label, tc.want)
		ndelWantUntouched(t, s, n.keys, before, tc.label)
	}
}

// TestDeleteNodeRefusesAnUnprovenAgent pins the arms both roles share: an
// agent that starts, one that does not hold the node, one that refuses its
// conf, one that holds no observation of the node yet and one that does not
// answer at all prove nothing, and each refusal writes nothing (AG3).
func TestDeleteNodeRefusesAnUnprovenAgent(t *testing.T) {
	s := newTestServer(t)
	type arm struct {
		label string
		set   func(f *fakeAgent, rev uint64)
		want  string
	}
	arms := []arm{
		{"a starting agent", func(f *fakeAgent, rev uint64) {
			f.setNodeAnswer(&pb.AgentReply{
				Code:    common.ReplyCodeStarting,
				Details: "starting",
			}, 0)
			f.setStamp(fakeAgentNoObsStamp())
		}, "code 6"},
		{"an agent that does not hold the node", func(f *fakeAgent, rev uint64) {
			f.setNodeAnswer(&pb.AgentReply{
				Code:    common.ReplyCodeUnknownObject,
				Details: "unknown node",
			}, 0)
			f.setStamp(fakeAgentNoObsStamp())
		}, "code 2"},
		{"an agent that refuses the conf", func(f *fakeAgent, rev uint64) {
			f.setNodeAnswer(&pb.AgentReply{
				Code:    common.ReplyCodeInvalidConf,
				Details: "bad conf",
			}, rev)
		}, "code 3"},
		{"no observation yet", func(f *fakeAgent, rev uint64) {
			f.setNodeAnswer(nil, rev)
			f.setStamp(fakeAgentNoObsStamp())
		}, "no observation"},
		{"an agent that does not answer", func(f *fakeAgent, rev uint64) {
			f.setFail(status.Error(codes.Unavailable, "agent gone"))
		}, "did not answer"},
	}
	for _, a := range arms {
		dn := ndelMakeDn(t, s, "dnunproven")
		a.set(dn.agent, dn.rev)
		before := ndelModRevs(t, s, dn.keys)
		err := ndelDelDn(t, s, dn, false)
		ndelWantRefused(t, err, "disk node, "+a.label, a.want)
		ndelWantUntouched(t, s, dn.keys, before, "disk node, "+a.label)

		cn := ndelMakeCn(t, s, "cnunproven")
		a.set(cn.agent, cn.rev)
		before = ndelModRevs(t, s, cn.keys)
		err = ndelDelCn(t, s, cn, false)
		ndelWantRefused(t, err, "controller node, "+a.label, a.want)
		ndelWantUntouched(t, s, cn.keys, before, "controller node, "+a.label)
	}
}

// TestDeleteNodeDeletesACleanNode pins the path that passes: the agent is
// asked once, about the node the planning snapshot resolved, with no
// read_io_stats (InspectDiskNode alone asks for counters), and the node's
// keys go.
func TestDeleteNodeDeletesACleanNode(t *testing.T) {
	s := newTestServer(t)
	dn := ndelMakeDn(t, s, "dnclean")
	wantCode(t, ndelDelDn(t, s, dn, false), codes.OK, "a clean disk node")
	if got := dn.agent.callCount("GetDnInfo"); got != 1 {
		t.Errorf("GetDnInfo was called %d times, want once", got)
	}
	if reqs := dn.agent.dnInfoReqList(); len(reqs) == 1 {
		req := reqs[0]
		if req.GetClusterId() != dn.cid || req.GetDnId() != dn.dnId ||
			req.GetReadIoStats() {
			t.Errorf("GetDnInfo asked %v, want cluster %d, dn %d and no "+
				"read_io_stats", req, dn.cid, dn.dnId)
		}
	}
	for _, key := range dn.keys[:2] {
		if hnodeExists(t, s, key) {
			t.Errorf("key %q survived the delete of a clean disk node", key)
		}
	}

	cn := ndelMakeCn(t, s, "cnclean")
	wantCode(t, ndelDelCn(t, s, cn, false), codes.OK, "a clean controller node")
	if got := cn.agent.callCount("GetCnInfo"); got != 1 {
		t.Errorf("GetCnInfo was called %d times, want once", got)
	}
	if reqs := cn.agent.cnInfoReqList(); len(reqs) == 1 {
		req := reqs[0]
		if req.GetClusterId() != cn.cid || req.GetCnId() != cn.cnId {
			t.Errorf("GetCnInfo asked %v, want cluster %d, cn %d",
				req, cn.cid, cn.cnId)
		}
	}
	for _, key := range cn.keys[:2] {
		if hnodeExists(t, s, key) {
			t.Errorf("key %q survived the delete of a clean controller node",
				key)
		}
	}
}

// TestDeleteNodeForceAsksNoAgent pins force: a node whose agent does not
// answer is deleted with no agent call; a node that still hosts a side or a
// cntlr is refused with force too, and that refusal does not offer force.
func TestDeleteNodeForceAsksNoAgent(t *testing.T) {
	s := newTestServer(t)
	cli := newTestClient(t)

	dn := ndelMakeDn(t, s, "dnforce")
	dn.agent.setFail(status.Error(codes.Unavailable, "agent gone"))
	wantCode(t, ndelDelDn(t, s, dn, true), codes.OK, "a forced disk node delete")
	if got := dn.agent.callCount("GetDnInfo"); got != 0 {
		t.Errorf("a forced delete called GetDnInfo %d times", got)
	}
	if hnodeExists(t, s, dn.keys[0]) {
		t.Errorf("the forced delete left the dn_conf")
	}

	cn := ndelMakeCn(t, s, "cnforce")
	cn.agent.setFail(status.Error(codes.Unavailable, "agent gone"))
	wantCode(t, ndelDelCn(t, s, cn, true), codes.OK,
		"a forced controller node delete")
	if got := cn.agent.callCount("GetCnInfo"); got != 0 {
		t.Errorf("a forced delete called GetCnInfo %d times", got)
	}
	if hnodeExists(t, s, cn.keys[0]) {
		t.Errorf("the forced delete left the cn_conf")
	}

	busyDn := ndelMakeDn(t, s, "dnforcebusy")
	conf := &pb.DnConf{}
	hnodeGet(t, s, busyDn.keys[0], conf)
	conf.SidePtrList = []*pb.SidePointer{{SpId: 7, LegId: 8, SideId: 9}}
	mustPut(t, cli, busyDn.keys[0], conf)
	before := ndelModRevs(t, s, busyDn.keys)
	err := ndelDelDn(t, s, busyDn, true)
	wantCode(t, err, codes.FailedPrecondition, "a forced delete of a busy dn")
	hnodeWantMsg(t, err, "still hosts 1 sides", "a forced delete of a busy dn")
	if strings.Contains(err.Error(), "force") {
		t.Errorf("the refusal of a busy disk node offers force: %v", err)
	}
	ndelWantUntouched(t, s, busyDn.keys, before, "a forced delete of a busy dn")

	busyCn := ndelMakeCn(t, s, "cnforcebusy")
	cnConf := &pb.CnConf{}
	hnodeGet(t, s, busyCn.keys[0], cnConf)
	cnConf.CntlrPtrList = []*pb.CntlrPointer{{SpId: 7, CntlrId: 8}}
	mustPut(t, cli, busyCn.keys[0], cnConf)
	before = ndelModRevs(t, s, busyCn.keys)
	err = ndelDelCn(t, s, busyCn, true)
	wantCode(t, err, codes.FailedPrecondition, "a forced delete of a busy cn")
	hnodeWantMsg(t, err, "still hosts 1 cntlrs", "a forced delete of a busy cn")
	if strings.Contains(err.Error(), "force") {
		t.Errorf("the refusal of a busy controller node offers force: %v",
			err)
	}
	ndelWantUntouched(t, s, busyCn.keys, before, "a forced delete of a busy cn")
	if got := busyDn.agent.callCount("GetDnInfo") +
		busyCn.agent.callCount("GetCnInfo"); got != 0 {
		t.Errorf("a forced delete of a busy node asked its agent %d times",
			got)
	}
}

// ndelWaitCall waits until the fake has received method.
func ndelWaitCall(t *testing.T, f *fakeAgent, method string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.callCount(method) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the fake never received %s", method)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ndelRunHeld runs del while the fake holds method, runs between the agent
// call and its answer, then lets the answer go and returns del's error.
func ndelRunHeld(
	t *testing.T,
	f *fakeAgent,
	method string,
	del func() error,
	between func(),
) error {
	t.Helper()
	unblock := f.setBlockOn(method)
	defer unblock()
	done := make(chan error, 1)
	go func() { done <- del() }()
	ndelWaitCall(t, f, method)
	between()
	unblock()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("the delete did not return")
		return errors.New("unreachable")
	}
}

// TestDeleteNodeRefusesWhenTheNodeMovesDuringTheCheck pins the deciding
// STM's compare with the planning snapshot (AG4), which runs whatever the
// token: the agent answers clean about the revision the snapshot read, and a
// revision moved during the call, or a node deleted and created again at the
// address, is ABORTED and nothing is deleted.
func TestDeleteNodeRefusesWhenTheNodeMovesDuringTheCheck(t *testing.T) {
	s := newTestServer(t)
	cli := newTestClient(t)

	// The revision moved during the call: a side added and dropped again
	// between the phases leaves the pointer list empty and the revision 2
	// above the one the agent proved.
	dn := ndelMakeDn(t, s, "dnmoved")
	err := ndelRunHeld(t, dn.agent, "GetDnInfo",
		func() error { return ndelDelDn(t, s, dn, false) },
		func() {
			mustPut(t, cli, dn.keys[1],
				&pb.DnRev{AddrPort: dn.addr, Revision: dn.rev + 2})
		})
	wantCode(t, err, codes.Aborted, "a disk node whose revision moved")
	hnodeWantMsg(t, err, "changed during the check",
		"a disk node whose revision moved")
	if !hnodeExists(t, s, dn.keys[0]) {
		t.Errorf("the disk node whose revision moved was deleted")
	}

	cn := ndelMakeCn(t, s, "cnmoved")
	err = ndelRunHeld(t, cn.agent, "GetCnInfo",
		func() error { return ndelDelCn(t, s, cn, false) },
		func() {
			mustPut(t, cli, cn.keys[1],
				&pb.CnRev{AddrPort: cn.addr, Revision: cn.rev + 2})
		})
	wantCode(t, err, codes.Aborted, "a controller node whose revision moved")
	hnodeWantMsg(t, err, "changed during the check",
		"a controller node whose revision moved")
	if !hnodeExists(t, s, cn.keys[0]) {
		t.Errorf("the controller node whose revision moved was deleted")
	}

	// The node was deleted, with force, and created again at the address
	// during the call: a new dn_id at the first revision, which the
	// revision compare alone would take for the node the agent proved.
	dnAgain := ndelMakeDn(t, s, "dnagain")
	err = ndelRunHeld(t, dnAgain.agent, "GetDnInfo",
		func() error { return ndelDelDn(t, s, dnAgain, false) },
		func() {
			wantCode(t, ndelDelDn(t, s, dnAgain, true), codes.OK,
				"the forced delete during the check")
			if _, err := s.CreateDiskNode(context.Background(),
				&pb.CreateDiskNodeRequest{
					ClusterName: dnAgain.cluster,
					AddrPort:    dnAgain.addr,
					NvmeTrConf:  testTrConf(dnAgain.addr),
					Location:    "rack-1",
				}); err != nil {
				t.Fatalf("CreateDiskNode again: %v", err)
			}
		})
	wantCode(t, err, codes.Aborted, "a disk node created again")
	hnodeWantMsg(t, err, "created again", "a disk node created again")
	again := &pb.DnConf{}
	hnodeGet(t, s, dnAgain.keys[0], again)
	if again.GetDnId() == dnAgain.dnId {
		t.Fatalf("the disk node was not created again under a new id")
	}

	cnAgain := ndelMakeCn(t, s, "cnagain")
	err = ndelRunHeld(t, cnAgain.agent, "GetCnInfo",
		func() error { return ndelDelCn(t, s, cnAgain, false) },
		func() {
			wantCode(t, ndelDelCn(t, s, cnAgain, true), codes.OK,
				"the forced delete during the check")
			if _, err := s.CreateControllerNode(context.Background(),
				&pb.CreateControllerNodeRequest{
					ClusterName: cnAgain.cluster,
					AddrPort:    cnAgain.addr,
					NvmeTrConf:  testTrConf(cnAgain.addr),
					Location:    "rack-1",
				}); err != nil {
				t.Fatalf("CreateControllerNode again: %v", err)
			}
		})
	wantCode(t, err, codes.Aborted, "a controller node created again")
	hnodeWantMsg(t, err, "created again", "a controller node created again")
	if !hnodeExists(t, s, cnAgain.keys[0]) {
		t.Errorf("the controller node created again was deleted")
	}
}

// TestDeleteNodeRefusesAClusterCreatedAgainDuringTheCheck pins the cluster
// arm of the compare: the cluster deleted and created again under its name
// during the call, and the node created again at the address, takes the
// first dn_id and the first revision again, the very numbers the agent
// proved; only the cluster id tells them apart.
func TestDeleteNodeRefusesAClusterCreatedAgainDuringTheCheck(t *testing.T) {
	s := newTestServer(t)
	dn := ndelMakeDn(t, s, "dnclusteragain")
	if dn.dnId != 1 || dn.rev != 1 {
		t.Fatalf("the node is dn %d at revision %d, want the first of each",
			dn.dnId, dn.rev)
	}
	err := ndelRunHeld(t, dn.agent, "GetDnInfo",
		func() error { return ndelDelDn(t, s, dn, false) },
		func() {
			wantCode(t, ndelDelDn(t, s, dn, true), codes.OK,
				"the forced delete during the check")
			if _, err := s.DeleteCluster(context.Background(),
				&pb.DeleteClusterRequest{ClusterName: dn.cluster}); err != nil {
				t.Fatalf("DeleteCluster: %v", err)
			}
			mustCluster(t, s, dn.cluster)
			if _, err := s.CreateDiskNode(context.Background(),
				&pb.CreateDiskNodeRequest{
					ClusterName: dn.cluster,
					AddrPort:    dn.addr,
					NvmeTrConf:  testTrConf(dn.addr),
					Location:    "rack-1",
				}); err != nil {
				t.Fatalf("CreateDiskNode again: %v", err)
			}
		})
	wantCode(t, err, codes.Aborted, "a cluster created again")
	hnodeWantMsg(t, err, "cluster was created again", "a cluster created again")
	reply, getErr := s.GetDiskNode(context.Background(),
		&pb.GetDiskNodeRequest{ClusterName: dn.cluster, AddrPort: dn.addr})
	if getErr != nil {
		t.Fatalf("the node created again is gone: %v", getErr)
	}
	if reply.GetDnConf().GetDnId() != 1 ||
		reply.GetDnRev().GetRevision() != 1 {
		t.Fatalf("the node created again is dn %d at revision %d, want "+
			"the numbers the agent proved", reply.GetDnConf().GetDnId(),
			reply.GetDnRev().GetRevision())
	}
}

// TestDeleteNodeRefusesTheRequestBeforeAskingTheAgent pins the order: every
// refusal about the request or the store — a stale token, a node that still
// hosts a side or a cntlr, a missing rev key — comes from the planning
// snapshot, before the agent is asked.
func TestDeleteNodeRefusesTheRequestBeforeAskingTheAgent(t *testing.T) {
	s := newTestServer(t)
	cli := newTestClient(t)
	ctx := context.Background()

	dn := ndelMakeDn(t, s, "dnorder")
	_, err := s.DeleteDiskNode(ctx, &pb.DeleteDiskNodeRequest{
		ClusterName: dn.cluster,
		AddrPort:    dn.addr,
		DnRev:       &pb.DnRev{Revision: dn.rev + 1},
	})
	wantCode(t, err, codes.Aborted, "a stale token")
	conf := &pb.DnConf{}
	hnodeGet(t, s, dn.keys[0], conf)
	conf.SidePtrList = []*pb.SidePointer{{SpId: 7, LegId: 8, SideId: 9}}
	mustPut(t, cli, dn.keys[0], conf)
	wantCode(t, ndelDelDn(t, s, dn, false), codes.FailedPrecondition,
		"a busy disk node")
	if err := cli.Delete(ctx, dn.keys[1]); err != nil {
		t.Fatalf("Delete %s: %v", dn.keys[1], err)
	}
	wantCode(t, ndelDelDn(t, s, dn, false), codes.Aborted,
		"a missing dn_rev key")
	if got := dn.agent.callCount("GetDnInfo"); got != 0 {
		t.Errorf("the refused disk node deletes asked the agent %d times",
			got)
	}

	cn := ndelMakeCn(t, s, "cnorder")
	_, err = s.DeleteControllerNode(ctx, &pb.DeleteControllerNodeRequest{
		ClusterName: cn.cluster,
		AddrPort:    cn.addr,
		CnRev:       &pb.CnRev{Revision: cn.rev + 1},
	})
	wantCode(t, err, codes.Aborted, "a stale token")
	cnConf := &pb.CnConf{}
	hnodeGet(t, s, cn.keys[0], cnConf)
	cnConf.CntlrPtrList = []*pb.CntlrPointer{{SpId: 7, CntlrId: 8}}
	mustPut(t, cli, cn.keys[0], cnConf)
	wantCode(t, ndelDelCn(t, s, cn, false), codes.FailedPrecondition,
		"a busy controller node")
	if err := cli.Delete(ctx, cn.keys[1]); err != nil {
		t.Fatalf("Delete %s: %v", cn.keys[1], err)
	}
	wantCode(t, ndelDelCn(t, s, cn, false), codes.Aborted,
		"a missing cn_rev key")
	if got := cn.agent.callCount("GetCnInfo"); got != 0 {
		t.Errorf("the refused controller node deletes asked the agent %d "+
			"times", got)
	}
}
