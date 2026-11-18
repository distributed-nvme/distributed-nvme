package nodecheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// GatherOptions are what a gather is asked for: where the agent's log is,
// a file (--log-file) or a journal unit (--journal-unit); the agent's local
// store; the one deadline of its reads that can wait in the kernel; and
// where to write the archive, "" for a name of the host and the time in the
// working directory.
type GatherOptions struct {
	LogFile     string
	JournalUnit string
	LocalStore  string
	Deadline    time.Duration
	Out         string
}

// gatherWarning is what the gather prints and puts first in the archive.
const gatherWarning = "This archive names hosts, NQNs and addresses of " +
	"the cluster. Read it before you share it."

// DefaultGatherDeadline is the gather's deadline when none is given: the
// one deadline of its reads that can wait in the kernel.
const DefaultGatherDeadline = 20 * time.Second

// gatherWidth is how many detached reads run at once: a read stuck in the
// kernel keeps its place, so the reads behind it run in the others.
const gatherWidth = 8

// RunGather is `dnv-agent collect` (dnagent.md CM6): it gathers the node's
// state into one archive, prints where it wrote it, the warning and each
// read that did not finish on w, and returns nil, or an ExitError of
// ExitNotRun when it could not run or write the archive.
func RunGather(
	ctx context.Context,
	env Env,
	opts GatherOptions,
	w io.Writer,
) error {
	if env.Euid() != 0 {
		return notRun("the gather runs as root: the kernel log and " +
			"nvmet's tree are root's alone")
	}
	if opts.Deadline <= 0 {
		return notRun("--deadline must be above zero, got %s", opts.Deadline)
	}
	g, err := Gather(ctx, env, opts)
	if err != nil {
		return &ExitError{Code: ExitNotRun, Err: err}
	}
	fmt.Fprintf(w, "wrote %s\n%s\n", g.Path, gatherWarning)
	for _, line := range g.Unfinished {
		fmt.Fprintf(w, "not finished: %s\n", line)
	}
	return nil
}

// Gathered is what a gather made: the archive's path, and each read that
// did not finish by the deadline, as the archive's read list names it.
type Gathered struct {
	Path       string
	Unfinished []string
}

// gatherer is one gather: what it reads through, the archive's members, in
// the order read, and the read list.
type gatherer struct {
	env     Env
	cmd     *agent.Cmd
	members []archiveMember
	reads   []string
}

type archiveMember struct {
	name string
	data []byte
}

func (g *gatherer) put(name string, data string) {
	g.members = append(g.members, archiveMember{name: name,
		data: []byte(data)})
}

// note adds one line to the read list: what was read, and how it ended.
func (g *gatherer) note(read string, outcome string) {
	g.reads = append(g.reads, read+": "+outcome)
}

// Gather reads the node in dnagent.md CM6's order and writes one tar.gz,
// built in memory, through the OsClient: the versions; the kernel log; md's
// and nvme's sysfs attributes and the /dev/md links' device numbers, in its
// own process, each attribute one whose show function takes no sleeping
// lock; dmsetup's listing, info and tables; then the reads that can wait in
// the kernel, each a detached child in a process group of its own
// (common.OsClient.StartDetached) — /proc/mdstat, nvmet's configfs tree, the
// nvme controllers' addresses and each dm device's status without a flush —
// collected until the one deadline and never waited for past it, each one
// that did not finish named with what it waits on; last the tail of the
// agent's log and the local store's file names, sizes and times. No read of
// it, in its own process or a child, reads a member device or examines
// one, and it never opens /proc/mdstat, the configfs tree or a dm status
// itself. The archive is its owner's alone (writeArchive). Its error is one
// that leaves no archive.
func Gather(ctx context.Context, env Env, opts GatherOptions) (*Gathered,
	error) {
	if err := refuseDirOut(ctx, env, opts.Out); err != nil {
		return nil, err
	}
	g := &gatherer{env: env, cmd: agent.NewCmd(env.Oc)}
	g.put("README.txt", gatherWarning+"\n\nreads.txt lists every read "+
		"and how it ended.\n")
	g.versions(ctx)
	g.command(ctx, "dmesg.txt", "dmesg")
	g.mdSysfs(ctx)
	g.nvmeSysfs(ctx)
	g.command(ctx, "dmsetup-ls-tree.txt", "dmsetup", "ls", "--tree")
	g.command(ctx, "dmsetup-info.txt", "dmsetup", "info", "-c")
	g.command(ctx, "dmsetup-table.txt", "dmsetup", "table")
	unfinished := g.detachedReads(ctx, opts.Deadline)
	g.agentLog(ctx, opts)
	g.store(ctx, opts.LocalStore)
	g.put("reads.txt", strings.Join(g.reads, "\n")+"\n")

	archive, err := g.archive()
	if err != nil {
		return nil, err
	}
	out := opts.Out
	if out == "" {
		out = g.defaultOut(ctx)
	}
	if err := g.writeArchive(ctx, out, archive); err != nil {
		return nil, fmt.Errorf("write %s: %w", out, err)
	}
	return &Gathered{Path: out, Unfinished: unfinished}, nil
}

// refuseDirOut refuses an --out that names a directory, or a link to one,
// before any read: a listing of it, which follows links, that answers says
// it is one. mv -T, which writeArchive's move runs, refuses a directory too,
// but GNU mv takes a link to one as no directory and replaces the link. A
// listing that fails for another cause than "not a directory" or "not
// there" leaves the move to judge.
func refuseDirOut(ctx context.Context, env Env, out string) error {
	if out == "" {
		return nil
	}
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	if _, err := env.Oc.ReadDir(cctx, out); err == nil {
		return fmt.Errorf("--out %s is a directory: name the archive's file",
			out)
	}
	return nil
}

// writeArchive writes the archive at out with mode 0600: it holds the
// kernel log, which is root's alone on most distributions. OsClient.WriteFile
// makes a file every user can read, so the archive is written into a
// directory of its own, which mktemp makes for its owner alone, beside out,
// made the owner's alone there, and then moved to out, never into it: the
// move takes out as the file's own name (mv -T), so an out that names a
// directory fails the gather rather than putting the archive inside it.
// The directory goes after. A step that fails removes the directory.
func (g *gatherer) writeArchive(ctx context.Context, out string,
	archive []byte) error {
	stdout, stderr, exitCode, err := g.cmd.Run(ctx, "mktemp", "-d",
		filepath.Join(filepath.Dir(out), ".dnv-collect.XXXXXX"))
	if err != nil {
		return fmt.Errorf("mktemp (exit %d): %s %w", exitCode,
			strings.TrimSpace(stderr), err)
	}
	dir := strings.TrimSpace(stdout)
	tmp := filepath.Join(dir, filepath.Base(out))
	cctx, cancel := agent.CmdCtx(ctx)
	err = g.env.Oc.WriteFile(cctx, tmp, string(archive))
	cancel()
	if err == nil {
		err = g.cmd.RunOk(ctx, "chmod", "0600", tmp)
	}
	if err == nil {
		err = g.cmd.RunOk(ctx, "mv", "-f", "-T", tmp, out)
	}
	if err != nil {
		g.cmd.RunOk(context.WithoutCancel(ctx), "rm", "-rf", dir)
		return err
	}
	return g.cmd.RunOk(ctx, "rmdir", dir)
}

// readFile reads one file in the gather's own process under the soft
// timeout, and notes the read.
func (g *gatherer) readFile(ctx context.Context, path string) (string, bool) {
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	data, err := g.env.Oc.ReadFile(cctx, path)
	if err != nil {
		g.note("read "+path, "failed: "+err.Error())
		return "", false
	}
	return data, true
}

// readDir lists one directory in the gather's own process under the soft
// timeout; a listing that fails is noted, never read as empty.
func (g *gatherer) readDir(ctx context.Context, path string) ([]string,
	bool) {
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	names, err := g.env.Oc.ReadDir(cctx, path)
	if err != nil {
		g.note("list "+path, "failed: "+err.Error())
		return nil, false
	}
	return names, true
}

// command runs one command that waits on no device, under the soft timeout
// (dnagent.md SH15), and puts its output in the archive.
func (g *gatherer) command(ctx context.Context, member string, name string,
	args ...string) string {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	stdout, stderr, exitCode, err := g.cmd.Run(ctx, name, args...)
	text := stdout
	if stderr != "" {
		text += "\n[stderr]\n" + stderr
	}
	if err != nil {
		g.note(line, fmt.Sprintf("failed, exit %d: %s", exitCode, err))
	} else {
		g.note(line, "done")
	}
	if member != "" {
		g.put(member, text)
	}
	return text
}

// versions are the kernel's, the distribution's, the tools' and the
// agent's own build.
func (g *gatherer) versions(ctx context.Context) {
	var b strings.Builder
	release, err := agent.KernelRelease(ctx, g.env.Oc)
	if err != nil {
		release = "not read: " + err.Error()
	}
	fmt.Fprintf(&b, "kernel: %s\n", release)
	osRelease, _ := g.readFile(ctx, "/etc/os-release")
	fmt.Fprintf(&b, "\n[/etc/os-release]\n%s\n", osRelease)
	for _, tool := range [][]string{
		{"mdadm", "--version"},
		{"nvme", "version"},
		{"dmsetup", "version"},
		{"thin_dump", "--version"},
	} {
		fmt.Fprintf(&b, "[%s]\n%s\n", strings.Join(tool, " "),
			strings.TrimSpace(g.command(ctx, "", tool[0], tool[1:]...)))
	}
	fmt.Fprintf(&b, "[dnv-agent]\n%s\n", buildText())
	g.put("versions.txt", b.String())
}

// buildText is the agent's own build: its module's version, its source
// revision and the Go it was built with.
func buildText() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "no build information"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, %s\n", info.Main.Path, info.Main.Version,
		info.GoVersion)
	for _, s := range info.Settings {
		if strings.HasPrefix(s.Key, "vcs.") {
			fmt.Fprintf(&b, "%s=%s\n", s.Key, s.Value)
		}
	}
	return b.String()
}

// The md attributes the gather reads in its own process, each checked in
// the kernel's source to take only spinlocks in its show function: an
// array's, and each member's, whose block/dev names the member's device
// number, which dmsetup-info.txt maps to a dm name (a dm device's own
// dm/name read takes a mutex, and is not read).
var (
	mdArrayAttrs = []string{"level", "raid_disks", "array_state",
		"degraded", "sync_action", "sync_completed", "metadata_version",
		"component_size", "uuid"}
	mdMemberAttrs = []string{"state", "slot", "errors", "recovery_start",
		"block/dev"}
	mdNodePattern   = regexp.MustCompile(`^md([0-9]+|_.+)$`)
	mdMemberPattern = regexp.MustCompile(`^dev-`)
	nvmeCtrlPattern = regexp.MustCompile(`^nvme[0-9]+$`)
	nvmePathPattern = regexp.MustCompile(`^nvme[0-9]+c[0-9]+n[0-9]+$`)
	nvmeCtrlAttrs   = []string{"state", "transport", "subsysnqn", "hostnqn"}
	sysBlockDir     = "/sys/block"
	sysClassNvmeDir = "/sys/class/nvme"
)

// attrLines reads each attribute under dir, a line each, an absent one or
// one that fails read as such.
func (g *gatherer) attrLines(ctx context.Context, b *strings.Builder,
	indent string, dir string, attrs []string) {
	for _, attr := range attrs {
		cctx, cancel := agent.CmdCtx(ctx)
		data, err := g.env.Oc.ReadFile(cctx, dir+"/"+attr)
		cancel()
		value := strings.TrimSpace(data)
		if err != nil {
			value = "(" + err.Error() + ")"
		}
		fmt.Fprintf(b, "%s%s: %s\n", indent, attr, value)
	}
}

// devMdDir holds udev's links of the md arrays by name, which give an
// array's name beside its kernel node: the device number each link leads
// to is the "dev" of an array under /sys/block. The gather runs no mdadm
// --detail for the names, since mdadm reads a member's superblock to
// print them.
const devMdDir = "/dev/md"

// mdSysfs is every md array's attributes, with each member's, and the
// /dev/md links with the device number each leads to.
func (g *gatherer) mdSysfs(ctx context.Context) {
	var b strings.Builder
	nodes, ok := g.readDir(ctx, sysBlockDir)
	if !ok {
		b.WriteString("(" + sysBlockDir + " not listed)\n")
	}
	for _, node := range nodes {
		if !mdNodePattern.MatchString(node) {
			continue
		}
		dir := sysBlockDir + "/" + node + "/md"
		fmt.Fprintf(&b, "%s\n", node)
		g.attrLines(ctx, &b, "  ", sysBlockDir+"/"+node, []string{"dev"})
		g.attrLines(ctx, &b, "  ", dir, mdArrayAttrs)
		entries, _ := g.readDir(ctx, dir)
		for _, entry := range entries {
			if !mdMemberPattern.MatchString(entry) {
				continue
			}
			fmt.Fprintf(&b, "  %s\n", entry)
			g.attrLines(ctx, &b, "    ", dir+"/"+entry, mdMemberAttrs)
		}
	}
	links, _ := g.readDir(ctx, devMdDir)
	if len(links) > 0 {
		b.WriteString("/dev/md links\n")
	}
	for _, link := range links {
		cctx, cancel := agent.CmdCtx(ctx)
		devNo, err := g.env.Oc.ReadDevNo(cctx, devMdDir+"/"+link)
		cancel()
		if err != nil {
			devNo = "(" + err.Error() + ")"
		}
		fmt.Fprintf(&b, "  %s -> %s\n", link, devNo)
	}
	g.note("md sysfs", "done")
	g.put("sysfs-md.txt", b.String())
}

// nvmeSysfs is every NVMe controller's state, transport, subsystem and host
// NQN, and each of its paths' ANA state. A controller's address is read
// detached (detachedReads): nvme-tcp's show of it takes the queue's mutex.
func (g *gatherer) nvmeSysfs(ctx context.Context) {
	var b strings.Builder
	ctrls, _ := g.readDir(ctx, sysClassNvmeDir)
	for _, ctrl := range ctrls {
		if !nvmeCtrlPattern.MatchString(ctrl) {
			continue
		}
		dir := sysClassNvmeDir + "/" + ctrl
		fmt.Fprintf(&b, "%s\n", ctrl)
		g.attrLines(ctx, &b, "  ", dir, nvmeCtrlAttrs)
		entries, _ := g.readDir(ctx, dir)
		for _, entry := range entries {
			if nvmePathPattern.MatchString(entry) {
				g.attrLines(ctx, &b, "  "+entry+" ", dir+"/"+entry,
					[]string{"ana_state"})
			}
		}
	}
	g.note("nvme sysfs", "done")
	g.put("sysfs-nvme.txt", b.String())
}

// detachedRead is one read the gather runs as a detached child: the
// archive member its output goes to, the command, how the read list names
// it when not by its command line, and what it waits on when it does not
// finish.
type detachedRead struct {
	member  string
	name    string
	args    []string
	label   string
	waitsOn string
	child   *common.Detached
	err     error
}

func (d *detachedRead) line() string {
	if d.label != "" {
		return d.label
	}
	return strings.TrimSpace(d.name + " " + strings.Join(d.args, " "))
}

// nvmetWalk is the configfs walk, a shell script: it lists the hosts, each
// subsystem's allowed hosts and namespaces with their device paths, enable
// and ANA group, and each port's address, subsystems and ANA groups. It
// never reads an attribute of a subsystem's own directory, since such a
// read moves the directory's time, which the disk node agent's attribution
// of an export judges by (dnagent.md DN6); and it never reads a host's
// keys.
const nvmetWalk = `cd /sys/kernel/config/nvmet || exit 1
echo "hosts: $(ls -1 hosts | tr '\n' ' ')"
for s in $(ls -1 subsystems); do
  echo "subsystem $s"
  echo "  allowed_hosts: $(ls -1 subsystems/$s/allowed_hosts | tr '\n' ' ')"
  for n in $(ls -1 subsystems/$s/namespaces); do
    d=subsystems/$s/namespaces/$n
    echo "  namespace $n: device_path=$(cat $d/device_path) enable=$(cat $d/enable) ana_grpid=$(cat $d/ana_grpid)"
  done
done
for p in $(ls -1 ports); do
  d=ports/$p
  echo "port $p: $(cat $d/addr_trtype) $(cat $d/addr_adrfam) $(cat $d/addr_traddr):$(cat $d/addr_trsvcid)"
  echo "  subsystems: $(ls -1 $d/subsystems | tr '\n' ' ')"
  for a in $(ls -1 $d/ana_groups); do
    echo "  ana_group $a: $(cat $d/ana_groups/$a/ana_state)"
  done
done
`

// nvmeAddresses prints each NVMe controller's address.
const nvmeAddresses = `for f in /sys/class/nvme/nvme*/address; do
  [ -e "$f" ] && echo "$f: $(cat "$f")"
done
`

// detachedReadList is the gather's detached reads: the three whole-node
// ones, then each dm device's status without a flush, in the order of
// their names.
func (g *gatherer) detachedReadList(ctx context.Context) []*detachedRead {
	reads := []*detachedRead{
		{member: "proc-mdstat.txt", name: "cat", args: []string{
			"/proc/mdstat"}, waitsOn: "md's bitmap mutex, which md's " +
			"bitmap daemon holds while it waits for an earlier bitmap " +
			"write stuck at a suspended or dead member"},
		{member: "nvmet-configfs.txt", name: "sh", args: []string{"-c",
			nvmetWalk}, label: "the nvmet configfs walk", waitsOn: "a configfs directory's lock, which a " +
			"mkdir or rmdir in it holds, such as a namespace's disable " +
			"waiting on a suspended device"},
		{member: "nvme-addresses.txt", name: "sh", args: []string{"-c",
			nvmeAddresses}, label: "the nvme controllers' addresses", waitsOn: "an nvme-tcp controller's queue lock"},
	}
	names, err := agent.NewDm(g.env.Oc).List(ctx)
	if err != nil {
		g.note("dmsetup ls", "failed: "+err.Error()+"; no dm status read")
		return reads
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		reads = append(reads, &detachedRead{
			member: "dm-status/" + name + ".txt",
			name:   "dmsetup",
			args:   []string{"status", "--noflush", name},
			waitsOn: "device " + name + ": its target's metadata lock, " +
				"which a commit waiting on IO holds",
		})
	}
	return reads
}

// detachedReads runs the detached reads, gatherWidth at a time, until each
// has ended or the deadline came, then notes how each ended and puts in the
// archive what each wrote so far, and only then kills the group of each
// that did not end, which takes effect once its wait in the kernel ends. It
// returns the read list's line of each that did not finish.
func (g *gatherer) detachedReads(ctx context.Context,
	deadline time.Duration) []string {
	reads := g.detachedReadList(ctx)
	// The children run on kctx, which only kill ends, after the reads are
	// sorted, or the caller's own end (a signal): a kill at the deadline
	// would end a child that waits in a wait a signal ends, such as a
	// shell's for its command, before the read could be named as not
	// finished. dctx is the deadline the gather waits by.
	kctx, kill := context.WithCancel(context.WithoutCancel(ctx))
	defer kill()
	defer context.AfterFunc(ctx, kill)()
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	ended := make(chan int, len(reads))
	next, running := 0, 0
	for {
		for running < gatherWidth && next < len(reads) && dctx.Err() == nil {
			d := reads[next]
			d.child, d.err = g.env.Oc.StartDetached(kctx, d.name, d.args,
				"")
			if d.err == nil {
				running++
				go func(i int, child *common.Detached) {
					select {
					case <-child.Done():
						ended <- i
					case <-dctx.Done():
					}
				}(next, d.child)
			}
			next++
		}
		if running == 0 && next >= len(reads) {
			break
		}
		select {
		case <-ended:
			running--
			continue
		case <-dctx.Done():
		}
		break
	}
	var unfinished, outputs []string
	for _, d := range reads {
		read := d.line()
		switch {
		case d.child == nil && d.err != nil:
			g.note(read, "not started: "+d.err.Error())
			continue
		case d.child == nil:
			g.note(read, "not started: the deadline came first")
			unfinished = append(unfinished, read+" (not started)")
			continue
		}
		outputs = append(outputs, d.child.Output)
		// Whether the child ended is read before its output, so an output
		// read as whole is one the child had ended writing.
		finished := false
		select {
		case <-d.child.Done():
			finished = true
		default:
		}
		data, ok := g.readFile(ctx, d.child.Output)
		if finished {
			code, err := d.child.Exit()
			if err != nil {
				g.note(read, fmt.Sprintf("done, exit %d: %s", code, err))
			} else {
				g.note(read, "done")
			}
		} else {
			line := fmt.Sprintf("did not finish by the %s deadline; "+
				"waits on %s", deadline, d.waitsOn)
			g.note(read, line)
			unfinished = append(unfinished, read+": "+line)
			data += "\n[did not finish by the deadline]\n"
		}
		if ok {
			g.put(d.member, data)
		}
	}
	// The groups still running are killed only now, once each read was
	// named; the kill takes effect when a wait in the kernel ends.
	kill()
	if len(outputs) > 0 {
		g.command(context.WithoutCancel(ctx), "", "rm",
			append([]string{"-f"}, outputs...)...)
	}
	return unfinished
}

// The bounds of the agent's log the gather takes: the last gatherLogBytes
// of a log file, and the last gatherJournalLines records of a journal unit,
// newest first, so the archive holds the most recent records, and its size
// and the memory the gather takes stay bounded on a node whose log has
// grown for months (log.md R2: dnv rotates no log). A read its bound cuts
// keeps what it printed first, which for the journal is the newest, and the
// member says it was cut.
const (
	gatherLogBytes     = 16 << 20
	gatherJournalLines = 100000
)

// agentLog is the tail of the agent's log, from --log-file or
// --journal-unit, read under a bound of the gather's deadline of its own;
// with neither, the archive says it holds none.
func (g *gatherer) agentLog(ctx context.Context, opts GatherOptions) {
	var member, name string
	var args []string
	switch {
	case opts.LogFile != "":
		member, name = "agent.log", "tail"
		args = []string{"-c", strconv.Itoa(gatherLogBytes), opts.LogFile}
	case opts.JournalUnit != "":
		member, name = "agent-journal.log", "journalctl"
		args = []string{"--unit", opts.JournalUnit, "--no-pager",
			"--output", "short-iso-precise", "--reverse", "--lines",
			strconv.Itoa(gatherJournalLines)}
	default:
		g.note("agent log", "not gathered: give --log-file or "+
			"--journal-unit")
		return
	}
	lctx, cancel := context.WithTimeout(ctx, opts.Deadline)
	defer cancel()
	stdout, stderr, exitCode, err := g.env.Oc.RunCommand(lctx, name, args,
		"")
	line := name + " " + strings.Join(args, " ")
	if err != nil {
		g.note(line, fmt.Sprintf("failed, exit %d: %s %s", exitCode,
			strings.TrimSpace(stderr), err))
	} else {
		g.note(line, "done")
	}
	// A read that never reported, and is no tool that is not there, was
	// cut: its bound ended it.
	if err != nil && exitCode == -1 &&
		!errors.Is(err, common.ErrCommandNotFound) {
		stdout += "\n[cut at the bound: the read did not end by itself]\n"
	}
	g.put(member, stdout)
}

// store is the local store's file names, sizes and times; never their
// contents, which name the cluster's objects.
func (g *gatherer) store(ctx context.Context, prefix string) {
	var b strings.Builder
	names, ok := g.readDir(ctx, prefix)
	if !ok {
		g.put("store.txt", "(not listed)\n")
		return
	}
	for _, name := range names {
		path := prefix + "/" + name
		data, _ := g.readFile(ctx, path)
		cctx, cancel := agent.CmdCtx(ctx)
		mtime, err := g.env.Oc.ReadMtime(cctx, path)
		cancel()
		when := mtime.UTC().Format(time.RFC3339)
		if err != nil {
			when = "(" + err.Error() + ")"
		}
		fmt.Fprintf(&b, "%s %d %s\n", name, len(data), when)
	}
	g.note("local store "+prefix, "done")
	g.put("store.txt", b.String())
}

// defaultOut is the archive's name in the working directory when no --out
// is given: the host's name and the time.
func (g *gatherer) defaultOut(ctx context.Context) string {
	host, _ := g.readFile(ctx, "/proc/sys/kernel/hostname")
	host = strings.TrimSpace(host)
	if host == "" {
		host = "node"
	}
	return fmt.Sprintf("dnv-collect-%s-%s.tar.gz", host,
		g.env.Now().UTC().Format("20060102T150405Z"))
}

// archive builds the tar.gz of the members, in the order read, under one
// directory.
func (g *gatherer) archive() ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	now := g.env.Now()
	for _, m := range g.members {
		hdr := &tar.Header{Name: "dnv-collect/" + m.name, Mode: 0o600,
			Size: int64(len(m.data)), ModTime: now}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(m.data); err != nil {
			return nil, err
		}
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
