package namefmt

import (
	"fmt"

	"github.com/google/uuid"
)

var (
	uuidNameSpace = uuid.MustParse("37833e01-35d4-4e5a-b0a1-fff158b9d03b")
)

type NameFmt struct {
	dmPrefix  string
	nqnPrefix string
}

const (
	devTypeLdDnDm        = "0000"
	devTypeLdCnDm        = "1000"
	devTypeGrpMetaDm     = "1001"
	devTypeGrpDataDm     = "1002"
	devTypeLegMetaDm     = "1003"
	devTypeLegDataDm     = "1004"
	devTypeLegPoolDm     = "1005"
	devTypeLegThinDm     = "1006"
	devTypeLegToLocalDm  = "1007"
	devTypeLegToRemoteDm = "1008"
	devTypeRaid0Dm       = "1009"
	devTypeErrorDm       = "1010"

	nqnTypeHostCn = "0000"
	nqnTypeLdDnDm = "1000"
	nqnTypeRemote = "1100"
	nqnTypeSubsys = "1200"
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

func (nf *NameFmt) SsNqnPrefix(
	spId string,
) string {
	return fmt.Sprintf(
		"%s:%s:%s",
		nf.nqnPrefix,
		nqnTypeSubsys,
		spId,
	)
}

func (nf *NameFmt) SsNqn(
	spId string,
	ssId string,
) string {
	prefix := nf.SsNqnPrefix(spId)
	return fmt.Sprintf(
		"%s:%s",
		prefix,
		ssId,
	)
}

func (nf *NameFmt) RemoteLegNqnPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s:%s:%s:%s",
		nf.nqnPrefix,
		nqnTypeRemote,
		cnId,
		spId,
	)
}

func (nf *NameFmt) RemoteLegNqn(
	cnId string,
	spId string,
	legId string,
) string {
	prefix := nf.RemoteLegNqnPrefix(cnId, spId)
	return fmt.Sprintf(
		"%s:%s",
		prefix,
		legId,
	)
}

func (nf *NameFmt) LdDnDmNsNum() string {
	return "1"
}

func (nf *NameFmt) NsUuid(nqn, nsId string) string {
	return uuid.NewMD5(
		uuidNameSpace,
		[]byte(fmt.Sprintf("%s-%s", nqn, nsId)),
	).String()
}

func (nf *NameFmt) NsPath(nqn, nsNum string) string {
	nsUuid := nf.NsUuid(nqn, nsNum)
	return fmt.Sprintf("/dev/disk/by-id/nvme-uuid.%s", nsUuid)
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

func (nf *NameFmt) GrpMetaDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeGrpMetaDm,
		cnId,
		spId,
	)
}
func (nf *NameFmt) GrpMetaDmName(
	cnId string,
	spId string,
	grpId string,
) string {
	prefix := nf.GrpMetaDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s",
		prefix,
		grpId,
	)
}

func (nf *NameFmt) GrpDataDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeGrpDataDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) GrpDataDmName(
	cnId string,
	spId string,
	grpId string,
) string {
	prefix := nf.GrpDataDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s",
		prefix,
		grpId,
	)
}

func (nf *NameFmt) LegMetaDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegMetaDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) LegMetaDmName(
	cnId string,
	spId string,
	legId string,
) string {
	prefix := nf.LegMetaDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s",
		prefix,
		legId,
	)
}

func (nf *NameFmt) LegDataDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegDataDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) LegDataDmName(
	cnId string,
	spId string,
	legId string,
) string {
	prefix := nf.LegDataDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s",
		prefix,
		legId,
	)
}

func (nf *NameFmt) LegPoolDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegPoolDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) LegPoolDmName(
	cnId string,
	spId string,
	legId string,
) string {
	prefix := nf.LegPoolDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s",
		prefix,
		legId,
	)
}

func (nf *NameFmt) LegThinDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegThinDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) LegThinDmName(
	cnId string,
	spId string,
	legId string,
	thinId uint32,
) string {
	prefix := nf.LegThinDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s-%08d",
		prefix,
		legId,
		thinId,
	)
}

func (nf *NameFmt) LegToLocalDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegToLocalDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) LegToLocalDmName(
	cnId string,
	spId string,
	legId string,
	thinId uint32,
) string {
	prefix := nf.LegToLocalDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s-%08d",
		prefix,
		legId,
		thinId,
	)
}

func (nf *NameFmt) LegToRemoteDmPrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeLegToRemoteDm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) LegToRemoteDmName(
	cnId string,
	spId string,
	legId string,
	thinId uint32,
) string {
	prefix := nf.LegToRemoteDmPrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%s-%08d",
		prefix,
		legId,
		thinId,
	)
}

func (nf *NameFmt) Raid0ThinDmNamePrefix(
	cnId string,
	spId string,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s",
		nf.dmPrefix,
		devTypeRaid0Dm,
		cnId,
		spId,
	)
}

func (nf *NameFmt) Raid0ThinDmName(
	cnId string,
	spId string,
	thinId uint32,
) string {
	prefix := nf.Raid0ThinDmNamePrefix(
		cnId,
		spId,
	)
	return fmt.Sprintf(
		"%s-%08d",
		prefix,
		thinId,
	)
}

func (nf *NameFmt) ErrorDmName(
	cnId string,
	spId string,
	devId uint32,
) string {
	return fmt.Sprintf(
		"%s-%s-%s-%s-%08d",
		nf.dmPrefix,
		devTypeErrorDm,
		cnId,
		spId,
		devId,
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
