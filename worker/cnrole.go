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

// cnDriver is the cn role's half of the per-object loop (§8.3), the mirror
// image of dnDriver: CnConf at CnConfKey, SyncupCn carrying the CN's
// cntlr_pointer_list and the cluster's qos_ratio, CheckCn rounds, HL1 health.
type cnDriver struct {
	deps *deps
	cid  uint64
	cnId uint64

	addr     string
	revision uint64

	// mu guards lastInfo, the latest CnInfo the agent reported (RW2, HL5).
	// fold runs on the stream's PUMP goroutine for a CheckCn reply and on the
	// LOOP's for a SyncupCn one, while the loop reads the field in observe and
	// rewrites the message's rows in place in unreachable (§9.5) — see
	// dnDriver.mu for the RW4 step 4 overlap the lock closes.
	mu       sync.Mutex
	lastInfo *pb.CnInfo

	health *healthMonitor
}

// storeInfo remembers the latest CnInfo (HL5); info returns it.
func (d *cnDriver) storeInfo(info *pb.CnInfo) {
	d.mu.Lock()
	d.lastInfo = info
	d.mu.Unlock()
}

func (d *cnDriver) info() *pb.CnInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastInfo
}

// markInfoUnknown is §9.5's "no answer from the node" applied in place to the
// last known info, under the lock a concurrent fold takes (HL1).
func (d *cnDriver) markInfoUnknown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	markCnUnknown(d.lastInfo)
}

// newCnDriver builds the cn driver of one revision worker (§8.3). host is the
// generic loop this driver belongs to; like the dn driver it never calls back
// into it.
func newCnDriver(p revWorkerParams, host *revWorker) objDriver {
	d := &cnDriver{
		deps:     p.deps,
		cid:      p.cid,
		cnId:     p.id,
		addr:     p.desired.handle,
		revision: p.desired.revision,
	}
	d.health = newCnMonitor(p.deps, p.cid, p.id, d.addrPort)
	return d
}

// logAttrs are the CN's ids as the §12 records carry them.
func (d *cnDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("cn_id", d.cnId),
	}
}

// childAttrs is empty: only the sp children carry a pointer attribute.
func (d *cnDriver) childAttrs() []slog.Attr {
	return nil
}

// addrPort is the CN agent's endpoint.
func (d *cnDriver) addrPort() string {
	return d.addr
}

// interval is the CN round period, health_check_conf.cn_interval of the
// resolved cluster conf (RW9).
func (d *cnDriver) interval(cc *pb.ClusterConf) time.Duration {
	return roundPeriod(cc.GetHealthCheckConf().GetCnInterval())
}

// setDesired installs a new revision and endpoint (RW3). A moved CN re-syncs
// at its new endpoint exactly like a moved DN (§10.2).
func (d *cnDriver) setDesired(next desiredState) {
	d.revision = next.revision
	d.addr = next.handle
}

// openStream opens the CN's CheckCn stream (RW4 step 1).
func (d *cnDriver) openStream(
	ctx context.Context,
	conn *grpc.ClientConn,
) (checkStream, error) {
	stream, err := pb.NewControllerNodeAgentClient(conn).CheckCn(ctx)
	if err != nil {
		return nil, fmt.Errorf("check cn stream: %w", err)
	}
	return &cnCheckStream{driver: d, stream: stream}, nil
}

// syncup issues one SyncupCn (RW5, §8.3). A missing CnConf is logged and
// skipped, and the next round retries.
func (d *cnDriver) syncup(
	ctx context.Context,
	conn *grpc.ClientConn,
	cc *pb.ClusterConf,
) (*replyState, error) {
	cnConf := &pb.CnConf{}
	found, err := d.deps.store.Get(
		ctx, model.CnConfKey(d.cid, d.addr), cnConf,
	)
	if err != nil {
		// The "etcd get" record carries the error; no SyncupCn was issued.
		return nil, nil
	}
	if !found {
		slog.InfoContext(ctx, "cn conf missing",
			slog.Uint64("cluster_id", d.cid),
			slog.Uint64("cn_id", d.cnId),
			slog.String("addr_port", d.addr),
		)
		return nil, nil
	}
	reply, err := pb.NewControllerNodeAgentClient(conn).SyncupCn(
		ctx, cnSyncupRequest(d.cid, d.cnId, d.revision, cnConf, cc),
	)
	if err != nil {
		return nil, fmt.Errorf("syncup cn: %w", err)
	}
	return d.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetCnInfo(),
	), nil
}

// observe folds one CheckCn/SyncupCn reply into the CN's health (HL1, HL4).
func (d *cnDriver) observe(ctx context.Context, r *replyState) {
	obs, res := cnObservation(r.code, d.info())
	d.health.observe(ctx, obs, res)
}

// unreachable folds a broken stream or a missed reply into the CN's health
// (HL1, §9.5).
func (d *cnDriver) unreachable(ctx context.Context) {
	d.markInfoUnknown()
	d.health.observe(ctx, healthUnreachable, "")
}

// fold turns one reply into a replyState, remembering the CnInfo it carried
// (HL5).
func (d *cnDriver) fold(
	agentReply *pb.AgentReply,
	revision uint64,
	info *pb.CnInfo,
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

// cnCheckStream adapts the generated CheckCn stream to checkStream (RW4).
type cnCheckStream struct {
	driver *cnDriver
	stream grpc.BidiStreamingClient[pb.CheckCnRequest, pb.CheckCnReply]
}

func (s *cnCheckStream) send(revision uint64, showInfo bool) error {
	req := cnCheckRequest(s.driver.cid, s.driver.cnId, revision, showInfo)
	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("check cn send: %w", err)
	}
	return nil
}

func (s *cnCheckStream) recv() (*replyState, error) {
	reply, err := s.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("check cn recv: %w", err)
	}
	return s.driver.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetCnInfo(),
	), nil
}

func (s *cnCheckStream) closeSend() error {
	return s.stream.CloseSend()
}

// cnSyncupRequest builds the SyncupCn request of §8.3: the cntlr pointer list
// is CnConf's, authoritative and complete (§9.1), and qos_ratio is the
// cluster conf's as stored (RW9).
func cnSyncupRequest(
	cid uint64,
	cnId uint64,
	revision uint64,
	cnConf *pb.CnConf,
	cc *pb.ClusterConf,
) *pb.SyncupCnRequest {
	return &pb.SyncupCnRequest{
		ClusterId:        cid,
		CnId:             cnId,
		Revision:         revision,
		CntlrPointerList: cnConf.GetCntlrPtrList(),
		QosRatio:         cc.GetQosRatio(),
	}
}

// cnCheckRequest builds one CheckCn round request (RW4 step 2).
func cnCheckRequest(
	cid uint64,
	cnId uint64,
	revision uint64,
	showInfo bool,
) *pb.CheckCnRequest {
	return &pb.CheckCnRequest{
		ClusterId: cid,
		CnId:      cnId,
		Revision:  revision,
		ShowInfo:  showInfo,
	}
}
