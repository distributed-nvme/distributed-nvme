package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md GW20, the status read: GetClusterStatus of
// architecture.md, Clusters, which lists what is wrong in a cluster, a page
// of pools at a time, from etcd and the latest reports of the pools'
// primaries, and stores nothing.
//
// It is the one handler that calls agents for many objects in one request,
// so it bounds them itself: StatusFanOut pools at once, StatusAgentTimeout a
// call, and the first call to an address alone, the others skipping the
// address when that call fails (statusGate), so a node that hangs costs one
// budget per read. An agent that does not answer is a row, never the RPC's
// error; only the cluster's own reads answer ABORTED.

// The status read's bounds (gateway.md GW20), variables so a test can
// shorten them: the budget of one agent call, the pools read at once, and
// the time the read keeps back before the request's deadline to answer.
var (
	statusAgentBudget = common.StatusAgentTimeout * time.Second
	statusFanOut      = common.StatusFanOut
	statusReplyMargin = 500 * time.Millisecond
)

// GetClusterStatus is the GetClusterStatus of architecture.md, Clusters
// (gateway.md GW20).
//
// The page arguments are judged before the cluster read (GW10); the
// cluster's conf is read plainly, as the capacity read reads it, and judged
// as every reader that computes from it judges it, since the stale rule
// reads its cntlr interval and the grow checks its extent size and batch.
// The pools are paged as ListStoragePools pages them (pageNames), the first
// page also carries the cluster rows, and each pool is loaded with
// model.LoadSp, its own snapshot, as the worker loads it. When the request
// has a deadline, the read starts no pool once less than one agent budget
// and the reply margin is left, the page's first pool aside, ends each
// pool's work the margin before the deadline, and leaves off the page a
// pool whose work that deadline cut and every pool after it, the first again
// aside (readPools); a page cut so carries the token after the last pool it
// keeps, which is a key of the pool prefix, so the next page starts at the
// first pool it did not finish.
func (s *Server) GetClusterStatus(
	ctx context.Context,
	req *pb.GetClusterStatusRequest,
) (*pb.GetClusterStatusReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validatePageArgs(
		req.GetCount(), req.GetPageToken(),
	); err != nil {
		return nil, err
	}
	readTime := time.Now()
	name := clusterNameOf(req.GetClusterName())
	cc := &pb.ClusterConf{}
	found, err := s.cli.Get(ctx, model.ClusterConfKey(name), cc)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	if !found {
		return nil, errNotFound("cluster %q not found", name)
	}
	if err := model.ValidateClusterConf(cc); err != nil {
		return nil, errAborted("%v", err)
	}
	cid := model.ClusterId(name, cc.GetCreationEpoch())
	prefix := model.SpConfPrefix(cid)
	names, token, err := pageNames(
		ctx, s.cli, prefix, req.GetCount(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	reply := &pb.GetClusterStatusReply{
		ClusterName: name,
		ReadTime:    timestamppb.New(readTime),
	}
	if req.GetPageToken() == "" {
		rows, err := s.statusClusterRows(ctx, cid, readTime)
		if err != nil {
			return nil, err
		}
		reply.RowList = rows
	}
	read := &statusRead{
		s:        s,
		cid:      cid,
		cc:       cc,
		readTime: readTime,
		gate:     &statusGate{first: map[string]*statusCall{}},
		room:     map[statusRoomKey]bool{},
	}
	pools, started := read.readPools(ctx, names)
	for _, pool := range pools[:started] {
		if !pool.read {
			continue
		}
		reply.SpCnt++
		reply.RowList = append(reply.RowList, pool.rows...)
	}
	if started < len(names) {
		token = encodePageToken(prefix + names[started-1])
	}
	reply.PageToken = token
	return reply, nil
}

// ---------------------------------------------------------------------------
// The cluster rows (the first page)
// ---------------------------------------------------------------------------

// statusClusterRows are the rows of the first page that belong to no pool:
// the worker roles, then the nodes. Each read is a plain range outside any
// STM, as ListWorkers and GetClusterCapacity read; a failed one is ABORTED.
func (s *Server) statusClusterRows(
	ctx context.Context,
	cid uint64,
	readTime time.Time,
) ([]*pb.StatusRow, error) {
	rows, err := s.statusWorkerRows(ctx, readTime)
	if err != nil {
		return nil, err
	}
	for _, node := range []struct {
		what      string
		confPfx   string
		revPfx    string
		parseRev  func(string) (uint32, uint64, uint64, bool)
		revKeyOf  func(shard uint32, id uint64) string
		nodeIdent string
	}{
		{"dn", model.DnConfPrefix(cid), model.DnRevKindPrefix(),
			model.ParseDnRevKey,
			func(shard uint32, id uint64) string {
				return model.DnRevKey(shard, cid, id)
			}, "disk node"},
		{"cn", model.CnConfPrefix(cid), model.CnRevKindPrefix(),
			model.ParseCnRevKey,
			func(shard uint32, id uint64) string {
				return model.CnRevKey(shard, cid, id)
			}, "controller node"},
	} {
		nodes, err := s.statusNodes(ctx, node.what, node.confPfx)
		if err != nil {
			return nil, err
		}
		keys, _, err := s.cli.RangeKeys(ctx, node.revPfx)
		if err != nil {
			return nil, errAborted("%v", err)
		}
		held := map[uint64]bool{}
		for _, entry := range keys {
			_, keyCid, id, ok := node.parseRev(entry.Key)
			if ok && keyCid == cid {
				held[id] = true
			}
		}
		for _, n := range nodes {
			object := node.what + " " + n.addr
			if n.err != nil {
				rows = append(rows, &pb.StatusRow{
					Kind:     pb.StatusKind_STATUS_KIND_LOST_RECORD,
					Object:   object,
					Text:     common.CutRowText("its record does not decode: " + n.err.Error()),
					AddrPort: n.addr,
				})
				continue
			}
			if !held[n.id] {
				rows = append(rows, &pb.StatusRow{
					Kind:   pb.StatusKind_STATUS_KIND_LOST_RECORD,
					Object: object,
					Text: common.CutRowText("its revision key " +
						node.revKeyOf(n.shard, n.id) +
						" is absent: no worker drives the " + node.nodeIdent),
					AddrPort: n.addr,
				})
			}
			if n.errEpoch != 0 {
				rows = append(rows, &pb.StatusRow{
					Kind:     pb.StatusKind_STATUS_KIND_HEALTH,
					Object:   object,
					Text:     "health epoch set: the worker's checks of the " + node.nodeIdent + " fail",
					Age:      epochAge(readTime, n.errEpoch),
					AddrPort: n.addr,
				})
			}
		}
	}
	return rows, nil
}

// statusNode is what the cluster rows read of one node record.
type statusNode struct {
	addr     string
	id       uint64
	shard    uint32
	errEpoch uint64
	err      error
}

// statusNodes reads one node kind's records of the cluster, in key order.
func (s *Server) statusNodes(
	ctx context.Context,
	what string,
	prefix string,
) ([]statusNode, error) {
	kvs, _, err := s.cli.Range(ctx, prefix)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	var out []statusNode
	for _, kv := range kvs {
		node := statusNode{addr: strings.TrimPrefix(kv.Key, prefix)}
		if what == "dn" {
			dn := &pb.DnConf{}
			node.err = s.cli.Decode(ctx, kv, dn)
			node.id, node.shard, node.errEpoch =
				dn.GetDnId(), dn.GetShardCode(), dn.GetErrEpoch()
		} else {
			cn := &pb.CnConf{}
			node.err = s.cli.Decode(ctx, kv, cn)
			node.id, node.shard, node.errEpoch =
				cn.GetCnId(), cn.GetShardCode(), cn.GetErrEpoch()
		}
		out = append(out, node)
	}
	return out, nil
}

// statusWorkerRows are the worker rows: a role with no registration, and a
// registration whose epoch trails the read's time by more than twice the
// default vote interval. The second is a hint by the gateway's clock and
// decides nothing (dnv-worker.md VW4): the gateway does not know a worker's
// own interval, and the epoch is its writer's clock.
func (s *Server) statusWorkerRows(
	ctx context.Context,
	readTime time.Time,
) ([]*pb.StatusRow, error) {
	kvs, _, err := s.cli.Range(ctx, model.WorkerRegistryPrefix())
	if err != nil {
		return nil, errAborted("%v", err)
	}
	type reg struct {
		seed  string
		epoch uint64
	}
	byRole := map[string][]reg{}
	for _, kv := range kvs {
		role, seed, ok := model.ParseWorkerRegKey(kv.Key)
		if !ok {
			continue
		}
		value := &pb.WorkerReg{}
		if s.cli.Decode(ctx, kv, value) != nil {
			value = &pb.WorkerReg{}
		}
		byRole[role] = append(byRole[role], reg{seed, value.GetEpoch()})
	}
	dead := 2 * common.DefaultVoteWorkerInterval * time.Second
	var rows []*pb.StatusRow
	for _, role := range listedWorkerRoles {
		object := "worker role " + role
		if len(byRole[role]) == 0 {
			rows = append(rows, &pb.StatusRow{
				Kind:    pb.StatusKind_STATUS_KIND_WORKER,
				Object:  object,
				Text:    "no worker of the role is registered: nothing does the role's work in any cluster",
				ObjName: role,
			})
			continue
		}
		for _, r := range byRole[role] {
			age := readTime.Sub(time.Unix(int64(r.epoch), 0))
			if age <= dead {
				continue
			}
			rows = append(rows, &pb.StatusRow{
				Kind:   pb.StatusKind_STATUS_KIND_WORKER,
				Object: object,
				Text: common.CutRowText(fmt.Sprintf("worker %s last wrote its "+
					"registration longer ago than twice the default vote "+
					"interval, by the gateway's clock: a hint that it is "+
					"gone", r.seed)),
				Age:     durationpb.New(age),
				ObjName: role,
			})
		}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// The pools of a page
// ---------------------------------------------------------------------------

// statusRead is one request's state: the cluster, the address gate and the
// memo of the disk node scans, which the pools of the request share.
type statusRead struct {
	s        *Server
	cid      uint64
	cc       *pb.ClusterConf
	readTime time.Time
	gate     *statusGate
	roomMu   sync.Mutex
	room     map[statusRoomKey]bool
}

// statusPool is what the read gives of one pool: its rows, and whether it
// counts as read (a pool whose SpConf went between the page's range and its
// load does not).
type statusPool struct {
	rows []*pb.StatusRow
	read bool
	// cut says the read's own deadline ended the pool's work before it
	// was done, so what it holds is not the pool's state.
	cut bool
}

// readPools reads the page's pools, statusFanOut at once, and returns their
// results in key order with how many the page keeps. It starts the first
// always, and the others while the request's deadline leaves one agent
// budget and the reply margin; every pool started ends by the deadline less
// the margin. A pool whose work that deadline ended (cut), and every pool
// after it, are left off the page, the first pool aside, so the page's token
// goes before the first pool the read did not finish and the next page reads
// it again; the first is kept, its lines saying the read's time ran out, so
// a page always moves on.
func (r *statusRead) readPools(
	ctx context.Context,
	names []string,
) ([]statusPool, int) {
	workCtx := ctx
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		var cancel context.CancelFunc
		workCtx, cancel = context.WithDeadline(
			ctx, deadline.Add(-statusReplyMargin))
		defer cancel()
	}
	timeLeft := func() bool {
		return !hasDeadline ||
			time.Until(deadline) > statusAgentBudget+statusReplyMargin
	}
	out := make([]statusPool, len(names))
	slots := make(chan struct{}, max(statusFanOut, 1))
	var wg sync.WaitGroup
	started := 0
	for idx, name := range names {
		if idx > 0 && !timeLeft() {
			break
		}
		slots <- struct{}{}
		if idx > 0 && !timeLeft() {
			<-slots
			break
		}
		started++
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			out[idx] = r.readPool(workCtx, name)
			out[idx].cut = hasDeadline && workCtx.Err() != nil
		}()
	}
	wg.Wait()
	for idx := 1; idx < started; idx++ {
		if out[idx].cut {
			return out, idx
		}
	}
	return out, started
}

// readPool loads one pool and computes its rows. A pool whose load fails
// gives one row of status UNKNOWN; a pool being deleted gives one row and
// no agent call, since its teardown's rows would say nothing the drain
// does not do alone.
func (r *statusRead) readPool(ctx context.Context, name string) statusPool {
	state, err := model.LoadSp(ctx, r.s.cli, r.cid, name)
	if errors.Is(err, model.ErrNotFound) {
		return statusPool{}
	}
	if err != nil {
		text := "pool not read: " + err.Error()
		if ctx.Err() != nil {
			text = "pool not read: the read's time ran out: " + err.Error()
		}
		return statusPool{read: true, rows: []*pb.StatusRow{{
			SpName: name,
			Kind:   pb.StatusKind_STATUS_KIND_REPORT,
			Object: "pool",
			Status: unknownStatus(),
			Text:   common.CutRowText(text),
		}}}
	}
	if state.Conf.GetDeleting() {
		return statusPool{read: true, rows: []*pb.StatusRow{{
			SpName: name,
			Kind:   pb.StatusKind_STATUS_KIND_POOL,
			Object: "pool",
			Text: "being deleted: the worker's drain takes its records " +
				"away, and no other row of it is read",
		}}}
	}
	in := &statusInput{
		spName:   name,
		cid:      r.cid,
		state:    state,
		cc:       r.cc,
		readTime: r.readTime,
		dst:      map[string]*statusSideReply{},
		dnRoom: func(extCnt uint64, legs int) (bool, error) {
			return r.dnRoom(ctx, extCnt, legs)
		},
		ended: func() bool { return ctx.Err() != nil },
	}
	in.primaryId, in.primary, _ = primaryCntlrOf(state)
	var wg sync.WaitGroup
	if in.primary != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.report = r.callPrimary(ctx, state, in.primaryId, in.primary)
		}()
	}
	var dstMu sync.Mutex
	for _, migrName := range state.Conf.GetMigrNameList() {
		migr, ok := state.Migrs[migrName]
		if !ok || migr.GetCutoverPending() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := r.callMigrDst(ctx, state, migr)
			dstMu.Lock()
			in.dst[migrName] = got
			dstMu.Unlock()
		}()
	}
	wg.Wait()
	if ctx.Err() == nil {
		in.nameErr = r.spNameRow(ctx, in)
		in.nameCut = in.nameErr != nil && ctx.Err() != nil
	}
	return statusPool{read: true, rows: capStatusRows(name, statusPoolRows(in))}
}

// spNameRow reads the pool's id-to-name record, which no list names and so
// LoadSp does not report missing: without it the worker's write of a
// cntlr's health epoch cannot rewrite the pool's discovery entries. It sets
// in.nameLost when the record is absent or names another pool, and returns
// the read's error.
func (r *statusRead) spNameRow(ctx context.Context, in *statusInput) error {
	rec := &pb.SpName{}
	found, err := r.s.cli.Get(ctx,
		model.SpNameKey(r.cid, in.state.Conf.GetSpId()), rec)
	if err != nil {
		return err
	}
	in.nameLost = !found || rec.GetSpName() != in.spName
	return nil
}

// primaryCntlrOf is the pool's primary cntlr, from the loaded state.
func primaryCntlrOf(state *model.SpState) (uint64, *pb.Cntlr, bool) {
	for _, id := range state.Conf.GetCntlrIdList() {
		if cntlr, ok := state.Cntlrs[id]; ok && cntlr.GetPrimary() {
			return id, cntlr, true
		}
	}
	return 0, nil, false
}

// statusReport is the primary's answer, or why there is none.
type statusReport struct {
	reply *pb.GetCntlrInfoReply
	err   error
	// cut says the read's own deadline ended the call, not the agent.
	cut bool
	// noNode says the primary's controller node record is absent, so the
	// read has no cn_id to call it by.
	noNode bool
}

// callPrimary reads the primary's latest report: GetCntlrInfo, without the
// counters, through the address gate.
func (r *statusRead) callPrimary(
	ctx context.Context,
	state *model.SpState,
	cntlrId uint64,
	cntlr *pb.Cntlr,
) *statusReport {
	addr := cntlr.GetAddrPort()
	cn, ok := state.CnByAddr[addr]
	if !ok {
		return &statusReport{noNode: true}
	}
	out := &statusReport{}
	out.err = r.gate.call(ctx, addr, func(ctx context.Context) error {
		return withCnAgent(ctx, addr,
			func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
				reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
					ClusterId: r.cid,
					CnId:      cn.GetCnId(),
					CntlrPointer: &pb.CntlrPointer{
						SpId:    state.Conf.GetSpId(),
						CntlrId: cntlrId,
					},
				})
				out.reply = reply
				return err
			})
	})
	out.cut = out.err != nil && ctx.Err() != nil
	return out
}

// statusSideReply is a migration destination's answer, or why there is
// none.
type statusSideReply struct {
	reply *pb.GetSideInfoReply
	err   error
	// cut says the read's own deadline ended the call, not the agent.
	cut bool
	// side is the destination side and legId its leg; side is nil when no
	// leg of the pool holds the destination side.
	side  *pb.Side
	legId uint64
}

// callMigrDst reads a migration's destination side past its cutover:
// GetSideInfo on its disk node, through the address gate.
func (r *statusRead) callMigrDst(
	ctx context.Context,
	state *model.SpState,
	migr *pb.Migration,
) *statusSideReply {
	out := &statusSideReply{}
	out.legId, out.side = stateSide(state, migr.GetDstSideId())
	if out.side == nil {
		return out
	}
	addr := out.side.GetAddrPort()
	dn, ok := state.DnByAddr[addr]
	if !ok {
		out.err = fmt.Errorf("its disk node record is absent")
		return out
	}
	out.err = r.gate.call(ctx, addr, func(ctx context.Context) error {
		return withDnAgent(ctx, addr,
			func(ctx context.Context, client pb.DiskNodeAgentClient) error {
				reply, err := client.GetSideInfo(ctx, &pb.GetSideInfoRequest{
					ClusterId: r.cid,
					DnId:      dn.GetDnId(),
					SidePointer: &pb.SidePointer{
						SpId:   state.Conf.GetSpId(),
						LegId:  out.legId,
						SideId: migr.GetDstSideId(),
					},
				})
				out.reply = reply
				return err
			})
	})
	out.cut = out.err != nil && ctx.Err() != nil
	return out
}

// stateSide is the leg of the pool's slices that holds sideId, and the side.
func stateSide(state *model.SpState, sideId uint64) (uint64, *pb.Side) {
	for _, sliceId := range state.Conf.GetSliceIdList() {
		for _, grp := range allGroups(state.Slices[sliceId]) {
			for _, leg := range grp.GetLegList() {
				for _, side := range leg.GetSideList() {
					if side.GetSideId() == sideId {
						return leg.GetLegId(), side
					}
				}
			}
		}
	}
	return 0, nil
}

// statusRoomKey names one disk node scan of a read: a group's extent count
// and its leg count.
type statusRoomKey struct {
	extCnt uint64
	legs   int
}

// dnRoom says whether the disk nodes have room for one new group of extCnt
// extents on legs nodes, by the scan AR6's grow runs (dnv-worker.md AR6):
// the plain scan, the batch the cluster stores, no black list and no
// location excluded. The answer is kept for the request, so a pool that asks
// a scan another pool has run reads its answer; two pools that ask one scan
// at once both run it.
func (r *statusRead) dnRoom(
	ctx context.Context,
	extCnt uint64,
	legs int,
) (bool, error) {
	key := statusRoomKey{extCnt, legs}
	r.roomMu.Lock()
	room, seen := r.room[key]
	r.roomMu.Unlock()
	if seen {
		return room, nil
	}
	batch := int(r.cc.GetAllocConf().GetDnBatchSize())
	cands, _, err := model.FindDnCandidatesAntiAffine(
		ctx, r.s.cli, r.cid, r.cc, extCnt, 0, legs*batch, legs,
		nil, nil, nil,
	)
	if err != nil {
		return false, err
	}
	room = len(cands) >= legs
	r.roomMu.Lock()
	r.room[key] = room
	r.roomMu.Unlock()
	return room, nil
}

// ---------------------------------------------------------------------------
// The address gate
// ---------------------------------------------------------------------------

// statusGate lets the first call of a read to an address run alone: a pool
// whose call names an address another pool's call is on waits for that
// call's end, skips the address if it failed, and calls itself, at once
// with the others, once it answered. So a node that hangs costs one budget
// per read, however many pools it holds a primary or a migration of.
type statusGate struct {
	mu    sync.Mutex
	first map[string]*statusCall
}

// statusCall is the first call of a read to one address.
type statusCall struct {
	done chan struct{}
	err  error
}

// call runs f against addr under the gate, f's ctx bounded by the agent
// budget.
func (g *statusGate) call(
	ctx context.Context,
	addr string,
	f func(ctx context.Context) error,
) error {
	bounded := func() error {
		callCtx, cancel := context.WithTimeout(ctx, statusAgentBudget)
		defer cancel()
		return f(callCtx)
	}
	g.mu.Lock()
	first, seen := g.first[addr]
	if !seen {
		first = &statusCall{done: make(chan struct{})}
		g.first[addr] = first
	}
	g.mu.Unlock()
	if !seen {
		first.err = bounded()
		close(first.done)
		return first.err
	}
	select {
	case <-first.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if first.err != nil {
		return fmt.Errorf("not called, since this read's first call to "+
			"%s failed: %w", addr, first.err)
	}
	return bounded()
}

// ---------------------------------------------------------------------------
// The rows of one pool
// ---------------------------------------------------------------------------

// statusInput is what the rows of one pool are computed from: the loaded
// state, the primary's answer, the answers of the migration destinations
// past their cutover, and the disk node scan, which is the one read the
// computation makes (dnRoom).
type statusInput struct {
	spName    string
	cid       uint64
	state     *model.SpState
	cc        *pb.ClusterConf
	readTime  time.Time
	primaryId uint64
	primary   *pb.Cntlr
	report    *statusReport
	dst       map[string]*statusSideReply
	dnRoom    func(extCnt uint64, legs int) (bool, error)
	// ended says whether the read's own deadline has ended the pool's
	// work (readPools), for a read whose error then is the read's, not
	// the store's or an agent's.
	ended func() bool
	// nameCut says the read's own deadline ended the name read.
	nameCut bool
	// nameLost says the pool's id-to-name record is absent or names
	// another pool, and nameErr is the error of its read (spNameRow).
	nameLost bool
	nameErr  error
}

// The ranks of the rows of one pool (architecture.md, Clusters,
// GetClusterStatus): the order a pool's rows are listed in, and so the
// order its cap keeps. A leg's row ranks before its group's, so a node's
// fault is listed by the legs it holds before the groups they touch.
const (
	rankLost = iota
	rankReport
	rankPool
	rankSpace
	rankSliceRow
	rankGrow
	rankCntlr
	rankLeg
	rankGroupRow
	rankGroup
	rankRepair
	rankMigration
	rankClone
	rankOtherRow
	rankTorn
)

// statusRow is a row with its rank, before the order and the cap.
type statusRow struct {
	row  *pb.StatusRow
	rank int
}

// statusPoolRows computes the rows of one pool, unordered.
func statusPoolRows(in *statusInput) []statusRow {
	b := &statusBuilder{in: in, used: map[statusRowKey]bool{}}
	b.lostRows()
	b.poolRows()
	info := b.reportRows()
	b.healthAndLegRows(info)
	if info != nil {
		b.groupRows(info)
		b.spaceAndGrowRows(info)
	} else {
		b.etcdGroupRows()
	}
	b.repairRows(info)
	b.migrationRows(info)
	b.cloneRows(info)
	b.tornRows()
	if info != nil {
		b.errorRows(info)
	}
	return b.rows
}

// statusRowKey names one row of the primary's report, so a row a merged
// row already shows is not listed again.
type statusRowKey struct {
	table string
	id    uint64
}

// statusBuilder accumulates the rows of one pool.
type statusBuilder struct {
	in   *statusInput
	rows []statusRow
	used map[statusRowKey]bool
	// answer is the agent's clock at its answer, the time a report row's
	// epoch is aged against; zero when the read has no report.
	answer time.Time
	// legRowed holds the legs that got a row of their own.
	legRowed map[uint64]bool
	// badConf says the pool's stored conf is one the sp worker's pass
	// refuses (poolRows): a bdev_conf the readers refuse, or a group whose
	// counts give its sides no length to zero. No grow is judged from it,
	// and the worker drives none of its repairs (repairRows).
	badConf bool
}

// add appends one row of the pool.
func (b *statusBuilder) add(rank int, row *pb.StatusRow) {
	row.SpName = b.in.spName
	row.Text = common.CutRowText(row.Text)
	b.rows = append(b.rows, statusRow{row: row, rank: rank})
}

// rowAge is the age of a report row: the agent's answer less the row's
// epoch, both by the node's clock; nil without an answer time or an epoch.
func (b *statusBuilder) rowAge(row *pb.ResInfo) *durationpb.Duration {
	if b.answer.IsZero() || row.GetEpoch() == 0 {
		return nil
	}
	return epochAge(b.answer, row.GetEpoch())
}

// lostRows are the pool's lost records (architecture.md, Clusters,
// GetClusterStatus): each key LoadSp found named and absent, the pool's
// id-to-name record absent or naming another pool (spNameRow), and an absent
// revision key, which LoadSp reads as revision zero and a create never
// writes. A failed read of the id-to-name record is a row of status UNKNOWN
// instead.
func (b *statusBuilder) lostRows() {
	state := b.in.state
	dnPfx, cnPfx := model.DnConfPrefix(b.in.cid), model.CnConfPrefix(b.in.cid)
	for _, key := range state.Missing {
		text := "the pool's records name this key and etcd does not " +
			"hold it: the worker sends the pool nothing; never remove " +
			"the name from its list"
		addr := ""
		switch {
		case strings.HasPrefix(key, dnPfx):
			addr = strings.TrimPrefix(key, dnPfx)
			text = "a side of the pool names this disk node and etcd " +
				"holds no record of it"
		case strings.HasPrefix(key, cnPfx):
			addr = strings.TrimPrefix(key, cnPfx)
			text = "a cntlr of the pool names this controller node and " +
				"etcd holds no record of it"
		}
		b.add(rankLost, &pb.StatusRow{
			Kind:     pb.StatusKind_STATUS_KIND_LOST_RECORD,
			Object:   key,
			Text:     text,
			AddrPort: addr,
		})
	}
	switch {
	case b.in.nameErr != nil:
		text := "the pool's id-to-name record not read: " +
			b.in.nameErr.Error()
		if b.in.nameCut {
			text = "the pool's id-to-name record not read: the read's " +
				"time ran out"
		}
		b.add(rankReport, &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_REPORT,
			Object: model.SpNameKey(b.in.cid, state.Conf.GetSpId()),
			Status: unknownStatus(),
			Text:   text,
		})
	case b.in.nameLost:
		b.add(rankLost, &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_LOST_RECORD,
			Object: model.SpNameKey(b.in.cid, state.Conf.GetSpId()),
			Text: "the pool's id-to-name record is absent or names another " +
				"pool: the worker cannot rewrite the pool's discovery " +
				"entries when a cntlr's health changes",
		})
	}
	if state.SpRevision == 0 {
		b.add(rankLost, &pb.StatusRow{
			Kind: pb.StatusKind_STATUS_KIND_LOST_RECORD,
			Object: model.SpRevKey(state.Conf.GetShardCode(), b.in.cid,
				state.Conf.GetSpId()),
			Text: "the pool's revision key is absent: no worker drives " +
				"the pool",
		})
	}
}

// groupZeroBytesErr is the sp worker's gate on its sides' length to zero
// (dnv-worker.md AR1, RW14), read from the same pure rules: the error of the
// first group, a slice's or a clone meta group's, whose stored counts give
// its sides no length, or nil.
func groupZeroBytesErr(state *model.SpState) error {
	blockSize := model.PoolBlockSize(state.Conf.GetBdevConf())
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range slice.GetMetaGrpList() {
			if _, err := model.SideZeroBytes(grp, true, blockSize); err != nil {
				return err
			}
		}
		for _, grp := range slice.GetDataGrpList() {
			if _, err := model.SideZeroBytes(grp, false, blockSize); err != nil {
				return err
			}
		}
	}
	for _, name := range state.Conf.GetCloneNameList() {
		clone, ok := state.Clones[name]
		if !ok || clone.GetMetaGrp() == nil {
			continue
		}
		if _, err := model.CloneMetaSideZeroBytes(
			clone.GetMetaGrp(), blockSize,
		); err != nil {
			return err
		}
	}
	return nil
}

// poolRows are the rows of the pool as a whole, from etcd alone: a stored
// conf the worker refuses, a level above read-write, and a primary that is
// disabled.
func (b *statusBuilder) poolRows() {
	state := b.in.state
	err := model.ValidateBdevConf(state.Conf.GetBdevConf())
	if err == nil {
		err = groupZeroBytesErr(state)
	}
	if err != nil {
		b.badConf = true
		b.add(rankPool, &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_POOL,
			Object: "pool",
			Text:   err.Error() + ": the worker drives the pool no more",
		})
	}
	if level := state.Conf.GetSpLevel(); level >
		pb.SpLevel_SP_LEVEL_READWRITE {
		text := "level " + level.String() + ": writes to its volumes fail"
		if level >= pb.SpLevel_SP_LEVEL_NO_THINPOOL {
			text += ", and the worker runs no reaction for the pool"
		}
		b.add(rankPool, &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_POOL,
			Object: "pool",
			Text:   text,
		})
	}
	if b.in.primary != nil && b.in.primary.GetDisabled() {
		text := "the primary is disabled: its volumes serve no IO until " +
			"a failover moves the role"
		enabled := false
		for _, cntlr := range state.Cntlrs {
			enabled = enabled || !cntlr.GetDisabled()
		}
		if !enabled {
			text += "; no cntlr of the pool is enabled, so no failover " +
				"can follow"
		}
		b.add(rankPool, &pb.StatusRow{
			Kind:     pb.StatusKind_STATUS_KIND_POOL,
			Object:   cntlrObject(b.in.primaryId, b.in.primary),
			Text:     text,
			CntlrId:  b.in.primaryId,
			AddrPort: b.in.primary.GetAddrPort(),
		})
	}
}

// reportRows are the rows that say the read has no state of the pool to
// judge by, status UNKNOWN, and returns the primary's report when it has
// one to read: an answer with code zero or a leftover, whose observation
// may be old or behind the pool's revision (a row says so, and its rows
// are still listed). An info read answers from the agent's last
// observation, made before a syncup in flight and kept past a refused one,
// so an agent that refuses the pool's syncups, as one whose store refuses
// its saves, shows only as a report behind the pool's revision beside the
// cntlr's health epoch, as a syncup still on its way does too; the row says
// both.
func (b *statusBuilder) reportRows() *pb.CntlrInfo {
	in := b.in
	unknown := func(text string) {
		row := &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_REPORT,
			Object: "pool",
			Status: unknownStatus(),
			Text:   text,
		}
		if in.primary != nil {
			row.Object = cntlrObject(in.primaryId, in.primary)
			row.CntlrId = in.primaryId
			row.AddrPort = in.primary.GetAddrPort()
		}
		b.add(rankReport, row)
	}
	switch {
	case in.primary == nil:
		unknown("no cntlr of the pool is primary: nothing serves its volumes")
		return nil
	case in.report == nil || in.report.noNode:
		unknown("not called: the primary's controller node record is absent")
		return nil
	case in.report.err != nil && in.report.cut:
		unknown("not read: the read's time ran out before the primary's " +
			"agent answered: " + in.report.err.Error())
		return nil
	case in.report.err != nil:
		unknown("the primary's agent did not answer: " +
			in.report.err.Error())
		return nil
	}
	reply := in.report.reply
	code := reply.GetAgentReply().GetCode()
	switch code {
	case 0, common.ReplyCodeLeftover:
	case common.ReplyCodeUnknownObject:
		unknown("the primary's agent holds no state of the cntlr")
		return nil
	case common.ReplyCodeStarting:
		unknown("the primary's agent is starting")
		return nil
	default:
		text := fmt.Sprintf("the primary's agent answers code %d", code)
		if details := reply.GetAgentReply().GetDetails(); details != "" {
			text += ": " + details
		}
		unknown(text)
		return nil
	}
	if at := reply.GetObsStamp().GetAnswerTime(); at != nil {
		b.answer = at.AsTime()
	}
	if reply.GetCntlrInfo() == nil {
		unknown("the primary's agent sent no report")
		return nil
	}
	age := obsAge(reply.GetObsStamp())
	if age == nil {
		unknown("the primary's agent holds no observation of the cntlr yet")
		return reply.GetCntlrInfo()
	}
	limit := time.Duration(common.ObsMaxAgeIntervals*
		in.cc.GetHealthCheckConf().GetCntlrInterval()) * time.Second
	if age.AsDuration() > limit {
		row := &pb.StatusRow{
			Kind:     pb.StatusKind_STATUS_KIND_REPORT,
			Object:   cntlrObject(in.primaryId, in.primary),
			Status:   unknownStatus(),
			Text:     "the primary's observation is old: no worker asks it for a read, or the agent's read of the cntlr does not end, which the cntlr's health line then shows; its rows are as it holds them",
			Age:      age,
			CntlrId:  in.primaryId,
			AddrPort: in.primary.GetAddrPort(),
		}
		b.add(rankReport, row)
	}
	if reply.GetRevision() < in.state.SpRevision &&
		in.primary.GetErrEpoch() != 0 {
		b.add(rankReport, &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_REPORT,
			Object: cntlrObject(in.primaryId, in.primary),
			Status: unknownStatus(),
			Text: fmt.Sprintf("report of revision %d, the pool's is %d, "+
				"while the health epoch is set: a syncup still on its way, "+
				"or one the agent refuses or fails, as when its store cannot "+
				"save; see the worker's log", reply.GetRevision(),
				in.state.SpRevision),
			CntlrId:  in.primaryId,
			AddrPort: in.primary.GetAddrPort(),
		})
	}
	return reply.GetCntlrInfo()
}

// healthAndLegRows are the cntlrs' health epochs and one row per leg: a
// leg's health epoch, its sides', its row of the primary's report that
// reads ERROR or carries an identity reason, and the bad blocks md records
// on its member, merged into one row. The legs are the slices' groups', the
// clone meta groups' of clones not being deleted, the repairs' new legs and
// the retired legs.
func (b *statusBuilder) healthAndLegRows(info *pb.CntlrInfo) {
	in, state := b.in, b.in.state
	for _, id := range state.Conf.GetCntlrIdList() {
		cntlr, ok := state.Cntlrs[id]
		if !ok || cntlr.GetErrEpoch() == 0 {
			continue
		}
		b.add(rankCntlr, &pb.StatusRow{
			Kind:     pb.StatusKind_STATUS_KIND_HEALTH,
			Object:   cntlrObject(id, cntlr),
			Text:     "health epoch set: the worker's checks of the cntlr fail",
			Age:      epochAge(in.readTime, cntlr.GetErrEpoch()),
			CntlrId:  id,
			AddrPort: cntlr.GetAddrPort(),
		})
	}
	b.legRowed = map[uint64]bool{}
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range allGroups(slice) {
			for _, leg := range grp.GetLegList() {
				b.legRow(info, leg, grp.GetGrpId(), sliceId, "",
					fmt.Sprintf("group %d, slice %d", grp.GetGrpId(), sliceId))
			}
		}
	}
	for _, name := range state.Conf.GetCloneNameList() {
		clone, ok := state.Clones[name]
		if !ok || clone.GetDeleting() || clone.GetMetaGrp() == nil {
			continue
		}
		grp := clone.GetMetaGrp()
		for _, leg := range grp.GetLegList() {
			b.legRow(info, leg, grp.GetGrpId(), 0, "",
				fmt.Sprintf("group %d, clone %s", grp.GetGrpId(), name))
		}
	}
	for _, name := range state.Conf.GetRepairNameList() {
		rep, ok := state.Repairs[name]
		if !ok || repairOfDeletingClone(state, rep) {
			continue
		}
		for _, leg := range rep.GetNewLegList() {
			b.legRow(info, leg, rep.GetGrpId(), rep.GetSliceId(), name,
				fmt.Sprintf("new leg of repair %s, group %d",
					name, rep.GetGrpId()))
		}
	}
	for _, legId := range state.Conf.GetRetiredLegIdList() {
		retired, ok := state.RetiredLegs[legId]
		if !ok || retired.GetSide().GetErrEpoch() == 0 {
			continue
		}
		side := retired.GetSide()
		b.add(rankLeg, &pb.StatusRow{
			Kind: pb.StatusKind_STATUS_KIND_HEALTH,
			Object: fmt.Sprintf("retired leg %d (group %d; side %d on dn %s)",
				legId, retired.GetGrpId(), side.GetSideId(),
				side.GetAddrPort()),
			Text:     "health epoch set on its side",
			Age:      epochAge(in.readTime, side.GetErrEpoch()),
			SliceId:  retired.GetSliceId(),
			GrpId:    retired.GetGrpId(),
			LegId:    legId,
			SideId:   side.GetSideId(),
			ObjName:  retired.GetCloneName(),
			AddrPort: side.GetAddrPort(),
		})
	}
}

// legRow is the one merged row of one leg, when anything of it is wrong.
func (b *statusBuilder) legRow(
	info *pb.CntlrInfo,
	leg *pb.Leg,
	grpId uint64,
	sliceId uint64,
	repName string,
	where string,
) {
	legId := leg.GetLegId()
	var parts []string
	var age *durationpb.Duration
	older := func(d *durationpb.Duration) {
		if d != nil && (age == nil || d.AsDuration() > age.AsDuration()) {
			age = d
		}
	}
	kind := pb.StatusKind_STATUS_KIND_NONE
	row, haveRow := info.GetLegIdToLeg()[legId]
	listed := haveRow && legRowListed(row)
	if listed {
		kind = pb.StatusKind_STATUS_KIND_ROW
		b.used[statusRowKey{"leg", legId}] = true
		parts = append(parts, legRowText(row))
		older(b.rowAge(row))
	}
	if leg.GetErrEpoch() != 0 {
		if kind == pb.StatusKind_STATUS_KIND_NONE {
			kind = pb.StatusKind_STATUS_KIND_HEALTH
		}
		parts = append(parts, "health epoch set on the leg")
		older(epochAge(b.in.readTime, leg.GetErrEpoch()))
	}
	var sideIds []string
	for _, side := range leg.GetSideList() {
		sideIds = append(sideIds, fmt.Sprintf("%d on dn %s",
			side.GetSideId(), side.GetAddrPort()))
		if side.GetErrEpoch() == 0 {
			continue
		}
		if kind == pb.StatusKind_STATUS_KIND_NONE {
			kind = pb.StatusKind_STATUS_KIND_HEALTH
		}
		parts = append(parts,
			fmt.Sprintf("health epoch set on side %d", side.GetSideId()))
		older(epochAge(b.in.readTime, side.GetErrEpoch()))
	}
	if blocks := info.GetGrpIdToGrpInfo()[grpId].
		GetLegIdToBadBlocks()[legId]; blocks != 0 {
		if kind == pb.StatusKind_STATUS_KIND_NONE {
			kind = pb.StatusKind_STATUS_KIND_GROUP
		}
		parts = append(parts, fmt.Sprintf(
			"md records %d bad-block ranges on its member", blocks))
	}
	if kind == pb.StatusKind_STATUS_KIND_NONE {
		return
	}
	b.legRowed[legId] = true
	out := &pb.StatusRow{
		Kind: kind,
		Object: fmt.Sprintf("leg %d (%s; side %s)", legId, where,
			strings.Join(sideIds, ", side ")),
		Text:    strings.Join(parts, "; "),
		Age:     age,
		SliceId: sliceId,
		GrpId:   grpId,
		LegId:   legId,
		ObjName: repName,
	}
	if haveRow {
		out.Status = statusOf(row.GetStatus())
		out.Reason = row.GetReason()
	}
	if sides := leg.GetSideList(); len(sides) > 0 {
		out.SideId = sides[0].GetSideId()
		out.AddrPort = sides[0].GetAddrPort()
	}
	b.add(rankLeg, out)
}

// legRowListed says whether a leg's report row is listed: it reads ERROR, or
// it carries an identity reason, a leg md keeps out.
func legRowListed(row *pb.ResInfo) bool {
	switch row.GetReason() {
	case pb.ResReason_RES_REASON_OTHER_IDENTITY,
		pb.ResReason_RES_REASON_IDENTITY_UNDECIDED:
		return true
	}
	return row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR
}

// legRowText is the text of a listed leg row.
func legRowText(row *pb.ResInfo) string {
	var text string
	switch row.GetReason() {
	case pb.ResReason_RES_REASON_OTHER_IDENTITY:
		text = "md keeps the leg out of the array: its superblock " +
			"carries another identity than the running array's"
	case pb.ResReason_RES_REASON_IDENTITY_UNDECIDED:
		text = "md assembles no array of the group: its legs carry two " +
			"identities and the agent cannot tell which is current"
	default:
		text = "the leg's row reads " + row.GetStatus().String()
	}
	if details := row.GetDetails(); details != "" {
		text += ": " + details
	}
	return text
}

// groupRows are the groups' rows: a group's row of the report that reads
// ERROR, its reading (an array not whole, unless every leg of its list md
// does not hold in sync has a row of its own, which says more; a refused
// add) and its marks in etcd.
func (b *statusBuilder) groupRows(info *pb.CntlrInfo) {
	b.eachGroup(func(grp *pb.Group, sliceId uint64, cloneName string,
		where string) {
		grpId := grp.GetGrpId()
		object := fmt.Sprintf("group %d (%s)", grpId, where)
		row, haveRow := info.GetGrpIdToMdRaid()[grpId]
		if haveRow && row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
			b.used[statusRowKey{"grp", grpId}] = true
			b.add(rankGroupRow, &pb.StatusRow{
				Kind:    pb.StatusKind_STATUS_KIND_ROW,
				Object:  object,
				Status:  statusOf(row.GetStatus()),
				Reason:  row.GetReason(),
				Text:    rowText(row),
				Age:     b.rowAge(row),
				SliceId: sliceId,
				GrpId:   grpId,
				ObjName: cloneName,
			})
		}
		reading, haveReading := info.GetGrpIdToGrpInfo()[grpId]
		if haveReading && haveRow &&
			row.GetStatus() == pb.ResStatus_RES_STATUS_OK {
			if !reading.GetWhole() && !b.lackingLegsRowed(grp, reading) {
				b.add(rankGroup, &pb.StatusRow{
					Kind:   pb.StatusKind_STATUS_KIND_GROUP,
					Object: object,
					Text: fmt.Sprintf("the array is not whole: md holds "+
						"legs %v in sync", reading.GetInSyncLegIdList()),
					SliceId: sliceId,
					GrpId:   grpId,
					ObjName: cloneName,
				})
			}
			if refusal := reading.GetRefusal(); refusal != "" {
				b.add(rankGroup, &pb.StatusRow{
					Kind:    pb.StatusKind_STATUS_KIND_GROUP,
					Object:  object,
					Text:    "the primary adds no leg: " + refusal,
					SliceId: sliceId,
					GrpId:   grpId,
					ObjName: cloneName,
				})
			}
		}
		b.markRows(grp, sliceId, cloneName, object)
	})
}

// etcdGroupRows are the groups' marks alone, for a read with no report.
func (b *statusBuilder) etcdGroupRows() {
	b.eachGroup(func(grp *pb.Group, sliceId uint64, cloneName string,
		where string) {
		b.markRows(grp, sliceId, cloneName,
			fmt.Sprintf("group %d (%s)", grp.GetGrpId(), where))
	})
}

// markRows are a group's marks in etcd: the operator in charge of its
// repair, and a forced start that stands.
func (b *statusBuilder) markRows(
	grp *pb.Group, sliceId uint64, cloneName string, object string,
) {
	if grp.GetOperatorInCharge() {
		b.add(rankGroup, &pb.StatusRow{
			Kind:    pb.StatusKind_STATUS_KIND_GROUP,
			Object:  object,
			Text:    "the operator is in charge of its repair: the worker repairs no leg of it",
			SliceId: sliceId,
			GrpId:   grp.GetGrpId(),
			ObjName: cloneName,
		})
	}
	if mark := grp.GetForcedStart(); mark != nil {
		b.add(rankGroup, &pb.StatusRow{
			Kind:   pb.StatusKind_STATUS_KIND_GROUP,
			Object: object,
			Text: fmt.Sprintf("a forced start from leg %d stands: the "+
				"worker clears it once the primary reports the array "+
				"running from it", mark.GetLegId()),
			SliceId: sliceId,
			GrpId:   grp.GetGrpId(),
			LegId:   mark.GetLegId(),
			ObjName: cloneName,
		})
	}
}

// eachGroup walks the pool's groups: the slices' in slice order, meta
// before data, then the clone meta groups of clones not being deleted.
func (b *statusBuilder) eachGroup(
	f func(grp *pb.Group, sliceId uint64, cloneName string, where string),
) {
	state := b.in.state
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range allGroups(slice) {
			f(grp, sliceId, "", fmt.Sprintf("slice %d", sliceId))
		}
	}
	for _, name := range state.Conf.GetCloneNameList() {
		clone, ok := state.Clones[name]
		if !ok || clone.GetDeleting() || clone.GetMetaGrp() == nil {
			continue
		}
		f(clone.GetMetaGrp(), 0, name, "clone "+name)
	}
}

// lackingLegsRowed says whether every leg of grp's leg_list that md does not
// hold in sync got a row of its own, which then says more than the group's
// "not whole" row; a group that lacks no leg of its list, whose array lacks
// a member all the same, says so in its own row.
func (b *statusBuilder) lackingLegsRowed(
	grp *pb.Group, reading *pb.CntlrInfo_GrpInfo,
) bool {
	inSync := map[uint64]bool{}
	for _, legId := range reading.GetInSyncLegIdList() {
		inSync[legId] = true
	}
	lacking := 0
	for _, leg := range grp.GetLegList() {
		if inSync[leg.GetLegId()] {
			continue
		}
		lacking++
		if !b.legRowed[leg.GetLegId()] {
			return false
		}
	}
	return lacking > 0
}

// spaceAndGrowRows are each slice's thin pool rows: a pool out of data or
// metadata space, whatever its grow, and a grow past the pool's mark that
// the worker cannot run. The grow's checks are AR6's, in AR6's order
// (dnv-worker.md AR6), from the rules model shares with the worker: a
// pending grow, a data grow the volumes do not need and a grow the disk
// nodes have room for are not listed; a full group list, the metadata
// ceiling, a slice with no data group, a controller node short of budget
// and no disk node with room are. An ERROR pool row is the error rows'.
func (b *statusBuilder) spaceAndGrowRows(info *pb.CntlrInfo) {
	state := b.in.state
	lwm := uint64(
		state.Conf.GetBdevConf().GetDmPoolConf().GetLowWaterMarkPct())
	blockSize := model.PoolBlockSize(state.Conf.GetBdevConf())
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		row, haveRow := info.GetSliceIdToDmPool()[sliceId]
		if !ok || !haveRow || row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			continue
		}
		pool, ok := common.ParseThinPoolStatus(row.GetDetails())
		if !ok {
			continue
		}
		object := fmt.Sprintf("slice %d thin pool", sliceId)
		switch {
		case pool.OutOfDataSpace:
			b.add(rankSpace, &pb.StatusRow{
				Kind:   pb.StatusKind_STATUS_KIND_SPACE,
				Object: object,
				Status: statusOf(row.GetStatus()),
				Text: "out of data space: a write that needs a new block " +
					"waits, then fails: " + row.GetDetails(),
				SliceId: sliceId,
			})
		case pool.ReadOnly && !pool.NeedsCheck:
			b.add(rankSpace, &pb.StatusRow{
				Kind:   pb.StatusKind_STATUS_KIND_SPACE,
				Object: object,
				Status: statusOf(row.GetStatus()),
				Text: "out of metadata space: the pool is read-only until " +
					"its metadata grows: " + row.GetDetails(),
				SliceId: sliceId,
			})
		}
		if lwm > 100 || b.badConf {
			continue
		}
		for _, kind := range []struct {
			isMeta      bool
			used, total uint64
		}{
			{false, pool.UsedDataBlocks, pool.TotalDataBlocks},
			{true, pool.UsedMetaBlocks, pool.TotalMetaBlocks},
		} {
			if kind.used*100 <= lwm*kind.total {
				continue
			}
			b.growRow(slice, sliceId, kind.isMeta, kind.total, blockSize)
		}
	}
}

// growRow lists one grow past the mark that the worker cannot run.
func (b *statusBuilder) growRow(
	slice *pb.Slice,
	sliceId uint64,
	isMeta bool,
	total uint64,
	blockSize uint64,
) {
	state := b.in.state
	what := "data"
	if isMeta {
		what = "metadata"
	}
	if model.GrowPending(slice, isMeta, total, blockSize) {
		return
	}
	if !isMeta && model.DataCovered(state, slice, blockSize) {
		return
	}
	refuse := func(reason, words string) {
		b.add(rankGrow, &pb.StatusRow{
			Kind:    pb.StatusKind_STATUS_KIND_GROW,
			Object:  fmt.Sprintf("slice %d thin pool", sliceId),
			Text:    fmt.Sprintf("%s grow refused (%s): %s", what, reason, words),
			SliceId: sliceId,
		})
	}
	extCnt, reason := model.GrowExtCnt(slice, isMeta,
		b.in.cc.GetDnBinConf().GetExtentSize())
	switch reason {
	case "":
	case model.ReasonGrpListFull:
		refuse(reason, "the slice holds the most groups of the kind")
		return
	case model.ReasonMetaLadderCap:
		refuse(reason, "its metadata is at dm-thin's ceiling")
		return
	default:
		refuse(reason, "the slice has no data group to size a grow by")
		return
	}
	order, charges := model.SpCnCharges(state.Conf, state.Cntlrs, extCnt)
	if addr, free, short := model.CnBudgetShort(
		order, charges, state.CnByAddr,
	); short {
		refuse(model.ReasonCnBudgetShort, fmt.Sprintf(
			"cn %s has %d free extents, and the grow charges %d",
			addr, free, charges[addr]))
		return
	}
	legs := legCntOf(state.Conf.GetBdevConf())
	room, err := b.in.dnRoom(extCnt, legs)
	if err != nil && b.in.ended != nil && b.in.ended() {
		refuse("op_failed", "not judged: the read's time ran out before "+
			"the scan of the disk nodes ended")
		return
	}
	if err != nil {
		refuse("op_failed", "the scan of the disk nodes failed: "+err.Error())
		return
	}
	if !room {
		refuse("no candidate", fmt.Sprintf(
			"no %d disk nodes in distinct locations have room for a group "+
				"of %d extents", legs, extCnt))
	}
}

// repairRows are the open repairs: every repair of the pool but a deleting
// clone's, with its stage and why the primary holds it back.
func (b *statusBuilder) repairRows(info *pb.CntlrInfo) {
	state := b.in.state
	for _, name := range state.Conf.GetRepairNameList() {
		rep, ok := state.Repairs[name]
		if !ok || repairOfDeletingClone(state, rep) {
			continue
		}
		verdict := info.GetRepIdToRepairInfo()[rep.GetRepId()]
		var text string
		worker := common.HasRepNameReservedPrefix(name)
		// The worker finishes and cancels its own repairs by a reaction
		// (dnv-worker.md AR8's drive step), which it runs neither at a
		// level that stops reactions (dnv-worker.md AR3) nor for a pool
		// whose stored conf its pass refuses (dnv-worker.md AR1): the
		// operator then drives them.
		workerDrives := worker && !b.badConf &&
			state.Conf.GetSpLevel() < pb.SpLevel_SP_LEVEL_NO_THINPOOL
		switch {
		case rep.GetRepStage() == pb.RepStage_REP_STAGE_ABORTED &&
			workerDrives:
			text = "aborted: the worker cancels it at its next pass"
		case rep.GetRepStage() == pb.RepStage_REP_STAGE_ABORTED:
			text = "aborted: it waits for the operator's repair cancel"
		case rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED &&
			worker && retiredLegsAllKept(state, rep):
			text = "started: its finish is refused, since the group " +
				"holds the most retired legs and every one carries keep; " +
				"clear a keep or delete a retired leg"
		case rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED &&
			workerDrives:
			text = "started: the worker finishes it at its next pass"
		case rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED:
			text = "started: its new legs hold their places in the array; " +
				"it waits for the operator's repair finish"
		case verdict.GetAbortConfirmed():
			text = "created: the primary confirms its abort, so the " +
				"worker moves it to aborted"
		case verdict.GetStarted():
			text = "created: the primary has added its new legs, so the " +
				"worker moves it to started"
		case rep.GetTryToAbort():
			text = "created, its abort asked and not confirmed yet"
		default:
			text = "created: the primary has not added its new legs yet"
		}
		if refusal := verdict.GetRefusal(); refusal != "" {
			text += "; the primary refuses the take-back: " + refusal
		}
		if info != nil {
			if waits := model.RepairWaitReason(info, rep); waits != "" {
				text += "; it waits: " + waits
			}
		}
		where := fmt.Sprintf("slice %d", rep.GetSliceId())
		if rep.GetCloneName() != "" {
			where = "clone " + rep.GetCloneName()
		}
		b.add(rankRepair, &pb.StatusRow{
			Kind:    pb.StatusKind_STATUS_KIND_REPAIR,
			Object:  fmt.Sprintf("repair %s (group %d, %s)", name, rep.GetGrpId(), where),
			Text:    text,
			SliceId: rep.GetSliceId(),
			GrpId:   rep.GetGrpId(),
			ObjName: name,
		})
	}
}

// retiredLegsAllKept says whether the finish of the worker's repair rep is
// refused at the retired-leg bound (model.ReasonRetiredLegsKept): the group
// holds MaxRetiredLegPerGrp retired legs and every one the state holds
// carries keep, so none can be given back for the bad leg it retires.
func retiredLegsAllKept(state *model.SpState, rep *pb.Repair) bool {
	grp := repairGroup(state, rep)
	if grp == nil ||
		len(grp.GetRetiredLegIdList()) < common.MaxRetiredLegPerGrp {
		return false
	}
	for _, legId := range grp.GetRetiredLegIdList() {
		retired, ok := state.RetiredLegs[legId]
		if ok && !retired.GetKeep() {
			return false
		}
	}
	return true
}

// repairGroup is the group a repair repairs, found in its parent, or nil.
func repairGroup(state *model.SpState, rep *pb.Repair) *pb.Group {
	if rep.GetCloneName() != "" {
		if grp := state.Clones[rep.GetCloneName()].GetMetaGrp(); grp != nil &&
			grp.GetGrpId() == rep.GetGrpId() {
			return grp
		}
		return nil
	}
	for _, grp := range allGroups(state.Slices[rep.GetSliceId()]) {
		if grp.GetGrpId() == rep.GetGrpId() {
			return grp
		}
	}
	return nil
}

// repairOfDeletingClone says whether rep repairs the meta group of a clone
// being deleted, or of one the state does not hold: the drain takes it
// away, as the worker's repair-waits record skips it (dnv-worker.md AR17).
func repairOfDeletingClone(state *model.SpState, rep *pb.Repair) bool {
	if rep.GetCloneName() == "" {
		return false
	}
	clone, ok := state.Clones[rep.GetCloneName()]
	return !ok || clone.GetDeleting()
}

// migrationRows are the migrations that wait: for their cutover, with the
// primary's path row to the new side, or past it for their finish, once the
// new side's copy is whole; and one whose copy reads ERROR, or whose new
// side gives no reading of its copy: no leg of the pool holds the side, its
// agent did not answer or answers a code, or it reports no copy. A copy that
// runs is not listed.
func (b *statusBuilder) migrationRows(info *pb.CntlrInfo) {
	state := b.in.state
	for _, name := range state.Conf.GetMigrNameList() {
		migr, ok := state.Migrs[name]
		if !ok {
			continue
		}
		legId, dst := stateSide(state, migr.GetDstSideId())
		row := &pb.StatusRow{
			Kind: pb.StatusKind_STATUS_KIND_MIGRATION,
			Object: fmt.Sprintf("migration %s (leg %d, side %d to side %d)",
				name, legId, migr.GetSrcSideId(), migr.GetDstSideId()),
			LegId:    legId,
			SideId:   migr.GetDstSideId(),
			ObjName:  name,
			AddrPort: dst.GetAddrPort(),
		}
		if migr.GetCutoverPending() {
			row.Text = "its cutover is pending"
			if path, ok := info.GetSideIdToPath()[migr.GetDstSideId()]; ok {
				b.used[statusRowKey{"path", migr.GetDstSideId()}] = true
				row.Status = statusOf(path.GetStatus())
				row.Reason = path.GetReason()
				row.Age = b.rowAge(path)
				row.Text += ": the primary's path to the new side reads " +
					rowText(path)
			}
			b.add(rankMigration, row)
			continue
		}
		got := b.in.dst[name]
		switch {
		case got == nil || got.side == nil:
			row.Status = unknownStatus()
			row.Text = "no leg of the pool holds its new side"
		case got.err != nil && got.cut:
			row.Status = unknownStatus()
			row.Text = "not read: the read's time ran out before the new " +
				"side's agent answered: " + got.err.Error()
		case got.err != nil:
			row.Status = unknownStatus()
			row.Text = "the new side's agent did not answer: " + got.err.Error()
		case got.reply.GetAgentReply().GetCode() != 0 &&
			got.reply.GetAgentReply().GetCode() != common.ReplyCodeLeftover:
			row.Status = unknownStatus()
			row.Text = fmt.Sprintf("the new side's agent answers code %d",
				got.reply.GetAgentReply().GetCode())
			if details := got.reply.GetAgentReply().GetDetails(); details != "" {
				row.Text += ": " + details
			}
		default:
			copyRow := got.reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo()
			switch {
			case copyRow == nil:
				row.Status = unknownStatus()
				row.Text = "the new side reports no copy"
			case copyRow.GetStatus() == pb.ResStatus_RES_STATUS_ERROR:
				row.Status = statusOf(copyRow.GetStatus())
				row.Reason = copyRow.GetReason()
				row.Text = "its copy reads " + rowText(copyRow)
			case common.HydrationComplete(copyRow.GetDetails()):
				row.Status = statusOf(copyRow.GetStatus())
				row.Text = "its copy is whole: it waits for migr finish"
			default:
				continue
			}
		}
		b.add(rankMigration, row)
	}
}

// cloneRows are the clones that wait: one being deleted, whose drain runs,
// and one the primary keeps missing because it does not read the source as
// ready (cnagent.md CN18).
func (b *statusBuilder) cloneRows(info *pb.CntlrInfo) {
	state := b.in.state
	for _, name := range state.Conf.GetCloneNameList() {
		clone, ok := state.Clones[name]
		if !ok {
			continue
		}
		cloneId := clone.GetCloneId()
		object := fmt.Sprintf("clone %s", name)
		if clone.GetDeleting() {
			b.add(rankClone, &pb.StatusRow{
				Kind:    pb.StatusKind_STATUS_KIND_CLONE,
				Object:  object,
				Text:    "being deleted: the worker's drain takes its records away",
				ObjName: name,
			})
			continue
		}
		for _, table := range []struct {
			name string
			rows map[uint64]*pb.ResInfo
		}{
			{"clone target", info.GetCloneIdToTarget()},
			{"clone dm-clone", info.GetCloneIdToDmClone()},
		} {
			row, ok := table.rows[cloneId]
			if !ok ||
				row.GetReason() != pb.ResReason_RES_REASON_SOURCE_FAULT {
				continue
			}
			b.used[statusRowKey{table.name, cloneId}] = true
			b.add(rankClone, &pb.StatusRow{
				Kind:    pb.StatusKind_STATUS_KIND_CLONE,
				Object:  object,
				Status:  statusOf(row.GetStatus()),
				Reason:  row.GetReason(),
				Text:    "it waits for its source: " + row.GetDetails(),
				Age:     b.rowAge(row),
				ObjName: name,
			})
			break
		}
	}
}

// tornRows are the torn thin devices (architecture.md, Thin devices).
func (b *statusBuilder) tornRows() {
	state := b.in.state
	for idx, td := range state.Tds {
		if !td.GetTorn() {
			continue
		}
		b.add(rankTorn, &pb.StatusRow{
			Kind:    pb.StatusKind_STATUS_KIND_TORN,
			Object:  "td " + state.TdNames[idx],
			Text:    "torn: its slices hold its data from different points in time; delete it and create it again",
			ObjName: state.TdNames[idx],
		})
	}
}

// errorRows are the rows of the primary's report that read ERROR, a leg's
// also when it carries an identity reason (legRowListed), and that no row
// above already shows: the slices' thin pools and their devices, the thin
// volumes, the namespaces and subsystems, the transfers, the clones' rows
// but a deleting clone's, the path rows, and each leg or group no row above
// walks, one the state does not hold or one of a clone being deleted.
func (b *statusBuilder) errorRows(info *pb.CntlrInfo) {
	state := b.in.state
	tdName := map[uint64]string{}
	for idx, td := range state.Tds {
		tdName[td.GetTdId()] = state.TdNames[idx]
	}
	ssName, nsName := map[uint64]string{}, map[uint64]string{}
	for nqn, ss := range state.Subsystems {
		ssName[ss.GetSsId()] = nqn
		for _, ns := range ss.GetNsList() {
			nsName[ns.GetNsId()] = fmt.Sprintf("namespace %d of %s",
				ns.GetNsIdx(), nqn)
		}
	}
	xferName, cloneName := map[uint64]string{}, map[uint64]string{}
	for name, xfer := range state.Xfers {
		xferName[xfer.GetXferId()] = name
	}
	for name, clone := range state.Clones {
		cloneName[clone.GetCloneId()] = name
	}
	named := func(names map[uint64]string, what string, id uint64) string {
		if name, ok := names[id]; ok {
			return what + " " + name
		}
		return fmt.Sprintf("%s id %d", what, id)
	}
	type table struct {
		key    string
		rank   int
		rows   map[uint64]*pb.ResInfo
		object func(id uint64) string
		fill   func(row *pb.StatusRow, id uint64)
		skip   func(id uint64) bool
	}
	// The rows of a clone being deleted are its teardown's, which its CLONE
	// row stands for.
	deletingClone := func(id uint64) bool {
		return state.Clones[cloneName[id]].GetDeleting()
	}
	slice := func(what string) func(id uint64) string {
		return func(id uint64) string {
			return fmt.Sprintf("slice %d %s", id, what)
		}
	}
	setSlice := func(row *pb.StatusRow, id uint64) { row.SliceId = id }
	setName := func(names map[uint64]string) func(*pb.StatusRow, uint64) {
		return func(row *pb.StatusRow, id uint64) { row.ObjName = names[id] }
	}
	tables := []table{
		{"pool", rankSliceRow, info.GetSliceIdToDmPool(), slice("thin pool"), setSlice, nil},
		{"meta", rankSliceRow, info.GetSliceIdToMeta(), slice("metadata device"), setSlice, nil},
		{"data", rankSliceRow, info.GetSliceIdToData(), slice("data device"), setSlice, nil},
		{"leg", rankLeg, info.GetLegIdToLeg(),
			func(id uint64) string { return fmt.Sprintf("leg %d", id) },
			func(row *pb.StatusRow, id uint64) { row.LegId = id }, nil},
		{"grp", rankGroupRow, info.GetGrpIdToMdRaid(),
			func(id uint64) string { return fmt.Sprintf("group %d", id) },
			func(row *pb.StatusRow, id uint64) { row.GrpId = id }, nil},
		{"ss", rankOtherRow, info.GetSsIdToSubsystem(),
			func(id uint64) string { return named(ssName, "subsystem", id) },
			setName(ssName), nil},
		{"ns", rankOtherRow, info.GetNsIdToNamespace(),
			func(id uint64) string { return nsLabel(nsName, id) }, nil, nil},
		{"ns dev", rankOtherRow, info.GetNsIdToDmLinear(),
			func(id uint64) string { return nsLabel(nsName, id) + " device" }, nil, nil},
		{"raid0", rankOtherRow, info.GetTdIdToRaid0(),
			func(id uint64) string { return named(tdName, "td", id) + " stripe" },
			setName(tdName), nil},
		{"td error", rankOtherRow, info.GetTdIdToDmError(),
			func(id uint64) string { return named(tdName, "td", id) + " error device" },
			setName(tdName), nil},
		{"xfer dev", rankOtherRow, info.GetXferIdToDmLinear(),
			func(id uint64) string { return named(xferName, "transfer", id) + " device" },
			setName(xferName), nil},
		{"xfer ss", rankOtherRow, info.GetXferIdToSubsystem(),
			func(id uint64) string { return named(xferName, "transfer", id) + " subsystem" },
			setName(xferName), nil},
		{"xfer ns", rankOtherRow, info.GetXferIdToNamespace(),
			func(id uint64) string { return named(xferName, "transfer", id) + " namespace" },
			setName(xferName), nil},
		{"clone target", rankOtherRow, info.GetCloneIdToTarget(),
			func(id uint64) string { return named(cloneName, "clone", id) + " target" },
			setName(cloneName), deletingClone},
		{"clone dm-clone", rankOtherRow, info.GetCloneIdToDmClone(),
			func(id uint64) string { return named(cloneName, "clone", id) + " copy" },
			setName(cloneName), deletingClone},
		{"path", rankOtherRow, info.GetSideIdToPath(),
			func(id uint64) string { return fmt.Sprintf("path to side %d", id) },
			func(row *pb.StatusRow, id uint64) { row.SideId = id }, nil},
	}
	for _, t := range tables {
		for _, id := range sortedIds(t.rows) {
			row := t.rows[id]
			listed := row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR
			if t.key == "leg" {
				listed = legRowListed(row)
			}
			if !listed || b.used[statusRowKey{t.key, id}] ||
				t.skip != nil && t.skip(id) {
				continue
			}
			out := &pb.StatusRow{
				Kind:   pb.StatusKind_STATUS_KIND_ROW,
				Object: t.object(id),
				Status: statusOf(row.GetStatus()),
				Reason: row.GetReason(),
				Text:   rowText(row),
				Age:    b.rowAge(row),
			}
			if t.fill != nil {
				t.fill(out, id)
			}
			b.add(t.rank, out)
		}
	}
	thins := info.GetTdIdToThinInfo()
	for _, tdId := range sortedIds(thins) {
		thin := thins[tdId]
		for _, sliceId := range sortedIds(thin.GetSliceIdToDmThin()) {
			row := thin.GetSliceIdToDmThin()[sliceId]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				continue
			}
			b.add(rankOtherRow, &pb.StatusRow{
				Kind: pb.StatusKind_STATUS_KIND_ROW,
				Object: fmt.Sprintf("%s, slice %d",
					named(tdName, "td", tdId), sliceId),
				Status:  statusOf(row.GetStatus()),
				Reason:  row.GetReason(),
				Text:    rowText(row),
				Age:     b.rowAge(row),
				SliceId: sliceId,
				ObjName: tdName[tdId],
			})
		}
	}
}

// nsLabel names a namespace row's object.
func nsLabel(names map[uint64]string, id uint64) string {
	if name, ok := names[id]; ok {
		return name
	}
	return fmt.Sprintf("namespace id %d", id)
}

// rowText is a listed row's text: its details, or its status when it has
// none.
func rowText(row *pb.ResInfo) string {
	if details := row.GetDetails(); details != "" {
		return details
	}
	return "the row reads " + row.GetStatus().String()
}

// sortedIds is the keys of a row map in ascending order.
func sortedIds[V any](rows map[uint64]V) []uint64 {
	ids := make([]uint64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// capStatusRows orders a pool's rows, by rank, then oldest first (a row
// with no age after those with one), then by object, and keeps the first
// MaxStatusRowPerSp, with one row more that counts the rest.
func capStatusRows(spName string, rows []statusRow) []*pb.StatusRow {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		ageA, ageB := a.row.GetAge(), b.row.GetAge()
		switch {
		case ageA != nil && ageB == nil:
			return true
		case ageA == nil && ageB != nil:
			return false
		case ageA != nil && ageA.AsDuration() != ageB.AsDuration():
			return ageA.AsDuration() > ageB.AsDuration()
		}
		return a.row.GetObject() < b.row.GetObject()
	})
	out := make([]*pb.StatusRow, 0, min(len(rows), common.MaxStatusRowPerSp+1))
	for idx, row := range rows {
		if idx == common.MaxStatusRowPerSp {
			out = append(out, &pb.StatusRow{
				SpName: spName,
				Kind:   pb.StatusKind_STATUS_KIND_MORE,
				Object: "pool",
				Text: fmt.Sprintf("%d more rows not listed: read them with "+
					"sp get, cntlr inspect and group inspect",
					len(rows)-idx),
			})
			break
		}
		out = append(out, row.row)
	}
	return out
}

// ---------------------------------------------------------------------------
// Small shapes
// ---------------------------------------------------------------------------

// cntlrObject names a cntlr: its id, its role and its controller node.
func cntlrObject(id uint64, cntlr *pb.Cntlr) string {
	role := "standby"
	if cntlr.GetPrimary() {
		role = "primary"
	}
	return fmt.Sprintf("cntlr %d (%s, cn %s)", id, role, cntlr.GetAddrPort())
}

// epochAge is the age of a unix-seconds epoch at now; never negative.
func epochAge(now time.Time, epoch uint64) *durationpb.Duration {
	age := now.Sub(time.Unix(int64(epoch), 0))
	if age < 0 {
		age = 0
	}
	return durationpb.New(age)
}

// statusOf is a report status for a row's optional status field.
func statusOf(status pb.ResStatus) *pb.ResStatus {
	return &status
}

// unknownStatus is RES_STATUS_UNKNOWN, the status of a row the read has no
// answer for.
func unknownStatus() *pb.ResStatus {
	return statusOf(pb.ResStatus_RES_STATUS_UNKNOWN)
}
