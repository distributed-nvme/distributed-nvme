package localdata

import (
	"fmt"
	"sync"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type DnLocal struct {
	DnId        string
	DevPath     string
	PortNum     string
	Revision    int64
	LiveSpLdMap map[string]bool
	DeadSpLdMap map[string]bool
}

type SpLdLocal struct {
	DnId     string
	SpId     string
	LdId     string
	Revision int64
}

type CnLocal struct {
	CnId           string
	PortNum        string
	Revision       int64
	LiveSpCntlrMap map[string]bool
	DeadSpCntlrMap map[string]bool
}

type SpCntlrLocal struct {
	CnId     string
	SpId     string
	CntlrId  string
	Revision int64
	PortNum  string
}

func spLdKey(dnId, spId, ldId string) string {
	return fmt.Sprintf("%s-%s-%s", dnId, spId, ldId)
}

func spCntlrKey(cnId, spId, cntlrId string) string {
	return fmt.Sprintf("%s-%s-%s", cnId, spId, cntlrId)
}

type LocalClient struct {
	dataPath   string
	dnMu       sync.Mutex
	dnMap      map[string]*DnLocal
	spLdMu     sync.Mutex
	spLdMap    map[string]*SpLdLocal
	cnMu       sync.Mutex
	cnMap      map[string]*CnLocal
	spCntlrMu  sync.Mutex
	spCntlrMap map[string]*SpCntlrLocal
}

func (local *LocalClient) GetDnLocal(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
) (*DnLocal, error) {
	local.dnMu.Lock()
	defer local.dnMu.Unlock()
	dnLocal, ok := local.dnMap[dnId]
	if ok {
		return dnLocal, nil
	}
	return nil, nil
}

func (local *LocalClient) SetDnLocal(
	pch *ctxhelper.PerCtxHelper,
	dnLocal *DnLocal,
) error {
	local.dnMu.Lock()
	defer local.dnMu.Unlock()
	if dnLocal != nil {
		local.dnMap[dnLocal.DnId] = dnLocal
	}
	return nil
}

func (local *LocalClient) DeleteDnLocal(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
) error {
	local.dnMu.Lock()
	defer local.dnMu.Unlock()
	delete(local.dnMap, dnId)
	return nil
}

func (local *LocalClient) GetSpLdLocal(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
	spId string,
	ldId string,
) (*SpLdLocal, error) {
	key := spLdKey(dnId, spId, ldId)
	local.spLdMu.Lock()
	defer local.spLdMu.Unlock()
	spLdLocal, ok := local.spLdMap[key]
	if ok {
		return spLdLocal, nil
	}
	return &SpLdLocal{
		DnId:     dnId,
		SpId:     spId,
		LdId:     ldId,
		Revision: constants.RevisionUninit,
	}, nil
}

func (local *LocalClient) SetSpLdLocal(
	pch *ctxhelper.PerCtxHelper,
	spLdLocal *SpLdLocal,
) error {
	key := spLdKey(spLdLocal.DnId, spLdLocal.SpId, spLdLocal.LdId)
	local.spLdMu.Lock()
	defer local.spLdMu.Unlock()
	local.spLdMap[key] = spLdLocal
	return nil
}

func (local *LocalClient) DeleteSpLdLocal(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
	spId string,
	ldId string,
) error {
	key := spLdKey(dnId, spId, ldId)
	local.spLdMu.Lock()
	defer local.spLdMu.Unlock()
	delete(local.spLdMap, key)
	return nil
}

func (local *LocalClient) GetCnLocal(
	pch *ctxhelper.PerCtxHelper,
	cnId string,
) (*CnLocal, error) {
	local.cnMu.Lock()
	defer local.cnMu.Unlock()
	cnLocal, ok := local.cnMap[cnId]
	if ok {
		return cnLocal, nil
	}
	return nil, nil
}

func (local *LocalClient) SetCnLocal(
	pch *ctxhelper.PerCtxHelper,
	cnLocal *CnLocal,
) error {
	local.cnMu.Lock()
	defer local.cnMu.Unlock()
	if cnLocal != nil {
		local.cnMap[cnLocal.CnId] = cnLocal
	}
	return nil
}

func (local *LocalClient) DeleteCnLocal(
	pch *ctxhelper.PerCtxHelper,
	cnId string,
) error {
	local.cnMu.Lock()
	defer local.cnMu.Unlock()
	delete(local.cnMap, cnId)
	return nil
}

func (local *LocalClient) GetSpCntlrLocal(
	pch *ctxhelper.PerCtxHelper,
	cnId string,
	spId string,
	cntlrId string,
) (*SpCntlrLocal, error) {
	key := spCntlrKey(cnId, spId, cntlrId)
	local.spCntlrMu.Lock()
	defer local.spCntlrMu.Unlock()
	spCntlrLocal, ok := local.spCntlrMap[key]
	if ok {
		return spCntlrLocal, nil
	}
	return &SpCntlrLocal{
		CnId:     cnId,
		SpId:     spId,
		CntlrId:  cntlrId,
		Revision: constants.RevisionUninit,
		PortNum:  "",
	}, nil
}

func (local *LocalClient) SetSpCntlrLocal(
	pch *ctxhelper.PerCtxHelper,
	spCntlrLocal *SpCntlrLocal,
) error {
	key := spCntlrKey(spCntlrLocal.CnId, spCntlrLocal.SpId, spCntlrLocal.CntlrId)
	local.spCntlrMu.Lock()
	defer local.spCntlrMu.Unlock()
	local.spCntlrMap[key] = spCntlrLocal
	return nil
}

func (local *LocalClient) DeleteSpCntlrLocal(
	pch *ctxhelper.PerCtxHelper,
	cnId string,
	spId string,
	cntlrId string,
) error {
	key := spCntlrKey(cnId, spId, cntlrId)
	local.spCntlrMu.Lock()
	defer local.spCntlrMu.Unlock()
	delete(local.spCntlrMap, key)
	return nil
}

func NewLocalClient(dataPath string) *LocalClient {
	return &LocalClient{
		dataPath: dataPath,
		dnMap:    make(map[string]*DnLocal),
		cnMap:    make(map[string]*CnLocal),
		spLdMap:  make(map[string]*SpLdLocal),
	}
}
