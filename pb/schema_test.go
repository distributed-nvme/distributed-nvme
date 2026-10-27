package pb_test

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestSyncupCntlrRequestReservesMigrList pins the reservation of the cntlr
// request's migration list. The cn agent stores its requests as binary
// protobuf, so a field that took number 13 would read a stored request's
// migrations as its own value, silently; the name is reserved with the
// number, so no field takes it either. The test reads the descriptor and
// decodes nothing: a decode can pass whether or not a field has taken the
// number, since a value of another wire type goes to unknown fields with no
// error.
func TestSyncupCntlrRequestReservesMigrList(t *testing.T) {
	md := (&pb.SyncupCntlrRequest{}).ProtoReflect().Descriptor()
	if !md.ReservedRanges().Has(13) {
		t.Errorf("%s does not reserve field number 13", md.FullName())
	}
	if fd := md.Fields().ByNumber(13); fd != nil {
		t.Errorf("%s has field %s at the reserved number 13",
			md.FullName(), fd.Name())
	}
	if !md.ReservedNames().Has("migr_list") {
		t.Errorf("%s does not reserve the name migr_list", md.FullName())
	}
	if fd := md.Fields().ByName("migr_list"); fd != nil {
		t.Errorf("%s has a field named migr_list at %d",
			md.FullName(), fd.Number())
	}
}

// TestGroupReservesFieldSix pins the reservation of the group's field 6 and
// of its name, spare_leg_list. A group is stored in its slice's and its
// clone's records, so a field that took number 6 would read a stored group's
// old entries as its own value, silently; the name is reserved with the
// number, so no field takes it either. The test reads the descriptor and
// decodes nothing, for TestSyncupCntlrRequestReservesMigrList's reason.
func TestGroupReservesFieldSix(t *testing.T) {
	md := (&pb.Group{}).ProtoReflect().Descriptor()
	if !md.ReservedRanges().Has(6) {
		t.Errorf("%s does not reserve field number 6", md.FullName())
	}
	if fd := md.Fields().ByNumber(6); fd != nil {
		t.Errorf("%s has field %s at the reserved number 6",
			md.FullName(), fd.Name())
	}
	if !md.ReservedNames().Has("spare_leg_list") {
		t.Errorf("%s does not reserve the name spare_leg_list",
			md.FullName())
	}
	if fd := md.Fields().ByName("spare_leg_list"); fd != nil {
		t.Errorf("%s has a field named spare_leg_list at %d",
			md.FullName(), fd.Number())
	}
}

// TestRowReasonAndTornMarkNumbers pins the numbers and kinds of the row's
// reason and of the two torn marks. Each is appended: a renumbered field
// decodes an old value into the wrong field, or into unknown fields, with no
// error.
func TestRowReasonAndTornMarkNumbers(t *testing.T) {
	for _, tc := range []struct {
		md     protoreflect.MessageDescriptor
		name   protoreflect.Name
		number protoreflect.FieldNumber
		kind   protoreflect.Kind
	}{
		{(&pb.ResInfo{}).ProtoReflect().Descriptor(),
			"reason", 5, protoreflect.EnumKind},
		{(&pb.CntlrInfo_ThinInfo{}).ProtoReflect().Descriptor(),
			"torn", 2, protoreflect.BoolKind},
		{(&pb.ThinDevice{}).ProtoReflect().Descriptor(),
			"torn", 6, protoreflect.BoolKind},
	} {
		fd := tc.md.Fields().ByName(tc.name)
		if fd == nil {
			t.Errorf("%s has no field %s", tc.md.FullName(), tc.name)
			continue
		}
		if fd.Number() != tc.number || fd.Kind() != tc.kind {
			t.Errorf("%s.%s is %v at %d, want %v at %d", tc.md.FullName(),
				tc.name, fd.Kind(), fd.Number(), tc.kind, tc.number)
		}
	}
	reason := (&pb.ResInfo{}).ProtoReflect().Descriptor().
		Fields().ByName("reason").Enum()
	for _, v := range []struct {
		name   protoreflect.Name
		number protoreflect.EnumNumber
	}{
		{"RES_REASON_NONE", 0},
		{"RES_REASON_THIN_ID_LOST", 1},
	} {
		ev := reason.Values().ByName(v.name)
		if ev == nil || ev.Number() != v.number {
			t.Errorf("%s.%s is %v, want the number %d",
				reason.FullName(), v.name, ev, v.number)
		}
	}
}

// TestDnInfoDiskCountsNumbers pins the numbers and kinds of the disk node's
// counts: the field DnInfo carries them in, and the five fields of the nested
// message. Each is appended; a renumbered field decodes an old value into the
// wrong field, or into unknown fields, with no error.
func TestDnInfoDiskCountsNumbers(t *testing.T) {
	info := (&pb.DnInfo{}).ProtoReflect().Descriptor()
	counts := (&pb.DnInfo_DiskCounts{}).ProtoReflect().Descriptor()
	if counts.Parent() != info {
		t.Errorf("%s is not nested in %s", counts.FullName(), info.FullName())
	}
	for _, tc := range []struct {
		md     protoreflect.MessageDescriptor
		name   protoreflect.Name
		number protoreflect.FieldNumber
		kind   protoreflect.Kind
	}{
		{info, "disk_counts", 4, protoreflect.MessageKind},
		{counts, "total_ext_cnt", 1, protoreflect.Uint64Kind},
		{counts, "free_ext_cnt", 2, protoreflect.Uint64Kind},
		{counts, "side_rec_cnt", 3, protoreflect.Uint64Kind},
		{counts, "free_meta_units", 4, protoreflect.Uint64Kind},
		{counts, "side_recs_match", 5, protoreflect.BoolKind},
	} {
		fd := tc.md.Fields().ByName(tc.name)
		if fd == nil {
			t.Errorf("%s has no field %s", tc.md.FullName(), tc.name)
			continue
		}
		if fd.Number() != tc.number || fd.Kind() != tc.kind {
			t.Errorf("%s.%s is %v at %d, want %v at %d", tc.md.FullName(),
				tc.name, fd.Kind(), fd.Number(), tc.kind, tc.number)
		}
	}
	if fd := info.Fields().ByName("disk_counts"); fd != nil &&
		fd.Message() != counts {
		t.Errorf("DnInfo.disk_counts is a %v, want %s",
			fd.Message(), counts.FullName())
	}
	if got := counts.Fields().Len(); got != 5 {
		t.Errorf("%s has %d fields, want 5", counts.FullName(), got)
	}
}

// TestCloneMetaGrpNumber pins the clone's group: one Group, not a list, at
// number 14, the number after the retired metadata units. It is appended: a
// field renumbered onto a stored record's number decodes that value into the
// wrong field, or into unknown fields, with no error, and a list in its place
// would decode an old record's group as a list of one.
func TestCloneMetaGrpNumber(t *testing.T) {
	md := (&pb.Clone{}).ProtoReflect().Descriptor()
	grp := (&pb.Group{}).ProtoReflect().Descriptor()
	fd := md.Fields().ByName("meta_grp")
	if fd == nil {
		t.Fatalf("%s has no field meta_grp", md.FullName())
	}
	if fd.Number() != 14 || fd.Kind() != protoreflect.MessageKind ||
		fd.Cardinality() != protoreflect.Optional {
		t.Errorf("%s.meta_grp is %v %v at %d, want a single message at 14",
			md.FullName(), fd.Cardinality(), fd.Kind(), fd.Number())
	}
	if fd.Message() != grp {
		t.Errorf("%s.meta_grp is a %v, want %s", md.FullName(),
			fd.Message().FullName(), grp.FullName())
	}
}

// TestRetiredCnMetadataFieldsStayReserved pins the reservations of the
// retired fields of a controller node's clone metadata: the clone's units,
// the node's used units on its record and its capacity value, the node's
// tmpfs, file and loop rows, and the cntlr's clone metadata row. Each keeps its
// number and its name reserved, so no field takes either: a field on a
// retired number would decode a stored record or an older agent's row as its
// own value, silently. The test reads the descriptors and decodes nothing.
func TestRetiredCnMetadataFieldsStayReserved(t *testing.T) {
	for _, tc := range []struct {
		md     protoreflect.MessageDescriptor
		name   protoreflect.Name
		number protoreflect.FieldNumber
	}{
		{(&pb.Clone{}).ProtoReflect().Descriptor(), "meta_units", 13},
		{(&pb.CnConf{}).ProtoReflect().Descriptor(), "used_meta_units", 10},
		{(&pb.CnCapacity{}).ProtoReflect().Descriptor(), "used_meta_units", 2},
		{(&pb.CnInfo{}).ProtoReflect().Descriptor(), "tmpfs_info", 2},
		{(&pb.CnInfo{}).ProtoReflect().Descriptor(), "tmp_file_info", 3},
		{(&pb.CnInfo{}).ProtoReflect().Descriptor(), "loop_dev_info", 4},
		{(&pb.CntlrInfo{}).ProtoReflect().Descriptor(), "clone_id_to_meta", 17},
	} {
		if !tc.md.ReservedRanges().Has(tc.number) {
			t.Errorf("%s does not reserve field number %d",
				tc.md.FullName(), tc.number)
		}
		if fd := tc.md.Fields().ByNumber(tc.number); fd != nil {
			t.Errorf("%s has field %s at the reserved number %d",
				tc.md.FullName(), fd.Name(), tc.number)
		}
		if !tc.md.ReservedNames().Has(tc.name) {
			t.Errorf("%s does not reserve the name %s",
				tc.md.FullName(), tc.name)
		}
		if fd := tc.md.Fields().ByName(tc.name); fd != nil {
			t.Errorf("%s has a field named %s at %d",
				tc.md.FullName(), tc.name, fd.Number())
		}
	}
	// The disk node keeps its units for the migrations: only the controller
	// node's half is retired.
	for _, md := range []protoreflect.MessageDescriptor{
		(&pb.DnConf{}).ProtoReflect().Descriptor(),
		(&pb.DnCapacity{}).ProtoReflect().Descriptor(),
	} {
		if md.Fields().ByName("used_meta_units") == nil {
			t.Errorf("%s has no field used_meta_units", md.FullName())
		}
	}
}

// TestRepairFieldNumbers pins the numbers, kinds and cardinalities of the
// repair records and of the fields that carry them: the Repair and RetiredLeg
// messages, which are stored, the group's and the pool's links to them, the
// group's forced-start mark, which etcd and the cn agent's stored request
// carry, the cntlr request's repairs and the cntlr report's verdicts, the
// take-back refusal among them, and group readings, which an agent stores
// or a worker keeps. Each is appended: a
// renumbered field decodes an old value into the wrong field, or into unknown
// fields, with no error. The stage's values are pinned too, since a stored
// record carries them as numbers.
func TestRepairFieldNumbers(t *testing.T) {
	repair := (&pb.Repair{}).ProtoReflect().Descriptor()
	retired := (&pb.RetiredLeg{}).ProtoReflect().Descriptor()
	grp := (&pb.Group{}).ProtoReflect().Descriptor()
	conf := (&pb.SpConf{}).ProtoReflect().Descriptor()
	syncup := (&pb.SyncupCntlrRequest{}).ProtoReflect().Descriptor()
	info := (&pb.CntlrInfo{}).ProtoReflect().Descriptor()
	repInfo := (&pb.CntlrInfo_RepairInfo{}).ProtoReflect().Descriptor()
	grpInfo := (&pb.CntlrInfo_GrpInfo{}).ProtoReflect().Descriptor()
	forced := (&pb.ForcedStart{}).ProtoReflect().Descriptor()
	for _, nested := range []protoreflect.MessageDescriptor{repInfo, grpInfo} {
		if nested.Parent() != info {
			t.Errorf("%s is not nested in %s", nested.FullName(),
				info.FullName())
		}
	}
	const (
		single = protoreflect.Optional
		list   = protoreflect.Repeated
	)
	for _, tc := range []struct {
		md          protoreflect.MessageDescriptor
		name        protoreflect.Name
		number      protoreflect.FieldNumber
		kind        protoreflect.Kind
		cardinality protoreflect.Cardinality
		isMap       bool
	}{
		{repair, "rep_id", 1, protoreflect.Uint64Kind, single, false},
		{repair, "grp_id", 2, protoreflect.Uint64Kind, single, false},
		{repair, "rep_stage", 3, protoreflect.EnumKind, single, false},
		{repair, "new_leg_list", 4, protoreflect.MessageKind, list, false},
		{repair, "bad_leg_id_list", 5, protoreflect.Uint64Kind, list, false},
		{repair, "try_to_abort", 6, protoreflect.BoolKind, single, false},
		{repair, "slice_id", 7, protoreflect.Uint64Kind, single, false},
		{repair, "clone_name", 8, protoreflect.StringKind, single, false},
		{repair, "new_leg_id_to_retired_epoch", 9,
			protoreflect.MessageKind, list, true},
		{retired, "grp_id", 1, protoreflect.Uint64Kind, single, false},
		{retired, "leg_idx", 2, protoreflect.Uint32Kind, single, false},
		{retired, "side", 3, protoreflect.MessageKind, single, false},
		{retired, "retired_epoch", 4, protoreflect.Uint64Kind, single, false},
		{retired, "slice_id", 5, protoreflect.Uint64Kind, single, false},
		{retired, "clone_name", 6, protoreflect.StringKind, single, false},
		{grp, "rep_name", 7, protoreflect.StringKind, single, false},
		{grp, "retired_leg_id_list", 8, protoreflect.Uint64Kind, list, false},
		{grp, "forced_start", 9, protoreflect.MessageKind, single, false},
		{forced, "leg_id", 1, protoreflect.Uint64Kind, single, false},
		{forced, "revision", 2, protoreflect.Uint64Kind, single, false},
		{conf, "repair_name_list", 17, protoreflect.StringKind, list, false},
		{conf, "retired_leg_id_list", 18, protoreflect.Uint64Kind, list,
			false},
		{syncup, "name_to_repair", 14, protoreflect.MessageKind, list, true},
		{info, "rep_id_to_repair_info", 19, protoreflect.MessageKind, list,
			true},
		{info, "grp_id_to_grp_info", 20, protoreflect.MessageKind, list,
			true},
		{repInfo, "started", 1, protoreflect.BoolKind, single, false},
		{repInfo, "abort_confirmed", 2, protoreflect.BoolKind, single, false},
		{repInfo, "refusal", 3, protoreflect.StringKind, single, false},
		{grpInfo, "whole", 1, protoreflect.BoolKind, single, false},
		{grpInfo, "in_sync_leg_id_list", 2, protoreflect.Uint64Kind, list,
			false},
	} {
		fd := tc.md.Fields().ByName(tc.name)
		if fd == nil {
			t.Errorf("%s has no field %s", tc.md.FullName(), tc.name)
			continue
		}
		if fd.Number() != tc.number || fd.Kind() != tc.kind ||
			fd.Cardinality() != tc.cardinality || fd.IsMap() != tc.isMap {
			t.Errorf("%s.%s is %v %v (map %v) at %d, want %v %v (map %v) "+
				"at %d", tc.md.FullName(), tc.name, fd.Cardinality(),
				fd.Kind(), fd.IsMap(), fd.Number(), tc.cardinality, tc.kind,
				tc.isMap, tc.number)
		}
	}
	stage := repair.Fields().ByName("rep_stage").Enum()
	for _, v := range []struct {
		name   protoreflect.Name
		number protoreflect.EnumNumber
	}{
		{"REP_STAGE_CREATED", 0},
		{"REP_STAGE_STARTED", 1},
		{"REP_STAGE_ABORTED", 2},
	} {
		ev := stage.Values().ByName(v.name)
		if ev == nil || ev.Number() != v.number {
			t.Errorf("%s.%s is %v, want the number %d",
				stage.FullName(), v.name, ev, v.number)
		}
	}
	if got := stage.Values().Len(); got != 3 {
		t.Errorf("%s has %d values, want 3", stage.FullName(), got)
	}
}

// TestFinishRepairReplyNumbers pins the numbers and kinds of FinishRepair's
// reply. dnvctl and the gateway are separate binaries that need not be built
// from one tree, and the reply's last field, the retired leg the finish gave
// back, is appended: a renumbered field decodes the other side's value into
// the wrong field, or into unknown fields, with no error, and an operator
// would be told the wrong leg was given back, or none.
func TestFinishRepairReplyNumbers(t *testing.T) {
	md := (&pb.FinishRepairReply{}).ProtoReflect().Descriptor()
	for _, tc := range []struct {
		name   protoreflect.Name
		number protoreflect.FieldNumber
	}{
		{"rep_id", 1},
		{"retired_leg_id", 2},
		{"given_back_leg_id", 3},
	} {
		fd := md.Fields().ByName(tc.name)
		if fd == nil {
			t.Errorf("%s has no field %s", md.FullName(), tc.name)
			continue
		}
		if fd.Number() != tc.number ||
			fd.Kind() != protoreflect.Uint64Kind ||
			fd.Cardinality() != protoreflect.Optional {
			t.Errorf("%s.%s is %v %v at %d, want a single uint64 at %d",
				md.FullName(), tc.name, fd.Cardinality(), fd.Kind(),
				fd.Number(), tc.number)
		}
	}
	if got := md.Fields().Len(); got != 3 {
		t.Errorf("%s has %d fields, want 3", md.FullName(), got)
	}
}
