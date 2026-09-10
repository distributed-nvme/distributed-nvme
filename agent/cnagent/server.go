// Package cnagent implements the cn policy of cnagent.md §4: the
// ControllerNodeAgent service — which leg connections, md arrays, thin pools,
// dm and nvmet objects a controller node builds, and when. All mechanism
// (bootstrap, local store, revision gate, locks, ResInfo tracking, the dm /
// nvmet / nvme-host wrappers, the bitmap store) comes from package agent; the
// tools only this role runs — mdadm and thin-provisioning-tools — are wrapped
// here (md.go, thinbm.go) because a wrapper with a single role is role code
// (dnagent.md §1 split rule). The §3.2 base state and the clone-metadata slot
// allocator that replaced LVM live in clonemeta.go ([D14]).
package cnagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CnAgentServer serves ControllerNodeAgent for one controller node.
type CnAgentServer struct {
	pb.UnimplementedControllerNodeAgentServer

	oc      common.OsClient
	probeIO LegProbeIO // CN11 probe IO — deliberately NOT via oc (update_01.md U2)
	nf      *common.NameFmt
	cmd     *agent.Cmd
	store   *agent.Store
	dm      *agent.Dm
	nvmet   *agent.Nvmet
	host    *agent.NvmeHost
	md      *Md
	cmeta   *CloneMeta
	locks   *agent.LockSet

	capacity uint64
	port     agent.PortConf

	// mu guards the in-memory mirrors of the local store below. It is a leaf
	// lock: never held across an OS call.
	mu     sync.Mutex
	cns    map[string]*cnState
	cntlrs map[string]*cntlrState

	// cloneMetaMu serializes the CN clone-metadata allocator ([D14]): the
	// registry is the kernel's dm table set, and two cntlrs of the same CN
	// converge concurrently under the node read lock, so enumerate → discard →
	// create must be one critical section. Unlike `mu` it IS held across OS
	// calls; it is a leaf lock, taken below every agent.LockSet lock and never
	// held while another lock is acquired.
	cloneMetaMu sync.Mutex

	// rootCtx is the process lifetime ctx, captured at Reconcile; the
	// CN10/CN18 connect retries and the CN11 probers hang off it so shutdown
	// stops them.
	rootCtx context.Context

	// probeInterval / probeStall / retryInterval are fields rather than
	// constants, and now — like probeIO above — is overridable, so tests can
	// drive the CN11 prober on a short fake clock and off the real syscalls;
	// nothing else changes them.
	probeInterval time.Duration
	probeStall    time.Duration
	retryInterval time.Duration
	now           func() time.Time
}

// cnState is one synced CN: its last fully applied SyncupCnRequest plus the
// ResInfo history of the §3.2 base state.
// The loop device carrying the clone-metadata arena is deliberately *not* a
// field here: it is kernel-assigned state, re-learned from `losetup
// --associated` on every converge and probe pass and never cached across one
// (CN5, [D14]) — a cached path would only be as fresh as the last SyncupCn,
// which a SyncupCntlr does not run.
type cnState struct {
	req     *pb.SyncupCnRequest
	tracker *agent.ResTracker
}

// cntlrState is one synced cntlr.
type cntlrState struct {
	req     *pb.SyncupCntlrRequest
	tracker *agent.ResTracker

	// chunks mirrors this cntlr's clone-bm-* files, one ChunkSet per clone;
	// the files stay the source of truth for the applied set (SH21).
	chunks map[uint64]*agent.ChunkSet

	// applied is the shape of the last converge, so a resource that leaves
	// the desired state — a td dropped from td_list, a clone deleted, a
	// layer suppressed by a rising sp_level — can still be named while it is
	// torn down.
	applied *cntlrPlan

	// pendingSweep marks slices whose pool device THIS incarnation created
	// but has not yet successfully swept for orphan thin ids (CN14's
	// activation sweep, update_05.md U3). Keyed by slice_id; in-memory only —
	// a crash in the window leaves a stray for the pool's next rebuild. The
	// startup Reconcile's Create branch arms it too; running additionally
	// requires reqFromRpc.
	pendingSweep map[uint64]bool
	// reqFromRpc is true once st.req was delivered by a revision-gated
	// SyncupCntlr in THIS incarnation. The startup Reconcile converges from
	// the persisted copy, which converge-then-persist (syncupCntlr saves
	// after convergeCntlr and only logs a failed Save) lets lag the pool's
	// true contents — the sweep deletes ids absent from td_list, so it only
	// ever trusts an RPC-delivered one (update_05.md U3).
	reqFromRpc bool

	// probers are the CN11 leg health probers, keyed by leg_id. They hold no
	// lock and are cancelled at teardown.
	probers map[uint64]*legProber

	// retrying/cancel drive the CN10/CN18 background connect retry.
	retrying bool
	cancel   context.CancelFunc
}

// NewCnAgentServer builds the cn role server. oc is the process's single
// OsClient, localStore the --local-store prefix (the same one nf was built
// with), capacity the --capacity budget and trConf the node's single nvmet
// port.
func NewCnAgentServer(
	oc common.OsClient,
	nf *common.NameFmt,
	localStore string,
	capacity uint64,
	trConf *pb.NvmeTrConf,
) *CnAgentServer {
	return &CnAgentServer{
		oc: oc,
		// The probers get the direct-syscall implementation, never a wrapper
		// over oc (update_01.md U2); tests swap the field after construction,
		// which is why the constructor's signature is unchanged.
		probeIO:  directLegProbeIO{},
		nf:       nf,
		cmd:      agent.NewCmd(oc),
		store:    agent.NewStore(oc, localStore),
		dm:       agent.NewDm(oc),
		nvmet:    agent.NewNvmet(oc),
		host:     agent.NewNvmeHost(oc),
		md:       NewMd(oc),
		cmeta:    NewCloneMeta(oc),
		locks:    agent.NewLockSet(),
		capacity: capacity,
		port: agent.PortConf{
			TrType:  trConf.GetTrType(),
			AdrFam:  trConf.GetAdrFam(),
			TrAddr:  trConf.GetTrAddr(),
			TrSvcId: trConf.GetTrSvcId(),
		},
		cns:           make(map[string]*cnState),
		cntlrs:        make(map[string]*cntlrState),
		rootCtx:       context.Background(),
		probeInterval: common.CnLegProbeInterval * time.Second,
		probeStall:    common.CnLegProbeStallSeconds * time.Second,
		retryInterval: common.CnConnectRetryInterval * time.Second,
		now:           time.Now,
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

func (s *CnAgentServer) putCn(key string, st *cnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.cntlrs[key] = st
}

func (s *CnAgentServer) dropCntlr(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if st.req.GetClusterId() == clusterId &&
			st.req.GetCnId() == cnId {
			keys = append(keys, key)
		}
	}
	return keys
}

func newCntlrState(req *pb.SyncupCntlrRequest) *cntlrState {
	// reqFromRpc stays false: a state born here may equally well come from
	// the startup Reconcile's persisted copy, and only syncupCntlr knows
	// otherwise (update_05.md U3).
	return &cntlrState{
		req:          req,
		tracker:      agent.NewResTracker(),
		chunks:       make(map[uint64]*agent.ChunkSet),
		pendingSweep: make(map[uint64]bool),
		probers:      make(map[uint64]*legProber),
	}
}

// chunkSet returns (creating on first use) the ChunkSet of one clone.
func (st *cntlrState) chunkSet(cloneId uint64) *agent.ChunkSet {
	set, ok := st.chunks[cloneId]
	if !ok {
		set = agent.NewChunkSet()
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

func (s *CnAgentServer) SyncupCn(
	ctx context.Context,
	req *pb.SyncupCnRequest,
) (*pb.SyncupCnReply, error) {
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()
	return s.syncupCn(ctx, req), nil
}

func (s *CnAgentServer) SyncupCntlr(
	ctx context.Context,
	req *pb.SyncupCntlrRequest,
) (*pb.SyncupCntlrReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.syncupCntlr(ctx, key, req), nil
}

func (s *CnAgentServer) PushCloneBitmap(
	ctx context.Context,
	req *pb.PushCloneBitmapRequest,
) (*pb.PushCloneBitmapReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.pushCloneBitmap(ctx, key, req), nil
}

// GetCnInfo probes fresh live state and never mutates (CN23).
func (s *CnAgentServer) GetCnInfo(
	ctx context.Context,
	req *pb.GetCnInfoRequest,
) (*pb.GetCnInfoReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	st := s.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
	if st == nil {
		return &pb.GetCnInfoReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown cn %d", req.GetCnId()),
		}, nil
	}
	return &pb.GetCnInfoReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
		CnInfo:     s.probeCn(ctx, st),
	}, nil
}

func (s *CnAgentServer) GetCntlrInfo(
	ctx context.Context,
	req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	unknown := &pb.GetCntlrInfoReply{
		AgentReply: agent.UnknownObjectReply(
			"unknown cntlr %s", cntlrPointerText(req.GetCntlrPointer())),
	}
	// A read of an unknown cntlr needs no serialization — and taking its
	// object lock would create one for an id that does not exist.
	if s.getCntlr(key) == nil {
		return unknown, nil
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	st := s.getCntlr(key)
	if st == nil {
		return unknown, nil
	}
	return &pb.GetCntlrInfoReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
		CntlrInfo:  s.probeCntlr(ctx, st),
	}, nil
}
