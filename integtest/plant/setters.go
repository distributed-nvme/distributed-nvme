package plant

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Setters: raw writes of one field
// ---------------------------------------------------------------------------

// CntlrFlags names the flags SetCntlr rewrites: a nil one is left as stored,
// so "not given" differs from "false" — the worker suite's case C7 flips the
// primary role with two calls and case D10 disables a standby without
// touching its role.
type CntlrFlags struct {
	Primary  *bool
	Disabled *bool
}

// SetCntlr rewrites one Cntlr's primary / disabled flags and bumps SpRev once.
// It returns the record as written and the pool's new revision.
func SetCntlr(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cntlrId uint64,
	flags CntlrFlags,
) (*pb.Cntlr, uint64, error) {
	if flags.Primary == nil && flags.Disabled == nil {
		return nil, 0, fmt.Errorf(
			"at least one of primary and disabled is required")
	}
	cid := cl.Id
	var stored *pb.Cntlr
	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		stored, spRev = nil, 0
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		key := model.CntlrKey(cid, target.Id, cntlrId)
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			return fmt.Errorf("%q not found", key)
		}
		if flags.Primary != nil {
			cntlr.Primary = *flags.Primary
		}
		if flags.Disabled != nil {
			cntlr.Disabled = *flags.Disabled
		}
		s.Put(key, cntlr)
		stored = cntlr
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return stored, spRev, nil
}

// SetLevel writes SpConf.sp_level and bumps SpRev once (architecture.md,
// Storage pools: UpdateStoragePoolLevel): the level rides in both Syncup
// requests, and AR3 makes NO_THINPOOL and above suppress every reaction —
// which is what the worker suite's case D10 proves. An undefined level is
// refused: it would be stored and then read back as a level no rule of
// architecture.md, Common validation, knows.
func SetLevel(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	level pb.SpLevel,
) (uint64, error) {
	if _, ok := pb.SpLevel_name[int32(level)]; !ok {
		return 0, fmt.Errorf("sp_level %d is not a defined SpLevel",
			int32(level))
	}
	cid := cl.Id
	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		spRev = 0
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		conf.SpLevel = level
		s.Put(model.SpConfKey(cid, target.Name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return 0, err
	}
	return spRev, nil
}

// SetLwm writes SpConf.bdev_conf.dm_pool_conf.low_water_mark_pct and bumps
// SpRev once: the lever the worker suite's case D5/D6 pulls to arm and
// disarm the AR6 grow rule (a value above 100 means "never grow
// automatically"). The gateway's rule, mirrored (architecture.md, Common
// validation): 0 asks for the default and is resolved before the write,
// because the worker refuses a stored 0; a value above 100 is a MEANING and
// is stored exactly as given. It returns the value stored and the pool's new
// revision.
func SetLwm(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	pct uint32,
) (uint32, uint64, error) {
	if pct == 0 {
		pct = common.DefaultPoolLowWatermarkPct
	}
	cid := cl.Id
	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		spRev = 0
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if conf.BdevConf == nil {
			conf.BdevConf = &pb.BdevConf{}
		}
		if conf.BdevConf.DmPoolConf == nil {
			conf.BdevConf.DmPoolConf = &pb.DmPoolConf{}
		}
		conf.BdevConf.DmPoolConf.LowWaterMarkPct = pct
		s.Put(model.SpConfKey(cid, target.Name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return pct, spRev, nil
}

// SetDnFree rewrites a DN's free_ext_cnt and maintains its capacity key in
// the same STM (architecture.md, Capacity index keys) — and bumps NO
// revision: a budget change is an allocator input, not agent-visible desired
// state (architecture.md, Revision keys and the sync fan-out). It is a case's
// lever for deterministic allocation. It returns the DN's endpoint and its
// record as written.
func SetDnFree(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	dnId uint64,
	freeExt uint64,
) (string, *pb.DnConf, error) {
	addr, err := DnAddr(ctx, cli, cl, dnId)
	if err != nil {
		return "", nil, err
	}
	var stored *pb.DnConf
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		stored = nil
		key := model.DnConfKey(cl.Id, addr)
		dn := &pb.DnConf{}
		if !s.Get(key, dn) {
			return fmt.Errorf("%q not found", key)
		}
		newDn := proto.Clone(dn).(*pb.DnConf)
		newDn.FreeExtCnt = freeExt
		s.Put(key, newDn)
		model.MaintainDnCapacity(s, cl.Id, addr, cl.Conf, dn, newDn)
		stored = newDn
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return addr, stored, nil
}

// SetCnFree is SetDnFree's mirror for a CN.
func SetCnFree(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	cnId uint64,
	freeExt uint64,
) (string, *pb.CnConf, error) {
	addr, err := CnAddr(ctx, cli, cl, cnId)
	if err != nil {
		return "", nil, err
	}
	var stored *pb.CnConf
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		stored = nil
		key := model.CnConfKey(cl.Id, addr)
		cn := &pb.CnConf{}
		if !s.Get(key, cn) {
			return fmt.Errorf("%q not found", key)
		}
		newCn := proto.Clone(cn).(*pb.CnConf)
		newCn.FreeExtCnt = freeExt
		s.Put(key, newCn)
		model.MaintainCnCapacity(s, cl.Id, addr, cn, newCn)
		stored = newCn
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return addr, stored, nil
}

// ---------------------------------------------------------------------------
// Health epochs (dnv-worker.md, Integration test plan, The driver)
// ---------------------------------------------------------------------------
//
// The Set*Epoch calls plant one err_epoch and write nothing else: a
// Cntlr's, or a Leg's or a Side's inside its Slice or inside its clone's
// clone meta group, and 0 clears it. They are raw: they bump no revision, as no health write does (HL3), and touch no
// CdcEntry and no settling flag. On a leg, a side, a primary or a disabled
// cntlr, a health write that sets or clears the epoch can leave just that as
// well, so there a case plants the epoch an earlier owner or another observer
// would have left (AR10). A health write that sets or clears the epoch of an
// enabled standby also rewrites the SP's CdcEntry records, because that
// epoch decides whether the listing rule lists the standby (architecture.md
// [D18]; dnv-worker.md HL2). These calls leave them as they are, so after
// one the records may disagree with that rule about such a cntlr. Each
// returns the epoch it replaced.

// SetCntlrEpoch plants one Cntlr's err_epoch.
func SetCntlrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cntlrId uint64,
	epoch uint64,
) (uint64, error) {
	cid := cl.Id
	var old uint64
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		old = 0
		if _, err := getSpConf(s, cid, target); err != nil {
			return err
		}
		key := model.CntlrKey(cid, target.Id, cntlrId)
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			return fmt.Errorf("%q not found", key)
		}
		old = cntlr.GetErrEpoch()
		cntlr.ErrEpoch = epoch
		s.Put(key, cntlr)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return old, nil
}

// SetLegEpoch plants the err_epoch of one leg of a slice, active or spare.
func SetLegEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	sliceId uint64,
	legId uint64,
	epoch uint64,
) (uint64, error) {
	return setSliceObjEpoch(ctx, cli, cl, target, "leg", sliceId, legId, epoch)
}

// SetSideEpoch plants the err_epoch of one side of a slice, on an active or a
// spare leg.
func SetSideEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	sliceId uint64,
	sideId uint64,
	epoch uint64,
) (uint64, error) {
	return setSliceObjEpoch(
		ctx, cli, cl, target, "side", sliceId, sideId, epoch)
}

// setSliceObjEpoch is SetLegEpoch and SetSideEpoch: kind is "leg" or "side".
func setSliceObjEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	kind string,
	sliceId uint64,
	id uint64,
	epoch uint64,
) (uint64, error) {
	cid := cl.Id
	var old uint64
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		old = 0
		if _, err := getSpConf(s, cid, target); err != nil {
			return err
		}
		key := model.SliceKey(cid, target.Id, sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return fmt.Errorf("%q not found", key)
		}
		var found bool
		old, found = setSliceEpoch(slice, kind, id, epoch)
		if !found {
			return fmt.Errorf("slice %#x holds no %s %#x", sliceId, kind, id)
		}
		s.Put(key, slice)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return old, nil
}

// setSliceEpoch sets the err_epoch of one leg, or of one side, of a slice —
// in a meta or a data group, on an active or a spare leg, wherever the health
// ops look them up (model.SetLegErrEpoch, model.SetSideErrEpoch) — and
// returns the value it replaced; found is false when the slice holds no such
// object.
func setSliceEpoch(
	slice *pb.Slice, kind string, id uint64, epoch uint64,
) (old uint64, found bool) {
	grps := append(
		append([]*pb.Group(nil), slice.GetMetaGrpList()...),
		slice.GetDataGrpList()...,
	)
	for _, grp := range grps {
		if old, found = setGroupEpoch(grp, kind, id, epoch); found {
			return old, true
		}
	}
	return 0, false
}

// setGroupEpoch is setSliceEpoch's walk of one group, which the clone setters
// share: the err_epoch of one leg or one side of grp, on an active or a spare
// leg, and the value it replaced; found is false when grp holds no such
// object, and a nil grp holds none.
func setGroupEpoch(
	grp *pb.Group, kind string, id uint64, epoch uint64,
) (old uint64, found bool) {
	legs := append(
		append([]*pb.Leg(nil), grp.GetLegList()...),
		grp.GetSpareLegList()...,
	)
	for _, leg := range legs {
		if kind == "leg" && leg.GetLegId() == id {
			old = leg.GetErrEpoch()
			leg.ErrEpoch = epoch
			return old, true
		}
		if kind != "side" {
			continue
		}
		for _, side := range leg.GetSideList() {
			if side.GetSideId() == id {
				old = side.GetErrEpoch()
				side.ErrEpoch = epoch
				return old, true
			}
		}
	}
	return 0, false
}

// SetCloneLegEpoch plants the err_epoch of one leg of a clone's clone meta
// group, active or spare, where model.SetCloneLegErrEpoch looks it up.
func SetCloneLegEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cloneName string,
	legId uint64,
	epoch uint64,
) (uint64, error) {
	return setCloneObjEpoch(
		ctx, cli, cl, target, "leg", cloneName, legId, epoch)
}

// SetCloneSideEpoch plants the err_epoch of one side of a clone's clone meta
// group, on an active or a spare leg, where model.SetCloneSideErrEpoch looks
// it up.
func SetCloneSideEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cloneName string,
	sideId uint64,
	epoch uint64,
) (uint64, error) {
	return setCloneObjEpoch(
		ctx, cli, cl, target, "side", cloneName, sideId, epoch)
}

// setCloneObjEpoch is SetCloneLegEpoch and SetCloneSideEpoch, the clone
// twin of setSliceObjEpoch: kind is "leg" or "side", and a clone with no
// group holds neither.
func setCloneObjEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	kind string,
	cloneName string,
	id uint64,
	epoch uint64,
) (uint64, error) {
	if cloneName == "" {
		return 0, fmt.Errorf("the clone name is empty")
	}
	cid := cl.Id
	var old uint64
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		old = 0
		if _, err := getSpConf(s, cid, target); err != nil {
			return err
		}
		key := model.CloneKey(cid, target.Id, cloneName)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return fmt.Errorf("%q not found", key)
		}
		var found bool
		old, found = setGroupEpoch(clone.GetMetaGrp(), kind, id, epoch)
		if !found {
			return fmt.Errorf("clone %q holds no %s %#x", cloneName, kind, id)
		}
		s.Put(key, clone)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return old, nil
}

// ---------------------------------------------------------------------------
// Playing the worker (gateway.md, Integration test plan, The etcd
// verification)
// ---------------------------------------------------------------------------
//
// These, with the drains below, are the ONLY writes the gateway integration
// suite performs through workerctl (gateway.md, Integration test plan, The
// etcd verification). They exist because three gateway preconditions are
// gated on a flag only the sp-worker ever moves — CreateThinDevice's snapshot
// gate on ThinDevice.created (architecture.md, Thin devices),
// SwitchSpareLeg's gate on Side.provisioned (architecture.md, Side
// provisioning protocol) and FinishMigration's gate on
// Migration.cutover_pending, which only the worker's cutover clears
// (architecture.md, Migrations) — and that suite deliberately runs no
// dnv-worker. All three go through the very model op the worker calls, so the
// state they leave behind is exactly the state a converging worker would have
// produced, including the single SpRev bump each owes.

// SetCreated flips ThinDevice.created through model.FlipCreated: the
// materialization the sp-worker performs once a cntlr has reported the td's
// thin volume RES_STATUS_OK in every slice (architecture.md, sp role;
// dnv-worker.md RW19). It returns the td's id, whether the flip applied and
// the pool's revision after it.
//
// The td_id comes from the stored record rather than from the caller:
// FlipCreated takes a TdRef of name AND id precisely so a td deleted and
// re-created under the same name between the read and the transaction is
// skipped instead of silently flipped, and a caller that could pass a wrong
// id would defeat that.
func SetCreated(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	tdName string,
) (uint64, bool, uint64, error) {
	if tdName == "" {
		return 0, false, 0, fmt.Errorf("the td name is empty")
	}
	key := model.ThinDeviceKey(cl.Id, target.Id, tdName)
	td := &pb.ThinDevice{}
	found, err := cli.Get(ctx, key, td)
	if err != nil {
		return 0, false, 0, err
	}
	if !found {
		return 0, false, 0, fmt.Errorf("key %q not found", key)
	}
	flipped, err := model.FlipCreated(
		ctx, cli, cl.Id, target.Shard, target.Id,
		[]model.TdRef{{Name: tdName, TdId: td.GetTdId()}},
	)
	if err != nil {
		return 0, false, 0, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return 0, false, 0, err
	}
	return td.GetTdId(), len(flipped) == 1, spRev, nil
}

// SetProvisioned flips one Side.provisioned through model.FlipProvisioned:
// the provisioning completion (architecture.md, Side provisioning protocol)
// the sp-worker records once the dn agent reports the side's zeroing done.
// The gateway suite pulls it exactly where a gateway precondition demands it
// — SwitchSpareLeg refuses an unprovisioned spare (architecture.md, Spare
// legs). It returns whether the flip applied and the pool's revision after
// it.
func SetProvisioned(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	ref model.SideRef,
) (bool, uint64, error) {
	flipped, _, err := model.FlipProvisioned(
		ctx, cli, cl.Id, target.Shard, target.Id, []model.SideRef{ref},
	)
	if err != nil {
		return false, 0, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return false, 0, err
	}
	return len(flipped) == 1, spRev, nil
}

// SetCloneProvisioned is SetProvisioned's twin for one side of a clone's
// clone meta group, through model.FlipCloneProvisioned, the clone-keyed
// twin of the provisioned flip (dnv-worker.md MD6). A case pulls it where it
// needs a provisioned side of such a group: a spare the switch takes
// (architecture.md, Spare legs), or a group with at most one unprovisioned
// spare. It returns the clone's id, whether the flip applied and the pool's
// revision after it.
//
// The clone_id comes from the stored record rather than from the caller, for
// SetCreated's reason: the op takes the clone's name and its id so that a
// clone deleted and re-created under the name between the read and the
// transaction is skipped. The op also skips, and so does not flip, a side
// already provisioned, a side that is not on the leg the caller names, and
// any side of a clone being deleted.
func SetCloneProvisioned(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cloneName string,
	legId uint64,
	sideId uint64,
) (uint64, bool, uint64, error) {
	if cloneName == "" {
		return 0, false, 0, fmt.Errorf("the clone name is empty")
	}
	key := model.CloneKey(cl.Id, target.Id, cloneName)
	clone := &pb.Clone{}
	found, err := cli.Get(ctx, key, clone)
	if err != nil {
		return 0, false, 0, err
	}
	if !found {
		return 0, false, 0, fmt.Errorf("key %q not found", key)
	}
	flipped, _, err := model.FlipCloneProvisioned(
		ctx, cli, cl.Id, target.Shard, target.Id,
		[]model.CloneSideRef{{
			CloneName: cloneName,
			CloneId:   clone.GetCloneId(),
			LegId:     legId,
			SideId:    sideId,
		}},
	)
	if err != nil {
		return 0, false, 0, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return 0, false, 0, err
	}
	return clone.GetCloneId(), len(flipped) == 1, spRev, nil
}

// Cutover is what StartCutover started: the migration it named, the cntlr
// the op checked is the primary, and the pool's revision after it.
type Cutover struct {
	Ref     model.CutoverRef
	CntlrId uint64
	SpRev   uint64
}

// StartCutover clears one Migration.cutover_pending through
// model.StartCutover: the cutover the sp worker starts beside its provisioned
// flip, once the pool's primary reports a live path to the migration's dst
// side and that side reports it waits for the cutover (dnv-worker.md RW18;
// architecture.md, Migrations). The gateway suite runs no worker, and
// FinishMigration refuses a migration whose cutover has not started, so this
// is how that suite takes a migration past it before a finish.
//
// The migration's leg comes from the stored record and slices (cutoverRefOf)
// and the cntlr the op checks is the pool's stored primary (storedPrimary)
// unless cntlrId names one. The op re-checks in its own STM what the store
// holds — the pool's level below SP_LEVEL_NO_MIGRATION, the flag still set,
// the dst side provisioned, the cntlr the primary and not disabled — and its
// refusal is returned, so a case that skipped the provisioned flip
// (SetProvisioned) learns it here rather than at the finish.
func StartCutover(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	migrName string,
	cntlrId uint64,
) (Cutover, error) {
	if migrName == "" {
		return Cutover{}, fmt.Errorf("the migration name is empty")
	}
	state, err := model.LoadSp(ctx, cli, cl.Id, target.Name)
	if err != nil {
		return Cutover{}, err
	}
	ref, err := cutoverRefOf(state, migrName)
	if err != nil {
		return Cutover{}, err
	}
	if cntlrId == 0 {
		if cntlrId, err = storedPrimary(state); err != nil {
			return Cutover{}, err
		}
	}
	_, err = model.StartCutover(
		ctx, cli, cl.Id, target.Shard, target.Id, target.Name, ref, cntlrId)
	if err != nil {
		return Cutover{}, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return Cutover{}, err
	}
	return Cutover{Ref: ref, CntlrId: cntlrId, SpRev: spRev}, nil
}

// cutoverRefOf names one migration of a load for model.StartCutover: its
// record's id and sides, and the slice and leg that carry both sides. It
// refuses a name the load has no record of and a record whose two sides
// share no leg, the two shapes no op could start a cutover on.
func cutoverRefOf(
	state *model.SpState,
	migrName string,
) (model.CutoverRef, error) {
	migr, ok := state.Migrs[migrName]
	if !ok {
		return model.CutoverRef{}, fmt.Errorf(
			"no migration %q in the pool", migrName)
	}
	ref := model.CutoverRef{
		MigrName:  migrName,
		MigrId:    migr.GetMigrId(),
		SrcSideId: migr.GetSrcSideId(),
		DstSideId: migr.GetDstSideId(),
	}
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		grpList := make([]*pb.Group, 0,
			len(slice.GetMetaGrpList())+len(slice.GetDataGrpList()))
		grpList = append(grpList, slice.GetMetaGrpList()...)
		grpList = append(grpList, slice.GetDataGrpList()...)
		for _, grp := range grpList {
			legList := make([]*pb.Leg, 0,
				len(grp.GetLegList())+len(grp.GetSpareLegList()))
			legList = append(legList, grp.GetLegList()...)
			legList = append(legList, grp.GetSpareLegList()...)
			for _, leg := range legList {
				src, dst := false, false
				for _, side := range leg.GetSideList() {
					src = src || side.GetSideId() == ref.SrcSideId
					dst = dst || side.GetSideId() == ref.DstSideId
				}
				if src && dst {
					ref.SliceId = sliceId
					ref.LegId = leg.GetLegId()
					return ref, nil
				}
			}
		}
	}
	return model.CutoverRef{}, fmt.Errorf(
		"migration %q: sides %#x and %#x share no leg",
		migrName, ref.SrcSideId, ref.DstSideId)
}

// storedPrimary returns the one cntlr of a load whose record says primary,
// refusing a pool with none or with more than one: the cntlr StartCutover
// names when its caller does not.
func storedPrimary(state *model.SpState) (uint64, error) {
	var primaries []uint64
	for _, cntlrId := range state.Conf.GetCntlrIdList() {
		if state.Cntlrs[cntlrId].GetPrimary() {
			primaries = append(primaries, cntlrId)
		}
	}
	if len(primaries) != 1 {
		return 0, fmt.Errorf(
			"want exactly one primary cntlr, the pool has %d", len(primaries))
	}
	return primaries[0], nil
}

// ---------------------------------------------------------------------------
// Latches and drains (dnv-worker.md, Package `model`; SPD3 to SPD13, CLD1 to
// CLD12)
// ---------------------------------------------------------------------------

// Latch is what a deleting latch did: the latched object's id, whether this
// call latched it (false when it was latched already) and the pool's
// revision after it.
type Latch struct {
	Id      uint64
	Latched bool
	SpRev   uint64
}

// SetDeleting latches an SP the way DeleteStoragePool does
// (architecture.md, Storage pools; gateway.md, Storage pools and GrowSlice):
// `deleting = true` plus ONE SpRev bump, and nothing else. A pool already
// latched is SPD3's no-op, so a case can re-run a stage without a second
// bump moving every token it holds.
//
// The worker suite runs no gateway, so this is how a case puts an SP into the
// state the sp coordinator's drain reacts to. It deliberately does NOT apply
// the five-empty-lists precondition: that gate is the gateway's, the library
// plants whatever shape a case needs, and re-implementing a public
// precondition in a test library is how the two drift apart.
//
// That precondition is also what lets the sp drain give back no metadata
// units and no clone meta group (architecture.md, Metadata units; Clones): a
// pool that still has a clone or a migration cannot be deleted. A pool
// latched here while it still holds a planted migration therefore leaves its
// units charged on its destination DN once it has drained, and a planted
// clone leaves its clone meta group charged on its DNs, its sides' pointers
// with it.
func SetDeleting(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
) (Latch, error) {
	cid := cl.Id
	var out Latch
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Latch{Id: target.Id}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if conf.GetDeleting() {
			out.SpRev = readSpRevIn(s, cid, target)
			return nil
		}
		conf.Deleting = true
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		out.Latched = true
		return err
	})
	if err != nil {
		return Latch{}, err
	}
	return out, nil
}

// SpDrain is what DrainSp ran: its steps, the cntlrs, repairs, retired legs
// and groups the steps removed, the slices they finished, and whether the
// pool is gone.
type SpDrain struct {
	Steps         int
	CntlrCnt      int
	RepairCnt     int
	RetiredLegCnt int
	GrpCnt        int
	SliceCnt      int
	Deleted       bool
}

// DrainSp runs a latched SP's drain to completion, or for maxSteps steps:
// SPD8's derivation in a loop over the five model ops of the sp drain, with
// no pass and no wake-up. A batch of repairs or of retired legs is derived
// from a load of the pool (model.SpDrainRepairBatch,
// model.SpDrainRetiredLegBatch).
//
// It is the gateway suite's stand-in for the sp coordinator, which that suite
// does not run — the same kind of worker-role write as SetProvisioned and
// SetCreated, and for the same reason: the gateway's END state after a delete
// cannot be asserted without SOMEBODY draining, and standing up a whole
// dnv-worker inside a gateway case would make every other assertion in it
// depend on a converging fleet.
//
// The worker suite drives exactly ONE batch through this, with maxSteps 1, to
// BUILD a partially drained SP rather than race one; the drain that finishes
// it is the real coordinator's, which is the point of that case. Running out
// of steps is therefore reported in Deleted, never an error.
func DrainSp(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	maxSteps int,
) (SpDrain, error) {
	cid := cl.Id
	var out SpDrain
	// Steps is incremented after the switch, not by a post statement, so
	// that the iteration which commits D3 counts once and still reports
	// Deleted: a budget sized to exactly the number of steps the drain needs
	// must not report a fully committed drain as unfinished.
	for out.Steps < maxSteps {
		conf := &pb.SpConf{}
		found, err := cli.Get(ctx, model.SpConfKey(cid, target.Name), conf)
		if err != nil {
			return out, err
		}
		if !found {
			// Already gone — by an earlier call, or by the real coordinator
			// racing this one. No step ran, so none is counted.
			out.Deleted = true
			break
		}
		if conf.GetSpId() != target.Id {
			return out, fmt.Errorf("storage pool %q changed its id",
				target.Name)
		}
		if !conf.GetDeleting() {
			return out, fmt.Errorf("storage pool %q is not being deleted",
				target.Name)
		}
		switch {
		case len(conf.GetCntlrIdList()) > 0:
			removed, err := model.DrainSpCntlrs(
				ctx, cli, cid, target.Shard, target.Id, target.Name)
			if err != nil {
				return out, fmt.Errorf("cntlrs: %w", err)
			}
			out.CntlrCnt += removed
		case len(conf.GetRepairNameList()) > 0:
			state, err := model.LoadSp(ctx, cli, cid, target.Name)
			if err != nil {
				return out, err
			}
			removed, err := model.DrainSpRepairs(
				ctx, cli, cid, target.Shard, target.Id, target.Name,
				model.SpDrainRepairBatch(state), cl.Conf)
			if err != nil {
				return out, fmt.Errorf("repairs: %w", err)
			}
			out.RepairCnt += removed
		case len(conf.GetRetiredLegIdList()) > 0:
			state, err := model.LoadSp(ctx, cli, cid, target.Name)
			if err != nil {
				return out, err
			}
			removed, err := model.DrainSpRetiredLegs(
				ctx, cli, cid, target.Shard, target.Id, target.Name,
				model.SpDrainRetiredLegBatch(state), cl.Conf)
			if err != nil {
				return out, fmt.Errorf("retired legs: %w", err)
			}
			out.RetiredLegCnt += removed
		case len(conf.GetSliceIdList()) > 0:
			removed, sliceDone, err := model.DrainSpSlice(
				ctx, cli, cid, target.Shard, target.Id, target.Name,
				lowestId(conf.GetSliceIdList()), cl.Conf)
			if err != nil {
				return out, fmt.Errorf("slice: %w", err)
			}
			out.GrpCnt += removed
			if sliceDone {
				out.SliceCnt++
			}
		default:
			if err := model.FinishSpDelete(
				ctx, cli, cid, target.Shard, target.Id, target.Name,
			); err != nil {
				return out, fmt.Errorf("final: %w", err)
			}
			out.Deleted = true
		}
		out.Steps++
		if out.Deleted {
			break
		}
	}
	return out, nil
}

// SetCloneDeleting latches one clone the way DeleteClone does (CLD4,
// architecture.md, Clones): `deleting = true` plus one SpRev bump. A clone
// already latched is CLD3's no-op, so a case can re-run a stage without a
// second bump moving every token it holds.
//
// It deliberately does NOT resume the destination namespaces, which the RPC
// does in the same transaction: that write belongs to the gateway's request
// validation and subsystem walk, the library plants whatever shape a case
// needs, and re-implementing a public action in a test library is how the
// two drift apart. A case that cares about the resume drives the real RPC.
func SetCloneDeleting(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cloneName string,
) (Latch, error) {
	if cloneName == "" {
		return Latch{}, fmt.Errorf("the clone name is empty")
	}
	cid := cl.Id
	var out Latch
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Latch{}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		key := model.CloneKey(cid, target.Id, cloneName)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return fmt.Errorf("clone %q not found", cloneName)
		}
		out.Id = clone.GetCloneId()
		if clone.GetDeleting() {
			out.SpRev = readSpRevIn(s, cid, target)
			return nil
		}
		clone.Deleting = true
		s.Put(key, clone)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		out.Latched = true
		return err
	})
	if err != nil {
		return Latch{}, err
	}
	return out, nil
}

// CloneDrain is what DrainClone ran: its steps, the chunk KEYS it removed
// (the Clone record carries no count of its own to confuse them with),
// whether the clone is gone, and, when this call ran the final step, the
// sides of the clone's clone meta group the records held as that step removed
// them — its legs' and spares', its repair's new legs' and its retired
// legs' — which the step gave back to their DNs.
type CloneDrain struct {
	Steps      int
	ChunkCnt   int
	Deleted    bool
	GrpSideCnt int
}

// DrainClone runs a latched clone's drain to completion, or for maxSteps
// steps: CLD7's derivation in a loop over model.DrainCloneBm and
// model.FinishCloneDelete, with no pass and no wake-up.
//
// It is the GATEWAY suite's stand-in for the sp coordinator, which that suite
// does not run — the clone twin of DrainSp, and there for the same reason:
// the gateway's end state after a clone delete cannot be asserted without
// somebody draining. The worker suite drives exactly ONE batch through it,
// with maxSteps 1, to BUILD case G's restart fixture; the drain that finishes
// that clone, and every step of the clone before it, is the real
// coordinator's.
//
// Its last step, model.FinishCloneDelete, gives the sides of the clone's
// clone meta group back to their DNs (dnv-worker.md CLD9), which moves their
// capacity keys, so it is handed the cluster's stored conf, cl.Conf, as
// DrainSp's slice batches are. Both suites read that release after a drain.
//
// A clone that was never there is a caller error, not a finished drain:
// Deleted must mean "this call, or an earlier one, removed it", so the
// existence check happens once, up front.
func DrainClone(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	cloneName string,
	maxSteps int,
) (CloneDrain, error) {
	if cloneName == "" {
		return CloneDrain{}, fmt.Errorf("the clone name is empty")
	}
	cid := cl.Id
	key := model.CloneKey(cid, target.Id, cloneName)
	found, err := cli.Get(ctx, key, &pb.Clone{})
	if err != nil {
		return CloneDrain{}, err
	}
	if !found {
		return CloneDrain{}, fmt.Errorf("clone %q not found", cloneName)
	}
	var out CloneDrain
	for out.Steps < maxSteps {
		clone := &pb.Clone{}
		found, err := cli.Get(ctx, key, clone)
		if err != nil {
			return out, err
		}
		if !found {
			out.Deleted = true
			break
		}
		if !clone.GetDeleting() {
			return out, fmt.Errorf("clone %q is not being deleted", cloneName)
		}
		batch, err := cloneChunks(ctx, cli, cid, target.Id, cloneName)
		if err != nil {
			return out, err
		}
		if len(batch) > common.MaxDelBmPerTxn {
			batch = batch[:common.MaxDelBmPerTxn]
		}
		if len(batch) == 0 {
			sideCnt, err := cloneGrpSideCnt(
				ctx, cli, cid, target.Id, clone.GetMetaGrp())
			if err != nil {
				return out, err
			}
			err = model.FinishCloneDelete(ctx, cli, cid, target.Shard,
				target.Id, target.Name, cloneName, clone.GetCloneId(),
				cl.Conf)
			if err != nil {
				return out, fmt.Errorf("final: %w", err)
			}
			out.Deleted = true
			out.GrpSideCnt = sideCnt
		} else {
			removed, err := model.DrainCloneBm(ctx, cli, cid,
				target.Id, target.Name, cloneName, clone.GetCloneId(), batch)
			if err != nil {
				return out, fmt.Errorf("batch: %w", err)
			}
			out.ChunkCnt += removed
		}
		out.Steps++
		if out.Deleted {
			break
		}
	}
	return out, nil
}

// cloneGrpSideCnt counts the sides the clone drain's final step gives back
// for grp: those of its legs and spares, of its repair's new legs and of its
// retired legs. A record the group names that is missing counts nothing:
// the final step refuses it.
func cloneGrpSideCnt(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	grp *pb.Group,
) (int, error) {
	legs := append(
		append([]*pb.Leg(nil), grp.GetLegList()...), grp.GetSpareLegList()...)
	if repName := grp.GetRepName(); repName != "" {
		rep := &pb.Repair{}
		if _, err := cli.Get(
			ctx, model.RepairKey(cid, spId, repName), rep); err != nil {
			return 0, err
		}
		legs = append(legs, rep.GetNewLegList()...)
	}
	cnt := len(grp.GetRetiredLegIdList())
	for _, leg := range legs {
		cnt += len(leg.GetSideList())
	}
	return cnt, nil
}

// cloneChunks is MD3's keys-only scan of one clone's chunk prefix, in
// ascending (src_slice_idx, bm_idx) order — the surviving set a drain step
// derives its position from (CLD7).
func cloneChunks(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	cloneName string,
) ([]model.BmChunk, error) {
	prefix := model.CloneBitmapPrefix(cid, spId, cloneName)
	keys, _, err := cli.RangeKeys(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]model.BmChunk, 0, len(keys))
	for _, entry := range keys {
		sliceIdx, bmIdx, ok := model.ParseCloneBmKey(entry.Key)
		if !ok {
			continue
		}
		out = append(out, model.BmChunk{
			SliceIdx: sliceIdx, Idx: bmIdx, ModRev: entry.ModRev,
		})
	}
	return out, nil
}

// lowestId is SPD10's target rule: the LOWEST listed id, so that two drivers
// pick the same one however the list is stored.
func lowestId(ids []uint64) uint64 {
	lowest := ids[0]
	for _, id := range ids[1:] {
		if id < lowest {
			lowest = id
		}
	}
	return lowest
}
