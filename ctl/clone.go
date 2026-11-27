// clone.go is the `clone` group (dnvctl.md, `clone` — `ctl/clone.go`): the
// five RPCs of the destination half of a cross-SP copy (architecture.md,
// Clones). A clone names one thin device of THIS SP as the destination of a
// dm-clone whose source is a namespace somewhere else, which is why
// `clone create` carries a whole `src-` family of flags describing a thing
// this SP cannot see.
//
// Four of the five are SP-scoped mutators and take the CT3 sp_rev token;
// `clone get` is a pure read and takes none.
package ctl

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// registerClone installs `dnvctl clone …`. Five clone RPCs, five leaves —
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
// carry it, CreateClone and UpdateCloneTrConf. It is prefixed with the group
// name so it cannot collide with a sibling file's helper (dnvctl.md, Files).
//
// --src-tr-addr and --src-tr-svc-id are comma-separated lists, so a clone
// can name every cntlr of its source pool and read on after the pool's
// failover: one entry per item of the longer list, entry i taking the i-th
// address and the i-th port; a list of one item goes to every entry, so one
// port serves every address, and a longer list shorter than the other leaves
// the missing items empty, which the gateway refuses, as dnvctl invents no
// value the operator did not type; --src-tr-type and --src-adr-fam go to
// every entry. Nothing is refused here (CT8): every parsed value is sent.
//
// The list is EMPTY — never nil — when all four parts were given as empty
// strings. That distinction is the point: the gateway's "src_tr_conf must
// not be empty" refusal (gateway/validate.go's validateTrConfList) has to
// stay reachable from the CLI, which it would not be if dnvctl substituted a
// default transport for the operator's deliberate blank.
func cloneSrcTrConfList() []*pb.NvmeTrConf {
	if trConfOf("src-") == nil {
		return []*pb.NvmeTrConf{}
	}
	addrs := strings.Split(strOf("src-tr-addr"), ",")
	svcIds := strings.Split(strOf("src-tr-svc-id"), ",")
	at := func(list []string, i int) string {
		switch {
		case len(list) == 1:
			return list[0]
		case i < len(list):
			return list[i]
		}
		return ""
	}
	out := make([]*pb.NvmeTrConf, 0, max(len(addrs), len(svcIds)))
	for i := range max(len(addrs), len(svcIds)) {
		out = append(out, &pb.NvmeTrConf{
			TrType:  strOf("src-tr-type"),
			AdrFam:  strOf("src-adr-fam"),
			TrAddr:  at(addrs, i),
			TrSvcId: at(svcIds, i),
		})
	}
	return out
}

// cloneSrcTrFlags adds the four `src-` transport flags, whose address and
// port take comma-separated lists (cloneSrcTrConfList).
func cloneSrcTrFlags(flags *pflag.FlagSet) {
	trConfFlags(flags, "src-")
	flags.Lookup("src-tr-addr").Usage = "comma-separated NvmeTrConf.tr_addr " +
		"list, paired with the ports; one src_tr_conf per item of the " +
		"longer list"
	flags.Lookup("src-tr-svc-id").Usage = "comma-separated " +
		"NvmeTrConf.tr_svc_id list, paired with the addresses; one port " +
		"serves every address"
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
// --auto-resume makes the destination's namespaces serve through the dm-clone
// while the copy runs, from the moment the clone is built, even while their
// stored suspended flag is set or an auto_suspend transfer names them
// (cnagent.md CN16). Once the primary reports the copy whole the worker
// latches the clone (dnv-worker.md CLD13), which clears the stored flag with
// or without --auto-resume, as DeleteClone's latch does.
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
	cloneSrcTrFlags(flags)
	flags.Bool("auto-resume", false,
		"auto_resume — let the destination's namespaces serve through the "+
			"dm-clone while the copy runs, even while stored suspended "+
			"or named by an auto_suspend transfer")
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

// cloneGetCmd is GetClone. A pure read, so it carries no token (CT3), and a
// typed --rev is a usage error here.
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
	cloneSrcTrFlags(flags)
	return cmd
}

// cloneAppendBmCmd is AppendCloneBitmap: it appends one opaque chunk of the
// source's allocation bitmap, which the primary uses to skip regions the
// source never allocated.
//
// A chunk is addressed by the PAIR --src-slice-idx / --bm-idx, never by either
// alone, or by --src-slice-idx and --src-start-block, the first block of the
// slice the page covers, which `td get-bm` prints and from which the gateway
// works out the chunk and the offset: chunk (s, b) holds bytes [b*C, b*C+len)
// of source slice s's bitmap, C = common.CloneBmChunkBytes. Chunks are self-positioned, so an operator may
// send them in any order and leave whole chunks unsent; only the PAGES of ONE
// chunk must arrive in slice-bitmap order, because the gateway appends each
// page at that chunk's current length. --offset names the page's byte offset
// in the chunk and is sent 0 when it is not typed, unless --src-start-block
// is, so a page sent again after a lost answer changes nothing and a page at a wrong offset —
// a later page sent with no --offset among them — is refused by the gateway,
// or taken with no write when its bytes equal those at that offset, never
// appended twice (architecture.md, Clones). dnvctl checks nothing itself
// (CT8): --src-start-block beside --bm-idx or --offset is sent as typed, and
// the gateway refuses it beside a non-zero --bm-idx or any --offset. The Clone record carries no chunk count: how many chunks a
// clone holds is how many chunk keys it has.
//
// --bm-hex splits the two failure kinds dnvctl.md, `clone` — `ctl/clone.go`,
// insists on: an EMPTY value sends an empty bitmap on purpose, so the
// gateway's "bitmap must not be empty" refusal stays reachable, while a
// malformed non-empty value never leaves the process and is a usage error
// (exit 2, CT8). hexBytesOf is exactly that rule, so this command only has
// to let its error out.
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
				SrcSliceIdx: u32Of("src-slice-idx"),
				BmIdx:       u32Of("bm-idx"),
				Bitmap:      bitmap,
				Offset:      proto.Uint32(u32Of("offset")),
			}
			// The first block `td get-bm` printed: the gateway then works
			// out the chunk and the offset itself. The offset is then sent
			// only when typed, and --bm-idx as typed, so that the gateway
			// refuses a non-zero --bm-idx or any offset beside the start
			// block (CT8).
			if invoked.Changed("src-start-block") {
				req.SrcStartBlock = proto.Uint64(u64Of("src-start-block"))
				if !invoked.Changed("offset") {
					req.Offset = nil
				}
			}
			return func(ctx context.Context, client pb.GatewayClient) (
				any, error,
			) {
				return client.AppendCloneBitmap(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("name", "", "clone_name")
	flags.Uint32("src-slice-idx", 0, "src_slice_idx the chunk describes")
	flags.Uint32("bm-idx", 0, "bm_idx, the chunk's index within the slice")
	flags.String("bm-hex", "",
		"bitmap as hex; empty sends an empty bitmap on purpose")
	flags.Uint32("offset", 0,
		"the page's byte offset in the chunk; 0, the chunk's start, when "+
			"not given")
	flags.Uint64("src-start-block", 0,
		"src_start_block, the first block of the source slice the page "+
			"covers, as td get-bm prints it in start_block; the gateway "+
			"then works out the chunk and the offset (no --bm-idx or "+
			"--offset with it)")
	return cmd
}
