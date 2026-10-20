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
	"context"
	"fmt"
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

	// mu guards the in-memory mirrors of the local store below — the two
	// maps — and what mirrors nothing: in each cntlr's state the
	// connect-retry and prober registries, in each
	// CN's and each cntlr's state its observation slot (obs), the converge
	// marks (marks) and the CN10 disconnect registry. It does not guard the
	// request each state holds, which is an atomic pointer (cnState.req,
	// cntlrState.req). It is a leaf lock: never held across an OS call.
	mu     sync.Mutex
	cns    map[string]*cnState
	cntlrs map[string]*cntlrState
	// marks are the converges in flight, by CN key and cntlr key, and the
	// SyncupCn calls in flight that carry a request newer than the CN's held
	// one, or for a CN the agent does not hold (dnagent.md SH30): what keeps
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

	// rootCtx is the process lifetime ctx, captured at Reconcile; the
	// CN10/CN18 connect retries, the CN10 background disconnects, the CN11
	// probers and the background reads (SH29) hang off it, so shutdown
	// cancels them.
	rootCtx context.Context

	// probeInterval / probeStall / retryInterval are fields rather than
	// constants, and now and sleep — like probeIO above — are overridable,
	// so tests can drive the CN11 prober on a short fake clock and off the
	// real syscalls, run the background retry's attempts by hand, and run a
	// converge pass's CN10 connect budget (newPassBudget) on a fake clock
	// too; nothing else changes them.
	probeInterval time.Duration
	probeStall    time.Duration
	retryInterval time.Duration
	now           func() time.Time
	sleep         func(context.Context, time.Duration) error
}

// cnState is one synced CN: its last accepted SyncupCnRequest as this process
// holds it (SH8) — a SyncupCn stores it once it has passed the revision gate,
// before its converge runs — plus the ResInfo history of the base state
// (architecture.md, Controller node, common), and its latest observation.
type cnState struct {
	// req is read and written through loadReq and storeReq only. It is
	// stored only under the node write lock, and every reader holds
	// at least the node lock's read half, so the lock alone would do; it is
	// an atomic pointer all the same, like cntlrState.req, so that a reader
	// added off the node lock cannot race it.
	req     atomic.Pointer[pb.SyncupCnRequest]
	tracker *agent.ResTracker
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
	// changes with it, needs none of that: only this cntlr's own converges
	// read it, and each holds this cntlr's object lock or the node write
	// lock, either of which orders it after the store.
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
	// tornTds marks the snapshot tds this incarnation tore (CN14): one run
	// of the snapshot pre-pass made the td's id in one slice while another
	// slice already held it (snapshotPrePass). Keyed by td_id; in-memory
	// only, so an agent restart loses a mark the worker has not read yet. A
	// pre-pass sets a mark and nothing but the request clears it: every
	// converge drops the marks of tds its request no longer lists or lists
	// created (keepTornMarks), the created flip having recorded the mark by
	// then (dnv-worker.md RW19). Both channels report a mark in the td's thin
	// info (tornMark). It is read and written under this cntlr's object lock
	// or the node write lock, as pendingSweep is.
	tornTds map[uint64]bool
	// reqFromRpc is true once req was delivered by a revision-gated
	// SyncupCntlr in THIS incarnation. The startup Reconcile converges from
	// the persisted copy, which a failed save lets lag the pool's true
	// contents: syncupCntlr saves before convergeCntlr but only logs a Save
	// that fails, and the converge runs on — the sweep deletes ids absent
	// from td_list, so it only ever trusts an RPC-delivered one (CN14).
	reqFromRpc bool

	// probers are the CN11 leg health probers, keyed by leg_id. They hold no
	// lock; every converge cancels those its plan no longer probes
	// (convergeCntlr), and the cntlr's drop cancels the rest (CN7).
	probers map[uint64]*legProber

	// retrying/cancel drive the CN10/CN18 background connect retry.
	retrying bool
	cancel   context.CancelFunc

	// obs is the cntlr's latest observation and its running read
	// (dnagent.md SH28, SH29), guarded by the server's mu.
	obs agent.ObsSlot[*pb.CntlrInfo]
}

// NewCnAgentServer builds the cn role server. oc is the process's single
// OsClient, localStore the --local-store prefix (the same one nf was built
// with), capacity the --capacity budget, trConf the transport of this
// agent's nvmet port and portId the --nvmet-port-id it converges
// (common.NvmetPortId unless the flag says otherwise).
func NewCnAgentServer(
	oc common.OsClient,
	nf *common.NameFmt,
	localStore string,
	capacity uint64,
	trConf *pb.NvmeTrConf,
	portId int,
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
		port: agent.PortConf{
			PortId:  portId,
			TrType:  trConf.GetTrType(),
			AdrFam:  trConf.GetAdrFam(),
			TrAddr:  trConf.GetTrAddr(),
			TrSvcId: trConf.GetTrSvcId(),
		},
		cns:             make(map[string]*cnState),
		cntlrs:          make(map[string]*cntlrState),
		marks:           agent.NewConvergeMarks(),
		roundReads:      true,
		disconnecting:   make(map[string]struct{}),
		disconnectSlots: make(chan struct{}, disconnectConcurrency),
		rootCtx:         context.Background(),
		probeInterval:   common.CnLegProbeInterval * time.Second,
		probeStall:      common.CnLegProbeStallSeconds * time.Second,
		retryInterval:   common.CnConnectRetryInterval * time.Second,
		now:             time.Now,
		sleep:           agent.SleepCtx,
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
		probers:      make(map[uint64]*legProber),
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
// clamps. No locks, no store, no OS access — and no AgentReply: it cannot
// fail.
func (s *CnAgentServer) GetCnSize(
	ctx context.Context,
	req *pb.GetCnSizeRequest,
) (*pb.GetCnSizeReply, error) {
	return &pb.GetCnSizeReply{Size: s.capacity}, nil
}

// SyncupCn marks its converge as it arrives, before it waits for the node
// write lock (dnagent.md SH30). A request newer than the CN's held one, or
// for a CN the agent does not hold, marks the node too and cancels every
// read of the agent: that lock waits for every holder of its read half,
// every cntlr's read among them, and the node's mark keeps a round from
// starting a new one until the syncup ends. A re-send at the held revision,
// which a CN-level leftover brings every round, cancels the CN's read alone
// and waits for the cntlr reads in flight, so it keeps no cntlr's read from
// ending. A request the revision gate refuses against the CN's held request
// marks nothing and cancels nothing, so a stale request the worker re-sends
// every round keeps no read from ending; the gate refuses it under the write
// lock (markNodeSyncup).
func (s *CnAgentServer) SyncupCn(
	ctx context.Context,
	req *pb.SyncupCnRequest,
) (*pb.SyncupCnReply, error) {
	defer s.markNodeSyncup(cnKey(req.GetClusterId(), req.GetCnId()),
		req.GetRevision())()
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()
	return s.syncupCn(ctx, req), nil
}

// SyncupCntlr marks its cntlr's converge in flight and cancels the cntlr's
// running read before it takes a lock (dnagent.md SH30), save a request CN8's
// gates refuse against the cntlr's held request, which marks nothing and
// cancels nothing, so a refused request the worker re-sends every round
// keeps no read from ending (markSyncup); the gates refuse it under the
// object lock.
//
// It takes the cntlr's object lock only for a cntlr this process holds or a
// request CN8's gates admit: a request they refuse for a cntlr it does not
// hold is answered under the node read lock alone, so it leaves no lock
// entry behind (dnagent.md, SH11) — only the teardown of a held cntlr drops
// one (dropCntlrState). syncupCntlr runs every gate again under the object
// lock.
func (s *CnAgentServer) SyncupCntlr(
	ctx context.Context,
	req *pb.SyncupCntlrRequest,
) (*pb.SyncupCntlrReply, error) {
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	defer s.markSyncup(key, req)()
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getCntlr(key) == nil {
		if reply := s.gateCntlr(ctx, req, 0); reply != nil {
			return reply, nil
		}
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.syncupCntlr(ctx, key, req), nil
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
	obs, since, known := s.answerCn(ctx,
		cnKey(req.GetClusterId(), req.GetCnId()), false)
	now := s.now()
	if !known {
		return &pb.GetCnInfoReply{
			AgentReply: unknownCn(req.GetCnId()),
			ObsStamp:   (&cnObs{}).Stamp(now, time.Time{}),
		}, nil
	}
	return &pb.GetCnInfoReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		CnInfo:     obs.Info,
		ObsStamp:   obs.Stamp(now, since),
	}, nil
}

// GetCntlrInfo answers from the cntlr's latest observation, as a CheckCntlr
// round does, and starts no read (dnagent.md SH28): it takes no lock, so a
// read of an unknown cntlr leaves no lock entry (dnagent.md, SH11), and reads
// nothing of the node (CN23).
func (s *CnAgentServer) GetCntlrInfo(
	ctx context.Context,
	req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	obs, since, known := s.answerCntlr(ctx, key, false)
	now := s.now()
	if !known {
		return &pb.GetCntlrInfoReply{
			AgentReply: unknownCntlr(req.GetCntlrPointer()),
			ObsStamp:   (&cntlrObs{}).Stamp(now, time.Time{}),
		}, nil
	}
	return &pb.GetCntlrInfoReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		CntlrInfo:  obs.Info,
		ObsStamp:   obs.Stamp(now, since),
	}, nil
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
