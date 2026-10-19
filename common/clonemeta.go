package common

// CloneMetaBytes is the metadata of one dm-clone copy in bytes
// (architecture.md, Metadata units): the superblock budget CloneMetaBaseBytes
// plus one byte per region. A region is one pool block, so the regions are
// copyBytes over blockSize, rounded down; a copy smaller than one block, and
// any copy when blockSize is zero, counts no region. One byte per region is a
// generous bound, since dm-clone's bitmap needs one bit per region.
//
// It is the one budget formula of a copy's metadata, in bytes, so a caller
// that bounds the metadata by a space not counted in units compares it with
// that space directly; CloneMetaUnits rounds it up to whole units of a
// metadata area.
func CloneMetaBytes(copyBytes, blockSize uint64) uint64 {
	regions := uint64(0)
	if blockSize != 0 {
		regions = copyBytes / blockSize
	}
	return CloneMetaBaseBytes + regions
}

// CloneMetaUnits is the metadata of one dm-clone copy in whole units of a
// metadata area (architecture.md, Metadata units): CloneMetaBytes rounded up
// to whole units, so a copy smaller than one block, and any copy when
// blockSize is zero, needs one unit.
//
// It is the one formula the gateway charges by and both agents size their
// runs by: copyBytes is the destination td's size for a clone and the moved
// side's bytes for a migration, and blockSize is the pool's data_block_size
// for both. One count serves both areas because CnCloneMetaUnit equals
// DnCloneMetaUnit, which TestCloneMetaUnitSizesAgree pins.
func CloneMetaUnits(copyBytes, blockSize uint64) uint64 {
	bytes := CloneMetaBytes(copyBytes, blockSize)
	return (bytes + CnCloneMetaUnit - 1) / CnCloneMetaUnit
}
