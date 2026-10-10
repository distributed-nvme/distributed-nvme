package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The migration guard of DeleteSpareLeg and SwitchSpareLeg (architecture.md,
// Spare legs; gateway.md, Spare legs), on the fixture of handler_vol_test.go.
//
// Between CreateMigration and Finish/Cancel a leg owns two sides
// (architecture.md, Migrations), and the source may be the side of a SPARE leg
// as well as of an active one. Without the guard either RPC would take such a
// leg away from under its migration: DeleteSpareLeg would release both sides
// and drop the leg, and SwitchSpareLeg would park a migrating active leg where
// the delete could reach it. Either way the Migration would be left naming
// sides in no leg, FinishMigration and CancelMigration would answer ABORTED
// for ever and the SP, whose migr_name_list could never empty, could never be
// deleted.
//
// The last two tests are CreateSpareLeg's per-round plan: a round whose group
// gained a DN, or whose pick was disabled or deleted, after its plan is a
// changed candidate, and the next round plans again.

// splMigrName is the one migration each test below starts.
const splMigrName = "migr-a"

// splMigrate starts splMigrName with srcSideId as its source and its
// destination pinned to dstAddr, and returns the destination's side_id:
// migr_id and dst_side_id are the next two ids, in that order
// (architecture.md, Migrations).
func splMigrate(env *volEnv, srcSideId uint64, dstAddr string) uint64 {
	env.t.Helper()
	reply, err := env.srv.CreateMigration(env.ctx, &pb.CreateMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		MigrName:    splMigrName,
		SrcSideId:   srcSideId,
		DnSelector:  volDnSelector(dstAddr),
	})
	if err != nil {
		env.t.Fatalf("CreateMigration: %v", err)
	}
	return reply.GetMigrId() + 1
}

// splFinish ends splMigrName keeping its destination. force = true skips the
// hydration proof, the only part of the RPC that leaves etcd.
func splFinish(env *volEnv) error {
	env.t.Helper()
	_, err := env.srv.FinishMigration(env.ctx, &pb.FinishMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		MigrName:    splMigrName,
		Force:       true,
	})
	return err
}

// splReadySpare parks one spare on dn-c for the data group and plays the
// sp-worker's provisioned flip on its side, so the only precondition a switch
// to it can still fail is the one under test. A direct put, so the SP's
// revision does not move.
func splReadySpare(env *volEnv) uint64 {
	env.t.Helper()
	legId := volCreateSpareLeg(env)
	slice := env.slice()
	spare := spareLegOf(volGrpOf(env.t, slice, volDataGrpId), legId)
	spare.GetSideList()[0].Provisioned = true
	env.putSlice(slice)
	return legId
}

// splWantMigrationRunning asserts CreateMigration's own refusal of a leg that
// already owns two sides (architecture.md, Migrations), naming that leg. It
// reports rather than stops, so a test can still show what the store did after
// an RPC that should have refused.
func splWantMigrationRunning(t *testing.T, err error, legId uint64) {
	t.Helper()
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("got %v, want FAILED_PRECONDITION", err)
		return
	}
	want := fmt.Sprintf(
		"leg %d already has 2 sides: a migration is running on it", legId)
	if got := status.Convert(err).Message(); got != want {
		t.Errorf("message: got %q, want %q", got, want)
	}
}

// TestDeleteSpareLegRefusesALegUnderMigration pins DeleteSpareLeg's guard: a
// spare whose side is a migration's source owns the destination as well, and
// releasing it would strand the migration. The delete is FAILED_PRECONDITION
// with nothing written; the migration then finishes, and the spare it leaves
// — one side again, the destination on dn-d — deletes cleanly. The slice,
// dn-c and dn-d end exactly as the fixture wrote them, and the SP names no
// migration.
func TestDeleteSpareLegRefusesALegUnderMigration(t *testing.T) {
	env := newVolEnv(t)
	sliceBefore := env.slice()
	dnCBefore := env.dn(volDnC)
	dnDBefore := env.dn(volDnD)
	legId := volCreateSpareLeg(env)
	spare := spareLegOf(volGrpOf(t, env.slice(), volDataGrpId), legId)
	// dn-a, dn-b and the spare's dn-c are all the group's, so the
	// destination can only be dn-d.
	splMigrate(env, spare.GetSideList()[0].GetSideId(), volDnD)

	before := env.spConf()
	beforeRev := env.spRev()
	sliceMigrating := env.slice()
	dnCMigrating := env.dn(volDnC)
	dnDMigrating := env.dn(volDnD)
	_, err := env.srv.DeleteSpareLeg(env.ctx, &pb.DeleteSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		GrpId:       volDataGrpId,
		LegId:       legId,
	})
	splWantMigrationRunning(t, err, legId)
	env.wantUntouched(before, beforeRev)
	if got := env.slice(); !proto.Equal(got, sliceMigrating) {
		t.Errorf("the slice moved on a refusal: %v", got)
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnCMigrating) {
		t.Errorf("dn-c moved on a refusal:\n got %v\nwant %v",
			got, dnCMigrating)
	}
	if got := env.dn(volDnD); !proto.Equal(got, dnDMigrating) {
		t.Errorf("dn-d moved on a refusal:\n got %v\nwant %v",
			got, dnDMigrating)
	}

	if err := splFinish(env); err != nil {
		t.Fatalf("FinishMigration after the refused delete: %v", err)
	}
	if _, err := env.srv.DeleteSpareLeg(env.ctx, &pb.DeleteSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		GrpId:       volDataGrpId,
		LegId:       legId,
	}); err != nil {
		t.Fatalf("DeleteSpareLeg once the migration has finished: %v", err)
	}
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("slice: got %v, want the fixture back", got)
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnCBefore) {
		t.Errorf("dn-c: got %v, want %v", got, dnCBefore)
	}
	if got := env.dn(volDnD); !proto.Equal(got, dnDBefore) {
		t.Errorf("dn-d: got %v, want %v", got, dnDBefore)
	}
	if got := env.spConf().GetMigrNameList(); len(got) != 0 {
		t.Errorf("migr_name_list: got %v, want empty", got)
	}
}

// TestSwitchSpareLegRefusesALegUnderMigration pins SwitchSpareLeg's guard on
// both legs it moves: a migrating active leg is not parked and a migrating
// spare is not promoted. Each switch is FAILED_PRECONDITION naming the leg,
// with nothing written. The refusal belongs to the migration, not to the leg:
// once the target's migration has finished, the same switch goes through.
// The spare case stops at the refusal: a forced finish here would leave that
// spare only its destination side, which CreateMigration wrote unprovisioned
// and nothing in this test flips, so the same switch would then be refused
// as "spare side is not provisioned" instead.
//
// The model op's own guard — what refuses the worker's AR8 switch, and a
// switch through this handler that carried no token, when a migration was
// created after the leg was read — is pinned in model
// (TestSwitchSpareLegPreconditions). In that race a switch that carried a
// token is refused first, by the STM's token check, because CreateMigration
// bumped SpRev.
func TestSwitchSpareLegRefusesALegUnderMigration(t *testing.T) {
	t.Run("the target", func(t *testing.T) {
		env := newVolEnv(t)
		legId := splReadySpare(env)
		// dn-c holds the spare, so the destination can only be dn-d.
		dstSideId := splMigrate(env, volDataSideA, volDnD)

		before := env.spConf()
		beforeRev := env.spRev()
		sliceMigrating := env.slice()
		_, err := env.srv.SwitchSpareLeg(env.ctx, &pb.SwitchSpareLegRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: beforeRev},
			GrpId:       volDataGrpId,
			SpareLegId:  legId,
			TargetLegId: volDataLegA,
		})
		splWantMigrationRunning(t, err, volDataLegA)
		env.wantUntouched(before, beforeRev)
		if got := env.slice(); !proto.Equal(got, sliceMigrating) {
			t.Errorf("the slice moved on a refusal: %v", got)
		}

		if err := splFinish(env); err != nil {
			t.Fatalf("FinishMigration after the refused switch: %v", err)
		}
		if _, err := env.srv.SwitchSpareLeg(
			env.ctx, &pb.SwitchSpareLegRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				GrpId:       volDataGrpId,
				SpareLegId:  legId,
				TargetLegId: volDataLegA,
			}); err != nil {
			t.Fatalf("SwitchSpareLeg once the migration has finished: %v",
				err)
		}
		grp := volGrpOf(t, env.slice(), volDataGrpId)
		if grp.GetLegList()[0].GetLegId() != legId {
			t.Errorf("leg_list: got %v, want the spare in position 0",
				grp.GetLegList())
		}
		parked := spareLegOf(grp, volDataLegA)
		if parked == nil || len(parked.GetSideList()) != 1 ||
			parked.GetSideList()[0].GetSideId() != dstSideId {
			t.Errorf("spare_leg_list: got %v, want leg %d parked on its "+
				"destination side %d alone",
				grp.GetSpareLegList(), volDataLegA, dstSideId)
		}
	})

	t.Run("the spare", func(t *testing.T) {
		env := newVolEnv(t)
		legId := splReadySpare(env)
		spare := spareLegOf(volGrpOf(t, env.slice(), volDataGrpId), legId)
		splMigrate(env, spare.GetSideList()[0].GetSideId(), volDnD)

		before := env.spConf()
		beforeRev := env.spRev()
		sliceMigrating := env.slice()
		_, err := env.srv.SwitchSpareLeg(env.ctx, &pb.SwitchSpareLegRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: beforeRev},
			GrpId:       volDataGrpId,
			SpareLegId:  legId,
			TargetLegId: volDataLegA,
		})
		splWantMigrationRunning(t, err, legId)
		env.wantUntouched(before, beforeRev)
		if got := env.slice(); !proto.Equal(got, sliceMigrating) {
			t.Errorf("the slice moved on a refusal: %v", got)
		}
	})
}

// volRaceSpareLeg runs a token-less CreateSpareLeg of the data group with no
// NodeSelector, and runs actor — another writer — inside the request's first
// read of key, the way volRaceMigrationA races a migration. It fails the test
// unless the hook fired and both writers succeeded, and returns the addr_port
// the spare's side landed on.
func volRaceSpareLeg(
	t *testing.T,
	env *volEnv,
	key string,
	actor func() error,
) string {
	t.Helper()
	var actorErr error
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     key,
		actor:   func() { actorErr = actor() },
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	// Bounded twice over, so that a unit which never settles fails as
	// ABORTED instead of hanging the package: GW9 retries until the round
	// cap or the ctx's end, whichever comes first.
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, volReadHookKey{}, true), 30*time.Second)
	defer cancel()
	_, err := env.srv.CreateSpareLeg(ctx, &pb.CreateSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		GrpId:       volDataGrpId,
	})
	if !hook.fired.Load() {
		t.Fatalf("the request never read %s: the other writer never ran "+
			"and nothing was raced", key)
	}
	if actorErr != nil {
		t.Fatalf("the interleaved writer: %v", actorErr)
	}
	if err != nil {
		t.Fatalf("CreateSpareLeg: %v", err)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	if len(grp.GetSpareLegList()) != 1 ||
		len(grp.GetSpareLegList()[0].GetSideList()) != 1 {
		t.Fatalf("spare_leg_list = %v, want one spare with one side",
			grp.GetSpareLegList())
	}
	return grp.GetSpareLegList()[0].GetSideList()[0].GetAddrPort()
}

// TestCreateSpareLegReplansAroundADnTheGroupGained pins the per-round plan of
// CreateSpareLeg (architecture.md, Spare legs; GW9): a round plans its black
// list and its tier-1 locations from the group as its snapshot read it, so a
// side another request hangs off the group after that read — here a
// migration of the group's other leg onto dn-c — is missing from the plan,
// and the scan offers that side's very DN. model.CreateSpareLeg refuses the
// gain as candidate changed, and the next round plans from the group as it
// now stands, so the spare lands where a serial run would put it. A plan read
// once for the whole unit would be refused the same way in every round until
// the round cap.
//
// Every draw is forced, as in TestCreateMigrationRefusesADnTheGroupAlreadyUses:
// dn-d shares dn-c's failure domain and holds less, so the first scan offers
// dn-c alone, and the second — dn-c black-listed and its domain excluded —
// offers dn-d alone, through tier 2.
func TestCreateSpareLegReplansAroundADnTheGroupGained(t *testing.T) {
	env := newVolEnv(t)
	env.relocateDn(volDnD, volDnC)
	env.setDnFree(volDnC, volDnCFree)
	got := volRaceSpareLeg(t, env, model.SliceKey(env.cid, volSpId, volSliceId),
		func() error { return volMigrateLegB(env) })
	if got != volDnD {
		t.Errorf("the spare on %s, want %s", got, volDnD)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	legOf := make(map[string]uint64)
	for _, leg := range allLegs(grp) {
		for _, side := range leg.GetSideList() {
			addrPort := side.GetAddrPort()
			if other, ok := legOf[addrPort]; ok && other != leg.GetLegId() {
				t.Errorf("%s carries a side of leg %d and of leg %d: two "+
					"legs of group %d on one disk node", addrPort, other,
					leg.GetLegId(), volDataGrpId)
			}
			legOf[addrPort] = leg.GetLegId()
		}
	}
	if legOf[volDnC] != volDataLegB {
		t.Errorf("%s carries a side of leg %d, want leg %d's destination",
			volDnC, legOf[volDnC], volDataLegB)
	}
	if rev := env.spRev(); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3 (one bump per request)", rev)
	}
}

// TestCreateSpareLegReplansAroundADnDisabledOrDeleted pins model.CreateSpareLeg's
// reading of a pick gone stale: the DN the scan offered is disabled, or
// deleted, while the op's transaction runs, and the op answers candidate
// changed rather than refusing, so the next round scans again and the spare
// lands on the other DN. dn_batch_size is 1 and dn-c holds the most free
// extents, so the first scan offers dn-c alone; the other writer runs inside
// the op's first read of dn-c's record, whose transaction then fails its
// commit and re-reads the record as the other writer left it.
func TestCreateSpareLegReplansAroundADnDisabledOrDeleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor func(env *volEnv) error
	}{
		{
			name: "disabled",
			actor: func(env *volEnv) error {
				_, err := env.srv.UpdateDiskNodeDisabled(env.ctx,
					&pb.UpdateDiskNodeDisabledRequest{
						ClusterName: env.cluster,
						AddrPort:    volDnC,
						Disabled:    true,
					})
				return err
			},
		},
		{
			name: "deleted",
			actor: func(env *volEnv) error {
				env.dropDn(volDnC)
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.setDnBatchSize(1)
			env.setDnFree(volDnC, volDnCFree)
			got := volRaceSpareLeg(t, env, model.DnConfKey(env.cid, volDnC),
				func() error { return tc.actor(env) })
			if got != volDnD {
				t.Errorf("the spare on %s, want %s", got, volDnD)
			}
			if free := env.dn(volDnD).GetFreeExtCnt(); free !=
				volDnFree-volDataExtCnt {
				t.Errorf("dn-d free_ext_cnt: got %d, want %d", free,
					volDnFree-volDataExtCnt)
			}
			if rev := env.spRev(); rev != 2 {
				t.Errorf("sp_rev: got %d, want 2 (the spare's one bump)",
					rev)
			}
		})
	}
}
