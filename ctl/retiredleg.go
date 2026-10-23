// retiredleg.go is dnvctl.md, `retired-leg` — `ctl/retiredleg.go`: the two
// RPCs of retired legs (architecture.md, Retired legs). A retired leg is the
// leg a finished repair replaced; its side keeps its data on its disk node
// until it is deleted here, by the sp worker's retire timer, or taken back by
// `repair create --retired-leg`.
//
// The identity flag is `--leg`, the retired leg's own leg id, read through
// hexOf like every id (CT8).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerRetiredLeg adds the `retired-leg` group to the root. The delete
// carries an `sp_rev` token when --rev is given (CT3); the list carries none.
func registerRetiredLeg(root *cobra.Command) {
	root.AddCommand(group(
		"retired-leg",
		"retired legs — the kept legs finished repairs replaced "+
			"(architecture.md, Retired legs)",
		retiredLegListCmd(),
		retiredLegDeleteCmd(),
	))
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
	cmd.Flags().String("leg", "", "leg_id of the retired leg (base 0)")
	return cmd
}
