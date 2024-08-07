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
	if (size >> extentSizeShift) == 0 {
		size = 1 << extentSizeShift
	}
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
	for i := extentCntTotal; i < bitCntTotal; i++ {
		bm.Set(uint32(i))
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
	metaExtentSize uint32,
	dataExtentSize uint32,
	dataExtentCnt uint32,
	thinBlockSize uint32,
) uint32 {
	dataSize := uint64(dataExtentSize) * uint64(dataExtentCnt)
	// according to
	// https://docs.kernel.org/admin-guide/device-mapper/thin-provisioning.html
	// 48 * $data_dev_size / $data_block_size
	metaSize := divRoundUp(48*dataSize, uint64(thinBlockSize))
	metaExtentCnt := divRoundUp(metaSize, uint64(metaExtentSize))
	return uint32(metaExtentCnt)
}

type buddyNode struct {
	start  uint32
	stop   uint32
	length uint32
}

func getMaxCnt(bm *bitmap.Bitmap, start uint32, size uint32) uint32 {
	nodeQueue := make([]*buddyNode, size)
	for i := start; i < size; i++ {
		var length uint32
		if bm.Contains(start) {
			length = 1
		} else {
			length = 0
		}
		node := &buddyNode{
			start:  i,
			stop:   i,
			length: length,
		}
		nodeQueue[i] = node
	}
	for len(nodeQueue) > 1 {
		tmpQueue := make([]*buddyNode, 0)
		for i := 0; i < len(nodeQueue); i += 2 {
			left := nodeQueue[i]
			right := nodeQueue[i+1]
			leftFull := (left.stop-left.start+1 == left.length)
			rightFull := (right.stop-right.start+1 == right.length)
			var length uint32
			if leftFull && rightFull {
				length = left.length + right.length
			} else {
				length = left.length
				if right.length > length {
					length = right.length
				}
			}
			node := &buddyNode{
				start:  left.start,
				stop:   right.stop,
				length: length,
			}
			tmpQueue = append(tmpQueue, node)
		}
		nodeQueue = tmpQueue
	}
	return nodeQueue[0].length
}

func allocateLd(
	extentConf *pbcp.ExtentConf,
	extentCnt uint32,
	extentSetSize uint32,
) (uint32, uint32, error) {
	extentCntShift := 0
	for {
		if (1 << extentCntShift) >= extentCnt {
			break
		}
		extentCntShift++
	}
	extentCnt = 1 << extentCntShift

	targetIdx := constants.Uint32Max
	targetCnt := constants.Uint32Max
	for idx, cnt := range extentConf.ExtentSetBucket {
		if cnt >= extentCnt && cnt < targetCnt {
			targetCnt = cnt
			targetIdx = uint32(idx)
		}
	}
	if targetIdx == constants.Uint32Max {
		return 0, 0, fmt.Errorf("No enough capacity")
	}
	start := targetIdx * extentSetSize
	stop := start + extentSetSize
	bm := bitmap.FromBytes(extentConf.Bitmap)
	startBit := constants.Uint32Max
	for i := start; i < stop-extentCnt; i += extentCnt {
		allZero := true
		for j := i; j < extentCnt; j++ {
			if bm.Contains(j) {
				allZero = false
				break
			}
		}
		if allZero {
			startBit = i
			break
		}
	}
	if startBit == constants.Uint32Max {
		return 0, 0, fmt.Errorf("No enough capacity")
	}
	for i := startBit; i < extentCnt; i++ {
		bm.Set(i)
	}
	maxCnt := getMaxCnt(&bm, startBit, extentSetSize)
	extentConf.ExtentSetBucket[targetIdx] = maxCnt
	return startBit, extentCnt, nil
}

func freeLd(
	extentConf *pbcp.ExtentConf,
	start uint32,
	cnt uint32,
	extentSetSize uint32,
) {
	targetIdx := start / extentSetSize
	bm := bitmap.FromBytes(extentConf.Bitmap)
	for i := start; i < cnt; i++ {
		bm.Remove(i)
	}
	maxCnt := getMaxCnt(&bm, start, extentSetSize)
	extentConf.ExtentSetBucket[targetIdx] = maxCnt
}

func initNextBit(bitSize uint32) *pbcp.NextBit {
	if bitSize%64 != 0 {
		panic("Bitmap size not align to 64 bits")
	}
	return &pbcp.NextBit{
		CurrIdx: 0,
		Bitmap:  make([]byte, bitSize/8),
	}
}

func updateNextBit(nextBit *pbcp.NextBit, idx uint32) uint32 {
	bitSize := uint32(len(nextBit.Bitmap)) * 8
	bm := bitmap.FromBytes(nextBit.Bitmap)
	bm.Set(idx)
	nextIdx := idx + 1
	nextIdx = nextIdx % bitSize
	nextBit.CurrIdx = nextIdx
	return idx
}

func getAndUpdateNextBit(nextBit *pbcp.NextBit) (uint32, error) {
	bm := bitmap.FromBytes(nextBit.Bitmap)
	bitSize := uint32(len(nextBit.Bitmap)) * 8
	for i := nextBit.CurrIdx; i < bitSize; i++ {
		if !bm.Contains(i) {
			return updateNextBit(nextBit, i), nil
		}
	}
	for i := uint32(0); i < nextBit.CurrIdx; i++ {
		if !bm.Contains(i) {
			return updateNextBit(nextBit, i), nil
		}
	}
	return uint32(0), fmt.Errorf("No available bit")
}

func clearNextBit(nextBit *pbcp.NextBit, idx uint32) {
	bm := bitmap.FromBytes(nextBit.Bitmap)
	bm.Remove(idx)
}
