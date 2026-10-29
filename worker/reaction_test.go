package worker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The fixture SP of AR1-AR10 (AR6's pending rule with real group geometry per
// architecture.md, Group on-leg layout: meta region, data region, health block)
// ---------------------------------------------------------------------------

const (
	// The geometry every reaction fixture is built with: 64 MiB extents,
	// 1 MiB pool blocks, 128-block bitmap chunks, md-raid1. model.GroupBlocks
	// turns them into the meta_blocks / data_blocks the AR6 pending rule does
	// its arithmetic on, so the numbers below are never hand-written.
	reactExtentSize = uint64(64) << 20
	reactBlockSize  = uint64(1) << 20
	// reactTdSize is the fixture thin device's size (reactFixture).
	reactTdSize      = uint64(1) << 40
	reactChunkBlocks = uint64(128)

	// The cluster's stored alloc_conf. Both are deliberately NOT
	// common.DefaultAllocDnBatchSize / DefaultAllocCnBatchSize (16 and 16) and
	// are deliberately DIFFERENT from each other: a scan that substituted the
	// constant, or that read the wrong one of the two members, would ask for a
	// different number than the assertions below name. They are untyped so
	// they compare against the int candCnt of a recorded query.
	reactDnBatch = 5
	reactCnBatch = 3

	reactSliceId = uint64(10)
	reactMetaGrp = uint64(20)
	reactDataGrp = uint64(21)

	reactCntlrA = uint64(1)
	reactCntlrB = uint64(2)

	reactMetaLegA = uint64(100)
	reactMetaLegB = uint64(101)
	reactDataLegA = uint64(110)
	reactDataLegB = uint64(111)

	reactSideMetaA = uint64(200)
	reactSideMetaB = uint64(201)
	reactSideDataA = uint64(210)
	reactSideDataB = uint64(211)

	reactDnA = "rdn0:9520"
	reactDnB = "rdn1:9520"
	reactDnC = "rdn2:9520"
	reactDnD = "rdn3:9520"

	reactCnA = "rcn0:9620"
	reactCnB = "rcn1:9620"
	reactCnC = "rcn2:9620"

	// reactSpRevision is what the seeded SpRev reports, i.e. the "revision"
	// attribute of every `reaction applied` record.
	reactSpRevision = uint64(77)
)

// reactBdevConf is the fixture's SP-wide geometry: the concrete bdev_conf
// CreateStoragePool stores (testBdevConf), with the pool block size and bitmap
// chunk count spelled out from the constants above so the group-layout
// arithmetic (architecture.md, Group on-leg layout: meta region, data region,
// health block) in this file and the stored conf can never drift apart. Every
// member is concrete because architecture.md, Common validation, resolves them
// on the write path — a zero anywhere in here is what model.ValidateBdevConf
// refuses.
func reactBdevConf() *pb.BdevConf {
	conf := testBdevConf()
	conf.DmPoolConf.DataBlockSize = reactBlockSize
	conf.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{
				BitmapChunkBlockCnt: reactChunkBlocks,
			},
		},
	}
	return conf
}

// reactClusterConf is the cluster the fixture SP lives in: the stored conf of
// testClusterConf with 64 MiB extents and an alloc_conf of its own, so the
// batch sizes the candidate scans below assert on can only have come from this
// message. Both values are inside the bounds [1, 1024] of architecture.md,
// Common validation, so the pass gate (model.ValidateClusterConf) accepts the
// fixture.
func reactClusterConf() *pb.ClusterConf {
	return testClusterConf(func(cc *pb.ClusterConf) {
		cc.DnBinConf.ExtentSize = reactExtentSize
		cc.BdevConf = reactBdevConf()
		cc.AllocConf.DnBatchSize = reactDnBatch
		cc.AllocConf.CnBatchSize = reactCnBatch
	})
}

// reactGroup builds one group with the real block counts (architecture.md,
// Group on-leg layout: meta region, data region, health block) of extCnt
// extents and one single-sided leg per (leg_id, side_id, addr) triple.
func reactGroup(
	t *testing.T,
	grpId uint64,
	extCnt uint64,
	legIds []uint64,
	sideIds []uint64,
	addrs []string,
) *pb.Group {
	t.Helper()
	metaBlocks, dataBlocks, err := model.GroupBlocks(
		extCnt, reactExtentSize, reactBdevConf(),
	)
	if err != nil {
		t.Fatalf("group blocks: %v", err)
	}
	grp := &pb.Group{
		GrpId:      grpId,
		ExtCnt:     extCnt,
		MetaBlocks: metaBlocks,
		DataBlocks: dataBlocks,
	}
	for idx := range legIds {
		grp.LegList = append(grp.LegList, &pb.Leg{
			LegId:  legIds[idx],
			LegIdx: uint32(idx),
			SideList: []*pb.Side{{
				SideId:      sideIds[idx],
				AddrPort:    addrs[idx],
				Provisioned: true,
			}},
		})
	}
	return grp
}

// reactDataBlocks is the data_blocks of a group of extCnt extents, the unit
// the AR6 data pending rule sums.
func reactDataBlocks(t *testing.T, extCnt uint64) uint64 {
	t.Helper()
	_, dataBlocks, err := model.GroupBlocks(
		extCnt, reactExtentSize, reactBdevConf(),
	)
	if err != nil {
		t.Fatalf("group blocks: %v", err)
	}
	return dataBlocks
}

// reactMetaBlocks is the number of dm-thin METADATA blocks, each
// common.ThinMetaBlockSize bytes, a meta group of extCnt extents contributes
// to the pool's metadata device.
func reactMetaBlocks(t *testing.T, extCnt uint64) uint64 {
	t.Helper()
	return reactDataBlocks(t, extCnt) * reactBlockSize / common.ThinMetaBlockSize
}

// reactFixture is the SP every reaction test starts from: two cntlrs (one
// primary), one slice with one meta group (1 extent) and one data group (2
// extents), every leg healthy and every side provisioned.
func reactFixture(t *testing.T) *model.SpState {
	t.Helper()
	return &model.SpState{
		Rev: 9,
		Conf: &pb.SpConf{
			SpId:           testSpId,
			ShardCode:      testShard,
			NextId:         1000,
			BdevConf:       reactBdevConf(),
			CntlidSlotList: []uint32{0},
			SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
			CntlrIdList:    []uint64{reactCntlrA, reactCntlrB},
			SliceIdList:    []uint64{reactSliceId},
			TdNameList:     []string{"td0"},
		},
		Cntlrs: map[uint64]*pb.Cntlr{
			reactCntlrA: {AddrPort: reactCnA, Primary: true},
			reactCntlrB: {AddrPort: reactCnB},
		},
		Slices: map[uint64]*pb.Slice{
			reactSliceId: {
				SliceIdx: 0,
				MetaGrpList: []*pb.Group{reactGroup(
					t, reactMetaGrp, 1,
					[]uint64{reactMetaLegA, reactMetaLegB},
					[]uint64{reactSideMetaA, reactSideMetaB},
					[]string{reactDnA, reactDnB},
				)},
				DataGrpList: []*pb.Group{reactGroup(
					t, reactDataGrp, 2,
					[]uint64{reactDataLegA, reactDataLegB},
					[]uint64{reactSideDataA, reactSideDataB},
					[]string{reactDnA, reactDnB},
				)},
			},
		},
		// td0's size keeps every test's slice far from covered (AR6's
		// covered rule): one TiB against data groups of a few hundred
		// blocks, so only the tests of that rule set a size of their own.
		Tds:      []*pb.ThinDevice{{TdId: 500, DevId: 1, Size: reactTdSize}},
		TdNames:  []string{"td0"},
		DnByAddr: map[string]*pb.DnConf{},
		CnByAddr: map[string]*pb.CnConf{},
	}
}

// ---------------------------------------------------------------------------
// The fake model surface
// ---------------------------------------------------------------------------

// reactionCall is one mutation a pass ran, in call order.
type reactionCall struct {
	op        string
	oldId     uint64
	newId     uint64
	sliceId   uint64
	grpId     uint64
	isMeta    bool
	poolTotal uint64
	legs      []model.Cand
	asPrimary bool
	badLegId  uint64
	now       uint64
	// The clone drain's target, for the CLD7 derivation assertions, and the
	// clone a repair's create names as its group's parent (AR8 on a clone
	// meta group).
	cloneName string
	cloneId   uint64
	// The plan a replacement was scanned against (AR7's spCnAddrs).
	spCn []string
	// The plan a repair's create was scanned against (AR8's black list),
	// which the op holds the group's DNs against.
	grpDn []string
	// The cluster conf the clone drain's final STM (CLD7) or an SPD15
	// batch was handed.
	cc *pb.ClusterConf
	// The batch an SPD15 drain step was handed: the repairs' names, or the
	// retired legs' ids.
	repNames []string
	legIds   []uint64
	// A stage move's repair and stage (AR11), the repair a leg repair's op
	// names (AR8), and the retire timer's delete (AR12): its leg in legIds,
	// with the revision it expected.
	repName   string
	repId     uint64
	stage     pb.RepStage
	expectRev uint64
	// The mark the forced-start clear (AR13) was handed: the group's parent
	// and id, and the mark's leg and revision as the snapshot read them.
	mark model.ForcedStartRef
}

// candQuery is one allocator scan a pass ran.
type candQuery struct {
	kind        string
	candExt     uint64
	candCnt     int
	requiredCnt int
	black       []string
	excludeLocs []string
	spCn        []string
}

// fakeReactionOps is the unit tests' stand-in for model: canned candidates, a
// canned error and a record of every scan and every mutation.
type fakeReactionOps struct {
	mu      sync.Mutex
	dnCands []model.Cand
	cnCands []model.Cand
	scanErr error
	opErr   error
	calls   []reactionCall
	queries []candQuery
	// The sp drain's canned returns (drain.go). drainErr is separate from
	// opErr so a test can fail a drain step without failing every reaction —
	// the two never run in the same pass (SPD6), and keeping them apart is
	// what lets a drain test reuse a fixture that sets opErr.
	drainCntlrCnt   int
	drainRepCnt     int
	drainRetiredCnt int
	drainGrpCnt     int
	drainDone       bool
	drainErr        error
	// The clone drain's canned returns (clonedrain.go). cloneDrainErr is
	// separate again, so a clone-drain test can fail its step while the sp
	// drain and the reactions of the same pass stay healthy — CLD7 runs them
	// in one pass, which the sp drain never does.
	cloneChunks   [][]model.BmChunk
	cloneDrainErr error
	// newCntlrIds are the cntlr_ids replaceCntlr hands out, one per call in
	// order; 555 once they run out.
	newCntlrIds []uint64
	// stageErr and retireErr fail the stage moves (AR11) and the retire
	// timer's deletes (AR12), apart from opErr, so a test can fail either
	// pass step while the reactions of the same pass stay healthy.
	stageErr  error
	retireErr error
	// markErr fails the forced-start clear (AR13) apart from opErr, as
	// stageErr fails a stage move, and markSkip makes it skip the mark as
	// one the store no longer holds, as model.ClearForcedStart skips a mark
	// replaced or withdrawn since the snapshot.
	markErr  error
	markSkip bool
	// repairErr fails the leg repair ops (AR8) on the repair they name,
	// ahead of opErr, so a test can refuse the drive step on one repair
	// while it drives another. finishRes is what a finish that commits
	// returns.
	repairErr map[string]error
	finishRes model.FinishRepairResult
	// spRev is the SpRev revision as the ops see it: the harness sets it to
	// the snapshot's SpRevision before each pass, and a stage move, a
	// forced-start clear, a clone drain's final step and a retired leg's
	// delete that succeed bump it, as model.SetRepairStage,
	// model.ClearForcedStart, model.FinishCloneDelete and
	// model.DeleteRetiredLeg bump SpRev. deleteRetiredLeg refuses an
	// expected revision that differs, as model's revision check does, so a
	// pass that bumped SpRev before its retire timer sees the delete
	// refused (AR12).
	spRev uint64
}

func (o *fakeReactionOps) record(call reactionCall) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
	return o.opErr
}

func (o *fakeReactionOps) allCalls() []reactionCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]reactionCall(nil), o.calls...)
}

func (o *fakeReactionOps) allQueries() []candQuery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]candQuery(nil), o.queries...)
}

func (o *fakeReactionOps) findDnCandidates(
	ctx context.Context,
	cid uint64,
	cc *pb.ClusterConf,
	candExt uint64,
	candCnt int,
	requiredCnt int,
	black []string,
	excludeLocs []string,
) ([]model.Cand, error) {
	o.mu.Lock()
	o.queries = append(o.queries, candQuery{
		kind: "dn", candExt: candExt, candCnt: candCnt,
		requiredCnt: requiredCnt, black: black, excludeLocs: excludeLocs,
	})
	cands, err := o.dnCands, o.scanErr
	o.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return cands, nil
}

func (o *fakeReactionOps) findCnCandidates(
	ctx context.Context,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	spCnAddrs []string,
	excludeLocs []string,
) ([]model.Cand, error) {
	o.mu.Lock()
	o.queries = append(o.queries, candQuery{
		kind: "cn", candExt: candExt, candCnt: candCnt,
		black: black, spCn: spCnAddrs, excludeLocs: excludeLocs,
	})
	cands, err := o.cnCands, o.scanErr
	o.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return cands, nil
}

func (o *fakeReactionOps) failover(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newId uint64,
	now uint64,
) error {
	return o.record(reactionCall{
		op: "failover", oldId: oldId, newId: newId, now: now,
	})
}

func (o *fakeReactionOps) growSlice(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	isMeta bool,
	poolTotal uint64,
	cc *pb.ClusterConf,
	legs []model.Cand,
) (uint64, error) {
	err := o.record(reactionCall{
		op: "grow", sliceId: sliceId, isMeta: isMeta,
		poolTotal: poolTotal, legs: legs,
	})
	return 999, err
}

func (o *fakeReactionOps) replaceCntlr(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newCn model.Cand,
	spCnAddrs []string,
	asPrimary bool,
	now uint64,
) (uint64, error) {
	err := o.record(reactionCall{
		op: "replace", oldId: oldId, asPrimary: asPrimary, now: now,
		legs: []model.Cand{newCn}, spCn: spCnAddrs,
	})
	o.mu.Lock()
	defer o.mu.Unlock()
	newId := uint64(555)
	if len(o.newCntlrIds) != 0 {
		newId, o.newCntlrIds = o.newCntlrIds[0], o.newCntlrIds[1:]
	}
	return newId, err
}

// The four leg repair ops (AR8) record like every other mutation, and fail
// with the repair's own canned error, when it has one, ahead of opErr.

func (o *fakeReactionOps) recordRepair(call reactionCall) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
	if err, ok := o.repairErr[call.repName]; ok {
		return err
	}
	return o.opErr
}

func (o *fakeReactionOps) createRepair(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
	parent model.GrpParent,
	grpId uint64,
	badLegId uint64,
	newLeg model.RepairNewLeg,
	cc *pb.ClusterConf,
) (uint64, uint64, error) {
	err := o.recordRepair(reactionCall{
		op: "create_repair", repName: repName, sliceId: parent.SliceId,
		cloneName: parent.CloneName, grpId: grpId, badLegId: badLegId,
		legs: []model.Cand{newLeg.Dn}, grpDn: newLeg.GrpDnAddrs, cc: cc,
	})
	return 887, 888, err
}

func (o *fakeReactionOps) finishRepair(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
	now uint64,
	cc *pb.ClusterConf,
) (model.FinishRepairResult, error) {
	err := o.recordRepair(reactionCall{
		op: "finish_repair", repName: repName, now: now, cc: cc,
	})
	if err != nil {
		return model.FinishRepairResult{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.finishRes, nil
}

func (o *fakeReactionOps) cancelRepair(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
	cc *pb.ClusterConf,
) (uint64, error) {
	err := o.recordRepair(reactionCall{
		op: "cancel_repair", repName: repName, cc: cc,
	})
	return 0, err
}

func (o *fakeReactionOps) setRepairAbort(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
) (uint64, error) {
	err := o.recordRepair(reactionCall{
		op: "abort_repair", repName: repName,
	})
	return 0, err
}

// The two pass steps of a repair (AR11, AR12) and the forced-start clear
// (AR13) record like every other mutation and fail with their own canned
// errors.

func (o *fakeReactionOps) setRepairStage(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
	repId uint64,
	stage pb.RepStage,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, reactionCall{
		op: "set_repair_stage", repName: repName, repId: repId,
		stage: stage,
	})
	if o.stageErr != nil {
		return o.stageErr
	}
	o.spRev++
	return nil
}

func (o *fakeReactionOps) clearForcedStart(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	ref model.ForcedStartRef,
) (bool, uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, reactionCall{
		op: "clear_forced_start", sliceId: ref.Parent.SliceId,
		cloneName: ref.Parent.CloneName, grpId: ref.GrpId, mark: ref,
	})
	if o.markErr != nil {
		return false, 0, o.markErr
	}
	if o.markSkip {
		return false, 0, nil
	}
	o.spRev++
	return true, o.spRev, nil
}

func (o *fakeReactionOps) deleteRetiredLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	legId uint64,
	cc *pb.ClusterConf,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, reactionCall{
		op: "delete_retired_leg", legIds: []uint64{legId},
		expectRev: expectRev, cc: cc,
	})
	if o.retireErr != nil {
		return o.retireErr
	}
	if expectRev != o.spRev {
		return &model.ErrPrecondition{
			Op: "DeleteRetiredLeg", Reason: model.ReasonStaleRevision,
		}
	}
	o.spRev++
	return nil
}

// The five sp-drain ops (SPD9, SPD15, SPD10, SPD12). They record like every
// other mutation — wantOps() therefore pins the drain's phase order exactly
// as it pins a reaction's — and report the harness's canned counts.

func (o *fakeReactionOps) drainSpCntlrs(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) (int, error) {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{op: "drain_cntlrs"})
	cnt, err := o.drainCntlrCnt, o.drainErr
	o.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return cnt, nil
}

func (o *fakeReactionOps) drainSpRepairs(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repNames []string,
	cc *pb.ClusterConf,
) (int, error) {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{
		op: "drain_repairs", repNames: append([]string(nil), repNames...),
		cc: cc,
	})
	cnt, err := o.drainRepCnt, o.drainErr
	o.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return cnt, nil
}

func (o *fakeReactionOps) drainSpRetiredLegs(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	legIds []uint64,
	cc *pb.ClusterConf,
) (int, error) {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{
		op: "drain_retired_legs", legIds: append([]uint64(nil), legIds...),
		cc: cc,
	})
	cnt, err := o.drainRetiredCnt, o.drainErr
	o.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return cnt, nil
}

func (o *fakeReactionOps) drainSpSlice(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	cc *pb.ClusterConf,
) (int, bool, error) {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{
		op: "drain_slice", sliceId: sliceId,
	})
	cnt, done, err := o.drainGrpCnt, o.drainDone, o.drainErr
	o.mu.Unlock()
	if err != nil {
		return 0, false, err
	}
	return cnt, done, nil
}

func (o *fakeReactionOps) finishSpDelete(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) error {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{op: "finish_delete"})
	err := o.drainErr
	o.mu.Unlock()
	return err
}

// The two clone-drain ops (CLD8/CLD9). drainCloneBm records the batch it was
// handed, so a test can pin the cut and the order rather than only the count.

func (o *fakeReactionOps) drainCloneBm(
	ctx context.Context,
	cid uint64,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	chunks []model.BmChunk,
) (int, error) {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{
		op: "drain_clone_bm", cloneName: cloneName, cloneId: cloneId,
	})
	o.cloneChunks = append(
		o.cloneChunks, append([]model.BmChunk(nil), chunks...))
	err := o.cloneDrainErr
	o.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(chunks), nil
}

func (o *fakeReactionOps) finishCloneDelete(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	cc *pb.ClusterConf,
) error {
	o.mu.Lock()
	o.calls = append(o.calls, reactionCall{
		op: "finish_clone", cloneName: cloneName, cloneId: cloneId, cc: cc,
	})
	err := o.cloneDrainErr
	if err == nil {
		o.spRev++
	}
	o.mu.Unlock()
	return err
}

// cloneBatches is the list of batches drainCloneBm was handed, in call order.
func (o *fakeReactionOps) cloneBatches() [][]model.BmChunk {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][]model.BmChunk(nil), o.cloneChunks...)
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

// reactHarness drives w.reactionPass synchronously over a fixture SpState:
// the pass is a pure function of the snapshot, the primary's CntlrInfo, the
// coordinator's own verdicts and now, so no goroutine and no clock stepping is
// needed to test it.
//
// AR10 lets a threshold fire only on an object the coordinator's own monitors
// judged unhealthy. Unless a test turns judgeStored off, every load notes a
// verdict of unhealthy for each cntlr, leg and side whose stored err_epoch is
// set, as the monitors of a coordinator that has driven the SP all along
// would have: the tests of the other rules then need no verdict of their own,
// and the AR10 tests drive the memo themselves.
type reactHarness struct {
	t     *testing.T
	logs  *logCapture
	clk   *fakeClock
	store *fakeStore
	deps  *deps
	state *model.SpState
	ops   *fakeSpOps
	rops  *fakeReactionOps
	w     *spWorker
	// judgeStored is the AR10 shortcut described above; true unless a test
	// turns it off.
	judgeStored bool
	// inSyncAll is the shortcut of AR8's gate and healthy-leg test: unless
	// a test turns it off, every pass first gives the snapshot's primary an
	// info of its own when no child of it holds one — an empty report, as
	// of a primary that has answered — and gives each primary child's info
	// a reading of each group of the state that the info does not read yet:
	// every leg of its leg_list held in sync and the array whole
	// (grp_id_to_grp_info), with a leg row OK for each of those legs that has
	// none (leg_id_to_leg), and a row OK for each group that has none
	// (grp_id_to_md_raid), as the report of a primary whose arrays run
	// whole would. The tests of the other rules then need no reading of
	// their own, and the tests of the gate, of the in-sync rule, of the
	// retire timer and of a pass with no report set theirs or turn it off.
	inSyncAll bool
}

func newReactHarness(t *testing.T, state *model.SpState) *reactHarness {
	t.Helper()
	logs := captureLogs(t)
	clk := newFakeClock()
	store := newFakeStore()
	d := newTestDeps(testConfig(common.WorkerRoleSp), store, clk)
	// As stored: the cache resolves nothing (architecture.md, Common
	// validation), and neither does this.
	setCachedConf(d, testCid, reactClusterConf())
	store.seed(t, model.SpRevKey(testShard, testCid, testSpId), &pb.SpRev{
		Revision: reactSpRevision,
		SpName:   testSpName,
	})
	w := spTestWorker(d)
	w.verdicts = newVerdictMemo()
	ops := &fakeSpOps{state: state}
	rops := &fakeReactionOps{}
	w.ops = ops
	w.react = newReactor(rops)
	h := &reactHarness{
		t: t, logs: logs, clk: clk, store: store, deps: d,
		state: state, ops: ops, rops: rops, w: w, judgeStored: true,
		inSyncAll: true,
	}
	ops.onLoad = func(loaded *model.SpState) {
		if h.judgeStored {
			judgeStoredEpochs(w.verdicts, loaded)
		}
	}
	return h
}

// roundVerdict notes one Check* round's verdict on a cntlr as its driver
// does: the round's own report for the run, then the verdict (AR10).
func roundVerdict(memo *verdictMemo, cntlrId uint64, unhealthy bool) {
	memo.noteRound(cntlrId, unhealthy)
	memo.noteCntlr(cntlrId, unhealthy)
}

// syncupVerdict notes a Syncup* reply's verdict on a cntlr, which counts for
// the latest verdict and, when clean, ends the run, but never adds to it.
func syncupVerdict(memo *verdictMemo, cntlrId uint64, unhealthy bool) {
	memo.noteCntlr(cntlrId, unhealthy)
}

// judgeStoredEpochs notes a verdict of unhealthy in memo for every cntlr, leg
// and side of state whose err_epoch is set — the legs and sides of the
// slices, of the clones' clone meta groups and the new legs of the repairs
// alike — and for a cntlr,
// failoverRun of them from rounds, the run AR5's threshold trigger needs
// (AR10).
func judgeStoredEpochs(memo *verdictMemo, state *model.SpState) {
	for cntlrId, cntlr := range state.Cntlrs {
		if cntlr.GetErrEpoch() != 0 {
			for range failoverRun {
				roundVerdict(memo, cntlrId, true)
			}
		}
	}
	var grps []*pb.Group
	for _, slice := range state.Slices {
		grps = append(grps, allGroups(slice)...)
	}
	for _, clone := range state.Clones {
		if grp := clone.GetMetaGrp(); grp != nil {
			grps = append(grps, grp)
		}
	}
	lists := make([][]*pb.Leg, 0, len(grps)+len(state.Repairs))
	for _, grp := range grps {
		lists = append(lists, grp.GetLegList())
	}
	for _, rep := range state.Repairs {
		lists = append(lists, rep.GetNewLegList())
	}
	for _, list := range lists {
		for _, leg := range list {
			if leg.GetErrEpoch() != 0 {
				memo.noteLeg(leg.GetLegId(), true)
			}
			for _, side := range leg.GetSideList() {
				if side.GetErrEpoch() != 0 {
					memo.noteSide(sideKey{
						legId:  leg.GetLegId(),
						sideId: side.GetSideId(),
					}, true)
				}
			}
		}
	}
}

// now is the pass's "now" — the fake clock's unix seconds.
func (h *reactHarness) now() uint64 {
	return h.clk.nowUnix()
}

// ago is an err_epoch that is seconds old at the current fake now.
func (h *reactHarness) ago(seconds uint64) uint64 {
	return h.now() - seconds
}

// pass runs one reaction pass, the fake ops' SpRev at the snapshot's
// revision.
func (h *reactHarness) pass() {
	h.t.Helper()
	if h.inSyncAll {
		h.fillGrpInfo()
	}
	h.ops.mu.Lock()
	if h.ops.state != nil {
		h.rops.mu.Lock()
		h.rops.spRev = h.ops.state.SpRevision
		h.rops.mu.Unlock()
	}
	h.ops.mu.Unlock()
	h.w.reactionPass(context.Background())
}

// fillGrpInfo is inSyncAll's reading: the snapshot's primary gets an empty
// info when it has no child, and on every primary child, each group of the
// state the pass loads that the info holds no reading of reads whole with
// every leg_list leg in sync, each of those legs with no row of its own
// reading OK (leg_id_to_leg), and each group with no row reads OK.
func (h *reactHarness) fillGrpInfo() {
	h.ops.mu.Lock()
	state := h.ops.state
	h.ops.mu.Unlock()
	if state == nil {
		return
	}
	for _, cntlrId := range sortedIds(state.Conf.GetCntlrIdList()) {
		if !state.Cntlrs[cntlrId].GetPrimary() {
			continue
		}
		if _, ok := h.w.cntlrs[cntlrId]; !ok {
			h.setPrimaryInfo(cntlrId, &pb.CntlrInfo{})
		}
		break
	}
	for _, child := range h.w.cntlrs {
		if child.plan == nil || !child.plan.primary || child.driver == nil {
			continue
		}
		info := child.driver.lastInfo
		if info == nil {
			continue
		}
		if info.GrpIdToGrpInfo == nil {
			info.GrpIdToGrpInfo = map[uint64]*pb.CntlrInfo_GrpInfo{}
		}
		if info.GrpIdToMdRaid == nil {
			info.GrpIdToMdRaid = map[uint64]*pb.ResInfo{}
		}
		for _, grp := range judgedGrps(state) {
			if _, ok := info.GrpIdToMdRaid[grp.GetGrpId()]; !ok {
				info.GrpIdToMdRaid[grp.GetGrpId()] = resOk("md")
			}
			if _, ok := info.GrpIdToGrpInfo[grp.GetGrpId()]; ok {
				continue
			}
			reading := &pb.CntlrInfo_GrpInfo{Whole: true}
			for _, leg := range grp.GetLegList() {
				reading.InSyncLegIdList = append(
					reading.InSyncLegIdList, leg.GetLegId())
				if info.LegIdToLeg == nil {
					info.LegIdToLeg = map[uint64]*pb.ResInfo{}
				}
				if _, ok := info.LegIdToLeg[leg.GetLegId()]; !ok {
					info.LegIdToLeg[leg.GetLegId()] = resOk("leg")
				}
			}
			info.GrpIdToGrpInfo[grp.GetGrpId()] = reading
		}
	}
}

// setGrpInfo installs one group's reading (grp_id_to_grp_info) on the
// primary: whether its array is whole and the legs md holds in sync.
func (h *reactHarness) setGrpInfo(grpId uint64, whole bool, inSync ...uint64) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.GrpIdToGrpInfo == nil {
		info.GrpIdToGrpInfo = map[uint64]*pb.CntlrInfo_GrpInfo{}
	}
	info.GrpIdToGrpInfo[grpId] = &pb.CntlrInfo_GrpInfo{
		Whole: whole, InSyncLegIdList: inSync,
	}
}

// setPrimaryInfo installs the CntlrInfo a cntlr's child last reported (AR1),
// on a child that drives the primary plan: the pass reads no other child's.
func (h *reactHarness) setPrimaryInfo(cntlrId uint64, info *pb.CntlrInfo) {
	h.w.cntlrs[cntlrId] = &cntlrChild{
		plan:   &cntlrPlan{cntlrId: cntlrId, primary: true},
		driver: &cntlrDriver{lastInfo: info},
	}
}

// setPool installs one slice's thin-pool row on the primary.
func (h *reactHarness) setPool(sliceId uint64, status pb.ResStatus, details string) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.SliceIdToDmPool == nil {
		info.SliceIdToDmPool = map[uint64]*pb.ResInfo{}
	}
	info.SliceIdToDmPool[sliceId] = &pb.ResInfo{
		ResName: "pool", Status: status, Details: details,
	}
}

// setGrpRow installs one group's row (grp_id_to_md_raid) on the primary.
// AR8's gate needs it OK before a create, the one status that says the
// group's array runs, whole or degraded (cnagent.md CN28).
func (h *reactHarness) setGrpRow(grpId uint64, status pb.ResStatus) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.GrpIdToMdRaid == nil {
		info.GrpIdToMdRaid = map[uint64]*pb.ResInfo{}
	}
	info.GrpIdToMdRaid[grpId] = &pb.ResInfo{ResName: "md", Status: status}
}

// slice is the fixture's one slice.
func (h *reactHarness) slice() *pb.Slice {
	return h.state.Slices[reactSliceId]
}

// dataGrp / metaGrp are the fixture's two groups.
func (h *reactHarness) dataGrp() *pb.Group {
	return h.slice().GetDataGrpList()[0]
}

func (h *reactHarness) metaGrp() *pb.Group {
	return h.slice().GetMetaGrpList()[0]
}

// legOf finds a leg of the fixture by id: in the leg_list of either group of
// the slice, or among the new legs of the state's repairs.
func (h *reactHarness) legOf(legId uint64) *pb.Leg {
	h.t.Helper()
	lists := [][]*pb.Leg{}
	for _, grp := range allGroups(h.slice()) {
		lists = append(lists, grp.GetLegList())
	}
	for _, rep := range h.state.Repairs {
		lists = append(lists, rep.GetNewLegList())
	}
	for _, list := range lists {
		for _, leg := range list {
			if leg.GetLegId() == legId {
				return leg
			}
		}
	}
	h.t.Fatalf("leg %d not in the fixture", legId)
	return nil
}

// dnCands hands the fake allocator a set of DN candidates.
func (h *reactHarness) dnCands(addrs ...string) {
	var cands []model.Cand
	for idx, addr := range addrs {
		cands = append(cands, model.Cand{
			AddrPort: addr,
			Location: fmt.Sprintf("rack%d", idx),
			FreeExt:  64,
		})
	}
	h.rops.dnCands = cands
}

// cnCands hands the fake allocator a set of CN candidates.
func (h *reactHarness) cnCands(addrs ...string) {
	var cands []model.Cand
	for idx, addr := range addrs {
		cands = append(cands, model.Cand{
			AddrPort: addr,
			Location: fmt.Sprintf("rack%d", idx),
			FreeExt:  64,
		})
	}
	h.rops.cnCands = cands
}

// ops assertions ------------------------------------------------------------

// wantOps asserts the exact sequence of mutations the passes ran.
func (h *reactHarness) wantOps(want ...string) []reactionCall {
	h.t.Helper()
	calls := h.rops.allCalls()
	got := make([]string, 0, len(calls))
	for _, call := range calls {
		got = append(got, call.op)
	}
	if len(got) != len(want) {
		h.t.Fatalf("ops = %v, want %v", got, want)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			h.t.Fatalf("ops = %v, want %v", got, want)
		}
	}
	return calls
}

// wantApplied asserts the kinds of the `reaction applied` records
// (dnv-worker.md, Log records).
func (h *reactHarness) wantApplied(want ...string) []map[string]any {
	h.t.Helper()
	recs := h.logs.withMsg(msgReactionApplied)
	if len(recs) != len(want) {
		h.t.Fatalf("applied = %v, want %v", kindsOf(recs), want)
	}
	for idx, kind := range want {
		if recs[idx]["kind"] != kind {
			h.t.Fatalf("applied = %v, want %v", kindsOf(recs), want)
		}
	}
	return recs
}

// wantSkipped asserts one `reaction skipped` record with that kind and reason.
func (h *reactHarness) wantSkipped(kind string, reason string) {
	h.t.Helper()
	for _, rec := range h.logs.withMsg(msgReactionSkipped) {
		if rec["kind"] == kind && rec["reason"] == reason {
			return
		}
	}
	h.t.Fatalf(
		"no `reaction skipped` kind=%s reason=%s; got %v",
		kind, reason, h.logs.withMsg(msgReactionSkipped),
	)
}

// wantSkippedBy asserts one `reaction skipped` record with that kind and
// reason whose op attribute names op, the model op that refused inside its
// transaction, or, op empty, that has none: the pass's own check refused.
func (h *reactHarness) wantSkippedBy(kind string, reason string, op string) {
	h.t.Helper()
	for _, rec := range h.logs.withMsg(msgReactionSkipped) {
		if rec["kind"] != kind || rec["reason"] != reason {
			continue
		}
		got, has := rec["op"]
		if (op == "" && !has) || (op != "" && got == op) {
			return
		}
	}
	h.t.Fatalf(
		"no `reaction skipped` kind=%s reason=%s op=%q; got %v",
		kind, reason, op, h.logs.withMsg(msgReactionSkipped),
	)
}

// wantNoSkip asserts that no `reaction skipped` record was emitted at all.
func (h *reactHarness) wantNoSkip() {
	h.t.Helper()
	if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 0 {
		h.t.Fatalf("unexpected `reaction skipped`: %v", recs)
	}
}

func kindsOf(recs []map[string]any) []any {
	out := make([]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec["kind"])
	}
	return out
}

// poolLine renders a realistic `dmsetup status` thin-pool line.
func poolLine(usedMeta, totalMeta, usedData, totalData uint64) string {
	return fmt.Sprintf(
		"0 20971520 thin-pool 4 %d/%d %d/%d - rw discard_passdown "+
			"queue_if_no_space - 1024",
		usedMeta, totalMeta, usedData, totalData,
	)
}

// ---------------------------------------------------------------------------
// AR2 — priority and one action per pass
// ---------------------------------------------------------------------------

// TestReactionPriority pins AR2: with all four triggers armed at once, each
// pass runs exactly ONE action, and the one it runs is the highest-priority
// applicable reaction.
func TestReactionPriority(t *testing.T) {
	// A pool far over the watermark, an unhealthy standby and an unhealthy
	// leg are armed in every case; only the failover trigger is varied.
	armed := func(h *reactHarness) {
		total := reactDataBlocks(t, 2)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, total, total))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(1300)
		h.dnCands(reactDnC, reactDnD)
		h.cnCands(reactCnC)
	}

	t.Run("failover first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		// The standby has to be healthy for AR5 to have a candidate at all.
		h.state.Cntlrs[reactCntlrB].ErrEpoch = 0
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.pass()
		calls := h.wantOps("failover")
		if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
			t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
		}
		recs := h.wantApplied(reactionFailover)
		if recs[0]["revision"] != float64(reactSpRevision) {
			t.Fatalf("revision = %v", recs[0]["revision"])
		}
	})

	t.Run("grow second", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
	})

	t.Run("replace third", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, 1, 1000))
		h.pass()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrB || calls[0].asPrimary {
			t.Fatalf("replace = %+v", calls[0])
		}
		h.wantApplied(reactionReplaceCntlr)
	})

	t.Run("leg repair last", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, 1, 1000))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = 0
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("repair of group %d", calls[0].grpId)
		}
		h.wantApplied(reactionRepairCreate)
	})
}

// TestReactionRunsOnTheCoordinatorTick pins AR1's cadence wiring: the pass is
// what the sp coordinator's cntlr_interval tick runs (RW20), not something a
// caller has to drive by hand.
func TestReactionRunsOnTheCoordinatorTick(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	h.w.tick()
	h.wantOps("failover")
	h.wantApplied(reactionFailover)
}

// TestReactionQuietPassDoesNothing checks that a healthy SP produces neither a
// mutation nor a record: a pass is not a heartbeat.
func TestReactionQuietPassDoesNothing(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.pass()
	h.wantOps()
	h.wantApplied()
	h.wantNoSkip()
}

// ---------------------------------------------------------------------------
// AR3 — suppression
// ---------------------------------------------------------------------------

// TestReactionSuppressed pins AR3's surviving half: nothing runs at
// sp_level >= SP_LEVEL_NO_THINPOOL, and the record is emitted once per
// transition rather than once per pass.
//
// `deleting` is deliberately not a row here: SPD6 keeps it out of AR3, so a
// latched SP runs the drain instead of nothing at all, at ANY sp_level, and
// TestDrainRunsAtEverySpLevel in drain_test.go is where that lives.
func TestReactionSuppressed(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(conf *pb.SpConf)
		suppressed bool
	}{
		{"no_thinpool", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		}, true},
		{"no_migration", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_NO_MIGRATION
		}, true},
		{"readonly", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_READONLY
		}, false},
		{"no_clone", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_NO_CLONE
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			tc.mutate(h.state.Conf)
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
			h.pass()
			h.pass()
			if tc.suppressed {
				h.wantOps()
				h.wantApplied()
				if got := len(
					h.logs.withMsg(msgReactionSuppressed),
				); got != 1 {
					t.Fatalf("suppressed records = %d, want 1", got)
				}
				return
			}
			h.wantOps("failover", "failover")
			if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 0 {
				t.Fatalf("suppressed records = %d, want 0", got)
			}
		})
	}
}

// TestReactionDisabledCntlrIsHandsOff pins the other half of AR3: a disabled
// cntlr is never failed over TO and never replaced.
func TestReactionDisabledCntlrIsHandsOff(t *testing.T) {
	t.Run("never failed over to", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.cnCands(reactCnC)
		h.pass()
		// AR5 has no candidate; AR7 does not fire either, because the
		// primary has not been unhealthy for cntlr_unhealthy yet.
		h.wantOps()
		h.wantSkipped(reactionFailover, reasonNoCandidate)
	})

	t.Run("never replaced", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.cnCands(reactCnC)
		h.pass()
		h.wantOps()
		h.wantApplied()
	})
}

// ---------------------------------------------------------------------------
// AR5 — the failover triggers
// ---------------------------------------------------------------------------

// TestReactionDisabledPrimaryFailsOver pins AR5's second trigger: `disabled`
// on the PRIMARY fires on its own and immediately (architecture.md, Cntlrs —
// "disabling the current primary triggers the primary re-election of Automatic
// reactions"), with no threshold to wait out. AR3's hands-off rule is unchanged
// in the other direction: a disabled cntlr is still never a candidate.
func TestReactionDisabledPrimaryFailsOver(t *testing.T) {
	t.Run("healthy disabled primary", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrA].Disabled = true
		h.pass()
		calls := h.wantOps("failover")
		if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
			t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
		}
		h.wantApplied(reactionFailover)
		h.wantNoSkip()
		for _, cntlrId := range sortedIds(h.state.Conf.GetCntlrIdList()) {
			if got := h.state.Cntlrs[cntlrId].GetErrEpoch(); got != 0 {
				t.Fatalf(
					"cntlr %d err_epoch = %d; the trigger is the disabled "+
						"flag alone", cntlrId, got,
				)
			}
		}
	})

	// The no-candidate skip still does not end the pass (AR2's continue
	// list): with every other cntlr ineligible — disabled, or unhealthy —
	// the disabled primary stays put and the next reaction runs.
	for _, tc := range []struct {
		name   string
		mutate func(h *reactHarness, cntlr *pb.Cntlr)
	}{
		{"other disabled", func(h *reactHarness, cntlr *pb.Cntlr) {
			cntlr.Disabled = true
		}},
		{"other unhealthy", func(h *reactHarness, cntlr *pb.Cntlr) {
			cntlr.ErrEpoch = h.ago(10)
		}},
	} {
		t.Run("no candidate, "+tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.state.Cntlrs[reactCntlrA].Disabled = true
			tc.mutate(h, h.state.Cntlrs[reactCntlrB])
			// AR6 is armed so that the continuation is observable.
			total := reactDataBlocks(t, 2)
			h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
				poolLine(1, 1000, total, total))
			h.dnCands(reactDnC, reactDnD)
			h.pass()
			h.wantSkipped(reactionFailover, reasonNoCandidate)
			h.wantOps("grow")
			h.wantApplied(reactionGrowData)
		})
	}
}

// TestReactionSettlingPrimary pins AR5's threshold selection (HL2): a
// SETTLING primary — one that has not yet reported its stack built and clean
// as primary since it acquired the role — is held to cntlr_unhealthy instead
// of primary_unhealthy when that is the longer; a settled one is judged as
// before; and the disabled trigger ignores the flag. The fixture's thresholds
// are the product defaults (DefaultPrimaryUnhealthy, DefaultCntlrUnhealthy),
// so the two readings cannot coincide; the last case inverts them.
func TestReactionSettlingPrimary(t *testing.T) {
	t.Run("settling, primary_unhealthy reached", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		primary := h.state.Cntlrs[reactCntlrA]
		primary.Settling = true
		primary.ErrEpoch = h.ago(common.DefaultPrimaryUnhealthy)
		// AR6 is armed so that the pass going on is observable.
		total := reactDataBlocks(t, 2)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, total, total))
		h.dnCands(reactDnC, reactDnD)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
		h.wantNoSkip()
	})

	t.Run("settling, just short of cntlr_unhealthy", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		primary := h.state.Cntlrs[reactCntlrA]
		primary.Settling = true
		primary.ErrEpoch = h.ago(common.DefaultCntlrUnhealthy - 1)
		h.pass()
		h.wantOps()
		h.wantApplied()
		h.wantNoSkip()
	})

	t.Run("settling, cntlr_unhealthy reached", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		primary := h.state.Cntlrs[reactCntlrA]
		primary.Settling = true
		primary.ErrEpoch = h.ago(common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("failover")
		if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
			t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
		}
		h.wantApplied(reactionFailover)
	})

	t.Run("settled, primary_unhealthy reached", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultPrimaryUnhealthy)
		h.pass()
		h.wantOps("failover")
		h.wantApplied(reactionFailover)
	})

	t.Run("disabled settling primary", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		primary := h.state.Cntlrs[reactCntlrA]
		primary.Settling = true
		primary.Disabled = true
		h.pass()
		h.wantOps("failover")
		h.wantApplied(reactionFailover)
		h.wantNoSkip()
	})

	// Nothing orders the two thresholds (AR4): where cntlr_unhealthy is the
	// SHORTER, a settling primary is still held to primary_unhealthy — the
	// hold never shortens the wait.
	t.Run("settling, cntlr_unhealthy the shorter", func(t *testing.T) {
		const primaryUnhealthy, cntlrUnhealthy = 60, 10
		for _, tc := range []struct {
			age  uint64
			want []string
		}{
			{cntlrUnhealthy, nil},
			{primaryUnhealthy - 1, nil},
			{primaryUnhealthy, []string{"failover"}},
		} {
			h := newReactHarness(t, reactFixture(t))
			h.state.Conf.EventThreshold = &pb.EventThreshold{
				PrimaryUnhealthy: primaryUnhealthy,
				CntlrUnhealthy:   cntlrUnhealthy,
			}
			primary := h.state.Cntlrs[reactCntlrA]
			primary.Settling = true
			primary.ErrEpoch = h.ago(tc.age)
			h.pass()
			h.wantOps(tc.want...)
		}
	})
}

// The shared-state fixture's ids (sharedStateFixture).
const (
	reactLostTd     = uint64(500)
	reactOtherTd    = uint64(501)
	reactLostNs     = uint64(800)
	reactOtherNs    = uint64(801)
	reactLostXfer   = uint64(900)
	reactOtherXfer  = uint64(901)
	reactLostClone  = uint64(950)
	reactOtherClone = uint64(951)
	reactCntlrC     = uint64(3)
	reactNqn        = "nqn.2024-01.io.dnv:react"
)

// sharedStateFixture is reactFixture with two created tds whose pool is the
// fixture's slice: reactLostTd, whose thin id the pool no longer holds, and
// reactOtherTd, which the pool still holds, each with a namespace, a transfer
// out of that namespace and a clone onto the td.
func sharedStateFixture(t *testing.T) *model.SpState {
	t.Helper()
	state := reactFixture(t)
	state.Conf.TdNameList = []string{"td0", "td1"}
	state.Tds = []*pb.ThinDevice{
		{TdId: reactLostTd, DevId: 1, Created: true, Size: reactTdSize},
		{TdId: reactOtherTd, DevId: 2, Created: true, Size: reactTdSize},
	}
	state.TdNames = []string{"td0", "td1"}
	state.Conf.NqnList = []string{reactNqn}
	state.Subsystems = map[string]*pb.Subsystem{reactNqn: {
		SsId: 600,
		NsList: []*pb.Namespace{
			{NsId: reactLostNs, NsIdx: 1, TdId: reactLostTd},
			{NsId: reactOtherNs, NsIdx: 2, TdId: reactOtherTd},
		},
	}}
	state.Conf.XferNameList = []string{"xfer0", "xfer1"}
	state.Xfers = map[string]*pb.Transfer{
		"xfer0": {XferId: reactLostXfer, OriNqn: reactNqn, OriNsIdx: 1},
		"xfer1": {XferId: reactOtherXfer, OriNqn: reactNqn, OriNsIdx: 2},
	}
	state.Conf.CloneNameList = []string{"clone0", "clone1"}
	state.Clones = map[string]*pb.Clone{
		"clone0": {CloneId: reactLostClone, DstTdId: reactLostTd},
		"clone1": {CloneId: reactOtherClone, DstTdId: reactOtherTd},
	}
	return state
}

// lostStackInfo is the primary's report once the pool has lost reactLostTd's
// thin id. With converge it is the converge's: the thin row carries the
// reason thin id lost — the `dmsetup create` of its thin table failed with
// ENODATA — and every other row of the td's stack fails with it: its raid0
// and dm-error, its namespace's ns-dev and nvmet namespace, the transfer out
// of that namespace and the clone onto the td. Otherwise it is a background
// read's, which the rounds after that read answer from (dnagent.md SH29): its
// probes read the absent volume MISSING, with no reason. Everything else is
// OK.
func lostStackInfo(converge bool) *pb.CntlrInfo {
	thin := &pb.ResInfo{
		ResName: "thin", Status: pb.ResStatus_RES_STATUS_MISSING,
	}
	if converge {
		thin = resLostId("thin", "dmsetup create thin --table 0 262144 thin "+
			"253:7 1: device-mapper: reload ioctl on thin  failed: "+
			"No data available")
	}
	lost := func(name string) map[uint64]*pb.ResInfo {
		return map[uint64]*pb.ResInfo{reactLostXfer: resErr(name, "x")}
	}
	return &pb.CntlrInfo{
		SsIdToSubsystem: map[uint64]*pb.ResInfo{600: resOk("ss")},
		NsIdToNamespace: map[uint64]*pb.ResInfo{
			reactLostNs:  resErr("ns", "namespace not enabled"),
			reactOtherNs: resOk("ns2"),
		},
		NsIdToDmLinear: map[uint64]*pb.ResInfo{
			reactLostNs:  resErr("nsdev", "lsblk raid0: not a block device"),
			reactOtherNs: resOk("nsdev2"),
		},
		TdIdToRaid0: map[uint64]*pb.ResInfo{
			reactLostTd:  resErr("raid0", "lsblk thin: not a block device"),
			reactOtherTd: resOk("raid0-2"),
		},
		TdIdToDmError: map[uint64]*pb.ResInfo{
			reactLostTd:  resErr("error", "x"),
			reactOtherTd: resOk("error2"),
		},
		XferIdToDmLinear:  lost("xfer-dev"),
		XferIdToSubsystem: lost("xfer-ss"),
		XferIdToNamespace: lost("xfer-ns"),
		CloneIdToTarget: map[uint64]*pb.ResInfo{
			reactLostClone: resErr("clone-src", "x"),
		},
		CloneIdToDmClone: map[uint64]*pb.ResInfo{
			reactLostClone: resErr("clone", "x"),
		},
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			reactLostTd: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				reactSliceId: thin,
			}},
			reactOtherTd: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				reactSliceId: resOk("thin2"),
			}},
		},
	}
}

// TestReactionSharedStateErrorIsNotATrigger pins AR5's first refusal (HL2's
// row classes): an unhealthy primary whose report fails only in the stack of
// a created td whose thin id the pool no longer holds is not failed over —
// every cntlr that takes the role reads the same rows from the same pool —
// and the pass records why and goes on. The lost id is the thin row's reason,
// whatever its details say. The refusal is judged ahead of the candidate, so
// a primary with none logs it rather than `no candidate`, and after the
// threshold, so a primary short of it logs nothing. Each other case fails it
// over: an ERROR row of anything else beside the stack — another td's raid0,
// namespace, thin row, transfer or clone among them — a thin row failing for
// any other reason, the lost id's text in a row with no reason, the reason on
// a row that does not read ERROR, an uncreated td, a probe's report, which
// carries no reason, a primary read unreachable after the converge's report,
// its rows UNKNOWN with their details and reason kept, and a disabled
// primary, whose trigger is the operator's.
func TestReactionSharedStateErrorIsNotATrigger(t *testing.T) {
	// armGrow arms AR6 so that the pass going on is observable.
	armGrow := func(t *testing.T, h *reactHarness) {
		t.Helper()
		total := reactDataBlocks(t, 2)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, total, total))
		h.dnCands(reactDnC, reactDnD)
	}

	t.Run("the lost td's stack alone", func(t *testing.T) {
		h := newReactHarness(t, sharedStateFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultPrimaryUnhealthy)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		armGrow(t, h)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
		h.wantSkipped(reactionFailover, "shared_state")
		rec := h.logs.withMsg(msgReactionSkipped)[0]
		for attr, want := range map[string]uint64{
			"cntlr_id": reactCntlrA,
			"td_id":    reactLostTd,
		} {
			if got, _ := rec[attr].(float64); uint64(got) != want {
				t.Fatalf("%s = %v, want %d", attr, rec[attr], want)
			}
		}
	})

	t.Run("no candidate", func(t *testing.T) {
		h := newReactHarness(t, sharedStateFixture(t))
		// The one other cntlr cannot take the role.
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultPrimaryUnhealthy)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		armGrow(t, h)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 1 || recs[0]["kind"] != reactionFailover ||
			recs[0]["reason"] != "shared_state" {
			t.Fatalf("skips = %v, want failover/shared_state alone", recs)
		}
	})

	t.Run("below the threshold", func(t *testing.T) {
		h := newReactHarness(t, sharedStateFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultPrimaryUnhealthy - 1)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		h.pass()
		h.wantOps()
		if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 0 {
			t.Fatalf("skips below the threshold = %v, want none", recs)
		}
	})

	// The class reads the row's reason and nothing in its details: the
	// reason with any text is the lost id, the case below without it is not.
	t.Run("the reason with other details", func(t *testing.T) {
		h := newReactHarness(t, sharedStateFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultPrimaryUnhealthy)
		info := lostStackInfo(true)
		info.TdIdToThinInfo[reactLostTd].SliceIdToDmThin[reactSliceId] =
			resLostId("thin", "x")
		h.setPrimaryInfo(reactCntlrA, info)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionFailover, "shared_state")
	})

	for _, tc := range []struct {
		name   string
		mutate func(h *reactHarness, info *pb.CntlrInfo)
	}{
		{"an own row beside it", func(_ *reactHarness, info *pb.CntlrInfo) {
			info.SsIdToSubsystem[600] = resErr("ss", "not linked to the port")
		}},
		{"another td's raid0", func(_ *reactHarness, info *pb.CntlrInfo) {
			info.TdIdToRaid0[reactOtherTd] = resErr("raid0-2", "x")
		}},
		{"another td's namespace", func(_ *reactHarness, info *pb.CntlrInfo) {
			info.NsIdToDmLinear[reactOtherNs] = resErr("nsdev2", "x")
		}},
		{"another td's thin row", func(_ *reactHarness, info *pb.CntlrInfo) {
			info.TdIdToThinInfo[reactOtherTd].SliceIdToDmThin[reactSliceId] =
				resErr("thin2", "x")
		}},
		{"a transfer out of another td's namespace", func(
			_ *reactHarness, info *pb.CntlrInfo,
		) {
			info.XferIdToDmLinear[reactOtherXfer] = resErr("xfer2-dev", "x")
		}},
		{"a clone onto another td", func(_ *reactHarness, info *pb.CntlrInfo) {
			info.CloneIdToDmClone[reactOtherClone] = resErr("clone2", "x")
		}},
		{"a thin row failing otherwise", func(
			_ *reactHarness, info *pb.CntlrInfo,
		) {
			info.TdIdToThinInfo[reactLostTd].SliceIdToDmThin[reactSliceId] =
				resErr("thin", "dmsetup info thin: signal: killed")
		}},
		{"the lost id's text with no reason", func(
			_ *reactHarness, info *pb.CntlrInfo,
		) {
			// The converge's own row, ENODATA text and all, without the
			// field: the class never parses details.
			info.TdIdToThinInfo[reactLostTd].SliceIdToDmThin[reactSliceId].
				Reason = pb.ResReason_RES_REASON_NONE
		}},
		{"the reason on a row that is not ERROR", func(
			_ *reactHarness, info *pb.CntlrInfo,
		) {
			info.TdIdToThinInfo[reactLostTd].SliceIdToDmThin[reactSliceId].
				Status = pb.ResStatus_RES_STATUS_MISSING
		}},
		{"an uncreated td", func(h *reactHarness, _ *pb.CntlrInfo) {
			h.state.Tds[0].Created = false
		}},
		{"a probe's report", func(_ *reactHarness, info *pb.CntlrInfo) {
			info.TdIdToThinInfo[reactLostTd].SliceIdToDmThin[reactSliceId] =
				lostStackInfo(false).TdIdToThinInfo[reactLostTd].
					SliceIdToDmThin[reactSliceId]
		}},
		{"an unreachable primary", func(_ *reactHarness, info *pb.CntlrInfo) {
			// The stream died after the converge's report carried the
			// reason: every row reads UNKNOWN and keeps its details and its
			// reason (markCntlrUnknown).
			markCntlrUnknown(info)
		}},
		{"a disabled primary", func(h *reactHarness, _ *pb.CntlrInfo) {
			h.state.Cntlrs[reactCntlrA].Disabled = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, sharedStateFixture(t))
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
				common.DefaultPrimaryUnhealthy)
			info := lostStackInfo(true)
			tc.mutate(h, info)
			h.setPrimaryInfo(reactCntlrA, info)
			h.pass()
			calls := h.wantOps("failover")
			if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
				t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
			}
			h.wantApplied(reactionFailover)
		})
	}
}

// TestReactionRoleNotHandedBackOverTheSameError pins AR5's second refusal: a
// failover on a probe's report of the lost td — no refusal of the first kind,
// the report carrying no reason — hands the role to a peer that fails on the
// same rows, and the role is not handed back to the cntlr that lost it while
// the primary fails only on rows that cntlr failed on, from an error set
// within cntlr_unhealthy of that failover: the error is presumed to have
// followed the role. The role moves on anything else — rows the old primary
// did not fail on, beside its rows or instead of them, a report with no ERROR
// row, a later error — and to another candidate, and at once when the
// primary is disabled; a hand-back over rows of the primary's own records
// them in turn.
func TestReactionRoleNotHandedBackOverTheSameError(t *testing.T) {
	// failedOver runs the pass that fails A over to B and commits it the way
	// model.Failover does: A a clean standby, B primary and settling, B's
	// report rows, its err_epoch the failover's second.
	failedOver := func(
		t *testing.T,
		state *model.SpState,
		rows *pb.CntlrInfo,
	) *reactHarness {
		t.Helper()
		h := newReactHarness(t, state)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultPrimaryUnhealthy)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(false))
		h.pass()
		h.wantOps("failover")
		a, b := h.state.Cntlrs[reactCntlrA], h.state.Cntlrs[reactCntlrB]
		a.Primary, a.ErrEpoch = false, 0
		b.Primary, b.Settling, b.ErrEpoch = true, true, h.now()
		h.setPrimaryInfo(reactCntlrB, rows)
		return h
	}
	advance := func(h *reactHarness, seconds uint64) {
		h.clk.advance(time.Duration(seconds) * time.Second)
	}

	t.Run("the same rows", func(t *testing.T) {
		h := failedOver(t, sharedStateFixture(t), lostStackInfo(false))
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		h.pass()
		h.wantOps("failover")
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 2 {
			t.Fatalf("skips = %v, want one per pass", recs)
		}
		for _, rec := range recs {
			if rec["kind"] != reactionFailover || rec["reason"] != "same_error" {
				t.Fatalf("skip = %v, want failover/same_error", rec)
			}
			old, _ := rec["old_cntlr_id"].(float64)
			cand, _ := rec["new_cntlr_id"].(float64)
			if uint64(old) != reactCntlrB || uint64(cand) != reactCntlrA {
				t.Fatalf("skip = %v, want %d -> %d refused", rec,
					reactCntlrB, reactCntlrA)
			}
		}
	})

	t.Run("other rows", func(t *testing.T) {
		rows := &pb.CntlrInfo{SsIdToSubsystem: map[uint64]*pb.ResInfo{
			600: resErr("ss", "not linked to the port"),
		}}
		h := failedOver(t, sharedStateFixture(t), rows)
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("failover", "failover")
		if calls[1].oldId != reactCntlrB || calls[1].newId != reactCntlrA {
			t.Fatalf("failover %d -> %d", calls[1].oldId, calls[1].newId)
		}
	})

	// The same rows and one of the new primary's own: the role goes back, and
	// that failover's record refuses the next hand-back over the lost td's
	// rows alone, so the role moves twice and then stays.
	t.Run("the same rows and an own one", func(t *testing.T) {
		rows := lostStackInfo(false)
		rows.SsIdToSubsystem[600] = resErr("ss", "not linked to the port")
		h := failedOver(t, sharedStateFixture(t), rows)
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("failover", "failover")
		if calls[1].oldId != reactCntlrB || calls[1].newId != reactCntlrA {
			t.Fatalf("failover %d -> %d", calls[1].oldId, calls[1].newId)
		}
		a, b := h.state.Cntlrs[reactCntlrA], h.state.Cntlrs[reactCntlrB]
		b.Primary, b.Settling, b.ErrEpoch = false, false, 0
		a.Primary, a.Settling, a.ErrEpoch = true, true, h.now()
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(false))
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		h.wantOps("failover", "failover")
		h.wantSkipped(reactionFailover, "same_error")
	})

	// A primary read unreachable has every row UNKNOWN: no row is one the
	// old primary failed on, and the role goes back.
	t.Run("no ERROR row", func(t *testing.T) {
		rows := lostStackInfo(false)
		markCntlrUnknown(rows)
		h := failedOver(t, sharedStateFixture(t), rows)
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("failover", "failover")
		if calls[1].oldId != reactCntlrB || calls[1].newId != reactCntlrA {
			t.Fatalf("failover %d -> %d", calls[1].oldId, calls[1].newId)
		}
	})

	// A new primary that settled and then fails on the same rows is held
	// while its error was set within cntlr_unhealthy of the failover, and
	// judged as any settled primary once it is set later.
	for _, tc := range []struct {
		after uint64
		want  []string
	}{
		{common.DefaultCntlrUnhealthy - 1, []string{"failover"}},
		{common.DefaultCntlrUnhealthy, []string{"failover", "failover"}},
	} {
		t.Run(fmt.Sprintf("settled, failing %d s after", tc.after),
			func(t *testing.T) {
				h := failedOver(t, sharedStateFixture(t), lostStackInfo(false))
				b := h.state.Cntlrs[reactCntlrB]
				advance(h, tc.after)
				b.Settling, b.ErrEpoch = false, h.now()
				advance(h, common.DefaultPrimaryUnhealthy)
				h.pass()
				h.wantOps(tc.want...)
			})
	}

	t.Run("another candidate", func(t *testing.T) {
		state := sharedStateFixture(t)
		state.Conf.CntlrIdList = append(state.Conf.CntlrIdList, reactCntlrC)
		state.Cntlrs[reactCntlrC] = &pb.Cntlr{AddrPort: reactCnC}
		h := failedOver(t, state, lostStackInfo(false))
		// The operator has disabled the cntlr that lost the role.
		h.state.Cntlrs[reactCntlrA].Disabled = true
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("failover", "failover")
		if calls[1].oldId != reactCntlrB || calls[1].newId != reactCntlrC {
			t.Fatalf("failover %d -> %d", calls[1].oldId, calls[1].newId)
		}
	})

	t.Run("a disabled primary", func(t *testing.T) {
		h := failedOver(t, sharedStateFixture(t), lostStackInfo(false))
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.pass()
		calls := h.wantOps("failover", "failover")
		if calls[1].oldId != reactCntlrB || calls[1].newId != reactCntlrA {
			t.Fatalf("failover %d -> %d", calls[1].oldId, calls[1].newId)
		}
	})
}

// loadHookOps runs a hook inside the next loadSp, ahead of the load itself: a
// write a child makes while a pass is between reading the children's write
// counts and loading the SP.
type loadHookOps struct {
	*fakeSpOps
	hook func()
}

func (o *loadHookOps) loadSp(
	ctx context.Context,
	cid uint64,
	spName string,
) (*model.SpState, error) {
	if hook := o.hook; hook != nil {
		o.hook = nil
		hook()
	}
	return o.fakeSpOps.loadSp(ctx, cid, spName)
}

// TestReactionPassReseedsHealthEveryLoad pins the pass's half of HL3's
// re-seed; TestHealthOfferPredatingOwnWriteIsDropped pins the monitor's. Every
// pass offers each side and cntlr child — a cntlr child here — the err_epoch
// its load read, and it reads the child's count of its own writes BEFORE that
// load, so that a write the child makes in between voids the offer: a pass
// that skipped the offer, or read the count after its load, would leave a
// stamp in place or fold a stale record in.
func TestReactionPassReseedsHealthEveryLoad(t *testing.T) {
	ctx := context.Background()
	h := newReactHarness(t, reactFixture(t))
	hw := &fakeHealthWriter{}
	h.deps.health = hw
	standby := newCntlrMonitor(h.deps, testCid, testSpId, reactCntlrB)
	h.w.cntlrs[reactCntlrB] = &cntlrChild{
		driver: &cntlrDriver{health: standby},
	}
	// writes spells the standby's epoch writes in order, 0 for a clear and
	// E for a set.
	writes := func() string {
		var out []string
		for _, write := range hw.all() {
			if write.epoch == 0 {
				out = append(out, "0")
			} else {
				out = append(out, "E")
			}
		}
		return fmt.Sprint(out)
	}
	standby.observe(ctx, healthClean, "")

	// Every pass, not one in two: another observer stamps the standby
	// before each of two passes, and the clean verdict after each pass
	// clears the stamp. The fake writer leaves the state alone, so the test
	// takes the stamp off itself.
	for i, want := range []string{"[0 0]", "[0 0 0]"} {
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(1)
		h.pass()
		standby.observe(ctx, healthClean, "")
		if got := writes(); got != want {
			t.Fatalf("pass %d: epoch writes = %s, want %s: the pass did "+
				"not hand over the stamp it loaded", i+1, got, want)
		}
		h.state.Cntlrs[reactCntlrB].ErrEpoch = 0
	}

	// The child sets the epoch after the pass has read its count, and the
	// load does not hold the write: the fake writer leaves the state alone,
	// as a snapshot taken before the write landed would. Folded in, the
	// offer of that load would make the clean verdict after it no
	// transition, and the set would stay.
	ops := &loadHookOps{fakeSpOps: h.ops}
	h.w.ops = ops
	ops.hook = func() { standby.observe(ctx, healthUnreachable, "") }
	h.pass()
	standby.observe(ctx, healthClean, "")
	if got, want := writes(), "[0 0 0 E 0]"; got != want {
		t.Fatalf("epoch writes = %s, want %s: an offer whose load "+
			"predates the child's write was folded in", got, want)
	}
}

// ---------------------------------------------------------------------------
// AR6 — the `dmsetup status` parser
// ---------------------------------------------------------------------------

// TestParseThinPoolStatus runs the AR6 parser over real lines and over
// garbage.
func TestParseThinPoolStatus(t *testing.T) {
	ok := []struct {
		name  string
		line  string
		usage poolUsage
	}{
		{
			name: "kernel line",
			line: "0 20971520 thin-pool 0 1128/32768 5344/163840 - rw " +
				"discard_passdown queue_if_no_space - 1024",
			usage: poolUsage{1128, 32768, 5344, 163840},
		},
		{
			name: "device name prefix",
			line: "dnv-0000000000000011-2-pool: 0 8192 thin-pool 5 " +
				"40/1024 600/1024 - rw discard_passdown " +
				"queue_if_no_space needs_check 512",
			usage: poolUsage{40, 1024, 600, 1024},
		},
		{
			name:  "trailing fields absent",
			line:  "0 8192 thin-pool 5 40/1024 600/1024",
			usage: poolUsage{40, 1024, 600, 1024},
		},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			usage, parsed := parseThinPoolStatus(tc.line)
			if !parsed || usage != tc.usage {
				t.Fatalf("parse = %+v %v, want %+v", usage, parsed, tc.usage)
			}
		})
	}

	bad := []struct {
		name string
		line string
	}{
		{"empty", ""},
		{"whitespace", "   \t "},
		{"failed pool", "0 20971520 thin-pool Fail"},
		{"other target", "0 8192 linear 8:16 2048"},
		{"truncated", "0 8192 thin-pool 5 40/1024"},
		{"not a number", "0 8192 thin-pool 5 x/1024 600/1024 - rw"},
		{"no slash", "0 8192 thin-pool 5 40 600 - rw"},
		{"negative", "0 8192 thin-pool 5 -1/1024 600/1024 - rw"},
		{"words", "the pool is fine, thanks"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if usage, parsed := parseThinPoolStatus(tc.line); parsed {
				t.Fatalf("parsed garbage %q as %+v", tc.line, usage)
			}
		})
	}
}

// TestReactionBadPoolLineLoggedOncePerChange pins AR6's "an unparsable line is
// skipped and logged once per change".
func TestReactionBadPoolLineLoggedOncePerChange(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, "thin-pool Fail")
	h.pass()
	h.pass()
	if got := len(h.logs.withMsg(msgPoolStatusUnparsable)); got != 1 {
		t.Fatalf("unparsable records = %d, want 1", got)
	}
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, "thin-pool Error")
	h.pass()
	if got := len(h.logs.withMsg(msgPoolStatusUnparsable)); got != 2 {
		t.Fatalf("unparsable records = %d, want 2", got)
	}
	// A line that parses drops the memo, so the same bad line reported again
	// is a change and is reported again.
	total := reactDataBlocks(t, 2)
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(1, 1000, 1, total))
	h.pass()
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, "thin-pool Error")
	h.pass()
	if got := len(h.logs.withMsg(msgPoolStatusUnparsable)); got != 3 {
		t.Fatalf("unparsable records = %d, want 3", got)
	}
	h.wantOps()
}

// ---------------------------------------------------------------------------
// AR6 — thresholds, the pending rule and the candidate rules
// ---------------------------------------------------------------------------

// TestReactionGrowOnlyForOkPool pins AR6's "an ERROR / PROVISIONING / absent
// pool is never grown", and the two readings of low_water_mark_pct
// (architecture.md, Common validation, resolves it at write time): above 100
// is the kill switch and grows nothing quietly, zero is a conf
// CreateStoragePool could not have written and is REFUSED — the pass gate stops
// before any reaction.
func TestReactionGrowOnlyForOkPool(t *testing.T) {
	total := reactDataBlocks(t, 2)
	breach := poolLine(1, 1000, total, total)
	cases := []struct {
		name    string
		status  pb.ResStatus
		lwm     uint32
		grow    bool
		refused bool
	}{
		{"ok", pb.ResStatus_RES_STATUS_OK, 50, true, false},
		{"error", pb.ResStatus_RES_STATUS_ERROR, 50, false, false},
		{"provisioning", pb.ResStatus_RES_STATUS_PROVISIONING, 50, false, false},
		{"missing", pb.ResStatus_RES_STATUS_MISSING, 50, false, false},
		{"lwm zero is refused", pb.ResStatus_RES_STATUS_OK, 0, false, true},
		{"lwm off", pb.ResStatus_RES_STATUS_OK, 101, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.state.Conf.BdevConf.DmPoolConf.LowWaterMarkPct = tc.lwm
			h.setPool(reactSliceId, tc.status, breach)
			h.dnCands(reactDnC, reactDnD)
			h.pass()
			if tc.grow {
				h.wantOps("grow")
			} else {
				h.wantOps()
			}
			got := len(h.logs.withMsg(msgInvalidStoredConf))
			want := 0
			if tc.refused {
				want = 1
			}
			if got != want {
				// "auto-grow off" above all must stay silent: it is an
				// operator's setting, not a corrupt conf.
				t.Fatalf("%d invalid stored conf records, want %d", got, want)
			}
		})
	}
}

// TestReactionGrowNoPrimaryInfo checks that a pass whose primary has not
// reported yet grows nothing: AR6 reads the usage off the primary's own
// CntlrInfo and never guesses.
func TestReactionGrowNoPrimaryInfo(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	h.pass()
	h.wantOps()
}

// TestReactionDataGrowPending pins AR6's stateless pending rule for the DATA
// unit: the first grow runs, no second one starts while the reported total is
// still the pre-grow total, and one does again as soon as the new group is
// visible in it.
func TestReactionDataGrowPending(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	oneGrp := reactDataBlocks(t, 2)

	// One data group: the sum over "all but the last" is empty, so nothing is
	// pending and the breach grows.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(1, 1000, oneGrp*80/100, oneGrp))
	h.pass()
	calls := h.wantOps("grow")
	if calls[0].isMeta || calls[0].sliceId != reactSliceId {
		t.Fatalf("grow = %+v, want a data grow of the fixture slice", calls[0])
	}
	if len(calls[0].legs) != 2 {
		t.Fatalf("grow legs = %v, want two", calls[0].legs)
	}
	if calls[0].legs[0].AddrPort == calls[0].legs[1].AddrPort {
		t.Fatalf("both legs on %s", calls[0].legs[0].AddrPort)
	}
	query := h.rops.allQueries()[0]
	if query.kind != "dn" || query.candExt != 2 {
		t.Fatalf("scan = %+v, want ext_cnt 2 of the first data group", query)
	}
	// One batch per leg of the cluster's STORED alloc_conf.dn_batch_size
	// (architecture.md, Common validation). The fixture's value is not the
	// default of architecture.md, Common validation, so a scan that substituted
	// the constant would ask for 32 here.
	if query.candCnt != 2*reactDnBatch {
		t.Fatalf("candCnt = %d, want two stored dn_batch_size", query.candCnt)
	}
	if query.requiredCnt != 2 {
		t.Fatalf("requiredCnt = %d, want one per leg", query.requiredCnt)
	}
	if len(query.black) != 0 {
		t.Fatalf("black = %v, want empty", query.black)
	}

	// The group lands in etcd but the pool still reports the old total: the
	// grow is pending and no second one starts.
	h.slice().DataGrpList = append(h.slice().DataGrpList, reactGroup(
		t, 22, 2, []uint64{300, 301}, []uint64{400, 401},
		[]string{reactDnC, reactDnD},
	))
	h.pass()
	h.wantOps("grow")
	h.wantSkipped(reactionGrowData, reasonGrowPending)

	// The new group becomes visible in the reported total: not pending any
	// more, and the usage still breaches.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(1, 1000, 2*oneGrp*80/100, 2*oneGrp))
	h.pass()
	h.wantOps("grow", "grow")
}

// TestReactionMetaGrowPending pins the same rule for the METADATA unit, whose
// counts are in dm-thin's fixed 4 KiB blocks rather than the pool's data
// block size.
func TestReactionMetaGrowPending(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	metaTotal := reactMetaBlocks(t, 1)
	dataTotal := reactDataBlocks(t, 2)

	// Metadata over the watermark, data well under it.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(metaTotal*80/100, metaTotal, 1, dataTotal))
	h.pass()
	calls := h.wantOps("grow")
	if !calls[0].isMeta {
		t.Fatalf("grow = %+v, want a meta grow", calls[0])
	}
	// The meta ladder of architecture.md, GrowSlice: a slice whose meta groups
	// total 1 extent grows by 1.
	if query := h.rops.allQueries()[0]; query.candExt != 1 {
		t.Fatalf("scan = %+v, want the ladder ext_cnt 1", query)
	}

	// The second meta group exists in etcd but not yet in the reported total.
	h.slice().MetaGrpList = append(h.slice().MetaGrpList, reactGroup(
		t, 23, 1, []uint64{310, 311}, []uint64{410, 411},
		[]string{reactDnC, reactDnD},
	))
	h.pass()
	h.wantOps("grow")
	h.wantSkipped(reactionGrowMeta, reasonGrowPending)

	// Visible now, and still over the watermark.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(2*metaTotal*80/100, 2*metaTotal, 1, dataTotal))
	h.pass()
	calls = h.wantOps("grow", "grow")
	if !calls[1].isMeta {
		t.Fatalf("second grow = %+v, want a meta grow", calls[1])
	}
	// The ladder doubles: a slice at 2 meta extents grows by 2.
	queries := h.rops.allQueries()
	if last := queries[len(queries)-1]; last.candExt != 2 {
		t.Fatalf("scan = %+v, want the ladder ext_cnt 2", last)
	}
}

// TestReactionDataGrowsBeforeMeta pins AR6's "data is checked first when both
// breach".
func TestReactionDataGrowsBeforeMeta(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	metaTotal := reactMetaBlocks(t, 1)
	dataTotal := reactDataBlocks(t, 2)
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
		metaTotal*90/100, metaTotal, dataTotal*90/100, dataTotal,
	))
	h.pass()
	calls := h.wantOps("grow")
	if calls[0].isMeta {
		t.Fatalf("grew metadata first")
	}
	h.wantApplied(reactionGrowData)
}

// TestReactionGrowNeedsOneCandidatePerLeg pins AR6's "fewer than legs
// candidates ⇒ reaction skipped": a RedundMdRaid1 group needs two distinct
// DNs, a RedundNone one needs a single DN.
func TestReactionGrowNeedsOneCandidatePerLeg(t *testing.T) {
	total := reactDataBlocks(t, 2)
	breach := poolLine(1, 1000, total, total)

	t.Run("raid1 with one candidate", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, breach)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionGrowData, reasonNoCandidate)
	})

	t.Run("redund none with one candidate", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, breach)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("grow")
		if len(calls[0].legs) != 1 {
			t.Fatalf("legs = %v, want one", calls[0].legs)
		}
		if q := h.rops.allQueries()[0]; q.candCnt != reactDnBatch {
			t.Fatalf("candCnt = %d, want one stored dn_batch_size", q.candCnt)
		}
	})
}

// reactPendingDataGrow puts the fixture slice into AR6's PENDING data state:
// a second data group exists in etcd but the pool still reports the pre-grow
// total, exactly as it does while the CN defers the grow ([D15];
// architecture.md, Automatic reactions). It returns the reported data total.
func reactPendingDataGrow(t *testing.T, h *reactHarness) uint64 {
	t.Helper()
	h.slice().DataGrpList = append(h.slice().DataGrpList, reactGroup(
		t, 22, 2, []uint64{300, 301}, []uint64{400, 401},
		[]string{reactDnC, reactDnD},
	))
	return reactDataBlocks(t, 2)
}

// TestReactionPendingGrowDoesNotBlockThePass pins AR6's "while pending, no
// grow OF THAT KIND starts" against AR2's one-action rule: a grow that is not
// APPLICABLE — pending, or capped by the meta ladder of architecture.md,
// GrowSlice — is not an action, so it holds back neither the pool's other grow
// kind nor AR7 and AR8.
//
// Without this the deferral is self-locking: the one reaction that can clear a
// grow the CN deferred on a provisioning leg is AR8 on that very group, and it
// is the reaction the pending grow would be disabling.
func TestReactionPendingGrowDoesNotBlockThePass(t *testing.T) {
	t.Run("meta grows while data is pending", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		dataTotal := reactPendingDataGrow(t, h)
		metaTotal := reactMetaBlocks(t, 1)
		// Both kinds breach; only the data one is pending. Metadata filling
		// up puts the pool into needs_check, so it must not wait for the CN.
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
			metaTotal*80/100, metaTotal, dataTotal*80/100, dataTotal,
		))
		// An unhealthy leg is armed too: the walk continues past the pending
		// data grow, but AR2 still applies exactly ONE action.
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		calls := h.wantOps("grow")
		if !calls[0].isMeta {
			t.Fatalf("grow = %+v, want the meta grow", calls[0])
		}
		// The pass's own check found it pending: the record names no op.
		h.wantSkippedBy(reactionGrowData, reasonGrowPending, "")
		h.wantApplied(reactionGrowMeta)
	})

	t.Run("replace cntlr while data is pending", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.cnCands(reactCnC)
		dataTotal := reactPendingDataGrow(t, h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, dataTotal*80/100, dataTotal))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.pass()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrB {
			t.Fatalf("replace = %+v", calls[0])
		}
		h.wantSkipped(reactionGrowData, reasonGrowPending)
		h.wantApplied(reactionReplaceCntlr)
	})

	t.Run("leg repair while data is pending", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC)
		dataTotal := reactPendingDataGrow(t, h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, dataTotal*80/100, dataTotal))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_repair = %+v, want the data group", calls[0])
		}
		h.wantSkipped(reactionGrowData, reasonGrowPending)
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("leg repair while the meta ladder is capped", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC)
		// 256 × 64 MiB = 16 GiB, past the dm-thin metadata ceiling of
		// architecture.md, GrowSlice: the meta grow can never run again for
		// this slice.
		h.slice().MetaGrpList = []*pb.Group{reactGroup(
			t, reactMetaGrp, 256,
			[]uint64{reactMetaLegA, reactMetaLegB},
			[]uint64{reactSideMetaA, reactSideMetaB},
			[]string{reactDnA, reactDnB},
		)}
		metaTotal := reactMetaBlocks(t, 256)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
			metaTotal*90/100, metaTotal, 1, reactDataBlocks(t, 2),
		))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		h.wantOps("create_repair")
		h.wantSkipped(reactionGrowMeta, reasonMetaLadderCap)
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("leg repair while the data group list is full", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		// The group ceiling of architecture.md, GrowSlice: the data list
		// already holds MaxGrpCntPerSlice groups, so no data grow can ever run
		// again for this slice. The pool reports every group's blocks, more
		// than the pending rule's sum over all but the newest, so the grow is
		// not pending either: the ceiling is the only thing holding it.
		for idx := 1; idx < common.MaxGrpCntPerSlice; idx++ {
			id := uint64(5000 + 10*idx)
			h.slice().DataGrpList = append(h.slice().DataGrpList, reactGroup(
				t, id, 2, []uint64{id + 1, id + 2}, []uint64{id + 3, id + 4},
				[]string{reactDnA, reactDnB},
			))
		}
		dataTotal := reactDataBlocks(t, 2) * common.MaxGrpCntPerSlice
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, dataTotal*90/100, dataTotal))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_repair = %+v, want the data group", calls[0])
		}
		// The capped grow ran no scan: the one scan is the repair's.
		if queries := h.rops.allQueries(); len(queries) != 1 ||
			queries[0].requiredCnt != 1 {
			t.Fatalf("queries = %+v, want only the repair's scan", queries)
		}
		h.wantSkipped(reactionGrowData, reasonGrpListFull)
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("meta grows while the data group list is full", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		// The group ceiling of architecture.md, GrowSlice, is per list: a data
		// list at MaxGrpCntPerSlice holds back no metadata grow, and metadata
		// filling up puts the pool into needs_check.
		for idx := 1; idx < common.MaxGrpCntPerSlice; idx++ {
			id := uint64(5000 + 10*idx)
			h.slice().DataGrpList = append(h.slice().DataGrpList, reactGroup(
				t, id, 2, []uint64{id + 1, id + 2}, []uint64{id + 3, id + 4},
				[]string{reactDnA, reactDnB},
			))
		}
		dataTotal := reactDataBlocks(t, 2) * common.MaxGrpCntPerSlice
		metaTotal := reactMetaBlocks(t, 1)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
			metaTotal*90/100, metaTotal, dataTotal*90/100, dataTotal,
		))
		h.pass()
		calls := h.wantOps("grow")
		if !calls[0].isMeta {
			t.Fatalf("grow = %+v, want the meta grow", calls[0])
		}
		// The capped data grow ran no scan: the one scan is the meta's.
		if queries := h.rops.allQueries(); len(queries) != 1 {
			t.Fatalf("queries = %+v, want only the meta grow's scan", queries)
		}
		h.wantSkipped(reactionGrowData, reasonGrpListFull)
		h.wantApplied(reactionGrowMeta)
	})
}

// TestReactionPreconditionSkips checks AR2's other half: an op that raises
// model.ErrPrecondition logs `reaction skipped` with the precondition's own
// reason and the op's name, and the pass ends there.
func TestReactionPreconditionSkips(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	h.rops.opErr = &model.ErrPrecondition{
		Op: "Failover", Reason: "old cntlr is not primary",
	}
	h.pass()
	h.wantOps("failover")
	h.wantApplied()
	h.wantSkippedBy(reactionFailover, "old cntlr is not primary", "Failover")
}

// ---------------------------------------------------------------------------
// AR7 — cntlr replacement
// ---------------------------------------------------------------------------

// TestReactionReplaceCntlrBlackList pins AR7's black list and its spCnAddrs:
// the old CN is excluded even though the node itself is healthy, and the SP's
// other cntlrs' CNs are handed over as the exclusion of architecture.md,
// Finding CN candidates.
func TestReactionReplaceCntlrBlackList(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
	h.cnCands(reactCnC)
	h.pass()
	calls := h.wantOps("replace")
	if calls[0].oldId != reactCntlrB || calls[0].asPrimary {
		t.Fatalf("replace = %+v", calls[0])
	}
	if calls[0].legs[0].AddrPort != reactCnC {
		t.Fatalf("new cn = %s", calls[0].legs[0].AddrPort)
	}
	query := h.rops.allQueries()[0]
	if query.kind != "cn" {
		t.Fatalf("query = %+v, want a cn scan", query)
	}
	// candExt is the SP footprint: 1 meta extent + 2 data extents.
	if query.candExt != 3 {
		t.Fatalf("candExt = %d, want the SP footprint 3", query.candExt)
	}
	// The cluster's STORED alloc_conf.cn_batch_size (architecture.md, Common
	// validation). The fixture stores a different value in each of the two
	// batch-size members, so this also pins WHICH one a cn scan reads.
	if query.candCnt != reactCnBatch {
		t.Fatalf("candCnt = %d, want the stored cn_batch_size", query.candCnt)
	}
	if len(query.black) != 1 || query.black[0] != reactCnB {
		t.Fatalf("black = %v, want the old cntlr's cn", query.black)
	}
	if len(query.spCn) != 1 || query.spCn[0] != reactCnA {
		t.Fatalf("spCnAddrs = %v, want the other cntlr's cn", query.spCn)
	}
	// The op is handed the very plan the scan ran against, which it holds
	// the SP's surviving cntlrs to inside its STM.
	if fmt.Sprint(calls[0].spCn) != fmt.Sprint(query.spCn) {
		t.Fatalf("replace spCnAddrs = %v, want the scan's %v",
			calls[0].spCn, query.spCn)
	}
	h.wantApplied(reactionReplaceCntlr)
}

// TestReactionReplaceCntlrExcludesCntlrLocations pins AR7's tier-1 exclusion
// (architecture.md, Per-operation allocation): the cn scan carries the
// locations of the SP's OTHER cntlrs' CNs, read from the pass's own snapshot of
// the node records (MD3) rather than from a second etcd round-trip, so the
// replacement lands outside the failure domains the surviving cntlrs occupy
// whenever tier 1 finds a CN. The old cntlr adds no location of its own — it is
// the one leaving — so its domain is excluded only through a survivor that
// shares it.
func TestReactionReplaceCntlrExcludesCntlrLocations(t *testing.T) {
	build := func(t *testing.T, locA string, locB string) *reactHarness {
		t.Helper()
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.state.CnByAddr[reactCnA] = &pb.CnConf{Location: locA}
		h.state.CnByAddr[reactCnB] = &pb.CnConf{Location: locB}
		h.cnCands(reactCnC)
		return h
	}
	wantScan := func(t *testing.T, h *reactHarness, wantLocs []string) {
		t.Helper()
		calls := h.wantOps("replace")
		if calls[0].legs[0].AddrPort != reactCnC {
			t.Fatalf("new cn = %s", calls[0].legs[0].AddrPort)
		}
		queries := h.rops.allQueries()
		if len(queries) != 1 || queries[0].kind != "cn" {
			t.Fatalf("queries = %+v", queries)
		}
		// Quoted, so that an empty-string location — what a missing
		// cn_conf would contribute through its nil-safe getter — shows up as
		// [""] rather than printing like no location at all.
		if fmt.Sprintf("%q", queries[0].excludeLocs) !=
			fmt.Sprintf("%q", wantLocs) {
			t.Errorf("exclude_locs = %q, want %q",
				queries[0].excludeLocs, wantLocs)
		}
	}

	t.Run("two named locations", func(t *testing.T) {
		h := build(t, "rack-left", "rack-right")
		h.pass()
		wantScan(t, h, []string{"rack-left"})
	})

	t.Run("a survivor sharing the old cntlr's location", func(t *testing.T) {
		// The old cntlr's domain is not taken out of the exclusion when a
		// survivor holds it too: tier 1 keeps the replacement out of it.
		h := build(t, "rack-right", "rack-right")
		h.pass()
		wantScan(t, h, []string{"rack-right"})
	})

	t.Run("a missing cn_conf contributes no location", func(t *testing.T) {
		h := build(t, "rack-left", "rack-right")
		delete(h.state.CnByAddr, reactCnA)
		h.pass()
		wantScan(t, h, nil)
	})

	t.Run("the default location is the addr_port", func(t *testing.T) {
		// The default of architecture.md, Controller nodes, makes the exclusion
		// the same CN spCnAddrs already excludes, which is AR7's behavior
		// before the two tiers covered it.
		h := build(t, reactCnA, reactCnB)
		h.pass()
		wantScan(t, h, []string{reactCnA})
	})

	t.Run("the locations of every survivor", func(t *testing.T) {
		// A third cntlr, healthy, on a fourth CN puts the failing cntlr B
		// between two survivors: the exclusion is both of their locations,
		// in cntlr_id order, and not the first survivor's alone.
		const cntlrC = uint64(3)
		const cnD = "rcn3:9620"
		h := build(t, "rack-left", "rack-right")
		h.state.Conf.CntlrIdList = append(h.state.Conf.CntlrIdList, cntlrC)
		h.state.Cntlrs[cntlrC] = &pb.Cntlr{AddrPort: cnD}
		h.state.CnByAddr[cnD] = &pb.CnConf{Location: "rack-mid"}
		h.pass()
		wantScan(t, h, []string{"rack-left", "rack-mid"})
	})
}

// TestReactionReplaceSolePrimary pins AR7's sole-primary variant: the primary
// of an SP with no failover candidate is replaced by a new PRIMARY, and AR5's
// no-candidate record is emitted first — the one skip that does not end the
// pass.
func TestReactionReplaceSolePrimary(t *testing.T) {
	state := reactFixture(t)
	state.Conf.CntlrIdList = []uint64{reactCntlrA}
	delete(state.Cntlrs, reactCntlrB)
	h := newReactHarness(t, state)
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(700)
	h.cnCands(reactCnC)
	h.pass()
	calls := h.wantOps("replace")
	if calls[0].oldId != reactCntlrA || !calls[0].asPrimary {
		t.Fatalf("replace = %+v, want the primary replaced as primary",
			calls[0])
	}
	if calls[0].now != h.now() {
		t.Fatalf("now = %d, want %d", calls[0].now, h.now())
	}
	h.wantSkipped(reactionFailover, reasonNoCandidate)
	h.wantApplied(reactionReplaceCntlr)
	if q := h.rops.allQueries()[0]; len(q.spCn) != 0 {
		t.Fatalf("spCnAddrs = %v, want empty for a sole cntlr", q.spCn)
	}
}

// TestReactionSharedStateErrorIsNotReplaced pins AR7's first refusal, judged
// by AR5's first (HL2's row classes): the primary of an SP with no failover
// candidate, unhealthy for cntlr_unhealthy, whose latest report fails only in
// the stack of a created td whose thin id the pool no longer holds is not
// replaced — the replacement would read the same rows from the same pool —
// and the pass records why and goes on: to a standby that is AR7's target in
// its own right, or, with none, to AR8. Short of cntlr_unhealthy AR7 records
// nothing, nor for a primary that has a failover candidate, which is AR5's
// alone, nor for a disabled primary, which is never AR7's target. The report
// judged is the one the pass holds, the primary's latest (AR1), so each other
// case gets the primary replaced: an own row beside the stack, a probe's
// report, which carries no reason, the converge's thin row with the lost id's
// text and no reason, a primary read unreachable after the converge's report,
// and a primary whose child has reported nothing.
func TestReactionSharedStateErrorIsNotReplaced(t *testing.T) {
	// sole is sharedStateFixture with the standby disabled, so AR5 has no
	// candidate, and the primary unhealthy for cntlr_unhealthy.
	sole := func(t *testing.T) *reactHarness {
		t.Helper()
		h := newReactHarness(t, sharedStateFixture(t))
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy)
		h.cnCands(reactCnC)
		return h
	}
	wantPrimaryReplaced := func(t *testing.T, h *reactHarness) {
		t.Helper()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrA || !calls[0].asPrimary {
			t.Fatalf("replace = %+v, want the primary replaced as primary",
				calls[0])
		}
		h.wantApplied(reactionReplaceCntlr)
	}

	t.Run("the lost td's stack alone", func(t *testing.T) {
		h := sole(t)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		// AR8 is armed so that the pass going on is observable.
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps("create_repair")
		h.wantApplied(reactionRepairCreate)
		var recs []map[string]any
		for _, rec := range h.logs.withMsg(msgReactionSkipped) {
			if rec["kind"] == reactionReplaceCntlr {
				recs = append(recs, rec)
			}
		}
		if len(recs) != 1 || recs[0]["reason"] != "shared_state" {
			t.Fatalf("replace_cntlr skips = %v, want shared_state once", recs)
		}
		for attr, want := range map[string]uint64{
			"old_cntlr_id": reactCntlrA,
			"td_id":        reactLostTd,
		} {
			if got, _ := recs[0][attr].(float64); uint64(got) != want {
				t.Fatalf("%s = %v, want %d", attr, recs[0][attr], want)
			}
		}
		h.wantSkipped(reactionFailover, "shared_state")
	})

	t.Run("a standby past its threshold", func(t *testing.T) {
		h := newReactHarness(t, sharedStateFixture(t))
		// Unhealthy, the standby is no failover candidate either.
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy)
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		h.cnCands(reactCnC)
		h.pass()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrB || calls[0].asPrimary {
			t.Fatalf("replace = %+v, want the standby replaced", calls[0])
		}
		h.wantApplied(reactionReplaceCntlr)
		h.wantSkipped(reactionReplaceCntlr, "shared_state")
	})

	t.Run("below the threshold", func(t *testing.T) {
		h := sole(t)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy - 1)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		h.pass()
		h.wantOps()
		for _, rec := range h.logs.withMsg(msgReactionSkipped) {
			if rec["kind"] == reactionReplaceCntlr {
				t.Fatalf("replace_cntlr skip below the threshold: %v", rec)
			}
		}
	})

	// A primary with a failover candidate is AR5's, past cntlr_unhealthy
	// too: AR7 passes it over before judging its report, so only AR5's
	// shared_state is recorded, not AR7's beside it.
	t.Run("a primary with a candidate", func(t *testing.T) {
		h := newReactHarness(t, sharedStateFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		h.cnCands(reactCnC)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionFailover, "shared_state")
		for _, rec := range h.logs.withMsg(msgReactionSkipped) {
			if rec["kind"] == reactionReplaceCntlr {
				t.Fatalf("replace_cntlr skip for a primary AR5 can move: %v",
					rec)
			}
		}
	})

	// A disabled cntlr is never AR7's target (AR3), so its report is not
	// judged: the refusal comes after the disabled check, and a disabled
	// primary with no candidate gets AR5's `no candidate` alone, not AR7's
	// shared_state beside it.
	t.Run("a disabled primary", func(t *testing.T) {
		h := sole(t)
		h.state.Cntlrs[reactCntlrA].Disabled = true
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(true))
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionFailover, reasonNoCandidate)
		for _, rec := range h.logs.withMsg(msgReactionSkipped) {
			if rec["kind"] == reactionReplaceCntlr {
				t.Fatalf("replace_cntlr skip for a disabled primary: %v", rec)
			}
		}
	})

	for _, tc := range []struct {
		name string
		info func() *pb.CntlrInfo
	}{
		{"an own row beside it", func() *pb.CntlrInfo {
			info := lostStackInfo(true)
			info.SsIdToSubsystem[600] = resErr("ss", "not linked to the port")
			return info
		}},
		{"a probe's report", func() *pb.CntlrInfo {
			return lostStackInfo(false)
		}},
		{"the lost id's text with no reason", func() *pb.CntlrInfo {
			info := lostStackInfo(true)
			info.TdIdToThinInfo[reactLostTd].SliceIdToDmThin[reactSliceId].
				Reason = pb.ResReason_RES_REASON_NONE
			return info
		}},
		{"an unreachable primary", func() *pb.CntlrInfo {
			info := lostStackInfo(true)
			markCntlrUnknown(info)
			return info
		}},
		{"no report", func() *pb.CntlrInfo { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := sole(t)
			if info := tc.info(); info != nil {
				h.setPrimaryInfo(reactCntlrA, info)
			}
			h.pass()
			wantPrimaryReplaced(t, h)
		})
	}
}

// The ids of TestReactionReplacementNotReplacedOverTheSameError: the cntlrs
// the fake's replaceCntlr hands out in turn, and a standby whose id puts it
// after the first of them in AR7's scan.
const (
	reactFreshA   = uint64(1000)
	reactFreshB   = uint64(1001)
	reactFreshC   = uint64(1002)
	reactStandbyD = uint64(1100)
	reactCnD      = "rcn3:9620"
)

// TestReactionReplacementNotReplacedOverTheSameError pins AR7's second
// refusal. The sole primary of an SP with no failover candidate is replaced
// over a probe's report of the lost td — no refusal of the first kind, the
// report carrying no reason — and the replacement reads the same rows from
// the same pool: it is not replaced while every ERROR row of its report is
// one the primary it replaced failed on, from an error set within
// cntlr_unhealthy of that replacement. The error is presumed to have
// followed the replacement. Each pass records why, and the scan goes on, to
// a standby due for replacement, whose replacement leaves the record as it
// was. The replacement is replaced on anything else: a row of its own beside
// those rows — after which that replacement's record holds the next one — a
// report with no ERROR row, an error set cntlr_unhealthy after the
// replacement, and a coordinator that holds no record, as after a restart or
// a shard handoff. The record names that replacement alone: another primary
// failing on the same rows is replaced.
func TestReactionReplacementNotReplacedOverTheSameError(t *testing.T) {
	// commit applies what model.ReplaceCntlr commits for a primary: oldId
	// leaves, fresh takes the role, settling, on a CN of its own, and its
	// child reports rows. Its first report fails at once, as a converge over
	// the lost td does.
	commit := func(
		h *reactHarness,
		oldId uint64,
		fresh uint64,
		rows *pb.CntlrInfo,
	) {
		ids := []uint64{}
		for _, id := range h.state.Conf.CntlrIdList {
			if id != oldId {
				ids = append(ids, id)
			}
		}
		h.state.Conf.CntlrIdList = append(ids, fresh)
		delete(h.state.Cntlrs, oldId)
		delete(h.w.cntlrs, oldId)
		h.state.Cntlrs[fresh] = &pb.Cntlr{
			AddrPort: reactCnC, Primary: true, Settling: true,
			ErrEpoch: h.now(),
		}
		h.setPrimaryInfo(fresh, rows)
	}
	// replaced runs the pass that replaces the sole primary A — the standby
	// is disabled — over a probe's report and commits the replacement,
	// reactFreshA, whose report holds rows.
	replaced := func(t *testing.T, rows *pb.CntlrInfo) *reactHarness {
		t.Helper()
		h := newReactHarness(t, sharedStateFixture(t))
		h.rops.newCntlrIds = []uint64{reactFreshA, reactFreshB, reactFreshC}
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy)
		h.setPrimaryInfo(reactCntlrA, lostStackInfo(false))
		h.cnCands(reactCnC)
		h.pass()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrA || !calls[0].asPrimary {
			t.Fatalf("replace = %+v, want the primary replaced as primary",
				calls[0])
		}
		commit(h, reactCntlrA, reactFreshA, rows)
		return h
	}
	advance := func(h *reactHarness, seconds uint64) {
		h.clk.advance(time.Duration(seconds) * time.Second)
	}
	// refusals are the pass records of AR7's second refusal.
	refusals := func(h *reactHarness) []map[string]any {
		var out []map[string]any
		for _, rec := range h.logs.withMsg(msgReactionSkipped) {
			if rec["kind"] == reactionReplaceCntlr &&
				rec["reason"] == "same_error" {
				out = append(out, rec)
			}
		}
		return out
	}
	wantRefused := func(t *testing.T, h *reactHarness, cntlrId uint64, n int) {
		t.Helper()
		recs := refusals(h)
		if len(recs) != n {
			t.Fatalf("replace_cntlr same_error skips = %v, want %d", recs, n)
		}
		for _, rec := range recs {
			if got, _ := rec["old_cntlr_id"].(float64); uint64(got) != cntlrId {
				t.Fatalf("skip = %v, want old_cntlr_id %d", rec, cntlrId)
			}
		}
	}

	t.Run("the same rows", func(t *testing.T) {
		h := replaced(t, lostStackInfo(false))
		advance(h, common.DefaultCntlrUnhealthy)
		// AR8 is armed so that the pass going on is observable.
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		h.pass()
		h.wantOps("replace", "create_repair", "create_repair")
		wantRefused(t, h, reactFreshA, 2)
	})

	// A standby due for replacement comes after the replacement in the scan:
	// the scan goes on to it, and its replacement, a standby's, touches no
	// record, so the next pass refuses the primary's replacement again.
	t.Run("a standby replaced meanwhile", func(t *testing.T) {
		h := replaced(t, lostStackInfo(false))
		h.state.Conf.CntlrIdList = append(h.state.Conf.CntlrIdList,
			reactStandbyD)
		h.state.Cntlrs[reactStandbyD] = &pb.Cntlr{
			AddrPort: reactCnD, ErrEpoch: h.now(),
		}
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("replace", "replace")
		if calls[1].oldId != reactStandbyD || calls[1].asPrimary {
			t.Fatalf("replace = %+v, want the standby replaced", calls[1])
		}
		wantRefused(t, h, reactFreshA, 1)
		// The fake commits nothing, so the standby is still due.
		h.pass()
		calls = h.wantOps("replace", "replace", "replace")
		if calls[2].oldId != reactStandbyD {
			t.Fatalf("replace = %+v, want the standby replaced again",
				calls[2])
		}
		wantRefused(t, h, reactFreshA, 2)
	})

	// A row of the replacement's own gets it replaced, and that replacement
	// records the rows in turn: the next one, failing on the lost td's rows
	// alone, is held.
	t.Run("a row of its own", func(t *testing.T) {
		rows := lostStackInfo(false)
		rows.SsIdToSubsystem[600] = resErr("ss", "not linked to the port")
		h := replaced(t, rows)
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("replace", "replace")
		if calls[1].oldId != reactFreshA || !calls[1].asPrimary {
			t.Fatalf("replace = %+v, want the replacement replaced as "+
				"primary", calls[1])
		}
		wantRefused(t, h, 0, 0)
		commit(h, reactFreshA, reactFreshB, lostStackInfo(false))
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		h.wantOps("replace", "replace")
		wantRefused(t, h, reactFreshB, 1)
	})

	// A replacement read unreachable has every row UNKNOWN: no row is one
	// the primary it replaced failed on.
	t.Run("no ERROR row", func(t *testing.T) {
		rows := lostStackInfo(false)
		markCntlrUnknown(rows)
		h := replaced(t, rows)
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("replace", "replace")
		if calls[1].oldId != reactFreshA {
			t.Fatalf("replace = %+v, want the replacement replaced", calls[1])
		}
	})

	// A replacement whose error was set within cntlr_unhealthy of its
	// replacement is held; one set later is a new error, judged as any.
	for _, tc := range []struct {
		after uint64
		want  []string
	}{
		{common.DefaultCntlrUnhealthy - 1, []string{"replace"}},
		{common.DefaultCntlrUnhealthy, []string{"replace", "replace"}},
	} {
		t.Run(fmt.Sprintf("failing %d s after", tc.after), func(t *testing.T) {
			h := replaced(t, lostStackInfo(false))
			fresh := h.state.Cntlrs[reactFreshA]
			fresh.ErrEpoch = 0
			advance(h, tc.after)
			fresh.ErrEpoch = h.now()
			advance(h, common.DefaultCntlrUnhealthy)
			h.pass()
			h.wantOps(tc.want...)
		})
	}

	// The record goes with the coordinator: one restarted, or handed the
	// shard, replaces the replacement once more.
	t.Run("no record", func(t *testing.T) {
		h := replaced(t, lostStackInfo(false))
		h.w.react = newReactor(h.rops)
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("replace", "replace")
		if calls[1].oldId != reactFreshA || !calls[1].asPrimary {
			t.Fatalf("replace = %+v, want the replacement replaced as "+
				"primary", calls[1])
		}
		wantRefused(t, h, 0, 0)
	})

	// The record names the replacement: a primary the role has moved to
	// since, failing on the same rows with no failover candidate — the
	// replacement disabled — is replaced.
	t.Run("another primary", func(t *testing.T) {
		h := replaced(t, lostStackInfo(false))
		fresh := h.state.Cntlrs[reactFreshA]
		fresh.Primary, fresh.Settling, fresh.Disabled = false, false, true
		delete(h.w.cntlrs, reactFreshA)
		h.state.Conf.CntlrIdList = append(h.state.Conf.CntlrIdList,
			reactCntlrC)
		h.state.Cntlrs[reactCntlrC] = &pb.Cntlr{
			AddrPort: reactCnD, Primary: true, ErrEpoch: h.now(),
		}
		h.setPrimaryInfo(reactCntlrC, lostStackInfo(false))
		advance(h, common.DefaultCntlrUnhealthy)
		h.pass()
		calls := h.wantOps("replace", "replace")
		if calls[1].oldId != reactCntlrC || !calls[1].asPrimary {
			t.Fatalf("replace = %+v, want the new primary replaced",
				calls[1])
		}
	})

	// The refusal judges the primary alone, on the report the pass holds for
	// it: a replacement that has lost the role since — to a primary that
	// reads the same rows and is not yet due for replacement — is judged as
	// any standby, and replaced.
	t.Run("a demoted replacement", func(t *testing.T) {
		h := replaced(t, lostStackInfo(false))
		fresh := h.state.Cntlrs[reactFreshA]
		fresh.Primary, fresh.Settling = false, false
		delete(h.w.cntlrs, reactFreshA)
		advance(h, common.DefaultCntlrUnhealthy)
		h.state.Conf.CntlrIdList = append(h.state.Conf.CntlrIdList,
			reactCntlrC)
		h.state.Cntlrs[reactCntlrC] = &pb.Cntlr{
			AddrPort: reactCnD, Primary: true, Settling: true,
			ErrEpoch: h.now(),
		}
		h.setPrimaryInfo(reactCntlrC, lostStackInfo(false))
		h.pass()
		calls := h.wantOps("replace", "replace")
		if calls[1].oldId != reactFreshA || calls[1].asPrimary {
			t.Fatalf("replace = %+v, want the demoted replacement replaced "+
				"as a standby", calls[1])
		}
		wantRefused(t, h, 0, 0)
	})
}

// TestReactionPrimaryWithCandidateIsNotReplaced checks the other side of AR7:
// while a failover candidate exists the unhealthy primary is failed over, not
// replaced, however long it has been unhealthy.
func TestReactionPrimaryWithCandidateIsNotReplaced(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(9000)
	h.cnCands(reactCnC)
	h.pass()
	h.wantOps("failover")
	h.wantApplied(reactionFailover)
}

// TestReactionReplaceNoCandidate pins AR7's "none ⇒ reaction skipped".
func TestReactionReplaceNoCandidate(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionReplaceCntlr, reasonNoCandidate)
}

// TestReactionCntlrThresholdNotReached checks AR4 on the replacement path: a
// cntlr unhealthy for less than cntlr_unhealthy is left alone.
func TestReactionCntlrThresholdNotReached(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(
		common.DefaultCntlrUnhealthy - 1,
	)
	h.cnCands(reactCnC)
	h.pass()
	h.wantOps()
	h.wantApplied()
}

// ---------------------------------------------------------------------------
// AR10 — the own-verdict rule
// ---------------------------------------------------------------------------

// TestReactionNeedsOwnVerdict pins AR10: a threshold reaction fires only on an
// object whose latest health verdict by this coordinator is unhealthy, however
// old its stored err_epoch, while the clock still runs from that epoch — a
// verdict given now on an epoch an hour old fires at once — and a reaction the
// rule holds back logs nothing. A failover on its threshold needs more: two
// unhealthy verdicts in a row from the primary's rounds, which a clean answer
// of either kind ends and a Syncup* reply with an ERROR row neither starts nor
// adds to. The disabled trigger needs no verdict; a cntlr replacement needs
// only the latest verdict, a Syncup*'s as much as a round's. The clocks of
// AR8's try-to-abort are TestLegRepairAbortClocks's. The harness's stored-epoch shortcut is off, so every verdict here
// is one the test notes.
func TestReactionNeedsOwnVerdict(t *testing.T) {
	newHarness := func(t *testing.T) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		h.judgeStored = false
		return h
	}

	t.Run("failover", func(t *testing.T) {
		h := newHarness(t)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(3600)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		// One round's verdict: the run is one short.
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		h.pass()
		h.wantOps("failover")
	})

	t.Run("failover after a clean verdict", func(t *testing.T) {
		h := newHarness(t)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(3600)
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		roundVerdict(h.w.verdicts, reactCntlrA, false)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
	})

	t.Run("a clean answer of either kind ends the run", func(t *testing.T) {
		for _, round := range []bool{true, false} {
			h := newHarness(t)
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(3600)
			roundVerdict(h.w.verdicts, reactCntlrA, true)
			if round {
				roundVerdict(h.w.verdicts, reactCntlrA, false)
			} else {
				syncupVerdict(h.w.verdicts, reactCntlrA, false)
			}
			roundVerdict(h.w.verdicts, reactCntlrA, true)
			h.pass()
			h.wantOps()
			h.wantNoSkip()
		}
	})

	t.Run("a Syncup reply neither starts nor adds to the run", func(t *testing.T) {
		h := newHarness(t)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(3600)
		for range 3 {
			syncupVerdict(h.w.verdicts, reactCntlrA, true)
		}
		h.pass()
		h.wantOps()
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		syncupVerdict(h.w.verdicts, reactCntlrA, true)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		h.pass()
		h.wantOps("failover")
	})

	t.Run("a grow: the Syncup's ERROR rows, a missed round, a clean round", func(t *testing.T) {
		// The slice-16 order: the converge reports the new groups ERROR while
		// a member is not connected yet, the next round misses, and the round
		// after it answers clean. A round answers at once from the agent's
		// latest observation, so here it misses when no reply comes in time,
		// or when its observation is too old while the agent's retry holds
		// the cntlr's reads off (HL7).
		h := newHarness(t)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(3600)
		syncupVerdict(h.w.verdicts, reactCntlrA, true)
		roundVerdict(h.w.verdicts, reactCntlrA, true)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		roundVerdict(h.w.verdicts, reactCntlrA, false)
		h.pass()
		h.wantOps()
	})

	t.Run("the disabled trigger", func(t *testing.T) {
		h := newHarness(t)
		h.state.Cntlrs[reactCntlrA].Disabled = true
		h.pass()
		h.wantOps("failover")
	})

	t.Run("cntlr replacement", func(t *testing.T) {
		h := newHarness(t)
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(
			common.DefaultCntlrUnhealthy)
		h.cnCands(reactCnC)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		// One Syncup reply's verdict is enough: only the failover counts runs.
		syncupVerdict(h.w.verdicts, reactCntlrB, true)
		h.pass()
		h.wantOps("replace")
	})

	t.Run("leg repair, the leg's clock", func(t *testing.T) {
		h := newHarness(t)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		h.w.verdicts.noteLeg(reactDataLegA, true)
		h.pass()
		h.wantOps("create_repair")
	})

	t.Run("leg repair, the side's clock", func(t *testing.T) {
		h := newHarness(t)
		leg := h.legOf(reactDataLegA)
		leg.ErrEpoch = h.ago(1)
		side := leg.SideList[0]
		side.ErrEpoch = h.ago(common.DefaultSideUnhealthy)
		key := sideKey{legId: leg.GetLegId(), sideId: side.GetSideId()}
		h.dnCands(reactDnC)
		// The leg judged, the side not: case 2's clock has no verdict.
		h.w.verdicts.noteLeg(reactDataLegA, true)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		// The side judged, the leg not: no leg verdict, no repair.
		h.w.verdicts.noteLeg(reactDataLegA, false)
		h.w.verdicts.noteSide(key, true)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
		h.w.verdicts.noteLeg(reactDataLegA, true)
		h.pass()
		h.wantOps("create_repair")
	})

}

// TestVerdictMemoDropsWhatTheSnapshotDoesNotList pins the memo's pruning: a
// verdict on a cntlr, leg or side the pass's snapshot no longer lists — a
// replaced cntlr, a removed leg — is dropped rather than kept for ever, ids
// being never reused within an SP, and so is a dropped cntlr's run. A nil
// memo, a coordinator assembled by hand, holds nothing.
func TestVerdictMemoDropsWhatTheSnapshotDoesNotList(t *testing.T) {
	state := reactFixture(t)
	leg := state.Slices[reactSliceId].GetDataGrpList()[0].GetLegList()[0]
	listed := sideKey{
		legId:  leg.GetLegId(),
		sideId: leg.GetSideList()[0].GetSideId(),
	}
	gone := sideKey{legId: 9001, sideId: 9002}
	memo := newVerdictMemo()
	roundVerdict(memo, reactCntlrB, true)
	roundVerdict(memo, 9000, true)
	memo.noteLeg(leg.GetLegId(), true)
	memo.noteLeg(9001, true)
	memo.noteSide(listed, true)
	memo.noteSide(gone, true)
	got := memo.snapshot(state)
	if !got.cntlrs[reactCntlrB] || !got.legs[leg.GetLegId()] ||
		!got.sides[listed] {
		t.Fatalf("snapshot %+v lost a listed object's verdict", got)
	}
	if got.cntlrs[9000] || got.legs[9001] || got.sides[gone] {
		t.Fatalf("snapshot %+v kept an unlisted object's verdict", got)
	}
	if len(memo.cntlrs) != 1 || len(memo.legs) != 1 || len(memo.sides) != 1 {
		t.Fatalf("memo kept %d cntlrs, %d legs, %d sides, want one each",
			len(memo.cntlrs), len(memo.legs), len(memo.sides))
	}
	if got.runs[reactCntlrB] != 1 || len(got.runs) != 1 || len(memo.runs) != 1 {
		t.Fatalf("runs: snapshot %v, memo %v, want only cntlr %d's run of one",
			got.runs, memo.runs, reactCntlrB)
	}
	var none *verdictMemo
	roundVerdict(none, reactCntlrB, true)
	if got := none.snapshot(state); len(got.cntlrs) != 0 {
		t.Fatalf("a nil memo judged %v", got.cntlrs)
	}
}

// ---------------------------------------------------------------------------
// Pass inputs
// ---------------------------------------------------------------------------

// TestReactionPassNeedsClusterConf checks that a pass whose cluster is not in
// the RW21 cache does nothing at all: every allocating reaction needs its
// extent_size and batch sizes, and the stage moves run after the pass's
// gates (AR11), so a repair the primary reports started moves no stage.
func TestReactionPassNeedsClusterConf(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.deps.conf.mu.Lock()
	delete(h.deps.conf.entries, testCid)
	h.deps.conf.mu.Unlock()
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
	h.setVerdict(reactRepId, true, false)
	h.pass()
	h.wantOps()
	h.wantApplied()
	h.wantNoRecord(msgRepairStageMoved)
}

// TestReactionPassRefusesAnInvalidConf checks the pass's gates (AR1;
// architecture.md, Common validation). Both stored confs are needed and both
// are checked before the reactions are evaluated: every allocating reaction
// computes with the cluster's extent_size and batch sizes, and AR6 reads
// low_water_mark_pct and data_block_size straight off the SP's own bdev_conf.
// Every group's length to zero (RW15) is checked too: the fan-out builds no
// request for an SP with a group whose stored counts give its sides none
// (RW14), so a reaction committed past it could not be delivered. Any one of
// them unusable makes the pass a complete no-op — no candidate scan, no model
// op, no `reaction applied` and no `reaction skipped` — with one Error record
// naming the field.
//
// The fixture is set up to WANT a reaction (an unhealthy primary, a breached
// pool) and a stage move (a repair the primary reports started), so a pass
// that did nothing because there was nothing to do could not be mistaken for
// a pass that refused; the stage moves run after the gates (AR11).
func TestReactionPassRefusesAnInvalidConf(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(h *reactHarness)
		field   string
	}{
		{
			name: "cluster conf without a bin ladder",
			corrupt: func(h *reactHarness) {
				cc := reactClusterConf()
				cc.DnBinConf = nil
				setCachedConf(h.deps, testCid, cc)
			},
			field: "dn_bin_conf",
		},
		{
			name: "sp bdev_conf without a pool block size",
			corrupt: func(h *reactHarness) {
				h.state.Conf.BdevConf.DmPoolConf.DataBlockSize = 0
			},
			field: "data_block_size",
		},
		{
			name: "a group with no length to zero",
			corrupt: func(h *reactHarness) {
				h.dataGrp().MetaBlocks = 0
			},
			field: fmt.Sprintf("group %d meta_blocks is zero", reactDataGrp),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			total := reactDataBlocks(t, 2)
			h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
				poolLine(1, 1000, total, total))
			h.dnCands(reactDnC, reactDnD)
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
			withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
			h.setVerdict(reactRepId, true, false)
			tc.corrupt(h)
			h.pass()

			h.wantOps()
			h.wantApplied()
			h.wantNoRecord(msgRepairStageMoved)
			if got := len(h.rops.allQueries()); got != 0 {
				t.Fatalf("%d candidate scans on a refused pass", got)
			}
			if got := len(h.logs.withMsg(msgReactionSkipped)); got != 0 {
				t.Fatalf("a refused pass logged a skip: it never got that far")
			}
			recs := h.logs.withMsg(msgInvalidStoredConf)
			if len(recs) != 1 {
				t.Fatalf("%d invalid stored conf records, want 1", len(recs))
			}
			if err, _ := recs[0]["error"].(string); !strings.Contains(
				err, tc.field,
			) {
				t.Fatalf("error = %q, want %s named", err, tc.field)
			}
			// The memo holds across ticks: a steady bad conf costs one record,
			// not one per pass.
			h.pass()
			h.pass()
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 1 {
				t.Fatalf("%d records over three passes, want 1", got)
			}
		})
	}
}

// TestReactionPassRefusesAGroupWithNoLength checks AR1's group gate on the
// reaction it stops: while a group's stored counts give its sides no length
// to zero (RW15), the failover the fixture wants is not committed, because
// the fan-out could not deliver it (RW14), and the first pass after the group
// is repaired commits it.
func TestReactionPassRefusesAGroupWithNoLength(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	metaBlocks := h.dataGrp().GetMetaBlocks()
	h.dataGrp().MetaBlocks = 0
	h.pass()
	h.wantOps()
	h.wantApplied()

	h.dataGrp().MetaBlocks = metaBlocks
	h.pass()
	calls := h.wantOps("failover")
	if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
		t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
	}
	h.wantApplied(reactionFailover)
}

// TestReactionPassLoadFailure checks the two load outcomes of AR1: a deleted
// SP is silent, a transient failure is reported and retried on the next tick.
func TestReactionPassLoadFailure(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.ops.setErr(model.ErrNotFound)
		h.pass()
		h.wantOps()
		if got := len(h.logs.withMsg(msgSpLoadFailed)); got != 0 {
			t.Fatalf("load failures = %d, want 0", got)
		}
	})

	t.Run("transient", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.ops.setErr(context.DeadlineExceeded)
		h.pass()
		h.wantOps()
		if got := len(h.logs.withMsg(msgSpLoadFailed)); got != 1 {
			t.Fatalf("load failures = %d, want 1", got)
		}
	})
}

// TestReactionScanFailure checks that a failed allocator scan ends the pass
// with an `op_failed` record rather than a mutation.
func TestReactionScanFailure(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.rops.scanErr = context.DeadlineExceeded
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionRepairCreate, reasonOpFailed)
}

// ---------------------------------------------------------------------------
// Unit-level helpers
// ---------------------------------------------------------------------------

// TestGrowPendingBoundaries pins the boundary AR6 is easiest to get wrong: the
// sum over "all groups but the last" is empty for zero and one group, so
// nothing is pending there whatever the pool reports.
func TestGrowPendingBoundaries(t *testing.T) {
	one := reactDataBlocks(t, 2)
	slice0 := &pb.Slice{}
	slice1 := &pb.Slice{DataGrpList: []*pb.Group{{DataBlocks: one}}}
	slice2 := &pb.Slice{DataGrpList: []*pb.Group{
		{DataBlocks: one}, {DataBlocks: one},
	}}
	cases := []struct {
		name  string
		slice *pb.Slice
		total uint64
		want  bool
	}{
		{"no group", slice0, 0, false},
		{"one group, nothing reported", slice1, 0, false},
		{"one group, reported", slice1, one, false},
		{"two groups, old total", slice2, one, true},
		{"two groups, one block more", slice2, one + 1, false},
		{"two groups, new total", slice2, 2 * one, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := poolUsage{totalData: tc.total}
			got := growPending(tc.slice, false, usage, reactBlockSize)
			if got != tc.want {
				t.Fatalf("growPending = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPickDistinct pins AR6's growing black list: never two picks on one DN,
// and fewer than asked for when the scan returned fewer.
func TestPickDistinct(t *testing.T) {
	cands := []model.Cand{
		{AddrPort: reactDnA}, {AddrPort: reactDnB}, {AddrPort: reactDnC},
	}
	for run := 0; run < 32; run++ {
		picks := pickDistinct(cands, 2)
		if len(picks) != 2 {
			t.Fatalf("picks = %v, want two", picks)
		}
		if picks[0].AddrPort == picks[1].AddrPort {
			t.Fatalf("picks = %v, want distinct DNs", picks)
		}
	}
	if got := pickDistinct(cands[:1], 2); len(got) != 1 {
		t.Fatalf("picks = %v, want the one candidate there was", got)
	}
	if got := pickDistinct(nil, 2); len(got) != 0 {
		t.Fatalf("picks = %v, want none", got)
	}
}

// TestReached pins AR4's underflow-safe comparison.
func TestReached(t *testing.T) {
	cases := []struct {
		name      string
		now       uint64
		errEpoch  uint64
		threshold uint32
		want      bool
	}{
		{"healthy", 1000, 0, 5, false},
		{"below", 1000, 998, 5, false},
		{"exactly", 1000, 995, 5, true},
		{"above", 1000, 900, 5, true},
		{"zero threshold", 1000, 1000, 0, true},
		{"clock went backwards", 1000, 2000, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reached(tc.now, tc.errEpoch, tc.threshold)
			if got != tc.want {
				t.Fatalf("reached = %v, want %v", got, tc.want)
			}
		})
	}
}
