package csi

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// runLoop runs the follower's loop over a scripted event socket until the
// returned stop, which waits for the loop and its listener to end; the
// test's end stops it before the package variables are restored.
func runLoop(t *testing.T, n *node, conn *fakeUevents) (stop func()) {
	t.Helper()
	stubUevents(t, conn)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.follow.run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Errorf("the follower's loop did not end")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// eventually waits until cond holds, or fails the test.
func eventually(t *testing.T, bound time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stagedOnRestart plants what a node holds after a restart of the plugin
// with the test volume staged: the discovery connection to the cdc and a
// path to cn0, which recover reads back.
func stagedOnRestart(t *testing.T, n *node, f *fakeNode) {
	t.Helper()
	f.mu.Lock()
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcA))
	f.addCtrl(subsystemNqnOf(testVolume), mustEndpoint(fakeCn0))
	f.mu.Unlock()
	if err := n.follow.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// connectsTo counts the connects to one endpoint.
func connectsTo(f *fakeNode, ep string) int {
	host, port, _ := strings.Cut(ep, ":")
	return f.count(nvmeBin + " connect -t tcp -a " + host + " -s " + port + " ")
}

// TestFollowerRunsAPassAtStart pins csi.md CSI15: the loop runs one pass at
// its start, so a plugin that restarts after the cdc's change notice it
// missed acts on the log at once, not at the timer.
func TestFollowerRunsAPassAtStart(t *testing.T) {
	n, f := newTestNode(t)
	nqn := subsystemNqnOf(testVolume)
	stagedOnRestart(t, n, f)
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{
		{subNqn: nqn, ep: mustEndpoint(fakeCn0)},
		{subNqn: nqn, ep: mustEndpoint(fakeCn2)},
	}
	f.mu.Unlock()
	runLoop(t, n, newFakeUevents())
	eventually(t, 2*time.Second, "a path to the listed cn2", func() bool {
		return strings.Join(f.paths(nqn), " ") == fakeCn0+" "+fakeCn2
	})
}

// TestFollowerRetriesAFailedPass pins the loop's retry timer (csi.md
// CSI15): a pass that failed is tried again after a delay that doubles, and
// not at once on the add and remove events the failed connect itself makes,
// which the loop does not take; while a change event still starts a pass at
// once, whatever the timer waits for.
func TestFollowerRetriesAFailedPass(t *testing.T) {
	setup := func(t *testing.T) (*node, *fakeNode, *fakeUevents) {
		n, f := newTestNode(t)
		stagedOnRestart(t, n, f)
		conn := newFakeUevents()
		return n, f, conn
	}

	t.Run("no pass per event of a failed connect", func(t *testing.T) {
		setRetryBase(t, 100*time.Millisecond)
		n, f, conn := setup(t)
		f.mu.Lock()
		f.connectErr[fakeCn1] = "Failed to write to /dev/nvme-fabrics: Connection refused"
		f.mu.Unlock()
		// The kernel adds a controller for a connect and removes it when
		// the connect fails.
		f.onCommand = func(name string, args []string) {
			if name == nvmeBin && args[0] == "connect" {
				conn.push(uevent("add", "nvme"))
				conn.push(uevent("remove", "nvme"))
			}
		}
		stop := runLoop(t, n, conn)
		n.follow.trigger()
		time.Sleep(700 * time.Millisecond)
		stop()
		// Passes at the start, at the kick, then 100, 200 and 400 ms
		// apart: at least the timer's first, at most five in 700 ms, and
		// one more allowed for the scheduler's slack.
		if got := connectsTo(f, fakeCn1); got < 3 || got > 6 {
			t.Errorf("%d connects to the refusing cn1 in 700 ms, want 3 to 6: "+
				"a retry after the delay, and no pass per event", got)
		}
	})

	t.Run("the timer retries with no event", func(t *testing.T) {
		setRetryBase(t, 100*time.Millisecond)
		n, f, conn := setup(t)
		// The loop's start pass takes the one refusal; with no kick and no
		// event, only the timer makes the path.
		f.mu.Lock()
		f.refuse[fakeCn1] = 1
		f.mu.Unlock()
		runLoop(t, n, conn)
		eventually(t, time.Second, "the path to cn1 after one refusal", func() bool {
			return len(f.paths(subsystemNqnOf(testVolume))) == 2
		})
	})

	t.Run("a change event is not held back", func(t *testing.T) {
		setRetryBase(t, 5*time.Second)
		n, f, conn := setup(t)
		f.mu.Lock()
		f.connectErr[fakeCn1] = "Failed to write to /dev/nvme-fabrics: Connection refused"
		f.mu.Unlock()
		runLoop(t, n, conn)
		n.follow.trigger()
		eventually(t, time.Second, "the first passes", func() bool {
			return connectsTo(f, fakeCn1) >= 1
		})
		time.Sleep(200 * time.Millisecond)
		before := connectsTo(f, fakeCn1)
		conn.push(uevent("change", "nvme", "NVME_AEN=0x70f002"))
		eventually(t, time.Second, "a pass on the cdc's change notice", func() bool {
			return connectsTo(f, fakeCn1) > before
		})
	})
}

// TestFollowerRetriesAPassWhoseLogReadFailed pins csi.md CSI15: a pass
// that could not read a cdc's log is a failed pass, which holds the drops
// and arms the retry timer, so the withdrawn path goes once the log reads,
// with no event and no kick.
func TestFollowerRetriesAPassWhoseLogReadFailed(t *testing.T) {
	setRetryBase(t, 100*time.Millisecond)
	n, f := newTestNode(t)
	nqn := subsystemNqnOf(testVolume)
	stagedOnRestart(t, n, f)
	reads := 0
	f.mu.Lock()
	f.addCtrl(nqn, mustEndpoint(fakeCn1))
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
	f.logFail[fakeCdcA] = true
	f.mu.Unlock()
	// The first read fails; the cdc answers from the second on.
	f.onCommand = func(name string, args []string) {
		if name != nvmeBin || args[0] != "discover" || argument(args, "-d") == "" {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if reads++; reads == 2 {
			f.logFail[fakeCdcA] = false
		}
	}
	runLoop(t, n, newFakeUevents())
	eventually(t, time.Second, "the drop of the withdrawn cn0 after a failed log read",
		func() bool { return strings.Join(f.paths(nqn), " ") == fakeCn1 })
}

// TestFollowerDropsEveryDiscoveryConnection pins csi.md CSI16: the last
// unstage drops every discovery connection, going on past one whose drop
// fails, and answers that failure, so the kubelet calls the unstage again.
// The drop that fails is the first one the unstage meets, whichever cdc the
// stage connected first, so a drop that stopped at the first failure would
// leave the other one too.
func TestFollowerDropsEveryDiscoveryConnection(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	n.follow = newFollower(n.host, time.Hour,
		[]endpoint{mustEndpoint(fakeCdcA), mustEndpoint(fakeCdcB)})
	f.mu.Lock()
	f.logs[fakeCdcB] = f.logs[fakeCdcA]
	f.mu.Unlock()
	req := stageReq(blockCap(rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage with two cdcs")
	if got := f.paths(common.NvmeDiscoveryNqn); len(got) != 2 {
		t.Fatalf("discovery connections %v, want both cdcs", got)
	}
	// The node's controller listing is in name order (ctrlNames), the
	// order the unstage meets them in.
	var failing, failingCdc string
	f.mu.Lock()
	for _, name := range f.ctrlNames() {
		if c := f.ctrls[name]; c.subNqn == common.NvmeDiscoveryNqn {
			failing, failingCdc = name, c.key()
			break
		}
	}
	f.disconnectFail[failing] = true
	f.mu.Unlock()
	unstage := &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	}
	_, err = n.NodeUnstageVolume(ctx, unstage)
	if err == nil {
		t.Error("the last unstage answered OK with a discovery connection left")
	}
	if got := f.paths(nqn); len(got) != 0 {
		t.Errorf("paths %v after the unstage, want none", got)
	}
	if got := f.paths(common.NvmeDiscoveryNqn); strings.Join(got, " ") != failingCdc {
		t.Errorf("discovery connections %v, want the failing one to %s alone left",
			got, failingCdc)
	}
	// The kubelet's repeat drops it once the disconnect works.
	f.mu.Lock()
	f.disconnectFail[failing] = false
	f.mu.Unlock()
	_, err = n.NodeUnstageVolume(ctx, unstage)
	wantCode(t, err, codes.OK, "the repeated unstage")
	if got := f.paths(common.NvmeDiscoveryNqn); len(got) != 0 {
		t.Errorf("discovery connections %v after the repeat, want none", got)
	}
}

// TestFollowerRetriesACntlrTheStageLeft pins csi.md CSI14: a stage that
// ends its connects holding a path while another cntlr still refuses hands
// that cntlr to the follower's loop at once, whose retry timer connects it
// within the stage's wait for the device, here the path to the primary.
func TestFollowerRetriesACntlrTheStageLeft(t *testing.T) {
	setRetryBase(t, 100*time.Millisecond)
	n, f := newTestNode(t)
	f.mu.Lock()
	f.optimized = fakeCn1
	f.refuse[fakeCn1] = 3
	f.mu.Unlock()
	runLoop(t, n, newFakeUevents())
	// The loop's start pass, with nothing staged, is over before the stage.
	time.Sleep(200 * time.Millisecond)
	_, err := n.NodeStageVolume(t.Context(), stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "a stage whose primary refuses three connects")
	if got := f.paths(subsystemNqnOf(testVolume)); len(got) != 2 {
		t.Errorf("paths %v, want both cntlrs", got)
	}
}

// TestFollowerPassesAgainWhileANewPathJoins pins csi.md CSI15: a pass that
// holds a withdrawn path's drop only because the listed path shows no
// namespace yet arms the retry timer, since the kernel sends no event when
// the namespace shows; the withdrawn path goes once it does.
func TestFollowerPassesAgainWhileANewPathJoins(t *testing.T) {
	setRetryBase(t, 100*time.Millisecond)
	n, f := newTestNode(t)
	nqn := subsystemNqnOf(testVolume)
	stagedOnRestart(t, n, f)
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn2)}}
	f.noNs[fakeCn2] = true
	f.mu.Unlock()
	runLoop(t, n, newFakeUevents())
	n.follow.trigger()
	eventually(t, 2*time.Second, "the path to cn2", func() bool {
		return ctrlTo(f, nqn, fakeCn2) != ""
	})
	time.Sleep(300 * time.Millisecond)
	if got := f.paths(nqn); len(got) != 2 {
		t.Fatalf("paths %v while cn2 shows no namespace, want cn0 kept", got)
	}
	f.mu.Lock()
	f.noNs[fakeCn2] = false
	f.mu.Unlock()
	eventually(t, 3*time.Second, "the drop of the withdrawn cn0", func() bool {
		return strings.Join(f.paths(nqn), " ") == fakeCn2
	})
}

// TestFollowerFollowsOnlyTheDeploymentsCdcs pins csi.md CSI15: the node
// follows the cdcs of its configuration and no other; a cdc an attach's
// context names is not followed, so one that is down holds no drop, and a
// context with no cdc stages as well.
func TestFollowerFollowsOnlyTheDeploymentsCdcs(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	f.mu.Lock()
	f.discFail[fakeCdcB] = true
	f.mu.Unlock()
	req := stageReq(blockCap(rwo))
	req.PublishContext[ctxCdc] = fakeCdcA + "," + fakeCdcB
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "a stage whose context names a dead cdc")
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn0)}}
	f.mu.Unlock()
	if err := syncDrained(ctx, n); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn0 {
		t.Errorf("paths %v after the cdc withdrew cn1, want cn0 alone", got)
	}
	if got := f.count(nvmeBin + " discover -t tcp -a 10.0.0.79"); got != 0 {
		t.Errorf("%d discovery connects to the cdc the context named", got)
	}
	// A context that names no cdc.
	other := stageReqFor("pvc-other", blockCap(rwo))
	delete(other.PublishContext, ctxCdc)
	f.mu.Lock()
	f.uuidOf[subsystemNqnOf("pvc-other")] = namespaceUuidOf("pvc-other")
	f.logs[fakeCdcA] = append(f.logs[fakeCdcA],
		logEntry{subNqn: subsystemNqnOf("pvc-other"), ep: mustEndpoint(fakeCn1)})
	f.mu.Unlock()
	_, err = n.NodeStageVolume(ctx, other)
	wantCode(t, err, codes.OK, "a stage whose context names no cdc")
}

// TestNvmeCommandsHaveADeadline pins csi.md CSI14: the connects and the two
// discovery reads run under the agents' command bound, and no disconnect
// does, the unstage's, a path drop's and the loop's stop's alike, since the
// kernel's delete runs to its end whatever the signal.
func TestNvmeCommandsHaveADeadline(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	req := stageReq(blockCap(rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
	f.mu.Unlock()
	if err := syncDrained(ctx, n); err != nil {
		t.Fatalf("sync: %v", err)
	}
	_, err = n.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	})
	wantCode(t, err, codes.OK, "unstage")
	// The loop's stop with nothing staged drops a discovery connection left
	// over, with no deadline either.
	f.mu.Lock()
	left := f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcA))
	f.mu.Unlock()
	stop := runLoop(t, n, newFakeUevents())
	stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.calls, nvmeBin+" disconnect -d "+left) {
		t.Errorf("the loop's stop did not drop the discovery connection %s", left)
	}
	seen := map[string]int{}
	for _, d := range f.deadlines {
		call, left, _ := strings.Cut(d, " | ")
		kind := strings.Fields(call)[0]
		if kind == "discover" && strings.Contains(call, " -d ") {
			kind = "discover -d"
		}
		seen[kind]++
		switch kind {
		case "disconnect":
			if left != "none" {
				t.Errorf("%s runs under a deadline, %s left", call, left)
			}
		default:
			l, err := time.ParseDuration(left)
			if err != nil || l <= 0 || l > common.CmdSoftTimeout*time.Second {
				t.Errorf("%s runs with %s left, want a deadline within %ds",
					call, left, common.CmdSoftTimeout)
			}
		}
	}
	for _, kind := range []string{"connect", "discover", "discover -d", "disconnect"} {
		if seen[kind] == 0 {
			t.Errorf("no %s ran: %v", kind, f.deadlines)
		}
	}
}

// TestConnectCountsOnlyALivePath pins csi.md CSI14: a connect that fails
// counts as made only when sysfs shows the path, a controller of the
// subsystem at the cntlr's address under the node's host NQN that is not
// being deleted, whatever nvme-cli printed.
func TestConnectCountsOnlyALivePath(t *testing.T) {
	refused := "Failed to write to /dev/nvme-fabrics: Connection refused"
	for _, tc := range []struct {
		name  string
		nqn   string
		setup func(f *fakeNode, nqn string)
		made  bool
	}{
		{"a refusal of a subsystem whose NQN holds the word",
			subsystemNqnOf("already-there"), func(f *fakeNode, nqn string) {
				f.connectErr[fakeCn0] = refused
			}, false},
		{"already connected, to a controller being deleted",
			subsystemNqnOf(testVolume), func(f *fakeNode, nqn string) {
				name := f.addCtrl(nqn, mustEndpoint(fakeCn0))
				c := f.ctrls[name]
				c.state = ctrlStateDeleting
				f.ctrls[name] = c
			}, false},
		{"already connected, under another host NQN",
			subsystemNqnOf(testVolume), func(f *fakeNode, nqn string) {
				f.addCtrlAs(nqn, mustEndpoint(fakeCn0), testNodeB)
			}, false},
		{"already connected, to another subsystem's controller",
			subsystemNqnOf(testVolume), func(f *fakeNode, nqn string) {
				f.connectErr[fakeCn0] = "already connected"
				f.addCtrl(subsystemNqnOf("pvc-other"), mustEndpoint(fakeCn0))
			}, false},
		{"a refusal while sysfs shows the live path",
			subsystemNqnOf(testVolume), func(f *fakeNode, nqn string) {
				f.connectErr[fakeCn0] = refused
				f.addCtrl(nqn, mustEndpoint(fakeCn0))
			}, true},
		{"already connected, to a path that reconnects",
			subsystemNqnOf(testVolume), func(f *fakeNode, nqn string) {
				name := f.addCtrl(nqn, mustEndpoint(fakeCn0))
				c := f.ctrls[name]
				c.state = "connecting"
				f.ctrls[name] = c
			}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newTestNode(t)
			f.mu.Lock()
			tc.setup(f, tc.nqn)
			f.mu.Unlock()
			err := n.host.connect(t.Context(), tc.nqn, mustEndpoint(fakeCn0))
			if made := err == nil; made != tc.made {
				t.Errorf("connect = %v, want made %v", err, tc.made)
			}
		})
	}
}
