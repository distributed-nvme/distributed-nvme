package worker

import (
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
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
	cnFastRetryCodeMap = make(map[uint32]bool)
)

type cnWorkerServer struct {
	pbcp.UnimplementedControllerNodeWorkerServer
	mu             sync.Mutex
	etcdCli        *clientv3.Client
	kf             *keyfmt.KeyFmt
	sm             *stmwrapper.StmWrapper
	initTrigger    chan struct{}
	idAndRevToConf map[string]map[int64]*pbcp.ControllerNodeConf
}

func (cnwkr *cnWorkerServer) getName() string {
	return "cn"
}

func (cnwkr *cnWorkerServer) getEtcdCli() *clientv3.Client {
	return cnwkr.etcdCli
}

func (cnwkr *cnWorkerServer) getMemberPrefix() string {
	return cnwkr.kf.CnMemberPrefix()
}

func (cnwkr *cnWorkerServer) getResPrefix() string {
	return cnwkr.kf.CnConfEntityPrefix()
}

func (cnwkr *cnWorkerServer) getInitTrigger() <-chan struct{} {
	return cnwkr.initTrigger
}

func (cnwkr *cnWorkerServer) addResRev(
	cnId string,
	resBody []byte,
	rev int64,
) ([]string, error) {
	cnConf := &pbcp.ControllerNodeConf{}
	if err := proto.Unmarshal(resBody, cnConf); err != nil {
		return nil, err
	}
	revToConf, ok := cnwkr.idAndRevToConf[cnId]
	if ok {
		if len(revToConf) > 1 {
			panic("More than 1 cn rev: " + cnId)
		}
	} else {
		revToConf = make(map[int64]*pbcp.ControllerNodeConf)
		cnwkr.idAndRevToConf[cnId] = revToConf
	}
	revToConf[rev] = cnConf
	grpcTargetList := make([]string, 1)
	grpcTargetList[0] = cnConf.GeneralConf.GrpcTarget
	return grpcTargetList, nil
}

func (cnwkr *cnWorkerServer) delResRev(
	cnId string,
	rev int64,
) error {
	revToConf, ok := cnwkr.idAndRevToConf[cnId]
	if !ok {
		panic("Unknown cn id: " + cnId)
	}
	delete(revToConf, rev)
	if len(revToConf) == 0 {
		delete(cnwkr.idAndRevToConf, cnId)
	}
	return nil
}

func generateCnStatusInfo(
	reply *pbnd.SyncupCnReply,
	repErr error,
) *pbcp.StatusInfo {
	if repErr != nil {
		return &pbcp.StatusInfo{
			Code:      constants.StatusCodeUnreachable,
			Msg:       repErr.Error(),
			Timestamp: time.Now().UnixMilli(),
		}
	}
	return &pbcp.StatusInfo{
		Code:      reply.CnInfo.StatusInfo.Code,
		Msg:       reply.CnInfo.StatusInfo.Msg,
		Timestamp: reply.CnInfo.StatusInfo.Timestamp,
	}
}

func (cnwkr *cnWorkerServer) updateCnInfo(
	pch *ctxhelper.PerCtxHelper,
	cnId string,
	revision int64,
	reply *pbnd.SyncupCnReply,
	repErr error,
) {
	statusInfo := generateCnStatusInfo(reply, repErr)
	cnInfo := &pbcp.ControllerNodeInfo{
		ConfRev:    revision,
		StatusInfo: statusInfo,
	}
	cnInfoKey := cnwkr.kf.CnInfoEntityKey(cnId)
	cnInfoVal, err := proto.Marshal(cnInfo)
	if err != nil {
		pch.Logger.Fatal("Marshal cnInfo err: %v %v", cnInfo, err)
	}
	cnInfoValStr := string(cnInfoVal)

	oldCnInfo := &pbcp.ControllerNodeInfo{}
	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(cnInfoKey))
		if len(val) > 0 {
			if err := proto.Unmarshal(val, oldCnInfo); err != nil {
				pch.Logger.Fatal(
					"get oldCnInfo err: %v %v",
					cnInfoKey,
					err,
				)
			}
			if oldCnInfo.ConfRev > revision {
				pch.Logger.Warning(
					"Ignore old cn  ConffRev: %d %d",
					oldCnInfo.ConfRev,
					revision,
				)
				return nil
			}
		}
		stm.Put(cnInfoKey, cnInfoValStr)
		return nil
	}

	if err := cnwkr.sm.RunStm(pch, apply); err != nil {
		pch.Logger.Error("Update cnInfo err: %s %v", cnId, err)
	}
}

func (cnwkr *cnWorkerServer) syncupCn(
	client pbnd.ControllerNodeAgentClient,
	pch *ctxhelper.PerCtxHelper,
	cnId string,
	revision int64,
	cnConf *pbcp.ControllerNodeConf,
) bool {
	spCntlrIdList := make([]*pbnd.SpCntlrId, len(cnConf.SpCntlrIdList))
	for i, spCntlrId := range cnConf.SpCntlrIdList {
		spCntlrIdList[i] = &pbnd.SpCntlrId{
			SpId:    spCntlrId.SpId,
			CntlrId: spCntlrId.CntlrId,
		}
	}
	req := &pbnd.SyncupCnRequest{
		CnConf: &pbnd.CnConf{
			CnId:     cnId,
			Revision: revision,
			NvmePortConf: &pbnd.NvmePortConf{
				PortNum: string(cnConf.GeneralConf.NvmePortConf.PortNum),
				NvmeListener: &pbnd.NvmeListener{
					TrType:  cnConf.GeneralConf.NvmePortConf.NvmeListener.TrType,
					AdrFam:  cnConf.GeneralConf.NvmePortConf.NvmeListener.AdrFam,
					TrAddr:  cnConf.GeneralConf.NvmePortConf.NvmeListener.TrAddr,
					TrSvcId: cnConf.GeneralConf.NvmePortConf.NvmeListener.TrSvcId,
				},
				TrEq: &pbnd.NvmeTReq{
					SeqCh: cnConf.GeneralConf.NvmePortConf.TrEq.SeqCh,
				},
			},
			SpCntlrIdList: spCntlrIdList,
		},
	}

	interval := constants.CnRetryBase
	fastRetry := false
	for {
		reply, err := client.SyncupCn(pch.Ctx, req)
		if err == nil {
			if reply.CnInfo.StatusInfo.Code == constants.StatusCodeSucceed {
				return false
			}
			_, ok := cnFastRetryCodeMap[reply.CnInfo.StatusInfo.Code]
			if ok {
				fastRetry = true
			}
		}
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(interval):
			if fastRetry {
				interval = constants.CnRetryBase
			} else {
				interval *= constants.CnRetryPower
				if interval > constants.CnRetryMax {
					interval = constants.CnRetryMax
				}
			}
		}
	}
}

func (cnwkr *cnWorkerServer) checkCn(
	client pbnd.ControllerNodeAgentClient,
	pch *ctxhelper.PerCtxHelper,
	cnId string,
	revision int64,
) bool {
	req := &pbnd.CheckCnRequest{
		CnId:     cnId,
		Revision: revision,
	}
	for {
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(constants.CnCheckInterval):
			reply, err := client.CheckCn(pch.Ctx, req)
			if err != nil {
				return false
			}
			if reply.CnInfo.StatusInfo.Code != constants.StatusCodeSucceed {
				pch.Logger.Error("cn failed")
			}
		}
	}
}

func (cnwkr *cnWorkerServer) trackRes(
	cnId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	revToConf, ok := cnwkr.idAndRevToConf[cnId]
	if !ok {
		pch.Logger.Fatal("Can not find cnId: %s", cnId)
	}
	if len(revToConf) != 1 {
		pch.Logger.Fatal("revToConf cnt error: %s %v", cnId, revToConf)
	}
	var revision int64
	var cnConf *pbcp.ControllerNodeConf
	for key, value := range revToConf {
		revision = key
		cnConf = value
		break
	}
	grpcTarget := cnConf.GeneralConf.GrpcTarget
	conn, ok := targetToConn[grpcTarget]
	if !ok {
		pch.Logger.Fatal("Can not find grpcTarget: %s %v", grpcTarget, targetToConn)
	}
	client := pbnd.NewControllerNodeAgentClient(conn)
	for {
		// FIXME: implement cn error handling
		if exit := cnwkr.syncupCn(client, pch, cnId, revision, cnConf); exit {
			return
		}
		if exit := cnwkr.checkCn(client, pch, cnId, revision); exit {
			return
		}
	}
}

func newCnWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *cnWorkerServer {
	return &cnWorkerServer{
		etcdCli:        etcdCli,
		kf:             keyfmt.NewKeyFmt(prefix),
		sm:             stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:    make(chan struct{}),
		idAndRevToConf: make(map[string]map[int64]*pbcp.ControllerNodeConf),
	}
}
