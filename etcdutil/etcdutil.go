// Package etcdutil is the one and only door to etcd in dnv (dnv-worker.md §3,
// log.md §5.3): it wraps a single clientv3 client, does the protobuf
// (un)marshaling of every value and emits the log records log.md requires, so
// that no other package ever calls clientv3 (or its STM) directly with ad-hoc
// marshaling. It takes proto.Message parameters and MUST NOT import pb
// (layout.md §3).
package etcdutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The log.md §5.3 record names. They are normative: the integration suite
// greps for them (dnv-worker.md §12).
const (
	msgGet        = "etcd get"
	msgPut        = "etcd put"
	msgDelete     = "etcd delete"
	msgRange      = "etcd range"
	msgWatchEvent = "etcd watch event"
)

// ---------------------------------------------------------------------------
// Client (EU1)
// ---------------------------------------------------------------------------

// Client wraps one clientv3 client. All dnv etcd access goes through its
// methods (EU1); a process creates one and shares it.
type Client struct {
	cli *clientv3.Client
}

// New builds the client (EU1). No dnv gRPC interceptor is attached: etcd
// logging is done by these helpers, not by message interceptors (grpc.md §1).
// dialTimeout defaults to common.DefaultEtcdDialTimeout seconds when it is not
// positive (EU5). Not reaching any endpoint here is not by itself fatal to the
// caller: clientv3 dials lazily and reconnects in the background, and the
// worker's vote layer stays fenced until its first successful put (VW8).
//
// ctx bounds the construction only. It is deliberately not installed as the
// client's own lifetime context, so that a short-lived startup ctx cannot
// silently kill a long-lived client; Close releases it instead.
func New(
	ctx context.Context,
	endpoints []string,
	dialTimeout time.Duration,
) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("etcd client: %w", err)
	}
	if dialTimeout <= 0 {
		dialTimeout = common.DefaultEtcdDialTimeout * time.Second
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
		// Silence the etcd client's own zap logger: stdout carries one JSON
		// record per line (log.md R2) and the integration suite parses it.
		Logger: zap.NewNop(),
	})
	if err != nil {
		return nil, fmt.Errorf("etcd client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Close releases the underlying client (EU1).
func (c *Client) Close() error {
	if err := c.cli.Close(); err != nil {
		return fmt.Errorf("etcd close: %w", err)
	}
	return nil
}

// opCtx bounds one etcd operation by common.DefaultEtcdOpTimeout seconds
// (EU5). A shorter caller ctx wins, because context.WithTimeout keeps the
// earlier of the two deadlines.
func opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, common.DefaultEtcdOpTimeout*time.Second)
}

// appendErr appends the optional "error" attribute of the log.md §5.3 records
// (the same shape common/osclient.go uses).
func appendErr(attrs []any, err error) []any {
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	return attrs
}

// logGet emits an "etcd get" record (log.md §5.3). value is logged decoded,
// never as raw bytes, and only when the key was found and could be decoded.
func logGet(
	ctx context.Context,
	key string,
	found bool,
	msg proto.Message,
	err error,
) {
	attrs := []any{
		slog.String("key", key),
		slog.Bool("found", found),
	}
	if found && msg != nil {
		attrs = append(attrs, slog.Any("value", common.PbToLogValue(msg)))
	}
	slog.InfoContext(ctx, msgGet, appendErr(attrs, err)...)
}

// logPut emits an "etcd put" record (log.md §5.3).
func logPut(ctx context.Context, key string, msg proto.Message, err error) {
	attrs := []any{
		slog.String("key", key),
		slog.Any("value", common.PbToLogValue(msg)),
	}
	slog.InfoContext(ctx, msgPut, appendErr(attrs, err)...)
}

// logDelete emits an "etcd delete" record (log.md §5.3).
func logDelete(ctx context.Context, key string, err error) {
	attrs := []any{slog.String("key", key)}
	slog.InfoContext(ctx, msgDelete, appendErr(attrs, err)...)
}

// ---------------------------------------------------------------------------
// Typed plain operations (EU2)
// ---------------------------------------------------------------------------

// KV is one scanned key and its still-encoded value (EU2). Decode turns the
// value into a message and logs it.
type KV struct {
	Key   string
	Value []byte
}

// KeyRev is one scanned key and its mod_revision, which BM5 memoizes (EU2).
type KeyRev struct {
	Key    string
	ModRev int64
}

// Get performs a point read (EU2). msg is left untouched when the key does not
// exist; the returned bool says whether it did. Logs "etcd get".
func (c *Client) Get(
	ctx context.Context,
	key string,
	msg proto.Message,
) (bool, error) {
	octx, cancel := opCtx(ctx)
	defer cancel()
	resp, err := c.cli.Get(octx, key)
	if err != nil {
		logGet(ctx, key, false, nil, err)
		return false, fmt.Errorf("etcd get %s: %w", key, err)
	}
	if len(resp.Kvs) == 0 {
		logGet(ctx, key, false, nil, nil)
		return false, nil
	}
	if err := proto.Unmarshal(resp.Kvs[0].Value, msg); err != nil {
		// The key exists, so found stays true; there is no decoded value to
		// log, and the error attribute carries the reason (EU6).
		logGet(ctx, key, true, nil, err)
		return false, fmt.Errorf("etcd get %s: unmarshal: %w", key, err)
	}
	logGet(ctx, key, true, msg, nil)
	return true, nil
}

// Put writes one message (EU2). Logs "etcd put".
func (c *Client) Put(ctx context.Context, key string, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		logPut(ctx, key, msg, err)
		return fmt.Errorf("etcd put %s: marshal: %w", key, err)
	}
	octx, cancel := opCtx(ctx)
	defer cancel()
	_, err = c.cli.Put(octx, key, string(data))
	logPut(ctx, key, msg, err)
	if err != nil {
		return fmt.Errorf("etcd put %s: %w", key, err)
	}
	return nil
}

// Delete removes one key (EU2). Deleting an absent key is not an error. Logs
// "etcd delete".
func (c *Client) Delete(ctx context.Context, key string) error {
	octx, cancel := opCtx(ctx)
	defer cancel()
	_, err := c.cli.Delete(octx, key)
	logDelete(ctx, key, err)
	if err != nil {
		return fmt.Errorf("etcd delete %s: %w", key, err)
	}
	return nil
}

// scan runs one prefix range and logs the single "etcd range" record of EU2:
// prefix and count only — a range never dumps its values (log.md §5.3).
func (c *Client) scan(
	ctx context.Context,
	prefix string,
	opts ...clientv3.OpOption,
) (*clientv3.GetResponse, error) {
	octx, cancel := opCtx(ctx)
	defer cancel()
	allOpts := make([]clientv3.OpOption, 0, len(opts)+1)
	allOpts = append(allOpts, clientv3.WithPrefix())
	allOpts = append(allOpts, opts...)
	resp, err := c.cli.Get(octx, prefix, allOpts...)
	count := 0
	if resp != nil {
		count = len(resp.Kvs)
	}
	attrs := []any{
		slog.String("prefix", prefix),
		slog.Int("count", count),
	}
	slog.InfoContext(ctx, msgRange, appendErr(attrs, err)...)
	if err != nil {
		return nil, fmt.Errorf("etcd range %s: %w", prefix, err)
	}
	return resp, nil
}

// Range scans a prefix in ascending key order (EU2). The returned rev is the
// store revision the scan was served at (resp.Header.Revision), which a caller
// then uses as the starting revision of a watch. Values stay encoded: pass
// each KV to Decode, which logs the value it decodes.
func (c *Client) Range(
	ctx context.Context,
	prefix string,
) ([]KV, int64, error) {
	resp, err := c.scan(
		ctx, prefix,
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return nil, 0, err
	}
	kvs := make([]KV, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		kvs = append(kvs, KV{Key: string(kv.Key), Value: kv.Value})
	}
	return kvs, resp.Header.Revision, nil
}

// RangeDesc scans a prefix in DESCENDING key order, at most limit keys
// (limit <= 0 means no limit). It exists for the §6.3/§6.4 allocator
// (model/alloc.go, MD5): the capacity keys embed free_ext_cnt in
// common.FreeSpaceFmt, so descending key order is "largest free first", and
// the allocator stops as soon as it has enough candidates — which is what the
// bounded scan expresses without pulling a whole bin into memory. Same "etcd
// range" record as Range.
func (c *Client) RangeDesc(
	ctx context.Context,
	prefix string,
	limit int64,
) ([]KV, int64, error) {
	opts := []clientv3.OpOption{
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortDescend),
	}
	if limit > 0 {
		opts = append(opts, clientv3.WithLimit(limit))
	}
	resp, err := c.scan(ctx, prefix, opts...)
	if err != nil {
		return nil, 0, err
	}
	kvs := make([]KV, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		kvs = append(kvs, KV{Key: string(kv.Key), Value: kv.Value})
	}
	return kvs, resp.Header.Revision, nil
}

// RangeKeys scans a prefix keys-only, in ascending key order (EU2). Each entry
// carries the key's mod_revision, which BM5 memoizes to detect a changed
// bitmap without reading it.
func (c *Client) RangeKeys(
	ctx context.Context,
	prefix string,
) ([]KeyRev, int64, error) {
	resp, err := c.scan(
		ctx, prefix,
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
		clientv3.WithKeysOnly(),
	)
	if err != nil {
		return nil, 0, err
	}
	keys := make([]KeyRev, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		keys = append(keys, KeyRev{
			Key:    string(kv.Key),
			ModRev: kv.ModRevision,
		})
	}
	return keys, resp.Header.Revision, nil
}

// Decode unmarshals one scanned value (EU2) and logs it as an "etcd get"
// record with found = true, so that every value a caller actually reads is
// logged individually even though the range that produced it logged only a
// count (log.md §5.3).
func (c *Client) Decode(ctx context.Context, kv KV, msg proto.Message) error {
	if err := proto.Unmarshal(kv.Value, msg); err != nil {
		logGet(ctx, kv.Key, true, nil, err)
		return fmt.Errorf("etcd decode %s: %w", kv.Key, err)
	}
	logGet(ctx, kv.Key, true, msg, nil)
	return nil
}

// ---------------------------------------------------------------------------
// Typed watch (EU3)
// ---------------------------------------------------------------------------

// EventType is the kind of a watch event (EU3).
type EventType int

const (
	// EventPut is a key created or overwritten; the event carries Msg.
	EventPut EventType = iota
	// EventDelete is a key removed; the event carries no Msg.
	EventDelete
)

// String renders the type as the log.md §5.3 "type" attribute.
func (t EventType) String() string {
	switch t {
	case EventPut:
		return "put"
	case EventDelete:
		return "delete"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// Event is one typed watch event (EU3). Msg is set for puts only; Rev is the
// key's mod_revision, so a caller resumes a watch at Rev + 1.
type Event struct {
	Type EventType
	Key  string
	Msg  proto.Message
	Rev  int64
}

// IsCompacted reports whether a WatchTyped error is the server's ErrCompacted
// cancellation (EU3). The caller must then rescan with Range and restart the
// watch at the revision the scan was served at (SW4, VW3) instead of resuming
// at the revision it had. It exists so that callers need not import
// go.etcd.io/etcd/api/v3/v3rpc/rpctypes themselves.
func IsCompacted(err error) bool {
	return errors.Is(err, rpctypes.ErrCompacted)
}

// WatchTyped opens one prefix watch starting at fromRev (EU3). Every event is
// delivered in order on the event channel, decoded with newMsg() for puts, and
// logged as "etcd watch event". The etcd client reconnects transparently; a
// watch the server cancels — ErrCompacted above all, see IsCompacted — is
// reported once on the error channel, after which BOTH channels close. When
// ctx ends both channels close without an error. There is no untyped watch:
// every dnv prefix holds exactly one message type.
func (c *Client) WatchTyped(
	ctx context.Context,
	prefix string,
	fromRev int64,
	newMsg func() proto.Message,
) (<-chan Event, <-chan error) {
	eventCh := make(chan Event)
	// Buffered: the single error is reported even if nobody is receiving yet.
	errCh := make(chan error, 1)
	// The watch gets its own cancellable ctx so that EVERY return path of the
	// goroutine below — a server cancellation, a decode failure, the caller's
	// ctx ending — tears the clientv3 substream down. Without it a watch that
	// ends on anything other than ctx would leak its substream for the life
	// of the client, and a worker that rescans after each ErrCompacted (SW4,
	// VW3) would accumulate one per rescan.
	wctx, cancel := context.WithCancel(ctx)
	watchCh := c.cli.Watch(
		wctx, prefix,
		clientv3.WithPrefix(),
		clientv3.WithRev(fromRev),
	)
	go func() {
		defer cancel()
		defer close(errCh)
		defer close(eventCh)
		for {
			select {
			case <-ctx.Done():
				return
			case resp, ok := <-watchCh:
				if !ok {
					// The client closed the watch: ctx ended, or Close was
					// called. No error to report (EU3).
					return
				}
				if err := resp.Err(); err != nil {
					errCh <- fmt.Errorf("etcd watch %s: %w", prefix, err)
					return
				}
				for _, rawEvent := range resp.Events {
					event, err := decodeEvent(ctx, rawEvent, newMsg)
					if err != nil {
						errCh <- err
						return
					}
					select {
					case eventCh <- event:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return eventCh, errCh
}

// decodeEvent turns one clientv3 event into an Event and logs the
// "etcd watch event" record of log.md §5.3.
func decodeEvent(
	ctx context.Context,
	rawEvent *clientv3.Event,
	newMsg func() proto.Message,
) (Event, error) {
	key := string(rawEvent.Kv.Key)
	event := Event{
		Key: key,
		Rev: rawEvent.Kv.ModRevision,
	}
	attrs := []any{slog.String("key", key)}
	if rawEvent.Type == clientv3.EventTypeDelete {
		event.Type = EventDelete
		attrs = append(attrs, slog.String("type", EventDelete.String()))
		slog.InfoContext(ctx, msgWatchEvent, attrs...)
		return event, nil
	}
	event.Type = EventPut
	attrs = append(attrs, slog.String("type", EventPut.String()))
	msg := newMsg()
	if err := proto.Unmarshal(rawEvent.Kv.Value, msg); err != nil {
		slog.InfoContext(ctx, msgWatchEvent, appendErr(attrs, err)...)
		return Event{}, fmt.Errorf("etcd watch event %s: unmarshal: %w", key, err)
	}
	event.Msg = msg
	attrs = append(attrs, slog.Any("value", common.PbToLogValue(msg)))
	slog.InfoContext(ctx, msgWatchEvent, attrs...)
	return event, nil
}

// ---------------------------------------------------------------------------
// STM (EU4)
// ---------------------------------------------------------------------------

// ErrNoCommit is the abort sentinel of RunSTM (EU4). A callback error that
// wraps it — errors.Is(err, ErrNoCommit) — aborts the transaction WITHOUT
// commit and WITHOUT retry, and RunSTM returns that error unchanged, so the
// caller can type-assert it.
//
// This is the contract model builds on: model.ErrPrecondition (MD7) is a
// normal error type whose Unwrap() returns ErrNoCommit, which keeps
// etcdutil free of any model import (layout.md §3). Note that the abort is
// not special-cased in the transaction machinery — ANY error returned by the
// callback ends the attempt before the commit txn is issued; ErrNoCommit only
// tells etcdutil (and the reader) that the error is a deliberate, non-fatal
// abort rather than a failure, and that it must reach the caller as it is.
var ErrNoCommit = errors.New("etcdutil: abort without commit")

// errSnapshotDone ends a Snapshot's single attempt before its commit. It never
// escapes Snapshot.
var errSnapshotDone = errors.New("etcdutil: snapshot complete")

// errReadOnly is the misuse a Snapshot callback can commit.
var errReadOnly = errors.New("write inside a read-only Snapshot")

// STM is the typed view of one transaction (EU4). Get/Put/Del each log their
// log.md §5.3 record; a retried transaction logs its records once per attempt,
// which log.md accepts.
type STM interface {
	// Get reads one key into msg and reports whether it exists. msg is left
	// untouched when it does not.
	Get(key string, msg proto.Message) bool
	// Put stages a write.
	Put(key string, msg proto.Message)
	// Del stages a delete.
	Del(key string)
	// Rev is the key's mod_revision in this transaction's read set, 0 when
	// the key does not exist.
	Rev(key string) int64
}

// stmPending is one staged write of the current attempt, kept so that reading
// back a key written in the same transaction is exact: a message with no
// populated field marshals to zero bytes, which the underlying STM cannot
// distinguish from a missing key.
type stmPending struct {
	data    []byte
	deleted bool
}

// stmView implements STM over one concurrency.STM attempt.
type stmView struct {
	ctx      context.Context
	stm      concurrency.STM
	readOnly bool
	pending  map[string]stmPending
	// err is the first (de)serialization or misuse error of this attempt; it
	// aborts the transaction, since the typed methods cannot return one.
	err error
}

// fail records the first error of the attempt (EU6).
func (v *stmView) fail(err error) {
	if v.err == nil {
		v.err = err
	}
}

// lookup returns the still-encoded value of a key and whether it exists,
// preferring this attempt's staged writes.
func (v *stmView) lookup(key string) ([]byte, bool) {
	if pending, ok := v.pending[key]; ok {
		if pending.deleted {
			return nil, false
		}
		return pending.data, true
	}
	value := v.stm.Get(key)
	if value != "" {
		return []byte(value), true
	}
	// An empty value and a missing key look identical through
	// concurrency.STM.Get, and an all-default message marshals to zero
	// bytes, so the mod revision decides. Rev costs no extra round trip:
	// the key is already in the attempt's read set.
	if v.stm.Rev(key) == 0 {
		return nil, false
	}
	return nil, true
}

// Get reads one key (EU4), logging "etcd get".
func (v *stmView) Get(key string, msg proto.Message) bool {
	data, found := v.lookup(key)
	if !found {
		logGet(v.ctx, key, false, nil, nil)
		return false
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		logGet(v.ctx, key, true, nil, err)
		v.fail(fmt.Errorf("etcd stm get %s: unmarshal: %w", key, err))
		return false
	}
	logGet(v.ctx, key, true, msg, nil)
	return true
}

// Put stages a write (EU4), logging "etcd put".
func (v *stmView) Put(key string, msg proto.Message) {
	if v.readOnly {
		err := fmt.Errorf("etcd stm put %s: %w", key, errReadOnly)
		logPut(v.ctx, key, msg, err)
		v.fail(err)
		return
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		logPut(v.ctx, key, msg, err)
		v.fail(fmt.Errorf("etcd stm put %s: marshal: %w", key, err))
		return
	}
	v.stm.Put(key, string(data))
	v.pending[key] = stmPending{data: data}
	logPut(v.ctx, key, msg, nil)
}

// Del stages a delete (EU4), logging "etcd delete".
func (v *stmView) Del(key string) {
	if v.readOnly {
		err := fmt.Errorf("etcd stm delete %s: %w", key, errReadOnly)
		logDelete(v.ctx, key, err)
		v.fail(err)
		return
	}
	v.stm.Del(key)
	v.pending[key] = stmPending{deleted: true}
	logDelete(v.ctx, key, nil)
}

// Rev returns the key's mod_revision, 0 when it does not exist. It emits no
// log record: it reads no value, and log.md §5.3's "etcd get" record is
// defined by the decoded value it carries.
func (v *stmView) Rev(key string) int64 {
	return v.stm.Rev(key)
}

// run is the shared body of RunSTM and Snapshot. readOnly makes the attempt
// end before its commit (see Snapshot).
//
// DELIBERATE DEVIATION from EU5's literal wording, documented here because it
// is the one place code and spec disagree. EU5 budgets each STM *attempt* with
// common.DefaultEtcdOpTimeout and EU4 lets the conflict retry run until the
// caller's ctx ends. concurrency.NewSTM owns its retry loop and fixes its abort
// ctx at construction, so a per-attempt deadline cannot be expressed through
// it; the budget here bounds the WHOLE transaction, every retry included.
//
// The alternative — re-running NewSTM under a fresh budget whenever the old one
// expired — was tried and rejected: a budget expiry caused by contention is
// indistinguishable from one caused by an unreachable etcd, so that loop never
// terminates while etcd is down and wedges the calling worker goroutine, which
// is strictly worse than the behaviour below. Reimplementing serializable-
// snapshot conflict detection to get a true per-attempt bound is not worth it.
//
// What the deviation costs: under sustained write contention on one key a
// transaction can exhaust the 10 s across its attempts and return a deadline
// error while the caller's ctx is still alive. That is benign here — every
// caller is a worker whose retry cadence is its own round (RW12), and every
// mutation re-validates its preconditions on the next pass (MD7, AR2).
func (c *Client) run(
	ctx context.Context,
	f func(s STM) error,
	readOnly bool,
) error {
	octx, cancel := opCtx(ctx)
	defer cancel()
	// callbackErr is rewritten by every attempt; after NewSTM returns it
	// holds the error of the last attempt, which is what NewSTM returns
	// too when the callback is what ended the transaction.
	var callbackErr error
	_, err := concurrency.NewSTM(
		c.cli,
		func(stm concurrency.STM) error {
			// Every attempt starts clean: a panic-driven abort inside the
			// STM machinery must not be mistaken for a previous attempt's
			// callback error below.
			callbackErr = nil
			view := &stmView{
				ctx:      ctx,
				stm:      stm,
				readOnly: readOnly,
				pending:  make(map[string]stmPending),
			}
			callbackErr = f(view)
			if view.err != nil {
				// A (de)serialization error is the root cause; it wins over
				// whatever the callback made of the bad value.
				callbackErr = view.err
			}
			if callbackErr != nil {
				return callbackErr
			}
			if readOnly {
				return errSnapshotDone
			}
			return nil
		},
		concurrency.WithAbortContext(octx),
		concurrency.WithIsolation(concurrency.SerializableSnapshot),
	)
	if readOnly && errors.Is(err, errSnapshotDone) {
		return nil
	}
	if err != nil {
		// The callback's own error is returned unchanged (EU4), so that
		// errors.Is/As on it — model.ErrPrecondition above all — keeps
		// working at the call site.
		if callbackErr != nil && err == callbackErr {
			return err
		}
		return fmt.Errorf("etcd stm: %w", err)
	}
	return nil
}

// RunSTM runs f as one read/write transaction (EU4) with serializable-snapshot
// isolation. The etcd client re-runs f on a write conflict until ctx or the
// common.DefaultEtcdOpTimeout budget of the whole transaction ends (EU5), so f
// MUST be a pure function of what it reads through s. An error returned by f
// ends the transaction before its commit and without a retry, and reaches the
// caller unchanged; ErrNoCommit is the sentinel a deliberate abort wraps.
// Every other error — connection, timeout, (de)serialization — is wrapped and
// never retried here (EU6).
func (c *Client) RunSTM(ctx context.Context, f func(s STM) error) error {
	return c.run(ctx, f, false)
}

// Snapshot runs f as one read-only transaction (EU4) in which every read is
// served at ONE store revision — the revision of the first read — which is
// what makes a multi-key load consistent (MD3, AR1).
//
// How the commit is made a no-op: the attempt ends by returning the internal
// errSnapshotDone sentinel out of the concurrency callback, which stops
// concurrency.NewSTM before it issues its commit txn, and Snapshot then
// translates that sentinel back to nil. Compared with letting an empty write
// set commit, this saves the commit round trip and, more importantly, removes
// the conflict retry: a snapshot whose read set was concurrently rewritten
// must still return the revision it read, not re-run f. s.Put/s.Del inside f
// are a programming error and make Snapshot fail without writing anything.
func (c *Client) Snapshot(ctx context.Context, f func(s STM) error) error {
	return c.run(ctx, f, true)
}

// RangeKeysAtRev is RangeKeys pinned to one store revision (EU2). It exists
// for model.LoadSp (MD3): the bitmap indexes of an SP must be scanned at the
// same revision the SP snapshot was read at, and an STM cannot range. rev must
// be a revision the store has not compacted away; a compacted rev is returned
// as an error, never silently served from a newer one. Same "etcd range"
// record as RangeKeys.
func (c *Client) RangeKeysAtRev(
	ctx context.Context,
	prefix string,
	rev int64,
) ([]KeyRev, error) {
	resp, err := c.scan(
		ctx, prefix,
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
		clientv3.WithKeysOnly(),
		clientv3.WithRev(rev),
	)
	if err != nil {
		return nil, err
	}
	keys := make([]KeyRev, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		keys = append(keys, KeyRev{
			Key:    string(kv.Key),
			ModRev: kv.ModRevision,
		})
	}
	return keys, nil
}

// snapshotView implements STM as a set of reads all served at ONE store
// revision, and — unlike concurrency.STM — reports that revision.
//
// Why it is not built on concurrency.NewSTM like Snapshot (EU4): the
// serializable-snapshot isolation pins its base revision at the first read but
// keeps it private, and MD3 needs exactly that number to run its keys-only
// bitmap scans (RangeKeysAtRev) at the very revision the snapshot was read at.
// The mechanism here is the same one concurrency.STM uses internally — the
// first read is linearizable and fixes the revision, every later read adds
// clientv3.WithRev — minus the commit txn a read-only transaction has no use
// for.
type snapshotView struct {
	ctx context.Context
	c   *Client
	// rev is the pinned store revision, 0 until the first read fixes it.
	rev int64
	// err is the first failure or misuse of the snapshot; the typed methods
	// cannot return one, so it ends the load instead.
	err error
}

// fail records the first error of the snapshot (EU6).
func (v *snapshotView) fail(err error) {
	if v.err == nil {
		v.err = err
	}
}

// fetch reads one key at the pinned revision, fixing that revision on the
// first call.
func (v *snapshotView) fetch(key string) (*clientv3.GetResponse, error) {
	if v.err != nil {
		return nil, v.err
	}
	var opts []clientv3.OpOption
	if v.rev > 0 {
		// WithRev alone, deliberately NOT paired with WithSerializable: a
		// serializable read is answered from whichever member the balancer
		// picks, out of its local store, so a member that has not yet applied
		// v.rev answers ErrFutureRev and fails the whole load. A linearizable
		// read at an explicit revision waits for the local applied index to
		// reach the read index first, which cannot observe v.rev as a future
		// revision. The snapshot semantics MD3 asks for come from WithRev.
		opts = append(opts, clientv3.WithRev(v.rev))
	}
	resp, err := v.c.cli.Get(v.ctx, key, opts...)
	if err != nil {
		return nil, fmt.Errorf("etcd snapshot get %s: %w", key, err)
	}
	if v.rev == 0 {
		v.rev = resp.Header.Revision
	}
	return resp, nil
}

// Get reads one key at the snapshot revision (EU4), logging "etcd get".
func (v *snapshotView) Get(key string, msg proto.Message) bool {
	resp, err := v.fetch(key)
	if err != nil {
		logGet(v.ctx, key, false, nil, err)
		v.fail(err)
		return false
	}
	if len(resp.Kvs) == 0 {
		logGet(v.ctx, key, false, nil, nil)
		return false
	}
	if err := proto.Unmarshal(resp.Kvs[0].Value, msg); err != nil {
		logGet(v.ctx, key, true, nil, err)
		v.fail(fmt.Errorf("etcd snapshot get %s: unmarshal: %w", key, err))
		return false
	}
	logGet(v.ctx, key, true, msg, nil)
	return true
}

// Put is a programming error inside a snapshot (EU4).
func (v *snapshotView) Put(key string, msg proto.Message) {
	err := fmt.Errorf("etcd snapshot put %s: %w", key, errReadOnly)
	logPut(v.ctx, key, msg, err)
	v.fail(err)
}

// Del is a programming error inside a snapshot (EU4).
func (v *snapshotView) Del(key string) {
	err := fmt.Errorf("etcd snapshot delete %s: %w", key, errReadOnly)
	logDelete(v.ctx, key, err)
	v.fail(err)
}

// Rev returns the key's mod_revision at the snapshot revision, 0 when the key
// does not exist there. Like stmView.Rev it emits no record: it reads no
// value.
func (v *snapshotView) Rev(key string) int64 {
	resp, err := v.fetch(key)
	if err != nil {
		v.fail(err)
		return 0
	}
	if len(resp.Kvs) == 0 {
		return 0
	}
	return resp.Kvs[0].ModRevision
}

// SnapshotRev is Snapshot plus the store revision every read was served at
// (EU4). It is the multi-key load MD3 builds on: the returned revision is what
// RangeKeysAtRev must be given so that a scan the STM cannot perform still
// sees the same store state as the snapshot.
//
// f runs exactly once — a read-only load has no write set and therefore
// nothing to conflict on — so, unlike RunSTM's callback, it need not be
// re-runnable. An error returned by f reaches the caller unchanged, so
// errors.Is/As on it keeps working; every other error is wrapped (EU6). The
// revision is reported even when f fails, and is 0 when f read nothing.
// s.Put/s.Del inside f are a programming error and fail the load.
func (c *Client) SnapshotRev(
	ctx context.Context,
	f func(s STM) error,
) (int64, error) {
	// One budget for the whole load, not one per key read (EU5): a snapshot is
	// a single logical operation, and a per-read deadline would let a slow
	// etcd stretch a many-key LoadSp without bound. A shorter caller ctx still
	// wins, since WithTimeout keeps the earlier deadline.
	octx, cancel := opCtx(ctx)
	defer cancel()
	view := &snapshotView{ctx: octx, c: c}
	callbackErr := f(view)
	if view.err != nil {
		// A read or misuse failure is the root cause; it wins over whatever
		// the callback made of the incomplete state.
		return view.rev, fmt.Errorf("etcd snapshot: %w", view.err)
	}
	if callbackErr != nil {
		return view.rev, callbackErr
	}
	return view.rev, nil
}
