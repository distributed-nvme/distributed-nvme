// cntlr.go is the `cntlr` group (dnvctl.md §5.5): the four RPCs that manage
// the NVMe-oF controllers of one storage pool. Every request here is SP-scoped
// — `cluster_name` from the global --cluster and `sp_name` from the global
// --sp (§5.0) — and the three mutators carry the SpRev token, not a CnRev:
// a cntlr belongs to the pool, so the pool's revision is what guards it (§4).
//
// The only package-level name this file adds beyond registerCntlr is
// cntlrIdFlag, which three of the four leaves need. The group prefix is
// deliberate: sp.go declares an --id of its own for `sp inspect-side` and
// cluster.go, td.go and the rest declare their own identity flags, so an
// unprefixed idFlag would collide across the package (§1.2).
package ctl

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// cntlrIdFlag declares the group's identity flag, the cntlr_id (§5.0). It is
// a string flag read back through hexOf so that Go base-0 parsing applies —
// 3 and 0x3 name the same controller, ids being printed in hex by the rest of
// dnv — and so that a malformed value is a usage error (exit 2) before any
// RPC is issued rather than an INVALID_ARGUMENT from the gateway.
//
// The spelling matches `sp inspect-side --id` on purpose even though that one
// is a side id: the RPC decides what the id means, as in gatewayctl (§5.0).
func cntlrIdFlag(flags *pflag.FlagSet) {
	flags.String("id", "", "cntlr_id (base 0; 0x accepted)")
}

// registerCntlr builds the `cntlr` group and hangs it off the root (§5.5).
func registerCntlr(root *cobra.Command) {
	// cntlr create — CreateCntlr. --slot is the cntlid_slot the new
	// controller takes; it is a plain decimal index, not an id, so it is a
	// uint32 flag rather than a base-0 string (the same split gatewayctl
	// draws between --slot and --id).
	//
	// --cn-black/--cn-white become the CnSelector that steers which CN the
	// gateway places the controller on; both empty yields a nil selector,
	// which is how the request says "not given" and lets the gateway choose.
	create := leaf("create", "create a controller in the storage pool",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			cluster := clusterOf()
			sp := spOf()
			slot := u32Of("slot")
			cnSelector := selectorOf("cn")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateCntlr(ctx, &pb.CreateCntlrRequest{
					ClusterName: cluster,
					SpName:      sp,
					SpRev:       rev,
					CntlidSlot:  slot,
					CnSelector:  cnSelector,
				})
			}, nil
		})
	createFlags := create.Flags()
	createFlags.Uint32("slot", 0, "cntlid_slot for the new controller")
	selectorFlags(createFlags, "cn")

	// cntlr delete — DeleteCntlr. The gateway requires the target to be
	// non-primary and already disabled; dnvctl checks neither and cannot,
	// since both are server state (CT8) — the refusal comes back as
	// FAILED_PRECONDITION.
	del := leaf("delete", "delete a controller",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			id, err := hexOf("id")
			if err != nil {
				return nil, err
			}
			cluster := clusterOf()
			sp := spOf()
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteCntlr(ctx, &pb.DeleteCntlrRequest{
					ClusterName: cluster,
					SpName:      sp,
					SpRev:       rev,
					CntlrId:     id,
				})
			}, nil
		})
	cntlrIdFlag(del.Flags())

	// cntlr set-enabled — UpdateCntlrEnabled. Note the polarity flip against
	// the node RPCs: `cn set-disabled` stores `disabled`, this one stores
	// `enabled`. --enabled defaults to TRUE because disabling is the
	// deliberate act and re-enabling is the undo, so turning it off must be
	// written --enabled=false; --enabled false would be read as a positional
	// argument and rejected by cobra.NoArgs.
	//
	// Disabling the last enabled controller stops IO for the pool. The
	// gateway allows it and dnvctl does not pre-warn in v1 (§0 #10): the
	// warning would need a pre-read of the pool's controllers, which is an
	// RPC the operator did not type. It waits until the gateway carries the
	// hint in the reply itself.
	setEnabled := leaf("set-enabled", "set a controller's enabled flag",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			id, err := hexOf("id")
			if err != nil {
				return nil, err
			}
			cluster := clusterOf()
			sp := spOf()
			enabled := boolOf("enabled")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.UpdateCntlrEnabled(
					ctx, &pb.UpdateCntlrEnabledRequest{
						ClusterName: cluster,
						SpName:      sp,
						SpRev:       rev,
						CntlrId:     id,
						Enabled:     enabled,
					})
			}, nil
		})
	setEnabledFlags := setEnabled.Flags()
	cntlrIdFlag(setEnabledFlags)
	setEnabledFlags.Bool("enabled", true,
		"the new enabled state; write --enabled=false to disable")

	// cntlr inspect — InspectCntlr. A read, so it carries no token: the
	// gateway asks the CN agent that hosts the controller for its live
	// CntlrInfo rather than reading etcd.
	inspect := leaf("inspect",
		"read a controller's live CntlrInfo from its agent",
		func() (job, error) {
			id, err := hexOf("id")
			if err != nil {
				return nil, err
			}
			cluster := clusterOf()
			sp := spOf()
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.InspectCntlr(ctx, &pb.InspectCntlrRequest{
					ClusterName: cluster,
					SpName:      sp,
					CntlrId:     id,
				})
			}, nil
		})
	cntlrIdFlag(inspect.Flags())

	root.AddCommand(group("cntlr", "storage pool controller commands",
		create, del, setEnabled, inspect))
}
