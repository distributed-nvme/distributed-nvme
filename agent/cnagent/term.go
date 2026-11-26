package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	pb "github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN35 — the term of a primary's stack
//
// A demotion's sweep can stop part way, and leave an array or a thin pool of
// the cntlr in place. dm-thin reads a pool's metadata from its legs only
// when the pool device is made, so an old primary that got the role back
// and kept such a pool would serve from mappings that miss every block the
// other primary wrote in between, and its next commit would write its old
// metadata over the newer. Each term of a primary has a number, the pool
// revision of the failover that made the cntlr the primary, or of a
// replacement made as the primary (Cntlr.primary_revision), and the agent
// keeps the number its stack was built under (StackTerm, in the local
// store). A primary under another number takes it at once where no piece
// of a primary stack is there, takes a stack whose namespaces serve while
// it keeps no number, and otherwise sweeps as a standby first and builds
// only once that sweep is clean (termStep).
// ---------------------------------------------------------------------------

// termMismatch says whether plan is a primary's under a term its stack was
// not built under: its request carries a term, and the agent keeps another
// one, or none. A request with no term, a pool's first primary or a record
// an older build wrote, never mismatches.
func termMismatch(st *cntlrState, plan *cntlrPlan) bool {
	term := plan.cntlr.GetPrimaryRevision()
	return plan.primary && term != 0 && st.stackTerm != term
}

// termStep is what a primary converge under a mismatched term does
// (termMismatch, CN35) before its sweep, by what the node holds: with no
// piece of a primary stack of the pool there (stackThere), nothing is left
// to remove, and the term is taken at once; with one there and no term kept,
// a stack whose namespaces serve, one in the optimized group, is the one the
// role's primary built and lost its term file with, and is taken as it
// stands; any other stack is torn down first (termTearDown). A read that
// does not answer tears down: a teardown is safe, the stale stack is not.
func (s *CnAgentServer) termStep(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) termAction {
	there, err := s.stackThere(ctx, plan)
	switch {
	case err != nil:
		return termTearDown
	case !there:
		return termTake
	case st.stackTerm == 0:
		_, nss, _ := fenceInputs(plan)
		if s.fenceServed(ctx, nss, nil) {
			slog.WarnContext(ctx, msgTermAdopted,
				slog.Uint64("term", plan.cntlr.GetPrimaryRevision()))
			return termTake
		}
	}
	return termTearDown
}

// termAction is termStep's answer.
type termAction int

const (
	// termTake: save the request's term and build as the primary.
	termTake termAction = iota
	// termTearDown: sweep as a standby first.
	termTearDown
)

// msgTermAdopted is the Warn record of a primary that kept no term and
// found its stack serving: it takes the stack as it stands under the
// request's term (CN35).
const msgTermAdopted = "a serving stack with no kept term taken under the role's term"

// stackThere says whether the node holds a piece of a primary stack of
// plan's pool on this node: an md array of one of its groups, or a
// device-mapper device of stackKinds of the pool.
func (s *CnAgentServer) stackThere(
	ctx context.Context,
	plan *cntlrPlan,
) (bool, error) {
	dms, err := s.dm.List(ctx)
	if err != nil {
		return false, err
	}
	for name := range dms {
		parsed, ok := common.ParseDmName(name)
		if ok && stackKinds[parsed.Kind] &&
			parsed.ClusterId == plan.clusterId &&
			parsed.NodeId == plan.cnId && len(parsed.Ids) > 0 &&
			parsed.Ids[0] == plan.spId {
			return true, nil
		}
	}
	walk, err := s.md.Walk(ctx)
	if err != nil {
		return false, err
	}
	for _, gp := range plan.grps {
		if !gp.raid1 {
			continue
		}
		detail, err := s.md.Detail(ctx, walk, gp.memberNames())
		if err != nil {
			return false, err
		}
		if detail != nil && detail.Exists {
			return true, nil
		}
	}
	return false, nil
}

// termLeftRows are the rows a primary converge reports when its standby
// sweep left an earlier term's stack (CN35): an error row for each slice's
// pool, so the primary reads unhealthy and settling, and the failover can
// move the role to a clean standby (dnv-worker.md HL2, AR5).
func termLeftRows(st *cntlrState, plan *cntlrPlan, info *pb.CntlrInfo) {
	details := fmt.Sprintf("a stack of the earlier term %d is not removed "+
		"yet; the role's term is %d", st.stackTerm,
		plan.cntlr.GetPrimaryRevision())
	for _, sp := range plan.slices {
		info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
			resKeyOf(resKeyPoolFmt, sp.sliceId), sp.poolFinalName, details)
	}
}

// standbyPlanOf is plan's request with the cntlr a standby, on plan's walk:
// the plan of the sweep that removes an earlier term's stack.
func standbyPlanOf(nf *common.NameFmt, plan *cntlrPlan) *cntlrPlan {
	req := proto.Clone(plan.req).(*pb.SyncupCntlrRequest)
	req.Cntlr.Primary = false
	standby := newCntlrPlan(nf, req)
	standby.walk = plan.walk
	return standby
}

// stackKinds are the device-mapper kinds of a cntlr's primary stack, which
// a standby never holds: the pool concats, the pool, the thin volumes, the
// raid0s and a RedundNone group's linear.
var stackKinds = map[common.DmKind]bool{
	common.DmKindCnPoolMeta:  true,
	common.DmKindCnPoolData:  true,
	common.DmKindCnPoolFinal: true,
	common.DmKindCnThinDev:   true,
	common.DmKindCnRaid0:     true,
	common.DmKindCnGrp:       true,
}

// stackLeft says whether a sweep's leftovers name a piece of a primary
// stack: an md array, or a device-mapper device of stackKinds.
func stackLeft(res *agent.SweepResult) bool {
	for _, left := range res.Leftovers() {
		kind, name, ok := strings.Cut(left, ":")
		if !ok {
			continue
		}
		if kind == agent.LeftoverKindMd {
			return true
		}
		if kind != agent.LeftoverKindDm {
			continue
		}
		if parsed, ok := common.ParseDmName(name); ok &&
			stackKinds[parsed.Kind] {
			return true
		}
	}
	return false
}

// noteStandbySweep is a standby's converge's or read's step after its
// sweep (CN35): the report says whether the sweep left a stack, and a
// converge whose sweep was clean forgets the term the stack was built
// under, as no stack is left.
func (s *CnAgentServer) noteStandbySweep(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
	res *agent.SweepResult,
	converge bool,
) {
	if plan.primary {
		return
	}
	info.StackLeft = stackLeft(res)
	if converge && res.Clean() && (st.stackTerm != 0 || !st.stackTermSaved) {
		s.saveStackTerm(ctx, st, 0)
	}
}

// saveStackTerm sets the term the cntlr's stack was built under and writes
// it into its file in the local store, or removes the file for none
// (architecture.md, Agent local-store paths). A failure is logged and keeps
// the term in memory, with the file marked unsaved, which the next primary
// converge, or standby converge for none, writes again (saveTermAgain).
func (s *CnAgentServer) saveStackTerm(
	ctx context.Context,
	st *cntlrState,
	term uint64,
) {
	st.stackTerm = term
	req := st.loadReq()
	ptr := req.GetCntlrPointer()
	path := s.nf.LocalStackTermPath(req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())
	var err error
	if term == 0 {
		err = s.store.Remove(ctx, path)
	} else {
		err = s.store.Save(ctx, path, &pb.StackTerm{
			ClusterId:       req.GetClusterId(),
			CnId:            req.GetCnId(),
			CntlrPointer:    ptr,
			PrimaryRevision: term,
		})
	}
	st.stackTermSaved = err == nil
	if err != nil {
		slog.ErrorContext(ctx, "saving the stack's term failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
}

// saveTermAgain writes the term of a primary under its kept term again
// while its last save failed (saveStackTerm).
func (s *CnAgentServer) saveTermAgain(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) {
	if plan.primary && !st.stackTermSaved && st.stackTerm != 0 &&
		st.stackTerm == plan.cntlr.GetPrimaryRevision() {
		s.saveStackTerm(ctx, st, st.stackTerm)
	}
}

// loadStackTerms reads the term files of the local store at the agent's
// start into the cntlrs they name (CN2), with the torn marks' rules for a
// file whose cntlr is not loaded (loadTornMarks): deleted as an orphan,
// unless a CN or cntlr file that did not load may name its cntlr. A file
// that does not load is logged and leaves the cntlr with no term
// (termStep).
func (s *CnAgentServer) loadStackTerms(
	ctx context.Context,
	paths []string,
	unreadParent func(clusterId, cnId uint64) bool,
	unreadCntlr bool,
) {
	var orphans []string
	for _, path := range paths {
		term := &pb.StackTerm{}
		if err := s.store.Load(ctx, path, term); err != nil {
			slog.ErrorContext(ctx, "skipping an unreadable stack term file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		if unreadParent(term.GetClusterId(), term.GetCnId()) {
			continue
		}
		ptr := term.GetCntlrPointer()
		st := s.getCntlr(cntlrKey(term.GetClusterId(), term.GetCnId(),
			ptr.GetSpId(), ptr.GetCntlrId()))
		if st == nil && unreadCntlr {
			cn := s.getCn(cnKey(term.GetClusterId(), term.GetCnId()))
			if cn != nil && pointerKnown(cn.loadReq(), ptr) {
				continue
			}
		}
		if st == nil {
			orphans = append(orphans, path)
			continue
		}
		st.stackTerm = term.GetPrimaryRevision()
	}
	if len(orphans) > 0 {
		if err := s.store.Remove(ctx, orphans...); err != nil {
			slog.ErrorContext(ctx, "removing orphan stack term files failed",
				slog.String("error", err.Error()))
		}
	}
}

// termWhat names the cntlr in a read's verdict on its term (termFailure).
func termWhat(plan *cntlrPlan) string {
	return fmt.Sprintf("cntlr %d of pool %d", plan.cntlrId, plan.spId)
}

// termFailure is what a primary's read names its cntlr with while the term
// its stack was built under is not its request's (CN35): the converge's
// standby sweep would act on it.
func termFailure(st *cntlrState, plan *cntlrPlan) error {
	return rowFailure(errRowDiffers, fmt.Sprintf(
		"the stack is of term %d, the role's is %d", st.stackTerm,
		plan.cntlr.GetPrimaryRevision()))
}

// msgTermTeardownLeft is the Warn record of a primary converge under a new
// term whose standby sweep left something of the earlier term's stack
// (CN35): the pass serves nothing.
const msgTermTeardownLeft = "a stack of an earlier term is not removed yet; nothing served"
