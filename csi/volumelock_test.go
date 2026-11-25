package csi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// This file pins the node plugin's answer to commands that wait for ever
// (csi.md CSI16): a stage or an unstage of a volume whose command waits for
// IO keeps the volume, so the kubelet's next tries of either are refused at
// once and take no command slot, and the follower's commands have slots of
// their own.

// TestAStageWhoseCommandWaitsKeepsTheVolume pins the lock over a volume's
// stage: while a stage waits in blkid, as one does on a device whose IO
// waits, a second stage and an unstage of the volume are ABORTED and run no
// blkid; a stage of another volume runs; once the first ends, the volume
// stages and unstages as before.
func TestAStageWhoseCommandWaitsKeepsTheVolume(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	req := stageReq(mountCap(fsExt4, rwo))
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.onCommand = func(name string, args []string) {
		if name == "blkid" && strings.Contains(args[len(args)-1], namespaceUuidOf(testVolume)) {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	first := make(chan error, 1)
	go func() {
		_, err := n.NodeStageVolume(ctx, req)
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first stage never reached blkid")
	}
	blkids := f.count("blkid ")

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"a second stage", func() error {
			_, err := n.NodeStageVolume(ctx, req)
			return err
		}},
		{"an unstage", func() error {
			_, err := n.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
				VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
			})
			return err
		}},
	} {
		err := within(t, call.name, call.run)
		wantCode(t, err, codes.Aborted, call.name+" while the stage waits")
		if !strings.Contains(err.Error(), "another stage") {
			t.Errorf("%s: %v does not name the stage that runs", call.name, err)
		}
	}
	if got := f.count("blkid "); got != blkids {
		t.Errorf("%d blkid runs after the refused tries, want %d", got, blkids)
	}

	other := "vol-other"
	f.mu.Lock()
	f.uuidOf[subsystemNqnOf(other)] = namespaceUuidOf(other)
	f.sizes[devicePath(namespaceUuidOf(other))] = 10 << 30
	f.logs[fakeCdcA] = append(f.logs[fakeCdcA],
		logEntry{subNqn: subsystemNqnOf(other), ep: mustEndpoint(fakeCn1)})
	f.mu.Unlock()
	err := within(t, "another volume's stage", func() error {
		_, err := n.NodeStageVolume(ctx, stageReqFor(other, blockCap(rwo)))
		return err
	})
	wantCode(t, err, codes.OK, "another volume's stage while the first waits")

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first stage: %v", err)
	}
	_, err = n.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	})
	wantCode(t, err, codes.OK, "the unstage once the stage ended")
}

// TestTheFollowerHasSlotsOfItsOwn pins the node plugin's two OS clients: a
// node plugin built with no client of a test's gets one for its mount side
// and another for its NVMe side, the follower's among them, each with slots
// of its own, so commands that wait for ever in the first leave the
// follower's connects free; one a test passes serves both.
func TestTheFollowerHasSlotsOfItsOwn(t *testing.T) {
	oc, foc := nodeClients(Config{})
	if oc == foc {
		t.Fatal("the stage's and the follower's OS clients are one")
	}
	full := oc.(*common.LimitedOsClient)
	free := foc.(*common.LimitedOsClient)
	// Every slot of the stage's client held by a command that waits.
	hold, stop := context.WithCancel(context.Background())
	var held sync.WaitGroup
	for range common.DefaultOsClientLimit {
		held.Add(1)
		go func() {
			defer held.Done()
			_, _, _, _ = full.RunCommand(hold, "sleep", []string{"30"}, "")
		}()
	}
	defer func() {
		stop()
		held.Wait()
	}()
	// The stage's client has no slot left once they all run.
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, cancelProbe := context.WithTimeout(t.Context(), 50*time.Millisecond)
		_, _, _, err := full.RunCommand(probe, "true", nil, "")
		cancelProbe()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stage's client still had a free slot")
		}
	}
	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, _, err := free.RunCommand(short, "true", nil, ""); err != nil {
		t.Errorf("the follower's command found no slot while the stage's were "+
			"all held: %v", err)
	}
	one := &common.FakeOsClient{}
	oc, foc = nodeClients(Config{OsClient: one})
	if oc != common.OsClient(one) || foc != common.OsClient(one) {
		t.Error("a test's OS client does not serve both")
	}
}
