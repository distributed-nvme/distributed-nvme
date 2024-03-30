package controlplane

import (
	// "crypto/md5"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

type shardWorker struct {
	leadingCode string
	grpcTarget string
	shardList []string
}

func buildShardWorkerList(
	pch *lib.PerCtxHelper,
	prefix string,
	etcdCli *clientv3.Client,
) []*shardWorker {
	resp, err := etcdCli.Get(pch.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		pch.Logger.Fatal("Get shard workers failed: %s %v", prefix, err)
	}
	swList := make([]*shardWorker, 0)
	for _, ev := range resp.Kvs {
		pch.Logger.Info("Shard workers: %s %s", ev.Key, ev.Value)
		leadingCode, grpcTarget, err := shardKeyDecode(prefix, string(ev.Key))
		if err != nil {
			pch.Logger.Warning(
				"Ignore invalid key: %s %s %v",
				prefix,
				ev.Key,
				err,
			)
			continue
		}
		sw := &shardWorker{
			leadingCode: leadingCode,
			grpcTarget: grpcTarget,
			shardList: make([]string, 0),
		}
		swList = append(swList, sw)
	}
	return swList
}
