package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// growRulesBlock is the pool block the covered-rule cases count in.
const growRulesBlock = uint64(4 << 20)

// growRulesState is a pool of sliceCnt slices whose td_name_list names one
// thin device per size, each held, so DataCovered sums the sizes.
func growRulesState(sliceCnt int, sizes ...uint64) *SpState {
	state := newSpState()
	state.Conf = &pb.SpConf{}
	for idx := 0; idx < sliceCnt; idx++ {
		state.Conf.SliceIdList = append(
			state.Conf.SliceIdList, uint64(100+idx))
	}
	for idx, size := range sizes {
		name := string(rune('a' + idx))
		state.Conf.TdNameList = append(state.Conf.TdNameList, name)
		state.Tds = append(state.Tds, &pb.ThinDevice{Size: size})
		state.TdNames = append(state.TdNames, name)
	}
	return state
}

// growRulesSlice is a slice whose data groups hold the given data blocks.
func growRulesSlice(dataBlocks ...uint64) *pb.Slice {
	slice := &pb.Slice{}
	for _, blocks := range dataBlocks {
		slice.DataGrpList = append(slice.DataGrpList,
			&pb.Group{ExtCnt: 4, DataBlocks: blocks})
	}
	return slice
}

// TestDataCovered pins AR6's covered rule as model holds it (dnv-worker.md
// AR6), the rule the sp worker's pass and the gateway's status read share:
// the data blocks of every data group of the slice against the blocks each
// thin device can use in one slice, rounded up; a thin device listed but not
// held, a zero block and a pool of no slice hold the rule off.
func TestDataCovered(t *testing.T) {
	// Two slices: a 40 MiB td needs 20 MiB per slice, five 4 MiB blocks,
	// and a 6 MiB one 3 MiB per slice, rounded up to one block: six in all.
	state := growRulesState(2, 40<<20, 6<<20)
	for _, tc := range []struct {
		label string
		slice *pb.Slice
		want  bool
	}{
		{"short by one block", growRulesSlice(2, 3), false},
		{"exactly the need over two groups", growRulesSlice(2, 4), true},
		{"more than the need", growRulesSlice(9), true},
		{"no data group", growRulesSlice(), false},
	} {
		if got := DataCovered(
			state, tc.slice, growRulesBlock,
		); got != tc.want {
			t.Errorf("%s: DataCovered = %v, want %v", tc.label, got, tc.want)
		}
	}

	whole := growRulesSlice(100)
	if DataCovered(state, whole, 0) {
		t.Errorf("a zero block size: DataCovered = true, want false")
	}
	lost := growRulesState(2, 40<<20)
	lost.Conf.TdNameList = append(lost.Conf.TdNameList, "gone")
	if DataCovered(lost, whole, growRulesBlock) {
		t.Errorf("a listed td not held: DataCovered = true, want false")
	}
	if DataCovered(growRulesState(0, 40<<20), whole, growRulesBlock) {
		t.Errorf("a pool of no slice: DataCovered = true, want false")
	}
	if !DataCovered(growRulesState(2), growRulesSlice(), growRulesBlock) {
		t.Errorf("a pool of no td: DataCovered = false, want true")
	}
}

// TestGrowExtCnt pins the size and the refusals of the group a grow appends
// (architecture.md, GrowSlice) as GrowExtCnt gives them to the sp worker's
// pass and the gateway's status read: the first data group's ext_cnt for a
// data grow, the ladder's doubling for a meta grow, the group ceiling ahead
// of either, and the two reasons of a grow with nothing to size.
func TestGrowExtCnt(t *testing.T) {
	extent := uint64(common.DefaultDnExtSize)
	full := &pb.Slice{}
	for idx := 0; idx < common.MaxGrpCntPerSlice; idx++ {
		full.DataGrpList = append(full.DataGrpList, &pb.Group{ExtCnt: 1})
		full.MetaGrpList = append(full.MetaGrpList, &pb.Group{ExtCnt: 1})
	}
	capped := &pb.Slice{MetaGrpList: []*pb.Group{
		{ExtCnt: metaSizeCap/extent + 1},
	}}
	for _, tc := range []struct {
		label      string
		slice      *pb.Slice
		isMeta     bool
		wantExt    uint64
		wantReason string
	}{
		{"data: the first group's size",
			&pb.Slice{DataGrpList: []*pb.Group{{ExtCnt: 6}, {ExtCnt: 2}}},
			false, 6, ""},
		{"data: no group", &pb.Slice{}, false, 0, ReasonNoDataGroup},
		{"data: an empty first group",
			&pb.Slice{DataGrpList: []*pb.Group{{}}},
			false, 0, ReasonNoDataGroup},
		{"data: the list full", full, false, 0, ReasonGrpListFull},
		{"meta: the total doubles",
			&pb.Slice{MetaGrpList: []*pb.Group{{ExtCnt: 1}, {ExtCnt: 2}}},
			true, 3, ""},
		{"meta: no group", &pb.Slice{}, true, 0, ReasonMetaLadderCap},
		{"meta: at the cap", capped, true, 0, ReasonMetaLadderCap},
		{"meta: the list full", full, true, 0, ReasonGrpListFull},
	} {
		gotExt, gotReason := GrowExtCnt(tc.slice, tc.isMeta, extent)
		if gotExt != tc.wantExt || gotReason != tc.wantReason {
			t.Errorf("%s: GrowExtCnt = (%d, %q), want (%d, %q)",
				tc.label, gotExt, gotReason, tc.wantExt, tc.wantReason)
		}
	}
}

// TestRepairWaitReason pins why the primary's report holds a repair back
// (dnv-worker.md AR17), the text the sp worker logs as `repair waits` and the
// gateway's status read lists: none once the verdict says started or abort
// confirmed, the verdict's waits text first, and for a CREATED repair with no
// such text the group's row when it reads other than OK.
func TestRepairWaitReason(t *testing.T) {
	const repId, grpId = 31, 7
	created := &pb.Repair{RepId: repId, GrpId: grpId}
	started := &pb.Repair{
		RepId: repId, GrpId: grpId, RepStage: pb.RepStage_REP_STAGE_STARTED,
	}
	info := func(verdict *pb.CntlrInfo_RepairInfo, row *pb.ResInfo) *pb.CntlrInfo {
		out := &pb.CntlrInfo{
			RepIdToRepairInfo: map[uint64]*pb.CntlrInfo_RepairInfo{},
			GrpIdToMdRaid:     map[uint64]*pb.ResInfo{},
		}
		if verdict != nil {
			out.RepIdToRepairInfo[repId] = verdict
		}
		if row != nil {
			out.GrpIdToMdRaid[grpId] = row
		}
		return out
	}
	errRow := &pb.ResInfo{
		Status: pb.ResStatus_RES_STATUS_ERROR, Details: "assemble failed",
	}
	for _, tc := range []struct {
		label string
		info  *pb.CntlrInfo
		rep   *pb.Repair
		want  string
	}{
		{"no report", nil, created, ""},
		{"started",
			info(&pb.CntlrInfo_RepairInfo{Started: true, Waits: "w"}, errRow),
			created, ""},
		{"abort confirmed",
			info(&pb.CntlrInfo_RepairInfo{AbortConfirmed: true}, errRow),
			created, ""},
		{"the waits text first",
			info(&pb.CntlrInfo_RepairInfo{Waits: "no member in sync"}, errRow),
			created, "no member in sync"},
		{"the group's row",
			info(nil, errRow), created,
			"the group's row reads RES_STATUS_ERROR: assemble failed"},
		{"the group's row with no details",
			info(nil, &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_MISSING}),
			created, "the group's row reads RES_STATUS_MISSING"},
		{"the group's row OK", info(nil,
			&pb.ResInfo{Status: pb.ResStatus_RES_STATUS_OK}), created, ""},
		{"no row of the group", info(nil, nil), created, ""},
		{"a started repair's group row", info(nil, errRow), started, ""},
		{"a started repair's waits",
			info(&pb.CntlrInfo_RepairInfo{Waits: "bad leg held"}, nil),
			started, "bad leg held"},
	} {
		if got := RepairWaitReason(tc.info, tc.rep); got != tc.want {
			t.Errorf("%s: RepairWaitReason = %q, want %q",
				tc.label, got, tc.want)
		}
	}
}
