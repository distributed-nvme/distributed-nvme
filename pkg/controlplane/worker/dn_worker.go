package worker

import (
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
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
	resId string,
	resBody []byte,
	rev int64,
) ([]string, error) {
	dnConf := &pbcp.DiskNodeConf{}
	if err := proto.Unmarshal(resBody, dnConf); err != nil {
		return nil, err
	}
	revToConf, ok := dnwkr.idAndRevToConf[resId]
	if ok {
		if len(revToConf) > 1 {
			panic("More than 1 dn rev: " + resId)
		}
	} else {
		revToConf = make(map[int64]*pbcp.DiskNodeConf)
		dnwkr.idAndRevToConf[resId] = revToConf
	}
	revToConf[rev] = dnConf
	grpcTargetList := make([]string, 0)
	grpcTargetList = append(grpcTargetList, dnConf.GeneralConf.GrpcTarget)
	return grpcTargetList, nil
}

func (dnwkr *dnWorkerServer) delResRev(
	resId string,
	rev int64,
) error {
	revToConf, ok := dnwkr.idAndRevToConf[resId]
	if !ok {
		panic("Unknown dn id: " + resId)
	}
	delete(revToConf, rev)
	if len(revToConf) == 0 {
		delete(dnwkr.idAndRevToConf, resId)
	}
	return nil
}

func (dnwkr *dnWorkerServer) trackRes(
	resId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	return
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
