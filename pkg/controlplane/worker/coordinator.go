package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
)

type workerI interface {
	getName() string
	getMemberPrefix() string
	getShardPrefix() string
	trackRes(resId string, pch *ctxhelper.PerCtxHelper)
	addRes(resId string, resBody string) ([]string, error)
	delRes(resId string)
}

type coordContext struct {
	etcdCli *clientv3.Client
	worker workerI
}

func newCoordCtx(
	etcdCli *clientv3.Client,
	worker workerI,
) *coordContext {
	return &coordContext{
		etcdCli: etcdCli,
		worker: worker,
	}
}

type resWorker struct {
	resWkrId string
	pch *ctxhelper.PerCtxHelper
	wg sync.WaitGroup
}

type shardWorker1 struct {
	shardWkrId string
	pch *ctxhelper.PerCtxHelper
	wg sync.WaitGroup
	resWkrMap map[string]*resWorker
}

type memberWorker struct {
	memberWkrId string
	pch *ctxhelper.PerCtxHelper
	wg sync.WaitGroup
	coordCtx *coordContext
	grpcTarget string
	prioCode string
	grantTimeout int64
	shardWkrMap map[string]*shardWorker
}

func (memberWkr *memberWorker) run() {
	defer memberWkr.wg.Done()
	memberWkr.pch.Logger.Info("Run")
}

func newMemberWorker(
	parentCtx context.Context,
	name string,
	grpcTarget string,
	prioCode string,
	grantTimeout int64,
	coordCtx *coordContext,
) *memberWorker {
	memberWkrId := uuid.New().String()
	logPrefix := fmt.Sprintf("%s-member|%s ", name, memberWkrId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, memberWkrId)
	return &memberWorker{
		memberWkrId: memberWkrId,
		pch: pch,
		coordCtx: coordCtx,
		grpcTarget: grpcTarget,
		prioCode: prioCode,
		grantTimeout: grantTimeout,
		shardWkrMap: make(map[string]*shardWorker),
	}
}

type coordinator struct {
	memberWkr *memberWorker
}

func (co *coordinator) run() {
	co.memberWkr.wg.Add(1)
	go co.memberWkr.run()
}

func (co *coordinator) cancel() {
	co.memberWkr.pch.Logger.Info("Cancel")
	co.memberWkr.pch.Cancel()
}

func (co *coordinator) wait() {
	co.memberWkr.pch.Logger.Info("Waiting")
	co.memberWkr.wg.Wait()
	co.memberWkr.pch.Logger.Info("Exit")
}

func newCoordinator(
	ctx context.Context,
	etcdCli *clientv3.Client,
	grpcTarget string,
	prioCode string,
	grantTimeout int64,
	worker workerI,
) *coordinator {
	coordCtx := newCoordCtx(
		etcdCli,
		worker,
	)
	memberWkr := newMemberWorker(
		ctx,
		worker.getName(),
		grpcTarget,
		prioCode,
		grantTimeout,
		coordCtx,
	)
	co := &coordinator{
		memberWkr: memberWkr,
	}
	return co
}
