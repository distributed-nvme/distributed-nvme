package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/localdata"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/oscmd"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

func encodeSpLdId(
	spId string,
	ldId string,
) string {
	return fmt.Sprintf("%s-%s", spId, ldId)
}

func decodeSpLdId(
	key string,
) (string, string, error) {
	items := strings.Split(key, "-")
	if len(items) != 2 {
		return "", "", fmt.Errorf("Invalid item len: %s %d", items, len(items))
	}
	return items[0], items[1], nil
}

type spLdRuntimeData struct {
	mu        sync.Mutex
	devPath   string
	portNum   uint32
	spLdLocal *localdata.SpLdLocal
}

func syncupSpLd(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	spLdConf *pbnd.SpLdConf,
	devPath string,
	portNum uint32,
) error {
	return nil
}

func cleanupSpLd(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	spLdLocal *localdata.SpLdLocal,
) error {
	return nil
}

type dnAgentServer struct {
	pbnd.UnimplementedDiskNodeAgentServer
	mu         sync.Mutex
	oc         *oscmd.OsCommand
	local      *localdata.LocalClient
	bgInterval time.Duration
	dnLocal    *localdata.DnLocal
	spLdMap    map[string]*spLdRuntimeData
}

func (dnAgent *dnAgentServer) GetDevSize(
	ctx context.Context,
	req *pbnd.GetDevSizeRequest,
) (*pbnd.GetDevSizeReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	timestamp := time.Now().UnixMilli()
	size, err := dnAgent.oc.GetBlockDevSize(pch, req.DevPath)
	if err != nil {
		return &pbnd.GetDevSizeReply{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: timestamp,
			},
		}, nil
	}
	return &pbnd.GetDevSizeReply{
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: timestamp,
		},
		Size: size,
	}, nil
}

func (dnAgent *dnAgentServer) SyncupDn(
	ctx context.Context,
	req *pbnd.SyncupDnRequest,
) (*pbnd.SyncupDnReply, error) {
	dnAgent.mu.Lock()
	defer dnAgent.mu.Unlock()

	pch := ctxhelper.GetPerCtxHelper(ctx)
	timestamp := time.Now().UnixMilli()

	if dnAgent.dnLocal == nil {
		dnLocal, err := dnAgent.local.GetDnLocal(pch, req.DnConf.DnId)
		if err != nil {
			return &pbnd.SyncupDnReply{
				DnInfo: &pbnd.DnInfo{
					StatusInfo: &pbnd.StatusInfo{
						Code:      constants.StatusCodeInternalErr,
						Msg:       err.Error(),
						Timestamp: timestamp,
					},
				},
			}, nil
		}
		if dnLocal == nil {
			dnAgent.dnLocal = &localdata.DnLocal{
				DnId:        req.DnConf.DnId,
				DevPath:     req.DnConf.DevPath,
				PortNum:     req.DnConf.NvmePortConf.PortNum,
				Revision:    req.DnConf.Revision,
				LiveSpLdMap: make(map[string]bool),
				DeadSpLdMap: make(map[string]bool),
			}
		} else {
			dnAgent.dnLocal = dnLocal
		}
	}

	if req.DnConf.DnId != dnAgent.dnLocal.DnId {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("DnId: %s", dnAgent.dnLocal.DnId),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.DevPath != dnAgent.dnLocal.DevPath {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("DevPath: %s", dnAgent.dnLocal.DevPath),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.NvmePortConf.PortNum != dnAgent.dnLocal.PortNum {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("PortNum: %d", dnAgent.dnLocal.PortNum),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.Revision < dnAgent.dnLocal.Revision {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", dnAgent.dnLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if err := dnAgent.oc.CreateNvmetPort(
		pch,
		dnAgent.dnLocal.PortNum,
		req.DnConf.NvmePortConf.NvmeListener.TrType,
		req.DnConf.NvmePortConf.NvmeListener.AdrFam,
		req.DnConf.NvmePortConf.NvmeListener.TrAddr,
		req.DnConf.NvmePortConf.NvmeListener.TrSvcId,
		req.DnConf.NvmePortConf.TrEq.SeqCh,
	); err != nil {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	keyInReq := make(map[string]bool)
	for _, spLd := range req.DnConf.SpLdIdList {
		key := encodeSpLdId(spLd.SpId, spLd.LdId)
		keyInReq[key] = true
	}

	for key := range dnAgent.dnLocal.LiveSpLdMap {
		_, ok := keyInReq[key]
		if !ok {
			delete(dnAgent.dnLocal.LiveSpLdMap, key)
			dnAgent.dnLocal.DeadSpLdMap[key] = true
		}
	}

	for key := range keyInReq {
		dnAgent.dnLocal.LiveSpLdMap[key] = true
	}

	keyToLoad := make([]string, 0)
	for key := range dnAgent.dnLocal.LiveSpLdMap {
		keyToLoad = append(keyToLoad, key)
	}
	for key := range dnAgent.dnLocal.DeadSpLdMap {
		keyToLoad = append(keyToLoad, key)
	}
	for _, key := range keyToLoad {
		if _, ok := dnAgent.spLdMap[key]; !ok {
			spId, ldId, err := decodeSpLdId(key)
			if err != nil {
				pch.Logger.Fatal("decodeSpLdId err: %s %v", key, err)
			}
			spLdLocal, err := dnAgent.local.GetSpLdLocal(
				pch,
				dnAgent.dnLocal.DnId,
				spId,
				ldId,
			)
			if err != nil {
				pch.Logger.Fatal(
					"GetSpLdLocal err: %s %s %s %v",
					dnAgent.dnLocal.DnId,
					spId,
					ldId,
					err,
				)
			}
			spLdData := &spLdRuntimeData{
				devPath:   dnAgent.dnLocal.DevPath,
				portNum:   dnAgent.dnLocal.PortNum,
				spLdLocal: spLdLocal,
			}
			dnAgent.spLdMap[key] = spLdData
		}
	}

	if err := dnAgent.local.SetDnLocal(pch, dnAgent.dnLocal); err != nil {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.SyncupDnReply{
		DnInfo: &pbnd.DnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func (dnAgent *dnAgentServer) fetchDeadSpLd(
	pch *ctxhelper.PerCtxHelper,
) map[string]*spLdRuntimeData {
	keyToSpLd := make(map[string]*spLdRuntimeData)

	dnAgent.mu.Lock()
	defer dnAgent.mu.Unlock()

	if dnAgent.dnLocal != nil {
		for key := range dnAgent.dnLocal.DeadSpLdMap {
			spLdData, ok := dnAgent.spLdMap[key]
			if !ok {
				pch.Logger.Fatal("Can not find key in spLdMap: %s", key)
			}
			keyToSpLd[key] = spLdData
		}
	}

	return keyToSpLd
}

func (dnAgent *dnAgentServer) updateDeadSpLd(
	pch *ctxhelper.PerCtxHelper,
	deleted []string,
) {
	dnAgent.mu.Lock()
	defer dnAgent.mu.Unlock()

	if dnAgent.dnLocal != nil {
		for _, key := range deleted {
			delete(dnAgent.dnLocal.DeadSpLdMap, key)
			delete(dnAgent.spLdMap, key)
		}
	}

	if err := dnAgent.local.SetDnLocal(pch, dnAgent.dnLocal); err != nil {
		pch.Logger.Error("SetDnLocal err: %v", err)
	}
}

func (dnAgent *dnAgentServer) cleanup(
	pch *ctxhelper.PerCtxHelper,
	keyToSpLd map[string]*spLdRuntimeData,
) []string {
	deleted := make([]string, 0)
	for key, spLdData := range keyToSpLd {
		spLdData.mu.Lock()
		err := cleanupSpLd(
			pch,
			dnAgent.oc,
			spLdData.spLdLocal,
		)
		spLdData.mu.Unlock()
		if err != nil {
			pch.Logger.Error("cleanupSpLd err: %v", err)
			continue
		}
		deleted = append(deleted, key)
	}
	return deleted
}

func (dnAgent *dnAgentServer) background(
	parentCtx context.Context,
) {
	traceId := uuid.New().String()
	logPrefix := fmt.Sprintf("DnCleanUp|%s", traceId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, traceId)
	select {
	case <-pch.Ctx.Done():
		return
	case <-time.After(dnAgent.bgInterval):
		keyToSpLd := dnAgent.fetchDeadSpLd(pch)
		deleted := dnAgent.cleanup(pch, keyToSpLd)
		dnAgent.updateDeadSpLd(pch, deleted)
	}
}

func (dnAgent *dnAgentServer) CheckDn(
	ctx context.Context,
	req *pbnd.CheckDnRequest,
) (*pbnd.CheckDnReply, error) {
	timestamp := time.Now().UnixMilli()
	return &pbnd.CheckDnReply{
		DnInfo: &pbnd.DnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func (dnAgent *dnAgentServer) getSpLdData(
	dnId string,
	spId string,
	ldId string,
) *spLdRuntimeData {
	key := encodeSpLdId(spId, ldId)
	dnAgent.mu.Lock()
	defer dnAgent.mu.Unlock()
	if spLdData, ok := dnAgent.spLdMap[key]; ok {
		return spLdData
	}
	return nil
}

func (dnAgent *dnAgentServer) SyncupSpLd(
	ctx context.Context,
	req *pbnd.SyncupSpLdRequest,
) (*pbnd.SyncupSpLdReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	timestamp := time.Now().UnixMilli()
	spLdData := dnAgent.getSpLdData(
		req.SpLdConf.DnId,
		req.SpLdConf.SpId,
		req.SpLdConf.LdId,
	)
	if spLdData == nil {
		return &pbnd.SyncupSpLdReply{
			SpLdInfo: &pbnd.SpLdInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code: constants.StatusCodeNotFound,
					Msg: fmt.Sprintf(
						"Do not find spLdData: %s %s %s",
						req.SpLdConf.DnId,
						req.SpLdConf.SpId,
						req.SpLdConf.LdId,
					),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spLdData.mu.Lock()
	defer spLdData.mu.Unlock()

	if spLdData.spLdLocal.Revision > req.SpLdConf.Revision {
		return &pbnd.SyncupSpLdReply{
			SpLdInfo: &pbnd.SpLdInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spLdData.spLdLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spLdData.spLdLocal.Revision = req.SpLdConf.Revision

	if err := dnAgent.local.SetSpLdLocal(pch, spLdData.spLdLocal); err != nil {
		return &pbnd.SyncupSpLdReply{
			SpLdInfo: &pbnd.SpLdInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if err := syncupSpLd(
		pch,
		dnAgent.oc,
		req.SpLdConf,
		spLdData.devPath,
		spLdData.portNum,
	); err != nil {
		return &pbnd.SyncupSpLdReply{
			SpLdInfo: &pbnd.SpLdInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.SyncupSpLdReply{
		SpLdInfo: &pbnd.SpLdInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.ReplyCodeSucceed,
				Msg:       constants.ReplyMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func newDnAgentServer(
	ctx context.Context,
	dataPath string,
	bgInterval time.Duration,
) *dnAgentServer {
	dnAgent := &dnAgentServer{
		oc:         oscmd.NewOsCommand(),
		local:      localdata.NewLocalClient(dataPath),
		dnLocal:    nil,
		bgInterval: bgInterval,
	}
	go dnAgent.background(ctx)
	return dnAgent
}
