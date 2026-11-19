// retiredleg.go is dnvctl.md, `retired-leg` — `ctl/retiredleg.go`: the three
// RPCs of retired legs (architecture.md, Retired legs). A retired leg is the
// leg a finished repair replaced; its side keeps its data on its disk node
// until it is deleted here, by the sp worker's retire timer, or taken back by
// `repair create --retired-leg`, and its keep flag, set here or by a forced
// start of its group, keeps it from the timer.
//
// The identity flag is `--leg`, the retired leg's own leg id, read through
// hexOf like every id (CT8).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerRetiredLeg adds the `retired-leg` group to the root. The delete and
// the keep carry an `sp_rev` token when --rev is given (CT3); the list
// carries none.
func registerRetiredLeg(root *cobra.Command) {
	root.AddCommand(group(
		"retired-leg",
		"retired legs — the kept legs finished repairs replaced "+
			"(architecture.md, Retired legs)",
		retiredLegListCmd(),
		retiredLegDeleteCmd(),
		retiredLegKeepCmd(),
	))
}

// retiredLegKeepCmd is UpdateRetiredLegKeep: it sets the retired leg's keep
// flag, or with --clear clears it. While the flag is set the sp worker's
// retire timer passes the leg over and the finish of its repair at the
// retired-leg bound never gives it back; `retired-leg delete` still deletes
// it. The leaf sends the value given, never a toggle: re-sending the stored
// value is the gateway's no-write. Its refusals are the gateway's (CT8).
func retiredLegKeepCmd() *cobra.Command {
	cmd := leaf(
		"keep",
		"UpdateRetiredLegKeep: keep a retired leg from the sp worker's "+
			"retire timer and its give-back, or with --clear let them take "+
			"it again",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			legId, err := hexOf("leg")
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateRetiredLegKeepRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				LegId:       legId,
				Keep:        !boolOf("clear"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.UpdateRetiredLegKeep(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("leg", "", "leg_id of the retired leg (decimal, or hex after 0x)")
	flags.Bool("clear", false, "clear the keep flag instead of setting it")
	return cmd
}

// retiredLegListCmd is ListRetiredLegs: every retired leg of the pool, keyed
// by leg id. The list is not paged.
func retiredLegListCmd() *cobra.Command {
	return leaf(
		"list",
		"ListRetiredLegs: read every retired leg of the pool",
		func() (job, error) {
			req := &pb.ListRetiredLegsRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ListRetiredLegs(ctx, req)
			}, nil
		})
}

// retiredLegDeleteCmd is DeleteRetiredLeg: the retired leg goes and its side
// gives its disk node back what it took. It runs at every sp_level, and its
// refusals are the gateway's (CT8).
func retiredLegDeleteCmd() *cobra.Command {
	cmd := leaf(
		"delete",
		"DeleteRetiredLeg: delete a retired leg and free its side",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			legId, err := hexOf("leg")
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteRetiredLegRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				LegId:       legId,
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteRetiredLeg(ctx, req)
			}, nil
		})
	cmd.Flags().String("leg", "", "leg_id of the retired leg (decimal, or hex after 0x)")
	return cmd
}
