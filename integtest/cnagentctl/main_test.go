package main

import (
	"strings"
	"testing"
)

// TestCloneSampleReadsTheMetadataMode pins the sample wait-hydrated takes
// from a clone's dm row: a count only from a dm-clone whose metadata is in
// read-write mode, no sample while the details hold no status line, and an
// error naming the mode for every other dm-clone (cnagent.md CN18).
func TestCloneSampleReadsTheMetadataMode(t *testing.T) {
	const lists = "0 1 no_discard_passdown 4 hydration_threshold 1 " +
		"hydration_batch_size 1"
	line := func(hydrated string, mode string) string {
		raw := "0 131072 clone 8 72/32768 2048 " + hydrated + " " + lists
		if mode != "" {
			raw += " " + mode
		}
		return raw
	}
	cases := []struct {
		name     string
		raw      string
		ok       bool
		hydrated uint64
		total    uint64
		err      string // a substring of the error; "" for none
	}{
		{"read-write", line("40/64", "rw"), true, 40, 64, ""},
		{"read-write behind a row's own text",
			"hydration not enabled\n" + line("32/64", "rw"),
			true, 32, 64, ""},
		{"no status line yet", "metadata group not ready", false, 0, 0, ""},
		{"no details at all", "", false, 0, 0, ""},
		{"read-only", line("40/64", "ro"), false, 0, 0, `"ro" mode`},
		{"fail mode", "0 131072 clone Fail", false, 0, 0, `"Fail" mode`},
		{"fail mode behind a row's own text",
			"metadata failed\n0 131072 clone Fail", false, 0, 0,
			`"Fail" mode`},
		{"no mode on the line", line("40/64", ""), false, 0, 0,
			"no metadata mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok, err := cloneSample(c.raw)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("cloneSample(%q) error %v, want one holding %q",
						c.raw, err, c.err)
				}
				if ok || status != nil {
					t.Fatalf("cloneSample(%q) = %v, %v beside its error",
						c.raw, status, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("cloneSample(%q): %v", c.raw, err)
			}
			if ok != c.ok {
				t.Fatalf("cloneSample(%q) ok %v, want %v", c.raw, ok, c.ok)
			}
			if !ok {
				return
			}
			if status.HydratedRegions != c.hydrated ||
				status.TotalRegions != c.total {
				t.Fatalf("cloneSample(%q) = %d/%d, want %d/%d", c.raw,
					status.HydratedRegions, status.TotalRegions,
					c.hydrated, c.total)
			}
		})
	}
}
