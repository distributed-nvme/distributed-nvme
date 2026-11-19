package csi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"syscall"
	"time"
)

// The kernel's event socket (csi.md CSI15): the follower takes every change
// event of an nvme controller, among them the discovery-log-change
// notification a persistent discovery connection delivers, as a trigger.
// The socket is the plugin's own, since the OS client has no socket; the
// kernel lets any process receive these events.
const (
	// ueventRecvTimeout bounds one receive, so the reader sees the end of
	// its ctx within it: a close of the socket from another goroutine does
	// not wake a receive blocked on it.
	ueventRecvTimeout = time.Second
	// ueventBufBytes is the receive buffer of one event; an event is a few
	// hundred bytes.
	ueventBufBytes = 1 << 16
)

// ueventConn is one open event socket: recv returns one event, or EAGAIN
// when the receive timeout passed with none; close is called by the reader
// alone.
type ueventConn interface {
	recv(buf []byte) (int, error)
	close() error
}

// openUevents opens the event socket; a variable, so the tests can stand a
// scripted socket in for the kernel's.
var openUevents = openNetlinkUevents

// netlinkUevents is the kernel's event socket.
type netlinkUevents struct {
	fd int
}

// openNetlinkUevents opens the kernel's netlink event socket, joined to the
// kernel's own group, with a receive timeout.
func openNetlinkUevents() (ueventConn, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, err
	}
	tv := syscall.NsecToTimeval(ueventRecvTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET,
		syscall.SO_RCVTIMEO, &tv); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: 1,
	}); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return &netlinkUevents{fd: fd}, nil
}

func (s *netlinkUevents) recv(buf []byte) (int, error) {
	n, _, err := syscall.Recvfrom(s.fd, buf, 0)
	return n, err
}

func (s *netlinkUevents) close() error { return syscall.Close(s.fd) }

// listenUevents reads the event socket until ctx ends and signals events
// for every change event of an nvme controller. It owns conn, which may be
// nil when the caller's open failed, and closes every socket it holds
// itself, after ctx ends included. A receive overflow (ENOBUFS) means
// events were lost: it signals, since a pass reads the node again, and goes
// on reading the same socket, which the kernel keeps usable. Any other
// failure of the socket signals as well, closes it and opens a new one
// after a pause, as does a failed open; the pause doubles from retryBase up
// to maxPause and starts again from retryBase once a receive returns an
// event. A socket opened after a failure signals once more, since the
// events of the time without a socket are lost. Each failure logs a
// follower sync error. It returns within the receive timeout of ctx's end.
func listenUevents(
	ctx context.Context,
	conn ueventConn,
	events chan<- struct{},
	maxPause time.Duration,
) {
	signal := func() {
		select {
		case events <- struct{}{}:
		default:
		}
	}
	pause := retryBase
	wait := func() bool {
		t := time.NewTimer(pause)
		defer t.Stop()
		pause = min(2*pause, max(maxPause, retryBase))
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	warn := func(what string, err error) {
		slog.InfoContext(ctx, msgFollowerSyncError,
			slog.String("error", "the kernel's event socket: "+what+": "+err.Error()))
	}
	buf := make([]byte, ueventBufBytes)
	for {
		if conn == nil {
			if ctx.Err() != nil {
				return
			}
			c, err := openUevents()
			if err != nil {
				warn("open", err)
				if !wait() {
					return
				}
				continue
			}
			conn = c
			signal()
		}
		n, err := conn.recv(buf)
		if ctx.Err() != nil {
			_ = conn.close()
			return
		}
		switch {
		case err == nil:
			pause = retryBase
			if isNvmeEvent(buf[:n]) {
				signal()
			}
		case errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN):
		case errors.Is(err, syscall.ENOBUFS):
			warn("receive", err)
			signal()
		default:
			warn("receive", err)
			_ = conn.close()
			conn = nil
			signal()
			if !wait() {
				return
			}
		}
	}
}

// isNvmeEvent reads one uevent, NUL-separated KEY=VALUE pairs after the
// header line, and takes a change event of an nvme controller: the
// kernel's notice that a controller connected, that a persistent discovery
// controller reconnected, or a notification such as the discovery-log
// change, which carries an NVME_AEN pair. The add and remove events of a
// controller are not taken: a connect that fails adds the controller and
// removes it again, and taking those would run a pass after every failed
// connect at once; the follower's retry timer covers a failed pass instead.
func isNvmeEvent(msg []byte) bool {
	var nvme, change bool
	for _, field := range strings.Split(string(msg), "\x00") {
		switch field {
		case "SUBSYSTEM=nvme":
			nvme = true
		case "ACTION=change":
			change = true
		}
	}
	return nvme && change
}
