package csi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunNodeRefusesAKernelWithoutWhatAConnectNeeds pins csi.md CSI13: the
// node plugin refuses to start, before it serves, on a kernel whose native
// NVMe multipath is off or not built, or with no NVMe over Fabrics device,
// naming the fact and its fix, and when it cannot read one of those facts,
// naming the file and the error; it starts without the NVMe/TCP module,
// which the kernel loads at the first connect.
func TestRunNodeRefusesAKernelWithoutWhatAConnectNeeds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(f *fakeNode)
		starts bool
		want   []string
	}{
		{"multipath off", func(f *fakeNode) { f.files[multipathParam] = "N\n" },
			false, []string{"multipath is off", "nvme_core.multipath=Y"}},
		{"multipath not built", func(f *fakeNode) { delete(f.files, multipathParam) },
			false, []string{"not built", "CONFIG_NVME_MULTIPATH"}},
		{"no fabrics device", func(f *fakeNode) { delete(f.files, fabricsDevice) },
			false, []string{fabricsDevice, "modprobe nvme-tcp", "/etc/modules-load.d"}},
		{"both", func(f *fakeNode) {
			f.files[multipathParam] = "N\n"
			delete(f.files, fabricsDevice)
		}, false, []string{"multipath is off", "modprobe nvme-tcp"}},
		{"a read that fails", func(f *fakeNode) {
			f.readFail[multipathParam] = errors.New("input/output error")
		}, false, []string{multipathParam, "input/output error"}},
		{"no nvme-tcp module", func(f *fakeNode) {
			delete(f.dirs, "/sys/module/nvme_tcp")
		}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeNode()
			tc.setup(f)
			dir := t.TempDir()
			endpoint := filepath.Join(dir, "csi.sock")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- RunNode(ctx, Config{Endpoint: endpoint, OsClient: f.client(),
					CdcAddresses: []string{fakeCdcA}})
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				select {
				case err := <-done:
					if tc.starts {
						t.Fatalf("RunNode returned %v, want it to serve", err)
					}
					if err == nil {
						t.Fatal("RunNode returned no error")
					}
					for _, w := range tc.want {
						if !strings.Contains(err.Error(), w) {
							t.Errorf("the refusal %q does not name %q", err, w)
						}
					}
					return
				default:
				}
				if _, err := os.Stat(endpoint); err == nil {
					if !tc.starts {
						t.Fatal("RunNode serves on a kernel it must refuse")
					}
					cancel()
					if err := <-done; err != nil {
						t.Errorf("RunNode returned %v", err)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("RunNode neither served nor refused")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
