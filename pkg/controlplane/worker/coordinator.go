package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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
	conn, err := grpc.Dial(
		grpcTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
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
	addResRev(resId string, resBody []byte, rev int64) ([]string, error)
	delResRev(resId string, rev int64) error
	trackRes(resId string, pch *ctxhelper.PerCtxHelper, targetToConn map[string]*grpc.ClientConn)
}

type resWorker struct {
	resWkrId     string
	pch          *ctxhelper.PerCtxHelper
	wg           sync.WaitGroup
	worker       workerI
	resId        string
	revision     int64
	targetToConn map[string]*grpc.ClientConn
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
}

func newResWorker(
	parentCtx context.Context,
	worker workerI,
	resId string,
	revision int64,
	targetToConn map[string]*grpc.ClientConn,
) *resWorker {
	resWkrId := uuid.New().String()
	logPrefix := fmt.Sprintf("%s-res|%s ", worker.getName(), resWkrId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, resWkrId)

	return &resWorker{
		resWkrId:     resWkrId,
		pch:          pch,
		worker:       worker,
		resId:        resId,
		revision:     revision,
		targetToConn: targetToConn,
	}
}

type bodyAndRev struct {
	resBody  []byte
	revision int64
}

type shardTask struct {
	toBeCreated map[string]*bodyAndRev
	toBeDeleted map[string]bool
	mu          sync.Mutex
}

func (st *shardTask) deleteAndCreate(resId string, resBody []byte, revision int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toBeDeleted[resId] = true
	bAndR := &bodyAndRev{
		resBody:  resBody,
		revision: revision,
	}
	st.toBeCreated[resId] = bAndR
}

func (st *shardTask) deleteOnly(resId string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toBeDeleted[resId] = true
	delete(st.toBeCreated, resId)
}

func (st *shardTask) fetchTasks() (map[string]*bodyAndRev, map[string]bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	toBeCreated := st.toBeCreated
	toBeDeleted := st.toBeDeleted
	st.toBeCreated = make(map[string]*bodyAndRev)
	st.toBeDeleted = make(map[string]bool)
	return toBeCreated, toBeDeleted
}

func newShardTask() *shardTask {
	return &shardTask{
		toBeCreated: make(map[string]*bodyAndRev),
		toBeDeleted: make(map[string]bool),
	}
}

type shardWorker struct {
	shardId string
	wkrId   string
	pch     *ctxhelper.PerCtxHelper
	wg      sync.WaitGroup
	worker  workerI
	gcCache *grpcConnCache
	st      *shardTask
}

func (swkr *shardWorker) watch() {
	defer swkr.wg.Done()
	prefix := fmt.Sprintf("%s|shard-w|%s|%s", swkr.worker.getName(), swkr.shardId, swkr.wkrId)
	logger := prefixlog.NewPrefixLogger(prefix)
	pch := ctxhelper.NewPerCtxHelper(swkr.pch.Ctx, logger, swkr.wkrId)

	pch.Logger.Info("Run")

	var revision int64

	etcdCli := swkr.worker.getEtcdCli()
	resPrefix := swkr.worker.getResPrefix()
	shardPrefix := fmt.Sprintf("%s/%s", resPrefix, swkr.shardId)

	resp, err := etcdCli.Get(
		pch.Ctx,
		shardPrefix,
		clientv3.WithPrefix(),
		clientv3.WithKeysOnly(),
	)
	if err != nil {
		pch.Logger.Fatal("Get res id list failed: %s %v", shardPrefix, err)
	}
	revision = resp.Header.Revision

	for _, ev := range resp.Kvs {
		resp1, err := etcdCli.Get(
			pch.Ctx,
			string(ev.Key),
			clientv3.WithRev(revision),
		)
		if err != nil {
			pch.Logger.Error("Get res value failed: %s %v", ev.Key, err)
			continue
		}
		if len(resp1.Kvs) != 1 {
			pch.Logger.Error("Wrong res count: %v", resp1.Kvs)
			continue
		}
		key := string(resp1.Kvs[0].Key)
		resId := key[len(resPrefix):]
		resBody := resp1.Kvs[0].Value
		swkr.st.deleteAndCreate(resId, resBody, revision)
	}

	for {
		shardCh := etcdCli.Watch(
			pch.Ctx,
			shardPrefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(revision+1),
		)

		select {
		case wresp := <-shardCh:
			revision = wresp.Header.Revision
			for _, ev := range wresp.Events {
				key := string(ev.Kv.Key)
				resId := key[len(resPrefix):]
				resBody := ev.Kv.Value
				switch ev.Type {
				case clientv3.EventTypePut:
					swkr.st.deleteAndCreate(resId, resBody, revision)
				case clientv3.EventTypeDelete:
					swkr.st.deleteOnly(resId)
				default:
					pch.Logger.Fatal("Unknow event type: %v", ev.Type)
				}
			}
		case <-pch.Ctx.Done():
			return
		}
	}
}

func (swkr *shardWorker) process(
	pch *ctxhelper.PerCtxHelper,
	resIdToWorker map[string]*resWorker,
	toBeCreated map[string]*bodyAndRev,
	toBeDeleted map[string]bool,
) {
	creatingList := make([]*resWorker, 0)
	for resId, bAndR := range toBeCreated {
		resBody := bAndR.resBody
		revision := bAndR.revision
		grpcTargetList, err := swkr.worker.addResRev(resId, resBody, revision)
		if err != nil {
			pch.Logger.Warning(
				"addResRev err, resId: %s revision: %d err: %v",
				resId,
				revision,
				err,
			)
			continue
		}
		targetToConn := make(map[string]*grpc.ClientConn)
		ignore := false
		for _, grpcTarget := range grpcTargetList {
			conn, err := swkr.gcCache.get(grpcTarget)
			if err != nil {
				pch.Logger.Warning(
					"get grpcTarget err, grpcTarget: %s err: %v",
					grpcTarget,
					err,
				)
				ignore = true
				break
			}
			targetToConn[grpcTarget] = conn
		}
		if ignore {
			continue
		}
		rwkr := newResWorker(
			pch.Ctx,
			swkr.worker,
			resId,
			revision,
			targetToConn,
		)
		creatingList = append(creatingList, rwkr)
	}

	deletingList := make([]*resWorker, 0)
	for resId := range toBeDeleted {
		rwkr, ok := resIdToWorker[resId]
		if !ok {
			continue
		}
		rwkr.cancel()
		deletingList = append(deletingList, rwkr)
	}

	for _, rwkr := range deletingList {
		rwkr.wait()
		for grpcTarget := range rwkr.targetToConn {
			err := swkr.gcCache.put(grpcTarget)
			if err != nil {
				pch.Logger.Fatal(
					"grpc conn cache put err, %s %v",
					grpcTarget,
					err,
				)
			}
		}
		err := swkr.worker.delResRev(rwkr.resId, rwkr.revision)
		if err != nil {
			pch.Logger.Fatal(
				"delResRev err, %s %d %v",
				rwkr.resId,
				rwkr.revision,
				err,
			)
		}
	}

	for _, rwkr := range creatingList {
		rwkr.run()
	}
}

func (swkr *shardWorker) handle() {
	defer swkr.wg.Done()

	prefix := fmt.Sprintf(
		"%s|shard-h|%s|%s",
		swkr.worker.getName(),
		swkr.shardId,
		swkr.wkrId,
	)
	logger := prefixlog.NewPrefixLogger(prefix)
	pch := ctxhelper.NewPerCtxHelper(swkr.pch.Ctx, logger, swkr.wkrId)

	pch.Logger.Info("Handler")

	resIdToWorker := make(map[string]*resWorker)
	select {
	case <-time.After(constants.ShardWorkerDelayDefault):
		toBeCreated, toBeDeleted := swkr.st.fetchTasks()
		swkr.process(pch, resIdToWorker, toBeCreated, toBeDeleted)
	case <-pch.Ctx.Done():
		break
	}

	for _, rwkr := range resIdToWorker {
		rwkr.cancel()
	}
	for _, rwkr := range resIdToWorker {
		rwkr.wait()
	}
}

func (swkr *shardWorker) run() {
	swkr.wg.Add(2)
	go swkr.watch()
	go swkr.handle()
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
	shardWkrId := uuid.New().String()
	prefix := fmt.Sprintf("%s|shard|%s|%s", worker.getName(), shardId, shardWkrId)
	logger := prefixlog.NewPrefixLogger(prefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, shardWkrId)
	return &shardWorker{
		shardId: shardId,
		wkrId:   shardWkrId,
		pch:     pch,
		worker:  worker,
		gcCache: gcCache,
		st:      newShardTask(),
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

	var sms *mbrhelper.ShardMemberSummary
	var shardMap map[string]bool

	shardIdToWorker := make(map[string]*shardWorker)

	sms, err = mbrhelper.NewShardMemberSummary(
		etcdCli,
		mwkr.pch,
		memberPrefix,
		mwkr.replica,
	)
	if err != nil {
		mwkr.pch.Logger.Fatal("NewShardMemberSummary err: %v", err)
	}
	mwkr.pch.Logger.Info("First sms: %v", sms)
	shardMap = sms.GetShardMapByOwner(mwkr.grpcTarget)
	earlyBreak := false

	for {
		memberCh := etcdCli.Watch(
			mwkr.pch.Ctx,
			memberPrefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(sms.GetRevision()+1),
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
			mwkr.pch.Logger.Info("Early sms: %v", sms)
			shardMap = sms.GetShardMapByOwner(mwkr.grpcTarget)
		case <-mwkr.pch.Ctx.Done():
			mwkr.pch.Logger.Info("Early exit")
			return
		case <-mwkr.worker.getInitTrigger():
			mwkr.pch.Logger.Info("getInitTrigger")
			earlyBreak = true
			break
		case <-time.After(constants.ShardInitWaitTime):
			mwkr.pch.Logger.Info("After %d", constants.ShardInitWaitTime)
			earlyBreak = true
			break
		}

		if earlyBreak {
			break
		}
	}

	mwkr.pch.Logger.Info("Main loop")
	normalBreak := false
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
			clientv3.WithRev(sms.GetRevision()+1),
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
			mwkr.pch.Logger.Info("Normal sms: %v", sms)
			shardMap = sms.GetShardMapByOwner(mwkr.grpcTarget)
		case <-time.After(delay):
			for _, swkr := range toBeDeleted {
				swkr.cancel()
			}
			for _, swkr := range toBeDeleted {
				swkr.wait()
				delete(shardIdToWorker, swkr.shardId)
			}
			mwkr.pch.Logger.Info("Normal after %d", delay)
		case <-mwkr.pch.Ctx.Done():
			mwkr.pch.Logger.Info("Normal break")
			normalBreak = true
			break
		}

		if normalBreak {
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
	logPrefix := fmt.Sprintf("%s|member|%s|%s", worker.getName(), grpcTarget, memberWkrId)
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
