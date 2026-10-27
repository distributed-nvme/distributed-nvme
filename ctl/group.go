// group.go is dnvctl.md, `group` — `ctl/group.go`: the RPC of a raid1 group
// itself, ForceStartGroup, the forced start of the group's array
// (architecture.md, Repairs). The call writes a mark on the group record that
// names one leg, and the group's primary starts the array from that leg
// alone while the group's array does not run (a running array makes the mark
// wait): it stops an array that exists but does not run, zeroes the md
// superblock of every other leg the forced start keeps (the legs of its leg
// list, and the new leg of its open repair unless that repair is CREATED
// with its abort flag set), that repair's bad leg excepted, and md copies
// each of them whole from the named leg (architecture.md, Repair, Forced
// start). The mark is a field of the group record, so `sp get` shows a slice
// group's and `clone get` a clone meta group's (CT4);
// the sp worker clears it once the primary reports the array running with a
// member in sync on a leg whose own row reads OK (dnv-worker.md AR13). A
// start from an older copy restores the group alone, not the pool, whose
// other groups keep their newer state (architecture.md, Known limits).
//
// The identity flag is `--grp`, the group's grp_id, and the leg is `--leg`,
// both read through hexOf like every id (CT8). The leaf keeps its RPC's verb
// (dnvctl.md, Conventions).
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerGroup adds the `group` noun to the root. Its leaf carries an
// `sp_rev` token when --rev is given (CT3).
func registerGroup(root *cobra.Command) {
	root.AddCommand(group(
		"group",
		"groups — the groups of legs a pool's slices and clones hold "+
			"(architecture.md, Repairs)",
		groupForceStartCmd(),
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
