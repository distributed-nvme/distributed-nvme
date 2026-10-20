package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A clone's clone meta group (architecture.md, Terminology and object model)
// is built as a slice's group is: its legs connected and wrapped on every
// cntlr (CN10), its md array or, without redundancy, its CnGrpName dm-linear
// on the primary (CN12), deferred while a leg_list leg provisions (CN9),
// under the group gate of the levels (CN19), kept by the sweep while the
// clone is in the request and removed with it (CN21), and reported in the
// group and leg rows (CN28).

// The clone meta group's ids: their own, above the fixture's slice ids and
// below manySliceBase.
const (
	testCloneGrp       = uint64(0x61)
	testCloneLeg       = uint64(0x62)
	testCloneSide      = uint64(0x63)
	testCloneLeg2      = uint64(0x64)
	testCloneSide2     = uint64(0x65)
	testCloneSpareLeg  = uint64(0x66)
	testCloneSpareSide = uint64(0x67)
)

// cloneGrpBase and cloneGrpStride place the clone meta group of a clone
// other than testClone that cloneGrpOf makes: its ids start at cloneGrpBase
// plus the clone id times cloneGrpStride, above manySlices' ids.
const (
	cloneGrpBase   = uint64(0x2000)
	cloneGrpStride = uint64(0x10)
)

// cloneGrpOf is the clone meta group of a fixture clone, of the shape the
// pool's redundancy gives a group, at the geometry cntlrReq gives a slice's
// meta group: one leg on testIp without redundancy, and at raid1 a second leg
// on testIp2. testClone's group takes the testClone* ids, any other clone's
// ids of its own (cloneGrpBase).
func cloneGrpOf(cloneId uint64, raid1 bool) *pb.Group {
	grpId, legId, sideId, legId2, sideId2 := testCloneGrp, testCloneLeg,
		testCloneSide, testCloneLeg2, testCloneSide2
	if cloneId != testClone {
		base := cloneGrpBase + cloneId*cloneGrpStride
		grpId, legId, sideId, legId2, sideId2 =
			base, base+1, base+2, base+3, base+4
	}
	metaBlocks, dataBlocks := uint64(1), uint64(63)
	legs := []*pb.Leg{legOf(legId, sideOf(sideId, testIp, testSvcId))}
	if raid1 {
		metaBlocks, dataBlocks = 3, 61
		second := legOf(legId2, sideOf(sideId2, testIp2, testSvcId2))
		second.LegIdx = 1
		legs = append(legs, second)
	}
	return &pb.Group{
		GrpId:      grpId,
		ExtCnt:     1,
		MetaBlocks: metaBlocks,
		DataBlocks: dataBlocks,
		LegList:    legs,
	}
}

// withCloneGrps is a request's clone_list (reqOpts.clones): every clone that
// carries no clone meta group gets a copy with cloneGrpOf's, unless none
// is set, and a clone that carries one is kept as it is.
func withCloneGrps(clones []*pb.Clone, raid1, none bool) []*pb.Clone {
	if none || clones == nil {
		return clones
	}
	out := make([]*pb.Clone, 0, len(clones))
	for _, clone := range clones {
		if clone.GetMetaGrp() == nil {
			clone = proto.Clone(clone).(*pb.Clone)
			clone.MetaGrp = cloneGrpOf(clone.GetCloneId(), raid1)
		}
		out = append(out, clone)
	}
	return out
}

// cloneGrpNotReady leaves a clone meta group without redundancy, its
// CnGrpName dm-linear over the leg legId, with a table the group does not
// want over a leg that is not available, which neither channel reads OK and
// the converge does not repair (ensureLinearGroup).
func cloneGrpNotReady(
	srv *CnAgentServer,
	node *fakeNode,
	grpId uint64,
	legId uint64,
) {
	node.setAnaState(legNqn(srv, legId), testIp, testSvcId, "inaccessible")
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[grpName(srv, grpId)]
	dm.table = strings.Replace(dm.table, " 2048", " 4096", 1)
}

// cloneWithGrp is cloneOf with its clone meta group (cloneGrpOf).
func cloneWithGrp(raid1 bool) *pb.Clone {
	clone := cloneOf()
	clone.MetaGrp = cloneGrpOf(testClone, raid1)
	return clone
}

// cloneSpareLeg is a spare leg for the clone meta group, on testIp2.
func cloneSpareLeg() *pb.Leg {
	spare := legOf(testCloneSpareLeg,
		sideOf(testCloneSpareSide, testIp2, testSvcId2))
	spare.LegIdx = 2
	return spare
}

// cloneMdPath is the /dev/md/{name} of the fixture clone meta group's array.
func cloneMdPath(srv *CnAgentServer) string {
	return srv.nf.MdPath(srv.nf.CnCloneMdDevName(
		testCluster, testCn, testSp, testCloneGrp))
}

// cloneLegIds are the fixture clone meta group's leg_list legs.
func cloneLegIds(raid1 bool) []uint64 {
	if raid1 {
		return []uint64{testCloneLeg, testCloneLeg2}
	}
	return []uint64{testCloneLeg}
}

// assertSameGrpRows holds a read's rows of one group and its legs to the
// converge's (CN28) by name, status and epoch, the part of a row the worker
// acts on: a converge writes a group's row from its step's error, a read
// from the array it reads, so the details may differ. A row one channel
// reports and the other does not is a difference too.
func assertSameGrpRows(
	t *testing.T,
	converged *pb.CntlrInfo,
	probed *pb.CntlrInfo,
	grpId uint64,
	legIds ...uint64,
) {
	t.Helper()
	same := func(label string, want, got *pb.ResInfo) {
		t.Helper()
		if (want == nil) != (got == nil) ||
			got.GetResName() != want.GetResName() ||
			got.GetStatus() != want.GetStatus() ||
			got.GetEpoch() != want.GetEpoch() {
			t.Fatalf("%s: the read reports %v, the converge %v",
				label, got, want)
		}
	}
	same(fmt.Sprintf("group %#x", grpId),
		converged.GetGrpIdToMdRaid()[grpId], probed.GetGrpIdToMdRaid()[grpId])
	for _, legId := range legIds {
		want, got := converged.GetLegIdToLeg()[legId],
			probed.GetLegIdToLeg()[legId]
		if want == nil {
			t.Fatalf("leg %#x has no row", legId)
		}
		same(fmt.Sprintf("leg %#x", legId), want, got)
	}
}

// syncupCntlrUntilClean re-sends one request, as the worker re-sends a
// syncup whose reply is a leftover, until a reply is clean, letting the
// sweep's disconnects return between the passes (CN10, CN21). A teardown
// that leaves a disconnect in flight stops its descent there, so the layers
// below it go on later passes.
func syncupCntlrUntilClean(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	const maxPasses = 6
	var reply *pb.SyncupCntlrReply
	for pass := 0; pass < maxPasses; pass++ {
		reply = syncupCntlrOnce(t, srv, o)
		if reply.GetAgentReply().GetCode() == 0 {
			return reply
		}
		awaitDisconnects(t, srv)
	}
	t.Fatalf("no clean reply in %d passes: %v", maxPasses,
		reply.GetAgentReply())
	return nil
}

// syncupCntlrOnce sends one request and fails the test on an error or
// a rejection; a leftover reply is returned.
func syncupCntlrOnce(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if code := reply.GetAgentReply().GetCode(); code != 0 &&
		code != common.ReplyCodeLeftover {
		t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// TestCloneMetaGroupBuild: on the primary the clone meta group's legs are
// connected and wrapped and the group gets its array, under the clone form
// of the md names, or its CnGrpName linear over the leg's data region; on a
// standby only the legs. A read reports the same group and leg rows, and no
// leg of the group has a path row.
func TestCloneMetaGroupBuild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raid1   bool
		primary bool
	}{
		{"raid1 primary", true, true},
		{"raid1 standby", true, false},
		{"no redundancy primary", false, true},
		{"no redundancy standby", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			reply := syncupBoth(t, srv, reqOpts{revision: 2,
				primary: tc.primary, raid1: tc.raid1,
				clones: []*pb.Clone{cloneWithGrp(tc.raid1)}})
			info := reply.GetCntlrInfo()
			legIds := cloneLegIds(tc.raid1)

			for _, legId := range legIds {
				if !node.hasCall("--nqn " + legNqn(srv, legId) +
					" --hostnqn") {
					t.Fatalf("leg %#x of the clone meta group was not "+
						"connected:\n%s", legId,
						strings.Join(node.Calls(), "\n"))
				}
				if _, ok := node.dms[legName(srv, legId)]; !ok {
					t.Fatalf("leg %#x of the clone meta group has no "+
						"wrapper", legId)
				}
				row := info.GetLegIdToLeg()[legId]
				if tc.primary {
					// The prober has not run a round yet (CN11).
					assertPending(t, row, "clone leg")
				} else {
					assertOk(t, row, "clone leg")
				}
			}
			for _, side := range []uint64{testCloneSide, testCloneSide2} {
				if row, ok := info.GetSideIdToPath()[side]; ok {
					t.Fatalf("side %#x of a one-sided clone meta leg has "+
						"a path row: %v", side, row)
				}
			}

			grpRow, hasGrp := info.GetGrpIdToMdRaid()[testCloneGrp]
			switch {
			case !tc.primary:
				// A standby has no group (CN12).
				if hasGrp {
					t.Fatalf("a standby reported the clone meta group: %v",
						grpRow)
				}
				assertNoCall(t, node, "cmd mdadm")
				assertNoCall(t, node,
					"cmd dmsetup create "+grpName(srv, testCloneGrp))
			case tc.raid1:
				mdPath := cloneMdPath(srv)
				assertOk(t, grpRow, "clone meta group")
				if grpRow.GetResName() != mdPath {
					t.Fatalf("the group row is named %q, want %q",
						grpRow.GetResName(), mdPath)
				}
				creates := node.callsMatching("cmd mdadm --create " + mdPath)
				if len(creates) != 1 {
					t.Fatalf("want one create of %s, got %v", mdPath, creates)
				}
				for _, want := range []string{
					"--name " + srv.nf.CnCloneMdArrayName(testSp),
					"--assume-clean", "--failfast",
					srv.nf.DmPath(legName(srv, testCloneLeg)),
					srv.nf.DmPath(legName(srv, testCloneLeg2)),
				} {
					if !strings.Contains(creates[0], want) {
						t.Fatalf("the create lacks %q: %s", want, creates[0])
					}
				}
				assertOrder(t, node,
					"cmd dmsetup create "+legName(srv, testCloneLeg2),
					"cmd mdadm --create "+mdPath)
			default:
				assertOk(t, grpRow, "clone meta group")
				dm := node.dms[grpName(srv, testCloneGrp)]
				if dm == nil {
					t.Fatalf("the clone meta group has no %s",
						grpName(srv, testCloneGrp))
				}
				// The data region of the leg: 63 blocks of 1 MiB after its
				// one meta block, in sectors.
				legNo := node.devNo[srv.nf.DmPath(
					legName(srv, testCloneLeg))]
				if want := agent.LinearTable(63*2048, legNo, 2048); dm.table !=
					want {
					t.Fatalf("the group device's table is %q, want %q",
						dm.table, want)
				}
				assertNoCall(t, node, "cmd mdadm")
			}

			node.Reset()
			probed := checkRound(t, srv, 2)
			for _, call := range node.Mutations() {
				t.Fatalf("the read mutated: %q", call)
			}
			assertSameGrpRows(t, info, probed, testCloneGrp, legIds...)
		})
	}
}

// TestACloneWithNoGroupBuildsNone: a clone record without a clone meta
// group gets no group and no leg on the cntlr, and nothing of the clone is
// read or built: no source connect and no dm-clone, its two rows ERROR "no
// metadata group" on both channels (CN18).
func TestACloneWithNoGroupBuildsNone(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		raid1: true, clones: []*pb.Clone{cloneOf()}, noCloneGrp: true})
	info := reply.GetCntlrInfo()
	if got := len(info.GetGrpIdToMdRaid()); got != 2 {
		t.Fatalf("%d group rows, want the fixture's 2: %v",
			got, info.GetGrpIdToMdRaid())
	}
	if got := len(info.GetLegIdToLeg()); got != 2 {
		t.Fatalf("%d leg rows, want the fixture's 2: %v",
			got, info.GetLegIdToLeg())
	}
	if got := len(node.callsMatching("cmd mdadm --create")); got != 2 {
		t.Fatalf("%d arrays created, want the fixture's 2", got)
	}
	assertNoCall(t, node, "--nqn "+testSrcNqn)
	if _, ok := node.dms[cloneName(srv, testClone)]; ok {
		t.Fatalf("a clone with no metadata group got a dm-clone")
	}
	probed := checkRound(t, srv, 2)
	for label, rows := range map[string]*pb.CntlrInfo{
		"converge": info, "check round": probed} {
		assertErrorDetails(t, rows.GetCloneIdToTarget()[testClone],
			"no metadata group", label+" target row")
		assertErrorDetails(t, rows.GetCloneIdToDmClone()[testClone],
			"no metadata group", label+" dm row")
	}
}

// TestCloneMetaGroupWaitsForItsSides: a clone meta group one of whose
// leg_list legs still provisions is deferred as a slice's group is (CN9,
// CN12): no superblock probe and no array, the group and that leg reading
// PROVISIONING, its provisioned leg connected and wrapped. Once the side is
// provisioned the next converge creates the array. A spare that provisions
// defers only itself.
func TestCloneMetaGroupWaitsForItsSides(t *testing.T) {
	srv, node := newTestServer(t)
	clone := cloneWithGrp(true)
	clone.MetaGrp.LegList[1].SideList[0].Provisioned = false
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		raid1: true, clones: []*pb.Clone{clone}})
	info := reply.GetCntlrInfo()

	mdPath := cloneMdPath(srv)
	assertNoCall(t, node, "cmd mdadm --create "+mdPath)
	for _, legId := range []uint64{testCloneLeg, testCloneLeg2} {
		assertNoCall(t, node, "cmd mdadm --examine --export "+
			srv.nf.DmPath(legName(srv, legId)))
	}
	assertNoCall(t, node, "--nqn "+legNqn(srv, testCloneLeg2)+" --hostnqn")
	if _, ok := node.dms[legName(srv, testCloneLeg2)]; ok {
		t.Fatalf("the provisioning leg was wrapped")
	}
	if _, ok := node.dms[legName(srv, testCloneLeg)]; !ok {
		t.Fatalf("the provisioned leg of a deferred group was not wrapped")
	}
	assertProvisioning(t, info.GetGrpIdToMdRaid()[testCloneGrp],
		"deferred clone meta group")
	assertProvisioning(t, info.GetLegIdToLeg()[testCloneLeg2],
		"provisioning clone leg")
	assertPending(t, info.GetLegIdToLeg()[testCloneLeg], "provisioned leg")
	probed := checkRound(t, srv, 2)
	assertSameGrpRows(t, info, probed, testCloneGrp,
		testCloneLeg, testCloneLeg2)

	// The side's flip arrives as a new revision: the group is built.
	node.Reset()
	clone = cloneWithGrp(true)
	clone.MetaGrp.SpareLegList = []*pb.Leg{cloneSpareLeg()}
	clone.MetaGrp.SpareLegList[0].SideList[0].Provisioned = false
	reply = syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		raid1: true, clones: []*pb.Clone{clone}})
	info = reply.GetCntlrInfo()
	if !node.hasCall("cmd mdadm --create " + mdPath) {
		t.Fatalf("the provisioned group was not created:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testCloneGrp], "clone meta group")
	assertProvisioning(t, info.GetLegIdToLeg()[testCloneSpareLeg],
		"provisioning spare")
}

// TestCloneMetaGroupFollowsTheGroupGate: the clone meta group's array follows
// the group gate of CN19 and its legs the leg gate, as a slice's do. At
// SP_LEVEL_NO_CLONE the array stays while the dm-clone goes; at
// SP_LEVEL_NO_REDUND the array is stopped, its row MISSING "sp_level", and
// the legs stay connected and wrapped; lowering the level again assembles it
// from its legs.
func TestCloneMetaGroupFollowsTheGroupGate(t *testing.T) {
	srv, node := newTestServer(t)
	clones := []*pb.Clone{cloneWithGrp(true)}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true,
		clones: clones})
	mdPath := cloneMdPath(srv)
	mdNode := node.mdNode(mdPath)
	if mdNode == "" {
		t.Fatalf("the clone meta group has no array")
	}
	clone := cloneName(srv, testClone)
	if _, ok := node.dms[clone]; !ok {
		t.Fatalf("the case is vacuous: no dm-clone was built")
	}

	node.Reset()
	reply := syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		raid1: true, clones: clones, level: pb.SpLevel_SP_LEVEL_NO_CLONE})
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetGrpIdToMdRaid()[testCloneGrp],
		"clone meta group at NO_CLONE")
	assertMissingSpLevel(t, info.GetCloneIdToDmClone()[testClone],
		"dm-clone at NO_CLONE")
	if _, ok := node.dms[clone]; ok {
		t.Fatalf("SP_LEVEL_NO_CLONE kept the dm-clone")
	}
	if node.arrayGone(mdNode) {
		t.Fatalf("SP_LEVEL_NO_CLONE stopped the clone meta group's array")
	}

	node.Reset()
	reply = syncupCntlrAt(t, srv, reqOpts{revision: 4, primary: true,
		raid1: true, clones: clones, level: pb.SpLevel_SP_LEVEL_NO_REDUND})
	info = reply.GetCntlrInfo()
	if !node.arrayGone(mdNode) {
		t.Fatalf("SP_LEVEL_NO_REDUND kept the clone meta group's array")
	}
	assertMissingSpLevel(t, info.GetGrpIdToMdRaid()[testCloneGrp],
		"clone meta group at NO_REDUND")
	for _, legId := range cloneLegIds(true) {
		assertNoCall(t, node, "cmd dmsetup remove "+legName(srv, legId))
		assertPending(t, info.GetLegIdToLeg()[legId], "clone leg at NO_REDUND")
	}
	assertNoCall(t, node, "cmd nvme disconnect")

	node.Reset()
	reply = syncupCntlrAt(t, srv, reqOpts{revision: 5, primary: true,
		raid1: true, clones: clones})
	if !node.hasCall("cmd mdadm --assemble " + mdPath) {
		t.Fatalf("lowering the level did not assemble the clone meta "+
			"group:\n%s", strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, "cmd mdadm --create "+mdPath)
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testCloneGrp],
		"clone meta group back at READWRITE")
}

// TestCloneMetaGroupSweptWithItsClone: the sweep's wanted set holds the
// clone meta group for as long as its clone is in the request, so a re-sent
// request removes nothing of it; once the clone leaves the request the sweep
// stops its array, disconnects its legs and removes their wrappers, and the
// slice's groups stay.
func TestCloneMetaGroupSweptWithItsClone(t *testing.T) {
	for _, raid1 := range []bool{true, false} {
		t.Run(fmt.Sprintf("raid1=%v", raid1), func(t *testing.T) {
			srv, node := newTestServer(t)
			clones := []*pb.Clone{cloneWithGrp(raid1)}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: raid1, clones: clones})
			mdPath := cloneMdPath(srv)
			mdNode := ""
			if raid1 {
				mdNode = node.mdNode(mdPath)
				if mdNode == "" {
					t.Fatalf("the clone meta group has no array")
				}
			}
			legIds := cloneLegIds(raid1)

			node.Reset()
			syncupCntlrAt(t, srv, reqOpts{revision: 2, primary: true,
				raid1: raid1, clones: clones})
			assertNoCall(t, node, "cmd mdadm --stop")
			assertNoCall(t, node, "cmd nvme disconnect")
			assertNoCall(t, node,
				"cmd dmsetup remove "+grpName(srv, testCloneGrp))
			for _, legId := range legIds {
				assertNoCall(t, node, "cmd dmsetup remove "+
					legName(srv, legId))
			}

			node.Reset()
			reply := syncupCntlrUntilClean(t, srv, reqOpts{revision: 3,
				primary: true, raid1: raid1})
			if raid1 && !node.arrayGone(mdNode) {
				t.Fatalf("the clone meta group's array survived its clone")
			}
			if _, ok := node.dms[grpName(srv, testCloneGrp)]; ok {
				t.Fatalf("the clone meta group's device survived its clone")
			}
			for _, legId := range legIds {
				if _, ok := node.dms[legName(srv, legId)]; ok {
					t.Fatalf("leg %#x's wrapper survived its clone", legId)
				}
				if _, ok := node.subsystems[legNqn(srv, legId)]; ok {
					t.Fatalf("leg %#x is still connected", legId)
				}
			}
			info := reply.GetCntlrInfo()
			if _, ok := info.GetGrpIdToMdRaid()[testCloneGrp]; ok {
				t.Fatalf("the departed clone's group still has a row")
			}
			for _, grpId := range []uint64{testMetaGrp, testDataGrp} {
				assertOk(t, info.GetGrpIdToMdRaid()[grpId], "slice group")
			}
		})
	}
}

// TestCloneMetaGroupSpareSwitch: the member reconciliation of CN12 carries a
// switch on a clone meta group as on a slice's: the spare is connected and
// wrapped but no member, and once it is switched in the leg it replaces is
// failed and removed and the spare added. The dm-clone over the array stays
// and reads the hydrated count its metadata held before the switch.
func TestCloneMetaGroupSpareSwitch(t *testing.T) {
	srv, node := newTestServer(t)
	clone := cloneWithGrp(true)
	clone.MetaGrp.SpareLegList = []*pb.Leg{cloneSpareLeg()}
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		raid1: true, clones: []*pb.Clone{clone}})
	mdPath := cloneMdPath(srv)
	spareWrapper := srv.nf.DmPath(legName(srv, testCloneSpareLeg))
	if _, ok := node.dms[legName(srv, testCloneSpareLeg)]; !ok {
		t.Fatalf("the clone meta group's spare was not wrapped")
	}
	for _, call := range node.callsMatching("cmd mdadm --create " + mdPath) {
		if strings.Contains(call, spareWrapper) {
			t.Fatalf("a spare leg became an md member: %s", call)
		}
	}
	assertPending(t, reply.GetCntlrInfo().GetLegIdToLeg()[testCloneSpareLeg],
		"the spare on the primary")
	dmClone := cloneName(srv, testClone)
	node.setCloneHydrated(t, dmClone, 37)

	node.Reset()
	switched := cloneWithGrp(true)
	member := switched.MetaGrp.LegList[0]
	spare := cloneSpareLeg()
	spare.LegIdx = member.GetLegIdx()
	switched.MetaGrp.LegList[0] = spare
	switched.MetaGrp.SpareLegList = []*pb.Leg{member}
	reply = syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
		raid1: true, clones: []*pb.Clone{switched}})
	assertOrder(t, node,
		"cmd mdadm "+mdPath+" --fail "+
			srv.nf.DmPath(legName(srv, testCloneLeg)),
		"cmd mdadm "+mdPath+" --remove "+
			srv.nf.DmPath(legName(srv, testCloneLeg)),
		"cmd mdadm "+mdPath+" --add --failfast "+spareWrapper,
	)
	assertNoCall(t, node, "--remove "+
		srv.nf.DmPath(legName(srv, testCloneLeg2)))
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testCloneGrp],
		"the clone meta group after the switch")
	assertNoCall(t, node, "cmd dmsetup create "+dmClone)
	assertNoCall(t, node, "cmd dmsetup reload "+dmClone)
	row := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	assertOk(t, row, "the dm-clone after the switch")
	assertHydrated(t, row, 37, "the dm-clone after the switch")
}

// TestAFailedReconciliationUnderACloneMetaGroup: a converge whose member
// reconciliation of a clone meta group fails reads the group ERROR, so the
// group is not ready for that pass (CN18 step 2). A dm-clone an earlier pass
// built over the running array is left as it is and serves on: its row OK,
// the namespace optimized over it, and, as for a slice's group, the failure
// registers no CN10 retry (CN12). With no dm-clone there, the same failure
// builds none: the clone's row reads MISSING naming the group, the namespace
// waits parked and inaccessible, and the retry is registered. Step 2
// registers it itself: with the namespace effectively suspended — its
// stored flag set, the clone's auto_resume off — CN16 parks it whatever the
// clone does and its desired group is inaccessible, so no namespace waits on
// the dm-clone, and the retry is still registered, since the copy itself
// waits for the dm-clone.
func TestAFailedReconciliationUnderACloneMetaGroup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serving   bool
		suspended bool
	}{
		{"serving", true, false},
		{"no dm-clone", false, false},
		{"no dm-clone, the namespace suspended", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clone := cloneWithGrp(true)
			clone.AutoResume = !tc.suspended
			clone.MetaGrp.SpareLegList = []*pb.Leg{cloneSpareLeg()}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: true, suspended: tc.suspended,
				clones: []*pb.Clone{clone}})
			dmClone := cloneName(srv, testClone)
			if !tc.serving {
				removeCloneDevice(node, srv)
			}
			spareWrapper := srv.nf.DmPath(legName(srv, testCloneSpareLeg))
			node.failCmdAlways["mdadm "+cloneMdPath(srv)+
				" --add --failfast "+spareWrapper] = "mdadm: add refused"

			node.Reset()
			switched := cloneWithGrp(true)
			switched.AutoResume = !tc.suspended
			member := switched.MetaGrp.LegList[0]
			spare := cloneSpareLeg()
			spare.LegIdx = member.GetLegIdx()
			switched.MetaGrp.LegList[0] = spare
			switched.MetaGrp.SpareLegList = []*pb.Leg{member}
			reply := syncupCntlrAt(t, srv, reqOpts{revision: 3,
				primary: true, raid1: true, suspended: tc.suspended,
				clones: []*pb.Clone{switched}})
			info := reply.GetCntlrInfo()
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testCloneGrp],
				"add refused", "the clone meta group")
			assertNoCall(t, node, "cmd dmsetup create "+dmClone)
			assertNoCall(t, node, "cmd dmsetup reload "+dmClone)
			row := info.GetCloneIdToDmClone()[testClone]
			if tc.serving {
				assertOk(t, row, "the serving dm-clone")
				want := agent.LinearTable(testTdSize/512,
					node.devNo["/dev/mapper/"+dmClone], 0)
				if got := node.dms[nsDevName(srv, testNs)].table; got !=
					want {
					t.Fatalf("the ns-dev is %q, want it serving through "+
						"the dm-clone %q", got, want)
				}
				assertAna(t, node, testNqn, "1",
					"the namespace over the serving dm-clone")
				if retrying(t, srv) {
					t.Fatalf("a failed reconciliation under a serving " +
						"dm-clone registered the retry")
				}
				return
			}
			assertMissing(t, row, "the dm row with no dm-clone")
			if !strings.Contains(row.GetDetails(),
				detailsCloneMetaGrpNotReady) {
				t.Fatalf("the dm row's details are %q, want %q",
					row.GetDetails(), detailsCloneMetaGrpNotReady)
			}
			assertParked(t, srv, node, testNs, testTd,
				"the namespace with no dm-clone")
			assertAna(t, node, testNqn, "3",
				"the namespace with no dm-clone")
			if !retrying(t, srv) {
				t.Fatalf("a clone meta group not ready under no dm-clone " +
					"registered no retry")
			}
		})
	}
}

// TestGetLegBmRefusesACloneMetaLeg: a leg of a clone meta group backs no
// pool, so GetLegBm refuses it, naming the clone, and takes no metadata
// snapshot (CN27).
func TestGetLegBmRefusesACloneMetaLeg(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true,
		clones: []*pb.Clone{cloneWithGrp(true)}})
	scriptDump(srv, node, caseBDump)
	node.Reset()
	_, err := srv.GetLegBm(context.Background(), &pb.GetLegBmRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, LegId: testCloneLeg,
	})
	if status.Code(err) != codes.Internal ||
		!strings.Contains(err.Error(), fmt.Sprintf(
			"leg %d is a leg of clone %d's clone meta group",
			testCloneLeg, testClone)) {
		t.Fatalf("GetLegBm of a clone meta leg answered %v", err)
	}
	assertNoCall(t, node, "reserve_metadata_snap")
}

// TestCloneMetaLegsAreProbedOnThePrimary: the primary runs the CN11 health
// probe on every leg of a clone meta group, its spare included, at the tail
// of the leg's meta region, and a read reports each such leg OK once its
// probe has completed a round.
func TestCloneMetaLegsAreProbedOnThePrimary(t *testing.T) {
	srv, node := newTestServer(t)
	withClock(srv)
	clone := cloneWithGrp(true)
	clone.MetaGrp.SpareLegList = []*pb.Leg{cloneSpareLeg()}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true,
		clones: []*pb.Clone{clone}})
	st := legState(t, srv)
	// The clone meta group's three meta blocks end in the health block.
	wantOffset := 3*testBlockSize - common.LegHealthBlockSize
	for _, legId := range []uint64{
		testCloneLeg, testCloneLeg2, testCloneSpareLeg} {
		prober := st.probers[legId]
		if prober == nil {
			t.Fatalf("the primary started no prober for clone leg %#x",
				legId)
		}
		if prober.offset != wantOffset {
			t.Fatalf("clone leg %#x is probed at %d, want %d",
				legId, prober.offset, wantOffset)
		}
	}

	node.Reset()
	srv.runLegProbe(context.Background(), st.probers[testCloneLeg], testCn)
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d len=%d",
			srv.nf.DmPath(legName(srv, testCloneLeg)), wantOffset,
			common.LegHealthBlockSize),
		fmt.Sprintf("readblockdirect %s off=%d len=%d",
			srv.nf.DmPath(legName(srv, testCloneLeg)), wantOffset,
			common.LegHealthBlockSize),
	)

	probeAllLegs(t, srv)
	probed := checkRound(t, srv, 2)
	for _, legId := range []uint64{
		testCloneLeg, testCloneLeg2, testCloneSpareLeg} {
		assertOk(t, probed.GetLegIdToLeg()[legId], "probed clone leg")
	}
	assertOk(t, probed.GetGrpIdToMdRaid()[testCloneGrp], "clone meta group")
}

// assertHydrated fails unless a dm-clone row carries a status line that
// shows count of the fixture td's 64 regions hydrated.
func assertHydrated(t *testing.T, row *pb.ResInfo, count uint64, label string) {
	t.Helper()
	status, ok := common.ParseCloneStatus(row.GetDetails())
	if !ok || status.HydratedRegions != count || status.TotalRegions != 64 {
		t.Fatalf("%s: the dm row reads %v %q, want %d of 64 regions "+
			"hydrated", label, row.GetStatus(), row.GetDetails(), count)
	}
}

// TestACloneWaitsForItsCloneMetaGroup: a clone whose clone meta group still
// has a leg_list side zeroing is deferred as a clone whose destination still
// provisions is (CN18, [D15]): no source connect and no dm-clone, its two
// rows and its destination's namespace rows PROVISIONING on both channels,
// the namespace inaccessible over the td's dm-error, and no retry. The
// provisioned flip, a new revision, builds the group and the clone in one
// pass and moves the namespace optimized. A spare that zeroes defers
// nothing.
func TestACloneWaitsForItsCloneMetaGroup(t *testing.T) {
	for _, raid1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("raid1=%v", raid1), func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clone := cloneWithGrp(raid1)
			zeroing := clone.MetaGrp.LegList[len(clone.MetaGrp.LegList)-1]
			zeroing.SideList[0].Provisioned = false
			reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: raid1, clones: []*pb.Clone{clone}})
			info := reply.GetCntlrInfo()
			assertNoCall(t, node, "--nqn "+testSrcNqn)
			if _, ok := node.dms[cloneName(srv, testClone)]; ok {
				t.Fatalf("a deferred clone got a dm-clone")
			}
			probed := checkRound(t, srv, 2)
			for label, rows := range map[string]*pb.CntlrInfo{
				"converge": info, "check round": probed} {
				assertProvisioning(t, rows.GetCloneIdToTarget()[testClone],
					label+" target row")
				assertProvisioning(t, rows.GetCloneIdToDmClone()[testClone],
					label+" dm row")
				assertProvisioning(t, rows.GetNsIdToDmLinear()[testNs],
					label+" ns-dev row")
				assertProvisioning(t, rows.GetNsIdToNamespace()[testNs],
					label+" namespace row")
			}
			assertSameCloneRows(t, info, probed)
			assertParked(t, srv, node, testNs, testTd, "the deferred clone")
			assertAna(t, node, testNqn, "3", "the deferred clone")
			if retrying(t, srv) {
				t.Fatalf("a deferred clone registered the retry")
			}

			node.Reset()
			clone = cloneWithGrp(raid1)
			spare := cloneSpareLeg()
			spare.SideList[0].Provisioned = false
			clone.MetaGrp.SpareLegList = []*pb.Leg{spare}
			reply = syncupCntlrAt(t, srv, reqOpts{revision: 3,
				primary: true, raid1: raid1, clones: []*pb.Clone{clone}})
			info = reply.GetCntlrInfo()
			assertOrder(t, node,
				"--nqn "+legNqn(srv, zeroing.GetLegId())+" --hostnqn",
				"cmd nvme connect --transport tcp --traddr "+testIp2+
					" --trsvcid "+testSvcId2+" --nqn "+testSrcNqn,
				"cmd dmsetup create "+cloneName(srv, testClone),
				"cmd dmsetup message "+cloneName(srv, testClone)+
					" 0 enable_hydration",
				"writedirect "+anaPath(testNqn, 1)+"=1")
			assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm row")
			assertOk(t, info.GetNsIdToNamespace()[testNs], "namespace row")
			assertAna(t, node, testNqn, "1", "the built clone")
		})
	}
}

// TestACloneMetaGroupThatProvisionsDefersOnlyItsNamespaces: a clone meta
// group that still provisions defers its destination td's namespaces below
// SP_LEVEL_NO_CLONE only, and never a transfer of them (CN9, CN17, CN19). At
// SP_LEVEL_NO_CLONE the level wins: the clone's dm row reads the level, and
// the destination namespace stays optimized over the td's dm-error (CN16
// rule 4), its rows OK on both channels. When the level is lowered while
// the group still provisions, the namespace is deferred: the pass moves it
// inaccessible at its start and builds no dm-clone (CN16 rule 0, CN19).
// Below the level, a transfer of the destination namespace maps the td's
// raid0 and serves, its rows OK and its namespace optimized, while the
// namespace itself waits inaccessible with its rows PROVISIONING.
func TestACloneMetaGroupThatProvisionsDefersOnlyItsNamespaces(t *testing.T) {
	zeroingClone := func() *pb.Clone {
		clone := cloneWithGrp(false)
		clone.MetaGrp.LegList[0].SideList[0].Provisioned = false
		return clone
	}
	t.Run("at SP_LEVEL_NO_CLONE", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			level:  pb.SpLevel_SP_LEVEL_NO_CLONE,
			clones: []*pb.Clone{zeroingClone()}})
		probed := checkRound(t, srv, 2)
		for label, rows := range map[string]*pb.CntlrInfo{
			"converge": reply.GetCntlrInfo(), "check round": probed} {
			assertMissingSpLevel(t, rows.GetCloneIdToDmClone()[testClone],
				label+" dm row")
			assertOk(t, rows.GetNsIdToDmLinear()[testNs],
				label+" ns-dev row")
			assertOk(t, rows.GetNsIdToNamespace()[testNs],
				label+" namespace row")
		}
		assertParked(t, srv, node, testNs, testTd, "the level")
		assertAna(t, node, testNqn, "1", "the level")

		node.Reset()
		lowered, err := srv.SyncupCntlr(context.Background(), cntlrReq(
			reqOpts{revision: 3, primary: true,
				clones: []*pb.Clone{zeroingClone()}}))
		if err != nil {
			t.Fatalf("the level lowered: %v", err)
		}
		rows := lowered.GetCntlrInfo()
		assertProvisioning(t, rows.GetCloneIdToDmClone()[testClone],
			"the lowered level's dm row")
		assertProvisioning(t, rows.GetNsIdToNamespace()[testNs],
			"the lowered level's namespace row")
		assertNoCall(t, node, "cmd dmsetup create "+cloneName(srv, testClone))
		// CN9's pre-step 1 moves it, ahead of the build phase's leg reads.
		moved := node.indexOfCallFrom(
			"writedirect "+anaPath(testNqn, 1)+"=3", 0)
		build := node.indexOfCallFrom("read /sys/class/nvme/nvme0/state", 0)
		if moved < 0 || build < 0 || moved > build {
			t.Fatalf("the lowered level moved the namespace at %d, the "+
				"build began at %d, want the move first\ncalls:\n%s",
				moved, build, strings.Join(node.Calls(), "\n"))
		}
		assertParked(t, srv, node, testNs, testTd, "the lowered level")
		assertAna(t, node, testNqn, "3", "the lowered level")
	})
	t.Run("a transfer of the destination namespace", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		xfers := []*pb.Transfer{
			{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
		reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{zeroingClone()}, xfers: xfers})
		probed := checkRound(t, srv, 2)
		for label, rows := range map[string]*pb.CntlrInfo{
			"converge": reply.GetCntlrInfo(), "check round": probed} {
			assertProvisioning(t, rows.GetNsIdToNamespace()[testNs],
				label+" namespace row")
			assertOk(t, rows.GetXferIdToDmLinear()[testXfer],
				label+" transfer device row")
			assertOk(t, rows.GetXferIdToSubsystem()[testXfer],
				label+" transfer subsystem row")
			assertOk(t, rows.GetXferIdToNamespace()[testXfer],
				label+" transfer namespace row")
		}
		raid0No := devNoOf(node, raid0Name(srv, testTd))
		if raid0No == "" {
			t.Fatalf("the destination td has no raid0")
		}
		if got := node.dms[xferName(srv, testXfer)].table; got !=
			nsDevTableOn(raid0No) {
			t.Fatalf("the transfer device is %q, want it on the raid0 %q",
				got, nsDevTableOn(raid0No))
		}
		xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
		assertAna(t, node, xferNqn, "1", "the transfer")
		assertAna(t, node, testNqn, "3", "the deferred namespace")
	})
}

// TestAFailoverBuildsTheCloneOverItsCloneMetaGroup: a clone's metadata
// survives the move of the primary role (CN18). On demotion the standby
// keeps the clone meta group's legs connected and wrapped while the dm-clone
// and the group device go; on promotion the new primary assembles the group
// from its legs — never creates it — and creates the dm-clone over it, which
// finds the hydrated count the copy had reached and carries on from there.
func TestAFailoverBuildsTheCloneOverItsCloneMetaGroup(t *testing.T) {
	for _, raid1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("raid1=%v", raid1), func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			opts := func(revision uint64, primary bool) reqOpts {
				return reqOpts{revision: revision, primary: primary,
					raid1: raid1, clones: []*pb.Clone{cloneOf()}}
			}
			syncupBoth(t, srv, opts(2, true))
			clone := cloneName(srv, testClone)
			node.setCloneHydrated(t, clone, 29)

			syncupCntlrUntilClean(t, srv, opts(3, false))
			if _, ok := node.dms[clone]; ok {
				t.Fatalf("the standby kept the dm-clone")
			}
			if raid1 {
				if node.mdNode(cloneMdPath(srv)) != "" {
					t.Fatalf("the standby kept the clone meta group's array")
				}
			} else if _, ok := node.dms[grpName(srv, testCloneGrp)]; ok {
				t.Fatalf("the standby kept the clone meta group's device")
			}
			for _, legId := range cloneLegIds(raid1) {
				if _, ok := node.dms[legName(srv, legId)]; !ok {
					t.Fatalf("the standby dropped leg %#x's wrapper", legId)
				}
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			if raid1 {
				assertOrder(t, node,
					"cmd mdadm --assemble "+cloneMdPath(srv),
					"cmd dmsetup create "+clone)
				assertNoCall(t, node, "cmd mdadm --create "+cloneMdPath(srv))
			} else {
				assertOrder(t, node,
					"cmd dmsetup create "+grpName(srv, testCloneGrp),
					"cmd dmsetup create "+clone)
			}
			row := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
			assertOk(t, row, "the new primary's dm row")
			assertHydrated(t, row, 29, "the new primary")
			assertHydrated(t, probedCntlrInfo(t, srv).
				GetCloneIdToDmClone()[testClone], 29, "a read of it")
			assertAna(t, node, testNqn, "1", "the new primary")
		})
	}
}

// TestAnAgentRestartKeepsTheClone: a cn agent that restarts over a node whose
// clone stack is up (CN2) converges it as it is — no leg wrapper, array or
// group device created or reloaded, no dm-clone created or reloaded, no
// chunk applied and no enable sent — and its rows read the hydrated count
// the dm-clone's metadata holds.
func TestAnAgentRestartKeepsTheClone(t *testing.T) {
	for _, raid1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("raid1=%v", raid1), func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: raid1, clones: []*pb.Clone{cloneOf()}})
			pushBitmap(t, srv, hexBytes(t, testSkipHex))
			clone := cloneName(srv, testClone)
			node.setCloneHydrated(t, clone, 41)
			table := node.dms[clone].table

			node.Reset()
			fresh := newCnServer(node)
			reconcileForTest(t, fresh)
			assertNoCall(t, node, "cmd mdadm --create")
			assertNoCall(t, node, "cmd mdadm --assemble")
			for _, legId := range cloneLegIds(raid1) {
				assertNoCall(t, node, "cmd dmsetup create "+legName(srv, legId))
				assertNoCall(t, node, "cmd dmsetup reload "+legName(srv, legId))
			}
			if !raid1 {
				grp := grpName(srv, testCloneGrp)
				if _, ok := node.dms[grp]; !ok {
					t.Fatalf("the case is vacuous: no group device %s", grp)
				}
				assertNoCall(t, node, "cmd dmsetup create "+grp)
				assertNoCall(t, node, "cmd dmsetup reload "+grp)
			}
			assertNoCall(t, node, "cmd dmsetup create "+clone)
			assertNoCall(t, node, "cmd dmsetup reload "+clone)
			assertNoCall(t, node, "cmd dmsetup message "+clone)
			assertNoCall(t, node, "cmd blkdiscard")
			if got := node.dms[clone].table; got != table {
				t.Fatalf("the restart changed the dm-clone's table to %q, "+
					"want %q", got, table)
			}
			assertHydrated(t, probedCntlrInfo(t, fresh).
				GetCloneIdToDmClone()[testClone], 41, "after the restart")
		})
	}
}

// TestABrokenMetadataModeIsLeftAsItIs: a dm-clone whose metadata is
// read-only or failed — what dm-clone does when an operation on its clone
// meta group fails — is left as it is (CN18): no chunk, no knob, no enable,
// no reload and no removal. CN16 does not serve through it, so its
// namespace waits inaccessible over the td's dm-error, its dm row reads
// ERROR naming the mode on both channels, and the retry is registered.
func TestABrokenMetadataModeIsLeftAsItIs(t *testing.T) {
	for _, tc := range []struct {
		mode, details string
	}{
		{common.CloneMetadataReadOnly, detailsCloneMetaReadOnly},
		{common.CloneMetadataFail, detailsCloneMetaFailed},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: true, raid1: true,
				clones: []*pb.Clone{cloneOf()}}
			syncupBoth(t, srv, opts)
			pushBitmap(t, srv, hexBytes(t, testSkipHex))
			clone := cloneName(srv, testClone)
			node.dms[clone].metaMode = tc.mode
			node.dms[clone].noHydration = true

			node.Reset()
			opts.revision = 3
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			for _, call := range []string{"cmd dmsetup message " + clone,
				"cmd dmsetup reload " + clone, "cmd dmsetup remove " + clone,
				"cmd dmsetup create " + clone, "cmd blkdiscard"} {
				assertNoCall(t, node, call)
			}
			assertParked(t, srv, node, testNs, testTd, "a broken dm-clone")
			assertAna(t, node, testNqn, "3", "a broken dm-clone")
			for label, rows := range map[string]*pb.CntlrInfo{
				"converge": reply.GetCntlrInfo(),
				"read":     probedCntlrInfo(t, srv)} {
				assertErrorDetails(t, rows.GetCloneIdToDmClone()[testClone],
					tc.details, label+" dm row")
				assertOk(t, rows.GetNsIdToDmLinear()[testNs],
					label+" ns-dev row")
			}
			if !retrying(t, srv) {
				t.Fatalf("a dm-clone that does not serve registered no " +
					"retry")
			}
		})
	}
}
