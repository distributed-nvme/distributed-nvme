package controlplane

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kelindar/bitmap"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

func validStringLength(inpStr, name string) error {
	if len(inpStr) > lib.StringLengthMax {
		return fmt.Errorf("%s is longer than %d", name, lib.StringLengthMax)
	}
	if len(inpStr) < lib.StringLengthMin {
		return fmt.Errorf("%s is shorter than %d", name, lib.StringLengthMin)
	}
	return nil
}

func initPrioCode(prioCodeConf string) (string, error) {
	prioCodeList := make([]string, lib.ShardCnt)
	prioCodeGroupList := strings.Split(prioCodeConf, ",")
	for _,  prioCodeGroup := range prioCodeGroupList {
		if prioCodeGroup == "" {
			continue
		}
		prioCodeItems := strings.Split(prioCodeGroup, "-")
		if len(prioCodeItems) < 2 {
			return "", fmt.Errorf("Invalid prioCodeConf, miss item(s): %s", prioCodeGroup)
		}
		var prioCode string
		switch prioCodeItems[0] {
		case lib.ShardHighPrioText:
			prioCode = lib.ShardHighPrioCode
		case lib.ShardMediumPrioText:
			prioCode = lib.ShardMediumPrioCode
		case lib.ShardLowPrioText:
			prioCode = lib.ShardLowPrioCode
		default:
			return "", fmt.Errorf("Invalid priority: %v", prioCodeItems[0])
		}
		idx, err := strconv.ParseUint(prioCodeItems[1], 10, 32)
		if err != nil {
			return "", err
		}
		if len(prioCodeItems) == 2 {
			prioCodeList[idx] = prioCode
		} else if len(prioCodeItems) == 3 {
			endIdx, err := strconv.ParseUint(prioCodeItems[2], 10, 32)
			if err != nil {
				return "", err
			}
			for i := idx; i <= endIdx; i++ {
				prioCodeList[i] = prioCode
			}
		} else {
			return "", fmt.Errorf("Invalid prioCode, too many items: %s", prioCodeGroup)
		}
	}
	var s strings.Builder
	for _, prioCode := range prioCodeList {
		if prioCode == "" {
			s.WriteString(lib.ShardDefaultPrioCode)
		} else {
			s.WriteString(prioCode)
		}
	}
	return s.String(), nil
}

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
