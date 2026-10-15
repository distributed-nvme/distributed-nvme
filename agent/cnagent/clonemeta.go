package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CloneMeta wraps the CN base-state tooling (architecture.md, Controller node,
// common; cnagent.md CN5) — the tmpfs mount,
// its sparse backing file and the single loop device — plus the clone-metadata
// slot allocator over that loop device ([D14]: dnv runs no LVM, whose
// `vgs`/`lvs` label scan would read every block device on the node and wedge
// in unkillable D state on the error targets, md members and pathless legs a
// CN holds).
//
// The allocator's registry is the kernel's own dm tables: every kind-`cb`
// wrapper's `0 {len} linear {loopdev} {offset}` line records its own
// allocation, and the arena and the dm state are volatile *together* (a reboot
// clears both, an agent restart preserves both), so none of diskmeta.go's
// header/CRC/A-B machinery is needed here.
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
// ensureLoopDev attaches a second loop to that, and every kind-cb wrapper
// still maps the first loop, so every clone on the CN is rebuilt (CN5).
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

// Truncate creates the sparse clone-metadata arena file. tmpfs pages
// materialize only as a dm-clone writes its metadata through a wrapper, and
// the allocator's hole-punch discard frees them again.
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
// then fails with "2 loop devices …, want 1" and no clone metadata can be
// allocated or probed on this CN (CN5: a *single* loop device; [D14] rejects
// loop sprawl).
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
// The clone-metadata slot allocator
// ---------------------------------------------------------------------------

// cnCloneMetaUnitSectors is one unit in dm sectors: 8192.
const cnCloneMetaUnitSectors = common.CnCloneMetaUnit / agent.SectorSize

// The arena is **per CN**, not per SP: CnTmpFilePath and CnCloneMetaDmPrefix
// are keyed by (cluster, cn), so its common.CnCloneMetaUnitCnt units are
// shared by every clone of every cntlr on the node, and a clone's run is
// common.CloneMetaUnits of its destination td (plan.go buildClones;
// architecture.md, Metadata units). The gateway counts each CN's used units
// against that ceiling, which keeps a full arena rare; a node a cntlr
// replacement charged past the ceiling, or an arena whose free units are
// split into runs too short, can still have no free run for a clone. A clone
// that leaves every stored request frees its run again (CN18).

// cloneMetaNoRunError is the allocator's "no free run": the arena answered,
// and no contiguous run of want free units is left in it. It is the one
// allocation failure that waits rather than fails: its clone's two rows read
// RES_STATUS_PROVISIONING with this text, and the cntlr registers CN18's slow
// retry (startCloneMetaRetry), because the run can come back when another
// clone leaves the node. The type is what tells it from every other failure
// of the allocation, so no caller reads the text to decide; the text is what
// the rows carry after their prefix (detailsWaitingForRoom).
type cloneMetaNoRunError struct {
	want  uint64
	total uint64
}

func (e *cloneMetaNoRunError) Error() string {
	return fmt.Sprintf(
		"no contiguous run of %d clone-metadata units in the %d-unit arena",
		e.want, e.total)
}

// isCloneMetaNoRun reports whether err is the allocator's typed "no free run".
func isCloneMetaNoRun(err error) bool {
	var noRun *cloneMetaNoRunError
	return errors.As(err, &noRun)
}

// detailsWaitingForRoom is the details text of both rows of a clone the arena
// has no free run for: a fixed prefix, then the allocator's own message
// (architecture.md, Metadata units; CN18 step 2).
func detailsWaitingForRoom(err error) string {
	return "waiting for metadata room: " + err.Error()
}

// cloneMetaSlot is one kind-`cb` wrapper as read back out of its own dm table —
// the allocator's registry record. unitCount == 0 marks a wrapper whose table
// is not a single linear target over the arena: it holds its name but claims
// no units, and cloneMetaSlotStatus reads it as a mismatch.
//
// sectors is the table's length exactly as the kernel prints it, and it is
// what the CN28 probe compares; unitCount is the *footprint* that length
// occupies, rounded up. The two must not be conflated: comparing a rounded
// count would let any length inside the last unit pass as converged, and
// rounding the footprint down would hand a unit a live wrapper still maps to
// the next allocation, whose discard-first hole punch would then wipe it.
type cloneMetaSlot struct {
	name      string
	devNo     string // the backing device the wrapper's table names
	unitStart uint64
	unitCount uint64 // whole units occupied, rounded UP
	sectors   uint64 // the raw table length
}

// cloneMetaArena is one pass's view of the arena: the loop device it currently
// lives on and every kind-`cb` wrapper of this CN. It is probed afresh in
// every converge, check round or info read, never carried from one to the
// next and never persisted — loop names are kernel-assigned and a tmpfs
// remount can change them under a live agent.
type cloneMetaArena struct {
	loopDev   string // e.g. "/dev/loop0"
	loopDevNo string // e.g. "7:0"
	slots     map[string]cloneMetaSlot
}

// Wrappers enumerates the kind-`cb` wrappers of one CN and the unit range each
// holds: the names that carry prefix, then each one's table. Only the name
// is taken from the name listing, while the major:minor and the offset come
// from the wrapper's table, which is the registry itself.
//
// With no reading (rd nil) the names come from one `dmsetup ls` and each
// table from one `dmsetup table` of the wrapper. `dmsetup ls` prints the
// literal "No devices found" and still exits 0 on an empty node, which the
// prefix filter drops by construction; a non-zero exit really is a failure.
// wanted is not used.
//
// With the reading of a check round or an info read (CN5, CN28) the names
// come from its `dmsetup info` listing and the tables from its `dmsetup
// table` listing (cnReading). A partial info listing can lack a wrapper this
// cntlr wants, so each name of wanted that the listing lacks is looked up on
// its own (cnReading.info) and enumerated when it is there; without that,
// such a listing would read this cntlr's own wrapper MISSING in a round
// while a converge's `dmsetup ls` reads it OK. A complete listing that lacks
// a wanted name answers absent with no read.
func (c *CloneMeta) Wrappers(
	ctx context.Context,
	rd *cnReading,
	prefix string,
	wanted []string,
) (map[string]cloneMetaSlot, error) {
	names, err := c.wrapperNames(ctx, rd, prefix, wanted)
	if err != nil {
		return nil, err
	}
	slots := make(map[string]cloneMetaSlot)
	for _, name := range names {
		var targets []common.DmTarget
		if rd == nil {
			targets, err = c.dm.Table(ctx, name)
		} else {
			targets, err = rd.table(ctx, name)
		}
		if err != nil {
			// The name list is a snapshot that is stale the instant it is
			// printed: SyncupCntlr holds only the node read lock, so another
			// cntlr's sweep (CN21), at SP_LEVEL_DISABLE or otherwise, can
			// remove a wrapper between the listing that named it and this
			// `table`. A wrapper that is gone claims no units, so the name is
			// dropped rather than failing the CN-wide enumeration and
			// flipping healthy clones of unrelated cntlrs to RES_STATUS_ERROR
			// (ERROR feeds err_epoch; PROVISIONING does not).
			//
			// Absence is confirmed rather than assumed, by a fresh `dmsetup
			// info` and never the reading's: a `table` that fails for a
			// wrapper that is still *there* is a real failure and stays
			// fatal, because reporting a live wrapper's units as free would
			// let Alloc hole-punch a serving dm-clone's superblock — the one
			// thing the discard-first order exists to prevent.
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
// carry prefix with no reading; with a reading, the names of its `dmsetup
// info` listing that carry prefix, and each name of wanted, carrying prefix,
// that the listing lacks and that its own lookup finds there, sorted. A
// listing, or a lookup, that did not answer is the error.
func (c *CloneMeta) wrapperNames(
	ctx context.Context,
	rd *cnReading,
	prefix string,
	wanted []string,
) ([]string, error) {
	var names []string
	if rd == nil {
		stdout, stderr, _, err := c.cmd.Run(ctx, "dmsetup", "ls")
		if err != nil {
			return nil, fmt.Errorf("dmsetup ls: %s",
				firstNonEmpty(stderr, stdout, err.Error()))
		}
		for _, line := range strings.Split(stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || !strings.HasPrefix(fields[0], prefix) {
				continue
			}
			names = append(names, fields[0])
		}
		return names, nil
	}
	if rd.infoErr != nil {
		return nil, rd.infoErr
	}
	for name := range rd.infoList.Devs {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	for _, name := range wanted {
		if _, listed := rd.infoList.Devs[name]; listed ||
			!strings.HasPrefix(name, prefix) || slices.Contains(names, name) {
			continue
		}
		dev, err := rd.info(ctx, name)
		if err != nil {
			return nil, err
		}
		if dev != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// parseCloneMetaSlot reads one wrapper's allocation back out of its table.
// Anything but a single `0 {len} linear {maj:min} {off}` line claims no units.
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
	// unit, and the allocator must never hand it out again.
	slot.unitCount = (targets[0].Length + cnCloneMetaUnitSectors - 1) /
		cnCloneMetaUnitSectors
	return slot
}

// Alloc reserves want contiguous units, hole-punches them on the loop device
// and creates the wrapper. The discard comes **first** and is the
// recycled-unit guard: a freed unit still holds the previous clone's valid
// dm-clone superblock, which a new dm-clone would misparse. A hole punch gives
// guaranteed zeros by file semantics (no device DLFEAT involved) and frees the
// tmpfs pages; the zeroing variant of `blkdiscard` is forbidden here — it
// would materialize up to the whole arena in RAM and defeat the sparse-file
// design ([D14]).
//
// The caller holds cloneMetaMu across the enumeration that produced used and
// this call: the registry is the kernel's dm table set, and two cntlrs of the
// same CN converge concurrently.
func (c *CloneMeta) Alloc(
	ctx context.Context,
	name string,
	loopDev string,
	loopDevNo string,
	used map[string]cloneMetaSlot,
	want uint64,
) (cloneMetaSlot, error) {
	taken := cloneMetaTakenUnits(used, loopDevNo, "")
	start, err := cloneMetaAllocContiguous(
		taken, common.CnCloneMetaUnitCnt, want)
	if err != nil {
		return cloneMetaSlot{}, err
	}
	offset := start * common.CnCloneMetaUnit
	length := want * common.CnCloneMetaUnit
	if err := c.dm.BlkDiscardRange(ctx, loopDev, offset, length); err != nil {
		return cloneMetaSlot{}, err
	}
	table := agent.LinearTable(want*cnCloneMetaUnitSectors, loopDevNo,
		start*cnCloneMetaUnitSectors)
	if err := c.dm.Create(ctx, name, table); err != nil {
		return cloneMetaSlot{}, err
	}
	return cloneMetaSlot{
		name:      name,
		devNo:     loopDevNo,
		unitStart: start,
		unitCount: want,
		// The table just written, so the freshly cached slot reads back the
		// way the next enumeration will parse it.
		sectors: want * cnCloneMetaUnitSectors,
	}, nil
}

// cloneMetaTakenUnits is the used-unit footprint one enumeration claims on one
// loop device: the map Alloc's first fit searches around. The wrapper named
// except claims nothing, which is how cloneMetaCanSupply counts the run of a
// wrapper about to be replaced as free; Alloc passes "", because its caller
// has already removed that wrapper and dropped it from the map. It is pure —
// it reads the slots it is handed and touches neither the kernel nor the
// arena — which is what lets the read-only CN28 probe ask the allocator's own
// question through cloneMetaCanSupply without allocating anything.
func cloneMetaTakenUnits(
	used map[string]cloneMetaSlot,
	loopDevNo string,
	except string,
) map[uint64]struct{} {
	taken := make(map[uint64]struct{})
	for _, slot := range used {
		// A wrapper on another device claims nothing here — after a tmpfs
		// remount it is stale, and the converge rebuilds it.
		if slot.devNo != loopDevNo || slot.unitCount == 0 ||
			slot.name == except {
			continue
		}
		for i := uint64(0); i < slot.unitCount; i++ {
			taken[slot.unitStart+i] = struct{}{}
		}
	}
	return taken
}

// cloneMetaCanSupply answers, over an enumeration the caller already holds,
// the question ensureCloneMeta's Alloc will ask for a clone whose wrapper is
// absent or does not match: is there a contiguous run of cp.metaUnits units
// for a fresh wrapper? It returns nil when there is, and the allocator's own
// error when there is not — the typed cloneMetaNoRunError when the arena has
// no such free run, a plain error when the run is larger than the whole
// arena and can never fit. It allocates nothing: no hole punch, no `dmsetup
// create`, no change to the arena it is handed.
//
// The clone's own wrapper claims nothing here, whichever loop device it is
// on: a wrapper that does not match is removed before the allocation
// (ensureCloneMeta), so its run is free by the time Alloc looks, and a
// mismatched wrapper whose own units are the only free ones must not read as
// a full arena (architecture.md, Clone crash recovery). A wrapper that
// matches is never asked about: it is reused as it is.
func cloneMetaCanSupply(
	arena *cloneMetaArena,
	cp *clonePlan,
) error {
	taken := cloneMetaTakenUnits(arena.slots, arena.loopDevNo, cp.metaDmName)
	_, err := cloneMetaAllocContiguous(
		taken, common.CnCloneMetaUnitCnt, cp.metaUnits)
	return err
}

// cloneMetaAllocContiguous is first fit, contiguous only — the wrapper is a
// single linear target, so its units must be one run. It and cloneMetaFreeRuns
// are deliberate local twins of the dn allocator's freeRuns/allocContiguous
// (agent/dnagent/diskmeta.go): the two arenas share the unit formula
// (common.CloneMetaUnits) and nothing else, and promoting the pair into a
// package both agents import would gain nothing at run time.
//
// A run larger than the whole arena can never be supplied, however many
// clones leave the node, so it is a plain error and its clone's rows read
// RES_STATUS_ERROR; only a run that would fit an emptier arena is the typed
// cloneMetaNoRunError, whose rows read RES_STATUS_PROVISIONING (CN18 step 2).
func cloneMetaAllocContiguous(
	used map[uint64]struct{},
	total uint64,
	want uint64,
) (uint64, error) {
	if want > total {
		return 0, fmt.Errorf(
			"a run of %d clone-metadata units is larger than the %d-unit arena",
			want, total)
	}
	for _, run := range cloneMetaFreeRuns(used, total) {
		if run.count >= want {
			return run.start, nil
		}
	}
	return 0, &cloneMetaNoRunError{want: want, total: total}
}

type cloneMetaRun struct {
	start uint64
	count uint64
}

// cloneMetaFreeRuns lists the maximal free runs of [0, total), ascending by
// start.
func cloneMetaFreeRuns(
	used map[uint64]struct{},
	total uint64,
) []cloneMetaRun {
	var out []cloneMetaRun
	var start uint64
	inRun := false
	for i := uint64(0); i < total; i++ {
		if _, taken := used[i]; taken {
			if inRun {
				out = append(out, cloneMetaRun{start, i - start})
				inRun = false
			}
			continue
		}
		if !inRun {
			start, inRun = i, true
		}
	}
	if inRun {
		out = append(out, cloneMetaRun{start, total - start})
	}
	return out
}

// ---------------------------------------------------------------------------
// Server-side plumbing (CN18 step 2, CN28, CN2)
// ---------------------------------------------------------------------------

// cloneMetaArena probes the arena: the single loop device behind
// CnTmpFilePath (CN5's `losetup --associated`, never persisted), its number
// from /sys/block, plus the registry read back out of the kind-`cb` dm
// tables (Wrappers, with rd and wanted as it takes them).
func (s *CnAgentServer) cloneMetaArena(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	rd *cnReading,
	wanted []string,
) (*cloneMetaArena, error) {
	filePath := s.nf.CnTmpFilePath(clusterId, cnId)
	devs, err := s.cmeta.LoopDevices(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("clone-metadata arena unavailable: %w", err)
	}
	if len(devs) != 1 {
		return nil, fmt.Errorf("clone-metadata arena unavailable: "+
			"%d loop devices back %s, want 1", len(devs), filePath)
	}
	devNo, err := s.blockDevNo(ctx, filepath.Base(devs[0]))
	if err != nil {
		return nil, fmt.Errorf("clone-metadata arena unavailable: %w", err)
	}
	slots, err := s.cmeta.Wrappers(
		ctx, rd, s.nf.CnCloneMetaDmPrefix(clusterId, cnId), wanted)
	if err != nil {
		return nil, fmt.Errorf("clone-metadata arena unavailable: %w", err)
	}
	return &cloneMetaArena{
		loopDev:   devs[0],
		loopDevNo: devNo,
		slots:     slots,
	}, nil
}

// planArena resolves the arena at its first call in a converge, check round
// or info read and caches it on the plan, so a cntlr with N clones still
// issues one `losetup` and one enumeration of the wrappers (CN18: the loop
// device is re-learned every pass and the probe compares against the
// *currently probed* one). A converge enumerates them with `dmsetup ls`, a
// check round and an info read from their reading, where the wrapper of
// every clone of the plan is looked up on its own when a partial listing
// lacks it (Wrappers). A converge that allocates enumerates once more under
// cloneMetaMu, and that enumeration replaces the cache (ensureCloneMeta);
// after a failed allocation the next call resolves the arena afresh
// (ensureClone). The cache lives exactly as long as the pass: each pass
// builds its plan afresh (newCntlrPlan), and nothing keeps a plan between
// passes.
func (s *CnAgentServer) planArena(
	ctx context.Context,
	plan *cntlrPlan,
) (*cloneMetaArena, error) {
	if !plan.arenaDone {
		wanted := make([]string, 0, len(plan.clones))
		for _, cp := range plan.clones {
			wanted = append(wanted, cp.metaDmName)
		}
		plan.arena, plan.arenaErr = s.cloneMetaArena(
			ctx, plan.clusterId, plan.cnId, plan.rd, wanted)
		plan.arenaDone = true
	}
	return plan.arena, plan.arenaErr
}

// cloneMetaSlotStatus is the verdict on a clone's wrapper as one enumeration
// found it, shared by the CN28 probe row and the CN18 rebuild test: present, a
// single linear target of exactly the budgeted size, and backed by the
// *currently probed* loop device. Any mismatch — a tmpfs remounted under a live
// agent is the interesting one — is an error whose repair is the clone
// rebuild of architecture.md, Clone crash recovery.
//
// It is not the only source of the CN28 `clone_id_to_meta` row:
// cloneMetaVerdict answers first for a clone whose source is ready
// (cloneRows), and the row is its verdict whenever the arena did not answer,
// whatever the wrapper, or cannot supply a fresh run for a wrapper that is
// absent or does not match.
func cloneMetaSlotStatus(
	arena *cloneMetaArena,
	cp *clonePlan,
) (pb.ResStatus, string) {
	slot, ok := arena.slots[cp.metaDmName]
	if !ok {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	if slot.unitCount == 0 {
		return pb.ResStatus_RES_STATUS_ERROR,
			"table is not the desired linear target"
	}
	// The raw table length, never the rounded unit count: "table length
	// matches the computed size" (CN28) is the check, and
	// every length dnv itself writes is a whole multiple of a unit, so a
	// length that is not is rejected here too.
	if slot.sectors != cp.metaSectors {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table is %d sectors, want %d", slot.sectors, cp.metaSectors)
	}
	if slot.devNo != arena.loopDevNo {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"backed by %s, want the loop device %s",
			slot.devNo, arena.loopDevNo)
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// cloneMetaConverged reports whether this clone's wrapper is already the one
// CN18 wants. A wrapper that is present and matches is reused as-is and is
// **never** re-discarded: it carries a live dm-clone's superblock, and the
// "the wrapper survived, the dm-clone did not" case of the recovery build
// (cnagent.md CN18) depends on it.
func (s *CnAgentServer) cloneMetaConverged(
	ctx context.Context,
	arena *cloneMetaArena,
	cp *clonePlan,
) bool {
	status, details := cloneMetaSlotStatus(arena, cp)
	if status == pb.ResStatus_RES_STATUS_ERROR {
		// Worth a record: the rebuild below discards the slot, so this is the
		// one place the reason survives.
		slog.ErrorContext(ctx, "the clone metadata wrapper does not match",
			slog.String("dm", cp.metaDmName),
			slog.String("error", details))
	}
	return status == pb.ResStatus_RES_STATUS_OK
}

// ensureCloneMeta allocates the clone's units, hole-punches them and creates
// the wrapper (CN18 step 2). cloneMetaMu is held across the whole
// enumerate → discard → create critical section: the registry is the kernel's
// dm table set, and SyncupCntlr holds only the node **read** lock, so two
// cntlrs of the same CN can converge at once — two enumerations could pick the
// same free run and, because their wrappers have different names, neither
// `dmsetup create` would fail.
//
// The room comes first: before anything is removed, the fresh enumeration
// must be able to supply the run (cloneMetaCanSupply), and when it cannot the
// allocator's error comes back unwrapped — the typed cloneMetaNoRunError for
// an arena with no free run, which the caller reports as provisioning — and
// the wrapper is left as it is, so nothing is removed for a run that will
// not come.
//
// A present-but-mismatched wrapper is removed and reallocated. That is safe
// only in the CN18 order: the caller has already tried to remove the
// dm-clone, so nothing maps the wrapper once that removal succeeded; when it
// failed, the dm-clone still maps the wrapper, this removal fails EBUSY, and
// the error ends CN18 step 2.
func (s *CnAgentServer) ensureCloneMeta(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) error {
	s.cloneMetaMu.Lock()
	defer s.cloneMetaMu.Unlock()

	// Fresh inside the lock: a cached view from earlier in this pass could
	// already be behind another cntlr's allocation. The fresh enumeration
	// replaces this pass's cached one at once, whatever becomes of this
	// clone, so a later clone's room check (cloneMetaVerdict) reads the
	// newest enumeration the pass has made: a run another cntlr took, which
	// this one shows, must not read as free there, or that check would park
	// a serving namespace for a run the pass already knows is gone. An
	// enumeration that failed replaces nothing.
	arena, err := s.cloneMetaArena(ctx, plan.clusterId, plan.cnId, nil, nil)
	if err != nil {
		return err
	}
	plan.arena, plan.arenaErr, plan.arenaDone = arena, nil, true
	if status, _ := cloneMetaSlotStatus(arena, cp); status ==
		pb.ResStatus_RES_STATUS_OK {
		return nil
	}
	if err := cloneMetaCanSupply(arena, cp); err != nil {
		return err
	}
	if _, exists := arena.slots[cp.metaDmName]; exists {
		if !s.removeDm(ctx, cp.metaDmName) {
			return fmt.Errorf(
				"the mismatched metadata wrapper %s could not be removed",
				cp.metaDmName)
		}
		// The cached view follows the removal, so a later clone's room check
		// does not count the freed run as taken, whatever becomes of the
		// allocation below.
		delete(arena.slots, cp.metaDmName)
	}
	slot, err := s.cmeta.Alloc(ctx, cp.metaDmName, arena.loopDev,
		arena.loopDevNo, arena.slots, cp.metaUnits)
	if err != nil {
		return err
	}
	// ...and the allocation, in step with the kernel, so a second clone of
	// the same cntlr does not read the run back as free.
	arena.slots[slot.name] = slot
	return nil
}

// probeCloneMeta is the ordinary read-only CN28 row of clone_id_to_meta, read
// from the wrapper's own dm table in this pass's enumeration. The row for a
// clone cloneMetaVerdict decides comes from that verdict instead, which
// cloneRows consults first.
func (s *CnAgentServer) probeCloneMeta(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) (pb.ResStatus, string) {
	arena, err := s.planArena(ctx, plan)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	return cloneMetaSlotStatus(arena, cp)
}

// cloneMetaVerdict is CN18 step 2's verdict on one clone's metadata, read
// from this pass's enumeration of the arena: what the converge asks before it
// parks or removes anything (ensureClone), and what the clone's rows take for
// a source that is ready (cloneRows), so the two channels report the same two
// rows for the same node. decided is false, with no status or details, when
// step 2 goes on to build: the clone's wrapper matches, or the arena can
// supply a fresh run for it. When decided is true the clone gets no further
// in step 2, and status and details are the clone_id_to_meta row:
//
//   - RES_STATUS_ERROR with planArena's own error when the arena did not
//     answer — the converge also registers the CN10 retry for that;
//   - RES_STATUS_PROVISIONING with detailsWaitingForRoom when the arena
//     answered and has no free run for a fresh wrapper (cloneMetaNoRunError):
//     clone_id_to_dm_clone then reads the same (reportCloneNoRoom), and the
//     converge registers CN18's slow retry;
//   - RES_STATUS_ERROR with the allocator's message when the run is larger
//     than the whole arena and can never fit.
//
// For both ERROR verdicts clone_id_to_dm_clone reads ERROR
// detailsCloneMetaMissing (reportCloneMetaErr). The plain "no wrapper, no
// dm-clone" MISSING pair, or a dm-clone's own row, would not say that no slot
// can come; a dm-clone that is still up keeps its status line in the row all
// the same.
//
// A wrapper that is absent or does not match, in an arena that can supply a
// fresh run, is left to the ordinary probes: MISSING, or the mismatch's
// ERROR, is truthful there, because the converge is going to rebuild it.
// Step 2 has other ways to fail, and the verdict cannot see them because a
// read-only pass cannot tell them from that ordinary "not built yet" — a hole
// punch or a `dmsetup create` that failed inside Alloc leaves an absent
// wrapper in an arena with room, for instance, and a mismatched wrapper
// ensureCloneMeta could not remove leaves a present one. The converge's rows
// read the node as the check round's do, with the failure in their details,
// and it registers the CN10 retry, which runs the pass again (ensureClone).
//
// Read-only throughout (CN23, SH25): planArena is this pass's cached
// enumeration and cloneMetaCanSupply is pure, so nothing here takes
// cloneMetaMu, calls Alloc or changes what the arena holds. The one thing
// written is planArena's cache, which the first call of a pass fills.
func (s *CnAgentServer) cloneMetaVerdict(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) (status pb.ResStatus, details string, decided bool) {
	arena, err := s.planArena(ctx, plan)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), true
	}
	if slotStatus, _ := cloneMetaSlotStatus(arena, cp); slotStatus ==
		pb.ResStatus_RES_STATUS_OK {
		return 0, "", false
	}
	if err := cloneMetaCanSupply(arena, cp); err != nil {
		if isCloneMetaNoRun(err) {
			return pb.ResStatus_RES_STATUS_PROVISIONING,
				detailsWaitingForRoom(err), true
		}
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), true
	}
	return 0, "", false
}

// reconcileCloneMeta removes the kind-`cb` wrappers of one CN whose clone is in
// no locally stored cntlr's desired state — their units are free again in the
// next enumeration. It runs only from the two passes that see *every* stored
// cntlr (Reconcile and syncupCn), both under the node write lock; a
// SyncupCntlr sees one cntlr and could never tell an orphan from a stranger's
// wrapper. Wanted names are compared as a set, so no reverse parser for the
// kind-`cb` name format is needed.
func (s *CnAgentServer) reconcileCloneMeta(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
) {
	// The wanted set is read first, so cloneMetaMu is never held while `mu` is
	// taken — it stays the leaf of the hierarchy.
	wanted := make(map[string]struct{})
	for _, key := range s.cntlrKeysOf(clusterId, cnId) {
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		req := st.loadReq()
		spId := req.GetCntlrPointer().GetSpId()
		for _, clone := range req.GetCloneList() {
			wanted[s.nf.CnCloneMetaDmName(
				clusterId, cnId, spId, clone.GetCloneId())] = struct{}{}
		}
	}

	s.cloneMetaMu.Lock()
	defer s.cloneMetaMu.Unlock()

	arena, err := s.cloneMetaArena(ctx, clusterId, cnId, nil, nil)
	if err != nil {
		slog.ErrorContext(ctx, "enumerating clone metadata wrappers failed",
			slog.String("error", err.Error()))
		return
	}
	orphans := make([]string, 0, len(arena.slots))
	for name := range arena.slots {
		if _, ok := wanted[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		// removeDm logs its own failure. A wrapper a live dm-clone still maps
		// refuses to go (EBUSY); the next pass sweeps it again.
		s.removeDm(ctx, name)
	}
}
