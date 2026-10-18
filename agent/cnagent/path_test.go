package cnagent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN28 — the path rows of a two-sided leg (legPathRows)
// ---------------------------------------------------------------------------
//
// A leg whose side_list holds two sides is a migration's. Every cntlr reports
// one row per side of such a leg in side_id_to_path, on both channels, from
// one function: whether this node holds a live controller to the side's
// address. The worker's cutover waits for the primary's row of the
// migration's dst side to read OK (dnv-worker.md RW18). The extraSide fixture
// gives the meta leg and the first data leg a second side at testIp2.

// The fixture's two-sided legs' sides: each leg's first side at testIp, its
// second at testIp2.
const (
	testMetaDst = testMetaSide + 0x100
	testDataDst = testDataSide + 0x100
)

var (
	srcPathName = testIp + ":" + testSvcId
	dstPathName = testIp2 + ":" + testSvcId2
)

// wantPath is one expected path row.
type wantPath struct {
	name    string
	status  pb.ResStatus
	details string
}

func pathOk(name, details string) wantPath {
	return wantPath{name, pb.ResStatus_RES_STATUS_OK, details}
}

// liveOptimized is what a live path the fake connected reads: the fake's
// controllers are live and, unless a test says otherwise, optimized.
const liveOptimized = "live/optimized"

// allPathsOk is the four rows of the extraSide fixture once every side is
// connected.
func allPathsOk() map[uint64]wantPath {
	return map[uint64]wantPath{
		testMetaSide: pathOk(srcPathName, liveOptimized),
		testMetaDst:  pathOk(dstPathName, liveOptimized),
		testDataSide: pathOk(srcPathName, liveOptimized),
		testDataDst:  pathOk(dstPathName, liveOptimized),
	}
}

// pathChannels is the fixture cntlr's path rows on every channel, in this
// order: the reply of a SyncupCntlr of opts, then a check round's and an info
// read's.
type pathChannels struct {
	converged map[uint64]*pb.ResInfo
	checked   map[uint64]*pb.ResInfo
	read      map[uint64]*pb.ResInfo
}

func readPathChannels(
	t *testing.T,
	srv *CnAgentServer,
	opts reqOpts,
) pathChannels {
	t.Helper()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	// A pass that leaves leftovers — the legs a level sweeps, disconnecting
	// in the background — or whose enumeration did not answer still answers
	// with its rows.
	if code := reply.GetAgentReply().GetCode(); code != 0 &&
		code != common.ReplyCodeLeftover {
		t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
	}
	return pathChannels{
		converged: reply.GetCntlrInfo().GetSideIdToPath(),
		checked:   checkRound(t, srv, opts.revision).GetSideIdToPath(),
		read:      probedCntlrInfo(t, srv).GetSideIdToPath(),
	}
}

// assert holds every channel to want: the same side ids, each row's
// name, status and details, and a tracker's epoch. The three channels carry
// the same rows, epochs included: nothing changed between them, so a row
// that moved would be a row the converge and the probe disagree about, and
// the two would trade it every round.
func (c pathChannels) assert(
	t *testing.T,
	label string,
	want map[uint64]wantPath,
) {
	t.Helper()
	for _, ch := range []struct {
		name string
		rows map[uint64]*pb.ResInfo
	}{
		{"converge", c.converged},
		{"check round", c.checked},
		{"info read", c.read},
	} {
		if got, wantIds := sortedSideIds(ch.rows), sortedWantIds(want); !equalIds(got, wantIds) {
			t.Fatalf("%s: the %s reports path rows for sides %#x, want %#x",
				label, ch.name, got, wantIds)
		}
		for sideId, w := range want {
			row := ch.rows[sideId]
			if row.GetResName() != w.name || row.GetStatus() != w.status ||
				row.GetDetails() != w.details {
				t.Fatalf("%s: the %s's path row of side %#x is %q %v/%q, "+
					"want %q %v/%q", label, ch.name, sideId,
					row.GetResName(), row.GetStatus(), row.GetDetails(),
					w.name, w.status, w.details)
			}
			if row.GetEpoch() == 0 {
				t.Fatalf("%s: the %s's path row of side %#x has no epoch",
					label, ch.name, sideId)
			}
			if row.GetReason() != pb.ResReason_RES_REASON_NONE {
				t.Fatalf("%s: the %s's path row of side %#x carries "+
					"reason %v", label, ch.name, sideId, row.GetReason())
			}
		}
	}
	for _, other := range []struct {
		name string
		rows map[uint64]*pb.ResInfo
	}{
		{"check round", c.checked},
		{"info read", c.read},
	} {
		for sideId, row := range c.converged {
			if !proto.Equal(row, other.rows[sideId]) {
				t.Fatalf("%s: the %s's path row of side %#x is %v, the "+
					"converge's %v", label, other.name, sideId,
					other.rows[sideId], row)
			}
		}
	}
}

func sortedSideIds(rows map[uint64]*pb.ResInfo) []uint64 {
	ids := make([]uint64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sortedWantIds(want map[uint64]wantPath) []uint64 {
	ids := make([]uint64, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func equalIds(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pathRoles is the two roles every path test runs as: a standby and a
// primary report the rows alike.
var pathRoles = []struct {
	name    string
	primary bool
}{
	{"standby", false},
	{"primary", true},
}

// TestPathRowsReportEachSideOfATwoSidedLeg pins the rows' shape: one per side
// of each two-sided leg, keyed by side id, named by the side's address, OK
// with the controller's state and ana_state, from a standby and a primary
// alike, on both channels. The second data leg has one side and reports none.
func TestPathRowsReportEachSideOfATwoSidedLeg(t *testing.T) {
	for _, role := range pathRoles {
		t.Run(role.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: role.primary,
				extraSide: true, twoLegs: true, raid1: role.primary}
			syncupBoth(t, srv, opts)
			readPathChannels(t, srv, opts).assert(t, "both sides live",
				allPathsOk())
		})
	}
}

// TestPathRowsOfOneSidedLegs: a cntlr none of whose legs has two sides
// reports no path row on any channel.
func TestPathRowsOfOneSidedLegs(t *testing.T) {
	for _, role := range pathRoles {
		t.Run(role.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			opts := reqOpts{revision: 2, primary: role.primary,
				twoLegs: true}
			syncupBoth(t, srv, opts)
			readPathChannels(t, srv, opts).assert(t, "one side each",
				map[uint64]wantPath{})
		})
	}
}

// TestPathRowsWhileASideProvisions: a side still zeroing has nothing exported,
// so its row reads PROVISIONING with the leg rows' details and no lookup
// judges it, while the leg's other side, provisioned, reads its controller.
// A leg whose every side is unprovisioned is itself provisioning and is not
// connected: both its rows read PROVISIONING.
func TestPathRowsWhileASideProvisions(t *testing.T) {
	provisioning := func(name string) wantPath {
		return wantPath{name, pb.ResStatus_RES_STATUS_PROVISIONING,
			detailsProvisioning}
	}
	for _, role := range pathRoles {
		t.Run(role.name+", the dst sides zeroing", func(t *testing.T) {
			srv, _ := newTestServer(t)
			opts := reqOpts{revision: 2, primary: role.primary,
				extraSide: true, unprovisionedExtraSide: true}
			syncupBoth(t, srv, opts)
			readPathChannels(t, srv, opts).assert(t, "dst zeroing",
				map[uint64]wantPath{
					testMetaSide: pathOk(srcPathName, liveOptimized),
					testMetaDst:  provisioning(dstPathName),
					testDataSide: pathOk(srcPathName, liveOptimized),
					testDataDst:  provisioning(dstPathName),
				})
		})
		t.Run(role.name+", a provisioning leg", func(t *testing.T) {
			srv, node := newTestServer(t)
			opts := reqOpts{revision: 2, primary: role.primary,
				extraSide: true, unprovisionedExtraSide: true,
				unprovisionedDataLeg: true}
			syncupBoth(t, srv, opts)
			if ctrlNameOf(node, legNqn(srv, testDataLeg), testIp,
				testSvcId) != "" {
				t.Fatalf("the provisioning data leg is connected")
			}
			readPathChannels(t, srv, opts).assert(t, "data leg provisioning",
				map[uint64]wantPath{
					testMetaSide: pathOk(srcPathName, liveOptimized),
					testMetaDst:  provisioning(dstPathName),
					testDataSide: provisioning(srcPathName),
					testDataDst:  provisioning(dstPathName),
				})
		})
	}
}

// ctrlNameOf is the name of the fake's controller of nqn at one address, ""
// when it holds none.
func ctrlNameOf(node *fakeNode, nqn, addr, svcId string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	if subsys := node.subsystems[nqn]; subsys != nil {
		for _, ctrl := range subsys.ctrls {
			if ctrl.trAddr == addr && ctrl.trSvcId == svcId {
				return ctrl.name
			}
		}
	}
	return ""
}

// TestPathRowsFollowTheController pins each row's verdict on the data leg's
// dst side, built and then changed under the agent, on both channels: no
// controller to the address is MISSING "no controller" — the converge's
// connect is refused, and its row reads what the node holds, not the connect's
// error; a controller that is not live is ERROR with its state and ana_state;
// a live one is OK whatever its ana_state, inaccessible included, which is
// how a dst side waiting for its cutover exports it; and a live path whose
// ana_state the kernel's scan has not added yet reads "live/".
func TestPathRowsFollowTheController(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arm changes the data leg's dst path after the build.
		arm  func(t *testing.T, node *fakeNode, nqn string)
		want wantPath
	}{
		{
			name: "no controller",
			arm: func(t *testing.T, node *fakeNode, nqn string) {
				name := ctrlNameOf(node, nqn, testIp2, testSvcId2)
				if name == "" {
					t.Fatalf("the data leg has no dst controller")
				}
				node.mu.Lock()
				node.nvmeDisconnect([]string{"disconnect", "--device", name})
				node.mu.Unlock()
				failConnects(node, nqn, testIp2, testSvcId2, 1<<20)
			},
			want: wantPath{dstPathName, pb.ResStatus_RES_STATUS_MISSING,
				detailsNoController},
		},
		{
			name: "connecting",
			arm: func(t *testing.T, node *fakeNode, nqn string) {
				node.setCtrlState(nqn, testIp2, testSvcId2, "connecting")
			},
			want: wantPath{dstPathName, pb.ResStatus_RES_STATUS_ERROR,
				"connecting/optimized"},
		},
		{
			name: "live and inaccessible",
			arm: func(t *testing.T, node *fakeNode, nqn string) {
				node.setAnaState(nqn, testIp2, testSvcId2, "inaccessible")
			},
			want: pathOk(dstPathName, "live/inaccessible"),
		},
		{
			name: "live, its ana_state not added yet",
			arm: func(t *testing.T, node *fakeNode, nqn string) {
				name := ctrlNameOf(node, nqn, testIp2, testSvcId2)
				node.mu.Lock()
				defer node.mu.Unlock()
				for file := range node.files {
					if strings.HasPrefix(file, sysfsNvmeCtrlDir+"/"+name+
						"/") && strings.HasSuffix(file, "/ana_state") {
						delete(node.files, file)
					}
				}
			},
			want: pathOk(dstPathName, "live/"),
		},
	} {
		for _, role := range pathRoles {
			t.Run(tc.name+", "+role.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				opts := reqOpts{revision: 2, primary: role.primary,
					extraSide: true}
				syncupBoth(t, srv, opts)
				tc.arm(t, node, legNqn(srv, testDataLeg))
				want := allPathsOk()
				want[testDataDst] = tc.want
				opts.revision = 3
				readPathChannels(t, srv, opts).assert(t, tc.name, want)
			})
		}
	}
}

// TestPathRowsFollowTheLevel pins CN19 on the path rows: at a level that
// suppresses legs every row reads MISSING "sp_level", on a standby as on a
// primary, as the leg rows do; at a level below it, SP_LEVEL_NO_MIGRATION
// among them, the cntlr holds its legs and reports their paths.
func TestPathRowsFollowTheLevel(t *testing.T) {
	suppressed := map[uint64]wantPath{}
	for sideId, w := range allPathsOk() {
		suppressed[sideId] = wantPath{w.name,
			pb.ResStatus_RES_STATUS_MISSING, detailsSpLevel}
	}
	for _, tc := range []struct {
		level pb.SpLevel
		want  map[uint64]wantPath
	}{
		{pb.SpLevel_SP_LEVEL_NO_MIGRATION, allPathsOk()},
		{pb.SpLevel_SP_LEVEL_NO_SIDE, suppressed},
		{pb.SpLevel_SP_LEVEL_DISABLE, suppressed},
	} {
		for _, role := range pathRoles {
			t.Run(tc.level.String()+", "+role.name, func(t *testing.T) {
				srv, _ := newTestServer(t)
				srv.retryInterval = time.Hour
				opts := reqOpts{revision: 2, primary: role.primary,
					extraSide: true}
				syncupBoth(t, srv, opts)
				opts.revision, opts.level = 3, tc.level
				readPathChannels(t, srv, opts).assert(t, tc.level.String(),
					tc.want)
			})
		}
	}
}

// TestPathRowsWhileTheLegDisconnects: while a sweep's disconnect of the leg's
// subsystem is in flight the connect step stops before it looks the leg up
// (CN10's disconnect registry), and so do the path rows of its provisioned
// sides, which read ERROR with the leg row's details on both channels. A side
// still zeroing reads PROVISIONING all the same.
func TestPathRowsWhileTheLegDisconnects(t *testing.T) {
	for _, role := range pathRoles {
		t.Run(role.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: role.primary,
				extraSide: true, unprovisionedExtraSide: true}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			srv.mu.Lock()
			srv.disconnecting[nqn] = struct{}{}
			srv.mu.Unlock()
			t.Cleanup(func() {
				srv.mu.Lock()
				delete(srv.disconnecting, nqn)
				srv.mu.Unlock()
			})
			opts.revision = 3
			readPathChannels(t, srv, opts).assert(t, "disconnect in flight",
				map[uint64]wantPath{
					testMetaSide: pathOk(srcPathName, liveOptimized),
					testMetaDst: {dstPathName,
						pb.ResStatus_RES_STATUS_PROVISIONING,
						detailsProvisioning},
					testDataSide: {srcPathName,
						pb.ResStatus_RES_STATUS_ERROR,
						detailsDisconnectInFlight(nqn)},
					testDataDst: {dstPathName,
						pb.ResStatus_RES_STATUS_PROVISIONING,
						detailsProvisioning},
				})
		})
	}
}

// TestPathRowsOfAnUnansweredRead pins CN10's "unknown, never unwanted" on the
// path rows: a walk that did not answer makes every provisioned side's row
// ERROR naming the read, never MISSING; and a controller whose address read
// did not answer may be any side's path, so the side no answering controller
// holds reads ERROR naming that controller, while the side whose controller
// answered reads it.
func TestPathRowsOfAnUnansweredRead(t *testing.T) {
	for _, role := range pathRoles {
		t.Run("the walk, "+role.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: role.primary,
				extraSide: true}
			syncupBoth(t, srv, opts)
			node.mu.Lock()
			node.failCmdAlways["list "+sysfsNvmeSubsysDir] = "EIO"
			node.mu.Unlock()
			opts.revision = 3
			ch := readPathChannels(t, srv, opts)
			details := ch.converged[testDataDst].GetDetails()
			if !strings.Contains(details, "is unknown") ||
				!strings.Contains(details, sysfsNvmeSubsysDir) {
				t.Fatalf("the dst path row reads %q, want the walk named",
					details)
			}
			want := map[uint64]wantPath{}
			for _, lp := range []struct{ leg, src, dst uint64 }{
				{testMetaLeg, testMetaSide, testMetaDst},
				{testDataLeg, testDataSide, testDataDst},
			} {
				text := fmt.Sprintf("subsystem %s is unknown: ",
					legNqn(srv, lp.leg))
				detail := ch.converged[lp.src].GetDetails()
				if !strings.HasPrefix(detail, text) {
					t.Fatalf("leg %#x's path row reads %q, want it to "+
						"start %q", lp.leg, detail, text)
				}
				want[lp.src] = wantPath{srcPathName,
					pb.ResStatus_RES_STATUS_ERROR, detail}
				want[lp.dst] = wantPath{dstPathName,
					pb.ResStatus_RES_STATUS_ERROR, detail}
			}
			ch.assert(t, "walk unanswered", want)
		})
		t.Run("an address, "+role.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: role.primary,
				extraSide: true}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			src := ctrlNameOf(node, nqn, testIp, testSvcId)
			node.mu.Lock()
			node.killReadAlways[sysfsNvmeCtrlDir+"/"+src+"/address"] = true
			node.mu.Unlock()
			opts.revision = 3
			ch := readPathChannels(t, srv, opts)
			want := allPathsOk()
			detail := ch.converged[testDataSide].GetDetails()
			if !strings.HasPrefix(detail,
				"controller "+src+" is unknown: ") {
				t.Fatalf("the src path row reads %q, want the unknown "+
					"controller %s named", detail, src)
			}
			want[testDataSide] = wantPath{srcPathName,
				pb.ResStatus_RES_STATUS_ERROR, detail}
			ch.assert(t, "src address unanswered", want)
		})
	}
}

// TestPathRowsTakeOneLookupPerLeg pins what the rows cost. In a cntlr's
// read, which a check round answers from, a primary looks up a two-sided leg
// alone — its entry's directory is listed once, a one-sided leg's not at
// all, as its leg row reads the prober — and a standby's leg row reads the
// lookup its path rows took, so each leg's entry is listed once there too. A
// converge's rows read the view its connect step took, so its lookup of each
// leg is the connect step's alone.
func TestPathRowsTakeOneLookupPerLeg(t *testing.T) {
	for _, tc := range []struct {
		role    string
		primary bool
		// oneSided is how often a read lists the one-sided leg's entry.
		oneSided int
	}{
		{"standby", false, 1},
		{"primary", true, 0},
	} {
		t.Run(tc.role, func(t *testing.T) {
			srv, node := newTestServer(t)
			opts := reqOpts{revision: 2, primary: tc.primary,
				extraSide: true, twoLegs: true, raid1: tc.primary}
			syncupBoth(t, srv, opts)
			entryOf := func(legId uint64) string {
				node.mu.Lock()
				defer node.mu.Unlock()
				subsys := node.subsystems[legNqn(srv, legId)]
				if subsys == nil {
					t.Fatalf("leg %#x is not connected", legId)
				}
				return fmt.Sprintf("list %s/nvme-subsys%d",
					sysfsNvmeSubsysDir, subsys.idx)
			}
			node.Reset()
			info := checkRound(t, srv, opts.revision)
			if got := len(info.GetSideIdToPath()); got != 4 {
				t.Fatalf("the check round reports %d path rows, want 4",
					got)
			}
			for _, leg := range []struct {
				legId uint64
				want  int
			}{
				{testMetaLeg, 1},
				{testDataLeg, 1},
				{testDataLeg2, tc.oneSided},
			} {
				if got := countCalls(node, entryOf(leg.legId)); got !=
					leg.want {
					t.Fatalf("the check round listed leg %#x's entry %d "+
						"times, want %d", leg.legId, got, leg.want)
				}
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := len(reply.GetCntlrInfo().GetSideIdToPath()); got != 4 {
				t.Fatalf("the converge reports %d path rows, want 4", got)
			}
			for _, legId := range []uint64{
				testMetaLeg, testDataLeg, testDataLeg2} {
				if got := countCalls(node, entryOf(legId)); got != 1 {
					t.Fatalf("the converge listed leg %#x's entry %d "+
						"times, want 1: its connect step's lookup",
						legId, got)
				}
			}
		})
	}
}

// TestPathRowKeysFollowTheSides pins the rows' tracker histories (SH14): the
// keys a plan wants hold one per side of each two-sided leg, so a side that
// leaves its leg — a finished or cancelled migration — leaves its history
// too, and a later row of the same side reports a fresh epoch.
func TestPathRowKeysFollowTheSides(t *testing.T) {
	pathKeys := func(opts reqOpts) []string {
		var keys []string
		for key := range cntlrResKeys(newCntlrPlan(common.NewNameFmt(
			common.DefaultLocalStorPrefix), cntlrReq(opts))) {
			if strings.HasPrefix(key, "path/") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		return keys
	}
	var want []string
	for _, sideId := range []uint64{
		testMetaSide, testDataSide, testMetaDst, testDataDst} {
		want = append(want, resKeyOf(resKeyPathFmt, sideId))
	}
	sort.Strings(want)
	if got := pathKeys(reqOpts{revision: 2, extraSide: true,
		twoLegs: true}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a plan with two two-sided legs wants path keys %q, "+
			"want %q", got, want)
	}
	if got := pathKeys(reqOpts{revision: 2, twoLegs: true}); len(got) != 0 {
		t.Fatalf("a plan with one-sided legs wants path keys %q", got)
	}
}
