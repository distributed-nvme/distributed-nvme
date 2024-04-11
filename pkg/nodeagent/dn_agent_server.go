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
	spId uint64,
	ldId uint64,
) string {
	return fmt.Sprintf("%016x-%016x", spId, ldId)
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

type spLdBasic struct {
	dnId string
	spId string
	ldId string
}

type spLdDetail struct {
	start    uint64
	length   uint64
	cnIdList []string
}

type spLd struct {
	mu       sync.Mutex
	basic    *spLdBasic
	detail   *spLdDetail
	revision int64
}

func syncupSpLd(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	devPath string,
	portNum uint32,
	basic *spLdBasic,
	detail *spLdDetail,
	cnIdList []string,
) (uint32, string) {
	return 0, "succeeded"
}

func cleanupSpLd(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	devPath string,
	portNum uint32,
	basic *spLdBasic,
) error {
	return nil
}

type dnAgentServer struct {
	pbnd.UnimplementedDiskNodeAgentServer
	mu      sync.Mutex
	oc      *oscmd.OsCommand
	lData   *localdata.LocalData
	dnData  *localdata.DnData
	spLdMap map[string]*spLd
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
	dnId := fmt.Sprintf("%016x", req.DnConf.DnId)

	if dnAgent.dnData == nil {
		dnData, err := dnAgent.lData.GetDnData(pch, dnId)
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
		if dnData == nil {
			dnAgent.dnData = &localdata.DnData{
				DnId:        dnId,
				DevPath:     req.DnConf.DevPath,
				PortNum:     req.DnConf.NvmePortConf.PortNum,
				Revision:    req.DnConf.Revision,
				LiveSpLdMap: make(map[string]bool),
				DeadSpLdMap: make(map[string]bool),
			}
		} else {
			dnAgent.dnData = dnData
		}
	}

	if dnId != dnAgent.dnData.DnId {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("DnId: %s", dnAgent.dnData.DnId),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.DevPath != dnAgent.dnData.DevPath {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("DevPath: %s", dnAgent.dnData.DevPath),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.NvmePortConf.PortNum != dnAgent.dnData.PortNum {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("PortNum: %d", dnAgent.dnData.PortNum),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.Revision < dnAgent.dnData.Revision {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %s", dnAgent.dnData.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if err := dnAgent.oc.CreateNvmetPort(
		pch,
		dnAgent.dnData.PortNum,
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

	for key := range dnAgent.dnData.LiveSpLdMap {
		_, ok := keyInReq[key]
		if !ok {
			delete(dnAgent.dnData.LiveSpLdMap, key)
			dnAgent.dnData.DeadSpLdMap[key] = true
		}
	}

	for key := range keyInReq {
		dnAgent.dnData.LiveSpLdMap[key] = true
	}

	if err := dnAgent.lData.SetDnData(pch, dnAgent.dnData); err != nil {
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
) map[string]*spLd {
	keyToSpLd := make(map[string]*spLd)

	dnAgent.mu.Lock()
	defer dnAgent.mu.Unlock()

	if dnAgent.dnData != nil {
		for key := range dnAgent.dnData.DeadSpLdMap {
			spld, ok := dnAgent.spLdMap[key]
			if !ok {
				spId, ldId, err := decodeSpLdId(key)
				if err != nil {
					pch.Logger.Fatal("%v", err)
				}
				spld = &spLd{
					basic: &spLdBasic{
						dnId: dnAgent.dnData.DnId,
						spId: spId,
						ldId: ldId,
					},
					revision: 0,
				}
				dnAgent.spLdMap[key] = spld
			}
			keyToSpLd[key] = spld
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

	if dnAgent.dnData != nil {
		for _, key := range deleted {
			delete(dnAgent.dnData.DeadSpLdMap, key)
			delete(dnAgent.spLdMap, key)
		}
	}

	if err := dnAgent.lData.SetDnData(pch, dnAgent.dnData); err != nil {
		pch.Logger.Error("SetDnData err: %v", err)
	}
}

func (dnAgent *dnAgentServer) cleanup(
	pch *ctxhelper.PerCtxHelper,
	keyToSpLd map[string]*spLd,
) []string {
	deleted := make([]string, 0)
	for key, spld := range keyToSpLd {
		spld.mu.Lock()
		err := cleanupSpLd(
			pch,
			dnAgent.oc,
			dnAgent.dnData.DevPath,
			dnAgent.dnData.PortNum,
			spld.basic,
		)
		spld.mu.Unlock()
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
	case <-time.After(1):
		keyToSpLd := dnAgent.fetchDeadSpLd(pch)
		deleted := dnAgent.cleanup(pch, keyToSpLd)
		dnAgent.updateDeadSpLd(pch, deleted)
	}
}

func newDnAgentServer(
	ctx context.Context,
	ldataPath string,
) *dnAgentServer {
	dnAgent := &dnAgentServer{
		oc:     oscmd.NewOsCommand(),
		lData:  localdata.NewLocalData(ldataPath),
		dnData: nil,
	}
	go dnAgent.background(ctx)
	return dnAgent
}
