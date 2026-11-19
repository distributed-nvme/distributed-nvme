package pb_test

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The v0.1.0 samples under testdata/v0.1.0 are what v0.1.0's own code wrote,
// made once in a git archive of the tag by throwaway tests that were never
// committed, and never refreshed:
//
//   - etcd.txt, one line per key, the key and its value in base64: every key
//     of one cluster, as v0.1.0's worker harness (worker/harness_test.go)
//     left them after planting, through v0.1.0's integtest/plant, a RAID1
//     pool with two cntlrs, two thin devices, a subsystem, a clone with two
//     bitmap chunks, a transfer, a migration with two bitmap chunks, a repair
//     and a retired leg, and after one v0.1.0 worker had driven the fake
//     agents until no syncup or push came for three seconds. plant writes
//     through v0.1.0's model, as its gateway did, but the migration bitmap
//     chunks follow v0.1.0's schema rather than what its gateway would have
//     cut. Every address is 10.1.0.1 and the worker's seed is random, so the
//     samples hold nothing of the machine that made them.
//   - store/dn, the local store v0.1.0's dn agent wrote on its fake node
//     (agent/dnagent/fakenode_test.go) when fed disk node 3's requests and
//     bitmap pushes of that run: its dn file, three sides (a migration's
//     destination, a repair's new leg and a retired leg's) and the migration's
//     two chunks; and disk, that agent's disk header block and its two
//     volume-table slots, each as far as its envelope reaches.
//   - store/cn, the requests and pushes the primary's fake cn agent accepted
//     in that run, each marshaled as an agent's store writes it and named as
//     v0.1.0's agent names its file. A v0.1.0 agent stores the request it
//     admits as it is; the dn agent's files above match its fake's byte for
//     byte.
//
// The cluster id, the disk node and the extent size are one across the
// samples, so a test may join them. The samples' tests: this file's, which
// decodes each as the message its v0.1.0 kind names and holds every field
// the current schema does not know to a reserved number; model's
// TestV010KeysAndRecords; agent/dnagent's TestDiskMetaReadsTheV010Disk and
// TestDnAgentStartsOverTheV010Store; agent/cnagent's
// TestCnAgentStartsOverTheV010Store; and csi's TestV010VolumeIdsAndContexts.

// v010EtcdKinds is the message of each etcd key kind of v0.1.0, by the
// key's second field: the kinds as that build wrote them, not as model names
// them today. A nil entry is a kind no build since reads, with its reason.
var v010EtcdKinds = map[string]func() proto.Message{
	"cluster_conf":  func() proto.Message { return &pb.ClusterConf{} },
	"dn_global":     func() proto.Message { return &pb.DnGlobal{} },
	"cn_global":     func() proto.Message { return &pb.CnGlobal{} },
	"sp_global":     func() proto.Message { return &pb.SpGlobal{} },
	"dn_rev":        func() proto.Message { return &pb.DnRev{} },
	"cn_rev":        func() proto.Message { return &pb.CnRev{} },
	"sp_rev":        func() proto.Message { return &pb.SpRev{} },
	"dn_conf":       func() proto.Message { return &pb.DnConf{} },
	"cn_conf":       func() proto.Message { return &pb.CnConf{} },
	"dn_capacity":   func() proto.Message { return &pb.DnCapacity{} },
	"cn_capacity":   func() proto.Message { return &pb.CnCapacity{} },
	"cdc":           func() proto.Message { return &pb.CdcEntry{} },
	"sp_conf":       func() proto.Message { return &pb.SpConf{} },
	"cntlr":         func() proto.Message { return &pb.Cntlr{} },
	"slice":         func() proto.Message { return &pb.Slice{} },
	"thin_device":   func() proto.Message { return &pb.ThinDevice{} },
	"subsystem":     func() proto.Message { return &pb.Subsystem{} },
	"clone":         func() proto.Message { return &pb.Clone{} },
	"clone_bitmap":  func() proto.Message { return &pb.CloneBitmap{} },
	"transfer":      func() proto.Message { return &pb.Transfer{} },
	"migration":     func() proto.Message { return &pb.Migration{} },
	"repair":        func() proto.Message { return &pb.Repair{} },
	"retired_leg":   func() proto.Message { return &pb.RetiredLeg{} },
	"sp_id_to_name": func() proto.Message { return &pb.SpName{} },
	"worker":        func() proto.Message { return &pb.WorkerReg{} },
	// MigrBitmap, retired (retiredMessages): the end of a migration deletes
	// these keys, which model's TestV010KeysAndRecords checks.
	"migration_bitmap": nil,
}

// v010StoreKinds is the message of each local-store file kind of v0.1.0, by
// its name's prefix.
var v010StoreKinds = map[string]func() proto.Message{
	"dn-":       func() proto.Message { return &pb.SyncupDnRequest{} },
	"side-":     func() proto.Message { return &pb.SyncupSideRequest{} },
	"migr-bm-":  func() proto.Message { return &pb.PushMigrBitmapRequest{} },
	"cn-":       func() proto.Message { return &pb.SyncupCnRequest{} },
	"cntlr-":    func() proto.Message { return &pb.SyncupCntlrRequest{} },
	"clone-bm-": func() proto.Message { return &pb.PushCloneBitmapRequest{} },
}

// v010Dir is the samples' directory, from this package's.
const v010Dir = "testdata/v0.1.0"

// TestV010SamplesDecode decodes every v0.1.0 sample as the message its
// v0.1.0 kind names, and holds every field of it the current schema does not
// know to a number the message reserves: Go's protobuf decodes a removed
// field, and a value of another wire type at a known number, with no error,
// into unknown fields, so a decode that does not fail proves nothing alone.
// The samples hold v0.1.0's migration bm_cnt, a field removed since, in the
// migration record and in the destination side's request: the reserved
// numbers they meet are checked too.
func TestV010SamplesDecode(t *testing.T) {
	reservedSeen := map[string]bool{}
	check := func(where string, raw []byte, msg proto.Message) {
		t.Helper()
		if err := proto.Unmarshal(raw, msg); err != nil {
			t.Errorf("%s: does not decode as %s: %v", where,
				msg.ProtoReflect().Descriptor().FullName(), err)
			return
		}
		for _, e := range unknownFields(msg.ProtoReflect()) {
			if !e.reserved {
				t.Errorf("%s: field %d of %s is unknown and not reserved: "+
					"a renumbered, retyped or removed field", where, e.number, e.msg)
				continue
			}
			reservedSeen[fmt.Sprintf("%s %d", e.msg, e.number)] = true
		}
	}

	kinds := map[string]int{}
	for _, kv := range readV010Etcd(t, filepath.Join(v010Dir, "etcd.txt")) {
		fields := strings.Split(kv.key, " ")
		if len(fields) < 2 {
			t.Fatalf("key %q has no kind", kv.key)
		}
		kind := fields[1]
		mk, ok := v010EtcdKinds[kind]
		if !ok {
			t.Errorf("key %q: kind %q is no v0.1.0 kind", kv.key, kind)
			continue
		}
		kinds[kind]++
		if mk != nil {
			check(kv.key, kv.value, mk())
		}
	}
	for kind := range v010EtcdKinds {
		if kinds[kind] == 0 {
			t.Errorf("no sample of the etcd kind %q", kind)
		}
	}

	prefixes := map[string]int{}
	for _, dir := range []string{"store/dn", "store/cn"} {
		entries, err := os.ReadDir(filepath.Join(v010Dir, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			prefix := storeKindOf(e.Name())
			if prefix == "" {
				t.Errorf("%s/%s: no v0.1.0 store kind", dir, e.Name())
				continue
			}
			prefixes[prefix]++
			raw, err := os.ReadFile(filepath.Join(v010Dir, dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			check(dir+"/"+e.Name(), raw, v010StoreKinds[prefix]())
		}
	}
	for prefix := range v010StoreKinds {
		if prefixes[prefix] == 0 {
			t.Errorf("no sample of the store kind %q", prefix)
		}
	}

	for _, want := range []string{
		"Migration 5", "SyncupSideRequest.MigrDstConf 8",
	} {
		if !reservedSeen[want] {
			t.Errorf("no sample holds the removed field %q: the reserved-number "+
				"check has nothing to bite on", want)
		}
	}
}

// storeKindOf is the v0.1.0 store kind of a file name, "" for none; the
// longest prefix wins.
func storeKindOf(name string) string {
	best := ""
	for prefix := range v010StoreKinds {
		if strings.HasPrefix(name, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}

type v010KV struct {
	key   string
	value []byte
}

// readV010Etcd reads an etcd.txt of key, tab, base64 value lines.
func readV010Etcd(t *testing.T, path string) []v010KV {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []v010KV
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, enc, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			t.Fatalf("%s: line %q is not key, tab, value", path, sc.Text())
		}
		value, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("%s: key %q: %v", path, key, err)
		}
		out = append(out, v010KV{key: key, value: value})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// unknownField is one field a decoded message did not know.
type unknownField struct {
	msg      protoreflect.FullName
	number   protowire.Number
	reserved bool
}

// unknownFields lists the unknown fields of m and of every message it holds.
func unknownFields(m protoreflect.Message) []unknownField {
	md := m.Descriptor()
	var out []unknownField
	b := m.GetUnknown()
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return append(out, unknownField{msg: md.FullName()})
		}
		b = b[n:]
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return append(out, unknownField{msg: md.FullName(), number: num})
		}
		b = b[n:]
		out = append(out, unknownField{
			msg: md.FullName(), number: num, reserved: md.ReservedRanges().Has(num),
		})
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					out = append(out, unknownFields(mv.Message())...)
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				for i := 0; i < v.List().Len(); i++ {
					out = append(out, unknownFields(v.List().Get(i).Message())...)
				}
			}
		case fd.Message() != nil:
			out = append(out, unknownFields(v.Message())...)
		}
		return true
	})
	return out
}
