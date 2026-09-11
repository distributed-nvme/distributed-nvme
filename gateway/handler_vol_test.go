package gateway

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md §9.3 for the volume half of the API: architecture.md
// §8.7 (thin devices), §8.8 (subsystems and namespaces), §8.9 (clones), §8.10
// (transfers), §8.11 (migrations) and §8.12 (spare legs).
//
// Every test drives a real Server over the shared etcd of etcdenv_test.go and
// then asserts the EXACT store state — keys, ids, name lists, capacity keys
// and the §5.5 revisions — rather than only the reply, because a handler that
// replies correctly and writes the wrong key set is exactly the bug §9.3 asks
// these tests to catch. Refusals are asserted the other way round: the code
// AND the fact that nothing moved, which is the "an error out of the closure
// aborts the transaction uncommitted" contract (EU4) every §8 precondition
// rests on.
//
// The fixture is written with plain puts instead of the create RPCs on
// purpose. CreateDiskNode / CreateControllerNode reach an agent (AG1) and
// CreateStoragePool runs the allocator, so building the SP through them would
// make every test below depend on §8.2/§8.4 as well as on the rule it pins;
// the agent paths have their own tests (§9.4).

// ---------------------------------------------------------------------------
// The fixture: one md-raid1 SP with two cntlrs and one slice
// ---------------------------------------------------------------------------

const (
	volSpName    = "vol-pool"
	volShard     = uint32(5)
	volSpId      = uint64(0x200)
	volNextId    = uint64(1000)
	volNextDevId = uint32(1)
	// volSlot is the cntlid slot every fixture side holds, volSlotAlt the
	// first different entry of cntlid_slot_list — which is what [D-I] makes
	// a migration destination take.
	volSlot    = uint32(3)
	volSlotAlt = uint32(4)

	volNqn    = "nqn.2024-01.io.dnv:vol-a"
	volNqnB   = "nqn.2024-01.io.dnv:vol-b"
	volSrcNqn = "nqn.2024-01.io.dnv:src"
	volHostA  = "nqn.2024-01.io.dnv:host-a"
	volHostB  = "nqn.2024-01.io.dnv:host-b"
	volHostC  = "nqn.2024-01.io.dnv:host-c"

	volCntlrA = uint64(201)
	volCntlrB = uint64(202)

	volSliceId   = uint64(301)
	volMetaGrpId = uint64(401)
	volDataGrpId = uint64(402)
	volMetaLegA  = uint64(411)
	volMetaLegB  = uint64(412)
	volDataLegA  = uint64(421)
	volDataLegB  = uint64(422)
	volMetaSideA = uint64(431)
	volMetaSideB = uint64(432)
	volDataSideA = uint64(441)
	volDataSideB = uint64(442)

	volDnA = "dn-a:9000"
	volDnB = "dn-b:9000"
	volDnC = "dn-c:9000"
	volDnD = "dn-d:9000"
	volCnA = "cn-a:9000"
	volCnB = "cn-b:9000"

	volExtSize   = uint64(1) << 30
	volPoolBlock = uint64(1) << 20
	// volStripe is deliberately NOT DefaultDmRaid0StripeSize: the size rule
	// of §8.7 must be computed from the SP's STORED geometry, so a fixture
	// that happened to match the constant could not tell the two apart.
	volStripe     = uint64(128) * 1024
	volMetaExtCnt = uint64(1)
	volDataExtCnt = uint64(4)
	volDnFree     = uint64(100)
	volCnFree     = uint64(50)
	// volTdSize is 8192 x (slice_cnt 1 x volStripe), a legal thin-device size
	// for this SP.
	volTdSize = uint64(1) << 30
)

// volClusterSeq numbers the cluster names the fixture hands out, so that two
// tests — and two iterations of one test under -count=N — never share a
// cluster_id and the shared etcd needs no cleanup (§5.2).
var volClusterSeq atomic.Uint64

// volTrConf is one node's transport configuration. It is derived from the
// node's addr_port so that a CdcEntry's nvme_tr_conf_list can be checked entry
// by entry rather than only by length.
func volTrConf(addrPort string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  addrPort,
		TrSvcId: "4420",
	}
}

// volSpConf is the SpConf the fixture writes: an md-raid1 SP with two cntlrs,
// one slice and no thin device, subsystem, clone, transfer or migration of its
// own — every test creates exactly the objects its rule needs.
func volSpConf() *pb.SpConf {
	return &pb.SpConf{
		SpId:      volSpId,
		ShardCode: volShard,
		NextId:    volNextId,
		NextDevId: volNextDevId,
		BdevConf: &pb.BdevConf{
			DmPoolConf:  &pb.DmPoolConf{DataBlockSize: volPoolBlock},
			DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: volStripe},
			RedundConf: &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundMdRaid1{
					RedundMdRaid1: &pb.RedundMdRaid1{
						BitmapChunkBlockCnt: common.DefaultChunkBlockCnt,
					},
				},
			},
		},
		EventThreshold: &pb.EventThreshold{},
		CntlidSlotList: []uint32{volSlot, volSlotAlt, 5},
		SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
		CntlrIdList:    []uint64{volCntlrA, volCntlrB},
		SliceIdList:    []uint64{volSliceId},
	}
}

// volSide builds one side of the fixture slice.
func volSide(sideId uint64, addrPort string) *pb.Side {
	return &pb.Side{
		SideId:      sideId,
		AddrPort:    addrPort,
		CntlidSlot:  volSlot,
		NvmeTrConf:  volTrConf(addrPort),
		Provisioned: true,
	}
}

// volSlice is the fixture's single slice: one meta group and one data group,
// each an md-raid1 pair on dn-a and dn-b. dn-c and dn-d are therefore the only
// destinations a migration or a spare leg of either group can land on (§6.5),
// which is what makes the allocating tests below deterministic.
func volSlice() *pb.Slice {
	return &pb.Slice{
		SliceIdx: 0,
		MetaGrpList: []*pb.Group{{
			GrpId:      volMetaGrpId,
			ExtCnt:     volMetaExtCnt,
			MetaBlocks: 3,
			DataBlocks: 1021,
			LegList: []*pb.Leg{
				{
					LegId:    volMetaLegA,
					LegIdx:   0,
					SideList: []*pb.Side{volSide(volMetaSideA, volDnA)},
				},
				{
					LegId:    volMetaLegB,
					LegIdx:   1,
					SideList: []*pb.Side{volSide(volMetaSideB, volDnB)},
				},
			},
		}},
		DataGrpList: []*pb.Group{{
			GrpId:      volDataGrpId,
			ExtCnt:     volDataExtCnt,
			MetaBlocks: 3,
			DataBlocks: 4093,
			LegList: []*pb.Leg{
				{
					LegId:    volDataLegA,
					LegIdx:   0,
					SideList: []*pb.Side{volSide(volDataSideA, volDnA)},
				},
				{
					LegId:    volDataLegB,
					LegIdx:   1,
					SideList: []*pb.Side{volSide(volDataSideB, volDnB)},
				},
			},
		}},
	}
}

// volEnv is one test's cluster, SP and Server.
type volEnv struct {
	t       *testing.T
	ctx     context.Context
	cli     *etcdutil.Client
	srv     *Server
	cluster string
	cid     uint64
	cc      *pb.ClusterConf
}

// newVolEnv writes the whole fixture and returns the environment the tests
// drive. It skips when no etcd binary is available (EU7).
//
// The cluster is name-keyed and its id is derived (§5.2), so a unique name and
// a unique creation_epoch together give the test its own key space; testCid's
// per-invocation counter is what makes the epoch unique across -count=N.
func newVolEnv(t *testing.T) *volEnv {
	t.Helper()
	cli := newTestClient(t)
	name := fmt.Sprintf("vol-cluster-%d", volClusterSeq.Add(1))
	epoch := testCid(t)
	env := &volEnv{
		t:       t,
		ctx:     context.Background(),
		cli:     cli,
		srv:     NewServer(cli),
		cluster: name,
		cid:     model.ClusterId(name, epoch),
		cc: &pb.ClusterConf{
			CreationEpoch: epoch,
			DnBinConf:     &pb.DnBinConf{ExtentSize: volExtSize},
		},
	}
	mustPut(t, cli, model.ClusterConfKey(name), env.cc)
	mustPut(t, cli, model.SpConfKey(env.cid, volSpName), volSpConf())
	mustPut(t, cli, model.SpNameKey(env.cid, volSpId), &pb.SpName{
		SpName: volSpName,
	})
	mustPut(t, cli, model.SpRevKey(volShard, env.cid, volSpId), &pb.SpRev{
		SpName:   volSpName,
		Revision: 1,
	})
	mustPut(t, cli, model.SliceKey(env.cid, volSpId, volSliceId), volSlice())
	mustPut(t, cli, model.CntlrKey(env.cid, volSpId, volCntlrA), &pb.Cntlr{
		AddrPort:   volCnA,
		NvmeTrConf: volTrConf(volCnA),
		CntlidSlot: 0,
		Primary:    true,
	})
	mustPut(t, cli, model.CntlrKey(env.cid, volSpId, volCntlrB), &pb.Cntlr{
		AddrPort:   volCnB,
		NvmeTrConf: volTrConf(volCnB),
		CntlidSlot: 1,
	})
	// dn-a and dn-b already carry the sides of both groups, so their
	// side_ptr_list is what a release must shrink; dn-c and dn-d are empty
	// and are what an allocation may take.
	env.putDn(volDnA, 700, 0, []*pb.SidePointer{
		{SpId: volSpId, LegId: volMetaLegA, SideId: volMetaSideA},
		{SpId: volSpId, LegId: volDataLegA, SideId: volDataSideA},
	})
	env.putDn(volDnB, 701, 1, []*pb.SidePointer{
		{SpId: volSpId, LegId: volMetaLegB, SideId: volMetaSideB},
		{SpId: volSpId, LegId: volDataLegB, SideId: volDataSideB},
	})
	env.putDn(volDnC, 702, 2, nil)
	env.putDn(volDnD, 703, 3, nil)
	env.putCn(volCnA, 800, 0)
	env.putCn(volCnB, 801, 1)
	return env
}

// putDn writes one DnConf, the capacity key the §5.6 presence rule implies for
// it and its DnRev, all consistently.
func (e *volEnv) putDn(
	addrPort string,
	dnId uint64,
	shard uint32,
	ptrs []*pb.SidePointer,
) {
	e.t.Helper()
	dn := &pb.DnConf{
		DnId:        dnId,
		ShardCode:   shard,
		NvmeTrConf:  volTrConf(addrPort),
		Location:    addrPort,
		SidePtrList: ptrs,
		TotalExtCnt: 1000,
		FreeExtCnt:  volDnFree,
	}
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addrPort), dn)
	mustPut(e.t, e.cli, model.DnRevKey(shard, e.cid, dnId), &pb.DnRev{
		AddrPort: addrPort,
		Revision: 1,
	})
	mustPut(
		e.t, e.cli,
		e.dnCapKey(addrPort, volDnFree),
		&pb.DnCapacity{Location: addrPort},
	)
}

// putCn writes one CnConf, its capacity key and its CnRev. No RPC of this file
// charges a CN — a thin device, a subsystem, a clone, a transfer, a migration
// destination and a spare leg all leave every cntlr's footprint unchanged
// (§8.4, §8.6, §8.12) — so these records exist only to be asserted UNTOUCHED.
func (e *volEnv) putCn(addrPort string, cnId uint64, shard uint32) {
	e.t.Helper()
	cn := &pb.CnConf{
		CnId:        cnId,
		ShardCode:   shard,
		NvmeTrConf:  volTrConf(addrPort),
		Location:    addrPort,
		TotalExtCnt: 1000,
		FreeExtCnt:  volCnFree,
	}
	mustPut(e.t, e.cli, model.CnConfKey(e.cid, addrPort), cn)
	mustPut(e.t, e.cli, model.CnRevKey(shard, e.cid, cnId), &pb.CnRev{
		AddrPort: addrPort,
		Revision: 1,
	})
	mustPut(
		e.t, e.cli,
		model.CnCapacityKey(e.cid, volCnFree, addrPort),
		&pb.CnCapacity{Location: addrPort},
	)
}

// dnCapKey is the capacity key a DN with that many free extents has (§5.6,
// §6.2), which is what the allocating tests below assert moved.
func (e *volEnv) dnCapKey(addrPort string, freeExt uint64) string {
	e.t.Helper()
	binIdx, ok := model.DnBinIdx(freeExt, e.cc.GetDnBinConf())
	if !ok {
		e.t.Fatalf("free_ext_cnt %d has no bin", freeExt)
	}
	return model.DnCapacityKey(e.cid, binIdx, freeExt, addrPort)
}

// get reads one key, failing the test when it is absent.
func (e *volEnv) get(key string, msg proto.Message) {
	e.t.Helper()
	found, err := e.cli.Get(e.ctx, key, msg)
	if err != nil {
		e.t.Fatalf("Get %s: %v", key, err)
	}
	if !found {
		e.t.Fatalf("Get %s: not found", key)
	}
}

// exists reports whether a key is present. msg only has to be a type the value
// decodes into, which is why every caller passes the key's own message type.
func (e *volEnv) exists(key string, msg proto.Message) bool {
	e.t.Helper()
	found, err := e.cli.Get(e.ctx, key, msg)
	if err != nil {
		e.t.Fatalf("Get %s: %v", key, err)
	}
	return found
}

func (e *volEnv) spConf() *pb.SpConf {
	e.t.Helper()
	conf := &pb.SpConf{}
	e.get(model.SpConfKey(e.cid, volSpName), conf)
	return conf
}

func (e *volEnv) putSpConf(conf *pb.SpConf) {
	e.t.Helper()
	mustPut(e.t, e.cli, model.SpConfKey(e.cid, volSpName), conf)
}

func (e *volEnv) spRev() uint64 {
	e.t.Helper()
	rev := &pb.SpRev{}
	e.get(model.SpRevKey(volShard, e.cid, volSpId), rev)
	if rev.GetSpName() != volSpName {
		e.t.Fatalf("sp_rev handle: got %q, want %q",
			rev.GetSpName(), volSpName)
	}
	return rev.GetRevision()
}

// token is the SpRev message a mutator has to carry to be let through right
// now: GW6 compares only `revision`, and only when the message is there at all,
// so this is the one value a present token may hold (§5.5, §0 #7).
func (e *volEnv) token() *pb.SpRev {
	e.t.Helper()
	return &pb.SpRev{Revision: e.spRev()}
}

func (e *volEnv) slice() *pb.Slice {
	e.t.Helper()
	slice := &pb.Slice{}
	e.get(model.SliceKey(e.cid, volSpId, volSliceId), slice)
	return slice
}

func (e *volEnv) putSlice(slice *pb.Slice) {
	e.t.Helper()
	mustPut(e.t, e.cli, model.SliceKey(e.cid, volSpId, volSliceId), slice)
}

func (e *volEnv) td(name string) *pb.ThinDevice {
	e.t.Helper()
	td := &pb.ThinDevice{}
	e.get(model.ThinDeviceKey(e.cid, volSpId, name), td)
	return td
}

func (e *volEnv) subsystem(nqn string) *pb.Subsystem {
	e.t.Helper()
	subsystem := &pb.Subsystem{}
	e.get(model.SubsystemKey(e.cid, volSpId, nqn), subsystem)
	return subsystem
}

func (e *volEnv) cdcEntry(ssId uint64) *pb.CdcEntry {
	e.t.Helper()
	entry := &pb.CdcEntry{}
	e.get(model.CdcEntryKey(e.cid, volShard, volSpId, ssId), entry)
	return entry
}

func (e *volEnv) clone(name string) *pb.Clone {
	e.t.Helper()
	clone := &pb.Clone{}
	e.get(model.CloneKey(e.cid, volSpId, name), clone)
	return clone
}

func (e *volEnv) migration(name string) *pb.Migration {
	e.t.Helper()
	migr := &pb.Migration{}
	e.get(model.MigrationKey(e.cid, volSpId, name), migr)
	return migr
}

func (e *volEnv) dn(addrPort string) *pb.DnConf {
	e.t.Helper()
	dn := &pb.DnConf{}
	e.get(model.DnConfKey(e.cid, addrPort), dn)
	return dn
}

func (e *volEnv) dnRev(addrPort string) uint64 {
	e.t.Helper()
	dn := e.dn(addrPort)
	rev := &pb.DnRev{}
	e.get(model.DnRevKey(dn.GetShardCode(), e.cid, dn.GetDnId()), rev)
	return rev.GetRevision()
}

func (e *volEnv) cnRev(addrPort string) uint64 {
	e.t.Helper()
	cn := &pb.CnConf{}
	e.get(model.CnConfKey(e.cid, addrPort), cn)
	rev := &pb.CnRev{}
	e.get(model.CnRevKey(cn.GetShardCode(), e.cid, cn.GetCnId()), rev)
	return rev.GetRevision()
}

// putTd writes one ThinDevice row and adds it to td_name_list, the shape every
// test that needs a pre-existing device starts from. It does not bump SpRev:
// the fixture is state, not a mutation the workers have to see.
func (e *volEnv) putTd(name string, tdId uint64, devId uint32, oriId uint32,
	created bool) {
	e.t.Helper()
	mustPut(e.t, e.cli, model.ThinDeviceKey(e.cid, volSpId, name),
		&pb.ThinDevice{
			TdId:    tdId,
			DevId:   devId,
			OriId:   oriId,
			Size:    volTdSize,
			Created: created,
		})
	conf := e.spConf()
	conf.TdNameList = append(conf.GetTdNameList(), name)
	e.putSpConf(conf)
}

// putSubsystem writes one Subsystem row, its CdcEntry and its nqn_list entry.
func (e *volEnv) putSubsystem(
	nqn string,
	ssId uint64,
	nsList []*pb.Namespace,
) {
	e.t.Helper()
	mustPut(e.t, e.cli, model.SubsystemKey(e.cid, volSpId, nqn),
		&pb.Subsystem{
			SsId:   ssId,
			Serial: fmt.Sprintf(common.IdKeyFmt, ssId),
			Model:  subsystemModel,
			NsList: nsList,
		})
	mustPut(e.t, e.cli, model.CdcEntryKey(e.cid, volShard, volSpId, ssId),
		&pb.CdcEntry{
			Nqn: nqn,
			NvmeTrConfList: []*pb.NvmeTrConf{
				volTrConf(volCnA), volTrConf(volCnB),
			},
		})
	conf := e.spConf()
	conf.NqnList = append(conf.GetNqnList(), nqn)
	e.putSpConf(conf)
}

// wantUntouched is the "a refusal writes nothing" assertion of the handler
// pattern: an error returned from an STM closure aborts the transaction
// uncommitted (EU4), so neither the SP's revision nor any counter or name list
// of its SpConf may have moved.
func (e *volEnv) wantUntouched(before *pb.SpConf, beforeRev uint64) {
	e.t.Helper()
	if got := e.spRev(); got != beforeRev {
		e.t.Errorf("sp_rev: got %d, want %d (a refusal must not bump)",
			got, beforeRev)
	}
	if got := e.spConf(); !proto.Equal(got, before) {
		e.t.Errorf("sp_conf moved on a refusal:\n got %v\nwant %v",
			got, before)
	}
}

// volWantCode asserts the GW7 status code of a refusal and hands the message
// back, for the callers that also pin a normative sentence.
func volWantCode(t *testing.T, err error, want codes.Code) string {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got a nil error", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("want a gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != want {
		t.Fatalf("code: got %s (%v), want %s", st.Code(), err, want)
	}
	return st.Message()
}

// volGrpOf is the group of the fixture slice with that id.
func volGrpOf(t *testing.T, slice *pb.Slice, grpId uint64) *pb.Group {
	t.Helper()
	for _, grp := range append(
		append([]*pb.Group(nil), slice.GetMetaGrpList()...),
		slice.GetDataGrpList()...,
	) {
		if grp.GetGrpId() == grpId {
			return grp
		}
	}
	t.Fatalf("group %d is not in the slice", grpId)
	return nil
}

// volDnSelector pins the allocator to one disk node. Every allocating RPC of
// this file scans the DN capacity index outside its transaction (GW9) and then
// PickRandoms from what it found, so a test that wants an EXACT destination
// has to leave exactly one candidate — which is what a white list does (§6.3).
func volDnSelector(addrPort string) *pb.NodeSelector {
	return &pb.NodeSelector{WhiteList: []string{addrPort}}
}

// relocateDn moves one DN into another failure domain. Both copies have to
// move: the DnConf the §6.5 location exclusion reads, and the copy the
// capacity-key value carries so that the scan needs no point read ([D5]).
func (e *volEnv) relocateDn(addrPort string, location string) {
	e.t.Helper()
	dn := e.dn(addrPort)
	dn.Location = location
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addrPort), dn)
	mustPut(e.t, e.cli, e.dnCapKey(addrPort, dn.GetFreeExtCnt()),
		&pb.DnCapacity{Location: location})
}

// setDnFree rewrites one DN's free_ext_cnt and moves its capacity key with it
// (§5.6). It is how a test decides which candidate the scan sees FIRST: one
// bin is one descending range over free counts (§6.3), so the emptier DN can
// only be reached past the fuller one.
func (e *volEnv) setDnFree(addrPort string, freeExt uint64) {
	e.t.Helper()
	dn := e.dn(addrPort)
	oldKey := e.dnCapKey(addrPort, dn.GetFreeExtCnt())
	dn.FreeExtCnt = freeExt
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addrPort), dn)
	if err := e.cli.Delete(e.ctx, oldKey); err != nil {
		e.t.Fatalf("Delete %s: %v", oldKey, err)
	}
	mustPut(e.t, e.cli, e.dnCapKey(addrPort, freeExt),
		&pb.DnCapacity{Location: dn.GetLocation()})
}

// dropDn leaves the cluster without that DN as an allocator sees it: no
// capacity key, so it is no candidate, and no conf, so it contributes no
// location either (§6.5).
func (e *volEnv) dropDn(addrPort string) {
	e.t.Helper()
	dn := e.dn(addrPort)
	for _, key := range []string{
		model.DnConfKey(e.cid, addrPort),
		e.dnCapKey(addrPort, dn.GetFreeExtCnt()),
	} {
		if err := e.cli.Delete(e.ctx, key); err != nil {
			e.t.Fatalf("Delete %s: %v", key, err)
		}
	}
}

// volDnCFree is what volTwoDomainEnv leaves dn-c holding: more than any other
// DN, so the descending scan reaches it FIRST and a placement on dn-d can only
// be the location rule's doing, never the index order's.
const volDnCFree = volDnFree * 2

// volTwoDomainEnv is the §6.5 two-tier fixture: dn-c joins dn-a's failure
// domain — dn-a carries a side of every group — so dn-d is the only candidate
// in a domain of its own, and dn-c is the one the scan would otherwise hand
// back first.
//
// The default dn_batch_size (16) is left alone on purpose: the tier-2 trigger
// is RequiredCnt — one DN for both of these RPCs — and not the oversampled
// candCnt, so tier 1 finding its single candidate is enough to settle the
// placement. That single candidate is also what makes the assertion exact:
// dn-a and dn-b are black-listed, dn-c shares dn-a's domain, so PickRandom
// draws dn-d out of a one-entry list.
func volTwoDomainEnv(t *testing.T) *volEnv {
	t.Helper()
	env := newVolEnv(t)
	env.relocateDn(volDnC, volDnA)
	env.setDnFree(volDnC, volDnCFree)
	return env
}

// volTier1DrawCnt is how often the two tier-1 assertions below re-run against a
// FRESH fixture. At the gateway the §6.5 requiredCnt is observed only through
// the pick, and PickRandom draws uniformly, so the exact regression the
// requiredCnt trigger exists to refuse — handing FindDnCandidatesAntiAffine the
// oversampled scan width (Legs × dn_batch_size) where the DNs to place belong
// — does not turn the assertion red on its own: it drags tier 1's single find
// into tier 2, which merges the same-domain DN behind it, and a one-of-two
// draw then lands on the right DN half the time. Every repeat is an
// independent draw against its own cluster, so twelve of them leave that
// regression 2^-12 of a chance to stay green, at about half a second per test.
//
// The tier-2 assertions need no repeat: with dn-d dropped the scan has exactly
// one candidate and the draw is forced.
const volTier1DrawCnt = 12

// ---------------------------------------------------------------------------
// §8.7 CreateThinDevice
// ---------------------------------------------------------------------------

// TestCreateThinDeviceWritesRowAndAdvancesCounters pins §8.7's whole write
// set: the ThinDevice row (created = false, ori_id 0 for a fresh device), the
// SpConf whose td_name_list gained the name and whose next_id and next_dev_id
// each advanced by exactly one, and the single SpRev bump they share (§5.5).
func TestCreateThinDeviceWritesRowAndAdvancesCounters(t *testing.T) {
	env := newVolEnv(t)
	reply, err := env.srv.CreateThinDevice(env.ctx, &pb.CreateThinDeviceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		TdName:      "td-a",
		Size:        volTdSize,
	})
	if err != nil {
		t.Fatalf("CreateThinDevice: %v", err)
	}
	if reply.GetTdId() != volNextId || reply.GetDevId() != 1 {
		t.Errorf("reply: got td_id %d dev_id %d, want %d and 1",
			reply.GetTdId(), reply.GetDevId(), volNextId)
	}
	want := &pb.ThinDevice{
		TdId:    volNextId,
		DevId:   1,
		OriId:   0,
		Size:    volTdSize,
		Created: false,
	}
	if got := env.td("td-a"); !proto.Equal(got, want) {
		t.Errorf("thin device: got %v, want %v", got, want)
	}
	conf := env.spConf()
	if len(conf.GetTdNameList()) != 1 || conf.GetTdNameList()[0] != "td-a" {
		t.Errorf("td_name_list: got %v, want [td-a]", conf.GetTdNameList())
	}
	if conf.GetNextId() != volNextId+1 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), volNextId+1)
	}
	if conf.GetNextDevId() != 2 {
		t.Errorf("next_dev_id: got %d, want 2", conf.GetNextDevId())
	}
	if got := env.spRev(); got != 2 {
		t.Errorf("sp_rev: got %d, want 2", got)
	}
}

// TestCreateThinDeviceDevIdSequence pins the §5.4 dev_id sequence: dev_ids come
// from next_dev_id, start at 1 and are never reused, and each device also draws
// exactly one per-SP id from next_id. Both counters have to advance in lock
// step across a run of creates, because dev_id is the id a snapshot's ori_id
// points at and a repeat would silently re-parent it.
func TestCreateThinDeviceDevIdSequence(t *testing.T) {
	env := newVolEnv(t)
	for idx, name := range []string{"td-a", "td-b", "td-c"} {
		reply, err := env.srv.CreateThinDevice(
			env.ctx, &pb.CreateThinDeviceRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				TdName:      name,
				Size:        volTdSize,
			})
		if err != nil {
			t.Fatalf("CreateThinDevice %s: %v", name, err)
		}
		wantTdId := volNextId + uint64(idx)
		wantDevId := uint32(idx) + 1
		if reply.GetTdId() != wantTdId || reply.GetDevId() != wantDevId {
			t.Errorf("%s: got td_id %d dev_id %d, want %d and %d",
				name, reply.GetTdId(), reply.GetDevId(),
				wantTdId, wantDevId)
		}
		if got := env.td(name).GetDevId(); got != wantDevId {
			t.Errorf("%s stored dev_id: got %d, want %d",
				name, got, wantDevId)
		}
	}
	conf := env.spConf()
	if conf.GetNextId() != volNextId+3 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), volNextId+3)
	}
	if conf.GetNextDevId() != 4 {
		t.Errorf("next_dev_id: got %d, want 4", conf.GetNextDevId())
	}
	if got := env.spRev(); got != 4 {
		t.Errorf("sp_rev: got %d, want 4 (one bump per create)", got)
	}
}

// TestCreateThinDeviceSnapshotGateConsumesNothing is §8.7's snapshot gate: an
// origin whose created flag the sp-worker has not set yet refuses the request
// with FAILED_PRECONDITION and must write LITERALLY nothing — no row, no
// td_name_list entry, no next_id or next_dev_id consumption and no SpRev bump.
//
// The counters are the point. Ids are never reused (§5.4), so an id burned by
// a refusal is visible for the life of the SP; the check therefore sits before
// the minter is ever built.
func TestCreateThinDeviceSnapshotGateConsumesNothing(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("origin", 900, 7, 0, false)
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.CreateThinDevice(env.ctx, &pb.CreateThinDeviceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		TdName:      "snap",
		OriName:     "origin",
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "is not created yet") {
		t.Errorf("message %q must carry §8.7's normative refusal", msg)
	}
	env.wantUntouched(before, beforeRev)
	if env.exists(
		model.ThinDeviceKey(env.cid, volSpId, "snap"), &pb.ThinDevice{},
	) {
		t.Errorf("the refused snapshot must not have a row")
	}
}

// TestCreateThinDeviceSnapshotOfCreatedOrigin is the other half of the gate:
// once the origin is created, the snapshot is written with ori_id = the
// ORIGIN'S dev_id and, per [D-H], inherits the origin's size when the request
// carries none.
func TestCreateThinDeviceSnapshotOfCreatedOrigin(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("origin", 900, 7, 0, true)
	reply, err := env.srv.CreateThinDevice(env.ctx, &pb.CreateThinDeviceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		TdName:      "snap",
		OriName:     "origin",
	})
	if err != nil {
		t.Fatalf("CreateThinDevice: %v", err)
	}
	want := &pb.ThinDevice{
		TdId:    volNextId,
		DevId:   1,
		OriId:   7,
		Size:    volTdSize,
		Created: false,
	}
	if got := env.td("snap"); !proto.Equal(got, want) {
		t.Errorf("snapshot: got %v, want %v", got, want)
	}
	if reply.GetTdId() != volNextId {
		t.Errorf("reply td_id: got %d, want %d", reply.GetTdId(), volNextId)
	}
}

// TestCreateThinDeviceRefusals is the §8.7 error table. Every row asserts the
// code AND that the SP did not move, because each of these refusals returns
// before the first Put and must therefore leave the store byte-identical.
func TestCreateThinDeviceRefusals(t *testing.T) {
	unit := uint64(len(volSpConf().GetSliceIdList())) * volStripe
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv)
		req   *pb.CreateThinDeviceRequest
		want  codes.Code
	}{
		{
			name: "size 0 without an origin",
			req:  &pb.CreateThinDeviceRequest{TdName: "td-a"},
			want: codes.InvalidArgument,
		},
		{
			name: "size is not a multiple of slice_cnt x stripe_size",
			req: &pb.CreateThinDeviceRequest{
				TdName: "td-a", Size: unit + 1,
			},
			want: codes.InvalidArgument,
		},
		{
			name: "empty td_name",
			req:  &pb.CreateThinDeviceRequest{Size: volTdSize},
			want: codes.InvalidArgument,
		},
		{
			name:  "name already taken",
			setup: func(env *volEnv) { env.putTd("td-a", 900, 7, 0, true) },
			req: &pb.CreateThinDeviceRequest{
				TdName: "td-a", Size: volTdSize,
			},
			want: codes.AlreadyExists,
		},
		{
			name: "unknown origin",
			req: &pb.CreateThinDeviceRequest{
				TdName: "snap", OriName: "nope",
			},
			want: codes.NotFound,
		},
		{
			name: "unknown storage pool",
			req: &pb.CreateThinDeviceRequest{
				TdName: "td-a", Size: volTdSize,
			},
			want: codes.NotFound,
		},
		{
			name: "the per-SP thin device ceiling",
			setup: func(env *volEnv) {
				conf := env.spConf()
				for idx := 0; idx < common.MaxTdCntPerSp; idx++ {
					conf.TdNameList = append(
						conf.TdNameList, fmt.Sprintf("filler-%d", idx))
				}
				env.putSpConf(conf)
			},
			req: &pb.CreateThinDeviceRequest{
				TdName: "td-a", Size: volTdSize,
			},
			want: codes.ResourceExhausted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.spConf()
			beforeRev := env.spRev()
			req := proto.Clone(tc.req).(*pb.CreateThinDeviceRequest)
			req.ClusterName = env.cluster
			req.SpRev = &pb.SpRev{Revision: beforeRev}
			if req.GetSpName() == "" && tc.name != "unknown storage pool" {
				req.SpName = volSpName
			}
			if tc.name == "unknown storage pool" {
				req.SpName = "no-such-pool"
			}
			_, err := env.srv.CreateThinDevice(env.ctx, req)
			volWantCode(t, err, tc.want)
			env.wantUntouched(before, beforeRev)
		})
	}
}

// ---------------------------------------------------------------------------
// §8.7 DeleteThinDevice
// ---------------------------------------------------------------------------

// TestDeleteThinDeviceReferenceGuards pins §8.7's three FAILED_PRECONDITION
// guards, each with nothing written: a td that backs a namespace, a td that is
// the destination of a clone, and a td with a snapshot the sp-worker has not
// materialized yet. All three are evaluated from reads made INSIDE the
// deleting transaction, which is what makes a concurrent create conflict
// rather than slip through.
func TestDeleteThinDeviceReferenceGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv)
		want  string
	}{
		{
			name: "backs a namespace",
			setup: func(env *volEnv) {
				env.putSubsystem(volNqn, 501, []*pb.Namespace{{
					NsId: 601, NsIdx: 1, TdId: 900,
				}})
			},
			want: "backs namespace 1 of subsystem",
		},
		{
			name: "is the destination of a clone",
			setup: func(env *volEnv) {
				mustPut(t, env.cli,
					model.CloneKey(env.cid, volSpId, "clone-a"),
					&pb.Clone{CloneId: 701, DstTdId: 900})
				conf := env.spConf()
				conf.CloneNameList = []string{"clone-a"}
				env.putSpConf(conf)
			},
			want: "is the destination of clone clone-a",
		},
		{
			name: "has a snapshot that is not created yet",
			setup: func(env *volEnv) {
				env.putTd("snap", 901, 8, 7, false)
			},
			want: "snapshot snap of vol is not created yet",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putTd("vol", 900, 7, 0, true)
			tc.setup(env)
			before := env.spConf()
			beforeRev := env.spRev()
			_, err := env.srv.DeleteThinDevice(
				env.ctx, &pb.DeleteThinDeviceRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					TdName:      "vol",
				})
			msg := volWantCode(t, err, codes.FailedPrecondition)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message %q must name the blocker %q", msg, tc.want)
			}
			env.wantUntouched(before, beforeRev)
			if !env.exists(
				model.ThinDeviceKey(env.cid, volSpId, "vol"),
				&pb.ThinDevice{},
			) {
				t.Errorf("a refused delete must leave the row")
			}
		})
	}
}

// TestDeleteThinDeviceReportsEverySnapshotBlocker pins the "every blocker, not
// only the first" half of §8.7's third guard: the operator's next step is to
// wait for all of them, so a one-at-a-time refusal would make that a guessing
// game.
func TestDeleteThinDeviceReportsEverySnapshotBlocker(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("snap-a", 901, 8, 7, false)
	env.putTd("snap-b", 902, 9, 7, false)
	_, err := env.srv.DeleteThinDevice(env.ctx, &pb.DeleteThinDeviceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		TdName:      "vol",
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	for _, want := range []string{"snap-a", "snap-b"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q must name %s", msg, want)
		}
	}
}

// TestDeleteThinDeviceHappyPath pins the delete's write set and the two
// snapshots that DO NOT block it: a created snapshot (dm-thin snapshots stay
// valid without their origin) and a snapshot whose ori_id belongs to some
// earlier device of the same name — which is exactly why the guard matches on
// dev_id and not on td_id or name.
func TestDeleteThinDeviceHappyPath(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("snap-created", 901, 8, 7, true)
	env.putTd("snap-of-a-dead-dev", 902, 9, 6, false)
	reply, err := env.srv.DeleteThinDevice(
		env.ctx, &pb.DeleteThinDeviceRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			TdName:      "vol",
		})
	if err != nil {
		t.Fatalf("DeleteThinDevice: %v", err)
	}
	if reply.GetTdId() != 900 {
		t.Errorf("reply td_id: got %d, want 900", reply.GetTdId())
	}
	if env.exists(
		model.ThinDeviceKey(env.cid, volSpId, "vol"), &pb.ThinDevice{},
	) {
		t.Errorf("the row must be gone")
	}
	conf := env.spConf()
	want := []string{"snap-created", "snap-of-a-dead-dev"}
	if fmt.Sprint(conf.GetTdNameList()) != fmt.Sprint(want) {
		t.Errorf("td_name_list: got %v, want %v", conf.GetTdNameList(), want)
	}
	if conf.GetNextId() != volNextId || conf.GetNextDevId() != volNextDevId {
		t.Errorf("a delete must return no id to a counter: %v", conf)
	}
	if got := env.spRev(); got != 2 {
		t.Errorf("sp_rev: got %d, want 2", got)
	}
}

// TestThinDeviceSameNameRecreateTakesFreshIds walks §8.7's same-name recreate:
// a td created, deleted and created again under the SAME name is a DIFFERENT
// device — a fresh td_id and the next dev_id, `created` false again — because
// a delete returns nothing to either counter (§5.4) and the sp-worker's flip
// is a fact about the pool metadata of THAT dev_id.
//
// The last step is why the delete guard matches on dev_id: a snapshot record
// left over from the dead device names a dev_id nothing carries any more, so
// it can never block deleting its namesake, and a guard written against
// ori_name or td_id would refuse this delete for ever.
func TestThinDeviceSameNameRecreateTakesFreshIds(t *testing.T) {
	env := newVolEnv(t)
	create := func() *pb.CreateThinDeviceReply {
		t.Helper()
		reply, err := env.srv.CreateThinDevice(
			env.ctx, &pb.CreateThinDeviceRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				TdName:      "a",
				Size:        volTdSize,
			})
		if err != nil {
			t.Fatalf("CreateThinDevice a: %v", err)
		}
		return reply
	}
	del := func() error {
		t.Helper()
		_, err := env.srv.DeleteThinDevice(
			env.ctx, &pb.DeleteThinDeviceRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				TdName:      "a",
			})
		return err
	}
	first := create()
	if first.GetTdId() != volNextId || first.GetDevId() != 1 {
		t.Fatalf("first a: got td_id %d dev_id %d, want %d and 1",
			first.GetTdId(), first.GetDevId(), volNextId)
	}
	if err := del(); err != nil {
		t.Fatalf("DeleteThinDevice a: %v", err)
	}
	second := create()
	if second.GetTdId() != volNextId+1 || second.GetDevId() != 2 {
		t.Fatalf("recreated a: got td_id %d dev_id %d, want %d and 2",
			second.GetTdId(), second.GetDevId(), volNextId+1)
	}
	want := &pb.ThinDevice{TdId: volNextId + 1, DevId: 2, Size: volTdSize}
	if got := env.td("a"); !proto.Equal(got, want) {
		t.Errorf("recreated a: got %v, want %v", got, want)
	}

	// A snapshot of the DEAD device: ori_id 1 is the first a's dev_id, which
	// the recreated a does not carry.
	env.putTd("snap-of-the-dead-a", 902, 9, 1, false)
	if err := del(); err != nil {
		t.Fatalf("delete the recreated a: %v", err)
	}
	if env.exists(
		model.ThinDeviceKey(env.cid, volSpId, "a"), &pb.ThinDevice{},
	) {
		t.Errorf("the recreated row must be gone")
	}
	if got := env.td("snap-of-the-dead-a"); got.GetOriId() != 1 ||
		got.GetCreated() {
		t.Errorf("the leftover snapshot must be untouched: got %v", got)
	}
}

// TestListThinDevicesReadsWholeMap pins §8.7's wait primitive: one Snapshot
// over the SpConf and every td it lists, so the map a client polls `created`
// through can never be a torn read.
func TestListThinDevicesReadsWholeMap(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("snap", 901, 8, 7, false)
	reply, err := env.srv.ListThinDevices(env.ctx, &pb.ListThinDevicesRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
	})
	if err != nil {
		t.Fatalf("ListThinDevices: %v", err)
	}
	if len(reply.GetNameToTd()) != 2 {
		t.Fatalf("name_to_td: got %v, want two entries", reply.GetNameToTd())
	}
	if !reply.GetNameToTd()["vol"].GetCreated() {
		t.Errorf("vol must report created = true")
	}
	if reply.GetNameToTd()["snap"].GetCreated() {
		t.Errorf("snap must report created = false")
	}
	if got := reply.GetNameToTd()["snap"].GetOriId(); got != 7 {
		t.Errorf("snap ori_id: got %d, want 7", got)
	}
}

// ---------------------------------------------------------------------------
// §8.8 subsystems
// ---------------------------------------------------------------------------

// TestCreateSubsystemWritesEntryAndCdc pins §8.8's three-key write set: the
// Subsystem with serial = %016x(ss_id) and model = "dnv" [D2], the CdcEntry
// dnv-cdc serves the discovery log from, and the SpConf whose nqn_list gained
// the NQN and whose next_id advanced — all under one SpRev bump.
func TestCreateSubsystemWritesEntryAndCdc(t *testing.T) {
	env := newVolEnv(t)
	reply, err := env.srv.CreateSubsystem(env.ctx, &pb.CreateSubsystemRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		Nqn:          volNqn,
		AllowedHosts: []string{volHostA, volHostB},
	})
	if err != nil {
		t.Fatalf("CreateSubsystem: %v", err)
	}
	if reply.GetSsId() != volNextId {
		t.Fatalf("reply ss_id: got %d, want %d", reply.GetSsId(), volNextId)
	}
	wantSs := &pb.Subsystem{
		SsId:         volNextId,
		Serial:       fmt.Sprintf(common.IdKeyFmt, volNextId),
		Model:        subsystemModel,
		AllowedHosts: []string{volHostA, volHostB},
	}
	if got := env.subsystem(volNqn); !proto.Equal(got, wantSs) {
		t.Errorf("subsystem: got %v, want %v", got, wantSs)
	}
	wantEntry := &pb.CdcEntry{
		Nqn: volNqn,
		NvmeTrConfList: []*pb.NvmeTrConf{
			volTrConf(volCnA), volTrConf(volCnB),
		},
		AllowedHosts: []string{volHostA, volHostB},
	}
	if got := env.cdcEntry(volNextId); !proto.Equal(got, wantEntry) {
		t.Errorf("cdc entry: got %v, want %v", got, wantEntry)
	}
	conf := env.spConf()
	if len(conf.GetNqnList()) != 1 || conf.GetNqnList()[0] != volNqn {
		t.Errorf("nqn_list: got %v", conf.GetNqnList())
	}
	if conf.GetNextId() != volNextId+1 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), volNextId+1)
	}
	if got := env.spRev(); got != 2 {
		t.Errorf("sp_rev: got %d, want 2", got)
	}
}

// TestCreateSubsystemAdvertisesOnlyEnabledCntlrs pins the one rule that makes
// the CdcEntry more than a copy of the cntlr list: a DISABLED cntlr is not
// advertised, because its namespaces are ANA inaccessible and a host that
// discovered its address would keep connecting to a path that serves nothing.
func TestCreateSubsystemAdvertisesOnlyEnabledCntlrs(t *testing.T) {
	env := newVolEnv(t)
	mustPut(t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrB), &pb.Cntlr{
		AddrPort:   volCnB,
		NvmeTrConf: volTrConf(volCnB),
		CntlidSlot: 1,
		Disabled:   true,
	})
	reply, err := env.srv.CreateSubsystem(env.ctx, &pb.CreateSubsystemRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		Nqn:         volNqn,
	})
	if err != nil {
		t.Fatalf("CreateSubsystem: %v", err)
	}
	entry := env.cdcEntry(reply.GetSsId())
	want := []*pb.NvmeTrConf{volTrConf(volCnA)}
	if len(entry.GetNvmeTrConfList()) != 1 ||
		!proto.Equal(entry.GetNvmeTrConfList()[0], want[0]) {
		t.Errorf("nvme_tr_conf_list: got %v, want %v",
			entry.GetNvmeTrConfList(), want)
	}
}

// TestUpdateSubsystemHostsRewritesBothCopies pins §8.8's two-copy rule: the
// Subsystem's allowed_hosts is what every cntlr's nvmet allowed_hosts is built
// from and the CdcEntry's is what dnv-cdc filters its discovery log with, so
// the two move together in one transaction and under one bump.
func TestUpdateSubsystemHostsRewritesBothCopies(t *testing.T) {
	env := newVolEnv(t)
	created, err := env.srv.CreateSubsystem(env.ctx, &pb.CreateSubsystemRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		Nqn:          volNqn,
		AllowedHosts: []string{volHostA},
	})
	if err != nil {
		t.Fatalf("CreateSubsystem: %v", err)
	}
	reply, err := env.srv.UpdateSubsystemHosts(
		env.ctx, &pb.UpdateSubsystemHostsRequest{
			ClusterName:  env.cluster,
			SpName:       volSpName,
			SpRev:        env.token(),
			Nqn:          volNqn,
			AllowedHosts: []string{volHostB, volHostC},
		})
	if err != nil {
		t.Fatalf("UpdateSubsystemHosts: %v", err)
	}
	if reply.GetSsId() != created.GetSsId() {
		t.Errorf("reply ss_id: got %d, want %d",
			reply.GetSsId(), created.GetSsId())
	}
	want := []string{volHostB, volHostC}
	got := env.subsystem(volNqn).GetAllowedHosts()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("subsystem allowed_hosts: got %v, want %v", got, want)
	}
	entry := env.cdcEntry(created.GetSsId())
	if fmt.Sprint(entry.GetAllowedHosts()) != fmt.Sprint(want) {
		t.Errorf("cdc allowed_hosts: got %v, want %v",
			entry.GetAllowedHosts(), want)
	}
	if len(entry.GetNvmeTrConfList()) != 2 {
		t.Errorf("the transports are the cntlrs' and must not move: %v",
			entry.GetNvmeTrConfList())
	}
	if rev := env.spRev(); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3", rev)
	}
}

// TestUpdateSubsystemHostsSkipsMissingCdcEntry pins the "skipped, never
// invented" half of the CdcEntry rule: only CreateSubsystem knows an entry's
// other fields, so an entry that is not there is left absent rather than
// guessed at — and the RPC still succeeds and still bumps.
func TestUpdateSubsystemHostsSkipsMissingCdcEntry(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, nil)
	entryKey := model.CdcEntryKey(env.cid, volShard, volSpId, 501)
	if err := env.cli.Delete(env.ctx, entryKey); err != nil {
		t.Fatalf("Delete %s: %v", entryKey, err)
	}
	_, err := env.srv.UpdateSubsystemHosts(
		env.ctx, &pb.UpdateSubsystemHostsRequest{
			ClusterName:  env.cluster,
			SpName:       volSpName,
			SpRev:        env.token(),
			Nqn:          volNqn,
			AllowedHosts: []string{volHostA},
		})
	if err != nil {
		t.Fatalf("UpdateSubsystemHosts: %v", err)
	}
	if env.exists(entryKey, &pb.CdcEntry{}) {
		t.Errorf("a missing CdcEntry must not be invented")
	}
	if got := env.subsystem(volNqn).GetAllowedHosts(); len(got) != 1 {
		t.Errorf("subsystem allowed_hosts: got %v", got)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// TestDeleteSubsystem pins both halves of §8.8's delete: namespaces block it
// (a namespace is a device a host may still be using) while allowed hosts
// never do (a host entry is a permission that goes with the subsystem), and a
// successful delete removes the CdcEntry in the same transaction so dnv-cdc
// stops advertising a subsystem no cntlr serves.
func TestDeleteSubsystem(t *testing.T) {
	t.Run("namespaces block it", func(t *testing.T) {
		env := newVolEnv(t)
		env.putSubsystem(volNqn, 501, []*pb.Namespace{{
			NsId: 601, NsIdx: 1, TdId: 900,
		}})
		before := env.spConf()
		beforeRev := env.spRev()
		_, err := env.srv.DeleteSubsystem(env.ctx, &pb.DeleteSubsystemRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: beforeRev},
			Nqn:         volNqn,
		})
		volWantCode(t, err, codes.FailedPrecondition)
		env.wantUntouched(before, beforeRev)
		if !env.exists(
			model.SubsystemKey(env.cid, volSpId, volNqn), &pb.Subsystem{},
		) {
			t.Errorf("a refused delete must leave the subsystem")
		}
	})
	t.Run("allowed hosts do not", func(t *testing.T) {
		env := newVolEnv(t)
		env.putSubsystem(volNqn, 501, nil)
		subsystem := env.subsystem(volNqn)
		subsystem.AllowedHosts = []string{volHostA, volHostB}
		mustPut(t, env.cli,
			model.SubsystemKey(env.cid, volSpId, volNqn), subsystem)
		reply, err := env.srv.DeleteSubsystem(
			env.ctx, &pb.DeleteSubsystemRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				Nqn:         volNqn,
			})
		if err != nil {
			t.Fatalf("DeleteSubsystem: %v", err)
		}
		if reply.GetSsId() != 501 {
			t.Errorf("reply ss_id: got %d, want 501", reply.GetSsId())
		}
		if env.exists(
			model.SubsystemKey(env.cid, volSpId, volNqn), &pb.Subsystem{},
		) {
			t.Errorf("the subsystem key must be gone")
		}
		if env.exists(
			model.CdcEntryKey(env.cid, volShard, volSpId, 501),
			&pb.CdcEntry{},
		) {
			t.Errorf("the cdc entry must go with it")
		}
		if got := env.spConf().GetNqnList(); len(got) != 0 {
			t.Errorf("nqn_list: got %v, want empty", got)
		}
		if rev := env.spRev(); rev != 2 {
			t.Errorf("sp_rev: got %d, want 2", rev)
		}
	})
}

// TestListSubsystemsReadsWholeMap pins §8.8's read: one Snapshot whose reply
// agrees with the nqn_list it was read from.
func TestListSubsystemsReadsWholeMap(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, nil)
	env.putSubsystem(volNqnB, 502, []*pb.Namespace{{NsId: 601, NsIdx: 2}})
	reply, err := env.srv.ListSubsystems(env.ctx, &pb.ListSubsystemsRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
	})
	if err != nil {
		t.Fatalf("ListSubsystems: %v", err)
	}
	if len(reply.GetNqnToSubsystem()) != 2 {
		t.Fatalf("nqn_to_subsystem: got %v", reply.GetNqnToSubsystem())
	}
	if got := reply.GetNqnToSubsystem()[volNqnB].GetNsList(); len(got) != 1 {
		t.Errorf("%s ns_list: got %v", volNqnB, got)
	}
}

// ---------------------------------------------------------------------------
// §8.8 namespaces
// ---------------------------------------------------------------------------

// volUuidPattern is the canonical RFC 4122 version 4 form §8.8 says an empty
// dev_uuid defaults to: lower-case, dashed, with the version nibble and the
// variant bits stamped. A host that parses the version would reject anything
// else, and validateDevIdentity accepts only this shape — so a generated
// identity and a supplied one are indistinguishable once stored.
var volUuidPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// volNguidPattern is the 16 random bytes an empty dev_nguid defaults to, as 32
// lower-case hex characters.
var volNguidPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestCreateNamespaceGeneratesIdentities pins §8.8's Defaults: an empty
// dev_uuid and dev_nguid are generated inside the transaction, in exactly the
// forms validateDevIdentity accepts, and two namespaces never draw the same
// value — the identity ends up in host-visible identify data, where a
// reproducible value would let two unrelated namespaces claim one identity.
func TestCreateNamespaceGeneratesIdentities(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, nil)
	env.putTd("vol", 900, 7, 0, true)
	seenUuid := make(map[string]bool)
	seenNguid := make(map[string]bool)
	for idx, nsIdx := range []uint32{1, 5} {
		reply, err := env.srv.CreateNamespace(
			env.ctx, &pb.CreateNamespaceRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				Nqn:         volNqn,
				NsIdx:       nsIdx,
				TdName:      "vol",
			})
		if err != nil {
			t.Fatalf("CreateNamespace %d: %v", nsIdx, err)
		}
		if want := volNextId + uint64(idx); reply.GetNsId() != want {
			t.Errorf("reply ns_id: got %d, want %d", reply.GetNsId(), want)
		}
		ns := findNs(env.subsystem(volNqn), nsIdx)
		if ns == nil {
			t.Fatalf("namespace %d was not stored", nsIdx)
		}
		if !volUuidPattern.MatchString(ns.GetDevUuid()) {
			t.Errorf("dev_uuid %q is not a canonical v4 uuid",
				ns.GetDevUuid())
		}
		if !volNguidPattern.MatchString(ns.GetDevNguid()) {
			t.Errorf("dev_nguid %q is not 32 lower-case hex characters",
				ns.GetDevNguid())
		}
		if err := validateDevIdentity(
			ns.GetDevUuid(), ns.GetDevNguid(),
		); err != nil {
			t.Errorf("a generated identity must validate as a supplied one: %v",
				err)
		}
		if ns.GetTdId() != 900 {
			t.Errorf("td_id: got %d, want 900 (the id, never the name)",
				ns.GetTdId())
		}
		if seenUuid[ns.GetDevUuid()] || seenNguid[ns.GetDevNguid()] {
			t.Errorf("two namespaces drew the same identity")
		}
		seenUuid[ns.GetDevUuid()] = true
		seenNguid[ns.GetDevNguid()] = true
	}
	if got := env.spConf().GetNextId(); got != volNextId+2 {
		t.Errorf("next_id: got %d, want %d", got, volNextId+2)
	}
}

// TestCreateNamespaceKeepsSuppliedIdentity pins the other branch of the same
// Defaults row: a supplied uuid/nguid is stored verbatim, and the suspended
// flag the request carries is stored with it (§11.3 creates the destination's
// namespaces already suspended).
func TestCreateNamespaceKeepsSuppliedIdentity(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, nil)
	env.putTd("vol", 900, 7, 0, true)
	const uuid = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	const nguid = "0123456789abcdef0123456789abcdef"
	_, err := env.srv.CreateNamespace(env.ctx, &pb.CreateNamespaceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		Nqn:         volNqn,
		NsIdx:       1,
		DevUuid:     uuid,
		DevNguid:    nguid,
		Suspended:   true,
		TdName:      "vol",
	})
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	ns := findNs(env.subsystem(volNqn), 1)
	if ns.GetDevUuid() != uuid || ns.GetDevNguid() != nguid {
		t.Errorf("identity: got %q/%q, want %q/%q",
			ns.GetDevUuid(), ns.GetDevNguid(), uuid, nguid)
	}
	if !ns.GetSuspended() {
		t.Errorf("suspended must be stored as requested")
	}
}

// TestCreateNamespaceRefusals is §8.8's error table for the one field the user
// chooses: ns_idx is the NVMe NSID, so 0 (reserved by NVMe) and a value the
// subsystem already uses are INVALID_ARGUMENT and never ALREADY_EXISTS — a
// namespace is a field of the subsystem, not a key of its own.
func TestCreateNamespaceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv)
		req   *pb.CreateNamespaceRequest
		want  codes.Code
	}{
		{
			name: "ns_idx 0 is reserved",
			req:  &pb.CreateNamespaceRequest{NsIdx: 0, TdName: "vol"},
			want: codes.InvalidArgument,
		},
		{
			name: "ns_idx already used",
			setup: func(env *volEnv) {
				subsystem := env.subsystem(volNqn)
				subsystem.NsList = []*pb.Namespace{{NsId: 1, NsIdx: 1}}
				mustPut(env.t, env.cli,
					model.SubsystemKey(env.cid, volSpId, volNqn), subsystem)
			},
			req:  &pb.CreateNamespaceRequest{NsIdx: 1, TdName: "vol"},
			want: codes.InvalidArgument,
		},
		{
			name: "malformed dev_uuid",
			req: &pb.CreateNamespaceRequest{
				NsIdx: 1, TdName: "vol", DevUuid: "not-a-uuid",
			},
			want: codes.InvalidArgument,
		},
		{
			name: "unknown thin device",
			req:  &pb.CreateNamespaceRequest{NsIdx: 1, TdName: "nope"},
			want: codes.NotFound,
		},
		{
			name: "the per-subsystem namespace ceiling",
			setup: func(env *volEnv) {
				subsystem := env.subsystem(volNqn)
				for idx := 0; idx < common.MaxNsCntPerSs; idx++ {
					subsystem.NsList = append(subsystem.NsList, &pb.Namespace{
						NsId: uint64(idx) + 1, NsIdx: uint32(idx) + 10,
					})
				}
				mustPut(env.t, env.cli,
					model.SubsystemKey(env.cid, volSpId, volNqn), subsystem)
			},
			req:  &pb.CreateNamespaceRequest{NsIdx: 1, TdName: "vol"},
			want: codes.ResourceExhausted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putSubsystem(volNqn, 501, nil)
			env.putTd("vol", 900, 7, 0, true)
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.spConf()
			beforeRev := env.spRev()
			req := proto.Clone(tc.req).(*pb.CreateNamespaceRequest)
			req.ClusterName = env.cluster
			req.SpName = volSpName
			req.Nqn = volNqn
			req.SpRev = &pb.SpRev{Revision: beforeRev}
			_, err := env.srv.CreateNamespace(env.ctx, req)
			volWantCode(t, err, tc.want)
			env.wantUntouched(before, beforeRev)
		})
	}
}

// TestNamespaceUpdatesAndDelete pins the three RPCs that only ever rewrite the
// Subsystem value: DeleteNamespace (which returns no id to any counter, since
// per-SP ids are never reused), UpdateNamespaceDev (which repoints the ns at
// another td by ID) and UpdateNamespaceSuspended (which writes and bumps even
// when the flag does not move — §0 #17's idempotent no-write covers the three
// Update*Enabled/Disabled RPCs only, and the §11.3 choreography relies on the
// bump reaching the cntlrs).
func TestNamespaceUpdatesAndDelete(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{{
		NsId: 601, NsIdx: 1, TdId: 900, Suspended: true,
	}})
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("snap", 901, 8, 7, true)

	devReply, err := env.srv.UpdateNamespaceDev(
		env.ctx, &pb.UpdateNamespaceDevRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			Nqn:         volNqn,
			NsIdx:       1,
			TdName:      "snap",
		})
	if err != nil {
		t.Fatalf("UpdateNamespaceDev: %v", err)
	}
	if devReply.GetNsId() != 601 {
		t.Errorf("reply ns_id: got %d, want 601", devReply.GetNsId())
	}
	if got := findNs(env.subsystem(volNqn), 1).GetTdId(); got != 901 {
		t.Errorf("td_id: got %d, want 901", got)
	}

	// The flag already holds true, and the RPC still writes and still bumps.
	beforeRev := env.spRev()
	if _, err := env.srv.UpdateNamespaceSuspended(
		env.ctx, &pb.UpdateNamespaceSuspendedRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: beforeRev},
			Nqn:         volNqn,
			NsIdx:       1,
			Suspended:   true,
		}); err != nil {
		t.Fatalf("UpdateNamespaceSuspended: %v", err)
	}
	if got := env.spRev(); got != beforeRev+1 {
		t.Errorf("sp_rev: got %d, want %d (no idempotent no-write here)",
			got, beforeRev+1)
	}

	delReply, err := env.srv.DeleteNamespace(
		env.ctx, &pb.DeleteNamespaceRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			Nqn:         volNqn,
			NsIdx:       1,
		})
	if err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if delReply.GetNsId() != 601 {
		t.Errorf("reply ns_id: got %d, want 601", delReply.GetNsId())
	}
	if got := env.subsystem(volNqn).GetNsList(); len(got) != 0 {
		t.Errorf("ns_list: got %v, want empty", got)
	}
	if got := env.spConf().GetNextId(); got != volNextId {
		t.Errorf("next_id: got %d, want %d (a delete returns no id)",
			got, volNextId)
	}
	if _, err := env.srv.DeleteNamespace(
		env.ctx, &pb.DeleteNamespaceRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			Nqn:         volNqn,
			NsIdx:       1,
		}); volWantCode(t, err, codes.NotFound) == "" {
		t.Errorf("a NOT_FOUND must carry a message")
	}
}

// ---------------------------------------------------------------------------
// §8.10 transfers
// ---------------------------------------------------------------------------

// volCreateTransfer creates the fixture's standard transfer over an origin
// namespace that already exists, and returns the reply.
func volCreateTransfer(env *volEnv, name string) *pb.CreateTransferReply {
	env.t.Helper()
	reply, err := env.srv.CreateTransfer(env.ctx, &pb.CreateTransferRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		XferName:     name,
		OriNqn:       volNqn,
		OriNsIdx:     1,
		AllowedHosts: []string{volHostA},
		AutoSuspend:  true,
	})
	if err != nil {
		env.t.Fatalf("CreateTransfer %s: %v", name, err)
	}
	return reply
}

// TestCreateTransferWritesRecord pins §8.10's write set: the Transfer row is a
// pointer into the SP's own subsystem table, so the origin is resolved inside
// the transaction and stored as (ori_nqn, ori_ns_idx) verbatim, and nothing
// touches a CdcEntry — an xfer subsystem is never discovery-advertised.
func TestCreateTransferWritesRecord(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{{
		NsId: 601, NsIdx: 1, TdId: 900,
	}})
	reply := volCreateTransfer(env, "xfer-a")
	if reply.GetXferId() != volNextId {
		t.Errorf("reply xfer_id: got %d, want %d",
			reply.GetXferId(), volNextId)
	}
	want := &pb.Transfer{
		XferId:       volNextId,
		OriNqn:       volNqn,
		OriNsIdx:     1,
		AllowedHosts: []string{volHostA},
		AutoSuspend:  true,
	}
	got := &pb.Transfer{}
	env.get(model.TransferKey(env.cid, volSpId, "xfer-a"), got)
	if !proto.Equal(got, want) {
		t.Errorf("transfer: got %v, want %v", got, want)
	}
	conf := env.spConf()
	if fmt.Sprint(conf.GetXferNameList()) != "[xfer-a]" {
		t.Errorf("xfer_name_list: got %v", conf.GetXferNameList())
	}
	if conf.GetNextId() != volNextId+1 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), volNextId+1)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// TestCreateTransferRefusals pins §8.10's error table. A transfer that pointed
// at a namespace which does not exist would make every cntlr build a stack
// over nothing, which is why both halves of the origin are NOT_FOUND.
func TestCreateTransferRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv)
		req   *pb.CreateTransferRequest
		want  codes.Code
	}{
		{
			name: "unknown origin subsystem",
			req: &pb.CreateTransferRequest{
				XferName: "xfer-a", OriNqn: volNqnB, OriNsIdx: 1,
			},
			want: codes.NotFound,
		},
		{
			name: "unknown origin ns_idx",
			req: &pb.CreateTransferRequest{
				XferName: "xfer-a", OriNqn: volNqn, OriNsIdx: 9,
			},
			want: codes.NotFound,
		},
		{
			name: "name already taken",
			setup: func(env *volEnv) {
				volCreateTransfer(env, "xfer-a")
			},
			req: &pb.CreateTransferRequest{
				XferName: "xfer-a", OriNqn: volNqn, OriNsIdx: 1,
			},
			want: codes.AlreadyExists,
		},
		{
			name: "the per-SP transfer ceiling",
			setup: func(env *volEnv) {
				conf := env.spConf()
				for idx := 0; idx < common.MaxXferCntPerSp; idx++ {
					conf.XferNameList = append(
						conf.XferNameList, fmt.Sprintf("filler-%d", idx))
				}
				env.putSpConf(conf)
			},
			req: &pb.CreateTransferRequest{
				XferName: "xfer-a", OriNqn: volNqn, OriNsIdx: 1,
			},
			want: codes.ResourceExhausted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putSubsystem(volNqn, 501, []*pb.Namespace{{
				NsId: 601, NsIdx: 1, TdId: 900,
			}})
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.spConf()
			beforeRev := env.spRev()
			req := proto.Clone(tc.req).(*pb.CreateTransferRequest)
			req.ClusterName = env.cluster
			req.SpName = volSpName
			req.SpRev = &pb.SpRev{Revision: beforeRev}
			_, err := env.srv.CreateTransfer(env.ctx, req)
			volWantCode(t, err, tc.want)
			env.wantUntouched(before, beforeRev)
		})
	}
}

// TestDeleteTransferFinalizeSuspendsOrigin is [D-G] and §8.10's force flag:
// force == false FINALIZES a completed hand-over and therefore sets
// suspended = true on the origin namespace in the SAME transaction that
// removes the record — the only thing that closes the window in which the next
// syncup would resume the source while the destination already serves the same
// nguid. force == true ABORTS and leaves the origin untouched, so the next
// syncup brings it back.
func TestDeleteTransferFinalizeSuspendsOrigin(t *testing.T) {
	for _, tc := range []struct {
		name          string
		force         bool
		wantSuspended bool
	}{
		{name: "finalize retires the origin", force: false,
			wantSuspended: true},
		{name: "abort leaves the origin serving", force: true,
			wantSuspended: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putSubsystem(volNqn, 501, []*pb.Namespace{{
				NsId: 601, NsIdx: 1, TdId: 900,
			}})
			created := volCreateTransfer(env, "xfer-a")
			reply, err := env.srv.DeleteTransfer(
				env.ctx, &pb.DeleteTransferRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       env.token(),
					XferName:    "xfer-a",
					Force:       tc.force,
				})
			if err != nil {
				t.Fatalf("DeleteTransfer: %v", err)
			}
			if reply.GetXferId() != created.GetXferId() {
				t.Errorf("reply xfer_id: got %d, want %d",
					reply.GetXferId(), created.GetXferId())
			}
			if env.exists(
				model.TransferKey(env.cid, volSpId, "xfer-a"), &pb.Transfer{},
			) {
				t.Errorf("the transfer record must be gone")
			}
			if got := env.spConf().GetXferNameList(); len(got) != 0 {
				t.Errorf("xfer_name_list: got %v, want empty", got)
			}
			got := findNs(env.subsystem(volNqn), 1).GetSuspended()
			if got != tc.wantSuspended {
				t.Errorf("origin suspended: got %v, want %v",
					got, tc.wantSuspended)
			}
			if rev := env.spRev(); rev != 3 {
				t.Errorf("sp_rev: got %d, want 3", rev)
			}
		})
	}
}

// TestDeleteTransferFinalizeSkipsMissingOrigin is [D-G]'s second half: on the
// finalize path a missing origin subsystem or ns_idx is SKIPPED and not an
// error, because the transfer is being deleted either way and the RPC must not
// become unable to complete because the namespace it points at was removed
// first.
func TestDeleteTransferFinalizeSkipsMissingOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(env *volEnv)
	}{
		{
			name: "the origin subsystem is gone",
			drop: func(env *volEnv) {
				key := model.SubsystemKey(env.cid, volSpId, volNqn)
				if err := env.cli.Delete(env.ctx, key); err != nil {
					env.t.Fatalf("Delete %s: %v", key, err)
				}
				conf := env.spConf()
				conf.NqnList = nil
				env.putSpConf(conf)
			},
		},
		{
			name: "the origin ns_idx is gone",
			drop: func(env *volEnv) {
				subsystem := env.subsystem(volNqn)
				subsystem.NsList = nil
				mustPut(env.t, env.cli,
					model.SubsystemKey(env.cid, volSpId, volNqn), subsystem)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putSubsystem(volNqn, 501, []*pb.Namespace{{
				NsId: 601, NsIdx: 1, TdId: 900,
			}})
			volCreateTransfer(env, "xfer-a")
			tc.drop(env)
			if _, err := env.srv.DeleteTransfer(
				env.ctx, &pb.DeleteTransferRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       env.token(),
					XferName:    "xfer-a",
					Force:       false,
				}); err != nil {
				t.Fatalf("DeleteTransfer must still complete: %v", err)
			}
			if env.exists(
				model.TransferKey(env.cid, volSpId, "xfer-a"), &pb.Transfer{},
			) {
				t.Errorf("the transfer record must be gone")
			}
		})
	}
}

// TestGetAndUpdateTransferHosts pins the read and the host rewrite: the list is
// replaced wholesale (an operator calls it after a destination cntlr moved to
// another CN and so presents a different CnHostNqn) and the SP is bumped
// unconditionally, because a caller that resends the same list still wants the
// cntlrs to re-apply it.
func TestGetAndUpdateTransferHosts(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{{
		NsId: 601, NsIdx: 1, TdId: 900,
	}})
	created := volCreateTransfer(env, "xfer-a")
	beforeRev := env.spRev()
	if _, err := env.srv.UpdateTransferHosts(
		env.ctx, &pb.UpdateTransferHostsRequest{
			ClusterName:  env.cluster,
			SpName:       volSpName,
			SpRev:        &pb.SpRev{Revision: beforeRev},
			XferName:     "xfer-a",
			AllowedHosts: []string{volHostB, volHostC},
		}); err != nil {
		t.Fatalf("UpdateTransferHosts: %v", err)
	}
	reply, err := env.srv.GetTransfer(env.ctx, &pb.GetTransferRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		XferName:    "xfer-a",
	})
	if err != nil {
		t.Fatalf("GetTransfer: %v", err)
	}
	if reply.GetXfer().GetXferId() != created.GetXferId() {
		t.Errorf("xfer_id: got %d, want %d",
			reply.GetXfer().GetXferId(), created.GetXferId())
	}
	want := []string{volHostB, volHostC}
	if fmt.Sprint(reply.GetXfer().GetAllowedHosts()) != fmt.Sprint(want) {
		t.Errorf("allowed_hosts: got %v, want %v",
			reply.GetXfer().GetAllowedHosts(), want)
	}
	if got := env.spRev(); got != beforeRev+1 {
		t.Errorf("sp_rev: got %d, want %d", got, beforeRev+1)
	}
	if _, err := env.srv.GetTransfer(env.ctx, &pb.GetTransferRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		XferName:    "nope",
	}); volWantCode(t, err, codes.NotFound) == "" {
		t.Errorf("a NOT_FOUND must carry a message")
	}
}

// ---------------------------------------------------------------------------
// §8.9 clones
// ---------------------------------------------------------------------------

// volCloneSrcSliceCnt is the source geometry the clone tests are created with.
// It is deliberately > 1 so that AppendCloneBitmap can address more than one
// source slice and the bm_cnt high-water rule becomes observable.
const volCloneSrcSliceCnt = uint32(4)

// volCreateClone creates one clone over dstTdName and returns the reply.
func volCreateClone(
	env *volEnv,
	name string,
	dstTdName string,
) *pb.CreateCloneReply {
	env.t.Helper()
	reply, err := env.srv.CreateClone(env.ctx, &pb.CreateCloneRequest{
		ClusterName:   env.cluster,
		SpName:        volSpName,
		SpRev:         env.token(),
		CloneName:     name,
		SrcTrConf:     []*pb.NvmeTrConf{volTrConf(volCnA)},
		SrcNqn:        volSrcNqn,
		SrcNsIdx:      1,
		SrcSliceCnt:   volCloneSrcSliceCnt,
		SrcStripeSize: 64 * 1024,
		SrcBlockSize:  1024 * 1024,
		DstTdName:     dstTdName,
		DmCloneConf:   &pb.DmCloneConf{HydrationThreshold: 2},
		AutoResume:    true,
	})
	if err != nil {
		env.t.Fatalf("CreateClone %s: %v", name, err)
	}
	return reply
}

// TestCreateCloneWritesRecord pins §8.9's write set: the destination td is
// resolved to its td_id (an id is never reused, so a renamed or recreated
// device can never silently become the destination) and the record starts with
// bm_cnt 0 — the source bitmap is a pure optimization that may arrive later.
func TestCreateCloneWritesRecord(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	reply := volCreateClone(env, "clone-a", "dst")
	if reply.GetCloneId() != volNextId {
		t.Errorf("reply clone_id: got %d, want %d",
			reply.GetCloneId(), volNextId)
	}
	want := &pb.Clone{
		CloneId:       volNextId,
		SrcTrConfList: []*pb.NvmeTrConf{volTrConf(volCnA)},
		SrcNqn:        volSrcNqn,
		SrcNsIdx:      1,
		SrcSliceCnt:   volCloneSrcSliceCnt,
		SrcStripeSize: 64 * 1024,
		SrcBlockSize:  1024 * 1024,
		DstTdId:       900,
		DmCloneConf:   &pb.DmCloneConf{HydrationThreshold: 2},
		AutoResume:    true,
		BmCnt:         0,
	}
	if got := env.clone("clone-a"); !proto.Equal(got, want) {
		t.Errorf("clone: got %v, want %v", got, want)
	}
	conf := env.spConf()
	if fmt.Sprint(conf.GetCloneNameList()) != "[clone-a]" {
		t.Errorf("clone_name_list: got %v", conf.GetCloneNameList())
	}
	if conf.GetNextId() != volNextId+1 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), volNextId+1)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// TestCreateCloneRefusesASecondCloneOnOneTd pins §8.9's one-destination rule:
// two dm-clones writing the same raid0 would each believe they own its
// regions, so the second create is FAILED_PRECONDITION and writes nothing.
func TestCreateCloneRefusesASecondCloneOnOneTd(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.CreateClone(env.ctx, &pb.CreateCloneRequest{
		ClusterName:   env.cluster,
		SpName:        volSpName,
		SpRev:         &pb.SpRev{Revision: beforeRev},
		CloneName:     "clone-b",
		SrcTrConf:     []*pb.NvmeTrConf{volTrConf(volCnA)},
		SrcNqn:        volSrcNqn,
		SrcSliceCnt:   volCloneSrcSliceCnt,
		SrcStripeSize: 64 * 1024,
		SrcBlockSize:  1024 * 1024,
		DstTdName:     "dst",
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "clone-a") {
		t.Errorf("message %q must name the clone that already owns the td",
			msg)
	}
	env.wantUntouched(before, beforeRev)
}

// TestAppendCloneBitmapHighWaterMark pins the two rules §8.9 gives the source
// bitmap: a chunk is ADDRESSED by the source slice_idx and grows by appending
// (callers page one slice's bitmap and the concatenation IS the slice's
// bitmap), and bm_cnt is a high-water mark — max(bm_cnt, slice_idx + 1), never
// +1 — because it is what DeleteClone deletes the chunk keys from and lowering
// it would orphan them.
//
// The bytes are stored verbatim (GW14, [D-J]): the gateway never inspects or
// rewrites a bit.
func TestAppendCloneBitmapHighWaterMark(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	created := volCreateClone(env, "clone-a", "dst")
	for _, step := range []struct {
		sliceIdx  uint32
		bitmap    []byte
		wantChunk []byte
		wantBmCnt uint32
	}{
		{0, []byte{0x01, 0x02}, []byte{0x01, 0x02}, 1},
		{0, []byte{0x03}, []byte{0x01, 0x02, 0x03}, 1},
		{2, []byte{0xff}, []byte{0xff}, 3},
		{1, []byte{0xa0}, []byte{0xa0}, 3},
	} {
		reply, err := env.srv.AppendCloneBitmap(
			env.ctx, &pb.AppendCloneBitmapRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				CloneName:   "clone-a",
				SliceIdx:    step.sliceIdx,
				Bitmap:      step.bitmap,
			})
		if err != nil {
			t.Fatalf("AppendCloneBitmap slice %d: %v", step.sliceIdx, err)
		}
		if reply.GetCloneId() != created.GetCloneId() {
			t.Errorf("reply clone_id: got %d, want %d",
				reply.GetCloneId(), created.GetCloneId())
		}
		chunk := &pb.CloneBitmap{}
		env.get(model.CloneBitmapKey(
			env.cid, volSpId, "clone-a", step.sliceIdx), chunk)
		if fmt.Sprint(chunk.GetBitmap()) != fmt.Sprint(step.wantChunk) {
			t.Errorf("chunk %d: got %v, want %v",
				step.sliceIdx, chunk.GetBitmap(), step.wantChunk)
		}
		if got := env.clone("clone-a").GetBmCnt(); got != step.wantBmCnt {
			t.Errorf("bm_cnt after slice %d: got %d, want %d",
				step.sliceIdx, got, step.wantBmCnt)
		}
	}
	if got := env.spRev(); got != 6 {
		t.Errorf("sp_rev: got %d, want 6 (one bump per append)", got)
	}
}

// TestAppendCloneBitmapRefusals pins §8.9's two bounds. Both are
// INVALID_ARGUMENT and not the RESOURCE_EXHAUSTED of GW7's Append*Bitmap row:
// that row is AppendMigrationBitmap's ceiling, reached by previous appends,
// whereas these judge the slice_idx of THIS request against the geometry the
// clone was created with.
func TestAppendCloneBitmapRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sliceIdx uint32
		bitmap   []byte
	}{
		{
			name:     "slice_idx at src_slice_cnt",
			sliceIdx: volCloneSrcSliceCnt,
			bitmap:   []byte{0x01},
		},
		{
			name:     "empty bitmap",
			sliceIdx: 0,
			bitmap:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putTd("dst", 900, 7, 0, true)
			volCreateClone(env, "clone-a", "dst")
			before := env.spConf()
			beforeRev := env.spRev()
			_, err := env.srv.AppendCloneBitmap(
				env.ctx, &pb.AppendCloneBitmapRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					CloneName:   "clone-a",
					SliceIdx:    tc.sliceIdx,
					Bitmap:      tc.bitmap,
				})
			volWantCode(t, err, codes.InvalidArgument)
			env.wantUntouched(before, beforeRev)
			if got := env.clone("clone-a").GetBmCnt(); got != 0 {
				t.Errorf("bm_cnt: got %d, want 0", got)
			}
		})
	}
}

// TestDeleteCloneDropsChunksAndResumesNs pins §8.9's teardown: the Clone row,
// EVERY chunk key from bm_idx 0 to bm_cnt-1 (an STM cannot range, so the count
// the record carries is the only thing that can name them) and the
// clone_name_list entry go together, and the destination's namespaces are
// resumed in the same transaction — the clone record is the only thing that
// remembers why they were suspended.
func TestDeleteCloneDropsChunksAndResumesNs(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.putTd("other", 901, 8, 0, true)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{
		{NsId: 601, NsIdx: 1, TdId: 900, Suspended: true},
		{NsId: 602, NsIdx: 2, TdId: 901, Suspended: true},
	})
	created := volCreateClone(env, "clone-a", "dst")
	for idx := uint32(0); idx < 3; idx++ {
		if _, err := env.srv.AppendCloneBitmap(
			env.ctx, &pb.AppendCloneBitmapRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				CloneName:   "clone-a",
				SliceIdx:    idx,
				Bitmap:      []byte{byte(idx)},
			}); err != nil {
			t.Fatalf("AppendCloneBitmap %d: %v", idx, err)
		}
	}
	reply, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		CloneName:   "clone-a",
		// force skips the hydration proof, which is the only part of this
		// RPC that leaves etcd (§9.4 covers that path).
		Force: true,
	})
	if err != nil {
		t.Fatalf("DeleteClone: %v", err)
	}
	if reply.GetCloneId() != created.GetCloneId() {
		t.Errorf("reply clone_id: got %d, want %d",
			reply.GetCloneId(), created.GetCloneId())
	}
	if env.exists(
		model.CloneKey(env.cid, volSpId, "clone-a"), &pb.Clone{},
	) {
		t.Errorf("the clone record must be gone")
	}
	for idx := uint32(0); idx < 3; idx++ {
		key := model.CloneBitmapKey(env.cid, volSpId, "clone-a", idx)
		if env.exists(key, &pb.CloneBitmap{}) {
			t.Errorf("chunk %d must be gone", idx)
		}
	}
	subsystem := env.subsystem(volNqn)
	if findNs(subsystem, 1).GetSuspended() {
		t.Errorf("the destination's namespace must be resumed")
	}
	if !findNs(subsystem, 2).GetSuspended() {
		t.Errorf("a namespace on another td must be left suspended")
	}
	if got := env.spConf().GetCloneNameList(); len(got) != 0 {
		t.Errorf("clone_name_list: got %v, want empty", got)
	}
}

// TestDeleteCloneWithoutPrimaryCntlr pins the one force == false refusal that
// never leaves etcd: with no primary cntlr there is nobody whose
// clone_id_to_dm_clone row could prove the copy finished, and §8.9 makes
// unproven hydration FAILED_PRECONDITION rather than the usual AG3 ABORTED.
func TestDeleteCloneWithoutPrimaryCntlr(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	mustPut(t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrA), &pb.Cntlr{
		AddrPort:   volCnA,
		NvmeTrConf: volTrConf(volCnA),
	})
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		CloneName:   "clone-a",
		Force:       false,
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "primary cntlr") {
		t.Errorf("message %q must say why hydration is unprovable", msg)
	}
	env.wantUntouched(before, beforeRev)
	if !env.exists(
		model.CloneKey(env.cid, volSpId, "clone-a"), &pb.Clone{},
	) {
		t.Errorf("a refused delete must leave the record")
	}
}

// TestGetAndUpdateCloneTrConf pins the read and §8.9's transport rewrite: the
// list is a REPLACEMENT and never a merge, because an address that is gone
// must stop being retried by the primary.
func TestGetAndUpdateCloneTrConf(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	created := volCreateClone(env, "clone-a", "dst")
	if _, err := env.srv.UpdateCloneTrConf(
		env.ctx, &pb.UpdateCloneTrConfRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			CloneName:   "clone-a",
			SrcTrConf:   []*pb.NvmeTrConf{volTrConf(volCnB)},
		}); err != nil {
		t.Fatalf("UpdateCloneTrConf: %v", err)
	}
	reply, err := env.srv.GetClone(env.ctx, &pb.GetCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		CloneName:   "clone-a",
	})
	if err != nil {
		t.Fatalf("GetClone: %v", err)
	}
	list := reply.GetClone().GetSrcTrConfList()
	if len(list) != 1 || !proto.Equal(list[0], volTrConf(volCnB)) {
		t.Errorf("src_tr_conf_list: got %v, want only %v",
			list, volTrConf(volCnB))
	}
	if reply.GetClone().GetCloneId() != created.GetCloneId() {
		t.Errorf("clone_id: got %d, want %d",
			reply.GetClone().GetCloneId(), created.GetCloneId())
	}
}

// ---------------------------------------------------------------------------
// §8.11 migrations
// ---------------------------------------------------------------------------

// volCreateMigration hangs one migration destination off the fixture's data
// leg on dn-c, and returns the reply.
func volCreateMigration(env *volEnv, name string) *pb.CreateMigrationReply {
	env.t.Helper()
	reply, err := env.srv.CreateMigration(env.ctx, &pb.CreateMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		MigrName:    name,
		SrcSideId:   volDataSideA,
		DnSelector:  volDnSelector(volDnC),
		DmCloneConf: &pb.DmCloneConf{HydrationThreshold: 2},
	})
	if err != nil {
		env.t.Fatalf("CreateMigration %s: %v", name, err)
	}
	return reply
}

// TestCreateMigrationChargesDestination pins §8.11's whole allocation: the leg
// gains a SECOND side, written provisioned = false ([D15]) on a DN no leg of
// the group already uses, holding the first cntlid slot that differs from the
// source's ([D-I], §11.8), and the DN's ledger entry moves by exactly the
// group's ext_cnt — record, capacity key and revision bump included (§5.5,
// §5.6). Nothing else in the SP is touched: a migration destination changes no
// group's ext_cnt, so no cntlr's CN is charged (§8.4, §8.6).
func TestCreateMigrationChargesDestination(t *testing.T) {
	env := newVolEnv(t)
	reply := volCreateMigration(env, "migr-a")
	if reply.GetMigrId() != volNextId {
		t.Fatalf("reply migr_id: got %d, want %d",
			reply.GetMigrId(), volNextId)
	}
	dstSideId := volNextId + 1
	wantMigr := &pb.Migration{
		MigrId:      volNextId,
		SrcSideId:   volDataSideA,
		DstSideId:   dstSideId,
		DmCloneConf: &pb.DmCloneConf{HydrationThreshold: 2},
		BmCnt:       0,
	}
	if got := env.migration("migr-a"); !proto.Equal(got, wantMigr) {
		t.Errorf("migration: got %v, want %v", got, wantMigr)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	leg := activeLegOf(grp, volDataLegA)
	if leg == nil || len(leg.GetSideList()) != 2 {
		t.Fatalf("leg %d must own two sides: %v", volDataLegA, leg)
	}
	wantSide := &pb.Side{
		SideId:      dstSideId,
		AddrPort:    volDnC,
		CntlidSlot:  volSlotAlt,
		NvmeTrConf:  volTrConf(volDnC),
		ErrEpoch:    0,
		Provisioned: false,
	}
	if got := leg.GetSideList()[1]; !proto.Equal(got, wantSide) {
		t.Errorf("destination side: got %v, want %v", got, wantSide)
	}
	if got := leg.GetSideList()[0]; !proto.Equal(got, volSide(
		volDataSideA, volDnA,
	)) {
		t.Errorf("the source side must not move: %v", got)
	}
	dn := env.dn(volDnC)
	if dn.GetFreeExtCnt() != volDnFree-volDataExtCnt {
		t.Errorf("dn-c free_ext_cnt: got %d, want %d",
			dn.GetFreeExtCnt(), volDnFree-volDataExtCnt)
	}
	wantPtr := &pb.SidePointer{
		SpId: volSpId, LegId: volDataLegA, SideId: dstSideId,
	}
	if len(dn.GetSidePtrList()) != 1 ||
		!proto.Equal(dn.GetSidePtrList()[0], wantPtr) {
		t.Errorf("dn-c side_ptr_list: got %v, want [%v]",
			dn.GetSidePtrList(), wantPtr)
	}
	if env.exists(env.dnCapKey(volDnC, volDnFree), &pb.DnCapacity{}) {
		t.Errorf("the old capacity key must be deleted")
	}
	if !env.exists(
		env.dnCapKey(volDnC, volDnFree-volDataExtCnt), &pb.DnCapacity{},
	) {
		t.Errorf("the new capacity key must be written")
	}
	if got := env.dnRev(volDnC); got != 2 {
		t.Errorf("dn-c rev: got %d, want 2", got)
	}
	for _, addrPort := range []string{volDnA, volDnB, volDnD} {
		if got := env.dnRev(addrPort); got != 1 {
			t.Errorf("%s rev: got %d, want 1 (untouched)", addrPort, got)
		}
		if got := env.dn(addrPort).GetFreeExtCnt(); got != volDnFree {
			t.Errorf("%s free_ext_cnt: got %d, want %d",
				addrPort, got, volDnFree)
		}
	}
	for _, addrPort := range []string{volCnA, volCnB} {
		if got := env.cnRev(addrPort); got != 1 {
			t.Errorf("%s rev: got %d, want 1 (no CN is charged)",
				addrPort, got)
		}
	}
	conf := env.spConf()
	if fmt.Sprint(conf.GetMigrNameList()) != "[migr-a]" {
		t.Errorf("migr_name_list: got %v", conf.GetMigrNameList())
	}
	if conf.GetNextId() != volNextId+2 {
		t.Errorf("next_id: got %d, want %d (migr_id then dst side_id)",
			conf.GetNextId(), volNextId+2)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// volMigrationDst runs CreateMigration on the data group with NO NodeSelector
// at all — the allocator's own rule is what the caller wants to see — and
// returns the addr_port the destination side landed on.
func volMigrationDst(env *volEnv) (string, error) {
	env.t.Helper()
	if _, err := env.srv.CreateMigration(
		env.ctx, &pb.CreateMigrationRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			MigrName:    "migr-a",
			SrcSideId:   volDataSideA,
			DmCloneConf: &pb.DmCloneConf{HydrationThreshold: 2},
		}); err != nil {
		return "", err
	}
	leg := activeLegOf(volGrpOf(env.t, env.slice(), volDataGrpId), volDataLegA)
	if leg == nil || len(leg.GetSideList()) != 2 {
		env.t.Fatalf("leg %d must own two sides: %v", volDataLegA, leg)
	}
	return leg.GetSideList()[1].GetAddrPort(), nil
}

// TestCreateMigrationPrefersAnotherFailureDomain pins §6.5's two tiers on
// §8.11's destination scan: tier 1 keeps the destination out of the failure
// domains the group already occupies — dn-c is excluded although no side of
// the group is on it, because it shares dn-a's domain — and when no other
// domain is left tier 2 places the destination there anyway. A migration that
// cannot leave the domain is still a migration; refusing it is not the answer.
func TestCreateMigrationPrefersAnotherFailureDomain(t *testing.T) {
	t.Run("tier 1 leaves the group's domains", func(t *testing.T) {
		// One draw proves nothing here — see volTier1DrawCnt.
		for draw := 0; draw < volTier1DrawCnt; draw++ {
			env := volTwoDomainEnv(t)
			got, err := volMigrationDst(env)
			if err != nil {
				t.Fatalf("CreateMigration: %v", err)
			}
			if got != volDnD {
				t.Fatalf(
					"draw %d: destination on %s, want %s (dn-c is in "+
						"dn-a's domain, and tier 2 must not have run)",
					draw, got, volDnD)
			}
		}
	})

	t.Run("tier 2 places in an occupied domain", func(t *testing.T) {
		env := volTwoDomainEnv(t)
		env.dropDn(volDnD)
		got, err := volMigrationDst(env)
		if err != nil {
			t.Fatalf("CreateMigration: %v (tier 2 must place, never "+
				"RESOURCE_EXHAUSTED)", err)
		}
		if got != volDnC {
			t.Errorf("destination on %s, want %s", got, volDnC)
		}
		if got := env.dn(volDnC).GetFreeExtCnt(); got !=
			volDnCFree-volDataExtCnt {
			t.Errorf("dn-c free_ext_cnt: got %d, want %d",
				got, volDnCFree-volDataExtCnt)
		}
	})
}

// TestCancelMigrationIsTheExactMirror pins §8.11's rollback: whatever
// CreateMigration charged is returned, the destination side leaves the leg and
// the record and EVERY bitmap chunk go with it — while the source side, which
// the create never touched, is left byte-identical.
//
// The revisions are the one thing that does NOT mirror: a bump is a monotone
// counter that tells a worker "read again", so cancelling adds a bump rather
// than undoing one (§5.5).
func TestCancelMigrationIsTheExactMirror(t *testing.T) {
	env := newVolEnv(t)
	sliceBefore := env.slice()
	dnBefore := env.dn(volDnC)
	created := volCreateMigration(env, "migr-a")
	for idx := 0; idx < 2; idx++ {
		if _, err := env.srv.AppendMigrationBitmap(
			env.ctx, &pb.AppendMigrationBitmapRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				MigrName:    "migr-a",
				Bitmap:      []byte{byte(idx)},
			}); err != nil {
			t.Fatalf("AppendMigrationBitmap %d: %v", idx, err)
		}
	}
	revBefore := env.spRev()
	reply, err := env.srv.CancelMigration(
		env.ctx, &pb.CancelMigrationRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: revBefore},
			MigrName:    "migr-a",
		})
	if err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}
	if reply.GetMigrId() != created.GetMigrId() {
		t.Errorf("reply migr_id: got %d, want %d",
			reply.GetMigrId(), created.GetMigrId())
	}
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("slice did not roll back:\n got %v\nwant %v",
			got, sliceBefore)
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnBefore) {
		t.Errorf("dn-c did not roll back:\n got %v\nwant %v", got, dnBefore)
	}
	if !env.exists(env.dnCapKey(volDnC, volDnFree), &pb.DnCapacity{}) {
		t.Errorf("dn-c's original capacity key must be back")
	}
	if env.exists(
		env.dnCapKey(volDnC, volDnFree-volDataExtCnt), &pb.DnCapacity{},
	) {
		t.Errorf("the charged capacity key must be gone")
	}
	if env.exists(
		model.MigrationKey(env.cid, volSpId, "migr-a"), &pb.Migration{},
	) {
		t.Errorf("the migration record must be gone")
	}
	for idx := uint32(0); idx < 2; idx++ {
		key := model.MigrBitmapKey(env.cid, volSpId, "migr-a", idx)
		if env.exists(key, &pb.MigrBitmap{}) {
			t.Errorf("chunk %d must be gone", idx)
		}
	}
	conf := env.spConf()
	if len(conf.GetMigrNameList()) != 0 {
		t.Errorf("migr_name_list: got %v, want empty", conf.GetMigrNameList())
	}
	if conf.GetNextId() != volNextId+2 {
		t.Errorf("next_id: got %d, want %d (ids are never returned)",
			conf.GetNextId(), volNextId+2)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("sp_rev: got %d, want %d", got, revBefore+1)
	}
	if got := env.dnRev(volDnC); got != 3 {
		t.Errorf("dn-c rev: got %d, want 3 (charge then release)", got)
	}
}

// TestFinishMigrationReleasesSource pins §8.11's other terminal RPC: the
// destination becomes the leg's only side and the SOURCE's extents and pointer
// go back to its DN, which is what makes the source agent tear the side down
// on its next syncup. force = true skips the hydration proof, the only part of
// the RPC that leaves etcd (§9.4 covers that path).
func TestFinishMigrationReleasesSource(t *testing.T) {
	env := newVolEnv(t)
	created := volCreateMigration(env, "migr-a")
	dstSideId := created.GetMigrId() + 1
	reply, err := env.srv.FinishMigration(
		env.ctx, &pb.FinishMigrationRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			MigrName:    "migr-a",
			Force:       true,
		})
	if err != nil {
		t.Fatalf("FinishMigration: %v", err)
	}
	if reply.GetMigrId() != created.GetMigrId() {
		t.Errorf("reply migr_id: got %d, want %d",
			reply.GetMigrId(), created.GetMigrId())
	}
	leg := activeLegOf(volGrpOf(t, env.slice(), volDataGrpId), volDataLegA)
	if len(leg.GetSideList()) != 1 {
		t.Fatalf("the leg must be back to one side: %v", leg.GetSideList())
	}
	if got := leg.GetSideList()[0].GetSideId(); got != dstSideId {
		t.Errorf("survivor side_id: got %d, want %d (the destination)",
			got, dstSideId)
	}
	dnA := env.dn(volDnA)
	if dnA.GetFreeExtCnt() != volDnFree+volDataExtCnt {
		t.Errorf("dn-a free_ext_cnt: got %d, want %d",
			dnA.GetFreeExtCnt(), volDnFree+volDataExtCnt)
	}
	for _, ptr := range dnA.GetSidePtrList() {
		if ptr.GetSideId() == volDataSideA {
			t.Errorf("the source side pointer must be gone: %v",
				dnA.GetSidePtrList())
		}
	}
	if len(dnA.GetSidePtrList()) != 1 {
		t.Errorf("dn-a must keep its meta side: %v", dnA.GetSidePtrList())
	}
	if !env.exists(
		env.dnCapKey(volDnA, volDnFree+volDataExtCnt), &pb.DnCapacity{},
	) {
		t.Errorf("dn-a's capacity key must follow its new free count")
	}
	if got := env.dnRev(volDnA); got != 2 {
		t.Errorf("dn-a rev: got %d, want 2", got)
	}
	if env.exists(
		model.MigrationKey(env.cid, volSpId, "migr-a"), &pb.Migration{},
	) {
		t.Errorf("the migration record must be gone")
	}
	if got := env.spConf().GetMigrNameList(); len(got) != 0 {
		t.Errorf("migr_name_list: got %v, want empty", got)
	}
}

// TestCreateMigrationRefusals pins §8.11's error table, each with nothing
// written — the destination must never be charged for a request that fails.
func TestCreateMigrationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(env *volEnv)
		srcSideId uint64
		want      codes.Code
	}{
		{
			name:      "unknown source side",
			srcSideId: 999999,
			want:      codes.NotFound,
		},
		{
			name: "the leg already carries a destination",
			setup: func(env *volEnv) {
				volCreateMigration(env, "migr-a")
			},
			srcSideId: volDataSideA,
			want:      codes.FailedPrecondition,
		},
		{
			name: "cntlid_slot_list offers no other slot",
			setup: func(env *volEnv) {
				conf := env.spConf()
				conf.CntlidSlotList = []uint32{volSlot}
				env.putSpConf(conf)
			},
			srcSideId: volDataSideA,
			want:      codes.FailedPrecondition,
		},
		{
			name: "the per-SP migration ceiling",
			setup: func(env *volEnv) {
				conf := env.spConf()
				for idx := 0; idx < common.MaxMigrCntPerSp; idx++ {
					conf.MigrNameList = append(
						conf.MigrNameList, fmt.Sprintf("filler-%d", idx))
				}
				env.putSpConf(conf)
			},
			srcSideId: volDataSideA,
			want:      codes.ResourceExhausted,
		},
		{
			name: "the name is already taken",
			setup: func(env *volEnv) {
				conf := env.spConf()
				conf.MigrNameList = []string{"migr-b"}
				env.putSpConf(conf)
			},
			srcSideId: volDataSideA,
			want:      codes.AlreadyExists,
		},
		{
			name:      "no disk node outside the group",
			setup:     func(env *volEnv) {},
			srcSideId: volDataSideA,
			want:      codes.ResourceExhausted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			selector := volDnSelector(volDnC)
			if tc.name == "no disk node outside the group" {
				// dn-a already carries a leg of this group, so the black
				// list seeded from the group leaves the white list empty.
				selector = volDnSelector(volDnA)
			}
			before := env.spConf()
			beforeRev := env.spRev()
			dnBefore := env.dn(volDnC)
			_, err := env.srv.CreateMigration(
				env.ctx, &pb.CreateMigrationRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					MigrName:    "migr-b",
					SrcSideId:   tc.srcSideId,
					DnSelector:  selector,
				})
			volWantCode(t, err, tc.want)
			env.wantUntouched(before, beforeRev)
			if got := env.dn(volDnC); !proto.Equal(got, dnBefore) {
				t.Errorf("dn-c moved on a refusal:\n got %v\nwant %v",
					got, dnBefore)
			}
		})
	}
}

// TestAppendMigrationBitmapIsAppendOnly pins §8.11's append rule: a chunk
// lands at bm_idx = the CURRENT bm_cnt and the count then advances, so a
// written chunk is immutable and the index is a consequence of how many chunks
// exist rather than a request field. The bytes are verbatim (GW14, [D-J]).
func TestAppendMigrationBitmapIsAppendOnly(t *testing.T) {
	env := newVolEnv(t)
	created := volCreateMigration(env, "migr-a")
	payloads := [][]byte{{0x01}, {0x02, 0x03}, {0x04}, {0x05}}
	for idx, payload := range payloads {
		reply, err := env.srv.AppendMigrationBitmap(
			env.ctx, &pb.AppendMigrationBitmapRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				MigrName:    "migr-a",
				Bitmap:      payload,
			})
		if err != nil {
			t.Fatalf("AppendMigrationBitmap %d: %v", idx, err)
		}
		if reply.GetMigrId() != created.GetMigrId() {
			t.Errorf("reply migr_id: got %d, want %d",
				reply.GetMigrId(), created.GetMigrId())
		}
		chunk := &pb.MigrBitmap{}
		env.get(model.MigrBitmapKey(
			env.cid, volSpId, "migr-a", uint32(idx)), chunk)
		if fmt.Sprint(chunk.GetBitmap()) != fmt.Sprint(payload) {
			t.Errorf("chunk %d: got %v, want %v",
				idx, chunk.GetBitmap(), payload)
		}
		if got := env.migration("migr-a").GetBmCnt(); got != uint32(idx)+1 {
			t.Errorf("bm_cnt: got %d, want %d", got, idx+1)
		}
	}
	// The first chunk is still the first payload: appends never rewrite.
	first := &pb.MigrBitmap{}
	env.get(model.MigrBitmapKey(env.cid, volSpId, "migr-a", 0), first)
	if fmt.Sprint(first.GetBitmap()) != fmt.Sprint(payloads[0]) {
		t.Errorf("chunk 0: got %v, want %v", first.GetBitmap(), payloads[0])
	}
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.AppendMigrationBitmap(
		env.ctx, &pb.AppendMigrationBitmapRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       &pb.SpRev{Revision: beforeRev},
			MigrName:    "migr-a",
			Bitmap:      []byte{0x06},
		})
	volWantCode(t, err, codes.ResourceExhausted)
	env.wantUntouched(before, beforeRev)
	if got := env.migration("migr-a").GetBmCnt(); got != common.MaxMigrBmCnt {
		t.Errorf("bm_cnt: got %d, want %d", got, common.MaxMigrBmCnt)
	}
}

// TestGetMigration pins §8.11's read: the record is replied verbatim, bm_cnt
// included, which is what tells a caller how many chunks it has appended.
func TestGetMigration(t *testing.T) {
	env := newVolEnv(t)
	created := volCreateMigration(env, "migr-a")
	reply, err := env.srv.GetMigration(env.ctx, &pb.GetMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		MigrName:    "migr-a",
	})
	if err != nil {
		t.Fatalf("GetMigration: %v", err)
	}
	if reply.GetMigr().GetMigrId() != created.GetMigrId() {
		t.Errorf("migr_id: got %d, want %d",
			reply.GetMigr().GetMigrId(), created.GetMigrId())
	}
	if reply.GetMigr().GetSrcSideId() != volDataSideA {
		t.Errorf("src_side_id: got %d, want %d",
			reply.GetMigr().GetSrcSideId(), volDataSideA)
	}
	if _, err := env.srv.GetMigration(env.ctx, &pb.GetMigrationRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		MigrName:    "nope",
	}); volWantCode(t, err, codes.NotFound) == "" {
		t.Errorf("a NOT_FOUND must carry a message")
	}
}

// ---------------------------------------------------------------------------
// §8.12 spare legs
// ---------------------------------------------------------------------------

// volCreateSpareLeg parks one spare on dn-c for the fixture's data group and
// returns its leg_id.
func volCreateSpareLeg(env *volEnv) uint64 {
	env.t.Helper()
	reply, err := env.srv.CreateSpareLeg(env.ctx, &pb.CreateSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		GrpId:       volDataGrpId,
		DnSelector:  volDnSelector(volDnC),
	})
	if err != nil {
		env.t.Fatalf("CreateSpareLeg: %v", err)
	}
	return reply.GetLegId()
}

// TestCreateSpareLegAppendsStandbyCapacity pins §8.12's allocation: the spare
// joins the EXISTING group and inherits its geometry unchanged, so its side is
// charged the group's ext_cnt on a DN the group does not already occupy, its
// leg_idx is the next unused member slot, and its side is provisioned = false
// ([D15]) — which is exactly why a fresh spare cannot be switched in at once.
func TestCreateSpareLegAppendsStandbyCapacity(t *testing.T) {
	env := newVolEnv(t)
	legId := volCreateSpareLeg(env)
	if legId != volNextId {
		t.Fatalf("reply leg_id: got %d, want %d", legId, volNextId)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	if len(grp.GetLegList()) != 2 {
		t.Errorf("the active legs must not move: %v", grp.GetLegList())
	}
	want := &pb.Leg{
		LegId:  volNextId,
		LegIdx: 2,
		SideList: []*pb.Side{{
			SideId:      volNextId + 1,
			AddrPort:    volDnC,
			CntlidSlot:  volSlot,
			NvmeTrConf:  volTrConf(volDnC),
			Provisioned: false,
		}},
	}
	if len(grp.GetSpareLegList()) != 1 ||
		!proto.Equal(grp.GetSpareLegList()[0], want) {
		t.Fatalf("spare_leg_list: got %v, want [%v]",
			grp.GetSpareLegList(), want)
	}
	dn := env.dn(volDnC)
	if dn.GetFreeExtCnt() != volDnFree-volDataExtCnt {
		t.Errorf("dn-c free_ext_cnt: got %d, want %d",
			dn.GetFreeExtCnt(), volDnFree-volDataExtCnt)
	}
	if len(dn.GetSidePtrList()) != 1 {
		t.Errorf("dn-c side_ptr_list: got %v", dn.GetSidePtrList())
	}
	if got := env.dnRev(volDnC); got != 2 {
		t.Errorf("dn-c rev: got %d, want 2", got)
	}
	for _, addrPort := range []string{volCnA, volCnB} {
		if got := env.cnRev(addrPort); got != 1 {
			t.Errorf("%s rev: got %d, want 1 (a spare charges no CN)",
				addrPort, got)
		}
	}
	conf := env.spConf()
	if conf.GetNextId() != volNextId+2 {
		t.Errorf("next_id: got %d, want %d (leg_id then side_id)",
			conf.GetNextId(), volNextId+2)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// volSpareLegDst runs CreateSpareLeg on the data group with NO NodeSelector at
// all and returns the addr_port the spare's single side landed on.
func volSpareLegDst(env *volEnv) (string, error) {
	env.t.Helper()
	if _, err := env.srv.CreateSpareLeg(
		env.ctx, &pb.CreateSpareLegRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			SpRev:       env.token(),
			GrpId:       volDataGrpId,
		}); err != nil {
		return "", err
	}
	grp := volGrpOf(env.t, env.slice(), volDataGrpId)
	if len(grp.GetSpareLegList()) != 1 ||
		len(grp.GetSpareLegList()[0].GetSideList()) != 1 {
		env.t.Fatalf("spare_leg_list = %v", grp.GetSpareLegList())
	}
	return grp.GetSpareLegList()[0].GetSideList()[0].GetAddrPort(), nil
}

// TestCreateSpareLegPrefersAnotherFailureDomain pins §6.5's two tiers on
// §8.12's scan: a spare in the failure domain of the leg it exists to replace
// dies with it, so tier 1 excludes the group's domains — dn-c is skipped for
// sharing dn-a's, not for carrying a side — while tier 2 still creates the
// spare when the cluster has no other domain to offer, because no spare at all
// is the worse outcome.
func TestCreateSpareLegPrefersAnotherFailureDomain(t *testing.T) {
	t.Run("tier 1 avoids the group's domains", func(t *testing.T) {
		// One draw proves nothing here — see volTier1DrawCnt.
		for draw := 0; draw < volTier1DrawCnt; draw++ {
			env := volTwoDomainEnv(t)
			got, err := volSpareLegDst(env)
			if err != nil {
				t.Fatalf("CreateSpareLeg: %v", err)
			}
			if got != volDnD {
				t.Fatalf(
					"draw %d: spare on %s, want %s (dn-c is in dn-a's "+
						"domain, and tier 2 must not have run)",
					draw, got, volDnD)
			}
		}
	})

	t.Run("tier 2 places in an occupied domain", func(t *testing.T) {
		env := volTwoDomainEnv(t)
		env.dropDn(volDnD)
		got, err := volSpareLegDst(env)
		if err != nil {
			t.Fatalf("CreateSpareLeg: %v (tier 2 must place, never "+
				"RESOURCE_EXHAUSTED)", err)
		}
		if got != volDnC {
			t.Errorf("spare on %s, want %s", got, volDnC)
		}
		if got := env.dn(volDnC).GetFreeExtCnt(); got !=
			volDnCFree-volDataExtCnt {
			t.Errorf("dn-c free_ext_cnt: got %d, want %d",
				got, volDnCFree-volDataExtCnt)
		}
	})
}

// TestCreateSpareLegRefusals pins the three §8.12 pre-checks that give this
// RPC its own codes: model raises every one of its preconditions as
// FAILED_PRECONDITION, so a RedundNone group and a full spare list have to be
// judged before the op is called if they are to be INVALID_ARGUMENT and
// RESOURCE_EXHAUSTED.
func TestCreateSpareLegRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv)
		grpId uint64
		want  codes.Code
	}{
		{
			name: "the SP has no redundancy to repair",
			setup: func(env *volEnv) {
				conf := env.spConf()
				conf.BdevConf.RedundConf = &pb.RedundConf{
					RedunKind: &pb.RedundConf_RedundNone{
						RedundNone: &pb.RedundNone{},
					},
				}
				env.putSpConf(conf)
			},
			grpId: volDataGrpId,
			want:  codes.InvalidArgument,
		},
		{
			name: "the spare list is full",
			setup: func(env *volEnv) {
				slice := env.slice()
				grp := volGrpOf(env.t, slice, volDataGrpId)
				for idx := 0; idx < common.MaxSpareLegPerGrp; idx++ {
					grp.SpareLegList = append(grp.SpareLegList, &pb.Leg{
						LegId:  uint64(500 + idx),
						LegIdx: uint32(2 + idx),
						SideList: []*pb.Side{{
							SideId:   uint64(510 + idx),
							AddrPort: volDnD,
						}},
					})
				}
				env.putSlice(slice)
			},
			grpId: volDataGrpId,
			want:  codes.ResourceExhausted,
		},
		{
			name:  "unknown group",
			grpId: 999999,
			want:  codes.NotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.spConf()
			beforeRev := env.spRev()
			sliceBefore := env.slice()
			_, err := env.srv.CreateSpareLeg(
				env.ctx, &pb.CreateSpareLegRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					GrpId:       tc.grpId,
					DnSelector:  volDnSelector(volDnC),
				})
			volWantCode(t, err, tc.want)
			env.wantUntouched(before, beforeRev)
			if got := env.slice(); !proto.Equal(got, sliceBefore) {
				t.Errorf("the slice moved on a refusal: %v", got)
			}
			if got := env.dnRev(volDnC); got != 1 {
				t.Errorf("dn-c rev: got %d, want 1", got)
			}
		})
	}
}

// TestDeleteSpareLegReturnsCapacity pins §8.12's delete: the spare leaves
// spare_leg_list and its side gives the DN back exactly what it took — one
// DnConf write, one capacity key and one DnRev bump (§5.5, §5.6) — under one
// SpRev bump. No CN is touched: a spare leg changes no group's ext_cnt.
func TestDeleteSpareLegReturnsCapacity(t *testing.T) {
	env := newVolEnv(t)
	sliceBefore := env.slice()
	dnBefore := env.dn(volDnC)
	legId := volCreateSpareLeg(env)
	reply, err := env.srv.DeleteSpareLeg(env.ctx, &pb.DeleteSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		GrpId:       volDataGrpId,
		LegId:       legId,
	})
	if err != nil {
		t.Fatalf("DeleteSpareLeg: %v", err)
	}
	if reply.GetLegId() != legId {
		t.Errorf("reply leg_id: got %d, want %d", reply.GetLegId(), legId)
	}
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("slice: got %v, want the fixture back", got)
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnBefore) {
		t.Errorf("dn-c: got %v, want %v", got, dnBefore)
	}
	if !env.exists(env.dnCapKey(volDnC, volDnFree), &pb.DnCapacity{}) {
		t.Errorf("dn-c's original capacity key must be back")
	}
	if got := env.dnRev(volDnC); got != 3 {
		t.Errorf("dn-c rev: got %d, want 3 (charge then release)", got)
	}
	if rev := env.spRev(); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3", rev)
	}
}

// TestDeleteSpareLegNeverTouchesAnActiveLeg pins the reason DeleteSpareLeg
// searches spare_leg_list alone: a mistyped id must never name the ACTIVE leg,
// the one leg that must not be released.
func TestDeleteSpareLegNeverTouchesAnActiveLeg(t *testing.T) {
	for _, tc := range []struct {
		name  string
		legId uint64
	}{
		{name: "an active leg id", legId: volDataLegA},
		{name: "an unknown leg id", legId: 999999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			sliceBefore := env.slice()
			before := env.spConf()
			beforeRev := env.spRev()
			_, err := env.srv.DeleteSpareLeg(
				env.ctx, &pb.DeleteSpareLegRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					GrpId:       volDataGrpId,
					LegId:       tc.legId,
				})
			volWantCode(t, err, codes.NotFound)
			env.wantUntouched(before, beforeRev)
			if got := env.slice(); !proto.Equal(got, sliceBefore) {
				t.Errorf("the slice moved on a refusal: %v", got)
			}
			if got := env.dn(volDnA).GetFreeExtCnt(); got != volDnFree {
				t.Errorf("dn-a free_ext_cnt: got %d, want %d", got, volDnFree)
			}
		})
	}
}

// TestSwitchSpareLegRefusesAnUnprovisionedSpare pins §9.4's precondition, the
// one a caller meets in practice: a side that has not finished zeroing would
// put an unwritten member into the md array, so the switch is
// FAILED_PRECONDITION until the sp-worker has flipped `provisioned` — and it
// leaves the group exactly as it was.
func TestSwitchSpareLegRefusesAnUnprovisionedSpare(t *testing.T) {
	env := newVolEnv(t)
	legId := volCreateSpareLeg(env)
	sliceBefore := env.slice()
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.SwitchSpareLeg(env.ctx, &pb.SwitchSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		GrpId:       volDataGrpId,
		SpareLegId:  legId,
		TargetLegId: volDataLegA,
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "provisioned") {
		t.Errorf("message %q must name the zeroing precondition", msg)
	}
	env.wantUntouched(before, beforeRev)
	if got := env.slice(); !proto.Equal(got, sliceBefore) {
		t.Errorf("the slice moved on a refusal: %v", got)
	}
}

// TestSwitchSpareLegSwapsPositions pins §8.12's swap: the spare takes the
// target's POSITION in leg_list — the md member slot the array is missing —
// and the target is parked in spare_leg_list, still connected and probed but
// never repaired again. The reply is the two request ids exchanged.
func TestSwitchSpareLegSwapsPositions(t *testing.T) {
	env := newVolEnv(t)
	legId := volCreateSpareLeg(env)
	// Play the sp-worker: the zeroing of §9.4 has finished. A direct put, so
	// the SP's revision — and therefore the token below — does not move.
	slice := env.slice()
	spare := spareLegOf(volGrpOf(t, slice, volDataGrpId), legId)
	spare.GetSideList()[0].Provisioned = true
	env.putSlice(slice)

	beforeRev := env.spRev()
	reply, err := env.srv.SwitchSpareLeg(env.ctx, &pb.SwitchSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		GrpId:       volDataGrpId,
		SpareLegId:  legId,
		TargetLegId: volDataLegA,
	})
	if err != nil {
		t.Fatalf("SwitchSpareLeg: %v", err)
	}
	if reply.GetCurrActiveLegId() != legId ||
		reply.GetCurrSpareLegId() != volDataLegA {
		t.Errorf("reply: got active %d spare %d, want %d and %d",
			reply.GetCurrActiveLegId(), reply.GetCurrSpareLegId(),
			legId, volDataLegA)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	if len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[0].GetLegId() != legId {
		t.Errorf("leg_list: got %v, want the spare in position 0",
			grp.GetLegList())
	}
	if grp.GetLegList()[1].GetLegId() != volDataLegB {
		t.Errorf("the other active leg must not move: %v", grp.GetLegList())
	}
	if len(grp.GetSpareLegList()) != 1 ||
		grp.GetSpareLegList()[0].GetLegId() != volDataLegA {
		t.Errorf("spare_leg_list: got %v, want the parked target",
			grp.GetSpareLegList())
	}
	if got := env.spRev(); got != beforeRev+1 {
		t.Errorf("sp_rev: got %d, want %d", got, beforeRev+1)
	}
	if got := env.dnRev(volDnC); got != 2 {
		t.Errorf("dn-c rev: got %d, want 2 (a switch moves no capacity)", got)
	}
}

// TestSwitchSpareLegUnknownIds pins §8.12's NOT_FOUND: the pre-read checks
// both ids for membership in the right list, so an id that is in neither is a
// NOT_FOUND rather than the FAILED_PRECONDITION model would raise for it.
func TestSwitchSpareLegUnknownIds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spare  bool
		target bool
	}{
		{name: "unknown spare leg", spare: false, target: true},
		{name: "unknown target leg", spare: true, target: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			legId := volCreateSpareLeg(env)
			spareId := uint64(999999)
			if tc.spare {
				spareId = legId
			}
			targetId := uint64(999999)
			if tc.target {
				targetId = volDataLegA
			}
			before := env.spConf()
			beforeRev := env.spRev()
			_, err := env.srv.SwitchSpareLeg(
				env.ctx, &pb.SwitchSpareLegRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       &pb.SpRev{Revision: beforeRev},
					GrpId:       volDataGrpId,
					SpareLegId:  spareId,
					TargetLegId: targetId,
				})
			volWantCode(t, err, codes.NotFound)
			env.wantUntouched(before, beforeRev)
		})
	}
}

// ---------------------------------------------------------------------------
// GW6 / §0 #7: the revision token
// ---------------------------------------------------------------------------

// volSeedTokenFixture writes one of every object the token cases address,
// directly rather than through the RPCs, so that the SP's revision is still 1
// when the first case runs and every case's token arithmetic starts from the
// same number. Sharing ONE seeded environment across all of them is a luxury
// only the refusal test below has: a refused mutator writes nothing (EU4),
// whereas a mutator whose revision check was skipped runs and mutates, which is
// why the bypass test seeds a fresh environment per case.
func volSeedTokenFixture(env *volEnv) {
	env.t.Helper()
	env.putTd("vol", 900, 7, 0, true)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{{
		NsId: 601, NsIdx: 1, TdId: 900,
	}})
	mustPut(env.t, env.cli, model.CloneKey(env.cid, volSpId, "clone-a"),
		&pb.Clone{CloneId: 701, DstTdId: 900, SrcSliceCnt: 1})
	mustPut(env.t, env.cli, model.TransferKey(env.cid, volSpId, "xfer-a"),
		&pb.Transfer{XferId: 702, OriNqn: volNqn, OriNsIdx: 1})
	mustPut(env.t, env.cli, model.MigrationKey(env.cid, volSpId, "migr-a"),
		&pb.Migration{
			MigrId: 703, SrcSideId: volDataSideA, DstSideId: volDataSideB,
		})
	conf := env.spConf()
	conf.CloneNameList = []string{"clone-a"}
	conf.XferNameList = []string{"xfer-a"}
	conf.MigrNameList = []string{"migr-a"}
	env.putSpConf(conf)
}

// volTokenCase is one SP-scoped mutator of §8.7–§8.12 as the two GW6 tests
// below drive it: once with present tokens that cannot match the stored
// revision (refused, every one of them), and once with no token message at all
// (the revision comparison is skipped and the mutator is judged only by its own
// preconditions).
type volTokenCase struct {
	// name is the RPC, and the subtest's name in both tests.
	name string
	// call issues the RPC with rev as its sp_rev field. A nil rev is a
	// request that carries NO token message — the case GW6 now lets
	// through — and not a token whose revision happens to be 0.
	call func(env *volEnv, rev *pb.SpRev) error
	// bypassCode is what the RPC returns against volSeedTokenFixture once
	// the revision comparison has been skipped: codes.OK for the mutators
	// the fixture lets run cleanly, and the mutator's OWN refusal for the
	// ones whose other preconditions the fixture does not satisfy. Naming it
	// per case is what makes the bypass test positive evidence rather than
	// "some error came back".
	bypassCode codes.Code
	// bypassMsg is the sentence that accompanies a non-OK bypassCode, held
	// exactly: it names the precondition that did the refusing. The code
	// alone would not be enough — FinishMigration's precondition answers
	// ABORTED, the same code GW6 refuses with — so the message is what
	// separates a mutator's own verdict from a revision check that never
	// should have run.
	bypassMsg string
}

// volTokenCases is the mutator list both GW6 tests run: every SP-scoped
// mutator of §8.7–§8.12, each written against the state volSeedTokenFixture
// seeds — one shared seeding for the refusal test, a fresh one per case for the
// bypass test. The two tests share this one list on purpose: a mutator added to
// the API is then either covered by both or by neither, and the strict half can
// never quietly outgrow the bypass half.
func volTokenCases() []volTokenCase {
	return []volTokenCase{
		{
			name: "CreateThinDevice",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateThinDevice(
					env.ctx, &pb.CreateThinDeviceRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, TdName: "td-x", Size: volTdSize,
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "DeleteThinDevice",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.DeleteThinDevice(
					env.ctx, &pb.DeleteThinDeviceRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, TdName: "vol",
					})
				return err
			},
			// The seeded td backs the seeded namespace, so §8.7's in-use gate
			// refuses it — a check that lives far behind the revision one.
			bypassCode: codes.FailedPrecondition,
			bypassMsg: fmt.Sprintf(
				"thin device vol backs namespace 1 of subsystem %s", volNqn),
		},
		{
			name: "CreateSubsystem",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateSubsystem(
					env.ctx, &pb.CreateSubsystemRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqnB,
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "DeleteSubsystem",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.DeleteSubsystem(
					env.ctx, &pb.DeleteSubsystemRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqn,
					})
				return err
			},
			// The seeded subsystem still holds its namespace, which is §8.8's
			// own refusal and not the revision check's.
			bypassCode: codes.FailedPrecondition,
			bypassMsg: fmt.Sprintf(
				"subsystem %q still holds 1 namespaces", volNqn),
		},
		{
			name: "UpdateSubsystemHosts",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.UpdateSubsystemHosts(
					env.ctx, &pb.UpdateSubsystemHostsRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqn,
						AllowedHosts: []string{volHostA},
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "CreateNamespace",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateNamespace(
					env.ctx, &pb.CreateNamespaceRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqn, NsIdx: 3, TdName: "vol",
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "DeleteNamespace",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.DeleteNamespace(
					env.ctx, &pb.DeleteNamespaceRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqn, NsIdx: 1,
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "UpdateNamespaceDev",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.UpdateNamespaceDev(
					env.ctx, &pb.UpdateNamespaceDevRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqn, NsIdx: 1, TdName: "vol",
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "UpdateNamespaceSuspended",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.UpdateNamespaceSuspended(
					env.ctx, &pb.UpdateNamespaceSuspendedRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, Nqn: volNqn, NsIdx: 1, Suspended: true,
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "CreateClone",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateClone(
					env.ctx, &pb.CreateCloneRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, CloneName: "clone-x",
						SrcTrConf:   []*pb.NvmeTrConf{volTrConf(volCnA)},
						SrcNqn:      volSrcNqn,
						SrcSliceCnt: 1, SrcStripeSize: 64 * 1024,
						SrcBlockSize: 1024 * 1024, DstTdName: "vol",
					})
				return err
			},
			// The seeded td is already clone-a's destination, and §8.9 allows
			// one clone per thin device.
			bypassCode: codes.FailedPrecondition,
			bypassMsg: "thin device \"vol\" is already the destination " +
				"of clone \"clone-a\"",
		},
		{
			name: "DeleteClone",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.DeleteClone(
					env.ctx, &pb.DeleteCloneRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, CloneName: "clone-a", Force: true,
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "UpdateCloneTrConf",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.UpdateCloneTrConf(
					env.ctx, &pb.UpdateCloneTrConfRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, CloneName: "clone-a",
						SrcTrConf: []*pb.NvmeTrConf{volTrConf(volCnB)},
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "AppendCloneBitmap",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.AppendCloneBitmap(
					env.ctx, &pb.AppendCloneBitmapRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, CloneName: "clone-a",
						SliceIdx: 0, Bitmap: []byte{0x01},
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "CreateTransfer",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateTransfer(
					env.ctx, &pb.CreateTransferRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, XferName: "xfer-x",
						OriNqn: volNqn, OriNsIdx: 1,
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "DeleteTransfer",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.DeleteTransfer(
					env.ctx, &pb.DeleteTransferRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, XferName: "xfer-a",
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "UpdateTransferHosts",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.UpdateTransferHosts(
					env.ctx, &pb.UpdateTransferHostsRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, XferName: "xfer-a",
						AllowedHosts: []string{volHostA},
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "CreateMigration",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateMigration(
					env.ctx, &pb.CreateMigrationRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, MigrName: "migr-x",
						SrcSideId:  volDataSideA,
						DnSelector: volDnSelector(volDnC),
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "FinishMigration",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.FinishMigration(
					env.ctx, &pb.FinishMigrationRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, MigrName: "migr-a", Force: true,
					})
				return err
			},
			// The seeded migration names the two sides of the fixture's data
			// group, which are in DIFFERENT legs, so §8.11's finish refuses it.
			// This is the one bypass refusal that is itself ABORTED, and it is
			// why both tests compare the MESSAGE and not only the code: an
			// ABORTED here is a real precondition talking, not GW6.
			bypassCode: codes.Aborted,
			bypassMsg: fmt.Sprintf(
				"migration %q sides %d and %d are not in one leg",
				"migr-a", volDataSideA, volDataSideB),
		},
		{
			name: "CancelMigration",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CancelMigration(
					env.ctx, &pb.CancelMigrationRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, MigrName: "migr-a",
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "AppendMigrationBitmap",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.AppendMigrationBitmap(
					env.ctx, &pb.AppendMigrationBitmapRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, MigrName: "migr-a", Bitmap: []byte{0x01},
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "CreateSpareLeg",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.CreateSpareLeg(
					env.ctx, &pb.CreateSpareLegRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, GrpId: volDataGrpId,
						DnSelector: volDnSelector(volDnC),
					})
				return err
			},
			bypassCode: codes.OK,
		},
		{
			name: "DeleteSpareLeg",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.DeleteSpareLeg(
					env.ctx, &pb.DeleteSpareLegRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, GrpId: volDataGrpId, LegId: volDataLegA,
					})
				return err
			},
			// leg_a is an ACTIVE leg of the group, never a spare, so §8.12's
			// lookup in spare_leg_list misses it.
			bypassCode: codes.NotFound,
			bypassMsg: fmt.Sprintf(
				"spare leg %d not found in group %d",
				volDataLegA, volDataGrpId),
		},
		{
			name: "SwitchSpareLeg",
			call: func(env *volEnv, rev *pb.SpRev) error {
				_, err := env.srv.SwitchSpareLeg(
					env.ctx, &pb.SwitchSpareLegRequest{
						ClusterName: env.cluster, SpName: volSpName,
						SpRev: rev, GrpId: volDataGrpId,
						SpareLegId: 1, TargetLegId: volDataLegA,
					})
				return err
			},
			// Spare id 1 is nothing the fixture ever wrote, so §8.12's lookup
			// in spare_leg_list misses it.
			bypassCode: codes.NotFound,
			bypassMsg: fmt.Sprintf(
				"spare leg 1 not found in group %d", volDataGrpId),
		},
	}
}

// TestVolumeMutatorsRefuseAPresentStaleToken is GW6 and §0 #7 over every
// SP-scoped mutator of §8.7–§8.12, for the half of the rule that refuses: a
// request that DOES carry a token message is held to strict equality with the
// stored revision, and every value that is not it — a revision left far behind,
// the proto zero value, and a message that echoes only the sp_name handle —
// is the same ABORTED "stale revision".
//
// The last two rows are the discriminators. Stored revisions seed at 1 and only
// grow, so 0 can never match; they are here to prove that what GW6 keys on is
// the PRESENCE of the message and not the value 0, because they travel the same
// zero revision as the absent-token requests of the test below and are refused
// where those are let through. The second of them also pins that the echoed
// sp_name does not participate: only `revision` is compared.
//
// The check runs immediately after resolution and before every other state
// check, so none of these cases may see a NOT_FOUND or a FAILED_PRECONDITION
// computed against state the client has not read — which is also why all of
// them can share one environment: a refused mutator writes nothing (EU4), so
// the closing assertion is that the whole fixture, revision included, is
// exactly as volSeedTokenFixture left it.
func TestVolumeMutatorsRefuseAPresentStaleToken(t *testing.T) {
	env := newVolEnv(t)
	volSeedTokenFixture(env)
	before := env.spConf()
	beforeRev := env.spRev()
	for _, tc := range volTokenCases() {
		for _, token := range []struct {
			name string
			rev  *pb.SpRev
		}{
			{
				name: "stale token",
				rev:  &pb.SpRev{Revision: beforeRev + 99},
			},
			{
				name: "present but zero token",
				rev:  &pb.SpRev{},
			},
			{
				name: "present without a revision",
				rev:  &pb.SpRev{SpName: volSpName},
			},
		} {
			t.Run(tc.name+"/"+token.name, func(t *testing.T) {
				msg := volWantCode(t, tc.call(env, token.rev), codes.Aborted)
				if msg != msgStaleRevision {
					t.Errorf("message: got %q, want %q",
						msg, msgStaleRevision)
				}
			})
		}
	}
	env.wantUntouched(before, beforeRev)
}

// TestVolumeMutatorsWithoutATokenSkipTheCheck is the other half of GW6: a
// request that carries NO token message at all has its revision comparison
// skipped, and the mutator then runs on its other preconditions alone. That is
// a bypass, not a refusal, so it is asserted positively — for each mutator,
// exactly the outcome its own §8 rules produce against volSeedTokenFixture:
// a clean run and a single §5.5 bump for the seventeen the fixture satisfies,
// and that mutator's OWN code and sentence for the six it does not. What may
// never come back is ABORTED "stale revision"; FinishMigration's case shows why
// the message and not only the code has to be compared, since its precondition
// speaks ABORTED too.
//
// Each case gets a FRESH environment, which is the price of the new rule: these
// calls MUTATE. Sharing one fixture the way the refusal test above does would
// let CreateThinDevice's write decide what DeleteThinDevice sees and let
// DeleteClone's success turn the next case's lookup into a NOT_FOUND.
func TestVolumeMutatorsWithoutATokenSkipTheCheck(t *testing.T) {
	for _, tc := range volTokenCases() {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			volSeedTokenFixture(env)
			before := env.spConf()
			beforeRev := env.spRev()
			err := tc.call(env, nil)
			// The one sentence no case may produce, checked before the
			// per-case expectation so that a GW6 regression is reported as
			// itself rather than as whichever code it displaced.
			if st, ok := status.FromError(err); ok &&
				st.Code() == codes.Aborted &&
				st.Message() == msgStaleRevision {
				t.Fatalf("got ABORTED %q, but the request carried no token "+
					"for GW6 to compare it against", msgStaleRevision)
			}
			if tc.bypassCode == codes.OK {
				if err != nil {
					t.Fatalf("an absent token must not refuse: %v", err)
				}
				// The bump is what makes the success load-bearing here: the
				// mutator ran to the end of its transaction, wrote, and told
				// the workers so (§5.5). WHAT it wrote is pinned by that
				// mutator's own test above; this one owns the revision.
				if got := env.spRev(); got != beforeRev+1 {
					t.Errorf("sp_rev: got %d, want %d "+
						"(a bypassed mutator still bumps exactly once)",
						got, beforeRev+1)
				}
				return
			}
			if msg := volWantCode(t, err, tc.bypassCode); msg != tc.bypassMsg {
				t.Errorf("message: got %q, want %q", msg, tc.bypassMsg)
			}
			// The mutator's own refusal is still a refusal: EU4 rolls the
			// transaction back whole, so nothing moved and — unlike the
			// successes above — the revision did not bump either.
			env.wantUntouched(before, beforeRev)
		})
	}
}
