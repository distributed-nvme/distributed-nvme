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
