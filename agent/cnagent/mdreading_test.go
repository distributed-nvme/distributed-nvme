package cnagent

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// mdSbArea is a lab superblock area padded back to the length ReadMember
// reads.
func mdSbArea(t *testing.T, lit string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(lit)
	if err != nil {
		t.Fatalf("lab area: %v", err)
	}
	area := make([]byte, mdSbAreaLen)
	copy(area, raw)
	return area
}

// mdLabBadBlockList is the bad-block list md wrote beside mdSbLabBadblocks,
// at offset 16 sectors from the superblock: one entry, "4096 for 8
// sectors", then the end mark.
const mdLabBadBlockList = "0800400000000000ffffffffffffffff"

// TestParseMdSuperblockLabAreas parses areas md wrote in the lab and reads
// back what mdadm --examine printed of each, and the role table every one
// carries: two places, both recorded as working ("Array State : AA"), the
// half-copied member's and the spare's at member number 2.
func TestParseMdSuperblockLabAreas(t *testing.T) {
	const uuid = "e9b826b4:df214231:08620ea6:6f8f15ac"
	cases := []struct {
		name       string
		lit        string
		events     uint64
		role       uint16
		recovering bool
		recovery   uint64
		notFull    string
		bblStart   uint64
		bblLength  uint64
	}{
		{name: "full", lit: mdSbLabFull, events: 2, role: 0},
		{name: "halfcopied", lit: mdSbLabHalfcopied, events: 11, role: 1,
			recovering: true, recovery: 146944,
			notFull: "recovery offset 146944 sectors"},
		{name: "spare", lit: mdSbLabSpare, events: 3, role: mdRoleSpare,
			notFull: "device role spare"},
		{name: "badblocks", lit: mdSbLabBadblocks, events: 2, role: 1,
			bblStart: (8 + 16) * 512, bblLength: 8 * 512},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb, bbl, err := parseMdSuperblock(mdSbArea(t, tc.lit))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if sb == nil {
				t.Fatalf("parse: no superblock")
			}
			if sb.Uuid != uuid || sb.Events != tc.events ||
				sb.Role != tc.role || sb.Recovering != tc.recovering ||
				sb.RecoveryOffset != tc.recovery {
				t.Errorf("parse: %+v, want uuid %s events %d role %#x "+
					"recovering %v at %d", sb, uuid, tc.events, tc.role,
					tc.recovering, tc.recovery)
			}
			if sb.RaidDisks != 2 || sb.workingRoles() != 2 ||
				!sb.seesWorking(0) || !sb.seesWorking(1) ||
				sb.seesWorking(2) || len(sb.Roles) != 128 {
				t.Errorf("role table: %d places, %d working, roles %v; "+
					"want 2 places, both working, of 128 roles",
					sb.RaidDisks, sb.workingRoles(), sb.Roles[:4])
			}
			if got := sb.notFullMember(); got != tc.notFull {
				t.Errorf("notFullMember: %q, want %q", got, tc.notFull)
			}
			if bbl.start != tc.bblStart || bbl.length != tc.bblLength {
				t.Errorf("bad-block window %+v, want start %d length %d",
					bbl, tc.bblStart, tc.bblLength)
			}
		})
	}
}

// TestParseMdSuperblockRefusals: a blank area is no superblock; a torn one,
// another version or another place is an error, never a member.
func TestParseMdSuperblockRefusals(t *testing.T) {
	sb, _, err := parseMdSuperblock(make([]byte, mdSbAreaLen))
	if sb != nil || err != nil {
		t.Fatalf("blank area: %+v, %v; want no superblock, no error", sb, err)
	}
	torn := mdSbArea(t, mdSbLabFull)
	torn[mdSbEventsAt]++
	if _, _, err := parseMdSuperblock(torn); err == nil ||
		!strings.Contains(err.Error(), "checksum") {
		t.Errorf("torn area: %v, want a checksum error", err)
	}
	// A field changed with its checksum fixed is still refused.
	resum := func(area []byte) []byte {
		maxDev := binary.LittleEndian.Uint32(area[mdSbMaxDevAt:])
		binary.LittleEndian.PutUint32(area[mdSbCsumAt:],
			mdSbChecksum(area[:mdSbHeaderLen+2*maxDev]))
		return area
	}
	version := mdSbArea(t, mdSbLabFull)
	binary.LittleEndian.PutUint32(version[mdSbMajorVersionAt:], 0)
	if _, _, err := parseMdSuperblock(resum(version)); err == nil ||
		!strings.Contains(err.Error(), "major version") {
		t.Errorf("version 0: %v, want a version error", err)
	}
	place := mdSbArea(t, mdSbLabFull)
	binary.LittleEndian.PutUint64(place[mdSbSuperOffsetAt:], 0)
	if _, _, err := parseMdSuperblock(resum(place)); err == nil ||
		!strings.Contains(err.Error(), "sector 0") {
		t.Errorf("offset 0: %v, want a place error", err)
	}
	number := mdSbArea(t, mdSbLabFull)
	binary.LittleEndian.PutUint32(number[mdSbDevNumberAt:], 128)
	if _, _, err := parseMdSuperblock(resum(number)); err == nil ||
		!strings.Contains(err.Error(), "member number") {
		t.Errorf("member 128: %v, want a member number error", err)
	}
	// The lab's own checksum holds: resum changes nothing on it.
	full := mdSbArea(t, mdSbLabFull)
	before := binary.LittleEndian.Uint32(full[mdSbCsumAt:])
	if after := binary.LittleEndian.Uint32(resum(full)[mdSbCsumAt:]); after != before {
		t.Fatalf("checksum of the lab area: %#x, md wrote %#x", after, before)
	}
}

// TestReadMemberReadFailures: a read that fails, of the area or of the
// bad-block list, is a reading of its own, never "no superblock".
func TestReadMemberReadFailures(t *testing.T) {
	ctx := context.Background()
	eio := errors.New("input/output error")
	var reads []string
	oc := &common.FakeOsClient{
		ReadBlockDirectFn: func(_ context.Context, path string,
			offset, length uint64) ([]byte, error) {
			reads = append(reads, path)
			if offset != mdSbOffset || length != mdSbAreaLen {
				t.Errorf("read at %d length %d, want %d length %d",
					offset, length, mdSbOffset, mdSbAreaLen)
			}
			return nil, eio
		},
	}
	got := NewMd(oc).ReadMember(ctx, "/dev/mapper/leg")
	if got.Err == nil || !errors.Is(got.Err, errNotRead) ||
		!errors.Is(got.Err, eio) || got.Sb != nil {
		t.Fatalf("failed read: %+v, want an error that is not read", got)
	}
	if len(reads) != 1 {
		t.Fatalf("reads %v, want one", reads)
	}

	area := mdSbArea(t, mdSbLabBadblocks)
	list, _ := hex.DecodeString(mdLabBadBlockList)
	window := make([]byte, mdDirectAlign)
	copy(window, list)
	for i := len(list); i < len(window); i++ {
		window[i] = 0xff
	}
	listFails := false
	oc.ReadBlockDirectFn = func(_ context.Context, _ string,
		offset, length uint64) ([]byte, error) {
		switch offset {
		case mdSbOffset:
			return area, nil
		case (8 + 16) * 512:
			if length != mdDirectAlign {
				t.Errorf("list read of %d bytes, want %d", length,
					mdDirectAlign)
			}
			if listFails {
				return nil, eio
			}
			return window, nil
		}
		t.Errorf("read at %d", offset)
		return nil, eio
	}
	got = NewMd(oc).ReadMember(ctx, "/dev/mapper/leg")
	if got.Err != nil || got.Sb == nil || got.Sb.BadBlocks != 1 {
		t.Fatalf("member with a list: %+v, want one bad block", got)
	}
	listFails = true
	got = NewMd(oc).ReadMember(ctx, "/dev/mapper/leg")
	if got.Err == nil || !errors.Is(got.Err, errNotRead) || got.Sb != nil {
		t.Fatalf("failed list read: %+v, want an error that is not read",
			got)
	}
}

// TestCountBadBlocks: the list ends at the first all-ones word, and an entry
// of length zero counts and marks the list one the kernel refuses to import,
// at any sector; entries with a length, the lab's among them, do not.
func TestCountBadBlocks(t *testing.T) {
	list := make([]byte, 32)
	binary.LittleEndian.PutUint64(list[0:], 0)
	binary.LittleEndian.PutUint64(list[8:], 4096<<10|8)
	binary.LittleEndian.PutUint64(list[16:], mdBadBlockEnd)
	binary.LittleEndian.PutUint64(list[24:], 1)
	if n, refused := countBadBlocks(list); n != 2 || !refused {
		t.Fatalf("countBadBlocks: %d, %v; want 2, refused", n, refused)
	}
	binary.LittleEndian.PutUint64(list[0:], 8192<<10)
	if n, refused := countBadBlocks(list); n != 2 || !refused {
		t.Fatalf("countBadBlocks of an entry of length zero at sector "+
			"8192: %d, %v; want 2, refused", n, refused)
	}
	binary.LittleEndian.PutUint64(list[0:], 8192<<10|1)
	if n, refused := countBadBlocks(list); n != 2 || refused {
		t.Fatalf("countBadBlocks of entries with a length: %d, %v; want "+
			"2, not refused", n, refused)
	}
	lab, _ := hex.DecodeString(mdLabBadBlockList)
	if n, refused := countBadBlocks(lab); n != 1 || refused {
		t.Fatalf("countBadBlocks of the lab's list: %d, %v; want 1, not "+
			"refused", n, refused)
	}
}

// TestMdSysfsUuid: a running array's md/uuid spells the identity the
// superblock carries, dashed, as the lab read it beside mdadm's spelling of
// the same array: 61316d87-c89f-a890-b4d6-5cce2e838448 and
// 61316d87:c89fa890:b4d65cce:2e838448.
func TestMdSysfsUuid(t *testing.T) {
	if got := mdSysfsUuid("61316d87-c89f-a890-b4d6-5cce2e838448\n"); got !=
		"61316d87:c89fa890:b4d65cce:2e838448" {
		t.Fatalf("mdSysfsUuid: %q", got)
	}
	if got := mdSysfsUuid("not a uuid"); got != "" {
		t.Fatalf("mdSysfsUuid of garbage: %q", got)
	}
}
