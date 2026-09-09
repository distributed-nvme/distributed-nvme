// The storage pool and controller subcommands of gatewayctl (gateway.md
// §10.8): the eight SP-scoped RPCs of §5.4 including GrowSlice (§5.4/§8.5),
// and the five cntlr RPCs of §5.5 including the two inspects.
//
// Every setup here obeys the main.go contract: it registers its own flags on
// the flag set it is handed and returns the job closure that reads the flag
// POINTERS, never a value — setup runs before Parse, and `race` parses the
// very same set a second time from a JSON params object.
//
// The token convention is uniform across the group: an SP-scoped mutator
// spells its token `--rev <n>` as a hexUint and sends
// `SpRev{Revision: n}` with sp_name left empty, because the handler matches
// on the revision alone. An omitted `--rev` therefore sends a zero token
// deliberately rather than "no token" — that is the nil/zero-token refusal
// the §10.13 B4 stage asserts.

package main

import (
	"context"
	"flag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// spSelectorFlags registers one NodeSelector as a `--<prefix>-black` /
// `--<prefix>-white` pair and returns the accessor that builds the message,
// the way main.go's trConfFlags does for an NvmeTrConf. Both lists empty
// yields nil, so an unmentioned selector reaches the gateway as an absent
// message ("no preference") rather than as an empty black list.
func spSelectorFlags(fs *flag.FlagSet, prefix string) func() *pb.NodeSelector {
	var black, white stringList
	fs.Var(&black, prefix+"-black",
		"comma-separated addr_port values to exclude from the "+prefix+
			" candidates")
	fs.Var(&white, prefix+"-white",
		"comma-separated addr_port values to restrict the "+prefix+
			" candidates to")
	return func() *pb.NodeSelector {
		if len(black) == 0 && len(white) == 0 {
			return nil
		}
		return &pb.NodeSelector{BlackList: black, WhiteList: white}
	}
}

// setupCreateSp drives CreateStoragePool (§5.4).
//
// --cntlr-cnt, --slice-cnt, --init-ext-cnt, --slots and --raid1 are the
// §10.6 fixture knobs that shape the SP. Every numeric defaults to 0, which
// is the "substitute the default" value of the §7 bound table, and an empty
// --slots leaves cntlid_slot_list empty so the gateway defaults it to [0..7];
// the script therefore names only what it cares about.
//
// --stripe-size, --block-size and --feature-junk exist for the case C
// validation battery (§10.14 step 1): the first two push the DmRaid0Conf and
// DmPoolConf bounds, and --feature-junk appends one empty BdevFeature purely
// so the "bdev_feature_list must be empty" rule can be watched refusing a
// request. A BdevConf is built only when --raid1 or one of those three asks
// for it, so the default request carries no bdev_conf at all.
//
// The four --thr-* flags fill EventThreshold, where 0 again means "use the
// default", so the message is sent only when at least one is set. The four
// selector lists fill the CN and DN NodeSelectors through spSelectorFlags.
func setupCreateSp(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool to create")
	cntlrCnt := fs.Uint("cntlr-cnt", 0,
		"cntlr_cnt — how many controllers to allocate (0 = the default)")
	sliceCnt := fs.Uint("slice-cnt", 0,
		"slice_cnt — how many slices the SP has (0 = the default)")
	initExtCnt := fs.Uint64("init-ext-cnt", 0,
		"init_ext_cnt — DN extents per data group (0 = the default)")
	var slots u32List
	fs.Var(&slots, "slots",
		"cntlid_slot_list; empty lets the gateway default it to [0..7]")
	raid1 := fs.Bool("raid1", false,
		"redund_conf = redund_md_raid1 instead of the implicit redund_none")
	stripeSize := fs.Uint64("stripe-size", 0,
		"bdev_conf.dm_raid0_conf.stripe_size in bytes")
	blockSize := fs.Uint64("block-size", 0,
		"bdev_conf.dm_pool_conf.data_block_size in bytes")
	featureJunk := fs.Bool("feature-junk", false,
		"append one empty bdev_feature so the empty-list rule refuses it")
	thrPrimary := fs.Uint("thr-primary", 0,
		"event_threshold.primary_unhealthy in seconds")
	thrCntlr := fs.Uint("thr-cntlr", 0,
		"event_threshold.cntlr_unhealthy in seconds")
	thrSide := fs.Uint("thr-side", 0,
		"event_threshold.side_unhealthy in seconds")
	thrLeg := fs.Uint("thr-leg", 0,
		"event_threshold.leg_unhealthy in seconds")
	dnSelector := spSelectorFlags(fs, "dn")
	cnSelector := spSelectorFlags(fs, "cn")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		req := &pb.CreateStoragePoolRequest{
			ClusterName:    g.cluster,
			SpName:         *spName,
			CntlrCnt:       uint32(*cntlrCnt),
			SliceCnt:       uint32(*sliceCnt),
			InitExtCnt:     *initExtCnt,
			CntlidSlotList: slots,
			CnSelector:     cnSelector(),
			DnSelector:     dnSelector(),
		}
		if *raid1 || *stripeSize != 0 || *blockSize != 0 || *featureJunk {
			req.BdevConf = &pb.BdevConf{}
			if *raid1 {
				req.BdevConf.RedundConf = &pb.RedundConf{
					RedunKind: &pb.RedundConf_RedundMdRaid1{
						RedundMdRaid1: &pb.RedundMdRaid1{},
					},
				}
			}
			if *stripeSize != 0 {
				req.BdevConf.DmRaid0Conf = &pb.DmRaid0Conf{
					StripeSize: *stripeSize,
				}
			}
			if *blockSize != 0 {
				req.BdevConf.DmPoolConf = &pb.DmPoolConf{
					DataBlockSize: *blockSize,
				}
			}
			if *featureJunk {
				req.BdevConf.BdevFeatureList = []*pb.BdevFeature{{}}
			}
		}
		if *thrPrimary != 0 || *thrCntlr != 0 ||
			*thrSide != 0 || *thrLeg != 0 {
			req.EventThreshold = &pb.EventThreshold{
				PrimaryUnhealthy: uint32(*thrPrimary),
				CntlrUnhealthy:   uint32(*thrCntlr),
				SideUnhealthy:    uint32(*thrSide),
				LegUnhealthy:     uint32(*thrLeg),
			}
		}
		return client.CreateStoragePool(ctx, req)
	}
}

// setupDeleteSp drives DeleteStoragePool (§5.4), the one SP-scoped mutator
// the `deleting` gate does not apply to. It refuses an SP that still owns a
// td, nqn, clone, xfer or migr, which is the case C precondition the suite
// asserts, so the script always tears those down first.
func setupDeleteSp(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool to delete")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteStoragePool(ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
		})
	}
}

// setupGetSp drives GetStoragePool (§5.4). Besides being the state assertion
// of every stage, its reply is the token source: `race`'s stale-revision
// retry re-reads sp_rev.revision through this very RPC (refreshToken).
func setupGetSp(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool to read")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.GetStoragePool(ctx, &pb.GetStoragePoolRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
		})
	}
}

// setupListSps drives ListStoragePools (§5.4). --count is the page size that
// the §7 clamp turns into 64 when it is 0 and refuses above 1024, and
// --page-token continues a previous page — the pair case C probes with a
// bad token.
func setupListSps(fs *flag.FlagSet) job {
	count := fs.Uint("count", 0, "page size; 0 asks for the default of 64")
	pageToken := fs.String("page-token", "",
		"page_token returned by the previous page")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.ListStoragePools(ctx, &pb.ListStoragePoolsRequest{
			ClusterName: g.cluster,
			Count:       uint32(*count),
			PageToken:   *pageToken,
		})
	}
}

// setupSetCntlidSlots drives UpdateStoragePoolCntlidSlotList (§5.4). Unlike
// create-sp's --slots, an empty list here is a refusal the gateway owns (an
// SP with no slot can produce no side), so the flag is sent exactly as given.
func setupSetCntlidSlots(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	var slots u32List
	fs.Var(&slots, "slots", "the full new cntlid_slot_list")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateStoragePoolCntlidSlotList(ctx,
			&pb.UpdateStoragePoolCntlidSlotListRequest{
				ClusterName:    g.cluster,
				SpName:         *spName,
				SpRev:          &pb.SpRev{Revision: uint64(rev)},
				CntlidSlotList: slots,
			})
	}
}

// setupSetSpLevel drives UpdateStoragePoolLevel (§5.4). --level goes through
// main.go's parseSpLevel, which also accepts a raw number so the script can
// send a level the enum does not declare and watch the gateway refuse it
// (§10.14 step 1). The parse happens inside the job, not in setup, because
// setup runs before Parse.
func setupSetSpLevel(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	level := fs.String("level", "READWRITE",
		"sp_level name, full enum name or raw number")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		spLevel, err := parseSpLevel(*level)
		if err != nil {
			return nil, err
		}
		return client.UpdateStoragePoolLevel(ctx,
			&pb.UpdateStoragePoolLevelRequest{
				ClusterName: g.cluster,
				SpName:      *spName,
				SpRev:       &pb.SpRev{Revision: uint64(rev)},
				SpLevel:     spLevel,
			})
	}
}

// setupFindSpNames drives FindStoragePoolNames (§5.4), the snapshot read that
// maps sp_ids back to names. Unknown ids are omitted from the reply map
// rather than being an error, which is what the suite asserts by asking for a
// mix of live and dead ids in one --ids list.
func setupFindSpNames(fs *flag.FlagSet) job {
	var ids idList
	fs.Var(&ids, "ids", "comma-separated sp_id_list, decimal or 0x hex")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.FindStoragePoolNames(ctx,
			&pb.FindStoragePoolNamesRequest{
				ClusterName: g.cluster,
				SpIdList:    ids,
			})
	}
}

// setupGrowSlice drives GrowSlice (§5.4/§8.5). --ext and --meta are the two
// exclusive halves of that section: a data grow needs ext_cnt > 0, and a meta
// grow needs ext_cnt == 0 because the meta ladder picks the size itself.
// Sending both is the `--meta --ext 2` row of the case C battery, so neither
// flag constrains the other here — the gateway is the one that refuses.
// --dn-black seeds the §6.5 black list the placement scan grows.
func setupGrowSlice(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	var slice hexUint
	fs.Var(&slice, "slice", "slice_id to grow")
	extCnt := fs.Uint64("ext", 0, "ext_cnt to add to the data group")
	isMeta := fs.Bool("meta", false,
		"grow the meta group by one ladder step instead of the data group")
	dnSelector := spSelectorFlags(fs, "dn")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.GrowSlice(ctx, &pb.GrowSliceRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			SliceId:     uint64(slice),
			ExtCnt:      *extCnt,
			IsMeta:      *isMeta,
			DnSelector:  dnSelector(),
		})
	}
}

// setupCreateCntlr drives CreateCntlr (§5.5). --slot is the cntlid_slot the
// new controller takes; it must be in the SP's cntlid_slot_list and unused,
// which is the state-dependent refusal the suite drives by asking twice for
// the same slot. --cn-black keeps a race's two jobs off each other's CN.
func setupCreateCntlr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	slot := fs.Uint("slot", 0, "cntlid_slot for the new controller")
	cnSelector := spSelectorFlags(fs, "cn")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateCntlr(ctx, &pb.CreateCntlrRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			CntlidSlot:  uint32(*slot),
			CnSelector:  cnSelector(),
		})
	}
}

// setupDeleteCntlr drives DeleteCntlr (§5.5). --id is the cntlr_id; deleting
// the primary, or an enabled non-primary, is refused, which is the pair of
// FAILED_PRECONDITION rows of §10.14 step 3.
func setupDeleteCntlr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	var id hexUint
	fs.Var(&id, "id", "cntlr_id to delete")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteCntlr(ctx, &pb.DeleteCntlrRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			CntlrId:     uint64(id),
		})
	}
}

// setupSetCntlrEnabled drives UpdateCntlrEnabled (§5.5). --enabled defaults
// to true because disabling is the deliberate act and re-enabling is the
// undo; the flag package never consumes the next argument for a bool, so the
// script must write `--enabled=false`, never `--enabled false`.
func setupSetCntlrEnabled(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev.revision token")
	var id hexUint
	fs.Var(&id, "id", "cntlr_id to update")
	enabled := fs.Bool("enabled", true,
		"the new enabled state; write --enabled=false to disable")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateCntlrEnabled(ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			CntlrId:     uint64(id),
			Enabled:     *enabled,
		})
	}
}

// setupInspectCntlr drives InspectCntlr (§5.5): the gateway asks the CN agent
// that hosts the controller for its live CntlrInfo. It is a read, so it takes
// no token — --id is the cntlr_id.
func setupInspectCntlr(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var id hexUint
	fs.Var(&id, "id", "cntlr_id to inspect")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.InspectCntlr(ctx, &pb.InspectCntlrRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			CntlrId:     uint64(id),
		})
	}
}

// setupInspectSide drives InspectSide (§5.5): the DN-side mirror of
// inspect-cntlr, asking the DN agent that hosts the side for its SideInfo.
// --id is the SIDE_ID here, not a cntlr_id — the two inspects share the flag
// name so the §10.8 table reads as one row, and the RPC decides what it means.
func setupInspectSide(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name of the storage pool")
	var id hexUint
	fs.Var(&id, "id", "side_id to inspect")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.InspectSide(ctx, &pb.InspectSideRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SideId:      uint64(id),
		})
	}
}
