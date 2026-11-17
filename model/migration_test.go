package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// olderMigrName is a migration other than the one a test ends, whose keys
// of an older build's bitmap must stay.
const olderMigrName = "migr-other"

// seedOlderBitmapKeys puts the keys an older build kept a migration's
// bitmap under, at every index it could write, for migrName; the value
// stands for that build's chunk, a message no build decodes any more.
func seedOlderBitmapKeys(t *testing.T, env *opsEnv, migrName string) {
	t.Helper()
	for bmIdx := range uint32(LegacyMigrBmCnt) {
		mustPut(t, env.cli,
			LegacyMigrBitmapKey(env.cid, opsSpId, migrName, bmIdx),
			&pb.PushMigrBitmapRequest{Bitmap: []byte{0xff}})
	}
}

// TestWorkerMigrationEnds pins the sp worker's two migration ends (MD6,
// AR8): a cancel drops the destination side while the cutover is pending,
// its extents and units back to its disk node, which is bumped; a finish
// drops the source side once the cutover has started, its extents back to
// its disk node, which is bumped, and the migration's units back to the
// destination's disk node, which is not; either drops the record and its
// name, and the keys an older build kept the migration's bitmap under,
// with one SpRev bump; another migration's such keys stay. A cancel after
// the cutover, a finish before it, and a record that is not the one read
// are refused, writing nothing.
func TestWorkerMigrationEnds(t *testing.T) {
	const units = 3
	setup := func(t *testing.T, pending bool) *opsEnv {
		env := newOpsEnv(t)
		env.addMigration(true)
		migr := env.migration()
		migr.CutoverPending = pending
		migr.MetaUnits = units
		mustPut(t, env.cli, MigrationKey(env.cid, opsSpId, opsMigrName), migr)
		dn := env.dn(opsDnC)
		dn.UsedMetaUnits = units
		mustPut(t, env.cli, DnConfKey(env.cid, opsDnC), dn)
		seedOlderBitmapKeys(t, env, opsMigrName)
		seedOlderBitmapKeys(t, env, olderMigrName)
		return env
	}
	sides := func(env *opsEnv) []uint64 {
		var ids []uint64
		for _, side := range findLeg(env.slice(), opsDataLegA).GetSideList() {
			ids = append(ids, side.GetSideId())
		}
		return ids
	}
	ended := func(t *testing.T, env *opsEnv) {
		t.Helper()
		if env.exists(MigrationKey(env.cid, opsSpId, opsMigrName)) {
			t.Errorf("the migration's record survived")
		}
		if names := env.spConf().GetMigrNameList(); len(names) != 0 {
			t.Errorf("migr_name_list %v, want empty", names)
		}
		for bmIdx := range uint32(LegacyMigrBmCnt) {
			key := LegacyMigrBitmapKey(env.cid, opsSpId, opsMigrName, bmIdx)
			if env.exists(key) {
				t.Errorf("the older build's bitmap key %q survived", key)
			}
			key = LegacyMigrBitmapKey(env.cid, opsSpId, olderMigrName, bmIdx)
			if !env.exists(key) {
				t.Errorf("another migration's key %q went", key)
			}
		}
	}

	t.Run("a cancel before the cutover", func(t *testing.T) {
		env := setup(t, true)
		spRev, dnRev, free := env.spRev(), env.dnRev(opsDnC),
			env.dn(opsDnC).GetFreeExtCnt()
		if err := CancelMigration(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, opsMigrName, opsMigrId, env.cc); err != nil {
			t.Fatalf("CancelMigration: %v", err)
		}
		if got := sides(env); len(got) != 1 || got[0] != opsDataSideA {
			t.Errorf("leg A's sides %v, want the source alone", got)
		}
		ended(t, env)
		dn := env.dn(opsDnC)
		if dn.GetFreeExtCnt() != free+opsDataExtCnt ||
			dn.GetUsedMetaUnits() != 0 || env.dnRev(opsDnC) != dnRev+1 {
			t.Errorf("dn-c free %d units %d rev %d, want %d, 0 and %d",
				dn.GetFreeExtCnt(), dn.GetUsedMetaUnits(),
				env.dnRev(opsDnC), free+opsDataExtCnt, dnRev+1)
		}
		if env.spRev() != spRev+1 {
			t.Errorf("SpRev %d, want %d", env.spRev(), spRev+1)
		}
	})
	t.Run("a finish after the cutover", func(t *testing.T) {
		env := setup(t, false)
		dnRevA, freeA, dnRevC := env.dnRev(opsDnA),
			env.dn(opsDnA).GetFreeExtCnt(), env.dnRev(opsDnC)
		if err := FinishMigration(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, opsMigrName, opsMigrId, env.cc); err != nil {
			t.Fatalf("FinishMigration: %v", err)
		}
		if got := sides(env); len(got) != 1 || got[0] != opsDstSide {
			t.Errorf("leg A's sides %v, want the destination alone", got)
		}
		ended(t, env)
		if env.dn(opsDnA).GetFreeExtCnt() != freeA+opsDataExtCnt ||
			env.dnRev(opsDnA) != dnRevA+1 {
			t.Errorf("dn-a free %d rev %d, want %d and %d",
				env.dn(opsDnA).GetFreeExtCnt(), env.dnRev(opsDnA),
				freeA+opsDataExtCnt, dnRevA+1)
		}
		if env.dn(opsDnC).GetUsedMetaUnits() != 0 ||
			env.dnRev(opsDnC) != dnRevC {
			t.Errorf("dn-c units %d rev %d, want 0 and unbumped %d",
				env.dn(opsDnC).GetUsedMetaUnits(), env.dnRev(opsDnC), dnRevC)
		}
	})
	for _, tc := range []struct {
		name    string
		pending bool
		cancel  bool
		migrId  uint64
		reason  string
	}{
		{"a cancel after the cutover", false, true, opsMigrId,
			ReasonCutoverStarted},
		{"a finish before the cutover", true, false, opsMigrId,
			ReasonCutoverPending},
		{"another record under the name", true, true, opsMigrId + 1,
			ReasonMigrationGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t, tc.pending)
			store := env.storeRev()
			end, op := FinishMigration, opFinishMigration
			if tc.cancel {
				end, op = CancelMigration, opCancelMigration
			}
			err := end(env.ctx, env.cli, env.cid, opsShard, opsSpId,
				opsSpName, opsMigrName, tc.migrId, env.cc)
			if pre := wantPrecondition(t, err, op); pre.Reason != tc.reason {
				t.Errorf("reason %q, want %q", pre.Reason, tc.reason)
			}
			if env.storeRev() != store {
				t.Errorf("a refused end wrote")
			}
		})
	}
}
