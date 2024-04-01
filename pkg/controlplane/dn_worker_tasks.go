package controlplane

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

func dnMemberWorker(
	parentCtx context.Context,
	wg *sync.WaitGroup,
	dnWorker *dnWorkerServer,
) {
	defer wg.Done()
	workerId := uuid.New().String()
	logger := lib.NewPrefixLogger(fmt.Sprintf("dnMemberWorker|%s ", workerId))
	pch := lib.NewPerCtxHelper(parentCtx, logger, workerId)
	key := dnWorker.kf.dnShardKeyEncode(dnWorker.prioCode, dnWorker.grpcTarget)
	resp, err := dnWorker.etcdCli.Grant(pch.Ctx, dnWorker.grantTimeout)
	if err != nil {
		logger.Fatal("Grant err: %v", err)
	}
	if _, err := dnWorker.etcdCli.KeepAlive(pch.Ctx, resp.ID); err != nil {
		dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
		logger.Fatal("KeepAlive err: %v leaseId=%v", err, resp.ID)
	}
	_, err = dnWorker.etcdCli.Put(pch.Ctx, key, workerId, clientv3.WithLease(resp.ID))
	if err != nil {
		dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
		logger.Fatal("Put err: %v leaseId=%v key=%s", err, resp.ID, key)
	}
	defer func() {
		dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
	}()

	shardWorkerList := buildShardWorkerList(
		pch,
		dnWorker.kf.dnShardPrefix(),
		dnWorker.etcdCli,
	)

	dnShardCh := dnWorker.etcdCli.Watch(
		pch.Ctx,
		dnWorker.kf.dnShardPrefix(),
		clientv3.WithPrefix(),
	)
	for {
		select {
		case <-dnShardCh:
			shardWorkerList = buildShardWorkerList(
				pch,
				dnWorker.kf.dnShardPrefix(),
				dnWorker.etcdCli,
			)
			logger.Info("%v", shardWorkerList)
		case <-pch.Ctx.Done():
			return
		case <-time.After(time.Duration(lib.ShardMemberWaitTime) * time.Second):
			break
		}
	}
}
