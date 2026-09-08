package cdc

import (
	"context"
	"log/slog"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the §4 etcd watcher: the scan-then-watch loop over
// {p} cdc that keeps the §3 registry current. It is the shard.go skeleton of
// dnv-worker.md §7 minus the per-key workers — there is one prefix, one
// goroutine and one map.
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
}

// newWatcher fixes the owned shard-code set from the configured ranges (DS2,
// CM2). The ranges are already validated by cmd/dnv-cdc.
func newWatcher(d *deps, reg *registry, ranges []uint32) *watcher {
	w := &watcher{deps: d, reg: reg}
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

// run is the WV1/WV4/WV5 loop: scan, watch, and on any watch failure scan
// again. It returns when ctx ends.
func (w *watcher) run(ctx context.Context) {
	prefix := model.CdcEntryPrefix()
	for {
		if ctx.Err() != nil {
			return
		}
		genCtx := common.WithTraceId(ctx, common.NewTraceId())
		rev, ok := w.scan(genCtx, prefix)
		if !ok {
			// WV5: the server keeps answering from the held state while a
			// failed scan is retried. The etcdutil record carries the error.
			if !w.wait(ctx) {
				return
			}
			continue
		}
		evCh, errCh := w.deps.store.WatchTyped(
			genCtx, prefix, rev+1,
			func() proto.Message { return &pb.CdcEntry{} },
		)
		if !w.watch(ctx, evCh, errCh) {
			return
		}
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

// scan is WV1/WV2: range the whole discovery prefix, keep the owned entries,
// install them and log `cdc scan complete`. The rescan DIFFS — replace runs
// every change through the DS6 impact pass — so AENs are not lost across a
// watch gap (WV4). It reports the revision the scan was served at.
func (w *watcher) scan(ctx context.Context, prefix string) (int64, bool) {
	kvs, rev, err := w.deps.store.Range(ctx, prefix)
	if err != nil {
		return 0, false
	}
	entries := make(map[entryKey]*entry, len(kvs))
	for _, kv := range kvs {
		k, ok := w.parseKey(ctx, kv.Key)
		if !ok {
			continue
		}
		msg := &pb.CdcEntry{}
		if err := w.deps.store.Decode(ctx, kv, msg); err != nil {
			// The etcdutil record carries the decode error; this one says
			// what dnv-cdc did about it (WV2).
			slog.InfoContext(ctx, msgEntrySkipped,
				slog.String("key", kv.Key),
				slog.String("reason", skipMalformedValue),
			)
			continue
		}
		entries[k] = w.render(ctx, kv.Key, msg)
	}
	deliver(w.reg.replace(ctx, entries))
	slog.InfoContext(ctx, msgScanComplete,
		slog.Int("entries", len(entries)),
		slog.Int64("rev", rev),
	)
	return rev, true
}

// watch consumes one watch generation (WV3). It returns true when the caller
// must rescan — a compaction or any other watch error (WV4) — and false when
// the watcher must stop.
func (w *watcher) watch(
	ctx context.Context,
	evCh <-chan etcdutil.Event,
	errCh <-chan error,
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case event, ok := <-evCh:
			if !ok {
				// EU3: the error, if any, is already buffered and both
				// channels are closed, so this never blocks.
				if err := <-errCh; err != nil {
					slog.InfoContext(ctx, msgWatchRestarting,
						slog.String("error", err.Error()),
						slog.Bool("compacted", etcdutil.IsCompacted(err)),
					)
				}
				return ctx.Err() == nil
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
		slog.InfoContext(ctx, msgEntrySkipped,
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
// is the expected §12 key-field filter, not an anomaly — while a key that does
// not parse at all is logged and dropped.
func (w *watcher) parseKey(ctx context.Context, key string) (entryKey, bool) {
	cid, shard, spId, ssId, ok := model.ParseCdcEntryKey(key)
	if !ok {
		slog.InfoContext(ctx, msgEntrySkipped,
			slog.String("key", key),
			slog.String("reason", skipMalformedKey),
		)
		return entryKey{}, false
	}
	if !w.owns(shard) {
		return entryKey{}, false
	}
	return entryKey{cid: cid, shard: shard, spId: spId, ssId: ssId}, true
}

// render turns one CdcEntry value into its rendered entry (DS3) and logs the
// transport configurations it had to drop. The rest of the entry still serves
// (§0 #2).
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
