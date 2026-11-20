package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the handler tests of GetClusterStatus (architecture.md,
// Clusters; gateway.md GW20). Every state is planted by hand with mustPut:
// a gateway test may not import integtest/plant (layout.md, Dependency
// rules). Each test owns a cluster of its own, and the ones that read the
// cluster rows own the worker registry too (hwkClearRegistry), which no test
// runs beside, since none of the package's tests runs in parallel.

// stEnvSeq numbers the clusters the status tests plant.
var stEnvSeq atomic.Uint64

// stEnv is one status test's cluster: its conf as CreateCluster stores it,
// and the nodes and pools the test plants.
type stEnv struct {
	t      *testing.T
	ctx    context.Context
	cli    *etcdutil.Client
	srv    *Server
	name   string
	cid    uint64
	cc     *pb.ClusterConf
	nodeId uint64
	spId   uint64
}

// newStEnv writes the cluster conf of a fresh cluster.
func newStEnv(t *testing.T) *stEnv {
	t.Helper()
	cli := newTestClient(t)
	seq := stEnvSeq.Add(1)
	name := fmt.Sprintf("st-%d", seq)
	env := &stEnv{
		t:    t,
		ctx:  context.Background(),
		cli:  cli,
		srv:  NewServer(cli),
		name: name,
		cid:  model.ClusterId(name, seq),
		cc: testStoredClusterConf(
			seq, common.DefaultDnExtSize, common.DefaultDmPoolDataBlockSize),
	}
	mustPut(t, cli, model.ClusterConfKey(name), env.cc)
	return env
}

// putCn records one controller node at addr with its revision key.
func (e *stEnv) putCn(addr string, free uint64) *pb.CnConf {
	e.t.Helper()
	e.nodeId++
	cn := &pb.CnConf{
		CnId: e.nodeId, NvmeTrConf: testTrConf(addr), Location: addr,
		TotalExtCnt: 1 << 20, FreeExtCnt: free,
	}
	mustPut(e.t, e.cli, model.CnConfKey(e.cid, addr), cn)
	mustPut(e.t, e.cli, model.CnRevKey(0, e.cid, cn.GetCnId()),
		&pb.CnRev{AddrPort: addr, Revision: 1})
	return cn
}

// addCn starts a fake cn agent whose report holds no row, and records its
// node.
func (e *stEnv) addCn(label string) (string, *fakeAgent) {
	e.t.Helper()
	addr := fakeAddrPort(e.t, label)
	fake := startFakeAgent(e.t, addr, 0)
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{})
	e.putCn(addr, 1<<20)
	return addr, fake
}

// addDn records one disk node with free extents, its revision key and the
// capacity key the free count implies (architecture.md, Capacity index
// keys), so the candidate scan of a grow finds it.
func (e *stEnv) addDn(addr string, free uint64) *pb.DnConf {
	e.t.Helper()
	e.nodeId++
	dn := &pb.DnConf{
		DnId: e.nodeId, NvmeTrConf: testTrConf(addr), Location: addr,
		TotalExtCnt: 1 << 20, FreeExtCnt: free,
	}
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addr), dn)
	mustPut(e.t, e.cli, model.DnRevKey(0, e.cid, dn.GetDnId()),
		&pb.DnRev{AddrPort: addr, Revision: 1})
	if binIdx, ok := model.DnBinIdx(free, e.cc.GetDnBinConf()); ok {
		mustPut(e.t, e.cli,
			model.DnCapacityKey(e.cid, binIdx, free, addr),
			model.DnCapacityValue(dn))
	}
	return dn
}

// stPool is one planted pool: the records LoadSp reads, as a test shapes
// them before put writes them.
type stPool struct {
	name    string
	conf    *pb.SpConf
	cntlrs  map[uint64]*pb.Cntlr
	slices  map[uint64]*pb.Slice
	tds     map[string]*pb.ThinDevice
	repairs map[string]*pb.Repair
	retired map[uint64]*pb.RetiredLeg
	migrs   map[string]*pb.Migration
	clones  map[string]*pb.Clone
	noRev   bool
}

// The ids of the pool newPool plants: cntlr 1, slice 2, its meta group 3
// with legs 4 and 6 (sides 5 and 7), its data group 8 with legs 9 and 11
// (sides 10 and 12).
const (
	stCntlr   = 1
	stSlice   = 2
	stMetaGrp = 3
	stDataGrp = 8
	stLeg     = 9
	stSide    = 10
	stLeg2    = 11
)

// stLegOf is one leg of one side.
func stLegOf(legId uint64, idx uint32, addr string) *pb.Leg {
	return &pb.Leg{LegId: legId, LegIdx: idx, SideList: []*pb.Side{{
		SideId: legId + 1, AddrPort: addr, Provisioned: true,
	}}}
}

// newPool shapes an md-raid1 pool of one slice on dns, its one cntlr the
// primary on cn.
func (e *stEnv) newPool(name string, cn string, dns []string) *stPool {
	e.spId++
	bdev := proto.Clone(e.cc.GetBdevConf()).(*pb.BdevConf)
	bdev.RedundConf = &pb.RedundConf{RedunKind: &pb.RedundConf_RedundMdRaid1{
		RedundMdRaid1: &pb.RedundMdRaid1{BitmapChunkBlockCnt: 128},
	}}
	return &stPool{
		name: name,
		conf: &pb.SpConf{
			SpId: e.spId, NextId: 100, BdevConf: bdev,
			CntlrIdList: []uint64{stCntlr}, SliceIdList: []uint64{stSlice},
		},
		cntlrs: map[uint64]*pb.Cntlr{stCntlr: {
			AddrPort: cn, NvmeTrConf: testTrConf(cn), Primary: true,
		}},
		slices: map[uint64]*pb.Slice{stSlice: {
			MetaGrpList: []*pb.Group{{
				GrpId: stMetaGrp, ExtCnt: 1, MetaBlocks: 3, DataBlocks: 509,
				ArrayMade: true,
				LegList: []*pb.Leg{
					stLegOf(4, 0, dns[0]), stLegOf(6, 1, dns[1]),
				},
			}},
			DataGrpList: []*pb.Group{{
				GrpId: stDataGrp, ExtCnt: 4, MetaBlocks: 3, DataBlocks: 2045,
				ArrayMade: true,
				LegList: []*pb.Leg{
					stLegOf(stLeg, 0, dns[2]), stLegOf(stLeg2, 1, dns[3]),
				},
			}},
		}},
		tds:     map[string]*pb.ThinDevice{},
		repairs: map[string]*pb.Repair{},
		retired: map[uint64]*pb.RetiredLeg{},
		migrs:   map[string]*pb.Migration{},
		clones:  map[string]*pb.Clone{},
	}
}

// put writes the pool's records, its lists made from them; a name a test
// adds to a list after put names a record that is not there.
func (e *stEnv) put(p *stPool) {
	e.t.Helper()
	conf, cid, spId := p.conf, e.cid, p.conf.GetSpId()
	conf.TdNameList, conf.MigrNameList, conf.CloneNameList = nil, nil, nil
	conf.RepairNameList, conf.RetiredLegIdList = nil, nil
	for name, td := range p.tds {
		conf.TdNameList = append(conf.TdNameList, name)
		mustPut(e.t, e.cli, model.ThinDeviceKey(cid, spId, name), td)
	}
	for name, migr := range p.migrs {
		conf.MigrNameList = append(conf.MigrNameList, name)
		mustPut(e.t, e.cli, model.MigrationKey(cid, spId, name), migr)
	}
	for name, clone := range p.clones {
		conf.CloneNameList = append(conf.CloneNameList, name)
		mustPut(e.t, e.cli, model.CloneKey(cid, spId, name), clone)
	}
	for name, rep := range p.repairs {
		conf.RepairNameList = append(conf.RepairNameList, name)
		mustPut(e.t, e.cli, model.RepairKey(cid, spId, name), rep)
	}
	for legId, retired := range p.retired {
		conf.RetiredLegIdList = append(conf.RetiredLegIdList, legId)
		mustPut(e.t, e.cli, model.RetiredLegKey(cid, spId, legId), retired)
	}
	for id, cntlr := range p.cntlrs {
		mustPut(e.t, e.cli, model.CntlrKey(cid, spId, id), cntlr)
	}
	for id, slice := range p.slices {
		mustPut(e.t, e.cli, model.SliceKey(cid, spId, id), slice)
	}
	mustPut(e.t, e.cli, model.SpConfKey(cid, p.name), conf)
	mustPut(e.t, e.cli, model.SpNameKey(cid, spId),
		&pb.SpName{SpName: p.name})
	if !p.noRev {
		mustPut(e.t, e.cli, model.SpRevKey(0, cid, spId),
			&pb.SpRev{SpName: p.name, Revision: 1})
	}
}

// status reads one page, failing the test on an error.
func (e *stEnv) status(count uint32, token string) *pb.GetClusterStatusReply {
	e.t.Helper()
	reply, err := e.srv.GetClusterStatus(e.ctx, &pb.GetClusterStatusRequest{
		ClusterName: e.name, Count: count, PageToken: token,
	})
	if err != nil {
		e.t.Fatalf("GetClusterStatus: %v", err)
	}
	return reply
}

// stRowsOf is the rows of one pool, or the cluster rows for "".
func stRowsOf(reply *pb.GetClusterStatusReply, sp string) []*pb.StatusRow {
	var out []*pb.StatusRow
	for _, row := range reply.GetRowList() {
		if row.GetSpName() == sp {
			out = append(out, row)
		}
	}
	return out
}

// stKinds is the rows of one kind among rows.
func stKinds(rows []*pb.StatusRow, kind pb.StatusKind) []*pb.StatusRow {
	var out []*pb.StatusRow
	for _, row := range rows {
		if row.GetKind() == kind {
			out = append(out, row)
		}
	}
	return out
}

// stOne is the one row of a kind among rows, failing the test unless there
// is exactly one, and that row's text holds every fragment.
func stOne(
	t *testing.T, rows []*pb.StatusRow, kind pb.StatusKind,
	label string, fragments ...string,
) *pb.StatusRow {
	t.Helper()
	got := stKinds(rows, kind)
	if len(got) != 1 {
		t.Fatalf("%s: %d rows of kind %v, want 1: %v",
			label, len(got), kind, rows)
	}
	for _, fragment := range fragments {
		if !strings.Contains(got[0].GetText(), fragment) {
			t.Errorf("%s: text %q holds no %q",
				label, got[0].GetText(), fragment)
		}
	}
	return got[0]
}

// stWantNone fails the test when rows holds any row.
func stWantNone(t *testing.T, rows []*pb.StatusRow, label string) {
	t.Helper()
	if len(rows) != 0 {
		t.Errorf("%s: %d rows, want none: %v", label, len(rows), rows)
	}
}

// stWantStatus checks a row's report status.
func stWantStatus(t *testing.T, row *pb.StatusRow, want pb.ResStatus) {
	t.Helper()
	if row.Status == nil || row.GetStatus() != want {
		t.Errorf("row %q: status %v (set %v), want %v",
			row.GetObject(), row.GetStatus(), row.Status != nil, want)
	}
}

// stWantAge checks a row's age against want: an epoch is whole seconds, so
// a planted one reads up to a second older, and the read itself takes time.
func stWantAge(t *testing.T, row *pb.StatusRow, want time.Duration) {
	t.Helper()
	if row.GetAge() == nil {
		t.Errorf("row %q: no age, want %v", row.GetObject(), want)
		return
	}
	got := row.GetAge().AsDuration()
	if got < want-time.Second || got > want+2*time.Second {
		t.Errorf("row %q: age %v, want %v", row.GetObject(), got, want)
	}
}

// stFreshWorkers leaves one fresh registration of each role, so a read
// lists no worker row.
func stFreshWorkers(t *testing.T, s *Server) {
	t.Helper()
	hwkClearRegistry(t, s)
	now := uint64(time.Now().Unix())
	for _, role := range []string{
		common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp,
	} {
		mustPut(t, s.cli, model.WorkerRegKey(role, "fresh"),
			&pb.WorkerReg{Epoch: now})
	}
}

// stDns is four disk node addresses of one test.
func stDns(e *stEnv) []string {
	var dns []string
	for idx := 0; idx < 4; idx++ {
		addr := fmt.Sprintf("dn-%s-%d:9000", e.name, idx)
		e.addDn(addr, 64)
		dns = append(dns, addr)
	}
	return dns
}

// stOkRow is a row that reads OK.
func stOkRow() *pb.ResInfo {
	return &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_OK}
}

// stAnswer is the answer time of fakeAgentStamp.
func stAnswer() time.Time {
	return fakeAgentStamp().GetAnswerTime().AsTime()
}

// TestStatusHealthyPoolListsNothing pins the read of a healthy cluster: no
// row at all, the pool counted, the page the last one.
func TestStatusHealthyPoolListsNothing(t *testing.T) {
	env := newStEnv(t)
	stFreshWorkers(t, env.srv)
	cn, fake := env.addCn("cn")
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "0 1024 thin-pool 3 12/4096 5/62 - rw discard_passdown",
		}},
		LegIdToLeg: map[uint64]*pb.ResInfo{
			4: stOkRow(), 6: stOkRow(), stLeg: stOkRow(), stLeg2: stOkRow(),
		},
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{
			stMetaGrp: stOkRow(), stDataGrp: stOkRow(),
		},
		GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
			stMetaGrp: {Whole: true}, stDataGrp: {Whole: true},
		},
	})
	env.put(env.newPool("p0", cn, stDns(env)))
	reply := env.status(0, "")
	stWantNone(t, reply.GetRowList(), "a healthy cluster")
	if reply.GetSpCnt() != 1 || reply.GetPageToken() != "" ||
		reply.GetClusterName() != env.name || reply.GetReadTime() == nil {
		t.Errorf("reply %v: want one pool read, no token, the cluster's "+
			"name and the read time", reply)
	}
	if got := fake.callCount("GetCntlrInfo"); got != 1 {
		t.Errorf("GetCntlrInfo called %d times, want 1", got)
	}
	req := fake.cntlrInfoReqList()
	if len(req) == 1 && (req[0].GetCntlrPointer().GetCntlrId() != stCntlr ||
		req[0].GetReadIoStats()) {
		t.Errorf("GetCntlrInfo request %v: want cntlr %d, no counters",
			req[0], stCntlr)
	}
}

// TestStatusErrorRow pins an ERROR row of the primary's report: its status,
// reason and details, its age by the node's clock and the slice it names.
func TestStatusErrorRow(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	epoch := stAnswer().Add(-90 * time.Second)
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
			Status:  pb.ResStatus_RES_STATUS_ERROR,
			Reason:  pb.ResReason_RES_REASON_POOL_NEEDS_CHECK,
			Details: "0 1024 thin-pool 3 12/4096 5/62 - ro needs_check",
			Epoch:   uint64(epoch.Unix()),
		}},
	})
	env.put(env.newPool("p0", cn, stDns(env)))
	rows := stRowsOf(env.status(0, ""), "p0")
	row := stOne(t, rows, pb.StatusKind_STATUS_KIND_ROW, "needs_check",
		"needs_check")
	stWantStatus(t, row, pb.ResStatus_RES_STATUS_ERROR)
	if row.GetReason() != pb.ResReason_RES_REASON_POOL_NEEDS_CHECK ||
		row.GetSliceId() != stSlice {
		t.Errorf("row %v: want reason needs_check and slice %d", row, stSlice)
	}
	stWantAge(t, row, 90*time.Second)
}

// TestStatusHealthEpochs pins the health epochs: a cntlr's, a leg's merged
// with its side's in one row, a retired leg's side's, and on the cluster
// rows a disk node's and a controller node's, each aged by the gateway's
// clock.
func TestStatusHealthEpochs(t *testing.T) {
	env := newStEnv(t)
	stFreshWorkers(t, env.srv)
	cn, _ := env.addCn("cn")
	dns := stDns(env)
	now := time.Now()
	ago := func(d time.Duration) uint64 { return uint64(now.Add(-d).Unix()) }
	pool := env.newPool("p0", cn, dns)
	pool.cntlrs[stCntlr].ErrEpoch = ago(30 * time.Second)
	leg := pool.slices[stSlice].DataGrpList[0].LegList[0]
	leg.ErrEpoch = ago(40 * time.Second)
	leg.SideList[0].ErrEpoch = ago(50 * time.Second)
	pool.retired[30] = &pb.RetiredLeg{
		GrpId: stDataGrp, SliceId: stSlice,
		Side: &pb.Side{SideId: 31, AddrPort: dns[0], ErrEpoch: ago(60 *
			time.Second)},
	}
	pool.slices[stSlice].DataGrpList[0].RetiredLegIdList = []uint64{30}
	env.put(pool)
	dn := env.addDn("dn-sick:9000", 64)
	dn.ErrEpoch = ago(70 * time.Second)
	mustPut(t, env.cli, model.DnConfKey(env.cid, "dn-sick:9000"), dn)
	cnRec := env.putCn("cn-sick:9000", 64)
	cnRec.ErrEpoch = ago(80 * time.Second)
	mustPut(t, env.cli, model.CnConfKey(env.cid, "cn-sick:9000"), cnRec)

	reply := env.status(0, "")
	rows := stKinds(stRowsOf(reply, "p0"), pb.StatusKind_STATUS_KIND_HEALTH)
	if len(rows) != 3 {
		t.Fatalf("%d health rows of the pool, want 3: %v", len(rows), rows)
	}
	byId := map[string]*pb.StatusRow{}
	for _, row := range rows {
		switch {
		case row.GetCntlrId() == stCntlr:
			byId["cntlr"] = row
		case row.GetLegId() == stLeg:
			byId["leg"] = row
		case row.GetLegId() == 30:
			byId["retired"] = row
		}
	}
	for name, want := range map[string]time.Duration{
		"cntlr": 30 * time.Second, "leg": 50 * time.Second,
		"retired": 60 * time.Second,
	} {
		if byId[name] == nil {
			t.Errorf("no health row of the %s: %v", name, rows)
			continue
		}
		stWantAge(t, byId[name], want)
	}
	if leg := byId["leg"]; leg != nil &&
		(leg.GetSideId() != stSide || leg.GetAddrPort() != dns[2]) {
		t.Errorf("the leg's row %v: want its side %d on %s",
			leg, stSide, dns[2])
	}
	nodes := stKinds(stRowsOf(reply, ""), pb.StatusKind_STATUS_KIND_HEALTH)
	if len(nodes) != 2 {
		t.Fatalf("%d health rows of the nodes, want 2: %v", len(nodes), nodes)
	}
	for _, row := range nodes {
		switch row.GetAddrPort() {
		case "dn-sick:9000":
			stWantAge(t, row, 70*time.Second)
		case "cn-sick:9000":
			stWantAge(t, row, 80*time.Second)
		default:
			t.Errorf("a health row of %q", row.GetAddrPort())
		}
	}
}

// TestStatusPrimaryUnreachable pins an agent that does not answer: one
// UNKNOWN row and the next pool still read; and two pools whose primaries
// share a node that hangs cost one call and one budget, not two.
func TestStatusPrimaryUnreachable(t *testing.T) {
	saved := statusAgentBudget
	statusAgentBudget = 300 * time.Millisecond
	t.Cleanup(func() { statusAgentBudget = saved })

	env := newStEnv(t)
	dns := stDns(env)
	gone := fakeAddrPort(t, "gone")
	env.putCn(gone, 1<<20)
	env.put(env.newPool("p0", gone, dns))
	cn, _ := env.addCn("cn")
	env.put(env.newPool("p1", cn, dns))
	reply := env.status(0, "")
	row := stOne(t, stRowsOf(reply, "p0"), pb.StatusKind_STATUS_KIND_REPORT,
		"no listener", "did not answer")
	stWantStatus(t, row, pb.ResStatus_RES_STATUS_UNKNOWN)
	if row.GetCntlrId() != stCntlr || row.GetAddrPort() != gone {
		t.Errorf("row %v: want cntlr %d at %s", row, stCntlr, gone)
	}
	stWantNone(t, stRowsOf(reply, "p1"), "the pool behind it")
	if reply.GetSpCnt() != 2 {
		t.Errorf("sp_cnt %d, want 2", reply.GetSpCnt())
	}

	env2 := newStEnv(t)
	dns2 := stDns(env2)
	hang, fake := env2.addCn("hang")
	release := fake.setBlock()
	defer release()
	env2.put(env2.newPool("q0", hang, dns2))
	env2.put(env2.newPool("q1", hang, dns2))
	start := time.Now()
	reply = env2.status(0, "")
	took := time.Since(start)
	for _, sp := range []string{"q0", "q1"} {
		stOne(t, stRowsOf(reply, sp), pb.StatusKind_STATUS_KIND_REPORT, sp)
	}
	if got := fake.callCount("GetCntlrInfo"); got != 1 {
		t.Errorf("GetCntlrInfo called %d times on the hanging node, want 1",
			got)
	}
	if took > statusAgentBudget+time.Second {
		t.Errorf("the read took %v, want about one budget (%v)",
			took, statusAgentBudget)
	}
}

// TestStatusReportRows pins the answers that give the read no state to
// judge by: an observation older than the worker accepts, an agent that
// holds no observation of the cntlr yet, one that holds no state of it, one
// that is starting, and a pool with no primary; and that the rows of an old
// observation are still listed.
func TestStatusReportRows(t *testing.T) {
	interval := time.Duration(
		common.DefaultHealthCheckInterval*common.ObsMaxAgeIntervals) *
		time.Second
	for _, tc := range []struct {
		label    string
		shape    func(fake *fakeAgent, pool *stPool)
		fragment string
	}{
		{"an old observation", func(fake *fakeAgent, _ *stPool) {
			stamp := fakeAgentStamp()
			stamp.ObsTime = timestamppb.New(
				stamp.GetAnswerTime().AsTime().Add(-interval - time.Minute))
			fake.setStamp(stamp)
			fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
				SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
					Status: pb.ResStatus_RES_STATUS_ERROR, Details: "boom",
				}},
			})
		}, "does not end"},
		{"no observation yet", func(fake *fakeAgent, _ *stPool) {
			fake.setStamp(fakeAgentNoObsStamp())
		}, "no observation"},
		{"an unknown cntlr", func(fake *fakeAgent, _ *stPool) {
			fake.setCntlrReply(&pb.AgentReply{
				Code: common.ReplyCodeUnknownObject})
		}, "holds no state"},
		{"a starting agent", func(fake *fakeAgent, _ *stPool) {
			fake.setCntlrReply(&pb.AgentReply{Code: common.ReplyCodeStarting})
		}, "starting"},
		{"no primary", func(_ *fakeAgent, pool *stPool) {
			pool.cntlrs[stCntlr].Primary = false
		}, "no cntlr of the pool is primary"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			pool := env.newPool("p0", cn, stDns(env))
			tc.shape(fake, pool)
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			row := stOne(t, rows, pb.StatusKind_STATUS_KIND_REPORT, tc.label,
				tc.fragment)
			stWantStatus(t, row, pb.ResStatus_RES_STATUS_UNKNOWN)
			if tc.label == "an old observation" {
				if !strings.Contains(row.GetText(), "health line") {
					t.Errorf("text %q points to no health line",
						row.GetText())
				}
				stWantAge(t, row, interval+time.Minute)
				stOne(t, rows, pb.StatusKind_STATUS_KIND_ROW,
					"the old observation's row", "boom")
			}
		})
	}
}

// TestStatusLostRecord pins the lost records: a key a pool's list names
// and etcd does not hold, a pool whose revision key is absent, and on the
// cluster rows a node whose revision key is absent; the pool's other rows
// are still read.
func TestStatusLostRecord(t *testing.T) {
	env := newStEnv(t)
	stFreshWorkers(t, env.srv)
	cn, _ := env.addCn("cn")
	dns := stDns(env)
	pool := env.newPool("p0", cn, dns)
	pool.cntlrs[stCntlr].ErrEpoch = uint64(time.Now().Unix())
	env.put(pool)
	conf := proto.Clone(pool.conf).(*pb.SpConf)
	conf.TdNameList = append(conf.TdNameList, "vol-gone")
	mustPut(t, env.cli, model.SpConfKey(env.cid, "p0"), conf)
	norev := env.newPool("p1", cn, dns)
	norev.noRev = true
	env.put(norev)
	dn := &pb.DnConf{}
	if _, err := env.cli.Get(env.ctx, model.DnConfKey(env.cid, dns[0]),
		dn); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := env.cli.Delete(env.ctx,
		model.DnRevKey(0, env.cid, dn.GetDnId())); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reply := env.status(0, "")
	rows := stRowsOf(reply, "p0")
	row := stOne(t, rows, pb.StatusKind_STATUS_KIND_LOST_RECORD, "a listed td")
	if want := model.ThinDeviceKey(env.cid, pool.conf.GetSpId(),
		"vol-gone"); row.GetObject() != want {
		t.Errorf("object %q, want the key %q", row.GetObject(), want)
	}
	stOne(t, rows, pb.StatusKind_STATUS_KIND_HEALTH, "the pool's other rows")
	row = stOne(t, stRowsOf(reply, "p1"),
		pb.StatusKind_STATUS_KIND_LOST_RECORD, "a pool's revision key",
		"revision key")
	if want := model.SpRevKey(0, env.cid,
		norev.conf.GetSpId()); row.GetObject() != want {
		t.Errorf("object %q, want the key %q", row.GetObject(), want)
	}
	row = stOne(t, stRowsOf(reply, ""),
		pb.StatusKind_STATUS_KIND_LOST_RECORD, "a node's revision key",
		"revision key")
	if row.GetAddrPort() != dns[0] {
		t.Errorf("row %v: want the node %s", row, dns[0])
	}
}

// TestStatusRepairs pins the row of each open repair: a CREATED one with
// the verdict's waits, one whose take-back is refused, one whose abort is
// asked and not confirmed, and, each of an operator and of the worker, an
// ABORTED one and a STARTED one, the text naming who acts: the operator's
// cancel or finish, the worker's next pass, or the worker's finish refused
// at the retired-leg bound with every retired leg kept; the worker's own in
// a pool at a level that stops reactions, under a conf its pass refuses or
// with a record a list of the pool names missing, which the operator
// drives; a STARTED or ABORTED one, the worker's or the operator's, in a
// pool with no revision key, which waits for the key, since neither the
// worker's pass nor the gateway's cancel or finish acts without it, even
// where the retired-leg bound refuses the finish; and a CREATED one whose
// verdict says started in a pool whose pass is refused, which the worker
// moves only once it drives the pool again.
func TestStatusRepairs(t *testing.T) {
	worker := common.WorkerRepName(1, stDataGrp, 7)
	for _, tc := range []struct {
		label     string
		name      string
		rep       *pb.Repair
		verdict   *pb.CntlrInfo_RepairInfo
		keepAll   bool
		fragments []string
		// level and refused put the pool where the worker runs no
		// reaction, so the operator drives even the worker's repairs,
		// save under "norev", where nothing can: refused is "conf" for a
		// conf the pass refuses, "lost" for a thin device the pool's list
		// names and etcd does not hold, and "norev" for an absent
		// revision key.
		level   pb.SpLevel
		refused string
	}{
		{"waits", "r1", &pb.Repair{RepId: 50},
			&pb.CntlrInfo_RepairInfo{Waits: "no member to feed a copy"},
			false, []string{"created", "no member to feed a copy"}, 0, ""},
		{"refused", "r1", &pb.Repair{RepId: 50},
			&pb.CntlrInfo_RepairInfo{Refusal: "another identity"},
			false, []string{"created", "another identity"}, 0, ""},
		{"abort asked", "r1", &pb.Repair{RepId: 50, TryToAbort: true}, nil,
			false, []string{"abort", "not confirmed"}, 0, ""},
		{"aborted", "r1",
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_ABORTED},
			&pb.CntlrInfo_RepairInfo{AbortConfirmed: true},
			false, []string{"aborted", "operator's repair cancel"}, 0, ""},
		{"started", "r1",
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"started", "operator's repair finish"}, 0, ""},
		{"the worker's aborted", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_ABORTED},
			&pb.CntlrInfo_RepairInfo{AbortConfirmed: true},
			false, []string{"aborted", "the worker cancels it"}, 0, ""},
		{"the worker's started", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"started", "the worker finishes it"}, 0, ""},
		{"the worker's finish refused", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			true, []string{"finish is refused", "keep"}, 0, ""},
		{"the worker's aborted where reactions stop", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_ABORTED},
			&pb.CntlrInfo_RepairInfo{AbortConfirmed: true},
			false, []string{"operator's repair cancel"},
			pb.SpLevel_SP_LEVEL_NO_THINPOOL, ""},
		{"the worker's started under a refused conf", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"operator's repair finish"}, 0, "conf"},
		{"the worker's started with a lost record", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"operator's repair finish"}, 0, "lost"},
		{"the worker's aborted with no revision key", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_ABORTED},
			&pb.CntlrInfo_RepairInfo{AbortConfirmed: true},
			false, []string{"aborted", "revision key is back",
				"neither the worker's pass nor a repair cancel or finish"},
			0, "norev"},
		{"an operator's started with no revision key", "r1",
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"started", "revision key is back"}, 0, "norev"},
		{"an operator's aborted with no revision key", "r1",
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_ABORTED},
			&pb.CntlrInfo_RepairInfo{AbortConfirmed: true},
			false, []string{"aborted", "revision key is back"}, 0, "norev"},
		{"the worker's started with no revision key", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"started", "revision key is back"}, 0, "norev"},
		// The bound's own steps, a keep's clear and a retired leg's
		// delete, are pool writes the gateway refuses without the key
		// too, so the key's wait comes first.
		{"the worker's finish refused with no revision key", worker,
			&pb.Repair{RepId: 50, RepStage: pb.RepStage_REP_STAGE_STARTED},
			&pb.CntlrInfo_RepairInfo{Started: true},
			true, []string{"started", "revision key is back"}, 0, "norev"},
		{"a created one with a lost record", "r1", &pb.Repair{RepId: 50},
			&pb.CntlrInfo_RepairInfo{Started: true},
			false, []string{"moves it to started", "drives the pool again"},
			0, "lost"},
		{"a created one's abort under a refused conf", "r1",
			&pb.Repair{RepId: 50, TryToAbort: true},
			&pb.CntlrInfo_RepairInfo{AbortConfirmed: true},
			false, []string{"moves it to aborted", "drives the pool again"},
			0, "conf"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			dns := stDns(env)
			pool := env.newPool("p0", cn, dns)
			env.addDn("dn-new:9000", 64)
			tc.rep.GrpId, tc.rep.SliceId = stDataGrp, stSlice
			tc.rep.BadLegIdList = []uint64{stLeg}
			tc.rep.NewLegList = []*pb.Leg{stLegOf(60, 2, "dn-new:9000")}
			pool.repairs[tc.name] = tc.rep
			pool.conf.SpLevel = tc.level
			switch tc.refused {
			case "conf":
				pool.slices[stSlice].MetaGrpList[0].MetaBlocks = 0
			case "norev":
				pool.noRev = true
			}
			grp := pool.slices[stSlice].DataGrpList[0]
			grp.RepName = tc.name
			if tc.keepAll {
				for idx := 0; idx < common.MaxRetiredLegPerGrp; idx++ {
					legId := uint64(300 + 2*idx)
					pool.retired[legId] = &pb.RetiredLeg{
						GrpId: stDataGrp, SliceId: stSlice, Keep: true,
						Side: &pb.Side{SideId: legId + 1, AddrPort: dns[0]},
					}
					grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
				}
			}
			env.put(pool)
			if tc.refused == "lost" {
				pool.conf.TdNameList = append(pool.conf.TdNameList, "vol-gone")
				mustPut(t, env.cli, model.SpConfKey(env.cid, "p0"), pool.conf)
			}
			info := &pb.CntlrInfo{
				GrpIdToMdRaid: map[uint64]*pb.ResInfo{stDataGrp: stOkRow()},
			}
			if tc.verdict != nil {
				info.RepIdToRepairInfo = map[uint64]*pb.CntlrInfo_RepairInfo{
					50: tc.verdict,
				}
			}
			fake.setInfo(nil, nil, nil, info)
			rows := stRowsOf(env.status(0, ""), "p0")
			row := stOne(t, rows, pb.StatusKind_STATUS_KIND_REPAIR, tc.label,
				tc.fragments...)
			// Without the revision key the gateway refuses the operator's
			// cancel and finish too (checkSpToken): no row may name them.
			if tc.refused == "norev" &&
				strings.Contains(row.GetText(), "operator's repair") {
				t.Errorf("%s: text %q names an operator step the gateway "+
					"refuses", tc.label, row.GetText())
			}
			if row.GetObjName() != tc.name || row.GetGrpId() != stDataGrp {
				t.Errorf("row %v: want repair %s of group %d",
					row, tc.name, stDataGrp)
			}
		})
	}
}

// TestStatusGroups pins the group rows: an array not whole, a refused add,
// the operator in charge, a forced-start mark and a member with bad blocks;
// an array not whole whose leg has a row of its own gives the leg's row
// alone; and a RedundNone pool, which has no md array, lists none.
func TestStatusGroups(t *testing.T) {
	for _, tc := range []struct {
		label    string
		shape    func(pool *stPool, info *pb.CntlrInfo)
		kind     pb.StatusKind
		fragment string
	}{
		{"not whole", func(_ *stPool, info *pb.CntlrInfo) {
			info.GrpIdToGrpInfo[stDataGrp].Whole = false
			info.GrpIdToGrpInfo[stDataGrp].InSyncLegIdList = []uint64{stLeg}
		}, pb.StatusKind_STATUS_KIND_GROUP, "not whole"},
		{"refusal", func(_ *stPool, info *pb.CntlrInfo) {
			info.GrpIdToGrpInfo[stDataGrp].Refusal = "leg 11 carries uuid x"
		}, pb.StatusKind_STATUS_KIND_GROUP, "leg 11 carries uuid x"},
		{"operator in charge", func(pool *stPool, _ *pb.CntlrInfo) {
			pool.slices[stSlice].DataGrpList[0].OperatorInCharge = true
		}, pb.StatusKind_STATUS_KIND_GROUP, "operator"},
		{"forced start", func(pool *stPool, _ *pb.CntlrInfo) {
			pool.slices[stSlice].DataGrpList[0].ForcedStart =
				&pb.ForcedStart{LegId: stLeg, Revision: 3}
		}, pb.StatusKind_STATUS_KIND_GROUP, "forced start"},
		{"bad blocks", func(_ *stPool, info *pb.CntlrInfo) {
			info.GrpIdToGrpInfo[stDataGrp].LegIdToBadBlocks =
				map[uint64]uint32{stLeg: 3}
		}, pb.StatusKind_STATUS_KIND_GROUP, "bad-block"},
		{"not whole, the leg in error", func(_ *stPool, info *pb.CntlrInfo) {
			info.GrpIdToGrpInfo[stDataGrp].Whole = false
			info.GrpIdToGrpInfo[stDataGrp].InSyncLegIdList = []uint64{stLeg}
			info.LegIdToLeg[stLeg2] = &pb.ResInfo{
				Status: pb.ResStatus_RES_STATUS_ERROR, Details: "io error",
			}
		}, pb.StatusKind_STATUS_KIND_ROW, "io error"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			pool := env.newPool("p0", cn, stDns(env))
			info := &pb.CntlrInfo{
				GrpIdToMdRaid: map[uint64]*pb.ResInfo{stDataGrp: stOkRow()},
				LegIdToLeg:    map[uint64]*pb.ResInfo{},
				GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
					stDataGrp: {Whole: true},
				},
			}
			tc.shape(pool, info)
			fake.setInfo(nil, nil, nil, info)
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			row := stOne(t, rows, tc.kind, tc.label, tc.fragment)
			if len(rows) != 1 {
				t.Errorf("%d rows, want the one: %v", len(rows), rows)
			}
			if row.GetGrpId() != stDataGrp {
				t.Errorf("row %v: want group %d", row, stDataGrp)
			}
		})
	}

	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	pool := env.newPool("p0", cn, stDns(env))
	pool.conf.BdevConf.RedundConf = nil
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{stDataGrp: stOkRow()},
	})
	env.put(pool)
	stWantNone(t, stRowsOf(env.status(0, ""), "p0"), "a RedundNone pool")
}

// TestStatusForcedStartWithNoWorker pins the forced-start mark's text in a
// pool no worker drives, its revision key absent: the worker's clear is a
// step of its pass (dnv-worker.md AR13), so the text says the clear waits
// until a worker drives the pool again.
func TestStatusForcedStartWithNoWorker(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	pool := env.newPool("p0", cn, stDns(env))
	pool.slices[stSlice].DataGrpList[0].ForcedStart =
		&pb.ForcedStart{LegId: stLeg, Revision: 3}
	pool.noRev = true
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{stDataGrp: stOkRow()},
		GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
			stDataGrp: {Whole: true},
		},
	})
	env.put(pool)
	rows := stRowsOf(env.status(0, ""), "p0")
	stOne(t, rows, pb.StatusKind_STATUS_KIND_GROUP, "a forced start",
		"forced start", "drives the pool again")
	stOne(t, rows, pb.StatusKind_STATUS_KIND_LOST_RECORD,
		"the absent revision key", "revision key",
		"no step the pool's other rows name runs")
}

// TestStatusMigration pins the migration rows: one whose cutover is
// pending, with the primary's path row to the new side; past the cutover,
// one whose copy is whole and waits for its finish, read from the new
// side's disk node, and in a pool with no revision key for the key, which
// the finish reads first; and none for a copy still running.
func TestStatusMigration(t *testing.T) {
	const whole = "0 2048 clone 8 12/1024 64 1024/1024 0 0 1 " +
		"no_hydration 2 hydration_threshold 1 hydration_batch_size 1 rw"
	const half = "0 2048 clone 8 12/1024 64 512/1024 0 0 1 " +
		"no_hydration 2 hydration_threshold 1 hydration_batch_size 1 rw"
	for _, tc := range []struct {
		label    string
		pending  bool
		dmClone  string
		fragment string
		// noRev leaves the pool's revision key absent, which the
		// gateway's migr finish reads first and refuses without
		// (checkSpToken), so the row names the key's wait.
		noRev bool
	}{
		{"cutover pending", true, "", "cutover", false},
		{"whole", false, whole, "it waits for migr finish", false},
		{"whole, no revision key", false, whole, "revision key is back",
			true},
		{"half", false, half, "", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, cnFake := env.addCn("cn")
			dns := stDns(env)
			dst := fakeAddrPort(t, "dst")
			dnFake := startFakeAgent(t, dst, 0)
			env.addDn(dst, 64)
			dnFake.setInfo(nil, &pb.SideInfo{MigrDstInfo: &pb.SideInfo_MigrDstInfo{
				DmCloneInfo: &pb.ResInfo{
					Status: pb.ResStatus_RES_STATUS_OK, Details: tc.dmClone,
				},
			}}, nil, nil)
			pool := env.newPool("p0", cn, dns)
			leg := pool.slices[stSlice].DataGrpList[0].LegList[0]
			leg.SideList = append(leg.SideList,
				&pb.Side{SideId: 70, AddrPort: dst})
			pool.migrs["m1"] = &pb.Migration{
				MigrId: 71, SrcSideId: stSide, DstSideId: 70,
				CutoverPending: tc.pending,
			}
			pool.noRev = tc.noRev
			cnFake.setInfo(nil, nil, nil, &pb.CntlrInfo{
				SideIdToPath: map[uint64]*pb.ResInfo{70: {
					Status: pb.ResStatus_RES_STATUS_MISSING, Details: "no ctrl",
				}},
			})
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			if tc.fragment == "" {
				stWantNone(t, rows, tc.label)
				return
			}
			row := stOne(t, rows, pb.StatusKind_STATUS_KIND_MIGRATION,
				tc.label, tc.fragment)
			if row.GetObjName() != "m1" || row.GetSideId() != 70 {
				t.Errorf("row %v: want migration m1, new side 70", row)
			}
			if !tc.pending && dnFake.callCount("GetSideInfo") != 1 {
				t.Errorf("GetSideInfo called %d times, want 1",
					dnFake.callCount("GetSideInfo"))
			}
			if tc.pending && dnFake.callCount("GetSideInfo") != 0 {
				t.Errorf("GetSideInfo called before the cutover")
			}
		})
	}
}

// TestStatusClone pins the clone rows: one that waits for its source, one
// being deleted, whose drain waits for a worker in a pool whose revision key
// is absent, and none for a copy that runs.
func TestStatusClone(t *testing.T) {
	for _, tc := range []struct {
		label    string
		deleting bool
		target   *pb.ResInfo
		fragment string
		// noRev leaves the pool's revision key absent, so no worker
		// runs for the pool, its clone drain included.
		noRev bool
	}{
		{"source", false, &pb.ResInfo{
			Status:  pb.ResStatus_RES_STATUS_MISSING,
			Reason:  pb.ResReason_RES_REASON_SOURCE_FAULT,
			Details: "source not connected",
		}, "source not connected", false},
		{"deleting", true, stOkRow(), "deleted", false},
		{"deleting, no worker", true, stOkRow(),
			"once a worker drives the pool again", true},
		{"copying", false, stOkRow(), "", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			pool := env.newPool("p0", cn, stDns(env))
			pool.clones["c1"] = &pb.Clone{CloneId: 80, Deleting: tc.deleting}
			pool.noRev = tc.noRev
			fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
				CloneIdToTarget: map[uint64]*pb.ResInfo{80: tc.target},
			})
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			if tc.fragment == "" {
				stWantNone(t, rows, tc.label)
				return
			}
			row := stOne(t, rows, pb.StatusKind_STATUS_KIND_CLONE, tc.label,
				tc.fragment)
			if row.GetObjName() != "c1" {
				t.Errorf("row %v: want clone c1", row)
			}
		})
	}
}

// TestStatusDeletingCloneRowsAreItsDrains pins that a clone being deleted
// gives its one CLONE row and none of the primary's rows of it: neither its
// tables' rows, nor its meta group's, nor its legs', nor its repair's new
// leg's, though each reads ERROR, since they are its teardown's, which the
// CLONE row stands for, as its legs' health and its group rows are. A
// retired leg of its group still gives its health row, as in any pool.
func TestStatusDeletingCloneRowsAreItsDrains(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	dns := stDns(env)
	pool := env.newPool("p0", cn, dns)
	pool.clones["c1"] = &pb.Clone{CloneId: 80, Deleting: true,
		MetaGrp: &pb.Group{
			GrpId: 90, ExtCnt: 4, MetaBlocks: 3, DataBlocks: 2045,
			ArrayMade:        true,
			LegList:          []*pb.Leg{stLegOf(91, 0, dns[0]), stLegOf(93, 1, dns[1])},
			RetiredLegIdList: []uint64{97},
		}}
	pool.retired[97] = &pb.RetiredLeg{GrpId: 90, CloneName: "c1",
		Side: &pb.Side{SideId: 98, AddrPort: dns[2],
			ErrEpoch: uint64(time.Now().Add(-time.Minute).Unix())}}
	pool.repairs["r1"] = &pb.Repair{RepId: 50, GrpId: 90, CloneName: "c1",
		BadLegIdList: []uint64{91},
		NewLegList:   []*pb.Leg{stLegOf(95, 2, dns[2])}}
	errRow := func() *pb.ResInfo {
		return &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_ERROR,
			Details: "teardown"}
	}
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		GrpIdToMdRaid:    map[uint64]*pb.ResInfo{stDataGrp: stOkRow(), 90: errRow()},
		LegIdToLeg:       map[uint64]*pb.ResInfo{91: errRow(), 95: errRow()},
		CloneIdToTarget:  map[uint64]*pb.ResInfo{80: errRow()},
		CloneIdToDmClone: map[uint64]*pb.ResInfo{80: errRow()},
	})
	env.put(pool)
	rows := stRowsOf(env.status(0, ""), "p0")
	stOne(t, rows, pb.StatusKind_STATUS_KIND_CLONE, "the deleting clone",
		"being deleted")
	retired := stOne(t, rows, pb.StatusKind_STATUS_KIND_HEALTH,
		"its group's retired leg", "health epoch")
	if retired.GetLegId() != 97 ||
		!strings.HasPrefix(retired.GetObject(), "retired leg 97") {
		t.Errorf("row %v: want retired leg 97", retired)
	}
	if len(rows) != 2 {
		t.Errorf("%d rows, want the clone's and its retired leg's: %v",
			len(rows), rows)
	}
}

// stPoolLine is a thin pool's status line with these counts and mode.
func stPoolLine(usedMeta, totalMeta, usedData, totalData uint64,
	mode string) string {
	return fmt.Sprintf("0 1024 thin-pool 3 %d/%d %d/%d - %s "+
		"discard_passdown queue_if_no_space - 1024",
		usedMeta, totalMeta, usedData, totalData, mode)
}

// TestStatusGrowRefused pins the grows the read lists as refused by AR6's
// checks (dnv-worker.md AR6), one refusal a case: a full group list, the
// metadata ceiling, a controller node short of budget and no disk node with
// room; and the ones it does not list: a pending grow, a data grow the
// volumes do not need and a grow the disk nodes have room for. A pool out of
// data or metadata space gives a row of its own whatever its grow.
func TestStatusGrowRefused(t *testing.T) {
	bigTd := map[string]*pb.ThinDevice{"vol": {Size: 1 << 40}}
	for _, tc := range []struct {
		label    string
		line     string
		shape    func(env *stEnv, pool *stPool)
		roomy    bool
		kind     pb.StatusKind
		fragment string
	}{
		{"cn short", stPoolLine(1, 100, 60, 62, "rw"),
			func(env *stEnv, pool *stPool) {
				pool.tds = bigTd
				env.putCn(pool.cntlrs[stCntlr].GetAddrPort(), 2)
			}, true, pb.StatusKind_STATUS_KIND_GROW, model.ReasonCnBudgetShort},
		{"no disk node", stPoolLine(1, 100, 60, 62, "rw"),
			func(_ *stEnv, pool *stPool) { pool.tds = bigTd },
			false, pb.StatusKind_STATUS_KIND_GROW, "no candidate"},
		{"list full", stPoolLine(1, 100, 300, 310, "rw"),
			func(_ *stEnv, pool *stPool) {
				pool.tds = bigTd
				slice := pool.slices[stSlice]
				for len(slice.DataGrpList) < common.MaxGrpCntPerSlice {
					slice.DataGrpList = append(slice.DataGrpList,
						&pb.Group{GrpId: 1000 + uint64(len(slice.DataGrpList)),
							ExtCnt: 4, MetaBlocks: 3, DataBlocks: 1})
				}
				slice.DataGrpList[0].DataBlocks = 1
			}, true, pb.StatusKind_STATUS_KIND_GROW, model.ReasonGrpListFull},
		{"meta cap", stPoolLine(90, 100, 1, 62, "rw"),
			func(_ *stEnv, pool *stPool) {
				pool.slices[stSlice].MetaGrpList[0].ExtCnt = 1 << 10
			}, true, pb.StatusKind_STATUS_KIND_GROW, model.ReasonMetaLadderCap},
		{"covered", stPoolLine(1, 100, 60, 62, "rw"), nil,
			false, pb.StatusKind_STATUS_KIND_NONE, ""},
		{"pending", stPoolLine(1, 100, 60, 62, "rw"),
			func(_ *stEnv, pool *stPool) {
				pool.tds = bigTd
				slice := pool.slices[stSlice]
				slice.DataGrpList = append(slice.DataGrpList, &pb.Group{
					GrpId: 90, ExtCnt: 4, MetaBlocks: 3, DataBlocks: 2045,
				})
			}, false, pb.StatusKind_STATUS_KIND_NONE, ""},
		{"room", stPoolLine(1, 100, 60, 62, "rw"),
			func(_ *stEnv, pool *stPool) { pool.tds = bigTd },
			true, pb.StatusKind_STATUS_KIND_NONE, ""},
		{"out of data space", stPoolLine(1, 100, 62, 62, "out_of_data_space"),
			nil, false, pb.StatusKind_STATUS_KIND_SPACE, "out of data space"},
		{"out of metadata space", stPoolLine(100, 100, 1, 62, "ro"),
			nil, false, pb.StatusKind_STATUS_KIND_SPACE,
			"out of metadata space"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			var dns []string
			for idx := 0; idx < 4; idx++ {
				addr := fmt.Sprintf("dn-%d:9000", idx)
				env.addDn(addr, 1)
				dns = append(dns, addr)
			}
			if tc.roomy {
				env.addDn("dn-room-a:9000", 1000)
				env.addDn("dn-room-b:9000", 1000)
			}
			pool := env.newPool("p0", cn, dns)
			if tc.shape != nil {
				tc.shape(env, pool)
			}
			fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
				SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
					Status: pb.ResStatus_RES_STATUS_OK, Details: tc.line,
				}},
			})
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			if tc.kind == pb.StatusKind_STATUS_KIND_NONE {
				stWantNone(t, rows, tc.label)
				return
			}
			row := stOne(t, rows, tc.kind, tc.label, tc.fragment)
			if row.GetSliceId() != stSlice {
				t.Errorf("row %v: want slice %d", row, stSlice)
			}
			if len(rows) != 1 {
				t.Errorf("%d rows, want the one: %v", len(rows), rows)
			}
		})
	}
}

// TestStatusWorkers pins the worker rows, by the gateway's clock: a role
// with no registration, and a registration not refreshed for twice the
// default vote interval; a fresh one gives none.
func TestStatusWorkers(t *testing.T) {
	env := newStEnv(t)
	hwkClearRegistry(t, env.srv)
	now := time.Now()
	mustPut(t, env.cli, model.WorkerRegKey(common.WorkerRoleDn, "old"),
		&pb.WorkerReg{Epoch: uint64(now.Add(-time.Minute).Unix())})
	mustPut(t, env.cli, model.WorkerRegKey(common.WorkerRoleCn, "new"),
		&pb.WorkerReg{Epoch: uint64(now.Unix())})
	rows := stKinds(stRowsOf(env.status(0, ""), ""),
		pb.StatusKind_STATUS_KIND_WORKER)
	if len(rows) != 2 {
		t.Fatalf("%d worker rows, want 2: %v", len(rows), rows)
	}
	for _, row := range rows {
		switch row.GetObjName() {
		case common.WorkerRoleSp:
			if !strings.Contains(row.GetText(), "no worker") ||
				row.GetAge() != nil {
				t.Errorf("the sp role's row %v", row)
			}
		case common.WorkerRoleDn:
			if !strings.Contains(row.GetText(), "old") {
				t.Errorf("the dn role's row %v names no seed", row)
			}
			stWantAge(t, row, time.Minute)
		default:
			t.Errorf("a row of role %q", row.GetObjName())
		}
	}
}

// TestStatusPaging pins the pages: pools in key order, the cluster rows on
// the first page alone, the token of a full page, and the refusals of a bad
// count or token before the cluster read, and of an unknown cluster.
func TestStatusPaging(t *testing.T) {
	env := newStEnv(t)
	hwkClearRegistry(t, env.srv)
	cn, _ := env.addCn("cn")
	dns := stDns(env)
	for _, name := range []string{"pa", "pb", "pc"} {
		pool := env.newPool(name, cn, dns)
		pool.conf.SpLevel = pb.SpLevel_SP_LEVEL_READONLY
		env.put(pool)
	}
	first := env.status(2, "")
	if first.GetSpCnt() != 2 || first.GetPageToken() == "" {
		t.Fatalf("page 1: sp_cnt %d token %q, want 2 and a token",
			first.GetSpCnt(), first.GetPageToken())
	}
	if len(stRowsOf(first, "")) == 0 {
		t.Errorf("page 1 holds no cluster row")
	}
	for _, sp := range []string{"pa", "pb"} {
		stOne(t, stRowsOf(first, sp), pb.StatusKind_STATUS_KIND_POOL, sp,
			"SP_LEVEL_READONLY")
	}
	second := env.status(2, first.GetPageToken())
	if second.GetSpCnt() != 1 || second.GetPageToken() != "" {
		t.Errorf("page 2: sp_cnt %d token %q, want 1 and none",
			second.GetSpCnt(), second.GetPageToken())
	}
	if rows := stRowsOf(second, ""); len(rows) != 0 {
		t.Errorf("page 2 holds cluster rows: %v", rows)
	}
	stOne(t, stRowsOf(second, "pc"), pb.StatusKind_STATUS_KIND_POOL, "pc")

	for _, tc := range []struct {
		label string
		req   *pb.GetClusterStatusRequest
		want  codes.Code
	}{
		{"count", &pb.GetClusterStatusRequest{ClusterName: "nope",
			Count: common.MaxListCnt + 1}, codes.InvalidArgument},
		{"token", &pb.GetClusterStatusRequest{ClusterName: "nope",
			PageToken: "!!"}, codes.InvalidArgument},
		{"cluster", &pb.GetClusterStatusRequest{ClusterName: "nope"},
			codes.NotFound},
	} {
		_, err := env.srv.GetClusterStatus(env.ctx, tc.req)
		wantCode(t, err, tc.want, tc.label)
	}
}

// TestStatusRowCap pins the cap of a pool's rows: the first
// MaxStatusRowPerSp rows by rank, and one row more that counts the rest; a
// leg's row ranks before a group's, so a fault of one node is not hidden
// by the groups it touches.
func TestStatusRowCap(t *testing.T) {
	env := newStEnv(t)
	cn, _ := env.addCn("cn")
	dns := stDns(env)
	pool := env.newPool("p0", cn, dns)
	slice := pool.slices[stSlice]
	now := uint64(time.Now().Unix())
	const grpCnt = 20
	for idx := uint64(0); idx < grpCnt; idx++ {
		base := 200 + 10*idx
		leg := stLegOf(base+1, 0, dns[0])
		leg.ErrEpoch = now
		slice.DataGrpList = append(slice.DataGrpList, &pb.Group{
			GrpId: base, ExtCnt: 4, MetaBlocks: 3, DataBlocks: 2045,
			OperatorInCharge: true,
			LegList:          []*pb.Leg{leg, stLegOf(base+3, 1, dns[1])},
		})
	}
	env.put(pool)
	rows := stRowsOf(env.status(0, ""), "p0")
	if len(rows) != common.MaxStatusRowPerSp+1 {
		t.Fatalf("%d rows, want %d and one more", len(rows),
			common.MaxStatusRowPerSp)
	}
	for idx, row := range rows[:grpCnt] {
		if row.GetKind() != pb.StatusKind_STATUS_KIND_HEALTH ||
			row.GetLegId() == 0 {
			t.Errorf("row %d %v: want a leg's health row first", idx, row)
		}
	}
	more := rows[len(rows)-1]
	left := 2*grpCnt - common.MaxStatusRowPerSp
	if more.GetKind() != pb.StatusKind_STATUS_KIND_MORE ||
		!strings.Contains(more.GetText(), fmt.Sprint(left)) {
		t.Errorf("the last row %v: want one that counts %d more", more, left)
	}
}

// TestStatusTextCap pins the cut of a row's text to the row text cap
// (architecture.md, Live-state reporting), keeping its head and its end.
func TestStatusTextCap(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	long := "head " + strings.Repeat("x", 2*common.AgentTextMax) + " tail"
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{stDataGrp: {
			Status: pb.ResStatus_RES_STATUS_ERROR, Details: long,
		}},
	})
	env.put(env.newPool("p0", cn, stDns(env)))
	row := stOne(t, stRowsOf(env.status(0, ""), "p0"),
		pb.StatusKind_STATUS_KIND_ROW, "a long text", "head ", " tail",
		common.TextCutMark)
	if len(row.GetText()) > common.AgentTextMax {
		t.Errorf("text of %d bytes, want at most %d",
			len(row.GetText()), common.AgentTextMax)
	}
}

// TestStatusTraceId pins that the read's agent calls carry the request's
// trace id (grpc.md T3).
func TestStatusTraceId(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	env.put(env.newPool("p0", cn, stDns(env)))
	env.ctx = common.WithTraceId(context.Background(), "st-trace")
	env.status(0, "")
	ids := fake.traceIdList()
	if len(ids) != 1 || ids[0] != "st-trace" {
		t.Errorf("the agent saw trace ids %v, want [st-trace]", ids)
	}
}

// TestStatusPoolRows pins the rows of the pool as a whole, from etcd alone:
// a disabled primary, and none of its cntlrs enabled; a level above
// read-write; a pool being deleted, which gets that row alone and no agent
// call; and a torn thin device.
func TestStatusPoolRows(t *testing.T) {
	for _, tc := range []struct {
		label     string
		shape     func(env *stEnv, pool *stPool)
		kind      pb.StatusKind
		fragments []string
		calls     int
	}{
		{"disabled primary", func(env *stEnv, pool *stPool) {
			pool.cntlrs[stCntlr].Disabled = true
			env.putCn("cn-other:9000", 1<<20)
			pool.cntlrs[2] = &pb.Cntlr{AddrPort: "cn-other:9000"}
			pool.conf.CntlrIdList = append(pool.conf.CntlrIdList, 2)
		}, pb.StatusKind_STATUS_KIND_POOL, []string{"disabled"}, 1},
		{"no cntlr enabled", func(_ *stEnv, pool *stPool) {
			pool.cntlrs[stCntlr].Disabled = true
		}, pb.StatusKind_STATUS_KIND_POOL,
			[]string{"disabled", "no cntlr of the pool is enabled"}, 1},
		{"level", func(_ *stEnv, pool *stPool) {
			pool.conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		}, pb.StatusKind_STATUS_KIND_POOL,
			[]string{"SP_LEVEL_NO_THINPOOL", "reaction"}, 1},
		{"deleting", func(_ *stEnv, pool *stPool) {
			pool.conf.Deleting = true
			pool.cntlrs[stCntlr].ErrEpoch = uint64(time.Now().Unix())
		}, pb.StatusKind_STATUS_KIND_POOL, []string{"deleted"}, 0},
		{"torn", func(_ *stEnv, pool *stPool) {
			pool.tds["vol"] = &pb.ThinDevice{TdId: 90, Torn: true}
		}, pb.StatusKind_STATUS_KIND_TORN, []string{"torn"}, 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			pool := env.newPool("p0", cn, stDns(env))
			tc.shape(env, pool)
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			stOne(t, rows, tc.kind, tc.label, tc.fragments...)
			if len(rows) != 1 {
				t.Errorf("%d rows, want the one: %v", len(rows), rows)
			}
			if got := fake.callCount("GetCntlrInfo"); got != tc.calls {
				t.Errorf("GetCntlrInfo called %d times, want %d",
					got, tc.calls)
			}
		})
	}
}

// TestStatusDeadlineCutsThePage pins the read under a client's deadline: it
// starts no pool once less than one agent budget and the reply's margin is
// left, answers the pools it read with the token after the last of them,
// and the next page starts at the pool it did not read.
func TestStatusDeadlineCutsThePage(t *testing.T) {
	savedBudget, savedFan, savedMargin :=
		statusAgentBudget, statusFanOut, statusReplyMargin
	statusAgentBudget = 200 * time.Millisecond
	statusFanOut = 1
	statusReplyMargin = 50 * time.Millisecond
	t.Cleanup(func() {
		statusAgentBudget, statusFanOut, statusReplyMargin =
			savedBudget, savedFan, savedMargin
	})
	env := newStEnv(t)
	dns := stDns(env)
	for _, name := range []string{"pa", "pb", "pc"} {
		cn, fake := env.addCn("cn-" + name)
		release := fake.setBlock()
		t.Cleanup(release)
		env.put(env.newPool(name, cn, dns))
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		600*time.Millisecond)
	defer cancel()
	reply, err := env.srv.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{
		ClusterName: env.name,
	})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	if reply.GetSpCnt() != 2 || reply.GetPageToken() == "" {
		t.Fatalf("sp_cnt %d token %q, want 2 pools and a token",
			reply.GetSpCnt(), reply.GetPageToken())
	}
	for _, sp := range []string{"pa", "pb"} {
		stOne(t, stRowsOf(reply, sp), pb.StatusKind_STATUS_KIND_REPORT, sp)
	}
	statusAgentBudget = 50 * time.Millisecond
	next := env.status(0, reply.GetPageToken())
	if next.GetSpCnt() != 1 || len(stRowsOf(next, "pc")) == 0 {
		t.Errorf("the next page %v: want pool pc alone", next)
	}
}

// TestStatusPoolNotRead pins a pool whose load fails, a record that does
// not decode: one UNKNOWN row for it, and the other pools read.
func TestStatusPoolNotRead(t *testing.T) {
	env := newStEnv(t)
	cn, _ := env.addCn("cn")
	dns := stDns(env)
	bad := env.newPool("pa", cn, dns)
	env.put(bad)
	// A string field holding bytes that are not UTF-8 fails proto3's decode.
	mustPut(t, env.cli,
		model.CntlrKey(env.cid, bad.conf.GetSpId(), stCntlr),
		&pb.CloneBitmap{Bitmap: []byte{0xff, 0xfe}})
	good := env.newPool("pb", cn, dns)
	good.conf.SpLevel = pb.SpLevel_SP_LEVEL_READONLY
	env.put(good)
	reply := env.status(0, "")
	row := stOne(t, stRowsOf(reply, "pa"), pb.StatusKind_STATUS_KIND_REPORT,
		"a record that does not decode", "not read")
	stWantStatus(t, row, pb.ResStatus_RES_STATUS_UNKNOWN)
	stOne(t, stRowsOf(reply, "pb"), pb.StatusKind_STATUS_KIND_POOL, "pb")
	if reply.GetSpCnt() != 2 {
		t.Errorf("sp_cnt %d, want 2", reply.GetSpCnt())
	}
}

// TestStatusWidestReplyFits pins that the widest page fits GrpcMaxMsgSize:
// MaxListCnt pools, each with MaxStatusRowPerSp rows and the row that
// counts the rest, and the cluster rows of a cluster at the node ceilings,
// a lost revision key and a health epoch on every node, and MaxListCnt
// worker rows of each role, registrations that read gone; every row at once
// with the widest object the read composes, a text at the row text cap, the
// widest name in every name field and every id at its widest. No count
// bounds the registry's keys, so the worker rows are sized here, not
// bounded: every observer deletes a registration it commits dead
// (dnv-worker.md VW6), so the gone keys a read meets are those no live
// worker has committed dead yet, of workers that died within about one
// dead threshold and grace window or of a role with no live worker, and
// MaxListCnt per role is far more than a deployment runs. A worker row,
// with no id, is narrower than the row used for every row here.
func TestStatusWidestReplyFits(t *testing.T) {
	const maxId = ^uint64(0)
	name := strings.Repeat("n", common.MaxStrSize)
	addr := strings.Repeat("a", common.MaxStrSize)
	nqn := strings.Repeat("q", common.MaxNqnLength)
	sides := fmt.Sprintf("%d on dn %s, side %d on dn %s",
		maxId, addr, maxId, addr)
	var object string
	for _, candidate := range []string{
		model.SubsystemKey(maxId, maxId, nqn),
		fmt.Sprintf("namespace %d of %s", uint32(1<<32-1), nqn),
		fmt.Sprintf("leg %d (new leg of repair %s, group %d; side %s)",
			maxId, name, maxId, sides),
		fmt.Sprintf("leg %d (group %d, clone %s; side %s)",
			maxId, maxId, name, sides),
		fmt.Sprintf("retired leg %d (group %d; side %d on dn %s)",
			maxId, maxId, maxId, addr),
		fmt.Sprintf("migration %s (leg %d, side %d to side %d)",
			name, maxId, maxId, maxId),
		fmt.Sprintf("repair %s (group %d, clone %s)", name, maxId, name),
		cntlrObject(maxId, &pb.Cntlr{AddrPort: addr}),
	} {
		if len(candidate) > len(object) {
			object = candidate
		}
	}
	errStatus := pb.ResStatus_RES_STATUS_ERROR
	row := &pb.StatusRow{
		SpName:   name,
		Kind:     pb.StatusKind_STATUS_KIND_MORE,
		Object:   object,
		Status:   &errStatus,
		Reason:   pb.ResReason_RES_REASON_IDENTITY_UNDECIDED,
		Text:     strings.Repeat("t", common.AgentTextMax),
		Age:      durationpb.New(time.Duration(1<<63 - 1)),
		CntlrId:  maxId,
		SliceId:  maxId,
		GrpId:    maxId,
		LegId:    maxId,
		SideId:   maxId,
		ObjName:  nqn,
		AddrPort: addr,
	}
	reply := &pb.GetClusterStatusReply{
		ClusterName: name,
		PageToken:   encodePageToken(model.SpConfKey(maxId, name)),
		SpCnt:       common.MaxListCnt,
		ReadTime:    timestamppb.Now(),
	}
	rowCnt := common.MaxListCnt*(common.MaxStatusRowPerSp+1) +
		2*(common.MaxDnCntPerCluster+common.MaxCnCntPerCluster) +
		len(listedWorkerRoles)*common.MaxListCnt
	for idx := 0; idx < rowCnt; idx++ {
		reply.RowList = append(reply.RowList, row)
	}
	size := proto.Size(reply)
	t.Logf("the widest status page is %d bytes, %.1f%% of GrpcMaxMsgSize",
		size, 100*float64(size)/float64(common.GrpcMaxMsgSize))
	if size > common.GrpcMaxMsgSize {
		t.Errorf("the widest status page is %d bytes, over GrpcMaxMsgSize %d",
			size, common.GrpcMaxMsgSize)
	}
}

// TestStatusInvalidConf pins a pool whose stored bdev_conf the readers
// refuse: one POOL row that names the refusal, and no grow judged from that
// conf, though the pool's thin pool reads past any mark and no disk node
// has room.
func TestStatusInvalidConf(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	var dns []string
	for idx := 0; idx < 4; idx++ {
		addr := fmt.Sprintf("dn-%d:9000", idx)
		env.addDn(addr, 1)
		dns = append(dns, addr)
	}
	pool := env.newPool("p0", cn, dns)
	pool.conf.BdevConf.DmPoolConf.LowWaterMarkPct = 0
	pool.tds["vol"] = &pb.ThinDevice{Size: 1 << 40}
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: stPoolLine(1, 100, 60, 62, "rw"),
		}},
	})
	env.put(pool)
	rows := stRowsOf(env.status(0, ""), "p0")
	stOne(t, rows, pb.StatusKind_STATUS_KIND_POOL, "an invalid conf",
		"invalid stored conf")
	if len(rows) != 1 {
		t.Errorf("%d rows, want the one: %v", len(rows), rows)
	}
}

// TestStatusNotWholeBesideAnInSyncLegsRow pins that a group's "not whole"
// row stays when the leg that has a row of its own is one md holds in sync:
// that row, the bad blocks of the member the array runs on, does not say
// which leg the array lacks.
func TestStatusNotWholeBesideAnInSyncLegsRow(t *testing.T) {
	env := newStEnv(t)
	cn, fake := env.addCn("cn")
	fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
		LegIdToLeg: map[uint64]*pb.ResInfo{
			stLeg: stOkRow(), stLeg2: stOkRow(),
		},
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{stDataGrp: stOkRow()},
		GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
			stDataGrp: {
				InSyncLegIdList:  []uint64{stLeg},
				LegIdToBadBlocks: map[uint64]uint32{stLeg: 3},
			},
		},
	})
	env.put(env.newPool("p0", cn, stDns(env)))
	rows := stKinds(stRowsOf(env.status(0, ""), "p0"),
		pb.StatusKind_STATUS_KIND_GROUP)
	notWhole := 0
	for _, row := range rows {
		if row.GetLegId() == 0 &&
			strings.Contains(row.GetText(), "not whole") {
			notWhole++
		}
	}
	if len(rows) != 2 || notWhole != 1 {
		t.Errorf("rows %v: want the leg's bad blocks and the group's "+
			"not-whole row", rows)
	}
}

// TestStatusDeadlineLeavesACutPoolOff pins a pool whose work the read's own
// deadline ended, not its agent. The page's first pool answers at once;
// the two after it share an agent that answers each call in 400 ms, so the
// one that waits for the other's call makes its own after it and the read's
// deadline cuts it. That pool, and any after it, are left off the page, the
// token goes before it, and the next page reads them; no row says an agent
// did not answer.
func TestStatusDeadlineLeavesACutPoolOff(t *testing.T) {
	savedBudget, savedFan, savedMargin :=
		statusAgentBudget, statusFanOut, statusReplyMargin
	statusAgentBudget = 500 * time.Millisecond
	statusFanOut = 4
	statusReplyMargin = 50 * time.Millisecond
	t.Cleanup(func() {
		statusAgentBudget, statusFanOut, statusReplyMargin =
			savedBudget, savedFan, savedMargin
	})
	env := newStEnv(t)
	dns := stDns(env)
	fast, _ := env.addCn("fast")
	env.put(env.newPool("p0", fast, dns))
	slow, fake := env.addCn("slow")
	env.put(env.newPool("pa", slow, dns))
	env.put(env.newPool("pb", slow, dns))
	release1 := fake.setBlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(400 * time.Millisecond)
		release2 := fake.setBlock()
		release1()
		time.Sleep(400 * time.Millisecond)
		release2()
	}()
	ctx, cancel := context.WithTimeout(context.Background(),
		700*time.Millisecond)
	defer cancel()
	reply, err := env.srv.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{
		ClusterName: env.name,
	})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	<-done
	first := reply.GetSpCnt()
	if first < 1 || first > 2 || reply.GetPageToken() == "" {
		t.Fatalf("sp_cnt %d token %q, want the cut pool off the page and "+
			"a token", first, reply.GetPageToken())
	}
	next := env.status(0, reply.GetPageToken())
	if next.GetSpCnt() != 3-first {
		t.Errorf("the next page read %d pools, want %d",
			next.GetSpCnt(), 3-first)
	}
	for _, page := range []*pb.GetClusterStatusReply{reply, next} {
		for _, row := range page.GetRowList() {
			if row.GetKind() == pb.StatusKind_STATUS_KIND_REPORT {
				t.Errorf("a report row: %v", row)
			}
		}
	}
}

// TestStatusDeadlineKeepsTheFirstPool pins the page's first pool cut by the
// read's own deadline: it stays on the page, so a page always moves on, and
// its row says the read's time ran out, not that the agent did not answer;
// the id-to-name read, which the cut pool never made, has its row saying
// the same.
func TestStatusDeadlineKeepsTheFirstPool(t *testing.T) {
	savedBudget, savedMargin := statusAgentBudget, statusReplyMargin
	statusAgentBudget = 2 * time.Second
	statusReplyMargin = 50 * time.Millisecond
	t.Cleanup(func() {
		statusAgentBudget, statusReplyMargin = savedBudget, savedMargin
	})
	env := newStEnv(t)
	cn, fake := env.addCn("hang")
	release := fake.setBlock()
	defer release()
	pool := env.newPool("pa", cn, stDns(env))
	env.put(pool)
	ctx, cancel := context.WithTimeout(context.Background(),
		300*time.Millisecond)
	defer cancel()
	reply, err := env.srv.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{
		ClusterName: env.name,
	})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	if reply.GetSpCnt() != 1 {
		t.Fatalf("sp_cnt %d, want the first pool kept", reply.GetSpCnt())
	}
	nameKey := model.SpNameKey(env.cid, pool.conf.GetSpId())
	rows := stKinds(stRowsOf(reply, "pa"), pb.StatusKind_STATUS_KIND_REPORT)
	objects := map[string]bool{}
	for _, row := range rows {
		objects[row.GetObject()] = true
		if !strings.Contains(row.GetText(), "time ran out") {
			t.Errorf("row %q: text %q does not say the read's time ran out",
				row.GetObject(), row.GetText())
		}
	}
	if len(rows) != 2 || !objects[nameKey] {
		t.Errorf("report rows %v, want the primary's and the id-to-name "+
			"read's %s", rows, nameKey)
	}
}

// TestStatusPrimaryBehind pins a primary whose report is of an older
// revision than the pool's while its health epoch is set: a syncup still on
// its way, or one its agent refuses or fails, as one whose store refuses its
// saves; the read cannot tell which, and its row says both, whole, within
// the row text cap at the widest revisions.
func TestStatusPrimaryBehind(t *testing.T) {
	env := newStEnv(t)
	cn, _ := env.addCn("cn")
	pool := env.newPool("p0", cn, stDns(env))
	pool.cntlrs[stCntlr].ErrEpoch = uint64(time.Now().Unix())
	env.put(pool)
	mustPut(t, env.cli, model.SpRevKey(0, env.cid, pool.conf.GetSpId()),
		&pb.SpRev{SpName: "p0", Revision: fakeAgentRevision + 3})
	rows := stRowsOf(env.status(0, ""), "p0")
	row := stOne(t, rows, pb.StatusKind_STATUS_KIND_REPORT, "behind",
		fmt.Sprint(fakeAgentRevision+3), "on its way", "store cannot save",
		"worker's log")
	stWantStatus(t, row, pb.ResStatus_RES_STATUS_UNKNOWN)
	if strings.Contains(row.GetText(), common.TextCutMark) {
		t.Errorf("text %q is cut", row.GetText())
	}
	// At the widest revisions the text still fits the cap uncut.
	widest := &pb.SpRev{SpName: "p0", Revision: ^uint64(0)}
	mustPut(t, env.cli, model.SpRevKey(0, env.cid, pool.conf.GetSpId()),
		widest)
	row = stOne(t, stRowsOf(env.status(0, ""), "p0"),
		pb.StatusKind_STATUS_KIND_REPORT, "behind, widest",
		fmt.Sprint(^uint64(0)), "worker's log")
	if strings.Contains(row.GetText(), common.TextCutMark) {
		t.Errorf("text %q is cut at the widest revision", row.GetText())
	}

	// Behind with no health epoch is a fan-out in flight: no row.
	env2 := newStEnv(t)
	cn2, _ := env2.addCn("cn")
	pool2 := env2.newPool("p0", cn2, stDns(env2))
	env2.put(pool2)
	mustPut(t, env2.cli, model.SpRevKey(0, env2.cid, pool2.conf.GetSpId()),
		&pb.SpRev{SpName: "p0", Revision: fakeAgentRevision + 3})
	stWantNone(t, stKinds(stRowsOf(env2.status(0, ""), "p0"),
		pb.StatusKind_STATUS_KIND_REPORT), "behind, no epoch")
}

// TestStatusMigrationLeftover pins a migration's new side whose agent
// answers a leftover: its rows are read as a clean answer's are, so a
// whole copy still reads as one that waits for its finish.
func TestStatusMigrationLeftover(t *testing.T) {
	const whole = "0 2048 clone 8 12/1024 64 1024/1024 0 0 1 " +
		"no_hydration 2 hydration_threshold 1 hydration_batch_size 1 rw"
	env := newStEnv(t)
	cn, _ := env.addCn("cn")
	dns := stDns(env)
	dst := fakeAddrPort(t, "dst")
	dnFake := startFakeAgent(t, dst, 0)
	env.addDn(dst, 64)
	dnFake.setInfo(nil, &pb.SideInfo{MigrDstInfo: &pb.SideInfo_MigrDstInfo{
		DmCloneInfo: &pb.ResInfo{
			Status: pb.ResStatus_RES_STATUS_OK, Details: whole,
		},
	}}, nil, nil)
	dnFake.setSideReply(&pb.AgentReply{Code: common.ReplyCodeLeftover})
	pool := env.newPool("p0", cn, dns)
	leg := pool.slices[stSlice].DataGrpList[0].LegList[0]
	leg.SideList = append(leg.SideList, &pb.Side{SideId: 70, AddrPort: dst})
	pool.migrs["m1"] = &pb.Migration{
		MigrId: 71, SrcSideId: stSide, DstSideId: 70,
	}
	env.put(pool)
	stOne(t, stRowsOf(env.status(0, ""), "p0"),
		pb.StatusKind_STATUS_KIND_MIGRATION, "a leftover answer",
		"migr finish")
}

// TestStatusGroupWithNoLength pins a pool one of whose groups gives its
// sides no length to zero, which the sp worker's gate refuses (dnv-worker.md
// AR1): one POOL row naming the refusal.
func TestStatusGroupWithNoLength(t *testing.T) {
	env := newStEnv(t)
	cn, _ := env.addCn("cn")
	pool := env.newPool("p0", cn, stDns(env))
	pool.slices[stSlice].DataGrpList[0].MetaBlocks = 0
	env.put(pool)
	stOne(t, stRowsOf(env.status(0, ""), "p0"),
		pb.StatusKind_STATUS_KIND_POOL, "a group with no length",
		"meta_blocks is zero")
}

// TestStatusLostNameRecord pins a pool whose id-to-name record is absent,
// or names another pool: a lost-record row naming the key.
func TestStatusLostNameRecord(t *testing.T) {
	for _, tc := range []struct {
		label string
		plant func(env *stEnv, key string)
	}{
		{"absent", func(env *stEnv, key string) {
			if err := env.cli.Delete(env.ctx, key); err != nil {
				env.t.Fatalf("Delete: %v", err)
			}
		}},
		{"another pool's", func(env *stEnv, key string) {
			mustPut(env.t, env.cli, key, &pb.SpName{SpName: "other"})
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, _ := env.addCn("cn")
			pool := env.newPool("p0", cn, stDns(env))
			env.put(pool)
			key := model.SpNameKey(env.cid, pool.conf.GetSpId())
			tc.plant(env, key)
			row := stOne(t, stRowsOf(env.status(0, ""), "p0"),
				pb.StatusKind_STATUS_KIND_LOST_RECORD, tc.label,
				"id-to-name")
			if row.GetObject() != key {
				t.Errorf("object %q, want the key %q", row.GetObject(), key)
			}
		})
	}
}

// TestStatusDeadlineNamesOnlyTheCallItCuts pins that, on a page's first
// pool the read's own deadline cut, only a call that deadline ended says
// the read's time ran out: a primary that refused the connection at once
// still reads as an agent that did not answer, while the call to a
// migration's new side, held until the deadline, says the time ran out.
func TestStatusDeadlineNamesOnlyTheCallItCuts(t *testing.T) {
	savedBudget, savedMargin := statusAgentBudget, statusReplyMargin
	statusAgentBudget = 2 * time.Second
	statusReplyMargin = 50 * time.Millisecond
	t.Cleanup(func() {
		statusAgentBudget, statusReplyMargin = savedBudget, savedMargin
	})
	env := newStEnv(t)
	dns := stDns(env)
	gone := fakeAddrPort(t, "gone")
	env.putCn(gone, 1<<20)
	dst := fakeAddrPort(t, "dst")
	dnFake := startFakeAgent(t, dst, 0)
	env.addDn(dst, 64)
	release := dnFake.setBlock()
	defer release()
	pool := env.newPool("pa", gone, dns)
	leg := pool.slices[stSlice].DataGrpList[0].LegList[0]
	leg.SideList = append(leg.SideList, &pb.Side{SideId: 70, AddrPort: dst})
	pool.migrs["m1"] = &pb.Migration{
		MigrId: 71, SrcSideId: stSide, DstSideId: 70,
	}
	env.put(pool)
	ctx, cancel := context.WithTimeout(context.Background(),
		300*time.Millisecond)
	defer cancel()
	reply, err := env.srv.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{
		ClusterName: env.name,
	})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	// The id-to-name read, after the calls, is one the deadline cut too;
	// the primary's row is the other report row.
	nameKey := model.SpNameKey(env.cid, pool.conf.GetSpId())
	var rows, nameRows []*pb.StatusRow
	for _, row := range stRowsOf(reply, "pa") {
		if row.GetObject() == nameKey {
			nameRows = append(nameRows, row)
			continue
		}
		rows = append(rows, row)
	}
	stOne(t, nameRows, pb.StatusKind_STATUS_KIND_REPORT,
		"the name read the deadline cut", "time ran out")
	report := stOne(t, rows, pb.StatusKind_STATUS_KIND_REPORT,
		"the primary that refused at once", "did not answer")
	if strings.Contains(report.GetText(), "time ran out") {
		t.Errorf("the primary's row %q blames the read's deadline",
			report.GetText())
	}
	stOne(t, rows, pb.StatusKind_STATUS_KIND_MIGRATION,
		"the new side's call the deadline cut", "time ran out")
}
