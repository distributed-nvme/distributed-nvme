package controlplane

import (
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbds "github.com/distributed-nvme/distributed-nvme/pkg/proto/dataschema"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplaneapi"
)

type cpApiServer struct {
	pbcp.UnimplementedControlPlaneServer
	etcdCli *clientv3.Client
	logger *lib.Logger
	kf *lib.KeyFmt
	agentTimeout time.Duration
	cluster_init bool
	cluster pbds.Cluster
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
		cluster_init: false,
	}
}
