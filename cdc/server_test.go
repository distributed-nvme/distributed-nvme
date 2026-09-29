package cdc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
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

// srvSilence bounds how long a test listens for an answer that must not come.
// It orders nothing: a controller that answers too early does so as soon as
// the ICReq arrives, and one that is right stays silent however long the test
// listens.
const srvSilence = 500 * time.Millisecond

// srvWaitingRecord is the CM4 Error row of an instance whose first scan has
// not landed yet.
const srvWaitingRecord = "cdc waiting for first scan"

// srvAnswerWithin returns the controller's next PDU if one arrives within d,
// and nil if the read merely ran out of time — the silence the caller hopes
// for. A socket that ENDED is neither, and fails the test.
func srvAnswerWithin(t *testing.T, h *fakeHost, d time.Duration) *pdu {
	t.Helper()
	if err := h.nc.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	p, err := readCtrlPdu(h.br)
	if err == nil {
		return p
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return nil
	}
	t.Fatalf("the socket ended instead of waiting: %v", err)
	return nil
}

// srvServedTheEntry follows a host whose ICReq went unanswered until the first
// scan landed: that same connection must now get its ICResp, connect, and
// read a discovery log page holding exactly the one stored entry, subNqn.
func srvServedTheEntry(t *testing.T, h *fakeHost, subNqn string) {
	t.Helper()
	p, err := h.recv()
	if err != nil {
		t.Fatalf("no ICResp once the first scan landed: %v", err)
	}
	if p.typ != pduICResp {
		t.Fatalf("pdu type %#x once the first scan landed, want the ICResp",
			p.typ)
	}
	c := h.connect(connHostA, common.NvmeDiscoveryNqn, 0,
		common.CdcMaxAdminSqSize-1, 0)
	connStatus(t, c, statusSuccess, "connect after the first scan")
	c, data := connGetLog(h, lidDiscovery, false, 4096, 0)
	connStatus(t, c, statusSuccess, "get log page after the first scan")
	if numRec := binary.LittleEndian.Uint64(data[8:16]); numRec != 1 {
		t.Fatalf("numrec %d after the first scan, want 1", numRec)
	}
	nqn := string(bytes.TrimRight(data[1024+256:1024+512], "\x00"))
	if nqn != subNqn {
		t.Errorf("the served record is %q, want the scanned entry %q",
			nqn, subNqn)
	}
}

// TestNoDiscoveryAnswerBeforeTheFirstScan proves CM4's start order through the
// socket. Until the watcher's first scan has landed the registry holds no
// state at all, and a successful log page with NUMREC 0 served out of it
// would tell a host there are no subsystems. So the listener is open (the
// host's TCP connect completes into the backlog) but its ICReq goes
// unanswered, and once etcd answers and the scan lands, that same connection
// is served the real records.
func TestNoDiscoveryAnswerBeforeTheFirstScan(t *testing.T) {
	const subNqn = "nqn.2016-06.io.dnv:ss0"
	store := newFakeStore()
	store.set(t, testKey(0x01, 0x1, 0xa), connEntry(subNqn, "10.0.0.1", "4420"))
	store.failRange(errors.New("etcd unavailable"))
	ts := startInstance(t, store)

	h := ts.dial()
	h.write(buildICReqFor(0, 0, 0))
	if p := srvAnswerWithin(t, h, srvSilence); p != nil {
		// Answered with no scan behind the registry: follow the host through
		// Connect and its log read to what it is then told.
		if p.typ != pduICResp {
			t.Fatalf("pdu type %#x before the first scan, want nothing", p.typ)
		}
		c := h.connect(connHostA, common.NvmeDiscoveryNqn, 0,
			common.CdcMaxAdminSqSize-1, 0)
		connStatus(t, c, statusSuccess, "connect before the first scan")
		c, data := connGetLog(h, lidDiscovery, false, 4096, 0)
		t.Fatalf("before the first scan a host was served a discovery log "+
			"page with status %#06x, genctr %d, numrec %d: it was told there "+
			"are no subsystems", c.status,
			binary.LittleEndian.Uint64(data[0:8]),
			binary.LittleEndian.Uint64(data[8:16]))
	}
	if n := ts.srv.connCount(); n != 0 {
		t.Fatalf("%d connections accepted before the first scan, want 0", n)
	}
	if n := ts.accepts.Load(); n != 0 {
		t.Fatalf("Accept called %d times before the first scan, want 0", n)
	}

	// etcd answers, and the watcher's next WV5 retry lands the first scan.
	// Two timers are armed by now — that retry and the minute of the waiting
	// record — and one rescan interval fires only the retry.
	ts.clk.waitWaiters(t, 2)
	store.failRange(nil)
	ts.clk.advance(instanceRescan)
	srvServedTheEntry(t, h, subNqn)
}

// TestNoDiscoveryAnswerWhileTheFirstScanFills proves the order inside CM4's
// first scan: the accept loop starts once that scan has filled the registry,
// not as soon as etcd has answered its Range. With the Range answered and
// Decode held, the registry has no state yet, so a connecting host gets no
// answer and nothing is accepted; once the scan finishes, that same
// connection is served the stored entry.
func TestNoDiscoveryAnswerWhileTheFirstScanFills(t *testing.T) {
	const subNqn = "nqn.2016-06.io.dnv:ss0"
	store := newFakeStore()
	store.set(t, testKey(0x01, 0x1, 0xa), connEntry(subNqn, "10.0.0.1", "4420"))
	entered, release := store.holdDecode()
	ts := startInstance(t, store)
	h := ts.dial()
	h.write(buildICReqFor(0, 0, 0))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first scan never decoded the stored entry")
	}
	if p := srvAnswerWithin(t, h, srvSilence); p != nil {
		t.Fatalf("pdu type %#x while the first scan was still filling the "+
			"registry, want nothing", p.typ)
	}
	if n := ts.accepts.Load(); n != 0 {
		t.Fatalf("Accept called %d times while the first scan was still "+
			"filling the registry, want 0", n)
	}

	release()
	srvServedTheEntry(t, h, subNqn)
}

// TestShutdownWhileWaitingForTheFirstScan proves CM4's wait from its other
// side: while the first scan does not land, `cdc waiting for first scan` is
// logged at Error once a minute — and never before the minute is up — and a
// shutdown still ends the instance: the listener closes, and the host that
// connected meanwhile is dropped without ever having been answered, the
// accept loop never having run.
func TestShutdownWhileWaitingForTheFirstScan(t *testing.T) {
	logs := captureLogs(t)
	store := newFakeStore()
	store.failRange(errors.New("etcd unavailable"))
	ts := startInstance(t, store)
	h := ts.dial()
	h.write(buildICReqFor(0, 0, 0))

	// Two timers are armed: the watcher's WV5 retry and the minute of the
	// waiting record. Each minute is crossed in two steps: a nanosecond short
	// of it fires only the retry and must leave the count unchanged, and the
	// last nanosecond fires the minute. After each step the test waits until
	// both timers are armed again.
	ts.clk.waitWaiters(t, 2)
	const minutes = 3
	for m := 1; m <= minutes; m++ {
		ts.clk.advance(time.Minute - time.Nanosecond)
		ts.clk.waitWaiters(t, 2)
		if n := logs.count(srvWaitingRecord); n != m-1 {
			t.Fatalf("%d %q records a nanosecond before minute %d, want %d",
				n, srvWaitingRecord, m, m-1)
		}
		ts.clk.advance(time.Nanosecond)
		logs.waitFor(t, srvWaitingRecord, m)
		ts.clk.waitWaiters(t, 2)
	}
	recs := logs.find(srvWaitingRecord)
	if len(recs) != minutes {
		t.Fatalf("%d %q records after %d minutes, want %d",
			len(recs), srvWaitingRecord, minutes, minutes)
	}
	for _, rec := range recs {
		if rec["level"] != slog.LevelError.String() {
			t.Errorf("%q logged at %v, want %v", srvWaitingRecord,
				rec["level"], slog.LevelError)
		}
	}

	ts.cancel()
	select {
	case <-ts.done:
	case <-time.After(5 * time.Second):
		t.Fatal("an instance waiting for its first scan did not stop")
	}
	if n := ts.accepts.Load(); n != 0 {
		t.Fatalf("Accept called %d times by an instance that never scanned, "+
			"want 0", n)
	}
	srvDialFails(t, ts.addr)
	connSocketClosed(t, h)
	for _, msg := range []string{msgScanComplete, msgHostConnected} {
		if n := logs.count(msg); n != 0 {
			t.Errorf("%d %q records, want 0", n, msg)
		}
	}
}

// TestAnEmptyFirstScanStillOpensTheAcceptLoop pins the other side of CM4's
// gate: a first scan that lands holding no owned entry is a real answer — no
// subsystems — not the empty registry of an instance that has not scanned
// yet, so the accept loop starts and the host reads a successful NUMREC 0
// page. A gate keyed on the registry holding an entry would leave an instance
// whose owned shards hold none (a fresh cluster, a twin whose range has no
// subsystem yet) answering nobody.
func TestAnEmptyFirstScanStillOpensTheAcceptLoop(t *testing.T) {
	logs := captureLogs(t)
	store := newFakeStore()
	// A foreign shard only: the instance owns codes 00-0f, so the store is
	// not empty but the scan keeps nothing of it.
	store.set(t, testKey(0x21, 0x1, 0xa), connEntry("nqn.2016-06.io.dnv:ssx",
		"10.0.0.1", "4420"))
	ts := startInstance(t, store)
	scans := logs.waitFor(t, msgScanComplete, 1)
	if got := watchAttrInt(t, scans[0], "entries"); got != 0 {
		t.Fatalf("the first scan kept %d owned entries, want 0", got)
	}

	h := ts.dial()
	h.handshake()
	c := h.connect(connHostA, common.NvmeDiscoveryNqn, 0,
		common.CdcMaxAdminSqSize-1, 0)
	connStatus(t, c, statusSuccess, "connect after an empty first scan")
	c, data := connGetLog(h, lidDiscovery, false, 4096, 0)
	connStatus(t, c, statusSuccess, "get log page after an empty first scan")
	if numRec := binary.LittleEndian.Uint64(data[8:16]); numRec != 0 {
		t.Fatalf("numrec %d after an empty first scan, want 0", numRec)
	}
}

// TestRunAnswersNoHostBeforeTheFirstScan proves that Run itself goes through
// CM4's gate, not only the runInstance that startInstance runs. Run takes a
// real etcd client; this one still builds, since etcdutil.New dials lazily,
// but its only endpoint refuses connections, so no scan ever lands. A host's
// ICReq then goes unanswered, and a shutdown in that wait still makes Run log
// `cdc stopping` once and return nil, the host dropped unanswered.
func TestRunAnswersNoHostBeforeTheFirstScan(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli, err := etcdutil.New(ctx, []string{"127.0.0.1:1"}, time.Second)
	if err != nil {
		t.Fatalf("etcd client: %v", err)
	}
	// Run reports no address it bound, so it is handed a concrete port: one
	// the kernel has just handed out and this test released again. Should
	// another process take it meanwhile, Run fails to listen, which fails
	// this test rather than passing it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	addr := probe.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	probe.Close()
	cfg := Config{
		Ranges:  []uint32{0},
		TrType:  common.DefaultCdcTrType,
		AdrFam:  common.DefaultCdcAdrFam,
		TrAddr:  "127.0.0.1",
		TrSvcId: port,
	}
	done := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		done <- Run(ctx, cli, cfg)
	}()
	// A check that fails below must not leave this Run serving, and logging,
	// into the tests after this one.
	t.Cleanup(func() {
		cancel()
		select {
		case <-returned:
		case <-time.After(15 * time.Second):
			t.Error("Run did not return")
		}
	})

	var nc net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("Run returned %v before any shutdown, want it listening "+
				"on %s", err, addr)
		default:
		}
		if nc, err = net.Dial("tcp", addr); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run never listened on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer nc.Close()
	h := newFakeHost(t, nc)
	h.write(buildICReqFor(0, 0, 0))
	if p := srvAnswerWithin(t, h, srvSilence); p != nil {
		t.Fatalf("Run answered pdu type %#x before its first scan, want "+
			"nothing", p.typ)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on shutdown, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after a shutdown while waiting")
	}
	connSocketClosed(t, h)
	if n := logs.count(msgScanComplete); n != 0 {
		t.Errorf("%d %q records with an etcd endpoint that refuses "+
			"connections, want 0", n, msgScanComplete)
	}
	if n := logs.count(msgCdcStopping); n != 1 {
		t.Errorf("%d %q records, want 1", n, msgCdcStopping)
	}
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
