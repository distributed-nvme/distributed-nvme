package main

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const idleTimeout = 15 * time.Second

type conn struct {
	st      *store
	c       net.Conn
	hostnqn string
	mu      sync.Mutex
	lastLog []byte
	pending []uint16
}

func (c *conn) writeAll(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.c.Write(b)
	return err
}

func (c *conn) queueAEN(cid uint16) {
	c.mu.Lock()
	c.pending = append(c.pending, cid)
	c.mu.Unlock()
}

func (c *conn) maybeAEN(m *model, gen uint64) {
	logBytes := buildDiscLog(m, c.hostnqn, gen)
	c.mu.Lock()
	same := bytesEqual(c.lastLog, logBytes)
	var cid uint16
	hasPending := len(c.pending) > 0
	if hasPending {
		cid = c.pending[0]
		c.pending = c.pending[1:]
	}
	c.mu.Unlock()
	if same || !hasPending {
		return
	}
	c.mu.Lock()
	c.lastLog = logBytes
	c.mu.Unlock()
	result := uint64(logDisc)<<16 | uint64(aenNoticeDiscChg)<<8 | uint64(aenNotice)
	rsp := buildRspPdu(cid, cqeStatus(scSuccess), result)
	if err := c.writeAll(rsp[:]); err != nil {
		log.Printf("[WARN] AEN delivery to %s failed: %v", c.hostnqn, err)
		c.mu.Lock()
		c.pending = append([]uint16{cid}, c.pending...)
		c.mu.Unlock()
		return
	}
	log.Printf("AEN delivered to %s (cid=%d)", c.hostnqn, cid)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cqeStatus(sc uint16) uint16 { return sc << 1 }

func serve(st *store, nc net.Conn) {
	defer nc.Close()
	c := &conn{st: st, c: nc}

	if !c.handshake() {
		return
	}
	c.loop()
}

func (c *conn) readFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(c.c, buf)
	return buf, err
}

func (c *conn) readPDU() ([]byte, error) {
	hdr, err := c.readFull(8)
	if err != nil {
		return nil, err
	}
	h := readTcpHdr(hdr)
	if h.Plen < 8 || h.Plen > 16*1024*1024 {
		return hdr, nil
	}
	rest, err := c.readFull(int(h.Plen) - 8)
	if err != nil {
		return nil, err
	}
	return append(hdr, rest...), nil
}

func (c *conn) handshake() bool {
	pdu, err := c.readPDU()
	if err != nil {
		log.Printf("handshake read: %v", err)
		return false
	}
	h := readTcpHdr(pdu[:8])
	if h.Type != pduICReq {
		log.Printf("handshake: expected ICReq, got type %#x", h.Type)
		return false
	}
	resp := buildICResp()
	if err := c.writeAll(resp[:]); err != nil {
		log.Printf("handshake write: %v", err)
		return false
	}
	log.Printf("ICReq/ICResp ok with %s", c.c.RemoteAddr())
	return true
}

func (c *conn) loop() {
	for {
		_ = c.c.SetReadDeadline(time.Now().Add(idleTimeout))
		pdu, err := c.readPDU()
		if err != nil {
			if isTimeout(err) {
				log.Printf("idle timeout on %s (hostnqn=%q), closing", c.c.RemoteAddr(), c.hostnqn)
			}
			c.cleanup()
			return
		}
		h := readTcpHdr(pdu[:8])
		switch h.Type {
		case pduCmd:
			if !c.handleCmd(pdu, h) {
				c.cleanup()
				return
			}
		case pduH2CTerm:
			log.Printf("H2CTerm from %s", c.c.RemoteAddr())
			c.cleanup()
			return
		default:
			log.Printf("unexpected PDU type %#x from %s", h.Type, c.c.RemoteAddr())
		}
	}
}

func (c *conn) cleanup() {
	c.st.delConn(c)
	_ = c.c.Close()
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func (c *conn) handleCmd(pdu []byte, h tcpHdr) bool {
	if len(pdu) < 8+64 {
		return false
	}
	op := pdu[8]
	cid := binary.LittleEndian.Uint16(pdu[0x0A:0x0C])
	replyDone := true
	var closeAfter bool
	switch op {
	case opFabrics:
		replyDone, closeAfter = c.handleFabrics(pdu, h, cid)
	case opIdentify:
		replyDone = c.handleIdentify(pdu, cid)
	case opGetLog:
		replyDone = c.handleGetLog(pdu, cid)
	case opKeepAlive:
		c.sendRsp(cid, scSuccess, 0)
	case opAsyncEvt:
		c.queueAEN(cid)
		replyDone = false
		log.Printf("AsyncEventReq armed on %s (cid=%d)", c.hostnqn, cid)
	case opSetFeat:
		c.handleSetFeat(pdu, cid)
	case opGetFeat:
		c.handleGetFeat(pdu, cid)
	case opDisconnect:
		c.sendRsp(cid, scSuccess, 0)
		closeAfter = true
	default:
		c.sendRsp(cid, scInvalidOpcode|statusDNR, 0)
		log.Printf("opcode %#x invalid on %s", op, c.hostnqn)
	}
	_ = replyDone
	if closeAfter {
		c.cleanup()
		return false
	}
	return true
}

func (c *conn) handleFabrics(pdu []byte, h tcpHdr, cid uint16) (bool, bool) {
	if len(pdu) < 8+5 {
		c.sendRsp(cid, scInvalidOpcode|statusDNR, 0)
		return true, false
	}
	fc := pdu[8+4]
	switch fc {
	case fctypeConnect:
		return c.handleConnect(pdu, h, cid), false
	case fctypePropSet, fctypePropGet, fctypeAuthSend, fctypeAuthRecv:
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return true, false
	default:
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return true, false
	}
}

func (c *conn) handleConnect(pdu []byte, h tcpHdr, cid uint16) bool {
	inlineStart := int(h.Hlen)
	if h.Hlen == 0 {
		inlineStart = cmdPduHdrSize
	}
	inlineLen := int(h.Plen) - inlineStart
	if inlineLen < 1024 {
		c.sendRsp(cid, scConnectFmt|statusDNR, 0)
		log.Printf("connect: short inline data (%d) from %s", inlineLen, c.c.RemoteAddr())
		return true
	}
	if inlineStart+1024 > len(pdu) {
		c.sendRsp(cid, scConnectFmt|statusDNR, 0)
		return true
	}
	cdata := pdu[inlineStart : inlineStart+1024]
	hostnqn := trimZero(cdata[0x200:0x300])
	if hostnqn == "" {
		c.sendRsp(cid, scConnectInvalid|statusDNR, 0)
		log.Printf("connect: empty hostnqn from %s", c.c.RemoteAddr())
		return true
	}
	c.hostnqn = hostnqn
	c.sendRsp(cid, scSuccess, 1)

	old := c.st.addConn(c)
	if old != nil && old != c {
		log.Printf("new CONNECT from %q replacing existing connection", hostnqn)
		_ = old.c.Close()
	}
	log.Printf("connect ok hostnqn=%q cid=%d from %s", hostnqn, cid, c.c.RemoteAddr())
	return true
}

func (c *conn) handleIdentify(pdu []byte, cid uint16) bool {
	cns := pdu[8+0x28]
	if cns != 0x01 {
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return true
	}
	id := buildIdentifyCtrl()
	c.sendData(cid, id[:])
	return true
}

func (c *conn) handleGetLog(pdu []byte, cid uint16) bool {
	if len(pdu) < 8+0x38 {
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return true
	}
	lid := pdu[8+0x28]
	if lid != lidDiscovery {
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return true
	}
	numdl := binary.LittleEndian.Uint16(pdu[8+0x2A : 8+0x2C])
	numdu := binary.LittleEndian.Uint16(pdu[8+0x2C : 8+0x2E])
	numd := uint32(numdu)<<16 | uint32(numdl)
	lpol := binary.LittleEndian.Uint32(pdu[8+0x30 : 8+0x34])
	lpou := binary.LittleEndian.Uint32(pdu[8+0x34 : 8+0x38])
	lpo := uint64(lpou)<<32 | uint64(lpol)
	xferBytes := (numd + 1) * 4

	m := c.st.curModel()
	page := buildDiscLog(m, c.hostnqn, c.st.curGenctr())
	c.mu.Lock()
	c.lastLog = page
	c.mu.Unlock()

	if lpo >= uint64(len(page)) {
		c.sendRsp(cid, scSuccess, 0)
		return true
	}
	end := lpo + uint64(xferBytes)
	if end > uint64(len(page)) {
		end = uint64(len(page))
	}
	c.sendData(cid, page[lpo:end])
	return true
}

func (c *conn) handleSetFeat(pdu []byte, cid uint16) {
	if len(pdu) < 8+0x2C {
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return
	}
	fid := pdu[8+0x28]
	switch fid {
	case featAEN:
		c.sendRsp(cid, scSuccess, 1<<31)
	case featKATO:
		c.sendRsp(cid, scSuccess, 0)
	default:
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
	}
}

func (c *conn) handleGetFeat(pdu []byte, cid uint16) {
	if len(pdu) < 8+0x2C {
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
		return
	}
	fid := pdu[8+0x28]
	switch fid {
	case featAEN:
		c.sendRsp(cid, scSuccess, 1<<31)
	case featKATO:
		c.sendRsp(cid, scSuccess, 5)
	default:
		c.sendRsp(cid, scInvalidField|statusDNR, 0)
	}
}

func (c *conn) sendRsp(cid uint16, sc uint16, result uint64) {
	rsp := buildRspPdu(cid, cqeStatus(sc), result)
	_ = c.writeAll(rsp[:])
}

func (c *conn) sendData(cid uint16, data []byte) {
	hdr := buildDataHdr(cid, uint32(len(data)))
	var buf []byte
	buf = append(buf, hdr[:]...)
	buf = append(buf, data...)
	if c.writeAll(buf) != nil {
		return
	}
	c.sendRsp(cid, scSuccess, 0)
}

func trimZero(b []byte) string {
	for i, v := range b {
		if v == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
