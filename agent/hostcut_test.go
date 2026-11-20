package agent

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// cutNode is the node HostCutCauses reads: files and directories by path,
// and paths whose read fails with an error that is not fs.ErrNotExist.
type cutNode struct {
	files map[string]string
	dirs  map[string]bool
	fail  map[string]bool
}

// newCutNode is a node whose kernel can cut: nvmet loaded, debugfs mounted
// at /sys/kernel/debug, nvmet's debug directory there, no lockdown.
func newCutNode() *cutNode {
	return &cutNode{
		files: map[string]string{
			ProcMountsPath: "sysfs /sys sysfs rw 0 0\n" +
				"debugfs /sys/kernel/debug debugfs rw,relatime 0 0\n",
			LockdownPath: "[none] integrity confidentiality\n",
		},
		dirs: map[string]bool{NvmetRoot: true, NvmetDebugRoot: true},
		fail: map[string]bool{},
	}
}

func (n *cutNode) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		ReadFileFn: func(ctx context.Context, path string) (string, error) {
			if n.fail[path] {
				return "", &iofs.PathError{Op: "open", Path: path,
					Err: syscall.EACCES}
			}
			data, ok := n.files[path]
			if !ok {
				return "", fmt.Errorf("no file %s: %w", path, iofs.ErrNotExist)
			}
			return data, nil
		},
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			if n.fail[path] {
				return nil, &iofs.PathError{Op: "open", Path: path,
					Err: syscall.EACCES}
			}
			if !n.dirs[path] {
				return nil, &iofs.PathError{Op: "open", Path: path,
					Err: syscall.ENOENT}
			}
			return nil, nil
		},
	}
}

// TestHostCutCausesNamesEachCause: a kernel cuts only with nvmet loaded,
// debugfs mounted at /sys/kernel/debug, nvmet's debug directory there and
// no lockdown; each fact that fails is a cause, a read that does not answer
// is one too, and a missing lockdown file is no lockdown. The debug
// directory is judged only once nvmet is loaded and debugfs mounted.
func TestHostCutCausesNamesEachCause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(n *cutNode)
		want   []string
	}{
		{"can cut", func(n *cutNode) {}, nil},
		{"no lockdown file", func(n *cutNode) {
			delete(n.files, LockdownPath)
		}, nil},
		{"nvmet not loaded", func(n *cutNode) {
			delete(n.dirs, NvmetRoot)
			delete(n.dirs, NvmetDebugRoot)
		}, []string{"no " + NvmetRoot}},
		{"debugfs not mounted", func(n *cutNode) {
			n.files[ProcMountsPath] = "sysfs /sys sysfs rw 0 0\n"
			delete(n.dirs, NvmetDebugRoot)
		}, []string{"debugfs is not mounted at /sys/kernel/debug"}},
		{"debugfs elsewhere", func(n *cutNode) {
			n.files[ProcMountsPath] = "debugfs /mnt/dbg debugfs rw 0 0\n"
		}, []string{"debugfs is not mounted at /sys/kernel/debug"}},
		{"no debug directory", func(n *cutNode) {
			delete(n.dirs, NvmetDebugRoot)
		}, []string{"nvmet has no debug directory " + NvmetDebugRoot}},
		{"lockdown", func(n *cutNode) {
			n.files[LockdownPath] = "none [integrity] confidentiality\n"
		}, []string{"lockdown is integrity"}},
		{"empty lockdown file", func(n *cutNode) {
			n.files[LockdownPath] = ""
		}, []string{`lockdown is ""`}},
		{"nothing reads", func(n *cutNode) {
			n.fail[NvmetRoot] = true
			n.fail[ProcMountsPath] = true
			n.fail[LockdownPath] = true
		}, []string{"nvmet not read", "mounts not read",
			"lockdown not read"}},
		{"debug directory not read", func(n *cutNode) {
			n.fail[NvmetDebugRoot] = true
		}, []string{NvmetDebugRoot + " not read"}},
		{"every cause", func(n *cutNode) {
			delete(n.dirs, NvmetRoot)
			n.files[ProcMountsPath] = ""
			n.files[LockdownPath] = "none integrity [confidentiality]"
		}, []string{"no " + NvmetRoot, "debugfs is not mounted",
			"lockdown is confidentiality"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newCutNode()
			tc.change(n)
			got := HostCutCauses(context.Background(), n.osClient())
			if len(got) != len(tc.want) {
				t.Fatalf("causes %q, want %d starting %q", got,
					len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if !strings.HasPrefix(got[i], want) {
					t.Errorf("cause %d is %q, want it to start %q", i,
						got[i], want)
				}
			}
		})
	}
}

// TestHostCutCausesReadsOnly: the read changes nothing on the node.
func TestHostCutCausesReadsOnly(t *testing.T) {
	n := newCutNode()
	oc := n.osClient()
	var wrote error
	oc.WriteFileDirectFn = func(ctx context.Context, path, data string) error {
		wrote = errors.New("write " + path)
		return wrote
	}
	oc.RunCommandFn = func(ctx context.Context, cmd string, args []string,
		stdin string) (string, string, int, error) {
		wrote = errors.New("command " + cmd)
		return "", "", 1, wrote
	}
	HostCutCauses(context.Background(), oc)
	if wrote != nil {
		t.Errorf("the read changed the node: %v", wrote)
	}
}
