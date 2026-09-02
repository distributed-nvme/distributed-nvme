package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// SectorSize is the device-mapper unit: every table size and offset is in
// 512-byte sectors (architecture.md Appendix A).
const SectorSize = 512

// Dm wraps the Appendix A device-mapper command patterns.
type Dm struct {
	osBase
}

func NewDm(oc common.OsClient) *Dm {
	return &Dm{osBase{oc: oc}}
}

// DmDevInfo is the `dmsetup info -o attr` view of one live device.
type DmDevInfo struct {
	Suspended bool
	ReadOnly  bool
}

// The four positions of the dm `attr` column (dmsetup(8): "(L)ive,
// (I)nactive, (s)uspended, (r)ead-only, read-(w)rite"), e.g. `L--w` for a
// live, resumed, writeable device and `L-sw` for a suspended one.
const (
	dmAttrSuspendedIdx = 2
	dmAttrReadOnlyIdx  = 3
)

// DmTarget is one parsed line of a dm table or status.
type DmTarget struct {
	Start  uint64
	Length uint64
	Type   string
	Args   []string
}

// Info returns the live state of a dm device, or nil when it does not exist.
func (d *Dm) Info(ctx context.Context, name string) (*DmDevInfo, error) {
	stdout, _, _, err := d.run(ctx, "dmsetup", "info",
		"--columns", "--noheadings", "-o", "attr", name)
	if err != nil {
		return nil, nil
	}
	attr := strings.TrimSpace(stdout)
	info := &DmDevInfo{}
	if len(attr) > dmAttrSuspendedIdx && attr[dmAttrSuspendedIdx] == 's' {
		info.Suspended = true
	}
	if len(attr) > dmAttrReadOnlyIdx && attr[dmAttrReadOnlyIdx] == 'r' {
		info.ReadOnly = true
	}
	return info, nil
}

// Table returns the parsed live table of a dm device.
func (d *Dm) Table(ctx context.Context, name string) ([]DmTarget, error) {
	stdout, stderr, _, err := d.run(ctx, "dmsetup", "table", name)
	if err != nil {
		return nil, cmdError("dmsetup", []string{"table", name},
			stdout, stderr, err)
	}
	return ParseDmLines(stdout), nil
}

// Status returns the raw `dmsetup status` output (the dm-clone hydration
// line rides into ResInfo.details verbatim, §9.5).
func (d *Dm) Status(ctx context.Context, name string) (string, error) {
	stdout, stderr, _, err := d.run(ctx, "dmsetup", "status", name)
	if err != nil {
		return "", cmdError("dmsetup", []string{"status", name},
			stdout, stderr, err)
	}
	return strings.TrimSpace(stdout), nil
}

// ParseDmLines parses `dmsetup table`/`dmsetup status` output.
func ParseDmLines(out string) []DmTarget {
	var targets []DmTarget
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		start, err1 := strconv.ParseUint(fields[0], 10, 64)
		length, err2 := strconv.ParseUint(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		targets = append(targets, DmTarget{
			Start:  start,
			Length: length,
			Type:   fields[2],
			Args:   fields[3:],
		})
	}
	return targets
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

// LoadTable stages an inactive single-target table; it takes effect on the
// next resume. Never read-only, for the [D11] reasons above.
func (d *Dm) LoadTable(
	ctx context.Context,
	name string,
	table string,
) error {
	return d.runOk(ctx, "dmsetup", "reload", name, "--table", table)
}

// Reload swaps a live device's table: suspend, load, resume (Appendix A).
// The device is always resumed: a reload never leaves it suspended ([D12]).
// The one place dnv holds a suspension is the bounded §11.2 cutover window,
// and even that ends in a reload — which is what errors the deferred IO
// instead of replaying it.
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
	return d.Resume(ctx, name)
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
// Like Reload it always resumes — no dnv device is left suspended ([D12]).
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
	return d.Resume(ctx, name)
}

func (d *Dm) Suspend(ctx context.Context, name string) error {
	return d.runOk(ctx, "dmsetup", "suspend", name)
}

func (d *Dm) Resume(ctx context.Context, name string) error {
	return d.runOk(ctx, "dmsetup", "resume", name)
}

func (d *Dm) Remove(ctx context.Context, name string) error {
	return d.runOk(ctx, "dmsetup", "remove", name)
}

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
	stdout, stderr, _, err := d.run(ctx, "lsblk",
		"--nodeps", "--noheadings", "--output", "MAJ:MIN", path)
	if err != nil {
		return "", cmdError("lsblk", []string{path}, stdout, stderr, err)
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

// BlkDiscardRange discards one byte range of a device — how the agents mark
// dm-clone regions "already hydrated" (§9.6, §11.4).
func (d *Dm) BlkDiscardRange(
	ctx context.Context,
	dev string,
	offset uint64,
	length uint64,
) error {
	return d.runOk(ctx, "blkdiscard",
		"--offset", strconv.FormatUint(offset, 10),
		"--length", strconv.FormatUint(length, 10),
		dev)
}

// ---------------------------------------------------------------------------
// Table builders (Appendix A). Device references are "major:minor" strings.
// ---------------------------------------------------------------------------

func ErrorTable(sectors uint64) string {
	return fmt.Sprintf("0 %d error", sectors)
}

func LinearTable(sectors uint64, dev string, offsetSectors uint64) string {
	return fmt.Sprintf("0 %d linear %s %d", sectors, dev, offsetSectors)
}

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

// BlkDiscard discards a whole device (step 2 of the §9.4 trim protocol).
func (d *Dm) BlkDiscard(ctx context.Context, path string) error {
	return d.runOk(ctx, "blkdiscard", "--force", path)
}

// DiskSize returns the byte size of a block device
// (`lsblk --bytes --nodeps`, the GetDnSize probe).
func (d *Dm) DiskSize(ctx context.Context, dev string) (uint64, error) {
	stdout, stderr, _, err := d.run(ctx, "lsblk",
		"--bytes", "--nodeps", "--noheadings", "--output", "SIZE", dev)
	if err != nil {
		return 0, cmdError("lsblk", []string{dev}, stdout, stderr, err)
	}
	text := strings.TrimSpace(stdout)
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = strings.TrimSpace(text[:idx])
	}
	size, parseErr := strconv.ParseUint(text, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("lsblk %s: unparsable size %q", dev, text)
	}
	return size, nil
}

// CloneTable builds a dm-clone table. Hydration is a feature flag, the
// hydration knobs are core arguments; both are also reachable through
// `dmsetup message` on a live device, which is how they are changed later.
func CloneTable(
	sectors uint64,
	metaDev string,
	destDev string,
	srcDev string,
	regionSectors uint64,
	noHydration bool,
	threshold uint32,
	batchSize uint32,
) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "0 %d clone %s %s %s %d",
		sectors, metaDev, destDev, srcDev, regionSectors)
	if noHydration {
		sb.WriteString(" 1 no_hydration")
	} else {
		sb.WriteString(" 0")
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

// CloneStatus is the parsed `dmsetup status` of a dm-clone device.
type CloneStatus struct {
	Raw              string
	RegionSectors    uint64
	HydratedRegions  uint64
	TotalRegions     uint64
	HydrationEnabled bool
	Threshold        uint32
	BatchSize        uint32
}

// ParseCloneStatus reads the dm-clone status line:
//
//	<meta block size> <used>/<total> <region size> <hydrated>/<total>
//	<hydrating> <#feature args> <feature args>* <#core args> <core args>*
func ParseCloneStatus(raw string) (*CloneStatus, bool) {
	targets := ParseDmLines(raw)
	if len(targets) != 1 || targets[0].Type != "clone" {
		return nil, false
	}
	args := targets[0].Args
	if len(args) < 6 {
		return nil, false
	}
	st := &CloneStatus{Raw: strings.TrimSpace(raw), HydrationEnabled: true}
	st.RegionSectors, _ = strconv.ParseUint(args[2], 10, 64)
	if hydrated, total, ok := strings.Cut(args[3], "/"); ok {
		st.HydratedRegions, _ = strconv.ParseUint(hydrated, 10, 64)
		st.TotalRegions, _ = strconv.ParseUint(total, 10, 64)
	}
	featureCnt, err := strconv.Atoi(args[5])
	if err != nil || featureCnt < 0 || 6+featureCnt > len(args) {
		return st, true
	}
	for _, feature := range args[6 : 6+featureCnt] {
		if feature == "no_hydration" {
			st.HydrationEnabled = false
		}
	}
	rest := args[6+featureCnt:]
	if len(rest) == 0 {
		return st, true
	}
	coreCnt, err := strconv.Atoi(rest[0])
	if err != nil || coreCnt < 0 || 1+coreCnt > len(rest) {
		return st, true
	}
	core := rest[1 : 1+coreCnt]
	for i := 0; i+1 < len(core); i += 2 {
		value, convErr := strconv.ParseUint(core[i+1], 10, 32)
		if convErr != nil {
			continue
		}
		switch core[i] {
		case "hydration_threshold":
			st.Threshold = uint32(value)
		case "hydration_batch_size":
			st.BatchSize = uint32(value)
		}
	}
	return st, true
}
