package gateway

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file pins the block's bound of architecture.md, Common validation, at
// the calls: a pool's data block is at most common.MaxNvmeTcpCmdBytes, the
// most data one NVMe/TCP command may carry, since the block is the region of
// every migration of one of the pool's sides, and that migration's dm-clone
// reports at least one region as its optimal IO size. CreateCluster's half is
// TestCreateClusterRefusesABlockOverTheBound.

// bbOver is the smallest power-of-two block over the bound.
const bbOver = 2 * common.MaxNvmeTcpCmdBytes

// TestCreateStoragePoolRefusesANamedBlockOverTheBound pins the refusal of a
// block the request names: INVALID_ARGUMENT before any read, so a cluster
// that does not exist hears it too, the message naming the block, the bound
// and the reason, and nothing written. A block at the bound is stored.
func TestCreateStoragePoolRefusesANamedBlockOverTheBound(t *testing.T) {
	env := stbEnv(t, model.DefaultStripeSize(sptBlockSize))
	for _, cluster := range []string{env.name, "no-such-cluster"} {
		req := sptSpec{
			name: "over", cntlrCnt: 1, sliceCnt: 1, initExt: 1,
		}.req(cluster)
		req.BdevConf = &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{DataBlockSize: bbOver},
		}
		before := env.dumpAll()
		_, err := env.srv.CreateStoragePool(env.ctx, req)
		sptWantCode(t, err, codes.InvalidArgument)
		msg := status.Convert(err).Message()
		for _, want := range []string{
			fmt.Sprintf("data_block_size %d is over %d", bbOver,
				common.MaxNvmeTcpCmdBytes),
			"migration",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("on %s: message %q does not say %q", cluster, msg,
					want)
			}
		}
		if changed := sptChangedKeys(before, env.dumpAll()); len(
			changed) != 0 {
			t.Errorf("on %s: a refusal wrote %v", cluster, changed)
		}
	}
	req := sptSpec{
		name: "atbound", cntlrCnt: 1, sliceCnt: 1, initExt: 1,
	}.req(env.name)
	req.BdevConf = &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize: common.MaxNvmeTcpCmdBytes,
		},
	}
	if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
		t.Fatalf("CreateStoragePool with a block at the bound: %v", err)
	}
	if got := env.spConf("atbound").GetBdevConf().GetDmPoolConf().
		GetDataBlockSize(); got != common.MaxNvmeTcpCmdBytes {
		t.Errorf("stored block %d, want %d", got,
			common.MaxNvmeTcpCmdBytes)
	}
}

// TestCreateStoragePoolRefusesAnInheritedBlockOverTheBound pins the inherited
// half: on a cluster that stores a block over the bound, which only a create
// before the bound could store, a pool that names no block is refused
// INVALID_ARGUMENT, the message saying the block is the cluster's and to name
// one, and nothing is written. The block is never narrowed: the operator's
// block, and with it the md bitmap chunk's bytes, would change unasked. A
// pool that names a block within the bound is created with it.
func TestCreateStoragePoolRefusesAnInheritedBlockOverTheBound(t *testing.T) {
	env := stbEnv(t, model.DefaultStripeSize(sptBlockSize))
	env.cc.BdevConf.DmPoolConf.DataBlockSize = bbOver
	mustPut(t, env.cli, model.ClusterConfKey(env.name), env.cc)
	req := sptSpec{
		name: "inherit", cntlrCnt: 1, sliceCnt: 1, initExt: 1,
	}.req(env.name)
	before := env.dumpAll()
	_, err := env.srv.CreateStoragePool(env.ctx, req)
	sptWantCode(t, err, codes.InvalidArgument)
	msg := status.Convert(err).Message()
	for _, want := range []string{
		"bdev_conf merged over the cluster's: ",
		fmt.Sprintf("data_block_size %d is over %d", bbOver,
			common.MaxNvmeTcpCmdBytes),
		"the data_block_size is the cluster's",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	if changed := sptChangedKeys(before, env.dumpAll()); len(changed) != 0 {
		t.Errorf("a refusal wrote %v", changed)
	}
	req.BdevConf = &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize: common.MaxNvmeTcpCmdBytes,
		},
	}
	if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
		t.Fatalf("CreateStoragePool naming a block at the bound: %v", err)
	}
	if got := env.spConf("inherit").GetBdevConf().GetDmPoolConf().
		GetDataBlockSize(); got != common.MaxNvmeTcpCmdBytes {
		t.Errorf("stored block %d, want the named %d", got,
			common.MaxNvmeTcpCmdBytes)
	}
}

// TestCreateMigrationRefusesAPoolWhoseBlockIsOverTheBound pins the stored
// half: a pool made before the bound keeps its block, and a migration of one
// of its sides would make a dm-clone that reports at least one block as its
// optimal IO size, over what the destination's kernel takes in one command.
// CreateMigration refuses it FAILED_PRECONDITION, naming the block, the
// bound and the way out, a move of the pool's volumes to a new pool, since
// no call moves a healthy leg of such a pool in place; from its planning
// read: a selector that leaves no disk node, which the scan would answer
// RESOURCE_EXHAUSTED, does not hide it. Nothing is written.
func TestCreateMigrationRefusesAPoolWhoseBlockIsOverTheBound(t *testing.T) {
	env := newVolEnv(t)
	conf := env.spConf()
	conf.BdevConf.DmPoolConf.DataBlockSize = bbOver
	env.putSpConf(conf)
	for _, selector := range []string{volDnC, volDnA} {
		before := env.dump()
		_, err := env.srv.CreateMigration(env.ctx, &pb.CreateMigrationRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			MigrName:    "migr-a",
			SrcSideId:   volDataSideA,
			DnSelector:  volDnSelector(selector),
		})
		msg := volWantCode(t, err, codes.FailedPrecondition)
		for _, want := range []string{
			fmt.Sprint(bbOver), fmt.Sprint(common.MaxNvmeTcpCmdBytes),
			"move the pool's volumes to a new pool",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("selector %s: message %q does not name %s",
					selector, msg, want)
			}
		}
		if changed := sptChangedKeys(before, env.dump()); len(changed) != 0 {
			t.Errorf("selector %s: a refusal wrote %v", selector, changed)
		}
	}
}
