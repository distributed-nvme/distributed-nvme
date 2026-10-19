package etcdutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// watchProgressInterval is the progress notification interval the package's
// etcd runs with (startEtcd).
const watchProgressInterval = time.Second

// quietWatchSilence is the silence threshold of the tests whose watch must
// stay open on progress notifications alone: above twice the notification
// interval with its tenth of jitter, since etcd skips the first notification
// after an event (EU3), and a margin above that.
const quietWatchSilence = 4 * watchProgressInterval

// newWatchClient returns a Client against the package's etcd whose watches
// end after silence with nothing heard.
func newWatchClient(t *testing.T, silence time.Duration) *Client {
	t.Helper()
	tmo := prodTimeouts()
	tmo.watchSilence = silence
	cli, err := newClient(context.Background(), []string{testEndpoint},
		time.Second, tmo)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// wantSilentEnd waits up to bound for the watch to end with the silence
// error, reported once, and for both channels to close (EU3). It returns
// when it saw the watch end.
func wantSilentEnd(
	t *testing.T,
	eventCh <-chan Event,
	errCh <-chan error,
	bound time.Duration,
) time.Time {
	t.Helper()
	var (
		at  time.Time
		err error
		ok  bool
	)
	select {
	case err, ok = <-errCh:
		at = time.Now()
	case event, open := <-eventCh:
		if open {
			t.Fatalf("the watch delivered %+v, want its silence error", event)
		}
		// The error is buffered before both channels close, so a reader
		// that comes late finds both ready and may see the closed event
		// channel first; the error then waits on its channel.
		at = time.Now()
		err, ok = <-errCh
	case <-time.After(bound):
		t.Fatalf("the watch did not end within %v", bound)
	}
	if !ok {
		t.Fatalf("the watch ended without an error")
	}
	if !errors.Is(err, errWatchSilent) {
		t.Fatalf("the watch ended with %v, want the silence error", err)
	}
	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatalf("a second error after the silence: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the error channel did not close")
	}
	select {
	case event, ok := <-eventCh:
		if ok {
			t.Fatalf("an event after the silence: %+v", event)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the event channel did not close")
	}
	return at
}

// TestWatchTypedEndsASilentWatch: a watch that hears nothing from etcd for
// its silence threshold ends with an error that says so (EU3). It starts past
// the store's revision and nothing is written under its prefix, so it hears
// neither an event nor, within the threshold, a progress notification.
func TestWatchTypedEndsASilentWatch(t *testing.T) {
	const silence = watchProgressInterval / 4
	cli := newWatchClient(t, silence)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefix := testPrefix(t)
	_, rev, err := cli.Range(ctx, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	start := time.Now()
	eventCh, errCh := cli.WatchTyped(ctx, prefix, rev+1, newWorkerReg)
	at := wantSilentEnd(t, eventCh, errCh, 10*time.Second)
	if at.Sub(start) < silence {
		t.Fatalf("the watch ended after %v, before its threshold of %v",
			at.Sub(start), silence)
	}
}

// TestWatchTypedProgressKeepsAQuietWatchOpen: etcd's progress notifications
// keep a watch that sees no further event open past its silence threshold
// (EU3). The package's etcd sends one every watchProgressInterval. The put
// after WatchTyped returns is what starts them: etcd sends no notification
// to a watch whose start revision lies past the store's revision until the
// store reaches it, and every dnv watch starts at its scan's revision plus
// one. The open runs in the watch's goroutine, so the put may reach etcd
// before it or after it; either way it brings the store to the watch's
// start revision, and the watch gets the put as its one event.
func TestWatchTypedProgressKeepsAQuietWatchOpen(t *testing.T) {
	cli := newWatchClient(t, quietWatchSilence)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefix := testPrefix(t)
	_, rev, err := cli.Range(ctx, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	eventCh, errCh := cli.WatchTyped(ctx, prefix, rev+1, newWorkerReg)
	key := prefix + "k"
	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if event := recvEvent(t, eventCh); event.Key != key {
		t.Fatalf("event %+v, want the put of %q", event, key)
	}
	// Nothing but notifications for two thresholds and more.
	quiet := 2*quietWatchSilence + watchProgressInterval
	select {
	case err, ok := <-errCh:
		t.Fatalf("the watch ended (open %v) with %v while etcd sent "+
			"progress notifications", ok, err)
	case event, ok := <-eventCh:
		t.Fatalf("an event nobody wrote: %+v (open %v)", event, ok)
	case <-time.After(quiet):
	}
}

// TestWatchTypedEndsAWatchThatFallsSilent: a watch that heard an event and
// then hears nothing more, because etcd stopped answering, ends with the
// silence error one threshold after the last thing it heard (EU3).
func TestWatchTypedEndsAWatchThatFallsSilent(t *testing.T) {
	cli := newWatchClient(t, quietWatchSilence)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefix := testPrefix(t)
	_, rev, err := cli.Range(ctx, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	eventCh, errCh := cli.WatchTyped(ctx, prefix, rev+1, newWorkerReg)
	key := prefix + "k"
	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if event := recvEvent(t, eventCh); event.Key != key {
		t.Fatalf("event %+v, want the put of %q", event, key)
	}
	stopTestEtcd(t)
	stopped := time.Now()
	at := wantSilentEnd(t, eventCh, errCh, quietWatchSilence+10*time.Second)
	// The last thing heard is the event, or a notification after the event
	// and before the stop.
	if at.Sub(stopped) > quietWatchSilence+2*time.Second {
		t.Fatalf("the watch ended %v after etcd stopped, want about one "+
			"threshold of %v", at.Sub(stopped), quietWatchSilence)
	}
}

// TestWatchTypedEndsAnOpenThatHangs: a watch opened while etcd answers
// nothing ends with the silence error at its threshold, and WatchTyped itself
// returns at once (EU3). The client is new, so its first connection waits for
// etcd's side of the handshake, and the open waits for that connection.
func TestWatchTypedEndsAnOpenThatHangs(t *testing.T) {
	const silence = 3 * time.Second
	stopTestEtcd(t)
	cli := newWatchClient(t, silence)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefix := testPrefix(t)
	type watch struct {
		eventCh <-chan Event
		errCh   <-chan error
	}
	opened := make(chan watch, 1)
	start := time.Now()
	go func() {
		eventCh, errCh := cli.WatchTyped(ctx, prefix, 1, newWorkerReg)
		opened <- watch{eventCh, errCh}
	}()
	var w watch
	select {
	case w = <-opened:
	case <-time.After(silence / 3):
		t.Fatalf("WatchTyped blocked on an etcd that answers nothing")
	}
	at := wantSilentEnd(t, w.eventCh, w.errCh, silence+10*time.Second)
	if at.Sub(start) < silence {
		t.Fatalf("the watch ended after %v, before its threshold of %v",
			at.Sub(start), silence)
	}
}
