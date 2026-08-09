package main

import (
	"encoding/binary"
)

type tcpHdr struct {
	Type  uint8
	Flags uint8
	Hlen  uint8
	Pdo   uint8
	Plen  uint32
}

const (
	pduICReq      = 0x0
	pduICResp     = 0x1
	pduH2CTerm    = 0x2
	pduC2HTerm    = 0x3
	pduCmd        = 0x4
	pduRsp        = 0x5
	pduH2CData    = 0x6
	pduC2HData    = 0x7
	pduR2T        = 0x9
	flagDataLast  = 1 << 2
	flagDataSucc  = 1 << 3
	flagHDGST     = 1 << 0
	flagDDGST     = 1 << 1
	icreqSize     = 128
	icrespSize    = 128
	cmdPduHdrSize = 72
	rspPduHdrSize = 24
	dataPduHdrSz  = 24
)

type icreq struct {
	Hdr    tcpHdr
	Pfv    uint16
	Hpda   uint8
	Digest uint8
	Maxr2t uint32
	Rsvd   [112]byte
}

type icresp struct {
	Hdr    tcpHdr
	Pfv    uint16
	Cpda   uint8
	Digest uint8
	Maxdat uint32
	Rsvd   [112]byte
}

func writeTcpHdr(b []byte, t, flags, hlen, pdo uint8, plen uint32) {
	b[0] = t
	b[1] = flags
	b[2] = hlen
	b[3] = pdo
	binary.LittleEndian.PutUint32(b[4:8], plen)
}

func readTcpHdr(b []byte) tcpHdr {
	return tcpHdr{
		Type:  b[0],
		Flags: b[1],
		Hlen:  b[2],
		Pdo:   b[3],
		Plen:  binary.LittleEndian.Uint32(b[4:8]),
	}
}

func buildICResp() [icrespSize]byte {
	var b [icrespSize]byte
	writeTcpHdr(b[:], pduICResp, 0, icrespSize, 0, icrespSize)
	binary.LittleEndian.PutUint16(b[8:10], 0)
	b[10] = 0
	b[11] = 0
	binary.LittleEndian.PutUint32(b[12:16], 0)
	return b
}

type cqe struct {
	Result    uint64
	SqHead    uint16
	SqID      uint16
	CommandID uint16
	Status    uint16
}

func buildRspPdu(cid uint16, status uint16, result uint64) [rspPduHdrSize]byte {
	var b [rspPduHdrSize]byte
	writeTcpHdr(b[:], pduRsp, flagDataLast, rspPduHdrSize, 0, rspPduHdrSize)
	binary.LittleEndian.PutUint64(b[8:16], result)
	binary.LittleEndian.PutUint16(b[16:18], 0)
	binary.LittleEndian.PutUint16(b[18:20], 0)
	binary.LittleEndian.PutUint16(b[20:22], cid)
	binary.LittleEndian.PutUint16(b[22:24], status)
	return b
}

func buildDataHdr(cid uint16, datalen uint32) [dataPduHdrSz]byte {
	var b [dataPduHdrSz]byte
	writeTcpHdr(b[:], pduC2HData, flagDataLast, dataPduHdrSz, dataPduHdrSz, dataPduHdrSz+datalen)
	binary.LittleEndian.PutUint16(b[8:10], cid)
	binary.LittleEndian.PutUint16(b[10:12], 0)
	binary.LittleEndian.PutUint32(b[12:16], 0)
	binary.LittleEndian.PutUint32(b[16:20], datalen)
	return b
}

const (
	opIdentify   = 0x06
	opGetLog     = 0x02
	opKeepAlive  = 0x18
	opAsyncEvt   = 0x0c
	opSetFeat    = 0x09
	opGetFeat    = 0x0a
	opFabrics    = 0x7f
	opDisconnect = 0x84

	fctypeConnect  = 0x01
	fctypePropSet  = 0x00
	fctypePropGet  = 0x04
	fctypeAuthSend = 0x05
	fctypeAuthRecv = 0x06

	scSuccess        = 0x0000
	scInvalidOpcode  = 0x0001
	scInvalidField   = 0x0002
	scInternal       = 0x0006
	scConnectFmt     = 0x0180
	scConnectBusy    = 0x0181
	scConnectInvalid = 0x0182
	statusDNR        = 0x4000

	lidDiscovery = 0x70

	aenNotice        = 0x02
	aenNoticeDiscChg = 0xf0
	logDisc          = 0x70

	featAEN  = 0x0b
	featKATO = 0x0f

	identifyDataSize = 4096
	discLogHdrSize   = 1024
	discLogEntrySize = 1024
)
