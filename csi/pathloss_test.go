package csi

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

// This file pins three ways a node could lose the paths of a mounted volume
// that the follower guards against (csi.md CSI14, CSI15): a drop by a
// controller name the kernel gave to another connection meanwhile, an answer
// of a cdc that does not hold the cdc's own record, and a path whose
// reconnects give up, as one nvme-cli's own connect unit makes.

// TestADropReadsItsControllerAgain pins the re-read before a drop: a drop
// decided for cn0's path waits for a drop slot; meanwhile the kernel deletes
// that controller and gives its name to another path, of another volume or
// of the same volume at another cntlr; the drop then finds the name no
// longer names cn0's path, and disconnects nothing. Each of the subsystem
// and the address alone tells the paths apart.
func TestADropReadsItsControllerAgain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		otherVolume   bool
		otherEndpoint bool
	}{
		{"another volume at the same cntlr", true, false},
		{"the same volume at another cntlr", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newTestNode(t)
			ctx := t.Context()
			nqn := subsystemNqnOf(testVolume)
			_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
			wantCode(t, err, codes.OK, "stage")
			name := ctrlTo(f, nqn, fakeCn0)
			if name == "" {
				t.Fatal("the stage made no path to cn0")
			}

			// Every drop slot taken, so the drop the pass begins waits.
			for range dropConcurrency {
				n.follow.dropSlots <- struct{}{}
			}
			f.mu.Lock()
			f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
			f.mu.Unlock()
			if err := n.follow.sync(ctx); err != nil {
				t.Fatalf("sync: %v", err)
			}
			n.follow.mu.Lock()
			_, going := n.follow.dropping[name]
			n.follow.mu.Unlock()
			if !going {
				t.Fatalf("the pass began no drop of cn0's path %s", name)
			}

			// The kernel ends cn0's path and gives its name to another.
			f.mu.Lock()
			c := f.ctrls[name]
			if tc.otherVolume {
				c.subNqn = subsystemNqnOf("vol-other")
			}
			if tc.otherEndpoint {
				ep := mustEndpoint(fakeCn2)
				c.trAddr, c.trSvcId = ep.trAddr, ep.trSvcId
			}
			f.ctrls[name] = c
			f.mu.Unlock()

			<-n.follow.dropSlots
			if err := within(t, "the drop", func() error {
				return n.follow.waitDrops(ctx, nqn)
			}); err != nil {
				t.Fatalf("waitDrops: %v", err)
			}
			for range dropConcurrency - 1 {
				<-n.follow.dropSlots
			}
			if got := f.count(nvmeBin + " disconnect -d " + name); got != 0 {
				t.Errorf("%d disconnects of %s, which names another path now", got, name)
			}
			f.mu.Lock()
			_, still := f.ctrls[name]
			f.mu.Unlock()
			if !still {
				t.Errorf("the other path %s went", name)
			}
		})
	}
}

// TestAnAnswerWithoutTheCdcsOwnRecordDropsNothing pins the answer that
// counts: a cdc's log that does not hold the cdc's own record, an empty list
// or a list of other records, is not an answer that the cdc lists no cntlr,
// so the volume keeps every path and the pass fails, to be tried again; once
// the cdc answers with its own record again, a withdrawn path goes as
// before.
func TestAnAnswerWithoutTheCdcsOwnRecordDropsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		empty bool
	}{
		{"an empty list", true},
		{"a list without the cdc's record", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newTestNode(t)
			ctx := t.Context()
			nqn := subsystemNqnOf(testVolume)
			_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
			wantCode(t, err, codes.OK, "stage")
			before := f.paths(nqn)
			if len(before) != 2 {
				t.Fatalf("paths after the stage %v, want two", before)
			}

			f.mu.Lock()
			f.logs[fakeCdcA] = nil
			f.noSelf[fakeCdcA] = true
			f.emptyLog[fakeCdcA] = tc.empty
			f.mu.Unlock()
			err = n.follow.sync(ctx)
			if err == nil || !strings.Contains(err.Error(), "no record of the cdc itself") {
				t.Errorf("sync over the answer = %v, want its refusal", err)
			}
			if err := within(t, "the drops", func() error {
				return n.follow.waitDrops(ctx, "")
			}); err != nil {
				t.Fatalf("waitDrops: %v", err)
			}
			if got := f.paths(nqn); len(got) != len(before) {
				t.Fatalf("paths after the answer %v, want %v", got, before)
			}

			// The positive control: an answer with the cdc's record that
			// withdraws cn0 drops cn0's path.
			f.mu.Lock()
			f.noSelf[fakeCdcA] = false
			f.emptyLog[fakeCdcA] = false
			f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
			f.mu.Unlock()
			if err := n.follow.sync(ctx); err != nil {
				t.Fatalf("sync: %v", err)
			}
			if err := within(t, "the drops", func() error {
				return n.follow.waitDrops(ctx, "")
			}); err != nil {
				t.Fatalf("waitDrops: %v", err)
			}
			if got := f.paths(nqn); len(got) != 1 || got[0] != fakeCn1 {
				t.Errorf("paths after cn0 was withdrawn %v, want cn1 alone", got)
			}
		})
	}
}

// TestAPathWithALossLimitGetsItLifted pins the limit of a path the plugin
// did not make: a path of a staged volume whose reconnects give up after
// 600 seconds, as nvme-cli's own connect unit makes it, is set to reconnect
// without end at the next pass; one the plugin made is left as it is.
func TestAPathWithALossLimitGetsItLifted(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")
	theirs := ctrlTo(f, nqn, fakeCn1)
	ours := ctrlTo(f, nqn, fakeCn0)
	f.mu.Lock()
	f.lossTmo[theirs] = "600"
	f.mu.Unlock()

	if err := within(t, "the pass", func() error {
		return n.follow.sync(ctx)
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.mu.Lock()
	got, kept := f.lossTmo[theirs], f.lossTmo[ours]
	f.mu.Unlock()
	if got != lossTmoOff {
		t.Errorf("the limit of %s after a pass = %q, want %q", theirs, got, lossTmoOff)
	}
	if kept != "" {
		t.Errorf("the plugin's own path %s was written: %q", ours, kept)
	}

	// A limit the plugin cannot lift is the pass's failure, and the limit
	// stays; so is one it cannot read.
	f.mu.Lock()
	f.lossTmo[theirs] = "600"
	f.writeFail[sysClassNvme+"/"+theirs+"/ctrl_loss_tmo"] = errDenied
	f.mu.Unlock()
	if err := n.follow.sync(ctx); err == nil {
		t.Error("a pass whose lift failed returned no error")
	}
	f.mu.Lock()
	got = f.lossTmo[theirs]
	delete(f.writeFail, sysClassNvme+"/"+theirs+"/ctrl_loss_tmo")
	f.readFail[sysClassNvme+"/"+theirs+"/ctrl_loss_tmo"] = errDenied
	f.mu.Unlock()
	if got != "600" {
		t.Errorf("the limit after a failed lift = %q, want it kept at 600", got)
	}
	if err := n.follow.sync(ctx); err == nil {
		t.Error("a pass that could not read a limit returned no error")
	}
}

// TestALiftReadsItsControllerAgain pins the re-read before a lift: the
// pass's list is read before its log reads, and when the kernel gives a
// limited path's name to another tool's connection meanwhile, here during
// the log read, the lift writes nothing.
func TestALiftReadsItsControllerAgain(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")
	theirs := ctrlTo(f, nqn, fakeCn1)
	f.mu.Lock()
	f.lossTmo[theirs] = "600"
	f.mu.Unlock()
	f.onCommand = func(name string, args []string) {
		if name != nvmeBin || len(args) < 2 || args[0] != "discover" || args[1] != "-d" {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.ctrls[theirs]
		c.subNqn = "nqn.2014-08.org.example:another-tool"
		f.ctrls[theirs] = c
	}
	if err := within(t, "the pass", func() error {
		return n.follow.sync(ctx)
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.mu.Lock()
	got := f.lossTmo[theirs]
	f.mu.Unlock()
	if got != "600" {
		t.Errorf("the limit of %s, now another tool's connection, = %q, "+
			"want it kept at 600", theirs, got)
	}
}

// errDenied is a sysfs access the kernel refuses.
var errDenied = &deniedError{}

type deniedError struct{}

func (*deniedError) Error() string { return "permission denied" }
