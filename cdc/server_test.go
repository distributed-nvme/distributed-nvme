package cdc

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The NP1/NP5/NP13 tests of the listener itself: the controller ids it hands
// out, the host states its connections create and drop, and what a context
// cancellation does to a fleet of live hosts.

// srvDialFails asserts that nothing is listening on addr any more (NP1: a
// stopped instance stops accepting).
func srvDialFails(t *testing.T, addr string) {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		nc.Close()
		t.Fatal("the listener is still accepting connections")
	}
}

// srvDisconnects returns the `host disconnected` reasons captured so far,
// keyed by hostnqn.
func srvDisconnects(t *testing.T, logs *logCapture) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, rec := range logs.find(msgHostDisconnected) {
		nqn, _ := rec["hostnqn"].(string)
		reason, _ := rec["reason"].(string)
		out[nqn] = reason
	}
	return out
}

// TestCntlIdIsDynamicAndRoundRobin proves NP5's controller ids: every
// connection gets its own out of [1, common.CdcCntlIdMax], two live
// connections never share one, and the counter wraps rather than running off
// the end of the range.
func TestCntlIdIsDynamicAndRoundRobin(t *testing.T) {
	ts := startServer(t)
	a := ts.dial()
	b := ts.dial()
	idA := a.connectOk(connHostA)
	idB := b.connectOk(connHostB)
	for _, id := range []uint16{idA, idB} {
		if id < 1 || id > common.CdcCntlIdMax {
			t.Fatalf("cntlid %d is outside [1, %d]", id, common.CdcCntlIdMax)
		}
	}
	if idA == idB {
		t.Fatalf("two simultaneous connections share cntlid %d", idA)
	}
	if idB != idA+1 {
		t.Errorf("cntlids %d then %d, want a round-robin counter", idA, idB)
	}
	// A third connection continues the walk, and the counter returns to 1
	// only after the whole range has been used.
	c := ts.dial()
	if idC := c.connectOk(connHostA); idC != idB+1 {
		t.Errorf("third cntlid %d, want %d", idC, idB+1)
	}
	ts.srv.mu.Lock()
	ts.srv.cntlIdSeq = common.CdcCntlIdMax
	ts.srv.mu.Unlock()
	if id := ts.srv.nextCntlId(); id != 1 {
		t.Fatalf("cntlid after %#x is %d, want 1", common.CdcCntlIdMax, id)
	}
}

// TestHostStateDroppedAtLastDisconnect proves DS7 and NP13: the host state is
// shared by every connection of one hostnqn and dies with the last of them,
// while another hostnqn's state is untouched.
func TestHostStateDroppedAtLastDisconnect(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	a1 := ts.dial()
	a2 := ts.dial()
	b := ts.dial()
	a1.connectOk(connHostA)
	a2.connectOk(connHostA)
	b.connectOk(connHostB)
	if n := ts.reg.hostCount(); n != 2 {
		t.Fatalf("registry tracks %d hosts, want 2 (one per hostnqn)", n)
	}
	if n := ts.srv.connCount(); n != 3 {
		t.Fatalf("%d live connections, want 3", n)
	}

	// The first connection of host A goes away: its state stays, because the
	// second one still holds it.
	a1.close()
	logs.waitFor(t, msgHostDisconnected, 1)
	if n := ts.reg.hostCount(); n != 2 {
		t.Fatalf("registry tracks %d hosts after one of two connections of "+
			"a hostnqn closed, want 2", n)
	}

	// The last one takes the state with it (§0 #6: this is what restarts
	// GENCTR for the next connection).
	a2.close()
	logs.waitFor(t, msgHostDisconnected, 2)
	if n := ts.reg.hostCount(); n != 1 {
		t.Fatalf("registry tracks %d hosts after host A left, want 1", n)
	}
	b.close()
	logs.waitFor(t, msgHostDisconnected, 3)
	if n := ts.reg.hostCount(); n != 0 {
		t.Fatalf("registry tracks %d hosts, want 0", n)
	}
	for nqn, reason := range srvDisconnects(t, logs) {
		if reason != reasonClosed {
			t.Errorf("%s: reason %q, want %q", nqn, reason, reasonClosed)
		}
	}
	if n := ts.srv.connCount(); n != 0 {
		t.Errorf("%d live connections after every host left, want 0", n)
	}
}

// TestReconnectingHostRestartsGenCtr proves §0 #6 through the socket: a
// hostnqn whose last connection dropped is a new host when it comes back, so
// its GENCTR starts at 1 again even though the served entries moved on.
func TestReconnectingHostRestartsGenCtr(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	first := ts.dial()
	first.connectOk(connHostA)
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	_, data := connGetLog(first, lidDiscovery, false, 2048, 0)
	if genCtr := binary.LittleEndian.Uint64(data[0:8]); genCtr != 2 {
		t.Fatalf("genctr %d after one impact, want 2", genCtr)
	}
	first.close()
	logs.waitFor(t, msgHostDisconnected, 1)

	second := ts.dial()
	second.connectOk(connHostA)
	_, data = connGetLog(second, lidDiscovery, false, 2048, 0)
	if genCtr := binary.LittleEndian.Uint64(data[0:8]); genCtr != 1 {
		t.Fatalf("genctr %d on a rebuilt host state, want 1", genCtr)
	}
	if numRec := binary.LittleEndian.Uint64(data[8:16]); numRec != 1 {
		t.Fatalf("numrec %d, want 1: the view is rendered at attach", numRec)
	}
}

// TestServerShutdownClosesEveryConnection proves NP1's SIGTERM behavior and
// the §7 reason it logs: the listener stops accepting, every live connection
// is closed as `shutdown`, and the host states go with them.
func TestServerShutdownClosesEveryConnection(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	a := ts.dial()
	b := ts.dial()
	a.connectOk(connHostA)
	b.connectOk(connHostB)
	if n := ts.srv.connCount(); n != 2 {
		t.Fatalf("%d live connections, want 2", n)
	}

	// stop() returns once the accept loop has joined every connection, so
	// every record below has already been emitted.
	ts.stop()

	reasons := srvDisconnects(t, logs)
	if len(reasons) != 2 {
		t.Fatalf("%d `host disconnected` records, want 2: %v",
			len(reasons), reasons)
	}
	for _, nqn := range []string{connHostA, connHostB} {
		if reasons[nqn] != reasonShutdown {
			t.Errorf("%s: reason %q, want %q", nqn, reasons[nqn],
				reasonShutdown)
		}
	}
	if n := ts.reg.hostCount(); n != 0 {
		t.Errorf("registry tracks %d hosts after shutdown, want 0", n)
	}
	if n := ts.srv.connCount(); n != 0 {
		t.Errorf("%d live connections after shutdown, want 0", n)
	}
	connSocketClosed(t, a)
	connSocketClosed(t, b)
	srvDialFails(t, ts.addr)
}

// TestServerShutdownWithNoConnections proves the same path is safe when
// nothing is connected: an idle instance stops without waiting for anything.
func TestServerShutdownWithNoConnections(t *testing.T) {
	ts := startServer(t)
	ts.stop()
	srvDialFails(t, ts.addr)
}

// TestServeUnconnectedSocketIsReapedNotLogged proves the NP13 half that has no
// host: a socket that opened and went away before Connect leaves no
// `host disconnected` record, because no host state ever existed.
func TestServeUnconnectedSocketIsReapedNotLogged(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	h.handshake()
	h.close()
	// stop() joins every connection goroutine, so by the time it returns the
	// closed socket has been through the whole NP13 teardown.
	ts.stop()
	if n := ts.srv.connCount(); n != 0 {
		t.Errorf("%d live connections, want 0", n)
	}
	if n := logs.count(msgHostDisconnected); n != 0 {
		t.Errorf("%d `host disconnected` records for a socket that never "+
			"connected, want 0", n)
	}
	if n := ts.reg.hostCount(); n != 0 {
		t.Errorf("registry tracks %d hosts, want 0", n)
	}
}
