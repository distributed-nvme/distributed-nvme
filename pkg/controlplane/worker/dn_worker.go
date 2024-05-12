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
	dnFastRetryCodeMap = make(map[uint32]bool)
)

type dnResource struct {
	dnId   string
	dnConf *pbcp.DiskNodeConf
}

func (dnRes *dnResource) GetId() string {
	return dnRes.dnId
}

func (dnRes *dnResource) GetQos() uint32 {
	return dnRes.dnConf.GeneralConf.DnCapacity.FreeQos
}

func (dnRes *dnResource) GetCnt() uint32 {
	return dnRes.dnConf.GeneralConf.DnCapacity.LdCnt
}

func newDnResource(
	dnId string,
	dnConf *pbcp.DiskNodeConf,
) *dnResource {
	return &dnResource{
		dnId:   dnId,
		dnConf: dnConf,
	}
}

type dnWorkerServer struct {
	pbcp.UnimplementedDiskNodeWorkerServer
	mu            sync.Mutex
	etcdCli       *clientv3.Client
	kf            *keyfmt.KeyFmt
	sm            *stmwrapper.StmWrapper
	initTrigger   chan struct{}
	idAndRevToRes map[string]map[int64]*dnResource
	dnResTree     *restree.ResourceTree
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
	dnwkr.mu.Lock()
	defer dnwkr.mu.Unlock()

	dnConf := &pbcp.DiskNodeConf{}
	if err := proto.Unmarshal(resBody, dnConf); err != nil {
		return nil, err
	}
	dnRes := newDnResource(dnId, dnConf)
	revToRes, ok := dnwkr.idAndRevToRes[dnId]
	if ok {
		if len(revToRes) > 1 {
			panic("More than 1 dn rev: " + dnId)
		}
		for _, oldDnRes := range revToRes {
			dnwkr.dnResTree.Remove(oldDnRes)
		}
	} else {
		revToRes = make(map[int64]*dnResource)
		dnwkr.idAndRevToRes[dnId] = revToRes
	}
	revToRes[rev] = dnRes
	dnwkr.dnResTree.Put(dnRes)
	grpcTargetList := make([]string, 1)
	grpcTargetList[0] = dnConf.GeneralConf.GrpcTarget
	return grpcTargetList, nil
}

func (dnwkr *dnWorkerServer) delResRev(
	dnId string,
	rev int64,
) error {
	dnwkr.mu.Lock()
	defer dnwkr.mu.Unlock()

	revToRes, ok := dnwkr.idAndRevToRes[dnId]
	if !ok {
		panic("Unknown dn id: " + dnId)
	}
	dnRes, _ := revToRes[rev]
	delete(revToRes, rev)
	if len(revToRes) == 0 {
		dnwkr.dnResTree.Remove(dnRes)
		delete(dnwkr.idAndRevToRes, dnId)
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
	revToRes, ok := dnwkr.idAndRevToRes[dnId]
	if !ok {
		pch.Logger.Fatal("Can not find dnId: %s", dnId)
	}
	if len(revToRes) != 1 {
		pch.Logger.Fatal("revToRes cnt error: %s %v", dnId, revToRes)
	}
	var revision int64
	var dnConf *pbcp.DiskNodeConf
	for key, value := range revToRes {
		revision = key
		dnConf = value.dnConf
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

func (dnwkr *dnWorkerServer) AllocateDn(
	ctx context.Context,
	req *pbcp.AllocateDnRequest,
) (*pbcp.AllocateDnReply, error) {
	dnwkr.mu.Lock()
	defer dnwkr.mu.Unlock()

	dnItemList := make([]*pbcp.DnAllocItem, 0)
	distinguishMap := make(map[string]bool)

	excludeMap := make(map[string]bool)
	for _, dnId := range req.ExcludeIdList {
		excludeMap[dnId] = true
	}

	apply := func(res restree.Resource) bool {
		dnRes := res.(*dnResource)

		if _, ok := excludeMap[dnRes.dnId]; ok {
			return false
		}

		value := ""
		for _, tag := range dnRes.dnConf.TagList {
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

		if !dnRes.dnConf.GeneralConf.Online {
			return false
		}

		if dnRes.dnConf.GeneralConf.DnCapacity.DataMaxExtentSetSize < req.DataExtentCnt {
			return false
		}

		distinguishMap[value] = true

		item := &pbcp.DnAllocItem{
			DnId:             dnRes.dnId,
			DistinguishValue: value,
		}
		dnItemList = append(dnItemList, item)
		if len(dnItemList) < int(req.DnCnt) {
			return false
		}

		return true
	}

	dnwkr.dnResTree.IterateAt(req.Qos, apply)

	return &pbcp.AllocateDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
		DnItemList: dnItemList,
	}, nil
}

func newDnWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *dnWorkerServer {
	return &dnWorkerServer{
		etcdCli:       etcdCli,
		kf:            keyfmt.NewKeyFmt(prefix),
		sm:            stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:   make(chan struct{}),
		idAndRevToRes: make(map[string]map[int64]*dnResource),
		dnResTree:     restree.NewResourceTree(),
	}
}
