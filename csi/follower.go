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
// no path to one the cdc has withdrawn, so a fenced old primary never
// answers the node's IO with errors. It keeps the set of staged subsystems
// and the cdc addresses their attaches named, holds a persistent discovery
// connection to each cdc, and on every trigger reads the discovery logs and
// connects or drops paths to match.
const (
	// DefaultSyncPeriod is the follower's timer, the safety net under the
	// kernel's change notification.
	DefaultSyncPeriod = 30 * time.Second

	msgPathConnected     = "csi path connected"
	msgPathDropped       = "csi path dropped"
	msgDiscoveryOpened   = "csi discovery connection opened"
	msgDiscoveryDropped  = "csi discovery connection dropped"
	msgFollowerSyncError = "csi follower sync error"
)

type follower struct {
	host   *nvmeHost
	period time.Duration

	// mu runs the follower's operations one at a time, a stage, an
	// unstage, a sync and the stop's drop, and guards the sets below: an
	// unstage that found nothing staged would otherwise forget the cdcs
	// while a stage of another volume ran, and the next sync, hearing from
	// no cdc, would drop that volume's paths.
	mu     sync.Mutex
	staged map[string]bool // subsystem NQN
	cdcs   map[string]endpoint
	// pending holds the discovery connections a restart found under the
	// node's host NQN whose log could not be read yet: each sync reads them
	// again, and none holds a drop off, since it may be another tool's.
	pending map[string]endpoint

	// kick asks the follower's loop for a sync; a kick that finds one
	// pending is folded into it.
	kick chan struct{}
}

func newFollower(host *nvmeHost, period time.Duration) *follower {
	if period <= 0 {
		period = DefaultSyncPeriod
	}
	return &follower{
		host:    host,
		period:  period,
		staged:  map[string]bool{},
		cdcs:    map[string]endpoint{},
		pending: map[string]endpoint{},
		kick:    make(chan struct{}, 1),
	}
}

// recover reads the follower's state back from sysfs after a start: the
// subsystems of the driver's own NQN prefix are staged, and a discovery
// connection under the node's host NQN whose log lists a subsystem of the
// driver names a cdc.
func (f *follower) recover(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	for _, c := range ctrls {
		switch {
		case f.isNodeDiscovery(c):
			f.pending[c.key()] = endpoint{trAddr: c.trAddr, trSvcId: c.trSvcId}
		case strings.HasPrefix(c.subNqn, nqnPrefix):
			f.staged[c.subNqn] = true
		}
	}
	f.adoptLocked(ctx, ctrls)
	return nil
}

// isNodeDiscovery reports whether a controller is a discovery connection
// under the node's own host NQN; one under another host NQN is never the
// follower's.
func (f *follower) isNodeDiscovery(c ctrl) bool {
	return c.subNqn == common.NvmeDiscoveryNqn && c.hostNqn == f.host.id.hostNqn
}

// ownsLocked reports whether a controller is one of the follower's
// discovery connections: the node's, to a cdc the follower knows.
func (f *follower) ownsLocked(c ctrl) bool {
	if !f.isNodeDiscovery(c) {
		return false
	}
	_, ok := f.cdcs[c.key()]
	return ok
}

// adoptLocked reads the log of every discovery connection waiting in
// pending: one whose log lists a subsystem of the driver names a cdc, one
// whose log lists none is left alone, as another tool's, and one whose log
// cannot be read waits for the next sync. A connection that is gone is
// forgotten.
func (f *follower) adoptLocked(ctx context.Context, ctrls []ctrl) {
	if len(f.pending) == 0 {
		return
	}
	names := map[string]string{} // endpoint -> controller name
	for _, c := range ctrls {
		if f.isNodeDiscovery(c) {
			names[c.key()] = c.name
		}
	}
	for key, ep := range f.pending {
		name, ok := names[key]
		if _, known := f.cdcs[key]; known || !ok {
			delete(f.pending, key)
			continue
		}
		entries, err := f.host.discoveryLog(ctx, name)
		if err != nil {
			continue
		}
		delete(f.pending, key)
		for _, e := range entries {
			if strings.HasPrefix(e.subNqn, nqnPrefix) {
				f.cdcs[key] = ep
				break
			}
		}
	}
}

// stage adds a subsystem and its cdcs and syncs at once, so that the
// stage's wait for the device starts with the connects made.
func (f *follower) stage(ctx context.Context, nqn string, cdcs []endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staged[nqn] = true
	for _, ep := range cdcs {
		f.cdcs[ep.String()] = ep
	}
	return f.syncLocked(ctx)
}

// unstage drops every path of a subsystem and forgets it; when nothing is
// staged any more, the discovery connections go too (csi.md CSI16).
func (f *follower) unstage(ctx context.Context, nqn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.staged, nqn)
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
	if len(f.staged) == 0 {
		return f.dropDiscoveryLocked(ctx)
	}
	return nil
}

// dropDiscoveryLocked disconnects the follower's own discovery connections
// and forgets the cdcs; a discovery connection of another tool stays.
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
	f.pending = map[string]endpoint{}
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
// missing is not an answer that the cntlr is gone.
func (f *follower) syncLocked(ctx context.Context) error {
	if len(f.staged) == 0 {
		return nil
	}
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	f.adoptLocked(ctx, ctrls)
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
		for key, ep := range listed[nqn] {
			if _, ok := have[nqn][key]; ok {
				continue
			}
			if err := f.host.connect(ctx, nqn, ep); err != nil {
				// A cntlr the cdc lists before its port listens refuses
				// the connect; the next sync, or the stage's own retry,
				// tries again.
				slog.WarnContext(ctx, msgFollowerSyncError,
					slog.String("nqn", nqn), slog.String("endpoint", key),
					slog.String("error", err.Error()))
				keep(err)
				continue
			}
			slog.InfoContext(ctx, msgPathConnected,
				slog.String("nqn", nqn), slog.String("endpoint", key))
		}
		if !complete {
			continue
		}
		for key, c := range have[nqn] {
			if _, ok := listed[nqn][key]; ok {
				continue
			}
			if err := f.host.disconnectCtrl(ctx, c.name); err != nil {
				slog.WarnContext(ctx, msgFollowerSyncError,
					slog.String("nqn", nqn), slog.String("endpoint", key),
					slog.String("error", err.Error()))
				keep(err)
				continue
			}
			slog.InfoContext(ctx, msgPathDropped,
				slog.String("nqn", nqn), slog.String("endpoint", key))
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
