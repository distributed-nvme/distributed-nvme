package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the gateway's answer to a connection to etcd that breaks while
// a request to etcd is in flight: an etcd connection error, which is ABORTED
// like every other (architecture.md, UNEXPECTED_ERROR → `ABORTED`; Running
// without the control plane), although the etcd client reports it with a gRPC
// status of its own.

// etcdCutProxy forwards TCP between one etcd client and the package's etcd.
// Once armed, it closes every connection it carries as soon as etcd starts its
// next reply, and drops that reply, so the request the reply answers is in
// flight, sent and unanswered, when its connection dies; a connection made
// after that is closed at once. A reply starts with an HTTP/2 HEADERS frame,
// and gRPC over plain TCP is HTTP/2 from etcd's first byte, so the proxy
// relays etcd's side one frame at a time.
type etcdCutProxy struct {
	ln    net.Listener
	armed atomic.Bool

	mu    sync.Mutex
	cut   bool
	conns []net.Conn
}

// http2FrameHeaders is the frame type of an HTTP/2 HEADERS frame (RFC 9113).
const http2FrameHeaders = 0x1

// startEtcdCutProxy listens on a free localhost port and forwards to the
// package's etcd until the test ends.
func startEtcdCutProxy(t *testing.T) *etcdCutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &etcdCutProxy{ln: ln}
	go p.accept()
	t.Cleanup(func() {
		ln.Close()
		p.closeAll()
	})
	return p
}

func (p *etcdCutProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", testEndpoint)
		if err != nil {
			client.Close()
			continue
		}
		if !p.track(client, server) {
			client.Close()
			server.Close()
			continue
		}
		go func() {
			_, _ = io.Copy(server, client)
			server.Close()
		}()
		go p.relayReplies(server, client)
	}
}

// track records a connection pair, and refuses it once the proxy has cut.
func (p *etcdCutProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cut {
		return false
	}
	p.conns = append(p.conns, conns...)
	return true
}

// closeAll closes every connection the proxy carries and refuses every later
// one.
func (p *etcdCutProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// relayReplies copies etcd's side of one connection to the client frame by
// frame, and cuts at the first HEADERS frame once armed.
func (p *etcdCutProxy) relayReplies(server, client net.Conn) {
	defer client.Close()
	r := bufio.NewReader(server)
	header := make([]byte, 9)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return
		}
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return
		}
		if header[3] == http2FrameHeaders && p.armed.Load() {
			p.closeAll()
			return
		}
		if _, err := client.Write(header); err != nil {
			return
		}
		if _, err := client.Write(payload); err != nil {
			return
		}
	}
}

// newCutClient is an etcd client whose one connection runs through p.
func newCutClient(t *testing.T, p *etcdCutProxy) *etcdutil.Client {
	t.Helper()
	cli, err := etcdutil.New(
		context.Background(),
		[]string{p.ln.Addr().String()},
		time.Second,
	)
	if err != nil {
		t.Fatalf("New through the proxy: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// TestEtcdRequestInFlightAtABreakIsAborted holds GW7's ABORTED row for an
// etcd request in flight when the connection to etcd breaks, as when etcd
// crashes. The etcd client fails such a request at once, a commit and an STM
// read alike, with UNAVAILABLE, and etcdutil wraps that (dnv-worker.md EU6);
// mapStmErr must answer ABORTED, not pass the etcd client's code on as one the
// gateway chose. Each row first checks that the request really failed that
// way, so a row that took another path cannot pass by accident.
//
// The commit row also shows why the failed call is no proof that nothing was
// written: etcd had applied the commit before it began its reply.
func TestEtcdRequestInFlightAtABreakIsAborted(t *testing.T) {
	ctx := context.Background()

	t.Run("a commit in flight", func(t *testing.T) {
		p := startEtcdCutProxy(t)
		cli := newCutClient(t, p)
		key := fmt.Sprintf("etcdloss-commit-%d", testSeq.Add(1))
		start := time.Now()
		err := cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			stm.Get(key, &pb.SpRev{})
			stm.Put(key, &pb.SpRev{SpName: "etcdloss", Revision: 1})
			// The read above is answered; the reply to the commit is not.
			p.armed.Store(true)
			return nil
		})
		var inner interface{ GRPCStatus() *status.Status }
		if !errors.As(err, &inner) ||
			inner.GRPCStatus().Code() != codes.Unavailable {
			t.Fatalf("RunSTM after %v: %v; want the etcd client's own "+
				"UNAVAILABLE inside it", time.Since(start), err)
		}
		if got := status.Code(mapStmErr(err)); got != codes.Aborted {
			t.Errorf("mapStmErr(%v) = %v, want Aborted", err, got)
		}
		landed := &pb.SpRev{}
		found, getErr := newTestClient(t).Get(ctx, key, landed)
		if getErr != nil {
			t.Fatalf("Get %s: %v", key, getErr)
		}
		if !found || landed.GetSpName() != "etcdloss" {
			t.Errorf("the commit in flight did not land (found=%v %v): "+
				"etcd replies only once it has applied a commit", found, landed)
		}
	})

	t.Run("a read in flight, through GetCluster", func(t *testing.T) {
		name := fmt.Sprintf("etcdloss-%d", testSeq.Add(1))
		mustCluster(t, newTestServer(t), name)
		p := startEtcdCutProxy(t)
		s := NewServer(newCutClient(t, p))
		p.armed.Store(true)
		_, err := s.GetCluster(ctx, &pb.GetClusterRequest{ClusterName: name})
		if got := status.Code(err); got != codes.Aborted {
			t.Fatalf("GetCluster: %v, want Aborted", err)
		}
		if msg := status.Convert(err).Message(); !strings.Contains(
			msg, "code = Unavailable") {
			t.Errorf("GetCluster's message %q: the read did not fail as "+
				"one in flight at the break does", msg)
		}
	})
}
