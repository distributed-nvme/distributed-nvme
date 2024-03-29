package controlplane

import (
	"time"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnWorkerServer struct {
	pbcp.UnimplementedDiskNodeWorkerServer

	// Shared fields, concurrency safe
	etcdCli *clientv3.Client
	kf *keyFmt
	sm *stmWrapper

	// Shared fields, protected by Mutex
	mu sync.Mutex
	inited bool
	initRequired bool

	// dnWorkerMember only fields
	leadingCode string
	grpcTarget string
	bucket []string
	grantTimeout int64

	// dnIndividualWorker only fields
	agentTimeout time.Duration
}

func newDnWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
	leadingCode string,
	grpcTarget string,
) *dnWorkerServer {
	return &dnWorkerServer{
		etcdCli: etcdCli,
		kf: newKeyFmt(prefix),
		sm: newStmWrapper(etcdCli),
		inited: false,
		initRequired: false,
		leadingCode: leadingCode,
		grpcTarget: grpcTarget,
		bucket: make([]string, 0),
		grantTimeout: lib.GrantTimeoutDefault,
		agentTimeout: time.Duration(lib.AgentTimeoutSecondDefault) * time.Second,
		
	}
}
