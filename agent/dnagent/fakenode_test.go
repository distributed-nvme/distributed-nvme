package dnagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// fakeNode is an in-memory model of the slice of the OS the dn agent drives:
// the raw disk's [D13] metadata regions, device-mapper, the nvmet configfs
// tree, the nvme host and the local store. It backs common.FakeOsClient, records every call, and lets tests
// assert command sequences, probe-first idempotency and teardown order
// without root or real devices (dnagent.md §6).
type fakeNode struct {
	mu sync.Mutex

	calls []string

	// block devices
	devSize   map[string]uint64
	devNo     map[string]string
	nextMinor int

	// raw-device metadata regions ([D13]). A sparse overlay, never a flat
	// buffer: the data area alone is hundreds of MiB.
	blocks map[string][]fakeSegment

	// device-mapper
	dms map[string]*fakeDm
	// lsGhosts are names `dmsetup ls` reports that no longer exist. The
	// listing is an inherently stale snapshot — a converge can remove a
	// wrapper between the `ls` and the probe of that one name — and this is
	// how the suite reproduces that window deterministically.
	lsGhosts []string

	// configfs / directories
	dirs  map[string]bool
	files map[string]string
	links map[string]string

	// local store (WriteProto/ReadProto)
	protos map[string][]byte

	// nvme host connections, keyed by subsystem nqn
	conns map[string]*fakeConn
	// nextSubsys and nextCtrl are monotonic and never reused. Sizing the
	// index off len(conns) let a disconnect hand the next connect an index a
	// live subsystem was still using, so two subsystems collided on one
	// /sys/class/nvme-subsystem path and the sysfs walk saw one of them
	// twice.
	nextSubsys int
	nextCtrl   int

	// failBlockWrite fails every WriteBlock at this offset (0 disables it),
	// so a test can build the crash windows of the [D13] save protocol.
	failBlockWrite uint64
	failBlockSet   bool

	// dispatchStderr lets one command answer with the stderr the kernel
	// really prints — the EBUSY of a `dmsetup remove` on a device something
	// above it still maps.
	dispatchStderr string

	// failCmd fails the first matching command with the given stderr;
	// failCmdAlways fails every matching command, for the persistent
	// failures a single converge pass is supposed to survive. Both model
	// "the tool ran and answered no": exit code 1, a non-nil error, and no
	// dispatch, so the fake's state is left exactly as a refused ioctl
	// leaves the node.
	failCmd       map[string]string
	failCmdAlways map[string]string
	// killCmd / killCmdAlways model the OTHER half of agent.Reported: a
	// command that never answered — exit code -1 with a non-nil error, what
	// common.OsClient.RunCommand returns when the SH15 soft timeout killed
	// the child. These two DISPATCH first: the signal reaches the tool, but
	// the ioctl it had already issued completes in the kernel regardless, so
	// the node changed and the agent was told nothing.
	killCmd       map[string]bool
	killCmdAlways map[string]bool
	// killCmdNoEffect / killCmdNoEffectAlways are the same answer with the
	// opposite truth underneath: killed before the tool touched anything.
	//
	// Both halves exist because a sweep must be INDIFFERENT to which one
	// happened. It cannot tell them apart — that is the whole content of
	// "did not answer" — so the only correct behaviour is to re-enumerate
	// and act on what it then finds. A test that only ever kills one half
	// would pass against an agent that quietly assumed the other.
	killCmdNoEffect       map[string]bool
	killCmdNoEffectAlways map[string]bool
	// failRead / killRead are the ReadFile counterparts, matched on a
	// substring of the path. failRead* returns an error that is NOT
	// fs.ErrNotExist (an unreadable attribute), killRead* returns
	// context.DeadlineExceeded (a sysfs read the soft timeout cut off).
	// Neither may ever read as "absent": agent.readAttrStrict, which the
	// nvme host walk reads through, tests for fs.ErrNotExist and nothing
	// else.
	failRead       map[string]bool
	failReadAlways map[string]bool
	killRead       map[string]bool
	killReadAlways map[string]bool
	// gate blocks a command until the channel is closed (lock tests).
	gate map[string]chan struct{}
	// hardGate blocks a command until the channel is closed and — unlike gate
	// — ignores ctx cancellation, modelling the real child of
	// common/osclient.go: a cancelled RunCommand SIGTERMs the process and only
	// SIGKILLs it CmdHardTimeout−CmdSoftTimeout later, so `cmd.Run` can return
	// seconds after the cancel while the child still holds its fds open. That
	// gap is exactly what makes the §9.4 cancel-**and-wait** different from a
	// bare cancel, so a test that pins the wait cannot use the ctx-aware gate.
	hardGate map[string]chan struct{}
}

// fakeSegment is one recorded WriteBlock, in write order: later segments
// overlay earlier ones.
type fakeSegment struct {
	off  uint64
	data []byte
}

type fakeDm struct {
	table     string
	readOnly  bool
	suspended bool
	// dm-clone bookkeeping
	noHydration       bool
	noDiscardPassdown bool
	threshold         uint32
	batchSize         uint32
	// discards holds the metadata-only `blkdiscard --offset/--length` hints
	// (dm-clone hydration marking); zeroouts holds the §9.4
	// `blkdiscard --zeroout` side-provisioning writes. They are deliberately
	// separate — see cmdBlkdiscard.
	discards []string
	zeroouts []string
}

type fakeConn struct {
	device string
	state  string
	// ctrl and subsys are the sysfs names the connection materialises.
	// NvmeHost.ListSubsys reads /sys, not `nvme list-subsys -o json` (which
	// on real nvme-cli lists no namespaces at all), so the fake has to build
	// the same tree a real connect does. ctrl names the FIRST controller
	// still attached to the subsystem.
	ctrl   string
	subsys string
	// nqn keys this connection in f.conns; idx is the monotonic subsystem
	// index its sysfs names are built from.
	nqn string
	idx int
	// ctrls is every controller attached to this subsystem. A subsystem can
	// hold more than one — the two sides of a migrating leg share one NQN
	// ([D1]) — which is exactly why `nvme disconnect --device` exists and
	// why it has to be modelled separately from `--nqn`.
	ctrls []*fakeCtrl
}

// fakeCtrl is one controller (one path) of a subsystem.
type fakeCtrl struct {
	name    string // "nvme3"
	trAddr  string
	trSvcId string
	trType  string
	state   string
	pathDev string // "nvme0c3n1", the hidden path device carrying ana_state
	// hostNqn is the --hostnqn the connect was made with. The kernel
	// publishes it per controller, and it is the only field that says WHICH
	// agent on a shared kernel opened the connection — a MigrSrcNqn names
	// the source dn, not the connecting one.
	hostNqn string
}

func newFakeNode() *fakeNode {
	return &fakeNode{
		devSize:       make(map[string]uint64),
		devNo:         make(map[string]string),
		nextMinor:     1,
		blocks:        make(map[string][]fakeSegment),
		dms:           make(map[string]*fakeDm),
		dirs:          map[string]bool{common.DefaultLocalStorPrefix: true},
		files:         make(map[string]string),
		links:         make(map[string]string),
		protos:        make(map[string][]byte),
		conns:         make(map[string]*fakeConn),
		failCmd:       make(map[string]string),
		failCmdAlways: make(map[string]string),

		killCmd:               make(map[string]bool),
		killCmdAlways:         make(map[string]bool),
		killCmdNoEffect:       make(map[string]bool),
		killCmdNoEffectAlways: make(map[string]bool),
		failRead:              make(map[string]bool),
		failReadAlways:        make(map[string]bool),
		killRead:              make(map[string]bool),
		killReadAlways:        make(map[string]bool),

		gate:     make(map[string]chan struct{}),
		hardGate: make(map[string]chan struct{}),
	}
}

// blockCmd parks every command whose recorded line contains key until
// releaseCmd, ignoring ctx cancellation (see hardGate).
func (f *fakeNode) blockCmd(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.hardGate[key]; ok {
		return
	}
	f.hardGate[key] = make(chan struct{})
}

// releaseCmd lets a blockCmd'd command finish. It is idempotent, so a test may
// both release it inline and register a cleanup that releases it again — which
// it must, or the server's WaitBackground would never return.
func (f *fakeNode) releaseCmd(key string) {
	f.mu.Lock()
	ch, ok := f.hardGate[key]
	delete(f.hardGate, key)
	f.mu.Unlock()
	if ok {
		close(ch)
	}
}

func (f *fakeNode) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn:      f.runCommand,
		ReadFileFn:        f.readFile,
		WriteFileFn:       f.writeFile,
		WriteFileDirectFn: f.writeFileDirect,
		ReadBlockFn:       f.readBlock,
		WriteBlockFn:      f.writeBlock,
		ReadProtoFn:       f.readProto,
		WriteProtoFn:      f.writeProto,
	}
}

// ---------------------------------------------------------------------------
// Call recording
// ---------------------------------------------------------------------------

func (f *fakeNode) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// Calls returns every recorded call, in order.
func (f *fakeNode) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeNode) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// readOnlyPrefixes are the probes; everything else changes the system.
var readOnlyPrefixes = []string{
	"cmd ls ", "cmd lsblk ",
	"cmd dmsetup info", "cmd dmsetup table", "cmd dmsetup status",
	// `dmsetup ls` is the root of the sweep's enumeration and changes
	// nothing; without it here every SH16 no-mutation assertion would fail
	// the moment a converge started sweeping.
	"cmd dmsetup ls",
	"cmd nvme list-subsys", "read ", "readproto ", "readblock ",
}

// Mutations returns only the calls that change the system.
func (f *fakeNode) Mutations() []string {
	var out []string
	for _, call := range f.Calls() {
		mutating := true
		for _, prefix := range readOnlyPrefixes {
			if strings.HasPrefix(call, prefix) {
				mutating = false
				break
			}
		}
		if mutating {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeNode) callsMatching(substr string) []string {
	var out []string
	for _, call := range f.Calls() {
		if strings.Contains(call, substr) {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeNode) hasCall(substr string) bool {
	return len(f.callsMatching(substr)) > 0
}

// indexOfCall returns the position of the first call containing substr, or -1.
func (f *fakeNode) indexOfCall(substr string) int {
	return f.indexOfCallFrom(substr, 0)
}

// indexOfCallFrom is indexOfCall starting at from — what makes assertOrder a
// subsequence match, so a sequence may name the same call shape twice (two
// volume-table slot writes around a blkdiscard, say).
func (f *fakeNode) indexOfCallFrom(substr string, from int) int {
	calls := f.Calls()
	for i := from; i < len(calls); i++ {
		if strings.Contains(calls[i], substr) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// OsClient surface
// ---------------------------------------------------------------------------

func (f *fakeNode) readFile(ctx context.Context, path string) (string, error) {
	if err := f.ctxErr(ctx); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("read %s", path)
	if err := f.readHookErr(path); err != nil {
		return "", err
	}
	data, ok := f.files[path]
	if !ok {
		// The absent-file error MUST wrap fs.ErrNotExist. The production
		// LimitedOsClient returns os.ReadFile's *fs.PathError, and
		// agent.readAttrStrict — which the nvme host walk reads through —
		// tells "absent" from "did not answer" by
		// errors.Is(err, fs.ErrNotExist) and by nothing else. A bare
		// fmt.Errorf here would turn every missing attribute into a stalled
		// read and every enumerator into an error.
		return "", fmt.Errorf("no such file: %s: %w", path, fs.ErrNotExist)
	}
	return data, nil
}

// readHookErr applies the failRead/killRead hooks: the ReadFile half of the
// same "answered no" / "did not answer" split the command hooks model. Both
// errors are deliberately NOT fs.ErrNotExist — an unreadable attribute and a
// timed-out one are the two ways a read can fail without the file being
// absent, and neither may make a sweep believe an object is gone.
//
// Deleting the matched key inside the range is the one-shot form (deleting
// the current key during a range is defined behaviour in Go).
func (f *fakeNode) readHookErr(path string) error {
	for key := range f.failRead {
		if strings.Contains(path, key) {
			delete(f.failRead, key)
			return errors.New("input/output error")
		}
	}
	for key := range f.failReadAlways {
		if strings.Contains(path, key) {
			return errors.New("input/output error")
		}
	}
	for key := range f.killRead {
		if strings.Contains(path, key) {
			delete(f.killRead, key)
			return context.DeadlineExceeded
		}
	}
	for key := range f.killReadAlways {
		if strings.Contains(path, key) {
			return context.DeadlineExceeded
		}
	}
	return nil
}

func (f *fakeNode) writeFile(
	ctx context.Context, path string, data string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("write %s=%s", path, data)
	f.files[path] = data
	return nil
}

func (f *fakeNode) writeFileDirect(
	ctx context.Context, path string, data string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writedirect %s=%s", path, data)
	if !f.dirs[parentDir(path)] {
		return fmt.Errorf("no such directory: %s", parentDir(path))
	}
	f.files[path] = configfsNormalize(path, data)
	return nil
}

// configfsNormalize models how the nvmet kernel module stores an attribute
// rather than echoing back the bytes written. device_uuid and device_nguid
// both accept a bare 32-hex-digit string and both always read back
// dash-separated, which is what makes a byte-wise idempotency check on the
// nguid rewrite it forever (agent.sameNsId).
func configfsNormalize(path, data string) string {
	base := path[strings.LastIndex(path, "/")+1:]
	if base != "device_uuid" && base != "device_nguid" {
		return data
	}
	hexed := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(data), "-", ""))
	if len(hexed) != 32 {
		return data
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s\n",
		hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32])
}

func (f *fakeNode) readProto(
	ctx context.Context, path string, target proto.Message,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("readproto %s", path)
	raw, ok := f.protos[path]
	if !ok {
		return fmt.Errorf("no such file: %s", path)
	}
	return proto.Unmarshal(raw, target)
}

func (f *fakeNode) writeProto(
	ctx context.Context, path string, msg proto.Message,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writeproto %s", path)
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	f.protos[path] = raw
	return nil
}

// readBlock / writeBlock model the raw --disk device the [D13] metadata lives
// on: a device always exists at its full size, an unwritten region reads as
// zeros, and a read that runs past the end is a short read (an error).
func (f *fakeNode) readBlock(
	ctx context.Context, path string, offset uint64, length uint64,
) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("readblock %s off=%d len=%d", path, offset, length)
	size, ok := f.devSize[path]
	if !ok {
		return nil, fmt.Errorf("no such device: %s", path)
	}
	if offset+length > size {
		return nil, fmt.Errorf("short read: %s off=%d len=%d size=%d",
			path, offset, length, size)
	}
	out := make([]byte, length)
	for _, seg := range f.blocks[path] {
		lo := max64(seg.off, offset)
		hi := min64(seg.off+uint64(len(seg.data)), offset+length)
		if lo >= hi {
			continue
		}
		copy(out[lo-offset:hi-offset], seg.data[lo-seg.off:hi-seg.off])
	}
	return out, nil
}

func (f *fakeNode) writeBlock(
	ctx context.Context, path string, offset uint64, data []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writeblock %s off=%d len=%d", path, offset, len(data))
	if f.failBlockSet && offset == f.failBlockWrite {
		return fmt.Errorf("fake: write block at %d failed", offset)
	}
	size, ok := f.devSize[path]
	if !ok {
		return fmt.Errorf("no such device: %s", path)
	}
	if offset+uint64(len(data)) > size {
		return fmt.Errorf("short write: %s off=%d len=%d size=%d",
			path, offset, len(data), size)
	}
	f.blocks[path] = append(f.blocks[path], fakeSegment{
		off: offset, data: append([]byte(nil), data...),
	})
	return nil
}

// corruptBlock flips bytes behind the agent's back, so tests can build a torn
// slot or a damaged header.
func (f *fakeNode) corruptBlock(path string, offset uint64, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks[path] = append(f.blocks[path], fakeSegment{
		off: offset, data: append([]byte(nil), data...),
	})
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func parentDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "/"
	}
	return path[:idx]
}

// ---------------------------------------------------------------------------
// Command dispatch
// ---------------------------------------------------------------------------

// A CANCELLED CONTEXT FAILS, the way the production client does.
// common/osclient.go runs every command through exec.CommandContext and tests
// ctx.Err() at the head of each file operation, so a converge whose context
// dies part-way stops doing work at that point. The fake ignored the context
// entirely, which made it blind to a whole class of bug by construction: a
// DN8 retry converge that cancelled its OWN context in stopMigrRetry ran to
// completion here and stalled for ever on the lab, and the unit test written
// for it passed against the broken code until this check existed.
func (f *fakeNode) ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (f *fakeNode) runCommand(
	ctx context.Context,
	name string,
	args []string,
	stdin string,
) (string, string, int, error) {
	if err := f.ctxErr(ctx); err != nil {
		return "", err.Error(), -1, err
	}
	line := "cmd " + name + " " + strings.Join(args, " ")
	if stdin != "" {
		// A multi-line table travels through stdin (dmsetup's --table is
		// single-line only); fold it onto the call string so assertOrder can
		// match on the table the agent actually sent.
		line += " stdin=" + strings.Join(dmTargets(stdin), " | ")
	}

	f.mu.Lock()
	f.record("%s", line)
	var gate chan struct{}
	for key, ch := range f.gate {
		if strings.Contains(line, key) {
			gate = ch
			break
		}
	}
	var hard chan struct{}
	for key, ch := range f.hardGate {
		if strings.Contains(line, key) {
			hard = ch
			break
		}
	}
	for key, stderr := range f.failCmd {
		if strings.Contains(line, key) {
			delete(f.failCmd, key)
			f.mu.Unlock()
			return "", stderr, 1, fmt.Errorf("exit status 1")
		}
	}
	for key, stderr := range f.failCmdAlways {
		if strings.Contains(line, key) {
			f.mu.Unlock()
			return "", stderr, 1, fmt.Errorf("exit status 1")
		}
	}
	// The kill hooks are checked here, in the same place as the fail hooks,
	// and after them: a key registered in both fails rather than being
	// killed. "No effect" returns before the dispatch; "killed" runs the
	// dispatch and throws the answer away.
	killedNoEffect := takeKill(f.killCmdNoEffect, f.killCmdNoEffectAlways, line)
	var killed bool
	if !killedNoEffect {
		killed = takeKill(f.killCmd, f.killCmdAlways, line)
	}
	f.mu.Unlock()
	if killedNoEffect {
		return killedCmdResult()
	}

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", "", -1, ctx.Err()
		}
	}
	// No ctx arm: a hard gate is a child that outlives the cancel.
	if hard != nil {
		<-hard
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatchStderr = ""
	stdout, code := f.dispatch(name, args, stdin)
	if killed {
		// The tool was killed, but the ioctl it had already issued ran to
		// completion in the kernel: the node changed and the agent was told
		// nothing. Whatever the dispatch returned is discarded.
		return killedCmdResult()
	}
	if code != 0 {
		stderr := f.dispatchStderr
		if stderr == "" {
			stderr = "fake: " + name + " failed"
		}
		return stdout, stderr, code, fmt.Errorf("exit status %d", code)
	}
	return stdout, "", 0, nil
}

// takeKill reports whether line matches a one-shot or an always kill hook,
// consuming the one-shot key so it fires exactly once.
func takeKill(oneShot, always map[string]bool, line string) bool {
	for key := range oneShot {
		if strings.Contains(line, key) {
			delete(oneShot, key)
			return true
		}
	}
	for key := range always {
		if strings.Contains(line, key) {
			return true
		}
	}
	return false
}

// killedCmdResult is what common.OsClient.RunCommand returns for a child the
// SH15 soft timeout killed: no output, exit code -1 and a non-nil error —
// exactly the (exitCode, err) pair agent.Reported calls "did not answer",
// and the one thing a `dmsetup info` must never be allowed to read as "the
// device is not there".
func killedCmdResult() (string, string, int, error) {
	return "", "signal: killed", -1, errors.New("signal: killed")
}

func (f *fakeNode) dispatch(
	name string, args []string, stdin string,
) (string, int) {
	switch name {
	case "ls":
		return f.cmdLs(args)
	case "rm":
		return f.cmdRm(args)
	case "mkdir":
		return f.cmdMkdir(args)
	case "rmdir":
		return f.cmdRmdir(args)
	case "ln":
		return f.cmdLn(args)
	case "lsblk":
		return f.cmdLsblk(args)
	case "blkdiscard":
		return f.cmdBlkdiscard(args)
	case "dmsetup":
		return f.cmdDmsetup(args, stdin)
	case "nvme":
		return f.cmdNvme(args)
	}
	return "", 127
}

func (f *fakeNode) children(path string) []string {
	seen := make(map[string]struct{})
	for _, set := range []map[string]bool{f.dirs} {
		for entry := range set {
			if child, ok := childOf(path, entry); ok {
				seen[child] = struct{}{}
			}
		}
	}
	for entry := range f.files {
		if child, ok := childOf(path, entry); ok {
			seen[child] = struct{}{}
		}
	}
	for entry := range f.links {
		if child, ok := childOf(path, entry); ok {
			seen[child] = struct{}{}
		}
	}
	if path == common.DefaultLocalStorPrefix {
		for entry := range f.protos {
			if child, ok := childOf(path, entry); ok {
				seen[child] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for child := range seen {
		out = append(out, child)
	}
	sort.Strings(out)
	return out
}

func childOf(dir, entry string) (string, bool) {
	prefix := dir + "/"
	if !strings.HasPrefix(entry, prefix) {
		return "", false
	}
	rest := entry[len(prefix):]
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func (f *fakeNode) cmdLs(args []string) (string, int) {
	path := args[len(args)-1]
	if !f.dirs[path] {
		return "", 2
	}
	return strings.Join(f.children(path), "\n") + "\n", 0
}

func (f *fakeNode) cmdRm(args []string) (string, int) {
	for _, path := range args {
		if strings.HasPrefix(path, "-") {
			continue
		}
		delete(f.files, path)
		delete(f.links, path)
		delete(f.protos, path)
	}
	return "", 0
}

func (f *fakeNode) cmdMkdir(args []string) (string, int) {
	path := args[len(args)-1]
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	cur := ""
	for _, part := range parts {
		cur += "/" + part
		if !f.dirs[cur] {
			f.dirs[cur] = true
			f.autoCreate(cur)
		}
	}
	return "", 0
}

// autoCreate mirrors configfs: creating an nvmet port or subsystem directory
// makes the kernel populate its standard subdirectories (a port's
// subsystems/ and ana_groups/1, a subsystem's namespaces/ and
// allowed_hosts/). Attribute files appear only once written, which is what
// makes a fresh node's probes read empty.
func (f *fakeNode) autoCreate(path string) {
	rest, ok := strings.CutPrefix(path, agent.NvmetRoot+"/")
	if !ok {
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 2 && parts[0] == "ports":
		for _, child := range []string{
			"subsystems", "referrals", "ana_groups", "ana_groups/1",
		} {
			f.dirs[path+"/"+child] = true
		}
	case len(parts) == 2 && parts[0] == "subsystems":
		for _, child := range []string{"namespaces", "allowed_hosts"} {
			f.dirs[path+"/"+child] = true
		}
	}
}

// cmdRmdir models configfs: removing a directory takes its whole subtree
// (attributes and auto-created children) with it.
func (f *fakeNode) cmdRmdir(args []string) (string, int) {
	path := args[len(args)-1]
	if !f.dirs[path] {
		return "", 1
	}
	prefix := path + "/"
	for _, set := range []map[string]bool{f.dirs} {
		for entry := range set {
			if strings.HasPrefix(entry, prefix) {
				delete(set, entry)
			}
		}
	}
	for entry := range f.files {
		if strings.HasPrefix(entry, prefix) {
			delete(f.files, entry)
		}
	}
	for entry := range f.links {
		if strings.HasPrefix(entry, prefix) {
			delete(f.links, entry)
		}
	}
	delete(f.dirs, path)
	return "", 0
}

func (f *fakeNode) cmdLn(args []string) (string, int) {
	target, link := args[len(args)-2], args[len(args)-1]
	if _, ok := f.links[link]; ok {
		return "", 1
	}
	f.links[link] = target
	return "", 0
}

func (f *fakeNode) cmdLsblk(args []string) (string, int) {
	path := args[len(args)-1]
	if contains(args, "SIZE") {
		size, ok := f.devSize[path]
		if !ok {
			return "", 32
		}
		return fmt.Sprintf("%d\n", size), 0
	}
	if contains(args, "KNAME") {
		// The kernel name Dm.WriteZeroesMaxBytes turns into a
		// /sys/class/block entry (§9.4's DN5 check). The fake's devices are
		// already plain /dev paths, so the basename is the kernel name.
		if _, ok := f.devNo[path]; !ok {
			return "", 32
		}
		return path[strings.LastIndexByte(path, '/')+1:] + "\n", 0
	}
	devNo, ok := f.devNo[path]
	if !ok {
		return "", 32
	}
	return devNo + "  \n", 0
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func (f *fakeNode) newDevNo() string {
	f.nextMinor++
	return fmt.Sprintf("253:%d", f.nextMinor)
}

func (f *fakeNode) cmdBlkdiscard(args []string) (string, int) {
	dev := args[len(args)-1]
	name := strings.TrimPrefix(dev, "/dev/mapper/")
	dm, ok := f.dms[name]
	if !ok {
		return "", 0
	}
	// The §9.4 side-provisioning write and the metadata-only "mark this
	// region hydrated" discard are recorded in separate lists: a
	// migration-dst side zeroes its own dm-linear while the dm-clone above it
	// takes hydration discards, and one shared list would make either
	// assertion meaningless.
	if contains(args, "--zeroout") {
		dm.zeroouts = append(dm.zeroouts,
			strings.Join(args[:len(args)-1], " "))
		return "", 0
	}
	dm.discards = append(dm.discards, strings.Join(args[:len(args)-1], " "))
	return "", 0
}

// heldBy reports the dm device whose live table still maps path, i.e.
// whoever holds it open — a migration destination's dm-clone over its side's
// dm-linear, say. dm refuses to release a device something above it still
// references.
func (f *fakeNode) heldBy(path string) string {
	devNo := f.devNo[path]
	if devNo == "" {
		return ""
	}
	for name, dm := range f.dms {
		if f.devNo["/dev/mapper/"+name] == devNo {
			continue
		}
		for _, line := range dmTargets(dm.table) {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			for _, field := range fields[3:] {
				if field == devNo {
					return name
				}
			}
		}
	}
	return ""
}

// flagValue returns the value that follows --flag in an argument list.
func flagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeNode) cmdDmsetup(args []string, stdin string) (string, int) {
	if len(args) == 0 {
		return "", 3
	}
	switch args[0] {
	case "ls":
		// The real tool prints "{name}\t({major}:{minor})" per device and the
		// literal "No devices found" — still exit 0 — on an empty node. It is
		// the root of the sweep's enumeration: what to remove is what exists
		// minus what is wanted, and nothing else on the node can name a
		// device the agent has no plan for.
		names := make([]string, 0, len(f.dms)+len(f.lsGhosts))
		for name := range f.dms {
			names = append(names, name)
		}
		// A listing may name a device that is already gone (lsGhosts).
		names = append(names, f.lsGhosts...)
		if len(names) == 0 {
			return "No devices found\n", 0
		}
		sort.Strings(names)
		var sb strings.Builder
		for _, name := range names {
			fmt.Fprintf(&sb, "%s\t(%s)\n",
				name, f.devNo["/dev/mapper/"+name])
		}
		return sb.String(), 0
	case "info":
		name := args[len(args)-1]
		dm, ok := f.dms[name]
		if !ok {
			return "", 1
		}
		// The real four-position attr column: live, inactive-table,
		// suspended, read-only/read-write — e.g. "L--w", "L-sw", "L--r".
		attr := []byte("L---")
		if dm.suspended {
			attr[2] = 's'
		}
		if dm.readOnly {
			attr[3] = 'r'
		} else {
			attr[3] = 'w'
		}
		return string(attr) + "\n", 0
	case "table":
		dm, ok := f.dms[args[1]]
		if !ok {
			return "", 1
		}
		return strings.Join(dmTargets(dm.table), "\n") + "\n", 0
	case "status":
		return f.dmStatus(args[1])
	case "create":
		return f.dmCreate(args, stdin)
	case "reload":
		return f.dmReload(args, stdin)
	case "suspend", "resume":
		dm, ok := f.dms[args[1]]
		if !ok {
			return "", 1
		}
		dm.suspended = args[0] == "suspend"
		return "", 0
	case "remove":
		name := args[1]
		dm, ok := f.dms[name]
		if !ok {
			return "", 1
		}
		if dm.suspended {
			// `dmsetup remove` does not succeed on a suspended device.
			return "", 1
		}
		if holder := f.heldBy("/dev/mapper/" + name); holder != "" {
			// A device another live table still maps is open, and the
			// kernel refuses to remove it (-EBUSY). Modelling this is what
			// makes a teardown that runs out of order fail a test rather
			// than only a real node — and what lets a sweep test express
			// "this device is pinned" at all.
			f.dispatchStderr = "device-mapper: remove ioctl on " + name +
				" failed: Device or resource busy (held by " + holder + ")"
			return "", 1
		}
		delete(f.dms, name)
		path := "/dev/mapper/" + name
		delete(f.devNo, path)
		delete(f.devSize, path)
		return "", 0
	case "message":
		return f.dmMessage(args)
	}
	return "", 3
}

// parseDmCreateArgs accepts both table forms: the single-line --table option
// and the multi-line stdin form dmsetup reads when --table is absent
// (architecture.md Appendix A). The stdin table is normalized to one space-
// joined line per target, keeping the parsing below single-line.
func parseDmCreateArgs(args []string, stdin string) (string, string, bool) {
	name := args[1]
	table := ""
	readOnly := false
	hasTableFlag := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--table":
			i++
			table = args[i]
			hasTableFlag = true
		case "--readonly":
			readOnly = true
		}
	}
	if !hasTableFlag {
		table = strings.TrimRight(stdin, "\n")
	}
	return name, table, readOnly
}

// dmTargets splits a table into its target lines.
func dmTargets(table string) []string {
	var out []string
	for _, line := range strings.Split(table, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func (f *fakeNode) dmCreate(args []string, stdin string) (string, int) {
	name, table, readOnly := parseDmCreateArgs(args, stdin)
	if table == "" {
		return "", 3
	}
	if _, exists := f.dms[name]; exists {
		return "", 1
	}
	dm := &fakeDm{table: table, readOnly: readOnly}
	applyCloneTable(dm, table)
	f.dms[name] = dm
	path := "/dev/mapper/" + name
	f.devNo[path] = f.newDevNo()
	f.devSize[path] = tableSectors(table) * 512
	return "", 0
}

func (f *fakeNode) dmReload(args []string, stdin string) (string, int) {
	name, table, readOnly := parseDmCreateArgs(args, stdin)
	if table == "" {
		return "", 3
	}
	dm, ok := f.dms[name]
	if !ok {
		return "", 1
	}
	dm.table = table
	dm.readOnly = readOnly
	applyCloneTable(dm, table)
	f.devSize["/dev/mapper/"+name] = tableSectors(table) * 512
	return "", 0
}

// tableSectors is the device's size: the sum of its targets' lengths.
func tableSectors(table string) uint64 {
	var total uint64
	for _, line := range dmTargets(table) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sectors, _ := strconv.ParseUint(fields[1], 10, 64)
		total += sectors
	}
	return total
}

func applyCloneTable(dm *fakeDm, table string) {
	lines := dmTargets(table)
	if len(lines) != 1 {
		return
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 3 || fields[2] != "clone" {
		return
	}
	dm.noHydration = contains(fields, "no_hydration")
	dm.noDiscardPassdown = contains(fields, "no_discard_passdown")
	for i := 0; i+1 < len(fields); i++ {
		value, err := strconv.ParseUint(fields[i+1], 10, 32)
		if err != nil {
			continue
		}
		switch fields[i] {
		case "hydration_threshold":
			dm.threshold = uint32(value)
		case "hydration_batch_size":
			dm.batchSize = uint32(value)
		}
	}
}

func (f *fakeNode) dmMessage(args []string) (string, int) {
	dm, ok := f.dms[args[1]]
	if !ok {
		return "", 1
	}
	message := strings.Join(args[3:], " ")
	switch {
	case message == "enable_hydration":
		dm.noHydration = false
	case message == "disable_hydration":
		dm.noHydration = true
	case strings.HasPrefix(message, "hydration_threshold "):
		value, _ := strconv.ParseUint(strings.Fields(message)[1], 10, 32)
		dm.threshold = uint32(value)
	case strings.HasPrefix(message, "hydration_batch_size "):
		value, _ := strconv.ParseUint(strings.Fields(message)[1], 10, 32)
		dm.batchSize = uint32(value)
	}
	return "", 0
}

func (f *fakeNode) dmStatus(name string) (string, int) {
	dm, ok := f.dms[name]
	if !ok {
		return "", 1
	}
	lines := dmTargets(dm.table)
	if len(lines) == 0 {
		return "", 1
	}
	if len(lines) > 1 {
		// A multi-segment side device: report each line back. dmsetup
		// status and table agree for a linear target.
		var sb strings.Builder
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 5 || fields[2] != "linear" {
				return "", 1
			}
			fmt.Fprintf(&sb, "%s %s linear %s %s\n",
				fields[0], fields[1], fields[3], fields[4])
		}
		return sb.String(), 0
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 3 {
		return "", 1
	}
	sectors := fields[1]
	switch fields[2] {
	case "error":
		return fmt.Sprintf("0 %s error\n", sectors), 0
	case "linear":
		return fmt.Sprintf("0 %s linear %s %s\n",
			sectors, fields[3], fields[4]), 0
	case "clone":
		regionSectors := fields[6]
		regions := tableSectors(dm.table)
		region, _ := strconv.ParseUint(regionSectors, 10, 64)
		if region > 0 {
			regions /= region
		}
		// The kernel prints back the features that are still in force, in
		// table order, with a derived count: a dnv dm-clone starts at
		// `2 no_hydration no_discard_passdown` and drops to
		// `1 no_discard_passdown` once hydration is enabled.
		var names []string
		if dm.noHydration {
			names = append(names, "no_hydration")
		}
		if dm.noDiscardPassdown {
			names = append(names, "no_discard_passdown")
		}
		features := strconv.Itoa(len(names))
		if len(names) > 0 {
			features += " " + strings.Join(names, " ")
		}
		return fmt.Sprintf(
			"0 %s clone 8 1/1024 %s 0/%d 0 %s 4 hydration_threshold %d "+
				"hydration_batch_size %d\n",
			sectors, regionSectors, regions, features,
			dm.threshold, dm.batchSize), 0
	}
	return "", 1
}

func (f *fakeNode) cmdNvme(args []string) (string, int) {
	switch args[0] {
	case "connect":
		return f.nvmeConnect(args)
	case "disconnect":
		return f.nvmeDisconnect(args)
	}
	return "", 3
}

// nvmeConnect attaches one controller. A second connect to an NQN the host
// already holds adds a PATH to the same subsystem rather than a second
// subsystem — the two sides of a migrating leg share one NQN ([D1]) — and
// every index is drawn from a monotonic counter, so a disconnected
// subsystem's /sys/class/nvme-subsystem path is never handed to a later one.
func (f *fakeNode) nvmeConnect(args []string) (string, int) {
	nqn := flagValue(args, "--nqn")
	if nqn == "" {
		return "", 3
	}
	conn, ok := f.conns[nqn]
	if !ok {
		idx := f.nextSubsys
		f.nextSubsys++
		conn = &fakeConn{
			device: fmt.Sprintf("nvme%dn1", idx),
			state:  "live",
			subsys: fmt.Sprintf("nvme-subsys%d", idx),
			nqn:    nqn,
			idx:    idx,
		}
		f.conns[nqn] = conn
		f.devNo["/dev/"+conn.device] = f.newDevNo()
		f.addSubsysSysfs(conn)
	}
	ctrlName := fmt.Sprintf("nvme%d", f.nextCtrl)
	f.nextCtrl++
	ctrl := &fakeCtrl{
		name:    ctrlName,
		trAddr:  flagValue(args, "--traddr"),
		trSvcId: flagValue(args, "--trsvcid"),
		trType:  flagValue(args, "--transport"),
		state:   conn.state,
		pathDev: fmt.Sprintf("nvme%dc%sn1", conn.idx,
			strings.TrimPrefix(ctrlName, "nvme")),
		hostNqn: flagValue(args, "--hostnqn"),
	}
	conn.ctrls = append(conn.ctrls, ctrl)
	f.addCtrlSysfs(conn, ctrl)
	conn.refresh()
	return "", 0
}

// nvmeDisconnect handles both forms. `--nqn` drops every controller of the
// subsystem; `--device` drops exactly one, which is the only way to retire
// the dead side of a migrating leg without killing the live one (SH20,
// cnagent.md §2.3). Neither is idempotent: nvme-cli exits non-zero when it
// finds nothing to disconnect.
func (f *fakeNode) nvmeDisconnect(args []string) (string, int) {
	if nqn := flagValue(args, "--nqn"); nqn != "" {
		conn, ok := f.conns[nqn]
		if !ok {
			return "", 1
		}
		for _, ctrl := range append([]*fakeCtrl(nil), conn.ctrls...) {
			f.dropCtrl(conn, ctrl)
		}
		return "", 0
	}
	dev := flagValue(args, "--device")
	if dev == "" {
		return "", 3
	}
	for _, conn := range f.conns {
		for _, ctrl := range conn.ctrls {
			if ctrl.name == dev {
				f.dropCtrl(conn, ctrl)
				return "", 0
			}
		}
	}
	return "", 1
}

// refresh keeps the legacy single-controller view pointing at the first
// controller the subsystem still holds.
func (c *fakeConn) refresh() {
	c.ctrl = ""
	if len(c.ctrls) > 0 {
		c.ctrl = c.ctrls[0].name
	}
}

// addSubsysSysfs materialises the subsystem half of the /sys tree a real
// `nvme connect` creates: the directory keyed by subsysnqn, holding the
// multipath namespace node.
func (f *fakeNode) addSubsysSysfs(conn *fakeConn) {
	subsysDir := "/sys/class/nvme-subsystem/" + conn.subsys
	f.dirs["/sys/class/nvme-subsystem"] = true
	f.dirs["/sys/class/nvme"] = true
	f.dirs[subsysDir] = true
	f.dirs[subsysDir+"/"+conn.device] = true
	f.files[subsysDir+"/subsysnqn"] = conn.nqn + "\n"
}

// addCtrlSysfs materialises one controller: its link under the subsystem, its
// own directory with transport, address and state, and the hidden path device
// that is the only place ana_state lives.
func (f *fakeNode) addCtrlSysfs(conn *fakeConn, ctrl *fakeCtrl) {
	subsysDir := "/sys/class/nvme-subsystem/" + conn.subsys
	ctrlDir := "/sys/class/nvme/" + ctrl.name
	f.dirs[subsysDir+"/"+ctrl.name] = true
	f.dirs[ctrlDir] = true
	f.dirs[ctrlDir+"/"+ctrl.pathDev] = true
	f.files[ctrlDir+"/transport"] = ctrl.trType + "\n"
	f.files[ctrlDir+"/address"] = fmt.Sprintf(
		"traddr=%s,trsvcid=%s\n", ctrl.trAddr, ctrl.trSvcId)
	f.files[ctrlDir+"/state"] = ctrl.state + "\n"
	f.files[ctrlDir+"/hostnqn"] = ctrl.hostNqn + "\n"
	f.files[ctrlDir+"/"+ctrl.pathDev+"/ana_state"] = "optimized\n"
}

// dropCtrl removes one controller; the subsystem itself goes only with its
// last path, which is what the kernel does.
func (f *fakeNode) dropCtrl(conn *fakeConn, ctrl *fakeCtrl) {
	subsysDir := "/sys/class/nvme-subsystem/" + conn.subsys
	ctrlDir := "/sys/class/nvme/" + ctrl.name
	delete(f.dirs, subsysDir+"/"+ctrl.name)
	deleteTree(f.dirs, ctrlDir)
	deleteTree(f.files, ctrlDir)
	var kept []*fakeCtrl
	for _, held := range conn.ctrls {
		if held != ctrl {
			kept = append(kept, held)
		}
	}
	conn.ctrls = kept
	conn.refresh()
	if len(kept) > 0 {
		return
	}
	f.dropSubsysSysfs(conn)
	delete(f.devNo, "/dev/"+conn.device)
	delete(f.conns, conn.nqn)
}

func (f *fakeNode) dropSubsysSysfs(conn *fakeConn) {
	subsysDir := "/sys/class/nvme-subsystem/" + conn.subsys
	deleteTree(f.dirs, subsysDir)
	deleteTree(f.files, subsysDir)
}

// deleteTree drops root and everything under it from a path-keyed map.
func deleteTree[V any](set map[string]V, root string) {
	for path := range set {
		if path == root || strings.HasPrefix(path, root+"/") {
			delete(set, path)
		}
	}
}

// setConnState re-stamps a live connection's controller state, so a test can
// make a path dead without disconnecting it. Every path of the subsystem
// moves together; a single path is aged by dropping its controller.
func (f *fakeNode) setConnState(nqn, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conn, ok := f.conns[nqn]
	if !ok {
		return
	}
	conn.state = state
	for _, ctrl := range conn.ctrls {
		ctrl.state = state
		f.files["/sys/class/nvme/"+ctrl.name+"/state"] = state + "\n"
	}
}
