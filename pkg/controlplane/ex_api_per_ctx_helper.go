package controlplane

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbds "github.com/distributed-nvme/distributed-nvme/pkg/proto/dataschema"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

type dnAgentClient struct {
	conn *grpc.ClientConn
	c pbnd.DnAgentClient
}

type perCtxHelper struct {
	ctx context.Context
	cpas *cpApiServer
	logger *lib.Logger
	dacMap map[string]*dnAgentClient
}

func (pch *perCtxHelper) getDnAgentClient(sockAddr string) (pbnd.DnAgentClient, error) {
	dac, ok := pch.dacMap[sockAddr]
	if ok {
		return dac.c, nil
	}
	opts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
	}
	conn, err := grpc.Dial(
		sockAddr,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(pch.cpas.agentTimeout),
		grpc.WithChainUnaryInterceptor(
			logging.UnaryClientInterceptor(lib.InterceptorLogger(pch.logger), opts...),
		),
		grpc.WithChainStreamInterceptor(
			logging.StreamClientInterceptor(lib.InterceptorLogger(pch.logger), opts...),
		),
	)
	if err != nil {
		pch.logger.Warning("Get conn err: %s %v", sockAddr, err)
		return nil, err
	}
	c := pbnd.NewDnAgentClient(conn)
	dac = &dnAgentClient{
		conn: conn,
		c: c,
	}
	pch.dacMap[sockAddr] = dac
	return dac.c, nil
}

func (pch *perCtxHelper) runStm(
	apply func(stm concurrency.STM) error,
	name string,
) error {
	cnt := 0
	applyWrapper := func(stm concurrency.STM) error {
		cnt++
		pch.logger.Info("stm apply: %s %d", name, cnt)
		err := apply(stm)
		if err != nil {
			pch.logger.Warning("stm apply err: %s %v", name, err)
		}
		return err
	}
	_, err := concurrency.NewSTM(
		pch.cpas.etcdCli,
		applyWrapper,
		concurrency.WithAbortContext(pch.ctx),
	)
	if err != nil {
		pch.logger.Warning("stm create err: %s %v", name, err)
	}
	return err
}

func (pch *perCtxHelper) getCluster() (*pbds.Cluster,error) {
	if !pch.cpas.cluster_init {
		clusterEntityKey := pch.cpas.kf.ClusterEntityKey()
		resp, err := pch.cpas.etcdCli.Get(pch.ctx, clusterEntityKey)
		if err != nil {
			return nil, err
		}
		if len(resp.Kvs) != 1 {
			return nil, fmt.Errorf("Invalid cluster entity cnt: %d", len(resp.Kvs))
		}
		err = proto.Unmarshal(resp.Kvs[0].Value, &pch.cpas.cluster)
		if err != nil {
			return nil, err
		}
		pch.cpas.cluster_init = true
	}
	return &pch.cpas.cluster, nil
}

func (pch *perCtxHelper) close() {
	for _, dac := range pch.dacMap {
		dac.conn.Close()
	}
}

func newPerCtxHelper(ctx context.Context, cpas *cpApiServer) *perCtxHelper {
	return &perCtxHelper{
		ctx: ctx,
		cpas: cpas,
		logger: lib.NewLogger(fmt.Sprintf("apiserver-%s", lib.GetReqId(ctx))),
		dacMap: make(map[string]*dnAgentClient),
	}
}
