package worker

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The etcd-backed half of the unit tests (EU7, MD9): everything here goes
// through the real etcdutil client and the real model ops.

// TestHealthWritesThroughModel checks HL1/HL3 against real etcd: the monitor
// writes only on a transition, model.SetDnErrEpoch never restarts the
// threshold clock, and two owners observing the same transition write once.
func TestHealthWritesThroughModel(t *testing.T) {
	cli := newTestClient(t)
	captureLogs(t)
	ctx := context.Background()
	cid := testClusterId()
	const addr = "dn0:9520"

	err := cli.Put(ctx, model.DnConfKey(cid, addr), &pb.DnConf{
		DnId:        11,
		TotalExtCnt: 100,
		FreeExtCnt:  100,
	})
	if err != nil {
		t.Fatalf("seed DnConf: %v", err)
	}

	clk := newFakeClock()
	d := newTestDeps(testConfig(common.WorkerRoleDn), newFakeStore(), clk)
	d.cli = cli
	d.health = &modelHealthWriter{cli: cli}
	d.conf.entries[cid] = testClusterConf()

	owner := newDnMonitor(d, cid, 11, func() string { return addr }, nil)
	// A second owner of the same DN, the accepted overlap of VW7.
	peer := newDnMonitor(d, cid, 11, func() string { return addr }, nil)

	owner.observe(ctx, healthUnreachable, "")
	stored := &pb.DnConf{}
	if _, err := cli.Get(ctx, model.DnConfKey(cid, addr), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	first := stored.GetErrEpoch()
	if first == 0 {
		t.Fatalf("err_epoch not set")
	}

	// The peer observes the same transition later; the STM re-read makes its
	// write a no-op, so the threshold clock does not restart (HL3, MD6).
	clk.advance(30 * time.Second)
	peer.observe(ctx, healthErrorRow, "disk")
	if _, err := cli.Get(ctx, model.DnConfKey(cid, addr), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.GetErrEpoch() != first {
		t.Fatalf("err_epoch moved from %d to %d", first, stored.GetErrEpoch())
	}

	// Recovery clears it.
	owner.observe(ctx, healthClean, "")
	if _, err := cli.Get(ctx, model.DnConfKey(cid, addr), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.GetErrEpoch() != 0 {
		t.Fatalf("err_epoch = %d after recovery", stored.GetErrEpoch())
	}
}

// TestCntlrSettleWritesThroughModel checks HL2's settle against real etcd:
// the cntlr monitor's write reaches model.SetCntlrErrEpoch with both its epoch
// and its settle, so an unhealthy observation sets err_epoch and keeps the
// record settling, the recovery clears err_epoch alone when the driver does
// not judge the reply to prove the primary role, and the next observation
// that does clears settling in a write of its own (HL3). The fake health
// writer of the other settle tests only records the bool.
func TestCntlrSettleWritesThroughModel(t *testing.T) {
	cli := newTestClient(t)
	captureLogs(t)
	ctx := context.Background()
	cid := testClusterId()
	key := model.CntlrKey(cid, 3, 1)

	err := cli.Put(ctx, key, &pb.Cntlr{Primary: true, Settling: true})
	if err != nil {
		t.Fatalf("seed Cntlr: %v", err)
	}

	clk := newFakeClock()
	d := newTestDeps(testConfig(common.WorkerRoleSp), newFakeStore(), clk)
	d.cli = cli
	d.health = &modelHealthWriter{cli: cli}
	m := newCntlrMonitor(d, cid, 3, 1)
	m.settlePending = true

	stored := &pb.Cntlr{}
	load := func() {
		t.Helper()
		if _, err := cli.Get(ctx, key, stored); err != nil {
			t.Fatalf("get: %v", err)
		}
	}

	m.observeSettle(ctx, healthUnreachable, "", true)
	load()
	if got, want := stored.GetErrEpoch(), clk.nowUnix(); got != want {
		t.Fatalf("err_epoch = %d, want %d", got, want)
	}
	if !stored.GetSettling() {
		t.Fatalf("an unhealthy observation cleared settling")
	}

	m.observeSettle(ctx, healthClean, "", false)
	load()
	if stored.GetErrEpoch() != 0 || !stored.GetSettling() {
		t.Fatalf("recovery without canSettle: err_epoch %d settling %v, "+
			"want 0 true", stored.GetErrEpoch(), stored.GetSettling())
	}

	if !m.observeSettle(ctx, healthClean, "", true) {
		t.Fatalf("a clean canSettle observation did not settle")
	}
	load()
	if stored.GetSettling() {
		t.Fatalf("still settling after the settle write")
	}
	if !stored.GetPrimary() {
		t.Fatalf("the settle rewrote primary")
	}
}

// TestCloneGroupWritesThroughModel checks the sp role's writes on a clone
// meta group below the healthWriter and spOps seams, against real etcd: a leg
// monitor and a side monitor of the group set and clear their own object's
// err_epoch in the Clone record through the clone-keyed twins, and the clone
// flip sets the side's provisioned flag and returns the SpRev revision its
// STM committed (HL2, RW18, MD6). The fakes of the other tests only record
// the arguments, so an adapter that called the wrong twin would pass them.
func TestCloneGroupWritesThroughModel(t *testing.T) {
	cli := newTestClient(t)
	captureLogs(t)
	ctx := context.Background()
	cid := testClusterId()
	const (
		spId   = uint64(3)
		shard  = uint32(5)
		name   = "c0"
		id     = uint64(9)
		legA   = uint64(20)
		legB   = uint64(21)
		sideA  = uint64(30)
		sideB  = uint64(31)
		spRevN = uint64(40)
	)
	key := model.CloneKey(cid, spId, name)
	err := cli.Put(ctx, key, &pb.Clone{
		CloneId: id,
		MetaGrp: &pb.Group{
			GrpId: 10, ExtCnt: 1, MetaBlocks: 7, DataBlocks: 1017,
			LegList: []*pb.Leg{
				{LegId: legA, SideList: []*pb.Side{{SideId: sideA}}},
				{LegId: legB, SideList: []*pb.Side{{SideId: sideB}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("seed Clone: %v", err)
	}
	err = cli.Put(ctx, model.SpRevKey(shard, cid, spId), &pb.SpRev{
		Revision: spRevN, SpName: "sp0",
	})
	if err != nil {
		t.Fatalf("seed SpRev: %v", err)
	}

	clk := newFakeClock()
	d := newTestDeps(testConfig(common.WorkerRoleSp), newFakeStore(), clk)
	d.cli = cli
	d.health = &modelHealthWriter{cli: cli}
	stored := &pb.Clone{}
	load := func() {
		t.Helper()
		if _, err := cli.Get(ctx, key, stored); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	legs := func() []*pb.Leg { return stored.GetMetaGrp().GetLegList() }

	leg := newCloneLegMonitor(d, cid, spId, name, id, legA)
	side := newCloneSideMonitor(d, cid, spId, name, id, sideB)
	leg.observe(ctx, healthErrorRow, "leg")
	side.observe(ctx, healthUnreachable, "")
	load()
	now := clk.nowUnix()
	if got := legs()[0].GetErrEpoch(); got != now {
		t.Fatalf("leg A err_epoch = %d, want %d", got, now)
	}
	if got := legs()[1].GetSideList()[0].GetErrEpoch(); got != now {
		t.Fatalf("side B err_epoch = %d, want %d", got, now)
	}
	if legs()[1].GetErrEpoch() != 0 || legs()[0].GetSideList()[0].
		GetErrEpoch() != 0 {
		t.Fatalf("a write landed on another object: %v", stored)
	}
	leg.observe(ctx, healthClean, "")
	side.observe(ctx, healthClean, "")
	load()
	if legs()[0].GetErrEpoch() != 0 ||
		legs()[1].GetSideList()[0].GetErrEpoch() != 0 {
		t.Fatalf("recovery did not clear: %v", stored)
	}

	ops := &modelSpOps{cli: cli}
	ref := model.CloneSideRef{
		CloneName: name, CloneId: id, LegId: legB, SideId: sideB,
	}
	flipped, revision, err := ops.flipCloneProvisioned(
		ctx, cid, shard, spId, []model.CloneSideRef{ref},
	)
	if err != nil {
		t.Fatalf("clone flip: %v", err)
	}
	if len(flipped) != 1 || flipped[0] != ref || revision != spRevN+1 {
		t.Fatalf("clone flip = %v at %d, want %v at %d", flipped,
			revision, ref, spRevN+1)
	}
	load()
	if !legs()[1].GetSideList()[0].GetProvisioned() ||
		legs()[0].GetSideList()[0].GetProvisioned() {
		t.Fatalf("flip landed wrong: %v", stored)
	}
}

// TestReactionCnScanThroughModel checks AR7's scan below the reactionOps seam,
// against real etcd: the production adapter hands the exclusion to model's
// two-tier FindCnCandidatesAntiAffine, not to the one-tier FindCnCandidates.
// The fake of the reaction tests only records the argument, so an adapter that
// dropped excludeLocs, or that called the one-tier scan, would pass every one
// of them — and in a two-rack cluster where only the survivor's rack has room,
// the one-tier scan would leave AR7 skipping with "no candidate" on every pass
// instead of replacing.
func TestReactionCnScanThroughModel(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testClusterId()
	const (
		cnSurvivor = "cn0:9620" // hosts the SP's surviving cntlr
		cnOld      = "cn1:9620" // hosts the cntlr being replaced
		cnRackMate = "cn2:9620" // shares the survivor's rack
		cnOther    = "cn3:9620" // shares the old cntlr's rack
	)
	for _, cn := range []struct {
		addr     string
		free     uint64
		location string
	}{
		{cnSurvivor, 50, "rack0"},
		{cnOld, 45, "rack1"},
		{cnRackMate, 40, "rack0"},
		{cnOther, 30, "rack1"},
	} {
		err := cli.Put(ctx, model.CnCapacityKey(cid, cn.free, cn.addr),
			&pb.CnCapacity{Location: cn.location})
		if err != nil {
			t.Fatalf("seed %s: %v", cn.addr, err)
		}
	}
	ops := &modelReactionOps{cli: cli}
	scan := func(spCnAddrs []string, excludeLocs []string) []string {
		t.Helper()
		cands, err := ops.findCnCandidates(
			ctx, cid, 20, 10, []string{cnOld}, spCnAddrs, excludeLocs,
		)
		if err != nil {
			t.Fatalf("findCnCandidates: %v", err)
		}
		addrs := make([]string, 0, len(cands))
		for _, cand := range cands {
			addrs = append(addrs, cand.AddrPort)
		}
		return addrs
	}

	// Tier 1: the survivor's rack is excluded, so its rack-mate is skipped
	// although it has more room, and only the old cntlr's rack is left.
	if got := scan([]string{cnSurvivor}, []string{"rack0"}); len(got) != 1 ||
		got[0] != cnOther {
		t.Errorf("tier 1 = %v, want [%s] alone", got, cnOther)
	}
	// Tier 2: both racks hold a survivor, so tier 1 finds nothing and the
	// rescan without the location exclusion still places — on the one CN
	// that is neither black-listed nor one of the SP's.
	got := scan([]string{cnSurvivor, cnOther}, []string{"rack0", "rack1"})
	if len(got) != 1 || got[0] != cnRackMate {
		t.Errorf("tier 2 = %v, want [%s]", got, cnRackMate)
	}
}

// TestReactionDnScanThroughModel checks that the DN scan of AR6's grow and
// AR8's create asks for no metadata units below the reactionOps seam: a grown
// group's sides and a repair's new leg's side hold none of a DN's metadata
// area, which
// only a migration's destination uses (architecture.md, Metadata units), so a
// DN whose area is full is still a candidate. The same index scanned for one
// unit offers only the DN with room, so it is the adapter's zero that keeps
// the full one.
func TestReactionDnScanThroughModel(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testClusterId()
	cc := testClusterConf()
	const (
		dnFull = "fdn0:9520" // its metadata area full
		dnRoom = "fdn1:9520"
	)
	for _, dn := range []struct {
		addr string
		free uint64
		used uint64
	}{
		{dnFull, 100, common.DnCloneMetaUnitCnt},
		{dnRoom, 90, 0},
	} {
		binIdx, ok := model.DnBinIdx(dn.free, cc.GetDnBinConf())
		if !ok {
			t.Fatalf("no bin for %d free extents", dn.free)
		}
		err := cli.Put(ctx, model.DnCapacityKey(cid, binIdx, dn.free, dn.addr),
			&pb.DnCapacity{Location: dn.addr, UsedMetaUnits: dn.used})
		if err != nil {
			t.Fatalf("seed %s: %v", dn.addr, err)
		}
	}
	ops := &modelReactionOps{cli: cli}
	cands, err := ops.findDnCandidates(ctx, cid, cc, 10, 10, 1, nil, nil)
	if err != nil {
		t.Fatalf("findDnCandidates: %v", err)
	}
	offered := map[string]bool{}
	for _, cand := range cands {
		offered[cand.AddrPort] = true
	}
	if len(cands) != 2 || !offered[dnFull] || !offered[dnRoom] {
		t.Errorf("worker scan = %v, want both DNs, the full one included",
			cands)
	}
	// The control: asked for one unit, the same index offers the DN with
	// room alone.
	cands, _, err = model.FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc, 10, 1, 10, 1, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("FindDnCandidatesAntiAffine: %v", err)
	}
	if len(cands) != 1 || cands[0].AddrPort != dnRoom {
		t.Errorf("a scan asking for a unit = %v, want [%s] alone",
			cands, dnRoom)
	}
}

// TestReactionRepairOpsThroughModel checks AR8's four ops below the
// reactionOps seam, against real etcd. The create: the production adapter
// hands model.CreateRepair the plan createRepair gave it, the group's DNs
// its scan black-listed (MD6), under the worker's name. The fake of the
// reaction tests only records the argument, so an adapter that dropped the
// plan — every create then refused, since the group's own DNs are outside an
// empty plan — or one that read the group afresh in its place — every gain
// then missed — would pass all of them. Both directions are checked: a plan
// missing a DN the group holds is `candidate changed`, and the group's own
// DNs commit. Then the drive: the try-to-abort sets the flag, the cancel of
// the aborted repair gives the new leg's extents back, and a second repair,
// started, is finished and retires the bad leg.
func TestReactionRepairOpsThroughModel(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testClusterId()
	cc := testClusterConf()
	const (
		shard   = uint32(5)
		spId    = uint64(0x200)
		spName  = "repair-pool"
		sliceId = uint64(31)
		grpId   = uint64(41)
		badLeg  = uint64(50)
		dnA     = "sdn0:9520"
		dnB     = "sdn1:9520"
		dnC     = "sdn2:9520" // the pick
		dnFree  = uint64(100)
	)
	put := func(key string, msg proto.Message) {
		t.Helper()
		if err := cli.Put(ctx, key, msg); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	put(model.SpConfKey(cid, spName), &pb.SpConf{
		SpId:           spId,
		ShardCode:      shard,
		NextId:         1000,
		BdevConf:       testBdevConf(),
		CntlidSlotList: []uint32{0},
		SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
		SliceIdList:    []uint64{sliceId},
	})
	put(model.SpRevKey(shard, cid, spId),
		&pb.SpRev{SpName: spName, Revision: 1})
	grp := &pb.Group{GrpId: grpId, ExtCnt: 1, MetaBlocks: 3, DataBlocks: 253}
	for idx, addrPort := range []string{dnA, dnB} {
		grp.LegList = append(grp.LegList, &pb.Leg{
			LegId:  badLeg + uint64(idx),
			LegIdx: uint32(idx),
			SideList: []*pb.Side{{
				SideId: uint64(60 + idx), AddrPort: addrPort, Provisioned: true,
			}},
		})
	}
	grp.LegList[0].ErrEpoch = 1
	put(model.SliceKey(cid, spId, sliceId), &pb.Slice{
		DataGrpList: []*pb.Group{grp},
	})
	binIdx, ok := model.DnBinIdx(dnFree, cc.GetDnBinConf())
	if !ok {
		t.Fatalf("no bin for %d free extents", dnFree)
	}
	put(model.DnConfKey(cid, dnC), &pb.DnConf{
		DnId:        70,
		ShardCode:   shard,
		Location:    dnC,
		TotalExtCnt: 1000,
		FreeExtCnt:  dnFree,
	})
	put(model.DnRevKey(shard, cid, 70), &pb.DnRev{AddrPort: dnC, Revision: 1})
	put(model.DnCapacityKey(cid, binIdx, dnFree, dnC),
		&pb.DnCapacity{Location: dnC})
	pick := func() model.Cand {
		t.Helper()
		dn := &pb.DnConf{}
		if _, err := cli.Get(ctx, model.DnConfKey(cid, dnC), dn); err != nil {
			t.Fatalf("get dn: %v", err)
		}
		free := dn.GetFreeExtCnt()
		bin, ok := model.DnBinIdx(free, cc.GetDnBinConf())
		if !ok {
			t.Fatalf("no bin for %d free extents", free)
		}
		return model.Cand{
			AddrPort: dnC, Location: dnC, FreeExt: free, BinIdx: bin,
		}
	}
	ops := &modelReactionOps{cli: cli}
	parent := model.GrpParent{SliceId: sliceId}
	repName := common.WorkerRepName(spId, grpId, 1)
	create := func(name string, plan []string) (uint64, error) {
		t.Helper()
		repId, _, err := ops.createRepair(
			ctx, cid, shard, spId, spName, name, parent, grpId, badLeg,
			model.RepairNewLeg{Dn: pick(), GrpDnAddrs: plan}, cc,
		)
		return repId, err
	}
	loadRepair := func(name string) *pb.Repair {
		t.Helper()
		rep := &pb.Repair{}
		found, err := cli.Get(ctx, model.RepairKey(cid, spId, name), rep)
		if err != nil {
			t.Fatalf("get repair: %v", err)
		}
		if !found {
			return nil
		}
		return rep
	}

	_, err := create(repName, []string{dnA})
	var pre *model.ErrPrecondition
	if !errors.As(err, &pre) || pre.Reason != model.ReasonCandidateChanged {
		t.Fatalf("a plan missing %s: err = %v, want %q", dnB, err,
			model.ReasonCandidateChanged)
	}
	repId, err := create(repName, []string{dnA, dnB})
	if err != nil {
		t.Fatalf("createRepair with the group's own plan: %v", err)
	}
	rep := loadRepair(repName)
	if rep.GetRepId() != repId || len(rep.GetNewLegList()) != 1 ||
		rep.GetNewLegList()[0].GetSideList()[0].GetAddrPort() != dnC ||
		!slices.Equal(rep.GetBadLegIdList(), []uint64{badLeg}) {
		t.Fatalf("repair = %v, want rep_id %d, a new leg on %s, bad leg %d",
			rep, repId, dnC, badLeg)
	}

	// The try-to-abort, the primary's confirmation (the stage move), and
	// the cancel, which gives dn C its extents back.
	if _, err := ops.setRepairAbort(
		ctx, cid, shard, spId, spName, repName,
	); err != nil {
		t.Fatalf("setRepairAbort: %v", err)
	}
	if !loadRepair(repName).GetTryToAbort() {
		t.Fatalf("the try-to-abort set no flag")
	}
	if err := model.SetRepairStage(ctx, cli, cid, shard, spId, spName,
		repName, repId, pb.RepStage_REP_STAGE_ABORTED); err != nil {
		t.Fatalf("SetRepairStage ABORTED: %v", err)
	}
	if _, err := ops.cancelRepair(
		ctx, cid, shard, spId, spName, repName, cc,
	); err != nil {
		t.Fatalf("cancelRepair: %v", err)
	}
	if loadRepair(repName) != nil {
		t.Fatalf("the repair outlived its cancel")
	}
	if got := pick().FreeExt; got != dnFree {
		t.Fatalf("dn C holds %d free extents after the cancel, want %d",
			got, dnFree)
	}

	// A second repair, started by the primary, is finished: the bad leg is
	// retired.
	repName = common.WorkerRepName(spId, grpId, 2)
	repId, err = create(repName, []string{dnA, dnB})
	if err != nil {
		t.Fatalf("second createRepair: %v", err)
	}
	if err := model.SetRepairStage(ctx, cli, cid, shard, spId, spName,
		repName, repId, pb.RepStage_REP_STAGE_STARTED); err != nil {
		t.Fatalf("SetRepairStage STARTED: %v", err)
	}
	res, err := ops.finishRepair(
		ctx, cid, shard, spId, spName, repName, 1234, cc,
	)
	if err != nil {
		t.Fatalf("finishRepair: %v", err)
	}
	if res.RepId != repId || res.RetiredLegId != badLeg ||
		res.GivenBackLegId != 0 {
		t.Fatalf("finish = %+v, want rep_id %d retiring leg %d", res, repId,
			badLeg)
	}
	retired := &pb.RetiredLeg{}
	if found, err := cli.Get(ctx, model.RetiredLegKey(cid, spId, badLeg),
		retired); err != nil || !found || retired.GetRetiredEpoch() != 1234 {
		t.Fatalf("retired leg = %v (found %t, err %v), want leg %d retired "+
			"at 1234", retired, found, err, badLeg)
	}
}

// TestConfCacheAgainstEtcd runs the RW21 cache over the real client: scan,
// watch, key -> id derivation, delete, and the rule of architecture.md, Common
// validation, that the value a reader gets is the value that was written — no
// default is applied on the way out.
func TestConfCacheAgainstEtcd(t *testing.T) {
	cli := newTestClient(t)
	captureLogs(t)
	ctx := context.Background()
	name := "etcdcache"
	const epoch = uint64(1234567)
	cid := model.ClusterId(name, epoch)
	key := model.ClusterConfKey(name)
	t.Cleanup(func() { _ = cli.Delete(context.Background(), key) })

	d := newTestDeps(
		testConfig(common.WorkerRoleDn), newFakeStore(), newFakeClock(),
	)
	d.store = cli
	d.cli = cli
	cache := newConfCache(d)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Deliberately NOT a conf the gateway could have written: no
	// health_check_conf at all. It comes back exactly as it went in, over the
	// real client and the real decode path, because architecture.md, Common
	// validation, moved every default to the write side and the cache resolves
	// nothing.
	stored := &pb.ClusterConf{
		CreationEpoch: epoch,
		QosRatio:      &pb.QosRatio{BytesPerIops: 512},
	}
	if err := cli.Put(ctx, key, stored); err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := cache.get(cid)
		return ok
	})
	cc, _ := cache.get(cid)
	if !proto.Equal(cc, stored) {
		t.Fatalf("cached conf =\n%v\nwant the stored\n%v", cc, stored)
	}
	if cc.GetHealthCheckConf() != nil {
		t.Fatalf("health_check_conf = %v, want the stored absence",
			cc.GetHealthCheckConf())
	}
	if cc.GetQosRatio().GetBytesPerIops() != 512 {
		t.Fatalf("qos_ratio = %v", cc.GetQosRatio())
	}
	if err := cli.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "cluster dropped", func() bool {
		_, ok := cache.get(cid)
		return !ok
	})
}

// TestVoteWorkerAgainstEtcd is the end-to-end smoke of the vote worker
// (VW1-VW11) over real etcd: the worker registers, observes its own key in its
// own first scan, drives nothing for one grace window, then owns every shard of
// its role, and deletes its registration on a graceful stop (CM5).
func TestVoteWorkerAgainstEtcd(t *testing.T) {
	cli := newTestClient(t)
	logs := captureLogs(t)

	cfg := Config{
		Roles:        []string{common.WorkerRoleDn},
		VoteInterval: 100 * time.Millisecond,
		GraceTime:    400 * time.Millisecond,
		Endpoints:    []string{testEndpoint},
	}
	d := &deps{
		cfg:    cfg,
		store:  cli,
		cli:    cli,
		conns:  newConnCache(),
		health: &fakeHealthWriter{},
		clk:    realClock{},
		tmo:    prodTimeouts(),
		kinds:  kindFor,
	}
	d.conf = newConfCache(d)

	seed, err := newSeed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	vote := newVoteWorker(d, seed)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		vote.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = cli.Delete(
			context.Background(),
			workerRegKey(common.WorkerRoleDn, seed),
		)
	})

	key := workerRegKey(common.WorkerRoleDn, seed)
	waitFor(t, "registration in etcd", func() bool {
		reg := &pb.WorkerReg{}
		found, err := cli.Get(context.Background(), key, reg)
		return err == nil && found && reg.GetEpoch() > 0
	})
	// The etcd put and its 'worker registered' record are not atomic, so the
	// wait above can win the race against the log append. Wait for the record
	// too, then assert it appears exactly once (VW2 logs only the FIRST
	// successful put of a role).
	waitFor(t, "worker registered record", func() bool {
		return len(logs.withMsg(msgWorkerRegistered)) >= 1
	})
	if got := len(logs.withMsg(msgWorkerRegistered)); got != 1 {
		t.Fatalf("%d worker registered records, want 1", got)
	}
	waitFor(t, "own key observed live", func() bool {
		for _, rec := range logs.withMsg(msgMembershipObserved) {
			if rec["seed"] == seed && rec["state"] == stateLive &&
				rec["own"] == true {
				return true
			}
		}
		return false
	})
	waitFor(t, "membership committed", func() bool {
		for _, rec := range logs.withMsg(msgMembershipCommitted) {
			if rec["seed"] == seed && rec["state"] == stateMember {
				return true
			}
		}
		return false
	})
	waitFor(t, "every shard owned", func() bool {
		return len(logs.withMsg(msgShardOwned)) == common.ShardBucketSize
	})
	if got := len(logs.withMsg(msgWorkerFenced)); got != 0 {
		t.Fatalf("the worker fenced itself: %v", logs.withMsg(msgWorkerFenced))
	}

	cancel()
	<-done
	reg := &pb.WorkerReg{}
	found, err := cli.Get(context.Background(), key, reg)
	if err != nil {
		t.Fatalf("get after shutdown: %v", err)
	}
	if found {
		t.Fatalf("registration survived a graceful stop")
	}
	if got := len(logs.withMsg(msgShardReleased)); got != common.ShardBucketSize {
		t.Fatalf("%d shard released records, want %d",
			got, common.ShardBucketSize)
	}
}
