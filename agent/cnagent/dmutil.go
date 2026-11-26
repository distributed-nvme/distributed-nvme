package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The device-mapper converge/probe helpers shared by the converge files
// (leg.go, md.go, pool.go, td.go, clone.go and xfer.go), by
// probe.go and by syncup_cntlr.go. Every Ensure* here is probe-first (SH16):
// it reads the live table and mutates only a real difference, because
// reloading a live dm table is not a no-op — it stalls host IO. The probes
// read a device's attrs and table through dmInfo and dmTable (reading.go),
// from the pass's reading when it took one; the Ensure* helpers read their
// own device directly (Dm.Info, Dm.Table). A backing dm device's number
// comes from dmDevNo, from the pass's reading when it took one, and an md
// group's from the array the pass's md walk matches (mdDevNo): buildConcat
// takes a concat's segment numbers so in a probe and in a converge alike,
// and ensureDmLinear takes its backing's number from its caller.

// argCmpAny tells dmTableMatches to check only the target type and size —
// what the leg wrapper's probe wants, where the backing device number is
// kernel-assigned and carries no desired value to compare against.
const argCmpAny = -1

// dmTableMatches compares a live single-target table with a desired one.
// argCmp bounds the comparison to the leading arguments a target reproduces
// verbatim; dm-thin and dm-clone append status-derived arguments of their own,
// so comparing all of them would report a converged device as drifted.
func dmTableMatches(
	targets []common.DmTarget,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
) bool {
	if len(targets) != 1 || targets[0].Type != targetType {
		return false
	}
	if sectors != 0 && targets[0].Length != sectors {
		return false
	}
	if argCmp < 0 {
		return true
	}
	if argCmp == 0 {
		argCmp = len(args)
		if len(targets[0].Args) != len(args) {
			return false
		}
	}
	if len(targets[0].Args) < argCmp {
		return false
	}
	for i := 0; i < argCmp; i++ {
		if targets[0].Args[i] != args[i] {
			return false
		}
	}
	return true
}

func dmTable(targetType string, sectors uint64, args []string) string {
	if len(args) == 0 {
		return fmt.Sprintf("0 %d %s", sectors, targetType)
	}
	return fmt.Sprintf("0 %d %s %s",
		sectors, targetType, strings.Join(args, " "))
}

// dmShape is what one read of a single-target dm device found against the
// table a pass wants it to carry (dmShapeOf), which decides the step a
// converge takes on it (dmSingleStep) and the row a probe writes for it
// (probeDmArgs).
type dmShape int

const (
	// dmShapeWanted: there, with the wanted table, and not suspended.
	dmShapeWanted dmShape = iota
	// dmShapeAbsent: not there.
	dmShapeAbsent
	// dmShapeOther: there with another table.
	dmShapeOther
	// dmShapeSuspended: there with the wanted table, and suspended.
	dmShapeSuspended
	// dmShapeReadOnly: there and read-only, whatever its table, which a
	// converge reloads onto the wanted table and makes writable
	// (dmSingleStep); only a read that counts the flag finds it (dmShapeOf's
	// readOnlyDiffers).
	dmShapeReadOnly
)

// dmStep is the command a converge runs on one single-target device.
type dmStep int

const (
	dmStepNone dmStep = iota
	dmStepCreate
	dmStepReload
	dmStepResume
	// dmStepReloadWritable is a reload followed by a clear of the block
	// layer's read-only flag (Dm.SetWritable), the step for a read-only
	// device: dmsetup reads a device read-only while its table's mode or
	// that flag says so, and a reload clears only the first (cnagent.md
	// CN10).
	dmStepReloadWritable
)

// dmShapeOf reads one single-target device, from rd, the reading of a
// cntlr's read, or with no reading its own `dmsetup info` and, for a device
// that is there, `dmsetup table`: the shape it has against the wanted
// table, or the error of a read that did not answer. readOnlyDiffers reads
// the read-only flag (dmShapeReadOnly), as a converge does, which reloads
// such a device and makes it writable (ensureDmSingle), and as a cntlr's
// read does for what it names by a converge's step (wrapperName,
// linearGroupName); a probe's row does not read the flag.
func (s *CnAgentServer) dmShapeOf(
	ctx context.Context,
	rd *cnReading,
	name string,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
	readOnlyDiffers bool,
) (dmShape, error) {
	dev, err := s.dmInfo(ctx, rd, name)
	if err != nil {
		return dmShapeWanted, err
	}
	if dev == nil {
		return dmShapeAbsent, nil
	}
	targets, err := s.dmTable(ctx, rd, name)
	if err != nil {
		return dmShapeWanted, err
	}
	return shapeOf(dev,
		dmTableMatches(targets, targetType, sectors, args, argCmp),
		readOnlyDiffers), nil
}

// shapeOf is the shape of a device that is there, whose table does or does
// not match the wanted one (dmShapeOf).
func shapeOf(dev *agent.DmDevInfo, matches, readOnlyDiffers bool) dmShape {
	switch {
	case readOnlyDiffers && dev.ReadOnly:
		return dmShapeReadOnly
	case !matches:
		return dmShapeOther
	case dev.Suspended:
		return dmShapeSuspended
	}
	return dmShapeWanted
}

// dmSingleStep is the step a converge takes on a single-target device of
// shape (ensureDmSingle): it creates one that is absent, reloads one with
// another table, reloads one that is read-only and makes it writable, and
// resumes one suspended on the table it wants. A cntlr's
// read names a leg wrapper by the same step (wrapperName, cnagent.md CN30).
func dmSingleStep(shape dmShape) dmStep {
	switch shape {
	case dmShapeAbsent:
		return dmStepCreate
	case dmShapeOther:
		return dmStepReload
	case dmShapeReadOnly:
		return dmStepReloadWritable
	case dmShapeSuspended:
		return dmStepResume
	}
	return dmStepNone
}

// takeDmStep runs step on the device name, table being the table a create
// or a reload loads.
func (s *CnAgentServer) takeDmStep(
	ctx context.Context,
	name string,
	step dmStep,
	table string,
) error {
	switch step {
	case dmStepCreate:
		return s.dm.Create(ctx, name, table)
	case dmStepReload:
		return s.dm.Reload(ctx, name, table)
	case dmStepReloadWritable:
		if err := s.dm.Reload(ctx, name, table); err != nil {
			return err
		}
		return s.dm.SetWritable(ctx, s.nf.DmPath(name))
	case dmStepResume:
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// ensureDmSingle converges one single-target device. The reload path resumes
// on its own when its load succeeds; a load that fails leaves the device
// suspended on its old table (Dm.Reload fails closed, cnagent.md CN16) for a
// later pass to converge again. A device found suspended with the table it
// wants is resumed here.
func (s *CnAgentServer) ensureDmSingle(
	ctx context.Context,
	name string,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
) error {
	_, err := s.stepDmSingle(ctx, name, targetType, sectors, args, argCmp)
	return err
}

// stepDmSingle is ensureDmSingle, and says which step it took
// (dmSingleStep): dmStepNone for a device it found as wanted and for a read
// that did not answer, whose error it returns.
func (s *CnAgentServer) stepDmSingle(
	ctx context.Context,
	name string,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
) (dmStep, error) {
	if sectors == 0 {
		return dmStepNone, fmt.Errorf("device size is 0")
	}
	shape, err := s.dmShapeOf(
		ctx, nil, name, targetType, sectors, args, argCmp, true)
	if err != nil {
		return dmStepNone, err
	}
	step := dmSingleStep(shape)
	return step, s.takeDmStep(
		ctx, name, step, dmTable(targetType, sectors, args))
}

// ensureDmError converges a single-target dm-error: a transfer device that
// does not serve (CN17), which no ns-dev maps.
func (s *CnAgentServer) ensureDmError(
	ctx context.Context,
	name string,
	sectors uint64,
) error {
	return s.ensureDmSingle(ctx, name, "error", sectors, nil, 0)
}

// ensureTdError converges a td's dm-error (CN15). Its table reports a write
// cache (agent.ErrorWcTable over the agent's write cache loop device,
// wcacheDevNo), so a host scans an ns-dev that maps it, a standby's or a
// parked one, as having a write cache, which the serving device it stands
// in for, a raid0 over thin devices, has: a failover or the end of a park
// then leaves the host's reading right (CN15, The dm-error reports a write
// cache). The one-target table of an older agent is loaded again into this
// one once the loop device is there; while it is not, either table is the
// one wanted. A read-only one is loaded again and made writable, a
// suspended one on a wanted table resumed.
func (s *CnAgentServer) ensureTdError(
	ctx context.Context,
	name string,
	sectors uint64,
) error {
	if sectors == 0 {
		return fmt.Errorf("device size is 0")
	}
	wcNo := s.wcacheDevNo(ctx, true)
	table := agent.ErrorWcTable(sectors, wcNo)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		err := s.dm.CreateMulti(ctx, name, table)
		if err != nil && wcNo != "" {
			// The loop device can go between its find and the load: the
			// next pass builds the one-target table (wcacheLoadFailed).
			s.wcacheLoadFailed(ctx, name, err)
		}
		return err
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	shape := agent.ErrorShapeOf(targets, sectors)
	if shape == agent.ErrorShapeOther || dev.ReadOnly ||
		(shape == agent.ErrorShapePlain && wcNo != "" &&
			sectors > 2*common.DmErrorWcSectors) {
		err := s.dm.ReloadMulti(ctx, name, table)
		if err != nil && wcNo != "" {
			s.wcacheLoadFailed(ctx, name, err)
			if shape == agent.ErrorShapePlain && !dev.ReadOnly {
				// A load that fails leaves the device suspended on its old
				// table (Dm.Reload fails closed), which this one keeps,
				// live again: suspended, it would queue IO it must fail.
				return s.dm.Resume(ctx, name)
			}
		}
		if err != nil {
			return err
		}
		if dev.ReadOnly {
			return s.dm.SetWritable(ctx, s.nf.DmPath(name))
		}
		return nil
	}
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// msgTdErrorPlain is the Warn record of a td dm-error whose two-target
// table did not load, which then keeps or gets the one-target table (CN15).
const msgTdErrorPlain = "the td dm-error's write cache table did not load"

// wcacheLoadFailed logs a two-target table of the dm-error name that did not
// load, and forgets the loop device as a make that failed does
// (wcacheDevNo): no converge asks for it again, and no read names a
// one-target table by it (probeDmError), before common.CnWcacheRetry, so a
// load that keeps failing brings no re-send on every round.
func (s *CnAgentServer) wcacheLoadFailed(
	ctx context.Context,
	name string,
	err error,
) {
	slog.WarnContext(ctx, msgTdErrorPlain,
		slog.String("dm", name), slog.String("error", err.Error()))
	s.wcache.mu.Lock()
	s.wcache.devNo = ""
	s.wcache.failedAt = s.now()
	s.wcache.mu.Unlock()
}

// probeDmError reads a td's dm-error's row (CN15): OK on either table of a
// device that fails every IO (agent.ErrorShapeOf), the one that reports a
// write cache and an older agent's one-target table. The one-target table
// reads OK with RES_REASON_DIFFERS while the agent knows its write cache
// loop device (wcacheKnown), which names it, so the worker sends the syncup
// whose converge loads the two-target table (ensureTdError, CN30): a row
// that reads OK sets no health epoch. A read never looks for the loop
// device, so with none known the one-target table is named by nothing. The
// read-only flag is not read, as no probe reads it (shapeRow).
func (s *CnAgentServer) probeDmError(
	ctx context.Context,
	rd *cnReading,
	name string,
	sectors uint64,
) (pb.ResStatus, string, pb.ResReason) {
	none := pb.ResReason_RES_REASON_NONE
	dev, err := s.dmInfo(ctx, rd, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	if dev == nil {
		status, details := shapeRow(dmShapeAbsent, "error")
		return status, details, none
	}
	targets, err := s.dmTable(ctx, rd, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	shape := agent.ErrorShapeOf(targets, sectors)
	if shape == agent.ErrorShapeOther {
		status, details := shapeRow(dmShapeOther, "error")
		return status, details, none
	}
	if dev.Suspended {
		status, details := shapeRow(dmShapeSuspended, "error")
		return status, details, none
	}
	if shape == agent.ErrorShapePlain && s.wcacheKnown() != "" &&
		sectors > 2*common.DmErrorWcSectors {
		return pb.ResStatus_RES_STATUS_OK, detailsTdErrorNoWcache,
			pb.ResReason_RES_REASON_DIFFERS
	}
	return pb.ResStatus_RES_STATUS_OK, "", none
}

// detailsTdErrorNoWcache names a td dm-error on the one-target table while
// the agent has its write cache loop device (probeDmError).
const detailsTdErrorNoWcache = "the one-target table reports no write cache"

// ensureDmLinear converges a dm-linear over the device whose number is
// backingNo. The caller reads that number first, from where its device
// keeps it: an NVMe multipath head's from /sys/block (blockDevNo), a dm
// device's from dmsetup info (dmDevNo).
func (s *CnAgentServer) ensureDmLinear(
	ctx context.Context,
	name string,
	sectors uint64,
	backingNo string,
	offsetSectors uint64,
) error {
	_, err := s.stepDmLinear(ctx, name, sectors, backingNo, offsetSectors)
	return err
}

// linearArgs are the table arguments of a dm-linear over the device whose
// number is backingNo, at offsetSectors of it.
func linearArgs(backingNo string, offsetSectors uint64) []string {
	return []string{backingNo, strconv.FormatUint(offsetSectors, 10)}
}

// stepDmLinear is ensureDmLinear, and says which step it took
// (stepDmSingle).
func (s *CnAgentServer) stepDmLinear(
	ctx context.Context,
	name string,
	sectors uint64,
	backingNo string,
	offsetSectors uint64,
) (dmStep, error) {
	return s.stepDmSingle(ctx, name, "linear", sectors,
		linearArgs(backingNo, offsetSectors), 0)
}

// ensureDmMulti converges a multi-target concat (the pool meta/data linears of
// CN13). dmsetup's --table is single-line only, so the table travels on stdin.
// It reports whether the device's table actually changed: a grown metadata
// concat is invisible in the thin-pool's own table, so nothing above would
// ever notice a GrowSlice that appended only meta groups (CN13).
func (s *CnAgentServer) ensureDmMulti(
	ctx context.Context,
	plan *cntlrPlan,
	name string,
	segments []dmSegment,
) (bool, error) {
	if len(segments) == 0 {
		return false, fmt.Errorf("concat has no segments")
	}
	lines, want, err := s.buildConcat(ctx, plan, segments)
	if err != nil {
		return false, err
	}
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return false, err
	}
	if dev == nil {
		return true, s.dm.CreateMulti(ctx, name, lines)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return false, err
	}
	if !concatMatches(targets, want) || dev.ReadOnly {
		// A live pool concat may only ever **grow**: its target list is the
		// physical layout of every block the thin-pool above it has already
		// allocated (CN13), so shortening it remaps live data — and dm-thin
		// then either refuses the resume and leaves the pool suspended, or
		// accepts it and serves the wrong device. A desired state shorter than
		// the live table therefore never means "reload"; it means the
		// effective state lost a group that is already serving, which the
		// provisioning deferral is not allowed to do (it holds *new* groups
		// out until they are ready, architecture.md, GrowSlice). Report it so
		// the automatic reactions (architecture.md, Automatic reactions) or an
		// operator repair the group instead.
		if live, desired := liveConcatSectors(targets),
			wantConcatSectors(want); live > desired {
			return false, fmt.Errorf(
				"%s from %d to %d sectors", detailsConcatNoShrink,
				live, desired)
		}
		if err := s.dm.ReloadMulti(ctx, name, lines); err != nil {
			return true, err
		}
		if dev.ReadOnly {
			// A reload leaves the block layer's own read-only flag set
			// (cnagent.md CN10, A read-only device is made writable).
			if err := s.dm.SetWritable(
				ctx, s.nf.DmPath(name)); err != nil {
				return true, writableError{err}
			}
		}
		return true, nil
	}
	if dev.Suspended {
		return false, s.dm.Resume(ctx, name)
	}
	return false, nil
}

// writableError is a clear of the block layer's read-only flag that failed
// after a reload that went through (Dm.SetWritable): the device is there,
// resumed and on the table it wants, so what is built over it goes on, and
// only the device's own row reads the failure (cnagent.md CN10, A read-only
// device is made writable).
type writableError struct{ err error }

func (e writableError) Error() string { return e.err.Error() }

func (e writableError) Unwrap() error { return e.err }

// stopsOver reports whether a device's step error stops what is built over
// the device: any error but a failed clear of its read-only flag
// (writableError).
func stopsOver(err error) bool {
	var clear writableError
	return err != nil && !errors.As(err, &clear)
}

// dmSegment is one target of a concat: a group's device and how much of it,
// starting at offsetSectors.
type dmSegment struct {
	grp           *grpPlan
	sectors       uint64
	offsetSectors uint64
}

type concatTarget struct {
	start         uint64
	sectors       uint64
	devNo         string
	offsetSectors uint64
}

// grpDevNo is the number a group's device is named by in a table over it, a
// pool concat's (buildConcat) or a dm-clone's (ensureDmClone): an md group's
// array's from /sys/block, of the array the pass's md walk matches by the
// group's legs (mdDevNo), so the table and the group's row read one array; a
// RedundNone group's dm-linear's from dmsetup info, with the pass's reading
// (dmDevNo).
func (s *CnAgentServer) grpDevNo(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
) (string, error) {
	if gp.raid1 {
		return s.mdDevNo(ctx, plan, gp)
	}
	return s.dmDevNo(ctx, plan.rd, gp.dmName)
}

// buildConcat is a concat's table and its targets, each group's device
// named by its number (grpDevNo).
func (s *CnAgentServer) buildConcat(
	ctx context.Context,
	plan *cntlrPlan,
	segments []dmSegment,
) (string, []concatTarget, error) {
	var sb strings.Builder
	var start uint64
	want := make([]concatTarget, 0, len(segments))
	for _, seg := range segments {
		if seg.sectors == 0 {
			return "", nil, fmt.Errorf("concat segment %s has size 0",
				seg.grp.resName)
		}
		devNo, err := s.grpDevNo(ctx, plan, seg.grp)
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(&sb, "%d %d linear %s %d\n",
			start, seg.sectors, devNo, seg.offsetSectors)
		want = append(want, concatTarget{
			start:         start,
			sectors:       seg.sectors,
			devNo:         devNo,
			offsetSectors: seg.offsetSectors,
		})
		start += seg.sectors
	}
	return sb.String(), want, nil
}

// liveConcatSectors / wantConcatSectors are the total length of a concat, live
// and desired. They exist for the one comparison ensureDmMulti makes before a
// reload: a concat may grow, never shrink.
func liveConcatSectors(targets []common.DmTarget) uint64 {
	var total uint64
	for _, target := range targets {
		total += target.Length
	}
	return total
}

func wantConcatSectors(want []concatTarget) uint64 {
	var total uint64
	for _, target := range want {
		total += target.sectors
	}
	return total
}

func concatMatches(targets []common.DmTarget, want []concatTarget) bool {
	if len(targets) != len(want) {
		return false
	}
	for i, target := range targets {
		if target.Type != "linear" || target.Start != want[i].start ||
			target.Length != want[i].sectors ||
			len(target.Args) != 2 || target.Args[0] != want[i].devNo ||
			target.Args[1] != strconv.FormatUint(
				want[i].offsetSectors, 10) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Probes (CN23/CN29: a probe changes no object of the node)
// ---------------------------------------------------------------------------

// detailsConcatNoShrink starts the error of a concat that would shrink: no
// converge cures it, so the cntlr's verdict leaves its row out
// (cntlrRowVerdict).
const detailsConcatNoShrink = "refusing to shrink the concat"

// probeDmTarget probes a single-target device over the dm device backingDm,
// at offsetSectors of it: the backing's number first (dmDevNo), then the
// device. With no backingDm only the target type and size are compared.
func (s *CnAgentServer) probeDmTarget(
	ctx context.Context,
	rd *cnReading,
	name string,
	targetType string,
	sectors uint64,
	backingDm string,
	offsetSectors uint64,
) (pb.ResStatus, string) {
	if backingDm == "" {
		return s.probeDmArgs(
			ctx, rd, name, targetType, sectors, nil, argCmpAny)
	}
	devNo, err := s.dmDevNo(ctx, rd, backingDm)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	args := []string{devNo, strconv.FormatUint(offsetSectors, 10)}
	return s.probeDmArgs(ctx, rd, name, targetType, sectors, args, 0)
}

func (s *CnAgentServer) probeDmArgs(
	ctx context.Context,
	rd *cnReading,
	name string,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
) (pb.ResStatus, string) {
	shape, err := s.dmShapeOf(
		ctx, rd, name, targetType, sectors, args, argCmp, false)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	return shapeRow(shape, targetType)
}

// shapeRow is the row a probe writes for a device of shape, and the
// details a verdict names it with (shapeFailure). A probe never finds
// dmShapeReadOnly: its row does not read the flag.
func shapeRow(shape dmShape, targetType string) (pb.ResStatus, string) {
	switch shape {
	case dmShapeAbsent:
		return pb.ResStatus_RES_STATUS_MISSING, ""
	case dmShapeOther:
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table is not the desired %s target", targetType)
	case dmShapeSuspended:
		return pb.ResStatus_RES_STATUS_ERROR, detailsSuspended
	case dmShapeReadOnly:
		return pb.ResStatus_RES_STATUS_ERROR, "read-only"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

func (s *CnAgentServer) probeDmConcat(
	ctx context.Context,
	plan *cntlrPlan,
	name string,
	segments []dmSegment,
) (pb.ResStatus, string) {
	dev, err := s.dmInfo(ctx, plan.rd, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	_, want, err := s.buildConcat(ctx, plan, segments)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	targets, err := s.dmTable(ctx, plan.rd, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !concatMatches(targets, want) {
		// A live concat longer than the desired one is the converge's
		// refusal to shrink it (ensureDmMulti), read with the converge's
		// text, so the cntlr's verdict leaves it out on both channels.
		if live, desired := liveConcatSectors(targets),
			wantConcatSectors(want); live > desired {
			return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
				"%s from %d to %d sectors", detailsConcatNoShrink,
				live, desired)
		}
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table does not match the %d group segment(s)", len(want))
	}
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_ERROR, detailsSuspended
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

// removeDm removes a dm device if it exists and reports whether it is gone
// afterwards. A suspended device is resumed first: `dmsetup remove` does not
// succeed on one. Nothing dnv builds is suspended in steady state — an
// effectively suspended ns-dev is parked, live (CN16; architecture.md,
// Namespace suspend semantics) — so the resume guards a device that a reload
// which was interrupted or failed (Dm.Reload fails closed) left suspended.
func (s *CnAgentServer) removeDm(ctx context.Context, name string) bool {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "probing dm device failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
		return false
	}
	if dev == nil {
		return true
	}
	if dev.Suspended {
		if err := s.dm.Resume(ctx, name); err != nil {
			slog.ErrorContext(ctx, "resuming a suspended dm device failed",
				slog.String("name", name),
				slog.String("error", err.Error()))
			return false
		}
	}
	if err := s.dm.Remove(ctx, name); err != nil {
		slog.ErrorContext(ctx, "removing dm device failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

func (s *CnAgentServer) removeExport(ctx context.Context, nqn string) {
	if err := s.nvmet.RemoveSubsystem(
		ctx, s.port.PortId, nqn); err != nil {
		slog.ErrorContext(ctx, "removing nvmet subsystem failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
	}
}
