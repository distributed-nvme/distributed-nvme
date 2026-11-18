package nodecheck

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/agent/cnagent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// udevRulesDirs are the directories udev reads rules from, in its order of
// precedence: a file of one name in an earlier directory hides the file of
// that name in every later one, and an empty file or a link to /dev/null
// there masks it.
var udevRulesDirs = []string{
	"/etc/udev/rules.d",
	"/run/udev/rules.d",
	"/usr/local/lib/udev/rules.d",
	"/usr/lib/udev/rules.d",
	"/lib/udev/rules.d",
}

// ruleFile is the file udev reads for one rule name: where, and its text
// ("" for an empty file or a link to /dev/null) or the read's error.
type ruleFile struct {
	path    string
	text    string
	readErr error
}

// udevRules is the rule files udev reads, by name.
type udevRules struct {
	files map[string]*ruleFile
}

// effectiveRules reads, for every rule name in udevRulesDirs, the file udev
// reads for it. A listing that fails is the error; a directory that is not
// there holds none. A name that starts with "." is skipped, as udev skips
// it (systemd's conf_files_list).
func effectiveRules(ctx context.Context, oc common.OsClient) (*udevRules,
	error) {
	c := &checker{env: Env{Oc: oc}}
	rules := &udevRules{files: map[string]*ruleFile{}}
	for _, dir := range udevRulesDirs {
		names, ok, err := c.readDir(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
		if !ok {
			continue
		}
		for _, name := range names {
			if !strings.HasSuffix(name, ".rules") ||
				strings.HasPrefix(name, ".") || rules.files[name] != nil {
				continue
			}
			rf := &ruleFile{path: dir + "/" + name}
			text, ok, err := c.readFile(ctx, rf.path)
			switch {
			case err != nil:
				rf.readErr = err
			case !ok:
				rf.readErr = fmt.Errorf("%s: a link to nothing", rf.path)
			default:
				rf.text = text
			}
			rules.files[name] = rf
		}
	}
	return rules, nil
}

// maskRow is the md assembly mask (architecture.md, Components: invocation
// reference), which must keep md's incremental assembly off a dnv leg. The
// check counts a mask in exactly two ways: the shipped mask rule in effect
// (shippedMaskWhy), or the configuration mdadm reads in the strict form
// strictWhy judges, with no ARRAY line of a dnv array (confMask). Any other
// rule or setting is no mask to the check, and the row fails: a rule's text
// cannot be judged soundly, since udev's order of a line's keys, its lookup
// of the program and its substitutions all stand between the text and what
// runs, and a correct node installs the shipped rules. The shipped mask
// rests on the stock assembly rule's skip of a device it marks not ready,
// and on that rule sorting after it, as the tested distribution's mdadm
// ships it; the check does not read the stock rule.
func (c *checker) maskRow(
	ctx context.Context,
	rules *udevRules,
	rulesErr error,
) Result {
	res := Result{Name: "md assembly mask"}
	ruleWhy := shippedMaskWhy(rules, rulesErr)
	if ruleWhy == "" {
		res.Level = LevelOk
		res.Detail = rules.files[mdMaskRuleName].path + ", the shipped mask"
		return res
	}
	conf, confWhy := c.confMask(ctx)
	if confWhy == "" {
		res.Level = LevelOk
		res.Detail = conf.path + " refuses every format in the strict " +
			"form, and no ARRAY line names a dnv array"
		return res
	}
	res.Level = LevelFail
	res.Detail = "no mask the check counts: " + ruleWhy + "; " + confWhy +
		"; the check counts only the shipped mask rule, byte for byte, or " +
		"an mdadm.conf whose first AUTO line refuses every format with " +
		"minus words and no homehost, with no POLICY line that sets auto= " +
		"and no ARRAY line of a dnv array"
	res.Fix = "install deploy/udev/" + mdMaskRuleName + " in " +
		udevRulesDirs[0]
	return res
}

// shippedMaskWhy says why the shipped mask rule is not in effect, "" when
// it is: the file udev reads for its name, found as its rule row finds it
// (shippedRuleRow), byte for byte the shipped text, which a mask of it by an
// empty file or a link to /dev/null is not.
func shippedMaskWhy(rules *udevRules, rulesErr error) string {
	if rulesErr != nil {
		return "the udev rules not read: " + rulesErr.Error()
	}
	rf := rules.files[mdMaskRuleName]
	switch {
	case rf == nil:
		return mdMaskRuleName + " is not installed"
	case rf.readErr != nil:
		return rf.path + " not read: " + rf.readErr.Error()
	case rf.text != shippedRules[mdMaskRuleName]:
		return rf.path + " is not the shipped text byte for byte"
	}
	return ""
}

// confMask says why the configuration mdadm reads is no mask the check
// counts, "" when it is one: the strict form (strictWhy) and no ARRAY line
// that can match a dnv array (dnvArrayLines), which mdadm assembles whatever
// AUTO says.
func (c *checker) confMask(ctx context.Context) (*mdadmConf, string) {
	conf, err := c.readMdadmConf(ctx)
	if err != nil {
		return nil, "mdadm.conf not read: " + err.Error()
	}
	if why, _ := conf.strictWhy(); why != "" {
		return conf, conf.path + " " + why
	}
	lines, err := c.dnvArrayLines(ctx, conf)
	if err != nil {
		return conf, "the node's arrays not read: " + err.Error()
	}
	if len(lines) > 0 {
		return conf, conf.path + " matches a dnv array by the uuid of " +
			strings.Join(lines, "; ") + ", and mdadm assembles an array a " +
			"line matches whatever AUTO says"
	}
	return conf, ""
}

// shippedRuleRow is one of the rules dnv ships, the dm rule, the md
// assembly mask or the md array rule: absent fails, and so does one masked
// by an empty file or a link to /dev/null of its name, which udev reads in
// its place and which holds no line; another text, byte for byte, warns.
func (c *checker) shippedRuleRow(
	name string,
	rules *udevRules,
	rulesErr error,
) Result {
	res := Result{Name: "udev rule " + name}
	fix := "install deploy/udev/" + name + " in " + udevRulesDirs[0]
	if rulesErr != nil {
		res.Level, res.Detail, res.Fix = LevelFail,
			"not read: "+rulesErr.Error(), fix
		return res
	}
	rf := rules.files[name]
	switch {
	case rf == nil:
		res.Level, res.Detail, res.Fix = LevelFail, "not installed", fix
	case rf.readErr != nil:
		res.Level, res.Detail, res.Fix = LevelFail,
			"not read: "+rf.readErr.Error(), fix
	case strings.TrimSpace(rf.text) == "":
		res.Level = LevelFail
		res.Detail = rf.path + " masks it: an empty file or a link to " +
			"/dev/null"
		// A mask in the install directory is replaced by the install; one
		// in a later directory is hidden by it.
		res.Fix = fix + ", where it then hides " + rf.path
		if path.Dir(rf.path) == udevRulesDirs[0] {
			res.Fix = "replace " + rf.path + " with deploy/udev/" + name
		}
	case rf.text != shippedRules[name]:
		res.Level, res.Detail = LevelWarn, rf.path+" is not the shipped text"
		res.Fix = "install deploy/udev/" + name + " over it"
	default:
		res.Level, res.Detail = LevelOk, rf.path
	}
	return res
}

// mdadmConf is the configuration mdadm reads: where (its main file, or the
// directory alone when no main file exists), its first AUTO line's words,
// whether a POLICY line sets auto=, each ARRAY line, and the uuids the
// ARRAY lines name.
type mdadmConf struct {
	path       string
	auto       []string
	hasAuto    bool
	policyAuto bool
	arrays     []mdadmArray
	uuids      map[string]bool
}

// mdadmArray is one ARRAY line as mdadm's arrayline reads it (config.c):
// its device, the first word that starts with "/" or holds no "=", at any
// place; its name=; its uuid=, the first that mdadm's parse_uuid takes, ""
// for none; and whether it has devices= or super-minor=, by which mdadm
// also matches a device.
type mdadmArray struct {
	dev        string
	name       string
	uuid       string
	byDevices  bool
	superMinor bool
}

// mdadmUuid reports whether a uuid= value is one mdadm's parse_uuid takes
// (uuid.c): exactly 32 hex digits, with ":", ".", " " or "-" between.
func mdadmUuid(value string) bool {
	digits := 0
	for _, r := range value {
		switch {
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') ||
			(r >= 'A' && r <= 'F'):
			digits++
		case strings.ContainsRune(":. -", r):
		default:
			return false
		}
	}
	return digits == 32
}

// The configuration mdadm reads (config.c, load_conffile): /etc/mdadm.conf
// and the .conf files of /etc/mdadm.conf.d when that file exists; else
// Debian's /etc/mdadm/mdadm.conf and /etc/mdadm/mdadm.conf.d when that one
// does; and else /etc/mdadm.conf.d alone.
var mdadmConfFiles = [][2]string{
	{"/etc/mdadm.conf", "/etc/mdadm.conf.d"},
	{"/etc/mdadm/mdadm.conf", "/etc/mdadm/mdadm.conf.d"},
}

// readMdadmConf reads mdadm's configuration in mdadm's order. A node with
// none has an empty one.
func (c *checker) readMdadmConf(ctx context.Context) (*mdadmConf, error) {
	conf := &mdadmConf{uuids: map[string]bool{}}
	for _, pair := range mdadmConfFiles {
		text, ok, err := c.readFile(ctx, pair[0])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		conf.path = pair[0]
		conf.parse(text)
		return conf, c.readMdadmConfDir(ctx, conf, pair[1])
	}
	conf.path = mdadmConfFiles[0][1]
	return conf, c.readMdadmConfDir(ctx, conf, conf.path)
}

// readMdadmConfDir reads the .conf files of a configuration directory, in
// the order of their names, as mdadm's conf_file_or_dir does: a name that
// starts with "." or is shorter than six characters is skipped; a directory
// that is not there holds none.
func (c *checker) readMdadmConfDir(ctx context.Context, conf *mdadmConf,
	dir string) error {
	names, _, err := c.readDir(ctx, dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".conf") || strings.HasPrefix(name, ".") ||
			len(name) < 6 {
			continue
		}
		text, _, err := c.readFile(ctx, dir+"/"+name)
		if err != nil {
			return err
		}
		conf.parse(text)
	}
	return nil
}

// mdadmKeyword reports whether word names keyword as mdadm reads a line's
// first word: at least three letters of it, in any case.
func mdadmKeyword(word, keyword string) bool {
	return len(word) >= 3 && len(word) <= len(keyword) &&
		strings.EqualFold(word, keyword[:len(word)])
}

// confWords splits one line of an mdadm configuration into words as
// mdadm's conf_word does (lib.c): at blanks outside quotes, each pair of
// single or double quotes taken away, and a "#" at a word's start beginning
// a comment to the line's end; a "#" inside a word is part of it.
func confWords(line string) []string {
	var words []string
	i := 0
	for i < len(line) {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) || line[i] == '#' {
			break
		}
		var word strings.Builder
		var quote byte
		for i < len(line) && (quote != 0 ||
			(line[i] != ' ' && line[i] != '\t')) {
			c := line[i]
			switch {
			case quote != 0 && c == quote:
				quote = 0
			case quote == 0 && (c == '\'' || c == '"'):
				quote = c
			default:
				word.WriteByte(c)
			}
			i++
		}
		words = append(words, word.String())
	}
	return words
}

// parse takes the AUTO, POLICY and ARRAY lines of one configuration text,
// its words as mdadm reads them (confWords): a line that starts with a
// blank continues the one before, past blank and comment lines. Only the
// first AUTO line counts, as in mdadm; of a POLICY line only whether it
// sets auto= counts, by mdadm's own test of the word (policy.c, try_rule).
func (conf *mdadmConf) parse(text string) {
	var lines [][]string
	for _, raw := range strings.Split(text, "\n") {
		words := confWords(raw)
		if len(words) == 0 {
			continue
		}
		if (raw[0] == ' ' || raw[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] = append(lines[len(lines)-1], words...)
			continue
		}
		lines = append(lines, words)
	}
	for _, words := range lines {
		switch {
		case mdadmKeyword(words[0], "auto"):
			if !conf.hasAuto {
				conf.hasAuto = true
				conf.auto = words[1:]
			}
		case mdadmKeyword(words[0], "policy"):
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "auto=") {
					conf.policyAuto = true
				}
			}
		case mdadmKeyword(words[0], "array"):
			var array mdadmArray
			for _, w := range words[1:] {
				key, value, ok := strings.Cut(w, "=")
				switch {
				case strings.HasPrefix(w, "/") || !ok:
					if array.dev == "" {
						array.dev = w
					}
				case strings.EqualFold(key, "uuid"):
					if array.uuid == "" && mdadmUuid(value) {
						array.uuid = value
						conf.uuids[uuidDigits(value)] = true
					}
				case strings.EqualFold(key, "name"):
					array.name = value
				case strings.EqualFold(key, "devices"):
					array.byDevices = true
				case strings.EqualFold(key, "super-minor"):
					array.superMinor = true
				}
			}
			conf.arrays = append(conf.arrays, array)
		}
	}
}

// mdadmFormats are the metadata formats mdadm's incremental assembly can
// take an array in, by mdadm's names for them.
var mdadmFormats = []string{"0.90", "1.x", "ddf", "imsm"}

// autoWordNames reports whether an AUTO word's version, the word less its
// sign and in lower case, names the format, as mdadm's autoline matches it
// (strcasecmp): the format's own name, a bare major number ("1" for "1.x",
// "0" for "0.90"), or "1." and anything for "1.x".
func autoWordNames(version, format string) bool {
	switch {
	case version == format:
		return true
	case format[1] == '.' && len(version) == 1 && version[0] == format[0]:
		return true
	case format == "1.x" && strings.HasPrefix(version, "1."):
		return true
	}
	return false
}

// strictWhy says why the configuration is not in the strict form the md
// assembly mask counts, "" when it is, and what makes it so: its first AUTO
// line names no homehost, under which mdadm takes an array it judges the
// node's own, and for each format the first word that names it or "all" is
// a minus word (autoRefuses); no POLICY line sets auto=, since the check
// does not judge POLICY lines; and no ARRAY line matches by devices= or
// super-minor= with no uuid, by which mdadm assembles any device the line
// covers whatever AUTO says, and which the check does not judge either;
// every line `mdadm --detail --brief` prints carries a uuid. The
// MDADM_CONF_AUTO environment variable, whose words mdadm puts before the
// AUTO line's and which a rule can set for a device's udev event, is not
// read.
func (conf *mdadmConf) strictWhy() (why, fix string) {
	end := " in " + conf.path + ", and run update-initramfs -u"
	firstAuto := "make the first AUTO line AUTO -all" + end
	if !conf.hasAuto {
		return "has no AUTO line", "write ARRAY lines for the node's own " +
			"arrays, then AUTO -all" + end
	}
	for _, w := range conf.auto {
		if strings.EqualFold(w, "homehost") {
			return "names homehost on its AUTO line", firstAuto
		}
	}
	for _, format := range mdadmFormats {
		if !conf.autoRefuses(format) {
			return "does not refuse the " + format + " format by a minus " +
				"word on its first AUTO line", firstAuto
		}
	}
	if conf.policyAuto {
		return "has a POLICY line that sets auto=, which the check does " +
			"not judge", "remove auto= from every POLICY line" + end
	}
	for _, a := range conf.arrays {
		if a.uuid == "" && (a.byDevices || a.superMinor) {
			return "has an ARRAY line that matches by devices= or " +
					"super-minor= with no uuid, which the check does not judge",
				"give every ARRAY line its array's UUID= (mdadm --detail " +
					"--brief prints it), or remove the line" + end
		}
	}
	return "", ""
}

// autoRefuses reports whether the first word of the AUTO line that names
// the format, or "all", is a minus word, reading the words in order as
// mdadm's autoline does; a "yes" or "no" first, which ends mdadm's reading
// of the line, is not the strict form, nor is a line with no such word.
func (conf *mdadmConf) autoRefuses(format string) bool {
	for _, w := range conf.auto {
		lw := strings.ToLower(w)
		if lw == "yes" || lw == "no" {
			return false
		}
		if len(w) < 2 || (w[0] != '+' && w[0] != '-') {
			continue
		}
		if lw[1:] == "all" || autoWordNames(lw[1:], format) {
			return w[0] == '-'
		}
	}
	return false
}

// uuidDigits is a uuid's hex digits alone, in lower case, so mdadm's
// colon-separated spelling and sysfs's dashed one compare.
func uuidDigits(uuid string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(uuid) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// mdadmConfRow is the requirement that a node's mdadm.conf says AUTO -all,
// written after ARRAY lines for the node's own arrays: AUTO -all keeps
// md's incremental assembly off a leg that carries no dnv superblock (a new
// leg before its create, every leg of a pool without RAID) and off a disk
// node's raw disk, and it also keeps off every array no ARRAY line names.
// The configuration is judged as the md assembly mask judges it, in the
// strict form (strictWhy). Each falls short of it warns.
func (c *checker) mdadmConfRow(ctx context.Context) Result {
	res := Result{Name: "mdadm.conf AUTO -all"}
	conf, err := c.readMdadmConf(ctx)
	if err != nil {
		res.Level, res.Detail = LevelWarn, "not read: "+err.Error()
		res.Fix = "read " + mdadmConfFiles[0][0] + ", " +
			mdadmConfFiles[1][0] + " and their .d directories by hand"
		return res
	}
	if why, fix := conf.strictWhy(); why != "" {
		res.Level = LevelWarn
		res.Detail = conf.path + " " + why + ", so the check does not take " +
			"it to refuse every array, a leg with no dnv superblock among them"
		res.Fix = fix
		return res
	}
	own, dnvUuids, err := c.nodeArrays(ctx)
	if err != nil {
		res.Level = LevelWarn
		res.Detail = "not read: the node's arrays, with AUTO -all set: " +
			err.Error()
		res.Fix = "list the node's arrays by hand (mdadm --detail --scan) " +
			"and check that " + conf.path + " has an ARRAY line for each " +
			"of the node's own arrays and none for a dnv array"
		return res
	}
	if lines := conf.dnvLines(dnvUuids); len(lines) > 0 {
		res.Level = LevelWarn
		res.Detail = conf.path + " matches a dnv array by the uuid of " +
			strings.Join(lines, "; ") + ", and md assembles an array a " +
			"line matches behind the agent, whatever AUTO says"
		res.Fix = "remove every ARRAY line of a dnv array from " + conf.path +
			" and run update-initramfs -u"
		return res
	}
	var unlisted, fixes []string
	for node, uuid := range own {
		if !conf.uuids[uuidDigits(uuid)] {
			unlisted = append(unlisted, node+" ("+uuid+")")
			fixes = append(fixes, "mdadm --detail --brief /dev/"+node)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(fixes)
	if len(unlisted) > 0 {
		res.Level = LevelWarn
		res.Detail = "AUTO -all set, and no ARRAY line names " +
			strings.Join(unlisted, ", ") + ", which then does not start " +
			"at boot"
		res.Fix = "add each one's ARRAY line (" + strings.Join(fixes, "; ") +
			") to " + conf.path + " and run update-initramfs -u"
		return res
	}
	res.Level, res.Detail = LevelOk, conf.path+" says AUTO "+
		strings.Join(conf.auto, " ")
	return res
}

// nodeArrays reads the node's md arrays from sysfs
// (cnagent.Md.ListArrays): its own, those of no dnv agent, by kernel node
// with their uuids, and the uuids of the dnv agents' arrays, each array
// whose members include a dnv dm device, as uuidDigits spells them.
func (c *checker) nodeArrays(ctx context.Context) (map[string]string,
	map[string]bool, error) {
	arrays, err := cnagent.NewMd(c.env.Oc).ListArrays(ctx)
	if err != nil {
		return nil, nil, err
	}
	own, dnv := map[string]string{}, map[string]bool{}
	for _, array := range arrays {
		uuid, _, err := c.readFile(ctx,
			"/sys/block/"+array.Name+"/md/uuid")
		if err != nil {
			return nil, nil, err
		}
		uuid = strings.TrimSpace(uuid)
		if isDnvArray(array) {
			dnv[uuidDigits(uuid)] = true
			continue
		}
		own[array.Name] = uuid
	}
	return own, dnv, nil
}

// dnvMdDevPattern is the name of a dnv array's node under /dev/md
// (common.NameFmt.CnMdDevName).
var dnvMdDevPattern = regexp.MustCompile(`^[0-9a-f]{28}$`)

// dnvArrayLines are the ARRAY lines that can match a dnv array, each as the
// line reads. mdadm matches an ARRAY line to an array by its uuid, its
// devices= or its super-minor, never by its name= or device name, and its
// incremental assembly takes an array a line matches whatever AUTO says.
// So a line with a uuid counts by it: the uuid of a dnv array that runs on
// the node, or any uuid on a line whose name= or device name carries a dnv
// array's, in the exact form of its name (common.CnMdArrayNameGlob) or of
// its node (CnMdDevName), since such a line was copied from a dnv array,
// whose uuid it then matches on whichever node the array's legs appear, a
// standby's among them. The exact form keeps a node's own array out when a
// host called "dnv-cn0" names it "dnv-cn0:0"; a line in dnv's exact form
// counts even when its array runs here with no dnv member, since a dnv
// array that udev put together straight over a leg's NVMe device has none.
// A line with no uuid is the strict form's (strictWhy).
func (c *checker) dnvArrayLines(ctx context.Context,
	conf *mdadmConf) ([]string, error) {
	if len(conf.arrays) == 0 {
		return nil, nil
	}
	_, dnvUuids, err := c.nodeArrays(ctx)
	if err != nil {
		return nil, err
	}
	return conf.dnvLines(dnvUuids), nil
}

// dnvLines are the ARRAY lines that can match a dnv array (dnvArrayLines),
// by the uuids of the dnv arrays running on the node.
func (conf *mdadmConf) dnvLines(dnvUuids map[string]bool) []string {
	var lines []string
	for _, a := range conf.arrays {
		if a.uuid == "" {
			continue
		}
		base := path.Base(a.dev)
		if dnvUuids[uuidDigits(a.uuid)] ||
			(a.name != "" && globMatch(common.CnMdArrayNameGlob, a.name)) ||
			(a.dev != "" && (globMatch(common.CnMdArrayNameGlob, base) ||
				dnvMdDevPattern.MatchString(base))) {
			lines = append(lines, strings.TrimSpace("ARRAY "+a.dev+
				" name="+a.name+" uuid="+a.uuid))
		}
	}
	return lines
}

// globMatch reports whether a value matches a pattern of "|"-separated
// shell globs.
func globMatch(pattern, value string) bool {
	for _, alt := range strings.Split(pattern, "|") {
		if ok, _ := path.Match(alt, value); ok {
			return true
		}
	}
	return false
}

// isDnvArray reports whether an md array is a dnv agent's: a member of it
// is a dnv dm device.
func isDnvArray(array cnagent.MdArray) bool {
	for _, member := range array.Members {
		if strings.HasPrefix(member, common.DmPrefix+"-") {
			return true
		}
	}
	return false
}

// lvmConfigArgs is the `lvmconfig` run that prints lvm2's effective
// devices settings the lvm row judges, defaults included, one
// "name=value" line each.
var lvmConfigArgs = []string{"--typeconfig", "full",
	"devices/use_devicesfile", "devices/filter", "devices/global_filter"}

const lvmDevicesFile = "/etc/lvm/devices/system.devices"

var (
	lvmSettingPattern = regexp.MustCompile(
		`(?m)^\s*(use_devicesfile|filter|global_filter)=(.*)$`)
	lvmFilterItemPattern = regexp.MustCompile(`"([ar])(.)(.*?)"`)
)

// lvmProbeDevices are the names lvm2 reads a dnv dm device, a dnv md
// array and a partition of one by when it scans /dev, which it walks
// whole, every link to a device one more name of it: a dm device's
// /dev/mapper link, kernel node and /dev/block link (the dm rule keeps the
// /dev/disk links off it); an md array's /dev/md link, kernel node,
// /dev/block link and the /dev/disk/by-id links by name and uuid that the
// stock md array rule makes; and the same names of a partition the kernel
// finds on an array.
var lvmProbeDevices = func() [][]string {
	nf := common.NewNameFmt("")
	md := nf.CnMdDevName(1, 1, 1, 0, 0, false)
	byName := "/dev/disk/by-id/md-name-any:" + nf.CnMdArrayName(1, 0, 0,
		false)
	byUuid := "/dev/disk/by-id/md-uuid-01234567:89abcdef:01234567:89abcdef"
	return [][]string{
		{"/dev/mapper/" + nf.DnSideName(1, 1, 1, 1), "/dev/dm-1",
			"/dev/block/252:1"},
		{"/dev/md/" + md, "/dev/md127", "/dev/block/9:127", byName, byUuid},
		{"/dev/md/" + md + "p1", "/dev/md127p1", "/dev/block/259:0",
			byName + "-part1", byUuid + "-part1"},
	}
}()

// lvmRow is the requirement that lvm2 does not scan dnv's devices: a
// devices file in use, or a filter or global filter that rejects them. Each
// absent warns, since an LVM label a host writes would then make the
// controller node's lvm2 take the volume up; a devices file lvm2 uses that
// cannot be read, with no filter that rejects, warns as not read. The
// settings are lvm2's effective ones, as `lvmconfig` prints them; a node
// with no lvm2 has nothing that scans.
func (c *checker) lvmRow(ctx context.Context) Result {
	res := Result{Name: "lvm2 scan"}
	stdout, stderr, exitCode, err := agent.NewCmd(c.env.Oc).Run(ctx,
		"lvmconfig", lvmConfigArgs...)
	switch {
	case errors.Is(err, common.ErrCommandNotFound):
		res.Level, res.Detail = LevelOk, "lvm2 is not installed"
		return res
	case err != nil:
		res.Level = LevelWarn
		res.Detail = fmt.Sprintf("not read: lvmconfig did not answer "+
			"(exit %d): %s", exitCode, strings.TrimSpace(stderr))
		res.Fix = "read lvm2's devices settings by hand: lvmconfig " +
			strings.Join(lvmConfigArgs, " ")
		return res
	}
	settings := map[string]string{}
	for _, m := range lvmSettingPattern.FindAllStringSubmatch(stdout, -1) {
		settings[m[1]] = strings.TrimSpace(m[2])
	}
	useFile := settings["use_devicesfile"] == "1"
	_, devFile, devErr := c.readFile(ctx, lvmDevicesFile)
	if devErr == nil && devFile && useFile {
		res.Level, res.Detail = LevelOk, "lvm2 scans only "+lvmDevicesFile
		return res
	}
	for _, key := range []string{"global_filter", "filter"} {
		if lvmFilterRejects(settings[key]) {
			res.Level, res.Detail = LevelOk, "lvm2's "+key+" rejects dnv's "+
				"devices by every name"
			return res
		}
	}
	if devErr != nil && useFile {
		res.Level = LevelWarn
		res.Detail = "not read: " + lvmDevicesFile + ": " + devErr.Error()
		res.Fix = "read " + lvmDevicesFile + " by hand: lvm2 uses it, and " +
			"it must name only the node's own disks"
		return res
	}
	res.Level = LevelWarn
	res.Detail = "no devices file in use and no filter keeps lvm2 off " +
		"dnv's devices: an LVM label a host writes is taken up here"
	res.Fix = "use an lvm2 devices file (use_devicesfile = 1), or a " +
		"global_filter that accepts the node's own disks and ends with " +
		"\"r|.*|\"; a filter that ends with an accept-all still accepts a " +
		"dnv device by a name it does not reject, such as /dev/dm-N, " +
		"/dev/mdN or a /dev/disk/by-id link"
	return res
}

// lvmFilterRejects reports whether an lvm2 filter list rejects every
// device of lvmProbeDevices, by lvm2's rule: a device is accepted when any
// of its names first matches an "a" item, rejected when a name first
// matches an "r" item and none an "a" item, and accepted when no name
// matches any item.
func lvmFilterRejects(list string) bool {
	items := lvmFilterItemPattern.FindAllStringSubmatch(list, -1)
	if len(items) == 0 {
		return false
	}
	first := func(name string) string {
		for _, item := range items {
			pattern := strings.TrimSuffix(item[3], item[2])
			re, err := regexp.Compile(pattern)
			if err == nil && re.MatchString(name) {
				return item[1]
			}
		}
		return ""
	}
	for _, names := range lvmProbeDevices {
		rejected := false
		for _, name := range names {
			switch first(name) {
			case "a":
				return false
			case "r":
				rejected = true
			}
		}
		if !rejected {
			return false
		}
	}
	return true
}
