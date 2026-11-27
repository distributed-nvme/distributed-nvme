// Package cnagent implements the cn policy of cnagent.md, The cn role —
// package `cnagent`: the
// ControllerNodeAgent service — which leg connections, md arrays, thin pools,
// dm and nvmet objects a controller node builds, and when. All mechanism
// (bootstrap, local store, revision gate, locks, ResInfo tracking, the dm /
// nvmet / nvme-host wrappers, the bitmap store) comes from package agent; the
// tools only this role runs — mdadm and thin-provisioning-tools — are wrapped
// here (md.go, thinbm.go) because a wrapper with a single role is role code
// (the split rule of dnagent.md, Scope and placement).
package cnagent

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CnAgentServer serves ControllerNodeAgent for one controller node.
type CnAgentServer struct {
	pb.UnimplementedControllerNodeAgentServer

	oc      common.OsClient
	probeIO LegProbeIO // CN11 probe IO — deliberately NOT via oc (osclient.md, Exported raw helpers and the probe-IO carve-out)
	nf      *common.NameFmt
	cmd     *agent.Cmd
	store   *agent.Store
	dm      *agent.Dm
	nvmet   *agent.Nvmet
	host    *agent.NvmeHost
	md      *Md
	locks   *agent.LockSet

	capacity uint64
	port     agent.PortConf

	// wcache is the agent's write cache loop device (CN15; wcacheDevNo).
	wcache wcacheLoop

	// fenceWatches are CN34's fence watches, one for each primary cntlr that
	// builds legs, by cntlr key, and fenceLoopStarted says the node's watch
	// loop runs; both guarded by mu (keepFenceWatch).
	fenceWatches     map[string]*fenceWatch
	fenceLoopStarted bool
	// strayCut is the cut mode the agent decided at its start (CN33,
	// CN-CM2): set where the start check found no cause, and then every
	// converge cuts and every read reads the stray connections of the
	// subsystems it steps; clear where it found one and
	// --ignore-host-disconnection let the agent run, and then nothing reads
	// the debug directory. It never changes while the agent runs.
	strayCut bool

	// mu guards the in-memory mirrors of the local store below — the two
	// maps — and what mirrors nothing: in each cntlr's state the prober and
	// path-watcher (cutters) registries, in each
	// CN's and each cntlr's state its observation slot (obs), the converge
	// marks (marks) and the CN10 disconnect registry. It does not guard the
	// request each state holds, which is an atomic pointer (cnState.req,
	// cntlrState.req). It is a leaf lock: never held across an OS call.
	mu     sync.Mutex
	cns    map[string]*cnState
	cntlrs map[string]*cntlrState
	// cntlrFloors are, by cntlr key, the revisions of the cntlr files the
	// startup pass decoded but skipped with their CN, whose cn file did not
	// load (CN2): the floor of the revision gate of a cntlr the process does
	// not hold, until a request of it is accepted (cntlrFloor; CN8).
	cntlrFloors map[string]uint64
	// marks are the converges in flight, by CN key and cntlr key, and the
	// SyncupCn calls in flight that carry a request newer than the CN's held
	// one, save one that only adds cntlrs (syncupCnAdds), or for a CN the
	// agent does not hold (dnagent.md SH30): what keeps
	// a round from starting a read beside a converge, and the converge start
	// a reply reports.
	marks *agent.ConvergeMarks
	// roundReads says whether a check round starts the background read of
	// its object (dnagent.md SH29). NewCnAgentServer sets it; it is a field
	// so unit tests can keep rounds from starting reads of their own, which
	// would race the test's own steps, and run the read themselves
	// (refreshCn, refreshCntlr).
	roundReads bool
	// reads counts the background reads rounds have started and that have
	// not ended. Nothing in production waits on it: the cn joins none of its
	// background tasks, and a cancelled read stores nothing (SH29). Unit
	// tests join it.
	reads sync.WaitGroup
	// disconnecting is the CN10 disconnect registry: the subsystem NQNs whose
	// sweep `nvme disconnect` is running in the background right now, or
	// queued for one of disconnectSlots (startDisconnect). An entry lives
	// exactly as long as its goroutine; it is never persisted.
	disconnecting map[string]struct{}
	// disconnectSlots caps the registry's disconnects that run at once at
	// disconnectConcurrency: a goroutine holds one of its buffer slots for
	// the whole `nvme disconnect`. It is a field so a test can shrink it.
	disconnectSlots chan struct{}
	// connectSlots are the agent's connect places (CN10, connectWithin):
	// each connect attempt of a pass, its dial and its `nvme connect`,
	// holds one of connectConcurrency buffer slots, taken before any
	// OsClient slot. It is a field so a test can read it.
	connectSlots chan struct{}

	// thinSlots are the places of the thin-pool metadata checks that run at
	// once (startThinCheck), common.ThinCheckConcurrency of them, each run
	// holding one while its thin_check runs. thinBg enrolls every run, under
	// thinMu, and WaitBackground joins them once thinClosed, set under
	// thinMu, keeps new runs from starting (dnagent.md SH27).
	// thinCheckBackoff is how long a slice whose run did not pass waits
	// before the next run; a field, so a test can shorten it.
	thinSlots        chan struct{}
	thinBg           sync.WaitGroup
	thinMu           sync.Mutex
	thinClosed       bool
	thinCheckBackoff time.Duration

	// rootCtx is the process lifetime ctx, captured by Prepare before the
	// gRPC listener opens, or at Reconcile when nothing prepared the server
	// (a test); the CN10 background disconnects and path watchers, the CN11
	// probers and the background reads (SH29) hang off it, so shutdown
	// cancels them. rootSet says Prepare set it.
	rootCtx context.Context
	rootSet bool
	// startup is the agent's startup state (dnagent.md SH33), and bootId
	// the kernel's boot id Prepare read, which the startup pass saves as it
	// ends.
	startup agent.Startup
	bootId  string

	// probeInterval / probeStall are fields rather than constants, and now
	// and sleep — like probeIO above — are overridable, so tests can drive
	// the CN11 prober on a short fake clock and off the real syscalls, and
	// run a converge pass's CN10 connect budget (newPassBudget) on a fake
	// clock too; nothing else changes them.
	probeInterval time.Duration
	// fenceInterval is the period of CN34's fence watch loop
	// (common.CnFenceCheckInterval), a field so a test can lengthen it.
	fenceInterval time.Duration
	probeStall    time.Duration
	now           func() time.Time
	sleep         func(context.Context, time.Duration) error
}

// cnState is one synced CN: its last accepted SyncupCnRequest as this process
// holds it (SH8) — a SyncupCn stores it once it has passed the revision gate
// and its save has succeeded (dnagent.md SH5), before its converge runs —
// plus the ResInfo history of the base state (architecture.md, Controller
// node, common), and its latest observation.
type cnState struct {
	// req is read and written through loadReq and storeReq only. It is
	// stored under the node write lock, or under the node read lock and
	// addsMu by a SyncupCn that only adds cntlrs (syncupCnAdds), so a
	// reader under the node read lock can see it change: it is an atomic
	// pointer, like cntlrState.req, and a reader that needs it to stay the
	// same across several reads loads it once (loadReq).
	req     atomic.Pointer[pb.SyncupCnRequest]
	tracker *agent.ResTracker
	// addsMu keeps two SyncupCn that only add cntlrs apart, which run under
	// the node read lock (syncupCnAdds, cnagent.md CN1): each runs the gate,
	// the save and the store of its request as one. A syncup under the node
	// write lock keeps every one of them out already.
	addsMu sync.Mutex
	// obs is the CN's latest observation and its running read (dnagent.md
	// SH28, SH29), guarded by the server's mu.
	obs agent.ObsSlot[*pb.CnInfo]
}

func newCnState(req *pb.SyncupCnRequest) *cnState {
	st := &cnState{tracker: agent.NewResTracker()}
	st.storeReq(req)
	return st
}

// loadReq is the CN's current request. A request is never modified once
// stored, so a caller may read it for as long as it likes; one that needs
// the request to stay the same across several reads loads it once.
func (st *cnState) loadReq() *pb.SyncupCnRequest {
	return st.req.Load()
}

func (st *cnState) storeReq(req *pb.SyncupCnRequest) {
	st.req.Store(req)
}

// cntlrState is one synced cntlr.
type cntlrState struct {
	// req is the cntlr's current desired state, read and written through
	// loadReq and storeReq only. A SyncupCntlr stores it under the node read
	// lock and this cntlr's object lock, while other goroutines read it
	// holding neither that object lock nor the node write lock: every other
	// cntlr's converge and read asks every cntlr of the CN whether its
	// request still claims a host-facing subsystem or a clone source
	// (hostFacingClaim, srcNqnsInUse), and the node-level verdict of a CN's
	// read reads every cntlr's host-facing subsystems and clone list
	// (hostFacingClaim, srcNqnsInUse) —
	// cntlrKeysOf reads every cntlr's ids too. So it is an atomic pointer: a
	// lock-free read cannot race the store, and taking no lock for it adds no
	// lock order. A request is never modified once stored. reqFromRpc, which
	// changes with it and on a refused save, needs none of that: only this
	// cntlr's own converges and reads read it, and each holds this cntlr's
	// object lock or the node write lock, either of which orders it after
	// the write.
	req     atomic.Pointer[pb.SyncupCntlrRequest]
	tracker *agent.ResTracker

	// chunks mirrors the clone-bm-* files of this cntlr that this process
	// loaded at startup or received since, one CloneChunkSet per clone keyed
	// by the pair (src_slice_idx, bm_idx); the files stay the source of
	// truth for the applied set (SH21). A cntlr rebuilt after CN2's reload
	// skipped its files starts empty over them until the worker pushes them
	// again.
	chunks map[uint64]*agent.CloneChunkSet

	// pendingSweep marks slices whose pool device THIS incarnation created
	// but has not yet successfully swept for orphan thin ids (CN14's
	// activation sweep). Keyed by slice_id; in-memory only — a crash in the
	// window leaves a stray for the next sweep of the pool's ids that gets
	// through. The startup Reconcile's Create branch arms it too; running
	// additionally requires reqFromRpc.
	pendingSweep map[uint64]bool
	// tornTds marks the snapshot tds this cntlr tore (CN14): one run of the
	// snapshot pre-pass made the td's id in one slice while another slice
	// already held it, or made ids in two slices without the origin's
	// quiesce (snapshotPrePass). Keyed by td_id. The pre-pass saves a new
	// mark in the local store at once (saveTornMarks), and the startup
	// reconcile loads the saved marks (CN2), so an agent restart keeps them.
	// A pre-pass sets a mark and nothing but the request clears it: every
	// converge drops the marks of tds its request no longer lists or lists
	// torn (keepTornMarks), the worker having recorded the mark by then
	// (dnv-worker.md RW19). Both channels report a mark in the td's thin
	// info, in any role (tornMark). It is read and written under this
	// cntlr's object lock or the node write lock, as pendingSweep is.
	tornTds map[uint64]bool
	// forcedStarts holds, per group id, the revision of the forced-start
	// mark whose forced start this incarnation ran its assembly for, the
	// acted mark (CN12). In memory only: an agent restart forgets it, and
	// the mark then waits for the operator's cancel (cnagent.md, Known
	// limits). A forced start that runs its assembly sets it
	// (grpPlan.forcedRan), and one whose stop to drop the named leg's
	// bad-block list failed drops it (grpPlan.forcedListKept), since that
	// start is not done; nothing else but the request clears it: every
	// converge drops the entry of a group whose request carries no mark or a
	// mark of another revision (keepForcedStarts). Both channels report it in
	// the group's reading of a running array while the request, counting as
	// delivered, carries a mark of that revision (actedMark). It is read and
	// written under this cntlr's object lock or the node write lock, as
	// tornTds is.
	forcedStarts map[uint64]uint64
	// mdChecks holds, per group id, the md check this incarnation started,
	// or adopted running, for a request of the group (Group.md_check), and
	// whether it saw that check end (CN12, The md check). In memory only: an
	// agent restart forgets it, and a check md still runs is then adopted
	// again, while one that ended is started again. A converge drops the
	// entry of a group whose array does not run or that the request no
	// longer lists for a primary (keepMdChecks, stepMdCheck), and both
	// channels mark an entry done (noteMdCheck) and report it
	// (mdCheckDone). It is read and written under this cntlr's object lock
	// or the node write lock, as forcedStarts is.
	mdChecks map[uint64]mdCheckRun
	// recopies holds, per leg id, the revision of the recopy mark
	// (Leg.recopy_revision) whose recopy this incarnation ran: the leg's
	// member failed, removed, its md superblock zeroed and the leg added
	// fresh (CN12, The recopy). In memory only: an agent restart forgets
	// it, and the next converge copies the leg once more, which a member in
	// sync makes cost time alone. Every converge drops the entry of a leg
	// whose request carries no mark or a mark of another revision
	// (keepRecopies); both channels report an entry while the request's
	// mark carries its revision (recopiedLegs). It is read and written
	// under this cntlr's object lock or the node write lock, as
	// forcedStarts is.
	recopies map[uint64]uint64
	// thinChecks holds, per slice id, the background check of the slice's
	// thin-pool metadata this incarnation started because the metadata
	// carries dm-thin's needs-check mark (CN13, The needs-check mark;
	// thinCheckGate). In memory only: an agent restart forgets it, and the
	// next converge that finds the mark starts the check again. A converge
	// drops the entry once it finds the mark gone, and stops and drops the
	// run of a slice whose pool it does not build (keepThinChecks). It is read
	// and written under this cntlr's object lock or the node write lock, as
	// tornTds is; each run's own fields under its mutex.
	thinChecks map[uint64]*thinCheckRun
	// reqFromRpc says whether req counts as delivered (CN8): a
	// revision-gated SyncupCntlr of THIS incarnation stored it, whose save
	// held, and no SyncupCntlr newer than it has been refused for a failed
	// save since (dnagent.md SH5). syncupCntlr sets it with each request it
	// stores and clears it when it refuses a newer request for a failed
	// save; a request at the held revision refused so leaves it as it is.
	// The startup Reconcile converges from the persisted copy, which can
	// carry a forced-start mark the worker has cleared since, whose clear
	// this agent never accepted (the clear never arrived, say, or its save
	// failed), and a held request whose newer successor was refused can
	// carry a mark the refused request clears or withdraws, so a converge
	// acts on a mark only while this is set, and a read names a mark while
	// it is not (CN12; dropForcedStartMarks, storedMarks). CN14's thin id
	// sweeps, which delete ids absent from td_list, run only while it is
	// set as well. The cntlr's reads read it too, each under this cntlr's
	// object lock, which orders it after the write as well.
	reqFromRpc bool

	// probers are the CN11 leg health probers, keyed by leg_id. They hold no
	// lock; every converge cancels those its plan no longer probes
	// (convergeCntlr), and the cntlr's drop cancels the rest (CN7).
	probers map[uint64]*legProber
	// cutters are CN10's path watchers of the legs whose path the primary
	// may cut, keyed by leg_id (keepPathWatchers). Like the probers they
	// hold no lock; every converge keeps them in step with its plan, and
	// the cntlr's drop stops them all.
	cutters map[uint64]*pathWatcher

	// obs is the cntlr's latest observation and its running read
	// (dnagent.md SH28, SH29), guarded by the server's mu.
	obs agent.ObsSlot[*pb.CntlrInfo]

	// wcStale holds, per leg id, the legs whose write cache a primary's
	// reads look at again (CN10, The write cache of a leg's path; wcMark):
	// one a converge or a read left stale, a path or the leg's wrapper, and
	// one with a live path that is not optimized, which its disk node can
	// open over another device with no converge of this cntlr. A read that
	// finds neither drops the entry (legWcacheVerdict), and every converge
	// sets the map anew for its own legs, empty on a standby. In memory
	// only: the startup reconcile's converge reads every leg again. It is
	// read and written under this cntlr's object lock or the node write
	// lock, as tornTds is.
	wcStale map[uint64]wcMark

	// stackTerm is the term the cntlr's primary stack was built under, zero
	// for none or for a stack built under no term (CN35; saveStackTerm),
	// kept in the local store and loaded at the start. It is read and
	// written under this cntlr's object lock or the node write lock, as
	// tornTds is.
	stackTerm uint64
	// stackTermSaved says the file holds stackTerm (saveStackTerm); true
	// for a cntlr loaded with its file or with none and no term.
	stackTermSaved bool

	// fenced is the cntlr's fence flag (CN34): set by the fence watch as it
	// moves the namespaces for the fence it read, and by a converge that
	// reads the fence itself (fenceAfterLegs); dropped when the watch reads a
	// leg path optimized and when a converge's plan gives the watch no leg:
	// the cntlr leaves the primary role, its level builds no leg, or every
	// leg it builds provisions (keepFenceWatch, fenceInputs). Every plan of the cntlr reads it once (applyFence).
	// Atomic: the watch drops it holding no lock.
	fenced atomic.Bool
}

// NewCnAgentServer builds the cn role server. oc is the process's single
// OsClient, localStore the --local-store prefix (the same one nf was built
// with), capacity the --capacity budget, trConf the transport of this
// agent's nvmet port, portId the --nvmet-port-id it converges
// (common.NvmetPortId unless the flag says otherwise) and qidMax the
// --nvmet-qid-max IO queue cap of the subsystems it links (dnagent.md SH32;
// 0 writes none), and strayCut the cut mode the start check decided
// (CN33, CN-CM2).
func NewCnAgentServer(
	oc common.OsClient,
	nf *common.NameFmt,
	localStore string,
	capacity uint64,
	trConf *pb.NvmeTrConf,
	portId int,
	qidMax int,
	strayCut bool,
) *CnAgentServer {
	return &CnAgentServer{
		oc: oc,
		// The probers get the direct-syscall implementation, never a wrapper
		// over oc (osclient.md, Exported raw helpers and the probe-IO
		// carve-out); tests swap the field after construction.
		probeIO:  directLegProbeIO{},
		nf:       nf,
		cmd:      agent.NewCmd(oc),
		store:    agent.NewStore(oc, localStore),
		dm:       agent.NewDm(oc),
		nvmet:    agent.NewNvmet(oc),
		host:     agent.NewNvmeHost(oc),
		md:       NewMd(oc),
		locks:    agent.NewLockSet(),
		capacity: capacity,
		strayCut: strayCut,
		port: agent.PortConf{
			PortId:  portId,
			TrType:  trConf.GetTrType(),
			AdrFam:  trConf.GetAdrFam(),
			TrAddr:  trConf.GetTrAddr(),
			TrSvcId: trConf.GetTrSvcId(),
			QidMax:  qidMax,
		},
		cns:             make(map[string]*cnState),
		cntlrs:          make(map[string]*cntlrState),
		cntlrFloors:     make(map[string]uint64),
		fenceWatches:    make(map[string]*fenceWatch),
		marks:           agent.NewConvergeMarks(),
		roundReads:      true,
		disconnecting:   make(map[string]struct{}),
		disconnectSlots: make(chan struct{}, disconnectConcurrency),
		connectSlots:    make(chan struct{}, connectConcurrency),
		thinSlots: make(chan struct{},
			common.ThinCheckConcurrency),
		thinCheckBackoff: common.ThinCheckRetrySeconds * time.Second,
		rootCtx:          context.Background(),
		probeInterval:    common.CnLegProbeInterval * time.Second,
		fenceInterval:    common.CnFenceCheckInterval,
		probeStall:       common.CnLegProbeStallSeconds * time.Second,
		now:              time.Now,
		sleep:            agent.SleepCtx,
	}
}

// ---------------------------------------------------------------------------
// Object keys and state lookup
// ---------------------------------------------------------------------------

func cnKey(clusterId, cnId uint64) string {
	return fmt.Sprintf("%016x-%016x", clusterId, cnId)
}

// cntlrKey is the CN1 object key: (cluster_id, cn_id, sp_id, cntlr_id).
func cntlrKey(clusterId, cnId, spId, cntlrId uint64) string {
	return fmt.Sprintf(
		"%016x-%016x-%016x-%016x", clusterId, cnId, spId, cntlrId)
}

func cntlrPointerText(ptr *pb.CntlrPointer) string {
	return fmt.Sprintf("(sp %d, cntlr %d)", ptr.GetSpId(), ptr.GetCntlrId())
}

func (s *CnAgentServer) getCn(key string) *cnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cns[key]
}

// putCn and putCntlr store a state under its key. A state they replace —
// the startup reconcile builds every state afresh — has its read cancelled,
// as a dropped one has (dropCntlr).
func (s *CnAgentServer) putCn(key string, st *cnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.cns[key]; old != nil && old != st {
		old.obs.CancelReadLocked()
	}
	s.cns[key] = st
}

func (s *CnAgentServer) getCntlr(key string) *cntlrState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cntlrs[key]
}

func (s *CnAgentServer) putCntlr(key string, st *cntlrState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.cntlrs[key]; old != nil && old != st {
		old.obs.CancelReadLocked()
	}
	s.cntlrs[key] = st
}

// dropCntlr forgets a cntlr's state, and cancels its running read, if any,
// waiting for nothing: the read finds the cntlr gone and stores nothing
// (dnagent.md SH30).
func (s *CnAgentServer) dropCntlr(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.cntlrs[key]; st != nil {
		st.obs.CancelReadLocked()
	}
	delete(s.cntlrs, key)
}

func (s *CnAgentServer) cnKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.cns))
	for key := range s.cns {
		keys = append(keys, key)
	}
	return keys
}

func (s *CnAgentServer) allCntlrKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.cntlrs))
	for key := range s.cntlrs {
		keys = append(keys, key)
	}
	return keys
}

// cntlrKeysOf lists the cntlrs currently belonging to one CN.
func (s *CnAgentServer) cntlrKeysOf(clusterId, cnId uint64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key, st := range s.cntlrs {
		req := st.loadReq()
		if req.GetClusterId() == clusterId && req.GetCnId() == cnId {
			keys = append(keys, key)
		}
	}
	return keys
}

func newCntlrState(req *pb.SyncupCntlrRequest) *cntlrState {
	// reqFromRpc stays false: a state born here may equally well come from
	// the startup Reconcile's persisted copy, and only syncupCntlr knows
	// otherwise.
	st := &cntlrState{
		tracker:      agent.NewResTracker(),
		chunks:       make(map[uint64]*agent.CloneChunkSet),
		pendingSweep: make(map[uint64]bool),
		tornTds:      make(map[uint64]bool),
		forcedStarts: make(map[uint64]uint64),
		mdChecks:     make(map[uint64]mdCheckRun),
		recopies:     make(map[uint64]uint64),
		thinChecks:   make(map[uint64]*thinCheckRun),
		probers:      make(map[uint64]*legProber),
		cutters:      make(map[uint64]*pathWatcher),
		// No term and no file: in step until a save.
		stackTermSaved: true,
	}
	st.storeReq(req)
	return st
}

// loadReq is the cntlr's current request; see cntlrState.req. A caller that
// needs the request to stay the same across several reads loads it once.
func (st *cntlrState) loadReq() *pb.SyncupCntlrRequest {
	return st.req.Load()
}

func (st *cntlrState) storeReq(req *pb.SyncupCntlrRequest) {
	st.req.Store(req)
}

// chunkSet returns (creating on first use) the CloneChunkSet of one clone.
func (st *cntlrState) chunkSet(cloneId uint64) *agent.CloneChunkSet {
	set, ok := st.chunks[cloneId]
	if !ok {
		set = agent.NewCloneChunkSet()
		st.chunks[cloneId] = set
	}
	return set
}

// pointerKnown reports whether a cntlr pointer is in the CN's authoritative
// list — what makes a cntlr known to the agent at all (CN7, CN8).
func pointerKnown(req *pb.SyncupCnRequest, ptr *pb.CntlrPointer) bool {
	for _, known := range req.GetCntlrPointerList() {
		if known.GetSpId() == ptr.GetSpId() &&
			known.GetCntlrId() == ptr.GetCntlrId() {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// RPC entry points — locking per CN1, the work in the syncup_*/check/probe
// files.
// ---------------------------------------------------------------------------

// GetCnSize replies the --capacity flag verbatim (CN3). 0 means "no local
// opinion" and the CP substitutes DefaultCnCap; the agent never validates or
// clamps. It also replies the machine's host name, read on every call
// (dnagent.md SH34), its one OS access. No locks, no store — and no
// AgentReply: it cannot fail, since a failed host name read leaves the name
// empty.
func (s *CnAgentServer) GetCnSize(
	ctx context.Context,
	req *pb.GetCnSizeRequest,
) (*pb.GetCnSizeReply, error) {
	return &pb.GetCnSizeReply{
		Size:     s.capacity,
		HostName: agent.HostName(ctx, s.oc),
	}, nil
}

// SyncupCn marks its converge as it arrives, before it waits for its node
// lock (dnagent.md SH30). A newer request that only adds cntlrs to the CN's
// held one (addsCntlrsOnly) marks the CN alone, cancels the CN's own read
// alone and runs under the node read lock (syncupCnAdds, cnagent.md CN1),
// so it holds off no cntlr's read and waits for no converge in flight, save
// behind a syncup that asked for the write lock first; one
// that is no longer such a request once it holds that lock runs as below,
// under the write lock with the CN's mark alone. Any other request newer
// than the CN's held one, or for a CN the agent does not hold, marks the
// node too and cancels every read of the agent: the write lock waits for
// every holder of its read half, every cntlr's read among them, and the
// node's mark keeps a round from starting a new one until the syncup ends.
// A re-send at the held revision,
// which a CN-level leftover brings every round, cancels the CN's read alone
// and waits for the cntlr reads in flight, so it keeps no cntlr's read from
// ending. A request the revision gate refuses against the CN's held request
// marks nothing and cancels nothing, so a stale request the worker re-sends
// every round keeps no read from ending; the gate refuses it under the write
// lock. While the store's latest save failed, a request the gate admits
// marks the CN alone and cancels nothing, since a request refused for a
// failed save (dnagent.md SH5) comes back every round as well
// (markNodeSyncup).
func (s *CnAgentServer) SyncupCn(
	ctx context.Context,
	req *pb.SyncupCnRequest,
) (*pb.SyncupCnReply, error) {
	if s.startup.Starting() {
		// dnagent.md SH33: refused, not applied, while the pass runs.
		return &pb.SyncupCnReply{
			AgentReply: agent.StartingReply(),
			ObsStamp:   s.startingStamp(s.now()),
		}, nil
	}
	adds, end := s.markNodeSyncup(req)
	defer end()
	if adds {
		if reply := s.syncupCnAdds(ctx, req); reply != nil {
			return reply, nil
		}
	}
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()
	return s.syncupCn(ctx, req), nil
}

// SyncupCntlr marks its cntlr's converge in flight and cancels the cntlr's
// running read before it takes a lock (dnagent.md SH30), save a request CN8's
// gates refuse against the cntlr's held request, which marks nothing and
// cancels nothing, so a refused request the worker re-sends every round
// keeps no read from ending; the gates refuse it under the object lock.
// While the store's latest save failed, a request the gates admit marks the
// cntlr and cancels nothing, since a request refused for a failed save
// (dnagent.md SH5) comes back every round as well, and its object lock
// waits for the cntlr's read (markSyncup).
//
// It takes the cntlr's object lock only for a cntlr this process holds or a
// request CN8's gates admit: a request they refuse for a cntlr it does not
// hold is answered under the node read lock alone, so it leaves no lock
// entry behind (dnagent.md, SH11). syncupCntlr runs every gate again under
// the object lock. A request the gates admit whose save then fails, for a
// cntlr the process does not hold, leaves the entry it created: the request
// sent again reuses it, and the CN7 pointer diff drops it once the cntlr's
// pointer leaves the CN's list (dropRemovedCntlrs), as the teardown of a
// held cntlr drops its own (dropCntlrState).
func (s *CnAgentServer) SyncupCntlr(
	ctx context.Context,
	req *pb.SyncupCntlrRequest,
) (*pb.SyncupCntlrReply, error) {
	if s.startup.Starting() {
		// dnagent.md SH33: refused, not applied, while the pass runs.
		return &pb.SyncupCntlrReply{
			AgentReply: agent.StartingReply(),
			ObsStamp:   s.startingStamp(s.now()),
		}, nil
	}
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	defer s.markSyncup(key, req)()
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getCntlr(key) == nil {
		if reply := s.gateCntlr(ctx, req, s.cntlrFloor(key)); reply != nil {
			return reply, nil
		}
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.syncupCntlr(ctx, key, req), nil
}

// cntlrFloor is the revision a request for the cntlr key must reach while
// the process does not hold the cntlr: the revision of its cntlr file that
// the startup pass decoded and skipped with its CN (cntlrFloors; CN2, CN8),
// zero for any other.
func (s *CnAgentServer) cntlrFloor(key string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cntlrFloors[key]
}

// setCntlrFloor raises the floor of the cntlr key to revision (CN2).
func (s *CnAgentServer) setCntlrFloor(key string, revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision > s.cntlrFloors[key] {
		s.cntlrFloors[key] = revision
	}
}

// dropCntlrFloor ends the floor of the cntlr key once a request of it is
// accepted, which the held request's revision gates from then on (CN8).
func (s *CnAgentServer) dropCntlrFloor(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cntlrFloors, key)
}

// PushCloneBitmap refuses a push for a cntlr this process does not hold under
// the node read lock alone, so the refusal leaves no lock entry behind
// (dnagent.md, SH11); pushCloneBitmap checks the cntlr again under its object
// lock. It cancels no read: it waits for one as it waits for a converge
// (dnagent.md SH30).
func (s *CnAgentServer) PushCloneBitmap(
	ctx context.Context,
	req *pb.PushCloneBitmapRequest,
) (*pb.PushCloneBitmapReply, error) {
	if s.startup.Starting() {
		// dnagent.md SH33: refused, not applied, while the pass runs.
		return &pb.PushCloneBitmapReply{AgentReply: agent.StartingReply()},
			nil
	}
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	if s.getCntlr(key) == nil {
		return unknownCntlrPush(req), nil
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.pushCloneBitmap(ctx, key, req), nil
}

// GetCnInfo answers from the CN's latest observation, as a CheckCn round
// does, and starts no read (dnagent.md SH28): it takes no lock and reads
// nothing of the node (CN23).
func (s *CnAgentServer) GetCnInfo(
	ctx context.Context,
	req *pb.GetCnInfoRequest,
) (*pb.GetCnInfoReply, error) {
	// Starting is read before the observation: the pass ends the start
	// after it stored its last observation, so a call that finds the
	// start ended reads every observation the pass made (dnagent.md SH33).
	if s.startup.Starting() {
		return &pb.GetCnInfoReply{
			AgentReply: agent.StartingReply(),
			ObsStamp:   s.startingStamp(s.now()),
		}, nil
	}
	obs, since, known := s.answerCn(ctx,
		cnKey(req.GetClusterId(), req.GetCnId()), false)
	now := s.now()
	if !known {
		return &pb.GetCnInfoReply{
			AgentReply: unknownCn(req.GetCnId()),
			ObsStamp:   s.startup.Stamped((&cnObs{}).Stamp(now, time.Time{})),
		}, nil
	}
	return &pb.GetCnInfoReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		CnInfo:     obs.Info,
		ObsStamp:   s.startup.Stamped(obs.Stamp(now, since)),
	}, nil
}

// GetCntlrInfo answers from the cntlr's latest observation, as a CheckCntlr
// round does, and starts no read (dnagent.md SH28): it takes no lock, so a
// read of an unknown cntlr leaves no lock entry (dnagent.md, SH11), and reads
// nothing of the node (CN23), but for the devices' counters a request with
// read_io_stats asks for (cntlrIoStats).
func (s *CnAgentServer) GetCntlrInfo(
	ctx context.Context,
	req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	// Starting is read before the observation: the pass ends the start
	// after it stored its last observation, so a call that finds the
	// start ended reads every observation the pass made (dnagent.md SH33).
	if s.startup.Starting() {
		return &pb.GetCntlrInfoReply{
			AgentReply: agent.StartingReply(),
			ObsStamp:   s.startingStamp(s.now()),
		}, nil
	}
	obs, since, known := s.answerCntlr(ctx, key, false)
	now := s.now()
	if !known {
		return &pb.GetCntlrInfoReply{
			AgentReply: unknownCntlr(req.GetCntlrPointer()),
			ObsStamp:   s.startup.Stamped((&cntlrObs{}).Stamp(now, time.Time{})),
		}, nil
	}
	reply := &pb.GetCntlrInfoReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		CntlrInfo:  obs.Info,
		ObsStamp:   s.startup.Stamped(obs.Stamp(now, since)),
	}
	// CN23's one exception: the flagged call, which only InspectCntlr
	// makes, reads the devices' counters at the call, after the answer
	// above is taken and holding no lock of CN1.
	if req.GetReadIoStats() {
		if st := s.getCntlr(key); st != nil {
			reply.IoStats = s.cntlrIoStats(ctx, st, now)
		}
	}
	return reply, nil
}

// startingStamp is the stamp of a reply the agent gives while its startup
// pass runs (dnagent.md SH33): no observation, the answer's time and the
// agent's start.
func (s *CnAgentServer) startingStamp(now time.Time) *pb.ObsStamp {
	return s.startup.Stamped(&pb.ObsStamp{AnswerTime: agent.Timestamp(now)})
}

// unknownCn and unknownCntlr are the refusals of a round or an info read of
// an object this process holds no request for (CN23).
func unknownCn(cnId uint64) *pb.AgentReply {
	return agent.UnknownObjectReply("unknown cn %d", cnId)
}

func unknownCntlr(ptr *pb.CntlrPointer) *pb.AgentReply {
	return agent.UnknownObjectReply(
		"unknown cntlr %s", cntlrPointerText(ptr))
}

// GetCnStore answers the desired state the agent holds (CN32): for each CN
// of the request's cluster it holds — the one cn_id names, or every one for
// a zero cn_id — the held SyncupCnRequest's revision and pointer list, and
// a summary of each held SyncupCntlrRequest that list names. It reads the
// requests the states hold, which are atomic pointers, under the leaf mutex
// alone, takes no lock of CN1, reads nothing of the node and changes
// nothing. A pointer whose cntlr the agent holds no request of, as when the
// startup reload skipped its file (CN2), is in the pointer list only. While
// the agent starts it answers ReplyCodeStarting, since the reload is still
// filling the states (dnagent.md SH33); with no CN of the cluster held it
// answers ReplyCodeUnknownObject.
func (s *CnAgentServer) GetCnStore(
	ctx context.Context,
	req *pb.GetCnStoreRequest,
) (*pb.GetCnStoreReply, error) {
	if s.startup.Starting() {
		return &pb.GetCnStoreReply{AgentReply: agent.StartingReply()}, nil
	}
	cnReqs, cntlrReqs := s.heldRequests(req.GetClusterId(), req.GetCnId())
	if len(cnReqs) == 0 {
		return &pb.GetCnStoreReply{
			AgentReply: agent.UnknownObjectReply(
				"no cn of cluster %d held", req.GetClusterId()),
		}, nil
	}
	slices.SortFunc(cnReqs, func(a, b *pb.SyncupCnRequest) int {
		return cmp.Compare(a.GetCnId(), b.GetCnId())
	})
	reply := &pb.GetCnStoreReply{AgentReply: agent.OkReply()}
	for _, cnReq := range cnReqs {
		stored := &pb.GetCnStoreReply_StoredCn{
			CnId:             cnReq.GetCnId(),
			Revision:         cnReq.GetRevision(),
			CntlrPointerList: cnReq.GetCntlrPointerList(),
		}
		for _, ptr := range cnReq.GetCntlrPointerList() {
			cntlrReq := cntlrReqs[cntlrKey(cnReq.GetClusterId(),
				cnReq.GetCnId(), ptr.GetSpId(), ptr.GetCntlrId())]
			if cntlrReq != nil {
				stored.CntlrList = append(stored.CntlrList,
					storedCntlr(cntlrReq))
			}
		}
		reply.CnList = append(reply.CnList, stored)
	}
	return reply, nil
}

// heldRequests is the requests of the CNs of one cluster the agent holds,
// one CN's or every one's for a zero cnId, and of their cntlrs by cntlr key,
// read under the leaf mutex.
func (s *CnAgentServer) heldRequests(
	clusterId uint64,
	cnId uint64,
) ([]*pb.SyncupCnRequest, map[string]*pb.SyncupCntlrRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cnReqs []*pb.SyncupCnRequest
	held := map[uint64]bool{}
	for _, st := range s.cns {
		cnReq := st.loadReq()
		if cnReq.GetClusterId() != clusterId ||
			(cnId != 0 && cnReq.GetCnId() != cnId) {
			continue
		}
		cnReqs = append(cnReqs, cnReq)
		held[cnReq.GetCnId()] = true
	}
	cntlrReqs := map[string]*pb.SyncupCntlrRequest{}
	for key, st := range s.cntlrs {
		cntlrReq := st.loadReq()
		if cntlrReq.GetClusterId() == clusterId && held[cntlrReq.GetCnId()] {
			cntlrReqs[key] = cntlrReq
		}
	}
	return cnReqs, cntlrReqs
}

// storedCntlr sums up one held cntlr request for GetCnStore: its pointer,
// revision and role, its thin devices with their thin ids, the NQNs of its
// subsystems in order, and the largest side id and the count of the sides
// of its layout: every leg of the slices' meta and data groups, of the meta
// group of each clone the request carries and the new legs of each of its
// repairs, and each retired leg those groups list, by the side id the
// request's retired_leg_id_to_side_id gives it. A retired leg with no side
// id there, as in a request from a worker without the map, is counted and
// marks the summary as missing one. The gateway counts the restored layout
// the same way (architecture.md, Controller nodes,
// CheckControllerNodeRestore). It also lists, in order, the ns_id of every
// namespace the request holds and of every one it keeps effectively
// suspended, so parked (nsEffectivelySuspended), which the gateway compares
// with the restored state. A stored request is never modified, so the summary shares its
// messages.
func storedCntlr(req *pb.SyncupCntlrRequest) *pb.GetCnStoreReply_StoredCntlr {
	nqns := slices.Sorted(maps.Keys(req.GetNqnToSubsystem()))
	var grps []*pb.Group
	for _, slice := range req.GetIdToSlice() {
		grps = append(grps, slice.GetMetaGrpList()...)
		grps = append(grps, slice.GetDataGrpList()...)
	}
	for _, clone := range req.GetCloneList() {
		if clone.GetMetaGrp() != nil {
			grps = append(grps, clone.GetMetaGrp())
		}
	}
	var legs []*pb.Leg
	for _, rep := range req.GetNameToRepair() {
		legs = append(legs, rep.GetNewLegList()...)
	}
	var maxSideId, sideCnt uint64
	missing := false
	for _, grp := range grps {
		legs = append(legs, grp.GetLegList()...)
		for _, legId := range grp.GetRetiredLegIdList() {
			sideCnt++
			sideId, ok := req.GetRetiredLegIdToSideId()[legId]
			if !ok {
				missing = true
				continue
			}
			maxSideId = max(maxSideId, sideId)
		}
	}
	for _, leg := range legs {
		for _, side := range leg.GetSideList() {
			maxSideId = max(maxSideId, side.GetSideId())
			sideCnt++
		}
	}
	var nsIds, parked []uint64
	for _, nqn := range nqns {
		for _, ns := range req.GetNqnToSubsystem()[nqn].GetNsList() {
			nsIds = append(nsIds, ns.GetNsId())
			if nsEffectivelySuspended(req, ns) {
				parked = append(parked, ns.GetNsId())
			}
		}
	}
	slices.Sort(nsIds)
	slices.Sort(parked)
	return &pb.GetCnStoreReply_StoredCntlr{
		CntlrPointer:   req.GetCntlrPointer(),
		Revision:       req.GetRevision(),
		Primary:        req.GetCntlr().GetPrimary(),
		TdList:         req.GetTdList(),
		NqnList:        nqns,
		MaxSideId:      maxSideId,
		SideCnt:        sideCnt,
		ParkedNsIdList: parked,
		NsIdList:       nsIds,

		RetiredSideIdMissing: missing,
	}
}
