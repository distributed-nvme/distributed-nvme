package dnagent

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// DiskMeta owns the dnv on-disk format of architecture.md [D13]: the 4 KiB
// header block, the two alternating volume-table slots, and the allocators
// derived from the table. It replaces LVM on the DN entirely — the on-disk
// table, not the agent's local store, is authoritative for extent placement,
// so a node that loses --local-store but keeps its disk recovers exactly the
// same layout.
//
// Every method loads lazily on first use and is safe for concurrent callers;
// all raw-device IO goes through OsClient.ReadBlock/WriteBlock ([P7]).
type DiskMeta struct {
	oc   common.OsClient
	disk string

	// mu guards everything below. It is a leaf lock: the OS calls it covers
	// are all bounded by the SH15 soft timeout.
	mu sync.Mutex
	// loaded is set once a load succeeded; a load that fails is simply not
	// remembered, so the next call retries it and a transient read error
	// never latches.
	loaded bool
	// formatted distinguishes "the header magic is absent" (a blank disk,
	// which EnsureFormatted may format) from "the header is valid". A header
	// whose magic matches but whose version or CRC does not is neither: it
	// makes every method return an error, and is never overwritten.
	formatted bool
	// verified says the header's identity was checked against this agent's
	// (cluster_id, dn_id, extent_size) and matched. Only a successful
	// EnsureFormatted sets it. **Every mutator requires it**: a disk that
	// belongs to another node must never have its volume table rewritten,
	// and a failed DN converge does not stop the side converges that follow
	// (DN19), so the guard has to live here rather than in the caller.
	verified bool
	hdr      *pb.DnDiskHeader
	table    *pb.DnDiskTable
	seq      uint64
	// newestSlot is the slot the current table came from (-1 when the table
	// is the empty fresh-format one); the next save goes to the other slot.
	newestSlot int
	// diskSize is the raw byte size of the device, the only input to the
	// allocator the disk format itself does not carry; 0 until a DN converge
	// or probe has read it. The extent count is derived from it and the
	// *header's* extent_size, which is the authority ([D13]).
	diskSize uint64
}

// Envelope layout (§5.2 of the plan; all integers little-endian).
const (
	dnHeaderMagic   = "DNVDISK1"
	dnTableMagic    = "DNVTABL1"
	dnHeaderVersion = 1

	// Header block: magic[0:8] version[8:12] len[12:16] proto[16:16+N]
	// crc[4092:4096] over bytes 0..4091.
	dnHeaderProtoOff = 16
	dnHeaderCrcOff   = common.DnHeaderSize - 4

	// Table slot: magic[0:8] uuid[8:16] seq[16:24] len[24:28] crc[28:32]
	// proto[32:32+N]. The CRC covers bytes 0..27 **and** the N proto bytes —
	// everything but the CRC field itself. Covering only the body would
	// leave `seq` unprotected, and a single flipped bit there could make the
	// stale slot outrank the live one and roll the whole volume table back.
	dnSlotProtoOff = 32
	dnSlotCrcOff   = 28
	dnSlotBlock    = 4096
)

func NewDiskMeta(oc common.OsClient, disk string) *DiskMeta {
	return &DiskMeta{oc: oc, disk: disk, newestSlot: -1}
}

// readBlock / writeBlock wrap every raw-device call in the §7 soft timeout,
// exactly as the osBase wrappers do for commands (SH15). Without it a stalled
// device would hold the DiskMeta mutex — and, through it, a converge pass —
// indefinitely.
func (d *DiskMeta) readBlock(
	ctx context.Context,
	offset uint64,
	length uint64,
) ([]byte, error) {
	cctx, cancel := context.WithTimeout(
		ctx, common.CmdSoftTimeout*time.Second)
	defer cancel()
	return d.oc.ReadBlock(cctx, d.disk, offset, length)
}

func (d *DiskMeta) writeBlock(
	ctx context.Context,
	offset uint64,
	data []byte,
) error {
	cctx, cancel := context.WithTimeout(
		ctx, common.CmdSoftTimeout*time.Second)
	defer cancel()
	return d.oc.WriteBlock(cctx, d.disk, offset, data)
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// load reads the header and, when it is valid, the newer of the two table
// slots. Rule 1: a header whose magic is absent means "unformatted"; a header
// whose magic matches but whose version or CRC does not is an error and is
// never overwritten.
func (d *DiskMeta) load(ctx context.Context) error {
	if d.loaded {
		return nil
	}
	raw, err := d.readBlock(ctx, common.DnHeaderOffset, common.DnHeaderSize)
	if err != nil {
		return fmt.Errorf("reading the disk header: %w", err)
	}
	hdr, ok, err := parseDnHeader(raw)
	if err != nil {
		return err
	}
	if !ok {
		d.formatted = false
		d.verified = false
		d.hdr = nil
		d.table = &pb.DnDiskTable{}
		d.seq = 0
		d.newestSlot = -1
		d.loaded = true
		return nil
	}
	table, seq, slot, err := d.loadTable(ctx, hdr.GetFormatUuid())
	if err != nil {
		return err
	}
	d.formatted = true
	d.verified = false
	d.hdr = hdr
	d.table = table
	d.seq = seq
	d.newestSlot = slot
	d.loaded = true
	return nil
}

// loadTable returns the valid slot with the higher seq. Because the format
// writes slot A before the header, a valid header guarantees a valid slot:
// finding none means the table area is damaged, and that is an error, never a
// silently empty table — presenting one would hand out extents a live side
// still owns.
func (d *DiskMeta) loadTable(
	ctx context.Context,
	formatUuid uint64,
) (*pb.DnDiskTable, uint64, int, error) {
	best := (*pb.DnDiskTable)(nil)
	var bestSeq uint64
	bestSlot := -1
	for slot, offset := range dnSlotOffsets {
		table, seq, ok, err := d.readSlot(ctx, offset, formatUuid)
		if err != nil {
			return nil, 0, 0, err
		}
		if !ok {
			continue
		}
		if bestSlot < 0 || seq > bestSeq {
			best, bestSeq, bestSlot = table, seq, slot
		}
	}
	if bestSlot < 0 {
		return nil, 0, 0, fmt.Errorf(
			"corrupt volume table: neither slot is valid for format %016x",
			formatUuid)
	}
	return best, bestSeq, bestSlot, nil
}

var dnSlotOffsets = [2]uint64{
	common.DnTableSlotAOffset, common.DnTableSlotBOffset,
}

// readSlot fetches a slot's first block, validates the envelope, then pulls
// the remainder the length field asks for. ok is false for any slot that
// fails magic, format_uuid, CRC or the size bound — a torn or stale slot is
// simply not a candidate.
func (d *DiskMeta) readSlot(
	ctx context.Context,
	offset uint64,
	formatUuid uint64,
) (*pb.DnDiskTable, uint64, bool, error) {
	head, err := d.readBlock(ctx, offset, dnSlotBlock)
	if err != nil {
		return nil, 0, false, fmt.Errorf(
			"reading the table slot at %d: %w", offset, err)
	}
	if string(head[0:8]) != dnTableMagic {
		return nil, 0, false, nil
	}
	if binary.LittleEndian.Uint64(head[8:16]) != formatUuid {
		// A slot from a previous format of this disk. The uuid is what makes
		// a stale slot with a higher seq lose.
		return nil, 0, false, nil
	}
	seq := binary.LittleEndian.Uint64(head[16:24])
	size := uint64(binary.LittleEndian.Uint32(head[24:28]))
	want := binary.LittleEndian.Uint32(head[28:32])
	if dnSlotProtoOff+size > common.DnTableSlotSize {
		return nil, 0, false, nil
	}

	body := head[dnSlotProtoOff:]
	if dnSlotProtoOff+size > uint64(len(head)) {
		rest, err := d.readBlock(ctx, offset+dnSlotBlock,
			dnSlotRoundUp(dnSlotProtoOff+size)-dnSlotBlock)
		if err != nil {
			return nil, 0, false, fmt.Errorf(
				"reading the table slot at %d: %w", offset, err)
		}
		body = append(append([]byte{}, body...), rest...)
	}
	body = body[:size]
	if dnSlotCrc(head[:dnSlotCrcOff], body) != want {
		return nil, 0, false, nil
	}
	table := &pb.DnDiskTable{}
	if err := proto.Unmarshal(body, table); err != nil {
		return nil, 0, false, nil
	}
	return table, seq, true, nil
}

// parseDnHeader validates the header block. ok is false — with no error —
// only when the magic is absent, i.e. the disk is unformatted.
func parseDnHeader(raw []byte) (*pb.DnDiskHeader, bool, error) {
	if len(raw) < common.DnHeaderSize {
		return nil, false, fmt.Errorf(
			"short disk header: %d bytes", len(raw))
	}
	if string(raw[0:8]) != dnHeaderMagic {
		return nil, false, nil
	}
	version := binary.LittleEndian.Uint32(raw[8:12])
	if version != dnHeaderVersion {
		return nil, false, fmt.Errorf(
			"corrupt header: version is %d, want %d",
			version, dnHeaderVersion)
	}
	want := binary.LittleEndian.Uint32(raw[dnHeaderCrcOff:])
	if crc32.ChecksumIEEE(raw[:dnHeaderCrcOff]) != want {
		return nil, false, fmt.Errorf("corrupt header: crc mismatch")
	}
	size := uint64(binary.LittleEndian.Uint32(raw[12:16]))
	if dnHeaderProtoOff+size > uint64(dnHeaderCrcOff) {
		return nil, false, fmt.Errorf(
			"corrupt header: proto length %d out of range", size)
	}
	hdr := &pb.DnDiskHeader{}
	if err := proto.Unmarshal(
		raw[dnHeaderProtoOff:dnHeaderProtoOff+size], hdr); err != nil {
		return nil, false, fmt.Errorf("corrupt header: %w", err)
	}
	return hdr, true, nil
}

// dnSlotCrc is the slot checksum: the envelope's leading fields followed by
// the serialized table.
func dnSlotCrc(envelope []byte, body []byte) uint32 {
	sum := crc32.NewIEEE()
	sum.Write(envelope)
	sum.Write(body)
	return sum.Sum32()
}

func dnSlotRoundUp(n uint64) uint64 {
	return ((n + dnSlotBlock - 1) / dnSlotBlock) * dnSlotBlock
}

// ---------------------------------------------------------------------------
// Format
// ---------------------------------------------------------------------------

// EnsureFormatted is the DN5 replacement: probe first, format only an
// unformatted disk, and never overwrite a foreign one. On a converged disk it
// issues zero writes (SH16).
func (d *DiskMeta) EnsureFormatted(
	ctx context.Context,
	clusterId uint64,
	dnId uint64,
	extentSize uint64,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return err
	}
	if d.formatted {
		if d.hdr.GetClusterId() != clusterId ||
			d.hdr.GetDnId() != dnId ||
			d.hdr.GetExtentSize() != extentSize {
			d.verified = false
			return fmt.Errorf(
				"foreign disk: cluster/dn/extent is %d/%d/%d, want %d/%d/%d",
				d.hdr.GetClusterId(), d.hdr.GetDnId(), d.hdr.GetExtentSize(),
				clusterId, dnId, extentSize)
		}
		d.verified = true
		return nil
	}
	if extentSize == 0 {
		return fmt.Errorf("extent_size is 0")
	}
	formatUuid, err := newFormatUuid()
	if err != nil {
		return err
	}
	hdr := &pb.DnDiskHeader{
		ClusterId:       clusterId,
		DnId:            dnId,
		ExtentSize:      extentSize,
		FormatUuid:      formatUuid,
		DataOffset:      common.DnDataOffset,
		CloneMetaOffset: common.DnCloneMetaOffset,
		CloneMetaSize:   common.DnCloneMetaSize,
	}
	block, err := buildDnHeader(hdr)
	if err != nil {
		return err
	}
	// **Slot A first, header second.** That ordering makes "a valid header
	// implies at least one valid table slot" an invariant of the format, so
	// a later load that finds a valid header and *no* valid slot knows it is
	// looking at corruption rather than at a fresh-format crash window — and
	// can refuse instead of silently presenting an empty table and handing
	// out extents that are already in use. A crash between the two writes
	// leaves a slot with no header: the disk reads as unformatted, and the
	// next format stamps a new uuid that makes the orphan slot inert.
	table := &pb.DnDiskTable{}
	if err := d.writeSlot(ctx, 0, formatUuid, 1, table); err != nil {
		return err
	}
	if err := d.writeBlock(ctx, common.DnHeaderOffset, block); err != nil {
		return fmt.Errorf("writing the disk header: %w", err)
	}
	d.formatted = true
	d.verified = true
	d.hdr = hdr
	d.table = table
	d.seq = 1
	d.newestSlot = 0
	return nil
}

func newFormatUuid() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("generating a format uuid: %w", err)
	}
	// 0 is reserved: it is what an all-zero slot reads back as, so a slot
	// left over from a wiped header must never match a live format.
	value := binary.LittleEndian.Uint64(b[:])
	if value == 0 {
		value = 1
	}
	return value, nil
}

func buildDnHeader(hdr *pb.DnDiskHeader) ([]byte, error) {
	body, err := proto.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	if dnHeaderProtoOff+len(body) > dnHeaderCrcOff {
		return nil, fmt.Errorf("disk header proto is %d bytes, too large",
			len(body))
	}
	block := make([]byte, common.DnHeaderSize)
	copy(block[0:8], dnHeaderMagic)
	binary.LittleEndian.PutUint32(block[8:12], dnHeaderVersion)
	binary.LittleEndian.PutUint32(block[12:16], uint32(len(body)))
	copy(block[dnHeaderProtoOff:], body)
	binary.LittleEndian.PutUint32(
		block[dnHeaderCrcOff:], crc32.ChecksumIEEE(block[:dnHeaderCrcOff]))
	return block, nil
}

// ---------------------------------------------------------------------------
// Save protocol (rule 3)
// ---------------------------------------------------------------------------

// mutableLocked is the gate every mutator passes through: the disk must be
// formatted **and** its identity confirmed for this agent by a successful
// EnsureFormatted. Without it a node pointed at another node's disk would
// happily allocate extents in that disk's volume table — the DN converge
// reports the foreign disk as RES_STATUS_ERROR but, per DN19, does not stop
// the side converges that follow.
func (d *DiskMeta) mutableLocked() error {
	if !d.formatted {
		return fmt.Errorf("disk is not formatted")
	}
	if !d.verified {
		return fmt.Errorf(
			"disk identity is not confirmed for this node; " +
				"refusing to modify its volume table")
	}
	return nil
}

// save persists next into the slot that is *not* the newest valid one, at
// seq+1, and commits it in memory only once the write succeeded. A torn write
// can therefore only damage the older slot; the newest valid table is never
// touched. On failure the in-memory state stays at the old table.
func (d *DiskMeta) save(ctx context.Context, next *pb.DnDiskTable) error {
	slot := 0
	if d.newestSlot == 0 {
		slot = 1
	}
	seq := d.seq + 1
	if err := d.writeSlot(
		ctx, slot, d.hdr.GetFormatUuid(), seq, next); err != nil {
		return err
	}
	d.table = next
	d.seq = seq
	d.newestSlot = slot
	return nil
}

func (d *DiskMeta) writeSlot(
	ctx context.Context,
	slot int,
	formatUuid uint64,
	seq uint64,
	table *pb.DnDiskTable,
) error {
	body, err := proto.Marshal(table)
	if err != nil {
		return err
	}
	if uint64(dnSlotProtoOff+len(body)) > common.DnTableSlotSize {
		return fmt.Errorf("volume table is %d bytes, over the %d slot size",
			dnSlotProtoOff+len(body), common.DnTableSlotSize)
	}
	block := make([]byte, dnSlotRoundUp(uint64(dnSlotProtoOff+len(body))))
	copy(block[0:8], dnTableMagic)
	binary.LittleEndian.PutUint64(block[8:16], formatUuid)
	binary.LittleEndian.PutUint64(block[16:24], seq)
	binary.LittleEndian.PutUint32(block[24:28], uint32(len(body)))
	binary.LittleEndian.PutUint32(
		block[dnSlotCrcOff:], dnSlotCrc(block[:dnSlotCrcOff], body))
	copy(block[dnSlotProtoOff:], body)
	if err := d.writeBlock(ctx, dnSlotOffsets[slot], block); err != nil {
		return fmt.Errorf("writing table slot %d: %w", slot, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// Describe is the meta_info details string (§9.5).
func (d *DiskMeta) Describe() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.describeLocked()
}

func (d *DiskMeta) describeLocked() string {
	if !d.formatted {
		return "unformatted"
	}
	return fmt.Sprintf(
		"seq=%d sides=%d clone_metas=%d free_ext=%d free_meta_units=%d"+
			" provisioning=%d",
		d.seq, len(d.table.GetSideList()), len(d.table.GetCloneMetaList()),
		d.freeExtCnt(), d.freeUnitCnt(), d.provisioningSideCntLocked())
}

// provisioningSideCntLocked counts the sides whose §9.4 zeroing has not
// finished — the meta_info half of the per-side "zeroing k/n" detail
// (update_01.md U4). It is appended to Describe rather than folded into
// sides=%d so an operator can tell "this DN carries 12 sides" from "3 of them
// are still being provisioned" at a glance.
func (d *DiskMeta) provisioningSideCntLocked() uint64 {
	var cnt uint64
	for _, rec := range d.table.GetSideList() {
		if !sideFullyZeroed(rec) {
			cnt++
		}
	}
	return cnt
}

// ProbeHeader re-reads only the 4 KiB header — cheap enough for the 5 s
// health rounds — and verifies magic, version, CRC and identity.
func (d *DiskMeta) ProbeHeader(
	ctx context.Context,
	clusterId uint64,
	dnId uint64,
	extentSize uint64,
) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// The lazy load populates the table counts Describe reports. It is
	// read-only, so a probe round still mutates nothing (DN16/SH25).
	if err := d.load(ctx); err != nil {
		return "", err
	}
	raw, err := d.readBlock(ctx, common.DnHeaderOffset, common.DnHeaderSize)
	if err != nil {
		return "", fmt.Errorf("reading the disk header: %w", err)
	}
	hdr, ok, err := parseDnHeader(raw)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unformatted disk")
	}
	if hdr.GetClusterId() != clusterId || hdr.GetDnId() != dnId ||
		hdr.GetExtentSize() != extentSize {
		return "", fmt.Errorf(
			"foreign disk: cluster/dn/extent is %d/%d/%d, want %d/%d/%d",
			hdr.GetClusterId(), hdr.GetDnId(), hdr.GetExtentSize(),
			clusterId, dnId, extentSize)
	}
	return d.describeLocked(), nil
}

// LookupSide / LookupCloneMeta return the live record. A load failure is
// reported as an error and never collapsed into "absent" — a probe must be
// able to tell an unreadable disk from a side that was never allocated.
// Records are never mutated in place — every mutator saves a clone — so a
// returned pointer is safe to read but goes stale after the next write;
// re-look-up rather than caching one across a mutation.
func (d *DiskMeta) LookupSide(
	ctx context.Context,
	spId uint64,
	sideId uint64,
) (*pb.DnDiskTable_SideRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return nil, false, err
	}
	rec := findSide(d.table, spId, sideId)
	if rec == nil {
		return nil, false, nil
	}
	return rec, true, nil
}

func (d *DiskMeta) LookupCloneMeta(
	ctx context.Context,
	spId uint64,
	migrId uint64,
) (*pb.DnDiskTable_CloneMetaRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return nil, false, err
	}
	rec := findCloneMeta(d.table, spId, migrId)
	if rec == nil {
		return nil, false, nil
	}
	return rec, true, nil
}

// SideRecords / CloneMetaRecords are snapshots for the DN6 orphan sweep.
func (d *DiskMeta) SideRecords(
	ctx context.Context,
) ([]*pb.DnDiskTable_SideRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return nil, err
	}
	out := make([]*pb.DnDiskTable_SideRecord, 0,
		len(d.table.GetSideList()))
	out = append(out, d.table.GetSideList()...)
	return out, nil
}

func (d *DiskMeta) CloneMetaRecords(
	ctx context.Context,
) ([]*pb.DnDiskTable_CloneMetaRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return nil, err
	}
	out := make([]*pb.DnDiskTable_CloneMetaRecord, 0,
		len(d.table.GetCloneMetaList()))
	out = append(out, d.table.GetCloneMetaList()...)
	return out, nil
}

// Identity is the (cluster_id, dn_id) of a **confirmed** disk; ok is false on
// an unformatted, unloaded or unverified one. It is what lets a record found
// by the orphan sweep be turned back into a dm device name — and, because the
// sweep both removes dm devices and frees records, requiring verification
// here is what keeps the sweep off a foreign disk.
func (d *DiskMeta) Identity() (uint64, uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.formatted || !d.verified {
		return 0, 0, false
	}
	return d.hdr.GetClusterId(), d.hdr.GetDnId(), true
}

// SetDiskSize tells the allocator how big the raw device is; the DN converge
// and probe pass it the size they already read with lsblk. An allocation
// attempted before it is an error rather than a silent overrun.
func (d *DiskMeta) SetDiskSize(diskSize uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.diskSize = diskSize
}

// extentCntLocked is the number of extents the data area holds: the raw size
// minus the fixed prefix, divided by the header's extent_size.
func (d *DiskMeta) extentCntLocked() uint64 {
	extentSize := d.hdr.GetExtentSize()
	if !d.formatted || extentSize == 0 || d.diskSize <= common.DnDataOffset {
		return 0
	}
	return (d.diskSize - common.DnDataOffset) / extentSize
}

func findSide(
	table *pb.DnDiskTable,
	spId uint64,
	sideId uint64,
) *pb.DnDiskTable_SideRecord {
	for _, rec := range table.GetSideList() {
		if rec.GetSpId() == spId && rec.GetSideId() == sideId {
			return rec
		}
	}
	return nil
}

func findCloneMeta(
	table *pb.DnDiskTable,
	spId uint64,
	migrId uint64,
) *pb.DnDiskTable_CloneMetaRecord {
	for _, rec := range table.GetCloneMetaList() {
		if rec.GetSpId() == spId && rec.GetMigrId() == migrId {
			return rec
		}
	}
	return nil
}

func runTotal(rec *pb.DnDiskTable_SideRecord) uint64 {
	var total uint64
	for _, run := range rec.GetRunList() {
		total += run.GetCount()
	}
	return total
}

// ---------------------------------------------------------------------------
// zeroed_bits — the §9.4 side-provisioning bitmap (update_01.md U4)
//
// **Logical extent i** is the i-th extent of the concatenation of the record's
// run_list, i.e. bytes [i, i+1) x extent_size of the side's DnSideName
// dm-linear. That device is a gap-free concatenation of the runs in run_list
// order (sideRunSectors + sideDmConverged), so the logical index is the one
// address the zeroing command needs: one `blkdiscard --zeroout` covers a whole
// batch of consecutive logical extents no matter how fragmented the physical
// placement is.
//
// Bit i of zeroed_bits says logical extent i has been zeroed. The encoding is
// agent/bitmap.go's LSB-first one, and the bit count is ALWAYS the record's own
// extent total — never agent.BitmapBitCount, which returns len(bits)*8 and
// would make a 10-extent side look 16-extent, fire "zeroed == total" early and
// export another tenant's bytes.
//
// The helpers are free functions on the record rather than DiskMeta methods so
// the converge, the probe and the zeroing loop can all work off one LookupSide
// snapshot without re-taking d.mu (rulings R4.6/R4.7).
// ---------------------------------------------------------------------------

// sideExtCnt is the side's logical extent count — the bit count of
// zeroed_bits, and SideInfo.total_ext_cnt. It is runTotal by another name; use
// it wherever the number is a bit count so the two can never drift.
func sideExtCnt(rec *pb.DnDiskTable_SideRecord) uint64 {
	return runTotal(rec)
}

// sideZeroedCnt is SideInfo.zeroed_ext_cnt: how many of the side's extents are
// already zeroed.
func sideZeroedCnt(rec *pb.DnDiskTable_SideRecord) uint64 {
	return agent.BitmapCountSet(rec.GetZeroedBits(), runTotal(rec))
}

// sideFullyZeroed is the export gate's local half (§9.4 step 4): the agent
// trusts its own bits over the request's provisioned flag, because the disk is
// authoritative ([D13]) and the etcd flag is a gate, never evidence.
func sideFullyZeroed(rec *pb.DnDiskTable_SideRecord) bool {
	return agent.BitmapAllSet(rec.GetZeroedBits(), runTotal(rec))
}

// sideNextZeroBatch is the zeroing loop's cursor: the first not-yet-zeroed
// logical extent and how many consecutive not-yet-zeroed extents to cover in
// one `blkdiscard --zeroout`, capped at batch. ok is false when the side is
// fully zeroed.
//
// The offset comes from first-unset, never from the *count* of zeroed extents
// (ruling R4.7): the two agree only while the set bits are a contiguous
// prefix, which is all this protocol produces but not all the record can
// hold, and a count-derived offset would silently zero the wrong range for
// any other bitmap.
func sideNextZeroBatch(
	rec *pb.DnDiskTable_SideRecord,
	batch uint64,
) (from uint64, count uint64, ok bool) {
	total := runTotal(rec)
	from, ok = agent.BitmapFirstUnset(rec.GetZeroedBits(), total)
	if !ok {
		return 0, 0, false
	}
	return from, agent.BitmapUnsetRunFrom(
		rec.GetZeroedBits(), from, total, batch), true
}

// ---------------------------------------------------------------------------
// Allocation (rule 4). The free maps are derived from the table on load and
// never persisted.
// ---------------------------------------------------------------------------

// AllocSide returns the side's existing record, or allocates one. Resize is
// out of scope, exactly as it was with the fixed-size LV: an existing record
// whose extent total disagrees with the request is an error.
func (d *DiskMeta) AllocSide(
	ctx context.Context,
	spId uint64,
	sideId uint64,
	extCnt uint64,
) (*pb.DnDiskTable_SideRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return nil, err
	}
	if err := d.mutableLocked(); err != nil {
		return nil, err
	}
	if rec := findSide(d.table, spId, sideId); rec != nil {
		if got := runTotal(rec); got != extCnt {
			return nil, fmt.Errorf(
				"allocated %d extents, want %d", got, extCnt)
		}
		return rec, nil
	}
	if extCnt == 0 {
		return nil, fmt.Errorf("ext_cnt is 0")
	}
	runs, err := allocRuns(d.freeExtentMap(), d.extentCntLocked(), extCnt)
	if err != nil {
		return nil, fmt.Errorf("allocating %d extents: %w", extCnt, err)
	}
	// zeroed_bits is left empty on purpose (ruling R4.5): proto3 does not
	// serialize an empty bytes field, out-of-range bits read as 0, and
	// BitmapSetRange grows the slice on the first batch — so an absent field
	// is exactly update_01.md's "persist the record with zeroed_bits all 0",
	// at no cost in every slot write that follows.
	//
	// This literal is also where the U4 invariant is enforced: **zeroed is a
	// property of the side's ALLOCATION, not of the disk extent**. Extents
	// freed and reallocated to a new side start all-not-zeroed again, whatever
	// happened to them before, because this constructor is the only code that
	// can put extents into a record (an existing record's runs are never
	// changed — a resize is rejected above) and FreeSide deletes records
	// whole rather than blanking fields. A recycled extent therefore cannot
	// inherit a previous side's set bit.
	rec := &pb.DnDiskTable_SideRecord{
		SpId:    spId,
		SideId:  sideId,
		RunList: runs,
	}
	next := proto.Clone(d.table).(*pb.DnDiskTable)
	next.SideList = append(next.SideList, rec)
	if err := d.save(ctx, next); err != nil {
		return nil, err
	}
	// The saved copy is the one the table now holds.
	return findSide(d.table, spId, sideId), nil
}

// SetSideZeroed marks logical extents [fromExt, toExt) of a side as zeroed —
// step 3 of the §9.4 provisioning protocol (update_01.md U4), run once per
// completed batch. The range is half-open, and persisting it *after* the
// `blkdiscard --zeroout` returned is what makes an interrupted batch simply
// re-run: its bits stay 0, so the next pass redoes it rather than leaving a
// hole nobody knows about.
//
// It is a no-op — and issues no write — when every bit in the range is
// already set, which is what makes a restart's replay free (SH16).
func (d *DiskMeta) SetSideZeroed(
	ctx context.Context,
	spId uint64,
	sideId uint64,
	fromExt uint64,
	toExt uint64,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return err
	}
	if err := d.mutableLocked(); err != nil {
		return err
	}
	rec := findSide(d.table, spId, sideId)
	if rec == nil {
		return fmt.Errorf("no allocation record for side %016x-%016x",
			spId, sideId)
	}
	// The record's own extent total is the bit count, so a batch the caller
	// computed against a stale record — one whose side was freed and
	// reallocated meanwhile — is refused rather than setting pad bits.
	total := runTotal(rec)
	if fromExt > toExt || toExt > total {
		return fmt.Errorf(
			"zeroed range [%d,%d) is outside the side's %d extents",
			fromExt, toExt, total)
	}
	if fromExt == toExt {
		return nil
	}
	bits := rec.GetZeroedBits()
	already := true
	for i := fromExt; i < toExt; i++ {
		if !agent.BitmapBit(bits, i) {
			already = false
			break
		}
	}
	if already {
		return nil
	}
	next := proto.Clone(d.table).(*pb.DnDiskTable)
	target := findSide(next, spId, sideId)
	target.ZeroedBits = agent.BitmapSetRange(
		target.GetZeroedBits(), fromExt, toExt)
	return d.save(ctx, next)
}

// FreeSide is idempotent: an absent record issues no write.
func (d *DiskMeta) FreeSide(
	ctx context.Context,
	spId uint64,
	sideId uint64,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return err
	}
	if !d.formatted || findSide(d.table, spId, sideId) == nil {
		return nil
	}
	if err := d.mutableLocked(); err != nil {
		return err
	}
	next := proto.Clone(d.table).(*pb.DnDiskTable)
	kept := next.SideList[:0]
	for _, rec := range next.GetSideList() {
		if rec.GetSpId() == spId && rec.GetSideId() == sideId {
			continue
		}
		kept = append(kept, rec)
	}
	next.SideList = kept
	return d.save(ctx, next)
}

// AllocCloneMeta reserves a contiguous run of DnCloneMetaUnit units for one
// migration's dm-clone metadata. The first 8 KiB of a freshly chosen slot is
// zeroed **before** the record is persisted ([P6]): lvcreate used to zero
// implicitly, and without it stale bytes from a previous tenant would be
// misparsed as a valid dm-clone superblock. A crash after the zeroing but
// before the record leaves the units free and re-zeroed next time; a crash
// after the record means the slot is already clean.
func (d *DiskMeta) AllocCloneMeta(
	ctx context.Context,
	spId uint64,
	migrId uint64,
	bytes uint64,
) (*pb.DnDiskTable_CloneMetaRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return nil, err
	}
	if err := d.mutableLocked(); err != nil {
		return nil, err
	}
	if bytes == 0 {
		return nil, fmt.Errorf("clone metadata size is 0")
	}
	units := (bytes + common.DnCloneMetaUnit - 1) / common.DnCloneMetaUnit
	if rec := findCloneMeta(d.table, spId, migrId); rec != nil {
		// The slot is fixed once allocated, like a side's extents: report a
		// request that no longer fits rather than silently under-sizing the
		// dm-clone's metadata device.
		if rec.GetUnitCount() < units {
			return nil, fmt.Errorf(
				"allocated %d clone-metadata units, want %d",
				rec.GetUnitCount(), units)
		}
		return rec, nil
	}
	start, err := allocContiguous(d.freeUnitMap(), dnCloneMetaUnitCnt, units)
	if err != nil {
		return nil, fmt.Errorf(
			"allocating %d clone-metadata units: %w", units, err)
	}
	if err := d.zeroCloneMetaHead(ctx, start); err != nil {
		return nil, err
	}
	rec := &pb.DnDiskTable_CloneMetaRecord{
		SpId:      spId,
		MigrId:    migrId,
		UnitStart: start,
		UnitCount: units,
	}
	next := proto.Clone(d.table).(*pb.DnDiskTable)
	next.CloneMetaList = append(next.CloneMetaList, rec)
	if err := d.save(ctx, next); err != nil {
		return nil, err
	}
	return findCloneMeta(d.table, spId, migrId), nil
}

// dnCloneMetaZero is the prefix a fresh dm-clone metadata slot must read as
// zeros: the dm-clone superblock lives in the first block, and 8 KiB clears
// it under every block size the target uses.
const dnCloneMetaZero = 8 * 1024

func (d *DiskMeta) zeroCloneMetaHead(ctx context.Context, start uint64) error {
	offset := common.DnCloneMetaOffset + start*common.DnCloneMetaUnit
	if err := d.writeBlock(ctx, offset, make([]byte, dnCloneMetaZero)); err != nil {
		return fmt.Errorf("zeroing the clone-metadata slot at %d: %w",
			offset, err)
	}
	return nil
}

// FreeCloneMeta is idempotent.
func (d *DiskMeta) FreeCloneMeta(
	ctx context.Context,
	spId uint64,
	migrId uint64,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(ctx); err != nil {
		return err
	}
	if !d.formatted || findCloneMeta(d.table, spId, migrId) == nil {
		return nil
	}
	if err := d.mutableLocked(); err != nil {
		return err
	}
	next := proto.Clone(d.table).(*pb.DnDiskTable)
	kept := next.CloneMetaList[:0]
	for _, rec := range next.GetCloneMetaList() {
		if rec.GetSpId() == spId && rec.GetMigrId() == migrId {
			continue
		}
		kept = append(kept, rec)
	}
	next.CloneMetaList = kept
	return d.save(ctx, next)
}

const dnCloneMetaUnitCnt = common.DnCloneMetaSize / common.DnCloneMetaUnit

// freeExtentMap marks every extent index the table already hands out.
func (d *DiskMeta) freeExtentMap() map[uint64]struct{} {
	used := make(map[uint64]struct{})
	for _, rec := range d.table.GetSideList() {
		for _, run := range rec.GetRunList() {
			for i := uint64(0); i < run.GetCount(); i++ {
				used[run.GetStart()+i] = struct{}{}
			}
		}
	}
	return used
}

func (d *DiskMeta) freeUnitMap() map[uint64]struct{} {
	used := make(map[uint64]struct{})
	for _, rec := range d.table.GetCloneMetaList() {
		for i := uint64(0); i < rec.GetUnitCount(); i++ {
			used[rec.GetUnitStart()+i] = struct{}{}
		}
	}
	return used
}

func (d *DiskMeta) freeExtCnt() uint64 {
	total := d.extentCntLocked()
	used := uint64(len(d.freeExtentMap()))
	if total <= used {
		return 0
	}
	return total - used
}

func (d *DiskMeta) freeUnitCnt() uint64 {
	return dnCloneMetaUnitCnt - uint64(len(d.freeUnitMap()))
}

// allocRuns is the deterministic side allocator: first fit one contiguous run
// of want extents; failing that, take free runs largest-first (ties broken by
// the lower start) until the request is satisfied. The result is ordered by
// the order it was taken, and concatenating the runs gives the side device.
func allocRuns(
	used map[uint64]struct{},
	total uint64,
	want uint64,
) ([]*pb.DnDiskTable_ExtentRun, error) {
	if total == 0 {
		return nil, fmt.Errorf("the data area holds no extents")
	}
	free := freeRuns(used, total)
	for _, run := range free {
		if run.count >= want {
			return []*pb.DnDiskTable_ExtentRun{
				{Start: run.start, Count: want},
			}, nil
		}
	}
	var have uint64
	for _, run := range free {
		have += run.count
	}
	if have < want {
		return nil, fmt.Errorf("only %d of %d extents are free", have, total)
	}
	bySize := append([]extentRun(nil), free...)
	sort.Slice(bySize, func(i, j int) bool {
		if bySize[i].count != bySize[j].count {
			return bySize[i].count > bySize[j].count
		}
		return bySize[i].start < bySize[j].start
	})
	var out []*pb.DnDiskTable_ExtentRun
	left := want
	for _, run := range bySize {
		if left == 0 {
			break
		}
		take := run.count
		if take > left {
			take = left
		}
		out = append(out, &pb.DnDiskTable_ExtentRun{
			Start: run.start, Count: take,
		})
		left -= take
	}
	return out, nil
}

// allocContiguous is the clone-metadata allocator: first fit, contiguous
// only — the wrapper dm device is a single linear line.
func allocContiguous(
	used map[uint64]struct{},
	total uint64,
	want uint64,
) (uint64, error) {
	for _, run := range freeRuns(used, total) {
		if run.count >= want {
			return run.start, nil
		}
	}
	return 0, fmt.Errorf(
		"no contiguous run of %d units in the %d-unit area", want, total)
}

type extentRun struct {
	start uint64
	count uint64
}

// freeRuns lists the maximal free runs of [0, total), ascending by start.
func freeRuns(used map[uint64]struct{}, total uint64) []extentRun {
	var out []extentRun
	var start uint64
	inRun := false
	for i := uint64(0); i < total; i++ {
		if _, taken := used[i]; taken {
			if inRun {
				out = append(out, extentRun{start, i - start})
				inRun = false
			}
			continue
		}
		if !inRun {
			start, inRun = i, true
		}
	}
	if inRun {
		out = append(out, extentRun{start, total - start})
	}
	return out
}
