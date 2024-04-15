package localdata

import (
	"fmt"
	"sync"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type DnLocal struct {
	DnId        string
	DevPath     string
	PortNum     uint32
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

func spLdKey(dnId, spId, ldId string) string {
	return fmt.Sprintf("%s-%s-%s", dnId, spId, ldId)
}

type LocalClient struct {
	dataPath string
	dnMu     sync.Mutex
	dnMap    map[string]*DnLocal
	spLdMu   sync.Mutex
	spLdMap  map[string]*SpLdLocal
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
	local.dnMap[dnLocal.DnId] = dnLocal
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
	return nil, nil
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

func NewLocalClient(dataPath string) *LocalClient {
	return &LocalClient{
		dataPath: dataPath,
		dnMap:    make(map[string]*DnLocal),
		spLdMap:  make(map[string]*SpLdLocal),
	}
}
