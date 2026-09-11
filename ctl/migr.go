// migr.go is dnvctl.md §5.11: the five RPCs of a migration, the side → side
// move of one leg's data inside an SP (architecture.md §8.11, §11.2). The
// gateway gives the leg a second `Side` on another DN, a dm-clone hydrates it
// from the first, and the operator ends the migration by committing (finish)
// or rolling back (cancel).
//
// Two of the RPCs are deliberately NOT symmetric with their siblings, and §5
// is exhaustive about it (CT1):
//
//   - `migr cancel` has no --force. Throwing an unfinished copy away needs no
//     proof about the copy, so CancelMigrationRequest has no such field —
//     unlike FinishMigrationRequest, which does.
//   - `migr append-bm` has no slice index. A migration copies ONE side, so its
//     chunks are a single sequence numbered by the record's own `bm_cnt`;
//     AppendCloneBitmap's per-source-slice `slice_idx` has no counterpart here.
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerMigr adds the `migr` group to the root. Identity is the
// group-uniform `--name`; `sp_name`/`cluster_name` come from the globals on
// all five.
func registerMigr(root *cobra.Command) {
	root.AddCommand(group(
		"migr",
		"migrations — move one leg's side to another disk node (§8.11)",
		migrCreateCmd(),
		migrFinishCmd(),
		migrCancelCmd(),
		migrGetCmd(),
		migrAppendBmCmd(),
	))
}

// migrCreateCmd is CreateMigration.
//
// --src-side names the side being moved AWAY from; the destination side is
// allocated by the gateway, so the only placement input is the optional
// --dn-black / --dn-white selector (nil when both are empty). The hydration
// knobs are the same pair CreateClone takes, because the destination side is
// driven by a dm-clone as well, and both zero means "not given" so the gateway
// applies its own defaults (§5.0, GW11).
func migrCreateCmd() *cobra.Command {
	cmd := leaf(
		"create",
		"CreateMigration: start copying a leg's side onto a new disk node",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			srcSideId, err := hexOf("src-side")
			if err != nil {
				return nil, err
			}
			req := &pb.CreateMigrationRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				MigrName:    strOf("name"),
				SrcSideId:   srcSideId,
				DnSelector:  selectorOf("dn"),
				DmCloneConf: dmCloneConfOf(),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CreateMigration(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "migr_name")
	flags.String("src-side", "",
		"src_side_id, the side being moved away from (base 0)")
	selectorFlags(flags, "dn")
	dmCloneConfFlags(flags)
	return cmd
}

// migrFinishCmd is FinishMigration, the commit: the destination side becomes
// the leg's only side and the source is released.
//
// --force mirrors DeleteClone's. Without it the gateway must first prove, with
// a live `GetSideInfo` of the DESTINATION side, that every dm-clone region is
// hydrated, and an incomplete copy is FAILED_PRECONDITION; with it the
// operator takes that risk deliberately.
func migrFinishCmd() *cobra.Command {
	cmd := leaf(
		"finish",
		"FinishMigration: commit the copy and release the source side",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.FinishMigrationRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				MigrName:    strOf("name"),
				Force:       boolOf("force"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.FinishMigration(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "migr_name")
	flags.Bool("force", false,
		"skip the \"fully hydrated\" proof on the destination side")
	return cmd
}

// migrCancelCmd is CancelMigration, the rollback: the destination side is
// dropped and the source stays the leg's side. No --force — see the file
// header; the RPC has no such field and adding a flag for it would send
// nothing (CT1).
func migrCancelCmd() *cobra.Command {
	cmd := leaf(
		"cancel",
		"CancelMigration: roll back and keep the source side",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CancelMigrationRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				MigrName:    strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CancelMigration(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "migr_name")
	return cmd
}

// migrGetCmd is GetMigration, a pure STM read: no token, and the global --rev
// is ignored here (§4).
func migrGetCmd() *cobra.Command {
	cmd := leaf(
		"get",
		"GetMigration: read one migration record",
		func() (job, error) {
			req := &pb.GetMigrationRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				MigrName:    strOf("name"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.GetMigration(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "migr_name")
	return cmd
}

// migrAppendBmCmd is AppendMigrationBitmap: one more immutable chunk of the
// skip bitmap, stored at `bm_idx = bm_cnt`. The chunks concatenate in bm_idx
// order into one bitmap over the leg's DATA region, 1 = never written ⇒
// skippable, and the destination DN agent shifts by the leg's `meta_blocks`
// before blkdiscarding the fully-skippable dm-clone regions (§8.11, §11.4).
//
// An empty --bm-hex sends an EMPTY bitmap on purpose rather than being
// rejected here, so the gateway's own refusal is what the operator sees; a
// malformed non-empty value is the usage error (exit 2, §5.9's rule for the
// clone twin). hexBytesOf draws exactly that line.
func migrAppendBmCmd() *cobra.Command {
	cmd := leaf(
		"append-bm",
		"AppendMigrationBitmap: append one skip-bitmap chunk",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			bitmap, err := hexBytesOf("bm-hex")
			if err != nil {
				return nil, err
			}
			req := &pb.AppendMigrationBitmapRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				MigrName:    strOf("name"),
				Bitmap:      bitmap,
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.AppendMigrationBitmap(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "migr_name")
	flags.String("bm-hex", "",
		"bitmap as hex; empty sends an empty bitmap on purpose")
	return cmd
}
