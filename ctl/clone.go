// clone.go is the `clone` group (dnvctl.md §5.9): the five RPCs of the
// destination half of a cross-SP copy (architecture.md §8.9). A clone names one
// thin device of THIS SP as the destination of a dm-clone whose source is a
// namespace somewhere else, which is why `clone create` carries a whole `src-`
// family of flags describing a thing this SP cannot see.
//
// Four of the five are SP-scoped mutators and take the §4 sp_rev token;
// `clone get` is a pure read and takes none.
package ctl

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerClone installs `dnvctl clone …`. Five rows in §5.9, five leaves —
// CT1 allows no sixth.
func registerClone(root *cobra.Command) {
	root.AddCommand(group("clone", "destination side of a cross-SP copy",
		cloneCreateCmd(),
		cloneDeleteCmd(),
		cloneGetCmd(),
		cloneSetTrCmd(),
		cloneAppendBmCmd(),
	))
}

// cloneSrcTrConfList is the repeated `src_tr_conf` of the two commands that
// carry it, CreateClone and UpdateCloneTrConf. It lives here rather than in
// root.go because only this group needs it, and it is prefixed with the group
// name so it cannot collide with a sibling file's helper (§1.2, the gatewayctl
// lesson).
//
// The list is a one-element list of the `src-` transport when trConfOf returns
// a conf, and an EMPTY list — never nil — when all four parts were given as
// empty strings. That distinction is the point: the gateway's "src_tr_conf
// must not be empty" refusal (gateway/validate.go's validateTrConfList) has to
// stay reachable from the CLI, which it would not be if dnvctl substituted a
// default transport for the operator's deliberate blank.
func cloneSrcTrConfList() []*pb.NvmeTrConf {
	conf := trConfOf("src-")
	if conf == nil {
		return []*pb.NvmeTrConf{}
	}
	return []*pb.NvmeTrConf{conf}
}

// cloneCreateCmd is CreateClone.
//
// The three source-geometry flags stay separate because each describes a
// different thing: --src-slices is the SOURCE SP's slice count, --src-stripe
// its raid0 stripe and --src-block the dm-clone region size. The gateway's
// validateCloneGeometry bounds all three and then checks that the block is a
// multiple of the stripe; CT8 keeps dnvctl out of every bit of that — no
// bound, no cross-check, no default-filling — so a bad geometry is
// INVALID_ARGUMENT from the gateway and never a client-side refusal.
//
// --auto-resume asks the CP to resume the destination's namespaces itself once
// the copy completes, instead of the operator following up with
// `ns set-suspended --suspended=false`.
func cloneCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a clone (CreateClone)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.CreateCloneRequest{
				ClusterName:   clusterOf(),
				SpName:        spOf(),
				SpRev:         rev,
				CloneName:     strOf("name"),
				SrcTrConf:     cloneSrcTrConfList(),
				SrcNqn:        strOf("src-nqn"),
				SrcNsIdx:      u32Of("src-idx"),
				SrcSliceCnt:   u32Of("src-slices"),
				SrcStripeSize: u64Of("src-stripe"),
				SrcBlockSize:  u64Of("src-block"),
				DstTdName:     strOf("dst-td"),
				DmCloneConf:   dmCloneConfOf(),
				AutoResume:    boolOf("auto-resume"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.CreateClone(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "clone_name")
	flags.String("dst-td", "", "dst_td_name, the destination thin device")
	flags.String("src-nqn", "", "src_nqn, the source subsystem")
	flags.Uint32("src-idx", 0, "src_ns_idx, the source ns_idx")
	flags.Uint32("src-slices", 0, "src_slice_cnt, the source SP's slice count")
	flags.Uint64("src-stripe", 0, "src_stripe_size in bytes")
	flags.Uint64("src-block", 0, "src_block_size in bytes")
	trConfFlags(flags, "src-")
	flags.Bool("auto-resume", false,
		"auto_resume — let the CP resume the destination's namespaces "+
			"when the copy completes")
	dmCloneConfFlags(flags)
	return cmd
}

// cloneDeleteCmd is DeleteClone.
//
// --force is the whole of the difference between the two paths: without it the
// gateway must PROVE from the primary cntlr's CN that the copy finished before
// it tears the dm-clone down, and an unreachable agent is a refusal rather
// than a pass; with it that proof is skipped, which is how a clone whose
// source has already gone away is abandoned.
func cloneDeleteCmd() *cobra.Command {
	cmd := leaf("delete", "delete a clone (DeleteClone)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteCloneRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				CloneName:   strOf("name"),
				Force:       boolOf("force"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.DeleteClone(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "clone_name")
	flags.Bool("force", false,
		"force — skip the \"copy finished\" proof on the primary's CN")
	return cmd
}

// cloneGetCmd is GetClone. A pure read, so it carries no token (§4) and
// declares no --rev of its own; the global flag is simply ignored here.
func cloneGetCmd() *cobra.Command {
	cmd := leaf("get", "read one clone (GetClone)",
		func() (job, error) {
			req := &pb.GetCloneRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				CloneName:   strOf("name"),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.GetClone(ctx, req)
			}, nil
		})
	cmd.Flags().String("name", "", "clone_name")
	return cmd
}

// cloneSetTrCmd is UpdateCloneTrConf, the RPC that re-points a stored clone at
// a source that moved. It takes the same `src-` prefixed transport flags
// `clone create` does, so the spelling that created a clone is the spelling
// that updates it, and the same all-empty set again sends an empty list.
func cloneSetTrCmd() *cobra.Command {
	cmd := leaf("set-tr", "re-point a clone at a moved source "+
		"(UpdateCloneTrConf)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateCloneTrConfRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				CloneName:   strOf("name"),
				SrcTrConf:   cloneSrcTrConfList(),
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.UpdateCloneTrConf(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "clone_name")
	trConfFlags(flags, "src-")
	return cmd
}

// cloneAppendBmCmd is AppendCloneBitmap: it appends one opaque chunk of the
// source's allocation bitmap, which the primary uses to skip regions the
// source never allocated.
//
// --slice-idx is the source SLICE the chunk describes, not the chunk's own
// index; the record's bm_cnt is what numbers the chunks.
//
// --bm-hex splits the two failure kinds §5.9 insists on: an EMPTY value sends
// an empty bitmap on purpose, so the gateway's "bitmap must not be empty"
// refusal stays reachable, while a malformed non-empty value never leaves the
// process and is a usage error (exit 2, §7.12 c6). hexBytesOf is exactly that
// rule, so this command only has to let its error out.
func cloneAppendBmCmd() *cobra.Command {
	cmd := leaf("append-bm", "append a source bitmap chunk "+
		"(AppendCloneBitmap)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			bitmap, err := hexBytesOf("bm-hex")
			if err != nil {
				return nil, err
			}
			req := &pb.AppendCloneBitmapRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				CloneName:   strOf("name"),
				SliceIdx:    u32Of("slice-idx"),
				Bitmap:      bitmap,
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.AppendCloneBitmap(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "clone_name")
	flags.Uint32("slice-idx", 0, "slice_idx the chunk describes")
	flags.String("bm-hex", "",
		"bitmap as hex; empty sends an empty bitmap on purpose")
	return cmd
}
