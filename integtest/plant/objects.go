package plant

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Thin devices (architecture.md, Thin devices)
// ---------------------------------------------------------------------------

// TdSpec is one thin device to plant.
type TdSpec struct {
	Name string
	// Id is the td_id; 0 takes the pool's next free one.
	Id uint64
	// DevId is the thin dev_id; 0 takes SpConf.next_dev_id.
	DevId uint32
	// Size is ThinDevice.size in bytes.
	Size uint64
}

// Check refuses a thin device no key could be written for.
func (spec TdSpec) Check() error {
	if spec.Name == "" {
		return fmt.Errorf("the td name is empty")
	}
	return nil
}

// Td is one planted thin device and the pool revision its transaction wrote.
type Td struct {
	Name  string
	Id    uint64
	DevId uint32
	SpRev uint64
}

// PutTd writes one ThinDevice and its SpConf.td_name_list entry
// (architecture.md, Thin devices: CreateThinDevice), then bumps SpRev once.
func PutTd(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec TdSpec,
) (Td, error) {
	tds, err := PutTds(ctx, cli, cl, target, []TdSpec{spec})
	if err != nil {
		return Td{}, err
	}
	return tds[0], nil
}

// PutTds is PutTd for a list, in order, tdsPerTxn thin devices per
// transaction, each transaction bumping SpRev once. A failure leaves the
// transactions before it committed and returns the thin devices they wrote.
func PutTds(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	specs []TdSpec,
) ([]Td, error) {
	for i, spec := range specs {
		if err := spec.Check(); err != nil {
			return nil, fmt.Errorf("td %d: %w", i, err)
		}
	}
	cid := cl.Id
	out := make([]Td, 0, len(specs))
	for start := 0; start < len(specs); start += tdsPerTxn {
		batch := specs[start:min(start+tdsPerTxn, len(specs))]
		var written []Td
		err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
			written = written[:0]
			conf, err := getSpConf(s, cid, target)
			if err != nil {
				return err
			}
			for _, spec := range batch {
				if containsName(conf.GetTdNameList(), spec.Name) {
					return fmt.Errorf("td %q already exists", spec.Name)
				}
				key := model.ThinDeviceKey(cid, target.Id, spec.Name)
				if s.Get(key, &pb.ThinDevice{}) {
					return fmt.Errorf("%q already exists", key)
				}
				devId := spec.DevId
				if devId == 0 {
					devId = conf.GetNextDevId()
				}
				if conf.NextDevId <= devId {
					conf.NextDevId = devId + 1
				}
				tdId, err := takeId(conf, "PutTds", spec.Id)
				if err != nil {
					return err
				}
				s.Put(key, &pb.ThinDevice{
					TdId:    tdId,
					DevId:   devId,
					OriId:   0,
					Size:    spec.Size,
					Created: false,
				})
				conf.TdNameList = append(conf.TdNameList, spec.Name)
				written = append(written, Td{
					Name: spec.Name, Id: tdId, DevId: devId,
				})
			}
			s.Put(model.SpConfKey(cid, target.Name), conf)
			spRev, err := bumpSpRev(s, cid, conf)
			if err != nil {
				return err
			}
			for i := range written {
				written[i].SpRev = spRev
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		out = append(out, written...)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Subsystems and namespaces (architecture.md, Subsystems, namespaces)
// ---------------------------------------------------------------------------

// NsSpec is one namespace of a subsystem.
type NsSpec struct {
	// NsId is the ns_id; 0 takes the pool's next free one.
	NsId  uint64
	NsIdx uint32
	TdId  uint64
}

// SsSpec is one subsystem to plant, with its namespaces.
type SsSpec struct {
	Nqn string
	// Id is the ss_id; 0 takes the pool's next free one.
	Id         uint64
	Serial     string
	Model      string
	Namespaces []NsSpec
}

// Check refuses a subsystem no key could be written for, and a namespace
// index no host could address: ns_idx 0 is never valid, and two namespaces
// of one subsystem never share one.
func (spec SsSpec) Check() error {
	if spec.Nqn == "" {
		return fmt.Errorf("the subsystem nqn is empty")
	}
	seenIdx := make(map[uint32]uint64)
	for _, ns := range spec.Namespaces {
		if ns.NsIdx == 0 {
			return fmt.Errorf("ns %#x: ns_idx 0 is never valid", ns.NsId)
		}
		if prev, ok := seenIdx[ns.NsIdx]; ok {
			return fmt.Errorf("ns %#x: ns_idx %d already used by ns %#x",
				ns.NsId, ns.NsIdx, prev)
		}
		seenIdx[ns.NsIdx] = ns.NsId
	}
	return nil
}

// Ss is one planted subsystem: its id, its namespaces' ids in the spec's
// order, the key of the CdcEntry it wrote and the pool revision.
type Ss struct {
	Nqn    string
	Id     uint64
	NsIds  []uint64
	CdcKey string
	SpRev  uint64
}

// PutSs writes one Subsystem, its SpConf.nqn_list entry and the SP's CdcEntry
// (architecture.md, Subsystems, namespaces: CreateSubsystem; and dnv-cdc):
// the discovery entry is keyed by the SP's shard code and lists the transport
// of every cntlr the listing rule lists (architecture.md [D18]), and it is
// what the worker suite's case D3 reads back after a cntlr replacement and
// case H after every failover.
func PutSs(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec SsSpec,
) (Ss, error) {
	if err := spec.Check(); err != nil {
		return Ss{}, err
	}
	cid := cl.Id
	var out Ss
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Ss{Nqn: spec.Nqn}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetNqnList(), spec.Nqn) {
			return fmt.Errorf("subsystem %q already exists", spec.Nqn)
		}
		key := model.SubsystemKey(cid, target.Id, spec.Nqn)
		if s.Get(key, &pb.Subsystem{}) {
			return fmt.Errorf("%q already exists", key)
		}
		out.Id, err = takeId(conf, "PutSs", spec.Id)
		if err != nil {
			return err
		}
		subsystem := &pb.Subsystem{
			SsId:   out.Id,
			Serial: spec.Serial,
			Model:  spec.Model,
		}
		for _, ns := range spec.Namespaces {
			nsId, err := takeId(conf, "PutSs", ns.NsId)
			if err != nil {
				return err
			}
			uuid, nguid := nsIdentity(cid, target.Id, nsId)
			subsystem.NsList = append(subsystem.NsList, &pb.Namespace{
				NsId:      nsId,
				NsIdx:     ns.NsIdx,
				TdId:      ns.TdId,
				DevUuid:   uuid,
				DevNguid:  nguid,
				Suspended: false,
			})
			out.NsIds = append(out.NsIds, nsId)
		}
		s.Put(key, subsystem)
		// The discovery entry lists what the listing rule lists
		// (architecture.md [D18]), as CreateSubsystem writes it.
		cntlrs := make([]*pb.Cntlr, 0, len(conf.GetCntlrIdList()))
		for _, cntlrId := range conf.GetCntlrIdList() {
			cntlr := &pb.Cntlr{}
			if !s.Get(model.CntlrKey(cid, target.Id, cntlrId), cntlr) {
				return fmt.Errorf("cntlr %#x not found", cntlrId)
			}
			cntlrs = append(cntlrs, cntlr)
		}
		out.CdcKey = model.CdcEntryKey(
			cid, conf.GetShardCode(), target.Id, out.Id,
		)
		s.Put(out.CdcKey, &pb.CdcEntry{
			Nqn:            spec.Nqn,
			NvmeTrConfList: model.CdcTrConfList(cntlrs),
		})
		conf.NqnList = append(conf.NqnList, spec.Nqn)
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Ss{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Clones (architecture.md, Clones)
// ---------------------------------------------------------------------------

// CloneLegSpec places one leg of a clone's clone meta group and the leg's
// one side on a DN, with the side's cntlid slot. A Spare leg goes to the
// group's spare_leg_list and needs a pool of md-raid1 groups, its leg_idx
// sharing the group's index space with the active legs. Every side is
// planted unprovisioned, so two or more spares in one group make a shape no
// gateway writes, for LegSpec's reason: model.CreateCloneSpareLeg refuses a
// new spare while a spare of the group still has an unprovisioned side. A
// case that wants a shape the gateway can reach flips, with
// SetCloneProvisioned, the sides of every spare of the group but at most one.
type CloneLegSpec struct {
	// LegId and SideId are the leg's and its side's ids; 0 takes the pool's
	// next free one.
	LegId  uint64
	LegIdx uint32
	Spare  bool
	SideId uint64
	DnId   uint64
	Slot   uint32
}

func (l CloneLegSpec) String() string {
	spare := ""
	if l.Spare {
		spare = ":spare"
	}
	return fmt.Sprintf("%#x:%d:%#x:%#x:%d%s",
		l.LegId, l.LegIdx, l.SideId, l.DnId, l.Slot, spare)
}

// CloneSpec is one clone to plant, with its clone meta group
// (architecture.md, Terminology and object model) placed as the caller
// chooses.
type CloneSpec struct {
	Name string
	// Id is the clone_id; 0 takes the pool's next free one.
	Id uint64
	// DstTd is the destination td's name, or its td_id in decimal or 0x hex.
	DstTd         string
	SrcSliceCnt   uint32
	SrcNqn        string
	SrcNsIdx      uint32
	SrcStripeSize uint64
	SrcBlockSize  uint64
	// GrpId is the clone meta group's grp_id; 0 takes the pool's next free
	// one.
	GrpId uint64
	// Legs places the group's legs, one side each. No leg plants a clone
	// with no group, a shape CreateClone never writes, which no clone-keyed
	// op takes and the clone drain deletes all the same (dnv-worker.md
	// MD6).
	Legs []CloneLegSpec
}

// Check refuses a clone no key could be written for, and a clone meta group
// placed in a way no gateway places one: a leg on dn 0, a leg id, a side id
// or a leg_idx used twice, no leg that is not a spare, or two legs on one DN,
// which leave the group no redundancy. A GrpId with no leg is refused too:
// it names a group the clone would not get.
func (spec CloneSpec) Check() error {
	if spec.Name == "" {
		return fmt.Errorf("the clone name is empty")
	}
	if spec.DstTd == "" {
		return fmt.Errorf("the clone's destination td is not named")
	}
	if len(spec.Legs) == 0 {
		if spec.GrpId != 0 {
			return fmt.Errorf("clone %q: group %#x has no leg",
				spec.Name, spec.GrpId)
		}
		return nil
	}
	legIds := make(map[uint64]bool, len(spec.Legs))
	sideIds := make(map[uint64]bool, len(spec.Legs))
	legIdxs := make(map[uint32]bool, len(spec.Legs))
	dnIds := make(map[uint64]bool, len(spec.Legs))
	active := 0
	for _, leg := range spec.Legs {
		switch {
		case leg.DnId == 0:
			return fmt.Errorf("clone %q: leg %s: dn 0 is never valid",
				spec.Name, leg)
		case leg.LegId != 0 && legIds[leg.LegId]:
			return fmt.Errorf("clone %q: leg id %#x is used twice",
				spec.Name, leg.LegId)
		case leg.SideId != 0 && sideIds[leg.SideId]:
			return fmt.Errorf("clone %q: side id %#x is used twice",
				spec.Name, leg.SideId)
		case legIdxs[leg.LegIdx]:
			return fmt.Errorf("clone %q: leg_idx %d is used twice",
				spec.Name, leg.LegIdx)
		case dnIds[leg.DnId]:
			return fmt.Errorf(
				"clone %q: dn %#x carries two legs of the group "+
					"(two legs of one group on one node have no redundancy)",
				spec.Name, leg.DnId)
		}
		if leg.LegId != 0 {
			legIds[leg.LegId] = true
		}
		if leg.SideId != 0 {
			sideIds[leg.SideId] = true
		}
		legIdxs[leg.LegIdx] = true
		dnIds[leg.DnId] = true
		if !leg.Spare {
			active++
		}
	}
	if active == 0 {
		return fmt.Errorf("clone %q: the group has no leg that is not a spare",
			spec.Name)
	}
	return nil
}

// Clone is one planted clone: its id, its destination td's id, and its clone
// meta group's — the group's id, its legs' ids and their sides', both in the
// spec's order, its geometry and the revision each DN it charged went to,
// keyed by the DN's endpoint. A clone with no group has zero and empty ones.
type Clone struct {
	Name        string
	Id          uint64
	DstTdId     uint64
	GrpId       uint64
	LegIds      []uint64
	SideIds     []uint64
	MetaBlocks  uint64
	DataBlocks  uint64
	DnRevisions map[string]uint64
	SpRev       uint64
}

// opPutClone names PutClone in a refusal of model.TakeSpIds.
const opPutClone = "PutClone"

// PutClone writes one Clone and its SpConf.clone_name_list entry
// (architecture.md, Clones), then bumps SpRev once.
//
// It writes the clone's clone meta group with the placement the spec gives
// (putCloneGrp) and charges its DNs as CreateClone charges a clone's
// (architecture.md, Clones): a group of common.CloneMetaGrpExtCnt extents
// laid out by model.GroupBlocks, every side unprovisioned, and each DN
// charged once through chargeDn, its pointer in, the extents off, its
// capacity key moved and its DnRev bumped, and no metadata unit, which a
// clone meta side never takes. No CN is charged, as CreateClone charges
// none. The clone drain's last step, model.FinishCloneDelete, gives back
// exactly the sides, so a planted clone that drains leaves its nodes' counts
// where it found them.
// The zero ids are taken as CreateClone mints them: the clone's, the
// group's, then each leg's before its side's, in the spec's order of legs.
func PutClone(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec CloneSpec,
) (Clone, error) {
	if err := spec.Check(); err != nil {
		return Clone{}, err
	}
	// The group's DNs are resolved to the endpoints their records are keyed
	// by outside the STM, as PutSp resolves a pool's; the STM re-reads each
	// record and re-checks its id. The extent size the group is laid out by
	// is the cluster's stored conf, gated as PutSp gates it.
	addrs := make([]string, 0, len(spec.Legs))
	for _, leg := range spec.Legs {
		addr, err := DnAddr(ctx, cli, cl, leg.DnId)
		if err != nil {
			return Clone{}, err
		}
		addrs = append(addrs, addr)
	}
	if len(spec.Legs) > 0 {
		if err := model.ValidateClusterConf(cl.Conf); err != nil {
			return Clone{}, err
		}
	}
	cid := cl.Id
	var out Clone
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Clone{Name: spec.Name, DnRevisions: make(map[string]uint64)}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetCloneNameList(), spec.Name) {
			return fmt.Errorf("clone %q already exists", spec.Name)
		}
		out.DstTdId, err = plantedCloneTd(s, cid, conf, spec.DstTd)
		if err != nil {
			return err
		}
		key := model.CloneKey(cid, target.Id, spec.Name)
		if s.Get(key, &pb.Clone{}) {
			return fmt.Errorf("%q already exists", key)
		}
		if out.Id, err = takeId(conf, opPutClone, spec.Id); err != nil {
			return err
		}
		grp, err := putCloneGrp(s, cl, target, conf, spec, addrs, &out)
		if err != nil {
			return err
		}
		s.Put(key, &pb.Clone{
			CloneId:       out.Id,
			SrcNqn:        spec.SrcNqn,
			SrcNsIdx:      spec.SrcNsIdx,
			SrcSliceCnt:   spec.SrcSliceCnt,
			SrcStripeSize: spec.SrcStripeSize,
			SrcBlockSize:  spec.SrcBlockSize,
			DstTdId:       out.DstTdId,
			MetaGrp:       grp,
		})
		conf.CloneNameList = append(conf.CloneNameList, spec.Name)
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Clone{}, err
	}
	return out, nil
}

// putCloneGrp is PutClone's clone meta group inside its STM: it lays the
// group out, takes its ids, charges its DNs and returns the record the Clone
// carries, nil for a spec with no leg, filling out's group fields. addrs are
// the endpoints of the legs' DNs, in the spec's order.
//
// It refuses, before anything is written, what no gateway could have
// written and what would make the clone drain go wrong: a spare in a pool
// whose groups are not md-raid1 (the pool's stored redund_conf decides, a
// group having no redundancy of its own), a slot outside the pool's
// cntlid_slot_list, a DN short of the group's extents, and a group, leg or
// side id that a slice or another clone of the pool already uses for its
// own kind (poolIds). The side id above all: the clone drain takes a side's
// pointer out of its DN by the pool and the side id alone
// (model.FinishCloneDelete), so a clone side sharing a slice side's id would
// take that slice side's pointer with it. Ids of two kinds may meet, as
// newSpPlan lets them. It does not judge the leg count against the pool's
// redundancy, the destination against the one-extent bound CreateClone
// applies, or a DN for being disabled: the library plants whatever shape a
// case needs.
//
// It reads every slice and every clone of the pool for the id check, so
// PutClone's STM grows with the pool: at the widest pool, MaxSliceCntPerSp
// slices and MaxCloneCntPerSp - 1 clones read, beside the few keys of the
// pool and the clone, and six compares per DN of the group, its record and
// its revision read and written and its capacity key moved. That stays far
// below common.EtcdMaxTxnOps, which every etcd the suites and the Go cases
// start runs with.
func putCloneGrp(
	s etcdutil.STM,
	cl Cluster,
	target SpRef,
	conf *pb.SpConf,
	spec CloneSpec,
	addrs []string,
	out *Clone,
) (*pb.Group, error) {
	if len(spec.Legs) == 0 {
		return nil, nil
	}
	cid, cc := cl.Id, cl.Conf
	raid1 := conf.GetBdevConf().GetRedundConf().GetRedundMdRaid1() != nil
	for _, leg := range spec.Legs {
		if leg.Spare && !raid1 {
			return nil, fmt.Errorf(
				"clone %q: leg %s: a spare leg needs md-raid1 groups "+
					"(architecture.md, Spare legs)", spec.Name, leg)
		}
		if !slices.Contains(conf.GetCntlidSlotList(), leg.Slot) {
			return nil, fmt.Errorf(
				"clone %q: leg %s: cntlid slot %d is not in the slot list "+
					"of storage pool %q", spec.Name, leg, leg.Slot, target.Name)
		}
	}
	metaBlocks, dataBlocks, err := model.GroupBlocks(
		common.CloneMetaGrpExtCnt, cc.GetDnBinConf().GetExtentSize(),
		conf.GetBdevConf())
	if err != nil {
		return nil, fmt.Errorf("storage pool %q: %w", target.Name, err)
	}
	used, err := readPoolIds(s, cid, conf)
	if err != nil {
		return nil, err
	}
	claim := func(kind string, id uint64) error {
		if err := used.claim(kind, id); err != nil {
			return fmt.Errorf("clone %q: %w in storage pool %q",
				spec.Name, err, target.Name)
		}
		return nil
	}
	grp := &pb.Group{
		ExtCnt:     common.CloneMetaGrpExtCnt,
		MetaBlocks: metaBlocks,
		DataBlocks: dataBlocks,
	}
	if grp.GrpId, err = takeId(conf, opPutClone, spec.GrpId); err != nil {
		return nil, err
	}
	if err := claim("group", grp.GrpId); err != nil {
		return nil, err
	}
	for i, l := range spec.Legs {
		legId, err := takeId(conf, opPutClone, l.LegId)
		if err != nil {
			return nil, err
		}
		sideId, err := takeId(conf, opPutClone, l.SideId)
		if err != nil {
			return nil, err
		}
		if err := claim("leg", legId); err != nil {
			return nil, err
		}
		if err := claim("side", sideId); err != nil {
			return nil, err
		}
		key := model.DnConfKey(cid, addrs[i])
		dn := &pb.DnConf{}
		if !s.Get(key, dn) {
			return nil, fmt.Errorf("%q not found", key)
		}
		if dn.GetDnId() != l.DnId {
			return nil, fmt.Errorf("%q holds dn %#x, want %#x",
				addrs[i], dn.GetDnId(), l.DnId)
		}
		revision, err := chargeDn(s, cid, cc, addrs[i], dn,
			[]*pb.SidePointer{{
				SpId:   target.Id,
				LegId:  legId,
				SideId: sideId,
			}}, common.CloneMetaGrpExtCnt, 0)
		if err != nil {
			return nil, err
		}
		leg := &pb.Leg{
			LegId:  legId,
			LegIdx: l.LegIdx,
			SideList: []*pb.Side{{
				SideId:      sideId,
				AddrPort:    addrs[i],
				CntlidSlot:  l.Slot,
				NvmeTrConf:  dn.GetNvmeTrConf(),
				ErrEpoch:    0,
				Provisioned: false,
			}},
		}
		if l.Spare {
			grp.SpareLegList = append(grp.SpareLegList, leg)
		} else {
			grp.LegList = append(grp.LegList, leg)
		}
		out.LegIds = append(out.LegIds, legId)
		out.SideIds = append(out.SideIds, sideId)
		out.DnRevisions[addrs[i]] = revision
	}
	out.GrpId = grp.GetGrpId()
	out.MetaBlocks = metaBlocks
	out.DataBlocks = dataBlocks
	return grp, nil
}

// poolIds holds, per kind — "group", "leg" and "side" — every id the slices
// and the clones of one pool use, as readPoolIds reads them.
type poolIds map[string]map[uint64]bool

// readPoolIds reads every slice, every clone, every repair and every retired
// leg of the pool inside the caller's STM and collects their groups', legs'
// and sides' ids, spare legs, a migration's second side, the repairs' new
// legs and the retired legs included. A listed record whose key is missing
// refuses: an id it held could no longer be told apart.
func readPoolIds(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) (poolIds, error) {
	ids := poolIds{
		"group": make(map[uint64]bool),
		"leg":   make(map[uint64]bool),
		"side":  make(map[uint64]bool),
	}
	addLegs := func(legs []*pb.Leg) {
		for _, leg := range legs {
			ids["leg"][leg.GetLegId()] = true
			for _, side := range leg.GetSideList() {
				ids["side"][side.GetSideId()] = true
			}
		}
	}
	addGrp := func(grp *pb.Group) {
		ids["group"][grp.GetGrpId()] = true
		addLegs(grp.GetLegList())
		addLegs(grp.GetSpareLegList())
	}
	for _, sliceId := range conf.GetSliceIdList() {
		key := model.SliceKey(cid, conf.GetSpId(), sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return nil, fmt.Errorf("%q not found", key)
		}
		for _, grp := range slice.GetMetaGrpList() {
			addGrp(grp)
		}
		for _, grp := range slice.GetDataGrpList() {
			addGrp(grp)
		}
	}
	for _, cloneName := range conf.GetCloneNameList() {
		key := model.CloneKey(cid, conf.GetSpId(), cloneName)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return nil, fmt.Errorf("%q not found", key)
		}
		if grp := clone.GetMetaGrp(); grp != nil {
			addGrp(grp)
		}
	}
	for _, repName := range conf.GetRepairNameList() {
		key := model.RepairKey(cid, conf.GetSpId(), repName)
		rep := &pb.Repair{}
		if !s.Get(key, rep) {
			return nil, fmt.Errorf("%q not found", key)
		}
		addLegs(rep.GetNewLegList())
	}
	for _, legId := range conf.GetRetiredLegIdList() {
		key := model.RetiredLegKey(cid, conf.GetSpId(), legId)
		retired := &pb.RetiredLeg{}
		if !s.Get(key, retired) {
			return nil, fmt.Errorf("%q not found", key)
		}
		ids["leg"][legId] = true
		ids["side"][retired.GetSide().GetSideId()] = true
	}
	return ids, nil
}

// claim refuses an id the pool already uses for its kind, and counts it as
// used otherwise, so the ids of one group are kept apart from each other
// too.
func (ids poolIds) claim(kind string, id uint64) error {
	if ids[kind][id] {
		return fmt.Errorf("%s id %#x is already used", kind, id)
	}
	ids[kind][id] = true
	return nil
}

// plantedCloneTd resolves PutClone's DstTd inside its STM to a td_id: a td
// name first, so a td named like an id is that td, else the text as a td_id
// in decimal or 0x form, which need not name a td of the pool.
func plantedCloneTd(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
	dstTd string,
) (uint64, error) {
	td := &pb.ThinDevice{}
	if s.Get(model.ThinDeviceKey(cid, conf.GetSpId(), dstTd), td) {
		return td.GetTdId(), nil
	}
	tdId, err := strconv.ParseUint(strings.TrimSpace(dstTd), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("destination td %q: no such td", dstTd)
	}
	return tdId, nil
}

// ---------------------------------------------------------------------------
// Transfers (architecture.md, Transfers)
// ---------------------------------------------------------------------------

// XferSpec is one transfer to plant.
type XferSpec struct {
	Name string
	// Id is the xfer_id; 0 takes the pool's next free one.
	Id       uint64
	OriNqn   string
	OriNsIdx uint32
}

// Check refuses a transfer no key could be written for.
func (spec XferSpec) Check() error {
	if spec.Name == "" {
		return fmt.Errorf("the transfer name is empty")
	}
	if spec.OriNqn == "" {
		return fmt.Errorf("the transfer's ori_nqn is empty")
	}
	return nil
}

// Xfer is one planted transfer.
type Xfer struct {
	Name  string
	Id    uint64
	SpRev uint64
}

// PutXfer writes one Transfer and its SpConf.xfer_name_list entry
// (architecture.md, Transfers), then bumps SpRev once.
func PutXfer(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec XferSpec,
) (Xfer, error) {
	if err := spec.Check(); err != nil {
		return Xfer{}, err
	}
	cid := cl.Id
	var out Xfer
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Xfer{Name: spec.Name}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetXferNameList(), spec.Name) {
			return fmt.Errorf("transfer %q already exists", spec.Name)
		}
		key := model.TransferKey(cid, target.Id, spec.Name)
		if s.Get(key, &pb.Transfer{}) {
			return fmt.Errorf("%q already exists", key)
		}
		out.Id, err = takeId(conf, "PutXfer", spec.Id)
		if err != nil {
			return err
		}
		s.Put(key, &pb.Transfer{
			XferId:   out.Id,
			OriNqn:   spec.OriNqn,
			OriNsIdx: spec.OriNsIdx,
		})
		conf.XferNameList = append(conf.XferNameList, spec.Name)
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Xfer{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Migrations (architecture.md, Migrations)
// ---------------------------------------------------------------------------

// MigrSpec is one migration to plant: the side it moves and where its new
// side goes.
type MigrSpec struct {
	Name string
	// Id is the migr_id; 0 takes the pool's next free one.
	Id      uint64
	SrcSide uint64
	// DstSide is the new side's side_id; 0 takes the pool's next free one.
	DstSide uint64
	// DstDn is the dn_id that hosts the new side, DstSlot its cntlid_slot.
	DstDn   uint64
	DstSlot uint32
	// CutoverPending writes the record's cutover_pending, which
	// CreateMigration always sets.
	CutoverPending bool
}

// Check refuses a migration no key could be written for.
func (spec MigrSpec) Check() error {
	if spec.Name == "" {
		return fmt.Errorf("the migration name is empty")
	}
	if spec.SrcSide == 0 {
		return fmt.Errorf("migration %q: src side 0 is never valid",
			spec.Name)
	}
	if spec.DstDn == 0 {
		return fmt.Errorf("migration %q: dst dn 0 is never valid", spec.Name)
	}
	return nil
}

// Migr is one planted migration: its ids, the leg, group and slice that
// carry both its sides, the destination DN and the charge it took, the
// destination DN's new revision and the pool revision.
type Migr struct {
	Name           string
	Id             uint64
	SrcSide        uint64
	DstSide        uint64
	LegId          uint64
	GrpId          uint64
	SliceId        uint64
	DstDn          uint64
	Addr           string
	ExtCnt         uint64
	MetaUnits      uint64
	CutoverPending bool
	DnRevision     uint64
	SpRev          uint64
}

// PutMigr writes one Migration and its SpConf.migr_name_list entry
// (architecture.md, Migrations) AND appends the destination Side to the
// SOURCE side's leg, which is what makes the leg carry two sides — the shape
// RW15 keys migr_src_conf/migr_dst_conf off and the precondition AR8 reads as
// "a leg with two sides is left alone". The destination DN gets the
// CreateMigration treatment (architecture.md, Migrations; Metadata units):
// the side pointer, the group's ext_cnt off its budget, the migration's
// metadata units (plantedMigrUnits) on its used count, its capacity key
// maintained and its DnRev bumped once (chargeDn). The units are stored
// on the Migration record too, which is where FinishMigration and
// CancelMigration read what to give back. It refuses a DN short of extents,
// whose count would wrap below zero, but never one past its metadata area: a
// used count only grows, and the library plants whatever shape a case needs.
// The source is looked up in the slices alone, so a side of a clone meta
// group is no source, as CreateMigration refuses one (architecture.md,
// Clones), and a destination side id that a slice or a clone of the pool
// already uses is refused, as PutClone refuses one.
//
// CutoverPending writes the record's cutover_pending, which CreateMigration
// always sets and only the sp worker's cutover clears (architecture.md,
// Migrations; dnv-worker.md RW18); without it the record reads as a
// migration whose cutover has started, a shape no gateway writes but a case
// may plant.
func PutMigr(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec MigrSpec,
) (Migr, error) {
	if err := spec.Check(); err != nil {
		return Migr{}, err
	}
	cid, cc := cl.Id, cl.Conf
	dstAddr, err := DnAddr(ctx, cli, cl, spec.DstDn)
	if err != nil {
		return Migr{}, err
	}
	var out Migr
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Migr{
			Name:           spec.Name,
			SrcSide:        spec.SrcSide,
			DstDn:          spec.DstDn,
			Addr:           dstAddr,
			CutoverPending: spec.CutoverPending,
		}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetMigrNameList(), spec.Name) {
			return fmt.Errorf("migration %q already exists", spec.Name)
		}
		dn := &pb.DnConf{}
		if !s.Get(model.DnConfKey(cid, dstAddr), dn) {
			return fmt.Errorf("%q not found", model.DnConfKey(cid, dstAddr))
		}
		if dn.GetDnId() != spec.DstDn {
			return fmt.Errorf(
				"%q holds dn %#x, want %#x",
				dstAddr, dn.GetDnId(), spec.DstDn,
			)
		}
		// Find the leg that carries the source side.
		var slice *pb.Slice
		var leg *pb.Leg
		for _, candidateId := range conf.GetSliceIdList() {
			candidate := &pb.Slice{}
			key := model.SliceKey(cid, target.Id, candidateId)
			if !s.Get(key, candidate) {
				return fmt.Errorf("%q not found", key)
			}
			grpLists := [][]*pb.Group{
				candidate.GetMetaGrpList(),
				candidate.GetDataGrpList(),
			}
			for _, grpList := range grpLists {
				for _, grp := range grpList {
					for _, candidateLeg := range grp.GetLegList() {
						for _, side := range candidateLeg.GetSideList() {
							if side.GetSideId() != spec.SrcSide {
								continue
							}
							slice = candidate
							leg = candidateLeg
							out.LegId = candidateLeg.GetLegId()
							out.GrpId = grp.GetGrpId()
							out.SliceId = candidateId
							out.ExtCnt = grp.GetExtCnt()
						}
					}
				}
			}
		}
		if leg == nil {
			return fmt.Errorf(
				"no side %#x in storage pool %q", spec.SrcSide, target.Name,
			)
		}
		if out.Id, err = takeId(conf, "PutMigr", spec.Id); err != nil {
			return err
		}
		out.DstSide, err = takeId(conf, "PutMigr", spec.DstSide)
		if err != nil {
			return err
		}
		for _, side := range leg.GetSideList() {
			if side.GetSideId() == out.DstSide {
				return fmt.Errorf(
					"leg %#x already carries side %#x", out.LegId, out.DstSide,
				)
			}
		}
		// A DN pointer is removed by the pool and the side id alone, by the
		// clone drain's final step among others, so the new side's id must
		// be one no slice or clone of the pool uses.
		used, err := readPoolIds(s, cid, conf)
		if err != nil {
			return err
		}
		if err := used.claim("side", out.DstSide); err != nil {
			return fmt.Errorf("migration %q: %w in storage pool %q",
				spec.Name, err, target.Name)
		}
		out.MetaUnits, err = plantedMigrUnits(
			conf, out.ExtCnt, cc.GetDnBinConf().GetExtentSize())
		if err != nil {
			return fmt.Errorf("storage pool %q: %w", target.Name, err)
		}
		leg.SideList = append(leg.SideList, &pb.Side{
			SideId:      out.DstSide,
			AddrPort:    dstAddr,
			CntlidSlot:  spec.DstSlot,
			NvmeTrConf:  dn.GetNvmeTrConf(),
			ErrEpoch:    0,
			Provisioned: false,
		})
		s.Put(model.SliceKey(cid, target.Id, out.SliceId), slice)
		s.Put(model.MigrationKey(cid, target.Id, spec.Name), &pb.Migration{
			MigrId:         out.Id,
			SrcSideId:      spec.SrcSide,
			DstSideId:      out.DstSide,
			BmCnt:          0,
			MetaUnits:      out.MetaUnits,
			CutoverPending: spec.CutoverPending,
		})
		conf.MigrNameList = append(conf.MigrNameList, spec.Name)
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.DnRevision, err = chargeDn(s, cid, cc, dstAddr, dn,
			[]*pb.SidePointer{{
				SpId:   target.Id,
				LegId:  out.LegId,
				SideId: out.DstSide,
			}}, out.ExtCnt, out.MetaUnits)
		if err != nil {
			return err
		}
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Migr{}, err
	}
	return out, nil
}

// plantedMigrUnits is the units CreateMigration stores on a migration and
// charges its destination DN (architecture.md, Metadata units):
// common.CloneMetaUnits of the moved group's ext_cnt × extent_size at the
// pool's block size, behind the ValidateBdevConf gate that every
// model.PoolBlockSize caller sits behind.
func plantedMigrUnits(
	conf *pb.SpConf,
	extCnt uint64,
	extentSize uint64,
) (uint64, error) {
	bdev := conf.GetBdevConf()
	if err := model.ValidateBdevConf(bdev); err != nil {
		return 0, err
	}
	return common.CloneMetaUnits(
		extCnt*extentSize, model.PoolBlockSize(bdev)), nil
}

// chargeDn is one DN's bookkeeping inside a caller's STM, as the gateway's
// creates do it (architecture.md, Storage pools; Capacity index keys;
// Metadata units; Revision keys and the sync fan-out): the side pointers in,
// extCnt off free_ext_cnt and units on used_meta_units, all in the one write
// of the record, the capacity key following the record
// (model.MaintainDnCapacity) and the DN's revision bumped once. PutSp charges
// each DN of a pool through it, its units zero; PutMigr its destination DN,
// with the units FinishMigration and CancelMigration give back; and PutClone
// each DN of a clone's clone meta group, its units zero, since a clone meta
// side takes no metadata unit. dn is the record as read in the same STM,
// which MaintainDnCapacity needs to find the key it replaces. It refuses a
// DN short of extents, whose count would wrap below zero, and writes nothing
// then, but never refuses one past its metadata area. It returns the DN's
// new revision.
func chargeDn(
	s etcdutil.STM,
	cid uint64,
	cc *pb.ClusterConf,
	addrPort string,
	dn *pb.DnConf,
	ptrs []*pb.SidePointer,
	extCnt uint64,
	units uint64,
) (uint64, error) {
	if dn.GetFreeExtCnt() < extCnt {
		return 0, fmt.Errorf(
			"dn %#x at %q has free_ext_cnt %d, needs %d",
			dn.GetDnId(), addrPort, dn.GetFreeExtCnt(), extCnt,
		)
	}
	newDn := proto.Clone(dn).(*pb.DnConf)
	newDn.SidePtrList = append(newDn.SidePtrList, ptrs...)
	newDn.FreeExtCnt -= extCnt
	newDn.UsedMetaUnits += units
	s.Put(model.DnConfKey(cid, addrPort), newDn)
	model.MaintainDnCapacity(s, cid, addrPort, cc, dn, newDn)
	return bumpDnRev(s, cid, newDn)
}

// ---------------------------------------------------------------------------
// Bitmap chunks (architecture.md, Clones and Migrations; BM3)
// ---------------------------------------------------------------------------

// CloneBitmapSpec is one chunk of a clone's bitmap. A clone chunk is
// addressed by the PAIR SrcSliceIdx / BmIdx and holds the bytes at offset
// bm_idx*CloneBmChunkBytes of that source slice's bitmap, so the same bm_idx
// on two slices seeds two distinct chunks. A chunk is opaque bytes to
// everything in dnv above the agent (BM3), so the caller supplies it
// literally.
type CloneBitmapSpec struct {
	Clone       string
	SrcSliceIdx uint32
	BmIdx       uint32
	Data        []byte
}

// Check refuses a chunk no key could be written for: a clone chunk key
// carries BOTH indexes as a common.BmIdxFmt field, exactly two hex digits
// each (model.ParseCloneBmKey), so an index of 256 or more could not be
// parsed back out of it.
func (spec CloneBitmapSpec) Check() error {
	if spec.Clone == "" {
		return fmt.Errorf("the clone name is empty")
	}
	if err := checkBmIdx("src_slice_idx", spec.SrcSliceIdx); err != nil {
		return err
	}
	return checkBmIdx("bm_idx", spec.BmIdx)
}

// MigrBitmapSpec is one chunk of a migration's bitmap, which keeps its flat
// append sequence: the new index IS the count.
type MigrBitmapSpec struct {
	Migr  string
	BmIdx uint32
	Data  []byte
}

// Check refuses a chunk no key could be written for: a migration key carries
// the bm_idx field alone (model.ParseBmIdx), as two hex digits.
func (spec MigrBitmapSpec) Check() error {
	if spec.Migr == "" {
		return fmt.Errorf("the migration name is empty")
	}
	return checkBmIdx("bm_idx", spec.BmIdx)
}

// checkBmIdx refuses an index that does not fit a common.BmIdxFmt key field.
func checkBmIdx(what string, idx uint32) error {
	if idx >= 1<<8 {
		return fmt.Errorf("%s %d does not fit in the %q key field",
			what, idx, common.BmIdxFmt)
	}
	return nil
}

// Bitmap is one planted chunk: whether the key was new, the parent
// migration's raised bm_cnt (a Clone record carries no chunk count, so it is
// 0 for a clone chunk) and the pool revision.
type Bitmap struct {
	Created bool
	BmCnt   uint32
	SpRev   uint64
}

// PutCloneBitmap writes one chunk of a clone's bitmap and bumps SpRev once.
// The Clone record carries no chunk count, so the chunk key is all it
// writes. Rewriting an existing chunk still bumps SpRev — which is exactly
// the "grown chunk" trigger of BM5.
func PutCloneBitmap(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec CloneBitmapSpec,
) (Bitmap, error) {
	if err := spec.Check(); err != nil {
		return Bitmap{}, err
	}
	cid := cl.Id
	var out Bitmap
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Bitmap{}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		// The clone record is still LOADED — the existence guard is what
		// keeps a chunk of an unknown clone from planting an orphan chunk
		// key under a clone nothing else knows about — but it is never
		// rewritten: an append changes nothing in it.
		parentKey := model.CloneKey(cid, target.Id, spec.Clone)
		if !s.Get(parentKey, &pb.Clone{}) {
			return fmt.Errorf("%q not found", parentKey)
		}
		key := model.CloneBitmapKey(
			cid, target.Id, spec.Clone, spec.SrcSliceIdx, spec.BmIdx,
		)
		out.Created = !s.Get(key, &pb.CloneBitmap{})
		s.Put(key, &pb.CloneBitmap{Bitmap: spec.Data})
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Bitmap{}, err
	}
	return out, nil
}

// PutMigrBitmap writes one chunk of a migration's bitmap, raises `bm_cnt` on
// the parent Migration when the chunk is new, and bumps SpRev once.
// Rewriting an existing chunk raises no count but still bumps SpRev.
func PutMigrBitmap(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec MigrBitmapSpec,
) (Bitmap, error) {
	if err := spec.Check(); err != nil {
		return Bitmap{}, err
	}
	cid := cl.Id
	var out Bitmap
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Bitmap{}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		parentKey := model.MigrationKey(cid, target.Id, spec.Migr)
		parent := &pb.Migration{}
		if !s.Get(parentKey, parent) {
			return fmt.Errorf("%q not found", parentKey)
		}
		key := model.MigrBitmapKey(cid, target.Id, spec.Migr, spec.BmIdx)
		out.Created = !s.Get(key, &pb.MigrBitmap{})
		s.Put(key, &pb.MigrBitmap{Bitmap: spec.Data})
		if out.Created {
			parent.BmCnt++
			s.Put(parentKey, parent)
		}
		out.BmCnt = parent.GetBmCnt()
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Bitmap{}, err
	}
	return out, nil
}
