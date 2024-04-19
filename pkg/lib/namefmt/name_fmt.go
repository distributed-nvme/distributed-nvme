package namefmt

import (
	"fmt"
)

type NameFmt struct {
	prefix string
}

const (
	devTypeLd = "0000"
)


func NewNameFmt(prefix string) *NameFmt {
	return &NameFmt{
		prefix: prefix,
	}
}
