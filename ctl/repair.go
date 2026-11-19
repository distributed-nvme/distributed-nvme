// repair.go is dnvctl.md, `repair` — `ctl/repair.go`: the six RPCs of an
// operator's repair of a raid1 group's leg (architecture.md, Repairs). A
// repair names one bad leg of the group and carries a new leg; the primary
// adds the new leg to the running array, the sp worker moves the repair's
// stage, and the operator finishes, aborts or cancels it (architecture.md,
// Repair).
//
// The identity flag is the group-uniform `--name`, the repair's rep_name.
// `repair abort` is the one leaf of the tree whose verb is not its RPC's:
// the call is TryToAbortRepair, which only sets the abort flag, and the
// leaf keeps the short verb (dnvctl.md, `repair` — `ctl/repair.go`).
// dnvctl checks no stage: a call the repair's stage does not allow is the
// gateway's FAILED_PRECONDITION (CT8).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerRepair adds the `repair` group to the root. The four mutators carry
// an `sp_rev` token when --rev is given (CT3); get and list carry none.
func registerRepair(root *cobra.Command) {
	root.AddCommand(group(
		"repair",
		"repairs — replace a bad leg of a raid1 group by a new leg "+
			"(architecture.md, Repairs)",
		repairCreateCmd(),
		repairFinishCmd(),
		repairCancelCmd(),
		repairAbortCmd(),
		repairGetCmd(),
		repairListCmd(),
	))
}

// repairCreateCmd is CreateRepair.
//
// --grp is the group and --bad-legs the bad leg ids, a comma list because the
// request's field is a list: dnvctl sends it as typed, and a count other than
// one is the gateway's INVALID_ARGUMENT (CT8). The new leg is the retired leg
// --retired-leg of the group, or, when that is zero or not given, a fresh leg
// the --dn-black / --dn-white selector steers (nil when both are empty).
// --force lets the primary copy over a leg of another md array: the retired
// leg taken back, and the bad leg once the repair is aborted (cnagent.md
// CN12). --force-undecided is another flag: it lets the gateway take a bad
// leg whose row the primary's report marks as one of two identities the
// assembly cannot decide between, naming it the stale copy; the repair keeps
// nothing of it, so an abort leaves the leg as it is (architecture.md,
// Repairs).
func repairCreateCmd() *cobra.Command {
	cmd := leaf(
		"create",
		"CreateRepair: start replacing a bad leg of a group by a new leg",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			grpId, err := hexOf("grp")
			if err != nil {
				return nil, err
			}
			badLegIds, err := idListOf("bad-legs")
			if err != nil {
				return nil, err
			}
			retiredLegId, err := hexOf("retired-leg")
			if err != nil {
				return nil, err
			}
			req := &pb.CreateRepairRequest{
				ClusterName:    clusterOf(),
				SpName:         spOf(),
				SpRev:          rev,
				RepName:        strOf("name"),
				GrpId:          grpId,
				BadLegIdList:   badLegIds,
				RetiredLegId:   retiredLegId,
				DnSelector:     selectorOf("dn"),
				Force:          boolOf("force"),
				ForceUndecided: boolOf("force-undecided"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateRepair(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "rep_name")
	flags.String("grp", "",
		"grp_id of the group repaired (decimal, or hex after 0x)")
	flags.String("bad-legs", "",
		"comma-separated bad_leg_id_list (each in decimal, or in hex "+
			"after 0x)")
	flags.String("retired-leg", "",
		"retired_leg_id taken back as the new leg (decimal, or hex "+
			"after 0x; 0 or left out = a fresh leg)")
	selectorFlags(flags, "dn")
	flags.Bool("force", false,
		"copy over a leg of another md array, the retired leg taken back "+
			"and the bad leg after an abort, losing what it holds")
	flags.Bool("force-undecided", false,
		"take a bad leg of two md identities the primary cannot decide "+
			"between, naming it the stale copy; an abort keeps it")
	return cmd
}

// repairFinishCmd is FinishRepair: the new leg takes the bad leg's place and
// the bad leg is kept as a retired leg. The reply names the retired leg and,
// for a worker's repair whose group held the most retired legs it may hold,
// the oldest retired leg that is not kept the finish gave back
// (given_back_leg_id, zero when none).
func repairFinishCmd() *cobra.Command {
	cmd := leaf(
		"finish",
		"FinishRepair: put the new leg in the bad leg's place",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.FinishRepairRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				RepName:     strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.FinishRepair(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "rep_name")
	return cmd
}

// repairCancelCmd is CancelRepair: an aborted repair goes, and a new leg
// taken from a retired leg goes back as that retired leg.
func repairCancelCmd() *cobra.Command {
	cmd := leaf(
		"cancel",
		"CancelRepair: drop an aborted repair",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CancelRepairRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				RepName:     strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CancelRepair(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "rep_name")
	return cmd
}

// repairAbortCmd is TryToAbortRepair, the one leaf whose verb is not its
// RPC's: it sets the repair's abort flag, and the primary's answer moves the
// repair to ABORTED, or to STARTED when it added the new leg first
// (architecture.md, Repair), which `repair get` shows.
func repairAbortCmd() *cobra.Command {
	cmd := leaf(
		"abort",
		"TryToAbortRepair: ask the primary to abort a repair",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.TryToAbortRepairRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				RepName:     strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.TryToAbortRepair(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "rep_name")
	return cmd
}

// repairGetCmd is GetRepair, a read with no token: the record as stored, its
// stage and abort flag included (CT4).
func repairGetCmd() *cobra.Command {
	cmd := leaf(
		"get",
		"GetRepair: read one repair record",
		func() (job, error) {
			req := &pb.GetRepairRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				RepName:     strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetRepair(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "rep_name")
	return cmd
}

// repairListCmd is ListRepairs: every repair of the pool, keyed by name. The
// list is not paged, so the leaf has no flag of its own.
func repairListCmd() *cobra.Command {
	return leaf(
		"list",
		"ListRepairs: read every repair of the pool",
		func() (job, error) {
			req := &pb.ListRepairsRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ListRepairs(ctx, req)
			}, nil
		})
}
