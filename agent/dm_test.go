package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// Whole-node readers: the lab's output, and the answer rule of the
// whole-node listings (osclient.md, RunCommand)
// ---------------------------------------------------------------------------

// The lab fixtures are dmsetup's own output on a controller node
// (libdevmapper 1.02.205) holding three devices: one with no table, one
// suspended, and one whose name holds ",", ";" and "\" with two targets. The
// info listing is not sorted and does not escape names. The concise listing
// escapes them, ends each table line with a space, puts ";" between devices
// and ends with one newline.
const (
	labInfoListing = `dnvprobe-notable/252/1/---w
dnvprobe-susp/252/2/L-sw
dnvprobe,a;b\c/252/0/L--w
`
	labTableListing = "dnvprobe-notable,,1,rw;" +
		"dnvprobe-susp,,2,rw,0 8 zero ;" +
		`dnvprobe\,a\;b\\c,,0,rw,0 8 zero ,8 8 zero ` + "\n"
	labOddName    = `dnvprobe,a;b\c`
	labNoDevices  = "No devices found\n"
	labGoneDevice = "Device does not exist.\nCommand failed.\n"
)

// dmInfoListArgv and dmTableListArgv are the two whole-node command lines,
// spelled out, so that a change to either is a change a test sees.
var (
	dmInfoListArgv = []string{"info", "--columns", "--noheadings",
		"--separator", "/", "-o", "name,major,minor,attr"}
	dmTableListArgv = []string{"table", "--concise"}
)

// exitedWith is a run that reported: the tool printed stdout and stderr and
// exited with code.
func exitedWith(code int, stdout, stderr string) osRun {
	run := osRun{stdout: stdout, stderr: stderr, exitCode: code}
	if code != 0 {
		run.err = fmt.Errorf("exit status %d", code)
	}
	return run
}

// killedAfter is a run that never reported: killed at the soft timeout after
// it printed stdout.
func killedAfter(stdout string) osRun {
	return osRun{stdout: stdout, exitCode: -1,
		err: errors.New("signal: killed")}
}

// slotRefused is a run that never started: the OS client's semaphore gave
// it no slot before the ctx ran out.
var slotRefused = osRun{exitCode: -1, err: context.DeadlineExceeded}

// dmsetupAnswering answers the one dmsetup command line argv with run and
// counts the runs in *runs. Any other command line, and a run with no SH15
// deadline on its ctx, fails the test.
func dmsetupAnswering(
	t *testing.T,
	argv []string,
	run osRun,
	runs *int,
) *common.FakeOsClient {
	t.Helper()
	return &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, name string, args []string, _ string,
		) (string, string, int, error) {
			*runs++
			if name != "dmsetup" || !slices.Equal(args, argv) {
				t.Errorf("ran %s %q, want dmsetup %q", name, args, argv)
				return "", "", 127, errors.New("unexpected command")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("dmsetup %q ran without the SH15 deadline", args)
			}
			return run.stdout, run.stderr, run.exitCode, run.err
		},
	}
}

// TestDmListInfoAnswers pins the whole-node info listing over the lab's
// output and the answer rule. A complete listing gives every device with its
// number and flags, and its name verbatim whatever bytes it holds. A run
// that exits 1 after printing the devices that remain answers, as a partial
// listing: dmsetup exits so whenever a device goes away during the listing,
// as another pool's devices do while that pool is torn down, and read as
// "did not answer" such a listing would turn to error every row it serves.
// A line of more than four fields does not parse, since "/" is the one byte
// a name cannot hold. A line whose numbers do not parse keeps its device, as
// `dmsetup ls` keeps a name with no number
// (TestDmListParsesBothDevNoSpellings). "No devices found" with exit status
// 0 is an empty node, but only as the whole output. An empty output, and a
// run that exits 1 having printed no device, did not answer: an empty
// listing read off either would make every device of the node read as
// absent. So did a run that never reported, whatever it printed first: a
// kill can cut its last line inside the attr column, which then reads as
// resumed and writeable. The error of a listing that did not answer gives
// the reason and never the listing, which every row the listing serves
// would carry: dmsetup's stderr, the run's own error, or the first line it
// cannot read.
func TestDmListInfoAnswers(t *testing.T) {
	ctx := context.Background()
	labDevs := map[string]DmDevInfo{
		"dnvprobe-notable": {DevNo: "252:1"},
		"dnvprobe-susp":    {DevNo: "252:2", Suspended: true},
		labOddName:         {DevNo: "252:0"},
	}
	for _, tc := range []struct {
		name string
		run  osRun
		// want is the listing's devices, nil when it did not answer, and
		// err then the error's text past the command line.
		want    map[string]DmDevInfo
		partial bool
		err     string
	}{
		{
			name: "complete",
			run:  exitedWith(0, labInfoListing, ""),
			want: labDevs,
		},
		{
			name: "a device gone during the listing",
			run: exitedWith(1, "dnvprobe-susp/252/2/L-sw\n"+
				labOddName+"/252/0/L--w\n", labGoneDevice),
			want: map[string]DmDevInfo{
				"dnvprobe-susp": {DevNo: "252:2", Suspended: true},
				labOddName:      {DevNo: "252:0"},
			},
			partial: true,
		},
		{
			name:    "a line that does not parse",
			run:     exitedWith(0, labInfoListing+"dnvprobe-cut/252\n", ""),
			want:    labDevs,
			partial: true,
		},
		{
			name:    "a line with no name",
			run:     exitedWith(0, "/252/9/L--w\n"+labInfoListing, ""),
			want:    labDevs,
			partial: true,
		},
		{
			name:    "a line of five fields",
			run:     exitedWith(0, "a/b/252/9/L--w\n"+labInfoListing, ""),
			want:    labDevs,
			partial: true,
		},
		{
			name: "a name is taken verbatim",
			run:  exitedWith(0, " dnv pad /252/7/L--w \n", ""),
			want: map[string]DmDevInfo{" dnv pad ": {DevNo: "252:7"}},
		},
		{
			name: "numbers that do not parse keep the name",
			run: exitedWith(0, "dnv-nonum//3/L--r\n"+
				"dnv-signed/-252/3/L-sw\n"+
				"dnv-text/252/x/---w\n", ""),
			want: map[string]DmDevInfo{
				"dnv-nonum":  {ReadOnly: true},
				"dnv-signed": {Suspended: true},
				"dnv-text":   {},
			},
		},
		{
			name: "an empty node",
			run:  exitedWith(0, labNoDevices, ""),
			want: map[string]DmDevInfo{},
		},
		{
			name: "empty output",
			run:  exitedWith(0, "", ""),
			err:  "empty output",
		},
		{
			name: "blank output",
			run:  exitedWith(0, " \n\n", ""),
			err:  "empty output",
		},
		{
			name: "no line that parses",
			run:  exitedWith(0, "dnvprobe-cut/252\ndnvprobe-cut/253\n", ""),
			err:  `cannot read "dnvprobe-cut/252"`,
		},
		{
			name: "No devices found beside a line",
			run:  exitedWith(0, labNoDevices+"x\n", ""),
			err:  `cannot read "x"`,
		},
		{
			name: "exit 1 with no device",
			run:  exitedWith(1, "", labGoneDevice),
			err:  "Device does not exist.\nCommand failed.",
		},
		{
			name: "exit 1 after No devices found",
			run:  exitedWith(1, labNoDevices, "Command failed.\n"),
			err:  "Command failed.",
		},
		{
			name: "killed after its lines",
			run:  killedAfter(labInfoListing),
			err:  "signal: killed",
		},
		{
			name: "no slot",
			run:  slotRefused,
			err:  "context deadline exceeded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			listing, err := NewDm(dmsetupAnswering(
				t, dmInfoListArgv, tc.run, &runs)).ListInfo(ctx)
			if runs != 1 {
				t.Errorf("ran dmsetup %d times, want once", runs)
			}
			if tc.want == nil {
				if err == nil {
					t.Fatalf("listed %d devices, want an error",
						len(listing.Devs))
				}
				want := "dmsetup " + strings.Join(dmInfoListArgv, " ") +
					": " + tc.err
				if err.Error() != want {
					t.Errorf("error %q, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListInfo: %v", err)
			}
			if listing.Devs == nil {
				t.Fatal("Devs is nil, want a map a caller can look up")
			}
			got := map[string]DmDevInfo{}
			for name, info := range listing.Devs {
				got[name] = *info
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("devices = %v, want %v", got, tc.want)
			}
			if listing.Partial != tc.partial {
				t.Errorf("Partial = %v, want %v",
					listing.Partial, tc.partial)
			}
		})
	}
}

// sameTargets compares two parsed tables target by target; a target with no
// arguments matches whether its Args is nil or empty.
func sameTargets(a, b []common.DmTarget) bool {
	return slices.EqualFunc(a, b, func(x, y common.DmTarget) bool {
		return x.Start == y.Start && x.Length == y.Length &&
			x.Type == y.Type && slices.Equal(x.Args, y.Args)
	})
}

// TestDmListTablesParsesConcise pins the reading of the whole-node concise
// table listing. A backslash makes the next byte literal, in a name as in a
// table line: the lab's device whose name holds ",", ";" and "\" is one
// device with its two targets, not broken records, and a crypt key
// description can hold the same bytes. The space the tool ends a table line
// with is no part of the line. A device with no live table is listed, with
// no target, so that a caller tells it from a device the listing lacks. The
// answers follow the info listing's rule (TestDmListInfoAnswers): exit status
// 1 after records that parse is a partial listing, "No devices found" with
// exit status 0, as the whole output, an empty node, and a run that printed
// nothing readable, or never reported, did not answer, with an error that
// gives the reason and never the listing.
func TestDmListTablesParsesConcise(t *testing.T) {
	ctx := context.Background()
	zero := func(start uint64) common.DmTarget {
		return common.DmTarget{Start: start, Length: 8, Type: "zero"}
	}
	labTables := map[string][]common.DmTarget{
		"dnvprobe-notable": {},
		"dnvprobe-susp":    {zero(0)},
		labOddName:         {zero(0), zero(8)},
	}
	const goneTable = "device-mapper: table ioctl on dnvprobe-gone  " +
		"failed: No such device or address\nCommand failed."
	for _, tc := range []struct {
		name string
		run  osRun
		// want is the listing's tables, nil when it did not answer, and
		// err then the error's text past the command line.
		want    map[string][]common.DmTarget
		partial bool
		err     string
	}{
		{
			name: "the lab's listing",
			run:  exitedWith(0, labTableListing, ""),
			want: labTables,
		},
		{
			name: "a concat and escapes in a table line",
			run: exitedWith(0, "dnv-concat,,5,rw,"+
				"0 2048 linear 9:127 0,2048 2048 linear 9:126 0;"+
				`dnv-crypt,CRYPT-x\;y,6,ro,0 8 crypt aes-xts-plain64 `+
				`:64:logon:dnv\,key\;1\\x 0 252:0 0`+"\n", ""),
			want: map[string][]common.DmTarget{
				"dnv-concat": {
					{Start: 0, Length: 2048, Type: "linear",
						Args: []string{"9:127", "0"}},
					{Start: 2048, Length: 2048, Type: "linear",
						Args: []string{"9:126", "0"}},
				},
				"dnv-crypt": {
					{Start: 0, Length: 8, Type: "crypt",
						Args: []string{"aes-xts-plain64",
							`:64:logon:dnv,key;1\x`, "0", "252:0", "0"}},
				},
			},
		},
		{
			name: "a trailing separator is no record",
			run:  exitedWith(0, "dnvprobe-susp,,2,rw,0 8 zero ;\n", ""),
			want: map[string][]common.DmTarget{
				"dnvprobe-susp": {zero(0)},
			},
		},
		{
			name: "a device gone during the listing",
			run: exitedWith(1,
				"dnvprobe-notable,,1,rw;dnvprobe-susp,,2,rw,0 8 zero \n",
				goneTable+"\n"),
			want: map[string][]common.DmTarget{
				"dnvprobe-notable": {},
				"dnvprobe-susp":    {zero(0)},
			},
			partial: true,
		},
		{
			name:    "a record that does not parse",
			run:     exitedWith(0, labTableListing+"dnvprobe-cut,,3\n", ""),
			want:    labTables,
			partial: true,
		},
		{
			name: "a record with no name",
			run: exitedWith(0, ",,3,rw,0 8 zero ;"+
				labTableListing, ""),
			want:    labTables,
			partial: true,
		},
		{
			name: "an empty node",
			run:  exitedWith(0, labNoDevices, ""),
			want: map[string][]common.DmTarget{},
		},
		{
			name: "empty output",
			run:  exitedWith(0, "", ""),
			err:  "empty output",
		},
		{
			name: "no record that parses",
			run:  exitedWith(0, "dnvprobe-cut,,3;dnvprobe-cut,,4\n", ""),
			err:  `cannot read "dnvprobe-cut,,3"`,
		},
		{
			name: "No devices found beside a record",
			run:  exitedWith(0, labNoDevices+"x\n", ""),
			err:  `cannot read "No devices found"`,
		},
		{
			name: "exit 1 with no device",
			run:  exitedWith(1, "", goneTable+"\n"),
			err:  goneTable,
		},
		{
			name: "exit 1 after No devices found",
			run:  exitedWith(1, labNoDevices, "Command failed.\n"),
			err:  "Command failed.",
		},
		{
			name: "killed after its records",
			run:  killedAfter(labTableListing),
			err:  "signal: killed",
		},
		{
			name: "no slot",
			run:  slotRefused,
			err:  "context deadline exceeded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			listing, err := NewDm(dmsetupAnswering(
				t, dmTableListArgv, tc.run, &runs)).ListTables(ctx)
			if runs != 1 {
				t.Errorf("ran dmsetup %d times, want once", runs)
			}
			if tc.want == nil {
				if err == nil {
					t.Fatalf("listed %d tables, want an error",
						len(listing.Tables))
				}
				want := "dmsetup table --concise: " + tc.err
				if err.Error() != want {
					t.Errorf("error %q, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListTables: %v", err)
			}
			if listing.Tables == nil {
				t.Fatal("Tables is nil, want a map a caller can look up")
			}
			if !maps.EqualFunc(listing.Tables, tc.want, sameTargets) {
				t.Errorf("tables = %v, want %v", listing.Tables, tc.want)
			}
			for name, targets := range listing.Tables {
				if targets == nil {
					t.Errorf("%s maps to a nil table, want an empty one",
						name)
				}
			}
			if listing.Partial != tc.partial {
				t.Errorf("Partial = %v, want %v",
					listing.Partial, tc.partial)
			}
		})
	}
}

// TestDmInfoDevNo pins the read of one device with its number, a probe under
// Info's rule: a device that is there gives its number and flags, its name
// matched verbatim; a run that reported no such device gives nil, nil; and a
// run that never reported is an error, never an absence, the rule every
// probe keeps (osclient.md, RunCommand). A run that exits 0 with output it
// cannot read, or whose first line names another device, is an error as
// well: neither says the device is gone. A line whose numbers do not parse
// gives the device with no number and no error, as the listing does, so a
// caller that needs the number fails the same way whichever of the two it
// read.
func TestDmInfoDevNo(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		dev  string
		run  osRun
		// want is the device; absent says the run reported no device; with
		// neither, the read did not answer.
		want   *DmDevInfo
		absent bool
	}{
		{
			name: "there",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, "dnvprobe-notable/252/1/---w\n", ""),
			want: &DmDevInfo{DevNo: "252:1"},
		},
		{
			name: "suspended",
			dev:  "dnvprobe-susp",
			run:  exitedWith(0, "dnvprobe-susp/252/2/L-sw\n", ""),
			want: &DmDevInfo{DevNo: "252:2", Suspended: true},
		},
		{
			name: "a name that holds the concise escapes",
			dev:  labOddName,
			run:  exitedWith(0, labOddName+"/252/0/L--w\n", ""),
			want: &DmDevInfo{DevNo: "252:0"},
		},
		{
			name: "after a blank line",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, "\ndnvprobe-notable/252/1/---w\n", ""),
			want: &DmDevInfo{DevNo: "252:1"},
		},
		{
			name: "numbers that do not parse",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, "dnvprobe-notable//1/L--r\n", ""),
			want: &DmDevInfo{ReadOnly: true},
		},
		{
			name:   "not there",
			dev:    "dnvprobe-nosuch",
			run:    exitedWith(1, "", labGoneDevice),
			absent: true,
		},
		{
			name: "killed",
			dev:  "dnvprobe-notable",
			run:  killedAfter("dnvprobe-notable/252/1/---w\n"),
		},
		{name: "no slot", dev: "dnvprobe-notable", run: slotRefused},
		{
			name: "another device's line",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, "dnvprobe-susp/252/2/L-sw\n", ""),
		},
		{
			name: "another device's line first",
			dev:  "dnvprobe-notable",
			run: exitedWith(0, "dnvprobe-susp/252/2/L-sw\n"+
				"dnvprobe-notable/252/1/---w\n", ""),
		},
		{
			name: "output it cannot read",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, "L--w\n", ""),
		},
		{
			name: "empty output",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, "", ""),
		},
		{
			name: "No devices found",
			dev:  "dnvprobe-notable",
			run:  exitedWith(0, labNoDevices, ""),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			argv := append(slices.Clone(dmInfoListArgv), tc.dev)
			dev, err := NewDm(dmsetupAnswering(
				t, argv, tc.run, &runs)).InfoDevNo(ctx, tc.dev)
			if runs != 1 {
				t.Errorf("ran dmsetup %d times, want once", runs)
			}
			switch {
			case tc.want != nil:
				if err != nil {
					t.Fatalf("InfoDevNo: %v", err)
				}
				if dev == nil || *dev != *tc.want {
					t.Errorf("device = %+v, want %+v", dev, *tc.want)
				}
			case tc.absent:
				if dev != nil || err != nil {
					t.Errorf("got %+v, %v; want nil, nil", dev, err)
				}
			default:
				if err == nil {
					t.Fatalf("got %+v and no error, want an error", dev)
				}
				if dev != nil {
					t.Errorf("got %+v beside the error %v", dev, err)
				}
				if !strings.HasPrefix(err.Error(),
					"dmsetup "+strings.Join(argv, " ")+":") {
					t.Errorf("error %q does not name the read", err)
				}
			}
		})
	}
}
