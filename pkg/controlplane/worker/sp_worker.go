package worker

import (
	"sync"
	// "time"

	clientv3 "go.etcd.io/etcd/client/v3"
	// "go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	// "github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

var (
	spFastRetryCodeMap = make(map[uint32]bool)
)

type spWorkerServer struct {
	pbcp.UnimplementedStoragePoolWorkerServer
	mu             sync.Mutex
	etcdCli        *clientv3.Client
	kf             *keyfmt.KeyFmt
	sm             *stmwrapper.StmWrapper
	initTrigger    chan struct{}
	idAndRevToConf map[string]map[int64]*pbcp.StoragePoolConf
}

func (spwkr *spWorkerServer) getName() string {
	return "sp"
}

func (spwkr *spWorkerServer) getEtcdCli() *clientv3.Client {
	return spwkr.etcdCli
}

func (spwkr *spWorkerServer) getMemberPrefix() string {
	return spwkr.kf.SpMemberPrefix()
}

func (spwkr *spWorkerServer) getResPrefix() string {
	return spwkr.kf.SpConfEntityPrefix()
}

func (spwkr *spWorkerServer) getInitTrigger() <-chan struct{} {
	return spwkr.initTrigger
}

func (spwkr *spWorkerServer) addResRev(
	spId string,
	resBody []byte,
	rev int64,
) ([]string, error) {
	spConf := &pbcp.StoragePoolConf{}
	if err := proto.Unmarshal(resBody, spConf); err != nil {
		return nil, err
	}
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if ok {
		if len(revToConf) > 1 {
			panic("More tahn 1 sp rev: " + spId)
		}
	} else {
		revToConf = make(map[int64]*pbcp.StoragePoolConf)
		spwkr.idAndRevToConf[spId] = revToConf
	}
	revToConf[rev] = spConf
	grpcTargetList := make([]string, 0)
	for _, legConf := range spConf.LegConfList {
		for _, grpConf := range legConf.GrpConfList {
			for _, ldConf := range grpConf.LdConfList {
				grpcTargetList = append(grpcTargetList, ldConf.DnGrpcTarget)
			}
		}
	}
	for _, cntlrConf := range spConf.CntlrConfList {
		grpcTargetList = append(grpcTargetList, cntlrConf.CnGrpcTarget)
	}
	return grpcTargetList, nil
}

func (spwkr *spWorkerServer) delResRev(
	spId string,
	rev int64,
) error {
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if !ok {
		panic("Unknown sp id: " + spId)
	}
	delete(revToConf, rev)
	if len(revToConf) == 0 {
		delete(spwkr.idAndRevToConf, spId)
	}
	return nil
}

func (spwkr *spWorkerServer) updateSpInfo(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	reply *pbnd.SyncupDnReply,
	repErr error,
) {
	return
}

func (spwkr *spWorkerServer) syncupSp(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
) bool {
	return false
}

func (spwkr *spWorkerServer) checkSp(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
) bool {
	return false
}

func (spwkr *spWorkerServer) trackRes(
	spId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if !ok {
		pch.Logger.Fatal("Can not find spId: %s", spId)
	}
	if len(revToConf) != 1 {
		pch.Logger.Fatal("revToConf cnt error: %s %v", spId, revToConf)
	}
	var revision int64
	var spConf *pbcp.StoragePoolConf
	for key, value := range revToConf {
		revision = key
		spConf = value
		break
	}
	for {
		// FIXME: implement sp error handling
		if exit := spwkr.syncupSp(targetToConn, pch, spId, revision, spConf); exit {
			return
		}
		if exit := spwkr.checkSp(targetToConn, pch, spId, revision); exit {
			return
		}
	}
}

func newSpWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *spWorkerServer {
	return &spWorkerServer{
		etcdCli:        etcdCli,
		kf:             keyfmt.NewKeyFmt(prefix),
		sm:             stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:    make(chan struct{}),
		idAndRevToConf: make(map[string]map[int64]*pbcp.StoragePoolConf),
	}
}
