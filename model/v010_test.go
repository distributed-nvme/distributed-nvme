package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// v010Etcd is the etcd sample of v0.1.0; pb/samples_test.go says how it was
// made.
const v010Etcd = "../pb/testdata/v0.1.0/etcd.txt"

// TestV010KeysAndRecords holds today's model to the keys and records v0.1.0
// wrote (architecture.md, Upgrade in lockstep): every key is rebuilt byte for
// byte by today's key builder from its own fields; the cluster id is the
// derivation of the stored name and epoch; and over an etcd that holds the
// sample, the stored cluster conf passes its validation, LoadSp reads the
// pool whole, and the end of the migration, a finish of the gateway's or the
// worker's, deletes the bitmap keys v0.1.0 kept for it.
func TestV010KeysAndRecords(t *testing.T) {
	kvs := readV010Etcd(t)
	hexOf := func(key, s string) uint64 {
		t.Helper()
		v, err := strconv.ParseUint(s, 16, 64)
		if err != nil {
			t.Fatalf("key %q: field %q is not hex: %v", key, s, err)
		}
		return v
	}
	// By v0.1.0's kind strings, not model's names for them: a kind that
	// model renames leaves its v0.1.0 keys behind.
	builders := map[string]func(key string, f []string) string{
		"cluster_conf": func(_ string, f []string) string { return ClusterConfKey(f[0]) },
		"dn_global":    func(k string, f []string) string { return DnGlobalKey(hexOf(k, f[0])) },
		"cn_global":    func(k string, f []string) string { return CnGlobalKey(hexOf(k, f[0])) },
		"sp_global":    func(k string, f []string) string { return SpGlobalKey(hexOf(k, f[0])) },
		"dn_rev": func(k string, f []string) string {
			return DnRevKey(uint32(hexOf(k, f[0])), hexOf(k, f[1]), hexOf(k, f[2]))
		},
		"cn_rev": func(k string, f []string) string {
			return CnRevKey(uint32(hexOf(k, f[0])), hexOf(k, f[1]), hexOf(k, f[2]))
		},
		"sp_rev": func(k string, f []string) string {
			return SpRevKey(uint32(hexOf(k, f[0])), hexOf(k, f[1]), hexOf(k, f[2]))
		},
		"dn_conf": func(k string, f []string) string { return DnConfKey(hexOf(k, f[0]), f[1]) },
		"cn_conf": func(k string, f []string) string { return CnConfKey(hexOf(k, f[0]), f[1]) },
		"dn_capacity": func(k string, f []string) string {
			return DnCapacityKey(hexOf(k, f[0]), uint32(hexOf(k, f[1])), hexOf(k, f[2]), f[3])
		},
		"cn_capacity": func(k string, f []string) string {
			return CnCapacityKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2])
		},
		"cdc": func(k string, f []string) string {
			return CdcEntryKey(hexOf(k, f[0]), uint32(hexOf(k, f[1])), hexOf(k, f[2]), hexOf(k, f[3]))
		},
		"sp_conf":       func(k string, f []string) string { return SpConfKey(hexOf(k, f[0]), f[1]) },
		"sp_id_to_name": func(k string, f []string) string { return SpNameKey(hexOf(k, f[0]), hexOf(k, f[1])) },
		"cntlr": func(k string, f []string) string {
			return CntlrKey(hexOf(k, f[0]), hexOf(k, f[1]), hexOf(k, f[2]))
		},
		"slice": func(k string, f []string) string {
			return SliceKey(hexOf(k, f[0]), hexOf(k, f[1]), hexOf(k, f[2]))
		},
		"thin_device": func(k string, f []string) string {
			return ThinDeviceKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2])
		},
		"subsystem": func(k string, f []string) string {
			return SubsystemKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2])
		},
		"clone": func(k string, f []string) string { return CloneKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2]) },
		"clone_bitmap": func(k string, f []string) string {
			return CloneBitmapKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2],
				uint32(hexOf(k, f[3])), uint32(hexOf(k, f[4])))
		},
		"transfer": func(k string, f []string) string {
			return TransferKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2])
		},
		"migration": func(k string, f []string) string {
			return MigrationKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2])
		},
		"migration_bitmap": func(k string, f []string) string {
			return LegacyMigrBitmapKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2], uint32(hexOf(k, f[3])))
		},
		"repair": func(k string, f []string) string { return RepairKey(hexOf(k, f[0]), hexOf(k, f[1]), f[2]) },
		"retired_leg": func(k string, f []string) string {
			return RetiredLegKey(hexOf(k, f[0]), hexOf(k, f[1]), hexOf(k, f[2]))
		},
		"worker": func(_ string, f []string) string { return WorkerRegKey(f[0], f[1]) },
	}
	for _, kv := range kvs {
		f := strings.Split(kv.key, keySep)
		if len(f) < 3 || f[0] != common.DnvPrefix {
			t.Errorf("key %q is not a dnv key of a kind and its fields", kv.key)
			continue
		}
		build, ok := builders[f[1]]
		if !ok {
			t.Errorf("key %q: kind %q has no key builder", kv.key, f[1])
			continue
		}
		if got := build(kv.key, f[2:]); got != kv.key {
			t.Errorf("v0.1.0 key %q is rebuilt as %q", kv.key, got)
		}
	}
	if t.Failed() {
		return
	}

	// The cluster, the pool and the migration of the sample.
	var (
		cc       pb.ClusterConf
		ccName   string
		cid      uint64
		spName   string
		spShard  uint32
		migrName string
		migr     pb.Migration
		bmKeys   []string
	)
	for _, kv := range kvs {
		f := strings.Split(kv.key, keySep)
		switch f[1] {
		case "cluster_conf":
			ccName = f[2]
			if err := proto.Unmarshal(kv.value, &cc); err != nil {
				t.Fatal(err)
			}
		case "sp_conf":
			cid, spName = hexOfOrFail(t, f[2]), f[3]
		case "sp_rev":
			spShard = uint32(hexOfOrFail(t, f[2]))
		case "migration":
			migrName = f[4]
			if err := proto.Unmarshal(kv.value, &migr); err != nil {
				t.Fatal(err)
			}
		case "migration_bitmap":
			bmKeys = append(bmKeys, kv.key)
		}
	}
	if got := ClusterId(ccName, cc.GetCreationEpoch()); got != cid {
		t.Errorf("ClusterId(%q, %d) = %016x, want the stored %016x",
			ccName, cc.GetCreationEpoch(), got, cid)
	}
	if err := ValidateClusterConf(&cc); err != nil {
		t.Errorf("the v0.1.0 cluster conf fails today's validation: %v", err)
	}
	if len(bmKeys) == 0 || migrName == "" {
		t.Fatalf("the sample lacks a migration with bitmap keys "+
			"(%q, %d keys)", migrName, len(bmKeys))
	}

	ctx := context.Background()
	cli := newPrivateTestClient(t)
	for _, kv := range kvs {
		// A message with no fields keeps every byte as an unknown field and
		// writes it back as it read it: the put stores v0.1.0's value as is.
		raw := &emptypb.Empty{}
		if err := proto.Unmarshal(kv.value, raw); err != nil {
			t.Fatal(err)
		}
		if b, err := proto.Marshal(raw); err != nil || !bytes.Equal(b, kv.value) {
			t.Fatalf("key %q: the value does not pass through as is (%v)", kv.key, err)
		}
		mustPut(t, cli, kv.key, raw)
	}

	state, err := LoadSp(ctx, cli, cid, spName)
	if err != nil {
		t.Fatalf("LoadSp of the v0.1.0 pool: %v", err)
	}
	if len(state.Missing) != 0 {
		t.Errorf("LoadSp misses %q", state.Missing)
	}
	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"cntlrs", len(state.Cntlrs), 2},
		{"slices", len(state.Slices), 1},
		{"thin devices", len(state.Tds), 2},
		{"subsystems", len(state.Subsystems), 1},
		{"clones", len(state.Clones), 1},
		{"transfers", len(state.Xfers), 1},
		{"migrations", len(state.Migrs), 1},
		{"repairs", len(state.Repairs), 1},
		{"retired legs", len(state.RetiredLegs), 1},
		{"disk nodes", len(state.DnByAddr), 3},
		{"controller nodes", len(state.CnByAddr), 2},
	} {
		if c.got != c.want {
			t.Errorf("LoadSp read %d %s, want %d", c.got, c.what, c.want)
		}
	}
	for name, chunks := range state.CloneBmIdx {
		if len(chunks) != 2 {
			t.Errorf("LoadSp read %d bitmap chunks of clone %s, want 2", len(chunks), name)
		}
	}

	// The v0.1.0 worker started the cutover, so the migration ends by a
	// finish; a pending one would end by a cancel.
	end, endName := FinishMigration, "FinishMigration"
	if migr.GetCutoverPending() {
		end, endName = CancelMigration, "CancelMigration"
	}
	if err := end(ctx, cli, cid, spShard, state.Conf.GetSpId(),
		spName, migrName, migr.GetMigrId(), &cc); err != nil {
		t.Fatalf("%s of the v0.1.0 migration: %v", endName, err)
	}
	for _, key := range append(bmKeys, MigrationKey(cid, state.Conf.GetSpId(), migrName)) {
		found, err := cli.Get(ctx, key, &emptypb.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Errorf("key %q outlives the end of its migration", key)
		}
	}
}

func hexOfOrFail(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type v010KV struct {
	key   string
	value []byte
}

func readV010Etcd(t *testing.T) []v010KV {
	t.Helper()
	f, err := os.Open(v010Etcd)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []v010KV
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, enc, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			t.Fatalf("line %q is not key, tab, value", sc.Text())
		}
		value, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		out = append(out, v010KV{key: key, value: value})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
