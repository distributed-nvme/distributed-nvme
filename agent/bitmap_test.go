package agent

import "testing"

// ---------------------------------------------------------------------------
// Explicit-bit-count bitmap helpers (§9.4 zeroed_bits)
// ---------------------------------------------------------------------------

func TestBitmapByteLen(t *testing.T) {
	for bits, want := range map[uint64]int{
		0: 0, 1: 1, 7: 1, 8: 1, 9: 2, 10: 2, 16: 2, 17: 3,
	} {
		if got := BitmapByteLen(bits); got != want {
			t.Errorf("BitmapByteLen(%d) = %d, want %d", bits, got, want)
		}
	}
}

func TestBitmapRangeHelpers(t *testing.T) {
	// An empty range never allocates: a side whose batch covered nothing must
	// not turn an absent proto3 bytes field into an explicit empty one.
	if got := BitmapSetRange(nil, 3, 3); got != nil {
		t.Errorf("an empty range grew the slice: %v", got)
	}
	if got := BitmapSetRange(nil, 5, 2); got != nil {
		t.Errorf("an inverted range grew the slice: %v", got)
	}

	// The first batch of a freshly allocated side starts from a nil slice and
	// grows it to exactly the bytes the range needs (AllocSide writes no
	// zeroed_bits at all, ruling R4.5).
	bits := BitmapSetRange(nil, 0, 3)
	if len(bits) != 1 || bits[0] != 0b0000_0111 {
		t.Fatalf("first batch = %v, want [0b111]", bits)
	}
	for idx := uint64(0); idx < 3; idx++ {
		if !BitmapBit(bits, idx) {
			t.Errorf("bit %d is not set", idx)
		}
	}
	if BitmapBit(bits, 3) {
		t.Error("BitmapSetRange set a bit past the range")
	}

	// A multi-byte range crossing a byte boundary, appended to the first one:
	// the batches are half-open and adjacent, so bits 0..9 end up set and the
	// slice grows to 2 bytes.
	bits = BitmapSetRange(bits, 3, 10)
	if len(bits) != 2 || bits[0] != 0xff || bits[1] != 0b0000_0011 {
		t.Fatalf("second batch = %v, want [0xff 0b11]", bits)
	}

	// A logical count that is not a multiple of 8: the two pad bits of byte 1
	// must never inflate the count or complete the side.
	if got := BitmapCountSet(bits, 10); got != 10 {
		t.Errorf("count of 10 bits = %d, want 10", got)
	}
	if got := BitmapCountSet(bits, 12); got != 10 {
		t.Errorf("count of 12 bits = %d, want 10", got)
	}
	if !BitmapAllSet(bits, 10) {
		t.Error("10 set bits did not read as complete")
	}
	if BitmapAllSet(bits, 12) {
		t.Error("two unset bits read as complete")
	}
	if _, unset := BitmapFirstUnset(bits, 10); unset {
		t.Error("a fully set range reported an unset bit")
	}
	if idx, unset := BitmapFirstUnset(bits, 12); !unset || idx != 10 {
		t.Errorf("first unset = %d/%v, want 10/true", idx, unset)
	}

	// A padded byte that is physically 0xff still counts only the logical
	// bits: this is the case BitmapBitCount would get wrong (risk R2).
	padded := []byte{0xff, 0xff}
	if got := BitmapCountSet(padded, 10); got != 10 {
		t.Errorf("padded count = %d, want 10", got)
	}
	if !BitmapAllSet(padded, 10) {
		t.Error("a padded byte broke the all-set predicate")
	}
	if got := BitmapBitCount(padded); got != 16 {
		t.Errorf("BitmapBitCount = %d, want 16 (the wire-chunk rule)", got)
	}

	// The zero-bit edge cases the converge matrix leans on.
	if !BitmapAllSet(nil, 0) {
		t.Error("BitmapAllSet(nil, 0) must be true")
	}
	if got := BitmapCountSet(nil, 4); got != 0 {
		t.Errorf("an absent bitmap counted %d set bits", got)
	}
	if idx, unset := BitmapFirstUnset(nil, 4); !unset || idx != 0 {
		t.Errorf("first unset of an absent bitmap = %d/%v", idx, unset)
	}
}

func TestBitmapUnsetRunFrom(t *testing.T) {
	// Extents 0-2 zeroed, 3-6 not, 7 zeroed: the batch cursor must stop at
	// the next already-zeroed extent rather than re-zeroing it.
	bits := []byte{0b1000_0111}
	from, unset := BitmapFirstUnset(bits, 8)
	if !unset || from != 3 {
		t.Fatalf("first unset = %d/%v, want 3/true", from, unset)
	}
	if got := BitmapUnsetRunFrom(bits, from, 8, 10); got != 4 {
		t.Errorf("run from %d = %d, want 4", from, got)
	}
	// The batch cap wins over the run length (DnZeroBatchExtCnt).
	if got := BitmapUnsetRunFrom(bits, from, 8, 2); got != 2 {
		t.Errorf("capped run = %d, want 2", got)
	}
	// The logical bit count wins too: extent 8 does not exist.
	if got := BitmapUnsetRunFrom(nil, 0, 6, 10); got != 6 {
		t.Errorf("run of an absent bitmap = %d, want 6", got)
	}
	// A set bit at from, and a from at/past the count, both yield 0.
	if got := BitmapUnsetRunFrom(bits, 0, 8, 10); got != 0 {
		t.Errorf("run from a set bit = %d, want 0", got)
	}
	if got := BitmapUnsetRunFrom(bits, 8, 8, 10); got != 0 {
		t.Errorf("run past the bit count = %d, want 0", got)
	}
}
