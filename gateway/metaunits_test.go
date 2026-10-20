package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The metadata units of architecture.md, Metadata units, as the gateway keeps
// them: CreateMigration charges a destination's units to its DN and
// FinishMigration and CancelMigration give them back. The scan skips a DN
// without room, the deciding transaction re-checks the room on the pick's
// record, and a refusal for room says "metadata room", which the gateway
// suite greps. A write that changes only a DN's units bumps no revision, and
// a release stops at zero. No CN counts units: CreateClone, CreateCntlr and
// DeleteCntlr read and write no CN for a clone.

// mustNotHaveCandidateChanged fails a test whose request leaked the internal
// sentinel instead of re-running its unit (GW9).
func mustNotHaveCandidateChanged(t *testing.T, err error) {
	t.Helper()
	if isCandidateChanged(err) {
		t.Fatalf("the internal candidate-changed sentinel reached the "+
			"client: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateClone (architecture.md, Clones)
// ---------------------------------------------------------------------------

// TestCreateCloneChargesNoCn pins that CreateClone reads and writes no CN: a
// clone keeps its dm-clone metadata on its clone meta group, on DNs, so the
// CN of every cntlr of the pool keeps its record, its capacity key and its
// revision, whatever the destination's size. The destination here is very
// large, 2 PiB, and still fits one side of the group at the fixture's
// geometry. A lost cntlr key does not stop it either:
// the create reads no cntlr.
func TestCreateCloneChargesNoCn(t *testing.T) {
	env := newVolEnv(t)
	const size = uint64(1) << 51
	mustPut(t, env.cli, model.ThinDeviceKey(env.cid, volSpId, "huge"),
		&pb.ThinDevice{TdId: 900, DevId: 7, Size: size, Created: true})
	conf := env.spConf()
	conf.TdNameList = append(conf.GetTdNameList(), "huge")
	env.putSpConf(conf)
	if err := env.cli.Delete(env.ctx,
		model.CntlrKey(env.cid, volSpId, volCntlrB)); err != nil {
		t.Fatalf("Delete cntlr b: %v", err)
	}
	type cnKeys struct {
		conf, capKey, rev int64
	}
	keysOf := func(addrPort string) cnKeys {
		cn := env.cn(addrPort)
		return cnKeys{
			volModRev(env, model.CnConfKey(env.cid, addrPort)),
			volModRev(env, model.CnCapacityKey(
				env.cid, cn.GetFreeExtCnt(), addrPort)),
			volModRev(env, model.CnRevKey(
				cn.GetShardCode(), env.cid, cn.GetCnId())),
		}
	}
	before := map[string]cnKeys{}
	for _, addrPort := range []string{volCnA, volCnB} {
		before[addrPort] = keysOf(addrPort)
	}

	volCreateClone(env, "clone-a", "huge")
	for addrPort, keys := range before {
		if keys.conf == 0 || keys.capKey == 0 || keys.rev == 0 {
			t.Fatalf("%s: a key the fixture writes is absent: %+v",
				addrPort, keys)
		}
		if got := keysOf(addrPort); got != keys {
			t.Errorf("%s was written by CreateClone: mod_revisions %+v, "+
				"want %+v", addrPort, got, keys)
		}
	}
}

// ---------------------------------------------------------------------------
// CreateMigration, FinishMigration, CancelMigration (architecture.md,
// Migrations; Metadata units)
// ---------------------------------------------------------------------------

// TestCreateMigrationSkipsADnWithoutMetadataRoom pins the scan's skip on the
// destination scan: a DN whose capacity value leaves it fewer free units than
// the destination needs is no candidate. dn_batch_size is 1 and dn-c holds the
// most free extents, so the scan offers the first eligible DN alone and,
// without the skip, would offer dn-c. With room for exactly the destination,
// dn-c is taken and ends at the ceiling.
func TestCreateMigrationSkipsADnWithoutMetadataRoom(t *testing.T) {
	for _, tc := range []struct {
		name string
		used uint64
		want string
	}{
		{
			name: "one unit short",
			used: common.DnCloneMetaUnitCnt - volMigrUnits + 1,
			want: volDnD,
		},
		{
			name: "room to the ceiling",
			used: common.DnCloneMetaUnitCnt - volMigrUnits,
			want: volDnC,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.setDnBatchSize(1)
			env.setDnFree(volDnC, volDnCFree)
			env.setDnUsed(volDnC, tc.used)
			usedBefore := map[string]uint64{volDnC: tc.used, volDnD: 0}
			got, err := volMigrationDst(env)
			if err != nil {
				t.Fatalf("CreateMigration: %v", err)
			}
			if got != tc.want {
				t.Fatalf("destination on %s, want %s", got, tc.want)
			}
			for addrPort, used := range usedBefore {
				want, wantRev := used, uint64(1)
				if addrPort == tc.want {
					want, wantRev = used+volMigrUnits, 2
				}
				dn := env.dn(addrPort)
				if dn.GetUsedMetaUnits() != want {
					t.Errorf("%s used_meta_units: got %d, want %d",
						addrPort, dn.GetUsedMetaUnits(), want)
				}
				if got := env.dnCapValue(addrPort); !proto.Equal(
					got, model.DnCapacityValue(dn),
				) {
					t.Errorf("%s capacity value: got %v, want %v", addrPort,
						got, model.DnCapacityValue(dn))
				}
				if got := env.dnRev(addrPort); got != wantRev {
					t.Errorf("%s rev: got %d, want %d", addrPort, got,
						wantRev)
				}
			}
			if got := env.migration("migr-a").GetMetaUnits(); got !=
				volMigrUnits {
				t.Errorf("migration meta_units: got %d, want %d", got,
					volMigrUnits)
			}
		})
	}
}

// TestCreateMigrationRefusesWhenNoDnHasMetadataRoom pins the refusal when the
// skip leaves the scan nothing: RESOURCE_EXHAUSTED saying "metadata room",
// because a scan without the units would have found the DN. When the DN is
// short of extents as well, the refusal is the extents' own, which the units
// play no part in. Either way nothing is written.
func TestCreateMigrationRefusesWhenNoDnHasMetadataRoom(t *testing.T) {
	for _, tc := range []struct {
		name string
		free uint64
		want string
	}{
		{
			name: "room is what is short",
			free: volDnFree,
			want: fmt.Sprintf("%s: no disk node with %d free extents has "+
				"metadata room for %d units", whatMigrDst, volDataExtCnt,
				volMigrUnits),
		},
		{
			name: "extents are short too",
			free: volDataExtCnt - 1,
			want: fmt.Sprintf("%s: 0 disk nodes with %d free extents, "+
				"need 1", whatMigrDst, volDataExtCnt),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.setDnFree(volDnC, tc.free)
			env.setDnUsed(volDnC, common.DnCloneMetaUnitCnt-volMigrUnits+1)
			before := env.spConf()
			beforeRev := env.spRev()
			dnBefore := env.dn(volDnC)
			_, err := env.srv.CreateMigration(
				env.ctx, &pb.CreateMigrationRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					MigrName:    "migr-a",
					SrcSideId:   volDataSideA,
					DnSelector:  volDnSelector(volDnC),
				})
			msg := volWantCode(t, err, codes.ResourceExhausted)
			if msg != tc.want {
				t.Errorf("message: got %q, want %q", msg, tc.want)
			}
			env.wantUntouched(before, beforeRev)
			if got := env.dn(volDnC); !proto.Equal(got, dnBefore) {
				t.Errorf("dn-c moved on a refusal: %v", got)
			}
			if got := env.dnRev(volDnC); got != 1 {
				t.Errorf("dn-c rev: got %d, want 1", got)
			}
		})
	}
}

// TestCreateMigrationReplansWhenItsDnFillsUp pins the deciding STM's re-check
// of the pick's room (dnLedger.verifyPick): a DN the scan saw with room, whose
// area fills up before the transaction commits, is a changed candidate, and
// the next round's scan skips it. The capacity key does not move when only
// the units do, so only the record the transaction reads can tell.
//
// dn-d shares dn-c's failure domain and holds fewer free extents, so the
// first scan offers dn-c alone (one DN per location, fullest first). The
// other writer fills dn-c inside the request's first read of dn-c's record,
// in the deciding STM. The next round's scan must skip dn-c BEFORE taking its
// location, or dn-d — the one DN with room — would be skipped for sharing it,
// and the request would be refused for room it has.
func TestCreateMigrationReplansWhenItsDnFillsUp(t *testing.T) {
	env := newVolEnv(t)
	env.relocateDn(volDnD, volDnC)
	env.setDnFree(volDnC, volDnCFree)
	filled := common.DnCloneMetaUnitCnt - volMigrUnits + 1
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     model.DnConfKey(env.cid, volDnC),
		actor:   func() { env.setDnUsed(volDnC, filled) },
	}
	slog.SetDefault(slog.New(hook))
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, volReadHookKey{}, true), 30*time.Second)
	_, err := env.srv.CreateMigration(ctx, &pb.CreateMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		MigrName:    "migr-a",
		SrcSideId:   volDataSideA,
		DmCloneConf: &pb.DmCloneConf{HydrationThreshold: 2},
	})
	cancel()
	slog.SetDefault(prev)
	if !hook.fired.Load() {
		t.Fatalf("the request never read dn-c's record: nothing was raced")
	}
	mustNotHaveCandidateChanged(t, err)
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	leg := activeLegOf(volGrpOf(t, env.slice(), volDataGrpId), volDataLegA)
	if got := leg.GetSideList()[1].GetAddrPort(); got != volDnD {
		t.Errorf("destination on %s, want %s", got, volDnD)
	}
	dnC := env.dn(volDnC)
	if dnC.GetUsedMetaUnits() != filled || dnC.GetFreeExtCnt() != volDnCFree ||
		len(dnC.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: got %v, want only the other writer's %d units",
			dnC, filled)
	}
	if got := env.dnRev(volDnC); got != 1 {
		t.Errorf("dn-c rev: got %d, want 1 (never charged)", got)
	}
	dnD := env.dn(volDnD)
	if dnD.GetUsedMetaUnits() != volMigrUnits ||
		dnD.GetFreeExtCnt() != volDnFree-volDataExtCnt {
		t.Errorf("dn-d: got %v, want the destination's extents and units",
			dnD)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2 (one commit)", rev)
	}
}

// volEndMigration ends migr-a, by cancel or by a forced finish, which comes
// after the cutover: a finish of a migration whose cutover has not started is
// refused (architecture.md, Migrations).
func volEndMigration(env *volEnv, finish bool) error {
	env.t.Helper()
	if finish {
		volCutover(env, "migr-a")
		_, err := env.srv.FinishMigration(env.ctx,
			&pb.FinishMigrationRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				MigrName:    "migr-a",
				Force:       true,
			})
		return err
	}
	_, err := env.srv.CancelMigration(env.ctx, &pb.CancelMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		MigrName:    "migr-a",
	})
	return err
}

// TestMigrationUnitsReleaseStopsAtZero pins the release's floor on both
// terminal RPCs: a destination DN whose count is already below what the
// Migration records — set by hand here — ends at zero, never at a count
// wrapped around to one that refuses every later charge. It also pins what
// the release reads and subtracts: the units the Migration records and no
// more, so a DN that also holds another migration's units — set by hand
// too — keeps exactly those, and a Migration that records none leaves the
// DN's count alone, and the finish, whose destination DN keeps its side,
// then neither reads nor writes that DN.
func TestMigrationUnitsReleaseStopsAtZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish bool
	}{
		{name: "cancel"},
		{name: "finish", finish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			volCreateMigration(env, "migr-a")
			env.setDnUsed(volDnC, volMigrUnits-1)
			if err := volEndMigration(env, tc.finish); err != nil {
				t.Fatalf("ending the migration: %v", err)
			}
			dn := env.dn(volDnC)
			if got := dn.GetUsedMetaUnits(); got != 0 {
				t.Errorf("dn-c used_meta_units: got %d, want 0", got)
			}
			if got := env.dnCapValue(volDnC); !proto.Equal(
				got, model.DnCapacityValue(dn),
			) {
				t.Errorf("dn-c capacity value: got %v, want %v", got,
					model.DnCapacityValue(dn))
			}
		})
		t.Run(tc.name+" beside another migration's units",
			func(t *testing.T) {
				env := newVolEnv(t)
				volCreateMigration(env, "migr-a")
				const other = 5
				env.setDnUsed(volDnC, volMigrUnits+other)
				if err := volEndMigration(env, tc.finish); err != nil {
					t.Fatalf("ending the migration: %v", err)
				}
				dn := env.dn(volDnC)
				if got := dn.GetUsedMetaUnits(); got != other {
					t.Errorf("dn-c used_meta_units: got %d, want %d", got,
						other)
				}
				if got := env.dnCapValue(volDnC); !proto.Equal(
					got, model.DnCapacityValue(dn),
				) {
					t.Errorf("dn-c capacity value: got %v, want %v", got,
						model.DnCapacityValue(dn))
				}
			})
		t.Run(tc.name+" of a migration recording no units",
			func(t *testing.T) {
				env := newVolEnv(t)
				volCreateMigration(env, "migr-a")
				migr := env.migration("migr-a")
				migr.MetaUnits = 0
				mustPut(t, env.cli,
					model.MigrationKey(env.cid, volSpId, "migr-a"), migr)
				const held = 5
				env.setDnUsed(volDnC, held)
				dnKey := model.DnConfKey(env.cid, volDnC)
				modRev := volModRev(env, dnKey)
				if err := volEndMigration(env, tc.finish); err != nil {
					t.Fatalf("ending the migration: %v", err)
				}
				if got := env.dn(volDnC).GetUsedMetaUnits(); got != held {
					t.Errorf("dn-c used_meta_units: got %d, want %d", got,
						held)
				}
				if tc.finish {
					if got := volModRev(env, dnKey); got != modRev {
						t.Errorf("the finish rewrote dn-c (mod_revision %d "+
							"-> %d) with no units to give back", modRev, got)
					}
				}
			})
	}
}

// volModRev is the mod_revision of one key, or zero when it is absent.
func volModRev(env *volEnv, key string) int64 {
	env.t.Helper()
	keys, _, err := env.cli.RangeKeys(env.ctx, key)
	if err != nil {
		env.t.Fatalf("RangeKeys %s: %v", key, err)
	}
	for _, entry := range keys {
		if entry.Key == key {
			return entry.ModRev
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// CreateCntlr and DeleteCntlr (architecture.md, Cntlrs)
// ---------------------------------------------------------------------------

// createClonePool creates sptDefaultSpec's pool with one clone by RPC and
// returns its sp_id.
func (e *sptEnv) createClonePool() uint64 {
	e.t.Helper()
	spId := e.createSp(sptDefaultSpec(sptSpName))
	if _, err := e.srv.CreateThinDevice(e.ctx,
		&pb.CreateThinDeviceRequest{
			ClusterName: e.name,
			SpName:      sptSpName,
			TdName:      "td-dst",
			Size:        uint64(1) << 30,
		}); err != nil {
		e.t.Fatalf("CreateThinDevice: %v", err)
	}
	if _, err := e.srv.CreateClone(e.ctx, &pb.CreateCloneRequest{
		ClusterName:   e.name,
		SpName:        sptSpName,
		CloneName:     "clone-a",
		SrcTrConf:     []*pb.NvmeTrConf{sptTrConf("src:9000")},
		SrcNqn:        "nqn.2024-01.io.dnv:src",
		SrcNsIdx:      1,
		SrcSliceCnt:   1,
		SrcStripeSize: 64 * 1024,
		SrcBlockSize:  1024 * 1024,
		DstTdName:     "td-dst",
	}); err != nil {
		e.t.Fatalf("CreateClone: %v", err)
	}
	return spId
}

// TestCntlrMutatorsReadNoClone pins that CreateCntlr and DeleteCntlr leave
// the pool's clones out: a cntlr's CN is charged the pool's footprint alone,
// since a clone keeps its dm-clone metadata on its clone meta group, on DNs.
// The pool lists two clones, one of them with its key gone; neither RPC reads
// either, so both commit, and the new cntlr's CN moves by its pointer and its
// footprint and by nothing else, and gets both back.
func TestCntlrMutatorsReadNoClone(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createClonePool()
	conf := env.spConf(sptSpName)
	conf.CloneNameList = append(conf.GetCloneNameList(), "lost")
	mustPut(t, env.cli, model.SpConfKey(env.cid, sptSpName), conf)
	before := map[string]*pb.CnConf{}
	for _, addrPort := range env.cnAddrs {
		before[addrPort] = env.cnConf(addrPort)
	}

	if _, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		CntlidSlot:  2,
	}); err != nil {
		t.Fatalf("CreateCntlr over a lost clone key: %v", err)
	}
	ids := env.spConf(sptSpName).GetCntlrIdList()
	cntlrId := ids[len(ids)-1]
	addrPort := env.cntlr(spId, cntlrId).GetAddrPort()
	want := proto.Clone(before[addrPort]).(*pb.CnConf)
	want.CntlrPtrList = append(want.CntlrPtrList,
		&pb.CntlrPointer{SpId: spId, CntlrId: cntlrId})
	want.FreeExtCnt -= sptFootprint
	if got := env.cnConf(addrPort); !proto.Equal(got, want) {
		t.Errorf("%s after CreateCntlr: got %v, want %v (the pointer in "+
			"and the footprint charged, nothing else)", addrPort, got, want)
	}

	if _, err := env.srv.UpdateCntlrEnabled(env.ctx,
		&pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			CntlrId:     cntlrId,
			Enabled:     false,
		}); err != nil {
		t.Fatalf("UpdateCntlrEnabled: %v", err)
	}
	if _, err := env.srv.DeleteCntlr(env.ctx, &pb.DeleteCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		CntlrId:     cntlrId,
	}); err != nil {
		t.Fatalf("DeleteCntlr over a lost clone key: %v", err)
	}
	for addrPort, cn := range before {
		if got := env.cnConf(addrPort); !proto.Equal(got, cn) {
			t.Errorf("%s after CreateCntlr and DeleteCntlr: got %v, want "+
				"it as it was: %v", addrPort, got, cn)
		}
	}
}
