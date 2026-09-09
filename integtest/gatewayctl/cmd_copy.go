// The copy half of the §10.8 subcommand table: clones (architecture.md §8.9),
// migrations (§8.11), spare legs (§8.12) and the two bitmap reads (§8.13).
//
// Everything here is SP-scoped, so every mutator carries the SP's revision
// token as `--rev` and every reader carries none — the read RPCs take no token
// at all (gateway.md §5.5). `--rev 0` is a legal, deliberate value: a zero
// token is the "I did not read first" case the B4 stage sends on purpose, so
// no subcommand of this file second-guesses it.
//
// The two bitmap reads are the one place where a job returns something other
// than its reply message: protojson would print the `bitmap` bytes as base64,
// and §10.8 says bitmaps are printed as hex, so they return a plain map that
// resultToAny passes through untouched.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Shared flag shapes of this group
// ---------------------------------------------------------------------------

// dmCloneConfFlags registers the two dm-clone hydration knobs shared by
// CreateClone and CreateMigration and returns their accessor.
//
// Both knobs are proto3 "unset means default at use time" values (§7, GW11):
// the gateway's validateBound lets a zero through and resolves it later. An
// untouched pair therefore sends NO DmCloneConf at all rather than an empty
// one, the same "not given" convention trConfFlags uses, so the record the
// script then reads back through get-clone / get-migr holds exactly what the
// caller asked for and nothing the driver invented.
func dmCloneConfFlags(fs *flag.FlagSet) func() *pb.DmCloneConf {
	threshold := fs.Uint("hyd-threshold", 0,
		"dm_clone_conf.hydration_threshold (0 = let the CP default it)")
	batch := fs.Uint("hyd-batch", 0,
		"dm_clone_conf.hydration_batch_size (0 = let the CP default it)")
	return func() *pb.DmCloneConf {
		if *threshold == 0 && *batch == 0 {
			return nil
		}
		return &pb.DmCloneConf{
			HydrationThreshold: uint32(*threshold),
			HydrationBatchSize: uint32(*batch),
		}
	}
}

// dnSelectorFlags registers the NodeSelector of the two RPCs that allocate a
// fresh DN side — CreateMigration's destination and CreateSpareLeg's spare —
// and returns their accessor. Both lists are comma-separated addr:port lists
// (§6.3). An entirely empty selector is sent as nil, so "no preference" and
// "an empty black list" stay the same request the gateway's own callers make.
func dnSelectorFlags(fs *flag.FlagSet) func() *pb.NodeSelector {
	var black, white stringList
	fs.Var(&black, "dn-black",
		"dn_selector.black_list, comma-separated addr:port")
	fs.Var(&white, "dn-white",
		"dn_selector.white_list, comma-separated addr:port")
	return func() *pb.NodeSelector {
		if len(black) == 0 && len(white) == 0 {
			return nil
		}
		return &pb.NodeSelector{BlackList: black, WhiteList: white}
	}
}

// bitmapArg turns a --bm-hex value into the bytes the Append*Bitmap request
// carries.
//
// An empty value is deliberately NOT a driver error. parseHexBitmap refuses
// it, but "bitmap must not be empty" is one of the refusals the gateway owns
// and case C asserts, so an empty --bm-hex must travel to the gateway as an
// empty bitmap and come back as INVALID_ARGUMENT from there. Anything else
// that fails to decode is a mistake in the script itself and is a usage error.
func bitmapArg(spec string) []byte {
	bitmap, err := parseHexBitmap(spec)
	if err == nil {
		return bitmap
	}
	if strings.TrimPrefix(strings.TrimSpace(spec), "0x") != "" {
		usageDie("--bm-hex %q: %v", spec, err)
	}
	return nil
}

// hexBitmapResult reshapes a bitmap reply for the script (§10.8, "bitmaps
// printed as hex"). protojson renders a bytes field as base64, which is not
// what the fakes are seeded with nor what the assertions quote, so both reads
// return this plain map instead of their reply message; resultToAny prints a
// non-proto result as it is. byte_cnt travels with it so a length assertion
// needs no jq string arithmetic.
func hexBitmapResult(bitmap []byte) map[string]any {
	return map[string]any{
		"bitmap_hex": hex.EncodeToString(bitmap),
		"byte_cnt":   len(bitmap),
	}
}

// ---------------------------------------------------------------------------
// clone (architecture.md §8.9)
// ---------------------------------------------------------------------------

// setupCreateClone drives CreateClone: it names one thin device of this SP as
// the destination of a dm-clone whose source is a namespace somewhere else.
//
// The source geometry flags are separate because the gateway checks each of
// them on its own (validateCloneGeometry): --src-slices is the source SP's
// slice count, --src-stripe its raid0 stripe and --src-block the dm-clone
// region size, and case C walks every one of the three past its bound.
//
// The source transport is registered through trConfFlags under the `src-`
// prefix and becomes the single entry of the repeated src_tr_conf. When all
// four parts are given as empty strings the accessor returns nil and the
// request carries an EMPTY list, which is exactly what case C's validation
// battery needs to see refused ("src_tr_conf must not be empty").
func setupCreateClone(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	cloneName := fs.String("name", "", "clone_name")
	dstTd := fs.String("dst-td", "", "dst_td_name, the destination thin device")
	srcNqn := fs.String("src-nqn", "", "src_nqn")
	srcIdx := fs.Uint("src-idx", 0, "src_ns_idx")
	srcSlices := fs.Uint("src-slices", 0, "src_slice_cnt")
	srcStripe := fs.Uint64("src-stripe", 0, "src_stripe_size in bytes")
	srcBlock := fs.Uint64("src-block", 0, "src_block_size in bytes")
	srcTrConf := trConfFlags(fs, "src-", "tcp", "ipv4", "127.0.0.1", "4420")
	autoResume := fs.Bool("auto-resume", false,
		"auto_resume — let the CP resume the destination's namespaces "+
			"when the copy completes")
	dmCloneConf := dmCloneConfFlags(fs)
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		var trConfList []*pb.NvmeTrConf
		if conf := srcTrConf(); conf != nil {
			trConfList = append(trConfList, conf)
		}
		return client.CreateClone(ctx, &pb.CreateCloneRequest{
			ClusterName:   g.cluster,
			SpName:        *spName,
			SpRev:         &pb.SpRev{Revision: uint64(rev)},
			CloneName:     *cloneName,
			SrcTrConf:     trConfList,
			SrcNqn:        *srcNqn,
			SrcNsIdx:      uint32(*srcIdx),
			SrcSliceCnt:   uint32(*srcSlices),
			SrcStripeSize: *srcStripe,
			SrcBlockSize:  *srcBlock,
			DstTdName:     *dstTd,
			DmCloneConf:   dmCloneConf(),
			AutoResume:    *autoResume,
		})
	}
}

// setupDeleteClone drives DeleteClone, the two-phase RPC of AG4.
//
// --force is the whole point of the subcommand: without it the gateway must
// PROVE from the primary's CN that the copy finished before it removes the
// dm-clone, and an unreachable agent is a refusal rather than a pass; with it
// the proof is skipped, which is how §10.11 step 13 abandons a clone whose
// fake source has already been torn down.
func setupDeleteClone(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	cloneName := fs.String("name", "", "clone_name")
	force := fs.Bool("force", false,
		"force — skip the \"copy finished\" proof on the primary's CN")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteClone(ctx, &pb.DeleteCloneRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			CloneName:   *cloneName,
			Force:       *force,
		})
	}
}

// setupGetClone drives GetClone. It is a pure read, so it carries no token.
func setupGetClone(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	cloneName := fs.String("name", "", "clone_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.GetClone(ctx, &pb.GetCloneRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			CloneName:   *cloneName,
		})
	}
}

// setupSetCloneTr drives UpdateCloneTrConf, the RPC that re-points a stored
// clone at a moved source. The transport flags are the same `src-` prefixed
// set CreateClone registers, so the same spelling that created a clone
// updates it, and an all-empty set again sends an empty list for the refusal
// case C expects.
func setupSetCloneTr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	cloneName := fs.String("name", "", "clone_name")
	srcTrConf := trConfFlags(fs, "src-", "tcp", "ipv4", "127.0.0.1", "4420")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		var trConfList []*pb.NvmeTrConf
		if conf := srcTrConf(); conf != nil {
			trConfList = append(trConfList, conf)
		}
		return client.UpdateCloneTrConf(ctx, &pb.UpdateCloneTrConfRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			CloneName:   *cloneName,
			SrcTrConf:   trConfList,
		})
	}
}

// setupAppendCloneBm drives AppendCloneBitmap: it appends one opaque chunk of
// the source's allocation bitmap, which the primary uses to skip regions the
// source never allocated.
//
// --slice-idx is the source SLICE the chunk describes, not the chunk's own
// index: the record's bm_cnt is what numbers the chunks, and §10.11 step 13
// asserts it grows by one per call.
func setupAppendCloneBm(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	cloneName := fs.String("name", "", "clone_name")
	sliceIdx := fs.Uint("slice-idx", 0, "slice_idx the chunk describes")
	bmHex := fs.String("bm-hex", "",
		"bitmap as hex; empty sends an empty bitmap on purpose")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.AppendCloneBitmap(ctx, &pb.AppendCloneBitmapRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			CloneName:   *cloneName,
			SliceIdx:    uint32(*sliceIdx),
			Bitmap:      bitmapArg(*bmHex),
		})
	}
}

// ---------------------------------------------------------------------------
// migration (architecture.md §8.11)
// ---------------------------------------------------------------------------

// setupCreateMigr drives CreateMigration: it gives one leg a second side on a
// different DN and copies the first into it.
//
// --src-side names the side being moved AWAY from; the destination side is
// allocated by the gateway, which is why the only placement input is the
// optional --dn-black / --dn-white selector. The hydration knobs are the same
// pair CreateClone takes, because the destination side is driven by a
// dm-clone as well.
func setupCreateMigr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	migrName := fs.String("name", "", "migr_name")
	var srcSide hexUint
	fs.Var(&srcSide, "src-side", "src_side_id, the side being moved away from")
	dnSelector := dnSelectorFlags(fs)
	dmCloneConf := dmCloneConfFlags(fs)
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateMigration(ctx, &pb.CreateMigrationRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			MigrName:    *migrName,
			SrcSideId:   uint64(srcSide),
			DnSelector:  dnSelector(),
			DmCloneConf: dmCloneConf(),
		})
	}
}

// setupFinishMigr drives FinishMigration, the commit: the destination becomes
// the leg's only side and the source is released.
//
// --force mirrors DeleteClone's: without it the gateway must prove from the
// destination DN that every region is hydrated, and with it the operator
// takes that risk deliberately.
func setupFinishMigr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	migrName := fs.String("name", "", "migr_name")
	force := fs.Bool("force", false,
		"force — skip the \"fully hydrated\" proof on the destination DN")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.FinishMigration(ctx, &pb.FinishMigrationRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			MigrName:    *migrName,
			Force:       *force,
		})
	}
}

// setupCancelMigr drives CancelMigration, the abort: the destination side is
// dropped and the source stays the leg's side. It has no --force, because
// throwing the unfinished COPY away needs no proof about it.
func setupCancelMigr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	migrName := fs.String("name", "", "migr_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CancelMigration(ctx, &pb.CancelMigrationRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			MigrName:    *migrName,
		})
	}
}

// setupGetMigr drives GetMigration. A pure read, so no token.
func setupGetMigr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	migrName := fs.String("name", "", "migr_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.GetMigration(ctx, &pb.GetMigrationRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			MigrName:    *migrName,
		})
	}
}

// setupAppendMigrBm drives AppendMigrationBitmap. It is AppendCloneBitmap
// without the slice: a migration copies one side, so the chunks are a single
// sequence numbered by the record's bm_cnt (§10.11 step 14 appends two and
// asserts it reaches 2). An empty --bm-hex is again sent as an empty bitmap
// so the gateway's own refusal is the one under test.
func setupAppendMigrBm(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	migrName := fs.String("name", "", "migr_name")
	bmHex := fs.String("bm-hex", "",
		"bitmap as hex; empty sends an empty bitmap on purpose")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.AppendMigrationBitmap(
			ctx, &pb.AppendMigrationBitmapRequest{
				ClusterName: g.cluster,
				SpName:      *spName,
				SpRev:       &pb.SpRev{Revision: uint64(rev)},
				MigrName:    *migrName,
				Bitmap:      bitmapArg(*bmHex),
			})
	}
}

// ---------------------------------------------------------------------------
// spare leg (architecture.md §8.12)
// ---------------------------------------------------------------------------

// setupCreateSpare drives CreateSpareLeg: it allocates one extra leg for a
// raid1 group, parked and unprovisioned, ready to be switched in. --grp is the
// group id the leg joins, and the selector steers the DN the spare lands on
// away from the ones the group's active legs already use.
func setupCreateSpare(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	var grp hexUint
	fs.Var(&grp, "grp", "grp_id the spare leg joins")
	dnSelector := dnSelectorFlags(fs)
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateSpareLeg(ctx, &pb.CreateSpareLegRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			GrpId:       uint64(grp),
			DnSelector:  dnSelector(),
		})
	}
}

// setupDeleteSpare drives DeleteSpareLeg. Both ids are given: --grp says which
// group holds the spare list and --leg which entry of it goes away, which is
// how §10.11 step 15 releases the leg a switch parked.
func setupDeleteSpare(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	var grp, leg hexUint
	fs.Var(&grp, "grp", "grp_id holding the spare leg")
	fs.Var(&leg, "leg", "leg_id of the spare leg to delete")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteSpareLeg(ctx, &pb.DeleteSpareLegRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			GrpId:       uint64(grp),
			LegId:       uint64(leg),
		})
	}
}

// setupSwitchSpare drives SwitchSpareLeg, the swap: --spare is the parked leg
// that becomes active and --target the active leg it replaces, which is then
// parked in the spare list. Naming both sides explicitly is what lets §10.11
// step 15 assert the reply's curr_active_leg_id / curr_spare_leg_id pair
// rather than infer which way the swap went.
func setupSwitchSpare(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	var grp, spare, target hexUint
	fs.Var(&grp, "grp", "grp_id holding both legs")
	fs.Var(&spare, "spare", "spare_leg_id, the parked leg switched in")
	fs.Var(&target, "target", "target_leg_id, the active leg switched out")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.SwitchSpareLeg(ctx, &pb.SwitchSpareLegRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			GrpId:       uint64(grp),
			SpareLegId:  uint64(spare),
			TargetLegId: uint64(target),
		})
	}
}

// ---------------------------------------------------------------------------
// bitmap reads (architecture.md §8.13)
// ---------------------------------------------------------------------------

// setupGetTdBm drives GetThinDeviceBitmap: the allocation bitmap of one slice
// of one thin device, measured on the primary cntlr's CN. It is read-only and
// takes no token.
//
// --start and --cnt are the paging window in blocks; both default to zero,
// which asks the CP for the whole slice. The reply is reshaped into the §10.8
// hex map instead of being printed as protojson base64.
func setupGetTdBm(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	tdName := fs.String("td", "", "td_name")
	sliceIdx := fs.Uint("slice-idx", 0, "slice_idx within the thin device")
	start := fs.Uint64("start", 0, "start_block of the window (0 = from 0)")
	cnt := fs.Uint64("cnt", 0, "block_cnt of the window (0 = all)")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		reply, err := client.GetThinDeviceBitmap(
			ctx, &pb.GetThinDeviceBitmapRequest{
				ClusterName: g.cluster,
				SpName:      *spName,
				TdName:      *tdName,
				SliceIdx:    uint32(*sliceIdx),
				StartBlock:  *start,
				BlockCnt:    *cnt,
			})
		if err != nil {
			return nil, err
		}
		return hexBitmapResult(reply.GetBitmap()), nil
	}
}

// setupGetLegBm drives GetLegBitmap: the same measurement for one leg of a
// raid1 group, addressed by leg id rather than by name because a leg has no
// name. Read-only, no token, same paging window and same hex map as
// setupGetTdBm.
func setupGetLegBm(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name")
	var leg hexUint
	fs.Var(&leg, "leg", "leg_id")
	start := fs.Uint64("start", 0, "start_block of the window (0 = from 0)")
	cnt := fs.Uint64("cnt", 0, "block_cnt of the window (0 = all)")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		reply, err := client.GetLegBitmap(ctx, &pb.GetLegBitmapRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			LegId:       uint64(leg),
			StartBlock:  *start,
			BlockCnt:    *cnt,
		})
		if err != nil {
			return nil, err
		}
		return hexBitmapResult(reply.GetBitmap()), nil
	}
}
