package controlplane

import (
	"context"

	"google.golang.org/grpc"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

type dnAgentClient struct {
	conn *grpc.ClientConn
	c pbnd.DnAgentClient
}

type perCtxHelper struct {
	ctx context.Context
	cpas *cpApiServer
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
			logging.UnaryClientInterceptor(lib.InterceptorLogger(pch.cpas.logger), opts...),
		),
		grpc.WithChainStreamInterceptor(
			logging.StreamClientInterceptor(lib.InterceptorLogger(pch.cpas.logger), opts...),
		),
	)
	if err != nil {
		pch.cpas.logger.Warning("Get conn err: %s %v", sockAddr, err)
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

func (pch *perCtxHelper) close() {
	for _, dac := range pch.dacMap {
		dac.conn.Close()
	}
}

func newPerCtxHelper(ctx context.Context, cpas *cpApiServer) *perCtxHelper {
	return &perCtxHelper{
		ctx: ctx,
		cpas: cpas,
		dacMap: make(map[string]*dnAgentClient),
	}
}
