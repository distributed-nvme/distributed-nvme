package csi

import (
	"context"
	"strings"
	"syscall"
)

// listenUevents reads the kernel's device events off its netlink socket and
// signals every event of an nvme controller, among them the
// discovery-log-change notification a persistent discovery connection
// delivers (csi.md CSI15). The socket is the plugin's own, since the OS
// client has no socket; it needs the privileges the node plugin runs with.
// The function returns when ctx ends or the socket fails; the follower's
// timer covers a node whose events cannot be read.
func listenUevents(ctx context.Context, events chan<- struct{}) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: 1,
	}); err != nil {
		_ = syscall.Close(fd)
		return err
	}
	go func() {
		<-ctx.Done()
		_ = syscall.Close(fd)
	}()
	buf := make([]byte, 1<<16)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			return err
		}
		if isNvmeEvent(buf[:n]) {
			select {
			case events <- struct{}{}:
			default:
			}
		}
	}
}

// isNvmeEvent reads one uevent: NUL-separated KEY=VALUE pairs after the
// header line. An event of an nvme controller is one whose subsystem is
// nvme; the change notification carries an NVME_AEN pair as well.
func isNvmeEvent(msg []byte) bool {
	for _, field := range strings.Split(string(msg), "\x00") {
		if field == "SUBSYSTEM=nvme" {
			return true
		}
	}
	return false
}
