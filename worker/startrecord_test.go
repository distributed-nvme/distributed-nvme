package worker

import (
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TestStartRecordCarriesTheBuildVersion pins the build version in the
// "worker starting" record (CM6): the build of the process that runs,
// which --version, a read of the file on disk, cannot give
// (architecture.md, Upgrade in lockstep).
func TestStartRecordCarriesTheBuildVersion(t *testing.T) {
	h := newWorkerHarness(t, harnessOpts{})
	w := h.startWorker(t, "w1")
	rec := h.waitRecord(t, 10*time.Second, w, msgWorkerStarting, nil)
	if got := rec.str("version"); got != common.BuildVersion() {
		t.Errorf("worker starting carries version %q, want %q", got, common.BuildVersion())
	}
}
