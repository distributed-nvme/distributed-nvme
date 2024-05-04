package worker

import (
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	// "go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

var (
	spFastRetryCodeMap = make(map[uint32]bool)
)

type spWorkerServer struct {
	pbcp.UnimplementedStoragePoolWorkerServer
	mu             sync.Mutex
	etcdCli        *clientv3.Client
	kf             *keyfmt.KeyFmt
	sm             *stmwrapper.StmWrapper
	initTrigger    chan struct{}
	idAndRevToConf map[string]map[int64]*pbcp.StoragePoolConf
}

func (spwkr *spWorkerServer) getName() string {
	return "sp"
}

func (spwkr *spWorkerServer) getEtcdCli() *clientv3.Client {
	return spwkr.etcdCli
}

func (spwkr *spWorkerServer) getMemberPrefix() string {
	return spwkr.kf.SpMemberPrefix()
}

func (spwkr *spWorkerServer) getResPrefix() string {
	return spwkr.kf.SpConfEntityPrefix()
}

func (spwkr *spWorkerServer) getInitTrigger() <-chan struct{} {
	return spwkr.initTrigger
}

func (spwkr *spWorkerServer) addResRev(
	spId string,
	resBody []byte,
	rev int64,
) ([]string, error) {
	spConf := &pbcp.StoragePoolConf{}
	if err := proto.Unmarshal(resBody, spConf); err != nil {
		return nil, err
	}
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if ok {
		if len(revToConf) > 1 {
			panic("More tahn 1 sp rev: " + spId)
		}
	} else {
		revToConf = make(map[int64]*pbcp.StoragePoolConf)
		spwkr.idAndRevToConf[spId] = revToConf
	}
	revToConf[rev] = spConf
	grpcTargetList := make([]string, 0)
	for _, legConf := range spConf.LegConfList {
		for _, grpConf := range legConf.GrpConfList {
			for _, ldConf := range grpConf.LdConfList {
				grpcTargetList = append(grpcTargetList, ldConf.DnGrpcTarget)
			}
		}
	}
	for _, cntlrConf := range spConf.CntlrConfList {
		grpcTargetList = append(grpcTargetList, cntlrConf.CnGrpcTarget)
	}
	return grpcTargetList, nil
}

func (spwkr *spWorkerServer) delResRev(
	spId string,
	rev int64,
) error {
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if !ok {
		panic("Unknown sp id: " + spId)
	}
	delete(revToConf, rev)
	if len(revToConf) == 0 {
		delete(spwkr.idAndRevToConf, spId)
	}
	return nil
}

type storagePoolMap struct {
	legIdToConf      map[string]*pbcp.LegConf
	grpIdToConf      map[string]*pbcp.GrpConf
	ldIdToCnIdList   map[string][]string
	cntlrIdToLegList map[string][]*pbcp.LegConf
}

func generateSpMap(spConf *pbcp.StoragePoolConf) *storagePoolMap {
	legIdToConf := make(map[string]*pbcp.LegConf)
	grpIdToConf := make(map[string]*pbcp.GrpConf)
	ldIdToCnIdList := make(map[string][]string)
	cntlrIdToLegList := make(map[string][]*pbcp.LegConf)
	for _, legConf := range spConf.LegConfList {
		legIdToConf[legConf.LegId] = legConf
		if _, ok := cntlrIdToLegList[legConf.AcCntlrId]; !ok {
			cntlrIdToLegList[legConf.AcCntlrId] = make([]*pbcp.LegConf, 0)
		}
		legList, _ := cntlrIdToLegList[legConf.AcCntlrId]
		legList = append(legList, legConf)
		cntlrIdToLegList[legConf.AcCntlrId] = legList
		for _, grpConf := range legConf.GrpConfList {
			grpIdToConf[grpConf.GrpId] = grpConf
			for _, ldConf := range grpConf.LdConfList {
				cnIdList := make([]string, 1)
				cnIdList[0] = legConf.AcCntlrId
				ldIdToCnIdList[ldConf.LdId] = cnIdList
			}
		}
	}
	return &storagePoolMap{
		legIdToConf:      legIdToConf,
		grpIdToConf:      grpIdToConf,
		ldIdToCnIdList:   ldIdToCnIdList,
		cntlrIdToLegList: cntlrIdToLegList,
	}
}

func (spwkr *spWorkerServer) updateSpInfo(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	reply *pbnd.SyncupDnReply,
	repErr error,
) {
	return
}

func (spwkr *spWorkerServer) syncupSpLd(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	conn *grpc.ClientConn,
	ldConf *pbcp.LdConf,
	spMap *storagePoolMap,
) *pbnd.SpLdInfo {
	req := &pbnd.SyncupSpLdRequest{
		SpLdConf: &pbnd.SpLdConf{
			DnId:     ldConf.DnId,
			SpId:     spId,
			LdId:     ldConf.LdId,
			Revision: revision,
			Start:    ldConf.Start,
			Length:   ldConf.Length,
			CnIdList: spMap.ldIdToCnIdList[ldConf.LdId],
			Inited:   ldConf.Inited,
		},
	}
	client := pbnd.NewDiskNodeAgentClient(conn)
	reply, err := client.SyncupSpLd(pch.Ctx, req)
	if err != nil {
		return &pbnd.SpLdInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeUnreachable,
				Msg:       err.Error(),
				Timestamp: time.Now().UnixMilli(),
			},
		}
	}
	return reply.SpLdInfo
}

func (spwkr *spWorkerServer) syncupSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	conn *grpc.ClientConn,
	spConf *pbcp.StoragePoolConf,
	cntlrConf *pbcp.CntlrConf,
	spMap *storagePoolMap,
) *pbnd.SpCntlrInfo {
	var creatingSnapConf *pbnd.SnapConf
	if spConf.CreatingSnapConf != nil {
		creatingSnapConf = &pbnd.SnapConf{
			DevId: spConf.CreatingSnapConf.DevId,
			OriId: spConf.CreatingSnapConf.OriId,
		}
	}
	var deletingSnapConf *pbnd.SnapConf
	if spConf.DeletingSnapConf != nil {
		deletingSnapConf = &pbnd.SnapConf{
			DevId: spConf.CreatingSnapConf.DevId,
			OriId: spConf.CreatingSnapConf.OriId,
		}
	}
	ssConfList := make([]*pbnd.SsConf, 0)
	localLegConfList := make([]*pbnd.LocalLegConf, 0)
	remoteLegConfList := make([]*pbnd.RemoteLegConf, 0)
	mtConfList := make([]*pbnd.MtConf, 0)
	itConfList := make([]*pbnd.ItConf, 0)
	req := &pbnd.SyncupSpCntlrRequest{
		SpCntlrConf: &pbnd.SpCntlrConf{
			CnId:     cntlrConf.CnId,
			SpId:     spId,
			CntlrId:  cntlrConf.CntlrId,
			Revision: revision,
			CntlrIdx: cntlrConf.CntlrIdx,
			NvmePortConf: &pbnd.NvmePortConf{
				PortNum: cntlrConf.NvmePortConf.PortNum,
				NvmeListener: &pbnd.NvmeListener{
					TrType:  cntlrConf.NvmePortConf.NvmeListener.TrType,
					AdrFam:  cntlrConf.NvmePortConf.NvmeListener.AdrFam,
					TrAddr:  cntlrConf.NvmePortConf.NvmeListener.TrAddr,
					TrSvcId: cntlrConf.NvmePortConf.NvmeListener.TrSvcId,
				},
				TrEq: &pbnd.NvmeTReq{
					SeqCh: cntlrConf.NvmePortConf.TrEq.SeqCh,
				},
			},
			SsConfList: ssConfList,
			ActiveCntlrConf: &pbnd.ActiveCntlrConf{
				StripeConf: &pbnd.StripeConf{
					ChunkSize: spConf.GeneralConf.StripeConf.ChunkSize,
				},
				ThinPoolConf: &pbnd.ThinPoolConf{
					DataBlockSize:  spConf.GeneralConf.ThinPoolConf.DataBlockSize,
					LowWaterMark:   spConf.GeneralConf.ThinPoolConf.LowWaterMark,
					ErrorIfNoSpace: spConf.GeneralConf.ThinPoolConf.ErrorIfNoSpace,
				},
				RedundancyConf: &pbnd.RedundancyConf{
					RedunType:       spConf.GeneralConf.RedundancyConf.RedunType,
					RegionSize:      spConf.GeneralConf.RedundancyConf.RegionSize,
					ChunkSize:       spConf.GeneralConf.RedundancyConf.ChunkSize,
					DaemonSleep:     spConf.GeneralConf.RedundancyConf.DaemonSleep,
					MinRecoveryRate: spConf.GeneralConf.RedundancyConf.MinRecoveryRate,
					MaxRecoveryRate: spConf.GeneralConf.RedundancyConf.MaxRecoveryRate,
					StripeCache:     spConf.GeneralConf.RedundancyConf.StripeCache,
					JournalMode:     spConf.GeneralConf.RedundancyConf.JournalMode,
				},
				CreatingSnapConf:  creatingSnapConf,
				DeletingSnapConf:  deletingSnapConf,
				LocalLegConfList:  localLegConfList,
				RemoteLegConfList: remoteLegConfList,
				MtConfList:        mtConfList,
				ItConfList:        itConfList,
			},
		},
	}
	client := pbnd.NewControllerNodeAgentClient(conn)
	reply, err := client.SyncupSpCntlr(pch.Ctx, req)
	if err != nil {
		return &pbnd.SpCntlrInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeUnreachable,
				Msg:       err.Error(),
				Timestamp: time.Now().UnixMilli(),
			},
		}
	}
	return reply.SpCntlrInfo
}

func (spwkr *spWorkerServer) updateConfAndInfo(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
	ldIdToInfo map[string]*pbnd.SpLdInfo,
	cntlrIdToInfo map[string]*pbnd.SpCntlrInfo,
	updateConf bool,
	spMap *storagePoolMap,
) bool {
	return false
}

func (spwkr *spWorkerServer) syncupAllLdAndCntlr(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
	spMap *storagePoolMap,
) bool {
	allSucceeded := true
	updateConf := false
	ldIdToInfo := make(map[string]*pbnd.SpLdInfo)
	for _, legConf := range spConf.LegConfList {
		for _, grpConf := range legConf.GrpConfList {
			for _, ldConf := range grpConf.LdConfList {
				conn, _ := targetToConn[ldConf.DnGrpcTarget]
				spLdInfo := spwkr.syncupSpLd(
					pch,
					spId,
					revision,
					conn,
					ldConf,
					spMap,
				)
				if spLdInfo.StatusInfo.Code != constants.StatusCodeSucceed {
					allSucceeded = false
				}
				if !ldConf.Inited &&
					spLdInfo.StatusInfo.Code == constants.StatusCodeSucceed {
					ldConf.Inited = true
					updateConf = true
				}
				ldIdToInfo[ldConf.LdId] = spLdInfo
			}
		}
	}

	cntlrIdToInfo := make(map[string]*pbnd.SpCntlrInfo)
	for _, cntlrConf := range spConf.CntlrConfList {
		conn, _ := targetToConn[cntlrConf.CnGrpcTarget]
		spCntlrInfo := spwkr.syncupSpCntlr(
			pch,
			spId,
			revision,
			conn,
			spConf,
			cntlrConf,
			spMap,
		)
		if spCntlrInfo.StatusInfo.Code != constants.StatusCodeSucceed {
			allSucceeded = false
		}
		for _, localLegInfo := range spCntlrInfo.ActiveCntlrInfo.LocalLegInfoList {
			legConf := spMap.legIdToConf[localLegInfo.LegId]
			if legConf.Reload &&
				localLegInfo.StatusInfo.Code == constants.StatusCodeSucceed {
				legConf.Reload = false
				updateConf = true
			}
			for _, grpInfo := range localLegInfo.GrpInfoList {
				grpConf := spMap.grpIdToConf[grpInfo.GrpId]
				if grpConf.NoSync &&
					grpInfo.StatusInfo.Code == constants.StatusCodeSucceed {
					grpConf.NoSync = false
					updateConf = true
				}
			}
		}
		cntlrIdToInfo[cntlrConf.CntlrId] = spCntlrInfo
	}
	if ret := spwkr.updateConfAndInfo(
		pch,
		spId,
		revision,
		spConf,
		ldIdToInfo,
		cntlrIdToInfo,
		updateConf,
		spMap,
	); !ret {
		allSucceeded = false
	}
	return allSucceeded
}

func (spwkr *spWorkerServer) syncupSp(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
	spMap *storagePoolMap,
) bool {
	interval := constants.SpRetryBase
	for {
		if allSucceeded := spwkr.syncupAllLdAndCntlr(
			targetToConn,
			pch,
			spId,
			revision,
			spConf,
			spMap,
		); allSucceeded {
			return false
		}
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(interval):
			// FIXME: support fast retry
			interval = constants.SpRetryBase
		}
	}

}

func (spwkr *spWorkerServer) checkSp(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
) bool {
	return false
}

func (spwkr *spWorkerServer) trackRes(
	spId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if !ok {
		pch.Logger.Fatal("Can not find spId: %s", spId)
	}
	if len(revToConf) != 1 {
		pch.Logger.Fatal("revToConf cnt error: %s %v", spId, revToConf)
	}
	var revision int64
	var spConf *pbcp.StoragePoolConf
	for key, value := range revToConf {
		revision = key
		spConf = value
		break
	}

	spMap := generateSpMap(spConf)
	for {
		// FIXME: implement sp error handling
		if exit := spwkr.syncupSp(
			targetToConn,
			pch,
			spId,
			revision,
			spConf,
			spMap,
		); exit {
			return
		}
		if exit := spwkr.checkSp(
			targetToConn,
			pch,
			spId,
			revision,
		); exit {
			return
		}
	}
}

func newSpWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *spWorkerServer {
	return &spWorkerServer{
		etcdCli:        etcdCli,
		kf:             keyfmt.NewKeyFmt(prefix),
		sm:             stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:    make(chan struct{}),
		idAndRevToConf: make(map[string]map[int64]*pbcp.StoragePoolConf),
	}
}
