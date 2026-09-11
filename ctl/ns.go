// ns.go is the `ns` group (dnvctl.md §5.8): the four namespace RPCs of
// architecture.md §8.8. A namespace is addressed by its subsystem's --nqn plus
// --idx, the pair every one of the four carries, and all four are SP-scoped
// mutators, so all four take the §4 sp_rev token.
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerNs installs `dnvctl ns …`. §5.8 lists exactly four rows and CT1
// forbids a fifth, so there is no convenience verb here — retiring a namespace
// and repointing it are two RPCs and stay two commands.
func registerNs(root *cobra.Command) {
	root.AddCommand(group("ns", "namespaces of a subsystem",
		nsCreateCmd(),
		nsDeleteCmd(),
		nsSetDevCmd(),
		nsSetSuspendedCmd(),
	))
}

// nsCreateCmd is CreateNamespace.
//
// --idx is the NVMe NSID the host will see. It is the operator's to choose and
// has no useful default; 0 is reserved and the gateway refuses it
// (gateway/subsystem.go, "ns_idx must not be 0"), which under CT8 is a refusal
// dnvctl forwards rather than pre-empts.
//
// --uuid and --nguid are empty by default and an EMPTY value is the documented
// "mint one for me" request (§5.8; the gateway's newDevUuid/newDevNguid run
// inside the transaction). dnvctl therefore generates nothing client-side:
// passing them explicitly is how two SPs' namespaces are deliberately given
// the SAME identity, and that distinction would be destroyed by a client-side
// mint.
//
// --suspended defaults to FALSE here — a namespace is normally created live —
// which is the opposite of `ns set-suspended`'s default; the two flags share a
// name because they carry the same field, not the same intent.
func nsCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a namespace (CreateNamespace)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CreateNamespaceRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				Nqn:         strOf("nqn"),
				NsIdx:       u32Of("idx"),
				DevUuid:     strOf("uuid"),
				DevNguid:    strOf("nguid"),
				Suspended:   boolOf("suspended"),
				TdName:      strOf("td"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.CreateNamespace(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("nqn", "", "nqn of the subsystem the namespace lives in")
	flags.Uint32("idx", 0, "ns_idx, the host-visible NSID")
	flags.String("td", "", "td_name the namespace exports")
	flags.String("uuid", "", "dev_uuid; empty asks the gateway to mint one")
	flags.String("nguid", "", "dev_nguid; empty asks the gateway to mint one")
	flags.Bool("suspended", false,
		"create the namespace already suspended (inaccessible ANA group)")
	return cmd
}

// nsDeleteCmd is DeleteNamespace.
func nsDeleteCmd() *cobra.Command {
	cmd := leaf("delete", "delete a namespace (DeleteNamespace)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteNamespaceRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				Nqn:         strOf("nqn"),
				NsIdx:       u32Of("idx"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.DeleteNamespace(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("nqn", "", "nqn of the subsystem the namespace lives in")
	flags.Uint32("idx", 0, "ns_idx, the host-visible NSID")
	return cmd
}

// nsSetDevCmd is UpdateNamespaceDev: it repoints a live namespace at another
// thin device. The new device travels by NAME — the gateway resolves it to a
// td_id inside the transaction, so a wrong name is a refusal and never a
// stored dangling pointer.
func nsSetDevCmd() *cobra.Command {
	cmd := leaf("set-dev", "repoint a namespace at another thin device "+
		"(UpdateNamespaceDev)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateNamespaceDevRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				Nqn:         strOf("nqn"),
				NsIdx:       u32Of("idx"),
				TdName:      strOf("td"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.UpdateNamespaceDev(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("nqn", "", "nqn of the subsystem the namespace lives in")
	flags.Uint32("idx", 0, "ns_idx, the host-visible NSID")
	flags.String("td", "", "td_name the namespace now exports")
	return cmd
}

// nsSetSuspendedCmd is UpdateNamespaceSuspended, the retire/resume flip.
//
// --suspended defaults to TRUE (§5.8): retiring is the direction an operator
// reaches for, and resuming is the explicit `--suspended=false`. The `=`
// spelling is the only one that works — `--suspended false` would leave
// `false` as a positional argument, which cobra.NoArgs rejects (§5.0).
//
// The RPC writes and bumps the revision even when the stored flag already
// matches, so re-sending the same value is a deliberate way to produce a bump
// rather than a no-op dnvctl should elide.
func nsSetSuspendedCmd() *cobra.Command {
	cmd := leaf("set-suspended", "suspend or resume a namespace "+
		"(UpdateNamespaceSuspended)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateNamespaceSuspendedRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				Nqn:         strOf("nqn"),
				NsIdx:       u32Of("idx"),
				Suspended:   boolOf("suspended"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.UpdateNamespaceSuspended(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("nqn", "", "nqn of the subsystem the namespace lives in")
	flags.Uint32("idx", 0, "ns_idx, the host-visible NSID")
	flags.Bool("suspended", true,
		"the new suspended flag; pass --suspended=false to resume")
	return cmd
}
