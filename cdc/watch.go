package cdc

import (
	"context"
	"log/slog"
	"maps"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is cdc.md, The etcd watcher: the scan-then-watch loop over
// {p} cdc that keeps the view registry (DS1 to DS11) current. It is the
// shard.go skeleton of dnv-worker.md SW2 to SW4 minus the per-key workers —
// there is one prefix, one goroutine and one map.
//
// It NEVER writes etcd (WV6): no put, no delete, no lease, no lock.

// watcher owns the etcd side of one dnv-cdc instance.
type watcher struct {
	deps *deps
	reg  *registry
	// owned is the DS2 shard-code set, fixed at startup from --range: range
	// digit h owns the sixteen codes h0…hf. It is an array rather than a
	// map because the lookup happens once per key of every scan.
	owned [common.ShardBucketSize]bool
	// skipped is the set of `cdc entry skipped` records the last scan that
	// wrote its records owed, each as its key and reason; a full read that
	// owes the same set and finds the held entries writes none (WV7). Only
	// the run goroutine touches it.
	skipped map[string]struct{}
	// scanned is closed by the first scan that lands, and only by the run
	// goroutine. Until then the registry holds no state at all — empty, not
	// "no subsystems" — so Run keeps the accept loop shut (CM4).
	scanned chan struct{}
}

// newWatcher fixes the owned shard-code set from the configured ranges (DS2,
// CM2). The ranges are already validated by cmd/dnv-cdc.
func newWatcher(d *deps, reg *registry, ranges []uint32) *watcher {
	w := &watcher{deps: d, reg: reg, scanned: make(chan struct{})}
	for _, digit := range ranges {
		base := digit * 16
		for i := uint32(0); i < 16; i++ {
			w.owned[base+i] = true
		}
	}
	return w
}

// owns is DS2: an entry is served iff its key's shard code is in the set.
func (w *watcher) owns(shard uint32) bool {
	if shard >= common.ShardBucketSize {
		return false
	}
	return w.owned[shard]
}

// run is the WV1/WV4/WV5/WV7 loop: scan, watch, and on any watch failure,
// or once a watch generation has run its full-read interval, scan again. It
// returns when ctx ends.
func (w *watcher) run(ctx context.Context) {
	prefix := model.CdcEntryPrefix()
	full := false
	for {
		if ctx.Err() != nil {
			return
		}
		genCtx := common.WithTraceId(ctx, common.NewTraceId())
		rev, ok := w.scan(genCtx, prefix, full)
		if !ok {
			// WV5: while a failed scan is retried the server keeps answering
			// from the held state — or, before the first scan has landed,
			// answers nobody (CM4). The etcdutil record carries the error.
			if !w.wait(ctx) {
				return
			}
			continue
		}
		// CM4: the first scan that lands lets the accept loop start.
		select {
		case <-w.scanned:
		default:
			close(w.scanned)
		}
		// Each generation has its own ctx, so a generation that ends on
		// the full-read timer (WV7) closes its etcd watch before the scan.
		watchCtx, cancel := context.WithCancel(genCtx)
		evCh, errCh := w.deps.store.WatchTyped(
			watchCtx, prefix, rev+1,
			func() proto.Message { return &pb.CdcEntry{} },
		)
		again, timed := w.watch(ctx, evCh, errCh)
		cancel()
		if !again {
			return
		}
		full = timed
	}
}

// wait sleeps one rescan interval before a failed scan is retried (WV5). It
// returns false when the watcher must stop.
func (w *watcher) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-w.deps.clk.after(w.deps.cfg.rescanInterval()):
		return true
	}
}

// skipRecord is one `cdc entry skipped` record a scan owes (WV2), which a
// full read that finds nothing new does not write (WV7).
type skipRecord struct {
	key    string
	reason string
	warn   bool
}

// scan is WV1/WV2: range the whole discovery prefix, keep the owned entries,
// install them and log `cdc scan complete`. The rescan DIFFS — replace
// re-renders every active host against the new map and impacts, by the DS6
// rule, the ones whose bytes moved from what the held state renders — so
// AENs are not lost across a watch gap (WV4). A full read (WV7) that finds
// the scanned map equal to the held one, and owes the same skip records the
// last scan that wrote them owed, installs nothing, renders no host, writes
// none of them, and logs its `cdc scan complete` at Debug, as it repeats and
// changes nothing (log.md R6). It reports the revision the scan was served
// at.
func (w *watcher) scan(ctx context.Context, prefix string, full bool) (int64, bool) {
	kvs, rev, err := w.deps.store.Range(ctx, prefix)
	if err != nil {
		return 0, false
	}
	entries := make(map[entryKey]*entry, len(kvs))
	var skips []skipRecord
	for _, kv := range kvs {
		k, ok, malformed := w.keyOf(kv.Key)
		if malformed {
			skips = append(skips, skipRecord{kv.Key, skipMalformedKey, true})
		}
		if !ok {
			continue
		}
		msg := &pb.CdcEntry{}
		if err := w.deps.store.Decode(ctx, kv, msg); err != nil {
			// The etcdutil record carries the decode error; this one says
			// what dnv-cdc did about it (WV2), at Warn: the entry is gone
			// from every host until an operator mends the record, once per
			// scan that changes the served state or event (log.md R6).
			skips = append(skips, skipRecord{kv.Key, skipMalformedValue, true})
			continue
		}
		e := newEntry(msg)
		for _, reason := range e.skips {
			skips = append(skips, skipRecord{kv.Key, reason, false})
		}
		entries[k] = e
	}
	owed := make(map[string]struct{}, len(skips))
	for _, r := range skips {
		owed[r.key+"\x00"+r.reason] = struct{}{}
	}
	if full && maps.Equal(owed, w.skipped) && w.reg.holds(entries) {
		slog.DebugContext(ctx, msgScanComplete,
			slog.Int("entries", len(entries)),
			slog.Int64("rev", rev),
		)
		return rev, true
	}
	w.skipped = owed
	for _, r := range skips {
		level := slog.LevelInfo
		if r.warn {
			level = slog.LevelWarn
		}
		slog.Log(ctx, level, msgEntrySkipped,
			slog.String("key", r.key),
			slog.String("reason", r.reason),
		)
	}
	deliver(w.reg.replace(ctx, entries))
	slog.InfoContext(ctx, msgScanComplete,
		slog.Int("entries", len(entries)),
		slog.Int64("rev", rev),
	)
	return rev, true
}

// watch consumes one watch generation (WV3). It returns true when the caller
// must rescan — a compaction or any other watch error (WV4), or the
// generation's full-read interval passed (WV7), which the second result
// says — and false when the watcher must stop.
func (w *watcher) watch(
	ctx context.Context,
	evCh <-chan etcdutil.Event,
	errCh <-chan error,
) (bool, bool) {
	fullRead, stop := w.deps.clk.timer(w.deps.cfg.fullReadInterval())
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return false, false
		case <-fullRead:
			// WV7: a watch that still hears etcd's progress notifications
			// while it loses events is noticed by nothing else, so the
			// generation ends here and the rescan diffs against the held
			// state (WV4).
			return true, true
		case event, ok := <-evCh:
			if !ok {
				// The generation's timer goes before its record, so that no
				// one who reads the record sees it still armed.
				stop()
				// EU3: the error, if any, is already buffered and both
				// channels are closed, so this never blocks.
				if err := <-errCh; err != nil {
					slog.InfoContext(ctx, msgWatchRestarting,
						slog.String("error", err.Error()),
						slog.Bool("compacted", etcdutil.IsCompacted(err)),
					)
				}
				return ctx.Err() == nil, false
			}
			w.applyEvent(common.WithTraceId(ctx, common.NewTraceId()), event)
		}
	}
}

// applyEvent folds one watch event into the registry (WV3): a put upserts the
// entry, a delete removes it, and either way the DS6 impact pass runs.
func (w *watcher) applyEvent(ctx context.Context, event etcdutil.Event) {
	k, ok := w.parseKey(ctx, event.Key)
	if !ok {
		return
	}
	if event.Type == etcdutil.EventDelete {
		deliver(w.reg.apply(ctx, k, nil))
		slog.InfoContext(ctx, msgEntryApplied,
			slog.String("key", event.Key),
			slog.String("op", etcdutil.EventDelete.String()),
		)
		return
	}
	msg, ok := event.Msg.(*pb.CdcEntry)
	if !ok || msg == nil {
		slog.WarnContext(ctx, msgEntrySkipped,
			slog.String("key", event.Key),
			slog.String("reason", skipMalformedValue),
		)
		return
	}
	deliver(w.reg.apply(ctx, k, w.render(ctx, event.Key, msg)))
	slog.InfoContext(ctx, msgEntryApplied,
		slog.String("key", event.Key),
		slog.String("op", etcdutil.EventPut.String()),
	)
}

// parseKey turns one key under the discovery prefix into an entryKey (WV2). A
// key whose shard code this instance does not own is skipped SILENTLY — that
// is the expected key-field filter of architecture.md, dnv-cdc, not an
// anomaly — while a key that does not parse at all is logged and dropped.
func (w *watcher) parseKey(ctx context.Context, key string) (entryKey, bool) {
	k, ok, malformed := w.keyOf(key)
	if malformed {
		slog.WarnContext(ctx, msgEntrySkipped,
			slog.String("key", key),
			slog.String("reason", skipMalformedKey),
		)
	}
	return k, ok
}

// keyOf is parseKey without the record: ok for an owned key, malformed for
// one that does not parse.
func (w *watcher) keyOf(key string) (entryKey, bool, bool) {
	cid, shard, spId, ssId, ok := model.ParseCdcEntryKey(key)
	if !ok {
		return entryKey{}, false, true
	}
	if !w.owns(shard) {
		return entryKey{}, false, false
	}
	return entryKey{cid: cid, shard: shard, spId: spId, ssId: ssId}, true, false
}

// render turns one CdcEntry value into its rendered entry (DS3) and logs the
// transport configurations it had to drop. The rest of the entry still serves
// (DS3).
func (w *watcher) render(
	ctx context.Context,
	key string,
	msg *pb.CdcEntry,
) *entry {
	e := newEntry(msg)
	// One record per distinct reason, not one per dropped element: the
	// operator needs to know THAT an entry is being served short and why,
	// and a subsystem with eight foreign ports is one mistake, not eight.
	for _, reason := range e.skips {
		slog.InfoContext(ctx, msgEntrySkipped,
			slog.String("key", key),
			slog.String("reason", reason),
		)
	}
	return e
}
