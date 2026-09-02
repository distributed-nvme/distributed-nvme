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

	// mu guards the in-memory mirrors of the local store below. It is a
	// leaf lock: never held across an OS call.
	mu    sync.Mutex
	dns   map[string]*dnState
	sides map[string]*sideState

	// rootCtx is the process lifetime ctx, captured at Reconcile; the DN8
	// background retries hang off it so shutdown stops them.
	rootCtx context.Context
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
	appliedMigrDst *pb.SyncupSideRequest_MigrDstConf

	// retrying/cancel drive the DN8 background migration-connect retry.
	retrying bool
	cancel   context.CancelFunc

	// fenceAt is when this side's per-CN dm-linears were suspended for the
	// §11.2 src cutover; zero when no fence is in progress. fenceTimer
	// re-runs the converge at the end of the window so the RPC never waits
	// for it. Both are in-memory only: after a restart a suspended linear
	// has no recorded start, and DN12 treats that as "the window is over"
	// rather than starting a second one — the whole point of the bound is
	// that no dnv device stays suspended indefinitely.
	fenceAt    time.Time
	fenceTimer *time.Timer
	// fenceRestarted marks a side reloaded from the local store: any
	// suspended linear it owns is debris from the previous process, and is
	// retired at once rather than starting a second window.
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
		nf:        nf,
		store:     agent.NewStore(oc, localStore),
		meta:      NewDiskMeta(oc, disk),
		dm:        agent.NewDm(oc),
		nvmet:     agent.NewNvmet(oc),
		host:      agent.NewNvmeHost(oc),
		locks:     agent.NewLockSet(),
		disk:      disk,
		fenceWait: common.SuspendSeconds * time.Second,
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
