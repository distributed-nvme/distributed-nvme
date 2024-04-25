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

func (nf *NameFmt) LdDnDmNsNum() string {
	return "1"
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
