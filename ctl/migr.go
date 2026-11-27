// migr.go is dnvctl.md, `migr` — `ctl/migr.go`: the four RPCs of a
// migration, the side → side move of one leg's data inside an SP
// (architecture.md, Migrations; architecture.md, Migration). The gateway
// gives the leg a second `Side` on another DN, a dm-clone hydrates it from
// the first, the worker reads and pushes the leg's bitmap itself, and the
// operator ends the migration by committing (finish) or rolling back
// (cancel).
package ctl

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerMigr adds the `migr` group to the root. Identity is the
// group-uniform `--name`; `sp_name`/`cluster_name` come from the global
// flags on all five.
func registerMigr(root *cobra.Command) {
	root.AddCommand(group(
		"migr",
		"migrations — move one leg's side to another disk node "+
			"(architecture.md, Migrations)",
		migrCreateCmd(),
		migrFinishCmd(),
		migrCancelCmd(),
		migrGetCmd(),
	))
}

// migrCreateCmd is CreateMigration.
//
// --src-side names the side being moved AWAY from; the destination side is
// allocated by the gateway, so the only placement input is the optional
// --dn-black / --dn-white selector (nil when both are empty). The hydration
// knobs are the same pair CreateClone takes, because the destination side is
// driven by a dm-clone as well, and both zero means "not given": the gateway
// stores the message as it arrived, and the sp-worker fills a migration's
// zeros in with the constants of architecture.md, Common validation, as it
// builds the side request (dnvctl.md, Conventions; GW11;
// dnv-worker.md RW15).
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
		"src_side_id, the side being moved away from (decimal, or "+
			"hex after 0x)")
	selectorFlags(flags, "dn")
	dmCloneConfFlags(flags)
	return cmd
}

// migrFinishCmd is FinishMigration, the commit: the destination side becomes
// the leg's only side and the source is released.
//
// --force mirrors DeleteClone's. Without it the gateway must first prove, with
// a `GetSideInfo` of the DESTINATION side, the agent's latest observation of
// it, that every dm-clone region is hydrated, and an incomplete copy is
// FAILED_PRECONDITION; with it the operator takes that risk deliberately.
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
// dropped and the source stays the leg's side.
//
// --force is the only way to cancel once the cutover has started: the
// gateway refuses it otherwise, since the destination may hold the only copy
// of the writes since, and with it the source serves its content as of the
// cutover. The cancel marks the leg, and in an md-raid1 group of more than
// one leg the primary copies it whole from a leg in sync; the reply says so
// (copy_again), and why the writes since the cutover
// are lost on the leg where they are (loss). The answer waits until the
// primary uses the old side's path alone, for up to
// common.MigrCancelWaitSeconds, so the call needs a --timeout that covers
// that wait (architecture.md, Migrations, The forced cancel).
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
				Force:       boolOf("force"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.CancelMigration(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "migr_name")
	flags.Bool("force", false,
		fmt.Sprintf("cancel after the cutover started: the source serves "+
			"its content as of the cutover; in a RAID1 group the primary "+
			"then copies the leg again from a leg in sync (copy_again), "+
			"and the reply's loss says where the writes since the cutover "+
			"are lost; the answer waits up to %d s for the primary to use "+
			"the old side alone, so give a --timeout above that",
			common.MigrCancelWaitSeconds))
	return cmd
}

// migrGetCmd is GetMigration, a pure STM read: no token, so a typed --rev is
// a usage error here (CT3).
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
