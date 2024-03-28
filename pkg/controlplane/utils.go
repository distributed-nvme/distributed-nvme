package controlplane

import (
	"fmt"

	"github.com/kelindar/bitmap"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

func extentInitCalc(
	size uint64,
	extentSizeShift, extentPerSetShift uint32,
) ([]byte, []uint32, uint32) {
	extentCntTotal := size >> extentSizeShift
	setCntFull := extentCntTotal >> extentPerSetShift
	setCntPartial := 0
	extentCntFull := 1 <<  extentPerSetShift
	extentCntPartial := extentCntTotal - (setCntFull << extentPerSetShift)
	if extentCntPartial > 0 {
		setCntPartial += 1
	}
	setCntTotal := setCntFull + uint64(setCntPartial)
	bitCntTotal := setCntTotal * (2 << extentPerSetShift)
	bitCntTotal = ((bitCntTotal + 63) >> 6) << 6
	byteCntTotal := bitCntTotal >> 3
	bmRaw := make([]byte, byteCntTotal)
	bm := bitmap.FromBytes(bmRaw)
	bm.Ones()
	for i := extentCntTotal; i < bitCntTotal; i++ {
		bm.Remove(uint32(i))
	}
	bucket := make([]uint32, setCntTotal)
	for i := 0; uint64(i) < setCntFull; i++ {
		bucket[i] = 1 << extentCntFull
	}
	if extentCntPartial > 0 {
		bucket[setCntTotal-1] = uint32(extentCntPartial)
	}
	return bmRaw, bucket, uint32(extentCntTotal)
}

func validStringLength(inpStr, name string) error {
	if len(inpStr) > lib.StringLengthMax {
		return fmt.Errorf("%s is longer than %d", name, lib.StringLengthMax)
	}
	if len(inpStr) < lib.StringLengthMin {
		return fmt.Errorf("%s is shorter than %d", name, lib.StringLengthMin)
	}
	return nil
}
