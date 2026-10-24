package plant

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Storage pools with explicit placement (architecture.md, Storage pools)
// ---------------------------------------------------------------------------

// CntlrSpec places one cntlr of a pool: its id, the CN that hosts it, its
// cntlid slot and whether it is the primary.
type CntlrSpec struct {
	CntlrId uint64
	CnId    uint64
	Slot    uint32
	Primary bool
}

func (c CntlrSpec) String() string {
	return fmt.Sprintf("%#x:%#x:%d:%t", c.CntlrId, c.CnId, c.Slot, c.Primary)
}

// SliceSpec is one slice of a pool and its slice_idx.
type SliceSpec struct {
	SliceId  uint64
	SliceIdx uint32
}

func (s SliceSpec) String() string {
	return fmt.Sprintf("%#x:%d", s.SliceId, s.SliceIdx)
}

// GroupSpec is one group of a slice. The redundancy kind is per group because
// that is what decides the group's meta_blocks (architecture.md, Group on-leg
// layout: meta region, data region, health block); every group of one pool
// must name the same kind, since SpConf.bdev_conf holds exactly one
// redund_conf.
type GroupSpec struct {
	SliceId uint64
	GrpId   uint64
	// Meta puts the group in the slice's meta list, else in its data list.
	Meta   bool
	ExtCnt uint64
	Raid1  bool
}

func (g GroupSpec) String() string {
	kind, redund := "data", "none"
	if g.Meta {
		kind = "meta"
	}
	if g.Raid1 {
		redund = "raid1"
	}
	return fmt.Sprintf("%#x:%#x:%s:%d:%s",
		g.SliceId, g.GrpId, kind, g.ExtCnt, redund)
}

// LegSpec is one leg of a group's leg_list.
type LegSpec struct {
	GrpId  uint64
	LegId  uint64
	LegIdx uint32
}

func (l LegSpec) String() string {
	return fmt.Sprintf("%#x:%#x:%d", l.GrpId, l.LegId, l.LegIdx)
}

// SideSpec places one side of a leg on a DN, with its cntlid slot.
type SideSpec struct {
	LegId  uint64
	SideId uint64
	DnId   uint64
	Slot   uint32
}

func (s SideSpec) String() string {
	return fmt.Sprintf("%#x:%#x:%#x:%d", s.LegId, s.SideId, s.DnId, s.Slot)
}

// SpSpec is the CreateStoragePool transaction with explicit placement: the
// pool and the five lists of what it holds, each item naming its parent by
// id.
type SpSpec struct {
	Name  string
	Id    uint64
	Shard uint32
	// Slots is the cntlid_slot_list; empty means every slot, 0 to
	// common.CnCntlidSlotCnt - 1.
	Slots []uint32
	Level pb.SpLevel
	// Threshold is the event_threshold; nil stores the all-zero one, which
	// reads as every default.
	Threshold *pb.EventThreshold
	// Lwm overrides the low_water_mark_pct the pool inherits from the
	// cluster when it is not nil.
	Lwm    *uint32
	Cntlrs []CntlrSpec
	Slices []SliceSpec
	Groups []GroupSpec
	Legs   []LegSpec
	Sides  []SideSpec
}

// slotList is the cntlid_slot_list the pool stores.
func (spec SpSpec) slotList() []uint32 {
	if len(spec.Slots) > 0 {
		return append([]uint32(nil), spec.Slots...)
	}
	slots := make([]uint32, common.CnCntlidSlotCnt)
	for i := range slots {
		slots[i] = uint32(i)
	}
	return slots
}

// Check refuses a pool whose placement no worker could drive: every dangling
// reference and every duplicate id is a hard error, because a mistyped
// placement would otherwise become an SP nothing can converge.
func (spec SpSpec) Check() error {
	_, err := newSpPlan(spec)
	return err
}

// spPlan is the validated tree an SpSpec describes. The lists name their
// parent by id, so the tree is built by joining on those ids.
type spPlan struct {
	slots  []uint32
	cntlrs []CntlrSpec
	slices []SliceSpec
	groups []GroupSpec
	legs   []LegSpec
	sides  []SideSpec
	// raid1 is the one redundancy kind every group named.
	raid1 bool
	// grpOfLeg and extOfLeg answer "which group owns this leg" and "how many
	// extents does a side of this leg cost".
	grpOfLeg map[uint64]uint64
	extOfLeg map[uint64]uint64
	// footprint is Σ ext_cnt over ALL groups — what one cntlr's CN reserves
	// for the SP (architecture.md, Storage pools and Cntlrs).
	footprint uint64
	// nextId is one past the largest id the plan uses, so that a worker
	// reaction allocates fresh ids ABOVE the caller's.
	nextId uint64
}

// newSpPlan validates an SpSpec and joins its lists into one tree.
func newSpPlan(spec SpSpec) (*spPlan, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("the storage pool name is empty")
	}
	if spec.Id == 0 {
		return nil, fmt.Errorf("sp id 0 is never valid")
	}
	if spec.Shard >= common.ShardBucketSize {
		return nil, fmt.Errorf("shard code %#x is out of range (00..%02x)",
			spec.Shard, common.ShardBucketSize-1)
	}
	if _, ok := pb.SpLevel_name[int32(spec.Level)]; !ok {
		return nil, fmt.Errorf("sp_level %d is not a defined SpLevel",
			int32(spec.Level))
	}
	plan := &spPlan{
		slots:    spec.slotList(),
		grpOfLeg: make(map[uint64]uint64),
		extOfLeg: make(map[uint64]uint64),
	}
	slotSet := make(map[uint32]struct{}, len(plan.slots))
	for _, slot := range plan.slots {
		if slot >= common.CnCntlidSlotCnt {
			return nil, fmt.Errorf("slot %d is >= CnCntlidSlotCnt (%d)",
				slot, common.CnCntlidSlotCnt)
		}
		if _, ok := slotSet[slot]; ok {
			return nil, fmt.Errorf("slot %d appears twice in the slot list",
				slot)
		}
		slotSet[slot] = struct{}{}
	}
	// Every sub-object id must be distinct WITHIN ITS KIND, and next_id ends
	// up past all of them (architecture.md, Globals: id allocation + shard
	// buckets). Uniqueness is not enforced ACROSS kinds even though a real
	// gateway draws all of them from the one SpConf.next_id counter:
	// worker_test.sh labels the objects of one SP per kind
	// (slice `1`, group `G1`, leg `L1`, side `S1`), nothing in model or
	// worker ever looks an id up without knowing its kind, and refusing the
	// numbering the cases are written in would buy nothing.
	ids := make(map[string]map[uint64]struct{})
	claim := func(id uint64, what string) error {
		if id == 0 {
			return fmt.Errorf("%s id 0 is never valid", what)
		}
		if ids[what] == nil {
			ids[what] = make(map[uint64]struct{})
		}
		if _, ok := ids[what][id]; ok {
			return fmt.Errorf("%s id %#x is used twice", what, id)
		}
		ids[what][id] = struct{}{}
		if plan.nextId <= id {
			plan.nextId = id + 1
		}
		return nil
	}

	primaryCnt := 0
	usedSlots := make(map[uint32]uint64)
	usedCns := make(map[uint64]uint64)
	for _, cntlr := range spec.Cntlrs {
		if err := claim(cntlr.CntlrId, "cntlr"); err != nil {
			return nil, err
		}
		if _, ok := slotSet[cntlr.Slot]; !ok {
			return nil, fmt.Errorf(
				"cntlr %s: cntlid slot %d is not in the slot list",
				cntlr, cntlr.Slot,
			)
		}
		if prev, ok := usedSlots[cntlr.Slot]; ok {
			return nil, fmt.Errorf(
				"cntlr %s: cntlid slot %d already taken by cntlr %#x "+
					"(architecture.md, cntlid slots, wants them distinct)",
				cntlr, cntlr.Slot, prev,
			)
		}
		usedSlots[cntlr.Slot] = cntlr.CntlrId
		if prev, ok := usedCns[cntlr.CnId]; ok {
			return nil, fmt.Errorf(
				"cntlr %s: cn %#x already hosts a cntlr of this sp, "+
					"%#x (architecture.md, Finding CN candidates)",
				cntlr, cntlr.CnId, prev,
			)
		}
		usedCns[cntlr.CnId] = cntlr.CntlrId
		if cntlr.Primary {
			primaryCnt++
		}
		plan.cntlrs = append(plan.cntlrs, cntlr)
	}
	if len(plan.cntlrs) == 0 {
		return nil, fmt.Errorf("at least one cntlr is required")
	}
	// Each cntlr holds a slot of the list that no other cntlr holds, so the
	// list is never shorter than the cntlrs and needs no check of its own.
	if primaryCnt != 1 {
		return nil, fmt.Errorf(
			"exactly one cntlr must be primary, got %d", primaryCnt,
		)
	}

	sliceIdx := make(map[uint32]uint64)
	knownSlices := make(map[uint64]struct{})
	for _, slice := range spec.Slices {
		if err := claim(slice.SliceId, "slice"); err != nil {
			return nil, err
		}
		if prev, ok := sliceIdx[slice.SliceIdx]; ok {
			return nil, fmt.Errorf(
				"slice %s: slice_idx %d already used by slice %#x",
				slice, slice.SliceIdx, prev,
			)
		}
		sliceIdx[slice.SliceIdx] = slice.SliceId
		knownSlices[slice.SliceId] = struct{}{}
		plan.slices = append(plan.slices, slice)
	}
	if len(plan.slices) == 0 {
		return nil, fmt.Errorf("at least one slice is required")
	}

	knownGroups := make(map[uint64]GroupSpec)
	for i, grp := range spec.Groups {
		if err := claim(grp.GrpId, "group"); err != nil {
			return nil, err
		}
		if grp.ExtCnt == 0 {
			return nil, fmt.Errorf("group %s: ext_cnt must not be zero", grp)
		}
		if _, ok := knownSlices[grp.SliceId]; !ok {
			return nil, fmt.Errorf(
				"group %s: no slice with id %#x", grp, grp.SliceId,
			)
		}
		if i == 0 {
			plan.raid1 = grp.Raid1
		} else if grp.Raid1 != plan.raid1 {
			return nil, fmt.Errorf(
				"group %s: every group of one sp must name the same "+
					"redundancy (SpConf.bdev_conf holds one redund_conf)",
				grp,
			)
		}
		knownGroups[grp.GrpId] = grp
		plan.footprint += grp.ExtCnt
		plan.groups = append(plan.groups, grp)
	}
	if len(plan.groups) == 0 {
		return nil, fmt.Errorf("at least one group is required")
	}

	legIdx := make(map[uint64]map[uint32]uint64)
	grpLegCnt := make(map[uint64]int)
	for _, leg := range spec.Legs {
		if err := claim(leg.LegId, "leg"); err != nil {
			return nil, err
		}
		grp, ok := knownGroups[leg.GrpId]
		if !ok {
			return nil, fmt.Errorf(
				"leg %s: no group with id %#x", leg, leg.GrpId,
			)
		}
		if legIdx[leg.GrpId] == nil {
			legIdx[leg.GrpId] = make(map[uint32]uint64)
		}
		if prev, ok := legIdx[leg.GrpId][leg.LegIdx]; ok {
			return nil, fmt.Errorf(
				"leg %s: leg_idx %d already used by leg %#x in group %#x",
				leg, leg.LegIdx, prev, leg.GrpId,
			)
		}
		legIdx[leg.GrpId][leg.LegIdx] = leg.LegId
		grpLegCnt[leg.GrpId]++
		plan.grpOfLeg[leg.LegId] = leg.GrpId
		plan.extOfLeg[leg.LegId] = grp.ExtCnt
		plan.legs = append(plan.legs, leg)
	}
	for _, grp := range plan.groups {
		if grpLegCnt[grp.GrpId] == 0 {
			return nil, fmt.Errorf("group %#x has no leg", grp.GrpId)
		}
	}

	legSideCnt := make(map[uint64]int)
	grpDns := make(map[uint64]map[uint64]uint64)
	for _, side := range spec.Sides {
		if err := claim(side.SideId, "side"); err != nil {
			return nil, err
		}
		grpId, ok := plan.grpOfLeg[side.LegId]
		if !ok {
			return nil, fmt.Errorf(
				"side %s: no leg with id %#x", side, side.LegId,
			)
		}
		if _, ok := slotSet[side.Slot]; !ok {
			return nil, fmt.Errorf(
				"side %s: cntlid slot %d is not in the slot list",
				side, side.Slot,
			)
		}
		if grpDns[grpId] == nil {
			grpDns[grpId] = make(map[uint64]uint64)
		}
		if prev, ok := grpDns[grpId][side.DnId]; ok {
			return nil, fmt.Errorf(
				"side %s: dn %#x already carries side %#x of group %#x "+
					"(two legs of one group on one node have no redundancy)",
				side, side.DnId, prev, grpId,
			)
		}
		grpDns[grpId][side.DnId] = side.SideId
		legSideCnt[side.LegId]++
		plan.sides = append(plan.sides, side)
	}
	for _, leg := range plan.legs {
		if legSideCnt[leg.LegId] == 0 {
			return nil, fmt.Errorf("leg %#x has no side", leg.LegId)
		}
	}
	return plan, nil
}

// GroupGeometry is the block geometry PutSp stored for one group
// (architecture.md, Group on-leg layout: meta region, data region, health
// block).
type GroupGeometry struct {
	GrpId      uint64
	SliceId    uint64
	Meta       bool
	ExtCnt     uint64
	MetaBlocks uint64
	DataBlocks uint64
}

// Sp is one planted storage pool: the reference every later call takes, the
// next_id it stored, the CN footprint, the revision its SpRev starts at, the
// geometry of every group in the spec's order, and the revision each node
// it charged went to, keyed by the node's endpoint.
type Sp struct {
	Ref         SpRef
	NextId      uint64
	Footprint   uint64
	Revision    uint64
	Groups      []GroupGeometry
	DnRevisions map[string]uint64
	CnRevisions map[string]uint64
}

// PutSp implements the CreateStoragePool STM with EXPLICIT PLACEMENT
// (dnv-worker.md, Integration test plan, The driver; architecture.md, Storage
// pools). Everything CreateStoragePool's Action step 2 does is done here,
// with the allocator of architecture.md, Per-operation allocation, replaced
// by the spec's lists: SpConf (next_id past every id used, next_dev_id 1,
// bdev_conf inherited from ClusterConf, event_threshold, cntlid_slot_list,
// sp_level, deleting = false, every id list filled), SpName, one Cntlr per
// cntlr, one Slice per slice with its groups → legs → sides
// (provisioned = false, [D15]), the SpRev key with revision = 1, SpGlobal,
// and the DN/CN bookkeeping: every DN gains its side pointers, loses the
// group's ext_cnt per side, has its capacity key maintained (architecture.md,
// Capacity index keys) and its DnRev bumped ONCE; every CN gains its cntlr
// pointer, loses the SP footprint, and likewise. It writes no CdcEntry;
// PutSs does.
//
// The whole pool is one transaction, as CreateStoragePool's is, so a pool
// whose sides sit on more DNs than etcd's limit on operations per
// transaction allows is refused by etcd: each DN costs six compares, its
// conf and its revision read and written and its capacity key moved.
func PutSp(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	spec SpSpec,
) (Sp, error) {
	plan, err := newSpPlan(spec)
	if err != nil {
		return Sp{}, err
	}
	cid, cc := cl.Id, cl.Conf
	spId, shardCode := spec.Id, spec.Shard
	threshold := &pb.EventThreshold{}
	if spec.Threshold != nil {
		threshold = proto.Clone(spec.Threshold).(*pb.EventThreshold)
	}

	// The SP's bdev_conf is the cluster's, with the redundancy the groups
	// name and the Lwm override (architecture.md, Storage pools:
	// CreateStoragePool takes bdev_conf member-wise from
	// ClusterConf.bdev_conf, then from the constants).
	bdev := cloneBdevConf(cc.GetBdevConf())
	if bdev.DmPoolConf == nil {
		bdev.DmPoolConf = &pb.DmPoolConf{}
	}
	if spec.Lwm != nil {
		bdev.DmPoolConf.LowWaterMarkPct = *spec.Lwm
	}
	chunkBlocks := bdev.GetRedundConf().GetRedundMdRaid1().
		GetBitmapChunkBlockCnt()
	bdev.RedundConf = RedundConf(plan.raid1, chunkBlocks)
	// The gateway's resolve-at-write, mirrored (architecture.md, Common
	// validation): CreateStoragePool stores the merge passed through
	// model.ResolveBdevConf, so PutSp does too — the cluster's bdev_conf is
	// already concrete, but the SP's redund kind was just rebuilt above and
	// the merge's own constants rung still has to fire.
	bdev = model.ResolveBdevConf(bdev)
	if err := model.ValidateClusterConf(cc); err != nil {
		return Sp{}, err
	}
	extentSize := cc.GetDnBinConf().GetExtentSize()

	// The geometry of every group (architecture.md, Group on-leg layout:
	// meta region, data region, health block), computed by model.GroupBlocks
	// — the one implementation of the formula (MD6).
	geometry := make(map[uint64]GroupGeometry, len(plan.groups))
	groups := make([]GroupGeometry, 0, len(plan.groups))
	for _, grp := range plan.groups {
		metaBlocks, dataBlocks, err := model.GroupBlocks(
			grp.ExtCnt, extentSize, bdev,
		)
		if err != nil {
			return Sp{}, fmt.Errorf("group %#x: %w", grp.GrpId, err)
		}
		geo := GroupGeometry{
			GrpId:      grp.GrpId,
			SliceId:    grp.SliceId,
			Meta:       grp.Meta,
			ExtCnt:     grp.ExtCnt,
			MetaBlocks: metaBlocks,
			DataBlocks: dataBlocks,
		}
		geometry[grp.GrpId] = geo
		groups = append(groups, geo)
	}

	// Resolve every named node to the endpoint its record is keyed by. The
	// STM re-reads each record and re-checks the id, so a node that moved
	// between the scan and the transaction fails the put instead of
	// corrupting the placement.
	dnAddr := make(map[uint64]string)
	dnOrder := make([]string, 0, len(plan.sides))
	dnCharge := make(map[string]uint64)
	dnPtrs := make(map[string][]*pb.SidePointer)
	dnIdOf := make(map[string]uint64)
	for _, side := range plan.sides {
		addr, ok := dnAddr[side.DnId]
		if !ok {
			addr, err = DnAddr(ctx, cli, cl, side.DnId)
			if err != nil {
				return Sp{}, err
			}
			dnAddr[side.DnId] = addr
			dnIdOf[addr] = side.DnId
			dnOrder = append(dnOrder, addr)
		}
		dnCharge[addr] += plan.extOfLeg[side.LegId]
		dnPtrs[addr] = append(dnPtrs[addr], &pb.SidePointer{
			SpId:   spId,
			LegId:  side.LegId,
			SideId: side.SideId,
		})
	}
	cnAddr := make(map[uint64]string)
	cnOrder := make([]string, 0, len(plan.cntlrs))
	cnPtrs := make(map[string][]*pb.CntlrPointer)
	cnIdOf := make(map[string]uint64)
	for _, cntlr := range plan.cntlrs {
		addr, ok := cnAddr[cntlr.CnId]
		if !ok {
			addr, err = CnAddr(ctx, cli, cl, cntlr.CnId)
			if err != nil {
				return Sp{}, err
			}
			cnAddr[cntlr.CnId] = addr
			cnIdOf[addr] = cntlr.CnId
			cnOrder = append(cnOrder, addr)
		}
		cnPtrs[addr] = append(cnPtrs[addr], &pb.CntlrPointer{
			SpId:    spId,
			CntlrId: cntlr.CntlrId,
		})
	}

	var dnRevisions, cnRevisions map[string]uint64
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		dnRevisions = make(map[string]uint64)
		cnRevisions = make(map[string]uint64)
		confKey := model.SpConfKey(cid, spec.Name)
		if s.Get(confKey, &pb.SpConf{}) {
			return fmt.Errorf("%q already exists", confKey)
		}
		nameKey := model.SpNameKey(cid, spId)
		if s.Get(nameKey, &pb.SpName{}) {
			return fmt.Errorf("%q already exists", nameKey)
		}
		// --- the nodes, read once and used for both the copies and the
		// bookkeeping ---
		dns := make(map[string]*pb.DnConf, len(dnOrder))
		for _, addr := range dnOrder {
			dn := &pb.DnConf{}
			if !s.Get(model.DnConfKey(cid, addr), dn) {
				return fmt.Errorf("%q not found", model.DnConfKey(cid, addr))
			}
			if dn.GetDnId() != dnIdOf[addr] {
				return fmt.Errorf(
					"%q holds dn %#x, want %#x",
					addr, dn.GetDnId(), dnIdOf[addr],
				)
			}
			dns[addr] = dn
		}
		cns := make(map[string]*pb.CnConf, len(cnOrder))
		for _, addr := range cnOrder {
			cn := &pb.CnConf{}
			if !s.Get(model.CnConfKey(cid, addr), cn) {
				return fmt.Errorf("%q not found", model.CnConfKey(cid, addr))
			}
			if cn.GetCnId() != cnIdOf[addr] {
				return fmt.Errorf(
					"%q holds cn %#x, want %#x",
					addr, cn.GetCnId(), cnIdOf[addr],
				)
			}
			cns[addr] = cn
		}
		// --- the slices, built by joining the lists on their ids ---
		legs := make(map[uint64]*pb.Leg, len(plan.legs))
		grps := make(map[uint64]*pb.Group, len(plan.groups))
		slices := make(map[uint64]*pb.Slice, len(plan.slices))
		sliceIds := make([]uint64, 0, len(plan.slices))
		for _, slice := range plan.slices {
			slices[slice.SliceId] = &pb.Slice{SliceIdx: slice.SliceIdx}
			sliceIds = append(sliceIds, slice.SliceId)
		}
		for _, g := range plan.groups {
			geo := geometry[g.GrpId]
			grp := &pb.Group{
				GrpId:      g.GrpId,
				ExtCnt:     g.ExtCnt,
				MetaBlocks: geo.MetaBlocks,
				DataBlocks: geo.DataBlocks,
			}
			grps[g.GrpId] = grp
			slice := slices[g.SliceId]
			if g.Meta {
				slice.MetaGrpList = append(slice.MetaGrpList, grp)
			} else {
				slice.DataGrpList = append(slice.DataGrpList, grp)
			}
		}
		for _, l := range plan.legs {
			leg := &pb.Leg{LegId: l.LegId, LegIdx: l.LegIdx}
			legs[l.LegId] = leg
			grp := grps[l.GrpId]
			grp.LegList = append(grp.LegList, leg)
		}
		for _, sd := range plan.sides {
			addr := dnAddr[sd.DnId]
			leg := legs[sd.LegId]
			leg.SideList = append(leg.SideList, &pb.Side{
				SideId:      sd.SideId,
				AddrPort:    addr,
				CntlidSlot:  sd.Slot,
				NvmeTrConf:  dns[addr].GetNvmeTrConf(),
				ErrEpoch:    0,
				Provisioned: false,
			})
		}
		for _, sliceId := range sliceIds {
			s.Put(model.SliceKey(cid, spId, sliceId), slices[sliceId])
		}
		// --- the cntlrs ---
		cntlrIds := make([]uint64, 0, len(plan.cntlrs))
		for _, c := range plan.cntlrs {
			addr := cnAddr[c.CnId]
			s.Put(model.CntlrKey(cid, spId, c.CntlrId), &pb.Cntlr{
				AddrPort:   addr,
				NvmeTrConf: cns[addr].GetNvmeTrConf(),
				CntlidSlot: c.Slot,
				Primary:    c.Primary,
				Disabled:   false,
				ErrEpoch:   0,
				// As CreateStoragePool: the primary is created settling
				// (dnv-worker.md HL2 and Integration test plan, The driver).
				Settling: c.Primary,
			})
			cntlrIds = append(cntlrIds, c.CntlrId)
		}
		// --- SpConf, SpName, SpRev ---
		s.Put(confKey, &pb.SpConf{
			SpId:           spId,
			ShardCode:      shardCode,
			NextId:         plan.nextId,
			NextDevId:      1,
			BdevConf:       bdev,
			EventThreshold: threshold,
			CntlidSlotList: plan.slots,
			SpLevel:        spec.Level,
			Deleting:       false,
			CntlrIdList:    cntlrIds,
			SliceIdList:    sliceIds,
		})
		s.Put(nameKey, &pb.SpName{SpName: spec.Name})
		s.Put(model.SpRevKey(shardCode, cid, spId), &pb.SpRev{
			SpName:   spec.Name,
			Revision: 1,
		})
		// --- the DN bookkeeping, once per DN (architecture.md, Storage
		// pools; Capacity index keys; Revision keys and the sync fan-out) ---
		for _, addr := range dnOrder {
			revision, err := chargeDn(s, cid, cc, addr, dns[addr],
				dnPtrs[addr], dnCharge[addr], 0)
			if err != nil {
				return err
			}
			dnRevisions[addr] = revision
		}
		// --- the CN bookkeeping, once per CN (architecture.md, Storage
		// pools and Cntlrs) ---
		for _, addr := range cnOrder {
			cn := cns[addr]
			if cn.GetFreeExtCnt() < plan.footprint {
				return fmt.Errorf(
					"cn %#x at %q has free_ext_cnt %d, needs the sp "+
						"footprint %d",
					cn.GetCnId(), addr, cn.GetFreeExtCnt(), plan.footprint,
				)
			}
			newCn := proto.Clone(cn).(*pb.CnConf)
			newCn.CntlrPtrList = append(newCn.CntlrPtrList, cnPtrs[addr]...)
			newCn.FreeExtCnt -= plan.footprint
			s.Put(model.CnConfKey(cid, addr), newCn)
			model.MaintainCnCapacity(s, cid, addr, cn, newCn)
			revision, err := bumpCnRev(s, cid, newCn)
			if err != nil {
				return err
			}
			cnRevisions[addr] = revision
		}
		// --- SpGlobal (architecture.md, Globals: id allocation + shard
		// buckets) ---
		globalKey := model.SpGlobalKey(cid)
		global := &pb.SpGlobal{}
		if s.Get(globalKey, global) {
			if global.NextId <= spId {
				global.NextId = spId + 1
			}
			if int(shardCode) < len(global.ShardBucket) {
				global.ShardBucket[shardCode]++
			}
			s.Put(globalKey, global)
		}
		return nil
	})
	if err != nil {
		return Sp{}, err
	}
	return Sp{
		Ref:         SpRef{Name: spec.Name, Id: spId, Shard: shardCode},
		NextId:      plan.nextId,
		Footprint:   plan.footprint,
		Revision:    1,
		Groups:      groups,
		DnRevisions: dnRevisions,
		CnRevisions: cnRevisions,
	}, nil
}

// ---------------------------------------------------------------------------
// Naming a planted pool
// ---------------------------------------------------------------------------

// SpRef names one storage pool for the calls that write inside it: SpConf is
// keyed by name, and the id and shard code address its other keys. Every
// such call re-reads the SpConf in its own transaction and refuses it when its
// id changed (getSpConf).
type SpRef struct {
	Name  string
	Id    uint64
	Shard uint32
}

// ResolveSp turns a pool's name or sp_id, decimal or 0x hex, into an SpRef.
// A value that parses as a number is looked up through the sp_id_to_name
// reverse key first; a value that does not, or an id with no reverse key, is
// taken as a name.
func ResolveSp(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	spec string,
) (SpRef, error) {
	name := strings.TrimSpace(spec)
	if name == "" {
		return SpRef{}, fmt.Errorf("the storage pool is not named")
	}
	if id, err := strconv.ParseUint(name, 0, 64); err == nil {
		reverse := &pb.SpName{}
		found, err := cli.Get(ctx, model.SpNameKey(cl.Id, id), reverse)
		if err != nil {
			return SpRef{}, err
		}
		if found {
			name = reverse.GetSpName()
		}
	}
	conf := &pb.SpConf{}
	found, err := cli.Get(ctx, model.SpConfKey(cl.Id, name), conf)
	if err != nil {
		return SpRef{}, err
	}
	if !found {
		return SpRef{}, fmt.Errorf("no storage pool %q in cluster %s",
			spec, fmt.Sprintf(common.IdKeyFmt, cl.Id))
	}
	return SpRef{
		Name:  name,
		Id:    conf.GetSpId(),
		Shard: conf.GetShardCode(),
	}, nil
}

// getSpConf re-reads an SP inside an STM and re-checks its identity, the way
// model does for the worker's ops (AR3): the SpConf is name-keyed, so an SP
// deleted and re-created under the same name between the resolve and this
// transaction is a different SP and none of the ids the caller carries mean
// anything in it.
func getSpConf(
	s etcdutil.STM,
	cid uint64,
	target SpRef,
) (*pb.SpConf, error) {
	conf := &pb.SpConf{}
	if !s.Get(model.SpConfKey(cid, target.Name), conf) {
		return nil, fmt.Errorf("storage pool %q not found", target.Name)
	}
	if conf.GetSpId() != target.Id {
		return nil, fmt.Errorf(
			"storage pool %q changed its id: %#x, want %#x",
			target.Name, conf.GetSpId(), target.Id,
		)
	}
	return conf, nil
}

// readSpRevIn is SpRevision inside a caller's STM, for the paths that must
// not issue a second round trip after a decision.
func readSpRevIn(s etcdutil.STM, cid uint64, target SpRef) uint64 {
	rev := &pb.SpRev{}
	if !s.Get(model.SpRevKey(target.Shard, cid, target.Id), rev) {
		return 0
	}
	return rev.GetRevision()
}

// SpRevision reads a pool's revision, the token a flip invalidates, so a
// caller refreshes it without reading the key itself.
func SpRevision(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
) (uint64, error) {
	rev := &pb.SpRev{}
	found, err := cli.Get(
		ctx, model.SpRevKey(target.Shard, cl.Id, target.Id), rev)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("sp_rev of %q not found", target.Name)
	}
	return rev.GetRevision(), nil
}
