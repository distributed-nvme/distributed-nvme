// This file is the `td` group: the thin devices of architecture.md §8.7 and
// the two bitmap reads of §8.13 (dnvctl.md §5.6, five RPCs).
//
// Every request here is SP-scoped, so cluster_name and sp_name come from the
// §2.1 globals --cluster and --sp and never from a local flag; what is left is
// only the flags that name the device and window the reads. The two mutators
// carry `sp_rev` from the global --rev, presence-based per §4; the three reads
// carry no token at all.
//
// `td get-leg-bm` sits in this group because §5.6 puts it here: a leg is not a
// thin device, but the command is the same measurement through the same CN
// path as `td get-bm`, and the two are the whole CLI's only deviation from
// CT4's "print the reply message" — they print hexBitmapResult instead,
// because protojson renders a bytes field as base64 (§3.1).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerTd adds the `td` group to the root. NewRootCmd calls this by name,
// so the name and signature are the contract (§1.2).
func registerTd(root *cobra.Command) {
	root.AddCommand(group(
		"td", "thin devices of a storage pool",
		tdCreateCmd(),
		tdDeleteCmd(),
		tdListCmd(),
		tdGetBmCmd(),
		tdGetLegBmCmd(),
	))
}

// tdCreateCmd is CreateThinDevice.
//
// --ori is what turns the call into a SNAPSHOT of an existing thin device of
// the same SP rather than a fresh one, and it is also the only case in which
// --size 0 means anything, because a snapshot inherits its origin's size.
// dnvctl does not enforce that pairing: CT8 leaves every cross-flag rule to
// the gateway, so a bare `--size 0` goes out as typed and is refused there.
func tdCreateCmd() *cobra.Command {
	cmd := leaf(
		"create", "create a thin device or snapshot (CreateThinDevice)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CreateThinDeviceRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				TdName:      strOf("name"),
				OriName:     strOf("ori"),
				Size:        u64Of("size"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.CreateThinDevice(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "td_name")
	flags.String("ori", "",
		"ori_name: snapshot this existing thin device of the same "+
			"storage pool instead of creating a fresh one")
	flags.Uint64("size", 0,
		"size in bytes; 0 is meaningful only together with --ori")
	return cmd
}

// tdDeleteCmd is DeleteThinDevice. The gateway refuses a device that still has
// a namespace or a snapshot on it; dnvctl knows none of that and asks.
func tdDeleteCmd() *cobra.Command {
	cmd := leaf(
		"delete", "delete a thin device (DeleteThinDevice)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteThinDeviceRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				TdName:      strOf("name"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.DeleteThinDevice(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "td_name")
	return cmd
}

// tdListCmd is ListThinDevices — the `created` poll of ThinDeviceCreated.md
// R13. The gateway always writes `created` false and only the sp-worker flips
// it, so this command is how an operator learns a device is real enough to be
// snapshotted; §3.1's EmitUnpopulated is what keeps the false visible in the
// JSON rather than eliding it as a proto3 default.
//
// The request is nothing but the two scope globals: ListThinDevices is one of
// the List* RPCs with no pagination, so there are no page flags either (§5.6
// lists none).
func tdListCmd() *cobra.Command {
	return leaf(
		"list", "list the thin devices of a pool (ListThinDevices)",
		func() (job, error) {
			req := &pb.ListThinDevicesRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.ListThinDevices(ctx, req)
			}, nil
		})
}

// tdGetBmCmd is GetThinDeviceBitmap: the allocation bitmap of one slice of one
// thin device, measured live on the primary cntlr's CN (architecture.md
// §8.13). Read-only, hence no token.
//
// --start/--cnt are the window in blocks; both default to 0, which asks the CP
// for the whole slice — dnvctl substitutes no window of its own (CT8). The
// identity flag is --name, not gatewayctl's --td: §5.0 makes the identity flag
// uniform across a group, and within this command there is only one name to
// mean.
//
// The reply is reshaped into the §3.1 hex map instead of being emitted as a
// message, so the bitmap reads as hex rather than protojson's base64.
func tdGetBmCmd() *cobra.Command {
	cmd := leaf(
		"get-bm", "read a thin device's bitmap (GetThinDeviceBitmap)",
		func() (job, error) {
			req := &pb.GetThinDeviceBitmapRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				TdName:      strOf("name"),
				SliceIdx:    u32Of("slice-idx"),
				StartBlock:  u64Of("start"),
				BlockCnt:    u64Of("cnt"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				reply, err := client.GetThinDeviceBitmap(ctx, req)
				if err != nil {
					return nil, err
				}
				return hexBitmapResult(reply.GetBitmap()), nil
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "td_name")
	flags.Uint32("slice-idx", 0, "slice_idx within the thin device")
	flags.Uint64("start", 0, "start_block of the window (0 = from 0)")
	flags.Uint64("cnt", 0, "block_cnt of the window (0 = the whole slice)")
	return cmd
}

// tdGetLegBmCmd is GetLegBitmap: the same measurement for one leg of a raid1
// group. A leg has no name, so it is addressed by id — a base-0 hexOf value
// like every other id flag (§5.0) — and the request carries no slice_idx,
// because a leg is not sliced. Read-only, same window flags, same §3.1 hex map
// as `td get-bm`.
func tdGetLegBmCmd() *cobra.Command {
	cmd := leaf(
		"get-leg-bm", "read a raid1 leg's bitmap (GetLegBitmap)",
		func() (job, error) {
			legId, err := hexOf("leg")
			if err != nil {
				return nil, err
			}
			req := &pb.GetLegBitmapRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				LegId:       legId,
				StartBlock:  u64Of("start"),
				BlockCnt:    u64Of("cnt"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				reply, err := client.GetLegBitmap(ctx, req)
				if err != nil {
					return nil, err
				}
				return hexBitmapResult(reply.GetBitmap()), nil
			}, nil
		})
	flags := cmd.Flags()
	flags.String("leg", "", "leg_id (base 0)")
	flags.Uint64("start", 0, "start_block of the window (0 = from 0)")
	flags.Uint64("cnt", 0, "block_cnt of the window (0 = the whole leg)")
	return cmd
}
