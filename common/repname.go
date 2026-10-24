package common

import (
	"fmt"
	"strings"
)

// RepNameReservedPrefix starts the name of every repair the sp worker can
// create (WorkerRepName), and no other name (architecture.md, Common
// validation). The gateway refuses it in CreateRepair's rep_name, so no
// operator's repair carries it, and the calls that address a repair by its
// name take it, so an operator can still drive a worker's repair. The model
// tells the two kinds of repair apart by it (model.CreateRepair,
// model.FinishRepair).
const RepNameReservedPrefix = "__internal__"

// WorkerRepName is the name of the repair the sp worker creates for the group
// grpId of the pool spId at createdEpoch, its unix seconds:
// RepNameReservedPrefix, then each of the three numbers as sixteen lower-case
// hex digits, with an underscore between two numbers. The name stays within
// MaxStrSize and ValidStrPattern for every value, so it passes the common
// name rule the calls that address a repair by name apply. A group has one
// repair at a time, which makes the name unique; the time only tells a
// reader when the repair was made.
func WorkerRepName(spId uint64, grpId uint64, createdEpoch uint64) string {
	return fmt.Sprintf("%s%016x_%016x_%016x",
		RepNameReservedPrefix, spId, grpId, createdEpoch)
}

// HasRepNameReservedPrefix reports whether name starts with
// RepNameReservedPrefix: whether it is the name of a repair the sp worker
// created, which the gateway's create refuses.
func HasRepNameReservedPrefix(name string) bool {
	return strings.HasPrefix(name, RepNameReservedPrefix)
}
