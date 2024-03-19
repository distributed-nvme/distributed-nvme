package controlplane

import (
	clientv3 "go.etcd.io/etcd/client/v3"

	pbCpApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/cpapi"
)

type cpApiServer struct {
	pbCpApi.UnimplementedControlPlaneServer
	etcdCli *clientv3.Client
}

func newCpApiServer(etcdCli *clientv3.Client) *cpApiServer {
	return &cpApiServer{
		etcdCli: etcdCli,
	}
}

