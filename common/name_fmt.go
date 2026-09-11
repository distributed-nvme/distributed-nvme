package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
)

const (
	dmKindDnError     = 0x0
	dmKindDnLinear    = 0x1
	dmKindDnMigrSrc   = 0x2
	dmKindDnMigrFinal = 0x3
	dmKindDnSide      = 0x4
	dmKindDnMigrMeta  = 0x5

	dmKindCnPoolMeta   = 0x0
	dmKindCnPoolData   = 0x1
	dmKindCnPoolFinal  = 0x2
	dmKindCnThinDev    = 0x3
	dmKindCnRaid0      = 0x4
	dmKindCnError      = 0x5
	dmKindCnNsDev      = 0x6
	dmKindCnCloneFinal = 0x7
	dmKindCnXferFinal  = 0x8
	dmKindCnLeg        = 0x9
	dmKindCnGrp        = 0xa
	dmKindCnCloneMeta  = 0xb

	nqnKindDnHost   = 0x0
	nqnKindCnHost   = 0x1
	nqnKindSideToCn = 0x2
	nqnKindMigrSrc  = 0x3
	nqnKindXfer     = 0x4

	localStorKindDn      = "dn"
	localStorKindCn      = "cn"
	localStorKindSide    = "side"
	localStorKindCntlr   = "cntlr"
	localStorKindMigrBm  = "migr-bm"
	localStorKindCloneBm = "clone-bm"

	tmpFileName = "tmp-file"
)

type NameFmt struct {
	dmPrefix        string
	nqnPrefix       string
	tmpfsPrefix     string
	localStorPrefix string
}

// NewNameFmt builds the process-wide NameFmt from the prefixes in
// constants.go (architecture.md §4). localStorPrefix comes from the agent's
// --local-store flag; an empty string selects DefaultLocalStorPrefix.
func NewNameFmt(localStorPrefix string) *NameFmt {
	if localStorPrefix == "" {
		localStorPrefix = DefaultLocalStorPrefix
	}
	return &NameFmt{
		dmPrefix:        DmPrefix,
		nqnPrefix:       NqnPrefix,
		tmpfsPrefix:     DefaultTmpfsPrefix,
		localStorPrefix: localStorPrefix,
	}
}

func (nf *NameFmt) DnErrorName(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnError,
		spId,
		sideId,
		cnId,
	)
}

func (nf *NameFmt) DnLinearName(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnLinear,
		spId,
		sideId,
		cnId,
	)
}

// DnSideName is the side's data device ([D13]): one dm-linear concatenating
// the extent runs the on-disk volume table allocated to the side. It is the
// successor of the LVM logical volume the side used to sit on.
func (nf *NameFmt) DnSideName(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnSide,
		spId,
		sideId,
	)
}

// DnMigrMetaDmName wraps one migration's dm-clone metadata slot: the dm-clone
// target reads its metadata device from sector 0 and takes no offset
// argument, so the slot needs a dm-linear of its own ([P6]).
func (nf *NameFmt) DnMigrMetaDmName(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	migrId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnMigrMeta,
		spId,
		migrId,
	)
}

func (nf *NameFmt) DnMigrSrcName(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	migrId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnMigrSrc,
		spId,
		migrId,
	)
}

func (nf *NameFmt) DnMigrFinalName(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	migrId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnMigrFinal,
		spId,
		migrId,
	)
}

func getShortId(clusterId, nodeId uint64) uint32 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%016x%016x", clusterId, nodeId)
	return uint32(h.Sum64())
}

func (nf *NameFmt) CnMdDevName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	sliceIdx uint32,
	grpIdx uint32,
	isMeta bool,
) string {
	shortId := getShortId(clusterId, cnId)
	if isMeta {
		sliceIdx |= 0x80
	}
	return fmt.Sprintf(
		"%08x%016x%02x%02x",
		shortId,
		spId,
		sliceIdx,
		grpIdx,
	)
}

// CnMdArrayName is the array's superblock name (mdadm --name). The fixed
// "dnv-" prefix is what the udev guard matches (ENV{MD_NAME}=="dnv-*",
// architecture.md §4.3 / Appendix A); 26 chars, within mdadm's 32-byte limit.
func (nf *NameFmt) CnMdArrayName(
	spId uint64,
	sliceIdx uint32,
	grpIdx uint32,
	isMeta bool,
) string {
	if isMeta {
		sliceIdx |= 0x80
	}
	return fmt.Sprintf(
		"%s-%016x-%02x-%02x",
		DnvPrefix,
		spId,
		sliceIdx,
		grpIdx,
	)
}

func (nf *NameFmt) CnPoolMetaName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	sliceId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnPoolMeta,
		spId,
		sliceId,
	)
}

func (nf *NameFmt) CnPoolDataName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	sliceId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnPoolData,
		spId,
		sliceId,
	)
}

func (nf *NameFmt) CnPoolFinalName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	sliceId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnPoolFinal,
		spId,
		sliceId,
	)
}

func (nf *NameFmt) CnThinDevName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	tdId uint64,
	sliceId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnThinDev,
		spId,
		tdId,
		sliceId,
	)
}

func (nf *NameFmt) CnRaid0Name(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	tdId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnRaid0,
		spId,
		tdId,
	)
}

func (nf *NameFmt) CnErrorName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	tdId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnError,
		spId,
		tdId,
	)
}

func (nf *NameFmt) CnNsDevName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	nsId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnNsDev,
		spId,
		nsId,
	)
}

// CnLegName is the cn-local leg wrapper of architecture.md §3.3 step 1
// ([D1]): one dm-linear over the leg's single nvme multipath namespace
// device, kept as the leg-level indirection point (what a teardown reloads
// onto an error target, and what md/groups consume as the member device).
func (nf *NameFmt) CnLegName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	legId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnLeg,
		spId,
		legId,
	)
}

// CnGrpName is a RedundNone group device (§3.3 step 2): a dm-linear over the
// single leg's data region. RedundMdRaid1 groups use the md names of §4.3
// instead and have no dm name.
func (nf *NameFmt) CnGrpName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	grpId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnGrp,
		spId,
		grpId,
	)
}

func (nf *NameFmt) CnTmpfsPath(
	clusterId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s/%016x-%016x",
		nf.tmpfsPrefix,
		clusterId,
		cnId,
	)
}

func (nf *NameFmt) CnTmpFileName() string {
	return tmpFileName
}

func (nf *NameFmt) CnTmpFilePath(
	clusterId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s/%s",
		nf.CnTmpfsPath(clusterId, cnId),
		nf.CnTmpFileName(),
	)
}

// CnCloneMetaDmName wraps one clone's dm-clone metadata slot in the CN
// clone-metadata arena ([D14], cnagent.md CN18): the slot is a unit range of
// the single loop device over CnTmpFilePath, and the dm-clone target reads its
// metadata device from sector 0 and takes no offset argument, so the slot needs
// a dm-linear of its own — exactly the reason DnMigrMetaDmName exists on the dn
// side. The wrapper's own table is the allocator's registry:
// there is no on-file allocation table.
func (nf *NameFmt) CnCloneMetaDmName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	cloneId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnCloneMeta,
		spId,
		cloneId,
	)
}

// CnCloneMetaDmPrefix is the `dmsetup ls` filter that enumerates this CN's
// clone-metadata wrappers. Because the dm tables ARE the allocator's registry
// (CN18), rebuilding the used-unit map means listing every kind-`b`
// device of this cluster and cn — so the prefix has to be built from the same
// nf.dmPrefix and the same kind constant as CnCloneMetaDmName, never from an
// ad-hoc literal in the role package.
func (nf *NameFmt) CnCloneMetaDmPrefix(
	clusterId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnCloneMeta,
	)
}

func (nf *NameFmt) CnCloneFinalName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	cloneId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnCloneFinal,
		spId,
		cloneId,
	)
}

func (nf *NameFmt) CnXferFinalName(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	xferId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnXferFinal,
		spId,
		xferId,
	)
}

func (nf *NameFmt) DmPath(name string) string {
	return fmt.Sprintf(
		"/dev/mapper/%s",
		name,
	)
}

func (nf *NameFmt) MdPath(name string) string {
	return fmt.Sprintf(
		"/dev/md/%s",
		name,
	)
}

func (nf *NameFmt) DnHostNqn(
	clusterId uint64,
	dnId uint64,
) string {
	return fmt.Sprintf(
		"%s:%01x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindDnHost,
		clusterId,
		dnId,
	)
}

func (nf *NameFmt) CnHostNqn(
	clusterId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s:%01x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindCnHost,
		clusterId,
		cnId,
	)
}

// SideToCnNqn is keyed by leg_id, not side_id, and carries no dn_id: both
// sides of a migrating leg export this same subsystem NQN from their two DNs,
// so the CN's kernel aggregates them into one nvme multipath namespace and
// ANA picks the live path (architecture.md §4.4, §11.2).
func (nf *NameFmt) SideToCnNqn(
	clusterId uint64,
	spId uint64,
	legId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s:%01x:%016x:%016x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindSideToCn,
		clusterId,
		spId,
		legId,
		cnId,
	)
}

func (nf *NameFmt) MigrSrcNqn(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	migrId uint64,
) string {
	return fmt.Sprintf(
		"%s:%01x:%016x:%016x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindMigrSrc,
		clusterId,
		dnId,
		spId,
		migrId,
	)
}

func (nf *NameFmt) XferNqn(
	clusterId uint64,
	spId uint64,
	xferId uint64,
) string {
	return fmt.Sprintf(
		"%s:%01x:%016x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindXfer,
		clusterId,
		spId,
		xferId,
	)
}

// DnNsIdentity derives the deterministic namespace identity that both sides
// of a leg MUST present identically (architecture.md §3.1): 16 bytes of
// sha256("dnv-ns:{cluster:%016x}:{sp:%016x}:{leg:%016x}"), rendered as an
// RFC-4122-shaped uuid string and a 32-hex-digit nguid.
func DnNsIdentity(clusterId, spId, legId uint64) (uuid string, nguid string) {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"dnv-ns:%016x:%016x:%016x", clusterId, spId, legId,
	)))
	nguid = hex.EncodeToString(sum[:16])
	uuid = fmt.Sprintf("%s-%s-%s-%s-%s",
		nguid[0:8], nguid[8:12], nguid[12:16], nguid[16:20], nguid[20:32])
	return uuid, nguid
}

func (nf *NameFmt) LocalDnPath(
	clusterId uint64,
	dnId uint64,
) string {
	return fmt.Sprintf(
		"%s/%s-%016x-%016x",
		nf.localStorPrefix,
		localStorKindDn,
		clusterId,
		dnId,
	)
}

func (nf *NameFmt) LocalSidePath(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
) string {
	return fmt.Sprintf(
		"%s/%s-%016x-%016x-%016x-%016x",
		nf.localStorPrefix,
		localStorKindSide,
		clusterId,
		dnId,
		spId,
		sideId,
	)
}

func (nf *NameFmt) LocalCnPath(
	clusterId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s/%s-%016x-%016x",
		nf.localStorPrefix,
		localStorKindCn,
		clusterId,
		cnId,
	)
}

func (nf *NameFmt) LocalCntlrPath(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	cntlrId uint64,
) string {
	return fmt.Sprintf(
		"%s/%s-%016x-%016x-%016x-%016x",
		nf.localStorPrefix,
		localStorKindCntlr,
		clusterId,
		cnId,
		spId,
		cntlrId,
	)
}

func (nf *NameFmt) LocalMigrBmPath(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	migrId uint64,
	bmIdx uint32,
) string {
	return fmt.Sprintf(
		"%s/%s-%016x-%016x-%016x-%016x-%02x",
		nf.localStorPrefix,
		localStorKindMigrBm,
		clusterId,
		dnId,
		spId,
		migrId,
		bmIdx,
	)
}

func (nf *NameFmt) LocalCloneBmPath(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	cloneId uint64,
	bmIdx uint32,
) string {
	return fmt.Sprintf(
		"%s/%s-%016x-%016x-%016x-%016x-%02x",
		nf.localStorPrefix,
		localStorKindCloneBm,
		clusterId,
		cnId,
		spId,
		cloneId,
		bmIdx,
	)
}

// NvmeHostId derives the deterministic NVMe host id that every dnv
// `nvme connect` MUST carry alongside its dnv hostnqn: 16 bytes of
// sha256("dnv-hostid:{hostnqn}"), rendered RFC-4122-shaped, exactly like
// DnNsIdentity.
//
// The kernel keeps a strict 1:1 hostnqn<->hostid mapping (nvmf_host_add: a
// second hostnqn under an already-known hostid is rejected with EINVAL,
// "found same hostid ... but different hostnqn"), and nvme-cli fills an
// omitted --hostid from the node-wide /etc/nvme/hostid. dnv picks its own
// per-identity hostnqn, so an implicit hostid makes every connect a hostage
// to whatever else on the node connected first — an unrelated NVMe-oF mount
// under /etc/nvme/hostnqn is enough to make dnv's first connect fail EINVAL,
// and two dnv identities on one node (a migration dst that is also a leg
// target, or the integration suite's emulated CNs) collide with each other.
// Deriving the id from the nqn makes the mapping hold by construction and
// stay stable across reconnects and restarts.
func NvmeHostId(hostNqn string) string {
	sum := sha256.Sum256([]byte("dnv-hostid:" + hostNqn))
	hexed := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32])
}
