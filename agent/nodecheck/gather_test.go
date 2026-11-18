package nodecheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// gatherTree is a node for the gather: an md array over a dnv dm device,
// with its /dev/md link, one NVMe controller with a path, a local store,
// and dm devices by name. Each detached child writes its output and ends
// at once, but one whose command line stuck names, which ends only when its
// ctx ends, as a child whose wait a signal ends does once its group is
// killed: a shell's for its command, or a configfs listing's.
func gatherTree(dms []string, stuck ...string) *tree {
	t := newTree()
	f := t.files
	f["/proc/sys/kernel/osrelease"] = labRelease + "\n"
	f["/proc/sys/kernel/hostname"] = "cn2\n"
	f["/etc/os-release"] = "PRETTY_NAME=\"Ubuntu 26.04 LTS\"\n"
	t.cmd("mdadm --version", "", "mdadm - v4.5\n", 0)
	t.cmd("nvme version", "nvme version 2.16\n", "", 0)
	t.cmd("dmsetup version", "Library version: 1.02.205\n", "", 0)
	t.cmd("thin_dump --version", "thin_dump 1.1.0\n", "", 0)
	t.cmd("dmesg", "[    0.000000] Linux version 7.0.0\n", "", 0)
	t.cmd("dmsetup ls --tree", "dnv-a (253:0)\n", "", 0)
	t.cmd("dmsetup info -c", "Name Maj Min\n", "", 0)
	t.cmd("dmsetup table", "dnv-a: 0 8 linear 7:0 0\n", "", 0)
	var ls strings.Builder
	for i, name := range dms {
		fmt.Fprintf(&ls, "%s\t(253:%d)\n", name, i)
	}
	if len(dms) == 0 {
		ls.WriteString("No devices found\n")
	}
	t.cmd("dmsetup ls", ls.String(), "", 0)
	md := "/sys/block/md127/md/"
	for _, attr := range mdArrayAttrs {
		f[md+attr] = attr + "-value\n"
	}
	for _, attr := range mdMemberAttrs {
		f[md+"dev-dm-0/"+attr] = attr + "-value\n"
	}
	nvme := "/sys/class/nvme/nvme0/"
	for _, attr := range nvmeCtrlAttrs {
		f[nvme+attr] = attr + "-value\n"
	}
	f[nvme+"address"] = "traddr=10.0.0.1,trsvcid=4420\n"
	f[nvme+"nvme0c0n1/ana_state"] = "optimized\n"
	f["/var/lib/dnv/cn-0000000000000001-0000000000000011"] = "proto"
	link := devMdDir + "/" + "0000000100000000000003b10000"
	t.dirs[devMdDir] = true
	f[link] = ""
	t.devNos[link] = "9:127"
	t.euid = 0
	var mu sync.Mutex
	n := 0
	t.detachFn = func(ctx context.Context, name string, args []string) (
		*common.Detached, error) {
		line := strings.TrimSpace(name + " " + strings.Join(args, " "))
		mu.Lock()
		n++
		out := fmt.Sprintf("/tmp/dnv-detached-%d.out", n)
		pid := 1000 + n
		mu.Unlock()
		child, end := common.NewDetached(pid, out)
		t.mu.Lock()
		t.files[out] = "output of " + line + "\n"
		t.mu.Unlock()
		for _, s := range stuck {
			if line == s {
				context.AfterFunc(ctx, func() {
					end(-1, errors.New("signal: killed"))
				})
				return child, nil
			}
		}
		end(0, nil)
		return child, nil
	}
	return t
}

// unpack reads the archive the gather wrote, member name to text.
func unpack(t *testing.T, data string) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		out[strings.TrimPrefix(hdr.Name, "dnv-collect/")] = string(body)
	}
	return out
}

func gather(t *testing.T, tr *tree, opts GatherOptions) (*Gathered,
	map[string]string, time.Duration) {
	t.Helper()
	if opts.Deadline == 0 {
		opts.Deadline = 200 * time.Millisecond
	}
	if opts.LocalStore == "" {
		opts.LocalStore = "/var/lib/dnv"
	}
	start := time.Now()
	g, err := Gather(context.Background(), tr.env(), opts)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	tr.mu.Lock()
	data, ok := tr.written[g.Path]
	tr.mu.Unlock()
	if !ok {
		t.Fatalf("no archive written at %s: %v", g.Path, tr.written)
	}
	return g, unpack(t, data), took
}

// A dm status that never returns, as one stuck behind a thin pool's commit
// waiting on a dead leg: the gather ends by its deadline, never waits for
// the read, and names the read and the device in its read list.
func TestGatherEndsByItsDeadlineNamingTheStuckRead(t *testing.T) {
	stuck := "dmsetup status --noflush dnv-b"
	tr := gatherTree([]string{"dnv-a", "dnv-b", "dnv-c"}, stuck)
	g, files, took := gather(t, tr, GatherOptions{})
	if took > 3*time.Second {
		t.Errorf("the gather took %s past a 200ms deadline", took)
	}
	if len(g.Unfinished) != 1 || !strings.Contains(g.Unfinished[0], stuck) {
		t.Errorf("unfinished = %v, want the one stuck read", g.Unfinished)
	}
	reads := files["reads.txt"]
	if !strings.Contains(reads, stuck+": did not finish by the 200ms "+
		"deadline; waits on device dnv-b") {
		t.Errorf("reads.txt does not name the stuck read:\n%s", reads)
	}
	for _, name := range []string{"dnv-a", "dnv-c"} {
		if !strings.Contains(reads, "dmsetup status --noflush "+name+
			": done") {
			t.Errorf("reads.txt does not show %s done:\n%s", name, reads)
		}
		if files["dm-status/"+name+".txt"] == "" {
			t.Errorf("no status of %s in the archive", name)
		}
	}
	if !strings.Contains(files["dm-status/dnv-b.txt"],
		"[did not finish by the deadline]") {
		t.Errorf("the stuck read's member: %q", files["dm-status/dnv-b.txt"])
	}
}

// A stuck read whose child the group kill ends, as a shell's wait for its
// command, is still named as not finished: the gather kills the groups only
// once it has sorted the reads, and it does kill them.
func TestGatherKillsOnlyAfterNamingTheStuckReads(t *testing.T) {
	walk := strings.TrimSpace("sh -c " + nvmetWalk)
	tr := gatherTree([]string{"dnv-a"}, walk)
	var walkChild *common.Detached
	inner := tr.detachFn
	tr.detachFn = func(ctx context.Context, name string, args []string) (
		*common.Detached, error) {
		child, err := inner(ctx, name, args)
		if strings.TrimSpace(name+" "+strings.Join(args, " ")) == walk {
			walkChild = child
		}
		return child, err
	}
	g, files, _ := gather(t, tr, GatherOptions{})
	if len(g.Unfinished) != 1 ||
		!strings.Contains(g.Unfinished[0], "the nvmet configfs walk") {
		t.Errorf("unfinished = %v, want the configfs walk", g.Unfinished)
	}
	if !strings.Contains(files["reads.txt"], "the nvmet configfs walk: "+
		"did not finish by the") {
		t.Errorf("reads.txt does not name the walk:\n%s", files["reads.txt"])
	}
	if walkChild == nil {
		t.Fatal("the walk was not started")
	}
	// The kill ends the fake child from a goroutine of its own, as exec's
	// does a real one, so its end is waited for, within a bound.
	select {
	case <-walkChild.Done():
	case <-time.After(2 * time.Second):
		t.Error("the gather left the walk's group unkilled")
	}
}

// Every read that can wait in the kernel runs detached; the gather's own
// process reads only sysfs attributes and files that wait on no device:
// never /proc/mdstat, nvmet's configfs tree, a dm status, an nvme
// controller's address, a member device or an examine.
func TestGatherReadsNothingThatCanWaitItself(t *testing.T) {
	tr := gatherTree([]string{"dnv-a", "dnv-b"})
	gather(t, tr, GatherOptions{})
	for _, read := range tr.reads {
		for _, banned := range []string{"/proc/mdstat",
			"/sys/kernel/config", "/address", "block "} {
			if strings.Contains(read, banned) {
				t.Errorf("the gather's own process ran %q", read)
			}
		}
		if strings.HasPrefix(read, "block") {
			t.Errorf("the gather read a block device: %q", read)
		}
	}
	for _, line := range tr.commands {
		for _, banned := range []string{"dmsetup status", "/proc/mdstat",
			"mdadm --detail", "--examine", "/sys/kernel/config"} {
			if strings.Contains(line, banned) {
				t.Errorf("the gather waited on %q", line)
			}
		}
	}
	want := []string{"cat /proc/mdstat",
		"dmsetup status --noflush dnv-a", "dmsetup status --noflush dnv-b"}
	for _, line := range want {
		found := false
		for _, d := range tr.detached {
			found = found || d == line
		}
		if !found {
			t.Errorf("%q was not run detached: %v", line, tr.detached)
		}
	}
	walk, addresses := false, false
	for _, d := range tr.detached {
		walk = walk || strings.Contains(d, "/sys/kernel/config/nvmet")
		addresses = addresses || strings.Contains(d, "/address")
		if strings.Contains(d, "--examine") || strings.Contains(d, "mdadm") {
			t.Errorf("an mdadm read of the arrays ran, which reads a "+
				"member's superblock: %q", d)
		}
		if strings.HasPrefix(d, "dmsetup status") &&
			!strings.Contains(d, "--noflush") {
			t.Errorf("a status read with a flush: %q", d)
		}
	}
	if !walk || !addresses {
		t.Errorf("the configfs walk (%v) or the addresses (%v) did not "+
			"run detached: %v", walk, addresses, tr.detached)
	}
}

// The configfs walk reads no attribute of a subsystem's own directory,
// which would move the directory's time, and no host's key.
func TestTheConfigfsWalkLeavesSubsystemTimesAlone(t *testing.T) {
	for _, line := range strings.Split(nvmetWalk, "\n") {
		if strings.Contains(line, "attr_") ||
			strings.Contains(line, "dhchap") ||
			strings.Contains(line, "subsystems/$s/attr") {
			t.Errorf("the walk reads %q", line)
		}
	}
	if strings.Contains(nvmetWalk, "ls -l") {
		t.Error("the walk stats each attribute (ls -l)")
	}
}

// The archive holds every member in order, the warning first, and the
// gather prints where it wrote it and the warning.
func TestGatherArchive(t *testing.T) {
	tr := gatherTree([]string{"dnv-a"})
	tr.files["/var/log/dnv-cn.log"] = `{"msg":"agent started"}` + "\n"
	g, files, _ := gather(t, tr, GatherOptions{LogFile: "/var/log/dnv-cn.log"})
	if g.Path != "dnv-collect-cn2-20260102T030405Z.tar.gz" {
		t.Errorf("default path = %s", g.Path)
	}
	// The archive is its owner's alone before it reaches its path: written
	// into a private directory, made 0600 there, then moved.
	if tr.modes[g.Path] != "0600" || len(tr.writtenAt) != 1 ||
		tr.writtenAt[0] != ".dnv-collect.abc123/"+g.Path {
		t.Errorf("archive mode %q, written at %v", tr.modes[g.Path],
			tr.writtenAt)
	}
	// The log is its tail, by tail, never read whole in process.
	tail := fmt.Sprintf("tail -c %d /var/log/dnv-cn.log", gatherLogBytes)
	found := false
	for _, line := range tr.commands {
		found = found || line == tail
	}
	if !found {
		t.Errorf("no %q: %v", tail, tr.commands)
	}
	for _, read := range tr.reads {
		if strings.Contains(read, "/var/log/dnv-cn.log") {
			t.Errorf("the log was read whole in process: %q", read)
		}
	}
	for name, want := range map[string]string{
		"README.txt":          gatherWarning,
		"versions.txt":        "mdadm - v4.5",
		"dmesg.txt":           "Linux version",
		"sysfs-md.txt":        "0000000100000000000003b10000 -> 9:127",
		"sysfs-nvme.txt":      "nvme0c0n1 ana_state: optimized",
		"dmsetup-table.txt":   "linear",
		"proc-mdstat.txt":     "output of cat /proc/mdstat",
		"nvmet-configfs.txt":  "output of sh -c",
		"agent.log":           "agent started",
		"store.txt":           "cn-0000000000000001-0000000000000011 5 ",
		"dm-status/dnv-a.txt": "output of dmsetup status --noflush dnv-a",
	} {
		if !strings.Contains(files[name], want) {
			t.Errorf("%s = %q, want it to hold %q", name, files[name], want)
		}
	}
	if strings.Contains(files["sysfs-md.txt"], "dm/name") {
		t.Error("the gather read a dm device's name in its own process")
	}
	if strings.Contains(files["store.txt"], "proto") {
		t.Error("the archive holds a store file's contents")
	}
	var out bytes.Buffer
	tr = gatherTree(nil)
	err := RunGather(context.Background(), tr.env(), GatherOptions{
		Deadline: time.Second, LocalStore: "/var/lib/dnv",
		Out: "/tmp/x.tar.gz"}, &out)
	if err != nil {
		t.Fatalf("RunGather: %v", err)
	}
	if !strings.Contains(out.String(), "wrote /tmp/x.tar.gz\n"+gatherWarning) {
		t.Errorf("printed %q", out.String())
	}
	files = unpack(t, tr.written["/tmp/x.tar.gz"])
	if !strings.Contains(files["reads.txt"], "agent log: not gathered") {
		t.Errorf("reads.txt does not say the log was not gathered:\n%s",
			files["reads.txt"])
	}
	if tr.writtenAt[0] != "/tmp/.dnv-collect.abc123/x.tar.gz" {
		t.Errorf("written at %v, want a private directory beside --out",
			tr.writtenAt)
	}

	// A journal unit's log is its most recent records, newest first.
	tr = gatherTree(nil)
	tr.cmd(journalLine, "the latest record\n", "", 0)
	_, files, _ = gather(t, tr, GatherOptions{JournalUnit: "dnv-cn"})
	if files["agent-journal.log"] != "the latest record\n" {
		t.Errorf("agent-journal.log = %q, want the bounded journal read",
			files["agent-journal.log"])
	}
}

// journalLine is the gather's read of the journal unit dnv-cn.
var journalLine = "journalctl --unit dnv-cn --no-pager --output " +
	"short-iso-precise --reverse --lines " + strconv.Itoa(gatherJournalLines)

// A log read its bound cut keeps what it printed, the newest records first,
// and the member says it was cut.
func TestGatherMarksACutLog(t *testing.T) {
	tr := gatherTree(nil)
	tr.cmd(journalLine, "the latest record\n", "", -1)
	_, files, _ := gather(t, tr, GatherOptions{JournalUnit: "dnv-cn"})
	got := files["agent-journal.log"]
	if !strings.HasPrefix(got, "the latest record\n") ||
		!strings.Contains(got, "[cut at the bound") {
		t.Errorf("agent-journal.log = %q, want the newest record and the "+
			"cut's mark", got)
	}
	tr = gatherTree(nil)
	tr.cmd(journalLine, "the latest record\n", "", 0)
	_, files, _ = gather(t, tr, GatherOptions{JournalUnit: "dnv-cn"})
	if strings.Contains(files["agent-journal.log"], "[cut at the bound") {
		t.Error("a read that ended by itself is marked cut")
	}
	// A journalctl that is not installed is no cut read: the read list
	// names the failure, and the member carries no cut's mark.
	tr = gatherTree(nil)
	_, files, _ = gather(t, tr, GatherOptions{JournalUnit: "dnv-cn"})
	if strings.Contains(files["agent-journal.log"], "[cut at the bound") {
		t.Errorf("a missing journalctl is marked cut: %q",
			files["agent-journal.log"])
	}
	if !strings.Contains(files["reads.txt"], journalLine+": failed") {
		t.Errorf("reads.txt does not name the failed journal read:\n%s",
			files["reads.txt"])
	}
}

// An --out that names a directory, or a link to one, which a listing
// follows, is refused before the gather reads anything.
func TestGatherRefusesADirectoryOutFirst(t *testing.T) {
	tr := gatherTree(nil)
	tr.dirs["/root/reports"] = true
	_, err := Gather(context.Background(), tr.env(), GatherOptions{
		Deadline: time.Second, LocalStore: "/var/lib/dnv",
		Out: "/root/reports"})
	if err == nil {
		t.Fatal("an --out that is a directory was taken")
	}
	if len(tr.commands)+len(tr.detached)+len(tr.written) != 0 ||
		len(tr.reads) != 1 {
		t.Errorf("the gather read the node before refusing: %v %v %v %v",
			tr.reads, tr.commands, tr.detached, tr.written)
	}
}

// An --out that names a directory fails the gather, rather than putting the
// archive inside it while the gather says it wrote --out, also when the
// move is the one that meets it (mv -T).
func TestGatherRefusesADirectoryOut(t *testing.T) {
	tr := gatherTree(nil)
	tr.dirs["/var/tmp"] = true
	_, err := Gather(context.Background(), tr.env(), GatherOptions{
		Deadline: time.Second, LocalStore: "/var/lib/dnv",
		Out: "/var/tmp/"})
	if err == nil {
		t.Fatalf("an --out that is a directory was written: %v",
			tr.written)
	}
	for path := range tr.written {
		if strings.HasPrefix(path, "/var/tmp/") &&
			!strings.HasPrefix(path, "/var/tmp/.dnv-collect.") {
			t.Errorf("the archive was put inside the directory at %s", path)
		}
	}
}

// The detached reads run gatherWidth at a time: the three whole-node reads
// end at once and free their places, and past gatherWidth stuck statuses
// the rest are named not started.
func TestGatherNamesReadsNotStarted(t *testing.T) {
	const devices = 20
	var dms, stuck []string
	for i := 0; i < devices; i++ {
		name := fmt.Sprintf("dnv-%02d", i)
		dms = append(dms, name)
		stuck = append(stuck, "dmsetup status --noflush "+name)
	}
	tr := gatherTree(dms, stuck...)
	g, files, _ := gather(t, tr, GatherOptions{})
	if len(tr.detached) != 3+gatherWidth {
		t.Errorf("%d detached starts, want the 3 whole-node reads and %d "+
			"statuses", len(tr.detached), gatherWidth)
	}
	if len(g.Unfinished) != devices {
		t.Errorf("unfinished = %v, want all %d", g.Unfinished, devices)
	}
	if n := strings.Count(files["reads.txt"], "not started: the deadline "+
		"came first"); n != devices-gatherWidth {
		t.Errorf("%d reads not started, want %d:\n%s", n,
			devices-gatherWidth, files["reads.txt"])
	}
}

// The gather runs as root alone, and reads nothing otherwise.
func TestGatherNotRoot(t *testing.T) {
	tr := gatherTree(nil)
	tr.euid = 1000
	err := RunGather(context.Background(), tr.env(),
		GatherOptions{Deadline: time.Second}, io.Discard)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != ExitNotRun {
		t.Errorf("not root: %v, want exit %d", err, ExitNotRun)
	}
	if len(tr.reads)+len(tr.commands)+len(tr.detached) != 0 {
		t.Errorf("read the node: %v %v %v", tr.reads, tr.commands,
			tr.detached)
	}
}
