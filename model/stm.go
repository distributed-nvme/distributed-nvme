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
// the chunk index parsed out of the key, and the key's mod_revision, which BM5
// memoizes to tell a changed bitmap from an unchanged one without reading a
// single byte of it.
type BmChunk struct {
	Idx    uint32
	ModRev int64
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
	// CloneBmIdx and MigrBmIdx are keyed by clone / migration name.
	CloneBmIdx map[string][]BmChunk
	MigrBmIdx  map[string][]BmChunk
	// DnByAddr holds one record per distinct Side.addr_port of the SP,
	// spare legs included; CnByAddr one per distinct Cntlr.addr_port.
	DnByAddr map[string]*pb.DnConf
	CnByAddr map[string]*pb.CnConf
	// Missing lists the keys the SpConf pointed at that did not exist.
	Missing []string
}

// newSpState builds an SpState with every map allocated, so that a caller
// never has to nil-check one.
func newSpState() *SpState {
	return &SpState{
		Cntlrs:     make(map[uint64]*pb.Cntlr),
		Slices:     make(map[uint64]*pb.Slice),
		Subsystems: make(map[string]*pb.Subsystem),
		Clones:     make(map[string]*pb.Clone),
		Xfers:      make(map[string]*pb.Transfer),
		Migrs:      make(map[string]*pb.Migration),
		CloneBmIdx: make(map[string][]BmChunk),
		MigrBmIdx:  make(map[string][]BmChunk),
		DnByAddr:   make(map[string]*pb.DnConf),
		CnByAddr:   make(map[string]*pb.CnConf),
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

// sideAddrs walks the slices of the SP in Conf.slice_id_list order and returns
// every distinct Side.addr_port — both groups of a slice, and both the active
// legs and the spare legs of a group, because a spare's side is provisioned
// and health-checked exactly like an active one (§8.12).
func sideAddrs(conf *pb.SpConf, slices map[uint64]*pb.Slice) []string {
	var addrs []string
	seen := make(map[string]struct{})
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
				legLists := [][]*pb.Leg{
					grp.GetLegList(),
					grp.GetSpareLegList(),
				}
				for _, legList := range legLists {
					for _, leg := range legList {
						for _, side := range leg.GetSideList() {
							addrs = appendAddr(
								addrs, seen, side.GetAddrPort(),
							)
						}
					}
				}
			}
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

// loadSpConf reads the SpConf and every sub-object it lists into a fresh
// SpState. It is the body of the MD3 snapshot; the bitmap indexes are not part
// of it, because an STM cannot range.
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
	// The node records come last: their addresses are embedded in the
	// slices and cntlrs read above. They are part of the same snapshot
	// because a side's syncup carries the DN's nvme_tr_conf and a cntlr's
	// the CN's (§10.3), and a reaction weighs their free_ext_cnt.
	for _, addrPort := range sideAddrs(conf, state.Slices) {
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
	return state, nil
}

// loadBmIdx scans one bitmap prefix keys-only at rev and returns the chunks it
// found (MD3). A key that does not parse is skipped: nothing in dnv writes one
// under this prefix, and one stray key must not fail a whole SP load.
func loadBmIdx(
	ctx context.Context,
	cli *etcdutil.Client,
	prefix string,
	rev int64,
) ([]BmChunk, error) {
	keyRevs, err := cli.RangeKeysAtRev(ctx, prefix, rev)
	if err != nil {
		return nil, fmt.Errorf("load bitmap index %s: %w", prefix, err)
	}
	chunks := make([]BmChunk, 0, len(keyRevs))
	for _, keyRev := range keyRevs {
		bmIdx, ok := ParseBmIdx(keyRev.Key)
		if !ok {
			continue
		}
		chunks = append(chunks, BmChunk{
			Idx:    bmIdx,
			ModRev: keyRev.ModRev,
		})
	}
	return chunks, nil
}

// LoadSp reads one SP's whole desired state (MD3).
//
// Everything but the bitmap indexes is read in ONE etcdutil snapshot, so the
// SpConf and every sub-object it lists come from the same store revision and
// can never disagree. The bitmap indexes are then two keys-only scans per
// clone / migration, run OUTSIDE that snapshot but pinned to the very
// revision it was served at (SpState.Rev): an STM cannot range, and MD3
// accepts the split because bitmap key sets only ever grow (§8.9/§8.11), so
// even a slightly newer view would be harmless.
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
	spId := state.Conf.GetSpId()
	for _, cloneName := range state.Conf.GetCloneNameList() {
		if _, ok := state.Clones[cloneName]; !ok {
			continue
		}
		chunks, err := loadBmIdx(
			ctx, cli, CloneBitmapPrefix(cid, spId, cloneName), rev,
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
		)
		if err != nil {
			return nil, err
		}
		state.MigrBmIdx[migrName] = chunks
	}
	return state, nil
}
