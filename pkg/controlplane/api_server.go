package controlplane

import (
	pbCpApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/cpapi"
)

type cpApiServer struct {
	pbCpApi.UnimplementedControlPlaneServer
}

func newCpApiServer() *cpApiServer {
	return &cpApiServer{}
}

