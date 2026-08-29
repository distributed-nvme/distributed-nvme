package common

import (
	"fmt"
	"hash/fnv"
)

const (
	dmKindDnError     = 0x0
	dmKindDnLinear    = 0x1
	dmKindDnMigrSrc   = 0x2
	dmKindDnMigrFinal = 0x3

	dmKindCnPoolMeta   = 0x0
	dmKindCnPoolData   = 0x1
	dmKindCnPoolFinal  = 0x2
	dmKindCnThinDev    = 0x3
	dmKindCnRaid0      = 0x4
	dmKindCnError      = 0x5
	dmKindCnNsDev      = 0x6
	dmKindCnCloneFinal = 0x7
	dmKindCnXferFinal  = 0x8

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

	migrPvName  = "migr-pv"
	tmpFileName = "tmp-file"
)

type NameFmt struct {
	dmPrefix        string
	nqnPrefix       string
	tmpfsPrefix     string
	dnVgPrefix      string
	cloneVgPrefix   string
	migrVgPrefix    string
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
		dnVgPrefix:      DefaultDnVgPrefix,
		cloneVgPrefix:   DefaultCloneVgPrefix,
		migrVgPrefix:    DefaultMigrVgPrefix,
		localStorPrefix: localStorPrefix,
	}
}

func (nf *NameFmt) DnVgName(
	clusterId uint64,
	dnId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x",
		nf.dnVgPrefix,
		clusterId,
		dnId,
	)
}

func (nf *NameFmt) DnLvName(
	spId uint64,
	sideId uint64,
) string {
	return fmt.Sprintf(
		"%016x-%016x",
		spId,
		sideId,
	)
}

func (nf *NameFmt) DnLvPath(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
) string {
	vgName := nf.DnVgName(clusterId, dnId)
	lvName := nf.DnLvName(spId, sideId)
	return fmt.Sprintf(
		"/dev/%s/%s",
		vgName,
		lvName,
	)
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

func (nf *NameFmt) DnMigrPvName() string {
	return migrPvName
}

func (nf *NameFmt) DnMigrPvPath(
	clusterId uint64,
	dnId uint64,
) string {
	return fmt.Sprintf(
		"/dev/%s/%s",
		nf.DnVgName(clusterId, dnId),
		nf.DnMigrPvName(),
	)
}

func (nf *NameFmt) DnMigrVgName(
	clusterId uint64,
	dnId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x",
		nf.migrVgPrefix,
		clusterId,
		dnId,
	)
}

func (nf *NameFmt) DnMigrMetaName(
	spId uint64,
	migrId uint64,
) string {
	return fmt.Sprintf(
		"%016x-%016x",
		spId,
		migrId,
	)
}

func (nf *NameFmt) DnMigrMetaPath(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	migrId uint64,
) string {
	return fmt.Sprintf(
		"/dev/%s/%s",
		nf.DnMigrVgName(clusterId, dnId),
		nf.DnMigrMetaName(spId, migrId),
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

func getShortId(clusterId, nodeId uint64) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%016x%016x", clusterId, nodeId)
	return h.Sum64() & 0x0000FFFFFFFFFFFF
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
		"%012x%016x%02x%02x",
		shortId,
		spId,
		sliceIdx,
		grpIdx,
	)
}

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
		"%016x-%02x-%02x",
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
	tdId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%01x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		cnId,
		dmKindCnNsDev,
		spId,
		tdId,
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

func (nf *NameFmt) CnCloneVgName(
	clusterId uint64,
	cnId uint64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x",
		nf.cloneVgPrefix,
		clusterId,
		cnId,
	)
}

func (nf *NameFmt) CnCloneMetaName(
	spId uint64,
	cloneId uint64,
) string {
	return fmt.Sprintf(
		"%016x-%016x",
		spId,
		cloneId,
	)
}

func (nf *NameFmt) CnCloneMetaPath(
	clusterId uint64,
	cnId uint64,
	spId uint64,
	cloneId uint64,
) string {
	return fmt.Sprintf(
		"/dev/%s/%s",
		nf.CnCloneVgName(clusterId, cnId),
		nf.CnCloneMetaName(spId, cloneId),
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
	dnId uint64,
	spId uint64,
	xferId uint64,
) string {
	return fmt.Sprintf(
		"%s:%01x:%016x:%016x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindXfer,
		clusterId,
		dnId,
		spId,
		xferId,
	)
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
