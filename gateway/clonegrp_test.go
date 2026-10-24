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

// The clone meta group as the gateway keeps it (architecture.md, Clones;
// gateway.md, Clones), on the fixture of handler_vol_test.go: CreateClone
// places the group's sides on DNs as a new group's, refuses fewer candidate
// locations than legs and a destination whose metadata one side cannot hold,
// and the calls that look an object up by id — the repair calls, the cntlid
// slot list update, CreateMigration and GetLegBitmap — find a clone meta
// group's legs and sides after the slices'.

// volCloneMetaBlocks and volCloneDataBlocks are the layout of a one-extent
// group at the fixture's geometry, the same as volSlice's meta group: a
// clone meta group is laid out as every one-extent group of the pool is
// (architecture.md, Group on-leg layout: meta region, data region, health
// block).
const (
	volCloneMetaBlocks = uint64(3)
	volCloneDataBlocks = uint64(2045)
)

// volDnState is one DN as a test saw it before a call: its record and its
// revision.
type volDnState struct {
	conf *pb.DnConf
	rev  uint64
}

// volDnSnapshot is the state of the fixture's four DNs.
func volDnSnapshot(env *volEnv) map[string]volDnState {
	env.t.Helper()
	out := make(map[string]volDnState, 4)
	for _, addrPort := range []string{volDnA, volDnB, volDnC, volDnD} {
		out[addrPort] = volDnState{
			conf: env.dn(addrPort),
			rev:  env.dnRev(addrPort),
		}
	}
	return out
}

// volSameAddrs reports whether got holds exactly the addresses of want, in
// any order.
func volSameAddrs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, addrPort := range want {
		seen[addrPort]++
	}
	for _, addrPort := range got {
		seen[addrPort]--
		if seen[addrPort] < 0 {
			return false
		}
	}
	return true
}

// volWantCloneGrp asserts a freshly created clone's clone meta group at the
// fixture's geometry and its DNs' bookkeeping against before, and returns
// the legs' addresses in leg order.
//
// The group: grp_id the id after the clone's, one extent, the one-extent
// layout, MaxAllocLegPerGrp legs (the fixture is md-raid1); leg
// idx i holds leg_id and side_id in GW15's order, on a DN no other leg of the
// group uses, with the pool's first cntlid slot, the DN's transport and
// provisioned false. Each such DN lost one extent and gained exactly that
// side's pointer, its capacity key moved with its free count, and its
// revision went up by one; every other DN is as it was.
func volWantCloneGrp(
	t *testing.T,
	env *volEnv,
	clone *pb.Clone,
	before map[string]volDnState,
) []string {
	t.Helper()
	grp := clone.GetMetaGrp()
	if grp == nil {
		t.Fatalf("the clone has no clone meta group: %v", clone)
	}
	grpId := clone.GetCloneId() + 1
	if grp.GetGrpId() != grpId ||
		grp.GetExtCnt() != common.CloneMetaGrpExtCnt ||
		grp.GetMetaBlocks() != volCloneMetaBlocks ||
		grp.GetDataBlocks() != volCloneDataBlocks {
		t.Errorf("group: grp_id %d ext_cnt %d meta %d data %d, want %d, "+
			"%d, %d, %d", grp.GetGrpId(), grp.GetExtCnt(),
			grp.GetMetaBlocks(), grp.GetDataBlocks(), grpId,
			common.CloneMetaGrpExtCnt, volCloneMetaBlocks,
			volCloneDataBlocks)
	}
	if len(grp.GetLegList()) != common.MaxAllocLegPerGrp {
		t.Fatalf("%d legs, want %d", len(grp.GetLegList()),
			common.MaxAllocLegPerGrp)
	}
	addrs := make([]string, 0, len(grp.GetLegList()))
	for idx, leg := range grp.GetLegList() {
		legId := grpId + 1 + 2*uint64(idx)
		if leg.GetLegId() != legId || leg.GetLegIdx() != uint32(idx) ||
			len(leg.GetSideList()) != 1 {
			t.Fatalf("leg %d: got %v, want leg_id %d, leg_idx %d and one "+
				"side", idx, leg, legId, idx)
		}
		side := leg.GetSideList()[0]
		addrPort := side.GetAddrPort()
		wantSide := &pb.Side{
			SideId:      legId + 1,
			AddrPort:    addrPort,
			CntlidSlot:  volSlot,
			NvmeTrConf:  volTrConf(addrPort),
			ErrEpoch:    0,
			Provisioned: false,
		}
		if !proto.Equal(side, wantSide) {
			t.Errorf("leg %d side: got %v, want %v", idx, side, wantSide)
		}
		for _, other := range addrs {
			if other == addrPort {
				t.Errorf("two legs of the group on %s", addrPort)
			}
		}
		addrs = append(addrs, addrPort)
		was, ok := before[addrPort]
		if !ok {
			t.Fatalf("leg %d is on %s, a DN the test did not snapshot",
				idx, addrPort)
		}
		want := proto.Clone(was.conf).(*pb.DnConf)
		want.FreeExtCnt -= common.CloneMetaGrpExtCnt
		want.SidePtrList = append(want.SidePtrList, &pb.SidePointer{
			SpId:   volSpId,
			LegId:  legId,
			SideId: legId + 1,
		})
		dn := env.dn(addrPort)
		if !proto.Equal(dn, want) {
			t.Errorf("%s: got %v, want %v", addrPort, dn, want)
		}
		if env.exists(env.dnCapKey(addrPort, was.conf.GetFreeExtCnt()),
			&pb.DnCapacity{}) {
			t.Errorf("%s: the old capacity key must be deleted", addrPort)
		}
		if got := env.dnCapValue(addrPort); !proto.Equal(
			got, model.DnCapacityValue(dn),
		) {
			t.Errorf("%s capacity value: got %v, want the record's %v",
				addrPort, got, model.DnCapacityValue(dn))
		}
		if got := env.dnRev(addrPort); got != was.rev+1 {
			t.Errorf("%s rev: got %d, want %d", addrPort, got, was.rev+1)
		}
	}
	used := make(map[string]bool, len(addrs))
	for _, addrPort := range addrs {
		used[addrPort] = true
	}
	for addrPort, was := range before {
		if used[addrPort] {
			continue
		}
		if got := env.dn(addrPort); !proto.Equal(got, was.conf) {
			t.Errorf("%s holds no leg and moved: got %v, want %v",
				addrPort, got, was.conf)
		}
		if got := env.dnRev(addrPort); got != was.rev {
			t.Errorf("%s holds no leg and its rev moved to %d", addrPort,
				got)
		}
	}
	return addrs
}

// volCloneGrpOf is the clone meta group of one clone, failing the test when
// it has none.
func volCloneGrpOf(env *volEnv, name string) *pb.Group {
	env.t.Helper()
	grp := env.clone(name).GetMetaGrp()
	if grp == nil {
		env.t.Fatalf("clone %q has no clone meta group", name)
	}
	return grp
}

// TestCreateCloneRefusesFewerLocationsThanLegs pins the placement of
// architecture.md, Per-operation allocation, on CreateClone: the legs of a
// clone meta group land in distinct locations, so a cluster with fewer
// locations offering a free extent than the group has legs is
// RESOURCE_EXHAUSTED, naming the clone, and writes nothing. With a second
// location back, the create commits, one leg in each location.
func TestCreateCloneRefusesFewerLocationsThanLegs(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	for _, addrPort := range []string{volDnB, volDnC, volDnD} {
		env.relocateDn(addrPort, volDnA)
	}
	before := env.dump()
	_, err := env.srv.CreateClone(env.ctx,
		volCloneRequest(env, "clone-a", "dst"))
	msg := volWantCode(t, err, codes.ResourceExhausted)
	want := fmt.Sprintf("clone %q: 1 disk nodes with %d free extents, "+
		"need %d", "clone-a", common.CloneMetaGrpExtCnt,
		common.MaxAllocLegPerGrp)
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	env.wantNoWrite("the refused CreateClone", before)

	env.relocateDn(volDnD, volDnD)
	dnBefore := volDnSnapshot(env)
	volCreateClone(env, "clone-a", "dst")
	addrs := volWantCloneGrp(t, env, env.clone("clone-a"), dnBefore)
	locs := make(map[string]bool, len(addrs))
	for _, addrPort := range addrs {
		locs[env.dn(addrPort).GetLocation()] = true
	}
	if len(locs) != len(addrs) || !locs[volDnD] {
		t.Errorf("the legs are on %v, want one in dn-d's location and one "+
			"in the other", addrs)
	}
}

// TestCreateCloneLandsOnThePoolsOwnDns pins the clone meta group's
// placement (architecture.md, Per-operation allocation): the scan
// keeps no preference against the DNs the pool's other groups already use, so
// a cluster whose only DNs with room are the pool's own still takes the
// clone. dn-c and dn-d are gone, and dn-a and dn-b, which hold every side of
// the fixture slice, take the group.
func TestCreateCloneLandsOnThePoolsOwnDns(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.dropDn(volDnC)
	env.dropDn(volDnD)
	before := map[string]volDnState{
		volDnA: {conf: env.dn(volDnA), rev: env.dnRev(volDnA)},
		volDnB: {conf: env.dn(volDnB), rev: env.dnRev(volDnB)},
	}
	volCreateClone(env, "clone-a", "dst")
	addrs := volWantCloneGrp(t, env, env.clone("clone-a"), before)
	if !volSameAddrs(addrs, volDnA, volDnB) {
		t.Errorf("the legs are on %v, want dn-a and dn-b", addrs)
	}
}

// TestCreateCloneRefusesADestinationOverOneSide pins the bound of
// architecture.md, Clones: the dm-clone metadata of the destination, the base
// plus one byte per whole block, must fit the data region of one side of a
// one-extent group, and a destination past it is FAILED_PRECONDITION naming
// the bound, with nothing written.
//
// The cluster's extent is the smallest a disk node may be formatted at, 64
// MiB. At the fixture's 2 MiB block a 64 MiB group is 32 blocks, 3 of them
// the md layout (one superblock, one bitmap block, the health block), so one
// side holds 29 blocks, 58 MiB, of data: the metadata may count 54 Mi regions
// past the 4 MiB base, and the largest destination is one byte short of 54
// Mi + 1 whole blocks. That size is created, laid out as 3 and 29 blocks;
// one byte more is refused.
func TestCreateCloneRefusesADestinationOverOneSide(t *testing.T) {
	env := newVolEnv(t)
	env.cc.DnBinConf.ExtentSize = common.MinDnExtSize
	mustPut(t, env.cli, model.ClusterConfKey(env.cluster), env.cc)
	const (
		room    = uint64(29) * volPoolBlock
		largest = (room-common.CloneMetaBaseBytes+1)*volPoolBlock - 1
	)
	putTd := func(name string, tdId uint64, size uint64) {
		mustPut(t, env.cli, model.ThinDeviceKey(env.cid, volSpId, name),
			&pb.ThinDevice{TdId: tdId, DevId: uint32(tdId), Size: size,
				Created: true})
		conf := env.spConf()
		conf.TdNameList = append(conf.GetTdNameList(), name)
		env.putSpConf(conf)
	}
	putTd("over", 900, largest+1)
	putTd("fits", 901, largest)

	before := env.dump()
	_, err := env.srv.CreateClone(env.ctx,
		volCloneRequest(env, "clone-a", "over"))
	msg := volWantCode(t, err, codes.FailedPrecondition)
	want := fmt.Sprintf("clone %q: the dm-clone metadata of the %d-byte "+
		"destination %q at a %d-byte block takes %d bytes, over the %d "+
		"bytes of data one side of a clone meta group holds; the largest "+
		"destination it holds is %d bytes", "clone-a", largest+1, "over",
		volPoolBlock, room+1, room, largest)
	if msg != want {
		t.Errorf("message:\n got %q\nwant %q", msg, want)
	}
	env.wantNoWrite("the refused CreateClone", before)

	volCreateClone(env, "clone-a", "fits")
	grp := volCloneGrpOf(env, "clone-a")
	if grp.GetMetaBlocks() != 3 || grp.GetDataBlocks() != 29 {
		t.Errorf("a 64 MiB group: meta %d data %d, want 3 and 29",
			grp.GetMetaBlocks(), grp.GetDataBlocks())
	}
}

// TestCreateCloneReplansWhenAPickMoves pins GW9 on CreateClone: the deciding
// STM re-verifies every pick against the capacity key the scan saw, and a
// pick whose free count moved between the scan and the STM sends the round
// back to plan and scan again. dn_batch_size 1 and dn-c and dn-d ahead of the
// others make the first scan offer exactly those two; dn-c then falls to 50
// free extents the moment the STM first reads it, so the next scan offers
// dn-d and one of dn-a and dn-b, and dn-c is left exactly as the other
// writer left it.
func TestCreateCloneReplansWhenAPickMoves(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.setDnBatchSize(1)
	env.setDnFree(volDnC, volDnCFree)
	env.setDnFree(volDnD, volDnCFree-1)
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     model.DnConfKey(env.cid, volDnC),
		actor:   func() { env.setDnFree(volDnC, volDnFree/2) },
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, volReadHookKey{}, true), 30*time.Second)
	defer cancel()
	if _, err := env.srv.CreateClone(ctx,
		volCloneRequest(env, "clone-a", "dst")); err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	slog.SetDefault(prev)
	if !hook.fired.Load() {
		t.Fatalf("the deciding STM never read dn-c: nothing was raced")
	}
	grp := volCloneGrpOf(env, "clone-a")
	var addrs []string
	for _, leg := range grp.GetLegList() {
		addrs = append(addrs, leg.GetSideList()[0].GetAddrPort())
	}
	if !volSameAddrs(addrs, volDnD, volDnA) &&
		!volSameAddrs(addrs, volDnD, volDnB) {
		t.Errorf("the legs are on %v, want dn-d and one of dn-a and dn-b",
			addrs)
	}
	dnC := env.dn(volDnC)
	if dnC.GetFreeExtCnt() != volDnFree/2 || len(dnC.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: got %v, want the other writer's %d free extents "+
			"and no side", dnC, volDnFree/2)
	}
}

// TestRepairCallsOnACloneGroup pins a repair's whole course on a clone meta
// group (architecture.md, Repairs, Retired legs): the group is found among
// the clones once no slice holds its id; the create charges the new leg's DN
// and bumps it, and the repair records the clone as its parent; the finish
// puts the new leg in the bad leg's place in the clone's group and keeps the
// bad leg as a retired leg of the clone; and the delete of that retired leg
// gives its DN back its extent and its pointer. Each call bumps the SpRev
// once, and the slice is never touched.
func TestRepairCallsOnACloneGroup(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.setDnBatchSize(1)
	env.setDnFree(volDnC, volDnCFree)
	env.setDnFree(volDnD, volDnCFree-1)
	volCreateClone(env, "clone-a", "dst")
	clone := env.clone("clone-a")
	grpId := clone.GetMetaGrp().GetGrpId()
	bad := clone.GetMetaGrp().GetLegList()[0]
	bad.ErrEpoch = repEpoch
	mustPut(t, env.cli, model.CloneKey(env.cid, volSpId, "clone-a"), clone)
	badAddr := bad.GetSideList()[0].GetAddrPort()
	slice := env.slice()
	nextId := env.spConf().GetNextId()

	// CreateRepair with the new leg on dn-a, which holds the slice's sides
	// and no leg of the clone's group: the rep_id, the new leg and its side
	// take the next three ids.
	dnABefore := env.dn(volDnA)
	revBefore := env.spRev()
	created, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		RepName:      "rep-a",
		GrpId:        grpId,
		BadLegIdList: []uint64{bad.GetLegId()},
		DnSelector:   volDnSelector(volDnA),
	})
	if err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}
	newId := nextId + 1
	if created.GetRepId() != nextId || created.GetNewLegId() != newId {
		t.Errorf("reply: rep_id %d, new_leg_id %d, want %d and %d",
			created.GetRepId(), created.GetNewLegId(), nextId, newId)
	}
	wantNew := &pb.Leg{
		LegId:  newId,
		LegIdx: uint32(common.MaxAllocLegPerGrp),
		SideList: []*pb.Side{{
			SideId:      newId + 1,
			AddrPort:    volDnA,
			CntlidSlot:  volSlot,
			NvmeTrConf:  volTrConf(volDnA),
			Provisioned: false,
		}},
	}
	rep := env.repair("rep-a")
	if len(rep.GetNewLegList()) != 1 ||
		!proto.Equal(rep.GetNewLegList()[0], wantNew) {
		t.Errorf("new_leg_list: got %v, want [%v]", rep.GetNewLegList(),
			wantNew)
	}
	if rep.GetCloneName() != "clone-a" || rep.GetSliceId() != 0 {
		t.Errorf("parent: clone %q, slice %d, want clone %q, slice 0",
			rep.GetCloneName(), rep.GetSliceId(), "clone-a")
	}
	wantDnA := proto.Clone(dnABefore).(*pb.DnConf)
	wantDnA.FreeExtCnt -= common.CloneMetaGrpExtCnt
	wantDnA.SidePtrList = append(wantDnA.SidePtrList, &pb.SidePointer{
		SpId: volSpId, LegId: newId, SideId: newId + 1,
	})
	if got := env.dn(volDnA); !proto.Equal(got, wantDnA) {
		t.Errorf("dn-a: got %v, want %v", got, wantDnA)
	}
	if got := env.dnRev(volDnA); got != 2 {
		t.Errorf("dn-a rev: got %d, want 2", got)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("sp_rev after the create: got %d, want %d", got,
			revBefore+1)
	}

	// The finish, once the sp worker has moved the repair to STARTED: the
	// new leg takes the bad leg's place and leg_idx, and the bad leg becomes
	// a retired leg of the clone, its side still on its DN.
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_STARTED)
	revBefore = env.spRev()
	finished, err := repFinish(env, "rep-a")
	if err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if finished.GetRetiredLegId() != bad.GetLegId() ||
		finished.GetGivenBackLegId() != 0 {
		t.Errorf("reply: retired_leg_id %d, given_back_leg_id %d, want %d "+
			"and 0", finished.GetRetiredLegId(),
			finished.GetGivenBackLegId(), bad.GetLegId())
	}
	grp := volCloneGrpOf(env, "clone-a")
	wantNew.LegIdx = bad.GetLegIdx()
	if len(grp.GetLegList()) != common.MaxAllocLegPerGrp ||
		!proto.Equal(grp.GetLegList()[0], wantNew) {
		t.Errorf("after the finish: legs %v, want the new leg %v first",
			grp.GetLegList(), wantNew)
	}
	if grp.GetRepName() != "" || len(grp.GetRetiredLegIdList()) != 1 ||
		grp.GetRetiredLegIdList()[0] != bad.GetLegId() {
		t.Errorf("after the finish: rep_name %q, retired legs %v, want "+
			"none and [%d]", grp.GetRepName(), grp.GetRetiredLegIdList(),
			bad.GetLegId())
	}
	retired := env.retiredLeg(bad.GetLegId())
	if retired.GetCloneName() != "clone-a" || retired.GetSliceId() != 0 ||
		!proto.Equal(retired.GetSide(), bad.GetSideList()[0]) {
		t.Errorf("retired leg: got %v, want clone %q and the bad leg's side",
			retired, "clone-a")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("sp_rev after the finish: got %d, want %d", got,
			revBefore+1)
	}

	// DeleteRetiredLeg of the retired leg: its DN gets its extent and its
	// pointer back.
	badDnBefore := env.dn(badAddr)
	badRevBefore := env.dnRev(badAddr)
	revBefore = env.spRev()
	if _, err := repDeleteRetired(env, bad.GetLegId()); err != nil {
		t.Fatalf("DeleteRetiredLeg: %v", err)
	}
	grp = volCloneGrpOf(env, "clone-a")
	if len(grp.GetRetiredLegIdList()) != 0 {
		t.Errorf("after the delete: retired legs %v, want none",
			grp.GetRetiredLegIdList())
	}
	badDn := env.dn(badAddr)
	if badDn.GetFreeExtCnt() !=
		badDnBefore.GetFreeExtCnt()+common.CloneMetaGrpExtCnt ||
		len(badDn.GetSidePtrList()) != 0 {
		t.Errorf("%s after the delete: got %v, want one extent more and "+
			"no side", badAddr, badDn)
	}
	if got := env.dnRev(badAddr); got != badRevBefore+1 {
		t.Errorf("%s rev: got %d, want %d", badAddr, got, badRevBefore+1)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("sp_rev after the delete: got %d, want %d", got,
			revBefore+1)
	}
	if got := env.slice(); !proto.Equal(got, slice) {
		t.Errorf("the slice moved: got %v, want %v", got, slice)
	}
}

// TestUpdateCntlidSlotListRefusesASlotACloneSideUses pins the slot list
// update's walk of the clone meta groups (architecture.md, Storage pools): a
// side of a clone's group uses its cntlid slot as a slice's side does, so a
// list without that slot is INVALID_ARGUMENT naming the side and its clone.
// The clone is created while slot 6 heads the list, so its sides are the
// only users of 6: the cntlrs hold 4 and 5, the slice's sides 3.
func TestUpdateCntlidSlotListRefusesASlotACloneSideUses(t *testing.T) {
	env := newVolEnv(t)
	env.patchCntlr(volCntlrA, func(cntlr *pb.Cntlr) { cntlr.CntlidSlot = 4 })
	env.patchCntlr(volCntlrB, func(cntlr *pb.Cntlr) { cntlr.CntlidSlot = 5 })
	update := func(slots ...uint32) error {
		_, err := env.srv.UpdateStoragePoolCntlidSlotList(env.ctx,
			&pb.UpdateStoragePoolCntlidSlotListRequest{
				ClusterName:    env.cluster,
				SpName:         volSpName,
				CntlidSlotList: slots,
			})
		return err
	}
	if err := update(6, volSlot, 4, 5); err != nil {
		t.Fatalf("UpdateStoragePoolCntlidSlotList [6 3 4 5]: %v", err)
	}
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	side := volCloneGrpOf(env, "clone-a").GetLegList()[0].GetSideList()[0]
	if side.GetCntlidSlot() != 6 {
		t.Fatalf("the clone's side holds slot %d, want 6",
			side.GetCntlidSlot())
	}
	before := env.dump()
	msg := volWantCode(t, update(volSlot, 4, 5), codes.InvalidArgument)
	want := fmt.Sprintf("cntlid_slot_list drops slot 6, which side %d of "+
		"clone %q uses", side.GetSideId(), "clone-a")
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	env.wantNoWrite("the refused update", before)
	if err := update(volSlot, 4, 5, 6); err != nil {
		t.Errorf("a list that keeps slot 6: %v", err)
	}
}

// TestCreateMigrationRefusesACloneSide pins the refusal of a clone meta
// group's side (architecture.md, Clones, Migrations): it is never a
// migration source, FAILED_PRECONDITION naming the clone, with nothing
// written; a side id in neither a slice nor a clone stays NOT_FOUND.
func TestCreateMigrationRefusesACloneSide(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	side := volCloneGrpOf(env, "clone-a").GetLegList()[0].GetSideList()[0]
	migrate := func(sideId uint64) error {
		_, err := env.srv.CreateMigration(env.ctx,
			&pb.CreateMigrationRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				MigrName:    "migr-a",
				SrcSideId:   sideId,
			})
		return err
	}
	before := env.dump()
	msg := volWantCode(t, migrate(side.GetSideId()),
		codes.FailedPrecondition)
	want := fmt.Sprintf("side %d belongs to the clone meta group of clone "+
		"%q, which no migration moves", side.GetSideId(), "clone-a")
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	volWantCode(t, migrate(side.GetSideId()+1000), codes.NotFound)
	env.wantNoWrite("the refused CreateMigration", before)
}

// TestGetLegBitmapRefusesACloneLeg pins the bitmap read's refusal of a leg
// of a clone meta group (architecture.md, Bitmap reads): no migration moves
// one, so its bitmap is never read, and the refusal is FAILED_PRECONDITION
// naming the clone, judged before the primary is asked anything — the
// fixture's cntlrs are not dialable. A leg id in neither a slice nor a clone
// stays NOT_FOUND.
func TestGetLegBitmapRefusesACloneLeg(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	legId := volCloneGrpOf(env, "clone-a").GetLegList()[1].GetLegId()
	read := func(legId uint64) error {
		_, err := env.srv.GetLegBitmap(env.ctx, &pb.GetLegBitmapRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			LegId:       legId,
		})
		return err
	}
	msg := volWantCode(t, read(legId), codes.FailedPrecondition)
	want := fmt.Sprintf("leg %d belongs to the clone meta group of clone "+
		"%q, which no migration moves, so its bitmap is never read", legId,
		"clone-a")
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	volWantCode(t, read(legId+1000), codes.NotFound)
}
