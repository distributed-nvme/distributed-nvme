// This file is the `ss` group: the NVMe-oF subsystems of architecture.md §8.8
// (dnvctl.md §5.7, four RPCs).
//
// A subsystem is SP-scoped like everything in §5.6-§5.11, so cluster_name and
// sp_name come from the §2.1 globals and the only identity flag is --nqn. The
// three mutators carry `sp_rev` from the global --rev (presence-based, §4);
// `ss list` carries no token and takes no page flags, because ListSubsystems
// is unpaginated.
//
// A namespace is a field of its subsystem rather than a key of its own, so
// `ss list` is also the only gateway read that shows namespaces at all — the
// `ns` group (§5.8) can create and update them but never read one back.
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerSs adds the `ss` group to the root. NewRootCmd calls this by name,
// so the name and signature are the contract (§1.2).
func registerSs(root *cobra.Command) {
	root.AddCommand(group(
		"ss", "nvme-of subsystems of a storage pool",
		ssCreateCmd(),
		ssDeleteCmd(),
		ssListCmd(),
		ssSetHostsCmd(),
	))
}

// ssCreateCmd is CreateSubsystem.
//
// --hosts fills allowed_hosts, the host NQNs permitted to connect. It is
// optional because a subsystem with no host is legal and useful: it is the
// shape to create before granting access, so that the namespaces can be
// attached first and the hosts let in afterwards with `ss set-hosts`.
func ssCreateCmd() *cobra.Command {
	cmd := leaf(
		"create", "create a subsystem (CreateSubsystem)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CreateSubsystemRequest{
				ClusterName:  clusterOf(),
				SpName:       spOf(),
				SpRev:        rev,
				Nqn:          strOf("nqn"),
				AllowedHosts: strListOf("hosts"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.CreateSubsystem(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("nqn", "", "subsystem nqn")
	flags.String("hosts", "",
		"allowed_hosts: comma-separated host nqns; empty grants none")
	return cmd
}

// ssDeleteCmd is DeleteSubsystem. The gateway refuses one that still has
// namespaces; dnvctl knows none of that and asks (CT8).
func ssDeleteCmd() *cobra.Command {
	cmd := leaf(
		"delete", "delete a subsystem (DeleteSubsystem)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteSubsystemRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				Nqn:         strOf("nqn"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.DeleteSubsystem(ctx, req)
			}, nil
		})
	cmd.Flags().String("nqn", "", "subsystem nqn")
	return cmd
}

// ssListCmd is ListSubsystems: the read-back that carries each subsystem's
// namespaces with it, and the only place a namespace is visible from the
// gateway. The request is nothing but the two scope globals — no token, and no
// page flags because the RPC is unpaginated (§5.7 lists none).
func ssListCmd() *cobra.Command {
	return leaf(
		"list", "list the subsystems of a pool (ListSubsystems)",
		func() (job, error) {
			req := &pb.ListSubsystemsRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.ListSubsystems(ctx, req)
			}, nil
		})
}

// ssSetHostsCmd is UpdateSubsystemHosts. --hosts is the FULL replacement list,
// matching the replace-on-occurrence semantics of every §5.0 string list: an
// empty --hosts= revokes every host rather than leaving the stored list alone,
// which is the only way to revoke one and is why the flag has no "add" or
// "remove" spelling (CT1 — one command per RPC, and the RPC replaces).
func ssSetHostsCmd() *cobra.Command {
	cmd := leaf(
		"set-hosts", "replace a subsystem's host list "+
			"(UpdateSubsystemHosts)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateSubsystemHostsRequest{
				ClusterName:  clusterOf(),
				SpName:       spOf(),
				SpRev:        rev,
				Nqn:          strOf("nqn"),
				AllowedHosts: strListOf("hosts"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.UpdateSubsystemHosts(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("nqn", "", "subsystem nqn")
	flags.String("hosts", "",
		"allowed_hosts: comma-separated host nqns — the whole new "+
			"list; empty revokes every host")
	return cmd
}
