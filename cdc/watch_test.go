package cdc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
)

// The §4 watcher tests: the scan that builds the owned map (WV1/WV2), the DS2
// ownership arithmetic, the events that flow through the DS6 impact pass
// (WV3), the rescan a watch failure forces and the diff it produces (WV4),
// the retry cadence of a failing Range (WV5) with the held state still served
// through it (DS10), and the read-only rule (WV6).

// watchHostNqn is the host every impact assertion in this file is made for.
const watchHostNqn = "nqn.2026-01.io.dnv-test:cdc:host1"

// watchRescan is the WV5 retry cadence the harness configures. It is
// deliberately NOT the production default, so a watcher that ignored
// Config.RescanInterval would hang here instead of passing.
const watchRescan = 5 * time.Second

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// watchStore is the fake etcd store with every call to it recorded, which is
// how the WV6 assertion is made: the watcher may reach etcd only through
// Range, Decode and WatchTyped.
type watchStore struct {
	*fakeStore
	mu    sync.Mutex
	calls []string
}

func newWatchStore() *watchStore {
	return &watchStore{fakeStore: newFakeStore()}
}

func (s *watchStore) record(name string) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.mu.Unlock()
}

// callNames is every etcdStore method the watcher has invoked, in order.
func (s *watchStore) callNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// keys is the store's key set, so a test can prove the watcher changed
// nothing behind the interface either (WV6).
func (s *watchStore) keys() []string {
	s.fakeStore.mu.Lock()
	defer s.fakeStore.mu.Unlock()
	out := make([]string, 0, len(s.fakeStore.kvs))
	for key := range s.fakeStore.kvs {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// rev is the revision the next Range will be served at.
func (s *watchStore) currentRev() int64 {
	s.fakeStore.mu.Lock()
	defer s.fakeStore.mu.Unlock()
	return s.fakeStore.rev
}

func (s *watchStore) Range(
	ctx context.Context,
	prefix string,
) ([]etcdutil.KV, int64, error) {
	s.record("Range")
	return s.fakeStore.Range(ctx, prefix)
}

func (s *watchStore) Decode(
	ctx context.Context,
	kv etcdutil.KV,
	msg proto.Message,
) error {
	s.record("Decode")
	return s.fakeStore.Decode(ctx, kv, msg)
}

func (s *watchStore) WatchTyped(
	ctx context.Context,
	prefix string,
	fromRev int64,
	newMsg func() proto.Message,
) (<-chan etcdutil.Event, <-chan error) {
	s.record("WatchTyped")
	return s.fakeStore.WatchTyped(ctx, prefix, fromRev, newMsg)
}

// watchHarness is one watcher under test: the fake store and fake clock it
// runs on, the registry it feeds, the capture the §7 assertions read, and the
// goroutine running the WV1 loop.
type watchHarness struct {
	t      *testing.T
	store  *watchStore
	clk    *fakeClock
	reg    *registry
	w      *watcher
	logs   *logCapture
	cancel context.CancelFunc
	done   chan struct{}
}

// newWatchHarness builds a watcher owning the given --range digits (DS2). It
// does not start it, so a test can seed the store first.
func newWatchHarness(t *testing.T, ranges ...uint32) *watchHarness {
	t.Helper()
	logs := captureLogs(t)
	store := newWatchStore()
	clk := newFakeClock()
	d := &deps{
		cfg: Config{
			Ranges:         ranges,
			TrType:         common.DefaultCdcTrType,
			AdrFam:         common.DefaultCdcAdrFam,
			TrAddr:         "127.0.0.1",
			TrSvcId:        common.DefaultCdcTrSvcId,
			RescanInterval: watchRescan,
		},
		store: store,
		clk:   clk,
	}
	reg := newRegistry()
	return &watchHarness{
		t:     t,
		store: store,
		clk:   clk,
		reg:   reg,
		w:     newWatcher(d, reg, ranges),
		logs:  logs,
	}
}

// start runs the WV1 loop until the test ends. The cleanup registered here
// runs BEFORE captureLogs' one (cleanups are LIFO), so the goroutine is gone
// before the default logger is put back.
func (h *watchHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		h.w.run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		h.waitStopped()
	})
}

// waitStopped asserts the run loop has left.
func (h *watchHarness) waitStopped() {
	h.t.Helper()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("watcher did not stop")
	}
}

// attachHost registers one stub connection under a hostnqn (DS7), which is
// what makes the host active and therefore impactable.
func (h *watchHarness) attachHost(hostNqn string) *conn {
	h.t.Helper()
	c := stubConn()
	h.reg.attach(hostNqn, c)
	return c
}

// assertView asserts one host's DS9 snapshot: its GENCTR, its NUMREC and that
// the body is exactly NUMREC entries long.
func (h *watchHarness) assertView(
	hostNqn string,
	wantGenCtr uint64,
	wantNumRec uint64,
) {
	h.t.Helper()
	genCtr, numRec, body := h.reg.snapshot(hostNqn)
	if genCtr != wantGenCtr || numRec != wantNumRec {
		h.t.Fatalf("snapshot(%s) = (genctr %d, numrec %d), want (%d, %d)",
			hostNqn, genCtr, numRec, wantGenCtr, wantNumRec)
	}
	if want := int(wantNumRec) * common.CdcDiscLogEntrySize; len(body) != want {
		h.t.Fatalf("snapshot(%s) body %d bytes, want %d",
			hostNqn, len(body), want)
	}
}

// ---------------------------------------------------------------------------
// Record attribute readers
// ---------------------------------------------------------------------------

// watchAttrString reads one string attribute off a captured record.
func watchAttrString(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	v, ok := rec[key].(string)
	if !ok {
		t.Fatalf("record %v: %q is %T, want string", rec, key, rec[key])
	}
	return v
}

// watchAttrInt reads one integer attribute; slog hands ints, int64s and
// uint64s all back through Value.Any(), so all three are accepted.
func watchAttrInt(t *testing.T, rec map[string]any, key string) int64 {
	t.Helper()
	switch v := rec[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case uint64:
		return int64(v)
	}
	t.Fatalf("record %v: %q is %T, want an integer", rec, key, rec[key])
	return 0
}

// watchAttrBool reads one bool attribute off a captured record.
func watchAttrBool(t *testing.T, rec map[string]any, key string) bool {
	t.Helper()
	v, ok := rec[key].(bool)
	if !ok {
		t.Fatalf("record %v: %q is %T, want bool", rec, key, rec[key])
	}
	return v
}

// ---------------------------------------------------------------------------
// WV1 / WV2 — the scan
// ---------------------------------------------------------------------------

// TestWatchScanBuildsOwnedMap proves WV1/WV2 and DS3's skip-and-serve rule:
// the scan keeps the owned entries, drops a key whose shard code this
// instance does not own SILENTLY, logs `cdc entry skipped` with reason
// malformed_key for an unparsable key and malformed_value for a value that is
// not a CdcEntry, logs foreign_tr_type for an entry carrying a transport it
// cannot render and foreign_adr_fam for one carrying an address family it
// cannot name — while still serving those entries' other elements — and
// reports the owned count and the revision in `cdc scan complete`.
func TestWatchScanBuildsOwnedMap(t *testing.T) {
	h := newWatchHarness(t, 0x0)

	ownedKey := testKey(0x05, 0x1, 0xa)
	foreignShardKey := testKey(0x10, 0x1, 0xb)
	mixedKey := testKey(0x0f, 0x2, 0xc)
	foreignFamKey := testKey(0x0e, 0x2, 0xe)
	badValueKey := testKey(0x00, 0x3, 0xd)
	badKey := model.CdcEntryPrefix() + "not-a-cdc-entry-key"

	h.store.set(t, ownedKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssa", nil,
		tcpConf("10.0.0.1", "4420"),
	))
	// Owned by range 1, which this instance was not given: expected, and
	// therefore silent.
	h.store.set(t, foreignShardKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssb", nil,
		tcpConf("10.0.0.2", "4420"),
	))
	// One rdma transport (skipped, §0 #2) and one tcp one (served).
	h.store.set(t, mixedKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssc", []string{watchHostNqn},
		trConf("rdma", common.DefaultCdcAdrFam, "10.0.0.3", "4420"),
		tcpConf("10.0.0.3", "4421"),
	))
	// A transport this controller serves, over an address family it cannot
	// name: dropped under its OWN reason, not the transport's, and the tcp
	// ipv4 element beside it still serves (DS3).
	h.store.set(t, foreignFamKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssf", []string{watchHostNqn},
		trConf(common.DefaultCdcTrType, "fc", "10.0.0.6", "4420"),
		tcpConf("10.0.0.6", "4421"),
	))
	// Bytes that are not a CdcEntry: field 31 with wire type 7 never
	// decodes.
	h.store.setRaw(badValueKey, []byte{0xff, 0x01, 0x02})
	h.store.set(t, badKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:sse", nil,
		tcpConf("10.0.0.5", "4420"),
	))
	wantRev := h.store.currentRev()

	h.start()
	scans := h.logs.waitFor(t, msgScanComplete, 1)

	if got := watchAttrInt(t, scans[0], "entries"); got != 3 {
		t.Errorf("cdc scan complete entries = %d, want 3", got)
	}
	if got := watchAttrInt(t, scans[0], "rev"); got != wantRev {
		t.Errorf("cdc scan complete rev = %d, want %d", got, wantRev)
	}
	if got := h.reg.entryCount(); got != 3 {
		t.Errorf("registry holds %d entries, want 3", got)
	}

	reasons := make(map[string]string)
	for _, rec := range h.logs.find(msgEntrySkipped) {
		reasons[watchAttrString(t, rec, "key")] =
			watchAttrString(t, rec, "reason")
	}
	want := map[string]string{
		badKey:        skipMalformedKey,
		badValueKey:   skipMalformedValue,
		mixedKey:      skipForeignTrType,
		foreignFamKey: skipForeignAdrFam,
	}
	if !reflect.DeepEqual(reasons, want) {
		t.Errorf("cdc entry skipped records = %v, want %v", reasons, want)
	}
	// WV2's silence, spelled out: the unowned shard code is never reported.
	if _, ok := reasons[foreignShardKey]; ok {
		t.Errorf("unowned shard code %q was logged, want silence",
			foreignShardKey)
	}

	// The foreign transport and the foreign address family were each
	// dropped and the rest of their entries still serve: this host sees the
	// open entry plus the surviving half of each of the other two.
	h.attachHost(watchHostNqn)
	h.assertView(watchHostNqn, 1, 3)
}

// ---------------------------------------------------------------------------
// DS2 — ownership
// ---------------------------------------------------------------------------

// TestWatchOwnsShardCodes proves DS2/CM2: the --range digit h owns exactly
// the sixteen shard codes h0…hf, at both edges of every range and nowhere
// else.
func TestWatchOwnsShardCodes(t *testing.T) {
	tests := []struct {
		name   string
		ranges []uint32
		probes map[uint32]bool
	}{
		{
			name:   "range 0 owns 00..0f",
			ranges: []uint32{0x0},
			probes: map[uint32]bool{
				0x00: true, 0x0f: true, 0x10: false, 0xff: false,
			},
		},
		{
			name:   "range f owns f0..ff",
			ranges: []uint32{0xf},
			probes: map[uint32]bool{
				0xf0: true, 0xff: true, 0xef: false, 0x00: false,
			},
		},
		{
			name:   "ranges 0 and f, nothing between",
			ranges: []uint32{0x0, 0xf},
			probes: map[uint32]bool{
				0x0f: true, 0x10: false, 0x80: false,
				0xef: false, 0xf0: true,
			},
		},
		{
			name:   "low half 0..7 stops at 7f",
			ranges: []uint32{0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7},
			probes: map[uint32]bool{
				0x00: true, 0x3c: true, 0x7f: true, 0x80: false,
				0xff: false,
			},
		},
		{
			name:   "interior digits 3 and c",
			ranges: []uint32{0x3, 0xc},
			probes: map[uint32]bool{
				0x2f: false, 0x30: true, 0x3c: true, 0x40: false,
				0xc0: true, 0xcf: true, 0xd0: false,
			},
		},
		{
			name: "all sixteen ranges own everything",
			ranges: []uint32{
				0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7,
				0x8, 0x9, 0xa, 0xb, 0xc, 0xd, 0xe, 0xf,
			},
			probes: map[uint32]bool{0x00: true, 0x7f: true, 0xff: true},
		},
		{
			name:   "no range owns nothing",
			ranges: nil,
			probes: map[uint32]bool{0x00: false, 0x80: false, 0xff: false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatcher(&deps{}, newRegistry(), tc.ranges)
			for code, want := range tc.probes {
				if got := w.owns(code); got != want {
					t.Errorf("owns(%#02x) = %v, want %v", code, got, want)
				}
			}
			// The whole 256-code space, so no boundary can hide.
			for code := uint32(0); code < common.ShardBucketSize; code++ {
				want := false
				for _, digit := range tc.ranges {
					if code/16 == digit {
						want = true
					}
				}
				if got := w.owns(code); got != want {
					t.Fatalf("owns(%#02x) = %v, want %v", code, got, want)
				}
			}
			// An out-of-range code is never owned, whatever the ranges.
			if w.owns(common.ShardBucketSize) {
				t.Errorf("owns(%d) = true, want false",
					common.ShardBucketSize)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WV3 — events
// ---------------------------------------------------------------------------

// TestWatchAppliesEvents proves WV3 and its DS6 tail: a put upserts — a new
// key and a re-put of a held one alike — and logs `cdc entry applied` op=put,
// a delete removes and logs op=delete, and either way the impact pass bumps
// the active host's GENCTR and pokes its connection. It also proves WV2 on
// the event path: an unowned shard code is dropped silently while a malformed
// key is logged.
func TestWatchAppliesEvents(t *testing.T) {
	h := newWatchHarness(t, 0x0)
	h.start()
	h.logs.waitFor(t, msgScanComplete, 1)
	gen := h.store.nextWatch(t)

	c := h.attachHost(watchHostNqn)
	h.assertView(watchHostNqn, 1, 0)

	keyA := testKey(0x01, 0x1, 0xa)
	keyB := testKey(0x02, 0x1, 0xb)
	unownedKey := testKey(0x91, 0x1, 0xc)
	badKey := model.CdcEntryPrefix() + "still-not-a-key"

	// A put on an owned key: upsert, log, impact.
	gen.put(keyA, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssa", nil,
		tcpConf("10.0.0.1", "4420"),
	))
	applied := h.logs.waitFor(t, msgEntryApplied, 1)
	if got := watchAttrString(t, applied[0], "key"); got != keyA {
		t.Errorf("cdc entry applied key = %q, want %q", got, keyA)
	}
	if got := watchAttrString(t, applied[0], "op"); got != "put" {
		t.Errorf("cdc entry applied op = %q, want %q", got, "put")
	}
	h.assertView(watchHostNqn, 2, 1)
	if !poked(c) {
		t.Error("put impact did not poke the host's connection")
	}
	changed := h.logs.waitFor(t, msgViewChanged, 1)
	if got := watchAttrString(t, changed[0], "hostnqn"); got != watchHostNqn {
		t.Errorf("view changed hostnqn = %q, want %q", got, watchHostNqn)
	}
	if got := watchAttrInt(t, changed[0], "genctr"); got != 2 {
		t.Errorf("view changed genctr = %d, want 2", got)
	}
	if got := watchAttrInt(t, changed[0], "numrec"); got != 1 {
		t.Errorf("view changed numrec = %d, want 1", got)
	}

	// Two events that must not survive parsing, then one that must. Events
	// are applied serially, so the third one's record proves the first two
	// are done being ignored.
	gen.put(unownedKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssx", nil,
		tcpConf("10.0.0.9", "4420"),
	))
	gen.put(badKey, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssy", nil,
		tcpConf("10.0.0.8", "4420"),
	))
	gen.put(keyB, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssb", []string{watchHostNqn},
		tcpConf("10.0.0.2", "4420"),
	))
	applied = h.logs.waitFor(t, msgEntryApplied, 2)
	if len(applied) != 2 {
		t.Fatalf("%d cdc entry applied records, want 2", len(applied))
	}
	if got := watchAttrString(t, applied[1], "key"); got != keyB {
		t.Errorf("cdc entry applied key = %q, want %q", got, keyB)
	}
	skipped := h.logs.find(msgEntrySkipped)
	if len(skipped) != 1 {
		t.Fatalf("%d cdc entry skipped records, want 1", len(skipped))
	}
	if got := watchAttrString(t, skipped[0], "key"); got != badKey {
		t.Errorf("cdc entry skipped key = %q, want %q", got, badKey)
	}
	if got := watchAttrString(t, skipped[0], "reason"); got != skipMalformedKey {
		t.Errorf("cdc entry skipped reason = %q, want %q",
			got, skipMalformedKey)
	}
	h.assertView(watchHostNqn, 3, 2)
	if !poked(c) {
		t.Error("second put impact did not poke the host's connection")
	}

	// The UPSERT half of WV3: a put on a key already held replaces it in
	// place — the rendered bytes move and the host is impacted, but the
	// entry is not duplicated and the record count does not grow.
	_, _, beforeUpsert := h.reg.snapshot(watchHostNqn)
	gen.put(keyB, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssb", []string{watchHostNqn},
		tcpConf("10.0.0.2", "4421"),
	))
	applied = h.logs.waitFor(t, msgEntryApplied, 3)
	if got := watchAttrString(t, applied[2], "key"); got != keyB {
		t.Errorf("cdc entry applied key = %q, want %q", got, keyB)
	}
	if got := watchAttrString(t, applied[2], "op"); got != "put" {
		t.Errorf("cdc entry applied op = %q, want %q", got, "put")
	}
	h.assertView(watchHostNqn, 4, 2)
	if got := h.reg.entryCount(); got != 2 {
		t.Errorf("registry holds %d entries after the upsert, want 2", got)
	}
	_, _, afterUpsert := h.reg.snapshot(watchHostNqn)
	if bytes.Equal(afterUpsert, beforeUpsert) {
		t.Error("the upsert left the rendered log page unchanged")
	}
	if !poked(c) {
		t.Error("upsert impact did not poke the host's connection")
	}

	// A delete: removal, log, impact.
	gen.del(keyA)
	applied = h.logs.waitFor(t, msgEntryApplied, 4)
	if got := watchAttrString(t, applied[3], "key"); got != keyA {
		t.Errorf("cdc entry applied key = %q, want %q", got, keyA)
	}
	if got := watchAttrString(t, applied[3], "op"); got != "delete" {
		t.Errorf("cdc entry applied op = %q, want %q", got, "delete")
	}
	h.assertView(watchHostNqn, 5, 1)
	if !poked(c) {
		t.Error("delete impact did not poke the host's connection")
	}
	if got := h.reg.entryCount(); got != 1 {
		t.Errorf("registry holds %d entries after the delete, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// WV4 — watch failure
// ---------------------------------------------------------------------------

// TestWatchRestartsOnWatchError proves WV4: any watch error — a plain one and
// a compaction alike — logs `cdc watch restarting` carrying the error and the
// compacted flag, and sends the watcher back to WV1, which rescans and opens
// the next generation at the revision that scan was served at plus one.
func TestWatchRestartsOnWatchError(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantCompacted bool
	}{
		{
			name: "plain error",
			err:  errors.New("watch closed: connection refused"),
		},
		{
			name: "compacted",
			err: fmt.Errorf("watch %q cancelled: %w",
				model.CdcEntryPrefix(), rpctypes.ErrCompacted),
			wantCompacted: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Guard the fixture itself: the compacted case is only a test
			// of WV4 if etcdutil agrees the error IS a compaction.
			if got := etcdutil.IsCompacted(tc.err); got != tc.wantCompacted {
				t.Fatalf("etcdutil.IsCompacted(%v) = %v, want %v",
					tc.err, got, tc.wantCompacted)
			}
			h := newWatchHarness(t, 0x0)
			h.start()
			h.logs.waitFor(t, msgScanComplete, 1)
			gen := h.store.nextWatch(t)

			gen.fail(tc.err)

			recs := h.logs.waitFor(t, msgWatchRestarting, 1)
			if got := watchAttrString(t, recs[0], "error"); got != tc.err.Error() {
				t.Errorf("cdc watch restarting error = %q, want %q",
					got, tc.err.Error())
			}
			if got := watchAttrBool(t, recs[0], "compacted"); got != tc.wantCompacted {
				t.Errorf("cdc watch restarting compacted = %v, want %v",
					got, tc.wantCompacted)
			}

			scans := h.logs.waitFor(t, msgScanComplete, 2)
			rev := watchAttrInt(t, scans[1], "rev")
			next := h.store.nextWatch(t)
			if next.fromRev != rev+1 {
				t.Errorf("new watch from rev %d, want %d",
					next.fromRev, rev+1)
			}
		})
	}
}

// TestWatchRescanDiffsMissedChange proves WV4's second half: the rescan is a
// DIFF against the held state, so a change the watch never delivered an event
// for still impacts the host — GENCTR moves and the connection is poked — the
// moment the watcher recovers.
func TestWatchRescanDiffsMissedChange(t *testing.T) {
	h := newWatchHarness(t, 0x0)
	keyA := testKey(0x01, 0x1, 0xa)
	h.store.set(t, keyA, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssa", nil,
		tcpConf("10.0.0.1", "4420"),
	))
	h.start()
	h.logs.waitFor(t, msgScanComplete, 1)
	gen := h.store.nextWatch(t)

	c := h.attachHost(watchHostNqn)
	h.assertView(watchHostNqn, 1, 1)

	// The gap: the store changes twice while the watch is down — one entry
	// added, one removed — and no event is ever delivered for either.
	keyB := testKey(0x02, 0x1, 0xb)
	h.store.set(t, keyB, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssb", []string{watchHostNqn},
		tcpConf("10.0.0.2", "4420"), tcpConf("10.0.0.2", "4421"),
	))
	h.store.del(keyA)
	gen.fail(errors.New("watch closed: compaction unrelated failure"))

	h.logs.waitFor(t, msgWatchRestarting, 1)
	h.logs.waitFor(t, msgScanComplete, 2)

	// Two rendered records now (ssB's two transports), one GENCTR move for
	// the whole diff, and the connection poked — the AEN is not lost.
	h.assertView(watchHostNqn, 2, 2)
	if !poked(c) {
		t.Error("the rescan diff did not poke the host's connection")
	}
	changed := h.logs.waitFor(t, msgViewChanged, 1)
	if got := watchAttrInt(t, changed[0], "numrec"); got != 2 {
		t.Errorf("view changed numrec = %d, want 2", got)
	}
	if got := h.logs.count(msgEntryApplied); got != 0 {
		t.Errorf("%d cdc entry applied records, want 0: the change was "+
			"never seen as an event", got)
	}
}

// ---------------------------------------------------------------------------
// WV5 — scan failure
// ---------------------------------------------------------------------------

// TestWatchRetriesFailedScan proves WV5 and DS10: a failing Range is retried
// once per Config.rescanInterval on the fake clock and never in a hot loop —
// exactly one timer armed, exactly one Range per interval, none in between —
// while the registry keeps serving the state it held when etcd went away, and
// the recovery rescan diffs that held state through DS6.
func TestWatchRetriesFailedScan(t *testing.T) {
	h := newWatchHarness(t, 0x0)
	keyA := testKey(0x01, 0x1, 0xa)
	h.store.set(t, keyA, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssa", nil,
		tcpConf("10.0.0.1", "4420"),
	))
	h.start()
	h.logs.waitFor(t, msgScanComplete, 1)
	gen := h.store.nextWatch(t)

	c := h.attachHost(watchHostNqn)
	h.assertView(watchHostNqn, 1, 1)
	_, _, held := h.reg.snapshot(watchHostNqn)

	// etcd goes away: the watch dies and every rescan fails.
	h.store.failRange(errors.New("etcd unavailable"))
	gen.fail(errors.New("watch closed: no leader"))
	h.logs.waitFor(t, msgWatchRestarting, 1)

	// The first retry timer proves the failed rescan is already behind us.
	h.clk.waitWaiters(t, 1)
	const scansBeforeOutage = 1
	wantRanges := scansBeforeOutage + 1
	if got := h.store.rangeCount(); got != wantRanges {
		t.Fatalf("%d Range calls after the first failure, want %d",
			got, wantRanges)
	}

	// Half an interval is not an interval: nothing retries.
	h.clk.advance(watchRescan / 2)
	if got := h.store.rangeCount(); got != wantRanges {
		t.Fatalf("%d Range calls half an interval in, want %d",
			got, wantRanges)
	}

	// One Range per interval, one timer at a time, for several intervals.
	for i := 0; i < 4; i++ {
		h.clk.advance(watchRescan)
		h.clk.waitWaiters(t, 1)
		wantRanges++
		if got := h.store.rangeCount(); got != wantRanges {
			t.Fatalf("%d Range calls after %d intervals, want %d",
				got, i+1, wantRanges)
		}
		if got := h.clk.waiterCount(); got != 1 {
			t.Fatalf("%d timers armed after %d intervals, want 1",
				got, i+1)
		}
		// DS10: the held state is served unchanged all the way through,
		// byte for byte and not merely record for record.
		h.assertView(watchHostNqn, 1, 1)
		if _, _, body := h.reg.snapshot(watchHostNqn); !bytes.Equal(body, held) {
			t.Fatalf("the served log page changed during the outage")
		}
	}
	if got := h.logs.count(msgScanComplete); got != scansBeforeOutage {
		t.Errorf("%d cdc scan complete records during the outage, want %d",
			got, scansBeforeOutage)
	}
	if poked(c) {
		t.Error("a failed rescan poked the host's connection")
	}

	// etcd comes back, with a change made while it was gone.
	keyB := testKey(0x02, 0x1, 0xb)
	h.store.set(t, keyB, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssb", []string{watchHostNqn},
		tcpConf("10.0.0.2", "4420"),
	))
	h.store.failRange(nil)
	h.clk.advance(watchRescan)
	h.logs.waitFor(t, msgScanComplete, 2)
	h.assertView(watchHostNqn, 2, 2)
	if !poked(c) {
		t.Error("the recovery rescan did not poke the host's connection")
	}
}

// ---------------------------------------------------------------------------
// WV6 — read-only
// ---------------------------------------------------------------------------

// TestWatchNeverWrites proves WV6 structurally: the etcdStore interface the
// package depends on has no write method to call, and a watcher driven
// through a whole lifecycle — scan, put, delete, watch failure, rescan —
// reaches etcd only through Range, Decode and WatchTyped, leaving the store's
// contents exactly as it found them.
func TestWatchNeverWrites(t *testing.T) {
	iface := reflect.TypeOf((*etcdStore)(nil)).Elem()
	got := make([]string, 0, iface.NumMethod())
	for i := 0; i < iface.NumMethod(); i++ {
		got = append(got, iface.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"Decode", "Range", "WatchTyped"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("etcdStore methods = %v, want %v (WV6 forbids a write)",
			got, want)
	}

	h := newWatchHarness(t, 0x0)
	keyA := testKey(0x01, 0x1, 0xa)
	keyB := testKey(0x02, 0x1, 0xb)
	h.store.set(t, keyA, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssa", nil,
		tcpConf("10.0.0.1", "4420"),
	))
	before := h.store.keys()

	h.start()
	h.logs.waitFor(t, msgScanComplete, 1)
	gen := h.store.nextWatch(t)
	h.attachHost(watchHostNqn)

	gen.put(keyB, cdcEntry(
		"nqn.2026-01.io.dnv-test:cdc:ssb", nil,
		tcpConf("10.0.0.2", "4420"),
	))
	h.logs.waitFor(t, msgEntryApplied, 1)
	gen.del(keyA)
	h.logs.waitFor(t, msgEntryApplied, 2)
	gen.fail(errors.New("watch closed: no leader"))
	h.logs.waitFor(t, msgScanComplete, 2)
	h.store.nextWatch(t)

	for _, name := range h.store.callNames() {
		switch name {
		case "Range", "Decode", "WatchTyped":
		default:
			t.Errorf("watcher called etcdStore.%s, which WV6 forbids", name)
		}
	}
	if after := h.store.keys(); !reflect.DeepEqual(after, before) {
		t.Errorf("store keys = %v, want %v: the watcher changed etcd",
			after, before)
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

// TestWatchStopsOnContextCancel proves the WV1 loop leaves from either state
// it can be parked in: waiting out a WV5 retry interval after a failed scan,
// and blocked on a healthy watch generation.
func TestWatchStopsOnContextCancel(t *testing.T) {
	t.Run("scanning", func(t *testing.T) {
		h := newWatchHarness(t, 0x0)
		h.store.failRange(errors.New("etcd unavailable"))
		h.start()
		// Parked on the retry timer, with no scan ever completed.
		h.clk.waitWaiters(t, 1)
		h.cancel()
		h.waitStopped()
		if got := h.logs.count(msgScanComplete); got != 0 {
			t.Errorf("%d cdc scan complete records, want 0", got)
		}
	})

	t.Run("watching", func(t *testing.T) {
		h := newWatchHarness(t, 0x0)
		h.start()
		h.logs.waitFor(t, msgScanComplete, 1)
		h.store.nextWatch(t)
		h.cancel()
		h.waitStopped()
		// A cancellation is not a watch failure: it restarts nothing.
		if got := h.logs.count(msgWatchRestarting); got != 0 {
			t.Errorf("%d cdc watch restarting records, want 0", got)
		}
	})
}
