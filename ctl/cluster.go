// The cluster subcommands of dnvctl
// (dnvctl.md, `cluster` — `ctl/cluster.go`): the six RPCs that address a
// cluster as a whole. This is the one group whose commands do not simply
// take the global --cluster: `create`, `delete`, `get`, `get-capacity` and
// `advance-ids` declare their own --name, which WINS when non-empty and falls back to the
// global flag when empty (the field→flag rule of dnvctl.md, Conventions;
// gatewayctl's clusterNameOf).
// root.go's clusterNameOf implements that fallback; the flag itself is
// declared here because only this group has it.
//
// `cluster list` is the one leaf of this group whose request,
// ListClustersRequest, has no cluster_name field at all (the other in the
// whole CLI is `worker list`, ctl/worker.go), so the global --cluster is
// simply ignored there (dnvctl.md, `cluster` — `ctl/cluster.go`) and --name is
// not declared on it.
//
// Three conf flags (dnvctl.md, `cluster` — `ctl/cluster.go`): `create`
// declares --extent-size, which fills dn_bin_conf.extent_size and nothing
// else, --md-check-days, which fills md_check_conf.interval_days, and
// --start-hold-seconds, which fills start_hold_conf.seconds. The
// ClusterConf's other conf members — qos_ratio, bdev_conf, alloc_conf and
// health_check_conf — have no flag at all, so a created cluster still takes
// the gateway's defaults for the last three and an unset qos_ratio, which
// has no default; it takes the default dn_bin_conf too whenever
// --extent-size is left at its zero, the default md_check_conf whenever
// --md-check-days is not typed, and the default start_hold_conf whenever
// --start-hold-seconds is not typed. Cluster-scoped is the whole of that claim:
// `sp create` does declare flags that fill an SP's OWN bdev_conf (dnvctl.md,
// `sp` — `ctl/sp.go`), which is a different message from the cluster-wide
// one this command leaves untouched.

package ctl

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerCluster adds the `cluster` group to the root command.
func registerCluster(root *cobra.Command) {
	root.AddCommand(group("cluster", "clusters",
		clusterCreateCmd(),
		clusterDeleteCmd(),
		clusterGetCmd(),
		clusterListCmd(),
		clusterAdvanceIdsCmd(),
		clusterGetCapacityCmd(),
	))
}

// clusterNameFlag declares the group's own identity flag. It is shared by the
// five commands that carry cluster_name, and is named with this file's group
// prefix so it cannot collide with a sibling group file (dnvctl.md, Files).
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
// --extent-size is one of the three cluster-conf values dnvctl exposes. It
// follows the `sp create` convention for an optional sub-message
// (spCreateCmd's --stripe-size and --block-size): zero means "not given", so
// dn_bin_conf is built only when the flag is non-zero and an untouched create
// sends no dn_bin_conf at all. --md-check-days and --start-hold-seconds, the
// other two, do not follow it: the zero of the first turns the md check off
// and of the second the start hold (dnv-worker.md HL8), so each is sent
// whenever typed (TestClusterCreateMdCheckDays,
// TestClusterCreateStartHoldSeconds).
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
// parsed. pflag turns the flag's argument into a uint64 and refuses what does
// not fit (exit 2, no RPC), and the command line is the flag's only carrier
// (CT9): DNVCTL_EXTENT_SIZE and an extent-size key in a --config file reach
// no request. A ClusterConf is write-once — no RPC updates one — so a second
// carrier would be the worst kind here: a stale variable would size every
// cluster created under it, and text viper cannot cast would read back
// as 0, leaving the cluster on common.DefaultDnExtSize for good.
// TestClusterCreateExtentSize pins the flag's refusal and the environment's
// silence.
func clusterCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a cluster (CreateCluster)",
		func() (job, error) {
			req := &pb.CreateClusterRequest{
				ClusterName: clusterNameOf(),
			}
			if extentSize := u64Of("extent-size"); extentSize != 0 {
				req.DnBinConf = &pb.DnBinConf{ExtentSize: extentSize}
			}
			// Sent only when typed, since its zero turns the check off and
			// an absent setting takes the gateway's default.
			if invoked.Changed("md-check-days") {
				req.MdCheckConf = &pb.MdCheckConf{
					IntervalDays: u32Of("md-check-days"),
				}
			}
			// Sent only when typed, for the same reason: its zero turns
			// the hold off, and an absent setting takes the default.
			if invoked.Changed("start-hold-seconds") {
				req.StartHoldConf = &pb.StartHoldConf{
					Seconds: u32Of("start-hold-seconds"),
				}
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
	flags.Uint32("md-check-days", 0,
		fmt.Sprintf("md_check_conf.interval_days, the days between two md "+
			"checks of a group; 0 turns the check off (sent only when "+
			"given; the gateway default is %d)", common.DefaultMdCheckDays))
	flags.Uint32("start-hold-seconds", 0,
		fmt.Sprintf("start_hold_conf.seconds, the seconds from an agent's "+
			"start during which the worker holds its verdict on a starting "+
			"reply; 0 turns the hold off (sent only when given; the "+
			"gateway default is %d)", common.DefaultStartHoldSeconds))
	return cmd
}

// clusterDeleteCmd drives DeleteCluster. A cluster delete carries no revision
// token — the CT3 trio covers DNs, CNs and SPs only — so a typed --rev is a
// usage error here.
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

// clusterListCmd drives ListClusters, a request with no cluster_name: it
// takes the page flags and nothing else, and declares no --name at all so that
// `cluster list --name x` is a usage error rather than a silently dropped
// value.
//
// Count is a uint32 on the wire and pageFlags declares --count as a Uint32
// flag, so a --count above 2^32-1 dies as a pflag parse failure (exit 2, no
// RPC issued) rather than being truncated; u32Of reads that Uint32 flag
// itself, so nothing is narrowed after the parse.
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

// clusterAdvanceIdsCmd drives AdvanceClusterIds, the id step of
// architecture.md, etcd backup and restore. Like `cluster delete` it carries
// no revision token — the CT3 trio covers DNs, CNs and SPs only — so a typed
// --rev is a usage error here, and --name is its only flag. Its RPC commits
// one transaction per pool of the cluster, so a cluster of many pools needs a
// --timeout that covers them all (dnvctl.md, `cluster` — `ctl/cluster.go`).
func clusterAdvanceIdsCmd() *cobra.Command {
	cmd := leaf("advance-ids",
		"move a cluster's id counters far forward after an etcd restore "+
			"(AdvanceClusterIds)",
		func() (job, error) {
			req := &pb.AdvanceClusterIdsRequest{
				ClusterName: clusterNameOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.AdvanceClusterIds(ctx, req)
			}, nil
		})
	clusterNameFlag(cmd)
	return cmd
}

// clusterGetCapacityCmd drives GetClusterCapacity, the capacity read of
// architecture.md, Clusters: every node's recorded extent counts and flags,
// and the space answer for the pool shape the three counts name, each sent
// as typed, so a zero takes the gateway's default as `sp create` does. A
// read, so it carries no token (CT3); like `cluster get` it names its
// cluster with --name and falls back to the global --cluster.
func clusterGetCapacityCmd() *cobra.Command {
	cmd := leaf("get-capacity",
		"read every node's recorded extent counts and flags, and what "+
			"pools of one shape can still place (GetClusterCapacity)",
		func() (job, error) {
			req := &pb.GetClusterCapacityRequest{
				ClusterName: clusterNameOf(),
				LegCnt:      u32Of("leg-cnt"),
				CntlrCnt:    u32Of("cntlr-cnt"),
				SliceCnt:    u32Of("slice-cnt"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetClusterCapacity(ctx, req)
			}, nil
		})
	clusterNameFlag(cmd)
	// The defaults are interpolated, spCreateCmd's --slice-cnt rule.
	flags := cmd.Flags()
	flags.Uint32("leg-cnt", 0,
		"leg_cnt — legs a group has: 2 for RAID1, 1 without redundancy "+
			"(0 = the cluster's redundancy)")
	flags.Uint32("cntlr-cnt", 0,
		fmt.Sprintf("cntlr_cnt — cntlrs a pool has (0 = the gateway "+
			"default, %d)", common.DefaultCntlrCntPerSp))
	flags.Uint32("slice-cnt", 0,
		fmt.Sprintf("slice_cnt — slices a pool has (0 = the gateway "+
			"default, %d)", common.DefaultSliceCntPerSp))
	return cmd
}
