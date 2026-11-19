package csi

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeUevents is a scripted event socket in place of the kernel's (csi.md
// CSI15): each receive takes the next item pushed, or answers EAGAIN after
// a short wait, as the kernel's socket does at its receive timeout.
type fakeUevents struct {
	items  chan fakeUevent
	closed atomic.Bool
}

type fakeUevent struct {
	msg string
	err error
}

func newFakeUevents() *fakeUevents {
	return &fakeUevents{items: make(chan fakeUevent, 1024)}
}

func (c *fakeUevents) recv(buf []byte) (int, error) {
	select {
	case it := <-c.items:
		if it.err != nil {
			return 0, it.err
		}
		return copy(buf, it.msg), nil
	case <-time.After(20 * time.Millisecond):
		return 0, syscall.EAGAIN
	}
}

func (c *fakeUevents) close() error {
	c.closed.Store(true)
	return nil
}

func (c *fakeUevents) push(it fakeUevent) {
	select {
	case c.items <- it:
	default:
	}
}

// uevent renders one kernel event of a controller as the netlink socket
// carries it: the header, then NUL-separated pairs.
func uevent(action string, subsystem string, extra ...string) fakeUevent {
	dev := "/devices/virtual/nvme-fabrics/ctl/nvme3"
	fields := append([]string{action + "@" + dev, "ACTION=" + action,
		"DEVPATH=" + dev, "SUBSYSTEM=" + subsystem, "DEVNAME=nvme3", "SEQNUM=4711"},
		extra...)
	return fakeUevent{msg: strings.Join(fields, "\x00") + "\x00"}
}

// stubUevents stands the scripted sockets in for the kernel's, one per open
// in order, and counts the opens; an open past the last fails. The caller
// stops every loop that opens before the test ends, so the restore races
// none.
func stubUevents(t *testing.T, conns ...*fakeUevents) *atomic.Int32 {
	t.Helper()
	var opens atomic.Int32
	old := openUevents
	t.Cleanup(func() { openUevents = old })
	openUevents = func() (ueventConn, error) {
		i := int(opens.Add(1)) - 1
		if i < len(conns) {
			return conns[i], nil
		}
		return nil, syscall.EMFILE
	}
	return &opens
}

// setRetryBase shortens the follower's retry delay for one test.
func setRetryBase(t *testing.T, d time.Duration) {
	t.Helper()
	old := retryBase
	t.Cleanup(func() { retryBase = old })
	retryBase = d
}

// listen runs listenUevents until the returned stop, which waits for it to
// return and fails the test unless it does within the bound.
func listen(t *testing.T, conn ueventConn, events chan<- struct{}) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		listenUevents(ctx, conn, events, time.Hour)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2*ueventRecvTimeout + time.Second):
				t.Errorf("the event listener did not return after its end")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitSignals waits until n signals came, or fails the test.
func waitSignals(t *testing.T, events <-chan struct{}, n int, what string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-events:
		case <-deadline:
			t.Fatalf("%s: %d signals of %d came", what, i, n)
		}
	}
}

// TestUeventOverflowStartsAPass pins csi.md CSI15: a receive overflow means
// events were lost, so it signals a pass, and the listener goes on reading
// the same socket, whose next event signals too.
func TestUeventOverflowStartsAPass(t *testing.T) {
	setRetryBase(t, 10*time.Millisecond)
	conn := newFakeUevents()
	opens := stubUevents(t)
	conn.push(fakeUevent{err: syscall.ENOBUFS})
	conn.push(uevent("change", "nvme", "NVME_AEN=0x70f002"))
	events := make(chan struct{}, 8)
	stop := listen(t, conn, events)
	waitSignals(t, events, 2, "an overflow, then a change event")
	stop()
	if n := opens.Load(); n != 0 {
		t.Errorf("the socket was opened again %d times after an overflow", n)
	}
	if !conn.closed.Load() {
		t.Error("the listener left its socket open at its end")
	}
}

// TestUeventSocketReopensAfterAnError pins csi.md CSI15: any other failure
// of the socket signals a pass, closes the socket and opens a new one after
// a pause, which signals a pass for the events lost meanwhile, and whose
// events signal in turn.
func TestUeventSocketReopensAfterAnError(t *testing.T) {
	setRetryBase(t, 10*time.Millisecond)
	first, second := newFakeUevents(), newFakeUevents()
	opens := stubUevents(t, second)
	first.push(fakeUevent{err: syscall.EIO})
	second.push(uevent("change", "nvme", "NVME_EVENT=connected"))
	events := make(chan struct{}, 8)
	stop := listen(t, first, events)
	waitSignals(t, events, 3, "a failed receive, the reopen, then the new socket's event")
	stop()
	if n := opens.Load(); n != 1 || !first.closed.Load() || !second.closed.Load() {
		t.Errorf("opens %d, first closed %v, second closed %v: want one "+
			"reopen and both closed", n, first.closed.Load(), second.closed.Load())
	}
}

// TestUeventSocketOpensAgainAfterAFailedOpen pins the listener's start
// without a socket: an open that fails is tried again after a pause, and
// the socket it opens at last signals a pass.
func TestUeventSocketOpensAgainAfterAFailedOpen(t *testing.T) {
	setRetryBase(t, 10*time.Millisecond)
	conn := newFakeUevents()
	conn.push(uevent("change", "nvme", "NVME_EVENT=rediscover"))
	var opens atomic.Int32
	old := openUevents
	t.Cleanup(func() { openUevents = old })
	openUevents = func() (ueventConn, error) {
		if opens.Add(1) < 3 {
			return nil, syscall.ENOMEM
		}
		return conn, nil
	}
	events := make(chan struct{}, 8)
	stop := listen(t, nil, events)
	waitSignals(t, events, 2, "the open at the third try, then its socket's event")
	stop()
	if n := opens.Load(); n != 3 {
		t.Errorf("%d opens, want 3", n)
	}
}

// TestUeventListenerStopsAtTheEnd runs the listener on the kernel's own
// socket: it returns within the receive timeout of its end, since a close
// from another goroutine would not wake a blocked receive. A machine whose
// tests cannot open the socket skips it.
func TestUeventListenerStopsAtTheEnd(t *testing.T) {
	conn, err := openNetlinkUevents()
	if err != nil {
		t.Skipf("the kernel's event socket: %v", err)
	}
	events := make(chan struct{}, 8)
	stop := listen(t, conn, events)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	stop()
	if d := time.Since(start); d > ueventRecvTimeout+500*time.Millisecond {
		t.Errorf("the listener returned %s after its end", d)
	}
}

// TestIsNvmeEvent pins the events the follower takes (csi.md CSI15): a
// change event of an nvme controller, and no add or remove event, which a
// connect that fails makes, and no event of another subsystem.
func TestIsNvmeEvent(t *testing.T) {
	for _, tc := range []struct {
		ev   fakeUevent
		want bool
	}{
		{uevent("change", "nvme", "NVME_AEN=0x70f002"), true},
		{uevent("change", "nvme", "NVME_EVENT=connected"), true},
		{uevent("change", "nvme", "NVME_EVENT=rediscover"), true},
		{uevent("add", "nvme"), false},
		{uevent("remove", "nvme"), false},
		{uevent("change", "nvme-subsystem"), false},
		{uevent("change", "block"), false},
	} {
		if got := isNvmeEvent([]byte(tc.ev.msg)); got != tc.want {
			t.Errorf("isNvmeEvent(%q) = %v, want %v",
				strings.ReplaceAll(tc.ev.msg, "\x00", " "), got, tc.want)
		}
	}
}
