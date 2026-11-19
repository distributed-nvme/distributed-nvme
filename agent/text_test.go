package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file pins the row text cap of architecture.md, Live-state reporting:
// every text an agent writes into a row or beside one is at most
// common.AgentTextMax bytes, cut in its middle so its head, what failed,
// and its end, the cause, stay, with "..." between, never splitting a UTF-8
// sequence. The leftover answer is bounded by its own construction instead,
// its head and names kept.

// wantCut asserts got is text cut to the cap: within it, a head of text and
// text's end around the cut's mark, most of the cap kept, and valid UTF-8
// when text is.
func wantCut(t *testing.T, what string, got string, text string) {
	t.Helper()
	if len(got) > common.AgentTextMax {
		t.Errorf("%s is %d bytes, over the cap of %d", what, len(got),
			common.AgentTextMax)
	}
	head, tail, ok := strings.Cut(got, "...")
	if !ok || !strings.HasPrefix(text, head) ||
		!strings.HasSuffix(text, tail) || len(head) < 120 ||
		len(got) < common.AgentTextMax-6 {
		t.Errorf("%s is %q, want the text's head and end around the "+
			"cut's mark in about %d bytes", what, got, common.AgentTextMax)
	}
	if utf8.ValidString(text) && !utf8.ValidString(got) {
		t.Errorf("%s splits a UTF-8 sequence: %q", what, got)
	}
}

// TestResTrackerCutsTheRowText pins the cut at the one writer of every row
// (ResTracker.SetReason, which Set, Ok, Err, FromErr and the rest call): a
// failed command's text over the cap is recorded and returned cut, keeping
// the command's name at its head and the cause at its end; a text at the
// cap is kept whole; a cut never splits a multi-byte character.
func TestResTrackerCutsTheRowText(t *testing.T) {
	cause := ": exit status 1: device-mapper: reload ioctl on dnv-x " +
		"failed: Invalid argument"
	long := "dmsetup create dnv-x --table " +
		strings.Repeat("0 8 linear 253:4 0 ", 60) + cause
	tr := NewResTracker()
	row := tr.FromErr("k", "dnv-x", "", errors.New(long))
	wantCut(t, "the returned row", row.GetDetails(), long)
	if !strings.HasSuffix(row.GetDetails(), cause) ||
		!strings.HasPrefix(row.GetDetails(), "dmsetup create dnv-x ") {
		t.Errorf("the row %q lost the command at the text's head or the "+
			"cause at its end", row.GetDetails())
	}
	again := tr.Err("k", "dnv-x", long)
	if again.GetDetails() != row.GetDetails() {
		t.Errorf("the recorded row %q differs from the returned %q",
			again.GetDetails(), row.GetDetails())
	}
	atCap := strings.Repeat("a", common.AgentTextMax)
	if got := tr.Ok("k2", "n", atCap).GetDetails(); got != atCap {
		t.Errorf("a text at the cap became %q", got)
	}
	wide := strings.Repeat("é", common.AgentTextMax)
	wantCut(t, "a text of two-byte characters",
		tr.Set("k3", "n", pb.ResStatus_RES_STATUS_ERROR, wide).GetDetails(),
		wide)
}

// TestRepliesCutTheirText pins the cut of the agent replies whose details
// carry an error or a caller's text: the failed save, the refused conf and
// the unknown object.
func TestRepliesCutTheirText(t *testing.T) {
	long := strings.Repeat("/var/lib/dnv/x", 40) + ": no space left on device"
	save := SaveFailedReply(errors.New(long))
	wantCut(t, "the save-failed reply", save.GetDetails(),
		"saving the request failed: "+long)
	conf := InvalidConfReply("side_conf.zero_bytes: %s", long)
	wantCut(t, "the invalid-conf reply", conf.GetDetails(),
		"side_conf.zero_bytes: "+long)
	unknown := UnknownObjectReply("no side %s", long)
	wantCut(t, "the unknown-object reply", unknown.GetDetails(),
		"no side "+long)
}

// TestLeftoverReplyIsBoundedByItsConstruction pins the leftover answer's own
// bound: its head, "leftover(N): ", which the cn suite reads, and the names
// stay whole; at most maxLeftoverNames names and as many failure lines are
// shown, each line naming what failed whole and its error cut to the cap,
// the rest counted in a tail. The agent's log record keeps every failure
// whole.
func TestLeftoverReplyIsBoundedByItsConstruction(t *testing.T) {
	var res SweepResult
	for idx := 0; idx < maxLeftoverNames+2; idx++ {
		res.Add(LeftoverKindDm, fmt.Sprintf("dnv-%02d", idx))
	}
	cause := strings.Repeat("output ", 80) + "the cause"
	for idx := 0; idx < maxLeftoverNames+3; idx++ {
		res.Fail(fmt.Sprintf("dm table of dnv-f%02d", idx),
			errors.New(cause))
	}
	details := res.Reply().GetDetails()
	head := fmt.Sprintf("leftover(%d): dm:dnv-00, dm:dnv-01",
		maxLeftoverNames+2)
	if !strings.HasPrefix(details, head) {
		t.Fatalf("the answer %q does not begin %q", details, head)
	}
	if !strings.Contains(details, "dm:dnv-07 [+2 more]") ||
		strings.Contains(details, "dm:dnv-08") {
		t.Errorf("the answer %q does not show the first %d names and "+
			"count the rest", details, maxLeftoverNames)
	}
	lines := strings.Split(details, "; ")
	failures := lines[1:]
	if len(failures) != maxLeftoverNames+1 {
		t.Fatalf("the answer holds %d failure parts, want %d lines and a "+
			"tail: %q", len(failures), maxLeftoverNames+1, details)
	}
	for idx, line := range failures[:maxLeftoverNames] {
		prefix := fmt.Sprintf("enumeration failed: dm table of "+
			"dnv-f%02d: ", idx)
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("failure line %d %q does not name what failed whole",
				idx, line)
			continue
		}
		wantCut(t, fmt.Sprintf("failure line %d's error", idx),
			strings.TrimPrefix(line, prefix), cause)
	}
	if tail := failures[maxLeftoverNames]; tail != "[+3 more failures]" {
		t.Errorf("the failure tail is %q, want the three not shown counted",
			tail)
	}
}

// TestJoinNoteKeepsTheLineAndTheNotesEnd pins the dm-clone row's cut
// (CloneStatusDetails, JoinNote): a converge's failure in front of the raw
// status line is cut to the room the line leaves, its head and its cause
// kept, so the row fits the cap whole and the line every reader parses
// stays whole; a note that fits is kept whole.
func TestJoinNoteKeepsTheLineAndTheNotesEnd(t *testing.T) {
	raw := "0 131072 clone 8 1/1024 2048 0/64 0 2 no_hydration " +
		"no_discard_passdown 4 hydration_threshold 1 hydration_batch_size 1 rw"
	note := "dmsetup message dnv-" + strings.Repeat("0", 80) +
		" 0 enable_hydration: device-mapper: message ioctl failed: " +
		"Invalid argument\nhydration not enabled"
	got := CloneStatusDetails(note, raw)
	if len(got) > common.AgentTextMax || !strings.HasSuffix(got, "\n"+raw) {
		t.Fatalf("the row is %q (%d bytes), want the whole line at its end "+
			"within %d bytes", got, len(got), common.AgentTextMax)
	}
	cut := strings.TrimSuffix(got, "\n"+raw)
	head, tail, ok := strings.Cut(cut, "...")
	if !ok || !strings.HasPrefix(note, head) ||
		!strings.HasSuffix(note, tail) ||
		!strings.HasSuffix(tail, "Invalid argument\nhydration not enabled") {
		t.Errorf("the note is %q, want its head and its cause", cut)
	}
	if row := NewResTracker().Err("k", "n", got).GetDetails(); row != got {
		t.Errorf("the row's writer cut it again: %q", row)
	}
	short := "hydration not enabled"
	if got := CloneStatusDetails(short, raw); got != short+"\n"+raw {
		t.Errorf("a short note became %q", got)
	}
}
