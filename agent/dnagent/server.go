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

	// bg tracks every background goroutine that owns a child process, so
	// agent.Serve can join them after GracefulStop and no orphan
	// `blkdiscard --zeroout` ever outlives the agent (§9.4, update_01.md U4).
	bg sync.WaitGroup

	// mu guards the in-memory mirrors of the local store below. It is a
	// leaf lock: never held across an OS call.
	mu    sync.Mutex
	dns   map[string]*dnState
	sides map[string]*sideState

	// rootCtx is the process lifetime ctx, captured at Reconcile; the DN8
	// background retries and the §9.4 zeroing loops hang off it so shutdown
	// stops them.
	rootCtx context.Context
}

// WaitBackground joins every dn background goroutine. cmd/dnv-agent passes it
// to agent.Serve, which cancels rootCtx and calls it after GracefulStop has
// drained the last RPC (update_01.md U4, ruling R4.13).
func (s *DnAgentServer) WaitBackground() {
	s.bg.Wait()
}

// dnState is one synced DN: its last fully applied request plus the ResInfo
// history of its node-level resources.
type dnState struct {
	req     *pb.SyncupDnRequest
	tracker *agent.ResTracker
}

// sideState is one synced side. chunks mirrors the migr-bm-* files on disk,
// which stay the source of truth for the applied set (SH21).
type sideState struct {
	req     *pb.SyncupSideRequest
	tracker *agent.ResTracker
	chunks  *agent.ChunkSet
	// chunkMigrId identifies the migration the chunks belong to, so chunks
	// left over from an earlier migration are never applied to a new one.
	chunkMigrId uint64
	// applied* is the shape of the last converge, so resources that leave
	// the desired state — a CN dropping out of standby_id_list, a migration
	// role ending — can still be named when they are torn down.
	appliedCnIds   []uint64
	appliedMigrSrc *pb.SyncupSideRequest_MigrSrcConf
	// appliedMigrSrcRaw is the source conf as received, deferred or not.
	// appliedMigrSrc is nil for a deferred one — nothing was built — but the
	// role still registered its migr_src_* rows, so the tracker cleanup has to
	// key on this one or a role that ends while deferred leaks its entries and
	// the next migration inherits their epoch (§11.2, SH14).
	appliedMigrSrcRaw *pb.SyncupSideRequest_MigrSrcConf
	appliedMigrDst    *pb.SyncupSideRequest_MigrDstConf

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
	// cleared by the next successful batch (ruling R4.14).
	zeroErr error

	// fenceAt is when this side's per-CN dm-linears were suspended for the
	// §11.2 src cutover; zero when no fence is in progress. fenceTimer
	// re-runs the converge at the end of the window so the RPC never waits
	// for it. Both are in-memory only: after a restart a suspended linear
	// has no recorded start, and DN12 treats that as "the window is over"
	// rather than starting a second one — the whole point of the bound is
	// that no dnv device stays suspended indefinitely.
	fenceAt    time.Time
	fenceTimer *time.Timer
	// fenceRestarted marks a side reloaded from the local store whose per-CN
	// linears this process found suspended: they are debris from the previous
	// one, and are retired at once rather than starting a second window. It is
	// set only for those sides — a blanket flag would silently skip the first
	// real cutover window after any restart (adoptFence).
	fenceRestarted bool
}

// NewDnAgentServer builds the dn role server. oc is the process's single
// OsClient, localStore the --local-store prefix (the same one nf was built
// with), disk the --disk device and trConf the node's single nvmet port.
func NewDnAgentServer(
	oc common.OsClient,
	nf *common.NameFmt,
	localStore string,
	disk string,
	trConf *pb.NvmeTrConf,
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
		port: agent.PortConf{
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
		if st.req.GetClusterId() == clusterId && st.req.GetDnId() == dnId {
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

// GetDnInfo probes fresh live state and never mutates (DN16).
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
	return &pb.GetDnInfoReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
		DnInfo:     s.probeDn(ctx, st),
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
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
		SideInfo:   s.probeSide(ctx, st),
	}, nil
}

func sidePointerText(ptr *pb.SidePointer) string {
	return fmt.Sprintf("(sp %d, leg %d, side %d)",
		ptr.GetSpId(), ptr.GetLegId(), ptr.GetSideId())
}
