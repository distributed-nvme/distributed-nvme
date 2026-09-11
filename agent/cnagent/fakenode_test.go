package cnagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// fakeNode is an in-memory model of the slice of the OS the cn agent drives:
// device-mapper, md, the tmpfs/loop base state, the nvmet configfs tree,
// the sysfs nvme-subsystem tree the leg probe walks, thin-provisioning-tools
// and the local store. It backs common.FakeOsClient, records every call, and
// lets tests assert command sequences, probe-first idempotency and teardown
// order without root or real devices (cnagent.md §6).
type fakeNode struct {
	mu sync.Mutex

	calls []string
	// sysfsNoDeadline records every /sys read that arrived on a ctx carrying
	// no deadline. The leg walk's sysfs reads are
	// SH15-bounded like every other OS touch, so this must stay empty.
	sysfsNoDeadline []string

	// block devices
	devSize   map[string]uint64
	devNo     map[string]string
	nextMinor int

	// device-mapper
	dms map[string]*fakeDm
	// thinPools is the dm-thin metadata of every pool this node has ever
	// activated, keyed by the pool's dm name and outliving the dm device
	// itself: the real metadata lives on the DN legs, so a CN21 teardown —
	// which removes the pool device and sends no `delete` — leaves every
	// thin id in place for the next hosting cntlr to attach (CN14, CN21).
	// A fakeDm's thinIds field aliases this map, so the message handlers
	// need no separate lookup.
	thinPools map[string]map[uint32]bool
	// lsGhosts are names `dmsetup ls` reports that no longer exist. The
	// listing is an inherently stale snapshot — another cntlr's retire or
	// SP_LEVEL_DISABLE teardown runs under the same node *read* lock and can
	// remove a wrapper between the `ls` and the `dmsetup table` of that one
	// name — and this is how the suite reproduces that window deterministically
	// ([D14]).
	lsGhosts []string

	// md arrays, keyed by the /dev/md/{name} path
	arrays map[string]*fakeArray
	// superblocks is the set of member devices carrying md metadata.
	superblocks map[string]bool
	// assembleDrop models mdadm leaving a member out of an assembly for
	// stale metadata (§11.1.1 case 1.3): the array starts without it and the
	// agent has to re-add it.
	assembleDrop map[string]bool

	// the §3.2 base state: the clone-metadata arena is a plain file on a
	// tmpfs mount behind one loop device, carved by kind-`b` dm wrappers
	// ([D14]) — no LVM state of any kind.
	mounts   map[string]string
	loops    map[string][]string // backing file → loop devices
	plain    map[string]uint64   // plain files → size
	nextLoop int

	// configfs / sysfs / directories
	dirs  map[string]bool
	files map[string]string
	links map[string]string

	// local store (WriteProto/ReadProto)
	protos map[string][]byte

	// nvme host connections
	subsystems map[string]*fakeSubsys // nqn → subsystem
	nextSubsys int
	nextCtrl   int
	// anaOf overrides a path's ana_state, keyed "{nqn}|{traddr}:{trsvcid}";
	// unset endpoints come up "optimized".
	anaOf map[string]string
	// nsIdxOf is the nsid a subsystem's namespace reports (default 1).
	nsIdxOf map[string]uint32

	// thinDumps is the scripted `thin_dump` output per pool metadata path;
	// without one the fake synthesizes an empty document from the pool whose
	// metadata snapshot is currently reserved.
	thinDumps    map[string]string
	lastSnapPool string
	// dispatchStderr lets one command answer with the stderr the kernel
	// really prints — the EBUSY of a second reserve_metadata_snap is what
	// CN25's release-and-retry-once branches on.
	dispatchStderr string

	// failCmd fails the first matching command; failCmdAlways every one.
	failCmd       map[string]string
	failCmdAlways map[string]string
	// gate blocks a command until the channel is closed (lock tests).
	gate map[string]chan struct{}
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
	discards          []string
	// thin-pool bookkeeping
	thinIds  map[uint32]bool
	snapshot bool
	// heldRoot models a reserved metadata snapshot.
	heldRoot bool
}

type fakeArray struct {
	name    string // the mdadm --name value
	members []string
	state   string
}

type fakeSubsys struct {
	idx   int
	nqn   string
	ctrls []*fakeCtrl
}

type fakeCtrl struct {
	name     string // "nvme3"
	trAddr   string
	trSvcId  string
	state    string
	anaState string
	pathDev  string // "nvme0c3n1"
}

func newFakeNode() *fakeNode {
	f := &fakeNode{
		devSize:       make(map[string]uint64),
		devNo:         make(map[string]string),
		nextMinor:     1,
		dms:           make(map[string]*fakeDm),
		thinPools:     make(map[string]map[uint32]bool),
		arrays:        make(map[string]*fakeArray),
		superblocks:   make(map[string]bool),
		assembleDrop:  make(map[string]bool),
		mounts:        make(map[string]string),
		loops:         make(map[string][]string),
		plain:         make(map[string]uint64),
		dirs:          map[string]bool{common.DefaultLocalStorPrefix: true},
		files:         make(map[string]string),
		links:         make(map[string]string),
		protos:        make(map[string][]byte),
		subsystems:    make(map[string]*fakeSubsys),
		anaOf:         make(map[string]string),
		nsIdxOf:       make(map[string]uint32),
		thinDumps:     make(map[string]string),
		failCmd:       make(map[string]string),
		failCmdAlways: make(map[string]string),
		gate:          make(map[string]chan struct{}),
	}
	f.dirs[sysfsNvmeSubsysDir] = true
	f.dirs[sysfsNvmeCtrlDir] = true
	return f
}

// osClient is the cn role's OsClient double. The block-IO halves are NOT wired
// here any more: the CN11 probe does not use the OsClient (osclient.md §4.5.1), so the
// only cn caller of WriteBlock/ReadBlockDirect is the LegProbeIO double below.
// ReadBlockFn stays connected so that a buffered read — which the probe must
// never issue — is still recorded rather than silently succeeding.
func (f *fakeNode) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn:      f.runCommand,
		ReadFileFn:        f.readFile,
		WriteFileFn:       f.writeFile,
		WriteFileDirectFn: f.writeFileDirect,
		ReadBlockFn:       f.readBlock,
		ReadProtoFn:       f.readProto,
		WriteProtoFn:      f.writeProto,
	}
}

// fakeProbeIO is the LegProbeIO double, in FakeOsClient's fn-field style: set
// only the halves a test needs; unset fields succeed with zero values.
type fakeProbeIO struct {
	WriteFn      func(ctx context.Context, path string, offset uint64, data []byte) error
	ReadDirectFn func(ctx context.Context, path string, offset, length uint64) ([]byte, error)
}

var _ LegProbeIO = (*fakeProbeIO)(nil)

func (f *fakeProbeIO) Write(
	ctx context.Context, path string, offset uint64, data []byte,
) error {
	if f.WriteFn != nil {
		return f.WriteFn(ctx, path, offset, data)
	}
	return nil
}

func (f *fakeProbeIO) ReadDirect(
	ctx context.Context, path string, offset, length uint64,
) ([]byte, error) {
	if f.ReadDirectFn != nil {
		return f.ReadDirectFn(ctx, path, offset, length)
	}
	return nil, nil
}

// probeIO routes the CN11 prober's block IO into the same recorder the
// OsClient halves used, so the call assertions are unchanged by the carve-out. Every test
// server gets one: with the real directLegProbeIO a fired round would open
// /dev/mapper/dnv-… on the machine running the suite.
func (f *fakeNode) probeIO() *fakeProbeIO {
	return &fakeProbeIO{
		WriteFn:      f.writeBlock,
		ReadDirectFn: f.readBlockDirect,
	}
}

// ---------------------------------------------------------------------------
// Call recording
// ---------------------------------------------------------------------------

func (f *fakeNode) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

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

// SysfsNoDeadline is the SH15 evidence: the sysfs paths read on a
// ctx with no SH15 deadline. Assertions live in cnagent_test.go, so that the
// fake never touches *testing.T from the prober goroutines calling into it.
func (f *fakeNode) SysfsNoDeadline() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sysfsNoDeadline...)
}

// readOnlyPrefixes are the probes; everything else changes the system.
var readOnlyPrefixes = []string{
	"cmd ls ", "cmd lsblk ", "cmd stat ", "cmd findmnt ",
	"cmd dmsetup info", "cmd dmsetup table", "cmd dmsetup status",
	"cmd dmsetup ls", "cmd nvme list-subsys",
	"cmd losetup --associated", "cmd mdadm --detail", "cmd mdadm --examine",
	"cmd thin_dump ", "read ", "readproto ", "readblock ", "readblockdirect ",
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

func (f *fakeNode) indexOfCall(substr string) int {
	return f.indexOfCallFrom(substr, 0)
}

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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("read %s", path)
	if _, ok := ctx.Deadline(); !ok && strings.HasPrefix(path, "/sys/") {
		// SH15. The local store under --local-store is a
		// plain-file path and is deliberately not covered by the prefix.
		f.sysfsNoDeadline = append(f.sysfsNoDeadline, path)
	}
	data, ok := f.files[path]
	if !ok {
		return "", fmt.Errorf("no such file: %s", path)
	}
	return data, nil
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

// readBlock / writeBlock / readBlockDirect model the CN11 health probe: the
// leg wrapper accepts a 4 KiB write at the health offset and reads it back.
// The last two are reached through fakeProbeIO, not the OsClient (osclient.md §4.5.1).
func (f *fakeNode) readBlock(
	ctx context.Context, path string, offset uint64, length uint64,
) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("readblock %s off=%d len=%d", path, offset, length)
	return make([]byte, length), nil
}

func (f *fakeNode) writeBlock(
	ctx context.Context, path string, offset uint64, data []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writeblock %s off=%d len=%d", path, offset, len(data))
	if _, ok := f.devNo[path]; !ok {
		return fmt.Errorf("no such device: %s", path)
	}
	return nil
}

func (f *fakeNode) readBlockDirect(
	ctx context.Context, path string, offset uint64, length uint64,
) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("readblockdirect %s off=%d len=%d", path, offset, length)
	if _, ok := f.devNo[path]; !ok {
		return nil, fmt.Errorf("no such device: %s", path)
	}
	return make([]byte, length), nil
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

func (f *fakeNode) runCommand(
	ctx context.Context,
	name string,
	args []string,
	stdin string,
) (string, string, int, error) {
	line := "cmd " + name + " " + strings.Join(args, " ")
	if stdin != "" {
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
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", "", -1, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatchStderr = ""
	stdout, code := f.dispatch(name, args, stdin)
	if code != 0 {
		stderr := f.dispatchStderr
		if stderr == "" {
			stderr = "fake: " + name + " failed"
		}
		return stdout, stderr, code, fmt.Errorf("exit status %d", code)
	}
	return stdout, "", 0, nil
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
	case "mdadm":
		return f.cmdMdadm(args)
	case "mount":
		return f.cmdMount(args)
	case "findmnt":
		return f.cmdFindmnt(args)
	case "stat":
		return f.cmdStat(args)
	case "truncate":
		return f.cmdTruncate(args)
	case "losetup":
		return f.cmdLosetup(args)
	case "thin_dump":
		return f.cmdThinDump(args)
	}
	return "", 127
}

func (f *fakeNode) children(path string) []string {
	seen := make(map[string]struct{})
	for entry := range f.dirs {
		if child, ok := childOf(path, entry); ok {
			seen[child] = struct{}{}
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
// makes the kernel populate its standard subdirectories.
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

func (f *fakeNode) cmdRmdir(args []string) (string, int) {
	path := args[len(args)-1]
	if !f.dirs[path] {
		return "", 1
	}
	prefix := path + "/"
	for entry := range f.dirs {
		if strings.HasPrefix(entry, prefix) {
			delete(f.dirs, entry)
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

// heldBy reports the dm device or md array whose live table still maps path,
// i.e. whoever holds it open. dm and md both refuse to release a device that
// something above them still references.
func (f *fakeNode) heldBy(path string) string {
	devNo := f.devNo[path]
	for name, dm := range f.dms {
		if f.devNo["/dev/mapper/"+name] == devNo && devNo != "" {
			continue
		}
		for _, line := range dmTargets(dm.table) {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			for _, field := range fields[3:] {
				if devNo != "" && field == devNo {
					return name
				}
			}
		}
	}
	for dev, array := range f.arrays {
		for _, member := range array.members {
			if member == path {
				return dev
			}
		}
	}
	return ""
}

func (f *fakeNode) newDevNo() string {
	f.nextMinor++
	return fmt.Sprintf("253:%d", f.nextMinor)
}

func (f *fakeNode) cmdBlkdiscard(args []string) (string, int) {
	dev := args[len(args)-1]
	name := strings.TrimPrefix(dev, "/dev/mapper/")
	if dm, ok := f.dms[name]; ok {
		dm.discards = append(dm.discards,
			strings.Join(args[:len(args)-1], " "))
	}
	return "", 0
}

// ---------------------------------------------------------------------------
// device-mapper
// ---------------------------------------------------------------------------

func (f *fakeNode) cmdDmsetup(args []string, stdin string) (string, int) {
	if len(args) == 0 {
		return "", 3
	}
	switch args[0] {
	case "ls":
		// The real tool prints "{name}\t({major}:{minor})" per device and the
		// literal "No devices found" — still exit 0 — on an empty node, which
		// is what the allocator's prefix filter has to drop ([D14]).
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
			// makes a retire phase that runs out of order fail a test
			// rather than only a real node.
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
	dm := &fakeDm{
		table:    table,
		readOnly: readOnly,
		thinIds:  f.poolThinIds(name, table),
	}
	applyCloneTable(dm, table)
	if code := f.checkTableDeps(table); code != 0 {
		return "", code
	}
	if code := f.checkThinTable(name, table); code != 0 {
		return "", code
	}
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
	if code := f.checkTableDeps(table); code != 0 {
		return "", code
	}
	dm.table = table
	dm.readOnly = readOnly
	applyCloneTable(dm, table)
	f.devSize["/dev/mapper/"+name] = tableSectors(table) * 512
	return "", 0
}

// poolThinIds is the thin-id set a freshly activated device carries. For a
// thin-pool it is the node-level metadata of that pool name, which outlives
// the dm device: the real thing lives on the DN legs, so a cntlr teardown
// (CN21, no `delete` messages) and the next `dmsetup create` of the pool find
// the same ids. Every other target gets its own empty map, which nothing
// reads.
func (f *fakeNode) poolThinIds(name, table string) map[uint32]bool {
	if !isDmTarget(table, "thin-pool") {
		return map[uint32]bool{}
	}
	held, ok := f.thinPools[name]
	if !ok {
		held = make(map[uint32]bool)
		f.thinPools[name] = held
	}
	return held
}

// holdThinIds seeds a pool's dm-thin metadata with ids no cntlr of this node
// created — the state a fresh primary meets when the control plane has
// already marked the tds `created` (U4-S2).
func (f *fakeNode) holdThinIds(pool string, ids ...uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.thinPools[pool]
	if !ok {
		held = make(map[uint32]bool)
		f.thinPools[pool] = held
	}
	for _, id := range ids {
		held[id] = true
	}
}

// isDmTarget reports whether table is a single target of the given type.
func isDmTarget(table, target string) bool {
	lines := dmTargets(table)
	if len(lines) != 1 {
		return false
	}
	fields := strings.Fields(lines[0])
	return len(fields) > 2 && fields[2] == target
}

// checkThinTable rejects a `thin` table whose dev_id the pool's metadata does
// not hold. The kernel does exactly that, and modelling it is what makes
// U4-S2 observable: a created td is never re-created by message, so a pool
// that lost the id has to surface as a failing `dmsetup create` — an
// RES_STATUS_ERROR the worker records — instead of a fresh empty volume
// quietly taking the dev_id over.
func (f *fakeNode) checkThinTable(name, table string) int {
	if !isDmTarget(table, "thin") {
		return 0
	}
	fields := strings.Fields(dmTargets(table)[0])
	if len(fields) < 5 {
		return 3
	}
	pool := f.dmByDevNo(fields[3])
	if pool == nil {
		return 0
	}
	devId, err := strconv.ParseUint(fields[4], 10, 32)
	if err != nil {
		return 3
	}
	if pool.thinIds[uint32(devId)] {
		return 0
	}
	f.dispatchStderr = "device-mapper: reload ioctl on " + name +
		" failed: No data available"
	return 1
}

// dmByDevNo resolves the major:minor a table argument carries back to the dm
// device it names, or nil when it is not one of this node's dm devices.
func (f *fakeNode) dmByDevNo(devNo string) *fakeDm {
	for name, dm := range f.dms {
		if f.devNo["/dev/mapper/"+name] == devNo {
			return dm
		}
	}
	return nil
}

// checkTableDeps rejects a table naming a device number that does not exist —
// the kernel's behavior, and what makes an out-of-order build fail in tests.
func (f *fakeNode) checkTableDeps(table string) int {
	known := make(map[string]bool, len(f.devNo))
	for _, devNo := range f.devNo {
		known[devNo] = true
	}
	for _, line := range dmTargets(table) {
		fields := strings.Fields(line)
		for _, field := range fields[3:] {
			if strings.Count(field, ":") != 1 {
				continue
			}
			if !known[field] {
				return 1
			}
		}
	}
	return 0
}

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
	fields := strings.Fields(message)
	switch {
	case message == "enable_hydration":
		dm.noHydration = false
	case message == "disable_hydration":
		dm.noHydration = true
	case strings.HasPrefix(message, "hydration_threshold "):
		value, _ := strconv.ParseUint(fields[1], 10, 32)
		dm.threshold = uint32(value)
	case strings.HasPrefix(message, "hydration_batch_size "):
		value, _ := strconv.ParseUint(fields[1], 10, 32)
		dm.batchSize = uint32(value)
	case strings.HasPrefix(message, "create_thin "):
		devId, _ := strconv.ParseUint(fields[1], 10, 32)
		if dm.thinIds[uint32(devId)] {
			return "", 1
		}
		dm.thinIds[uint32(devId)] = true
	case strings.HasPrefix(message, "create_snap "):
		if len(fields) < 3 {
			return "", 3
		}
		devId, _ := strconv.ParseUint(fields[1], 10, 32)
		oriId, _ := strconv.ParseUint(fields[2], 10, 32)
		// dm-thin rejects a create_snap whose origin the pool does not hold.
		// That is the U4-S5/R11 failure mode: the gateway's job is to keep
		// the origin materialized, and an agent that meets a violated
		// precondition simply reports the error and retries.
		if dm.thinIds[uint32(devId)] || !dm.thinIds[uint32(oriId)] {
			return "", 1
		}
		dm.thinIds[uint32(devId)] = true
		dm.snapshot = true
	case strings.HasPrefix(message, "delete "):
		devId, _ := strconv.ParseUint(fields[1], 10, 32)
		delete(dm.thinIds, uint32(devId))
	case message == "reserve_metadata_snap":
		if dm.heldRoot {
			f.dispatchStderr = "device-mapper: message ioctl on " +
				args[1] + " failed: Device or resource busy"
			return "", 1
		}
		dm.heldRoot = true
		f.lastSnapPool = args[1]
	case message == "release_metadata_snap":
		if !dm.heldRoot {
			return "", 1
		}
		dm.heldRoot = false
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
	case "flakey":
		return fmt.Sprintf("0 %s flakey\n", sectors), 0
	case "linear":
		return fmt.Sprintf("0 %s linear %s %s\n",
			sectors, fields[3], fields[4]), 0
	case "striped":
		return fmt.Sprintf("0 %s striped 1 0\n", sectors), 0
	case "thin":
		return fmt.Sprintf("0 %s thin 0 -1\n", sectors), 0
	case "thin-pool":
		held := "-"
		if dm.heldRoot {
			held = "123"
		}
		// The §10.4 auto-grow parses the two used/total pairs out of this.
		return fmt.Sprintf(
			"0 %s thin-pool 0 12/1024 5/%s %s rw discard_passdown "+
				"queue_if_no_space - 1024\n",
			sectors, fields[6], held), 0
	case "clone":
		regionSectors := fields[6]
		regions := tableSectors(dm.table)
		region, _ := strconv.ParseUint(regionSectors, 10, 64)
		if region > 0 {
			regions /= region
		}
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

// ---------------------------------------------------------------------------
// md
// ---------------------------------------------------------------------------

func (f *fakeNode) cmdMdadm(args []string) (string, int) {
	switch {
	case contains(args, "--examine"):
		dev := args[len(args)-1]
		if !f.superblocks[dev] {
			return "", 1
		}
		return "MD_LEVEL=raid1\nMD_DEVICES=2\nMD_NAME=dnv-x\n", 0
	case contains(args, "--create"):
		return f.mdCreate(args)
	case contains(args, "--assemble"):
		return f.mdAssemble(args)
	case contains(args, "--detail"):
		return f.mdDetail(args)
	case contains(args, "--stop"):
		dev := args[len(args)-1]
		if _, ok := f.arrays[dev]; !ok {
			return "", 1
		}
		if holder := f.heldBy(dev); holder != "" {
			f.dispatchStderr = "mdadm: Cannot get exclusive access to " +
				dev + ": held by " + holder
			return "", 1
		}
		delete(f.arrays, dev)
		delete(f.devNo, dev)
		delete(f.devSize, dev)
		return "", 0
	case contains(args, "--add"):
		return f.mdAdd(args)
	case contains(args, "--fail"):
		return "", 0
	case contains(args, "--remove"):
		return f.mdRemove(args)
	}
	return "", 1
}

func mdFlagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// mdMembers are the trailing device arguments — everything after the last
// flag value that is not itself a flag.
func mdMembers(args []string) []string {
	var out []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "/dev/mapper/") || arg == "missing" {
			out = append(out, arg)
		}
	}
	return out
}

func (f *fakeNode) mdCreate(args []string) (string, int) {
	dev := mdFlagValue(args, "--create")
	name := mdFlagValue(args, "--name")
	var members []string
	for _, member := range mdMembers(args) {
		if member == "missing" {
			continue
		}
		members = append(members, member)
		f.superblocks[member] = true
	}
	f.arrays[dev] = &fakeArray{name: name, members: members,
		state: "clean"}
	f.devNo[dev] = f.newDevNo()
	f.devSize[dev] = f.arraySize(members)
	return "", 0
}

func (f *fakeNode) mdAssemble(args []string) (string, int) {
	dev := mdFlagValue(args, "--assemble")
	name := mdFlagValue(args, "--name")
	var members []string
	for _, member := range mdMembers(args) {
		if !f.superblocks[member] {
			return "", 1
		}
		if f.assembleDrop[member] {
			continue
		}
		members = append(members, member)
	}
	if len(members) == 0 {
		return "", 1
	}
	f.arrays[dev] = &fakeArray{name: name, members: members,
		state: "clean, degraded"}
	if len(members) > 1 {
		f.arrays[dev].state = "clean"
	}
	f.devNo[dev] = f.newDevNo()
	f.devSize[dev] = f.arraySize(members)
	return "", 0
}

// arraySize models a raid1 whose members carry a --data-offset: the array is
// the member minus the offset. Tests set the leg wrapper sizes, and the agent
// never probes this — it sizes from the desired state — so a simple mirror of
// the first member is enough for the dm tables built on top.
func (f *fakeNode) arraySize(members []string) uint64 {
	if len(members) == 0 {
		return 0
	}
	return f.devSize[members[0]]
}

func (f *fakeNode) mdDetail(args []string) (string, int) {
	dev := args[len(args)-1]
	array, ok := f.arrays[dev]
	if !ok {
		return "", 1
	}
	if contains(args, "--export") {
		var sb strings.Builder
		sb.WriteString("MD_LEVEL=raid1\n")
		fmt.Fprintf(&sb, "MD_DEVICES=%d\n", len(array.members))
		fmt.Fprintf(&sb, "MD_NAME=%s\n", array.name)
		for i, member := range array.members {
			key := strings.NewReplacer("/", "_", "-", "_").Replace(
				strings.TrimPrefix(member, "/"))
			fmt.Fprintf(&sb, "MD_DEVICE_%s_ROLE=%d\n", key, i)
			fmt.Fprintf(&sb, "MD_DEVICE_%s_DEV=%s\n", key, member)
		}
		return sb.String(), 0
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s:\n", dev)
	fmt.Fprintf(&sb, "        Version : 1.2\n")
	fmt.Fprintf(&sb, "          State : %s\n", array.state)
	fmt.Fprintf(&sb, "           Name : %s\n", array.name)
	return sb.String(), 0
}

func (f *fakeNode) mdAdd(args []string) (string, int) {
	dev := args[0]
	array, ok := f.arrays[dev]
	if !ok {
		return "", 1
	}
	member := args[len(args)-1]
	array.members = append(array.members, member)
	f.superblocks[member] = true
	array.state = "clean"
	return "", 0
}

func (f *fakeNode) mdRemove(args []string) (string, int) {
	dev := args[0]
	array, ok := f.arrays[dev]
	if !ok {
		return "", 1
	}
	member := args[len(args)-1]
	var kept []string
	for _, held := range array.members {
		if held != member {
			kept = append(kept, held)
		}
	}
	array.members = kept
	return "", 0
}

// ---------------------------------------------------------------------------
// tmpfs / loop
// ---------------------------------------------------------------------------

func (f *fakeNode) cmdMount(args []string) (string, int) {
	path := args[len(args)-1]
	f.mounts[path] = "tmpfs"
	f.dirs[path] = true
	return "", 0
}

func (f *fakeNode) cmdFindmnt(args []string) (string, int) {
	path := args[len(args)-1]
	fsType, ok := f.mounts[path]
	if !ok {
		return "", 1
	}
	if contains(args, "TARGET") {
		return path + "\n", 0
	}
	return fsType + "\n", 0
}

func (f *fakeNode) cmdStat(args []string) (string, int) {
	path := args[len(args)-1]
	size, ok := f.plain[path]
	if !ok {
		return "", 1
	}
	return fmt.Sprintf("%d\n", size), 0
}

func (f *fakeNode) cmdTruncate(args []string) (string, int) {
	path := args[len(args)-1]
	size, _ := strconv.ParseUint(mdFlagValue(args, "--size"), 10, 64)
	f.plain[path] = size
	return "", 0
}

func (f *fakeNode) cmdLosetup(args []string) (string, int) {
	if contains(args, "--associated") {
		path := args[len(args)-1]
		devs := f.loops[path]
		// util-linux answers "nothing is attached" with exit 0 and no output;
		// only a real failure of the tool exits non-zero, which is the whole
		// distinction LoopDevices now makes.
		if len(devs) == 0 {
			return "", 0
		}
		var sb strings.Builder
		for _, dev := range devs {
			fmt.Fprintf(&sb, "%s: 0 %s\n", dev, path)
		}
		return sb.String(), 0
	}
	if contains(args, "--find") {
		path := args[len(args)-1]
		dev := fmt.Sprintf("/dev/loop%d", f.nextLoop)
		f.nextLoop++
		f.loops[path] = append(f.loops[path], dev)
		f.devNo[dev] = f.newDevNo()
		f.devSize[dev] = f.plain[path]
		return dev + "\n", 0
	}
	return "", 1
}

func (f *fakeNode) cmdThinDump(args []string) (string, int) {
	path := args[len(args)-1]
	if dump, ok := f.thinDumps[path]; ok {
		return dump, 0
	}
	// A pool whose thin devices exist but have never been written: every
	// device is present in the metadata with no mappings at all.
	var sb strings.Builder
	sb.WriteString(`<superblock uuid="" time="0" transaction="0" ` +
		`flags="0" version="2" data_block_size="2048" ` +
		"nr_data_blocks=\"0\">\n")
	if pool, ok := f.dms[f.lastSnapPool]; ok {
		ids := make([]int, 0, len(pool.thinIds))
		for id := range pool.thinIds {
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		for _, id := range ids {
			fmt.Fprintf(&sb, "  <device dev_id=\"%d\" mapped_blocks=\"0\" "+
				"transaction=\"0\" creation_time=\"0\" snap_time=\"0\">\n"+
				"  </device>\n", id)
		}
	}
	sb.WriteString("</superblock>\n")
	return sb.String(), 0
}

// ---------------------------------------------------------------------------
// nvme host + the sysfs tree the leg probe walks
// ---------------------------------------------------------------------------

func (f *fakeNode) cmdNvme(args []string) (string, int) {
	switch args[0] {
	case "connect":
		return f.nvmeConnect(args)
	case "disconnect":
		return f.nvmeDisconnect(args)
	case "list-subsys":
		return f.listSubsysJson(), 0
	}
	return "", 3
}

func (f *fakeNode) nvmeConnect(args []string) (string, int) {
	nqn := mdFlagValue(args, "--nqn")
	trAddr := mdFlagValue(args, "--traddr")
	trSvcId := mdFlagValue(args, "--trsvcid")
	if nqn == "" {
		return "", 3
	}
	subsys, ok := f.subsystems[nqn]
	if !ok {
		subsys = &fakeSubsys{idx: f.nextSubsys, nqn: nqn}
		f.nextSubsys++
		f.subsystems[nqn] = subsys
		dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, subsys.idx)
		f.dirs[dir] = true
		f.files[dir+"/subsysnqn"] = nqn + "\n"
		nsIdx := uint32(1)
		if want, ok := f.nsIdxOf[nqn]; ok {
			nsIdx = want
		}
		nsDev := fmt.Sprintf("nvme%dn%d", subsys.idx, nsIdx)
		f.dirs[dir+"/"+nsDev] = true
		f.files[dir+"/"+nsDev+"/nsid"] = fmt.Sprintf("%d\n", nsIdx)
		f.devNo["/dev/"+nsDev] = f.newDevNo()
		f.devSize["/dev/"+nsDev] = 1 << 40
	}
	ctrlName := fmt.Sprintf("nvme%d", f.nextCtrl)
	f.nextCtrl++
	ana := "optimized"
	if want, ok := f.anaOf[nqn+"|"+trAddr+":"+trSvcId]; ok {
		ana = want
	}
	pathDev := fmt.Sprintf("nvme%dc%sn1", subsys.idx,
		strings.TrimPrefix(ctrlName, "nvme"))
	ctrl := &fakeCtrl{
		name: ctrlName, trAddr: trAddr, trSvcId: trSvcId,
		state: "live", anaState: ana, pathDev: pathDev,
	}
	subsys.ctrls = append(subsys.ctrls, ctrl)

	subsysDir := fmt.Sprintf("%s/nvme-subsys%d",
		sysfsNvmeSubsysDir, subsys.idx)
	f.dirs[subsysDir+"/"+ctrlName] = true
	ctrlDir := sysfsNvmeCtrlDir + "/" + ctrlName
	f.dirs[ctrlDir] = true
	f.files[ctrlDir+"/address"] = fmt.Sprintf(
		"traddr=%s,trsvcid=%s,src_addr=%s\n", trAddr, trSvcId, trAddr)
	f.files[ctrlDir+"/state"] = ctrl.state + "\n"
	f.dirs[ctrlDir+"/"+pathDev] = true
	f.files[ctrlDir+"/"+pathDev+"/ana_state"] = ctrl.anaState + "\n"
	return "", 0
}

func (f *fakeNode) nvmeDisconnect(args []string) (string, int) {
	if nqn := mdFlagValue(args, "--nqn"); nqn != "" {
		subsys, ok := f.subsystems[nqn]
		if !ok {
			return "", 1
		}
		for _, ctrl := range append([]*fakeCtrl(nil), subsys.ctrls...) {
			f.dropCtrl(subsys, ctrl)
		}
		return "", 0
	}
	dev := mdFlagValue(args, "--device")
	for _, subsys := range f.subsystems {
		for _, ctrl := range subsys.ctrls {
			if ctrl.name == dev {
				f.dropCtrl(subsys, ctrl)
				return "", 0
			}
		}
	}
	// `nvme disconnect --device` is not idempotent.
	return "", 1
}

func (f *fakeNode) dropCtrl(subsys *fakeSubsys, ctrl *fakeCtrl) {
	subsysDir := fmt.Sprintf("%s/nvme-subsys%d",
		sysfsNvmeSubsysDir, subsys.idx)
	delete(f.dirs, subsysDir+"/"+ctrl.name)
	ctrlDir := sysfsNvmeCtrlDir + "/" + ctrl.name
	for entry := range f.dirs {
		if strings.HasPrefix(entry, ctrlDir) {
			delete(f.dirs, entry)
		}
	}
	for entry := range f.files {
		if strings.HasPrefix(entry, ctrlDir) {
			delete(f.files, entry)
		}
	}
	var kept []*fakeCtrl
	for _, held := range subsys.ctrls {
		if held != ctrl {
			kept = append(kept, held)
		}
	}
	subsys.ctrls = kept
	if len(kept) > 0 {
		return
	}
	for entry := range f.dirs {
		if strings.HasPrefix(entry, subsysDir) {
			delete(f.dirs, entry)
		}
	}
	for entry := range f.files {
		if strings.HasPrefix(entry, subsysDir) {
			delete(f.files, entry)
		}
	}
	delete(f.dirs, subsysDir)
	nsDev := fmt.Sprintf("/dev/nvme%dn1", subsys.idx)
	delete(f.devNo, nsDev)
	delete(f.devSize, nsDev)
	delete(f.subsystems, subsys.nqn)
}

// setAnaState re-stamps a live path's ana_state, so a test can make a leg
// unavailable the way a standby's side does.
func (f *fakeNode) setAnaState(nqn, trAddr, trSvcId, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.anaOf[nqn+"|"+trAddr+":"+trSvcId] = state
	subsys, ok := f.subsystems[nqn]
	if !ok {
		return
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.trAddr != trAddr || ctrl.trSvcId != trSvcId {
			continue
		}
		ctrl.anaState = state
		f.files[sysfsNvmeCtrlDir+"/"+ctrl.name+"/"+ctrl.pathDev+
			"/ana_state"] = state + "\n"
	}
}

func (f *fakeNode) listSubsysJson() string {
	nqns := make([]string, 0, len(f.subsystems))
	for nqn := range f.subsystems {
		nqns = append(nqns, nqn)
	}
	sort.Strings(nqns)
	subsystems := make([]map[string]any, 0, len(nqns))
	for _, nqn := range nqns {
		subsys := f.subsystems[nqn]
		paths := make([]map[string]any, 0, len(subsys.ctrls))
		for _, ctrl := range subsys.ctrls {
			paths = append(paths, map[string]any{
				"Name":  ctrl.name,
				"State": ctrl.state,
				"Address": fmt.Sprintf("traddr=%s,trsvcid=%s",
					ctrl.trAddr, ctrl.trSvcId),
			})
		}
		subsystems = append(subsystems, map[string]any{
			"Name":  fmt.Sprintf("nvme-subsys%d", subsys.idx),
			"NQN":   nqn,
			"Paths": paths,
			"Namespaces": []map[string]any{
				{"NameSpace": fmt.Sprintf("nvme%dn1", subsys.idx), "NSID": 1},
			},
		})
	}
	raw, _ := json.Marshal([]map[string]any{
		{"HostNQN": "fake", "Subsystems": subsystems},
	})
	return string(raw)
}
