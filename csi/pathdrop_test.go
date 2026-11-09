package csi

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// stageReqFor is stageReq for another volume of the same cdc.
func stageReqFor(name string, cap *csi.VolumeCapability) *csi.NodeStageVolumeRequest {
	req := stageReq(cap)
	req.VolumeId = fakeClusterOne + "/" + name
	req.StagingTargetPath = "/var/lib/kubelet/plugins/kubernetes.io/csi/staging/" +
		name
	req.PublishContext[ctxNqn] = subsystemNqnOf(name)
	req.PublishContext[ctxUuid] = namespaceUuidOf(name)
	return req
}

// Every path the driver makes reconnects without end, as the agents' paths
// do (dnagent.md SH20; csi.md CSI14).
func TestEveryConnectReconnectsWithoutEnd(t *testing.T) {
	n, f := newTestNode(t)
	_, err := n.NodeStageVolume(t.Context(), stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")
	f.mu.Lock()
	defer f.mu.Unlock()
	connects := 0
	for _, call := range f.calls {
		if !strings.HasPrefix(call, nvmeBin+" connect ") {
			continue
		}
		connects++
		if !strings.Contains(call, " --ctrl-loss-tmo -1") {
			t.Errorf("a connect without the unlimited reconnect: %s", call)
		}
	}
	if connects == 0 {
		t.Fatalf("the stage made no connect")
	}
}

// A path the cdc withdrew stays while the node holds no path to a listed
// cntlr whose namespace has joined: while the cntlr that replaced it refuses
// the connect, in the pass that connects it, and while that path shows no
// namespace yet; it goes once one does; and when the cdc lists no cntlr for
// the subsystem at all, as after a detach, the paths go (csi.md CSI15).
func TestAWithdrawnPathStaysUntilAListedOneIsHeld(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn0)}}
	f.mu.Unlock()
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")
	paths := func(want, why string) {
		t.Helper()
		if got := strings.Join(f.paths(nqn), " "); got != want {
			t.Fatalf("paths %q %s, want %q", got, why, want)
		}
	}

	// A cntlr replacement: cn2 listed instead of cn0, and cn2 not built yet.
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn2)}}
	f.refuse[fakeCn2] = 1
	f.noNs[fakeCn2] = true
	f.mu.Unlock()
	_ = syncDrained(ctx, n)
	paths(fakeCn0, "while cn2 refuses")
	if err := syncDrained(ctx, n); err != nil {
		t.Fatalf("sync: %v", err)
	}
	paths(fakeCn0+" "+fakeCn2, "in the pass that connects cn2")
	if err := syncDrained(ctx, n); err != nil {
		t.Fatalf("sync: %v", err)
	}
	paths(fakeCn0+" "+fakeCn2, "while cn2's path shows no namespace")
	f.mu.Lock()
	f.noNs[fakeCn2] = false
	f.mu.Unlock()
	if err := syncDrained(ctx, n); err != nil {
		t.Fatalf("sync: %v", err)
	}
	paths(fakeCn2, "once cn2's path shows the namespace")

	// The cdc lists no cntlr for the subsystem: every path goes.
	f.mu.Lock()
	f.logs[fakeCdcA] = nil
	f.mu.Unlock()
	if err := syncDrained(ctx, n); err != nil {
		t.Fatalf("sync: %v", err)
	}
	paths("", "when the cdc lists none")
}

// blockDrop makes the disconnect of one controller hang until the returned
// release is called, and reports on entered when it starts; the test's end
// releases it in any case.
func blockDrop(t *testing.T, f *fakeNode, name string) (
	entered chan struct{}, release func(),
) {
	entered = make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	f.onCommand = func(cmd string, args []string) {
		if cmd == nvmeBin && args[0] == "disconnect" &&
			argument(args, "-d") == name {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
		}
	}
	return entered, release
}

// within runs fn and fails the test unless it returns within the wait.
func within(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: it waited for a hung drop", what)
	}
	return nil
}

// ctrlTo is the name of the node's controller to one endpoint for one
// subsystem.
func ctrlTo(f *fakeNode, nqn string, ep string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, c := range f.ctrls {
		if c.subNqn == nqn && c.key() == ep {
			return name
		}
	}
	return ""
}

// A drop that hangs, as an `nvme disconnect` of a vanished target does,
// runs outside the follower's lock: the pass that began it returns, another
// volume's stage and unstage go on meanwhile, and the unstage of the dropped
// path's volume waits for the drop.
func TestABlockedDropHoldsUpNoOtherVolume(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")

	// cn0 withdrawn; its disconnect hangs until released.
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
	f.mu.Unlock()
	entered, release := blockDrop(t, f, ctrlTo(f, nqn, fakeCn0))
	if err := within(t, "the pass", func() error {
		return n.follow.sync(ctx)
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("the drop of cn0 never started")
	}

	// Another volume stages and unstages while the drop hangs.
	other := "vol-other"
	f.mu.Lock()
	f.uuidOf[subsystemNqnOf(other)] = namespaceUuidOf(other)
	f.sizes[devicePath(namespaceUuidOf(other))] = 10 << 30
	f.logs[fakeCdcA] = append(f.logs[fakeCdcA],
		logEntry{subNqn: subsystemNqnOf(other), ep: mustEndpoint(fakeCn1)})
	f.mu.Unlock()
	err = within(t, "the other volume's stage", func() error {
		_, err := n.NodeStageVolume(ctx, stageReqFor(other, blockCap(rwo)))
		return err
	})
	wantCode(t, err, codes.OK, "the other volume's stage")
	if err := within(t, "the other volume's unstage", func() error {
		return n.follow.unstage(ctx, subsystemNqnOf(other))
	}); err != nil {
		t.Fatalf("the other volume's unstage: %v", err)
	}

	// The unstage of the first volume waits for its drop.
	unstaged := make(chan error, 1)
	go func() {
		unstaged <- n.follow.unstage(ctx, nqn)
	}()
	select {
	case err := <-unstaged:
		t.Fatalf("the unstage returned (%v) while its drop still ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-unstaged:
		if err != nil {
			t.Fatalf("unstage: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the unstage never returned after the drop ended")
	}
	if got := f.paths(nqn); len(got) != 0 {
		t.Fatalf("paths %v after the unstage, want none", got)
	}
}

// A listed cntlr whose path is being dropped is not a path the node holds:
// while cn0's drop hangs and the cdc lists cn0 again and withdraws cn1, cn1
// stays, since cn0's path is going.
func TestAPathBeingDroppedIsNotHeld(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
	f.mu.Unlock()
	entered, _ := blockDrop(t, f, ctrlTo(f, nqn, fakeCn0))
	if err := n.follow.sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	<-entered
	cn1 := ctrlTo(f, nqn, fakeCn1)
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn0)}}
	f.mu.Unlock()
	if err := n.follow.sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// A drop the pass began is registered before the pass returns, and
	// done once the controller is gone.
	n.follow.mu.Lock()
	_, going := n.follow.dropping[cn1]
	n.follow.mu.Unlock()
	if going || ctrlTo(f, nqn, fakeCn1) == "" {
		t.Fatalf("cn1 is dropped while the only listed path is going")
	}
}

// A stage of a volume whose unstage still drops its paths waits for that
// unstage, so it does not connect paths the unstage then drops.
func TestAStageWaitsForTheVolumesUnstage(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	f.onCommand = func(cmd string, args []string) {
		if cmd == nvmeBin && args[0] == "disconnect" &&
			argument(args, "-n") == nqn {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
		}
	}
	unstaged := make(chan error, 1)
	go func() { unstaged <- n.follow.unstage(ctx, nqn) }()
	<-entered
	staged := make(chan error, 1)
	go func() {
		staged <- n.follow.stage(ctx, nqn, []endpoint{mustEndpoint(fakeCdcA)})
	}()
	select {
	case err := <-staged:
		t.Fatalf("the stage returned (%v) while the unstage still ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-unstaged; err != nil {
		t.Fatalf("unstage: %v", err)
	}
	if err := <-staged; err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(f.paths(nqn)) == 0 {
		t.Fatalf("the stage after the unstage holds no path")
	}
}
