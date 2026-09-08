package cdc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The NP2/NP3 codec tests: pure bytes in, pure bytes out. Nothing here opens a
// socket — readPdu takes an io.Reader and the builders return slices — so every
// assertion is on the wire format the specs pin and the Linux host validates.

// ---------------------------------------------------------------------------
// Framing helpers
// ---------------------------------------------------------------------------

// pduCommonHdr builds the 8 byte common header alone: the fields readPdu
// validates before it reads a single byte more.
func pduCommonHdr(typ, flags, hlen, pdo uint8, plen uint32) []byte {
	buf := make([]byte, pduCommonHdrLen)
	buf[0] = typ
	buf[1] = flags
	buf[2] = hlen
	buf[3] = pdo
	binary.LittleEndian.PutUint32(buf[4:8], plen)
	return buf
}

// pduFrame builds a whole plen byte PDU with that header, the body filled with
// a recognizable ramp so a decode that mis-slices shows up as wrong bytes
// rather than as a length that happens to match.
func pduFrame(typ, flags, hlen, pdo uint8, plen uint32) []byte {
	buf := make([]byte, plen)
	copy(buf, pduCommonHdr(typ, flags, hlen, pdo, plen))
	for i := pduCommonHdrLen; i < len(buf); i++ {
		buf[i] = byte(i)
	}
	return buf
}

// pduWantErr asserts that readPdu refused the frame with the given FES and FEI
// (NP2: every malformed input names the fatal error status the C2HTermReq
// carries).
func pduWantErr(t *testing.T, frame []byte, fes uint16, fei uint32) {
	t.Helper()
	_, err := readPdu(bytes.NewReader(frame))
	var perr *pduError
	if !errors.As(err, &perr) {
		t.Fatalf("readPdu: err %v, want a *pduError", err)
	}
	if perr.fes != fes {
		t.Errorf("fes %#x, want %#x (%v)", perr.fes, fes, perr)
	}
	if perr.fei != fei {
		t.Errorf("fei %#x, want %#x (%v)", perr.fei, fei, perr)
	}
	if perr.reason == "" {
		t.Error("reason is empty; the `pdu error` record needs one")
	}
}

// ---------------------------------------------------------------------------
// readPdu (NP2)
// ---------------------------------------------------------------------------

// TestReadPduDecodesAcceptedTypes proves NP2's accepted set: each PDU a host
// may send decodes into its header and, where it has one, its data.
func TestReadPduDecodesAcceptedTypes(t *testing.T) {
	cmdWithData := pduFrame(
		pduCapsuleCmd, 0, capsuleCmdHdrLen, capsuleCmdHdrLen,
		capsuleCmdHdrLen+connectDataLen,
	)
	tests := []struct {
		name     string
		frame    []byte
		wantTyp  uint8
		wantHlen int
		wantData int
	}{
		{
			name:     "icreq",
			frame:    buildICReqFor(0, 0, 0),
			wantTyp:  pduICReq,
			wantHlen: icReqLen,
		},
		{
			name:     "h2c term req",
			frame:    pduFrame(pduH2CTermReq, 0, termReqHdrLen, 0, termReqHdrLen),
			wantTyp:  pduH2CTermReq,
			wantHlen: termReqHdrLen,
		},
		{
			name: "capsule cmd without data",
			frame: pduFrame(
				pduCapsuleCmd, 0, capsuleCmdHdrLen, 0, capsuleCmdHdrLen,
			),
			wantTyp:  pduCapsuleCmd,
			wantHlen: capsuleCmdHdrLen,
		},
		{
			name:     "capsule cmd with the connect data blob",
			frame:    cmdWithData,
			wantTyp:  pduCapsuleCmd,
			wantHlen: capsuleCmdHdrLen,
			wantData: connectDataLen,
		},
		{
			name: "h2c data",
			frame: pduFrame(
				pduH2CData, 0, h2cDataHdrLen, h2cDataHdrLen, h2cDataHdrLen+16,
			),
			wantTyp:  pduH2CData,
			wantHlen: h2cDataHdrLen,
			wantData: 16,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := readPdu(bytes.NewReader(tt.frame))
			if err != nil {
				t.Fatalf("readPdu: %v", err)
			}
			if p.typ != tt.wantTyp {
				t.Errorf("typ %#x, want %#x", p.typ, tt.wantTyp)
			}
			if int(p.hlen) != tt.wantHlen {
				t.Errorf("hlen %d, want %d", p.hlen, tt.wantHlen)
			}
			if len(p.hdr) != tt.wantHlen {
				t.Errorf("hdr is %d bytes, want %d", len(p.hdr), tt.wantHlen)
			}
			if !bytes.Equal(p.hdr, tt.frame[:tt.wantHlen]) {
				t.Error("hdr is not the frame's first hlen bytes")
			}
			if len(p.data) != tt.wantData {
				t.Fatalf("data is %d bytes, want %d", len(p.data), tt.wantData)
			}
			if tt.wantData > 0 {
				if !bytes.Equal(p.data, tt.frame[int(p.pdo):]) {
					t.Error("data is not the bytes from PDO to the end")
				}
			}
		})
	}
}

// TestReadPduRejectsMalformed proves the NP2 rejects, each with the FES the
// C2HTermReq must carry. The controller-direction PDUs are in the table on
// purpose: a controller that is handed an ICResp, a CapsuleResp or C2HData has
// been sent a PDU of the wrong direction, which is a header error like any
// other.
func TestReadPduRejectsMalformed(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		fes   uint16
		fei   uint32
	}{
		{
			name:  "unknown pdu type",
			frame: pduCommonHdr(0xff, 0, 24, 0, 24),
			fes:   fesInvalidPduHdr,
		},
		{
			name:  "icresp from a host",
			frame: pduCommonHdr(pduICResp, 0, icRespLen, 0, icRespLen),
			fes:   fesInvalidPduHdr,
		},
		{
			name:  "capsule resp from a host",
			frame: pduCommonHdr(pduCapsuleResp, 0, capsuleRespLen, 0, capsuleRespLen),
			fes:   fesInvalidPduHdr,
		},
		{
			name:  "c2h data from a host",
			frame: pduCommonHdr(pduC2HData, 0, c2hDataHdrLen, 0, c2hDataHdrLen),
			fes:   fesInvalidPduHdr,
		},
		{
			name:  "r2t is never part of this protocol",
			frame: pduCommonHdr(pduR2T, 0, 24, 0, 24),
			fes:   fesInvalidPduHdr,
		},
		{
			name:  "icreq with the wrong hlen",
			frame: pduCommonHdr(pduICReq, 0, icReqLen-1, 0, icReqLen),
			fes:   fesInvalidPduHdr,
			fei:   2,
		},
		{
			name:  "capsule cmd with the wrong hlen",
			frame: pduCommonHdr(pduCapsuleCmd, 0, 64, 0, capsuleCmdHdrLen),
			fes:   fesInvalidPduHdr,
			fei:   2,
		},
		{
			name:  "plen below hlen",
			frame: pduCommonHdr(pduCapsuleCmd, 0, capsuleCmdHdrLen, 0, 40),
			fes:   fesInvalidPduHdr,
			fei:   4,
		},
		{
			name: "plen over the in-capsule data cap",
			frame: pduCommonHdr(
				pduCapsuleCmd, 0, capsuleCmdHdrLen, capsuleCmdHdrLen,
				maxPduLen+1,
			),
			fes: fesDataLimitExceeded,
			fei: 4,
		},
		{
			name: "pdo inside the header",
			frame: pduFrame(
				pduCapsuleCmd, 0, capsuleCmdHdrLen, pduCommonHdrLen,
				capsuleCmdHdrLen+8,
			),
			fes: fesInvalidPduHdr,
			fei: 3,
		},
		{
			name: "pdo past the end of the pdu",
			frame: pduFrame(
				pduCapsuleCmd, 0, capsuleCmdHdrLen, 200, capsuleCmdHdrLen+8,
			),
			fes: fesInvalidPduHdr,
			fei: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pduWantErr(t, tt.frame, tt.fes, tt.fei)
		})
	}
}

// TestReadPduTruncatedStreamIsNotAProtocolError proves the NP2 split: a host
// that simply went away mid-PDU yields the io error unwrapped, so the
// connection is closed as `closed` and not answered with a C2HTermReq.
func TestReadPduTruncatedStreamIsNotAProtocolError(t *testing.T) {
	full := pduFrame(
		pduCapsuleCmd, 0, capsuleCmdHdrLen, capsuleCmdHdrLen,
		capsuleCmdHdrLen+64,
	)
	tests := []struct {
		name  string
		frame []byte
		want  error
	}{
		{name: "nothing at all", frame: nil, want: io.EOF},
		{name: "half a common header", frame: full[:4], want: io.ErrUnexpectedEOF},
		{name: "header without its body", frame: full[:40], want: io.ErrUnexpectedEOF},
		{name: "body cut short", frame: full[:len(full)-8], want: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readPdu(bytes.NewReader(tt.frame))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err %v, want %v", err, tt.want)
			}
			var perr *pduError
			if errors.As(err, &perr) {
				t.Fatalf("a short read must not be a pduError: %v", perr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ICReq / ICResp (NP3)
// ---------------------------------------------------------------------------

// TestParseICReq proves NP3's acceptance rules: PFV 0 and HPDA 0, everything
// else refused with an unsupported-parameter FES naming the offending field's
// offset.
func TestParseICReq(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		frame := buildICReqFor(pfv10, 0, digestHdrEnable|digestDataEnable)
		binary.LittleEndian.PutUint32(frame[12:16], 7) // MAXR2T
		p, err := readPdu(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("readPdu: %v", err)
		}
		req, err := parseICReq(p)
		if err != nil {
			t.Fatalf("parseICReq: %v", err)
		}
		if req.pfv != pfv10 {
			t.Errorf("pfv %d, want %d", req.pfv, pfv10)
		}
		if req.hpda != 0 {
			t.Errorf("hpda %d, want 0", req.hpda)
		}
		// The request's digest bits are decoded, not honored: NP3's answer
		// disables both whatever the host asked for.
		if req.digest != digestHdrEnable|digestDataEnable {
			t.Errorf("digest %#x, want %#x",
				req.digest, digestHdrEnable|digestDataEnable)
		}
		if req.maxr2t != 7 {
			t.Errorf("maxr2t %d, want 7", req.maxr2t)
		}
	})

	tests := []struct {
		name  string
		frame []byte
		fes   uint16
		fei   uint32
	}{
		{
			name:  "pfv is not 1.0",
			frame: buildICReqFor(1, 0, 0),
			fes:   fesUnsupportedParam,
			fei:   8,
		},
		{
			name:  "host demands pdu data alignment",
			frame: buildICReqFor(0, 1, 0),
			fes:   fesUnsupportedParam,
			fei:   10,
		},
		{
			name: "icreq longer than the spec's 128 bytes",
			frame: pduFrame(
				pduICReq, 0, icReqLen, icReqLen, icReqLen+8,
			),
			fes: fesInvalidPduHdr,
			fei: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := readPdu(bytes.NewReader(tt.frame))
			if err != nil {
				t.Fatalf("readPdu: %v", err)
			}
			_, err = parseICReq(p)
			var perr *pduError
			if !errors.As(err, &perr) {
				t.Fatalf("parseICReq: err %v, want a *pduError", err)
			}
			if perr.fes != tt.fes || perr.fei != tt.fei {
				t.Errorf("fes/fei %#x/%#x, want %#x/%#x",
					perr.fes, perr.fei, tt.fes, tt.fei)
			}
		})
	}
}

// TestBuildICRespGolden is the NP3 answer byte for byte: PFV 0, CPDA 0, BOTH
// digest bits off, MAXH2CDATA = common.CdcMaxH2CData, and nothing else set.
func TestBuildICRespGolden(t *testing.T) {
	got := buildICResp()
	if len(got) != icRespLen {
		t.Fatalf("icresp is %d bytes, want %d", len(got), icRespLen)
	}
	want := make([]byte, icRespLen)
	want[0] = pduICResp                                 // PDU type
	want[2] = icRespLen                                 // HLEN
	binary.LittleEndian.PutUint32(want[4:8], icRespLen) // PLEN
	binary.LittleEndian.PutUint16(want[8:10], pfv10)    // PFV
	want[10] = 0                                        // CPDA
	want[11] = 0                                        // DGST
	binary.LittleEndian.PutUint32(want[12:16], common.CdcMaxH2CData)
	if !bytes.Equal(got, want) {
		t.Fatalf("icresp\n got %v\nwant %v", got[:16], want[:16])
	}
}

// ---------------------------------------------------------------------------
// C2HTermReq (NP2)
// ---------------------------------------------------------------------------

// TestBuildTermReqGolden pins the terminate-connection request's layout: FES
// at byte 8 and FEI at byte 10, the two fields a host logs when dnv-cdc kills
// a connection.
func TestBuildTermReqGolden(t *testing.T) {
	got := buildTermReq(fesDataLimitExceeded, 0x11223344)
	if len(got) != termReqHdrLen {
		t.Fatalf("term req is %d bytes, want %d", len(got), termReqHdrLen)
	}
	want := make([]byte, termReqHdrLen)
	want[0] = pduC2HTermReq
	want[2] = termReqHdrLen
	binary.LittleEndian.PutUint32(want[4:8], termReqHdrLen)
	binary.LittleEndian.PutUint16(want[8:10], fesDataLimitExceeded)
	binary.LittleEndian.PutUint32(want[10:14], 0x11223344)
	if !bytes.Equal(got, want) {
		t.Fatalf("term req\n got %v\nwant %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Capsules (NP2)
// ---------------------------------------------------------------------------

// TestBuildCapsuleRespGoldenAndRoundTrip pins the CQE's byte offsets and
// proves parseCapsuleResp is buildCapsuleResp's exact inverse.
func TestBuildCapsuleRespGoldenAndRoundTrip(t *testing.T) {
	c := completion{
		dw0:    aenDiscLogChanged,
		dw1:    0x89abcdef,
		sqhd:   0x0102,
		sqid:   0,
		cid:    0xbeef,
		status: statusInvalidField,
	}
	got := buildCapsuleResp(c)
	if len(got) != capsuleRespLen {
		t.Fatalf("capsule resp is %d bytes, want %d", len(got), capsuleRespLen)
	}
	want := make([]byte, capsuleRespLen)
	want[0] = pduCapsuleResp
	want[2] = capsuleRespLen
	binary.LittleEndian.PutUint32(want[4:8], capsuleRespLen)
	binary.LittleEndian.PutUint32(want[8:12], c.dw0)
	binary.LittleEndian.PutUint32(want[12:16], c.dw1)
	binary.LittleEndian.PutUint16(want[16:18], c.sqhd)
	binary.LittleEndian.PutUint16(want[18:20], c.sqid)
	binary.LittleEndian.PutUint16(want[20:22], c.cid)
	binary.LittleEndian.PutUint16(want[22:24], c.status)
	if !bytes.Equal(got, want) {
		t.Fatalf("capsule resp\n got %v\nwant %v", got, want)
	}
	p, err := readCtrlPdu(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("readCtrlPdu: %v", err)
	}
	if back := parseCapsuleResp(p); back != c {
		t.Fatalf("round trip: %+v, want %+v", back, c)
	}
}

// TestBuildC2HDataGolden pins the C2HData layout of NP8: one PDU carries the
// whole transfer, so LAST is set, DATAO is 0 and SUCCESS is deliberately never
// set (the CapsuleResp that follows is what keeps SQHD flowing).
func TestBuildC2HDataGolden(t *testing.T) {
	payload := make([]byte, common.CdcDiscLogHeaderSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	got := buildC2HData(0x0304, payload)
	if len(got) != c2hDataHdrLen+len(payload) {
		t.Fatalf("c2hdata is %d bytes, want %d",
			len(got), c2hDataHdrLen+len(payload))
	}
	if got[0] != pduC2HData {
		t.Errorf("pdu type %#x, want %#x", got[0], pduC2HData)
	}
	// The FLAGS byte is pinned as a literal: the specs order the bits
	// HDGSTF, DDGSTF, LAST_PDU, SUCCESS, so a single-PDU transfer with no
	// digests and no SUCCESS shortcut is exactly 0x04. Asserting through
	// the constants alone would survive them being defined one bit too low,
	// which is a real bug a Linux host happens not to notice.
	if got[1] != 0x04 {
		t.Errorf("flags %#02x, want 0x04 (LAST_PDU only)", got[1])
	}
	if got[1]&c2hDataLast == 0 {
		t.Error("LAST is not set on a single-PDU transfer")
	}
	if got[1]&c2hDataSuccess != 0 {
		t.Error("SUCCESS is set; NP8 keeps the CapsuleResp instead")
	}
	if got[1]&(pduFlagHdrDigest|pduFlagDataDigest) != 0 {
		t.Error("a digest-present flag is set; neither digest is negotiated")
	}
	if got[2] != c2hDataHdrLen {
		t.Errorf("hlen %d, want %d", got[2], c2hDataHdrLen)
	}
	if got[3] != c2hDataHdrLen {
		t.Errorf("pdo %d, want %d", got[3], c2hDataHdrLen)
	}
	if plen := binary.LittleEndian.Uint32(got[4:8]); int(plen) != len(got) {
		t.Errorf("plen %d, want %d", plen, len(got))
	}
	if cid := binary.LittleEndian.Uint16(got[8:10]); cid != 0x0304 {
		t.Errorf("cccid %#x, want %#x", cid, 0x0304)
	}
	if ttag := binary.LittleEndian.Uint16(got[10:12]); ttag != 0 {
		t.Errorf("ttag %d, want 0", ttag)
	}
	if datao := binary.LittleEndian.Uint32(got[12:16]); datao != 0 {
		t.Errorf("datao %d, want 0", datao)
	}
	if datal := binary.LittleEndian.Uint32(got[16:20]); int(datal) != len(payload) {
		t.Errorf("datal %d, want %d", datal, len(payload))
	}
	if !bytes.Equal(got[c2hDataHdrLen:], payload) {
		t.Error("payload is not at PDO")
	}
	p, err := readCtrlPdu(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("readCtrlPdu: %v", err)
	}
	if !bytes.Equal(p.data, payload) {
		t.Error("round trip lost the payload")
	}
}

// ---------------------------------------------------------------------------
// Status words (NP12) and Connect's IPO (NP5)
// ---------------------------------------------------------------------------

// TestNvmeStatusBitLayout proves the CQE status word's layout: the status
// field occupies bits 15:1 (SC in 8:1, SCT in 11:9), DNR lands in bit 15 of
// the final word and bit 0 is the phase tag fabrics leaves alone.
func TestNvmeStatusBitLayout(t *testing.T) {
	tests := []struct {
		name string
		sct  uint16
		sc   uint16
		dnr  bool
		want uint16
	}{
		{name: "success", sct: sctGeneric, sc: scSuccess, want: 0},
		{
			name: "invalid field, dnr",
			sct:  sctGeneric,
			sc:   scInvalidField,
			dnr:  true,
			want: (0x4000 | 0x02) << 1,
		},
		{
			name: "async event limit is command specific",
			sct:  sctCommandSpecific,
			sc:   scAsyncEventLimit,
			dnr:  true,
			want: (0x4000 | 0x105) << 1,
		},
		{
			name: "connect invalid parameters",
			sct:  sctCommandSpecific,
			sc:   scConnectInvalidParam,
			dnr:  true,
			want: (0x4000 | 0x182) << 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nvmeStatus(tt.sct, tt.sc, tt.dnr)
			if got != tt.want {
				t.Fatalf("status %#06x, want %#06x", got, tt.want)
			}
			if got&0x1 != 0 {
				t.Error("bit 0 is the phase tag and must stay clear")
			}
			if sc := (got >> 1) & 0xff; sc != tt.sc {
				t.Errorf("sc %#x, want %#x", sc, tt.sc)
			}
			if sct := (got >> 9) & 0x7; sct != tt.sct {
				t.Errorf("sct %#x, want %#x", sct, tt.sct)
			}
			if dnr := got>>15 != 0; dnr != tt.dnr {
				t.Errorf("dnr %v, want %v", dnr, tt.dnr)
			}
		})
	}
	// The package's shared statuses must be the ones NP12 hands out, DNR
	// included: a host that retries an invalid field forever is a bug.
	for name, status := range map[string]uint16{
		"invalid field":  statusInvalidField,
		"invalid opcode": statusInvalidOpcode,
		"async limit":    statusAsyncLimit,
		"connect param":  statusConnectParam,
		"connect format": statusConnectFormat,
		"connect host":   statusConnectHost,
	} {
		if status>>15 == 0 {
			t.Errorf("%s: dnr is not set (%#06x)", name, status)
		}
	}
	if statusSuccess != 0 {
		t.Errorf("statusSuccess %#06x, want 0", statusSuccess)
	}
}

// TestConnectIpoIattrBit proves NP5's error pointer: the parameter's byte
// offset in the low word and IATTR in bit 16, SET when the offending field is
// in the connect DATA and clear when it is in the command's SQE. The `want`
// values are literals, not connectIpo's own output, because the polarity is
// the whole point: the Linux host selects "Connect Invalid Data Parameter"
// with `if (offset >> 16)`.
func TestConnectIpoIattrBit(t *testing.T) {
	tests := []struct {
		name   string
		offset uint16
		inData bool
		want   uint32
	}{
		{
			name:   "subnqn lives in the connect data",
			offset: connectDataSubNqnOff,
			inData: true,
			want:   1<<16 | 256,
		},
		{
			name:   "hostnqn lives in the connect data",
			offset: connectDataHostNqnOff,
			inData: true,
			want:   1<<16 | 512,
		},
		{
			name:   "sqsize lives in the command",
			offset: connectCmdSqsizeOff,
			want:   44,
		},
		{
			name:   "qid lives in the command",
			offset: connectCmdQidOff,
			want:   42,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := connectIpo(tt.offset, tt.inData)
			if got != tt.want {
				t.Fatalf("ipo %#x, want %#x", got, tt.want)
			}
			if iattr := got>>16&1 != 0; iattr != tt.inData {
				t.Errorf("iattr %v, want %v", iattr, tt.inData)
			}
			if off := uint16(got); off != tt.offset {
				t.Errorf("offset %d, want %d", off, tt.offset)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SQE accessors (NP4-NP12)
// ---------------------------------------------------------------------------

// TestSqeAccessorsReadSpecOffsets builds submission queue entries by hand and
// proves every accessor reads the specs' byte offsets — in particular the two
// split fields of Get Log Page, NUMD across NUMDL/NUMDU and LPO across
// LPOL/LPOU, which is what makes paging a discovery log work at all (NP8).
func TestSqeAccessorsReadSpecOffsets(t *testing.T) {
	t.Run("common header", func(t *testing.T) {
		s := make(sqe, sqeLen)
		s[0] = opcFabrics
		s[1] = 0x40
		binary.LittleEndian.PutUint16(s[2:4], 0xbeef)
		binary.LittleEndian.PutUint32(s[4:8], 0x11223344)
		binary.LittleEndian.PutUint32(s[32:36], 0x1000)
		s[39] = 0x5<<4 | 0xa
		binary.LittleEndian.PutUint32(s[40:44], 0xaaaa0001)
		binary.LittleEndian.PutUint32(s[44:48], 0xaaaa0002)
		binary.LittleEndian.PutUint32(s[60:64], 0xaaaa0005)
		if s.opc() != opcFabrics {
			t.Errorf("opc %#x, want %#x", s.opc(), opcFabrics)
		}
		if s.flags() != 0x40 {
			t.Errorf("flags %#x, want %#x", s.flags(), 0x40)
		}
		if s.cid() != 0xbeef {
			t.Errorf("cid %#x, want %#x", s.cid(), 0xbeef)
		}
		if s.nsid() != 0x11223344 {
			t.Errorf("nsid %#x, want %#x", s.nsid(), 0x11223344)
		}
		// FCTYPE overlays the first byte of NSID: a fabrics command has no
		// namespace.
		if s.fctype() != 0x44 {
			t.Errorf("fctype %#x, want %#x", s.fctype(), 0x44)
		}
		if s.sglLen() != 0x1000 {
			t.Errorf("sgl len %#x, want %#x", s.sglLen(), 0x1000)
		}
		if s.sglType() != 0x5a {
			t.Errorf("sgl type %#x, want %#x", s.sglType(), 0x5a)
		}
		if s.dw(10) != 0xaaaa0001 {
			t.Errorf("cdw10 %#x, want %#x", s.dw(10), 0xaaaa0001)
		}
		if s.dw(11) != 0xaaaa0002 {
			t.Errorf("cdw11 %#x, want %#x", s.dw(11), 0xaaaa0002)
		}
		if s.dw(15) != 0xaaaa0005 {
			t.Errorf("cdw15 %#x, want %#x", s.dw(15), 0xaaaa0005)
		}
	})

	t.Run("connect", func(t *testing.T) {
		s := make(sqe, sqeLen)
		binary.LittleEndian.PutUint16(s[40:42], 1)
		binary.LittleEndian.PutUint16(s[42:44], 3)
		binary.LittleEndian.PutUint16(s[44:46], common.CdcMaxAdminSqSize-1)
		s[46] = connectDisableSqflow
		binary.LittleEndian.PutUint32(s[48:52], 30000)
		if s.connectRecfmt() != 1 {
			t.Errorf("recfmt %d, want 1", s.connectRecfmt())
		}
		if s.connectQid() != 3 {
			t.Errorf("qid %d, want 3", s.connectQid())
		}
		if s.connectSqsize() != common.CdcMaxAdminSqSize-1 {
			t.Errorf("sqsize %d, want %d",
				s.connectSqsize(), common.CdcMaxAdminSqSize-1)
		}
		if s.connectCattr()&connectDisableSqflow == 0 {
			t.Errorf("cattr %#x lost the disable-sqflow bit", s.connectCattr())
		}
		if s.connectKato() != 30000 {
			t.Errorf("kato %d, want 30000", s.connectKato())
		}
	})

	t.Run("property get and set", func(t *testing.T) {
		s := make(sqe, sqeLen)
		s[40] = 1 // ATTRIB: the 8 byte form
		binary.LittleEndian.PutUint32(s[44:48], regCsts)
		binary.LittleEndian.PutUint64(s[48:56], 0x0123456789abcdef)
		if s.propAttrib() != 1 {
			t.Errorf("attrib %d, want 1", s.propAttrib())
		}
		if s.propOffset() != regCsts {
			t.Errorf("offset %#x, want %#x", s.propOffset(), regCsts)
		}
		if s.propValue() != 0x0123456789abcdef {
			t.Errorf("value %#x, want %#x",
				s.propValue(), uint64(0x0123456789abcdef))
		}
	})

	t.Run("get log page", func(t *testing.T) {
		s := make(sqe, sqeLen)
		s[40] = lidDiscovery
		s[41] = 0x80 // RAE
		binary.LittleEndian.PutUint16(s[42:44], 0xfffe)
		binary.LittleEndian.PutUint16(s[44:46], 0x0001)
		binary.LittleEndian.PutUint32(s[48:52], 0x89abcdef)
		binary.LittleEndian.PutUint32(s[52:56], 0x01234567)
		if s.logLid() != lidDiscovery {
			t.Errorf("lid %#x, want %#x", s.logLid(), lidDiscovery)
		}
		if !s.logRae() {
			t.Error("rae is bit 15 of cdw10 and was not read")
		}
		if s.logNumd() != 0x0001fffe {
			t.Errorf("numd %#x, want %#x", s.logNumd(), 0x0001fffe)
		}
		if s.logOffset() != 0x0123456789abcdef {
			t.Errorf("lpo %#x, want %#x",
				s.logOffset(), uint64(0x0123456789abcdef))
		}
		s[41] = 0x7f
		if s.logRae() {
			t.Error("rae read a bit that is not bit 15 of cdw10")
		}
	})

	t.Run("identify and features", func(t *testing.T) {
		s := make(sqe, sqeLen)
		s[40] = cnsController
		if s.identifyCns() != cnsController {
			t.Errorf("cns %#x, want %#x", s.identifyCns(), cnsController)
		}
		s[40] = fidAsyncEventConfig
		binary.LittleEndian.PutUint32(s[44:48], aenCfgDiscChange)
		if s.featFid() != fidAsyncEventConfig {
			t.Errorf("fid %#x, want %#x", s.featFid(), fidAsyncEventConfig)
		}
		if s.featDword11() != aenCfgDiscChange {
			t.Errorf("cdw11 %#x, want %#x",
				s.featDword11(), uint32(aenCfgDiscChange))
		}
	})
}
