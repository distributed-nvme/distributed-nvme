package pb_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The schema freeze holds pb/schema.proto to the layout of every release
// (architecture.md, Upgrade in lockstep): each pb/testdata/<tag>/schema.binpb
// is the file descriptor of that release's schema, written once by that
// release's own generated code (protodesc.ToFileDescriptorProto of its
// File_pb_schema_proto, marshaled deterministically as a FileDescriptorSet of
// one file, in a git archive of the tag) and never refreshed. A binary set
// rather than the old schema text, so the old text's citations meet no doc
// lint and no second file of the same names is compiled.
//
// Every message is held, not only the ones etcd stores: an agent stores its
// syncup requests, which carry most of the others, and dnvctl prints the
// rest as JSON by their field names. Services are not compared: a store
// keeps a message and never the call that carried it, and the lockstep
// upgrade replaces every caller with its server.

// retiredMessages are the messages a release had that the schema has
// removed, each with the reason no build reads it any more. A message
// missing from the schema and from this table breaks the freeze.
var retiredMessages = map[protoreflect.FullName]string{
	"MigrBitmap": "the etcd value of the migration_bitmap kind, which no " +
		"build since v0.1.0 reads; the end of a migration deletes those keys " +
		"(model.EndMigrationIn)",
	"AppendMigrationBitmapRequest": "the message of a removed gateway call; " +
		"no store holds it and the lockstep upgrade replaces every caller",
	"AppendMigrationBitmapReply": "the reply of a removed gateway call",
	"GetLegBitmapRequest": "the message of a removed gateway call; no " +
		"store holds it and the lockstep upgrade replaces every caller",
	"GetLegBitmapReply": "the reply of a removed gateway call",
}

// TestSchemaKeepsEachReleaseLayout holds the schema to the layout of every
// release under pb/testdata.
func TestSchemaKeepsEachReleaseLayout(t *testing.T) {
	sets, err := filepath.Glob(filepath.Join("testdata", "*", "schema.binpb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) == 0 {
		t.Fatal("no release layout under testdata")
	}
	for _, p := range sets {
		frozen := loadFrozenSchema(t, p)
		for _, b := range schemaBreaks(frozen, pb.File_pb_schema_proto, retiredMessages) {
			t.Errorf("%s: %s", p, b)
		}
	}
}

// TestSchemaBreaksCatchesEachUnsafeChange proves the checker: each change
// below, made to a copy of the v0.1.0 layout, is reported exactly once, and
// each safe change is not reported. Every branch of schemaBreaks has a case
// that fails when the branch is taken away.
func TestSchemaBreaksCatchesEachUnsafeChange(t *testing.T) {
	base := loadFrozenProto(t, filepath.Join("testdata", "v0.1.0", "schema.binpb"))

	field := func(f *descriptorpb.FileDescriptorProto, msg, name string) *descriptorpb.FieldDescriptorProto {
		t.Helper()
		m := findMsg(f, msg)
		for _, fd := range m.Field {
			if fd.GetName() == name {
				return fd
			}
		}
		t.Fatalf("no field %s.%s", msg, name)
		return nil
	}
	removeField := func(f *descriptorpb.FileDescriptorProto, msg, name string) *descriptorpb.DescriptorProto {
		t.Helper()
		m := findMsg(f, msg)
		for i, fd := range m.Field {
			if fd.GetName() == name {
				m.Field = append(m.Field[:i], m.Field[i+1:]...)
				return m
			}
		}
		t.Fatalf("no field %s.%s", msg, name)
		return nil
	}
	reserve := func(m *descriptorpb.DescriptorProto, n int32) {
		m.ReservedRange = append(m.ReservedRange,
			&descriptorpb.DescriptorProto_ReservedRange{Start: proto.Int32(n), End: proto.Int32(n + 1)})
	}

	enumReserve := func(e *descriptorpb.EnumDescriptorProto, from, to int32) {
		e.ReservedRange = append(e.ReservedRange,
			&descriptorpb.EnumDescriptorProto_EnumReservedRange{Start: proto.Int32(from), End: proto.Int32(to)})
	}

	for _, tc := range []struct {
		name string
		want string // the one break, in part; empty: none
		// whole holds the break to want exactly.
		whole  bool
		retire map[protoreflect.FullName]string
		// empty compares copies of the layout with no message, enum or
		// service, so a change of the whole file is its one break.
		empty bool
		// frozen, when set, changes the frozen side first, for a case that
		// needs a frozen shape v0.1.0 has not.
		frozen func(f *descriptorpb.FileDescriptorProto)
		mutate func(f *descriptorpb.FileDescriptorProto)
	}{
		{name: "renumber a field", want: "Migration.migr_id = 1 is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "Migration", "migr_id").Number = proto.Int32(90)
		}},
		{name: "change a field's type", want: "type uint64 is now uint32", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "Migration", "meta_units").Type = descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()
		}},
		{name: "change a field's message type", want: "type message BdevConf is now message EventThreshold", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "SpConf", "bdev_conf").TypeName = proto.String(".EventThreshold")
		}},
		{name: "change a field's enum type", want: "type enum ResReason is now enum SpLevel", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "ResInfo", "reason").TypeName = proto.String(".SpLevel")
		}},
		{name: "make a field repeated", want: "cardinality optional is now repeated", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "Migration", "migr_id").Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}},
		{name: "change a map's value type", want: "map value message CntlrInfo.ThinInfo is now message ResInfo", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "TdIdToThinInfoEntry", "value").TypeName = proto.String(".ResInfo")
		}},
		{name: "remove a field unreserved", want: "Migration.migr_id = 1 is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			removeField(f, "Migration", "migr_id")
		}},
		{name: "remove a field reserved by number only", want: "Migration.migr_id = 1 is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			n := field(f, "Migration", "migr_id").GetNumber()
			reserve(removeField(f, "Migration", "migr_id"), n)
		}},
		{name: "remove a field reserved by name only", want: "Migration.migr_id = 1 is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			m := removeField(f, "Migration", "migr_id")
			m.ReservedName = append(m.ReservedName, "migr_id")
		}},
		{name: "remove a field reserved by number and name", mutate: func(f *descriptorpb.FileDescriptorProto) {
			n := field(f, "Migration", "migr_id").GetNumber()
			m := removeField(f, "Migration", "migr_id")
			reserve(m, n)
			m.ReservedName = append(m.ReservedName, "migr_id")
		}},
		{name: "rename a field", want: "name migr_id is now migration_id", mutate: func(f *descriptorpb.FileDescriptorProto) {
			fd := field(f, "Migration", "migr_id")
			fd.Name = proto.String("migration_id")
			fd.JsonName = proto.String("migrationId")
		}},
		{name: "add optional to a field", whole: true,
			// The hidden oneof of the optional is no oneof change.
			want: "field Migration.meta_units = 6: optional false is now true", mutate: func(f *descriptorpb.FileDescriptorProto) {
				m := findMsg(f, "Migration")
				fd := field(f, "Migration", "meta_units")
				fd.Proto3Optional = proto.Bool(true)
				fd.OneofIndex = proto.Int32(int32(len(m.OneofDecl)))
				m.OneofDecl = append(m.OneofDecl,
					&descriptorpb.OneofDescriptorProto{Name: proto.String("_meta_units")})
			}},
		{name: "move a field into a oneof", want: "oneof \"\" is now \"units_kind\"", mutate: func(f *descriptorpb.FileDescriptorProto) {
			m := findMsg(f, "Migration")
			fd := field(f, "Migration", "meta_units")
			fd.OneofIndex = proto.Int32(int32(len(m.OneofDecl)))
			m.OneofDecl = append(m.OneofDecl,
				&descriptorpb.OneofDescriptorProto{Name: proto.String("units_kind")})
		}},
		{name: "drop a reserved number", want: "Group no longer reserves number 6", mutate: func(f *descriptorpb.FileDescriptorProto) {
			m := findMsg(f, "Group")
			if len(m.ReservedRange) != 1 {
				t.Fatalf("Group has %d reserved ranges, want 1", len(m.ReservedRange))
			}
			m.ReservedRange = nil
		}},
		{name: "drop a reserved name", want: "Group no longer reserves name spare_leg_list", mutate: func(f *descriptorpb.FileDescriptorProto) {
			m := findMsg(f, "Group")
			if len(m.ReservedName) != 1 {
				t.Fatalf("Group has %d reserved names, want 1", len(m.ReservedName))
			}
			m.ReservedName = nil
		}},
		{name: "drop an enum value", want: "REP_STAGE_ABORTED = 2 is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			e := findEnum(f, "RepStage")
			e.Value = e.Value[:len(e.Value)-1]
		}},
		{name: "drop an enum value reserved by number and name", mutate: func(f *descriptorpb.FileDescriptorProto) {
			e := findEnum(f, "RepStage")
			last := e.Value[len(e.Value)-1]
			e.Value = e.Value[:len(e.Value)-1]
			e.ReservedRange = append(e.ReservedRange, &descriptorpb.EnumDescriptorProto_EnumReservedRange{
				Start: proto.Int32(last.GetNumber()), End: proto.Int32(last.GetNumber())})
			e.ReservedName = append(e.ReservedName, last.GetName())
		}},
		{name: "rename an enum value", want: "is now named REP_STAGE_RENAMED", mutate: func(f *descriptorpb.FileDescriptorProto) {
			e := findEnum(f, "RepStage")
			e.Value[len(e.Value)-1].Name = proto.String("REP_STAGE_RENAMED")
		}},
		{name: "renumber an enum value", want: "REP_STAGE_ABORTED = 2 is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			e := findEnum(f, "RepStage")
			e.Value[len(e.Value)-1].Number = proto.Int32(90)
		}},
		{name: "remove an enum", want: "enum RepStage is gone", mutate: func(f *descriptorpb.FileDescriptorProto) {
			// Repair.rep_stage holds a RepStage: drop the field with its
			// number and name reserved, so the file builds and the enum is
			// the one break.
			n := field(f, "Repair", "rep_stage").GetNumber()
			m := removeField(f, "Repair", "rep_stage")
			reserve(m, n)
			m.ReservedName = append(m.ReservedName, "rep_stage")
			for i, e := range f.EnumType {
				if e.GetName() == "RepStage" {
					f.EnumType = append(f.EnumType[:i], f.EnumType[i+1:]...)
					return
				}
			}
			t.Fatal("no enum RepStage")
		}},
		{name: "remove a message not retired", want: "message MigrBitmap is gone and not retired", retire: map[protoreflect.FullName]string{},
			mutate: func(f *descriptorpb.FileDescriptorProto) { removeMsg(t, f, "MigrBitmap") }},
		{name: "remove a retired message",
			mutate: func(f *descriptorpb.FileDescriptorProto) { removeMsg(t, f, "MigrBitmap") }},
		// A package moves every full name, and proto2 gives every field
		// presence: an empty copy makes each the one break.
		{name: "a package statement", want: "package \"\" is now \"dnv\"", empty: true,
			mutate: func(f *descriptorpb.FileDescriptorProto) { f.Package = proto.String("dnv") }},
		{name: "another syntax", want: "syntax proto3 is now proto2", empty: true,
			mutate: func(f *descriptorpb.FileDescriptorProto) { f.Syntax = proto.String("proto2") }},
		{name: "renumber a field of a nested message", want: "DnDiskTable.SideRecord.sp_id = 1 is gone",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				field(f, "SideRecord", "sp_id").Number = proto.Int32(90)
			}},
		{name: "change a field's JSON name", want: "JSON name migrId is now mid", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "Migration", "migr_id").JsonName = proto.String("mid")
		}},
		{name: "change a map's key type", want: "map key uint64 is now uint32", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "TdIdToThinInfoEntry", "key").Type = descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()
		}},
		{name: "make a map a list", want: "map true is now false", mutate: func(f *descriptorpb.FileDescriptorProto) {
			field(f, "CntlrInfo", "td_id_to_thin_info").TypeName = proto.String(".CntlrInfo.ThinInfo")
		}},
		{name: "merge reserved numbers into one range", mutate: func(f *descriptorpb.FileDescriptorProto) {
			findMsg(f, "CnInfo").ReservedRange = []*descriptorpb.DescriptorProto_ReservedRange{
				{Start: proto.Int32(2), End: proto.Int32(6)}}
		}},
		{name: "split a reserved range",
			frozen: func(f *descriptorpb.FileDescriptorProto) {
				findMsg(f, "CnInfo").ReservedRange = []*descriptorpb.DescriptorProto_ReservedRange{
					{Start: proto.Int32(2), End: proto.Int32(6)}}
			},
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				findMsg(f, "CnInfo").ReservedRange = []*descriptorpb.DescriptorProto_ReservedRange{
					{Start: proto.Int32(2), End: proto.Int32(4)}, {Start: proto.Int32(4), End: proto.Int32(6)}}
			}},
		{name: "merge reserved numbers into a range short of one", want: "CnInfo no longer reserves number 5",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				findMsg(f, "CnInfo").ReservedRange = []*descriptorpb.DescriptorProto_ReservedRange{
					{Start: proto.Int32(2), End: proto.Int32(5)}}
			}},
		{name: "drop an enum value reserved by number only", want: "REP_STAGE_ABORTED = 2 is gone",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				e := findEnum(f, "RepStage")
				e.Value = e.Value[:len(e.Value)-1]
				enumReserve(e, 2, 2)
			}},
		{name: "drop an enum value reserved by name only", want: "REP_STAGE_ABORTED = 2 is gone",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				e := findEnum(f, "RepStage")
				e.Value = e.Value[:len(e.Value)-1]
				e.ReservedName = append(e.ReservedName, "REP_STAGE_ABORTED")
			}},
		{name: "drop an enum's reserved numbers", want: "RepStage no longer reserves number 90",
			frozen: func(f *descriptorpb.FileDescriptorProto) { enumReserve(findEnum(f, "RepStage"), 90, 92) },
			mutate: func(f *descriptorpb.FileDescriptorProto) { findEnum(f, "RepStage").ReservedRange = nil }},
		{name: "drop an enum's reserved name", want: "RepStage no longer reserves name REP_STAGE_OLD",
			frozen: func(f *descriptorpb.FileDescriptorProto) {
				e := findEnum(f, "RepStage")
				e.ReservedName = append(e.ReservedName, "REP_STAGE_OLD")
			},
			mutate: func(f *descriptorpb.FileDescriptorProto) { findEnum(f, "RepStage").ReservedName = nil }},
		{name: "split an enum's reserved range",
			frozen: func(f *descriptorpb.FileDescriptorProto) { enumReserve(findEnum(f, "RepStage"), 90, 92) },
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				e := findEnum(f, "RepStage")
				e.ReservedRange = nil
				enumReserve(e, 90, 90)
				enumReserve(e, 91, 92)
			}},
		{name: "shorten an enum's reserved range", want: "RepStage no longer reserves number 92",
			frozen: func(f *descriptorpb.FileDescriptorProto) { enumReserve(findEnum(f, "RepStage"), 90, 92) },
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				e := findEnum(f, "RepStage")
				e.ReservedRange = nil
				enumReserve(e, 90, 91)
			}},
		{name: "add a field at a new number", mutate: func(f *descriptorpb.FileDescriptorProto) {
			m := findMsg(f, "Migration")
			m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
				Name: proto.String("added_later"), JsonName: proto.String("addedLater"),
				Number: proto.Int32(90), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type: descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum(),
			})
		}},
		{name: "add an enum value", mutate: func(f *descriptorpb.FileDescriptorProto) {
			e := findEnum(f, "RepStage")
			e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{
				Name: proto.String("REP_STAGE_ADDED_LATER"), Number: proto.Int32(90)})
		}},
		{name: "add a message", mutate: func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("AddedLater")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := proto.Clone(base).(*descriptorpb.FileDescriptorProto)
			if tc.empty {
				old.MessageType, old.EnumType, old.Service = nil, nil, nil
			}
			if tc.frozen != nil {
				tc.frozen(old)
			}
			cur := proto.Clone(old).(*descriptorpb.FileDescriptorProto)
			tc.mutate(cur)
			retire := tc.retire
			if retire == nil {
				retire = retiredMessages
			}
			got := schemaBreaks(buildFile(t, old), buildFile(t, cur), retire)
			checkBreaks(t, got, tc.want)
			if tc.whole && len(got) == 1 && got[0] != tc.want {
				t.Errorf("got break %q, want exactly %q", got[0], tc.want)
			}
		})
	}
}

func checkBreaks(t *testing.T, got []string, want string) {
	t.Helper()
	switch {
	case want == "" && len(got) != 0:
		t.Errorf("got breaks %q, want none", got)
	case want != "" && (len(got) != 1 || !strings.Contains(got[0], want)):
		t.Errorf("got breaks %q, want one that holds %q", got, want)
	}
}

// schemaBreaks lists each place where current breaks the layout of frozen:
// a message or enum that is gone, unless retired names the message; a field
// whose number now holds another name, kind, cardinality, map key or value,
// message or enum type, presence or oneof, or whose number is gone unless
// both the number and the name are reserved; an enum value whose number now
// holds another name, or is gone unless both are reserved; a reserved number
// or name that is no longer reserved; and a package or syntax that changed.
// A proto3 optional sits in a hidden oneof, so presence is read from the
// optional keyword and only a declared oneof counts as one. One field, value
// or reservation gives one entry at most.
func schemaBreaks(frozen, current protoreflect.FileDescriptor, retired map[protoreflect.FullName]string) []string {
	var out []string
	if frozen.Package() != current.Package() {
		out = append(out, fmt.Sprintf("package %q is now %q", frozen.Package(), current.Package()))
	}
	if frozen.Syntax() != current.Syntax() {
		out = append(out, fmt.Sprintf("syntax %v is now %v", frozen.Syntax(), current.Syntax()))
	}
	curMsgs := map[protoreflect.FullName]protoreflect.MessageDescriptor{}
	curEnums := map[protoreflect.FullName]protoreflect.EnumDescriptor{}
	indexDescriptors(current.Messages(), current.Enums(), curMsgs, curEnums)

	var walk func(ms protoreflect.MessageDescriptors, es protoreflect.EnumDescriptors)
	walk = func(ms protoreflect.MessageDescriptors, es protoreflect.EnumDescriptors) {
		for i := 0; i < es.Len(); i++ {
			out = append(out, enumBreaks(es.Get(i), curEnums[es.Get(i).FullName()])...)
		}
		for i := 0; i < ms.Len(); i++ {
			fm := ms.Get(i)
			if fm.IsMapEntry() {
				continue // compared through the map field that owns it
			}
			cm, ok := curMsgs[fm.FullName()]
			if !ok {
				if _, ok := retired[fm.FullName()]; !ok {
					out = append(out, fmt.Sprintf("message %s is gone and not retired", fm.FullName()))
				}
				continue
			}
			out = append(out, messageBreaks(fm, cm)...)
			walk(fm.Messages(), fm.Enums())
		}
	}
	walk(frozen.Messages(), frozen.Enums())
	return out
}

func indexDescriptors(ms protoreflect.MessageDescriptors, es protoreflect.EnumDescriptors,
	msgs map[protoreflect.FullName]protoreflect.MessageDescriptor,
	enums map[protoreflect.FullName]protoreflect.EnumDescriptor) {
	for i := 0; i < es.Len(); i++ {
		enums[es.Get(i).FullName()] = es.Get(i)
	}
	for i := 0; i < ms.Len(); i++ {
		m := ms.Get(i)
		msgs[m.FullName()] = m
		indexDescriptors(m.Messages(), m.Enums(), msgs, enums)
	}
}

func messageBreaks(fm, cm protoreflect.MessageDescriptor) []string {
	var out []string
	for i := 0; i < fm.Fields().Len(); i++ {
		ff := fm.Fields().Get(i)
		cf := cm.Fields().ByNumber(ff.Number())
		if cf == nil {
			if !cm.ReservedRanges().Has(ff.Number()) || !cm.ReservedNames().Has(ff.Name()) {
				out = append(out, fmt.Sprintf("field %s = %d is gone without its number and name reserved",
					ff.FullName(), ff.Number()))
			}
			continue
		}
		if d := fieldDiffs(ff, cf); len(d) > 0 {
			out = append(out, fmt.Sprintf("field %s = %d: %s", ff.FullName(), ff.Number(), strings.Join(d, "; ")))
		}
	}
	out = append(out, reservedBreaks(string(fm.FullName()), fm.ReservedRanges(), cm.ReservedRanges(),
		fm.ReservedNames(), cm.ReservedNames())...)
	return out
}

// fieldDiffs lists what differs between two fields at one number.
func fieldDiffs(ff, cf protoreflect.FieldDescriptor) []string {
	var d []string
	if ff.Name() != cf.Name() {
		d = append(d, fmt.Sprintf("name %s is now %s", ff.Name(), cf.Name()))
	} else if ff.JSONName() != cf.JSONName() {
		d = append(d, fmt.Sprintf("JSON name %s is now %s", ff.JSONName(), cf.JSONName()))
	}
	if ff.Cardinality() != cf.Cardinality() {
		d = append(d, fmt.Sprintf("cardinality %v is now %v", ff.Cardinality(), cf.Cardinality()))
	}
	if ff.IsMap() != cf.IsMap() {
		d = append(d, fmt.Sprintf("map %v is now %v", ff.IsMap(), cf.IsMap()))
	} else if ff.IsMap() {
		if t1, t2 := typeOf(ff.MapKey()), typeOf(cf.MapKey()); t1 != t2 {
			d = append(d, fmt.Sprintf("map key %s is now %s", t1, t2))
		}
		if t1, t2 := typeOf(ff.MapValue()), typeOf(cf.MapValue()); t1 != t2 {
			d = append(d, fmt.Sprintf("map value %s is now %s", t1, t2))
		}
	} else if t1, t2 := typeOf(ff), typeOf(cf); t1 != t2 {
		d = append(d, fmt.Sprintf("type %s is now %s", t1, t2))
	}
	if ff.HasOptionalKeyword() != cf.HasOptionalKeyword() {
		d = append(d, fmt.Sprintf("optional %v is now %v", ff.HasOptionalKeyword(), cf.HasOptionalKeyword()))
	}
	if o1, o2 := oneofOf(ff), oneofOf(cf); o1 != o2 {
		d = append(d, fmt.Sprintf("oneof %q is now %q", o1, o2))
	}
	return d
}

// typeOf names a field's kind and, for a message or an enum, its type.
func typeOf(f protoreflect.FieldDescriptor) string {
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return fmt.Sprintf("%v %s", f.Kind(), f.Message().FullName())
	case protoreflect.EnumKind:
		return fmt.Sprintf("%v %s", f.Kind(), f.Enum().FullName())
	}
	return f.Kind().String()
}

// oneofOf names a field's declared oneof; the hidden oneof of a proto3
// optional is no oneof here.
func oneofOf(f protoreflect.FieldDescriptor) string {
	if o := f.ContainingOneof(); o != nil && !o.IsSynthetic() {
		return string(o.Name())
	}
	return ""
}

func enumBreaks(fe, ce protoreflect.EnumDescriptor) []string {
	if ce == nil {
		return []string{fmt.Sprintf("enum %s is gone", fe.FullName())}
	}
	var out []string
	for i := 0; i < fe.Values().Len(); i++ {
		fv := fe.Values().Get(i)
		cv := ce.Values().ByNumber(fv.Number())
		switch {
		case cv == nil:
			if !ce.ReservedRanges().Has(fv.Number()) || !ce.ReservedNames().Has(fv.Name()) {
				out = append(out, fmt.Sprintf("enum value %s = %d is gone without its number and name reserved",
					fv.FullName(), fv.Number()))
			}
		case cv.Name() != fv.Name():
			out = append(out, fmt.Sprintf("enum value %s = %d is now named %s", fv.FullName(), fv.Number(), cv.Name()))
		}
	}
	out = append(out, enumReservedBreaks(string(fe.FullName()), fe.ReservedRanges(), ce.ReservedRanges(),
		fe.ReservedNames(), ce.ReservedNames())...)
	return out
}

// reservedBreaks lists each frozen reservation of a message the current
// one no longer holds. A range may be split or widened; every number of it
// must stay reserved.
func reservedBreaks(owner string, fr, cr protoreflect.FieldRanges, fn, cn protoreflect.Names) []string {
	var out []string
	for i := 0; i < fr.Len(); i++ {
		r := fr.Get(i) // [start, end)
		for n := r[0]; n < r[1]; {
			end, ok := protoreflect.FieldNumber(0), false
			for j := 0; j < cr.Len(); j++ {
				if c := cr.Get(j); c[0] <= n && n < c[1] {
					end, ok = c[1], true
				}
			}
			if !ok {
				out = append(out, fmt.Sprintf("%s no longer reserves number %d", owner, n))
				break
			}
			n = end
		}
	}
	return append(out, reservedNameBreaks(owner, fn, cn)...)
}

// enumReservedBreaks is reservedBreaks for an enum, whose ranges hold their
// end.
func enumReservedBreaks(owner string, fr, cr protoreflect.EnumRanges, fn, cn protoreflect.Names) []string {
	var out []string
	for i := 0; i < fr.Len(); i++ {
		r := fr.Get(i) // [start, end]
		for n := r[0]; ; {
			end, ok := protoreflect.EnumNumber(0), false
			for j := 0; j < cr.Len(); j++ {
				if c := cr.Get(j); c[0] <= n && n <= c[1] {
					end, ok = c[1], true
				}
			}
			if !ok {
				out = append(out, fmt.Sprintf("%s no longer reserves number %d", owner, n))
				break
			}
			if end >= r[1] {
				break
			}
			n = end + 1
		}
	}
	return append(out, reservedNameBreaks(owner, fn, cn)...)
}

func reservedNameBreaks(owner string, fn, cn protoreflect.Names) []string {
	var out []string
	for i := 0; i < fn.Len(); i++ {
		if !cn.Has(fn.Get(i)) {
			out = append(out, fmt.Sprintf("%s no longer reserves name %s", owner, fn.Get(i)))
		}
	}
	return out
}

func loadFrozenProto(t *testing.T, path string) *descriptorpb.FileDescriptorProto {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, set); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(set.File) != 1 {
		t.Fatalf("%s holds %d files, want 1", path, len(set.File))
	}
	return set.File[0]
}

func loadFrozenSchema(t *testing.T, path string) protoreflect.FileDescriptor {
	t.Helper()
	return buildFile(t, loadFrozenProto(t, path))
}

// buildFile builds a descriptor of its own: its messages resolve to its own
// types, never to the registered ones of the same names; only its imports
// come from the registry.
func buildFile(t *testing.T, fdp *descriptorpb.FileDescriptorProto) protoreflect.FileDescriptor {
	t.Helper()
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func findMsg(f *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	var find func(ms []*descriptorpb.DescriptorProto) *descriptorpb.DescriptorProto
	find = func(ms []*descriptorpb.DescriptorProto) *descriptorpb.DescriptorProto {
		for _, m := range ms {
			if m.GetName() == name {
				return m
			}
			if n := find(m.NestedType); n != nil {
				return n
			}
		}
		return nil
	}
	if m := find(f.MessageType); m != nil {
		return m
	}
	panic("no message " + name)
}

func findEnum(f *descriptorpb.FileDescriptorProto, name string) *descriptorpb.EnumDescriptorProto {
	for _, e := range f.EnumType {
		if e.GetName() == name {
			return e
		}
	}
	panic("no enum " + name)
}

func removeMsg(t *testing.T, f *descriptorpb.FileDescriptorProto, name string) {
	t.Helper()
	for i, m := range f.MessageType {
		if m.GetName() == name {
			f.MessageType = append(f.MessageType[:i], f.MessageType[i+1:]...)
			return
		}
	}
	t.Fatalf("no message %s", name)
}
