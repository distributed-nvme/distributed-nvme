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

	mu     sync.Mutex
	staged map[string]bool // subsystem NQN
	cdcs   map[string]endpoint

	// kick asks the follower's loop for a sync; a kick that finds one
	// pending is folded into it.
	kick chan struct{}
}

func newFollower(host *nvmeHost, period time.Duration) *follower {
	if period <= 0 {
		period = DefaultSyncPeriod
	}
	return &follower{
		host:   host,
		period: period,
		staged: map[string]bool{},
		cdcs:   map[string]endpoint{},
		kick:   make(chan struct{}, 1),
	}
}

// recover reads the follower's state back from sysfs after a start: the
// subsystems of the driver's own NQN prefix are staged, and the persistent
// discovery connections name the cdcs.
func (f *follower) recover(ctx context.Context) error {
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range ctrls {
		switch {
		case c.subNqn == common.NvmeDiscoveryNqn:
			ep := endpoint{trAddr: c.trAddr, trSvcId: c.trSvcId}
			f.cdcs[ep.String()] = ep
		case strings.HasPrefix(c.subNqn, nqnPrefix):
			f.staged[c.subNqn] = true
		}
	}
	return nil
}

// stage adds a subsystem and its cdcs and syncs at once, so that the
// stage's wait for the device starts with the connects made.
func (f *follower) stage(ctx context.Context, nqn string, cdcs []endpoint) error {
	f.mu.Lock()
	f.staged[nqn] = true
	for _, ep := range cdcs {
		f.cdcs[ep.String()] = ep
	}
	f.mu.Unlock()
	return f.sync(ctx)
}

// unstage drops every path of a subsystem and forgets it; when nothing is
// staged any more, the discovery connections go too (csi.md CSI16).
func (f *follower) unstage(ctx context.Context, nqn string) error {
	f.mu.Lock()
	delete(f.staged, nqn)
	empty := len(f.staged) == 0
	f.mu.Unlock()
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
	if empty {
		return f.dropDiscovery(ctx)
	}
	return nil
}

// dropDiscovery disconnects the persistent discovery connections and
// forgets the cdcs.
func (f *follower) dropDiscovery(ctx context.Context) error {
	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	for _, c := range ctrls {
		if c.subNqn != common.NvmeDiscoveryNqn {
			continue
		}
		if err := f.host.disconnectCtrl(ctx, c.name); err != nil {
			return err
		}
		slog.InfoContext(ctx, msgDiscoveryDropped,
			slog.String("endpoint", c.key()))
	}
	f.mu.Lock()
	f.cdcs = map[string]endpoint{}
	f.mu.Unlock()
	return nil
}

// sync makes the node's paths match the cdcs' answer for every staged
// subsystem: a discovery connection to every cdc, then a path to every
// listed cntlr, then no path to a cntlr no cdc lists. A cdc whose log
// cannot be read holds the drops off: an answer that is missing is not an
// answer that the cntlr is gone.
func (f *follower) sync(ctx context.Context) error {
	f.mu.Lock()
	staged := make(map[string]bool, len(f.staged))
	for nqn := range f.staged {
		staged[nqn] = true
	}
	cdcs := make([]endpoint, 0, len(f.cdcs))
	for _, ep := range f.cdcs {
		cdcs = append(cdcs, ep)
	}
	f.mu.Unlock()
	if len(staged) == 0 {
		return nil
	}

	ctrls, err := f.host.controllers(ctx)
	if err != nil {
		return err
	}
	discovery := map[string]string{} // cdc endpoint -> controller name
	for _, c := range ctrls {
		if c.subNqn == common.NvmeDiscoveryNqn {
			discovery[c.key()] = c.name
		}
	}
	opened := false
	for _, ep := range cdcs {
		if _, ok := discovery[ep.String()]; ok {
			continue
		}
		if err := f.host.connectDiscovery(ctx, ep); err != nil {
			return err
		}
		slog.InfoContext(ctx, msgDiscoveryOpened, slog.String("endpoint", ep.String()))
		opened = true
	}
	if opened {
		if ctrls, err = f.host.controllers(ctx); err != nil {
			return err
		}
		for _, c := range ctrls {
			if c.subNqn == common.NvmeDiscoveryNqn {
				discovery[c.key()] = c.name
			}
		}
	}

	// The union of the cdcs' logs, for the staged subsystems only.
	listed := map[string]map[string]endpoint{}
	complete := true
	for _, ep := range cdcs {
		name, ok := discovery[ep.String()]
		if !ok {
			complete = false
			continue
		}
		entries, err := f.host.discoveryLog(ctx, name)
		if err != nil {
			slog.WarnContext(ctx, msgFollowerSyncError,
				slog.String("cdc", ep.String()), slog.String("error", err.Error()))
			complete = false
			continue
		}
		for _, e := range entries {
			if !staged[e.subNqn] {
				continue
			}
			if listed[e.subNqn] == nil {
				listed[e.subNqn] = map[string]endpoint{}
			}
			listed[e.subNqn][e.ep.String()] = e.ep
		}
	}

	have := map[string]map[string]ctrl{} // nqn -> endpoint -> ctrl
	for _, c := range ctrls {
		if !staged[c.subNqn] {
			continue
		}
		if have[c.subNqn] == nil {
			have[c.subNqn] = map[string]ctrl{}
		}
		have[c.subNqn][c.key()] = c
	}
	var firstErr error
	for nqn := range staged {
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
				if firstErr == nil {
					firstErr = err
				}
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
				if firstErr == nil {
					firstErr = err
				}
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
			f.mu.Lock()
			empty := len(f.staged) == 0
			f.mu.Unlock()
			if empty {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
					10*time.Second)
				if err := f.dropDiscovery(stopCtx); err != nil {
					slog.WarnContext(stopCtx, msgFollowerSyncError,
						slog.String("error", err.Error()))
				}
				cancel()
			}
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
