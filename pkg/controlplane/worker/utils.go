package worker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

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
