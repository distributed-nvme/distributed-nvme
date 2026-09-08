package cdc

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// This file is the NVMe/TCP PDU codec of cdc.md NP2/NP3: the byte layouts the
// specs define, the subset dnv-cdc implements, and nothing else. It knows
// about sockets only through io.Reader/io.Writer, so the §8 tests drive it
// over a pipe.
//
// Every multi-byte field on the wire is little-endian, as the specs require.

// ---------------------------------------------------------------------------
// PDU types (NP2)
// ---------------------------------------------------------------------------

const (
	pduICReq       = 0x00
	pduICResp      = 0x01
	pduH2CTermReq  = 0x02
	pduC2HTermReq  = 0x03
	pduCapsuleCmd  = 0x04
	pduCapsuleResp = 0x05
	pduH2CData     = 0x06
	pduC2HData     = 0x07
	pduR2T         = 0x09
)

// The header lengths of every PDU this controller reads or writes. They are
// the specs' HLEN values and the host validates them, so they are constants
// and never computed.
const (
	pduCommonHdrLen  = 8
	icReqLen         = 128
	icRespLen        = 128
	termReqHdrLen    = 24
	capsuleCmdHdrLen = 72
	capsuleRespLen   = 24
	c2hDataHdrLen    = 24
	h2cDataHdrLen    = 24

	// sqeLen and cqeLen are the submission and completion queue entry
	// sizes: a CapsuleCmd carries one SQE, a CapsuleResp one CQE.
	sqeLen = 64
	cqeLen = 16
)

// pfv10 is the PDU format version of NVMe/TCP 1.0, the only one accepted or
// sent (NP3).
const pfv10 = 0

// The ICReq/ICResp DGST bits.
const (
	digestHdrEnable  = 1 << 0
	digestDataEnable = 1 << 1
)

// The PDU FLAGS bits, in the specs' order: the two digest-present flags come
// FIRST, so LAST_PDU is bit 2 and SUCCESS bit 3 (Linux spells them
// NVME_TCP_F_HDGST/F_DDGST/F_DATA_LAST/F_DATA_SUCCESS). SUCCESS is
// deliberately never set: NP8 keeps the CapsuleResp after every data
// transfer, which is also what keeps SQHD flowing when SQ flow control is on.
const (
	pduFlagHdrDigest  = 1 << 0
	pduFlagDataDigest = 1 << 1
	c2hDataLast       = 1 << 2
	c2hDataSuccess    = 1 << 3
)

// Fatal Error Status values of a Terminate Connection Request (NP2).
const (
	fesInvalidPduHdr     = 0x01
	fesPduSequenceErr    = 0x02
	fesHdrDigestErr      = 0x03
	fesDataOutOfRange    = 0x04
	fesDataLimitExceeded = 0x05
	fesUnsupportedParam  = 0x06
)

// maxPduLen bounds one received PDU: the largest header dnv-cdc accepts plus
// the in-capsule data cap of NP3. Anything longer is refused before a single
// byte of it is buffered.
const maxPduLen = capsuleCmdHdrLen + common.CdcMaxH2CData

// ---------------------------------------------------------------------------
// Errors (NP2)
// ---------------------------------------------------------------------------

// pduError is a terminal protocol error: the connection answers C2HTermReq
// with fes/fei, logs `pdu error` with reason and closes (NP2, NP13).
type pduError struct {
	fes    uint16
	fei    uint32
	reason string
}

func (e *pduError) Error() string {
	return fmt.Sprintf("nvme/tcp: %s (fes %#x, fei %#x)", e.reason, e.fes, e.fei)
}

// newPduError builds one, with the FEI naming the offending field's byte
// offset inside the PDU where that is meaningful (0 otherwise).
func newPduError(fes uint16, fei uint32, format string, args ...any) *pduError {
	return &pduError{fes: fes, fei: fei, reason: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// Reading (NP2)
// ---------------------------------------------------------------------------

// pdu is one received PDU. hdr is exactly HLEN bytes (the common header
// included) and data is what follows PDO, empty when the PDU carries none.
type pdu struct {
	typ   uint8
	flags uint8
	hlen  uint8
	pdo   uint8
	plen  uint32
	hdr   []byte
	data  []byte
}

// hdrLenFor is the HLEN this controller requires of each PDU type it accepts.
// A PDU type that is not in the table is one only a controller sends, or one
// the specs do not define: either way the connection dies (NP2).
func hdrLenFor(typ uint8) (int, bool) {
	switch typ {
	case pduICReq:
		return icReqLen, true
	case pduH2CTermReq:
		return termReqHdrLen, true
	case pduCapsuleCmd:
		return capsuleCmdHdrLen, true
	case pduH2CData:
		return h2cDataHdrLen, true
	}
	return 0, false
}

// readPdu reads exactly one PDU (NP2). Every malformed input returns a
// *pduError carrying the FES the caller answers with; an EOF or socket error
// returns that error unwrapped, because a host that simply went away is not a
// protocol violation.
func readPdu(r io.Reader) (*pdu, error) {
	var ch [pduCommonHdrLen]byte
	if _, err := io.ReadFull(r, ch[:]); err != nil {
		return nil, err
	}
	p := &pdu{
		typ:   ch[0],
		flags: ch[1],
		hlen:  ch[2],
		pdo:   ch[3],
		plen:  binary.LittleEndian.Uint32(ch[4:8]),
	}
	want, known := hdrLenFor(p.typ)
	if !known {
		return nil, newPduError(fesInvalidPduHdr, 0,
			"unsupported pdu type %#x", p.typ)
	}
	if int(p.hlen) != want {
		return nil, newPduError(fesInvalidPduHdr, 2,
			"pdu type %#x: hlen %d, want %d", p.typ, p.hlen, want)
	}
	if p.plen < uint32(p.hlen) {
		return nil, newPduError(fesInvalidPduHdr, 4,
			"pdu type %#x: plen %d below hlen %d", p.typ, p.plen, p.hlen)
	}
	if p.plen > maxPduLen {
		return nil, newPduError(fesDataLimitExceeded, 4,
			"pdu type %#x: plen %d over the %d byte cap",
			p.typ, p.plen, maxPduLen)
	}
	// PDO is the offset of the data from the start of the PDU. CPDA is 0
	// (NP3), so a host never needs padding; a PDO inside the header or past
	// the PDU is malformed either way.
	if p.plen > uint32(p.hlen) {
		if int(p.pdo) < want || uint32(p.pdo) > p.plen {
			return nil, newPduError(fesInvalidPduHdr, 3,
				"pdu type %#x: pdo %d outside [%d, %d]",
				p.typ, p.pdo, want, p.plen)
		}
	}
	rest := make([]byte, int(p.plen)-pduCommonHdrLen)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	p.hdr = make([]byte, 0, want)
	p.hdr = append(p.hdr, ch[:]...)
	p.hdr = append(p.hdr, rest[:want-pduCommonHdrLen]...)
	if p.plen > uint32(p.hlen) {
		p.data = rest[int(p.pdo)-pduCommonHdrLen:]
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// ICReq / ICResp (NP3)
// ---------------------------------------------------------------------------

// icReq is the decoded connection establishment request.
type icReq struct {
	pfv    uint16
	hpda   uint8
	digest uint8
	maxr2t uint32
}

// parseICReq decodes an ICReq PDU and enforces NP3: PFV must be 0 and the
// host may not demand PDU data alignment this controller does not do.
func parseICReq(p *pdu) (icReq, error) {
	var req icReq
	if p.plen != icReqLen {
		return req, newPduError(fesInvalidPduHdr, 4,
			"icreq: plen %d, want %d", p.plen, icReqLen)
	}
	req.pfv = binary.LittleEndian.Uint16(p.hdr[8:10])
	req.hpda = p.hdr[10]
	req.digest = p.hdr[11]
	req.maxr2t = binary.LittleEndian.Uint32(p.hdr[12:16])
	if req.pfv != pfv10 {
		return req, newPduError(fesUnsupportedParam, 8,
			"icreq: pfv %d, want %d", req.pfv, pfv10)
	}
	if req.hpda != 0 {
		return req, newPduError(fesUnsupportedParam, 10,
			"icreq: hpda %d, want 0", req.hpda)
	}
	return req, nil
}

// buildICResp is the NP3 answer: PFV 0, CPDA 0, BOTH digests off whatever the
// host asked for (a controller enables only what both sides support, and
// dnv-cdc supports neither), MAXH2CDATA = common.CdcMaxH2CData.
func buildICResp() []byte {
	buf := make([]byte, icRespLen)
	buf[0] = pduICResp
	buf[1] = 0
	buf[2] = icRespLen
	buf[3] = 0
	binary.LittleEndian.PutUint32(buf[4:8], icRespLen)
	binary.LittleEndian.PutUint16(buf[8:10], pfv10)
	buf[10] = 0 // CPDA
	buf[11] = 0 // DGST: neither digest
	binary.LittleEndian.PutUint32(buf[12:16], common.CdcMaxH2CData)
	return buf
}

// ---------------------------------------------------------------------------
// C2HTermReq (NP2)
// ---------------------------------------------------------------------------

// buildTermReq is the terminate-connection request sent before the socket is
// closed. It carries no data: the offending header is already in this
// instance's `pdu error` record, and a host that receives a C2HTermReq tears
// the connection down without reading further.
func buildTermReq(fes uint16, fei uint32) []byte {
	buf := make([]byte, termReqHdrLen)
	buf[0] = pduC2HTermReq
	buf[1] = 0
	buf[2] = termReqHdrLen
	buf[3] = 0
	binary.LittleEndian.PutUint32(buf[4:8], termReqHdrLen)
	binary.LittleEndian.PutUint16(buf[8:10], fes)
	binary.LittleEndian.PutUint32(buf[10:14], fei)
	return buf
}

// ---------------------------------------------------------------------------
// Capsules (NP2)
// ---------------------------------------------------------------------------

// completion is one CQE, named field by field so the §8 tests can assert on
// it without decoding bytes.
type completion struct {
	dw0    uint32
	dw1    uint32
	sqhd   uint16
	sqid   uint16
	cid    uint16
	status uint16
}

// buildCapsuleResp wraps one CQE in a CapsuleResp PDU.
func buildCapsuleResp(c completion) []byte {
	buf := make([]byte, capsuleRespLen)
	buf[0] = pduCapsuleResp
	buf[1] = 0
	buf[2] = capsuleRespLen
	buf[3] = 0
	binary.LittleEndian.PutUint32(buf[4:8], capsuleRespLen)
	binary.LittleEndian.PutUint32(buf[8:12], c.dw0)
	binary.LittleEndian.PutUint32(buf[12:16], c.dw1)
	binary.LittleEndian.PutUint16(buf[16:18], c.sqhd)
	binary.LittleEndian.PutUint16(buf[18:20], c.sqid)
	binary.LittleEndian.PutUint16(buf[20:22], c.cid)
	binary.LittleEndian.PutUint16(buf[22:24], c.status)
	return buf
}

// parseCapsuleResp is the inverse, for the in-process fake host of §8.
func parseCapsuleResp(p *pdu) completion {
	return completion{
		dw0:    binary.LittleEndian.Uint32(p.hdr[8:12]),
		dw1:    binary.LittleEndian.Uint32(p.hdr[12:16]),
		sqhd:   binary.LittleEndian.Uint16(p.hdr[16:18]),
		sqid:   binary.LittleEndian.Uint16(p.hdr[18:20]),
		cid:    binary.LittleEndian.Uint16(p.hdr[20:22]),
		status: binary.LittleEndian.Uint16(p.hdr[22:24]),
	}
}

// buildC2HData wraps one controller-to-host payload in a C2HData PDU. dnv-cdc
// sends the whole transfer in one PDU (nvmet does the same), so LAST is always
// set and DATAO is always 0; SUCCESS never is (NP8).
func buildC2HData(cid uint16, payload []byte) []byte {
	plen := c2hDataHdrLen + len(payload)
	buf := make([]byte, plen)
	buf[0] = pduC2HData
	buf[1] = c2hDataLast
	buf[2] = c2hDataHdrLen
	buf[3] = c2hDataHdrLen
	binary.LittleEndian.PutUint32(buf[4:8], uint32(plen))
	binary.LittleEndian.PutUint16(buf[8:10], cid)
	binary.LittleEndian.PutUint16(buf[10:12], 0) // TTAG, controller-assigned
	binary.LittleEndian.PutUint32(buf[12:16], 0) // DATAO
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(payload)))
	copy(buf[c2hDataHdrLen:], payload)
	return buf
}

// ---------------------------------------------------------------------------
// SQE accessors (NP4-NP12)
// ---------------------------------------------------------------------------

// Admin and fabrics opcodes dnv-cdc answers.
const (
	opcGetLogPage  = 0x02
	opcIdentify    = 0x06
	opcSetFeatures = 0x09
	opcGetFeatures = 0x0a
	opcAsyncEvent  = 0x0c
	opcKeepAlive   = 0x18
	opcFabrics     = 0x7f
)

// Fabrics command types (the FCTYPE byte of a fabrics SQE).
const (
	fctypePropertySet = 0x00
	fctypeConnect     = 0x01
	fctypePropertyGet = 0x04
)

// sqe is one 64-byte submission queue entry, addressed by the field names the
// specs use. It never copies: it is a view over the CapsuleCmd header.
type sqe []byte

func (s sqe) opc() uint8    { return s[0] }
func (s sqe) flags() uint8  { return s[1] }
func (s sqe) cid() uint16   { return binary.LittleEndian.Uint16(s[2:4]) }
func (s sqe) nsid() uint32  { return binary.LittleEndian.Uint32(s[4:8]) }
func (s sqe) fctype() uint8 { return s[4] }

// sglLen is the length field of the SQE's one SGL descriptor (bytes 24..39):
// how many bytes of data the host has room for. dnv-cdc never writes more.
func (s sqe) sglLen() uint32 { return binary.LittleEndian.Uint32(s[32:36]) }

// sglType is the SGL identifier byte: descriptor type in the high nibble,
// subtype in the low one.
func (s sqe) sglType() uint8 { return s[39] }

// dw returns command dword n (n >= 10), the command-specific words. CDW10
// starts at byte 40.
func (s sqe) dw(n int) uint32 {
	off := 40 + (n-10)*4
	return binary.LittleEndian.Uint32(s[off : off+4])
}

// Connect (NP5): the fabrics-specific fields past the SGL.
func (s sqe) connectRecfmt() uint16 {
	return binary.LittleEndian.Uint16(s[40:42])
}
func (s sqe) connectQid() uint16 {
	return binary.LittleEndian.Uint16(s[42:44])
}
func (s sqe) connectSqsize() uint16 {
	return binary.LittleEndian.Uint16(s[44:46])
}
func (s sqe) connectCattr() uint8 {
	return s[46]
}
func (s sqe) connectKato() uint32 {
	return binary.LittleEndian.Uint32(s[48:52])
}

// connectDisableSqflow is the Connect CATTR bit that tells the controller to
// stop maintaining SQHD (NP4).
const connectDisableSqflow = 1 << 2

// Property Get/Set (NP6).
func (s sqe) propAttrib() uint8  { return s[40] }
func (s sqe) propOffset() uint32 { return binary.LittleEndian.Uint32(s[44:48]) }
func (s sqe) propValue() uint64  { return binary.LittleEndian.Uint64(s[48:56]) }

// The controller property offsets (NP6).
const (
	regCap  = 0x00
	regVs   = 0x08
	regCc   = 0x14
	regCsts = 0x1c
)

// Get Log Page (NP8): LID, RAE, the two-word NUMD and the two-word LPO.
func (s sqe) logLid() uint8 { return s[40] }
func (s sqe) logRae() bool  { return s[41]&0x80 != 0 }
func (s sqe) logNumd() uint32 {
	numdl := uint32(binary.LittleEndian.Uint16(s[42:44]))
	numdu := uint32(binary.LittleEndian.Uint16(s[44:46]))
	return numdl | numdu<<16
}
func (s sqe) logOffset() uint64 {
	lpol := uint64(binary.LittleEndian.Uint32(s[48:52]))
	lpou := uint64(binary.LittleEndian.Uint32(s[52:56]))
	return lpol | lpou<<32
}

// Identify (NP7).
func (s sqe) identifyCns() uint8 { return s[40] }

// Features (NP9): the FID byte and the value dword.
func (s sqe) featFid() uint8      { return s[40] }
func (s sqe) featDword11() uint32 { return binary.LittleEndian.Uint32(s[44:48]) }

// The two feature identifiers this controller implements (NP9).
const (
	fidAsyncEventConfig = 0x0b
	fidKeepAliveTimer   = 0x0f
)

// ---------------------------------------------------------------------------
// Status codes (NP12)
// ---------------------------------------------------------------------------

// Status code types.
const (
	sctGeneric         = 0x0
	sctCommandSpecific = 0x1
)

// Generic status codes.
const (
	scSuccess       = 0x00
	scInvalidOpcode = 0x01
	scInvalidField  = 0x02
	scInternal      = 0x06
)

// Command-specific status codes, including the fabrics ones.
const (
	scAsyncEventLimit      = 0x05
	scConnectInvalidFormat = 0x80
	scConnectCtrlBusy      = 0x81
	scConnectInvalidParam  = 0x82
	scConnectInvalidHost   = 0x84
)

// dnrFlag is the Do Not Retry bit of a status field, before the one-bit shift
// that makes room for the phase tag.
const dnrFlag = 0x4000

// nvmeStatus assembles a CQE status word: the status field occupies bits 15:1
// and the phase tag, unused on fabrics, bit 0.
func nvmeStatus(sct uint16, sc uint16, dnr bool) uint16 {
	value := sct<<8 | sc
	if dnr {
		value |= dnrFlag
	}
	return value << 1
}

// statusSuccess is the status of every command that worked.
var statusSuccess = nvmeStatus(sctGeneric, scSuccess, false)

// The three statuses NP12 hands out by default.
var (
	statusInvalidField  = nvmeStatus(sctGeneric, scInvalidField, true)
	statusInvalidOpcode = nvmeStatus(sctGeneric, scInvalidOpcode, true)
	statusInternal      = nvmeStatus(sctGeneric, scInternal, true)
	statusAsyncLimit    = nvmeStatus(sctCommandSpecific, scAsyncEventLimit, true)
	statusConnectParam  = nvmeStatus(sctCommandSpecific, scConnectInvalidParam, true)
	statusConnectFormat = nvmeStatus(sctCommandSpecific, scConnectInvalidFormat, true)
	statusConnectHost   = nvmeStatus(sctCommandSpecific, scConnectInvalidHost, true)
)

// connectIpo builds the Connect error response's dword 0: the offset of the
// offending parameter in bits 15:0 and the IATTR bit at 16, which is SET when
// the parameter sits in the 1024 byte connect DATA and clear when it sits in
// the command's SQE. The polarity is the one the Linux host actually parses
// (`if (offset >> 16)` selects "Connect Invalid Data Parameter" in
// nvmf_log_connect_error), which is all this field is ever used for.
func connectIpo(offset uint16, inData bool) uint32 {
	value := uint32(offset)
	if inData {
		value |= 1 << 16
	}
	return value
}

// The connect-data field offsets IPO points at (NP5).
const (
	connectDataHostIdOff  = 0
	connectDataCntlIdOff  = 16
	connectDataSubNqnOff  = 256
	connectDataHostNqnOff = 512
	connectDataLen        = 1024
)

// The connect-command field offsets IPO points at.
const (
	connectCmdRecfmtOff = 40
	connectCmdQidOff    = 42
	connectCmdSqsizeOff = 44
)
