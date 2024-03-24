package controlplane

import (
	"fmt"
)
type cpStmError struct {
	code uint32
	msg  string
}

func (e *cpStmError) Error() string {
	return fmt.Sprintf("code: %d msg: %s", e.code, e.msg)
}
