package worker

import (
	// "crypto/md5"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

type shardWorker struct {
	prioCode string
	grpcTarget string
	shardList []string
}

func getShards(
	pch *lib.PerCtxHelper,
	etcdCli *clientv3.Client,
	prefix string,
	selfTarget string,
) (map[string]bool, int64) {
	resp, err := etcdCli.Get(pch.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		pch.Logger.Fatal("Get shard workers failed: %s %v", prefix, err)
	}
	var selfShardWorker *shardWorker
	swList := make([]*shardWorker, 0)
	for _, ev := range resp.Kvs {
		pch.Logger.Info("Shard workers: %s %s", ev.Key, ev.Value)
		keyStr := string(ev.Key)
		grpcTarget := keyStr[len(prefix):]
		prioCode := string(ev.Value)
		if len(prioCode) != lib.ShardCnt {
			pch.Logger.Warning("Ignore invalid prioCode: %s %s", grpcTarget, prioCode)
			continue
		}
		sw := &shardWorker{
			prioCode: prioCode,
			grpcTarget: grpcTarget,
			shardList: make([]string, 0),
		}
		if sw.grpcTarget == selfTarget {
			selfShardWorker = sw
		}
		swList = append(swList, sw)
	}
	if selfShardWorker == nil {
		pch.Logger.Fatal("selfShardWorker is nil: %v", swList)
	}
	shards := make(map[string]bool)
	for _, shard := range selfShardWorker.shardList {
		shards[shard] = true
	}
	return shards, resp.Header.Revision
}