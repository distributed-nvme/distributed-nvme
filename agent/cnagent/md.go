package cnagent

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Md wraps the mdadm patterns of cnagent.md CN12. Only the cn role runs mdadm,
// so by the split rule of dnagent.md, Scope and placement, this wrapper is
// role code.
//
// The agent never runs `mdadm --zero-superblock`: a leg only ever leaves an
// array into the spare list — where a stale superblock makes a later re-add
// cheap — or out of existence together with its side (CN12).
type Md struct {
	cmd *agent.Cmd
}

func NewMd(oc common.OsClient) *Md {
	return &Md{cmd: agent.NewCmd(oc)}
}

// ---------------------------------------------------------------------------
// The sysfs view of the node's arrays
//
// Everything the sweep, CN12's member reconciliation, CN28's md row and the
// pool concats need about an array they read from sysfs, which never touches
// a member device and therefore never blocks on a dead leg:
//
//   - a listing of /sys/block names every array node, so a sweep finds
//     arrays it has no plan for — the whole point of deriving removal from
//     the live system;
//   - `/sys/block/mdN/dev` is the array's device number, which a pool concat
//     maps (mdDevNo);
//   - `/sys/block/mdN/md/array_state` says whether it still holds its
//     members. Only "clear" is gone; "inactive" is an assembled but not
//     running array, which pins them just as hard;
//   - `/sys/block/mdN/md/dev-*/block/dm/name` names each member's dm device,
//     which is what attributes the array to an sp and to a group. A member
//     with no such attribute is not a dm device at all, and an array with one
//     is not ours;
//   - for a running array, `md/{degraded,sync_action,sync_completed}` are
//     what CN28's md row composes its details from; each member's
//     `dev-*/block/dev` names a foreign member in its error, and its
//     `dev-*/state` and `dev-*/slot` say whether md has failed it and moved
//     it out of its slot, the member CN12's member reconciliation puts
//     back. That reconciliation compares members by dm name alone.
//
// `mdadm --detail` (and `--detail --scan`) would be the obvious source and is
// deliberately never run by this agent: it loads a superblock from whichever
// member opens first, a dead one included. Of Md's probes only the assembly's
// `mdadm --examine` (HasSuperblock) still reads a member, and only a leg
// available this pass.
// ---------------------------------------------------------------------------

const sysfsBlockDir = "/sys/block"

// mdBlockEntryPattern matches an array's kernel node under /sys/block: mdN
// by default, or md_<name> — what mdadm makes of /dev/md/<name> when
// mdadm.conf says `CREATE names=yes` (architecture.md, md names, sizes
// CnMdDevName for that node). The /dev/md/<name> entries themselves are udev
// symlinks and never appear here. "mdN" below stands for either spelling.
var mdBlockEntryPattern = regexp.MustCompile(`^md([0-9]+|_.+)$`)

// mdRunningStates are the array_state values dnv treats as a running array:
// Detail reads md/degraded, md/sync_action and md/sync_completed for these
// alone, ensureGroup reconciles only these, and CN28 reports them OK. clean
// and active are the steady pair (active for ~200 ms after a write);
// active-idle is an active array whose safe-mode timer has fired;
// write-pending is a superblock write in flight, which lasts seconds while it
// is stuck on a dead member; readonly and read-auto are a read-only array.
// Everything else — inactive (assembled, not started), broken, clear, a value
// this list does not know — is not running.
var mdRunningStates = map[string]bool{
	"clean":         true,
	"active":        true,
	"active-idle":   true,
	"write-pending": true,
	"readonly":      true,
	"read-auto":     true,
}

func mdRunning(state string) bool {
	return mdRunningStates[state]
}

// MdArray is one array as sysfs sees it.
type MdArray struct {
	// Dev is the node the sweep stops: /dev/mdN (or /dev/md_<name>), the
	// kernel node sysfs named. Never /dev/md/<name>, which depends on udev
	// having run.
	Dev  string
	Name string
	// State is array_state verbatim.
	State string
	// Members are the dm names of the array's dm members.
	Members []string
	// Foreign is set when a member is not a dm device of ours to name — a
	// bare disk, a partition, or a member whose directory vanished mid-walk.
	// An array with one is never attributed and never stopped.
	Foreign bool
}

// ListArrays enumerates every md array on the node from sysfs alone. An array
// that vanished between the listing and its reads is dropped rather than
// reported; a listing or read that did not answer — of /sys/block, or of any
// one array — is an error, never Walk's unanswered record (cnagent.md CN21),
// because a removal decision needs the whole node and the caller would
// otherwise read the array as not there and sweep on. So is the EBUSY a
// stopped array's array_state reads from the moment md marks it deleted until
// its md/ goes (cnagent.md CN12): an array another cntlr is stopping fails
// the enumeration for that window.
func (m *Md) ListArrays(ctx context.Context) ([]MdArray, error) {
	entries, ok, err := m.cmd.ListDir(ctx, sysfsBlockDir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var out []MdArray
	for _, entry := range entries {
		if !mdBlockEntryPattern.MatchString(entry) {
			continue
		}
		mdDir := mdDirOf(entry)
		state, ok, err := m.cmd.ReadAttr(ctx, mdDir+"/array_state")
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		members, ok, err := m.memberDirs(ctx, mdDir)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, mdArrayOf(entry, state, members))
	}
	return out, nil
}

// mdDirOf is the md/ directory of an array node under /sys/block.
func mdDirOf(node string) string {
	return sysfsBlockDir + "/" + node + "/md"
}

// mdArrayOf is the MdArray of one node from its array_state and its
// members: a member with no dm name makes the array Foreign.
func mdArrayOf(node, state string, members []mdMemberDir) MdArray {
	array := MdArray{
		Dev:   "/dev/" + node,
		Name:  node,
		State: state,
	}
	for _, member := range members {
		if member.dmName == "" {
			array.Foreign = true
			continue
		}
		array.Members = append(array.Members, member.dmName)
	}
	return array
}

// MdScan is the one md walk of a cntlr's read: one listing of /sys/block and
// one read of each array, which serves both md rules of the read. The group
// rows and the concat rows read Walk() under Md.Walk's rule, which records an
// array that did not answer and lets a match beside it stand (CN12); the
// verdict reads Arrays() under ListArrays' rule, which fails on any read that
// did not answer, array_state's included (CN21, CN30). A converge keeps its
// two md walks: its sweep's ListArrays, and the Md.Walk of its groups, which
// an assembly of the build brings up to date (Refresh).
type MdScan struct {
	// walk is the tolerant view, built as the scan reads.
	walk *MdWalk
	// nodes is every array node of the listing, in listing order, with
	// what the scan read of it.
	nodes []mdScanNode
}

// mdScanNode is what a scan read of one array node: its md/array_state, and
// its md/ listing with each member's dm name (memberDirs). ok false is a
// read that found nothing; err a read that did not answer.
type mdScanNode struct {
	node       string
	state      string
	stateOk    bool
	stateErr   error
	members    []mdMemberDir
	membersOk  bool
	membersErr error
}

// Scan lists /sys/block once and reads, for every array node in listing
// order, its array_state and then its md/ listing and members' dm names. An
// absent /sys/block is no array; a listing of it that did not answer is the
// scan's error, never "no arrays" (ListArrays' rule). It reads every node's
// members whatever its array_state read gave, since the walk view never
// looks at array_state.
func (m *Md) Scan(ctx context.Context) (*MdScan, error) {
	entries, ok, err := m.cmd.ListDir(ctx, sysfsBlockDir)
	if err != nil {
		return nil, err
	}
	sc := &MdScan{walk: &MdWalk{
		arrays:     make(map[string][]mdMemberDir),
		unanswered: make(map[string]error),
	}}
	if !ok {
		return sc, nil
	}
	for _, entry := range entries {
		if !mdBlockEntryPattern.MatchString(entry) {
			continue
		}
		node := mdScanNode{node: entry}
		node.state, node.stateOk, node.stateErr = m.cmd.ReadAttr(
			ctx, mdDirOf(entry)+"/array_state")
		node.members, node.membersOk, node.membersErr = m.walkNode(
			ctx, sc.walk, entry)
		sc.nodes = append(sc.nodes, node)
	}
	return sc, nil
}

// Walk is the scan under Md.Walk's rule, the walk a group row and a concat
// row read (Detail, Match): every node whose members answered, and every
// node whose md/ listing or a member's dm name did not answer recorded as
// unanswered. An array_state read never reaches it, so the EBUSY of an
// array another cntlr is stopping turns no group row of this cntlr ERROR.
func (sc *MdScan) Walk() *MdWalk {
	return sc.walk
}

// Arrays is the scan under ListArrays' rule, the verdict's md enumeration:
// the same arrays and the same first error, in the same order of reads, as
// a ListArrays over the same node — a node whose array_state is gone is
// skipped and its members are not looked at, and any read that did not
// answer is the error.
func (sc *MdScan) Arrays() ([]MdArray, error) {
	var out []MdArray
	for _, node := range sc.nodes {
		if node.stateErr != nil {
			return nil, node.stateErr
		}
		if !node.stateOk {
			continue
		}
		if node.membersErr != nil {
			return nil, node.membersErr
		}
		if !node.membersOk {
			continue
		}
		out = append(out, mdArrayOf(node.node, node.state, node.members))
	}
	return out, nil
}

// Gone verifies a stop. Only an absent sysfs directory or array_state
// "clear" counts; "inactive" is an array that still holds its members and
// would still pin them against a dmsetup remove. A read that fails is an
// error, never gone — the EBUSY array_state reads between md marking the
// stopped array deleted and its md/ going included.
func (m *Md) Gone(ctx context.Context, dev string) (bool, error) {
	name := dev
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return false, fmt.Errorf("md device %q has no node name", dev)
	}
	state, ok, err := m.cmd.ReadAttr(
		ctx, sysfsBlockDir+"/"+name+"/md/array_state")
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	return state == "clear", nil
}

// NameInUse reports whether a named array node, /dev/md/<name>, resolves to a
// block device — whether an array already runs under that name. It is an
// lsblk of the node, which reads no member device; a run that did not answer
// is an error. The named node is a symlink that depends on udev having run, so
// its "no" proves little; only assembleGroup's create guard asks, and it acts
// on the "yes" alone.
func (m *Md) NameInUse(ctx context.Context, devPath string) (bool, error) {
	_, ok, err := m.cmd.RunProbe(ctx, "lsblk",
		"--nodeps", "--noheadings", "--output", "MAJ:MIN", devPath)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// MdMember is one member of an array as sysfs reports it.
type MdMember struct {
	// DevNo is "major:minor", from dev-*/block/dev.
	DevNo string
	// DmName is dev-*/block/dm/name, or "" for a member that is not a dm
	// device (MdDetail.Foreign).
	DmName string
	// State is dev-*/state verbatim. It is a comma-separated flag list; while
	// the array runs, every member dnv creates or adds carries failfast
	// ("in_sync,failfast", "faulty,failfast", "faulty,blocked,failfast",
	// "spare,failfast"), and in an array that is not running a member reads a
	// bare "spare". Code that interprets it tests a flag for membership,
	// never the string for equality (mdFlagSet).
	State string
	// Slot is dev-*/slot verbatim: the member's place number in the array,
	// or "none" for a member that holds no place — one md has failed and
	// moved out of its slot once no IO to it was pending, or a spare with
	// no place to recover into.
	Slot string
}

// mdSlotNone is what dev-*/slot reads for a member that holds no place in
// the array.
const mdSlotNone = "none"

// mdFlagSet reports whether a member's dev-*/state flag list carries flag.
func mdFlagSet(state, flag string) bool {
	for _, set := range strings.Split(state, ",") {
		if set == flag {
			return true
		}
	}
	return false
}

// failedOutOfSlot is the member CN12's member reconciliation puts back: md
// has failed it ("faulty") and moved it out of its slot ("none"). md moves a
// failed member out by itself once no IO to it is pending, so a member that
// still holds its slot may still have IO pending, which an `mdadm --remove`
// would be refused for (cnagent.md CN12).
func (m MdMember) failedOutOfSlot() bool {
	return mdFlagSet(m.State, "faulty") && m.Slot == mdSlotNone
}

// inSync is a member md counts as holding the whole data, which CN12's
// in-sync guard looks for (inSyncGuard): "in_sync" in its state and not
// "faulty". A member md is copying onto reads "spare" with a slot number
// until the copy ends, so it never counts, nor does one md has failed. Nor
// does one md shows faulty while it still marks it in sync: md prints
// "faulty" beside "in_sync" for a member whose bad blocks it has not yet
// acknowledged, so a member that leaves leg_list may hold the only good copy
// of those regions, which its fail would lose.
func (m MdMember) inSync() bool {
	return mdFlagSet(m.State, "in_sync") && !mdFlagSet(m.State, "faulty")
}

// MdDetail is the live state of one array, read from /sys/block alone.
type MdDetail struct {
	Exists bool
	// Dev is the node sysfs named, /dev/mdN — never /dev/md/<name>, which
	// depends on udev having run.
	Dev string
	// State is md/array_state verbatim.
	State string
	// Degraded, SyncAction and SyncCompleted are md/degraded (missing,
	// failed and rebuilding members), md/sync_action and md/sync_completed
	// ("<done> / <total>" sectors, or "none" while no sync runs), verbatim.
	// md publishes the three for a running array only — an inactive one has
	// none of them — so they are read, and set, only when mdRunning(State).
	Degraded      int
	SyncAction    string
	SyncCompleted string
	Members       []MdMember
	// Foreign is set when a member is not a dm device (ListArrays' rule):
	// an array holding one is never ours to reconcile.
	Foreign bool
}

// MdWalk is one walk of /sys/block: every array node and its dev-* member
// directories, each with the dm name it carries. Every listing and read is
// one call to the OS, so the walk is what finding a group's array costs;
// Match finds a group's array in a walk, and Detail and mdDevNo read only the
// matched array's attributes, which lets a pass over g groups list each of
// the node's a arrays once instead of g times — at 32 slices, 64 groups over
// 64 arrays, 65 listings a read instead of 4160.
//
// The walk reads every array on the node, other sps' included, so an array
// whose md/ listing or a member's dm-name read did not answer — held past
// the soft timeout on the agent's OsClient semaphore, which every cntlr it
// serves shares, ctx cancelled, or an errno other than ENOENT — is recorded
// as unanswered rather than failing the walk: failing this pass for another
// sp's array would turn this sp's md rows ERROR, and an md row counts toward
// cntlr health, which is exactly the fault a failover cannot fix. Match
// decides what an unanswered array may mean for one group. An array
// another cntlr is stopping does not land here: md removes a member's block
// link as it unbinds it, so that member's dm name reads ENOENT and it is
// recorded with no dm name, which no group's names match
// (TestMdWalkUnboundMember); and an md/ that went makes its listing answer
// "no" and drops the array (TestMdWalkRefreshUnanswered).
type MdWalk struct {
	arrays map[string][]mdMemberDir // "mdN" → its members
	// unanswered are the nodes whose md/ listing, or a member's dm name,
	// did not answer on the latest look, with the error.
	unanswered map[string]error
}

// Walk lists /sys/block and every array's md/ directory, reading each
// member's dm name. An array that vanished between the two listings is left
// out, and one whose listing or reads did not answer is recorded as
// unanswered; only a /sys/block listing that did not answer is an error of
// the walk itself, never "no arrays" (ListArrays' rule).
func (m *Md) Walk(ctx context.Context) (*MdWalk, error) {
	w := &MdWalk{
		arrays:     make(map[string][]mdMemberDir),
		unanswered: make(map[string]error),
	}
	if err := m.Refresh(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

// Refresh brings a walk up to date after the pass itself changed the node's
// arrays — an assembly or a create adds one. It re-lists /sys/block, drops
// the nodes that went, and walks again only a node it has not seen, saw with
// no member, recorded as unanswered, or recorded with member directories that
// are not all still there under the same dm names — or whose check did not
// answer, which is a reason to walk that node again, not an error (the node
// may be any sp's array; the walk of it decides). A member of an array
// another cntlr is stopping fails the check as not still there once md has
// removed its block link. That check is reads, not listings, and it is not
// optional: other cntlrs converge beside this one, so a node this walk
// recorded may have been stopped and its name taken by the very array this
// pass just assembled. A node whose walk does not answer loses its record and
// is recorded as unanswered (MdWalk), never an error of the refresh.
//
// Outside the tests the walk Refresh starts from has no unanswered node: Walk
// hands it an empty walk, and its other caller, ensureGroup after an
// assembly, runs only after Detail read absent, which it reads only when no
// node is unanswered. Dropping an unanswered node that went — its /sys/block
// entry, or only its md/, which a stopping array loses first — and clearing
// one that answers are this function's contract all the same, pinned by
// calling it directly (TestMdWalkUnansweredArray, TestMdWalkRefreshUnanswered).
func (m *Md) Refresh(ctx context.Context, w *MdWalk) error {
	entries, ok, err := m.cmd.ListDir(ctx, sysfsBlockDir)
	if err != nil {
		return err
	}
	listed := make(map[string]bool, len(entries))
	if ok {
		for _, entry := range entries {
			if mdBlockEntryPattern.MatchString(entry) {
				listed[entry] = true
			}
		}
	}
	for node := range w.arrays {
		if !listed[node] {
			delete(w.arrays, node)
		}
	}
	for node := range w.unanswered {
		if !listed[node] {
			delete(w.unanswered, node)
		}
	}
	for _, node := range entries {
		if !listed[node] {
			continue
		}
		if recorded, seen := w.arrays[node]; seen && len(recorded) > 0 &&
			m.sameMembers(ctx, recorded) {
			continue
		}
		m.walkNode(ctx, w, node)
	}
	return nil
}

// walkNode reads one array node's members (memberDirs) into a walk, the one
// read of a node that Refresh and Scan share: a read that did not answer
// records the node as unanswered, a md/ directory gone drops it, and members
// that answered replace whatever the walk held for it. It returns the read
// as it came, for the scan's strict view.
func (m *Md) walkNode(
	ctx context.Context,
	w *MdWalk,
	node string,
) ([]mdMemberDir, bool, error) {
	members, ok, err := m.memberDirs(ctx, mdDirOf(node))
	if err != nil {
		delete(w.arrays, node)
		w.unanswered[node] = err
		return nil, false, err
	}
	delete(w.unanswered, node)
	if !ok {
		delete(w.arrays, node)
		return nil, false, nil
	}
	w.arrays[node] = members
	return members, true, nil
}

// sameMembers reports whether every member directory a walk recorded is still
// there and still carries the dm name it had. It reads block/dev, not the
// member's own state: md unbinds a member (--stop, --remove) by clearing its
// array pointer and removing its block link, and the dev-* directory stays
// until md deletes it at its next unlock, every attribute of its own (state
// among them) reading ENODEV meanwhile. Any read that fails here only means
// "walk this node again"; memberDirs, which keeps the did-not-answer rule,
// takes it from there.
func (m *Md) sameMembers(ctx context.Context, members []mdMemberDir) bool {
	for _, member := range members {
		_, ok, err := m.cmd.ReadAttr(ctx, member.dir+"/block/dev")
		if err != nil || !ok {
			return false
		}
		dmName, _, err := m.cmd.ReadAttr(ctx, member.dir+"/block/dm/name")
		if err != nil || dmName != member.dmName {
			return false
		}
	}
	return true
}

// Detail finds, in a walk, the one array whose members include any of
// dmNames, a group's leg wrapper names (Match), and reads its state from
// sysfs. It never runs mdadm and never resolves /dev/md/<name>.
//
// Detail runs no mdadm because `mdadm --detail`
// opens a member device: it loads the superblock from the first member that
// opens, and when that member's DN side has gone the read sits in the
// multipath head's requeue list until the path's failfast expires — up to
// ~13 s after the side died (the keep-alive timeout, error recovery, then
// fast_io_fail_tmo), far past the soft timeout. A kill would turn the group's
// md row ERROR for a member fault the leg row already reports, and an md row
// counts toward cntlr health. A killed read taken as absent would also let a
// teardown skip `mdadm --stop` and leak the array's leg wrappers.
//
// No array holding any of dmNames is "absent" (Exists false, nil error): the
// assembly is what fixes that. So is a matched array whose array_state has
// gone by the read — stopped since the walk, as Gone reads it. Two arrays
// holding them is an error — neither may be reconciled while the other holds
// the rest of the group. A read that did not answer is an error, never absent
// (ListArrays' rule), and so is an unanswered node of the walk whenever the
// answer would otherwise be absent — no match, or a match that went: it may
// be the group's own. Absent therefore always means every array of the walk
// answered. Beside a match that is still there an unanswered node is left
// alone — it is most likely another sp's array whose read timed out, or at
// worst a second array of this group that the two-array check cannot see
// this pass — so that a read of another sp's array never turns this group's
// row ERROR.
func (m *Md) Detail(
	ctx context.Context,
	w *MdWalk,
	dmNames []string,
) (*MdDetail, error) {
	node, err := w.Match(dmNames)
	if err != nil {
		return nil, err
	}
	if node != "" {
		detail, err := m.readDetail(ctx, "/dev/"+node,
			sysfsBlockDir+"/"+node+"/md", w.arrays[node])
		if err != nil || detail.Exists {
			return detail, err
		}
		// The match stopped between the walk and this read: absent, under
		// the same rule as no match at all.
	}
	if err := w.unansweredErr(); err != nil {
		return nil, err
	}
	return &MdDetail{}, nil
}

// Match is the array node a walk holds for a group (CN12's match): the one
// array whose members include any of dmNames, the group's leg_list wrapper
// names, so spares do not count. "" with no error when none matches and
// every array answered; an error for two matches, or for no match beside an
// array that did not answer, which may be the group's own. A degraded array
// matches: one member the group names is enough.
func (w *MdWalk) Match(dmNames []string) (string, error) {
	want := make(map[string]bool, len(dmNames))
	for _, name := range dmNames {
		want[name] = true
	}
	nodes := make([]string, 0, len(w.arrays))
	for node := range w.arrays {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	var found string
	for _, node := range nodes {
		if !holdsAny(w.arrays[node], want) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf(
				"arrays /dev/%s and /dev/%s both hold legs of the group",
				found, node)
		}
		found = node
	}
	if found != "" {
		return found, nil
	}
	return "", w.unansweredErr()
}

// unansweredErr is the error of an absence the walk cannot vouch for: the
// first node, by name, whose md/ listing or a member's dm name did not
// answer. nil when every array of the walk answered.
func (w *MdWalk) unansweredErr() error {
	nodes := sortedKeys(w.unanswered)
	if len(nodes) == 0 {
		return nil
	}
	return fmt.Errorf("no array holds legs of the group, but /dev/%s did "+
		"not answer: %w", nodes[0], w.unanswered[nodes[0]])
}

// mdDevNo is the device number of a group's array, which the pool concats
// map: /sys/block/<node>/dev of the node Match finds in the pass's md walk,
// the array the group's own row reads, so a read and a converge take
// the number from one array and their concat rows agree. Nothing here waits
// for the /dev/md/<name> link udev makes. It reads the number apart from the
// array's other attributes, so a failed md/sync_completed read fails the
// group's row and not the concat rows. Each of its errors — the walk's,
// Match's, no match and the number read's — names the group, because the
// concat rows that carry it are keyed by the slice.
func (s *CnAgentServer) mdDevNo(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
) (string, error) {
	walk, err := plan.mdWalks.get(ctx)
	if err != nil {
		return "", fmt.Errorf("group %s: %w", gp.resName, err)
	}
	node, err := walk.Match(gp.legNames())
	if err != nil {
		return "", fmt.Errorf("group %s: %w", gp.resName, err)
	}
	if node == "" {
		return "", fmt.Errorf("group %s: no array holds its legs",
			gp.resName)
	}
	devNo, err := s.blockDevNo(ctx, node)
	if err != nil {
		return "", fmt.Errorf("group %s: %w", gp.resName, err)
	}
	return devNo, nil
}

// mdWalkOnce hands every group row and every concat row of one pass the same
// walk (cntlrPlan.mdWalks). A converge takes it at the first group that needs
// it, so a converge with no md group lists nothing; a read hands over the walk
// view of its reading's md walk (cnReading.mdWalks), already taken. A nil one,
// a plan whose builder set none, is an error of every read rather than a walk
// of its own.
type mdWalkOnce struct {
	md   *Md
	walk *MdWalk
	err  error
	done bool
}

func (o *mdWalkOnce) get(ctx context.Context) (*MdWalk, error) {
	if o == nil {
		return nil, fmt.Errorf("no md walk in this pass")
	}
	if !o.done {
		o.walk, o.err = o.md.Walk(ctx)
		o.done = true
	}
	return o.walk, o.err
}

// mdMemberDir is one dev-* entry of an array and the dm name it carries.
type mdMemberDir struct {
	dir    string
	dmName string // "" for a member that is not a dm device
}

func sortedKeys(m map[string]error) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func holdsAny(members []mdMemberDir, want map[string]bool) bool {
	for _, member := range members {
		if member.dmName != "" && want[member.dmName] {
			return true
		}
	}
	return false
}

// memberDirs lists an array's dev-<kname> member directories and reads each
// one's dm name. The rdN entries beside them are symlinks to the same
// directories and are skipped. ok is false when the md/ directory is gone.
func (m *Md) memberDirs(
	ctx context.Context,
	mdDir string,
) ([]mdMemberDir, bool, error) {
	entries, ok, err := m.cmd.ListDir(ctx, mdDir)
	if err != nil || !ok {
		return nil, false, err
	}
	var out []mdMemberDir
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "dev-") {
			continue
		}
		dir := mdDir + "/" + entry
		dmName, _, err := m.cmd.ReadAttr(ctx, dir+"/block/dm/name")
		if err != nil {
			return nil, false, err
		}
		out = append(out, mdMemberDir{dir: dir, dmName: dmName})
	}
	return out, true, nil
}

// readDetail reads the array Detail matched. array_state comes first because
// it decides which attributes exist at all: an inactive array has no
// md/degraded, md/sync_action or md/sync_completed, so for a state that is not
// running they are not read, rather than read and found missing.
func (m *Md) readDetail(
	ctx context.Context,
	dev string,
	mdDir string,
	members []mdMemberDir,
) (*MdDetail, error) {
	state, ok, err := m.cmd.ReadAttr(ctx, mdDir+"/array_state")
	if err != nil {
		return nil, err
	}
	if !ok {
		// Stopped between the walk and this read: gone, as Gone reads it.
		return &MdDetail{}, nil
	}
	detail := &MdDetail{Exists: true, Dev: dev, State: state}
	if mdRunning(state) {
		degraded, err := m.presentAttr(ctx, mdDir+"/degraded")
		if err != nil {
			return nil, err
		}
		if detail.Degraded, err = strconv.Atoi(degraded); err != nil {
			return nil, fmt.Errorf("%s md/degraded %q: %w", dev, degraded, err)
		}
		if detail.SyncAction, err = m.presentAttr(
			ctx, mdDir+"/sync_action"); err != nil {
			return nil, err
		}
		if detail.SyncCompleted, err = m.presentAttr(
			ctx, mdDir+"/sync_completed"); err != nil {
			return nil, err
		}
	}
	for _, member := range members {
		devNo, err := m.presentAttr(ctx, member.dir+"/block/dev")
		if err != nil {
			return nil, err
		}
		memberState, err := m.presentAttr(ctx, member.dir+"/state")
		if err != nil {
			return nil, err
		}
		slot, err := m.presentAttr(ctx, member.dir+"/slot")
		if err != nil {
			return nil, err
		}
		detail.Members = append(detail.Members, MdMember{
			DevNo:  devNo,
			DmName: member.dmName,
			State:  memberState,
			Slot:   slot,
		})
		if member.dmName == "" {
			detail.Foreign = true
		}
	}
	return detail, nil
}

// presentAttr reads an attribute the kernel publishes whenever its directory
// exists. ENOENT here is an array or member that went away mid-read, and it
// is reported rather than read as an empty value.
func (m *Md) presentAttr(ctx context.Context, path string) (string, error) {
	value, ok, err := m.cmd.ReadAttr(ctx, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("read %s: vanished mid-read", path)
	}
	return value, nil
}

// foreignErr names the first member of a Foreign array that is not a dm
// device.
func (d *MdDetail) foreignErr() error {
	for _, member := range d.Members {
		if member.DmName == "" {
			return fmt.Errorf("array %s holds a foreign member %s",
				d.Dev, member.DevNo)
		}
	}
	return fmt.Errorf("array %s holds a foreign member", d.Dev)
}

// mdSyncWords are the progress words of a running sync: mdadm's State-line
// words where it has one, and "repairing" for a repair.
var mdSyncWords = map[string]string{
	"recover": "recovering",
	"resync":  "resyncing",
	"check":   "checking",
	"repair":  "repairing",
	"reshape": "reshaping",
}

// mdStateLine composes the CN28 details of a running array: array_state
// verbatim, then "degraded" while md/degraded is non-zero, then the progress
// word of a sync that is actually running, followed by its
// "(<done> / <total>)" sectors from md/sync_completed. The words are the ones
// mdadm's State line uses, which the suites grep; the state and the sectors
// suffix are sysfs's own (mdadm prints a readonly array as "clean" and its
// progress as a separate "Rebuild Status : N% complete" line).
//
// The word needs sync_completed, not sync_action alone: sync_completed reads
// "none" exactly when no sync thread runs, while sync_action can read
// "recover" for seconds on a degraded array that has nothing to rebuild onto
// (a member failed with a write in flight). mdadm prints no word there either.
func mdStateLine(d *MdDetail) string {
	parts := []string{d.State}
	if d.Degraded > 0 {
		parts = append(parts, "degraded")
	}
	word := mdSyncWords[d.SyncAction]
	if word == "" || d.SyncCompleted == "" || d.SyncCompleted == "none" {
		return strings.Join(parts, ", ")
	}
	parts = append(parts, word)
	return strings.Join(parts, ", ") + " (" + d.SyncCompleted + ")"
}

// HasSuperblock reports whether a member device carries an md superblock.
// `mdadm --examine` exits non-zero on a device without one, which is the
// "no superblock" answer of the assembly cases (cnagent.md CN12) rather than
// an error.
//
// A run that did not answer is an error and must never read as "no
// superblock": `--examine` opens and reads the member device, so on a leg
// whose DN side has gone it blocks until failfast and gets killed — and if
// that happened to every member of an available group,
// assembleGroup would take the answer as case 1 and `mdadm --create
// --assume-clean` over live data.
//
// An answer is not always the truth either: once the path's failfast has
// expired the read fails with an IO error, and mdadm answers that exactly as
// it answers a member without a superblock ("No md superblock detected", exit
// status 1), which reads here as false. Only a leg that read available earlier
// in the pass is probed, so it takes a failfast expiring between that read and
// this answer (cnagent.md CN12, Known limits).
func (m *Md) HasSuperblock(ctx context.Context, dev string) (bool, error) {
	_, ok, err := m.cmd.RunProbe(ctx, "mdadm", "--examine", "--export", dev)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// MdCreateConf carries the options every dnv array is created with
// (cnagent.md CN12): an internal write-intent bitmap,
// failfast members, and a data offset that clears the leg's meta region
// (architecture.md, Group on-leg layout: meta region, data region, health
// block).
type MdCreateConf struct {
	DevPath        string
	ArrayName      string
	RaidDevices    int
	BitmapChunkKiB uint64
	DataOffsetKiB  uint64
	AssumeClean    bool
}

// Create builds a fresh array. A member list shorter than RaidDevices is
// padded with `missing`, the mdadm idiom for a degraded create; cnagent never
// gets there — CN12 case 1 creates only when every leg_list member of the
// group is available, which assembleGroup enforces before it calls this.
func (m *Md) Create(
	ctx context.Context,
	conf MdCreateConf,
	members []string,
) error {
	args := []string{
		"--create", conf.DevPath,
		"--name", conf.ArrayName,
		"--level", "1",
		"--raid-devices", strconv.Itoa(conf.RaidDevices),
		"--bitmap", "internal",
		"--bitmap-chunk",
		strconv.FormatUint(conf.BitmapChunkKiB, 10) + "K",
		"--data-offset",
		strconv.FormatUint(conf.DataOffsetKiB, 10) + "K",
		"--homehost", "any",
		"--run",
	}
	if conf.AssumeClean {
		args = append(args, "--assume-clean")
	}
	// --failfast applies to the devices listed after it.
	args = append(args, "--failfast")
	args = append(args, members...)
	for i := len(members); i < conf.RaidDevices; i++ {
		args = append(args, "missing")
	}
	return m.cmd.RunOk(ctx, "mdadm", args...)
}

// Assemble starts an existing array from the members that carry a superblock.
// It deliberately passes no `--run`: with a single available member mdadm
// itself decides whether a degraded start is safe (the assemble case of
// cnagent.md CN12), and a
// refusal must leave the group in error rather than force a start.
func (m *Md) Assemble(
	ctx context.Context,
	devPath string,
	arrayName string,
	members []string,
) error {
	args := []string{"--assemble", devPath, "--name", arrayName}
	args = append(args, members...)
	return m.cmd.RunOk(ctx, "mdadm", args...)
}

func (m *Md) Add(ctx context.Context, devPath, member string) error {
	return m.cmd.RunOk(ctx, "mdadm", devPath, "--add", "--failfast", member)
}

func (m *Md) Fail(ctx context.Context, devPath, member string) error {
	return m.cmd.RunOk(ctx, "mdadm", devPath, "--fail", member)
}

func (m *Md) Remove(ctx context.Context, devPath, member string) error {
	return m.cmd.RunOk(ctx, "mdadm", devPath, "--remove", member)
}

func (m *Md) Stop(ctx context.Context, devPath string) error {
	return m.cmd.RunOk(ctx, "mdadm", "--stop", devPath)
}

// ---------------------------------------------------------------------------
// CN12 — groups
// ---------------------------------------------------------------------------

// ensureGroup converges one group device. A RedundNone group is a dm-linear
// over its single leg's data region (ensureLinearGroup); a RedundMdRaid1
// group instantiates the assembly cases of cnagent.md CN12 over the member
// leg wrappers. Spare legs are never members
// (architecture.md, Spare legs) — they stay connected, wrapped and probed.
//
// A provisioning-deferred group never gets here: the build phase reports it
// PROVISIONING and skips it ([D15]). The leg-count check of ensureLinearGroup
// is kept explicit all the same, so gp.legs[0] can never be indexed on an
// empty member list whatever a future caller does.
//
// legRows are the pass's leg rows, which the member reconciliation's put-back
// reads (reconcileMembers).
func (s *CnAgentServer) ensureGroup(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	available map[uint64]bool,
	legRows map[uint64]*pb.ResInfo,
) error {
	if !gp.raid1 {
		return s.ensureLinearGroup(ctx, gp, available)
	}
	walk, err := plan.mdWalks.get(ctx)
	if err != nil {
		return err
	}
	detail, err := s.md.Detail(ctx, walk, gp.legNames())
	if err != nil {
		return err
	}
	if !detail.Exists {
		if err := s.assembleGroup(ctx, gp, available); err != nil {
			return err
		}
		if err := s.md.Refresh(ctx, walk); err != nil {
			return err
		}
		detail, err = s.md.Detail(ctx, walk, gp.legNames())
		if err != nil {
			return err
		}
		if !detail.Exists {
			return fmt.Errorf("array %s did not start", gp.devPath)
		}
	}
	if detail.Foreign {
		return detail.foreignErr()
	}
	// An array that is not running — inactive, broken — is left exactly as
	// it is: member reconciliation is for a running mirror, and an --add,
	// --fail or --remove on an array that has not started could only change
	// which members an operator's start would find. Its state is the group's
	// error.
	if !mdRunning(detail.State) {
		return fmt.Errorf("array %s is %s", detail.Dev, detail.State)
	}
	return s.reconcileMembers(ctx, gp, detail, available, legRows)
}

// ensureLinearGroup converges a RedundNone group's dm-linear over its leg
// wrapper (cnagent.md CN12), and creates or reloads it only over an
// available leg. The layers above do IO through the group, and a side that
// has not flipped to this CN yet exports dm-error to it: a pool, a raid0 and
// an ns-dev built over such a leg would let a promotion that came before the
// sides' switch-over ([D16]) move its namespaces optimized over IO errors.
// A device that is there with the table it wants is kept whatever the leg's
// availability, resumed if it is suspended, as ensureDmSingle does: a
// primary that serves keeps serving through a short loss of its leg's path,
// with no teardown and no reload. A late leg under a device that is missing
// or maps something else fails the group with "no available leg" and
// touches nothing. A missing device leaves the slice's concat without the
// group's number: a pool not built yet, as at a promotion's first converge,
// waits, and every layer above it with it, the namespaces inaccessible
// (CN16); a pool that serves when a grow adds the group keeps serving at its
// old size, with the raid0s and ns-devs over it, while its concat and pool
// rows read ERROR. The CN10 retry that the late member registers (build)
// then finds the leg available, builds the group and builds or grows the
// pool.
func (s *CnAgentServer) ensureLinearGroup(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
) error {
	if len(gp.legs) != 1 {
		return fmt.Errorf(
			"RedundNone group has %d legs, want 1", len(gp.legs))
	}
	lp := gp.legs[0]
	if available[lp.legId] {
		legNo, err := s.dmDevNo(ctx, nil, lp.name)
		if err != nil {
			return err
		}
		return s.ensureDmLinear(ctx, gp.dmName, gp.dataSectors,
			legNo, gp.dataOffsetSectors)
	}
	dev, err := s.dm.Info(ctx, gp.dmName)
	if err != nil {
		return err
	}
	if dev == nil {
		return errNoAvailableLeg()
	}
	legNo, err := s.dmDevNo(ctx, nil, lp.name)
	if err != nil {
		return err
	}
	targets, err := s.dm.Table(ctx, gp.dmName)
	if err != nil {
		return err
	}
	args := []string{legNo, strconv.FormatUint(gp.dataOffsetSectors, 10)}
	if dev.ReadOnly ||
		!dmTableMatches(targets, "linear", gp.dataSectors, args, 0) {
		return errNoAvailableLeg()
	}
	if dev.Suspended {
		return s.dm.Resume(ctx, gp.dmName)
	}
	return nil
}

// errNoAvailableLeg is the error of a group this pass leaves unbuilt because
// none of its legs is available (cnagent.md CN12), one text for both kinds of
// group.
func errNoAvailableLeg() error {
	return fmt.Errorf("no available leg")
}

// assembleGroup is the case selection of cnagent.md CN12: probe each
// available member for an md superblock and either create the array or
// assemble it from the members that have one.
func (s *CnAgentServer) assembleGroup(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
) error {
	var members []string
	var withSuperblock []string
	for _, lp := range gp.legs {
		if !available[lp.legId] {
			continue
		}
		members = append(members, lp.path)
		// A probe that did not answer aborts the whole assembly: with no
		// answer this pass cannot tell case 1 from case 2, and guessing case 1
		// creates over live data.
		has, err := s.md.HasSuperblock(ctx, lp.path)
		if err != nil {
			return err
		}
		if has {
			withSuperblock = append(withSuperblock, lp.path)
		}
	}
	if len(members) == 0 {
		return errNoAvailableLeg()
	}
	conf := MdCreateConf{
		DevPath:        gp.devPath,
		ArrayName:      gp.mdArrayName,
		RaidDevices:    len(gp.legs),
		BitmapChunkKiB: gp.bitmapChunkKiB,
		DataOffsetKiB:  gp.dataOffsetKiB,
	}
	if len(withSuperblock) == 0 {
		// Case 1: --assume-clean is correct only when **every** member was
		// probed. A side is never exported before the provisioning protocol
		// (architecture.md, Side provisioning protocol) has zeroed its meta
		// region, where the md superblock sits, and its first data block,
		// and its `provisioned` gate has opened ([D15]); the effective
		// desired state defers any group whose legs are still provisioning
		// ([D15]); and ids are never reused — so a superblock-free leg can
		// only be a freshly provisioned side — short of a read that failed
		// after the path's failfast, which mdadm answers the same way
		// (HasSuperblock). Only a slice's meta group's members are zeroed
		// whole; a data group's and a clone meta group's are zeroed no
		// further than their first data block, and the rest may differ
		// between them, which a clean create does not resync: nothing dnv
		// stacks on a data group's array reads such a block before writing
		// it through the array, because dm-thin writes a block whole before
		// anything reads it ([D15]); and the dm-clone over a clone meta
		// group's array finds the zeroed first data block, where its
		// superblock goes, formats its metadata there and reads only
		// metadata blocks it has written through the array (CN18). But that
		// argument covers the legs
		// actually examined. An unavailable member may be the one carrying
		// the group's data (its DN rebooting, its path mid-ANA-move), and
		// creating over the survivors would resync the data away. cnagent.md
		// CN12 puts a single available member in case 2 (assemble, and mdadm
		// decides on a degraded start), never in case 1.
		if len(members) != len(gp.legs) {
			return fmt.Errorf(
				"only %d of %d legs available and none carries a superblock",
				len(members), len(gp.legs))
		}
		// Nor while an array already runs under the group's name. Detail
		// finds the array by its members, so one holding NONE of the leg_list
		// wrappers — both legs switched out, and the parked ones released,
		// while this cntlr was not converging — never reaches reconcile and
		// lands here instead, with every fresh leg answering "no
		// superblock". A create would put a second array under the name
		// mdadm gives the group's array, and the guard keeps it from doing
		// so. The pool's concat finds the group's array by its legs
		// (mdDevNo), never by that name, so until the group has an array of
		// its own the concat rows read an error rather than take the running
		// array's number. The guard errs only towards refusing, and a false
		// refusal needs a stale node of this very name: short of the case
		// above, a group whose legs carry no superblock has never had an
		// array that could have left one.
		inUse, err := s.md.NameInUse(ctx, gp.devPath)
		if err != nil {
			return err
		}
		if inUse {
			return fmt.Errorf(
				"an array runs under %s holding none of the group's legs",
				gp.devPath)
		}
		conf.AssumeClean = true
		return s.md.Create(ctx, conf, members)
	}
	// Case 2: assemble from the members that carry metadata; anything left
	// out — a freshly provisioned addition or a stale-metadata re-add — is
	// re-added by reconcileMembers (cnagent.md CN12).
	return s.md.Assemble(ctx, gp.devPath, gp.mdArrayName, withSuperblock)
}

// reconcileMembers covers SwitchSpareLeg with no extra mechanism (CN12): a
// member the array holds that is no longer in leg_list is failed and removed,
// but only while a member that stays keeps the data (inSyncGuard); an
// available leg_list member the array lacks is added, and md then resyncs —
// a bitmap catch-up for a briefly absent leg, a full rebuild for a promoted
// spare. And it puts a member md has failed back: a leg_list member md has
// failed and moved out of its slot (MdMember.failedOutOfSlot), on a leg that
// can take it (legTakesMember), is removed, and the add loop adds it again,
// which mdadm turns into a re-add that md resyncs from its write-intent
// bitmap. md keeps its claim on a failed member until the member is removed,
// so an --add alone is refused. The put-back is an mdadm child the soft
// timeout can kill, never a write to the member's sysfs state, which the
// agent's own process would make and which waits on md's lock for as long as
// a superblock write is stuck on a member.
//
// The held set is the dm names sysfs gave the array's members
// (dev-*/block/dm/name), compared with the leg_list wrapper names — the key
// Detail matched the array by — and each held member is addressed by its dm
// path. Nothing here reads a device number: were the wanted set keyed by the
// number of each leg_list wrapper, a number read that did not answer would
// read as "not wanted" and fail and remove that wrapper's in-sync member. A
// leg whose wrapper this pass could not build is never available, so the add
// loop leaves it alone, and its member, if md still holds one, stays wanted
// and held, failed or not.
// `mdadm --fail` and `--remove` open the member's path (open only, no IO) and
// then act on its device number. `--remove` can sleep in md's suspend for as
// long as a superblock write is stuck on the member — seconds after its side
// dies with a write in flight — and the soft timeout then kills it with the
// member still held: this pass fails before the add loop, so the promoted
// spare is not added either. A remove or an add that fails is the group's
// error for the pass, a row, which registers no background retry of its own.
// What re-drives it is the read's verdict, which names a leg_list member
// that this function would put back or add on a leg the read finds able to
// take it (putBackNames, cnagent.md CN30), so the worker re-sends
// the SyncupCntlr whose converge tries again; the CN10 retry tries again too
// when the same pass registers it for something else, such as a leg that
// failed to converge or any leg_list member of this cntlr that is not
// available. The switched-out member itself has left leg_list and is never
// named. Nothing here remembers the failure. A refusal of the in-sync guard
// is re-driven the same way once it clears: until then the read's row reads
// ERROR too and its verdict names nothing for the group, and the first read
// that finds a member that stays in sync on an available leg reads the row
// OK and names the promoted spare the array lacks once its leg can take it.
// The refusal skips the put-back too, so a leg_list member md has failed
// stays out while the guard holds; when it is the only member that stays,
// nothing brings it back in sync and the guard does not clear by itself
// (cnagent.md CN12, Known limits).
func (s *CnAgentServer) reconcileMembers(
	ctx context.Context,
	gp *grpPlan,
	detail *MdDetail,
	available map[uint64]bool,
	legRows map[uint64]*pb.ResInfo,
) error {
	// A Foreign array never gets here (ensureGroup), so every member has a
	// dm name.
	held := make(map[string]MdMember, len(detail.Members))
	for _, member := range detail.Members {
		held[member.DmName] = member
	}
	wanted := make(map[string]bool, len(gp.legs))
	for _, lp := range gp.legs {
		wanted[lp.name] = true
	}
	// The in-sync guard comes before the first --fail, and its refusal
	// leaves the array exactly as it is: no fail, no put-back and no add
	// this pass. The test is the same for every member that leaves — the
	// member it asks for stays in leg_list, which this loop never fails —
	// so one test guards each fail of the loop.
	if leaver := inSyncGuard(gp, detail, func(lp *legPlan) bool {
		return available[lp.legId]
	}); leaver != "" {
		return errInSyncGuard(leaver, detail.Dev)
	}
	// Extras leave before promotions arrive, so the array never briefly
	// holds more members than `--raid-devices`.
	for _, member := range detail.Members {
		if wanted[member.DmName] {
			continue
		}
		path := s.nf.DmPath(member.DmName)
		if err := s.md.Fail(ctx, gp.devPath, path); err != nil {
			return err
		}
		if err := s.md.Remove(ctx, gp.devPath, path); err != nil {
			return err
		}
	}
	// The put-back's remove. A removed member is no longer held, so the
	// add loop below adds it in this same pass.
	for _, lp := range gp.legs {
		member, ok := held[lp.name]
		if !ok || !member.failedOutOfSlot() ||
			!legTakesMember(available[lp.legId], legRows[lp.legId]) {
			continue
		}
		if err := s.md.Remove(ctx, gp.devPath, lp.path); err != nil {
			return err
		}
		delete(held, lp.name)
	}
	for _, lp := range gp.legs {
		if _, ok := held[lp.name]; ok || !available[lp.legId] {
			continue
		}
		if err := s.md.Add(ctx, gp.devPath, lp.path); err != nil {
			return err
		}
	}
	return nil
}

// legTakesMember is CN12's test of a leg whose member the reconciliation puts
// back: the leg serves this pass (serves) and its own row reads OK, which on
// the primary is the leg health probe's last result (legProbeOutcome). A leg
// whose disk returns errors usually fails its probe too, and is left to the
// leg repair (dnv-worker.md AR8) rather than put back on every pass. serves
// is the leg's availability; the verdict's read-only form of this test is
// putBackNames', which reads the row first and only then looks the path up.
func legTakesMember(serves bool, legRow *pb.ResInfo) bool {
	return serves && legRow.GetStatus() == pb.ResStatus_RES_STATUS_OK
}

// inSyncGuard is CN12's in-sync guard over a group's running array, which
// the converge (reconcileMembers) and the read (probeGroup) both apply: the
// dm name of the first member the array holds that is no longer in leg_list,
// when no member that stays in leg_list is in sync (MdMember.inSync) on a leg
// that serves; "" when no member leaves, or when such a member keeps the
// data. serves is the leg's availability as the caller can read it: the
// converge's available map, and for a read legServes, the part of it a read
// can see, so the two report the same row short of what legServes does not
// read (cnagent.md, Known limits). serves is called only for a
// held, in-sync member of leg_list, and only once a member leaves, so a read
// of a group no switch has touched makes no lookup for it.
//
// md answers a fail of the last in-sync member of a mirror by marking the
// whole array broken (CN29), so a fail of a member that left leg_list needs
// another to keep the data. That member must be one md marks in sync on an
// available leg, because on an array with no IO md still marks a dead member
// in sync: the fail would go through, the add after it would start a copy
// that reads the dead member, and that read breaks the array. And it must
// stay in leg_list, because a member that leaves is about to be failed too.
// The guard holds whatever the leaving member's own state: with no such
// member, the data sits only on a member that leaves, which the loop fails
// too, or on a leg that does not serve, which the copy onto the added member
// would read, or on a member md shows faulty beside in_sync, whose bad
// blocks md has not yet acknowledged, so that the member that leaves may hold
// the only good copy of those regions (MdMember.inSync).
func inSyncGuard(
	gp *grpPlan,
	detail *MdDetail,
	serves func(lp *legPlan) bool,
) string {
	wanted := make(map[string]bool, len(gp.legs))
	for _, lp := range gp.legs {
		wanted[lp.name] = true
	}
	leaver := ""
	held := make(map[string]MdMember, len(detail.Members))
	for _, member := range detail.Members {
		held[member.DmName] = member
		if leaver == "" && !wanted[member.DmName] {
			leaver = member.DmName
		}
	}
	if leaver == "" {
		return ""
	}
	for _, lp := range gp.legs {
		if member, ok := held[lp.name]; ok && member.inSync() && serves(lp) {
			return ""
		}
	}
	return leaver
}

// errInSyncGuard is the group's error while the in-sync guard holds
// (inSyncGuard), one text for the converge and the read, naming the member
// it keeps from being failed and the array.
func errInSyncGuard(member, dev string) error {
	return fmt.Errorf("member %s left leg_list but is not failed: "+
		"no other member of %s is in sync on an available leg of leg_list",
		member, dev)
}

// putBackNames is what a cntlr's verdict names beside its leftovers for one
// md group (cnagent.md CN30): the dm name of each leg_list member that the
// converge's member reconciliation would put back or add (CN12) — one md has
// failed and moved out of its slot, or one the running array lacks — on a leg
// that can take it. detail is the group's running array as its row read it
// OK, nil for any other row, the in-sync guard's ERROR among them
// (probeGroup); legRows are the call's leg rows. legServes is what a
// call can read of a leg's availability without building anything, so no
// syncup is brought for a leg whose path is not live and optimized; a step
// only the converge takes, the connect of a provisioned side that has no
// controller, can still fail, and the member is then named again by every
// read, and each round that answers from such a read brings the SyncupCntlr
// again (cnagent.md, Known limits). A converge's code never names it
// (convergeCntlr), so the round that answers from the re-send's observation
// brings none for it. A member that still holds its slot, one being
// recovered or a spare is never named. The late-member retry does not count
// any of them (hasLateMember).
func (s *CnAgentServer) putBackNames(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	detail *MdDetail,
	legRows map[uint64]*pb.ResInfo,
) []string {
	if detail == nil {
		return nil
	}
	held := make(map[string]MdMember, len(detail.Members))
	for _, member := range detail.Members {
		held[member.DmName] = member
	}
	var names []string
	for _, lp := range gp.legs {
		if member, ok := held[lp.name]; ok && !member.failedOutOfSlot() {
			continue
		}
		if legRows[lp.legId].GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			!s.legServes(ctx, plan, lp) {
			continue
		}
		names = append(names, lp.name)
	}
	return names
}

// legServes is the part of a leg's availability (CN12) that a call can read
// without building anything, which putBackNames tests: its subsystem, looked
// up in the call's walk, has every controller's address known, a multipath
// namespace and a path that is both live and optimized. The wrapper is the
// leg row's (legInfo), which putBackNames reads beside it. Not read: the
// converge's connect of a provisioned side that has no controller, which
// ensureLeg makes and which can fail (cnagent.md, Known limits). A lookup
// that did not answer serves nothing: the next read asks again.
func (s *CnAgentServer) legServes(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
) bool {
	view, err := s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
	return err == nil && view.unknownCtrl() == nil && view.nsDev != "" &&
		view.available()
}

// probeGroup is the read-only CN28 view of a group: for RedundMdRaid1 a
// running array (degraded included) is OK with its mdStateLine in details,
// read from sysfs alone (Md.Detail) — a dead member of an array that keeps
// another in-sync member is the leg row's to report, never by itself a
// reason for this row to block or fail, and one md has failed on a leg that
// serves again is the verdict's to name (putBackNames); an error md charges
// to the last in-sync member marks the array broken instead of failing the
// member, and an array_state of "broken" is not running and reads ERROR
// (CN29); for RedundNone the dm table decides. A running array holding a
// member that left leg_list, which the converge would not fail, reads ERROR
// with the converge's own text (inSyncGuard), so a read and a converge
// report the same row. The array it read OK is returned too, for the
// verdict, and nil whenever the row is not that: the verdict names no member
// of a group the guard holds, whose converge would refuse again, so no
// syncup comes for it on every round.
//
// Like ensureGroup it is only ever reached for a non-deferred group ([D15]), and
// like ensureGroup it states the leg-count invariant rather than trusting the
// caller with an unguarded gp.legs[0].
func (s *CnAgentServer) probeGroup(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
) (pb.ResStatus, string, *MdDetail) {
	if !gp.raid1 {
		if len(gp.legs) != 1 {
			return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
				"RedundNone group has %d legs, want 1", len(gp.legs)), nil
		}
		status, details := s.probeDmTarget(ctx, plan.rd, gp.dmName, "linear",
			gp.dataSectors, gp.legs[0].name, gp.dataOffsetSectors)
		return status, details, nil
	}
	walk, err := plan.mdWalks.get(ctx)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), nil
	}
	detail, err := s.md.Detail(ctx, walk, gp.legNames())
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), nil
	}
	if !detail.Exists || detail.State == "clear" {
		return pb.ResStatus_RES_STATUS_MISSING, "", nil
	}
	if detail.Foreign {
		return pb.ResStatus_RES_STATUS_ERROR, detail.foreignErr().Error(), nil
	}
	if !mdRunning(detail.State) {
		return pb.ResStatus_RES_STATUS_ERROR, detail.State, nil
	}
	if leaver := inSyncGuard(gp, detail, func(lp *legPlan) bool {
		return s.legServes(ctx, plan, lp)
	}); leaver != "" {
		return pb.ResStatus_RES_STATUS_ERROR,
			errInSyncGuard(leaver, detail.Dev).Error(), nil
	}
	return pb.ResStatus_RES_STATUS_OK, mdStateLine(detail), detail
}
