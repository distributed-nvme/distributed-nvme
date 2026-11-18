package dnagent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Migration source (DN12) — step 3, in two halves: the build, which
// convergeSide runs before step 1, and the port link, which it runs after
// ensureCnDm's suspend of step 2.
// ---------------------------------------------------------------------------

// buildMigrSrc is the build of src step 3 (architecture.md, Migration): the
// dm-linear over the side device and, where the level has an export layer,
// the subsystem that admits the destination's host and its namespace, all
// probe-first. It reports whether the build held. It makes no port link, so
// what it builds is out of the destination's reach (buildExport): the
// destination can read through it only once linkMigrSrc runs, after the
// suspend of step 2 has run, the order [D12]'s reason needs: no write may
// reach a region of the side's data after the copy has read it, and only
// once every per-CN dm-linear of the side is proven fenced (sourceFenceGap,
// DN12). The migration source rows carry a step that failed; the export's row is
// otherwise left to linkMigrSrc, so that it reports the link.
func (s *DnAgentServer) buildMigrSrc(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) bool {
	t := st.tracker
	srcInfo := &pb.SideInfo_MigrSrcInfo{}
	info.MigrSrcInfo = srcInfo

	name := plan.migrSrcName()
	err := s.ensureDmLinear(ctx, name, plan.sectors, plan.sideDevPath)
	srcInfo.DmLinearInfo = t.FromErr(resKeyMigrSrcDm, name, "", err)
	if err != nil {
		return false
	}
	if !plan.wantExport {
		return true
	}
	nqn := plan.migrSrcNqn()
	err = s.buildExport(ctx,
		agent.SubsysConf{
			Nqn: nqn,
			AllowedHosts: []string{
				s.nf.DnHostNqn(plan.clusterId, plan.migrSrc.GetDstDnId()),
			},
		},
		agent.NsConf{
			Nqn:        nqn,
			Nsid:       sideNsid,
			DevicePath: s.nf.DmPath(name),
			AnaGrpId:   common.AnaGrpIdOptimized,
		})
	if err != nil {
		srcInfo.NvmeofInfo = t.FromErr(resKeyMigrSrcNvmeof, nqn, "", err)
		return false
	}
	return true
}

// linkMigrSrc is the last of src step 3: the port link of the export
// buildMigrSrc built, the one act that puts it in the destination's reach.
// It sets the export's row, after the link, so the row reports it.
func (s *DnAgentServer) linkMigrSrc(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) {
	if !plan.wantExport {
		return
	}
	nqn := plan.migrSrcNqn()
	err := s.nvmet.EnsurePortLink(ctx, s.port.PortId, nqn, s.port.QidMax)
	if err != nil {
		// A link that failed because the port itself is not there reads as
		// the read reads it, which the side's verdict leaves to the DN's
		// (SH31).
		_, portThere, perr := s.nvmet.PortLinkedState(
			ctx, s.port.PortId, nqn)
		if perr == nil && !portThere {
			info.MigrSrcInfo.NvmeofInfo = st.tracker.Err(
				resKeyMigrSrcNvmeof, nqn, detailsPortMissing)
			return
		}
	}
	info.MigrSrcInfo.NvmeofInfo = st.tracker.FromErr(
		resKeyMigrSrcNvmeof, nqn, "", err)
}

// sourceFenceGap names the per-CN dm-linears of a migration source's side
// that are not proven fenced, each with why, sorted, read from the node: every
// per-CN dm-linear of the side a fresh `dmsetup ls` finds, the ones of CNs the
// request no longer names included, since a leftover one still maps the side
// device until the sweep removes it. One is fenced when it is absent,
// suspended (phase 1 of the fence holds the IO it took, DN12), or not
// suspended with every target of its live table on its own dm-error. A read
// that does not answer proves nothing and is a gap too. The port link of
// step 3 waits for an empty answer (DN12).
func (s *DnAgentServer) sourceFenceGap(
	ctx context.Context,
	plan *sidePlan,
) []string {
	dms, err := s.dm.List(ctx)
	if err != nil {
		return []string{"dmsetup ls: " + err.Error()}
	}
	type linear struct {
		name string
		cnId uint64
	}
	var linears []linear
	for name := range dms {
		dn, ok := common.ParseDmName(name)
		if !ok || dn.Kind != common.DmKindDnLinear ||
			dn.ClusterId != plan.clusterId || dn.NodeId != plan.dnId ||
			len(dn.Ids) != 3 || dn.Ids[0] != plan.spId ||
			dn.Ids[1] != plan.sideId {
			continue
		}
		linears = append(linears, linear{name, dn.Ids[2]})
	}
	sort.Slice(linears, func(i, j int) bool {
		return linears[i].name < linears[j].name
	})
	var gaps []string
	for _, l := range linears {
		dev, err := s.dm.Info(ctx, l.name)
		switch {
		case err != nil:
			gaps = append(gaps, l.name+": "+err.Error())
		case dev == nil || dev.Suspended:
		default:
			if why := s.linearUnfenced(
				ctx, plan, l.name, l.cnId); why != "" {
				gaps = append(gaps, l.name+": "+why)
			}
		}
	}
	return gaps
}

// detailsLinkHeld starts the row of a migration source export left unlinked
// because a per-CN dm-linear of the side is not proven fenced (DN12); the
// gap follows (sourceFenceGap).
const detailsLinkHeld = "not linked until the fence holds: "

// holdMigrSrcLink is the source's step 3 in a pass where a per-CN dm-linear
// of the side is not proven fenced (sourceFenceGap, DN12): it links nothing.
// An export an earlier pass linked stays linked, since that pass's fence held
// and nothing but a request that ends the source role takes a source's
// dm-linear off its fence; its row reads as the link reads. An export not
// linked reads RES_STATUS_PENDING naming the gap, as a check round's read of
// it does (probeStacks), and the side's verdict names it (DN16), so the
// worker sends the syncup again; a port that is not there reads as
// linkMigrSrc reads it.
func (s *DnAgentServer) holdMigrSrcLink(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	gap []string,
) {
	if !plan.wantExport {
		return
	}
	nqn := plan.migrSrcNqn()
	linked, portThere, err := s.nvmet.PortLinkedState(
		ctx, s.port.PortId, nqn)
	switch {
	case err != nil:
		info.MigrSrcInfo.NvmeofInfo = st.tracker.Err(
			resKeyMigrSrcNvmeof, nqn, err.Error())
	case !portThere:
		info.MigrSrcInfo.NvmeofInfo = st.tracker.Err(
			resKeyMigrSrcNvmeof, nqn, detailsPortMissing)
	case linked:
		info.MigrSrcInfo.NvmeofInfo = st.tracker.Ok(
			resKeyMigrSrcNvmeof, nqn, "")
	default:
		info.MigrSrcInfo.NvmeofInfo = st.tracker.Set(resKeyMigrSrcNvmeof,
			nqn, pb.ResStatus_RES_STATUS_PENDING,
			detailsLinkHeld+strings.Join(gap, "; "))
	}
}

// ---------------------------------------------------------------------------
// Migration destination (DN13)
// ---------------------------------------------------------------------------

// migrCloneState is where a migration destination's per-CN stacks serve from
// under the destination rule (migrDstServing) — the three answers the per-CN
// layer has to tell apart, which a bool cannot.
type migrCloneState int

const (
	// migrCloneDown: the dm-clone does not serve — it is not there, the
	// source connection answered without a namespace to read from, or a
	// step stopped, or the pass waits for the cutover, and every read of
	// the per-CN stacks answered with none of them serving through it. The
	// per-CN stacks sit on their dm-errors with every namespace
	// inaccessible (DN13 step (1)).
	migrCloneDown migrCloneState = iota
	// migrCloneUp: the dm-clone serves — every step up to its status read
	// held over the source's namespace device, or a step stopped, or the
	// pass waits for the cutover, while a per-CN stack served through it
	// (migrCloneMapped). The primary's stack is put on it (DN13 step (5)).
	migrCloneUp
	// migrCloneUnread: a step stopped, or the pass waits for the cutover,
	// and where the per-CN stacks are could not be read — the dm-clone's
	// probe did not answer; or it is there and its device number did not
	// answer; or a per-CN dm-linear's probe or table read, or a namespace's
	// group read, did not answer and no other per-CN stack was found
	// serving through it.
	// Nothing decides the per-CN stacks, so a converge leaves them exactly
	// where they are, and both channels judge their rows by the dm-clone's
	// presence (migrServing.cloneLive).
	migrCloneUnread
)

// The details of the destination's two rows that more than one reading
// reports (DN18).
const (
	// detailsCloneMetaMissing is target_info's when DN13 step (2), the slot
	// or its wrapper, refuses for any reason but an area with no free run,
	// which reads provisioning instead (metaRoomDetails): no connect is
	// attempted without them.
	detailsCloneMetaMissing = "clone metadata missing"
	// detailsNoCtrl and detailsNoNs are target_info's when the walk of the
	// source connection finds no controller, or a controller with no
	// namespace device: the source connection's own answer that there is
	// nothing to read from.
	detailsNoCtrl = "no controller for the subsystem"
	detailsNoNs   = "controller has no namespace"
	// detailsPaths leads target_info's once the walk found a controller
	// with a namespace device, the state of each path after it: OK while a
	// path is live, ERROR while none is and the kernel reconnects.
	detailsPaths = "paths: "
	// detailsTargetNotRead leads dm_clone_info's, before the walk's error,
	// when the walk of the source connection did not answer.
	detailsTargetNotRead = "target not read: "
	// detailsTargetNotConnected is dm_clone_info's when the source
	// connection answered that there is nothing to read from.
	detailsTargetNotConnected = "target not connected"
)

// migrDstSeen is what one pass read of a migration destination, in the order
// of DN13's steps: step (2), the slot and its wrapper; whether the pass
// stopped there to wait for the cutover; step (3), the walk of the source
// connection; step (4), the dm-clone; and the dm-clone's status line. A
// converge records what its own steps left behind, a failed step as its error
// (runMigrDst), a check round's read what it reads (probeMigrDst), and one
// rule turns either reading into the destination's two rows (targetRow,
// cloneRow) and into where the per-CN stacks serve from (migrDstServing), so
// the two channels give the same statuses for the same state (DN13), save
// after a command that took effect and still failed: that converge reads its
// step as not held though the node holds it, until the next converge reads the
// step again. Their details may differ: a converge reports the error of a
// command that changed something, a check round's read what it read.
type migrDstSeen struct {
	// metaErr is step (2)'s refusal: the area's, the record's or the
	// wrapper's. Nothing of the later steps is read past it.
	metaErr error
	// awaitingCutover is set when step (2) held on a request that carries
	// migr_dst_conf.cutover_pending: the pass stops there, so nothing of the
	// later steps is read or built, and where the per-CN stacks serve from
	// is read as after a step that stopped (migrDstServing) — on their
	// dm-errors, unless a stack already serves through a dm-clone (DN13).
	awaitingCutover bool
	// conn is the walk of the source connection and connErr its error — on a
	// converge also the error of the connect the pass made. connUnread marks
	// a walk that did not answer, which says nothing about whether the
	// connection is there. Step (4) is read only over a connection that has
	// a controller and a namespace device (srcServes).
	conn       *agent.SubsysState
	connErr    error
	connUnread bool
	// cloneErr is step (4)'s failure: the dm-clone absent, not on the table
	// the step builds, or suspended, or one of that table's devices not
	// read.
	cloneErr error
	// raw is the dm-clone's status line, set when statusRead is, and
	// statusErr the error of a read of it that did not answer.
	raw        string
	statusRead bool
	statusErr  error
	// hydrationErr is the error of a converge's hydration messages; the read
	// a check round starts (SH29) sends none.
	hydrationErr error
}

// srcServes reports whether the walk answered with a source to read from: a
// controller, and its namespace device.
func (m *migrDstSeen) srcServes() bool {
	return m.connErr == nil && m.conn.HasCtrl() && m.conn.DevicePath != ""
}

// targetRow is migr_dst_info.target_info's status and details (DN18). Step
// (2)'s refusal comes first: an area with no free run for a slot not yet held
// is a wait, not a fault, so the row reads provisioning with the area's text,
// as the side's own row does while it waits before zeroing (ensureSideDev);
// once units come free a read finds no run held (HeldCloneMeta) and reads the
// slot missing, which the side's verdict names, so the worker's re-send takes
// the run. Any other refusal reads the slot missing. A pass that stopped
// after step (2) for the cutover reads provisioning, waiting for the
// cutover, with the reason the worker's cutover reads (reason). Past them
// the row is the walk of the source connection, with the error of the
// connect a converge made.
func (m *migrDstSeen) targetRow() (pb.ResStatus, string) {
	switch {
	case m.metaErr != nil && isNoFreeRun(m.metaErr):
		return pb.ResStatus_RES_STATUS_PROVISIONING,
			metaRoomDetails(m.metaErr)
	case m.metaErr != nil:
		return pb.ResStatus_RES_STATUS_MISSING, detailsCloneMetaMissing
	case m.awaitingCutover:
		return pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait
	case m.connErr != nil:
		return pb.ResStatus_RES_STATUS_ERROR, m.connErr.Error()
	case !m.conn.HasCtrl():
		return pb.ResStatus_RES_STATUS_ERROR, detailsNoCtrl
	case m.conn.DevicePath == "":
		return pb.ResStatus_RES_STATUS_ERROR, detailsNoNs
	}
	paths := detailsPaths + strings.Join(m.conn.States, ",")
	if !m.conn.Live {
		return pb.ResStatus_RES_STATUS_ERROR, paths
	}
	return pb.ResStatus_RES_STATUS_OK, paths
}

// cloneRow is migr_dst_info.dm_clone_info's status and details (DN18): the
// first step that stopped, the wait for the cutover as target_info reads it,
// else the hydration the status line shows
// (hydrationGap), under the error of the message that would not apply when a
// converge's message failed. The raw status line follows the row's own text
// on a line of its own whenever the pass read it, whatever the status: the
// gateway's hydration checks read the progress from it (architecture.md,
// Live-state reporting).
func (m *migrDstSeen) cloneRow(plan *sidePlan) (pb.ResStatus, string) {
	status, note := pb.ResStatus_RES_STATUS_OK, ""
	switch {
	case m.metaErr != nil && isNoFreeRun(m.metaErr):
		status, note = pb.ResStatus_RES_STATUS_PROVISIONING,
			metaRoomDetails(m.metaErr)
	case m.metaErr != nil:
		status, note = pb.ResStatus_RES_STATUS_ERROR, m.metaErr.Error()
	case m.awaitingCutover:
		status, note = pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait
	case m.connUnread:
		status, note = pb.ResStatus_RES_STATUS_ERROR,
			detailsTargetNotRead+m.connErr.Error()
	case !m.srcServes():
		status, note = pb.ResStatus_RES_STATUS_MISSING,
			detailsTargetNotConnected
	case m.cloneErr != nil:
		status, note = pb.ResStatus_RES_STATUS_ERROR, m.cloneErr.Error()
	case m.statusErr != nil:
		status, note = pb.ResStatus_RES_STATUS_ERROR, m.statusErr.Error()
	case cloneMetaMode(m.raw) != "":
		status, note = pb.ResStatus_RES_STATUS_ERROR,
			detailsCloneMetaMode+cloneMetaMode(m.raw)
	default:
		if gap := hydrationGap(plan, m.raw); gap != nil {
			status, note = pb.ResStatus_RES_STATUS_ERROR, gap.Error()
			if m.hydrationErr != nil {
				note = m.hydrationErr.Error()
			}
		}
	}
	if m.statusRead {
		return status, agent.CloneStatusDetails(note, m.raw)
	}
	return status, note
}

// detailsCloneMetaMode starts the dm-clone row of a destination whose
// dm-clone's metadata is read-only or failed, the mode after it. dm-clone
// enters either mode when an operation on its metadata fails, and no step of
// a converge brings it back: a reload of its table would reopen the metadata
// from its last commit and read the regions copied since from the source
// again, losing what was written to them. So the row reads RES_STATUS_ERROR
// and the side's verdict does not name it (rowVerdict): no re-send repeats
// a converge that cannot cure it.
const detailsCloneMetaMode = "dm-clone metadata mode "

// cloneMetaMode is the metadata mode of a dm-clone status line when it is
// read-only or failed, "" otherwise, a line that does not parse included.
func cloneMetaMode(raw string) string {
	if common.CloneFailed(raw) {
		return common.CloneMetadataFail
	}
	if status, ok := common.ParseCloneStatus(raw); ok &&
		status.MetadataMode == common.CloneMetadataReadOnly {
		return common.CloneMetadataReadOnly
	}
	return ""
}

// reason is the reason of both of the destination's rows (DN18):
// RES_REASON_AWAITING_CUTOVER while the pass waits for the cutover with its
// metadata wrapper standing, which the worker's cutover reads in place of the
// rows' text (dnv-worker.md RW18), and none otherwise.
func (m *migrDstSeen) reason() pb.ResReason {
	if m.awaitingCutover {
		return pb.ResReason_RES_REASON_AWAITING_CUTOVER
	}
	return pb.ResReason_RES_REASON_NONE
}

// reportMigrDst sets the destination's two rows from what the pass read, the
// one place either channel sets them (DN13, DN18).
func (s *DnAgentServer) reportMigrDst(
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	seen *migrDstSeen,
) {
	t := st.tracker
	dstInfo := &pb.SideInfo_MigrDstInfo{}
	info.MigrDstInfo = dstInfo
	status, details := seen.targetRow()
	dstInfo.TargetInfo = t.SetReason(resKeyMigrDstTarget,
		plan.srcNqnOfDst(), status, details, seen.reason())
	status, details = seen.cloneRow(plan)
	dstInfo.DmCloneInfo = t.SetReason(resKeyMigrDstClone,
		plan.migrFinalName(), status, details, seen.reason())
}

// migrServing is where the destination rule puts the per-CN stacks
// (migrDstServing), and whether the reading found the dm-clone there.
type migrServing struct {
	state   migrCloneState
	present bool
}

// cloneLive is the reading of the dm-clone the per-CN rows are judged by
// (sidePlan.linearBacking, sidePlan.anaGrpId): serving where the rule puts
// the stacks on it, and, where the rule cannot read where they are, wherever
// it is present — what a converge that leaves the stacks alone and the read
// a check round starts (SH29) both judge them by.
func (m migrServing) cloneLive() bool {
	return m.state == migrCloneUp ||
		(m.state == migrCloneUnread && m.present)
}

// migrDstServing is DN13's answer to where a destination's per-CN stacks serve
// from, for a converge and a check round's read alike. Up when every step up
// to the status read held over the source's namespace device. Down when the
// source connection's own answer is that there is no namespace device to read
// from — no controller and a connect that failed or brought none, or a
// controller without one — whatever the dm-linears show, as a level that
// suppresses the role does (DN11): the dm-clone has lost the device it reads
// unhydrated regions from. Otherwise a step stopped for a reason of its own,
// or the pass waits for the cutover, and the answer is read from where the
// stacks are (migrDstStopped). Before its cutover a destination has no
// dm-clone, so a waiting pass puts the stacks on their dm-errors with every
// namespace inaccessible (DN13 step (1)). The worker never sends
// cutover_pending again once the cutover cleared it, so a waiting pass that
// finds a stack serving through the dm-clone converges a request older than
// the one whose converge built that dm-clone, and it leaves that stack
// serving: reloading it onto its dm-error would fail host IO until the
// worker sent the newer request again.
//
// Hydration that will not converge (a knob or the enable) does not enter it:
// the dm-clone is converged onto the source the pass found and reads through
// to it, so demoting the primary's dm-linear back onto its dm-error over it
// would turn a healthy side into an erroring one.
func (s *DnAgentServer) migrDstServing(
	ctx context.Context,
	plan *sidePlan,
	seen *migrDstSeen,
) migrServing {
	switch {
	case seen.metaErr != nil, seen.awaitingCutover, seen.connUnread:
		return s.migrDstStopped(ctx, plan)
	case !seen.srcServes():
		return migrServing{state: migrCloneDown}
	case seen.cloneErr != nil, seen.statusErr != nil:
		return s.migrDstStopped(ctx, plan)
	}
	return migrServing{state: migrCloneUp, present: true}
}

// migrDstStopped reads where the per-CN stacks serve from when a destination
// step stopped for a reason of its own — the slot or its wrapper, a walk of
// the source connection that did not answer, the dm-clone, its status read —
// or the pass waits for the cutover (migrDstServing), so a stopped step
// takes the serving path neither onto the dm-clone nor off it. Off it, one
// unanswered command would cut the leg's only serving path until a later
// pass finished the build. Onto it, a stopped pass would serve
// a dm-clone that an earlier pass took the stacks off when the source
// connection answered without a namespace ("target not connected"): that pass
// leaves the dm-clone in place, still mapping the source device that is gone.
// It only reads, so the read a check round starts reads the stacks through
// it as a converge does.
func (s *DnAgentServer) migrDstStopped(
	ctx context.Context,
	plan *sidePlan,
) migrServing {
	dev, err := s.dm.Info(ctx, plan.migrFinalName())
	switch {
	case err != nil:
		return migrServing{state: migrCloneUnread}
	case dev == nil:
		return migrServing{state: migrCloneDown}
	}
	return migrServing{state: s.migrCloneMapped(ctx, plan), present: true}
}

// ensureMigrDst runs DN13 steps (2) to (4) of the destination sequence of
// architecture.md, Migration — the slot and its wrapper, the source connection,
// the dm-clone with hydration on — and the dm-clone's status read (runMigrDst),
// then reports the destination's two rows and decides where the per-CN stacks
// serve from by the rule a check round's read applies to what it reads
// (reportMigrDst, migrDstServing). It returns that decision, which sets the
// per-CN dm-linear targets and ANA groups for this pass. Step (5), the
// primary's stack onto the dm-clone, is convergeSide's. A step that stops
// leaves its row as it reads, and nothing of the agent's own runs the converge
// again but an agent restart's reconcile (DN2): a SyncupSide does, the worker's
// re-send among them, which the side's verdict brings while a converge would
// act on the row (rowVerdict). The slot of step (2) is normally the run the
// side has held since it provisioned (holdMigrMeta), so step (2) finds its
// record and builds the wrapper over it. connectBudget is the budget of step
// (3)'s retries (connectMigrSrc).
func (s *DnAgentServer) ensureMigrDst(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	connectBudget time.Duration,
) migrServing {
	// Chunks left over from a previous migration on this side are never
	// applied to a new one.
	if st.chunks.Len() > 0 &&
		st.chunkMigrId != plan.migrDst.GetMigrId() {
		s.dropChunks(ctx, st)
	}
	seen := s.runMigrDst(ctx, st, plan, connectBudget)
	s.reportMigrDst(st, plan, info, seen)
	return s.migrDstServing(ctx, plan, seen)
}

// runMigrDst runs the destination's steps (2) to (4), the hydration messages
// and the status read, and records what each left behind: the converge's
// reading of the destination. A step that fails ends the run there, and its
// error is its reading, whatever its command did in the kernel (DN13). While
// the request carries cutover_pending the run ends after step (2) holds.
// connectBudget is the budget of step (3)'s retries (connectMigrSrc).
func (s *DnAgentServer) runMigrDst(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	connectBudget time.Duration,
) *migrDstSeen {
	seen := &migrDstSeen{}
	nqn := plan.srcNqnOfDst()

	// (2) the dm-clone metadata slot and its wrapper device.
	if seen.metaErr = s.ensureMigrMeta(ctx, plan); seen.metaErr != nil {
		return seen
	}
	// Before the cutover the src side still serves the leg and has built no
	// migration source export, so there is nothing to connect to yet: the
	// side stops with its wrapper standing and its stacks where a stopped
	// step puts them (migrDstServing), and says so (migrDstSeen.reason).
	if plan.migrDst.GetCutoverPending() {
		seen.awaitingCutover = true
		return seen
	}

	// (3) the connect, tried again within the pass for a short while
	// (connectMigrSrc), and after a connect that succeeded the wait for the
	// source's namespace node, which the kernel adds from a scan the connect
	// only queued (awaitMigrSrcNs). Past that pass, the connect is tried
	// again by the next converge: a SyncupSide's, the worker's re-send among
	// them (architecture.md, Migration), which the side's verdict brings
	// while the walk finds no controller or does not answer
	// (nameMigrDstRows), or an agent restart's reconcile (DN2).
	state, err := s.host.ListSubsys(ctx, nqn)
	// unread: the walk did not answer, so the connection is not known to be
	// gone. A connect that failed is the opposite case — the walk had just
	// answered "no controller".
	unread := err != nil
	// No controller is no connection, so the pass connects when the walk
	// answered and found none (HasCtrl): no subsystem, or one the kernel
	// keeps with no controller in it, which it does for as long as something
	// holds the subsystem's multipath head open, as the dm-clone holds the
	// source's (architecture.md, Teardown by sweep). The cn twin is
	// ensureCloneSource.
	if err == nil && !state.HasCtrl() {
		if connErr := s.connectMigrSrc(
			ctx, plan, nqn, connectBudget); connErr != nil {
			err = connErr
		} else {
			state, err = s.awaitMigrSrcNs(ctx, nqn)
			unread = err != nil
		}
	}
	seen.conn, seen.connErr, seen.connUnread = state, err, unread
	if unread || !seen.srcServes() {
		return seen
	}

	// (4) the dm-clone itself, created with hydration off so the bitmap
	// chunks land before any region is copied.
	rebuilt, err := s.ensureDmClone(ctx, plan, state.DevicePath)
	if err != nil {
		seen.cloneErr = err
		return seen
	}
	if rebuilt {
		// Re-apply every chunk of the applied set whenever the dm-clone is
		// created, resumed or reloaded (dnagent.md DN15).
		s.applyChunks(ctx, st, plan)
	}
	seen.hydrationErr = s.ensureHydration(ctx, plan)
	seen.raw, seen.statusErr = s.dm.CloneStatus(ctx, plan.migrFinalName())
	seen.statusRead = seen.statusErr == nil
	return seen
}

// migrCloneMapped reads whether the per-CN stacks serve through the dm-clone,
// which is there: up when a per-CN dm-linear of the plan maps it and that
// CN's namespace is in the optimized or the non-optimized group, down when
// every read answered and no stack does — an absent dm-linear maps nothing,
// and an absent namespace is in no group — and unread when a read the
// answer needs did not answer. The group is read strictly (NsAnaGrpId), so a
// group read that did not answer leaves the answer unread: taken as a group
// that is not optimized, it would reload the leg's only serving path onto
// its dm-error. The namespace's group is the half that records the last
// decision: a pass that takes the stacks off the dm-clone moves every
// namespace to the inaccessible group even when its reload of a dm-linear is
// refused, and that dm-linear stays on the dm-clone (a reload fails closed;
// a later pass's pre-step resumes the primary's there when no dm-error table
// waits on it, unfenceLinears, and the build reloads any other). Read from
// the table alone, such a dm-linear would put a stopped pass back on a
// dm-clone whose source is gone. A pass that serves through the dm-clone
// puts the primary's namespace in the optimized group while its path is
// open (DN10) and every other in the non-optimized one, so a non-optimized
// namespace over the dm-clone is a stack of such a pass: a CN the primary
// flip demoted, whose fence failed, while the new primary's path waits for
// that fence in the inaccessible group. Any per-CN
// stack counts, not only the primary's: a primary flip that arrives in a
// stopped pass then moves the serving path to the new primary once its
// fence holds, the old primary's stack still mapping the dm-clone. Only plan.cnIds
// are read, so when a flip also drops the old primary, whose stack the
// pass's sweep removes, the read finds none on the dm-clone and the new
// primary waits on its dm-error until a pass runs the whole sequence, which
// the re-send the stopped step's named row brings runs (rowVerdict). It only
// reads: the read a check round starts reads the stacks through it as a
// converge does (migrDstStopped).
func (s *DnAgentServer) migrCloneMapped(
	ctx context.Context,
	plan *sidePlan,
) migrCloneState {
	cloneNo, err := s.dm.DevNo(ctx, s.nf.DmPath(plan.migrFinalName()))
	if err != nil {
		return migrCloneUnread
	}
	answer := migrCloneDown
	for _, cnId := range plan.cnIds {
		name := plan.linearName(cnId)
		dev, err := s.dm.Info(ctx, name)
		if err != nil {
			answer = migrCloneUnread
			continue
		}
		if dev == nil {
			continue
		}
		// A suspended linear with a table waiting is a reload cut after its
		// load: the waiting table, not the live one, is where its last pass
		// put the stack. Read from the live table, a promotion onto the
		// dm-clone cut after its load would read down, and its stack would
		// be resumed on its dm-error, failing the IO it holds.
		read := s.dm.Table
		if dev.Suspended && dev.Waiting {
			read = s.dm.WaitingTable
		}
		targets, err := read(ctx, name)
		if err != nil {
			answer = migrCloneUnread
			continue
		}
		if !linearMaps(targets, plan.sectors, cloneNo) {
			continue
		}
		grpId, present, err := s.nvmet.NsAnaGrpId(
			ctx, plan.sideNqn(cnId), sideNsid)
		if err != nil {
			answer = migrCloneUnread
			continue
		}
		if present && (grpId == common.AnaGrpIdOptimized ||
			grpId == common.AnaGrpIdNonOptimized) {
			return migrCloneUp
		}
	}
	return answer
}

// migrMetaUnits is the size of this migration's run in the node's
// clone-metadata area, in DnCloneMetaUnit units: the shared formula,
// common.CloneMetaUnits, over the moved side's bytes — ext_cnt extents of the
// DN's extent size — and the pool's block size, migr_dst_conf.block_size
// (architecture.md, Metadata units). The control plane charges the node by
// the same inputs when it creates the migration, so the units it counts and
// the runs on the disk agree.
//
// The area is **per DN**, not per SP: DnCloneMetaSize is one region of the
// disk shared by every destination role the node hosts, across every SP on
// it, so DnCloneMetaUnitCnt bounds the sum of their runs node-wide and
// MaxMigrCntPerSp says nothing about it (DN13). A block size under one sector
// is refused, as the dm-clone refuses it for its region size (ensureDmClone).
func migrMetaUnits(plan *sidePlan) (uint64, error) {
	blockSize := plan.migrDst.GetBlockSize()
	if blockSize/agent.SectorSize == 0 {
		return 0, fmt.Errorf("migr_dst_conf.block_size is 0")
	}
	return common.CloneMetaUnits(
		plan.conf.GetExtCnt()*plan.extentSize, blockSize), nil
}

// migrMetaRefusal is the refusal of this migration's run as a read-only pass
// can establish it: the size error of migrMetaUnits, else the clone-metadata
// area's refusal of the run, as CloneMetaRefusal reads it. The probe of a
// destination that has not provisioned reports what the converge reports
// from holdMigrMeta for the same refusal (probeSideDev), so the side's rows
// of the two channels agree, and the side's verdict reads it
// (zeroingVerdict).
func (s *DnAgentServer) migrMetaRefusal(
	ctx context.Context,
	plan *sidePlan,
) error {
	units, err := migrMetaUnits(plan)
	if err != nil {
		return err
	}
	return s.meta.CloneMetaRefusal(
		ctx, plan.spId, plan.migrDst.GetMigrId(), units)
}

// migrMetaRead is DN13 step (2) as a read-only pass reads it, and nil when
// the step would hold with nothing to do: the run the area hands back behind
// the gates AllocCloneMeta applies, in its texts (HeldCloneMeta), and its
// wrapper there, on the table ensureMigrMeta builds, writable and resumed —
// what that step leaves behind when it holds.
func (s *DnAgentServer) migrMetaRead(
	ctx context.Context,
	plan *sidePlan,
) error {
	units, err := migrMetaUnits(plan)
	if err != nil {
		return err
	}
	rec, err := s.meta.HeldCloneMeta(
		ctx, plan.spId, plan.migrDst.GetMigrId(), units)
	if err != nil {
		return err
	}
	devNo, err := s.dm.DevNo(ctx, s.disk)
	if err != nil {
		return err
	}
	name := plan.migrMetaDmName()
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return fmt.Errorf("%s is missing", name)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	sectors, offsetSectors := metaWrapperGeometry(rec)
	switch {
	case !metaWrapperMaps(targets, sectors, devNo, offsetSectors):
		return fmt.Errorf("%s does not map its clone-metadata run", name)
	case dev.ReadOnly:
		return fmt.Errorf("%s is read-only", name)
	case dev.Suspended:
		return fmt.Errorf("%s is suspended", name)
	}
	return nil
}

// connectMigrSrc connects this destination to the migration source export
// on the source DN (DN13 step (3)), and tries a connect that failed again
// after DnMigrConnectRetryPause, within this pass, for as long as
// connectBudget covers the pause; each failed connect's own elapsed time
// draws on the budget too, so a slow refusal can spend it at once. After the
// cutover the src side links that export to its port only after its step 1
// and the suspend that starts its step 2 (DN12), in its own pass, so the
// first connect of the pass the cutover brings often comes too early. That
// pass's budget is DnMigrCutoverConnectBudget, sized to outlast the src
// side's pass on a loaded node, so that normally the connect that ends the
// migration gap is this pass's and not that of the converge the worker's
// re-send brings, which comes on the side's next round, up to one interval
// later (dnv-worker.md RW4, RW6); a src side slower than that leaves it to
// that re-send all the same. Every other pass's budget is
// DnMigrConnectPassBudget (convergeSide). The error is never classified: an
// export not linked yet, a dead source DN and a refused host answer alike,
// and the budget is what caps how often the last two are tried. The last
// attempt's error is the one returned; the target row carries it and the
// side's verdict names it (rowVerdict). The cn twin is connectWithin.
func (s *DnAgentServer) connectMigrSrc(
	ctx context.Context,
	plan *sidePlan,
	nqn string,
	connectBudget time.Duration,
) error {
	tr := plan.migrDst.GetSrcNvmeTrConf()
	conf := agent.TrConf{
		TrType:  tr.GetTrType(),
		AdrFam:  tr.GetAdrFam(),
		TrAddr:  tr.GetTrAddr(),
		TrSvcId: tr.GetTrSvcId(),
	}
	budget := agent.NewWaitBudget(connectBudget, s.now, s.sleep)
	for {
		start := budget.Now()
		// The default fast IO fail timeout: the side syncup carries no
		// pool setting, and only leg connections take the pool's (SH20).
		err := s.host.Connect(ctx, conf, nqn, plan.dnHostNqn(),
			common.DefaultNvmeFastIoFailTmo)
		if err == nil {
			return nil
		}
		budget.Charge(budget.Now().Sub(start))
		if !budget.Pause(ctx, common.DnMigrConnectRetryPause) {
			return err
		}
	}
}

// awaitMigrSrcNs re-reads the migration source's subsystem after a connect
// this pass made, until its namespace head is complete — its device there
// and its ng device listed beside it — pausing DnMigrDstNsPause between
// reads and DnMigrDstNsWait in all — the reads themselves are not counted
// (DN13 step (3)), as the cn twin waits (cnagent.md CN10, the head wait).
// The kernel returns from `nvme connect` as soon as the controller is live
// and only QUEUES the namespace scan that adds the device, so a single
// re-read could find the controller and no namespace yet, failing the
// target with "controller has no namespace" for a device milliseconds away;
// and it adds the ng device last, after the node, so a head whose ng device
// is not listed yet may not be opened by its number, which the dm-clone's
// table takes. Past the budget the head is used as the last read found it:
// a device with no ng device listed yet is handed on, and a step after the
// wait that fails fails the target with its own error.
// A ListSubsys that fails ends the wait at once. It connects nothing and
// has a budget of its own: the connect's retries are connectMigrSrc's.
func (s *DnAgentServer) awaitMigrSrcNs(
	ctx context.Context,
	nqn string,
) (*agent.SubsysState, error) {
	budget := agent.NewWaitBudget(common.DnMigrDstNsWait, s.now, s.sleep)
	for {
		state, err := s.host.ListSubsys(ctx, nqn)
		if err != nil || (state.DevicePath != "" && state.NgListed) {
			return state, err
		}
		if !budget.Pause(ctx, common.DnMigrDstNsPause) {
			return state, nil
		}
	}
}

// holdMigrMeta takes this migration's run in the node's clone-metadata area
// before the destination side zeroes (DN13): the record alone, its head
// zeroed before it is written (AllocCloneMeta), and nothing above it — the
// wrapper, the source connection and the dm-clone wait for DN13 step (2) and
// the open provisioning gate. It runs at every level, as the record's claim
// carries no level gate (claimedMigrs), and a level that suppresses the role
// still wants no wrapper (DN11). A record already held is handed back with
// nothing written.
//
// The run comes first because a destination that zeroes is flipped
// provisioned once it is done, and the cutover then waits for the wrapper
// over that run (DN13 step (2)): one that found no room only after that
// would hold its migration until units came free, or, where its request
// never carries cutover_pending (architecture.md, Upgrade in lockstep), leave
// the leg in the migration gap that long, since the source then cuts its own
// paths at the flip (architecture.md, Migration). While it holds no run it
// zeroes nothing, so it is not flipped and the source keeps serving.
func (s *DnAgentServer) holdMigrMeta(
	ctx context.Context,
	plan *sidePlan,
) error {
	units, err := migrMetaUnits(plan)
	if err != nil {
		return err
	}
	_, err = s.meta.AllocCloneMeta(
		ctx, plan.spId, plan.migrDst.GetMigrId(), units)
	return err
}

// ensureMigrMeta reserves this migration's dm-clone metadata slot and builds
// the wrapper dm-linear over it. The slot is normally held already, taken
// before the side zeroed (holdMigrMeta), and AllocCloneMeta then hands its
// record back with nothing written. The wrapper exists because the dm-clone
// target reads its metadata device from sector 0 and takes no offset
// argument (DN13); AllocCloneMeta zeroes a freshly chosen slot's first 8 KiB
// before its record is persisted, so stale bytes in that slot can never be
// misparsed as a valid dm-clone superblock. A wrapper that reads read-only
// is reloaded and made writable (makeWritable), as step (2)'s read wants it
// (migrMetaRead).
func (s *DnAgentServer) ensureMigrMeta(
	ctx context.Context,
	plan *sidePlan,
) error {
	units, err := migrMetaUnits(plan)
	if err != nil {
		return err
	}
	migrId := plan.migrDst.GetMigrId()
	rec, err := s.meta.AllocCloneMeta(ctx, plan.spId, migrId, units)
	if err != nil {
		return err
	}
	devNo, err := s.dm.DevNo(ctx, s.disk)
	if err != nil {
		return err
	}
	name := plan.migrMetaDmName()
	sectors, offsetSectors := metaWrapperGeometry(rec)
	table := agent.LinearTable(sectors, devNo, offsetSectors)

	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return s.dm.Create(ctx, name, table)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	if !metaWrapperMaps(targets, sectors, devNo, offsetSectors) ||
		dev.ReadOnly {
		if err := s.dm.Reload(ctx, name, table); err != nil {
			return err
		}
		return s.makeWritable(ctx, name, dev)
	}
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// metaWrapperGeometry is where the wrapper of a clone-metadata run maps on
// the disk: the run's length and its first sector, both in sectors.
func metaWrapperGeometry(
	rec *pb.DnDiskTable_CloneMetaRecord,
) (sectors uint64, offsetSectors uint64) {
	sectors = rec.GetUnitCount() * common.DnCloneMetaUnit / agent.SectorSize
	offsetSectors = (common.DnCloneMetaOffset +
		rec.GetUnitStart()*common.DnCloneMetaUnit) / agent.SectorSize
	return sectors, offsetSectors
}

// metaWrapperMaps reports whether a live table is the one ensureMigrMeta
// builds over the disk devNo: a single "linear" target of the run's sectors
// from its first sector, offsetSectors.
func metaWrapperMaps(
	targets []common.DmTarget,
	sectors uint64,
	devNo string,
	offsetSectors uint64,
) bool {
	return len(targets) == 1 && targets[0].Type == "linear" &&
		targets[0].Length == sectors && len(targets[0].Args) == 2 &&
		targets[0].Args[0] == devNo &&
		targets[0].Args[1] == strconv.FormatUint(offsetSectors, 10)
}

// ensureDmClone converges the dm-clone and reports whether this pass created,
// resumed or reloaded it: each is a pass after which the stored pieces are
// applied again (DN15), a resume because a piece that came while the dm-clone
// was suspended sent no discard. The source device number changes across
// reconnects, so a converged check compares the resolved devices, not just
// presence.
func (s *DnAgentServer) ensureDmClone(
	ctx context.Context,
	plan *sidePlan,
	srcDevPath string,
) (bool, error) {
	name := plan.migrFinalName()
	regionSectors := plan.migrDst.GetBlockSize() / agent.SectorSize
	if regionSectors == 0 {
		return false, fmt.Errorf("migr_dst_conf.block_size is 0")
	}
	if plan.sectors == 0 {
		return false, fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	metaNo, err := s.dm.DevNo(ctx, plan.migrMetaDmPath())
	if err != nil {
		return false, err
	}
	destNo, err := s.dm.DevNo(ctx, plan.sideDevPath)
	if err != nil {
		return false, err
	}
	srcNo, err := s.dm.DevNo(ctx, srcDevPath)
	if err != nil {
		return false, err
	}
	conf := plan.migrDst.GetDmCloneConf()
	// no_discard_passdown is mandatory on every dnv dm-clone (DN13 step
	// 4): a discard on a dm-clone is this design's metadata-only "mark
	// this region hydrated" primitive (architecture.md, Bitmap push protocol,
	// and raid0 bitmap math), and dm-clone
	// turns passdown on by default whenever the destination's discard
	// granularity is no larger than a region — which a dm-linear over a raw
	// disk satisfies. The hazard is *after* the cutover (architecture.md,
	// Migration, dst step 5), not before it:
	// host IO then flows through this dm-clone, a host write hydrates region
	// r, and a skip-bitmap chunk whose bit for r was read from the CN thin
	// metadata before that write can still arrive later (chunk pushes are
	// legal at any time, and a restart re-applies every stored chunk). The
	// agent then discards r; with passdown that discard would reach the
	// side device and destroy the only copy of an acknowledged write. Without
	// it the same discard is the metadata no-op that
	// architecture.md, Bitmap push protocol, and [D7] already
	// assume. Cost: only discards issued during an active migration stop
	// reaching the disk; after FinishMigration the dm-clone is gone.
	table := agent.CloneTable(plan.sectors, metaNo, destNo, srcNo,
		regionSectors, true, true,
		conf.GetHydrationThreshold(), conf.GetHydrationBatchSize())

	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return false, err
	}
	if dev == nil {
		if err := s.dm.Create(ctx, name, table); err != nil {
			return false, err
		}
		return true, nil
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return false, err
	}
	if cloneTableMaps(targets, plan.sectors, metaNo, destNo, srcNo,
		regionSectors) {
		// Suspended on the table it wants — an interrupted reload, or a
		// failed one whose old table is wanted again, since a reload fails
		// closed (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`)
		// — it is resumed ([D12]).
		if dev.Suspended {
			return true, s.dm.Resume(ctx, name)
		}
		return false, nil
	}
	if err := s.dm.Reload(ctx, name, table); err != nil {
		return false, err
	}
	return true, nil
}

// cloneTableMaps reports whether a live table is the dm-clone ensureDmClone
// builds: a single "clone" target of the side's sectors over the metadata
// wrapper metaNo, the side device destNo and the source's namespace device
// srcNo, in regions of regionSectors.
func cloneTableMaps(
	targets []common.DmTarget,
	sectors uint64,
	metaNo, destNo, srcNo string,
	regionSectors uint64,
) bool {
	return len(targets) == 1 && targets[0].Type == "clone" &&
		targets[0].Length == sectors && len(targets[0].Args) >= 4 &&
		targets[0].Args[0] == metaNo && targets[0].Args[1] == destNo &&
		targets[0].Args[2] == srcNo &&
		targets[0].Args[3] == strconv.FormatUint(regionSectors, 10)
}

// migrCloneRead is DN13 step (4) as a read-only pass reads it over the
// source's namespace device srcDevPath, and nil when the step would hold
// with nothing to do: the dm-clone there, on the table ensureDmClone builds
// over the wrapper, the side device and that device, and resumed — what that
// step leaves behind when it holds.
func (s *DnAgentServer) migrCloneRead(
	ctx context.Context,
	plan *sidePlan,
	srcDevPath string,
) error {
	name := plan.migrFinalName()
	regionSectors := plan.migrDst.GetBlockSize() / agent.SectorSize
	if regionSectors == 0 {
		return fmt.Errorf("migr_dst_conf.block_size is 0")
	}
	if plan.sectors == 0 {
		return fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	metaNo, err := s.dm.DevNo(ctx, plan.migrMetaDmPath())
	if err != nil {
		return err
	}
	destNo, err := s.dm.DevNo(ctx, plan.sideDevPath)
	if err != nil {
		return err
	}
	srcNo, err := s.dm.DevNo(ctx, srcDevPath)
	if err != nil {
		return err
	}
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return fmt.Errorf("%s is missing", name)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	if !cloneTableMaps(targets, plan.sectors, metaNo, destNo, srcNo,
		regionSectors) {
		return fmt.Errorf("%s does not map the clone-metadata wrapper, "+
			"the side device and %s", name, srcDevPath)
	}
	if dev.Suspended {
		return fmt.Errorf("%s is suspended", name)
	}
	return nil
}

// ensureHydration applies the desired hydration state and knobs through
// dmsetup messages — probe-first, so a converged dm-clone is left alone: it
// sends what hydrationWants lists for the status it reads. Hydration is
// infrastructure IO, not user IO, so no sp_level ever pauses it: this
// function only ever enables it ([D11]).
func (s *DnAgentServer) ensureHydration(
	ctx context.Context,
	plan *sidePlan,
) error {
	name := plan.migrFinalName()
	raw, err := s.dm.CloneStatus(ctx, name)
	if err != nil {
		return err
	}
	status, ok := common.ParseCloneStatus(raw)
	if !ok {
		return fmt.Errorf("unparsable dm-clone status %q", raw)
	}
	for _, msg := range hydrationWants(
		plan.migrDst.GetDmCloneConf(), status) {
		if err := s.dm.Message(ctx, name, 0, msg); err != nil {
			return err
		}
	}
	return nil
}

// hydrationWants lists the messages that bring a dm-clone whose status reads
// st to the hydration conf asks for, in the order ensureHydration sends them:
// each knob conf sets that st does not show, then the enable while hydration
// is off.
func hydrationWants(conf *pb.DmCloneConf, st *common.CloneStatus) []string {
	var wants []string
	if threshold := conf.GetHydrationThreshold(); threshold != 0 &&
		st.Threshold != threshold {
		wants = append(wants,
			fmt.Sprintf("hydration_threshold %d", threshold))
	}
	if batch := conf.GetHydrationBatchSize(); batch != 0 &&
		st.BatchSize != batch {
		wants = append(wants, fmt.Sprintf("hydration_batch_size %d", batch))
	}
	if !st.HydrationEnabled {
		wants = append(wants, "enable_hydration")
	}
	return wants
}

// hydrationGap is the destination rule's test of hydration (DN13): nil when
// the status line raw shows the knobs the plan sets and hydration on, else
// what it still lacks, or that it does not parse.
func hydrationGap(plan *sidePlan, raw string) error {
	status, ok := common.ParseCloneStatus(raw)
	if !ok {
		return fmt.Errorf("unparsable dm-clone status %q", raw)
	}
	if wants := hydrationWants(
		plan.migrDst.GetDmCloneConf(), status); len(wants) > 0 {
		return fmt.Errorf("hydration not converged: %s",
			strings.Join(wants, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// The DN12 fence timer's converge
// ---------------------------------------------------------------------------

// reconvergeSide re-runs one side's converge from a background goroutine under
// the DN1 locks. The DN12 fence timer is its one caller: the converge that ends
// a source's bounded suspension once the window has elapsed, which no RPC may
// wait for. It is no retry: it is armed only for a source's window, never
// because a step stopped, though its converge runs that source's steps again; a
// stopped step is otherwise re-driven only by the worker's re-send its name
// brings, or by an agent restart's reconcile (SH31, DN2). It passes
// DnMigrConnectPassBudget, the budget of a destination's connect, which a
// migration source's converge never draws on.
//
// The side is checked under the node read lock and before its object lock is
// taken: a side dropped since the attempt was scheduled — the drop runs under
// the node write lock and forgets the side's lock entry (dropSideState) —
// gets no new entry from an attempt that converges nothing (dnagent.md,
// Concurrency — `locks.go`). Nothing that drops or replaces a held side runs
// under the read lock, so the side the check finds stays for the attempt.
// Once found, the attempt marks its converge before it waits for the object
// lock and cancels no read (SH30), and its converge stores the side's
// observation, with the revision of the request it converged, before the
// locks go (SH28).
func (s *DnAgentServer) reconvergeSide(
	ctx context.Context,
	key string,
	st *sideState,
) {
	// Every background attempt is its own traceable operation (SH2).
	attemptCtx := common.WithTraceId(ctx, common.NewTraceId())
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getSide(key) != st {
		return
	}
	s.beginSideConverge(key)
	defer s.endConverge(key)
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	req := st.req.Load()
	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if dn == nil {
		return
	}
	info, sweep := s.convergeSide(attemptCtx, st,
		dn.req.Load().GetExtentSize(), common.DnMigrConnectPassBudget)
	storeObs(s, attemptCtx, &st.obs, info, sweep.Reply(), req.GetRevision())
}
