package cdc

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The shared fixtures of the §8 unit tests: a fake clock for every timer, a
// fake etcd store for the watcher, a log-record capture for the §7
// assertions, and — the one that matters — an in-process fake NVMe/TCP host
// that speaks NP2/NP3 over a loopback socket.

func TestMain(m *testing.M) {
	// Tests that assert on records install their own capture handler; the
	// rest of the package's log volume goes nowhere.
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Fake clock
// ---------------------------------------------------------------------------

// fakeClock is the test implementation of the package clock. Every keep-alive
// deadline (NP10) and every rescan retry (WV5) goes through it, so advancing
// this drives them without a single real sleep.
type fakeClock struct {
	mu      sync.Mutex
	current time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		current: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &fakeWaiter{
		deadline: c.current.Add(d),
		ch:       make(chan time.Time, 1),
	}
	c.waiters = append(c.waiters, w)
	return w.ch
}

// waiterCount is how many timers are armed right now; a test waits on it
// before advancing, so it never races the goroutine it is driving.
func (c *fakeClock) waiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// waitWaiters blocks until at least n timers are armed.
func (c *fakeClock) waitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.waiterCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d timers armed, want %d", c.waiterCount(), n)
}

// advance moves the clock forward and fires every waiter that comes due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	now := c.current
	var fire []*fakeWaiter
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.deadline.After(now) {
			kept = append(kept, w)
			continue
		}
		fire = append(fire, w)
	}
	c.waiters = kept
	c.mu.Unlock()
	for _, w := range fire {
		w.ch <- now
	}
}

// ---------------------------------------------------------------------------
// Fake etcd store (EU2, EU3)
// ---------------------------------------------------------------------------

// fakeWatch is one generation of WatchTyped: the channels the watcher is
// consuming, plus the helpers a test drives them with.
type fakeWatch struct {
	fromRev int64
	events  chan etcdutil.Event
	errs    chan error
	once    sync.Once
}

// put pushes one put event.
func (w *fakeWatch) put(key string, msg *pb.CdcEntry) {
	w.events <- etcdutil.Event{
		Type: etcdutil.EventPut,
		Key:  key,
		Msg:  msg,
		Rev:  w.fromRev,
	}
}

// del pushes one delete event.
func (w *fakeWatch) del(key string) {
	w.events <- etcdutil.Event{
		Type: etcdutil.EventDelete,
		Key:  key,
		Rev:  w.fromRev,
	}
}

// fail ends the generation the way EU3 does: the error is reported once and
// BOTH channels close.
func (w *fakeWatch) fail(err error) {
	w.once.Do(func() {
		if err != nil {
			w.errs <- err
		}
		close(w.events)
		close(w.errs)
	})
}

// fakeStore is the etcdStore of the §8 watcher tests.
type fakeStore struct {
	mu       sync.Mutex
	kvs      map[string][]byte
	rev      int64
	rangeErr error
	rangeCnt int
	watches  chan *fakeWatch
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		kvs:     make(map[string][]byte),
		rev:     1,
		watches: make(chan *fakeWatch, 8),
	}
}

// set writes one entry into the fake store, as the gateway would.
func (s *fakeStore) set(t *testing.T, key string, msg *pb.CdcEntry) {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kvs[key] = raw
	s.rev++
}

// setRaw writes bytes that are not a CdcEntry, for the malformed_value path.
func (s *fakeStore) setRaw(key string, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kvs[key] = raw
	s.rev++
}

func (s *fakeStore) del(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kvs, key)
	s.rev++
}

// failRange makes the next Range calls fail, which is WV5's retry path.
func (s *fakeStore) failRange(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rangeErr = err
}

func (s *fakeStore) rangeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rangeCnt
}

func (s *fakeStore) Range(
	ctx context.Context,
	prefix string,
) ([]etcdutil.KV, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rangeCnt++
	if s.rangeErr != nil {
		return nil, 0, s.rangeErr
	}
	keys := make([]string, 0, len(s.kvs))
	for key := range s.kvs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	kvs := make([]etcdutil.KV, 0, len(keys))
	for _, key := range keys {
		kvs = append(kvs, etcdutil.KV{Key: key, Value: s.kvs[key]})
	}
	return kvs, s.rev, nil
}

func (s *fakeStore) Decode(
	ctx context.Context,
	kv etcdutil.KV,
	msg proto.Message,
) error {
	if err := proto.Unmarshal(kv.Value, msg); err != nil {
		return fmt.Errorf("decode %s: %w", kv.Key, err)
	}
	return nil
}

func (s *fakeStore) WatchTyped(
	ctx context.Context,
	prefix string,
	fromRev int64,
	newMsg func() proto.Message,
) (<-chan etcdutil.Event, <-chan error) {
	w := &fakeWatch{
		fromRev: fromRev,
		events:  make(chan etcdutil.Event),
		errs:    make(chan error, 1),
	}
	go func() {
		<-ctx.Done()
		w.fail(nil)
	}()
	s.watches <- w
	return w.events, w.errs
}

// nextWatch waits for the watcher to open its next watch generation.
func (s *fakeStore) nextWatch(t *testing.T) *fakeWatch {
	t.Helper()
	select {
	case w := <-s.watches:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("no watch was opened")
		return nil
	}
}

// ---------------------------------------------------------------------------
// Log capture (§7)
// ---------------------------------------------------------------------------

// logCapture collects the records emitted while it is installed, so the tests
// can assert on the normative §7 msg strings and attributes.
type logCapture struct {
	mu      sync.Mutex
	records []map[string]any
}

type captureHandler struct {
	cap   *logCapture
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := map[string]any{"msg": r.Message, "level": r.Level.String()}
	for _, a := range h.attrs {
		rec[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.Any()
		return true
	})
	h.cap.mu.Lock()
	h.cap.records = append(h.cap.records, rec)
	h.cap.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &captureHandler{cap: h.cap, attrs: merged}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// captureLogs installs a capture for the duration of one test.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	cap := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&captureHandler{cap: cap}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

// find returns every captured record with the given msg.
func (c *logCapture) find(msg string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, rec := range c.records {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// count is how many records carry the given msg.
func (c *logCapture) count(msg string) int {
	return len(c.find(msg))
}

// waitFor blocks until at least n records with msg have been captured.
func (c *logCapture) waitFor(t *testing.T, msg string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if recs := c.find(msg); len(recs) >= n {
			return recs
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d %q records, want %d", c.count(msg), msg, n)
	return nil
}

// ---------------------------------------------------------------------------
// Entry fixtures
// ---------------------------------------------------------------------------

// testCid is the cluster id every fixture entry lives under.
const testCid = uint64(0xcdc)

// trConf builds one transport configuration.
func trConf(trType, adrFam, trAddr, trSvcId string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  trType,
		AdrFam:  adrFam,
		TrAddr:  trAddr,
		TrSvcId: trSvcId,
	}
}

// tcpConf is the common case: a tcp/ipv4 port.
func tcpConf(trAddr, trSvcId string) *pb.NvmeTrConf {
	return trConf(
		common.DefaultCdcTrType, common.DefaultCdcAdrFam, trAddr, trSvcId,
	)
}

// cdcEntry builds one CdcEntry value.
func cdcEntry(nqn string, allowed []string, confs ...*pb.NvmeTrConf) *pb.CdcEntry {
	return &pb.CdcEntry{
		Nqn:            nqn,
		NvmeTrConfList: confs,
		AllowedHosts:   allowed,
	}
}

// testKey builds one CdcEntry key.
func testKey(shard uint32, spId uint64, ssId uint64) string {
	return model.CdcEntryKey(testCid, shard, spId, ssId)
}

// stubConn is a connection with only the state notify() touches: the view
// tests need something to attach without a socket behind it.
func stubConn() *conn {
	return &conn{
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
	}
}

// poked reports whether a stub connection has been woken.
func poked(c *conn) bool {
	select {
	case <-c.wake:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// A running server (NP1)
// ---------------------------------------------------------------------------

// testServer is one dnv-cdc instance on a loopback port, with the fake clock
// and the fake store behind it.
type testServer struct {
	t     *testing.T
	srv   *server
	reg   *registry
	store *fakeStore
	clk   *fakeClock
	addr  string
	// cancel ends the instance's context, which is the NP1 shutdown path.
	cancel context.CancelFunc
	done   chan struct{}
}

// stop cancels the instance and waits for its accept loop to return: what a
// SIGTERM does in production (NP1, CM5).
func (ts *testServer) stop() {
	ts.cancel()
	<-ts.done
}

// startServer starts a listener-only instance: no watcher, so the tests drive
// the registry directly. ranges is used only to build the deps.
func startServer(t *testing.T) *testServer {
	t.Helper()
	clk := newFakeClock()
	store := newFakeStore()
	d := &deps{
		cfg: Config{
			Ranges:  []uint32{0},
			TrType:  common.DefaultCdcTrType,
			AdrFam:  common.DefaultCdcAdrFam,
			TrAddr:  "127.0.0.1",
			TrSvcId: "0",
		},
		store: store,
		clk:   clk,
	}
	reg := newRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := newServer(ctx, d, reg)
	if err != nil {
		cancel()
		t.Fatalf("newServer: %v", err)
	}
	ts := &testServer{
		t:      t,
		srv:    srv,
		reg:    reg,
		store:  store,
		clk:    clk,
		addr:   srv.addr().String(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(ts.done)
		srv.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-ts.done
	})
	return ts
}

// ---------------------------------------------------------------------------
// The in-process fake NVMe/TCP host (§8)
// ---------------------------------------------------------------------------

// fakeHost is a minimal NVMe/TCP host: it speaks exactly the NP2/NP3 subset
// dnv-cdc implements, so the server tests exercise the real codec rather than
// a mock of it.
type fakeHost struct {
	t  *testing.T
	nc net.Conn
	br *bufio.Reader

	nextCid uint16
	// pending holds completions read while waiting for another command's,
	// which is how an AER completion arrives in the middle of a Get Log
	// Page.
	pending map[uint16]completion
	// data holds C2HData payloads by command id.
	data map[uint16][]byte
}

// dial opens a connection to a running test server.
func (ts *testServer) dial() *fakeHost {
	ts.t.Helper()
	nc, err := net.Dial("tcp", ts.addr)
	if err != nil {
		ts.t.Fatalf("dial: %v", err)
	}
	h := &fakeHost{
		t:       ts.t,
		nc:      nc,
		br:      bufio.NewReaderSize(nc, maxPduLen),
		pending: make(map[uint16]completion),
		data:    make(map[uint16][]byte),
	}
	ts.t.Cleanup(func() { nc.Close() })
	return h
}

// close drops the socket, which is what a host that goes away looks like.
func (h *fakeHost) close() {
	h.nc.Close()
}

// write sends raw bytes.
func (h *fakeHost) write(buf []byte) {
	h.t.Helper()
	if err := h.nc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		h.t.Fatalf("set write deadline: %v", err)
	}
	if _, err := h.nc.Write(buf); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// recv reads one PDU with a test-scale deadline.
func (h *fakeHost) recv() (*pdu, error) {
	if err := h.nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	return readCtrlPdu(h.br)
}

// ctrlHdrLenFor is hdrLenFor's mirror for the PDUs a CONTROLLER sends. readPdu
// refuses them on purpose: a controller that is handed an ICResp, a
// CapsuleResp or C2HData has been sent a PDU of the wrong direction, which is
// exactly the NP2 protocol error it terminates on. The fake host reads that
// direction, so it needs its own table.
func ctrlHdrLenFor(typ uint8) (int, bool) {
	switch typ {
	case pduICResp:
		return icRespLen, true
	case pduC2HTermReq:
		return termReqHdrLen, true
	case pduCapsuleResp:
		return capsuleRespLen, true
	case pduC2HData:
		return c2hDataHdrLen, true
	}
	return 0, false
}

// readCtrlPdu reads one controller-to-host PDU. It mirrors readPdu's framing
// and validation so that a malformed controller PDU fails the test that
// produced it instead of being silently accepted.
func readCtrlPdu(r io.Reader) (*pdu, error) {
	var ch [pduCommonHdrLen]byte
	if _, err := io.ReadFull(r, ch[:]); err != nil {
		return nil, err
	}
	p := &pdu{
		typ:   ch[0],
		flags: ch[1],
		hlen:  ch[2],
		pdo:   ch[3],
		plen:  binary.LittleEndian.Uint32(ch[4:8]),
	}
	want, known := ctrlHdrLenFor(p.typ)
	if !known {
		return nil, fmt.Errorf("controller sent pdu type %#x", p.typ)
	}
	if int(p.hlen) != want {
		return nil, fmt.Errorf("pdu type %#x: hlen %d, want %d",
			p.typ, p.hlen, want)
	}
	if p.plen < uint32(p.hlen) {
		return nil, fmt.Errorf("pdu type %#x: plen %d below hlen %d",
			p.typ, p.plen, p.hlen)
	}
	rest := make([]byte, int(p.plen)-pduCommonHdrLen)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	p.hdr = make([]byte, 0, want)
	p.hdr = append(p.hdr, ch[:]...)
	p.hdr = append(p.hdr, rest[:want-pduCommonHdrLen]...)
	if p.plen > uint32(p.hlen) {
		p.data = rest[int(p.pdo)-pduCommonHdrLen:]
	}
	return p, nil
}

// handshake performs NP3 and returns the decoded ICResp fields.
func (h *fakeHost) handshake() (pfv uint16, cpda uint8, digest uint8, maxh2c uint32) {
	h.t.Helper()
	h.write(buildICReqFor(0, 0, 0))
	p, err := h.recv()
	if err != nil {
		h.t.Fatalf("icresp: %v", err)
	}
	if p.typ != pduICResp {
		h.t.Fatalf("icresp: pdu type %#x", p.typ)
	}
	if p.plen != icRespLen {
		h.t.Fatalf("icresp: plen %d, want %d", p.plen, icRespLen)
	}
	return binary.LittleEndian.Uint16(p.hdr[8:10]),
		p.hdr[10],
		p.hdr[11],
		binary.LittleEndian.Uint32(p.hdr[12:16])
}

// buildICReqFor builds an ICReq with the given PFV, HPDA and digest bits.
func buildICReqFor(pfv uint16, hpda uint8, digest uint8) []byte {
	buf := make([]byte, icReqLen)
	buf[0] = pduICReq
	buf[2] = icReqLen
	binary.LittleEndian.PutUint32(buf[4:8], icReqLen)
	binary.LittleEndian.PutUint16(buf[8:10], pfv)
	buf[10] = hpda
	buf[11] = digest
	return buf
}

// allocCid hands out the next command id.
func (h *fakeHost) allocCid() uint16 {
	h.nextCid++
	return h.nextCid
}

// send writes one CapsuleCmd, with in-capsule data when data is non-empty.
func (h *fakeHost) send(s []byte, data []byte) {
	h.t.Helper()
	plen := capsuleCmdHdrLen + len(data)
	buf := make([]byte, plen)
	buf[0] = pduCapsuleCmd
	buf[2] = capsuleCmdHdrLen
	if len(data) > 0 {
		buf[3] = capsuleCmdHdrLen
	}
	binary.LittleEndian.PutUint32(buf[4:8], uint32(plen))
	copy(buf[pduCommonHdrLen:], s)
	copy(buf[capsuleCmdHdrLen:], data)
	h.write(buf)
}

// await reads PDUs until the completion of cid arrives, stashing anything
// else (another command's completion, a C2HData payload) on the way.
func (h *fakeHost) await(cid uint16) completion {
	h.t.Helper()
	if c, ok := h.pending[cid]; ok {
		delete(h.pending, cid)
		return c
	}
	for {
		p, err := h.recv()
		if err != nil {
			h.t.Fatalf("await %d: %v", cid, err)
		}
		switch p.typ {
		case pduC2HData:
			dataCid := binary.LittleEndian.Uint16(p.hdr[8:10])
			h.data[dataCid] = append(h.data[dataCid], p.data...)
		case pduCapsuleResp:
			c := parseCapsuleResp(p)
			if c.cid == cid {
				return c
			}
			h.pending[c.cid] = c
		case pduC2HTermReq:
			h.t.Fatalf("await %d: c2h term req, fes %#x", cid,
				binary.LittleEndian.Uint16(p.hdr[8:10]))
		default:
			h.t.Fatalf("await %d: unexpected pdu type %#x", cid, p.typ)
		}
	}
}

// pollPending reads any completion that is already waiting on the socket,
// without blocking beyond d. It is how an AEN is observed.
func (h *fakeHost) pollPending(d time.Duration) (completion, bool) {
	h.t.Helper()
	deadline := time.Now().Add(d)
	for {
		if err := h.nc.SetReadDeadline(deadline); err != nil {
			return completion{}, false
		}
		p, err := readCtrlPdu(h.br)
		if err != nil {
			return completion{}, false
		}
		switch p.typ {
		case pduC2HData:
			dataCid := binary.LittleEndian.Uint16(p.hdr[8:10])
			h.data[dataCid] = append(h.data[dataCid], p.data...)
		case pduCapsuleResp:
			return parseCapsuleResp(p), true
		}
	}
}

// newSqe builds a zeroed SQE with the opcode and command id set.
func newSqe(opc uint8, cid uint16) []byte {
	s := make([]byte, sqeLen)
	s[0] = opc
	binary.LittleEndian.PutUint16(s[2:4], cid)
	return s
}

// setSgl fills in the one SGL descriptor: a transport data block of len bytes,
// which is what the Linux host sends for a controller-to-host transfer.
func setSgl(s []byte, length uint32) {
	binary.LittleEndian.PutUint32(s[32:36], length)
	s[39] = 0x5<<4 | 0xa
}

// connect performs the NP5 fabrics connect and returns the completion.
func (h *fakeHost) connect(
	hostNqn string,
	subNqn string,
	katoMs uint32,
	sqSize uint16,
	cattr uint8,
) completion {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcFabrics, cid)
	s[4] = fctypeConnect
	setSgl(s, connectDataLen)
	s[39] = 0x5<<4 | 0x1 // in-capsule data
	binary.LittleEndian.PutUint16(s[40:42], 0)
	binary.LittleEndian.PutUint16(s[42:44], 0)
	binary.LittleEndian.PutUint16(s[44:46], sqSize)
	s[46] = cattr
	binary.LittleEndian.PutUint32(s[48:52], katoMs)
	data := make([]byte, connectDataLen)
	copy(data[connectDataSubNqnOff:], subNqn)
	copy(data[connectDataHostNqnOff:], hostNqn)
	h.send(s, data)
	return h.await(cid)
}

// connectOk is connect plus the assertion that it worked; it returns the
// CNTLID.
func (h *fakeHost) connectOk(hostNqn string) uint16 {
	h.t.Helper()
	h.handshake()
	c := h.connect(
		hostNqn, common.NvmeDiscoveryNqn, 0, common.CdcMaxAdminSqSize-1, 0,
	)
	if c.status != statusSuccess {
		h.t.Fatalf("connect: status %#x", c.status)
	}
	return uint16(c.dw0)
}

// propertyGet issues a fabrics property get.
func (h *fakeHost) propertyGet(offset uint32, wide bool) completion {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcFabrics, cid)
	s[4] = fctypePropertyGet
	if wide {
		s[40] = 1
	}
	binary.LittleEndian.PutUint32(s[44:48], offset)
	h.send(s, nil)
	return h.await(cid)
}

// propertySet issues a fabrics property set.
func (h *fakeHost) propertySet(offset uint32, value uint64) completion {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcFabrics, cid)
	s[4] = fctypePropertySet
	binary.LittleEndian.PutUint32(s[44:48], offset)
	binary.LittleEndian.PutUint64(s[48:56], value)
	h.send(s, nil)
	return h.await(cid)
}

// identify issues Identify with the given CNS and returns the payload.
func (h *fakeHost) identify(cns uint8) (completion, []byte) {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcIdentify, cid)
	setSgl(s, identifyLen)
	s[40] = cns
	h.send(s, nil)
	c := h.await(cid)
	return c, h.data[cid]
}

// getLogPage issues Get Log Page and returns the payload.
func (h *fakeHost) getLogPage(lid uint8, length uint32, off uint64) (completion, []byte) {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcGetLogPage, cid)
	setSgl(s, length)
	s[40] = lid
	s[41] = 0x80 // RAE
	numd := length/4 - 1
	binary.LittleEndian.PutUint16(s[42:44], uint16(numd))
	binary.LittleEndian.PutUint16(s[44:46], uint16(numd>>16))
	binary.LittleEndian.PutUint32(s[48:52], uint32(off))
	binary.LittleEndian.PutUint32(s[52:56], uint32(off>>32))
	h.send(s, nil)
	c := h.await(cid)
	return c, h.data[cid]
}

// setFeatures and getFeatures drive NP9.
func (h *fakeHost) setFeatures(fid uint8, dword11 uint32) completion {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcSetFeatures, cid)
	s[40] = fid
	binary.LittleEndian.PutUint32(s[44:48], dword11)
	h.send(s, nil)
	return h.await(cid)
}

func (h *fakeHost) getFeatures(fid uint8) completion {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcGetFeatures, cid)
	s[40] = fid
	h.send(s, nil)
	return h.await(cid)
}

// keepAlive issues one Keep Alive command.
func (h *fakeHost) keepAlive() completion {
	h.t.Helper()
	cid := h.allocCid()
	h.send(newSqe(opcKeepAlive, cid), nil)
	return h.await(cid)
}

// armAer submits one Asynchronous Event Request and returns its command id
// WITHOUT waiting: an AER completes only when there is an event.
func (h *fakeHost) armAer() uint16 {
	h.t.Helper()
	cid := h.allocCid()
	h.send(newSqe(opcAsyncEvent, cid), nil)
	return cid
}

// enableAen turns on the discovery-log-change notice (NP9).
func (h *fakeHost) enableAen() {
	h.t.Helper()
	if c := h.setFeatures(fidAsyncEventConfig, aenCfgDiscChange); c.status != statusSuccess {
		h.t.Fatalf("set features 0bh: status %#x", c.status)
	}
}
