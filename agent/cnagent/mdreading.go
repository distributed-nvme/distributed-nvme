package cnagent

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// The reading of a member (cnagent.md CN12)
//
// The assembly, the forced start, the re-assembly, the inspect, the
// take-back check, the add of a leg_list member, and a cntlr's read for its
// names of an absent array and of a forced start (absentArrayNamed,
// forcedStartNamed), decide from one reading of each member's superblock
// area,
// which the agent reads itself (Md.ReadMember): one O_DIRECT read of the area
// where md's version-1.2 superblock sits, 4096 bytes into the member, and, when
// the superblock says its bad-block list is not empty, one more of the window
// that holds the list. The area is parsed in the layout of the kernel's struct
// mdp_superblock_1 (include/uapi/linux/raid/md_p.h). A read that fails, or
// does not answer, is a reading of its own and never "no superblock": mdadm
// answers both the same way, which is why the agent reads the area itself.
// The read is O_DIRECT because md writes a superblock with bios of its own,
// past the member's page cache, so a buffered read can answer an old copy.
// ---------------------------------------------------------------------------

const (
	// mdSbOffset is where md's version-1.2 superblock sits on a member, in
	// bytes: the kernel's super_offset of 8 sectors, which every dnv array
	// has, since the agent creates and assembles each one in that format
	// (mdMetadata).
	mdSbOffset = 4096
	// mdSbAreaLen is the length of the area read at mdSbOffset: one direct
	// read's unit, which holds the superblock with all its roles.
	mdSbAreaLen = 4096
	// mdSbOffsetSectors is mdSbOffset in sectors, as the superblock records
	// it.
	mdSbOffsetSectors = 8
	// mdSbMagic is MD_SB_MAGIC, the superblock's first word.
	mdSbMagic = 0xa92b4efc
	// mdSbHeaderLen is the fixed part of the superblock, before its roles.
	mdSbHeaderLen = 256
	// mdFeatureRecoveryOffset is MD_FEATURE_RECOVERY_OFFSET: the member
	// carries a copy position, so md was copying onto it.
	mdFeatureRecoveryOffset = 2
	// mdFeatureBadBlocks is MD_FEATURE_BAD_BLOCKS: the member's bad-block
	// list is not empty, and the kernel loads it.
	mdFeatureBadBlocks = 8
	// mdRoleMax is MD_DISK_ROLE_MAX: a role below it is a place in the
	// array.
	mdRoleMax = 0xff00
	// mdRoleSpare and mdRoleFaulty are MD_DISK_ROLE_SPARE and
	// MD_DISK_ROLE_FAULTY.
	mdRoleSpare  = 0xffff
	mdRoleFaulty = 0xfffe
	// mdBadBlockEnd is the bad-block list's end mark: a word whose bits are
	// all ones.
	mdBadBlockEnd = ^uint64(0)
	// mdBadBlockLenMask is the length field of a bad-block list entry, its
	// low ten bits; the sector is the rest.
	mdBadBlockLenMask = 0x3ff
	// mdDirectAlign is the unit an O_DIRECT read is aligned to
	// (OsClient.ReadBlockDirect).
	mdDirectAlign = 4096
)

// The byte offsets of the fields a reading takes, in struct
// mdp_superblock_1.
const (
	mdSbMagicAt          = 0
	mdSbMajorVersionAt   = 4
	mdSbFeatureMapAt     = 8
	mdSbSetUuidAt        = 16
	mdSbRaidDisksAt      = 92
	mdSbSuperOffsetAt    = 144
	mdSbRecoveryOffsetAt = 152
	mdSbDevNumberAt      = 160
	mdSbBblogSizeAt      = 186
	mdSbBblogOffsetAt    = 188
	mdSbEventsAt         = 200
	mdSbCsumAt           = 216
	mdSbMaxDevAt         = 220
)

// mdSuperblock is what a reading takes of one member's md superblock.
type mdSuperblock struct {
	// Uuid is the array's identity, in mdadm's spelling (four groups of
	// eight hex digits, colon-separated), which a forced start's new
	// identity replaces.
	Uuid string
	// Events is md's change count, which md writes into every member's
	// superblock as the array changes, so a member that left the array
	// keeps the count it had then.
	Events uint64
	// Role is the member's role: below mdRoleMax a place in the array,
	// mdRoleSpare for a member md added and never copied onto, mdRoleFaulty
	// for one md failed.
	Role uint16
	// Recovering is set when the superblock carries a copy position, which
	// md writes on a member it was copying onto, and RecoveryOffset is how
	// far the copy got, in sectors: a half-copied member.
	Recovering     bool
	RecoveryOffset uint64
	// BadBlocks is the length of the member's bad-block list: md's list of
	// the regions it could not read or write on the member.
	BadBlocks int
	// BadBlocksRefused is set when that list holds an entry of length zero,
	// a list the kernel refuses to import (super_1_load), so md refuses to
	// take the member into an array (cnagent_integtest.md, Lab facts).
	BadBlocksRefused bool
	// RaidDisks is the number of places the array has. Roles is the
	// superblock's role table, one role for each member number md has
	// given, the member's own (Role) at its own number: the roles md held
	// its members in when it last wrote this superblock, which mdadm's
	// assembly weighs (workingRoles, seesWorking; mdadmStarts).
	RaidDisks uint32
	Roles     []uint16
}

// workingRoles is how many members the superblock records as working, as
// mdadm counts them for its assembly (super1.c getinfo_super1's
// working_disks): the entries of the role table that are places of the
// array, below RaidDisks.
func (sb *mdSuperblock) workingRoles() int {
	n := 0
	for _, role := range sb.Roles {
		if uint32(role) < sb.RaidDisks {
			n++
		}
	}
	return n
}

// seesWorking reports whether the superblock records the member in place as
// working: an entry of its role table holds place, a place of the array.
// mdadm's assembly drops a member whose superblock does not record the
// newest member's place so (mdadmStarts).
func (sb *mdSuperblock) seesWorking(place uint16) bool {
	if uint32(place) >= sb.RaidDisks {
		return false
	}
	for _, role := range sb.Roles {
		if role == place {
			return true
		}
	}
	return false
}

// mdReading is the reading of one member's superblock area: Err when the
// read did not answer or failed, or the area does not parse; otherwise Sb,
// nil for a member whose area holds no superblock.
type mdReading struct {
	Err error
	Sb  *mdSuperblock
}

// notFullMember is "" for a superblock of a full member of its array — a place
// in the array and no copy position — and otherwise what it shows instead,
// which CN12's forced start asks of its named leg. A nil superblock is a
// member that carries none.
func (sb *mdSuperblock) notFullMember() string {
	switch {
	case sb == nil:
		return "no md superblock"
	case sb.Recovering:
		return fmt.Sprintf("recovery offset %d sectors", sb.RecoveryOffset)
	case sb.Role >= mdRoleMax:
		return "device role " + mdRoleText(sb.Role)
	}
	return ""
}

// mdRoleText names a role as mdadm's "Device Role" line does.
func mdRoleText(role uint16) string {
	switch {
	case role < mdRoleMax:
		return fmt.Sprintf("Active device %d", role)
	case role == mdRoleSpare:
		return "spare"
	case role == mdRoleFaulty:
		return "faulty"
	}
	return fmt.Sprintf("%#x", role)
}

// errNotRead marks a reading whose read did not answer or failed, as
// against an area that answered and does not parse.
var errNotRead = errors.New("not read")

// ReadMember reads one member's superblock area (the reading of CN12). A
// read that fails or does not answer is the reading's error, wrapping
// errNotRead, never "no superblock"; an area that answered and holds no
// superblock magic reads as none; a superblock this agent does not make —
// another version, another place, a member number past its roles — or one
// whose checksum does not match is the reading's error, so a torn or foreign
// block never passes for a member.
func (m *Md) ReadMember(ctx context.Context, dev string) mdReading {
	area, err := m.oc.ReadBlockDirect(ctx, dev, mdSbOffset, mdSbAreaLen)
	if err != nil {
		return mdReading{Err: fmt.Errorf(
			"%s: superblock area %w: %w", dev, errNotRead, err)}
	}
	sb, bbl, err := parseMdSuperblock(area)
	if err != nil {
		return mdReading{Err: fmt.Errorf("%s: %w", dev, err)}
	}
	if sb == nil || bbl.length == 0 {
		return mdReading{Sb: sb}
	}
	start := bbl.start &^ (mdDirectAlign - 1)
	end := (bbl.start + bbl.length + mdDirectAlign - 1) &^ (mdDirectAlign - 1)
	window, err := m.oc.ReadBlockDirect(ctx, dev, start, end-start)
	if err != nil {
		return mdReading{Err: fmt.Errorf(
			"%s: bad-block list %w: %w", dev, errNotRead, err)}
	}
	sb.BadBlocks, sb.BadBlocksRefused = countBadBlocks(
		window[bbl.start-start : bbl.start-start+bbl.length])
	return mdReading{Sb: sb}
}

// mdBblWindow is where a superblock says its bad-block list sits on the
// member, in bytes: zero length when the list is empty.
type mdBblWindow struct {
	start  uint64
	length uint64
}

// parseMdSuperblock parses the area read at mdSbOffset: nil with no error
// when the magic is absent; the superblock and, when the feature map says
// its bad-block list is not empty, where the list sits; an error for a
// superblock this agent does not make or whose checksum does not match.
func parseMdSuperblock(area []byte) (*mdSuperblock, mdBblWindow, error) {
	le := binary.LittleEndian
	if len(area) < mdSbHeaderLen {
		return nil, mdBblWindow{}, fmt.Errorf(
			"superblock area of %d bytes", len(area))
	}
	if le.Uint32(area[mdSbMagicAt:]) != mdSbMagic {
		return nil, mdBblWindow{}, nil
	}
	if v := le.Uint32(area[mdSbMajorVersionAt:]); v != 1 {
		return nil, mdBblWindow{}, fmt.Errorf(
			"md superblock major version %d, not 1", v)
	}
	if off := le.Uint64(area[mdSbSuperOffsetAt:]); off != mdSbOffsetSectors {
		return nil, mdBblWindow{}, fmt.Errorf(
			"md superblock records itself at sector %d, not %d",
			off, mdSbOffsetSectors)
	}
	maxDev := le.Uint32(area[mdSbMaxDevAt:])
	size := uint64(mdSbHeaderLen) + 2*uint64(maxDev)
	if size > uint64(len(area)) {
		return nil, mdBblWindow{}, fmt.Errorf(
			"md superblock has %d roles, more than its area holds", maxDev)
	}
	if want, got := mdSbChecksum(area[:size]),
		le.Uint32(area[mdSbCsumAt:]); want != got {
		return nil, mdBblWindow{}, fmt.Errorf(
			"md superblock checksum %#x, want %#x", got, want)
	}
	devNumber := le.Uint32(area[mdSbDevNumberAt:])
	if devNumber >= maxDev {
		return nil, mdBblWindow{}, fmt.Errorf(
			"md superblock member number %d past its %d roles",
			devNumber, maxDev)
	}
	features := le.Uint32(area[mdSbFeatureMapAt:])
	roles := make([]uint16, maxDev)
	for i := range roles {
		roles[i] = le.Uint16(area[mdSbHeaderLen+2*i:])
	}
	sb := &mdSuperblock{
		Uuid:      mdUuidText(area[mdSbSetUuidAt : mdSbSetUuidAt+16]),
		Events:    le.Uint64(area[mdSbEventsAt:]),
		Role:      roles[devNumber],
		RaidDisks: le.Uint32(area[mdSbRaidDisksAt:]),
		Roles:     roles,
	}
	if features&mdFeatureRecoveryOffset != 0 {
		sb.Recovering = true
		sb.RecoveryOffset = le.Uint64(area[mdSbRecoveryOffsetAt:])
	}
	if features&mdFeatureBadBlocks == 0 {
		return sb, mdBblWindow{}, nil
	}
	// The kernel loads the list only with the feature bit, from the signed
	// offset in sectors from the superblock, at most one page long
	// (super_1_load).
	sectors := uint64(le.Uint16(area[mdSbBblogSizeAt:]))
	offset := int64(int32(le.Uint32(area[mdSbBblogOffsetAt:])))
	start := (int64(mdSbOffsetSectors) + offset) * 512
	if sectors == 0 || sectors*512 > mdDirectAlign || offset == 0 ||
		start < 0 {
		return nil, mdBblWindow{}, fmt.Errorf(
			"md superblock bad-block list of %d sectors at offset %d",
			sectors, offset)
	}
	return sb, mdBblWindow{start: uint64(start), length: sectors * 512}, nil
}

// mdSbChecksum is the kernel's calc_sb_1_csum over the superblock and its
// roles: the sum of its little-endian 32-bit words, with the checksum field
// taken as zero, a trailing 16-bit half added, folded once to 32 bits.
func mdSbChecksum(sb []byte) uint32 {
	le := binary.LittleEndian
	var sum uint64
	i := 0
	for ; i+4 <= len(sb); i += 4 {
		if i == mdSbCsumAt {
			continue
		}
		sum += uint64(le.Uint32(sb[i:]))
	}
	if len(sb)-i == 2 {
		sum += uint64(le.Uint16(sb[i:]))
	}
	return uint32(sum&0xffffffff) + uint32(sum>>32)
}

// countBadBlocks is the length of a bad-block list, its words up to the end
// mark, and whether one of them has length zero, its low ten bits, which the
// kernel refuses to import (super_1_load), and with it the member
// (cnagent_integtest.md, Lab facts). Such an entry counts.
func countBadBlocks(list []byte) (int, bool) {
	n, refused := 0, false
	for i := 0; i+8 <= len(list); i += 8 {
		entry := binary.LittleEndian.Uint64(list[i:])
		if entry == mdBadBlockEnd {
			break
		}
		if entry&mdBadBlockLenMask == 0 {
			refused = true
		}
		n++
	}
	return n, refused
}

// mdUuidText spells an array's identity as mdadm prints it: the 16 bytes in
// their order, as four groups of eight hex digits joined by colons.
func mdUuidText(raw []byte) string {
	h := hex.EncodeToString(raw)
	return h[0:8] + ":" + h[8:16] + ":" + h[16:24] + ":" + h[24:32]
}

// mdSysfsUuid turns the array identity a running array's md/uuid shows — the
// same 16 bytes in their order, spelled as a dashed UUID — into mdadm's
// spelling (mdUuidText), "" for a value that is not one.
func mdSysfsUuid(value string) string {
	raw, err := hex.DecodeString(strings.ReplaceAll(
		strings.TrimSpace(value), "-", ""))
	if err != nil || len(raw) != 16 {
		return ""
	}
	return mdUuidText(raw)
}
