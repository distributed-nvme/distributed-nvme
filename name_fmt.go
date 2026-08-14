const (
	dmKindDnReal = 0x00
	dmKindDnError = 0x01
	dmKindDnDelay = 0x02
	dmKindDnLd = 0x03

	dmKindCnPoolMetaSide = 0x20
	dmKindCnPoolMetaRedunMeta = 0x21
	dmKindCnPoolMetaRedunData = 0x22
	dmKindCnPoolMetaRedunFinal = 0x23
	dmKindCnPoolMetaFinal = 0x24
	dmKindCnPoolDataSide = 0x25
	dmKindCnPoolDataRedunMeta = 0x26
	dmKindCnPoolDataRedunData = 0x27
	dmKindCnPoolDataRedunFinal = 0x28
	dmKindCnPoolDataFinal = 0x29
	dmKindCnPoolFinal = 0x2a
	dmKindCnThinDev = 0x2b
	dmKindCnError = 0x2c
	dmKindCnDelay = 0x2d
	dmKindCnNsBackend = 0x2e

	nqnKindHostCn = 0x00
	nqnKindTargetLdToCn = 0x01
)

type NameFmt struct {
	dmPrefix  string
	nqnPrefix string
}

// {dnv_prefix}-{cluster_id}-{dn_id}-{kind}-{sp_id}-{ld_id}
func (nf *NameFmt) DnRealName(
	clusterId int64,
	dnId int64,
	spId int64,
	ldId int64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%02x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnReal,
		spId,
		ldId,
	)
}

// {dnv_prefix}-{cluster_id}-{dn_id}-{kind}-{sp_id}-{ld_id}
func (nf *NameFmt) DnErrorName(
	clusterId int64,
	dnId int64,
	spId int64,
	ldId int64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%02x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnError,
		spId,
		ldId,
	)
}

// {dnv_prefix}-{cluster_id}-{dn_id}-{kind}-{sp_id}-{ld_id}
func (nf *NameFmt) DnDelayName(
	clusterId int64,
	dnId int64,
	spId int64,
	ldId int64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%02x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnDelay,
		spId,
		ldId,
	)
}

// {dnv_prefix}-{cluster_id}-{dn_id}-{kind}-{sp_id}-{ld_id}-{cn_id}
func (nf *NameFmt) DnLdName(
	clusterId int64,
	dnId int64,
	spId int64,
	ldId int64,
	cnId int64,
) string {
	return fmt.Sprintf(
		"%s-%016x-%016x-%02x-%016x-%016x-%016x",
		nf.dmPrefix,
		clusterId,
		dnId,
		dmKindDnLd,
		spId,
		ldId,
		cnId,
	)
}

// CnPoolMetaSideName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{ld_id}

// CnPoolMetaRedunMetaName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{ld_id}

// CnPoolMetaRedunDataName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{ld_id}

// CnPoolMetaRedunFinalName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{grp_id}

// CnPoolMetaFinalName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{leg_id}

// CnPoolDataSideNameName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{ld_id}

// CnPoolDataRedunMetaName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{ld_id}

// CnPoolDataRedunDataName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{ld_id}

// CnPoolDataRedunFinalName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{grp_id}

// CnPoolDataFinalName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{leg_id}

// CnPoolFinalName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{leg_id}

// CnThinDevName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{td_id}

// CnErrorName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}

// CnDelayName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}

// CnNsBackendName
// {dnv_prefix}-{cluster_id}-{cn_id}-{kind}-{sp_id}-{cntlr_id}-{td_id}


func (nf *NameFmt) HostCnNqn(
	clusterId int64,
	cnId int64,
) -> string {
	return fmt.Sprintf(
		"%s:%02x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindHostCn,
		clusterId,
		cnId,
	)
}

func (nf *NameFmt) TargetLdToCnNqn(
	clusterId int64,
	dnId int64,
	spId int64,
	ldId int64,
	cnId int64,
) -> string {
	return fmt.Sprintf(
		"%s:%02x:%016x:%016x:%016x:%016x:%016x",
		nf.nqnPrefix,
		nqnKindTargetLdToCn,
		clusterId,
		dnId,
		spId,
		ldId,
		cnId,
	)
}