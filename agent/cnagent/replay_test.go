package cnagent

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
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// replayStandIn is a stand-in node for the md replay: loop devices over
// files, and an mdadm that keeps each leg's superblock (rendered as md lays
// it out, fakeSuperblockArea) and decides a plain assembly by the lab's
// mdadm's degraded gate, with switches for a node whose mdadm or kernel
// does otherwise.
type replayStandIn struct {
	mu    sync.Mutex
	calls []string

	loops    map[string]string
	nextLoop int
	sbs      map[string]*replaySb
	ids      int
	// running is the array's members while it runs, its source the member
	// it started from, whose data it reads.
	running []string
	source  string
	data    map[string][]byte

	// The node's other objects, which the replay must refuse.
	dms        []string
	dnvArray   bool
	leftover   bool
	subsystems []string
	storeFiles []string

	// runAlways is an mdadm that adds --run to every assembly, as a wrapper
	// first in PATH can; pickOlder one that starts from the older leg;
	// examineEventsOff an examine that misreports the change count;
	// bblRefused a leg whose bad-block list the kernel refuses to import,
	// which this mdadm ignores; failDetach a losetup -d that fails.
	runAlways        bool
	pickOlder        bool
	examineEventsOff uint64
	bblRefused       string
	failDetach       bool
	// lostAttach is a losetup that attaches its first loop device and is
	// cut short before it prints it.
	lostAttach bool
	// refuseWords, when set, is what this mdadm says when its gate refuses
	// a start, in place of the lab's mdadm's words.
	refuseWords string
	// foreignTail puts a host's newer 1.0 superblock at every leg's end,
	// which a plain examine reads in place of the leg's 1.2 one;
	// zeroGhost is an examine that still finds a 1.2 superblock on a leg
	// after its zero, while the leg's bytes carry none.
	foreignTail bool
	zeroGhost   map[string]*replaySb
}

// replaySb is one leg's superblock: the array's identity, its change
// count, its place and its view of the array's places.
type replaySb struct {
	id         string
	events     uint64
	place      int
	arrayState string
}

func newReplayStandIn() *replayStandIn {
	return &replayStandIn{loops: map[string]string{},
		sbs: map[string]*replaySb{}, data: map[string][]byte{}}
}

func (s *replayStandIn) record(line string) {
	s.calls = append(s.calls, line)
}

func (s *replayStandIn) called(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.calls {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func notExist(path string) error {
	return &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

func (s *replayStandIn) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(_ context.Context, name string, args []string,
			_ string) (string, string, int, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.record(strings.TrimSpace(name + " " + strings.Join(args, " ")))
			stdout, stderr, code := s.run(name, args)
			if code != 0 {
				return stdout, stderr, code,
					fmt.Errorf("exit status %d", code)
			}
			return stdout, stderr, 0, nil
		},
		ReadDirFn: func(_ context.Context, path string) ([]string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.readDir(path)
		},
		ReadFileFn: func(_ context.Context, path string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.readFile(path)
		},
		ReadBlockDirectFn: func(_ context.Context, path string, offset,
			length uint64) ([]byte, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.readBlock(path, offset, length)
		},
		WriteBlockFn: func(_ context.Context, path string, offset uint64,
			data []byte) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.record("writeblock " + path)
			if path != "/dev/md127" || s.running == nil || offset != 0 {
				return fmt.Errorf("no array at %s", path)
			}
			for _, leg := range s.running {
				s.data[leg] = append([]byte{}, data...)
			}
			return nil
		},
	}
}

func (s *replayStandIn) run(name string, args []string) (string, string,
	int) {
	switch name {
	case "mktemp":
		return "/var/tmp/dnv-replay.abc123\n", "", 0
	case "truncate", "rm", "udevadm":
		return "", "", 0
	case "losetup":
		switch args[0] {
		case "-d":
			if s.failDetach {
				return "", "losetup: " + args[1] + ": detach failed", 1
			}
			delete(s.loops, args[1])
			return "", "", 0
		case "-j":
			var out strings.Builder
			for dev, file := range s.loops {
				if file == args[1] {
					fmt.Fprintf(&out, "%s: []: (%s)\n", dev, file)
				}
			}
			return out.String(), "", 0
		}
		dev := fmt.Sprintf("/dev/loop%d", s.nextLoop)
		s.nextLoop++
		s.loops[dev] = args[len(args)-1]
		if s.lostAttach {
			s.lostAttach = false
			return "", "", -1
		}
		return dev + "\n", "", 0
	case "dmsetup":
		if len(s.dms) == 0 {
			return "No devices found\n", "", 0
		}
		var b strings.Builder
		for i, dm := range s.dms {
			fmt.Fprintf(&b, "%s\t(253:%d)\n", dm, i)
		}
		return b.String(), "", 0
	case "mdadm":
		return s.mdadm(args)
	}
	return "", name + ": not modelled", 127
}

// legsOf are the loop devices among an mdadm call's arguments, in order.
func legsOf(args []string) []string {
	var out []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "/dev/loop") {
			out = append(out, arg)
		}
	}
	return out
}

func (s *replayStandIn) newId() string {
	s.ids++
	return fmt.Sprintf("replay-array-%d", s.ids)
}

// setRunning starts the array over members from source, and writes each
// member's superblock: one change more, the array's identity, and a view of
// its places that counts each member running.
func (s *replayStandIn) setRunning(members []string, source, id string) {
	s.running, s.source = members, source
	s.bump(id)
}

// bump writes every running member's superblock once more, as md does on
// each change of the array.
func (s *replayStandIn) bump(id string) {
	state := []byte("..")
	for _, leg := range s.running {
		state[s.sbs[leg].place] = 'A'
	}
	for _, leg := range s.running {
		sb := s.sbs[leg]
		sb.events++
		sb.arrayState = string(state)
		if id != "" {
			sb.id = id
		}
	}
}

func (s *replayStandIn) mdadm(args []string) (string, string, int) {
	flagged := slices.Contains(args, "--metadata=1.2")
	switch {
	case len(args) > 0 && strings.HasPrefix(args[0], "--metadata"):
		return "", "mdadm: --metadata does not set the mode, and so cannot " +
			"be the first option.\n", 2
	case slices.Contains(args, "--examine-bitmap") && flagged:
		// mdadm 4.5 aborts on a format named to --examine-bitmap.
		return "", "corrupted size vs. prev_size\n", 134
	case slices.Contains(args, "--examine-badblocks") &&
		(flagged || slices.Contains(args, "-e")):
		// It crashes on one named to --examine-badblocks, printing nothing.
		return "", "", 139
	}
	switch {
	case slices.Contains(args, "--version"):
		return "", "mdadm - v4.5\n", 0
	case slices.Contains(args, "--create"):
		id := s.newId()
		legs := legsOf(args)
		for i, leg := range legs {
			s.sbs[leg] = &replaySb{id: id, place: i}
			s.data[leg] = make([]byte, 4096)
		}
		s.setRunning(legs, legs[0], "")
		return "", "mdadm: array " + replayDevPath + " started.\n", 0
	case slices.Contains(args, "--stop"):
		if s.running == nil {
			return "", "mdadm: error opening " + args[len(args)-1], 1
		}
		s.bump("")
		s.running = nil
		return "", "mdadm: stopped " + args[len(args)-1] + "\n", 0
	case slices.Contains(args, "--fail"):
		leg := args[len(args)-1]
		s.running = slices.DeleteFunc(slices.Clone(s.running),
			func(l string) bool { return l == leg })
		s.bump("")
		return "", "mdadm: set " + leg + " faulty\n", 0
	case slices.Contains(args, "--zero-superblock"):
		leg := args[len(args)-1]
		if s.zeroGhost != nil {
			s.zeroGhost[leg] = s.sbs[leg]
		}
		delete(s.sbs, leg)
		return "", "", 0
	case slices.Contains(args, "--examine"):
		leg := args[len(args)-1]
		sb := s.sbs[leg]
		if sb == nil && s.zeroGhost != nil {
			sb = s.zeroGhost[leg]
		}
		switch {
		case s.foreignTail && !flagged:
			return fmt.Sprintf("MD_LEVEL=raid1\nMD_DEVICES=2\n"+
				"MD_NAME=any:host-array\nMD_UUID=%s\nMD_EVENTS=7\n",
				fakeUuidText("host-array")), "", 0
		case sb == nil && flagged:
			// mdadm 4.5, named for 1.2, answers a device with none with
			// exit status 0 and nothing on its output.
			return "", "mdadm: No super block found on " + leg +
				" (Expected magic a92b4efc, got 00000000)\n", 0
		case sb == nil:
			return "", "mdadm: No md superblock detected on " + leg + ".\n",
				1
		}
		return fmt.Sprintf("MD_LEVEL=raid1\nMD_DEVICES=2\n"+
			"MD_NAME=any:"+replayArrayName+"\nMD_UUID=%s\nMD_UPDATE_TIME=%d\n"+
			"MD_EVENTS=%d\n", fakeUuidText(sb.id), sb.events,
			sb.events+s.examineEventsOff), "", 0
	case slices.Contains(args, "--assemble"):
		return s.assemble(args)
	}
	return "", "mdadm: not modelled", 1
}

// assemble is the stand-in's assembly: the newest member by change count
// decides how many working members the array needs; a member counts when
// its change count is at most one below and its view sees the newest's
// place working; the gate holds unless --run is given or the wrapper adds
// it.
func (s *replayStandIn) assemble(args []string) (string, string, int) {
	legs := legsOf(args)
	var newest, oldest string
	for _, leg := range legs {
		sb := s.sbs[leg]
		if sb == nil {
			return "", "mdadm: no recogniseable superblock on " + leg, 1
		}
		if newest == "" || sb.events > s.sbs[newest].events {
			newest = leg
		}
		if oldest == "" || sb.events < s.sbs[oldest].events {
			oldest = leg
		}
	}
	nsb := s.sbs[newest]
	need := strings.Count(nsb.arrayState, "A")
	var counted []string
	for _, leg := range legs {
		sb := s.sbs[leg]
		if sb.events+1 < nsb.events ||
			sb.arrayState[nsb.place] != 'A' {
			continue
		}
		counted = append(counted, leg)
	}
	forced := slices.Contains(args, "--run") || s.runAlways
	if !forced && len(counted) < need && s.refuseWords != "" {
		return "", s.refuseWords, 1
	}
	if !forced && len(counted) < need {
		return "", fmt.Sprintf("mdadm: "+replayDevPath+" assembled from "+
			"%d drive - need all %d to start it (use --run to insist).\n",
			len(counted), need), 1
	}
	source := newest
	if s.pickOlder {
		source = oldest
	}
	members := counted
	if forced {
		members = legs
	}
	id := ""
	if slices.Contains(args, "--update=uuid") {
		id = s.newId()
	}
	s.setRunning(members, source, id)
	return "", "mdadm: " + replayDevPath + " has been started.\n", 0
}

func (s *replayStandIn) readDir(path string) ([]string, error) {
	switch {
	case path == "/sys/block":
		names := []string{}
		for dev := range s.loops {
			names = append(names, strings.TrimPrefix(dev, "/dev/"))
		}
		if s.running != nil {
			names = append(names, "md127")
		}
		if s.dnvArray {
			names = append(names, "md126")
		}
		sort.Strings(names)
		return names, nil
	case path == "/sys/block/md126/md":
		return []string{"array_state", "dev-dm-3"}, nil
	case path == "/sys/block/md127/md" && s.running != nil:
		names := []string{"array_state", "degraded"}
		for _, leg := range s.running {
			names = append(names, "dev-"+strings.TrimPrefix(leg, "/dev/"))
		}
		return names, nil
	case strings.HasSuffix(path, "/holders"):
		leg := "/dev/" + strings.TrimSuffix(strings.TrimPrefix(path,
			"/sys/block/"), "/holders")
		if _, ok := s.loops[leg]; !ok {
			return nil, notExist(path)
		}
		if slices.Contains(s.running, leg) {
			return []string{"md127"}, nil
		}
		return []string{}, nil
	case path == "/dev/md":
		var names []string
		if s.running != nil || s.leftover {
			names = append(names, replayArrayName)
		}
		return names, nil
	case path == agent.NvmetRoot+"/subsystems":
		return s.subsystems, nil
	case path == "/var/lib/dnv":
		return s.storeFiles, nil
	}
	return nil, notExist(path)
}

func (s *replayStandIn) readFile(path string) (string, error) {
	switch path {
	case "/sys/block/md126/md/array_state":
		return "clean\n", nil
	case "/sys/block/md126/md/dev-dm-3/block/dm/name":
		return "dnv-0000000000000001-0000000000000011-c9-" +
			"0000000000000001-0000000000000002\n", nil
	case "/sys/block/md127/md/array_state":
		if s.running != nil {
			return "clean\n", nil
		}
	case "/sys/block/md127/md/degraded":
		if s.running != nil {
			return strconv.Itoa(2-len(s.running)) + "\n", nil
		}
	}
	return "", notExist(path)
}

func (s *replayStandIn) readBlock(path string, offset, length uint64) (
	[]byte, error) {
	if path == "/dev/md127" {
		if s.running == nil {
			return nil, notExist(path)
		}
		return append([]byte{}, s.data[s.source][:length]...), nil
	}
	if _, ok := s.loops[path]; !ok {
		return nil, notExist(path)
	}
	sb := s.sbs[path]
	refused := path == s.bblRefused
	switch {
	case offset == mdSbOffset && sb == nil:
		return make([]byte, length), nil
	case offset == mdSbOffset:
		facts := fakeSuperblock{role: fmt.Sprintf("Active device %d",
			sb.place), events: sb.events, arrayState: sb.arrayState}
		return fakeSuperblockArea(sb.id, facts, refused)[:length], nil
	case refused:
		return fakeBadBlockList(0, true)[:length], nil
	}
	return make([]byte, length), nil
}

func runReplay(t *testing.T, s *replayStandIn) *MdReplayReport {
	t.Helper()
	return MdReplay(context.Background(), s.osClient(), "/var/lib/dnv")
}

// stepNames are the names of the replay's steps, in order.
func stepNames(rep *MdReplayReport) []string {
	var out []string
	for _, step := range rep.Steps {
		out = append(out, step.Name)
	}
	return out
}

// lastStep is the replay's last step, the one that failed when one did.
func lastStep(t *testing.T, rep *MdReplayReport) MdReplayStep {
	t.Helper()
	if len(rep.Steps) == 0 {
		t.Fatalf("the replay ran no step: %+v", rep)
	}
	return rep.Steps[len(rep.Steps)-1]
}

// assertTornDown checks that the replay left no array, no loop device and
// named nothing left.
func assertTornDown(t *testing.T, s *replayStandIn, rep *MdReplayReport) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != nil {
		t.Errorf("an array still runs over %v", s.running)
	}
	if len(s.loops) != 0 {
		t.Errorf("loop devices left: %v", s.loops)
	}
	if len(rep.Left) != 0 {
		t.Errorf("the report names leftovers: %v", rep.Left)
	}
	if !slices.Contains(s.calls, "rm -rf /var/tmp/dnv-replay.abc123") {
		t.Errorf("the replay's directory was not removed: %v", s.calls)
	}
}

// The replay passes on a node whose mdadm keeps the lab's gate: every step,
// in order, and a teardown that leaves nothing.
func TestMdReplayPassesOnTheLabsMdadm(t *testing.T) {
	s := newReplayStandIn()
	rep := runReplay(t, s)
	if rep.Failed() {
		t.Fatalf("the replay failed: %+v", rep)
	}
	want := []string{"build", "create", "stop", "superblocks",
		"lone leg refused", "both legs", "fail one leg",
		"failed leg refused", "survivor started", "survivor writes",
		"newer leg picked", "newer leg's data", "zero", "forced start"}
	if got := stepNames(rep); !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
	assertTornDown(t, s, rep)
	// The forced start runs the agent's own forced form, named for the 1.2
	// format.
	if !s.called("mdadm --assemble " + replayDevPath + " --metadata=1.2" +
		" --name " + replayArrayName + " --run --force --update=uuid") {
		t.Errorf("no forced start in the agent's form: %v", s.calls)
	}
}

// An mdadm that starts a lone clean leg, as a wrapper that adds --run to
// every assembly makes it, fails the replay at that step, and the teardown
// stops the array it started.
func TestMdReplayFailsWhenMdadmStartsALoneLeg(t *testing.T) {
	s := newReplayStandIn()
	s.runAlways = true
	rep := runReplay(t, s)
	if !rep.Failed() {
		t.Fatalf("the replay passed over an mdadm that starts a lone leg")
	}
	last := lastStep(t, rep)
	if last.Name != "lone leg refused" || last.Ok ||
		!strings.Contains(last.Detail, "starts the array") {
		t.Errorf("last step = %+v, want the lone leg's step failed", last)
	}
	assertTornDown(t, s, rep)
}

// The superblock reader is held to mdadm's examine: a change count that
// differs fails the replay, naming the key.
func TestMdReplayHoldsTheReaderToMdadmsExamine(t *testing.T) {
	s := newReplayStandIn()
	s.examineEventsOff = 1
	rep := runReplay(t, s)
	last := lastStep(t, rep)
	if last.Name != "superblocks" || last.Ok ||
		!strings.Contains(last.Detail, "MD_EVENTS") {
		t.Errorf("last step = %+v, want the superblocks' step failed on "+
			"MD_EVENTS", last)
	}
	assertTornDown(t, s, rep)
}

// The agent's model of mdadm's start is held to mdadm: an mdadm that starts
// both legs while one's bad-block list is one the model counts refused
// fails the step, naming the model.
func TestMdReplayHoldsTheModelToMdadm(t *testing.T) {
	s := newReplayStandIn()
	s.bblRefused = "/dev/loop1"
	rep := runReplay(t, s)
	last := lastStep(t, rep)
	if last.Name != "both legs" || last.Ok ||
		!strings.Contains(last.Detail, "the agent's model") {
		t.Errorf("last step = %+v, want the both legs' step failed on "+
			"the model", last)
	}
	assertTornDown(t, s, rep)
}

// A refusal counts only for mdadm's degraded gate: one for another cause,
// as a name mdadm reads differently, fails the step.
func TestMdReplayHoldsARefusalToItsCause(t *testing.T) {
	s := newReplayStandIn()
	s.refuseWords = "mdadm: /dev/loop0 has wrong name.\n"
	rep := runReplay(t, s)
	last := lastStep(t, rep)
	if last.Name != "lone leg refused" || last.Ok ||
		!strings.Contains(last.Detail, "another cause") {
		t.Errorf("last step = %+v, want the lone leg's step failed on the "+
			"refusal's cause", last)
	}
	assertTornDown(t, s, rep)
}

// An mdadm that starts the array from the older leg when both return fails
// the replay: the array does not read what the survivor wrote.
func TestMdReplayFailsWhenTheOlderLegIsPicked(t *testing.T) {
	s := newReplayStandIn()
	s.pickOlder = true
	rep := runReplay(t, s)
	last := lastStep(t, rep)
	if last.Name != "newer leg's data" || last.Ok {
		t.Errorf("last step = %+v, want the newer leg's data step failed",
			last)
	}
	assertTornDown(t, s, rep)
}

// The replay refuses a node in service, and changes nothing there.
func TestMdReplayRefusesANodeWithDnvObjects(t *testing.T) {
	for name, setup := range map[string]func(*replayStandIn){
		"dm device": func(s *replayStandIn) {
			s.dms = []string{"dnv-0000000000000001-0000000000000002-" +
				"d4-0000000000000003-0000000000000004"}
		},
		"md array":       func(s *replayStandIn) { s.dnvArray = true },
		"replay's array": func(s *replayStandIn) { s.leftover = true },
		"subsystem": func(s *replayStandIn) {
			s.subsystems = []string{common.NqnPrefix + ":x"}
		},
		"store file": func(s *replayStandIn) {
			s.storeFiles = []string{"cn-0000000000000001-0000000000000011"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newReplayStandIn()
			setup(s)
			rep := runReplay(t, s)
			if rep.Refused == "" || len(rep.Steps) != 0 {
				t.Fatalf("report = %+v, want a refusal and no step", rep)
			}
			for _, cmd := range []string{"mktemp", "losetup", "mdadm"} {
				if s.called(cmd) {
					t.Errorf("a refused replay ran %s: %v", cmd, s.calls)
				}
			}
		})
	}
	// Other names are no reason to refuse.
	s := newReplayStandIn()
	s.dms = []string{"vg0-root"}
	s.subsystems = []string{"nqn.2014-08.org.example:other"}
	if rep := runReplay(t, s); rep.Refused != "" {
		t.Errorf("refused a node with no dnv object: %s", rep.Refused)
	}
}

// The replay reads a leg's 1.2 superblock, the one dnv writes, never a
// host's newer superblock of another format at the leg's end: its examine
// names the format, after the mode words.
func TestMdReplayReadsThe12Format(t *testing.T) {
	s := newReplayStandIn()
	s.foreignTail = true
	rep := runReplay(t, s)
	if rep.Failed() {
		t.Fatalf("the replay failed over legs with a foreign tail: %+v", rep)
	}
	examined := 0
	for _, line := range s.calls {
		if !strings.HasPrefix(line, "mdadm ") ||
			!strings.Contains(line, "--examine") {
			continue
		}
		examined++
		if !strings.HasPrefix(line, "mdadm --examine --export "+
			"--metadata=1.2 /dev/loop") {
			t.Errorf("examine %q, want the 1.2 format after the mode words",
				line)
		}
	}
	if examined == 0 {
		t.Error("the replay ran no examine")
	}
	// mdadm 4.5 aborts or crashes when a format is named to its bitmap
	// and bad-block reads.
	for _, line := range s.calls {
		if (strings.Contains(line, "--examine-bitmap") ||
			strings.Contains(line, "--examine-badblocks")) &&
			(strings.Contains(line, "--metadata") ||
				strings.Contains(line, " -e ")) {
			t.Errorf("a bitmap or bad-block read names a format: %q", line)
		}
	}
}

// An examine named for 1.2 answers a leg with no superblock with exit
// status 0 and no output, so the replay judges it by its output: after the
// zero, an examine that still prints a superblock fails the step, while
// the agent's reader reads none.
func TestMdReplayHoldsTheZeroToMdadmsExamine(t *testing.T) {
	s := newReplayStandIn()
	s.zeroGhost = map[string]*replaySb{}
	rep := runReplay(t, s)
	last := lastStep(t, rep)
	if last.Name != "zero" || last.Ok ||
		!strings.Contains(last.Detail, "mdadm's examine still finds") {
		t.Errorf("last step = %+v, want the zero's step failed on mdadm's "+
			"examine", last)
	}
	assertTornDown(t, s, rep)
}

// A loop device losetup attached but did not print, as when a signal cut
// it short, is found by its file and detached.
func TestMdReplayDetachesALoopItWasNotTold(t *testing.T) {
	s := newReplayStandIn()
	s.lostAttach = true
	rep := runReplay(t, s)
	last := lastStep(t, rep)
	if last.Name != "build" || last.Ok {
		t.Errorf("last step = %+v, want the build failed", last)
	}
	assertTornDown(t, s, rep)
}

// A teardown that cannot remove what it made names it.
func TestMdReplayNamesWhatTheTeardownLeft(t *testing.T) {
	s := newReplayStandIn()
	s.runAlways = true
	s.failDetach = true
	rep := runReplay(t, s)
	if len(rep.Left) != 2 || !strings.Contains(rep.Left[0], "/dev/loop0") ||
		!strings.Contains(rep.Left[1], "/dev/loop1") {
		t.Errorf("left = %v, want both loop devices named", rep.Left)
	}
	if s.running != nil {
		t.Errorf("the teardown left the array running over %v", s.running)
	}
}

// The replay's array name has the form of dnv's array names, so the md
// assembly mask keeps udev off the replay's legs, with slice and group
// fields no agent array has.
func TestReplayArrayNameIsMasked(t *testing.T) {
	matched := false
	for _, alt := range strings.Split(common.CnMdArrayNameGlob, "|") {
		for _, v := range []string{replayArrayName, "any:" + replayArrayName} {
			if ok, _ := path.Match(alt, v); ok {
				matched = true
			}
		}
	}
	if !matched {
		t.Errorf("%q does not match %q", replayArrayName,
			common.CnMdArrayNameGlob)
	}
	nf := common.NewNameFmt("")
	lastMeta := nf.CnMdArrayName(0, common.MaxSliceCntPerSp-1,
		common.MaxGrpCntPerSlice-1, true)
	fields := replayArrayName[len(replayArrayName)-5:]
	if fields <= lastMeta[len(lastMeta)-5:] ||
		fields == nf.CnCloneMdArrayName(0)[len(lastMeta)-5:] {
		t.Errorf("%q's fields %q can be an agent array's", replayArrayName,
			fields)
	}
}
