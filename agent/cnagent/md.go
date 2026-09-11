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

// MdDetail is the live state of one array.
type MdDetail struct {
	Exists bool
	// Raw is the human-readable `mdadm --detail` output; its State/rebuild
	// lines ride into ResInfo.details (CN28).
	Raw   string
	State string
	// Devices are the member device paths the array currently holds, from
	// `mdadm --detail --export` (MD_DEVICE_*_DEV).
	Devices []string
}

// Detail probes one array. A non-zero exit means the array is not running —
// which is "absent", not a failure: assembly is exactly what fixes it.
func (m *Md) Detail(ctx context.Context, dev string) (*MdDetail, error) {
	stdout, _, _, err := m.cmd.Run(ctx, "mdadm", "--detail", dev)
	if err != nil {
		return &MdDetail{}, nil
	}
	detail := &MdDetail{Exists: true, Raw: strings.TrimSpace(stdout)}
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "State" {
			detail.State = strings.TrimSpace(value)
			break
		}
	}
	export, _, _, err := m.cmd.Run(ctx, "mdadm", "--detail", "--export", dev)
	if err == nil {
		detail.Devices = parseMdExportDevices(export)
	}
	return detail, nil
}

// parseMdExportDevices pulls the member paths out of `mdadm --detail --export`:
// one MD_DEVICE_<sanitized name>_DEV line per member.
func parseMdExportDevices(export string) []string {
	var devs []string
	for _, line := range strings.Split(export, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, "MD_DEVICE_") ||
			!strings.HasSuffix(key, "_DEV") {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			devs = append(devs, value)
		}
	}
	return devs
}

// HasSuperblock reports whether a member device carries an md superblock.
// `mdadm --examine` exits non-zero on a device without one, which is the
// §11.1.1 "no superblock" case rather than an error.
func (m *Md) HasSuperblock(ctx context.Context, dev string) bool {
	_, _, _, err := m.cmd.Run(ctx, "mdadm", "--examine", "--export", dev)
	return err == nil
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

// Create builds a fresh array. Members shorter than RaidDevices are padded
// with `missing`, the mdadm idiom for a degraded create — a leg whose side is
// not exporting an optimized path yet is added later by Add.
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
) error {
	if !gp.raid1 {
		if len(gp.legs) != 1 {
			return fmt.Errorf(
				"RedundNone group has %d legs, want 1", len(gp.legs))
		}
		return s.ensureDmLinear(ctx, gp.dmName, gp.dataSectors,
			gp.legs[0].path, gp.dataOffsetSectors)
	}
	detail, err := s.md.Detail(ctx, gp.devPath)
	if err != nil {
		return err
	}
	if !detail.Exists {
		if err := s.assembleGroup(ctx, gp, available); err != nil {
			return err
		}
		detail, err = s.md.Detail(ctx, gp.devPath)
		if err != nil {
			return err
		}
		if !detail.Exists {
			return fmt.Errorf("array %s did not start", gp.devPath)
		}
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
		if s.md.HasSuperblock(ctx, lp.path) {
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
		// superblock-free leg can only be a freshly zeroed side. But that
		// argument covers the legs actually examined. An unavailable member
		// may be the one carrying the group's data (its DN rebooting, its
		// path mid-ANA-move), and creating over the survivors would resync
		// the data away. §11.1.1 puts "one leg available" in case 2, never in
		// case 1.
		if len(members) != len(gp.legs) {
			return fmt.Errorf(
				"only %d of %d legs available and none carries a superblock",
				len(members), len(gp.legs))
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
// a leg_list member the array lacks is added, and md then resyncs — a bitmap
// catch-up for a briefly absent leg, a full rebuild for a promoted spare.
func (s *CnAgentServer) reconcileMembers(
	ctx context.Context,
	gp *grpPlan,
	detail *MdDetail,
	available map[uint64]bool,
) error {
	held, err := s.devNoSet(ctx, detail.Devices)
	if err != nil {
		return err
	}
	wanted := make(map[string]string, len(gp.legs))
	for _, lp := range gp.legs {
		devNo, err := s.dm.DevNo(ctx, lp.path)
		if err != nil {
			// The wrapper is missing; its own ResInfo already says so and
			// the next pass retries. Never fail a member on that basis.
			continue
		}
		wanted[devNo] = lp.path
	}
	// Extras leave before promotions arrive, so the array never briefly
	// holds more members than `--raid-devices`.
	for devNo, path := range held {
		if _, ok := wanted[devNo]; ok {
			continue
		}
		if err := s.md.Fail(ctx, gp.devPath, path); err != nil {
			return err
		}
		if err := s.md.Remove(ctx, gp.devPath, path); err != nil {
			return err
		}
	}
	for _, lp := range gp.legs {
		devNo, err := s.dm.DevNo(ctx, lp.path)
		if err != nil {
			continue
		}
		if _, ok := held[devNo]; ok {
			continue
		}
		if !available[lp.legId] {
			continue
		}
		if err := s.md.Add(ctx, gp.devPath, lp.path); err != nil {
			return err
		}
	}
	return nil
}

// devNoSet resolves member paths to "major:minor" so a comparison against the
// desired leg wrappers is name-independent: mdadm reports members by their
// kernel node (/dev/dm-3), the plan names them by their dm path.
func (s *CnAgentServer) devNoSet(
	ctx context.Context,
	paths []string,
) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		devNo, err := s.dm.DevNo(ctx, path)
		if err != nil {
			// A member whose node vanished cannot be addressed by devno;
			// keep its path so it can still be failed/removed by name.
			out[path] = path
			continue
		}
		out[devNo] = path
	}
	return out, nil
}

// removeGroup tears one group device down: `mdadm --stop` for an array,
// `dmsetup remove` for a RedundNone linear.
func (s *CnAgentServer) removeGroup(ctx context.Context, gp *grpPlan) {
	if !gp.raid1 {
		s.removeDm(ctx, gp.dmName)
		return
	}
	detail, err := s.md.Detail(ctx, gp.devPath)
	if err != nil || !detail.Exists {
		return
	}
	if err := s.md.Stop(ctx, gp.devPath); err != nil {
		slog.ErrorContext(ctx, "stopping md array failed",
			slog.String("array", gp.devPath),
			slog.String("error", err.Error()))
	}
}

// probeGroup is the read-only CN28 view of a group: for RedundMdRaid1 an
// active array (degraded included) is OK with its state line in details; for
// RedundNone the dm table decides.
//
// Like ensureGroup it is only ever reached for a non-deferred group ([D15]), and
// like ensureGroup it states the leg-count invariant rather than trusting the
// caller with an unguarded gp.legs[0].
func (s *CnAgentServer) probeGroup(
	ctx context.Context,
	gp *grpPlan,
) (pb.ResStatus, string) {
	if !gp.raid1 {
		if len(gp.legs) != 1 {
			return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
				"RedundNone group has %d legs, want 1", len(gp.legs))
		}
		return s.probeDmTarget(ctx, gp.dmName, "linear", gp.dataSectors,
			gp.legs[0].path, gp.dataOffsetSectors)
	}
	detail, err := s.md.Detail(ctx, gp.devPath)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !detail.Exists {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	if strings.Contains(detail.State, "inactive") {
		return pb.ResStatus_RES_STATUS_ERROR, detail.State
	}
	return pb.ResStatus_RES_STATUS_OK, detail.State
}
