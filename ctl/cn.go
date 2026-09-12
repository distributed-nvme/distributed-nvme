// cn.go is the `cn` group (dnvctl.md §5.3): the six ControllerNode RPCs.
//
// §5.3 defines the group as "exact `dn` mirrors ... same flags", and that is
// literal: the DN and CN message families differ on the wire only in their
// names and in which revision token they carry (CnRev here, DnRev there), so
// every verb, every flag spelling and every default matches ctl/dn.go row for
// row. A change to one is a change to both.
//
// The only package-level name this file adds beyond registerCn is cnAddrFlag,
// which five of the six leaves need. It carries the group prefix on purpose:
// dn.go declares the identical --addr flag for its own leaves and the two
// files share one package, so an unprefixed nodeAddrFlag would collide the
// moment both land (§1.2, the gatewayctl lesson).
package ctl

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// cnAddrFlag declares the group's identity flag: a controller node's name IS
// its ip:port (§5.0), so `addr_port` is what every CN request is keyed on.
//
// It has no default, and an omitted --addr therefore sends an EMPTY
// addr_port rather than being rejected here — CT8 leaves "required field is
// empty" to the gateway, which is the only validator.
func cnAddrFlag(flags *pflag.FlagSet) {
	flags.String("addr", "", "controller node addr_port as ip:port")
}

// registerCn builds the `cn` group and hangs it off the root (§5.3).
func registerCn(root *cobra.Command) {
	// cn create — CreateControllerNode. The gateway answers this one by
	// calling the CN agent's GetCnSize inline, so the invocation's trace id
	// chains through the gateway into the agent's log.
	//
	// --location defaults to empty rather than to a rack name because the
	// gateway then defaults it to addr_port itself; the four transport flags
	// default through trConfFlags to tcp/ipv4/127.0.0.1/4420 (§5.0).
	create := leaf("create", "create a controller node",
		func() (job, error) {
			cluster := clusterOf()
			addr := strOf("addr")
			trConf := trConfOf("")
			location := strOf("location")
			disabled := boolOf("disabled")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateControllerNode(
					ctx, &pb.CreateControllerNodeRequest{
						ClusterName: cluster,
						AddrPort:    addr,
						NvmeTrConf:  trConf,
						Location:    location,
						Disabled:    disabled,
					})
			}, nil
		})
	createFlags := create.Flags()
	cnAddrFlag(createFlags)
	createFlags.String("location", "",
		"failure domain; empty lets the gateway default it to addr_port")
	createFlags.Bool("disabled", false,
		"create the node already disabled (no capacity key)")
	trConfFlags(createFlags, "")

	// cn delete — DeleteControllerNode, the first of the group's two token
	// carriers. cnRev() is nil unless --rev was typed, and that nil must
	// reach the field as nil: GW6 is presence-based, so an absent token
	// means "skip the check" and a present one means strict equality (§4).
	del := leaf("delete", "delete a controller node",
		func() (job, error) {
			rev, err := cnRev()
			if err != nil {
				return nil, err
			}
			cluster := clusterOf()
			addr := strOf("addr")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.DeleteControllerNode(
					ctx, &pb.DeleteControllerNodeRequest{
						ClusterName: cluster,
						AddrPort:    addr,
						CnRev:       rev,
					})
			}, nil
		})
	cnAddrFlag(del.Flags())

	// cn get — GetControllerNode. This is the CnRev token source (§4): its
	// reply carries the CnConf and the current cn_rev, which is where an
	// operator reads the number to pass back as --rev.
	get := leaf("get", "read a controller node's conf and cn_rev token",
		func() (job, error) {
			cluster := clusterOf()
			addr := strOf("addr")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetControllerNode(
					ctx, &pb.GetControllerNodeRequest{
						ClusterName: cluster,
						AddrPort:    addr,
					})
			}, nil
		})
	cnAddrFlag(get.Flags())

	// cn list — ListControllerNodes. The request's `count` is a uint32 and
	// pageFlags declares --count as a Uint32 flag, so an out-of-range value
	// dies at parse time and u32Of's read is lossless; 0 still means "the
	// server's default page size", which dnvctl does not substitute for (CT8).
	list := leaf("list", "list controller node addresses",
		func() (job, error) {
			cluster := clusterOf()
			count := u32Of("count")
			pageToken := strOf("page-token")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ListControllerNodes(
					ctx, &pb.ListControllerNodesRequest{
						ClusterName: cluster,
						Count:       count,
						PageToken:   pageToken,
					})
			}, nil
		})
	pageFlags(list.Flags())

	// cn set-disabled — UpdateControllerNodeDisabled. --disabled carries an
	// explicit value rather than toggling the stored one, so re-sending the
	// state the node already has is the gateway's idempotent no-op (no write,
	// no revision bump, OK) instead of flipping it back (§5.2).
	setDisabled := leaf("set-disabled",
		"set a controller node's disabled flag",
		func() (job, error) {
			rev, err := cnRev()
			if err != nil {
				return nil, err
			}
			cluster := clusterOf()
			addr := strOf("addr")
			disabled := boolOf("disabled")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.UpdateControllerNodeDisabled(
					ctx, &pb.UpdateControllerNodeDisabledRequest{
						ClusterName: cluster,
						AddrPort:    addr,
						CnRev:       rev,
						Disabled:    disabled,
					})
			}, nil
		})
	setDisabledFlags := setDisabled.Flags()
	cnAddrFlag(setDisabledFlags)
	setDisabledFlags.Bool("disabled", false,
		"the disabled flag to store; write --disabled=false to re-enable")

	// cn inspect — InspectControllerNode. The gateway answers it from the CN
	// agent's live GetCnInfo, not from etcd, which is why it can fail with
	// the agent's own error while `cn get` on the same node succeeds.
	inspect := leaf("inspect",
		"read a controller node's live CnInfo from its agent",
		func() (job, error) {
			cluster := clusterOf()
			addr := strOf("addr")
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.InspectControllerNode(
					ctx, &pb.InspectControllerNodeRequest{
						ClusterName: cluster,
						AddrPort:    addr,
					})
			}, nil
		})
	cnAddrFlag(inspect.Flags())

	root.AddCommand(group("cn", "controller node commands",
		create, del, get, list, setDisabled, inspect))
}
