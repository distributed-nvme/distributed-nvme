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
	agentTimeout time.Duration
}

func newCpApiServer(etcdCli *clientv3.Client, logger *lib.Logger) *cpApiServer {
	return &cpApiServer{
		etcdCli: etcdCli,
		logger: logger,
		agentTimeout: time.Duration(lib.DefaultAgentTimeoutSecond)*time.Second,
	}
}
