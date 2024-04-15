package worker

import (
	"fmt"
)

func idToStr(resId uint64) string {
	return fmt.Sprintf("%016x", resId)
}
