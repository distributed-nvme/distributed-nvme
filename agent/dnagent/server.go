// Package dnagent implements the dn policy of dnagent.md §4: the
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

	// fenceWait is the §11.2 src-cutover grace window (common.SuspendSeconds).
	// It is a field rather than a constant so tests can shorten it; nothing
	// else ever changes it.
	fenceWait time.Duration

	// zeroRetryInterval paces the §9.4 background zeroing loop's retries
	// (common.DnZeroRetryInterval). Field rather than constant for the same
	// reason as fenceWait: no unit test can wait out the production value.
	zeroRetryInterval time.Duration

	// zeroSlots holds one token per §9.4 zeroing batch in flight, so at most
	// common.DnZeroConcurrency of them run at once on this agent however many
	// of its sides are zeroing (DN9).
	zeroSlots chan struct{}

	// migrRetryInterval paces the DN8 background migration-connect retry
	// (common.DnMigrConnectRetryInterval), a field for the same reason. It
	// is what lets a test drive the successful connect from the RETRY LOOP
	// rather than from an RPC, which is the only shape in which that
	// converge runs on the loop's own cancellable context.
	migrRetryInterval time.Duration

	// now and sleep are the clock seams, real by default: DN6's orphan age
	// gate compares a subsystem directory's mtime with now, and DN13's wait
	// for the migration source's namespace paces itself through both
	// (agent.WaitBudget). Fields for the same reason as the three above: a
	// unit test runs them on a fake clock; nothing else changes them.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// bg tracks every background goroutine that owns a child process, so
	// agent.Serve can join them after GracefulStop and no orphan
	// `blkdiscard --zeroout` ever outlives the agent (§9.4, SH27).
	bg sync.WaitGroup

	// mu guards the in-memory mirrors of the local store below: which DNs
	// and sides this agent holds. The request each of them holds is an
	// atomic pointer of its own (dnState.req, sideState.req). It is a leaf
	// lock: never held across an OS call.
	mu    sync.Mutex
	dns   map[string]*dnState
	sides map[string]*sideState

	// rootCtx is the process lifetime ctx, captured at Reconcile; the DN8
	// background retries and the §9.4 zeroing loops hang off it so shutdown
	// stops them.
	rootCtx context.Context
}

// WaitBackground joins the dn background goroutines enrolled in bg — the §9.4
// zeroing loops and the DN8 connect retries. The §11.2 fence timer is the
// deliberate carve-out (SH27); rootCtx cancellation is what stops that one.
// cmd/dnv-agent passes it to agent.Serve, which cancels rootCtx and calls it
// after GracefulStop has drained the last RPC (SH27).
func (s *DnAgentServer) WaitBackground() {
	s.bg.Wait()
}

// dnState is one synced DN: its last fully applied request plus the ResInfo
// history of its node-level resources.
type dnState struct {
	// req is replaced only under the node write lock (SyncupDn), which
	// orders it against every reader, since each holds the node lock. It is
	// an atomic pointer all the same, like a side's, so that no read of it
	// depends on which lock its caller happens to hold.
	req     atomic.Pointer[pb.SyncupDnRequest]
	tracker *agent.ResTracker
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
	// not hold that object lock read it: another side's converge or Check
	// round, and a CheckDn round, read every held side's request — for the
	// claims and for the sides this node may host — under the node read
	// lock, which the replacement shares. So it is an atomic pointer, and
	// each of them loads a whole request, the old one or the new one.
	// Nothing else a SyncupSide changes is read that way: the chunk set and
	// the tracker stay under this side's object lock, and the retry,
	// zeroing and fence fields under s.mu.
	req     atomic.Pointer[pb.SyncupSideRequest]
	tracker *agent.ResTracker
	chunks  *agent.ChunkSet
	// chunkMigrId identifies the migration the chunks belong to, so chunks
	// left over from an earlier migration are never applied to a new one.
	chunkMigrId uint64
	// retrying/cancel drive the DN8 background migration-connect retry.
	retrying bool
	cancel   context.CancelFunc

	// zeroing/zeroCancel/zeroDone drive the §9.4 background side-zeroing
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
	// §11.2 src cutover; zero when no fence is in progress. fenceTimer
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
		zeroSlots:         make(chan struct{}, common.DnZeroConcurrency),
		migrRetryInterval: common.DnMigrConnectRetryInterval * time.Second,
		now:               time.Now,
		sleep:             agent.SleepCtx,
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

func (s *DnAgentServer) dropSide(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
// by extent_size to get total_ext_cnt (§6.1), so there is no further
// subtraction anywhere. It has no AgentReply, so failure travels in the gRPC
// status (§9.1); dn_id is for logging only, and the call takes no lock and
// touches no store (DN3).
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

func (s *DnAgentServer) SyncupDn(
	ctx context.Context,
	req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()
	return s.syncupDn(ctx, req), nil
}

func (s *DnAgentServer) SyncupSide(
	ctx context.Context,
	req *pb.SyncupSideRequest,
) (*pb.SyncupSideReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.syncupSide(ctx, key, req), nil
}

func (s *DnAgentServer) PushMigrBitmap(
	ctx context.Context,
	req *pb.PushMigrBitmapRequest,
) (*pb.PushMigrBitmapReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	return s.pushMigrBitmap(ctx, key, req), nil
}

// GetDnInfo probes fresh live state and never mutates the node (DN16).
func (s *DnAgentServer) GetDnInfo(
	ctx context.Context,
	req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	st := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if st == nil {
		return &pb.GetDnInfoReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown dn %d", req.GetDnId()),
		}, nil
	}
	// The probe runs first, as in checkDnRound: its read of the disk can be
	// the first that answers, which is what confirms DN5's identity, and a
	// verdict taken before it would report the volume table unconfirmed
	// beside a meta row that reads OK.
	info := s.probeDn(ctx, st)
	return &pb.GetDnInfoReply{
		AgentReply: s.dnVerdict(ctx, st).Reply(),
		Revision:   st.req.Load().GetRevision(),
		DnInfo:     info,
	}, nil
}

func (s *DnAgentServer) GetSideInfo(
	ctx context.Context,
	req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	unknown := &pb.GetSideInfoReply{
		AgentReply: agent.UnknownObjectReply(
			"unknown side %s", sidePointerText(req.GetSidePointer())),
	}
	// A read of an unknown side needs no serialization — and taking its
	// object lock would create one for an id that does not exist.
	if s.getSide(key) == nil {
		return unknown, nil
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	st := s.getSide(key)
	if st == nil {
		return unknown, nil
	}
	return &pb.GetSideInfoReply{
		AgentReply: s.sideVerdict(ctx, st).Reply(),
		Revision:   st.req.Load().GetRevision(),
		SideInfo:   s.probeSide(ctx, st),
	}, nil
}

func sidePointerText(ptr *pb.SidePointer) string {
	return fmt.Sprintf("(sp %d, leg %d, side %d)",
		ptr.GetSpId(), ptr.GetLegId(), ptr.GetSideId())
}
