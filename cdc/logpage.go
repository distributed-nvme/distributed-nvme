package cdc

import (
	"encoding/binary"
	"hash/fnv"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the DS3/DS9 rendering half of the discovery service: one
// CdcEntry transport-configuration element becomes one discovery log entry,
// and a host's entries plus a header become the bytes a Get Log Page command
// serves. The byte layout is the specs'; this file names the fields.

// ---------------------------------------------------------------------------
// Discovery log entry layout (DS3)
// ---------------------------------------------------------------------------

// The field offsets inside one common.CdcDiscLogEntrySize entry.
const (
	entryOffTrType   = 0
	entryOffAdrFam   = 1
	entryOffSubtype  = 2
	entryOffTreq     = 3
	entryOffPortId   = 4
	entryOffCntlId   = 6
	entryOffAsqsz    = 8
	entryOffEflags   = 10
	entryOffTrSvcId  = 32
	entryLenTrSvcId  = 32
	entryOffSubNqn   = 256
	entryLenSubNqn   = 256
	entryOffTrAddr   = 512
	entryLenTrAddr   = 256
	entryOffTsas     = 768
	entryOffTcpSecTp = entryOffTsas + 0
)

// The field VALUES of DS3. TRTYPE/ADRFAM are the specs' transport and address
// family codes; SUBTYPE says these entries name NVM subsystems, never further
// discovery services (dnv-cdc serves no referrals, §0 #1).
const (
	trTypeTcp = 3

	adrFamIpv4 = 1
	adrFamIpv6 = 2

	subtypeNvmSubsystem = 2

	// TREQ "not specified": dnv exports no TLS, so a host is neither
	// required nor forbidden to use it by this record.
	treqNotSpecified = 0

	// CNTLID 0xffff means "dynamic controller": the host asks the target
	// for whatever controller id it likes, which is what every dnv nvmet
	// port hands out.
	cntlIdDynamic = 0xffff

	// TSAS for TCP is one byte, the security type; none means plain TCP.
	tcpSecTypeNone = 0
)

// ---------------------------------------------------------------------------
// Log page header layout (DS9)
// ---------------------------------------------------------------------------

const (
	hdrOffGenCtr = 0
	hdrOffNumRec = 8
	hdrOffRecFmt = 16
)

// recFmtV0 is the only discovery log record format the specs define.
const recFmtV0 = 0

// ---------------------------------------------------------------------------
// Rendering (DS3)
// ---------------------------------------------------------------------------

// trTypeCode maps a CdcEntry transport type to its TRTYPE byte. Only tcp is
// implemented (§0 #2); anything else makes the element unrenderable and the
// caller skips it with `cdc entry skipped`, reason foreign_tr_type.
func trTypeCode(trType string) (uint8, bool) {
	if trType == common.DefaultCdcTrType {
		return trTypeTcp, true
	}
	return 0, false
}

// adrFamCode maps a CdcEntry address family to its ADRFAM byte.
func adrFamCode(adrFam string) (uint8, bool) {
	switch adrFam {
	case common.DefaultCdcAdrFam:
		return adrFamIpv4, true
	case common.CdcAdrFamIpv6:
		return adrFamIpv6, true
	}
	return 0, false
}

// portId is the DS3 PORTID: the low 16 bits of fnv64a over
// "<tr_addr> <tr_svc_id>". It is deterministic and equal for the same CN port
// wherever that port appears, which is the only property the field has to
// have — it is informational, so a collision costs nothing.
func portId(trAddr string, trSvcId string) uint16 {
	h := fnv.New64a()
	// hash.Hash.Write never returns an error.
	h.Write([]byte(trAddr))
	h.Write([]byte(" "))
	h.Write([]byte(trSvcId))
	return uint16(h.Sum64())
}

// putField copies a string into a fixed-size, NUL-padded character field, the
// way every NVMe string field on the wire is shaped. A value longer than the
// field is truncated rather than rejected: dnv itself never writes one (an NQN
// is capped at common.MaxNqnLength, below the 256 byte field), and a truncated
// record still serves the rest of the entry.
func putField(dst []byte, value string) {
	n := copy(dst, value)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

// renderEntry renders one transport-configuration element of one CdcEntry
// into one discovery log entry (DS3). The second return is "" on success and
// otherwise the §7 `cdc entry skipped` reason that dropped the element — a
// transport or an address family dnv-cdc cannot name has no byte to put in
// the record. The rest of the entry still serves either way.
func renderEntry(nqn string, conf *pb.NvmeTrConf) ([]byte, string) {
	trType, ok := trTypeCode(conf.GetTrType())
	if !ok {
		return nil, skipForeignTrType
	}
	adrFam, ok := adrFamCode(conf.GetAdrFam())
	if !ok {
		return nil, skipForeignAdrFam
	}
	buf := make([]byte, common.CdcDiscLogEntrySize)
	buf[entryOffTrType] = trType
	buf[entryOffAdrFam] = adrFam
	buf[entryOffSubtype] = subtypeNvmSubsystem
	buf[entryOffTreq] = treqNotSpecified
	binary.LittleEndian.PutUint16(
		buf[entryOffPortId:entryOffPortId+2],
		portId(conf.GetTrAddr(), conf.GetTrSvcId()),
	)
	binary.LittleEndian.PutUint16(
		buf[entryOffCntlId:entryOffCntlId+2], cntlIdDynamic,
	)
	binary.LittleEndian.PutUint16(
		buf[entryOffAsqsz:entryOffAsqsz+2], common.CdcMaxAdminSqSize,
	)
	binary.LittleEndian.PutUint16(buf[entryOffEflags:entryOffEflags+2], 0)
	putField(
		buf[entryOffTrSvcId:entryOffTrSvcId+entryLenTrSvcId],
		conf.GetTrSvcId(),
	)
	putField(buf[entryOffSubNqn:entryOffSubNqn+entryLenSubNqn], nqn)
	putField(buf[entryOffTrAddr:entryOffTrAddr+entryLenTrAddr], conf.GetTrAddr())
	buf[entryOffTcpSecTp] = tcpSecTypeNone
	return buf, ""
}

// ---------------------------------------------------------------------------
// Serving (DS9)
// ---------------------------------------------------------------------------

// renderHeader builds the log page's header block: GENCTR, NUMREC and the
// record format, the rest reserved.
func renderHeader(genCtr uint64, numRec uint64) []byte {
	buf := make([]byte, common.CdcDiscLogHeaderSize)
	binary.LittleEndian.PutUint64(buf[hdrOffGenCtr:hdrOffGenCtr+8], genCtr)
	binary.LittleEndian.PutUint64(buf[hdrOffNumRec:hdrOffNumRec+8], numRec)
	binary.LittleEndian.PutUint16(buf[hdrOffRecFmt:hdrOffRecFmt+2], recFmtV0)
	return buf
}

// logPageBytes serves one Get Log Page read out of a snapshot (DS9): the
// header block at offset 0, entry i at
// common.CdcDiscLogHeaderSize + i * common.CdcDiscLogEntrySize, and zeros past
// the end. body is the concatenation of the snapshot's rendered entries, so
// its length is always a whole multiple of the entry size.
//
// The returned slice is always exactly length bytes, which is what the host's
// buffer expects; an offset past the end therefore yields all zeros rather
// than an error, as the specs require of a log page read.
func logPageBytes(genCtr uint64, numRec uint64, body []byte, off uint64, length int) []byte {
	out := make([]byte, length)
	if length == 0 {
		return out
	}
	header := renderHeader(genCtr, numRec)
	copyFrom(out, header, off, 0)
	copyFrom(out, body, off, uint64(len(header)))
	return out
}

// copyFrom copies the part of src that the read window [off, off+len(out))
// overlaps, where src starts at srcStart in the log's flat address space.
func copyFrom(out []byte, src []byte, off uint64, srcStart uint64) {
	srcEnd := srcStart + uint64(len(src))
	readEnd := off + uint64(len(out))
	if off >= srcEnd || readEnd <= srcStart {
		return
	}
	from := uint64(0)
	if off > srcStart {
		from = off - srcStart
	}
	to := uint64(len(src))
	if readEnd < srcEnd {
		to = readEnd - srcStart
	}
	dst := uint64(0)
	if srcStart > off {
		dst = srcStart - off
	}
	copy(out[dst:], src[from:to])
}
