package cdc

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the §3 discovery service model: the owned CdcEntry map, the
// per-active-host views rendered out of it, and the DS6 impact pass that
// decides which hosts a change is worth an AEN to.
//
// One mutex serializes every read and every mutation of served state, which is
// the §4 rule that "every mutation of served state happens on one goroutine"
// expressed the way Go expresses it: the watcher mutates, the connections
// read, and neither ever observes a half-applied change. Delivery is
// deliberately NOT done under the lock — impact returns the list of
// connections to poke and the caller writes to their sockets afterwards, so a
// host that has stopped reading can never stall the watcher.

// ---------------------------------------------------------------------------
// Entries (DS1)
// ---------------------------------------------------------------------------

// entryKey identifies one CdcEntry by its key fields, in the order the key
// spells them: cluster_id, shard_code, sp_id, ss_id. That order IS the DS5
// ordering of the rendered log.
type entryKey struct {
	cid   uint64
	shard uint32
	spId  uint64
	ssId  uint64
}

// less is the DS5 total order over entry keys.
func (k entryKey) less(o entryKey) bool {
	if k.cid != o.cid {
		return k.cid < o.cid
	}
	if k.shard != o.shard {
		return k.shard < o.shard
	}
	if k.spId != o.spId {
		return k.spId < o.spId
	}
	return k.ssId < o.ssId
}

// entry is one owned CdcEntry, already rendered (DS3). records is the
// concatenation of its log entries in tr-conf order, so building a host's view
// is a copy and never a re-render.
type entry struct {
	nqn     string
	allowed map[string]struct{}
	records []byte
	numRec  uint64
	// skips are the distinct §7 reasons DS3 could not render some of this
	// entry's transport configurations under, in first-seen order. Empty
	// for the overwhelmingly common case of an entry that rendered whole.
	skips []string
}

// newEntry renders one CdcEntry value (DS3). skipped counts the transport
// configurations dropped for a foreign tr_type or address family, which the
// caller logs once per entry.
func newEntry(msg *pb.CdcEntry) *entry {
	e := &entry{nqn: msg.GetNqn()}
	if hosts := msg.GetAllowedHosts(); len(hosts) > 0 {
		e.allowed = make(map[string]struct{}, len(hosts))
		for _, host := range hosts {
			e.allowed[host] = struct{}{}
		}
	}
	for _, conf := range msg.GetNvmeTrConfList() {
		record, skip := renderEntry(e.nqn, conf)
		if skip != "" {
			e.addSkip(skip)
			continue
		}
		e.records = append(e.records, record...)
		e.numRec++
	}
	return e
}

// addSkip records one §7 skip reason, once.
func (e *entry) addSkip(reason string) {
	for _, seen := range e.skips {
		if seen == reason {
			return
		}
	}
	e.skips = append(e.skips, reason)
}

// visibleTo is DS4: an entry with an empty allowed_hosts is visible to
// everyone, otherwise exactly to the hostnqns it names, matched as exact
// strings.
func (e *entry) visibleTo(hostNqn string) bool {
	if e == nil {
		return false
	}
	if len(e.allowed) == 0 {
		return true
	}
	_, ok := e.allowed[hostNqn]
	return ok
}

// sameAs reports whether two renderings of one key are indistinguishable to
// every host: same rendered records AND same visibility. It is what makes a
// re-put of an unchanged value a no-op.
func (e *entry) sameAs(o *entry) bool {
	if e == nil || o == nil {
		return e == o
	}
	if !bytes.Equal(e.records, o.records) {
		return false
	}
	if len(e.allowed) != len(o.allowed) {
		return false
	}
	for host := range e.allowed {
		if _, ok := o.allowed[host]; !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Host state (DS7)
// ---------------------------------------------------------------------------

// hostState is one active hostnqn's view. It exists exactly while the hostnqn
// holds at least one connection to THIS instance (DS7): created at the first
// Connect with GENCTR 1, shared by every connection of the host, dropped at
// the last disconnect. Hosts without a connection are never tracked and never
// notified — they catch up by reading the log when they next connect.
type hostState struct {
	hostNqn string
	genCtr  uint64
	numRec  uint64
	body    []byte
	conns   map[*conn]struct{}
}

// delivery is one connection that must be told its view moved (DS6 -> NP11).
// The caller performs it outside the registry lock.
type delivery struct {
	c      *conn
	genCtr uint64
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// registry holds the whole served state of one dnv-cdc instance: the owned
// entries and the views of the hosts currently connected to it. Nothing here
// is persisted and nothing is ever written back to etcd (WV6, DS11).
type registry struct {
	mu      sync.Mutex
	entries map[entryKey]*entry
	order   []entryKey
	hosts   map[string]*hostState
}

func newRegistry() *registry {
	return &registry{
		entries: make(map[entryKey]*entry),
		hosts:   make(map[string]*hostState),
	}
}

// entryCount is the owned entry count the `cdc scan complete` record reports.
func (r *registry) entryCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// insertOrder places a new key in the DS5 order.
func (r *registry) insertOrder(k entryKey) {
	i := sort.Search(len(r.order), func(i int) bool {
		return !r.order[i].less(k)
	})
	r.order = append(r.order, entryKey{})
	copy(r.order[i+1:], r.order[i:])
	r.order[i] = k
}

// removeOrder takes a key back out of the DS5 order.
func (r *registry) removeOrder(k entryKey) {
	i := sort.Search(len(r.order), func(i int) bool {
		return !r.order[i].less(k)
	})
	if i < len(r.order) && r.order[i] == k {
		r.order = append(r.order[:i], r.order[i+1:]...)
	}
}

// renderLocked builds one host's view (DS5): the rendered records of its
// visible owned entries, concatenated in the deterministic order.
func (r *registry) renderLocked(hostNqn string) ([]byte, uint64) {
	var body []byte
	var numRec uint64
	for _, k := range r.order {
		e := r.entries[k]
		if !e.visibleTo(hostNqn) {
			continue
		}
		body = append(body, e.records...)
		numRec += e.numRec
	}
	return body, numRec
}

// refreshLocked re-renders the given host states and returns the deliveries
// the ones that actually moved earn (DS6). A host whose rendered bytes are
// unchanged gets nothing at all — not an AEN, and not a GENCTR bump.
func (r *registry) refreshLocked(
	ctx context.Context,
	cands []*hostState,
) []delivery {
	var out []delivery
	for _, h := range cands {
		body, numRec := r.renderLocked(h.hostNqn)
		if bytes.Equal(body, h.body) {
			continue
		}
		h.body = body
		h.numRec = numRec
		h.genCtr++
		slog.InfoContext(ctx, msgViewChanged,
			slog.String("hostnqn", h.hostNqn),
			slog.Uint64("genctr", h.genCtr),
			slog.Uint64("numrec", h.numRec),
		)
		for c := range h.conns {
			out = append(out, delivery{c: c, genCtr: h.genCtr})
		}
	}
	return out
}

// candidatesLocked are the active hosts one entry change can possibly matter
// to (DS6): those the entry was visible to before, or is visible to after. A
// host on neither side is skipped without so much as a render.
func (r *registry) candidatesLocked(before *entry, after *entry) []*hostState {
	var cands []*hostState
	for _, h := range r.hosts {
		if before.visibleTo(h.hostNqn) || after.visibleTo(h.hostNqn) {
			cands = append(cands, h)
		}
	}
	return cands
}

// apply folds one watched event into the served state and returns the
// deliveries it caused (WV3 -> DS6). A nil after deletes the key.
func (r *registry) apply(
	ctx context.Context,
	k entryKey,
	after *entry,
) []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.entries[k]
	if before.sameAs(after) {
		// A put that changes nothing visible: no render, no GENCTR move.
		return nil
	}
	cands := r.candidatesLocked(before, after)
	switch {
	case after == nil:
		delete(r.entries, k)
		r.removeOrder(k)
	case before == nil:
		r.entries[k] = after
		r.insertOrder(k)
	default:
		r.entries[k] = after
	}
	return r.refreshLocked(ctx, cands)
}

// replace installs a whole new owned entry map, as a (re)scan produces it, and
// returns the deliveries the diff against the held state earns (WV1, WV4).
//
// Every active host is re-rendered rather than only the ones a per-key diff
// would name: the answer is identical (DS6 impact is defined on rendered
// bytes), and a rescan is rare enough that the cost is irrelevant next to
// getting the "changes missed across the watch gap still AEN" rule exactly
// right.
func (r *registry) replace(
	ctx context.Context,
	entries map[entryKey]*entry,
) []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = entries
	r.order = make([]entryKey, 0, len(entries))
	for k := range entries {
		r.order = append(r.order, k)
	}
	sort.Slice(r.order, func(i, j int) bool {
		return r.order[i].less(r.order[j])
	})
	cands := make([]*hostState, 0, len(r.hosts))
	for _, h := range r.hosts {
		cands = append(cands, h)
	}
	return r.refreshLocked(ctx, cands)
}

// attach registers one connection under its hostnqn (DS7). The first
// connection of a hostnqn creates the host state with GENCTR 1 and its view
// rendered; every later one shares it.
func (r *registry) attach(hostNqn string, c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[hostNqn]
	if !ok {
		h = &hostState{
			hostNqn: hostNqn,
			genCtr:  1,
			conns:   make(map[*conn]struct{}),
		}
		h.body, h.numRec = r.renderLocked(hostNqn)
		r.hosts[hostNqn] = h
	}
	h.conns[c] = struct{}{}
}

// detach unregisters one connection (NP13). The host state dies with its last
// connection, which is what makes GENCTR restart at the next one (§0 #6).
func (r *registry) detach(hostNqn string, c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[hostNqn]
	if !ok {
		return
	}
	delete(h.conns, c)
	if len(h.conns) == 0 {
		delete(r.hosts, hostNqn)
	}
}

// snapshot takes the (genctr, records) pair one Get Log Page command serves
// (DS9). The returned body is never mutated afterwards — refreshLocked
// replaces the slice rather than writing into it — so the command can page
// through it without holding anything.
func (r *registry) snapshot(hostNqn string) (uint64, uint64, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[hostNqn]
	if !ok {
		// Only a connection that has completed Connect asks, and that
		// connection is attached, so this is unreachable in practice; an
		// empty log is nonetheless the right answer to "a host nobody
		// tracks".
		return 0, 0, nil
	}
	return h.genCtr, h.numRec, h.body
}

// hostCount is the number of hostnqns currently tracked, for the §8 tests.
func (r *registry) hostCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hosts)
}

// deliver performs the NP11 half of an impact, outside the registry lock: each
// impacted connection gets its pending bit set and, if it has an armed AER and
// the host has enabled the notice, an immediate completion.
func deliver(deliveries []delivery) {
	for _, d := range deliveries {
		d.c.notify(d.genCtr)
	}
}
