package controlplane

import (
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbCpApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/cpapi"
)

type cpApiServer struct {
	pbCpApi.UnimplementedControlPlaneServer
	etcdCli *clientv3.Client
	logger *lib.Logger
}

func newCpApiServer(etcdCli *clientv3.Client, logger *lib.Logger) *cpApiServer {
	return &cpApiServer{
		etcdCli: etcdCli,
		logger: logger,
	}
}
