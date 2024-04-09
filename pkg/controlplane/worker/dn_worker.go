package worker

import (
	"strconv"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
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
	grpcTargetList := make([]string, 0)
	grpcTargetList = append(grpcTargetList, dnConf.GeneralConf.GrpcTarget)
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

func syncup(
	client pbnd.DiskNodeAgentClient,
	pch *ctxhelper.PerCtxHelper,
	dnId uint64,
	revision int64,
	dnConf *pbcp.DiskNodeConf,
) bool {
	req := &pbnd.SyncupDnRequest{
		DnConf: &pbnd.DnConf{
			DnId:     dnId,
			Revision: revision,
			DevPath:  dnConf.GeneralConf.DevPath,
			NvmePortConf: &pbnd.NvmePortConf{
				PortNum: dnConf.GeneralConf.NvmePortConf.PortNum,
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
		},
	}

	interval := 1 * time.Second
	for {
		reply, err := client.SyncupDn(pch.Ctx, req)
		if err == nil {
			if reply.DnInfo.StatusInfo.Code == 0 {
				return false
			}
		}
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(interval):
			interval *= 2
			if interval > 32 {
				interval = 32
			}
		}
	}
}

func check(
	client pbnd.DiskNodeAgentClient,
	pch *ctxhelper.PerCtxHelper,
	dnId uint64,
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
		case <-time.After(1):
			reply, err := client.CheckDn(pch.Ctx, req)
			if err != nil {
				return false
			}
			if reply.DnInfo.StatusInfo.Code != 0 {
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
	dnIdNum, err := strconv.ParseUint(dnId, 16, 64)
	if err != nil {
		pch.Logger.Fatal("Invalid dnId: %s", dnId)
	}
	for {
		if exit := syncup(client, pch, dnIdNum, revision, dnConf); exit {
			return
		}
		if exit := check(client, pch, dnIdNum, revision); exit {
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
