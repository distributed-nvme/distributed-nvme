package common

import (
	"math"
	"regexp"
	"testing"
)

// TestWorkerRepName pins the shape of the sp worker's repair name: the
// reserved prefix, then the pool id, the group id and the creation time as
// sixteen lower-case hex digits each, underscores between; within the common
// name rule (MaxStrSize, ValidStrPattern) at the smallest and the largest
// values; and carrying the prefix by HasRepNameReservedPrefix.
func TestWorkerRepName(t *testing.T) {
	if got, want := WorkerRepName(0x100, 0x1ab, 0x6539c4f0),
		"__internal__0000000000000100_00000000000001ab_000000006539c4f0"; got != want {
		t.Errorf("WorkerRepName: got %q, want %q", got, want)
	}
	valid := regexp.MustCompile(ValidStrPattern)
	for _, ids := range [][3]uint64{
		{0, 0, 0},
		{math.MaxUint64, math.MaxUint64, math.MaxUint64},
		{1, math.MaxUint64, 0},
	} {
		name := WorkerRepName(ids[0], ids[1], ids[2])
		if len(name) > MaxStrSize {
			t.Errorf("%q is %d bytes, over MaxStrSize %d", name, len(name),
				MaxStrSize)
		}
		if !valid.MatchString(name) {
			t.Errorf("%q does not match ValidStrPattern %s", name,
				ValidStrPattern)
		}
		if !HasRepNameReservedPrefix(name) {
			t.Errorf("%q does not carry the reserved prefix", name)
		}
	}
	if WorkerRepName(1, 2, 3) == WorkerRepName(1, 3, 2) {
		t.Errorf("the group id and the time are not told apart")
	}
}

// TestHasRepNameReservedPrefix: the prefix counts only at the start of a
// name, exactly as spelled; the prefix alone carries it.
func TestHasRepNameReservedPrefix(t *testing.T) {
	for name, want := range map[string]bool{
		"__internal__":      true,
		"__internal__x":     true,
		"rep-a":             false,
		"":                  false,
		"_internal__x":      false,
		"__internal_x":      false,
		"__INTERNAL__x":     false,
		"x__internal__":     false,
		" __internal__x":    false,
		"__internal":        false,
		"rep__internal__-a": false,
	} {
		if got := HasRepNameReservedPrefix(name); got != want {
			t.Errorf("HasRepNameReservedPrefix(%q) = %v, want %v",
				name, got, want)
		}
	}
}
