package cnagent

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// The md replay (dnagent.md CM5; architecture.md [D16]): `dnv-agent check cn
// --md-replay` runs, on two small loop devices of a spare machine, the md
// facts the cn agent's safety rests on, with the agent's own Md calls, and
// holds the agent's model of mdadm's start (mdadmStarts) and its superblock
// reader (Md.ReadMember) to what the node's mdadm does and reports at each
// step. It is the cn suite's first redund stage, then the newer leg's pick
// and the forced start.

// The replay's legs and array. A leg is one extent of the smallest size a
// disk node allocates, laid out as a RAID1 group's leg at the default pool
// block: a meta region of replayMetaBlocks blocks, where the superblock
// sits, then data, and a bitmap chunk of replayBitmapChunkBlocks blocks, the
// sizes the cn suite's first redund stage uses.
const (
	replayLegBytes          = common.MinDnExtSize
	replayBlockBytes        = common.DefaultDmPoolDataBlockSize
	replayMetaBlocks        = 3
	replayBitmapChunkBlocks = 128
	// replayArrayName has the form of dnv's array names
	// (common.CnMdArrayNameGlob), so the md assembly mask keeps the node's
	// udev off the replay's legs, as off an agent's. It is no agent array's
	// name: its sp field is zero, and sp ids start at one (architecture.md,
	// Globals: id allocation + shard buckets); its fields, 0xfe, also sort
	// after a pool's last meta group's and differ from the clone field 0xff
	// (TestReplayArrayNameIsMasked).
	replayArrayName = common.DnvPrefix + "-0000000000000000-fe-fe"
	replayDevPath   = "/dev/md/" + replayArrayName
	// replayWait bounds each wait for md to take or let go of a leg.
	replayWait = 10 * time.Second
	// replaySettleSecs bounds each wait for udev after an mdadm call.
	replaySettleSecs = 10
	// mdadmGateWords end mdadm's refusal of a degraded start, the gate the
	// replay holds the agent's model to: "… need all 2 to start it (use
	// --run to insist)."
	mdadmGateWords = "use --run to insist"
)

// mdExamineArgs is the replay's `mdadm --examine --export` of one leg, the
// read its superblock reader is held to, named for the 1.2 format after the
// mode words (mdFormatArgs), never first, as the md assembly mask's examine
// is: the plain examine reads, among every superblock it finds, the one
// made last, a host's 0.90 or 1.0 superblock at a leg's end among them
// (cnagent.md CN12, The superblock format). Named for 1.2, mdadm answers a
// device with no such superblock with exit status 0 and nothing on its
// output, so the replay judges the examine by what it prints (examine),
// never by its exit status alone, and trusts no output of a run whose exit
// status is not 0 (runOut). No replay read names a format to
// `--examine-bitmap` or `--examine-badblocks`, and none runs either: mdadm
// 4.5 aborts on the first so named and crashes on the second, with nothing
// on its output.
func mdExamineArgs(dev string) []string {
	return mdFormatArgs([]string{"--examine", "--export"}, dev)
}

// MdReplayStep is one step of the md replay: its name, whether it held, and
// what it saw.
type MdReplayStep struct {
	Name   string `json:"name"`
	Ok     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// MdReplayReport is the md replay's answer: why it refused to start, when
// it did; each step it ran, the first that failed last; and what its
// teardown left on the node.
type MdReplayReport struct {
	Refused string         `json:"refused,omitempty"`
	Steps   []MdReplayStep `json:"steps"`
	Left    []string       `json:"left,omitempty"`
}

// Failed reports whether the replay refused, a step failed, or its
// teardown left something.
func (r *MdReplayReport) Failed() bool {
	if r.Refused != "" || len(r.Left) > 0 {
		return true
	}
	for _, step := range r.Steps {
		if !step.Ok {
			return true
		}
	}
	return false
}

// replay is one run of the md replay.
type replay struct {
	oc  common.OsClient
	md  *Md
	cmd *agent.Cmd
	rep *MdReplayReport
	dir string
	// files are the legs' backing files, legs the loop devices losetup
	// printed for them.
	files []string
	legs  []string
}

// MdReplay runs the md replay. It refuses to start on a node that holds an
// md array over a dnv dm device or a replay's array, a dnv dm device, a
// subsystem under dnv's NQN prefix, or a file in the local store at
// localStore, since it is for a spare machine, never a node in service. The
// caller has checked that it runs as root and that the md assembly mask
// holds. It always tears down what it made, and names what a teardown that
// failed left.
func MdReplay(
	ctx context.Context,
	oc common.OsClient,
	localStore string,
) *MdReplayReport {
	r := &replay{oc: oc, md: NewMd(oc), cmd: agent.NewCmd(oc),
		rep: &MdReplayReport{}}
	if why := r.refusal(ctx, localStore); why != "" {
		r.rep.Refused = why
		return r.rep
	}
	// The teardown runs on a ctx no signal ends: a replay stopped half-way
	// still removes what it made.
	defer r.teardown(context.WithoutCancel(ctx))
	r.run(ctx)
	return r.rep
}

// refusal is why the replay must not start on this node, "" when it may.
func (r *replay) refusal(ctx context.Context, localStore string) string {
	arrays, err := r.md.ListArrays(ctx)
	if err != nil {
		return "md arrays not read: " + err.Error()
	}
	for _, array := range arrays {
		for _, member := range array.Members {
			if strings.HasPrefix(member, common.DmPrefix+"-") {
				return "md array " + array.Dev + " holds the dnv dm " +
					"device " + member
			}
		}
	}
	names, _, err := r.cmd.ListDir(ctx, "/dev/md")
	if err != nil {
		return "/dev/md not read: " + err.Error()
	}
	if slices.Contains(names, replayArrayName) {
		return "an earlier replay's array " + replayDevPath + " is there"
	}
	dms, err := agent.NewDm(r.oc).List(ctx)
	if err != nil {
		return "dm devices not read: " + err.Error()
	}
	for name := range dms {
		if strings.HasPrefix(name, common.DmPrefix+"-") {
			return "the dnv dm device " + name + " is there"
		}
	}
	subsystems, _, err := r.cmd.ListDir(ctx, agent.NvmetRoot+"/subsystems")
	if err != nil {
		return "nvmet subsystems not read: " + err.Error()
	}
	for _, nqn := range subsystems {
		if strings.HasPrefix(nqn, common.NqnPrefix) {
			return "the dnv nvmet subsystem " + nqn + " is there"
		}
	}
	files, _, err := r.cmd.ListDir(ctx, localStore)
	if err != nil {
		return "the local store " + localStore + " not read: " + err.Error()
	}
	if len(files) > 0 {
		return "the local store " + localStore + " holds " + files[0]
	}
	return ""
}

// step records one step, and reports whether it held.
func (r *replay) step(name string, problem string, detail string) bool {
	ok := problem == ""
	if !ok {
		detail = problem
	}
	r.rep.Steps = append(r.rep.Steps, MdReplayStep{Name: name, Ok: ok,
		Detail: detail})
	return ok
}

// run runs the steps in order and stops at the first that fails.
func (r *replay) run(ctx context.Context) {
	if !r.step("build", r.build(ctx), "two loop devices") {
		return
	}
	leg0, leg1 := r.legs[0], r.legs[1]
	conf := MdCreateConf{
		DevPath:        replayDevPath,
		ArrayName:      replayArrayName,
		RaidDevices:    2,
		BitmapChunkKiB: replayBitmapChunkBlocks * replayBlockBytes / 1024,
		DataOffsetKiB:  replayMetaBlocks * replayBlockBytes / 1024,
		AssumeClean:    true,
	}
	err := r.md.Create(ctx, conf, r.legs)
	r.settle(ctx)
	problem := errProblem("mdadm --create", err)
	if problem == "" {
		_, problem = r.waitArray(ctx, r.legs, true)
	}
	if !r.step("create", problem, "both legs in one array") {
		return
	}
	if !r.step("stop", r.stop(ctx), "no array holds a leg") {
		return
	}
	sbs, problem := r.readLegs(ctx, r.legs)
	if problem == "" && (sbs[0].Events != sbs[1].Events ||
		sbs[0].Uuid != sbs[1].Uuid) {
		problem = fmt.Sprintf("the legs' superblocks differ after the "+
			"stop: events %d and %d, uuid %s and %s", sbs[0].Events,
			sbs[1].Events, sbs[0].Uuid, sbs[1].Uuid)
	}
	if !r.step("superblocks", problem,
		"both legs at one event count, read alike by mdadm and the agent") {
		return
	}
	if !r.assembleStep(ctx, "lone leg refused", []string{leg0}, false, 0) {
		return
	}
	if !r.assembleStep(ctx, "both legs", r.legs, true, 0) {
		return
	}
	err = r.md.Fail(ctx, replayDevPath, leg1)
	r.settle(ctx)
	problem = errProblem("mdadm --fail", err)
	if problem == "" {
		problem = r.stop(ctx)
	}
	if problem == "" {
		sbs, problem = r.readLegs(ctx, r.legs)
	}
	if problem == "" && sbs[0].seesWorking(sbs[1].Role) {
		problem = "the survivor's superblock still counts the failed leg " +
			"as working"
	}
	if problem == "" && !sbs[1].seesWorking(sbs[0].Role) {
		problem = "the failed leg's superblock does not count the " +
			"survivor as working"
	}
	if !r.step("fail one leg", problem,
		"the survivor records the death, the failed leg still counts both") {
		return
	}
	if !r.assembleStep(ctx, "failed leg refused", []string{leg1}, false,
		0) {
		return
	}
	if !r.assembleStep(ctx, "survivor started", []string{leg0}, true, 1) {
		return
	}
	marker := replayMarker()
	problem = r.writeMarker(ctx, marker)
	if problem == "" {
		problem = r.stop(ctx)
	}
	if !r.step("survivor writes", problem,
		"a block written through the degraded array") {
		return
	}
	if !r.assembleStep(ctx, "newer leg picked", []string{leg1, leg0}, true,
		-1) {
		return
	}
	problem = r.readMarker(ctx, marker)
	if problem == "" {
		problem = r.stop(ctx)
	}
	if !r.step("newer leg's data", problem,
		"the array reads the block the survivor wrote") {
		return
	}
	r.forcedStart(ctx, leg0, leg1)
}

// forcedStart is CN12's forced start from the named leg alone: the other
// leg's superblock zeroed first, then the start with a new identity.
func (r *replay) forcedStart(ctx context.Context, named, other string) {
	before := r.md.ReadMember(ctx, named)
	problem := ""
	switch {
	case before.Err != nil:
		problem = before.Err.Error()
	case before.Sb.notFullMember() != "":
		problem = named + ": " + before.Sb.notFullMember()
	}
	if problem == "" {
		err := r.md.ZeroSuperblock(ctx, other)
		r.settle(ctx)
		problem = errProblem("mdadm --zero-superblock", err)
	}
	if problem == "" {
		after := r.md.ReadMember(ctx, other)
		switch {
		case after.Err != nil:
			problem = after.Err.Error()
		case after.Sb != nil:
			problem = other + " still carries a superblock after its zero"
		}
	}
	if problem == "" {
		_, _, found, err := r.examine(ctx, other)
		switch {
		case err != nil:
			problem = err.Error()
		case found:
			problem = other + ": mdadm's examine still finds a 1.2 " +
				"superblock after the zero, the agent's reader none"
		}
	}
	if !r.step("zero", problem, "the other leg carries no superblock") {
		return
	}
	err := r.md.AssembleForced(ctx, replayDevPath, replayArrayName, named,
		mdUpdateUuid)
	r.settle(ctx)
	problem = errProblem("mdadm --assemble --run --force", err)
	if problem == "" {
		problem = r.arrayRuns(ctx, []string{named}, 1)
	}
	if problem == "" {
		after := r.md.ReadMember(ctx, named)
		switch {
		case after.Err != nil:
			problem = after.Err.Error()
		case after.Sb == nil:
			problem = named + " carries no superblock after the start"
		case after.Sb.Uuid == before.Sb.Uuid:
			problem = "the forced start kept the array's identity " +
				before.Sb.Uuid
		}
	}
	if problem == "" {
		problem = r.stop(ctx)
	}
	r.step("forced start", problem,
		"the named leg alone started under a new identity")
}

// build makes a directory, two files of a leg's size in it and a loop
// device over each.
func (r *replay) build(ctx context.Context) string {
	out, err := r.runOut(ctx, "mktemp", "-d", "/var/tmp/dnv-replay.XXXXXX")
	if err != nil {
		return err.Error()
	}
	r.dir = strings.TrimSpace(out)
	for i := 0; i < 2; i++ {
		file := fmt.Sprintf("%s/leg%d.img", r.dir, i)
		r.files = append(r.files, file)
		if _, err := r.runOut(ctx, "truncate", "-s",
			strconv.FormatUint(replayLegBytes, 10), file); err != nil {
			return err.Error()
		}
		dev, err := r.runOut(ctx, "losetup", "--find", "--show", file)
		if err != nil {
			return err.Error()
		}
		r.legs = append(r.legs, strings.TrimSpace(dev))
	}
	return ""
}

// runOut runs one command under the soft timeout and returns its stdout,
// or an error that carries its output.
func (r *replay) runOut(
	ctx context.Context,
	name string,
	args ...string,
) (string, error) {
	stdout, stderr, exitCode, err := r.cmd.Run(ctx, name, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s: exit %d: %s %v", name,
			strings.Join(args, " "), exitCode, strings.TrimSpace(stderr), err)
	}
	return stdout, nil
}

// settle waits for udev to handle the events an mdadm call made, as the cn
// suite does after each, so the next call never meets a device udev holds
// open. A settle that fails is no step's fault.
func (r *replay) settle(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx,
		(replaySettleSecs+2)*time.Second)
	defer cancel()
	r.oc.RunCommand(sctx, "udevadm",
		[]string{"settle", "--timeout=" + strconv.Itoa(replaySettleSecs)}, "")
}

// errProblem is a failed call's problem, "" when it did not fail.
func errProblem(what string, err error) string {
	if err == nil {
		return ""
	}
	return what + ": " + err.Error()
}

// holderArray is the md array that holds a leg, "" when none does.
func (r *replay) holderArray(ctx context.Context, leg string) (string, error) {
	name := leg[strings.LastIndex(leg, "/")+1:]
	holders, _, err := r.cmd.ListDir(ctx, sysfsBlockDir+"/"+name+"/holders")
	if err != nil {
		return "", err
	}
	for _, holder := range holders {
		if mdBlockEntryPattern.MatchString(holder) {
			return holder, nil
		}
	}
	return "", nil
}

// waitArray waits until an array holds every leg of legs, when held, or
// until none holds any, and returns the array's kernel node.
func (r *replay) waitArray(
	ctx context.Context,
	legs []string,
	held bool,
) (string, string) {
	deadline := time.Now().Add(replayWait)
	for {
		node, holding, err := "", 0, error(nil)
		for _, leg := range legs {
			var holder string
			holder, err = r.holderArray(ctx, leg)
			if err != nil {
				break
			}
			if holder != "" {
				holding++
				node = holder
			}
		}
		switch {
		case err != nil:
			return "", "holders not read: " + err.Error()
		case held && holding == len(legs):
			return node, ""
		case !held && holding == 0:
			return "", ""
		}
		if time.Now().After(deadline) {
			if held {
				return "", fmt.Sprintf("after %s an array holds %d of %d "+
					"legs", replayWait, holding, len(legs))
			}
			return "", fmt.Sprintf("after %s %s still holds a leg",
				replayWait, node)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err().Error()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stop stops the replay's array and waits until it holds no leg.
func (r *replay) stop(ctx context.Context) string {
	err := r.md.Stop(ctx, replayDevPath)
	r.settle(ctx)
	if problem := errProblem("mdadm --stop", err); problem != "" {
		return problem
	}
	_, problem := r.waitArray(ctx, r.legs, false)
	return problem
}

// examine is mdadm's examine of a leg (mdExamineArgs): its export lines by
// key, its whole output, and whether it found a 1.2 superblock, which the
// output alone says, by the array identity it prints (MD_UUID). A run that
// fails is the error.
func (r *replay) examine(ctx context.Context, leg string) (map[string]string,
	string, bool, error) {
	out, err := r.runOut(ctx, "mdadm", mdExamineArgs(leg)...)
	if err != nil {
		return nil, "", false, err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[key] = value
		}
	}
	return fields, out, fields["MD_UUID"] != "", nil
}

// readLegs reads each leg's superblock with the agent's reader and holds it
// to mdadm's examine of the leg: the array's identity, its change count and
// its number of places.
func (r *replay) readLegs(
	ctx context.Context,
	legs []string,
) ([]*mdSuperblock, string) {
	var sbs []*mdSuperblock
	for _, leg := range legs {
		reading := r.md.ReadMember(ctx, leg)
		if reading.Err != nil {
			return nil, reading.Err.Error()
		}
		if reading.Sb == nil {
			return nil, leg + ": the agent reads no superblock"
		}
		fields, _, found, err := r.examine(ctx, leg)
		if err != nil {
			return nil, err.Error()
		}
		if !found {
			return nil, leg + ": mdadm's examine finds no 1.2 superblock, " +
				"the agent's reader finds one"
		}
		sb := reading.Sb
		for _, pair := range [][3]string{
			{"MD_UUID", fields["MD_UUID"], sb.Uuid},
			{"MD_EVENTS", fields["MD_EVENTS"],
				strconv.FormatUint(sb.Events, 10)},
			{"MD_DEVICES", fields["MD_DEVICES"],
				strconv.FormatUint(uint64(sb.RaidDisks), 10)},
		} {
			if pair[1] != pair[2] {
				return nil, fmt.Sprintf("%s: mdadm's examine says %s=%s, "+
					"the agent's reader %s", leg, pair[0], pair[1], pair[2])
			}
		}
		sbs = append(sbs, sb)
	}
	return sbs, ""
}

// assembleStep runs the agent's plain assembly (Md.Assemble) of legs, in
// that order, and holds it to three things: what the lab's mdadm does,
// start or not (want); what the agent's model of mdadm says it does
// (mdadmStarts), from the superblocks read before; and, for a refusal,
// mdadm's own gate as its cause, no array holding a leg after it, and each
// leg's superblock unchanged. A start must leave the array running with
// degraded members, when degraded is not negative.
func (r *replay) assembleStep(
	ctx context.Context,
	name string,
	legs []string,
	want bool,
	degraded int,
) bool {
	sbs, problem := r.readLegs(ctx, legs)
	if problem != "" {
		return r.step(name, problem, "")
	}
	var before []string
	for _, leg := range legs {
		_, out, _, err := r.examine(ctx, leg)
		if err != nil {
			return r.step(name, err.Error(), "")
		}
		before = append(before, out)
	}
	model := mdadmStarts(sbs)
	err := r.md.Assemble(ctx, replayDevPath, replayArrayName, legs)
	r.settle(ctx)
	started := err == nil
	verb := map[bool]string{true: "starts", false: "refuses"}
	switch {
	case started != want:
		problem = fmt.Sprintf("mdadm %s the array from %s, which the "+
			"tested mdadm %s", verb[started], strings.Join(legs, " "),
			verb[want])
		if err != nil {
			problem += ": " + err.Error()
		}
	case model != started:
		problem = fmt.Sprintf("the agent's model of mdadm's start says "+
			"mdadm %s the array from %s, and mdadm %s it", verb[model],
			strings.Join(legs, " "), verb[started])
	case !started && !strings.Contains(err.Error(), mdadmGateWords):
		problem = "mdadm refused for another cause than its degraded " +
			"gate: " + err.Error()
	case !started:
		problem = r.unchanged(ctx, legs, before)
	default:
		problem = r.arrayRuns(ctx, legs, degraded)
	}
	return r.step(name, problem, fmt.Sprintf("mdadm and the agent's model "+
		"both %s the array from %s", verb[started], strings.Join(legs, " ")))
}

// unchanged reports what a refused assembly changed on the legs: an array
// left holding one, or a superblock mdadm's examine reads differently.
func (r *replay) unchanged(
	ctx context.Context,
	legs []string,
	before []string,
) string {
	if _, problem := r.waitArray(ctx, legs, false); problem != "" {
		return problem
	}
	for i, leg := range legs {
		_, out, _, err := r.examine(ctx, leg)
		if err != nil {
			return err.Error()
		}
		if out != before[i] {
			return leg + ": the refused start changed its superblock"
		}
	}
	return ""
}

// arrayRuns reports what is wrong with the array the replay started over
// legs: not running, or not degraded by want members when want is not
// negative.
func (r *replay) arrayRuns(ctx context.Context, legs []string,
	want int) string {
	node, problem := r.waitArray(ctx, legs[len(legs)-1:], true)
	if problem != "" {
		return problem
	}
	dir := mdDirOf(node)
	state, _, err := r.cmd.ReadAttr(ctx, dir+"/array_state")
	if err != nil {
		return err.Error()
	}
	if !mdRunning(state) {
		return node + " reads array_state " + state
	}
	if want < 0 {
		return ""
	}
	degraded, _, err := r.cmd.ReadAttr(ctx, dir+"/degraded")
	if err != nil {
		return err.Error()
	}
	if degraded != strconv.Itoa(want) {
		return fmt.Sprintf("%s reads degraded %s, want %d", node, degraded,
			want)
	}
	return ""
}

// replayMarker is the block the survivor writes through the degraded
// array, which the newer leg's start must read back.
func replayMarker() []byte {
	block := bytes.Repeat([]byte("dnv-replay newer leg "), 4096/21+1)
	return block[:4096]
}

// writeMarker writes the marker at the start of the running array.
func (r *replay) writeMarker(ctx context.Context, marker []byte) string {
	node, problem := r.waitArray(ctx, r.legs[:1], true)
	if problem != "" {
		return problem
	}
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	if err := r.oc.WriteBlock(cctx, "/dev/"+node, 0, marker); err != nil {
		return "write /dev/" + node + ": " + err.Error()
	}
	return ""
}

// readMarker reads the start of the running array, bypassing the page
// cache, and compares it with the marker.
func (r *replay) readMarker(ctx context.Context, marker []byte) string {
	node, problem := r.waitArray(ctx, r.legs[:1], true)
	if problem != "" {
		return problem
	}
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	data, err := r.oc.ReadBlockDirect(cctx, "/dev/"+node, 0,
		uint64(len(marker)))
	if err != nil {
		return "read /dev/" + node + ": " + err.Error()
	}
	if !bytes.Equal(data, marker) {
		return "the array does not read the block the survivor wrote: " +
			"mdadm started it from the older leg"
	}
	return ""
}

// teardown stops the replay's array, detaches its loop devices, those
// losetup printed and any other over its files, and removes its files, and
// names in the report what it could not remove.
func (r *replay) teardown(ctx context.Context) {
	for _, leg := range r.legs {
		holder, err := r.holderArray(ctx, leg)
		if err != nil {
			r.rep.Left = append(r.rep.Left, leg+": holders not read: "+
				err.Error())
			continue
		}
		if holder == "" {
			continue
		}
		err = r.md.Stop(ctx, "/dev/"+holder)
		r.settle(ctx)
		if err != nil {
			r.rep.Left = append(r.rep.Left, "/dev/"+holder+" still runs: "+
				err.Error())
		}
	}
	failed := map[string]bool{}
	for _, leg := range r.legs {
		if _, err := r.runOut(ctx, "losetup", "-d", leg); err != nil {
			failed[leg] = true
			r.rep.Left = append(r.rep.Left, leg+" still attached: "+
				err.Error())
		}
	}
	// A loop device losetup attached but never printed, as one whose
	// losetup a signal cut short, is found by its file.
	for _, file := range r.files {
		out, err := r.runOut(ctx, "losetup", "-j", file)
		if err != nil {
			r.rep.Left = append(r.rep.Left, file+": its loop devices not "+
				"listed: "+err.Error())
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			dev, _, _ := strings.Cut(strings.TrimSpace(line), ":")
			if dev == "" || failed[dev] {
				continue
			}
			if _, err := r.runOut(ctx, "losetup", "-d", dev); err != nil {
				r.rep.Left = append(r.rep.Left, dev+" still attached: "+
					err.Error())
			}
		}
	}
	if r.dir != "" {
		if _, err := r.runOut(ctx, "rm", "-rf", r.dir); err != nil {
			r.rep.Left = append(r.rep.Left, r.dir+" still there: "+
				err.Error())
		}
	}
}
