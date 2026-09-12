package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// dnDriver is the dn role's half of the per-object loop (RW13, §8.2). One
// per DN: cluster_id and dn_id come from the DnRev key, addr_port and
// revision from its value.
type dnDriver struct {
	deps *deps
	cid  uint64
	dnId uint64

	addr     string
	revision uint64

	// mu guards lastInfo, the latest DnInfo the agent reported (RW2). Health
	// is evaluated on it, not only on the info a given reply carried (HL5).
	//
	// A CheckDn reply is decoded by fold on the STREAM'S PUMP goroutine
	// (dnCheckStream.recv) and a SyncupDn reply by fold on the LOOP's, while
	// the loop reads the field in observe and rewrites the message's rows in
	// place in unreachable (§9.5). They overlap on the RW4 step 4 path:
	// dropStream does not join the pump, so a late reply can still be inside
	// fold while fail() reports the object unreachable. Unguarded, that loses
	// the round's ERROR rows and HL1 never sets err_epoch.
	mu       sync.Mutex
	lastInfo *pb.DnInfo

	health *healthMonitor
}

// storeInfo remembers the latest DnInfo (HL5); info returns it. See the mu
// comment above for why the pointer is guarded.
func (d *dnDriver) storeInfo(info *pb.DnInfo) {
	d.mu.Lock()
	d.lastInfo = info
	d.mu.Unlock()
}

func (d *dnDriver) info() *pb.DnInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastInfo
}

// markInfoUnknown is §9.5's "no answer from the node" applied in place to the
// last known info, under the lock a concurrent fold takes (HL1).
func (d *dnDriver) markInfoUnknown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	markDnUnknown(d.lastInfo)
}

// newDnDriver builds the dn driver of one revision worker (RW13). host is the
// generic loop this driver belongs to; a DN drives no bitmap pushes and no
// flips, so it never calls back into it (unlike the sp children of §8.4).
func newDnDriver(p revWorkerParams, host *revWorker) objDriver {
	d := &dnDriver{
		deps:     p.deps,
		cid:      p.cid,
		dnId:     p.id,
		addr:     p.desired.handle,
		revision: p.desired.revision,
	}
	d.health = newDnMonitor(p.deps, p.cid, p.id, d.addrPort)
	return d
}

// logAttrs are the DN's ids as the §12 records carry them.
func (d *dnDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("dn_id", d.dnId),
	}
}

// childAttrs is empty: only the sp children carry a pointer attribute.
func (d *dnDriver) childAttrs() []slog.Attr {
	return nil
}

// addrPort is the DN agent's endpoint (RW13).
func (d *dnDriver) addrPort() string {
	return d.addr
}

// interval is the DN round period, health_check_conf.dn_interval of the
// resolved cluster conf (RW9).
func (d *dnDriver) interval(cc *pb.ClusterConf) time.Duration {
	return roundPeriod(cc.GetHealthCheckConf().GetDnInterval())
}

// setDesired installs a new revision and endpoint (RW3). A put whose only
// change is addr_port re-syncs the node at its new endpoint: the loop's
// connect drops the stream and the connection reference and continues there
// (RW13, §10.2). No delete ever reaches the agent.
func (d *dnDriver) setDesired(next desiredState) {
	d.revision = next.revision
	d.addr = next.handle
}

// openStream opens the DN's CheckDn stream (RW4 step 1, architecture.md
// §9.7).
func (d *dnDriver) openStream(
	ctx context.Context,
	conn *grpc.ClientConn,
) (checkStream, error) {
	stream, err := pb.NewDiskNodeAgentClient(conn).CheckDn(ctx)
	if err != nil {
		return nil, fmt.Errorf("check dn stream: %w", err)
	}
	return &dnCheckStream{driver: d, stream: stream}, nil
}

// syncup issues one SyncupDn (RW5, RW13). Per syncup it reads the DN's
// DnConf at DnConfKey(cluster_id, addr_port) with a plain Get; a missing
// record is logged and skipped, and the next round retries.
func (d *dnDriver) syncup(
	ctx context.Context,
	conn *grpc.ClientConn,
	cc *pb.ClusterConf,
) (*replyState, error) {
	dnConf := &pb.DnConf{}
	found, err := d.deps.store.Get(
		ctx, model.DnConfKey(d.cid, d.addr), dnConf,
	)
	if err != nil {
		// The "etcd get" record carries the error; no SyncupDn was issued, so
		// there is no syncup result to report. Retried next round (RW12).
		return nil, nil
	}
	if !found {
		slog.InfoContext(ctx, "dn conf missing",
			slog.Uint64("cluster_id", d.cid),
			slog.Uint64("dn_id", d.dnId),
			slog.String("addr_port", d.addr),
		)
		return nil, nil
	}
	reply, err := pb.NewDiskNodeAgentClient(conn).SyncupDn(
		ctx, dnSyncupRequest(d.cid, d.dnId, d.revision, dnConf, cc),
	)
	if err != nil {
		return nil, fmt.Errorf("syncup dn: %w", err)
	}
	return d.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetDnInfo(),
	), nil
}

// observe folds one CheckDn/SyncupDn reply into the DN's health (HL1, HL4).
func (d *dnDriver) observe(ctx context.Context, r *replyState) {
	obs, res := dnObservation(r.code, d.info())
	d.health.observe(ctx, obs, res)
}

// unreachable folds a broken stream or a missed reply into the DN's health
// (HL1). The in-memory info is marked RES_STATUS_UNKNOWN — what the worker
// records itself while the stream is dead (§9.5) — and never written to etcd.
func (d *dnDriver) unreachable(ctx context.Context) {
	d.markInfoUnknown()
	d.health.observe(ctx, healthUnreachable, "")
}

// fold turns one reply into a replyState, remembering the DnInfo it carried
// (HL5: a show_info = false reply carries one only when something changed).
func (d *dnDriver) fold(
	agentReply *pb.AgentReply,
	revision uint64,
	info *pb.DnInfo,
) *replyState {
	if info != nil {
		d.storeInfo(info)
	}
	return &replyState{
		revision:    revision,
		code:        agentReply.GetCode(),
		details:     agentReply.GetDetails(),
		infoPresent: info != nil,
	}
}

// dnCheckStream adapts the generated CheckDn stream to checkStream (RW4).
type dnCheckStream struct {
	driver *dnDriver
	stream grpc.BidiStreamingClient[pb.CheckDnRequest, pb.CheckDnReply]
}

func (s *dnCheckStream) send(revision uint64, showInfo bool) error {
	req := dnCheckRequest(s.driver.cid, s.driver.dnId, revision, showInfo)
	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("check dn send: %w", err)
	}
	return nil
}

func (s *dnCheckStream) recv() (*replyState, error) {
	reply, err := s.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("check dn recv: %w", err)
	}
	return s.driver.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetDnInfo(),
	), nil
}

func (s *dnCheckStream) closeSend() error {
	return s.stream.CloseSend()
}

// dnSyncupRequest builds the SyncupDn request of RW13: the side pointer list
// is DnConf's, authoritative and complete (§9.1), and extent_size is the
// STORED dn_bin_conf's, validated by the loop's pass gate before this is ever
// built (§7). Nothing here substitutes a default, and it matters more here
// than anywhere: the dn agent formats every disk header with this number
// (§3.1), so shipping a zero — or a constant this binary happens to carry —
// would be a geometry the rest of the cluster does not share.
func dnSyncupRequest(
	cid uint64,
	dnId uint64,
	revision uint64,
	dnConf *pb.DnConf,
	cc *pb.ClusterConf,
) *pb.SyncupDnRequest {
	return &pb.SyncupDnRequest{
		ClusterId:       cid,
		DnId:            dnId,
		Revision:        revision,
		SidePointerList: dnConf.GetSidePtrList(),
		ExtentSize:      cc.GetDnBinConf().GetExtentSize(),
	}
}

// dnCheckRequest builds one CheckDn round request (RW4 step 2, RW13).
func dnCheckRequest(
	cid uint64,
	dnId uint64,
	revision uint64,
	showInfo bool,
) *pb.CheckDnRequest {
	return &pb.CheckDnRequest{
		ClusterId: cid,
		DnId:      dnId,
		Revision:  revision,
		ShowInfo:  showInfo,
	}
}
