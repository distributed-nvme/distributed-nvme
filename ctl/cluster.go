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
// One conf flag in v1 (§5.1's notes): `create` declares --extent-size, which
// fills dn_bin_conf.extent_size and nothing else. The ClusterConf's other
// conf members — qos_ratio, bdev_conf, alloc_conf and health_check_conf —
// have no flag at all, so a created cluster still takes the gateway's pure
// defaults for them, and for dn_bin_conf too whenever --extent-size is left
// at its zero. Cluster-scoped is the whole of that claim: `sp create` does
// declare flags that fill an SP's OWN bdev_conf (§5.4), which is a different
// message from the cluster-wide one this command leaves untouched.

package ctl

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/common"
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
//
// --extent-size is the one cluster-conf value v1 exposes. It follows the
// `sp create` convention for an optional sub-message (spCreateCmd's
// --stripe-size and --block-size): zero means "not given", so dn_bin_conf is
// built only when the flag is non-zero and an untouched create sends no
// dn_bin_conf at all.
//
// The four bin shifts are deliberately NOT exposed, and the message this
// builds carries only extent_size. model.ResolveDnBinConf takes the shift set
// as a WHOLE — the all-zero set is not a ladder, so it falls back to the
// 0/4/8/12 defaults together rather than shift by shift — which means an
// extent-size-only request is stored as {extent_size, 0, 4, 8, 12}: the
// operator's size on the default ladder. A flag for a SINGLE shift could
// therefore only ever produce a set that changes nothing (all four still
// zero) or one the gateway refuses (validateDnBinConf takes the all-zero set
// and a strict ladder, and nothing in between).
//
// No client-side range check (CT8): the gateway bounds a non-zero extent_size
// to [MinDnExtSize, MaxDnExtSize] itself, and dnvctl forwards whatever pflag
// parsed. Only the command line is parsed, though. pflag turns the flag's
// argument into a uint64 and refuses what does not fit (exit 2, no RPC),
// while CT9's two other carriers hand viper their value unparsed and u64Of
// is viper.GetUint64 — a cast that drops its error. So DNVCTL_EXTENT_SIZE=-1
// (or an "abc" under that key in a --config file) reads back as 0 and sends
// no dn_bin_conf at all, leaving the cluster on common.DefaultDnExtSize
// rather than refusing. And a ClusterConf is write-once — no RPC updates one
// — so that is the difference between a usage error and a cluster whose
// extent size is permanently wrong. TestClusterCreateExtentSize pins both
// sides of the asymmetry, on the flag and on the environment.
func clusterCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a cluster (CreateCluster)",
		func() (job, error) {
			req := &pb.CreateClusterRequest{
				ClusterName: clusterNameOf(),
			}
			if extentSize := u64Of("extent-size"); extentSize != 0 {
				req.DnBinConf = &pb.DnBinConf{ExtentSize: extentSize}
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateCluster(ctx, req)
			}, nil
		})
	clusterNameFlag(cmd)
	flags := cmd.Flags()
	// The default is INTERPOLATED from common.DefaultDnExtSize, spCreateCmd's
	// --slice-cnt rule: `--help` is where an operator learns what a zero buys
	// them, so the number has to be the gateway's own and has to move with it
	// — a Go identifier in a usage string is one an operator cannot resolve.
	flags.Uint64("extent-size", 0,
		fmt.Sprintf("dn_bin_conf.extent_size in bytes (0 = not sent; the "+
			"gateway default is %d)", common.DefaultDnExtSize))
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
