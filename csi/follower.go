package csi

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The follower is the node plugin's part of dnv's host contract (csi.md
// CSI15; architecture.md, Host view): while a volume is staged, the node
// holds a path to every cntlr the cdc lists for the volume's subsystem and
// drops a path to one the cdc has withdrawn once it holds a path to a listed
// one, so a fenced old primary answers the node's IO with errors only while
// the node can reach no listed cntlr. It keeps the set of staged subsystems
// and the cdc addresses their attaches named, holds a persistent discovery
// connection to each cdc, and on every trigger reads the discovery logs and
// connects or drops paths to match.
const (
	// DefaultSyncPeriod is the follower's timer, the safety net under the
	// kernel's change notification.
	DefaultSyncPeriod = 30 * time.Second

	// dropConcurrency bounds the path drops running at once: a drop that
	// hangs holds one of the OS client's slots, and the rest must stay
	// free for the stages and syncs of other volumes.
	dropConcurrency = 8

	msgPathConnected     = "csi path connected"
	msgPathDropped       = "csi path dropped"
	msgDiscoveryOpened   = "csi discovery connection opened"
	msgDiscoveryDropped  = "csi discovery connection dropped"
	msgFollowerSyncError = "csi follower sync error"
)

type follower struct {
	host   *nvmeHost
	period time.Duration

	// mu runs the follower's decisions one at a time, a stage, a sync, an
	// unstage's forget and drop of the discovery connections, and the stop's
	// drop, and guards the sets below: an unstage that found nothing staged
	// would otherwise forget the cdcs while a stage of another volume ran,
	// and the next sync, hearing from no cdc, would drop that volume's paths.
	// The path drops and an unstage's own disconnect run outside it.
	mu     sync.Mutex
	staged map[string]bool // subsystem NQN
	// cdcs are the cdcs the follower follows: the configured ones, known
	// from the start, and those the attaches named.
	cdcs map[string]endpoint
	// conf are the configured cdcs, which a drop of the discovery
	// connections leaves known.
	conf map[string]endpoint

	// kick asks the follower's loop for a sync; a kick that finds one
	// pending is folded into it.
	kick chan struct{}

	// dropping holds the path drops running outside mu, by controller
	// name: an `nvme disconnect` of a target that vanished can wait out the
	// kernel's admin timeout, and under mu it would hold up every other
	// volume's stage. A controller already being dropped is not dropped
	// again.
	dropping map[string]*pathDrop
	// dropSlots holds a slot for each drop running (dropConcurrency).
	dropSlots chan struct{}
	// unstaging holds the subsystems an unstage is dropping outside the
	// lock, each with a channel closed when that unstage returns; a stage
	// of one waits for it, or it would connect paths the unstage then drops.
	unstaging map[string]chan struct{}
}

// pathDrop is one path drop running outside the follower's lock; done is
// closed when its disconnect has returned.
type pathDrop struct {
	nqn  string
	done chan struct{}
}

func newFollower(host *nvmeHost, period time.Duration, conf []endpoint) *follower {
	if period <= 0 {
		period = DefaultSyncPeriod
	}
	f := &follower{
		host:   host,
		period: period,
		staged: map[string]bool{},
		cdcs:   map[string]endpoint{},
		conf:   map[string]endpoint{},
		kick:   make(chan struct{}, 1),

		dropping:  map[string]*pathDrop{},
		dropSlots: make(chan struct{}, dropConcurrency),
		unstaging: map[string]chan struct{}{},
	}
	for _, ep := range conf {
		f.conf[ep.String()] = ep
		f.cdcs[ep.String()] = ep
	}
	return f
}

// recover reads the follower's state back from sysfs after a start: the
// subsystems of the driver's own NQN prefix are staged. Its discovery
// connections are those to the configured cdcs, which it knows from the
// start, so a cdc that cannot be read holds the drops off after a restart
// as before one. A discovery connection under the node's host NQN to
// another address is followed as well when its log lists a staged
// subsystem: an attach named that cdc before the configuration changed.
// Such a connection only adds a cdc that must answer; one whose log cannot
// be read stays another tool's.
func (f *follower) recover(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	for _, c := range ctrls {
		if strings.HasPrefix(c.subNqn, nqnPrefix) {
			f.staged[c.subNqn] = true
		}
	}
	for _, c := range ctrls {
		if c.subNqn != common.NvmeDiscoveryNqn || c.hostNqn != f.host.id.hostNqn {
			continue
		}
		if _, ok := f.cdcs[c.key()]; ok {
			continue
		}
		entries, err := f.host.discoveryLog(ctx, c.name)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if f.staged[e.subNqn] {
				f.cdcs[c.key()] = endpoint{trAddr: c.trAddr, trSvcId: c.trSvcId}
				break
			}
		}
	}
	return nil
}

// ownsLocked reports whether a controller is one of the follower's
// discovery connections: one under the node's host NQN, to a cdc the
// follower knows; one under another host NQN or to another address is
// another tool's.
func (f *follower) ownsLocked(c ctrl) bool {
	if c.subNqn != common.NvmeDiscoveryNqn || c.hostNqn != f.host.id.hostNqn {
		return false
	}
	_, ok := f.cdcs[c.key()]
	return ok
}

// stage adds a subsystem and its cdcs and syncs at once, so that the
// stage's wait for the device starts with the connects made. A stage of a
// subsystem an unstage is still dropping waits for that unstage first.
func (f *follower) stage(ctx context.Context, nqn string, cdcs []endpoint) error {
	f.mu.Lock()
	for {
		wait, ok := f.unstaging[nqn]
		if !ok {
			break
		}
		f.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	f.staged[nqn] = true
	for _, ep := range cdcs {
		f.cdcs[ep.String()] = ep
	}
	return f.syncLocked(ctx)
}

// unstage forgets a subsystem and drops every path of it; when nothing is
// staged any more, the discovery connections go too (csi.md CSI16). The
// drops run outside the lock, so another volume's stage does not wait for
// them, but the unstage waits for its own, the ones a sync began included,
// before it returns.
func (f *follower) unstage(ctx context.Context, nqn string) error {
	f.mu.Lock()
	for {
		wait, ok := f.unstaging[nqn]
		if !ok {
			break
		}
		f.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
	}
	delete(f.staged, nqn)
	done := make(chan struct{})
	f.unstaging[nqn] = done
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.unstaging, nqn)
		f.mu.Unlock()
		close(done)
	}()
	if err := f.waitDrops(ctx, nqn); err != nil {
		return err
	}
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	var held bool
	for _, c := range ctrls {
		if c.subNqn == nqn {
			held = true
			break
		}
	}
	if held {
		if err := f.host.disconnectSubsystem(ctx, nqn); err != nil {
			return err
		}
		slog.InfoContext(ctx, msgPathDropped, slog.String("nqn", nqn),
			slog.String("endpoint", "every path"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.staged) == 0 {
		return f.dropDiscoveryLocked(ctx)
	}
	return nil
}

// waitDrops waits for the path drops running outside the lock, those of one
// subsystem, or every one for an empty nqn, until none is left or ctx ends.
func (f *follower) waitDrops(ctx context.Context, nqn string) error {
	for {
		f.mu.Lock()
		var wait chan struct{}
		for _, d := range f.dropping {
			if nqn == "" || d.nqn == nqn {
				wait = d.done
				break
			}
		}
		f.mu.Unlock()
		if wait == nil {
			return nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// dropPathLocked starts the drop of one path outside the lock, unless one
// is running for that controller already. The drop outlives the call that
// began it, so its command runs on a ctx that call's end does not cancel.
func (f *follower) dropPathLocked(
	ctx context.Context,
	nqn string,
	key string,
	name string,
) {
	if _, ok := f.dropping[name]; ok {
		return
	}
	d := &pathDrop{nqn: nqn, done: make(chan struct{})}
	f.dropping[name] = d
	dropCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(d.done)
		f.dropSlots <- struct{}{}
		err := f.host.disconnectCtrl(dropCtx, name)
		<-f.dropSlots
		f.mu.Lock()
		delete(f.dropping, name)
		f.mu.Unlock()
		if err != nil {
			slog.WarnContext(dropCtx, msgFollowerSyncError,
				slog.String("nqn", nqn), slog.String("endpoint", key),
				slog.String("error", err.Error()))
			return
		}
		slog.InfoContext(dropCtx, msgPathDropped,
			slog.String("nqn", nqn), slog.String("endpoint", key))
	}()
}

// dropDiscoveryLocked disconnects the follower's own discovery connections
// and forgets the cdcs the attaches named; a discovery connection of
// another tool stays.
func (f *follower) dropDiscoveryLocked(ctx context.Context) error {
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	for _, c := range ctrls {
		if !f.ownsLocked(c) {
			continue
		}
		if err := f.host.disconnectCtrl(ctx, c.name); err != nil {
			return err
		}
		slog.InfoContext(ctx, msgDiscoveryDropped,
			slog.String("endpoint", c.key()))
	}
	f.cdcs = map[string]endpoint{}
	for key, ep := range f.conf {
		f.cdcs[key] = ep
	}
	return nil
}

// sync makes the node's paths match the cdcs' answer for every staged
// subsystem.
func (f *follower) sync(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncLocked(ctx)
}

// syncLocked is one pass: a discovery connection to every cdc, then a path
// to every listed cntlr, then no path to a cntlr no cdc lists. A cdc that
// cannot be reached or whose log cannot be read holds the drops off and
// leaves the connects to the others, since twins exist so that one can be
// down; and a pass that read no log at all drops nothing: an answer that is
// missing is not an answer that the cntlr is gone. A withdrawn path is
// dropped only once the node holds a path to a listed cntlr, or when the
// cdcs list none for the subsystem, which is what a detach leaves (csi.md
// CSI15): a cntlr that replaced the withdrawn one but refuses the connect
// does not cost the volume its last path. The drops run outside the lock
// (dropPathLocked); the pass does not wait for them.
func (f *follower) syncLocked(ctx context.Context) error {
	if len(f.staged) == 0 {
		return nil
	}
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	keep := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	ownDiscovery := func() map[string]string { // cdc endpoint -> controller name
		out := map[string]string{}
		for _, c := range ctrls {
			if f.ownsLocked(c) {
				out[c.key()] = c.name
			}
		}
		return out
	}
	discovery := ownDiscovery()
	opened := false
	for key, ep := range f.cdcs {
		if _, ok := discovery[key]; ok {
			continue
		}
		if err := f.host.connectDiscovery(ctx, ep); err != nil {
			slog.WarnContext(ctx, msgFollowerSyncError,
				slog.String("cdc", key), slog.String("error", err.Error()))
			keep(err)
			continue
		}
		slog.InfoContext(ctx, msgDiscoveryOpened, slog.String("endpoint", key))
		opened = true
	}
	if opened {
		if ctrls, err = f.host.controllers(ctx); err != nil {
			return err
		}
		discovery = ownDiscovery()
	}

	// The union of the cdcs' logs, for the staged subsystems only.
	listed := map[string]map[string]endpoint{}
	complete := true
	read := 0
	for key := range f.cdcs {
		name, ok := discovery[key]
		if !ok {
			complete = false
			continue
		}
		entries, err := f.host.discoveryLog(ctx, name)
		if err != nil {
			slog.WarnContext(ctx, msgFollowerSyncError,
				slog.String("cdc", key), slog.String("error", err.Error()))
			complete = false
			continue
		}
		read++
		for _, e := range entries {
			if !f.staged[e.subNqn] {
				continue
			}
			if listed[e.subNqn] == nil {
				listed[e.subNqn] = map[string]endpoint{}
			}
			listed[e.subNqn][e.ep.String()] = e.ep
		}
	}
	if read == 0 {
		complete = false
	}

	have := map[string]map[string]ctrl{} // nqn -> endpoint -> ctrl
	for _, c := range ctrls {
		if !f.staged[c.subNqn] {
			continue
		}
		if have[c.subNqn] == nil {
			have[c.subNqn] = map[string]ctrl{}
		}
		have[c.subNqn][c.key()] = c
	}
	for nqn := range f.staged {
		// A listed cntlr's path counts as held only once the kernel shows
		// the volume's namespace under it and it is not being dropped: a
		// connect returns before the scan that joins the path to the
		// multipath device, and a drop of the last joined path before then
		// would take the device the mount uses. A path connected in this
		// pass counts in the next one, which its own device events start.
		holdsListed := false
		for key, ep := range listed[nqn] {
			if c, ok := have[nqn][key]; ok {
				if _, going := f.dropping[c.name]; !going &&
					f.host.showsNamespace(ctx, c.name) {
					holdsListed = true
				}
				continue
			}
			if err := f.host.connect(ctx, nqn, ep); err != nil {
				// A cntlr the cdc lists before it has built the subsystem
				// refuses the connect; the next sync tries again, as does
				// the stage's own retry until the node holds a path.
				slog.WarnContext(ctx, msgFollowerSyncError,
					slog.String("nqn", nqn), slog.String("endpoint", key),
					slog.String("error", err.Error()))
				keep(err)
				continue
			}
			slog.InfoContext(ctx, msgPathConnected,
				slog.String("nqn", nqn), slog.String("endpoint", key))
		}
		if !complete || (len(listed[nqn]) > 0 && !holdsListed) {
			continue
		}
		for key, c := range have[nqn] {
			if _, ok := listed[nqn][key]; ok {
				continue
			}
			f.dropPathLocked(ctx, nqn, key, c.name)
		}
	}
	return firstErr
}

// hasPath reports whether the node holds a path to a subsystem, for a
// stage's retry of the connects a cntlr refused.
func (f *follower) hasPath(ctx context.Context, nqn string) (bool, error) {
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range ctrls {
		if c.subNqn == nqn {
			return true, nil
		}
	}
	return false, nil
}

// trigger asks the loop for a sync.
func (f *follower) trigger() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

// run is the follower's loop: a sync on every kick, every change
// notification and every period, until ctx ends; then, with nothing
// staged, the discovery connections are dropped, and with volumes staged
// every connection stays, since the data path must outlive the plugin.
func (f *follower) run(ctx context.Context) {
	events := make(chan struct{}, 1)
	go func() {
		if err := listenUevents(ctx, events); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, msgFollowerSyncError,
				slog.String("error", "the kernel's event socket: "+err.Error()),
			)
		}
	}()
	ticker := time.NewTicker(f.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
				10*time.Second)
			f.mu.Lock()
			if len(f.staged) == 0 {
				if err := f.dropDiscoveryLocked(stopCtx); err != nil {
					slog.WarnContext(stopCtx, msgFollowerSyncError,
						slog.String("error", err.Error()))
				}
			}
			f.mu.Unlock()
			cancel()
			return
		case <-f.kick:
		case <-events:
		case <-ticker.C:
		}
		if err := f.sync(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, msgFollowerSyncError,
				slog.String("error", err.Error()))
		}
	}
}
