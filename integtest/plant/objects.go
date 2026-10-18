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
				tdId := takeId(conf, spec.Id)
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
		out.Id = takeId(conf, spec.Id)
		subsystem := &pb.Subsystem{
			SsId:   out.Id,
			Serial: spec.Serial,
			Model:  spec.Model,
		}
		for _, ns := range spec.Namespaces {
			nsId := takeId(conf, ns.NsId)
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

// CloneSpec is one clone to plant.
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
}

// Check refuses a clone no key could be written for.
func (spec CloneSpec) Check() error {
	if spec.Name == "" {
		return fmt.Errorf("the clone name is empty")
	}
	if spec.DstTd == "" {
		return fmt.Errorf("the clone's destination td is not named")
	}
	return nil
}

// Clone is one planted clone: its id, its destination td's id, the units it
// stored and the units it charged each CN, keyed by the CN's endpoint.
type Clone struct {
	Name      string
	Id        uint64
	DstTdId   uint64
	MetaUnits uint64
	Charged   map[string]uint64
	SpRev     uint64
}

// PutClone writes one Clone and its SpConf.clone_name_list entry
// (architecture.md, Clones), then bumps SpRev once.
//
// It charges the clone's metadata units as CreateClone does (architecture.md,
// Metadata units): common.CloneMetaUnits of the destination td's size at the
// pool's block size, stored on the Clone record and charged to the CN of
// every cntlr of the pool (chargeCloneCns). The clone drain's last step,
// model.FinishCloneDelete, gives exactly those charges back, so a planted
// clone that drains leaves its nodes' counts where it found them. Unlike
// CreateClone it never refuses a CN past its area: the library plants
// whatever shape a case needs, and the worker's cntlr replacement can leave
// a CN past its ceiling anyway. A DstTd id that no td of the pool carries is
// planted with no units, as there is no size to charge for.
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
	cid := cl.Id
	var out Clone
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Clone{Name: spec.Name}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetCloneNameList(), spec.Name) {
			return fmt.Errorf("clone %q already exists", spec.Name)
		}
		var td *pb.ThinDevice
		out.DstTdId, td, err = plantedCloneTd(s, cid, conf, spec.DstTd)
		if err != nil {
			return err
		}
		out.MetaUnits, err = plantedCloneUnits(conf, td)
		if err != nil {
			return fmt.Errorf("storage pool %q: %w", target.Name, err)
		}
		key := model.CloneKey(cid, target.Id, spec.Name)
		if s.Get(key, &pb.Clone{}) {
			return fmt.Errorf("%q already exists", key)
		}
		out.Charged, err = chargeCloneCns(s, cid, conf, out.MetaUnits)
		if err != nil {
			return err
		}
		out.Id = takeId(conf, spec.Id)
		s.Put(key, &pb.Clone{
			CloneId:       out.Id,
			SrcNqn:        spec.SrcNqn,
			SrcNsIdx:      spec.SrcNsIdx,
			SrcSliceCnt:   spec.SrcSliceCnt,
			SrcStripeSize: spec.SrcStripeSize,
			SrcBlockSize:  spec.SrcBlockSize,
			DstTdId:       out.DstTdId,
			MetaUnits:     out.MetaUnits,
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

// plantedCloneTd resolves PutClone's DstTd inside its STM: a td name first,
// else a td_id, which is looked up among the pool's tds so the clone can be
// sized. A td_id that no td of the pool carries resolves to itself with a nil
// td, and PutClone plants that clone with no units.
func plantedCloneTd(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
	dstTd string,
) (uint64, *pb.ThinDevice, error) {
	td := &pb.ThinDevice{}
	if s.Get(model.ThinDeviceKey(cid, conf.GetSpId(), dstTd), td) {
		return td.GetTdId(), td, nil
	}
	tdId, err := strconv.ParseUint(strings.TrimSpace(dstTd), 0, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("destination td %q: no such td", dstTd)
	}
	for _, tdName := range conf.GetTdNameList() {
		candidate := &pb.ThinDevice{}
		if !s.Get(model.ThinDeviceKey(cid, conf.GetSpId(), tdName), candidate) {
			continue
		}
		if candidate.GetTdId() == tdId {
			return tdId, candidate, nil
		}
	}
	return tdId, nil, nil
}

// plantedCloneUnits is the units CreateClone stores on a clone of td
// (architecture.md, Metadata units): common.CloneMetaUnits of the td's size
// at the pool's block size, behind the ValidateBdevConf gate that every
// model.PoolBlockSize caller sits behind. A nil td has no size and gets no
// units.
func plantedCloneUnits(conf *pb.SpConf, td *pb.ThinDevice) (uint64, error) {
	if td == nil {
		return 0, nil
	}
	bdev := conf.GetBdevConf()
	if err := model.ValidateBdevConf(bdev); err != nil {
		return 0, err
	}
	return common.CloneMetaUnits(td.GetSize(), model.PoolBlockSize(bdev)), nil
}

// chargeCloneCns charges a planted clone's units, inside PutClone's STM, to
// the CN of every cntlr of the pool, as CreateClone charges them
// (architecture.md, Metadata units): once per cntlr, summed per CN, so each
// CN is written once, in the order the cntlrs first name it, and a CN that
// hosts two cntlrs of the pool is charged twice in that one write — exactly
// what model.FinishCloneDelete later gives back. The capacity value follows
// the record (model.MaintainCnCapacity) and no CnRev is bumped, since a write
// that changes only units raises no revision. Nothing is refused for room.
//
// Every Cntlr is read before any write, and a listed one that is missing
// refuses: the record is what names the CN to charge. A CnConf that is gone
// is skipped, as the release skips it, since there is nothing to charge. Zero
// units read nothing. The result maps each CN it writes to the units it
// charges that CN.
func chargeCloneCns(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
	units uint64,
) (map[string]uint64, error) {
	charged := make(map[string]uint64)
	if units == 0 {
		return charged, nil
	}
	var order []string
	perCn := make(map[string]uint64)
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr := &pb.Cntlr{}
		if !s.Get(model.CntlrKey(cid, conf.GetSpId(), cntlrId), cntlr) {
			return nil, fmt.Errorf("cntlr %#x not found", cntlrId)
		}
		addrPort := cntlr.GetAddrPort()
		if _, seen := perCn[addrPort]; !seen {
			order = append(order, addrPort)
		}
		perCn[addrPort] += units
	}
	for _, addrPort := range order {
		key := model.CnConfKey(cid, addrPort)
		cn := &pb.CnConf{}
		if !s.Get(key, cn) {
			continue
		}
		newCn := proto.Clone(cn).(*pb.CnConf)
		newCn.UsedMetaUnits += perCn[addrPort]
		s.Put(key, newCn)
		model.MaintainCnCapacity(s, cid, addrPort, cn, newCn)
		charged[addrPort] = perCn[addrPort]
	}
	return charged, nil
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
		out.Id = takeId(conf, spec.Id)
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
// maintained and its DnRev bumped once (chargeMigrDst). The units are stored
// on the Migration record too, which is where FinishMigration and
// CancelMigration read what to give back. It refuses a DN short of extents,
// whose count would wrap below zero, but never one past its metadata area: a
// used count only grows, and the library plants whatever shape a case needs.
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
		out.Id = takeId(conf, spec.Id)
		out.DstSide = takeId(conf, spec.DstSide)
		for _, side := range leg.GetSideList() {
			if side.GetSideId() == out.DstSide {
				return fmt.Errorf(
					"leg %#x already carries side %#x", out.LegId, out.DstSide,
				)
			}
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
		out.DnRevision, err = chargeMigrDst(s, cid, cc, dstAddr, dn,
			&pb.SidePointer{
				SpId:   target.Id,
				LegId:  out.LegId,
				SideId: out.DstSide,
			}, out.ExtCnt, out.MetaUnits)
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

// chargeMigrDst is PutMigr's bookkeeping of its destination DN, inside
// PutMigr's STM, as CreateMigration does it (architecture.md, Storage pools;
// Capacity index keys; Metadata units; Revision keys and the sync fan-out):
// the side pointer in, extCnt off free_ext_cnt and the migration's units on
// used_meta_units, all in the one write of the record, the capacity key
// following the record (model.MaintainDnCapacity) and the DN's revision
// bumped once — the units FinishMigration and CancelMigration give back. dn
// is the record as read in the same STM, which MaintainDnCapacity needs to
// find the key it replaces. It refuses a DN short of extents, whose count
// would wrap below zero, and writes nothing then, but never refuses one past
// its metadata area. It returns the DN's new revision.
func chargeMigrDst(
	s etcdutil.STM,
	cid uint64,
	cc *pb.ClusterConf,
	addrPort string,
	dn *pb.DnConf,
	ptr *pb.SidePointer,
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
	newDn.SidePtrList = append(newDn.SidePtrList, ptr)
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
