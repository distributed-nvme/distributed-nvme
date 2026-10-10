package agent

import (
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Conf validation on the agent side (architecture.md, Common validation).
//
// The control plane resolves every defaultable conf member when it WRITES the
// conf, so a request that reaches an agent carries concrete values. An agent
// that finds a zero refuses the converge instead of substituting a constant:
// a geometry the agent invented is one the rest of the cluster does not share,
// and dm, md and the on-disk headers would be built against it. Refusing
// leaves the object unconverged and the operator with a named field, which is
// the loud failure Common validation asks for.
//
// These are deliberately a SECOND copy of model.ValidateBdevConf's rules,
// error strings included: layout.md, Dependency rules, forbids the agent
// packages from
// importing model, which links the etcd client. The two must stay identical,
// which is why agent/conf_test.go and model/capacity_test.go assert the same
// literals: a change made to one copy and not the other goes red.

// invalidConf builds the one error shape both validators below use. The
// "invalid stored conf: " prefix is what an operator greps for.
func invalidConf(format string, args ...any) error {
	return fmt.Errorf("invalid stored conf: "+format, args...)
}

// ValidateBdevConf refuses a BdevConf whose geometry is not concrete: the four
// defaultable members are checked for presence, and nothing else — the ranges
// of Common validation belong to the gateway, which sees the request that set
// them.
//
// low_water_mark_pct is checked for zero ONLY. A value above 100 is the legal
// "never grow this pool automatically" setting (Common validation), and the
// chunk count only exists when the redund_conf oneof chose md-raid1.
func ValidateBdevConf(conf *pb.BdevConf) error {
	if conf.GetDmPoolConf().GetDataBlockSize() == 0 {
		return invalidConf("bdev_conf.dm_pool_conf.data_block_size is zero")
	}
	if conf.GetDmPoolConf().GetLowWaterMarkPct() == 0 {
		return invalidConf("bdev_conf.dm_pool_conf.low_water_mark_pct is zero")
	}
	if conf.GetDmRaid0Conf().GetStripeSize() == 0 {
		return invalidConf("bdev_conf.dm_raid0_conf.stripe_size is zero")
	}
	if raid1 := conf.GetRedundConf().GetRedundMdRaid1(); raid1 != nil &&
		raid1.GetBitmapChunkBlockCnt() == 0 {
		return invalidConf(
			"bdev_conf.redund_conf.redund_md_raid1." +
				"bitmap_chunk_block_cnt is zero")
	}
	return nil
}

// ValidateExtentSize refuses the dn agent's one defaultable member. It is the
// number every disk header is formatted with (architecture.md, Disk node), so
// a zero must never reach a format or a verify.
func ValidateExtentSize(extentSize uint64) error {
	if extentSize == 0 {
		return invalidConf("dn_bin_conf.extent_size is zero")
	}
	return nil
}

// InvalidConfReply builds the refusal the two validators above feed, and the
// dn agent's side conf gate on side_conf.zero_bytes too (dnagent.md DN8),
// whose validator lives in package dnagent because it copies no model rule.
// It is the revision gate's sibling: the request is not applied, nothing is
// converged, and the worker retries with whatever the control plane stores
// next round.
func InvalidConfReply(format string, args ...any) *pb.AgentReply {
	return &pb.AgentReply{
		Code:    common.ReplyCodeInvalidConf,
		Details: fmt.Sprintf(format, args...),
	}
}
