package cdc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The cases that run a whole instance over the real etcd, with the cdc
// entries written into it the way the worker writes them
// (plantsupport_test.go), against the in-process fake host: the largest
// entry set a cluster can hold, more hosts than the pending cap, one change
// reaching every connection it concerns, hosts that stop reading or read
// slowly, the host NQN bounds, the transfer bound of one Get Log Page and
// the CNTLID counter's wrap. The two full-shape cases are skipped under
// -short.

// failedOver is cntlrs after a failover from cntlr 0 to cntlr 1, as the
// worker's failover commit leaves them (architecture.md [D18]): the old
// primary is a standby with its health epoch set, which the listing rule no
// longer lists, and cntlr 1 is the primary.
func failedOver(cntlrs []*pb.Cntlr) []*pb.Cntlr {
	out := make([]*pb.Cntlr, 0, len(cntlrs))
	for i, c := range cntlrs {
		next := &pb.Cntlr{NvmeTrConf: c.GetNvmeTrConf(), Primary: i == 1}
		if i == 0 {
			next.ErrEpoch = 1
		}
		out = append(out, next)
	}
	return out
}

// ownsAll is the shard-code test of an instance that owns every range.
func ownsAll(uint32) bool { return true }

// ---------------------------------------------------------------------------
// The full shape: the most pools and subsystems (DS1 to DS6, NP8)
// ---------------------------------------------------------------------------

// TestTheMostEntriesAClusterHolds proves the discovery service model at its
// largest shape: a cluster's most pools, common.MaxSpCntPerCluster, each
// with its most subsystems, common.MaxSsCntPerSp, spread over every shard
// code, each entry listing the widest transport list of
// common.MaxCntlrCntPerSp cntlrs and naming the most allowed hosts,
// common.MaxHostCntPerSs. One host is named in every entry, so its log holds
// every record. An instance that owns every range serves all of them, and
// an instance that owns one range digit exactly the pools whose shard codes
// that digit claims (DS2), each in the order of DS5 and read in commands of
// the most one command moves (NP8). One entry then changes, as a failover
// rewrites it: every connected host it names gets one AEN and sees the
// change, and a connected host it does not name gets nothing (DS6).
func TestTheMostEntriesAClusterHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("full shape: every pool and subsystem a cluster holds")
	}
	// The entries are several MiB, written and deleted on every run, so the
	// case runs on an etcd of its own.
	cli := newPrivateTestClient(t)
	env := newPlantEnv(t, cli)
	const otherCnt = 64
	hostAll := env.hostNqn(0)
	other := func(i int) string { return env.hostNqn(1 + i%otherCnt) }
	entries := make([]plantedEntry, 0,
		common.MaxSpCntPerCluster*common.MaxSsCntPerSp)
	for p := 0; p < common.MaxSpCntPerCluster; p++ {
		spId := uint64(p + 1)
		shard := uint32(p % common.ShardBucketSize)
		cntlrs := plantCntlrs(spId, common.MaxCntlrCntPerSp)
		for ss := 0; ss < common.MaxSsCntPerSp; ss++ {
			idx := p*common.MaxSsCntPerSp + ss
			allowed := []string{hostAll}
			for j := 0; j < common.MaxHostCntPerSs-1; j++ {
				allowed = append(allowed,
					other(idx*(common.MaxHostCntPerSs-1)+j))
			}
			entries = append(entries,
				env.poolEntry(shard, spId, uint64(ss+1), cntlrs, allowed))
		}
	}
	env.put(entries...)

	const digit = 0x3
	ownsDigit := func(shard uint32) bool { return shard/16 == digit }
	whole := startPlanted(t, cli, allRanges(), nil)
	part := startPlanted(t, cli, []uint32{digit}, nil)
	waitScanned(t, whole, len(entries), 90*time.Second)
	waitScanned(t, part, len(entries)/16, 90*time.Second)
	if n := whole.reg.entryCount(); n != len(entries) {
		t.Fatalf("the instance owning every range holds %d entries, want %d",
			n, len(entries))
	}
	if n := part.reg.entryCount(); n != len(entries)/16 {
		t.Fatalf("the instance owning range %x holds %d entries, want %d",
			digit, n, len(entries)/16)
	}

	// The change is to an entry the one-digit instance owns, so both
	// instances serve it.
	changedIdx := 0x35*common.MaxSsCntPerSp + 1
	changed := entries[changedIdx]
	named := changed.msg.GetAllowedHosts()[1]
	notNamed := ""
	for i := 0; i < otherCnt && notNamed == ""; i++ {
		candidate := other(i)
		found := false
		for _, h := range changed.msg.GetAllowedHosts() {
			found = found || h == candidate
		}
		if !found {
			notNamed = candidate
		}
	}

	type watched struct {
		h       *fakeHost
		hostNqn string
		owns    func(uint32) bool
		aer     uint16
	}
	connectAndCheck := func(ts *testServer, hostNqn string,
		owns func(uint32) bool) *watched {
		h := ts.dial()
		h.connectOk(hostNqn)
		h.enableAen()
		checkLog(t, h, 1, newWantLog(entries, hostNqn, owns))
		return &watched{h: h, hostNqn: hostNqn, owns: owns,
			aer: armConfirmed(t, h)}
	}
	impacted := []*watched{
		connectAndCheck(whole, hostAll, ownsAll),
		connectAndCheck(part, hostAll, ownsDigit),
		connectAndCheck(whole, named, ownsAll),
	}
	spared := connectAndCheck(whole, notNamed, ownsAll)

	next := env.poolEntry(changed.shard, changed.spId, changed.ssId,
		failedOver(plantCntlrs(changed.spId, common.MaxCntlrCntPerSp)),
		changed.msg.GetAllowedHosts())
	env.put(next)
	entries[changedIdx] = next
	for _, w := range impacted {
		awaitAen(t, w.h, w.aer, "after one entry of the full shape changed")
	}
	noAen(t, spared.h, spared.aer, "for a host the changed entry names not")
	if gen, _ := logHeader(t, spared.h); gen != 1 {
		t.Errorf("genctr %d for a host the change did not impact, want 1", gen)
	}
	// Every impacted connection reads its whole log again: the change is in
	// each at its DS5 place.
	for _, w := range impacted {
		checkLog(t, w.h, 2, newWantLog(entries, w.hostNqn, w.owns))
	}
}

// ---------------------------------------------------------------------------
// More hosts than the pending cap (NP1, NP12)
// ---------------------------------------------------------------------------

// burstHost is one host of the burst, run off the test goroutine, so it
// reports a failure rather than ending the test: it connects, sends its
// ICReq and says so on sent, then waits for the controller to answer, which
// it counts in answered, connects as hostNqn and reads its log, which must
// hold exactly the one record of subNqn. It returns the connection, still
// open.
func burstHost(
	addr string,
	hostNqn string,
	subNqn string,
	sent func(),
	answered *atomic.Int32,
) (net.Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, 30*time.Second)
	if err != nil {
		sent()
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		nc.Close()
		return nil, err
	}
	if err := nc.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		sent()
		return fail(err)
	}
	_, err = nc.Write(buildICReqFor(0, 0, 0))
	sent()
	if err != nil {
		return fail(err)
	}
	br := bufio.NewReaderSize(nc, maxPduLen)
	p, err := readCtrlPdu(br)
	if err != nil {
		return fail(fmt.Errorf("icresp: %w", err))
	}
	if p.typ != pduICResp {
		return fail(fmt.Errorf("pdu type %#x, want the icresp", p.typ))
	}
	answered.Add(1)
	if _, err := nc.Write(capsuleFrame(connCmd(1, connDefaults(hostNqn)))); err != nil {
		return fail(err)
	}
	if p, err = readCtrlPdu(br); err != nil {
		return fail(fmt.Errorf("connect: %w", err))
	}
	if p.typ != pduCapsuleResp {
		return fail(fmt.Errorf("pdu type %#x, want the connect completion",
			p.typ))
	}
	if c := parseCapsuleResp(p); c.status != statusSuccess {
		return fail(fmt.Errorf("connect refused with status %#x", c.status))
	}
	const length = common.CdcDiscLogHeaderSize + common.CdcDiscLogEntrySize
	s := newSqe(opcGetLogPage, 2)
	setSgl(s, length)
	s[40] = lidDiscovery
	binary.LittleEndian.PutUint16(s[42:44], length/4-1)
	if _, err := nc.Write(capsuleFrame(s, nil)); err != nil {
		return fail(err)
	}
	if p, err = readCtrlPdu(br); err != nil {
		return fail(fmt.Errorf("log page data: %w", err))
	}
	if p.typ != pduC2HData || len(p.data) != length {
		return fail(fmt.Errorf("pdu type %#x of %d bytes, want the %d "+
			"bytes of the log", p.typ, len(p.data), length))
	}
	if n := binary.LittleEndian.Uint64(p.data[8:16]); n != 1 {
		return fail(fmt.Errorf("numrec %d, want 1", n))
	}
	record := p.data[common.CdcDiscLogHeaderSize:]
	if got := string(bytes.TrimRight(record[256:512], "\x00")); got != subNqn {
		return fail(fmt.Errorf("subnqn %q, want %q", got, subNqn))
	}
	if p, err = readCtrlPdu(br); err != nil {
		return fail(fmt.Errorf("log page completion: %w", err))
	}
	if c := parseCapsuleResp(p); p.typ != pduCapsuleResp ||
		c.status != statusSuccess {
		return fail(fmt.Errorf("log page completion: pdu type %#x status "+
			"%#x", p.typ, c.status))
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return nc, nil
}

// TestMoreHostsThanThePendingCap proves NP1's cap at a burst of hosts, the
// shape a cdc restart sends (DS11): common.CdcMaxPendingConnCnt
// connections that never send a byte fill the cap, then three times as many
// hosts connect at once, each named in an entry of its own. While the cap
// is full the instance accepts none of them and answers no ICReq; once the
// idle connections close, every host is accepted, connects and reads its
// own record, and none is refused (NP12: the cap delays a connection, it
// refuses none).
func TestMoreHostsThanThePendingCap(t *testing.T) {
	if testing.Short() {
		t.Skip("full shape: more hosts than the pending cap")
	}
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	pendingCap := common.CdcMaxPendingConnCnt
	burst := 3 * pendingCap
	entries := make([]plantedEntry, 0, burst)
	for i := 0; i < burst; i++ {
		spId := uint64(i/common.MaxSsCntPerSp + 1)
		ssId := uint64(i%common.MaxSsCntPerSp + 1)
		entries = append(entries, env.poolEntry(
			uint32(spId%common.ShardBucketSize), spId, ssId,
			plantCntlrs(spId, 1), []string{env.hostNqn(i)}))
	}
	env.put(entries...)
	ts := startPlanted(t, cli, allRanges(), nil)
	waitScanned(t, ts, burst, 30*time.Second)
	idle := srvFillPending(t, ts, pendingCap, pendingCap)

	var sentWg, doneWg sync.WaitGroup
	var answered atomic.Int32
	conns := make([]net.Conn, burst)
	errs := make([]error, burst)
	// On a failure before the idle connections close, the burst is still
	// waiting at the cap: closing them lets it finish before its
	// connections are closed.
	t.Cleanup(func() {
		for _, h := range idle {
			h.close()
		}
		doneWg.Wait()
		for _, nc := range conns {
			if nc != nil {
				nc.Close()
			}
		}
	})
	for i := 0; i < burst; i++ {
		sentWg.Add(1)
		doneWg.Add(1)
		go func(i int) {
			defer doneWg.Done()
			var once sync.Once
			conns[i], errs[i] = burstHost(ts.addr, env.hostNqn(i),
				entries[i].msg.GetNqn(), func() { once.Do(sentWg.Done) },
				&answered)
		}(i)
	}
	sentWg.Wait()
	time.Sleep(srvSilence)
	if n := answered.Load(); n != 0 {
		t.Fatalf("%d hosts of the burst were answered while the pending cap "+
			"was full, want 0", n)
	}
	if n := ts.accepts.Load(); n != int32(pendingCap) {
		t.Fatalf("Accept called %d times with %d connections pending, want "+
			"%d: at the cap the loop calls no Accept", n, pendingCap,
			pendingCap)
	}

	for _, h := range idle {
		h.close()
	}
	doneWg.Wait()
	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			if failed <= 5 {
				t.Errorf("host %d of the burst: %v", i, err)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d hosts of the burst failed, want none: the pending "+
			"cap must delay a connection, never refuse it", failed, burst)
	}
	srvWaitConns(t, ts, burst)
	if n := ts.reg.hostCount(); n != burst {
		t.Errorf("registry tracks %d hosts, want %d", n, burst)
	}
	// Every connection has connected, so the one slot left is the accept
	// loop's, for the Accept it waits in.
	srvWaitPending(t, ts, 1)
}

// ---------------------------------------------------------------------------
// One change reaches every connection it concerns (DS6, NP11)
// ---------------------------------------------------------------------------

// TestOneRecordChangeReachesEveryConnectedHost proves DS6 and NP11 across
// twins, two instances serving the same ranges over one etcd, as hosts hold
// a discovery connection to every cdc endpoint (DS2). One entry names the
// most hosts a subsystem admits, common.MaxHostCntPerSs, and each of them
// holds two connections to each twin. One write to the entry, as a failover
// rewrites its transports, gives every one of those connections exactly one
// AEN and then a log with the new transports at GENCTR 2, while the
// connections of hosts the entry does not name get no AEN and keep GENCTR 1.
func TestOneRecordChangeReachesEveryConnectedHost(t *testing.T) {
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	named := make([]string, common.MaxHostCntPerSs)
	for i := range named {
		named[i] = env.hostNqn(i)
	}
	others := []string{env.hostNqn(100), env.hostNqn(101)}
	const spId = 7
	const shard = 0x42
	cntlrs := plantCntlrs(spId, common.MaxCntlrCntPerSp)
	changed := env.poolEntry(shard, spId, 1, cntlrs, named)
	unrelated := env.poolEntry(shard, spId, 2, cntlrs, others)
	entries := []plantedEntry{changed, unrelated}
	env.put(entries...)
	twins := []*testServer{
		startPlanted(t, cli, allRanges(), nil),
		startPlanted(t, cli, allRanges(), nil),
	}

	type watched struct {
		h       *fakeHost
		hostNqn string
		aer     uint16
	}
	open := func(ts *testServer, hostNqn string) *watched {
		h := ts.dial()
		h.connectOk(hostNqn)
		h.enableAen()
		checkLog(t, h, 1, newWantLog(entries, hostNqn, ownsAll))
		return &watched{h: h, hostNqn: hostNqn, aer: armConfirmed(t, h)}
	}
	var impacted, spared []*watched
	for _, ts := range twins {
		waitScanned(t, ts, len(entries), 10*time.Second)
		for _, hostNqn := range named {
			for i := 0; i < 2; i++ {
				impacted = append(impacted, open(ts, hostNqn))
			}
		}
		for _, hostNqn := range others {
			spared = append(spared, open(ts, hostNqn))
		}
	}

	next := env.poolEntry(shard, spId, 1, failedOver(cntlrs), named)
	env.put(next)
	entries[0] = next
	for i, w := range impacted {
		awaitAen(t, w.h, w.aer, fmt.Sprintf("on connection %d of %d", i+1,
			len(impacted)))
	}
	for _, w := range impacted {
		checkLog(t, w.h, 2, newWantLog(entries, w.hostNqn, ownsAll))
	}
	for _, w := range spared {
		noAen(t, w.h, w.aer, "for a host the changed entry names not")
		checkLog(t, w.h, 1, newWantLog(entries, w.hostNqn, ownsAll))
	}
}

// ---------------------------------------------------------------------------
// Hosts that stop reading, and slow ones (NP13, DS6 -> NP11, DS9)
// ---------------------------------------------------------------------------

// tightBuf is the socket buffer size the deaf and slow host cases ask for on
// both ends, so a host that stops reading blocks the controller's write
// after a few KiB rather than after the MiBs loopback buffers grow to.
const tightBuf = 4096

// tightListener wraps an instance's listener: each accepted socket gets a
// tightBuf send buffer, and when writeTmo is not zero every write deadline
// the controller sets is moved to writeTmo from now, so NP13's deadline
// ends a write the host does not read in seconds rather than at
// socketWriteTimeout. A write the controller sets no deadline on still has
// none.
type tightListener struct {
	net.Listener
	writeTmo time.Duration
}

func (l *tightListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		if err := tc.SetWriteBuffer(tightBuf); err != nil {
			nc.Close()
			return nil, err
		}
	}
	if l.writeTmo == 0 {
		return nc, nil
	}
	return &tightConn{Conn: nc, writeTmo: l.writeTmo}, nil
}

// tightConn is one socket tightListener accepted.
type tightConn struct {
	net.Conn
	writeTmo time.Duration
}

func (c *tightConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		deadline = time.Now().Add(c.writeTmo)
	}
	return c.Conn.SetWriteDeadline(deadline)
}

// tightWrap is the wrap startPlanted takes for a tightListener.
func tightWrap(writeTmo time.Duration) func(net.Listener) net.Listener {
	return func(ln net.Listener) net.Listener {
		return &tightListener{Listener: ln, writeTmo: writeTmo}
	}
}

// dialTight is ts.dial with a tightBuf receive buffer, set before the
// connect so the window the host offers is small from the start.
func dialTight(t *testing.T, ts *testServer) *fakeHost {
	t.Helper()
	d := net.Dialer{
		Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			err := rc.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET,
					syscall.SO_RCVBUF, tightBuf)
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
	nc, err := d.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { nc.Close() })
	return newFakeHost(t, nc)
}

// sendLogRead sends one Get Log Page of length bytes at LPO 0 and returns
// its command id without reading the answer.
func sendLogRead(h *fakeHost, length uint32) uint16 {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcGetLogPage, cid)
	setSgl(s, length)
	s[40] = lidDiscovery
	numd := length/4 - 1
	binary.LittleEndian.PutUint16(s[42:44], uint16(numd))
	binary.LittleEndian.PutUint16(s[44:46], uint16(numd>>16))
	h.send(s, nil)
	return cid
}

// hostEntries is cnt entries of one subsystem pool each, from sp id first
// on, each listing one cntlr and naming hostNqn alone.
func hostEntries(env *plantEnv, first uint64, cnt int, hostNqn string) []plantedEntry {
	out := make([]plantedEntry, 0, cnt)
	for i := 0; i < cnt; i++ {
		spId := first + uint64(i)
		out = append(out, env.poolEntry(uint32(spId%common.ShardBucketSize),
			spId, 1, plantCntlrs(spId, 1), []string{hostNqn}))
	}
	return out
}

// waitDisconnected waits up to within for the `host disconnected` record of
// hostNqn and returns it.
func waitDisconnected(
	t *testing.T,
	logs *logCapture,
	hostNqn string,
	within time.Duration,
) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, rec := range logs.find(msgHostDisconnected) {
			if rec["hostnqn"] == hostNqn {
				return rec
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no `host disconnected` record for %s within %v", hostNqn, within)
	return nil
}

// TestHostThatStopsReadingMidLogPage proves NP13's write deadline: a host
// asks for its whole log, reads the first few KiB of the answer and then
// reads nothing more, so the controller's write of the rest blocks. The
// write ends at its deadline and the connection with it, as `closed`, the
// host's state is dropped (DS7) and the host gets no more of the page.
// Meanwhile a host that has stopped reading stalls neither the watcher nor
// another connection (DS6 -> NP11): after a change for the deaf host comes
// one for another host on the same instance, and that host's AEN arrives
// well within the deaf host's write deadline, which it could not if
// handling the first change had to wait for the deaf host's socket.
func TestHostThatStopsReadingMidLogPage(t *testing.T) {
	// writeTmo is long enough that a watcher held up by the deaf host's
	// socket would deliver the healthy host's AEN only after aenBy, and
	// aenBy, half of it, is a generous wait for an AEN on a loaded machine.
	const writeTmo = 8 * time.Second
	logs := captureLogs(t)
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	deafNqn, healthyNqn := env.hostNqn(0), env.hostNqn(1)
	const deafCnt = 64
	entries := hostEntries(env, 1, deafCnt, deafNqn)
	entries = append(entries, hostEntries(env, 1000, 1, healthyNqn)...)
	env.put(entries...)
	ts := startPlanted(t, cli, allRanges(), tightWrap(writeTmo))
	waitScanned(t, ts, len(entries), 10*time.Second)

	healthy := ts.dial()
	healthy.connectOk(healthyNqn)
	healthy.enableAen()
	healthyAer := armConfirmed(t, healthy)
	deaf := dialTight(t, ts)
	deaf.connectOk(deafNqn)
	deaf.enableAen()
	armConfirmed(t, deaf)

	total := uint32(common.CdcDiscLogHeaderSize +
		deafCnt*common.CdcDiscLogEntrySize)
	sendLogRead(deaf, total)
	head := make([]byte, 2*tightBuf)
	if err := deaf.nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := io.ReadFull(deaf.br, head); err != nil {
		t.Fatalf("the first bytes of the log page: %v", err)
	}
	if head[0] != pduC2HData {
		t.Fatalf("pdu type %#x, want the log page's c2h data", head[0])
	}
	// From here on the deaf host reads nothing.

	// A change for the deaf host, whose AEN waits behind its blocked page,
	// then one for the healthy host, which gets its AEN and its log at once.
	// The watcher handles the two in this order, so a watcher that wrote
	// the deaf host's AEN itself would be held until the deaf host's write
	// deadline.
	deafExtra := hostEntries(env, 2000, 1, deafNqn)[0]
	env.put(deafExtra)
	healthyExtra := hostEntries(env, 3000, 1, healthyNqn)[0]
	env.put(healthyExtra)
	aenBy := time.Now().Add(writeTmo / 2)
	entries = append(entries, deafExtra, healthyExtra)
	awaitAenBy(t, healthy, healthyAer, aenBy,
		"while another host stopped reading")
	checkLog(t, healthy, 2, newWantLog(entries, healthyNqn, ownsAll))

	rec := waitDisconnected(t, logs, deafNqn, writeTmo+20*time.Second)
	if rec["reason"] != reasonClosed {
		t.Errorf("reason %v, want %q: a write deadline ends a connection "+
			"like any other socket error", rec["reason"], reasonClosed)
	}
	srvWaitConns(t, ts, 1)
	if n := ts.reg.hostCount(); n != 1 {
		t.Errorf("registry tracks %d hosts after the deaf host's end, want "+
			"1", n)
	}
	// What the deaf host can still read is what sat in the buffers when
	// the controller gave up, and then the end of the stream: never the
	// whole page.
	if err := deaf.nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	rest, err := io.ReadAll(deaf.br)
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("the deaf host's socket is still open")
	}
	got := len(head) + len(rest)
	if full := c2hDataHdrLen + int(total); got >= full {
		t.Errorf("the deaf host received %d bytes, want fewer than the %d "+
			"of the whole page", got, full)
	}
	connStatus(t, healthy.keepAlive(), statusSuccess,
		"keep alive on the healthy host after the deaf host ended")
}

// TestSlowHostIsServedWhileOthersAreToo proves that a slow host, one that
// reads its log a KiB at a time and stops for a while in the middle, yet
// within the write deadline, costs nobody else anything: while its page is
// in flight, a change for another host is applied and delivered at once
// (DS6 -> NP11). The slow host then gets every byte of the page as the
// snapshot taken when its command arrived (DS9), its own AEN for the change
// it missed after the page, and it keeps its connection.
func TestSlowHostIsServedWhileOthersAreToo(t *testing.T) {
	logs := captureLogs(t)
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	slowNqn, fastNqn := env.hostNqn(0), env.hostNqn(1)
	const slowCnt = 255
	entries := hostEntries(env, 1, slowCnt, slowNqn)
	entries = append(entries, hostEntries(env, 1000, 1, fastNqn)...)
	env.put(entries...)
	ts := startPlanted(t, cli, allRanges(), tightWrap(0))
	waitScanned(t, ts, len(entries), 10*time.Second)

	fast := ts.dial()
	fast.connectOk(fastNqn)
	fast.enableAen()
	fastAer := armConfirmed(t, fast)
	slow := dialTight(t, ts)
	slow.connectOk(slowNqn)
	slow.enableAen()
	slowAer := armConfirmed(t, slow)

	total := uint32(common.CdcDiscLogHeaderSize +
		slowCnt*common.CdcDiscLogEntrySize)
	want := newWantLog(entries, slowNqn, ownsAll).all(t)
	logCid := sendLogRead(slow, total)

	// The slow reader takes exactly the page's C2HData PDU off the socket,
	// a KiB at a time, and stops after its first few KiB until resume.
	started := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	type pageRead struct {
		buf []byte
		err error
	}
	read := make(chan pageRead, 1)
	go func() {
		pduLen := c2hDataHdrLen + int(total)
		buf := make([]byte, 0, pduLen)
		piece := make([]byte, 1024)
		paused := false
		for len(buf) < pduLen {
			if len(buf) >= 4*tightBuf && !paused {
				paused = true
				close(started)
				select {
				case <-resume:
				case <-time.After(20 * time.Second):
				}
			}
			if err := slow.nc.SetReadDeadline(
				time.Now().Add(10 * time.Second),
			); err != nil {
				read <- pageRead{err: err}
				return
			}
			n, err := io.ReadFull(slow.br, piece[:min(len(piece),
				pduLen-len(buf))])
			buf = append(buf, piece[:n]...)
			if err != nil {
				read <- pageRead{buf: buf, err: err}
				return
			}
			time.Sleep(time.Millisecond)
		}
		read <- pageRead{buf: buf}
	}()
	select {
	case <-started:
	case r := <-read:
		t.Fatalf("the slow reader ended before its pause: %v", r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("the slow host received none of its page")
	}

	// While the slow host's page is in flight: first a change only the
	// slow host sees, then one only the fast host sees. The fast host's AEN
	// comes at once, which it could not if delivering the first had to
	// wait for the slow host's socket.
	slowExtra := hostEntries(env, 2000, 1, slowNqn)[0]
	env.put(slowExtra)
	fastExtra := hostEntries(env, 3000, 1, fastNqn)[0]
	env.put(fastExtra)
	entries = append(entries, slowExtra, fastExtra)
	awaitAen(t, fast, fastAer, "while another host's page is in flight")
	checkLog(t, fast, 2, newWantLog(entries, fastNqn, ownsAll))
	release()

	var r pageRead
	select {
	case r = <-read:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow host's page never completed")
	}
	if r.err != nil {
		t.Fatalf("the slow host's page: %v", r.err)
	}
	p, err := readCtrlPdu(bytes.NewReader(r.buf))
	if err != nil {
		t.Fatalf("the slow host's page: %v", err)
	}
	if p.typ != pduC2HData {
		t.Fatalf("pdu type %#x, want the page's c2h data", p.typ)
	}
	if g := binary.LittleEndian.Uint64(p.data[0:8]); g != 1 {
		t.Errorf("genctr %d in the page, want 1: the page is the snapshot "+
			"taken when the command arrived (DS9)", g)
	}
	if !bytes.Equal(p.data[common.CdcDiscLogHeaderSize:], want) {
		t.Error("the slow host's page is not the log it was served")
	}
	connStatus(t, slow.await(logCid), statusSuccess, "the slow host's page")
	awaitAen(t, slow, slowAer, "for the slow host after its page")
	checkLog(t, slow, 2, newWantLog(entries, slowNqn, ownsAll))
	connStatus(t, slow.keepAlive(), statusSuccess, "keep alive on the slow host")
	if n := logs.count(msgHostDisconnected); n != 0 {
		t.Errorf("%d `host disconnected` records, want 0: a slow host keeps "+
			"its connection", n)
	}
}

// ---------------------------------------------------------------------------
// The host NQN bounds (NP5, DS4)
// ---------------------------------------------------------------------------

// TestHostNqnAtItsLimits proves NP5's host NQN bounds at the cdc, through
// entries in etcd: a host NQN of exactly common.MaxNqnLength bytes connects,
// is logged whole, sees exactly the entries that name all of it (DS4: an
// exact match, so an entry naming its first 222 bytes is not its) and gets
// its AEN; one byte more is refused; a one byte host NQN connects and sees
// its entry; an empty one is refused.
func TestHostNqnAtItsLimits(t *testing.T) {
	logs := captureLogs(t)
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	prefix := fmt.Sprintf("nqn.2024-01.io.dnv.test:%016x:", env.cid)
	atLimit := prefix + strings.Repeat("a", common.MaxNqnLength-len(prefix))
	shorter := atLimit[:common.MaxNqnLength-1]
	// shortest is too short to carry the cluster id, so it is the one host
	// NQN every run of the case shares. Its log is compared whole with this
	// case's entries, so an entry of another cluster that names it fails
	// the case; it cannot make the case pass.
	const shortest = "h"
	entries := hostEntries(env, 1, 2, atLimit)
	entries = append(entries, hostEntries(env, 10, 1, shorter)...)
	entries = append(entries, hostEntries(env, 20, 1, shortest)...)
	env.put(entries...)
	ts := startPlanted(t, cli, allRanges(), nil)
	waitScanned(t, ts, len(entries), 10*time.Second)

	h := ts.dial()
	h.connectOk(atLimit)
	rec := logs.waitFor(t, msgHostConnected, 1)[0]
	if rec["hostnqn"] != atLimit {
		t.Errorf("`host connected` names %q, want all %d bytes of %q",
			rec["hostnqn"], len(atLimit), atLimit)
	}
	h.enableAen()
	checkLog(t, h, 1, newWantLog(entries, atLimit, ownsAll))
	aer := armConfirmed(t, h)
	more := hostEntries(env, 30, 1, atLimit)[0]
	env.put(more)
	entries = append(entries, more)
	awaitAen(t, h, aer, "for a host nqn at its limit")
	checkLog(t, h, 2, newWantLog(entries, atLimit, ownsAll))

	one := ts.dial()
	one.connectOk(shortest)
	checkLog(t, one, 1, newWantLog(entries, shortest, ownsAll))

	for _, tt := range []struct {
		name    string
		hostNqn string
	}{
		{"one byte over the limit", atLimit + "a"},
		{"empty", ""},
	} {
		r := ts.dial()
		r.handshake()
		c := connSend(r, connDefaults(tt.hostNqn))
		connStatus(t, c, statusConnectHost, "connect with a host nqn "+tt.name)
		if c.dw0 != connectIpo(connectDataHostNqnOff, true) {
			t.Errorf("%s: ipo %#x, want the host nqn's", tt.name, c.dw0)
		}
	}
	if n := ts.reg.hostCount(); n != 2 {
		t.Errorf("registry tracks %d hosts, want 2: a refused Connect leaves "+
			"no host state", n)
	}
}

// ---------------------------------------------------------------------------
// One page and a second page (NP8, DS9)
// ---------------------------------------------------------------------------

// TestLogOfExactlyOnePageAndOneMore proves NP8's transfer bound at its
// limit, where a page is the most one Get Log Page command moves: a log of
// exactly that size, the header and as many records as fill it, is read
// whole by one command. One more entry in etcd makes the log one record
// longer: one command for all of it is refused with invalid field, and the
// host reads it in two, at LPO 0 and at the bound, the second one past the
// end answered with zeros (DS9).
func TestLogOfExactlyOnePageAndOneMore(t *testing.T) {
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	hostNqn := env.hostNqn(0)
	perPage := (maxLogTransfer - common.CdcDiscLogHeaderSize) /
		common.CdcDiscLogEntrySize
	entries := hostEntries(env, 1, perPage, hostNqn)
	env.put(entries...)
	ts := startPlanted(t, cli, allRanges(), nil)
	waitScanned(t, ts, len(entries), 10*time.Second)

	h := ts.dial()
	h.connectOk(hostNqn)
	h.enableAen()
	if gen, num := logHeader(t, h); gen != 1 || num != uint64(perPage) {
		t.Fatalf("genctr %d numrec %d, want 1 and %d", gen, num, perPage)
	}
	c, page := logAt(h, maxLogTransfer, 0)
	connStatus(t, c, statusSuccess, "a log of exactly one page in one read")
	if len(page) != maxLogTransfer {
		t.Fatalf("one page moved %d bytes, want %d", len(page), maxLogTransfer)
	}
	want := newWantLog(entries, hostNqn, ownsAll).all(t)
	if !bytes.Equal(page[common.CdcDiscLogHeaderSize:], want) {
		t.Fatal("the one page is not the log")
	}

	aer := armConfirmed(t, h)
	extra := hostEntries(env, uint64(perPage+1), 1, hostNqn)[0]
	env.put(extra)
	entries = append(entries, extra)
	awaitAen(t, h, aer, "for the record that needs a second page")
	if gen, num := logHeader(t, h); gen != 2 || num != uint64(perPage+1) {
		t.Fatalf("genctr %d numrec %d, want 2 and %d", gen, num, perPage+1)
	}
	total := uint32(maxLogTransfer + common.CdcDiscLogEntrySize)
	c, _ = logAt(h, total, 0)
	connStatus(t, c, statusInvalidField,
		"a read of a log one record past one page")
	c, first := logAt(h, maxLogTransfer, 0)
	connStatus(t, c, statusSuccess, "the first page")
	c, second := logAt(h, maxLogTransfer, maxLogTransfer)
	connStatus(t, c, statusSuccess, "the second page")
	if len(first) != maxLogTransfer || len(second) != maxLogTransfer {
		t.Fatalf("pages of %d and %d bytes, want %d each", len(first),
			len(second), maxLogTransfer)
	}
	if g := binary.LittleEndian.Uint64(first[0:8]); g != 2 {
		t.Errorf("genctr %d in the first page, want 2", g)
	}
	want = newWantLog(entries, hostNqn, ownsAll).all(t)
	got := append(append([]byte{}, first[common.CdcDiscLogHeaderSize:]...),
		second[:common.CdcDiscLogEntrySize]...)
	if !bytes.Equal(got, want) {
		t.Error("the two pages are not the log")
	}
	if !bytes.Equal(second[common.CdcDiscLogEntrySize:],
		make([]byte, maxLogTransfer-common.CdcDiscLogEntrySize)) {
		t.Error("the second page past the log's end is not zeros")
	}
}

// ---------------------------------------------------------------------------
// The CNTLID counter's wrap (NP5)
// ---------------------------------------------------------------------------

// TestCntlIdWrapsWhileOldConnectionsLive pins the wrap of NP5's counter: it
// walks [1, common.CdcCntlIdMax] round robin and, past the wrap, hands out 1
// again while a live connection still holds 1, because NP5's counter
// withholds no id. A host's first connection gets 1; the counter is moved to
// one below the top of its range, as if every other id had been handed out
// and given back; another host gets the top id, and the first host's second
// connection gets 1 again. Both connections with id 1 are served on their
// own: each Identify carries the id its own Connect returned, which the
// Linux host checks, both get the AEN of one change, and the end of one
// leaves the other and the host's state in place (DS7).
func TestCntlIdWrapsWhileOldConnectionsLive(t *testing.T) {
	logs := captureLogs(t)
	cli := newTestClient(t)
	env := newPlantEnv(t, cli)
	hostA, hostB := env.hostNqn(0), env.hostNqn(1)
	cntlrs := plantCntlrs(1, common.MaxCntlrCntPerSp)
	entry := env.poolEntry(1, 1, 1, cntlrs, []string{hostA, hostB})
	entries := []plantedEntry{entry}
	env.put(entries...)
	ts := startPlanted(t, cli, allRanges(), nil)
	waitScanned(t, ts, len(entries), 10*time.Second)

	a1 := ts.dial()
	if id := a1.connectOk(hostA); id != 1 {
		t.Fatalf("the first cntlid is %d, want 1", id)
	}
	ts.srv.mu.Lock()
	ts.srv.cntlIdSeq = common.CdcCntlIdMax - 1
	ts.srv.mu.Unlock()
	b := ts.dial()
	if id := b.connectOk(hostB); id != common.CdcCntlIdMax {
		t.Fatalf("cntlid %d, want the top of the range %#x", id,
			common.CdcCntlIdMax)
	}
	a2 := ts.dial()
	if id := a2.connectOk(hostA); id != 1 {
		t.Fatalf("cntlid %d after the top of the range, want 1 again while "+
			"a live connection holds 1", id)
	}

	type watched struct {
		h       *fakeHost
		hostNqn string
		id      uint16
		aer     uint16
	}
	conns := []*watched{
		{h: a1, hostNqn: hostA, id: 1},
		{h: b, hostNqn: hostB, id: common.CdcCntlIdMax},
		{h: a2, hostNqn: hostA, id: 1},
	}
	for i, w := range conns {
		c, data := w.h.identify(cnsController)
		connStatus(t, c, statusSuccess, "identify")
		if id := binary.LittleEndian.Uint16(
			data[idOffCntlId : idOffCntlId+2]); id != w.id {
			t.Errorf("connection %d: identify cntlid %d, want its connect's "+
				"%d", i, id, w.id)
		}
		w.h.enableAen()
		checkLog(t, w.h, 1, newWantLog(entries, w.hostNqn, ownsAll))
		w.aer = armConfirmed(t, w.h)
	}
	next := env.poolEntry(1, 1, 1, failedOver(cntlrs), []string{hostA, hostB})
	env.put(next)
	entries[0] = next
	for i, w := range conns {
		awaitAen(t, w.h, w.aer, fmt.Sprintf("on connection %d", i))
	}

	a1.close()
	rec := logs.waitFor(t, msgHostDisconnected, 1)[0]
	if rec["hostnqn"] != hostA || rec["cntlid"] != uint64(1) {
		t.Fatalf("`host disconnected` for %v cntlid %v, want %s cntlid 1",
			rec["hostnqn"], rec["cntlid"], hostA)
	}
	if n := ts.reg.hostCount(); n != 2 {
		t.Errorf("registry tracks %d hosts after one of host A's two "+
			"connections ended, want 2", n)
	}
	connStatus(t, a2.keepAlive(), statusSuccess,
		"keep alive on the other connection with cntlid 1")
	checkLog(t, a2, 2, newWantLog(entries, hostA, ownsAll))
}
