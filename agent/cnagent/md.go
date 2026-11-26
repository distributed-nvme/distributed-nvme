package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Md wraps the mdadm patterns of cnagent.md CN12. Only the cn role runs mdadm,
// so by the split rule of dnagent.md, Scope and placement, this wrapper is
// role code.
//
// The agent runs `mdadm --zero-superblock` only in CN12's forced start, which
// an operator's call asks for: it zeroes every other member the forced start
// keeps (forcedStartLegs) but the open repair's bad leg, each one that carries
// a superblock, before the array starts from the named leg alone, so that
// each of them is added back later
// as a fresh member, which md copies whole, and is no candidate of a later
// assembly, where its superblock of the array's old identity would have
// mdadm refuse the assembly. Otherwise a leg leaves an array only as a
// repair's bad leg,
// which the finish keeps as a retired leg and an abort leaves in leg_list;
// as a repair's new leg taken from a retired leg, which a cancel gives back
// as that retired leg; or out of existence together with its side. The
// superblock md left on it can make a later re-add cheap, and keeps an
// assembly from taking it as in sync (CN12).
type Md struct {
	cmd *agent.Cmd
	// oc reads a member's superblock area itself (ReadMember), the one read
	// of Md that is no command and no sysfs attribute.
	oc common.OsClient
}

func NewMd(oc common.OsClient) *Md {
	return &Md{cmd: agent.NewCmd(oc), oc: oc}
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
// member opens first, a dead one included. Of Md's reads only the
// superblock read (ReadMember) reads a member — the assembly's, the forced
// start's, the take-back check's and the identity check's — and only that of
// a leg available this pass, or, in a read, of a leg whose path the read
// finds serving.
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
// this list does not know — is not running. array_state reads broken only
// while the array is otherwise clean, so one read of broken proves the array
// broken, while one of write-pending, active or active-idle does not prove
// it whole: md writes the superblock of a broken array that keeps retrying a
// copy onto a half-copied member to the dead member too, and the state reads
// write-pending during each such write, so a pass can read a broken array
// running. A pass that reads it running while the group's forced-start mark
// stands runs no forced start, and a read names the forced start once it
// reads no array of the group running, or only the stop of the array that
// the start begins with while the start's checks do not hold, as while the
// named leg is dead (forcedStartNamed; cnagent.md CN12).
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

// Holders are the kernel names of the devices that hold the array dev open,
// from its sysfs holders directory: the dm devices built over it, such as a
// slice's pool devices or a clone's dm-clone; none when nothing does. mdadm
// stops an array only while nothing holds it (cnagent.md CN12, The
// re-assembly). A listing that did not answer is an error, and so is an
// array whose directory is gone, which a caller that read it there a moment
// ago cannot tell from a listing that lies.
func (m *Md) Holders(ctx context.Context, dev string) ([]string, error) {
	name := dev
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return nil, fmt.Errorf("md device %q has no node name", dev)
	}
	holders, ok, err := m.cmd.ListDir(
		ctx, sysfsBlockDir+"/"+name+"/holders")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("array %s has no holders directory", dev)
	}
	return holders, nil
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
	// moved out of its slot once no IO to it was pending, or one md holds
	// with no place to copy into.
	Slot string
	// BadBlocks is how many ranges md's bad-block list records for the
	// member, in dev-*/bad_blocks, which lists every range, acknowledged or
	// not yet written to the superblock (dev-*/unacknowledged_bad_blocks
	// lists the second kind again, so it is not added): md records one
	// where it could neither read a sector nor write it again from another
	// member, as its check does (CN12, The md check). Read for a running
	// array only, and zero for a member that publishes no list.
	BadBlocks int
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

// holdsPlace is a member that holds one of the array's places, copying or in
// sync, which a repair's started verdict asks of its new leg (CN12): a slot
// number, and not "faulty", since a member md has failed holds its slot only
// while IO to it is pending.
func (m MdMember) holdsPlace() bool {
	return m.Slot != mdSlotNone && !mdFlagSet(m.State, "faulty")
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
	// LastSyncAction is md/last_sync_action verbatim, the action md last
	// ran or stopped ("check" after a check, "none" before any); SyncMin is
	// md/sync_min verbatim, where md's next requested check starts: md sets
	// it back to 0 when a check runs to its end and leaves it where a check
	// it stopped had come to. Both read for a running array only, "" where md
	// publishes none.
	LastSyncAction string
	SyncMin        string
	Members        []MdMember
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
// Walk hands Refresh an empty walk, and startedArray's call after the
// assembly of a group with no mark, which ensureGroup makes, comes only
// after Detail read absent, which it reads only when no node is unanswered.
// The calls of CN12's forced start can start from a walk that holds an
// unanswered node: forcedStartPass's after its stop of an array that does
// not run, since the Detail before it matched that array and a match never
// asks about the other nodes; assemblyLeft's after a failed forced
// assembly, the second of which starts from a walk the first may have
// marked; and startedArray's after a forced start, which forcedStartPass
// makes and which can follow those. Dropping an unanswered node that
// went — its /sys/block entry, or only its md/, which a stopping array loses
// first — and clearing one that answers are this function's contract, which
// covers each of them, pinned by calling it directly
// (TestMdWalkUnansweredArray, TestMdWalkRefreshUnanswered).
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

// RefreshArray walks again the one array node that dev, an MdDetail's Dev,
// names, after the pass's own member changes to it: Refresh walks again only
// a node whose recorded member directories went or changed, and an --add
// makes a member directory no record holds, which its check cannot see. A
// walk of the node that answers replaces its record, or drops it when its
// md/ is gone, as Refresh does. One that does not answer is its error and
// leaves the walk exactly as it was, unlike walkNode: the pass's other
// groups and its pool concats share the walk (mdWalkOnce), and the node
// recorded as unanswered would turn every concat row of the group's slice
// ERROR, and fail every later group of the pass whose array is absent,
// while the reconciliation itself went through.
func (m *Md) RefreshArray(ctx context.Context, w *MdWalk, dev string) error {
	node := strings.TrimPrefix(dev, "/dev/")
	members, ok, err := m.memberDirs(ctx, mdDirOf(node))
	if err != nil {
		return err
	}
	delete(w.unanswered, node)
	if !ok {
		delete(w.arrays, node)
		return nil
	}
	w.arrays[node] = members
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
// dmNames, the wrapper names of a group's members (Match), and reads its
// state from sysfs. It never runs mdadm and never resolves /dev/md/<name>.
//
// Detail runs no mdadm because `mdadm --detail` opens a member device: it loads
// the superblock from the first member that opens, and when that member's DN
// side has gone the read, sent without failfast, sits in the multipath head's
// requeue list until the path's failfast expires — the teardown, 6 to 10 s
// after the side died in the lab (the wait for the next keep-alive, the
// keep-alive timeout and the distribution kernel's quarter-KATO recovery
// delay), then the pool's leg fast IO fail timeout, so 11 to 15 s at the
// default and 7 to 11 s at the shortest, since the teardown fails only IO
// sent failfast and moves this read to the head (architecture.md, Known
// limits) — or without limit while another path of the leg stays live and
// none can serve — far past the soft timeout. A kill would turn the group's
// md row ERROR for a member fault the leg row already reports, and an md row
// counts toward cntlr health. No enumerator in the agent runs mdadm for the same reason: a killed
// read taken as absent would leave a teardown no array to stop (ListArrays,
// cnagent.md CN21).
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
// array whose members include any of dmNames, the wrapper names of the
// group's members (grpPlan.memberNames), so an aborted repair's new leg does
// not count. "" with no
// error when none matches and every array answered; an error for two
// matches, or for no match beside an array that did not answer, which may be
// the group's own. A degraded array matches: one member the group names is
// enough.
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
	node, err := walk.Match(gp.memberNames())
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
			return nil, fmt.Errorf("%s md/degraded %q: %w", dev, degraded,
				err)
		}
		if detail.SyncAction, err = m.presentAttr(
			ctx, mdDir+"/sync_action"); err != nil {
			return nil, err
		}
		if detail.SyncCompleted, err = m.presentAttr(
			ctx, mdDir+"/sync_completed"); err != nil {
			return nil, err
		}
		if detail.LastSyncAction, _, err = m.cmd.ReadAttr(
			ctx, mdDir+"/last_sync_action"); err != nil {
			return nil, err
		}
		if detail.SyncMin, _, err = m.cmd.ReadAttr(
			ctx, mdDir+"/sync_min"); err != nil {
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
		badBlocks := 0
		if mdRunning(state) {
			ranges, _, err := m.cmd.ReadAttr(ctx, member.dir+"/bad_blocks")
			if err != nil {
				return nil, err
			}
			badBlocks = mdRanges(ranges)
		}
		detail.Members = append(detail.Members, MdMember{
			DevNo:     devNo,
			DmName:    member.dmName,
			State:     memberState,
			Slot:      slot,
			BadBlocks: badBlocks,
		})
		if member.dmName == "" {
			detail.Foreign = true
		}
	}
	return detail, nil
}

// mdRanges counts the ranges of one bad-block list as md prints it: one
// "<sector> <length>" line per range.
func mdRanges(list string) int {
	n := 0
	for _, line := range strings.Split(list, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
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

// ArrayUuid reads a running array's identity from its md/uuid, in mdadm's
// spelling (mdSysfsUuid): a sysfs read that touches no member, which CN12's
// identity check on a leg_list member's add compares the member's reading
// with (legListAddRefusal). A value that does not parse is an error the
// pass leaves the array alone on.
func (m *Md) ArrayUuid(ctx context.Context, dev string) (string, error) {
	path := mdDirOf(strings.TrimPrefix(dev, "/dev/")) + "/uuid"
	value, err := m.presentAttr(ctx, path)
	if err != nil {
		return "", err
	}
	uuid := mdSysfsUuid(value)
	if uuid == "" {
		return "", fmt.Errorf("%s md/uuid %q does not parse", dev, value)
	}
	return uuid, nil
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

// strangerErr is nil when every member of the array is a leg wrapper of this
// cn naming spId — the test by which the sweep calls an array ours
// (arrayOwner, cnagent.md CN21) — and otherwise names the first member that
// is not: a member that is not a dm device (foreignErr), or a dm device of
// somebody else, such as a co-hosted dn agent's export device in an array
// udev assembled. CN12's forced start stops a group's array only when this
// is nil (forcedStartPass, assemblyLeft).
func (d *MdDetail) strangerErr(clusterId, cnId, spId uint64) error {
	if d.Foreign {
		return d.foreignErr()
	}
	for _, member := range d.Members {
		if legSp, ok := legWrapperSp(
			clusterId, cnId, member.DmName); !ok || legSp != spId {
			return fmt.Errorf("array %s holds %s, which is no leg of "+
				"this sp, so it is not stopped", d.Dev, member.DmName)
		}
	}
	return nil
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

// mdMetadata names the superblock format of every dnv array, version 1.2,
// which sits 4 KiB into a member (mdSbOffset), inside the meta region no host
// writes (cnagent.md CN12, The superblock format).
const mdMetadata = "--metadata=1.2"

// mdFormatArgs is the argument list of the mdadm calls that name the
// superblock format, the create, the two assemblies and the zero: lead,
// then mdMetadata, then rest. lead is the mode word and, for a create or an
// assembly, the md device, so the members stay at the line's end. An add, a
// fail, a remove and a stop take no format: mdadm refuses the flag in manage
// mode, where an add writes the array's own format onto its member, and a
// stop reads no member. Reason: without the flag mdadm reads,
// among every superblock format it finds on a member, the one created last,
// and a host's 0.90 or 1.0 superblock at a leg's end, newer than dnv's, then
// makes a plain assembly refuse the member for its name.
func mdFormatArgs(lead []string, rest ...string) []string {
	args := make([]string, 0, len(lead)+1+len(rest))
	args = append(args, lead...)
	args = append(args, mdMetadata)
	return append(args, rest...)
}

// ZeroSuperblock wipes a member's md superblock, which only CN12's forced
// start asks for (Md). mdadm refuses a member an array holds, a broken array
// included. Named for the 1.2 format (mdFormatArgs), the zero removes only
// that superblock, and fails on a member that carries none and on one whose
// read fails. So the forced start zeroes only a member whose reading shows a
// superblock, counts one it reads blank as zeroed, and reads each member back
// after its zero (ReadMember), taking the zero as done only when the read
// answers with no superblock (forceStartGroup).
func (m *Md) ZeroSuperblock(ctx context.Context, dev string) error {
	return m.cmd.RunOk(ctx, "mdadm",
		mdFormatArgs([]string{"--zero-superblock"}, dev)...)
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
	args := mdFormatArgs([]string{"--create", conf.DevPath},
		"--name", conf.ArrayName,
		"--level", "1",
		"--raid-devices", strconv.Itoa(conf.RaidDevices),
		"--bitmap", "internal",
		"--bitmap-chunk",
		strconv.FormatUint(conf.BitmapChunkKiB, 10)+"K",
		"--data-offset",
		strconv.FormatUint(conf.DataOffsetKiB, 10)+"K",
		"--homehost", "any",
		"--run",
	)
	if conf.AssumeClean {
		args = append(args, "--assume-clean")
	}
	// --failfast applies to the devices listed after it.
	args = append(args, "--failfast")
	args = append(args, members...)
	for i := len(members); i < conf.RaidDevices; i++ {
		args = append(args, "missing")
	}
	// A create writes each member's superblock in turn, and a stop between
	// two leaves one leg with a record that counts both: it runs under the
	// longer bound of an array build (CN12).
	return m.cmd.RunOkFor(ctx, common.MdBuildTimeout*time.Second, "mdadm",
		args...)
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
	args := mdFormatArgs([]string{"--assemble", devPath},
		append([]string{"--name", arrayName}, members...)...)
	// The longer bound of an array build (CN12): an assembly stopped once
	// md holds a member leaves the array inactive.
	return m.cmd.RunOkFor(ctx, common.MdBuildTimeout*time.Second, "mdadm",
		args...)
}

// The superblock updates of CN12's forced assembly (AssembleForced), one per
// run, since mdadm takes one --update a run.
const (
	// mdUpdateUuid gives the array a new identity, so that a member of its
	// old history added later is copied whole and never re-added from the
	// bitmap.
	mdUpdateUuid = "uuid"
	// mdUpdateForceNoBbl drops the named leg's bad-block log, which a stop
	// of the array the leg came back to can leave with empty entries that
	// the kernel refuses to import. It runs only over the new identity a
	// refused first run left on the leg (forcedAssemble).
	mdUpdateForceNoBbl = "force-no-bbl"
)

// AssembleForced starts an array from one member alone for CN12's forced
// start, which an operator's call asks for: `--run` starts it degraded, and
// `--force` starts it even from a superblock mdadm reads as out of date.
// update is the one superblock update of the run (mdUpdateUuid,
// mdUpdateForceNoBbl). Assemble stays the form of every other start, and
// passes neither flag.
func (m *Md) AssembleForced(
	ctx context.Context,
	devPath string,
	arrayName string,
	member string,
	update string,
) error {
	// The longer bound of an array build (CN12, SH15): the run writes its
	// superblock update before md starts the array.
	return m.cmd.RunOkFor(ctx, common.MdBuildTimeout*time.Second, "mdadm",
		mdFormatArgs([]string{"--assemble", devPath}, "--name", arrayName,
			"--run", "--force", "--update="+update, member)...)
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
// leg wrappers. A repair's new legs, connected, wrapped and probed at every
// stage, join only by CN12's rules for a repair.
//
// A provisioning-deferred group never gets here: the build phase reports it
// PROVISIONING and skips it ([D15]). The leg-count check of ensureLinearGroup
// is kept explicit all the same, so gp.legs[0] can never be indexed on an
// empty member list whatever a future caller does.
//
// legRows are the pass's leg rows, which the member reconciliation's put-back
// reads (reconcileMembers). For an md group whose array runs at the end of
// the pass, the array is returned too, as read after the pass's own member
// changes, for what the primary reports beside the group's row
// (reportGrpReading) and for the row's own state line (mdStateLine): a pass that changed a member reads the array's node
// again (Md.RefreshArray), since Md.Refresh does not see an added member. A
// read again whose listing of the node fails returns no array and no error
// and leaves the pass's walk as it was; one whose listing answers updates the
// walk, and when it then cannot read the array, or finds it not running or
// gone, it too returns no array and no error. Each way the group's row is the
// reconciliation's, the pass reports no verdict and no reading for the
// group, and the next read of the cntlr reports what it finds. When the
// member reconciliation fails, the array as the pass last read it (arrayAfter)
// comes back beside its error, for the md check alone: the build steps the
// check over it all the same (CN12, The md check), so a read's name of the
// check is one a converge cures; not while the in-sync guard holds
// (inSyncGuardError), whose row a read reads as the converge does.
//
// running reports whether the pass found the md group's array running, as
// it was or after its assembly, so that the member reconciliation ran over
// it, whether or not that reconciliation went through; the build forgets
// the group's md check when it reads false (forgetMdCheck). A RedundNone
// group has no array and reports false. refusals are the reconciliation's
// (grpRefusals): the take-back check's refusal of one that went through
// (takeBackStep), which the repair's verdict carries, and the identity
// check's marks, which a reconciliation that failed after its check returns
// too, as does one whose array, read again after its member changes, gives
// no detail (arrayAfter).
//
// While the group's forced-start mark stands and no array of the group runs,
// the forced start (forcedStartPass) stands in for the assembly. A running
// array is reconciled as without a mark, which leaves the mark waiting for
// the sp worker's clear (dnv-worker.md AR13); a broken array can read
// running (mdRunningStates), and a read names the forced start once it reads
// no array of the group running, or the stop alone while the forced start's
// checks do not hold (forcedStartNamed), so the worker's re-send brings a
// converge that acts on the mark if it too reads the array not running;
// each read and each pass reads the array afresh and carries no earlier
// reading into a later read or pass. One that holds a member that is not a
// dm device is the group's error (runningArrayErr), as without a mark: the
// same array read broken ends the forced start in that error
// (MdDetail.strangerErr), and a read names nothing for it.
func (s *CnAgentServer) ensureGroup(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	available map[uint64]bool,
	legRows map[uint64]*pb.ResInfo,
	dead map[uint64]bool,
) (
	detail *MdDetail,
	running bool,
	refusals grpRefusals,
	err error,
) {
	if !gp.raid1 {
		return nil, false, grpRefusals{},
			s.ensureLinearGroup(ctx, gp, available)
	}
	// A walk or an array read that did not answer leaves the group unbuilt
	// for the pass, as a failed create does.
	walk, err := plan.mdWalks.get(ctx)
	if err != nil {
		return nil, false, grpRefusals{}, err
	}
	detail, err = s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil {
		return nil, false, grpRefusals{}, err
	}
	forced := gp.mark != nil && !(detail.Exists && mdRunning(detail.State))
	switch {
	case forced:
		detail, err = s.forcedStartPass(ctx, plan, gp, walk, detail,
			available)
	case reassembles(gp, detail):
		detail, err = s.reassemblePass(ctx, plan, gp, walk, detail,
			available, legRows)
	case !detail.Exists:
		err = s.assembleGroup(ctx, gp, available)
		if err == nil {
			detail, err = s.startedArray(ctx, gp, walk)
			break
		}
		// CN12: an assembly that failed can leave the array inactive,
		// holding a member, as one the bound stopped once md took it; the
		// pass reads the array again and stops one that does not run when
		// every member is this pool's, so the next assembly finds the legs
		// free, while the operator is not in charge of the group. One that
		// runs after all is the group's array. The stop runs even when the
		// syncup's deadline cancelled the pass.
		if gp.grp.GetOperatorInCharge() {
			break
		}
		if started, leftErr := s.assemblyLeft(context.WithoutCancel(ctx),
			plan, gp, walk); leftErr != nil {
			err = fmt.Errorf("%w; %w", err, leftErr)
		} else if started {
			detail, err = s.startedArray(ctx, gp, walk)
		}
	}
	if err == nil {
		err = runningArrayErr(detail)
	}
	if err != nil {
		return nil, false, grpRefusals{}, err
	}
	changed, refusals, err := s.reconcileMembers(
		ctx, gp, detail, available, legRows, dead)
	if err != nil {
		// The array runs, and the build steps its md check over it all
		// the same (stepMdCheck), as the pass last read it, but while the
		// in-sync guard holds, whose row a read reads alike.
		var guard inSyncGuardError
		if errors.As(err, &guard) {
			return nil, true, grpRefusals{}, err
		}
		return s.arrayAfter(ctx, walk, gp, detail, changed), true,
			refusals, err
	}
	// An array read again that fails gives no reading (reportGrpReading),
	// and the identity check's marks stand all the same.
	detail = s.arrayAfter(ctx, walk, gp, detail, changed)
	return detail, true, refusals, nil
}

// arrayAfter is the group's running array as the pass's member
// reconciliation left it: detail, the array read before it, when it changed
// nothing, and otherwise the array read again (Md.RefreshArray, Md.Detail),
// since Md.Refresh does not see an added member; nil when that read fails,
// finds the array gone or not running, or finds a member that is not a dm
// device.
func (s *CnAgentServer) arrayAfter(
	ctx context.Context,
	walk *MdWalk,
	gp *grpPlan,
	detail *MdDetail,
	changed bool,
) *MdDetail {
	if !changed {
		return detail
	}
	if err := s.md.RefreshArray(ctx, walk, detail.Dev); err != nil {
		return nil
	}
	after, err := s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil || !after.Exists || runningArrayErr(after) != nil {
		return nil
	}
	return after
}

// startedArray reads the group's array again after this pass's assembly or
// forced start returned with no error: the walk refreshed, then the array
// found by the members the group keeps. An array still absent is the
// group's error.
func (s *CnAgentServer) startedArray(
	ctx context.Context,
	gp *grpPlan,
	walk *MdWalk,
) (*MdDetail, error) {
	if err := s.md.Refresh(ctx, walk); err != nil {
		return nil, err
	}
	detail, err := s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil {
		return nil, err
	}
	if !detail.Exists {
		return nil, fmt.Errorf("array %s did not start", gp.devPath)
	}
	return detail, nil
}

// runningArrayErr is the group's error for an array the member
// reconciliation may not touch: one holding a foreign member, and one that
// is not running — inactive, broken — which is left exactly as it is, since
// member reconciliation is for a running mirror, and an --add, --fail or
// --remove on an array that has not started could only change which members
// an operator's start would find. Its state is the group's error. Only the
// stop of CN12's forced start, while the group's mark stands, the stop of
// CN12's re-assembly of a broken or inactive array (reassemblePass) and the
// stop of what a failed assembly left (assemblyLeft) act on such an array,
// all before this test (ensureGroup).
func runningArrayErr(detail *MdDetail) error {
	if detail.Foreign {
		return detail.foreignErr()
	}
	if !mdRunning(detail.State) {
		return fmt.Errorf("array %s is %s", detail.Dev, detail.State)
	}
	return nil
}

// ensureLinearGroup converges a RedundNone group's dm-linear over its leg
// wrapper (cnagent.md CN12), and creates or reloads it only over an
// available leg. The layers above do IO through the group, and a side that
// has not flipped to this CN yet exports dm-error to it: a pool, a raid0 and
// an ns-dev built over such a leg would let a promotion that came before the
// sides' switch-over ([D16]) move its namespaces optimized over IO errors.
// A device that is there with the table it wants and is not read-only is
// kept whatever the leg's availability, resumed if it is suspended, as
// ensureDmSingle does: a primary that serves keeps serving through a short
// loss of its leg's path, with no teardown and no reload. A late leg under
// a device that is missing, maps something else or is read-only fails the
// group with "no available leg" and touches nothing; over a leg that serves,
// a read-only one is reloaded and made writable (dmStepReloadWritable). A missing device leaves the slice's concat without the
// group's number: a pool not built yet, as at a promotion's first converge,
// waits, and every layer above it with it, the namespaces inaccessible
// (CN16); a pool that serves when a grow adds the group keeps serving at its
// old size, with the raid0s and ns-devs over it, while its concat and pool
// rows read ERROR. Once the leg serves, a read of the cntlr names the
// device (linearGroupStep, CN30), and the SyncupCntlr the worker re-sends on
// that name builds the group and builds or grows the pool.
//
// The step is linearGroupStep's, over the device as the pass reads it
// (linearGroupShape). A create, reload or resume that fails is named by the
// converge's own verdict (grpPlan.linearName); a read that did not answer
// is left to the next read.
func (s *CnAgentServer) ensureLinearGroup(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
) error {
	if len(gp.legs) != 1 {
		return fmt.Errorf(
			"RedundNone group has %d legs, want 1", len(gp.legs))
	}
	if gp.dataSectors == 0 {
		return fmt.Errorf("device size is 0")
	}
	lp := gp.legs[0]
	shape, legNo, err := s.linearGroupShape(ctx, nil, gp, true)
	if err != nil {
		return err
	}
	step, err := linearGroupStep(shape, func() bool {
		return available[lp.legId]
	})
	if err != nil {
		return err
	}
	if step == dmStepCreate {
		// An absent device is read with no number of the wrapper.
		if legNo, err = s.dmDevNo(ctx, nil, lp.name); err != nil {
			return err
		}
	}
	err = s.takeDmStep(ctx, gp.dmName, step, dmTable("linear",
		gp.dataSectors, linearGroupArgs(gp, legNo)))
	if err != nil {
		gp.linearName = rowFailure(errRowDiffers, err.Error())
	}
	return err
}

// linearGroupArgs is the table arguments of a RedundNone group's dm-linear
// over its leg wrapper, whose number is legNo.
func linearGroupArgs(gp *grpPlan, legNo string) []string {
	return linearArgs(legNo, gp.dataOffsetSectors)
}

// linearGroupShape reads a RedundNone group's dm-linear against the linear
// over its leg's wrapper at the group's data offset, from rd, the reading of
// a cntlr's read, or with no reading the device's own reads: the device's
// info, then, for a device that is there, the wrapper's number and the
// device's table. It returns the shape (dmShapeOf), the wrapper's number,
// "" for an absent device, and the error of a read that did not answer, a
// wrapper that is not there among them. readOnlyDiffers is dmShapeOf's.
func (s *CnAgentServer) linearGroupShape(
	ctx context.Context,
	rd *cnReading,
	gp *grpPlan,
	readOnlyDiffers bool,
) (dmShape, string, error) {
	dev, err := s.dmInfo(ctx, rd, gp.dmName)
	if err != nil {
		return dmShapeWanted, "", err
	}
	if dev == nil {
		return dmShapeAbsent, "", nil
	}
	legNo, err := s.dmDevNo(ctx, rd, gp.legs[0].name)
	if err != nil {
		return dmShapeWanted, "", err
	}
	targets, err := s.dmTable(ctx, rd, gp.dmName)
	if err != nil {
		return dmShapeWanted, "", err
	}
	return shapeOf(dev, dmTableMatches(targets, "linear", gp.dataSectors,
		linearGroupArgs(gp, legNo), 0), readOnlyDiffers), legNo, nil
}

// linearGroupStep is CN12's step on a RedundNone group's dm-linear of shape,
// which ensureLinearGroup takes and a cntlr's read names the device by
// (CN30): dmSingleStep's, save that a device absent, with another table or
// read-only is created or reloaded only while its leg serves — serves, the
// converge's availability, a read's readAvailable — and is refused with
// "no available leg" otherwise. A device with the table it wants, not
// read-only, is resumed whatever the leg, so a primary keeps serving through
// a short loss of its leg's path.
func linearGroupStep(shape dmShape, serves func() bool) (dmStep, error) {
	step := dmSingleStep(shape)
	if step != dmStepNone && step != dmStepResume && !serves() {
		return dmStepNone, errNoAvailableLeg()
	}
	return step, nil
}

// linearGroupName is what a cntlr's read names a RedundNone group's dm-linear
// with (CN30), "absent" or "differs", while linearGroupStep, over the device
// as the read's reading holds it, read-only flag included as the converge
// reads it, would change it, and nil otherwise. A read that did not answer
// names nothing, and neither does a device over a wrapper that is not there,
// which the wrapper's own name covers (wrapperName), nor a group of no size,
// which the converge refuses.
func (s *CnAgentServer) linearGroupName(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
) error {
	if len(gp.legs) != 1 || gp.dataSectors == 0 {
		return nil
	}
	shape, _, err := s.linearGroupShape(ctx, plan.rd, gp, true)
	if err != nil {
		return nil
	}
	step, err := linearGroupStep(shape, func() bool {
		return s.readAvailable(ctx, plan, gp.legs[0])
	})
	if err != nil || step == dmStepNone {
		return nil
	}
	return shapeFailure(shape, "linear")
}

// readAvailable is a read's twin of the converge's availability of a leg
// (CN12), which needs ensureLeg to get through, and by which a cntlr's read
// decides a group's names — a RedundNone group's dm-linear
// (linearGroupName), an absent array (absentArrayNamed) and a forced start
// (forcedStartNamed): the leg is not provisioning, no sweep disconnects its
// subsystem, its lookup answers, the lookup holds a path both live and
// optimized, and ensureLeg's steps up to the wrapper's would take none
// (wrapperStepOf: the lookup's test, the head's number, the wrapper the
// whole-size dm-linear over that number, there, resumed and not read-only),
// which with the path test is legServes' reading of the leg. A leg whose
// lookup in the read's leg loop (legPlan.readView) holds no such path is
// out at once; every other leg is looked up afresh, since its path can have
// stopped serving since the loop's lookup, and the superblock read the
// group's name makes next through such a path can wait. The path is tested
// before the head's number is read, so a leg that does not serve costs no
// number read. A wrapper a converge would change is the wrapper's own name
// (wrapperName), and the group waits for it, as a row waits for the rows it
// is built on (cntlrRowVerdict); so no superblock is read through a
// wrapper that is missing, suspended, read-only or over another number than
// the head the lookup finds, a read that could wait for a resume or read
// another device.
func (s *CnAgentServer) readAvailable(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
) bool {
	if lp.provisioning || s.disconnectInFlight(lp.nqn) {
		return false
	}
	// A leg whose lookup in the leg loop found no path both live and
	// optimized does not serve, and costs nothing more. One that did, or that
	// the loop did not look up, is looked up afresh: its path can have
	// stopped serving since, and the superblock read the group's name makes
	// next through such a path can wait as long as the path holds its IO.
	if lp.readView != nil && !lp.readView.available() {
		return false
	}
	view, err := s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
	if err != nil || !view.available() {
		return false
	}
	// The path before the head's number, so a leg whose path does not
	// serve costs no number read.
	_, step, ok := s.wrapperStepOf(ctx, plan.rd, lp, view)
	return ok && step == dmStepNone
}

// errNoAvailableLeg is the error of a group this pass leaves unbuilt because
// none of its legs is available (cnagent.md CN12), one text for both kinds of
// group.
func errNoAvailableLeg() error {
	return fmt.Errorf("no available leg")
}

// assembleGroup is CN12's assembly: its case selection (planAssembly), then
// the one mdadm command that selection decides, the create of case 1 or the
// assembly of case 2, which hands mdadm the members of one identity with no
// --run, so that mdadm itself decides whether a degraded start is safe
// (Md.Assemble).
func (s *CnAgentServer) assembleGroup(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
) error {
	ap, err := s.planAssembly(ctx, gp, available)
	if err != nil {
		return err
	}
	if ap.create {
		return s.md.Create(ctx, ap.conf, ap.members)
	}
	return s.md.Assemble(ctx, gp.devPath, gp.mdArrayName, ap.members)
}

// assemblyPlan is what CN12's assembly would run (planAssembly): with create
// set, the create of case 1 with conf over members, every leg_list member;
// otherwise the assembly of case 2 from members, the candidates that carry
// a superblock of the identity it hands mdadm, sbs their superblocks in the
// same order.
type assemblyPlan struct {
	create  bool
	conf    MdCreateConf
	members []string
	sbs     []*mdSuperblock
}

// planAssembly is the case selection of cnagent.md CN12, with no mdadm
// command that changes the node: read each available member's superblock
// area (ReadMember) and decide either to create the array or to assemble it
// from the members that carry a superblock of one identity. The converge's
// assembly (assembleGroup) and a cntlr's read (absentArrayNamed) both run
// it, the read with its own availability (readAvailable), so the read names
// an absent array by the converge's own decision. The candidates are the
// available leg_list legs and the available new legs of an open repair: a
// new leg that carries a superblock is handed to the assembly beside the
// leg_list legs that carry one, after them, and mdadm and the kernel judge
// its membership from the superblocks, so a half-copied new leg resumes its
// copy and never starts the array alone, and a whole one can start it when
// no leg_list leg is available, if md recorded the other legs as failed. A
// new leg never counts toward case 1, whose create holds only when no
// candidate carries a superblock, and is never one of its members.
//
// A reading whose read failed or did not answer aborts the whole assembly:
// with no answer this pass cannot tell case 1 from case 2, and guessing case
// 1 creates over live data. mdadm refuses an assembly handed members of two
// identities, so case 2 hands it only the members of one (assemblyIdentity),
// and the others stay out: a leg_list member of another identity is refused
// at its add (legListAddRefusal) and a taken-back new leg at the take-back
// check (takeBackStep), both named in the primary's report. Case 1's two
// refusals and the test of the name (Md.NameInUse, an lsblk, which changes
// nothing) are part of the selection.
func (s *CnAgentServer) planAssembly(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
) (*assemblyPlan, error) {
	var members []string
	var cands []assemblyCand
	read := func(lp *legPlan, tier int) error {
		reading := s.md.ReadMember(ctx, lp.path)
		if reading.Err != nil {
			return reading.Err
		}
		cands = append(cands, assemblyCand{lp: lp, tier: tier, sb: reading.Sb})
		return nil
	}
	for _, lp := range gp.legs {
		if !available[lp.legId] {
			continue
		}
		members = append(members, lp.path)
		tier := assemblyTierLegList
		if gp.rep != nil && gp.rep.badLegIds[lp.legId] {
			tier = assemblyTierBadLeg
		}
		if err := read(lp, tier); err != nil {
			return nil, err
		}
	}
	if rp := gp.openRep(); rp != nil {
		for _, lp := range rp.newLegs {
			if !available[lp.legId] {
				continue
			}
			if err := read(lp, assemblyTierNewLeg); err != nil {
				return nil, err
			}
		}
	}
	uuid, err := assemblyIdentity(gp, cands)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 && uuid == "" {
		return nil, errNoAvailableLeg()
	}
	conf := MdCreateConf{
		DevPath:        gp.devPath,
		ArrayName:      gp.mdArrayName,
		RaidDevices:    len(gp.legs),
		BitmapChunkKiB: gp.bitmapChunkKiB,
		DataOffsetKiB:  gp.dataOffsetKiB,
	}
	if uuid == "" {
		// Case 1: --assume-clean is correct only when **every** member was
		// read. A side is never exported before the provisioning protocol
		// (architecture.md, Side provisioning protocol) has zeroed its meta
		// region, where the md superblock sits, and its first data block,
		// and its `provisioned` gate has opened ([D15]); the effective
		// desired state defers any group whose legs are still provisioning
		// ([D15]); ids are never reused; and the agent zeroes a superblock
		// only in the forced start (forceStartGroup) — so a superblock-free
		// leg is a freshly provisioned side — short of a member a forced
		// start zeroed that the member reconciliation has not added back,
		// which still holds its data: a mark that goes before the start, or
		// a zeroed member that dies before its add, leaves one, and the
		// group's flag that its array has run, which the sp worker sets from
		// the first report that reads the group's row OK, tells it from a
		// fresh side (below; architecture.md, Known limits).
		// A read that failed never reaches here: it is the reading's error
		// above, never "no superblock". Only a slice's meta group's members
		// are zeroed whole; a data group's and a clone meta group's are
		// zeroed no further than their first data block, and the rest may
		// differ between them, which a clean create does not resync:
		// nothing dnv stacks on a data group's array reads such a block
		// before writing it through the array, because dm-thin writes a
		// block whole before anything reads it ([D15]); and the dm-clone
		// over a clone meta group's array finds the zeroed first data
		// block, where its superblock goes, formats its metadata there and
		// reads only metadata blocks it has written through the array
		// (CN18). But that argument covers the legs actually read. An
		// unavailable member may be the one carrying the group's data (its
		// DN rebooting, its path mid-ANA-move), and creating over the
		// survivors would resync the data away. cnagent.md CN12 puts a
		// single available member in case 2 (assemble, and mdadm decides on
		// a degraded start), never in case 1.
		if len(members) != len(gp.legs) {
			return nil, fmt.Errorf(
				"only %d of %d legs available and none carries a superblock",
				len(members), len(gp.legs))
		}
		// Never for a group whose array has run (Group.array_made, which
		// the sp worker sets from the first report that reads the group's
		// row OK, dnv-worker.md AR15): its legs that read no superblock
		// were zeroed by a forced start that ended before its assembly, or
		// before the add of each, and hold data, and a create over them
		// would serve a mix with no error. A read's dry run of the
		// assembly meets the same refusal and names nothing for it
		// (absentArrayNamed); once a leg with a superblock serves, as a
		// repair's new leg the forced start named, the read names the
		// array case 2 would assemble, and otherwise the operator's mark
		// acts.
		if gp.grp.GetArrayMade() {
			return nil, fmt.Errorf(
				"the array of group %#x has run before and no member "+
					"carries a superblock: its legs hold data, so no new "+
					"array is created", gp.grpId)
		}
		// Nor while an array already runs under the group's name. Detail
		// finds the array by its members, so one holding NONE of the
		// wrappers the group keeps (its leg_list wrappers and its open
		// repair's new legs, grpPlan.memberNames) never reaches reconcile,
		// and lands here when every leg answers "no superblock". A create
		// would put a second array under the name mdadm gives the group's
		// array, and the guard keeps it from doing so. The pool's concat
		// finds the group's array by its legs (mdDevNo), never by that name,
		// so until the group has an array of its own the concat rows read an
		// error rather than take the running array's number. The guard errs
		// only towards refusing, and a false refusal needs a stale node of
		// this very name: short of the case above and of the members a
		// forced start zeroed, a group whose legs carry no superblock has
		// never had an array that could have left one.
		inUse, err := s.md.NameInUse(ctx, gp.devPath)
		if err != nil {
			return nil, err
		}
		if inUse {
			return nil, fmt.Errorf("an array runs under %s %w",
				gp.devPath, errArrayNameInUse)
		}
		conf.AssumeClean = true
		return &assemblyPlan{create: true, conf: conf, members: members}, nil
	}
	// Case 2: assemble from the members that carry a superblock of the
	// array's identity; anything left out — a freshly provisioned addition,
	// a stale-metadata re-add or a leg of another identity — is for the
	// member reconciliation (reconcileMembers, cnagent.md CN12).
	ap := &assemblyPlan{}
	for _, c := range cands {
		if c.sb != nil && c.sb.Uuid == uuid {
			ap.members = append(ap.members, c.lp.path)
			ap.sbs = append(ap.sbs, c.sb)
		}
	}
	return ap, nil
}

// The sets an assembly's candidates fall in, in order of trust
// (assemblyIdentity): the leg_list legs other than a bad leg of the group's
// repair, the open repair's new legs, and the repair's bad leg, at any stage
// of the repair, an ABORTED one's included.
const (
	assemblyTierLegList = iota
	assemblyTierNewLeg
	assemblyTierBadLeg
)

// assemblyCand is one candidate of an assembly with its reading: sb nil for
// a member that carries no superblock.
type assemblyCand struct {
	lp   *legPlan
	tier int
	sb   *mdSuperblock
}

// assemblyIdentity is the identity case 2 of CN12's assembly hands mdadm the
// members of: that of the first set, in order of trust, in which a candidate
// carries a superblock — the leg_list legs other than a bad leg of the
// group's repair, then the open repair's new legs, then the repair's bad
// leg, whatever the repair's stage. "" when no candidate carries one, which
// is case 1. Two identities inside that first set are the group's error,
// which the operator ends, and no read names it: a repair whose bad leg is
// the leg of the identity not wanted moves it to the last set, keeping its
// data, and a forced start names the leg to start from. Reason: mdadm refuses
// an assembly handed members of two identities, as a forced start leaves them
// when it starts the array from one leg under a new identity and the
// repair's bad leg keeps the old one, or a taken-back leg its own; a majority
// alone would tie one against one in a group of two legs. The bad leg is the
// leg the repair replaces, also once an abort let it stay, which a forced
// start from another leg left out of the array, and the new legs join only
// by the repair, so the leg_list legs that stay decide first. The error is
// an identityConflict, which names every leg of that set that carries a
// superblock: each disagrees with another, and the agent cannot tell which
// identity is current, so both channels mark each such leg's row
// (RES_REASON_IDENTITY_UNDECIDED).
func assemblyIdentity(gp *grpPlan, cands []assemblyCand) (string, error) {
	for tier := assemblyTierLegList; tier <= assemblyTierBadLeg; tier++ {
		uuid := ""
		var first, other *legPlan
		var otherUuid string
		var legs []uint64
		for _, c := range cands {
			if c.tier != tier || c.sb == nil {
				continue
			}
			legs = append(legs, c.lp.legId)
			switch {
			case uuid == "":
				uuid, first = c.sb.Uuid, c.lp
			case c.sb.Uuid != uuid && other == nil:
				other, otherUuid = c.lp, c.sb.Uuid
			}
		}
		if other != nil {
			slices.Sort(legs)
			return "", &identityConflict{legIds: legs, text: fmt.Sprintf(
				"members of two identities: leg %#x carries %s and "+
					"leg %#x %s; a forced start names the leg to start "+
					"from", first.legId, uuid, other.legId, otherUuid)}
		}
		if uuid != "" {
			return uuid, nil
		}
	}
	return "", nil
}

// identityConflict is assemblyIdentity's refusal of an assembly whose first
// set of candidates carries two identities: legIds are the legs of that set
// that carry a superblock, ascending, whose rows both channels mark
// RES_REASON_IDENTITY_UNDECIDED (markLegRows).
type identityConflict struct {
	legIds []uint64
	text   string
}

func (e *identityConflict) Error() string {
	return e.text
}

// markLegRows sets reason on the rows of legIds in info's leg map, each
// row's status and details kept (CN12, CN28): the identity check's refused
// adds of a running array (RES_REASON_OTHER_IDENTITY) and an assembly's two
// identities (RES_REASON_IDENTITY_UNDECIDED). A leg with no row is left
// out. A change of the reason alone moves no epoch (dnagent.md SH14).
func markLegRows(
	t *agent.ResTracker,
	info *pb.CntlrInfo,
	legIds []uint64,
	reason pb.ResReason,
) {
	for _, legId := range legIds {
		row := info.GetLegIdToLeg()[legId]
		if row == nil {
			continue
		}
		info.LegIdToLeg[legId] = t.SetReason(resKeyOf(resKeyLegFmt, legId),
			row.GetResName(), row.GetStatus(), row.GetDetails(), reason)
	}
}

// msgForcedStart is the Warn record of a forced start that started the
// group's array from its named leg (CN12): it names the group, the leg, the
// members whose zero the read after it proved, and the superblock update of
// the assembly that started it.
const msgForcedStart = "forced start"

// msgForcedStartRefused is the Warn record of a forced start refused for
// refusals the mark does not pass (CN12): it names the group, the leg, each
// such refusal whole, its kind's word first, and the words of the kinds the
// mark passed among those the checks met.
const msgForcedStartRefused = "forced start refused"

// forcedStartPass is ensureGroup's part while the group's forced-start mark
// stands and no array of the group runs (CN12): an array of the group that
// does not run — broken, or inactive — is stopped first (stopGroupArray),
// since it holds its members, so mdadm could neither zero them nor assemble
// the named leg, and it serves nothing; then the forced start
// (forceStartGroup) starts the absent array in place of the assembly, and
// the array is read again (startedArray). An array that does not run is
// stopped only when every member it holds is a leg wrapper of this cn naming
// the cntlr's sp, as the sweep stops only an array it calls ours (cnagent.md
// CN21); one that holds any other member is the group's error, and nothing
// stops it (MdDetail.strangerErr). An array still there after the stop is
// returned as the read found it, for ensureGroup's test of an array that
// runs: one that runs is reconciled and leaves the mark waiting, as one the
// first read found running does. An array the forced start started is
// returned as read again (startedArray).
func (s *CnAgentServer) forcedStartPass(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	walk *MdWalk,
	detail *MdDetail,
	available map[uint64]bool,
) (*MdDetail, error) {
	if detail.Exists {
		if err := detail.strangerErr(
			plan.clusterId, plan.cnId, plan.spId); err != nil {
			return nil, err
		}
		if err := s.stopGroupArray(ctx, detail.Dev); err != nil {
			return nil, err
		}
		if err := s.md.Refresh(ctx, walk); err != nil {
			return nil, err
		}
		found, err := s.md.Detail(ctx, walk, gp.memberNames())
		if err != nil {
			return nil, err
		}
		if found.Exists {
			return found, nil
		}
	}
	if err := s.forceStartGroup(ctx, plan, gp, walk, available); err != nil {
		return nil, err
	}
	return s.startedArray(ctx, gp, walk)
}

// mdStateBroken is the array_state of an array md has marked broken, which
// refuses every write: every member has failed under IO, or the last one md
// held in sync (CN29). mdStateInactive is that of an array assembled in part
// and not started, as an assembly stopped once md held a member leaves it.
const (
	mdStateBroken   = "broken"
	mdStateInactive = "inactive"
)

// reassembles reports whether ensureGroup's pass tries CN12's re-assembly of
// the group's array: it reads broken or inactive, holds no member that is not
// a dm
// device, the group's record carries no forced-start mark, and the operator
// has not taken the group over (Group.operator_in_charge). A mark acts in the
// re-assembly's place; one held from a request that does not count as
// delivered, which the pass does not act on (dropForcedStartMarks), holds it
// too, since the operator's choice of a leg may still stand.
func reassembles(gp *grpPlan, detail *MdDetail) bool {
	return detail.Exists &&
		(detail.State == mdStateBroken || detail.State == mdStateInactive) &&
		!detail.Foreign && gp.grp.GetForcedStart() == nil &&
		!gp.grp.GetOperatorInCharge()
}

// reassemblyRefusal is "" when CN12's re-assembly may stop the group's
// broken or inactive array and assemble it again, and otherwise why not, the converge's
// (reassemblePass) and the read's (probeCntlr) alike: the array holds a
// member that is no leg wrapper of this pool, which the sweep's rule leaves
// alone (MdDetail.strangerErr); a device holds the array open (Md.Holders),
// so mdadm cannot stop it; or the reading of the members is not clear.
// Clear is: every member the group keeps — its leg_list legs and the new
// legs of its open repair — is on a leg that serves and whose row does not
// read ERROR (usable: copySourceLeg's test, the converge's or the read's),
// and its superblock reads; every superblock read is a full member's
// (mdSuperblock.notFullMember) with no bad-block list, all of one identity;
// no new leg the repair took back from a retired leg carries one, which
// only the take-back check may let in (takeBackStep); and at least one
// member carries a superblock. A member that carries none is left out of
// the assembly and added after it, as at any assembly (assembleGroup).
// Reason: mdadm's plain assemble then starts the array from the newest
// members and leaves an older one out (cnagent_integtest.md, Lab facts),
// while a half-copied member, a bad-block list or two identities are the
// cases where only the operator knows which leg holds the data, and an
// unread member may hold the newest copy; the leg row keeps a disk that
// returns errors out, since its array would break again at the next write
// and a stop and an assembly would follow on every round. An error is a
// read that did not answer: the holders' listing.
func (s *CnAgentServer) reassemblyRefusal(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	detail *MdDetail,
	usable func(*legPlan) bool,
) (string, error) {
	if err := detail.strangerErr(
		plan.clusterId, plan.cnId, plan.spId); err != nil {
		return err.Error(), nil
	}
	holders, err := s.md.Holders(ctx, detail.Dev)
	if err != nil {
		return "", err
	}
	if len(holders) > 0 {
		return fmt.Sprintf("held open by %s, so it cannot be stopped",
			strings.Join(holders, ", ")), nil
	}
	uuid := ""
	var first *legPlan
	for _, lp := range inspectedLegs(gp) {
		if lp.provisioning || !usable(lp) {
			return fmt.Sprintf("leg %#x does not serve, or its row reads "+
				"ERROR", lp.legId), nil
		}
		reading := s.md.ReadMember(ctx, lp.path)
		if reading.Err != nil {
			return fmt.Sprintf("the superblock read of leg %#x: %v",
				lp.legId, reading.Err), nil
		}
		sb := reading.Sb
		if sb == nil {
			continue
		}
		if gp.rep != nil && gp.rep.takenBack(lp.legId) &&
			!slices.Contains(gp.legs, lp) {
			return fmt.Sprintf("leg %#x, taken back from a retired leg, "+
				"carries a superblock", lp.legId), nil
		}
		if why := sb.notFullMember(); why != "" {
			return fmt.Sprintf("leg %#x is not a full member: %s",
				lp.legId, why), nil
		}
		if sb.BadBlocks > 0 {
			return fmt.Sprintf("leg %#x carries a bad-block list of %d "+
				"entries", lp.legId, sb.BadBlocks), nil
		}
		switch {
		case uuid == "":
			uuid, first = sb.Uuid, lp
		case sb.Uuid != uuid:
			return fmt.Sprintf("members of two identities: leg %#x "+
				"carries %s and leg %#x %s", first.legId, uuid, lp.legId,
				sb.Uuid), nil
		}
	}
	if uuid == "" {
		return "no member carries a superblock", nil
	}
	return "", nil
}

// brokenLeftError is the group's error of a pass whose re-assembly refused
// (reassemblyRefusal): the array stays as it is, as before CN12's
// re-assembly, and its text keeps runningArrayErr's words and adds why. The
// read's verdict names the array once its reading is clear (CN30), which
// brings the worker's re-send.
func brokenLeftError(detail *MdDetail, why string) error {
	return fmt.Errorf("array %s is %s; not assembled again: %s",
		detail.Dev, detail.State, why)
}

// reassemblePass is ensureGroup's part for a group whose array reads broken or
// inactive with no forced-start mark and no operator in charge (reassembles; cnagent.md
// CN12, The re-assembly): once reassemblyRefusal finds nothing against it, the
// array is stopped (stopGroupArray) and assembled again by CN12's assembly
// (assembleGroup), whose plain assemble starts it from the members md holds
// newest, and it is read again (startedArray). A refusal leaves it as it is
// (brokenLeftError). A stop or an assembly that fails is the group's error,
// and a read names what a converge would do next: the broken array while it
// is there (reassemblyNamed), the absent array once the stop went through
// (absentArrayNamed). An assembly that leaves an array that does not run is
// the group's error by runningArrayErr, as it is after any assembly.
func (s *CnAgentServer) reassemblePass(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	walk *MdWalk,
	detail *MdDetail,
	available map[uint64]bool,
	legRows map[uint64]*pb.ResInfo,
) (*MdDetail, error) {
	why, err := s.reassemblyRefusal(ctx, plan, gp, detail,
		func(lp *legPlan) bool {
			return copySourceLeg(available[lp.legId], legRows[lp.legId])
		})
	if err != nil {
		return nil, err
	}
	if why != "" {
		return nil, brokenLeftError(detail, why)
	}
	if err := s.stopGroupArray(ctx, detail.Dev); err != nil {
		return nil, err
	}
	if err := s.md.Refresh(ctx, walk); err != nil {
		return nil, err
	}
	found, err := s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil {
		return nil, err
	}
	if found.Exists {
		return nil, fmt.Errorf(
			"array %s is still there after its stop: %s", found.Dev,
			found.State)
	}
	if err := s.assembleGroup(ctx, gp, available); err != nil {
		// As ensureGroup's plain assembly: an inactive array the failed
		// assembly left is stopped, and one that runs after all is kept.
		started, leftErr := s.assemblyLeft(context.WithoutCancel(ctx), plan,
			gp, walk)
		if leftErr != nil {
			return nil, fmt.Errorf("%w; %w", err, leftErr)
		}
		if !started {
			return nil, err
		}
	}
	found, err = s.startedArray(ctx, gp, walk)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, msgReassembly, append(s.cntlrIdAttrs(plan),
		slog.Uint64("grp_id", gp.grpId),
		slog.String("stopped", detail.Dev),
		slog.String("started", found.Dev))...)
	return found, nil
}

// msgReassembly is the Info record of CN12's re-assembly, made once the
// array it assembled again is there, at Info since the agent mended the
// group alone (log.md R6): it names the group, the broken or inactive
// array's node it stopped and the node the assembly started.
const msgReassembly = "array assembled again"

// errArrayNameInUse is the create's refusal while an array that holds none of
// the group's legs runs under the group's name (assembleGroup). Only an
// operator ends it, and a read names nothing for it: the read's dry run of
// the assembly meets the same refusal (absentArrayNamed).
var errArrayNameInUse = errors.New("holding none of the group's legs")

// errStoredMark is the failure a cntlr's read names a forced-start mark by
// while the request the agent holds does not count as delivered
// (storedMarks).
var errStoredMark = errors.New(
	"the mark comes from the stored request, or from one held after a " +
		"newer SyncupCntlr failed to save, so no converge acts on it")

// storedMarks names, in a cntlr's read-only verdict, each forced-start mark
// of a group a converge would build while the request the agent holds does
// not count as delivered (cntlrState.reqFromRpc) — the stored copy the
// startup reconcile loaded, or a held request whose newer successor
// syncupCntlr refused for a failed save — so that the worker re-sends the
// SyncupCntlr whose converge acts on the mark if it still stands (cnagent.md
// CN12, CN30). A converge of such a request acts on no mark
// (dropForcedStartMarks): it may be one the worker has cleared since, and a
// read names no forced start of such a request (forcedStartNamed), nor the
// absent array where the plain assembly is refused (absentArrayNamed), the
// case a mark is for. A
// deferred group builds nothing, and its provisioned flip raises the pool's
// revision, whose fan-out delivers the request.
func storedMarks(plan *cntlrPlan, res *agent.SweepResult) {
	if !plan.wantGrp {
		return
	}
	for _, gp := range plan.grps {
		if gp.mark == nil || !gp.raid1 || gp.deferred {
			continue
		}
		res.Fail(fmt.Sprintf("forced start of group %#x", gp.grpId),
			errStoredMark)
	}
}

// mdadmStarts reports whether mdadm's plain assembly (Md.Assemble), handed
// members whose superblocks are sbs, in that order, starts the array: the
// degraded gate of mdadm's Assemble.c, which holds for an explicit device
// list with neither --run nor --force, the form Md.Assemble runs (cnagent.md
// CN12), and the kernel's refusal of a member whose bad-block list it cannot
// import. The newest member is the one in a place of the array with the
// highest change count, the first handed among equals; each place keeps its
// newest member, the first handed among equals, and mdadm drops one whose
// superblock does not record the newest member's place as working
// (seesWorking). A kept member counts when its change count is the newest's
// or one below: in sync when it carries no copy position, rebuilding when it
// does. mdadm adds every kept member to the array, counted or not, and takes
// one off its count for each add the kernel refuses (BadBlocksRefused). The
// array starts when at least one member counts in sync, one such member's
// add goes through, since md runs no RAID1 array without one, and the
// members that count, less the refused adds, number at least the working
// members the newest superblock records (workingRoles). Reason: a read names
// an absent array only while the converge's assembly would start it
// (absentArrayNamed), and mdadm refuses a lone survivor whose superblock
// still counts its dead peer as working (cnagent_integtest.md, Lab facts),
// so a name without the gate would bring a SyncupCntlr whose assembly mdadm
// refuses, every second round for as long as the peer stays dead.
// Replacement members, which mdadm keeps beside the member of their place,
// are not modelled: dnv's adds never make one, since a repair takes its bad
// leg out of its place before its new leg's add and a leg_list member is
// added only to a free place, so only an operator's mdadm run makes one
// (cnagent.md, Known limits).
func mdadmStarts(sbs []*mdSuperblock) bool {
	var newest *mdSuperblock
	best := make(map[uint16]*mdSuperblock)
	for _, sb := range sbs {
		if sb.Role >= mdRoleMax {
			continue
		}
		if newest == nil || sb.Events > newest.Events {
			newest = sb
		}
		if cur := best[sb.Role]; cur == nil || sb.Events > cur.Events {
			best[sb.Role] = sb
		}
	}
	if newest == nil {
		return false
	}
	inSync, rebuilding, refused := 0, 0, 0
	inSyncAdded := false
	for place, sb := range best {
		if uint32(place) >= newest.RaidDisks ||
			(sb != newest && !sb.seesWorking(newest.Role)) {
			continue
		}
		if sb.BadBlocksRefused {
			refused++
		}
		if sb.Events+1 < newest.Events {
			continue
		}
		switch {
		case sb.Recovering:
			rebuilding++
		default:
			inSync++
			inSyncAdded = inSyncAdded || !sb.BadBlocksRefused
		}
	}
	return inSync >= 1 && inSyncAdded &&
		inSync+rebuilding-refused >= newest.workingRoles()
}

// readAvailability is the availability a cntlr's read gives the members a
// group keeps, its leg_list legs and the new legs of its open repair
// (readAvailable), as the converge's availability map holds it.
func (s *CnAgentServer) readAvailability(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
) map[uint64]bool {
	available := make(map[uint64]bool, len(gp.legs)+1)
	for _, lp := range gp.legs {
		available[lp.legId] = s.readAvailable(ctx, plan, lp)
	}
	if rp := gp.openRep(); rp != nil {
		for _, lp := range rp.newLegs {
			available[lp.legId] = s.readAvailable(ctx, plan, lp)
		}
	}
	return available
}

// actsOnMark reports whether a converge of the request the agent holds acts
// on the group's forced-start mark (CN12): one stands, and the request counts
// as delivered (cntlrState.reqFromRpc), since the converge of any other
// request drops the marks (dropForcedStartMarks).
func actsOnMark(st *cntlrState, gp *grpPlan) bool {
	return gp.mark != nil && st.reqFromRpc
}

// absentArrayNamed reports whether a cntlr's read names the group's array as
// absent (CN30), under agent.LeftoverKindMdAbsent: the group is a wanted
// RedundMdRaid1 group that is not deferred and whose mark no converge acts
// on (actsOnMark); found, the array the read's md walk matched to it
// (probeGroup), answered and is not there; and CN12's case selection
// (planAssembly), run with the read's availability (readAvailability),
// decides a create, or an assembly that mdadm's degraded gate lets start
// (mdadmStarts). So it names the array exactly while the converge's
// assembly would start it, short of the states the gate does not read
// (cnagent.md, Known limits), and names nothing for the refusals that last: a
// member not available, two identities, a group whose array has run and no
// member that carries a superblock, the name another array holds, and a
// start mdadm or the kernel refuses as mdadmStarts reads it. A superblock
// read, or a test of the name, that did not answer names nothing; the next
// read reads again. The converge passes its group's assembly to mdadm
// whatever the gate says: mdadm is the authority on its own refusal, and the
// gate here is only the read's guess of it. A guess that names where mdadm
// refuses costs a SyncupCntlr every second round while the state lasts, and
// one that withholds a name leaves the array to another re-drive; neither
// refuses a start (cnagent.md, Known limits).
//
// The assembly's refusal for two identities comes back too, nil for none:
// its legs' rows the read marks as the converge does (identityConflict).
func (s *CnAgentServer) absentArrayNamed(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	gp *grpPlan,
	found *MdDetail,
) (bool, *identityConflict) {
	if !plan.wantGrp || !gp.raid1 || gp.deferred || actsOnMark(st, gp) ||
		found == nil || found.Exists {
		return false, nil
	}
	ap, err := s.planAssembly(ctx, gp, s.readAvailability(ctx, plan, gp))
	var conflict *identityConflict
	if errors.As(err, &conflict) {
		return false, conflict
	}
	return err == nil && (ap.create || mdadmStarts(ap.sbs)), nil
}

// errForcedStartDue is the failure a cntlr's read names a group's forced
// start by (forcedStartNamed): its leg, and what a converge would do.
func errForcedStartDue(named *legPlan) error {
	return fmt.Errorf("no array of the group runs, and a converge starts "+
		"it from leg %#x", named.legId)
}

// errForcedStartStopDue is the failure a cntlr's read names a group's forced
// start by (forcedStartNamed) when a converge would stop the array dev of
// the group, which does not run, and the forced start's checks would then
// not hold: the stop alone.
func errForcedStartStopDue(dev string) error {
	return fmt.Errorf("array %s of the group does not run, and a converge "+
		"stops it", dev)
}

// forcedStartNamed is the failure by which a cntlr's read names the group's
// forced start (CN30), nil for none. It names one only when a converge acts
// on the group's mark (actsOnMark), the group is a wanted RedundMdRaid1
// group that is not deferred, found, the array the read's md walk matched
// to it (probeGroup), answered and no array of the group runs, and an array
// of the group that is there holds no member that is not a leg wrapper of
// this pool (MdDetail.strangerErr) and md's holders listing of it answers
// with no holder, so no layer above blocks the forced start's stop
// (forcedStartPass). Then it names the start (errForcedStartDue) when the
// forced start's checks (planForcedStart), run with the read's availability
// (readAvailability), return a plan, so the converge goes through to its
// zeroes and its assembly; otherwise the stop alone (errForcedStartStopDue)
// when such an array is there, which the converge stops before any check;
// and otherwise nothing. Reason for the stop's name: md moves a broken
// array between broken and running states while it retries a copy whose
// only source is dead (mdRunningStates), so the converge a mark brings can
// read the array running and leave the mark waiting; while the named leg is
// dead the checks do not hold, and without the name nothing would stop the
// array before that leg is back. Once the stop is made no array of the
// group is there, and the read names nothing until the checks hold, so each
// read that finds the array not running brings at most one converge, and a
// read that finds it running brings none. So it names nothing for an array
// of the group that holds a member that is no leg of this pool or that a
// device above holds; and, with no array of the group there, nothing for
// the refusals that last — a named leg that is no full member, carries a
// bad-block list or is older than another under a mark that does not pass
// it — and nothing while a member the start keeps is not available, a kept
// new leg that is still provisioning among them, whose provisioned flip
// raises the pool's revision. A process that holds the array open shows in
// no holders listing, and the stop it refuses is named on every read
// (cnagent.md, Known limits). A read of the holders that did not answer
// names nothing, and so does a read of a superblock that did not answer
// while no array of the group is there; the next read reads again.
func (s *CnAgentServer) forcedStartNamed(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	gp *grpPlan,
	found *MdDetail,
) error {
	if !plan.wantGrp || !gp.raid1 || gp.deferred || !actsOnMark(st, gp) ||
		found == nil || (found.Exists && mdRunning(found.State)) {
		return nil
	}
	if found.Exists {
		if found.strangerErr(plan.clusterId, plan.cnId, plan.spId) != nil {
			return nil
		}
		holders, err := s.md.Holders(ctx, found.Dev)
		if err != nil || len(holders) > 0 {
			return nil
		}
	}
	fp, _, err := s.planForcedStart(ctx, gp, s.readAvailability(ctx, plan, gp))
	switch {
	case err == nil:
		return errForcedStartDue(fp.named)
	case found.Exists:
		return errForcedStartStopDue(found.Dev)
	}
	return nil
}

// forcedStartPlan is what CN12's forced start would do from its checks
// (planForcedStart): the named leg and its superblock read, the members it
// leaves with no superblock, in the order it zeroes them, and those among
// them whose read showed none already, which it counts as zeroed with no
// zero run.
type forcedStartPlan struct {
	named *legPlan
	sb    *mdSuperblock
	zero  []*legPlan
	blank map[uint64]bool
}

// update is the assembly's update the plan's start runs first, then, with
// a bad-block list on the named leg, the second run's (forcedAssemble).
func (fp *forcedStartPlan) update() string {
	if fp.sb.BadBlocks > 0 {
		return mdUpdateUuid + "," + mdUpdateForceNoBbl
	}
	return mdUpdateUuid
}

// forcedStartRefusal is one refusal of CN12's forced start that an
// override passes: its kind and what it says.
type forcedStartRefusal struct {
	override pb.ForcedStartOverride
	text     string
}

// forcedStartWords are the words that name each kind of refusal an override
// passes, in a row's text and in the record of a refused start.
var forcedStartWords = map[pb.ForcedStartOverride]string{
	pb.ForcedStartOverride_FORCED_START_OVERRIDE_OLDER_LEG:  "older-leg",
	pb.ForcedStartOverride_FORCED_START_OVERRIDE_BAD_BLOCKS: "bad-blocks",
	pb.ForcedStartOverride_FORCED_START_OVERRIDE_PARTIAL:    "partial",
}

// forcedStartPasses reports whether the group's mark passes a refusal of
// the kind override: its override_list names the kind, or it carries the
// force a mark an older gateway wrote may carry, which passes every kind.
func forcedStartPasses(gp *grpPlan, override pb.ForcedStartOverride) bool {
	return gp.mark.GetForce() ||
		slices.Contains(gp.mark.GetOverrideList(), override)
}

// forcedStartRefused is the group's error of a forced start whose checks
// all held but for refusals the mark does not pass: its text names their
// kinds first, each once, then each refusal, so a row cut to the text cap
// keeps the kinds at its head (agent.CutAgentText).
type forcedStartRefused struct {
	refusals []forcedStartRefusal
}

func (e *forcedStartRefused) Error() string {
	var words, texts []string
	for _, r := range e.refusals {
		if word := forcedStartWords[r.override]; !slices.Contains(words, word) {
			words = append(words, word)
		}
		texts = append(texts, r.text)
	}
	return fmt.Sprintf("forced start refused (%s): %s",
		strings.Join(words, ", "), strings.Join(texts, "; "))
}

// planForcedStart is the checks of CN12's forced start, with no mdadm
// command and no write, for the mark the group's plan carries: the named
// leg is a member the forced start keeps (forcedStartLegs), other than the
// bad leg of a STARTED repair (isStartedBadLeg), available, and its
// superblock read (ReadMember) answers and shows a full member, one md held
// in a place with no copy position (mdSuperblock.notFullMember); every other
// member the forced start keeps but the open repair's bad leg is available
// and its read answers, each read before any is zeroed. The first of these
// that does not hold is its error, the group's, and ends the checks. On the
// way it meets the refusals an override passes and goes on past each: a
// named leg that carries a bad-block list (badBlocksRefusal), and each other
// member that is a full member of the named leg's identity with a higher
// change count (olderNamedLeg). refusals are every such refusal met before
// the checks ended, passed or not, in that order; when every check held and
// the mark does not pass one of them (forcedStartPasses), the error is
// forcedStartRefused, naming those. The real start (forceStartGroup) and
// the dry run of InspectGrp both run it, so a broken check fails the tests
// of both.
func (s *CnAgentServer) planForcedStart(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
) (*forcedStartPlan, []forcedStartRefusal, error) {
	named, kept := gp.forcedStartLegs()
	if named == nil && gp.isStartedBadLeg(gp.mark.GetLegId()) {
		return nil, nil, fmt.Errorf(
			"forced start names leg %#x, the bad leg of group %#x's "+
				"started repair, which the finish retires",
			gp.mark.GetLegId(), gp.grpId)
	}
	if named == nil {
		return nil, nil, fmt.Errorf(
			"forced start names leg %#x, which group %#x does not keep",
			gp.mark.GetLegId(), gp.grpId)
	}
	if !available[named.legId] {
		return nil, nil, errForcedStartWaits(named)
	}
	reading := s.md.ReadMember(ctx, named.path)
	if reading.Err != nil {
		return nil, nil, fmt.Errorf("forced start: leg %#x: %w",
			named.legId, reading.Err)
	}
	sb := reading.Sb
	if gap := sb.notFullMember(); gap != "" {
		return nil, nil, fmt.Errorf(
			"forced start: leg %#x is not a full member: %s",
			named.legId, gap)
	}
	var refusals []forcedStartRefusal
	if text := badBlocksRefusal(named, sb); text != "" {
		refusals = append(refusals, forcedStartRefusal{
			pb.ForcedStartOverride_FORCED_START_OVERRIDE_BAD_BLOCKS, text})
	}
	fp := &forcedStartPlan{named: named, sb: sb, blank: map[uint64]bool{}}
	for _, lp := range kept {
		if lp == named || gp.isBadLeg(lp.legId) {
			continue
		}
		if !available[lp.legId] {
			return nil, refusals, errForcedStartWaits(lp)
		}
		other := s.md.ReadMember(ctx, lp.path)
		if other.Err != nil {
			return nil, refusals, fmt.Errorf(
				"forced start: member leg %#x: %w", lp.legId, other.Err)
		}
		if text := olderNamedLeg(named, sb, lp, other.Sb); text != "" {
			refusals = append(refusals, forcedStartRefusal{
				pb.ForcedStartOverride_FORCED_START_OVERRIDE_OLDER_LEG, text})
		}
		fp.zero = append(fp.zero, lp)
		if other.Sb == nil {
			fp.blank[lp.legId] = true
		}
	}
	var refused []forcedStartRefusal
	for _, r := range refusals {
		if !forcedStartPasses(gp, r.override) {
			refused = append(refused, r)
		}
	}
	if len(refused) > 0 {
		return nil, refusals, &forcedStartRefused{refusals: refused}
	}
	return fp, refusals, nil
}

// forceStartGroup is CN12's forced start, which replaces the assembly while
// the group's mark stands and no array of the group is there: its checks
// (planForcedStart) hold; each member they would zero whose read showed a
// superblock has it zeroed and read back, and the zero counts only when the
// read answers with no superblock, while one whose read showed none counts
// as zeroed with no zero run, since a zero named for the 1.2 format fails on
// a member that carries none (ZeroSuperblock); and the array starts from the
// named leg alone under a new identity (forcedAssemble), with the leg's
// bad-block list dropped when its read shows one. Each step that does not
// hold is the group's error and ends the pass for the group, the zero before
// any assembly, so no pass starts the array beside a member it has not
// zeroed; the mark stands and the next pass, which the worker's re-send
// brings once a read names the forced start (forcedStartNamed), tries again
// from the first step. A member zeroed by a pass that ends later is blank at
// the next, which counts it as zeroed. ensureGroup's member reconciliation
// then adds the zeroed members, which md copies whole.
func (s *CnAgentServer) forceStartGroup(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	walk *MdWalk,
	available map[uint64]bool,
) error {
	fp, met, err := s.planForcedStart(ctx, gp, available)
	var refused *forcedStartRefused
	if errors.As(err, &refused) {
		texts := make([]string, 0, len(refused.refusals))
		for _, r := range refused.refusals {
			texts = append(texts, forcedStartWords[r.override]+": "+r.text)
		}
		passed := []string{}
		for _, r := range met {
			word := forcedStartWords[r.override]
			if forcedStartPasses(gp, r.override) &&
				!slices.Contains(passed, word) {
				passed = append(passed, word)
			}
		}
		slog.WarnContext(ctx, msgForcedStartRefused,
			append(s.cntlrIdAttrs(plan),
				slog.Uint64("grp_id", gp.grpId),
				slog.Uint64("leg_id", gp.mark.GetLegId()),
				slog.Any("refusals", texts),
				slog.Any("overridden", passed))...)
	}
	if err != nil {
		return err
	}
	zeroed := make([]uint64, 0, len(fp.zero))
	for _, lp := range fp.zero {
		if fp.blank[lp.legId] {
			zeroed = append(zeroed, lp.legId)
			continue
		}
		if err := s.md.ZeroSuperblock(ctx, lp.path); err != nil {
			return err
		}
		back := s.md.ReadMember(ctx, lp.path)
		if back.Err != nil {
			return fmt.Errorf("forced start: the zero of leg %#x is not "+
				"proven: %w", lp.legId, back.Err)
		}
		if back.Sb != nil {
			return fmt.Errorf("forced start: leg %#x still carries a "+
				"superblock of %s after its zero", lp.legId, back.Sb.Uuid)
		}
		zeroed = append(zeroed, lp.legId)
	}
	update, err := s.forcedAssemble(ctx, plan, gp, walk, fp.named, fp.sb)
	if err != nil {
		return err
	}
	slog.WarnContext(ctx, msgForcedStart, append(s.cntlrIdAttrs(plan),
		slog.Uint64("grp_id", gp.grpId),
		slog.Uint64("leg_id", fp.named.legId),
		slog.Any("zeroed", zeroed),
		slog.String("update", update))...)
	return nil
}

// badBlocksRefusal is CN12's refusal of a named leg that carries a bad-block
// list, "" when the leg's superblock read shows none. The start drops the
// list (forcedStartPlan.update), and md then serves the regions it listed
// as written, from what the leg holds there — on a half-copied leg md
// marked in sync by mistake, the regions it never copied, another pool's
// bytes among them. Past it the drop is the operator's choice, which the
// inspect call's list length and the dry run's update show first.
func badBlocksRefusal(named *legPlan, namedSb *mdSuperblock) string {
	if namedSb.BadBlocks == 0 {
		return ""
	}
	return fmt.Sprintf("named leg %#x carries a bad-block list of %d "+
		"entries, which the start drops, so md would serve the regions it "+
		"lists as written", named.legId, namedSb.BadBlocks)
}

// olderNamedLeg is CN12's refusal of a named leg older than a member the
// forced start would zero, "" when none holds: the other member is a full
// member of the named leg's identity with a higher change count. Within one
// identity md raises the count on every change of the array, so such a
// member holds newer data than the named leg, which the zero and the copy
// from the named leg would lose; across identities the counts mean nothing,
// and a member that is not full (a half-copied one, a spare) holds no whole
// copy, so neither is compared.
func olderNamedLeg(
	named *legPlan,
	namedSb *mdSuperblock,
	other *legPlan,
	otherSb *mdSuperblock,
) string {
	if otherSb == nil || otherSb.notFullMember() != "" ||
		otherSb.Uuid != namedSb.Uuid || otherSb.Events <= namedSb.Events {
		return ""
	}
	return fmt.Sprintf("leg %#x is a full member with md change count %d, "+
		"above the %d of named leg %#x, so its newer data would be copied "+
		"over", other.legId, otherSb.Events, namedSb.Events, named.legId)
}

// forcedAssemble is the assembly of CN12's forced start (forceStartGroup):
// the array starts from the named leg alone (AssembleForced) under a new
// identity (mdUpdateUuid), and update is the superblock update, or the two,
// of the runs that started it. When that run fails, the same pass reads what
// it left (assemblyLeft): an array that does not run is stopped, and one
// that runs, which a run killed at its time limit (MdBuildTimeout) can have
// started in the kernel, is taken as started.
//
// A second run drops the named leg's bad-block list (mdUpdateForceNoBbl), as
// mdadm takes one update a run, in two cases. When the first run started
// the array and the leg's reading before it (before) showed a list: such a
// start keeps the list, and the array would serve IO errors over regions the
// leg holds, so the array, under its new identity, is stopped and started
// again. And when the first run failed and the named leg's superblock, read
// again, carries an identity other than before's: mdadm writes an
// assembly's update into the member's superblock before the kernel takes
// the member, so a run the kernel refused for its bad-block list has already
// given the leg its new identity, which the second run keeps; a run that
// failed before the write leaves the old one, under which a second run
// would start the array, so its failure is the group's error and the next
// pass runs the first form again. A second run that fails is read again the
// same way, and is otherwise the group's error with both runs' outputs. So
// the array never starts under its old identity.
func (s *CnAgentServer) forcedAssemble(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	walk *MdWalk,
	named *legPlan,
	before *mdSuperblock,
) (update string, err error) {
	// The mark is acted on from here, whatever the runs bring: a run killed
	// at its timeout, or a read after it that fails, can leave the array
	// started with an error, and while the mark stands no other step brings
	// the group's array up on this node (ensureGroup), so an array that runs
	// then is this start's (cnagent.md CN12, CN28).
	gp.forcedRan = true
	first := s.md.AssembleForced(ctx, gp.devPath, gp.mdArrayName,
		named.path, mdUpdateUuid)
	started := first == nil
	if !started {
		started, err = s.assemblyLeft(ctx, plan, gp, walk)
		if err != nil {
			return "", fmt.Errorf("%w; %w", first, err)
		}
	}
	if started {
		if before.BadBlocks == 0 {
			return mdUpdateUuid, nil
		}
		if err := s.stopStartedArray(ctx, plan, gp, walk); err != nil {
			return "", fmt.Errorf("forced start: the array started from "+
				"leg %#x under its new identity keeps the leg's bad-block "+
				"list, and its stop to drop the list failed: %w",
				named.legId, err)
		}
	} else {
		after := s.md.ReadMember(ctx, named.path)
		if after.Err != nil {
			return "", fmt.Errorf("%w; %w", first, after.Err)
		}
		if after.Sb == nil || after.Sb.Uuid == before.Uuid {
			return "", first
		}
	}
	update = mdUpdateUuid + "," + mdUpdateForceNoBbl
	second := s.md.AssembleForced(ctx, gp.devPath, gp.mdArrayName,
		named.path, mdUpdateForceNoBbl)
	if second == nil {
		return update, nil
	}
	started, err = s.assemblyLeft(ctx, plan, gp, walk)
	if err != nil {
		return "", fmt.Errorf("%w; %w", second, err)
	}
	if started {
		return update, nil
	}
	if first != nil {
		return "", fmt.Errorf("%w; %w", first, second)
	}
	return "", second
}

// stopStartedArray stops the array the first run of a forced assembly
// started (forcedAssemble), found by the members the group keeps as the
// forced start's own stop finds it, under the same member test
// (MdDetail.strangerErr) and verify (stopGroupArray). An array that is
// already gone counts as stopped.
func (s *CnAgentServer) stopStartedArray(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	walk *MdWalk,
) error {
	if err := s.md.Refresh(ctx, walk); err != nil {
		return err
	}
	detail, err := s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil {
		return err
	}
	if !detail.Exists {
		return nil
	}
	if err := detail.strangerErr(
		plan.clusterId, plan.cnId, plan.spId); err != nil {
		return err
	}
	return s.stopGroupArray(ctx, detail.Dev)
}

// errForcedStartWaits is the group's error while CN12's forced start waits
// for a member it needs that is not available this pass. A read names the
// forced start once that member serves (forcedStartNamed), which brings the
// worker's re-send. The pass has stopped an array of the group that did not
// run before the wait (forcedStartPass); while such an array is there, as
// one the read finds after a pass that read it running, the read names that
// stop alone, whatever the wait (errForcedStartStopDue).
func errForcedStartWaits(lp *legPlan) error {
	return fmt.Errorf("forced start waits for leg %#x, which is not available",
		lp.legId)
}

// assemblyLeft is what an assembly that failed left of the group's array, a
// forced one's (forcedAssemble) or a plain one's (ensureGroup): started
// reports an array that runs, and one that does not run — a failed assembly
// can leave an inactive array holding a leg, as one its time limit
// (MdBuildTimeout) stopped once md took it — is stopped, so that the next
// assembly finds the
// leg free. An array holding a member that is not a dm device
// is the group's error whether it runs or not, as in ensureGroup, and one
// that does not run is stopped only when the sweep would call it ours: one
// that holds any other member is the group's error, and nothing stops it
// (MdDetail.strangerErr).
func (s *CnAgentServer) assemblyLeft(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	walk *MdWalk,
) (started bool, err error) {
	if err := s.md.Refresh(ctx, walk); err != nil {
		return false, err
	}
	detail, err := s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil {
		return false, err
	}
	switch {
	case !detail.Exists:
		return false, nil
	case detail.Foreign:
		return false, detail.foreignErr()
	case mdRunning(detail.State):
		return true, nil
	}
	if err := detail.strangerErr(
		plan.clusterId, plan.cnId, plan.spId); err != nil {
		return false, err
	}
	return false, s.stopGroupArray(ctx, detail.Dev)
}

// stopGroupArray stops a group's array that does not run, for CN12's forced
// start, by the node sysfs named, and verifies the stop from sysfs (Md.Gone)
// as the sweep's stopArrayVerified does: a stop killed at the soft timeout
// that the kernel completed counts, and an array still there is the group's
// error, the stop's own when it failed. A layer above that holds the array
// open makes the stop fail: the slice's concat at a level that builds thin
// pools, and a clone's dm-clone over its clone meta group at a level that
// builds clones. A verify that did not answer is the group's error too: the
// stop may have gone through, as when array_state reads EBUSY from the
// moment md marks the stopped array deleted until its md/ goes (cnagent.md
// CN12), and a read that finds the array absent names the forced start
// (forcedStartNamed), whose re-send brings the converge that starts it. The sweep's stop that did not verify is a Leftover,
// which the worker re-drives; this one is a row.
func (s *CnAgentServer) stopGroupArray(ctx context.Context, dev string) error {
	stopErr := s.md.Stop(ctx, dev)
	gone, err := s.md.Gone(ctx, dev)
	if err != nil {
		return fmt.Errorf("array %s stop not verified: %w", dev, err)
	}
	if gone {
		return nil
	}
	if stopErr != nil {
		return stopErr
	}
	return fmt.Errorf("array %s did not stop", dev)
}

// reconcileMembers is CN12's member reconciliation: a member the array holds
// that the group does not keep, such as an aborted repair's new leg, is
// failed and removed, but only while a member that stays keeps the data
// (inSyncGuard); an available leg_list member the array lacks is added, and
// md then resyncs it, from its write-intent bitmap for a briefly absent leg.
// And it puts a member md has failed back: a leg_list member md has failed
// and moved out of its slot (MdMember.failedOutOfSlot), on a leg that
// can take it (legTakesMember), is removed, and the add loop adds it again —
// both, and the add of a member the array lacks, only beside a copy source
// (hasCopySource) —
// which mdadm turns into a re-add that md resyncs from its write-intent
// bitmap. md keeps its claim on a failed member until the member is removed,
// so an --add alone is refused. Before the put-back it fails, and does not
// remove, a leg_list member whose leg is dead while another member keeps
// the data (deadMembers, from dead, the legs CnAgentServer.deadLegs read at
// the pass's start), so the death reaches the survivor's superblock on an
// idle array, and the put-back takes the member back from the bitmap once
// its leg serves again. The put-back is an mdadm child the soft
// timeout can kill, never a write to the member's sysfs state, which the
// agent's own process would make and which waits on md's lock for as long as
// a superblock write is stuck on a member.
//
// A group's repair adds its own steps (CN12): the members the group keeps are
// its leg_list wrappers and the new legs of its open repair
// (grpPlan.memberNames); an open repair's bad leg is never added or put back,
// and one md holds without a place leaves the array once the repair has
// started, STARTED or every new leg holding a place (startedBadLegs); a new
// leg md holds without a place leaves the array while its repair is CREATED
// with the abort flag set (abortRemovals); a new leg md has failed is put back
// as a leg_list member is, unless its repair is CREATED with that flag set
// (putBackLegs); and the new legs are added last, the bad leg failed and
// removed first when it cannot take a member, under CN12's conditions
// (repSteps) and its take-back check (takeBackStep), whose refusal it
// returns for the repair's verdict. It reports whether it ran an mdadm
// command against the array, so the converge reads the array again for its
// report (ensureGroup).
//
// The held set is the dm names sysfs gave the array's members
// (dev-*/block/dm/name), compared with the names of the wrappers the group
// keeps — the key Detail matched the array by — and each held member is
// addressed by its dm path. Nothing here reads a device number: were the
// wanted set keyed by the number of each leg_list wrapper, a number read
// that did not answer would read as "not wanted" and fail and remove that
// wrapper's in-sync member. A leg whose wrapper this pass could not build is
// never available, so the add loop leaves it alone, and its member, if md
// still holds one, stays wanted and held, failed or not.
// `mdadm --fail` and `--remove` open the member's path (open only, no IO) and
// then act on its device number. `--remove` can sleep in md's suspend for as
// long as a superblock write is stuck on the member — seconds after its side
// dies with a write in flight — and the soft timeout then kills it with the
// member still held: this pass fails before the add loops, so a leg_list
// member the array lacks is not added either, nor, after a repair's bad leg,
// its new leg. A
// remove or an add that fails is the group's error for the pass, a row, and
// nothing in this agent runs it again by itself. What re-drives it is the read's
// verdict, which names a member this function would put back or add on a leg
// the read finds able to take it, or remove for a repair (abortRemovals,
// startedBadLegs; putBackNames, cnagent.md CN30), so the worker re-sends the
// SyncupCntlr whose converge tries again. A member the group does not keep
// is never named itself. Nothing here remembers the
// failure. A refusal of the in-sync guard
// is re-driven the same way once it clears: until then the read's row reads
// ERROR too and its verdict names nothing for the group, and the first read
// that finds a member that stays in sync on an available leg reads the row
// OK and names the leg_list member the array lacks once its leg can take it.
// The refusal skips the put-back too, so a leg_list member md has failed
// stays out while the guard holds (cnagent.md CN12).
func (s *CnAgentServer) reconcileMembers(
	ctx context.Context,
	gp *grpPlan,
	detail *MdDetail,
	available map[uint64]bool,
	legRows map[uint64]*pb.ResInfo,
	dead map[uint64]bool,
) (bool, grpRefusals, error) {
	// A Foreign array never gets here (ensureGroup), so every member has a
	// dm name.
	held := heldMembers(detail)
	kept := make(map[string]bool, len(gp.legs)+1)
	for _, name := range gp.memberNames() {
		kept[name] = true
	}
	// The in-sync guard comes before the first --fail, and its refusal
	// leaves the array exactly as it is: no fail, no put-back and no add
	// this pass. The test is the same for every member that leaves — the
	// member it asks for stays in leg_list, which this loop never fails —
	// so one test guards each fail of the loop.
	if leaver := inSyncGuard(gp, detail, func(lp *legPlan) bool {
		return available[lp.legId]
	}); leaver != "" {
		return false, grpRefusals{}, errInSyncGuard(leaver, detail.Dev)
	}
	changed := false
	var refusals grpRefusals
	run := func(verb func(context.Context, string, string) error,
		path string) error {
		changed = true
		return verb(ctx, gp.devPath, path)
	}
	// Extras leave before any member is added, so the array never briefly
	// holds more members than `--raid-devices`.
	for _, member := range detail.Members {
		if kept[member.DmName] {
			continue
		}
		path := s.nf.DmPath(member.DmName)
		if err := run(s.md.Fail, path); err != nil {
			return changed, grpRefusals{}, err
		}
		if err := run(s.md.Remove, path); err != nil {
			return changed, grpRefusals{}, err
		}
	}
	for _, lp := range append(abortRemovals(gp, held),
		startedBadLegs(gp, held)...) {
		if err := run(s.md.Remove, lp.path); err != nil {
			return changed, grpRefusals{}, err
		}
		delete(held, lp.name)
	}
	takes := func(lp *legPlan) bool {
		return legTakesMember(available[lp.legId], legRows[lp.legId])
	}
	sources := func(lp *legPlan) bool {
		return copySourceLeg(available[lp.legId], legRows[lp.legId])
	}
	// The early fail of a dead leg's member (deadMembers): failed, not
	// removed, so the put-back takes it back once its leg serves again. No
	// later step of the pass reads the member's state: its leg does not
	// serve, so it is neither a copy source nor a leg that takes a member.
	failed := make(map[string]bool)
	for _, lp := range deadMembers(gp, held, func(lp *legPlan) bool {
		return dead[lp.legId] && !available[lp.legId]
	}, sources) {
		if err := run(s.md.Fail, lp.path); err != nil {
			return changed, grpRefusals{}, err
		}
		failed[lp.name] = true
	}
	// The put-back's remove. A removed member is no longer held, so the
	// add loops below add it in this same pass.
	for _, lp := range putBackLegs(gp, held, takes, sources) {
		if err := run(s.md.Remove, lp.path); err != nil {
			return changed, grpRefusals{}, err
		}
		delete(held, lp.name)
	}
	arrayUuid := s.arrayUuidOnce(detail)
	for _, lp := range gp.legs {
		if _, ok := held[lp.name]; ok || !available[lp.legId] ||
			gp.isBadLeg(lp.legId) ||
			!hasCopySource(gp, held, sources, lp.name) {
			continue
		}
		why, err := s.legListAddRefusal(ctx, gp, lp, arrayUuid)
		if err != nil {
			return changed, refusals, err
		}
		if why != "" {
			if refusals.grp == "" {
				refusals.grp = why
			}
			// The leg row is marked only while it reads OK, as a read,
			// which checks only such a leg (putBackNames), marks it.
			if takes(lp) {
				refusals.otherIdentity = append(refusals.otherIdentity,
					lp.legId)
			}
			continue
		}
		if err := run(s.md.Add, lp.path); err != nil {
			return changed, refusals, err
		}
	}
	step := repSteps(gp, held, func(lp *legPlan) bool {
		return available[lp.legId]
	}, takes, sources)
	// The take-back check comes before the bad leg goes out: a refused add
	// leaves the step, and a step it leaves with no add is empty, so the bad
	// leg stays in its place unless another new leg is added.
	step, rep, err := s.takeBackStep(ctx, gp, held, takes, step)
	if err != nil {
		return changed, refusals, err
	}
	for i := range step.out {
		// A bad leg the early fail failed this pass is not failed again.
		if failed[step.out[i].lp.name] {
			step.out[i].fail = false
		}
	}
	for _, out := range step.out {
		if out.fail {
			if err := run(s.md.Fail, out.lp.path); err != nil {
				return changed, refusals, err
			}
		}
		if err := run(s.md.Remove, out.lp.path); err != nil {
			return changed, refusals, err
		}
	}
	for _, lp := range step.add {
		if err := run(s.md.Add, lp.path); err != nil {
			return changed, refusals, err
		}
	}
	refusals.rep, refusals.waits = rep, step.waits
	return changed, refusals, nil
}

// grpRefusals are the refusals of one pass over a group that the primary
// reports beside the group's reading (CN28): rep is the take-back check's
// (takeBackStep), which the repair's verdict carries, and grp the identity
// check's on a leg_list member's add (legListAddRefusal), which the group's
// reading carries. "" where nothing holds an add back. waits is why the
// repair's step waits (repStep.waits), which the verdict carries too.
// otherIdentity are the legs whose add the identity check refused and whose
// leg row reads OK, which the pass marks RES_REASON_OTHER_IDENTITY
// (markLegRows), a converge's failure after the check included: from the
// identity check on, reconcileMembers and ensureGroup return the refusals
// beside their error, as a read keeps them past a failed take-back read
// (putBackNames).
type grpRefusals struct {
	rep           string
	grp           string
	waits         string
	otherIdentity []uint64
}

// arrayUuidOnce reads the identity of the group's running array once, at
// its first use (Md.ArrayUuid): a pass whose leg_list members all sit in the
// array reads nothing.
func (s *CnAgentServer) arrayUuidOnce(
	detail *MdDetail,
) func(context.Context) (string, error) {
	var uuid string
	var err error
	read := false
	return func(ctx context.Context) (string, error) {
		if !read {
			uuid, err = s.md.ArrayUuid(ctx, detail.Dev)
			read = true
		}
		return uuid, err
	}
}

// legListAddRefusal is CN12's identity check on the add of a leg_list member
// that the running array lacks, the converge's (reconcileMembers) and the
// read's (putBackNames) alike, "" for a member the add may take: one whose
// reading shows no superblock, which md copies whole, or one of the array's
// identity (arrayUuid), which md takes back from its bitmap or copies whole.
// A member whose superblock carries another identity is of an older
// history, as a forced start from another leg leaves the open repair's bad
// leg and the move to ABORTED then makes it a member like any other, and it
// can hold data the array lacks, which md's copy would overwrite with no
// error; so its add is refused, and the refusal names it, unless it is a bad
// leg of the group's repair, at any stage, and that repair carries the
// operator's force. The refusal stays while the leg does: the operator
// replaces it with a repair, which keeps its data as a retired leg, or
// starts the group from it with a forced start. A reading whose read failed
// or did not answer, or an array identity that did not read, is the error.
func (s *CnAgentServer) legListAddRefusal(
	ctx context.Context,
	gp *grpPlan,
	lp *legPlan,
	arrayUuid func(context.Context) (string, error),
) (string, error) {
	reading := s.md.ReadMember(ctx, lp.path)
	if reading.Err != nil {
		return "", reading.Err
	}
	if reading.Sb == nil {
		return "", nil
	}
	uuid, err := arrayUuid(ctx)
	if err != nil {
		return "", err
	}
	if reading.Sb.Uuid == uuid {
		return "", nil
	}
	if gp.rep != nil && gp.rep.badLegIds[lp.legId] && gp.rep.force() {
		return "", nil
	}
	return fmt.Sprintf("leg %#x carries another array's superblock, %s, "+
		"beside the running array's %s, so it may hold data the array "+
		"lacks; it is not added", lp.legId, reading.Sb.Uuid, uuid), nil
}

// heldMembers is an array's members by dm name.
func heldMembers(detail *MdDetail) map[string]MdMember {
	held := make(map[string]MdMember, len(detail.Members))
	for _, member := range detail.Members {
		held[member.DmName] = member
	}
	return held
}

// abortRemovals are the new legs of the group's repair that md holds without
// a place, its slot none, failed or not, while the repair is CREATED with its
// abort flag in the request: the converge removes them, never adds one, and
// the read's verdict names them, so that the primary can confirm the abort,
// which needs md to hold nothing on the new leg (CN12, CN30). A new leg that
// holds a place stays: the repair has started, and the verdict says so once
// md holds nothing on the bad leg (startedBadLegs).
func abortRemovals(gp *grpPlan, held map[string]MdMember) []*legPlan {
	if gp.rep == nil || !gp.rep.aborting() {
		return nil
	}
	var out []*legPlan
	for _, lp := range gp.rep.newLegs {
		if member, ok := held[lp.name]; ok && member.Slot == mdSlotNone {
			out = append(out, lp)
		}
	}
	return out
}

// startedBadLegs are the bad legs of the group's open repair that md holds
// without a place once the repair has started in md: it is STARTED, or it is
// CREATED, with or without its abort flag, and md holds every new leg in a
// place (MdMember.holdsPlace). The converge removes them, and the read's
// verdict names them (CN12, CN30). The primary reports started only once md
// holds nothing on the bad leg (repairVerdict), so md holds one again only
// from an assembly that took it back, as after a failover that comes before
// or after the worker's move to STARTED. Left there, it would keep a CREATED
// repair from being reported started, and, its new leg holding a place, from
// having its abort confirmed, so the repair would never move on; and the
// finish of a STARTED one would make it a retired leg, which no cntlr
// connects, so the sweep would read an array holding it as unwanted and stop
// it (CN21). A member without a place holds no data md reads, and md removes
// it with no IO to it.
func startedBadLegs(gp *grpPlan, held map[string]MdMember) []*legPlan {
	rp := gp.openRep()
	if rp == nil {
		return nil
	}
	if rp.rep.GetRepStage() == pb.RepStage_REP_STAGE_CREATED {
		if len(rp.newLegs) == 0 {
			return nil
		}
		for _, lp := range rp.newLegs {
			if member, ok := held[lp.name]; !ok || !member.holdsPlace() {
				return nil
			}
		}
	}
	var out []*legPlan
	for _, lp := range gp.legs {
		if member, ok := held[lp.name]; ok && rp.badLegIds[lp.legId] &&
			member.Slot == mdSlotNone {
			out = append(out, lp)
		}
	}
	return out
}

// putBackLegs are the members the put-back removes, so that an add loop adds
// them again (CN12): md has failed each and moved it out of its slot, on a
// leg that can take it (takes), another member can feed the copy md starts
// when it joins again (hasCopySource, over sources), and each is a leg_list
// member other than an open repair's bad leg, or a new leg of a repair that
// is STARTED, or CREATED with no abort flag in the request.
func putBackLegs(
	gp *grpPlan,
	held map[string]MdMember,
	takes, sources func(*legPlan) bool,
) []*legPlan {
	var out []*legPlan
	check := func(lp *legPlan) {
		if member, ok := held[lp.name]; ok && member.failedOutOfSlot() &&
			takes(lp) && hasCopySource(gp, held, sources, lp.name) {
			out = append(out, lp)
		}
	}
	for _, lp := range gp.legs {
		if !gp.isBadLeg(lp.legId) {
			check(lp)
		}
	}
	if rp := gp.rep; rp != nil && (rp.mayAdd() ||
		rp.rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED) {
		for _, lp := range rp.newLegs {
			check(lp)
		}
	}
	return out
}

// repStep is what CN12's add of a repair's new legs does in one converge:
// the bad legs it takes out of the array first, each failed first when md
// still holds it in a place, and the new legs it adds; and waits, why the
// step holds the repair back, "" when nothing does (CN12, Why a repair
// waits): text for people, which the repair's verdict carries.
type repStep struct {
	out   []repBadLeg
	add   []*legPlan
	waits string
}

// The texts of repStep.waits and of the verdict's waits (CN12, Why a repair
// waits), each naming the leg it is about as md.go's other texts do.
const (
	waitsNewLegUnprovisioned = "new leg %#x: its side is not provisioned yet"
	waitsNewLegUnavailable   = "new leg %#x is not available"
	waitsNoCopySource        = "new leg %#x: no member in sync can feed its copy"
	waitsNoKeeper            = "no member in sync is on a leg whose row " +
		"reads OK and that serves, to keep the data"
	waitsBadLegServes  = "bad leg %#x holds its place in the array and serves"
	waitsNoCleanKeeper = "bad leg %#x carries md bad-block ranges, and no " +
		"other member in sync is clean of them"
	waitsTakeBackKeeper = "new leg %#x, taken from a retired leg, waits for " +
		"a member that keeps the data to compare its superblock with"
	waitsNewLegHeld = "md still holds new leg %#x without a place"
	waitsBadLegHeld = "md still holds bad leg %#x"
)

// newLegWaits is the wait of a new leg the array lacks and the step cannot
// add: its side is not provisioned yet, or it is not available otherwise.
func newLegWaits(lp *legPlan) string {
	if lp.provisioning {
		return fmt.Sprintf(waitsNewLegUnprovisioned, lp.legId)
	}
	return fmt.Sprintf(waitsNewLegUnavailable, lp.legId)
}

type repBadLeg struct {
	lp   *legPlan
	fail bool
}

// repSteps is CN12's add of the group's repair's new legs, decided from one
// reading of the array, the members held after the pass's earlier removes:
// the converge runs it with the leg's availability (avail) and the put-back's
// test (takes, legTakesMember), and the read's verdict with the parts of them
// a read can test, so the verdict names the new legs the converge would add.
// A STARTED repair's new leg the array lacks is added once available, as a
// leg_list member is. A CREATED repair with no abort flag in the request adds
// its new legs that the array lacks and that are available — a provisioning
// leg never is — only while another member keeps the data: one md marks in
// sync, of a leg_list leg other than a bad leg, on a leg that can take a
// member (keepsData). Then each bad leg md still holds goes out first: one
// out of its place is removed; one in its place that cannot take a member is
// failed and removed, and so is one whose member carries md bad-block ranges
// (CN12, The md check) while another leg_list member md holds in sync
// carries none (cleanKeeper); any other in its place, serving with an OK
// row, stays, and the repair waits, so the step does nothing at all.
// So this step fails the bad leg only in a pass whose add's conditions
// hold, so that the new leg can follow at once; a remove or an add that md
// refuses still leaves the new leg out of that pass (cnagent.md CN12). A
// bad leg whose leg is dead is failed earlier, and stays in the array, by
// the early fail (deadMembers). Any other
// repair adds nothing. Both callers then put the step through CN12's
// take-back check (takeBackStep), which reads the members' superblocks and so
// stays out of this decision from one reading of sysfs.
func repSteps(
	gp *grpPlan,
	held map[string]MdMember,
	avail, takes, sources func(*legPlan) bool,
) repStep {
	rp := gp.rep
	started := rp != nil &&
		rp.rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED
	if rp == nil || !(started || rp.mayAdd()) {
		return repStep{}
	}
	var step repStep
	for _, lp := range rp.newLegs {
		if _, ok := held[lp.name]; ok {
			continue
		}
		if avail(lp) {
			step.add = append(step.add, lp)
		} else if step.waits == "" {
			step.waits = newLegWaits(lp)
		}
	}
	if len(step.add) == 0 {
		return repStep{waits: step.waits}
	}
	if started {
		// A STARTED repair's new leg joins as a leg_list member does, once
		// another member can feed its copy (hasCopySource).
		var add []*legPlan
		for _, lp := range step.add {
			if hasCopySource(gp, held, sources, lp.name) {
				add = append(add, lp)
			} else if step.waits == "" {
				step.waits = fmt.Sprintf(waitsNoCopySource, lp.legId)
			}
		}
		return repStep{add: add, waits: step.waits}
	}
	if !keepsData(gp, held, takes) {
		return repStep{waits: waitsNoKeeper}
	}
	for _, lp := range gp.legs {
		member, ok := held[lp.name]
		if !ok || !rp.badLegIds[lp.legId] {
			continue
		}
		switch {
		case member.Slot == mdSlotNone:
			step.out = append(step.out, repBadLeg{lp: lp})
		case takes(lp) && member.BadBlocks == 0:
			return repStep{waits: fmt.Sprintf(waitsBadLegServes, lp.legId)}
		case takes(lp) && !cleanKeeper(gp, held):
			return repStep{waits: fmt.Sprintf(waitsNoCleanKeeper, lp.legId)}
		default:
			step.out = append(step.out, repBadLeg{lp: lp, fail: true})
		}
	}
	return step
}

// cleanKeeper says whether md holds in sync a member of the group's leg_list,
// other than a bad leg of its repair, that carries no bad-block range: the
// copy of a repair that replaces a leg for its ranges reads from such a
// member, so the ranges do not come back on the new leg (CN12, The md
// check).
func cleanKeeper(gp *grpPlan, held map[string]MdMember) bool {
	for _, lp := range gp.legs {
		if gp.isBadLeg(lp.legId) {
			continue
		}
		if member, ok := held[lp.name]; ok && member.inSync() &&
			member.BadBlocks == 0 {
			return true
		}
	}
	return false
}

// deadMembers are the members CN12's member reconciliation fails early
// (cnagent.md CN12, The early fail): each leg_list member, an open repair's
// bad leg included, that md holds in its place and does not show faulty
// (MdMember.holdsPlace), whose leg is gone — not available this pass and its
// prober at deadLegRounds failed rounds in a row (CnAgentServer.deadLegs) —
// while another member md holds in sync is a copy source (hasCopySource): of
// a leg_list leg or an open repair's new leg, on a leg that serves and whose
// row does not read ERROR. md learns that a member is dead only from IO to
// it, so on an idle array it goes on counting a dead member in sync, and its
// survivor's superblock still records both: mdadm then refuses to start the
// survivor alone after a failover or a reboot. The fail writes the death
// into the survivor's superblock. The copy source is on a leg that serves,
// which a member this step takes is not, so the step never fails the member
// that keeps the data, nor leaves the array with no member in sync. gone and
// sources are the converge's or the read's view (CN30).
func deadMembers(
	gp *grpPlan,
	held map[string]MdMember,
	gone func(*legPlan) bool,
	sources func(*legPlan) bool,
) []*legPlan {
	var out []*legPlan
	for _, lp := range gp.legs {
		member, ok := held[lp.name]
		if ok && member.holdsPlace() && gone(lp) &&
			hasCopySource(gp, held, sources, lp.name) {
			out = append(out, lp)
		}
	}
	return out
}

// hasCopySource reports whether a member other than name can feed the copy
// md starts when name joins the array (CN12): one the array holds, md marks
// in sync (MdMember.inSync), of a leg_list leg or an open repair's new leg,
// on a leg that can be a source (sources, copySourceLeg). md learns that a
// member is dead only from IO to it, so on an idle array a dead member still
// reads in sync, and a copy that reads it breaks the array. It is the
// in-sync guard's test (inSyncGuard) with the joining member left out, an
// open repair's new legs counted and a source leg whose row reads ERROR left
// out; a repair's bad leg counts, since nothing leaves the array when a
// member joins. The put-back, the add of a member the array lacks and a
// STARTED repair's add all wait for it; a member left out is no error, and
// the read's verdict brings the converge that adds it once a source serves
// again.
func hasCopySource(
	gp *grpPlan,
	held map[string]MdMember,
	sources func(*legPlan) bool,
	name string,
) bool {
	source := func(lp *legPlan) bool {
		member, ok := held[lp.name]
		return lp.name != name && ok && member.inSync() && sources(lp)
	}
	for _, lp := range gp.legs {
		if source(lp) {
			return true
		}
	}
	if rp := gp.openRep(); rp != nil {
		for _, lp := range rp.newLegs {
			if source(lp) {
				return true
			}
		}
	}
	return false
}

// copySourceLeg is CN12's test of a leg whose member can feed a copy: the leg
// serves this pass (serves: the converge's availability, a read's
// legServes), and its row does not read ERROR. A row that reads PENDING, as
// before a new primary's first probe, does not hold a source back; one that
// reads ERROR — a leg whose probe IO failed, a disk returning errors — does,
// since a copy that reads errors from the last in-sync member makes md mark
// the array broken (CN29).
func copySourceLeg(serves bool, legRow *pb.ResInfo) bool {
	return serves && legRow.GetStatus() != pb.ResStatus_RES_STATUS_ERROR
}

// keepsData reports whether a member keeps the data while a repair's bad leg
// leaves the array (repSteps): a leg_list member other than a bad leg of the
// group's repair, which md marks in sync (MdMember.inSync), on a leg that
// can take a member (takes). It is the in-sync guard's test (inSyncGuard)
// with the leg row besides: md learns that a member is dead only from IO to
// it, so on an idle array a dead member still reads in sync, and a copy onto
// the new leg that reads a dead member breaks the array.
func keepsData(
	gp *grpPlan,
	held map[string]MdMember,
	takes func(*legPlan) bool,
) bool {
	return dataKeeper(gp, held, takes) != nil
}

// dataKeeper is the first leg, in leg_list order, whose member keeps the
// data (keepsData), nil when none does: the member whose superblock CN12's
// take-back check compares a new leg's with (takeBackStep).
func dataKeeper(
	gp *grpPlan,
	held map[string]MdMember,
	takes func(*legPlan) bool,
) *legPlan {
	for _, lp := range gp.legs {
		if gp.rep != nil && gp.rep.badLegIds[lp.legId] {
			continue
		}
		if member, ok := held[lp.name]; ok && member.inSync() && takes(lp) {
			return lp
		}
	}
	return nil
}

// takeBackStep is CN12's take-back check on a repair's step (repSteps), the
// converge's and the read's alike: a new leg taken from a retired leg
// (repPlan.takenBack) is added only while its superblock carries the
// identity of the member that keeps the data (dataKeeper) and a change count
// that is not above that member's, both read from their superblock areas
// (ReadMember). A leg of another identity is of an older history, from
// before a forced start, and can hold data the array lacks; a leg whose
// count is above holds newer data than the array; md's copy overwrites
// either however mdadm takes the add, so its add is refused, the first
// unless the repair carries the operator's force (repPlan.force): it leaves
// the step's adds, and the refusal, naming it (takeBackRefusal), is what the
// repair's verdict carries (repairVerdict), the first refused leg's should
// there be more than one. A new leg that carries no superblock is copied
// whole and never refused. One that carries one while no member keeps the
// data, as a STARTED repair's add can meet, waits with no refusal, since
// there is nothing to compare it with; so does one whose keeper reads no
// superblock. A step left with no add is no step at all, so a refused add
// never takes the bad leg out (repSteps). Nothing is kept: each converge and
// each read checks again.
//
// takes is the caller's test of a leg that can take a member, which the
// keeper passed, and the new leg passed the caller's availability test to be
// in the step, so only a leg that serves is read. A reading whose read
// failed or did not answer is the error, with no step and no refusal.
func (s *CnAgentServer) takeBackStep(
	ctx context.Context,
	gp *grpPlan,
	held map[string]MdMember,
	takes func(*legPlan) bool,
	step repStep,
) (repStep, string, error) {
	if len(step.add) == 0 {
		return step, "", nil
	}
	var add []*legPlan
	refusal, waits := "", step.waits
	keeperWaits := func(lp *legPlan) {
		if waits == "" {
			waits = fmt.Sprintf(waitsTakeBackKeeper, lp.legId)
		}
	}
	for _, lp := range step.add {
		if !gp.rep.takenBack(lp.legId) {
			add = append(add, lp)
			continue
		}
		reading := s.md.ReadMember(ctx, lp.path)
		if reading.Err != nil {
			return repStep{}, "", reading.Err
		}
		sb := reading.Sb
		if sb == nil {
			add = append(add, lp)
			continue
		}
		keeper := dataKeeper(gp, held, takes)
		if keeper == nil {
			keeperWaits(lp)
			continue
		}
		keeperReading := s.md.ReadMember(ctx, keeper.path)
		if keeperReading.Err != nil {
			return repStep{}, "", keeperReading.Err
		}
		keeperSb := keeperReading.Sb
		if keeperSb == nil {
			keeperWaits(lp)
			continue
		}
		if why := takeBackRefusal(gp, lp, sb, keeper,
			keeperSb); why != "" {
			if refusal == "" {
				refusal = why
			}
			continue
		}
		add = append(add, lp)
	}
	if len(add) == 0 {
		return repStep{waits: waits}, refusal, nil
	}
	step.add, step.waits = add, waits
	return step, refusal, nil
}

// heldWaits is the wait of a verdict that says neither started nor abort
// confirmed while the step holds nothing back (repairVerdict): for a repair
// whose abort flag the request carries, the first new leg md holds without
// a place, which keeps the abort from being confirmed; and for an open
// repair whose every new leg holds a place, the first bad leg md still
// holds, which keeps the verdict from saying started (startedBadLegs). ""
// when neither holds.
func heldWaits(gp *grpPlan, held map[string]MdMember) string {
	rp := gp.rep
	if rp.aborting() {
		for _, lp := range rp.newLegs {
			if member, ok := held[lp.name]; ok && !member.holdsPlace() {
				return fmt.Sprintf(waitsNewLegHeld, lp.legId)
			}
		}
	}
	if !rp.open() || len(rp.newLegs) == 0 {
		return ""
	}
	for _, lp := range rp.newLegs {
		if member, ok := held[lp.name]; !ok || !member.holdsPlace() {
			return ""
		}
	}
	for _, lp := range gp.legs {
		if _, ok := held[lp.name]; ok && rp.badLegIds[lp.legId] {
			return fmt.Sprintf(waitsBadLegHeld, lp.legId)
		}
	}
	return ""
}

// takeBackRefusal is the text of CN12's take-back refusal (takeBackStep),
// one text for the converge and the read, "" for a leg the check lets in:
// the refused new leg and the leg that keeps the data, with the new leg's
// identity beside the array's, or its change count. The keeper's own count
// stays out of it: md raises it as the array runs, so a text that carried it
// would differ from read to read while a refusal stands, the check stream
// would send the cntlr's info on every round (dnagent.md SH26), and the
// converge and the read would not report the same refusal (CN28); the new
// leg's count does not move, since nothing writes a leg md does not hold.
// The repair's force lets a leg of another identity in, never one of the
// array's own history with a count above.
func takeBackRefusal(
	gp *grpPlan,
	lp *legPlan,
	sb *mdSuperblock,
	keeper *legPlan,
	keeperSb *mdSuperblock,
) string {
	if sb.Uuid != keeperSb.Uuid {
		if gp.rep.force() {
			return ""
		}
		return fmt.Sprintf("leg %#x taken from a retired leg carries "+
			"another array's superblock, %s, not the %s of leg %#x: it may "+
			"hold data the array lacks", lp.legId, sb.Uuid, keeperSb.Uuid,
			keeper.legId)
	}
	if sb.Events > keeperSb.Events {
		return fmt.Sprintf("leg %#x taken from a retired leg holds newer "+
			"data than the array: its md events count %d is above that "+
			"of leg %#x", lp.legId, sb.Events, keeper.legId)
	}
	return ""
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
// dm name of the first member the array holds that the group does not keep
// (grpPlan.memberNames: neither in leg_list nor a new leg of an open
// repair, such as an aborted repair's new leg), when no member that stays in
// leg_list is in sync (MdMember.inSync) on a leg that serves; "" when no
// member leaves, or when such a member keeps the data. serves is the leg's
// availability as the caller can read it: the converge's available map, and
// for a read legServes, the part of it a read can see, so the two report the
// same row short of what legServes does not read (cnagent.md, Known limits).
// serves is called only for a held, in-sync member of leg_list, and only
// once a member leaves, so a read of a group whose array holds no such
// member makes no lookup for it.
//
// md answers a fail of the last in-sync member of a mirror by marking the
// whole array broken (CN29), so a fail of a member the group does not keep
// needs another to keep the data. That member must be one md marks in sync
// on an available leg, because on an array with no IO md still marks a dead
// member in sync: the fail would go through, the add after it would start a
// copy that reads the dead member, and that read breaks the array. And it must
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
	wanted := make(map[string]bool, len(gp.legs)+1)
	for _, name := range gp.memberNames() {
		wanted[name] = true
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
	return inSyncGuardError{fmt.Sprintf("member %s left the group but is "+
		"not failed: no other member of %s is in sync on an available leg "+
		"of leg_list", member, dev)}
}

// inSyncGuardError is the group's error while the in-sync guard holds
// (errInSyncGuard). Over it the build steps no md check (ensureGroup), as a
// read, whose row of the group reads the same error, names none (CN12, The
// md check): the members md shows in sync there may be on legs that do not
// serve, so a check would read the data where only the leaving member holds
// it.
type inSyncGuardError struct{ msg string }

func (e inSyncGuardError) Error() string { return e.msg }

// putBackNames is what a cntlr's verdict names beside its leftovers for one
// md group (cnagent.md CN30): the dm name of each member the converge's
// member reconciliation would fail early (deadMembers, with dead, the read's
// CnAgentServer.deadLegs, and legServes for the leg's availability), then of
// each member it would put back or add (CN12) — a leg_list member
// other than an open repair's bad leg that md has failed and moved out of its
// slot, or that the running array lacks, on a leg that can take it, beside a
// copy source (hasCopySource) — then
// each new leg of a CREATED repair whose abort flag the request carries,
// and each bad leg of a repair that has started in md, that the converge
// would remove (abortRemovals, startedBadLegs), and each new leg it would add,
// after the put-back's removes, under the repair's conditions (repSteps).
// detail is the group's running array as its row read it
// OK, nil for any other row, the in-sync guard's ERROR among them
// (probeGroup); legRows are the call's leg rows. legServes is what a
// call can read of a leg's availability without building anything, so no
// syncup is brought to put back or add a member on a leg whose path is not
// live and optimized. A repair's removals are named whatever the leg's
// path, since md removes a member that holds no place with no IO to it. A
// step only the converge takes, the connect of a provisioned side that has
// no controller, can still fail, and the member is then named again by
// every read, and each round that answers from such a read brings the
// SyncupCntlr again (cnagent.md, Known limits). A converge's code never
// names it (convergeCntlr), so the round that answers from the re-send's
// observation brings none for it. Except for the early fail's members and
// the repair's removals above (deadMembers; abortRemovals, startedBadLegs),
// a member that still holds its slot, one being recovered, or one md holds
// without a place and has not failed is never named, nor a new leg whose add
// waits for its bad leg.
//
// The new legs it would add pass the converge's take-back check first
// (takeBackStep), whose superblock reads of a new leg taken from a retired
// leg and of the member that keeps the data a read makes as the converge
// does, so
// the verdict never names a leg whose add the converge would refuse, which
// would bring a SyncupCntlr on every round, and the read reports the same
// refusal, returned here for the repair's verdict, once the new leg's own
// leg row reads OK (takes), where the converge makes the check on an
// available leg whatever its row reads (cnagent.md CN28, CN30). A superblock
// read that did not answer names no new leg and reports no refusal; the
// next read reads again. A leg_list member the array lacks passes the
// identity check (legListAddRefusal) the same way, whose refusal the group's
// reading carries.
func (s *CnAgentServer) putBackNames(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	detail *MdDetail,
	legRows map[uint64]*pb.ResInfo,
	dead map[uint64]bool,
) ([]string, grpRefusals) {
	if detail == nil {
		return nil, grpRefusals{}
	}
	held := heldMembers(detail)
	// serves is legServes, once per leg; takes is legTakesMember as a read
	// can test it: the leg row first, and only then the leg's serves.
	servesSeen := make(map[uint64]bool)
	serves := func(lp *legPlan) bool {
		ok, seen := servesSeen[lp.legId]
		if !seen {
			ok = s.legServes(ctx, plan, lp)
			servesSeen[lp.legId] = ok
		}
		return ok
	}
	takes := func(lp *legPlan) bool {
		return legRows[lp.legId].GetStatus() ==
			pb.ResStatus_RES_STATUS_OK && serves(lp)
	}
	// sources is copySourceLeg as a read can test it: the leg row first, and
	// only then the leg's serves.
	sources := func(lp *legPlan) bool {
		return legRows[lp.legId].GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR && serves(lp)
	}
	var names []string
	var refusals grpRefusals
	// The early fail's members (deadMembers), by the read's view of a leg
	// that serves and of a copy source. No later step reads what md would
	// show of them after the fail: each is on a leg that does not serve.
	for _, lp := range deadMembers(gp, held, func(lp *legPlan) bool {
		return dead[lp.legId] && !serves(lp)
	}, sources) {
		names = append(names, lp.name)
	}
	arrayUuid := s.arrayUuidOnce(detail)
	for _, lp := range gp.legs {
		if gp.isBadLeg(lp.legId) {
			continue
		}
		if member, ok := held[lp.name]; ok && !member.failedOutOfSlot() {
			continue
		}
		if !takes(lp) || !hasCopySource(gp, held, sources, lp.name) {
			continue
		}
		// The converge's add takes the member's superblock read and makes
		// the identity check, a member it puts back included, which its
		// put-back has removed by then; the read checks both alike.
		why, err := s.legListAddRefusal(ctx, gp, lp, arrayUuid)
		if err != nil {
			continue
		}
		if why != "" {
			if refusals.grp == "" {
				refusals.grp = why
			}
			refusals.otherIdentity = append(refusals.otherIdentity, lp.legId)
			continue
		}
		names = append(names, lp.name)
	}
	for _, lp := range append(abortRemovals(gp, held),
		startedBadLegs(gp, held)...) {
		names = append(names, lp.name)
	}
	for _, lp := range putBackLegs(gp, held, takes, sources) {
		delete(held, lp.name)
	}
	step, rep, err := s.takeBackStep(ctx, gp, held, takes,
		repSteps(gp, held, takes, takes, sources))
	if err != nil {
		return names, refusals
	}
	for _, lp := range step.add {
		names = append(names, lp.name)
	}
	refusals.rep, refusals.waits = rep, step.waits
	return names, refusals
}

// legServes is the part of a leg's availability (CN12) that a call can read
// without building anything, which putBackNames tests, and
// readAvailable with the read's lookup: its subsystem, looked
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

// probeGroup is the read-only CN28 view of a group: for RedundMdRaid1 the
// array that holds any of its members (grpPlan.memberNames), and a
// running array (degraded included) is OK with its mdStateLine in details,
// read from sysfs alone (Md.Detail) — a dead member of an array that keeps
// another in-sync member is the leg row's to report, never by itself a
// reason for this row to block or fail, and one md has failed on a leg that
// serves again is the verdict's to name (putBackNames); an error md charges
// to the last in-sync member marks the array broken instead of failing the
// member, and an array_state of "broken" is not running and reads ERROR
// (CN29); for RedundNone the dm table decides. A running array holding a
// member the group does not keep, which the converge would not fail, reads
// ERROR with the converge's own text (inSyncGuard), so a read and a converge
// report the same row. The array it read OK is returned too, for the
// verdict, and nil whenever the row is not that: the verdict names no member
// of a group the guard holds, whose converge would refuse again, so no
// syncup comes for it on every round. The fourth value is the group's
// array as the read's md walk matched it, Exists false when there is none,
// for the verdict's names of a broken or inactive array (brokenArray,
// reassemblyNamed),
// of an absent one (absentArrayNamed) and of a forced start
// (forcedStartNamed); it is nil for a RedundNone group and when the walk or
// the array's read did not answer.
//
// Like ensureGroup it is only ever reached for a non-deferred group ([D15]), and
// like ensureGroup it states the leg-count invariant rather than trusting the
// caller with an unguarded gp.legs[0].
func (s *CnAgentServer) probeGroup(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
) (pb.ResStatus, string, *MdDetail, *MdDetail) {
	if !gp.raid1 {
		if len(gp.legs) != 1 {
			return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
				"RedundNone group has %d legs, want 1", len(gp.legs)), nil, nil
		}
		status, details := s.probeDmTarget(ctx, plan.rd, gp.dmName, "linear",
			gp.dataSectors, gp.legs[0].name, gp.dataOffsetSectors)
		return status, details, nil, nil
	}
	walk, err := plan.mdWalks.get(ctx)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), nil, nil
	}
	detail, err := s.md.Detail(ctx, walk, gp.memberNames())
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), nil, nil
	}
	if !detail.Exists || detail.State == "clear" {
		return pb.ResStatus_RES_STATUS_MISSING, "", nil, detail
	}
	if detail.Foreign {
		return pb.ResStatus_RES_STATUS_ERROR, detail.foreignErr().Error(),
			nil, detail
	}
	if !mdRunning(detail.State) {
		return pb.ResStatus_RES_STATUS_ERROR, detail.State, nil, detail
	}
	if leaver := inSyncGuard(gp, detail, func(lp *legPlan) bool {
		return s.legServes(ctx, plan, lp)
	}); leaver != "" {
		return pb.ResStatus_RES_STATUS_ERROR,
			errInSyncGuard(leaver, detail.Dev).Error(), nil, detail
	}
	return pb.ResStatus_RES_STATUS_OK, mdStateLine(detail), detail, detail
}

// brokenArray is found, a group's array as a read's md walk matched it
// (probeGroup), when it reads broken or inactive and holds no member that is
// not a dm device, for the verdict's re-assembly name (reassemblyNamed); nil
// otherwise.
func brokenArray(found *MdDetail) *MdDetail {
	if found == nil || !found.Exists || found.Foreign ||
		(found.State != mdStateBroken && found.State != mdStateInactive) {
		return nil
	}
	return found
}

// reportGrpReading writes, beside a group's rows, what the primary reads of
// the group's running array (cnagent.md CN28): its reading, whether the
// array is whole and which legs md holds in sync, and the verdict on the
// group's repair (repairVerdict). detail is the array as the group's row read
// it OK — a converge's after the pass's own member changes (ensureGroup) — and
// nil for any other row: an absent, stopped or broken array, the in-sync
// guard's ERROR and a RedundNone group give neither, since that a new leg is
// no member of an array that does not run proves nothing about whether md
// wrote to it (architecture.md, Repair). A group that builds no array at all
// — a standby's, one the level suppresses, a deferred one — never gets here.
//
// Whole is md/degraded at zero: md counts a place with no member, a member
// it has failed in its place and one it copies onto, and not a member
// without a place. The legs held in sync are the group's leg_list legs and
// its repair's new legs whose members md marks in sync (MdMember.inSync), in
// ascending leg id, so a reading that does not change reads the same, each
// with its md slot, which the sp worker's fence order reads (dnv-worker.md
// RW24).
// refusal is the take-back check's (takeBackStep) for the verdict, which a
// converge's member reconciliation and a read's putBackNames return. acted
// is the revision of the group's mark whose forced start this incarnation
// ran (actedMark), zero for none, which the reading carries for the sp
// worker's clear (dnv-worker.md AR13).
func reportGrpReading(
	info *pb.CntlrInfo,
	gp *grpPlan,
	detail *MdDetail,
	refusals grpRefusals,
	acted uint64,
	mdDone uint64,
) {
	if detail == nil {
		return
	}
	held := heldMembers(detail)
	reading := &pb.CntlrInfo_GrpInfo{
		Whole:               detail.Degraded == 0,
		Refusal:             agent.CutAgentText(refusals.grp),
		ForcedStartRevision: acted,
		MdCheckDoneEpoch:    mdDone,
	}
	legs := gp.legs
	if gp.rep != nil {
		legs = append(slices.Clone(gp.legs), gp.rep.newLegs...)
	}
	for _, lp := range legs {
		member, ok := held[lp.name]
		if ok && member.inSync() {
			reading.InSyncLegIdList = append(reading.InSyncLegIdList, lp.legId)
			// The slot of an in-sync member is a place number; one that does
			// not parse is left out, and the sp worker orders no fence of the
			// group by it (dnv-worker.md RW24).
			if slot, err := strconv.ParseUint(member.Slot, 10, 32); err == nil {
				if reading.InSyncLegIdToSlot == nil {
					reading.InSyncLegIdToSlot = make(map[uint64]uint32)
				}
				reading.InSyncLegIdToSlot[lp.legId] = uint32(slot)
			}
		}
		if ok && member.BadBlocks > 0 {
			if reading.LegIdToBadBlocks == nil {
				reading.LegIdToBadBlocks = make(map[uint64]uint32)
			}
			reading.LegIdToBadBlocks[lp.legId] = uint32(member.BadBlocks)
		}
	}
	slices.Sort(reading.InSyncLegIdList)
	info.GrpIdToGrpInfo[gp.grpId] = reading
	if gp.rep != nil {
		info.RepIdToRepairInfo[gp.rep.repId] = repairVerdict(
			gp, held, refusals.rep, refusals.waits)
	}
}

// repairVerdict is the primary's verdict on the group's repair from the
// members its running array holds (CN12; architecture.md, Repair): started,
// for an open repair, when every new leg holds a place (MdMember.holdsPlace),
// copying or in sync, and md holds nothing on a bad leg; abort confirmed, for
// a CREATED repair whose abort flag is in the cntlr's own request, when md
// holds nothing on a new leg, the flag tying the confirmation to the flag the
// sp worker set. The two never hold together, and an aborted repair has
// neither. held_new_leg_id_list names, ascending, every new leg md holds a
// member on in any state (MdMember, held at all): md has written its
// superblock onto it, which the sp worker records as a taken-back leg's
// partial mark (dnv-worker.md AR11). A repair with no new leg is never
// started. refusal is the take-back check's refusal of the new leg's add
// (takeBackStep), "" when
// nothing holds it back: text for people, which the sp worker logs while
// the repair waits (dnv-worker.md AR11) and no rule decides anything on, and
// never an error on the group's row, which would fail the primary over for a
// refusal the next primary makes again. waits is why the repair's step
// waits (repStep.waits), "" for a step that holds nothing back; for a
// verdict that neither starts nor confirms an abort, with no such text, it
// is what md still holds that keeps the verdict back (heldWaits) (CN12, Why
// a repair waits).
func repairVerdict(
	gp *grpPlan,
	held map[string]MdMember,
	refusal string,
	waits string,
) *pb.CntlrInfo_RepairInfo {
	rp := gp.rep
	started := rp.open() && len(rp.newLegs) > 0
	confirmed := rp.aborting()
	var heldNew []uint64
	for _, lp := range rp.newLegs {
		member, ok := held[lp.name]
		if !ok || !member.holdsPlace() {
			started = false
		}
		if ok {
			confirmed = false
			heldNew = append(heldNew, lp.legId)
		}
	}
	for _, lp := range gp.legs {
		if _, ok := held[lp.name]; ok && rp.badLegIds[lp.legId] {
			started = false
		}
	}
	slices.Sort(heldNew)
	if !started && !confirmed && waits == "" {
		waits = heldWaits(gp, held)
	}
	return &pb.CntlrInfo_RepairInfo{
		Started:          started,
		AbortConfirmed:   confirmed,
		Refusal:          agent.CutAgentText(refusal),
		HeldNewLegIdList: heldNew,
		Waits:            agent.CutAgentText(waits),
	}
}
