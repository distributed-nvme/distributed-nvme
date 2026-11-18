package nodecheck

// The udev rules dnv ships under deploy/udev/, which the node check compares
// a node's rule files with (shippedRuleRow, maskRow; dnagent.md CM5). The binary
// cannot embed a file outside its package's directory, so the texts are
// copies, and TestShippedRulesMatchDeploy holds each copy to its file.

// The names of the shipped rules: the dm rule, the md assembly mask and the
// md array rule (architecture.md, Components: invocation reference).
const (
	dmRuleName      = "58-dnv-dm.rules"
	mdMaskRuleName  = "63-dnv-md.rules"
	mdArrayRuleName = "63-zz-dnv-md-array.rules"
)

// shippedRules is the text of each shipped rule, by its name.
var shippedRules = map[string]string{
	dmRuleName: `# dnv: the device mapper devices of dnv's agents, whose names start "dnv-",
# carry tenants' data: no rule of the system may probe them, link them by
# content, scan them for LVM or register them with bcache. 55-dm.rules has
# set the device's name before this file. The /dev/mapper link stays: the
# agents use it. systemd reads such a device as not ready; nothing of dnv
# waits for its device unit. Install on every node that runs a dnv agent
# (architecture.md, Components: invocation reference).
SUBSYSTEM!="block", GOTO="dnv_dm_end"
KERNEL!="dm-[0-9]*", GOTO="dnv_dm_end"
ACTION!="add|change", GOTO="dnv_dm_end"
ENV{DM_NAME}!="dnv-*", GOTO="dnv_dm_end"
ENV{DM_UDEV_DISABLE_DISK_RULES_FLAG}="1"
ENV{DM_UDEV_DISABLE_OTHER_RULES_FLAG}="1"
ENV{UDEV_DISABLE_PERSISTENT_STORAGE_RULES_FLAG}="1"
OPTIONS:="nowatch"
LABEL="dnv_dm_end"
`,
	mdMaskRuleName: `# dnv: only the dnv agent assembles a dnv md array. A device whose md
# version-1.2 superblock carries a dnv array name, in the exact form dnv
# makes, bare or after "homehost:", is marked not ready, and
# 64-md-raid-assembly.rules then skips it; a node's own array whose name a
# host called dnv-... starts is left alone. --metadata=1.2 reads the
# superblock dnv writes, 4 KiB into the device, and never a 0.90 or 1.0
# superblock a host wrote at the device's end. Install on every node that
# runs a dnv agent (architecture.md, Components: invocation reference).
ACTION=="add|change", SUBSYSTEM=="block", ENV{ID_FS_TYPE}=="linux_raid_member", \
  IMPORT{program}="/sbin/mdadm --examine --export --metadata=1.2 $devnode"
ENV{MD_NAME}=="dnv-????????????????-??-??|*:dnv-????????????????-??-??", ENV{SYSTEMD_READY}="0"
`,
	mdArrayRuleName: `# dnv: a dnv md array starts with blocks a host writes through a thin pool,
# so it can carry a host's md superblock, LVM label, bcache superblock, file
# system or partition table. 63-md-raid-arrays.rules has just read the
# array's name, probed its content and added links by that content; for a
# dnv array, its name in the exact form dnv makes, and each partition the
# kernel found on one, this file sets a file system type no stock rule
# matches, so the md assembly, btrfs, bcache and LVM rules skip it, takes
# the links by content away again, and puts any other link it shares with a
# node's own device below that device. It must sort after
# 63-md-raid-arrays.rules and before every 64- file. The link removal
# (SYMLINK-=) needs systemd 253 or newer. Install on every node that runs a
# dnv agent (architecture.md, Components: invocation reference).
SUBSYSTEM!="block", GOTO="dnv_md_array_end"
KERNEL!="md*", GOTO="dnv_md_array_end"
ACTION!="add|change", GOTO="dnv_md_array_end"
ENV{MD_NAME}!="dnv-????????????????-??-??|*:dnv-????????????????-??-??", GOTO="dnv_md_array_end"
ENV{ID_FS_TYPE}="dnv_md_array"
ENV{ID_FS_USAGE}=""
SYMLINK-="disk/by-uuid/$env{ID_FS_UUID_ENC}"
SYMLINK-="disk/by-label/$env{ID_FS_LABEL_ENC}"
SYMLINK-="disk/by-partuuid/$env{ID_PART_ENTRY_UUID}"
SYMLINK-="disk/by-partlabel/$env{ID_PART_ENTRY_NAME}"
OPTIONS+="link_priority=-100"
LABEL="dnv_md_array_end"
`,
}
