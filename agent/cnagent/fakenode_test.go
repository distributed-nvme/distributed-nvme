package cnagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// fakeNode is an in-memory model of the slice of the OS the cn agent drives:
// device-mapper, md, the tmpfs/loop base state, the nvmet configfs tree,
// the sysfs nvme-subsystem tree the leg probe walks, thin-provisioning-tools
// and the local store. It backs common.FakeOsClient, records every call, and
// lets tests assert command sequences, probe-first idempotency and teardown
// order without root or real devices.
type fakeNode struct {
	mu sync.Mutex

	calls []string
	// sysfsNoDeadline records every /sys read that arrived on a ctx carrying
	// no deadline. The leg walk's sysfs reads carry the SH15 soft timeout
	// (readSysfs takes it from agent.CmdCtx), so this must stay empty.
	sysfsNoDeadline []string
	// traced is every command and file read keyed by the trace id of the
	// ctx it ran on: the trace_id its `os command` / `os read file` record
	// carries in production (log.md R5); "" for a ctx without one.
	traced map[string][]string

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
	// dmLsLegacyDevNo switches `dmsetup ls` to the older "(253, 4)" device
	// number spelling. Dm.List normalizes both into "253:4"; nothing on the
	// lab kernels prints the comma form any more, so this is the only way
	// the second branch of that normalization is ever exercised.
	dmLsLegacyDevNo bool

	// md arrays, keyed by the /dev/md/{name} path
	arrays map[string]*fakeArray
	// nextMdMinor numbers the kernel node, mdN, an array is published under
	// when the test set no fakeArray.node. An array has two spellings: the
	// /dev/md/{name} symlink mdadm created it with, and the /dev/mdN (or
	// /dev/md_<name>) the kernel owns — and a sweep only ever has the second
	// one, because it enumerates /sys/block (md.go ListArrays).
	nextMdMinor int
	// superblocks is the set of member devices carrying md metadata.
	superblocks map[string]bool
	// assembleDrop models mdadm leaving a member out of an assembly for
	// stale metadata (architecture.md, "Make sure all groups are available",
	// case 1.3): the array starts without it and the
	// agent has to re-add it.
	assembleDrop map[string]bool

	// the base state (architecture.md, Controller node, common): the
	// clone-metadata arena is a plain file on a
	// tmpfs mount behind one loop device, carved by kind-`cb` dm wrappers
	// ([D14]) — no LVM state of any kind.
	mounts   map[string]string
	loops    map[string][]string // backing file → loop devices
	plain    map[string]uint64   // plain files → size
	nextLoop int

	// configfs / sysfs / directories
	dirs  map[string]bool
	files map[string]string
	links map[string]string
	// errFiles are attributes that exist and whose read fails — an unbound
	// md member's dev-*/state reads ENODEV. They belong to the tree they are
	// published in and go with it, unlike the failRead* hooks below.
	errFiles map[string]error

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
	// connectFail refuses the first N `nvme connect`s to one endpoint, keyed
	// by connectKey, and then lets them through: the disk node that links
	// its export into its port a few milliseconds after the primary's first
	// connect. Unlike failCmd*, the refusal is DISPATCHED —
	// it reaches nvmeConnect, answers exit 1 with the kernel's words and
	// leaves no controller behind — so it can be counted per endpoint and
	// combined with connectTakes.
	connectFail map[string]int
	// connectTakes makes every `nvme connect` to one endpoint take a fake
	// duration: nvmeConnect moves the clock the test handed it (advance) on
	// by that much before it answers — a connect to a disk node whose VM is
	// down spends CmdSoftTimeout. Without an advance it is inert.
	connectTakes map[string]time.Duration
	advance      func(time.Duration)
	// duringConnect runs, under the fake's lock, when a connect to one
	// endpoint (connectKey) succeeds: what else changes on the node while
	// that connect runs — a migrating leg's src controller whose `address`
	// stops answering, say, which the pass's re-read after the connect then
	// meets. It must not take the lock.
	duringConnect map[string]func()
	// nsHeadAfter defers a subsystem's multipath namespace head, keyed by
	// NQN: the connect that creates the subsystem adds its controller but
	// not the head, which appears on the Nth listing of the subsystem
	// directory — the kernel's namespace scan, which `nvme connect` only
	// queues. 0 or 1 is the ordinary connect.
	nsHeadAfter map[string]int
	// pendingHeads are the heads nsHeadAfter deferred, keyed by the
	// subsystem's sysfs directory.
	pendingHeads map[string]*pendingHead

	// thinDumps is the scripted `thin_dump` output per pool metadata path;
	// without one the fake synthesizes an empty document from the pool whose
	// metadata snapshot is currently reserved.
	thinDumps    map[string]string
	lastSnapPool string
	// dispatchStderr lets one command answer with the stderr the kernel
	// really prints — the EBUSY of a second reserve_metadata_snap is what
	// CN25's release-and-retry-once branches on.
	dispatchStderr string
	// stdouts is the stdout every dispatched command answered with, per
	// tool, in call order: what the production client logs in full as the
	// `os command` record's stdout (log.md), so a test can bound what a
	// command puts there.
	stdouts map[string][]string

	// failCmd fails the first matching command; failCmdAlways every one.
	// Both model "the tool ran and answered no": exit code 1, a non-nil
	// error, and no dispatch, so the fake's state is left exactly as the
	// real kernel would leave it after a refused ioctl.
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
	// killCmdNth is killCmd counted: it kills only the Nth command matching
	// its key, counting from when the key is set, and every match the fail
	// hooks let through counts. It is how a test kills one read of a device
	// the pass reads several times — the fourth `dmsetup status` of a
	// dm-clone and not the first.
	killCmdNth map[string]int
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
	// nvme host walk and the md sysfs walk both read through, tests for
	// fs.ErrNotExist and nothing else.
	failRead       map[string]bool
	failReadAlways map[string]bool
	killRead       map[string]bool
	killReadAlways map[string]bool
	// gate blocks a command until the channel is closed (lock tests).
	gate map[string]chan struct{}
	// hardGate blocks a command until the channel is closed and — unlike gate
	// — ignores ctx cancellation, modelling the real child of
	// common/osclient.go: a child in an uninterruptible kernel wait (an
	// `nvme disconnect` whose controller delete waits out the admin timeout)
	// is signalled at the soft and hard timeouts and still does not return
	// until the kernel does. The dn twin of this fake carries the same knob.
	hardGate map[string]chan struct{}
	// traceOf records, per recorded command line, the trace id its ctx
	// carried ("" for none). nil leaves it off.
	traceOf map[string]string
	// cancelCmd models a caller whose ctx is cancelled while a matching
	// command runs — a client that went away, a gateway deadline. The first
	// match calls its cancel func; the command has dispatched, as killCmd's
	// does, and answers what exec answers for a child killed on a done ctx:
	// exit code -1 with ctx.Err(). Everything the caller runs afterwards on
	// that ctx fails runCommand's ctx check, as it does in the production
	// client.
	cancelCmd map[string]context.CancelFunc
	// dirStat scripts the `stat --format "%u %a %F"` answer per path —
	// owner uid, octal mode, file type. A directory the fake holds without
	// one answers as a private directory of the agent's own user.
	dirStat map[string]string
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
	// node is the kernel's own name for the array: mdN by default
	// (installArray numbers it from nextMdMinor), or md_<name> when a test
	// sets it, as mdadm.conf `CREATE names=yes` makes it. It is what
	// /sys/block publishes the array under and the only spelling a sweep
	// ever sees.
	node string
	// arrayState is /sys/block/{node}/md/array_state: "clean"/"active" for a
	// running array, "inactive" for one that is assembled but not running —
	// which still pins its members — and "clear" for one that is gone.
	arrayState string
	// degraded, syncAction and syncCompleted are md/degraded,
	// md/sync_action and md/sync_completed, scripted (setMdSync). They are
	// published only while arrayState is a running one: the kernel has none
	// of the three for an inactive array.
	degraded      int
	syncAction    string
	syncCompleted string
	// memberState scripts dev-*/state per member path; a member without an
	// entry reads what the kernel shows for dnv's members (memberStateOf).
	memberState map[string]string
	// unbound are the members md has unbound (fakeNode.unbindMember). A
	// republish keeps them unbound; an mdadm --add or --remove of the
	// member, or a stop, ends it.
	unbound map[string]bool
}

type fakeSubsys struct {
	idx   int
	nqn   string
	ctrls []*fakeCtrl
}

// pendingHead is one namespace head the kernel's scan has not added yet.
type pendingHead struct {
	after int // the listing of the subsystem directory that shows it
	reads int // listings so far
	nsDev string
	nsIdx uint32
}

// connectKey is the per-endpoint key of connectFail and connectTakes: one
// side's (nqn, traddr, trsvcid), the triple a controller is connected by.
func connectKey(nqn, trAddr, trSvcId string) string {
	return nqn + "|" + trAddr + ":" + trSvcId
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
		errFiles:      make(map[string]error),
		protos:        make(map[string][]byte),
		subsystems:    make(map[string]*fakeSubsys),
		anaOf:         make(map[string]string),
		nsIdxOf:       make(map[string]uint32),
		connectFail:   make(map[string]int),
		connectTakes:  make(map[string]time.Duration),
		duringConnect: make(map[string]func()),
		nsHeadAfter:   make(map[string]int),
		pendingHeads:  make(map[string]*pendingHead),
		thinDumps:     make(map[string]string),
		failCmd:       make(map[string]string),
		failCmdAlways: make(map[string]string),

		killCmd:               make(map[string]bool),
		killCmdAlways:         make(map[string]bool),
		killCmdNth:            make(map[string]int),
		killCmdNoEffect:       make(map[string]bool),
		killCmdNoEffectAlways: make(map[string]bool),
		failRead:              make(map[string]bool),
		failReadAlways:        make(map[string]bool),
		killRead:              make(map[string]bool),
		killReadAlways:        make(map[string]bool),

		gate:      make(map[string]chan struct{}),
		hardGate:  make(map[string]chan struct{}),
		cancelCmd: make(map[string]context.CancelFunc),
		dirStat:   make(map[string]string),
	}
	f.dirs[sysfsNvmeSubsysDir] = true
	f.dirs[sysfsNvmeCtrlDir] = true
	// /sys/block exists on every node, empty or not: an `ls` of it that does
	// not answer is an error to Md.ListArrays, never "this node runs no
	// arrays", so the fake must not model a missing directory by default.
	f.dirs[sysfsBlockDir] = true
	return f
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
// it must, or a command parked by a test that failed early would hold its
// goroutine, and every lock that goroutine holds, for the rest of the run.
func (f *fakeNode) releaseCmd(key string) {
	f.mu.Lock()
	ch, ok := f.hardGate[key]
	delete(f.hardGate, key)
	f.mu.Unlock()
	if ok {
		close(ch)
	}
}

// osClient is the cn role's OsClient double. Its block-write half is left
// unwired: since the probe-IO carve-out the CN11 probe does not use the
// OsClient (osclient.md, Exported raw helpers and the probe-IO carve-out) but
// the raw WriteBlockAt/ReadBlockDirectAt
// helpers, faked through the LegProbeIO double below, and the interface has
// no ReadBlockDirect to wire at all.
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

// recordTrace files one call under its ctx's trace id (traced). Called with
// f.mu held.
func (f *fakeNode) recordTrace(ctx context.Context, line string) {
	traceId, _ := common.TraceIdFromCtx(ctx)
	if f.traced == nil {
		f.traced = make(map[string][]string)
	}
	f.traced[traceId] = append(f.traced[traceId], line)
}

// Traced returns the calls that ran under traceId, in order.
func (f *fakeNode) Traced(traceId string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.traced[traceId]...)
}

func (f *fakeNode) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.stdouts = nil
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
	if err := f.ctxErr(ctx); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("read %s", path)
	f.recordTrace(ctx, "read "+path)
	if _, ok := ctx.Deadline(); !ok && strings.HasPrefix(path, "/sys/") {
		// SH15. The local store under --local-store is a
		// plain-file path and is deliberately not covered by the prefix.
		f.sysfsNoDeadline = append(f.sysfsNoDeadline, path)
	}
	if err := f.readHookErr(path); err != nil {
		return "", err
	}
	if err, ok := f.errFiles[path]; ok {
		return "", err
	}
	data, ok := f.files[path]
	if !ok {
		// The absent-file error MUST wrap fs.ErrNotExist. The production
		// LimitedOsClient returns os.ReadFile's *fs.PathError, and
		// agent.readAttrStrict — which the nvme host walk and the md sysfs
		// walk read through — tells "absent" from "did not answer" by
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
	if err := f.cntlidRefusal(path, data); err != nil {
		return err
	}
	f.files[path] = configfsNormalize(path, data)
	return nil
}

// cntlidRefusal models nvmet's store handlers for a subsystem's cntlid bounds
// (nvmet_subsys_attr_cntlid_{min,max}_store): a bound of 0, an
// attr_cntlid_min above the current attr_cntlid_max, and an attr_cntlid_max
// below the current attr_cntlid_min are refused with EINVAL and change
// nothing, as is a value this fake cannot read as a 16-bit number. A bound
// nothing has written yet holds the kernel's default, NVME_CNTLID_MIN = 1 or
// NVME_CNTLID_MAX = 0xffef. Without this a converge that writes the two
// bounds in an order nvmet refuses passes here and fails on every real node.
func (f *fakeNode) cntlidRefusal(path, data string) error {
	attr := path[strings.LastIndex(path, "/")+1:]
	var other string
	var bound uint64
	switch attr {
	case "attr_cntlid_min":
		other, bound = "attr_cntlid_max", 0xffef
	case "attr_cntlid_max":
		other, bound = "attr_cntlid_min", 1
	default:
		return nil
	}
	refused := &fs.PathError{Op: "write", Path: path, Err: syscall.EINVAL}
	val, err := strconv.ParseUint(strings.TrimSpace(data), 10, 16)
	if err != nil || val == 0 {
		return refused
	}
	if cur, ok := f.files[parentDir(path)+"/"+other]; ok {
		if bound, err = strconv.ParseUint(
			strings.TrimSpace(cur), 10, 16); err != nil {
			return fmt.Errorf("fake: %s holds %q", other, cur)
		}
	}
	if (attr == "attr_cntlid_min" && val > bound) ||
		(attr == "attr_cntlid_max" && val < bound) {
		return refused
	}
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
// The last two are reached through fakeProbeIO, not the OsClient (osclient.md,
// Exported raw helpers and the probe-IO carve-out).
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

// A CANCELLED CONTEXT FAILS, the way the production client does, and the dn
// twin of this fake carries the reasoning: common/osclient.go runs commands
// through exec.CommandContext and tests ctx.Err() per file operation, so a
// converge whose context dies part-way stops there. A fake that ignores the
// context runs such a pass to completion and is blind by construction to a
// retry loop that cancels its own attempt — which is exactly the bug the dn
// agent had.
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
		line += " stdin=" + strings.Join(dmTargets(stdin), " | ")
	}

	f.mu.Lock()
	f.record("%s", line)
	if f.traceOf != nil {
		tid, _ := common.TraceIdFromCtx(ctx)
		f.traceOf[line] = tid
	}
	f.recordTrace(ctx, line)
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
	killedNth := takeKillNth(f.killCmdNth, line)
	killedNoEffect := takeKill(f.killCmdNoEffect, f.killCmdNoEffectAlways, line)
	var killed bool
	if !killedNoEffect {
		killed = takeKill(f.killCmd, f.killCmdAlways, line) || killedNth
	}
	var cancel context.CancelFunc
	for key, fn := range f.cancelCmd {
		if strings.Contains(line, key) {
			delete(f.cancelCmd, key)
			cancel = fn
			break
		}
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
	if f.stdouts == nil {
		f.stdouts = make(map[string][]string)
	}
	f.stdouts[name] = append(f.stdouts[name], stdout)
	if cancel != nil {
		cancel()
		return "", "signal: killed", -1, ctx.Err()
	}
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

// takeKillNth counts line against every counted kill hook it matches and
// reports whether it is the Nth match of one, consuming that key. Every
// matching key counts, so the answer never depends on map order.
func takeKillNth(counted map[string]int, line string) bool {
	fired := false
	for key, n := range counted {
		if !strings.Contains(line, key) {
			continue
		}
		if n <= 1 {
			delete(counted, key)
			fired = true
			continue
		}
		counted[key] = n - 1
	}
	return fired
}

// killedCmdResult is what common.OsClient.RunCommand returns for a child the
// SH15 soft timeout killed: no output, exit code -1 and a non-nil error —
// exactly the (exitCode, err) pair agent.Reported calls "did not answer",
// and the one thing a `dmsetup info` or an `mdadm --stop` must never be
// allowed to read as "the object is not there".
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
	if head := f.pendingHeads[path]; head != nil {
		head.reads++
		if head.reads >= head.after {
			f.addNsHead(path, head.nsDev, head.nsIdx)
			delete(f.pendingHeads, path)
		}
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

// lsDevNo renders one `dmsetup ls` device number in the spelling this node's
// dmsetup uses: "253:4" by default, "253, 4" with dmLsLegacyDevNo set. A
// name the listing carries but the kernel has already dropped has no device
// number at all and prints as the empty "()" — which Dm.List keeps, by name.
func (f *fakeNode) lsDevNo(name string) string {
	devNo := f.devNo["/dev/mapper/"+name]
	if !f.dmLsLegacyDevNo {
		return devNo
	}
	major, minor, ok := strings.Cut(devNo, ":")
	if !ok {
		return devNo
	}
	return major + ", " + minor
}

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
			fmt.Fprintf(&sb, "%s\t(%s)\n", name, f.lsDevNo(name))
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
// already marked the tds `created` (CN14).
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
// CN14's created-td rule observable: a created td is never re-created by
// message, so a pool
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
		// That is the failure mode of CN14's "A violated precondition is left
		// to dm-thin": the gateway's job is to keep
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
		// The thin-pool auto-grow (architecture.md, Automatic reactions)
		// parses the two used/total pairs out of this.
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
	case contains(args, "--stop"):
		// Both spellings are accepted: the named /dev/md/{name} symlink the
		// build path uses, and the /dev/mdN (or /dev/md_<name>) a sweep
		// stops — a sweep gets its arrays from /sys/block and never learns
		// the name.
		dev := args[len(args)-1]
		key, array := f.arrayByDev(dev)
		if array == nil {
			return "", 1
		}
		if holder := f.heldBy(key); holder != "" {
			f.dispatchStderr = "mdadm: Cannot get exclusive access to " +
				dev + ": held by " + holder
			return "", 1
		}
		// A successful stop is published in sysfs, not just in the fake's
		// own map: that is what lets a test kill `mdadm --stop` and still
		// see Md.Gone report true, which is the whole "killed but the kernel
		// completed it" case.
		f.unpublishArray(array)
		delete(f.arrays, key)
		delete(f.devNo, key)
		delete(f.devSize, key)
		return "", 0
	case contains(args, "--add"):
		return f.mdAdd(args)
	case contains(args, "--fail"):
		return f.mdFail(args)
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
	f.installArray(dev, &fakeArray{name: name, members: members})
	f.devNo[dev] = f.newDevNo()
	f.devSize[dev] = f.arraySize(members)
	f.publishArray(f.arrays[dev])
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
	array := &fakeArray{name: name, members: members}
	f.installArray(dev, array)
	f.devNo[dev] = f.newDevNo()
	f.devSize[dev] = f.arraySize(members)
	f.publishArray(array)
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

func (f *fakeNode) mdAdd(args []string) (string, int) {
	dev := args[0]
	_, array := f.arrayByDev(dev)
	if array == nil {
		return "", 1
	}
	member := args[len(args)-1]
	array.members = append(array.members, member)
	delete(array.memberState, member)
	delete(array.unbound, member)
	f.superblocks[member] = true
	f.publishArray(array)
	return "", 0
}

// mdFail marks a member faulty, as SET_DISK_FAULTY does: it stays held, and
// its dev-*/state keeps the failfast flag beside the new one.
func (f *fakeNode) mdFail(args []string) (string, int) {
	dev := args[0]
	_, array := f.arrayByDev(dev)
	if array == nil {
		return "", 1
	}
	member := args[len(args)-1]
	if !contains(array.members, member) {
		return "", 1
	}
	if array.memberState == nil {
		array.memberState = make(map[string]string)
	}
	array.memberState[member] = "faulty,failfast"
	f.publishArray(array)
	return "", 0
}

func (f *fakeNode) mdRemove(args []string) (string, int) {
	dev := args[0]
	_, array := f.arrayByDev(dev)
	if array == nil {
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
	delete(array.memberState, member)
	delete(array.unbound, member)
	f.publishArray(array)
	return "", 0
}

// ---------------------------------------------------------------------------
// The /sys/block view of md
//
// Md.ListArrays, Md.Gone and Md.Detail read arrays out of sysfs alone, because
// an mdadm probe opens a member device and one whose DN side is gone blocks
// until failfast — past the soft timeout. So the fake has to publish what the
// kernel publishes:
//
//	/sys/block/{node}                               the array node, mdN or md_<name>
//	/sys/block/{node}/md/array_state                clean / inactive / clear
//	/sys/block/{node}/md/degraded                   running arrays only
//	/sys/block/{node}/md/sync_action                running arrays only
//	/sys/block/{node}/md/sync_completed             running arrays only
//	/sys/block/{node}/md/dev-{kname}                one per member
//	/sys/block/{node}/md/dev-{kname}/state          "in_sync,failfast", …
//	/sys/block/{node}/md/dev-{kname}/block/dev      the member's major:minor
//	/sys/block/{node}/md/dev-{kname}/block/dm/name  the member's DM NAME
//
// The last one is absent for a member that is not a dm device, which is how
// an array of ours is told from a foreign one (MdArray.Foreign). The three
// "running arrays only" attributes are what the lab kernel really does: an
// inactive array has none of them, and a Detail that read them regardless
// would turn the "inactive" verdict into a read error.
//
// It all lives in the ordinary dirs/files maps, exactly like the configfs and
// nvme-subsystem trees, so `ls -1 /sys/block` picks the arrays up through
// children() and anything else a test puts under /sys/block keeps listing.
//
// "mdN" below stands for either spelling, as in md.go.
// ---------------------------------------------------------------------------

// arrayByDev resolves either spelling of an array node: the /dev/md/{name}
// symlink the fake keys arrays by, and the /dev/mdN sysfs publishes.
func (f *fakeNode) arrayByDev(dev string) (string, *fakeArray) {
	if array, ok := f.arrays[dev]; ok {
		return dev, array
	}
	for key, array := range f.arrays {
		if array.node != "" && dev == "/dev/"+array.node {
			return key, array
		}
	}
	return "", nil
}

// installArray puts an array at dev, giving it a kernel node name. An array
// replacing one at the same path inherits its node, so a re-create does not
// leak a /sys/block entry; the sysfs subtree of the old one is dropped.
func (f *fakeNode) installArray(dev string, array *fakeArray) {
	if old, ok := f.arrays[dev]; ok {
		f.unpublishArray(old)
		array.node = old.node
	}
	if array.node == "" {
		array.node = fmt.Sprintf("md%d", f.nextMdMinor)
		f.nextMdMinor++
	}
	if array.arrayState == "" {
		array.arrayState = "clean"
	}
	if array.syncAction == "" {
		array.syncAction = "idle"
	}
	if array.syncCompleted == "" {
		array.syncCompleted = "none"
	}
	f.arrays[dev] = array
}

// publishArray rewrites an array's whole /sys/block subtree from its current
// members, so a member add or remove shows up in the sysfs view too.
func (f *fakeNode) publishArray(array *fakeArray) {
	f.unpublishArray(array)
	mdDir := sysfsBlockDir + "/" + array.node + "/md"
	f.dirs[sysfsBlockDir] = true
	f.dirs[sysfsBlockDir+"/"+array.node] = true
	f.dirs[mdDir] = true
	f.files[mdDir+"/array_state"] = array.arrayState + "\n"
	if mdRunning(array.arrayState) {
		f.files[mdDir+"/degraded"] = strconv.Itoa(array.degraded) + "\n"
		f.files[mdDir+"/sync_action"] = array.syncAction + "\n"
		f.files[mdDir+"/sync_completed"] = array.syncCompleted + "\n"
	}
	for _, member := range array.members {
		devDir := mdDir + "/dev-" + f.kernelName(member)
		f.dirs[devDir] = true
		if array.unbound[member] {
			// No block link, and an attribute of the member's own reads
			// ENODEV (unbindMember).
			f.errFiles[devDir+"/state"] = &fs.PathError{
				Op: "read", Path: devDir + "/state", Err: syscall.ENODEV}
			continue
		}
		f.dirs[devDir+"/block"] = true
		f.files[devDir+"/state"] = f.memberStateOf(array, member) + "\n"
		if devNo := f.devNo[member]; devNo != "" {
			f.files[devDir+"/block/dev"] = devNo + "\n"
		}
		dmName, ok := strings.CutPrefix(member, "/dev/mapper/")
		if !ok {
			// Not a dm device: the whole dm/ directory is absent, which is
			// the ENOENT that makes the array foreign.
			continue
		}
		f.dirs[devDir+"/block/dm"] = true
		f.files[devDir+"/block/dm/name"] = dmName + "\n"
	}
}

// memberStateOf is a member's dev-*/state: the scripted one, else what the
// lab kernel shows for dnv's members — "in_sync,failfast" in a running array
// (every member is created or added with --failfast, and the flag lives in
// the superblock, so an assembly keeps it) and a bare "spare" in one that
// has not started.
func (f *fakeNode) memberStateOf(array *fakeArray, member string) string {
	if state, ok := array.memberState[member]; ok {
		return state
	}
	if mdRunning(array.arrayState) {
		return "in_sync,failfast"
	}
	return "spare"
}

func (f *fakeNode) unpublishArray(array *fakeArray) {
	if array.node == "" {
		return
	}
	root := sysfsBlockDir + "/" + array.node
	for entry := range f.dirs {
		if entry == root || strings.HasPrefix(entry, root+"/") {
			delete(f.dirs, entry)
		}
	}
	for entry := range f.files {
		if strings.HasPrefix(entry, root+"/") {
			delete(f.files, entry)
		}
	}
	for entry := range f.errFiles {
		if strings.HasPrefix(entry, root+"/") {
			delete(f.errFiles, entry)
		}
	}
}

// kernelName is the /sys/class/block name of a member device: dm-N for a
// /dev/mapper path — the minor of its device number, exactly as the kernel
// numbers them — and the bare basename for anything else.
func (f *fakeNode) kernelName(path string) string {
	if strings.HasPrefix(path, "/dev/mapper/") {
		if _, minor, ok := strings.Cut(f.devNo[path], ":"); ok {
			return "dm-" + minor
		}
	}
	return path[strings.LastIndexByte(path, '/')+1:]
}

// mdNode is the /dev/mdN a sweep sees for the array created at dev, or "".
func (f *fakeNode) mdNode(dev string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return ""
	}
	return "/dev/" + array.node
}

// setArrayState scripts /sys/block/{node}/md/array_state — "inactive" for an
// assembled-but-not-running array, which Md.Gone must NOT accept as gone —
// and republishes the subtree, so the running-only attributes come and go
// with it.
func (f *fakeNode) setArrayState(dev, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return
	}
	array.arrayState = state
	f.publishArray(array)
}

// setMdSync scripts md/degraded, md/sync_action and md/sync_completed.
func (f *fakeNode) setMdSync(
	dev string, degraded int, action, completed string,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return
	}
	array.degraded = degraded
	array.syncAction = action
	array.syncCompleted = completed
	f.publishArray(array)
}

// setMemberState scripts one member's dev-*/state, verbatim.
func (f *fakeNode) setMemberState(dev, member, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return
	}
	if array.memberState == nil {
		array.memberState = make(map[string]string)
	}
	array.memberState[member] = state
	f.publishArray(array)
}

// unbindMember models the instant md has unbound one member of an array
// another cntlr is stopping (or removing the member from): md clears the
// member's array pointer and removes its `block` link, so dev-*/block/dev and
// dev-*/block/dm/name read ENOENT, while the dev-* directory stays until md
// deletes it at its next unlock and every attribute of the member's own —
// dev-*/state among them — reads ENODEV meanwhile (a read error that is not
// ENOENT). The fake holds that instant: a later republish (setMdSync,
// setMemberState, mdFail, …) keeps the member unbound, and only an mdadm
// --add or --remove of the member, or a stop, ends it.
func (f *fakeNode) unbindMember(dev, member string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return
	}
	if array.unbound == nil {
		array.unbound = make(map[string]bool)
	}
	array.unbound[member] = true
	f.publishArray(array)
}

// dropMdDir removes an array's /sys/block/{node}/md subtree and keeps
// /sys/block/{node}: a stopping array's md/ goes before its node does (the
// kernel deletes the md kobject, then the gendisk), so a walk can list the
// node and find no md/ in it.
func (f *fakeNode) dropMdDir(dev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return
	}
	root := sysfsBlockDir + "/" + array.node + "/md"
	for entry := range f.dirs {
		if entry == root || strings.HasPrefix(entry, root+"/") {
			delete(f.dirs, entry)
		}
	}
	for entry := range f.files {
		if strings.HasPrefix(entry, root+"/") {
			delete(f.files, entry)
		}
	}
	for entry := range f.errFiles {
		if strings.HasPrefix(entry, root+"/") {
			delete(f.errFiles, entry)
		}
	}
}

// arrayGone is the assertion side of a stop: neither mdadm nor sysfs still
// knows the array. An array whose /sys/block entry survived is NOT gone —
// it still pins its members against a `dmsetup remove`.
func (f *fakeNode) arrayGone(dev string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return true
	}
	return !f.dirs[sysfsBlockDir+"/"+array.node]
}

// seedArray installs an array no mdadm run of this test created — an array
// left behind by a previous incarnation, or a foreign one whose members are
// not dm devices at all (`seedArray("/dev/md/other", "other", "/dev/sdb")`).
// Unlike mdCreate it stamps no superblocks: a foreign array's members are not
// ours to claim. A member the node has no device number for gets one, since
// every block device the kernel holds in an array has a dev-*/block/dev.
func (f *fakeNode) seedArray(dev, name string, members ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, member := range members {
		if _, ok := f.devNo[member]; !ok {
			f.devNo[member] = f.newDevNo()
		}
	}
	array := &fakeArray{name: name, members: members}
	f.installArray(dev, array)
	if _, ok := f.devNo[dev]; !ok {
		f.devNo[dev] = f.newDevNo()
	}
	f.devSize[dev] = f.arraySize(members)
	f.publishArray(array)
}

// ---------------------------------------------------------------------------
// tmpfs / loop
// ---------------------------------------------------------------------------

// cmdMount mounts a fresh tmpfs at the path. Over a path that already carries
// one, the new mount STACKS, as the kernel's does: the old tmpfs stays
// mounted underneath with everything on it, and the path now shows an empty
// filesystem. So a file under the path is gone from `stat`, and `losetup
// --associated` stops listing the loop device it backs — losetup matches a
// loop by the backing file's inode, and a file created there afterwards is a
// new one — while the loop device itself stays. That is what a converge that
// read a killed `findmnt` as "nothing mounted" does to the live arena (CN5),
// and it is modelled so that such a regression shows what follows on a node:
// a fresh `truncate` and a second loop device.
func (f *fakeNode) cmdMount(args []string) (string, int) {
	path := args[len(args)-1]
	if _, stacked := f.mounts[path]; stacked {
		for file := range f.plain {
			if strings.HasPrefix(file, path+"/") {
				delete(f.plain, file)
				delete(f.loops, file)
			}
		}
	}
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
	if contains(args, "%u %a %F") {
		if answer, ok := f.dirStat[path]; ok {
			return answer + "\n", 0
		}
		if f.dirs[path] {
			return fmt.Sprintf("%d 700 directory\n", os.Geteuid()), 0
		}
		return "", 1
	}
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

// cmdThinDump answers with the scripted document, or a synthesized one. With
// `-o FILE` the document goes to that file and nothing to stdout, as
// thin-provisioning-tools writes it.
func (f *fakeNode) cmdThinDump(args []string) (string, int) {
	var path, out string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o" && i+1 < len(args):
			i++
			out = args[i]
		case !strings.HasPrefix(args[i], "-"):
			path = args[i]
		}
	}
	dump := f.thinDumpDoc(path)
	if out != "" {
		f.files[out] = dump
		return "", 0
	}
	return dump, 0
}

func (f *fakeNode) thinDumpDoc(path string) string {
	if dump, ok := f.thinDumps[path]; ok {
		return dump
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
	return sb.String()
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
	// A connect to an endpoint the host already holds a controller for is
	// refused before it reaches the network: nvme-cli's own "already
	// connected" check, and behind it the kernel's
	// nvme_tcp_existing_controller, which answers EALREADY — the agent never
	// passes --duplicate-connect. A controller being deleted, or dead, no
	// longer counts (nvmf_ctlr_matches_baseopts).
	if held := f.subsystems[nqn]; held != nil {
		for _, ctrl := range held.ctrls {
			if ctrl.trAddr == trAddr && ctrl.trSvcId == trSvcId &&
				ctrl.state != "deleting" &&
				ctrl.state != "deleting (no IO)" && ctrl.state != "dead" {
				f.dispatchStderr =
					"could not add new controller: already connected"
				return "", 1
			}
		}
	}
	key := connectKey(nqn, trAddr, trSvcId)
	if took := f.connectTakes[key]; took > 0 && f.advance != nil {
		f.advance(took)
	}
	if left := f.connectFail[key]; left > 0 {
		f.connectFail[key] = left - 1
		f.dispatchStderr = "could not add new controller: connection refused"
		return "", 1
	}
	// nvme-core registers both classes when it loads, which a fabrics
	// connect needs: a host that never loaded it has neither directory, and
	// the first connect finds them there.
	f.dirs[sysfsNvmeSubsysDir] = true
	f.dirs[sysfsNvmeCtrlDir] = true
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
		if after := f.nsHeadAfter[nqn]; after > 1 {
			f.pendingHeads[dir] = &pendingHead{
				after: after, nsDev: nsDev, nsIdx: nsIdx}
		} else {
			f.addNsHead(dir, nsDev, nsIdx)
		}
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
	if during := f.duringConnect[key]; during != nil {
		during()
	}
	return "", 0
}

// addNsHead publishes a subsystem's multipath namespace head: the nvmeXnY
// directory with its nsid, and the block device behind it.
func (f *fakeNode) addNsHead(dir, nsDev string, nsIdx uint32) {
	f.dirs[dir+"/"+nsDev] = true
	f.files[dir+"/"+nsDev+"/nsid"] = fmt.Sprintf("%d\n", nsIdx)
	f.devNo["/dev/"+nsDev] = f.newDevNo()
	f.devSize["/dev/"+nsDev] = 1 << 40
}

// dropNsHead takes a connected subsystem's namespace head away and leaves its
// controllers: the shape of a subsystem whose only path is ANA inaccessible,
// which never gets a head at all.
func (f *fakeNode) dropNsHead(nqn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	subsys, ok := f.subsystems[nqn]
	if !ok {
		return
	}
	dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, subsys.idx)
	for entry := range f.dirs {
		name, ok := childOf(dir, entry)
		if ok && nvmeNsEntryPattern.MatchString(name) {
			delete(f.dirs, entry)
			delete(f.files, entry+"/nsid")
			delete(f.devNo, "/dev/"+name)
			delete(f.devSize, "/dev/"+name)
		}
	}
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
	delete(f.pendingHeads, subsysDir)
	nsDev := fmt.Sprintf("/dev/nvme%dn1", subsys.idx)
	delete(f.devNo, nsDev)
	delete(f.devSize, nsDev)
	delete(f.subsystems, subsys.nqn)
}

// deleteCtrlDevice deletes the device of one path's controller and leaves
// its subsystem's link to it: the kernel's window between deleting a
// controller's device (nvme_uninit_ctrl) and dropping its last reference
// (nvme_free_ctrl, which removes the link). Every read under
// /sys/class/nvme/{ctrl} answers ENOENT from then on, while the subsystem
// directory still lists {ctrl}. The controller leaves subsys.ctrls, so a
// connect of its endpoint is no duplicate — the kernel's check passes over a
// controller being deleted. It returns the controller's name.
func (f *fakeNode) deleteCtrlDevice(nqn, trAddr, trSvcId string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	subsys, ok := f.subsystems[nqn]
	if !ok {
		return ""
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.trAddr != trAddr || ctrl.trSvcId != trSvcId {
			continue
		}
		ctrlDir := sysfsNvmeCtrlDir + "/" + ctrl.name
		for entry := range f.dirs {
			if entry == ctrlDir || strings.HasPrefix(entry, ctrlDir+"/") {
				delete(f.dirs, entry)
			}
		}
		for entry := range f.files {
			if strings.HasPrefix(entry, ctrlDir+"/") {
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
		return ctrl.name
	}
	return ""
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

// setCtrlState re-stamps a path's controller state and leaves its ana_state
// alone, which is how a dead side looks to the host: the controller goes
// resetting, then connecting, and ana_state keeps its last-known value for
// the whole outage.
func (f *fakeNode) setCtrlState(nqn, trAddr, trSvcId, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	subsys, ok := f.subsystems[nqn]
	if !ok {
		return
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.trAddr != trAddr || ctrl.trSvcId != trSvcId {
			continue
		}
		ctrl.state = state
		f.files[sysfsNvmeCtrlDir+"/"+ctrl.name+"/state"] = state + "\n"
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
