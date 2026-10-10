package common

// CloneMetaUnits is the metadata of one dm-clone copy in whole units of a
// metadata area (architecture.md, Metadata units): the superblock budget
// CloneMetaBaseBytes plus one byte per region, rounded up to whole units. A
// region is one pool block, so the regions are copyBytes over blockSize,
// rounded down; a copy smaller than one block, and any copy when blockSize is
// zero, needs one unit. One byte per region is a generous bound, since
// dm-clone's bitmap needs one bit per region.
//
// It is the one formula the gateway charges by and both agents size their
// runs by: copyBytes is the destination td's size for a clone and the moved
// side's bytes for a migration, and blockSize is the pool's data_block_size
// for both. One count serves both areas because CnCloneMetaUnit equals
// DnCloneMetaUnit, which TestCloneMetaUnitSizesAgree pins.
func CloneMetaUnits(copyBytes, blockSize uint64) uint64 {
	regions := uint64(0)
	if blockSize != 0 {
		regions = copyBytes / blockSize
	}
	bytes := CloneMetaBaseBytes + regions
	return (bytes + CnCloneMetaUnit - 1) / CnCloneMetaUnit
}
