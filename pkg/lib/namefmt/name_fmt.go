package namefmt

import (
	"fmt"
)

type NameFmt struct {
	dmPrefix  string
	nqnPrefix string
}

const (
	devTypeLdDnDm = "0000"
	devTypeLdCnDm = "1000"
	devTypeGrpMetaDm = "1001"
	devTypeGrpDataDm = "1002"
	devTypeLegMetaDm = "1003"
	devTypeLegDataDm = "1004"
	devTypeLegPoolDm = "1005"
	devTypeLegThinDm = "1006"
	devTypeRaid0ThinDm = "1007"

	nqnTypeHostCn = "0000"
	nqnTypeLdDnDm = "1000"
	nqnTypeRemote = "1100"
)

func (nf *NameFmt) LdDnDmName(
	dnId string,
	spId string,
	ldId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLdDnDm,
		dnId,
		spId,
		ldId,
	)
}

func (nf *NameFmt) HostNqnCn(cnId string) string {
	return fmt.Sprintf(
		"%s:%s:%s",
		nf.nqnPrefix,
		nqnTypeHostCn,
		cnId,
	)
}

func (nf *NameFmt) LdDnDmNqn(
	dnId string,
	spId string,
	ldId string,
) string {
	return fmt.Sprintf(
		"%s:%s:%s:%s:%s",
		nf.nqnPrefix,
		nqnTypeLdDnDm,
		dnId,
		spId,
		ldId,
	)
}

func (nf *NameFmt) RemoteLegNqn(
	cnId string,
	spId string,
	legId string,
) string {
	return fmt.Sprintf(
		"%s:%s:%s:%s:%s",
		nf.nqnPrefix,
		nqnTypeRemote,
		cnId,
		spId,
		legId,
	)
}

func (nf *NameFmt) LdDnDmNsNum() string {
	return "1"
}

func (nf *NameFmt) LdCnDmName(
	cnId string,
	spId string,
	ldId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLdCnDm,
		cnId,
		spId,
		ldId,
	)
}

func (nf *NameFmt) GrpMetaDmName(
	cnId string,
	spId string,
	grpId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeGrpMetaDm,
		cnId,
		spId,
		grpId,
	)
}

func (nf *NameFmt) GrpDataDmName(
	cnId string,
	spId string,
	grpId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeGrpDataDm,
		cnId,
		spId,
		grpId,
	)
}

func (nf *NameFmt) LegMetaDmName(
	cnId string,
	spId string,
	legId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegMetaDm,
		cnId,
		spId,
		legId,
	)
}

func (nf *NameFmt) LegDataDmName(
	cnId string,
	spId string,
	legId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegDataDm,
		cnId,
		spId,
		legId,
	)
}

func (nf *NameFmt) LegPoolDmName(
	cnId string,
	spId string,
	legId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegPoolDm,
		cnId,
		spId,
		legId,
	)
}

func (nf *NameFmt) LegThinDmName(
	cnId string,
	spId string,
	legId string,
	thinId uint32,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s-%08d",
		nf.dmPrefix,
		devTypeLegThinDm,
		cnId,
		spId,
		legId,
		thinId,
	)
}

func (nf *NameFmt) Raid0ThinDmName(
	cnId string,
	spId string,
	thinId uint32,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%08d",
		nf.dmPrefix,
		devTypeRaid0ThinDm,
		cnId,
		spId,
		thinId,
	)
}

func (nf *NameFmt) DmNameToPath(dmName string) string {
	return fmt.Sprintf("/dev/mapper/%s", dmName)
}

func NewNameFmt(
	dmPrefix string,
	nqnPrefix string,
) *NameFmt {
	return &NameFmt{
		dmPrefix:  dmPrefix,
		nqnPrefix: nqnPrefix,
	}
}
