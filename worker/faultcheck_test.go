package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The rules the fault schedule checks (TestWholeWorkerFaultSchedule), read
// from every write of the harness's etcd in the order etcd applied them, so
// that no set, clear and set again between two polls goes unseen:
//
//   - a revision never goes back: each put of a DnRev, CnRev or SpRev key
//     carries a revision at least the key's last one (architecture.md,
//     Revision keys and the sync fan-out);
//   - a set health epoch changes only when it is cleared: no put of a dn,
//     a cn, a cntlr, a leg or a side moves its err_epoch from one nonzero
//     value to another, wherever the object's record is, a slice's group, a
//     repair's new leg or a retired leg (HL3, MD6);
//   - a group has one repair at most (architecture.md, Repairs);
//   - the legs of a group sit on different disk nodes: no disk node holds
//     a side of two legs of one group, and no repair's new leg sits on the
//     disk node of another leg of its group, save a leg taken back from a
//     retired leg, which may sit on its bad leg's (architecture.md,
//     Per-operation allocation).
//
// The last two are checked on the state after all the writes of one store
// revision, since a transaction such as FinishRepair moves a leg from one
// record to another; the first two on each write. The writes of one
// transaction share one store revision, and the watch hands them over in
// order, so a revision's writes are applied together once the first write
// of a later revision arrives; sync writes a key of its own to close the
// revision it waits for.

// The faultKind* values are the message kinds of the keys the check decodes,
// their second fields, read from the key functions of model rather than
// copied.
var (
	faultKindDnRev   = keyKind(model.DnRevKey(0, 0, 0))
	faultKindCnRev   = keyKind(model.CnRevKey(0, 0, 0))
	faultKindSpRev   = keyKind(model.SpRevKey(0, 0, 0))
	faultKindDnConf  = keyKind(model.DnConfKey(0, "a:1"))
	faultKindCnConf  = keyKind(model.CnConfKey(0, "a:1"))
	faultKindCntlr   = keyKind(model.CntlrKey(0, 0, 0))
	faultKindSlice   = keyKind(model.SliceKey(0, 0, 0))
	faultKindRepair  = keyKind(model.RepairKey(0, 0, "r"))
	faultKindRetired = keyKind(model.RetiredLegKey(0, 0, 0))
)

// keyKind is the message kind of a dnv key, its second field.
func keyKind(key string) string {
	fields := strings.Split(key, " ")
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// faultCheck is the check's state, fed by one watch of the whole dnv key
// space from the store's first revision.
type faultCheck struct {
	mu sync.Mutex
	// done is the highest store revision whose writes are checked, and
	// writes how many writes were; heard is the highest store revision the
	// watch has handed over a write of, every one below it checked.
	done   int64
	writes int
	heard  int64
	// found are the broken rules, in the order found.
	found []string
	// ended is set once the watch ends, with its error.
	ended bool
	err   error

	// The rest belongs to the watch's goroutine.
	revs    map[string]uint64
	epochs  map[string]uint64
	slices  map[string]*pb.Slice
	repairs map[string]*pb.Repair
	seen    map[string]bool
}

// startFaultCheck opens the watch and checks every write from the store's
// first revision on; the case's cleanup ends it, ahead of the harness's
// cleanup, which stops the etcd.
func startFaultCheck(t *testing.T, h *workerHarness) *faultCheck {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fc := newFaultCheck()
	// Each prefix of dnv holds one message type (etcdutil.WatchTyped), and
	// this watch spans them all: a put decodes into an Empty, which keeps
	// the value's bytes as unknown fields, and record decodes those bytes by
	// the key's kind.
	events, errs := h.cli.WatchTyped(ctx, common.DnvPrefix+" ", 1,
		func() proto.Message { return &emptypb.Empty{} })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fc.run(events, errs)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return fc
}

// newFaultCheck is a check with nothing read yet.
func newFaultCheck() *faultCheck {
	return &faultCheck{
		revs:    make(map[string]uint64),
		epochs:  make(map[string]uint64),
		slices:  make(map[string]*pb.Slice),
		repairs: make(map[string]*pb.Repair),
		seen:    make(map[string]bool),
	}
}

// run reads the watch's events, one store revision's writes at a time: the
// writes of one revision, one transaction's, are applied together once the
// first write of a later revision arrives.
func (fc *faultCheck) run(events <-chan etcdutil.Event, errs <-chan error) {
	var batch []etcdutil.Event
	for ev := range events {
		if len(batch) > 0 && batch[0].Rev != ev.Rev {
			fc.apply(batch)
			batch = batch[:0]
		}
		batch = append(batch, ev)
		fc.mu.Lock()
		fc.heard = ev.Rev
		fc.mu.Unlock()
	}
	fc.apply(batch)
	err := <-errs
	fc.mu.Lock()
	fc.ended, fc.err = true, err
	fc.mu.Unlock()
}

// apply checks one store revision's writes.
func (fc *faultCheck) apply(batch []etcdutil.Event) {
	if len(batch) == 0 {
		return
	}
	var found []string
	placed := false
	for _, ev := range batch {
		f, p := fc.record(ev)
		found = append(found, f...)
		placed = placed || p
	}
	if placed {
		for _, f := range fc.placement() {
			// A broken placement stands until a later write ends it; it is
			// reported once.
			if !fc.seen[f] {
				fc.seen[f] = true
				found = append(found, f)
			}
		}
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, f := range found {
		fc.found = append(fc.found, fmt.Sprintf("store revision %d: %s",
			batch[0].Rev, f))
	}
	fc.done = batch[0].Rev
	fc.writes += len(batch)
}

// record applies one write and checks the first two rules on it; placed
// says whether it wrote a slice or a repair, whose placement the last two
// rules read.
func (fc *faultCheck) record(ev etcdutil.Event) (found []string, placed bool) {
	kind := keyKind(ev.Key)
	fields := strings.Split(ev.Key, " ")
	switch kind {
	case faultKindSlice:
		placed = true
		if ev.Type == etcdutil.EventDelete {
			delete(fc.slices, ev.Key)
			return nil, true
		}
	case faultKindRepair:
		placed = true
		if ev.Type == etcdutil.EventDelete {
			delete(fc.repairs, ev.Key)
			return nil, true
		}
	}
	if ev.Type == etcdutil.EventDelete {
		// Deletes end no rule: ids are never reused in a pool, and a
		// revision key keeps its last revision past a delete.
		return nil, placed
	}
	raw, err := proto.Marshal(ev.Msg)
	if err != nil {
		return []string{fmt.Sprintf("%s: re-marshal: %v", ev.Key, err)}, placed
	}
	decode := func(msg proto.Message) bool {
		if err := proto.Unmarshal(raw, msg); err != nil {
			found = append(found, fmt.Sprintf("%s: decode: %v", ev.Key, err))
			return false
		}
		return true
	}
	// pool is the sp_id field of a key of a pool's record.
	pool := func() string {
		if len(fields) > 3 {
			return fields[3]
		}
		return "?"
	}
	epoch := func(id string, e uint64) {
		if prev := fc.epochs[id]; prev != 0 && e != 0 && e != prev {
			found = append(found, fmt.Sprintf(
				"%s: err_epoch moved from %d to %d without a clear (%s)",
				id, prev, e, ev.Key))
		}
		fc.epochs[id] = e
	}
	legs := func(sp string, list []*pb.Leg) {
		for _, leg := range list {
			epoch(fmt.Sprintf("pool %s leg %d", sp, leg.GetLegId()), leg.GetErrEpoch())
			for _, side := range leg.GetSideList() {
				epoch(fmt.Sprintf("pool %s side %d", sp, side.GetSideId()),
					side.GetErrEpoch())
			}
		}
	}
	revision := func(rev uint64) {
		if prev, ok := fc.revs[ev.Key]; ok && rev < prev {
			found = append(found, fmt.Sprintf("%s: revision went back from %d to %d",
				ev.Key, prev, rev))
		}
		fc.revs[ev.Key] = rev
	}
	switch kind {
	case faultKindDnRev:
		msg := &pb.DnRev{}
		if decode(msg) {
			revision(msg.GetRevision())
		}
	case faultKindCnRev:
		msg := &pb.CnRev{}
		if decode(msg) {
			revision(msg.GetRevision())
		}
	case faultKindSpRev:
		msg := &pb.SpRev{}
		if decode(msg) {
			revision(msg.GetRevision())
		}
	case faultKindDnConf:
		msg := &pb.DnConf{}
		if decode(msg) {
			epoch(ev.Key, msg.GetErrEpoch())
		}
	case faultKindCnConf:
		msg := &pb.CnConf{}
		if decode(msg) {
			epoch(ev.Key, msg.GetErrEpoch())
		}
	case faultKindCntlr:
		msg := &pb.Cntlr{}
		if decode(msg) {
			epoch(ev.Key, msg.GetErrEpoch())
		}
	case faultKindSlice:
		msg := &pb.Slice{}
		if decode(msg) {
			for _, grp := range slices.Concat(msg.GetMetaGrpList(), msg.GetDataGrpList()) {
				legs(pool(), grp.GetLegList())
			}
			fc.slices[ev.Key] = msg
		}
	case faultKindRepair:
		msg := &pb.Repair{}
		if decode(msg) {
			legs(pool(), msg.GetNewLegList())
			fc.repairs[ev.Key] = msg
		}
	case faultKindRetired:
		msg := &pb.RetiredLeg{}
		if decode(msg) {
			if side := msg.GetSide(); side != nil {
				epoch(fmt.Sprintf("pool %s side %d", pool(), side.GetSideId()),
					side.GetErrEpoch())
			}
		}
	}
	return found, placed
}

// placement checks the last two rules on the slices and repairs as they
// stand. The case plants no clone, so a repair of a clone meta group is
// checked for the one-repair rule alone.
func (fc *faultCheck) placement() []string {
	var found []string
	// grpOf finds a group of a pool's slices by its slice key's pool field
	// and its grp_id.
	grpOf := func(sliceKey string, grpId uint64) *pb.Group {
		slice := fc.slices[sliceKey]
		for _, grp := range slices.Concat(slice.GetMetaGrpList(), slice.GetDataGrpList()) {
			if grp.GetGrpId() == grpId {
				return grp
			}
		}
		return nil
	}
	for key, slice := range fc.slices {
		for _, grp := range slices.Concat(slice.GetMetaGrpList(), slice.GetDataGrpList()) {
			owner := make(map[string]uint64)
			for _, leg := range grp.GetLegList() {
				for _, side := range leg.GetSideList() {
					addr := side.GetAddrPort()
					if other, ok := owner[addr]; ok && other != leg.GetLegId() {
						found = append(found, fmt.Sprintf(
							"%s group %d: legs %d and %d both on %s",
							key, grp.GetGrpId(), other, leg.GetLegId(), addr))
					}
					owner[addr] = leg.GetLegId()
				}
			}
		}
	}
	byGrp := make(map[string][]string)
	for key, rep := range fc.repairs {
		fields := strings.Split(key, " ")
		if len(fields) < 5 {
			continue
		}
		grp := fmt.Sprintf("pool %s clone %q group %d", fields[3],
			rep.GetCloneName(), rep.GetGrpId())
		byGrp[grp] = append(byGrp[grp], fields[4])
		if rep.GetCloneName() != "" {
			continue
		}
		sliceKey := strings.Join([]string{fields[0], faultKindSlice, fields[2],
			fields[3], fmt.Sprintf(common.IdKeyFmt, rep.GetSliceId())}, " ")
		g := grpOf(sliceKey, rep.GetGrpId())
		// A fresh new leg sits on no disk node of its group, its bad leg's
		// included; a leg taken back from a retired leg may sit on its bad
		// leg's. A new leg is never matched with itself.
		var newIds []uint64
		for _, leg := range rep.GetNewLegList() {
			newIds = append(newIds, leg.GetLegId())
		}
		all := make(map[string]uint64)
		others := make(map[string]uint64)
		for _, leg := range g.GetLegList() {
			if slices.Contains(newIds, leg.GetLegId()) {
				continue
			}
			for _, side := range leg.GetSideList() {
				all[side.GetAddrPort()] = leg.GetLegId()
				if !slices.Contains(rep.GetBadLegIdList(), leg.GetLegId()) {
					others[side.GetAddrPort()] = leg.GetLegId()
				}
			}
		}
		for _, leg := range rep.GetNewLegList() {
			taken := all
			if _, back := rep.GetNewLegIdToRetiredEpoch()[leg.GetLegId()]; back {
				taken = others
			}
			for _, side := range leg.GetSideList() {
				if legId, ok := taken[side.GetAddrPort()]; ok {
					found = append(found, fmt.Sprintf(
						"%s: new leg %d on %s, the disk node of leg %d of its group",
						key, leg.GetLegId(), side.GetAddrPort(), legId))
				}
			}
		}
	}
	for grp, names := range byGrp {
		if len(names) > 1 {
			slices.Sort(names)
			found = append(found, fmt.Sprintf("%s has %d repairs: %v",
				grp, len(names), names))
		}
	}
	slices.Sort(found)
	return found
}

// faultMarkerKey is the key sync writes to close the store revision it waits
// for: a kind no worker reads and the check skips.
var faultMarkerKey = common.DnvPrefix + " faultcheck_marker"

// sync waits until every write before it is checked, the plant's and the
// first convergence's included, and returns every rule found broken. It
// writes faultMarkerKey: the watch hands that write over after every write
// before it, which closes their revisions.
func (fc *faultCheck) sync(t *testing.T, h *workerHarness) []string {
	t.Helper()
	ctx := context.Background()
	if err := h.cli.Put(ctx, faultMarkerKey, &pb.SpRev{}); err != nil {
		t.Fatalf("the check's marker: %v", err)
	}
	_, rev, err := h.cli.Range(ctx, faultMarkerKey)
	if err != nil {
		t.Fatalf("the store's revision: %v", err)
	}
	h.waitUntil(t, 20*time.Second, "the check to read every write",
		func() (bool, string) {
			fc.mu.Lock()
			defer fc.mu.Unlock()
			if fc.ended {
				t.Fatalf("the check's watch ended: %v", fc.err)
			}
			return fc.heard >= rev, fmt.Sprintf("heard to %d of %d", fc.heard, rev)
		})
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.found)
}

// read is how many writes the check has read, up to which store revision.
func (fc *faultCheck) read() (int, int64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.writes, fc.done
}

// TestFaultCheckCatchesEachRule feeds the fault schedule's check planted
// writes, one store revision a batch, with no etcd, and requires one report
// for each break of each of its four rules, and none for the states the
// rules allow: an epoch set again after a clear, a leg taken back from a
// retired leg on its bad leg's disk node, and a finish that moves a repair's
// new leg into its group in the transaction that deletes the repair.
func TestFaultCheckCatchesEachRule(t *testing.T) {
	const cid, sp = uint64(7), uint64(1)
	put := func(rev int64, key string, msg proto.Message) etcdutil.Event {
		return etcdutil.Event{Type: etcdutil.EventPut, Key: key, Msg: msg, Rev: rev}
	}
	del := func(rev int64, key string) etcdutil.Event {
		return etcdutil.Event{Type: etcdutil.EventDelete, Key: key, Rev: rev}
	}
	side := func(id uint64, addr string) *pb.Side {
		return &pb.Side{SideId: id, AddrPort: addr}
	}
	leg := func(id uint64, sides ...*pb.Side) *pb.Leg {
		return &pb.Leg{LegId: id, SideList: sides}
	}
	slice := func(legs ...*pb.Leg) *pb.Slice {
		return &pb.Slice{DataGrpList: []*pb.Group{{GrpId: 5, LegList: legs}}}
	}
	sliceKey := model.SliceKey(cid, sp, 1)
	repair := func(newLeg *pb.Leg, retired bool) *pb.Repair {
		rep := &pb.Repair{GrpId: 5, SliceId: 1, BadLegIdList: []uint64{6},
			NewLegList: []*pb.Leg{newLeg}}
		if retired {
			rep.NewLegIdToRetiredEpoch = map[uint64]uint64{newLeg.GetLegId(): 9}
		}
		return rep
	}
	twoLegs := slice(leg(6, side(10, "dn1:1")), leg(7, side(11, "dn2:1")))
	cntlrKey := model.CntlrKey(cid, sp, 1)
	for _, tc := range []struct {
		name    string
		batches [][]etcdutil.Event
		want    string
	}{
		{"a revision that goes back", [][]etcdutil.Event{
			{put(10, model.SpRevKey(0x20, cid, sp), &pb.SpRev{Revision: 5})},
			{put(11, model.SpRevKey(0x20, cid, sp), &pb.SpRev{Revision: 4})},
		}, "revision went back from 5 to 4"},
		{"an epoch moved without a clear", [][]etcdutil.Event{
			{put(10, cntlrKey, &pb.Cntlr{ErrEpoch: 100})},
			{put(11, cntlrKey, &pb.Cntlr{ErrEpoch: 200})},
		}, "err_epoch moved from 100 to 200 without a clear"},
		{"an epoch cleared and set again", [][]etcdutil.Event{
			{put(10, cntlrKey, &pb.Cntlr{ErrEpoch: 100})},
			{put(11, cntlrKey, &pb.Cntlr{})},
			{put(12, cntlrKey, &pb.Cntlr{ErrEpoch: 200})},
		}, ""},
		{"two repairs of one group", [][]etcdutil.Event{
			{put(10, sliceKey, twoLegs)},
			{put(11, model.RepairKey(cid, sp, "a"), repair(leg(20, side(21, "dn3:1")), false))},
			{put(12, model.RepairKey(cid, sp, "b"), repair(leg(22, side(23, "dn4:1")), false))},
		}, "has 2 repairs"},
		{"two legs of a group on one disk node", [][]etcdutil.Event{
			{put(10, sliceKey, slice(leg(6, side(10, "dn1:1")), leg(7, side(11, "dn1:1"))))},
		}, "legs 6 and 7 both on dn1:1"},
		{"a new leg on another leg's disk node", [][]etcdutil.Event{
			{put(10, sliceKey, twoLegs)},
			{put(11, model.RepairKey(cid, sp, "a"), repair(leg(20, side(21, "dn2:1")), false))},
		}, "new leg 20 on dn2:1, the disk node of leg 7"},
		{"a fresh new leg on its bad leg's disk node", [][]etcdutil.Event{
			{put(10, sliceKey, twoLegs)},
			{put(11, model.RepairKey(cid, sp, "a"), repair(leg(20, side(21, "dn1:1")), false))},
		}, "new leg 20 on dn1:1, the disk node of leg 6"},
		{"a leg taken back on its bad leg's disk node", [][]etcdutil.Event{
			{put(10, sliceKey, twoLegs)},
			{put(11, model.RepairKey(cid, sp, "a"), repair(leg(20, side(21, "dn1:1")), true))},
		}, ""},
		{"a finish in one transaction, the slice's write first", [][]etcdutil.Event{
			{put(10, sliceKey, twoLegs)},
			{put(11, model.RepairKey(cid, sp, "a"), repair(leg(20, side(21, "dn3:1")), false))},
			{put(12, sliceKey, slice(leg(20, side(21, "dn3:1")), leg(7, side(11, "dn2:1")))),
				del(12, model.RepairKey(cid, sp, "a"))},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFaultCheck()
			for _, batch := range tc.batches {
				fc.apply(batch)
			}
			switch {
			case tc.want == "" && len(fc.found) != 0:
				t.Errorf("found %q, want nothing", fc.found)
			case tc.want != "" && (len(fc.found) != 1 ||
				!strings.Contains(fc.found[0], tc.want)):
				t.Errorf("found %q, want one report with %q", fc.found, tc.want)
			}
		})
	}
}
