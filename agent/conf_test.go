package agent

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Stored-conf validation (dnagent.md §2.1, architecture.md §7)
// ---------------------------------------------------------------------------
//
// conf.go is a deliberate SECOND copy of model's stored-conf rules, because
// layout.md §3 forbids the agent packages from importing model. The error
// strings are the contract that keeps the two copies in step: the gateway
// turns model's into an ABORTED message, the agents put these into an
// AgentReply's details, and one grep finds every refusal. So the table below
// pins the whole string, prefix included, with the same literals
// model/capacity_test.go asserts — a change made to one copy and not the
// other goes red here or there.
//
// The defaults themselves stay out of this package. cnagent.md §7 item 10
// greps agent/ for the four `common.Default*` conf constants and requires no
// hit, because an agent that can name one is an agent that could substitute
// it; the fixtures below therefore carry plain numbers.

// confBdevConf is a stored BdevConf with every member ValidateBdevConf
// requires concrete, and no redund_conf — which is the redund_none choice
// (architecture.md §8.4), the arm that has no bitmap at all.
func confBdevConf() *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   1 << 20,
			LowWaterMarkPct: 50,
		},
		DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: 65536},
	}
}

// confRaid1BdevConf is confBdevConf plus the md-raid1 arm of the redund_conf
// oneof, whose bitmap_chunk_block_cnt is the one member ValidateBdevConf
// requires for that choice and only for it.
func confRaid1BdevConf(chunkBlockCnt uint64) *pb.BdevConf {
	conf := confBdevConf()
	conf.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{
				BitmapChunkBlockCnt: chunkBlockCnt,
			},
		},
	}
	return conf
}

func TestValidateBdevConf(t *testing.T) {
	autoGrowOff := confBdevConf()
	autoGrowOff.DmPoolConf.LowWaterMarkPct = 4096
	explicitNone := confBdevConf()
	explicitNone.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundNone{RedundNone: &pb.RedundNone{}},
	}
	// A conf missing every one of the four members still fails on the first
	// one: the validator names a field, not a list, and nil takes the same
	// path because every getter is nil-safe.
	noBlockSize := confRaid1BdevConf(128)
	noBlockSize.DmPoolConf.DataBlockSize = 0
	noPct := confBdevConf()
	noPct.DmPoolConf.LowWaterMarkPct = 0
	noStripe := confBdevConf()
	noStripe.DmRaid0Conf.StripeSize = 0
	cases := []struct {
		name string
		conf *pb.BdevConf
		// wantErr is "" when the conf must be accepted.
		wantErr string
	}{
		{
			// A redund_none pool has no bitmap: the missing chunk count is
			// correct, not an omission.
			name: "redund_none, no bitmap chunk count",
			conf: confBdevConf(),
		},
		{
			name: "an explicit redund_none, no bitmap chunk count",
			conf: explicitNone,
		},
		{
			name: "md-raid1 with a chunk count",
			conf: confRaid1BdevConf(128),
		},
		{
			// Above 100 means "never auto-grow this pool" (§7) and is a
			// value, not a fault: this validator checks presence, never
			// range.
			name: "low_water_mark_pct above 100",
			conf: autoGrowOff,
		},
		{
			name: "nil",
			conf: nil,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			name: "data_block_size zero",
			conf: noBlockSize,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			name: "low_water_mark_pct zero",
			conf: noPct,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.low_water_mark_pct is zero",
		},
		{
			name: "stripe_size zero",
			conf: noStripe,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_raid0_conf.stripe_size is zero",
		},
		{
			name: "md-raid1 without a chunk count",
			conf: confRaid1BdevConf(0),
			wantErr: "invalid stored conf: bdev_conf.redund_conf." +
				"redund_md_raid1.bitmap_chunk_block_cnt is zero",
		},
	}
	for _, tc := range cases {
		err := ValidateBdevConf(tc.conf)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: got %v, want accepted", tc.name, err)
			}
			continue
		}
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateExtentSize(t *testing.T) {
	if err := ValidateExtentSize(0); err == nil ||
		err.Error() !=
			"invalid stored conf: dn_bin_conf.extent_size is zero" {
		t.Errorf("zero: got %v, want the extent_size refusal", err)
	}
	// No range check: a cluster whose DNs were formatted at an unusual
	// extent size must keep working, so presence is all that is asked.
	for _, size := range []uint64{1, 4096, 1 << 20, 1 << 40} {
		if err := ValidateExtentSize(size); err != nil {
			t.Errorf("extent size %d: got %v, want accepted", size, err)
		}
	}
}

// InvalidConfReply is the refusal both validators feed, and its code is the
// one the worker branches on: the object is known and the revision current,
// so neither of the other two reply codes fits (dnagent.md DN4, cnagent.md
// CN8).
func TestInvalidConfReply(t *testing.T) {
	reply := InvalidConfReply("%v", ValidateExtentSize(0))
	if reply.GetCode() != common.ReplyCodeInvalidConf {
		t.Errorf("code %d, want %d", reply.GetCode(),
			common.ReplyCodeInvalidConf)
	}
	if reply.GetCode() == common.ReplyCodeStaleRevision ||
		reply.GetCode() == common.ReplyCodeUnknownObject {
		t.Errorf("the refusal reuses another code: %d", reply.GetCode())
	}
	if reply.GetDetails() !=
		"invalid stored conf: dn_bin_conf.extent_size is zero" {
		t.Errorf("details %q, want the validator's own text",
			reply.GetDetails())
	}
}
