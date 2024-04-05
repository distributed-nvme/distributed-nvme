package worker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
)

func initPrioCode(prioCodeConf string) (string, error) {
	prioCodeList := make([]string, constants.ShardCnt)
	prioCodeGroupList := strings.Split(prioCodeConf, ",")
	for _, prioCodeGroup := range prioCodeGroupList {
		if prioCodeGroup == "" {
			continue
		}
		prioCodeItems := strings.Split(prioCodeGroup, "-")
		if len(prioCodeItems) < 2 {
			return "", fmt.Errorf("Invalid prioCodeConf, miss item(s): %s", prioCodeGroup)
		}
		var prioCode string
		switch prioCodeItems[0] {
		case constants.ShardHighPrioText:
			prioCode = constants.ShardHighPrioCode
		case constants.ShardMediumPrioText:
			prioCode = constants.ShardMediumPrioCode
		case constants.ShardLowPrioText:
			prioCode = constants.ShardLowPrioCode
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
			s.WriteString(constants.ShardDefaultPrioCode)
		} else {
			s.WriteString(prioCode)
		}
	}
	return s.String(), nil
}
