package cdc

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The §3 discovery service model tests: DS3 skip-and-serve, DS4 visibility,
// DS5 ordering, the DS6 impact table, DS7 host state and GENCTR, the WV4
// rescan path through registry.replace, and the DS9 snapshot promise.
//
// Everything here drives the registry directly with stub connections: no
// socket, no clock, no watcher. What an impact "does" is observed exactly the
// way NP11 observes it — the connection's wake channel — so a change that
// stops poking a connection cannot pass.

// The hostnqns of the fixtures, in the shape a Linux host presents.
const (
	vwHostPrefix = "nqn.2014-08.org.nvmexpress:uuid:"

	vwHost1 = vwHostPrefix + "11111111-1111-1111-1111-111111111111"
	vwHost2 = vwHostPrefix + "22222222-2222-2222-2222-222222222222"
	vwHost3 = vwHostPrefix + "33333333-3333-3333-3333-333333333333"
)

// The subsystem NQNs of the fixtures.
const (
	vwNqnA = "nqn.2024-01.io.dnv:ss-a"
	vwNqnB = "nqn.2024-01.io.dnv:ss-b"
	vwNqnE = "nqn.2024-01.io.dnv:ss-e"
)

// The CN ports of the fixtures: port1 and port2 of §9.3, on one CN.
const (
	vwAddr1  = "192.168.0.21"
	vwAddr2  = "192.168.0.22"
	vwSvcId1 = "4420"
	vwSvcId2 = "4421"
)

// vwKey builds one entry key in the fixture cluster.
func vwKey(shard uint32, spId uint64, ssId uint64) entryKey {
	return entryKey{cid: testCid, shard: shard, spId: spId, ssId: ssId}
}

// The keys of the impact fixtures: ssA and ssB under one SP, ssE under
// another in a different shard.
var (
	vwKeyA = vwKey(0x10, 1, 1)
	vwKeyB = vwKey(0x10, 1, 2)
	vwKeyE = vwKey(0x3c, 2, 5)
)

// vwRecord is the rendered log entry of one transport configuration, the unit
// every "want" in this file is built out of.
func vwRecord(t *testing.T, nqn string, conf *pb.NvmeTrConf) []byte {
	t.Helper()
	rec, skip := renderEntry(nqn, conf)
	if skip != "" {
		t.Fatalf("renderEntry(%q, %v) skipped it: %s", nqn, conf, skip)
	}
	return rec
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// vwFixture is a registry plus the stub connections attached to it.
type vwFixture struct {
	t     *testing.T
	ctx   context.Context
	reg   *registry
	conns map[string][]*conn
	owner map[*conn]string
}

func vwNew(t *testing.T) *vwFixture {
	t.Helper()
	return &vwFixture{
		t:     t,
		ctx:   context.Background(),
		reg:   newRegistry(),
		conns: make(map[string][]*conn),
		owner: make(map[*conn]string),
	}
}

// attach registers n stub connections for one hostnqn (DS7).
func (f *vwFixture) attach(hostNqn string, n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		c := stubConn()
		f.reg.attach(hostNqn, c)
		f.conns[hostNqn] = append(f.conns[hostNqn], c)
		f.owner[c] = hostNqn
	}
}

// detachLast drops one connection of a host, newest first (NP13).
func (f *vwFixture) detachLast(hostNqn string) {
	f.t.Helper()
	conns := f.conns[hostNqn]
	if len(conns) == 0 {
		f.t.Fatalf("host %q has no connection to detach", hostNqn)
	}
	c := conns[len(conns)-1]
	f.conns[hostNqn] = conns[:len(conns)-1]
	delete(f.owner, c)
	f.reg.detach(hostNqn, c)
}

// put applies one put event the way the watcher does — apply then deliver —
// and returns the deliveries it earned (WV3 -> DS6 -> NP11).
func (f *vwFixture) put(k entryKey, msg *pb.CdcEntry) []delivery {
	f.t.Helper()
	ds := f.reg.apply(f.ctx, k, newEntry(msg))
	deliver(ds)
	return ds
}

// del applies one delete event.
func (f *vwFixture) del(k entryKey) []delivery {
	f.t.Helper()
	ds := f.reg.apply(f.ctx, k, nil)
	deliver(ds)
	return ds
}

// pokes counts — and drains — the connections of one host that were woken, so
// every event is measured on its own.
func (f *vwFixture) pokes(hostNqn string) int {
	f.t.Helper()
	n := 0
	for _, c := range f.conns[hostNqn] {
		if poked(c) {
			n++
		}
	}
	return n
}

// view is the (genctr, numrec, body) a Get Log Page would serve this host.
func (f *vwFixture) view(hostNqn string) (uint64, uint64, []byte) {
	f.t.Helper()
	return f.reg.snapshot(hostNqn)
}

// ---------------------------------------------------------------------------
// DS3 — skip and serve
// ---------------------------------------------------------------------------

// TestNewEntrySkipAndServe proves the DS3 skip rule: an element with a foreign
// tr_type or a foreign address family is dropped and counted, and the rest of
// the entry still renders, in tr-conf order.
func TestNewEntrySkipAndServe(t *testing.T) {
	good1 := tcpConf(vwAddr1, vwSvcId1)
	foreignType := trConf("rdma", common.DefaultCdcAdrFam, vwAddr1, vwSvcId2)
	good2 := trConf(
		common.DefaultCdcTrType, common.CdcAdrFamIpv6, "fd00::21", vwSvcId1,
	)
	foreignFam := trConf(common.DefaultCdcTrType, "fc", vwAddr2, vwSvcId2)
	e := newEntry(cdcEntry(
		vwNqnA, nil, good1, foreignType, good2, foreignFam,
	))
	// Both reasons are recorded, once each and in first-seen order: the
	// foreign transport came before the foreign address family.
	if want := []string{skipForeignTrType, skipForeignAdrFam}; !slices.Equal(
		e.skips, want,
	) {
		t.Errorf("skips = %v, want %v", e.skips, want)
	}
	if e.numRec != 2 {
		t.Errorf("numRec = %d, want 2", e.numRec)
	}
	want := append(
		vwRecord(t, vwNqnA, good1), vwRecord(t, vwNqnA, good2)...,
	)
	if !bytes.Equal(e.records, want) {
		t.Error("the surviving elements did not render in tr-conf order")
	}
	if e.nqn != vwNqnA {
		t.Errorf("nqn = %q, want %q", e.nqn, vwNqnA)
	}
}

// TestNewEntryAllForeign proves that an entry whose every element is foreign
// still exists and still serves — it simply contributes no records (§0 #2).
func TestNewEntryAllForeign(t *testing.T) {
	e := newEntry(cdcEntry(
		vwNqnA, []string{vwHost1},
		trConf("rdma", common.DefaultCdcAdrFam, vwAddr1, vwSvcId1),
		trConf("fc", common.DefaultCdcAdrFam, vwAddr2, vwSvcId2),
	))
	// Two elements, both foreign for the SAME reason: one record, not two.
	if want := []string{skipForeignTrType}; !slices.Equal(e.skips, want) {
		t.Errorf("skips = %v, want %v", e.skips, want)
	}
	if e.numRec != 0 || len(e.records) != 0 {
		t.Errorf("numRec = %d, %d record bytes, want 0 and 0",
			e.numRec, len(e.records))
	}
	if !e.visibleTo(vwHost1) {
		t.Error("a record-less entry lost its visibility")
	}
}

// TestSkipAndServeInAView proves the same rule end to end: the foreign element
// is missing from the served view, the good ones are there (DS3, DS5).
func TestSkipAndServeInAView(t *testing.T) {
	f := vwNew(t)
	f.attach(vwHost1, 1)
	good1 := tcpConf(vwAddr1, vwSvcId1)
	good2 := tcpConf(vwAddr2, vwSvcId2)
	f.put(vwKeyA, cdcEntry(vwNqnA, nil,
		good1,
		trConf("rdma", common.DefaultCdcAdrFam, vwAddr1, vwSvcId2),
		good2,
	))
	_, numRec, body := f.view(vwHost1)
	if numRec != 2 {
		t.Errorf("numrec = %d, want 2", numRec)
	}
	want := append(
		vwRecord(t, vwNqnA, good1), vwRecord(t, vwNqnA, good2)...,
	)
	if !bytes.Equal(body, want) {
		t.Error("the served view is not the two renderable elements")
	}
}

// ---------------------------------------------------------------------------
// DS4 — visibility
// ---------------------------------------------------------------------------

// TestEntryVisibleTo proves the DS4 table: an empty allowed_hosts is visible
// to everyone, a non-empty one exactly to the hostnqns it names, matched as
// exact strings.
func TestEntryVisibleTo(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		host    string
		want    bool
	}{
		{"nil list sees everyone", nil, vwHost1, true},
		{"empty list sees everyone", []string{}, vwHost1, true},
		{"member of a one host list", []string{vwHost1}, vwHost1, true},
		{"non member of a one host list", []string{vwHost2}, vwHost1, false},
		{
			"member of a two host list",
			[]string{vwHost2, vwHost1}, vwHost1, true,
		},
		{
			"non member of a two host list",
			[]string{vwHost2, vwHost3}, vwHost1, false,
		},
		{"the empty hostnqn is not a member", []string{vwHost1}, "", false},
		{
			"a trailing space is a different host",
			[]string{vwHost1 + " "}, vwHost1, false,
		},
		{
			"a prefix is not a match",
			[]string{vwHost1 + "x"}, vwHost1, false,
		},
		{
			"case matters",
			[]string{strings.ToUpper(vwHost1)}, vwHost1, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEntry(cdcEntry(
				vwNqnA, tc.allowed, tcpConf(vwAddr1, vwSvcId1),
			))
			if got := e.visibleTo(tc.host); got != tc.want {
				t.Errorf("visibleTo(%q) = %v, want %v",
					tc.host, got, tc.want)
			}
		})
	}
	// A missing entry — the "before" side of a first put — is visible to
	// nobody, which is what makes the DS6 candidate set work.
	var missing *entry
	if missing.visibleTo(vwHost1) {
		t.Error("a nil entry claimed to be visible")
	}
}

// TestVisibilityInAView proves DS4 through the served views: three hosts, one
// open entry and one restricted entry (§9.12's ssA/ssB shape).
func TestVisibilityInAView(t *testing.T) {
	f := vwNew(t)
	f.attach(vwHost1, 1)
	f.attach(vwHost2, 1)
	f.attach(vwHost3, 1)
	confA := tcpConf(vwAddr1, vwSvcId1)
	confB := tcpConf(vwAddr1, vwSvcId2)
	f.put(vwKeyA, cdcEntry(vwNqnA, nil, confA))
	f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost1, vwHost2}, confB))
	recA := vwRecord(t, vwNqnA, confA)
	recB := vwRecord(t, vwNqnB, confB)
	cases := []struct {
		host string
		want []byte
	}{
		{vwHost1, append(append([]byte(nil), recA...), recB...)},
		{vwHost2, append(append([]byte(nil), recA...), recB...)},
		{vwHost3, recA},
	}
	for _, tc := range cases {
		_, numRec, body := f.view(tc.host)
		if numRec != uint64(len(tc.want)/common.CdcDiscLogEntrySize) {
			t.Errorf("%s: numrec = %d, want %d", tc.host, numRec,
				len(tc.want)/common.CdcDiscLogEntrySize)
		}
		if !bytes.Equal(body, tc.want) {
			t.Errorf("%s: served the wrong records", tc.host)
		}
	}
}

// ---------------------------------------------------------------------------
// DS5 — ordering
// ---------------------------------------------------------------------------

// vwOrdered is the DS5 fixture: the entries in the order the log must serve
// them — (cluster_id, shard_code, sp_id, ss_id, tr-conf index) — so any
// insertion order must reproduce exactly this concatenation.
func vwOrdered() []vwPut {
	return []vwPut{
		{
			key: entryKey{cid: 0x0cdc, shard: 0x00, spId: 1, ssId: 1},
			msg: cdcEntry("nqn.2024-01.io.dnv:o1", nil,
				tcpConf(vwAddr1, vwSvcId1)),
		},
		{
			key: entryKey{cid: 0x0cdc, shard: 0x00, spId: 1, ssId: 2},
			msg: cdcEntry("nqn.2024-01.io.dnv:o2", nil,
				tcpConf(vwAddr1, vwSvcId2)),
		},
		{
			key: entryKey{cid: 0x0cdc, shard: 0x00, spId: 2, ssId: 1},
			msg: cdcEntry("nqn.2024-01.io.dnv:o3", nil,
				tcpConf(vwAddr2, vwSvcId1)),
		},
		{
			key: entryKey{cid: 0x0cdc, shard: 0x3c, spId: 1, ssId: 1},
			msg: cdcEntry("nqn.2024-01.io.dnv:o4", nil,
				// Two transport configurations: their order inside the
				// entry is the tr-conf index, the last DS5 key.
				tcpConf(vwAddr1, vwSvcId1),
				tcpConf(vwAddr2, vwSvcId2)),
		},
		{
			key: entryKey{cid: 0x1cdc, shard: 0x00, spId: 1, ssId: 1},
			msg: cdcEntry("nqn.2024-01.io.dnv:o5", nil,
				tcpConf(vwAddr2, vwSvcId2)),
		},
	}
}

// vwPut is one (key, value) the fixtures apply.
type vwPut struct {
	key entryKey
	msg *pb.CdcEntry
}

// vwWantOrdered renders the DS5 fixture in its required order.
func vwWantOrdered(t *testing.T) []byte {
	t.Helper()
	var want []byte
	for _, p := range vwOrdered() {
		for _, conf := range p.msg.GetNvmeTrConfList() {
			want = append(want, vwRecord(t, p.msg.GetNqn(), conf)...)
		}
	}
	return want
}

// TestRegistryOrdering proves DS5: whatever order entries arrive in, the view
// comes out sorted by (cluster_id, shard_code, sp_id, ss_id, tr-conf index).
func TestRegistryOrdering(t *testing.T) {
	// A scrambled insertion order that touches every key field: cluster
	// last, a middle shard first, sp and ss out of order.
	perms := [][]int{
		{0, 1, 2, 3, 4},
		{4, 3, 2, 1, 0},
		{3, 0, 4, 2, 1},
		{2, 4, 1, 0, 3},
	}
	want := vwWantOrdered(t)
	for _, attachFirst := range []bool{true, false} {
		for i, perm := range perms {
			name := fmt.Sprintf("incremental %d", i)
			if !attachFirst {
				name = fmt.Sprintf("rendered at attach %d", i)
			}
			t.Run(name, func(t *testing.T) {
				f := vwNew(t)
				if attachFirst {
					f.attach(vwHost1, 1)
				}
				puts := vwOrdered()
				for _, idx := range perm {
					f.put(puts[idx].key, puts[idx].msg)
				}
				if !attachFirst {
					f.attach(vwHost1, 1)
				}
				_, numRec, body := f.view(vwHost1)
				if got := f.reg.entryCount(); got != len(puts) {
					t.Errorf("entryCount = %d, want %d", got, len(puts))
				}
				if numRec != 6 {
					t.Errorf("numrec = %d, want 6", numRec)
				}
				if !bytes.Equal(body, want) {
					t.Errorf("permutation %d served the wrong order", i)
				}
			})
		}
	}
}

// TestRegistryOrderingAfterDelete proves that DS5 order survives removal and
// re-insertion — the insertOrder/removeOrder pair, which a naive append would
// get wrong.
func TestRegistryOrderingAfterDelete(t *testing.T) {
	f := vwNew(t)
	f.attach(vwHost1, 1)
	puts := vwOrdered()
	for _, p := range puts {
		f.put(p.key, p.msg)
	}
	// Take the middle entry out and put it back last.
	f.del(puts[2].key)
	if got := f.reg.entryCount(); got != len(puts)-1 {
		t.Fatalf("entryCount after delete = %d, want %d",
			got, len(puts)-1)
	}
	f.put(puts[2].key, puts[2].msg)
	_, numRec, body := f.view(vwHost1)
	if numRec != 6 {
		t.Errorf("numrec = %d, want 6", numRec)
	}
	if !bytes.Equal(body, vwWantOrdered(t)) {
		t.Error("the re-inserted entry did not return to its DS5 place")
	}
}

// ---------------------------------------------------------------------------
// DS6 — impact
// ---------------------------------------------------------------------------

// vwImpact is what one host must look like after an event: its GENCTR, its
// NUMREC and how many of its connections were poked (DS6 -> NP11).
type vwImpact struct {
	genCtr uint64
	numRec uint64
	poked  int
}

// TestRegistryImpact is the DS6 table. Every row starts from an initial state,
// attaches its hosts (so every GENCTR starts at 1, DS7), applies exactly one
// event, and is asserted on GENCTR, NUMREC and which connections were poked.
//
// Every host is attached with two connections, because DS6 says the pending
// bit is set on EACH of a host's connections, not on one of them.
func TestRegistryImpact(t *testing.T) {
	confA := tcpConf(vwAddr1, vwSvcId1)
	confA2 := tcpConf(vwAddr1, vwSvcId2)
	confB := tcpConf(vwAddr2, vwSvcId1)
	openA := cdcEntry(vwNqnA, nil, confA)

	cases := []struct {
		name    string
		initial []vwPut
		hosts   []string
		key     entryKey
		after   *pb.CdcEntry // nil is a delete
		want    map[string]vwImpact
	}{
		{
			name:    "gain",
			initial: []vwPut{{vwKeyA, openA}},
			hosts:   []string{vwHost1, vwHost2},
			key:     vwKeyB,
			after:   cdcEntry(vwNqnB, []string{vwHost1}, confB),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 2, numRec: 2, poked: 2},
				vwHost2: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
		{
			name: "loss by allowed_hosts removal",
			initial: []vwPut{
				{vwKeyA, openA},
				{vwKeyB, cdcEntry(
					vwNqnB, []string{vwHost1, vwHost2}, confB)},
			},
			hosts: []string{vwHost1, vwHost2},
			key:   vwKeyB,
			after: cdcEntry(vwNqnB, []string{vwHost2}, confB),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 2, numRec: 1, poked: 2},
				// h2 keeps its membership and its bytes: not impacted.
				vwHost2: {genCtr: 1, numRec: 2, poked: 0},
			},
		},
		{
			name: "loss by delete",
			initial: []vwPut{
				{vwKeyA, openA},
				{vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB)},
			},
			hosts: []string{vwHost1, vwHost2},
			key:   vwKeyB,
			after: nil,
			want: map[string]vwImpact{
				vwHost1: {genCtr: 2, numRec: 1, poked: 2},
				vwHost2: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
		{
			name:    "tr conf change inside a visible entry",
			initial: []vwPut{{vwKeyA, openA}},
			hosts:   []string{vwHost1, vwHost2},
			key:     vwKeyA,
			after:   cdcEntry(vwNqnA, nil, confA2),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 2, numRec: 1, poked: 2},
				vwHost2: {genCtr: 2, numRec: 1, poked: 2},
			},
		},
		{
			name:    "tr conf added inside a visible entry",
			initial: []vwPut{{vwKeyA, openA}},
			hosts:   []string{vwHost1, vwHost2},
			key:     vwKeyA,
			after:   cdcEntry(vwNqnA, nil, confA, confA2),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 2, numRec: 2, poked: 2},
				vwHost2: {genCtr: 2, numRec: 2, poked: 2},
			},
		},
		{
			name: "invisible before and after",
			initial: []vwPut{
				{vwKeyA, openA},
				{vwKeyE, cdcEntry(vwNqnE, []string{vwHost3}, confB)},
			},
			// h3 is NOT attached: the entry is invisible to every active
			// host on both sides of the change.
			hosts: []string{vwHost1, vwHost2},
			key:   vwKeyE,
			after: cdcEntry(vwNqnE, []string{vwHost3}, confA2),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 1, poked: 0},
				vwHost2: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
		{
			name: "invisible entry deleted",
			initial: []vwPut{
				{vwKeyA, openA},
				{vwKeyE, cdcEntry(vwNqnE, []string{vwHost3}, confB)},
			},
			hosts: []string{vwHost1, vwHost2},
			key:   vwKeyE,
			after: nil,
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 1, poked: 0},
				vwHost2: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
		{
			// The §9.12 step 4 rule: allowed_hosts is not log page content,
			// so adding a SECOND host to an entry the first host already
			// sees must not move the first host's GENCTR at all.
			name: "membership preserving allowed_hosts edit",
			initial: []vwPut{
				{vwKeyA, openA},
				{vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB)},
			},
			hosts: []string{vwHost1, vwHost2},
			key:   vwKeyB,
			after: cdcEntry(vwNqnB, []string{vwHost1, vwHost2}, confB),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 2, poked: 0},
				vwHost2: {genCtr: 2, numRec: 2, poked: 2},
			},
		},
		{
			name:    "empty allowed_hosts fans out to every active host",
			initial: nil,
			hosts:   []string{vwHost1, vwHost2, vwHost3},
			key:     vwKeyA,
			after:   openA,
			want: map[string]vwImpact{
				vwHost1: {genCtr: 2, numRec: 1, poked: 2},
				vwHost2: {genCtr: 2, numRec: 1, poked: 2},
				vwHost3: {genCtr: 2, numRec: 1, poked: 2},
			},
		},
		{
			name:    "restricted entry opened to everyone",
			initial: []vwPut{{vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB)}},
			hosts:   []string{vwHost1, vwHost2, vwHost3},
			key:     vwKeyB,
			after:   cdcEntry(vwNqnB, nil, confB),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 1, poked: 0},
				vwHost2: {genCtr: 2, numRec: 1, poked: 2},
				vwHost3: {genCtr: 2, numRec: 1, poked: 2},
			},
		},
		{
			name:    "identical re-put",
			initial: []vwPut{{vwKeyA, openA}},
			hosts:   []string{vwHost1, vwHost2},
			key:     vwKeyA,
			after:   cdcEntry(vwNqnA, nil, tcpConf(vwAddr1, vwSvcId1)),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 1, poked: 0},
				vwHost2: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
		{
			name: "allowed_hosts reordered only",
			initial: []vwPut{{vwKeyB, cdcEntry(
				vwNqnB, []string{vwHost1, vwHost2}, confB)}},
			hosts: []string{vwHost1, vwHost2},
			key:   vwKeyB,
			after: cdcEntry(vwNqnB, []string{vwHost2, vwHost1}, confB),
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 1, poked: 0},
				vwHost2: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
		{
			name:    "delete of a key that was never there",
			initial: []vwPut{{vwKeyA, openA}},
			hosts:   []string{vwHost1},
			key:     vwKeyE,
			after:   nil,
			want: map[string]vwImpact{
				vwHost1: {genCtr: 1, numRec: 1, poked: 0},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := vwNew(t)
			for _, p := range tc.initial {
				f.put(p.key, p.msg)
			}
			// Hosts attach AFTER the initial state, so every GENCTR
			// starts at 1 with the view already rendered (DS7).
			for _, host := range tc.hosts {
				f.attach(host, 2)
				if genCtr, _, _ := f.view(host); genCtr != 1 {
					t.Fatalf("%s: GENCTR starts at %d, want 1",
						host, genCtr)
				}
			}
			var ds []delivery
			if tc.after == nil {
				ds = f.del(tc.key)
			} else {
				ds = f.put(tc.key, tc.after)
			}
			// Every delivery names a connection of an impacted host and
			// carries that host's new GENCTR.
			wantDeliveries := 0
			for host, want := range tc.want {
				wantDeliveries += want.poked
				genCtr, numRec, _ := f.view(host)
				if genCtr != want.genCtr {
					t.Errorf("%s: genctr = %d, want %d",
						host, genCtr, want.genCtr)
				}
				if numRec != want.numRec {
					t.Errorf("%s: numrec = %d, want %d",
						host, numRec, want.numRec)
				}
				if got := f.pokes(host); got != want.poked {
					t.Errorf("%s: %d connections poked, want %d",
						host, got, want.poked)
				}
			}
			if len(ds) != wantDeliveries {
				t.Errorf("%d deliveries, want %d", len(ds), wantDeliveries)
			}
			for _, d := range ds {
				host := f.owner[d.c]
				if host == "" {
					t.Fatal("a delivery named an unknown connection")
				}
				if d.genCtr != tc.want[host].genCtr {
					t.Errorf("%s: delivery genctr = %d, want %d",
						host, d.genCtr, tc.want[host].genCtr)
				}
			}
		})
	}
}

// TestRegistryGenCtrBumpsOnlyOnImpact proves the DS7 counter over a sequence:
// it starts at 1, moves by exactly one per impacting event, and does not move
// at all for events that leave the host's bytes alone.
func TestRegistryGenCtrBumpsOnlyOnImpact(t *testing.T) {
	f := vwNew(t)
	f.attach(vwHost1, 1)
	f.attach(vwHost2, 1)
	confA := tcpConf(vwAddr1, vwSvcId1)
	confB := tcpConf(vwAddr2, vwSvcId2)
	steps := []struct {
		name    string
		event   func()
		wantOne uint64
		wantTwo uint64
	}{
		{
			name:    "start",
			event:   func() {},
			wantOne: 1, wantTwo: 1,
		},
		{
			name:    "an open entry impacts both",
			event:   func() { f.put(vwKeyA, cdcEntry(vwNqnA, nil, confA)) },
			wantOne: 2, wantTwo: 2,
		},
		{
			name: "a host 1 entry impacts host 1 only",
			event: func() {
				f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB))
			},
			wantOne: 3, wantTwo: 2,
		},
		{
			name: "re-putting it unchanged impacts nobody",
			event: func() {
				f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB))
			},
			wantOne: 3, wantTwo: 2,
		},
		{
			name: "widening its allowed_hosts impacts host 2 only",
			event: func() {
				f.put(vwKeyB, cdcEntry(
					vwNqnB, []string{vwHost1, vwHost2}, confB))
			},
			wantOne: 3, wantTwo: 3,
		},
		{
			name:    "deleting the open entry impacts both",
			event:   func() { f.del(vwKeyA) },
			wantOne: 4, wantTwo: 4,
		},
		{
			name:    "deleting it again impacts nobody",
			event:   func() { f.del(vwKeyA) },
			wantOne: 4, wantTwo: 4,
		},
	}
	for _, step := range steps {
		step.event()
		one, _, _ := f.view(vwHost1)
		two, _, _ := f.view(vwHost2)
		if one != step.wantOne || two != step.wantTwo {
			t.Fatalf("after %q: genctr h1 = %d (want %d), h2 = %d (want %d)",
				step.name, one, step.wantOne, two, step.wantTwo)
		}
	}
}

// ---------------------------------------------------------------------------
// DS7 — host state lifecycle
// ---------------------------------------------------------------------------

// TestHostStateLifecycle proves DS7 and §0 #6: the state is created at the
// first connection with GENCTR 1, shared by every later connection, and
// dropped at the last disconnect — so a new connection of the same hostnqn
// starts counting again at 1.
func TestHostStateLifecycle(t *testing.T) {
	f := vwNew(t)
	confA := tcpConf(vwAddr1, vwSvcId1)
	confB := tcpConf(vwAddr2, vwSvcId2)
	f.put(vwKeyA, cdcEntry(vwNqnA, nil, confA))

	if got := f.reg.hostCount(); got != 0 {
		t.Fatalf("hostCount before any connection = %d, want 0", got)
	}
	if genCtr, numRec, body := f.view(vwHost1); genCtr != 0 ||
		numRec != 0 || body != nil {
		t.Errorf("an untracked host got (%d, %d, %d bytes), want (0, 0, 0)",
			genCtr, numRec, len(body))
	}

	// First connection: GENCTR 1 and the view already rendered.
	f.attach(vwHost1, 1)
	genCtr, numRec, _ := f.view(vwHost1)
	if genCtr != 1 || numRec != 1 {
		t.Fatalf("first connection: (%d, %d), want (1, 1)", genCtr, numRec)
	}

	// An impact moves it, and pokes the one connection there is.
	f.put(vwKeyB, cdcEntry(vwNqnB, nil, confB))
	if genCtr, _, _ = f.view(vwHost1); genCtr != 2 {
		t.Fatalf("after an impact: genctr = %d, want 2", genCtr)
	}
	if got := f.pokes(vwHost1); got != 1 {
		t.Errorf("%d connections poked, want 1", got)
	}

	// A second connection of the same hostnqn shares the state: no new host,
	// the same GENCTR, and no poke for an event it did not miss.
	f.attach(vwHost1, 1)
	if got := f.reg.hostCount(); got != 1 {
		t.Fatalf("hostCount with two connections = %d, want 1", got)
	}
	if genCtr, _, _ = f.view(vwHost1); genCtr != 2 {
		t.Fatalf("second connection: genctr = %d, want 2", genCtr)
	}
	if got := f.pokes(vwHost1); got != 0 {
		t.Errorf("attaching poked %d connections, want 0", got)
	}

	// Both connections are poked by the next impact.
	f.del(vwKeyB)
	if genCtr, _, _ = f.view(vwHost1); genCtr != 3 {
		t.Fatalf("after the delete: genctr = %d, want 3", genCtr)
	}
	if got := f.pokes(vwHost1); got != 2 {
		t.Errorf("%d connections poked, want 2", got)
	}

	// Dropping one connection keeps the state and the counter.
	f.detachLast(vwHost1)
	if got := f.reg.hostCount(); got != 1 {
		t.Fatalf("hostCount after one detach = %d, want 1", got)
	}
	if genCtr, _, _ = f.view(vwHost1); genCtr != 3 {
		t.Fatalf("after one detach: genctr = %d, want 3", genCtr)
	}

	// Dropping the last one drops the state (NP13).
	f.detachLast(vwHost1)
	if got := f.reg.hostCount(); got != 0 {
		t.Fatalf("hostCount after the last detach = %d, want 0", got)
	}

	// While the host is gone the served state moves on, unnoticed.
	f.put(vwKeyB, cdcEntry(vwNqnB, nil, confB))

	// A new connection of the same hostnqn restarts at 1 (§0 #6) and sees
	// the current state, not the one it left.
	f.attach(vwHost1, 1)
	genCtr, numRec, body := f.view(vwHost1)
	if genCtr != 1 {
		t.Errorf("after reattaching: genctr = %d, want 1", genCtr)
	}
	if numRec != 2 {
		t.Errorf("after reattaching: numrec = %d, want 2", numRec)
	}
	want := append(
		vwRecord(t, vwNqnA, confA), vwRecord(t, vwNqnB, confB)...,
	)
	if !bytes.Equal(body, want) {
		t.Error("a reattached host did not get the current view")
	}
}

// TestDetachOfAnUnknownHost proves detach is safe on a hostnqn the registry is
// not tracking — the teardown path of a connection that never completed
// Connect (NP13).
func TestDetachOfAnUnknownHost(t *testing.T) {
	f := vwNew(t)
	f.reg.detach(vwHost1, stubConn())
	if got := f.reg.hostCount(); got != 0 {
		t.Errorf("hostCount = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// WV4 — the rescan path
// ---------------------------------------------------------------------------

// TestRegistryReplaceMatchesApply proves the WV4 rule: a rescan that diffs the
// new map against the held state impacts exactly the hosts the equivalent
// sequence of watch events would have, with the same resulting views — so an
// AEN missed across a watch gap is not lost, and an unaffected host is not
// woken for nothing.
func TestRegistryReplaceMatchesApply(t *testing.T) {
	confA := tcpConf(vwAddr1, vwSvcId1)
	confA2 := tcpConf(vwAddr1, vwSvcId2)
	confB := tcpConf(vwAddr2, vwSvcId1)
	confE := tcpConf(vwAddr2, vwSvcId2)

	// The state both registries start from.
	initial := []vwPut{
		{vwKeyA, cdcEntry(vwNqnA, nil, confA)},
		{vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB)},
	}
	// The changes: A's transport moves, B is deleted, E appears for h2, and
	// an entry only h3 (never connected) can see churns.
	final := []vwPut{
		{vwKeyA, cdcEntry(vwNqnA, nil, confA2)},
		{vwKeyE, cdcEntry(vwNqnE, []string{vwHost2}, confE)},
		{vwKey(0x3c, 9, 9), cdcEntry("nqn.2024-01.io.dnv:ss-x",
			[]string{vwHost3}, confE)},
	}
	hosts := []string{vwHost1, vwHost2}

	newFixture := func(t *testing.T) *vwFixture {
		t.Helper()
		f := vwNew(t)
		for _, p := range initial {
			f.put(p.key, p.msg)
		}
		for _, host := range hosts {
			f.attach(host, 2)
		}
		return f
	}

	// The event-by-event path: one apply per change.
	byApply := newFixture(t)
	byApply.put(final[0].key, final[0].msg)
	byApply.del(vwKeyB)
	byApply.put(final[1].key, final[1].msg)
	byApply.put(final[2].key, final[2].msg)

	// The rescan path: one replace with the whole new map.
	byReplace := newFixture(t)
	entries := make(map[entryKey]*entry, len(final))
	for _, p := range final {
		entries[p.key] = newEntry(p.msg)
	}
	deliver(byReplace.reg.replace(byReplace.ctx, entries))

	for _, host := range hosts {
		applyGen, applyNum, applyBody := byApply.view(host)
		replGen, replNum, replBody := byReplace.view(host)
		if !bytes.Equal(applyBody, replBody) {
			t.Errorf("%s: replace served a different view than apply", host)
		}
		if applyNum != replNum {
			t.Errorf("%s: numrec %d by apply, %d by replace",
				host, applyNum, replNum)
		}
		// GENCTR values differ by construction — a rescan is one event —
		// but "did it move" must not.
		if (applyGen > 1) != (replGen > 1) {
			t.Errorf("%s: genctr moved %v by apply, %v by replace",
				host, applyGen > 1, replGen > 1)
		}
		applyPokes := byApply.pokes(host) > 0
		replPokes := byReplace.pokes(host) > 0
		if applyPokes != replPokes {
			t.Errorf("%s: poked %v by apply, %v by replace",
				host, applyPokes, replPokes)
		}
		if !replPokes {
			t.Errorf("%s: the rescan lost an impact", host)
		}
	}

	// A rescan that changes nothing impacts nobody: the second replace of
	// the same map must neither bump a GENCTR nor poke a connection.
	before := make(map[string]uint64, len(hosts))
	for _, host := range hosts {
		before[host], _, _ = byReplace.view(host)
	}
	same := make(map[entryKey]*entry, len(final))
	for _, p := range final {
		same[p.key] = newEntry(p.msg)
	}
	deliver(byReplace.reg.replace(byReplace.ctx, same))
	for _, host := range hosts {
		genCtr, _, _ := byReplace.view(host)
		if genCtr != before[host] {
			t.Errorf("%s: an idempotent rescan moved genctr %d -> %d",
				host, before[host], genCtr)
		}
		if got := byReplace.pokes(host); got != 0 {
			t.Errorf("%s: an idempotent rescan poked %d connections",
				host, got)
		}
	}
}

// TestRegistryReplaceLeavesUnimpactedHostsAlone proves the other half of the
// WV4 rule: a rescan re-renders every active host, but only the ones whose
// rendered bytes actually moved are impacted. A host the rescan left alone
// keeps its GENCTR and is never woken, however much else the scan changed.
func TestRegistryReplaceLeavesUnimpactedHostsAlone(t *testing.T) {
	f := vwNew(t)
	confA := tcpConf(vwAddr1, vwSvcId1)
	confA2 := tcpConf(vwAddr1, vwSvcId2)
	confB := tcpConf(vwAddr2, vwSvcId1)
	// One entry per host, each visible only to its own host.
	f.put(vwKeyA, cdcEntry(vwNqnA, []string{vwHost1}, confA))
	f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost2}, confB))
	f.attach(vwHost1, 2)
	f.attach(vwHost2, 2)

	// The rescan moves host 1's entry and re-states host 2's unchanged.
	entries := map[entryKey]*entry{
		vwKeyA: newEntry(cdcEntry(vwNqnA, []string{vwHost1}, confA2)),
		vwKeyB: newEntry(cdcEntry(vwNqnB, []string{vwHost2}, confB)),
	}
	ds := f.reg.replace(f.ctx, entries)
	deliver(ds)

	if len(ds) != 2 {
		t.Errorf("%d deliveries, want 2", len(ds))
	}
	for _, d := range ds {
		if host := f.owner[d.c]; host != vwHost1 {
			t.Errorf("the rescan delivered to %q, want host 1 only", host)
		}
	}
	genOne, numOne, bodyOne := f.view(vwHost1)
	if genOne != 2 || numOne != 1 {
		t.Errorf("host 1 = (%d, %d), want (2, 1)", genOne, numOne)
	}
	if !bytes.Equal(bodyOne, vwRecord(t, vwNqnA, confA2)) {
		t.Error("host 1 did not get the rescanned record")
	}
	if got := f.pokes(vwHost1); got != 2 {
		t.Errorf("host 1: %d connections poked, want 2", got)
	}
	genTwo, numTwo, bodyTwo := f.view(vwHost2)
	if genTwo != 1 || numTwo != 1 {
		t.Errorf("host 2 = (%d, %d), want (1, 1)", genTwo, numTwo)
	}
	if !bytes.Equal(bodyTwo, vwRecord(t, vwNqnB, confB)) {
		t.Error("host 2's untouched view changed")
	}
	if got := f.pokes(vwHost2); got != 0 {
		t.Errorf("host 2: %d connections poked, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// DS9 — snapshot isolation
// ---------------------------------------------------------------------------

// TestSnapshotIsolation proves the DS9 snapshot promise: the
// (genctr, records) a Get Log Page command took at receipt is not mutated by
// a later apply, so a paged read stays self-consistent even while the watcher
// works.
//
// The first change deliberately keeps the record COUNT the same: a view
// re-rendered into the slice it already owns would scribble on exactly this
// snapshot, and only an equal-length change catches that.
func TestSnapshotIsolation(t *testing.T) {
	f := vwNew(t)
	confA := tcpConf(vwAddr1, vwSvcId1)
	confA2 := tcpConf(vwAddr1, vwSvcId2)
	confB := tcpConf(vwAddr2, vwSvcId1)
	confC := tcpConf(vwAddr2, vwSvcId2)
	f.put(vwKeyA, cdcEntry(vwNqnA, nil, confA))
	f.put(vwKeyB, cdcEntry(vwNqnB, nil, confB))
	f.attach(vwHost1, 1)

	genCtr, numRec, body := f.reg.snapshot(vwHost1)
	if genCtr != 1 || numRec != 2 {
		t.Fatalf("snapshot = (%d, %d), want (1, 2)", genCtr, numRec)
	}
	keep := append([]byte(nil), body...)
	firstPage := logPageBytes(genCtr, numRec, body, 0, 3072)

	// An equal-length change, then a growing one, both while the command is
	// still paging through the snapshot it took.
	f.put(vwKeyA, cdcEntry(vwNqnA, nil, confA2))
	f.put(vwKeyE, cdcEntry(vwNqnE, nil, confC))

	if !bytes.Equal(body, keep) {
		t.Error("a later apply mutated a taken snapshot's body")
	}
	if genCtr != 1 || numRec != 2 {
		t.Error("a later apply mutated a taken snapshot's counters")
	}
	want := append(
		vwRecord(t, vwNqnA, confA), vwRecord(t, vwNqnB, confB)...,
	)
	if !bytes.Equal(body, want) {
		t.Error("the snapshot no longer holds the records it was taken of")
	}
	// The rest of the paged read still comes out of the same snapshot.
	for _, off := range []uint64{1024, 2048} {
		page := logPageBytes(genCtr, numRec, body, off, 1024)
		if !bytes.Equal(page, firstPage[off:off+1024]) {
			t.Errorf("the page at %d stopped being self-consistent", off)
		}
	}

	// A new command sees the new state: two impacts later, three records.
	newGen, newNum, newBody := f.reg.snapshot(vwHost1)
	if newGen != 3 || newNum != 3 {
		t.Errorf("the next snapshot = (%d, %d), want (3, 3)",
			newGen, newNum)
	}
	if bytes.Equal(newBody[:len(keep)], keep) {
		t.Error("the next snapshot served the old records")
	}
}

// ---------------------------------------------------------------------------
// §7 — the `view changed` record
// ---------------------------------------------------------------------------

// TestViewChangedRecords proves the LG contract for DS6: exactly one
// `view changed` per impacted host, carrying hostnqn, genctr and numrec, and
// none at all for a host the change did not impact.
func TestViewChangedRecords(t *testing.T) {
	logs := captureLogs(t)
	f := vwNew(t)
	confA := tcpConf(vwAddr1, vwSvcId1)
	confB := tcpConf(vwAddr2, vwSvcId2)
	f.put(vwKeyA, cdcEntry(vwNqnA, nil, confA))
	f.attach(vwHost1, 2)
	f.attach(vwHost2, 1)
	if got := logs.count(msgViewChanged); got != 0 {
		t.Fatalf("attaching logged %d %q records, want 0",
			got, msgViewChanged)
	}

	// An entry only h1 can see: one record, for h1, whatever the number of
	// connections it holds.
	f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB))
	recs := logs.find(msgViewChanged)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1", len(recs), msgViewChanged)
	}
	rec := recs[0]
	if rec["hostnqn"] != vwHost1 {
		t.Errorf("hostnqn = %v, want %q", rec["hostnqn"], vwHost1)
	}
	if rec["genctr"] != uint64(2) {
		t.Errorf("genctr = %v, want 2", rec["genctr"])
	}
	if rec["numrec"] != uint64(2) {
		t.Errorf("numrec = %v, want 2", rec["numrec"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}

	// A put that changes nothing logs nothing.
	f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost1}, confB))
	if got := logs.count(msgViewChanged); got != 1 {
		t.Errorf("an idempotent put logged %d records in total, want 1", got)
	}

	// The membership-preserving edit of §9.12 step 4: h2 gains the entry and
	// is logged, h1 keeps exactly its one record.
	f.put(vwKeyB, cdcEntry(vwNqnB, []string{vwHost1, vwHost2}, confB))
	recs = logs.find(msgViewChanged)
	if len(recs) != 2 {
		t.Fatalf("%d %q records, want 2", len(recs), msgViewChanged)
	}
	perHost := make(map[string]int)
	for _, r := range recs {
		host, _ := r["hostnqn"].(string)
		perHost[host]++
	}
	if perHost[vwHost1] != 1 {
		t.Errorf("%d records for host 1, want 1", perHost[vwHost1])
	}
	if perHost[vwHost2] != 1 {
		t.Errorf("%d records for host 2, want 1", perHost[vwHost2])
	}
	if recs[1]["genctr"] != uint64(2) {
		t.Errorf("host 2 genctr = %v, want 2", recs[1]["genctr"])
	}
	if recs[1]["numrec"] != uint64(2) {
		t.Errorf("host 2 numrec = %v, want 2", recs[1]["numrec"])
	}
}
