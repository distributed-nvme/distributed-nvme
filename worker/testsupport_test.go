package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Observation stamps (dnagent.md SH28; dnv-worker.md HL7, RW23)
// ---------------------------------------------------------------------------

// testObsCount numbers the observation times the stub agents hand out. A
// counter, not time.Now: inside a synctest bubble time stands still between
// blocking points, so two replies would share a time and read as one
// observation.
var testObsCount atomic.Int64

// testObsBase is the first observation time's base; any fixed instant will
// do, since the worker only ever compares a node's times with each other.
var testObsBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// nextObsTime returns an observation time later than every one returned
// before it.
func nextObsTime() time.Time {
	return testObsBase.Add(
		time.Duration(testObsCount.Add(1)) * time.Millisecond)
}

// testStamp is the stamp of a reply that answers from a new observation made
// as it answers: a new time, age zero, no converge running.
func testStamp() *pb.ObsStamp {
	at := nextObsTime()
	return &pb.ObsStamp{
		ObsTime:    timestamppb.New(at),
		AnswerTime: timestamppb.New(at),
	}
}

// agedStamp is the stamp of a reply that answers age after an observation
// made under traceId.
func agedStamp(age time.Duration, traceId string) *pb.ObsStamp {
	at := nextObsTime()
	return &pb.ObsStamp{
		ObsTime:    timestamppb.New(at),
		ObsTraceId: traceId,
		AnswerTime: timestamppb.New(at.Add(age)),
	}
}

// testNodeBase is where the loop tests that judge an observation's age (HL7)
// start their agent's clock, a day past testObsBase, so that no time
// nextObsTime hands out — a default-stamped Syncup* reply's, say — reads
// later than theirs. Those tests give every round's answer time themselves:
// HL7 counts an age from no earlier than the first round after a pause, two
// rounds the agent answered more than the bound apart, and the answer times
// of testStamp and agedStamp jump about.
var testNodeBase = testObsBase.Add(24 * time.Hour)

// answerStamp is the stamp of a reply answered at at from no observation.
func answerStamp(at time.Time) *pb.ObsStamp {
	return &pb.ObsStamp{AnswerTime: timestamppb.New(at)}
}

// stamped returns reply with testStamp's stamp when it carries none — a copy,
// since a test's builder may hand one reply to every round — and as it is
// when it carries one, which is the test's own. The stub agents send every
// reply through it, a Syncup* reply a test builds through syncupStamped, so
// a loop test that sets no stamp sees each round answer from a new
// observation, as a live agent's reads give it.
func stamped[R interface {
	proto.Message
	GetObsStamp() *pb.ObsStamp
}](reply R) R {
	if !reply.ProtoReflect().IsValid() || reply.GetObsStamp() != nil {
		return reply
	}
	out := proto.CloneOf(reply)
	msg := out.ProtoReflect()
	msg.Set(msg.Descriptor().Fields().ByName("obs_stamp"),
		protoreflect.ValueOfMessage(testStamp().ProtoReflect()))
	return out
}

// syncupStamped is stamped for a Syncup* reply, but a reply that refuses its
// request it returns as it is: a live agent's refusal carries no stamp
// (architecture.md, Live-state reporting), so a stub refusal gets one only
// when the test sets it.
func syncupStamped[R interface {
	proto.Message
	GetObsStamp() *pb.ObsStamp
	GetAgentReply() *pb.AgentReply
}](reply R) R {
	if code := reply.GetAgentReply().GetCode(); reply.ProtoReflect().IsValid() &&
		code != 0 && code != common.ReplyCodeLeftover {
		return reply
	}
	return stamped(reply)
}

// ---------------------------------------------------------------------------
// Fake clock
// ---------------------------------------------------------------------------

// fakeClock is the test implementation of the package clock. Every timer of
// worker/ goes through the clock interface, so advancing this one drives the
// vote grace windows, the liveness deadlines, the heartbeat ticker and the
// round timers without a single real sleep.
type fakeClock struct {
	mu      sync.Mutex
	current time.Time
	waiters []*fakeWaiter
}

// fakeWaiter is one armed timer or ticker.
type fakeWaiter struct {
	deadline time.Time
	period   time.Duration
	ch       chan time.Time
	stopped  bool
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

func (c *fakeClock) nowUnix() uint64 {
	return uint64(c.now().Unix())
}

func (c *fakeClock) arm(d time.Duration, period time.Duration) *fakeWaiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &fakeWaiter{
		deadline: c.current.Add(d),
		period:   period,
		ch:       make(chan time.Time, 1),
	}
	c.waiters = append(c.waiters, w)
	return w
}

func (c *fakeClock) newTimer(d time.Duration) *timerHandle {
	w := c.arm(d, 0)
	return &timerHandle{C: w.ch, stop: func() bool { return c.cancel(w) }}
}

func (c *fakeClock) newTicker(d time.Duration) *tickerHandle {
	w := c.arm(d, d)
	return &tickerHandle{C: w.ch, stop: func() { c.cancel(w) }}
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	return c.arm(d, 0).ch
}

func (c *fakeClock) cancel(w *fakeWaiter) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.stopped {
		return false
	}
	w.stopped = true
	kept := c.waiters[:0]
	for _, other := range c.waiters {
		if other != w {
			kept = append(kept, other)
		}
	}
	c.waiters = kept
	return true
}

// advance moves the clock forward and fires every waiter that comes due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	now := c.current
	var fire []*fakeWaiter
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.stopped {
			continue
		}
		if w.deadline.After(now) {
			kept = append(kept, w)
			continue
		}
		fire = append(fire, w)
		if w.period > 0 {
			for !w.deadline.After(now) {
				w.deadline = w.deadline.Add(w.period)
			}
			kept = append(kept, w)
			continue
		}
		w.stopped = true
	}
	c.waiters = kept
	c.mu.Unlock()
	for _, w := range fire {
		select {
		case w.ch <- now:
		default:
		}
	}
}

// waiterCount reports how many timers are armed right now.
func (c *fakeClock) waiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// ---------------------------------------------------------------------------
// Fake etcd store
// ---------------------------------------------------------------------------

// fakeStore is an in-memory etcdStore: a key/value map with a revision
// counter and prefix watches that follow the EU3 contract (an error is
// buffered on the error channel BEFORE both channels close).
type fakeStore struct {
	mu       sync.Mutex
	kvs      map[string][]byte
	rev      int64
	watchers []*fakeWatch
	// modRevs is each key's mod_revision: the store revision of its last
	// write, which Range reports (EU2) and VW3's rescan compares.
	modRevs map[string]int64

	putErr   error
	rangeErr error
	getErr   error
	// muteEvents drops every watch event without touching the store, the way
	// a broken watch that still looks open does (VW8b).
	muteEvents bool
	// mutedPrefixes drops the watch events of the keys under these prefixes
	// only, the way one role's broken watch does while the others still
	// deliver (VW8 (b), VW10).
	mutedPrefixes map[string]bool

	puts    []string
	deletes []string
}

// fakeWatch is one open prefix watch.
//
// Its own mutex — not the store's — guards the end of the watch. A delivery
// starts inside the store lock (matching) but SENDS after it has been released,
// while a watch is torn down from the goroutine WatchTyped left waiting on the
// caller's context: with one lock covering only the first half, a close could
// land between the two and panic with "send on closed channel". That is not a
// theoretical interleaving — it is what a fence does, whose new incarnation
// puts (VW2) while the old incarnation's watches are being cancelled (VW8).
type fakeWatch struct {
	prefix string
	newMsg func() proto.Message
	evCh   chan etcdutil.Event
	errCh  chan error

	mu     sync.Mutex
	closed bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		kvs:     make(map[string][]byte),
		rev:     1,
		modRevs: make(map[string]int64),
	}
}

// seed writes a key without emitting a watch event, as if it had been there
// before the test started.
func (s *fakeStore) seed(t *testing.T, key string, msg proto.Message) {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev++
	s.kvs[key] = data
	s.modRevs[key] = s.rev
}

func (s *fakeStore) Get(
	ctx context.Context, key string, msg proto.Message,
) (bool, error) {
	s.mu.Lock()
	err := s.getErr
	data, ok := s.kvs[key]
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		return false, err
	}
	return true, nil
}

func (s *fakeStore) Put(
	ctx context.Context, key string, msg proto.Message,
) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.putErr != nil {
		err = s.putErr
		s.mu.Unlock()
		return err
	}
	s.rev++
	rev := s.rev
	s.kvs[key] = data
	s.modRevs[key] = rev
	s.puts = append(s.puts, key)
	watchers := s.matching(key)
	s.mu.Unlock()
	for _, w := range watchers {
		value := w.newMsg()
		if err := proto.Unmarshal(data, value); err != nil {
			continue
		}
		w.deliver(etcdutil.Event{
			Type: etcdutil.EventPut, Key: key, Msg: value, Rev: rev,
		})
	}
	return nil
}

func (s *fakeStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.rev++
	rev := s.rev
	_, existed := s.kvs[key]
	delete(s.kvs, key)
	delete(s.modRevs, key)
	s.deletes = append(s.deletes, key)
	watchers := s.matching(key)
	s.mu.Unlock()
	if !existed {
		return nil
	}
	for _, w := range watchers {
		w.deliver(etcdutil.Event{
			Type: etcdutil.EventDelete, Key: key, Rev: rev,
		})
	}
	return nil
}

func (s *fakeStore) Range(
	ctx context.Context, prefix string,
) ([]etcdutil.KV, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rangeErr != nil {
		return nil, 0, s.rangeErr
	}
	keys := make([]string, 0, len(s.kvs))
	for key := range s.kvs {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	kvs := make([]etcdutil.KV, 0, len(keys))
	for _, key := range keys {
		kvs = append(kvs, etcdutil.KV{
			Key:    key,
			Value:  s.kvs[key],
			ModRev: s.modRevs[key],
		})
	}
	return kvs, s.rev, nil
}

func (s *fakeStore) Decode(
	ctx context.Context, kv etcdutil.KV, msg proto.Message,
) error {
	return proto.Unmarshal(kv.Value, msg)
}

// SnapshotRev runs f once over a read-only view of the store as it stands
// when the call begins, the one store revision every read of the view is
// served at, and returns that revision (EU4). As in the production view, a
// read fails while getErr is set, and a Put or a Del inside f is a misuse:
// the first failure ends the snapshot with its error, ahead of whatever f
// returns, and every read after it finds nothing.
func (s *fakeStore) SnapshotRev(
	ctx context.Context,
	f func(stm etcdutil.STM) error,
) (int64, error) {
	s.mu.Lock()
	view := &fakeSnapshot{
		kvs:     maps.Clone(s.kvs),
		modRevs: maps.Clone(s.modRevs),
		getErr:  s.getErr,
	}
	rev := s.rev
	s.mu.Unlock()
	callbackErr := f(view)
	if view.err != nil {
		return rev, fmt.Errorf("etcd snapshot: %w", view.err)
	}
	return rev, callbackErr
}

// fakeSnapshot is the read-only STM view of fakeStore.SnapshotRev.
type fakeSnapshot struct {
	kvs     map[string][]byte
	modRevs map[string]int64
	getErr  error
	// err is the first failure or misuse of the snapshot.
	err error
}

func (v *fakeSnapshot) fail(err error) {
	if v.err == nil {
		v.err = err
	}
}

func (v *fakeSnapshot) Get(key string, msg proto.Message) bool {
	switch {
	case v.err != nil:
		return false
	case v.getErr != nil:
		v.fail(fmt.Errorf("snapshot get %s: %w", key, v.getErr))
		return false
	}
	data, ok := v.kvs[key]
	if !ok {
		return false
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		v.fail(fmt.Errorf("snapshot get %s: unmarshal: %w", key, err))
		return false
	}
	return true
}

func (v *fakeSnapshot) Put(key string, msg proto.Message) {
	v.fail(fmt.Errorf("snapshot put %s: the snapshot is read-only", key))
}

func (v *fakeSnapshot) Del(key string) {
	v.fail(fmt.Errorf("snapshot delete %s: the snapshot is read-only", key))
}

func (v *fakeSnapshot) Rev(key string) int64 {
	switch {
	case v.err != nil:
		return 0
	case v.getErr != nil:
		v.fail(fmt.Errorf("snapshot rev %s: %w", key, v.getErr))
		return 0
	}
	return v.modRevs[key]
}

func (s *fakeStore) WatchTyped(
	ctx context.Context,
	prefix string,
	fromRev int64,
	newMsg func() proto.Message,
) (<-chan etcdutil.Event, <-chan error) {
	w := &fakeWatch{
		prefix: prefix,
		newMsg: newMsg,
		evCh:   make(chan etcdutil.Event, 256),
		errCh:  make(chan error, 1),
	}
	s.mu.Lock()
	s.watchers = append(s.watchers, w)
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.closeWatch(w, nil)
	}()
	return w.evCh, w.errCh
}

// matching returns the watchers a key's event belongs to. The caller holds
// the store lock; deliver re-checks each watch under the WATCH lock, which is
// what actually decides whether the send is safe.
func (s *fakeStore) matching(key string) []*fakeWatch {
	if s.muteEvents {
		return nil
	}
	for prefix := range s.mutedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return nil
		}
	}
	var out []*fakeWatch
	for _, w := range s.watchers {
		if !w.isClosed() && strings.HasPrefix(key, w.prefix) {
			out = append(out, w)
		}
	}
	return out
}

// isClosed reports whether the watch has ended.
func (w *fakeWatch) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// deliver queues one event, under the watch lock so that it cannot race the
// close of the channel it sends on.
func (w *fakeWatch) deliver(event etcdutil.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.evCh <- event:
	default:
	}
}

// closeWatch ends one watch the EU3 way: the error, if any, is buffered
// first, then both channels close. It takes only the watch's own lock, so the
// store lock is never held across it and the two can never deadlock.
func (s *fakeStore) closeWatch(w *fakeWatch, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	if err != nil {
		w.errCh <- err
	}
	close(w.evCh)
	close(w.errCh)
}

// breakWatches ends every open watch with err, the way a compaction does.
func (s *fakeStore) breakWatches(err error) {
	s.mu.Lock()
	watchers := append([]*fakeWatch(nil), s.watchers...)
	s.mu.Unlock()
	for _, w := range watchers {
		s.closeWatch(w, err)
	}
}

// watchCount reports how many watches are open.
func (s *fakeStore) watchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.watchers {
		if !w.isClosed() {
			n++
		}
	}
	return n
}

func (s *fakeStore) setPutErr(err error) {
	s.mu.Lock()
	s.putErr = err
	s.mu.Unlock()
}

func (s *fakeStore) setMuteEvents(mute bool) {
	s.mu.Lock()
	s.muteEvents = mute
	s.mu.Unlock()
}

// setMutePrefix mutes or unmutes the watch events of the keys under one
// prefix.
func (s *fakeStore) setMutePrefix(prefix string, mute bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutedPrefixes == nil {
		s.mutedPrefixes = make(map[string]bool)
	}
	if mute {
		s.mutedPrefixes[prefix] = true
	} else {
		delete(s.mutedPrefixes, prefix)
	}
}

func (s *fakeStore) setRangeErr(err error) {
	s.mu.Lock()
	s.rangeErr = err
	s.mu.Unlock()
}

func (s *fakeStore) putKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

func (s *fakeStore) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletes...)
}

func (s *fakeStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.kvs[key]
	return ok
}

// ---------------------------------------------------------------------------
// Fake health writer
// ---------------------------------------------------------------------------

// healthWrite is one recorded err_epoch write (HL3). settle is a cntlr
// write's request to clear the record's settling flag too (HL2).
type healthWrite struct {
	record  string
	cid     uint64
	addr    string
	spId    uint64
	sliceId uint64
	objId   uint64
	epoch   uint64
	settle  bool
}

// fakeHealthWriter records the MD6 ops health.go would have run.
type fakeHealthWriter struct {
	mu     sync.Mutex
	writes []healthWrite
	err    error
}

func (w *fakeHealthWriter) record(entry healthWrite) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.writes = append(w.writes, entry)
	return nil
}

// refusal is the error the fake fails every write with, nil while writes
// succeed. A wrapper that also lands each write in a store of its own asks it
// first, so that a write the fake fails lands nowhere.
func (w *fakeHealthWriter) refusal() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *fakeHealthWriter) all() []healthWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]healthWrite(nil), w.writes...)
}

func (w *fakeHealthWriter) setDnErrEpoch(
	ctx context.Context,
	cid uint64,
	addrPort string,
	epoch uint64,
	cc *pb.ClusterConf,
) error {
	return w.record(healthWrite{
		record: healthRecordDn, cid: cid, addr: addrPort, epoch: epoch,
	})
}

func (w *fakeHealthWriter) setCnErrEpoch(
	ctx context.Context, cid uint64, addrPort string, epoch uint64,
) error {
	return w.record(healthWrite{
		record: healthRecordCn, cid: cid, addr: addrPort, epoch: epoch,
	})
}

func (w *fakeHealthWriter) setCntlrErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	cntlrId uint64,
	epoch uint64,
	settle bool,
) error {
	return w.record(healthWrite{
		record: healthRecordCntlr, cid: cid, spId: spId,
		objId: cntlrId, epoch: epoch, settle: settle,
	})
}

func (w *fakeHealthWriter) setLegErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	sliceId uint64,
	legId uint64,
	epoch uint64,
) error {
	return w.record(healthWrite{
		record: healthRecordLeg, cid: cid, spId: spId,
		sliceId: sliceId, objId: legId, epoch: epoch,
	})
}

func (w *fakeHealthWriter) setSideErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	sliceId uint64,
	sideId uint64,
	epoch uint64,
) error {
	return w.record(healthWrite{
		record: healthRecordSide, cid: cid, spId: spId,
		sliceId: sliceId, objId: sideId, epoch: epoch,
	})
}

// ---------------------------------------------------------------------------
// Log capture
// ---------------------------------------------------------------------------

// syncBuffer is a mutex-guarded buffer, so the capture handler is safe for the
// worker's many goroutines.
type syncBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// logCapture installs a JSON slog handler over a buffer for one test.
type logCapture struct {
	buf *syncBuffer
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	buf := &syncBuffer{}
	handler := &common.TraceIdHandler{
		Handler: slog.NewJSONHandler(buf, nil),
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logCapture{buf: buf}
}

// records decodes every captured JSON line.
func (c *logCapture) records() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// withMsg returns every record whose msg equals msg.
func (c *logCapture) withMsg(msg string) []map[string]any {
	var out []map[string]any
	for _, rec := range c.records() {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// msgOrder returns the msg strings of every record whose msg is one of the
// listed ones, in emission order.
func (c *logCapture) msgOrder(msgs ...string) []string {
	want := make(map[string]bool, len(msgs))
	for _, msg := range msgs {
		want[msg] = true
	}
	var out []string
	for _, rec := range c.records() {
		msg, _ := rec["msg"].(string)
		if want[msg] {
			out = append(out, msg)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Stored-conf fixtures (architecture.md, Common validation)
// ---------------------------------------------------------------------------

// Defaults are resolved on the WRITE path: CreateCluster stores a ClusterConf
// whose every defaultable member is already concrete, and CreateStoragePool
// does the same for an SP's bdev_conf. Nothing in worker/ substitutes a member
// of either conf on the way back out — the RW21 cache hands a reader the conf
// exactly as stored, and each reader validates it (model.ValidateClusterConf /
// ValidateBdevConf) and refuses what it cannot use.
//
// So a fixture that leaves a defaultable member at its proto3 zero describes
// a corrupt cluster, one the gateway could not have created, and the loops in
// this package refuse it. The two builders below are this package's ONE
// source of a usable stored conf.
//
// They are written out literally rather than run through model.Resolve*, so a
// test sees the bytes the worker is actually handed, and so an invalid-conf
// test can be written as "the builder's result with one member cleared".

// testBlockSize is the fixture SP's stored dm_pool_conf.data_block_size, and
// is deliberately NOT common.DefaultDmPoolDataBlockSize (1 MiB): it is the one
// bdev_conf member a worker request carries as a NUMBER rather than inside the
// verbatim bdev_conf, so a request that substituted the default of
// architecture.md, Common validation, instead of forwarding the stored value
// has to read as a different number somewhere. It is a legal dm-thin data block
// size (a multiple of 64 KiB, inside the bounds of architecture.md, Common
// validation), so model.ValidateBdevConf accepts the fixture.
const testBlockSize = uint64(4) << 20

// testBdevConf is the concrete SP geometry CreateStoragePool stores: every
// member a value the write path resolved, spelled out. All but the data block
// size are the defaults of architecture.md, Common validation, written down;
// that one is testBlockSize (see it).
func testBdevConf() *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   testBlockSize,
			LowWaterMarkPct: common.DefaultPoolLowWatermarkPct,
		},
		DmRaid0Conf: &pb.DmRaid0Conf{
			StripeSize: common.DefaultDmRaid0StripeSize,
		},
		RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: common.DefaultChunkBlockCnt,
				},
			},
		},
	}
}

// testClusterConf is a concrete ClusterConf as CreateCluster stores it: the
// extent size, the 0/4/8/12 bin ladder, both batch sizes, the four
// health-check intervals and a bdev_conf, every one of them a value the
// gateway resolved at write time. Most of them are written as the
// common.Default* constants, because that is what the WRITE path resolves them
// from for a request that asked for nothing — nothing re-derives them here.
//
// A fixture that names a constant cannot tell a reader that FORWARDED it from
// one that SUBSTITUTED it, so the members a test asserts on that way carry a
// deliberately non-default value instead: testBdevConf's data block size here,
// and the extent size and both batch sizes in reactClusterConf (reaction_test).
//
// Each edit runs on the finished message, which is how a test names the one
// member it cares about, or clears one to build the invalid stored conf a
// refusal test needs.
func testClusterConf(edits ...func(*pb.ClusterConf)) *pb.ClusterConf {
	cc := &pb.ClusterConf{
		CreationEpoch: 1,
		BdevConf:      testBdevConf(),
		DnBinConf: &pb.DnBinConf{
			ExtentSize: common.DefaultDnExtSize,
			Bin0Shift:  common.DefaultDnBin0Shift,
			Bin1Shift:  common.DefaultDnBin1Shift,
			Bin2Shift:  common.DefaultDnBin2Shift,
			Bin3Shift:  common.DefaultDnBin3Shift,
		},
		AllocConf: &pb.AllocConf{
			DnBatchSize: common.DefaultAllocDnBatchSize,
			CnBatchSize: common.DefaultAllocCnBatchSize,
		},
		HealthCheckConf: &pb.HealthCheckConf{
			DnInterval:    common.DefaultHealthCheckInterval,
			CnInterval:    common.DefaultHealthCheckInterval,
			SideInterval:  common.DefaultHealthCheckInterval,
			CntlrInterval: common.DefaultHealthCheckInterval,
		},
	}
	for _, edit := range edits {
		edit(cc)
	}
	return cc
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testConfig is a configuration in the style of the worker suite
// (dnv-worker.md, Integration test plan): seconds-scale timers so a
// membership case runs in a few fake-clock steps.
func testConfig(roles ...string) Config {
	return Config{
		Roles:        roles,
		VoteInterval: 10 * time.Second,
		GraceTime:    60 * time.Second,
		Endpoints:    []string{"127.0.0.1:2379"},
	}
}

// newTestDeps wires a deps around the fakes. kinds defaults to a recording
// fake so no test accidentally dials an agent.
func newTestDeps(cfg Config, store *fakeStore, clk *fakeClock) *deps {
	d := &deps{
		cfg:    cfg,
		store:  store,
		conns:  newConnCache(),
		health: &fakeHealthWriter{},
		clk:    clk,
		kinds:  kindFor,
	}
	d.conf = newConfCache(d)
	return d
}

// resOk / resErr build the ResInfo rows the HL1/HL2 tables read.
func resOk(name string) *pb.ResInfo {
	return &pb.ResInfo{ResName: name, Status: pb.ResStatus_RES_STATUS_OK}
}

func resErr(name string, details string) *pb.ResInfo {
	return &pb.ResInfo{
		ResName: name,
		Status:  pb.ResStatus_RES_STATUS_ERROR,
		Details: details,
	}
}

func resStatus(name string, status pb.ResStatus) *pb.ResInfo {
	return &pb.ResInfo{ResName: name, Status: status}
}

// resLostId is the thin row of a converge's report whose build found the thin
// id gone from its slice's pool: RES_STATUS_ERROR with the reason
// RES_REASON_THIN_ID_LOST (cnagent.md CN14), which HL2's shared-state class
// reads (sharedStateTds), and details holding what the command printed.
func resLostId(name string, details string) *pb.ResInfo {
	res := resErr(name, details)
	res.Reason = pb.ResReason_RES_REASON_THIN_ID_LOST
	return res
}

// seedOf builds a deterministic, canonically shaped v4 seed for a test.
func seedOf(n int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n)
}
