package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/mbrhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
)

type cacheItem struct {
	conn   *grpc.ClientConn
	refCnt int
}

type grpcConnCache struct {
	cache map[string]*cacheItem
	mu    sync.Mutex
}

func (gcCache *grpcConnCache) get(
	grpcTarget string,
) (*grpc.ClientConn, error) {
	gcCache.mu.Lock()
	defer gcCache.mu.Unlock()
	item, ok := gcCache.cache[grpcTarget]
	if ok {
		item.refCnt++
		return item.conn, nil
	}
	conn, err := grpc.Dial(grpcTarget)
	if err != nil {
		return nil, err
	}
	item = &cacheItem{
		conn:   conn,
		refCnt: 1,
	}
	gcCache.cache[grpcTarget] = item
	return conn, nil
}

func (gcCache *grpcConnCache) put(grpcTarget string) error {
	gcCache.mu.Lock()
	defer gcCache.mu.Unlock()
	item, ok := gcCache.cache[grpcTarget]
	if !ok {
		return fmt.Errorf("Can not find grpcTarget: %s", grpcTarget)
	}
	item.refCnt--
	if item.refCnt < 0 {
		return fmt.Errorf("Negative refCnt: %s %d", grpcTarget, item.refCnt)
	}
	if item.refCnt == 0 {
		item.conn.Close()
		delete(gcCache.cache, grpcTarget)
	}
	return nil
}

func (gcCache *grpcConnCache) cleanup() {
	gcCache.mu.Lock()
	defer gcCache.mu.Unlock()
	for grpcTarget, item := range gcCache.cache {
		item.conn.Close()
		delete(gcCache.cache, grpcTarget)
	}
}

func newGrpcConnCache() *grpcConnCache {
	return &grpcConnCache{
		cache: make(map[string]*cacheItem),
	}
}

type workerI interface {
	getName() string
	getEtcdCli() *clientv3.Client
	getMemberPrefix() string
	getResPrefix() string
	getInitTrigger() <-chan struct{}
	addResRev(resId, resBody string, rev int64) ([]string, error)
	delResRev(resId string, rev int64)
	trackRes(resId string, pch *ctxhelper.PerCtxHelper, targetToConn map[string]*grpc.ClientConn)
	addRes(resId string)
	delRes(resId string)
}

type resWorker struct {
	resWkrId     string
	pch          *ctxhelper.PerCtxHelper
	wg           sync.WaitGroup
	worker       workerI
	resId        string
	targetToConn map[string]*grpc.ClientConn
	gcCache      *grpcConnCache
}

func (rwkr *resWorker) asyncRun() {
	defer rwkr.wg.Done()
	rwkr.worker.trackRes(rwkr.resId, rwkr.pch, rwkr.targetToConn)
}

func (rwkr *resWorker) run() {
	rwkr.wg.Add(1)
	go rwkr.asyncRun()
}

func (rwkr *resWorker) cancel() {
	rwkr.pch.Logger.Info("Cancel")
	rwkr.pch.Cancel()
}

func (rwkr *resWorker) wait() {
	rwkr.pch.Logger.Info("Waiting")
	rwkr.wg.Wait()
	rwkr.pch.Logger.Info("Wait done")
	rwkr.worker.delRes(rwkr.resId)
	for grpcTarget := range rwkr.targetToConn {
		rwkr.gcCache.put(grpcTarget)
	}
}

func newResWorker(
	parentCtx context.Context,
	worker workerI,
	gcCache *grpcConnCache,
	resId string,
	targetToConn map[string]*grpc.ClientConn,
) *resWorker {
	resWkrId := uuid.New().String()
	logPrefix := fmt.Sprintf("%s-res|%s ", worker.getName(), resWkrId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, resWkrId)

	worker.addRes(resId)

	return &resWorker{
		resWkrId:     resWkrId,
		pch:          pch,
		worker:       worker,
		resId:        resId,
		targetToConn: targetToConn,
		gcCache:      gcCache,
	}
}

type shardTask struct {
	toBeCreated map[string]string
	toBeDeleted map[string]bool
	mu          sync.Mutex
}

func (st *shardTask) addToCreate(resId, resBody string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toBeCreated[resId] = resBody
}

func (st *shardTask) addToDelete(resId string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toBeDeleted[resId] = true
}

func (st *shardTask) addToCreateAndDelete(resId, resBody string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toBeCreated[resId] = resBody
	st.toBeDeleted[resId] = true
}

func (st *shardTask) fetchTasks() (map[string]string, map[string]bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	toBeCreated := st.toBeCreated
	toBeDeleted := st.toBeDeleted
	st.toBeCreated = make(map[string]string)
	st.toBeDeleted = make(map[string]bool)
	return toBeCreated, toBeDeleted
}

func newShardTask() *shardTask {
	return &shardTask{
		toBeCreated: make(map[string]string),
		toBeDeleted: make(map[string]bool),
	}
}

type shardWorker struct {
	shardId string
	pch     *ctxhelper.PerCtxHelper
	wg      sync.WaitGroup
	worker  workerI
	gcCache *grpcConnCache
}

func (swkr *shardWorker) asyncRun() {
	defer swkr.wg.Done()
	swkr.pch.Logger.Info("Run")

	var revision int64

	etcdCli := swkr.worker.getEtcdCli()
	resPrefix := swkr.worker.getResPrefix()
	shardPrefix := fmt.Sprintf("%s/%s", resPrefix, swkr.shardId)

	resp, err := etcdCli.Get(
		swkr.pch.Ctx,
		shardPrefix,
		clientv3.WithPrefix(),
		clientv3.WithKeysOnly(),
	)
	if err != nil {
		swkr.pch.Logger.Fatal("Get res id list failed: %s %v", shardPrefix, err)
	}
	revision = resp.Header.Revision

	resIdToWorker := make(map[string]*resWorker)
	for _, ev := range resp.Kvs {
		resp1, err := etcdCli.Get(
			swkr.pch.Ctx,
			string(ev.Key),
			clientv3.WithRev(revision),
		)
		if err != nil {
			swkr.pch.Logger.Error("Get res value failed: %s %v", ev.Key, err)
			continue
		}
		if len(resp1.Kvs) != 1 {
			swkr.pch.Logger.Error("Wrong res count: %v", resp1.Kvs)
			continue
		}
		swkr.pch.Logger.Info("%v", resIdToWorker)
		// key := string(resp1.Kvs[0].Key)
		// resId := key[len(resPrefix):]
		// resBody := string(resp1.Kvs[0].Value)
		// rwkr, err := newResWorker(
		// 	swkr.pch,
		// 	swkr.worker,
		// 	swkr.gcCache,
		// 	resId,
		// 	resBody,
		// )
		// if err != nil {
		// 	swkr.pch.Logger.Error("Create resWorker err: %s %v", resId, err)
		// 	continue
		// }
		// resIdToWorker[resId] = rwkr
		// rwkr.run()
	}

	for {
		shardCh := etcdCli.Watch(
			swkr.pch.Ctx,
			shardPrefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(revision),
		)

		select {
		case wresp := <-shardCh:
			revision = wresp.Header.Revision
			for _, ev := range wresp.Events {
				swkr.pch.Logger.Info("%v", ev)
			}
		}
	}
}

func (swkr *shardWorker) background() {
	defer swkr.wg.Done()
}

func (swkr *shardWorker) run() {
	swkr.wg.Add(2)
	go swkr.asyncRun()
	go swkr.background()
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
	gcCache *grpcConnCache,
) *shardWorker {
	logPrefix := fmt.Sprintf("%s-shard|%s", worker.getName(), shardId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, shardId)
	return &shardWorker{
		shardId: shardId,
		pch:     pch,
		worker:  worker,
		gcCache: gcCache,
	}
}

type memberWorker struct {
	memberWkrId string
	pch         *ctxhelper.PerCtxHelper
	wg          sync.WaitGroup
	worker      workerI
	grpcTarget  string
	prioCode    string
	replica     uint32
	grantTTL    int64
	gcCache     *grpcConnCache
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
					mwkr.gcCache,
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
	mwkr.gcCache.cleanup()
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
		memberWkrId: memberWkrId,
		pch:         pch,
		worker:      worker,
		grpcTarget:  grpcTarget,
		prioCode:    prioCode,
		replica:     replica,
		grantTTL:    grantTTL,
		gcCache:     newGrpcConnCache(),
	}
}
