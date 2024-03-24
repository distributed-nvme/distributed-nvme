package controlplane

import (
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbCpApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplaneapi"
)

type cpApiServer struct {
	pbCpApi.UnimplementedControlPlaneServer
	etcdCli *clientv3.Client
	logger *lib.Logger
	kf *lib.KeyFmt
	agentTimeout time.Duration
}

func newCpApiServer(
	etcdCli *clientv3.Client,
	logger *lib.Logger,
	prefix string,
) *cpApiServer {
	return &cpApiServer{
		etcdCli: etcdCli,
		logger: logger,
		kf: lib.NewKeyFmt(prefix),
		agentTimeout: time.Duration(lib.AgentTimeoutSecondDefault)*time.Second,
	}
}
