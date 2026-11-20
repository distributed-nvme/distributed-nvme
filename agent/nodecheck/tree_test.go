package nodecheck

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// tree is a modelled node for the node check and the gather: files and
// directories by path, a block device's number by path, and each command's
// answer by its command line. A directory exists when it is named in dirs
// or a file or directory lies under it. Every in-process read and every
// command is recorded.
type tree struct {
	mu      sync.Mutex
	files   map[string]string
	dirs    map[string]bool
	errs    map[string]error
	devNos  map[string]string
	cmds    map[string]cmdAnswer
	missing map[string]bool
	euid    int

	reads    []string
	commands []string
	written  map[string]string
	// modes is the mode a chmod gave each path, and writtenAt each path
	// WriteFile wrote, in order.
	modes     map[string]string
	writtenAt []string
	detached  []string
	// detachFn answers a detached start, when set.
	detachFn func(ctx context.Context, name string, args []string) (
		*common.Detached, error)
}

type cmdAnswer struct {
	stdout, stderr string
	code           int
}

func newTree() *tree {
	return &tree{files: map[string]string{}, dirs: map[string]bool{},
		errs: map[string]error{}, devNos: map[string]string{},
		cmds: map[string]cmdAnswer{}, missing: map[string]bool{},
		written: map[string]string{}, modes: map[string]string{}}
}

func (t *tree) cmd(line string, stdout, stderr string, code int) {
	t.cmds[line] = cmdAnswer{stdout, stderr, code}
}

func notThere(path string) error {
	return &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

func (t *tree) readFile(path string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads = append(t.reads, "read "+path)
	if err := t.errs[path]; err != nil {
		return "", err
	}
	data, ok := t.files[path]
	if !ok {
		return "", notThere(path)
	}
	return data, nil
}

func (t *tree) readDir(path string) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads = append(t.reads, "list "+path)
	if err := t.errs[path]; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	prefix := path + "/"
	for _, all := range []map[string]bool{t.dirs, keys(t.files)} {
		for p := range all {
			if rest, ok := strings.CutPrefix(p, prefix); ok && rest != "" {
				seen[strings.SplitN(rest, "/", 2)[0]] = true
			}
		}
	}
	if len(seen) == 0 && !t.dirs[path] {
		return nil, notThere(path)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func keys(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func (t *tree) run(name string, args []string) (string, string, int, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	t.mu.Lock()
	t.commands = append(t.commands, line)
	t.mu.Unlock()
	// The PATH lookup of the commands row prints the commands it misses.
	if name == "sh" && len(args) > 3 && args[0] == "-c" &&
		strings.Contains(args[1], "command -v") {
		var out strings.Builder
		for _, c := range args[3:] {
			if t.missing[c] {
				out.WriteString(c + "\n")
			}
		}
		return out.String(), "", 0, nil
	}
	if out, code, handled := t.fileCommand(name, args); handled {
		if code != 0 {
			return "", out, code, fmt.Errorf("exit status %d", code)
		}
		return out, "", 0, nil
	}
	answer, ok := t.cmds[line]
	if !ok {
		// A tool that is not installed never starts, as RunCommand says.
		return "", "", -1, fmt.Errorf("exec: %q: %w", name,
			common.ErrCommandNotFound)
	}
	if answer.code != 0 {
		return answer.stdout, answer.stderr, answer.code,
			fmt.Errorf("exit status %d", answer.code)
	}
	return answer.stdout, answer.stderr, 0, nil
}

// fileCommand answers the commands that act on the tree's files: the
// gather's removal of its outputs, its archive's private directory, mode
// and move, and the tail of a log file. A move onto a directory of the tree
// puts the file inside it, as mv does, but with -T, where mv refuses.
func (t *tree) fileCommand(name string, args []string) (string, int,
	bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch name {
	case "rm", "rmdir":
		return "", 0, true
	case "mktemp":
		return strings.Replace(args[len(args)-1], "XXXXXX", "abc123", 1) +
			"\n", 0, true
	case "chmod":
		t.modes[args[1]] = args[0]
		return "", 0, true
	case "mv":
		from, to := args[len(args)-2], args[len(args)-1]
		if t.dirs[strings.TrimSuffix(to, "/")] {
			if slices.Contains(args, "-T") {
				return "mv: cannot overwrite directory '" + to +
					"' with non-directory", 1, true
			}
			to = strings.TrimSuffix(to, "/") + "/" + path.Base(from)
		}
		t.written[to] = t.written[from]
		t.modes[to] = t.modes[from]
		delete(t.written, from)
		return "", 0, true
	case "tail":
		data := t.files[args[len(args)-1]]
		n, _ := strconv.Atoi(args[1])
		if len(data) > n {
			data = data[len(data)-n:]
		}
		return data, 0, true
	}
	return "", 0, false
}

func (t *tree) env() Env {
	return Env{
		Oc:   t.osClient(),
		Euid: func() int { return t.euid },
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		},
	}
}

func (t *tree) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(_ context.Context, name string, args []string,
			_ string) (string, string, int, error) {
			return t.run(name, args)
		},
		ReadFileFn: func(_ context.Context, path string) (string, error) {
			return t.readFile(path)
		},
		ReadDirFn: func(_ context.Context, path string) ([]string, error) {
			return t.readDir(path)
		},
		ReadDevNoFn: func(_ context.Context, path string) (string, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.reads = append(t.reads, "devno "+path)
			if devNo, ok := t.devNos[path]; ok {
				return devNo, nil
			}
			return "", notThere(path)
		},
		ReadMtimeFn: func(_ context.Context, path string) (time.Time,
			error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.reads = append(t.reads, "mtime "+path)
			return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil
		},
		ReadBlockFn: func(_ context.Context, path string, _, _ uint64) (
			[]byte, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.reads = append(t.reads, "block "+path)
			return nil, fmt.Errorf("no block read here")
		},
		ReadBlockDirectFn: func(_ context.Context, path string, _, _ uint64) (
			[]byte, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.reads = append(t.reads, "block direct "+path)
			return nil, fmt.Errorf("no block read here")
		},
		WriteFileFn: func(_ context.Context, path, data string) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.written[path] = data
			t.writtenAt = append(t.writtenAt, path)
			return nil
		},
		StartDetachedFn: func(ctx context.Context, name string,
			args []string, _ string) (*common.Detached, error) {
			t.mu.Lock()
			line := strings.TrimSpace(name + " " + strings.Join(args, " "))
			t.detached = append(t.detached, line)
			fn := t.detachFn
			t.mu.Unlock()
			if fn == nil {
				return nil, fmt.Errorf("no detached child here")
			}
			return fn(ctx, name, args)
		},
	}
}

// The modules of the lab's guests: loaded now, built in, and shipped.
var (
	labLoaded = []string{"nvmet", "nvmet_tcp", "nvme_fabrics", "nvme_core",
		"nvme_tcp", "dm_mod", "dm_clone", "dm_thin_pool", "dm_flakey",
		"raid1", "md_mod"}
	labShipped = map[string]string{
		"nvmet":        "kernel/drivers/nvme/target/nvmet.ko.zst",
		"nvmet_tcp":    "kernel/drivers/nvme/target/nvmet-tcp.ko.zst",
		"nvme_tcp":     "kernel/drivers/nvme/host/nvme-tcp.ko.zst",
		"nvme_fabrics": "kernel/drivers/nvme/host/nvme-fabrics.ko.zst",
		"nvme_core":    "kernel/drivers/nvme/host/nvme-core.ko.zst",
		"dm_clone":     "kernel/drivers/md/dm-clone.ko.zst",
		"dm_thin_pool": "kernel/drivers/md/dm-thin-pool.ko.zst",
		"dm_flakey":    "kernel/drivers/md/dm-flakey.ko.zst",
		"raid1":        "kernel/drivers/md/raid1.ko.zst",
		"dm_mod":       "kernel/drivers/md/dm-mod.ko.zst",
	}
)

const (
	labRelease = "7.0.0-42-generic"
	labDisk    = "/dev/loop0"
	labDiskNo  = "7:0"
)

// stockAssemblyRule is mdadm's stock udev rule, as Ubuntu ships it, in the
// parts that run its assembly. No row reads it: the lab tree carries it, and
// mdadm's array rule beside it, as a node does, so that a test can show that
// hiding it is no mask the check counts.
const stockAssemblyRule = `SUBSYSTEM!="block", GOTO="md_inc_end"
ENV{SYSTEMD_READY}=="0", GOTO="md_inc_end"
ENV{ID_FS_TYPE}=="linux_raid_member", GOTO="md_inc"
GOTO="md_inc_end"
LABEL="md_inc"
ACTION!="remove", IMPORT{program}="/usr/sbin/mdadm --incremental --export $devnode --offroot $env{DEVLINKS}"
LABEL="md_inc_end"
`

const thinDumpHelp = `Usage: thin_dump [OPTIONS] <INPUT>
      --dev-id <DEV_ID>
  -m, --metadata-snap [<BLOCKNR>]
      --skip-mappings
`

// lvmSettings is lvm2's effective devices settings, as lvmconfig prints
// them.
func lvmSettings(t *tree, use, filter, global string) {
	t.cmd("lvmconfig "+strings.Join(lvmConfigArgs, " "),
		"use_devicesfile="+use+"\nfilter="+filter+"\nglobal_filter="+global+
			"\n", "", 0)
}

// labTree is a node that passes every read of the node check for both
// roles, as a lab guest set up as the procedures say.
func labTree() *tree {
	t := newTree()
	f := t.files
	f["/proc/sys/kernel/osrelease"] = labRelease + "\n"
	f[mdParamsDir+"/"+mdFeatureParam] = "Y\n"
	mdadmLine := "mdadm - v4.5 - Ubuntu 4.5-5ubuntu1\n"
	t.cmd("mdadm --version", "", mdadmLine, 0)
	t.cmd("/sbin/mdadm --version", "", mdadmLine, 0)
	f["/usr/lib/udev/rules.d/64-md-raid-assembly.rules"] = stockAssemblyRule
	f["/usr/lib/udev/rules.d/63-md-raid-arrays.rules"] =
		`IMPORT{program}="/usr/sbin/mdadm --detail --no-devices --export $devnode"` + "\n"
	for _, name := range []string{dmRuleName, mdMaskRuleName,
		mdArrayRuleName} {
		f["/etc/udev/rules.d/"+name] = shippedRules[name]
	}
	f["/etc/mdadm/mdadm.conf"] = "HOMEHOST <system>\nMAILADDR root\n" +
		"AUTO -all\n"
	t.cmd("udevadm --version", "257\n", "", 0)
	lvmSettings(t, "1", `["a|.*|"]`, `["a|.*|"]`)
	f[lvmDevicesFile] = "# LVM uses devices listed in this file.\n"
	t.dirs["/sys/block"] = true
	f["/sys/block/vda/dev"] = "252:0\n"
	for _, name := range labLoaded {
		t.dirs[sysModuleDir+"/"+name] = true
	}
	modDir := modulesDir + "/" + labRelease
	f[modDir+"/modules.builtin"] = "kernel/drivers/md/md-mod.ko\n"
	var dep strings.Builder
	for name, file := range labShipped {
		deps := ""
		switch name {
		case "nvme_tcp":
			deps = " " + labShipped["nvme_fabrics"] + " " +
				labShipped["nvme_core"]
		case "nvme_fabrics":
			deps = " " + labShipped["nvme_core"]
		case "nvmet_tcp":
			deps = " " + labShipped["nvmet"]
		}
		fmt.Fprintf(&dep, "%s:%s\n", file, deps)
	}
	f[modDir+"/modules.dep"] = dep.String()
	f[multipathPath] = "Y\n"
	f[ioTimeoutPath] = "30\n"
	t.dirs[nvmetConfigDir+"/subsystems"] = true
	f["/etc/modules-load.d/dnv.conf"] = "# dnv\nnvmet\nnvme-tcp\n" +
		"dm-mod\nraid1\n"
	t.cmd("thin_dump --help", thinDumpHelp, "", 0)
	f[agent.ProcMountsPath] = "sysfs /sys sysfs rw 0 0\n" +
		"debugfs /sys/kernel/debug debugfs rw,relatime 0 0\n"
	t.dirs[agent.NvmetDebugRoot] = true
	f[agent.LockdownPath] = "[none] integrity confidentiality\n"
	t.devNos[labDisk] = labDiskNo
	disk := "/sys/dev/block/" + labDiskNo
	f[disk+"/size"] = "4194304\n"
	f[disk+"/queue/logical_block_size"] = "512\n"
	f[disk+"/queue/optimal_io_size"] = "0\n"
	f[disk+"/queue/minimum_io_size"] = "512\n"
	return t
}
