package common

import "testing"

const (
	kib = uint64(1024)
	mib = 1024 * kib
	gib = 1024 * mib
	tib = 1024 * gib
)

// TestCloneMetaUnits pins the formula of architecture.md, Metadata units, on
// the examples both agents state and on its edges: the base is one unit, each
// whole block adds one byte, a partial block adds nothing, and a block size
// of zero counts no region at all.
func TestCloneMetaUnits(t *testing.T) {
	// fourMiRegions is the region count at which the regions alone fill one
	// unit, so the total is exactly two units.
	const fourMiRegions = 4 * 1024 * 1024
	cases := []struct {
		name      string
		copyBytes uint64
		blockSize uint64
		want      uint64
	}{
		{"an empty copy", 0, mib, 1},
		{"under one block", mib / 2, mib, 1},
		{"one byte under one block", mib - 1, mib, 1},
		{"one block", mib, mib, 2},
		{"a partial second block", 2*mib - 1, mib, 2},
		{"1 TiB at 64 KiB", tib, 64 * kib, 5},
		{"1 TiB at 1 MiB", tib, mib, 2},
		{"10 GiB at 1 MiB", 10 * gib, mib, 2},
		{"one region under 4 Mi regions", (fourMiRegions - 1) * 64 * kib,
			64 * kib, 2},
		{"4 Mi regions", fourMiRegions * 64 * kib, 64 * kib, 2},
		{"4 Mi regions and one", (fourMiRegions + 1) * 64 * kib, 64 * kib, 3},
		{"8 Mi regions", 2 * fourMiRegions * 64 * kib, 64 * kib, 3},
		{"8 Mi regions and one", (2*fourMiRegions + 1) * 64 * kib,
			64 * kib, 4},
		{"block size 0", tib, 0, 1},
		{"block size 0 and an empty copy", 0, 0, 1},
	}
	for _, tc := range cases {
		if got := CloneMetaUnits(tc.copyBytes, tc.blockSize); got != tc.want {
			t.Errorf("%s: CloneMetaUnits(%d, %d) = %d, want %d",
				tc.name, tc.copyBytes, tc.blockSize, got, tc.want)
		}
	}
}

// TestCloneMetaBytes pins the byte budget CloneMetaUnits rounds up
// (architecture.md, Metadata units): the base, and one byte per whole block
// of the copy, with no rounding of its own, so a partial block adds nothing
// and a block size of zero counts no region at all.
func TestCloneMetaBytes(t *testing.T) {
	cases := []struct {
		name      string
		copyBytes uint64
		blockSize uint64
		want      uint64
	}{
		{"an empty copy", 0, mib, CloneMetaBaseBytes},
		{"under one block", mib - 1, mib, CloneMetaBaseBytes},
		{"one block", mib, mib, CloneMetaBaseBytes + 1},
		{"a partial second block", 2*mib - 1, mib, CloneMetaBaseBytes + 1},
		{"1 TiB at 1 MiB", tib, mib, CloneMetaBaseBytes + mib},
		{"1 TiB at 64 KiB", tib, 64 * kib, CloneMetaBaseBytes + 16*mib},
		{"57 TiB at 1 MiB", 57 * tib, mib, CloneMetaBaseBytes + 57*mib},
		{"block size 0", tib, 0, CloneMetaBaseBytes},
	}
	for _, tc := range cases {
		got := CloneMetaBytes(tc.copyBytes, tc.blockSize)
		if got != tc.want {
			t.Errorf("%s: CloneMetaBytes(%d, %d) = %d, want %d",
				tc.name, tc.copyBytes, tc.blockSize, got, tc.want)
		}
		// The units are these bytes in whole units, rounded up.
		units := CloneMetaUnits(tc.copyBytes, tc.blockSize)
		if units*CnCloneMetaUnit < got ||
			(units-1)*CnCloneMetaUnit >= got {
			t.Errorf("%s: CloneMetaUnits = %d units for %d bytes, want "+
				"the bytes rounded up to whole units of %d", tc.name,
				units, got, CnCloneMetaUnit)
		}
	}
}

// oldCnCloneMetaUnits is the cn agent's own arithmetic that CloneMetaUnits
// replaces: the regions of the clone's destination td at the pool's block
// size, none when the block size is zero, and the 4 MiB base plus one byte
// per region rounded up to whole CnCloneMetaUnit units.
func oldCnCloneMetaUnits(tdSize, blockSize uint64) uint64 {
	regionCnt := uint64(0)
	if blockSize != 0 {
		regionCnt = tdSize / blockSize
	}
	bytes := uint64(4*1024*1024) + regionCnt
	return (bytes + CnCloneMetaUnit - 1) / CnCloneMetaUnit
}

// oldDnCloneMetaUnits is the dn agent's own arithmetic that CloneMetaUnits
// replaces, which counts in 512-byte sectors (agent.SectorSize, which common
// cannot import): the side's whole sectors over the region's sectors, and the
// 4 MiB base plus one byte per region rounded up to whole DnCloneMetaUnit
// units. It also returns the side's sectors, and ok is false where the dn
// agent refuses the request: a block size under one sector.
func oldDnCloneMetaUnits(
	extCnt uint64,
	extentSize uint64,
	blockSize uint64,
) (units uint64, sectors uint64, ok bool) {
	const sectorSize = 512
	sectors = extCnt * extentSize / sectorSize
	regionSectors := blockSize / sectorSize
	if regionSectors == 0 {
		return 0, sectors, false
	}
	bytes := uint64(4*1024*1024) + sectors/regionSectors
	return (bytes + DnCloneMetaUnit - 1) / DnCloneMetaUnit, sectors, true
}

// TestCloneMetaUnitsMatchesTheAgents compares CloneMetaUnits with the two
// agents' arithmetic it replaces, over a grid of sizes.
//
// The cn arithmetic is the same function, so it must match at every block
// size, zero included. The dn arithmetic floors twice, the side to whole
// sectors and then the sectors to whole regions, so it must match wherever
// the block size is a whole number of sectors, as every stored pool's is. It
// must match whether the caller passes the side's bytes, as the gateway does,
// or the side's whole sectors in bytes, and at extent sizes that are not a
// whole number of sectors too, since the extent size is only bounds-checked.
func TestCloneMetaUnitsMatchesTheAgents(t *testing.T) {
	blockSizes := []uint64{
		512, 4 * kib, 64 * kib, 128 * kib, mib, 2 * mib, 64 * mib, gib,
		// Whole sectors that are no power of two.
		3 * 512, 1000 * 512,
	}
	// The cn arithmetic is also checked at block sizes the dn refuses or no
	// pool stores.
	cnBlockSizes := append(
		append([]uint64(nil), blockSizes...),
		0, 1, 511, 1000, 64*kib+1,
	)
	tdSizes := []uint64{
		0, 1, 511, 512, 64*kib - 1, 64 * kib, 64*kib + 1, mib - 1, mib,
		mib + 1, gib, 10 * gib, 10*gib + 12345, tib - 1, tib, 16 * tib,
		64 * tib, 256 * gib, 256*gib + 64*kib,
	}
	extentSizes := []uint64{
		64 * mib, gib, 4 * gib, tib,
		gib + 1, 100*mib + 7, 3 * 1000 * 1000 * 1000,
	}
	extCnts := []uint64{1, 2, 3, 7, 16, 100, 1000, 1024, 4096}

	compared := 0
	for _, blockSize := range cnBlockSizes {
		for _, tdSize := range tdSizes {
			got := CloneMetaUnits(tdSize, blockSize)
			if want := oldCnCloneMetaUnits(tdSize, blockSize); got != want {
				t.Errorf("cn: CloneMetaUnits(%d, %d) = %d, the cn "+
					"arithmetic gives %d", tdSize, blockSize, got, want)
			}
			compared++
		}
	}
	for _, blockSize := range blockSizes {
		for _, extentSize := range extentSizes {
			for _, extCnt := range extCnts {
				want, sectors, ok := oldDnCloneMetaUnits(
					extCnt, extentSize, blockSize,
				)
				if !ok {
					t.Fatalf("dn: block size %d refused", blockSize)
				}
				got := CloneMetaUnits(extCnt*extentSize, blockSize)
				if got != want {
					t.Errorf("dn: CloneMetaUnits(%d x %d, %d) = %d, the dn "+
						"arithmetic gives %d",
						extCnt, extentSize, blockSize, got, want)
				}
				got = CloneMetaUnits(sectors*512, blockSize)
				if got != want {
					t.Errorf("dn: CloneMetaUnits(%d sectors, %d) = %d, the "+
						"dn arithmetic gives %d",
						sectors, blockSize, got, want)
				}
				compared += 2
			}
		}
	}
	// A list emptied by an edit would let the comparison pass vacuously.
	want := len(cnBlockSizes)*len(tdSizes) +
		2*len(blockSizes)*len(extentSizes)*len(extCnts)
	if compared != want || want < 1000 {
		t.Fatalf("compared %d cases, want %d and at least 1000",
			compared, want)
	}
}

// TestCloneMetaUnitSizesAgree pins what lets one count serve both areas: the
// cn and the dn unit are one size, and the superblock budget is exactly one
// unit (architecture.md, Metadata units).
func TestCloneMetaUnitSizesAgree(t *testing.T) {
	if CnCloneMetaUnit != DnCloneMetaUnit {
		t.Fatalf("CnCloneMetaUnit %d != DnCloneMetaUnit %d: one unit count "+
			"no longer serves both metadata areas",
			CnCloneMetaUnit, DnCloneMetaUnit)
	}
	if CloneMetaBaseBytes != CnCloneMetaUnit {
		t.Errorf("CloneMetaBaseBytes %d is not one unit of %d",
			CloneMetaBaseBytes, CnCloneMetaUnit)
	}
}

// TestCloneMetaUnitCounts pins the two ceilings of architecture.md, Metadata
// units: each count of units times its unit is its whole area, so the
// ceiling counts no unit the area lacks and leaves no part of it uncounted.
func TestCloneMetaUnitCounts(t *testing.T) {
	if got := uint64(CnCloneMetaUnitCnt) * CnCloneMetaUnit; got !=
		CnCloneMetaAreaSize {
		t.Errorf("CnCloneMetaUnitCnt %d x CnCloneMetaUnit %d = %d, want "+
			"CnCloneMetaAreaSize %d", CnCloneMetaUnitCnt, CnCloneMetaUnit,
			got, CnCloneMetaAreaSize)
	}
	if got := uint64(DnCloneMetaUnitCnt) * DnCloneMetaUnit; got !=
		DnCloneMetaSize {
		t.Errorf("DnCloneMetaUnitCnt %d x DnCloneMetaUnit %d = %d, want "+
			"DnCloneMetaSize %d", DnCloneMetaUnitCnt, DnCloneMetaUnit, got,
			DnCloneMetaSize)
	}
}
