package localdata

import (
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type LocalData struct {
	ldataPath string
}

type DnData struct {
	DnId string
	Revision int64
}

type SpLdData struct {
	DnId string
	SpId string
	LdId string
	Revision int64
}

func (lData *LocalData) GetDnData(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
) (*DnData, error) {
	return &DnData{
		DnId: dnId,
		Revision: 0,
	}, nil
}

func (ldata *LocalData) SetDnData(
	pch *ctxhelper.PerCtxHelper,
	dnData *DnData,
) error {
	return nil
}

func (lData *LocalData) GetAllSpLdData(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
) ([]*SpLdData, error) {
	return make([]*SpLdData, 0), nil
}

func (lData *LocalData) SetSpLdData(
	pch *ctxhelper.PerCtxHelper,
	spLdData *SpLdData,
) error {
	return nil
}

func NewLocalData(ldataPath string) *LocalData {
	return &LocalData{
		ldataPath: ldataPath,
	}
}
