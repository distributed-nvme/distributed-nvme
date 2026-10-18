// Tests of the library's helpers that need no etcd: the transport record
// and the namespace identity it plants, the sub-object id rules, how
// StartCutover names a migration and its cntlr, and where the epoch setters
// find their object.
package plant

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

func TestNvmeTrConfOf(t *testing.T) {
	conf := nvmeTrConfOf("192.168.10.20:29600")
	if conf.GetTrAddr() != "192.168.10.20" {
		t.Fatalf("tr_addr = %q", conf.GetTrAddr())
	}
	// The node's own port, so two nodes on one host get distinct
	// NvmeTrConfs and a CdcEntry's transport list stays attributable
	// (worker_test.sh case D step 3).
	if conf.GetTrSvcId() != "29600" {
		t.Fatalf("tr_svc_id = %q, want %q", conf.GetTrSvcId(), "29600")
	}
	if other := nvmeTrConfOf("192.168.10.20:29700"); other.GetTrSvcId() ==
		conf.GetTrSvcId() {
		t.Fatalf("two nodes on one host share an NvmeTrConf")
	}
	// An address with no port still yields a usable default.
	if bare := nvmeTrConfOf("192.168.10.20"); bare.GetTrSvcId() != nvmeTrSvcId {
		t.Fatalf("bare addr tr_svc_id = %q", bare.GetTrSvcId())
	}
	if conf.GetTrType() != "tcp" || conf.GetAdrFam() != "ipv4" {
		t.Fatalf("tr_type/adr_fam = %q/%q",
			conf.GetTrType(), conf.GetAdrFam())
	}
}

func TestNsIdentityIsDeterministic(t *testing.T) {
	uuid, nguid := nsIdentity(1, 2, 3)
	again, againNguid := nsIdentity(1, 2, 3)
	if uuid != again || nguid != againNguid {
		t.Fatalf("nsIdentity is not deterministic")
	}
	other, _ := nsIdentity(1, 2, 4)
	if uuid == other {
		t.Fatalf("two namespaces share one uuid")
	}
	if len(nguid) != 32 {
		t.Fatalf("nguid %q is not 16 bytes of hex", nguid)
	}
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 {
		t.Fatalf("uuid %q is not RFC 4122 shaped", uuid)
	}
	if uuid[14] != '4' {
		t.Fatalf("uuid %q is not version 4", uuid)
	}
}

func TestContainsNameAndAdvanceNextId(t *testing.T) {
	if !containsName([]string{"a", "b"}, "b") {
		t.Fatalf("containsName missed an entry")
	}
	if containsName([]string{"a", "b"}, "c") {
		t.Fatalf("containsName invented an entry")
	}
	conf := &pb.SpConf{NextId: 5}
	advanceNextId(conf, 3)
	if conf.NextId != 5 {
		t.Fatalf("next_id went backwards to %d", conf.NextId)
	}
	advanceNextId(conf, 5, 9)
	if conf.NextId != 10 {
		t.Fatalf("next_id = %d, want 10", conf.NextId)
	}
}

// TestCutoverRefOf pins how StartCutover names a migration for
// model.StartCutover: the record's id and sides, and the slice and leg that
// carry both sides — found among the spare legs and in a later slice too —
// and a refusal for a name with no record and for sides that share no leg.
func TestCutoverRefOf(t *testing.T) {
	state := &model.SpState{
		Conf: &pb.SpConf{SliceIdList: []uint64{1, 2}},
		Slices: map[uint64]*pb.Slice{
			1: {MetaGrpList: []*pb.Group{{
				LegList: []*pb.Leg{{
					LegId:    11,
					SideList: []*pb.Side{{SideId: 101}},
				}},
			}}},
			2: {DataGrpList: []*pb.Group{{
				LegList: []*pb.Leg{{
					LegId:    21,
					SideList: []*pb.Side{{SideId: 201}},
				}},
				SpareLegList: []*pb.Leg{{
					LegId: 22,
					SideList: []*pb.Side{
						{SideId: 202}, {SideId: 203},
					},
				}},
			}}},
		},
		Migrs: map[string]*pb.Migration{
			"m0": {MigrId: 7, SrcSideId: 202, DstSideId: 203},
			"m1": {MigrId: 8, SrcSideId: 101, DstSideId: 201},
		},
	}
	ref, err := cutoverRefOf(state, "m0")
	if err != nil {
		t.Fatalf("cutoverRefOf(m0): %v", err)
	}
	want := model.CutoverRef{
		MigrName: "m0", MigrId: 7, SliceId: 2, LegId: 22,
		SrcSideId: 202, DstSideId: 203,
	}
	if ref != want {
		t.Errorf("cutoverRefOf(m0) = %+v, want %+v", ref, want)
	}
	if _, err := cutoverRefOf(state, "m1"); err == nil ||
		!strings.Contains(err.Error(), "share no leg") {
		t.Errorf("cutoverRefOf(m1) = %v, want a share-no-leg refusal", err)
	}
	if _, err := cutoverRefOf(state, "nosuch"); err == nil ||
		!strings.Contains(err.Error(), "no migration") {
		t.Errorf("cutoverRefOf(nosuch) = %v, want a no-migration refusal",
			err)
	}
}

// TestStoredPrimary pins the cntlr StartCutover names by default: the one
// listed cntlr whose record says primary, and a refusal for none or two.
func TestStoredPrimary(t *testing.T) {
	stateOf := func(primaries ...bool) *model.SpState {
		state := &model.SpState{
			Conf:   &pb.SpConf{},
			Cntlrs: map[uint64]*pb.Cntlr{},
		}
		for i, primary := range primaries {
			cntlrId := uint64(i + 1)
			state.Conf.CntlrIdList = append(state.Conf.CntlrIdList, cntlrId)
			state.Cntlrs[cntlrId] = &pb.Cntlr{Primary: primary}
		}
		return state
	}
	got, err := storedPrimary(stateOf(false, true, false))
	if err != nil || got != 2 {
		t.Errorf("storedPrimary = %d, %v; want 2", got, err)
	}
	for _, primaries := range [][]bool{{false, false}, {true, true}} {
		if got, err := storedPrimary(stateOf(primaries...)); err == nil {
			t.Errorf("storedPrimary(%v) = %d, want a refusal",
				primaries, got)
		}
	}
}

// TestSetSliceEpoch pins where SetLegEpoch and SetSideEpoch find a leg or a
// side: in either group list and on an active or a spare leg, as the health
// ops do, touching that one object and nothing else.
func TestSetSliceEpoch(t *testing.T) {
	newSlice := func() *pb.Slice {
		return &pb.Slice{
			MetaGrpList: []*pb.Group{{
				GrpId: 1,
				LegList: []*pb.Leg{{
					LegId: 1, SideList: []*pb.Side{{SideId: 11}},
				}},
				SpareLegList: []*pb.Leg{{
					LegId: 2, ErrEpoch: 7,
					SideList: []*pb.Side{{SideId: 12, ErrEpoch: 8}},
				}},
			}},
			DataGrpList: []*pb.Group{{
				GrpId: 2,
				LegList: []*pb.Leg{{
					LegId: 3, SideList: []*pb.Side{{SideId: 13}},
				}},
			}},
		}
	}
	cases := []struct {
		kind  string
		id    uint64
		old   uint64
		found bool
		// read returns the epoch the case set, from a fresh walk.
		read func(*pb.Slice) uint64
	}{
		{"leg", 2, 7, true, func(s *pb.Slice) uint64 {
			return s.GetMetaGrpList()[0].GetSpareLegList()[0].GetErrEpoch()
		}},
		{"leg", 3, 0, true, func(s *pb.Slice) uint64 {
			return s.GetDataGrpList()[0].GetLegList()[0].GetErrEpoch()
		}},
		{"side", 12, 8, true, func(s *pb.Slice) uint64 {
			return s.GetMetaGrpList()[0].GetSpareLegList()[0].
				GetSideList()[0].GetErrEpoch()
		}},
		{"side", 13, 0, true, func(s *pb.Slice) uint64 {
			return s.GetDataGrpList()[0].GetLegList()[0].
				GetSideList()[0].GetErrEpoch()
		}},
		{"leg", 11, 0, false, nil},
		{"side", 3, 0, false, nil},
		{"cntlr", 1, 0, false, nil},
	}
	for _, tc := range cases {
		slice := newSlice()
		old, found := setSliceEpoch(slice, tc.kind, tc.id, 42)
		if found != tc.found || old != tc.old {
			t.Errorf("setSliceEpoch(%s %d) = %d, %v; want %d, %v",
				tc.kind, tc.id, old, found, tc.old, tc.found)
			continue
		}
		if !found {
			if !proto.Equal(slice, newSlice()) {
				t.Errorf("setSliceEpoch(%s %d) found nothing and still "+
					"changed the slice: %v", tc.kind, tc.id, slice)
			}
			continue
		}
		if got := tc.read(slice); got != 42 {
			t.Errorf("setSliceEpoch(%s %d) left %d, want 42",
				tc.kind, tc.id, got)
		}
		// Undo the one write: what remains must be the fixture.
		setSliceEpoch(slice, tc.kind, tc.id, tc.old)
		if !proto.Equal(slice, newSlice()) {
			t.Errorf("setSliceEpoch(%s %d) changed more than its object: %v",
				tc.kind, tc.id, slice)
		}
	}
}

// TestTakeId pins how a sub-object gets its id: a given one is kept, 0 takes
// the pool's next free one (model.SpNextId), and next_id moves past either.
func TestTakeId(t *testing.T) {
	conf := &pb.SpConf{NextId: 7}
	if got := takeId(conf, 0); got != 7 || conf.NextId != 8 {
		t.Fatalf("takeId(0) = %d, next_id %d; want 7, 8", got, conf.NextId)
	}
	if got := takeId(conf, 3); got != 3 || conf.NextId != 8 {
		t.Fatalf("takeId(3) = %d, next_id %d; want 3, 8", got, conf.NextId)
	}
	if got := takeId(conf, 20); got != 20 || conf.NextId != 21 {
		t.Fatalf("takeId(20) = %d, next_id %d; want 20, 21", got,
			conf.NextId)
	}
	// A pool whose next_id was never set starts at model.SpFirstId.
	fresh := &pb.SpConf{}
	if got := takeId(fresh, 0); got != model.SpFirstId ||
		fresh.NextId != model.SpFirstId+1 {
		t.Fatalf("takeId(0) on a fresh pool = %d, next_id %d; want %d, %d",
			got, fresh.NextId, model.SpFirstId, model.SpFirstId+1)
	}
}
