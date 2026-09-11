// The storage-pool group of dnvctl (dnvctl.md §5.4). Five commands are the
// pool object's own RPCs — create, delete, get, list, set-level — and the
// other four are the RPCs §0 #4 homed here rather than giving each a group of
// its own: UpdateStoragePoolCntlidSlotList, FindStoragePoolNames, GrowSlice
// and InspectSide. Everything else under an SP (cntlr, td, ss, ns, clone,
// xfer, migr, spare) is its own group file.
//
// Three group-wide notes:
//
//   - `sp_name` is never a local flag here. It is §5.0's second field→flag
//     exception: the global --sp fills it on every request that carries it,
//     `sp create` included. `sp list` and `sp find-names` have no sp_name
//     field at all, so for those two the global is simply ignored.
//   - Four commands carry a token: delete, set-cntlid-slots, set-level and
//     grow-slice call spRev. The four reads carry none, and neither does
//     `sp create` — there is no SP yet to have a revision. `sp get` is where
//     the operator read the number they pass back as --rev (§4).
//   - The two parse helpers below are prefixed `sp` because ctl/ is a single
//     package and the twelve group files must not collide (§1.2 — the
//     gatewayctl lesson).

package ctl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// spLevels maps the short --level spellings onto the enum. The full
// `SP_LEVEL_*` spelling reaches the same entries because spParseLevel strips
// that prefix before the lookup. The enum numbers step by 16, so the gaps
// between them are not levels — but see spParseLevel on why dnvctl does not
// say so.
var spLevels = map[string]pb.SpLevel{
	"READWRITE":    pb.SpLevel_SP_LEVEL_READWRITE,
	"READONLY":     pb.SpLevel_SP_LEVEL_READONLY,
	"NO_CLONE":     pb.SpLevel_SP_LEVEL_NO_CLONE,
	"NO_THINPOOL":  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	"NO_REDUND":    pb.SpLevel_SP_LEVEL_NO_REDUND,
	"NO_MIGRATION": pb.SpLevel_SP_LEVEL_NO_MIGRATION,
	"NO_SIDE":      pb.SpLevel_SP_LEVEL_NO_SIDE,
	"DISABLE":      pb.SpLevel_SP_LEVEL_DISABLE,
}

// spParseLevel turns --level into an SpLevel (§5.4). It accepts a short name,
// the full enum name (both case-insensitive) and a RAW NUMBER — the last of
// those is not a convenience but CT8: dnvctl must not second-guess an enum, so
// `--level 17`, a value the enum does not declare, is forwarded as typed and
// the gateway's validateSpLevel is the one that refuses it. Only a spelling
// that is neither a known name nor a number fails to parse (exit 2).
func spParseLevel(spec string) (pb.SpLevel, error) {
	name := strings.ToUpper(strings.TrimSpace(spec))
	if level, ok := spLevels[strings.TrimPrefix(name, "SP_LEVEL_")]; ok {
		return level, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(spec), 0, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid --level: unknown sp_level %q", spec)
	}
	return pb.SpLevel(value), nil
}

// spRedundConf builds the redund_conf arm --redund names. This is the one
// dnvctl-side default the spec keeps (§0 #11, architecture.md §8.4): `sp
// create` ALWAYS sends a bdev_conf with a redund_conf, and an unmentioned
// --redund means raid1 rather than "let the gateway decide". A flag default is
// not a hidden RPC, so CT8 is untouched.
//
// chunkBlocks is --bitmap-chunk-blocks; 0 leaves the control plane its own
// chunk size. It belongs to the raid1 arm only and is quietly unused under
// `none` — cross-checking the two flags would be exactly the client-side
// validation CT8 forbids.
//
// An unrecognised spelling IS a parse failure (exit 2): the flag names a oneof
// arm, and the oneof has no third arm an unknown value could be forwarded in.
func spRedundConf(spec string, chunkBlocks uint64) (*pb.RedundConf, error) {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "raid1":
		// Note the proto's one-`d` `RedunKind` oneof label; it is load-bearing.
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: chunkBlocks,
				},
			},
		}, nil
	case "none":
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}, nil
	default:
		return nil, fmt.Errorf(
			"invalid --redund %q: want raid1 or none", spec)
	}
}

// registerSp adds the `sp` group and its nine leaves (§5.4).
func registerSp(root *cobra.Command) {
	root.AddCommand(group("sp", "storage pools",
		spCreateCmd(),
		spDeleteCmd(),
		spGetCmd(),
		spListCmd(),
		spSetCntlidSlotsCmd(),
		spSetLevelCmd(),
		spFindNamesCmd(),
		spGrowSliceCmd(),
		spInspectSideCmd(),
	))
}

// spCreateCmd is CreateStoragePool, the widest request in the service.
//
// What is always sent: bdev_conf, because its redund_conf always is — see
// spRedundConf. What is sent only on demand: dm_raid0_conf and dm_pool_conf,
// each built only when its size flag is non-zero, so an untuned create leaves
// them absent and the control plane picks the geometry; and event_threshold,
// built only when at least one of the four --thr-* values is non-zero, for the
// same reason — an all-zero EventThreshold would say "default everything" the
// long way.
//
// What is never sent: bdev_feature_list. v1 exposes no BdevFeature flags at
// all (§1.1; gatewayctl's --feature-junk was a driver-only poke at the
// gateway's "must be empty" rule and is deliberately not ported).
//
// CreateStoragePool carries no token — there is no SP yet to have a revision —
// so the global --rev is ignored here.
func spCreateCmd() *cobra.Command {
	cmd := leaf("create", "create a storage pool (CreateStoragePool)",
		func() (job, error) {
			slots, err := u32ListOf("slots")
			if err != nil {
				return nil, err
			}
			redundConf, err := spRedundConf(
				strOf("redund"), u64Of("bitmap-chunk-blocks"))
			if err != nil {
				return nil, err
			}
			req := &pb.CreateStoragePoolRequest{
				ClusterName:    clusterOf(),
				SpName:         spOf(),
				BdevConf:       &pb.BdevConf{RedundConf: redundConf},
				CntlidSlotList: slots,
				CntlrCnt:       u32Of("cntlr-cnt"),
				SliceCnt:       u32Of("slice-cnt"),
				InitExtCnt:     u64Of("init-ext-cnt"),
				CnSelector:     selectorOf("cn"),
				DnSelector:     selectorOf("dn"),
			}
			if stripeSize := u64Of("stripe-size"); stripeSize != 0 {
				req.BdevConf.DmRaid0Conf = &pb.DmRaid0Conf{
					StripeSize: stripeSize,
				}
			}
			if blockSize := u64Of("block-size"); blockSize != 0 {
				req.BdevConf.DmPoolConf = &pb.DmPoolConf{
					DataBlockSize: blockSize,
				}
			}
			primary := u32Of("thr-primary")
			cntlr := u32Of("thr-cntlr")
			side := u32Of("thr-side")
			leg := u32Of("thr-leg")
			if primary != 0 || cntlr != 0 || side != 0 || leg != 0 {
				req.EventThreshold = &pb.EventThreshold{
					PrimaryUnhealthy: primary,
					CntlrUnhealthy:   cntlr,
					SideUnhealthy:    side,
					LegUnhealthy:     leg,
				}
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.CreateStoragePool(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.Uint32("cntlr-cnt", 0,
		"cntlr_cnt — controllers to allocate (0 = the gateway default)")
	flags.Uint32("slice-cnt", 0,
		"slice_cnt — slices the SP has (0 = the gateway default)")
	flags.Uint64("init-ext-cnt", 0,
		"init_ext_cnt — DN extents per data group (0 = the default)")
	flags.String("slots", "",
		"comma-separated cntlid_slot_list (empty = the gateway default)")
	flags.String("redund", "raid1",
		"bdev_conf.redund_conf arm: raid1 or none")
	flags.Uint64("bitmap-chunk-blocks", 0,
		"redund_md_raid1.bitmap_chunk_block_cnt (0 = the CP default)")
	flags.Uint64("stripe-size", 0,
		"bdev_conf.dm_raid0_conf.stripe_size in bytes (0 = not sent)")
	flags.Uint64("block-size", 0,
		"bdev_conf.dm_pool_conf.data_block_size in bytes (0 = not sent)")
	flags.Uint32("thr-primary", 0,
		"event_threshold.primary_unhealthy in seconds")
	flags.Uint32("thr-cntlr", 0,
		"event_threshold.cntlr_unhealthy in seconds")
	flags.Uint32("thr-side", 0,
		"event_threshold.side_unhealthy in seconds")
	flags.Uint32("thr-leg", 0,
		"event_threshold.leg_unhealthy in seconds")
	selectorFlags(flags, "dn")
	selectorFlags(flags, "cn")
	return cmd
}

// spDeleteCmd is DeleteStoragePool. It takes no flag of its own: the SP is
// named by the global --sp and the optional token by the global --rev.
func spDeleteCmd() *cobra.Command {
	return leaf("delete", "delete a storage pool (DeleteStoragePool)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			req := &pb.DeleteStoragePoolRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.DeleteStoragePool(ctx, req)
			}, nil
		})
}

// spGetCmd is GetStoragePool — the SpRev token source (§4). Its reply carries
// sp_rev.revision, which is the number an operator feeds back as --rev on the
// 30 SP-scoped mutators.
func spGetCmd() *cobra.Command {
	return leaf("get", "read a storage pool (GetStoragePool)",
		func() (job, error) {
			req := &pb.GetStoragePoolRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.GetStoragePool(ctx, req)
			}, nil
		})
}

// spListCmd is ListStoragePools. The request has no sp_name field, so the
// global --sp is ignored; --count 0 asks for the server's own page size, which
// dnvctl does not substitute (CT8).
func spListCmd() *cobra.Command {
	cmd := leaf("list", "list storage pool names (ListStoragePools)",
		func() (job, error) {
			req := &pb.ListStoragePoolsRequest{
				ClusterName: clusterOf(),
				Count:       u32Of("count"),
				PageToken:   strOf("page-token"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.ListStoragePools(ctx, req)
			}, nil
		})
	pageFlags(cmd.Flags())
	return cmd
}

// spSetCntlidSlotsCmd is UpdateStoragePoolCntlidSlotList: --slots replaces the
// whole list. Unlike `sp create`'s --slots, an EMPTY list here is not "let the
// gateway default it" — it is sent as-is and refused server-side, because
// validateCntlidSlotList is called with allowEmpty=false on this path and
// with allowEmpty=true on create's. u32ListOf yields a nil slice for an empty
// flag, which is the same empty repeated field on the wire as a zero-length
// one; what matters is that dnvctl neither suppresses the field nor
// substitutes a list of its own (CT8).
func spSetCntlidSlotsCmd() *cobra.Command {
	cmd := leaf("set-cntlid-slots",
		"replace the cntlid slot list (UpdateStoragePoolCntlidSlotList)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			slots, err := u32ListOf("slots")
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateStoragePoolCntlidSlotListRequest{
				ClusterName:    clusterOf(),
				SpName:         spOf(),
				SpRev:          rev,
				CntlidSlotList: slots,
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.UpdateStoragePoolCntlidSlotList(ctx, req)
			}, nil
		})
	cmd.Flags().String("slots", "",
		"the full new cntlid_slot_list, comma-separated")
	return cmd
}

// spSetLevelCmd is UpdateStoragePoolLevel. --level defaults to READWRITE, the
// enum's zero value, so the flag's default and the field's default agree and
// omitting it sends no surprise. See spParseLevel for the accepted spellings.
func spSetLevelCmd() *cobra.Command {
	cmd := leaf("set-level",
		"set the storage pool level (UpdateStoragePoolLevel)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			level, err := spParseLevel(strOf("level"))
			if err != nil {
				return nil, err
			}
			req := &pb.UpdateStoragePoolLevelRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				SpLevel:     level,
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.UpdateStoragePoolLevel(ctx, req)
			}, nil
		})
	cmd.Flags().String("level", "READWRITE",
		"sp_level: a short name, an SP_LEVEL_* name, or a raw number")
	return cmd
}

// spFindNamesCmd is FindStoragePoolNames, the id→name snapshot read. The
// request has no sp_name field, so the global --sp is ignored. Ids the cluster
// does not know are omitted from the reply map rather than being an error,
// which is the gateway's business, not dnvctl's.
func spFindNamesCmd() *cobra.Command {
	cmd := leaf("find-names",
		"map sp ids to sp names (FindStoragePoolNames)",
		func() (job, error) {
			ids, err := idListOf("ids")
			if err != nil {
				return nil, err
			}
			req := &pb.FindStoragePoolNamesRequest{
				ClusterName: clusterOf(),
				SpIdList:    ids,
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.FindStoragePoolNames(ctx, req)
			}, nil
		})
	cmd.Flags().String("ids", "",
		"comma-separated sp_id_list (base 0, so 0x11 is accepted)")
	return cmd
}

// spGrowSliceCmd is GrowSlice (§5.4, architecture.md §8.5). A data grow wants
// --ext > 0 and a meta grow wants --meta with no --ext, because the meta
// ladder picks its own step — but the two flags do NOT constrain each other
// here: `--meta --ext 2` is built and sent, and the gateway is the one that
// refuses it (CT8). --dn-black / --dn-white steer the placement scan.
func spGrowSliceCmd() *cobra.Command {
	cmd := leaf("grow-slice", "grow one slice (GrowSlice)",
		func() (job, error) {
			rev, err := spRev()
			if err != nil {
				return nil, err
			}
			sliceId, err := hexOf("slice")
			if err != nil {
				return nil, err
			}
			req := &pb.GrowSliceRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SpRev:       rev,
				SliceId:     sliceId,
				ExtCnt:      u64Of("ext"),
				IsMeta:      boolOf("meta"),
				DnSelector:  selectorOf("dn"),
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.GrowSlice(ctx, req)
			}, nil
		})
	flags := cmd.Flags()
	flags.String("slice", "", "slice_id to grow")
	flags.Uint64("ext", 0, "ext_cnt to add to the data group")
	flags.Bool("meta", false,
		"grow the meta group by one ladder step instead of the data group")
	selectorFlags(flags, "dn")
	return cmd
}

// spInspectSideCmd is InspectSide: the gateway asks the DN agent that hosts
// the side for its live SideInfo, so this reads past etcd. --id is a SIDE id
// here, spelled the same as `cntlr … --id` on purpose (§5.0) — the RPC, not
// the flag name, decides what the id means.
func spInspectSideCmd() *cobra.Command {
	cmd := leaf("inspect-side", "read a side from its DN agent (InspectSide)",
		func() (job, error) {
			sideId, err := hexOf("id")
			if err != nil {
				return nil, err
			}
			req := &pb.InspectSideRequest{
				ClusterName: clusterOf(),
				SpName:      spOf(),
				SideId:      sideId,
			}
			return func(
				ctx context.Context,
				client pb.GatewayClient,
			) (any, error) {
				return client.InspectSide(ctx, req)
			}, nil
		})
	cmd.Flags().String("id", "", "side_id to inspect")
	return cmd
}
