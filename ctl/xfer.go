// xfer.go is dnvctl.md §5.10: the four RPCs of a transfer, the SOURCE half of
// a cross-SP copy (architecture.md §8.10). A transfer re-exports one existing
// namespace's raid0 under its own `XferNqn` subsystem so a clone in another SP
// — possibly another cluster — can read the bytes; transfer + clone is the
// §11.3 cross-SP live migration of a volume.
//
// Everything here follows root.go's contract: `build` reads its values back
// through viper (CT9), returns a usage error for anything that fails to PARSE
// (CT8, exit 2, no RPC issued), and the returned job does nothing but call the
// client method.
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerXfer adds the `xfer` group to the root. The identity flag is the
// group-uniform `--name` (§5.0), and `sp_name`/`cluster_name` come from the
// globals on all four.
func registerXfer(root *cobra.Command) {
	root.AddCommand(group(
		"xfer",
		"transfers — the source side of a cross-SP copy (§8.10)",
		xferCreateCmd(),
		xferDeleteCmd(),
		xferGetCmd(),
		xferSetHostsCmd(),
	))
}

// xferCreateCmd is CreateTransfer.
//
// --ori-nqn and --ori-idx name an existing namespace of THIS SP, the one whose
// bytes the destination clone will read. --hosts is `allowed_hosts`, which for
// a transfer carries the DESTINATION cntlrs' host NQNs: the xfer subsystem is
// reached directly and is never advertised through a CdcEntry.
//
// --auto-suspend asks the primary cntlr to retire the origin namespace itself
// while it builds the transfer stack (ANA inaccessible everywhere, then the
// origin's dm device suspended), instead of the operator sending a separate
// `ns set-suspended`. CT8: dnvctl does not check that the pair resolves — a
// wrong one is the gateway's NOT_FOUND.
func xferCreateCmd() *cobra.Command {
	cmd := leaf(
		"create",
		"CreateTransfer: export an origin namespace for a remote clone",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CreateTransferRequest{
				ClusterName:  clusterOf(),
				SpName:       spOf(),
				SpRev:        rev,
				XferName:     strOf("name"),
				OriNqn:       strOf("ori-nqn"),
				OriNsIdx:     u32Of("ori-idx"),
				AllowedHosts: strListOf("hosts"),
				AutoSuspend:  boolOf("auto-suspend"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateTransfer(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "xfer_name")
	flags.String("ori-nqn", "", "ori_nqn, the origin subsystem")
	flags.Uint32("ori-idx", 0, "ori_ns_idx, the origin ns_idx")
	flags.String("hosts", "",
		"allowed_hosts, comma-separated destination cntlr host nqns")
	flags.Bool("auto-suspend", false,
		"suspend the origin namespace as part of building the transfer")
	return cmd
}

// xferDeleteCmd is DeleteTransfer.
//
// --force picks between the two teardown semantics, which are opposites rather
// than degrees: without it the STM additionally sets `suspended = true` on the
// origin namespace, FINALIZING a completed hand-over so the source stays
// retired; with it the origin is left as it is, ABORTING the transfer so the
// next syncup restores normal service (§8.10).
func xferDeleteCmd() *cobra.Command {
	cmd := leaf(
		"delete",
		"DeleteTransfer: drop the xfer subsystem and its stack",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteTransferRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				XferName:    strOf("name"),
				Force:       boolOf("force"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteTransfer(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "xfer_name")
	flags.Bool("force", false,
		"abort path: delete without suspending the origin namespace")
	return cmd
}

// xferGetCmd is GetTransfer, a pure STM read. It bumps nothing, so §4 gives it
// no token and the global --rev is simply ignored here.
func xferGetCmd() *cobra.Command {
	cmd := leaf(
		"get",
		"GetTransfer: read one transfer record",
		func() (job, error) {
			req := &pb.GetTransferRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				XferName:    strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetTransfer(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "xfer_name")
	return cmd
}

// xferSetHostsCmd is UpdateTransferHosts — the RPC for "the destination cntlr
// moved to another CN". Like every --hosts in dnvctl the value REPLACES the
// stored `allowed_hosts` rather than adding to it, so an empty --hosts clears
// the list and locks the subsystem down.
func xferSetHostsCmd() *cobra.Command {
	cmd := leaf(
		"set-hosts",
		"UpdateTransferHosts: replace allowed_hosts of a transfer",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateTransferHostsRequest{
				ClusterName:  clusterOf(),
				SpName:       spOf(),
				SpRev:        rev,
				XferName:     strOf("name"),
				AllowedHosts: strListOf("hosts"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.UpdateTransferHosts(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "xfer_name")
	flags.String("hosts", "",
		"allowed_hosts, comma-separated host nqns — the whole new list")
	return cmd
}
