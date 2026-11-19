package nodecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
)

func runCheck(t *testing.T, tr *tree, role string) *Report {
	t.Helper()
	opts := CheckOptions{Role: role, LocalStore: "/var/lib/dnv"}
	if role == RoleDn {
		opts.Disk = labDisk
	}
	rep, err := Check(context.Background(), tr.env(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return rep
}

func resultNamed(t *testing.T, rep *Report, name string) (Result, bool) {
	t.Helper()
	for _, res := range rep.Results {
		if res.Name == name {
			return res, true
		}
	}
	return Result{}, false
}

// The lab's guests, set up as the procedures say, pass every read on both
// roles with no warning, and each role reads its rows alone.
func TestLabNodePasses(t *testing.T) {
	cnOnly := []string{"md format (the 6.19 line)", "nvmet host cut (debugfs)"}
	dnOnly := []string{"disk logical block size", "disk best IO size"}
	for _, role := range []string{RoleDn, RoleCn} {
		rep := runCheck(t, labTree(), role)
		for _, res := range rep.Results {
			if res.Level != LevelOk {
				t.Errorf("%s: %s is %s: %s", role, res.Name, res.Level,
					res.Detail)
			}
		}
		if rep.Failures != 0 || rep.Warnings != 0 {
			t.Errorf("%s: %d failures, %d warnings", role, rep.Failures,
				rep.Warnings)
		}
		mine, other := cnOnly, dnOnly
		if role == RoleDn {
			mine, other = dnOnly, cnOnly
		}
		for _, name := range mine {
			if _, ok := resultNamed(t, rep, name); !ok {
				t.Errorf("%s: no %s row", role, name)
			}
		}
		for _, name := range other {
			if _, ok := resultNamed(t, rep, name); ok {
				t.Errorf("%s: reads %s", role, name)
			}
		}
	}
}

// rowCase is one read of the node check at one level: the tree changed so,
// for the roles named, the row reads the level.
type rowCase struct {
	name   string
	roles  []string
	change func(*tree)
	row    string
	level  Level
}

var both = []string{RoleDn, RoleCn}

func release(r string) func(*tree) {
	return func(t *tree) {
		t.files["/proc/sys/kernel/osrelease"] = r + "\n"
		// The modules of that release are the lab's.
		for _, name := range []string{"modules.builtin", "modules.dep"} {
			t.files[modulesDir+"/"+r+"/"+name] =
				t.files[modulesDir+"/"+labRelease+"/"+name]
		}
	}
}

func mdadmLine(line string) func(*tree) {
	return func(t *tree) { t.cmd("mdadm --version", "", line, 0) }
}

func setFile(path, data string) func(*tree) {
	return func(t *tree) { t.files[path] = data }
}

func dropFile(path string) func(*tree) {
	return func(t *tree) { delete(t.files, path) }
}

func all(changes ...func(*tree)) func(*tree) {
	return func(t *tree) {
		for _, c := range changes {
			c(t)
		}
	}
}

const (
	maskPath  = "/etc/udev/rules.d/" + mdMaskRuleName
	confPath  = "/etc/mdadm/mdadm.conf"
	diskQueue = "/sys/dev/block/" + labDiskNo + "/queue/"
)

func noAuto(t *tree) { t.files[confPath] = "HOMEHOST <system>\n" }

func TestEachRowAtEachLevel(t *testing.T) {
	cases := []rowCase{
		// The kernel: the series, never the build.
		{"7.0.0-43", both, release("7.0.0-43-generic"), "kernel release",
			LevelOk},
		{"7.1.0", both, release("7.1.0"), "kernel release", LevelWarn},
		{"6.19.2", both, release("6.19.2-1"), "kernel release", LevelWarn},
		{"6.1", both, release("6.1.0-31-amd64"), "kernel release", LevelWarn},
		{"6.0.9", both, release("6.0.9"), "kernel release", LevelFail},
		{"5.15", both, release("5.15.0-1-generic"), "kernel release",
			LevelFail},
		{"no version", both, setFile("/proc/sys/kernel/osrelease",
			"generic\n"), "kernel release", LevelFail},
		{"release not read", both, dropFile("/proc/sys/kernel/osrelease"),
			"kernel release", LevelFail},

		// md's side of the 6.19 line, on a controller node.
		{"no check_new_feature", []string{RoleCn}, all(
			dropFile(mdParamsDir+"/"+mdFeatureParam),
			setFile(mdParamsDir+"/legacy_async_del_gendisk", "Y\n")),
			"md format (the 6.19 line)", LevelWarn},
		{"md_mod not there", []string{RoleCn},
			dropFile(mdParamsDir + "/" + mdFeatureParam),
			"md format (the 6.19 line)", LevelWarn},

		// mdadm.
		{"mdadm 4.6", []string{RoleCn}, mdadmLine("mdadm - v4.6"),
			"mdadm", LevelWarn},
		{"mdadm from git", []string{RoleCn},
			mdadmLine("mdadm - v4.5-12-gabcdef0"), "mdadm",
			LevelOk},
		{"mdadm no version", []string{RoleCn}, mdadmLine("mdadm - unknown"),
			"mdadm", LevelWarn},
		{"mdadm missing", []string{RoleCn}, func(t *tree) {
			delete(t.cmds, "mdadm --version")
		}, "mdadm", LevelFail},
		{"mdadm missing on a dn", []string{RoleDn}, func(t *tree) {
			delete(t.cmds, "mdadm --version")
		}, "mdadm", LevelOk},
		{"/sbin/mdadm missing on a dn", []string{RoleDn}, func(t *tree) {
			delete(t.cmds, "/sbin/mdadm --version")
		}, "mdadm", LevelFail},
		{"mdadm 4.6 on a dn", []string{RoleDn}, func(t *tree) {
			t.cmd("/sbin/mdadm --version", "", "mdadm - v4.6 - x", 0)
		}, "mdadm", LevelOk},

		// udev's version, which the shipped rules need.
		{"udev 252", both, func(t *tree) {
			t.cmd("udevadm --version", "252\n", "", 0)
		}, "udev version", LevelFail},
		{"udev 253", both, func(t *tree) {
			t.cmd("udevadm --version", "253\n", "", 0)
		}, "udev version", LevelOk},
		{"udev 257.4", both, func(t *tree) {
			t.cmd("udevadm --version", "257.4-1ubuntu3\n", "", 0)
		}, "udev version", LevelOk},
		{"no udevadm", both, func(t *tree) {
			delete(t.cmds, "udevadm --version")
		}, "udev version", LevelFail},
		{"udevadm prints no version", both, func(t *tree) {
			t.cmd("udevadm --version", "systemd\n", "", 0)
		}, "udev version", LevelFail},

		// The md assembly mask counts in two ways alone: the shipped mask
		// rule in effect, byte for byte, or mdadm.conf in the strict form
		// with no ARRAY line of a dnv array.
		{"no mask", both, all(dropFile(maskPath), noAuto),
			"md assembly mask", LevelFail},
		{"mask alone", both, noAuto, "md assembly mask", LevelOk},
		{"AUTO -all alone", both, dropFile(maskPath), "md assembly mask",
			LevelOk},
		// The shipped mask: the file udev reads for its name, byte for byte.
		{"the shipped mask in /usr/lib", both, all(noAuto, dropFile(maskPath),
			setFile("/usr/lib/udev/rules.d/"+mdMaskRuleName,
				shippedRules[mdMaskRuleName])), "md assembly mask", LevelOk},
		{"the shipped mask with a line end more", both, all(noAuto,
			setFile(maskPath, shippedRules[mdMaskRuleName]+"\n")),
			"md assembly mask", LevelFail},
		{"the shipped mask under another name", both, all(noAuto,
			dropFile(maskPath), setFile("/etc/udev/rules.d/61-local-md.rules",
				shippedRules[mdMaskRuleName])), "md assembly mask", LevelFail},
		{"the shipped mask hidden by another text", both, all(noAuto,
			setFile("/usr/lib/udev/rules.d/"+mdMaskRuleName,
				shippedRules[mdMaskRuleName]), setFile(maskPath,
				oldMaskText())), "md assembly mask", LevelFail},
		{"a masked mask rule and no AUTO", both, all(noAuto,
			maskedRule(mdMaskRuleName)), "md assembly mask", LevelFail},
		{"the mask rule not read and no AUTO", both, all(noAuto,
			func(t *tree) { t.errs[maskPath] = syscall.EIO }),
			"md assembly mask", LevelFail},
		{"an old mask text and no AUTO", both, all(noAuto, setFile(maskPath,
			oldMaskText())), "md assembly mask", LevelFail},
		{"an old mask text beside AUTO -all", both, setFile(maskPath,
			oldMaskText()), "md assembly mask", LevelOk},
		{"the stock rule hidden and no AUTO", both, all(dropFile(maskPath),
			noAuto, setFile("/etc/udev/rules.d/64-md-raid-assembly.rules",
				"")), "md assembly mask", LevelFail},
		{"a rules directory not read and no AUTO", both, all(noAuto,
			func(t *tree) { t.errs["/run/udev/rules.d"] = syscall.EIO }),
			"md assembly mask", LevelFail},
		{"a rules directory not read beside AUTO -all", both, func(t *tree) {
			t.errs["/run/udev/rules.d"] = syscall.EIO
		}, "md assembly mask", LevelOk},
		// mdadm.conf's strict form: the first AUTO line names no homehost,
		// and the first word that names each format, or "all", is a minus.
		{"AUTO -1.x -0.90 -ddf -imsm", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -1.x -0.90 -ddf -imsm\n")),
			"md assembly mask", LevelOk},
		{"AUTO -1.2 -0 -DDF -imsm +all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -1.2 -0 -DDF -imsm +all\n")),
			"md assembly mask", LevelOk},
		{"AUTO -1 -0.90 -ddf -imsm +all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -1 -0.90 -ddf -imsm +all\n")),
			"md assembly mask", LevelOk},
		{"AUTO -1.x -0.90 -ddf +all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -1.x -0.90 -ddf +all\n")),
			"md assembly mask", LevelFail},
		{"AUTO -1.x -0.90 -imsm +all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -1.x -0.90 -imsm +all\n")),
			"md assembly mask", LevelFail},
		{"AUTO +1.x -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO +1.x -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO -1.x +all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -1.x +all\n")), "md assembly mask",
			LevelFail},
		{"AUTO +ddf -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO +ddf -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO +0 -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO +0 -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO +1 -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO +1 -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO +1.2 -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO +1.2 -all\n")), "md assembly mask",
			LevelFail},
		// homehost lets mdadm take an array it judges the node's own, as a
		// host's superblock with the node's name is, or every dnv array
		// under HOMEHOST any.
		{"AUTO homehost -all", both, all(dropFile(maskPath),
			setFile(confPath, "auto homehost -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO homehost -all, POLICY auto=no", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO homehost -all\nPOLICY auto=no\n")),
			"md assembly mask", LevelFail},
		{"AUTO -all homehost", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all homehost\n")), "md assembly mask",
			LevelFail},
		// yes and no end mdadm's reading of the line; neither is a minus.
		{"AUTO no", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO no\n")), "md assembly mask", LevelFail},
		{"AUTO yes -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO yes -all\n")), "md assembly mask",
			LevelFail},
		// A POLICY line that sets auto= is not judged, so mdadm.conf does
		// not count; one that sets no auto= changes nothing.
		{"AUTO -all, POLICY auto=no", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all\nPOLICY auto=no\n")),
			"md assembly mask", LevelFail},
		{"AUTO -all, POLICY auto=yes for imsm", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all\nPOLICY metadata=imsm auto=yes\n")),
			"md assembly mask", LevelFail},
		{"AUTO -all, a POLICY with no auto=", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all\nPOLICY domain=local action=spare\n")),
			"md assembly mask", LevelOk},
		// mdadm's reading of lines and words (lib.c, conf_word; config.c,
		// autoline): the first AUTO line alone, a line that starts with a
		// blank continuing the one before past comment lines, quotes taken
		// away, and a "#" a comment only at a word's start.
		{"AUTO +1.x, then AUTO -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO +1.x\nAUTO -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO -all continued from MAILADDR", both, all(dropFile(maskPath),
			setFile(confPath, "MAILADDR root\n  AUTO -all\n")),
			"md assembly mask", LevelFail},
		{"AUTO, then -all on the next line", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO\n -all\n")), "md assembly mask", LevelOk},
		{"AUTO, a comment, then -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO\n# the rest\n -all\n")),
			"md assembly mask", LevelOk},
		{"AUTO # -all", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO # -all\n")), "md assembly mask",
			LevelFail},
		{"AUTO -all#x", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all#x\n")), "md assembly mask",
			LevelFail},
		{"AUTO '-all'", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO '-all'\n")), "md assembly mask", LevelOk},
		// The configuration's files, as mdadm reads them.
		{"AUTO -all in mdadm.conf.d alone", both, all(dropFile(maskPath),
			dropFile(confPath), setFile("/etc/mdadm.conf.d/dnv.conf",
				"AUTO -all\n")), "md assembly mask", LevelOk},
		{"AUTO -all in Debian's mdadm.conf.d", both, all(dropFile(maskPath),
			setFile(confPath, "MAILADDR root\n"),
			setFile("/etc/mdadm/mdadm.conf.d/dnv.conf", "AUTO -all\n")),
			"md assembly mask", LevelOk},
		{"AUTO -all in a hidden file of mdadm.conf.d", both, all(
			dropFile(maskPath), setFile(confPath, "MAILADDR root\n"),
			setFile("/etc/mdadm/mdadm.conf.d/.dnv.conf", "AUTO -all\n")),
			"md assembly mask", LevelFail},
		{"mdadm.conf, not Debian's, is read", both, all(dropFile(maskPath),
			setFile("/etc/mdadm.conf", "MAILADDR root\n")),
			"md assembly mask", LevelFail},
		// An ARRAY line that can match a dnv array makes mdadm assemble it
		// whatever AUTO says.
		{"an ARRAY line named for a dnv array", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all\nARRAY /dev/md/x metadata=1.2 "+
				"name=any:dnv-0000000000000001-00-00 "+
				"UUID=11111111:22222222:33333333:44444444\n")),
			"md assembly mask", LevelFail},
		{"an ARRAY line by a dnv array's node", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/0000000100000000000003b10000 "+
				"UUID=11111111:22222222:33333333:44444444\n")),
			"md assembly mask", LevelFail},
		{"an ARRAY line by a live dnv array's uuid", both, all(
			dropFile(maskPath), dnvArray, setFile(confPath, "AUTO -all\n"+
				"ARRAY /dev/md/y UUID=aaaaaaaa:bbbbbbbb:cccccccc:dddddddd\n")),
			"md assembly mask", LevelFail},
		{"an ARRAY line named for a dnv array with no uuid", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/q name=any:dnv-0000000000000001-00-00\n")),
			"md assembly mask", LevelOk},
		{"an ARRAY line of the node's own array", both, all(
			dropFile(maskPath), dnvArray, ownArray(
				"ARRAY /dev/md/root UUID=5e7d1c2a:0b8f4e3d:9a6c2b1f:4d3e2a1b\n")),
			"md assembly mask", LevelOk},
		// A dnv array's name or node counts in its exact form alone, bare or
		// after a homehost, never a node's own array whose name its host's
		// name starts ("dnv-cn0:0" on a host called dnv-cn0).
		{"an ARRAY line named for a dnv array in the bare form", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/x name=dnv-0000000000000001-00-00 "+
				"UUID=11111111:22222222:33333333:44444444\n")),
			"md assembly mask", LevelFail},
		{"an ARRAY line named after a dnv- host", both, all(dropFile(maskPath),
			setFile(confPath, "AUTO -all\nARRAY /dev/md/0 metadata=1.2 "+
				"name=dnv-cn0:0 UUID=11111111:22222222:33333333:44444444\n")),
			"md assembly mask", LevelOk},
		{"an ARRAY line by a node named after a dnv- host", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/dnv-cn0:0 UUID=11111111:22222222:33333333:44444444\n")),
			"md assembly mask", LevelOk},
		// A running array with no dnv member whose line has dnv's exact name
		// form counts as dnv's: a dnv array that udev put together straight
		// over a leg's NVMe device has no dnv member either.
		{"an ARRAY line in dnv's name form of an array with no dnv member",
			both, all(dropFile(maskPath), ownArray("ARRAY /dev/md/0 "+
				"metadata=1.2 name=any:dnv-0000000000000001-00-00 "+
				"UUID=5e7d1c2a:0b8f4e3d:9a6c2b1f:4d3e2a1b\n")),
			"md assembly mask", LevelFail},
		// mdadm's arrayline: the device name is the first word, at any
		// place, that starts with "/" or holds no "=".
		{"an ARRAY line by a dnv array's node after its uuid", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"UUID=11111111:22222222:33333333:44444444 "+
				"/dev/md/dnv-0000000000000001-00-00\n")),
			"md assembly mask", LevelFail},
		// A line that matches by devices= or super-minor= with no uuid takes
		// any device it covers, whatever AUTO says, and is not judged.
		{"an ARRAY line by devices= with no uuid", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/x devices=/dev/nvme*\n")),
			"md assembly mask", LevelFail},
		{"an ARRAY line by super-minor= with no uuid", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/x super-minor=0\n")), "md assembly mask", LevelFail},
		{"an ARRAY line by devices= with a uuid mdadm does not take", both,
			all(dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/x UUID=1111-xyz devices=/dev/nvme*\n")),
			"md assembly mask", LevelFail},
		{"an ARRAY line by devices= with its uuid", both, all(
			dropFile(maskPath), setFile(confPath, "AUTO -all\nARRAY "+
				"/dev/md/x devices=/dev/vda2 "+
				"UUID=11111111:22222222:33333333:44444444\n")),
			"md assembly mask", LevelOk},
		{"an old mask text", both, all(noAuto, setFile(maskPath,
			oldMaskText())), "udev rule " + mdMaskRuleName, LevelWarn},
		{"the shipped mask with a line end more, row", both, setFile(maskPath,
			shippedRules[mdMaskRuleName]+"\n"), "udev rule " + mdMaskRuleName,
			LevelWarn},

		// The shipped rules, the mask among them: absent or masked by an
		// empty file of their name fails, whatever else masks md.
		{"no dm rule", both, dropFile("/etc/udev/rules.d/" + dmRuleName),
			"udev rule " + dmRuleName, LevelFail},
		{"no mask rule beside AUTO -all", both, dropFile(maskPath),
			"udev rule " + mdMaskRuleName, LevelFail},
		{"a masked dm rule", both, maskedRule(dmRuleName),
			"udev rule " + dmRuleName, LevelFail},
		{"a masked mask rule", both, maskedRule(mdMaskRuleName),
			"udev rule " + mdMaskRuleName, LevelFail},
		{"a masked md array rule", both, maskedRule(mdArrayRuleName),
			"udev rule " + mdArrayRuleName, LevelFail},
		{"another dm rule", both, setFile("/etc/udev/rules.d/"+dmRuleName,
			"# mine\n"), "udev rule " + dmRuleName, LevelWarn},
		{"dm rule in /usr/lib", both, all(
			dropFile("/etc/udev/rules.d/"+dmRuleName),
			setFile("/usr/lib/udev/rules.d/"+dmRuleName,
				shippedRules[dmRuleName])),
			"udev rule " + dmRuleName, LevelOk},
		{"no md array rule", both, dropFile(
			"/etc/udev/rules.d/" + mdArrayRuleName),
			"udev rule " + mdArrayRuleName, LevelFail},
		{"another md array rule", both, setFile(
			"/etc/udev/rules.d/"+mdArrayRuleName, "# mine\n"),
			"udev rule " + mdArrayRuleName, LevelWarn},

		// lvm2, by its effective settings.
		{"no devices file", both, dropFile(lvmDevicesFile), "lvm2 scan",
			LevelWarn},
		{"devices file off", both, func(t *tree) {
			lvmSettings(t, "0", `["a|.*|"]`, `["a|.*|"]`)
		}, "lvm2 scan", LevelWarn},
		{"a filter that rejects dnv's links but accepts all", both,
			func(t *tree) {
				lvmSettings(t, "0", `["a|.*|"]`,
					`["r|^/dev/mapper/dnv-.*|","r|^/dev/md/.*|","a|.*|"]`)
			}, "lvm2 scan", LevelWarn},
		{"a filter that misses a dm device's kernel name", both,
			func(t *tree) {
				lvmSettings(t, "0", `["a|.*|"]`, `["r|^/dev/mapper/dnv-.*|",`+
					`"r|^/dev/md.*|","r|^/dev/block/9:.*|",`+
					`"r|^/dev/block/259:.*|","r|^/dev/disk/by-id/md-.*|",`+
					`"a|.*|"]`)
			}, "lvm2 scan", LevelWarn},
		{"a filter that misses an array's by-id links", both,
			func(t *tree) {
				lvmSettings(t, "0", `["a|.*|"]`, `["r|^/dev/mapper/dnv-.*|",`+
					`"r|^/dev/dm-.*|","r|^/dev/md.*|","r|^/dev/block/.*|",`+
					`"a|.*|"]`)
			}, "lvm2 scan", LevelWarn},
		{"a filter that accepts the node's disks alone", both,
			func(t *tree) {
				lvmSettings(t, "0", `["a|.*|"]`, `["a|^/dev/vda.*|","r|.*|"]`)
			}, "lvm2 scan", LevelOk},
		{"a filter that accepts first", both, func(t *tree) {
			lvmSettings(t, "0", `["a|.*|"]`, `["a|.*|","r|.*|"]`)
		}, "lvm2 scan", LevelWarn},
		{"no lvm2", both, func(t *tree) {
			delete(t.cmds, "lvmconfig "+strings.Join(lvmConfigArgs, " "))
		}, "lvm2 scan", LevelOk},
		{"lvmconfig fails", both, func(t *tree) {
			t.cmd("lvmconfig "+strings.Join(lvmConfigArgs, " "), "",
				"Configuration node devices/use_devicesfile not found", 5)
		}, "lvm2 scan", LevelWarn},

		// mdadm.conf's AUTO -all and the node's own arrays.
		{"no AUTO line", both, noAuto, "mdadm.conf AUTO -all", LevelWarn},
		{"AUTO -1.x +all", both, setFile(confPath, "AUTO -1.x +all\n"),
			"mdadm.conf AUTO -all", LevelWarn},
		{"an ARRAY line of a dnv array beside the mask", both,
			setFile(confPath, "AUTO -all\nARRAY /dev/md/x "+
				"name=any:dnv-0000000000000001-00-00 "+
				"UUID=11111111:22222222:33333333:44444444\n"),
			"mdadm.conf AUTO -all", LevelWarn},
		{"an ARRAY line with a dnv name and no uuid", both,
			setFile(confPath, "AUTO -all\nARRAY /dev/md/x "+
				"name=any:dnv-0000000000000001-00-00\n"),
			"mdadm.conf AUTO -all", LevelOk},
		{"AUTO homehost -all, row", both, setFile(confPath,
			"AUTO homehost -all\n"), "mdadm.conf AUTO -all", LevelWarn},
		{"AUTO -all with a POLICY auto=no, row", both, setFile(confPath,
			"AUTO -all\nPOLICY auto=no\n"), "mdadm.conf AUTO -all",
			LevelWarn},
		{"AUTO -1.x -0.90 -ddf -imsm, row", both, setFile(confPath,
			"AUTO -1.x -0.90 -ddf -imsm\n"), "mdadm.conf AUTO -all", LevelOk},
		{"AUTO -all in mdadm.conf.d alone", both, all(dropFile(confPath),
			setFile("/etc/mdadm.conf.d/dnv.conf", "AUTO -all\n")),
			"mdadm.conf AUTO -all", LevelOk},
		{"an own array named after a dnv- host, with its line", both,
			ownArray("ARRAY /dev/md/0 metadata=1.2 name=dnv-cn0:0 " +
				"UUID=5e7d1c2a:0b8f4e3d:9a6c2b1f:4d3e2a1b\n"),
			"mdadm.conf AUTO -all", LevelOk},
		{"an ARRAY line by devices= with no uuid, row", both, setFile(
			confPath, "AUTO -all\nARRAY /dev/md/x devices=/dev/nvme*\n"),
			"mdadm.conf AUTO -all", LevelWarn},
		{"an own array with no ARRAY line", both, ownArray(""),
			"mdadm.conf AUTO -all", LevelWarn},
		{"an own array with its ARRAY line", both,
			ownArray("ARRAY /dev/md/root metadata=1.2 " +
				"UUID=5e7d1c2a:0b8f4e3d:9a6c2b1f:4d3e2a1b\n"),
			"mdadm.conf AUTO -all", LevelOk},

		// The modules.
		{"nvmet not loaded", both, func(t *tree) {
			delete(t.dirs, sysModuleDir+"/nvmet")
		}, "kernel modules", LevelFail},
		{"md_mod not loaded", []string{RoleCn}, mdModGone,
			"kernel modules", LevelFail},
		{"md_mod not loaded on a dn", []string{RoleDn}, mdModGone,
			"kernel modules", LevelOk},
		{"md_mod built in", []string{RoleCn}, func(t *tree) {
			delete(t.dirs, sysModuleDir+"/md_mod")
		}, "kernel modules", LevelOk},
		{"raid1 not shipped", []string{RoleCn}, func(t *tree) {
			delete(t.dirs, sysModuleDir+"/raid1")
			dep := modulesDir + "/" + labRelease + "/modules.dep"
			t.files[dep] = strings.Replace(t.files[dep],
				labShipped["raid1"]+":\n", "", 1)
		}, "kernel modules", LevelFail},
		{"raid1 loads on demand", []string{RoleCn}, func(t *tree) {
			delete(t.dirs, sysModuleDir+"/raid1")
		}, "kernel modules", LevelOk},
		{"multipath off", both, setFile(multipathPath, "N\n"),
			"kernel modules", LevelFail},
		{"multipath not built", both, dropFile(multipathPath),
			"kernel modules", LevelFail},
		{"no nvmet tree", both, func(t *tree) {
			delete(t.dirs, nvmetConfigDir+"/subsystems")
		}, "kernel modules", LevelFail},

		// The modules at boot.
		{"nvmet loaded by hand", both, setFile(
			"/etc/modules-load.d/dnv.conf", "nvme-tcp\ndm-mod\n"),
			"kernel modules at boot", LevelWarn},
		{"in /etc/modules", both, all(
			dropFile("/etc/modules-load.d/dnv.conf"),
			setFile(modulesFile, "nvmet\nnvme_tcp\ndm_mod\n")),
			"kernel modules at boot", LevelOk},
		{"nvme_core by nvme_tcp's need of it", both, setFile(
			"/etc/modules-load.d/dnv.conf", "nvmet\nnvme-tcp\ndm-mod\n"),
			"kernel modules at boot", LevelOk},

		// The commands.
		// Both agents run blockdev: each clears a read-only flag with it
		// after a reload (Dm.SetWritable).
		{"no blockdev", both, func(t *tree) {
			t.missing["blockdev"] = true
		}, "commands", LevelFail},
		{"no blkdiscard", []string{RoleDn}, func(t *tree) {
			t.missing["blkdiscard"] = true
		}, "commands", LevelFail},
		{"no blkdiscard on a cn", []string{RoleCn}, func(t *tree) {
			t.missing["blkdiscard"] = true
		}, "commands", LevelOk},
		{"no thin_dump", []string{RoleCn}, func(t *tree) {
			t.missing["thin_dump"] = true
		}, "commands", LevelFail},
		{"thin_dump with no header read", []string{RoleCn}, func(t *tree) {
			t.cmd("thin_dump --help", "Usage: thin_dump [options]\n", "", 0)
		}, "commands", LevelFail},
		{"no nvme", both, func(t *tree) { t.missing["nvme"] = true },
			"commands", LevelFail},

		// nvme_core.io_timeout.
		{"io_timeout never on a cn", []string{RoleCn}, setFile(ioTimeoutPath,
			"4294967295\n"), "nvme_core.io_timeout", LevelFail},
		{"io_timeout never on a dn", []string{RoleDn}, setFile(ioTimeoutPath,
			"4294967295\n"), "nvme_core.io_timeout", LevelWarn},
		{"io_timeout 60 on a cn", []string{RoleCn}, setFile(ioTimeoutPath,
			"60\n"), "nvme_core.io_timeout", LevelWarn},
		{"io_timeout 60 on a dn", []string{RoleDn}, setFile(ioTimeoutPath,
			"60\n"), "nvme_core.io_timeout", LevelOk},
		{"io_timeout not there", both, dropFile(ioTimeoutPath),
			"nvme_core.io_timeout", LevelWarn},
		{"io_timeout not read", both, func(t *tree) {
			t.errs[ioTimeoutPath] = syscall.EIO
		}, "nvme_core.io_timeout", LevelWarn},

		// Wave 8's kernel option and lockdown, on a controller node.
		{"no debugfs", []string{RoleCn}, setFile(procMountsPath,
			"sysfs /sys sysfs rw 0 0\n"), "nvmet host cut (debugfs)",
			LevelWarn},
		{"no nvmet debugfs", []string{RoleCn}, func(t *tree) {
			delete(t.dirs, nvmetDebugfsDir)
		}, "nvmet host cut (debugfs)", LevelWarn},
		{"lockdown", []string{RoleCn}, setFile(lockdownPath,
			"none [integrity] confidentiality\n"),
			"nvmet host cut (debugfs)", LevelWarn},
		{"no lockdown file", []string{RoleCn}, dropFile(lockdownPath),
			"nvmet host cut (debugfs)", LevelOk},

		// The disk.
		{"4096-byte blocks", []string{RoleDn}, setFile(
			diskQueue+"logical_block_size", "4096\n"),
			"disk logical block size", LevelFail},
		{"a partition's disk", []string{RoleDn}, func(t *tree) {
			delete(t.files, diskQueue+"logical_block_size")
			delete(t.files, diskQueue+"optimal_io_size")
			delete(t.files, diskQueue+"minimum_io_size")
			part := "/sys/dev/block/" + labDiskNo + "/"
			t.files[part+"partition"] = "1\n"
			t.files[part+"../queue/logical_block_size"] = "512\n"
			t.files[part+"../queue/optimal_io_size"] = "0\n"
			t.files[part+"../queue/minimum_io_size"] = "512\n"
		}, "disk logical block size", LevelOk},
		{"no disk there", []string{RoleDn}, func(t *tree) {
			delete(t.devNos, labDisk)
		}, "disk logical block size", LevelFail},
		{"best IO 4 MiB", []string{RoleDn}, setFile(
			diskQueue+"optimal_io_size", "4194304\n"), "disk best IO size",
			LevelOk},
		{"best IO 8 MiB", []string{RoleDn}, setFile(
			diskQueue+"optimal_io_size", "8388608\n"), "disk best IO size",
			LevelWarn},
		{"minimum IO 8 MiB", []string{RoleDn}, setFile(
			diskQueue+"minimum_io_size", "8388608\n"), "disk best IO size",
			LevelWarn},
	}
	for _, tc := range cases {
		for _, role := range tc.roles {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				tr := labTree()
				tc.change(tr)
				rep := runCheck(t, tr, role)
				res, ok := resultNamed(t, rep, tc.row)
				if !ok {
					t.Fatalf("no %q row", tc.row)
				}
				if res.Level != tc.level {
					t.Errorf("%s is %s (%s), want %s", tc.row, res.Level,
						res.Detail, tc.level)
				}
				if res.Level != LevelOk && res.Fix == "" {
					t.Errorf("%s is %s with no fix line", tc.row, res.Level)
				}
			})
		}
	}
}

// oldMaskText is the shipped mask's text with its examine as the suites
// installed it before the mask named the 1.2 format: the IMPORT line's
// flag taken away, not a comment's.
func oldMaskText() string {
	return strings.Replace(shippedRules[mdMaskRuleName],
		"--export --metadata=1.2 $devnode", "--export $devnode", 1)
}

// maskedRule masks a shipped rule by an empty file of its name in
// /etc/udev/rules.d over the shipped text in /usr/lib/udev/rules.d, as a
// link to /dev/null does.
func maskedRule(name string) func(*tree) {
	return func(t *tree) {
		t.files["/usr/lib/udev/rules.d/"+name] = shippedRules[name]
		t.files["/etc/udev/rules.d/"+name] = ""
	}
}

// dnvArray gives the node a running dnv array, over a dnv dm device.
func dnvArray(t *tree) {
	t.files["/sys/block/md5/md/array_state"] = "clean\n"
	t.files["/sys/block/md5/md/uuid"] =
		"aaaaaaaa-bbbb-bbbb-cccc-ccccdddddddd\n"
	t.files["/sys/block/md5/md/dev-dm-3/block/dm/name"] =
		"dnv-0000000000000001-0000000000000011-c9-0000000000000001-" +
			"0000000000000002\n"
}

// A masked rule's fix line replaces a mask in the install directory, which
// the install itself does, and else installs over a later one; it never
// installs and then removes the same path.
func TestMaskedRuleFixLine(t *testing.T) {
	tr := labTree()
	maskedRule(dmRuleName)(tr)
	res, _ := resultNamed(t, runCheck(t, tr, RoleDn), "udev rule "+dmRuleName)
	want := "replace /etc/udev/rules.d/" + dmRuleName + " with deploy/udev/" +
		dmRuleName
	if res.Level != LevelFail || res.Fix != want {
		t.Errorf("masked in /etc: %+v, want the fix %q", res, want)
	}
	tr = labTree()
	delete(tr.files, "/etc/udev/rules.d/"+dmRuleName)
	tr.files["/run/udev/rules.d/"+dmRuleName] = ""
	tr.files["/usr/lib/udev/rules.d/"+dmRuleName] = shippedRules[dmRuleName]
	res, _ = resultNamed(t, runCheck(t, tr, RoleDn), "udev rule "+dmRuleName)
	if res.Level != LevelFail || strings.Contains(res.Fix, "remove") ||
		!strings.HasPrefix(res.Fix, "install deploy/udev/"+dmRuleName+
			" in /etc/udev/rules.d") {
		t.Errorf("masked in /run: %+v, want an install in /etc", res)
	}
}

// The old mask text differs from the shipped one in its examine's flag,
// not in a comment.
func TestOldMaskTextIsTheOldExamine(t *testing.T) {
	old := oldMaskText()
	if old == shippedRules[mdMaskRuleName] {
		t.Fatal("the old mask text is the shipped one")
	}
	for _, line := range strings.Split(old, "\n") {
		if strings.Contains(line, "IMPORT{program}") &&
			strings.Contains(line, "--metadata") {
			t.Errorf("the old mask's examine still names a format: %q", line)
		}
	}
}

// The mask row's failure says why neither counted mask is there, what the
// check counts, and that the fix is the shipped mask rule.
func TestMaskRowSaysWhatItCounts(t *testing.T) {
	tr := labTree()
	noAuto(tr)
	tr.files[maskPath] = oldMaskText()
	res, _ := resultNamed(t, runCheck(t, tr, RoleCn), "md assembly mask")
	for _, want := range []string{
		maskPath + " is not the shipped text byte for byte",
		confPath + " has no AUTO line",
		"the check counts only the shipped mask rule",
	} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail %q does not say %q", res.Detail, want)
		}
	}
	if want := "install deploy/udev/" + mdMaskRuleName +
		" in /etc/udev/rules.d"; res.Level != LevelFail || res.Fix != want {
		t.Errorf("%s with the fix %q, want a failure with the fix %q",
			res.Level, res.Fix, want)
	}
}

// The mdadm.conf row's fix line follows its reason: a second AUTO line, or
// an AUTO -all beside a POLICY auto=, would leave the warning in place.
func TestMdadmConfFixByReason(t *testing.T) {
	cases := []struct{ conf, fix string }{
		{"HOMEHOST <system>\n", "write ARRAY lines for the node's own " +
			"arrays, then AUTO -all in " + confPath},
		{"AUTO homehost -all\n", "make the first AUTO line AUTO -all in " +
			confPath},
		{"AUTO no\n", "make the first AUTO line AUTO -all in " + confPath},
		{"AUTO +1.x -all\n", "make the first AUTO line AUTO -all in " +
			confPath},
		{"AUTO -all\nPOLICY auto=no\n", "remove auto= from every POLICY " +
			"line in " + confPath},
		{"AUTO -all\nARRAY /dev/md/x devices=/dev/nvme*\n", "give every " +
			"ARRAY line its array's UUID="},
	}
	for _, tc := range cases {
		tr := labTree()
		tr.files[confPath] = tc.conf
		res, _ := resultNamed(t, runCheck(t, tr, RoleCn), "mdadm.conf AUTO -all")
		if res.Level != LevelWarn || !strings.HasPrefix(res.Fix, tc.fix) {
			t.Errorf("%q: %s with the fix %q, want one that starts %q",
				tc.conf, res.Level, res.Fix, tc.fix)
		}
	}
}

// A warning for a fact the check could not read says so first, "not read",
// and its fix line says how to read the fact by hand, and what to do first
// where something must come first.
func TestUnreadWarningsSayNotRead(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		change func(*tree)
		row    string
		fix    []string
	}{
		{"md_mod's parameters not listed", RoleCn, func(t *tree) {
			t.errs[mdParamsDir] = syscall.EIO
		}, "md format (the 6.19 line)",
			[]string{"read " + mdParamsDir + " by hand", mdFeatureParam}},
		{"md_mod not loaded", RoleCn, mdModGone, "md format (the 6.19 line)",
			[]string{"modprobe raid1", "read " + mdParamsDir + " by hand"}},
		{"lvmconfig fails", RoleDn, func(t *tree) {
			t.cmd("lvmconfig "+strings.Join(lvmConfigArgs, " "), "",
				"Configuration node devices/use_devicesfile not found", 5)
		}, "lvm2 scan",
			[]string{"lvmconfig " + strings.Join(lvmConfigArgs, " ")}},
		{"the devices file not read", RoleDn, func(t *tree) {
			t.errs[lvmDevicesFile] = syscall.EIO
		}, "lvm2 scan", []string{"read " + lvmDevicesFile + " by hand"}},
		{"mdadm.conf not read", RoleCn, func(t *tree) {
			t.errs[confPath] = syscall.EIO
		}, "mdadm.conf AUTO -all",
			[]string{"/etc/mdadm.conf", "/etc/mdadm/mdadm.conf", "by hand"}},
		{"an array's uuid not read", RoleCn, func(t *tree) {
			dnvArray(t)
			t.errs["/sys/block/md5/md/uuid"] = syscall.EIO
		}, "mdadm.conf AUTO -all", []string{"mdadm --detail --scan"}},
		{"a modules-load file not read", RoleDn, func(t *tree) {
			t.errs[modulesFile] = syscall.EIO
		}, "kernel modules at boot",
			[]string{modulesFile, "/usr/lib/modules-load.d", "by hand"}},
		{"io_timeout not there", RoleDn, dropFile(ioTimeoutPath),
			"nvme_core.io_timeout",
			[]string{"modprobe nvme-tcp", "read " + ioTimeoutPath + " by hand"}},
		{"io_timeout not read", RoleCn, func(t *tree) {
			t.errs[ioTimeoutPath] = syscall.EIO
		}, "nvme_core.io_timeout",
			[]string{"read " + ioTimeoutPath + " by hand"}},
		{"the disk's IO size not read", RoleDn, func(t *tree) {
			t.errs[diskQueue+"optimal_io_size"] = syscall.EIO
		}, "disk best IO size",
			[]string{"optimal_io_size", "minimum_io_size", "by hand"}},
	}
	for _, tc := range cases {
		tr := labTree()
		tc.change(tr)
		res, ok := resultNamed(t, runCheck(t, tr, tc.role), tc.row)
		if !ok {
			t.Fatalf("%s: no %q row", tc.name, tc.row)
		}
		if res.Level != LevelWarn || !strings.HasPrefix(res.Detail,
			"not read: ") {
			t.Errorf("%s: %+v, want a warning that starts \"not read: \"",
				tc.name, res)
		}
		for _, want := range tc.fix {
			if !strings.Contains(res.Fix, want) {
				t.Errorf("%s: fix %q does not say %q", tc.name, res.Fix, want)
			}
		}
	}
}

// mdModGone is a kernel with no md: neither loaded nor built in.
func mdModGone(t *tree) {
	delete(t.dirs, sysModuleDir+"/md_mod")
	delete(t.files, mdParamsDir+"/"+mdFeatureParam)
	t.files[modulesDir+"/"+labRelease+"/modules.builtin"] = ""
}

// ownArray gives the node an md array of its own, over a disk partition,
// and conf after AUTO -all in mdadm.conf.
func ownArray(conf string) func(*tree) {
	return func(t *tree) {
		t.files["/sys/block/md0/md/array_state"] = "clean\n"
		t.files["/sys/block/md0/md/uuid"] =
			"5e7d1c2a-0b8f-4e3d-9a6c-2b1f4d3e2a1b\n"
		t.dirs["/sys/block/md0/md/dev-vda2"] = true
		t.files[confPath] = "AUTO -all\n" + conf
	}
}

// A disk node check with no disk named fails each disk row.
func TestNoDiskFailsTheDiskRows(t *testing.T) {
	rep, err := Check(context.Background(), labTree().env(),
		CheckOptions{Role: RoleDn})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, name := range []string{"disk logical block size",
		"disk best IO size"} {
		res, _ := resultNamed(t, rep, name)
		if res.Level != LevelFail || res.Detail != "no disk named" {
			t.Errorf("%s = %+v, want a failure naming no disk", name, res)
		}
	}
}

// Exit statuses: 0 with warnings alone, 1 with a failure, 2 when the check
// could not run; and the two output forms.
func TestCheckExitsAndPrints(t *testing.T) {
	run := func(tr *tree, output string) (string, error) {
		var out bytes.Buffer
		err := RunCheck(context.Background(), tr.env(),
			CheckOptions{Role: RoleCn}, output, &out)
		return out.String(), err
	}
	warned := labTree()
	warned.files[ioTimeoutPath] = "60\n"
	out, err := run(warned, OutputText)
	if err != nil {
		t.Errorf("a warning alone: %v, want exit 0", err)
	}
	if !strings.Contains(out, "WARN  nvme_core.io_timeout: 60 seconds") ||
		!strings.Contains(out, "      fix: set nvme_core.io_timeout") ||
		!strings.Contains(out, "0 failures, 1 warning\n") {
		t.Errorf("text report:\n%s", out)
	}

	failed := labTree()
	failed.files[ioTimeoutPath] = neverIoTimeout + "\n"
	out, err = run(failed, OutputJson)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != ExitFailed {
		t.Errorf("a failure: %v, want exit %d", err, ExitFailed)
	}
	var rep Report
	if jsonErr := json.Unmarshal([]byte(out), &rep); jsonErr != nil {
		t.Fatalf("json report: %v\n%s", jsonErr, out)
	}
	if rep.Role != RoleCn || rep.Failures != 1 {
		t.Errorf("json report: %+v", rep)
	}

	notRoot := labTree()
	notRoot.euid = 1000
	_, err = run(notRoot, OutputText)
	if !errors.As(err, &exit) || exit.Code != ExitNotRun {
		t.Errorf("not root: %v, want exit %d", err, ExitNotRun)
	}
	if len(notRoot.reads)+len(notRoot.commands) != 0 {
		t.Errorf("a check not run as root read the node: %v %v",
			notRoot.reads, notRoot.commands)
	}
	_, err = run(labTree(), "yaml")
	if !errors.As(err, &exit) || exit.Code != ExitNotRun {
		t.Errorf("--output yaml: %v, want exit %d", err, ExitNotRun)
	}
}

// The node check reads; it changes nothing on the node.
func TestCheckChangesNothing(t *testing.T) {
	for _, role := range both {
		tr := labTree()
		runCheck(t, tr, role)
		if len(tr.written) != 0 || len(tr.detached) != 0 {
			t.Errorf("%s: wrote %v, started %v", role, tr.written,
				tr.detached)
		}
		for _, line := range tr.commands {
			for _, verb := range []string{"--create", "--assemble",
				"--stop", "--zero-superblock", "modprobe", "losetup",
				"dmsetup create", "dmsetup remove"} {
				if strings.Contains(line, verb) {
					t.Errorf("%s: ran %q", role, line)
				}
			}
		}
	}
}

// The md replay runs only on a controller node asked for it, and is
// refused when the md assembly mask failed.
func TestReplayOnlyWhenAskedAndMasked(t *testing.T) {
	tr := labTree()
	rep := runCheck(t, tr, RoleCn)
	for _, res := range rep.Results {
		if strings.HasPrefix(res.Name, replayName) {
			t.Errorf("a check with no --md-replay has %+v", res)
		}
	}
	for _, line := range tr.commands {
		if strings.HasPrefix(line, "mktemp") ||
			strings.HasPrefix(line, "dmsetup ls") {
			t.Errorf("a check with no --md-replay ran %q", line)
		}
	}
	unmasked := labTree()
	delete(unmasked.files, maskPath)
	noAuto(unmasked)
	rep, err := Check(context.Background(), unmasked.env(),
		CheckOptions{Role: RoleCn, MdReplay: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	res, ok := resultNamed(t, rep, replayName)
	if !ok || res.Level != LevelFail ||
		!strings.Contains(res.Detail, "mask") {
		t.Errorf("replay row = %+v, want a refusal for the mask", res)
	}
	for _, line := range unmasked.commands {
		if strings.HasPrefix(line, "mktemp") || strings.HasPrefix(line,
			"dmsetup ls") {
			t.Errorf("a refused replay ran %q", line)
		}
	}
}
