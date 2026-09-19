package cnagent

import (
	"context"
	"fmt"
	"regexp"
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
// which is "absent", not a failure: assembly is exactly what fixes it. A run
// that did NOT answer is an error and must never read as absent: `mdadm
// --detail` opens the array's members and loads a superblock from one of
// them, so on a leg whose DN side is gone it blocks in the multipath head's
// requeue list until fast_io_fail_tmo expires — past the soft timeout — and
// gets killed. Reading that kill as "the array is not there" is exactly what
// let a teardown skip `mdadm --stop`, leave the array pinning its two leg
// wrappers, and leak them for ever.
//
// Which is also why no sweep calls this: a probe that reads member devices
// can never run in a teardown. ListArrays and Gone answer from sysfs instead.
func (m *Md) Detail(ctx context.Context, dev string) (*MdDetail, error) {
	stdout, ok, err := m.cmd.RunProbe(ctx, "mdadm", "--detail", dev)
	if err != nil {
		return nil, err
	}
	if !ok {
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
	export, ok, err := m.cmd.RunProbe(ctx, "mdadm", "--detail", "--export", dev)
	if err != nil {
		return nil, err
	}
	if ok {
		detail.Devices = parseMdExportDevices(export)
	}
	return detail, nil
}

// ---------------------------------------------------------------------------
// The sysfs view of the node's arrays
//
// Everything the sweep needs about an array it reads from sysfs, which never
// touches a member device and therefore never blocks on a dead leg:
//
//   - `ls /sys/block` names every array node, so a sweep finds arrays it has
//     no plan for — the whole point of deriving removal from the live system;
//   - `/sys/block/mdN/md/array_state` says whether it still holds its
//     members. Only "clear" is gone; "inactive" is an assembled but not
//     running array, which pins them just as hard;
//   - `/sys/block/mdN/md/dev-*/block/dm/name` names each member's dm device,
//     which is what attributes the array to an sp. A member with no such
//     attribute is not a dm device at all, and an array with one is not ours.
//
// `mdadm --detail --scan` would be the obvious enumerator and is deliberately
// not used: it loads superblocks.
// ---------------------------------------------------------------------------

const sysfsBlockDir = "/sys/block"

// mdBlockEntryPattern matches an array's node name under /sys/block. The
// named nodes (/dev/md/<name>) are symlinks and never appear here.
var mdBlockEntryPattern = regexp.MustCompile(`^md[0-9]+$`)

// MdArray is one array as sysfs sees it.
type MdArray struct {
	// Dev is the node the sweep stops: /dev/mdN, the number sysfs gave.
	// Never /dev/md/<name>, which depends on udev having run.
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
// reported; a listing or read that did not answer is an error, because the
// caller would otherwise read it as "no arrays" and sweep on.
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
		members, ok, err := m.cmd.ListDir(ctx, mdDir)
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
			if !strings.HasPrefix(member, "dev-") {
				continue
			}
			dmName, ok, err := m.cmd.ReadAttr(
				ctx, mdDir+"/"+member+"/block/dm/name")
			if err != nil {
				return nil, err
			}
			if !ok {
				array.Foreign = true
				continue
			}
			array.Members = append(array.Members, dmName)
		}
		out = append(out, array)
	}
	return out, nil
}

// Gone verifies a stop. Only an absent sysfs directory or array_state
// "clear" counts; "inactive" is an array that still holds its members and
// would still pin them against a dmsetup remove.
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
//
// A run that did not answer is an error and must never read as "no
// superblock": `--examine` opens the member device exactly as `--detail`
// does, so on a leg whose DN side has gone it blocks until failfast and gets
// killed — and if that happened to every member of an available group,
// assembleGroup would take the answer as case 1 and `mdadm --create
// --assume-clean` over live data.
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
