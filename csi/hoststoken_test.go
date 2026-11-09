package csi

import (
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// Two attaches of one volume to two nodes that overlap: the second lands
// between the first's read of the hosts and its write. The first's write
// carries the revision it read before, so the gateway refuses it as stale;
// its next attempt finds the other node and is FAILED_PRECONDITION, and the
// other node keeps the volume (csi.md CSI10).
func TestOverlappingAttachesLeaveOneNode(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	publish := func(node string) error {
		_, err := c.ControllerPublishVolume(ctx,
			&csi.ControllerPublishVolumeRequest{
				VolumeId: id, NodeId: node,
				VolumeCapability: mountCap(fsExt4, rwo),
			})
		return err
	}
	var otherErr error
	g.mu.Lock()
	g.afterList = func() { otherErr = publish(testNodeB) }
	g.mu.Unlock()
	err = publish(testNodeA)
	wantCode(t, otherErr, codes.OK, "the attach to B in between")
	wantCode(t, err, codes.FailedPrecondition, "the overlapping attach to A")
	if !strings.Contains(err.Error(), testNodeB) {
		t.Errorf("the refusal does not name B: %v", err)
	}
	hosts := g.pool(fakeClusterOne, testVolume).
		ss[subsystemNqnOf(testVolume)].GetAllowedHosts()
	if len(hosts) != 1 || hosts[0] != testNodeB {
		t.Fatalf("allowed hosts %v, want [%s]", hosts, testNodeB)
	}
}

// A detach of node A that runs late, after A was detached and the volume
// attached to B in between: its write carries the revision it read before
// those, so it is refused as stale, and its next attempt finds A gone and
// leaves B's attach alone (csi.md CSI10).
func TestAStaleDetachLeavesTheNextAttach(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	_, err = c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: testNodeA,
		VolumeCapability: mountCap(fsExt4, rwo),
	})
	wantCode(t, err, codes.OK, "attach to A")
	unpublish := func(node string) error {
		_, err := c.ControllerUnpublishVolume(ctx,
			&csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: node})
		return err
	}
	var betweenErr error
	g.mu.Lock()
	g.afterList = func() {
		if betweenErr = unpublish(testNodeA); betweenErr != nil {
			return
		}
		_, betweenErr = c.ControllerPublishVolume(ctx,
			&csi.ControllerPublishVolumeRequest{
				VolumeId: id, NodeId: testNodeB,
				VolumeCapability: mountCap(fsExt4, rwo),
			})
	}
	g.mu.Unlock()
	err = unpublish(testNodeA)
	wantCode(t, betweenErr, codes.OK, "the detach of A and attach to B in between")
	wantCode(t, err, codes.OK, "the late detach of A")
	hosts := g.pool(fakeClusterOne, testVolume).
		ss[subsystemNqnOf(testVolume)].GetAllowedHosts()
	if len(hosts) != 1 || hosts[0] != testNodeB {
		t.Fatalf("allowed hosts %v, want [%s]", hosts, testNodeB)
	}
}

// The refusals of a host write: a stale revision runs the attempt again, up
// to hostsAttempts, and then the call is ABORTED, a detach included, which
// must never answer done while its node stays in the list; any other
// refusal, an ABORTED of another cause among them, is passed on after one
// write with the gateway's own message.
func TestHostWriteRefusals(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	publish := func() error {
		_, err := c.ControllerPublishVolume(ctx,
			&csi.ControllerPublishVolumeRequest{
				VolumeId: id, NodeId: testNodeA,
				VolumeCapability: mountCap(fsExt4, rwo),
			})
		return err
	}
	writes := func() int { return g.methodCalls("UpdateSubsystemHosts") }

	before := writes()
	g.mu.Lock()
	g.fail = map[string]codes.Code{"UpdateSubsystemHosts": codes.Unavailable}
	g.mu.Unlock()
	wantCode(t, publish(), codes.Unavailable, "a write the gateway cannot take")
	if n := writes() - before; n != 1 {
		t.Errorf("%d writes for an UNAVAILABLE, want 1", n)
	}

	before = writes()
	g.mu.Lock()
	g.fail["UpdateSubsystemHosts"] = codes.Aborted
	g.mu.Unlock()
	err = publish()
	wantCode(t, err, codes.Aborted, "an ABORTED of another cause")
	if n := writes() - before; n != 1 || !strings.Contains(err.Error(),
		"injected") {
		t.Errorf("%d writes, error %v; want one write and the gateway's "+
			"message", n, err)
	}

	before = writes()
	g.mu.Lock()
	delete(g.fail, "UpdateSubsystemHosts")
	g.alwaysStale = true
	g.mu.Unlock()
	wantCode(t, publish(), codes.Aborted, "every write stale")
	if n := writes() - before; n != hostsAttempts {
		t.Errorf("%d writes, want %d", n, hostsAttempts)
	}

	g.mu.Lock()
	g.alwaysStale = false
	g.mu.Unlock()
	wantCode(t, publish(), codes.OK, "attach")
	g.mu.Lock()
	g.alwaysStale = true
	g.mu.Unlock()
	_, err = c.ControllerUnpublishVolume(ctx,
		&csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: testNodeA})
	wantCode(t, err, codes.Aborted, "a detach whose every write is stale")
}
