package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// CloneMeta wraps the CN base-state tooling (architecture.md, Controller node,
// common; cnagent.md CN5) — the tmpfs mount, its sparse backing file and the
// single loop device — and the enumeration of the kind-`cb` clone-metadata
// wrappers over that loop device, which the sweep removes wherever it finds
// one, since no clone keeps its metadata there (CN2, CN21): a clone's
// dm-clone metadata is on its clone meta group (CN18).
type CloneMeta struct {
	cmd *agent.Cmd
	dm  *agent.Dm
}

func NewCloneMeta(oc common.OsClient) *CloneMeta {
	return &CloneMeta{cmd: agent.NewCmd(oc), dm: agent.NewDm(oc)}
}

// ---------------------------------------------------------------------------
// tmpfs, backing file, loop device
// ---------------------------------------------------------------------------

// Mounted probes a mountpoint. `findmnt` exits non-zero when nothing is
// mounted at the path, which is "absent", not a failure; a mounted path
// reports its filesystem type so the probe can insist on tmpfs.
//
// A `findmnt` that did not answer — either of the two — is an error, never
// "absent" (SH15, the runProbe split). ensureTmpfs answers "absent" with a
// `mount`, and a mount over the live arena stacks a second, empty tmpfs on
// it: the arena file drops out of sight, ensureTmpFile truncates a fresh one,
// and ensureLoopDev attaches a second loop to that, while the first loop
// stays attached to the hidden file (CN5).
func (c *CloneMeta) Mounted(
	ctx context.Context,
	path string,
) (bool, string, error) {
	stdout, ok, err := c.cmd.RunProbe(ctx, "findmnt",
		"--noheadings", "--output", "FSTYPE", "--target", path)
	if err != nil || !ok {
		return false, "", err
	}
	fsType := strings.TrimSpace(stdout)
	if idx := strings.IndexByte(fsType, '\n'); idx >= 0 {
		fsType = strings.TrimSpace(fsType[:idx])
	}
	if fsType == "" {
		return false, "", nil
	}
	// --target resolves to the closest mountpoint, so a path that merely
	// *lives* under another mount would answer that mount's type. Insist the
	// mountpoint itself is the path.
	source, ok, err := c.cmd.RunProbe(ctx, "findmnt",
		"--noheadings", "--output", "TARGET", "--mountpoint", path)
	if err != nil || !ok || strings.TrimSpace(source) == "" {
		return false, "", err
	}
	return true, fsType, nil
}

func (c *CloneMeta) MountTmpfs(
	ctx context.Context,
	path string,
	sizeBytes uint64,
) error {
	if err := c.cmd.RunOk(ctx, "mkdir", "-p", path); err != nil {
		return err
	}
	return c.cmd.RunOk(ctx, "mount", "-t", "tmpfs",
		"-o", "size="+strconv.FormatUint(sizeBytes, 10), "tmpfs", path)
}

// FileSize returns the size of a plain file; ok is false when it does not
// exist. A `stat` that did not answer is an error, never "absent" (SH15):
// ensureTmpFile answers "absent" with a `truncate --size
// CnCloneMetaAreaSize`, which leaves a live arena file of that size as it is
// but resizes one of any other size in place — cutting off its tail on a
// shrink — where the converge otherwise leaves a file of another size alone
// while anything uses it (arenaFileInUse).
func (c *CloneMeta) FileSize(
	ctx context.Context,
	path string,
) (uint64, bool, error) {
	stdout, ok, err := c.cmd.RunProbe(ctx, "stat", "--format", "%s", path)
	if err != nil || !ok {
		return 0, false, err
	}
	text := strings.TrimSpace(stdout)
	size, convErr := strconv.ParseUint(text, 10, 64)
	if convErr != nil {
		return 0, false, fmt.Errorf("stat %s: unparsable size %q", path, text)
	}
	return size, true, nil
}

// Truncate creates the sparse clone-metadata arena file: tmpfs pages
// materialize only as something writes into it.
func (c *CloneMeta) Truncate(
	ctx context.Context,
	path string,
	sizeBytes uint64,
) error {
	return c.cmd.RunOk(ctx, "truncate",
		"--size", strconv.FormatUint(sizeBytes, 10), path)
}

// LoopDevices lists the loop devices backed by path. The set is
// kernel-assigned state, re-learned from this probe on every converge and
// never persisted (CN5); the CN28 probe additionally insists there is exactly
// one.
//
// A failure of the tool is **not** an empty result: `losetup --associated`
// exits 0 with no output when nothing is attached, so the only meaning left
// for a non-zero exit is "the question was not answered". Reporting that as
// "no loop" would make ensureLoopDev attach a second loop to the arena file,
// which no converge of a file of the right size detaches — every later pass
// then fails with "2 loop devices …, want 1" (CN5: a *single* loop device).
func (c *CloneMeta) LoopDevices(
	ctx context.Context,
	path string,
) ([]string, error) {
	stdout, stderr, _, err := c.cmd.Run(ctx, "losetup", "--associated", path)
	if err != nil {
		return nil, fmt.Errorf("losetup --associated %s: %s",
			path, firstNonEmpty(stderr, stdout, err.Error()))
	}
	var devs []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "/dev/loop0: 0 /path/to/file" (and older "[2049]:12" variants):
		// the device is everything before the first colon.
		dev, _, ok := strings.Cut(line, ":")
		if !ok {
			dev = line
		}
		if dev = strings.TrimSpace(dev); dev != "" {
			devs = append(devs, dev)
		}
	}
	return devs, nil
}

func (c *CloneMeta) LoopAttach(
	ctx context.Context,
	path string,
) (string, error) {
	stdout, stderr, _, err := c.cmd.Run(ctx, "losetup",
		"--find", "--show", path)
	if err != nil {
		return "", fmt.Errorf("losetup --find --show %s: %s",
			path, firstNonEmpty(stderr, stdout, err.Error()))
	}
	dev := strings.TrimSpace(stdout)
	if idx := strings.IndexByte(dev, '\n'); idx >= 0 {
		dev = strings.TrimSpace(dev[:idx])
	}
	if dev == "" {
		return "", fmt.Errorf("losetup --find --show %s: no device", path)
	}
	return dev, nil
}

// LoopDetach detaches one loop device from its backing file. The kernel
// detaches a loop device something still has open only when the last user
// closes it, and the device stays attached meanwhile, so a caller that needs
// it gone lists the file's loop devices again (recreateArenaFile).
func (c *CloneMeta) LoopDetach(ctx context.Context, dev string) error {
	return c.cmd.RunOk(ctx, "losetup", "--detach", dev)
}

// LoopHolders lists the devices that hold a loop device open from above —
// a dm table or an md array that maps it — from its sysfs holders
// directory. A device whose sysfs directory is gone has no holder; a listing
// that did not answer is an error, never "no holder".
func (c *CloneMeta) LoopHolders(
	ctx context.Context,
	dev string,
) ([]string, error) {
	holders, _, err := c.cmd.ListDir(ctx,
		sysfsBlockDir+"/"+filepath.Base(dev)+"/holders")
	return holders, err
}

// RemoveFile removes a plain file; an absent one is no error.
func (c *CloneMeta) RemoveFile(ctx context.Context, path string) error {
	return c.cmd.RunOk(ctx, "rm", "-f", path)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The clone-metadata wrappers
// ---------------------------------------------------------------------------

// cnCloneMetaUnitSectors is one unit in dm sectors: 8192.
const cnCloneMetaUnitSectors = common.CnCloneMetaUnit / agent.SectorSize

// cloneMetaSlot is one kind-`cb` wrapper as read back out of its own dm
// table: the device it maps and the unit range it covers. unitCount == 0
// marks a wrapper whose table is not a single linear target over whole units:
// what it maps is unknown, and arenaFileInUse counts it as using the arena.
// sectors is the table's length exactly as the kernel prints it; unitCount is
// the footprint that length occupies, rounded up.
type cloneMetaSlot struct {
	name      string
	devNo     string // the backing device the wrapper's table names
	unitStart uint64
	unitCount uint64 // whole units occupied, rounded UP
	sectors   uint64 // the raw table length
}

// Wrappers enumerates the kind-`cb` wrappers of one CN and the unit range each
// holds: the names that carry prefix, from one `dmsetup ls`, then each one's
// table, from one `dmsetup table` of the wrapper. Only the name is taken from
// the name listing, while the major:minor and the offset come from the
// wrapper's table. `dmsetup ls` prints the literal "No devices found" and
// still exits 0 on an empty node, which the prefix filter drops by
// construction; a non-zero exit really is a failure.
func (c *CloneMeta) Wrappers(
	ctx context.Context,
	prefix string,
) (map[string]cloneMetaSlot, error) {
	names, err := c.wrapperNames(ctx, prefix)
	if err != nil {
		return nil, err
	}
	slots := make(map[string]cloneMetaSlot)
	for _, name := range names {
		targets, err := c.dm.Table(ctx, name)
		if err != nil {
			// The name list is a snapshot that is stale the instant it is
			// printed: a sweep (CN21) can remove a wrapper between the
			// listing that named it and this `table`. A wrapper that is gone
			// maps nothing, so the name is dropped rather than failing the
			// CN-wide enumeration.
			//
			// Absence is confirmed rather than assumed, by a fresh `dmsetup
			// info`: a `table` that fails for a wrapper that is still
			// *there* is a real failure and stays fatal, because leaving out
			// a live wrapper would let arenaFileInUse read the arena file as
			// unused while a wrapper still maps it.
			dev, infoErr := c.dm.Info(ctx, name)
			if infoErr != nil || dev != nil {
				return nil, err
			}
			slog.InfoContext(ctx,
				"a clone metadata wrapper vanished during enumeration",
				slog.String("dm", name))
			continue
		}
		slots[name] = parseCloneMetaSlot(name, targets)
	}
	return slots, nil
}

// wrapperNames is the names Wrappers enumerates: the `dmsetup ls` names that
// carry prefix. A listing that did not answer is the error.
func (c *CloneMeta) wrapperNames(
	ctx context.Context,
	prefix string,
) ([]string, error) {
	stdout, stderr, _, err := c.cmd.Run(ctx, "dmsetup", "ls")
	if err != nil {
		return nil, fmt.Errorf("dmsetup ls: %s",
			firstNonEmpty(stderr, stdout, err.Error()))
	}
	var names []string
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		names = append(names, fields[0])
	}
	sort.Strings(names)
	return names, nil
}

// parseCloneMetaSlot reads what one wrapper maps back out of its table.
// Anything but a single `0 {len} linear {maj:min} {off}` line over whole
// units claims no units, and arenaFileInUse counts it as using the arena.
func parseCloneMetaSlot(name string, targets []common.DmTarget) cloneMetaSlot {
	slot := cloneMetaSlot{name: name}
	if len(targets) != 1 || targets[0].Type != "linear" ||
		targets[0].Start != 0 || len(targets[0].Args) != 2 {
		return slot
	}
	offsetSectors, err := strconv.ParseUint(targets[0].Args[1], 10, 64)
	if err != nil || offsetSectors%cnCloneMetaUnitSectors != 0 {
		return slot
	}
	slot.devNo = targets[0].Args[0]
	slot.unitStart = offsetSectors / cnCloneMetaUnitSectors
	slot.sectors = targets[0].Length
	// Round UP: a table that runs one sector into a unit occupies that whole
	// unit.
	slot.unitCount = (targets[0].Length + cnCloneMetaUnitSectors - 1) /
		cnCloneMetaUnitSectors
	return slot
}

// reconcileCloneMeta removes every kind-`cb` wrapper of one CN: no clone
// keeps its dm-clone metadata in the arena (CN18), so each one is a leftover.
// It runs only from the node-level sweep (sweepCn), under the node write
// lock, which no cntlr's converge runs beside.
func (s *CnAgentServer) reconcileCloneMeta(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
) {
	slots, err := s.cmeta.Wrappers(
		ctx, s.nf.CnCloneMetaDmPrefix(clusterId, cnId))
	if err != nil {
		slog.ErrorContext(ctx, "enumerating clone metadata wrappers failed",
			slog.String("error", err.Error()))
		return
	}
	names := make([]string, 0, len(slots))
	for name := range slots {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// removeDm logs its own failure. A wrapper something still maps
		// refuses to go (EBUSY); the next pass sweeps it again.
		s.removeDm(ctx, name)
	}
}
