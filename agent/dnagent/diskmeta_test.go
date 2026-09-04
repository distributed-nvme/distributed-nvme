package dnagent

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The [D13] on-disk format, exercised against the fakeNode segment store.
// Every rule of the plan's §5.4 gets a test here.

const (
	metaDisk       = "/dev/meta-disk"
	metaDiskSize   = uint64(512 << 20) // 256 data extents at 1 MiB
	metaExtentSize = uint64(1 << 20)
	metaExtCnt     = (metaDiskSize - common.DnDataOffset) / metaExtentSize
)

func newTestMeta(t *testing.T) (*DiskMeta, *fakeNode) {
	t.Helper()
	node := newFakeNode()
	node.devSize[metaDisk] = metaDiskSize
	node.devNo[metaDisk] = "253:0"
	meta := NewDiskMeta(node.osClient(), metaDisk)
	meta.SetDiskSize(metaDiskSize)
	return meta, node
}

func formatted(t *testing.T) (*DiskMeta, *fakeNode) {
	t.Helper()
	meta, node := newTestMeta(t)
	if err := meta.EnsureFormatted(
		context.Background(), testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("EnsureFormatted: %v", err)
	}
	node.Reset()
	return meta, node
}

// reopen builds a second DiskMeta over the same fake disk — the "agent
// restarted, --local-store is gone, the disk is authoritative" case ([P4]).
func reopen(node *fakeNode) *DiskMeta {
	meta := NewDiskMeta(node.osClient(), metaDisk)
	meta.SetDiskSize(metaDiskSize)
	return meta
}

// ---------------------------------------------------------------------------
// Rule 1/2 — lazy load, format, probe-first idempotency
// ---------------------------------------------------------------------------

func TestDiskMetaFormatRoundTrip(t *testing.T) {
	meta, node := newTestMeta(t)
	ctx := context.Background()

	if err := meta.EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("EnsureFormatted: %v", err)
	}
	// Slot A with the first (empty) table, and only then the header: a valid
	// header must imply a valid table slot.
	assertOrder(t, node,
		fmt.Sprintf("readblock %s off=%d len=%d",
			metaDisk, common.DnHeaderOffset, common.DnHeaderSize),
		fmt.Sprintf("writeblock %s off=%d", metaDisk, common.DnTableSlotAOffset),
		fmt.Sprintf("writeblock %s off=%d", metaDisk, common.DnHeaderOffset),
	)
	if got := meta.Describe(); !strings.HasPrefix(got, "seq=1 sides=0") {
		t.Errorf("Describe = %q", got)
	}

	// A second DiskMeta over the same bytes reads the same state back.
	again := reopen(node)
	if got := again.Describe(); got != "unformatted" {
		t.Errorf("Describe before any load = %q, want unformatted", got)
	}
	if err := again.EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("re-open EnsureFormatted: %v", err)
	}
	if got := again.Describe(); !strings.HasPrefix(got, "seq=1 sides=0") {
		t.Errorf("re-opened Describe = %q", got)
	}
	if got := again.Describe(); !strings.Contains(
		got, fmt.Sprintf("free_ext=%d", metaExtCnt)) {
		t.Errorf("Describe = %q, want free_ext=%d", got, metaExtCnt)
	}
}

// SH16: re-calling EnsureFormatted on a converged disk issues zero writes.
// Integration case D's mutation-free reconcile depends on it.
func TestDiskMetaFormatIsIdempotent(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := meta.EnsureFormatted(
			ctx, testCluster, testDn, metaExtentSize); err != nil {
			t.Fatalf("EnsureFormatted #%d: %v", i, err)
		}
	}
	// A fresh DiskMeta must load from disk and still write nothing.
	if err := reopen(node).EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("re-opened EnsureFormatted: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("a converged disk was written: %s", call)
		}
	}
}

// Rule 2: a disk belonging to another cluster/dn, or formatted at a different
// extent size, is refused — never overwritten (parity with pvcreate).
func TestDiskMetaForeignDiskRefused(t *testing.T) {
	_, node := formatted(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name                string
		cluster, dn, extent uint64
	}{
		{"other cluster", testCluster + 1, testDn, metaExtentSize},
		{"other dn", testCluster, testDn + 1, metaExtentSize},
		{"other extent size", testCluster, testDn, metaExtentSize * 2},
	} {
		meta := reopen(node)
		err := meta.EnsureFormatted(ctx, tc.cluster, tc.dn, tc.extent)
		if err == nil {
			t.Fatalf("%s: EnsureFormatted succeeded", tc.name)
		}
		if !strings.Contains(err.Error(), "foreign disk") {
			t.Errorf("%s: error = %v, want a foreign-disk error", tc.name, err)
		}
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("a foreign disk was written: %s", call)
		}
	}
}

// A DiskMeta that has NOT confirmed the disk's identity — because
// EnsureFormatted was never called, or was refused as foreign — must refuse
// every mutation. A failed DN converge does not stop the side converges that
// follow (DN19), so without this guard a node pointed at another node's disk
// would allocate extents in that disk's volume table.
func TestDiskMetaUnverifiedRefusesMutation(t *testing.T) {
	_, node := formatted(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		build func() *DiskMeta
	}{
		{"never verified", func() *DiskMeta { return reopen(node) }},
		{"refused as foreign", func() *DiskMeta {
			m := reopen(node)
			if err := m.EnsureFormatted(
				ctx, testCluster+1, testDn, metaExtentSize); err == nil {
				t.Fatal("EnsureFormatted accepted a foreign disk")
			}
			return m
		}},
	} {
		meta := tc.build()
		if _, err := meta.AllocSide(ctx, testSp, testSide, 1); err == nil {
			t.Errorf("%s: AllocSide succeeded", tc.name)
		}
		if _, err := meta.AllocCloneMeta(
			ctx, testSp, testMigrId, 1<<20); err == nil {
			t.Errorf("%s: AllocCloneMeta succeeded", tc.name)
		}
		if err := meta.SetSideZeroed(
			ctx, testSp, testSide, 0, 1); err == nil {
			t.Errorf("%s: SetSideZeroed succeeded", tc.name)
		}
		if _, _, ok := meta.Identity(); ok {
			t.Errorf("%s: Identity reported a usable disk", tc.name)
		}
		// Reads still work — a probe must be able to report what it sees.
		if _, err := meta.ProbeHeader(
			ctx, testCluster, testDn, metaExtentSize); err != nil {
			t.Errorf("%s: ProbeHeader failed: %v", tc.name, err)
		}
		for _, call := range node.Calls() {
			if strings.HasPrefix(call, "writeblock") {
				t.Errorf("%s: an unverified disk was written: %s",
					tc.name, call)
			}
		}
	}

	// A record that exists is still freed idempotently only after
	// verification; an unverified free of an absent record stays a no-op.
	if err := reopen(node).FreeSide(ctx, testSp, 0xdead); err != nil {
		t.Errorf("freeing an absent record on an unverified disk: %v", err)
	}
}

// Rule 1: a header whose magic matches but whose CRC does not fails every
// operation and is never auto-formatted over.
func TestDiskMetaCorruptHeaderRefused(t *testing.T) {
	_, node := formatted(t)
	ctx := context.Background()
	// Flip a byte inside the header proto, leaving the magic intact.
	node.corruptBlock(metaDisk, common.DnHeaderOffset+20, []byte{0xff, 0xff})

	meta := reopen(node)
	err := meta.EnsureFormatted(ctx, testCluster, testDn, metaExtentSize)
	if err == nil || !strings.Contains(err.Error(), "corrupt header") {
		t.Fatalf("EnsureFormatted on a corrupt header = %v", err)
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide, 1); err == nil {
		t.Error("AllocSide succeeded on a corrupt header")
	}
	if _, err := meta.ProbeHeader(
		ctx, testCluster, testDn, metaExtentSize); err == nil {
		t.Error("ProbeHeader succeeded on a corrupt header")
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("a corrupt-header disk was written: %s", call)
		}
	}
}

func TestDiskMetaProbeHeader(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	details, err := meta.ProbeHeader(
		ctx, testCluster, testDn, metaExtentSize)
	if err != nil {
		t.Fatalf("ProbeHeader: %v", err)
	}
	if !strings.HasPrefix(details, "seq=") {
		t.Errorf("ProbeHeader details = %q", details)
	}
	// Only the 4 KiB header is re-read, and nothing is written (DN16).
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("ProbeHeader mutated: %s", call)
		}
	}
	if _, err := meta.ProbeHeader(
		ctx, testCluster+1, testDn, metaExtentSize); err == nil {
		t.Error("ProbeHeader accepted a foreign identity")
	}

	blank, blankNode := newTestMeta(t)
	if _, err := blank.ProbeHeader(
		ctx, testCluster, testDn, metaExtentSize); err == nil {
		t.Error("ProbeHeader accepted an unformatted disk")
	}
	_ = blankNode
}

// ---------------------------------------------------------------------------
// Rule 3 — the A/B save protocol
// ---------------------------------------------------------------------------

func TestDiskMetaSlotsAlternate(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	// Format wrote slot A at seq 1, so the saves go B, A, B.
	want := []uint64{
		common.DnTableSlotBOffset,
		common.DnTableSlotAOffset,
		common.DnTableSlotBOffset,
	}
	for i, offset := range want {
		node.Reset()
		if _, err := meta.AllocSide(
			ctx, testSp, testSide+uint64(i), 1); err != nil {
			t.Fatalf("AllocSide #%d: %v", i, err)
		}
		wantCall := fmt.Sprintf("writeblock %s off=%d", metaDisk, offset)
		if !node.hasCall(wantCall) {
			t.Fatalf("save #%d did not write slot at %d; calls: %v",
				i, offset, node.Calls())
		}
		for _, call := range node.Calls() {
			if strings.HasPrefix(call, "writeblock") &&
				!strings.HasPrefix(call, wantCall) {
				t.Errorf("save #%d wrote an unexpected slot: %s", i, call)
			}
		}
	}
	if got := meta.Describe(); !strings.HasPrefix(got, "seq=4 sides=3") {
		t.Errorf("Describe = %q, want seq=4 sides=3", got)
	}
}

// A torn newest slot loses to the older one: the CRC no longer checks, so the
// table the agent reloads is the previous committed state, never garbage.
func TestDiskMetaTornNewestSlotFallsBack(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	if _, err := meta.AllocSide(ctx, testSp, testSide, 4); err != nil {
		t.Fatalf("AllocSide: %v", err) // → slot B, seq 2
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide2, 4); err != nil {
		t.Fatalf("AllocSide: %v", err) // → slot A, seq 3 (the newest)
	}
	// Tear slot A's proto body — the CRC covers exactly those bytes.
	node.corruptBlock(metaDisk, common.DnTableSlotAOffset+dnSlotProtoOff,
		[]byte{0xde, 0xad, 0xbe, 0xef})

	back := reopen(node)
	if _, ok, _ := back.LookupSide(ctx, testSp, testSide); !ok {
		t.Error("the seq-2 side is missing after the fallback")
	}
	if _, ok, _ := back.LookupSide(ctx, testSp, testSide2); ok {
		t.Error("the torn slot's side survived")
	}
	if got := back.Describe(); !strings.HasPrefix(got, "seq=2 sides=1") {
		t.Errorf("Describe = %q, want the seq-2 table", got)
	}
}

// A slot left over from an earlier format of the same disk loses even with a
// higher seq: format_uuid, not seq, is what makes it foreign.
func TestDiskMetaStaleSlotRejectedAfterReformat(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := meta.AllocSide(
			ctx, testSp, testSide+uint64(i), 1); err != nil {
			t.Fatalf("AllocSide: %v", err)
		}
	}
	// Re-format: cleanup zeroes only the header block (§7.4), so both slots
	// survive with their old uuid and their high seqs.
	node.corruptBlock(metaDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))

	fresh := reopen(node)
	if err := fresh.EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("re-format: %v", err)
	}
	if got := fresh.Describe(); !strings.HasPrefix(got, "seq=1 sides=0") {
		t.Errorf("Describe after re-format = %q, want an empty table", got)
	}
	// And the freshly written slot A is the one that wins on the next load.
	if got := reopen(node); func() string {
		if err := got.EnsureFormatted(
			ctx, testCluster, testDn, metaExtentSize); err != nil {
			t.Fatalf("reload: %v", err)
		}
		return got.Describe()
	}() != fresh.Describe() {
		t.Error("the re-formatted disk did not reload identically")
	}
}

// A valid header with no valid table slot is corruption, not an empty disk.
// The format writes slot A before the header precisely so this can never be a
// fresh-format crash window — and reading it as an empty table would hand out
// extents that live sides still own.
func TestDiskMetaBothSlotsInvalidIsCorruption(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	if _, err := meta.AllocSide(ctx, testSp, testSide, 4); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	node.corruptBlock(metaDisk, common.DnTableSlotAOffset, make([]byte, 64))
	node.corruptBlock(metaDisk, common.DnTableSlotBOffset, make([]byte, 64))
	node.Reset()

	back := reopen(node)
	err := back.EnsureFormatted(ctx, testCluster, testDn, metaExtentSize)
	if err == nil {
		t.Fatal("a disk with no valid table slot was accepted")
	}
	if !strings.Contains(err.Error(), "corrupt volume table") {
		t.Errorf("error = %v, want a corrupt-volume-table error", err)
	}
	if _, err := back.AllocSide(ctx, testSp, testSide2, 1); err == nil {
		t.Error("AllocSide handed out extents from a corrupt table")
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("a corrupt-table disk was written: %s", call)
		}
	}
}

// The slot CRC covers the envelope, not just the proto body: a flipped bit in
// `seq` must not let the stale slot outrank the live one and roll the volume
// table back.
func TestDiskMetaSlotEnvelopeIsChecksummed(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	if _, err := meta.AllocSide(ctx, testSp, testSide, 4); err != nil {
		t.Fatalf("AllocSide: %v", err) // slot B, seq 2
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide2, 4); err != nil {
		t.Fatalf("AllocSide: %v", err) // slot A, seq 3 — the live one
	}

	// Forge a higher seq into the stale slot (B, seq 2 -> 99) without
	// touching its body. With a body-only CRC this would win and silently
	// discard testSide2's allocation.
	var seq [8]byte
	binary.LittleEndian.PutUint64(seq[:], 99)
	node.corruptBlock(metaDisk, common.DnTableSlotBOffset+16, seq[:])

	back := reopen(node)
	if err := back.EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("EnsureFormatted: %v", err)
	}
	if _, ok, _ := back.LookupSide(ctx, testSp, testSide2); !ok {
		t.Error("a forged seq rolled the volume table back")
	}
	if got := back.Describe(); !strings.HasPrefix(got, "seq=3 sides=2") {
		t.Errorf("Describe = %q, want the seq-3 table", got)
	}
}

// A format whose slot write fails must not leave a header behind: slot A goes
// first precisely so the disk stays "unformatted" until a complete format
// lands, and a retry then formats it cleanly.
func TestDiskMetaFormatSlotWriteFailure(t *testing.T) {
	meta, node := newTestMeta(t)
	ctx := context.Background()
	node.failBlockWrite = common.DnTableSlotAOffset
	node.failBlockSet = true

	if err := meta.EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err == nil {
		t.Fatal("EnsureFormatted succeeded with a failing slot write")
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, fmt.Sprintf(
			"writeblock %s off=%d", metaDisk, common.DnHeaderOffset)) {
			t.Fatalf("a header was written without a table slot: %s", call)
		}
	}
	if _, ok, _ := reopen(node).LookupSide(ctx, testSp, testSide); ok {
		t.Error("a half-formatted disk reads as formatted")
	}

	// The retry formats cleanly and the disk works.
	node.failBlockSet = false
	if err := meta.EnsureFormatted(
		ctx, testCluster, testDn, metaExtentSize); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide, 1); err != nil {
		t.Fatalf("AllocSide after the retry: %v", err)
	}
	back := reopen(node)
	if _, ok, err := back.LookupSide(ctx, testSp, testSide); !ok || err != nil {
		t.Errorf("the retried format did not survive a reload: %v %v", ok, err)
	}
}

// A save that fails leaves the in-memory table at its old value and reports
// the error; it never half-commits.
func TestDiskMetaSaveFailureDoesNotCommit(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	// Take the device away so WriteBlock fails.
	size := node.devSize[metaDisk]
	delete(node.devSize, metaDisk)

	if _, err := meta.AllocSide(ctx, testSp, testSide, 2); err == nil {
		t.Fatal("AllocSide succeeded with a failing device")
	}
	node.devSize[metaDisk] = size
	if _, ok, _ := meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("a failed save committed the record in memory")
	}
	if got := meta.Describe(); !strings.HasPrefix(got, "seq=1 sides=0") {
		t.Errorf("Describe = %q, want the pre-save table", got)
	}
	// The retry succeeds and lands on the same slot the failed one targeted.
	if _, err := meta.AllocSide(ctx, testSp, testSide, 2); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, ok, _ := meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Error("the retry did not commit")
	}
}

// ---------------------------------------------------------------------------
// Rule 4 — allocation
// ---------------------------------------------------------------------------

func TestDiskMetaAllocSideContiguous(t *testing.T) {
	meta, _ := formatted(t)
	ctx := context.Background()

	first, err := meta.AllocSide(ctx, testSp, testSide, 4)
	if err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	if len(first.GetRunList()) != 1 || first.GetRunList()[0].GetStart() != 0 ||
		first.GetRunList()[0].GetCount() != 4 {
		t.Fatalf("first allocation = %v, want one run 0+4", first.GetRunList())
	}
	if sideZeroedCnt(first) != 0 || sideExtCnt(first) != 4 {
		t.Errorf("a fresh record must start not-zeroed (§9.4): %d/%d",
			sideZeroedCnt(first), sideExtCnt(first))
	}
	if len(first.GetZeroedBits()) != 0 {
		t.Errorf("a fresh record carries a bitmap: %v",
			first.GetZeroedBits())
	}

	second, err := meta.AllocSide(ctx, testSp, testSide2, 3)
	if err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	if len(second.GetRunList()) != 1 || second.GetRunList()[0].GetStart() != 4 {
		t.Fatalf("second allocation = %v, want one run at 4",
			second.GetRunList())
	}

	// Re-calling with the same size returns the same record and writes
	// nothing; a different size is an error (resize is out of scope).
	again, err := meta.AllocSide(ctx, testSp, testSide, 4)
	if err != nil {
		t.Fatalf("re-AllocSide: %v", err)
	}
	if again.GetRunList()[0].GetStart() != 0 {
		t.Errorf("re-AllocSide moved the record: %v", again.GetRunList())
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide, 5); err == nil {
		t.Error("AllocSide accepted a resize")
	} else if !strings.Contains(err.Error(), "allocated 4 extents, want 5") {
		t.Errorf("resize error = %v", err)
	}
}

// With no contiguous run left, the allocator takes free runs largest-first —
// deterministically, and the concatenation is still exactly extCnt extents.
func TestDiskMetaAllocSideFragmentationFallback(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	// Fill the disk with 1-extent sides, then free every other one: the
	// largest free run is 1, so a 3-extent side must be stitched.
	for i := uint64(0); i < metaExtCnt; i++ {
		if _, err := meta.AllocSide(ctx, testSp, i, 1); err != nil {
			t.Fatalf("filling: %v", err)
		}
	}
	if _, err := meta.AllocSide(ctx, testSp, metaExtCnt, 1); err == nil {
		t.Error("the allocator handed out an extent past the end")
	}
	for i := uint64(1); i < metaExtCnt; i += 2 {
		if err := meta.FreeSide(ctx, testSp, i); err != nil {
			t.Fatalf("freeing: %v", err)
		}
	}

	rec, err := meta.AllocSide(ctx, testSp, 0xf00d, 3)
	if err != nil {
		t.Fatalf("fragmented AllocSide: %v", err)
	}
	if len(rec.GetRunList()) != 3 {
		t.Fatalf("runs = %v, want three 1-extent runs", rec.GetRunList())
	}
	var total uint64
	seen := map[uint64]bool{}
	for _, run := range rec.GetRunList() {
		total += run.GetCount()
		for i := uint64(0); i < run.GetCount(); i++ {
			idx := run.GetStart() + i
			if seen[idx] {
				t.Errorf("extent %d handed out twice", idx)
			}
			seen[idx] = true
			if idx%2 == 0 {
				t.Errorf("extent %d is still allocated to another side", idx)
			}
		}
	}
	if total != 3 {
		t.Errorf("run total = %d, want 3", total)
	}

	// Deterministic: the same disk state produces the same layout.
	replayed := reopen(node)
	if _, ok, _ := replayed.LookupSide(ctx, testSp, 0xf00d); !ok {
		t.Fatal("the stitched record did not survive a reload")
	}
}

func TestDiskMetaExhaustion(t *testing.T) {
	meta, _ := formatted(t)
	ctx := context.Background()

	if _, err := meta.AllocSide(ctx, testSp, testSide, metaExtCnt+1); err == nil {
		t.Error("AllocSide handed out more extents than exist")
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide, metaExtCnt); err != nil {
		t.Fatalf("whole-disk AllocSide: %v", err)
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide2, 1); err == nil {
		t.Error("AllocSide succeeded on a full disk")
	}

	// The clone-metadata area exhausts independently.
	units := uint64(common.DnCloneMetaSize / common.DnCloneMetaUnit)
	if _, err := meta.AllocCloneMeta(ctx, testSp, testMigrId,
		units*common.DnCloneMetaUnit); err != nil {
		t.Fatalf("whole-area AllocCloneMeta: %v", err)
	}
	if _, err := meta.AllocCloneMeta(
		ctx, testSp, testMigrId+1, 1); err == nil {
		t.Error("AllocCloneMeta succeeded on a full area")
	}
}

// [P6]: the chosen slot's first 8 KiB is zeroed *before* the record is
// persisted, so a crash can never leave a stale dm-clone superblock behind a
// live record.
func TestDiskMetaCloneMetaZeroedBeforeRecord(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	// Plant a superblock-shaped pattern where the slot will land.
	stale := make([]byte, 8*1024)
	for i := range stale {
		stale[i] = 0xa5
	}
	node.corruptBlock(metaDisk, common.DnCloneMetaOffset, stale)

	node.Reset()
	rec, err := meta.AllocCloneMeta(ctx, testSp, testMigrId, 5<<20)
	if err != nil {
		t.Fatalf("AllocCloneMeta: %v", err)
	}
	if rec.GetUnitStart() != 0 || rec.GetUnitCount() != 2 {
		t.Errorf("record = %v, want 2 units at 0", rec)
	}
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d len=8192",
			metaDisk, common.DnCloneMetaOffset),
		fmt.Sprintf("writeblock %s off=%d", metaDisk, common.DnTableSlotBOffset),
	)
	got, err := node.readBlock(ctx, metaDisk, common.DnCloneMetaOffset, 8192)
	if err != nil {
		t.Fatalf("readBlock: %v", err)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("byte %d of the fresh slot = %#x, want 0", i, b)
		}
	}

	// Re-calling returns the same record and re-zeroes nothing.
	node.Reset()
	again, err := meta.AllocCloneMeta(ctx, testSp, testMigrId, 5<<20)
	if err != nil {
		t.Fatalf("re-AllocCloneMeta: %v", err)
	}
	if again.GetUnitStart() != rec.GetUnitStart() {
		t.Errorf("re-AllocCloneMeta moved the slot")
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("a converged clone-meta record was rewritten: %s", call)
		}
	}

	// A second migration lands after the first, contiguously.
	next, err := meta.AllocCloneMeta(ctx, testSp, testMigrId+1, 1<<20)
	if err != nil {
		t.Fatalf("second AllocCloneMeta: %v", err)
	}
	if next.GetUnitStart() != 2 || next.GetUnitCount() != 1 {
		t.Errorf("second record = %v, want 1 unit at 2", next)
	}
}

// ---------------------------------------------------------------------------
// Rule 5 — free is idempotent; the zeroed bits; the sweep snapshots
// ---------------------------------------------------------------------------

func TestDiskMetaFreeIsIdempotent(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	if _, err := meta.AllocSide(ctx, testSp, testSide, 2); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	if _, err := meta.AllocCloneMeta(
		ctx, testSp, testMigrId, 1<<20); err != nil {
		t.Fatalf("AllocCloneMeta: %v", err)
	}
	if err := meta.FreeSide(ctx, testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	if err := meta.FreeCloneMeta(ctx, testSp, testMigrId); err != nil {
		t.Fatalf("FreeCloneMeta: %v", err)
	}

	node.Reset()
	if err := meta.FreeSide(ctx, testSp, testSide); err != nil {
		t.Errorf("second FreeSide: %v", err)
	}
	if err := meta.FreeSide(ctx, testSp, 0xdead); err != nil {
		t.Errorf("FreeSide of an unknown side: %v", err)
	}
	if err := meta.FreeCloneMeta(ctx, testSp, testMigrId); err != nil {
		t.Errorf("second FreeCloneMeta: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("an idempotent free wrote: %s", call)
		}
	}
	// The space came back.
	if got := meta.Describe(); !strings.Contains(
		got, fmt.Sprintf("free_ext=%d", metaExtCnt)) {
		t.Errorf("Describe = %q, want every extent free again", got)
	}
	if got := meta.Describe(); !strings.Contains(got, "free_meta_units=48") {
		t.Errorf("Describe = %q, want every unit free again", got)
	}
}

// The §9.4 batch setter: half-open ranges, an idempotent re-set that writes
// nothing, and progress that survives the A/B slot round trip
// (update_01.md U4).
func TestDiskMetaSetSideZeroed(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	if _, err := meta.AllocSide(ctx, testSp, testSide, 4); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	rec, _, _ := meta.LookupSide(ctx, testSp, testSide)
	if sideZeroedCnt(rec) != 0 || sideFullyZeroed(rec) {
		t.Fatalf("a fresh record is already zeroed: %d/%d",
			sideZeroedCnt(rec), sideExtCnt(rec))
	}
	if from, count, ok := sideNextZeroBatch(rec, 10); !ok ||
		from != 0 || count != 4 {
		t.Fatalf("first batch = (%d,%d,%v), want (0,4,true)",
			from, count, ok)
	}
	// meta_info carries the same fact per node: provisioning= counts the sides
	// whose bits are INCOMPLETE, not the sides (update_01.md U4, DN18). An
	// operator reading it on a fully provisioned DN must see 0.
	if got := meta.Describe(); !strings.Contains(got, "provisioning=1") {
		t.Errorf("Describe() = %q, want provisioning=1 while the side is "+
			"being zeroed", got)
	}

	// An empty range is a no-op, not an error, and writes nothing.
	node.Reset()
	if err := meta.SetSideZeroed(ctx, testSp, testSide, 2, 2); err != nil {
		t.Fatalf("empty SetSideZeroed: %v", err)
	}
	assertNoWrite(t, node, "an empty zeroed range")

	if err := meta.SetSideZeroed(ctx, testSp, testSide, 0, 2); err != nil {
		t.Fatalf("SetSideZeroed: %v", err)
	}
	rec, _, _ = meta.LookupSide(ctx, testSp, testSide)
	if sideZeroedCnt(rec) != 2 || sideFullyZeroed(rec) {
		t.Errorf("after [0,2): %d/%d zeroed, fully=%v",
			sideZeroedCnt(rec), sideExtCnt(rec), sideFullyZeroed(rec))
	}
	if from, count, ok := sideNextZeroBatch(rec, 10); !ok ||
		from != 2 || count != 2 {
		t.Errorf("second batch = (%d,%d,%v), want (2,2,true)",
			from, count, ok)
	}

	// Re-setting a range whose bits are already set issues no write: that is
	// what makes a restart's replay free (SH16).
	node.Reset()
	if err := meta.SetSideZeroed(ctx, testSp, testSide, 0, 2); err != nil {
		t.Fatalf("re-SetSideZeroed: %v", err)
	}
	assertNoWrite(t, node, "re-zeroing an already-zeroed range")

	if err := meta.SetSideZeroed(ctx, testSp, testSide, 2, 4); err != nil {
		t.Fatalf("SetSideZeroed: %v", err)
	}
	rec, _, _ = meta.LookupSide(ctx, testSp, testSide)
	if !sideFullyZeroed(rec) {
		t.Errorf("after [2,4) the side is not fully zeroed: %d/%d",
			sideZeroedCnt(rec), sideExtCnt(rec))
	}
	if _, _, ok := sideNextZeroBatch(rec, 10); ok {
		t.Error("a fully zeroed side still offers a batch")
	}
	if got := meta.Describe(); !strings.Contains(got, "sides=1") ||
		!strings.Contains(got, "provisioning=0") {
		t.Errorf("Describe() = %q, want sides=1 with provisioning=0 once "+
			"every bit is set", got)
	}

	// Out of range and inverted ranges are refused, and refused before any
	// write: the record's own extent total is the bit count, so a batch
	// computed against a stale record can never set a pad bit.
	node.Reset()
	for _, tc := range []struct{ from, to uint64 }{{0, 5}, {3, 1}, {4, 9}} {
		err := meta.SetSideZeroed(ctx, testSp, testSide, tc.from, tc.to)
		if err == nil {
			t.Errorf("SetSideZeroed accepted [%d,%d)", tc.from, tc.to)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf(
			"zeroed range [%d,%d) is outside the side's 4 extents",
			tc.from, tc.to)) {
			t.Errorf("range error = %v", err)
		}
	}
	assertNoWrite(t, node, "an out-of-range zeroed range")

	if err := meta.SetSideZeroed(ctx, testSp, 0xdead, 0, 1); err == nil {
		t.Error("SetSideZeroed accepted an unknown side")
	}

	// The bits survive a reload — the disk, not the local store, is
	// authoritative ([D13]), and every batch went through the alternating
	// A/B slots.
	back := reopen(node)
	rec, ok, _ := back.LookupSide(ctx, testSp, testSide)
	if !ok || !sideFullyZeroed(rec) {
		t.Error("the zeroed bits did not survive a reload")
	}

	// The U4 invariant: zeroed is a property of the side's ALLOCATION, not of
	// the disk extent. Freeing and re-allocating the same ids hands back the
	// same extents with a record that starts all-not-zeroed again.
	if err := meta.FreeSide(ctx, testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	again, err := meta.AllocSide(ctx, testSp, testSide, 4)
	if err != nil {
		t.Fatalf("re-AllocSide: %v", err)
	}
	if again.GetRunList()[0].GetStart() != 0 {
		t.Fatalf("the re-allocation moved: %v", again.GetRunList())
	}
	if sideZeroedCnt(again) != 0 || sideFullyZeroed(again) {
		t.Errorf("a re-allocated side inherited zeroed bits: %d/%d",
			sideZeroedCnt(again), sideExtCnt(again))
	}
}

// The on-disk encoding of zeroed_bits: LSB-first, trailing pad bits 0, and a
// bit count that is the side's extent total rather than len(bits)*8 — a
// 10-extent side must never look 16-extent (ruling R4.6, U5 item 3).
func TestDiskMetaZeroedBitsEncoding(t *testing.T) {
	meta, _ := formatted(t)
	ctx := context.Background()
	if _, err := meta.AllocSide(ctx, testSp, testSide, 10); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}

	// A batch that crosses a byte boundary sets exactly its own bits.
	if err := meta.SetSideZeroed(ctx, testSp, testSide, 6, 9); err != nil {
		t.Fatalf("SetSideZeroed: %v", err)
	}
	rec, _, _ := meta.LookupSide(ctx, testSp, testSide)
	if got := rec.GetZeroedBits(); len(got) != 2 ||
		got[0] != 0xc0 || got[1] != 0x01 {
		t.Fatalf("zeroed_bits after [6,9) = %#v, want [0xc0 0x01]", got)
	}
	if got := sideZeroedCnt(rec); got != 3 {
		t.Errorf("zeroed count = %d, want 3", got)
	}

	if err := meta.SetSideZeroed(ctx, testSp, testSide, 0, 10); err != nil {
		t.Fatalf("SetSideZeroed: %v", err)
	}
	rec, _, _ = meta.LookupSide(ctx, testSp, testSide)
	if got := rec.GetZeroedBits(); len(got) != 2 ||
		got[0] != 0xff || got[1] != 0x03 {
		t.Fatalf("zeroed_bits after [0,10) = %#v, want [0xff 0x03]", got)
	}
	// The pad bits of the last byte stay 0, and the count is 10 — not the 16
	// a len(bits)*8 bit count would report.
	if got := sideZeroedCnt(rec); got != 10 {
		t.Errorf("zeroed count = %d, want 10", got)
	}
	if !sideFullyZeroed(rec) {
		t.Error("10 of 10 extents zeroed is not fully zeroed")
	}
}

// sideNextZeroBatch walks a side in DnZeroBatchExtCnt-sized steps and stops
// exactly at the side's extent total, which is not a multiple of 8 (nor of the
// batch size) — the case where a pad-bit-counting cursor would run off the end
// of the side and zero another tenant's extents.
func TestDiskMetaNextZeroBatchSteps(t *testing.T) {
	meta, _ := formatted(t)
	ctx := context.Background()
	const extCnt = 21
	if _, err := meta.AllocSide(ctx, testSp, testSide, extCnt); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}

	want := [][2]uint64{{0, 10}, {10, 10}, {20, 1}}
	for _, step := range want {
		rec, _, _ := meta.LookupSide(ctx, testSp, testSide)
		from, count, ok := sideNextZeroBatch(rec, common.DnZeroBatchExtCnt)
		if !ok || from != step[0] || count != step[1] {
			t.Fatalf("batch = (%d,%d,%v), want (%d,%d,true)",
				from, count, ok, step[0], step[1])
		}
		if err := meta.SetSideZeroed(
			ctx, testSp, testSide, from, from+count); err != nil {
			t.Fatalf("SetSideZeroed: %v", err)
		}
	}
	rec, _, _ := meta.LookupSide(ctx, testSp, testSide)
	if _, _, ok := sideNextZeroBatch(rec, common.DnZeroBatchExtCnt); ok {
		t.Error("the walk did not stop at the side's last extent")
	}
	if got := sideZeroedCnt(rec); got != extCnt {
		t.Errorf("zeroed count = %d, want %d", got, extCnt)
	}

	// A hole left behind by a failed batch is picked up again from
	// first-unset, not from a count-derived offset (ruling R4.7).
	if err := meta.FreeSide(ctx, testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide, extCnt); err != nil {
		t.Fatalf("re-AllocSide: %v", err)
	}
	if err := meta.SetSideZeroed(
		ctx, testSp, testSide, 3, extCnt); err != nil {
		t.Fatalf("SetSideZeroed: %v", err)
	}
	rec, _, _ = meta.LookupSide(ctx, testSp, testSide)
	from, count, ok := sideNextZeroBatch(rec, common.DnZeroBatchExtCnt)
	if !ok || from != 0 || count != 3 {
		t.Errorf("batch over a hole = (%d,%d,%v), want (0,3,true)",
			from, count, ok)
	}
}

// assertNoWrite fails when anything reached the disk since the last Reset.
func assertNoWrite(t *testing.T, node *fakeNode, what string) {
	t.Helper()
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("%s wrote: %s", what, call)
		}
	}
}

func TestDiskMetaRecordSnapshots(t *testing.T) {
	meta, _ := formatted(t)
	ctx := context.Background()
	if _, err := meta.AllocSide(ctx, testSp, testSide, 1); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	if _, err := meta.AllocSide(ctx, testSp, testSide2, 1); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	if _, err := meta.AllocCloneMeta(
		ctx, testSp, testMigrId, 1<<20); err != nil {
		t.Fatalf("AllocCloneMeta: %v", err)
	}
	if got := mustSideRecords(t, meta, ctx); len(got) != 2 {
		t.Errorf("SideRecords = %d records, want 2", len(got))
	}
	if got := mustCloneMetaRecords(t, meta, ctx); len(got) != 1 {
		t.Errorf("CloneMetaRecords = %d records, want 1", len(got))
	}
	// The snapshot is a copy of the slice: appending to it cannot corrupt
	// the live table.
	snapshot := mustSideRecords(t, meta, ctx)
	snapshot = append(snapshot, nil)
	if got := mustSideRecords(t, meta, ctx); len(got) != 2 {
		t.Errorf("the live table was disturbed: %d records", len(got))
	}
	_ = snapshot
}

// The envelope is exactly what §5.2 of the plan specifies — a decoder written
// against the document must be able to read it.
func TestDiskMetaEnvelopeLayout(t *testing.T) {
	_, node := formatted(t)
	ctx := context.Background()

	hdr, err := node.readBlock(
		ctx, metaDisk, common.DnHeaderOffset, common.DnHeaderSize)
	if err != nil {
		t.Fatalf("readBlock: %v", err)
	}
	if string(hdr[0:8]) != "DNVDISK1" {
		t.Errorf("header magic = %q", hdr[0:8])
	}
	if got := binary.LittleEndian.Uint32(hdr[8:12]); got != 1 {
		t.Errorf("header version = %d, want 1", got)
	}

	slot, err := node.readBlock(ctx, metaDisk, common.DnTableSlotAOffset, 4096)
	if err != nil {
		t.Fatalf("readBlock: %v", err)
	}
	if string(slot[0:8]) != "DNVTABL1" {
		t.Errorf("slot magic = %q", slot[0:8])
	}
	if got := binary.LittleEndian.Uint64(slot[16:24]); got != 1 {
		t.Errorf("slot seq = %d, want 1", got)
	}
	uuid := binary.LittleEndian.Uint64(slot[8:16])
	if uuid == 0 {
		t.Error("format_uuid is 0; a wiped slot would then look valid")
	}

	// The layout constants must not overlap and must tile up to the data
	// area — a later edit that breaks this is a format-version bump.
	if common.DnHeaderOffset+common.DnHeaderSize > common.DnTableSlotAOffset {
		t.Error("the header overlaps table slot A")
	}
	if common.DnTableSlotAOffset+common.DnTableSlotSize >
		common.DnTableSlotBOffset {
		t.Error("table slot A overlaps slot B")
	}
	if common.DnTableSlotBOffset+common.DnTableSlotSize >
		common.DnCloneMetaOffset {
		t.Error("table slot B overlaps the clone-metadata area")
	}
	if common.DnCloneMetaOffset+common.DnCloneMetaSize !=
		common.DnDataOffset {
		t.Error("the clone-metadata area does not end at the data area")
	}
	if common.DnCloneMetaSize%common.DnCloneMetaUnit != 0 {
		t.Error("the clone-metadata area is not a whole number of units")
	}
}

func mustSideRecords(
	t *testing.T, meta *DiskMeta, ctx context.Context,
) []*pb.DnDiskTable_SideRecord {
	t.Helper()
	recs, err := meta.SideRecords(ctx)
	if err != nil {
		t.Fatalf("SideRecords: %v", err)
	}
	return recs
}

func mustCloneMetaRecords(
	t *testing.T, meta *DiskMeta, ctx context.Context,
) []*pb.DnDiskTable_CloneMetaRecord {
	t.Helper()
	recs, err := meta.CloneMetaRecords(ctx)
	if err != nil {
		t.Fatalf("CloneMetaRecords: %v", err)
	}
	return recs
}

// A volume table that outgrows the first 4 KiB block exercises the slot's
// two-part read path (envelope block, then the remainder the length field
// asks for). Nothing else in the suite makes the table that big.
func TestDiskMetaMultiBlockSlot(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()

	// One extent each, ids wide enough that the serialized records are not
	// varint-tiny: 256 of them run the table well past one block.
	for i := uint64(0); i < metaExtCnt; i++ {
		if _, err := meta.AllocSide(ctx,
			0xdeadbeefcafe0000+i, 0xfeedfacefeed0000+i, 1); err != nil {
			t.Fatalf("AllocSide %d: %v", i, err)
		}
	}
	var largest uint64
	for _, call := range node.Calls() {
		var off, ln uint64
		if _, err := fmt.Sscanf(call,
			"writeblock "+metaDisk+" off=%d len=%d", &off, &ln); err == nil {
			if ln > largest {
				largest = ln
			}
		}
	}
	if largest <= dnSlotBlock {
		t.Fatalf("the table never exceeded one %d-byte block (largest write "+
			"%d); this test is not exercising the multi-block path",
			dnSlotBlock, largest)
	}

	// Every record must come back from disk, byte for byte.
	back := reopen(node)
	recs, err := back.SideRecords(ctx)
	if err != nil {
		t.Fatalf("SideRecords: %v", err)
	}
	if uint64(len(recs)) != metaExtCnt {
		t.Fatalf("reloaded %d records, want %d", len(recs), metaExtCnt)
	}
	seen := map[uint64]bool{}
	for i := uint64(0); i < metaExtCnt; i++ {
		rec, ok, err := back.LookupSide(ctx,
			0xdeadbeefcafe0000+i, 0xfeedfacefeed0000+i)
		if !ok || err != nil {
			t.Fatalf("record %d missing after reload: %v", i, err)
		}
		if runTotal(rec) != 1 {
			t.Fatalf("record %d has %d extents, want 1", i, runTotal(rec))
		}
		idx := rec.GetRunList()[0].GetStart()
		if seen[idx] {
			t.Fatalf("extent %d handed out twice", idx)
		}
		seen[idx] = true
	}
	// A torn multi-block slot still falls back to the older one.
	node.corruptBlock(metaDisk, common.DnTableSlotAOffset+dnSlotBlock+8,
		[]byte{0xff, 0xff, 0xff, 0xff})
	node.corruptBlock(metaDisk, common.DnTableSlotBOffset+dnSlotBlock+8,
		[]byte{0xff, 0xff, 0xff, 0xff})
	if _, err := reopen(node).SideRecords(ctx); err == nil {
		t.Error("a torn multi-block slot was accepted")
	}
}

// The fragmentation fallback must be deterministic: the same disk state
// produces the same layout, whichever DiskMeta computes it.
func TestDiskMetaAllocIsDeterministic(t *testing.T) {
	ctx := context.Background()
	// Fill the disk with 1-extent sides and free every other one: the
	// largest free run is 1, so the request below cannot be satisfied
	// contiguously and must go through the largest-first fallback.
	layout := func() string {
		meta, _ := formatted(t)
		for i := uint64(0); i < metaExtCnt; i++ {
			if _, err := meta.AllocSide(ctx, 1, i, 1); err != nil {
				t.Fatalf("alloc %d: %v", i, err)
			}
		}
		for i := uint64(1); i < metaExtCnt; i += 2 {
			if err := meta.FreeSide(ctx, 1, i); err != nil {
				t.Fatalf("free %d: %v", i, err)
			}
		}
		rec, err := meta.AllocSide(ctx, 3, 99, 7)
		if err != nil {
			t.Fatalf("fragmented alloc: %v", err)
		}
		return fmt.Sprintf("%v", rec.GetRunList())
	}
	first := layout()
	if second := layout(); first != second {
		t.Errorf("allocation is not deterministic:\n  %s\n  %s", first, second)
	}
	if strings.Count(first, "count:1") != 7 {
		t.Errorf("the fallback did not stitch seven 1-extent runs: %s", first)
	}
}
