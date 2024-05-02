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
	dnFastRetryCodeMap = make(map[uint32]bool)
)

type dnWorkerServer struct {
	pbcp.UnimplementedDiskNodeWorkerServer
	mu             sync.Mutex
	etcdCli        *clientv3.Client
	kf             *keyfmt.KeyFmt
	sm             *stmwrapper.StmWrapper
	initTrigger    chan struct{}
	idAndRevToConf map[string]map[int64]*pbcp.DiskNodeConf
}

func (dnwkr *dnWorkerServer) getName() string {
	return "dn"
}

func (dnwkr *dnWorkerServer) getEtcdCli() *clientv3.Client {
	return dnwkr.etcdCli
}

func (dnwkr *dnWorkerServer) getMemberPrefix() string {
	return dnwkr.kf.DnMemberPrefix()
}

func (dnwkr *dnWorkerServer) getResPrefix() string {
	return dnwkr.kf.DnConfEntityPrefix()
}

func (dnwkr *dnWorkerServer) getInitTrigger() <-chan struct{} {
	return dnwkr.initTrigger
}

func (dnwkr *dnWorkerServer) addResRev(
	dnId string,
	resBody []byte,
	rev int64,
) ([]string, error) {
	dnConf := &pbcp.DiskNodeConf{}
	if err := proto.Unmarshal(resBody, dnConf); err != nil {
		return nil, err
	}
	revToConf, ok := dnwkr.idAndRevToConf[dnId]
	if ok {
		if len(revToConf) > 1 {
			panic("More than 1 dn rev: " + dnId)
		}
	} else {
		revToConf = make(map[int64]*pbcp.DiskNodeConf)
		dnwkr.idAndRevToConf[dnId] = revToConf
	}
	revToConf[rev] = dnConf
	grpcTargetList := make([]string, 1)
	grpcTargetList[0] = dnConf.GeneralConf.GrpcTarget
	return grpcTargetList, nil
}

func (dnwkr *dnWorkerServer) delResRev(
	dnId string,
	rev int64,
) error {
	revToConf, ok := dnwkr.idAndRevToConf[dnId]
	if !ok {
		panic("Unknown dn id: " + dnId)
	}
	delete(revToConf, rev)
	if len(revToConf) == 0 {
		delete(dnwkr.idAndRevToConf, dnId)
	}
	return nil
}

func generateDnStatusInfo(
	reply *pbnd.SyncupDnReply,
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
		Code:      reply.DnInfo.StatusInfo.Code,
		Msg:       reply.DnInfo.StatusInfo.Msg,
		Timestamp: reply.DnInfo.StatusInfo.Timestamp,
	}
}

func (dnwkr *dnWorkerServer) updateDnInfo(
	pch *ctxhelper.PerCtxHelper,
	dnId string,
	revision int64,
	reply *pbnd.SyncupDnReply,
	repErr error,
) {
	statusInfo := generateDnStatusInfo(reply, repErr)
	dnInfo := &pbcp.DiskNodeInfo{
		ConfRev:    revision,
		StatusInfo: statusInfo,
	}
	dnInfoKey := dnwkr.kf.DnInfoEntityKey(dnId)
	dnInfoVal, err := proto.Marshal(dnInfo)
	if err != nil {
		pch.Logger.Fatal("Marshal dnInfo err: %v %v", dnInfo, err)
	}
	dnInfoValStr := string(dnInfoVal)

	oldDnInfo := &pbcp.DiskNodeInfo{}
	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(dnInfoKey))
		if len(val) > 0 {
			if err := proto.Unmarshal(val, oldDnInfo); err != nil {
				pch.Logger.Fatal(
					"Get oldDnInfo err: %v %v",
					dnInfoKey,
					err,
				)
			}
			if oldDnInfo.ConfRev > revision {
				pch.Logger.Warning(
					"Ignore old dn ConfRev: %d %d",
					oldDnInfo.ConfRev,
					revision,
				)
				return nil
			}
		}
		stm.Put(dnInfoKey, dnInfoValStr)
		return nil
	}

	if err := dnwkr.sm.RunStm(pch, apply); err != nil {
		pch.Logger.Error("Update dnInfo err: %s %v", dnId, err)
	}
}

func (dnwkr *dnWorkerServer) syncupDn(
	client pbnd.DiskNodeAgentClient,
	pch *ctxhelper.PerCtxHelper,
	dnId string,
	revision int64,
	dnConf *pbcp.DiskNodeConf,
) bool {
	spLdIdList := make([]*pbnd.SpLdId, len(dnConf.SpLdIdList))
	for i, spLdId := range dnConf.SpLdIdList {
		spLdIdList[i] = &pbnd.SpLdId{
			SpId: spLdId.SpId,
			LdId: spLdId.LdId,
		}
	}
	req := &pbnd.SyncupDnRequest{
		DnConf: &pbnd.DnConf{
			DnId:     dnId,
			Revision: revision,
			DevPath:  dnConf.GeneralConf.DevPath,
			NvmePortConf: &pbnd.NvmePortConf{
				PortNum: string(dnConf.GeneralConf.NvmePortConf.PortNum),
				NvmeListener: &pbnd.NvmeListener{
					TrType:  dnConf.GeneralConf.NvmePortConf.NvmeListener.TrType,
					AdrFam:  dnConf.GeneralConf.NvmePortConf.NvmeListener.AdrFam,
					TrAddr:  dnConf.GeneralConf.NvmePortConf.NvmeListener.TrAddr,
					TrSvcId: dnConf.GeneralConf.NvmePortConf.NvmeListener.TrSvcId,
				},
				TrEq: &pbnd.NvmeTReq{
					SeqCh: dnConf.GeneralConf.NvmePortConf.TrEq.SeqCh,
				},
			},
			SpLdIdList: spLdIdList,
		},
	}

	interval := constants.DnRetryBase
	fastRetry := false
	for {
		reply, err := client.SyncupDn(pch.Ctx, req)
		dnwkr.updateDnInfo(pch, dnId, revision, reply, err)
		if err == nil {
			if reply.DnInfo.StatusInfo.Code == constants.StatusCodeSucceed {
				return false
			}
			_, ok := dnFastRetryCodeMap[reply.DnInfo.StatusInfo.Code]
			if ok {
				fastRetry = true
			}
		}
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(interval):
			if fastRetry {
				interval = constants.DnRetryBase
			} else {
				interval *= constants.DnRetryPower
				if interval > constants.DnRetryMax {
					interval = constants.DnRetryMax
				}
			}
		}
	}
}

func (dnwkr *dnWorkerServer) checkDn(
	client pbnd.DiskNodeAgentClient,
	pch *ctxhelper.PerCtxHelper,
	dnId string,
	revision int64,
) bool {
	req := &pbnd.CheckDnRequest{
		DnId:     dnId,
		Revision: revision,
	}
	for {
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(constants.DnCheckInterval):
			reply, err := client.CheckDn(pch.Ctx, req)
			if err != nil {
				return false
			}
			if reply.DnInfo.StatusInfo.Code != constants.StatusCodeSucceed {
				pch.Logger.Error("dn failed")
			}
		}
	}
}

func (dnwkr *dnWorkerServer) trackRes(
	dnId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	revToConf, ok := dnwkr.idAndRevToConf[dnId]
	if !ok {
		pch.Logger.Fatal("Can not find dnId: %s", dnId)
	}
	if len(revToConf) != 1 {
		pch.Logger.Fatal("revToConf cnt error: %s %v", dnId, revToConf)
	}
	var revision int64
	var dnConf *pbcp.DiskNodeConf
	for key, value := range revToConf {
		revision = key
		dnConf = value
		break
	}
	grpcTarget := dnConf.GeneralConf.GrpcTarget
	conn, ok := targetToConn[grpcTarget]
	if !ok {
		pch.Logger.Fatal("Can not find grpcTarget: %s %v", grpcTarget, targetToConn)
	}
	client := pbnd.NewDiskNodeAgentClient(conn)
	for {
		// FIXME: implement dn error handling
		if exit := dnwkr.syncupDn(client, pch, dnId, revision, dnConf); exit {
			return
		}
		if exit := dnwkr.checkDn(client, pch, dnId, revision); exit {
			return
		}
	}
}

func newDnWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *dnWorkerServer {
	return &dnWorkerServer{
		etcdCli:        etcdCli,
		kf:             keyfmt.NewKeyFmt(prefix),
		sm:             stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:    make(chan struct{}),
		idAndRevToConf: make(map[string]map[int64]*pbcp.DiskNodeConf),
	}
}
