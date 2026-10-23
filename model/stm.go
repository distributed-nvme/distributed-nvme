package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ErrNotFound is what a loader returns when the key that anchors the object it
// was asked for does not exist (MD3). For LoadSp that is the SpConf: the SP is
// being deleted and the delete of its rev key follows, so the sp-role worker
// treats it as "nothing to drive any more", not as a failure. Callers test it
// with errors.Is.
var ErrNotFound = errors.New("model: not found")

// BmChunk is one chunk of a clone's or a migration's bitmap as MD3 reports it:
// the chunk's address parsed out of the key, and the key's mod_revision, which
// BM5 memoizes to tell a changed bitmap from an unchanged one without reading a
// single byte of it.
//
// For a clone the pair (SliceIdx, Idx) is the chunk's full address: Idx names
// the chunk's fixed position inside source slice SliceIdx's bitmap
// (architecture.md, Bitmap push protocol).
// SliceIdx is ALWAYS 0 for a migration chunk, whose bm_idx is an append
// sequence over the leg's one bitmap and names no slice.
type BmChunk struct {
	SliceIdx uint32
	Idx      uint32
	ModRev   int64
}

// SpState is everything the sp role fans out or reacts on for one SP (MD3),
// read at ONE store revision.
//
// Rev is that revision. Sub-object maps hold only the keys that existed;
// anything the SpConf lists but etcd does not have is reported in Missing
// instead, as the full key, and the load still succeeds — a worker logs those
// and drives what it can rather than stalling an SP on one lost sub-object.
//
// Bitmap VALUES are never part of this: a push reads one chunk at a time
// (BM3). CloneBmIdx and MigrBmIdx carry only which chunks exist, one entry per
// loaded clone / migration (an empty slice when it has none yet).
type SpState struct {
	// Rev is the store revision every field below was read at.
	Rev int64
	// SpRevision is the SP's SpRev.revision, read in the same snapshot; 0
	// when that key is absent. It is the revision the loaded state belongs to,
	// which the sp role's fan-out compares with the one it was delivered
	// (dnv-worker.md RW14).
	SpRevision uint64
	// Conf is the SP's configuration; it is never nil.
	Conf *pb.SpConf
	// Cntlrs is keyed by cntlr_id, from Conf.cntlr_id_list.
	Cntlrs map[uint64]*pb.Cntlr
	// Slices is keyed by slice_id, from Conf.slice_id_list.
	Slices map[uint64]*pb.Slice
	// Tds is in Conf.td_name_list order; TdNames is the matching name of
	// each entry, so Tds[i] is always the record named TdNames[i].
	Tds     []*pb.ThinDevice
	TdNames []string
	// Subsystems is keyed by nqn, from Conf.nqn_list.
	Subsystems map[string]*pb.Subsystem
	// Clones, Xfers and Migrs are keyed by name, from the matching
	// Conf.*_name_list.
	Clones map[string]*pb.Clone
	Xfers  map[string]*pb.Transfer
	Migrs  map[string]*pb.Migration
	// Repairs is keyed by rep_name, from Conf.repair_name_list, and
	// RetiredLegs by leg_id, from Conf.retired_leg_id_list. Each is read by
	// one range of its prefix at Rev rather than a read per key, and only
	// the keys a list names are kept.
	Repairs     map[string]*pb.Repair
	RetiredLegs map[uint64]*pb.RetiredLeg
	// CloneBmIdx and MigrBmIdx are keyed by clone / migration name. A
	// clone's entries carry the (SliceIdx, Idx) pair of every chunk key
	// found, in ascending pair order; a migration's carry the append
	// sequence in Idx alone, with SliceIdx 0.
	CloneBmIdx map[string][]BmChunk
	MigrBmIdx  map[string][]BmChunk
	// DnByAddr holds one record per distinct Side.addr_port of the SP,
	// spare legs, the sides of every clone meta group, the new legs of the
	// repairs and the retired sides included; CnByAddr one per distinct
	// Cntlr.addr_port.
	DnByAddr map[string]*pb.DnConf
	CnByAddr map[string]*pb.CnConf
	// Missing lists the keys the SpConf pointed at that did not exist.
	Missing []string
}

// newSpState builds an SpState with every map allocated, so that a caller
// never has to nil-check one.
func newSpState() *SpState {
	return &SpState{
		Cntlrs:      make(map[uint64]*pb.Cntlr),
		Slices:      make(map[uint64]*pb.Slice),
		Subsystems:  make(map[string]*pb.Subsystem),
		Clones:      make(map[string]*pb.Clone),
		Xfers:       make(map[string]*pb.Transfer),
		Migrs:       make(map[string]*pb.Migration),
		Repairs:     make(map[string]*pb.Repair),
		RetiredLegs: make(map[uint64]*pb.RetiredLeg),
		CloneBmIdx:  make(map[string][]BmChunk),
		MigrBmIdx:   make(map[string][]BmChunk),
		DnByAddr:    make(map[string]*pb.DnConf),
		CnByAddr:    make(map[string]*pb.CnConf),
	}
}

// miss records one listed-but-absent key (MD3).
func (state *SpState) miss(key string) {
	state.Missing = append(state.Missing, key)
}

// appendAddr adds one addr_port to list unless seen already, keeping the
// first-seen order so that the node reads of one load are deterministic.
func appendAddr(
	list []string,
	seen map[string]struct{},
	addrPort string,
) []string {
	if addrPort == "" {
		return list
	}
	if _, ok := seen[addrPort]; ok {
		return list
	}
	seen[addrPort] = struct{}{}
	return append(list, addrPort)
}

// sideAddrs walks the slices of the SP in Conf.slice_id_list order, the
// clones in Conf.clone_name_list order, the repairs in Conf.repair_name_list
// order and the retired legs in Conf.retired_leg_id_list order, and returns
// every distinct Side.addr_port — both groups of a slice and the clone meta
// group of a clone, a latched clone's included, since its sides stay pointed
// at from their DNs until the clone drain's final transaction; both the
// active legs and the spare legs of a group, because a spare's side sits on
// its DN exactly like an active one's (architecture.md, Spare legs); the new
// legs of every repair, which live in the repair until it is finished; and
// every retired leg's side, which keeps its place on its DN. The walk keeps
// that order so that one load's node reads keep a fixed order; a clone with
// no group adds nothing.
func sideAddrs(state *SpState) []string {
	conf := state.Conf
	var addrs []string
	seen := make(map[string]struct{})
	addLegs := func(legs []*pb.Leg) {
		for _, leg := range legs {
			for _, side := range leg.GetSideList() {
				addrs = appendAddr(addrs, seen, side.GetAddrPort())
			}
		}
	}
	addGrp := func(grp *pb.Group) {
		addLegs(legsOf(grp))
	}
	slices, clones := state.Slices, state.Clones
	for _, sliceId := range conf.GetSliceIdList() {
		slice, ok := slices[sliceId]
		if !ok {
			continue
		}
		grpLists := [][]*pb.Group{
			slice.GetMetaGrpList(),
			slice.GetDataGrpList(),
		}
		for _, grpList := range grpLists {
			for _, grp := range grpList {
				addGrp(grp)
			}
		}
	}
	for _, cloneName := range conf.GetCloneNameList() {
		clone, ok := clones[cloneName]
		if !ok {
			continue
		}
		addGrp(clone.GetMetaGrp())
	}
	for _, repName := range conf.GetRepairNameList() {
		if rep, ok := state.Repairs[repName]; ok {
			addLegs(rep.GetNewLegList())
		}
	}
	for _, legId := range conf.GetRetiredLegIdList() {
		if retired, ok := state.RetiredLegs[legId]; ok {
			addrs = appendAddr(
				addrs, seen, retired.GetSide().GetAddrPort())
		}
	}
	return addrs
}

// cntlrAddrs returns every distinct Cntlr.addr_port of the SP, in
// Conf.cntlr_id_list order.
func cntlrAddrs(conf *pb.SpConf, cntlrs map[uint64]*pb.Cntlr) []string {
	var addrs []string
	seen := make(map[string]struct{})
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr, ok := cntlrs[cntlrId]
		if !ok {
			continue
		}
		addrs = appendAddr(addrs, seen, cntlr.GetAddrPort())
	}
	return addrs
}

// loadSpConf reads the SpConf and every sub-object it lists by a key of its
// own into a fresh SpState. It is the first part of the MD3 snapshot; the
// repairs, the retired legs and the bitmap indexes are not part of it,
// because an STM cannot range, and the node records follow the repairs and
// the retired legs, whose sides name nodes too (loadNodes).
func loadSpConf(
	s etcdutil.STM,
	cid uint64,
	spName string,
) (*SpState, error) {
	state := newSpState()
	conf := &pb.SpConf{}
	if !s.Get(SpConfKey(cid, spName), conf) {
		return nil, fmt.Errorf("load sp %s: %w", spName, ErrNotFound)
	}
	state.Conf = conf
	spId := conf.GetSpId()
	// The rev key is not a listed sub-object: an absent one is no Missing
	// entry, it leaves SpRevision 0.
	spRev := &pb.SpRev{}
	if s.Get(SpRevKey(conf.GetShardCode(), cid, spId), spRev) {
		state.SpRevision = spRev.GetRevision()
	}
	for _, cntlrId := range conf.GetCntlrIdList() {
		key := CntlrKey(cid, spId, cntlrId)
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			state.miss(key)
			continue
		}
		state.Cntlrs[cntlrId] = cntlr
	}
	for _, sliceId := range conf.GetSliceIdList() {
		key := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			state.miss(key)
			continue
		}
		state.Slices[sliceId] = slice
	}
	for _, tdName := range conf.GetTdNameList() {
		key := ThinDeviceKey(cid, spId, tdName)
		td := &pb.ThinDevice{}
		if !s.Get(key, td) {
			state.miss(key)
			continue
		}
		// Tds and TdNames stay index-aligned: a missing td contributes to
		// neither.
		state.Tds = append(state.Tds, td)
		state.TdNames = append(state.TdNames, tdName)
	}
	for _, nqn := range conf.GetNqnList() {
		key := SubsystemKey(cid, spId, nqn)
		subsystem := &pb.Subsystem{}
		if !s.Get(key, subsystem) {
			state.miss(key)
			continue
		}
		state.Subsystems[nqn] = subsystem
	}
	for _, cloneName := range conf.GetCloneNameList() {
		key := CloneKey(cid, spId, cloneName)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			state.miss(key)
			continue
		}
		state.Clones[cloneName] = clone
	}
	for _, xferName := range conf.GetXferNameList() {
		key := TransferKey(cid, spId, xferName)
		xfer := &pb.Transfer{}
		if !s.Get(key, xfer) {
			state.miss(key)
			continue
		}
		state.Xfers[xferName] = xfer
	}
	for _, migrName := range conf.GetMigrNameList() {
		key := MigrationKey(cid, spId, migrName)
		migr := &pb.Migration{}
		if !s.Get(key, migr) {
			state.miss(key)
			continue
		}
		state.Migrs[migrName] = migr
	}
	return state, nil
}

// loadRepairs reads every repair and every retired leg of the SP by one range
// of each prefix at the snapshot's revision (MD3) and keeps the records the
// SpConf lists, in Conf.repair_name_list and Conf.retired_leg_id_list order;
// a listed key the range did not find lands in Missing, in that order. A
// key under the prefixes that no list names is not kept: no writer leaves
// one, and nothing could reach it.
func loadRepairs(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	state *SpState,
) error {
	spId := state.Conf.GetSpId()
	ranged := func(prefix string) (map[string]etcdutil.KV, error) {
		kvs, err := cli.RangeAtRev(ctx, prefix, state.Rev)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", prefix, err)
		}
		byKey := make(map[string]etcdutil.KV, len(kvs))
		for _, kv := range kvs {
			byKey[kv.Key] = kv
		}
		return byKey, nil
	}
	repairs, err := ranged(RepairPrefix(cid, spId))
	if err != nil {
		return err
	}
	for _, repName := range state.Conf.GetRepairNameList() {
		key := RepairKey(cid, spId, repName)
		kv, ok := repairs[key]
		if !ok {
			state.miss(key)
			continue
		}
		rep := &pb.Repair{}
		if err := cli.Decode(ctx, kv, rep); err != nil {
			return err
		}
		state.Repairs[repName] = rep
	}
	retiredLegs, err := ranged(RetiredLegPrefix(cid, spId))
	if err != nil {
		return err
	}
	for _, legId := range state.Conf.GetRetiredLegIdList() {
		key := RetiredLegKey(cid, spId, legId)
		kv, ok := retiredLegs[key]
		if !ok {
			state.miss(key)
			continue
		}
		retired := &pb.RetiredLeg{}
		if err := cli.Decode(ctx, kv, retired); err != nil {
			return err
		}
		state.RetiredLegs[legId] = retired
	}
	return nil
}

// loadNodesAt reads the node records of the SP into state (loadNodes) in a
// snapshot pinned to SpState.Rev, the revision of the load's first snapshot
// (MD3), as loadRepairs pins its two ranges.
func loadNodesAt(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	state *SpState,
) error {
	return cli.SnapshotAtRev(ctx, state.Rev, func(s etcdutil.STM) error {
		loadNodes(s, cid, state)
		return nil
	})
}

// loadNodes reads the node records of the SP into state, at the snapshot's
// revision (MD3). They come last: their addresses are embedded in the
// slices, clones, repairs, retired legs and cntlrs read before. They are
// part of the same snapshot because a side's request resolves its DN's
// endpoint and a cntlr's its CN's (dnv-worker.md RW14), and leg repair
// excludes the locations of a group's DNs (dnv-worker.md AR8).
func loadNodes(s etcdutil.STM, cid uint64, state *SpState) {
	conf := state.Conf
	for _, addrPort := range sideAddrs(state) {
		key := DnConfKey(cid, addrPort)
		dn := &pb.DnConf{}
		if !s.Get(key, dn) {
			state.miss(key)
			continue
		}
		state.DnByAddr[addrPort] = dn
	}
	for _, addrPort := range cntlrAddrs(conf, state.Cntlrs) {
		key := CnConfKey(cid, addrPort)
		cn := &pb.CnConf{}
		if !s.Get(key, cn) {
			state.miss(key)
			continue
		}
		state.CnByAddr[addrPort] = cn
	}
}

// bmKeyParser decodes one scanned bitmap key into the chunk address it carries
// (MD2): ParseCloneBmKey for a clone prefix, migrBmChunk for a migration one.
type bmKeyParser func(key string) (sliceIdx uint32, bmIdx uint32, ok bool)

// migrBmChunk adapts ParseBmIdx to the bmKeyParser shape: a migration bm_idx
// names no slice, so the chunk's address is (0, bm_idx).
func migrBmChunk(key string) (uint32, uint32, bool) {
	bmIdx, ok := ParseBmIdx(key)
	return 0, bmIdx, ok
}

// loadBmIdx scans one bitmap prefix keys-only at rev and returns the chunks it
// found (MD3), decoding each key with the parser of that prefix's kind. A key
// that does not parse is skipped: nothing dnv writes lands under this prefix in
// another shape, and one stray key of another shape must not fail a whole SP
// load.
func loadBmIdx(
	ctx context.Context,
	cli *etcdutil.Client,
	prefix string,
	rev int64,
	parse bmKeyParser,
) ([]BmChunk, error) {
	keyRevs, err := cli.RangeKeysAtRev(ctx, prefix, rev)
	if err != nil {
		return nil, fmt.Errorf("load bitmap index %s: %w", prefix, err)
	}
	chunks := make([]BmChunk, 0, len(keyRevs))
	for _, keyRev := range keyRevs {
		sliceIdx, bmIdx, ok := parse(keyRev.Key)
		if !ok {
			continue
		}
		chunks = append(chunks, BmChunk{
			SliceIdx: sliceIdx,
			Idx:      bmIdx,
			ModRev:   keyRev.ModRev,
		})
	}
	return chunks, nil
}

// LoadSp reads one SP's whole desired state (MD3).
//
// Everything is read at ONE store revision, so the SpConf and every
// sub-object it lists can never disagree. An etcdutil snapshot reads the
// SpConf and every sub-object it lists by a key of its own and fixes the
// revision (SpState.Rev). The repairs and the retired legs are then one range
// of each prefix pinned to that revision: a pool holds up to one repair and
// common.MaxRetiredLegPerGrp retired legs per group, too many for a point
// read each in the load's time budget. The node records follow in a snapshot
// pinned to the same revision, since the repairs' new legs and the retired
// legs name nodes too. The bitmap indexes are last, two keys-only scans per
// clone / migration pinned the same way: an STM cannot range, and MD3
// accepts the split because the pin is what keeps the reads consistent —
// none of the ranged key sets is append-only: the clone drain deletes chunk
// keys (CLD8), FinishMigration/CancelMigration delete a migration's whole run
// (architecture.md, Migrations), and repairs and retired legs come and go,
// so a newer view could disagree with the snapshot.
//
// A missing SpConf returns ErrNotFound (the SP is being deleted; the delete of
// its rev key follows). Every other listed key that is absent lands in
// SpState.Missing and the load still succeeds.
func LoadSp(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spName string,
) (*SpState, error) {
	var state *SpState
	rev, err := cli.SnapshotRev(ctx, func(s etcdutil.STM) error {
		loaded, err := loadSpConf(s, cid, spName)
		if err != nil {
			return err
		}
		state = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	state.Rev = rev
	if err := loadRepairs(ctx, cli, cid, state); err != nil {
		return nil, err
	}
	if err := loadNodesAt(ctx, cli, cid, state); err != nil {
		return nil, err
	}
	spId := state.Conf.GetSpId()
	for _, cloneName := range state.Conf.GetCloneNameList() {
		if _, ok := state.Clones[cloneName]; !ok {
			continue
		}
		chunks, err := loadBmIdx(
			ctx, cli, CloneBitmapPrefix(cid, spId, cloneName), rev,
			ParseCloneBmKey,
		)
		if err != nil {
			return nil, err
		}
		state.CloneBmIdx[cloneName] = chunks
	}
	for _, migrName := range state.Conf.GetMigrNameList() {
		if _, ok := state.Migrs[migrName]; !ok {
			continue
		}
		chunks, err := loadBmIdx(
			ctx, cli, MigrBitmapPrefix(cid, spId, migrName), rev,
			migrBmChunk,
		)
		if err != nil {
			return nil, err
		}
		state.MigrBmIdx[migrName] = chunks
	}
	return state, nil
}
