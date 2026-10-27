// group.go is dnvctl.md, `group` — `ctl/group.go`: the two RPCs of a raid1
// group itself, ForceStartGroup, the forced start of the group's array, and
// UpdateGroupOperatorInCharge, the operator's take-over of the group's repair
// from the sp worker (architecture.md, Repairs).
//
// ForceStartGroup writes a mark on the group record that names one leg, and
// the group's primary starts the array from that leg alone while the group's
// array does not run (a running array makes the mark wait): it stops an array
// that exists but does not run, zeroes the md superblock of every other leg
// the forced start keeps (the legs of its leg list, and the new leg of its
// open repair unless that repair is CREATED with its abort flag set), that
// repair's bad leg excepted, and md copies each of them whole from the named
// leg (architecture.md, Repair, Forced start). The mark is a field of the
// group record, so `sp get` shows a slice group's and `clone get` a clone meta
// group's (CT4); the sp worker clears it once the primary reports the array
// running with a member in sync on a leg whose own row reads OK
// (dnv-worker.md AR13). A start from an older copy restores the group alone,
// not the pool, whose other groups keep their newer state
// (architecture.md, Known limits).
//
// UpdateGroupOperatorInCharge sets or clears the group's operator_in_charge
// flag. While it is set the sp worker creates no repair of the group, so the
// operator can abort and cancel the worker's repair and run their own while
// the pool's IO goes on (architecture.md, Repairs). The flag is a field of
// the group record too, so `sp get` and `clone get` show it as they show the
// mark (CT4).
//
// The identity flag is `--grp`, the group's grp_id, and the leg is `--leg`,
// both read through hexOf like every id (CT8). `group force-start` keeps its
// RPC's verb, and `group set-operator-in-charge` is the `set-` leaf of an
// update RPC (dnvctl.md, Conventions).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerGroup adds the `group` noun to the root. Both leaves carry an
// `sp_rev` token when --rev is given (CT3).
func registerGroup(root *cobra.Command) {
	root.AddCommand(group(
		"group",
		"groups — the groups of legs a pool's slices and clones hold "+
			"(architecture.md, Repairs)",
		groupForceStartCmd(),
		groupSetOperatorInChargeCmd(),
	))
}

// groupForceStartCmd is ForceStartGroup.
//
// --grp is the group, which the gateway looks up among the slices and then
// the clones, as it does a repair's, so the leaf takes no parent. --leg is
// the leg the array starts from: a leg of the group's leg list other than
// the bad leg of a started repair, or the new leg of its open repair while
// that repair is started or is created with its abort flag clear, as the
// gateway's refusal says. --cancel withdraws the group's mark instead of
// writing one. dnvctl checks neither pairing: a --cancel with a non-zero
// --leg, and a write with no --leg, are forwarded, and the gateway refuses
// them (CT8).
//
// The forced start destroys what every other leg the forced start keeps
// holds, an open repair's bad leg excepted, and dnvctl asks for no
// confirmation: the leaf's name and its help say so, as a delete leaf's do,
// and the help names both kinds of leg it overwrites and that a start from
// an older copy restores the group alone.
func groupForceStartCmd() *cobra.Command {
	cmd := leaf(
		"force-start",
		"ForceStartGroup: start a group's array from one leg alone; every "+
			"other leg the forced start keeps (a leg of its leg list, or the "+
			"new leg of its open repair unless that repair is created with "+
			"its abort flag set), the open repair's bad leg excepted, loses "+
			"its md superblock and is overwritten by a full copy; a start "+
			"from an older copy restores this group alone, not the pool",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			grpId, err := hexOf("grp")
			if err != nil {
				return nil, err
			}
			legId, err := hexOf("leg")
			if err != nil {
				return nil, err
			}
			req := &pb.ForceStartGroupRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				GrpId:       grpId,
				LegId:       legId,
				Cancel:      boolOf("cancel"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.ForceStartGroup(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("grp", "", "grp_id of the group started (base 0)")
	flags.String("leg", "",
		"leg_id of the leg the array starts from alone (base 0; none with "+
			"--cancel)")
	flags.Bool("cancel", false,
		"withdraw the group's forced-start mark instead of writing one")
	return cmd
}

// groupSetOperatorInChargeCmd is UpdateGroupOperatorInCharge.
//
// --grp is the group, looked up as the forced start's is, so the leaf takes
// no parent. --operator-in-charge defaults to TRUE (dnvctl.md, `group` —
// `ctl/group.go`): taking the group over is the deliberate act and giving it
// back is the undo, the reverse of `cntlr set-enabled`, whose default true is
// the undo. Giving the group back is written --operator-in-charge=false;
// --operator-in-charge false would leave `false` as a positional argument,
// which cobra.NoArgs rejects, so the trap fails loudly rather than setting
// the flag (dnvctl.md, Conventions).
//
// The leaf sends the value given, never a toggle: re-sending the stored value
// is the gateway's no-write. dnvctl checks nothing (CT8): a RedundNone pool,
// an unknown group or a clone being deleted reach the gateway, which refuses
// them.
func groupSetOperatorInChargeCmd() *cobra.Command {
	cmd := leaf(
		"set-operator-in-charge",
		"UpdateGroupOperatorInCharge: set or clear a group's "+
			"operator-in-charge flag; while it is set the sp worker creates "+
			"no repair of the group, and failover and every other reaction "+
			"go on",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			grpId, err := hexOf("grp")
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateGroupOperatorInChargeRequest{
				ClusterName:      clusterOf(),
				SpName:           spOf(),
				SpRev:            rev,
				GrpId:            grpId,
				OperatorInCharge: boolOf("operator-in-charge"),
			}
			return func(
				ctx context.Context, client pb.GatewayClient,
			) (any, error) {
				return client.UpdateGroupOperatorInCharge(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("grp", "",
		"grp_id of the group taken over or given back (base 0)")
	flags.Bool("operator-in-charge", true,
		"the new operator_in_charge flag; write --operator-in-charge=false "+
			"to give the group back to the sp worker")
	return cmd
}
