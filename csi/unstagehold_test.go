package csi

import (
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// This file pins csi.md CSI16's guard of a volume's paths at unstage: the
// paths are dropped only once nothing holds the volume's device, which an
// exclusive open shows, and two tries for one volume never run side by side,
// so a try whose unmount still writes keeps the paths for its data.

// TestNodeUnstageKeepsThePathsWhileTheDeviceIsHeld pins the exclusive open: a
// device the kernel refuses to open exclusively, as while an unmount still
// writes the file system's data or another mount namespace holds a copy of
// the mount, fails the call FAILED_PRECONDITION and keeps every path; once
// the open succeeds the paths go.
func TestNodeUnstageKeepsThePathsWhileTheDeviceIsHeld(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	req := stageReq(mountCap(fsExt4, rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")
	client := f.client().(*common.FakeOsClient)
	held := true
	var opened []string
	client.OpenExclusiveFn = func(_ context.Context, path string) error {
		opened = append(opened, path)
		if held {
			return &os.PathError{Op: "open", Path: path, Err: syscall.EBUSY}
		}
		return nil
	}
	n.mnt = &mounter{oc: client}
	unstage := &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	}
	_, err = n.NodeUnstageVolume(ctx, unstage)
	wantCode(t, err, codes.FailedPrecondition, "unstage of a held device")
	if len(f.paths(subsystemNqnOf(testVolume))) == 0 {
		t.Error("the paths of a held device went")
	}
	if want := devicePath(namespaceUuidOf(testVolume)); len(opened) != 1 ||
		opened[0] != want {
		t.Errorf("opened %v, want %s once", opened, want)
	}
	held = false
	_, err = n.NodeUnstageVolume(ctx, unstage)
	wantCode(t, err, codes.OK, "unstage once nothing holds the device")
	if len(f.paths(subsystemNqnOf(testVolume))) != 0 {
		t.Error("the paths are still there")
	}
}

// TestNodeUnstageRunsOneTryAtATime pins the lock of each volume: while a try
// waits in its unmount, a second try for the same volume is refused ABORTED
// and drops nothing; the first then ends and drops the paths.
func TestNodeUnstageRunsOneTryAtATime(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	req := stageReq(mountCap(fsExt4, rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")
	client := f.client().(*common.FakeOsClient)
	run := client.RunCommandFn
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	client.RunCommandFn = func(ctx context.Context, name string, args []string,
		stdin string) (string, string, int, error) {
		if name == "umount" {
			once.Do(func() { close(entered) })
			<-release
		}
		return run(ctx, name, args, stdin)
	}
	n.mnt = &mounter{oc: client}
	unstage := &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	}
	first := make(chan error, 1)
	go func() {
		_, err := n.NodeUnstageVolume(ctx, unstage)
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first try never reached its unmount")
	}
	second := make(chan error, 1)
	go func() {
		_, err := n.NodeUnstageVolume(ctx, unstage)
		second <- err
	}()
	select {
	case err = <-second:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the second try did not return while the first waited: " +
			"the two ran side by side")
	}
	wantCode(t, err, codes.Aborted, "a second try beside the first")
	if len(f.paths(subsystemNqnOf(testVolume))) == 0 {
		t.Error("the second try dropped the paths")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first try: %v", err)
	}
	if len(f.paths(subsystemNqnOf(testVolume))) != 0 {
		t.Error("the first try left the paths")
	}
}

// TestNodeUnstageAnswersItsEndedCall pins the answer of a try whose call
// ended while it ran, as the kubelet's deadline ends the first try while
// its unmount waits to write: the real client's command returns only once
// the umount process exits, with an error, as the call's end killed it on
// its way out, so the try answers that end, DEADLINE_EXCEEDED, and keeps
// the paths; and a call that
// ends between the unmount and the exclusive open answers its end too, not
// a hold it never read.
func TestNodeUnstageAnswersItsEndedCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unmountErrs makes the fake umount answer an error after it
		// unmounted, as RunCommand does once a child that outlived its call
		// exits, killed by the call's end.
		unmountErrs bool
		want        string
	}{
		{"in the unmount", true, "the call ended while its unmount ran"},
		{"before the open", false, "the call ended before its device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newTestNode(t)
			req := stageReq(mountCap(fsExt4, rwo))
			_, err := n.NodeStageVolume(t.Context(), req)
			wantCode(t, err, codes.OK, "stage")
			client := f.client().(*common.FakeOsClient)
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			run := client.RunCommandFn
			client.RunCommandFn = func(c context.Context, name string,
				args []string, stdin string) (string, string, int, error) {
				if name != "umount" {
					return run(c, name, args, stdin)
				}
				// The unmount outlives the call, as one that waits for IO
				// does.
				<-ctx.Done()
				stdout, stderr, code, err := run(c, name, args, stdin)
				if err == nil && tc.unmountErrs {
					err = c.Err()
				}
				return stdout, stderr, code, err
			}
			client.OpenExclusiveFn = func(c context.Context, _ string) error {
				return c.Err()
			}
			n.mnt = &mounter{oc: client}
			_, err = n.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
				VolumeId:          req.GetVolumeId(),
				StagingTargetPath: req.GetStagingTargetPath(),
			})
			wantCode(t, err, codes.DeadlineExceeded, "unstage whose call ended")
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the answer %q does not say %q", err, tc.want)
			}
			if len(f.paths(subsystemNqnOf(testVolume))) == 0 {
				t.Error("the paths went though the device was never read free")
			}
		})
	}
}

// TestAFailedStageKeepsTheHeldDevicesPaths pins the same guard on the drop a
// stage that fails for good makes (csi.md CSI16): with nothing mounted at
// the staging path but the device still held, as by a mount another mount
// namespace keeps, the paths stay; once nothing holds it, a failed stage
// drops them as before.
func TestAFailedStageKeepsTheHeldDevicesPaths(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	client := f.client().(*common.FakeOsClient)
	held := true
	client.OpenExclusiveFn = func(_ context.Context, path string) error {
		if held {
			return &os.PathError{Op: "open", Path: path, Err: syscall.EBUSY}
		}
		return nil
	}
	n.mnt = &mounter{oc: client}
	f.mountErr = true
	_, err := n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.Internal, "a stage whose mount fails")
	if len(f.paths(nqn)) == 0 {
		t.Error("a failed stage dropped the paths of a device still held")
	}
	held = false
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.Internal, "the stage again")
	if len(f.paths(nqn)) != 0 {
		t.Errorf("paths %v after a failed stage of a free device", f.paths(nqn))
	}
}
