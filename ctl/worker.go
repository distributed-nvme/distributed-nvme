// worker.go is dnvctl.md, `worker` — `ctl/worker.go`: the read of the worker
// registry (architecture.md, Worker registry), the registrations of the
// dnv-worker processes by role and seed.
//
// The registry belongs to no cluster, so ListWorkersRequest has no field at
// all: the global --cluster and --sp are ignored here, as --sp is on
// `cluster list` (dnvctl.md, Global flags, env, config).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerWorker adds the `worker` group to the root. Its one leaf is a read
// and carries no token (CT3).
func registerWorker(root *cobra.Command) {
	root.AddCommand(group(
		"worker",
		"worker registrations — each dnv-worker by role and seed "+
			"(architecture.md, Worker registry)",
		workerListCmd(),
	))
}

// workerListCmd is ListWorkers: every registration as stored, by role and
// seed, with the epoch its worker last wrote. It takes no flag, and the list
// is not paged.
func workerListCmd() *cobra.Command {
	return leaf(
		"list",
		"ListWorkers: read every worker registration, by role and seed",
		func() (job, error) {
			req := &pb.ListWorkersRequest{}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ListWorkers(ctx, req)
			}, nil
		})
}
