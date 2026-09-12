// The cluster subcommands of dnvctl (dnvctl.md §5.1): the four RPCs that
// address a cluster as a whole. This is the one group whose commands do not
// simply take the global --cluster: each of `create`, `delete` and `get`
// declares its own --name, which WINS when non-empty and falls back to the
// global when empty (§5.0's field→flag rule, gatewayctl's clusterNameOf).
// root.go's clusterNameOf implements that fallback; the flag itself is
// declared here because only this group has it.
//
// `cluster list` is the single exception in the whole CLI: ListClustersRequest
// is the only Gateway request with no cluster_name field at all, so the global
// --cluster is simply ignored there (§5.1) and --name is not declared on it.
//
// No conf flags in v1 (§5.1's notes): a created cluster takes the gateway's
// pure defaults for qos_ratio, bdev_conf, dn_bin_conf, alloc_conf and
// health_check_conf, which is why CreateClusterRequest here carries only a
// name.

package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerCluster adds the §5.1 group to the root command.
func registerCluster(root *cobra.Command) {
	root.AddCommand(group("cluster", "clusters",
		clusterCreateCmd(),
		clusterDeleteCmd(),
		clusterGetCmd(),
		clusterListCmd(),
	))
}

// clusterNameFlag declares the group's own identity flag. It is shared by the
// three commands that carry cluster_name, and is named with this file's group
// prefix so it cannot collide with a sibling group file (§1.2).
//
// The default is empty rather than anything cluster-like precisely so that
// "not given" is distinguishable and clusterNameOf can fall back to --cluster.
// Leaving BOTH unset sends an empty cluster_name, which the gateway does not
// refuse — it substitutes common.DefaultClusterName (`gateway/common.go`'s own
// clusterNameOf). dnvctl neither substitutes nor complains (CT8): an operator
// who wants the default cluster gets it by naming nothing.
func clusterNameFlag(cmd *cobra.Command) {
	cmd.Flags().String("name", "",
		"cluster_name to act on; empty falls back to the global --cluster")
}

// clusterCreateCmd drives CreateCluster.
func clusterCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a cluster (CreateCluster)",
		func() (job, error) {
			req := &pb.CreateClusterRequest{
				ClusterName: clusterNameOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateCluster(ctx, req)
			}, nil
		})
	clusterNameFlag(cmd)
	return cmd
}

// clusterDeleteCmd drives DeleteCluster. A cluster delete carries no revision
// token — the §4 trio covers DNs, CNs and SPs only — so the global --rev is
// ignored here.
func clusterDeleteCmd() *cobra.Command {
	cmd := leaf("delete", "delete a cluster (DeleteCluster)",
		func() (job, error) {
			req := &pb.DeleteClusterRequest{
				ClusterName: clusterNameOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteCluster(ctx, req)
			}, nil
		})
	clusterNameFlag(cmd)
	return cmd
}

// clusterGetCmd drives GetCluster, whose reply carries the ClusterConf and the
// DN/CN/SP globals.
func clusterGetCmd() *cobra.Command {
	cmd := leaf("get", "read a cluster's conf and globals (GetCluster)",
		func() (job, error) {
			req := &pb.GetClusterRequest{
				ClusterName: clusterNameOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetCluster(ctx, req)
			}, nil
		})
	clusterNameFlag(cmd)
	return cmd
}

// clusterListCmd drives ListClusters, the one request with no cluster_name: it
// takes the page flags and nothing else, and declares no --name at all so that
// `cluster list --name x` is a usage error rather than a silently dropped
// value.
//
// Count is a uint32 on the wire and pageFlags declares --count as a Uint32
// flag, so a --count above 2^32-1 dies as a pflag parse failure (exit 2, no
// RPC issued) rather than being truncated; u32Of only narrows viper's uint64
// read, which is lossless for any value the flag accepts.
func clusterListCmd() *cobra.Command {
	cmd := leaf("list", "list cluster names (ListClusters)",
		func() (job, error) {
			req := &pb.ListClustersRequest{
				Count:     u32Of("count"),
				PageToken: strOf("page-token"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ListClusters(ctx, req)
			}, nil
		})
	pageFlags(cmd.Flags())
	return cmd
}
