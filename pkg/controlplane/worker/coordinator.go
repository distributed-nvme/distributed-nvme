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
	shardId string
	pch        *ctxhelper.PerCtxHelper
	wg         sync.WaitGroup
	worker workerI
}

func (swkr *shardWorker) asyncRun() {
	defer swkr.wg.Done()
	swkr.pch.Logger.Info("Run")
}

func (swkr *shardWorker) run() {
	swkr.wg.Add(1)
	go swkr.asyncRun()
}

func (swkr *shardWorker) cancel() {
	swkr.pch.Logger.Info("Cancel")
	swkr.pch.Cancel()
}

func (swkr *shardWorker) wait() {
	swkr.pch.Logger.Info("Waiting")
	swkr.wg.Wait()
	swkr.pch.Logger.Info("Exit")
}

func newShardWorker(
	parentCtx context.Context,
	shardId string,
	worker workerI,
) *shardWorker {
	logPrefix := fmt.Sprintf("%s-shard|%s", worker.getName(), shardId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, shardId)
	return &shardWorker{
		shardId: shardId,
		pch: pch,
		worker: worker,
	}
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

	var shardIdToWorker map[string]*shardWorker
	var sms *mbrhelper.ShardMemberSummary
	var shardMap map[string]bool

	sms, err = mbrhelper.NewShardMemberSummary(
		etcdCli,
		mwkr.pch,
		memberPrefix,
		mwkr.replica,
	)
	if err != nil {
		mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
	}
	shardMap = sms.GetShardMapByOwner(mwkr.grpcTarget)
	mwkr.pch.Logger.Info("shardMap: %v", shardMap)

	for {
		memberCh := etcdCli.Watch(
			mwkr.pch.Ctx,
			memberPrefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(sms.GetRevision()),
		)
		select {
		case <-memberCh:
			sms, err = mbrhelper.NewShardMemberSummary(
				etcdCli,
				mwkr.pch,
				memberPrefix,
				mwkr.replica,
			)
			if err != nil {
				mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
			}
			shardMap = sms.GetShardMapByOwner(mwkr.grpcTarget)
		case <-mwkr.pch.Ctx.Done():
			return
		case <-mwkr.worker.getInitTrigger():
			break
		case <-time.After(constants.ShardInitWaitTime):
			break
		}
	}

	for {
		toBeCreated := make([]*shardWorker, 0)
		toBeDeleted := make([]*shardWorker, 0)
		for shardId := range shardMap {
			_, ok := shardIdToWorker[shardId]
			if !ok {
				swkr := newShardWorker(
					mwkr.pch.Ctx,
					shardId,
					mwkr.worker,
				)
				toBeCreated = append(toBeCreated, swkr)
			}
		}
		for shardId, swkr := range shardIdToWorker {
			_, ok := shardMap[shardId]
			if !ok {
				toBeDeleted = append(toBeDeleted, swkr)
			}
		}
		for _, swkr := range toBeCreated {
			swkr.run()
			shardIdToWorker[swkr.shardId] = swkr
		}
		delay := (3600 * 24 * 365 * 100) * time.Second
		if len(toBeDeleted) > 0 {
			delay = constants.ShardDeleteWaitTime
		}

		memberCh := etcdCli.Watch(
			mwkr.pch.Ctx,
			memberPrefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(sms.GetRevision()),
		)

		select {
		case <- memberCh:
			sms, err = mbrhelper.NewShardMemberSummary(
				etcdCli,
				mwkr.pch,
				memberPrefix,
				mwkr.replica,
			)
			if err != nil {
				mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
			}
			shardMap = sms.GetShardMapByOwner(mwkr.grpcTarget)
		case <-time.After(delay):
			for _, swkr := range toBeDeleted {
				swkr.cancel()
			}
			for _, swkr := range toBeDeleted {
				swkr.wait()
				delete(shardIdToWorker, swkr.shardId)
			}
		case <-mwkr.pch.Ctx.Done():
			break
		}
	}

	for _, swkr := range shardIdToWorker {
		swkr.wait()
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
	grpcTarget string,
	prioCode string,
	replica uint32,
	grantTTL int64,
	worker workerI,
) *memberWorker {
	memberWkrId := uuid.New().String()
	logPrefix := fmt.Sprintf("%s-member|%s ", worker.getName(), memberWkrId)
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
	}
}
