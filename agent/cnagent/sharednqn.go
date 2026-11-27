package cnagent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN16 — a shared NQN
// ---------------------------------------------------------------------------

// markShared is CN16's test of a shared NQN (cnagent.md CN16, A shared
// NQN): it marks each host-facing subsystem of plan whose NQN the stored
// request of another cntlr of this agent also names, with the text its row
// reads, which names the other cntlrs' pools. The kernel holds one subsystem
// per NQN, so no cntlr changes it while two name it, and neither takes over
// the subsystem the other may have built. A request is stored before its
// converge acts (storeReq, putCntlr), so of two cntlrs that come at once at
// least one marks the other's NQN, and at most one builds it.
func (s *CnAgentServer) markShared(plan *cntlrPlan) {
	if len(plan.subsystems) == 0 {
		return
	}
	self := cntlrKey(plan.clusterId, plan.cnId, plan.spId, plan.cntlrId)
	others := make(map[string][]string)
	for _, key := range s.allCntlrKeys() {
		if key == self {
			continue
		}
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		req := st.loadReq()
		ptr := req.GetCntlrPointer()
		for nqn := range req.GetNqnToSubsystem() {
			others[nqn] = append(others[nqn], fmt.Sprintf(
				"pool %d (cntlr %d)", ptr.GetSpId(), ptr.GetCntlrId()))
		}
	}
	for _, ssp := range plan.subsystems {
		names := others[ssp.nqn]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		ssp.shared = fmt.Sprintf("subsystem %s is also named by %s on this "+
			"node, so no cntlr of the node changes it", ssp.nqn,
			strings.Join(names, ", "))
	}
}

// nqnShared says whether the stored requests of two cntlrs or more of this
// agent name nqn, which the sweep then never attributes to a pool (CN16, A
// shared NQN; nvmetOwner).
func (s *CnAgentServer) nqnShared(nqn string) bool {
	named := 0
	for _, key := range s.allCntlrKeys() {
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		if _, ok := st.loadReq().GetNqnToSubsystem()[nqn]; ok {
			named++
		}
	}
	return named > 1
}

// ownNs says whether the kernel namespace of np's NQN and index stands over
// np's own ns-dev: its device_path, read strictly, is the ns-dev's path. A
// read that does not answer says no.
func (s *CnAgentServer) ownNs(ctx context.Context, np *nsPlan) bool {
	path, ok, err := s.nvmet.NsDevicePath(ctx, np.ss.nqn, np.nsIdx)
	return err == nil && ok &&
		strings.TrimSpace(path) == s.nf.DmPath(np.devName)
}

// mayDarken says whether a pass may move np's namespace inaccessible: always,
// but under a shared NQN (CN16, A shared NQN), where only a namespace over
// np's own ns-dev moves, as a demoted cntlr's must, and never one over
// another pool's device. A read that does not answer moves nothing there.
func (s *CnAgentServer) mayDarken(ctx context.Context, np *nsPlan) bool {
	return np.ss.shared == "" || s.ownNs(ctx, np)
}

// markOwnShared reads, for each namespace of the shared ssp, whether the
// kernel namespace of its index stands over its own ns-dev (ownNs,
// nsPlan.ownShared), and decides whether the subsystem is safe in the pass:
// the kernel admits no host off the desired list (admitsOnlyDesired), as a
// list step would make it (CN33). A namespace of its own goes inaccessible
// at once while it is not safe, and one whose move fails is undarkened, as
// darkenSubsys leaves one. Reason: the list step that would cut a host taken
// off the list does not run while the NQN is shared, so such a host keeps
// its admission, and no namespace this cntlr serves may be optimized for it.
func (s *CnAgentServer) markOwnShared(
	ctx context.Context,
	plan *cntlrPlan,
	ssp *ssPlan,
) {
	for _, np := range ssp.namespaces {
		np.ownShared = s.ownNs(ctx, np)
	}
	ssp.safe = s.admitsOnlyDesired(ctx, s.subsysConf(plan, ssp))
}

// darkenOwnShared moves each namespace of the shared ssp that stands over
// its own ns-dev inaccessible while the subsystem is not safe in the pass
// (markOwnShared), marking one whose move fails undarkened.
func (s *CnAgentServer) darkenOwnShared(ctx context.Context, ssp *ssPlan) {
	if ssp.safe {
		return
	}
	for _, np := range ssp.namespaces {
		if np.ownShared && !s.moveDark(ctx, ssp.nqn, np.nsIdx) {
			np.undarkened = true
		}
	}
}

// reportSharedSubsys writes the row of a subsystem of a shared NQN, alike on
// a converge and a read (CN16, A shared NQN; CN28), with
// RES_REASON_SHARED_NQN. The cntlr that built it — a namespace of its own
// stands under it (nsPlan.ownShared) — reads RES_STATUS_PENDING, as it keeps
// the subsystem as built and its hosts keep their paths; so does a cntlr
// with no namespace under the NQN, which loses nothing. Any other reads
// RES_STATUS_ERROR, which sets its health epoch, so the worker's cntlr
// replacement moves it off the node (dnv-worker.md AR7). The namespaces of
// its own get the rows of any namespace (reportNamespaces, probeCntlr); the
// others get none.
func (s *CnAgentServer) reportSharedSubsys(
	t *agent.ResTracker,
	ssp *ssPlan,
	info *pb.CntlrInfo,
) {
	builder := false
	for _, np := range ssp.namespaces {
		builder = builder || np.ownShared
	}
	status, details := pb.ResStatus_RES_STATUS_ERROR,
		ssp.shared+"; this cntlr builds nothing of it"
	switch {
	case builder:
		status, details = pb.ResStatus_RES_STATUS_PENDING,
			ssp.shared+"; this cntlr built it and keeps it as built"
		if !ssp.safe {
			details += ", and the kernel admits a host off its list, so " +
				"its namespaces stay inaccessible"
		}
	case len(ssp.namespaces) == 0:
		status, details = pb.ResStatus_RES_STATUS_PENDING,
			ssp.shared+"; this cntlr has no namespace under it"
	}
	info.SsIdToSubsystem[ssp.ssId] = t.SetReason(
		resKeyOf(resKeySubsysFmt, ssp.ssId), ssp.nqn, status, details,
		pb.ResReason_RES_REASON_SHARED_NQN)
}

// foreignNs is the first namespace under the kernel subsystem of ssp's NQN
// whose device is an ns-dev of another pool of this node (CN16, A shared
// NQN): what a cntlr of that pool built while the NQN was shared, and left
// when it went. It returns the text that names it, empty when there is
// none, and every namespace index the kernel lists. A read that does not
// answer is an error.
func (s *CnAgentServer) foreignNs(
	ctx context.Context,
	plan *cntlrPlan,
	ssp *ssPlan,
) (string, []int, error) {
	nsIdxs, found, err := s.nvmet.ListNamespaces(ctx, ssp.nqn)
	if err != nil || !found {
		return "", nil, err
	}
	for _, nsIdx := range nsIdxs {
		path, ok, err := s.nvmet.NsDevicePath(ctx, ssp.nqn, nsIdx)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			continue
		}
		dn, ok := common.ParseDmName(strings.TrimPrefix(
			strings.TrimSpace(path), "/dev/mapper/"))
		if ok && dn.Kind == common.DmKindCnNsDev &&
			dn.ClusterId == plan.clusterId && dn.NodeId == plan.cnId &&
			dn.SpId() != plan.spId {
			return fmt.Sprintf("namespace %s stands over pool %d's "+
				"ns-dev %s", nsResName(ssp.nqn, nsIdx), dn.SpId(),
				strings.TrimSpace(path)), nsIdxs, nil
		}
	}
	return "", nsIdxs, nil
}

// replaceForeignSubsys removes the kernel subsystem of ssp's NQN before its
// list step when a namespace under it stands over another pool's ns-dev
// (foreignNs; CN16, A shared NQN), by removeOldSubsys, so the list step
// builds it again as this cntlr's. Reason: the other pool's serial, hosts
// and namespaces are not this pool's, the kernel refuses a new serial once
// a host has discovered the subsystem, and a list step that wrote this
// pool's hosts first would let them reach the other pool's volume. A read
// that does not answer, or a removal removeOldSubsys refuses, is the error
// the subsystem's row reads, and the pass leaves the subsystem as it is.
func (s *CnAgentServer) replaceForeignSubsys(
	ctx context.Context,
	plan *cntlrPlan,
	ssp *ssPlan,
) error {
	foreign, nsIdxs, err := s.foreignNs(ctx, plan, ssp)
	if err != nil {
		return fmt.Errorf("reading the namespaces of %s: %w", ssp.nqn, err)
	}
	if foreign == "" {
		return nil
	}
	return s.removeOldSubsys(ctx, ssp.nqn, foreign, nsIdxs)
}

// sharedOwnNs are the namespaces under the kernel subsystem of a host-facing
// NQN that is shared, or that another pool of this node holds, which the
// sweep of a pool removes (CN16, A shared NQN; CN21): each one whose device
// is an ns-dev of the pool that the chain removes (unwanted), what the
// pool's cntlr built while the NQN was shared and still wanted, or before it
// left the node. No other namespace and never the subsystem: it is no pool's,
// or the other pool's. A read that does not answer is a failure of the
// pass, and the namespace stays.
func (s *CnAgentServer) sharedOwnNs(
	ctx context.Context,
	nqn string,
	unwanted map[string]bool,
	res *agent.SweepResult,
) []cnNsRef {
	nsIdxs, found, err := s.nvmet.ListNamespaces(ctx, nqn)
	if err != nil {
		res.Fail("nvmet namespaces of "+nqn, err)
		return nil
	}
	if !found {
		return nil
	}
	var out []cnNsRef
	for _, nsIdx := range nsIdxs {
		if s.nsOverUnwanted(ctx, nqn, nsIdx, unwanted, res) {
			out = append(out, cnNsRef{nqn: nqn, nsid: nsIdx})
		}
	}
	return out
}

// nsOverUnwanted says whether the kernel namespace nsIdx of nqn stands over
// an ns-dev the sweep's chain removes (unwanted), read strictly. A read
// that does not answer is a failure of the pass, and says no.
func (s *CnAgentServer) nsOverUnwanted(
	ctx context.Context,
	nqn string,
	nsIdx int,
	unwanted map[string]bool,
	res *agent.SweepResult,
) bool {
	path, ok, err := s.nvmet.NsDevicePath(ctx, nqn, nsIdx)
	if err != nil {
		res.Fail(fmt.Sprintf("nvmet %s ns %d device_path", nqn, nsIdx), err)
		return false
	}
	name := strings.TrimPrefix(strings.TrimSpace(path), "/dev/mapper/")
	return ok && unwanted[name]
}
