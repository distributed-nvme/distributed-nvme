package worker

import (
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnWorkerServer struct {
	pbcp.UnimplementedDiskNodeWorkerServer

	// Shared fields, concurrency safe
	etcdCli *clientv3.Client
	kf      *keyfmt.KeyFmt
	sm      *stmwrapper.StmWrapper

	// Shared fields
	initTrigger chan struct{}
	mu          sync.Mutex

	// dnWorkerMember only fields
	prioCode     string
	grpcTarget   string
	bucket       []string
	grantTimeout int64

	// dnIndividualWorker only fields
	agentTimeout time.Duration
}

func newDnWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
	prioCode string,
	grpcTarget string,
) *dnWorkerServer {
	return &dnWorkerServer{
		etcdCli:      etcdCli,
		kf:           keyfmt.NewKeyFmt(prefix),
		sm:           stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:  make(chan struct{}),
		prioCode:     prioCode,
		grpcTarget:   grpcTarget,
		bucket:       make([]string, 0),
		grantTimeout: constants.GrantTimeoutDefault,
		agentTimeout: time.Duration(constants.AgentTimeoutSecondDefault) * time.Second,
	}
}
