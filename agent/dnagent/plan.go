package dnagent

import (
	"fmt"
	"sort"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ResTracker keys of the per-side resources.
const (
	resKeySideDev       = "side_dev"
	resKeyDmErrorFmt    = "dm_error/%016x"
	resKeyDmLinearFmt   = "dm_linear/%016x"
	resKeyNvmeofFmt     = "nvmeof/%016x"
	resKeyMigrSrcDm     = "migr_src_dm"
	resKeyMigrSrcNvmeof = "migr_src_nvmeof"
	resKeyMigrDstTarget = "migr_dst_target"
	resKeyMigrDstClone  = "migr_dst_clone"
)

// details strings of the §9.4 side-provisioning protocol. The bits themselves
// live in the side's on-disk allocation record ([D13]); these are only what a
// side reports about them.
const (
	// zeroingDetailsFmt is the RES_STATUS_PROVISIONING progress detail: k of
	// n logical extents zeroed. PROVISIONING means healthy / not ready / no
	// action, and never feeds err_epoch.
	zeroingDetailsFmt = "zeroing %d/%d"
	// tagNotZeroed is the RES_STATUS_ERROR detail for a request that claims
	// provisioned = true over a side whose bits are incomplete: the agent
	// trusts its own bits over the flag and refuses the exports.
	tagNotZeroed = "not zeroed"
	// tagRecordMissing is the RES_STATUS_ERROR detail for provisioned = true
	// with no allocation record: the data is gone (lost or foreign disk), and
	// re-allocating would present a zeroed impostor as the data-bearing leg.
	tagRecordMissing = "record missing"
	// tagProvisioningWait is what the rows above the side device report while
	// the side itself is still provisioning: one cause is reported once, on
	// side_dev_info, instead of multiplying err_epoch churn across every
	// per-CN stack.
	tagProvisioningWait = "side provisioning"
)

// nsModel is the attr_model every DN side export presents (§3.1).
const nsModel = "dnv"

// sideNsid is the single namespace id both sides of a leg export (§3.1).
const sideNsid = 1

// sidePlan is the desired state of one side, derived from its
// SyncupSideRequest and its DN's extent size. It is a pure function of the
// request: the same plan drives the converge pass and the read-only probe.
type sidePlan struct {
	nf *common.NameFmt

	clusterId uint64
	dnId      uint64
	spId      uint64
	legId     uint64
	sideId    uint64

	conf    *pb.SyncupSideRequest_SideConf
	migrSrc *pb.SyncupSideRequest_MigrSrcConf
	migrDst *pb.SyncupSideRequest_MigrDstConf
	level   pb.SpLevel

	// provisioned is side_conf.provisioned: the CP's gate on exporting this
	// side. It is a gate, never evidence — the agent always trusts its own
	// zeroed_bits over the flag, because the disk is authoritative ([D13]).
	provisioned bool
	// migrSrcRaw is migr_src_conf exactly as received, and migrSrcDeferred
	// says the destination has not provisioned yet. In that case migrSrc above
	// is nil, because `dst_provisioned = false` is **exactly
	// equivalent** to "no migr_src_conf at all" (§11.2): fencing at migration
	// start would leave the leg with no serving path for the whole zeroing
	// window. The only visible difference is that the would-be migr_src_info
	// rows report PROVISIONING.
	migrSrcRaw      *pb.SyncupSideRequest_MigrSrcConf
	migrSrcDeferred bool

	sideDevName string
	sideDevPath string
	extentSize  uint64
	sectors     uint64

	primaryCnId uint64
	cnIds       []uint64

	// sp_level gates (DN11). Levels are desired state: raising one tears
	// layers down, lowering it rebuilds them.
	wantDm     bool
	wantExport bool
	wantMigr   bool
}

func newSidePlan(
	nf *common.NameFmt,
	req *pb.SyncupSideRequest,
	extentSize uint64,
) *sidePlan {
	ptr := req.GetSidePointer()
	conf := req.GetSideConf()
	level := conf.GetSpLevel()

	p := &sidePlan{
		nf:          nf,
		clusterId:   req.GetClusterId(),
		dnId:        req.GetDnId(),
		spId:        ptr.GetSpId(),
		legId:       ptr.GetLegId(),
		sideId:      ptr.GetSideId(),
		conf:        conf,
		migrSrc:     req.GetMigrSrcConf(),
		migrDst:     req.GetMigrDstConf(),
		level:       level,
		primaryCnId: conf.GetPrimaryCnId(),
		extentSize:  extentSize,
		sectors:     conf.GetExtCnt() * extentSize / agent.SectorSize,
		wantDm:      level < pb.SpLevel_SP_LEVEL_DISABLE,
		wantExport:  level < pb.SpLevel_SP_LEVEL_NO_SIDE,
	}
	p.wantMigr = p.migrDst != nil &&
		level < pb.SpLevel_SP_LEVEL_NO_MIGRATION
	p.provisioned = conf.GetProvisioned()
	p.migrSrcRaw = req.GetMigrSrcConf()
	if p.migrSrcRaw != nil && !p.migrSrcRaw.GetDstProvisioned() {
		// Nil-ing the field is the whole implementation of the equivalence:
		// linearBacking, preFenceBacking, anaGrpId, ensureCnDm's fence,
		// convergeSide's ANA handover and teardownForbidden's applied-role diff
		// all key off migrSrc, so the side keeps serving exactly as it did
		// before the migration was created.
		p.migrSrcDeferred = true
		p.migrSrc = nil
	}
	p.sideDevName = nf.DnSideName(p.clusterId, p.dnId, p.spId, p.sideId)
	p.sideDevPath = nf.DmPath(p.sideDevName)
	p.cnIds = cnIdsOf(conf)
	return p
}

// cnIdsOf lists the CNs a side exports to: the primary first, then the
// standbys ascending, deduplicated.
func cnIdsOf(conf *pb.SyncupSideRequest_SideConf) []uint64 {
	standbys := make([]uint64, 0, len(conf.GetStandbyIdList()))
	seen := map[uint64]struct{}{conf.GetPrimaryCnId(): {}}
	for _, cnId := range conf.GetStandbyIdList() {
		if _, ok := seen[cnId]; ok {
			continue
		}
		seen[cnId] = struct{}{}
		standbys = append(standbys, cnId)
	}
	sort.Slice(standbys, func(i, j int) bool {
		return standbys[i] < standbys[j]
	})
	return append([]uint64{conf.GetPrimaryCnId()}, standbys...)
}

func (p *sidePlan) errName(cnId uint64) string {
	return p.nf.DnErrorName(p.clusterId, p.dnId, p.spId, p.sideId, cnId)
}

func (p *sidePlan) linearName(cnId uint64) string {
	return p.nf.DnLinearName(p.clusterId, p.dnId, p.spId, p.sideId, cnId)
}

func (p *sidePlan) sideNqn(cnId uint64) string {
	return p.nf.SideToCnNqn(p.clusterId, p.spId, p.legId, cnId)
}

func (p *sidePlan) cnHostNqn(cnId uint64) string {
	return p.nf.CnHostNqn(p.clusterId, cnId)
}

// migrSrcName / migrSrcNqn describe the export this side publishes while it
// is a migration source.
func (p *sidePlan) migrSrcName() string {
	return p.nf.DnMigrSrcName(
		p.clusterId, p.dnId, p.spId, p.migrSrc.GetMigrId())
}

func (p *sidePlan) migrSrcNqn() string {
	return p.nf.MigrSrcNqn(
		p.clusterId, p.dnId, p.spId, p.migrSrc.GetMigrId())
}

// srcNqnOfDst is the source-side subsystem this destination side connects to
// — on the *source* DN, so it is keyed by migr_dst_conf.src_dn_id.
func (p *sidePlan) srcNqnOfDst() string {
	return p.nf.MigrSrcNqn(p.clusterId, p.migrDst.GetSrcDnId(), p.spId,
		p.migrDst.GetMigrId())
}

func (p *sidePlan) dnHostNqn() string {
	return p.nf.DnHostNqn(p.clusterId, p.dnId)
}

func (p *sidePlan) migrFinalName() string {
	return p.nf.DnMigrFinalName(
		p.clusterId, p.dnId, p.spId, p.migrDst.GetMigrId())
}

// migrMetaDmName / migrMetaDmPath name the wrapper dm-linear over this
// migration's dm-clone metadata slot ([P6]).
func (p *sidePlan) migrMetaDmName() string {
	return p.nf.DnMigrMetaDmName(
		p.clusterId, p.dnId, p.spId, p.migrDst.GetMigrId())
}

func (p *sidePlan) migrMetaDmPath() string {
	return p.nf.DmPath(p.migrMetaDmName())
}

// nsIdentity is the deterministic identity both sides of a leg present, so
// the CN kernel merges them into one multipath namespace (§3.1, [D1]).
func (p *sidePlan) nsIdentity() (string, string) {
	return common.DnNsIdentity(p.clusterId, p.spId, p.legId)
}

func (p *sidePlan) cntlidRange() (uint32, uint32) {
	min := uint32(common.DnCntlidSlotBase) +
		p.conf.GetCntlidSlot()*uint32(common.DnCntlidSlotStep)
	return min, min + uint32(common.DnCntlidSlotStep)
}

// linearBacking is the table target of one CN's dm-linear: the LV for the
// primary CN, the dm-error device for standbys — and, on a migration
// destination, the dm-clone for the primary once it is live and dm-error for
// everyone until then (§3.1, §11.2).
//
// A migration *source* fences every per-CN linear — the primary's included —
// onto its dm-error ([D12], §11.2 src step 2). Its namespaces are already
// AnaGrpIdInaccessible by then, so this only affects stragglers, and erroring
// a straggler write is the safe outcome: a suspended device would defer it
// and replay it at resume, possibly after hydration copied that region.
func (p *sidePlan) linearBacking(cnId uint64, cloneLive bool) string {
	if p.migrSrc != nil {
		return p.nf.DmPath(p.errName(cnId))
	}
	if cnId != p.primaryCnId {
		return p.nf.DmPath(p.errName(cnId))
	}
	if p.migrDst != nil {
		if !cloneLive {
			return p.nf.DmPath(p.errName(cnId))
		}
		return p.nf.DmPath(p.migrFinalName())
	}
	return p.sideDevPath
}

// preFenceBacking is where a per-CN dm-linear sits during the §11.2 src
// grace window: still exactly where it sat before the migration started,
// because the fence suspends the device in place and only swaps the table at
// the end of the window. Computing it as "linearBacking with no migr_src"
// keeps the two definitions from drifting, and stays correct for a side that
// is somehow both a source and a destination.
func (p *sidePlan) preFenceBacking(cnId uint64, cloneLive bool) string {
	unfenced := *p
	unfenced.migrSrc = nil
	return unfenced.linearBacking(cnId, cloneLive)
}

// anaGrpId is the fixed ANA group a CN's namespace belongs to ([D4]).
func (p *sidePlan) anaGrpId(cnId uint64, cloneLive bool) int {
	if p.migrSrc != nil {
		// The source side hands IO over to the destination (§11.2 src
		// step 1).
		return common.AnaGrpIdInaccessible
	}
	if p.migrDst != nil && !cloneLive {
		// Destination stacks start on dm-error, all inaccessible
		// (§11.2 dst step 1; also the SP_LEVEL_NO_MIGRATION state).
		return common.AnaGrpIdInaccessible
	}
	if cnId == p.primaryCnId {
		return common.AnaGrpIdOptimized
	}
	return common.AnaGrpIdNonOptimized
}

func resKeyOf(format string, cnId uint64) string {
	return fmt.Sprintf(format, cnId)
}
