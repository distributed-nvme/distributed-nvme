package nodeagent

import (
	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbNdApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

type dnAgentServer struct {
	pbNdApi.UnimplementedDnAgentServer
	logger *lib.Logger
	oc *lib.OsCmd
}

func newDnAgentServer(logger *lib.Logger) *dnAgentServer {
	return &dnAgentServer{
		logger: logger,
		oc: lib.NewOsCmd(logger),
	}
}

type cnAgentServer struct {
	pbNdApi.UnimplementedCnAgentServer
	logger *lib.Logger
	oc *lib.OsCmd
}

func newCnAgentServer(logger *lib.Logger) *cnAgentServer {
	return &cnAgentServer{
		logger: logger,
		oc: lib.NewOsCmd(logger),
	}
}
