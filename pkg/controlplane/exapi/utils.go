package exapi

import (
	"fmt"

	"github.com/kelindar/bitmap"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

func validStringLength(inpStr, name string) error {
	if len(inpStr) > constants.StringLengthMax {
		return fmt.Errorf("%s is longer than %d", name, constants.StringLengthMax)
	}
	if len(inpStr) < constants.StringLengthMin {
		return fmt.Errorf("%s is shorter than %d", name, constants.StringLengthMin)
	}
	return nil
}

func extentInitCalc(
	size uint64,
	extentSizeShift, extentPerSetShift uint32,
) ([]byte, []uint32, uint32) {
	extentCntTotal := size >> extentSizeShift
	setCntFull := extentCntTotal >> extentPerSetShift
	setCntPartial := 0
	extentCntFull := 1 << extentPerSetShift
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
		bucket[i] = uint32(extentCntFull)
	}
	if extentCntPartial > 0 {
		maxContinueExtentCnt := uint64(1)
		for {
			if (maxContinueExtentCnt << 1) > extentCntPartial {
				break
			}
			maxContinueExtentCnt = maxContinueExtentCnt << 1
		}
		bucket[setCntTotal-1] = uint32(maxContinueExtentCnt)
	}
	return bmRaw, bucket, uint32(extentCntTotal)
}

func divRoundUp(a, b uint64) uint64 {
	return (a + b - 1) / b
}

func thinMetaExtentCntCalc(
	metaExtentSize uint64,
	dataExtentSize uint64,
	dataExtentCnt uint64,
	thinBlockSize uint64,
) uint64 {
	dataSize := dataExtentSize * dataExtentCnt
	// according to
	// https://docs.kernel.org/admin-guide/device-mapper/thin-provisioning.html
	// 48 * $data_dev_size / $data_block_size
	metaSize := divRoundUp(48*dataSize, thinBlockSize)
	metaExtentCnt := divRoundUp(metaSize, metaExtentSize)
	return metaExtentCnt
}

func allocateLd(
	extentConf *pbcp.ExtentConf,
	extentCnt uint64,
) (uint64, uint64, error) {
	return 0, 0, nil
}
