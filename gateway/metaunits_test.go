package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The metadata units of architecture.md, Metadata units, as the gateway keeps
// them: CreateClone charges a clone's units to the CN of every cntlr of its
// pool, CreateCntlr charges the pool's whole clone sum to the new cntlr's CN
// and DeleteCntlr gives it back, CreateMigration charges a destination's
// units to its DN and FinishMigration and CancelMigration give them back. The
// scans skip a node without room, a deciding transaction re-checks the room
// on the pick's record, and a refusal for room says "metadata room", which
// the gateway suite greps. A write that changes only a node's units bumps no
// revision, and a release stops at zero.

// mustNotHaveCandidateChanged fails a test whose request leaked the internal
// sentinel instead of re-running its unit (GW9).
func mustNotHaveCandidateChanged(t *testing.T, err error) {
	t.Helper()
	if isCandidateChanged(err) {
		t.Fatalf("the internal candidate-changed sentinel reached the "+
			"client: %v", err)
	}
}

// volWantCnUnits asserts one CN's used units, on its record and on its
// capacity value alike, and that its revision stayed at wantRev.
func volWantCnUnits(
	t *testing.T,
	env *volEnv,
	addrPort string,
	want uint64,
	wantRev uint64,
) {
	t.Helper()
	cn := env.cn(addrPort)
	if got := cn.GetUsedMetaUnits(); got != want {
		t.Errorf("%s used_meta_units: got %d, want %d", addrPort, got, want)
	}
	if got := env.cnCapValue(addrPort); !proto.Equal(
		got, model.CnCapacityValue(cn),
	) {
		t.Errorf("%s capacity value: got %v, want the record's %v",
			addrPort, got, model.CnCapacityValue(cn))
	}
	if got := env.cnRev(addrPort); got != wantRev {
		t.Errorf("%s rev: got %d, want %d", addrPort, got, wantRev)
	}
}

// ---------------------------------------------------------------------------
// CreateClone (architecture.md, Clones; Metadata units)
// ---------------------------------------------------------------------------

// TestCreateCloneChargesEveryCntlrCn pins CreateClone's charge: the clone's
// units go to the CN of EVERY cntlr of the pool, the standby's as well as the
// primary's and a disabled one's as well as an enabled one's, because a
// failover or a cntlr replacement can make any of them the primary, which
// builds every clone of the pool on its own CN. The record stores the units.
// Only the count moves on a CN — no extent, no pointer — so its record and
// capacity value are rewritten and its revision is not bumped. A second clone
// adds to the count rather than setting it.
func TestCreateCloneChargesEveryCntlrCn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
	}{
		{name: "an enabled standby"},
		{name: "a disabled standby", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			if tc.disabled {
				env.patchCntlr(volCntlrB, func(cntlr *pb.Cntlr) {
					cntlr.Disabled = true
				})
			}
			env.putTd("dst-a", 900, 7, 0, true)
			env.putTd("dst-b", 901, 8, 0, true)
			before := map[string]*pb.CnConf{
				volCnA: env.cn(volCnA),
				volCnB: env.cn(volCnB),
			}
			for idx, name := range []string{"clone-a", "clone-b"} {
				volCreateClone(env, name, fmt.Sprintf("dst-%c", 'a'+idx))
				if got := env.clone(name).GetMetaUnits(); got !=
					volCloneUnits {
					t.Errorf("%s meta_units: got %d, want %d", name, got,
						volCloneUnits)
				}
				held := volCloneUnits * uint64(idx+1)
				for addrPort, cnBefore := range before {
					want := proto.Clone(cnBefore).(*pb.CnConf)
					want.UsedMetaUnits = held
					if got := env.cn(addrPort); !proto.Equal(got, want) {
						t.Errorf("after %s, %s: got %v, want only its units "+
							"moved: %v", name, addrPort, got, want)
					}
					volWantCnUnits(t, env, addrPort, held, 1)
				}
			}
			if rev := env.spRev(); rev != 3 {
				t.Errorf("sp_rev: got %d, want 3 (one bump per clone)", rev)
			}
		})
	}
}

// TestCreateCloneRefusesWhenACntlrCnLacksRoom pins the refusal: a pool's clone
// that the metadata area of one of its cntlrs' CNs cannot hold is
// RESOURCE_EXHAUSTED, naming that CN and saying "metadata room", and nothing
// is written — not the clone, not the other CN's charge. One unit more of room
// and the same clone fits exactly, the full CN ending at the ceiling, which a
// charge may reach but not pass. The room is judged after every other
// refusal, so a request that is wrong on its own hears that instead.
func TestCreateCloneRefusesWhenACntlrCnLacksRoom(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.setCnUsed(volCnB, common.CnCloneMetaUnitCnt-volCloneUnits+1)
	before := env.spConf()
	beforeRev := env.spRev()
	cnBefore := map[string]*pb.CnConf{
		volCnA: env.cn(volCnA),
		volCnB: env.cn(volCnB),
	}

	_, err := env.srv.CreateClone(env.ctx, volCloneRequest(env, "clone-a",
		"dst"))
	msg := volWantCode(t, err, codes.ResourceExhausted)
	want := fmt.Sprintf("clone %q: controller node %q has no metadata room: "+
		"%d free metadata units, need %d", "clone-a", volCnB,
		volCloneUnits-1, volCloneUnits)
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	env.wantUntouched(before, beforeRev)
	for addrPort, cn := range cnBefore {
		if got := env.cn(addrPort); !proto.Equal(got, cn) {
			t.Errorf("%s moved on a refusal: got %v, want %v", addrPort,
				got, cn)
		}
		if got := env.cnCapValue(addrPort); !proto.Equal(
			got, model.CnCapacityValue(cn),
		) {
			t.Errorf("%s capacity value moved on a refusal: %v",
				addrPort, got)
		}
	}
	if env.exists(model.CloneKey(env.cid, volSpId, "clone-a"), &pb.Clone{}) {
		t.Errorf("a refused clone left its key")
	}

	// The order: an absent destination is NOT_FOUND although no CN could
	// have held the clone either.
	_, err = env.srv.CreateClone(env.ctx, volCloneRequest(env, "clone-a",
		"no-such-td"))
	volWantCode(t, err, codes.NotFound)

	env.setCnUsed(volCnB, common.CnCloneMetaUnitCnt-volCloneUnits)
	volCreateClone(env, "clone-a", "dst")
	volWantCnUnits(t, env, volCnB, common.CnCloneMetaUnitCnt, 1)
	volWantCnUnits(t, env, volCnA, volCloneUnits, 1)
	if free := model.CnCapacityFreeMetaUnits(env.cnCapValue(volCnB)); free !=
		0 {
		t.Errorf("cn-b at the ceiling reads %d free units, want 0", free)
	}
}

// TestCreateCloneTooBigForTheArea pins the clone no node can ever hold: one
// whose units exceed a whole metadata area is refused on CNs that hold
// nothing at all, rather than created to wait for room that cannot come.
func TestCreateCloneTooBigForTheArea(t *testing.T) {
	env := newVolEnv(t)
	const size = uint64(1) << 51
	units := common.CloneMetaUnits(size, volPoolBlock)
	if units <= common.CnCloneMetaUnitCnt {
		t.Fatalf("a %d-byte td takes %d units, which fit the %d of an area: "+
			"the fixture no longer exceeds it", size, units,
			common.CnCloneMetaUnitCnt)
	}
	mustPut(t, env.cli, model.ThinDeviceKey(env.cid, volSpId, "huge"),
		&pb.ThinDevice{TdId: 900, DevId: 7, Size: size, Created: true})
	conf := env.spConf()
	conf.TdNameList = append(conf.GetTdNameList(), "huge")
	env.putSpConf(conf)
	before := env.spConf()
	beforeRev := env.spRev()

	_, err := env.srv.CreateClone(env.ctx, volCloneRequest(env, "clone-a",
		"huge"))
	msg := volWantCode(t, err, codes.ResourceExhausted)
	want := fmt.Sprintf("clone %q: controller node %q has no metadata room: "+
		"%d free metadata units, need %d", "clone-a", volCnA,
		common.CnCloneMetaUnitCnt, units)
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	env.wantUntouched(before, beforeRev)
	volWantCnUnits(t, env, volCnA, 0, 1)
	volWantCnUnits(t, env, volCnB, 0, 1)
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

// volEndMigration ends migr-a, by cancel or by a forced finish.
func volEndMigration(env *volEnv, finish bool) error {
	env.t.Helper()
	if finish {
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
// CreateCntlr and DeleteCntlr (architecture.md, Cntlrs; Metadata units)
// ---------------------------------------------------------------------------

// The pool the cntlr tests run on is sptDefaultSpec's with two clones by RPC:
// one onto a 1 GiB td, two units at the fixture's 4 MiB pool blocks, and one
// onto a 20 TiB td, five Mi regions and so three units. The second is
// latched, and still counts: a latched clone holds its units on the CN of
// every cntlr until the clone drain's final step gives them back.
const (
	sptMetaSmallTd = uint64(1) << 30
	sptMetaLargeTd = uint64(20) << 40
	sptMetaUnits   = uint64(5)
)

// createMetaPool creates that pool and returns its sp_id, after checking that
// the CN of each of its cntlrs holds sptMetaUnits.
func (e *sptEnv) createMetaPool() uint64 {
	e.t.Helper()
	spId := e.createSp(sptDefaultSpec(sptSpName))
	sum := uint64(0)
	for _, td := range []struct {
		name string
		size uint64
	}{
		{"td-small", sptMetaSmallTd},
		{"td-large", sptMetaLargeTd},
	} {
		if _, err := e.srv.CreateThinDevice(e.ctx,
			&pb.CreateThinDeviceRequest{
				ClusterName: e.name,
				SpName:      sptSpName,
				TdName:      td.name,
				Size:        td.size,
			}); err != nil {
			e.t.Fatalf("CreateThinDevice %s: %v", td.name, err)
		}
		if _, err := e.srv.CreateClone(e.ctx, &pb.CreateCloneRequest{
			ClusterName:   e.name,
			SpName:        sptSpName,
			CloneName:     "clone-" + td.name,
			SrcTrConf:     []*pb.NvmeTrConf{sptTrConf("src:9000")},
			SrcNqn:        "nqn.2024-01.io.dnv:src",
			SrcNsIdx:      1,
			SrcSliceCnt:   1,
			SrcStripeSize: 64 * 1024,
			SrcBlockSize:  1024 * 1024,
			DstTdName:     td.name,
		}); err != nil {
			e.t.Fatalf("CreateClone onto %s: %v", td.name, err)
		}
		sum += common.CloneMetaUnits(td.size, sptBlockSize)
	}
	if sum != sptMetaUnits {
		e.t.Fatalf("the pool's clones take %d units, want %d: the fixture "+
			"no longer holds what the tests count", sum, sptMetaUnits)
	}
	if _, err := e.srv.DeleteClone(e.ctx, &pb.DeleteCloneRequest{
		ClusterName: e.name,
		SpName:      sptSpName,
		CloneName:   "clone-td-large",
		Force:       true,
	}); err != nil {
		e.t.Fatalf("DeleteClone: %v", err)
	}
	addrs, _ := e.cntlrPlaces(sptSpName)
	for _, addrPort := range addrs {
		if got := e.cnConf(addrPort).GetUsedMetaUnits(); got != sptMetaUnits {
			e.t.Fatalf("%s holds %d units of the pool's clones, want %d",
				addrPort, got, sptMetaUnits)
		}
	}
	return spId
}

// freeCns is every CN of the fixture that hosts no cntlr of the pool, in
// fixture order.
func (e *sptEnv) freeCns() []string {
	e.t.Helper()
	addrs, _ := e.cntlrPlaces(sptSpName)
	var free []string
	for _, addrPort := range e.cnAddrs {
		if !containsName(addrs, addrPort) {
			free = append(free, addrPort)
		}
	}
	return free
}

// createCntlrReq is a token-less CreateCntlr of the pool at cntlid slot 2,
// which sptDefaultSpec's two cntlrs leave free.
func (e *sptEnv) createCntlrReq() *pb.CreateCntlrRequest {
	return &pb.CreateCntlrRequest{
		ClusterName: e.name,
		SpName:      sptSpName,
		CntlidSlot:  2,
	}
}

// newestCntlr is the CN of the pool's last cntlr, CreateCntlr's append.
func (e *sptEnv) newestCntlr() (uint64, string) {
	e.t.Helper()
	conf := e.spConf(sptSpName)
	ids := conf.GetCntlrIdList()
	cntlrId := ids[len(ids)-1]
	return cntlrId, e.cntlr(conf.GetSpId(), cntlrId).GetAddrPort()
}

// sptWantCnUnits is volWantCnUnits for this fixture.
func sptWantCnUnits(
	t *testing.T,
	env *sptEnv,
	addrPort string,
	want uint64,
	wantRev uint64,
) {
	t.Helper()
	cn := env.cnConf(addrPort)
	if got := cn.GetUsedMetaUnits(); got != want {
		t.Errorf("%s used_meta_units: got %d, want %d", addrPort, got, want)
	}
	if got := env.cnCapValue(addrPort); !proto.Equal(
		got, model.CnCapacityValue(cn),
	) {
		t.Errorf("%s capacity value: got %v, want the record's %v",
			addrPort, got, model.CnCapacityValue(cn))
	}
	if got := env.cnRev(addrPort); got != wantRev {
		t.Errorf("%s rev: got %d, want %d", addrPort, got, wantRev)
	}
}

// TestCreateCntlrChargesThePoolsCloneUnits pins CreateCntlr's charge: the new
// cntlr's CN is charged the pool's whole clone sum along with the footprint,
// under its one CnRev bump, since the cntlr may become the primary and build
// every clone of the pool there. The latched clone counts too, because the
// clone drain's final step gives its units back on the CN of every cntlr the
// pool then has. No other CN moves.
func TestCreateCntlrChargesThePoolsCloneUnits(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createMetaPool()
	revBefore := make(map[string]uint64)
	for _, addrPort := range env.cnAddrs {
		revBefore[addrPort] = env.cnRev(addrPort)
	}
	poolCns, _ := env.cntlrPlaces(sptSpName)

	if _, err := env.srv.CreateCntlr(env.ctx,
		env.createCntlrReq()); err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	_, addrPort := env.newestCntlr()
	cn := env.cnConf(addrPort)
	if got := cn.GetFreeExtCnt(); got != sptCnFree-sptFootprint {
		t.Errorf("%s free_ext_cnt: got %d, want %d", addrPort, got,
			sptCnFree-sptFootprint)
	}
	sptWantCnUnits(t, env, addrPort, sptMetaUnits, revBefore[addrPort]+1)
	for _, other := range env.cnAddrs {
		if other == addrPort {
			continue
		}
		want := uint64(0)
		if containsName(poolCns, other) {
			want = sptMetaUnits
		}
		sptWantCnUnits(t, env, other, want, revBefore[other])
	}
}

// TestDeleteCntlrReleasesThePoolsCloneUnits pins DeleteCntlr's release, the
// mirror of the charge: the CN gives back the pool's whole sum, the latched
// clone included, with its footprint and under its one CnRev bump — and no
// more, so the units it also holds for another pool's clones — set by hand
// here — stay charged.
func TestDeleteCntlrReleasesThePoolsCloneUnits(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createMetaPool()
	if _, err := env.srv.CreateCntlr(env.ctx,
		env.createCntlrReq()); err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	cntlrId, addrPort := env.newestCntlr()
	const other = 7
	env.setCnUsed(addrPort, sptMetaUnits+other)
	sptDeleteCntlr(t, env, spId, cntlrId)
	cn := env.cnConf(addrPort)
	if cn.GetFreeExtCnt() != sptCnFree || len(cn.GetCntlrPtrList()) != 0 {
		t.Errorf("%s: got %v, want its footprint and pointer back",
			addrPort, cn)
	}
	// One bump for the create and one for the delete.
	sptWantCnUnits(t, env, addrPort, other, 3)
}

// sptDeleteCntlr disables one cntlr and deletes it, token-less.
func sptDeleteCntlr(t *testing.T, env *sptEnv, spId uint64, cntlrId uint64) {
	t.Helper()
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
		t.Fatalf("DeleteCntlr: %v", err)
	}
	if containsId(env.spConf(sptSpName).GetCntlrIdList(), cntlrId) {
		t.Fatalf("cntlr %d of sp %d is still listed", cntlrId, spId)
	}
}

// TestCntlrUnitsReleaseStopsAtZero pins DeleteCntlr's floor: a CN whose count
// is already below the pool's sum — set by hand here — ends at zero, never at
// a count wrapped around to one that refuses every later charge.
func TestCntlrUnitsReleaseStopsAtZero(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createMetaPool()
	if _, err := env.srv.CreateCntlr(env.ctx,
		env.createCntlrReq()); err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	cntlrId, addrPort := env.newestCntlr()
	env.setCnUsed(addrPort, sptMetaUnits-3)
	sptDeleteCntlr(t, env, spId, cntlrId)
	sptWantCnUnits(t, env, addrPort, 0, 3)
}

// fillCn leaves one CN one unit short of the pool's clone sum.
func (e *sptEnv) fillCn(addrPort string) {
	e.t.Helper()
	e.setCnUsed(addrPort, common.CnCloneMetaUnitCnt-sptMetaUnits+1)
}

// TestCreateCntlrSkipsACnWithoutMetadataRoom pins the CN scan's skip:
// cn_batch_size is 1, so a scan offers the fullest eligible CN alone, and the
// two fullest CNs the pool does not use lack room for its clones by one unit.
// The scan skips both and offers the third, in ONE scan: without the skip it
// would offer a full CN in every round, each refused, until the round cap.
// The full CNs are left as they were.
func TestCreateCntlrSkipsACnWithoutMetadataRoom(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createMetaPool()
	env.setCnBatchSize(1)
	free := env.freeCns()
	if len(free) != 3 {
		t.Fatalf("the fixture leaves %d CNs free, want 3", len(free))
	}
	for idx, addrPort := range free[:2] {
		env.setCnFree(addrPort, 100-uint64(idx))
		env.fillCn(addrPort)
	}
	scans, err := env.createCntlrScans(env.createCntlrReq())
	mustNotHaveCandidateChanged(t, err)
	if err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	if scans != 1 {
		t.Errorf("CreateCntlr scanned %d times, want 1", scans)
	}
	if _, addrPort := env.newestCntlr(); addrPort != free[2] {
		t.Errorf("the cntlr landed on %s, want %s", addrPort, free[2])
	}
	for _, addrPort := range free[:2] {
		cn := env.cnConf(addrPort)
		if len(cn.GetCntlrPtrList()) != 0 {
			t.Errorf("%s holds a cntlr: %v", addrPort, cn)
		}
		sptWantCnUnits(t, env, addrPort,
			common.CnCloneMetaUnitCnt-sptMetaUnits+1, 1)
	}
}

// TestCreateCntlrRefusesWhenNoCnHasMetadataRoom pins the refusal when the skip
// leaves the scan nothing: RESOURCE_EXHAUSTED saying "metadata room", since
// every CN the pool does not use has the extents, and nothing is written.
func TestCreateCntlrRefusesWhenNoCnHasMetadataRoom(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createMetaPool()
	for _, addrPort := range env.freeCns() {
		env.fillCn(addrPort)
	}
	before := env.dump()
	_, err := env.srv.CreateCntlr(env.ctx, env.createCntlrReq())
	sptWantCode(t, err, codes.ResourceExhausted)
	want := fmt.Sprintf("%s: no controller node with %d free extents has "+
		"metadata room for %d units", opCreateCntlr, sptFootprint,
		sptMetaUnits)
	if msg := status.Convert(err).Message(); msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	if !strings.Contains(want, "metadata room") {
		t.Fatalf("the refusal no longer says \"metadata room\", which the " +
			"gateway suite greps")
	}
	scWantUntouched(t, before, env.dump())
}

// TestCreateCntlrReplansWhenItsCnFillsUp pins the deciding STM's re-check of
// the pick's room (cnLedger.verifyPick): the CN the first scan offers fills up
// inside the request's first read of its record, in the deciding STM. The
// capacity key does not move with the units, so only that record can tell;
// the round is a changed candidate, the second scan skips the full CN, and
// the cntlr lands on the next one. The full CN is never charged.
func TestCreateCntlrReplansWhenItsCnFillsUp(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createMetaPool()
	env.setCnBatchSize(1)
	free := env.freeCns()
	env.setCnFree(free[0], 100)
	env.setCnFree(free[1], 90)
	fullKey := model.CnConfKey(env.cid, free[0])
	prev := slog.Default()
	hook := &sptRecordHook{
		Handler: prev.Handler(),
		trigger: func(rec slog.Record) bool {
			return rec.Message == "etcd get" &&
				sptRecordAttr(rec, "key") == fullKey
		},
		count: sptIsCnScan(env),
		actor: func() {
			env.setCnUsed(free[0], common.CnCloneMetaUnitCnt)
		},
	}
	slog.SetDefault(slog.New(hook))
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, sptHookKey{}, true), 30*time.Second)
	_, err := env.srv.CreateCntlr(ctx, env.createCntlrReq())
	cancel()
	slog.SetDefault(prev)
	if !hook.fired.Load() {
		t.Fatalf("the request never read %s's record: nothing was raced",
			free[0])
	}
	mustNotHaveCandidateChanged(t, err)
	if err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	if got := hook.counted.Load(); got != 2 {
		t.Errorf("CreateCntlr scanned %d times, want 2: the round the "+
			"full CN refused and the one that committed", got)
	}
	if _, addrPort := env.newestCntlr(); addrPort != free[1] {
		t.Errorf("the cntlr landed on %s, want %s", addrPort, free[1])
	}
	full := env.cnConf(free[0])
	if full.GetFreeExtCnt() != 100 || len(full.GetCntlrPtrList()) != 0 {
		t.Errorf("%s was charged: %v", free[0], full)
	}
	sptWantCnUnits(t, env, free[0], common.CnCloneMetaUnitCnt, 1)
	sptWantCnUnits(t, env, free[1], sptMetaUnits, 2)
}

// TestCreateCntlrChargesTheSumItsTransactionReads pins the deciding STM's own
// read of the pool's clones: a clone created inside the request's first read
// of the pick's record, in the deciding STM, conflicts that STM, and its next
// attempt charges the pick the pool's sum as that attempt reads it, the new
// clone's units included, not the sum the plan read. The pick has room for
// the larger sum, so the round commits after one scan. Every CN of a cntlr of
// the pool then holds that sum: the clone create charged the two it saw, and
// the cntlr create charged the third.
func TestCreateCntlrChargesTheSumItsTransactionReads(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createMetaPool()
	env.setCnBatchSize(1)
	free := env.freeCns()
	env.setCnFree(free[0], 100)
	if _, err := env.srv.CreateThinDevice(env.ctx,
		&pb.CreateThinDeviceRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			TdName:      "td-race",
			Size:        sptMetaSmallTd,
		}); err != nil {
		t.Fatalf("CreateThinDevice td-race: %v", err)
	}
	raceUnits := common.CloneMetaUnits(sptMetaSmallTd, sptBlockSize)
	pickKey := model.CnConfKey(env.cid, free[0])
	prev := slog.Default()
	hook := &sptRecordHook{
		Handler: prev.Handler(),
		trigger: func(rec slog.Record) bool {
			return rec.Message == "etcd get" &&
				sptRecordAttr(rec, "key") == pickKey
		},
		count: sptIsCnScan(env),
		actor: func() {
			if _, err := env.srv.CreateClone(env.ctx,
				&pb.CreateCloneRequest{
					ClusterName:   env.name,
					SpName:        sptSpName,
					CloneName:     "clone-td-race",
					SrcTrConf:     []*pb.NvmeTrConf{sptTrConf("src:9000")},
					SrcNqn:        "nqn.2024-01.io.dnv:src",
					SrcNsIdx:      1,
					SrcSliceCnt:   1,
					SrcStripeSize: 64 * 1024,
					SrcBlockSize:  1024 * 1024,
					DstTdName:     "td-race",
				}); err != nil {
				t.Errorf("CreateClone onto td-race: %v", err)
			}
		},
	}
	slog.SetDefault(slog.New(hook))
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, sptHookKey{}, true), 30*time.Second)
	_, err := env.srv.CreateCntlr(ctx, env.createCntlrReq())
	cancel()
	slog.SetDefault(prev)
	if !hook.fired.Load() {
		t.Fatalf("the request never read %s's record: nothing was raced",
			free[0])
	}
	mustNotHaveCandidateChanged(t, err)
	if err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	if got := hook.counted.Load(); got != 1 {
		t.Errorf("CreateCntlr scanned %d times, want 1: the pick has room "+
			"for the larger sum", got)
	}
	if _, addrPort := env.newestCntlr(); addrPort != free[0] {
		t.Errorf("the cntlr landed on %s, want %s", addrPort, free[0])
	}
	want := sptMetaUnits + raceUnits
	sptWantCnUnits(t, env, free[0], want, 2)
	addrs, _ := env.cntlrPlaces(sptSpName)
	if len(addrs) != 3 {
		t.Fatalf("the pool has %d cntlrs, want 3", len(addrs))
	}
	for _, addrPort := range addrs {
		if got := env.cnConf(addrPort).GetUsedMetaUnits(); got != want {
			t.Errorf("%s used_meta_units: got %d, want %d", addrPort, got,
				want)
		}
	}
}

// TestCntlrMutatorsAbortOnALostCloneKey pins loadClones' refusal on both cntlr
// mutators: a clone the pool lists whose key is gone leaves the pool's unit
// sum unknown, so CreateCntlr — in its plan, before any scan — and
// DeleteCntlr answer ABORTED, as for any lost invariant key (architecture.md,
// UNEXPECTED_ERROR → `ABORTED`), rather than charge or give back a partial
// sum. Nothing is written.
func TestCntlrMutatorsAbortOnALostCloneKey(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createMetaPool()
	if _, err := env.srv.CreateCntlr(env.ctx,
		env.createCntlrReq()); err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	cntlrId, _ := env.newestCntlr()
	if _, err := env.srv.UpdateCntlrEnabled(env.ctx,
		&pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			CntlrId:     cntlrId,
			Enabled:     false,
		}); err != nil {
		t.Fatalf("UpdateCntlrEnabled: %v", err)
	}
	key := model.CloneKey(env.cid, spId, "clone-td-small")
	if err := env.cli.Delete(env.ctx, key); err != nil {
		t.Fatalf("Delete %s: %v", key, err)
	}
	before := env.dump()
	want := fmt.Sprintf("clone key %q is missing", key)

	scans, err := env.createCntlrScans(&pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		CntlidSlot:  3,
	})
	sptWantCode(t, err, codes.Aborted)
	if msg := status.Convert(err).Message(); msg != want {
		t.Errorf("CreateCntlr: message %q, want %q", msg, want)
	}
	if scans != 0 {
		t.Errorf("CreateCntlr scanned %d times on a pool it cannot sum", scans)
	}
	_, err = env.srv.DeleteCntlr(env.ctx, &pb.DeleteCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		CntlrId:     cntlrId,
	})
	sptWantCode(t, err, codes.Aborted)
	if msg := status.Convert(err).Message(); msg != want {
		t.Errorf("DeleteCntlr: message %q, want %q", msg, want)
	}
	scWantUntouched(t, before, env.dump())
}
