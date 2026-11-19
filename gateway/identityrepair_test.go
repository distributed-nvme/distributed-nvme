package gateway

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CreateRepair's read of the primary's latest report (architecture.md,
// Repairs): a bad leg whose row carries an identity reason, which no record
// holds, is taken as a repair's bad leg though it has no health epoch; one
// whose identity is undecided only with force_undecided, whatever its health
// epoch; and a primary that does not answer leaves the answer a record
// gives. The primary sits on the package's fake agent (fsPrimary), so the
// read is a real call the test can count and steer.

// irReport installs the primary's report with the data group's leg A row
// reading OK with reason, as the cn agent marks a leg the running array does
// not take or an assembly cannot decide on (cnagent.md CN12).
func irReport(agent *fakeAgent, reason pb.ResReason) {
	agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
		LegIdToLeg: map[uint64]*pb.ResInfo{
			volDataLegA: {
				Status: pb.ResStatus_RES_STATUS_OK,
				Reason: reason,
			},
		},
	})
}

// irWantCreated asserts the create succeeded with a repair that keeps no
// force and retires its bad leg with keep at the finish, and that the
// primary was asked once.
func irWantCreated(
	t *testing.T,
	env *volEnv,
	agent *fakeAgent,
	err error,
) {
	t.Helper()
	if err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}
	if rep := env.repair("rep-a"); rep.GetForce() || !rep.GetKeepBadLegs() ||
		rep.GetBadLegIdList()[0] != volDataLegA {
		t.Errorf("the repair %v, want leg %d bad, no force and its bad "+
			"leg kept at the finish", rep, volDataLegA)
	}
	if got := agent.callCount("GetCntlrInfo"); got != 1 {
		t.Errorf("GetCntlrInfo called %d times, want once", got)
	}
}

// TestCreateRepairTakesALegOfAnotherIdentity: a leg whose row the
// primary's report marks RES_REASON_OTHER_IDENTITY, a leg the running array
// does not take back, is a repair's bad leg with no flag, though its health
// epoch is clear: the running array tells which identity is current.
func TestCreateRepairTakesALegOfAnotherIdentity(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	irReport(agent, pb.ResReason_RES_REASON_OTHER_IDENTITY)
	_, err := env.srv.CreateRepair(env.ctx, repRequest(env))
	irWantCreated(t, env, agent, err)
}

// TestCreateRepairAsksAFlagForAnUndecidedLeg: a leg whose row the primary's
// report marks RES_REASON_IDENTITY_UNDECIDED is refused without
// force_undecided, FAILED_PRECONDITION naming the flag, whatever its health
// epoch says — a repair of such a leg would assemble the array from the
// other leg's copy — and nothing is written; with the flag it is taken, and
// the repair keeps no force, so an abort leaves the leg as it is.
func TestCreateRepairAsksAFlagForAnUndecidedLeg(t *testing.T) {
	for _, epoch := range []uint64{0, repEpoch} {
		t.Run(fmt.Sprintf("err_epoch %d", epoch), func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			irReport(agent, pb.ResReason_RES_REASON_IDENTITY_UNDECIDED)
			repSetLegEpoch(env, volDataGrpId, volDataLegA, epoch)
			before := env.dump()
			_, err := env.srv.CreateRepair(env.ctx, repRequest(env))
			msg := volWantCode(t, err, codes.FailedPrecondition)
			for _, want := range []string{
				fmt.Sprintf("leg %d", volDataLegA), "force_undecided",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not say %q", msg, want)
				}
			}
			env.wantNoWrite("the refused CreateRepair", before)
			agent.mu.Lock()
			agent.calls = nil
			agent.mu.Unlock()
			req := repRequest(env)
			req.ForceUndecided = true
			_, err = env.srv.CreateRepair(env.ctx, req)
			irWantCreated(t, env, agent, err)
		})
	}
}

// TestCreateRepairReadsTheReportWhateverTheEpoch: with no identity reason on
// the leg's row, the record decides as it always did, after one read of the
// report: a healthy leg is refused, its words naming the migration, and one
// with a health epoch is taken. A primary whose agent does not answer leaves
// the same answers, so a dead leg's repair never waits on a silent primary.
func TestCreateRepairReadsTheReportWhateverTheEpoch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		silent bool
		epoch  uint64
	}{
		{"a healthy leg, the report naming nothing", false, 0},
		{"a leg with an epoch, the report naming nothing", false, repEpoch},
		{"a healthy leg, the primary silent", true, 0},
		{"a leg with an epoch, the primary silent", true, repEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			irReport(agent, pb.ResReason_RES_REASON_NONE)
			if tc.silent {
				agent.setFail(errors.New("the agent is gone"))
			}
			repSetLegEpoch(env, volDataGrpId, volDataLegA, tc.epoch)
			before := env.dump()
			_, err := env.srv.CreateRepair(env.ctx, repRequest(env))
			if got := agent.callCount("GetCntlrInfo"); got != 1 {
				t.Errorf("GetCntlrInfo called %d times, want once", got)
			}
			if tc.epoch != 0 {
				if err != nil {
					t.Fatalf("CreateRepair of a leg with an epoch: %v", err)
				}
				return
			}
			msg := volWantCode(t, err, codes.FailedPrecondition)
			if want := fmt.Sprintf("leg %d is healthy: use a migration "+
				"to move it", volDataLegA); !strings.HasPrefix(msg, want) {
				t.Errorf("message %q, want it to begin %q", msg, want)
			}
			env.wantNoWrite("the refused CreateRepair", before)
		})
	}
}

// TestCreateRepairOfAHealthyLegOfALargeBlockPool: in a pool stored with a
// data block over the block's bound, a healthy leg's refusal does not send
// the operator to a migration, which that pool refuses too (migrPoolChecks),
// but names the way out, a move of the pool's volumes to a new pool.
func TestCreateRepairOfAHealthyLegOfALargeBlockPool(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	irReport(agent, pb.ResReason_RES_REASON_NONE)
	conf := env.spConf()
	conf.BdevConf.DmPoolConf.DataBlockSize = 2 * common.MaxNvmeTcpCmdBytes
	env.putSpConf(conf)
	before := env.dump()
	_, err := env.srv.CreateRepair(env.ctx, repRequest(env))
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if strings.Contains(msg, "use a migration") ||
		!strings.Contains(msg, "move the pool's volumes to a new pool") ||
		!strings.Contains(msg, fmt.Sprintf("leg %d is healthy", volDataLegA)) {
		t.Errorf("message %q, want the healthy leg and the way out, not "+
			"a migration", msg)
	}
	env.wantNoWrite("the refused CreateRepair", before)
}
