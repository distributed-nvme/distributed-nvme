package gateway

import (
	"context"
	"math"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.4 / §8.5 and gateway.md §5.4: the seven
// storage-pool RPCs plus GrowSlice.
//
// It is the only file that CREATES an SP, and therefore the only one that has
// to make three separate allocations agree inside one transaction: the DNs
// that carry every leg (§6.5), the CNs that carry every cntlr, and the per-SP
// id counter that names all of them. All three are decided from scans that
// necessarily ran OUTSIDE the transaction, so every one of them is re-checked
// against its exact capacity key inside it and the whole unit — scan and
// commit — is retried when one moved (GW9).
//
// The counterpart is DeleteStoragePool, which reverses all of it in one STM:
// there is no partial teardown state, because a half-deleted SP would leave DN
// and CN budgets charged for legs no key describes any more.

// The op names the bump helpers cite, so a log record names something
// greppable. They are the RPC names verbatim.
const (
	opCreateStoragePool               = "CreateStoragePool"
	opDeleteStoragePool               = "DeleteStoragePool"
	opUpdateStoragePoolCntlidSlotList = "UpdateStoragePoolCntlidSlotList"
	opUpdateStoragePoolLevel          = "UpdateStoragePoolLevel"
	opGrowSlice                       = "GrowSlice"
)

// ---------------------------------------------------------------------------
// Pre-STM planning helpers
// ---------------------------------------------------------------------------

// spPreReadCluster is the plain pre-read §5.8 allows for the work that cannot
// wait for the transaction: a candidate scan needs the cluster_id, the extent
// size and the batch sizes, and an STM cannot range at all. The in-STM
// resolveCluster stays authoritative — everything read here is re-derived and
// re-checked inside the transaction — so this read can only ever be an
// optimization, never a decision.
func spPreReadCluster(
	ctx context.Context,
	cli *etcdutil.Client,
	clusterName string,
) (uint64, *pb.ClusterConf, error) {
	name := clusterNameOf(clusterName)
	cc := &pb.ClusterConf{}
	found, err := cli.Get(ctx, model.ClusterConfKey(name), cc)
	if err != nil {
		return 0, nil, errAborted("%v", err)
	}
	if !found {
		return 0, nil, errNotFound("cluster %q not found", name)
	}
	return model.ClusterId(name, cc.GetCreationEpoch()), cc, nil
}

// spDefaultCntlidSlots is the `cntlid_slot_list = [0..7]` default of §8.4: an
// SP that names no slots may use every slot a CN has.
func spDefaultCntlidSlots() []uint32 {
	slots := make([]uint32, 0, common.CnCntlidSlotCnt)
	for slot := uint32(0); slot < common.CnCntlidSlotCnt; slot++ {
		slots = append(slots, slot)
	}
	return slots
}

// spMergeUint64 is one member of decision D-C's merge: the request wins unless
// it left the member at the proto3 zero that means "unset" (§7, GW11).
func spMergeUint64(reqValue uint64, clusterValue uint64) uint64 {
	if reqValue != 0 {
		return reqValue
	}
	return clusterValue
}

// spMergeUint32 is spMergeUint64 for a 32-bit member.
func spMergeUint32(reqValue uint32, clusterValue uint32) uint32 {
	if reqValue != 0 {
		return reqValue
	}
	return clusterValue
}

// redundKindSet reports whether a RedundConf actually selects a kind. An unset
// oneof — including the empty message a caller may send — is not a choice: it
// already means redund_none (validate.go), so it must not shadow the cluster's
// own setting during the merge.
func redundKindSet(conf *pb.RedundConf) bool {
	return conf.GetRedunKind() != nil
}

// mergeSpBdevConf is decision D-C: the bdev_conf CreateStoragePool STORES is
// the member-wise merge of the request over ClusterConf.bdev_conf, plus the
// one structural default of §8.4 — a redund_conf unset in both is redund_none.
//
// The merge is stored rather than re-resolved at read time because an SP's
// geometry must be immutable: legCntOf, model.GroupBlocks and every thin
// device sized against the stripe would otherwise move under a live SP the
// moment someone edited the cluster's defaults. Numeric members left zero in
// both stay zero — model.GroupBlocks and model.PoolBlockSize resolve them to
// the same constants everywhere, so a stored zero and a stored default are the
// same geometry (GW11).
//
// bdev_feature_list has no merge: §7 refuses a non-empty one on both the
// cluster and the SP, so there is never anything to carry over. The chosen
// redund_conf is cloned because it ends up inside a stored message and must
// not alias a request the caller still owns.
func mergeSpBdevConf(
	reqConf *pb.BdevConf,
	clusterConf *pb.BdevConf,
) *pb.BdevConf {
	merged := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize: spMergeUint64(
				reqConf.GetDmPoolConf().GetDataBlockSize(),
				clusterConf.GetDmPoolConf().GetDataBlockSize()),
			LowWaterMarkPct: spMergeUint32(
				reqConf.GetDmPoolConf().GetLowWaterMarkPct(),
				clusterConf.GetDmPoolConf().GetLowWaterMarkPct()),
		},
		DmRaid0Conf: &pb.DmRaid0Conf{
			StripeSize: spMergeUint64(
				reqConf.GetDmRaid0Conf().GetStripeSize(),
				clusterConf.GetDmRaid0Conf().GetStripeSize()),
		},
	}
	merged.RedundConf = mergeRedundConf(
		reqConf.GetRedundConf(), clusterConf.GetRedundConf())
	return merged
}

// mergeRedundConf is the redund_conf half of decision D-C's member-wise merge.
//
// The KIND is a choice, not a member: the request's wins, then the cluster's,
// and an unset oneof on both is §8.4's structural default redund_none. But
// when both sides choose md-raid1 the merge has to continue INSIDE the chosen
// message, or a `--raid1` that names no chunk count would silently discard the
// cluster's bitmap_chunk_block_cnt and give the SP a different §3.6 geometry
// than the cluster was configured for — the exact drift "member-wise" exists
// to prevent.
//
// The result is always a fresh message: it ends up inside a stored SpConf and
// must not alias a request the caller still owns.
func mergeRedundConf(
	reqConf *pb.RedundConf,
	clusterConf *pb.RedundConf,
) *pb.RedundConf {
	reqRaid1 := reqConf.GetRedundMdRaid1()
	clusterRaid1 := clusterConf.GetRedundMdRaid1()
	if reqRaid1 != nil {
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: spMergeUint64(
						reqRaid1.GetBitmapChunkBlockCnt(),
						clusterRaid1.GetBitmapChunkBlockCnt()),
				},
			},
		}
	}
	if redundKindSet(reqConf) || !redundKindSet(clusterConf) {
		// The request chose redund_none, or neither chose anything: §8.4's
		// "redund_conf unset ⇒ redund_none" covers both.
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
	}
	return proto.Clone(clusterConf).(*pb.RedundConf)
}

// growSliceCnBudget is §8.5's CN half of RESOURCE_EXHAUSTED: a grow every
// cntlr of the SP will stack needs extCnt free extents on every one of their
// CNs (§6.5). It reads the cntlrs and their CnConfs at one store revision, so
// the answer describes one consistent moment rather than a walk that a
// concurrent delete could tear.
//
// A cntlr or a CnConf whose key is gone is skipped rather than raised: this is
// a pre-check whose only job is to give the common case the code §8.5 names,
// and the deciding STM re-reads all of it — model.chargeSpCns is what actually
// refuses.
func (s *Server) growSliceCnBudget(
	ctx context.Context,
	cid uint64,
	conf *pb.SpConf,
	extCnt uint64,
) error {
	var short string
	var free uint64
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		short = ""
		for _, cntlrId := range conf.GetCntlrIdList() {
			cntlr := &pb.Cntlr{}
			if !stm.Get(model.CntlrKey(cid, conf.GetSpId(), cntlrId), cntlr) {
				continue
			}
			cn := &pb.CnConf{}
			if !stm.Get(model.CnConfKey(cid, cntlr.GetAddrPort()), cn) {
				continue
			}
			if cn.GetFreeExtCnt() < extCnt {
				short, free = cntlr.GetAddrPort(), cn.GetFreeExtCnt()
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return mapStmErr(err)
	}
	if short != "" {
		return errExhausted(
			"controller node %q has %d free extents, the new group needs %d",
			short, free, extCnt)
	}
	return nil
}

// spGrpPlan is one group CreateStoragePool intends to build. It carries no
// ids — those are minted inside the STM — only what the §6.5 scan and
// model.GroupBlocks need, and the position that fixes decision D-D's mint
// order.
type spGrpPlan struct {
	SliceIdx int
	IsMeta   bool
	ExtCnt   uint64
}

// planSpGroups is the §8.4 step-1 plan, in decision D-D's order: per slice one
// META group of 1 extent — the first rung of the §8.5 ladder — followed by one
// DATA group of init_ext_cnt. The order is the contract: the DN scan draws its
// picks in it and the STM mints ids in it, so a retried attempt reproduces
// exactly the same write set.
func planSpGroups(sliceCnt int, initExtCnt uint64) []spGrpPlan {
	plans := make([]spGrpPlan, 0, 2*sliceCnt)
	for sliceIdx := 0; sliceIdx < sliceCnt; sliceIdx++ {
		plans = append(plans,
			spGrpPlan{SliceIdx: sliceIdx, IsMeta: true, ExtCnt: 1},
			spGrpPlan{SliceIdx: sliceIdx, IsMeta: false, ExtCnt: initExtCnt},
		)
	}
	return plans
}

// ---------------------------------------------------------------------------
// The RPCs
// ---------------------------------------------------------------------------

// CreateStoragePool is architecture.md §8.4's CreateStoragePool.
//
// The whole RPC is ONE candidate unit (GW9): both scans run outside the
// transaction, the transaction re-validates every single pick against the
// exact capacity key the scan saw, and errCandidateChanged sends the unit back
// to the scan rather than re-running the transaction — a pick whose key moved
// is stale information, not a write conflict, and re-validating it for ever
// would never succeed.
//
// The scans need the extent size, the batch sizes and the leg count, all of
// which live in ClusterConf, so each iteration starts with one plain pre-read
// of it (§5.8). That read decides nothing: the in-STM read is authoritative,
// and when it yields a different cluster_id — the cluster was deleted and
// re-created under the scan — or a different leg count, every pick was drawn
// for a different SP shape and the unit re-plans.
//
// Nothing is bumped here. SpRev is CREATED at revision 1, and the DN/CN
// revisions are bumped once per node by the two ledgers' flush (§5.5), however
// many sides or cntlrs of this SP one node ended up carrying.
func (s *Server) CreateStoragePool(
	ctx context.Context,
	req *pb.CreateStoragePoolRequest,
) (*pb.CreateStoragePoolReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateCntlidSlotList(
		req.GetCntlidSlotList(), true,
	); err != nil {
		return nil, err
	}
	slots := req.GetCntlidSlotList()
	if len(slots) == 0 {
		slots = spDefaultCntlidSlots()
	}
	cntlrCnt := int(req.GetCntlrCnt())
	if cntlrCnt == 0 {
		cntlrCnt = common.DefaultCntlrCntPerSp
	}
	if cntlrCnt < common.MinCntlrCntPerSp ||
		cntlrCnt > common.MaxCntlrCntPerSp {
		return nil, errInvalid("cntlr_cnt %d is outside [%d, %d]",
			cntlrCnt, common.MinCntlrCntPerSp, common.MaxCntlrCntPerSp)
	}
	if cntlrCnt > len(slots) {
		// §11.8: the cntlid slots of one SP's cntlrs are all distinct, so a
		// list shorter than cntlr_cnt cannot name them.
		return nil, errInvalid(
			"cntlr_cnt %d exceeds the %d entries of cntlid_slot_list",
			cntlrCnt, len(slots))
	}
	sliceCnt := int(req.GetSliceCnt())
	if sliceCnt < 1 || sliceCnt > common.MaxSliceCntPerSp {
		return nil, errInvalid("slice_cnt %d is outside [1, %d]",
			sliceCnt, common.MaxSliceCntPerSp)
	}
	if req.GetInitExtCnt() == 0 {
		return nil, errInvalid("init_ext_cnt must not be zero")
	}
	if err := validateBdevConf(req.GetBdevConf()); err != nil {
		return nil, err
	}
	if err := validateEventThreshold(req.GetEventThreshold()); err != nil {
		return nil, err
	}
	if err := validateNodeSelector(
		"dn_selector", req.GetDnSelector(),
	); err != nil {
		return nil, err
	}
	if err := validateNodeSelector(
		"cn_selector", req.GetCnSelector(),
	); err != nil {
		return nil, err
	}
	plans := planSpGroups(sliceCnt, req.GetInitExtCnt())
	// §6.5: one cntlr's CN reserves the WHOLE SP, because every cntlr stacks
	// every group of every slice.
	footprint := uint64(0)
	for _, plan := range plans {
		footprint += plan.ExtCnt
	}
	var spId uint64
	err := candidateUnit(ctx, func() error {
		scanCid, scanCc, err := spPreReadCluster(
			ctx, s.cli, req.GetClusterName())
		if err != nil {
			return err
		}
		legs := legCntOf(mergeSpBdevConf(
			req.GetBdevConf(), scanCc.GetBdevConf()))
		// §6.5 DN scan, in decision D-D's group order. The black list starts
		// as the request's own (pickDns folds dn_selector.black_list in) and
		// grows with every pick, so every leg of the WHOLE SP lands on a
		// distinct DN — not merely every leg of one group. ExcludeLocs stays
		// empty: §6.5 leaves CreateStoragePool out of the two-tier rule, an SP
		// being created has no failure domains to keep out of yet, and the
		// scan's own one-DN-per-location rule already spreads each group.
		dnPicks := make([][]model.Cand, 0, len(plans))
		var dnBlack []string
		for _, plan := range plans {
			picks, err := pickDns(
				ctx, s.cli, scanCid, scanCc,
				dnPickPlan{ExtCnt: plan.ExtCnt, Legs: legs, ExcludeLocs: nil},
				req.GetDnSelector(), dnBlack, "create storage pool")
			if err != nil {
				return err
			}
			dnPicks = append(dnPicks, picks)
			dnBlack = append(dnBlack, candAddrs(picks)...)
		}
		// §6.5 CN scan: one pick per cntlr, each for the SP's whole
		// footprint, each black-listed so two cntlrs never share a CN.
		cnPicks := make([]model.Cand, 0, cntlrCnt)
		var cnBlack []string
		for idx := 0; idx < cntlrCnt; idx++ {
			pick, err := pickCn(
				ctx, s.cli, scanCid, scanCc, footprint,
				req.GetCnSelector(), cnBlack, nil, "create storage pool")
			if err != nil {
				return err
			}
			cnPicks = append(cnPicks, pick)
			cnBlack = append(cnBlack, pick.AddrPort)
		}
		return s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			spId = 0
			cid, cc, err := resolveCluster(stm, req.GetClusterName())
			if err != nil {
				return err
			}
			bdev := mergeSpBdevConf(req.GetBdevConf(), cc.GetBdevConf())
			if cid != scanCid || legCntOf(bdev) != legs {
				// The scan planned for another cluster or another leg count;
				// its picks describe an SP this transaction is not building.
				return errCandidateChanged
			}
			confKey := model.SpConfKey(cid, req.GetSpName())
			if stm.Get(confKey, &pb.SpConf{}) {
				return errExists(
					"storage pool %q already exists", req.GetSpName())
			}
			globalKey := model.SpGlobalKey(cid)
			global := &pb.SpGlobal{}
			if !stm.Get(globalKey, global) {
				return errAborted("sp_global key %q is missing", globalKey)
			}
			minted, err := mintClusterId(
				global.GetNextId(), global.GetShardBucket(),
				common.MaxSpCntPerCluster, "storage pool")
			if err != nil {
				return err
			}
			spId = minted.Id
			conf := &pb.SpConf{
				SpId:           spId,
				ShardCode:      minted.Shard,
				NextDevId:      1,
				BdevConf:       bdev,
				EventThreshold: req.GetEventThreshold(),
				CntlidSlotList: slots,
				SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
				Deleting:       false,
			}
			// The minter is seeded on the SpConf being BUILT, whose next_id is
			// still the proto3 zero, so it starts at model.SpFirstId and the
			// counter it commits is the same on every attempt (GW12).
			minter := newSpIdMinter(conf)
			// D-D: every cntlr_id first, in pick order.
			cntlrIds := make([]uint64, cntlrCnt)
			for idx := range cntlrIds {
				cntlrIds[idx] = minter.mint()
				conf.CntlrIdList = append(conf.CntlrIdList, cntlrIds[idx])
			}
			extentSize := model.ResolveDnBinConf(
				cc.GetDnBinConf()).GetExtentSize()
			// Everything below is staged in memory and written only once
			// every pick has been verified and every budget charged: a
			// refusal — a moved capacity key above all — must return before
			// the first Put, not merely before the commit.
			dnl := newDnLedger(stm, cid, cc)
			builtSlices := make([]*pb.Slice, 0, sliceCnt)
			for sliceIdx := 0; sliceIdx < sliceCnt; sliceIdx++ {
				// D-D: then per slice its slice_id, then the META group and
				// the DATA group in plan order, each one grp_id then per leg
				// leg_id and its side_id.
				sliceId := minter.mint()
				conf.SliceIdList = append(conf.SliceIdList, sliceId)
				slice := &pb.Slice{SliceIdx: uint32(sliceIdx)}
				for planIdx, plan := range plans {
					if plan.SliceIdx != sliceIdx {
						continue
					}
					metaBlocks, dataBlocks, err := model.GroupBlocks(
						plan.ExtCnt, extentSize, bdev)
					if err != nil {
						// §8.4's "init_ext_cnt × extent_size exceeds what one
						// slice's data groups may hold": the requested group
						// is too small to carry its own §3.6 metadata, or the
						// product overflows.
						return errInvalid("%v", err)
					}
					grp := &pb.Group{
						GrpId:      minter.mint(),
						ExtCnt:     plan.ExtCnt,
						MetaBlocks: metaBlocks,
						DataBlocks: dataBlocks,
					}
					for legIdx, cand := range dnPicks[planIdx] {
						legId := minter.mint()
						sideId := minter.mint()
						dn, err := dnl.verifyPick(cand, plan.ExtCnt)
						if err != nil {
							return err
						}
						grp.LegList = append(grp.LegList, &pb.Leg{
							LegId:  legId,
							LegIdx: uint32(legIdx),
							SideList: []*pb.Side{{
								SideId:     sideId,
								AddrPort:   cand.AddrPort,
								CntlidSlot: slots[0],
								NvmeTrConf: dn.GetNvmeTrConf(),
								ErrEpoch:   0,
								// [D15]: the sp-worker flips it once the DN
								// agent has zeroed the side (§9.4, §10.3).
								Provisioned: false,
							}},
						})
						err = dnl.charge(
							cand.AddrPort,
							&pb.SidePointer{
								SpId:   spId,
								LegId:  legId,
								SideId: sideId,
							},
							plan.ExtCnt)
						if err != nil {
							return err
						}
					}
					if plan.IsMeta {
						slice.MetaGrpList = append(slice.MetaGrpList, grp)
					} else {
						slice.DataGrpList = append(slice.DataGrpList, grp)
					}
				}
				builtSlices = append(builtSlices, slice)
			}
			cnl := newCnLedger(stm, cid)
			builtCntlrs := make([]*pb.Cntlr, 0, cntlrCnt)
			for idx, cand := range cnPicks {
				cn, err := cnl.verifyPick(cand, footprint)
				if err != nil {
					return err
				}
				err = cnl.charge(
					cand.AddrPort,
					&pb.CntlrPointer{SpId: spId, CntlrId: cntlrIds[idx]},
					footprint)
				if err != nil {
					return err
				}
				builtCntlrs = append(builtCntlrs, &pb.Cntlr{
					AddrPort:   cand.AddrPort,
					NvmeTrConf: cn.GetNvmeTrConf(),
					// §11.8: the slots of one SP's cntlrs are all distinct,
					// and the list was checked long enough for cntlr_cnt.
					CntlidSlot: slots[idx],
					Primary:    idx == 0,
					Disabled:   false,
					ErrEpoch:   0,
				})
			}
			for idx, slice := range builtSlices {
				stm.Put(
					model.SliceKey(cid, spId, conf.GetSliceIdList()[idx]),
					slice)
			}
			for idx, cntlr := range builtCntlrs {
				stm.Put(model.CntlrKey(cid, spId, cntlrIds[idx]), cntlr)
			}
			minter.commit(conf)
			stm.Put(confKey, conf)
			stm.Put(model.SpNameKey(cid, spId),
				&pb.SpName{SpName: req.GetSpName()})
			// The rev key is created, not bumped: revision starts at 1 (§5.5)
			// and carries the sp_name a watching worker needs to form the
			// SpConf key without a second lookup [D10].
			stm.Put(model.SpRevKey(minted.Shard, cid, spId), &pb.SpRev{
				SpName:   req.GetSpName(),
				Revision: 1,
			})
			if err := dnl.flush(opCreateStoragePool); err != nil {
				return err
			}
			if err := cnl.flush(opCreateStoragePool); err != nil {
				return err
			}
			global.NextId = minted.NextId
			global.ShardBucket = minted.Bucket
			stm.Put(globalKey, global)
			return nil
		})
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateStoragePoolReply{SpId: spId}, nil
}

// DeleteStoragePool is architecture.md §8.4's DeleteStoragePool.
//
// It is the one mutator that opens an SP with rejectDeleting = false: an SP
// whose teardown has begun refuses every other change, and refusing the RPC
// that finishes the teardown would strand it.
//
// The five name lists are the whole precondition: cntlrs, slices, groups, legs
// and sides were created implicitly by CreateStoragePool and are deleted
// implicitly here, while thin devices, subsystems, clones, transfers and
// migrations are objects a user made and must remove first.
//
// One STM tears everything down. There is no partial teardown state, because a
// half-deleted SP would leave DN and CN budgets charged for legs no key
// describes any more — and the ledgers are what keep that one write, one
// capacity-key maintenance and one revision bump per node (§5.5) no matter how
// many sides of this SP one DN happened to carry.
func (s *Server) DeleteStoragePool(
	ctx context.Context,
	req *pb.DeleteStoragePoolRequest,
) (*pb.DeleteStoragePoolReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var spId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		spId = 0
		sc, err := openSpFlags(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev().GetRevision(), false)
		if err != nil {
			return err
		}
		held := []struct {
			what string
			cnt  int
		}{
			{"thin devices", len(sc.Conf.GetTdNameList())},
			{"subsystems", len(sc.Conf.GetNqnList())},
			{"clones", len(sc.Conf.GetCloneNameList())},
			{"transfers", len(sc.Conf.GetXferNameList())},
			{"migrations", len(sc.Conf.GetMigrNameList())},
		}
		for _, item := range held {
			if item.cnt != 0 {
				return errPrecondition(
					"storage pool %q still holds %d %s",
					req.GetSpName(), item.cnt, item.what)
			}
		}
		cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		// Every cntlr reserved the SP's whole footprint, so every one of them
		// returns it (§6.5). It is computed from the slices as they are NOW,
		// which is what makes a grown SP release exactly what it charged.
		footprint := spFootprint(slices)
		dnl := newDnLedger(stm, sc.Cid, sc.Cc)
		cnl := newCnLedger(stm, sc.Cid)
		for _, slice := range slices {
			for _, grp := range allGroups(slice) {
				// Spare legs occupy a DN exactly like an active one (§8.12),
				// so allLegs walks both lists.
				for _, leg := range allLegs(grp) {
					for _, side := range leg.GetSideList() {
						err := dnl.release(
							side.GetAddrPort(), sc.SpId(),
							side.GetSideId(), grp.GetExtCnt())
						if err != nil {
							return err
						}
					}
				}
			}
		}
		for idx, cntlr := range cntlrs {
			err := cnl.release(cntlr.GetAddrPort(), sc.SpId(),
				sc.Conf.GetCntlrIdList()[idx], footprint)
			if err != nil {
				return err
			}
		}
		globalKey := model.SpGlobalKey(sc.Cid)
		global := &pb.SpGlobal{}
		if !stm.Get(globalKey, global) {
			// Read before the first Del so this refusal, like every other
			// one, returns without having staged a write.
			return errAborted("sp_global key %q is missing", globalKey)
		}
		for _, cntlrId := range sc.Conf.GetCntlrIdList() {
			stm.Del(model.CntlrKey(sc.Cid, sc.SpId(), cntlrId))
		}
		for _, sliceId := range sc.Conf.GetSliceIdList() {
			stm.Del(model.SliceKey(sc.Cid, sc.SpId(), sliceId))
		}
		stm.Del(model.SpNameKey(sc.Cid, sc.SpId()))
		// The sp-worker stops dispatching when this key disappears (§10.3).
		stm.Del(model.SpRevKey(sc.Shard(), sc.Cid, sc.SpId()))
		stm.Del(model.SpConfKey(sc.Cid, req.GetSpName()))
		// GW12: the bucket shrinks, next_id never rewinds — a deleted sp_id
		// must never come back, so agents may assume it never does (§5.4).
		global.ShardBucket = releaseShard(
			global.GetShardBucket(), sc.Shard())
		stm.Put(globalKey, global)
		if err := dnl.flush(opDeleteStoragePool); err != nil {
			return err
		}
		if err := cnl.flush(opDeleteStoragePool); err != nil {
			return err
		}
		spId = sc.SpId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteStoragePoolReply{SpId: spId}, nil
}

// GetStoragePool is architecture.md §8.4's GetStoragePool: one Snapshot, so
// the SpConf, its token and every listed sub-object come from ONE store
// revision. A reply assembled from several revisions could show a cntlr list
// from before a CreateCntlr next to an SpRev from after it, and a client that
// then used that token would be refused for a reason it could not see.
//
// A listed key that is missing is §5.9's ABORTED, not an omission: the id
// lists are the SP's inventory, and a reply that quietly dropped an entry
// would tell a caller the object never existed.
func (s *Server) GetStoragePool(
	ctx context.Context,
	req *pb.GetStoragePoolRequest,
) (*pb.GetStoragePoolReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var reply *pb.GetStoragePoolReply
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		reply = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		revKey := model.SpRevKey(sc.Shard(), sc.Cid, sc.SpId())
		rev := &pb.SpRev{}
		if !stm.Get(revKey, rev) {
			return errAborted("sp_rev key %q is missing", revKey)
		}
		cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		reply = &pb.GetStoragePoolReply{
			SpName:    req.GetSpName(),
			SpConf:    sc.Conf,
			SpRev:     rev,
			CntlrList: cntlrs,
			SliceList: slices,
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return reply, nil
}

// ListStoragePools is architecture.md §8.4's ListStoragePools (GW10).
//
// Like every paged list it uses plain reads and no transaction (§5.7), but it
// still needs the cluster_id its prefix is built from, which is the one plain
// pre-read of ClusterConf §5.7 prescribes.
func (s *Server) ListStoragePools(
	ctx context.Context,
	req *pb.ListStoragePoolsRequest,
) (*pb.ListStoragePoolsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	// GW4: the two page arguments are pure §7 checks, so they run
	// before the ClusterConf read the prefix needs — a bad count or
	// page_token must be INVALID_ARGUMENT, not the NOT_FOUND a missing
	// cluster would otherwise answer first.
	if err := validatePageArgs(
		req.GetCount(), req.GetPageToken(),
	); err != nil {
		return nil, err
	}
	cid, _, err := spPreReadCluster(ctx, s.cli, req.GetClusterName())
	if err != nil {
		return nil, err
	}
	names, nextToken, err := pageNames(
		ctx, s.cli, model.SpConfPrefix(cid),
		req.GetCount(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListStoragePoolsReply{
		SpName:    names,
		PageToken: nextToken,
	}, nil
}

// UpdateStoragePoolCntlidSlotList is architecture.md §8.4's
// UpdateStoragePoolCntlidSlotList.
//
// The list's shape is checked outside the STM (§7: values below 8, no
// duplicates, and — unlike CreateStoragePool — not empty, because an SP with
// no slot can produce no side). The state-dependent half has to be inside it:
// a slot may only leave the list when nothing uses it, and what uses one is
// every cntlr's cntlid_slot and every side's, which are only knowable from the
// cntlrs and slices this transaction reads.
//
// Sides are included, not just cntlrs, because a side's cntlid_slot is what
// the DN's nvmet subsystem exports it under (§11.8); dropping a slot a side
// still names would make the SP undeployable, not merely inconsistent.
func (s *Server) UpdateStoragePoolCntlidSlotList(
	ctx context.Context,
	req *pb.UpdateStoragePoolCntlidSlotListRequest,
) (*pb.UpdateStoragePoolCntlidSlotListReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateCntlidSlotList(
		req.GetCntlidSlotList(), false,
	); err != nil {
		return nil, err
	}
	var spId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		spId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev().GetRevision())
		if err != nil {
			return err
		}
		cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		wanted := make(map[uint32]bool, len(req.GetCntlidSlotList()))
		for _, slot := range req.GetCntlidSlotList() {
			wanted[slot] = true
		}
		for idx, cntlr := range cntlrs {
			if !wanted[cntlr.GetCntlidSlot()] {
				return errInvalid(
					"cntlid_slot_list drops slot %d, which cntlr %d uses",
					cntlr.GetCntlidSlot(),
					sc.Conf.GetCntlrIdList()[idx])
			}
		}
		for _, slice := range slices {
			for _, grp := range allGroups(slice) {
				for _, leg := range allLegs(grp) {
					for _, side := range leg.GetSideList() {
						if wanted[side.GetCntlidSlot()] {
							continue
						}
						return errInvalid(
							"cntlid_slot_list drops slot %d, which side %d "+
								"uses",
							side.GetCntlidSlot(), side.GetSideId())
					}
				}
			}
		}
		sc.Conf.CntlidSlotList = req.GetCntlidSlotList()
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		spId = sc.SpId()
		return bumpSp(stm, opUpdateStoragePoolCntlidSlotList, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateStoragePoolCntlidSlotListReply{SpId: spId}, nil
}

// UpdateStoragePoolLevel is architecture.md §8.4's UpdateStoragePoolLevel: the
// staged disaster-recovery / maintenance switch of §11.7.
//
// The level is not applied here in any sense — it rides in both Syncup*
// requests, so the bump is the whole mechanism: the workers see the new SpRev,
// re-push every side and cntlr, and the agents tear the stack down to whatever
// the level allows.
//
// It is written and bumped unconditionally even when the level is unchanged:
// §0 #17's idempotent no-write covers the three Update*Enabled/Disabled RPCs
// only, and an operator resending a level after a partial convergence wants
// exactly the re-push a bump produces.
func (s *Server) UpdateStoragePoolLevel(
	ctx context.Context,
	req *pb.UpdateStoragePoolLevelRequest,
) (*pb.UpdateStoragePoolLevelReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateSpLevel(req.GetSpLevel()); err != nil {
		return nil, err
	}
	var spId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		spId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev().GetRevision())
		if err != nil {
			return err
		}
		sc.Conf.SpLevel = req.GetSpLevel()
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		spId = sc.SpId()
		return bumpSp(stm, opUpdateStoragePoolLevel, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateStoragePoolLevelReply{SpId: spId}, nil
}

// FindStoragePoolNames is architecture.md §8.4's FindStoragePoolNames: the
// reverse lookup admin tooling and log analysis need, because keys and device
// names carry sp_id and never sp_name.
//
// It is a Snapshot — one store revision, no commit — so a batch of ids is
// answered from one consistent view rather than from a store that moved
// between two of them. An id whose sp_id_to_name key is absent is simply left
// out of the map: absence IS the answer "no such sp_id", and making it an
// error would force a caller to probe ids one at a time.
func (s *Server) FindStoragePoolNames(
	ctx context.Context,
	req *pb.FindStoragePoolNamesRequest,
) (*pb.FindStoragePoolNamesReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	var found map[uint64]string
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		found = make(map[uint64]string, len(req.GetSpIdList()))
		cid, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		for _, spId := range req.GetSpIdList() {
			name := &pb.SpName{}
			if !stm.Get(model.SpNameKey(cid, spId), name) {
				continue
			}
			found[spId] = name.GetSpName()
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.FindStoragePoolNamesReply{SpIdToName: found}, nil
}

// GrowSlice is architecture.md §8.5's GrowSlice.
//
// The transaction is model.GrowSlice's, not this handler's: the gateway and
// the sp-worker's AR6 auto-grow append a group by the same op, so the rule
// that computes the new group's size, re-validates every pick and writes the
// Group has exactly one implementation. Consequently this handler bumps
// NOTHING — model.GrowSlice bumps SpRev, every leg DN's DnRev and the CnRev of
// every cntlr's CN itself, in the same transaction as the write, and a bump
// here would be a second one.
//
// req.ext_cnt never reaches the model (decision D-E): it is only §8.5's
// exclusivity signal — a data grow states one, a meta grow must not, because
// meta sizes come from the ladder. The size the DN scan must ask for is the
// one model.GrowSlice will itself compute, so it is recomputed here from the
// same inputs: the slice's first data group, or the meta ladder.
//
// poolTotal is math.MaxUint64 (gateway.md §5.4, architecture.md §8.5;
// update_04.md U7 pins it). model.GrowSlice re-applies AR6's
// pending rule, which exists so that a WORKER cannot issue a second grow
// before the primary has reported the first; a user-driven GrowSlice is
// explicit operator intent, and the gateway holds no pool report to judge
// "pending" with, so the rule is disabled by passing a total no group total
// can reach.
//
// The planning pre-reads sit INSIDE the candidate unit: a re-scan that
// re-planned from the same stale slice would ask for the same wrong size for
// ever, so each iteration re-reads what it plans from (all of it outside every
// STM, §5.8).
func (s *Server) GrowSlice(
	ctx context.Context,
	req *pb.GrowSliceRequest,
) (*pb.GrowSliceReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateGrowExclusivity(
		req.GetIsMeta(), req.GetExtCnt(),
	); err != nil {
		return nil, err
	}
	if err := validateNodeSelector(
		"dn_selector", req.GetDnSelector(),
	); err != nil {
		return nil, err
	}
	token := req.GetSpRev().GetRevision()
	var grpId uint64
	err := candidateUnit(ctx, func() error {
		grpId = 0
		// The planning pre-read is ONE read-only snapshot (§5.8) opened with
		// openSp, so the token is checked before any other state check
		// (GW6): a stale client sees ABORTED "stale revision" and never a
		// NOT_FOUND computed against a slice list it has not read.
		//
		// That check is also what keeps a 0 out of model.checkSpRev, which
		// reads 0 as "skip the check entirely" — the worker's mode
		// (gateway.md §2.2 #3). A live SpRev starts at 1, so a token of 0 is
		// a client that sent none, and it has to be refused HERE or the grow
		// would commit with no optimistic-concurrency gate at all (§0 #7).
		var cid uint64
		var cc *pb.ClusterConf
		var conf *pb.SpConf
		var slice *pb.Slice
		snapErr := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				token)
			if err != nil {
				return err
			}
			cid, cc, conf = sc.Cid, sc.Cc, sc.Conf
			if !containsId(conf.GetSliceIdList(), req.GetSliceId()) {
				return errNotFound("slice %d is not in storage pool %q",
					req.GetSliceId(), req.GetSpName())
			}
			sliceKey := model.SliceKey(cid, conf.GetSpId(), req.GetSliceId())
			slice = &pb.Slice{}
			if !stm.Get(sliceKey, slice) {
				// A listed slice whose key is gone is §5.9's ABORTED,
				// exactly as in loadSlices: the SP has lost an invariant key.
				return errAborted("slice key %q is missing", sliceKey)
			}
			return nil
		})
		if snapErr != nil {
			return mapStmErr(snapErr)
		}
		extentSize := model.ResolveDnBinConf(
			cc.GetDnBinConf()).GetExtentSize()
		extCnt := uint64(0)
		if req.GetIsMeta() {
			total := uint64(0)
			for _, grp := range slice.GetMetaGrpList() {
				total += grp.GetExtCnt()
			}
			ladder, ok := model.MetaLadderExtCnt(total, extentSize)
			if !ok {
				// The 16 GiB dm-thin metadata cap is the SP's own permanent
				// ceiling — object state, not exhaustible capacity — so it
				// is FAILED_PRECONDITION (architecture.md §8.5, gateway.md
				// GW7; update_05.md U4 resolved the old GW7-vs-§8.5 conflict
				// this way, and model.GrowSlice's in-STM re-check already
				// maps there).
				return errPrecondition(
					"slice %d has %d meta extents and cannot grow past the "+
						"16 GiB dm-thin metadata cap",
					req.GetSliceId(), total)
			}
			extCnt = ladder
		} else {
			if len(slice.GetDataGrpList()) == 0 {
				return errPrecondition(
					"slice %d has no data group to size a data grow by",
					req.GetSliceId())
			}
			// A data grow adds the slice's original allocation unit, not the
			// caller's ext_cnt (D-E).
			extCnt = slice.GetDataGrpList()[0].GetExtCnt()
		}
		// §8.5 Errors: "RESOURCE_EXHAUSTED ... when any cntlr's CN has
		// free_ext_cnt below the new group's ext_cnt". Every cntlr of the SP
		// stacks the new group, so every one of their CNs reserves it
		// (§6.5), and model.chargeSpCns refuses inside the STM when one
		// cannot — but as an ErrPrecondition, which mapModelErr can only
		// render as FAILED_PRECONDITION. The check is therefore made HERE,
		// where the shortfall still has a name and the §8.5 code.
		//
		// It is a pre-check, not the decision: the STM re-charges every CN
		// from what IT reads, so a CN that lost its budget in between still
		// refuses — just with the other code. Being occasionally generous is
		// the right failure direction for a check whose only job is to give
		// the common case its documented error.
		if err := s.growSliceCnBudget(ctx, cid, conf, extCnt); err != nil {
			return err
		}
		// D-F: the black list is the request's own and nothing is ever
		// appended to it — one scan-and-pick round serves the whole group
		// (§6.5): the scan keeps one candidate per DN and per location, and
		// the random pick draws the group's legs as distinct entries from it,
		// so the new group spreads over distinct DNs while another group's
		// DNs stay allowed. (worker/reaction.go runGrow reaches the same
		// spread its own way, via pickDistinct.) ExcludeLocs stays empty for
		// the same reason: §6.5 leaves GrowSlice out of the two-tier rule — a
		// grow spreads the NEW group, it does not avoid the slice's existing
		// failure domains.
		picks, err := pickDns(
			ctx, s.cli, cid, cc,
			dnPickPlan{
				ExtCnt:      extCnt,
				Legs:        legCntOf(spBdevConf(conf)),
				ExcludeLocs: nil,
			},
			req.GetDnSelector(), nil, "grow slice")
		if err != nil {
			return err
		}
		newGrpId, err := model.GrowSlice(
			ctx, s.cli, cid, conf.GetShardCode(), conf.GetSpId(),
			req.GetSpName(), req.GetSpRev().GetRevision(),
			req.GetSliceId(), req.GetIsMeta(), math.MaxUint64, cc, picks)
		if err != nil {
			return mapModelErr(err)
		}
		grpId = newGrpId
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GrowSliceReply{
		SliceId: req.GetSliceId(),
		GrpId:   grpId,
	}, nil
}
