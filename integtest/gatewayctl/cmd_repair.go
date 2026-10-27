// The repair half of the driver's subcommand table: repairs and retired legs
// (architecture.md, Repairs and Retired legs; gateway.md, Repairs and
// retired legs), the forced start of a group, and the group's
// operator_in_charge flag.
//
// Every RPC here is SP-scoped, so each subcommand has the shape of
// cmd_vol.go's: `--sp` names the pool, every mutator carries the `--rev`
// token, always sent (a present zero when omitted, GW6's always-stale
// probe), and the readers carry none. `--name` is rep_name, and `--leg` the
// retired leg's id, or for force-start-grp the leg the group's array starts
// from. abort-repair drives TryToAbortRepair, the spelling dnvctl gives the
// same call (`repair abort`). All are pure etcd but force-start-grp, whose
// gateway reads the primary's report on the way (gateway.md AG1).
package main

import (
	"context"
	"flag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// setupCreateRepair drives CreateRepair. --bad-legs is the bad_leg_id_list,
// sent as typed so the suite can send a count other than one; --retired-leg
// takes a retired leg of the group back as the new leg, and when it is zero
// the selector steers the fresh new leg's DN.
func setupCreateRepair(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	repName := fs.String("name", "", "rep_name")
	var grp, retiredLeg hexUint
	fs.Var(&grp, "grp", "grp_id of the group repaired")
	var badLegs idList
	fs.Var(&badLegs, "bad-legs", "bad_leg_id_list, comma-separated")
	fs.Var(&retiredLeg, "retired-leg",
		"retired_leg_id taken back as the new leg (0 = a fresh leg)")
	dnSelector := dnSelectorFlags(fs)
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateRepair(ctx, &pb.CreateRepairRequest{
			ClusterName:  g.cluster,
			SpName:       *spName,
			SpRev:        &pb.SpRev{Revision: uint64(rev)},
			RepName:      *repName,
			GrpId:        uint64(grp),
			BadLegIdList: badLegs,
			RetiredLegId: uint64(retiredLeg),
			DnSelector:   dnSelector(),
		})
	}
}

// setupFinishRepair drives FinishRepair.
func setupFinishRepair(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	repName := fs.String("name", "", "rep_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.FinishRepair(ctx, &pb.FinishRepairRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			RepName:     *repName,
		})
	}
}

// setupCancelRepair drives CancelRepair.
func setupCancelRepair(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	repName := fs.String("name", "", "rep_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CancelRepair(ctx, &pb.CancelRepairRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			RepName:     *repName,
		})
	}
}

// setupAbortRepair drives TryToAbortRepair.
func setupAbortRepair(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	repName := fs.String("name", "", "rep_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.TryToAbortRepair(ctx, &pb.TryToAbortRepairRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			RepName:     *repName,
		})
	}
}

// setupGetRepair drives GetRepair. Read-only, no token.
func setupGetRepair(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	repName := fs.String("name", "", "rep_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.GetRepair(ctx, &pb.GetRepairRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			RepName:     *repName,
		})
	}
}

// setupListRepairs drives ListRepairs. Read-only, no token, not paged.
func setupListRepairs(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.ListRepairs(ctx, &pb.ListRepairsRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
		})
	}
}

// setupListRetiredLegs drives ListRetiredLegs. Read-only, no token, not
// paged.
func setupListRetiredLegs(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.ListRetiredLegs(ctx, &pb.ListRetiredLegsRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
		})
	}
}

// setupDeleteRetiredLeg drives DeleteRetiredLeg.
func setupDeleteRetiredLeg(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	var leg hexUint
	fs.Var(&leg, "leg", "leg_id of the retired leg")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteRetiredLeg(ctx, &pb.DeleteRetiredLegRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			LegId:       uint64(leg),
		})
	}
}

// setupForceStartGrp drives ForceStartGroup. --grp is the group's grp_id and
// --leg the leg its array starts from; --cancel withdraws the group's mark
// instead and is sent with --leg as typed, so the suite can send a leg with
// a cancel and see it refused.
func setupForceStartGrp(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	var grp, leg hexUint
	fs.Var(&grp, "grp", "grp_id of the group")
	fs.Var(&leg, "leg",
		"leg_id the group's array starts from (0 with --cancel)")
	cancel := fs.Bool("cancel", false,
		"cancel — withdraw the group's forced-start mark")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.ForceStartGroup(ctx, &pb.ForceStartGroupRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			GrpId:       uint64(grp),
			LegId:       uint64(leg),
			Cancel:      *cancel,
		})
	}
}

// setupSetGrpOperatorInCharge drives UpdateGroupOperatorInCharge. --grp is
// the group's grp_id. --operator-in-charge defaults to true, as dnvctl's
// does, because the set is the deliberate act and the clear the undo; the
// flag package never consumes the next argument for a bool, so the script
// must write `--operator-in-charge=false`, never `--operator-in-charge
// false`.
func setupSetGrpOperatorInCharge(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	var grp hexUint
	fs.Var(&grp, "grp", "grp_id of the group")
	on := fs.Bool("operator-in-charge", true,
		"the value the group's flag takes; write --operator-in-charge=false "+
			"to clear it")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateGroupOperatorInCharge(ctx,
			&pb.UpdateGroupOperatorInChargeRequest{
				ClusterName:      g.cluster,
				SpName:           *spName,
				SpRev:            &pb.SpRev{Revision: uint64(rev)},
				GrpId:            uint64(grp),
				OperatorInCharge: *on,
			})
	}
}
