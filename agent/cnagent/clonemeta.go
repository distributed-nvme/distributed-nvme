package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CloneMeta wraps the CN base-state tooling of Appendix A — the tmpfs mount,
// its sparse backing file and the single loop device — plus the clone-metadata
// slot allocator that replaced the clone VG ([D14]). No LVM
// runs anywhere in dnv any more: the bare `vgs`/`lvs` label scan touched every
// block device on the node, including the suspended transfer-origin ns-devs
// that wedge LVM in unkillable D state, which is the [D13](a) class the dn
// agent evicted first.
//
// The allocator's registry is the kernel's own dm tables: every kind-`b`
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
func (c *CloneMeta) Mounted(
	ctx context.Context,
	path string,
) (bool, string, error) {
	stdout, _, _, err := c.cmd.Run(ctx, "findmnt",
		"--noheadings", "--output", "FSTYPE", "--target", path)
	if err != nil {
		return false, "", nil
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
	source, _, _, err := c.cmd.Run(ctx, "findmnt",
		"--noheadings", "--output", "TARGET", "--mountpoint", path)
	if err != nil || strings.TrimSpace(source) == "" {
		return false, "", nil
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
// exist.
func (c *CloneMeta) FileSize(
	ctx context.Context,
	path string,
) (uint64, bool, error) {
	stdout, _, _, err := c.cmd.Run(ctx, "stat", "--format", "%s", path)
	if err != nil {
		return 0, false, nil
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
// which nothing ever detaches — every later pass then fails with "2 loop
// devices …, want 1" and no clone metadata can be allocated or probed on this
// CN (CN18: a *single* loop device; §8 rejects loop sprawl).
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

// cnCloneMetaUnitCnt is the arena in allocation units: 1 GiB / 4 MiB = 256.
const cnCloneMetaUnitCnt = common.CnCloneMetaAreaSize / common.CnCloneMetaUnit

// cnCloneMetaUnitSectors is one unit in dm sectors: 8192.
const cnCloneMetaUnitSectors = common.CnCloneMetaUnit / agent.SectorSize

// cloneMetaUnits budgets one clone's metadata (CN18 step 2, the cn twin of the
// dn agent's migrMetaSize): the dm-clone superblock plus one byte per region
// is a generous bound, rounded up to whole CnCloneMetaUnit units.
// region_cnt = td.size / block_size.
//
// The arena is **per CN**, not per SP: CnTmpFilePath and CnCloneMetaDmPrefix
// are keyed by (cluster, cn), so its 256 units are shared by every clone of
// every cntlr on the node — up to MaxCntlrCntPerCn cntlrs, one per SP. The
// real capacity is therefore sum(metaUnits(clone)) <= 256 across the whole CN,
// and MaxCloneCntPerSp says nothing about it. Because the 4 MiB base term is
// exactly one unit, every real clone costs >= 2 units, so 128 *minimum-cost*
// clones is the CN-wide ceiling — and far fewer for large tds at a small
// data_block_size, where region_cnt dominates (a 1 TiB td at the 64 KiB
// minimum block size costs 5 units). Nothing gates the clone count against
// this; exhaustion is reported as RES_STATUS_ERROR on the clone's rows, the
// lvcreate-ENOSPC equivalent (CN18), and retiring any clone
// on the CN frees its run again.
func cloneMetaUnits(regionCnt uint64) uint64 {
	bytes := uint64(4*1024*1024) + regionCnt
	return (bytes + common.CnCloneMetaUnit - 1) / common.CnCloneMetaUnit
}

// cloneMetaSlot is one kind-`b` wrapper as read back out of its own dm table —
// the allocator's registry record. unitCount == 0 marks a wrapper whose table
// is not a single linear target over the arena: it holds its name but claims
// no units, and its clone reports RES_STATUS_ERROR.
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
// lives on and every kind-`b` wrapper of this CN. It is re-probed once per
// converge or probe pass and never persisted — loop names
// are kernel-assigned and a tmpfs remount can change them under a live agent.
type cloneMetaArena struct {
	loopDev   string // e.g. "/dev/loop0"
	loopDevNo string // e.g. "7:0"
	slots     map[string]cloneMetaSlot
}

// Wrappers enumerates the kind-`b` wrappers of one CN and the unit range each
// holds. `dmsetup ls` prints the literal "No devices found" and still exits 0
// on an empty node, which the prefix filter drops by construction; a non-zero
// exit really is a failure. Only the name is taken from `ls` — its device
// number formatting varies across dmsetup versions — while the major:minor and
// the offset come from `dmsetup table`, which is the registry itself.
func (c *CloneMeta) Wrappers(
	ctx context.Context,
	prefix string,
) (map[string]cloneMetaSlot, error) {
	stdout, stderr, _, err := c.cmd.Run(ctx, "dmsetup", "ls")
	if err != nil {
		return nil, fmt.Errorf("dmsetup ls: %s",
			firstNonEmpty(stderr, stdout, err.Error()))
	}
	slots := make(map[string]cloneMetaSlot)
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		name := fields[0]
		targets, err := c.dm.Table(ctx, name)
		if err != nil {
			// The name list is a snapshot that is stale the instant it is
			// printed: SyncupCntlr holds only the node read lock, so another
			// cntlr's retire or SP_LEVEL_DISABLE teardown can remove a wrapper
			// between the `ls` and this `table`. A wrapper that is gone claims
			// no units, so the name is dropped rather than failing the CN-wide
			// enumeration and flipping healthy clones of unrelated cntlrs to
			// RES_STATUS_ERROR (ERROR feeds err_epoch; PROVISIONING does not).
			//
			// Absence is confirmed rather than assumed: a `table` that fails
			// for a wrapper that is still *there* is a real failure and stays
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

// parseCloneMetaSlot reads one wrapper's allocation back out of its table.
// Anything but a single `0 {len} linear {maj:min} {off}` line claims no units.
func parseCloneMetaSlot(name string, targets []agent.DmTarget) cloneMetaSlot {
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
// design (§8).
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
	taken := make(map[uint64]struct{})
	for _, slot := range used {
		// A wrapper on another device claims nothing here — after a tmpfs
		// remount it is stale, and the converge rebuilds it.
		if slot.devNo != loopDevNo || slot.unitCount == 0 {
			continue
		}
		for i := uint64(0); i < slot.unitCount; i++ {
			taken[slot.unitStart+i] = struct{}{}
		}
	}
	start, err := cloneMetaAllocContiguous(taken, cnCloneMetaUnitCnt, want)
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

// cloneMetaAllocContiguous is first fit, contiguous only — the wrapper is a
// single linear target, so its units must be one run. It and cloneMetaFreeRuns
// are deliberate local twins of the dn allocator's freeRuns/allocContiguous
// (agent/dnagent/diskmeta.go): the two arenas share only their arithmetic, and
// promoting the pair would widen this change into the dn package for no
// runtime gain.
func cloneMetaAllocContiguous(
	used map[uint64]struct{},
	total uint64,
	want uint64,
) (uint64, error) {
	for _, run := range cloneMetaFreeRuns(used, total) {
		if run.count >= want {
			return run.start, nil
		}
	}
	return 0, fmt.Errorf(
		"no contiguous run of %d clone-metadata units in the %d-unit arena",
		want, total)
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
// CnTmpFilePath (CN5's `losetup --associated`, never persisted) plus the
// registry read back out of the kind-`b` dm tables.
func (s *CnAgentServer) cloneMetaArena(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
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
	devNo, err := s.dm.DevNo(ctx, devs[0])
	if err != nil {
		return nil, fmt.Errorf("clone-metadata arena unavailable: %w", err)
	}
	slots, err := s.cmeta.Wrappers(
		ctx, s.nf.CnCloneMetaDmPrefix(clusterId, cnId))
	if err != nil {
		return nil, fmt.Errorf("clone-metadata arena unavailable: %w", err)
	}
	return &cloneMetaArena{
		loopDev:   devs[0],
		loopDevNo: devNo,
		slots:     slots,
	}, nil
}

// planArena resolves the arena once per converge or probe pass and caches it
// on the plan, so a cntlr with N clones still issues one `losetup` and one
// `dmsetup ls` (CN18: the loop device is re-learned every pass and
// the probe compares against the *currently probed* one). The cache lives
// exactly as long as the pass — the plan kept in cntlrState.applied is only
// ever consulted for the retire diff, which names devices and probes nothing.
func (s *CnAgentServer) planArena(
	ctx context.Context,
	plan *cntlrPlan,
) (*cloneMetaArena, error) {
	if !plan.arenaDone {
		plan.arena, plan.arenaErr = s.cloneMetaArena(
			ctx, plan.clusterId, plan.cnId)
		plan.arenaDone = true
	}
	return plan.arena, plan.arenaErr
}

// cloneMetaSlotStatus is the one verdict on a clone's wrapper, shared by the
// CN28 probe row and the CN18 rebuild test: present, a single linear target of
// exactly the budgeted size, and backed by the *currently probed* loop device.
// Any mismatch — a tmpfs remounted under a live agent is the interesting one —
// is an error whose repair is the §11.5 clone rebuild.
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
// "the wrapper survived, the dm-clone did not" case of §11.5 depends on it.
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
// A present-but-mismatched wrapper is removed and reallocated. That is safe
// only in the CN18 order: the caller has already removed the dm-clone, so
// nothing maps the wrapper and the removal cannot fail EBUSY.
func (s *CnAgentServer) ensureCloneMeta(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) error {
	s.cloneMetaMu.Lock()
	defer s.cloneMetaMu.Unlock()

	// Fresh inside the lock: a cached view from earlier in this pass could
	// already be behind another cntlr's allocation.
	arena, err := s.cloneMetaArena(ctx, plan.clusterId, plan.cnId)
	if err != nil {
		return err
	}
	if status, _ := cloneMetaSlotStatus(arena, cp); status ==
		pb.ResStatus_RES_STATUS_OK {
		return nil
	}
	if _, exists := arena.slots[cp.metaDmName]; exists {
		if !s.removeDm(ctx, cp.metaDmName) {
			return fmt.Errorf(
				"the mismatched metadata wrapper %s could not be removed",
				cp.metaDmName)
		}
		delete(arena.slots, cp.metaDmName)
	}
	slot, err := s.cmeta.Alloc(ctx, cp.metaDmName, arena.loopDev,
		arena.loopDevNo, arena.slots, cp.metaUnits)
	if err != nil {
		return err
	}
	// Keep this pass's cached view in step with the kernel, so a second clone
	// of the same cntlr does not read the run back as free.
	if plan.arena != nil {
		plan.arena.slots[slot.name] = slot
	}
	return nil
}

// removeCloneMetaDm removes one kind-`b` wrapper under cloneMetaMu. Removal is
// a *mutation of the registry* — the registry being the kernel's dm table set
// itself — so it belongs in the same critical section as the
// enumerate → discard → create of ensureCloneMeta (ruling R3.6): two cntlrs of
// one CN converge concurrently under the node read lock, and a retire that
// deleted a wrapper in the middle of another cntlr's allocation would both
// break that enumeration and free a run under it.
//
// cloneMetaMu stays a leaf: the only thing held inside it here is one bounded
// `dmsetup info`/`remove` pair, and no other lock is ever acquired under it.
// It must therefore never be called from a path that already holds the mutex
// (ensureCloneMeta, reconcileCloneMeta), which call s.removeDm directly.
func (s *CnAgentServer) removeCloneMetaDm(
	ctx context.Context,
	name string,
) bool {
	s.cloneMetaMu.Lock()
	defer s.cloneMetaMu.Unlock()
	return s.removeDm(ctx, name)
}

// probeCloneMeta is the read-only CN28 row of clone_id_to_meta: the wrapper's
// own dm table, in place of the `lvs` scan the clone VG needed.
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

// reconcileCloneMeta removes the kind-`b` wrappers of one CN whose clone is in
// no locally stored cntlr's desired state — their units are free again in the
// next enumeration. It runs only from the two passes that see *every* stored
// cntlr (Reconcile and syncupCn), both under the node write lock; a
// SyncupCntlr sees one cntlr and could never tell an orphan from a stranger's
// wrapper. Wanted names are compared as a set, so no reverse parser for the
// kind-`b` name format is needed.
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
		spId := st.req.GetCntlrPointer().GetSpId()
		for _, clone := range st.req.GetCloneList() {
			wanted[s.nf.CnCloneMetaDmName(
				clusterId, cnId, spId, clone.GetCloneId())] = struct{}{}
		}
	}

	s.cloneMetaMu.Lock()
	defer s.cloneMetaMu.Unlock()

	arena, err := s.cloneMetaArena(ctx, clusterId, cnId)
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
