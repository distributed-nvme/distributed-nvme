package nodecheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// The roles the node check checks a node for: the disk node's and the
// controller node's agent.
const (
	RoleDn = "dn"
	RoleCn = "cn"
)

// The kernel and mdadm versions the node check judges by (dnagent.md CM5).
// A version is compared by its series, never its build, so every build of
// the tested series passes.
const (
	// minKernelMajor and minKernelMinor are the oldest kernel that has every
	// feature both agents use, the newest of them nvmet's attr_qid_max: a
	// kernel below fails.
	minKernelMajor = 6
	minKernelMinor = 1
	// testedKernelMajor and testedKernelMinor are the kernel series dnv is
	// tested on: a kernel outside it warns.
	testedKernelMajor = 7
	testedKernelMinor = 0
	// testedMdadmMajor and testedMdadmMinor are the mdadm dnv is tested
	// with: on a controller node another warns, since the cn agent's model
	// of mdadm's start (cnagent.md, Known limits) is checked against this
	// one alone.
	testedMdadmMajor = 4
	testedMdadmMinor = 5
	// neverIoTimeout is nvme_core.io_timeout's largest value, which the
	// kernel reads as "never time a command out".
	neverIoTimeout = "4294967295"
	// defaultIoTimeout is the kernel's default nvme_core.io_timeout, in
	// seconds, the value dnv is tested with.
	defaultIoTimeout = "30"
	// minUdevVersion is the oldest udev, by systemd's version number, that
	// takes a link removal (SYMLINK-=), which the shipped md array rule
	// makes: an older one fails.
	minUdevVersion = 253
)

// The paths the node check reads.
const (
	mdParamsDir      = "/sys/module/md_mod/parameters"
	mdFeatureParam   = "check_new_feature"
	ioTimeoutPath    = "/sys/module/nvme_core/parameters/io_timeout"
	multipathPath    = "/sys/module/nvme_core/parameters/multipath"
	sysModuleDir     = "/sys/module"
	modulesDir       = "/lib/modules"
	mdadmMaskProgram = "/sbin/mdadm"
)

// Env is what the node check and the gather read the node through: the
// process's one OsClient, the effective user id, and the clock.
type Env struct {
	Oc   common.OsClient
	Euid func() int
	Now  func() time.Time
}

// CheckOptions are what a node check is asked for: the role, the disk node
// agent's --disk, the agent's --local-store, and, on a controller node, the
// md replay (MdReplay) and the cn agent's --ignore-host-disconnection, which
// the host cut row judges by (hostCutRow).
type CheckOptions struct {
	Role                    string
	Disk                    string
	LocalStore              string
	MdReplay                bool
	IgnoreHostDisconnection bool
}

// RunCheck is `dnv-agent check dn|cn` (dnagent.md CM5): it runs the check,
// prints the report on w in the form output names, and returns nil when
// nothing failed, an ExitError of ExitFailed when a result failed, and one
// of ExitNotRun when the check could not run.
func RunCheck(
	ctx context.Context,
	env Env,
	opts CheckOptions,
	output string,
	w io.Writer,
) error {
	if output != OutputText && output != OutputJson {
		return notRun("--output must be %s or %s, got %q", OutputText,
			OutputJson, output)
	}
	rep, err := Check(ctx, env, opts)
	if err != nil {
		return &ExitError{Code: ExitNotRun, Err: err}
	}
	if err := rep.write(w, output); err != nil {
		return &ExitError{Code: ExitNotRun, Err: err}
	}
	if code := rep.exitCode(); code != ExitOk {
		return &ExitError{Code: code}
	}
	return nil
}

// Check reads the node for the role opts names, each read of the role's
// rows of dnagent.md CM5 in turn, and on a controller node asked for it the
// md replay (cnagent.MdReplay) last. It returns an error, and no report,
// only when it cannot run: not root, or a role it does not know. A read
// that fails is a result, never the check's error.
func Check(ctx context.Context, env Env, opts CheckOptions) (*Report, error) {
	if opts.Role != RoleDn && opts.Role != RoleCn {
		return nil, fmt.Errorf("no node check for role %q", opts.Role)
	}
	if env.Euid() != 0 {
		return nil, errors.New("the node check runs as root: several of " +
			"its reads are root's alone")
	}
	c := &checker{env: env, opts: opts, rep: &Report{Role: opts.Role}}
	cn := opts.Role == RoleCn
	c.rep.add(c.kernelRow(ctx))
	if cn {
		c.rep.add(c.mdFeatureRow(ctx))
	}
	c.rep.add(c.mdadmRow(ctx))
	c.rep.add(c.udevVersionRow(ctx))
	rules, rulesErr := effectiveRules(ctx, env.Oc)
	mask := c.maskRow(ctx, rules, rulesErr)
	c.rep.add(mask)
	for _, name := range []string{dmRuleName, mdMaskRuleName,
		mdArrayRuleName} {
		c.rep.add(c.shippedRuleRow(name, rules, rulesErr))
	}
	c.rep.add(c.lvmRow(ctx))
	c.rep.add(c.mdadmConfRow(ctx))
	mods := c.readModules(ctx)
	c.rep.add(c.modulesRow(ctx, mods))
	c.rep.add(c.bootModulesRow(ctx, mods))
	c.rep.add(c.commandsRow(ctx))
	c.rep.add(c.ioTimeoutRow(ctx))
	if cn {
		c.rep.add(c.hostCutRow(ctx))
	} else {
		c.rep.add(c.diskBlockRow(ctx))
		c.rep.add(c.diskIoSizeRow(ctx))
	}
	if cn && opts.MdReplay {
		c.replay(ctx, mask)
	}
	return c.rep, nil
}

// checker is one node check: what it reads through, what it was asked, and
// the report it builds.
type checker struct {
	env  Env
	opts CheckOptions
	rep  *Report
	// release is the kernel release kernelRow read, "" when it read none.
	release string
}

// readFile reads a file in the check's own process under the soft timeout
// (dnagent.md SH15). ok is false only for a file that does not exist.
func (c *checker) readFile(
	ctx context.Context,
	path string,
) (string, bool, error) {
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	data, err := c.env.Oc.ReadFile(cctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return data, true, nil
}

// readDir lists a directory in the check's own process under the soft
// timeout. ok is false only for a directory that does not exist.
func (c *checker) readDir(
	ctx context.Context,
	path string,
) ([]string, bool, error) {
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	names, err := c.env.Oc.ReadDir(cctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return names, true, nil
}

// kernelRow is the kernel release: below the feature floor fails, outside
// the tested series warns.
func (c *checker) kernelRow(ctx context.Context) Result {
	res := Result{Name: "kernel release"}
	release, err := agent.KernelRelease(ctx, c.env.Oc)
	if err != nil {
		res.Level, res.Detail = LevelFail, "not read: "+err.Error()
		res.Fix = "the check reads " + agent.KernelReleasePath +
			"; check that procfs is mounted"
		return res
	}
	c.release = release
	major, minor, ok := agent.KernelSeries(release)
	tested := fmt.Sprintf("%d.%d", testedKernelMajor, testedKernelMinor)
	switch {
	case !ok:
		res.Level = LevelFail
		res.Detail = fmt.Sprintf("release %q names no version", release)
		res.Fix = "run a kernel of the tested " + tested + " series"
	case versionBelow(major, minor, minKernelMajor, minKernelMinor):
		res.Level = LevelFail
		res.Detail = fmt.Sprintf("%s is older than %d.%d, the oldest "+
			"kernel with every feature the agents use", release,
			minKernelMajor, minKernelMinor)
		res.Fix = "run a kernel of the tested " + tested + " series"
	case major != testedKernelMajor || minor != testedKernelMinor:
		res.Level = LevelWarn
		res.Detail = fmt.Sprintf("%s is outside the tested %s series",
			release, tested)
		res.Fix = "run a kernel of the " + tested + " series, or run the " +
			"cn suite on this series before you trust it"
	default:
		res.Level = LevelOk
		res.Detail = fmt.Sprintf("%s, the tested %s series", release, tested)
	}
	return res
}

func versionBelow(major, minor, wantMajor, wantMinor int) bool {
	return major < wantMajor || (major == wantMajor && minor < wantMinor)
}

// mdFeatureRow says which side of the 6.19 line the controller node's md is
// on, by md_mod's check_new_feature parameter, which kernels from 6.19 have:
// they write an array's logical block size into its superblock, and a kernel
// before 6.19 refuses such an array (cnagent.md, Known limits). A kernel
// without the parameter warns, since a failover can strand its arrays on a
// newer node's, or a newer node's on it.
func (c *checker) mdFeatureRow(ctx context.Context) Result {
	res := Result{Name: "md format (the 6.19 line)"}
	names, ok, err := c.readDir(ctx, mdParamsDir)
	sameSeries := "run one kernel series on every controller node of " +
		"the cluster"
	switch {
	case err != nil:
		res.Level, res.Detail = LevelWarn, "not read: "+err.Error()
		res.Fix = "read " + mdParamsDir + " by hand (" + mdFeatureParam +
			" there means 6.19 or newer); " + sameSeries
	case !ok:
		res.Level = LevelWarn
		res.Detail = "not read: md_mod is not loaded, so the side of the " +
			"line is not known"
		res.Fix = "load md (modprobe raid1), then check again or read " +
			mdParamsDir + " by hand; " + sameSeries
	case contains(names, mdFeatureParam):
		res.Level = LevelOk
		res.Detail = "6.19 or newer: an array made here is refused by a " +
			"kernel before 6.19"
	default:
		res.Level = LevelWarn
		res.Detail = "before 6.19: this kernel refuses an array a kernel " +
			"from 6.19 made"
		res.Fix = sameSeries
	}
	return res
}

// mdadmRow is mdadm. On a disk node only the md assembly mask's rule runs
// it, by an absolute path, so that path must run; on a controller node the
// agent runs it by name, which must run, and a version other than the
// tested one warns.
func (c *checker) mdadmRow(ctx context.Context) Result {
	res := Result{Name: "mdadm"}
	tested := fmt.Sprintf("%d.%d", testedMdadmMajor, testedMdadmMinor)
	if c.opts.Role == RoleDn {
		line, err := agent.MdadmVersion(ctx, c.env.Oc, mdadmMaskProgram)
		if err != nil {
			res.Level, res.Detail = LevelFail, err.Error()
			res.Fix = "install mdadm: the md assembly mask's rule runs " +
				mdadmMaskProgram
			return res
		}
		res.Level, res.Detail = LevelOk, line
		return res
	}
	line, err := agent.MdadmVersion(ctx, c.env.Oc, "mdadm")
	if err != nil {
		res.Level, res.Detail = LevelFail, err.Error()
		res.Fix = "install mdadm " + tested
		return res
	}
	major, minor, ok := agent.MdadmSeries(line)
	switch {
	case !ok:
		res.Level = LevelWarn
		res.Detail = fmt.Sprintf("version line %q names no version", line)
		res.Fix = "install mdadm " + tested
	case major != testedMdadmMajor || minor != testedMdadmMinor:
		res.Level = LevelWarn
		res.Detail = fmt.Sprintf("%s: not the tested %s", line, tested)
		res.Fix = "install mdadm " + tested + ", or run `dnv-agent check " +
			"cn --md-replay` on a spare machine with this mdadm and the " +
			"cn suite before you trust it"
	default:
		res.Level, res.Detail = LevelOk, line
	}
	return res
}

// udevVersionPattern is the version number `udevadm --version` prints
// first: "255" of "255", "257" of "257.4-1ubuntu3".
var udevVersionPattern = regexp.MustCompile(`^([0-9]+)`)

// udevVersionRow is udev's version, by `udevadm --version`, on both roles:
// udev reads the shipped rules, and the md array rule removes links
// (SYMLINK-=), which udev takes from minUdevVersion on, so an older one
// fails, and so does a udevadm that does not run.
func (c *checker) udevVersionRow(ctx context.Context) Result {
	res := Result{Name: "udev version"}
	res.Fix = fmt.Sprintf("run systemd %d or newer: the shipped udev "+
		"rules need its udev", minUdevVersion)
	stdout, stderr, exitCode, err := agent.NewCmd(c.env.Oc).Run(ctx,
		"udevadm", "--version")
	if err != nil {
		res.Level = LevelFail
		res.Detail = fmt.Sprintf("udevadm --version did not answer "+
			"(exit %d): %s %v", exitCode, strings.TrimSpace(stderr), err)
		return res
	}
	text := strings.TrimSpace(stdout)
	m := udevVersionPattern.FindStringSubmatch(text)
	if m == nil {
		res.Level = LevelFail
		res.Detail = fmt.Sprintf("udevadm --version printed %q, no "+
			"version", text)
		return res
	}
	version, _ := strconv.Atoi(m[1])
	if version < minUdevVersion {
		res.Level = LevelFail
		res.Detail = fmt.Sprintf("udev %d is older than %d, which the "+
			"shipped md array rule's link removal needs", version,
			minUdevVersion)
		return res
	}
	res.Level, res.Detail, res.Fix = LevelOk, "udev "+text, ""
	return res
}

// ioTimeoutRow is nvme_core.io_timeout: the only bound on a write to a
// stalled disk, so "never" fails on a controller node, whose legs are NVMe
// connections, and warns on a disk node, an NVMe host only for a
// migration's new side; on a controller node another value than the
// default warns.
func (c *checker) ioTimeoutRow(ctx context.Context) Result {
	res := Result{Name: "nvme_core.io_timeout"}
	text, ok, err := c.readFile(ctx, ioTimeoutPath)
	value := strings.TrimSpace(text)
	fix := "set nvme_core.io_timeout to " + defaultIoTimeout +
		" on the kernel command line or in /etc/modprobe.d, then reboot"
	switch {
	case err != nil:
		res.Level, res.Detail = LevelWarn, "not read: "+err.Error()
		res.Fix = "read " + ioTimeoutPath + " by hand"
	case !ok:
		res.Level = LevelWarn
		res.Detail = "not read: nvme_core is not loaded (see modules)"
		res.Fix = "load nvme_core (modprobe nvme-tcp), then check again " +
			"or read " + ioTimeoutPath + " by hand"
	case value == neverIoTimeout && c.opts.Role == RoleCn:
		res.Level = LevelFail
		res.Detail = value + ": a write to a stalled disk never times out"
		res.Fix = fix
	case value == neverIoTimeout:
		res.Level = LevelWarn
		res.Detail = value + ": a migration's write to a stalled disk " +
			"never times out"
		res.Fix = fix
	case value != defaultIoTimeout && c.opts.Role == RoleCn:
		res.Level = LevelWarn
		res.Detail = value + " seconds: not the tested " + defaultIoTimeout
		res.Fix = fix
	default:
		res.Level, res.Detail = LevelOk, value+" seconds"
	}
	return res
}

// hostCutRow is the cut of a removed host's nvmet connection (cnagent.md
// CN33), read through agent.HostCutCauses, the read of the cn agent's start
// check (cnagent.md CN-CM2), so the two never disagree. A cause it finds
// fails the row, since the agent refuses to start there; where the check is
// given --ignore-host-disconnection, as the agent would be, it warns, a
// known limit: the agent then runs without the cut.
func (c *checker) hostCutRow(ctx context.Context) Result {
	res := Result{Name: "nvmet host cut (debugfs)"}
	causes := agent.HostCutCauses(ctx, c.env.Oc)
	if len(causes) == 0 {
		res.Level = LevelOk
		res.Detail = "nvmet's debug directory is there and lockdown is off"
		// A missing lockdown file reads as no lockdown, as the start check
		// reads it; the row says so rather than that lockdown is off. The
		// read changes the words alone, never the level.
		if _, ok, err := c.readFile(ctx, agent.LockdownPath); err == nil &&
			!ok {
			res.Detail = "nvmet's debug directory is there; no lockdown " +
				"file at " + agent.LockdownPath + " (securityfs not " +
				"mounted, or no lockdown support), read as no lockdown: a " +
				"lockdown in force then shows only as a refused cut"
		}
		return res
	}
	res.Fix = "run an nvmet built with CONFIG_NVME_TARGET_DEBUGFS on a " +
		"kernel of 6.11 or newer, load it, mount configfs at " +
		"/sys/kernel/config and debugfs at " + agent.DebugfsDir +
		", and boot without lockdown; or start the cn " +
		"agent with --ignore-host-disconnection, under which a removed " +
		"host keeps its connection until it disconnects"
	if c.opts.IgnoreHostDisconnection {
		res.Level = LevelWarn
		res.Detail = strings.Join(causes, "; ") + ": with " +
			"--ignore-host-disconnection the cn agent runs without the " +
			"cut, and a removed host keeps its connection until it " +
			"disconnects"
		return res
	}
	res.Level = LevelFail
	res.Detail = strings.Join(causes, "; ") + ": the cn agent refuses to " +
		"start here without --ignore-host-disconnection"
	return res
}

// noDisk is the result of a disk read with no disk to read.
func noDisk(name string) Result {
	return Result{Name: name, Level: LevelFail,
		Detail: "no disk named",
		Fix:    "pass the disk node agent's --disk"}
}

// diskBlockRow is the disk's logical block size, read as the agent reads it
// (agent.Dm.DiskSizes): any but common.DnLogicalBlockSize fails, as the
// agent refuses the disk (dnagent.md DN3).
func (c *checker) diskBlockRow(ctx context.Context) Result {
	const name = "disk logical block size"
	if c.opts.Disk == "" {
		return noDisk(name)
	}
	res := Result{Name: name}
	sizes, err := agent.NewDm(c.env.Oc).DiskSizes(ctx, c.opts.Disk)
	switch {
	case sizes.LogicalBlock == 0:
		res.Level, res.Detail = LevelFail, "not read: "+errText(err)
		res.Fix = "name the disk node agent's --disk, a block device"
	case sizes.LogicalBlock != common.DnLogicalBlockSize:
		res.Level = LevelFail
		res.Detail = fmt.Sprintf("%s has %d-byte logical blocks; the "+
			"agent takes %d alone", c.opts.Disk, sizes.LogicalBlock,
			common.DnLogicalBlockSize)
		res.Fix = fmt.Sprintf("format the disk to %d-byte blocks (nvme "+
			"format), or set a loop device up without -b",
			common.DnLogicalBlockSize)
	default:
		res.Level = LevelOk
		res.Detail = fmt.Sprintf("%s: %d bytes", c.opts.Disk,
			sizes.LogicalBlock)
	}
	return res
}

// diskIoSizeRow is the disk's best and minimum IO size (agent.Dm.IoSizes):
// nvmet passes them to every controller node a side of the disk is exported
// to, which raises its largest request to them, and nvmet-tcp refuses a
// command over common.MaxNvmeTcpCmdBytes, so one above warns.
func (c *checker) diskIoSizeRow(ctx context.Context) Result {
	const name = "disk best IO size"
	if c.opts.Disk == "" {
		return noDisk(name)
	}
	res := Result{Name: name}
	optimal, minimum, err := agent.NewDm(c.env.Oc).IoSizes(ctx, c.opts.Disk)
	if err != nil {
		res.Level, res.Detail = LevelWarn, "not read: "+err.Error()
		res.Fix = "read the disk's queue/optimal_io_size and " +
			"queue/minimum_io_size by hand"
		return res
	}
	detail := fmt.Sprintf("%s: best %d bytes, minimum %d bytes",
		c.opts.Disk, optimal, minimum)
	if optimal > common.MaxNvmeTcpCmdBytes ||
		minimum > common.MaxNvmeTcpCmdBytes {
		res.Level = LevelWarn
		res.Detail = fmt.Sprintf("%s, above the %d bytes an NVMe/TCP "+
			"command carries: a controller node's write can be refused",
			detail, common.MaxNvmeTcpCmdBytes)
		res.Fix = "give the disk a smaller stripe (a hardware RAID " +
			"volume's), or use another disk"
		return res
	}
	res.Level, res.Detail = LevelOk, detail
	return res
}

func errText(err error) string {
	if err == nil {
		return "no answer"
	}
	return err.Error()
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
