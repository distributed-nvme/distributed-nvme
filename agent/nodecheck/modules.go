package nodecheck

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
)

// The kernel modules each role needs (dnagent.md CM5). loadedNow must be
// loaded, or built in, before the agent starts: nvmet's configfs tree, the
// fabrics device, the NVMe host core and the device mapper are reached as
// files, which load nothing. onDemand the kernel loads itself when the
// agent first asks for them: an nvme transport by its connect, an nvmet
// transport by a port's enable, a dm target by a table, an md personality
// by an array's start; they must be there to load. Names are spelled as
// /sys/module spells them.
var (
	loadedNowBoth = []string{"nvmet", "nvme_fabrics", "nvme_core", "dm_mod"}
	loadedNowCn   = []string{"md_mod"}
	onDemandBoth  = []string{"nvmet_tcp", "nvme_tcp", "dm_clone"}
	onDemandCn    = []string{"dm_thin_pool", "dm_flakey", "raid1"}
)

// modulesLoadFiles are where systemd-modules-load and Debian's
// /etc/modules name a module to load at boot.
var (
	modulesLoadDirs = []string{
		"/etc/modules-load.d",
		"/run/modules-load.d",
		"/usr/local/lib/modules-load.d",
		"/usr/lib/modules-load.d",
	}
	modulesFile = "/etc/modules"
)

// nvmetConfigDir is nvmet's configfs tree, which every agent's port and
// exports are made in.
var nvmetConfigDir = agent.NvmetRoot

// moduleFacts is what the check read of the kernel's modules: those loaded
// now, those built in, those the running release ships with the modules
// each needs, and what failed to read.
type moduleFacts struct {
	loaded  map[string]bool
	builtin map[string]bool
	deps    map[string][]string
	errs    []string
}

// moduleName is a module's name as /sys/module spells it, from a name or a
// path in modules.builtin or modules.dep.
func moduleName(file string) string {
	base := path.Base(strings.TrimSpace(file))
	if i := strings.Index(base, ".ko"); i >= 0 {
		base = base[:i]
	}
	return strings.ReplaceAll(base, "-", "_")
}

// readModules reads /sys/module, and the running release's modules.builtin
// and modules.dep.
func (c *checker) readModules(ctx context.Context) *moduleFacts {
	m := &moduleFacts{loaded: map[string]bool{}, builtin: map[string]bool{},
		deps: map[string][]string{}}
	names, _, err := c.readDir(ctx, sysModuleDir)
	if err != nil {
		m.errs = append(m.errs, sysModuleDir+": "+err.Error())
	}
	for _, name := range names {
		m.loaded[name] = true
	}
	if c.release == "" {
		m.errs = append(m.errs, "no kernel release to find its modules by")
		return m
	}
	dir := modulesDir + "/" + c.release
	text, _, err := c.readFile(ctx, dir+"/modules.builtin")
	if err != nil {
		m.errs = append(m.errs, dir+"/modules.builtin: "+err.Error())
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			m.builtin[moduleName(line)] = true
		}
	}
	text, _, err = c.readFile(ctx, dir+"/modules.dep")
	if err != nil {
		m.errs = append(m.errs, dir+"/modules.dep: "+err.Error())
	}
	for _, line := range strings.Split(text, "\n") {
		file, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		var deps []string
		for _, dep := range strings.Fields(rest) {
			deps = append(deps, moduleName(dep))
		}
		m.deps[moduleName(file)] = deps
	}
	return m
}

func (c *checker) roleModules() (loadedNow, onDemand []string) {
	loadedNow = append([]string{}, loadedNowBoth...)
	onDemand = append([]string{}, onDemandBoth...)
	if c.opts.Role == RoleCn {
		loadedNow = append(loadedNow, loadedNowCn...)
		onDemand = append(onDemand, onDemandCn...)
	}
	return loadedNow, onDemand
}

// modulesRow is the role's modules, nvmet's configfs tree and native NVMe
// multipath: a module of loadedNow that is neither loaded nor built in
// fails, one of onDemand the running release does not have fails, and so
// do a missing nvmet tree and native multipath off or not built.
func (c *checker) modulesRow(ctx context.Context, m *moduleFacts) Result {
	res := Result{Name: "kernel modules"}
	loadedNow, onDemand := c.roleModules()
	var notLoaded, notThere, problems []string
	for _, name := range loadedNow {
		if !m.loaded[name] && !m.builtin[name] {
			notLoaded = append(notLoaded, name)
		}
	}
	for _, name := range onDemand {
		_, shipped := m.deps[name]
		if !m.loaded[name] && !m.builtin[name] && !shipped {
			notThere = append(notThere, name)
		}
	}
	if len(notLoaded) > 0 {
		problems = append(problems, "not loaded: "+
			strings.Join(notLoaded, " "))
	}
	if len(notThere) > 0 {
		problems = append(problems, "not in this kernel's modules: "+
			strings.Join(notThere, " "))
	}
	if _, ok, err := c.readDir(ctx, nvmetConfigDir); err != nil {
		problems = append(problems, nvmetConfigDir+" not read: "+err.Error())
	} else if !ok {
		problems = append(problems, nvmetConfigDir+" is not there "+
			"(nvmet loaded, configfs mounted)")
	}
	mp, ok, err := c.readFile(ctx, multipathPath)
	switch {
	case err != nil:
		problems = append(problems, "multipath not read: "+err.Error())
	case ok && strings.TrimSpace(mp) != "Y":
		problems = append(problems, "native NVMe multipath is off")
	case !ok && (m.loaded["nvme_core"] || m.builtin["nvme_core"]):
		// nvme_core has the parameter only when native multipath is built
		// in (CONFIG_NVME_MULTIPATH builds multipath.c, which defines it).
		problems = append(problems, "native NVMe multipath not built "+
			"(CONFIG_NVME_MULTIPATH)")
	}
	problems = append(problems, m.errs...)
	if len(problems) == 0 {
		res.Level = LevelOk
		res.Detail = "loaded: " + strings.Join(loadedNow, " ") +
			"; available: " + strings.Join(onDemand, " ")
		return res
	}
	res.Level = LevelFail
	res.Detail = strings.Join(problems, "; ")
	fix := []string{}
	if len(notLoaded) > 0 {
		fix = append(fix, "modprobe -a "+strings.Join(
			dashed(notLoaded), " "))
	}
	if len(notThere) > 0 {
		fix = append(fix, "install the kernel's extra modules package")
	}
	fix = append(fix, "keep nvme_core.multipath at its default, Y")
	res.Fix = strings.Join(fix, "; ")
	return res
}

// dashed spells module names as modprobe takes them.
func dashed(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = strings.ReplaceAll(name, "_", "-")
	}
	return out
}

// bootModulesRow warns for each module of loadedNow, loaded now, that is
// not built in and that no modules-load file names, alone or as a module
// a named one needs: a modprobe by hand is gone at the next boot, and the
// agent loads nothing.
func (c *checker) bootModulesRow(ctx context.Context, m *moduleFacts) Result {
	res := Result{Name: "kernel modules at boot"}
	named, err := c.namedModules(ctx)
	if err != nil {
		res.Level, res.Detail = LevelWarn, "not read: "+err.Error()
		res.Fix = "read the modules-load files by hand: " +
			strings.Join(append(append([]string{}, modulesLoadDirs...),
				modulesFile), ", ")
		return res
	}
	atBoot := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if atBoot[name] {
			return
		}
		atBoot[name] = true
		for _, dep := range m.deps[name] {
			walk(dep)
		}
	}
	for name := range named {
		walk(name)
	}
	loadedNow, _ := c.roleModules()
	var missing []string
	for _, name := range loadedNow {
		if m.builtin[name] || atBoot[name] {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		res.Level, res.Detail = LevelOk, "each is built in or loaded at boot"
		return res
	}
	res.Level = LevelWarn
	res.Detail = strings.Join(missing, " ") + ": loaded by no " +
		"modules-load file, so gone after a reboot"
	res.Fix = "name them in a file under " + modulesLoadDirs[0] +
		", such as dnv.conf"
	return res
}

// namedModules are the modules the modules-load files name.
func (c *checker) namedModules(ctx context.Context) (map[string]bool, error) {
	named := map[string]bool{}
	var files []string
	for _, dir := range modulesLoadDirs {
		names, _, err := c.readDir(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, ".conf") {
				files = append(files, dir+"/"+name)
			}
		}
	}
	files = append(files, modulesFile)
	for _, file := range files {
		text, _, err := c.readFile(ctx, file)
		if err != nil {
			return nil, err
		}
		for _, name := range modulesLoadNames(text) {
			named[name] = true
		}
	}
	return named, nil
}

// modulesLoadNames are the modules one modules-load file names, as
// /sys/module spells them: the first word of each line, blank lines and
// lines that start with '#' or ';' skipped.
func modulesLoadNames(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		names = append(names, moduleName(strings.Fields(line)[0]))
	}
	return names
}

// The commands each role's agent runs, by name, through its OsClient
// (dnagent.md CM5). blockdev is both roles': each agent clears a device's
// read-only flag with it after a reload (Dm.SetWritable). losetup is the cn
// agent's: it makes the loop device whose write cache a td's dm-error
// reports (cnagent.md CN15). thin_check is the cn agent's too: it checks a
// slice's thin-pool metadata that carries dm-thin's needs-check mark
// before the pool loads (cnagent.md CN13).
var (
	commandsBoth = []string{"dmsetup", "nvme", "lsblk", "mkdir", "rmdir",
		"ln", "rm", "blockdev"}
	commandsDn = []string{"blkdiscard"}
	commandsCn = []string{"mdadm", "thin_dump", "thin_check", "stat",
		"losetup"}
)

// thinDumpHeaderFlags are the flags of the cn agent's thin header read
// (cnagent.md CN14), which thin_dump's help must name.
var thinDumpHeaderFlags = []string{"--metadata-snap", "--skip-mappings"}

// commandsRow is the role's commands, looked up by PATH as the agent runs
// them; on a controller node also thin_dump's header read. Each missing
// fails.
func (c *checker) commandsRow(ctx context.Context) Result {
	res := Result{Name: "commands"}
	commands := append([]string{}, commandsBoth...)
	if c.opts.Role == RoleCn {
		commands = append(commands, commandsCn...)
	} else {
		commands = append(commands, commandsDn...)
	}
	sort.Strings(commands)
	cmd := agent.NewCmd(c.env.Oc)
	script := `for t in "$@"; do command -v "$t" >/dev/null 2>&1 || echo "$t"; done`
	stdout, stderr, exitCode, err := cmd.Run(ctx, "sh",
		append([]string{"-c", script, "sh"}, commands...)...)
	var problems []string
	if err != nil {
		problems = append(problems, fmt.Sprintf("the lookup did not run "+
			"(exit %d): %s %s", exitCode, strings.TrimSpace(stderr), err))
	} else if missing := strings.Fields(stdout); len(missing) > 0 {
		problems = append(problems, "not found by PATH: "+
			strings.Join(missing, " "))
	}
	if c.opts.Role == RoleCn && !contains(strings.Fields(stdout),
		"thin_dump") {
		help, helpErr, _, err := cmd.Run(ctx, "thin_dump", "--help")
		text := help + helpErr
		for _, flag := range thinDumpHeaderFlags {
			if err == nil && !strings.Contains(text, flag) {
				problems = append(problems, "thin_dump takes no "+flag)
			}
		}
		if err != nil {
			problems = append(problems, "thin_dump --help: "+err.Error())
		}
	}
	if len(problems) == 0 {
		res.Level, res.Detail = LevelOk, strings.Join(commands, " ")
		return res
	}
	res.Level = LevelFail
	res.Detail = strings.Join(problems, "; ")
	res.Fix = "install the packages that hold them (mdadm, nvme-cli, " +
		"dmsetup, util-linux, thin-provisioning-tools) and keep them in " +
		"the agent's PATH"
	return res
}
