package mbrhelper

import (
	"context"
	"crypto/md5"
	"fmt"
	"slices"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type shardMember struct {
	prioCode   string
	grpcTarget string
	shardList  []string
}

type ShardMemberSummary struct {
	revision      int64
	shardToOwners map[string][]string
	ownerToShards map[string][]string
}

func (sms *ShardMemberSummary) GetRevision() int64 {
	return sms.revision
}

func (sms *ShardMemberSummary) GetShardListByOwner(
	grpcTarget string,
) []string {
	var shardList []string
	shardList, ok := sms.ownerToShards[grpcTarget]
	if !ok {
		shardList = make([]string, 0)
	}
	return shardList
}

func (sms *ShardMemberSummary) GetShardMapByOwner(
	grpcTarget string,
) map[string]bool {
	shardMap := make(map[string]bool)
	shardList := sms.GetShardListByOwner(grpcTarget)
	for _, shardId := range shardList {
		shardMap[shardId] = true
	}
	return shardMap
}

func NewShardMemberSummary(
	etcdCli *clientv3.Client,
	pch *ctxhelper.PerCtxHelper,
	prefix string,
	replica uint32,
) (*ShardMemberSummary, error) {
	resp, err := etcdCli.Get(pch.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	smList := make([]*shardMember, 0)
	for _, ev := range resp.Kvs {
		pch.Logger.Info("member conf: %s %s", ev.Key, ev.Value)
		keyStr := string(ev.Key)
		grpcTarget := keyStr[len(prefix):]
		prioCode := string(ev.Value)
		if len(prioCode) != constants.ShardCnt {
			pch.Logger.Warning("Ingore invalid prioCode: %s %s", ev.Key, prioCode)
			continue
		}
		sm := &shardMember{
			prioCode:   prioCode,
			grpcTarget: grpcTarget,
			shardList:  make([]string, 0),
		}
		smList = append(smList, sm)
	}

	shardToOwners := make(map[string][]string)
	ownerToShards := make(map[string][]string)
	for i := 0; i < constants.ShardCnt; i++ {
		shardId := fmt.Sprintf(constants.ShardIdFormat, i)
		valueToShardMember := make(map[string]*shardMember)
		valueList := make([]string, 0)
		for _, sm := range smList {
			code := string(sm.prioCode[i])
			if code == "0" {
				continue
			}
			md5In := fmt.Sprintf("%s-%s", sm.grpcTarget, shardId)
			md5Out := md5.Sum([]byte(md5In))
			value := fmt.Sprintf("%s-%s-%s", code, md5Out, sm.grpcTarget)
			valueToShardMember[value] = sm
			valueList = append(valueList, value)
		}
		slices.Sort(valueList)
		slices.Reverse(valueList)
		owners := make([]string, 0)
		for j := 0; uint32(j) < replica+1 && j < len(valueList); j++ {
			value := valueList[j]
			sm := valueToShardMember[value]
			sm.shardList = append(sm.shardList, shardId)
			owners = append(owners, sm.grpcTarget)
		}
		shardToOwners[shardId] = owners
	}
	for _, sm := range smList {
		ownerToShards[sm.grpcTarget] = sm.shardList
	}
	return &ShardMemberSummary{
		revision:      resp.Header.Revision,
		shardToOwners: shardToOwners,
		ownerToShards: ownerToShards,
	}, nil
}

func RegisterMember(
	etcdCli *clientv3.Client,
	pch *ctxhelper.PerCtxHelper,
	prefix string,
	grpcTarget string,
	prioCode string,
	grantTimeout int64,
) (func(), error) {
	resp, err := etcdCli.Grant(pch.Ctx, grantTimeout)
	if err != nil {
		return nil, err
	}

	revokeFun := func() {
		revokeCtx, _ := context.WithTimeout(
			context.Background(),
			constants.RollbackTimeout,
		)
		etcdCli.Revoke(revokeCtx, resp.ID)
	}

	if _, err := etcdCli.KeepAlive(
		pch.Ctx,
		resp.ID,
	); err != nil {
		revokeFun()
		return nil, err
	}

	key := fmt.Sprintf("%s/%s", prefix, grpcTarget)
	if _, err := etcdCli.Put(
		pch.Ctx,
		key,
		prioCode,
		clientv3.WithLease(resp.ID),
	); err != nil {
		revokeFun()
		return nil, err
	}

	return revokeFun, nil
}
