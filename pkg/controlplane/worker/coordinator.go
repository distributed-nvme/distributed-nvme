package worker

import (
	"context"
	"fmt"
	"time"
	"sync"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/mbrhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
)

type workerI interface {
	getName() string
	getEtcdCli() *clientv3.Client
	getMemberPrefix() string
	getShardPrefix() string
	getInitTrigger() <-chan struct{}
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
	grantTTL int64
	shardWkrMap  map[string]*shardWorker
}

func (mwkr *memberWorker) asyncRun() {
	defer mwkr.wg.Done()
	mwkr.pch.Logger.Info("Run")

	etcdCli := mwkr.worker.getEtcdCli()
	memberPrefix := mwkr.worker.getMemberPrefix()

	revokeFun, err := mbrhelper.RegisterMember(
		etcdCli,
		mwkr.pch,
		memberPrefix,
		mwkr.grpcTarget,
		mwkr.prioCode,
		mwkr.grantTTL,
	)
	if err != nil {
		mwkr.pch.Logger.Fatal("RegisterMember err: %v", err)
	}
	defer revokeFun()

	var sms *mbrhelper.ShardMemberSummary
	var shardList []string
	sms, err = mbrhelper.NewShardMemberSummary(
		etcdCli,
		mwkr.pch,
		memberPrefix,
		mwkr.replica,
	)
	if err != nil {
		mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
	}
	shardList = sms.GetShardListByOwner(mwkr.grpcTarget)
	mwkr.pch.Logger.Info("shardList: %v", shardList)

	for {
		shardCh := etcdCli.Watch(
			mwkr.pch.Ctx,
			memberPrefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(sms.GetRevision()),
		)
		select {
		case <-shardCh:
			sms, err = mbrhelper.NewShardMemberSummary(
				etcdCli,
				mwkr.pch,
				memberPrefix,
				mwkr.replica,
			)
			if err != nil {
				mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
			}
		case <-mwkr.pch.Ctx.Done():
			return
		case <-mwkr.worker.getInitTrigger():
			break
		case <-time.After(constants.ShardInitWaitTime):
			break
		}
	}
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
	grantTTL int64,
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
		grantTTL: grantTTL,
		shardWkrMap:  make(map[string]*shardWorker),
	}
}
