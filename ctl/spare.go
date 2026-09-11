// spare.go is dnvctl.md §5.12: the three RPCs of spare legs
// (architecture.md §8.12). A spare is an extra leg of a raid1 group that every
// cntlr connects to and health-checks but that is NOT an md member — parked
// standby capacity, until a switch trades it for an active leg.
//
// The identity flags are the group's own (§5.0): `--grp` everywhere, plus
// `--leg` on delete and the `--spare`/`--target` pair on switch. All four are
// ids, so they are declared as strings and read through hexOf: Go base-0
// parsing, `0x` accepted, and a malformed value is a usage error (exit 2) with
// no RPC issued — the whole of dnvctl's input checking (CT8).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerSpare adds the `spare` group to the root. All three are SP-scoped
// mutators, so all three carry an `sp_rev` token when --rev is given (§4).
func registerSpare(root *cobra.Command) {
	root.AddCommand(group(
		"spare",
		"spare legs — parked standby replicas of a raid1 group (§8.12)",
		spareCreateCmd(),
		spareDeleteCmd(),
		spareSwitchCmd(),
	))
}

// spareCreateCmd is CreateSpareLeg: one extra leg for a raid1 group, allocated
// and connected but unprovisioned and outside the md array.
//
// --grp is the group the leg joins; the optional --dn-black / --dn-white
// selector steers the spare away from the DNs the group's active legs already
// use (nil when both lists are empty). dnvctl does not pre-check the group's
// redundancy or its spare count: a RedundNone group is the gateway's
// INVALID_ARGUMENT and a full one its RESOURCE_EXHAUSTED (CT8).
func spareCreateCmd() *cobra.Command {
	cmd := leaf(
		"create",
		"CreateSpareLeg: allocate a parked spare leg for a group",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			grpId, err := hexOf("grp")
			if err != nil {
				return nil, err
			}
			req := &pb.CreateSpareLegRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				GrpId:       grpId,
				DnSelector:  selectorOf("dn"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateSpareLeg(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("grp", "", "grp_id the spare leg joins (base 0)")
	selectorFlags(flags, "dn")
	return cmd
}

// spareDeleteCmd is DeleteSpareLeg. Both ids are given: --grp says which
// group holds the spare list and --leg which entry of it goes away, which is
// also how a leg that a switch just parked is released.
func spareDeleteCmd() *cobra.Command {
	cmd := leaf(
		"delete",
		"DeleteSpareLeg: release a parked spare leg",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			grpId, err := hexOf("grp")
			if err != nil {
				return nil, err
			}
			legId, err := hexOf("leg")
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteSpareLegRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				GrpId:       grpId,
				LegId:       legId,
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteSpareLeg(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("grp", "", "grp_id holding the spare leg (base 0)")
	flags.String("leg", "", "leg_id of the spare leg to delete (base 0)")
	return cmd
}

// spareSwitchCmd is SwitchSpareLeg, the swap and the only way a spare becomes
// active: --spare is the parked leg that takes the active role and --target
// the active leg it replaces, which is parked in the spare list in exchange.
//
// Naming both sides explicitly rather than inferring the direction is what
// lets a caller assert the reply's curr_active_leg_id / curr_spare_leg_id pair
// afterwards. The gateway refuses a spare whose side is not yet `provisioned`
// (an unzeroed spare must never become an md member); dnvctl forwards the
// request and lets that FAILED_PRECONDITION answer (CT8).
func spareSwitchCmd() *cobra.Command {
	cmd := leaf(
		"switch",
		"SwitchSpareLeg: promote a spare leg and park the active one",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			grpId, err := hexOf("grp")
			if err != nil {
				return nil, err
			}
			spareLegId, err := hexOf("spare")
			if err != nil {
				return nil, err
			}
			targetLegId, err := hexOf("target")
			if err != nil {
				return nil, err
			}
			req := &pb.SwitchSpareLegRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				GrpId:       grpId,
				SpareLegId:  spareLegId,
				TargetLegId: targetLegId,
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.SwitchSpareLeg(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("grp", "", "grp_id holding both legs (base 0)")
	flags.String("spare", "",
		"spare_leg_id, the parked leg switched in (base 0)")
	flags.String("target", "",
		"target_leg_id, the active leg switched out (base 0)")
	return cmd
}
