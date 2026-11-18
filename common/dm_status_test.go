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
		HydratingRead:    true,
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
		{"copies in flight",
			"0 20480 clone 8 1/1024 2048 4/10 3 0 0",
			with(func(st *CloneStatus) { st.Hydrating = 3 })},
		{"a count of copies in flight that does not parse",
			"0 20480 clone 8 1/1024 2048 4/10 x 0 0",
			with(func(st *CloneStatus) { st.HydratingRead = false })},
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

// TestParseThinPoolStatus pins the thin-pool line's four counts, its
// needs_check flag, its held metadata root and its ro mode word, with and
// without the device's name and range ahead of the target, and a failed
// pool's line, which does not parse.
func TestParseThinPoolStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want *ThinPoolStatus
	}{
		{"a bare line", "0 1024 thin-pool 3 12/4096 5/62 - rw " +
			"discard_passdown queue_if_no_space - 1024",
			&ThinPoolStatus{12, 4096, 5, 62, false, false, false}},
		{"a held root", "0 1024 thin-pool 3 12/4096 5/62 123 rw " +
			"discard_passdown queue_if_no_space - 1024",
			&ThinPoolStatus{12, 4096, 5, 62, false, true, false}},
		{"named, with needs_check", "pool: 0 1024 thin-pool 3 12/4096 5/62 " +
			"- ro discard_passdown queue_if_no_space needs_check 1024",
			&ThinPoolStatus{12, 4096, 5, 62, true, false, true}},
		{"read-only without needs_check, a held root",
			"0 1024 thin-pool 3 4096/4096 5/62 77 ro discard_passdown " +
				"queue_if_no_space - 1024",
			&ThinPoolStatus{4096, 4096, 5, 62, false, true, true}},
		{"out of data space", "0 1024 thin-pool 3 12/4096 62/62 - " +
			"out_of_data_space discard_passdown queue_if_no_space - 1024",
			&ThinPoolStatus{12, 4096, 62, 62, false, false, false}},
		{"no mode word", "0 1024 thin-pool 3 12/4096 5/62",
			&ThinPoolStatus{12, 4096, 5, 62, false, false, false}},
		{"a failed pool", "0 1024 thin-pool Fail", nil},
		{"a status read that failed", "0 1024 thin-pool Error", nil},
		{"no pool", "0 1024 linear 8:0 0", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseThinPoolStatus(tc.line)
			if tc.want == nil {
				if ok {
					t.Fatalf("parsed %+v, want no parse", got)
				}
				return
			}
			if !ok || *got != *tc.want {
				t.Fatalf("got %+v %v, want %+v", got, ok, tc.want)
			}
		})
	}
}

// TestThinPoolFault pins the two words dm-thin prints in place of a pool's
// numbers (pool_status): Fail for a pool it failed, Error for a status read
// that could not read the metadata; every other line has none.
func TestThinPoolFault(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"0 1024 thin-pool Fail", "Fail"},
		{"pool: 0 1024 thin-pool Error", "Error"},
		{"0 1024 thin-pool Fail\n", "Fail"},
		{"0 1024 thin-pool 3 12/4096 5/62 - rw discard_passdown " +
			"queue_if_no_space - 1024", ""},
		{"0 1024 thin-pool", ""},
		{"0 1024 clone Fail", ""},
		{"", ""},
	} {
		if got := ThinPoolFault(tc.line); got != tc.want {
			t.Errorf("ThinPoolFault(%q) = %q, want %q", tc.line, got,
				tc.want)
		}
	}
}

// TestThinMetaBlocks pins dm-thin's sizing of a metadata device: whole
// blocks of ThinMetaBlockSize, capped at ThinMetaMaxBlocks
// (get_metadata_dev_size_in_blocks).
func TestThinMetaBlocks(t *testing.T) {
	for _, tc := range []struct{ sectors, want uint64 }{
		{0, 0},
		{7, 0},
		{8, 1},
		{4096 * 8, 4096},
		{ThinMetaMaxBlocks * 8, ThinMetaMaxBlocks},
		{ThinMetaMaxBlocks*8 + 8, ThinMetaMaxBlocks},
		{1 << 40, ThinMetaMaxBlocks},
	} {
		if got := ThinMetaBlocks(tc.sectors); got != tc.want {
			t.Errorf("ThinMetaBlocks(%d) = %d, want %d", tc.sectors, got,
				tc.want)
		}
	}
}
