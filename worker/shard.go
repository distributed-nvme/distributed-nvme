package worker

import (
	"context"
	"log/slog"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// revKind is the per-role plumbing one shard worker needs (SW1): the rev
// prefix it watches, the message type that prefix holds, the parser of its
// keys and the constructor of the revision workers below it. The three kinds
// differ in nothing else, which is why §7 is one implementation.
type revKind struct {
	role     string
	prefix   func(shard uint32) string
	newMsg   func() proto.Message
	parseKey func(key string) (uint32, uint64, uint64, bool)
	// desired extracts the RW3 desired state from one rev value: the
	// revision plus the handle (addr_port for DnRev/CnRev, sp_name for
	// SpRev).
	desired func(msg proto.Message) (desiredState, bool)
	// newWorker starts one revision worker (SW2). It is a field so the §13
	// shard tests can substitute a recording fake for the real per-object
	// loop.
	newWorker func(p revWorkerParams) revWorkerHandle
}

// kindFor returns the revKind of one role (SW1).
func kindFor(role string) (revKind, bool) {
	switch role {
	case common.WorkerRoleDn:
		return dnKind(), true
	case common.WorkerRoleCn:
		return cnKind(), true
	case common.WorkerRoleSp:
		return spKind(), true
	default:
		return revKind{}, false
	}
}

// revObjKey identifies one revision worker inside a shard (SW2): the
// cluster_id and the object id, both taken from the rev key. Keys of every
// cluster share the shard prefix (SW6).
type revObjKey struct {
	cid uint64
	id  uint64
}

// revObjEntry is one running revision worker and the desired state it was
// last told about (RW3 coalescing).
type revObjEntry struct {
	handle  revWorkerHandle
	desired desiredState
}

// shardWorker watches one owned (role, shard) rev prefix and keeps one
// revision worker per key under it (§7). It is started and stopped only by
// the vote worker (VW9).
type shardWorker struct {
	deps  *deps
	kind  revKind
	shard uint32
	seed  string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// workers is owned by the run goroutine.
	workers map[revObjKey]*revObjEntry
}

// startShardWorker starts one shard worker (SW1/SW2).
func startShardWorker(
	d *deps,
	kind revKind,
	shard uint32,
	seed string,
) *shardWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &shardWorker{
		deps:    d,
		kind:    kind,
		shard:   shard,
		seed:    seed,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		workers: make(map[revObjKey]*revObjEntry),
	}
	go w.run()
	return w
}

// stop is the graceful stop of SW5: cancel the watch, stop every revision
// worker in parallel (RW11), join them. The vote worker logs "shard released"
// only after this returns, so a shard shows as released when nothing is
// driving it any more.
func (w *shardWorker) stop() {
	w.cancel()
	<-w.done
}

// run is the scan-then-watch loop of SW2/SW4.
func (w *shardWorker) run() {
	defer close(w.done)
	defer w.stopAll()
	prefix := w.kind.prefix(w.shard)
	for {
		if w.ctx.Err() != nil {
			return
		}
		kvs, rev, err := w.deps.store.Range(w.ctx, prefix)
		if err != nil {
			// SW4: a failing rescan is retried every vote interval — never a
			// hot loop. The etcdutil record carries the error.
			if !w.wait() {
				return
			}
			continue
		}
		w.applyScan(kvs)
		evCh, errCh := w.deps.store.WatchTyped(
			w.ctx, prefix, rev+1, w.kind.newMsg,
		)
		if !w.watch(evCh, errCh) {
			return
		}
	}
}

// wait sleeps one vote interval before a failed scan is retried (SW4). It
// returns false when the shard worker must stop.
func (w *shardWorker) wait() bool {
	select {
	case <-w.ctx.Done():
		return false
	case <-w.deps.clk.after(w.deps.cfg.VoteInterval):
		return true
	}
}

// watch consumes one watch generation (SW3). It returns true when the caller
// must rescan — a compaction or any other watch error (SW4) — and false when
// the shard worker must stop.
func (w *shardWorker) watch(
	evCh <-chan etcdutil.Event,
	errCh <-chan error,
) bool {
	for {
		select {
		case <-w.ctx.Done():
			return false
		case event, ok := <-evCh:
			if !ok {
				// EU3: the error, if any, is already buffered and both
				// channels are closed, so this never blocks.
				if err := <-errCh; err != nil {
					slog.InfoContext(w.ctx, "rev watch restarting",
						slog.String("role", w.kind.role),
						slog.String("shard", shardCode(w.shard)),
						slog.String("error", err.Error()),
						slog.Bool("compacted", etcdutil.IsCompacted(err)),
					)
				}
				return w.ctx.Err() == nil
			}
			w.applyEvent(event)
		}
	}
}

// applyScan diffs the shard against a full scan (SW2 for the first one, SW4
// for every rescan): start workers for keys not known, update known ones
// whose desired state differs, stop workers whose key is absent.
func (w *shardWorker) applyScan(kvs []etcdutil.KV) {
	seen := make(map[revObjKey]desiredState, len(kvs))
	for _, kv := range kvs {
		key, ok := w.parseKey(kv.Key)
		if !ok {
			continue
		}
		msg := w.kind.newMsg()
		if err := w.deps.store.Decode(w.ctx, kv, msg); err != nil {
			// The etcdutil record carries the decode error (SW2: a malformed
			// value is logged and skipped).
			continue
		}
		desired, ok := w.parseValue(kv.Key, msg)
		if !ok {
			continue
		}
		seen[key] = desired
	}
	for key, desired := range seen {
		w.start(key, desired)
	}
	var stopping []*revObjEntry
	for key, entry := range w.workers {
		if _, ok := seen[key]; ok {
			continue
		}
		stopping = append(stopping, entry)
		delete(w.workers, key)
	}
	stopParallel(stopping)
}

// applyEvent folds one watch event into the shard (SW3): a put for a known
// key updates its revision worker (RW3 coalescing; a put that changes neither
// revision nor handle is a no-op), a put for a new key starts one, a delete
// stops one gracefully and forgets it.
func (w *shardWorker) applyEvent(event etcdutil.Event) {
	key, ok := w.parseKey(event.Key)
	if !ok {
		return
	}
	if event.Type == etcdutil.EventDelete {
		entry, known := w.workers[key]
		if !known {
			return
		}
		delete(w.workers, key)
		entry.handle.stop()
		return
	}
	desired, ok := w.parseValue(event.Key, event.Msg)
	if !ok {
		return
	}
	w.start(key, desired)
}

// parseKey turns one rev key into a revObjKey (SW2). A key this shard's kind
// cannot parse, or one that belongs to another shard, is logged and skipped.
func (w *shardWorker) parseKey(key string) (revObjKey, bool) {
	shard, cid, id, ok := w.kind.parseKey(key)
	if !ok || shard != w.shard {
		slog.InfoContext(w.ctx, "rev key malformed",
			slog.String("role", w.kind.role),
			slog.String("shard", shardCode(w.shard)),
			slog.String("key", key),
		)
		return revObjKey{}, false
	}
	return revObjKey{cid: cid, id: id}, true
}

// parseValue turns one rev value into the RW3 desired state (SW2). A value of
// the wrong message type is logged and skipped.
func (w *shardWorker) parseValue(
	key string,
	msg proto.Message,
) (desiredState, bool) {
	if msg == nil {
		slog.InfoContext(w.ctx, "rev value malformed",
			slog.String("role", w.kind.role),
			slog.String("shard", shardCode(w.shard)),
			slog.String("key", key),
		)
		return desiredState{}, false
	}
	desired, ok := w.kind.desired(msg)
	if !ok {
		slog.InfoContext(w.ctx, "rev value malformed",
			slog.String("role", w.kind.role),
			slog.String("shard", shardCode(w.shard)),
			slog.String("key", key),
		)
		return desiredState{}, false
	}
	return desired, true
}

// start starts a revision worker for a key not known yet, or updates the one
// already running (SW2, SW3).
func (w *shardWorker) start(key revObjKey, desired desiredState) {
	if entry, ok := w.workers[key]; ok {
		if entry.desired == desired {
			// RW3: a put that changes neither revision nor handle.
			return
		}
		entry.desired = desired
		entry.handle.update(desired)
		return
	}
	handle := w.kind.newWorker(revWorkerParams{
		deps:    w.deps,
		role:    w.kind.role,
		shard:   w.shard,
		cid:     key.cid,
		id:      key.id,
		seed:    w.seed,
		desired: desired,
	})
	w.workers[key] = &revObjEntry{handle: handle, desired: desired}
}

// stopAll stops every revision worker of the shard in parallel and joins them
// (SW5).
func (w *shardWorker) stopAll() {
	entries := make([]*revObjEntry, 0, len(w.workers))
	for key, entry := range w.workers {
		entries = append(entries, entry)
		delete(w.workers, key)
	}
	stopParallel(entries)
}

// stopParallel stops a set of revision workers concurrently and joins them
// (SW5, RW11): each may take up to common.DefaultWorkerSyncupTimeout to
// finish an in-flight unary call, so they must not be stopped one by one.
func stopParallel(entries []*revObjEntry) {
	if len(entries) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Add(1)
		go func(e *revObjEntry) {
			defer wg.Done()
			e.handle.stop()
		}(entry)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// The three kinds (SW1)
// ---------------------------------------------------------------------------

// dnKind is the dn role's shard plumbing: {p} dn_rev {s}␠ holding DnRev
// (§8.2).
func dnKind() revKind {
	return revKind{
		role:     common.WorkerRoleDn,
		prefix:   model.DnRevPrefix,
		newMsg:   func() proto.Message { return &pb.DnRev{} },
		parseKey: model.ParseDnRevKey,
		desired: func(msg proto.Message) (desiredState, bool) {
			rev, ok := msg.(*pb.DnRev)
			if !ok {
				return desiredState{}, false
			}
			return desiredState{
				revision: rev.GetRevision(),
				handle:   rev.GetAddrPort(),
			}, true
		},
		newWorker: func(p revWorkerParams) revWorkerHandle {
			return startRevWorker(p, func(w *revWorker) objDriver {
				return newDnDriver(p, w)
			})
		},
	}
}

// cnKind is the cn role's shard plumbing: {p} cn_rev {s}␠ holding CnRev
// (§8.3).
func cnKind() revKind {
	return revKind{
		role:     common.WorkerRoleCn,
		prefix:   model.CnRevPrefix,
		newMsg:   func() proto.Message { return &pb.CnRev{} },
		parseKey: model.ParseCnRevKey,
		desired: func(msg proto.Message) (desiredState, bool) {
			rev, ok := msg.(*pb.CnRev)
			if !ok {
				return desiredState{}, false
			}
			return desiredState{
				revision: rev.GetRevision(),
				handle:   rev.GetAddrPort(),
			}, true
		},
		newWorker: func(p revWorkerParams) revWorkerHandle {
			return startRevWorker(p, func(w *revWorker) objDriver {
				return newCnDriver(p, w)
			})
		},
	}
}

// spKind is the sp role's shard plumbing: {p} sp_rev {s}␠ holding SpRev,
// whose handle is the sp_name (§8.4).
func spKind() revKind {
	return revKind{
		role:     common.WorkerRoleSp,
		prefix:   model.SpRevPrefix,
		newMsg:   func() proto.Message { return &pb.SpRev{} },
		parseKey: model.ParseSpRevKey,
		desired: func(msg proto.Message) (desiredState, bool) {
			rev, ok := msg.(*pb.SpRev)
			if !ok {
				return desiredState{}, false
			}
			return desiredState{
				revision: rev.GetRevision(),
				handle:   rev.GetSpName(),
			}, true
		},
		newWorker: newSpWorker,
	}
}
