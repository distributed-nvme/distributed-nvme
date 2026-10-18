package cdc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The fixtures of the cases that run a whole instance over the package's
// real etcd (etcdenv_test.go, dnv-worker.md EU7) instead of the in-memory
// fakeStore: cdc entries written into etcd the way the worker writes them,
// and an instance whose watcher scans and watches that etcd.
//
// The cdc watches every cluster at once (DS1), so each test writes under a
// cluster id of its own, connects as host NQNs derived from it, and deletes
// what it wrote when it ends: an entry left behind would be scanned by every
// later instance of the package's run. The one exception is the one byte
// host NQN of TestHostNqnAtItsLimits, too short to carry a cluster id.

// plantCidSeq numbers the key spaces newPlantEnv hands out, so the second
// iteration of a test under `go test -count=N` gets a key space of its own.
var plantCidSeq atomic.Uint64

// plantEnv is one test's key space in etcd: its cluster id and the client
// it writes through.
type plantEnv struct {
	t   *testing.T
	cli *etcdutil.Client
	cid uint64
}

// newPlantEnv gives the test a cluster id of its own on cli and deletes
// every cdc entry of that cluster when the test ends, the ones written after
// this call and those of a failed test included. Called after the client is
// made and before the instance starts, its cleanup runs after the instance
// has stopped and before the client closes.
func newPlantEnv(t *testing.T, cli *etcdutil.Client) *plantEnv {
	t.Helper()
	e := &plantEnv{
		t:   t,
		cli: cli,
		cid: model.ClusterId(t.Name(), plantCidSeq.Add(1)),
	}
	t.Cleanup(e.wipe)
	return e
}

// plantedEntry is one cdc entry a test writes: the key fields under the
// test's cluster id, and the value.
type plantedEntry struct {
	shard uint32
	spId  uint64
	ssId  uint64
	msg   *pb.CdcEntry
}

// key is the entry's etcd key in the test's cluster (MD2).
func (e *plantEnv) key(p plantedEntry) string {
	return model.CdcEntryKey(e.cid, p.shard, p.spId, p.ssId)
}

// hostNqn is the test's host NQN number i, unique to its cluster id.
func (e *plantEnv) hostNqn(i int) string {
	return fmt.Sprintf("nqn.2024-01.io.dnv.test:%016x:host%d", e.cid, i)
}

// subNqn is the subsystem NQN of one subsystem of one pool of the test.
func (e *plantEnv) subNqn(spId uint64, ssId uint64) string {
	return fmt.Sprintf("nqn.2024-01.io.dnv.test:%016x:sp%d:ss%d",
		e.cid, spId, ssId)
}

// plantCntlrs is a pool's cnt cntlrs as the worker reads them when it
// rewrites the pool's entries (architecture.md [D18]): cntlr 0 the primary
// and the others healthy standbys, each on a controller node port of its
// own.
func plantCntlrs(spId uint64, cnt int) []*pb.Cntlr {
	cntlrs := make([]*pb.Cntlr, 0, cnt)
	for i := 0; i < cnt; i++ {
		cntlrs = append(cntlrs, &pb.Cntlr{
			NvmeTrConf: tcpConf(
				fmt.Sprintf("10.%d.%d.%d", spId>>8&0xff, spId&0xff, i+1),
				fmt.Sprintf("%d", 4420+i),
			),
			Primary: i == 0,
		})
	}
	return cntlrs
}

// poolEntry is the entry of one subsystem of a pool whose cntlrs are
// cntlrs, as the worker writes it: the transport list is the listing rule's
// over those cntlrs (model.CdcTrConfList), and allowed are the subsystem's
// allowed hosts.
func (e *plantEnv) poolEntry(
	shard uint32,
	spId uint64,
	ssId uint64,
	cntlrs []*pb.Cntlr,
	allowed []string,
) plantedEntry {
	return plantedEntry{
		shard: shard,
		spId:  spId,
		ssId:  ssId,
		msg: &pb.CdcEntry{
			Nqn:            e.subNqn(spId, ssId),
			NvmeTrConfList: model.CdcTrConfList(cntlrs),
			AllowedHosts:   allowed,
		},
	}
}

// put writes the entries, at most common.EtcdMaxTxnOps of them in each
// transaction, which is how the worker's and the gateway's transactions put
// a pool's entries.
func (e *plantEnv) put(entries ...plantedEntry) {
	e.t.Helper()
	for start := 0; start < len(entries); start += common.EtcdMaxTxnOps {
		batch := entries[start:min(start+common.EtcdMaxTxnOps, len(entries))]
		err := e.cli.RunSTM(context.Background(), func(s etcdutil.STM) error {
			for _, p := range batch {
				s.Put(e.key(p), p.msg)
			}
			return nil
		})
		if err != nil {
			e.t.Fatalf("put %d cdc entries: %v", len(batch), err)
		}
	}
}

// wipe deletes every cdc entry of the test's cluster.
func (e *plantEnv) wipe() {
	ctx := context.Background()
	keys, _, err := e.cli.RangeKeys(ctx, model.CdcEntryPrefix())
	if err != nil {
		e.t.Errorf("list the cdc entries to delete: %v", err)
		return
	}
	var mine []string
	for _, k := range keys {
		cid, _, _, _, ok := model.ParseCdcEntryKey(k.Key)
		if ok && cid == e.cid {
			mine = append(mine, k.Key)
		}
	}
	for start := 0; start < len(mine); start += common.EtcdMaxTxnOps {
		batch := mine[start:min(start+common.EtcdMaxTxnOps, len(mine))]
		err := e.cli.RunSTM(ctx, func(s etcdutil.STM) error {
			for _, key := range batch {
				s.Del(key)
			}
			return nil
		})
		if err != nil {
			e.t.Errorf("delete %d cdc entries: %v", len(batch), err)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// An instance over the real etcd (CM4)
// ---------------------------------------------------------------------------

// startPlanted starts a whole instance the way Run composes one (CM4) —
// the listener, the watcher, then the accept loop once the first scan has
// landed — with cli as its store and owning the given range digits (DS2).
// Its timers run on a fake clock that no case advances, so no pre-Connect
// deadline or keep-alive ends a connection while the machine is loaded
// (NP10). Its listener counts the Accept calls into ts.accepts, and wrap,
// when not nil, wraps it first. The cleanup waits a bounded time, so an
// instance that never stops fails the test instead of hanging the package.
func startPlanted(
	t *testing.T,
	cli *etcdutil.Client,
	ranges []uint32,
	wrap func(net.Listener) net.Listener,
) *testServer {
	t.Helper()
	clk := newFakeClock()
	d := &deps{
		cfg: Config{
			Ranges:  ranges,
			TrType:  common.DefaultCdcTrType,
			AdrFam:  common.DefaultCdcAdrFam,
			TrAddr:  "127.0.0.1",
			TrSvcId: "0",
		},
		store: cli,
		clk:   clk,
	}
	reg := newRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := newServer(ctx, d, reg)
	if err != nil {
		cancel()
		t.Fatalf("newServer: %v", err)
	}
	if wrap != nil {
		srv.ln = wrap(srv.ln)
	}
	cl := &countingListener{Listener: srv.ln}
	srv.ln = cl
	ts := &testServer{
		t:       t,
		srv:     srv,
		reg:     reg,
		clk:     clk,
		addr:    srv.addr().String(),
		cancel:  cancel,
		done:    make(chan struct{}),
		accepts: &cl.accepts,
	}
	go func() {
		defer close(ts.done)
		runInstance(ctx, srv)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ts.done:
		case <-time.After(10 * time.Second):
			t.Error("the instance did not stop")
		}
	})
	return ts
}

// allRanges is every range digit, the --range default (CM1).
func allRanges() []uint32 {
	ranges := make([]uint32, 0, 16)
	for digit := uint32(0); digit < 16; digit++ {
		ranges = append(ranges, digit)
	}
	return ranges
}

// waitScanned waits until the instance's registry holds at least n entries.
// A scan installs its whole map at once (WV1), so once the first scan has
// counted the entries written before the instance started, it holds all of
// them.
func waitScanned(t *testing.T, ts *testServer, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for ts.reg.entryCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the instance holds %d entries after %v, want %d",
				ts.reg.entryCount(), within, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// What a host must read (DS3, DS5, DS9)
// ---------------------------------------------------------------------------

// sortPlanted puts entries in the log order of DS5. Every entry of a test is
// in its one cluster, so the order is shard code, sp id, ss id.
func sortPlanted(entries []plantedEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.shard != b.shard {
			return a.shard < b.shard
		}
		if a.spId != b.spId {
			return a.spId < b.spId
		}
		return a.ssId < b.ssId
	})
}

// wantLog yields, one record at a time and in log order (DS5), the records
// a host's log must hold: those of the entries that name the host (DS4) and
// whose shard code the instance owns (DS2), each entry's in its transport
// order.
type wantLog struct {
	entries []plantedEntry
	ei      int
	ci      int
}

// newWantLog is the log hostNqn must read from an instance that owns the
// shard codes owns accepts, out of entries.
func newWantLog(
	entries []plantedEntry,
	hostNqn string,
	owns func(shard uint32) bool,
) *wantLog {
	var seen []plantedEntry
	for _, p := range entries {
		if !owns(p.shard) {
			continue
		}
		for _, h := range p.msg.GetAllowedHosts() {
			if h == hostNqn {
				seen = append(seen, p)
				break
			}
		}
	}
	sortPlanted(seen)
	return &wantLog{entries: seen}
}

// numRec is how many records the log holds.
func (w *wantLog) numRec() uint64 {
	var n uint64
	for _, p := range w.entries {
		n += uint64(len(p.msg.GetNvmeTrConfList()))
	}
	return n
}

// next is the next record, or nil past the last one.
func (w *wantLog) next(t *testing.T) []byte {
	t.Helper()
	for w.ei < len(w.entries) {
		p := w.entries[w.ei]
		confs := p.msg.GetNvmeTrConfList()
		if w.ci >= len(confs) {
			w.ei++
			w.ci = 0
			continue
		}
		record, skip := renderEntry(p.msg.GetNqn(), confs[w.ci])
		if skip != "" {
			t.Fatalf("a planted transport renders with skip %q", skip)
		}
		w.ci++
		return record
	}
	return nil
}

// all is every remaining record, laid end to end.
func (w *wantLog) all(t *testing.T) []byte {
	t.Helper()
	var body []byte
	for record := w.next(t); record != nil; record = w.next(t) {
		body = append(body, record...)
	}
	return body
}

// logAt is one Get Log Page of length bytes at offset off. The payload is
// dropped from the host's buffers, so a host that reads a long log keeps no
// chunk it has already been handed.
func logAt(h *fakeHost, length uint32, off uint64) (completion, []byte) {
	h.t.Helper()
	c, data := connGetLog(h, lidDiscovery, false, length, off)
	delete(h.data, c.cid)
	return c, data
}

// logHeader reads the log's header (DS9) and returns its GENCTR and NUMREC.
func logHeader(t *testing.T, h *fakeHost) (uint64, uint64) {
	t.Helper()
	c, data := logAt(h, common.CdcDiscLogHeaderSize, 0)
	connStatus(t, c, statusSuccess, "get log page header")
	if len(data) != common.CdcDiscLogHeaderSize {
		t.Fatalf("header read moved %d bytes, want %d", len(data),
			common.CdcDiscLogHeaderSize)
	}
	return binary.LittleEndian.Uint64(data[0:8]),
		binary.LittleEndian.Uint64(data[8:16])
}

// checkLog reads the whole log a host is served, in commands of at most
// maxLogTransfer bytes, the most one command moves (NP8), and fails the test
// unless its GENCTR is genCtr and its records are exactly want's. Each
// chunk is compared as it arrives, so the test never holds the whole log.
func checkLog(t *testing.T, h *fakeHost, genCtr uint64, want *wantLog) {
	t.Helper()
	numRec := want.numRec()
	gotGen, gotNum := logHeader(t, h)
	if gotGen != genCtr || gotNum != numRec {
		t.Fatalf("log header: genctr %d numrec %d, want genctr %d numrec %d",
			gotGen, gotNum, genCtr, numRec)
	}
	total := uint64(common.CdcDiscLogHeaderSize) +
		numRec*common.CdcDiscLogEntrySize
	var pending []byte
	idx := uint64(0)
	for off := uint64(0); off < total; off += maxLogTransfer {
		length := min(total-off, maxLogTransfer)
		c, data := logAt(h, uint32(length), off)
		connStatus(t, c, statusSuccess,
			fmt.Sprintf("get log page at lpo %d", off))
		if uint64(len(data)) != length {
			t.Fatalf("the read at lpo %d moved %d bytes, want %d", off,
				len(data), length)
		}
		if off == 0 {
			if g := binary.LittleEndian.Uint64(data[0:8]); g != genCtr {
				t.Fatalf("genctr %d in the first chunk, want %d", g, genCtr)
			}
			data = data[common.CdcDiscLogHeaderSize:]
		}
		pending = append(pending, data...)
		for len(pending) >= common.CdcDiscLogEntrySize {
			record := want.next(t)
			if record == nil {
				t.Fatalf("record %d is past the %d the log must hold", idx,
					numRec)
			}
			if !bytes.Equal(pending[:common.CdcDiscLogEntrySize], record) {
				t.Fatalf("record %d of %d is not the one DS5 orders there: "+
					"subnqn %q, traddr %q", idx, numRec,
					bytes.TrimRight(pending[256:512], "\x00"),
					bytes.TrimRight(pending[512:768], "\x00"))
			}
			pending = pending[common.CdcDiscLogEntrySize:]
			idx++
		}
	}
	if idx != numRec || len(pending) != 0 {
		t.Fatalf("read %d records and %d stray bytes, want %d records", idx,
			len(pending), numRec)
	}
}

// ---------------------------------------------------------------------------
// AENs (DS8, NP11)
// ---------------------------------------------------------------------------

// armConfirmed arms one AER and returns its command id once the controller
// has taken it: a Keep Alive sent after it completes only after the AER has
// been handled, because one connection's commands run in order.
func armConfirmed(t *testing.T, h *fakeHost) uint16 {
	t.Helper()
	aer := h.armAer()
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive after the aer")
	return aer
}

// awaitAen waits for the AEN completing the AER aer and checks its value.
func awaitAen(t *testing.T, h *fakeHost, aer uint16, what string) {
	t.Helper()
	c := h.await(aer)
	connStatus(t, c, statusSuccess, "aen "+what)
	if c.dw0 != aenDiscLogChanged {
		t.Fatalf("aen %s: dword0 %#08x, want %#08x", what, c.dw0,
			aenDiscLogChanged)
	}
}

// awaitAenBy is awaitAen with the AEN due by the time by. No other command
// of h may be outstanding, so the first completion h receives is the AEN's.
func awaitAenBy(
	t *testing.T,
	h *fakeHost,
	aer uint16,
	by time.Time,
	what string,
) {
	t.Helper()
	c, ok := h.pollPending(time.Until(by))
	if !ok {
		t.Fatalf("aen %s: none arrived by its due time", what)
	}
	if c.cid != aer {
		t.Fatalf("aen %s: the completion of command %d, want the aer %d",
			what, c.cid, aer)
	}
	connStatus(t, c, statusSuccess, "aen "+what)
	if c.dw0 != aenDiscLogChanged {
		t.Fatalf("aen %s: dword0 %#08x, want %#08x", what, c.dw0,
			aenDiscLogChanged)
	}
}

// noAen asserts that the AER aer has not completed: no completion was put
// aside for it while the host waited for another, and none arrives within
// srvSilence.
func noAen(t *testing.T, h *fakeHost, aer uint16, what string) {
	t.Helper()
	if _, ok := h.pending[aer]; ok {
		t.Fatalf("aen %s: the aer completed, want no aen", what)
	}
	if p := srvAnswerWithin(t, h, srvSilence); p != nil {
		t.Fatalf("aen %s: pdu type %#x arrived, want nothing", what, p.typ)
	}
}
