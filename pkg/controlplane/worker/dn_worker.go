package worker

import (
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnWorkerServer struct {
	pbcp.UnimplementedDiskNodeWorkerServer
	etcdCli *clientv3.Client
	kf      *keyfmt.KeyFmt
	sm      *stmwrapper.StmWrapper
	initTrigger chan struct{}
	mu          sync.Mutex
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
	resBody string,
	rev int64,
) ([]string, error) {
	return nil, nil
}

func (dnwkr *dnWorkerServer) delResRev(
	resId string,
	rev int64,
) error {
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
		etcdCli:      etcdCli,
		kf:           keyfmt.NewKeyFmt(prefix),
		sm:           stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:  make(chan struct{}),
	}
}
