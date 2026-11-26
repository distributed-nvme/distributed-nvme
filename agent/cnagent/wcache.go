package cnagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// The write cache a host or this node reads (cnagent.md CN10, CN15, CN16)
//
// A host reads whether a path has a volatile write cache only when it scans
// the namespace: at its connect, at a reconnect and at a rescan, never at an
// ANA change. A path it read while the namespace's device had no write cache
// keeps that reading, and the host's block layer then completes every flush
// on it at once and sends nothing. This node reads its leg paths the same
// way. Three rules follow, one for each place such a reading is made:
//
//   - The td's dm-error, which an ns-dev maps while it serves no data,
//     reports a write cache (ensureTdError): its table names a loop device
//     of this agent's in its last target (wcacheDevNo), so a host reads a
//     standby's path, or a parked one, as it will read it once it serves.
//   - A one-target device whose write cache reads "write through" over a
//     device that reads "write back" is loaded again (wcacheStale): a
//     device-mapper device takes the setting of the device under it only
//     when its table is loaded.
//   - A primary's leg path that reads "write through" while its disk node's
//     NVMe target reports a write cache is scanned again (freshenLegPaths),
//     and a read names it until it reads what the target reports
//     (legWcacheVerdict).
// ---------------------------------------------------------------------------

// wcacheLoop is the agent's write cache loop device (CN15), a 4 KiB file in
// common.CnWcacheDir under a loop device. Its mutex is held across the OS
// calls of a find or a make, so two converges never make two; it is no leaf
// lock, and nothing takes the server's mu under it.
type wcacheLoop struct {
	mu sync.Mutex
	// devNo is the loop device's "major:minor" as last found or made, ""
	// before that; a find checks it again before it is used.
	devNo string
	// failedAt is when the last make failed: no make starts again until
	// common.CnWcacheRetry has passed.
	failedAt time.Time
}

// wcacheDevNo answers the number of this agent's write cache loop device,
// "" when it has none. The number last found is checked again by its reads
// (loopHolds), and kept when a read does not answer: a loop device is not
// given up on a read that says neither. A converge passes make, and then a
// loop device over the file is looked for with `losetup -j`, which finds
// the one an earlier process of the agent made, and, with none, one is
// made: the file is written where it is not there whole, and `losetup` puts
// a loop device over it, read-write, since a read-only one has no write
// cache. A look or a make that fails is logged at Warn and not tried again
// before common.CnWcacheRetry; a read never looks or makes.
func (s *CnAgentServer) wcacheDevNo(ctx context.Context, make bool) string {
	s.wcache.mu.Lock()
	defer s.wcache.mu.Unlock()
	file := s.nf.CnWcacheFile()
	if s.wcache.devNo != "" {
		holds, err := s.loopHolds(ctx, s.wcache.devNo, file)
		if err != nil || holds {
			return s.wcache.devNo
		}
		s.wcache.devNo = ""
	}
	if !make || (!s.wcache.failedAt.IsZero() &&
		s.now().Sub(s.wcache.failedAt) < common.CnWcacheRetry) {
		return ""
	}
	devNo, err := s.findWcacheLoop(ctx, file)
	if err == nil && devNo == "" {
		devNo, err = s.makeWcacheLoop(ctx, file)
	}
	if err != nil {
		s.wcache.failedAt = s.now()
		slog.WarnContext(ctx, msgWcacheLoopFailed,
			slog.String("file", file),
			slog.String("error", err.Error()))
		return ""
	}
	s.wcache.failedAt = time.Time{}
	s.wcache.devNo = devNo
	return devNo
}

// wcacheKnown is the number of the write cache loop device the agent last
// found or made, "" for none, with nothing read: what a read judges a
// one-target dm-error by (probeDmError).
func (s *CnAgentServer) wcacheKnown() string {
	s.wcache.mu.Lock()
	defer s.wcache.mu.Unlock()
	return s.wcache.devNo
}

// msgWcacheLoopFailed is the Warn record of a write cache loop device that
// could not be found or made: the td dm-errors this agent builds until one
// is made report no write cache, so a host that connects to a standby or a
// parked namespace meanwhile reads its path as having none (CN15).
const msgWcacheLoopFailed = "the write cache loop device could not be made"

// loopHolds says whether the block device devNo is a loop device over file,
// of common.DmErrorWcSectors sectors or more, whose write cache reads
// "write back". A read that does not answer is its error, which says
// neither.
func (s *CnAgentServer) loopHolds(
	ctx context.Context,
	devNo string,
	file string,
) (bool, error) {
	dir := "/sys/dev/block/" + devNo
	back, ok, err := s.cmd.ReadAttr(ctx, dir+"/loop/backing_file")
	if err != nil || !ok || strings.TrimSpace(back) != file {
		return false, err
	}
	size, ok, err := s.cmd.ReadAttr(ctx, dir+"/size")
	if err != nil || !ok {
		return false, err
	}
	if n, err := strconv.ParseUint(strings.TrimSpace(size), 10, 64); err != nil ||
		n < common.DmErrorWcSectors {
		return false, nil
	}
	wc, ok, err := s.cmd.ReadAttr(ctx, dir+"/queue/write_cache")
	if err != nil {
		return false, err
	}
	return ok && wc == writeBack, nil
}

// findWcacheLoop looks for a loop device over file that loopHolds, "" for
// none: a file that is not there has none, and is not asked about. A read
// that does not answer is an error, so no second loop device is made beside
// one that is there.
func (s *CnAgentServer) findWcacheLoop(
	ctx context.Context,
	file string,
) (string, error) {
	if _, err := s.oc.ReadFile(ctx, file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	out, stderr, _, err := s.cmd.Run(ctx, "losetup", "-j", file,
		"--noheadings", "--output", "MAJ:MIN")
	if err != nil {
		return "", fmt.Errorf("losetup -j %s: %w: %s", file, err,
			strings.TrimSpace(stderr))
	}
	for _, line := range strings.Split(out, "\n") {
		devNo := strings.TrimSpace(line)
		if !blockDevNoPattern.MatchString(devNo) {
			continue
		}
		holds, err := s.loopHolds(ctx, devNo, file)
		if err != nil {
			return "", err
		}
		if holds {
			return devNo, nil
		}
	}
	return "", nil
}

// makeWcacheLoop writes file, of common.CnWcacheFileBytes zeros, where it is
// not there whole, and puts a new loop device over it. A loop device it put
// there that loopHolds does not take is detached again.
func (s *CnAgentServer) makeWcacheLoop(
	ctx context.Context,
	file string,
) (string, error) {
	if err := s.cmd.RunOk(ctx, "mkdir", "-p", path.Dir(file)); err != nil {
		return "", err
	}
	data, err := s.oc.ReadFile(ctx, file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err != nil || len(data) != common.CnWcacheFileBytes {
		if err := s.oc.WriteFile(ctx, file,
			string(make([]byte, common.CnWcacheFileBytes))); err != nil {
			return "", err
		}
	}
	out, stderr, _, err := s.cmd.Run(ctx, "losetup", "--find", "--show", file)
	if err != nil {
		return "", fmt.Errorf("losetup --find --show %s: %w: %s", file, err,
			strings.TrimSpace(stderr))
	}
	dev := strings.TrimSpace(out)
	devNo, err := s.oc.ReadDevNo(ctx, dev)
	holds := false
	if err == nil {
		holds, err = s.loopHolds(ctx, devNo, file)
	}
	if err != nil || !holds {
		if detachErr := s.cmd.RunOk(ctx, "losetup", "-d", dev); detachErr != nil {
			slog.WarnContext(ctx, "detaching a write cache loop device "+
				"failed",
				slog.String("dev", dev),
				slog.String("error", detachErr.Error()))
		}
		if err == nil {
			err = fmt.Errorf("loop device %s (%s) is not over %s with a "+
				"write cache", dev, devNo, file)
		}
		return "", err
	}
	return devNo, nil
}

// The two values of a block device's queue/write_cache.
const (
	writeBack    = "write back"
	writeThrough = "write through"
)

// writeCacheOf reads the write cache setting of the block device devNo, ""
// for a read that did not answer or found nothing.
func (s *CnAgentServer) writeCacheOf(ctx context.Context, devNo string) string {
	got, ok, err := s.cmd.ReadAttr(ctx,
		"/sys/dev/block/"+devNo+"/queue/write_cache")
	if err != nil || !ok {
		return ""
	}
	return got
}

// wcacheStale says whether the device-mapper device name, one target over
// the device backingNo, reads "write through" while backingNo reads "write
// back": loaded before backingNo had a write cache, it sends no flush on,
// and a load of the same table again takes the setting anew. A read that
// does not answer says no.
func (s *CnAgentServer) wcacheStale(
	ctx context.Context,
	name string,
	backingNo string,
) bool {
	if s.writeCacheOf(ctx, backingNo) != writeBack {
		return false
	}
	ownNo, err := s.oc.ReadDevNo(ctx, s.nf.DmPath(name))
	if err != nil {
		return false
	}
	return s.writeCacheOf(ctx, ownNo) == writeThrough
}

// ---------------------------------------------------------------------------
// A primary's leg paths (CN10, The write cache of a leg's path)
// ---------------------------------------------------------------------------

// stalePaths are lp's controllers in view, on a primary, whose path the
// host scanned while its disk node served it from a device with no write
// cache: live, optimized, reading "write through", while the controller's
// target reports a write cache for the side's namespace now
// (NvmeHost.NsWriteCache). A path that reads "write back" is never asked:
// it sends flushes, and a target with no write cache completes them at once.
// An answer that did not come is logged and counts the path as not stale:
// nothing is known against it.
func (s *CnAgentServer) stalePaths(
	ctx context.Context,
	lp *legPlan,
	view *subsysView,
) []ctrlView {
	var stale []ctrlView
	for _, ctrl := range legCtrls(lp, view) {
		if ctrl.state != "live" || ctrl.anaState != "optimized" ||
			ctrl.wcache != writeThrough {
			continue
		}
		cache, err := s.host.NsWriteCache(ctx, ctrl.name, sideNsid)
		if err != nil {
			slog.InfoContext(ctx, "the write cache a leg path's target "+
				"reports is unread",
				slog.String("nqn", lp.nqn),
				slog.String("ctrl", ctrl.name),
				slog.String("error", err.Error()))
			continue
		}
		if cache {
			stale = append(stale, ctrl)
		}
	}
	return stale
}

// freshenLegPaths is a primary's converge step on lp's paths (CN10): view is
// the leg's lookup, which on a primary reads its paths' write cache
// (lookupLegWcache), and each stale path (stalePaths) is scanned again,
// which makes the kernel read what the target reports. It then looks the
// leg up again, after up to
// common.CnRescanPauses common.CnRescanPause steps drawn from the pass's
// wait budget, until each path whose rescan was written and the leg's
// multipath head read "write back": the kernel's scan of one namespace ends
// well inside a few steps, and the cap keeps one leg whose rescan does not
// take from spending the budget the pass's other legs need. A rescan whose
// write failed is logged and not waited for. A view read with no write
// cache, as the head wait after a connect reads it, has no stale path: a
// path connected in the pass read what its target reports. It returns the
// leg's view as last read and the controllers that were stale.
func (s *CnAgentServer) freshenLegPaths(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
	budget *agent.WaitBudget,
) (*subsysView, []ctrlView) {
	stale := s.stalePaths(ctx, lp, view)
	if len(stale) == 0 {
		return view, nil
	}
	var rescanned []ctrlView
	for _, ctrl := range stale {
		if err := s.host.Rescan(ctx, ctrl.name); err != nil {
			slog.InfoContext(ctx, "rescanning a leg path failed",
				slog.String("nqn", lp.nqn),
				slog.String("ctrl", ctrl.name),
				slog.String("error", err.Error()))
			continue
		}
		rescanned = append(rescanned, ctrl)
	}
	slog.InfoContext(ctx, msgLegPathsRescanned,
		slog.String("nqn", lp.nqn),
		slog.Int("paths", len(stale)),
		slog.Int("rescanned", len(rescanned)))
	for pauses := 0; len(rescanned) > 0; pauses++ {
		next, err := s.lookupLegWcache(ctx, plan.walk, lp.nqn)
		if err != nil {
			break
		}
		view = next
		if pathsFresh(view, rescanned) &&
			s.headWriteCache(ctx, view) == writeBack {
			break
		}
		if pauses >= common.CnRescanPauses ||
			!budget.Pause(ctx, common.CnRescanPause) {
			break
		}
	}
	return view, stale
}

// msgLegPathsRescanned is the Info record of a primary's rescan of a leg's
// stale paths (freshenLegPaths): its subsystem and how many paths.
const msgLegPathsRescanned = "leg paths rescanned for their write cache"

// pathsFresh says whether every controller of stale reads "write back" in
// view, found again by name.
func pathsFresh(view *subsysView, stale []ctrlView) bool {
	for _, was := range stale {
		found := false
		for _, ctrl := range view.ctrls {
			if ctrl.name == was.name {
				found = true
				if ctrl.wcache != writeBack {
					return false
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// headWriteCache reads the write cache of the leg's multipath head in view,
// "" when there is none or the read did not answer.
func (s *CnAgentServer) headWriteCache(
	ctx context.Context,
	view *subsysView,
) string {
	if view == nil || view.nsDev == "" {
		return ""
	}
	got, ok, err := s.cmd.ReadAttr(ctx,
		sysfsBlockDir+"/"+path.Base(view.nsDev)+"/queue/write_cache")
	if err != nil || !ok {
		return ""
	}
	return got
}

// legWcacheDetails is what a leg's row and its name say while its paths or
// its wrapper keep a stale write cache (CN10): the stale paths by
// controller, and the wrapper when it is stale. "" when neither is.
func legWcacheDetails(stale []ctrlView, wrapperStale bool) string {
	var parts []string
	for _, ctrl := range stale {
		parts = append(parts, fmt.Sprintf(
			"path %s to %s:%s reads write through, and its disk node "+
				"reports a write cache", ctrl.name, ctrl.trAddr, ctrl.trSvcId))
	}
	if wrapperStale {
		parts = append(parts,
			"the leg's device reads write through over a head that reads "+
				"write back")
	}
	return strings.Join(parts, "; ")
}

// wcMark is a leg's write cache mark (cntlrState.wcStale): stale is what
// is stale of the leg, its paths or its wrapper (legWcacheDetails), "" for a
// leg that is only watched, one with a live path that is not optimized.
type wcMark struct {
	stale string
}

// markLegWcache sets lp's mark from details, what is stale of it, and view,
// its lookup: stale when details is not "", watched while a live path of the
// leg in view is not optimized, and dropped otherwise. Reason for the watch:
// a disk node can open such a path over another device with no converge of
// this cntlr, as a migration's new side does at its cutover, whose dm-clone
// reports a write cache whatever the disk's, so a read must look at it once
// it is open.
func markLegWcache(
	st *cntlrState,
	lp *legPlan,
	view *subsysView,
	details string,
) {
	if st.wcStale == nil {
		st.wcStale = make(map[uint64]wcMark)
	}
	switch {
	case details != "":
		st.wcStale[lp.legId] = wcMark{stale: details}
	case pathNotOpen(lp, view):
		st.wcStale[lp.legId] = wcMark{}
	default:
		delete(st.wcStale, lp.legId)
	}
}

// pathNotOpen says whether a live path of lp in view is not optimized.
func pathNotOpen(lp *legPlan, view *subsysView) bool {
	for _, ctrl := range legCtrls(lp, view) {
		if ctrl.state == "live" && ctrl.anaState != "optimized" {
			return true
		}
	}
	return false
}

// legWcacheVerdict is a primary's read of a leg with a write cache mark
// (cntlrState.wcStale): the leg is looked up again with its paths' write
// cache, its paths' targets are asked (stalePaths), its wrapper is read
// (wcacheStale), and the mark is set again (markLegWcache). It returns that
// lookup, or view where it took none, and what is stale, "" for nothing,
// which the read names the leg's wrapper with. A leg with no mark, a
// standby's, and a lookup that did not answer give "" and change nothing:
// the converge, which stops at that lookup, would act on nothing. A read
// asks no target of a leg with no mark: a path a disk node opens over
// another device with no converge of this cntlr, as a migration's new side
// does at its cutover, is one that is not optimized first, which the watch
// mark covers (markLegWcache).
func (s *CnAgentServer) legWcacheVerdict(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
) (*subsysView, string) {
	if !plan.primary {
		return view, ""
	}
	if _, marked := st.wcStale[lp.legId]; !marked {
		return view, ""
	}
	wview, err := s.lookupLegWcache(ctx, plan.walk, lp.nqn)
	if err != nil {
		return view, ""
	}
	wrapperStale := false
	if wview.nsDev != "" {
		if headNo, err := s.blockDevNo(ctx, path.Base(wview.nsDev)); err == nil {
			wrapperStale = s.wcacheStale(ctx, lp.name, headNo)
		}
	}
	details := legWcacheDetails(s.stalePaths(ctx, lp, wview), wrapperStale)
	markLegWcache(st, lp, wview, details)
	return wview, details
}
