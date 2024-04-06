package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/mbrhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
)

type workerI interface {
	getName() string
	getEtcdCli() *clientv3.Client
	getMemberPrefix() string
	getShardPrefix() string
	trackRes(resId string, pch *ctxhelper.PerCtxHelper)
	addRes(resId string, resBody string) ([]string, error)
	delRes(resId string)
}

type resWorker struct {
	resWkrId string
	pch      *ctxhelper.PerCtxHelper
	wg       sync.WaitGroup
}

type shardWorker struct {
	shardWkrId string
	pch        *ctxhelper.PerCtxHelper
	wg         sync.WaitGroup
	resWkrMap  map[string]*resWorker
}

type memberWorker struct {
	memberWkrId  string
	pch          *ctxhelper.PerCtxHelper
	wg           sync.WaitGroup
	worker       workerI
	grpcTarget   string
	prioCode     string
	replica      uint32
	grantTimeout int64
	shardWkrMap  map[string]*shardWorker
}

func (mwkr *memberWorker) asyncRun() {
	defer mwkr.wg.Done()
	mwkr.pch.Logger.Info("Run")
	revokeFun, err := mbrhelper.RegisterMember(
		mwkr.worker.getEtcdCli(),
		mwkr.pch,
		mwkr.worker.getMemberPrefix(),
		mwkr.grpcTarget,
		mwkr.prioCode,
		mwkr.grantTimeout,
	)
	if err != nil {
		mwkr.pch.Logger.Fatal("RegisterMember err: %v", err)
	}
	defer revokeFun()

	sms, err := mbrhelper.NewShardMemberSummary(
		mwkr.worker.getEtcdCli(),
		mwkr.pch,
		mwkr.worker.getMemberPrefix(),
		mwkr.replica,
	)
	if err != nil {
		mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
	}
	mwkr.pch.Logger.Info("sms: %v", sms)
}

func (mwkr *memberWorker) run() {
	mwkr.wg.Add(1)
	go mwkr.asyncRun()
}

func (mwkr *memberWorker) cancel() {
	mwkr.pch.Logger.Info("Cancel")
	mwkr.pch.Cancel()
}

func (mwkr *memberWorker) wait() {
	mwkr.pch.Logger.Info("Waiting")
	mwkr.wg.Wait()
	mwkr.pch.Logger.Info("Exit")
}

func newMemberWorker(
	parentCtx context.Context,
	name string,
	grpcTarget string,
	prioCode string,
	replica uint32,
	grantTimeout int64,
	worker workerI,
) *memberWorker {
	memberWkrId := uuid.New().String()
	logPrefix := fmt.Sprintf("%s-member|%s ", name, memberWkrId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, memberWkrId)
	return &memberWorker{
		memberWkrId:  memberWkrId,
		pch:          pch,
		worker:       worker,
		grpcTarget:   grpcTarget,
		prioCode:     prioCode,
		replica:      replica,
		grantTimeout: grantTimeout,
		shardWkrMap:  make(map[string]*shardWorker),
	}
}
