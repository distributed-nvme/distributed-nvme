package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// applyNode is a scripted device mapper for ApplySkipRanges: a dm-clone
// "clone" (253:9) and its holders, each with a live table and an attr
// column, the dm-clone's copy switch and the copies-in-flight counts its
// status reads give one after another. Every command and every discard is
// recorded in order, and a command line can be made to fail.
type applyNode struct {
	devs      map[string]*applyDev
	hydration bool
	// inFlight are the copy counts successive status reads show; the last
	// one repeats. A value below zero prints an unparsable count.
	inFlight []int
	metaMode string
	// partial makes the whole-node table listing exit 1 after its records,
	// as dmsetup does when a device goes away during the listing.
	partial bool
	// inflight is what the dm-clone's sysfs inflight file reads; "" reads
	// as no IO in flight.
	inflight string
	fail     map[string]bool
	log      []string
}

type applyDev struct {
	minor     int
	table     string
	suspended bool
	waiting   bool
}

func newApplyNode() *applyNode {
	return &applyNode{
		devs: map[string]*applyDev{
			"meta": {minor: 1, table: "0 64 linear 259:1 0"},
			"dest": {minor: 2, table: "0 2048 linear 259:2 0"},
			"clone": {minor: 9, table: "0 2048 clone 253:1 253:2 259:0 8 " +
				"1 no_discard_passdown 4 hydration_threshold 1 " +
				"hydration_batch_size 1"},
			"lin-a": {minor: 10, table: "0 2048 linear 253:9 0"},
			"lin-b": {minor: 11, table: "0 2048 linear 253:9 0"},
			"other": {minor: 12, table: "0 2048 linear 253:2 0"},
		},
		hydration: true,
		inFlight:  []int{0},
		fail:      map[string]bool{},
	}
}

func (n *applyNode) attr(d *applyDev) string {
	attr := []byte("L--w")
	if d.waiting {
		attr[1] = 'I'
	}
	if d.suspended {
		attr[2] = 's'
	}
	return string(attr)
}

func (n *applyNode) status() string {
	count := n.inFlight[0]
	if len(n.inFlight) > 1 {
		n.inFlight = n.inFlight[1:]
	}
	countText := fmt.Sprint(count)
	if count < 0 {
		countText = "x"
	}
	features := "1 no_discard_passdown"
	if !n.hydration {
		features = "2 no_hydration no_discard_passdown"
	}
	mode := ""
	if n.metaMode != "" {
		mode = " " + n.metaMode
	}
	return fmt.Sprintf("0 2048 clone 8 1/1024 8 0/256 %s %s 4 "+
		"hydration_threshold 1 hydration_batch_size 1%s", countText,
		features, mode)
}

func (n *applyNode) oc() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			line := name + " " + strings.Join(args, " ")
			if n.fail[line] {
				n.log = append(n.log, "FAIL "+line)
				return "", "refused", 1, errors.New("exit status 1")
			}
			out, code := n.dmsetup(args)
			if n.partial && slices.Contains(args, "--concise") {
				return out, "No such device", 1, errors.New("exit status 1")
			}
			if code != 0 {
				return "", "Device does not exist", code,
					errors.New("exit status 1")
			}
			return out, "", 0, nil
		},
		ReadFileFn: func(ctx context.Context, path string) (string, error) {
			if path != "/sys/dev/block/253:9/inflight" {
				return "", fmt.Errorf("read of %s", path)
			}
			if n.inflight == "" {
				return "       0        0\n", nil
			}
			return n.inflight, nil
		},
		DiscardRangesFn: func(
			ctx context.Context, path string, ranges []common.DiscardRange,
		) (int, error) {
			n.log = append(n.log, fmt.Sprintf("discard %s %v", path, ranges))
			return len(ranges), nil
		},
	}
}

// applyNames is the order the stand-in lists its devices in.
var applyNames = []string{"meta", "dest", "clone", "lin-a", "lin-b", "other"}

func (n *applyNode) dmsetup(args []string) (string, int) {
	last := args[len(args)-1]
	switch {
	case args[0] == "info" && last == "name,major,minor,attr":
		var lines []string
		for _, name := range applyNames {
			if d, ok := n.devs[name]; ok {
				lines = append(lines, fmt.Sprintf("%s/253/%d/%s",
					name, d.minor, n.attr(d)))
			}
		}
		return strings.Join(lines, "\n") + "\n", 0
	case args[0] == "info" && slices.Contains(args, "name,major,minor,attr"):
		d, ok := n.devs[last]
		if !ok {
			return "", 1
		}
		return fmt.Sprintf("%s/253/%d/%s\n", last, d.minor, n.attr(d)), 0
	case args[0] == "info":
		d, ok := n.devs[last]
		if !ok {
			return "", 1
		}
		return n.attr(d) + "\n", 0
	case args[0] == "table" && last == "--concise":
		var records []string
		for _, name := range applyNames {
			if d, ok := n.devs[name]; ok {
				records = append(records, fmt.Sprintf(
					"%s,,%d,rw,%s", name, d.minor, d.table))
			}
		}
		return strings.Join(records, ";") + "\n", 0
	case args[0] == "suspend":
		n.log = append(n.log, "suspend "+last)
		n.devs[last].suspended = true
	case args[0] == "clear":
		n.devs[last].waiting = false
	case args[0] == "resume":
		n.log = append(n.log, "resume "+last)
		n.devs[last].suspended = false
	case args[0] == "status":
		return n.status() + "\n", 0
	case args[0] == "message":
		message := args[len(args)-1]
		n.log = append(n.log, message)
		n.hydration = message == "enable_hydration"
	}
	return "", 0
}

var applyRanges = []SkipRange{{Offset: 0, Length: 4096},
	{Offset: 8192, Length: 4096}}

// withFastDrain shortens the wait for the copies in flight for one test.
func withFastDrain(t *testing.T) {
	t.Helper()
	bound, poll := cloneApplyDrainBound, cloneApplyPollInterval
	cloneApplyDrainBound = 50 * time.Millisecond
	cloneApplyPollInterval = time.Millisecond
	t.Cleanup(func() {
		cloneApplyDrainBound, cloneApplyPollInterval = bound, poll
	})
}

// TestApplySkipRangesPausesTheHoldersAndTheCopy: on a dm-clone that serves,
// the background copy is stopped and its copies in flight waited out, every
// holder is paused, the ranges are discarded in one call, and then the
// holders are resumed, the last paused first, and the copy started; a device
// that does not map the dm-clone is never touched.
func TestApplySkipRangesPausesTheHoldersAndTheCopy(t *testing.T) {
	withFastDrain(t)
	n := newApplyNode()
	n.inFlight = []int{2, 1, 0}
	if err := ApplySkipRanges(context.Background(), NewDm(n.oc()),
		"clone", "/dev/mapper/clone", applyRanges); err != nil {
		t.Fatalf("ApplySkipRanges: %v", err)
	}
	want := []string{
		"disable_hydration",
		"suspend lin-a",
		"suspend lin-b",
		"discard /dev/mapper/clone [{0 4096} {8192 4096}]",
		"resume lin-b",
		"resume lin-a",
		"enable_hydration",
	}
	if !slices.Equal(n.log, want) {
		t.Fatalf("steps %q, want %q", n.log, want)
	}
	if !n.hydration || n.devs["lin-a"].suspended || n.devs["lin-b"].suspended {
		t.Fatalf("not restored: copy %v, holders %v %v", n.hydration,
			n.devs["lin-a"].suspended, n.devs["lin-b"].suspended)
	}
}

// TestApplySkipRangesKeepsWhatItFound: a holder already suspended is left
// suspended, and a copy that was off stays off.
func TestApplySkipRangesKeepsWhatItFound(t *testing.T) {
	withFastDrain(t)
	n := newApplyNode()
	n.devs["lin-a"].suspended = true
	n.hydration = false
	if err := ApplySkipRanges(context.Background(), NewDm(n.oc()),
		"clone", "/dev/mapper/clone", applyRanges); err != nil {
		t.Fatalf("ApplySkipRanges: %v", err)
	}
	want := []string{
		"suspend lin-b",
		"discard /dev/mapper/clone [{0 4096} {8192 4096}]",
		"resume lin-b",
	}
	if !slices.Equal(n.log, want) {
		t.Fatalf("steps %q, want %q", n.log, want)
	}
	if !n.devs["lin-a"].suspended || n.hydration {
		t.Fatal("the apply changed what it found")
	}
}

// TestApplySkipRangesSendsNothingToAnAbsentOrSuspendedClone: a dm-clone that
// is not there, or is suspended, gets no step at all, and the apply answers
// no error (item 1: a discard on a suspended device waits for its resume).
func TestApplySkipRangesSendsNothingToAnAbsentOrSuspendedClone(t *testing.T) {
	withFastDrain(t)
	n := newApplyNode()
	n.devs["clone"].suspended = true
	if err := ApplySkipRanges(context.Background(), NewDm(n.oc()),
		"clone", "/dev/mapper/clone", applyRanges); err != nil ||
		len(n.log) != 0 {
		t.Fatalf("suspended dm-clone: %v, steps %q, want none", err, n.log)
	}
	delete(n.devs, "clone")
	if err := ApplySkipRanges(context.Background(), NewDm(n.oc()),
		"clone", "/dev/mapper/clone", applyRanges); err != nil ||
		len(n.log) != 0 {
		t.Fatalf("absent dm-clone: %v, steps %q, want none", err, n.log)
	}
	n = newApplyNode()
	if err := ApplySkipRanges(context.Background(), NewDm(n.oc()),
		"clone", "/dev/mapper/clone", nil); err != nil || len(n.log) != 0 {
		t.Fatalf("no ranges: %v, steps %q, want none", err, n.log)
	}
}

// TestApplySkipRangesRefusesWhatItCannotMakeSure: a holder with a table
// waiting, a pause that fails, a stop that fails, a count that does not
// parse, copies still in flight at the bound and metadata out of read-write
// mode each send no discard, and everything the apply changed is restored.
func TestApplySkipRangesRefusesWhatItCannotMakeSure(t *testing.T) {
	withFastDrain(t)
	cases := []struct {
		name  string
		setup func(n *applyNode)
		want  []string
	}{
		{
			name:  "waiting table",
			setup: func(n *applyNode) { n.devs["lin-b"].waiting = true },
			want: []string{"disable_hydration", "suspend lin-a",
				"resume lin-a", "enable_hydration"},
		},
		{
			name: "failed pause",
			setup: func(n *applyNode) {
				n.fail["dmsetup suspend lin-b"] = true
			},
			want: []string{"disable_hydration", "suspend lin-a",
				"FAIL dmsetup suspend lin-b", "resume lin-b", "resume lin-a",
				"enable_hydration"},
		},
		{
			name: "failed stop",
			setup: func(n *applyNode) {
				n.fail["dmsetup message clone 0 disable_hydration"] = true
			},
			want: []string{
				"FAIL dmsetup message clone 0 disable_hydration",
				"enable_hydration"},
		},
		{
			name:  "unparsable count",
			setup: func(n *applyNode) { n.inFlight = []int{-1} },
			want: []string{"disable_hydration", "suspend lin-a",
				"suspend lin-b", "resume lin-b", "resume lin-a",
				"enable_hydration"},
		},
		{
			// The first wait goes on past its bound to the pause; the
			// second, with the holders paused, refuses past its own.
			name:  "copies past both bounds",
			setup: func(n *applyNode) { n.inFlight = []int{3} },
			want: []string{"disable_hydration", "suspend lin-a",
				"suspend lin-b", "resume lin-b", "resume lin-a",
				"enable_hydration"},
		},
		{
			// The second read, once the holders are paused, still shows a
			// copy in flight past the bound.
			name: "a copy in flight once the holders are paused",
			setup: func(n *applyNode) {
				n.inFlight = []int{0, 0, 2}
			},
			want: []string{"disable_hydration", "suspend lin-a",
				"suspend lin-b", "resume lin-b", "resume lin-a",
				"enable_hydration"},
		},
		{
			name:  "partial listing",
			setup: func(n *applyNode) { n.partial = true },
			want:  nil,
		},
		{
			name:  "a suspended device below",
			setup: func(n *applyNode) { n.devs["dest"].suspended = true },
			want:  nil,
		},
		{
			// A udev probe's read of the dm-clone node itself: IO the pause
			// does not hold.
			name:  "IO in flight past the pause",
			setup: func(n *applyNode) { n.inflight = "1 0\n" },
			want: []string{"disable_hydration", "suspend lin-a",
				"suspend lin-b", "resume lin-b", "resume lin-a",
				"enable_hydration"},
		},
		{
			name:  "read-only metadata",
			setup: func(n *applyNode) { n.metaMode = "ro" },
			want:  nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := newApplyNode()
			c.setup(n)
			err := ApplySkipRanges(context.Background(), NewDm(n.oc()),
				"clone", "/dev/mapper/clone", applyRanges)
			if err == nil {
				t.Fatal("the apply answered no error")
			}
			if !slices.Equal(n.log, c.want) {
				t.Fatalf("steps %q, want %q", n.log, c.want)
			}
			if !n.hydration || n.devs["lin-a"].suspended ||
				n.devs["lin-b"].suspended {
				t.Fatal("the apply left a change behind")
			}
		})
	}
}

// TestApplySkipRangesReturnsWhatItCannotRestore: a resume that fails is
// part of the error, and the other holders are still resumed.
func TestApplySkipRangesReturnsWhatItCannotRestore(t *testing.T) {
	withFastDrain(t)
	n := newApplyNode()
	n.fail["dmsetup resume lin-b"] = true
	err := ApplySkipRanges(context.Background(), NewDm(n.oc()), "clone",
		"/dev/mapper/clone", applyRanges)
	if err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("answered %v, want the failed resume", err)
	}
	if n.devs["lin-a"].suspended {
		t.Fatal("lin-a was not resumed after lin-b's resume failed")
	}
}

// TestApplySkipRangesRestoresUnderACancelledCaller: a caller whose ctx ends
// during the wait still gets its holders resumed and the copy started again,
// since the restore does not run under the caller's ctx.
func TestApplySkipRangesRestoresUnderACancelledCaller(t *testing.T) {
	withFastDrain(t)
	cloneApplyDrainBound = time.Second
	n := newApplyNode()
	n.inFlight = []int{1}
	ctx, cancel := context.WithCancel(context.Background())
	oc := n.oc()
	run := oc.RunCommandFn
	oc.RunCommandFn = func(c context.Context, name string, args []string,
		stdin string) (string, string, int, error) {
		if c.Err() != nil {
			n.log = append(n.log, "CANCELLED "+args[0])
			return "", "", -1, c.Err()
		}
		out, stderr, code, err := run(c, name, args, stdin)
		if args[0] == "status" {
			cancel()
		}
		return out, stderr, code, err
	}
	err := ApplySkipRanges(ctx, NewDm(oc), "clone", "/dev/mapper/clone",
		applyRanges)
	if err == nil {
		t.Fatal("a cancelled apply answered no error")
	}
	if !n.hydration || n.devs["lin-a"].suspended || n.devs["lin-b"].suspended {
		t.Fatalf("not restored after the cancel: steps %q", n.log)
	}
	for _, step := range n.log {
		if strings.HasPrefix(step, "discard") {
			t.Fatalf("a cancelled apply discarded: %q", n.log)
		}
	}
}

// TestApplySkipRangesGoesOnPastTheFirstBound: copies still in flight when
// the first wait's bound passes do not stop the apply; the copies the pause
// lets end are waited out by the second wait, and the discard follows.
func TestApplySkipRangesGoesOnPastTheFirstBound(t *testing.T) {
	withFastDrain(t)
	n := newApplyNode()
	// Status reads: the stop's, then the first wait's (in flight past its
	// bound), then the second wait's, which finds none.
	n.inFlight = make([]int, 0, 64)
	n.inFlight = append(n.inFlight, 1)
	for i := 0; i < 60; i++ {
		n.inFlight = append(n.inFlight, 1)
	}
	n.inFlight = append(n.inFlight, 0)
	oc := n.oc()
	run := oc.RunCommandFn
	paused := false
	oc.RunCommandFn = func(c context.Context, name string, args []string,
		stdin string) (string, string, int, error) {
		if args[0] == "suspend" && !paused {
			// The pause ends the copies in flight: the next read shows none.
			paused = true
			n.inFlight = []int{0}
		}
		return run(c, name, args, stdin)
	}
	if err := ApplySkipRanges(context.Background(), NewDm(oc), "clone",
		"/dev/mapper/clone", applyRanges); err != nil {
		t.Fatalf("ApplySkipRanges: %v", err)
	}
	want := []string{
		"disable_hydration",
		"suspend lin-a",
		"suspend lin-b",
		"discard /dev/mapper/clone [{0 4096} {8192 4096}]",
		"resume lin-b",
		"resume lin-a",
		"enable_hydration",
	}
	if !slices.Equal(n.log, want) {
		t.Fatalf("steps %q, want %q", n.log, want)
	}
}
