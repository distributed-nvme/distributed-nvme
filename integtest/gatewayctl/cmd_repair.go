// The repair half of the driver's subcommand table: repairs and retired legs
// (architecture.md, Repairs and Retired legs; gateway.md, Repairs and
// retired legs).
//
// Every RPC here is SP-scoped and pure etcd, so each subcommand has the shape
// of cmd_vol.go's: `--sp` names the pool, every mutator carries the `--rev`
// token, always sent (a present zero when omitted, GW6's always-stale
// probe), and the readers carry none. `--name` is rep_name, and `--leg` the
// retired leg's id. abort-repair drives TryToAbortRepair, the spelling
// dnvctl gives the same call (`repair abort`).
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
