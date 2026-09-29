package dnagent

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	testCluster    = uint64(0xebada5168620c5fe)
	testDn         = uint64(3)
	testSp         = uint64(0x11)
	testLeg        = uint64(0x15)
	testSide       = uint64(0x16)
	testSide2      = uint64(0x17)
	testCn0        = uint64(5)
	testCn1        = uint64(6)
	testDisk       = "/dev/fake-disk"
	testExtentSize = uint64(1 << 20)
	// testExtCnt is deliberately larger than common.DnZeroBatchExtCnt (10), so
	// every side in this package provisions in three §9.4 batches — 10, 10, 5.
	// A fixture equal to the batch size would zero the whole side in one
	// command and hide every multi-batch offset bug.
	testExtCnt    = uint64(25)
	testBlockSize = uint64(1 << 20)
	testMigrId    = uint64(0x21)
	testSrcDn     = uint64(4)
)

func testTrConf() *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "192.168.0.20",
		TrSvcId: "4200",
	}
}

func newTestServer(t *testing.T) (*DnAgentServer, *fakeNode) {
	t.Helper()
	return newTestServerOnPort(t, common.NvmetPortId)
}

// newTestServerOnPort is newTestServer with an explicit --nvmet-port-id.
// Every test but TestSyncupOnNonDefaultPort takes the default, which is what
// keeps the recorded call paths at ports/1.
func newTestServerOnPort(
	t *testing.T,
	portId int,
) (*DnAgentServer, *fakeNode) {
	t.Helper()
	node := newFakeNode()
	// Must exceed DnDataOffset: at testExtentSize (1 MiB) that leaves 256
	// data extents.
	node.devSize[testDisk] = 512 << 20
	node.devNo[testDisk] = "253:0"
	node.dirs[agent.NvmetRoot] = true

	srv := startTestServerOnPort(t, node, portId)
	node.Reset()
	return srv, node
}

// startTestServer builds a dn server over an existing fake node and reconciles
// it, the way cmd/dnv-agent does. rootCtx is cancellable and joined at test
// end, so the §9.4 zeroing goroutines can never outlive their test and mutate
// the fake under a later assertion — the unit-test shape of agent.Serve's
// cancel-then-WaitBackground shutdown.
func startTestServer(t *testing.T, node *fakeNode) *DnAgentServer {
	t.Helper()
	return startTestServerOnPort(t, node, common.NvmetPortId)
}

func startTestServerOnPort(
	t *testing.T,
	node *fakeNode,
	portId int,
) *DnAgentServer {
	t.Helper()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf(), portId)
	// The §11.2 cutover grace window is off unless a test asks for it: no
	// unit test can wait common.SuspendSeconds, and with it off a migration
	// source fences straight onto its dm-errors, which is the end state
	// every other test cares about. TestMigrationSourceFence covers the
	// window itself, and TestFenceWindowDefault pins the production value.
	srv.fenceWait = 0
	// Likewise for the §9.4 retry pace: DnZeroRetryInterval is 5 s, and
	// TestZeroRetryIntervalDefault pins the production value.
	srv.zeroRetryInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		srv.WaitBackground()
	})
	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return srv
}

// portPathOf is the configfs directory of one nvmet port, formatted the way
// agent.Nvmet.PortPath does. Expectations go through it rather than through a
// literal "ports/1" so the default is expressed as common.NvmetPortId and a
// non-default --nvmet-port-id can be asserted with the same strings.
func portPathOf(portId int) string {
	return fmt.Sprintf("%s/ports/%d", agent.NvmetRoot, portId)
}

// mentionsPort reports whether one recorded call names portPath. The path
// has to end where the call's next separator begins, which for this fake is
// one of: end of string, "/" (a child of the port), "=" (a write/writedirect
// of a port attribute) or " " (the next argument). Matching on portPath+"/"
// alone is not enough — it misses the two shapes a half-converted call site
// produces most often, `cmd ls -1 {portPath}` (every dirExists probe, so
// every ProbePort) and `cmd mkdir -p {portPath}` (EnsurePort's own mkdir).
// The separator test is what keeps ports/1 from matching ports/11.
func mentionsPort(call, portPath string) bool {
	rest := call
	for {
		i := strings.Index(rest, portPath)
		if i < 0 {
			return false
		}
		rest = rest[i+len(portPath):]
		if rest == "" || strings.HasPrefix(rest, "/") ||
			strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, " ") {
			return true
		}
	}
}

// assertDefaultPortUntouched is the negative half of
// TestSyncupOnNonDefaultPort: an agent on --nvmet-port-id 7 must neither
// create ports/1 nor name it in any call. It reports with t.Errorf and is
// called once per syncup round, because node.Reset() throws the previous
// round's calls away.
func assertDefaultPortUntouched(t *testing.T, node *fakeNode) {
	t.Helper()
	dflt := portPathOf(common.NvmetPortId)
	if node.dirs[dflt] {
		t.Errorf("%s was created by an agent on another port", dflt)
	}
	for _, call := range node.Calls() {
		if mentionsPort(call, dflt) {
			t.Errorf("a call touched the default port: %s", call)
		}
	}
}

func sidePtr(sideId uint64) *pb.SidePointer {
	return &pb.SidePointer{SpId: testSp, LegId: testLeg, SideId: sideId}
}

func dnReq(revision uint64, sideIds ...uint64) *pb.SyncupDnRequest {
	ptrs := make([]*pb.SidePointer, 0, len(sideIds))
	for _, sideId := range sideIds {
		ptrs = append(ptrs, sidePtr(sideId))
	}
	return &pb.SyncupDnRequest{
		ClusterId:       testCluster,
		DnId:            testDn,
		Revision:        revision,
		SidePointerList: ptrs,
		ExtentSize:      testExtentSize,
	}
}

// sideReq is a *steady-state* request: side_conf.provisioned is true, which is
// what the sp-worker sets once the side has finished zeroing (the §10.3
// flip rule). Almost every test wants that shape, so a side is brought
// there through syncupSideTwoPhase / syncupBoth rather than by hand.
func sideReq(
	revision uint64,
	sideId uint64,
	primary uint64,
	standbys []uint64,
	level pb.SpLevel,
) *pb.SyncupSideRequest {
	return &pb.SyncupSideRequest{
		ClusterId:   testCluster,
		DnId:        testDn,
		SidePointer: sidePtr(sideId),
		Revision:    revision,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        testExtCnt,
			CntlidSlot:    1,
			PrimaryCnId:   primary,
			StandbyIdList: standbys,
			SpLevel:       level,
			Provisioned:   true,
		},
	}
}

// unprovisionedSideReq is phase 1 of the [D15] flow: a freshly created side whose
// flag the CP has not flipped yet. The agent allocates its extents, builds the
// aggregate dm-linear and zeroes it — and exports nothing.
func unprovisionedSideReq(
	revision uint64,
	sideId uint64,
	primary uint64,
	standbys []uint64,
	level pb.SpLevel,
) *pb.SyncupSideRequest {
	req := sideReq(revision, sideId, primary, standbys, level)
	req.SideConf.Provisioned = false
	return req
}

// syncupSideTwoPhase plays the sp-worker's provisioning flip around one
// request: sync it once with provisioned = false so the side is allocated and
// zeroed, wait for the background goroutine to finish, then sync the request
// as given. Both phases carry the same revision, which SH8 accepts.
func syncupSideTwoPhase(
	t *testing.T,
	srv *DnAgentServer,
	req *pb.SyncupSideRequest,
) *pb.SyncupSideReply {
	t.Helper()
	ctx := context.Background()
	sideId := req.GetSidePointer().GetSideId()
	// The flip is one-way: a side that has already provisioned is never sent
	// back through phase 1, which would retract its live exports.
	rec, ok, err := srv.meta.LookupSide(ctx, testSp, sideId)
	if err != nil {
		t.Fatalf("LookupSide: %v", err)
	}
	if !ok || !sideFullyZeroed(rec) {
		phase1 := proto.Clone(req).(*pb.SyncupSideRequest)
		phase1.SideConf.Provisioned = false
		if _, err := srv.SyncupSide(ctx, phase1); err != nil {
			t.Fatalf("SyncupSide (provisioning): %v", err)
		}
		waitZeroed(t, srv, sideId)
	}
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupSide rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// provisionSide runs the §9.4 phase-1 flow for a side that does not exist yet:
// the CP has not flipped its provisioned flag, so the agent allocates the
// side's extents, builds its aggregate dm-linear and zeroes it — and builds
// nothing above it. A migration destination provisions exactly this way before
// its migr_dst_conf takes effect (§11.2).
func provisionSide(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
	sideId uint64,
) {
	t.Helper()
	if _, err := srv.SyncupSide(context.Background(), unprovisionedSideReq(
		revision, sideId, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide (provisioning): %v", err)
	}
	waitZeroed(t, srv, sideId)
}

// waitZeroed blocks until the side's record says every logical extent is
// zeroed — the unit-test twin of the integ suites' `wait-zeroed` helper.
func waitZeroed(t *testing.T, srv *DnAgentServer, sideId uint64) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok, err := srv.meta.LookupSide(ctx, testSp, sideId)
		if err != nil {
			t.Fatalf("LookupSide: %v", err)
		}
		if ok && sideFullyZeroed(rec) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("side %d never finished zeroing", sideId)
		}
		time.Sleep(time.Millisecond)
	}
}

// syncupBoth brings a DN and one side to a converged, exporting state. It
// returns the SyncupDn reply, which most callers ignore; the ones that do not
// are asserting the dn-side ResInfo rows that only this reply carries
// (SyncupDn's port_info comes from ensurePort, GetDnInfo's from probeDn —
// two different sites).
func syncupBoth(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
	sideId uint64,
) *pb.SyncupDnReply {
	t.Helper()
	dnReply, err := srv.SyncupDn(context.Background(), dnReq(revision, sideId))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if dnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn rejected: %v", dnReply.GetAgentReply())
	}
	syncupSideTwoPhase(t, srv, sideReq(revision, sideId, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	return dnReply
}

// assertOrder matches wanted as a *subsequence* of the recorded calls: each
// entry is found at or after the position of the previous one, so a sequence
// may legitimately name the same call shape twice (the two volume-table slot
// writes bracketing a zeroing batch, say).
func assertOrder(t *testing.T, node *fakeNode, wanted ...string) {
	t.Helper()
	previous := -1
	for _, want := range wanted {
		idx := node.indexOfCallFrom(want, previous+1)
		if idx < 0 {
			t.Fatalf("missing call %q after index %d in:\n%s",
				want, previous, strings.Join(node.Calls(), "\n"))
		}
		previous = idx
	}
}

// ---------------------------------------------------------------------------
// 1. Fresh SyncupDn (DN5)
// ---------------------------------------------------------------------------

func TestFreshSyncupDn(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	portPath := portPathOf(common.NvmetPortId)

	reply, err := srv.SyncupDn(context.Background(), dnReq(1))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	if reply.GetRevision() != 1 {
		t.Fatalf("revision = %d, want 1", reply.GetRevision())
	}

	assertOrder(t, node,
		// The dn file carries the side pointer list the node-level
		// sweep removes against, so it is persisted BEFORE anything is
		// converged or swept — a crash in the middle is then a startup sweep
		// rather than a rebuild against the old list.
		"writeproto "+nf.LocalDnPath(testCluster, testDn),
		fmt.Sprintf("readblock %s off=%d len=%d",
			testDisk, common.DnHeaderOffset, common.DnHeaderSize),
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotAOffset),
		fmt.Sprintf("writeblock %s off=%d", testDisk, common.DnHeaderOffset),
		"cmd mkdir -p "+portPath,
		"writedirect "+portPath+"/addr_trtype=tcp",
		"writedirect "+portPath+"/addr_traddr=192.168.0.20",
		"cmd mkdir -p "+portPath+"/ana_groups/2",
		"cmd mkdir -p "+portPath+"/ana_groups/3",
		"writedirect "+portPath+"/ana_groups/1/ana_state=optimized",
		"writedirect "+portPath+"/ana_groups/2/ana_state=non-optimized",
		"writedirect "+portPath+"/ana_groups/3/ana_state=inaccessible",
	)
	// LVM is gone from the dn agent entirely ([D13]).
	for _, call := range node.Calls() {
		for _, banned := range []string{
			"pvcreate", "vgcreate", "lvcreate", "lvremove", "lvchange",
			"cmd lvs ", "cmd vgs ", "cmd pvs ",
		} {
			if strings.Contains(call, banned) {
				t.Errorf("an LVM command survived: %s", call)
			}
		}
	}

	for _, field := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"disk", reply.GetDnInfo().GetDiskInfo()},
		{"meta", reply.GetDnInfo().GetMetaInfo()},
		{"port", reply.GetDnInfo().GetPortInfo()},
	} {
		if field.info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("%s: status %v, details %q",
				field.name, field.info.GetStatus(), field.info.GetDetails())
		}
	}
	if got := reply.GetDnInfo().GetMetaInfo().GetResName(); got != testDisk {
		t.Errorf("meta res_name = %q, want %q", got, testDisk)
	}
	if got := reply.GetDnInfo().GetMetaInfo().GetDetails(); !strings.HasPrefix(
		got, "seq=1 sides=0") {
		t.Errorf("meta details = %q", got)
	}
	// port_info names the port this agent converges, which with no
	// --nvmet-port-id is common.NvmetPortId.
	want := fmt.Sprintf("%d", common.NvmetPortId)
	if got := reply.GetDnInfo().GetPortInfo().GetResName(); got != want {
		t.Errorf("port res_name = %q, want %q", got, want)
	}
}

// TestSyncupOnNonDefaultPort pins CM2's --nvmet-port-id (use_32_slices §5):
// a dn server built with port id 7 creates ports/7, writes its ANA states
// there, links the side subsystem into ports/7, reports "7" as port_info's
// res_name on both the syncup and the probe path, and unlinks from ports/7
// on teardown — and never touches the default ports/1. Without it many dn
// agents could not share one node's kernel.
//
// The assertions are on the objects, not only on the recorded strings: the
// port link is read back out of the fake's symlink table before and after the
// teardown, and the side's nvmeof row is OK only because probeExport's
// PortLinked found the link under the same port the converge made it under.
func TestSyncupOnNonDefaultPort(t *testing.T) {
	const portId = 7
	if portId == common.NvmetPortId {
		t.Fatalf("this test needs a port id other than the default %d",
			common.NvmetPortId)
	}
	srv, node := newTestServerOnPort(t, portId)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	portPath := portPathOf(portId)

	syncupReply := syncupBoth(t, srv, 1, testSide)

	// Ahead of assertOrder, which is fatal: a regression that converges the
	// default port also drops the ports/7 calls assertOrder asks for, so with
	// the two the other way round this guard would never get to report.
	assertDefaultPortUntouched(t, node)

	assertOrder(t, node,
		"cmd mkdir -p "+portPath,
		"writedirect "+portPath+"/addr_trtype=tcp",
		"cmd mkdir -p "+portPath+"/ana_groups/2",
		"cmd mkdir -p "+portPath+"/ana_groups/3",
		"writedirect "+portPath+"/ana_groups/1/ana_state=optimized",
		"writedirect "+portPath+"/ana_groups/3/ana_state=inaccessible",
	)

	// SyncupDn's own reply names the port. This is ensurePort's res_name,
	// which is a different site from probeDn's below.
	syncupPort := syncupReply.GetDnInfo().GetPortInfo()
	if got := syncupPort.GetStatus(); got != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("SyncupDn port status = %v, details %q",
			got, syncupPort.GetDetails())
	}
	if got := syncupPort.GetResName(); got != "7" {
		t.Errorf("SyncupDn port res_name = %q, want \"7\"", got)
	}

	// The subsystem's port link is an object in the fake's symlink table,
	// not just a recorded command.
	nqns := make([]string, 0, 2)
	for _, cnId := range []uint64{testCn0, testCn1} {
		nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		nqns = append(nqns, nqn)
		link := portPath + "/subsystems/" + nqn
		target, ok := node.links[link]
		if !ok {
			t.Errorf("no port link at %s; links: %v", link, node.links)
			continue
		}
		if want := agent.NvmetRoot + "/subsystems/" + nqn; target != want {
			t.Errorf("link %s -> %q, want %q", link, target, want)
		}
	}

	// Probing reads the same port back: port_info names it, and the side's
	// nvmeof rows are OK, which PortLinked can only report from ports/7.
	dnReply, err := srv.GetDnInfo(context.Background(),
		&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := dnReply.GetDnInfo().GetPortInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("port status = %v, details %q", got,
			dnReply.GetDnInfo().GetPortInfo().GetDetails())
	}
	if got := dnReply.GetDnInfo().GetPortInfo().GetResName(); got != "7" {
		t.Errorf("port res_name = %q, want \"7\"", got)
	}
	sideReply, err := srv.GetSideInfo(context.Background(),
		&pb.GetSideInfoRequest{
			ClusterId:   testCluster,
			DnId:        testDn,
			SidePointer: sidePtr(testSide),
		})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	rows := sideReply.GetSideInfo().GetCnIdToNvmeof()
	if len(rows) != 2 {
		t.Fatalf("%d nvmeof rows, want 2: %v", len(rows), rows)
	}
	for cnId, info := range rows {
		if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("cn %d nvmeof: %v %q",
				cnId, info.GetStatus(), info.GetDetails())
		}
	}

	// Teardown unlinks from the same port. RemovePortLink asks PortLinked
	// about the id it was given and returns nil when the answer is "no", so
	// an agent that tore down against the default would report success,
	// delete the kernel-global subsystem and leave its ports/7 link behind.
	// Nothing but the symlink table can tell the two apart.
	node.Reset()
	if _, err := srv.SyncupDn(context.Background(), dnReq(2)); err != nil {
		t.Fatalf("SyncupDn teardown: %v", err)
	}
	assertDefaultPortUntouched(t, node)
	for _, nqn := range nqns {
		link := portPath + "/subsystems/" + nqn
		if target, ok := node.links[link]; ok {
			t.Errorf("the port link %s -> %q survived teardown", link, target)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Revision gate (SH8)
// ---------------------------------------------------------------------------

func TestRevisionGate(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()

	if _, err := srv.SyncupDn(ctx, dnReq(5)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(4))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeStaleRevision {
		t.Fatalf("code = %d, want ReplyCodeStaleRevision",
			reply.GetAgentReply().GetCode())
	}
	if reply.GetRevision() != 5 {
		t.Errorf("revision = %d, want the stored 5", reply.GetRevision())
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("stale revision issued mutations: %v", got)
	}

	node.Reset()
	if _, err := srv.SyncupDn(ctx, dnReq(5)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if !node.hasCall("writeproto") {
		t.Error("equal revision did not re-persist")
	}
	for _, call := range node.Mutations() {
		if !strings.HasPrefix(call, "writeproto") {
			t.Errorf("equal revision mutated a converged dn: %s", call)
		}
	}

	node.Reset()
	reply, err = srv.SyncupDn(ctx, dnReq(6))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetRevision() != 6 {
		t.Errorf("revision = %d, want 6", reply.GetRevision())
	}
}

// ---------------------------------------------------------------------------
// 3. Probe-first idempotency (SH16)
// ---------------------------------------------------------------------------

func TestSyncupSideIdempotent(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	reply, err := srv.SyncupSide(context.Background(),
		sideReq(1, testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is re-persisted
		}
		t.Errorf("converged side issued a mutating call: %s", call)
	}
	if reply.GetSideInfo().GetSideDevInfo().GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev status = %v",
			reply.GetSideInfo().GetSideDevInfo().GetStatus())
	}
	for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
		if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("cn %d nvmeof: %v %q",
				cnId, info.GetStatus(), info.GetDetails())
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Pointer diff (DN6, DN8)
// ---------------------------------------------------------------------------

func TestUnknownSidePointerRejected(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	reply, err := srv.SyncupSide(ctx,
		sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code = %d, want ReplyCodeUnknownObject",
			reply.GetAgentReply().GetCode())
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("unknown pointer issued mutations: %v", got)
	}
}

func TestSyncupDnTearsDownRemovedSide(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	if _, err := srv.SyncupDn(context.Background(), dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The side is FORGOTTEN first — its file and chunks go at pointer
	// removal — and the sweep then finds its resources by name, top-down:
	// nvmet exports, the per-CN dm devices, the side device, its allocation
	// record. The teardown this replaced ran in the opposite order and
	// deleted the file whether or not the removals worked.
	assertOrder(t, node,
		"cmd rm -f "+
			nf.LocalSidePath(testCluster, testDn, testSp, testSide),
		"cmd rmdir "+agent.NvmetRoot+"/subsystems/"+
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
		"cmd dmsetup remove "+
			nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
		"cmd dmsetup remove "+
			nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0),
		"cmd dmsetup remove "+
			nf.DnSideName(testCluster, testDn, testSp, testSide),
		fmt.Sprintf("writeblock %s off=", testDisk),
	)
	if _, ok, _ := srv.meta.LookupSide(
		context.Background(), testSp, testSide); ok {
		t.Error("the allocation record survived teardown")
	}
	if srv.getSide(sideKey(testCluster, testDn, testSp, testSide)) != nil {
		t.Error("side state survived teardown")
	}
	if _, ok := node.protos[nf.LocalSidePath(
		testCluster, testDn, testSp, testSide)]; ok {
		t.Error("side state file survived teardown")
	}
}

// ---------------------------------------------------------------------------
// 5. Side provisioning protocol (§9.4, DN9)
// ---------------------------------------------------------------------------

func TestSideProvisioningProtocol(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	sideDevPath := nf.DmPath(sideDevName)

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	// Phase 1 of the [D15] flow: the CP has not flipped the flag yet.
	reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The RPC itself only allocates and builds: the record is persisted with
	// zeroed_bits all 0 (slot B — the format wrote slot A) and the aggregate
	// device is created. The zeroing is a background goroutine, so the reply
	// says PROVISIONING and nothing above the side device exists.
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotBOffset),
		"cmd dmsetup create "+sideDevName,
	)
	devInfo := reply.GetSideInfo().GetSideDevInfo()
	if devInfo.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Errorf("side_dev on the first pass = %v/%q, want PROVISIONING",
			devInfo.GetStatus(), devInfo.GetDetails())
	}
	for cnId, cnInfo := range reply.GetSideInfo().GetCnIdToDmLinear() {
		if cnInfo.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			cnInfo.GetDetails() != tagProvisioningWait {
			t.Errorf("cn %d dm_linear = %v/%q, want PROVISIONING/%q", cnId,
				cnInfo.GetStatus(), cnInfo.GetDetails(), tagProvisioningWait)
		}
	}

	// The device is a single run of testExtCnt extents at the data area.
	wantOffset := common.DnDataOffset / agent.SectorSize
	wantLen := testExtCnt * testExtentSize / agent.SectorSize
	wantTable := fmt.Sprintf("stdin=0 %d linear 253:0 %d",
		wantLen, wantOffset)
	if !node.hasCall("cmd dmsetup create " + sideDevName + " " + wantTable) {
		t.Errorf("side device table is not %q; calls:\n%s",
			wantTable, strings.Join(node.Calls(), "\n"))
	}

	waitZeroed(t, srv, testSide)
	// One command per batch, in order, at exactly the byte ranges the batch
	// cursor names: testExtCnt = 25 extents in DnZeroBatchExtCnt = 10 sized
	// batches is 10, 10 and 5.
	wantBatches := zerooutBatches(sideDevPath, testExtCnt)
	assertOrder(t, node, wantBatches...)
	if got := node.dms[sideDevName].zeroouts; len(got) != len(wantBatches) {
		t.Errorf("side zeroouts = %v, want %d batches", got, len(wantBatches))
	}
	// Zeroing is never recorded as a hydration-marking discard.
	if got := node.dms[sideDevName].discards; len(got) != 0 {
		t.Errorf("the side device took hydration discards: %v", got)
	}
	// Each batch persists its own bits: three zeroouts, three slot writes.
	for i, batch := range wantBatches {
		zeroIdx := node.indexOfCall(batch)
		if node.indexOfCallFrom(
			fmt.Sprintf("writeblock %s off=", testDisk), zeroIdx+1) < 0 {
			t.Errorf("batch %d did not persist its bits", i)
		}
	}
	// Nothing was exported while the side was provisioning.
	if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("a provisioning side was exported")
	}

	// Phase 2: the worker flips the flag and the exports converge.
	node.Reset()
	reply, err = srv.SyncupSide(ctx,
		sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("side_dev after the flip = %v/%q",
			got.GetStatus(), got.GetDetails())
	}
	if !node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("the flip did not export the side")
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "blkdiscard") {
			t.Errorf("a zeroed side was re-zeroed: %s", call)
		}
	}
	sideInfo := reply.GetSideInfo()
	if sideInfo.GetZeroedExtCnt() != testExtCnt ||
		sideInfo.GetTotalExtCnt() != testExtCnt {
		t.Errorf("zeroed/total = %d/%d, want %d/%d",
			sideInfo.GetZeroedExtCnt(), sideInfo.GetTotalExtCnt(),
			testExtCnt, testExtCnt)
	}
}

// zerooutBatches is the exact command line of every §9.4 batch a side of
// extCnt extents produces, in order.
func zerooutBatches(sideDevPath string, extCnt uint64) []string {
	var out []string
	for from := uint64(0); from < extCnt; from += common.DnZeroBatchExtCnt {
		count := extCnt - from
		if count > common.DnZeroBatchExtCnt {
			count = common.DnZeroBatchExtCnt
		}
		out = append(out, fmt.Sprintf(
			"cmd blkdiscard --zeroout --offset %d --length %d %s",
			from*testExtentSize, count*testExtentSize, sideDevPath))
	}
	return out
}

// The §9.4 converge matrix, one sub-test per row.
func TestSideProvisioningMatrix(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	ctx := context.Background()

	// Row 1: provisioned = false, no record. Allocate, build, start zeroing.
	t.Run("unprovisioned/absent", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
			t.Fatalf("side_dev = %v/%q, want PROVISIONING",
				got.GetStatus(), got.GetDetails())
		}
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
			t.Error("row 1 did not allocate")
		}
		if _, ok := node.dms[sideDevName]; !ok {
			t.Error("row 1 did not build the side device")
		}
		waitZeroed(t, srv, testSide)
	})

	// Row 2: provisioned = false, partial bits. Keep converging the linear and
	// keep the goroutine; report the progress string with a real k.
	t.Run("unprovisioned/partial", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		// Let exactly the first batch land and park the second one *inside*
		// its child. A failing batch would freeze the record too, but it would
		// also publish a zeroErr — ERROR wins over the progress string while
		// one is outstanding (DN9) — and a batch merely paced by
		// zeroRetryInterval keeps moving, so k would be racy and only its
		// prefix assertable. Blocking is what makes the whole formatted
		// string exact.
		batch1 := "cmd blkdiscard --zeroout --offset " + strconv.FormatUint(
			common.DnZeroBatchExtCnt*testExtentSize, 10)
		node.blockCmd(batch1)
		t.Cleanup(func() { node.releaseCmd(batch1) })
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !waitFor(t, 5*time.Second, func() bool {
			rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
			return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
		}) {
			t.Fatal("the first batch never landed")
		}
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		// k/n in extents, k first: an operator reading "zeroing 25/10" would
		// be told a side that has zeroed nothing is nearly done.
		wantDetails := fmt.Sprintf(zeroingDetailsFmt,
			common.DnZeroBatchExtCnt, testExtCnt)
		if got.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			got.GetDetails() != wantDetails {
			t.Errorf("side_dev = %v/%q, want PROVISIONING/%q",
				got.GetStatus(), got.GetDetails(), wantDetails)
		}
		if reply.GetSideInfo().GetZeroedExtCnt() !=
			common.DnZeroBatchExtCnt {
			t.Errorf("zeroed_ext_cnt = %d, want %d",
				reply.GetSideInfo().GetZeroedExtCnt(),
				uint64(common.DnZeroBatchExtCnt))
		}
		if reply.GetSideInfo().GetTotalExtCnt() != testExtCnt {
			t.Errorf("total_ext_cnt = %d, want %d",
				reply.GetSideInfo().GetTotalExtCnt(), testExtCnt)
		}
		// The parked batch finishes the side once its child returns.
		node.releaseCmd(batch1)
		waitZeroed(t, srv, testSide)
	})

	// Row 3: provisioned = false, complete bits. The linear is ensured, the
	// goroutine is gone, and there are still no exports.
	t.Run("unprovisioned/complete", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		waitZeroed(t, srv, testSide)
		node.Reset()
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("side_dev = %v/%q, want OK",
				got.GetStatus(), got.GetDetails())
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		srv.mu.Lock()
		zeroing := st.zeroing
		srv.mu.Unlock()
		if zeroing {
			t.Error("a fully zeroed side kept its zeroing goroutine")
		}
		if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
			t.Error("an unflipped side was exported")
		}
		for cnId, cnInfo := range reply.GetSideInfo().GetCnIdToNvmeof() {
			if cnInfo.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
				t.Errorf("cn %d nvmeof = %v, want PROVISIONING",
					cnId, cnInfo.GetStatus())
			}
		}
	})

	// Row 4 is the steady state every other test in this package runs in, so
	// it only needs the export assertion.
	t.Run("provisioned/complete", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		if !node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
			t.Error("row 4 did not export the side")
		}
	})

	// Row 5: provisioned = true over incomplete bits. The agent trusts its own
	// bits over the flag: ERROR, no exports, and the goroutine keeps running
	// so the side self-heals.
	t.Run("provisioned/partial", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		node.mu.Lock()
		node.failCmdAlways["blkdiscard --zeroout"] = "zeroout refused"
		node.mu.Unlock()
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != tagNotZeroed {
			t.Errorf("side_dev = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), tagNotZeroed)
		}
		if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
			t.Error("a partially zeroed side was exported")
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		srv.mu.Lock()
		zeroing := st.zeroing
		srv.mu.Unlock()
		if !zeroing {
			t.Error("row 5 dropped the self-healing goroutine")
		}
		// And it does self-heal once the command works again.
		node.mu.Lock()
		delete(node.failCmdAlways, "blkdiscard --zeroout")
		node.mu.Unlock()
		waitZeroed(t, srv, testSide)
	})

	// Row 6: provisioned = true, no record. The data is gone; re-allocating
	// would present a zeroed impostor as the data-bearing leg.
	t.Run("provisioned/absent", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != tagRecordMissing {
			t.Errorf("side_dev = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), tagRecordMissing)
		}
		// No allocation write, and no side dm-linear.
		for _, call := range node.Calls() {
			if strings.HasPrefix(call, "writeblock") {
				t.Errorf("row 6 allocated: %s", call)
			}
		}
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
			t.Error("row 6 created an allocation record")
		}
		if _, ok := node.dms[sideDevName]; ok {
			t.Error("row 6 built the side device")
		}
		// total_ext_cnt is never omitted: with no record it comes from the
		// request (DN9).
		if reply.GetSideInfo().GetTotalExtCnt() != testExtCnt {
			t.Errorf("total_ext_cnt = %d, want %d",
				reply.GetSideInfo().GetTotalExtCnt(), testExtCnt)
		}
	})

	// A read-only probe never allocates, so "record absent at
	// provisioned = false" is MISSING with empty details, not the matrix's
	// "zeroing 0/n" (DN9).
	t.Run("probe/unprovisioned/absent", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		waitZeroed(t, srv, testSide)
		clearRecord(t, srv, node)
		node.Reset()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
			got.GetDetails() != "" {
			t.Errorf("side_dev = %v/%q, want MISSING/\"\"",
				got.GetStatus(), got.GetDetails())
		}
		for _, call := range node.Mutations() {
			t.Errorf("a probe mutated the node: %s", call)
		}
	})

	// A probe over a re-allocated (all-not-zeroed) record at
	// provisioned = true reports the same hard error the converge does — and
	// still starts nothing.
	t.Run("probe/provisioned/partial", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		clearZeroed(t, srv, node)
		node.Reset()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != tagNotZeroed {
			t.Errorf("side_dev = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), tagNotZeroed)
		}
		if reply.GetSideInfo().GetZeroedExtCnt() != 0 ||
			reply.GetSideInfo().GetTotalExtCnt() != testExtCnt {
			t.Errorf("zeroed/total = %d/%d, want 0/%d",
				reply.GetSideInfo().GetZeroedExtCnt(),
				reply.GetSideInfo().GetTotalExtCnt(), testExtCnt)
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		srv.mu.Lock()
		zeroing := st.zeroing
		srv.mu.Unlock()
		if zeroing {
			t.Error("a read-only probe started the zeroing goroutine")
		}
		for _, call := range node.Mutations() {
			t.Errorf("a probe mutated the node: %s", call)
		}
	})
}

// A restart resumes zeroing where the last process stopped: the bits are in
// the volume table, and only the extents whose bits are unset are redone.
func TestZeroingResumesAfterRestart(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevPath := nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// Let exactly the first batch through, then stop the process.
	node.mu.Lock()
	node.failCmdAlways["blkdiscard --zeroout --offset "+
		strconv.FormatUint(common.DnZeroBatchExtCnt*testExtentSize, 10)] =
		"crash"
	node.mu.Unlock()
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
		return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
	}) {
		t.Fatal("the first batch never landed")
	}
	stopTestServer(t, srv)

	// A fresh process over the same disk and store.
	node.mu.Lock()
	delete(node.failCmdAlways, "blkdiscard --zeroout --offset "+
		strconv.FormatUint(common.DnZeroBatchExtCnt*testExtentSize, 10))
	node.mu.Unlock()
	node.Reset()
	restarted := startTestServer(t, node)
	waitZeroed(t, restarted, testSide)

	// Batch 0 is never redone; batches 1 and 2 are.
	batches := zerooutBatches(sideDevPath, testExtCnt)
	if node.hasCall(batches[0]) {
		t.Errorf("the restart re-zeroed an already zeroed batch: %s",
			batches[0])
	}
	assertOrder(t, node, batches[1:]...)
}

// The teardown cancels the zeroing goroutine and *waits* for it before the
// side device is removed: the `blkdiscard --zeroout` child holds that device
// open and `dmsetup remove` would fail EBUSY.
//
// The batch is held in flight by a **hard** gate — one the cancel does not
// release — because that is the only shape in which the wait is observable. A
// loop parked in zeroPace's timer, or one whose batch fails instantly, is
// drained by a bare cancel just as fast as by cancel-and-wait, so a test built
// on those passes with the `<-done` deleted and with the stop moved after the
// removal. Here the removal must not appear until the child returns
// (doc/dnagent.md §6 test 19).
func TestZeroingCancelledBeforeDeviceRemoval(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	const zeroout = "cmd blkdiscard --zeroout"
	removeCall := "cmd dmsetup remove " + sideDevName

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The first batch enters its child and stays there until this test says
	// otherwise — the CmdSoftTimeout-to-CmdHardTimeout window of a real one.
	node.blockCmd(zeroout)
	t.Cleanup(func() { node.releaseCmd(zeroout) })
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(zeroout)
	}) {
		t.Fatal("the zeroing loop never ran a batch")
	}

	// The pointer leaves the DN's list: DN6 teardown. It blocks in
	// stopZeroing's wait, so it cannot run on this goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
			t.Errorf("SyncupDn: %v", err)
		}
	}()

	if waitFor(t, 200*time.Millisecond, func() bool {
		return node.hasCall(removeCall)
	}) {
		t.Fatal("the side device was removed while a zeroing batch still " +
			"held it open")
	}
	select {
	case <-done:
		t.Fatal("the teardown finished without waiting for the batch")
	default:
	}

	// The child returns; only now may the removal run.
	node.releaseCmd(zeroout)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the teardown never finished after the batch returned")
	}
	removeIdx := node.indexOfCall(removeCall)
	if removeIdx < 0 {
		t.Fatal("the side device was never removed")
	}
	if node.indexOfCallFrom(zeroout, removeIdx+1) >= 0 {
		t.Error("a zeroing batch ran after the side device was removed")
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if st != nil {
		t.Error("side state survived the teardown")
	}
}

// Every reply's counters come from the RECORD whenever one exists — including
// the paths that then fail (the DN9 ext-count mismatch, a disk the agent may
// not mutate). side_conf.ext_cnt is a gate, never evidence: the disk is
// authoritative ([D13], DN9). Only row 6 — no record at all — falls
// back to the request's number, which the matrix pins separately.
func TestAllocFailureReportsTheRecordsCounters(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	// The CP asks for a bigger side than the record holds: AllocSide refuses
	// (DN9) rather than re-allocating over live data.
	req := sideReq(2, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.ExtCnt = testExtCnt + 5
	node.Reset()
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	got := reply.GetSideInfo().GetSideDevInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "allocated") {
		t.Fatalf("side_dev = %v/%q, want ERROR carrying the DN9 mismatch",
			got.GetStatus(), got.GetDetails())
	}
	if reply.GetSideInfo().GetTotalExtCnt() != testExtCnt ||
		reply.GetSideInfo().GetZeroedExtCnt() != testExtCnt {
		t.Errorf("zeroed/total = %d/%d, want %d/%d — the disk's numbers, not "+
			"the request's",
			reply.GetSideInfo().GetZeroedExtCnt(),
			reply.GetSideInfo().GetTotalExtCnt(), testExtCnt, testExtCnt)
	}
	// The read-only twin already answers from the record, and the two must
	// not contradict each other over identical on-disk state.
	infoReply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if infoReply.GetSideInfo().GetTotalExtCnt() !=
		reply.GetSideInfo().GetTotalExtCnt() ||
		infoReply.GetSideInfo().GetZeroedExtCnt() !=
			reply.GetSideInfo().GetZeroedExtCnt() {
		t.Errorf("probe reports %d/%d, converge %d/%d",
			infoReply.GetSideInfo().GetZeroedExtCnt(),
			infoReply.GetSideInfo().GetTotalExtCnt(),
			reply.GetSideInfo().GetZeroedExtCnt(),
			reply.GetSideInfo().GetTotalExtCnt())
	}
}

// §9.4's DN5 fail-fast: a disk whose write_zeroes_max_bytes reads 0 cannot
// meet the fast-Write-Zeroes assumption, so meta_info is an error that feeds
// err_epoch. An absent attribute is not a verdict.
func TestWriteZeroesFailFast(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	wzPath := "/sys/class/block/fake-disk/queue/write_zeroes_max_bytes"

	// Absent: not a verdict (the fake has no sysfs tree by default).
	reply, err := srv.SyncupDn(ctx, dnReq(1))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetDnInfo().GetMetaInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("absent attribute = %v/%q, want OK",
			got.GetStatus(), got.GetDetails())
	}

	// A present 0 is.
	node.mu.Lock()
	node.files[wzPath] = "0\n"
	node.mu.Unlock()
	reply, err = srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	got := reply.GetDnInfo().GetMetaInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		got.GetDetails() != tagNoWriteZeroes {
		t.Fatalf("meta_info = %v/%q, want ERROR/%q",
			got.GetStatus(), got.GetDetails(), tagNoWriteZeroes)
	}
	// It reports, it does not gate: an already-populated DN keeps converging.
	infoReply, err := srv.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := infoReply.GetDnInfo().GetMetaInfo(); got.GetDetails() !=
		tagNoWriteZeroes {
		t.Errorf("the probe missed the fail-fast: %v/%q",
			got.GetStatus(), got.GetDetails())
	}

	// A non-zero value clears it again.
	node.mu.Lock()
	node.files[wzPath] = "2097152\n"
	node.mu.Unlock()
	reply, err = srv.SyncupDn(ctx, dnReq(3))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetDnInfo().GetMetaInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("fast Write Zeroes = %v/%q, want OK",
			got.GetStatus(), got.GetDetails())
	}
}

// A failed batch publishes the killed command's output on side_dev_info and is
// retried no sooner than zeroRetryInterval later — never a hot loop.
func TestZeroingRetryIsPaced(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	srv.zeroRetryInterval = 200 * time.Millisecond

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	node.failCmdAlways["blkdiscard --zeroout"] = "fake: blkdiscard failed"
	node.mu.Unlock()
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return len(node.callsMatching("cmd blkdiscard --zeroout")) >= 1
	}) {
		t.Fatal("the zeroing loop never ran a batch")
	}
	// Two retry intervals' worth of wall clock must not produce a hot loop.
	time.Sleep(500 * time.Millisecond)
	if got := len(node.callsMatching("cmd blkdiscard --zeroout")); got > 5 {
		t.Errorf("%d attempts in 500ms at a 200ms pace — the retry is a hot "+
			"loop", got)
	}

	// The outstanding failure is what side_dev_info reports (DN9).
	reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		2, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	got := reply.GetSideInfo().GetSideDevInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "blkdiscard") {
		t.Errorf("side_dev = %v/%q, want ERROR carrying the command output",
			got.GetStatus(), got.GetDetails())
	}

	// And the next successful batch clears it back to PROVISIONING. The batch
	// after it parks, so the row is read while the side is still zeroing: a
	// fully zeroed side never consults the zeroing error, and a read then
	// could not tell a cleared error from a stale one.
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	second := zerooutLine(nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide)),
		common.DnZeroBatchExtCnt, common.DnZeroBatchExtCnt)
	node.blockCmd(second)
	t.Cleanup(func() { node.releaseCmd(second) })
	node.mu.Lock()
	delete(node.failCmdAlways, "blkdiscard --zeroout")
	node.mu.Unlock()
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(second)
	}) {
		t.Fatal("the zeroing loop never got past its first batch")
	}
	reply, err = srv.SyncupSide(ctx, unprovisionedSideReq(
		3, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	healed := fmt.Sprintf(zeroingDetailsFmt, common.DnZeroBatchExtCnt,
		testExtCnt)
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_PROVISIONING || got.GetDetails() != healed {
		t.Errorf("side_dev after the heal = %v/%q, want PROVISIONING/%q",
			got.GetStatus(), got.GetDetails(), healed)
	}
	node.releaseCmd(second)
	waitZeroed(t, srv, testSide)
}

// zerooutLine is the recorded command of one §9.4 batch: count extents from
// logical extent from, through the side's dm-linear.
func zerooutLine(sideDevPath string, from, count uint64) string {
	return fmt.Sprintf("cmd blkdiscard --zeroout --offset %d --length %d %s",
		from*testExtentSize, count*testExtentSize, sideDevPath)
}

// assertZeroouts compares every zeroing batch the fake recorded with want —
// how many there were as well as their order and ranges, because DN9's rate
// control changes the number of batches a side takes.
func assertZeroouts(t *testing.T, node *fakeNode, want []string) {
	t.Helper()
	got := node.callsMatching("cmd blkdiscard --zeroout")
	for i := 0; i < len(got) || i < len(want); i++ {
		var gotLine, wantLine string
		if i < len(got) {
			gotLine = got[i]
		}
		if i < len(want) {
			wantLine = want[i]
		}
		if gotLine != wantLine {
			t.Fatalf("zeroing batch %d = %q, want %q; every batch:\n%s",
				i, gotLine, wantLine, strings.Join(got, "\n"))
		}
	}
}

// A batch the soft timeout killed is a rate signal (DN9): the side's next
// batch is half the killed one, the success after it doubles the batch back,
// and DnZeroBatchExtCnt bounds the doubling. One kill does not back the side
// off: while the next batch runs, the side reports the killed command's
// output alone, with no backoff note. A kill after a success starts a new
// streak — it halves again rather than backing off — and a batch the tool
// refused says nothing about the rate and is redone at its own size.
//
// killCmd is the fake's soft-timeout kill: the child is signalled after its
// ioctl ran, so the killed range is zeroed on the node while its bits stay
// unset, and the next batch starts at the same extent. The half batch parks
// inside its child while the side is read, which holds the first kill's
// report still. The side is four full batches long so the bound shows: the
// success after the refused batch leaves the next one at DnZeroBatchExtCnt,
// not twice that.
func TestZeroingBatchHalvesAfterAKill(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevPath := nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))
	const full = uint64(common.DnZeroBatchExtCnt)
	const half = full / 2

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	node.killCmd[zerooutLine(sideDevPath, 0, full)] = true
	node.killCmd[zerooutLine(sideDevPath, half, full)] = true
	node.failCmd[zerooutLine(sideDevPath, full, full)] =
		"blkdiscard: BLKZEROOUT ioctl failed: Input/output error"
	node.mu.Unlock()
	halved := zerooutLine(sideDevPath, 0, half)
	node.blockCmd(halved)
	t.Cleanup(func() { node.releaseCmd(halved) })
	req := unprovisionedSideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.ExtCnt = 4 * full
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(halved)
	}) {
		t.Fatalf("no half batch after the kill; batches:\n%s", strings.Join(
			node.callsMatching("cmd blkdiscard --zeroout"), "\n"))
	}

	// One kill does not back the side off: both reporting paths, the probe
	// and the converge, carry the killed command's output and nothing after
	// it.
	killedOut := strings.TrimPrefix(zerooutLine(sideDevPath, 0, full),
		"cmd ") + ": signal: killed"
	checkOneKill := func(path string, got *pb.ResInfo) {
		t.Helper()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != killedOut {
			t.Errorf("%s side_dev after one kill = %v/%q, want ERROR/%q",
				path, got.GetStatus(), got.GetDetails(), killedOut)
		}
	}
	info, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	checkOneKill("probe", info.GetSideInfo().GetSideDevInfo())
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	checkOneKill("converge", reply.GetSideInfo().GetSideDevInfo())
	node.releaseCmd(halved)
	waitZeroed(t, srv, testSide)

	assertZeroouts(t, node, []string{
		zerooutLine(sideDevPath, 0, full),      // killed
		zerooutLine(sideDevPath, 0, half),      // half the killed batch
		zerooutLine(sideDevPath, half, full),   // doubled back; killed again
		zerooutLine(sideDevPath, half, half),   // halved: a new streak
		zerooutLine(sideDevPath, full, full),   // refused
		zerooutLine(sideDevPath, full, full),   // the refusal halved nothing
		zerooutLine(sideDevPath, 2*full, full), // bounded, not 2 × full
		zerooutLine(sideDevPath, 3*full, full),
	})
}

// DnZeroKillBackoff kills in a row back a side off to one extent per batch —
// straight there, not one more halving — and while that kill is outstanding
// side_dev_info says so after the killed command's output (DN9); the first
// success clears it back to PROVISIONING. From one extent every success
// doubles the batch again, up to DnZeroBatchExtCnt.
func TestZeroingBacksOffAfterRepeatedKills(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevPath := nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The streak: the first batch and each halving of it, every one killed.
	var want []string
	count := uint64(common.DnZeroBatchExtCnt)
	node.mu.Lock()
	for i := 0; i < common.DnZeroKillBackoff; i++ {
		line := zerooutLine(sideDevPath, 0, count)
		node.killCmd[line] = true
		want = append(want, line)
		count /= 2
	}
	node.mu.Unlock()
	// The one-extent batch parks inside its child, which holds the published
	// error still while side_dev_info is read.
	backedOff := zerooutLine(sideDevPath, 0, 1)
	node.blockCmd(backedOff)
	t.Cleanup(func() { node.releaseCmd(backedOff) })
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(backedOff)
	}) {
		t.Fatalf("no one-extent batch after %d kills in a row; batches:\n%s",
			common.DnZeroKillBackoff, strings.Join(
				node.callsMatching("cmd blkdiscard --zeroout"), "\n"))
	}

	// Both reporting paths, the probe and the converge, carry the rate.
	streak := fmt.Sprintf("%d batches killed in a row",
		common.DnZeroKillBackoff)
	checkBackoff := func(path string, got *pb.ResInfo) {
		t.Helper()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			!strings.Contains(got.GetDetails(), "blkdiscard") ||
			!strings.Contains(got.GetDetails(), streak) ||
			!strings.Contains(got.GetDetails(), "1 extent per batch") {
			t.Errorf("%s side_dev = %v/%q, want ERROR carrying the killed "+
				"command, %q and the one-extent rate", path,
				got.GetStatus(), got.GetDetails(), streak)
		}
	}
	infoReq := &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	}
	info, err := srv.GetSideInfo(ctx, infoReq)
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	checkBackoff("probe", info.GetSideInfo().GetSideDevInfo())
	reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	checkBackoff("converge", reply.GetSideInfo().GetSideDevInfo())

	// Released, the one-extent batch succeeds, and that success clears the
	// outstanding kill and its rate with it. The row is read while the batch
	// after it is parked in turn: only a side still zeroing consults the
	// zeroing error at all, so a read after the last batch could not tell a
	// cleared error from a stale one.
	next := zerooutLine(sideDevPath, 1, 2)
	node.blockCmd(next)
	t.Cleanup(func() { node.releaseCmd(next) })
	node.releaseCmd(backedOff)
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(next)
	}) {
		t.Fatalf("no two-extent batch after the one-extent success; "+
			"batches:\n%s", strings.Join(
			node.callsMatching("cmd blkdiscard --zeroout"), "\n"))
	}
	cleared := fmt.Sprintf(zeroingDetailsFmt, 1, testExtCnt)
	checkCleared := func(path string, got *pb.ResInfo) {
		t.Helper()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			got.GetDetails() != cleared {
			t.Errorf("%s side_dev after the one-extent success = %v/%q, "+
				"want PROVISIONING/%q", path, got.GetStatus(),
				got.GetDetails(), cleared)
		}
	}
	info, err = srv.GetSideInfo(ctx, infoReq)
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	checkCleared("probe", info.GetSideInfo().GetSideDevInfo())
	reply, err = srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	checkCleared("converge", reply.GetSideInfo().GetSideDevInfo())

	// The side climbs back: one extent, then every success doubles the
	// batch, never past DnZeroBatchExtCnt.
	node.releaseCmd(next)
	waitZeroed(t, srv, testSide)
	assertZeroouts(t, node,
		append(want, zeroClimb(sideDevPath, 0, 1, testExtCnt)...))
}

// zeroClimb is the batches a side runs from extent from up to extent to once
// nothing more is killed: batch extents first, each success doubling the size
// up to DnZeroBatchExtCnt, the last batch cut short at to.
func zeroClimb(sideDevPath string, from, batch, to uint64) []string {
	var out []string
	for from < to {
		n := min(batch, to-from)
		out = append(out, zerooutLine(sideDevPath, from, n))
		from += n
		batch = min(2*batch, common.DnZeroBatchExtCnt)
	}
	return out
}

// Three rules of DN9's rate control that the streak of
// TestZeroingBacksOffAfterRepeatedKills cannot show. A batch the tool refused
// ends the kill streak, so a kill after a refusal halves the batch again
// instead of backing the side off to one extent; a kill halves the batch that
// was killed, its own extent count, not the size the side was working at,
// which differs when the killed batch was the side's short last one; and the
// halving stops at one extent, so a killed one-extent batch that starts a
// streak is redone at one extent. Half of one extent rounded down is none,
// and a zero-extent batch zeroes nothing and doubles to zero: the side would
// never finish.
func TestZeroingHalvingRules(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	p := nf.DmPath(nf.DnSideName(testCluster, testDn, testSp, testSide))
	if common.DnZeroBatchExtCnt != 10 || common.DnZeroKillBackoff != 2 ||
		testExtCnt != 25 {
		t.Fatal("the batches below are worked out for DnZeroBatchExtCnt = " +
			"10, DnZeroKillBackoff = 2 and a 25-extent side; rework them")
	}
	for _, tc := range []struct {
		name string
		// kill and fail are the fake's killCmd and failCmd lines. The fake
		// checks failCmd first, so a line in both is refused the first time
		// it runs and killed the second.
		kill []string
		fail []string
		want []string
	}{{
		name: "a refusal ends the streak",
		kill: []string{zerooutLine(p, 0, 10), zerooutLine(p, 0, 5)},
		fail: []string{zerooutLine(p, 0, 5)},
		want: []string{
			zerooutLine(p, 0, 10), // killed: the next batch is half of it
			zerooutLine(p, 0, 5),  // refused: same size, and the streak ends
			zerooutLine(p, 0, 5),  // killed: the first kill of a new streak
			zerooutLine(p, 0, 2),  // so halved, not backed off to 1 extent
			zerooutLine(p, 2, 4),
			zerooutLine(p, 6, 8),
			zerooutLine(p, 14, 10),
			zerooutLine(p, 24, 1),
		},
	}, {
		name: "a kill halves the killed batch",
		kill: []string{zerooutLine(p, 20, 5)},
		want: []string{
			zerooutLine(p, 0, 10),
			zerooutLine(p, 10, 10),
			zerooutLine(p, 20, 5), // the short last batch, killed
			zerooutLine(p, 20, 2), // half of those 5, not of the side's 10
			zerooutLine(p, 22, 3),
		},
	}, {
		// The first case's batches again, for the one-extent batch they
		// end on: extent 24, the side's last, runs after a success, so its
		// kill is the first of a streak and takes the halving, not the
		// backoff.
		name: "a killed one-extent batch is redone at one extent",
		kill: []string{zerooutLine(p, 0, 10), zerooutLine(p, 0, 5),
			zerooutLine(p, 24, 1)},
		fail: []string{zerooutLine(p, 0, 5)},
		want: []string{
			zerooutLine(p, 0, 10), zerooutLine(p, 0, 5),
			zerooutLine(p, 0, 5), zerooutLine(p, 0, 2),
			zerooutLine(p, 2, 4), zerooutLine(p, 6, 8),
			zerooutLine(p, 14, 10),
			zerooutLine(p, 24, 1), // killed: the first kill of a streak
			zerooutLine(p, 24, 1), // half of one extent is one, never zero
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			node.mu.Lock()
			for _, line := range tc.kill {
				node.killCmd[line] = true
			}
			for _, line := range tc.fail {
				node.failCmd[line] =
					"blkdiscard: BLKZEROOUT ioctl failed: Input/output error"
			}
			node.mu.Unlock()
			if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
				1, testSide, testCn0, nil,
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			waitZeroed(t, srv, testSide)
			assertZeroouts(t, node, tc.want)
		})
	}
}

// However many of its sides are zeroing, an agent runs at most
// DnZeroConcurrency batches at once (DN9): N concurrent streams would each get
// 1/N of the disk, and with enough of them every batch would outlive the soft
// timeout. The batches park inside their children, so none returns while the
// count is taken and what the fake has recorded is exactly what is in flight.
// A side queued for a slot is healthy, not failed: it reports PROVISIONING.
func TestZeroingConcurrencyIsCapped(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	const zeroout = "cmd blkdiscard --zeroout"
	sideIds := make([]uint64, 0, common.DnZeroConcurrency+2)
	for i := uint64(0); i < common.DnZeroConcurrency+2; i++ {
		sideIds = append(sideIds, testSide+i)
	}

	if _, err := srv.SyncupDn(ctx, dnReq(1, sideIds...)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.blockCmd(zeroout)
	t.Cleanup(func() { node.releaseCmd(zeroout) })
	for _, sideId := range sideIds {
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, sideId, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide %d: %v", sideId, err)
		}
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return len(node.callsMatching(zeroout)) >= common.DnZeroConcurrency
	}) {
		t.Fatalf("fewer than %d zeroing batches ever started",
			common.DnZeroConcurrency)
	}
	// Every loop is already running — each SyncupSide above started its own —
	// so an uncapped agent has the rest in flight within milliseconds.
	if waitFor(t, 500*time.Millisecond, func() bool {
		return len(node.callsMatching(zeroout)) > common.DnZeroConcurrency
	}) {
		t.Fatalf("%d zeroing batches in flight at once, want at most %d:\n%s",
			len(node.callsMatching(zeroout)), common.DnZeroConcurrency,
			strings.Join(node.callsMatching(zeroout), "\n"))
	}
	for _, sideId := range sideIds {
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(sideId),
		})
		if err != nil {
			t.Fatalf("GetSideInfo %d: %v", sideId, err)
		}
		if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_PROVISIONING {
			t.Errorf("side %d zeroing or queued = %v/%q, want PROVISIONING",
				sideId, got.GetStatus(), got.GetDetails())
		}
	}

	// Released, the queued sides take the slots the first ones hand back, and
	// every side zeroes in its own batches, none of them redone.
	node.releaseCmd(zeroout)
	for _, sideId := range sideIds {
		waitZeroed(t, srv, sideId)
	}
	perSide := len(zerooutBatches(nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide)), testExtCnt))
	if got, want := len(node.callsMatching(zeroout)),
		len(sideIds)*perSide; got != want {
		t.Errorf("%d zeroing batches, want %d (%d per side)",
			got, want, perSide)
	}
}

// A side waiting out its retry pace holds no zeroing slot (DN9: only the
// blkdiscard does), so DnZeroConcurrency sides whose every batch fails cannot
// stop a healthy side on the same agent from zeroing. The pace is an hour, so
// the failing sides spend the rest of the test inside it: each runs its first
// batch once, is refused, and waits. A slot held through that wait would leave
// the healthy side none until the pace ran out.
func TestZeroingPacedSidesHoldNoSlot(t *testing.T) {
	srv, node := newTestServer(t)
	srv.zeroRetryInterval = time.Hour
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	const zeroout = "cmd blkdiscard --zeroout"
	failing := make([]uint64, 0, common.DnZeroConcurrency)
	for i := uint64(0); i < common.DnZeroConcurrency; i++ {
		failing = append(failing, testSide+i)
	}
	healthy := testSide + common.DnZeroConcurrency
	pathOf := func(sideId uint64) string {
		return nf.DmPath(nf.DnSideName(testCluster, testDn, testSp, sideId))
	}
	zerooutsOf := func(sideId uint64) []string {
		var out []string
		for _, line := range node.callsMatching(zeroout) {
			if strings.HasSuffix(line, " "+pathOf(sideId)) {
				out = append(out, line)
			}
		}
		return out
	}

	if _, err := srv.SyncupDn(ctx,
		dnReq(1, append(failing, healthy)...)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	for _, sideId := range failing {
		node.failCmdAlways[zerooutLine(pathOf(sideId), 0,
			common.DnZeroBatchExtCnt)] =
			"blkdiscard: BLKZEROOUT ioctl failed: Input/output error"
	}
	node.mu.Unlock()
	syncup := func(sideId uint64) {
		t.Helper()
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, sideId, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide %d: %v", sideId, err)
		}
	}
	for _, sideId := range failing {
		syncup(sideId)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return len(node.callsMatching(zeroout)) >= common.DnZeroConcurrency
	}) {
		t.Fatalf("the failing sides never ran their first batch:\n%s",
			strings.Join(node.callsMatching(zeroout), "\n"))
	}

	// Every failing side has been refused and paces for the rest of the
	// test; the healthy side zeroes meanwhile, in its own batches.
	syncup(healthy)
	waitZeroed(t, srv, healthy)
	for _, sideId := range failing {
		want := zerooutLine(pathOf(sideId), 0, common.DnZeroBatchExtCnt)
		if got := zerooutsOf(sideId); len(got) != 1 || got[0] != want {
			t.Errorf("side %d zeroing batches = %q, want only %q", sideId,
				got, want)
		}
	}
	want := zerooutBatches(pathOf(healthy), testExtCnt)
	if got := zerooutsOf(healthy); strings.Join(got, "\n") !=
		strings.Join(want, "\n") {
		t.Errorf("healthy side zeroing batches:\n%s\nwant:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A side queued for a zeroing slot is torn down at once (DN9): dropping it
// from the DN's list cancels its loop where it waits, and nothing in that wait
// belongs to another side. The teardown runs under the node write lock, so a
// slot wait the cancel could not reach would hold the whole agent until some
// other side's batch returned — here, until the test released it.
//
// The first DnZeroConcurrency sides take every slot and park inside their
// children before the last side starts, so which side queues is fixed. The
// teardown starts only once that side's loop is parked in its slot wait: a
// cancel that landed before the loop got there would never test whether the
// wait itself answers a cancel.
func TestZeroingQueuedSideTearsDownAtOnce(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	const zeroout = "cmd blkdiscard --zeroout"
	kept := make([]uint64, 0, common.DnZeroConcurrency)
	for i := uint64(0); i < common.DnZeroConcurrency; i++ {
		kept = append(kept, testSide+i)
	}
	queued := testSide + common.DnZeroConcurrency
	zerooutsOf := func(sideId uint64) int {
		path := nf.DmPath(nf.DnSideName(testCluster, testDn, testSp, sideId))
		n := 0
		for _, line := range node.callsMatching(zeroout) {
			if strings.HasSuffix(line, " "+path) {
				n++
			}
		}
		return n
	}

	if _, err := srv.SyncupDn(ctx,
		dnReq(1, append(kept, queued)...)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.blockCmd(zeroout)
	t.Cleanup(func() { node.releaseCmd(zeroout) })
	syncup := func(sideId uint64) {
		t.Helper()
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, sideId, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide %d: %v", sideId, err)
		}
	}
	for _, sideId := range kept {
		syncup(sideId)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return len(node.callsMatching(zeroout)) == common.DnZeroConcurrency
	}) {
		t.Fatalf("the first %d sides never had a batch each in flight:\n%s",
			common.DnZeroConcurrency,
			strings.Join(node.callsMatching(zeroout), "\n"))
	}
	syncup(queued)
	if !waitFor(t, 5*time.Second, parkedInZeroSlot) {
		t.Fatal("the last side's loop never parked in its slot wait")
	}

	// Drop the queued side while every slot is held by a parked batch.
	done := make(chan error, 1)
	go func() {
		_, err := srv.SyncupDn(ctx, dnReq(2, kept...))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
	case <-time.After(2 * time.Second):
		// Let the teardown finish before failing, so it does not outlive the
		// test holding the node write lock.
		node.releaseCmd(zeroout)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("tearing down the queued side waited on another side's batch")
	}
	queuedName := nf.DnSideName(testCluster, testDn, testSp, queued)
	if !node.hasCall("cmd dmsetup remove " + queuedName) {
		t.Error("the queued side's device was never removed")
	}
	if srv.getSide(sideKey(testCluster, testDn, testSp, queued)) != nil {
		t.Error("the queued side's state survived the teardown")
	}
	if got := len(node.callsMatching(zeroout)); got !=
		common.DnZeroConcurrency {
		t.Errorf("%d zeroing batches by the end of the teardown, want only "+
			"the %d parked ones:\n%s", got, common.DnZeroConcurrency,
			strings.Join(node.callsMatching(zeroout), "\n"))
	}

	// Released, the kept sides zero in their own batches, none redone, and
	// the dropped side never runs one.
	node.releaseCmd(zeroout)
	for _, sideId := range kept {
		waitZeroed(t, srv, sideId)
	}
	if n := zerooutsOf(queued); n != 0 {
		t.Errorf("%d zeroing batches for the dropped side, want none", n)
	}
	perSide := len(zerooutBatches(nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide)), testExtCnt))
	for _, sideId := range kept {
		if n := zerooutsOf(sideId); n != perSide {
			t.Errorf("side %d: %d zeroing batches, want %d", sideId, n,
				perSide)
		}
	}
}

// parkedInZeroSlot reports whether some goroutine is blocked in zeroSlot,
// read off the runtime's goroutine dump: a loop waiting for a slot has run no
// command yet, so nothing the fake records can show that it got there.
func parkedInZeroSlot() bool {
	buf := make([]byte, 1<<20)
	dump := string(buf[:runtime.Stack(buf, true)])
	for _, g := range strings.Split(dump, "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(g, ").zeroSlot(") &&
			!strings.Contains(header, "[running]") &&
			!strings.Contains(header, "[runnable]") {
			return true
		}
	}
	return false
}

// TestZeroRetryIntervalDefault pins the production pace, which
// startTestServer shortens and some tests set for themselves.
func TestZeroRetryIntervalDefault(t *testing.T) {
	srv := NewDnAgentServer(newFakeNode().osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	if srv.zeroRetryInterval != common.DnZeroRetryInterval*time.Second {
		t.Errorf("zeroRetryInterval = %v, want %v", srv.zeroRetryInterval,
			common.DnZeroRetryInterval*time.Second)
	}
}

// stopTestServer plays process exit: stop every background goroutine and join
// them, the way agent.Serve does after GracefulStop.
func stopTestServer(t *testing.T, srv *DnAgentServer) {
	t.Helper()
	for _, key := range srv.allSideKeys() {
		st := srv.getSide(key)
		srv.stopMigrRetry(st)
		srv.stopZeroing(st)
	}
	srv.WaitBackground()
}

// clearRecord deletes the side's allocation record behind the agent's back —
// the lost-or-foreign-disk case — and makes the server re-read the disk.
func clearRecord(t *testing.T, srv *DnAgentServer, node *fakeNode) {
	t.Helper()
	other := newVerifiedMeta(t, node)
	if err := other.FreeSide(context.Background(), testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	srv.meta = newVerifiedMeta(t, node)
}

// clearZeroed puts the side's on-disk record back into the not-zeroed state —
// "somebody re-allocated the side behind our back" — and makes the server
// re-read the disk. It goes through a second, separately verified DiskMeta so
// it exercises the same API the agent does, and it is the invariant in
// action: a re-allocated record starts all-not-zeroed ([D15]).
func clearZeroed(t *testing.T, srv *DnAgentServer, node *fakeNode) {
	t.Helper()
	ctx := context.Background()
	other := newVerifiedMeta(t, node)
	if err := other.FreeSide(ctx, testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	if _, err := other.AllocSide(
		ctx, testSp, testSide, testExtCnt); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	srv.meta = newVerifiedMeta(t, node)
}

// newVerifiedMeta opens the test disk the way the agent does: read the header,
// confirm the identity, learn the raw size.
func newVerifiedMeta(t *testing.T, node *fakeNode) *DiskMeta {
	t.Helper()
	meta := NewDiskMeta(node.osClient(), testDisk)
	meta.SetDiskSize(node.devSize[testDisk])
	if err := meta.EnsureFormatted(context.Background(),
		testCluster, testDn, testExtentSize, mayFormat); err != nil {
		t.Fatalf("EnsureFormatted: %v", err)
	}
	return meta
}

// ---------------------------------------------------------------------------
// 6. ANA moves ([D4], SH18)
// ---------------------------------------------------------------------------

func TestPrimaryFlipRewritesOnlyAnaGrpIds(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	reply, err := srv.SyncupSide(context.Background(),
		sideReq(2, testSide, testCn1, []uint64{testCn0},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}

	nqn0 := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	nqn1 := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)
	wantWrites := map[string]bool{
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, nqn0, common.AnaGrpIdNonOptimized): false,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, nqn1, common.AnaGrpIdOptimized): false,
	}
	for _, call := range node.Calls() {
		if !strings.HasPrefix(call, "writedirect ") {
			continue
		}
		if _, ok := wantWrites[call]; ok {
			wantWrites[call] = true
			continue
		}
		t.Errorf("unexpected configfs write: %s", call)
	}
	for call, seen := range wantWrites {
		if !seen {
			t.Errorf("missing ana transition: %s", call)
		}
	}
	// The two dm-linear tables are reloaded; nothing else about the
	// exports changes.
	for _, name := range []string{
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1),
	} {
		if !node.hasCall("cmd dmsetup reload " + name) {
			t.Errorf("dm-linear %s was not reloaded", name)
		}
	}
}

func TestNoWriteFileOnConfigfsAndNoAnaStateRewrite(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	syncupBoth(t, srv, 2, testSide)
	if _, err := srv.SyncupSide(context.Background(),
		sideReq(3, testSide, testCn1, []uint64{testCn0},
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	anaStateWrites := 0
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "write ") &&
			strings.Contains(call, "/sys/kernel/config") {
			t.Errorf("WriteFile used on configfs: %s", call)
		}
		if strings.HasPrefix(call, "writedirect ") &&
			strings.Contains(call, "/ana_state=") {
			anaStateWrites++
		}
	}
	// Exactly the three one-time writes of the port setup.
	if anaStateWrites != 3 {
		t.Errorf("ana_state writes = %d, want 3 (port setup only)",
			anaStateWrites)
	}
}

// ---------------------------------------------------------------------------
// 9. sp_level (DN11)
// ---------------------------------------------------------------------------

func TestSpLevels(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	// SP_LEVEL_NO_SIDE: exports gone, dm and the side device kept.
	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_NO_SIDE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.dirs[agent.NvmetRoot+"/subsystems/"+nqn] {
		t.Error("SP_LEVEL_NO_SIDE kept the subsystem")
	}
	if _, ok := node.dms[linName]; !ok {
		t.Error("SP_LEVEL_NO_SIDE removed the dm-linear")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Error("SP_LEVEL_NO_SIDE removed the side device")
	}

	// SP_LEVEL_DISABLE: only the side device and its record remain.
	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_DISABLE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, ok := node.dms[linName]; ok {
		t.Error("SP_LEVEL_DISABLE kept the dm-linear")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Error("SP_LEVEL_DISABLE removed the side device")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Error("SP_LEVEL_DISABLE freed the allocation record")
	}

	// Lowering back rebuilds everything.
	reply, err := srv.SyncupSide(ctx, sideReq(4, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dirs[agent.NvmetRoot+"/subsystems/"+nqn] {
		t.Error("lowering the level did not rebuild the subsystem")
	}
	if _, ok := node.dms[linName]; !ok {
		t.Error("lowering the level did not rebuild the dm-linear")
	}
	for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
		if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("cn %d nvmeof after rebuild: %v %q",
				cnId, info.GetStatus(), info.GetDetails())
		}
	}
}

// A side re-synced at SP_LEVEL_DISABLE while its bits are still incomplete
// keeps issuing its zeroing batches (DN11, §6 test 21). §9.4 provisioning sits
// *below* the level ladder — exactly as the trim it replaced did — which is why
// convergeSide runs ensureSideDev, and with it startZeroing, before the
// !plan.wantDm early return: the level takes the layers that serve IO away, not
// the work that makes the side safe to serve at all, so a side disabled
// mid-zeroing is fully zeroed by the time the level comes back down instead of
// starting over. The invariants of the level itself are unchanged by the
// unfinished bits: the per-CN dm stacks and the exports go, the side device and
// its allocation record stay.
func TestDisableLevelKeepsZeroing(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	sideDevPath := nf.DmPath(sideDevName)
	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)

	// The side starts in the steady state, so the teardown below has real
	// exports and dm-linears to remove; then its record is re-allocated behind
	// the agent's back, which is what leaves the bits incomplete under a
	// request that still says provisioned = true (row 5 of the §9.4 converge matrix — the
	// flag is not what keeps the goroutine, the bits are).
	syncupBoth(t, srv, 1, testSide)
	clearZeroed(t, srv, node)

	// The second batch parks inside its child, so the loop cannot run out of
	// work while the assertions below look at it: "still zeroing" is then a
	// fact about the side rather than a race against the goroutine.
	batch1 := "cmd blkdiscard --zeroout --offset " + strconv.FormatUint(
		common.DnZeroBatchExtCnt*testExtentSize, 10)
	node.blockCmd(batch1)
	t.Cleanup(func() { node.releaseCmd(batch1) })

	node.Reset()
	reply, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_DISABLE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The agent trusts its own bits over the flag at every level.
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != tagNotZeroed {
		t.Errorf("side_dev = %v/%q, want ERROR/%q",
			got.GetStatus(), got.GetDetails(), tagNotZeroed)
	}

	// The DISABLE invariants.
	for _, cnId := range []uint64{testCn0, testCn1} {
		linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId)
		if _, ok := node.dms[linName]; ok {
			t.Errorf("SP_LEVEL_DISABLE kept the dm-linear of cn %d", cnId)
		}
	}
	if node.dirs[agent.NvmetRoot+"/subsystems/"+nqn] {
		t.Error("SP_LEVEL_DISABLE kept the subsystem")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Error("SP_LEVEL_DISABLE removed the side device")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Error("SP_LEVEL_DISABLE freed the allocation record")
	}

	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	srv.mu.Lock()
	zeroing := st.zeroing
	srv.mu.Unlock()
	if !zeroing {
		t.Fatal("SP_LEVEL_DISABLE dropped the zeroing goroutine")
	}
	// And it is *issuing batches*, not merely registered: the first one lands,
	// bits and all, with the side sitting at DISABLE.
	if !waitFor(t, 5*time.Second, func() bool {
		rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
		return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
	}) {
		t.Fatal("no zeroing batch landed at SP_LEVEL_DISABLE")
	}
	// The parked batch is released and the side runs to fully zeroed: three
	// batches at exactly the byte ranges an enabled side would use, every one
	// of them recorded after the Reset above and so issued at DISABLE.
	node.releaseCmd(batch1)
	waitZeroed(t, srv, testSide)
	assertOrder(t, node, zerooutBatches(sideDevPath, testExtCnt)...)

	// Provisioning is bottom-layer work, so finishing it rebuilds nothing the
	// level forbids — the side is ready, and still disabled.
	if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("a disabled side was exported while it finished zeroing")
	}
	for _, cnId := range []uint64{testCn0, testCn1} {
		linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId)
		if _, ok := node.dms[linName]; ok {
			t.Errorf("zeroing rebuilt the dm-linear of cn %d at DISABLE", cnId)
		}
	}
}

// TestReadOnlyLevelIsNoOpOnDn pins DN11/[D11]: SP_LEVEL_READONLY — and every
// CN-only level below SP_LEVEL_NO_MIGRATION — has no DN-side behavior at all.
// Read-only is enforced on the CN's user-facing namespaces; the DN cannot
// enforce it (nvmet needs a writeable backing bdev, dm remaps bypass mid-stack
// read-only flags, and md metadata/resync writes must keep flowing).
func TestReadOnlyLevelIsNoOpOnDn(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)

	revision := uint64(1)
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE,
		pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		pb.SpLevel_SP_LEVEL_NO_REDUND,
	} {
		revision++
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			sideReq(revision, testSide, testCn0, []uint64{testCn1}, level))
		if err != nil {
			t.Fatalf("SyncupSide(%v): %v", level, err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("%v rejected: %v", level, reply.GetAgentReply())
		}
		for _, call := range node.Mutations() {
			if strings.HasPrefix(call, "writeproto") {
				continue // SH5: the request itself is re-persisted
			}
			t.Errorf("%v mutated the dn: %s", level, call)
		}
		for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
			if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Errorf("cn %d export at %v: %v %q",
					cnId, level, info.GetStatus(), info.GetDetails())
			}
		}
		if _, ok := node.dms[sideDevName]; !ok {
			t.Errorf("%v removed the side device", level)
		}
		if node.dms[linName].readOnly {
			t.Errorf("%v made the dm-linear read-only", level)
		}
		if node.dms[sideDevName].readOnly {
			t.Errorf("%v made the side device read-only", level)
		}
	}
}

// ---------------------------------------------------------------------------
// 12. The orphan sweep is pointer-list driven ([D13], DN6)
// ---------------------------------------------------------------------------

// [D13]: the on-disk volume table, not --local-store, is authoritative for
// extent placement — a node that loses --local-store but keeps its disk
// recovers exactly the layout it had. The sweep must therefore never treat
// "no local state for this side" as proof that the side is gone: doing so
// frees its extents, and the next SyncupSide re-runs the §9.4 provisioning
// protocol and zeroes live data.
func TestLocalStoreLossKeepsSideAllocation(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	before, ok, err := srv.meta.LookupSide(ctx, testSp, testSide)
	if err != nil || !ok {
		t.Fatalf("no record after the first converge: %v %v", ok, err)
	}
	wantRuns := fmt.Sprintf("%v", before.GetRunList())

	// The agent restarts with an empty --local-store; the disk is intact.
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	srv2 := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	if err := srv2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Reconcile has no DN state at all, so it may sweep nothing.
	if _, ok, _ := srv2.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("reconcile with an empty store freed the side's extents")
	}

	// The CP re-syncs. SyncupDn reintroduces the pointer (DN8 ordering); the
	// side's state has not arrived yet, which is exactly the window the old
	// sweep got wrong.
	node.Reset()
	if _, err := srv2.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	after, ok, err := srv2.meta.LookupSide(ctx, testSp, testSide)
	if err != nil || !ok {
		t.Fatalf("SyncupDn swept a side still in the pointer list: %v %v",
			ok, err)
	}
	if got := fmt.Sprintf("%v", after.GetRunList()); got != wantRuns {
		t.Errorf("extent runs moved across the restart: %s, want %s",
			got, wantRuns)
	}
	if !sideFullyZeroed(after) {
		t.Error("the zeroed bits were lost across the restart")
	}
	if node.hasCall("cmd dmsetup remove " + nf.DnSideName(
		testCluster, testDn, testSp, testSide)) {
		t.Error("SyncupDn removed a live side device")
	}

	// And the re-sent SyncupSide reuses the allocation instead of re-zeroing
	// it.
	node.Reset()
	if _, err := srv2.SyncupSide(ctx, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "blkdiscard") {
			t.Errorf("the recovered side was re-zeroed (data loss): %s", call)
		}
	}
}

// A side whose pointer really did leave the DN's list, and whose FreeSide
// never ran, IS swept — that is what the sweep exists for (the DN6 crash
// window).
func TestOrphanSweepFreesTornDownSide(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	// Simulate the crash window: the side's local state and its resources
	// are gone, but the volume-table record survived.
	key := sideKey(testCluster, testDn, testSp, testSide)
	srv.dropSide(key)
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("no record to orphan")
	}

	node.Reset()
	// An empty pointer list is the authority saying the side is gone.
	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the orphan record survived the sweep")
	}
	if !node.hasCall("cmd dmsetup remove " + nf.DnSideName(
		testCluster, testDn, testSp, testSide)) {
		t.Error("the orphan's side device was not removed")
	}
}

// A disk formatted for another node is refused, and — because a failed DN
// converge does not stop the side converges that follow (DN19) — nothing
// downstream may write to it or build on its records either. That holds
// when the DN converge's own header read did not answer, too: the side
// converge is then the first to read whose disk this is, and the records it
// finds are another node's even where their (sp, side) ids match its own —
// ids restart in every cluster. Handing one out would put that node's extents
// behind this node's devices and export them.
func TestForeignDiskIsNeverWritten(t *testing.T) {
	header := fmt.Sprintf("readblock %s off=%d len=%d",
		testDisk, common.DnHeaderOffset, common.DnHeaderSize)
	for _, tc := range []struct {
		name string
		// killHeader cuts off the header read of the second agent's
		// SyncupDn, the check that would have found the disk foreign.
		killHeader bool
		// metaDetails is what that SyncupDn's meta row says.
		metaDetails string
	}{
		{name: "answered", metaDetails: "foreign disk"},
		{name: "header read killed", killHeader: true,
			metaDetails: "reading the disk header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)

			// A second agent claims the same device under a different dn_id.
			node.mu.Lock()
			node.protos = map[string][]byte{}
			node.mu.Unlock()
			srv2 := NewDnAgentServer(node.osClient(),
				common.NewNameFmt(common.DefaultLocalStorPrefix),
				common.DefaultLocalStorPrefix, testDisk, testTrConf(),
				common.NvmetPortId)
			if err := srv2.Reconcile(ctx); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			node.Reset()
			if tc.killHeader {
				setHook(node, node.killRead, header)
			}
			req := dnReq(1, testSide)
			req.DnId = testDn + 100
			reply, err := srv2.SyncupDn(ctx, req)
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			meta := reply.GetDnInfo().GetMetaInfo()
			if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(meta.GetDetails(), tc.metaDetails) {
				t.Errorf("meta_info = %v/%q, want ERROR/%s",
					meta.GetStatus(), meta.GetDetails(), tc.metaDetails)
			}

			// The side converge that follows must neither allocate on the
			// foreign disk nor hand out the record it holds for the same ids.
			sideReply, err := srv2.SyncupSide(ctx, func() *pb.SyncupSideRequest {
				r := sideReq(1, testSide, testCn0, nil,
					pb.SpLevel_SP_LEVEL_READWRITE)
				r.DnId = testDn + 100
				return r
			}())
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := sideReply.GetSideInfo().GetSideDevInfo(); got.
				GetStatus() == pb.ResStatus_RES_STATUS_OK ||
				!strings.Contains(got.GetDetails(), "foreign disk") {
				t.Errorf("side_dev on a foreign disk = %v/%q, want a "+
					"foreign-disk refusal", got.GetStatus(), got.GetDetails())
			}
			for _, call := range node.Calls() {
				if strings.HasPrefix(call, "writeblock") {
					t.Errorf("a foreign disk was written: %s", call)
				}
				if strings.Contains(call, "dmsetup remove") {
					t.Errorf("a foreign disk's devices were removed: %s",
						call)
				}
				if strings.Contains(call, "dmsetup create") ||
					strings.Contains(call, "dmsetup reload") {
					t.Errorf("a device was built on a foreign disk: %s",
						call)
				}
			}
			// The original owner's record is intact on disk.
			fresh := NewDiskMeta(node.osClient(), testDisk)
			if _, ok, err := fresh.LookupSide(ctx, testSp, testSide); !ok ||
				err != nil {
				t.Errorf("the owning node's record was destroyed: %v %v",
					ok, err)
			}
		})
	}
}

// One header read that did not answer at startup must not freeze the sides.
// DN5's identity check compares the header the volume table was loaded under
// with the identity the DN converge asked for, and the converge asks before
// it reads: after a restart whose DN converge had its header read cut off,
// the first read that answers — the side converge's own — confirms the disk,
// and a failover of a side the table records converges before any check
// round. The old agent confirmed the identity in the DN converge alone: one
// killed read at startup refused every later SyncupSide at AllocSide while
// CheckDn reported the node clean, and nothing ran the DN converge again
// until the next DN revision.
//
// While no read of the header has answered at all, the DN's verdict is not
// clean (DN6's record step), so the worker re-drives the SyncupDn; and the
// first GetDnInfo whose probe reads the header back replies clean, because
// the probe runs before the verdict is taken.
func TestATransientHeaderReadDoesNotFreezeTheSides(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	stopTestServer(t, srv)
	header := fmt.Sprintf("readblock %s off=%d len=%d",
		testDisk, common.DnHeaderOffset, common.DnHeaderSize)

	checkDn := func(srv *DnAgentServer) *pb.CheckDnReply {
		t.Helper()
		reply, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: 1,
			ShowInfo: true,
		}, nil)
		return reply
	}
	// flip is a failover of the side's primary: it converges only past
	// AllocSide, and it lands only if the new primary's namespace moves to
	// the optimized group — exactly once.
	flip := func(srv *DnAgentServer, revision uint64, primary uint64,
		standby uint64) {
		t.Helper()
		node.Reset()
		reply, err := srv.SyncupSide(ctx, sideReq(revision, testSide,
			primary, []uint64{standby}, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		dev := reply.GetSideInfo().GetSideDevInfo()
		if reply.GetAgentReply().GetCode() != 0 ||
			dev.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("rev %d: agent_reply %v, side_dev %v/%q, want 0 and OK",
				revision, reply.GetAgentReply(), dev.GetStatus(),
				dev.GetDetails())
		}
		nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, primary)
		moved := fmt.Sprintf(
			"writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, nqn, common.AnaGrpIdOptimized)
		if got := len(node.callsMatching(moved)); got != 1 {
			t.Errorf("rev %d: %d moves of cn %d's namespace to the "+
				"optimized group, want 1", revision, got, primary)
		}
	}

	// A fresh process over the same disk and store, whose first read of the
	// disk header — the startup DN converge's — the soft timeout cuts off.
	// Every later read answers.
	setHook(node, node.killRead, header)
	srv = startTestServer(t, node)
	node.mu.Lock()
	armed := node.killRead[header]
	node.mu.Unlock()
	if armed {
		t.Fatal("the startup reconcile never read the disk header")
	}
	// A failover converges before any check round, and the first round is
	// clean.
	flip(srv, 2, testCn1, testCn0)
	if _, _, ok := srv.meta.Identity(); !ok {
		t.Error("the startup side converge read this node's header back, " +
			"and the identity is still unconfirmed")
	}
	reply := checkDn(srv)
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("first verdict after the restart = %v, want 0", got)
	}
	stopTestServer(t, srv)

	// A restart during which no read of the header answers: nothing
	// confirms the disk, and a round must say so — a clean verdict here is
	// what left the old agent unconfirmed until the next DN revision.
	setHook(node, node.killReadAlways, header)
	srv = startTestServer(t, node)
	reply = checkDn(srv)
	if got := reply.GetDnInfo().GetMetaInfo().GetStatus(); got ==
		pb.ResStatus_RES_STATUS_OK {
		t.Error("meta_info = OK, but no header read answered")
	}
	if got := reply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), "disk identity") {
		t.Errorf("verdict with the identity unconfirmed = %v, want a "+
			"leftover naming the disk identity", got)
	}

	// Once the disk answers again, the first GetDnInfo is clean: its probe
	// reads the header back before the verdict is taken.
	clearHook(node, node.killReadAlways, header)
	info, err := srv.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := info.GetDnInfo().GetMetaInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("meta_info = %v/%q, want OK", got.GetStatus(),
			got.GetDetails())
	}
	if got := info.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("verdict of the GetDnInfo whose probe read the header = "+
			"%v, want 0", got)
	}

	// And the next failover converges.
	flip(srv, 3, testCn0, testCn1)
}

// The clone-metadata record is handed out on the same terms as the side's,
// after the same restart. Were it refused where the side record is handed
// out, the converge that gets past the side device would read the dm-clone
// as not live and reload a live destination's primary dm-linear off the
// clone onto its dm-error — an outage of the leg.
func TestATransientHeaderReadKeepsALiveCloneServing(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if reply, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupSide: %v %v", reply.GetAgentReply(), err)
	}
	stopTestServer(t, srv)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	cloneNo := node.devNo[nf.DmPath(
		nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId))]
	if !strings.Contains(node.dms[linName].table, cloneNo) {
		t.Fatalf("the primary's dm-linear %q is not on the dm-clone (%s)",
			node.dms[linName].table, cloneNo)
	}

	header := fmt.Sprintf("readblock %s off=%d len=%d",
		testDisk, common.DnHeaderOffset, common.DnHeaderSize)
	setHook(node, node.killRead, header)
	node.Reset()
	srv = startTestServer(t, node)
	node.mu.Lock()
	armed := node.killRead[header]
	node.mu.Unlock()
	if armed {
		t.Fatal("the startup reconcile never read the disk header")
	}
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo(); got.
		GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("dm_clone_info = %v/%q, want OK", got.GetStatus(),
			got.GetDetails())
	}
	if got := node.callsMatching("cmd dmsetup reload " + linName); len(got) !=
		0 {
		t.Errorf("the primary's dm-linear was reloaded: %v", got)
	}
	if !strings.Contains(node.dms[linName].table, cloneNo) {
		t.Errorf("the primary's dm-linear %q left the dm-clone (%s)",
			node.dms[linName].table, cloneNo)
	}
}

// A side still being zeroed when the agent stops goes on zeroing after a
// restart whose first header read did not answer, and each batch's bits are
// persisted the first time: the side converge that starts its goroutine again
// is the read that confirms the disk, so no batch is zeroed twice for want
// of a confirmation, and the side finishes without any check round. The old
// agent refused the side at AllocSide until the next DN revision, and the
// side reported its progress for ever with nothing zeroing it.
func TestATransientHeaderReadKeepsZeroing(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevPath := nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// Every batch fails, so the side is still at zeroing 0/n when the
	// process stops.
	setFailAlways(node, "blkdiscard --zeroout", "stalled")
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	stopTestServer(t, srv)
	if rec, ok, err := srv.meta.LookupSide(
		ctx, testSp, testSide); err != nil || !ok || sideZeroedCnt(rec) != 0 {
		t.Fatalf("the side was not left allocated and unzeroed: %v %v",
			ok, err)
	}
	clearFailAlways(node, "blkdiscard --zeroout")

	// A fresh process over the same disk and store, whose first read of the
	// disk header — the startup DN converge's — the soft timeout cuts off.
	header := fmt.Sprintf("readblock %s off=%d len=%d",
		testDisk, common.DnHeaderOffset, common.DnHeaderSize)
	node.Reset()
	setHook(node, node.killRead, header)
	srv = startTestServer(t, node)
	waitZeroed(t, srv, testSide)
	node.mu.Lock()
	armed := node.killRead[header]
	node.mu.Unlock()
	if armed {
		t.Fatal("the startup reconcile never read the disk header")
	}
	// Each batch exactly once, in order: a batch whose bits the volume
	// table refused would have been zeroed again.
	want := zerooutBatches(sideDevPath, testExtCnt)
	if got := node.callsMatching("blkdiscard --zeroout"); strings.Join(
		got, "\n") != strings.Join(want, "\n") {
		t.Errorf("zeroing batches after the restart:\n%s\nwant each "+
			"exactly once:\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
}

// A side still being zeroed when the agent stops, after a restart during
// which no read of the disk header answers: the startup converge of the side
// cannot read its record, so nothing starts its zeroing, and once the disk
// answers again every check round reads the record back fine. The side's
// verdict is what notices — a record with extents still to zero and no
// zeroing goroutine is not clean — and the worker re-sends the SyncupSide
// whose converge starts the goroutine. The old verdict was clean on every
// round, and the side reported "zeroing 0/n" for ever with nothing zeroing
// it.
func TestASideLeftWithNothingZeroingIsReDriven(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevPath := nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// Every batch fails, so the side is still at zeroing 0/n when the
	// process stops.
	setFailAlways(node, "blkdiscard --zeroout", "stalled")
	sideRequest := unprovisionedSideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	if _, err := srv.SyncupSide(ctx, sideRequest); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	stopTestServer(t, srv)
	clearFailAlways(node, "blkdiscard --zeroout")

	// A fresh process over the same disk and store, none of whose reads of
	// the disk header answers until the startup reconcile is over.
	header := fmt.Sprintf("readblock %s off=%d len=%d",
		testDisk, common.DnHeaderOffset, common.DnHeaderSize)
	node.Reset()
	setHook(node, node.killReadAlways, header)
	srv = startTestServer(t, node)
	clearHook(node, node.killReadAlways, header)
	if !node.hasCall(header) {
		t.Fatal("the startup reconcile never read the disk header")
	}
	if got := len(node.callsMatching("blkdiscard --zeroout")); got != 0 {
		t.Fatalf("zeroing ran although no read of the header answered: "+
			"%d batches", got)
	}
	node.Reset()

	// The worker's rounds: a Check reply that is not clean re-sends that
	// object's Syncup* (RW4).
	var firstSide *pb.AgentReply
	for round := 0; round < 3; round++ {
		dnReply, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: 1,
		}, nil)
		if dnReply.GetAgentReply().GetCode() != 0 {
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
		}
		sideReply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 1,
		}, nil)
		if firstSide == nil {
			firstSide = sideReply.GetAgentReply()
		}
		if sideReply.GetAgentReply().GetCode() != 0 {
			if _, err := srv.SyncupSide(ctx, sideRequest); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}
	}
	if firstSide.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(firstSide.GetDetails(), "zeroing 0/") {
		t.Errorf("first side verdict with nothing zeroing = %v, want a "+
			"leftover naming the zeroing", firstSide)
	}
	waitZeroed(t, srv, testSide)
	// Each batch exactly once: the one SyncupSide the verdict re-drove
	// started one goroutine.
	want := zerooutBatches(sideDevPath, testExtCnt)
	if got := node.callsMatching("blkdiscard --zeroout"); strings.Join(
		got, "\n") != strings.Join(want, "\n") {
		t.Errorf("zeroing batches after the re-drive:\n%s\nwant each "+
			"exactly once:\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
	// Once zeroed, the verdict is clean again.
	sideReply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 1,
	}, nil)
	if got := sideReply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("side verdict once zeroed = %v, want 0", got)
	}
}

// A running zeroing loop reads its record as this node's only (DN5, DN9). A
// header that turns into another node's under it — the same format, another
// dn id — makes the next probe drop the table, and the loop's next re-read
// loads the table under that header. No batch may be computed from it: the
// loop used to read the record ungated, zero the batch its bits named, have
// the bits refused and issue the same batch again every retry interval, for
// ever, over a disk that was no longer confirmed as this node's.
func TestAZeroingLoopIssuesNoBatchUnderAForeignHeader(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The first batch parks in its command, so the header changes while the
	// loop is at a known point: past its read of the record.
	const zeroout = "blkdiscard --zeroout"
	node.blockCmd(zeroout)
	t.Cleanup(func() { node.releaseCmd(zeroout) })
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(zeroout)
	}) {
		t.Fatal("the first batch never started")
	}

	// The header turns into another dn's, and a probe round finds it.
	srv.meta.mu.Lock()
	hdr := proto.Clone(srv.meta.hdr).(*pb.DnDiskHeader)
	srv.meta.mu.Unlock()
	hdr.DnId = testDn + 1
	block, err := buildDnHeader(hdr)
	if err != nil {
		t.Fatalf("buildDnHeader: %v", err)
	}
	node.corruptBlock(testDisk, common.DnHeaderOffset, block)
	reply, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 1, ShowInfo: true,
	}, nil)
	if meta := reply.GetDnInfo().GetMetaInfo(); !strings.Contains(
		meta.GetDetails(), "foreign disk") {
		t.Fatalf("meta_info = %v/%q, want the foreign disk",
			meta.GetStatus(), meta.GetDetails())
	}

	// The parked batch finishes and its bits are refused. Two sightings of
	// the refusal, the test clearing the first, are two full turns of the
	// loop after it: under the old lookup each turn issued a batch first.
	node.Reset()
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	refused := func() bool {
		err := srv.zeroingErr(st)
		return err != nil && strings.Contains(err.Error(), "foreign disk")
	}
	node.releaseCmd(zeroout)
	for turn := 0; turn < 2; turn++ {
		if !waitFor(t, 5*time.Second, refused) {
			t.Fatalf("turn %d: the loop never reported the foreign disk",
				turn)
		}
		srv.setZeroingErr(st, nil)
	}
	if got := node.callsMatching(zeroout); len(got) != 0 {
		t.Errorf("%d batches after the header became another node's: %v",
			len(got), got)
	}
	if got := node.callsMatching("writeblock"); len(got) != 0 {
		t.Errorf("the disk was written: %v", got)
	}

	// side_dev_info carries the refusal.
	if !waitFor(t, 5*time.Second, refused) {
		t.Fatal("the loop stopped reporting the foreign disk")
	}
	sideReply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 1, ShowInfo: true,
	}, nil)
	if dev := sideReply.GetSideInfo().GetSideDevInfo(); dev.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(dev.GetDetails(), "foreign disk") {
		t.Errorf("side_dev_info = %v/%q, want ERROR naming the foreign "+
			"disk", dev.GetStatus(), dev.GetDetails())
	}
}

// The side verdict's zeroing comparison reads the record as this node's only
// (DN5, DN16). A second agent syncing the same disk under another dn id finds
// the owner's record under the same (sp, side) ids, still being zeroed; its
// own side converge is refused, so nothing will ever zero that record on its
// behalf, and a verdict that counted it would re-send the refused SyncupSide
// every round. The DN's verdict is what reports the foreign disk.
func TestAForeignRecordIsNotZeroingToReDrive(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The owner's side stays at zeroing 0/n.
	setFailAlways(node, "blkdiscard --zeroout", "stalled")
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}

	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	srv2 := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	if err := srv2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dnRequest := dnReq(1, testSide)
	dnRequest.DnId = testDn + 100
	if _, err := srv2.SyncupDn(ctx, dnRequest); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	sideRequest := unprovisionedSideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	sideRequest.DnId = testDn + 100
	sideReply, err := srv2.SyncupSide(ctx, sideRequest)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := sideReply.GetSideInfo().GetSideDevInfo(); !strings.Contains(
		got.GetDetails(), "foreign disk") {
		t.Fatalf("side_dev on a foreign disk = %v/%q, want a foreign-disk "+
			"refusal", got.GetStatus(), got.GetDetails())
	}
	reply, _ := srv2.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn + 100,
		SidePointer: sidePtr(testSide), Revision: 1,
	}, nil)
	if got := reply.GetAgentReply(); strings.Contains(
		got.GetDetails(), "zeroing") {
		t.Errorf("side verdict on a foreign disk = %v, want no zeroing "+
			"to re-drive", got)
	}
	dnCheck, _ := srv2.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn + 100, Revision: 1,
	}, nil)
	if got := dnCheck.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), "disk identity") {
		t.Errorf("dn verdict on a foreign disk = %v, want a leftover "+
			"naming the disk identity", got)
	}
}

// A header that goes blank under live side devices is never formatted over
// (DN5). One mistaken dd over the disk's first 4 KiB is enough to blank it;
// the probe that finds it so drops the loaded table (DN18), and the verdict
// that says so re-drives the SyncupDn. Were that converge to format the disk,
// the fresh, empty table would hand the live side's extents to the next
// side, which would zero them and serve them as its own. The disk is
// formatted only once the sweep has removed every device that maps it.
func TestABlankHeaderUnderLiveSidesIsNeverFormatted(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	liveName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	newName := nf.DnSideName(testCluster, testDn, testSp, testSide2)
	node.mu.Lock()
	liveTable := node.dms[liveName].table
	node.mu.Unlock()
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))

	reply, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 1, ShowInfo: true,
	}, nil)
	if got := reply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover {
		t.Fatalf("verdict over a blank header = %v, want a leftover", got)
	}

	// The SyncupDn that verdict re-drives, then a new side in a new
	// revision: the disk is not formatted, and the new side gets no extents.
	node.Reset()
	dnReply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if meta := dnReply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "refusing to format") ||
		!strings.Contains(meta.GetDetails(), liveName) {
		t.Errorf("meta_info = %v/%q, want ERROR refusing to format under "+
			"%s", meta.GetStatus(), meta.GetDetails(), liveName)
	}
	if got := dnReply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover {
		t.Errorf("verdict of the refused converge = %v, want a leftover",
			got)
	}
	if _, err := srv.SyncupDn(ctx, dnReq(2, testSide, testSide2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	sideReply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		2, testSide2, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := sideReply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "not formatted") {
		t.Errorf("the new side's side_dev = %v/%q, want ERROR, not "+
			"formatted", got.GetStatus(), got.GetDetails())
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("the disk was written under a live side: %s", call)
		}
		if strings.Contains(call, "dmsetup create "+newName) {
			t.Errorf("the new side was built: %s", call)
		}
		if strings.Contains(call, "dmsetup remove") ||
			strings.Contains(call, "dmsetup reload") {
			t.Errorf("the live stack was touched: %s", call)
		}
	}
	node.mu.Lock()
	live := node.dms[liveName]
	node.mu.Unlock()
	if live == nil || live.table != liveTable {
		t.Errorf("the live side device changed: %+v, want table %q",
			live, liveTable)
	}

	// Both sides leave the DN: the sweep removes the live side's devices,
	// after the converge that still found them and refused, and the
	// verdict re-drives a SyncupDn whose converge formats — once.
	node.Reset()
	dnReply, err = srv.SyncupDn(ctx, dnReq(3))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if !node.hasCall("cmd dmsetup remove " + liveName) {
		t.Error("the sweep did not remove the live side's device")
	}
	if node.hasCall("writeblock") {
		t.Error("formatted while a side device still mapped the disk")
	}
	if got := dnReply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover {
		t.Errorf("verdict with the disk still blank = %v, want a leftover",
			got)
	}
	node.Reset()
	dnReply, err = srv.SyncupDn(ctx, dnReq(3))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if meta := dnReply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("meta_info after the devices went = %v/%q, want OK",
			meta.GetStatus(), meta.GetDetails())
	}
	if got := dnReply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("verdict after the format = %v, want 0", got)
	}
	if got := len(node.callsMatching("writeblock")); got != 2 {
		t.Errorf("%d block writes, want the format's 2", got)
	}
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d", testDisk,
			common.DnTableSlotAOffset),
		fmt.Sprintf("writeblock %s off=%d", testDisk, common.DnHeaderOffset))
}

// blankHeaderWithNoSide leaves a DN whose header was zeroed under a live side
// and whose own side device the sweep has since removed, with seed's devices
// (name to table) on the node from before the header was zeroed: the next
// SyncupDn's converge is the one that decides whether to format the disk
// (DN5).
func blankHeaderWithNoSide(
	t *testing.T,
	seed map[string]string,
) (*DnAgentServer, *fakeNode) {
	t.Helper()
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	node.mu.Lock()
	for name, table := range seed {
		node.dms[name] = &fakeDm{table: table}
	}
	node.mu.Unlock()
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))
	srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 1, ShowInfo: true,
	}, nil)
	// The side leaves the DN: this converge still finds its device and
	// refuses, and the sweep after it removes the device.
	node.Reset()
	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	liveName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	if !node.hasCall("cmd dmsetup remove " + liveName) {
		t.Fatal("the sweep did not remove the side's device")
	}
	if node.hasCall("writeblock") {
		t.Fatal("formatted while the side's device still mapped the disk")
	}
	node.Reset()
	return srv, node
}

// The format gate refuses on a read that did not answer as surely as on a
// device it found (DN5). "Nothing maps the disk" is an absence, read from
// three places that can each fail to answer: the disk's device number, the dm
// listing, and the table of every device whose name could map the disk. Each
// is killed in turn after the sweep has removed this dn's own side, and the
// converge must refuse and write nothing: a killed read taken as "nothing
// maps it" would format a disk that live devices may still map. The next
// converge, every read answering, formats.
func TestABlankHeaderIsNotFormattedWhenTheGateCannotRead(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	// other is another dn's side device over another disk: the gate reads
	// its table, and that table does not map this disk.
	other := nf.DnSideName(testCluster, testDn+7, testSp, testSide)
	for _, tc := range []struct{ name, key string }{
		{"lsblk", "--output MAJ:MIN " + testDisk},
		{"dmsetup ls", "cmd dmsetup ls"},
		{"dmsetup table", "cmd dmsetup table " + other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := blankHeaderWithNoSide(t,
				map[string]string{other: "0 8 linear 7:99 0"})
			ctx := context.Background()
			setHook(node, node.killCmd, tc.key)
			reply, err := srv.SyncupDn(ctx, dnReq(2))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			node.mu.Lock()
			armed := node.killCmd[tc.key]
			node.mu.Unlock()
			if armed {
				t.Fatalf("the gate never ran %q", tc.key)
			}
			if meta := reply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
				pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(meta.GetDetails(), "refusing to format") {
				t.Errorf("meta_info = %v/%q, want ERROR refusing to format",
					meta.GetStatus(), meta.GetDetails())
			}
			if got := reply.GetAgentReply(); got.GetCode() !=
				common.ReplyCodeLeftover {
				t.Errorf("verdict of the refused converge = %v, want a "+
					"leftover", got)
			}
			if node.hasCall("writeblock") {
				t.Error("formatted although a read of the gate did not " +
					"answer")
			}

			node.Reset()
			reply, err = srv.SyncupDn(ctx, dnReq(2))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if meta := reply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
				pb.ResStatus_RES_STATUS_OK {
				t.Errorf("meta_info once every read answers = %v/%q, "+
					"want OK", meta.GetStatus(), meta.GetDetails())
			}
			if got := len(node.callsMatching("writeblock")); got != 2 {
				t.Errorf("%d block writes, want the format's 2", got)
			}
		})
	}
}

// The gate matches every device that can map the disk, not only this dn's
// (DN5): a side device of another dn or of another cluster, or another dn's
// clone-metadata wrapper, over this disk keeps a blank header blank. None of
// them is this dn's sweep's to remove, so the refusal names it, the verdict
// stays a leftover every round, and the disk is formatted only once it has
// gone.
func TestABlankHeaderIsNotFormattedUnderAnotherNodesDevice(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	for _, name := range []string{
		nf.DnSideName(testCluster, testDn+7, testSp, testSide),
		nf.DnSideName(testCluster+1, testDn, testSp, testSide),
		nf.DnMigrMetaDmName(testCluster, testDn+7, testSp, testMigrId),
	} {
		t.Run(name, func(t *testing.T) {
			srv, node := blankHeaderWithNoSide(t, nil)
			ctx := context.Background()
			node.mu.Lock()
			node.dms[name] = &fakeDm{
				table: "0 8 linear " + node.devNo[testDisk] + " 0",
			}
			node.mu.Unlock()
			for round := 0; round < 2; round++ {
				reply, err := srv.SyncupDn(ctx, dnReq(2))
				if err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				if meta := reply.GetDnInfo().GetMetaInfo(); meta.
					GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					!strings.Contains(meta.GetDetails(),
						"refusing to format") ||
					!strings.Contains(meta.GetDetails(), name) {
					t.Errorf("round %d: meta_info = %v/%q, want ERROR "+
						"refusing to format under %s", round,
						meta.GetStatus(), meta.GetDetails(), name)
				}
				if got := reply.GetAgentReply(); got.GetCode() !=
					common.ReplyCodeLeftover {
					t.Errorf("round %d: verdict = %v, want a leftover",
						round, got)
				}
			}
			for _, call := range node.Calls() {
				if strings.HasPrefix(call, "writeblock") {
					t.Errorf("formatted under %s: %s", name, call)
				}
				if strings.Contains(call, "dmsetup remove") {
					t.Errorf("another node's device was removed: %s", call)
				}
			}

			// Once it is gone, the next converge formats.
			node.mu.Lock()
			delete(node.dms, name)
			node.mu.Unlock()
			node.Reset()
			reply, err := srv.SyncupDn(ctx, dnReq(2))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if meta := reply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
				pb.ResStatus_RES_STATUS_OK {
				t.Errorf("meta_info once it has gone = %v/%q, want OK",
					meta.GetStatus(), meta.GetDetails())
			}
			if got := len(node.callsMatching("writeblock")); got != 2 {
				t.Errorf("%d block writes, want the format's 2", got)
			}
		})
	}
}

// [D12]: outside the bounded §11.2 cutover window, no dnv device stays
// suspended. Whatever left one that way — a crash inside a reload's
// suspend/load/resume, or an older build — the next converge must resume it,
// for every dm kind the dn agent owns. (This side has no migr_src_conf, so no
// window applies.)
func TestConvergeResumesSuspendedDevices(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	names := []string{
		nf.DnSideName(testCluster, testDn, testSp, testSide),
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0),
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
	}
	node.mu.Lock()
	for _, name := range names {
		if node.dms[name] == nil {
			node.mu.Unlock()
			t.Fatalf("%s was never created", name)
		}
		node.dms[name].suspended = true
	}
	node.mu.Unlock()

	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, name := range names {
		if node.dms[name].suspended {
			t.Errorf("%s was left suspended", name)
		}
	}
}

// A record is released only once its device is really gone: freeing extents
// that a live device still maps would let the next allocation hand the same
// bytes to a second side.
func TestFailedDeviceRemovalKeepsAllocation(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	node.mu.Lock()
	node.failCmdAlways["dmsetup remove "+sideDevName] = "device-mapper: busy"
	node.mu.Unlock()

	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("the extents were freed while a device still mapped them")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Fatal("the fake removed the device the test told it to refuse")
	}

	// The orphan sweep retries on the next node-level pass, and once the
	// removal succeeds the record goes with it.
	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup remove "+sideDevName)
	node.mu.Unlock()
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok := node.dms[sideDevName]; ok {
		t.Error("the retry did not remove the side device")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the sweep never retried the freed record")
	}
}

// A side whose extents are not contiguous still exports one flat device: the
// aggregate dm-linear concatenates the runs in order, each target's start
// following the previous target's length. This is the only place a
// multi-target table reaches dmsetup, and it travels on stdin because
// `--table` is single-line only.
func TestFragmentedSideBuildsMultiTargetTable(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide, testSide2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	// Carve a hole: allocate two neighbours, free the first, then ask for a
	// side that no single free run can hold.
	total := (node.devSize[testDisk] - common.DnDataOffset) / testExtentSize
	for i := uint64(0); i < total; i++ {
		if _, err := srv.meta.AllocSide(ctx, 0xaa, i, 1); err != nil {
			t.Fatalf("filling: %v", err)
		}
	}
	for i := uint64(1); i < total; i += 2 {
		if err := srv.meta.FreeSide(ctx, 0xaa, i); err != nil {
			t.Fatalf("freeing: %v", err)
		}
	}

	node.Reset()
	syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE))
	rec, ok, err := srv.meta.LookupSide(ctx, testSp, testSide)
	if !ok || err != nil {
		t.Fatalf("no record: %v %v", ok, err)
	}
	if len(rec.GetRunList()) != int(testExtCnt) {
		t.Fatalf("runs = %v, want %d single-extent runs",
			rec.GetRunList(), testExtCnt)
	}

	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	table := node.dms[sideDevName].table
	lines := dmTargets(table)
	if len(lines) != int(testExtCnt) {
		t.Fatalf("table has %d targets, want %d:\n%s",
			len(lines), testExtCnt, table)
	}
	// Every target: contiguous starts, one extent long, and the disk offset
	// the record asks for.
	lenSectors := testExtentSize / agent.SectorSize
	for i, line := range lines {
		want := fmt.Sprintf("%d %d linear 253:0 %d",
			uint64(i)*lenSectors, lenSectors,
			(common.DnDataOffset+
				rec.GetRunList()[i].GetStart()*testExtentSize)/
				agent.SectorSize)
		if line != want {
			t.Errorf("target %d = %q, want %q", i, line, want)
		}
	}
	// The device is the right total size, and the per-CN linear on top sees
	// one flat address space.
	if got := node.devSize[nf.DmPath(sideDevName)]; got !=
		testExtCnt*testExtentSize {
		t.Errorf("side device size = %d, want %d",
			got, testExtCnt*testExtentSize)
	}
	// It went through stdin, not --table.
	if !node.hasCall("cmd dmsetup create " + sideDevName + " stdin=") {
		t.Errorf("the multi-target table did not travel on stdin:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if node.hasCall("cmd dmsetup create " + sideDevName + " --table") {
		t.Error("a multi-target table was passed with --table")
	}

	// And the probe agrees the live table matches the record.
	reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev_info = %v/%q", got.GetStatus(), got.GetDetails())
	}
}

// probeSideDev's two error paths: a side that has not been discarded yet is
// never exported, and a live table that disagrees with the record is an error.
func TestProbeSideDevErrorPaths(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	getInfo := func() *pb.ResInfo {
		t.Helper()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		return reply.GetSideInfo().GetSideDevInfo()
	}

	// A table that no longer matches the record.
	node.mu.Lock()
	node.dms[sideDevName].table = "0 8 linear 253:0 0"
	node.mu.Unlock()
	got := getInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "extent run") {
		t.Errorf("mismatched table -> %v/%q, want ERROR/extent run",
			got.GetStatus(), got.GetDetails())
	}
	// The converge repairs it by reloading the aggregate table.
	node.Reset()
	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.hasCall("cmd dmsetup reload " + sideDevName) {
		t.Error("the mismatched side table was not reloaded")
	}
	if got := getInfo(); got.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("after repair: %v/%q", got.GetStatus(), got.GetDetails())
	}

	// The device gone entirely.
	node.mu.Lock()
	delete(node.dms, sideDevName)
	node.mu.Unlock()
	if got := getInfo(); got.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("missing device -> %v, want MISSING", got.GetStatus())
	}

	// A side that is not fully zeroed is never exported: the converge stops
	// before the nvmet layer and reports through the rest of the side's info.
	srv2, node2 := newTestServer(t)
	if _, err := srv2.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node2.mu.Lock()
	node2.failCmdAlways["blkdiscard --zeroout"] = "zeroout failed"
	node2.mu.Unlock()
	reply, err := srv2.SyncupSide(ctx, sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("unzeroed side_dev = %v, want ERROR", got)
	}
	// Probing the exports is fine; creating one is not.
	if node2.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("an unzeroed side was exported")
	}
	for _, call := range node2.Mutations() {
		if strings.Contains(call, "/namespaces/1/enable") {
			t.Errorf("an unzeroed side enabled a namespace: %s", call)
		}
	}
	// The per-CN stacks are still reported, so the operator sees the whole
	// picture (probeAboveSideDev on the not-ready path).
	if len(reply.GetSideInfo().GetCnIdToDmError()) == 0 {
		t.Error("the not-ready path reported nothing above the side device")
	}
}

// startPartialZeroing brings a side to the row-2 shape and *holds* it there:
// the first batch lands, the second is parked inside its child, so the record
// stays at DnZeroBatchExtCnt of testExtCnt and the loop publishes no zeroErr.
// The returned function releases the child.
func startPartialZeroing(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
) func() {
	t.Helper()
	ctx := context.Background()
	batch1 := "cmd blkdiscard --zeroout --offset " + strconv.FormatUint(
		common.DnZeroBatchExtCnt*testExtentSize, 10)
	node.blockCmd(batch1)
	release := func() { node.releaseCmd(batch1) }
	t.Cleanup(release)
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
		return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
	}) {
		t.Fatal("the first batch never landed")
	}
	return release
}

// A side whose dm-linear is missing or carries the wrong table must say so,
// even while it is still being zeroed: DN18 judges side_dev_info by "the
// volume-table record + its zeroed_bits + `dmsetup table`", and a probe that
// skips the device check reports healthy PROVISIONING for ever. PROVISIONING
// never feeds err_epoch (§9.5), so nothing would ever re-send the SyncupSide
// that is the only thing able to rebuild the device — the side would be
// bricked and silent.
func TestProbeReportsABrokenDeviceWhileZeroing(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	startPartialZeroing(t, srv, node)

	getInfo := func() *pb.ResInfo {
		t.Helper()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		return reply.GetSideInfo().GetSideDevInfo()
	}

	// A healthy provisioning side still reports its progress, unchanged.
	node.Reset()
	if got := getInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Fatalf("side_dev while zeroing = %v/%q, want PROVISIONING",
			got.GetStatus(), got.GetDetails())
	}

	// A live table that does not match the record's extent runs.
	node.mu.Lock()
	node.dms[sideDevName].table = "0 8 linear 253:0 0"
	node.mu.Unlock()
	got := getInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "extent run") {
		t.Errorf("mismatched table while zeroing -> %v/%q, want ERROR/extent "+
			"run", got.GetStatus(), got.GetDetails())
	}

	// The device gone entirely — the ensureSideDm-failed shape, which no
	// PROVISIONING row would ever get retried.
	node.mu.Lock()
	delete(node.dms, sideDevName)
	node.mu.Unlock()
	if got := getInfo(); got.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("missing device while zeroing -> %v/%q, want MISSING",
			got.GetStatus(), got.GetDetails())
	}
	for _, call := range node.Mutations() {
		t.Errorf("a probe mutated the node: %s", call)
	}
}

// probeSideDev must bind the published batch error ONCE. setZeroingErr runs
// from the zeroing loop after release() — outside both DN1 locks — so a probe
// that tests the accessor and then re-reads it to format the details can see
// nil the second time and call .Error() on a nil error. That is a panic inside
// a gRPC handler, and nothing in this repo installs a recovery interceptor, so
// it takes the whole dn agent down with every side it hosts. `go test -race`
// cannot see it: both reads are correctly mutex-guarded, only the timing is
// wrong (DN9).
func TestProbeSideDevBindsTheZeroingErrorOnce(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	startPartialZeroing(t, srv, node)
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if st == nil {
		t.Fatal("the side state vanished")
	}
	// The parked batch publishes nothing, so this test is the only writer.
	plan := newSidePlan(srv.nf, st.req, testExtentSize)

	stop := make(chan struct{})
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		batchErr := fmt.Errorf("blkdiscard --zeroout: exit status 1")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				srv.setZeroingErr(st, batchErr)
			} else {
				srv.setZeroingErr(st, nil)
			}
		}
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("probeSideDev panicked while the zeroing loop "+
					"cleared its error: %v", r)
			}
		}()
		for i := 0; i < 20000; i++ {
			srv.probeSideDev(ctx, st, plan, &pb.SideInfo{})
			if i%1000 == 0 {
				// The probe records three dm reads per round; recording all
				// of them would be the only thing this test measured.
				node.Reset()
			}
		}
	}()
	close(stop)
	<-flipped
	srv.setZeroingErr(st, nil)
}
