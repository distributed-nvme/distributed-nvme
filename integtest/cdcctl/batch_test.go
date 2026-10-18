// Tests for `cdcctl batch` (cdc.md, Integration test plan, The driver,
// `cdcctl`): a batch is read and checked as a whole before etcd is dialled,
// and it is applied as ONE transaction, which is what keeps the hosts a
// batch moves at an AEN or two per batch instead of one per entry. The
// transaction is played by a fake that commits the staged writes only when
// the callback returns nil, as etcd's commit does.
package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// fakeStore is a committed key space and the runner of its transactions.
type fakeStore struct {
	committed map[string]*pb.CdcEntry
	// runs counts RunSTM calls: one per transaction a batch takes.
	runs int
}

func newFakeStore() *fakeStore {
	return &fakeStore{committed: map[string]*pb.CdcEntry{}}
}

// RunSTM runs f once over a view of the committed keys and commits what f
// staged only when f returns nil.
func (s *fakeStore) RunSTM(
	_ context.Context, f func(etcdutil.STM) error,
) error {
	s.runs++
	view := &fakeStm{store: s, puts: map[string]*pb.CdcEntry{},
		dels: map[string]bool{}}
	if err := f(view); err != nil {
		return err
	}
	for key := range view.dels {
		delete(s.committed, key)
	}
	for key, entry := range view.puts {
		s.committed[key] = entry
	}
	return nil
}

// fakeStm is one transaction's staged writes over the committed keys.
type fakeStm struct {
	store *fakeStore
	puts  map[string]*pb.CdcEntry
	dels  map[string]bool
}

func (v *fakeStm) Get(key string, msg proto.Message) bool {
	if v.dels[key] {
		return false
	}
	entry, ok := v.puts[key]
	if !ok {
		entry, ok = v.store.committed[key]
	}
	if !ok {
		return false
	}
	proto.Merge(msg, entry)
	return true
}

func (v *fakeStm) Put(key string, msg proto.Message) {
	delete(v.dels, key)
	v.puts[key] = proto.Clone(msg).(*pb.CdcEntry)
}

func (v *fakeStm) Del(key string) {
	delete(v.puts, key)
	v.dels[key] = true
}

func (v *fakeStm) Rev(key string) int64 {
	if _, ok := v.store.committed[key]; ok {
		return 1
	}
	return 0
}

const batchCid = 0xcdc7

func mustParseBatch(t *testing.T, text string) []batchOp {
	t.Helper()
	ops, err := parseBatch(strings.NewReader(text), batchCid)
	if err != nil {
		t.Fatalf("parseBatch failed: %v\nbatch:\n%s", err, text)
	}
	return ops
}

// TestParseBatchReadsEveryLine pins the line grammar: the single
// subcommands' flags, every key under the invocation's cluster, the
// transport list in its order (DS5's tr-conf index), blank and comment lines
// skipped.
func TestParseBatchReadsEveryLine(t *testing.T) {
	ops := mustParseBatch(t, `
# the base entries
put --shard 00 --sp 0x1 --ss 0xa --nqn nqn.x:a --tr tcp,ipv4,10.0.0.2,14421 --tr tcp,ipv4,10.0.0.2,14420 --allowed nqn.h1 --allowed nqn.h2

  put --shard 81 --sp 2 --ss 0xb --nqn nqn.x:b --tr tcp,ipv4,10.0.0.2,14422
del --shard ff --sp 0x3 --ss 0xc
`)
	if len(ops) != 3 {
		t.Fatalf("parsed %d operations, want 3", len(ops))
	}
	want := []struct {
		key     string
		line    int
		entry   *pb.CdcEntry
		deletes bool
	}{
		{
			key:  model.CdcEntryKey(batchCid, 0x00, 0x1, 0xa),
			line: 3,
			entry: &pb.CdcEntry{
				Nqn: "nqn.x:a",
				NvmeTrConfList: []*pb.NvmeTrConf{
					{TrType: "tcp", AdrFam: "ipv4", TrAddr: "10.0.0.2",
						TrSvcId: "14421"},
					{TrType: "tcp", AdrFam: "ipv4", TrAddr: "10.0.0.2",
						TrSvcId: "14420"},
				},
				AllowedHosts: []string{"nqn.h1", "nqn.h2"},
			},
		},
		{
			// --shard is hex, as for a single put: 81 is the high half.
			key:  model.CdcEntryKey(batchCid, 0x81, 0x2, 0xb),
			line: 5,
			entry: &pb.CdcEntry{
				Nqn: "nqn.x:b",
				NvmeTrConfList: []*pb.NvmeTrConf{
					{TrType: "tcp", AdrFam: "ipv4", TrAddr: "10.0.0.2",
						TrSvcId: "14422"},
				},
			},
		},
		{
			key:     model.CdcEntryKey(batchCid, 0xff, 0x3, 0xc),
			line:    6,
			deletes: true,
		},
	}
	for i, w := range want {
		op := ops[i]
		if op.key != w.key {
			t.Errorf("op %d key = %q, want %q", i, op.key, w.key)
		}
		if op.line != w.line {
			t.Errorf("op %d line = %d, want %d", i, op.line, w.line)
		}
		if w.deletes {
			if op.entry != nil {
				t.Errorf("op %d is a put of %v, want a delete", i, op.entry)
			}
			continue
		}
		if !proto.Equal(op.entry, w.entry) {
			t.Errorf("op %d entry = %v, want %v", i, op.entry, w.entry)
		}
	}
}

// TestParseBatchRefusesTheWholeBatch pins that one bad line refuses the
// batch, with the same words a single `put` or `del` uses where there is
// one, and names the line.
func TestParseBatchRefusesTheWholeBatch(t *testing.T) {
	good := "put --shard 00 --sp 1 --ss 1 --nqn nqn.x:a " +
		"--tr tcp,ipv4,10.0.0.2,14420\n"
	cases := []struct {
		name  string
		batch string
		want  string
	}{
		{"empty", "", "the batch names no entry"},
		{"comments only", "# nothing\n\n", "the batch names no entry"},
		{"unknown verb", good + "get --shard 00 --sp 1 --ss 2\n",
			`line 2: "get" is neither put nor del`},
		{"no shard", good + "del --sp 1 --ss 2\n",
			"line 2: del: --shard is required"},
		{"no sp", "del --shard 00 --ss 2\n",
			"line 1: del: --sp is required"},
		{"no ss", "del --shard 00 --sp 2\n",
			"line 1: del: --ss is required"},
		{"no nqn", "put --shard 00 --sp 1 --ss 1 " +
			"--tr tcp,ipv4,10.0.0.2,14420\n",
			"line 1: put: --nqn is required"},
		{"no tr", "put --shard 00 --sp 1 --ss 1 --nqn nqn.x:a\n",
			"line 1: put: at least one --tr is required"},
		{"bad shard", "del --shard 100 --sp 1 --ss 1\n",
			"line 1: del: invalid value"},
		{"bad tr", "put --shard 00 --sp 1 --ss 1 --nqn nqn.x:a " +
			"--tr tcp,ipv4,10.0.0.2\n", "line 1: put: invalid value"},
		{"put flag on del", "del --shard 00 --sp 1 --ss 1 --nqn nqn.x:a\n",
			"line 1: del: flag provided but not defined: -nqn"},
		{"global flag on a line", good + "del --shard 00 --sp 1 --ss 2 " +
			"--cluster 0x1\n",
			"line 2: del: flag provided but not defined: -cluster"},
		{"stray argument", "del --shard 00 --sp 1 --ss 1 extra\n",
			`line 1: del: unexpected argument "extra"`},
		{"key named twice", good + "\n" +
			"del --shard 0x00 --sp 0x1 --ss 1\n",
			"line 3: key \"" + model.CdcEntryKey(batchCid, 0, 1, 1) +
				"\" is already named on line 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, err := parseBatch(strings.NewReader(tc.batch), batchCid)
			if err == nil {
				t.Fatalf("parseBatch succeeded with %d operations, want "+
					"an error saying %q", len(ops), tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseBatch error = %q, want it to say %q",
					err.Error(), tc.want)
			}
		})
	}
}

// TestRunBatchIsOneTransaction is the batch's reason to exist: every put and
// delete of a batch lands in ONE transaction, so dnv-cdc applies them as
// the events of one revision.
func TestRunBatchIsOneTransaction(t *testing.T) {
	store := newFakeStore()
	var text strings.Builder
	const n = 200
	for i := 1; i <= n; i++ {
		text.WriteString("put --shard 00 --sp 1 --ss " + strconv.Itoa(i) +
			" --nqn nqn.x:e" + strconv.Itoa(i) +
			" --tr tcp,ipv4,10.0.0.2,14420 --allowed nqn.h1\n")
	}
	puts, dels, err := runBatch(context.Background(), store,
		mustParseBatch(t, text.String()))
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
	if puts != n || dels != 0 {
		t.Fatalf("runBatch reported %d puts and %d deletes, want %d and 0",
			puts, dels, n)
	}
	if store.runs != 1 {
		t.Fatalf("the batch took %d transactions, want 1", store.runs)
	}
	if len(store.committed) != n {
		t.Fatalf("%d keys committed, want %d", len(store.committed), n)
	}
	got := store.committed[model.CdcEntryKey(batchCid, 0, 1, 7)]
	if got.GetNqn() != "nqn.x:e7" {
		t.Fatalf("entry 7 = %v, want nqn nqn.x:e7", got)
	}

	// A second batch of deletes and rewrites is one transaction too.
	var second strings.Builder
	for i := 1; i <= n; i += 2 {
		second.WriteString("del --shard 00 --sp 1 --ss " + strconv.Itoa(i) +
			"\n")
	}
	second.WriteString("put --shard 00 --sp 1 --ss 2 --nqn nqn.x:moved " +
		"--tr tcp,ipv4,10.0.0.2,14421 --allowed nqn.h2\n")
	puts, dels, err = runBatch(context.Background(), store,
		mustParseBatch(t, second.String()))
	if err != nil {
		t.Fatalf("runBatch of the second batch: %v", err)
	}
	if puts != 1 || dels != n/2 {
		t.Fatalf("second batch reported %d puts and %d deletes, want 1 "+
			"and %d", puts, dels, n/2)
	}
	if store.runs != 2 {
		t.Fatalf("two batches took %d transactions, want 2", store.runs)
	}
	if len(store.committed) != n/2 {
		t.Fatalf("%d keys left, want %d", len(store.committed), n/2)
	}
	moved := store.committed[model.CdcEntryKey(batchCid, 0, 1, 2)]
	if moved.GetNqn() != "nqn.x:moved" ||
		strings.Join(moved.GetAllowedHosts(), ",") != "nqn.h2" {
		t.Fatalf("entry 2 = %v, want the rewritten entry", moved)
	}
}

// TestRunBatchDeleteOfAMissingKeyWritesNothing pins cmdDel's refusal inside a
// batch: a `del` of a key that is not there is a mistyped id, and it aborts
// the transaction with etcdutil.ErrNoCommit, so the puts before it are not
// written either.
func TestRunBatchDeleteOfAMissingKeyWritesNothing(t *testing.T) {
	store := newFakeStore()
	present := model.CdcEntryKey(batchCid, 0, 1, 1)
	store.committed[present] = &pb.CdcEntry{Nqn: "nqn.x:present"}
	ops := mustParseBatch(t,
		"put --shard 00 --sp 1 --ss 2 --nqn nqn.x:new "+
			"--tr tcp,ipv4,10.0.0.2,14420\n"+
			"del --shard 00 --sp 1 --ss 1\n"+
			"del --shard 00 --sp 1 --ss 3\n")
	puts, dels, err := runBatch(context.Background(), store, ops)
	if err == nil {
		t.Fatalf("runBatch succeeded (%d puts, %d deletes), want the "+
			"missing key refused", puts, dels)
	}
	if !errors.Is(err, etcdutil.ErrNoCommit) {
		t.Fatalf("runBatch error %v does not wrap etcdutil.ErrNoCommit", err)
	}
	missing := model.CdcEntryKey(batchCid, 0, 1, 3)
	if !strings.Contains(err.Error(), "line 3") ||
		!strings.Contains(err.Error(), missing) {
		t.Fatalf("runBatch error %q does not name line 3 and key %q",
			err.Error(), missing)
	}
	if puts != 0 || dels != 0 {
		t.Fatalf("a refused batch reported %d puts and %d deletes, want 0",
			puts, dels)
	}
	if len(store.committed) != 1 || store.committed[present] == nil {
		t.Fatalf("a refused batch changed the store: %v", store.committed)
	}
}
