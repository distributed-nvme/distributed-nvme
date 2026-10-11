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
