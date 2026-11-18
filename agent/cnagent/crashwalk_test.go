package cnagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A crash at every step of a converge.
//
// Each walk drives one converge against the fake node, ends the agent at
// every call that converge makes — before the call, and again after it —
// starts a new agent over the same node and the same local files, and
// drives it as the worker does (dnv-worker.md RW4, RW5, HL8): the held
// request is sent again on a non-zero code or another revision, and nothing
// is sent while the agent answers "starting". A cut passes when two passes
// of rounds in a row send nothing and read no leg "health probe pending",
// every held object then answers code 0 at its revision with every row OK,
// and the node holds what a run with no cut leaves — or, for a state a
// restart forgets by design, the outcome its doc names:
//
//   - a cut before the request's save: the next agent holds the request
//     before it, or none, and the re-send brings the new one (dnagent.md
//     SH5);
//   - the torn snapshot mark: the next agent's pre-pass sets it exactly when
//     the cut fell between the first and the last slice's snapshot message
//     (CN14), and it is no row, so the rows stay OK; the mark's file in the
//     local store is then the one difference the node holds, as no worker
//     records the mark here to clear it;
//   - the leg rows read "health probe pending" until the restarted probers'
//     first round (CN11): the drive waits for it;
//   - a node sweep that sets a disconnect going names it until it returns
//     (CN10, CN30), so the worker sends the node syncup again once (CN2);
//   - a thin_dump file the ended agent wrote and did not remove stays until
//     the pool's next dump (CN25).
//
// The next agent's startup pass converges every held object again (CN2), so
// the worker has to send something again only for a cut before the save and
// for a disconnect a sweep sets going (crashResendExpected); a cut that
// needs it anywhere else fails, for the worker's re-send would hide a
// startup pass that left its work undone.
//
// An end state can hide what happened on the way, so a cut also fails when
// the startup pass leaves a device suspended where it found one suspended
// ([D12]: no device outlives a converge suspended), or keeps a cntlr whose
// pointer the CN it holds does not name (crashKeptOrphans), when work of
// the ended agent that does not hang off its root context tries the node
// after its end (crashGate.late), and when a device the cut left there is
// there at the end under another number or name (dnagent.md SH16). A
// restart with no cut must keep every device under the number it had and
// make no call that changes the node (TestARestartWithNoCutChangesNothing),
// and a crash during the startup pass that follows a cut must end as a
// single cut does (TestACrashDuringTheStartupPassAfterACut).
//
// The cut counts the converge's own calls by its trace id and the gate's
// mark (crashGate); the background disconnects a node sweep sets going are
// held until the converge has returned, and a walk of their own cuts each of
// them.

var crashDevNoRe = regexp.MustCompile(`\b253:\d+\b`)

// crashNode is newTestServer's node, with nothing on it.
func crashNode() *fakeNode {
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	return node
}

// crashNoNames names each device number on the node by its device: a dm
// device by its /dev/mapper path, an md array by its /dev/md path, and a
// multipath namespace head by the NQN of its subsystem, whose kernel number
// a later connect may change.
func crashNoNames(node *fakeNode) map[string]string {
	heads := make(map[string]string)
	for nqn, subsys := range node.subsystems {
		heads[fmt.Sprintf("/dev/nvme%dn", subsys.idx)] = nqn
	}
	byNo := make(map[string]string)
	for path, no := range node.devNo {
		name := path
		if rest, ok := strings.CutPrefix(path, "/dev/nvme"); ok {
			if i := strings.Index(rest, "n"); i > 0 {
				if nqn, ok := heads["/dev/nvme"+rest[:i+1]]; ok {
					name = "head of " + nqn
				}
			}
		}
		byNo[no] = name
	}
	return byNo
}

// crashSnapshot is what the node holds, in lines: the dm devices with their
// tables (device numbers named by their devices, crashNoNames), states,
// dm-clone knobs and thin-pool facts, and the ranges a device was discarded
// at, as a set — a dm-clone's hydration marks, which a restart's apply
// makes again; the thin ids each pool's metadata holds; the md arrays with
// their members, identities and sysfs state, the members' superblocks, and
// the copy dm-clone's metadata holds; the nvmet configfs tree; the nvme host
// connections by NQN and address; and the local store, decoded. Left out:
// the device numbers themselves, the arrays' kernel names and the
// controllers' names, which restartAndJudge and
// TestARestartWithNoCutChangesNothing compare on their own (crashDevNos). A thin_dump file left in thinDumpDir is a line too
// (crashDumpLine).
func crashSnapshot(node *fakeNode) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	byNo := crashNoNames(node)
	norm := func(s string) string {
		return crashDevNoRe.ReplaceAllStringFunc(s, func(no string) string {
			if name, ok := byNo[no]; ok {
				return name
			}
			return no
		})
	}
	var out []string
	for name, dm := range node.dms {
		discarded := slices.Clone(dm.discards)
		slices.Sort(discarded)
		out = append(out, fmt.Sprintf("dm %s suspended=%t ro=%t "+
			"bdevro=%t waiting=%q nohydration=%t nodiscardpassdown=%t "+
			"threshold=%d batch=%d metamode=%q snapshot=%t heldroot=%t "+
			"poolmeta=%d needscheck=%t poolmode=%q poolfault=%q table=%s "+
			"discarded=%q", name, dm.suspended, dm.readOnly,
			dm.bdevReadOnly, norm(dm.waitingTable()), dm.noHydration,
			dm.noDiscardPassdown, dm.threshold, dm.batchSize, dm.metaMode,
			dm.snapshot, dm.heldRoot, dm.poolMetaBlocks, dm.poolNeedsCheck,
			dm.poolMode, dm.poolFault, norm(dm.table),
			slices.Compact(discarded)))
	}
	for pool, ids := range node.thinPools {
		var list []uint32
		for id := range ids {
			list = append(list, id)
		}
		slices.Sort(list)
		out = append(out, fmt.Sprintf("thin ids %s %v", pool, list))
	}
	for path, array := range node.arrays {
		out = append(out, fmt.Sprintf("array %s name=%s members=%v uuid=%s "+
			"state=%s degraded=%d sync=%s completed=%s last=%s min=%s "+
			"badblocks=%v memberstate=%v memberslot=%v unbound=%v", path,
			array.name, array.members, array.uuid, array.arrayState,
			array.degraded, array.syncAction, array.syncCompleted,
			array.lastSyncAction, array.syncMin, array.badBlocks,
			array.memberState, array.memberSlot, array.unbound))
	}
	for member, ok := range node.superblocks {
		if ok {
			out = append(out, fmt.Sprintf("superblock %s array=%s facts=%+v",
				member, node.memberArray[member], node.sbFacts[member]))
		}
	}
	for key, count := range node.cloneHydrated {
		out = append(out, fmt.Sprintf("clone metadata %s hydrated=%d", key,
			count))
	}
	for dir, ok := range node.dirs {
		if ok && strings.HasPrefix(dir, agent.NvmetRoot) {
			out = append(out, "dir "+dir)
		}
	}
	for path, value := range node.files {
		if strings.HasPrefix(path, agent.NvmetRoot) {
			out = append(out, fmt.Sprintf("file %s=%q", path,
				strings.TrimSpace(norm(value))))
		}
	}
	for link, target := range node.links {
		if strings.HasPrefix(link, agent.NvmetRoot) {
			out = append(out, "link "+link+" -> "+target)
		}
	}
	for path := range node.files {
		if strings.HasPrefix(path, thinDumpDir+"/") {
			out = append(out, crashDumpLine(path))
		}
	}
	for nqn, subsys := range node.subsystems {
		var ctrls []string
		for _, ctrl := range subsys.ctrls {
			ctrls = append(ctrls, fmt.Sprintf("%s:%s %s ana=%s", ctrl.trAddr,
				ctrl.trSvcId, ctrl.state, ctrl.anaState))
		}
		sort.Strings(ctrls)
		out = append(out, fmt.Sprintf("conn %s ctrls=%v", nqn, ctrls))
	}
	for path, raw := range node.protos {
		out = append(out, "store "+path+" "+crashStoreText(path, raw))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// crashStoreText decodes one local store file by its kind (dnagent.md SH6):
// a message's bytes need not be the same for the same message, as its maps
// are written in any order.
func crashStoreText(path string, raw []byte) string {
	base := path[strings.LastIndex(path, "/")+1:]
	var msg proto.Message
	switch {
	case strings.HasPrefix(base, "cntlr-"):
		msg = &pb.SyncupCntlrRequest{}
	case strings.HasPrefix(base, "clone-bm-"):
		msg = &pb.PushCloneBitmapRequest{}
	case strings.HasPrefix(base, "cn-"):
		msg = &pb.SyncupCnRequest{}
	case strings.HasPrefix(base, "torn-"):
		msg = &pb.TornMarks{}
	default:
		return fmt.Sprintf("%x", raw)
	}
	if err := proto.Unmarshal(raw, msg); err != nil {
		return "undecodable: " + err.Error()
	}
	return prototext.MarshalOptions{}.Format(msg)
}

// crashDevNos is every device's own number and kernel name, by what the
// device is: a dm device's number by its name, an array's number and kernel
// name by its path, a connection's subsystem entry by its NQN and each of
// its controllers' names by the NQN and the address. A restart that finds a
// device right keeps it, and so its number.
func crashDevNos(node *fakeNode) map[string]string {
	node.mu.Lock()
	defer node.mu.Unlock()
	out := make(map[string]string)
	for name := range node.dms {
		out["dm "+name] = node.devNo["/dev/mapper/"+name]
	}
	for path, array := range node.arrays {
		out["array "+path] = array.devNo + " " + array.node
	}
	for nqn, subsys := range node.subsystems {
		out["subsys "+nqn] = fmt.Sprintf("nvme-subsys%d", subsys.idx)
		for _, ctrl := range subsys.ctrls {
			key := "ctrl " + nqn + " " + ctrl.trAddr + ":" + ctrl.trSvcId
			out[key] = strings.TrimSpace(out[key] + " " + ctrl.name)
		}
	}
	return out
}

// crashNosText is crashDevNos as sorted lines, for crashDiff.
func crashNosText(nos map[string]string) string {
	var out []string
	for key, no := range nos {
		out = append(out, key+" = "+no)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// crashRenumbered lists each device present both in before and in after
// whose number or name changed.
func crashRenumbered(before, after map[string]string) []string {
	var out []string
	for key, no := range before {
		if now, ok := after[key]; ok && now != no {
			out = append(out, fmt.Sprintf("%s: %s, then %s", key, no, now))
		}
	}
	sort.Strings(out)
	return out
}

func crashAnySuspended(node *fakeNode) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, dm := range node.dms {
		if dm.suspended {
			return true
		}
	}
	return false
}

// crashNodeChanges is the calls of lines that change the node: all but the
// probes of readOnlyPrefixes.
func crashNodeChanges(lines []string) []string {
	var out []string
	for _, line := range lines {
		if !slices.ContainsFunc(readOnlyPrefixes, func(prefix string) bool {
			return strings.HasPrefix(line, prefix)
		}) {
			out = append(out, line)
		}
	}
	return out
}

// crashDiff is the lines one snapshot holds and the other does not.
func crashDiff(want, got string) string {
	wantSet := make(map[string]bool)
	for _, line := range strings.Split(want, "\n") {
		wantSet[line] = true
	}
	gotSet := make(map[string]bool)
	for _, line := range strings.Split(got, "\n") {
		gotSet[line] = true
	}
	var out []string
	for line := range wantSet {
		if !gotSet[line] {
			out = append(out, "  - "+crashTrunc(line))
		}
	}
	for line := range gotSet {
		if !wantSet[line] {
			out = append(out, "  + "+crashTrunc(line))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func crashTrunc(line string) string {
	if len(line) > 300 {
		return line[:300] + "..."
	}
	return line
}

// crashHeld is what the worker holds for the node: the CN's request and its
// cntlrs' requests.
type crashHeld struct {
	cn     *pb.SyncupCnRequest
	cntlrs []*pb.SyncupCntlrRequest
}

// crashDriveBound is how long a drive waits for two quiet passes in a row;
// crashSendBound is the most re-sends it makes before it gives up.
const (
	crashDriveBound = 5 * time.Second
	crashSendBound  = 4
)

// crashDriven is what a drive came to: the re-sends it made, each with what
// the round that brought it answered, the rows not OK of its last pass, the
// tds whose thin info says torn, and why no pass was quiet, "" when two
// were.
type crashDriven struct {
	sent    []string
	reasons []crashReason
	rows    string
	torn    []uint64
	failure string
}

// crashReason is what a round that brought a re-send answered: whose round
// it was, its details, and its rows that are not OK.
type crashReason struct {
	cntlr   bool
	details string
	rows    []string
}

// crashDrive plays the worker against srv until two passes in a row are
// quiet. A pass is one round per held object, the CN's first (checkCnRound,
// checkCntlrRound). A round that answers "starting" gives no verdict and
// sends nothing (dnv-worker.md HL8), but the pass is not quiet; any other
// non-zero code, or another revision, sends the held request again
// (dnv-worker.md RW4), and the pass is not quiet either; nor is one whose
// round answers from no observation, or whose rows read a leg "health probe
// pending" (CN11). The reads the rounds start
// are joined, and the disconnects a converge set going awaited, before the
// next pass, as a worker's next round comes well after them; the second
// quiet pass so answers from the read the first one started (dnagent.md
// SH29).
func crashDrive(
	srv *CnAgentServer,
	held crashHeld,
	prefix string,
) crashDriven {
	var out crashDriven
	deadline := time.Now().Add(crashDriveBound)
	quietPasses := 0
	for pass := 1; ; pass++ {
		ctx := traced(fmt.Sprintf("%s-%d", prefix, pass))
		quiet, pending := true, false
		var rows []string
		var torn []uint64
		round, cnInfo := srv.checkCnRound(ctx, &pb.CheckCnRequest{
			ClusterId: testCluster, CnId: testCn,
			Revision: held.cn.GetRevision(), ShowInfo: true,
		}, nil)
		switch code := round.GetAgentReply().GetCode(); {
		case code == common.ReplyCodeStarting:
			quiet = false
		case code != 0 || round.GetRevision() != held.cn.GetRevision():
			quiet = false
			out.sent = append(out.sent, fmt.Sprintf("pass %d: SyncupCn "+
				"after code %d rev %d (%s)", pass, code, round.GetRevision(),
				crashTrunc(round.GetAgentReply().GetDetails())))
			out.reasons = append(out.reasons, crashReason{
				details: round.GetAgentReply().GetDetails(),
				rows:    notOkRows(cnInfo)})
			_, _ = srv.SyncupCn(ctx,
				proto.Clone(held.cn).(*pb.SyncupCnRequest))
		case round.GetObsStamp().GetObsTime() == nil:
			// Held, with no observation to answer from.
			quiet = false
		default:
			for _, row := range notOkRows(cnInfo) {
				rows = append(rows, "cn"+row)
			}
		}
		for _, req := range held.cntlrs {
			round, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: req.GetCntlrPointer(),
				Revision:     req.GetRevision(), ShowInfo: true,
			}, nil)
			switch code := round.GetAgentReply().GetCode(); {
			case code == common.ReplyCodeStarting:
				quiet = false
			case code != 0 || round.GetRevision() != req.GetRevision():
				quiet = false
				out.sent = append(out.sent, fmt.Sprintf("pass %d: "+
					"SyncupCntlr %s after code %d rev %d (%s)", pass,
					cntlrPointerText(req.GetCntlrPointer()), code,
					round.GetRevision(),
					crashTrunc(round.GetAgentReply().GetDetails())))
				out.reasons = append(out.reasons, crashReason{cntlr: true,
					details: round.GetAgentReply().GetDetails(),
					rows:    notOkRows(info)})
				_, _ = srv.SyncupCntlr(ctx,
					proto.Clone(req).(*pb.SyncupCntlrRequest))
			case round.GetObsStamp().GetObsTime() == nil:
				quiet = false
			default:
				for _, row := range notOkRows(info) {
					rows = append(rows, "cntlr"+row)
					pending = pending || strings.Contains(row,
						detailsProbePending)
				}
				for tdId, thin := range info.GetTdIdToThinInfo() {
					if thin.GetTorn() {
						torn = append(torn, tdId)
					}
				}
			}
		}
		srv.reads.Wait()
		if !crashAwaitDisconnects(srv) {
			out.failure = "a disconnect never ended"
			return out
		}
		if quiet && !pending {
			quietPasses++
			if quietPasses == 2 {
				sort.Strings(rows)
				slices.Sort(torn)
				out.rows, out.torn = strings.Join(rows, "\n"), torn
				return out
			}
			continue
		}
		quietPasses = 0
		switch {
		case len(out.sent) > crashSendBound:
			out.failure = fmt.Sprintf("the worker sent %d times:\n%s",
				len(out.sent), strings.Join(out.sent, "\n"))
			return out
		case time.Now().After(deadline):
			out.failure = fmt.Sprintf("no two passes in a row were quiet "+
				"within %v; the last read rows %v", crashDriveBound, rows)
			return out
		}
		time.Sleep(crashProbeInterval)
	}
}

var crashRequestSave = regexp.MustCompile(`^writeproto \S*/(cn|cntlr)-`)

// crashResend is why a cut may leave the worker something to send once the
// next agent's startup pass has run.
type crashResend int

const (
	// crashResendNone: nothing; the pass converges every held object again
	// (CN2).
	crashResendNone crashResend = iota
	// crashResendSave: the cut fell before the request's save, so the next
	// agent holds the request before it, or none (dnagent.md SH5), and the
	// worker sends the request, and whatever its converge leaves, again.
	crashResendSave
	// crashResendDisconnects: the startup pass set a disconnect going,
	// which its node sweep names until it returns (CN2, CN10, CN30): the
	// worker sends the node syncup once, after a round that names the
	// disconnected connections and nothing else (crashOnlyConnections).
	crashResendDisconnects
)

// crashResendExpected says why a cut may leave the worker something to send.
// lines are the cut work's calls, of which ran ran, and startup the calls of
// the work whose observation the first round answers — the next agent's
// startup pass, its background disconnects included. The worker sends again
// only what a restart forgets by design, or what the pass itself leaves,
// named, for a round.
func crashResendExpected(lines []string, ran int, startup []string) crashResend {
	for _, line := range lines[min(ran, len(lines)):] {
		if crashRequestSave.MatchString(line) {
			return crashResendSave
		}
	}
	for _, line := range startup {
		if strings.HasPrefix(line, "cmd nvme disconnect ") {
			return crashResendDisconnects
		}
	}
	return crashResendNone
}

// crashResendFailure says why a drive's re-sends break the rule expected
// gives, "" when they keep it: none at all for crashResendNone; for
// crashResendDisconnects one SyncupCn after a round that named the
// connections the pass disconnects and nothing else, its rows all OK, since
// anything more is work the startup pass left undone and the re-send's
// converge would hide.
func crashResendFailure(expected crashResend, driven crashDriven) string {
	switch {
	case len(driven.sent) == 0 || expected == crashResendSave:
		return ""
	case expected == crashResendNone:
		return "the startup pass left the worker something to send:\n" +
			strings.Join(driven.sent, "\n")
	case len(driven.sent) > 1 || driven.reasons[0].cntlr ||
		len(driven.reasons[0].rows) > 0 ||
		!crashOnlyConnections(driven.reasons[0].details):
		return "the startup pass left the worker more to send than its " +
			"disconnects:\n" + strings.Join(driven.sent, "\n")
	}
	return ""
}

// crashOnlyConnections reports whether a round's details are a plain
// leftover list that names nvme host connections and nothing else
// (agent.LeftoverKindNvme), as a node sweep's are while its disconnects run
// (cnSweepOnlyDisconnects).
func crashOnlyConnections(details string) bool {
	_, names, ok := strings.Cut(details, "): ")
	if !ok || strings.Contains(names, "; ") ||
		strings.Contains(names, " more]") {
		return false
	}
	for _, name := range strings.Split(names, ", ") {
		if !strings.HasPrefix(name, agent.LeftoverKindNvme+":") {
			return false
		}
	}
	return true
}

// crashDumpLine is the snapshot's line of a thin_dump file at path.
func crashDumpLine(path string) string {
	return "dump file " + path
}

var crashDumpRe = regexp.MustCompile(`^cmd thin_dump .* -o (\S+)$`)

// crashWork is one piece of cut work: its calls, of which the first ran
// ran.
type crashWork struct {
	lines []string
	ran   int
}

// crashDumpsLeft lists the thin_dump files the calls that ran of the cut
// works, in their order, and then the calls of startup leave on the node:
// CN25 has the `rm -f` follow every dump that ran, so only an agent that
// dies between the dump and the `rm` leaves the file, and it stays until
// the pool's next dump, as the walk finds it.
func crashDumpsLeft(works []crashWork, startup []string) []string {
	left := make(map[string]bool)
	replay := func(lines []string) {
		for _, line := range lines {
			if m := crashDumpRe.FindStringSubmatch(line); m != nil {
				left[m[1]] = true
			}
			if path, ok := strings.CutPrefix(line, "cmd rm -f "); ok {
				delete(left, path)
			}
		}
	}
	for _, work := range works {
		replay(work.lines[:min(work.ran, len(work.lines))])
	}
	replay(startup)
	out := make([]string, 0, len(left))
	for path := range left {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// crashTornExpected reports whether the cut work's first ran calls hold
// the first slice's snapshot message and not the last one's: the next
// agent's pre-pass then makes the last slice's id while the first slice
// holds its own, and marks the snapshot torn (CN14).
func crashTornExpected(lines []string, ran int) bool {
	var at []int
	for i, line := range lines {
		if strings.Contains(line, " create_snap ") {
			at = append(at, i+1)
		}
	}
	return len(at) >= 2 && ran >= at[0] && ran < at[len(at)-1]
}

// crashTornNow reports whether the next agent's pre-pass will tear an
// uncreated snapshot of a held primary: some, but not all, of its slices'
// pools hold the snapshot's id now, and the pass makes the rest (CN14). A
// startup pass cut after it made the last ids loses the mark it set, as a
// restart does by design (cnagent.md, Known limits), and the pools then read
// whole.
func crashTornNow(node *fakeNode, held crashHeld) bool {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, req := range held.cntlrs {
		if !req.GetCntlr().GetPrimary() {
			continue
		}
		for _, td := range req.GetTdList() {
			if td.GetOriId() == 0 || td.GetCreated() {
				continue
			}
			holders := 0
			for key := range req.GetIdToSlice() {
				sliceId, _ := strconv.ParseUint(key, 16, 64)
				pool := nf.CnPoolFinalName(req.GetClusterId(), req.GetCnId(),
					req.GetCntlrPointer().GetSpId(), sliceId)
				if node.thinPools[pool][uint32(td.GetDevId())] {
					holders++
				}
			}
			if holders > 0 && holders < len(req.GetIdToSlice()) {
				return true
			}
		}
	}
	return false
}

// crashSampled reports whether the walk runs the i-th cut: every one, or
// every third under the race detector and under -short, which keeps the
// package's test time usable there.
func crashSampled(i int) bool {
	if raceEnabled || testing.Short() {
		return i%3 == 0
	}
	return true
}

// crashQuietLogs sends the agent's log records nowhere for the rest of the
// test: thousands of agent processes would bury a failure's message. No
// other test of the package runs meanwhile, none being parallel.
func crashQuietLogs(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// crashOk fails the test unless a setup step's call answered code 0.
func crashOk(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
	err error,
) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("%s: code %d (%s)", what, got.GetCode(), got.GetDetails())
	}
}

// crashShape is one converge to crash through. setup brings a fresh node,
// on the first agent, to the state before it, and conv is the converge;
// held is what the worker holds once conv has been sent. minCalls is the
// fewest calls the converge makes under its trace id: a change that quietly
// shortens it shrinks the walk, and this says so.
type crashShape struct {
	name     string
	setup    func(t *testing.T, srv *CnAgentServer)
	conv     func(ctx context.Context, srv *CnAgentServer)
	held     crashHeld
	minCalls int
	// minBackgroundCalls is the fewest background disconnects the converge
	// sets going (CN10); minStartupCalls, the fewest calls the startup pass
	// after the shape makes.
	minBackgroundCalls int
	minStartupCalls    int
	// minDoubleCuts is the fewest double cuts the shape gives
	// (TestACrashDuringTheStartupPassAfterACut) in a plain run, and a third
	// of it, rounded up, under the race detector and -short (crashSampled).
	// A count that falls short says the walk cuts fewer startup passes than
	// it did, and with none every check of a startup pass cut short runs
	// zero times.
	minDoubleCuts int
}

// crashRich is the rich primary's request, and its standby's: RAID1, a data
// group of two legs, two slices, a transfer, and the tds given (the
// fixture's one td when nil).
func crashRich(
	revision uint64,
	primary bool,
	tds []*pb.ThinDevice,
) *pb.SyncupCntlrRequest {
	return cntlrReq(reqOpts{revision: revision, primary: primary,
		raid1: true, twoLegs: true, twoSlices: true, tds: tds,
		xfers: []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
			OriNsIdx: 1}}})
}

// crashCloned is a RAID1 primary with a data group of two legs, with the
// fixture clone and its clone meta group (cloneGrpOf) when clone is set.
func crashCloned(revision uint64, clone bool) *pb.SyncupCntlrRequest {
	o := reqOpts{revision: revision, primary: true, raid1: true,
		twoLegs: true}
	if clone {
		o.clones = []*pb.Clone{cloneOf()}
	}
	return cntlrReq(o)
}

func crashShapes() []crashShape {
	ctx := context.Background()
	cn := func(t *testing.T, srv *CnAgentServer) {
		reply, err := srv.SyncupCn(ctx, cnReq(1, true))
		crashOk(t, "SyncupCn", reply, err)
	}
	built := func(req *pb.SyncupCntlrRequest) func(*testing.T,
		*CnAgentServer) {
		return func(t *testing.T, srv *CnAgentServer) {
			cn(t, srv)
			reply, err := srv.SyncupCntlr(ctx, req)
			crashOk(t, "SyncupCntlr", reply, err)
		}
	}
	sendCn := func(req *pb.SyncupCnRequest) func(context.Context,
		*CnAgentServer) {
		return func(ctx context.Context, srv *CnAgentServer) {
			_, _ = srv.SyncupCn(ctx, req)
		}
	}
	sendCntlr := func(req *pb.SyncupCntlrRequest) func(context.Context,
		*CnAgentServer) {
		return func(ctx context.Context, srv *CnAgentServer) {
			_, _ = srv.SyncupCntlr(ctx, req)
		}
	}
	withCntlr := func(req *pb.SyncupCntlrRequest) crashHeld {
		return crashHeld{cn: cnReq(1, true),
			cntlrs: []*pb.SyncupCntlrRequest{req}}
	}
	return []crashShape{{
		name:            "the first node syncup",
		setup:           func(*testing.T, *CnAgentServer) {},
		conv:            sendCn(cnReq(1, true)),
		held:            crashHeld{cn: cnReq(1, true)},
		minCalls:        35,
		minStartupCalls: 14,
		minDoubleCuts:   2,
	}, {
		name:            "the first build of a rich primary",
		setup:           cn,
		conv:            sendCntlr(crashRich(1, true, nil)),
		held:            withCntlr(crashRich(1, true, nil)),
		minCalls:        308,
		minStartupCalls: 278,
		minDoubleCuts:   272,
	}, {
		name:            "a snapshot",
		setup:           built(crashRich(1, true, nil)),
		conv:            sendCntlr(crashRich(2, true, snapTds())),
		held:            withCntlr(crashRich(2, true, snapTds())),
		minCalls:        278,
		minStartupCalls: 296,
		minDoubleCuts:   74,
	}, {
		name:            "a standby promoted",
		setup:           built(crashRich(1, false, nil)),
		conv:            sendCntlr(crashRich(2, true, nil)),
		held:            withCntlr(crashRich(2, true, nil)),
		minCalls:        288,
		minStartupCalls: 278,
		minDoubleCuts:   114,
	}, {
		name:            "a primary demoted",
		setup:           built(crashRich(1, true, nil)),
		conv:            sendCntlr(crashRich(2, false, nil)),
		held:            withCntlr(crashRich(2, false, nil)),
		minCalls:        203,
		minStartupCalls: 164,
		minDoubleCuts:   24,
	}, {
		name:            "a clone with its clone meta group",
		setup:           built(crashCloned(1, false)),
		conv:            sendCntlr(crashCloned(2, true)),
		held:            withCntlr(crashCloned(2, true)),
		minCalls:        239,
		minStartupCalls: 242,
		minDoubleCuts:   44,
	}, {
		// The sweep's five leg disconnects run off the pass (CN10).
		name:               "a node syncup that drops the cntlr",
		setup:              built(crashRich(1, true, nil)),
		conv:               sendCn(cnReq(2, false)),
		held:               crashHeld{cn: cnReq(2, false)},
		minCalls:           164,
		minBackgroundCalls: 5,
		minStartupCalls:    14,
		minDoubleCuts:      66,
	}}
}

// crashRef is what a run with no cut leaves: the node, and the rows of the
// last quiet pass.
type crashRef struct {
	snap string
	rows string
}

// crashCut is one cut: which call, and on which side of it.
type crashCut struct {
	phase string // which work: the shape's converge, its background, a pass
	k     int
	after bool
	line  string
}

func (c crashCut) String() string {
	when := "before"
	if c.after {
		when = "after"
	}
	return fmt.Sprintf("%s, %s call %d (%s)", c.phase, when, c.k, c.line)
}

// crashOutcome is what a cut came to.
type crashOutcome struct {
	failure   string
	sent      int
	torn      bool
	dumpLeft  bool
	suspended bool
}

// crashStarting fails unless srv answers every round and syncup of held
// that it is starting (dnagent.md SH33). The rounds go first: a round that
// is not refused answers at once, where a syncup that is not refused can
// wait on a startup pass that holds the node lock.
func crashStarting(srv *CnAgentServer, held crashHeld) string {
	ctx := traced("starting")
	var codes []uint32
	check := func() string {
		for _, code := range codes {
			if code != common.ReplyCodeStarting {
				return fmt.Sprintf("the starting agent answered codes %v, "+
					"not all starting", codes)
			}
		}
		return ""
	}
	round, _ := srv.checkCnRound(ctx, &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn,
		Revision: held.cn.GetRevision(), ShowInfo: true,
	}, nil)
	codes = append(codes, round.GetAgentReply().GetCode())
	for _, req := range held.cntlrs {
		round, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: req.GetCntlrPointer(),
			Revision:     req.GetRevision(), ShowInfo: true,
		}, nil)
		codes = append(codes, round.GetAgentReply().GetCode())
	}
	if failure := check(); failure != "" {
		return failure
	}
	syncup, _ := srv.SyncupCn(ctx, proto.Clone(held.cn).(*pb.SyncupCnRequest))
	codes = append(codes, syncup.GetAgentReply().GetCode())
	for _, req := range held.cntlrs {
		syncup, _ := srv.SyncupCntlr(ctx,
			proto.Clone(req).(*pb.SyncupCntlrRequest))
		codes = append(codes, syncup.GetAgentReply().GetCode())
	}
	return check()
}

// crashStartingWithin is crashStarting from a goroutine of its own: while a
// startup pass runs, it holds the node write lock, and a syncup that went
// past the "starting" answer would wait on it, where a round would answer
// at once from an observation, its read started in a goroutine. It fails
// when the answers do not come within a bound.
func crashStartingWithin(srv *CnAgentServer, held crashHeld) string {
	done := make(chan string, 1)
	go func() { done <- crashStarting(srv, held) }()
	select {
	case failure := <-done:
		return failure
	case <-time.After(5 * time.Second):
		return "while its startup pass ran, a round or a syncup of the " +
			"agent waited on it instead of answering starting"
	}
}

// restartAndJudge is everything after the cut. ended is the gate of the
// agent the cut ended. A new agent starts over the same node; it answers
// "starting" and touches nothing until its startup pass. The pass runs, and
// it must have resumed every device it found suspended. The worker then
// drives it to two quiet passes, and the node, the rows and the torn marks
// are compared with the reference; a device that was there at the cut and is
// there at the end has kept its number. works are the cut works, the first
// the one whose cut may fall before a request's save, and wantTorn whether
// the next agent's pre-pass tears a snapshot.
func (sh crashShape) restartAndJudge(
	node *fakeNode,
	ended *crashGate,
	ref crashRef,
	works []crashWork,
	wantTorn bool,
	startupId string,
) crashOutcome {
	suspended := crashAnySuspended(node)
	nos := crashDevNos(node)
	before := len(node.Calls())
	next := startCrashInc(node)
	defer next.end()
	if failure := crashStarting(next.srv, sh.held); failure != "" {
		return crashOutcome{failure: failure}
	}
	if after := len(node.Calls()); after != before {
		return crashOutcome{failure: fmt.Sprintf("the starting agent made "+
			"%d calls before its startup pass", after-before)}
	}
	if err := next.reconcile(startupId); err != nil {
		return crashOutcome{failure: "the startup pass failed: " +
			err.Error()}
	}
	if suspended && crashAnySuspended(node) {
		return crashOutcome{suspended: true, failure: "the startup pass " +
			"left a device suspended, and it found one suspended"}
	}
	if kept := crashKeptOrphans(next.srv, node); len(kept) > 0 {
		return crashOutcome{failure: fmt.Sprintf("the startup pass kept "+
			"cntlrs whose pointer the CN it holds does not name: %v", kept)}
	}
	if !crashAwaitDisconnects(next.srv) {
		return crashOutcome{failure: "a disconnect of the startup pass " +
			"never ended"}
	}
	startup := next.gate.linesUnder(startupId)
	expected := crashResendExpected(works[0].lines, works[0].ran, startup)
	driven := crashDrive(next.srv, sh.held, "drive")
	out := crashOutcome{sent: len(driven.sent), torn: len(driven.torn) > 0,
		suspended: suspended}
	if driven.failure != "" {
		out.failure = driven.failure
		return out
	}
	if failure := crashResendFailure(expected, driven); failure != "" {
		out.failure = failure
		return out
	}
	next.end()
	if late := ended.lateCalls(); late > 0 {
		out.failure = fmt.Sprintf("the ended agent tried %d calls after "+
			"its end, on a context its end did not cancel", late)
		return out
	}
	if out.torn != wantTorn {
		out.failure = fmt.Sprintf("the torn marks read %v, want torn %t",
			driven.torn, wantTorn)
		return out
	}
	// dnagent.md SH16: a converge changes only what differs, so a device the
	// cut left there and the end holds is the one the cut left.
	if moved := crashRenumbered(nos, crashDevNos(node)); len(moved) > 0 {
		out.failure = fmt.Sprintf("devices the cut left are there under "+
			"other numbers or names: %v", moved)
		return out
	}
	if driven.rows != ref.rows {
		out.failure = fmt.Sprintf("the rows read\n%s\nwant\n%s",
			driven.rows, ref.rows)
		return out
	}
	got := crashSnapshot(node)
	// CN25: a dump file the ended agent wrote and did not remove stays
	// until the pool's next dump; it must be there, and is then no
	// difference.
	for _, path := range crashDumpsLeft(works, startup) {
		line := crashDumpLine(path)
		rest := strings.Replace(got, "\n"+line, "", 1)
		if rest == got {
			rest = strings.Replace(got, line+"\n", "", 1)
		}
		if rest == got {
			out.failure = "the cut left the dump file " + path +
				", and the node holds none"
			return out
		}
		got = rest
		out.dumpLeft = true
	}
	// CN14: a torn snapshot's mark is saved in the local store, and stays
	// there while no request lists the td torn; it must be there exactly
	// when the cut tore the snapshot, and is then no difference.
	if wantTorn {
		rest, found := crashDropTornFile(got)
		if !found {
			out.failure = "the cut tore the snapshot, and the local store " +
				"holds no torn marks' file"
			return out
		}
		got = rest
	}
	if got != ref.snap {
		out.failure = "the node differs from a run with no cut:\n" +
			crashDiff(ref.snap, got)
	}
	return out
}

// crashDropTornFile removes from a node image (crashSnapshot) its one line
// of a torn marks' file, and says whether it found one.
func crashDropTornFile(image string) (string, bool) {
	lines := strings.Split(image, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "store ") &&
			strings.Contains(line, "/torn-") {
			return strings.Join(slices.Delete(lines, i, i+1), "\n"), true
		}
	}
	return image, false
}

// crashKeptOrphans names each cntlr srv holds, in memory or in a cntlr file
// of its local store, whose pointer the CN request srv holds does not name.
// The startup pass drops every such cntlr — its file, its chunks and its
// memory entry — and the node sweep finds its devices by name (CN2), so a
// pass that kept one leaves it for the worker's re-send to drop, which hides
// it.
func crashKeptOrphans(srv *CnAgentServer, node *fakeNode) []string {
	named := func(req *pb.SyncupCntlrRequest) bool {
		cn := srv.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
		return cn != nil && pointerKnown(cn.loadReq(), req.GetCntlrPointer())
	}
	var out []string
	for _, key := range srv.allCntlrKeys() {
		if st := srv.getCntlr(key); st != nil && !named(st.loadReq()) {
			out = append(out, "held "+key)
		}
	}
	node.mu.Lock()
	files := make(map[string][]byte, len(node.protos))
	for path, raw := range node.protos {
		files[path] = raw
	}
	node.mu.Unlock()
	for path, raw := range files {
		if !strings.HasPrefix(path[strings.LastIndex(path, "/")+1:],
			"cntlr-") {
			continue
		}
		req := &pb.SyncupCntlrRequest{}
		if err := proto.Unmarshal(raw, req); err != nil || !named(req) {
			out = append(out, "file "+path)
		}
	}
	sort.Strings(out)
	return out
}

// crashTally collects a walk's outcomes and reports the first failures.
type crashTally struct {
	cuts, fails, torn, dumps, suspended int
	sent                                map[int]int
}

func (tl *crashTally) add(t *testing.T, cut crashCut, out crashOutcome) {
	t.Helper()
	tl.cuts++
	if tl.sent == nil {
		tl.sent = make(map[int]int)
	}
	tl.sent[out.sent]++
	if out.torn {
		tl.torn++
	}
	if out.dumpLeft {
		tl.dumps++
	}
	if out.suspended {
		tl.suspended++
	}
	if out.failure != "" {
		tl.fails++
		if tl.fails <= 5 {
			t.Errorf("cut %s: %s", cut, out.failure)
		}
	}
}

func (tl *crashTally) report(t *testing.T, what string, start time.Time) {
	t.Helper()
	if tl.fails > 5 {
		t.Errorf("%d more cuts failed", tl.fails-5)
	}
	t.Logf("%s: %d cuts in %v; %d left a device suspended, %d a torn "+
		"snapshot, %d a dump file; re-sends per cut %v", what, tl.cuts,
		time.Since(start).Round(time.Millisecond), tl.suspended, tl.torn,
		tl.dumps, tl.sent)
}

// TestACrashAtEveryStepOfAConverge ends the agent at every call of each
// shape's converge, before it and after it, and starts it again over the
// same node. A shape whose converge sets disconnects going (CN10) also has
// each of them cut.
func TestACrashAtEveryStepOfAConverge(t *testing.T) {
	crashQuietLogs(t)
	for _, sh := range crashShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			ref, lines, bgLines := sh.reference(t)
			t.Logf("%s: the converge makes %d calls and sets %d "+
				"disconnects going", sh.name, len(lines), len(bgLines))
			if len(lines) < sh.minCalls {
				t.Errorf("the converge made %d calls, fewer than the %d "+
					"it makes at least", len(lines), sh.minCalls)
			}
			if len(bgLines) < sh.minBackgroundCalls {
				t.Errorf("the converge set %d disconnects going, fewer "+
					"than the %d it sets at least", len(bgLines),
					sh.minBackgroundCalls)
			}
			start := time.Now()
			var tally crashTally
			i := 0
			for k := 1; k <= len(lines); k++ {
				for _, after := range []bool{false, true} {
					i++
					if !crashSampled(i - 1) {
						continue
					}
					cut := crashCut{phase: "the converge", k: k,
						after: after, line: lines[k-1]}
					tally.add(t, cut, sh.cutConverge(t, ref, lines, cut))
				}
			}
			for k := 1; k <= len(bgLines); k++ {
				for _, after := range []bool{false, true} {
					i++
					if !crashSampled(i - 1) {
						continue
					}
					cut := crashCut{phase: "the disconnects", k: k,
						after: after, line: bgLines[k-1]}
					tally.add(t, cut, sh.cutBackground(t, ref, bgLines,
						cut))
				}
			}
			tally.report(t, sh.name, start)
		})
	}
}

// startFresh starts an agent over a fresh node and runs its startup pass,
// as the first process of every cut does.
func startFresh(t *testing.T) (*fakeNode, *crashInc) {
	t.Helper()
	node := crashNode()
	inc := startCrashInc(node)
	if err := inc.reconcile("startup-0"); err != nil {
		inc.end()
		t.Fatalf("startup pass: %v", err)
	}
	return node, inc
}

// reference runs the shape with no cut and returns what it leaves, the
// converge's calls, and the disconnects it sets going, sorted.
func (sh crashShape) reference(t *testing.T) (crashRef, []string, []string) {
	t.Helper()
	node, inc := startFresh(t)
	defer inc.end()
	sh.setup(t, inc.srv)
	inc.gate.arm("cut", false, 0, false)
	sh.conv(crashOwnCtx(context.Background(), "cut"), inc.srv)
	lines := inc.gate.counted()
	inc.gate.release(false, 0)
	if !crashAwaitDisconnects(inc.srv) {
		t.Fatal("with no cut, a disconnect never ended")
	}
	bgLines := inc.gate.backgroundLines()
	expected := crashResendExpected(nil, 0, inc.gate.linesUnder("cut"))
	driven := crashDrive(inc.srv, sh.held, "drive")
	if driven.failure != "" {
		t.Fatalf("with no cut: %s", driven.failure)
	}
	if failure := crashResendFailure(expected, driven); failure != "" {
		t.Fatalf("with no cut: %s", failure)
	}
	if len(driven.torn) > 0 {
		t.Fatalf("with no cut, tds read torn: %v", driven.torn)
	}
	inc.end()
	return crashRef{snap: crashSnapshot(node), rows: driven.rows}, lines,
		bgLines
}

// cutConverge builds the shape's state on a fresh node, cuts its converge,
// and judges what the next agent makes of it.
func (sh crashShape) cutConverge(
	t *testing.T,
	ref crashRef,
	refLines []string,
	cut crashCut,
) crashOutcome {
	t.Helper()
	node, inc := startFresh(t)
	defer inc.end()
	sh.setup(t, inc.srv)
	inc.gate.arm("cut", false, cut.k, cut.after)
	sh.conv(crashOwnCtx(context.Background(), "cut"), inc.srv)
	lines := inc.gate.counted()
	inc.end()
	if failure := crashSameCalls(lines, refLines, cut.k); failure != "" {
		return crashOutcome{failure: failure}
	}
	return sh.restartAndJudge(node, inc.gate, ref,
		[]crashWork{{lines: refLines, ran: crashRan(cut)}},
		crashTornExpected(refLines, crashRan(cut)), "startup-1")
}

// cutBackground runs the shape's converge whole, lets the disconnects it
// set going run one at a time in the order of their lines, cuts them, and
// judges what the next agent makes of it.
func (sh crashShape) cutBackground(
	t *testing.T,
	ref crashRef,
	bgLines []string,
	cut crashCut,
) crashOutcome {
	t.Helper()
	node, inc := startFresh(t)
	defer inc.end()
	sh.setup(t, inc.srv)
	inc.gate.arm("cut", true, cut.k, cut.after)
	sh.conv(crashOwnCtx(context.Background(), "cut"), inc.srv)
	inc.gate.release(true, len(bgLines))
	select {
	case <-inc.gate.cut:
	case <-time.After(5 * time.Second):
		return crashOutcome{failure: "the disconnects never reached the cut"}
	}
	lines := inc.gate.counted()
	inc.end()
	if failure := crashSameCalls(lines, bgLines, cut.k); failure != "" {
		return crashOutcome{failure: failure}
	}
	return sh.restartAndJudge(node, inc.gate, ref,
		[]crashWork{{lines: bgLines, ran: crashRan(cut)}}, false, "startup-1")
}

// crashRan is how many of the cut work's calls ran.
func crashRan(cut crashCut) int {
	if cut.after {
		return cut.k
	}
	return cut.k - 1
}

// crashSameCalls fails unless the cut work's first k calls are the
// reference's: the walk cuts the call it names.
func crashSameCalls(lines, refLines []string, k int) string {
	if len(lines) < k {
		return fmt.Sprintf("the work made %d calls, short of the cut",
			len(lines))
	}
	for i := range k {
		if lines[i] != refLines[i] {
			return fmt.Sprintf("call %d was %q, not %q as with no cut",
				i+1, lines[i], refLines[i])
		}
	}
	return ""
}

// buildShape builds a shape whole on a fresh node, with no cut, and returns
// the node; with settle it also drives the agent to two quiet passes.
func (sh crashShape) buildShape(t *testing.T, settle bool) *fakeNode {
	t.Helper()
	node, inc := startFresh(t)
	defer inc.end()
	sh.setup(t, inc.srv)
	sh.conv(traced("conv"), inc.srv)
	if !crashAwaitDisconnects(inc.srv) {
		t.Fatal("building: a disconnect never ended")
	}
	if !settle {
		return node
	}
	if driven := crashDrive(inc.srv, sh.held, "build"); driven.failure != "" {
		t.Fatalf("building: %s", driven.failure)
	}
	return node
}

// TestARestartWithNoCutChangesNothing builds each shape's state whole and
// restarts the agent over it with no cut: the restart must leave the built
// node as it found it, each device under the number it had, and make no call
// that changes the node (dnagent.md SH16) — one that removed and rebuilt a
// device it found right would show in the numbers and in its calls, and one
// that suspended, reloaded and resumed it only in its calls.
func TestARestartWithNoCutChangesNothing(t *testing.T) {
	crashQuietLogs(t)
	for _, sh := range crashShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			node := sh.buildShape(t, true)
			built := crashSnapshot(node)
			builtNos := crashDevNos(node)
			probe := startCrashInc(node)
			defer probe.end()
			probe.gate.arm("startup-1", false, 0, false)
			if err := probe.reconcile("startup-1"); err != nil {
				t.Fatalf("startup pass: %v", err)
			}
			lines := probe.gate.counted()
			probe.gate.release(false, 0)
			if !crashAwaitDisconnects(probe.srv) {
				t.Fatal("a disconnect never ended")
			}
			driven := crashDrive(probe.srv, sh.held, "drive")
			probe.end()
			if driven.failure != "" || len(driven.sent) > 0 {
				t.Fatalf("the restart: %s\n%s", driven.failure,
					strings.Join(driven.sent, "\n"))
			}
			if got := crashSnapshot(node); got != built {
				t.Fatalf("the restart changed the node:\n%s",
					crashDiff(built, got))
			}
			if got := crashDevNos(node); crashNosText(got) !=
				crashNosText(builtNos) {
				t.Fatalf("the restart rebuilt a device:\n%s",
					crashDiff(crashNosText(builtNos), crashNosText(got)))
			}
			if changes := crashNodeChanges(lines); len(changes) > 0 {
				t.Fatalf("the restart made %d calls that change the node "+
					"(dnagent.md SH16):\n%s", len(changes),
					strings.Join(changes, "\n"))
			}
			t.Logf("%s: the startup pass makes %d calls", sh.name,
				len(lines))
			if len(lines) < sh.minStartupCalls {
				t.Errorf("the startup pass made %d calls, fewer than the "+
					"%d it makes at least", len(lines), sh.minStartupCalls)
			}
		})
	}
}

// crashDoubleStride and crashDoubleStartupStride sample the double cuts:
// every crashDoubleStride-th cut of a shape's converge, counting the cut
// before and the cut after each call, and every crashDoubleStartupStride-th
// call that changes the node of the startup pass after it, cut before and
// after. Under the race detector and -short, crashSampled takes every third
// of those.
const (
	crashDoubleStride        = 41
	crashDoubleStartupStride = 4
)

// TestACrashDuringTheStartupPassAfterACut cuts each shape's converge at a
// sample of its calls, then starts a second agent over the node and cuts
// the startup pass that follows, which finishes the converge's work (CN2),
// before and after a sample of its calls that change the node, and judges a
// third agent as TestACrashAtEveryStepOfAConverge judges the second: a
// crash during the restart that follows a crash. A startup pass with
// nothing left to do changes nothing (TestARestartWithNoCutChangesNothing),
// so only the calls that change the node are cut. At each of those cuts,
// before the gate ends the second agent, every round and syncup of the held
// objects must answer that it is starting (dnagent.md SH33). The torn mark
// is expected from the pools before the third agent (crashTornNow), and the
// dump files from both cut works. The second agent ran a startup pass over
// the held objects, so its late calls (crashGate.late) show work of a
// startup pass that outlives the process.
func TestACrashDuringTheStartupPassAfterACut(t *testing.T) {
	crashQuietLogs(t)
	for _, sh := range crashShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			ref, convLines, _ := sh.reference(t)
			start := time.Now()
			var tally crashTally
			i := 0
			for c := 0; c < 2*len(convLines); c += crashDoubleStride {
				first := crashCut{phase: "the converge", k: c/2 + 1,
					after: c%2 == 1, line: convLines[c/2]}
				// cut cuts the converge on a fresh node and returns the
				// node, the ended agent's gate, and why the converge's
				// calls are not the reference's, "" when they are.
				cut := func() (*fakeNode, *crashGate, string) {
					node, inc := startFresh(t)
					defer inc.end()
					sh.setup(t, inc.srv)
					inc.gate.arm("cut", false, first.k, first.after)
					sh.conv(crashOwnCtx(context.Background(), "cut"), inc.srv)
					lines := inc.gate.counted()
					inc.end()
					return node, inc.gate, crashSameCalls(lines, convLines,
						first.k)
				}
				// The startup pass after this cut, whole: its calls, and
				// which of them change the node.
				node, _, failure := cut()
				if failure != "" {
					tally.add(t, first, crashOutcome{failure: failure})
					continue
				}
				whole := startCrashInc(node)
				whole.gate.arm("startup-1", false, 0, false)
				_ = whole.reconcile("startup-1")
				pass := whole.gate.counted()
				whole.gate.release(false, 0)
				crashAwaitDisconnects(whole.srv)
				whole.end()
				var changing []int
				for k, line := range pass {
					if len(crashNodeChanges([]string{line})) > 0 {
						changing = append(changing, k+1)
					}
				}
				for ci := 0; ci < len(changing); ci +=
					crashDoubleStartupStride {
					for _, after := range []bool{false, true} {
						i++
						if !crashSampled(i - 1) {
							continue
						}
						second := crashCut{phase: first.String() +
							", then the startup pass", k: changing[ci],
							after: after, line: pass[changing[ci]-1]}
						tally.add(t, second, sh.cutStartupAfter(t, ref,
							cut, crashRan(first), convLines, pass, second))
					}
				}
			}
			tally.report(t, sh.name+" (double cuts)", start)
			floor := sh.minDoubleCuts
			if raceEnabled || testing.Short() {
				// crashSampled runs the first of every three.
				floor = (floor + 2) / 3
			}
			if tally.cuts < floor {
				t.Errorf("the walk made %d double cuts, fewer than the %d "+
					"it makes at least", tally.cuts, floor)
			}
		})
	}
}

// cutStartupAfter cuts the shape's converge (cut), starts a second agent,
// cuts its startup pass at second — checking, at the cut, that the agent
// answers starting — and judges a third agent.
func (sh crashShape) cutStartupAfter(
	t *testing.T,
	ref crashRef,
	cut func() (*fakeNode, *crashGate, string),
	ran int,
	convLines []string,
	pass []string,
	second crashCut,
) crashOutcome {
	t.Helper()
	node, ended, failure := cut()
	if failure != "" {
		return crashOutcome{failure: failure}
	}
	inc := startCrashInc(node)
	defer inc.end()
	inc.gate.arm("startup-1", false, second.k, second.after)
	reached, resume := inc.gate.pauseAtCut()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A pass cut at its store listing fails as dnagent.md SH3 says;
		// the process would exit, and this one is ended.
		_ = inc.reconcile("startup-1")
	}()
	select {
	case <-reached:
		failure = crashStartingWithin(inc.srv, sh.held)
		close(resume)
	case <-done:
		close(resume)
	}
	<-done
	got := inc.gate.counted()
	inc.end()
	if failure != "" {
		return crashOutcome{failure: failure}
	}
	if failure := crashSameCalls(got, pass, second.k); failure != "" {
		return crashOutcome{failure: "the startup pass: " + failure}
	}
	out := sh.restartAndJudge(node, inc.gate, ref, []crashWork{
		{lines: convLines, ran: ran},
		{lines: pass, ran: crashRan(second)},
	}, crashTornNow(node, sh.held), "startup-2")
	if late := ended.lateCalls(); late > 0 && out.failure == "" {
		out.failure = fmt.Sprintf("the first agent tried %d calls after "+
			"its end, on a context its end did not cancel", late)
	}
	return out
}
