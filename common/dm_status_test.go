package common

import (
	"reflect"
	"testing"
)

// TestParseDmLines pins the line splitter both agents read `dmsetup table`
// with and ParseCloneStatus reads a status with: one target per line whose
// first two fields are unsigned numbers and which has a type, and nothing
// for any other line, so a row's own text never counts as a target.
func TestParseDmLines(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []DmTarget
	}{
		{"a linear line", "0 2048 linear 253:3 0\n",
			[]DmTarget{{0, 2048, "linear", []string{"253:3", "0"}}}},
		{"a target with no arguments", "0 2048 error",
			[]DmTarget{{0, 2048, "error", []string{}}}},
		{"a two-line table",
			"0 2048 linear 253:3 0\n2048 4096 linear 253:4 2048\n",
			[]DmTarget{
				{0, 2048, "linear", []string{"253:3", "0"}},
				{2048, 4096, "linear", []string{"253:4", "2048"}},
			}},
		{"a row's text ahead of a status line",
			"hydration not enabled\n0 20480 clone 8 1/1024",
			[]DmTarget{{0, 20480, "clone", []string{"8", "1/1024"}}}},
		{"garbage", "garbage\n", nil},
		{"empty output", "", nil},
		{"a start that is not a number", "x 2048 linear 253:3 0", nil},
		{"a length that is not a number", "0 x linear 253:3 0", nil},
		{"a negative start", "-1 2048 linear 253:3 0", nil},
		{"two numbers and no type", "0 2048", nil},
	}
	for _, tc := range cases {
		if got := ParseDmLines(tc.out); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ParseDmLines(%q) = %+v, want %+v",
				tc.name, tc.out, got, tc.want)
		}
	}
}

// TestParseCloneStatus pins the one Go reader of the dm-clone status line
// (architecture.md, Live-state reporting): which details parse, and what a
// line that parses reads as. Exactly one dm line, a dm-clone's with at least
// six arguments, parses; a count that does not parse, one past 64 bits
// included, reads as zero, which the gateway's rule that a total of zero is
// no proof of a finished copy depends on; and no count on the line panics
// the reader, which the gateway runs on an agent's reply.
func TestParseCloneStatus(t *testing.T) {
	// on is what the status of a dm-clone with hydration on and no knob in
	// its core arguments reads as, with 4 of 10 regions of 2048 sectors
	// hydrated.
	on := CloneStatus{
		RegionSectors:    2048,
		HydratedRegions:  4,
		TotalRegions:     10,
		HydrationEnabled: true,
	}
	with := func(edit func(st *CloneStatus)) *CloneStatus {
		st := on
		edit(&st)
		return &st
	}
	const head = "0 20480 clone 8 1/1024 2048 4/10 0 "
	cases := []struct {
		name string
		raw  string
		want *CloneStatus // nil: does not parse
	}{
		{"hydration off and both knobs",
			head + "1 no_hydration 4 hydration_threshold 3 " +
				"hydration_batch_size 5",
			with(func(st *CloneStatus) {
				st.HydrationEnabled = false
				st.Threshold, st.BatchSize = 3, 5
			})},
		{"hydration on",
			head + "0 4 hydration_threshold 1 hydration_batch_size 1",
			with(func(st *CloneStatus) {
				st.Threshold, st.BatchSize = 1, 1
			})},
		{"the kernel's line, its metadata mode after the core arguments",
			"0 2097152 clone 8 72/32768 2048 1024/1024 0 1 " +
				"no_discard_passdown 4 hydration_threshold 1 " +
				"hydration_batch_size 1 rw",
			with(func(st *CloneStatus) {
				st.HydratedRegions, st.TotalRegions = 1024, 1024
				st.Threshold, st.BatchSize = 1, 1
				st.MetadataMode = CloneMetadataReadWrite
			})},
		{"read-only metadata",
			head + "0 4 hydration_threshold 1 hydration_batch_size 1 ro",
			with(func(st *CloneStatus) {
				st.Threshold, st.BatchSize = 1, 1
				st.MetadataMode = CloneMetadataReadOnly
			})},
		{"a failed metadata mode after the lists",
			head + "1 no_hydration 0 Fail",
			with(func(st *CloneStatus) {
				st.HydrationEnabled = false
				st.MetadataMode = CloneMetadataFail
			})},
		{"a mode after an empty core list", head + "0 0 rw",
			with(func(st *CloneStatus) {
				st.MetadataMode = CloneMetadataReadWrite
			})},
		{"two arguments past the core list", head + "0 0 rw x", &on},
		{"no_hydration among other features",
			head + "2 no_discard_passdown no_hydration 0",
			with(func(st *CloneStatus) { st.HydrationEnabled = false })},
		{"a row's text ahead of the line",
			"hydration not enabled\n" + head + "1 no_hydration 0",
			with(func(st *CloneStatus) { st.HydrationEnabled = false })},
		{"exactly six arguments", head + "0", &on},
		{"a hydrated count that does not parse",
			"0 20480 clone 8 1/1024 2048 x/10 0 0",
			with(func(st *CloneStatus) { st.HydratedRegions = 0 })},
		{"a total that does not parse",
			"0 20480 clone 8 1/1024 2048 4/x 0 0",
			with(func(st *CloneStatus) { st.TotalRegions = 0 })},
		{"progress without a slash",
			"0 20480 clone 8 1/1024 2048 4 0 0",
			with(func(st *CloneStatus) {
				st.HydratedRegions, st.TotalRegions = 0, 0
			})},
		{"a region size that does not parse",
			"0 20480 clone 8 1/1024 x 4/10 0 0",
			with(func(st *CloneStatus) { st.RegionSectors = 0 })},
		// Past 64 bits strconv.ParseUint hands back the largest value
		// beside its error; the count still reads as zero.
		{"a hydrated count past 64 bits",
			"0 20480 clone 8 1/1024 2048 18446744073709551616/10 0 0",
			with(func(st *CloneStatus) { st.HydratedRegions = 0 })},
		{"a total past 64 bits",
			"0 20480 clone 8 1/1024 2048 4/99999999999999999999 0 0",
			with(func(st *CloneStatus) { st.TotalRegions = 0 })},
		{"a region size past 64 bits",
			"0 20480 clone 8 1/1024 99999999999999999999 4/10 0 0",
			with(func(st *CloneStatus) { st.RegionSectors = 0 })},
		{"a hydrated count at the top of 64 bits",
			"0 20480 clone 8 1/1024 2048 18446744073709551615/10 0 0",
			with(func(st *CloneStatus) {
				st.HydratedRegions = 18446744073709551615
			})},
		// A list whose count does not fit the line is not read, and the
		// knobs after it are not read either.
		{"a feature count past the line", head + "3 no_hydration", &on},
		{"a negative feature count", head + "-1 no_hydration", &on},
		{"a feature count that does not parse",
			head + "x 4 hydration_threshold 3 hydration_batch_size 5", &on},
		{"a feature count at the top of int",
			head + "9223372036854775807 no_hydration", &on},
		{"a feature count past int",
			head + "99999999999999999999 no_hydration", &on},
		{"a core count past the line",
			head + "0 4 hydration_threshold 3", &on},
		{"a negative core count", head + "0 -1 hydration_threshold 3", &on},
		{"a core count that does not parse",
			head + "0 x hydration_threshold 3", &on},
		{"a core count at the top of int",
			head + "0 9223372036854775807 hydration_threshold 3", &on},
		{"a knob without its value",
			head + "0 3 hydration_threshold 3 hydration_batch_size",
			with(func(st *CloneStatus) { st.Threshold = 3 })},
		{"a knob value past 32 bits",
			head + "0 4 hydration_threshold 4294967296 " +
				"hydration_batch_size 5",
			with(func(st *CloneStatus) { st.BatchSize = 5 })},
		{"a knob value that does not parse",
			head + "0 4 hydration_threshold x hydration_batch_size 5",
			with(func(st *CloneStatus) { st.BatchSize = 5 })},
		{"arguments past the core count",
			head + "0 2 hydration_threshold 3 hydration_batch_size 5",
			with(func(st *CloneStatus) { st.Threshold = 3 })},
		{"five arguments", head, nil},
		{"four arguments", "0 20480 clone 8 1/1024 2048 4/10", nil},
		{"a failed dm-clone", "0 2048 clone Fail", nil},
		{"a linear line", "0 2048 linear 253:3 0", nil},
		{"a thin-pool line with as many arguments",
			"0 2097152 thin-pool 0 77/4096 0/16384 - rw " +
				"no_discard_passdown queue_if_no_space - 1024", nil},
		{"two dm-clone lines", head + "0\n" + head + "0", nil},
		{"a linear line ahead of a dm-clone line",
			"0 2048 linear 253:3 0\n" + head + "0", nil},
		{"empty details", "", nil},
		{"a row's text alone", "hydration not enabled", nil},
	}
	for _, tc := range cases {
		got, ok := ParseCloneStatus(tc.raw)
		switch {
		case tc.want == nil && (ok || got != nil):
			t.Errorf("%s: %q parsed as %+v, want no parse",
				tc.name, tc.raw, got)
		case tc.want != nil && (!ok || got == nil):
			t.Errorf("%s: %q did not parse", tc.name, tc.raw)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("%s: %q = %+v, want %+v",
				tc.name, tc.raw, *got, *tc.want)
		}
	}
}

// TestCloneFailed pins the line of a dm-clone in fail mode, which carries
// the mode alone and which ParseCloneStatus does not parse.
func TestCloneFailed(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"0 2048 clone Fail", true},
		{"metadata failed\n0 2048 clone Fail", true},
		{"0 2048 clone 8 1/1024 2048 4/10 0 0 0 Fail", false},
		{"0 2048 clone Error", false},
		{"0 2048 linear Fail", false},
		{"0 2048 clone Fail\n0 2048 clone Fail", false},
		{"Fail", false},
		{"", false},
	} {
		if got := CloneFailed(tc.raw); got != tc.want {
			t.Errorf("CloneFailed(%q) = %v, want %v", tc.raw, got, tc.want)
		}
		if tc.want {
			if _, ok := ParseCloneStatus(tc.raw); ok {
				t.Errorf("ParseCloneStatus(%q) parsed a failed line", tc.raw)
			}
		}
	}
}
