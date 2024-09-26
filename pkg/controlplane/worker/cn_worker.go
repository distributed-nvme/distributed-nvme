package worker

import (
	"context"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/restree"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

var (
	cnFastRetryCodeMap = make(map[uint32]bool)
)

type cnResource struct {
	cnId   string
	cnConf *pbcp.ControllerNodeConf
}

func (cnRes *cnResource) GetId() string {
	return cnRes.cnId
}

func (cnRes *cnResource) GetQos() uint32 {
	return cnRes.cnConf.GeneralConf.CnCapacity.FreeQos
}

func (cnRes *cnResource) GetCnt() uint32 {
	return cnRes.cnConf.GeneralConf.CnCapacity.CntlrCnt
}

func newCnResource(
	cnId string,
	cnConf *pbcp.ControllerNodeConf,
) *cnResource {
	return &cnResource{
		cnId:   cnId,
		cnConf: cnConf,
	}
}

type cnWorkerServer struct {
	pbcp.UnimplementedControllerNodeWorkerServer
	mu            sync.Mutex
	etcdCli       *clientv3.Client
	kf            *keyfmt.KeyFmt
	sm            *stmwrapper.StmWrapper
	initTrigger   chan struct{}
	idAndRevToRes map[string]map[int64]*cnResource
	cnResTree     *restree.ResourceTree
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
	cnwkr.mu.Lock()
	defer cnwkr.mu.Unlock()

	cnConf := &pbcp.ControllerNodeConf{}
	if err := proto.Unmarshal(resBody, cnConf); err != nil {
		return nil, err
	}
	cnRes := newCnResource(cnId, cnConf)
	revToRes, ok := cnwkr.idAndRevToRes[cnId]
	if ok {
		if len(revToRes) > 1 {
			panic("More than 1 cn rev: " + cnId)
		}
		for _, oldCnRes := range revToRes {
			cnwkr.cnResTree.Remove(oldCnRes)
		}
	} else {
		revToRes = make(map[int64]*cnResource)
		cnwkr.idAndRevToRes[cnId] = revToRes
	}
	revToRes[rev] = cnRes
	cnwkr.cnResTree.Put(cnRes)
	grpcTargetList := make([]string, 1)
	grpcTargetList[0] = cnConf.GeneralConf.GrpcTarget
	return grpcTargetList, nil
}

func (cnwkr *cnWorkerServer) delResRev(
	cnId string,
	rev int64,
) error {
	cnwkr.mu.Lock()
	defer cnwkr.mu.Unlock()

	revToRes, ok := cnwkr.idAndRevToRes[cnId]
	if !ok {
		panic("Unknown cn id: " + cnId)
	}
	cnRes, _ := revToRes[rev]
	delete(revToRes, rev)
	if len(revToRes) == 0 {
		cnwkr.cnResTree.Remove(cnRes)
		delete(cnwkr.idAndRevToRes, cnId)
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
					"Ignore old cn ConfRev: %d %d",
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
		cnwkr.updateCnInfo(pch, cnId, revision, reply, err)
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
				pch.Logger.Error("cn failed, cnId=%v", cnId)
				return false
			}
		}
	}
}

func (cnwkr *cnWorkerServer) trackRes(
	cnId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	revToRes, ok := cnwkr.idAndRevToRes[cnId]
	if !ok {
		pch.Logger.Fatal("Can not find cnId: %s", cnId)
	}
	if len(revToRes) != 1 {
		pch.Logger.Fatal("revToRes cnt error: %s %v", cnId, revToRes)
	}
	var revision int64
	var cnConf *pbcp.ControllerNodeConf
	for key, value := range revToRes {
		revision = key
		cnConf = value.cnConf
		break
	}
	grpcTarget := cnConf.GeneralConf.GrpcTarget
	conn, ok := targetToConn[grpcTarget]
	if !ok {
		pch.Logger.Fatal("Can not find grpcTarget: %s %v", grpcTarget, targetToConn)
	}
	client := pbnd.NewControllerNodeAgentClient(conn)
	for {
		if exit := cnwkr.syncupCn(client, pch, cnId, revision, cnConf); exit {
			return
		}
		if exit := cnwkr.checkCn(client, pch, cnId, revision); exit {
			return
		}
	}
}

func (cnwkr *cnWorkerServer) AllocateCn(
	ctx context.Context,
	req *pbcp.AllocateCnRequest,
) (*pbcp.AllocateCnReply, error) {
	cnwkr.mu.Lock()
	defer cnwkr.mu.Unlock()

	cnItemList := make([]*pbcp.CnAllocateItem, 0)
	distinguishMap := make(map[string]bool)

	excludeMap := make(map[string]bool)
	for _, cnId := range req.ExcludeIdList {
		excludeMap[cnId] = true
	}

	apply := func(res restree.Resource) bool {
		cnRes := res.(*cnResource)

		if _, ok := excludeMap[cnRes.cnId]; ok {
			return false
		}

		value := ""
		for _, tag := range cnRes.cnConf.TagList {
			if tag.Key == req.DistinguishKey {
				value = tag.Value
			}
		}

		// ignore it if no distinguishKey
		if value == "" {
			return false
		}

		// ignore it if we already have the distinguishValue
		if _, ok := distinguishMap[value]; ok {
			return false
		}

		if !cnRes.cnConf.GeneralConf.Online {
			return false
		}

		distinguishMap[value] = true

		item := &pbcp.CnAllocateItem{
			CnId:             cnRes.cnId,
			DistinguishValue: value,
		}
		cnItemList = append(cnItemList, item)
		if len(cnItemList) < int(req.CnCnt) {
			return false
		}

		return true
	}

	cnwkr.cnResTree.IterateAt(req.Qos, apply)

	return &pbcp.AllocateCnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
		CnItemList: cnItemList,
	}, nil
}

func newCnWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *cnWorkerServer {
	return &cnWorkerServer{
		etcdCli:       etcdCli,
		kf:            keyfmt.NewKeyFmt(prefix),
		sm:            stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:   make(chan struct{}),
		idAndRevToRes: make(map[string]map[int64]*cnResource),
		cnResTree:     restree.NewResourceTree(),
	}
}
