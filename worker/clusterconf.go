package worker

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// confCache is the process-wide ClusterConf cache of RW21: one
// Range(ClusterConfPrefix()) followed by one WatchTyped from rev + 1, feeding
// a map keyed by cluster_id.
//
// The key of a ClusterConf is its cluster NAME (the only name-keyed message,
// MD2) while everything else in the worker addresses clusters by id, so the
// cache derives model.ClusterId(name, value.creation_epoch) on every put and
// remembers the name -> id mapping, which is what lets a delete — whose event
// carries no value — find the entry to drop.
//
// Readers get an immutable, defaults-resolved snapshot (model.ResolveClusterConf):
// the four health-check intervals (0 => 5, clamped to [1, 3600]), extent_size
// (0 => DefaultDnExtSize) and the dn_bin_conf shifts, with qos_ratio as
// stored. A cluster absent from the cache makes its revision workers idle
// (RW9, SW6) — the worker never guesses defaults for an unknown cluster.
type confCache struct {
	deps *deps

	mu      sync.RWMutex
	entries map[uint64]*pb.ClusterConf
	names   map[string]uint64
}

// newConfCache builds an empty cache. It reads nothing until run is called.
func newConfCache(d *deps) *confCache {
	return &confCache{
		deps:    d,
		entries: make(map[uint64]*pb.ClusterConf),
		names:   make(map[string]uint64),
	}
}

// get returns the resolved snapshot of one cluster's configuration (RW9). The
// returned message is never mutated by the cache, so callers may hold it for
// as long as they like.
func (c *confCache) get(cid uint64) (*pb.ClusterConf, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cc, ok := c.entries[cid]
	return cc, ok
}

// run keeps the cache fed until ctx ends (RW21). Watch errors and compaction
// are handled like SW4: rescan, diff, restart the watch at the new rev + 1; a
// failing rescan is retried every vote interval, never in a hot loop.
func (c *confCache) run(ctx context.Context) {
	prefix := model.ClusterConfPrefix()
	for {
		if ctx.Err() != nil {
			return
		}
		kvs, rev, err := c.deps.store.Range(ctx, prefix)
		if err != nil {
			if !c.wait(ctx) {
				return
			}
			continue
		}
		c.applyScan(ctx, kvs)
		evCh, errCh := c.deps.store.WatchTyped(
			ctx, prefix, rev+1,
			func() proto.Message { return &pb.ClusterConf{} },
		)
		if !c.watch(ctx, evCh, errCh) {
			return
		}
	}
}

// watch consumes one watch generation. It returns true when the caller must
// rescan (the watch was cancelled by the server, ErrCompacted above all) and
// false when ctx ended.
func (c *confCache) watch(
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
					slog.InfoContext(ctx, "cluster conf watch restarting",
						slog.String("error", err.Error()),
						slog.Bool("compacted", etcdutil.IsCompacted(err)),
					)
				}
				return ctx.Err() == nil
			}
			c.applyEvent(ctx, event)
		}
	}
}

// wait sleeps one vote interval before a failed scan is retried (RW21 via
// SW4: never a hot loop). It returns false when ctx ended first.
func (c *confCache) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-c.deps.clk.after(c.deps.cfg.VoteInterval):
		return true
	}
}

// applyScan replaces the cache with the scan's content (RW21). A scan is
// authoritative: a cluster that vanished while the watch was down disappears
// here.
func (c *confCache) applyScan(ctx context.Context, kvs []etcdutil.KV) {
	entries := make(map[uint64]*pb.ClusterConf, len(kvs))
	names := make(map[string]uint64, len(kvs))
	for _, kv := range kvs {
		name, ok := clusterConfName(kv.Key)
		if !ok {
			slog.InfoContext(ctx, "cluster conf key malformed",
				slog.String("key", kv.Key),
			)
			continue
		}
		cc := &pb.ClusterConf{}
		if err := c.deps.store.Decode(ctx, kv, cc); err != nil {
			continue
		}
		cid := model.ClusterId(name, cc.GetCreationEpoch())
		entries[cid] = model.ResolveClusterConf(cc)
		names[name] = cid
	}
	c.mu.Lock()
	c.entries = entries
	c.names = names
	c.mu.Unlock()
}

// applyEvent folds one watch event into the cache (RW21).
func (c *confCache) applyEvent(ctx context.Context, event etcdutil.Event) {
	name, ok := clusterConfName(event.Key)
	if !ok {
		slog.InfoContext(ctx, "cluster conf key malformed",
			slog.String("key", event.Key),
		)
		return
	}
	if event.Type == etcdutil.EventDelete {
		c.mu.Lock()
		if cid, known := c.names[name]; known {
			delete(c.entries, cid)
			delete(c.names, name)
		}
		c.mu.Unlock()
		return
	}
	cc, ok := event.Msg.(*pb.ClusterConf)
	if !ok {
		slog.InfoContext(ctx, "cluster conf value malformed",
			slog.String("key", event.Key),
		)
		return
	}
	cid := model.ClusterId(name, cc.GetCreationEpoch())
	c.mu.Lock()
	// creation_epoch is immutable, but a cluster deleted and re-created under
	// the same name gets a new id; drop the stale entry so the old id stops
	// resolving.
	if old, known := c.names[name]; known && old != cid {
		delete(c.entries, old)
	}
	c.entries[cid] = model.ResolveClusterConf(cc)
	c.names[name] = cid
	c.mu.Unlock()
}

// clusterConfName recovers the cluster name from a ClusterConf key (RW21).
// The key is "{p} cluster_conf {name}" and a name never contains a space
// (common.ValidStrPattern), so trimming the prefix is the whole parse.
func clusterConfName(key string) (string, bool) {
	prefix := model.ClusterConfPrefix()
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	name := key[len(prefix):]
	if name == "" || strings.Contains(name, " ") {
		return "", false
	}
	return name, true
}
