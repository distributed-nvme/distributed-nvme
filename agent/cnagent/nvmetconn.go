package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN33 — the cut of a removed host. nvmet checks a subsystem's allowed hosts
// only when a host connects, so a host the desired state takes off the list
// keeps its nvmet connection until the controller node ends it. A converge
// steps each subsystem's list (EnsureSubsystem) and, in cut mode
// (CnAgentServer.strayCut), ends its stray connections first, before any
// device step that could serve its namespaces (stepSubsysList), on every
// role, and a read reports both; in no cut mode neither reads the debug
// directory. A subsystem that is not safe (listStep.safe) has every
// namespace moved to inaccessible at once, and kept there to the pass's end
// (darkenSubsys).

// cutResult is what a pass knows of one subsystem's stray connections: held
// when its last listing of the subsystem's debug directory answered and
// showed none, and otherwise the subsystem row's text that says why not.
// failed says the connections could not be read, or a lockdown the lockdown
// file shows keeps a stray from being ended, which the row reports as
// RES_STATUS_ERROR; a stray still listed otherwise, after the wait, a
// refused write or a failed "state" read, or in a read's one listing
// (readStrays), is RES_STATUS_PENDING.
type cutResult struct {
	held   bool
	failed bool
	text   string
}

// cutHeld is the cutResult of a subsystem with no stray connection, or of a
// pass in no cut mode, which reads none.
var cutHeld = cutResult{held: true}

// detailsConnsUnread starts a subsystem row's text while its nvmet
// connections could not be read — a listing, or a read of a connection's
// host, that failed, or a subsystem with no debug directory — and
// detailsConnsNotEnded while a stray is listed under a lockdown the
// lockdown file shows, which refuses every write that ends one; in cut mode
// each is a cut that did not hold, and the row reads RES_STATUS_ERROR,
// which a converge and a read give alike (CN33).
const (
	detailsConnsUnread   = "nvmet connections not read: "
	detailsConnsNotEnded = "nvmet connections not ended: "
)

// cutErrorRow reports whether row is a subsystem row the cut leaves in
// error: its nvmet connections could not be read, or a lockdown the
// lockdown file shows keeps a stray from being ended (CN33), a name only a
// read gives (cntlrRowVerdict).
func cutErrorRow(row *pb.ResInfo) bool {
	return row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR &&
		(strings.HasPrefix(row.GetDetails(), detailsConnsUnread) ||
			strings.HasPrefix(row.GetDetails(), detailsConnsNotEnded))
}

// connsUnread is the cutResult of nvmet connections that could not be read.
func connsUnread(cause string) cutResult {
	return cutResult{failed: true, text: detailsConnsUnread + cause}
}

// strayConns is the connections of conns whose host is not on allowed, the
// subsystem's desired list.
func strayConns(conns []agent.NvmetConn, allowed []string) []agent.NvmetConn {
	var out []agent.NvmetConn
	for _, conn := range conns {
		if !slices.Contains(allowed, conn.HostNqn) {
			out = append(out, conn)
		}
	}
	return out
}

// strayText is a subsystem row's text while strays are connected: the first
// one's host, and how many more there are.
func strayText(strays []agent.NvmetConn) string {
	text := "host " + strays[0].HostNqn + " still connected"
	if more := len(strays) - 1; more > 0 {
		text += fmt.Sprintf(", and %d more", more)
	}
	return text
}

// listStrays lists the subsystem conf names in its debug directory and
// returns its stray connections, or the cutResult that did not hold because
// the listing did not answer or found no debug directory.
func (s *CnAgentServer) listStrays(
	ctx context.Context,
	conf agent.SubsysConf,
) ([]agent.NvmetConn, *cutResult) {
	conns, ok, err := s.nvmet.ListNvmetConns(ctx, conf.Nqn)
	switch {
	case err != nil:
		unread := connsUnread(err.Error())
		return nil, &unread
	case !ok:
		unread := connsUnread("no " + s.nvmet.SubsysDebugPath(conf.Nqn))
		return nil, &unread
	}
	return strayConns(conns, conf.AllowedHosts), nil
}

// strayResult is the cutResult of a subsystem whose last listing still shows
// strays, as a converge and a read decide it alike (CN33): failed, "nvmet
// connections not ended: lockdown <mode>", while the lockdown file shows a
// mode in force, which refuses every write that ends a connection, and
// otherwise pending, the strays' text, whatever ended the cut: its wait
// spent, a write the kernel refused or a read of a connection's "state"
// that failed.
// A read of the lockdown file that does not answer is read as no lockdown,
// and the row is pending.
func (s *CnAgentServer) strayResult(
	ctx context.Context,
	strays []agent.NvmetConn,
) cutResult {
	mode, err := s.nvmet.LockdownMode(ctx)
	if err == nil && mode != "" && mode != "none" {
		return cutResult{failed: true,
			text: detailsConnsNotEnded + "lockdown " + mode}
	}
	return cutResult{text: strayText(strays)}
}

// readStrays is a read's view of one subsystem's stray connections (CN33):
// in cut mode one listing of its debug directory, which writes nothing; in
// no cut mode nothing is read, and it holds.
func (s *CnAgentServer) readStrays(
	ctx context.Context,
	conf agent.SubsysConf,
) cutResult {
	if !s.strayCut {
		return cutHeld
	}
	strays, unread := s.listStrays(ctx, conf)
	if unread != nil {
		return *unread
	}
	if len(strays) > 0 {
		return s.strayResult(ctx, strays)
	}
	return cutHeld
}

// cutStrays is the cut of one subsystem in a converge pass (CN33), after
// its list step left the kernel admitting no host off the desired list
// (stepSubsysList), so a host it ends is refused when it connects again. In
// cut mode it lists the
// subsystem's debug directory, writes "fatal" to each stray connection, and
// lists again, pausing between listings on the pass's budget, until no
// stray is left or the budget is spent. Each listing judges a connection by
// the host its directory names alone, so a directory a new connection of an
// allowed host took over is no stray. The pass writes each name and host
// once while listings keep showing it: a pair a listing no longer shows is
// forgotten, so one that went and came back is written again, and a pair
// still shown is written a second time only when its "state" reads neither
// "fatal" nor nothing. A written connection reads "fatal" until it is
// freed, unless its host enables it after the write, which makes it read
// "ready", and then the second write only repeats the end; a new connection
// of the same stray host under the freed name reads "ready" once its host
// enabled it, and nothing before, which waits for a later listing or the
// next pass and only delays its end.
// Each write reads the connection's host again first (Nvmet.EndNvmetConn),
// so it goes by name only across that read and the write (cnagent.md,
// Known limits). A listing, or a read of a connection's host before a
// write, that fails ends the cut not held as "nvmet connections not read",
// an ERROR row, and so does no debug directory. A write the kernel refuses,
// logged, a read of a written connection's "state" that fails, and a wait
// that the pass's budget ends, end it with the strays still listed
// (strayResult): an ERROR row under a lockdown the lockdown file shows,
// pending otherwise,
// as a read of the same node gives. A
// file the kernel removed after its open is a connection that has gone. It
// keeps nothing between passes: the next converge lists and cuts again. In
// no cut mode it reads nothing, and it holds.
func (s *CnAgentServer) cutStrays(
	ctx context.Context,
	budget *agent.WaitBudget,
	conf agent.SubsysConf,
) cutResult {
	if !s.strayCut {
		return cutHeld
	}
	written := map[agent.NvmetConn]bool{}
	for {
		strays, unread := s.listStrays(ctx, conf)
		if unread != nil {
			return *unread
		}
		if len(strays) == 0 {
			return cutHeld
		}
		for conn := range written {
			if !slices.Contains(strays, conn) {
				delete(written, conn)
			}
		}
		for _, conn := range strays {
			if written[conn] {
				state, there, err := s.nvmet.NvmetConnState(
					ctx, conf.Nqn, conn.Name)
				if err != nil {
					return s.strayResult(ctx, strays)
				}
				if !there || state == "" ||
					strings.Contains(state, agent.NvmetConnEndWord) {
					continue
				}
			}
			wrote, err := s.nvmet.EndNvmetConn(ctx, conf.Nqn, conn)
			var readErr *agent.NvmetConnReadError
			switch {
			case errors.As(err, &readErr):
				return connsUnread(err.Error())
			case err != nil:
				slog.ErrorContext(ctx, msgStrayEndFailed,
					slog.String("nqn", conf.Nqn),
					slog.String("connection", conn.Name),
					slog.String("host_nqn", conn.HostNqn),
					slog.String("error", err.Error()))
				return s.strayResult(ctx, strays)
			}
			written[conn] = true
			if wrote {
				slog.InfoContext(ctx, msgStrayEnded,
					slog.String("nqn", conf.Nqn),
					slog.String("connection", conn.Name),
					slog.String("host_nqn", conn.HostNqn))
			}
		}
		if !budget.Pause(ctx, common.CnStrayCutPause) {
			slog.InfoContext(ctx, msgStrayWaitSpent,
				slog.String("nqn", conf.Nqn),
				slog.String("host_nqn", strays[0].HostNqn),
				slog.Int("strays", len(strays)))
			return s.strayResult(ctx, strays)
		}
	}
}

// The cut's log records (`log.md`, The cut of a removed host): a write of
// "fatal" the kernel took, a write the kernel refused, the only record of
// the cut that names the refusal, and a cut whose wait ended with a stray
// still listed, the pass's budget spent or the converge's ctx ended
// (WaitBudget.Pause), at Info since the next converge cuts again (log.md
// R6).
const (
	msgStrayEnded     = "stray connection ended"
	msgStrayEndFailed = "ending a stray connection failed"
	msgStrayWaitSpent = "stray connection wait spent"
)

// newStrayCutBudget is one converge pass's budget for the waits of its cuts
// (CN33), shared by every subsystem the pass steps and never kept across
// passes.
func (s *CnAgentServer) newStrayCutBudget() *agent.WaitBudget {
	return agent.NewWaitBudget(common.CnStrayCutPassBudget, s.now, s.sleep)
}

// listStep is what a converge's list step of one subsystem did (CN33): its
// EnsureSubsystem's error, the cut, and whether the subsystem is safe in the
// pass — the kernel admits no host off the desired list, by the step that
// held or, after one that failed, by a strict read (Nvmet.AdmitsOnly), and,
// in cut mode, the cut held. A read that fails is not safe.
type listStep struct {
	err  error
	cut  cutResult
	safe bool
}

// stepSubsysList is a converge's list step of one subsystem, the pool's or a
// transfer's, before the ns-dev loop and before a transfer's device step
// (CN9, CN33): EnsureSubsystem, then the cut, which runs only once the kernel
// admits no host off the desired list, since a host it ends while the list
// still admits it connects again.
func (s *CnAgentServer) stepSubsysList(
	ctx context.Context,
	budget *agent.WaitBudget,
	conf agent.SubsysConf,
) listStep {
	oldStays, err := s.ensureSubsysAnew(ctx, conf)
	step := listStep{err: err}
	if oldStays {
		// An old kernel subsystem that could not go is not safe, whatever
		// hosts it admits: its namespaces are not the ones the pass builds.
		return step
	}
	if step.err != nil && !s.admitsOnlyDesired(ctx, conf) {
		return step
	}
	step.cut = s.cutStrays(ctx, budget, conf)
	step.safe = step.cut.held
	return step
}

// ensureSubsysAnew is the list step's EnsureSubsystem, which builds the
// subsystem anew when nvmet refuses its serial over another live one
// (agent.SerialFrozenError; CN16, A subsystem made again): the kernel
// subsystem is then an older one of the NQN, made by a subsystem record
// deleted since, which a host has identified. removeOldSubsys moves its
// namespaces inaccessible and removes it, and EnsureSubsystem builds it
// again in the same pass; the namespaces and the port link of the new
// subsystem are built by the pass as usual. A removal that fails is the
// step's error, and oldStays says the old subsystem stays, which makes the
// subsystem not safe in the pass. Reason: nvmet takes no new serial on a
// subsystem a host has identified, so without the removal the subsystem
// would never be built again; and an old namespace left under it may stand
// over a device the sweep parked, which no ANA move may make serve.
func (s *CnAgentServer) ensureSubsysAnew(
	ctx context.Context,
	conf agent.SubsysConf,
) (oldStays bool, err error) {
	err = s.nvmet.EnsureSubsystem(ctx, conf)
	var frozen *agent.SerialFrozenError
	if !errors.As(err, &frozen) {
		return false, err
	}
	nsIdxs, _, lerr := s.nvmet.ListNamespaces(ctx, conf.Nqn)
	if lerr != nil {
		return true, fmt.Errorf("%v, and listing its namespaces failed: %w",
			err, lerr)
	}
	if rerr := s.removeOldSubsys(ctx, conf.Nqn, err.Error(),
		nsIdxs); rerr != nil {
		return true, rerr
	}
	return false, s.nvmet.EnsureSubsystem(ctx, conf)
}

// removeOldSubsys removes a kernel subsystem the list step cannot make this
// cntlr's (CN16): one over another pool's ns-dev (replaceForeignSubsys) or
// one whose serial nvmet no longer takes (ensureSubsysAnew). Every
// namespace of nsIdxs, the ones the kernel lists, moves inaccessible and
// reads back so (moveDark), then the subsystem goes (Nvmet.RemoveSubsystem:
// port link, namespaces, host links, the directory), which ends its hosts'
// connections. A Warn record `nvmet subsystem replaced` names the NQN and
// the cause (log.md, A subsystem replaced). A namespace that does not read
// back inaccessible, one over a dm device that reads suspended or does not
// read (disableSafe), or a removal that fails, is the error, and the
// subsystem stays. Reason: the move first makes a host queue rather than
// read errors; the removal's disable of a namespace waits, unkillable, for
// its requests in flight, which a suspended device never completes.
func (s *CnAgentServer) removeOldSubsys(
	ctx context.Context,
	nqn string,
	cause string,
	nsIdxs []int,
) error {
	slog.WarnContext(ctx, msgSubsysReplaced, slog.String("nqn", nqn),
		slog.String("cause", cause))
	for _, nsIdx := range nsIdxs {
		if !s.moveDark(ctx, nqn, nsIdx) {
			return fmt.Errorf("%s, and namespace %s did not move to "+
				"inaccessible, so the subsystem stays", cause,
				nsResName(nqn, nsIdx))
		}
		path, ok, err := s.nvmet.NsDevicePath(ctx, nqn, nsIdx)
		if err == nil && ok {
			err = s.disableSafe(ctx, path)
		}
		if err != nil {
			return fmt.Errorf("%s, and namespace %s cannot be disabled, "+
				"so the subsystem stays: %w", cause, nsResName(nqn, nsIdx),
				err)
		}
	}
	if err := s.nvmet.RemoveSubsystem(ctx, s.port.PortId, nqn); err != nil {
		return fmt.Errorf("%s, and removing the subsystem failed: %w",
			cause, err)
	}
	return nil
}

// msgSubsysReplaced is the Warn record of removeOldSubsys.
const msgSubsysReplaced = "nvmet subsystem replaced"

// admitsOnlyDesired reports whether a strict read finds the kernel admitting
// no host to the subsystem off its desired list (Nvmet.AdmitsOnly); a read
// that does not answer answers no.
func (s *CnAgentServer) admitsOnlyDesired(
	ctx context.Context,
	conf agent.SubsysConf,
) bool {
	ok, _, err := s.nvmet.AdmitsOnly(ctx, conf.Nqn, conf.AllowedHosts)
	return err == nil && ok
}

// darkenSubsys moves every namespace of a subsystem that is not safe in the
// pass to the inaccessible group, right after its list step, before the
// ns-dev loop and before a transfer's device step (CN9, CN33): the planned
// ones, nsIdxs, and every one the kernel lists under it, which a listing
// that fails leaves to the planned ones. A move that fails is logged, and so
// is a group that does not read back inaccessible; darkenSubsys returns
// those namespaces, whose device steps then put no data under them
// (nsPlan.undarkened, xferPlan.undarkened). The build's last loops leave the
// subsystem's namespaces where this puts them.
func (s *CnAgentServer) darkenSubsys(
	ctx context.Context,
	nqn string,
	nsIdxs []int,
) map[int]bool {
	all := slices.Clone(nsIdxs)
	if listed, _, err := s.nvmet.ListNamespaces(ctx, nqn); err == nil {
		for _, nsIdx := range listed {
			if !slices.Contains(all, nsIdx) {
				all = append(all, nsIdx)
			}
		}
	}
	failed := map[int]bool{}
	for _, nsIdx := range all {
		if !s.moveDark(ctx, nqn, nsIdx) {
			failed[nsIdx] = true
		}
	}
	return failed
}

// moveDark moves one namespace to the inaccessible group and reads the
// group back strictly: it reports whether the namespace is inaccessible, or
// not there. A move or a read that fails is logged and reports no.
func (s *CnAgentServer) moveDark(
	ctx context.Context,
	nqn string,
	nsIdx int,
) bool {
	err := s.setAna(ctx, nqn, nsIdx, common.AnaGrpIdInaccessible)
	if err == nil {
		err = s.isDark(ctx, nqn, nsIdx)
	}
	if err != nil {
		slog.ErrorContext(ctx, "ana transition failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// isDark is nil when a strict read finds the namespace in the inaccessible
// group, or not there, and otherwise says what it found (CN33).
func (s *CnAgentServer) isDark(
	ctx context.Context,
	nqn string,
	nsIdx int,
) error {
	grpId, present, err := s.nvmet.NsAnaGrpId(ctx, nqn, nsIdx)
	switch {
	case err != nil:
		return err
	case present && grpId != common.AnaGrpIdInaccessible:
		return fmt.Errorf("namespace %s/%d reads group %d", nqn, nsIdx, grpId)
	}
	return nil
}

// notDarkDetails is the text of the device row of a namespace its unsafe
// subsystem's move could not make inaccessible, whose device step put no
// data under it (CN16, CN17, CN33).
func notDarkDetails(nqn string, nsIdx int) string {
	return fmt.Sprintf("namespace %s/%d not moved to inaccessible", nqn, nsIdx)
}

// exportRow is a subsystem's row from what its pass found (CN28): the
// configfs and port link status and details, and the cut, which makes a row
// those read OK pending while a stray is still listed with no lockdown the
// lockdown file shows, and an error while the connections could not be read
// or such a lockdown keeps a stray from being ended (CN33). An error of
// configfs or of the port link wins.
func exportRow(
	status pb.ResStatus,
	details string,
	cut cutResult,
) (pb.ResStatus, string) {
	if status != pb.ResStatus_RES_STATUS_OK || cut.held {
		return status, details
	}
	if cut.failed {
		return pb.ResStatus_RES_STATUS_ERROR, cut.text
	}
	return pb.ResStatus_RES_STATUS_PENDING, cut.text
}

// readExport is a subsystem's row on a read, and whether the subsystem is
// safe, read as a converge's list step decides it (listStep): its configfs
// as desired (probeSubsys), or, where it is not, a strict read that finds
// the kernel admitting no host off the desired list; and, in cut mode, no
// stray connection, read only once the list is safe. The row is the
// configfs's when that is not as desired, and otherwise the port link's,
// whose error wins over the cut.
func (s *CnAgentServer) readExport(
	ctx context.Context,
	conf agent.SubsysConf,
) (pb.ResStatus, string, bool) {
	status, details, asDesired := s.probeSubsys(ctx, conf)
	if !asDesired && !s.admitsOnlyDesired(ctx, conf) {
		return status, details, false
	}
	cut := s.readStrays(ctx, conf)
	if !asDesired {
		return status, details, cut.held
	}
	status, details = s.probePortLink(ctx, conf.Nqn)
	status, details = exportRow(status, details, cut)
	return status, details, cut.held
}
