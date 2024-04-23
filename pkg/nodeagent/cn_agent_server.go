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
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/namefmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/oscmd"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

func encodeSpCntlrId(
	spId string,
	cntlrId string,
) string {
	return fmt.Sprintf("%s-%s", spId, cntlrId)
}

func decodeSpCntlrId(
	key string,
) (string, string, error) {
	items := strings.Split(key, "-")
	if len(items) != 2 {
		return "", "", fmt.Errorf("Invalid item len: %s %d", items, len(items))
	}
	return items[0], items[1], nil
}

type spCntlrRuntimeData struct {
	mu           sync.Mutex
	spCntlrLocal *localdata.SpCntlrLocal
	spCntlrConf  *pbnd.SpCntlrConf
}

func syncupSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
) error {
	return nil
}

func cleanupSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrLocal *localdata.SpCntlrLocal,
) error {
	return nil
}

type cnAgentServer struct {
	pbnd.UnimplementedControllerNodeAgentServer
	mu         sync.Mutex
	oc         *oscmd.OsCommand
	nf         *namefmt.NameFmt
	local      *localdata.LocalClient
	bgInterval time.Duration
	cnLocal    *localdata.CnLocal
	spCntlrMap map[string]*spCntlrRuntimeData
}

func (cnAgent *cnAgentServer) SyncupCn(
	ctx context.Context,
	req *pbnd.SyncupCnRequest,
) (*pbnd.SyncupCnReply, error) {
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	pch := ctxhelper.GetPerCtxHelper(ctx)
	timestamp := time.Now().UnixMilli()

	if cnAgent.cnLocal == nil {
		cnLocal, err := cnAgent.local.GetCnLocal(pch, req.CnConf.CnId)
		if err != nil {
			return &pbnd.SyncupCnReply{
				CnInfo: &pbnd.CnInfo{
					StatusInfo: &pbnd.StatusInfo{
						Code:      constants.StatusCodeInternalErr,
						Msg:       err.Error(),
						Timestamp: timestamp,
					},
				},
			}, nil
		}
		if cnLocal == nil {
			cnAgent.cnLocal = &localdata.CnLocal{
				CnId:           req.CnConf.CnId,
				Revision:       req.CnConf.Revision,
				LiveSpCntlrMap: make(map[string]bool),
				DeadSpCntlrMap: make(map[string]bool),
			}
		} else {
			cnAgent.cnLocal = cnLocal
		}
	}

	if req.CnConf.CnId != cnAgent.cnLocal.CnId {
		return &pbnd.SyncupCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("CnId: %s", cnAgent.cnLocal.CnId),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.CnConf.Revision < cnAgent.cnLocal.Revision {
		return &pbnd.SyncupCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", cnAgent.cnLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	keyInReq := make(map[string]bool)
	for _, spCntlr := range req.CnConf.SpCntlrIdList {
		key := encodeSpCntlrId(spCntlr.SpId, spCntlr.CntlrId)
		keyInReq[key] = true
	}

	for key := range cnAgent.cnLocal.LiveSpCntlrMap {
		_, ok := keyInReq[key]
		if !ok {
			delete(cnAgent.cnLocal.LiveSpCntlrMap, key)
			cnAgent.cnLocal.DeadSpCntlrMap[key] = true
		}
	}

	for key := range keyInReq {
		cnAgent.cnLocal.LiveSpCntlrMap[key] = true
	}

	keyToLoad := make([]string, 0)
	for key := range cnAgent.cnLocal.LiveSpCntlrMap {
		keyToLoad = append(keyToLoad, key)
	}
	for key := range cnAgent.cnLocal.DeadSpCntlrMap {
		keyToLoad = append(keyToLoad, key)
	}
	for _, key := range keyToLoad {
		var spCntlrData *spCntlrRuntimeData
		if spCntlrData, ok := cnAgent.spCntlrMap[key]; !ok {
			spId, cntlrId, err := decodeSpCntlrId(key)
			if err != nil {
				pch.Logger.Fatal("ecodeSpCntlrId err: %s %v", key, err)
			}
			spCntlrLocal, err := cnAgent.local.GetSpCntlrLocal(
				pch,
				cnAgent.cnLocal.CnId,
				spId,
				cntlrId,
			)
			if err != nil {
				pch.Logger.Fatal(
					"GetSpCntlrLocal err: %s %s %s %v",
					cnAgent.cnLocal.CnId,
					spId,
					cntlrId,
					err,
				)
			}
			spCntlrData = &spCntlrRuntimeData{
				spCntlrLocal: spCntlrLocal,
			}
			cnAgent.spCntlrMap[key] = spCntlrData
		}
		spCntlrData.mu.Lock()
		if _, ok := cnAgent.cnLocal.DeadSpCntlrMap[key]; ok {
			spCntlrData.spCntlrLocal.Revision = constants.RevisionDeleted
			if err := cnAgent.local.SetSpCntlrLocal(
				pch,
				spCntlrData.spCntlrLocal,
			); err != nil {
				spCntlrData.mu.Unlock()
				return &pbnd.SyncupCnReply{
					CnInfo: &pbnd.CnInfo{
						StatusInfo: &pbnd.StatusInfo{
							Code:      constants.StatusCodeInternalErr,
							Msg:       err.Error(),
							Timestamp: timestamp,
						},
					},
				}, nil
			}
		}
		spCntlrData.mu.Unlock()
	}

	if err := cnAgent.local.SetCnLocal(pch, cnAgent.cnLocal); err != nil {
		return &pbnd.SyncupCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.SyncupCnReply{
		CnInfo: &pbnd.CnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func (cnAgent *cnAgentServer) fetchDeadSpCntlr(
	pch *ctxhelper.PerCtxHelper,
) map[string]*spCntlrRuntimeData {
	keyToSpCntlr := make(map[string]*spCntlrRuntimeData)

	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	if cnAgent.cnLocal != nil {
		for key := range cnAgent.cnLocal.DeadSpCntlrMap {
			spCntlrData, ok := cnAgent.spCntlrMap[key]
			if !ok {
				pch.Logger.Fatal("Can not find key in spCntlrMap: %s", key)
			}
			keyToSpCntlr[key] = spCntlrData
		}
	}

	return keyToSpCntlr
}

func (cnAgent *cnAgentServer) updateDeadSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	deleted []string,
) {
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	if cnAgent.cnLocal != nil {
		for _, key := range deleted {
			delete(cnAgent.cnLocal.DeadSpCntlrMap, key)
			delete(cnAgent.spCntlrMap, key)
		}
	}

	if err := cnAgent.local.SetCnLocal(
		pch,
		cnAgent.cnLocal,
	); err != nil {
		pch.Logger.Fatal("SetCnLocal err: %v", err)
	}
}

func (cnAgent *cnAgentServer) cleanup(
	pch *ctxhelper.PerCtxHelper,
	keyToSpCntlr map[string]*spCntlrRuntimeData,
) []string {
	deleted := make([]string, 0)
	for key, spCntlrData := range keyToSpCntlr {
		spCntlrData.mu.Lock()
		err := cleanupSpCntlr(
			pch,
			cnAgent.oc,
			cnAgent.nf,
			spCntlrData.spCntlrLocal,
		)
		spCntlrData.mu.Unlock()
		if err != nil {
			pch.Logger.Error("cleanupSpCntlr err: %v", err)
			continue
		}
		deleted = append(deleted, key)
	}
	return deleted
}

func (cnAgent *cnAgentServer) background(
	parentCtx context.Context,
) {
	traceId := uuid.New().String()
	logPrefix := fmt.Sprintf("CnCleanUp|%s", traceId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, traceId)
	select {
	case <-pch.Ctx.Done():
		return
	case <-time.After(cnAgent.bgInterval):
		keyToSpCntlr := cnAgent.fetchDeadSpCntlr(pch)
		deleted := cnAgent.cleanup(pch, keyToSpCntlr)
		cnAgent.updateDeadSpCntlr(pch, deleted)
	}
}

func (cnAgent *cnAgentServer) CheckCn(
	ctx context.Context,
	req *pbnd.CheckCnRequest,
) (*pbnd.CheckCnReply, error) {
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	timestamp := time.Now().UnixMilli()

	if cnAgent.cnLocal == nil {
		return &pbnd.CheckCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if cnAgent.cnLocal.CnId != req.CnId {
		return &pbnd.CheckCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("CnId: %s", cnAgent.cnLocal.CnId),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if cnAgent.cnLocal.Revision != req.Revision {
		return &pbnd.CheckCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("Revision: %s", cnAgent.cnLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.CheckCnReply{
		CnInfo: &pbnd.CnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func (cnAgent *cnAgentServer) getSpCntlrData(
	cnId string,
	spId string,
	cntlrId string,
) *spCntlrRuntimeData {
	key := encodeSpCntlrId(spId, cntlrId)
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()
	if spCntlrData, ok := cnAgent.spCntlrMap[key]; ok {
		return spCntlrData
	}
	return nil
}

func (cnAgent *cnAgentServer) SyncupSpCntlr(
	ctx context.Context,
	req *pbnd.SyncupSpCntlrRequest,
) (*pbnd.SyncupSpCntlrReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	timestamp := time.Now().UnixMilli()
	spCntlrData := cnAgent.getSpCntlrData(
		req.SpCntlrConf.CnId,
		req.SpCntlrConf.SpId,
		req.SpCntlrConf.CntlrId,
	)
	if spCntlrData == nil {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code: constants.StatusCodeNotFound,
					Msg: fmt.Sprintf(
						"Do not find spCntlrData: %s %s %s",
						req.SpCntlrConf.CnId,
						req.SpCntlrConf.SpId,
						req.SpCntlrConf.CntlrId,
					),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spCntlrData.mu.Lock()
	defer spCntlrData.mu.Unlock()

	if spCntlrData.spCntlrLocal.Revision == constants.RevisionDeleted {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrLocal.Revision > req.SpCntlrConf.Revision {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spCntlrData.spCntlrLocal.Revision = req.SpCntlrConf.Revision

	if err := cnAgent.local.SetSpCntlrLocal(
		pch,
		spCntlrData.spCntlrLocal,
	); err != nil {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if err := syncupSpCntlr(
		pch,
		cnAgent.oc,
		cnAgent.nf,
		req.SpCntlrConf,
	); err != nil {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spCntlrData.spCntlrConf = req.SpCntlrConf

	return &pbnd.SyncupSpCntlrReply{
		SpCntlrInfo: &pbnd.SpCntlrInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.ReplyCodeSucceed,
				Msg:       constants.ReplyMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func (cnAgent *cnAgentServer) CheckSpCntlr(
	ctx context.Context,
	req *pbnd.CheckSpCntlrRequest,
) (*pbnd.CheckSpCntlrReply, error) {
	timestamp := time.Now().UnixMilli()

	spCntlrData := cnAgent.getSpCntlrData(
		req.CnId,
		req.SpId,
		req.CntlrId,
	)
	if spCntlrData == nil {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spCntlrData.mu.Lock()
	defer spCntlrData.mu.Unlock()

	if spCntlrData.spCntlrLocal.Revision == constants.RevisionDeleted {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrLocal.Revision != req.Revision {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("Revision: %s", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrConf == nil {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeNoConf,
					Msg:       fmt.Sprintf("Revision: %s", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.CheckSpCntlrReply{
		SpCntlrInfo: &pbnd.SpCntlrInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func newCnAgentServer(
	ctx context.Context,
	dataPath string,
	bgInterval time.Duration,
) *cnAgentServer {
	cnAgent := &cnAgentServer{
		oc: oscmd.NewOsCommand(),
		nf: namefmt.NewNameFmt(
			constants.DeviceMapperPrefixDefault,
			constants.NqnPrefixDefault,
		),
		local:      localdata.NewLocalClient(dataPath),
		cnLocal:    nil,
		bgInterval: bgInterval,
	}
	go cnAgent.background(ctx)
	return cnAgent
}
