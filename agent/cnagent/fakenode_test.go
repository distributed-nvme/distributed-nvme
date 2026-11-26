package cnagent

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// fakeNode is an in-memory model of the slice of the OS the cn agent drives:
// device-mapper, md, the nvmet configfs tree,
// the sysfs nvme-subsystem tree the leg probe walks, thin-provisioning-tools
// and the local store. It backs common.FakeOsClient, records every call, and
// lets tests assert command sequences, probe-first idempotency and teardown
// order without root or real devices.
type fakeNode struct {
	mu sync.Mutex

	// loopCnt is how many loop devices cmdLosetup has made: loop<n> is
	// numbered 7:<n>. loopWc is the write cache each new one reads, "write
	// back" when empty.
	loopCnt int
	loopWc  string
	// targetWc is what the NVMe target of a leg's path reports for the
	// side's namespace now, "write back" or "write through", keyed
	// "{nqn}|{traddr}:{trsvcid}" as anaOf is. A path with an entry gets the
	// kernel's write cache rules: a connect and a rescan of its controller
	// copy the entry into the path device's queue/write_cache, the
	// subsystem's head reads "write back" once any path did, an identify
	// answers the entry (nvmeNsWriteCache), and a dm device takes the write
	// cache of the devices its table names when the table goes live
	// (stackWc). A path with none has no write_cache file, as before.
	targetWc map[string]string

	calls []string
	// sysfsNoDeadline records every /sys read or listing that arrived on a
	// ctx carrying no deadline. The NVMe walk's and the md walk's reads and
	// listings carry the SH15 soft timeout, which the shared code they run
	// through takes from cmdCtx (agent/oswrap.go), so this must stay empty.
	sysfsNoDeadline []string
	// traced is every command, file read, listing and mtime read keyed by
	// the trace id of the ctx it ran on: the trace_id its `os command` / `os
	// read file` / `os read dir` / `os read mtime` record carries in
	// production (log.md R5); "" for a ctx without one.
	traced map[string][]string

	// block devices, by /dev path. The number of an md array and an NVMe
	// multipath head is published in sysfs too, at
	// /sys/block/<kname>/dev, which the agent reads it from (blockDevNo):
	// publishBlockDev and dropBlockDev keep the two in step, and an md
	// array's comes from its own fakeArray.devNo (publishArray).
	devSize   map[string]uint64
	devNo     map[string]string
	nextMinor int
	// devNoPaths and dmOfDevNo are reverse indexes of devNo: from a device
	// number to a path, and to a dm device, that held it when the index was
	// last built or the fake last numbered a device. They are hints only: a
	// hit is checked against devNo and dms, and a miss rebuilds the index,
	// so a test that writes devNo straight gets the answer a scan of devNo
	// gives. They keep the table checks of a converge that builds tens of
	// thousands of devices from going quadratic.
	devNoPaths map[string]string
	dmOfDevNo  map[string]string

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
	// heldRoots is the held metadata root a thin-pool's metadata keeps, by
	// the pool's dm name, and like thinPools it outlives the dm device:
	// dm-thin writes a reserve_metadata_snap's root into the metadata
	// superblock, so a pool created again over the same metadata — on a new
	// primary, or after a reboot — comes up holding it (cnagent.md CN13).
	// A create takes it into the device's heldRoot; a reserve and a release
	// change both (holdMetaSnap).
	heldRoots map[string]bool
	// thinMapped is the blocks each thin id maps in a pool's metadata, by
	// the pool's dm name and id, outliving the dm device as thinPools does:
	// the synthesized `thin_dump` document's mapped_blocks, which CN14's
	// header read reads (holdThinData), and what a thin volume's status
	// reads mapped (thinStatus, mapThinBlocks). An id it lacks maps no
	// block, and a delete message drops the id's entry.
	thinMapped map[string]map[uint32]uint64
	// thinMapLo is the first block each thin id maps, by pool and id, 0 for
	// an id it lacks: the fake maps an id's blocks as one run, from that
	// block on, thinMapped of them (mapThinRun).
	thinMapLo map[string]map[uint32]uint64
	// emptyDiscards counts the DiscardRanges calls that carried no range,
	// each of which still opens its device for writing, and so makes udev
	// probe it at the close (discardRanges).
	emptyDiscards int
	// lsGhosts are names `dmsetup ls` reports that no longer exist. The
	// listing is an inherently stale snapshot — another cntlr's sweep
	// (CN21), at SP_LEVEL_DISABLE or otherwise, can remove a device between
	// the `ls` and the `dmsetup table` of that one name — and this is how
	// the suite reproduces that window deterministically. The whole-node
	// `dmsetup info` listing names them too, with no numbers, and the
	// `dmsetup table --concise` listing does not: a device there at the
	// reading's info listing and gone by its table listing.
	lsGhosts []string
	// infoOmit and tableOmit are names the whole-node `dmsetup info` and
	// `dmsetup table --concise` listings leave out while the devices stay:
	// what dmsetup prints when a device goes away during the listing, with
	// the device back, or another cntlr's, by the time the agent reads it on
	// its own. A listing that left a name out exits 1 with the stderr the
	// lab's dmsetup writes for a device gone mid-listing.
	infoOmit  map[string]bool
	tableOmit map[string]bool
	// dmLsLegacyDevNo switches `dmsetup ls` to the "(253, 4)" device number
	// spelling. Dm.List normalizes both into "253:4"; the lab kernels print
	// the colon form, so this is the only way the second branch of that
	// normalization is ever exercised.
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
	// memberArray is the array identity each member's md superblock names
	// (fakeArray.uuid): mdCreate writes it on the members it creates the
	// array over, an --add on the member it adds, and an assembly takes it
	// from the members it starts. A member whose superblock a test stamped
	// by hand has none until an assembly gives it one.
	memberArray map[string]string
	// nextMdUuid numbers the array identities mdCreate and an assembly mint.
	nextMdUuid int
	// assembleDrop models mdadm leaving a member out of an assembly for
	// stale metadata (cnagent.md CN12, the stale-metadata re-add of case 2):
	// the array starts without it and the agent has to re-add it.
	assembleDrop map[string]bool
	// sbFacts are the superblock facts the human-readable `mdadm --examine`
	// prints of a member that carries one (superblocks): its role, its
	// recovery offset and its events count, scripted (setSuperblock). A
	// member with no entry reads a full member at events zero. A zero of the
	// superblock drops them with it.
	sbFacts map[string]fakeSuperblock
	// bblRefused are the members whose bad-block log the kernel refuses to
	// import, as a stop of a broken array the member came back to can leave
	// it (cnagent_integtest.md, Lab facts). A plain assembly, with no
	// --run, counts each such add as refused against its gate
	// (fakeAssemblyStarts) and starts without the member, or refuses with
	// the lab mdadm's "assembled from" words. A forced assembly that names
	// one writes its superblock update first, as mdadm does — the new
	// identity of an --update=uuid among them — and then fails with the lab
	// mdadm's "failed to add"; one with --update=force-no-bbl drops the log,
	// and the entry with it, and starts. With refusedLeaves the failed forced
	// run leaves an inactive array at its node that holds the member and
	// refusedLeavesAlso beside it, as an array the stock incremental rule
	// assembled does: a device there that is no dm device makes that array
	// foreign.
	bblRefused map[string]bool
	// zeroNoEffect are the members whose `mdadm --zero-superblock` answers
	// success with the superblock left in place, as a write that never
	// reached the leg leaves it: only the agent's own read back tells
	// (cnagent.md CN12, the forced start).
	zeroNoEffect  map[string]bool
	refusedLeaves bool
	// cutShortAssemblies is how many of the next plain assemblies are
	// stopped once md took their first member, as their time limit
	// (MdBuildTimeout) stops a slow one: each leaves an inactive array
	// holding that member and fails.
	cutShortAssemblies int
	// cmdLimits is, per mdadm action a call named (--create, --assemble,
	// --stop, and --force for a forced assembly), the largest time its
	// context left it when it began: the bound the agent ran it under.
	cmdLimits         map[string]time.Duration
	refusedLeavesAlso []string
	// stopDeleting makes a successful `mdadm --stop` leave the array's
	// /sys/block entry and its md/ directory, with no member in it and an
	// array_state that reads EBUSY, until the first read of that
	// array_state has its answer: the window from md marking a stopped
	// array deleted until its md/ goes (cnagent.md CN12), which the stop's
	// own verify then meets (leaveDeleted).
	stopDeleting bool
	// cloneHydrated is the hydrated region count dm-clone's metadata holds,
	// by the storage of the metadata device (cloneMetaKey): an md array's
	// identity, or a dm device's name. It lives with the storage, not with a
	// dm-clone, so a dm-clone created again over the same group reads the
	// count the metadata held, as the kernel's loads it, and a change of the
	// array's members — an --add, a --remove, an assembly that leaves a
	// member out — keeps it, as md copies the array's blocks to the member
	// it adds; an md --create over the members mints a new identity and so
	// starts it again at zero, as dm-clone formats fresh metadata over a
	// group whose first data block reads zero. A test moves it
	// (setCloneHydrated).
	cloneHydrated map[string]uint64

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
	// dialLost and dialRefused model a disk node's address at the dial
	// before a connect (NvmeHost.Dial), keyed by the dialled "host:port":
	// an address in dialLost answers nothing, so its dial moves the clock
	// (advance) on by its whole timeout and fails wrapping
	// common.ErrDialTimeout, as a machine that is down does; dialRefused
	// refuses the first N dials of an address and then lets them through,
	// as a port that does not listen yet does. Every dial is recorded as
	// "dial <address>".
	dialLost    map[string]bool
	dialRefused map[string]int
	// duringConnect runs, under the fake's lock, when a connect to one
	// endpoint (connectKey) succeeds: what else changes on the node while
	// that connect runs — a migrating leg's src controller whose `address`
	// stops answering, say, which the pass's re-read after the connect then
	// meets. It must not take the lock.
	duringConnect map[string]func()
	// nsHeadAt stages a subsystem's multipath namespace head, keyed by NQN:
	// the connect that creates the subsystem adds its controller but not
	// the head, whose steps show on the listings of the subsystem directory
	// headStages names — the kernel's namespace scan, which `nvme connect`
	// only queues, and which adds the head in steps. Without an entry, or
	// with every step at 0 or 1, the connect adds the whole head at once.
	nsHeadAt map[string]headStages
	// pendingHeads are the heads nsHeadAt staged, keyed by the subsystem's
	// sysfs directory, until their last step shows.
	pendingHeads map[string]*pendingHead
	// afterList and afterRead change the node while a pass reads it: each
	// runs once, keyed by the exact path, right after a listing (readDir)
	// or a file read (readFile) of that path has its answer, which the
	// change does not alter — a subsystem dropped, and its freed number
	// given to another one (plantSubsys), between a walk's read of an entry
	// and a lookup of it, say. They run under the fake's lock and must not
	// take it.
	afterList map[string]func()
	afterRead map[string]func()

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
	// real kernel would leave it after a refused ioctl. These and the kill
	// hooks below also match a listing in the agent's own process by its
	// line, "list <path>", and an mtime read by its line, "mtime <path>",
	// which neither half answers as absent (inProcessHookErr).
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
	// failWriteAlways is the WriteFileDirect counterpart of failCmdAlways,
	// matched on a substring of "path=data": the write is recorded, refused
	// with the given error text, and changes nothing — a configfs store
	// handler that answered no.
	failWriteAlways map[string]string
	// gate blocks a command until the channel is closed (lock tests). It, and
	// hardGate and cancelCmd below, also match an mtime read by its line
	// (readMtime); hardGate also matches a superblock read by its line
	// (readSuperblockArea).
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
	// mtimes scripts the time readMtime answers per path: the age the
	// node-level sweep judges a host-facing subsystem with no namespace
	// device by (cnagent.md CN21). A path without one answers fakeDirMtime,
	// long past.
	mtimes map[string]time.Time
	// stampAttrs, when set, makes a read of an nvmet subsystem's own
	// attr_* file set that subsystem directory's time to its answer, as
	// kernel 7.0 stamps the directory on every lookup of one of its
	// attribute files (Nvmet.SubsysMtime).
	stampAttrs func() time.Time

	// nvmetDebug models nvmet's debug directory (agent.NvmetDebugRoot), the
	// target side of the node, kept apart from the NVMe host model above:
	// while set, the directory is there, a configfs subsystem's mkdir makes
	// the subsystem's debug directory and its rmdir drops it with every
	// nvmet connection in it, as the kernel makes and frees them. A test
	// sets it with withNvmetDebug and plants connections with connectNvmet;
	// a connection is its directory ctrl<cntlid> with "hostnqn" (no newline
	// at its end) and "state".
	nvmetDebug bool
	// connEnds says, by a connection's directory, how many listings of its
	// subsystem's debug directory still show it after a write of "fatal"
	// to its "state" (writeConnState), connEndsNever for one that never
	// goes; without an entry it goes at the write. connEnding counts those
	// listings down.
	connEnds   map[string]int
	connEnding map[string]int
	// connReuse names, by a connection's directory, the host whose new
	// connection takes the same name the moment the old one goes, as the
	// kernel gives a freed cntlid again at once.
	connReuse map[string]string
	// connEIO holds files of nvmet connections whose removal began after
	// their open: the next read or write of one answers EIO, as debugfs's
	// proxy does, and the connection's directory goes with that answer.
	connEIO map[string]bool
}

// connEndsNever is the connEnds entry of a connection a write of "fatal"
// never ends.
const connEndsNever = -1

type fakeDm struct {
	table string
	// readOnly is the live table's mode, which a load with --readonly sets
	// and a resume onto a read-write table clears; bdevReadOnly is the block
	// layer's own read-only flag of the device, which `blockdev --setro`
	// sets and only `blockdev --setrw` clears, a reload and a resume leaving
	// it as it is. dmsetup's attr column reads the device read-only while
	// either is set, as kernel 7.0.0-42 does.
	readOnly     bool
	bdevReadOnly bool
	suspended    bool
	// waiting is the table a `dmsetup reload` loaded and no resume has made
	// live yet, nil when none waits: dmsetup's inactive table. A resume
	// makes it live, a `dmsetup clear` drops it, and the attr column shows
	// it in its second place.
	waiting *fakeTable
	// dm-clone bookkeeping
	noHydration       bool
	noDiscardPassdown bool
	threshold         uint32
	batchSize         uint32
	discards          []string
	// metaMode is the metadata mode a dm-clone's status ends with: "" reads
	// "rw", common.CloneMetadataReadOnly "ro", and common.CloneMetadataFail
	// prints "Fail" alone, as the kernel does in fail mode.
	metaMode string
	// thin-pool bookkeeping
	thinIds  map[uint32]bool
	snapshot bool
	// heldRoot models a reserved metadata snapshot.
	heldRoot bool
	// poolMetaBlocks is a thin-pool's total metadata blocks, as dm-thin
	// takes them from its metadata device at the create and at each
	// resume, and never otherwise (poolTakeMeta). poolNeedsCheck is the
	// needs_check flag, under which a resume grows nothing.
	poolMetaBlocks uint64
	poolNeedsCheck bool
	// poolMode is the mode word of the pool's status line, "rw" when
	// empty and "ro" under poolNeedsCheck; poolFault, when set, is the word
	// the line carries in place of its numbers ("Fail", "Error").
	poolMode  string
	poolFault string
}

type fakeArray struct {
	name    string // the mdadm --name value
	members []string
	// uuid is the array's identity, which md writes into every member's
	// superblock (fakeNode.memberArray). An array a test seeds or installs
	// by hand has none.
	uuid string
	// devNo is the array's number, which /sys/block/{node}/dev publishes:
	// the one mdadm's create or assembly gave it, set apart from
	// fakeNode.devNo of its /dev/md/{name} link, so a test can point that
	// link elsewhere and keep the array's own number.
	devNo string
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
	// lastSyncAction is md/last_sync_action, published with the three
	// above: a write of "check" or "idle" to md/sync_action sets it, as
	// md's action_store does, and a test may script it.
	lastSyncAction string
	// syncMin is md/sync_min, "0" when empty; a write to it sets it.
	syncMin string
	// badBlocks scripts dev-*/bad_blocks per member path, the list as md
	// prints it, one "<sector> <length>" line per range; a member without
	// an entry publishes an empty list. Published for a running array only,
	// with the same ranges in dev-*/unacknowledged_bad_blocks, as md lists
	// a range there too until it writes the range to the superblock.
	badBlocks map[string]string
	// memberState scripts dev-*/state per member path; a member without an
	// entry reads what the kernel shows for dnv's members (memberStateOf).
	memberState map[string]string
	// memberSlot scripts dev-*/slot per member path; a member without an
	// entry reads its place in members (memberSlotOf). mdFail moves a member
	// out of its slot, as md does once no IO to it is pending, and an mdadm
	// --add or --remove of the member ends the entry.
	memberSlot map[string]string
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

// headStages are the listings of a subsystem's directory, counted from the
// first one after the connect that created it, on which each step of adding
// its multipath namespace head shows, in the kernel's order: the head's
// directory, its nsid, its /sys/block entry with the number behind it, and
// its ng device last, which the kernel adds once the head can be opened by
// its number. A step shows no earlier than the one before it; 0 and 1 are
// the first listing.
type headStages struct {
	dir, nsid, block, ng int
}

// steps is the four counts in the kernel's order.
func (h headStages) steps() []int {
	return []int{h.dir, h.nsid, h.block, h.ng}
}

// pendingHead is one namespace head the kernel's scan has not finished adding.
type pendingHead struct {
	at    headStages
	reads int // listings so far
	shown int // steps shown so far, in the kernel's order
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
	// hostNqn is the host NQN the controller was connected with, which
	// its sysfs hostnqn shows; "" for a planted one, which publishes none.
	hostNqn string
}

func newFakeNode() *fakeNode {
	f := &fakeNode{
		devSize:       make(map[string]uint64),
		devNo:         make(map[string]string),
		nextMinor:     1,
		dms:           make(map[string]*fakeDm),
		thinPools:     make(map[string]map[uint32]bool),
		heldRoots:     make(map[string]bool),
		thinMapped:    make(map[string]map[uint32]uint64),
		thinMapLo:     make(map[string]map[uint32]uint64),
		infoOmit:      make(map[string]bool),
		tableOmit:     make(map[string]bool),
		arrays:        make(map[string]*fakeArray),
		superblocks:   make(map[string]bool),
		memberArray:   make(map[string]string),
		assembleDrop:  make(map[string]bool),
		sbFacts:       make(map[string]fakeSuperblock),
		bblRefused:    make(map[string]bool),
		zeroNoEffect:  make(map[string]bool),
		cloneHydrated: make(map[string]uint64),
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
		dialLost:      make(map[string]bool),
		dialRefused:   make(map[string]int),
		duringConnect: make(map[string]func()),
		nsHeadAt:      make(map[string]headStages),
		pendingHeads:  make(map[string]*pendingHead),
		afterList:     make(map[string]func()),
		afterRead:     make(map[string]func()),
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
		failWriteAlways:       make(map[string]string),

		gate:      make(map[string]chan struct{}),
		hardGate:  make(map[string]chan struct{}),
		cancelCmd: make(map[string]context.CancelFunc),
		dirStat:   make(map[string]string),
		mtimes:    make(map[string]time.Time),

		connEnds:   make(map[string]int),
		connEnding: make(map[string]int),
		connReuse:  make(map[string]string),
		connEIO:    make(map[string]bool),
	}
	f.dirs[sysfsNvmeSubsysDir] = true
	f.dirs[sysfsNvmeCtrlDir] = true
	// /sys/block exists on every node, empty or not: a listing of it that
	// does not answer is an error to Md.ListArrays, never "this node runs no
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
// unwired: under the probe-IO carve-out (osclient.md, Exported raw helpers
// and the probe-IO carve-out) the CN11 probe does not use the OsClient but
// the raw WriteBlockAt/ReadBlockDirectAt
// helpers, faked through the LegProbeIO double below, and the interface has
// no O_DIRECT read to wire at all.
// ReadBlockFn stays connected so that a buffered read — which the probe must
// never issue — is still recorded rather than silently succeeding.
func (f *fakeNode) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn:       f.runCommand,
		ReadFileFn:         f.readFile,
		ReadDirFn:          f.readDir,
		ReadMtimeFn:        f.readMtime,
		ReadDevNoFn:        f.readDevNo,
		WriteFileFn:        f.writeFile,
		WriteFileDirectFn:  f.writeFileDirect,
		ReadBlockFn:        f.readBlock,
		ReadBlockDirectFn:  f.readSuperblockArea,
		DiscardRangesFn:    f.discardRanges,
		NvmeNsWriteCacheFn: f.nvmeNsWriteCache,
		DialTcpFn:          f.dialTcp,
		ReadProtoFn:        f.readProto,
		WriteProtoFn:       f.writeProto,
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

// SysfsNoDeadline is the SH15 evidence: the sysfs paths read or listed on a
// ctx with no SH15 deadline. Assertions live in cnagent_test.go, so that the
// fake never touches *testing.T from the prober goroutines calling into it.
func (f *fakeNode) SysfsNoDeadline() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sysfsNoDeadline...)
}

// readOnlyPrefixes are the probes; everything else changes the system.
var readOnlyPrefixes = []string{
	"list ", "mtime ", "cmd lsblk ", "cmd stat ",
	"cmd dmsetup info", "cmd dmsetup table", "cmd dmsetup status",
	"cmd dmsetup ls", "cmd nvme list-subsys",
	"cmd mdadm --detail", "readsb ",
	"cmd thin_dump ", "read ", "readproto ", "readblock ", "readblockdirect ",
	"devno ", "cmd losetup -j ", "identify ",
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
	data, err := f.fileAnswer(path)
	if f.stampAttrs != nil {
		if rest, ok := strings.CutPrefix(
			path, agent.NvmetRoot+"/subsystems/"); ok {
			if nqn, attr, ok := strings.Cut(rest, "/"); ok &&
				strings.HasPrefix(attr, "attr_") {
				f.mtimes[agent.NvmetRoot+"/subsystems/"+nqn] = f.stampAttrs()
			}
		}
	}
	if after := f.afterRead[path]; after != nil {
		delete(f.afterRead, path)
		after()
	}
	return data, err
}

// readDevNo is the device-number read in the agent's own process
// (OsClient.ReadDevNo), recorded as "devno <path>": the number devNo holds
// for the path, or not there.
func (f *fakeNode) readDevNo(ctx context.Context, path string) (string, error) {
	if err := f.ctxErr(ctx); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("devno %s", path)
	f.recordTrace(ctx, "devno "+path)
	devNo, ok := f.devNo[path]
	if !ok || devNo == "" {
		return "", fmt.Errorf("stat %s: %w", path, fs.ErrNotExist)
	}
	return devNo, nil
}

// fileAnswer is what a read of path answers: a hook's failure, an attribute
// that exists and fails, its contents, or not there. Called with f.mu held.
func (f *fakeNode) fileAnswer(path string) (string, error) {
	if err := f.readHookErr(path); err != nil {
		return "", err
	}
	if err := f.connRemoved(path); err != nil {
		return "", err
	}
	if err, ok := f.errFiles[path]; ok {
		return "", err
	}
	if uuid, ok := f.mdUuidAnswer(path); ok {
		return uuid, nil
	}
	data, ok := f.files[path]
	if !ok && strings.HasPrefix(path, "/sys/dev/block/") &&
		strings.HasSuffix(path, "/inflight") {
		// A device's in-flight IO (Dm.InFlight): none, unless a test
		// plants a count.
		return "       0        0\n", nil
	}
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

// readDir answers OsClient.ReadDir: a listing in the agent's own process,
// of sysfs, of the local store or of the nvmet configfs tree (dnagent.md
// SH18), from the directory model. It records "list <path>", and the command
// hooks match that line as they match a command line, so a key "list "+X
// catches every listing of a path that starts with X. A fail hook answers an
// I/O error part-way through the listing and a kill hook a listing the soft
// timeout cut off: both are errors, never a missing directory, which only the
// model says. A hooked listing advances no pending head.
func (f *fakeNode) readDir(ctx context.Context, path string) ([]string, error) {
	if err := f.ctxErr(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	line := "list " + path
	f.record("%s", line)
	if f.traceOf != nil {
		tid, _ := common.TraceIdFromCtx(ctx)
		f.traceOf[line] = tid
	}
	f.recordTrace(ctx, line)
	if _, ok := ctx.Deadline(); !ok && strings.HasPrefix(path, "/sys/") {
		// SH15, as for readFile.
		f.sysfsNoDeadline = append(f.sysfsNoDeadline, path)
	}
	names, err := f.dirAnswer(line, path)
	if after := f.afterList[path]; after != nil {
		delete(f.afterList, path)
		after()
	}
	return names, err
}

// dirAnswer is what the listing line of path answers: a hook's failure, the
// directory's entries, or not there. Called with f.mu held.
func (f *fakeNode) dirAnswer(line, path string) ([]string, error) {
	if err := f.inProcessHookErr(line, &fs.PathError{
		Op: "readdirent", Path: path, Err: syscall.EIO}); err != nil {
		return nil, err
	}
	if rest, ok := strings.CutPrefix(path, sysfsBlockDir+"/"); ok {
		if node, ok := strings.CutSuffix(rest, "/holders"); ok {
			for key, array := range f.arrays {
				if array.node == node &&
					f.dirs[sysfsBlockDir+"/"+node] {
					return f.holdersOf(key), nil
				}
			}
		}
	}
	if !f.dirs[path] {
		return nil, fmt.Errorf("no such directory: %s: %w", path,
			fs.ErrNotExist)
	}
	f.countHeadListing(path)
	f.countConnListing(path)
	return f.children(path), nil
}

// holdersOf is the sysfs holders directory of the array at key: the kernel
// names of the dm devices whose live or waiting table maps the array's
// number, as heldBy finds the first of them, sorted. Called with f.mu held.
func (f *fakeNode) holdersOf(key string) []string {
	array := f.arrays[key]
	var out []string
	for name, dm := range f.dms {
		for _, line := range dmTargets(dm.table + "\n" + dm.waitingTable()) {
			fields := strings.Fields(line)
			if len(fields) >= 4 && array.devNo != "" &&
				slices.Contains(fields[3:], array.devNo) {
				out = append(out, f.kernelName("/dev/mapper/"+name))
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// readMtime answers OsClient.ReadMtime, an mtime read in the agent's own
// process: the port probe's test of whether the port's directory is there
// (dnagent.md SH18), and the age of a host-facing subsystem the node-level
// sweep judges (cnagent.md CN21). A directory answers its time in mtimes, or
// fakeDirMtime, long past. It records "mtime <path>", and the command hooks match that
// line as they match a listing's: a fail hook answers an I/O error and a
// kill hook a read the soft timeout cut off, both never a missing
// directory, which only the model says. The gate, hardGate and cancelCmd
// hooks match it as they match a command's line: it is the first OS call
// of every converge and every read of a CN (the port probe, cnagent.md
// CN5), and a test parks or cuts off such a pass there (portProbeLine).
func (f *fakeNode) readMtime(
	ctx context.Context, path string,
) (time.Time, error) {
	if err := f.ctxErr(ctx); err != nil {
		return time.Time{}, err
	}
	f.mu.Lock()
	line := "mtime " + path
	f.record("%s", line)
	f.recordTrace(ctx, line)
	if _, ok := ctx.Deadline(); !ok && strings.HasPrefix(path, "/sys/") {
		// SH15, as for readFile.
		f.sysfsNoDeadline = append(f.sysfsNoDeadline, path)
	}
	gate, hard, cancel := f.parksOf(line)
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
	}
	if hard != nil {
		<-hard
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cancel != nil {
		// The read ran, and its caller's ctx went while it did.
		cancel()
		return time.Time{}, ctx.Err()
	}
	if err := f.inProcessHookErr(line, &fs.PathError{
		Op: "lstat", Path: path, Err: syscall.EIO}); err != nil {
		return time.Time{}, err
	}
	if _, link := f.links[path]; !f.dirs[path] && !link {
		return time.Time{}, &fs.PathError{
			Op: "lstat", Path: path, Err: syscall.ENOENT}
	}
	if mtime, ok := f.mtimes[path]; ok {
		return mtime, nil
	}
	return fakeDirMtime, nil
}

// parksOf is the gate and the hard gate whose key line matches, and the
// cancelCmd hook it takes, as runCommand finds them. Called with f.mu held.
func (f *fakeNode) parksOf(
	line string,
) (chan struct{}, chan struct{}, context.CancelFunc) {
	var gate, hard chan struct{}
	for key, ch := range f.gate {
		if strings.Contains(line, key) {
			gate = ch
			break
		}
	}
	for key, ch := range f.hardGate {
		if strings.Contains(line, key) {
			hard = ch
			break
		}
	}
	var cancel context.CancelFunc
	for key, fn := range f.cancelCmd {
		if strings.Contains(line, key) {
			delete(f.cancelCmd, key)
			cancel = fn
			break
		}
	}
	return gate, hard, cancel
}

// fakeDirMtime is the time readMtime answers for a directory mtimes does not
// name: long before any test's clock, so such a directory reads old.
var fakeDirMtime = time.Unix(1700000000, 0)

// inProcessHookErr applies the command hooks to the line of a call in the
// agent's own process, a listing or an mtime read, in runCommand's order:
// failCmd* answers failed, the call's own I/O error, and killCmd*,
// killCmdNth and killCmdNoEffect* a call the soft timeout cut off — such a
// call changes nothing, so the two kill halves are one. Called with f.mu
// held.
func (f *fakeNode) inProcessHookErr(line string, failed error) error {
	for key := range f.failCmd {
		if strings.Contains(line, key) {
			delete(f.failCmd, key)
			return failed
		}
	}
	for key := range f.failCmdAlways {
		if strings.Contains(line, key) {
			return failed
		}
	}
	killedNth := takeKillNth(f.killCmdNth, line)
	if takeKill(f.killCmdNoEffect, f.killCmdNoEffectAlways, line) ||
		takeKill(f.killCmd, f.killCmdAlways, line) || killedNth {
		return context.DeadlineExceeded
	}
	return nil
}

// fileOf is the content of the fake file at path, read under f.mu: a test
// checks a file while the agent's background work (the path watcher's cut, a
// sweep's background disconnect) can write the map, and an unlocked read of a
// map that another goroutine writes stops the test binary.
func (f *fakeNode) fileOf(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[path]
}

// fileHas reports whether the fake file at path exists, read under f.mu
// as fileOf reads.
func (f *fakeNode) fileHas(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[path]
	return ok
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
	for key, refusal := range f.failWriteAlways {
		if strings.Contains(path+"="+data, key) {
			return errors.New(refusal)
		}
	}
	if strings.HasPrefix(path, agent.NvmetDebugRoot+"/") {
		return f.writeConnState(path, data)
	}
	if !f.dirs[parentDir(path)] {
		return fmt.Errorf("no such directory: %s", parentDir(path))
	}
	if err := f.cntlidRefusal(path, data); err != nil {
		return err
	}
	if f.mdSyncActionWrite(path, data) || f.rescanWc(path) {
		return nil
	}
	f.files[path] = configfsNormalize(path, data)
	return nil
}

// mdSyncActionWrite models a write to a running array's md/sync_action
// (md's action_store) or md/sync_min: "check" starts a check, which reads
// "check" until a test ends it (setMdSync), and sets md/last_sync_action to
// it; "idle" stops it; a sync_min write sets sync_min. It reports whether
// path was such an attribute. The caller holds f.mu.
func (f *fakeNode) mdSyncActionWrite(path, data string) bool {
	attr := "/md/sync_action"
	node, ok := strings.CutSuffix(path, attr)
	if !ok {
		attr = "/md/sync_min"
		if node, ok = strings.CutSuffix(path, attr); !ok {
			return false
		}
	}
	node = strings.TrimPrefix(node, sysfsBlockDir+"/")
	for _, array := range f.arrays {
		if array.node != node {
			continue
		}
		value := strings.TrimSpace(data)
		if attr == "/md/sync_min" {
			array.syncMin = value
			f.publishArray(array)
			return true
		}
		array.syncAction = value
		if value == mdActionCheck {
			array.lastSyncAction = value
		}
		f.publishArray(array)
		return true
	}
	return false
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
// nguid rewrite it forever (agent.SameNsId).
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

// writeProto refuses a cancelled ctx before it writes or records anything,
// as common.LimitedOsClient.WriteProto does (ctxErr), for the reason the dn
// twin of this fake gives: the cut-off tests of the save order rest on it.
func (f *fakeNode) writeProto(
	ctx context.Context, path string, msg proto.Message,
) error {
	if err := f.ctxErr(ctx); err != nil {
		return err
	}
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
	if deadline, ok := ctx.Deadline(); ok && name == "mdadm" {
		for _, action := range []string{"--create", "--assemble", "--stop",
			"--force"} {
			if contains(args, action) {
				if f.cmdLimits == nil {
					f.cmdLimits = make(map[string]time.Duration)
				}
				f.cmdLimits[action] = max(f.cmdLimits[action],
					time.Until(deadline))
			}
		}
	}
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
	if name == "ls" {
		// The agent lists every directory in its own process (readDir;
		// dnagent.md SH17, SH18): an `ls` took the wrong path, and it gets
		// the answer of a run that never reported.
		return killedCmdResult()
	}
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
	case "dmsetup":
		return f.cmdDmsetup(args, stdin)
	case "blockdev":
		return f.cmdBlockdev(args)
	case "nvme":
		return f.cmdNvme(args)
	case "mdadm":
		return f.cmdMdadm(args)
	case "stat":
		return f.cmdStat(args)
	case "thin_dump":
		return f.cmdThinDump(args)
	case "losetup":
		return f.cmdLosetup(args)
	}
	return "", 127
}

// cmdLosetup is the two losetup calls of the agent's write cache loop device
// (wcacheDevNo): `losetup --find --show <file>`, which puts a new loop device
// over a file the node holds, read-write with a write cache, and prints its
// node; and `losetup -j <file> --noheadings --output MAJ:MIN`, which prints
// the number of each loop device over the file. A loop device is a set of
// the files the agent reads of it: its node's number, its backing file and
// its write cache. Called with f.mu held.
func (f *fakeNode) cmdLosetup(args []string) (string, int) {
	switch {
	case len(args) == 3 && args[0] == "--find" && args[1] == "--show":
		file := args[2]
		if _, ok := f.files[file]; !ok {
			f.dispatchStderr = "losetup: " + file + ": failed to set up loop device: No such file or directory"
			return "", 1
		}
		n := f.loopCnt
		f.loopCnt++
		node := fmt.Sprintf("/dev/loop%d", n)
		devNo := fmt.Sprintf("7:%d", n)
		f.devNo[node] = devNo
		f.files["/sys/dev/block/"+devNo+"/loop/backing_file"] = file + "\n"
		wc := f.loopWc
		if wc == "" {
			wc = "write back"
		}
		f.files["/sys/dev/block/"+devNo+"/queue/write_cache"] = wc + "\n"
		f.files["/sys/dev/block/"+devNo+"/size"] = fmt.Sprintf("%d\n",
			len(f.files[file])/512)
		return node + "\n", 0
	case len(args) == 2 && args[0] == "-d":
		devNo, ok := f.devNo[args[1]]
		if !ok {
			return "", 1
		}
		delete(f.files, "/sys/dev/block/"+devNo+"/loop/backing_file")
		return "", 0
	case len(args) >= 2 && args[0] == "-j":
		var out []string
		for n := 0; n < f.loopCnt; n++ {
			devNo := fmt.Sprintf("7:%d", n)
			back := f.files["/sys/dev/block/"+devNo+"/loop/backing_file"]
			if strings.TrimSpace(back) == args[1] {
				out = append(out, devNo)
			}
		}
		if len(out) == 0 {
			return "", 0
		}
		return strings.Join(out, "\n") + "\n", 0
	}
	return "", 1
}

// cmdBlockdev is `blockdev --setro|--setrw <path>` of a dm device: the block
// layer's own read-only flag (fakeDm.bdevReadOnly).
func (f *fakeNode) cmdBlockdev(args []string) (string, int) {
	if len(args) != 2 {
		return "", 1
	}
	dm, ok := f.dms[strings.TrimPrefix(args[1], "/dev/mapper/")]
	if !ok || !strings.HasPrefix(args[1], "/dev/mapper/") {
		f.dispatchStderr = "blockdev: cannot open " + args[1] +
			": No such file or directory"
		return "", 1
	}
	switch args[0] {
	case "--setro":
		dm.bdevReadOnly = true
	case "--setrw":
		dm.bdevReadOnly = false
	default:
		return "", 1
	}
	return "", 0
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

// childOf is the name of entry inside dir when entry is a direct child of
// it. The test of the entry's last slash comes first, read from the end of
// the entry, so a listing that walks every entry of the fake compares dir
// only with the entries whose parent is as long as dir: a node of a thousand
// exports holds tens of thousands of entries that share a long prefix.
func childOf(dir, entry string) (string, bool) {
	if strings.LastIndexByte(entry, '/') != len(dir) ||
		len(entry) == len(dir)+1 || entry[:len(dir)] != dir {
		return "", false
	}
	return entry[len(dir)+1:], true
}

// countHeadListing counts one listing of a subsystem directory against the
// head nsHeadAt staged there, and shows each step of the head whose listing
// this is, in the kernel's order.
func (f *fakeNode) countHeadListing(path string) {
	head := f.pendingHeads[path]
	if head == nil {
		return
	}
	head.reads++
	steps := head.at.steps()
	for head.shown < len(steps) && steps[head.shown] <= head.reads {
		f.addNsHeadStep(path, head.nsDev, head.nsIdx, head.shown)
		head.shown++
	}
	if head.shown == len(steps) {
		delete(f.pendingHeads, path)
	}
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
// makes the kernel populate its standard subdirectories, a subsystem its
// debug directory, while the fake models one (nvmetDebug), and a namespace
// its attributes at nvmet's defaults.
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
		if f.nvmetDebug {
			f.dirs[agent.NvmetDebugRoot+"/"+parts[1]] = true
		}
	case len(parts) == 4 && parts[0] == "subsystems" &&
		parts[2] == "namespaces":
		// nvmet_ns_alloc's defaults: not enabled, in group 1, with no
		// device and a zero identity, so a step cut off after the mkdir
		// leaves what the kernel leaves.
		zero := "00000000-0000-0000-0000-000000000000\n"
		f.files[path+"/enable"] = "0\n"
		f.files[path+"/ana_grpid"] = "1\n"
		f.files[path+"/device_path"] = agent.NsDevicePathUnset + "\n"
		f.files[path+"/device_uuid"] = zero
		f.files[path+"/device_nguid"] = zero
	}
}

// withNvmetDebug turns the model of nvmet's debug directory on (nvmetDebug),
// with a debug directory for every configfs subsystem the node holds.
func (f *fakeNode) withNvmetDebug() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nvmetDebug = true
	f.dirs[agent.NvmetDebugRoot] = true
	prefix := agent.NvmetRoot + "/subsystems/"
	for dir := range f.dirs {
		if nqn, ok := strings.CutPrefix(dir, prefix); ok &&
			!strings.Contains(nqn, "/") {
			f.dirs[agent.NvmetDebugRoot+"/"+nqn] = true
		}
	}
}

// connectNvmet plants a host's nvmet connection to the subsystem nqn under
// the lowest cntlid no connection of it holds, and returns the connection's
// directory name. The subsystem must have its debug directory.
func (f *fakeNode) connectNvmet(nqn, hostNqn string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	debugDir := agent.NvmetDebugRoot + "/" + nqn
	if !f.dirs[debugDir] {
		panic("connectNvmet: no debug directory " + debugDir)
	}
	for cntlid := 1; ; cntlid++ {
		name := fmt.Sprintf("ctrl%d", cntlid)
		if !f.dirs[debugDir+"/"+name] {
			f.plantConn(debugDir+"/"+name, hostNqn)
			return name
		}
	}
}

// plantConn makes the connection directory dir of host hostNqn. The caller
// holds f.mu.
func (f *fakeNode) plantConn(dir, hostNqn string) {
	f.dirs[dir] = true
	f.files[dir+"/hostnqn"] = hostNqn
	f.files[dir+"/state"] = "ready\n"
}

// dropConn frees a connection's directory, as the kernel does at the
// connection's last reference, and gives its name to the connection
// connReuse names. The caller holds f.mu.
func (f *fakeNode) dropConn(dir string) {
	delete(f.dirs, dir)
	delete(f.files, dir+"/hostnqn")
	delete(f.files, dir+"/state")
	delete(f.connEnding, dir)
	if hostNqn, ok := f.connReuse[dir]; ok {
		delete(f.connReuse, dir)
		f.plantConn(dir, hostNqn)
	}
}

// nvmetConnsOf lists the subsystem nqn's nvmet connections as
// "<name>=<host>", sorted, for a test's assertions.
func (f *fakeNode) nvmetConnsOf(nqn string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	debugDir := agent.NvmetDebugRoot + "/" + nqn
	var out []string
	for _, name := range f.children(debugDir) {
		if host, ok := f.files[debugDir+"/"+name+"/hostnqn"]; ok {
			out = append(out, name+"="+host)
		}
	}
	return out
}

// writeConnState models a write to a file of nvmet's debug directory, as
// debugfs and nvmet's handler of a connection's "state" take it: a file
// that is not there fails wrapping fs.ErrNotExist, as the production
// client's open does; a write to any file but a "state", or one that does
// not start with "fatal" in fewer than 16 bytes, fails EINVAL; "fatal" ends
// the connection, whose directory goes at once, or after the listings
// connEnds names, or never. The caller holds f.mu.
func (f *fakeNode) writeConnState(path, data string) error {
	if err := f.connRemoved(path); err != nil {
		return err
	}
	if _, ok := f.files[path]; !ok {
		return &fs.PathError{Op: "open", Path: path, Err: syscall.ENOENT}
	}
	if !strings.HasSuffix(path, "/state") || len(data) >= 16 ||
		!strings.HasPrefix(data, "fatal") {
		return &fs.PathError{Op: "write", Path: path, Err: syscall.EINVAL}
	}
	dir := parentDir(path)
	f.files[path] = "fatal\n"
	switch n, ok := f.connEnds[dir]; {
	case !ok || n == 0:
		f.dropConn(dir)
	case n > 0:
		if _, ending := f.connEnding[dir]; !ending {
			f.connEnding[dir] = n
		}
	}
	return nil
}

// connRemoved answers EIO for a file of a connection whose removal began
// after its open (connEIO), and frees the connection's directory with it.
// The caller holds f.mu.
func (f *fakeNode) connRemoved(path string) error {
	if !f.connEIO[path] {
		return nil
	}
	delete(f.connEIO, path)
	f.dropConn(parentDir(path))
	return &fs.PathError{Op: "read", Path: path, Err: syscall.EIO}
}

// countConnListing counts one listing of a subsystem's debug directory
// against each of its connections whose end connEnds delays, and frees the
// one whose listings are spent before this listing answers. The caller
// holds f.mu.
func (f *fakeNode) countConnListing(path string) {
	for dir, left := range f.connEnding {
		if parentDir(dir) != path {
			continue
		}
		if left == 0 {
			f.dropConn(dir)
			continue
		}
		f.connEnding[dir] = left - 1
	}
}

func (f *fakeNode) cmdRmdir(args []string) (string, int) {
	path := args[len(args)-1]
	if !f.dirs[path] {
		return "", 1
	}
	if nqn, ok := strings.CutPrefix(path, agent.NvmetRoot+"/subsystems/"); ok &&
		f.nvmetDebug && !strings.Contains(nqn, "/") {
		// The kernel frees the subsystem's debug directory with it, every
		// nvmet connection in it gone by then.
		debugDir := agent.NvmetDebugRoot + "/" + nqn
		for entry := range f.dirs {
			if entry == debugDir || strings.HasPrefix(entry, debugDir+"/") {
				delete(f.dirs, entry)
			}
		}
		for entry := range f.files {
			if strings.HasPrefix(entry, debugDir+"/") {
				delete(f.files, entry)
			}
		}
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
	// `ln -s` into a directory that is not there fails (ENOENT), as a
	// subsystem's link into a port whose directory is gone does.
	if dir := parentDir(link); dir != "" && !f.dirs[dir] {
		return "", 1
	}
	f.links[link] = target
	return "", 0
}

// cmdLsblk answers the cn agent's one lsblk: Md.NameInUse asks whether an
// array runs under /dev/md/{name}, and uses no number it reads. Every number
// the agent uses comes from dmsetup info or /sys/block/{kname}/dev
// (dmDevNo, blockDevNo, mdDevNo), so a MAJ:MIN read of any other path took
// the wrong path: it gets the answer of a run that never reported, which
// names that read, and so fails whatever test it reaches.
func (f *fakeNode) cmdLsblk(args []string) (string, int) {
	path := args[len(args)-1]
	if contains(args, "SIZE") {
		size, ok := f.devSize[path]
		if !ok {
			return "", 32
		}
		return fmt.Sprintf("%d\n", size), 0
	}
	if !strings.HasPrefix(path, "/dev/md/") {
		f.dispatchStderr = "fake: the cn agent reads no device number " +
			"with lsblk: " + path
		return "", -1
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
// something above them still references. An md array is held by whatever
// maps its own number, the one /sys/block publishes (fakeArray.devNo),
// wherever a test points its /dev/md/{name} link.
func (f *fakeNode) heldBy(path string) string {
	devNo := f.devNo[path]
	if array := f.arrays[path]; array != nil && array.devNo != "" {
		devNo = array.devNo
	}
	for name, dm := range f.dms {
		if f.devNo["/dev/mapper/"+name] == devNo && devNo != "" {
			continue
		}
		// A waiting table holds the devices it maps open as the live one
		// does.
		for _, line := range dmTargets(dm.table + "\n" + dm.waitingTable()) {
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

// newMdUuid mints an md array identity (fakeArray.uuid).
func (f *fakeNode) newMdUuid() string {
	f.nextMdUuid++
	return fmt.Sprintf("uuid-%d", f.nextMdUuid)
}

// discardRanges answers OsClient.DiscardRanges, the agent's in-process
// "mark hydrated" discards: one "discard --offset <o> --length <l> <dev>"
// line per range, in order, each kept in the device's discards list, with
// ctx checked before each range and the in-process hooks matching each line
// (inProcessHookErr), so a test can cut a call off part-way.
func (f *fakeNode) discardRanges(
	ctx context.Context,
	path string,
	ranges []common.DiscardRange,
) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ranges) == 0 {
		f.emptyDiscards++
	}
	for i, r := range ranges {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		args := fmt.Sprintf("--offset %d --length %d", r.Offset, r.Length)
		line := "discard " + args + " " + path
		f.record("%s", line)
		f.recordTrace(ctx, line)
		if err := f.inProcessHookErr(line, fmt.Errorf("BLKDISCARD %s: %w",
			path, syscall.EIO)); err != nil {
			return i, err
		}
		if dm, ok := f.dms[strings.TrimPrefix(path, "/dev/mapper/")]; ok {
			dm.discards = append(dm.discards, args)
			f.thinUnmap(dm, r)
		}
	}
	return len(ranges), nil
}

// dialTcp is the dial of an address before a connect (dialLost,
// dialRefused): recorded as "dial <address>", under the ctx's trace id.
func (f *fakeNode) dialTcp(
	ctx context.Context,
	address string,
	timeout time.Duration,
) error {
	if err := f.ctxErr(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	line := "dial " + address
	f.record("%s", line)
	f.recordTrace(ctx, line)
	if f.dialLost[address] {
		if f.advance != nil {
			f.advance(timeout)
		}
		return fmt.Errorf("dial tcp %s: %w after %s", address,
			common.ErrDialTimeout, timeout)
	}
	if left := f.dialRefused[address]; left > 0 {
		f.dialRefused[address] = left - 1
		return fmt.Errorf("dial tcp %s: connect: %w", address,
			syscall.ECONNREFUSED)
	}
	return nil
}

// thinUnmap is dm-thin's unmap of a discard on a thin volume: the blocks
// the range covers whole leave the id's run (thinMapLo, thinMapped), and a
// range that covers less than a block unmaps nothing. A range that cuts the
// run in two takes its blocks off the count and leaves the run's ends, so
// the status reads the highest block as dm-thin's does. A device that is no
// thin volume is left alone.
func (f *fakeNode) thinUnmap(dm *fakeDm, r common.DiscardRange) {
	pool, id, blockSectors, ok := f.thinOf(dm)
	if !ok || blockSectors == 0 {
		return
	}
	blockBytes := blockSectors * 512
	from := (r.Offset + blockBytes - 1) / blockBytes
	to := (r.Offset + r.Length) / blockBytes
	count := f.thinMapped[pool][id]
	lo := f.thinMapLo[pool][id]
	hi := lo + count
	from, to = max(from, lo), min(to, hi)
	if count == 0 || from >= to {
		return
	}
	switch {
	case from == lo && to == hi:
		f.setThinRun(pool, id, 0, 0)
	case to == hi:
		f.setThinRun(pool, id, lo, from-lo)
	case from == lo:
		f.setThinRun(pool, id, to, hi-to)
	default:
		f.setThinRun(pool, id, lo, count-(to-from))
	}
}

// thinOf names the pool and the id of a thin volume's live table, and the
// pool's block in sectors; ok is false for any other device.
func (f *fakeNode) thinOf(dm *fakeDm) (string, uint32, uint64, bool) {
	if !isDmTarget(dm.table, "thin") {
		return "", 0, 0, false
	}
	fields := strings.Fields(dmTargets(dm.table)[0])
	if len(fields) < 5 {
		return "", 0, 0, false
	}
	devId, err := strconv.ParseUint(fields[4], 10, 32)
	if err != nil {
		return "", 0, 0, false
	}
	pool := f.dmByDevNo(fields[3])
	if pool == nil {
		return "", 0, 0, false
	}
	for name, other := range f.dms {
		if other != pool {
			continue
		}
		var blockSectors uint64
		poolFields := strings.Fields(dmTargets(pool.table)[0])
		if len(poolFields) > 5 {
			blockSectors, _ = strconv.ParseUint(poolFields[5], 10, 64)
		}
		return name, uint32(devId), blockSectors, true
	}
	return "", 0, 0, false
}

// setThinRun sets an id's run of mapped blocks: count of them from lo.
// Called with f.mu held.
func (f *fakeNode) setThinRun(pool string, id uint32, lo, count uint64) {
	if f.thinMapped[pool] == nil {
		f.thinMapped[pool] = make(map[uint32]uint64)
	}
	if f.thinMapLo[pool] == nil {
		f.thinMapLo[pool] = make(map[uint32]uint64)
	}
	f.thinMapped[pool][id] = count
	f.thinMapLo[pool][id] = lo
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

// fakeTable is one loaded table and its read-only flag.
type fakeTable struct {
	table    string
	readOnly bool
}

// waitingTable is the device's waiting table, "" when none waits.
func (dm *fakeDm) waitingTable() string {
	if dm.waiting == nil {
		return ""
	}
	return dm.waiting.table
}

func (f *fakeNode) cmdDmsetup(args []string, stdin string) (string, int) {
	if len(args) == 0 {
		return "", 3
	}
	switch args[0] {
	case "ls":
		// The real tool prints "{name}\t({major}:{minor})" per device and the
		// literal "No devices found" — still exit 0 — on an empty node, which
		// the sweep's strict listing reads as no device (Dm.List).
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
		fields, sep, name := parseDmInfoFlags(args[1:])
		if name == "" {
			if fields != "name,major,minor,attr" {
				return "", 3
			}
			return f.dmInfoListing(sep)
		}
		dm, ok := f.dms[name]
		switch fields {
		case "attr":
			if !ok {
				// The lab's words for a name no device has, the one failure
				// Dm.Info reads as absent.
				f.dispatchStderr = "Device does not exist.\nCommand failed."
				return "", 1
			}
			return dmAttr(dm) + "\n", 0
		case "name,major,minor,attr":
			// The per-device read with the number (Dm.InfoDevNo): one line,
			// or the lab's refusal of a name that is not there.
			if !ok {
				f.dispatchStderr = "Device does not exist.\nCommand failed."
				return "", 1
			}
			major, minor, _ := strings.Cut(f.devNo["/dev/mapper/"+name], ":")
			return strings.Join(
				[]string{name, major, minor, dmAttr(dm)}, sep) + "\n", 0
		}
		return "", 3
	case "table":
		if len(args) == 2 && args[1] == "--concise" {
			return f.dmTableListing()
		}
		if len(args) == 3 && args[1] == "--inactive" {
			dm, ok := f.dms[args[2]]
			if !ok {
				return "", 1
			}
			return strings.Join(dmTargets(dm.waitingTable()), "\n") + "\n", 0
		}
		dm, ok := f.dms[args[1]]
		if !ok {
			return "", 1
		}
		return strings.Join(dmTargets(dm.table), "\n") + "\n", 0
	case "status":
		// `dmsetup status [--noflush] <name>`: a thin pool's read and a
		// dm-clone's first read carry the flag (dnagent.md SH17).
		if len(args) == 3 && args[1] == "--noflush" {
			return f.dmStatus(args[2])
		}
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
		if args[0] == "resume" && dm.waiting != nil {
			// A resume makes the waiting table live.
			f.goLive(args[1], dm)
		}
		if args[0] == "resume" {
			f.poolTakeMeta(dm)
		}
		dm.suspended = args[0] == "suspend"
		return "", 0
	case "clear":
		dm, ok := f.dms[args[1]]
		if !ok {
			return "", 1
		}
		dm.waiting = nil
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
			// makes a teardown that runs out of order fail a test
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

// dmInfoListing is the whole-node `dmsetup info` listing (Dm.ListInfo): one
// line per device, name/MAJ/MIN/ATTR in sorted order, a name `dmsetup ls`
// names after the device went (lsGhosts) with no numbers, and on a
// node with no device at all the lab's "No devices found" with exit 0. A
// name infoOmit leaves out makes the listing exit 1 with the lab's stderr.
// The fake writes no name that holds the separator.
func (f *fakeNode) dmInfoListing(sep string) (string, int) {
	if len(f.dms) == 0 && len(f.lsGhosts) == 0 {
		return "No devices found\n", 0
	}
	type line struct{ name, text string }
	var lines []line
	omitted := false
	for name, dm := range f.dms {
		if f.infoOmit[name] {
			omitted = true
			continue
		}
		major, minor, _ := strings.Cut(f.devNo["/dev/mapper/"+name], ":")
		lines = append(lines, line{name, strings.Join(
			[]string{name, major, minor, dmAttr(dm)}, sep)})
	}
	for _, name := range f.lsGhosts {
		if _, live := f.dms[name]; live {
			continue
		}
		lines = append(lines, line{name, strings.Join(
			[]string{name, "", "", "L--w"}, sep)})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].name < lines[j].name })
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.text + "\n")
	}
	if omitted {
		f.dispatchStderr = "Device does not exist.\nCommand failed."
		return sb.String(), 1
	}
	return sb.String(), 0
}

// dmTableListing is the whole-node `dmsetup table --concise` listing
// (Dm.ListTables) in the lab's form: per device "name,uuid,minor,flags",
// then one field per table line, each ending in a space as dmsetup prints
// it; "," ";" and "\" escaped with a backslash; ";" between devices and one
// newline at the end; "No devices found" on a node with none. A ghost of
// lsGhosts is not there. A name tableOmit leaves out makes the listing exit 1
// with the lab's stderr for a table that went mid-listing.
func (f *fakeNode) dmTableListing() (string, int) {
	if len(f.dms) == 0 {
		return "No devices found\n", 0
	}
	names := make([]string, 0, len(f.dms))
	for name := range f.dms {
		names = append(names, name)
	}
	sort.Strings(names)
	var records []string
	var omitted []string
	for _, name := range names {
		if f.tableOmit[name] {
			omitted = append(omitted, name)
			continue
		}
		dm := f.dms[name]
		_, minor, _ := strings.Cut(f.devNo["/dev/mapper/"+name], ":")
		flags := "rw"
		if dm.readOnly {
			flags = "ro"
		}
		fields := []string{dmConciseEscape(name), "", minor, flags}
		for _, target := range dmTargets(dm.table) {
			fields = append(fields, dmConciseEscape(target)+" ")
		}
		records = append(records, strings.Join(fields, ","))
	}
	out := strings.Join(records, ";")
	if len(records) > 0 {
		out += "\n"
	}
	if len(omitted) > 0 {
		f.dispatchStderr = "device-mapper: table ioctl on " + omitted[0] +
			"  failed: No such device or address\nCommand failed."
		return out, 1
	}
	return out, 0
}

// dmConciseEscape escapes one field of the concise listing as dmsetup does:
// a backslash before each ",", ";" and "\".
func dmConciseEscape(field string) string {
	var sb strings.Builder
	for i := 0; i < len(field); i++ {
		switch field[i] {
		case ',', ';', '\\':
			sb.WriteByte('\\')
		}
		sb.WriteByte(field[i])
	}
	return sb.String()
}

// dmInfoListLine and dmTableListLine are the recorded lines of the two
// whole-node listings of a reading, exactly: a count by them counts the
// listings alone, never a per-device read whose line extends them.
const (
	dmInfoListLine = "cmd dmsetup info --columns --noheadings --separator / " +
		"-o name,major,minor,attr"
	dmTableListLine = "cmd dmsetup table --concise"
)

// dmNumberRead is the command line, without its "cmd ", of the per-device
// `dmsetup info` that prints a device's number (Dm.InfoDevNo), up to the
// device's name. A hook key or a call fragment that appends a name matches
// the number reads of that device alone, as no dnv name extends another.
const dmNumberRead = "dmsetup info --columns --noheadings --separator / " +
	"-o name,major,minor,attr "

// parseDmInfoFlags reads the options of a `dmsetup info`: the fields -o
// names, the --separator (":" by default, as dmsetup's) and the device name,
// "" when none is named.
func parseDmInfoFlags(args []string) (fields, sep, name string) {
	sep = ":"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--columns", "-c", "--noheadings":
		case "-o", "--options":
			if i+1 < len(args) {
				i++
				fields = args[i]
			}
		case "--separator":
			if i+1 < len(args) {
				i++
				sep = args[i]
			}
		default:
			name = args[i]
		}
	}
	return fields, sep, name
}

// dmAttr is a device's `dmsetup info` attr column: "L" for a live table, "-"
// for a device with none, as the lab's dmsetup prints one created with no
// table; then "I" while a table waits; then "s" when suspended, then "r" for
// read-only or "w".
func dmAttr(dm *fakeDm) string {
	attr := []byte("L---")
	if strings.TrimSpace(dm.table) == "" {
		attr[0] = '-'
	}
	if dm.waiting != nil {
		attr[1] = 'I'
	}
	if dm.suspended {
		attr[2] = 's'
	}
	if dm.readOnly || dm.bdevReadOnly {
		attr[3] = 'r'
	} else {
		attr[3] = 'w'
	}
	return string(attr)
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
	if isDmTarget(table, "thin-pool") {
		dm.heldRoot = f.heldRoots[name]
	}
	applyCloneTable(dm, table)
	if code := f.checkTableDeps(table); code != 0 {
		return "", code
	}
	if code := f.checkThinTable(name, table); code != 0 {
		return "", code
	}
	f.dms[name] = dm
	f.poolTakeMeta(dm)
	path := "/dev/mapper/" + name
	devNo := f.newDevNo()
	f.devNo[path] = devNo
	if f.devNoPaths != nil {
		f.devNoPaths[devNo] = path
	}
	if f.dmOfDevNo != nil {
		f.dmOfDevNo[devNo] = name
	}
	f.devSize[path] = tableSectors(table) * 512
	f.stackWc(devNo, table)
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
	dm.waiting = &fakeTable{table: table, readOnly: readOnly}
	return "", 0
}

// fakeThinMetaMaxBlocks is the kernel's DM_SM_METADATA_MAX_BLOCKS, written
// out here and not taken from common, so the fake does not prove the agent's
// arithmetic with the agent's own function.
const fakeThinMetaMaxBlocks = 255 * (16384 - 64)

// poolTakeMeta is dm-thin taking a thin-pool's metadata device, at its
// create and at each resume (pool_preresume, maybe_resize_metadata_dev): the
// total grows to what the device holds in whole 4 KiB blocks, capped as the
// kernel caps it, never shrinks, and stays put under needs_check. Called
// with f.mu held; a device that is no thin-pool, or whose metadata device is
// gone, is left alone.
func (f *fakeNode) poolTakeMeta(dm *fakeDm) {
	fields := strings.Fields(dm.table)
	if len(fields) < 4 || fields[2] != "thin-pool" || dm.poolNeedsCheck {
		return
	}
	meta := f.dmByDevNo(fields[3])
	if meta == nil {
		return
	}
	blocks := min(tableSectors(meta.table)/8, fakeThinMetaMaxBlocks)
	if blocks > dm.poolMetaBlocks {
		dm.poolMetaBlocks = blocks
	}
}

// goLive makes the waiting table of dm, the device name, its live one.
func (f *fakeNode) goLive(name string, dm *fakeDm) {
	dm.table, dm.readOnly = dm.waiting.table, dm.waiting.readOnly
	dm.waiting = nil
	applyCloneTable(dm, dm.table)
	f.devSize["/dev/mapper/"+name] = tableSectors(dm.table) * 512
	f.stackWc(f.devNo["/dev/mapper/"+name], dm.table)
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

// holdMetaSnap leaves a metadata snapshot held in a pool's metadata, and in
// its device when the pool is up, as an agent that died between a
// reserve_metadata_snap and its release leaves it (heldRoots).
func (f *fakeNode) holdMetaSnap(pool string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heldRoots[pool] = true
	if dm := f.dms[pool]; dm != nil {
		dm.heldRoot = true
	}
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

// holdThinData seeds a pool's dm-thin metadata with an id no cntlr of this
// node created that maps blocks: what an etcd restore leaves for a new volume
// that takes an old volume's id (CN14). The id is held as holdThinIds holds
// it.
func (f *fakeNode) holdThinData(pool string, id uint32, blocks uint64) {
	f.holdThinIds(pool, id)
	f.mapThinBlocks(pool, id, blocks)
}

// thinStatus is a thin volume's status line, as dm-thin prints it
// (thin_status): the sectors its id maps in its pool's metadata
// (thinMapped), then the last sector of its highest mapped block, or "-"
// while it maps none. The fake maps an id's blocks as one run (thinMapLo),
// so the highest is the run's last. fields are the volume's live table line,
// split.
func (f *fakeNode) thinStatus(sectors string, fields []string) string {
	var blocks, lo, blockSectors uint64
	if len(fields) >= 5 {
		line := &fakeDm{table: strings.Join(fields, " ")}
		if pool, id, bs, ok := f.thinOf(line); ok {
			blocks = f.thinMapped[pool][id]
			lo = f.thinMapLo[pool][id]
			blockSectors = bs
		}
	}
	if blocks == 0 || blockSectors == 0 {
		return fmt.Sprintf("0 %s thin 0 -\n", sectors)
	}
	return fmt.Sprintf("0 %s thin %d %d\n", sectors, blocks*blockSectors,
		(lo+blocks)*blockSectors-1)
}

// mapThinBlocks makes a pool's id map blocks (thinMapped), as a host's
// writes to its volume do, so the volume's status reads them mapped from
// its first block (thinStatus).
func (f *fakeNode) mapThinBlocks(pool string, id uint32, blocks uint64) {
	f.mapThinRun(pool, id, 0, blocks)
}

// mapThinRun makes a pool's id map blocks from block lo on, one run.
func (f *fakeNode) mapThinRun(pool string, id uint32, lo, blocks uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setThinRun(pool, id, lo, blocks)
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
	if name, ok := f.dmOfDevNo[devNo]; ok {
		if dm := f.dms[name]; dm != nil &&
			f.devNo["/dev/mapper/"+name] == devNo {
			return dm
		}
	}
	f.dmOfDevNo = make(map[string]string, len(f.dms))
	for name := range f.dms {
		f.dmOfDevNo[f.devNo["/dev/mapper/"+name]] = name
	}
	if name, ok := f.dmOfDevNo[devNo]; ok {
		return f.dms[name]
	}
	return nil
}

// devNoHeld reports whether some path of devNo holds the device number
// devNo, through the devNoPaths index.
func (f *fakeNode) devNoHeld(devNo string) bool {
	if path, ok := f.devNoPaths[devNo]; ok && f.devNo[path] == devNo {
		return true
	}
	f.devNoPaths = make(map[string]string, len(f.devNo))
	for path, number := range f.devNo {
		f.devNoPaths[number] = path
	}
	_, ok := f.devNoPaths[devNo]
	return ok
}

// checkTableDeps rejects a table naming a device number that does not exist —
// the kernel's behavior, and what makes an out-of-order build fail in tests.
// An md array's own number exists whatever its /dev/md/{name} link says.
func (f *fakeNode) checkTableDeps(table string) int {
	arrayHolds := func(devNo string) bool {
		for _, array := range f.arrays {
			if array.devNo == devNo {
				return true
			}
		}
		return false
	}
	for _, line := range dmTargets(table) {
		fields := strings.Fields(line)
		for _, field := range fields[3:] {
			if strings.Count(field, ":") != 1 {
				continue
			}
			if !f.devNoHeld(field) && !arrayHolds(field) {
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
	// dm-thin serves no pool message to a pool whose mode is out of
	// metadata space, read-only or failed (pool_message): its status reads
	// ro, needs_check or a fault word then.
	if isDmTarget(dm.table, "thin-pool") &&
		(dm.poolMode == "ro" || dm.poolNeedsCheck || dm.poolFault != "") {
		f.dispatchStderr = msgIoctlFailed(args[1], "Operation not supported")
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
			f.dispatchStderr = msgIoctlFailed(args[1], "File exists")
			return "", 1
		}
		dm.thinIds[uint32(devId)] = true
	case strings.HasPrefix(message, "create_snap "):
		if len(fields) < 3 {
			return "", 3
		}
		devId, _ := strconv.ParseUint(fields[1], 10, 32)
		oriId, _ := strconv.ParseUint(fields[2], 10, 32)
		// dm-thin refuses a create_snap of an id the pool already holds with
		// EEXIST — what CN14's torn mark reads as "the slice already had
		// it" — and one whose origin the pool does not hold with ENODATA.
		// The second is the failure mode of CN14's "A violated precondition
		// is left to dm-thin": the gateway's job is to keep the origin
		// materialized, and an agent that meets a violated precondition
		// simply reports the error and retries.
		if dm.thinIds[uint32(devId)] {
			f.dispatchStderr = msgIoctlFailed(args[1], "File exists")
			return "", 1
		}
		if !dm.thinIds[uint32(oriId)] {
			f.dispatchStderr = msgIoctlFailed(args[1], "No data available")
			return "", 1
		}
		dm.thinIds[uint32(devId)] = true
		dm.snapshot = true
	case strings.HasPrefix(message, "delete "):
		devId, _ := strconv.ParseUint(fields[1], 10, 32)
		delete(dm.thinIds, uint32(devId))
		// dm-thin frees the id's mappings with it.
		delete(f.thinMapped[args[1]], uint32(devId))
		delete(f.thinMapLo[args[1]], uint32(devId))
	case message == "reserve_metadata_snap":
		if dm.heldRoot {
			f.dispatchStderr = msgIoctlFailed(args[1],
				"Device or resource busy")
			return "", 1
		}
		dm.heldRoot = true
		f.heldRoots[args[1]] = true
		f.lastSnapPool = args[1]
	case message == "release_metadata_snap":
		if !dm.heldRoot {
			return "", 1
		}
		dm.heldRoot = false
		delete(f.heldRoots, args[1])
	}
	return "", 0
}

// msgIoctlFailed is dmsetup's stderr for a message the kernel refused: the
// ioctl, the device, and the errno in strerror's words.
func msgIoctlFailed(name, strerror string) string {
	return "device-mapper: message ioctl on " + name + "  failed: " + strerror
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
		return f.thinStatus(sectors, fields), 0
	case "thin-pool":
		held := "-"
		if dm.heldRoot {
			held = "123"
		}
		// The thin-pool auto-grow (dnv-worker.md AR6)
		// parses the two used/total pairs out of this, and
		// the converge and the read compare the metadata total with
		// the metadata device (CN13).
		if dm.poolFault != "" {
			return fmt.Sprintf("0 %s thin-pool %s\n", sectors,
				dm.poolFault), 0
		}
		check, mode := "-", "rw"
		if dm.poolMode != "" {
			mode = dm.poolMode
		}
		if dm.poolNeedsCheck {
			check, mode = "needs_check", "ro"
		}
		return fmt.Sprintf(
			"0 %s thin-pool 0 12/%d 5/%s %s %s discard_passdown "+
				"queue_if_no_space %s 1024\n",
			sectors, dm.poolMetaBlocks, fields[6], held, mode, check), 0
	case "clone":
		if dm.metaMode == common.CloneMetadataFail {
			return fmt.Sprintf("0 %s clone Fail\n", sectors), 0
		}
		regionSectors := fields[6]
		regions := tableSectors(dm.table)
		region, _ := strconv.ParseUint(regionSectors, 10, 64)
		if region > 0 {
			regions /= region
		}
		hydrated := f.cloneHydrated[f.cloneMetaKey(fields[3])]
		if hydrated > regions {
			hydrated = regions
		}
		mode := common.CloneMetadataReadWrite
		if dm.metaMode != "" {
			mode = dm.metaMode
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
			"0 %s clone 8 1/1024 %s %d/%d 0 %s 4 hydration_threshold %d "+
				"hydration_batch_size %d %s\n",
			sectors, regionSectors, hydrated, regions, features,
			dm.threshold, dm.batchSize, mode), 0
	}
	return "", 1
}

// ---------------------------------------------------------------------------
// md
// ---------------------------------------------------------------------------

func (f *fakeNode) cmdMdadm(args []string) (string, int) {
	// mdadm 4.5 takes the superblock format in its create, assemble and misc
	// modes only, and never as the first option (cnagent_integtest.md, Lab
	// facts).
	for i, arg := range args {
		if !strings.HasPrefix(arg, "--metadata") {
			continue
		}
		switch {
		case i == 0:
			f.dispatchStderr = "mdadm: --metadata does not set the mode, " +
				"and so cannot be the first option."
			return "", 2
		case contains(args, "--add") || contains(args, "--fail") ||
			contains(args, "--remove"):
			f.dispatchStderr = "mdadm: :option --metadata not valid in " +
				"manage mode"
			return "", 2
		}
	}
	switch {
	case contains(args, "--zero-superblock"):
		return f.mdZeroSuperblock(args)
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
		if f.stopDeleting {
			f.leaveDeleted(array.node)
		}
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

// cloneMetaKey is the storage a dm-clone's metadata device, named by its
// number in the dm-clone's table, sits on (fakeNode.cloneHydrated): an md
// array's identity (fakeArray.uuid), a dm device's name, or the number
// itself for anything else.
func (f *fakeNode) cloneMetaKey(metaNo string) string {
	for _, array := range f.arrays {
		if array.devNo == metaNo && array.uuid != "" {
			return "md:" + array.uuid
		}
	}
	for name := range f.dms {
		if f.devNo["/dev/mapper/"+name] == metaNo {
			return "dm:" + name
		}
	}
	return "dev:" + metaNo
}

// setCloneHydrated sets the hydrated count the metadata of one dm-clone
// holds, on the storage its table names (cloneMetaKey), as a copy that has
// gone that far leaves it.
func (f *fakeNode) setCloneHydrated(t *testing.T, name string, count uint64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	dm := f.dms[name]
	if dm == nil {
		t.Fatalf("no dm-clone %s", name)
	}
	fields := strings.Fields(dm.table)
	if len(fields) < 4 || fields[2] != "clone" {
		t.Fatalf("%s is not a dm-clone: %q", name, dm.table)
	}
	f.cloneHydrated[f.cloneMetaKey(fields[3])] = count
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
		delete(f.sbFacts, member)
	}
	uuid := f.newMdUuid()
	for _, member := range members {
		f.memberArray[member] = uuid
	}
	f.installArray(dev, &fakeArray{name: name, members: members, uuid: uuid})
	f.devNo[dev] = f.newDevNo()
	f.arrays[dev].devNo = f.devNo[dev]
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
	// mdadm refuses an assembly handed members of two identities, in every
	// order of the list (cnagent_integtest.md, Lab facts).
	for _, member := range members[1:] {
		if f.fakeIdentityOf(member) != f.fakeIdentityOf(members[0]) {
			f.dispatchStderr = "mdadm: superblock on " + member +
				" doesn't match others - assembly aborted"
			return "", 1
		}
	}
	// md starts no array from a half-copied member alone, --run and --force
	// or not.
	if len(members) == 1 && f.sbFacts[members[0]].recovery != "" {
		f.dispatchStderr = "mdadm: Not enough devices to start the array."
		return "", 1
	}
	if f.cutShortAssemblies > 0 && !contains(args, "--run") {
		f.cutShortAssemblies--
		f.leaveInactive(dev, name, members[0])
		f.dispatchStderr = "signal: terminated"
		return "", 1
	}
	// Without --run, mdadm starts the array only from enough members
	// (fakeAssemblyStarts), and says how many it lacks; one that starts, it
	// starts without each member whose add the kernel refused (bblRefused).
	if !contains(args, "--run") {
		if ok, why := f.fakeAssemblyStarts(members); !ok {
			f.dispatchStderr = "mdadm: " + dev + " assembled from " + why
			return "", 1
		}
		members = slices.DeleteFunc(members, func(member string) bool {
			return f.bblRefused[member]
		})
	}
	// The array is the one its started members' superblocks name. Only an
	// assembly whose started members all carry a superblock a test stamped
	// by hand mints a new identity, and an assembly with --update=uuid,
	// which gives the array a new one; every member it starts takes the
	// array's identity.
	uuid := ""
	for _, member := range members {
		if uuid = f.memberArray[member]; uuid != "" {
			break
		}
	}
	if uuid == "" || contains(args, "--update=uuid") {
		uuid = f.newMdUuid()
	}
	if contains(args, "--update=force-no-bbl") {
		for _, member := range members {
			delete(f.bblRefused, member)
			facts := f.sbFacts[member]
			facts.badBlocks = 0
			f.sbFacts[member] = facts
		}
	}
	for _, member := range members {
		if !f.bblRefused[member] {
			continue
		}
		// mdadm has written its update into every member's superblock
		// before the kernel refuses this one.
		if contains(args, "--update=uuid") {
			for _, member := range members {
				f.memberArray[member] = uuid
			}
		}
		f.dispatchStderr = "mdadm: failed to add " + member + " to " + dev +
			": Invalid argument"
		if f.refusedLeaves {
			f.leaveInactive(dev, name, member)
		}
		return "", 1
	}
	for _, member := range members {
		f.memberArray[member] = uuid
	}
	array := &fakeArray{name: name, members: members, uuid: uuid}
	f.installArray(dev, array)
	f.devNo[dev] = f.newDevNo()
	array.devNo = f.devNo[dev]
	f.devSize[dev] = f.arraySize(members)
	f.publishArray(array)
	return "", 0
}

// fakeAssemblyStarts is the fake's degraded gate of mdadm's plain assembly,
// from what each member's superblock says (sbFacts), as the lab's mdadm
// keeps it: the newest member in a place, by change count, decides how
// many working members the array needs, the places its view records as
// working; a member in a place counts, once a place, when its change count
// is at most one below the newest's and its view records the newest's
// place as working, the newest itself always; and the array starts when at
// least one member that counts carries no copy position and the members
// that count are as many as the array needs. mdadm adds every member it
// keeps in a place, counted or not, and each add the kernel refuses, of a
// member whose bad-block list it cannot import (bblRefused), takes one off
// the members that count; the array needs one in-sync member whose add
// went through. A refusal says why in the words of the lab's mdadm after
// "assembled from ". Called with f.mu held.
func (f *fakeNode) fakeAssemblyStarts(members []string) (bool, string) {
	type placed struct {
		member string
		sb     fakeSuperblock
		own    uint16
	}
	var newest *placed
	byPlace := make(map[uint16]*placed)
	for _, member := range members {
		sb := f.sbFacts[member]
		own := fakeRoleOf(sb.role)
		if own >= mdRoleMax {
			continue
		}
		p := &placed{member: member, sb: sb, own: own}
		if newest == nil || sb.events > newest.sb.events {
			newest = p
		}
		if cur := byPlace[own]; cur == nil || sb.events > cur.sb.events {
			byPlace[own] = p
		}
	}
	if newest == nil {
		return false, "0 drives - not enough to start the array."
	}
	need := 0
	for place := 0; place < fakeRaidDisks(newest.sb, newest.own); place++ {
		if fakeSeesWorking(newest.sb, newest.own, uint16(place)) {
			need++
		}
	}
	whole, counted, refused, wholeAdded := 0, 0, 0, 0
	for _, p := range byPlace {
		if p != newest && !fakeSeesWorking(p.sb, p.own, newest.own) {
			continue
		}
		if f.bblRefused[p.member] {
			refused++
		}
		if p.sb.events+1 < newest.sb.events {
			continue
		}
		counted++
		if p.sb.recovery == "" {
			whole++
			if !f.bblRefused[p.member] {
				wholeAdded++
			}
		}
	}
	switch {
	case whole == 0:
		return false, fmt.Sprintf("0 drives and %d rebuilding - not "+
			"enough to start the array.", counted)
	case counted-refused < need || wholeAdded == 0:
		return false, fmt.Sprintf("%d drive(s) - need %d to start (use "+
			"--run to insist).", counted-refused, need)
	}
	return true, ""
}

// leaveInactive installs the inactive array a refused assembly leaves at dev
// (refusedLeaves), holding member and refusedLeavesAlso.
func (f *fakeNode) leaveInactive(dev, name, member string) {
	members := append([]string{member}, f.refusedLeavesAlso...)
	for _, held := range members {
		if _, ok := f.devNo[held]; !ok {
			f.devNo[held] = f.newDevNo()
		}
	}
	array := &fakeArray{name: name, members: members,
		arrayState: "inactive"}
	f.installArray(dev, array)
	f.devNo[dev] = f.newDevNo()
	array.devNo = f.devNo[dev]
	f.publishArray(array)
}

// leaveDeleted publishes what md leaves of a stopped array at node while it
// has marked it deleted (stopDeleting): the node's /sys/block entry and its
// md/ directory, with no member directory, and an array_state whose read
// fails EBUSY. Both go once a read of that array_state has its answer.
// Called with f.mu held.
func (f *fakeNode) leaveDeleted(node string) {
	root := sysfsBlockDir + "/" + node
	state := root + "/md/array_state"
	f.dirs[root] = true
	f.dirs[root+"/md"] = true
	f.errFiles[state] = &fs.PathError{Op: "read", Path: state,
		Err: syscall.EBUSY}
	f.afterRead[state] = func() {
		delete(f.errFiles, state)
		delete(f.dirs, root+"/md")
		delete(f.dirs, root)
	}
}

// fakeStampedUuid is the identity of a member whose superblock a test
// stamped by hand, and of an array a test seeded or installed by hand, which
// names no array of the fake's (fakeNode.memberArray, fakeArray.uuid).
const fakeStampedUuid = "uuid-stamped"

// fakeSuperblock is what a member's superblock says beyond its identity
// (fakeNode.sbFacts), which the fake renders as md lays it out
// (fakeSuperblockArea).
type fakeSuperblock struct {
	// role is the role as mdadm's "Device Role" line spells it: "Active
	// device N", "spare" or "faulty"; "" reads "Active device 0".
	role string
	// recovery is the copy position as mdadm's "Recovery Offset" line
	// spells it, "N sectors", "" for none: a half-copied member.
	recovery string
	events   uint64
	// badBlocks is the length of the member's bad-block list.
	badBlocks int
	// arrayState is the member's view of the array as mdadm's "Array
	// State" line spells it: one character a place, 'A' for a place the
	// member records as working and '.' for one it does not, so its length
	// is the array's number of places (fakeSuperblockArea). "" records the
	// member's own place alone as working, in an array of two places, or
	// of one more than its own place: a view by which any one member that
	// carries a superblock starts the array, as the fake started one before
	// it read the view.
	arrayState string
}

// fakeRaidDisks is the number of places of the array a member's superblock
// says it has (fakeSuperblock.arrayState), own its own role word.
func fakeRaidDisks(sb fakeSuperblock, own uint16) int {
	if sb.arrayState != "" {
		return len(sb.arrayState)
	}
	if own < mdRoleMax && int(own) >= 2 {
		return int(own) + 1
	}
	return 2
}

// fakeSeesWorking reports whether a member's superblock records place as
// working (fakeSuperblock.arrayState), own its own role word.
func fakeSeesWorking(sb fakeSuperblock, own uint16, place uint16) bool {
	if sb.arrayState == "" {
		return place == own
	}
	return int(place) < len(sb.arrayState) && sb.arrayState[place] == 'A'
}

// setSuperblock stamps a superblock on a member device with the facts it
// says.
func (f *fakeNode) setSuperblock(dev string, sb fakeSuperblock) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.superblocks[dev] = true
	f.sbFacts[dev] = sb
}

// sameArrayAs gives dev's superblock the identity other's carries, as a
// member that left other's array keeps it: a retired leg of the group's own
// history.
func (f *fakeNode) sameArrayAs(dev, other string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memberArray[dev] = f.fakeIdentityOf(other)
}

// hasSuperblock reports whether a member device carries md metadata.
func (f *fakeNode) hasSuperblock(dev string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.superblocks[dev]
}

// fakeUuidBytes are the 16 bytes of a fake identity, as a superblock and
// md/uuid carry them.
func fakeUuidBytes(id string) []byte {
	sum := md5.Sum([]byte(id))
	return sum[:]
}

// fakeUuidText is a fake identity as the agent reads it, in mdadm's
// spelling (mdUuidText).
func fakeUuidText(id string) string {
	return mdUuidText(fakeUuidBytes(id))
}

// fakeIdentityOf is the identity a member's superblock carries: the array's
// that wrote it, else fakeStampedUuid. Called with f.mu held.
func (f *fakeNode) fakeIdentityOf(dev string) string {
	if id := f.memberArray[dev]; id != "" {
		return id
	}
	return fakeStampedUuid
}

// fakeBblSectors is where the fake puts a member's bad-block list, in
// sectors from the superblock, as mdadm places it on a dnv leg (the lab's
// "512 entries available at offset 16 sectors").
const fakeBblSectors = 16

// fakeSuperblockArea renders the area at mdSbOffset of a member that carries
// a superblock, in the kernel's layout of struct mdp_superblock_1 with a
// checksum md would accept: identity id, sb's role, copy position, events
// count and bad-block list, and, for a member whose bad-block log the kernel
// refuses (bblRefused), a list whose first entry has length zero, as lab
// fact 10 shows it.
func fakeSuperblockArea(id string, sb fakeSuperblock, refused bool) []byte {
	le := binary.LittleEndian
	area := make([]byte, mdSbAreaLen)
	const maxDev = 128
	le.PutUint32(area[mdSbMagicAt:], mdSbMagic)
	le.PutUint32(area[mdSbMajorVersionAt:], 1)
	features := uint32(1)
	copy(area[mdSbSetUuidAt:], fakeUuidBytes(id))
	le.PutUint64(area[mdSbSuperOffsetAt:], mdSbOffsetSectors)
	if sb.recovery != "" {
		features |= mdFeatureRecoveryOffset
		n, _ := strconv.ParseUint(strings.Fields(sb.recovery)[0], 10, 64)
		le.PutUint64(area[mdSbRecoveryOffsetAt:], n)
	}
	// The role table: entry i holds place i where the member's view records
	// place i as working, and faulty where it does not; the member's own
	// number is its own place, or the entry after the places for a member
	// in none.
	own := fakeRoleOf(sb.role)
	raidDisks := fakeRaidDisks(sb, own)
	devNumber := raidDisks
	if own < mdRoleMax {
		devNumber = int(own)
	}
	le.PutUint32(area[mdSbRaidDisksAt:], uint32(raidDisks))
	le.PutUint32(area[mdSbDevNumberAt:], uint32(devNumber))
	le.PutUint16(area[mdSbBblogSizeAt:], 8)
	le.PutUint32(area[mdSbBblogOffsetAt:], fakeBblSectors)
	if sb.badBlocks > 0 || refused {
		features |= mdFeatureBadBlocks
	}
	le.PutUint32(area[mdSbFeatureMapAt:], features)
	le.PutUint64(area[mdSbEventsAt:], sb.events)
	le.PutUint32(area[mdSbMaxDevAt:], maxDev)
	for i := 0; i < maxDev; i++ {
		role := uint16(mdRoleSpare)
		switch {
		case i < raidDisks && fakeSeesWorking(sb, own, uint16(i)):
			role = uint16(i)
		case i < raidDisks && sb.arrayState != "":
			role = mdRoleFaulty
		}
		le.PutUint16(area[mdSbHeaderLen+2*i:], role)
	}
	le.PutUint16(area[mdSbHeaderLen+2*devNumber:], own)
	le.PutUint32(area[mdSbCsumAt:],
		mdSbChecksum(area[:mdSbHeaderLen+2*maxDev]))
	return area
}

// fakeRoleOf is the role word of a role spelled as mdadm prints it.
func fakeRoleOf(role string) uint16 {
	switch {
	case role == "":
		return 0
	case role == "spare":
		return mdRoleSpare
	case role == "faulty":
		return mdRoleFaulty
	}
	n, err := strconv.Atoi(strings.TrimPrefix(role, "Active device "))
	if err != nil {
		panic("fakeRoleOf: " + role)
	}
	return uint16(n)
}

// fakeBadBlockList renders the window that holds a member's bad-block list:
// an entry of length zero first for a log the kernel refuses, n entries of 8
// sectors, then the end mark.
func fakeBadBlockList(n int, refused bool) []byte {
	le := binary.LittleEndian
	window := make([]byte, mdDirectAlign)
	for i := range window {
		window[i] = 0xff
	}
	at := 0
	if refused {
		le.PutUint64(window[at:], 0)
		at += 8
	}
	for i := 0; i < n; i++ {
		le.PutUint64(window[at:], uint64(4096+8*i)<<10|8)
		at += 8
	}
	return window
}

// readSuperblockArea answers OsClient.ReadBlockDirect, which only the
// reading of a member asks for (Md.ReadMember): the superblock area at
// mdSbOffset — zeros for a member that carries none — and the window of its
// bad-block list. It records "readsb <path> off=<o> len=<l>", and the
// command hooks match that line as they match a command line: a fail hook
// answers an I/O error, as a member whose disk returns errors does, and a
// kill hook a read that did not answer.
func (f *fakeNode) readSuperblockArea(
	ctx context.Context, path string, offset uint64, length uint64,
) ([]byte, error) {
	if err := f.ctxErr(ctx); err != nil {
		return nil, err
	}
	line := fmt.Sprintf("readsb %s off=%d len=%d", path, offset, length)
	f.mu.Lock()
	var hard chan struct{}
	for key, ch := range f.hardGate {
		if strings.Contains(line, key) {
			hard = ch
			break
		}
	}
	f.mu.Unlock()
	if hard != nil {
		// A read of a member whose disk node stalls: it ignores ctx.
		<-hard
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("%s", line)
	f.recordTrace(ctx, line)
	for key := range f.failCmd {
		if strings.Contains(line, key) {
			delete(f.failCmd, key)
			return nil, errors.New("input/output error")
		}
	}
	for key := range f.failCmdAlways {
		if strings.Contains(line, key) {
			return nil, errors.New("input/output error")
		}
	}
	if takeKillNth(f.killCmdNth, line) ||
		takeKill(f.killCmd, f.killCmdAlways, line) {
		return nil, context.DeadlineExceeded
	}
	if _, ok := f.devNo[path]; !ok {
		return nil, fmt.Errorf("open %s: no such device", path)
	}
	switch offset {
	case mdSbOffset:
		if !f.superblocks[path] {
			return make([]byte, length), nil
		}
		return fakeSuperblockArea(f.fakeIdentityOf(path), f.sbFacts[path],
			f.bblRefused[path]), nil
	case (mdSbOffsetSectors + fakeBblSectors) * 512:
		return fakeBadBlockList(f.sbFacts[path].badBlocks,
			f.bblRefused[path]), nil
	}
	return make([]byte, length), nil
}

// mdZeroSuperblock is `mdadm --zero-superblock`: refused for a member an
// array holds, whatever the array's state, with mdadm's own exit status 2. A
// device with no superblock fails with mdadm's words and exit status 2 when
// the zero names the 1.2 format, as Md.ZeroSuperblock's does, and answers
// success with no format named, as the lab's mdadm 4.5 answers both
// (cnagent_integtest.md, Lab facts). A zeroNoEffect member answers success
// with its superblock left; otherwise the superblock and everything it said
// go.
func (f *fakeNode) mdZeroSuperblock(args []string) (string, int) {
	dev := args[len(args)-1]
	for _, array := range f.arrays {
		if contains(array.members, dev) {
			f.dispatchStderr = "mdadm: Couldn't open " + dev +
				" for write - not zeroing"
			return "", 2
		}
	}
	if !f.superblocks[dev] {
		if contains(args, "--metadata=1.2") {
			f.dispatchStderr = "mdadm: No super block found on " + dev +
				" (Expected magic a92b4efc, got 00000000)"
			return "", 2
		}
		f.dispatchStderr = "mdadm: Unrecognised md component device - " + dev
		return "", 0
	}
	if f.zeroNoEffect[dev] {
		return "", 0
	}
	delete(f.superblocks, dev)
	delete(f.memberArray, dev)
	delete(f.sbFacts, dev)
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
	delete(array.memberSlot, member)
	delete(array.unbound, member)
	// md writes the member's superblock as the array's own.
	delete(f.sbFacts, member)
	f.superblocks[member] = true
	if array.uuid != "" {
		f.memberArray[member] = array.uuid
	}
	f.publishArray(array)
	return "", 0
}

// mdFail marks a member faulty, as SET_DISK_FAULTY does: it stays held, and
// its dev-*/state keeps the failfast flag beside the new one. md moves a
// failed member out of its slot once no IO to it is pending, and the fake
// has none, so the slot reads none at once.
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
	if array.memberSlot == nil {
		array.memberSlot = make(map[string]string)
	}
	array.memberSlot[member] = mdSlotNone
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
	delete(array.memberSlot, member)
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
//	/sys/block/{node}/dev                           the array's major:minor
//	/sys/block/{node}/md/array_state                clean / inactive / clear
//	/sys/block/{node}/md/degraded                   running arrays only
//	/sys/block/{node}/md/sync_action                running arrays only
//	/sys/block/{node}/md/sync_completed             running arrays only
//	/sys/block/{node}/md/dev-{kname}                one per member
//	/sys/block/{node}/md/dev-{kname}/state          "in_sync,failfast", …
//	/sys/block/{node}/md/dev-{kname}/slot           "0", "1", or "none"
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
// nvme-subsystem trees, so a listing of /sys/block picks the arrays up
// through children() and anything else a test puts under /sys/block keeps
// listing.
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
// leak a /sys/block entry; the sysfs subtree of the replaced one is dropped.
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
	if array.devNo != "" {
		f.files[sysfsBlockDir+"/"+array.node+"/dev"] = array.devNo + "\n"
	}
	f.dirs[mdDir] = true
	f.files[mdDir+"/array_state"] = array.arrayState + "\n"
	if mdRunning(array.arrayState) {
		f.files[mdDir+"/degraded"] = strconv.Itoa(array.degraded) + "\n"
		f.files[mdDir+"/sync_action"] = array.syncAction + "\n"
		f.files[mdDir+"/sync_completed"] = array.syncCompleted + "\n"
		last := array.lastSyncAction
		if last == "" {
			last = "none"
		}
		f.files[mdDir+"/last_sync_action"] = last + "\n"
		syncMin := array.syncMin
		if syncMin == "" {
			syncMin = "0"
		}
		f.files[mdDir+"/sync_min"] = syncMin + "\n"
	}
	for i, member := range array.members {
		devDir := mdDir + "/dev-" + f.kernelName(member)
		f.dirs[devDir] = true
		if array.unbound[member] {
			// No block link, and an attribute of the member's own reads
			// ENODEV (unbindMember).
			for _, attr := range []string{"/state", "/slot"} {
				f.errFiles[devDir+attr] = &fs.PathError{
					Op: "read", Path: devDir + attr, Err: syscall.ENODEV}
			}
			continue
		}
		f.dirs[devDir+"/block"] = true
		f.files[devDir+"/state"] = f.memberStateOf(array, member) + "\n"
		f.files[devDir+"/slot"] = memberSlotOf(array, member, i) + "\n"
		if mdRunning(array.arrayState) {
			f.files[devDir+"/bad_blocks"] = array.badBlocks[member]
			f.files[devDir+"/unacknowledged_bad_blocks"] =
				array.badBlocks[member]
		}
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

// mdUuidAnswer is a read of an array's md/uuid: the identity of the array
// published under that node — fakeStampedUuid for one a test seeded or
// installed by hand — dashed as the kernel spells it. It is answered on the
// read, not kept among the files, so that the model's directory listings,
// which scan every file, stay as fast as before. Called with f.mu held.
func (f *fakeNode) mdUuidAnswer(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, sysfsBlockDir+"/")
	if !ok {
		return "", false
	}
	node, ok := strings.CutSuffix(rest, "/md/uuid")
	if !ok || !f.dirs[sysfsBlockDir+"/"+node+"/md"] {
		return "", false
	}
	for _, array := range f.arrays {
		if array.node != node {
			continue
		}
		id := array.uuid
		if id == "" {
			id = fakeStampedUuid
		}
		raw := fakeUuidBytes(id)
		return fmt.Sprintf("%x-%x-%x-%x-%x\n", raw[0:4], raw[4:6],
			raw[6:8], raw[8:10], raw[10:16]), true
	}
	return "", false
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

// memberSlotOf is a member's dev-*/slot: the scripted one, else its place
// in the array's member list, the place md gives a member it holds in sync.
func memberSlotOf(array *fakeArray, member string, place int) string {
	if slot, ok := array.memberSlot[member]; ok {
		return slot
	}
	return strconv.Itoa(place)
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
	if action != mdActionIdle && action != "frozen" {
		// md sets last_sync_action when a sync starts.
		array.lastSyncAction = action
	}
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

// setMemberSlot scripts one member's dev-*/slot, verbatim.
func (f *fakeNode) setMemberSlot(dev, member, slot string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, array := f.arrayByDev(dev)
	if array == nil {
		return
	}
	if array.memberSlot == nil {
		array.memberSlot = make(map[string]string)
	}
	array.memberSlot[member] = slot
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
	array.devNo = f.devNo[dev]
	f.devSize[dev] = f.arraySize(members)
	f.publishArray(array)
}

// cmdStat answers the `stat --format "%u %a %F"` of a directory: its owner
// uid, octal mode and file type (dirStat, or a private directory of the
// agent's own user).
func (f *fakeNode) cmdStat(args []string) (string, int) {
	path := args[len(args)-1]
	if !contains(args, "%u %a %F") {
		return "", 1
	}
	if answer, ok := f.dirStat[path]; ok {
		return answer + "\n", 0
	}
	if f.dirs[path] {
		return fmt.Sprintf("%d 700 directory\n", os.Geteuid()), 0
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
			// A device whose mapped_blocks is not zero carries no mapping
			// element: only a header read (--skip-mappings) reads it, and
			// a full dump of it fails its mapping count, as a dump the
			// tests do not script must not be read for one.
			fmt.Fprintf(&sb, "  <device dev_id=\"%d\" mapped_blocks=\"%d\" "+
				"transaction=\"0\" creation_time=\"0\" snap_time=\"0\">\n"+
				"  </device>\n", id, f.thinMapped[f.lastSnapPool][uint32(id)])
		}
	}
	sb.WriteString("</superblock>\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// nvme host + the sysfs tree the leg probe walks
// ---------------------------------------------------------------------------

// The sysfs nvme tree the fake models, where the shared walk reads it
// (agent/nvmehost.go): the class directories of the subsystems and of the
// controllers, and the name of a multipath namespace head inside a
// subsystem's directory.
const (
	sysfsNvmeSubsysDir = "/sys/class/nvme-subsystem"
	sysfsNvmeCtrlDir   = "/sys/class/nvme"
)

var nvmeNsEntryPattern = regexp.MustCompile(`^nvme\d+n\d+$`)

// endpoint is one transport address a controller is connected to.
type endpoint struct {
	trAddr  string
	trSvcId string
}

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
	hostNqn := mdFlagValue(args, "--hostnqn")
	if nqn == "" {
		return "", 3
	}
	// A connect to an endpoint the host already holds a controller of the
	// same host NQN for is refused before it reaches the network: nvme-cli's
	// own "already connected" check, and behind it the kernel's
	// nvme_tcp_existing_controller, which answers EALREADY — the agent never
	// passes --duplicate-connect. A controller being deleted, or dead, no
	// longer counts, and neither does one of another host NQN
	// (nvmf_ctlr_matches_baseopts); a planted one, which carries none,
	// matches any.
	if held := f.subsystems[nqn]; held != nil {
		for _, ctrl := range held.ctrls {
			if ctrl.trAddr == trAddr && ctrl.trSvcId == trSvcId &&
				(ctrl.hostNqn == "" || ctrl.hostNqn == hostNqn) &&
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
		subsys = f.addSubsys(f.nextSubsys, nqn, f.nsHeadAt[nqn])
	}
	f.addCtrl(subsys, trAddr, trSvcId, hostNqn)
	if during := f.duringConnect[key]; during != nil {
		during()
	}
	return "", 0
}

// addSubsys adds the entry nvme-subsys<idx> for nqn, with its subsysnqn and
// its multipath namespace head, whose steps show on the listings of the
// entry's directory that at names when any of them is above 1 (nsHeadAt).
// The next connect of a new NQN takes a number above idx.
func (f *fakeNode) addSubsys(idx int, nqn string, at headStages) *fakeSubsys {
	subsys := &fakeSubsys{idx: idx, nqn: nqn}
	if idx >= f.nextSubsys {
		f.nextSubsys = idx + 1
	}
	f.subsystems[nqn] = subsys
	dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, subsys.idx)
	f.dirs[dir] = true
	f.files[dir+"/subsysnqn"] = nqn + "\n"
	nsIdx := uint32(1)
	if want, ok := f.nsIdxOf[nqn]; ok {
		nsIdx = want
	}
	nsDev := fmt.Sprintf("nvme%dn%d", subsys.idx, nsIdx)
	if slices.Max(at.steps()) > 1 {
		f.pendingHeads[dir] = &pendingHead{
			at: at, nsDev: nsDev, nsIdx: nsIdx}
	} else {
		f.addNsHead(dir, nsDev, nsIdx)
	}
	return subsys
}

// addCtrl adds a live controller of subsys at one endpoint, with its path
// device's ana_state (anaOf, else optimized) and, when hostNqn is set, its
// hostnqn.
func (f *fakeNode) addCtrl(
	subsys *fakeSubsys,
	trAddr, trSvcId, hostNqn string,
) {
	ctrlName := fmt.Sprintf("nvme%d", f.nextCtrl)
	f.nextCtrl++
	ana := "optimized"
	if want, ok := f.anaOf[subsys.nqn+"|"+trAddr+":"+trSvcId]; ok {
		ana = want
	}
	pathDev := fmt.Sprintf("nvme%dc%sn1", subsys.idx,
		strings.TrimPrefix(ctrlName, "nvme"))
	ctrl := &fakeCtrl{
		name: ctrlName, trAddr: trAddr, trSvcId: trSvcId,
		state: "live", anaState: ana, pathDev: pathDev, hostNqn: hostNqn,
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
	if hostNqn != "" {
		f.files[ctrlDir+"/hostnqn"] = hostNqn + "\n"
	}
	f.dirs[ctrlDir+"/"+pathDev] = true
	f.files[ctrlDir+"/"+pathDev+"/ana_state"] = ctrl.anaState + "\n"
	f.scanWc(subsys, ctrl)
}

// scanWc is the kernel's scan of ctrl's namespace for its write cache: the
// path device takes what the target reports now (targetWc), and the
// subsystem's head reads "write back" once any path did, as the head stacks
// its paths' limits and never drops the flag. Nothing for a path with no
// targetWc entry. Called with f.mu held.
func (f *fakeNode) scanWc(subsys *fakeSubsys, ctrl *fakeCtrl) {
	wc, ok := f.targetWc[subsys.nqn+"|"+ctrl.trAddr+":"+ctrl.trSvcId]
	if !ok {
		return
	}
	f.files[sysfsNvmeCtrlDir+"/"+ctrl.name+"/"+ctrl.pathDev+
		"/queue/write_cache"] = wc + "\n"
	nsDev := fmt.Sprintf("nvme%dn1", subsys.idx)
	head := sysfsBlockDir + "/" + nsDev + "/queue/write_cache"
	if strings.TrimSpace(f.files[head]) == "write back" {
		return
	}
	f.files[head] = wc + "\n"
	if devNo := f.devNo["/dev/"+nsDev]; devNo != "" {
		f.files["/sys/dev/block/"+devNo+"/queue/write_cache"] = wc + "\n"
	}
}

// setTargetWc changes what the target of one path reports (targetWc), as a
// disk node does when it puts another device under the path, and nothing the
// host read: that takes a rescan (rescanWc).
func (f *fakeNode) setTargetWc(nqn, trAddr, trSvcId, wc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.targetWc == nil {
		f.targetWc = make(map[string]string)
	}
	f.targetWc[nqn+"|"+trAddr+":"+trSvcId] = wc
}

// rescanWc is a write to /sys/class/nvme/<ctrl>/rescan_controller: the
// controller's namespace scanned again (scanWc). It reports whether path
// was such an attribute. Called with f.mu held.
func (f *fakeNode) rescanWc(path string) bool {
	rest, ok := strings.CutPrefix(path, sysfsNvmeCtrlDir+"/")
	if !ok {
		return false
	}
	name, ok := strings.CutSuffix(rest, "/rescan_controller")
	if !ok {
		return false
	}
	for _, subsys := range f.subsystems {
		for _, ctrl := range subsys.ctrls {
			if ctrl.name == name {
				f.scanWc(subsys, ctrl)
			}
		}
	}
	return true
}

// nvmeNsWriteCache is OsClient.NvmeNsWriteCache, recorded as
// "identify <path>": what targetWc holds for the controller's path, false
// for one with no entry.
func (f *fakeNode) nvmeNsWriteCache(
	ctx context.Context, ctrlPath string, nsid uint32,
) (bool, error) {
	if err := f.ctxErr(ctx); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("identify %s", ctrlPath)
	name := strings.TrimPrefix(ctrlPath, "/dev/")
	for _, subsys := range f.subsystems {
		for _, ctrl := range subsys.ctrls {
			if ctrl.name == name {
				return f.targetWc[subsys.nqn+"|"+ctrl.trAddr+":"+
					ctrl.trSvcId] == "write back", nil
			}
		}
	}
	return false, fmt.Errorf("open %s: %w", ctrlPath, fs.ErrNotExist)
}

// stackWc is a dm device's write cache when its table goes live: "write
// back" when a device its table names reads so, "write through" when the
// devices it names read only that, and no file when none of them has one,
// as before. Called with f.mu held.
func (f *fakeNode) stackWc(devNo, table string) {
	if devNo == "" {
		return
	}
	got := ""
	for _, field := range strings.Fields(table) {
		if !blockDevNoPattern.MatchString(field) {
			continue
		}
		switch strings.TrimSpace(
			f.files["/sys/dev/block/"+field+"/queue/write_cache"]) {
		case "write back":
			got = "write back"
		case "write through":
			if got == "" {
				got = "write through"
			}
		}
	}
	own := "/sys/dev/block/" + devNo + "/queue/write_cache"
	if got == "" {
		delete(f.files, own)
		return
	}
	f.files[own] = got + "\n"
}

// plantSubsys connects nqn at the chosen entry number, the way the kernel
// gives a freed number to the next new subsystem — another cntlr's, or a dn
// agent's on the same machine: the entry nvme-subsys<idx> with its
// subsysnqn, its namespace head and one live controller per endpoint, the
// shape nvmeConnect builds. It runs with f.mu held and never takes it, so an
// afterList or afterRead hook can call it; a test that calls it anywhere
// else holds f.mu around it. The number must be free.
func (f *fakeNode) plantSubsys(idx int, nqn string, eps ...endpoint) {
	dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, idx)
	if _, taken := f.subsystems[nqn]; taken || f.dirs[dir] {
		panic(fmt.Sprintf("fake: plantSubsys(%d, %s): taken", idx, nqn))
	}
	f.dirs[sysfsNvmeSubsysDir] = true
	f.dirs[sysfsNvmeCtrlDir] = true
	subsys := f.addSubsys(idx, nqn, headStages{})
	for _, ep := range eps {
		f.addCtrl(subsys, ep.trAddr, ep.trSvcId, "")
	}
}

// addNsHead publishes a subsystem's whole multipath namespace head: the
// nvmeXnY directory with its nsid, the block device behind it, and its ng
// device.
func (f *fakeNode) addNsHead(dir, nsDev string, nsIdx uint32) {
	for step := range len(headStages{}.steps()) {
		f.addNsHeadStep(dir, nsDev, nsIdx, step)
	}
}

// addNsHeadStep publishes one step of a multipath namespace head, numbered in
// the kernel's order (headStages): 0 the nvmeXnY directory, 1 its nsid, 2
// the block device behind it with its /sys/block entry, 3 its ng device
// beside it in the subsystem directory.
func (f *fakeNode) addNsHeadStep(dir, nsDev string, nsIdx uint32, step int) {
	switch step {
	case 0:
		f.dirs[dir+"/"+nsDev] = true
	case 1:
		f.files[dir+"/"+nsDev+"/nsid"] = fmt.Sprintf("%d\n", nsIdx)
	case 2:
		f.devNo["/dev/"+nsDev] = f.newDevNo()
		f.devSize["/dev/"+nsDev] = 1 << 40
		f.publishBlockDev(nsDev)
		// The head's write cache under its number too (scanWc).
		if wc, ok := f.files[sysfsBlockDir+"/"+nsDev+"/queue/write_cache"]; ok {
			f.files["/sys/dev/block/"+f.devNo["/dev/"+nsDev]+
				"/queue/write_cache"] = wc
		}
	case 3:
		f.dirs[dir+"/"+ngDevOf(nsDev)] = true
	}
}

// ngDevOf is the ng device the kernel adds beside the multipath namespace
// head nsDev: ng<X>n<Y> for nvme<X>n<Y>.
func ngDevOf(nsDev string) string {
	return "ng" + strings.TrimPrefix(nsDev, "nvme")
}

// publishBlockDev publishes /sys/block/{kname}/dev, the number the kernel
// shows for the block device it names kname, from devNo of /dev/{kname}.
func (f *fakeNode) publishBlockDev(kname string) {
	f.dirs[sysfsBlockDir+"/"+kname] = true
	f.files[sysfsBlockDir+"/"+kname+"/dev"] = f.devNo["/dev/"+kname] + "\n"
}

// dropBlockDev takes a block device away by its /dev path: its number, its
// size and its /sys/block entry.
func (f *fakeNode) dropBlockDev(dev string) {
	delete(f.devNo, dev)
	delete(f.devSize, dev)
	root := sysfsBlockDir + "/" + strings.TrimPrefix(dev, "/dev/")
	delete(f.files, root+"/dev")
	delete(f.dirs, root)
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
			delete(f.dirs, dir+"/"+ngDevOf(name))
			f.dropBlockDev("/dev/" + name)
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
		if inTree(ctrlDir, entry) {
			delete(f.dirs, entry)
		}
	}
	for entry := range f.files {
		if inTree(ctrlDir, entry) {
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
	// Every head of the subsystem goes with its last controller.
	for entry := range f.dirs {
		if name, ok := childOf(subsysDir, entry); ok &&
			nvmeNsEntryPattern.MatchString(name) {
			f.dropBlockDev("/dev/" + name)
		}
	}
	for entry := range f.dirs {
		if inTree(subsysDir, entry) {
			delete(f.dirs, entry)
		}
	}
	for entry := range f.files {
		if inTree(subsysDir, entry) {
			delete(f.files, entry)
		}
	}
	delete(f.pendingHeads, subsysDir)
	f.dropBlockDev(fmt.Sprintf("/dev/nvme%dn1", subsys.idx))
	delete(f.subsystems, subsys.nqn)
}

// inTree says whether path is dir or lies under it: nvme1's directory holds
// nothing of nvme10's.
func inTree(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
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
