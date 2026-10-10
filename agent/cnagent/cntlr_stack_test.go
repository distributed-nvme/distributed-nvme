package cnagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The cntlr's stack where its order matters: the teardown order of the CN9
// pass, the CN18 clone paths, the CN12/CN13 wrappers, the CN16 admission
// rules and the clone-row verdicts. The fake node models dm/md holders, so an
// out-of-order teardown surfaces as a real EBUSY.

// A transfer's dm-linear maps the origin td's raid0. If CN9's pre-step 3 does
// not demote it to an error table, `dmsetup remove` of that raid0 fails EBUSY
// and the whole cascade behind it — thin volumes, pool, concats,
// `mdadm --stop` — fails with it, leaving a demoted cntlr with its arrays
// still assembled while the new primary assembles the same ones.
func TestFailoverWithTransferReleasesTheStack(t *testing.T) {
	srv, node := newTestServer(t)
	xfers := []*pb.Transfer{{
		XferId:      testXfer,
		OriNqn:      testNqn,
		OriNsIdx:    1,
		AutoSuspend: true,
	}}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})
	if _, ok := node.dms[raid0Name(srv, testTd)]; !ok {
		t.Fatalf("the fixture never built the raid0")
	}

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: false, xfers: xfers})); err != nil {
		t.Fatalf("demote: %v", err)
	}
	// The transfer device is demoted before anything under it is removed.
	assertOrder(t, node,
		"cmd dmsetup reload "+xferName(srv, testXfer),
		"cmd dmsetup remove "+raid0Name(srv, testTd),
	)
	for _, name := range []string{
		raid0Name(srv, testTd),
		thinName(srv, testTd),
		poolName(srv),
		srv.nf.CnPoolMetaName(testCluster, testCn, testSp, testSlice),
		srv.nf.CnPoolDataName(testCluster, testCn, testSp, testSlice),
		grpName(srv, testMetaGrp),
	} {
		if _, ok := node.dms[name]; ok {
			t.Fatalf("%s survived the demotion", name)
		}
	}
	// A standby keeps the transfer exported, error-backed (architecture.md,
	// Standby cntlr).
	table := node.dms[xferName(srv, testXfer)].table
	if !strings.Contains(table, " error") {
		t.Fatalf("the standby's transfer device is %q, want an error table",
			table)
	}
}

// TestPrimaryTransferLetsGoOfADepartedOrigin is CN17's live-table size
// fallback on a cntlr that stays primary. The transfer's origin no longer
// resolves (CN29) in either of its two shapes — its namespace and td left the
// request together, or only its td left while the namespace stayed — so the
// plan can size nothing, but the transfer device still maps the departed td's
// raid0. Unless the primary demotes it too, the linear holds that raid0 open:
// in the first shape L6's `dmsetup remove` of it fails EBUSY on every pass,
// and the leftover re-drive never ends while the transfer stays in the
// request.
func TestPrimaryTransferLetsGoOfADepartedOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		// subsys is the request's subsystem map: nil keeps the default,
		// whose namespace still names the departed td.
		subsys map[string]*pb.Subsystem
		// released: nothing else holds the raid0, so the pass removes it
		// and replies clean. With the namespace kept, its still-wanted
		// ns-dev holds the raid0 open itself (it has no td to park onto),
		// so only the transfer's demotion is this test's business.
		released bool
	}{
		{name: "namespace and td gone",
			subsys: map[string]*pb.Subsystem{}, released: true},
		{name: "td gone, namespace kept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			xfers := []*pb.Transfer{{
				XferId:   testXfer,
				OriNqn:   testNqn,
				OriNsIdx: 1,
			}}
			syncupBoth(t, srv,
				reqOpts{revision: 2, primary: true, xfers: xfers})
			xfer := xferName(srv, testXfer)
			raid0 := raid0Name(srv, testTd)
			raid0No := node.devNo["/dev/mapper/"+raid0]
			if !strings.Contains(node.dms[xfer].table, "linear "+raid0No) {
				t.Fatalf("the fixture transfer does not map the raid0: %q",
					node.dms[xfer].table)
			}
			sectors := testTdSize / 512

			departed := reqOpts{revision: 3, primary: true, xfers: xfers,
				tds: []*pb.ThinDevice{}, subsys: tc.subsys}
			node.Reset()
			reply, err := srv.SyncupCntlr(
				context.Background(), cntlrReq(departed))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			want := fmt.Sprintf("0 %d error", sectors)
			if got := node.dms[xfer].table; got != want {
				t.Fatalf("the transfer device is %q, want %q", got, want)
			}
			if n := len(node.callsMatching(
				"cmd dmsetup reload " + xfer)); n != 1 {
				t.Fatalf("the transfer device was reloaded %d times, "+
					"want 1", n)
			}
			if tc.released {
				if code := reply.GetAgentReply().GetCode(); code != 0 {
					t.Fatalf("code %d (%q): the departed raid0 was not "+
						"released", code, reply.GetAgentReply().GetDetails())
				}
				if _, ok := node.dms[raid0]; ok {
					t.Fatalf("the departed td's raid0 %s survived", raid0)
				}
				assertOrder(t, node,
					"cmd dmsetup reload "+xfer,
					"cmd dmsetup remove "+raid0,
				)
			}

			// Nothing is remembered: the next pass reads the device's own
			// error table, finds it already the demoted shape, and reloads
			// nothing.
			node.Reset()
			if _, err := srv.SyncupCntlr(
				context.Background(), cntlrReq(departed)); err != nil {
				t.Fatalf("re-sync: %v", err)
			}
			assertNoCall(t, node, "cmd dmsetup reload "+xfer)
		})
	}
}

// TestStoppedPassDemotesATransferWithADepartedOrigin is the same demotion on
// a pass whose sweep an unanswered listing stopped. Such a pass runs no
// pre-step 3, so the build's own converge of the transfer device is the one
// that makes it let go of the departed td's raid0 — and for an origin that
// no longer resolves the build has no plan size to converge it to. Left
// alone, the transfer goes on serving the departed td's data, over an
// optimized path on a primary, until a pass whose listings answer. Each of
// the four listings is killed once on the departing pass, for the two shapes
// of a primary and for a cntlr demoted to standby in the same revision: the
// pass reloads the transfer device once, onto an error table of its own
// live size, and the next pass, its listings answering, reloads it no more.
func TestStoppedPassDemotesATransferWithADepartedOrigin(t *testing.T) {
	xfers := []*pb.Transfer{{
		XferId:   testXfer,
		OriNqn:   testNqn,
		OriNsIdx: 1,
	}}
	for _, shape := range []struct {
		name    string
		primary bool
		subsys  map[string]*pb.Subsystem
	}{
		{"namespace and td gone", true, map[string]*pb.Subsystem{}},
		{"td gone, namespace kept", true, nil},
		{"demoted to standby, namespace and td gone", false,
			map[string]*pb.Subsystem{}},
	} {
		for _, listing := range cnSweepListings {
			t.Run(shape.name+"/"+listing.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				syncupBoth(t, srv,
					reqOpts{revision: 2, primary: true, xfers: xfers})
				xfer := xferName(srv, testXfer)
				raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
				if !strings.Contains(node.dms[xfer].table,
					"linear "+raid0No) {
					t.Fatalf("the fixture transfer does not map the "+
						"raid0: %q", node.dms[xfer].table)
				}
				departed := cntlrReq(reqOpts{revision: 3,
					primary: shape.primary, xfers: xfers,
					tds: []*pb.ThinDevice{}, subsys: shape.subsys})

				node.Reset()
				node.killCmd[listing.kill] = true
				reply, err := srv.SyncupCntlr(context.Background(), departed)
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				cnSweepAssertCode(t, reply.GetAgentReply(),
					common.ReplyCodeLeftover, "the stopped pass")
				want := fmt.Sprintf("0 %d error", testTdSize/512)
				if got := node.dms[xfer].table; got != want {
					t.Fatalf("the transfer device is %q after the stopped "+
						"pass, want %q", got, want)
				}
				if n := len(node.callsMatching(
					"cmd dmsetup reload " + xfer)); n != 1 {
					t.Fatalf("the stopped pass reloaded the transfer "+
						"device %d times, want 1", n)
				}

				node.Reset()
				if _, err := srv.SyncupCntlr(
					context.Background(), departed); err != nil {
					t.Fatalf("re-sync: %v", err)
				}
				assertNoCall(t, node, "cmd dmsetup reload "+xfer)
			})
		}
	}
}

// CN22/CN19: a clone the role or the level merely suppresses keeps its chunk
// files. Deleting them made the worker re-push forever and left a promoted
// standby's rebuild (architecture.md, Clone crash recovery) nothing to skip
// with.
func TestSuppressedCloneKeepsItsChunks(t *testing.T) {
	srv, node := newTestServer(t)
	// A standby that nonetheless carries the clone in its desired state.
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: false, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0, 0)
	if _, ok := node.protos[bmPath]; !ok {
		t.Fatalf("the chunk was not persisted")
	}

	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: false, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if _, ok := node.protos[bmPath]; !ok {
		t.Fatalf("a standby converge deleted the clone's chunk file")
	}
	if len(reply.GetBmInfoList()) != 1 ||
		len(chunkIds(reply.GetBmInfoList()[0])) != 1 {
		t.Fatalf("the applied set was dropped: %v", reply.GetBmInfoList())
	}

	// Deleting the clone from clone_list does remove them (SH7).
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4, primary: false})); err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	if _, ok := node.protos[bmPath]; ok {
		t.Fatalf("a deleted clone kept its chunk file")
	}
}

// The recovery build of cnagent.md CN18 keys off missing dm-clone *metadata*,
// not off the metadata wrapper: a
// pass that created the wrapper and then failed to build the dm-clone leaves a
// freshly discarded slot that dm-clone would format fresh, with nothing
// hydrated.
func TestCloneRecoveryWhenOnlyTheDmCloneVanished(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)

	// The wrapper survives; the dm-clone does not.
	removeCloneDevice(node, srv)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap ",
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
		"cmd dmsetup message "+cloneName(srv, testClone)+
			" 0 enable_hydration",
	)
	// A wrapper that is present and matches is reused as-is: "allocate"
	// strictly means "a new unit range was chosen", so the surviving slot is
	// never re-created and — decisively — never re-hole-punched, which would
	// wipe the valid dm-clone superblock it carries (CN18).
	assertNoCall(t, node, "cmd dmsetup create "+cloneMetaName(srv, testClone))
	assertNoCall(t, node, "cmd blkdiscard --offset 0 --length 8388608 "+loop)
}

// removeCloneDevice drops the dm-clone behind the agent's back, the way a
// crash between the wrapper's creation and `dmsetup create` leaves it.
func removeCloneDevice(node *fakeNode, srv *CnAgentServer) {
	name := cloneName(srv, testClone)
	delete(node.dms, name)
	delete(node.devNo, "/dev/mapper/"+name)
	delete(node.devSize, "/dev/mapper/"+name)
}

// architecture.md, Clone crash recovery: the destination bitmaps must be
// applied in full before the dm-clone
// handles any IO. A read that fails must therefore leave nothing able to serve
// or hydrate — not enable hydration and report OK.
func TestCloneRecoveryFailsClosed(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	removeCloneDevice(node, srv)

	node.Reset()
	node.failCmdAlways["thin_dump"] = "thin_dump: out of time"
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	info := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("a failed destination-bitmap read reported %v/%q",
			info.GetStatus(), info.GetDetails())
	}
	assertNoCall(t, node, "0 enable_hydration")
	if _, ok := node.dms[cloneName(srv, testClone)]; ok {
		t.Fatalf("the dm-clone was left able to serve and hydrate")
	}
	// Nothing serves the td: the ns-dev stays on the td's dm-error.
	table := node.dms[nsDevName(srv, testNs)].table
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	if !strings.Contains(table, "linear "+errNo) {
		t.Fatalf("the ns-dev is not parked: %q", table)
	}
}

// cnagent.md CN12 puts a single available member in case 2 (assemble, and
// mdadm decides on a degraded start), never in case 1: creating with
// --assume-clean over the available subset would resync the unavailable
// survivor's data away.
func TestGroupNeverCreatesOverASubsetOfLegs(t *testing.T) {
	srv, node := newTestServer(t)
	// The second data leg's side exports dm-error, so its path is
	// non-optimized and the leg is not available.
	node.anaOf[srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2, testCn)+
		"|"+testIp2+":"+testSvcId2] = "non-optimized"

	reply := syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	for _, call := range node.callsMatching("cmd mdadm --create") {
		if strings.Contains(call, mdDev) {
			t.Fatalf("created an array over a subset of legs: %s", call)
		}
	}
	info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("the group reported %v, want ERROR", info.GetStatus())
	}
	assertNoCall(t, node, "cmd mdadm --detail")
}

// CN13: a GrowSlice that appends only meta groups changes nothing in the
// thin-pool's own table, and dm-thin picks up a resized metadata device only
// in pool_preresume — so the pool must be reloaded anyway.
func TestGrowSliceReloadsThePool(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	grown := cntlrReq(reqOpts{revision: 3, primary: true})
	slice := grown.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
	second := &pb.Group{
		GrpId:      testMetaGrp + 0x100,
		ExtCnt:     1,
		MetaBlocks: 1,
		DataBlocks: 63,
		LegList: []*pb.Leg{legOf(testMetaLeg+0x100,
			sideOf(testMetaSide+0x200, testIp2, testSvcId2))},
	}
	slice.MetaGrpList = append(slice.MetaGrpList, second)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), grown); err != nil {
		t.Fatalf("grow: %v", err)
	}
	metaName := srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice)
	assertOrder(t, node,
		"cmd dmsetup reload "+metaName,
		"cmd dmsetup reload "+poolName(srv),
	)
}

// TestPoolTableNeverSkipsBlockZeroing pins the thin-pool table at both of its
// writers, the create and the reload a grow forces: the four arguments of
// poolArgs — the metadata and data devices, the block size in sectors and the
// low-water mark — and nothing after them, not even a feature count. dm-thin
// then keeps its default of writing a block it provisions whole before a host
// can read any of it, which is what keeps the old bytes of a data group's
// recycled extents from every host ([D15]; cnagent.md CN13). The converge
// and the probe compare only the four leading arguments, since dm-thin
// appends its features to the table it reports, so no other check would
// notice a feature argument slipping in.
func TestPoolTableNeverSkipsBlockZeroing(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	wantPoolTable(t, srv, node)

	// A meta grow: the pool's table is reloaded unchanged (CN13).
	grown := cntlrReq(reqOpts{revision: 3, primary: true})
	slice := grown.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
	slice.MetaGrpList = append(slice.MetaGrpList, &pb.Group{
		GrpId:      testMetaGrp + 0x100,
		ExtCnt:     1,
		MetaBlocks: 1,
		DataBlocks: 63,
		LegList: []*pb.Leg{legOf(testMetaLeg+0x100,
			sideOf(testMetaSide+0x200, testIp2, testSvcId2))},
	})
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), grown); err != nil {
		t.Fatalf("grow: %v", err)
	}
	assertOrder(t, node, "cmd dmsetup reload "+poolName(srv))
	wantPoolTable(t, srv, node)
}

// wantPoolTable checks the slice's live thin-pool table against poolArgs'
// four arguments, with nothing after them.
func wantPoolTable(t *testing.T, srv *CnAgentServer, node *fakeNode) {
	t.Helper()
	dm := node.dms[poolName(srv)]
	if dm == nil {
		t.Fatalf("no thin-pool device %s", poolName(srv))
	}
	if strings.Contains(dm.table, "skip_block_zeroing") {
		t.Fatalf("pool table %q skips block zeroing", dm.table)
	}
	fields := strings.Fields(dm.table)
	if len(fields) != 7 {
		t.Fatalf("pool table %q has %d fields, want start, length, "+
			"target and exactly four arguments", dm.table, len(fields))
	}
	metaNo := node.devNo[srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))]
	dataNo := node.devNo[srv.nf.DmPath(srv.nf.CnPoolDataName(
		testCluster, testCn, testSp, testSlice))]
	want := []string{
		"0", fields[1], "thin-pool", metaNo, dataNo,
		strconv.FormatUint(testBlockSize/agent.SectorSize, 10),
	}
	for i, field := range want {
		if field == "" || fields[i] != field {
			t.Fatalf("pool table %q: field %d is %q, want %q",
				dm.table, i, fields[i], field)
		}
	}
	if _, err := strconv.ParseUint(fields[6], 10, 64); err != nil {
		t.Fatalf("pool table %q: the low-water mark %q is not a block "+
			"count", dm.table, fields[6])
	}
}

// ---------------------------------------------------------------------------
// Subsystem admission (CN16; architecture.md, Primary cntlr, step 6)
//
// The host links are the only admission gate: an empty `allowed_hosts` admits
// no host, and "attr_allow_any_host" is only ever written 0, ahead of the host
// links, since nvmet refuses a host link while it is 1.
// ---------------------------------------------------------------------------

// fixtureSubsysPath is one entry under the fixture's host-facing subsystem.
func fixtureSubsysPath(rel string) string {
	return agent.NvmetRoot + "/subsystems/" + testNqn + "/" + rel
}

// subsysWithHosts is defaultSubsys admitting exactly the given hosts; no
// argument is the empty list.
func subsysWithHosts(hosts ...string) map[string]*pb.Subsystem {
	subsys := defaultSubsys(false)
	subsys[testNqn].AllowedHosts = hosts
	return subsys
}

// probedSubsysRow is the fixture subsystem's row as the check channel reads it.
func probedSubsysRow(t *testing.T, srv *CnAgentServer) *pb.ResInfo {
	t.Helper()
	reply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	return reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs]
}

// TestEmptyingAllowedHostsRevokesEveryHost: emptying the list removes the
// last host link and leaves the subsystem closed, "attr_allow_any_host" still
// 0 and untouched, and both channels report that closed subsystem converged.
func TestEmptyingAllowedHostsRevokesEveryHost(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, subsys: subsysWithHosts(testHostNqn)})
	attr := fixtureSubsysPath("attr_allow_any_host")
	link := fixtureSubsysPath("allowed_hosts/" + testHostNqn)
	if _, ok := node.links[link]; !ok {
		t.Fatalf("the host was never linked")
	}
	if got := node.files[attr]; got != "0" {
		t.Fatalf("attr_allow_any_host is %q after the build, want 0", got)
	}
	assertNoCall(t, node, "writedirect "+attr+"=1")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, subsys: subsysWithHosts()}))
	if err != nil {
		t.Fatalf("empty the host list: %v", err)
	}
	if _, ok := node.links[link]; ok {
		t.Fatalf("the retired host link survived")
	}
	assertNoCall(t, node, "writedirect "+attr)
	if got := node.files[attr]; got != "0" {
		t.Fatalf("attr_allow_any_host is %q, want 0", got)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"converged subsystem with no host")
	assertOk(t, probedSubsysRow(t, srv), "probed subsystem with no host")
}

// TestAnOpenSubsystemIsClosedBeforeAnyHostLink: a subsystem found with
// "attr_allow_any_host" at 1 — and so with no host link, which nvmet refuses
// beside it — converges closed whatever its list, and with a host to admit,
// the 0 is written before that host's link, which nvmet refuses while the
// attribute is 1.
func TestAnOpenSubsystemIsClosedBeforeAnyHostLink(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts []string
	}{
		{name: "no host"},
		{name: "one host", hosts: []string{testHostNqn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			attr := fixtureSubsysPath("attr_allow_any_host")
			node.files[attr] = "1"

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					subsys: subsysWithHosts(tc.hosts...)}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := node.files[attr]; got != "0" {
				t.Fatalf("attr_allow_any_host is %q, want 0", got)
			}
			assertNoCall(t, node, "writedirect "+attr+"=1")
			for _, hostNqn := range tc.hosts {
				link := fixtureSubsysPath("allowed_hosts/" + hostNqn)
				assertOrder(t, node,
					"writedirect "+attr+"=0",
					"cmd ln -s "+agent.NvmetRoot+"/hosts/"+hostNqn+" "+link,
				)
				if _, ok := node.links[link]; !ok {
					t.Fatalf("%s was never linked", hostNqn)
				}
			}
			assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
				"converged subsystem")
		})
	}
}

// TestProbeReportsAnOpenSubsystem: the check channel compares
// "attr_allow_any_host" with every other attribute, so a subsystem found open
// is not converged. With an empty list the host links match exactly, so the
// attribute alone decides the row.
func TestProbeReportsAnOpenSubsystem(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	assertOk(t, probedSubsysRow(t, srv), "probed closed subsystem")

	node.files[fixtureSubsysPath("attr_allow_any_host")] = "1"
	row := probedSubsysRow(t, srv)
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), "attr_allow_any_host") {
		t.Fatalf("the open subsystem probes %v/%q, want ERROR naming "+
			"attr_allow_any_host", row.GetStatus(), row.GetDetails())
	}
}

// ---------------------------------------------------------------------------
// Probe / converge agreement (CN19, CN23, CN28)
//
// The two channels answer the same question about the same stored request, so
// a row they disagree on makes the worker's err_epoch and its proto.Equal
// suppression flap on a state that is not changing.
// ---------------------------------------------------------------------------

// TestDisabledCntlrProbeReportsTheSuppressedRows: at SP_LEVEL_DISABLE the
// converge reports every cntlr-scoped resource RES_STATUS_MISSING /
// "sp_level" rather than leaving it out, because "a worker cannot tell an
// omitted resource from one the agent never looked at" (CN19) and because
// show_info promises the complete current CntlrInfo (architecture.md, Check
// streams). A probe that
// returned early skipped seven row families the converge fills.
func TestDisabledCntlrProbeReportsTheSuppressedRows(t *testing.T) {
	srv, _ := newTestServer(t)
	xfers := []*pb.Transfer{{
		XferId:   testXfer,
		OriNqn:   testNqn,
		OriNsIdx: 1,
	}}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})

	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, xfers: xfers,
		level: pb.SpLevel_SP_LEVEL_DISABLE}))
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	converged := reply.GetCntlrInfo()
	for label, res := range map[string]*pb.ResInfo{
		"td dm-error": converged.GetTdIdToDmError()[testTd],
		"ns-dev":      converged.GetNsIdToDmLinear()[testNs],
		"namespace":   converged.GetNsIdToNamespace()[testNs],
		"subsystem":   converged.GetSsIdToSubsystem()[testSs],
		"xfer dm":     converged.GetXferIdToDmLinear()[testXfer],
		"xfer subsys": converged.GetXferIdToSubsystem()[testXfer],
		"xfer ns":     converged.GetXferIdToNamespace()[testXfer],
	} {
		if res.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
			res.GetDetails() != "sp_level" {
			t.Fatalf("converged %s is %v/%q, want MISSING/sp_level",
				label, res.GetStatus(), res.GetDetails())
		}
	}

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	// Identical, epochs included: nothing changed between the two passes, so
	// a row that moves is a row the two paths disagree about.
	if !proto.Equal(converged, probe.GetCntlrInfo()) {
		t.Fatalf("the probe disagrees with the converge at SP_LEVEL_DISABLE:\n"+
			"converge:\n%v\nprobe:\n%v", converged, probe.GetCntlrInfo())
	}
}

// TestSuppressedCloneReportsSpLevel: a clone in clone_list at a level that
// suppresses clones reads its three rows MISSING / "sp_level" on both
// channels (CN19) — at SP_LEVEL_NO_CLONE, CN18's staged gate, and at
// SP_LEVEL_DISABLE, where reportSuppressed leaves the clones to the same
// function. The worker's settle (dnv-worker.md HL2) is held by every other
// MISSING, so a clone row that lost its "sp_level" would keep a primary with a
// clone settling for as long as the level stands.
func TestSuppressedCloneReportsSpLevel(t *testing.T) {
	srv, _ := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	for i, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_NO_CLONE,
		pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: uint64(3 + i), primary: true,
			clones: []*pb.Clone{cloneOf()}, level: level}))
		if err != nil {
			t.Fatalf("%v: %v", level, err)
		}
		assertCloneSuppressed(t, reply.GetCntlrInfo(), level.String()+
			" converged")

		probe, err := srv.GetCntlrInfo(context.Background(),
			&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr()})
		if err != nil {
			t.Fatalf("%v: GetCntlrInfo: %v", level, err)
		}
		assertCloneSuppressed(t, probe.GetCntlrInfo(), level.String()+
			" probed")
	}
}

func assertCloneSuppressed(t *testing.T, info *pb.CntlrInfo, label string) {
	t.Helper()
	assertMissingSpLevel(t, info.GetCloneIdToTarget()[testClone],
		label+" clone_id_to_target")
	assertMissingSpLevel(t, info.GetCloneIdToDmClone()[testClone],
		label+" clone_id_to_dm_clone")
	assertMissingSpLevel(t, info.GetCloneIdToMeta()[testClone],
		label+" clone_id_to_meta")
}

// TestCloneWithUnresolvableDstTdAgrees: a clone whose dst_td_id is not in
// td_list is the CN18 error the converge reports on all three rows, and the
// probe must report the same. It must not fall through to the live-source /
// dm-clone / wrapper probes: a MISSING (or an OK for a stack that outlived
// the td), or — with region_cnt = 0 — a one-unit metadata budget and a size
// mismatch that describes nothing on the node, would flip the rows and bump
// their epochs on every interleaved converge and check round.
func TestCloneWithUnresolvableDstTdAgrees(t *testing.T) {
	srv, _ := newTestServer(t)
	// Phase 1: the clone is built over its own destination td, so its metadata
	// wrapper really exists when the td leaves td_list.
	clone := cloneOfTd(testClone, testSrcNqn, testSnapTd)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds(),
		clones: []*pb.Clone{clone}})

	// Phase 2: the destination td is dropped while the clone stays.
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{clone}}))
	if err != nil {
		t.Fatalf("drop the destination td: %v", err)
	}
	details := fmt.Sprintf("destination td %d not found", testSnapTd)
	converged := reply.GetCntlrInfo()
	assertCloneRows(t, converged, "converged", details)

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	assertCloneRows(t, probe.GetCntlrInfo(), "probed", details)
	// No flap: the epoch of a row whose status never changed must not move.
	if got, want := probe.GetCntlrInfo().GetCloneIdToMeta()[testClone].
		GetEpoch(), converged.GetCloneIdToMeta()[testClone].GetEpoch(); got !=
		want {
		t.Fatalf("clone_id_to_meta epoch moved %d -> %d", want, got)
	}
}

func assertCloneRows(
	t *testing.T,
	info *pb.CntlrInfo,
	label string,
	details string,
) {
	t.Helper()
	for row, res := range map[string]*pb.ResInfo{
		"clone_id_to_target":   info.GetCloneIdToTarget()[testClone],
		"clone_id_to_dm_clone": info.GetCloneIdToDmClone()[testClone],
		"clone_id_to_meta":     info.GetCloneIdToMeta()[testClone],
	} {
		if res.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			res.GetDetails() != details {
			t.Fatalf("%s %s is %v/%q, want ERROR/%q",
				label, row, res.GetStatus(), res.GetDetails(), details)
		}
	}
}
