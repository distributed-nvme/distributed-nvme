package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
)

type dnShardWorker struct {
	pch      *ctxhelper.PerCtxHelper
	wg       sync.WaitGroup
	dnWorker *dnWorkerServer
	shardId  string
}

func (dnsw *dnShardWorker) asyncRun() {
	dnsw.pch.Logger.Info("asyncRun: %s", dnsw.shardId)
	for {
		select {
		case <-dnsw.pch.Ctx.Done():
			dnsw.pch.Logger.Info("Exit for ctx done")
			return
		}
	}
}

func (dnsw *dnShardWorker) run() {
	defer dnsw.wg.Done()
	go dnsw.asyncRun()
}

func (dnsw *dnShardWorker) cancel() {
	dnsw.pch.Logger.Info("Cancel")
	dnsw.pch.Cancel()
}

func (dnsw *dnShardWorker) wait() {
	dnsw.pch.Logger.Info("Waiting")
	dnsw.wg.Wait()
	dnsw.pch.Logger.Info("Exit")
}

func newDnShardWorker(
	parentCtx context.Context,
	dnWorker *dnWorkerServer,
	shardId string,
) *dnShardWorker {
	workerId := uuid.New().String()
	logger := prefixlog.NewPrefixLogger(fmt.Sprintf("dnShardWorker|%s ", workerId))
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, workerId)
	return &dnShardWorker{
		pch:      pch,
		dnWorker: dnWorker,
		shardId:  shardId,
	}
}

type dnMemberWorker struct {
	pch      *ctxhelper.PerCtxHelper
	wg       sync.WaitGroup
	dnWorker *dnWorkerServer
}

func (dnmw *dnMemberWorker) asyncRun() {
	defer dnmw.wg.Done()
	dnmw.pch.Logger.Info("asyncRun")
	key := dnmw.dnWorker.kf.DnShardKey(dnmw.dnWorker.grpcTarget)
	resp, err := dnmw.dnWorker.etcdCli.Grant(dnmw.pch.Ctx, dnmw.dnWorker.grantTimeout)
	if err != nil {
		dnmw.pch.Logger.Fatal("Grant err: %v", err)
	}

	if _, err := dnmw.dnWorker.etcdCli.KeepAlive(dnmw.pch.Ctx, resp.ID); err != nil {
		dnmw.dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
		dnmw.pch.Logger.Fatal("KeepAlive err: %v leaseId=%v", err, resp.ID)
	}
	_, err = dnmw.dnWorker.etcdCli.Put(
		dnmw.pch.Ctx,
		key,
		dnmw.dnWorker.prioCode,
		clientv3.WithLease(resp.ID),
	)
	if err != nil {
		dnmw.dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
		dnmw.pch.Logger.Fatal("Put err: %v leaseId=%v key=%s", err, resp.ID, key)
	}
	defer func() {
		dnmw.dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
	}()

	var shardIdToWorker map[string]*dnShardWorker
	var shards map[string]bool
	var rev int64
	shards, rev = getShards(
		dnmw.pch,
		dnmw.dnWorker.etcdCli,
		dnmw.dnWorker.kf.DnShardPrefix(),
		dnmw.dnWorker.grpcTarget,
	)

	for {
		dnShardCh := dnmw.dnWorker.etcdCli.Watch(
			dnmw.pch.Ctx,
			dnmw.dnWorker.kf.DnShardPrefix(),
			clientv3.WithPrefix(),
			clientv3.WithRev(rev),
		)
		select {
		case <-dnShardCh:
			shards, rev = getShards(
				dnmw.pch,
				dnmw.dnWorker.etcdCli,
				dnmw.dnWorker.kf.DnShardPrefix(),
				dnmw.dnWorker.grpcTarget,
			)
			dnmw.pch.Logger.Info("shards: %v", shards)
		case <-dnmw.pch.Ctx.Done():
			return
		case <-dnmw.dnWorker.initTrigger:
			break
		case <-time.After(time.Duration(constants.ShardInitWaitTime) * time.Second):
			break
		}
	}

	for {
		toBeCreated := make([]*dnShardWorker, 0)
		toBeDeleted := make([]*dnShardWorker, 0)
		for shardId, _ := range shards {
			_, ok := shardIdToWorker[shardId]
			if !ok {
				dnsw := newDnShardWorker(
					dnmw.pch.Ctx,
					dnmw.dnWorker,
					shardId,
				)
				toBeCreated = append(toBeCreated, dnsw)
			}
		}
		for shardId, dnsw := range shardIdToWorker {
			_, ok := shards[shardId]
			if !ok {
				toBeDeleted = append(toBeDeleted, dnsw)
			}
		}
		for _, dnsw := range toBeCreated {
			dnsw.run()
			shardIdToWorker[dnsw.shardId] = dnsw
		}
		delay := 3600 * 24 * 365 * 100
		if len(toBeDeleted) > 0 {
			delay = constants.ShardDeleteWaitTime
		}

		dnShardCh := dnmw.dnWorker.etcdCli.Watch(
			dnmw.pch.Ctx,
			dnmw.dnWorker.kf.DnShardPrefix(),
			clientv3.WithPrefix(),
			clientv3.WithRev(rev),
		)
		select {
		case <-dnShardCh:
			shards, rev = getShards(
				dnmw.pch,
				dnmw.dnWorker.etcdCli,
				dnmw.dnWorker.kf.DnShardPrefix(),
				dnmw.dnWorker.grpcTarget,
			)
			dnmw.pch.Logger.Info("shards: %v", shards)
		case <-time.After(time.Duration(delay) * time.Second):
			for _, dnsw := range toBeDeleted {
				dnsw.cancel()
			}
			for _, dnsw := range toBeDeleted {
				dnsw.wait()
				delete(shardIdToWorker, dnsw.shardId)
			}
		case <-dnmw.pch.Ctx.Done():
			break
		}
	}

	for _, dnsw := range shardIdToWorker {
		dnsw.wait()
	}
}

func (dnmw *dnMemberWorker) run() {
	dnmw.wg.Add(1)
	go dnmw.asyncRun()
}

func (dnmw *dnMemberWorker) cancel() {
	dnmw.pch.Logger.Info("Cancel")
	dnmw.pch.Cancel()
}

func (dnmw *dnMemberWorker) wait() {
	dnmw.pch.Logger.Info("Waiting")
	dnmw.wg.Wait()
	dnmw.pch.Logger.Info("Exit")
}

func newDnMemberWorker(
	ctx context.Context,
	dnWorker *dnWorkerServer,
) *dnMemberWorker {
	workerId := uuid.New().String()
	logger := prefixlog.NewPrefixLogger(fmt.Sprintf("dnMemberWorker|%s ", workerId))
	pch := ctxhelper.NewPerCtxHelper(ctx, logger, workerId)
	return &dnMemberWorker{
		pch:      pch,
		dnWorker: dnWorker,
	}
}
