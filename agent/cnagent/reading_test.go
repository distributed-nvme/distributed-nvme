package cnagent

import (
	"context"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestReadingReadsAMissingNameOnce pins the reading's own reads (CN28): a
// name a partial listing lacks is read on its own at its first lookup, and
// that answer, an error included, serves every later lookup of the call, so
// no device is read twice; a name a complete info listing lacks is not
// there, with no read at all; a listed name, or a listing that did not
// answer, costs no read either. A check round looks no device's table up
// twice, so only this test proves the table cache. The readings are taken
// from the fake node (takeReading), whose listings leave out the names the
// test omits, as dmsetup's do for a device that goes away mid-listing.
func TestReadingReadsAMissingNameOnce(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	missing := raid0Name(srv, testTd)
	killed := errorName(srv, testTd)
	listed := thinName(srv, testTd)
	const absent = "dnv-test-not-there"
	node.mu.Lock()
	missingNo := node.devNo["/dev/mapper/"+missing]
	missingTable := common.ParseDmLines(node.dms[missing].table)
	listedNo := node.devNo["/dev/mapper/"+listed]
	listedTable := common.ParseDmLines(node.dms[listed].table)
	for _, name := range []string{missing, killed} {
		node.infoOmit[name] = true
		node.tableOmit[name] = true
	}
	node.mu.Unlock()
	rd := srv.takeReading(ctx)
	if rd.infoErr != nil || rd.tableErr != nil || !rd.infoList.Partial ||
		!rd.tableList.Partial {
		t.Fatalf("fixture: the listings read (%v, %v), want both partial",
			rd.infoErr, rd.tableErr)
	}
	if _, ok := rd.infoList.Devs[missing]; ok {
		t.Fatalf("fixture: the info listing names %s", missing)
	}

	node.Reset()
	for i := 0; i < 2; i++ {
		dev, err := rd.info(ctx, missing)
		if err != nil || dev == nil || dev.DevNo != missingNo {
			t.Fatalf("lookup %d: info of %s read (%+v, %v), want its number "+
				"%s", i+1, missing, dev, err, missingNo)
		}
		targets, err := rd.table(ctx, missing)
		if err != nil || len(targets) == 0 ||
			!reflect.DeepEqual(targets, missingTable) {
			t.Fatalf("lookup %d: table of %s read (%v, %v), want %v",
				i+1, missing, targets, err, missingTable)
		}
		if dev, err := rd.info(ctx, listed); err != nil || dev == nil ||
			dev.DevNo != listedNo {
			t.Fatalf("lookup %d: the listed %s read (%+v, %v), want its "+
				"listed line", i+1, listed, dev, err)
		}
		if targets, err := rd.table(ctx, listed); err != nil ||
			!reflect.DeepEqual(targets, listedTable) {
			t.Fatalf("lookup %d: the listed table of %s read (%v, %v), "+
				"want its listed record %v", i+1, listed, targets, err,
				listedTable)
		}
	}
	if got := countCalls(node, "cmd "+dmNumberRead+missing); got != 1 {
		t.Errorf("%s's own info ran %d times, want 1", missing, got)
	}
	if got := countCalls(node, "cmd dmsetup table "+missing); got != 1 {
		t.Errorf("%s's own table ran %d times, want 1", missing, got)
	}
	if got := len(node.Calls()); got != 2 {
		t.Errorf("the lookups made %d reads, want 2: %v", got, node.Calls())
	}

	// An own read that did not answer is kept for the call as well: the
	// second lookup gives the same error and runs nothing, though the kill
	// is spent and a new read would answer.
	node.mu.Lock()
	node.killCmd[dmNumberRead+killed] = true
	node.killCmd["dmsetup table "+killed] = true
	node.mu.Unlock()
	node.Reset()
	var infoErrs, tableErrs []string
	for i := 0; i < 2; i++ {
		if _, err := rd.info(ctx, killed); err != nil {
			infoErrs = append(infoErrs, err.Error())
		}
		if _, err := rd.table(ctx, killed); err != nil {
			tableErrs = append(tableErrs, err.Error())
		}
	}
	if len(infoErrs) != 2 || infoErrs[0] != infoErrs[1] ||
		!strings.Contains(infoErrs[0], "signal: killed") {
		t.Errorf("a killed own info read %v, want the one error twice",
			infoErrs)
	}
	if len(tableErrs) != 2 || tableErrs[0] != tableErrs[1] ||
		!strings.Contains(tableErrs[0], "signal: killed") {
		t.Errorf("a killed own table read %v, want the one error twice",
			tableErrs)
	}
	if got := len(node.Calls()); got != 2 {
		t.Errorf("the killed lookups made %d reads, want 2: %v",
			got, node.Calls())
	}

	// A complete info listing that lacks a name: not there, and no read.
	// A complete table listing that lacks a name the info listing holds —
	// a device gone between the two listings (lsGhosts) — gets the table's
	// own read all the same, and its answer is the read's.
	ghost := names(srv).CnGrpName(testCluster, testCn, testSp+1, 0x66)
	node.mu.Lock()
	clear(node.infoOmit)
	clear(node.tableOmit)
	node.lsGhosts = append(node.lsGhosts, ghost)
	node.mu.Unlock()
	complete := srv.takeReading(ctx)
	if complete.infoErr != nil || complete.infoList.Partial ||
		complete.tableErr != nil || complete.tableList.Partial {
		t.Fatalf("fixture: the listings read (%v, %v), want both complete",
			complete.infoErr, complete.tableErr)
	}
	if _, ok := complete.infoList.Devs[ghost]; !ok {
		t.Fatalf("fixture: the info listing does not name %s", ghost)
	}
	node.Reset()
	if dev, err := complete.info(ctx, absent); dev != nil || err != nil {
		t.Errorf("a name a complete listing lacks read (%+v, %v), want "+
			"absent", dev, err)
	}
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("a name a complete listing lacks was read: %v", calls)
	}
	if targets, err := complete.table(ctx, ghost); err == nil {
		t.Errorf("the table of %s, gone since the info listing, read %v, "+
			"want its own read's error", ghost, targets)
	}
	if got := countCalls(node, "cmd dmsetup table "+ghost); got != 1 {
		t.Errorf("%s's own table ran %d times, want 1", ghost, got)
	}

	// Listings that did not answer: their error, and no read.
	node.mu.Lock()
	node.killCmd[strings.TrimPrefix(dmInfoListLine, "cmd ")] = true
	node.killCmd[strings.TrimPrefix(dmTableListLine, "cmd ")] = true
	node.mu.Unlock()
	unanswered := srv.takeReading(ctx)
	if unanswered.infoErr == nil || unanswered.tableErr == nil {
		t.Fatalf("fixture: the killed listings read (%v, %v), want both "+
			"errors", unanswered.infoErr, unanswered.tableErr)
	}
	node.Reset()
	if _, err := unanswered.info(ctx, missing); err == nil ||
		err.Error() != unanswered.infoErr.Error() {
		t.Errorf("an info listing that did not answer gave %v, want %v",
			err, unanswered.infoErr)
	}
	if _, err := unanswered.table(ctx, missing); err == nil ||
		err.Error() != unanswered.tableErr.Error() {
		t.Errorf("a table listing that did not answer gave %v, want %v",
			err, unanswered.tableErr)
	}
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("lookups that need no read ran %v", calls)
	}
}

// TestDmDevNoSameOnBothChannels pins that a dm device's number reads the
// same with a reading and without one (CN28): the device's number, or one
// error text for a device that is not there and for one listed with numbers
// that do not parse. dmDevNo makes those checks for both channels, so a check
// round and a converge can never write two texts for one device.
func TestDmDevNoSameOnBothChannels(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	present := raid0Name(srv, testTd)
	const absent = "dnv-test-not-there"
	const unnumbered = "dnv-test-no-number"
	node.mu.Lock()
	presentNo := node.devNo["/dev/mapper/"+present]
	// A device the node holds no number for, which dmsetup prints with
	// empty number fields: "name///attr", in the listing and on its own.
	node.dms[unnumbered] = &fakeDm{table: "0 8 error"}
	node.mu.Unlock()
	rd := srv.takeReading(ctx)
	if rd.infoErr != nil || rd.infoList.Partial {
		t.Fatalf("fixture: the info listing read (%v), want it complete",
			rd.infoErr)
	}
	if dev := rd.infoList.Devs[unnumbered]; dev == nil || dev.DevNo != "" {
		t.Fatalf("fixture: the listing reads %s as %+v, want it listed "+
			"with no number", unnumbered, dev)
	}
	for _, tc := range []struct {
		name    string
		wantNo  string
		wantErr string
	}{
		{present, presentNo, ""},
		{absent, "", "dm device " + absent + " is not there"},
		{unnumbered, "",
			"dm device " + unnumbered + " has no device number in dmsetup info"},
	} {
		for _, channel := range []struct {
			label string
			rd    *cnReading
		}{{"one dmsetup info", nil}, {"the reading", rd}} {
			devNo, err := srv.dmDevNo(ctx, channel.rd, tc.name)
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if devNo != tc.wantNo || gotErr != tc.wantErr {
				t.Errorf("%s from %s: (%q, %q), want (%q, %q)", tc.name,
					channel.label, devNo, gotErr, tc.wantNo, tc.wantErr)
			}
		}
	}
}

// TestBlockDevNoReadsTheSysBlockNumber pins blockDevNo, where the number of
// an md array, an NVMe multipath head and a loop device comes from: its
// /sys/block/<kname>/dev, one read. A device whose file is not there and a
// value that is not a device number are errors, and so is a read that failed
// or was cut off: never an absence, never a number.
func TestBlockDevNoReadsTheSysBlockNumber(t *testing.T) {
	ctx := context.Background()
	node := newFakeNode()
	srv := newCnServer(node)
	node.mu.Lock()
	node.files[sysfsBlockDir+"/nvme3n1/dev"] = "259:3\n"
	node.files[sysfsBlockDir+"/odd/dev"] = "259:x\n"
	node.files[sysfsBlockDir+"/broken/dev"] = "259:4\n"
	node.files[sysfsBlockDir+"/stalled/dev"] = "259:5\n"
	node.failReadAlways[sysfsBlockDir+"/broken/dev"] = true
	node.killReadAlways[sysfsBlockDir+"/stalled/dev"] = true
	node.mu.Unlock()

	if devNo, err := srv.blockDevNo(ctx, "nvme3n1"); err != nil ||
		devNo != "259:3" {
		t.Errorf("nvme3n1 read (%q, %v), want 259:3", devNo, err)
	}
	for _, tc := range []struct {
		kname string
		want  string
	}{
		{"nvme9n1", "block device nvme9n1 is not there"},
		{"odd", sysfsBlockDir + `/odd/dev reads "259:x", not a device number`},
		{"broken", "read " + sysfsBlockDir + "/broken/dev: input/output error"},
		{"stalled", "read " + sysfsBlockDir + "/stalled/dev: " +
			context.DeadlineExceeded.Error()},
	} {
		devNo, err := srv.blockDevNo(ctx, tc.kname)
		if err == nil || err.Error() != tc.want || devNo != "" {
			t.Errorf("%s read (%q, %v), want the error %q",
				tc.kname, devNo, err, tc.want)
		}
	}
	for _, kname := range []string{"nvme3n1", "nvme9n1", "odd", "broken"} {
		if got := countCalls(
			node, "read "+sysfsBlockDir+"/"+kname+"/dev"); got != 1 {
			t.Errorf("%s's number was read %d times, want 1", kname, got)
		}
	}
	if got := node.SysfsNoDeadline(); len(got) != 0 {
		t.Errorf("number reads without the soft timeout: %v", got)
	}
}

// infoRows is every row of a CntlrInfo, keyed "<kind>[<id>]", the kind as
// errorRows names it.
func infoRows(info *pb.CntlrInfo) map[string]*pb.ResInfo {
	out := make(map[string]*pb.ResInfo)
	add := func(kind string, rows map[uint64]*pb.ResInfo) {
		for id, row := range rows {
			out[fmt.Sprintf("%s[%#x]", kind, id)] = row
		}
	}
	add("ss", info.GetSsIdToSubsystem())
	add("ns", info.GetNsIdToNamespace())
	add("ns_dev", info.GetNsIdToDmLinear())
	add("raid0", info.GetTdIdToRaid0())
	add("td_error", info.GetTdIdToDmError())
	add("pool", info.GetSliceIdToDmPool())
	add("pool_meta", info.GetSliceIdToMeta())
	add("pool_data", info.GetSliceIdToData())
	add("grp", info.GetGrpIdToMdRaid())
	add("leg", info.GetLegIdToLeg())
	add("xfer_dm", info.GetXferIdToDmLinear())
	add("xfer_ss", info.GetXferIdToSubsystem())
	add("xfer_ns", info.GetXferIdToNamespace())
	add("clone_tgt", info.GetCloneIdToTarget())
	add("clone_dm", info.GetCloneIdToDmClone())
	add("clone_meta", info.GetCloneIdToMeta())
	for tdId, thin := range info.GetTdIdToThinInfo() {
		add(fmt.Sprintf("thin[td %#x]", tdId), thin.GetSliceIdToDmThin())
	}
	return out
}

// rowKind is the kind part of an infoRows key.
func rowKind(key string) string {
	kind, _, _ := strings.Cut(key, "[")
	return kind
}

// assertRowsOk fails on every row of info that does not read OK.
func assertRowsOk(t *testing.T, info *pb.CntlrInfo, label string) {
	t.Helper()
	rows := infoRows(info)
	if len(rows) == 0 {
		t.Fatalf("%s: no rows at all", label)
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if row := rows[key]; row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("%s: %s reads %v %q, want OK", label, key,
				row.GetStatus(), row.GetDetails())
		}
	}
}

// readEntrance is one of the two read-only calls that take a reading: a
// check round or an info read of the fixture cntlr, its verdict and its
// rows.
type readEntrance struct {
	name string
	read func(t *testing.T, srv *CnAgentServer) (*pb.AgentReply, *pb.CntlrInfo)
}

var readEntrances = []readEntrance{
	{"a check round",
		func(t *testing.T, srv *CnAgentServer) (*pb.AgentReply, *pb.CntlrInfo) {
			t.Helper()
			req := srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
				testCntlr)).loadReq()
			reply, info := srv.checkCntlrRound(context.Background(),
				&pb.CheckCntlrRequest{ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(), Revision: req.GetRevision(),
					ShowInfo: true}, nil)
			if info == nil {
				t.Fatalf("the check round carried no CntlrInfo")
			}
			return reply.GetAgentReply(), info
		}},
	{"an info read",
		func(t *testing.T, srv *CnAgentServer) (*pb.AgentReply, *pb.CntlrInfo) {
			t.Helper()
			reply, err := srv.GetCntlrInfo(context.Background(),
				&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr()})
			if err != nil {
				t.Fatalf("GetCntlrInfo: %v", err)
			}
			return reply.GetAgentReply(), reply.GetCntlrInfo()
		}},
}

// perDeviceDmReads is every dmsetup info or dmsetup table of the call log
// that is not one of the reading's two whole-node listings: a read of one
// device.
func perDeviceDmReads(node *fakeNode) []string {
	var out []string
	for _, call := range node.Calls() {
		if (strings.HasPrefix(call, "cmd dmsetup info") &&
			call != dmInfoListLine) ||
			(strings.HasPrefix(call, "cmd dmsetup table") &&
				call != dmTableListLine) {
			out = append(out, call)
		}
	}
	return out
}

// TestCheckRoundReadsTheNodeOnce pins what a check round and an info read
// cost (CN28): one reading, whatever the slice count. On a primary and on a
// standby of 2 and of 8 slices — 8 and 32 legs at raid1 — each call lists
// /sys/class/nvme-subsystem once and /sys/block once, runs one whole-node
// `dmsetup info` and one `dmsetup table`, reads no device on its own with
// either, and runs no lsblk and no `dmsetup ls`; its status reads are one
// per thin pool on the primary and none on the standby, and its configfs
// listings do not grow with the slices. The cntlr holds a transfer, whose
// device the primary serves over the namespace's raid0 and the standby
// keeps over an error table, so both of its forms come from the reading
// too. Every row of the node, which matches its desired state, reads OK,
// and so does the verdict. The primary's legs have each had one successful
// probe round, the steady state in which its leg rows read OK.
func TestCheckRoundReadsTheNodeOnce(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
		OriNsIdx: 1, AllowedHosts: []string{testHostNqn}}}
	for _, primary := range []bool{true, false} {
		role := "standby"
		if primary {
			role = "primary"
		}
		configfsLists := make(map[string]int)
		for _, slices := range []int{2, 8} {
			t.Run(fmt.Sprintf("%s, %d slices", role, slices), func(t *testing.T) {
				srv, node := newTestServer(t)
				syncupBoth(t, srv, reqOpts{revision: 2, primary: primary,
					raid1: true, slices: slices, xfers: xfers})
				if primary {
					probeAllLegs(t, srv)
				}
				form := "error"
				if primary {
					form = "linear"
				}
				node.mu.Lock()
				var xferTargets []common.DmTarget
				if xfer := node.dms[xferName(srv, testXfer)]; xfer != nil {
					xferTargets = common.ParseDmLines(xfer.table)
				}
				node.mu.Unlock()
				if len(xferTargets) != 1 || xferTargets[0].Type != form {
					t.Fatalf("fixture: the transfer device's table is %v, "+
						"want the %s form", xferTargets, form)
				}
				for _, entrance := range readEntrances {
					node.Reset()
					verdict, info := entrance.read(t, srv)
					label := entrance.name
					if got := len(info.GetLegIdToLeg()); got != 4*slices {
						t.Fatalf("%s: %d leg rows, want %d", label, got,
							4*slices)
					}
					if info.GetXferIdToDmLinear()[testXfer] == nil {
						t.Fatalf("%s: no transfer device row", label)
					}
					for _, listing := range []string{
						"list " + sysfsNvmeSubsysDir, "list " + sysfsBlockDir,
						dmInfoListLine, dmTableListLine,
					} {
						if got := countCalls(node, listing); got != 1 {
							t.Errorf("%s: %q ran %d times, want 1", label,
								listing, got)
						}
					}
					if reads := perDeviceDmReads(node); len(reads) != 0 {
						t.Errorf("%s read devices on their own: %q", label,
							reads)
					}
					for _, call := range []string{
						"cmd lsblk", "cmd dmsetup ls"} {
						if node.hasCall(call) {
							t.Errorf("%s ran %q", label, call)
						}
					}
					wantStatus := 0
					if primary {
						wantStatus = slices
					}
					if got := callCnt(node, "cmd dmsetup status "); got !=
						wantStatus {
						t.Errorf("%s: %d status reads, want %d", label, got,
							wantStatus)
					}
					lists := callCnt(node, "cmd ls ")
					if first, seen := configfsLists[entrance.name]; !seen {
						configfsLists[entrance.name] = lists
					} else if lists != first {
						t.Errorf("%s: %d configfs listings at %d slices, "+
							"%d at 2", label, lists, slices, first)
					}
					assertRowsOk(t, info, label)
					if verdict.GetCode() != 0 {
						t.Errorf("%s: the verdict is %v, want clean", label,
							verdict)
					}
				}
			})
		}
	}
}

// TestUnansweredDmListingErrsEveryDeviceRow pins a whole-node dm listing
// that did not answer (CN28, CN30): killed at the soft timeout, or refused
// with nothing printed. Every row whose first dm read goes to that listing —
// for the info listing a device's attrs or number, for the table listing a
// device's table, which every dm row reads once its device is there — reads
// ERROR with the listing's error, and the verdict names the listing. The
// fixtures are two raid1 primaries, one serving a transfer and one holding a
// clone. The clone metadata area takes its wrappers' names from the info
// listing and their tables from the table listing, so with either listing
// unanswered the area did not answer: the clone's metadata row reads ERROR
// with the area's error, which wraps the listing's, and its dm-clone row
// reads ERROR beside it (cloneMetaVerdict). The md group rows, read from the
// md walk, the export rows, read from configfs, and the clone's source row,
// read from the NVMe walk, read OK. Read as an empty, complete listing, the
// info listing would turn every dm row MISSING, which sets no health epoch,
// and a clean verdict would hide that the listing told the call nothing.
func TestUnansweredDmListingErrsEveryDeviceRow(t *testing.T) {
	infoKey := strings.TrimPrefix(dmInfoListLine, "cmd ")
	tableKey := strings.TrimPrefix(dmTableListLine, "cmd ")
	// failed gives, by kind, the details a row the listing turns ERROR
	// reads, from the listing's error: a dm row's are that error itself, the
	// clone metadata row's are the area's error, which wraps it, and the
	// dm-clone row's are the text of the clone's ERROR pair, which the
	// dm-clone's status line follows on a line of its own when its own
	// attrs answered (rowDetailsMatch).
	same := func(want string) string { return want }
	failed := map[string]func(want string) string{
		"leg": same, "pool_meta": same, "pool_data": same, "pool": same,
		"thin": same, "td_error": same, "raid0": same, "ns_dev": same,
		"xfer_dm": same,
		"clone_meta": func(want string) string {
			return "clone-metadata arena unavailable: " + want
		},
		"clone_dm": func(string) string { return detailsCloneMetaMissing },
	}
	rowDetailsMatch := func(kind, details, want string) bool {
		return details == want ||
			(kind == "clone_dm" && strings.HasPrefix(details, want+"\n"))
	}
	devRows := []string{"leg", "pool_meta", "pool_data", "pool", "thin",
		"td_error", "raid0", "ns_dev"}
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
		OriNsIdx: 1, AllowedHosts: []string{testHostNqn}}}
	for _, fixture := range []struct {
		name string
		opts reqOpts
		// kinds are the kinds of row the fixture must hold, so that no
		// case is vacuous: those the listing turns ERROR and those it
		// leaves OK.
		kinds []string
	}{
		{"a served transfer",
			reqOpts{revision: 2, primary: true, raid1: true, xfers: xfers},
			append(slices.Clone(devRows), "xfer_dm", "grp", "ss", "ns",
				"xfer_ss", "xfer_ns")},
		{"a built clone",
			reqOpts{revision: 2, primary: true, raid1: true,
				clones: []*pb.Clone{cloneOf()}},
			append(slices.Clone(devRows), "clone_meta", "clone_dm", "grp",
				"ss", "ns", "clone_tgt")},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				arm     func(node *fakeNode)
				listing string // the verdict's name for it
				want    string // the listing's error
			}{
				{"the info listing killed",
					func(node *fakeNode) { node.killCmd[infoKey] = true },
					"dmsetup info", infoKey + ": signal: killed"},
				{"the info listing refused",
					func(node *fakeNode) {
						node.failCmd[infoKey] = "fake: refused"
					},
					"dmsetup info", infoKey + ": fake: refused"},
				{"the table listing killed",
					func(node *fakeNode) { node.killCmd[tableKey] = true },
					"dmsetup table", tableKey + ": signal: killed"},
				{"the table listing refused",
					func(node *fakeNode) {
						node.failCmd[tableKey] = "fake: refused"
					},
					"dmsetup table", tableKey + ": fake: refused"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					srv, node := newTestServer(t)
					syncupBoth(t, srv, fixture.opts)
					probeAllLegs(t, srv)
					for _, entrance := range readEntrances {
						// After the last converge: the info listing's key
						// is also the start of every per-device read with
						// a number, which a converge makes.
						node.mu.Lock()
						tc.arm(node)
						node.mu.Unlock()
						node.Reset()
						verdict, info := entrance.read(t, srv)
						label := entrance.name
						node.mu.Lock()
						armed := len(node.killCmd) + len(node.failCmd)
						node.mu.Unlock()
						if armed != 0 {
							t.Fatalf("%s: the listing's fault never fired",
								label)
						}
						kinds := make(map[string]bool)
						for key, row := range infoRows(info) {
							kind := rowKind(key)
							kinds[kind] = true
							details, status := row.GetDetails(),
								row.GetStatus()
							if wantOf, ok := failed[kind]; ok {
								want := wantOf(tc.want)
								if status != pb.ResStatus_RES_STATUS_ERROR ||
									!rowDetailsMatch(kind, details, want) {
									t.Errorf("%s: %s reads %v %q, want "+
										"ERROR %q", label, key, status,
										details, want)
								}
								continue
							}
							if status != pb.ResStatus_RES_STATUS_OK {
								t.Errorf("%s: %s reads %v %q, want OK",
									label, key, status, details)
							}
						}
						for _, kind := range fixture.kinds {
							if !kinds[kind] {
								t.Fatalf("%s: no %s row: the case is "+
									"vacuous", label, kind)
							}
						}
						if verdict.GetCode() != common.ReplyCodeLeftover ||
							!strings.Contains(verdict.GetDetails(),
								"enumeration failed: "+tc.listing+": "+
									tc.want) {
							t.Errorf("%s: the verdict is %v, want a "+
								"Leftover naming %s", label, verdict,
								tc.listing)
						}
						other := "dmsetup table"
						if tc.listing == other {
							other = "dmsetup info"
						}
						if strings.Contains(verdict.GetDetails(),
							other+":") {
							t.Errorf("%s: the verdict %q names %s, which "+
								"answered", label, verdict.GetDetails(),
								other)
						}
					}
				})
			}
		})
	}
}

// plantForeignDm puts a dm device of another pool on the node, and has both
// whole-node listings leave it out, as dmsetup's do for a device that goes
// away while they run: each then exits 1 after printing every other device.
func plantForeignDm(t *testing.T, srv *CnAgentServer, node *fakeNode) string {
	t.Helper()
	foreign := names(srv).CnGrpName(testCluster, testCn, testSp+1, 0x77)
	node.mu.Lock()
	node.dms[foreign] = &fakeDm{table: "0 8 error"}
	node.devNo["/dev/mapper/"+foreign] = node.newDevNo()
	node.infoOmit[foreign] = true
	node.tableOmit[foreign] = true
	node.mu.Unlock()
	info, infoErr := srv.dm.ListInfo(context.Background())
	tables, tableErr := srv.dm.ListTables(context.Background())
	if infoErr != nil || tableErr != nil || !info.Partial || !tables.Partial {
		t.Fatalf("fixture: the listings beside %s read (%v, %v), want both "+
			"partial", foreign, infoErr, tableErr)
	}
	return foreign
}

// TestPartialDmListingStillAnswers pins a whole-node dm listing that exits
// non-zero after printing lines that parse (CN28, CN30): dmsetup does so
// whenever a device goes away while it lists, a device of another pool or
// of a dn agent on the same machine as often as not. Such a listing answers.
// A device this cntlr wants that it lacks is read on its own, once, at its
// first lookup, and the answer serves every later lookup of the call; every
// other device's facts come from the listing. Read as "did not answer", the
// listing would turn every dm row ERROR, which sets the health epoch, for a
// device of someone else's.
func TestPartialDmListingStillAnswers(t *testing.T) {
	t.Run("another pool's device left out", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
		probeAllLegs(t, srv)
		plantForeignDm(t, srv, node)
		for _, entrance := range readEntrances {
			node.Reset()
			verdict, info := entrance.read(t, srv)
			assertRowsOk(t, info, entrance.name)
			if reads := perDeviceDmReads(node); len(reads) != 0 {
				t.Errorf("%s read devices on their own: %q", entrance.name,
					reads)
			}
			if verdict.GetCode() != 0 {
				t.Errorf("%s: the verdict is %v, want clean", entrance.name,
					verdict)
			}
		}
	})

	t.Run("its own thin volume and pool left out", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
		probeAllLegs(t, srv)
		omitted := []string{thinName(srv, testTd), poolName(srv)}
		node.mu.Lock()
		for _, name := range omitted {
			node.infoOmit[name] = true
		}
		node.mu.Unlock()
		for _, entrance := range readEntrances {
			node.Reset()
			verdict, info := entrance.read(t, srv)
			assertRowsOk(t, info, entrance.name)
			// The thin row and the raid0 row both look the thin volume up,
			// and the pool row and the thin row both the pool.
			for _, name := range omitted {
				if got := countCalls(node, "cmd "+dmNumberRead+name); got != 1 {
					t.Errorf("%s read %s on its own %d times, want 1",
						entrance.name, name, got)
				}
			}
			if reads := perDeviceDmReads(node); len(reads) != len(omitted) {
				t.Errorf("%s read %q on their own, want only %v",
					entrance.name, reads, omitted)
			}
			if verdict.GetCode() != 0 {
				t.Errorf("%s: the verdict is %v, want clean", entrance.name,
					verdict)
			}
		}
	})

	t.Run("an unfinished build", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		node.failCmdAlways["nvme connect"] = "fake: connection refused"
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
		plantForeignDm(t, srv, node)
		thin := thinName(srv, testTd)
		// The devices a row looks up, none of which the build reached. The
		// raid0 is not among them: its row stops at the thin volume.
		absent := []string{legName(srv, testMetaLeg),
			legName(srv, testDataLeg), poolName(srv), thin,
			names(srv).CnPoolMetaName(testCluster, testCn, testSp, testSlice),
			names(srv).CnPoolDataName(testCluster, testCn, testSp, testSlice),
			nsDevName(srv, testNs)}
		node.mu.Lock()
		for _, name := range absent {
			if _, built := node.dms[name]; built {
				node.mu.Unlock()
				t.Fatalf("fixture: the build reached %s", name)
			}
		}
		node.mu.Unlock()
		for _, entrance := range readEntrances {
			node.Reset()
			_, info := entrance.read(t, srv)
			label := entrance.name
			for _, name := range absent {
				if got := countCalls(node, "cmd "+dmNumberRead+name); got != 1 {
					t.Errorf("%s read %s on its own %d times, want 1",
						label, name, got)
				}
			}
			reads := perDeviceDmReads(node)
			if len(reads) != len(absent) {
				t.Errorf("%s read %q on their own, want one read of each "+
					"of %v", label, reads, absent)
			}
			// Each row whose first read is its own device's info.
			seen := make(map[string]bool)
			for key, row := range infoRows(info) {
				switch kind := rowKind(key); kind {
				case "leg", "pool_meta", "pool_data", "pool", "thin",
					"ns_dev":
					seen[kind] = true
					assertMissing(t, row, label+" "+key)
				}
			}
			if len(seen) != 6 {
				t.Fatalf("%s: rows of %v only: the case is vacuous", label,
					seen)
			}
			// The raid0 row's first read is the thin volume's number.
			assertErrorDetails(t, info.GetTdIdToRaid0()[testTd],
				"dm device "+thin+" is not there", label+" raid0")
		}
	})

	// The wrapper's own read decides the clone metadata row. A read that
	// answers finds the wrapper there, and the row reads OK, as the
	// converge's `dmsetup ls` does. A read that did not answer leaves the
	// clone metadata area unread: the row reads ERROR naming that read, and
	// the dm-clone row reads ERROR beside it (cloneMetaVerdict), never the
	// MISSING of a wrapper that is not there, which sets no health epoch.
	t.Run("its clone metadata wrapper left out", func(t *testing.T) {
		srv, node := newTestServer(t)
		reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{cloneOf()}})
		assertOk(t, reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
			"the converge's clone meta row")
		wrapper := cloneMetaName(srv, testClone)
		ownRead := dmNumberRead + wrapper
		node.mu.Lock()
		node.infoOmit[wrapper] = true
		node.mu.Unlock()
		for _, killed := range []bool{false, true} {
			for _, entrance := range readEntrances {
				label := entrance.name
				if killed {
					label += ", the wrapper's own read killed"
					node.mu.Lock()
					// One-shot: the call reads the wrapper on its own once.
					node.killCmd[ownRead] = true
					node.mu.Unlock()
				}
				node.Reset()
				_, info := entrance.read(t, srv)
				if got := countCalls(node, "cmd "+ownRead); got != 1 {
					t.Errorf("%s read the wrapper on its own %d times, "+
						"want 1", label, got)
				}
				meta := info.GetCloneIdToMeta()[testClone]
				if !killed {
					assertOk(t, meta, label+" clone meta row")
					continue
				}
				node.mu.Lock()
				armed := len(node.killCmd)
				node.mu.Unlock()
				if armed != 0 {
					t.Fatalf("%s: the kill never fired", label)
				}
				want := "clone-metadata arena unavailable: " + ownRead +
					": signal: killed"
				if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					meta.GetDetails() != want {
					t.Errorf("%s: the clone meta row reads %v %q, want "+
						"ERROR %q", label, meta.GetStatus(),
						meta.GetDetails(), want)
				}
				assertErrorDetails(t,
					info.GetCloneIdToDmClone()[testClone],
					detailsCloneMetaMissing, label+" dm-clone row")
			}
		}
	})
}

// TestACompleteListingReadsNoUnbuiltWrapper pins the rule for a clone
// metadata wrapper this cntlr wants under a complete info listing
// (Wrappers, CN28): a listing that lacks it answers it absent, with no read
// of its own. TestPartialDmListingStillAnswers pins the partial listing.
// The fixture's clone waits for room, so its wrapper is not built; a check
// round and an info read take the wrapper's absence from their reading,
// name it in no command and no file read, and their clone rows read the
// converge's PROVISIONING pair. Read on its own, the wrapper would cost one
// more process in every round for as long as the clone waits.
func TestACompleteListingReadsNoUnbuiltWrapper(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	// Another sp's wrapper fills the whole arena, as in
	// TestCloneMetaNoRunIsProvisioning.
	plantWrapper(t, srv, node, 0x999, 0, common.CnCloneMetaUnitCnt)
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertProvisioningDetails(t,
		reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		waitingForTwoUnits, "the converge's clone meta row")
	wrapper := cloneMetaName(srv, testClone)
	node.mu.Lock()
	_, built := node.dms[wrapper]
	node.mu.Unlock()
	if built {
		t.Fatalf("fixture: the waiting clone's wrapper %s is built", wrapper)
	}
	if listing, err := srv.dm.ListInfo(context.Background()); err != nil ||
		listing.Partial {
		t.Fatalf("fixture: the info listing read (%v), want it complete",
			err)
	}
	for _, entrance := range readEntrances {
		node.Reset()
		_, info := entrance.read(t, srv)
		if got := countCalls(node, dmInfoListLine); got != 1 {
			t.Fatalf("%s took %d info listings, want 1", entrance.name, got)
		}
		if calls := node.callsMatching(wrapper); len(calls) != 0 {
			t.Errorf("%s read the unbuilt wrapper: %q", entrance.name,
				calls)
		}
		assertProvisioningDetails(t, info.GetCloneIdToMeta()[testClone],
			waitingForTwoUnits, entrance.name+" clone meta row")
		assertProvisioningDetails(t, info.GetCloneIdToDmClone()[testClone],
			waitingForTwoUnits, entrance.name+" dm-clone row")
	}
}

// TestEmptyNodeListingIsAnAnswer pins the one output of a whole-node dm
// listing that holds no device: "No devices found" with exit status 0, which
// every dmsetup form prints on a node with no dm device. It is an empty
// node, not a listing that did not answer: a cntlr synced at
// SP_LEVEL_DISABLE from its first request builds nothing, the base state's
// loop device is no dm device, and the verdict of its check round and of its
// info read is clean, naming no dmsetup listing.
func TestEmptyNodeListingIsAnAnswer(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		level: pb.SpLevel_SP_LEVEL_DISABLE})
	node.mu.Lock()
	dms, subsystems := len(node.dms), len(node.subsystems)
	node.mu.Unlock()
	if dms != 0 || subsystems != 0 {
		t.Fatalf("fixture: the disabled cntlr left %d dm devices and %d "+
			"connections", dms, subsystems)
	}
	for _, entrance := range readEntrances {
		node.Reset()
		verdict, _ := entrance.read(t, srv)
		if got := node.callsMatching("cmd dmsetup"); !slices.Equal(got,
			[]string{dmInfoListLine, dmTableListLine}) {
			t.Fatalf("%s ran %q, want the two listings alone",
				entrance.name, got)
		}
		node.mu.Lock()
		outs := slices.Clone(node.stdouts["dmsetup"])
		node.mu.Unlock()
		if !slices.Equal(outs,
			[]string{"No devices found\n", "No devices found\n"}) {
			t.Fatalf("%s: the listings printed %q, want the empty node's "+
				"line twice", entrance.name, outs)
		}
		if verdict.GetCode() != 0 ||
			strings.Contains(verdict.GetDetails(), "dmsetup") {
			t.Errorf("%s: the verdict is %v, want clean", entrance.name,
				verdict)
		}
	}
}

// TestARoundBesideAStoppingArray pins the md walk of a check round and an
// info read under its two rules (CN12, CN21): another pool's array that md
// has marked deleted, whose array_state reads EBUSY until its md/ goes. The
// group rows and the concat rows read the walk under Md.Walk's rule, which
// never reads array_state, and stay OK; the verdict reads it under
// ListArrays' rule, which fails on that read, and names the md enumeration,
// as the converge's sweep does for the same window.
func TestARoundBesideAStoppingArray(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	probeAllLegs(t, srv)
	node.seedArray("/dev/md/other", "other", "/dev/mapper/other-sp-leg")
	node.mu.Lock()
	state := sysfsBlockDir + "/" + node.arrays["/dev/md/other"].node +
		"/md/array_state"
	node.errFiles[state] = &fs.PathError{Op: "read", Path: state,
		Err: syscall.EBUSY}
	node.mu.Unlock()
	for _, entrance := range readEntrances {
		node.Reset()
		verdict, info := entrance.read(t, srv)
		if !node.hasCall("read " + state) {
			t.Fatalf("%s never read %s", entrance.name, state)
		}
		assertRowsOk(t, info, entrance.name)
		if verdict.GetCode() != common.ReplyCodeLeftover ||
			!strings.Contains(verdict.GetDetails(),
				"enumeration failed: md arrays") {
			t.Errorf("%s: the verdict is %v, want a Leftover naming the md "+
				"enumeration", entrance.name, verdict)
		}
	}
}

// TestAReadOfARefusedConfTakesNoReading pins the gate of a check round and
// an info read (readCntlr; architecture.md, Common validation): a stored
// conf the agent refuses, a zero low_water_mark_pct here, takes no reading
// and reads nothing of the node. The call answers what a cntlr with nothing
// provable looks like — no row at all — and a clean verdict, and writes the
// one refusal record. Every table its rows would compare is built from that
// conf, so a probe with the zero would report a correctly built pool as
// needing a reload; and the converge refuses the conf before it builds or
// removes anything, so naming leftovers would name objects the agent may not
// remove.
func TestAReadOfARefusedConfTakesNoReading(t *testing.T) {
	for _, entrance := range readEntrances {
		t.Run(entrance.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
				testCntlr))
			st.storeReq(zeroConfReq(reqOpts{revision: 2, primary: true},
				func(conf *pb.BdevConf) { conf.DmPoolConf.LowWaterMarkPct = 0 }))
			capture := captureLogs(t)
			node.Reset()
			verdict, info := entrance.read(t, srv)
			if calls := node.Calls(); len(calls) != 0 {
				t.Fatalf("a read of a refused conf read the node:\n%s",
					strings.Join(calls, "\n"))
			}
			if rows := infoRows(info); len(rows) != 0 {
				t.Errorf("a read of a refused conf reported rows %v", rows)
			}
			if verdict.GetCode() != 0 {
				t.Errorf("a read of a refused conf gave the verdict %v, "+
					"want clean", verdict)
			}
			assertRefusalRecord(t, capture, msgNoWaterMark)
		})
	}
}

// TestUnansweredMdListingErrsTheGroupRows pins the md walk of a check round
// and an info read when its listing of /sys/block did not answer (CN12,
// CN28): every group row reads ERROR with the listing's error, and so does
// every concat row, naming its group, since a group's device number comes
// from the array the walk matches; every other row reads OK, and the verdict
// names the md enumeration. Read as a node with no array, the group rows
// would read MISSING, which sets no health epoch.
func TestUnansweredMdListingErrsTheGroupRows(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	probeAllLegs(t, srv)
	listing := "list " + sysfsBlockDir
	for _, entrance := range readEntrances {
		node.mu.Lock()
		// One-shot: the call's first listing of /sys/block is its md walk.
		node.killCmd[listing] = true
		node.mu.Unlock()
		node.Reset()
		verdict, info := entrance.read(t, srv)
		label := entrance.name
		if got := countCalls(node, listing); got != 1 {
			t.Fatalf("%s listed /sys/block %d times, want 1", label, got)
		}
		failed := map[string]int{}
		for key, row := range infoRows(info) {
			switch kind := rowKind(key); kind {
			case "grp", "pool_meta", "pool_data":
				failed[kind]++
				want := listing + ": " + context.DeadlineExceeded.Error()
				if kind != "grp" {
					want = "group "
				}
				if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					!strings.Contains(row.GetDetails(), want) ||
					!strings.Contains(row.GetDetails(), listing) {
					t.Errorf("%s: %s reads %v %q, want ERROR naming %q",
						label, key, row.GetStatus(), row.GetDetails(),
						listing)
				}
			default:
				if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("%s: %s reads %v %q, want OK", label, key,
						row.GetStatus(), row.GetDetails())
				}
			}
		}
		if failed["grp"] != 2 || failed["pool_meta"] != 1 ||
			failed["pool_data"] != 1 {
			t.Fatalf("%s: rows %v: the case is vacuous", label, failed)
		}
		if verdict.GetCode() != common.ReplyCodeLeftover ||
			!strings.Contains(verdict.GetDetails(),
				"enumeration failed: md arrays") {
			t.Errorf("%s: the verdict is %v, want a Leftover naming the md "+
				"enumeration", label, verdict)
		}
	}
}

// TestCheckRoundReadsTheCloneFromTheReading pins the clone stack's part of
// a check round and an info read (CN5, CN18, CN28): a primary with a built,
// hydrating clone takes the dm-clone's attrs, the clone metadata wrappers'
// names and tables and, in the verdict, the dm-clone's table for the source
// it maps, all from the reading. It reads no device on its own with
// `dmsetup info` or `dmsetup table`, runs no `dmsetup ls`, and its status
// reads stay per device: the pool's, and the dm-clone's for its rows and
// once more for the namespace it backs. Its one `losetup` finds the loop
// device of the metadata area. Every row reads OK and the verdict is clean.
func TestCheckRoundReadsTheCloneFromTheReading(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		clones: []*pb.Clone{cloneOf()}})
	probeAllLegs(t, srv)
	clone := cloneName(srv, testClone)
	for _, entrance := range readEntrances {
		node.Reset()
		verdict, info := entrance.read(t, srv)
		label := entrance.name
		if reads := perDeviceDmReads(node); len(reads) != 0 {
			t.Errorf("%s read devices on their own: %q", label, reads)
		}
		assertNoCall(t, node, "cmd dmsetup ls")
		for call, want := range map[string]int{
			"cmd dmsetup status " + poolName(srv): 1,
			"cmd dmsetup status " + clone:         2,
			"cmd losetup --associated ":           1,
			"cmd dmsetup status ":                 3,
		} {
			if got := callCnt(node, call); got != want {
				t.Errorf("%s: %q ran %d times, want %d", label, call, got,
					want)
			}
		}
		assertRowsOk(t, info, label)
		if verdict.GetCode() != 0 {
			t.Errorf("%s: the verdict is %v, want clean", label, verdict)
		}
	}
}
