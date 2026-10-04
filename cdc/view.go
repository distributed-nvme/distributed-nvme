package cdc

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is cdc.md, The discovery service model: the owned CdcEntry map,
// the per-active-host views rendered out of it, and the DS6 impact pass that
// decides which hosts a change is worth an AEN to.
//
// One mutex serializes every read and every mutation of served state, which is
// the serialization invariant of cdc.md, The etcd watcher — no served state
// is ever mutated concurrently — expressed the way Go expresses it: the
// watcher goroutine
// mutates the entries and runs the impact pass, the connection goroutines
// attach and detach their host state and render the dirty views their
// snapshots find (DS6), all under the same lock, and no reader ever observes
// a half-applied change. Delivery is deliberately NOT done under the
// lock — impact returns the list of connections to poke, deliver only sets
// each connection's pending bit and wakes its AEN goroutine, and that
// goroutine does the socket write, so a host that has stopped reading can
// never stall the watcher.

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
	// skips are the distinct reasons (cdc.md, Log records) DS3 could not
	// render some of this entry's transport configurations under, in
	// first-seen order. Empty for the overwhelmingly common case of an entry
	// that rendered whole.
	skips []string
}

// newEntry renders one CdcEntry value (DS3). A transport configuration with
// a foreign tr_type or address family is dropped, and its reason is kept once
// in e.skips (addSkip), so the caller (watcher.render) logs `cdc entry
// skipped` once per distinct reason, not once per dropped element.
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

// addSkip records one skip reason (cdc.md, Log records), once.
func (e *entry) addSkip(reason string) {
	for _, seen := range e.skips {
		if seen == reason {
			return
		}
	}
	e.skips = append(e.skips, reason)
}

// visibleTo is DS4: an entry is visible exactly to the hostnqns its
// allowed_hosts names, matched as exact strings, so an entry with an empty
// allowed_hosts is visible to no host.
func (e *entry) visibleTo(hostNqn string) bool {
	if e == nil {
		return false
	}
	_, ok := e.allowed[hostNqn]
	return ok
}

// contribution is what the entry puts into one host's view (DS5): its
// rendered records and their count when it is visible to the host, nothing
// when it is not.
func (e *entry) contribution(hostNqn string) ([]byte, uint64) {
	if !e.visibleTo(hostNqn) {
		return nil, 0
	}
	return e.records, e.numRec
}

// sameAs reports whether two renderings of one key carry the same rendered
// records AND the same allowed set, which makes them indistinguishable to
// every host. It is what makes a re-put of an unchanged value a no-op. A pair
// it calls different can still look the same to every host — two renderings
// that name no host, say — and apply's per-host compare then impacts nobody.
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
	// dirty says body may no longer be what the held state renders: an
	// event moved the view since its last render, and genCtr and numRec
	// already say so (DS6). The host's next snapshot renders it, or a
	// rescan does first (WV4).
	dirty bool
	conns map[*conn]struct{}
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
	// renders counts the views rendered, for the unit tests: DS6's cost rule
	// is stated in renders.
	renders int
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
	r.renders++
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

// freshenLocked renders a view an event left dirty (DS6), against the held
// state.
func (r *registry) freshenLocked(h *hostState) {
	if !h.dirty {
		return
	}
	h.body, h.numRec = r.renderLocked(h.hostNqn)
	h.dirty = false
}

// impactLocked is what one impacted host gets (DS6): GENCTR moves, `view
// changed` is logged, and each of its connections is owed a delivery, which
// is appended to out.
func impactLocked(
	ctx context.Context,
	h *hostState,
	out []delivery,
) []delivery {
	h.genCtr++
	slog.InfoContext(ctx, msgViewChanged,
		slog.String("hostnqn", h.hostNqn),
		slog.Uint64("genctr", h.genCtr),
		slog.Uint64("numrec", h.numRec),
	)
	for c := range h.conns {
		out = append(out, delivery{c: c, genCtr: h.genCtr})
	}
	return out
}

// refreshLocked re-renders the given host states and returns the deliveries
// the ones that actually moved earn (DS6). A host whose rendered bytes are
// unchanged gets nothing at all — not an AEN, and not a GENCTR bump. The
// compare is against each host's body, so none of them may be dirty.
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
		out = impactLocked(ctx, h, out)
	}
	return out
}

// apply folds one watched event into the served state and returns the
// deliveries it caused (WV3 -> DS6). A nil after deletes the key.
//
// No view is rendered here. A view is its visible entries' records laid end
// to end in the DS5 order, and the event changes one entry, whose place in
// that order its key fixes: what comes before and after it in a host's view
// stays as it was, so the view moves exactly when the entry's own
// contribution to it does. Deciding that takes, per active host, the entry's
// visibility on both sides and at most a compare of the entry's own records,
// never of a whole view. An impacted host's NUMREC moves by the difference
// and its view is marked dirty, for its next snapshot to render.
func (r *registry) apply(
	ctx context.Context,
	k entryKey,
	after *entry,
) []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.entries[k]
	if before.sameAs(after) {
		// A put that changes nothing visible: no impact, no GENCTR move.
		return nil
	}
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
	var out []delivery
	for _, h := range r.hosts {
		was, wasNum := before.contribution(h.hostNqn)
		is, isNum := after.contribution(h.hostNqn)
		if bytes.Equal(was, is) {
			// Not visible on either side, or the same records on both:
			// the host's view is byte for byte what it was.
			continue
		}
		h.numRec = h.numRec - wasNum + isNum
		h.dirty = true
		out = impactLocked(ctx, h, out)
	}
	return out
}

// replace installs a whole new owned entry map, as a (re)scan produces it, and
// returns the deliveries the diff against the held state earns (WV1, WV4).
//
// Every active host is re-rendered rather than only the ones a per-key diff
// would name: the answer is identical (DS6 impact is defined on rendered
// bytes), and a rescan is rare enough that the cost is irrelevant next to
// getting the "changes missed across the watch gap still AEN" rule exactly
// right. The diff is against what the held state renders, which a dirty
// view's body may no longer say, so a dirty view is first rendered against
// the held state.
func (r *registry) replace(
	ctx context.Context,
	entries map[entryKey]*entry,
) []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hosts {
		r.freshenLocked(h)
	}
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
// connection, which is what makes GENCTR restart at the next one (DS7).
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
// (DS9), rendering the view first when an event left it dirty (DS6): a render
// is paid here, once per read of a moved view, not once per event. The
// returned body is never mutated afterwards — a render builds a new slice
// rather than writing into the old one — so the command can page through it
// without holding anything.
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
	r.freshenLocked(h)
	return h.genCtr, h.numRec, h.body
}

// hostCount is the number of hostnqns currently tracked, for the unit tests.
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
