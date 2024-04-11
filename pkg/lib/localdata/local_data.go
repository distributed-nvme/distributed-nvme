package localdata

import (
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type LocalData struct {
	ldataPath string
}

type DnData struct {
	DnId        string
	DevPath     string
	PortNum     uint32
	Revision    int64
	LiveSpLdMap map[string]bool
	DeadSpLdMap map[string]bool
}

func (lData *LocalData) GetDnData(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
) (*DnData, error) {
	return nil, nil
}

func (ldata *LocalData) SetDnData(
	pch *ctxhelper.PerCtxHelper,
	dnData *DnData,
) error {
	return nil
}

func NewLocalData(ldataPath string) *LocalData {
	return &LocalData{
		ldataPath: ldataPath,
	}
}
