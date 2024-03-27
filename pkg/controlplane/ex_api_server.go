package controlplane

import (
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbsch "github.com/distributed-nvme/distributed-nvme/pkg/proto/schema"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type exApiServer struct {
	pbcp.UnimplementedControlPlaneServer
	etcdCli *clientv3.Client
	kf *lib.KeyFmt
	cluster_init bool
	cluster pbsch.Cluster
}

func newExApiServer(
	etcdCli *clientv3.Client,
	prefix string,
	nodeTimeout int,
) *exApiServer {
	return &exApiServer{
		etcdCli: etcdCli,
		kf: newKeyFmt(prefix),
		cluster_init: false,
	}
}
