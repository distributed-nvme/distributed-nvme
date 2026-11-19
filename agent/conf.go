package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

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

// validNqn is common.ValidNqnPattern, compiled once.
var validNqn = regexp.MustCompile(common.ValidNqnPattern)

// ValidateCntlrNqns refuses a cntlr request that carries an NQN the gateway
// would not store (architecture.md, Common validation): each key of
// nqn_to_subsystem, each subsystem's and each transfer's allowed_hosts and
// each clone's src_nqn is held to common.ValidNqnPattern, within
// common.MaxNqnLength, with no "..", the gateway's own rule (validateNqn,
// gateway/validate.go) copied as this file copies model's. Reason: the cn
// agent builds configfs paths from the subsystem and host NQNs and hands
// the clone's source NQN to the kernel's connect, so a "/" or ".." would let
// a request reach paths outside its directory and a "," would add options to
// the connect. Every NQN a released gateway stored passes this NQN rule;
// only a store written before the gateway held NQNs to it, or by hand,
// breaks it. A clone's src_nqn in the dnv namespace must also decode as a
// transfer NQN (XferNqn), the gateway's rule for CreateClone
// (validateCloneSrcNqn, gateway/validate.go): the cn agent removes a clone
// source once its clone has gone only when it is a transfer of this
// cluster, judged by its NQN, or a transfer of another cluster or one in no
// dnv namespace, judged at node level by its controllers' host NQN
// (cnagent.md CN21), so any other source in the dnv namespace would stay
// connected for good. A gateway from before this rule, the released one
// among them, took any src_nqn that passes the NQN rule, so its store can
// carry a clone source this rule refuses. The first refusal, in a fixed
// order, is the error.
func ValidateCntlrNqns(req *pb.SyncupCntlrRequest) error {
	check := func(field, nqn string) error {
		switch {
		case nqn == "":
			return invalidConf("%s is empty", field)
		case len(nqn) > common.MaxNqnLength:
			return invalidConf("%s is %d bytes, the maximum is %d", field,
				len(nqn), common.MaxNqnLength)
		case !validNqn.MatchString(nqn):
			return invalidConf("%s %q is not a valid NQN", field, nqn)
		case strings.Contains(nqn, ".."):
			return invalidConf("%s %q contains \"..\"", field, nqn)
		}
		return nil
	}
	nqns := make([]string, 0, len(req.GetNqnToSubsystem()))
	for nqn := range req.GetNqnToSubsystem() {
		nqns = append(nqns, nqn)
	}
	sort.Strings(nqns)
	for _, nqn := range nqns {
		if err := check("nqn_to_subsystem key", nqn); err != nil {
			return err
		}
		for _, host := range req.GetNqnToSubsystem()[nqn].GetAllowedHosts() {
			if err := check("subsystem allowed_hosts", host); err != nil {
				return err
			}
		}
	}
	for _, xfer := range req.GetXferList() {
		for _, host := range xfer.GetAllowedHosts() {
			if err := check("transfer allowed_hosts", host); err != nil {
				return err
			}
		}
	}
	for _, clone := range req.GetCloneList() {
		src := clone.GetSrcNqn()
		if err := check("clone src_nqn", src); err != nil {
			return err
		}
		if !common.IsCloneSrcNqn(src) {
			return invalidConf("clone src_nqn %q is in the dnv namespace %q "+
				"and is no transfer NQN", src, common.NqnPrefix+":")
		}
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

// InvalidConfReply builds the refusal the three validators above feed, and the
// dn agent's side conf gate on side_conf.zero_bytes too (dnagent.md DN8),
// whose validator lives in package dnagent because it copies no model rule.
// It is the revision gate's sibling: the request is not applied, nothing is
// converged, and the worker retries with whatever the control plane stores
// next round. Its details are cut to the row text cap (CutAgentText).
func InvalidConfReply(format string, args ...any) *pb.AgentReply {
	return &pb.AgentReply{
		Code:    common.ReplyCodeInvalidConf,
		Details: CutAgentText(fmt.Sprintf(format, args...)),
	}
}
