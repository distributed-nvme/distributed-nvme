package ctl

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestRepairCreateForceUndecided covers `repair create --force-undecided`
// (dnvctl.md, `repair` — `ctl/repair.go`): the flag sends force_undecided
// alone, never force, which the repair keeps and which makes an abort copy
// over the bad leg; a create without it sends it false.
func TestRepairCreateForceUndecided(t *testing.T) {
	rep := runArgv(t, "CreateRepair", "repair", "create", "--name", "r",
		"--grp", "1", "--bad-legs", "2", "--force-undecided")
	wantRequest(t, rep, &pb.CreateRepairRequest{
		ClusterName:    itCluster,
		SpName:         itSp,
		RepName:        "r",
		GrpId:          1,
		BadLegIdList:   []uint64{2},
		ForceUndecided: true,
	})
	plain := runArgv(t, "CreateRepair", "repair", "create", "--name", "r",
		"--grp", "1", "--bad-legs", "2").(*pb.CreateRepairRequest)
	if plain.GetForceUndecided() || plain.GetForce() {
		t.Errorf("a create with neither flag sent %v", plain)
	}
}
