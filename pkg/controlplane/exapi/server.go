package exapi

import (
	"time"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type exApiServer struct {
	pbcp.UnimplementedExternalApiServer
	etcdCli *clientv3.Client
	kf *keyfmt.KeyFmt
	sm *stmwrapper.StmWrapper
	agentTimeout time.Duration
	clusterInit bool
	cluster pbcp.Cluster
}

func (exApi *exApiServer)getCluster(
	pch *ctxhelper.PerCtxHelper,
) (*pbcp.Cluster, error) {
	if !exApi.clusterInit {
		clusterEntityKey := exApi.kf.ClusterEntityKey()
		resp, err := exApi.etcdCli.Get(pch.Ctx, clusterEntityKey)
		if err != nil {
			return nil, err
		}
		if len(resp.Kvs) != 1 {
			return nil, fmt.Errorf("invalid cluster entity cnt: %d", len(resp.Kvs))
		}
		err = proto.Unmarshal(resp.Kvs[0].Value, &exApi.cluster)
		if err != nil {
			return nil, err
		}
		exApi.clusterInit = true
	}
	return &exApi.cluster, nil
}

func newExApiServer(
	etcdCli *clientv3.Client,
	prefix string,
) *exApiServer {
	return &exApiServer{
		etcdCli: etcdCli,
		kf: keyfmt.NewKeyFmt(prefix),
		sm: stmwrapper.NewStmWrapper(etcdCli),
		agentTimeout: time.Duration(constants.AgentTimeoutSecondDefault) * time.Second,
		clusterInit: false,
	}
}
