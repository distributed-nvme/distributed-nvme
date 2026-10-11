package gateway

import "testing"

// TestHydrationComplete pins the proof DeleteClone and FinishMigration ask of
// a dm-clone's row before an unforced delete or finish (architecture.md,
// Clones): the details parse with common.ParseCloneStatus, the total is not
// zero, and every region is hydrated. Whatever does not parse is no proof, a
// count past 64 bits included, and neither is a total of zero; the row's own
// text ahead of the status line is not read as one.
func TestHydrationComplete(t *testing.T) {
	// status is the kernel's line of a dm-clone with the given
	// "<hydrated>/<total>".
	status := func(progress string) string {
		return "0 2097152 clone 8 72/32768 2048 " + progress + " 0 1 " +
			"no_discard_passdown 4 hydration_threshold 1 " +
			"hydration_batch_size 1 rw"
	}
	cases := []struct {
		name    string
		details string
		want    bool
	}{
		{"every region hydrated", status("1024/1024"), true},
		{"half the regions hydrated", status("256/512"), false},
		{"one region short", status("1023/1024"), false},
		{"a total of zero", status("0/0"), false},
		{"a total that does not parse", status("10/x"), false},
		{"a hydrated count that does not parse", status("x/10"), false},
		{"a hydrated count past 64 bits",
			status("99999999999999999999/1024"), false},
		{"both counts past 64 bits",
			status("99999999999999999999/99999999999999999999"), false},
		{"the row's text, then a complete line",
			"hydration not enabled\n" + status("512/512"), true},
		{"the row's text, then an incomplete line",
			"hydration not enabled\n" + status("511/512"), false},
		{"empty details", "", false},
		{"the row's text alone", "hydration not enabled", false},
		{"a linear line", "0 2048 linear 253:3 0", false},
		{"a failed dm-clone", "0 2048 clone Fail", false},
		// Two lines are no single dm-clone's status, even if each says
		// done: the proof reads one line or none.
		{"two complete dm-clone lines",
			status("512/512") + "\n" + status("512/512"), false},
	}
	for _, tc := range cases {
		if got := hydrationComplete(tc.details); got != tc.want {
			t.Errorf("%s: hydrationComplete(%q) = %v, want %v",
				tc.name, tc.details, got, tc.want)
		}
	}
}
