package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The device-mapper converge/probe helpers shared by leg.go, md.go, pool.go,
// td.go, clone.go and xfer.go. Every Ensure* here is probe-first (SH16): it
// reads the live table and mutates only a real difference, because reloading a
// live dm table is not a no-op — it stalls host IO.

// argCmpAny tells dmTableMatches to check only the target type and size —
// what the leg wrapper's probe wants, where the backing device number is
// kernel-assigned and carries no desired value to compare against.
const argCmpAny = -1

// dmTableMatches compares a live single-target table with a desired one.
// argCmp bounds the comparison to the leading arguments a target reproduces
// verbatim; dm-thin and dm-clone append status-derived arguments of their own,
// so comparing all of them would report a converged device as drifted.
func dmTableMatches(
	targets []agent.DmTarget,
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

// ensureDmSingle converges one single-target device. keepSuspended leaves a
// deliberately suspended device suspended (the §11.6 namespace suspend, the
// one steady state where that is correct); everywhere else no dnv device is
// ever left suspended ([D12]).
func (s *CnAgentServer) ensureDmSingle(
	ctx context.Context,
	name string,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
	keepSuspended bool,
) error {
	if sectors == 0 {
		return fmt.Errorf("device size is 0")
	}
	table := dmTable(targetType, sectors, args)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return s.dm.Create(ctx, name, table)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	if !dmTableMatches(targets, targetType, sectors, args, argCmp) ||
		dev.ReadOnly {
		return s.dm.Reload(ctx, name, table)
	}
	if dev.Suspended && !keepSuspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

func (s *CnAgentServer) ensureDmError(
	ctx context.Context,
	name string,
	sectors uint64,
) error {
	return s.ensureDmSingle(ctx, name, "error", sectors, nil, 0, false)
}

func (s *CnAgentServer) ensureDmLinear(
	ctx context.Context,
	name string,
	sectors uint64,
	backingPath string,
	offsetSectors uint64,
) error {
	devNo, err := s.dm.DevNo(ctx, backingPath)
	if err != nil {
		return err
	}
	args := []string{devNo, strconv.FormatUint(offsetSectors, 10)}
	return s.ensureDmSingle(ctx, name, "linear", sectors, args, 0, false)
}

// ensureDmMulti converges a multi-target concat (the pool meta/data linears of
// CN13). dmsetup's --table is single-line only, so the table travels on stdin.
// It reports whether the device's table actually changed: a grown metadata
// concat is invisible in the thin-pool's own table, so nothing above would
// ever notice a GrowSlice that appended only meta groups (CN13).
func (s *CnAgentServer) ensureDmMulti(
	ctx context.Context,
	name string,
	segments []dmSegment,
) (bool, error) {
	if len(segments) == 0 {
		return false, fmt.Errorf("concat has no segments")
	}
	lines, want, err := s.buildConcat(ctx, segments)
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
		// effective state lost a group that is already serving, which U4's
		// deferral is not allowed to do (it holds *new* groups out until they
		// are ready, update_01.md U4 / architecture.md §8.5). Report it so the
		// §10.4 reactions or an operator repair the group instead.
		if live, desired := liveConcatSectors(targets),
			wantConcatSectors(want); live > desired {
			return false, fmt.Errorf(
				"refusing to shrink the concat from %d to %d sectors",
				live, desired)
		}
		return true, s.dm.ReloadMulti(ctx, name, lines)
	}
	if dev.Suspended {
		return false, s.dm.Resume(ctx, name)
	}
	return false, nil
}

// dmSegment is one target of a concat: a backing device and how much of it,
// starting at offsetSectors.
type dmSegment struct {
	path          string
	sectors       uint64
	offsetSectors uint64
}

type concatTarget struct {
	start         uint64
	sectors       uint64
	devNo         string
	offsetSectors uint64
}

func (s *CnAgentServer) buildConcat(
	ctx context.Context,
	segments []dmSegment,
) (string, []concatTarget, error) {
	var sb strings.Builder
	var start uint64
	want := make([]concatTarget, 0, len(segments))
	for _, seg := range segments {
		if seg.sectors == 0 {
			return "", nil, fmt.Errorf("concat segment %s has size 0",
				seg.path)
		}
		devNo, err := s.dm.DevNo(ctx, seg.path)
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
func liveConcatSectors(targets []agent.DmTarget) uint64 {
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

func concatMatches(targets []agent.DmTarget, want []concatTarget) bool {
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
// Probes (read-only — CN23/CN29: a probe never mutates)
// ---------------------------------------------------------------------------

func (s *CnAgentServer) probeDmTarget(
	ctx context.Context,
	name string,
	targetType string,
	sectors uint64,
	backingPath string,
	offsetSectors uint64,
) (pb.ResStatus, string) {
	if backingPath == "" {
		return s.probeDmArgs(
			ctx, name, targetType, sectors, nil, argCmpAny)
	}
	devNo, err := s.dm.DevNo(ctx, backingPath)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	args := []string{devNo, strconv.FormatUint(offsetSectors, 10)}
	return s.probeDmArgs(ctx, name, targetType, sectors, args, 0)
}

func (s *CnAgentServer) probeDmArgs(
	ctx context.Context,
	name string,
	targetType string,
	sectors uint64,
	args []string,
	argCmp int,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !dmTableMatches(targets, targetType, sectors, args, argCmp) {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table is not the desired %s target", targetType)
	}
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_OK, detailsSuspended
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

func (s *CnAgentServer) probeDmConcat(
	ctx context.Context,
	name string,
	segments []dmSegment,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	_, want, err := s.buildConcat(ctx, segments)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !concatMatches(targets, want) {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table does not match the %d group segment(s)", len(want))
	}
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_OK, detailsSuspended
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

// removeDm removes a dm device if it exists and reports whether it is gone
// afterwards. A suspended device is resumed first: `dmsetup remove` does not
// succeed on one, and a transfer's origin ns-dev is deliberately suspended in
// steady state (CN16).
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
		ctx, common.NvmetPortId, nqn); err != nil {
		slog.ErrorContext(ctx, "removing nvmet subsystem failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
	}
}
