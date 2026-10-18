// Package dnagent implements the dn policy of
// dnagent.md, The dn role — package `dnagent`: the
// DiskNodeAgent service — which disk-metadata records, dm tables and nvmet
// objects a disk node builds, and when. All mechanism (bootstrap, local store, revision gate,
// locks, ResInfo tracking, OS wrappers, bitmap store) comes from package
// agent; this package never touches the OS directly.
package dnagent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// DnAgentServer serves DiskNodeAgent for one disk node.
type DnAgentServer struct {
	pb.UnimplementedDiskNodeAgentServer

	nf    *common.NameFmt
	store *agent.Store
	meta  *DiskMeta
	dm    *agent.Dm
	nvmet *agent.Nvmet
	host  *agent.NvmeHost
	locks *agent.LockSet

	disk string
	port agent.PortConf

	// fenceWait is the src-cutover grace window of architecture.md, Migration,
	// src step 2 (common.SuspendSeconds).
	// It is a field rather than a constant so tests can shorten it; nothing
	// else ever changes it.
	fenceWait time.Duration

	// zeroRetryInterval paces the DN9 background zeroing loop's retries
	// (common.DnZeroRetryInterval). Field rather than constant for the same
	// reason as fenceWait: no unit test can wait out the production value.
	zeroRetryInterval time.Duration

	// zeroBatchMax and zeroBatchMin bound a side's DN9 zeroing batch: its
	// first size and the ceiling a success doubles it back to
	// (common.DnZeroBatchMaxBytes), and the floor a kill halves it down to
	// (common.DnZeroBatchMinBytes). Fields for the same reason as
	// zeroRetryInterval: a unit-test side is a few MiB, which the production
	// first batch would zero in one command, hiding every multi-batch path.
	zeroBatchMax uint64
	zeroBatchMin uint64

	// zeroSlots holds one token per zeroing batch in flight, so at most
	// common.DnZeroConcurrency of them run at once on this agent however many
	// of its sides are zeroing (DN9).
	zeroSlots chan struct{}

	// migrRetryInterval paces the DN8 retry of a migration destination
	// (common.DnMigrConnectRetryInterval; DN13), a field for the same
	// reason. It is what lets a test finish a destination's build, or take
	// the run a destination waits for, from the RETRY LOOP rather than from
	// an RPC: a pass of the loop can deregister the loop it runs in, which is
	// why its attempts run on rootCtx (migrRetryLoop).
	migrRetryInterval time.Duration

	// now and sleep are the clock seams, real by default: DN6's orphan age
	// gate compares a subsystem directory's mtime with now, and DN13's
	// in-pass retry of the connect to the migration source export and its
	// wait for the source's namespace pace themselves through both
	// (agent.WaitBudget). Fields for the same reason as the three above: a
	// unit test runs them on a fake clock; nothing else changes them.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// bg tracks every background goroutine that owns a child process, so
	// agent.Serve can join them after GracefulStop and no orphan
	// `blkdiscard --zeroout` ever outlives the agent (DN9, SH27). A task
	// enrolls under mu (enrollLocked), and only while bgClosed is unset.
	bg sync.WaitGroup
	// bgClosed is set under mu when the join begins (WaitBackground) and
	// never cleared: from then on no task enrolls in bg and no DN12 fence
	// timer is armed (bgClosedLocked).
	bgClosed bool

	// mu guards the in-memory mirrors of the local store below: which DNs
	// and sides this agent holds. The request each of them holds is an
	// atomic pointer of its own (dnState.req, sideState.req). It also guards
	// bgClosed, each side's retry, zeroing and fence fields, each DN's and
	// side's observation slot (obs) and marks, so a task's start and the
	// join's mark are ordered by it, and so are a round's start of a read
	// and a syncup's mark and its cancel of that read (SH30). It is a leaf
	// lock: never held across an OS call.
	mu    sync.Mutex
	dns   map[string]*dnState
	sides map[string]*sideState

	// marks records the converges in flight, keyed by the DN and side keys,
	// and the SyncupDn calls in flight that carry a request newer than the
	// DN's held one, or for a DN the agent does not hold (SH30): a check
	// round starts no read while its object's or the node's mark stands, and
	// a reply reports the earliest start among its object's marks. Guarded
	// by mu.
	marks *agent.ConvergeMarks

	// roundReads lets a check round start the background read of its object
	// (SH29). NewDnAgentServer sets it and nothing else in production
	// changes it; it is a field so that a unit test can keep rounds from
	// starting reads of their own, which would race the test's own steps,
	// and call refreshDn and refreshSide itself instead.
	roundReads bool
	// reads counts the background reads check rounds started that have not
	// ended, so a unit test can join them. Production joins them through bg,
	// in which each read is enrolled too (SH27).
	reads sync.WaitGroup

	// rootCtx is the process lifetime ctx, captured at Reconcile; the DN8
	// background retries and the DN9 zeroing loops hang off it so shutdown
	// stops them.
	rootCtx context.Context
}

// WaitBackground joins the dn background goroutines enrolled in bg — the DN9
// zeroing loops, the DN8 connect retries and the background reads check
// rounds start (SH29) — after it stops every armed DN12
// fence timer (SH27). It first marks the server closed (bgClosed) under mu,
// the lock every enrollment and every arming takes, so a task either enrolled
// before the join began and is waited for, or is refused, and no timer is
// armed once it has begun. The mark is never cleared: a joined server starts
// no background work again. A fence callback already running is the
// deliberate carve-out: it is not enrolled and not waited for, and its
// converge runs on rootCtx, which agent.Serve cancels before the join, so it
// starts no command and no task. cmd/dnv-agent passes it to agent.Serve,
// which cancels rootCtx and calls it after GracefulStop has drained the last
// RPC (SH27).
func (s *DnAgentServer) WaitBackground() {
	s.mu.Lock()
	s.bgClosed = true
	var timers []*time.Timer
	for _, st := range s.sides {
		if st.fenceTimer != nil {
			timers = append(timers, st.fenceTimer)
			st.fenceTimer = nil
		}
	}
	s.mu.Unlock()
	for _, timer := range timers {
		timer.Stop()
	}
	s.bg.Wait()
}

// bgClosedLocked reports whether no background work may start: the join has
// begun (bgClosed), or rootCtx is done and a task would only end. The caller
// holds mu, the lock the join marks the server closed under, and enrolls a
// task in bg (enrollLocked), or arms a fence timer, before it lets go of it
// (SH27).
func (s *DnAgentServer) bgClosedLocked() bool {
	return s.bgClosed || s.rootCtx.Err() != nil
}

// enrollLocked enrolls one background task in bg, the only way a task
// enrolls. The caller holds mu and has found the server open under it
// (bgClosedLocked); the join marks the server closed under mu before its
// bg.Wait, so the task is enrolled before that mark, and waited for, and no
// bg.Add can follow the Wait (SH27).
func (s *DnAgentServer) enrollLocked() {
	bgEnrolling(&s.mu)
	s.bg.Add(1)
}

// bgEnrolling runs at every enrollment in bg, given the lock the enrolling
// call must hold, and does nothing in production. It is the seam a test
// checks that lock through: an enrollment made after its start let go of mu
// goes wrong only when the join lands between the two, which the race
// detector sees only when it happens.
var bgEnrolling = func(*sync.Mutex) {}

// dnState is one synced DN: its last accepted request as this process holds it
// (SH8) — a SyncupDn stores it once it has passed the revision and conf gates,
// before its converge runs — plus the ResInfo history of its node-level
// resources and its latest observation.
type dnState struct {
	// req is replaced only under the node write lock (SyncupDn), which
	// orders it against every converge and read, since each holds the node
	// lock. It is an atomic pointer all the same, like a side's, so that no
	// load of it depends on which lock its caller happens to hold. A check
	// round and GetDnInfo never load it: they answer the revision of the
	// observation (SH28).
	req     atomic.Pointer[pb.SyncupDnRequest]
	tracker *agent.ResTracker
	// obs is the DN's latest observation and its running background read
	// (SH28, SH29), under s.mu.
	obs agent.ObsSlot[*pb.DnInfo]
}

func newDnState(req *pb.SyncupDnRequest) *dnState {
	st := &dnState{tracker: agent.NewResTracker()}
	st.req.Store(req)
	return st
}

// sideState is one synced side. chunks mirrors the migr-bm-* files this
// process loaded at startup or received since, which stay the source of
// truth for the applied set (SH21); a side rebuilt after DN2's reload skipped
// its files starts empty over them until the worker pushes them again.
type sideState struct {
	// req is the side's current request. Its SyncupSide replaces it under
	// the node read lock and this side's object lock, and passes that do
	// not hold that object lock read it: another side's converge or read,
	// and a read of the DN, read every held side's request — for the
	// claims and for the sides this node may host — under the node read
	// lock, which the replacement shares. So it is an atomic pointer, and
	// each of them loads a whole request, the old one or the new one.
	// Nothing else a SyncupSide changes is read that way: the chunk set and
	// the tracker stay under this side's object lock, and the retry,
	// zeroing and fence fields and the observation under s.mu.
	req     atomic.Pointer[pb.SyncupSideRequest]
	tracker *agent.ResTracker
	chunks  *agent.ChunkSet
	// obs is the side's latest observation and its running background read
	// (SH28, SH29), under s.mu.
	obs agent.ObsSlot[*pb.SideInfo]
	// chunkMigrId identifies the migration the chunks belong to, so chunks
	// left over from an earlier migration are never applied to a new one.
	chunkMigrId uint64
	// retrying/cancel drive the DN8 retry of a migration destination (DN13):
	// one whose build stopped short, or one that has not provisioned and
	// waits for its run in the clone-metadata area before it zeroes.
	retrying bool
	cancel   context.CancelFunc

	// zeroing/zeroCancel/zeroDone drive the DN9 background side-zeroing
	// goroutine. zeroDone is closed by the goroutine on exit, which is what
	// makes cancel-and-wait possible before the side device is removed — its
	// `blkdiscard --zeroout` child holds that device open.
	zeroing    bool
	zeroCancel context.CancelFunc
	zeroDone   chan struct{}
	// zeroErr is the last failed batch's error, published for side_dev_info
	// exactly the way a cn legProber publishes its outcome (CN11). It is
	// cleared by the next successful batch (DN9).
	zeroErr error

	// fenceAt is when this side's per-CN dm-linears were suspended for the
	// src cutover (architecture.md, Migration, src step 2); zero when no fence
	// is in progress. fenceTimer
	// re-runs the converge at the end of the window so the RPC never waits
	// for it. Both are in-memory only: after a restart a suspended linear
	// has no recorded start, and DN12 treats a side in which the restart
	// finds one as "the window is over" rather than starting a second one (a
	// restart whose probes of the side's linears all went unanswered finds
	// none, and one that ends up holding no state for the side keeps
	// nothing of what it found: DN12 rule 1's known limit) — the whole point
	// of the bound is that no dnv device stays suspended indefinitely.
	fenceAt    time.Time
	fenceTimer *time.Timer
	// fenceRestarted marks a side reloaded from the local store whose per-CN
	// linears this process found suspended: they are debris from the previous
	// one, and are retired at once rather than starting a second window. It is
	// set only for those sides — a blanket flag would silently skip the first
	// real cutover window after any restart (adoptFence).
	fenceRestarted bool
	// fenceEnded marks a window a level with no export layer ended early
	// (endFence): over, as an adopted one is, until the source role ends, so
	// no later converge of this process opens it again over linears phase 2
	// has already retired. A restart forgets it (DN12's known limit).
	fenceEnded bool
}

// NewDnAgentServer builds the dn role server. oc is the process's single
// OsClient, localStore the --local-store prefix (the same one nf was built
// with), disk the --disk device, trConf the transport of this agent's nvmet
// port and portId the --nvmet-port-id it converges (common.NvmetPortId
// unless the flag says otherwise).
func NewDnAgentServer(
	oc common.OsClient,
	nf *common.NameFmt,
	localStore string,
	disk string,
	trConf *pb.NvmeTrConf,
	portId int,
) *DnAgentServer {
	return &DnAgentServer{
		nf:                nf,
		store:             agent.NewStore(oc, localStore),
		meta:              NewDiskMeta(oc, disk),
		dm:                agent.NewDm(oc),
		nvmet:             agent.NewNvmet(oc),
		host:              agent.NewNvmeHost(oc),
		locks:             agent.NewLockSet(),
		disk:              disk,
		fenceWait:         common.SuspendSeconds * time.Second,
		zeroRetryInterval: common.DnZeroRetryInterval * time.Second,
		zeroBatchMax:      common.DnZeroBatchMaxBytes,
		zeroBatchMin:      common.DnZeroBatchMinBytes,
		zeroSlots:         make(chan struct{}, common.DnZeroConcurrency),
		migrRetryInterval: common.DnMigrConnectRetryInterval * time.Second,
		now:               time.Now,
		sleep:             agent.SleepCtx,
		marks:             agent.NewConvergeMarks(),
		roundReads:        true,
		port: agent.PortConf{
			PortId:  portId,
			TrType:  trConf.GetTrType(),
			AdrFam:  trConf.GetAdrFam(),
			TrAddr:  trConf.GetTrAddr(),
			TrSvcId: trConf.GetTrSvcId(),
		},
		dns:     make(map[string]*dnState),
		sides:   make(map[string]*sideState),
		rootCtx: context.Background(),
	}
}

// ---------------------------------------------------------------------------
// Object keys and state lookup
// ---------------------------------------------------------------------------

func dnKey(clusterId, dnId uint64) string {
	return fmt.Sprintf("%016x-%016x", clusterId, dnId)
}

// sideKey is the DN1 object key: (cluster_id, dn_id, sp_id, side_id).
func sideKey(clusterId, dnId, spId, sideId uint64) string {
	return fmt.Sprintf(
		"%016x-%016x-%016x-%016x", clusterId, dnId, spId, sideId)
}

func (s *DnAgentServer) getDn(key string) *dnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dns[key]
}

func (s *DnAgentServer) getSide(key string) *sideState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sides[key]
}

func (s *DnAgentServer) putDn(key string, st *dnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dns[key] = st
}

func (s *DnAgentServer) putSide(key string, st *sideState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sides[key] = st
}

// dropSide forgets a side's state, the one place a state leaves the maps. It
// cancels the side's running read in the same hold of s.mu, and waits for
// nothing (SH30): the read may be parked on the node lock the dropper holds,
// and once it gets it, it finds the side gone and ends.
func (s *DnAgentServer) dropSide(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.sides[key]; st != nil {
		st.obs.CancelReadLocked()
	}
	delete(s.sides, key)
}

// sideKeysOf lists the sides currently belonging to one DN.
func (s *DnAgentServer) sideKeysOf(clusterId, dnId uint64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key, st := range s.sides {
		req := st.req.Load()
		if req.GetClusterId() == clusterId && req.GetDnId() == dnId {
			keys = append(keys, key)
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// RPC entry points — locking per DN1, the work in the syncup_*/check/probe
// files.
// ---------------------------------------------------------------------------

// GetDnSize returns the byte size of the --disk device's **data area** — the
// raw size minus DnDataOffset, the fixed [D13] prefix the header, the two
// volume-table slots and the clone-metadata area occupy. The CP divides this
// by extent_size to get total_ext_cnt (architecture.md, Size → extents), so
// there is no further subtraction anywhere. It has no AgentReply, so failure
// travels in the gRPC status (architecture.md, Common agent rules); dn_id is
// for logging only, and the call takes no lock and touches no store (DN3).
func (s *DnAgentServer) GetDnSize(
	ctx context.Context,
	req *pb.GetDnSizeRequest,
) (*pb.GetDnSizeReply, error) {
	size, err := s.dm.DiskSize(ctx, s.disk)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if size <= common.DnDataOffset {
		return nil, status.Errorf(codes.Internal,
			"disk too small: %d <= %d", size, common.DnDataOffset)
	}
	return &pb.GetDnSizeReply{Size: size - common.DnDataOffset}, nil
}

// SyncupDn marks its converge as it arrives, before it waits for the node
// write lock (SH30). A request newer than the DN's held one, or for a DN the
// agent does not hold, marks the node too and cancels every read of the
// agent: each read holds the node read lock that lock waits for, and the
// node's mark keeps a round from starting a new one until the syncup ends.
// A re-send at the held revision, which a leftover brings every round,
// cancels the DN's read alone and waits for the side reads in flight, so it
// keeps no side's read from ending. A request DN4's gates refuse against the
// held request marks nothing and cancels nothing, so a request the worker
// re-sends every round keeps no read from ending; the gates refuse it under
// the write lock (beginDnSyncup).
func (s *DnAgentServer) SyncupDn(
	ctx context.Context,
	req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	key := dnKey(req.GetClusterId(), req.GetDnId())
	if end := s.beginDnSyncup(key, req); end != nil {
		defer end()
	}
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()
	return s.syncupDn(ctx, req), nil
}

// SyncupSide marks its converge as it arrives and cancels the side's running
// read before it takes any lock (SH30), so it never waits behind a read; the
// mark ends once the call has, refusals included — save a request DN8's
// gates refuse against the side's held request, which marks nothing and
// cancels nothing, so a refused request the worker re-sends every round
// keeps no read from ending (beginSideSyncup); the gates refuse it under the
// object lock.
//
// It takes the side's object lock only for a side this process holds
// or a request DN8's gates admit: a request they refuse for a side it does
// not hold is answered under the node read lock alone, so it leaves no lock
// entry behind (dnagent.md, Concurrency — `locks.go`). The DN request the
// gates read cannot change under that lock, and syncupSide runs every gate
// again under the object lock.
func (s *DnAgentServer) SyncupSide(
	ctx context.Context,
	req *pb.SyncupSideRequest,
) (*pb.SyncupSideReply, error) {
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	if s.beginSideSyncup(key, req) {
		defer s.endConverge(key)
	}
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getSide(key) == nil {
		if reply, _ := s.gateSide(ctx, req, 0); reply != nil {
			return reply, nil
		}
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.syncupSide(ctx, key, req), nil
}

// PushMigrBitmap refuses a push for a side this process does not hold under
// the node read lock alone, so the refusal leaves no lock entry behind
// (dnagent.md, Concurrency — `locks.go`); pushMigrBitmap checks the side
// again under its object lock. It cancels no read: it waits for one as it
// waits for a converge (SH30).
func (s *DnAgentServer) PushMigrBitmap(
	ctx context.Context,
	req *pb.PushMigrBitmapRequest,
) (*pb.PushMigrBitmapReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	if s.getSide(key) == nil {
		return &pb.PushMigrBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown side %s", sidePointerText(req.GetSidePointer())),
		}, nil
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.pushMigrBitmap(ctx, key, req), nil
}

// GetDnInfo answers from the DN's latest observation, as a check round does,
// and takes no lock, reads nothing of the node and starts no read (SH28).
func (s *DnAgentServer) GetDnInfo(
	ctx context.Context,
	req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	key := dnKey(req.GetClusterId(), req.GetDnId())
	s.mu.Lock()
	st := s.dns[key]
	if st == nil {
		s.mu.Unlock()
		return &pb.GetDnInfoReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown dn %d", req.GetDnId()),
			ObsStamp: noObsStamp[*pb.DnInfo](s.now()),
		}, nil
	}
	obs := st.obs.LatestLocked()
	since := s.marks.SinceLocked(key)
	s.mu.Unlock()
	return &pb.GetDnInfoReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		DnInfo:     obs.Info,
		ObsStamp:   obs.Stamp(s.now(), since),
	}, nil
}

// GetSideInfo answers from the side's latest observation, as a check round
// does, and takes no lock, reads nothing of the node and starts no read
// (SH28). A side the agent does not hold gets no lock entry from it.
func (s *DnAgentServer) GetSideInfo(
	ctx context.Context,
	req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	s.mu.Lock()
	st := s.sides[key]
	if st == nil {
		s.mu.Unlock()
		return &pb.GetSideInfoReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown side %s", sidePointerText(req.GetSidePointer())),
			ObsStamp: noObsStamp[*pb.SideInfo](s.now()),
		}, nil
	}
	obs := st.obs.LatestLocked()
	since := s.marks.SinceLocked(key)
	s.mu.Unlock()
	return &pb.GetSideInfoReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		SideInfo:   obs.Info,
		ObsStamp:   obs.Stamp(s.now(), since),
	}, nil
}

func sidePointerText(ptr *pb.SidePointer) string {
	return fmt.Sprintf("(sp %d, leg %d, side %d)",
		ptr.GetSpId(), ptr.GetLegId(), ptr.GetSideId())
}
