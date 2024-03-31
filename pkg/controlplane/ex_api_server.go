package controlplane

import (
	"time"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type exApiServer struct {
	pbcp.UnimplementedExternalApiServer
	etcdCli *clientv3.Client
	kf *keyFmt
	sm *stmWrapper
	agentTimeout time.Duration
	clusterInit bool
	cluster pbcp.Cluster
}

func (exApi *exApiServer)getCluster(
	pch *lib.PerCtxHelper,
) (*pbcp.Cluster, error) {
	if !exApi.clusterInit {
		clusterEntityKey := exApi.kf.clusterEntityKey()
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
		kf: newKeyFmt(prefix),
		sm: newStmWrapper(etcdCli),
		agentTimeout: time.Duration(lib.AgentTimeoutSecondDefault) * time.Second,
		clusterInit: false,
	}
}
