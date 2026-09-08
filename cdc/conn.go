package cdc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// This file is the per-connection admin-queue state machine of §5: one
// goroutine reads PDUs, a second delivers AENs and a third reaps the
// connection when its keep-alive expires. Every field of the connection state
// is behind mu; every socket write is behind wmu, so an AEN and a command
// response can never interleave inside one PDU.

// ---------------------------------------------------------------------------
// Values this controller reports (NP6, NP7, NP14)
// ---------------------------------------------------------------------------

const (
	// capTimeout is CAP.TO, the CC.EN-to-CSTS.RDY timeout in 500 ms units.
	// 15 is nvmet's value (NP14).
	capTimeout = 15
	// controllerVersion is the VS property and Identify's VER: NVMe 1.3.0,
	// what nvmet reports for a subsystem that was never told otherwise.
	controllerVersion = 0x00010300
	// identifyLen is the size of the Identify data structure.
	identifyLen = 4096
	// cnsController is the only CNS this controller answers (NP7).
	cnsController = 0x01
	// lidDiscovery is the only log page identifier it answers (NP8).
	lidDiscovery = 0x70
	// maxLogTransfer bounds one Get Log Page read. MDTS is 0 ("no limit"),
	// as nvmet reports for discovery, but a controller still has to bound
	// the buffer it allocates for one command; hosts read the discovery log
	// in 4 KiB chunks, four hundred times below this.
	maxLogTransfer = 1 << 20
)

// The CC and CSTS register bits this controller acts on (NP6).
const (
	ccEnable   = 1 << 0
	ccShnShift = 14
	ccShnMask  = 0x3

	cstsReady       = 1 << 0
	cstsShstComplet = 0x2 << 2
	cstsShstMask    = 0x3 << 2
)

// The AEN this controller sends (DS8): a Notice of type "Discovery Log Page
// Changed", naming log page 70h.
const (
	aenTypeNotice      = 0x02
	aenInfoDiscChanged = 0xf0
)

// aenDiscLogChanged is that AEN as it appears in dword 0 of the completion.
const aenDiscLogChanged = aenTypeNotice |
	aenInfoDiscChanged<<8 |
	lidDiscovery<<16

// aenCfgDiscChange is the Asynchronous Event Configuration bit that gates it
// (NP9): bit 31, "Discovery Log Page Change Notices".
const aenCfgDiscChange = 1 << 31

// socketWriteTimeout bounds one PDU write. A host that has stopped reading is
// gone; the connection dies rather than holding a goroutine forever.
const socketWriteTimeout = 30 * time.Second

// errHostTerminated is the host's own H2CTermReq: the connection ends, but it
// is not this side's protocol error.
var errHostTerminated = errors.New("cdc: host terminated the connection")

// ---------------------------------------------------------------------------
// Connection
// ---------------------------------------------------------------------------

// conn is one accepted NVMe/TCP connection and the admin controller behind it.
type conn struct {
	srv    *server
	deps   *deps
	reg    *registry
	nc     net.Conn
	remote string
	ctx    context.Context

	// done is closed when the reader goroutine has left; the keep-alive and
	// AEN goroutines watch it.
	done chan struct{}
	// wake carries one bit: "there may be an AEN to deliver". Its capacity
	// of one is what makes multiple impacts before a delivery coalesce
	// (§0 #7).
	wake chan struct{}
	// kick makes the keep-alive loop recompute its deadline, which it must
	// whenever the idle budget SHRANK under the timer it is already sleeping
	// on (NP10).
	kick chan struct{}

	// wmu serializes socket writes so that two whole PDUs never interleave.
	wmu sync.Mutex

	mu sync.Mutex
	// connected is set once Fabrics Connect has succeeded (NP4).
	connected bool
	hostNqn   string
	hostId    string
	cntlId    uint16
	kato      time.Duration
	sqSize    uint16
	sqHead    uint16
	sqFlowOff bool
	cc        uint32
	csts      uint32
	// aenCfg is feature 0Bh, default 0: a host that has not enabled the
	// discovery-log-change notice gets no AEN (NP9, §0 #8).
	aenCfg uint32
	// pending is the NP11 pending bit and pendingGenCtr the GENCTR that set
	// it, for the `aen sent` record.
	pending       bool
	pendingGenCtr uint64
	// aers are the command ids of the outstanding Asynchronous Event
	// Requests, oldest first (NP11).
	aers    []uint16
	lastAct time.Time
	// closeReason is the §7 `host disconnected` reason, set by whoever ends
	// the connection first.
	closeReason string
	closing     bool
}

// newConn wraps one accepted socket.
func newConn(s *server, nc net.Conn) *conn {
	ctx := common.WithTraceId(s.ctx, common.NewTraceId())
	return &conn{
		srv:     s,
		deps:    s.deps,
		reg:     s.reg,
		nc:      nc,
		remote:  nc.RemoteAddr().String(),
		ctx:     ctx,
		done:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
		kick:    make(chan struct{}, 1),
		lastAct: s.deps.clk.now(),
	}
}

// ---------------------------------------------------------------------------
// Lifecycle (NP1, NP13)
// ---------------------------------------------------------------------------

// serve runs the connection to its end (NP1): the NP3 handshake, then one
// admin command per capsule until the socket, the host or the keep-alive
// timer ends it.
func (c *conn) serve() {
	defer c.finish()
	go c.keepAliveLoop()
	go c.aenLoop()
	reader := bufio.NewReaderSize(c.nc, maxPduLen)
	if err := c.handshake(reader); err != nil {
		c.fail(err)
		return
	}
	for {
		p, err := readPdu(reader)
		if err != nil {
			c.fail(err)
			return
		}
		if err := c.dispatch(p); err != nil {
			c.fail(err)
			return
		}
	}
}

// fail ends the connection on the reason err names (NP2, NP13). A protocol
// error answers C2HTermReq first; a socket that simply closed says so and
// nothing more.
func (c *conn) fail(err error) {
	var perr *pduError
	switch {
	case errors.As(err, &perr):
		slog.InfoContext(c.ctx, msgPduError,
			slog.String("remote", c.remote),
			slog.String("reason", perr.reason),
		)
		// Best effort: the socket may already be gone.
		_ = c.writePdu(buildTermReq(perr.fes, perr.fei))
		c.shutdown(reasonPduError)
	case errors.Is(err, errHostTerminated):
		c.shutdown(reasonClosed)
	default:
		c.shutdown(reasonClosed)
	}
}

// shutdown closes the socket under a reason. The first reason wins, so a
// keep-alive expiry or a fleet shutdown is not relabelled `closed` by the
// read error it causes.
func (c *conn) shutdown(reason string) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	c.closeReason = reason
	c.mu.Unlock()
	_ = c.nc.Close()
}

// finish is NP13: stop the helper goroutines, unregister from the host state
// (whose last connection takes it with them, DS7) and log
// `host disconnected`. Outstanding AERs die with the socket, unanswered.
func (c *conn) finish() {
	c.shutdown(reasonClosed)
	close(c.done)
	c.mu.Lock()
	connected := c.connected
	hostNqn := c.hostNqn
	cntlId := c.cntlId
	reason := c.closeReason
	c.mu.Unlock()
	if connected {
		c.reg.detach(hostNqn, c)
	}
	c.srv.forget(c)
	if connected {
		slog.InfoContext(c.ctx, msgHostDisconnected,
			slog.String("hostnqn", hostNqn),
			slog.Uint64("cntlid", uint64(cntlId)),
			slog.String("reason", reason),
		)
	}
}

// keepAliveLoop is NP10: a connection that has said nothing for its timeout is
// reaped. KATO > 0 expires at KATO + common.DefaultCdcKeepAliveGraceMs;
// KATO = 0 — a one-shot `nvme discover` — expires after
// common.DefaultCdcZeroKatoTmoMs idle (§0 #9).
//
// The deadline is recomputed at every wake-up, so a command that arrives late
// in a window simply pushes the next one out. A budget that SHRANK cannot
// wait for the armed timer, though — Connect naming a KATO below the
// zero-KATO cutoff, or a Set Features 0Fh that lowers it (NP9), would
// otherwise be reaped up to two minutes late — so those two paths call
// rearmKeepAlive and this select recomputes on the kick.
func (c *conn) keepAliveLoop() {
	for {
		c.mu.Lock()
		deadline := c.lastAct.Add(c.timeoutLocked())
		c.mu.Unlock()
		wait := deadline.Sub(c.deps.clk.now())
		if wait <= 0 {
			c.shutdown(reasonKeepAlive)
			return
		}
		select {
		case <-c.done:
			return
		case <-c.kick:
		case <-c.deps.clk.after(wait):
		}
	}
}

// rearmKeepAlive makes the loop recompute its deadline now. A timeout that
// GREW needs no kick — the armed timer fires early and the loop re-arms — but
// one that shrank does: without it a connection that raised the question by
// arriving with a short KATO would still be reaped on the zero-KATO budget the
// timer was armed with before Connect (NP10).
func (c *conn) rearmKeepAlive() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// timeoutLocked is the idle budget of the connection's current KATO.
func (c *conn) timeoutLocked() time.Duration {
	if c.kato > 0 {
		return c.kato +
			common.DefaultCdcKeepAliveGraceMs*time.Millisecond
	}
	return common.DefaultCdcZeroKatoTmoMs * time.Millisecond
}

// touch restarts the keep-alive timer, which every received command does (the
// nvmet rule, NP10).
func (c *conn) touch() {
	now := c.deps.clk.now()
	c.mu.Lock()
	c.lastAct = now
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// writePdu writes one complete PDU. wmu makes the write atomic against the AEN
// goroutine; the deadline makes a host that has stopped reading fatal rather
// than eternal.
func (c *conn) writePdu(buf []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.nc.SetWriteDeadline(
		time.Now().Add(socketWriteTimeout),
	); err != nil {
		return err
	}
	_, err := c.nc.Write(buf)
	return err
}

// respond completes one command (NP4): SQHD is maintained unless the host
// asked for it not to be at Connect.
func (c *conn) respond(cid uint16, status uint16, dw0 uint32, dw1 uint32) error {
	c.mu.Lock()
	sqHead := c.advanceSqHeadLocked()
	c.mu.Unlock()
	return c.writePdu(buildCapsuleResp(completion{
		dw0:    dw0,
		dw1:    dw1,
		sqhd:   sqHead,
		sqid:   0,
		cid:    cid,
		status: status,
	}))
}

// advanceSqHeadLocked consumes one submission queue slot and returns the value
// to report. 0xffff is the specs' "SQ flow control disabled" marker.
func (c *conn) advanceSqHeadLocked() uint16 {
	if c.sqFlowOff {
		return 0xffff
	}
	if size := uint32(c.sqSize) + 1; size > 0 {
		c.sqHead = uint16((uint32(c.sqHead) + 1) % size)
	}
	return c.sqHead
}

// transfer answers a command that returns data (NP7, NP8): one C2HData PDU
// with the whole payload, then the completion. The SUCCESS-flag shortcut is
// deliberately not used, so SQHD keeps flowing.
func (c *conn) transfer(cid uint16, payload []byte) error {
	if len(payload) > 0 {
		if err := c.writePdu(buildC2HData(cid, payload)); err != nil {
			return err
		}
	}
	return c.respond(cid, statusSuccess, 0, 0)
}

// ---------------------------------------------------------------------------
// Handshake (NP3, NP4)
// ---------------------------------------------------------------------------

// handshake performs connection establishment: exactly one ICReq, answered
// with the ICResp of NP3.
func (c *conn) handshake(reader *bufio.Reader) error {
	p, err := readPdu(reader)
	if err != nil {
		return err
	}
	if p.typ != pduICReq {
		return newPduError(fesPduSequenceErr, 0,
			"first pdu is type %#x, want icreq", p.typ)
	}
	if _, err := parseICReq(p); err != nil {
		return err
	}
	c.touch()
	return c.writePdu(buildICResp())
}

// dispatch routes one received PDU (NP2).
func (c *conn) dispatch(p *pdu) error {
	switch p.typ {
	case pduCapsuleCmd:
		return c.handleCapsule(p)
	case pduH2CTermReq:
		return errHostTerminated
	case pduICReq:
		return newPduError(fesPduSequenceErr, 0,
			"icreq after connection establishment")
	case pduH2CData:
		// R2T is never sent (NP2), so no host data is ever solicited.
		return newPduError(fesPduSequenceErr, 0, "unsolicited h2c data")
	}
	return newPduError(fesInvalidPduHdr, 0, "unsupported pdu type %#x", p.typ)
}

// handleCapsule executes one admin command.
//
// In-capsule data on a command other than Connect is READ AND DISCARDED, not
// treated as a protocol error. NP2 lists it among the terminal PDU errors, but
// that rule cannot survive contact with the production host stack: nvme-stas
// sends the TP-8010 Discovery Information Management command (opcode 21h)
// with its 1024 byte payload in the capsule to every discovery controller it
// connects to. Answering C2HTermReq puts the host in a permanent
// connect/reset loop, whereas refusing the COMMAND — invalid opcode, DNR, per
// NP12 and §0 #2's "nothing registers into it" — is what the specs prescribe
// and what stas is written to handle. The framing itself is already bounded:
// readPdu refuses any PDU past common.CdcMaxH2CData before a byte of it is
// buffered, which is the rule NP2 exists to enforce.
func (c *conn) handleCapsule(p *pdu) error {
	s := sqe(p.hdr[pduCommonHdrLen : pduCommonHdrLen+sqeLen])
	isConnect := s.opc() == opcFabrics && s.fctype() == fctypeConnect
	// NP10: every received command restarts the keep-alive timer.
	c.touch()

	c.mu.Lock()
	connected := c.connected
	c.mu.Unlock()
	if !connected {
		if !isConnect {
			return newPduError(fesPduSequenceErr, 0,
				"opcode %#x before fabrics connect", s.opc())
		}
		return c.handleConnect(s, p.data)
	}
	if isConnect {
		return newPduError(fesPduSequenceErr, 0,
			"a second fabrics connect on one connection")
	}

	switch s.opc() {
	case opcFabrics:
		switch s.fctype() {
		case fctypePropertyGet:
			return c.handlePropertyGet(s)
		case fctypePropertySet:
			return c.handlePropertySet(s)
		}
		// NP12: an unknown fabrics command type is an invalid field, not an
		// invalid opcode — the opcode itself was fine.
		return c.respond(s.cid(), statusInvalidField, 0, 0)
	case opcIdentify:
		return c.handleIdentify(s)
	case opcGetLogPage:
		return c.handleGetLogPage(s)
	case opcSetFeatures:
		return c.handleSetFeatures(s)
	case opcGetFeatures:
		return c.handleGetFeatures(s)
	case opcAsyncEvent:
		return c.handleAsyncEvent(s)
	case opcKeepAlive:
		// The timer was already restarted above; the command itself does
		// nothing else (NP10).
		return c.respond(s.cid(), statusSuccess, 0, 0)
	}
	return c.respond(s.cid(), statusInvalidOpcode, 0, 0)
}

// ---------------------------------------------------------------------------
// Connect (NP5)
// ---------------------------------------------------------------------------

// handleConnect validates the 1024 byte connect data and, on success, admits
// the connection: a CNTLID from the round-robin counter, registration under
// the hostnqn (DS7) and the `host connected` record.
func (c *conn) handleConnect(s sqe, data []byte) error {
	cid := s.cid()
	if len(data) != connectDataLen {
		return newPduError(fesInvalidPduHdr, 4,
			"connect data is %d bytes, want %d", len(data), connectDataLen)
	}
	if s.connectRecfmt() != 0 {
		return c.respond(cid, statusConnectFormat,
			connectIpo(connectCmdRecfmtOff, false), 0)
	}
	if s.connectQid() != 0 {
		// There are no I/O queues (§0 #2).
		return c.respond(cid, statusConnectParam,
			connectIpo(connectCmdQidOff, false), 0)
	}
	sqSize := s.connectSqsize()
	if sqSize == 0 {
		return c.respond(cid, statusConnectParam,
			connectIpo(connectCmdSqsizeOff, false), 0)
	}
	if sqSize > common.CdcMaxAdminSqSize-1 {
		sqSize = common.CdcMaxAdminSqSize - 1
	}
	subNqn := nqnField(data[connectDataSubNqnOff : connectDataSubNqnOff+256])
	if subNqn != common.NvmeDiscoveryNqn {
		return c.respond(cid, statusConnectParam,
			connectIpo(connectDataSubNqnOff, true), 0)
	}
	hostNqn := nqnField(data[connectDataHostNqnOff : connectDataHostNqnOff+256])
	if hostNqn == "" || len(hostNqn) > common.MaxNqnLength {
		return c.respond(cid, statusConnectHost,
			connectIpo(connectDataHostNqnOff, true), 0)
	}
	hostId := formatHostId(data[connectDataHostIdOff : connectDataHostIdOff+16])
	kato := time.Duration(s.connectKato()) * time.Millisecond
	cntlId := c.srv.nextCntlId()

	c.mu.Lock()
	c.connected = true
	c.hostNqn = hostNqn
	c.hostId = hostId
	c.cntlId = cntlId
	c.kato = kato
	c.sqSize = sqSize
	c.sqFlowOff = s.connectCattr()&connectDisableSqflow != 0
	c.lastAct = c.deps.clk.now()
	c.mu.Unlock()
	// The keep-alive timer was armed on the zero-KATO budget before Connect
	// arrived; a KATO shorter than that only takes effect once the loop has
	// been told to recompute (NP10).
	c.rearmKeepAlive()

	// The host state must exist before the response: the host may issue Get
	// Log Page on the very next capsule (DS7).
	c.reg.attach(hostNqn, c)

	slog.InfoContext(c.ctx, msgHostConnected,
		slog.String("hostnqn", hostNqn),
		slog.String("hostid", hostId),
		slog.Uint64("cntlid", uint64(cntlId)),
		slog.Int64("kato_ms", kato.Milliseconds()),
		slog.String("remote", c.remote),
	)
	return c.respond(cid, statusSuccess, uint32(cntlId), 0)
}

// nqnField reads one fixed-size NVMe character field, which the specs pad with
// NULs and some hosts pad with spaces.
func nqnField(field []byte) string {
	if i := bytes.IndexByte(field, 0); i >= 0 {
		field = field[:i]
	}
	return string(bytes.TrimRight(field, " "))
}

// formatHostId renders the connect data's 16 byte host identifier the way
// every host writes it in /etc/nvme/hostid, so the two are greppable together.
func formatHostId(raw []byte) string {
	hexed := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32])
}

// ---------------------------------------------------------------------------
// Properties (NP6)
// ---------------------------------------------------------------------------

// capValue is the CAP property: MQES one below the admin queue size, CQR set
// (this controller needs contiguous queues), DSTRD 0, the NVM command set,
// MPSMIN = MPSMAX = 0 and nvmet's CC.EN timeout.
func capValue() uint64 {
	var value uint64
	value |= uint64(common.CdcMaxAdminSqSize - 1)
	value |= 1 << 16
	value |= uint64(capTimeout) << 24
	value |= 1 << 37
	return value
}

// handlePropertyGet answers CAP, VS, CC and CSTS. The 8 byte form reads CAP
// only and the 4 byte form everything else, exactly as nvmet splits them.
func (c *conn) handlePropertyGet(s sqe) error {
	cid := s.cid()
	var value uint64
	if s.propAttrib()&1 != 0 {
		if s.propOffset() != regCap {
			return c.respond(cid, statusInvalidField, 0, 0)
		}
		value = capValue()
	} else {
		c.mu.Lock()
		cc, csts := c.cc, c.csts
		c.mu.Unlock()
		switch s.propOffset() {
		case regVs:
			value = controllerVersion
		case regCc:
			value = uint64(cc)
		case regCsts:
			value = uint64(csts)
		default:
			return c.respond(cid, statusInvalidField, 0, 0)
		}
	}
	return c.respond(cid, statusSuccess,
		uint32(value), uint32(value>>32))
}

// handlePropertySet writes CC and nothing else (NP6).
func (c *conn) handlePropertySet(s sqe) error {
	cid := s.cid()
	if s.propAttrib()&1 != 0 {
		return c.respond(cid, statusInvalidField, 0, 0)
	}
	if s.propOffset() != regCc {
		return c.respond(cid, statusInvalidField, 0, 0)
	}
	c.mu.Lock()
	c.updateCcLocked(uint32(s.propValue()))
	c.mu.Unlock()
	return c.respond(cid, statusSuccess, 0, 0)
}

// updateCcLocked follows nvmet: CSTS.RDY tracks CC.EN, and a shutdown notice
// clears RDY and reports the shutdown complete. The IOSQES/IOCQES and CSS
// fields are accepted without enforcement — a discovery controller has no I/O
// queues for them to describe.
func (c *conn) updateCcLocked(value uint32) {
	old := c.cc
	c.cc = value
	oldEn := old&ccEnable != 0
	newEn := value&ccEnable != 0
	oldShn := (old >> ccShnShift) & ccShnMask
	newShn := (value >> ccShnShift) & ccShnMask
	if newEn && !oldEn {
		c.csts |= cstsReady
	}
	if !newEn && oldEn {
		c.csts &^= cstsReady
	}
	if newShn != 0 && oldShn == 0 {
		c.csts &^= cstsReady
		c.csts |= cstsShstComplet
	}
	if newShn == 0 && oldShn != 0 {
		c.csts &^= cstsShstMask
	}
}

// ---------------------------------------------------------------------------
// Identify (NP7)
// ---------------------------------------------------------------------------

// handleIdentify answers CNS 01h and refuses everything else (NP7).
func (c *conn) handleIdentify(s sqe) error {
	cid := s.cid()
	if s.identifyCns() != cnsController {
		return c.respond(cid, statusInvalidField, 0, 0)
	}
	c.mu.Lock()
	cntlId := c.cntlId
	c.mu.Unlock()
	payload := c.srv.identifyController(cntlId)
	// The SGL is the authoritative size of the host's buffer: never write
	// more than it describes, a zero-length descriptor included.
	if n := uint64(s.sglLen()); n < uint64(len(payload)) {
		payload = payload[:n]
	}
	return c.transfer(cid, payload)
}

// The Identify Controller field offsets this controller fills in. Everything
// not named here stays zero, which is what nvmet's discovery identify does.
const (
	idOffSerial    = 4
	idLenSerial    = 20
	idOffModel     = 24
	idLenModel     = 40
	idOffFirmware  = 64
	idLenFirmware  = 8
	idOffMdts      = 77
	idOffCntlId    = 78
	idOffVer       = 80
	idOffOaes      = 92
	idOffCtratt    = 96
	idOffCntrlType = 111
	idOffAerl      = 259
	idOffLpa       = 261
	idOffKas       = 320
	idOffSqes      = 512
	idOffCqes      = 513
	idOffMaxCmd    = 514
	idOffSgls      = 536
	idOffSubNqn    = 768
	idLenSubNqn    = 256
	idOffIoccsz    = 1792
	idOffIorcsz    = 1796
	idOffIcdoff    = 1800
	idOffDctype    = 1806
)

// The Identify values of NP7.
const (
	// cntrlTypeDiscovery is CNTRLTYPE 2, "discovery controller" (1 is an
	// I/O controller and 3 an administrative one). The Linux host publishes
	// it as /sys/class/nvme/nvmeN/cntrltype, where the string must read
	// `discovery` for the stock nvmf autoconnect rule to recognise the
	// controller.
	cntrlTypeDiscovery = 2
	// oaesDiscChange is OAES bit 31: this controller supports Discovery Log
	// Page Change notices, which is what makes the host enable them (§0 #8).
	oaesDiscChange = 1 << 31
	// ctrattHostId128 says the host identifier is 128 bits, as fabrics
	// hosts always send it.
	ctrattHostId128 = 1 << 0
	// lpaExtended is LPA bit 2: the Log Page Offset and the extended Number
	// of Dwords fields are supported, which is how a host pages the
	// discovery log (NP8).
	lpaExtended = 1 << 2
	// kasUnits is KAS in 100 ms units: a one second keep-alive granularity.
	kasUnits = 10
	// sglsSupported is SGLS bit 0 (SGLs are supported) plus bit 20 (an SGL
	// data block address may be an offset), the pair nvmet advertises and
	// the Linux host requires of a fabrics controller.
	sglsSupported = 1<<0 | 1<<20
	// sqesValue and cqesValue are the required and maximum queue entry
	// sizes, 64 and 16 bytes as powers of two.
	sqesValue = 0x66
	cqesValue = 0x44
	// dctypeDdc is DCTYPE 1, "Direct Discovery Controller": dnv-cdc serves
	// discovery but accepts no TP-8010 registration (§0 #2), so a host must
	// not treat it as a Centralized Discovery Controller to register with.
	dctypeDdc = 1
)

// buildIdentify renders the Identify Controller data structure (NP7). Fields
// this document does not pin follow nvmet's discovery controller (NP14):
// everything unnamed is zero.
func buildIdentify(cntlId uint16, serial string) []byte {
	buf := make([]byte, identifyLen)
	putAscii(buf[idOffSerial:idOffSerial+idLenSerial], serial)
	putAscii(buf[idOffModel:idOffModel+idLenModel], "dnv")
	putAscii(buf[idOffFirmware:idOffFirmware+idLenFirmware], runtime.Version())
	buf[idOffMdts] = 0
	binary.LittleEndian.PutUint16(buf[idOffCntlId:idOffCntlId+2], cntlId)
	binary.LittleEndian.PutUint32(buf[idOffVer:idOffVer+4], controllerVersion)
	binary.LittleEndian.PutUint32(buf[idOffOaes:idOffOaes+4], oaesDiscChange)
	binary.LittleEndian.PutUint32(buf[idOffCtratt:idOffCtratt+4], ctrattHostId128)
	buf[idOffCntrlType] = cntrlTypeDiscovery
	buf[idOffAerl] = common.CdcAerl
	buf[idOffLpa] = lpaExtended
	binary.LittleEndian.PutUint16(buf[idOffKas:idOffKas+2], kasUnits)
	buf[idOffSqes] = sqesValue
	buf[idOffCqes] = cqesValue
	binary.LittleEndian.PutUint16(
		buf[idOffMaxCmd:idOffMaxCmd+2], common.CdcMaxAdminSqSize,
	)
	binary.LittleEndian.PutUint32(buf[idOffSgls:idOffSgls+4], sglsSupported)
	putField(buf[idOffSubNqn:idOffSubNqn+idLenSubNqn], common.NvmeDiscoveryNqn)
	binary.LittleEndian.PutUint32(
		buf[idOffIoccsz:idOffIoccsz+4],
		(sqeLen+common.CdcMaxH2CData)/16,
	)
	binary.LittleEndian.PutUint32(buf[idOffIorcsz:idOffIorcsz+4], cqeLen/16)
	binary.LittleEndian.PutUint16(buf[idOffIcdoff:idOffIcdoff+2], 0)
	buf[idOffDctype] = dctypeDdc
	return buf
}

// putAscii copies a string into a fixed-size, SPACE-padded ASCII field, which
// is how the specs shape Identify's SN, MN and FR.
func putAscii(dst []byte, value string) {
	n := copy(dst, value)
	for i := n; i < len(dst); i++ {
		dst[i] = ' '
	}
}

// ---------------------------------------------------------------------------
// Get Log Page (NP8)
// ---------------------------------------------------------------------------

// handleGetLogPage serves the discovery log out of a snapshot taken right
// here, when the command arrived (DS9). RAE is accepted and ignored: event
// clearing is delivery-based (§0 #7).
func (c *conn) handleGetLogPage(s sqe) error {
	cid := s.cid()
	if s.logLid() != lidDiscovery {
		return c.respond(cid, statusInvalidField, 0, 0)
	}
	off := s.logOffset()
	if off&0x3 != 0 {
		// The specs require dword aligned offsets; nvmet says the same.
		return c.respond(cid, statusInvalidField, 0, 0)
	}
	length := (uint64(s.logNumd()) + 1) * 4
	// As for Identify: the SGL bounds the transfer, whatever NUMD asked for.
	if n := uint64(s.sglLen()); n < length {
		length = n
	}
	if length > maxLogTransfer {
		return c.respond(cid, statusInvalidField, 0, 0)
	}
	c.mu.Lock()
	hostNqn := c.hostNqn
	c.mu.Unlock()
	genCtr, numRec, body := c.reg.snapshot(hostNqn)
	return c.transfer(cid, logPageBytes(genCtr, numRec, body, off, int(length)))
}

// ---------------------------------------------------------------------------
// Features (NP9)
// ---------------------------------------------------------------------------

// handleSetFeatures implements 0Bh and 0Fh and refuses every other feature.
//
// The Asynchronous Event Configuration value is stored as the host wrote it
// and only its discovery-log-change bit is ever read (NP9). nvmet rejects a
// value with bits outside its supported mask; dnv-cdc deliberately does not,
// because a host that asks for a notice this controller never sends loses
// nothing by being told yes.
func (c *conn) handleSetFeatures(s sqe) error {
	cid := s.cid()
	switch s.featFid() {
	case fidAsyncEventConfig:
		value := s.featDword11()
		c.mu.Lock()
		c.aenCfg = value
		c.mu.Unlock()
		// A host that enables the notice while an impact is already pending
		// must still get it (§0 #7).
		c.poke()
		return c.respond(cid, statusSuccess, 0, 0)
	case fidKeepAliveTimer:
		value := s.featDword11()
		c.mu.Lock()
		c.kato = time.Duration(value) * time.Millisecond
		c.lastAct = c.deps.clk.now()
		c.mu.Unlock()
		// The new KATO may be shorter than the one the timer is sleeping
		// on (NP10).
		c.rearmKeepAlive()
		return c.respond(cid, statusSuccess, value, 0)
	}
	return c.respond(cid, statusInvalidField, 0, 0)
}

// handleGetFeatures is its mirror.
func (c *conn) handleGetFeatures(s sqe) error {
	cid := s.cid()
	c.mu.Lock()
	aenCfg := c.aenCfg
	kato := c.kato
	c.mu.Unlock()
	switch s.featFid() {
	case fidAsyncEventConfig:
		return c.respond(cid, statusSuccess, aenCfg, 0)
	case fidKeepAliveTimer:
		return c.respond(cid, statusSuccess, uint32(kato.Milliseconds()), 0)
	}
	return c.respond(cid, statusInvalidField, 0, 0)
}

// ---------------------------------------------------------------------------
// Asynchronous events (NP11, DS8)
// ---------------------------------------------------------------------------

// handleAsyncEvent arms one AER. Up to common.CdcAerl + 1 may be outstanding;
// one more is refused with the AER-limit status. An AER that arrives while the
// pending bit is set is completed immediately.
func (c *conn) handleAsyncEvent(s sqe) error {
	c.mu.Lock()
	if len(c.aers) >= common.CdcAerl+1 {
		c.mu.Unlock()
		return c.respond(s.cid(), statusAsyncLimit, 0, 0)
	}
	c.aers = append(c.aers, s.cid())
	c.mu.Unlock()
	c.poke()
	return nil
}

// notify is the DS6 -> NP11 hand-off: the impact sets the pending bit and asks
// the AEN goroutine to try a delivery. It never blocks and never writes to the
// socket itself, so a host that has stopped reading cannot stall the watcher.
func (c *conn) notify(genCtr uint64) {
	c.mu.Lock()
	c.pending = true
	c.pendingGenCtr = genCtr
	c.mu.Unlock()
	c.poke()
}

// poke wakes the AEN goroutine. The channel's capacity of one IS the
// coalescing of §0 #7: several impacts before a delivery are one wake-up.
func (c *conn) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// aenLoop delivers pending AENs (NP11).
func (c *conn) aenLoop() {
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
			if err := c.deliverAen(); err != nil {
				c.shutdown(reasonClosed)
				return
			}
		}
	}
}

// deliverAen completes one armed AER if there is an impact to report and the
// host has enabled the notice (NP9). Delivery clears the pending bit; an
// impact while unarmed stays in the bit until an AER arrives.
func (c *conn) deliverAen() error {
	c.mu.Lock()
	if !c.pending || len(c.aers) == 0 || c.aenCfg&aenCfgDiscChange == 0 {
		c.mu.Unlock()
		return nil
	}
	cid := c.aers[0]
	c.aers = c.aers[1:]
	c.pending = false
	genCtr := c.pendingGenCtr
	hostNqn := c.hostNqn
	cntlId := c.cntlId
	c.mu.Unlock()

	if err := c.respond(cid, statusSuccess, aenDiscLogChanged, 0); err != nil {
		return err
	}
	slog.InfoContext(c.ctx, msgAenSent,
		slog.String("hostnqn", hostNqn),
		slog.Uint64("cntlid", uint64(cntlId)),
		slog.Uint64("genctr", genCtr),
	)
	return nil
}
