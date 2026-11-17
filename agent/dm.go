package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// SectorSize is the device-mapper unit: every table size and offset is in
// 512-byte sectors.
const SectorSize = 512

// Dm wraps the device-mapper command patterns of
// dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`.
type Dm struct {
	osBase
}

func NewDm(oc common.OsClient) *Dm {
	return &Dm{osBase{oc: oc}}
}

// DmDevInfo is the `dmsetup info` view of one live device: its attr flags
// and, where the read asks for it, its number.
type DmDevInfo struct {
	Suspended bool
	ReadOnly  bool
	// Waiting says a table waits in the device's second slot, the one
	// dmsetup calls inactive: a reload's load went through and no resume
	// has made it live (Reload, Resume).
	Waiting bool
	// DevNo is the device's "major:minor", the form a dm table names a
	// device in. Only ListInfo and InfoDevNo read it; Info leaves it empty,
	// and so does a line whose numbers do not parse.
	DevNo string
}

// The four positions of the dm `attr` column (dmsetup(8): "(L)ive,
// (I)nactive, (s)uspended, (r)ead-only, read-(w)rite"), e.g. `L--w` for a
// live, resumed, writeable device and `L-sw` for a suspended one.
const (
	dmAttrWaitingIdx   = 1
	dmAttrSuspendedIdx = 2
	dmAttrReadOnlyIdx  = 3
)

// dmAttrInfo reads the flags of one attr column. A column too short to hold
// a flag reads as resumed and writeable, with no table waiting.
func dmAttrInfo(attr string) *DmDevInfo {
	info := &DmDevInfo{}
	if len(attr) > dmAttrWaitingIdx && attr[dmAttrWaitingIdx] == 'I' {
		info.Waiting = true
	}
	if len(attr) > dmAttrSuspendedIdx && attr[dmAttrSuspendedIdx] == 's' {
		info.Suspended = true
	}
	if len(attr) > dmAttrReadOnlyIdx && attr[dmAttrReadOnlyIdx] == 'r' {
		info.ReadOnly = true
	}
	return info
}

// Info returns the live state of a dm device, nil when it does not exist, and
// an error when `dmsetup info` did not answer. The third case is never folded
// into the second: a killed probe read as "gone" would free the extent record
// that still backs the device, and the next allocation would hand those
// extents out twice.
func (d *Dm) Info(ctx context.Context, name string) (*DmDevInfo, error) {
	stdout, ok, err := d.runProbe(ctx, "dmsetup", "info",
		"--columns", "--noheadings", "-o", "attr", name)
	if err != nil || !ok {
		return nil, err
	}
	return dmAttrInfo(strings.TrimSpace(stdout)), nil
}

// dmNoDevicesLine is what `dmsetup ls`, and each whole-node form of `dmsetup
// info` and `dmsetup table`, prints with exit status 0 on a node that holds
// no dm device at all.
const dmNoDevicesLine = "No devices found"

// dmLsDevNoPattern is the normalized "major:minor" a `dmsetup ls` line ends
// with. Both spellings the tool has used are normalized into it before the
// match: "(253:4)" and the older "(253, 4)".
var dmLsDevNoPattern = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// List enumerates every dm device on the node, name to "major:minor". It is
// the root of every sweep: what to remove is derived by subtracting the
// desired state from what actually exists, and nothing else on the node can
// name a device the agent has no plan for.
//
// `dmsetup ls` prints the literal "No devices found" and still exits 0 on an
// empty node; that one line is dropped by name. A non-zero exit, or a run
// that did not answer, is an error — a caller must never read either as "the
// node holds no dm devices".
//
// The device number can be absent or unparsable for an entry the listing
// named and the kernel has already dropped. Such an entry is KEPT, with an
// empty devno: a name is all a sweep needs to attribute and remove, and
// dropping it would hide from the sweep exactly the objects a concurrent
// teardown is in the middle of.
func (d *Dm) List(ctx context.Context) (map[string]string, error) {
	stdout, stderr, exitCode, err := d.run(ctx, "dmsetup", "ls")
	if err != nil {
		return nil, cmdError("dmsetup", []string{"ls"},
			stdout, stderr, exitCode, err)
	}
	out := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == dmNoDevicesLine {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		devNo := strings.Trim(strings.Join(fields[1:], ""), "()")
		devNo = strings.ReplaceAll(devNo, ",", ":")
		if !dmLsDevNoPattern.MatchString(devNo) {
			devNo = ""
		}
		out[fields[0]] = devNo
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Whole-node readers. A cntlr's background read takes every dm device of the
// node from two listings, one run each, instead of from reads per device
// (cnagent.md CN28; dnagent.md SH29): a listing of the whole node costs about
// what a read of one device costs. ListInfo gives each device's number and attr
// flags, ListTables its live table, and InfoDevNo reads one device the way
// ListInfo reads each.
// ---------------------------------------------------------------------------

// dmInfoArgs is the `dmsetup info` that prints one line per device, of every
// device or of the one named: "name/major/minor/attr", with no padding. "/"
// separates the fields because it is the one byte a dm name cannot hold, so
// the name needs no escape and the line splits into exactly four fields.
func dmInfoArgs(names ...string) []string {
	return append([]string{"info", "--columns", "--noheadings",
		"--separator", "/", "-o", "name,major,minor,attr"}, names...)
}

// DmInfoListing is one whole-node `dmsetup info` listing.
type DmInfoListing struct {
	// Devs is every device the listing names, by name. A device whose
	// numbers do not parse is kept, with an empty DevNo: as for List, the
	// name alone says the device is there.
	Devs map[string]*DmDevInfo
	// Partial says the listing can lack a device the node holds: the run
	// exited above zero, as dmsetup does when a device goes away during the
	// listing, or one of its lines did not parse.
	Partial bool
}

// ListInfo lists every dm device of the node with its number and attr flags,
// in one `dmsetup info` run, under the answer rule of the whole-node
// listings (dmListingAnswer).
func (d *Dm) ListInfo(ctx context.Context) (*DmInfoListing, error) {
	args := dmInfoArgs()
	stdout, stderr, exitCode, err := d.run(ctx, "dmsetup", args...)
	listing := &DmInfoListing{Devs: make(map[string]*DmDevInfo)}
	parsed := 0
	var unparsed []string
	for _, line := range strings.Split(stdout, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" ||
			trimmed == dmNoDevicesLine {
			continue
		}
		name, info, ok := parseDmInfoLine(line)
		if !ok {
			unparsed = append(unparsed, line)
			continue
		}
		listing.Devs[name] = info
		parsed++
	}
	partial, err := dmListingAnswer(
		args, stdout, stderr, exitCode, err, parsed, unparsed)
	if err != nil {
		return nil, err
	}
	listing.Partial = partial
	return listing, nil
}

// InfoDevNo is Info with the device's number: one `dmsetup info` of name, in
// ListInfo's line form. Like Info it returns nil, nil when the device does
// not exist and an error when the run did not answer. A run that exits 0
// and does not print the device's line first is an error too, never an
// absence. A line whose numbers do not parse gives the device with an empty
// DevNo, as ListInfo does; a caller that needs the number turns that into
// its own error.
func (d *Dm) InfoDevNo(ctx context.Context, name string) (*DmDevInfo, error) {
	args := dmInfoArgs(name)
	stdout, ok, err := d.runProbe(ctx, "dmsetup", args...)
	if err != nil || !ok {
		return nil, err
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		got, info, parsed := parseDmInfoLine(line)
		if !parsed || got != name {
			break
		}
		return info, nil
	}
	return nil, dmUnexpected(args, stdout)
}

// parseDmInfoLine reads one line of dmInfoArgs' output: the name, verbatim,
// then the major, the minor and the attr column. A line that does not split
// into those four fields, or names no device, does not parse.
func parseDmInfoLine(line string) (string, *DmDevInfo, bool) {
	fields := strings.Split(line, "/")
	if len(fields) != 4 || fields[0] == "" {
		return "", nil, false
	}
	info := dmAttrInfo(strings.TrimSpace(fields[3]))
	major := strings.TrimSpace(fields[1])
	minor := strings.TrimSpace(fields[2])
	_, majorErr := strconv.ParseUint(major, 10, 32)
	_, minorErr := strconv.ParseUint(minor, 10, 32)
	if majorErr == nil && minorErr == nil {
		info.DevNo = major + ":" + minor
	}
	return fields[0], info, true
}

// DmTableListing is one whole-node `dmsetup table --concise` listing.
type DmTableListing struct {
	// Tables is the live table of every device the listing names, by name;
	// a device with no live table holds an empty, non-nil slice.
	Tables map[string][]common.DmTarget
	// Partial says the listing can lack a device the node holds, as
	// DmInfoListing's does.
	Partial bool
}

// ListTables lists the live table of every dm device of the node in one
// `dmsetup table --concise` run, under the answer rule of the whole-node
// listings (dmListingAnswer). A record is "name,uuid,minor,flags" and then
// one field per table line, and parses when it holds those four fields and a
// name. Each table field is read as a `dmsetup table` line
// (common.ParseDmLines), so the space the tool can end a line with is no
// part of it, and an empty field adds no target. The numbers come from
// ListInfo, never from here.
func (d *Dm) ListTables(ctx context.Context) (*DmTableListing, error) {
	args := []string{"table", "--concise"}
	stdout, stderr, exitCode, err := d.run(ctx, "dmsetup", args...)
	listing := &DmTableListing{Tables: make(map[string][]common.DmTarget)}
	parsed := 0
	var unparsed []string
	if strings.TrimSpace(stdout) != dmNoDevicesLine {
		for _, record := range dmConciseRecords(stdout) {
			if len(record) == 1 && strings.TrimSpace(record[0]) == "" {
				continue
			}
			if len(record) < 4 || record[0] == "" {
				unparsed = append(unparsed, strings.Join(record, ","))
				continue
			}
			targets := []common.DmTarget{}
			for _, line := range record[4:] {
				targets = append(targets, common.ParseDmLines(line)...)
			}
			listing.Tables[record[0]] = targets
			parsed++
		}
	}
	partial, err := dmListingAnswer(
		args, stdout, stderr, exitCode, err, parsed, unparsed)
	if err != nil {
		return nil, err
	}
	listing.Partial = partial
	return listing, nil
}

// dmConciseRecords splits concise dmsetup output into records of fields. A
// backslash makes the next byte literal, which is how a name or a table line
// holds a ",", a ";" or a "\"; an unescaped "," ends a field, and an
// unescaped ";" or newline ends a field and its record.
func dmConciseRecords(out string) [][]string {
	var records [][]string
	var fields []string
	var field strings.Builder
	for i := 0; i < len(out); i++ {
		switch c := out[i]; c {
		case '\\':
			if i+1 < len(out) {
				i++
				field.WriteByte(out[i])
			}
		case ',':
			fields = append(fields, field.String())
			field.Reset()
		case ';', '\n':
			records = append(records, append(fields, field.String()))
			fields = nil
			field.Reset()
		default:
			field.WriteByte(c)
		}
	}
	return append(records, append(fields, field.String()))
}

// dmListingAnswer is the answer rule of the whole-node listings
// (osclient.md, RunCommand) over one run: parsed counts its lines that parse
// (its records, in the concise listing) and unparsed holds its other
// non-blank ones, in order. Lines that parse answer even when dmsetup exits
// above zero, which it does when a device goes away during the listing, and
// the listing is then partial; so is one with a line that does not parse. A
// run that did not report, or printed nothing that parses, did not answer,
// but for "No devices found" with exit status 0: an empty node. It returns
// whether the listing is partial, or the error of one that did not answer.
//
// That error never quotes the listing, which grows with the node and would
// be copied into every row the listing serves: it holds dmsetup's stderr, or
// the run's own error when stderr is empty, and for a run that exited 0 the
// first line it cannot read.
func dmListingAnswer(
	args []string,
	stdout string,
	stderr string,
	exitCode int,
	err error,
	parsed int,
	unparsed []string,
) (bool, error) {
	if reported(exitCode, err) && parsed > 0 {
		return err != nil || len(unparsed) > 0, nil
	}
	if err != nil {
		return false, cmdError("dmsetup", args, "", stderr, exitCode, err)
	}
	if strings.TrimSpace(stdout) == dmNoDevicesLine {
		return false, nil
	}
	if len(unparsed) == 0 {
		return false, fmt.Errorf("dmsetup %s: empty output",
			strings.Join(args, " "))
	}
	return false, fmt.Errorf("dmsetup %s: cannot read %q",
		strings.Join(args, " "), unparsed[0])
}

// dmUnexpected is the error of a dmsetup run that exited 0 with output its
// reader cannot read: the command line, then the output.
func dmUnexpected(args []string, stdout string) error {
	return fmt.Errorf("dmsetup %s: unexpected output %q",
		strings.Join(args, " "), stdout)
}

// Table returns the parsed live table of a dm device.
func (d *Dm) Table(
	ctx context.Context,
	name string,
) ([]common.DmTarget, error) {
	stdout, stderr, exitCode, err := d.run(ctx, "dmsetup", "table", name)
	if err != nil {
		return nil, cmdError("dmsetup", []string{"table", name},
			stdout, stderr, exitCode, err)
	}
	return common.ParseDmLines(stdout), nil
}

// WaitingTable returns the parsed table that waits in a device's second slot
// (`dmsetup table --inactive`), empty when none waits.
func (d *Dm) WaitingTable(
	ctx context.Context,
	name string,
) ([]common.DmTarget, error) {
	args := []string{"table", "--inactive", name}
	stdout, stderr, exitCode, err := d.run(ctx, "dmsetup", args...)
	if err != nil {
		return nil, cmdError("dmsetup", args, stdout, stderr, exitCode, err)
	}
	return common.ParseDmLines(stdout), nil
}

// Status returns the raw `dmsetup status` output (the dm-clone hydration
// line rides into ResInfo.details verbatim; architecture.md,
// Live-state reporting). Without the noflush flag the kernel commits the
// target's metadata before it answers (dnagent.md SH17); only
// CloneStatus's read of a whole copy wants that.
func (d *Dm) Status(ctx context.Context, name string) (string, error) {
	return d.status(ctx, "status", name)
}

// StatusNoFlush returns the raw `dmsetup status --noflush` output: the read
// of a thin pool's status, the first read of a dm-clone's (CloneStatus) and
// the reads inside a bitmap chunk's apply (cloneStatusNow), which with the
// flag commit nothing (dnagent.md SH17). It still waits
// behind the metadata write of a commit already running on that device.
func (d *Dm) StatusNoFlush(ctx context.Context, name string) (string, error) {
	return d.status(ctx, "status", "--noflush", name)
}

// CloneStatus returns the raw status line of a dm-clone: a `dmsetup status
// --noflush`, which commits nothing, so the read never waits on the flush of
// the copy's destination that a commit starts with, though it still waits
// behind the metadata write of a commit already running; and, when that
// line shows the copy whole, a `dmsetup status` with the flush, whose line
// it returns. The kernel commits a dm-clone's metadata about once a second
// on its own, and every commit flushes the destination first, so a line
// that says whole is only a reading of memory until a commit; the flushing
// read makes the copied regions and their record durable before any caller
// reports the copy whole (dnagent.md SH17). Once the copy is whole the
// metadata no longer changes, so that read commits at most the last
// transaction.
func (d *Dm) CloneStatus(ctx context.Context, name string) (string, error) {
	raw, err := d.StatusNoFlush(ctx, name)
	if err != nil {
		return "", err
	}
	st, ok := common.ParseCloneStatus(raw)
	if !ok || st.TotalRegions == 0 || st.HydratedRegions != st.TotalRegions {
		return raw, nil
	}
	return d.Status(ctx, name)
}

// status runs one `dmsetup status` with args and returns its trimmed output.
func (d *Dm) status(ctx context.Context, args ...string) (string, error) {
	stdout, stderr, exitCode, err := d.run(ctx, "dmsetup", args...)
	if err != nil {
		return "", cmdError("dmsetup", args, stdout, stderr, exitCode, err)
	}
	return strings.TrimSpace(stdout), nil
}

// Create builds a device from a single-target table. dnv never passes
// `--readonly`: nvmet opens a namespace's backing device read-write, and a
// read-only flag below the top of a stack is bypassed by dm remapping anyway,
// so no dnv device is ever created read-only ([D11]).
func (d *Dm) Create(
	ctx context.Context,
	name string,
	table string,
) error {
	return d.runOk(ctx, "dmsetup", "create", name, "--table", table)
}

// LoadTable stages a single-target table as the device's waiting table,
// which dmsetup calls the inactive one; Reload's own resume makes it live,
// and nothing else does (Resume). Never read-only, for the [D11] reasons
// above.
func (d *Dm) LoadTable(
	ctx context.Context,
	name string,
	table string,
) error {
	return d.runOk(ctx, "dmsetup", "reload", name, "--table", table)
}

// Reload swaps a live device's table: suspend, load, resume, returning at the
// first command that fails. It fails CLOSED
// (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`,
// "A reload fails closed"): a load that fails returns its error with the
// device still suspended on its old table, and nothing here resumes it. After a
// refused load a resume would reinstate that old table, and where the reload
// retires it — a fence or a park onto a dm-error ([D12]) — the device would
// serve it again and replay onto it the IO the suspend absorbed. A resume that
// fails can leave the device suspended as well (one killed at the SH15 timeout
// may also have completed), and so can a suspend killed after it took effect.
// A load that went through and a resume that never reached the kernel —
// killed or refused before its call, or an agent that stopped between the
// two commands — leave the new table waiting; Resume clears it, so no later
// resume makes it live unread. The device then queues its IO until a later
// reload or resume of it succeeds;
// the kernel treats a suspend of an already suspended device as a no-op, so
// running Reload again retries just the load and the resume.
// Short of a failed command or a restarted agent, no dnv suspension outlives
// the operation that took it: the dn's cutover window (architecture.md,
// Migration, src step 2) is bounded by
// `SuspendSeconds` and ends in a reload — which is what errors the deferred IO
// instead of replaying it — and the cn's only one is CN14's snapshot
// quiesce, resumed inside the same converge pass. The namespace suspension
// (architecture.md, Namespace suspend semantics) is a *park*: a reload onto
// the td's dm-error, live (cnagent.md CN16).
func (d *Dm) Reload(
	ctx context.Context,
	name string,
	table string,
) error {
	if err := d.Suspend(ctx, name); err != nil {
		return err
	}
	if err := d.LoadTable(ctx, name, table); err != nil {
		return err
	}
	return d.resumeLoaded(ctx, name)
}

// SetWritable clears the read-only flag the block layer keeps for the
// device at path, the one `blockdev --setro` sets: `blockdev --setrw`.
// dmsetup's attr column reads a device read-only while its table's mode or
// that flag says so, and a reload of a read-write table clears only the
// first (cnagent.md CN10): after an operator's `blockdev --setro` and a
// read-write reload, a dd write to the device still failed with EPERM in
// the lab.
func (d *Dm) SetWritable(ctx context.Context, path string) error {
	return d.runOk(ctx, "blockdev", "--setrw", path)
}

// CreateMulti / ReloadMulti build or swap a **multi-target** table. dmsetup's
// --table option takes a single line only, so the table is fed through stdin:
// `dmsetup create {name}` and `dmsetup load {name}` read it from there when
// --table is absent. tableLines is newline-separated, one target per line.
func (d *Dm) CreateMulti(
	ctx context.Context,
	name string,
	tableLines string,
) error {
	return d.runStdinOk(ctx, tableLines, "dmsetup", "create", name)
}

// ReloadMulti is the multi-target Reload: suspend, load from stdin, resume.
// It fails closed exactly as Reload does: a load that fails returns its error
// with the device still suspended on its old table.
func (d *Dm) ReloadMulti(
	ctx context.Context,
	name string,
	tableLines string,
) error {
	if err := d.Suspend(ctx, name); err != nil {
		return err
	}
	if err := d.runStdinOk(
		ctx, tableLines, "dmsetup", "reload", name); err != nil {
		return err
	}
	return d.resumeLoaded(ctx, name)
}

func (d *Dm) Suspend(ctx context.Context, name string) error {
	return d.runOk(ctx, "dmsetup", "suspend", name)
}

// Resume resumes a device on the live table its caller compared: it first
// drops any waiting table with `dmsetup clear`, then resumes. A plain
// `dmsetup resume` makes a waiting table live, and a reload that stopped
// after its load leaves one, so without the clear a resume meant to keep a
// fenced device on its dm-error could put it back on the table that maps
// the side's data (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`,
// `nvmehost.go`). A clear that fails returns its error and resumes nothing:
// the device stays suspended, the closed side, as after a reload that fails.
// Clearing a device with no waiting table changes nothing.
func (d *Dm) Resume(ctx context.Context, name string) error {
	if err := d.runOk(ctx, "dmsetup", "clear", name); err != nil {
		return err
	}
	return d.resumeLoaded(ctx, name)
}

// resumeLoaded is the resume of Reload and ReloadMulti, the resume that keeps
// the waiting table: the table its own load just put there.
func (d *Dm) resumeLoaded(ctx context.Context, name string) error {
	return d.runOk(ctx, "dmsetup", "resume", name)
}

// FinishReload ends a reload that stopped after its load: a plain resume,
// which makes the waiting table live. It is for a caller that has read the
// waiting table (WaitingTable) and wants it live; every other resume goes
// through Resume, which drops it.
func (d *Dm) FinishReload(ctx context.Context, name string) error {
	return d.resumeLoaded(ctx, name)
}

func (d *Dm) Remove(ctx context.Context, name string) error {
	return d.runOk(ctx, "dmsetup", "remove", name)
}

// Message sends a target message. A refusal is a CmdError whose errno the
// caller reads through IsExists, IsNoData or IsBusy: a create_thin or
// create_snap of an id the pool holds, a create_snap of an origin it lacks,
// a second reserve_metadata_snap.
func (d *Dm) Message(
	ctx context.Context,
	name string,
	sector uint64,
	message string,
) error {
	return d.runOk(ctx, "dmsetup", "message", name,
		strconv.FormatUint(sector, 10), message)
}

// DevNo resolves a device path to the "major:minor" form dm tables use, so a
// freshly built table string is directly comparable with `dmsetup table`
// output (which always prints device numbers).
func (d *Dm) DevNo(ctx context.Context, path string) (string, error) {
	stdout, stderr, exitCode, err := d.run(ctx, "lsblk",
		"--nodeps", "--noheadings", "--output", "MAJ:MIN", path)
	if err != nil {
		return "", cmdError("lsblk", []string{path},
			stdout, stderr, exitCode, err)
	}
	devNo := strings.TrimSpace(stdout)
	if idx := strings.IndexByte(devNo, '\n'); idx >= 0 {
		devNo = strings.TrimSpace(devNo[:idx])
	}
	if !strings.Contains(devNo, ":") {
		return "", fmt.Errorf("lsblk %s: unparsable MAJ:MIN %q", path, devNo)
	}
	return devNo, nil
}

// DiscardRanges discards byte ranges of a device in the agent's own process
// (OsClient.DiscardRanges) — how the agents mark dm-clone regions "already
// hydrated" (architecture.md, Bitmap push protocol and raid0 bitmap math):
// one open, not exclusive, and one discard per range, with no child process
// per range. It returns only once every discard it sent has left the kernel
// (osclient.md, DiscardRanges), so ctx goes on unchanged.
func (d *Dm) DiscardRanges(
	ctx context.Context,
	dev string,
	ranges []common.DiscardRange,
) error {
	done, err := d.oc.DiscardRanges(ctx, dev, ranges)
	if err != nil {
		return fmt.Errorf("discarding %s: %d of %d ranges done: %w",
			dev, done, len(ranges), err)
	}
	return nil
}

// BlkZeroout writes zeros over one byte range of a device — the side
// provisioning primitive (architecture.md, Side provisioning protocol;
// [D15]), which zeroes the part of a new side that must read as zeros. Unlike
// DiscardRanges (a metadata-only "mark hydrated" hint) this is a
// *guaranteed* zero write: discard-reads-zeros is not a hardware guarantee
// (the kernel dropped discard_zeroes_data in 4.12, NVMe DLFEAT read-zeroes is
// optional). It needs no zeroing support in the disk: where the disk has
// none, the kernel writes the zeros itself.
//
// --zeroout is the first argument, so a `blkdiscard --zeroout` grep finds
// every zeroing batch; the hydration marking runs no blkdiscard at all
// (DiscardRanges).
//
// answered is Reported's verdict on the run: false when the tool never
// reported — typically killed at the SH15 soft timeout, which the zeroing loop
// reads as a batch too big for the disk's current rate (dnagent.md DN9) — and
// true when it ran, so that a non-nil err is the tool's own refusal.
func (d *Dm) BlkZeroout(
	ctx context.Context,
	dev string,
	offset uint64,
	length uint64,
) (answered bool, err error) {
	args := []string{"--zeroout",
		"--offset", strconv.FormatUint(offset, 10),
		"--length", strconv.FormatUint(length, 10),
		dev}
	stdout, stderr, exitCode, err := d.run(ctx, "blkdiscard", args...)
	if err != nil {
		return reported(exitCode, err),
			cmdError("blkdiscard", args, stdout, stderr, exitCode, err)
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Table builders (dnagent.md,
// OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`). Device references are
// "major:minor" strings.
// ---------------------------------------------------------------------------

func ErrorTable(sectors uint64) string {
	return fmt.Sprintf("0 %d error", sectors)
}

func LinearTable(sectors uint64, dev string, offsetSectors uint64) string {
	return fmt.Sprintf("0 %d linear %s %d", sectors, dev, offsetSectors)
}

// FlakeyErrorWritesTable is the [D11] read-only table: dm-flakey with
// up_interval 0 and down_interval 1 is permanently "down", and the single
// `error_writes` feature makes "down" mean reads pass / writes error.
// `<num_features>` counts the feature *name*, so it is 1 here.
func FlakeyErrorWritesTable(sectors uint64, dev string) string {
	return fmt.Sprintf("0 %d flakey %s 0 0 1 1 error_writes", sectors, dev)
}

// The remaining dm targets of architecture.md, Data-plane device stacks —
// striped, thin-pool, thin — have no
// builder here on purpose. dm-thin and dm-stripe print their tables back with
// status-derived arguments appended, so the cn role compares a *prefix* of the
// arguments rather than a whole string; it therefore builds those tables from
// the same argument slice it compares against, and a second representation
// here could only drift from it.

// ExtentRunSectors is one run of a side's allocation, already converted to
// sectors: the byte offset of its first extent on the raw disk and its
// length.
type ExtentRunSectors struct {
	OffsetSectors uint64
	LenSectors    uint64
}

// LinearRunsTable builds the multi-line dm-linear table of a side device
// ([D13]): one line per extent run, `start` accumulating from 0, so
// concatenating the runs in order gives the side's linear address space.
func LinearRunsTable(runs []ExtentRunSectors, devNo string) string {
	var sb strings.Builder
	var start uint64
	for _, run := range runs {
		fmt.Fprintf(&sb, "%d %d linear %s %d\n",
			start, run.LenSectors, devNo, run.OffsetSectors)
		start += run.LenSectors
	}
	return sb.String()
}

// sysDevBlockDir is the sysfs directory that names every block device by
// its number.
const sysDevBlockDir = "/sys/dev/block"

// DiskSize returns the byte size of a block device, the GetDnSize probe,
// read in the agent's own process (dnagent.md SH17): the number of the
// device dev names, then the "size" sysfs keeps under that number, which
// counts 512-byte sectors whatever the device's block size — the file
// `lsblk` reads the size from. Every failure is an error: a device that is
// not there, a read that did not answer, a value that is not a number.
func (d *Dm) DiskSize(ctx context.Context, dev string) (uint64, error) {
	devNo, err := d.readDevNo(ctx, dev)
	if err != nil {
		return 0, err
	}
	path := sysDevBlockDir + "/" + devNo + "/size"
	text, ok, err := d.readAttrStrict(ctx, path)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("block device %s (%s) is not there", dev, devNo)
	}
	sectors, parseErr := strconv.ParseUint(text, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("%s reads %q, not a size", path, text)
	}
	return sectors * SectorSize, nil
}

// InFlight reads the IO a block device has in flight, reads and writes, from
// "/sys/dev/block/<devNo>/inflight", the counts the block layer keeps of the
// requests the device has taken and not completed. A device that is not
// there, or a line that does not read as two counts, is an error.
func (d *Dm) InFlight(ctx context.Context, devNo string) (uint64, uint64,
	error) {
	path := sysDevBlockDir + "/" + devNo + "/inflight"
	text, ok, err := d.readAttrStrict(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, fmt.Errorf("block device %s is not there", devNo)
	}
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("%s reads %q, not two counts", path, text)
	}
	reads, rerr := strconv.ParseUint(fields[0], 10, 64)
	writes, werr := strconv.ParseUint(fields[1], 10, 64)
	if rerr != nil || werr != nil {
		return 0, 0, fmt.Errorf("%s reads %q, not two counts", path, text)
	}
	return reads, writes, nil
}

// CloneTable builds a dm-clone table. Hydration and discard passdown are
// feature flags, the hydration knobs are core arguments; both are also
// reachable through `dmsetup message` on a live device, which is how they are
// changed later.
//
// noDiscardPassdown matters because architecture.md, Bitmap push protocol and
// raid0 bitmap math, use a discard on the
// dm-clone as a metadata-only "mark this region hydrated" primitive. dm-clone
// enables discard passdown by default whenever the destination advertises a
// discard granularity no larger than a region, and then *also* remaps the
// discard to the destination — which would unmap exactly the blocks the
// destination already owns. Every dnv caller therefore passes true, with no
// exceptions: on the cn clone because the chunk pushes of Bitmap push protocol
// mark regions hydrated, and on the dn migration because after the cutover
// (architecture.md, Migration, dst step 5) host IO flows through the dst
// dm-clone, so a chunk whose bits were
// read from the CN thin metadata *before* a host write can arrive afterwards
// and discard a region the host has already written — with passdown that
// discard would reach the dst side device and destroy the only copy of an
// acknowledged write (cnagent.md CN18 step 3, dnagent.md DN13 and [D7]: the
// features `2 no_hydration no_discard_passdown`).
func CloneTable(
	sectors uint64,
	metaDev string,
	destDev string,
	srcDev string,
	regionSectors uint64,
	noHydration bool,
	noDiscardPassdown bool,
	threshold uint32,
	batchSize uint32,
) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "0 %d clone %s %s %s %d",
		sectors, metaDev, destDev, srcDev, regionSectors)
	var features []string
	if noHydration {
		features = append(features, "no_hydration")
	}
	if noDiscardPassdown {
		features = append(features, "no_discard_passdown")
	}
	fmt.Fprintf(&sb, " %d", len(features))
	for _, feature := range features {
		sb.WriteString(" ")
		sb.WriteString(feature)
	}
	var core []string
	if threshold > 0 {
		core = append(core, "hydration_threshold",
			strconv.FormatUint(uint64(threshold), 10))
	}
	if batchSize > 0 {
		core = append(core, "hydration_batch_size",
			strconv.FormatUint(uint64(batchSize), 10))
	}
	fmt.Fprintf(&sb, " %d", len(core))
	for _, arg := range core {
		sb.WriteString(" ")
		sb.WriteString(arg)
	}
	return sb.String()
}

// CloneStatusDetails is the details text of a dm-clone row whose pass read the
// device's status: the row's note, then the raw status line on a line of its
// own; the raw line alone when there is no note, and the note alone when
// there is no line. Both agents build every such row with it, so the raw
// line survives the note for every reader that scans the details line by
// line: common.ParseCloneStatus, which the gateway's hydration checks use,
// skips a note that holds no dm line and reads the status line
// (architecture.md, Live-state reporting).
func CloneStatusDetails(note, raw string) string {
	switch {
	case raw == "":
		return note
	case note == "":
		return raw
	}
	return note + "\n" + raw
}
