package cdc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The NP3-NP13 tests: a real listener, the real codec and the in-process fake
// host of §8 on the other end of a loopback socket. Nothing is mocked between
// the test's bytes and the connection state machine, so every assertion here
// is one a Linux host would make.

// The two hostnqns the connection tests connect as.
const (
	connHostA = "nqn.2014-08.org.nvmexpress:uuid:11111111-1111-1111-1111-111111111111"
	connHostB = "nqn.2014-08.org.nvmexpress:uuid:22222222-2222-2222-2222-222222222222"
)

// ---------------------------------------------------------------------------
// Local helpers
// ---------------------------------------------------------------------------

// connArgs is one fabrics Connect, spelled out field by field: the shared
// helper always sends a well-formed one, and the NP5 rejects need the opposite.
type connArgs struct {
	recfmt  uint16
	qid     uint16
	sqSize  uint16
	cattr   uint8
	katoMs  uint32
	subNqn  string
	hostNqn string
	// hostNqnRaw, when non-empty, is written into the connect data instead of
	// hostNqn, so a test can send an unterminated over-long field.
	hostNqnRaw []byte
}

// connDefaults is the Connect a healthy host sends.
func connDefaults(hostNqn string) connArgs {
	return connArgs{
		sqSize:  common.CdcMaxAdminSqSize - 1,
		subNqn:  common.NvmeDiscoveryNqn,
		hostNqn: hostNqn,
	}
}

// connSend performs one Connect and returns its completion.
func connSend(h *fakeHost, a connArgs) completion {
	h.t.Helper()
	return h.await(connSendCmd(h, a))
}

// connSendCmd writes one Connect capsule and returns its command id without
// waiting: a Connect that earns a C2HTermReq never gets a completion.
func connSendCmd(h *fakeHost, a connArgs) uint16 {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcFabrics, cid)
	s[4] = fctypeConnect
	setSgl(s, connectDataLen)
	s[39] = 0x5<<4 | 0x1 // in-capsule data
	binary.LittleEndian.PutUint16(s[40:42], a.recfmt)
	binary.LittleEndian.PutUint16(s[42:44], a.qid)
	binary.LittleEndian.PutUint16(s[44:46], a.sqSize)
	s[46] = a.cattr
	binary.LittleEndian.PutUint32(s[48:52], a.katoMs)
	data := make([]byte, connectDataLen)
	copy(data[connectDataSubNqnOff:], a.subNqn)
	if len(a.hostNqnRaw) > 0 {
		copy(data[connectDataHostNqnOff:], a.hostNqnRaw)
	} else {
		copy(data[connectDataHostNqnOff:], a.hostNqn)
	}
	h.send(s, data)
	return cid
}

// connTermReq reads the C2HTermReq a protocol error is answered with (NP2) and
// returns its FES and FEI.
func connTermReq(t *testing.T, h *fakeHost) (uint16, uint32) {
	t.Helper()
	p, err := h.recv()
	if err != nil {
		t.Fatalf("expected a c2h term req: %v", err)
	}
	if p.typ != pduC2HTermReq {
		t.Fatalf("pdu type %#x, want a c2h term req", p.typ)
	}
	return binary.LittleEndian.Uint16(p.hdr[8:10]),
		binary.LittleEndian.Uint32(p.hdr[10:14])
}

// connSocketClosed asserts the controller has hung up (NP2, NP13). A read that
// merely ran out of time is NOT a closed socket — the assertion is that the
// peer ended the stream (EOF, or a reset), which is what a host observes when
// dnv-cdc terminates a connection; anything weaker passes on a controller that
// answers C2HTermReq and then keeps the socket open forever.
func connSocketClosed(t *testing.T, h *fakeHost) {
	t.Helper()
	if err := h.nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	p, err := readCtrlPdu(h.br)
	if err == nil {
		t.Fatalf("socket still open: read a pdu of type %#x", p.typ)
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("socket still open: the read timed out instead of ending")
	}
}

// connInject applies one entry change to the registry and performs the
// delivery half, which is exactly what the watcher does (WV3 -> DS6 -> NP11).
// A nil msg deletes the entry.
func connInject(ts *testServer, spId uint64, msg *pb.CdcEntry) {
	k := entryKey{cid: testCid, shard: 0, spId: spId, ssId: spId}
	var e *entry
	if msg != nil {
		e = newEntry(msg)
	}
	deliver(ts.reg.apply(context.Background(), k, e))
}

// connGetLog issues one Get Log Page with the exact LID, RAE, length and LPO
// the test wants — the shared helper always sends a well-formed aligned read
// (NP8).
func connGetLog(
	h *fakeHost,
	lid uint8,
	rae bool,
	length uint32,
	off uint64,
) (completion, []byte) {
	h.t.Helper()
	cid := h.allocCid()
	s := newSqe(opcGetLogPage, cid)
	setSgl(s, length)
	s[40] = lid
	if rae {
		s[41] = 0x80
	}
	numd := length/4 - 1
	binary.LittleEndian.PutUint16(s[42:44], uint16(numd))
	binary.LittleEndian.PutUint16(s[44:46], uint16(numd>>16))
	binary.LittleEndian.PutUint32(s[48:52], uint32(off))
	binary.LittleEndian.PutUint32(s[52:56], uint32(off>>32))
	h.send(s, nil)
	return h.await(cid), h.data[cid]
}

// connStatus fails the test unless the completion carries the wanted status.
func connStatus(t *testing.T, c completion, want uint16, what string) {
	t.Helper()
	if c.status != want {
		t.Fatalf("%s: status %#06x, want %#06x", what, c.status, want)
	}
}

// connEntry is the fixture CdcEntry the log-page tests serve.
func connEntry(nqn, addr, port string, allowed ...string) *pb.CdcEntry {
	return cdcEntry(nqn, allowed, tcpConf(addr, port))
}

// ---------------------------------------------------------------------------
// Handshake (NP3)
// ---------------------------------------------------------------------------

// TestHandshakeAnswersWithDigestsOff proves NP3: PFV echoed as 0, CPDA 0, BOTH
// digest bits off even though this host asked for both, and MAXH2CDATA =
// common.CdcMaxH2CData.
func TestHandshakeAnswersWithDigestsOff(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.write(buildICReqFor(pfv10, 0, digestHdrEnable|digestDataEnable))
	p, err := h.recv()
	if err != nil {
		t.Fatalf("icresp: %v", err)
	}
	if p.typ != pduICResp {
		t.Fatalf("pdu type %#x, want icresp", p.typ)
	}
	if pfv := binary.LittleEndian.Uint16(p.hdr[8:10]); pfv != pfv10 {
		t.Errorf("pfv %d, want %d", pfv, pfv10)
	}
	if cpda := p.hdr[10]; cpda != 0 {
		t.Errorf("cpda %d, want 0", cpda)
	}
	if dgst := p.hdr[11]; dgst != 0 {
		t.Errorf("dgst %#x, want 0: a controller enables only what both "+
			"sides support", dgst)
	}
	maxh2c := binary.LittleEndian.Uint32(p.hdr[12:16])
	if maxh2c != common.CdcMaxH2CData {
		t.Errorf("maxh2cdata %d, want %d", maxh2c, common.CdcMaxH2CData)
	}
}

// TestHandshakeRejects proves the NP3 refusals and the NP2 terminal path they
// take: a C2HTermReq, a closed socket and one `pdu error` record naming the
// remote and the reason.
func TestHandshakeRejects(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		fes   uint16
	}{
		{
			name:  "pfv is not 1.0",
			frame: buildICReqFor(1, 0, 0),
			fes:   fesUnsupportedParam,
		},
		{
			name:  "host demands pdu data alignment",
			frame: buildICReqFor(0, 4, 0),
			fes:   fesUnsupportedParam,
		},
		{
			name: "a capsule before the icreq",
			frame: pduFrame(
				pduCapsuleCmd, 0, capsuleCmdHdrLen, 0, capsuleCmdHdrLen,
			),
			fes: fesPduSequenceErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			ts := startServer(t)
			h := ts.dial()
			h.write(tt.frame)
			fes, _ := connTermReq(t, h)
			if fes != tt.fes {
				t.Errorf("fes %#x, want %#x", fes, tt.fes)
			}
			connSocketClosed(t, h)
			recs := logs.waitFor(t, msgPduError, 1)
			if recs[0]["remote"] == nil || recs[0]["remote"] == "" {
				t.Error("`pdu error` has no remote")
			}
			if recs[0]["reason"] == nil || recs[0]["reason"] == "" {
				t.Error("`pdu error` has no reason")
			}
			// A connection that never connected is nobody's host, so NP13
			// logs no disconnect for it.
			if n := logs.count(msgHostDisconnected); n != 0 {
				t.Errorf("%d `host disconnected` records, want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Connect (NP4, NP5)
// ---------------------------------------------------------------------------

// TestFirstCapsuleMustBeConnect proves NP4: the admin queue does not exist
// until Fabrics Connect has made it, so any other capsule first — and any
// in-capsule data on a command that is not Connect — is a protocol error.
func TestFirstCapsuleMustBeConnect(t *testing.T) {
	t.Run("identify before connect", func(t *testing.T) {
		ts := startServer(t)
		h := ts.dial()
		h.handshake()
		h.send(newSqe(opcIdentify, 1), nil)
		fes, _ := connTermReq(t, h)
		if fes != fesPduSequenceErr {
			t.Errorf("fes %#x, want %#x", fes, fesPduSequenceErr)
		}
		connSocketClosed(t, h)
	})
	t.Run("in-capsule data does not change the answer", func(t *testing.T) {
		ts := startServer(t)
		h := ts.dial()
		h.handshake()
		h.send(newSqe(opcIdentify, 1), make([]byte, 8))
		fes, _ := connTermReq(t, h)
		if fes != fesPduSequenceErr {
			t.Errorf("fes %#x, want %#x", fes, fesPduSequenceErr)
		}
		connSocketClosed(t, h)
	})
	t.Run("a second connect on one connection", func(t *testing.T) {
		ts := startServer(t)
		h := ts.dial()
		h.connectOk(connHostA)
		connSendCmd(h, connDefaults(connHostA))
		fes, _ := connTermReq(t, h)
		if fes != fesPduSequenceErr {
			t.Errorf("fes %#x, want %#x", fes, fesPduSequenceErr)
		}
	})
}

// TestConnectHappyPath proves NP5: a CNTLID out of the dynamic range, the
// connection registered under its hostnqn (DS7) and the `host connected`
// record of §7.
func TestConnectHappyPath(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	h.handshake()
	args := connDefaults(connHostA)
	args.katoMs = 30000
	c := connSend(h, args)
	connStatus(t, c, statusSuccess, "connect")
	cntlId := uint16(c.dw0)
	if cntlId < 1 || cntlId > common.CdcCntlIdMax {
		t.Fatalf("cntlid %d is outside [1, %d]", cntlId, common.CdcCntlIdMax)
	}
	if n := ts.reg.hostCount(); n != 1 {
		t.Fatalf("registry tracks %d hosts, want 1", n)
	}
	rec := logs.waitFor(t, msgHostConnected, 1)[0]
	if rec["hostnqn"] != connHostA {
		t.Errorf("hostnqn %v, want %v", rec["hostnqn"], connHostA)
	}
	if rec["cntlid"] != uint64(cntlId) {
		t.Errorf("cntlid %v, want %d", rec["cntlid"], cntlId)
	}
	if rec["kato_ms"] != int64(30000) {
		t.Errorf("kato_ms %v, want 30000", rec["kato_ms"])
	}
	if rec["remote"] == nil || rec["remote"] == "" {
		t.Error("`host connected` has no remote")
	}
}

// TestConnectRejects proves every NP5 refusal, each with the status and the
// error pointer the specs make a host read.
func TestConnectRejects(t *testing.T) {
	longNqn := bytes.Repeat([]byte("a"), common.MaxNqnLength+1)
	tests := []struct {
		name   string
		mutate func(*connArgs)
		status uint16
		ipo    uint32
	}{
		{
			name:   "record format is not 0",
			mutate: func(a *connArgs) { a.recfmt = 1 },
			status: statusConnectFormat,
			ipo:    connectIpo(connectCmdRecfmtOff, false),
		},
		{
			name:   "there are no i/o queues",
			mutate: func(a *connArgs) { a.qid = 1 },
			status: statusConnectParam,
			ipo:    connectIpo(connectCmdQidOff, false),
		},
		{
			name:   "sqsize 0",
			mutate: func(a *connArgs) { a.sqSize = 0 },
			status: statusConnectParam,
			ipo:    connectIpo(connectCmdSqsizeOff, false),
		},
		{
			name: "some other subsystem's nqn",
			mutate: func(a *connArgs) {
				a.subNqn = "nqn.2016-06.io.dnv:ss0"
			},
			status: statusConnectParam,
			ipo:    connectIpo(connectDataSubNqnOff, true),
		},
		{
			name:   "empty hostnqn",
			mutate: func(a *connArgs) { a.hostNqn = "" },
			status: statusConnectHost,
			ipo:    connectIpo(connectDataHostNqnOff, true),
		},
		{
			name:   "hostnqn over 223 bytes",
			mutate: func(a *connArgs) { a.hostNqnRaw = longNqn },
			status: statusConnectHost,
			ipo:    connectIpo(connectDataHostNqnOff, true),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := startServer(t)
			h := ts.dial()
			h.handshake()
			args := connDefaults(connHostA)
			tt.mutate(&args)
			c := connSend(h, args)
			connStatus(t, c, tt.status, "connect")
			if c.dw0 != tt.ipo {
				t.Errorf("ipo %#x, want %#x", c.dw0, tt.ipo)
			}
			// A refused Connect leaves no host state behind (DS7).
			if n := ts.reg.hostCount(); n != 0 {
				t.Errorf("registry tracks %d hosts, want 0", n)
			}
		})
	}
}

// TestConnectSqSizeClampedNotRefused proves the NP4 cap: a host that asks for
// a deeper admin queue than common.CdcMaxAdminSqSize is served with the
// controller's size, not refused — and SQHD then wraps modulo that size.
func TestConnectSqSizeClampedNotRefused(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.handshake()
	args := connDefaults(connHostA)
	args.sqSize = 1000
	c := connSend(h, args)
	connStatus(t, c, statusSuccess, "connect")
	if c.sqhd != 1 {
		t.Fatalf("connect sqhd %d, want 1", c.sqhd)
	}
	// The clamped queue is common.CdcMaxAdminSqSize entries deep, so the
	// head returns to 0 exactly that many commands after it left it.
	for i := 1; i < common.CdcMaxAdminSqSize; i++ {
		k := h.keepAlive()
		connStatus(t, k, statusSuccess, "keep alive")
		want := uint16((1 + i) % common.CdcMaxAdminSqSize)
		if k.sqhd != want {
			t.Fatalf("command %d: sqhd %d, want %d", i, k.sqhd, want)
		}
	}
}

// TestSqHeadWithAndWithoutFlowControl proves NP4's SQHD rule: maintained
// modulo SQSIZE + 1 by default, reported as 0xffff when the host set the
// Disable SQ Flow Control attribute at Connect.
func TestSqHeadWithAndWithoutFlowControl(t *testing.T) {
	t.Run("maintained and wrapping", func(t *testing.T) {
		ts := startServer(t)
		h := ts.dial()
		h.handshake()
		args := connDefaults(connHostA)
		args.sqSize = 3 // a four-entry queue
		c := connSend(h, args)
		connStatus(t, c, statusSuccess, "connect")
		want := []uint16{1, 2, 3, 0, 1, 2}
		if c.sqhd != want[0] {
			t.Fatalf("connect sqhd %d, want %d", c.sqhd, want[0])
		}
		for i, w := range want[1:] {
			k := h.keepAlive()
			if k.sqhd != w {
				t.Fatalf("command %d: sqhd %d, want %d", i+1, k.sqhd, w)
			}
		}
	})
	t.Run("disable sqflow reports 0xffff", func(t *testing.T) {
		ts := startServer(t)
		h := ts.dial()
		h.handshake()
		args := connDefaults(connHostA)
		args.cattr = connectDisableSqflow
		c := connSend(h, args)
		connStatus(t, c, statusSuccess, "connect")
		if c.sqhd != 0xffff {
			t.Fatalf("connect sqhd %#x, want 0xffff", c.sqhd)
		}
		for i := 0; i < 3; i++ {
			if k := h.keepAlive(); k.sqhd != 0xffff {
				t.Fatalf("command %d: sqhd %#x, want 0xffff", i, k.sqhd)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Properties (NP6)
// ---------------------------------------------------------------------------

// propValue64 reassembles the property a completion carries: dword 0 is the
// low half and dword 1 the high one.
func propValue64(c completion) uint64 {
	return uint64(c.dw0) | uint64(c.dw1)<<32
}

// TestPropertyDanceToReady walks the NP6 sequence a host performs between
// Connect and its first admin command: CAP, VS, CC, CSTS, then CC.EN and the
// CSTS.RDY that follows it.
func TestPropertyDanceToReady(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)

	c := h.propertyGet(regCap, true)
	connStatus(t, c, statusSuccess, "property get cap")
	cap := propValue64(c)
	if mqes := cap & 0xffff; mqes != common.CdcMaxAdminSqSize-1 {
		t.Errorf("cap.mqes %d, want %d", mqes, common.CdcMaxAdminSqSize-1)
	}
	if cap&(1<<16) == 0 {
		t.Error("cap.cqr is clear; this controller needs contiguous queues")
	}
	if to := (cap >> 24) & 0xff; to == 0 {
		t.Error("cap.to is 0; a host would give up before CSTS.RDY")
	}
	if css := (cap >> 37) & 0xff; css&1 == 0 {
		t.Errorf("cap.css %#x does not advertise the nvm command set", css)
	}

	c = h.propertyGet(regVs, false)
	connStatus(t, c, statusSuccess, "property get vs")
	if propValue64(c) != controllerVersion {
		t.Errorf("vs %#x, want %#x", propValue64(c), controllerVersion)
	}
	if c := h.propertyGet(regCc, false); propValue64(c) != 0 {
		t.Errorf("cc %#x before enable, want 0", propValue64(c))
	}
	if c := h.propertyGet(regCsts, false); propValue64(c) != 0 {
		t.Errorf("csts %#x before enable, want 0", propValue64(c))
	}

	// EN 0 -> 1: the controller becomes ready.
	connStatus(t, h.propertySet(regCc, ccEnable), statusSuccess, "set cc.en")
	csts := propValue64(h.propertyGet(regCsts, false))
	if csts&cstsReady == 0 {
		t.Fatalf("csts %#x: RDY did not follow CC.EN", csts)
	}
	if cc := propValue64(h.propertyGet(regCc, false)); cc != ccEnable {
		t.Errorf("cc %#x, want %#x", cc, ccEnable)
	}

	// CC.SHN: shutdown complete, and no longer ready.
	shn := uint64(ccEnable | 1<<ccShnShift)
	connStatus(t, h.propertySet(regCc, shn), statusSuccess, "set cc.shn")
	csts = propValue64(h.propertyGet(regCsts, false))
	if csts&cstsShstMask != cstsShstComplet {
		t.Errorf("csts %#x: SHST is not `shutdown complete`", csts)
	}
	if csts&cstsReady != 0 {
		t.Errorf("csts %#x: RDY survived the shutdown notice", csts)
	}
}

// TestPropertyEnableThenDisableClearsReady proves the other NP6 transition:
// CC.EN 1 -> 0 marks the controller not ready and leaves teardown to the
// socket.
func TestPropertyEnableThenDisableClearsReady(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	connStatus(t, h.propertySet(regCc, ccEnable), statusSuccess, "set cc.en")
	if csts := propValue64(h.propertyGet(regCsts, false)); csts&cstsReady == 0 {
		t.Fatalf("csts %#x: RDY did not follow CC.EN", csts)
	}
	connStatus(t, h.propertySet(regCc, 0), statusSuccess, "clear cc.en")
	if csts := propValue64(h.propertyGet(regCsts, false)); csts&cstsReady != 0 {
		t.Fatalf("csts %#x: RDY survived CC.EN 1->0", csts)
	}
	if n := ts.srv.connCount(); n != 1 {
		t.Fatalf("%d connections, want 1: CC.EN 1->0 does not close the "+
			"socket", n)
	}
}

// TestPropertyRejects proves NP6's refusals: CAP is an 8 byte property and
// nothing else is, CC is the only writable one, and every refusal is an
// invalid field with DNR set.
func TestPropertyRejects(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	t.Run("cap in the 4 byte form", func(t *testing.T) {
		c := h.propertyGet(regCap, false)
		connStatus(t, c, statusInvalidField, "property get cap (narrow)")
	})
	t.Run("an unknown property offset", func(t *testing.T) {
		c := h.propertyGet(0x30, false)
		connStatus(t, c, statusInvalidField, "property get 0x30")
	})
	t.Run("csts in the 8 byte form", func(t *testing.T) {
		c := h.propertyGet(regCsts, true)
		connStatus(t, c, statusInvalidField, "property get csts (wide)")
	})
	t.Run("property set to vs", func(t *testing.T) {
		c := h.propertySet(regVs, 1)
		connStatus(t, c, statusInvalidField, "property set vs")
		if c.status>>15 == 0 {
			t.Error("a refused property must carry DNR")
		}
	})
	t.Run("property set to csts", func(t *testing.T) {
		c := h.propertySet(regCsts, 1)
		connStatus(t, c, statusInvalidField, "property set csts")
	})
}

// ---------------------------------------------------------------------------
// Identify (NP7)
// ---------------------------------------------------------------------------

// The Identify Controller offsets NP7 pins, spelled as the specs spell them so
// the test does not merely agree with the implementation's own constants.
const (
	idcOffModel     = 24
	idcOffMdts      = 77
	idcOffCntlId    = 78
	idcOffOaes      = 92
	idcOffCntrlType = 111
	idcOffAerl      = 259
	idcOffLpa       = 261
	idcOffKas       = 320
	idcOffSgls      = 536
	idcOffSubNqn    = 768
)

// TestIdentifyController proves NP7 field by field. CNTLID is the one a host
// refuses the controller over: it must equal the id Connect returned.
//
// A decoy connection goes first on purpose. The round-robin counter hands the
// FIRST connection of an instance CNTLID 1, so an Identify that reported a
// constant — or the wrong connection's id — would satisfy this test unnoticed;
// the host under test is therefore never the one holding id 1, and the decoy's
// own Identify is checked afterwards to prove the field is per connection.
func TestIdentifyController(t *testing.T) {
	ts := startServer(t)
	decoy := ts.dial()
	decoyId := decoy.connectOk(connHostB)
	h := ts.dial()
	cntlId := h.connectOk(connHostA)
	if cntlId == decoyId {
		t.Fatalf("both connections were given cntlid %d", cntlId)
	}
	if cntlId == 1 {
		t.Fatalf("the host under test holds cntlid 1, which makes the "+
			"assertion below vacuous (decoy has %d)", decoyId)
	}
	c, data := h.identify(cnsController)
	connStatus(t, c, statusSuccess, "identify cns 01h")
	if len(data) != identifyLen {
		t.Fatalf("identify data is %d bytes, want %d", len(data), identifyLen)
	}
	// The literal 2 on purpose: 1 is an I/O controller and 3 an
	// administrative one, and the Linux host renders this byte into
	// /sys/class/nvme/nvmeN/cntrltype, which the stock nvmf autoconnect
	// rule matches as `discovery`.
	if got := data[idcOffCntrlType]; got != 2 {
		t.Errorf("cntrltype %d, want 2 (discovery controller)", got)
	}
	if cntrlTypeDiscovery != 2 {
		t.Errorf("cntrlTypeDiscovery is %d, want 2", cntrlTypeDiscovery)
	}
	if got := data[idcOffMdts]; got != 0 {
		t.Errorf("mdts %d, want 0", got)
	}
	idCntlId := binary.LittleEndian.Uint16(data[idcOffCntlId : idcOffCntlId+2])
	if idCntlId != cntlId {
		t.Errorf("cntlid %d, want %d (the id Connect returned)",
			idCntlId, cntlId)
	}
	if kas := binary.LittleEndian.Uint16(data[idcOffKas : idcOffKas+2]); kas != 10 {
		t.Errorf("kas %d, want 10 (100 ms units)", kas)
	}
	if got := data[idcOffAerl]; got != common.CdcAerl {
		t.Errorf("aerl %d, want %d", got, common.CdcAerl)
	}
	oaes := binary.LittleEndian.Uint32(data[idcOffOaes : idcOffOaes+4])
	if oaes&(1<<31) == 0 {
		t.Errorf("oaes %#x: the discovery log page change bit is clear, so "+
			"no kernel would ever enable the notice", oaes)
	}
	if lpa := data[idcOffLpa]; lpa&(1<<2) == 0 {
		t.Errorf("lpa %#x: extended NUMD and LPO are not advertised", lpa)
	}
	sgls := binary.LittleEndian.Uint32(data[idcOffSgls : idcOffSgls+4])
	if sgls&(1<<0) == 0 || sgls&(1<<20) == 0 {
		t.Errorf("sgls %#x, want bits 0 and 20 set", sgls)
	}
	subNqn := string(bytes.TrimRight(
		data[idcOffSubNqn:idcOffSubNqn+256], "\x00",
	))
	if subNqn != common.NvmeDiscoveryNqn {
		t.Errorf("subnqn %q, want %q", subNqn, common.NvmeDiscoveryNqn)
	}
	model := string(bytes.TrimRight(data[idcOffModel:idcOffModel+40], " "))
	if model != "dnv" {
		t.Errorf("mn %q, want %q", model, "dnv")
	}

	// The other live connection is told its own id by the same instance.
	_, decoyData := decoy.identify(cnsController)
	decoyReported := binary.LittleEndian.Uint16(
		decoyData[idcOffCntlId : idcOffCntlId+2],
	)
	if decoyReported != decoyId {
		t.Errorf("the decoy's identify reports cntlid %d, want %d: the "+
			"field is per connection", decoyReported, decoyId)
	}
}

// TestIdentifyOtherCnsRefused proves NP7's other half: CNS 01h is the only one
// a discovery controller answers.
func TestIdentifyOtherCnsRefused(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	for _, cns := range []uint8{0x00, 0x02, 0x03, 0x13} {
		c, data := h.identify(cns)
		connStatus(t, c, statusInvalidField, "identify")
		if len(data) != 0 {
			t.Errorf("cns %#x: %d bytes of data on a refused command",
				cns, len(data))
		}
	}
}

// ---------------------------------------------------------------------------
// Get Log Page (NP8, DS9)
// ---------------------------------------------------------------------------

// TestGetLogPagePagedReads proves the DS9 layout and NP8's paging against an
// injected view: the header block first, one entry per record after it, and
// zeros past the end.
func TestGetLogPagePagedReads(t *testing.T) {
	ts := startServer(t)
	// Two entries, injected before the host connects, so its view is
	// rendered at attach with GENCTR 1 (DS7).
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	connInject(ts, 2, connEntry("nqn.2016-06.io.dnv:ss1", "10.0.0.2", "4421"))
	h := ts.dial()
	h.connectOk(connHostA)

	const total = common.CdcDiscLogHeaderSize + 2*common.CdcDiscLogEntrySize

	t.Run("header only", func(t *testing.T) {
		c, data := connGetLog(h, lidDiscovery, false,
			common.CdcDiscLogHeaderSize, 0)
		connStatus(t, c, statusSuccess, "get log page")
		if len(data) != common.CdcDiscLogHeaderSize {
			t.Fatalf("%d bytes, want %d", len(data),
				common.CdcDiscLogHeaderSize)
		}
		if genCtr := binary.LittleEndian.Uint64(data[0:8]); genCtr != 1 {
			t.Errorf("genctr %d, want 1", genCtr)
		}
		if numRec := binary.LittleEndian.Uint64(data[8:16]); numRec != 2 {
			t.Errorf("numrec %d, want 2", numRec)
		}
		if recFmt := binary.LittleEndian.Uint16(data[16:18]); recFmt != 0 {
			t.Errorf("recfmt %d, want 0", recFmt)
		}
	})

	// The full read is the reference every paged read is compared against.
	full := func() []byte {
		c, data := connGetLog(h, lidDiscovery, false, total, 0)
		connStatus(t, c, statusSuccess, "full get log page")
		if len(data) != total {
			t.Fatalf("%d bytes, want %d", len(data), total)
		}
		return data
	}()

	t.Run("entry fields", func(t *testing.T) {
		e0 := full[common.CdcDiscLogHeaderSize : common.CdcDiscLogHeaderSize+
			common.CdcDiscLogEntrySize]
		if e0[0] != trTypeTcp {
			t.Errorf("trtype %d, want %d", e0[0], trTypeTcp)
		}
		if e0[1] != adrFamIpv4 {
			t.Errorf("adrfam %d, want %d", e0[1], adrFamIpv4)
		}
		asqsz := binary.LittleEndian.Uint16(e0[8:10])
		if asqsz != common.CdcMaxAdminSqSize {
			t.Errorf("asqsz %d, want %d", asqsz, common.CdcMaxAdminSqSize)
		}
		nqn := string(bytes.TrimRight(e0[256:512], "\x00"))
		if nqn != "nqn.2016-06.io.dnv:ss0" {
			t.Errorf("subnqn %q, want the first entry's", nqn)
		}
		addr := string(bytes.TrimRight(e0[512:768], "\x00"))
		if addr != "10.0.0.1" {
			t.Errorf("traddr %q, want 10.0.0.1", addr)
		}
	})

	t.Run("aligned page at a non-zero lpo", func(t *testing.T) {
		off := uint64(common.CdcDiscLogHeaderSize)
		c, data := connGetLog(h, lidDiscovery, false,
			common.CdcDiscLogEntrySize, off)
		connStatus(t, c, statusSuccess, "get log page at lpo")
		if !bytes.Equal(data, full[off:off+common.CdcDiscLogEntrySize]) {
			t.Error("the page at lpo 1024 is not the first entry")
		}
	})

	t.Run("a page straddling two entries", func(t *testing.T) {
		off := uint64(common.CdcDiscLogHeaderSize + 512)
		c, data := connGetLog(h, lidDiscovery, false, 1024, off)
		connStatus(t, c, statusSuccess, "get log page straddling")
		if !bytes.Equal(data, full[off:off+1024]) {
			t.Error("a straddling page did not match the full read")
		}
	})

	t.Run("the 4096 byte read a real host does", func(t *testing.T) {
		c, data := connGetLog(h, lidDiscovery, false, 4096, 0)
		connStatus(t, c, statusSuccess, "get log page 4096")
		if !bytes.Equal(data[:total], full) {
			t.Error("the first 3072 bytes are not the log")
		}
		if !bytes.Equal(data[total:], make([]byte, 4096-total)) {
			t.Error("the bytes past the end are not zeros")
		}
	})

	t.Run("a read entirely past the end", func(t *testing.T) {
		c, data := connGetLog(h, lidDiscovery, false, 1024, 8192)
		connStatus(t, c, statusSuccess, "get log page past the end")
		if !bytes.Equal(data, make([]byte, 1024)) {
			t.Error("a read past the end must return zeros")
		}
	})
}

// TestGetLogPageRejects proves NP8's refusals, all of them invalid field with
// DNR set — the same answers nvmet's discovery parser gives.
func TestGetLogPageRejects(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	t.Run("another log page identifier", func(t *testing.T) {
		for _, lid := range []uint8{0x00, 0x01, 0x02, 0x71} {
			c, _ := connGetLog(h, lid, false, 1024, 0)
			connStatus(t, c, statusInvalidField, "get log page")
		}
	})
	t.Run("an offset that is not dword aligned", func(t *testing.T) {
		c, _ := connGetLog(h, lidDiscovery, false, 1024, 2)
		connStatus(t, c, statusInvalidField, "get log page at lpo 2")
	})
	t.Run("a transfer beyond what one command may move", func(t *testing.T) {
		c, _ := connGetLog(h, lidDiscovery, false, maxLogTransfer+4, 0)
		connStatus(t, c, statusInvalidField, "oversized get log page")
	})
}

// TestTransfersHonorTheSglLength proves the bound every controller-to-host
// transfer is under (NP7, NP8): the SGL descriptor is the host's buffer, so a
// command whose CNS or NUMD asks for more than it describes is served the
// SGL's length and not one byte more. A controller that ignored it would
// overrun a real host's buffer, which no status code can undo.
func TestTransfersHonorTheSglLength(t *testing.T) {
	ts := startServer(t)
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	h := ts.dial()
	h.connectOk(connHostA)

	t.Run("identify into a short buffer", func(t *testing.T) {
		const buffer = 512
		cid := h.allocCid()
		s := newSqe(opcIdentify, cid)
		setSgl(s, buffer)
		s[40] = cnsController
		h.send(s, nil)
		connStatus(t, h.await(cid), statusSuccess, "identify")
		if n := len(h.data[cid]); n != buffer {
			t.Fatalf("identify moved %d bytes into a %d byte buffer",
				n, buffer)
		}
	})

	t.Run("get log page whose numd exceeds its sgl", func(t *testing.T) {
		const buffer = common.CdcDiscLogHeaderSize
		cid := h.allocCid()
		s := newSqe(opcGetLogPage, cid)
		setSgl(s, buffer)
		s[40] = lidDiscovery
		// NUMD asks for 4096 bytes; the descriptor describes 1024.
		numd := uint32(4096/4 - 1)
		binary.LittleEndian.PutUint16(s[42:44], uint16(numd))
		binary.LittleEndian.PutUint16(s[44:46], uint16(numd>>16))
		h.send(s, nil)
		connStatus(t, h.await(cid), statusSuccess, "get log page")
		if n := len(h.data[cid]); n != buffer {
			t.Fatalf("get log page moved %d bytes into a %d byte buffer",
				n, buffer)
		}
		genCtr := binary.LittleEndian.Uint64(h.data[cid][0:8])
		if genCtr == 0 {
			t.Error("the truncated read is not the head of the log page")
		}
	})
}

// TestGetLogPageRaeIsAcceptedAndIgnored proves NP8's RAE rule: event clearing
// is delivery-based (§0 #7), so the bit changes neither the answer nor the
// connection's pending state.
func TestGetLogPageRaeIsAcceptedAndIgnored(t *testing.T) {
	ts := startServer(t)
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	h := ts.dial()
	h.connectOk(connHostA)
	cRae, withRae := connGetLog(h, lidDiscovery, true, 2048, 0)
	connStatus(t, cRae, statusSuccess, "get log page with rae")
	cNoRae, withoutRae := connGetLog(h, lidDiscovery, false, 2048, 0)
	connStatus(t, cNoRae, statusSuccess, "get log page without rae")
	if !bytes.Equal(withRae, withoutRae) {
		t.Fatal("RAE changed the log page content")
	}
}

// TestGetLogPageServesASnapshot proves DS9: the GENCTR a read reports is the
// one the command arrived with, and an impact between two reads shows up in
// the second.
func TestGetLogPageServesASnapshot(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	_, first := connGetLog(h, lidDiscovery, false, 1024, 0)
	if genCtr := binary.LittleEndian.Uint64(first[0:8]); genCtr != 1 {
		t.Fatalf("genctr %d before any change, want 1", genCtr)
	}
	if numRec := binary.LittleEndian.Uint64(first[8:16]); numRec != 0 {
		t.Fatalf("numrec %d on an empty view, want 0", numRec)
	}
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	_, second := connGetLog(h, lidDiscovery, false, 2048, 0)
	if genCtr := binary.LittleEndian.Uint64(second[0:8]); genCtr != 2 {
		t.Fatalf("genctr %d after one impact, want 2", genCtr)
	}
	if numRec := binary.LittleEndian.Uint64(second[8:16]); numRec != 1 {
		t.Fatalf("numrec %d, want 1", numRec)
	}
}

// TestLogPageIsFilteredPerHost proves DS4/DS5 through the socket, which is the
// reason this controller exists at all (§0 #1): two hosts connected to one
// instance are served different logs, and an entry with an empty
// allowed_hosts is served to both.
func TestLogPageIsFilteredPerHost(t *testing.T) {
	ts := startServer(t)
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:shared", "10.0.0.1", "4420"))
	connInject(ts, 2, connEntry(
		"nqn.2016-06.io.dnv:bonly", "10.0.0.2", "4421", connHostB,
	))
	a := ts.dial()
	b := ts.dial()
	a.connectOk(connHostA)
	b.connectOk(connHostB)

	_, aLog := connGetLog(a, lidDiscovery, false, 4096, 0)
	if numRec := binary.LittleEndian.Uint64(aLog[8:16]); numRec != 1 {
		t.Fatalf("host A sees %d records, want 1", numRec)
	}
	aNqn := string(bytes.TrimRight(aLog[1024+256:1024+512], "\x00"))
	if aNqn != "nqn.2016-06.io.dnv:shared" {
		t.Errorf("host A's only record is %q, want the shared entry", aNqn)
	}

	_, bLog := connGetLog(b, lidDiscovery, false, 4096, 0)
	if numRec := binary.LittleEndian.Uint64(bLog[8:16]); numRec != 2 {
		t.Fatalf("host B sees %d records, want 2", numRec)
	}
	bNqn := string(bytes.TrimRight(bLog[2048+256:2048+512], "\x00"))
	if bNqn != "nqn.2016-06.io.dnv:bonly" {
		t.Errorf("host B's second record is %q, want the entry allowed to "+
			"it alone", bNqn)
	}
}

// ---------------------------------------------------------------------------
// Features (NP9)
// ---------------------------------------------------------------------------

// TestFeaturesAenConfigGatesAens proves NP9 and §0 #8: an impact on a host
// that has not enabled the discovery-log-change notice delivers nothing, and
// enabling it afterwards releases the AEN that was already pending.
func TestFeaturesAenConfigGatesAens(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	cntlId := h.connectOk(connHostA)
	aer := h.armAer()
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))

	// The default configuration is 0, so nothing may be delivered. There is
	// no event to wait for here — the assertion is that none arrives.
	if c, ok := h.pollPending(200 * time.Millisecond); ok {
		t.Fatalf("an AEN was delivered before the host enabled it: %+v", c)
	}

	// A configuration that enables some other notice is not this one: only
	// the discovery-log-change bit gates DS8 delivery.
	connStatus(t, h.setFeatures(fidAsyncEventConfig, 1), statusSuccess,
		"set features 0bh")
	if c, ok := h.pollPending(200 * time.Millisecond); ok {
		t.Fatalf("an AEN was delivered for an unrelated notice bit: %+v", c)
	}

	h.enableAen()
	c := h.await(aer)
	connStatus(t, c, statusSuccess, "aen")
	if c.dw0 != aenDiscLogChanged {
		t.Fatalf("aen dword0 %#08x, want %#08x", c.dw0, aenDiscLogChanged)
	}
	if c.dw0 != 0x0070f002 {
		t.Fatalf("aen dword0 %#08x, want 0x0070f002 (NVME_AEN=0x70f002)",
			c.dw0)
	}
	rec := logs.waitFor(t, msgAenSent, 1)[0]
	if rec["hostnqn"] != connHostA {
		t.Errorf("`aen sent` hostnqn %v, want %v", rec["hostnqn"], connHostA)
	}
	if rec["cntlid"] != uint64(cntlId) {
		t.Errorf("`aen sent` cntlid %v, want %d", rec["cntlid"], cntlId)
	}
	if rec["genctr"] != uint64(2) {
		t.Errorf("`aen sent` genctr %v, want 2", rec["genctr"])
	}
}

// TestFeaturesReadBackAndReject proves the rest of NP9: 0Bh reads back what
// was set, 0Fh updates the KATO, and every other feature identifier is an
// invalid field with DNR.
func TestFeaturesReadBackAndReject(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)

	t.Run("0bh round trip", func(t *testing.T) {
		c := h.setFeatures(fidAsyncEventConfig, aenCfgDiscChange)
		connStatus(t, c, statusSuccess, "set features 0bh")
		g := h.getFeatures(fidAsyncEventConfig)
		connStatus(t, g, statusSuccess, "get features 0bh")
		if g.dw0 != aenCfgDiscChange {
			t.Fatalf("aen config %#x, want %#x", g.dw0, uint32(aenCfgDiscChange))
		}
	})
	t.Run("0fh updates the kato", func(t *testing.T) {
		c := h.setFeatures(fidKeepAliveTimer, 12000)
		connStatus(t, c, statusSuccess, "set features 0fh")
		if c.dw0 != 12000 {
			t.Errorf("set features 0fh dword0 %d, want 12000", c.dw0)
		}
		g := h.getFeatures(fidKeepAliveTimer)
		connStatus(t, g, statusSuccess, "get features 0fh")
		if g.dw0 != 12000 {
			t.Fatalf("kato %d ms, want 12000", g.dw0)
		}
	})
	t.Run("any other feature identifier", func(t *testing.T) {
		for _, fid := range []uint8{0x01, 0x02, 0x07, 0x80} {
			connStatus(t, h.setFeatures(fid, 1), statusInvalidField,
				"set features")
			connStatus(t, h.getFeatures(fid), statusInvalidField,
				"get features")
		}
	})
}

// ---------------------------------------------------------------------------
// Asynchronous events (NP11, DS8)
// ---------------------------------------------------------------------------

// TestAerArmedThenImpacted proves the first NP11 order: an AER waits, the
// impact arrives, the AER completes with the DS8 value.
func TestAerArmedThenImpacted(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	h.enableAen()
	aer := h.armAer()
	// One round trip before the impact. The reader goroutine handles capsules
	// in order, so a completed Keep Alive proves the AER is already armed —
	// without it this test would silently be the impact-then-arm one below.
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive")
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	c := h.await(aer)
	connStatus(t, c, statusSuccess, "aen")
	if c.dw0 != aenDiscLogChanged {
		t.Fatalf("aen dword0 %#08x, want %#08x", c.dw0, aenDiscLogChanged)
	}
}

// TestAerImpactedThenArmedCompletesImmediately proves the other NP11 order:
// the impact was recorded in the pending bit while nothing was armed, and the
// next AER is completed the moment it arrives.
func TestAerImpactedThenArmedCompletesImmediately(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	h.enableAen()
	connInject(ts, 1, connEntry("nqn.2016-06.io.dnv:ss0", "10.0.0.1", "4420"))
	aer := h.armAer()
	c := h.await(aer)
	connStatus(t, c, statusSuccess, "aen")
	if c.dw0 != aenDiscLogChanged {
		t.Fatalf("aen dword0 %#08x, want %#08x", c.dw0, aenDiscLogChanged)
	}
}

// TestAerImpactsCoalesce proves §0 #7: several impacts before a delivery are
// one pending bit, so the host gets exactly one AEN and re-reads the log once.
func TestAerImpactsCoalesce(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	h.enableAen()
	for i, nqn := range []string{
		"nqn.2016-06.io.dnv:ss0",
		"nqn.2016-06.io.dnv:ss1",
		"nqn.2016-06.io.dnv:ss2",
	} {
		connInject(ts, uint64(i+1), connEntry(nqn, "10.0.0.1", "4420"))
	}
	first := h.armAer()
	c := h.await(first)
	connStatus(t, c, statusSuccess, "aen")
	if c.dw0 != aenDiscLogChanged {
		t.Fatalf("aen dword0 %#08x, want %#08x", c.dw0, aenDiscLogChanged)
	}
	// A second AER must sit there: the three impacts were one event.
	h.armAer()
	if extra, ok := h.pollPending(200 * time.Millisecond); ok {
		t.Fatalf("a second AEN was delivered for coalesced impacts: %+v",
			extra)
	}
	// The GENCTR moved once per impact even though one AEN was sent: the
	// host re-reads and sees the newest state (DS6).
	_, data := connGetLog(h, lidDiscovery, false, 1024, 0)
	if genCtr := binary.LittleEndian.Uint64(data[0:8]); genCtr != 4 {
		t.Fatalf("genctr %d after three impacts, want 4", genCtr)
	}
}

// TestAerLimitExceeded proves NP11's depth: common.CdcAerl + 1 outstanding
// AERs are accepted and the next one is refused with the AER-limit status.
func TestAerLimitExceeded(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	for i := 0; i < common.CdcAerl+1; i++ {
		h.armAer()
	}
	// Nothing may have completed: there has been no impact.
	if c, ok := h.pollPending(100 * time.Millisecond); ok {
		t.Fatalf("an AER completed without an impact: %+v", c)
	}
	over := h.armAer()
	c := h.await(over)
	connStatus(t, c, statusAsyncLimit, "the AERL+2nd async event request")
	if c.status>>15 == 0 {
		t.Error("the AER-limit status must carry DNR")
	}
}

// TestAersNeverCompleteOnDisconnect proves the last sentence of NP11: the
// outstanding AER queue dies with the socket, unanswered.
func TestAersNeverCompleteOnDisconnect(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	h.enableAen()
	h.armAer()
	// One round trip after arming: the AER is registered by the time the
	// keep-alive completion comes back, because one reader goroutine handles
	// both in order.
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive")
	ts.stop()
	if c, ok := h.pollPending(time.Second); ok {
		t.Fatalf("an AER completed on disconnect: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// Keep alive (NP10)
// ---------------------------------------------------------------------------

// connKato is the KATO the keep-alive tests connect with, and connKatoExpiry
// the deadline NP10 gives it.
const (
	connKato       = 5 * time.Second
	connKatoExpiry = connKato +
		common.DefaultCdcKeepAliveGraceMs*time.Millisecond
)

// connConnectWithKato connects with a KATO and waits until the keep-alive loop
// has armed the timer for it, so the fake clock cannot be advanced past a
// deadline that is not there yet.
func connConnectWithKato(
	t *testing.T,
	ts *testServer,
	h *fakeHost,
	katoMs uint32,
) {
	t.Helper()
	h.handshake()
	args := connDefaults(connHostA)
	args.katoMs = katoMs
	connStatus(t, connSend(h, args), statusSuccess, "connect")
	// Two timers: the one armed before Connect on the zero-KATO budget, and
	// the one the connection re-armed once it knew its KATO.
	ts.clk.waitWaiters(t, 2)
}

// TestKeepAliveExpiresAtKatoPlusGrace proves NP10: a KATO > 0 connection
// survives to its deadline and is reaped past it, with the `host disconnected`
// reason of §7.
func TestKeepAliveExpiresAtKatoPlusGrace(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	connConnectWithKato(t, ts, h, uint32(connKato.Milliseconds()))

	// One millisecond short of the deadline the connection is still there
	// and still answering.
	ts.clk.advance(connKatoExpiry - time.Millisecond)
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive before expiry")

	// The keep-alive above restarted the timer, so the deadline moved with
	// it: a full budget later the connection is reaped.
	ts.clk.advance(connKatoExpiry)
	rec := logs.waitFor(t, msgHostDisconnected, 1)[0]
	if rec["reason"] != reasonKeepAlive {
		t.Fatalf("reason %v, want %v", rec["reason"], reasonKeepAlive)
	}
	if rec["hostnqn"] != connHostA {
		t.Errorf("hostnqn %v, want %v", rec["hostnqn"], connHostA)
	}
	connSocketClosed(t, h)
	if n := ts.reg.hostCount(); n != 0 {
		t.Errorf("registry tracks %d hosts after the reap, want 0", n)
	}
}

// TestKeepAliveRestartedByEveryCommand proves the nvmet rule NP10 adopts: any
// received command, not only Keep Alive, restarts the timer.
func TestKeepAliveRestartedByEveryCommand(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	connConnectWithKato(t, ts, h, uint32(connKato.Milliseconds()))

	// Three quarters of the budget, then a command that is not Keep Alive.
	ts.clk.advance(connKatoExpiry / 4 * 3)
	c := h.propertyGet(regCsts, false)
	connStatus(t, c, statusSuccess, "property get")

	// Past the original deadline the timer fires, recomputes against the
	// command above and arms itself again rather than reaping. Waiting for
	// that re-arm is what makes the assertion below race-free.
	ts.clk.advance(connKatoExpiry / 2)
	ts.clk.waitWaiters(t, 2)
	if n := logs.count(msgHostDisconnected); n != 0 {
		t.Fatalf("reaped despite a command inside the budget (%d records)", n)
	}
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive")

	// And reaped once nothing has been received for a whole budget.
	ts.clk.advance(connKatoExpiry * 2)
	rec := logs.waitFor(t, msgHostDisconnected, 1)[0]
	if rec["reason"] != reasonKeepAlive {
		t.Fatalf("reason %v, want %v", rec["reason"], reasonKeepAlive)
	}
}

// TestKeepAliveFollowsSetFeatures0Fh proves where NP9 and NP10 meet: a host
// that shortens its KATO with Set Features 0Fh is reaped on the NEW budget,
// including against the timer the connection is already sleeping on. A
// controller that recomputed only when its armed timer next fired would hold
// this connection for the whole zero-KATO cutoff instead.
func TestKeepAliveFollowsSetFeatures0Fh(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	// Connected with KATO 0, so the armed budget is the 120 s idle cutoff.
	connConnectWithKato(t, ts, h, 0)
	connStatus(t, h.setFeatures(
		fidKeepAliveTimer, uint32(connKato.Milliseconds()),
	), statusSuccess, "set features 0fh")
	// The third timer is the one armed on the shortened budget.
	ts.clk.waitWaiters(t, 3)

	ts.clk.advance(connKatoExpiry)
	rec := logs.waitFor(t, msgHostDisconnected, 1)[0]
	if rec["reason"] != reasonKeepAlive {
		t.Fatalf("reason %v, want %v", rec["reason"], reasonKeepAlive)
	}
	connSocketClosed(t, h)
}

// TestZeroKatoIdleCutoff proves §0 #9: a one-shot `nvme discover` that asks
// for no keep-alive is not immortal — it is reaped after
// common.DefaultCdcZeroKatoTmoMs of silence.
func TestZeroKatoIdleCutoff(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	connConnectWithKato(t, ts, h, 0)
	cutoff := common.DefaultCdcZeroKatoTmoMs * time.Millisecond

	ts.clk.advance(cutoff - time.Millisecond)
	if n := logs.count(msgHostDisconnected); n != 0 {
		t.Fatalf("reaped before the idle cutoff (%d records)", n)
	}
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive before the cutoff")

	ts.clk.advance(cutoff)
	rec := logs.waitFor(t, msgHostDisconnected, 1)[0]
	if rec["reason"] != reasonKeepAlive {
		t.Fatalf("reason %v, want %v", rec["reason"], reasonKeepAlive)
	}
	connSocketClosed(t, h)
}

// ---------------------------------------------------------------------------
// Terminal errors and unknown commands (NP2, NP12)
// ---------------------------------------------------------------------------

// TestGarbageOnTheWireTerminatesTheConnection proves NP2 end to end: garbage
// after a good handshake earns a C2HTermReq, a closed socket and one
// `pdu error` record carrying the remote and the reason.
func TestGarbageOnTheWireTerminatesTheConnection(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		fes   uint16
	}{
		{
			name:  "an unknown pdu type",
			frame: pduCommonHdr(0xee, 0, 24, 0, 24),
			fes:   fesInvalidPduHdr,
		},
		{
			name:  "a capsule cmd with the wrong hlen",
			frame: pduCommonHdr(pduCapsuleCmd, 0, 8, 0, capsuleCmdHdrLen),
			fes:   fesInvalidPduHdr,
		},
		{
			name: "more in-capsule data than MAXH2CDATA",
			frame: pduCommonHdr(
				pduCapsuleCmd, 0, capsuleCmdHdrLen, capsuleCmdHdrLen,
				maxPduLen+1,
			),
			fes: fesDataLimitExceeded,
		},
		{
			name: "unsolicited h2c data, which is never solicited",
			frame: pduFrame(
				pduH2CData, 0, h2cDataHdrLen, h2cDataHdrLen, h2cDataHdrLen+8,
			),
			fes: fesPduSequenceErr,
		},
		{
			name:  "a second icreq once the connection is established",
			frame: buildICReqFor(pfv10, 0, 0),
			fes:   fesPduSequenceErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			ts := startServer(t)
			h := ts.dial()
			h.connectOk(connHostA)
			h.write(tt.frame)
			fes, _ := connTermReq(t, h)
			if fes != tt.fes {
				t.Errorf("fes %#x, want %#x", fes, tt.fes)
			}
			connSocketClosed(t, h)
			rec := logs.waitFor(t, msgPduError, 1)[0]
			if rec["remote"] == nil || rec["remote"] == "" {
				t.Error("`pdu error` has no remote")
			}
			if rec["reason"] == nil || rec["reason"] == "" {
				t.Error("`pdu error` has no reason")
			}
			// NP13: the connection had a host, so its teardown says so, with
			// the reason the error gave it.
			d := logs.waitFor(t, msgHostDisconnected, 1)[0]
			if d["reason"] != reasonPduError {
				t.Errorf("`host disconnected` reason %v, want %v",
					d["reason"], reasonPduError)
			}
		})
	}
}

// TestHostTerminateRequestEndsTheConnection proves the other half of NP2's
// terminate handling: an H2CTermReq is the HOST's decision, so the connection
// ends without an answer and without a `pdu error` record, and NP13 files it
// under `closed` like any other client-side close.
func TestHostTerminateRequestEndsTheConnection(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)
	h.write(pduFrame(pduH2CTermReq, 0, termReqHdrLen, 0, termReqHdrLen))
	rec := logs.waitFor(t, msgHostDisconnected, 1)[0]
	if rec["reason"] != reasonClosed {
		t.Errorf("reason %v, want %v", rec["reason"], reasonClosed)
	}
	if n := logs.count(msgPduError); n != 0 {
		t.Errorf("%d `pdu error` records, want 0: the host terminated, this "+
			"side did not fault", n)
	}
	// No C2HTermReq comes back — the socket simply ends.
	connSocketClosed(t, h)
	if n := ts.reg.hostCount(); n != 0 {
		t.Errorf("registry tracks %d hosts after the host terminated, want 0",
			n)
	}
}

// TestInCapsuleDataOnANonConnectCommand proves what handleCapsule documents:
// a command that carries in-capsule data and is not Connect — nvme-stas sends
// the TP-8010 Discovery Information Management command (opcode 21h) with a
// 1024 byte payload to every discovery controller — has its data read and
// discarded and the COMMAND refused (NP12, §0 #2), rather than the connection
// terminated. A C2HTermReq here would put the production host stack in a
// permanent connect/reset loop.
func TestInCapsuleDataOnANonConnectCommand(t *testing.T) {
	logs := captureLogs(t)
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)

	const opcDiscInfoMgmt = 0x21
	cid := h.allocCid()
	s := newSqe(opcDiscInfoMgmt, cid)
	setSgl(s, connectDataLen)
	s[39] = 0x5<<4 | 0x1 // in-capsule data
	h.send(s, make([]byte, connectDataLen))
	c := h.await(cid)
	connStatus(t, c, statusInvalidOpcode, "discovery information management")
	if c.status>>15 == 0 {
		t.Error("a registration this controller does not accept must carry DNR")
	}

	// The stream is still in sync and the connection still serves: the
	// payload was consumed with its PDU.
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive after the dim")
	if n := logs.count(msgPduError); n != 0 {
		t.Errorf("%d `pdu error` records, want 0: this is not a framing "+
			"error", n)
	}
}

// TestUnknownCommandsAreRefusedNotFatal proves NP12: an unknown admin opcode
// is an invalid opcode and an unknown fabrics command type an invalid field —
// both with DNR, and neither kills the connection.
func TestUnknownCommandsAreRefusedNotFatal(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk(connHostA)

	cid := h.allocCid()
	h.send(newSqe(0xc0, cid), nil)
	c := h.await(cid)
	connStatus(t, c, statusInvalidOpcode, "admin opcode 0xc0")
	if c.status>>15 == 0 {
		t.Error("an invalid opcode must carry DNR")
	}

	cid = h.allocCid()
	s := newSqe(opcFabrics, cid)
	s[4] = 0x7f // an fctype no fabrics command uses
	h.send(s, nil)
	c = h.await(cid)
	connStatus(t, c, statusInvalidField, "fabrics fctype 0x7f")
	if c.status>>15 == 0 {
		t.Error("an unknown fabrics command type must carry DNR")
	}

	// The connection is still usable, which is the point of NP12.
	connStatus(t, h.keepAlive(), statusSuccess, "keep alive after the refusals")
}
