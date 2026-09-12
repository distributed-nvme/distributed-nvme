// The disk node subcommands of dnvctl (dnvctl.md §5.2): the six RPCs that
// manage a DN inside one cluster. Every one of them takes cluster_name from
// the global --cluster (§5.0) and names its node with --addr, because a DN's
// name IS its agent's ip:port — there is no separate id to type.
//
// Two of the six are token-carrying mutators (§4): `dn delete` and
// `dn set-disabled` send dn_rev, built by root.go's dnRev from the global
// --rev. dnvctl sends exactly what was typed and substitutes nothing (CT8):
// an omitted --rev leaves the field NIL, while `--rev 0` sends a PRESENT
// message with revision 0. §4 keeps those two distinguishable because the
// gateway's GW6 check keys on presence: a nil token skips the check entirely,
// while a present token — 0 included — is compared and refused ABORTED
// "stale revision" on mismatch (stored revisions seed at 1, so 0 is the
// always-stale probe; risks_and_gaps.md RK8 records the trade). Either way
// the wire content is the operator's to choose.
//
// `dn get` is the token source an operator reads before either mutator, and
// it reads etcd. `dn inspect` is the group's only live read: the gateway
// answers it from the DN agent's GetDnInfo instead. `dn create` also reaches
// the agent — for GetDnSize — which is why a create can fail on a node that
// is merely unreachable.

package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerDn adds the §5.2 group to the root command.
func registerDn(root *cobra.Command) {
	root.AddCommand(group("dn", "disk nodes",
		dnCreateCmd(),
		dnDeleteCmd(),
		dnGetCmd(),
		dnListCmd(),
		dnSetDisabledCmd(),
		dnInspectCmd(),
	))
}

// dnAddrFlag declares the identity flag of five of the six commands. It is
// named with this file's group prefix so it cannot collide with the identical
// helper cn.go needs for its own mirror group (§1.2).
//
// There is no default: an omitted --addr sends an empty addr_port on purpose
// and the gateway answers INVALID_ARGUMENT (CT8 — dnvctl checks nothing).
func dnAddrFlag(cmd *cobra.Command) {
	cmd.Flags().String("addr", "", "disk node addr_port as ip:port")
}

// dnCreateCmd drives CreateDiskNode. The four transport flags come from the
// shared trConfFlags in its UNPREFIXED form (§5.0), so they read --tr-type,
// --adr-fam, --tr-addr and --tr-svc-id and default to the lab-shaped
// tcp/ipv4/127.0.0.1/4420. Emptying all four sends a nil nvme_tr_conf, which
// the gateway then refuses ("nvme_tr_conf must not be empty"); dnvctl forwards
// it anyway, because CT8 leaves that judgement to the gateway.
//
// --location defaults to empty rather than to a rack name because the gateway
// then defaults it to addr_port itself, so a create that names only --addr
// still stores a well-formed DnConf.
func dnCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a disk node (CreateDiskNode)",
		func() (job, error) {
			req := &pb.CreateDiskNodeRequest{
				ClusterName: clusterOf(),
				AddrPort:    strOf("addr"),
				NvmeTrConf:  trConfOf(""),
				Location:    strOf("location"),
				Disabled:    boolOf("disabled"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateDiskNode(ctx, req)
			}, nil
		})
	dnAddrFlag(cmd)
	flags := cmd.Flags()
	flags.String("location", "",
		"failure domain; empty lets the gateway default it to addr_port")
	flags.Bool("disabled", false,
		"create the node already disabled (no capacity key)")
	trConfFlags(flags, "")
	return cmd
}

// dnDeleteCmd drives DeleteDiskNode. The DnRev message travels with only its
// revision set: the gateway matches on `revision` alone and never reads the
// token's echo fields back out.
func dnDeleteCmd() *cobra.Command {
	cmd := leaf("delete", "delete a disk node (DeleteDiskNode)",
		func() (job, error) {
			rev, err := dnRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteDiskNodeRequest{
				ClusterName: clusterOf(),
				AddrPort:    strOf("addr"),
				DnRev:       rev,
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteDiskNode(ctx, req)
			}, nil
		})
	dnAddrFlag(cmd)
	return cmd
}

// dnGetCmd drives GetDiskNode. Its reply carries the DnConf and the current
// dn_rev, which is where an operator reads the token to feed back into
// `dn delete` or `dn set-disabled` (§4).
func dnGetCmd() *cobra.Command {
	cmd := leaf("get", "read a disk node's conf and dn_rev (GetDiskNode)",
		func() (job, error) {
			req := &pb.GetDiskNodeRequest{
				ClusterName: clusterOf(),
				AddrPort:    strOf("addr"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetDiskNode(ctx, req)
			}, nil
		})
	dnAddrFlag(cmd)
	return cmd
}

// dnListCmd drives ListDiskNodes. Unlike `cluster list` this one IS cluster
// scoped, so it fills cluster_name from the global alongside the page flags.
// Count is narrowed to the wire's uint32 the same way `cluster list` does it.
func dnListCmd() *cobra.Command {
	cmd := leaf("list", "list disk node addr_ports (ListDiskNodes)",
		func() (job, error) {
			req := &pb.ListDiskNodesRequest{
				ClusterName: clusterOf(),
				Count:       u32Of("count"),
				PageToken:   strOf("page-token"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ListDiskNodes(ctx, req)
			}, nil
		})
	pageFlags(cmd.Flags())
	return cmd
}

// dnSetDisabledCmd drives UpdateDiskNodeDisabled. --disabled is an explicit
// value rather than a toggle, so re-sending the state the DN already has
// exercises the gateway's idempotency rule (no write, no revision bump, OK)
// instead of flipping it back — which also makes the command safe to script.
// It defaults to false, so re-enabling a node is the bare `--addr` form and
// disabling it is `--disabled`.
func dnSetDisabledCmd() *cobra.Command {
	cmd := leaf("set-disabled",
		"set a disk node's disabled flag (UpdateDiskNodeDisabled)",
		func() (job, error) {
			rev, err := dnRev()
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateDiskNodeDisabledRequest{
				ClusterName: clusterOf(),
				AddrPort:    strOf("addr"),
				DnRev:       rev,
				Disabled:    boolOf("disabled"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.UpdateDiskNodeDisabled(ctx, req)
			}, nil
		})
	dnAddrFlag(cmd)
	cmd.Flags().Bool("disabled", false, "the disabled flag to store")
	return cmd
}

// dnInspectCmd drives InspectDiskNode, which the gateway answers by calling
// the DN agent's GetDnInfo — a live read of the node, not of etcd, whose reply
// pairs the DnInfo with the revision the agent has actually applied.
func dnInspectCmd() *cobra.Command {
	cmd := leaf("inspect", "read a disk node live from its agent "+
		"(InspectDiskNode)",
		func() (job, error) {
			req := &pb.InspectDiskNodeRequest{
				ClusterName: clusterOf(),
				AddrPort:    strOf("addr"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.InspectDiskNode(ctx, req)
			}, nil
		})
	dnAddrFlag(cmd)
	return cmd
}
