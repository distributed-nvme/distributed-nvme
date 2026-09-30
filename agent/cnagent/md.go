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

// Md wraps the mdadm patterns of Appendix A. Only the cn role runs mdadm, so
// by the dnagent.md §1 split rule this wrapper is role code.
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
// Everything the sweep, CN12's member reconciliation and CN28's md row need
// about an array they read from sysfs, which never touches a member device
// and therefore never blocks on a dead leg:
//
//   - `ls /sys/block` names every array node, so a sweep finds arrays it has
//     no plan for — the whole point of deriving removal from the live system;
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
//     `dev-*/state` is read but not yet acted on (cnagent.md §7, known
//     limits). CN12's member reconciliation compares members by dm name
//     alone.
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
// mdadm.conf says `CREATE names=yes` (architecture.md §4.3 sizes CnMdDevName
// for that node). The /dev/md/<name> entries themselves are udev symlinks
// and never appear here. "mdN" below stands for either spelling.
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
		mdDir := sysfsBlockDir + "/" + entry + "/md"
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
		array := MdArray{
			Dev:   "/dev/" + entry,
			Name:  entry,
			State: state,
		}
		for _, member := range members {
			if member.dmName == "" {
				array.Foreign = true
				continue
			}
			array.Members = append(array.Members, member.dmName)
		}
		out = append(out, array)
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
	// never the string for equality.
	State string
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
// directories, each with the dm name it carries. Listing a directory is a
// command (`ls`), so the walk is what finding a group's array costs; Detail
// matches a group against a walk and reads only the matched array's
// attributes, which lets a pass over g groups list each of the node's a
// arrays once instead of g times — at 32 slices, 64 groups over 64 arrays,
// 65 listings a round instead of 4160.
//
// The walk reads every array on the node, other sps' included, so an array
// whose md/ listing or a member's dm-name read did not answer — killed at the
// soft timeout, held past it on the agent's OsClient semaphore, which every
// cntlr it serves shares, ctx cancelled, or an errno other than ENOENT — is
// recorded as unanswered rather than failing the walk: failing this pass for
// another sp's array would turn this sp's md rows ERROR, and an md row counts
// toward cntlr health, which is exactly the fault a failover cannot fix.
// Detail decides what an unanswered array may mean for one group. An array
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
		members, ok, err := m.memberDirs(
			ctx, sysfsBlockDir+"/"+node+"/md")
		if err != nil {
			delete(w.arrays, node)
			w.unanswered[node] = err
			continue
		}
		delete(w.unanswered, node)
		if !ok {
			delete(w.arrays, node)
			continue
		}
		w.arrays[node] = members
	}
	return nil
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
// dmNames — a group's leg wrapper names — and reads its state from sysfs. It
// never runs mdadm and never resolves /dev/md/<name>.
//
// `mdadm --detail` used to answer this, and it left the path because it
// opens a member device: it loads the superblock from the first member that
// opens, and when that member's DN side has gone the read sits in the
// multipath head's requeue list until the path's failfast expires — up to
// ~13 s after the side died (the keep-alive timeout, error recovery, then
// fast_io_fail_tmo), far past the soft timeout. The kill turned the group's
// md row ERROR for a member fault the leg row already reports, and an md row
// counts toward cntlr health: that was the trigger of the failover ping-pong
// found 2026-09-24. And before that, a killed `--detail` read as "absent"
// let a teardown skip `mdadm --stop` and leak the array's leg wrappers.
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
			return nil, fmt.Errorf(
				"arrays /dev/%s and /dev/%s both hold legs of the group",
				found, node)
		}
		found = node
	}
	if found != "" {
		detail, err := m.readDetail(ctx, "/dev/"+found,
			sysfsBlockDir+"/"+found+"/md", w.arrays[found])
		if err != nil || detail.Exists {
			return detail, err
		}
		// The match stopped between the walk and this read: absent, under
		// the same rule as no match at all.
	}
	for _, node := range sortedKeys(w.unanswered) {
		return nil, fmt.Errorf(
			"no array holds legs of the group, but /dev/%s did not "+
				"answer: %w", node, w.unanswered[node])
	}
	return &MdDetail{}, nil
}

// mdWalkOnce hands every group of one pass the same walk, taken at the first
// group that needs it — a pass with no md group lists nothing.
type mdWalkOnce struct {
	md   *Md
	walk *MdWalk
	err  error
	done bool
}

func (o *mdWalkOnce) get(ctx context.Context) (*MdWalk, error) {
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
		detail.Members = append(detail.Members, MdMember{
			DevNo:  devNo,
			DmName: member.dmName,
			State:  memberState,
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
// §11.1.1 "no superblock" case rather than an error.
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
// this answer (cnagent.md CN12, §7 known limits).
func (m *Md) HasSuperblock(ctx context.Context, dev string) (bool, error) {
	_, ok, err := m.cmd.RunProbe(ctx, "mdadm", "--examine", "--export", dev)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// MdCreateConf carries the §3.3 step 2 options every dnv array is created
// with: an internal write-intent bitmap, failfast members, and a data offset
// that clears the leg's meta region (§3.6).
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
// itself decides whether a degraded start is safe (§11.1.1 case 2), and a
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
// over its single leg's data region; a RedundMdRaid1 group instantiates the
// §11.1.1 assembly cases over the member leg wrappers. Spare legs are never
// members (§8.12) — they stay connected, wrapped and probed.
//
// A provisioning-deferred group never gets here: the build phase reports it
// PROVISIONING and skips it ([D15]). The leg-count check below is kept explicit
// all the same, so gp.legs[0] can never be indexed on an empty member list
// whatever a future caller does.
func (s *CnAgentServer) ensureGroup(
	ctx context.Context,
	gp *grpPlan,
	available map[uint64]bool,
	walks *mdWalkOnce,
) error {
	if !gp.raid1 {
		if len(gp.legs) != 1 {
			return fmt.Errorf(
				"RedundNone group has %d legs, want 1", len(gp.legs))
		}
		return s.ensureDmLinear(ctx, gp.dmName, gp.dataSectors,
			gp.legs[0].path, gp.dataOffsetSectors)
	}
	walk, err := walks.get(ctx)
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
	return s.reconcileMembers(ctx, gp, detail, available)
}

// assembleGroup is §11.1.1 case selection: probe each available member for an
// md superblock and either create the array or assemble it from the members
// that have one.
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
		return fmt.Errorf("no available leg")
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
		// probed. A side is never exported before the §9.4 whole-side zeroing
		// has written zeros over all of it and its `provisioned` gate has
		// opened ([D15]), the effective desired state defers any group whose
		// legs are still provisioning ([D15]), and ids are never reused — so a
		// superblock-free leg can only be a freshly zeroed side — short of a
		// read that failed after the path's failfast, which mdadm answers the
		// same way (HasSuperblock). But that argument covers the legs
		// actually examined. An unavailable member may be the one carrying
		// the group's data (its DN rebooting, its path mid-ANA-move), and
		// creating over the survivors would resync the data away. §11.1.1
		// puts "one leg available" in case 2, never in case 1.
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
		// superblock". A create would put a second array under the name the
		// pool's concat resolves. The guard errs only towards refusing, and a
		// false refusal needs a stale node of this very name: short of the
		// case above, a group whose legs carry no superblock has never had
		// an array that could have left one.
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
	// out is re-added by reconcileMembers (§11.1.1 cases 1.2/1.3/2).
	return s.md.Assemble(ctx, gp.devPath, gp.mdArrayName, withSuperblock)
}

// reconcileMembers covers SwitchSpareLeg with no extra mechanism (CN12): a
// member the array holds that is no longer in leg_list is failed and removed;
// an available leg_list member the array lacks is added, and md then resyncs —
// a bitmap catch-up for a briefly absent leg, a full rebuild for a promoted
// spare.
//
// The held set is the dm names sysfs gave the array's members
// (dev-*/block/dm/name), compared with the leg_list wrapper names — the key
// Detail matched the array by — and each held member is addressed by its dm
// path. Nothing here runs lsblk (amended 2026-09-26): the sets used to be
// keyed by device number, and an lsblk of a leg_list wrapper that did not
// answer read as "not wanted" and failed and removed that in-sync member. A
// leg whose wrapper this pass could not build is never available, so the add
// loop leaves it alone, and its member, if md still holds one, stays wanted.
// `mdadm --fail` and `--remove` open the member's path (open only, no IO) and
// then act on its device number. `--remove` can sleep in md's suspend for as
// long as a superblock write is stuck on the member — seconds after its side
// dies with a write in flight — and the soft timeout then kills it with the
// member still held: this pass fails before the add loop, so the promoted
// spare is not added either. The group's error is a row, not a reply code,
// and registers no background retry of its own. The retry finishes the switch
// only when the same pass registers it for something else (cnagent.md CN10's
// list), such as a leg that failed to converge or any leg_list member of this
// cntlr that is not available — build's late flag is cntlr-wide: the
// promoted spare if it is not available yet, or, after the switched-out leg's
// whole DN died, another group's leg still on it (AR8 switches one leg per
// pass); the switched-out member itself has left leg_list and never counts.
// Otherwise nothing schedules the converge that finishes the switch. The next
// converge of this cntlr, whatever brings it, removes the member in
// milliseconds and adds the spare — a spare that is not available yet is
// added by the first retry attempt that finds it available. Nothing here
// remembers the failure (cnagent.md §7, known limits).
func (s *CnAgentServer) reconcileMembers(
	ctx context.Context,
	gp *grpPlan,
	detail *MdDetail,
	available map[uint64]bool,
) error {
	// A Foreign array never gets here (ensureGroup), so every member has a
	// dm name.
	held := make(map[string]bool, len(detail.Members))
	for _, member := range detail.Members {
		held[member.DmName] = true
	}
	wanted := make(map[string]bool, len(gp.legs))
	for _, lp := range gp.legs {
		wanted[lp.name] = true
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
	for _, lp := range gp.legs {
		if held[lp.name] || !available[lp.legId] {
			continue
		}
		if err := s.md.Add(ctx, gp.devPath, lp.path); err != nil {
			return err
		}
	}
	return nil
}

// probeGroup is the read-only CN28 view of a group: for RedundMdRaid1 a
// running array (degraded included) is OK with its mdStateLine in details,
// read from sysfs alone (Md.Detail) — a dead member of an array that keeps
// another in-sync member is the leg row's to report, never a reason for this
// row to block or fail; an error md charges to the last in-sync member marks
// the array broken instead of failing the member, and an array_state of
// "broken" is not running and reads ERROR (CN29); for RedundNone the dm table
// decides.
//
// Like ensureGroup it is only ever reached for a non-deferred group ([D15]), and
// like ensureGroup it states the leg-count invariant rather than trusting the
// caller with an unguarded gp.legs[0].
func (s *CnAgentServer) probeGroup(
	ctx context.Context,
	gp *grpPlan,
	walks *mdWalkOnce,
) (pb.ResStatus, string) {
	if !gp.raid1 {
		if len(gp.legs) != 1 {
			return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
				"RedundNone group has %d legs, want 1", len(gp.legs))
		}
		return s.probeDmTarget(ctx, gp.dmName, "linear", gp.dataSectors,
			gp.legs[0].path, gp.dataOffsetSectors)
	}
	walk, err := walks.get(ctx)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	detail, err := s.md.Detail(ctx, walk, gp.legNames())
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !detail.Exists || detail.State == "clear" {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	if detail.Foreign {
		return pb.ResStatus_RES_STATUS_ERROR, detail.foreignErr().Error()
	}
	if !mdRunning(detail.State) {
		return pb.ResStatus_RES_STATUS_ERROR, detail.State
	}
	return pb.ResStatus_RES_STATUS_OK, mdStateLine(detail)
}
