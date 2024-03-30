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
	ctx context.Context,
	wg *sync.WaitGroup,
	dnWorker *dnWorkerServer,
) {
	defer wg.Done()
	workerId := uuid.New().String()
	logger := lib.NewPrefixLogger(fmt.Sprintf("dnMemberWorker|%s ", workerId))
	pch := &lib.PerCtxHelper{
		Ctx: ctx,
		Logger: logger,
		TraceId: workerId,
	}
	key := dnWorker.kf.dnShardKeyEncode(dnWorker.leadingCode, dnWorker.grpcTarget)
	resp, err := dnWorker.etcdCli.Grant(ctx, dnWorker.grantTimeout)
	if err != nil {
		logger.Fatal("Grant err: %v", err)
	}
	if _, err := dnWorker.etcdCli.KeepAlive(ctx, resp.ID); err != nil {
		dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
		logger.Fatal("KeepAlive err: %v leaseId=%v", err, resp.ID)
	}
	_, err = dnWorker.etcdCli.Put(ctx, key, workerId, clientv3.WithLease(resp.ID))
	if err != nil {
		dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
		logger.Fatal("Put err: %v leaseId=%v key=%s", err, resp.ID, key)
	}
	defer func() {
		dnWorker.etcdCli.Revoke(context.Background(), resp.ID)
	}()

	dnShardCh := dnWorker.etcdCli.Watch(
		ctx,
		dnWorker.kf.dnShardPrefix(),
		clientv3.WithPrefix(),
	)
	shardWorkerList := buildShardWorkerList(
		pch,
		dnWorker.kf.dnShardPrefix(),
		dnWorker.etcdCli,
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
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(lib.ShardMemberWaitTime) * time.Second):
			break
		}
	}
}
