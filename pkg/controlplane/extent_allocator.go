package controlplane

import (
	"github.com/kelindar/bitmap"
)

func extentInitCalc(
	size uint64,
	extentSizeShift, extentPerSetShift uint32,
) ([]byte, []uint32, uint32, error) {
	extentTotal := size >> extentSizeShift
	setCnt := extentTotal >> extentPerSetShift
	if (setCnt << extentPerSetShift) < extentTotal {
		setCnt += 1
	}
	bitCntTotal := setCnt * (2 << extentPerSetShift)
	bitCntTotal = ((bitCntTotal + 63) >> 6) << 6
	byteCntTotal := bitCntTotal >> 3
	bmRaw := make([]byte, byteCntTotal)
	bm := bitmap.FromBytes(bmRaw)
	bm.Ones()
	for i := extentTotal; i < bitCntTotal; i++ {
		bm.Remove(uint32(i))
	}
	return bmRaw, nil, 0, nil
}
