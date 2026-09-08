package cdc

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The DS3/DS9 byte-level tests: one CdcEntry transport configuration rendered
// to exact bytes, the log page header rendered to exact bytes, and the paging
// arithmetic a Get Log Page read is served with.
//
// Every offset and every value in this file is written as a LITERAL, never as
// one of logpage.go's own constants: a golden that imports the constants it is
// pinning proves nothing, because a layout change moves both sides at once.

// The fixture the goldens are taken from. The addresses are the lab's CN
// endpoints (§9.3) and the NQN is dnv-shaped, so the padded string fields are
// exercised at realistic lengths.
const (
	lpGoldenNqn   = "nqn.2024-01.io.dnv:cdc-golden-ss"
	lpGoldenAddr  = "192.168.0.21"
	lpGoldenAddr6 = "fd00::21"
	lpGoldenSvcId = "4420"
	lpOtherSvcId  = "4421"
	lpOtherAddr   = "192.168.0.22"
)

// The pinned PORTIDs of DS3: the low 16 bits of fnv64a("<addr> <svcid>"). They
// are goldens in the strongest sense — a host that has already seen a port
// under one PORTID must keep seeing it under the same one, so changing the
// hash is a wire-visible change, not a refactor.
const (
	lpPortIdAddr1Svc4420 = 0xd485
	lpPortIdAddr1Svc4421 = 0xd2d2
	lpPortIdAddr2Svc4420 = 0x00be
	lpPortIdAddr6Svc4420 = 0x0434
	lpGoldenEntryDigest  = "221a8cdd2cf7c6d018d1c1d033564738" +
		"ca64725713af376ece6c4f719d0dce6a"
	lpGoldenEntry6Digest = "5741064e33d4628611383b9fe09ed299" +
		"162c2a342988602a0a0b4dfdeea1f67f"
	lpGoldenHeaderDigest = "036531da1af8d875a162fd99b0a1bee5" +
		"dd379453317e3cdf5b2e51a2a1a64cf5"
)

// lpGoldenEntry builds the discovery log entry DS3 demands, from literal
// offsets and literal field values. It is the independent half of the golden:
// renderEntry must produce exactly these 1024 bytes.
func lpGoldenEntry(
	nqn string,
	adrFam byte,
	addr string,
	svcId string,
	portId uint16,
) []byte {
	buf := make([]byte, 1024)
	buf[0] = 3                                      // TRTYPE = TCP
	buf[1] = adrFam                                 // ADRFAM = 1 ipv4 / 2 ipv6
	buf[2] = 2                                      // SUBTYPE = NVM subsystem
	buf[3] = 0                                      // TREQ = not specified
	binary.LittleEndian.PutUint16(buf[4:6], portId) // PORTID
	binary.LittleEndian.PutUint16(buf[6:8], 0xffff) // CNTLID = dynamic
	binary.LittleEndian.PutUint16(buf[8:10], 32)    // ASQSZ
	binary.LittleEndian.PutUint16(buf[10:12], 0)    // EFLAGS
	copy(buf[32:64], svcId)                         // TRSVCID
	copy(buf[256:512], nqn)                         // SUBNQN
	copy(buf[512:768], addr)                        // TRADDR
	buf[768] = 0                                    // TSAS: TCP SECTYPE none
	return buf
}

// lpDigest is the hex sha256 of a rendered block, the second, coarser
// tripwire on the same bytes.
func lpDigest(buf []byte) string {
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// lpCompare reports the first differing offset rather than dumping two
// kilobytes of hex at a reader.
func lpCompare(t *testing.T, what string, got []byte, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: byte %d = %#02x, want %#02x", what, i,
				got[i], want[i])
		}
	}
}

// lpString reads one NUL-padded character field and asserts that everything
// after the value really is NUL, which is what an NVMe string field means.
func lpString(t *testing.T, buf []byte, off int, size int) string {
	t.Helper()
	field := buf[off : off+size]
	end := bytes.IndexByte(field, 0)
	if end < 0 {
		t.Fatalf("field at %d is not NUL terminated", off)
	}
	for i := end; i < len(field); i++ {
		if field[i] != 0 {
			t.Fatalf("field at %d: byte %d after the value is %#02x, want 0",
				off, i, field[i])
		}
	}
	return string(field[:end])
}

// TestRenderEntryFields proves DS3 field by field: the individual offsets and
// values of one rendered discovery log entry.
func TestRenderEntryFields(t *testing.T) {
	got, skip := renderEntry(lpGoldenNqn, tcpConf(lpGoldenAddr, lpGoldenSvcId))
	if skip != "" {
		t.Fatal("renderEntry: not ok for a tcp/ipv4 element")
	}
	if len(got) != 1024 {
		t.Fatalf("entry length %d, want 1024", len(got))
	}
	if len(got) != common.CdcDiscLogEntrySize {
		t.Fatalf("entry length %d != CdcDiscLogEntrySize %d",
			len(got), common.CdcDiscLogEntrySize)
	}
	numeric := []struct {
		name string
		off  int
		size int
		want uint64
	}{
		{"TRTYPE", 0, 1, 3},
		{"ADRFAM", 1, 1, 1},
		{"SUBTYPE", 2, 1, 2},
		{"TREQ", 3, 1, 0},
		{"PORTID", 4, 2, lpPortIdAddr1Svc4420},
		{"CNTLID", 6, 2, 0xffff},
		{"ASQSZ", 8, 2, 32},
		{"EFLAGS", 10, 2, 0},
		{"TSAS.SECTYPE", 768, 1, 0},
	}
	for _, tc := range numeric {
		var have uint64
		switch tc.size {
		case 1:
			have = uint64(got[tc.off])
		case 2:
			have = uint64(binary.LittleEndian.Uint16(
				got[tc.off : tc.off+2]))
		}
		if have != tc.want {
			t.Errorf("%s at %d = %#x, want %#x",
				tc.name, tc.off, have, tc.want)
		}
	}
	// ASQSZ is the constant, not just the number 32 (DS3, NP4).
	asqsz := binary.LittleEndian.Uint16(got[8:10])
	if asqsz != common.CdcMaxAdminSqSize {
		t.Errorf("ASQSZ = %d, want CdcMaxAdminSqSize %d",
			asqsz, common.CdcMaxAdminSqSize)
	}
	strings := []struct {
		name string
		off  int
		size int
		want string
	}{
		{"TRSVCID", 32, 32, lpGoldenSvcId},
		{"SUBNQN", 256, 256, lpGoldenNqn},
		{"TRADDR", 512, 256, lpGoldenAddr},
	}
	for _, tc := range strings {
		if have := lpString(t, got, tc.off, tc.size); have != tc.want {
			t.Errorf("%s at %d = %q, want %q",
				tc.name, tc.off, have, tc.want)
		}
	}
}

// TestRenderEntryGolden proves DS3 as a whole: the entry is byte-for-byte the
// independently built golden, digest included, for both address families.
func TestRenderEntryGolden(t *testing.T) {
	cases := []struct {
		name   string
		adrFam string
		addr   string
		famCod byte
		portId uint16
		digest string
	}{
		{
			name:   "ipv4",
			adrFam: common.DefaultCdcAdrFam,
			addr:   lpGoldenAddr,
			famCod: 1,
			portId: lpPortIdAddr1Svc4420,
			digest: lpGoldenEntryDigest,
		},
		{
			name:   "ipv6",
			adrFam: common.CdcAdrFamIpv6,
			addr:   lpGoldenAddr6,
			famCod: 2,
			portId: lpPortIdAddr6Svc4420,
			digest: lpGoldenEntry6Digest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conf := trConf(
				common.DefaultCdcTrType, tc.adrFam, tc.addr, lpGoldenSvcId,
			)
			got, skip := renderEntry(lpGoldenNqn, conf)
			if skip != "" {
				t.Fatalf("renderEntry skipped it: %s", skip)
			}
			want := lpGoldenEntry(
				lpGoldenNqn, tc.famCod, tc.addr, lpGoldenSvcId, tc.portId,
			)
			lpCompare(t, "entry", got, want)
			if d := lpDigest(got); d != tc.digest {
				t.Errorf("entry digest %s, want %s", d, tc.digest)
			}
		})
	}
}

// TestRenderEntryForeignTransport proves the DS3 skip half: an element naming
// a transport or an address family dnv-cdc does not serve renders to nothing
// and the caller counts it (§0 #2).
func TestRenderEntryForeignTransport(t *testing.T) {
	cases := []struct {
		name     string
		trType   string
		adrFam   string
		wantSkip string
	}{
		{"tcp ipv4", "tcp", "ipv4", ""},
		{"tcp ipv6", "tcp", "ipv6", ""},
		{"rdma", "rdma", "ipv4", skipForeignTrType},
		{"fc", "fc", "ipv4", skipForeignTrType},
		{"loop", "loop", "ipv4", skipForeignTrType},
		{"empty tr type", "", "ipv4", skipForeignTrType},
		{"upper case tcp", "TCP", "ipv4", skipForeignTrType},
		{"foreign adr fam", "tcp", "fc", skipForeignAdrFam},
		{"empty adr fam", "tcp", "", skipForeignAdrFam},
		{"upper case ipv4", "tcp", "IPv4", skipForeignAdrFam},
		// The transport is checked first: an element that is wrong twice
		// is reported as the foreign transport it is.
		{"both foreign", "rdma", "fc", skipForeignTrType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conf := trConf(
				tc.trType, tc.adrFam, lpGoldenAddr, lpGoldenSvcId,
			)
			got, skip := renderEntry(lpGoldenNqn, conf)
			if skip != tc.wantSkip {
				t.Fatalf("skip = %q, want %q", skip, tc.wantSkip)
			}
			if skip != "" && got != nil {
				t.Errorf("a skipped element rendered %d bytes", len(got))
			}
			if skip == "" && len(got) != 1024 {
				t.Errorf("entry length %d, want 1024", len(got))
			}
		})
	}
}

// TestPortIdDeterminism proves the DS3 PORTID rule: equal for the same
// (tr_addr, tr_svc_id) wherever it appears — a different subsystem, a
// different entry, a different call — and different for a different port.
func TestPortIdDeterminism(t *testing.T) {
	golden := []struct {
		addr   string
		svcId  string
		portId uint16
	}{
		{lpGoldenAddr, lpGoldenSvcId, lpPortIdAddr1Svc4420},
		{lpGoldenAddr, lpOtherSvcId, lpPortIdAddr1Svc4421},
		{lpOtherAddr, lpGoldenSvcId, lpPortIdAddr2Svc4420},
		{lpGoldenAddr6, lpGoldenSvcId, lpPortIdAddr6Svc4420},
	}
	seen := make(map[uint16]string)
	for _, tc := range golden {
		got := portId(tc.addr, tc.svcId)
		if got != tc.portId {
			t.Errorf("portId(%q, %q) = %#04x, want %#04x",
				tc.addr, tc.svcId, got, tc.portId)
		}
		if again := portId(tc.addr, tc.svcId); again != got {
			t.Errorf("portId(%q, %q) is not deterministic: %#04x then %#04x",
				tc.addr, tc.svcId, got, again)
		}
		key := tc.addr + " " + tc.svcId
		if prev, ok := seen[got]; ok {
			t.Errorf("%q and %q share PORTID %#04x", prev, key, got)
		}
		seen[got] = key
	}
	// The same CN port in two different subsystems' entries carries the same
	// PORTID; the same subsystem on two ports does not.
	first, skip := renderEntry("nqn.2024-01.io.dnv:one",
		tcpConf(lpGoldenAddr, lpGoldenSvcId))
	if skip != "" {
		t.Fatalf("renderEntry skipped it: %s", skip)
	}
	second, skip := renderEntry("nqn.2024-01.io.dnv:two",
		tcpConf(lpGoldenAddr, lpGoldenSvcId))
	if skip != "" {
		t.Fatalf("renderEntry skipped it: %s", skip)
	}
	other, skip := renderEntry("nqn.2024-01.io.dnv:one",
		tcpConf(lpGoldenAddr, lpOtherSvcId))
	if skip != "" {
		t.Fatalf("renderEntry skipped it: %s", skip)
	}
	firstPort := binary.LittleEndian.Uint16(first[4:6])
	secondPort := binary.LittleEndian.Uint16(second[4:6])
	otherPort := binary.LittleEndian.Uint16(other[4:6])
	if firstPort != secondPort {
		t.Errorf("one CN port rendered two PORTIDs: %#04x and %#04x",
			firstPort, secondPort)
	}
	if firstPort == otherPort {
		t.Errorf("two CN ports share PORTID %#04x", firstPort)
	}
}

// TestRenderHeaderGolden proves the DS9 header block: GENCTR at 0, NUMREC at
// 8, RECFMT 0 at 16, the rest reserved and zero.
func TestRenderHeaderGolden(t *testing.T) {
	got := renderHeader(7, 3)
	if len(got) != 1024 {
		t.Fatalf("header length %d, want 1024", len(got))
	}
	if len(got) != common.CdcDiscLogHeaderSize {
		t.Fatalf("header length %d != CdcDiscLogHeaderSize %d",
			len(got), common.CdcDiscLogHeaderSize)
	}
	if genCtr := binary.LittleEndian.Uint64(got[0:8]); genCtr != 7 {
		t.Errorf("GENCTR at 0 = %d, want 7", genCtr)
	}
	if numRec := binary.LittleEndian.Uint64(got[8:16]); numRec != 3 {
		t.Errorf("NUMREC at 8 = %d, want 3", numRec)
	}
	if recFmt := binary.LittleEndian.Uint16(got[16:18]); recFmt != 0 {
		t.Errorf("RECFMT at 16 = %d, want 0", recFmt)
	}
	want := make([]byte, 1024)
	binary.LittleEndian.PutUint64(want[0:8], 7)
	binary.LittleEndian.PutUint64(want[8:16], 3)
	lpCompare(t, "header", got, want)
	if d := lpDigest(got); d != lpGoldenHeaderDigest {
		t.Errorf("header digest %s, want %s", d, lpGoldenHeaderDigest)
	}
	// A big GENCTR must not bleed into NUMREC: the two are independent
	// 64 bit fields, which only a wide value proves.
	wide := renderHeader(0xfedcba9876543210, 0x0123456789abcdef)
	if v := binary.LittleEndian.Uint64(wide[0:8]); v != 0xfedcba9876543210 {
		t.Errorf("wide GENCTR = %#x", v)
	}
	if v := binary.LittleEndian.Uint64(wide[8:16]); v != 0x0123456789abcdef {
		t.Errorf("wide NUMREC = %#x", v)
	}
}

// lpBody builds a synthetic log body of n entry-sized blocks whose every byte
// is non-zero, so a zero in a served window is unambiguously fill and not
// content.
func lpBody(n int) []byte {
	body := make([]byte, n*1024)
	for i := range body {
		body[i] = byte(i%251) + 1
	}
	return body
}

// lpWindow is the obviously correct reading of DS9: the log is the header
// followed by the body, a read copies what it overlaps and zero fills the
// rest.
func lpWindow(image []byte, off uint64, length int) []byte {
	out := make([]byte, length)
	if off < uint64(len(image)) {
		copy(out, image[off:])
	}
	return out
}

// TestLogPageBytes proves the DS9 paging arithmetic: aligned reads, unaligned
// reads, a read spanning the header/body boundary, reads past the end (all
// zeros) and a zero length read.
func TestLogPageBytes(t *testing.T) {
	const genCtr, numRec = 9, 3
	body := lpBody(numRec)
	image := append(renderHeader(genCtr, numRec), body...)
	if len(image) != 4096 {
		t.Fatalf("fixture image is %d bytes, want 4096", len(image))
	}
	cases := []struct {
		name   string
		off    uint64
		length int
	}{
		{"the header alone", 0, 1024},
		{"header and first entry", 0, 2048},
		{"the whole log", 0, 4096},
		{"the whole log and then some", 0, 8192},
		{"first entry, aligned", 1024, 1024},
		{"second entry, aligned", 2048, 1024},
		{"last entry, aligned", 3072, 1024},
		{"spanning the header/body boundary", 1020, 8},
		{"spanning the boundary, wide", 1021, 1030},
		{"odd offset inside the header", 3, 17},
		{"odd offset inside an entry", 2049, 7},
		{"a dword aligned tail", 3900, 400},
		{"straddling the end", 4000, 512},
		{"exactly at the end", 4096, 64},
		{"entirely past the end", 8192, 1024},
		{"far past the end", 1 << 30, 16},
		{"zero length at zero", 0, 0},
		{"zero length past the end", 9000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logPageBytes(genCtr, numRec, body, tc.off, tc.length)
			if got == nil {
				t.Fatal("logPageBytes returned nil")
			}
			if len(got) != tc.length {
				t.Fatalf("length %d, want %d", len(got), tc.length)
			}
			lpCompare(t, "window",
				got, lpWindow(image, tc.off, tc.length))
		})
	}
	// The boundary case spelled out, because it is the one an off-by-one
	// hides in: four bytes of header tail then four of the first entry.
	edge := logPageBytes(genCtr, numRec, body, 1020, 8)
	if !bytes.Equal(edge[:4], image[1020:1024]) {
		t.Errorf("boundary read: header tail %x, want %x",
			edge[:4], image[1020:1024])
	}
	if !bytes.Equal(edge[4:], body[:4]) {
		t.Errorf("boundary read: body head %x, want %x", edge[4:], body[:4])
	}
	// Entry i really does start at 1024 + i*1024 (DS9).
	for i := 0; i < numRec; i++ {
		off := uint64(1024 + i*1024)
		got := logPageBytes(genCtr, numRec, body, off, 1024)
		if !bytes.Equal(got, body[i*1024:(i+1)*1024]) {
			t.Errorf("entry %d at offset %d is not the ith record", i, off)
		}
	}
	// Past the end is zeros, not an error and not stale bytes.
	past := logPageBytes(genCtr, numRec, body, 4096, 256)
	for i, b := range past {
		if b != 0 {
			t.Fatalf("byte %d past the end is %#02x, want 0", i, b)
		}
	}
}

// TestLogPageBytesEmptyView proves DS9 for a host that sees nothing: the
// header still serves, NUMREC is 0 and everything after the header is zeros.
func TestLogPageBytesEmptyView(t *testing.T) {
	got := logPageBytes(1, 0, nil, 0, 2048)
	if len(got) != 2048 {
		t.Fatalf("length %d, want 2048", len(got))
	}
	if genCtr := binary.LittleEndian.Uint64(got[0:8]); genCtr != 1 {
		t.Errorf("GENCTR = %d, want 1", genCtr)
	}
	if numRec := binary.LittleEndian.Uint64(got[8:16]); numRec != 0 {
		t.Errorf("NUMREC = %d, want 0", numRec)
	}
	for i := 1024; i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d of an empty view is %#02x, want 0", i, got[i])
		}
	}
}

// TestLogPageBytesDoesNotAliasBody proves the DS9 snapshot promise at the
// rendering level: the served window is a copy, so a caller cannot scribble
// on the registry's body through it.
func TestLogPageBytesDoesNotAliasBody(t *testing.T) {
	body := lpBody(1)
	keep := append([]byte(nil), body...)
	got := logPageBytes(1, 1, body, 1024, 1024)
	for i := range got {
		got[i] = 0xee
	}
	if !bytes.Equal(body, keep) {
		t.Fatal("writing to a served window mutated the snapshot body")
	}
}
