package gateway

import (
	"bytes"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the READ half of gateway.md §9.3's GW11 item: a fixture that
// plants a sparse stored conf and the RPCs that read it refusing with ABORTED.
//
// The WRITE half — what CreateCluster and CreateStoragePool put in the store —
// is pinned in handler_node_test.go and handler_sp_test.go. Those two are what
// make the confs below reachable only by planting them: every defaultable
// member is resolved on the write path, so a zero read back out of etcd is
// corruption or foreign data and a reader that substituted a constant for it
// would format a pool one way and address it another (§7, GW11).
//
// Three things make each refusal attributable to the gate rather than to
// something else about the request:
//
//   - the message is compared in full, so a refusal that happened to be
//     ABORTED for another reason — a stale token, a missing invariant key, an
//     unreachable agent — cannot stand in for the conf gate;
//   - every key the fixture's cluster_id tags is asserted byte-identical
//     across the round of refusals, which is the "an error out of the closure
//     aborts the transaction uncommitted" contract (EU4);
//   - every refused request is replayed unchanged once the conf is repaired,
//     where it must succeed. That is what rules out a request that was simply
//     malformed, and it is also the GW11 claim an operator cares about: the
//     refusal is the conf's, so repairing the conf is the whole recovery.

// The stored-conf sentences these tests pin, spelled out rather than built
// from the constants they name. model.ValidateClusterConf/ValidateBdevConf
// compose them and model/capacity_test.go pins them there; what is asserted
// HERE is that the sentence survives the trip through a handler's errAborted
// to the client, prefix and field name intact (§7's "the prefix an operator
// and the acceptance checklist grep for").
const (
	scMsgDnBatchSize = "invalid stored conf: " +
		"alloc_conf.dn_batch_size 0 is outside [1, 1024]"
	scMsgExtentSize = "invalid stored conf: dn_bin_conf.extent_size is zero"
	scMsgLadder     = "invalid stored conf: dn_bin_conf shifts 0/0/8/12 " +
		"are not a ladder 0 <= bin0 < bin1 < bin2 < bin3 <= 63"
	scMsgCntlrInterval = "invalid stored conf: " +
		"health_check_conf.cntlr_interval 0 is outside [1, 3600]"
	scMsgDataBlockSize = "invalid stored conf: " +
		"bdev_conf.dm_pool_conf.data_block_size is zero"
	scMsgLowWaterMark = "invalid stored conf: " +
		"bdev_conf.dm_pool_conf.low_water_mark_pct is zero"
	scMsgStripeSize = "invalid stored conf: " +
		"bdev_conf.dm_raid0_conf.stripe_size is zero"
	scMsgChunkBlockCnt = "invalid stored conf: " +
		"bdev_conf.redund_conf.redund_md_raid1.bitmap_chunk_block_cnt is zero"
)

// scCase is one read site: the name a failure reports and the RPC that reaches
// the gate. The call is replayed after the repair, so it must be a request
// that succeeds against a healthy conf — a case that could only ever fail
// would prove nothing about which of its properties the gate refused.
type scCase struct {
	name string
	call func() error
}

// scRefusals drives every case against the conf currently in the store and
// asserts the one refusal, then that nothing moved.
func scRefusals(t *testing.T, env *sptEnv, want string, cases []scCase) {
	t.Helper()
	before := env.dump()
	for _, tc := range cases {
		err := tc.call()
		wantCode(t, err, codes.Aborted, tc.name)
		if got := status.Convert(err).Message(); got != want {
			t.Errorf("%s: message %q, want %q", tc.name, got, want)
		}
	}
	scWantUntouched(t, before, env.dump())
}

// scReplay re-runs every case with the conf repaired. This is the control: it
// is what says the requests above were refused for their conf and for nothing
// else.
func scReplay(t *testing.T, cases []scCase) {
	t.Helper()
	for _, tc := range cases {
		if err := tc.call(); err != nil {
			t.Errorf("%s after the stored conf was repaired: %v", tc.name, err)
		}
	}
}

// scWantUntouched asserts that a round of refusals wrote nothing, in the shape
// TestCreateStoragePoolRefusals and TestDeleteStoragePoolRefusals use.
func scWantUntouched(
	t *testing.T,
	before map[string][]byte,
	after map[string][]byte,
) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("a refusal wrote keys: %d before, %d after",
			len(before), len(after))
	}
	for key, value := range before {
		if !bytes.Equal(value, after[key]) {
			t.Errorf("a refusal rewrote %q", key)
		}
	}
}

// scPutCc plants one stored ClusterConf under the cluster's name key, with
// mustPut — the fixture's own writer (§9.3). Planting is what it takes: the
// gateway writes this key in exactly one place (CreateCluster, which stores
// model.ResolveClusterConf's output) and deletes it in one other, so no
// request can leave a conf here that model.ValidateClusterConf refuses.
func scPutCc(env *sptEnv, cc *pb.ClusterConf) {
	env.t.Helper()
	mustPut(env.t, env.cli, model.ClusterConfKey(env.name), cc)
}

// scSparseCc is the fixture's healthy stored ClusterConf with one member
// zeroed. The clone matters: env.cc is the message the repair puts back.
func scSparseCc(env *sptEnv, zero func(cc *pb.ClusterConf)) *pb.ClusterConf {
	cc := proto.Clone(env.cc).(*pb.ClusterConf)
	zero(cc)
	return cc
}

// scPutSpConf plants one stored SpConf, and scSparseSpConf zeroes one member
// of a copy of the healthy one.
func scPutSpConf(env *sptEnv, conf *pb.SpConf) {
	env.t.Helper()
	mustPut(env.t, env.cli, model.SpConfKey(env.cid, sptSpName), conf)
}

func scSparseSpConf(conf *pb.SpConf, zero func(conf *pb.SpConf)) *pb.SpConf {
	sparse := proto.Clone(conf).(*pb.SpConf)
	zero(sparse)
	return sparse
}

// ---------------------------------------------------------------------------
// The stored ClusterConf (GW11, §7)
// ---------------------------------------------------------------------------

// TestStoredClusterConfZeroIsRefusedByEveryReader plants a sparse stored
// ClusterConf and drives every RPC that computes a DN's geometry or its
// capacity key from one — the RPCs that take DN capacity as well as the ones
// that give it back.
//
// The thirteen cases are five different gates:
//
//   - CreateDiskNode and CreateControllerNode check inside their own STM,
//     immediately above the division that turns the node's byte count into
//     extents (§6.1) — the zero the gate refuses is the one that would
//     otherwise be divided by;
//   - GrowSlice checks both confs after its snapshot and before the meta
//     ladder, so that model.MetaLadderExtCnt's "false" can keep meaning the
//     16 GiB cap and nothing else;
//   - the five allocating RPCs — CreateStoragePool, GrowSlice, CreateCntlr,
//     CreateSpareLeg and CreateMigration, i.e. every caller of pickDns or
//     pickCn — reach the check through the §6.5 scan itself, which refuses
//     rather than scan with a batch size of zero: a zero width finds no
//     candidate and would report a cluster full of free extents as
//     RESOURCE_EXHAUSTED. CreateCntlr, CreateSpareLeg and CreateMigration
//     carry no conf gate of their own (nor does model.CreateSpareLeg), so
//     those three are the cases that pin the allocator's;
//   - DeleteStoragePool, DeleteSpareLeg, FinishMigration and CancelMigration
//     reach newDnLedger's gate: those four are every handler that RELEASES DN
//     capacity through a ledger and had no conf gate of its own, and all four
//     are driven here rather than one standing for the rest, so moving any of
//     them off the shared constructor is caught. The ladder matters most on
//     these paths: MaintainDnCapacity names the key to delete by shifting the
//     stored ladder, so a gate-less release deletes a key nothing wrote and
//     strands the live one — which outlives the DnConf and keeps being
//     offered by the §6.5 scan once the conf is repaired;
//   - DeleteDiskNode and UpdateDiskNodeDisabled check in their own STM, as
//     the last thing before their first write. Both move exactly one capacity
//     key and nothing else, so without a gate their whole effect would be the
//     wrong one.
//
// Each gate validates the WHOLE stored conf rather than the member it is
// about to use, which is why one zeroed member reaches all thirteen. Two are
// planted in turn: `dn_bin_conf.extent_size`, the §9 fixture's member and the
// one a gateway that lost its gate would divide by, and
// `alloc_conf.dn_batch_size`, which nothing divides by — so the second row
// keeps failing as an assertion rather than a panic when a gate is removed.
//
// CreateStoragePool cannot distinguish its own in-STM gate from the
// allocator's — the two run the same predicate over the same stored key and
// pickDns runs first — so what that case pins is the RPC's refusal, not which
// of its two gates produced it. CreateMigration is in the same position once
// newDnLedger has a gate, and for the same reason: pickDns runs first.
//
// The six release cases come FIRST in the list, because scReplay runs the
// cases in order against the repaired conf: an allocating case replayed
// earlier could place a side on the very disk node DeleteDiskNode is about,
// and turn its replay into a FAILED_PRECONDITION about sides.
func TestStoredClusterConfZeroIsRefusedByEveryReader(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	sliceId := conf.GetSliceIdList()[0]
	slice := env.slice(spId, sliceId)
	dataGrp := slice.GetDataGrpList()[0]
	grpId := dataGrp.GetGrpId()
	// The source of the migration below: one side of the data group, whose
	// leg holds exactly one, so §8.11's "a migration is already running on
	// this leg" precondition cannot stand in for the conf refusal.
	srcSideId := dataGrp.GetLegList()[0].GetSideList()[0].GetSideId()
	// What the four ledger cases release, built while the conf is still
	// healthy, each on an object of its own so that no case's own §8
	// preconditions can stand in for the conf refusal:
	//
	//   - a second SP that holds nothing (DeleteStoragePool);
	//   - a spare leg on the META group, not the data group the CreateSpareLeg
	//     case below adds one to, so neither can fill the other's §8.12 slot;
	//   - a migration on the data group's SECOND leg, which FinishMigration
	//     ends, leaving leg 0 free for the CreateMigration case;
	//   - a migration on the meta group's first leg, which CancelMigration
	//     rolls back.
	delSpName := "sc-del-pool"
	env.createSp(sptSmallSpec(delSpName))
	metaGrp := slice.GetMetaGrpList()[0]
	metaGrpId := metaGrp.GetGrpId()
	spareReply, err := env.srv.CreateSpareLeg(env.ctx,
		&pb.CreateSpareLegRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			GrpId:       metaGrpId,
		})
	if err != nil {
		t.Fatalf("CreateSpareLeg for the DeleteSpareLeg case: %v", err)
	}
	finMigrName := "sc-fin-migr"
	if _, err := env.srv.CreateMigration(env.ctx, &pb.CreateMigrationRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		MigrName:    finMigrName,
		SrcSideId:   dataGrp.GetLegList()[1].GetSideList()[0].GetSideId(),
	}); err != nil {
		t.Fatalf("CreateMigration for the FinishMigration case: %v", err)
	}
	cancelMigrName := "sc-cancel-migr"
	if _, err := env.srv.CreateMigration(env.ctx, &pb.CreateMigrationRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		MigrName:    cancelMigrName,
		SrcSideId:   metaGrp.GetLegList()[0].GetSideList()[0].GetSideId(),
	}); err != nil {
		t.Fatalf("CreateMigration for the CancelMigration case: %v", err)
	}
	// A listener per node-creating RPC: both probe the agent for a size
	// BEFORE they open a transaction (AG1), so a case without one would be
	// ABORTED by the agent path and never reach the conf at all.
	dnAddr := fakeAddrPort(t, "sc-dn")
	startFakeAgent(t, dnAddr, 100<<30)
	cnAddr := fakeAddrPort(t, "sc-cn")
	startFakeAgent(t, cnAddr, 1<<40)
	// The node DeleteDiskNode is about, created after every SP above so it
	// hosts no side: §8.2's "delete or migrate the owning storage pools
	// first" is the one refusal that would otherwise hide the conf's. It is
	// created ENABLED, so it carries a capacity key and its delete is a real
	// MaintainDnCapacity delete rather than a no-op on an absent key (§5.6).
	delDnAddr := fakeAddrPort(t, "sc-del-dn")
	startFakeAgent(t, delDnAddr, 100<<30)
	if _, err := env.srv.CreateDiskNode(env.ctx, &pb.CreateDiskNodeRequest{
		ClusterName: env.name,
		AddrPort:    delDnAddr,
		NvmeTrConf:  sptTrConf(delDnAddr),
		Location:    "sc-del-dn-rack",
	}); err != nil {
		t.Fatalf("CreateDiskNode for the DeleteDiskNode case: %v", err)
	}

	cases := []scCase{
		{"DeleteStoragePool", func() error {
			// No SpRev, as in GrowSlice below: GW6 is presence-based, so no
			// stale-token refusal can stand in for the conf's (§0 #7).
			_, err := env.srv.DeleteStoragePool(env.ctx,
				&pb.DeleteStoragePoolRequest{
					ClusterName: env.name,
					SpName:      delSpName,
				})
			return err
		}},
		{"DeleteSpareLeg", func() error {
			_, err := env.srv.DeleteSpareLeg(env.ctx,
				&pb.DeleteSpareLegRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					GrpId:       metaGrpId,
					LegId:       spareReply.GetLegId(),
				})
			return err
		}},
		{"FinishMigration", func() error {
			// force: §8.11's hydration proof is a GetSideInfo on the
			// destination DN's agent, which this fixture has no listener for
			// — without the flag the case would be FAILED_PRECONDITION from
			// the agent path and never reach the deciding STM at all. force
			// skips only that judgement; the transaction below it, ledger
			// included, is the same one.
			_, err := env.srv.FinishMigration(env.ctx,
				&pb.FinishMigrationRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					MigrName:    finMigrName,
					Force:       true,
				})
			return err
		}},
		{"CancelMigration", func() error {
			_, err := env.srv.CancelMigration(env.ctx,
				&pb.CancelMigrationRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					MigrName:    cancelMigrName,
				})
			return err
		}},
		{"DeleteDiskNode", func() error {
			_, err := env.srv.DeleteDiskNode(env.ctx,
				&pb.DeleteDiskNodeRequest{
					ClusterName: env.name,
					AddrPort:    delDnAddr,
				})
			return err
		}},
		{"UpdateDiskNodeDisabled", func() error {
			// A flag the stored record does not already carry: §0 #17's
			// no-op arm returns nil BEFORE the gate, precisely because a
			// request that writes nothing maintains no capacity key.
			_, err := env.srv.UpdateDiskNodeDisabled(env.ctx,
				&pb.UpdateDiskNodeDisabledRequest{
					ClusterName: env.name,
					AddrPort:    env.dnAddrs[0],
					Disabled:    true,
				})
			return err
		}},
		{"CreateDiskNode", func() error {
			_, err := env.srv.CreateDiskNode(env.ctx,
				&pb.CreateDiskNodeRequest{
					ClusterName: env.name,
					AddrPort:    dnAddr,
					NvmeTrConf:  sptTrConf(dnAddr),
					Location:    "sc-dn-rack",
				})
			return err
		}},
		{"CreateControllerNode", func() error {
			_, err := env.srv.CreateControllerNode(env.ctx,
				&pb.CreateControllerNodeRequest{
					ClusterName: env.name,
					AddrPort:    cnAddr,
					NvmeTrConf:  sptTrConf(cnAddr),
					Location:    "sc-cn-rack",
				})
			return err
		}},
		{"CreateStoragePool", func() error {
			_, err := env.srv.CreateStoragePool(env.ctx,
				sptDefaultSpec("sc-pool").req(env.name))
			return err
		}},
		{"GrowSlice", func() error {
			// No SpRev: GW6 is presence-based, so a request that carries no
			// token is never compared and no stale-revision refusal can
			// stand in for the conf's (§0 #7).
			_, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				SliceId:     sliceId,
				// §8.5 refuses a data grow that names no ext_cnt before it
				// reads anything, so the request has to carry one to reach
				// the conf at all. What it grows by is the slice's own
				// allocation unit either way (D-E).
				ExtCnt: 1,
			})
			return err
		}},
		{"CreateCntlr", func() error {
			_, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				// §8.4's default slot list is [0..7] and CreateStoragePool
				// hands its idx-th cntlr slots[idx], so the fixture SP's two
				// cntlrs hold 0 and 1 and slot 2 passes the §11.8 rules.
				CntlidSlot: 2,
			})
			return err
		}},
		{"CreateSpareLeg", func() error {
			_, err := env.srv.CreateSpareLeg(env.ctx,
				&pb.CreateSpareLegRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					GrpId:       grpId,
				})
			return err
		}},
		{"CreateMigration", func() error {
			_, err := env.srv.CreateMigration(env.ctx,
				&pb.CreateMigrationRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					MigrName:    "sc-migr",
					SrcSideId:   srcSideId,
				})
			return err
		}},
	}

	for _, tc := range []struct {
		name string
		zero func(cc *pb.ClusterConf)
		want string
	}{
		{
			"extent_size",
			func(cc *pb.ClusterConf) { cc.GetDnBinConf().ExtentSize = 0 },
			scMsgExtentSize,
		},
		{
			"dn_batch_size",
			func(cc *pb.ClusterConf) { cc.GetAllocConf().DnBatchSize = 0 },
			scMsgDnBatchSize,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scPutCc(env, scSparseCc(env, tc.zero))
			scRefusals(t, env, tc.want, cases)
		})
	}

	scPutCc(env, env.cc)
	scReplay(t, cases)
}

// TestStoredClusterConfRefusalNamesTheZeroedMember walks three different
// members a sparse ClusterConf can carry — a missing size, a collapsed ladder
// and an out-of-range interval — and pins the sentence each produces, so that
// the refusal names the member that is wrong rather than the first member the
// validator looks at.
//
// One RPC is enough here: the sentence is the validator's, and every conf gate
// in the gateway hands it on the same way, as `errAborted("%v", err)` — grep
// Validate{Cluster,Bdev}Conf across the package and each of the eleven hits is
// followed by exactly that line. CreateStoragePool is the one chosen because
// it reads the whole conf on its way through — the ladder and the batch sizes
// in the allocator, the extent size in the STM for §3.6.
//
// The ladder row is what a genuinely SPARSE conf looks like rather than a
// hand-zeroed one: bin1_shift's §7 constant is 4 and bin0's is 0, so a
// message written by something that skipped the resolve comes back with the
// ladder collapsed at the bottom rather than merely missing a number.
func TestStoredClusterConfRefusalNamesTheZeroedMember(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	for _, tc := range []struct {
		name string
		zero func(cc *pb.ClusterConf)
		want string
	}{
		{
			"extent_size",
			func(cc *pb.ClusterConf) { cc.GetDnBinConf().ExtentSize = 0 },
			scMsgExtentSize,
		},
		{
			"bin ladder",
			func(cc *pb.ClusterConf) { cc.GetDnBinConf().Bin1Shift = 0 },
			scMsgLadder,
		},
		{
			"cntlr_interval",
			func(cc *pb.ClusterConf) {
				cc.GetHealthCheckConf().CntlrInterval = 0
			},
			scMsgCntlrInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scPutCc(env, scSparseCc(env, tc.zero))
			before := env.dump()
			_, err := env.srv.CreateStoragePool(env.ctx,
				sptDefaultSpec(sptSpName).req(env.name))
			wantCode(t, err, codes.Aborted, tc.name)
			if got := status.Convert(err).Message(); got != tc.want {
				t.Errorf("message %q, want %q", got, tc.want)
			}
			scWantUntouched(t, before, env.dump())
		})
	}
	// The cluster is left repaired, and the SP every row above was refused
	// creates: the rows refused a conf, not a request.
	scPutCc(env, env.cc)
	env.createSp(sptDefaultSpec(sptSpName))
}

// ---------------------------------------------------------------------------
// The stored SpConf.bdev_conf (GW11, §7)
// ---------------------------------------------------------------------------

// TestStoredSpBdevConfZeroIsRefusedByEveryReader is the SP-side mirror: the
// bdev_conf CreateStoragePool resolved, written back sparse, refused by the
// two RPCs that compute from an SP's own geometry.
//
// CreateThinDevice reads it for the stripe it sizes against (§8.7) and
// GrowSlice for the §3.6 layout of the group it adds, and each has its own
// gate — GrowSlice's sits right below the ClusterConf gate the test above
// drives, so the two tests together cover both of its checks.
//
// Two members are planted in turn. `dm_raid0_conf.stripe_size` is the §9
// fixture's, and it is also the member that makes the gate's placement
// visible: CreateThinDevice's own next check is `unit == 0`, so without the
// gate a zero stripe would come back as the SP "has no slice" instead of as
// the conf fault it is. `bitmap_chunk_block_cnt` is the member nobody named —
// sptDefaultSpec's request chooses the md-raid1 KIND and nothing else, and
// the fixture cluster has no redund_conf to inherit a count from, so the
// stored 128 came from the §7 constant alone (its write side is
// TestCreateStoragePoolResolvesTheMergedBdevConf) — which makes a zero there
// exactly what a pre-GW11 write left behind.
func TestStoredSpBdevConfZeroIsRefusedByEveryReader(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptDefaultSpec(sptSpName))
	healthy := env.spConf(sptSpName)
	sliceId := healthy.GetSliceIdList()[0]

	cases := []scCase{
		{"CreateThinDevice", func() error {
			_, err := env.srv.CreateThinDevice(env.ctx,
				&pb.CreateThinDeviceRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					TdName:      "sc-td",
					// slice_cnt 2 x the stored 64 KiB stripe = 128 KiB, and 1
					// GiB is 8192 of those: a size the §8.7 rule accepts, so
					// the only thing left to refuse it is the conf.
					Size: 1 << 30,
				})
			return err
		}},
		{"GrowSlice", func() error {
			// ext_cnt as in the ClusterConf test above: §8.5's pure check
			// runs before any read and would otherwise refuse this request
			// as INVALID_ARGUMENT without ever reaching the conf.
			_, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				SliceId:     sliceId,
				ExtCnt:      1,
			})
			return err
		}},
	}

	for _, tc := range []struct {
		name string
		zero func(conf *pb.SpConf)
		want string
	}{
		{
			"stripe_size",
			func(conf *pb.SpConf) {
				conf.GetBdevConf().GetDmRaid0Conf().StripeSize = 0
			},
			scMsgStripeSize,
		},
		{
			"bitmap_chunk_block_cnt",
			func(conf *pb.SpConf) {
				conf.GetBdevConf().GetRedundConf().
					GetRedundMdRaid1().BitmapChunkBlockCnt = 0
			},
			scMsgChunkBlockCnt,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scPutSpConf(env, scSparseSpConf(healthy, tc.zero))
			scRefusals(t, env, tc.want, cases)
		})
	}

	scPutSpConf(env, healthy)
	scReplay(t, cases)
}

// TestStoredSpBdevConfRefusalNamesTheZeroedMember walks all four defaultable
// members of a stored bdev_conf and pins the sentence each produces, through
// the one RPC whose whole geometry is the SP's own conf.
//
// All four rows are here because the fourth is not like the other three:
// bitmap_chunk_block_cnt exists only under the md-raid1 arm of the redund_conf
// oneof, so a table that stopped at the three always-present members would
// leave the one conditional branch of ValidateBdevConf untested through any
// RPC. The three unconditional rows in turn are what say the refusal names the
// member that is zero rather than the first member of the message.
func TestStoredSpBdevConfRefusalNamesTheZeroedMember(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptDefaultSpec(sptSpName))
	healthy := env.spConf(sptSpName)
	for _, tc := range []struct {
		name string
		zero func(conf *pb.SpConf)
		want string
	}{
		{
			"data_block_size",
			func(conf *pb.SpConf) {
				conf.GetBdevConf().GetDmPoolConf().DataBlockSize = 0
			},
			scMsgDataBlockSize,
		},
		{
			"low_water_mark_pct",
			func(conf *pb.SpConf) {
				conf.GetBdevConf().GetDmPoolConf().LowWaterMarkPct = 0
			},
			scMsgLowWaterMark,
		},
		{
			"stripe_size",
			func(conf *pb.SpConf) {
				conf.GetBdevConf().GetDmRaid0Conf().StripeSize = 0
			},
			scMsgStripeSize,
		},
		{
			"bitmap_chunk_block_cnt",
			func(conf *pb.SpConf) {
				conf.GetBdevConf().GetRedundConf().
					GetRedundMdRaid1().BitmapChunkBlockCnt = 0
			},
			scMsgChunkBlockCnt,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scPutSpConf(env, scSparseSpConf(healthy, tc.zero))
			before := env.dump()
			_, err := env.srv.CreateThinDevice(env.ctx,
				&pb.CreateThinDeviceRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					TdName:      "sc-td",
					Size:        1 << 30,
				})
			wantCode(t, err, codes.Aborted, tc.name)
			if got := status.Convert(err).Message(); got != tc.want {
				t.Errorf("message %q, want %q", got, tc.want)
			}
			scWantUntouched(t, before, env.dump())
		})
	}
	// Repaired, the thin device every row above was refused is created: the
	// rows refused a conf, not a request.
	scPutSpConf(env, healthy)
	if _, err := env.srv.CreateThinDevice(env.ctx,
		&pb.CreateThinDeviceRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			TdName:      "sc-td",
			Size:        1 << 30,
		}); err != nil {
		t.Errorf("CreateThinDevice after the stored conf was repaired: %v", err)
	}
}
