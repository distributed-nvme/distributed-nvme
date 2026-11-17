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

// This file pins the bound of architecture.md, Common validation, on a
// pool's stripe times its slice count, the optimal IO size its volumes
// report to hosts, and the bound of architecture.md, Clones, on the optimal
// IO size of a clone's dm-clone: neither may be over
// common.MaxNvmeTcpCmdBytes, the most data one NVMe/TCP command may carry to
// a controller node.

// stbKib is one KiB, for the stripe sizes below.
const stbKib = uint64(1024)

// stbCreate runs CreateStoragePool for a redund_none pool of sliceCnt
// slices, one cntlr and one extent per data group, with stripe as the
// request's stripe (zero names none), and returns the stored stripe.
func stbCreate(
	t *testing.T,
	env *sptEnv,
	name string,
	sliceCnt uint32,
	stripe uint64,
) uint64 {
	t.Helper()
	req := sptSpec{
		name: name, cntlrCnt: 1, sliceCnt: sliceCnt, initExt: 1,
	}.req(env.name)
	if stripe != 0 {
		req.BdevConf = &pb.BdevConf{
			DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: stripe},
		}
	}
	if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
		t.Fatalf("CreateStoragePool %s (%d slices, stripe %d): %v",
			name, sliceCnt, stripe, err)
	}
	return env.spConf(name).GetBdevConf().GetDmRaid0Conf().GetStripeSize()
}

// stbEnv is a cluster whose stored stripe is clusterStripe, with a disk node
// for every side of a redund_none pool at the slice ceiling and one
// controller node with room for several such pools. The block is the
// fixture's sptBlockSize, 4 MiB, which every stripe here divides.
func stbEnv(t *testing.T, clusterStripe uint64) *sptEnv {
	t.Helper()
	const dnCnt = spCreateGrpsPerSlice * common.MaxSliceCntPerSp
	env := sptNewEnv(t, dnCnt, 1, 1024)
	env.cc.BdevConf.DmRaid0Conf.StripeSize = clusterStripe
	mustPut(t, env.cli, model.ClusterConfKey(env.name), env.cc)
	return env
}

// TestCreateStoragePoolRefusesANamedStripeOverTheBound pins the refusal of
// architecture.md, Common validation, and its edge. A named 1 MiB stripe at
// five slices is a 5 MiB product, which hosts would read in commands the
// controller node refuses: INVALID_ARGUMENT, before any read, so the same
// request names a cluster that does not exist and still hears the bound, and
// nothing is written. The message gives the product, the bound and the
// largest stripe that fits. A product of exactly the bound is accepted,
// four slices of 1 MiB and the slice ceiling of 128 KiB, and stored as
// named.
func TestCreateStoragePoolRefusesANamedStripeOverTheBound(t *testing.T) {
	env := stbEnv(t, model.DefaultStripeSize(sptBlockSize))
	for _, tc := range []struct {
		name    string
		cluster string
	}{
		{"on the fixture's cluster", env.name},
		{"on a cluster that does not exist", "no-such-cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sptSpec{
				name: "over0", cntlrCnt: 1, sliceCnt: 5, initExt: 1,
			}.req(tc.cluster)
			req.BdevConf = &pb.BdevConf{
				DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: 1024 * stbKib},
			}
			before := env.dumpAll()
			_, err := env.srv.CreateStoragePool(env.ctx, req)
			sptWantCode(t, err, codes.InvalidArgument)
			msg := status.Convert(err).Message()
			for _, want := range []string{
				"stripe_size 1048576 times slice_cnt 5 is 5242880 bytes",
				fmt.Sprintf("over %d", common.MaxNvmeTcpCmdBytes),
				"hosts would send commands the controller node refuses",
				"the largest stripe_size that fits is 524288",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not say %q", msg, want)
				}
			}
			if changed := sptChangedKeys(before, env.dumpAll()); len(changed) != 0 {
				t.Errorf("a refusal wrote %v", changed)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		sliceCnt uint32
		stripe   uint64
	}{
		{"exact4x1m", 4, 1024 * stbKib},
		{"exact32x128k", common.MaxSliceCntPerSp, 128 * stbKib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stripe*uint64(tc.sliceCnt) != common.MaxNvmeTcpCmdBytes {
				t.Fatalf("%d x %d is not the bound %d", tc.sliceCnt,
					tc.stripe, common.MaxNvmeTcpCmdBytes)
			}
			got := stbCreate(t, env, tc.name, tc.sliceCnt, tc.stripe)
			if got != tc.stripe {
				t.Errorf("stored stripe %d, want the named %d", got,
					tc.stripe)
			}
		})
	}
}

// TestCreateStoragePoolNarrowsTheClusterStripe pins the narrowing of
// architecture.md, Common validation: a pool that names no stripe takes the
// cluster's stored stripe, narrowed to the largest power of two whose
// product with the pool's slice count is within the bound. The cluster's 1
// MiB at five slices is stored as 512 KiB; a cluster stripe that already
// fits, 64 KiB at the slice ceiling, is stored as it is, never narrowed
// further.
func TestCreateStoragePoolNarrowsTheClusterStripe(t *testing.T) {
	t.Run("1 MiB at five slices", func(t *testing.T) {
		env := stbEnv(t, 1024*stbKib)
		if got := stbCreate(t, env, "five0", 5, 0); got != 512*stbKib {
			t.Errorf("stored stripe %d, want 524288", got)
		}
	})
	t.Run("64 KiB at the slice ceiling", func(t *testing.T) {
		env := stbEnv(t, 64*stbKib)
		got := stbCreate(t, env, "wide0", common.MaxSliceCntPerSp, 0)
		if got != 64*stbKib {
			t.Errorf("stored stripe %d, want the cluster's 65536", got)
		}
	})
}

// stbCloneDst rewrites the vol fixture's pool to the given stored geometry,
// as a pool created with it would store it, and adds the destination td
// "dst". The planning read and the deciding STM of CreateClone read the
// pool's stripe, slice count and block from this record.
func stbCloneDst(
	env *volEnv,
	stripe uint64,
	sliceCnt int,
	block uint64,
) {
	env.t.Helper()
	conf := env.spConf()
	conf.BdevConf.DmRaid0Conf.StripeSize = stripe
	conf.BdevConf.DmPoolConf.DataBlockSize = block
	ids := []uint64{volSliceId}
	for idx := 1; idx < sliceCnt; idx++ {
		ids = append(ids, volSliceId+uint64(1000+idx))
	}
	conf.SliceIdList = ids
	env.putSpConf(conf)
	env.putTd("dst", 900, 7, 0, true)
}

// TestCreateCloneRefusesAnOptimalIoSizeOverTheBound pins the refusal of
// architecture.md, Clones: a clone whose dm-clone would report an optimal IO
// size over the bound is FAILED_PRECONDITION and writes nothing, the
// message naming both products, the size and the bound. A one-slice 1 MiB
// source into a pool of five 512 KiB slices keeps each end within the bound,
// 1 MiB and 2.5 MiB, and still stacks to their least common multiple, 5 MiB,
// a whole number of the pool's 1 MiB regions. A pool whose block is over the
// bound refuses any clone, since dm-clone reports at least one region.
func TestCreateCloneRefusesAnOptimalIoSizeOverTheBound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dstStripe   uint64
		dstSliceCnt int
		dstBlock    uint64
		srcStripe   uint64
		srcSliceCnt uint32
		wantSize    string
	}{
		{"1 x 1 MiB into 5 x 512 KiB", 512 * stbKib, 5, 1024 * stbKib,
			1024 * stbKib, 1, "optimal IO size of 5242880 bytes"},
		{"into a pool of 8 MiB blocks", 128 * stbKib, 1, 8192 * stbKib,
			64 * stbKib, 1, "optimal IO size of 8388608 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			stbCloneDst(env, tc.dstStripe, tc.dstSliceCnt, tc.dstBlock)
			req := volCloneRequest(env, "clone-a", "dst")
			req.SrcStripeSize = tc.srcStripe
			req.SrcSliceCnt = tc.srcSliceCnt
			req.SrcBlockSize = 1024 * stbKib
			before := env.dump()
			_, err := env.srv.CreateClone(env.ctx, req)
			msg := volWantCode(t, err, codes.FailedPrecondition)
			for _, want := range []string{
				fmt.Sprintf("src_stripe_size %d times src_slice_cnt %d is "+
					"%d bytes", tc.srcStripe, tc.srcSliceCnt,
					tc.srcStripe*uint64(tc.srcSliceCnt)),
				fmt.Sprintf("stripe_size %d times its slice count %d is "+
					"%d bytes", tc.dstStripe, tc.dstSliceCnt,
					tc.dstStripe*uint64(tc.dstSliceCnt)),
				tc.wantSize,
				fmt.Sprintf("over %d", common.MaxNvmeTcpCmdBytes),
				"hosts would send commands the controller node refuses",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not say %q", msg, want)
				}
			}
			env.wantNoWrite("a refused clone", before)
		})
	}
}

// TestCreateCloneAcceptsAnOptimalIoSizeAtTheBound is the converse: a
// 32-slice source of 128 KiB stripes into the fixture's one-slice pool, and
// a one-slice 1 MiB source into a pool of 32 slices of 128 KiB, each stack
// to exactly the bound, 4 MiB, and are created.
func TestCreateCloneAcceptsAnOptimalIoSizeAtTheBound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dstStripe   uint64
		dstSliceCnt int
		dstBlock    uint64
		srcStripe   uint64
		srcSliceCnt uint32
	}{
		{"32 x 128 KiB into 1 x 128 KiB", volStripe, 1, volPoolBlock,
			128 * stbKib, common.MaxSliceCntPerSp},
		{"1 x 1 MiB into 32 x 128 KiB", 128 * stbKib,
			common.MaxSliceCntPerSp, 1024 * stbKib, 1024 * stbKib, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			stbCloneDst(env, tc.dstStripe, tc.dstSliceCnt, tc.dstBlock)
			if size := model.CloneOptimalIo(tc.srcStripe, tc.srcSliceCnt,
				tc.dstStripe, uint32(tc.dstSliceCnt), tc.dstBlock); size !=
				common.MaxNvmeTcpCmdBytes {
				t.Fatalf("the geometry stacks to %d, not the bound", size)
			}
			req := volCloneRequest(env, "clone-a", "dst")
			req.SrcStripeSize = tc.srcStripe
			req.SrcSliceCnt = tc.srcSliceCnt
			req.SrcBlockSize = 1024 * stbKib
			if _, err := env.srv.CreateClone(env.ctx, req); err != nil {
				t.Fatalf("CreateClone: %v", err)
			}
			if env.clone("clone-a").GetSrcSliceCnt() != tc.srcSliceCnt {
				t.Errorf("the clone record does not carry the source's "+
					"%d slices", tc.srcSliceCnt)
			}
		})
	}
}
