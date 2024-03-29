package controlplane

import (
	"context"
	"fmt"
	"sync"

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
}
