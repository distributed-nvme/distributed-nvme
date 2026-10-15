package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// CmdError: the errno a refusal names, read in one place
// ---------------------------------------------------------------------------

// osRun is the one answer a fake OS gives to every command: what the tool
// printed, its exit status and the run's error, in the shape
// common.OsClient.RunCommand documents.
type osRun struct {
	stdout, stderr string
	exitCode       int
	err            error
}

// refusing answers every command with run and records the last command line.
func refusing(run osRun, gotName *string, gotArgs *[]string) *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(
			_ context.Context, name string, args []string, _ string,
		) (string, string, int, error) {
			if gotName != nil {
				*gotName = name
			}
			if gotArgs != nil {
				*gotArgs = append([]string(nil), args...)
			}
			return run.stdout, run.stderr, run.exitCode, run.err
		},
	}
}

// dmRefusal is a dmsetup refusal as the tool prints it: the ioctl, the
// device, the strerror text of the errno, then the closing line.
func dmRefusal(ioctl, dev, strerror string) osRun {
	return osRun{
		stderr: "device-mapper: " + ioctl + " ioctl on " + dev +
			"  failed: " + strerror + "\nCommand failed.\n",
		exitCode: 1,
		err:      errors.New("exit status 1"),
	}
}

// TestDmsetupRefusalCarriesItsErrno pins the one place that turns a tool's
// error words into an errno. A dmsetup refusal names its errno in strerror's
// words, and the wrapper turns those words into the errno a caller branches
// on: ENODATA for a thin table whose id the pool lacks (the lost thin id of
// cnagent.md CN14), EEXIST for a snapshot message whose id the pool already
// holds (the torn test of CN14), EBUSY for a held metadata snapshot (CN25's
// reserve retry). A caller that read the text instead would break with the
// next wording change, and one that read it from the whole error would match
// the command line too. Around those three, the cases that must name no
// errno: another errno, a run that never reported, a tool other than
// dmsetup.
func TestDmsetupRefusalCarriesItsErrno(t *testing.T) {
	ctx := context.Background()
	const dev = "dnv-thin-0000000000000001"
	const table = "0 8 thin 253:4 7"
	for _, tc := range []struct {
		name  string
		run   osRun
		call  func(oc common.OsClient) error
		errno syscall.Errno
	}{
		{
			name: "a thin table whose id the pool lacks",
			run:  dmRefusal("reload", dev, "No data available"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).Create(ctx, dev, table)
			},
			errno: syscall.ENODATA,
		},
		{
			name: "a reload of a thin table whose id the pool lacks",
			run:  dmRefusal("reload", dev, "No data available"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).LoadTable(ctx, dev, table)
			},
			errno: syscall.ENODATA,
		},
		{
			name: "a snapshot message of an id the pool holds",
			run:  dmRefusal("message", "dnv-pool", "File exists"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).Message(ctx, "dnv-pool", 0,
					"create_snap 7 3")
			},
			errno: syscall.EEXIST,
		},
		{
			name: "the same refusal in other words",
			run: osRun{
				stderr:   "Pool metadata snapshot already exists",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				return NewDm(oc).Message(ctx, "dnv-pool", 0,
					"reserve_metadata_snap")
			},
			errno: syscall.EEXIST,
		},
		{
			name: "a second metadata snapshot reserve",
			run:  dmRefusal("message", "dnv-pool", "Device or resource busy"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).Message(ctx, "dnv-pool", 0,
					"reserve_metadata_snap")
			},
			errno: syscall.EBUSY,
		},
		{
			name: "a removal of an open device, words in any case",
			run:  dmRefusal("remove", dev, "DEVICE OR RESOURCE BUSY"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).Remove(ctx, dev)
			},
			errno: syscall.EBUSY,
		},
		{
			name: "a multi-target create, through stdin",
			run:  dmRefusal("reload", dev, "No data available"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).CreateMulti(ctx, dev, table+"\n")
			},
			errno: syscall.ENODATA,
		},
		{
			name: "a status read, which builds its own error",
			run:  dmRefusal("status", dev, "Device or resource busy"),
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).Status(ctx, dev)
				return err
			},
			errno: syscall.EBUSY,
		},
		{
			name: "a table read, which builds its own error",
			run:  dmRefusal("table", dev, "Device or resource busy"),
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).Table(ctx, dev)
				return err
			},
			errno: syscall.EBUSY,
		},
		{
			name: "a listing, which builds its own error",
			run:  dmRefusal("list", "", "Device or resource busy"),
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).List(ctx)
				return err
			},
			errno: syscall.EBUSY,
		},
		{
			name: "the words on stdout when stderr is empty",
			run: osRun{
				stdout:   "device-mapper: message ioctl failed: File exists",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				return NewDm(oc).Message(ctx, "dnv-pool", 0,
					"create_thin 7")
			},
			errno: syscall.EEXIST,
		},
		{
			name: "another errno",
			run:  dmRefusal("reload", dev, "Invalid argument"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).Create(ctx, dev, table)
			},
		},
		{
			// The command line is no part of the match: a message whose
			// own words read like an errno names none when the tool
			// refused it for another reason.
			name: "errno words in the command line only",
			run:  dmRefusal("message", "dnv-pool", "Invalid argument"),
			call: func(oc common.OsClient) error {
				return NewDm(oc).Message(ctx, "dnv-pool", 0,
					"no data available, file exists, device or "+
						"resource busy")
			},
		},
		{
			// A run that never reported refused nothing, whatever it
			// printed before it was killed.
			name: "a killed run",
			run: osRun{
				stderr: "device-mapper: message ioctl on dnv-pool  " +
					"failed: File exists",
				exitCode: -1,
				err:      errors.New("signal: killed"),
			},
			call: func(oc common.OsClient) error {
				return NewDm(oc).Message(ctx, "dnv-pool", 0,
					"create_snap 7 3")
			},
		},
		{
			name: "a run that printed nothing",
			run: osRun{
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				return NewDm(oc).Remove(ctx, dev)
			},
		},
		{
			// Only dmsetup's words are read: another tool's text is its
			// own, and nothing branches on it.
			name: "blkdiscard",
			run: osRun{
				stderr:   "blkdiscard: /dev/x: BLKDISCARD: Device or resource busy",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				return NewDm(oc).BlkDiscardRange(ctx, "/dev/x", 0, 4096)
			},
		},
		{
			name: "blkdiscard --zeroout, which builds its own error",
			run: osRun{
				stderr:   "blkdiscard: /dev/x: BLKZEROOUT: Device or resource busy",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).BlkZeroout(ctx, "/dev/x", 0, 4096)
				return err
			},
		},
		{
			name: "lsblk's device number, which builds its own error",
			run: osRun{
				stderr:   "lsblk: /dev/x: not a block device",
				exitCode: 32,
				err:      errors.New("exit status 32"),
			},
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).DevNo(ctx, "/dev/x")
				return err
			},
		},
		{
			name: "lsblk's size, which builds its own error",
			run: osRun{
				stderr:   "lsblk: /dev/x: Device or resource busy",
				exitCode: 32,
				err:      errors.New("exit status 32"),
			},
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).DiskSize(ctx, "/dev/x")
				return err
			},
		},
		{
			name: "losetup through Cmd",
			run: osRun{
				stderr:   "losetup: /dev/loop0: File exists",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				return NewCmd(oc).RunOk(ctx, "losetup", "-d", "/dev/loop0")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(refusing(tc.run, nil, nil))
			if err == nil {
				t.Fatal("a refused command returned no error")
			}
			var cmdErr *CmdError
			if !errors.As(err, &cmdErr) {
				t.Fatalf("the error is %T, not a *CmdError: %v", err, err)
			}
			if cmdErr.Errno != tc.errno {
				t.Errorf("errno = %v (%d), want %v (%d)",
					cmdErr.Errno, cmdErr.Errno, tc.errno, tc.errno)
			}
			if cmdErr.ExitCode != tc.run.exitCode {
				t.Errorf("exit code = %d, want %d",
					cmdErr.ExitCode, tc.run.exitCode)
			}
			for _, h := range []struct {
				name  string
				is    func(error) bool
				errno syscall.Errno
			}{
				{"IsNoData", IsNoData, syscall.ENODATA},
				{"IsExists", IsExists, syscall.EEXIST},
				{"IsBusy", IsBusy, syscall.EBUSY},
			} {
				want := tc.errno == h.errno
				if got := h.is(err); got != want {
					t.Errorf("%s = %v, want %v", h.name, got, want)
				}
				// A caller that wraps the error keeps its errno.
				wrapped := fmt.Errorf("pool slice 1: %w", err)
				if got := h.is(wrapped); got != want {
					t.Errorf("%s of a wrapped error = %v, want %v",
						h.name, got, want)
				}
				if got := errors.Is(err, h.errno); got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v",
						h.errno, got, want)
				}
			}
		})
	}

	// The errno is a type, never a text: an error that merely says the
	// words is not a refusal, and no error is none.
	for _, err := range []error{
		nil,
		errors.New("device-mapper: reload ioctl on x  failed: " +
			"No data available"),
		errors.New("failed: File exists"),
		errors.New("failed: Device or resource busy"),
	} {
		if IsNoData(err) || IsExists(err) || IsBusy(err) {
			t.Errorf("%v reads as a dmsetup refusal", err)
		}
	}
}

// TestCmdErrorText pins the text of a failed command, which callers log and
// failed rows carry in their details (DN19): the command line, a colon, and
// the tool's trimmed stderr, else its trimmed stdout, else the run's own
// error. The type carries the errno beside the text and changes no word of
// it.
func TestCmdErrorText(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  osRun
		want string
	}{
		{
			name: "stderr",
			run:  dmRefusal("message", "dnv-pool", "File exists"),
			want: "dmsetup message dnv-pool 0 create_snap 7 3: " +
				"device-mapper: message ioctl on dnv-pool  failed: " +
				"File exists\nCommand failed.",
		},
		{
			name: "stdout when stderr is blank",
			run: osRun{
				stdout:   "  out text \n",
				stderr:   " \n",
				exitCode: 2,
				err:      errors.New("exit status 2"),
			},
			want: "dmsetup message dnv-pool 0 create_snap 7 3: out text",
		},
		{
			name: "the run's error when the tool printed nothing",
			run: osRun{
				exitCode: -1,
				err:      errors.New("signal: killed"),
			},
			want: "dmsetup message dnv-pool 0 create_snap 7 3: " +
				"signal: killed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var name string
			var args []string
			err := NewDm(refusing(tc.run, &name, &args)).Message(
				ctx, "dnv-pool", 0, "create_snap 7 3")
			if err == nil {
				t.Fatal("a refused command returned no error")
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
			var cmdErr *CmdError
			if !errors.As(err, &cmdErr) {
				t.Fatalf("the error is %T, not a *CmdError", err)
			}
			if cmdErr.Name != name || fmt.Sprint(cmdErr.Args) !=
				fmt.Sprint(args) {
				t.Errorf("command line = %s %v, want %s %v",
					cmdErr.Name, cmdErr.Args, name, args)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A listing in the agent's own process: absent only when missing
// ---------------------------------------------------------------------------

// TestOsBaseReadDirAbsentOnlyForMissing pins the split of a listing made in
// the agent's own process (osclient.md, ReadDir): only a directory that does
// not exist is absent. Every other failure — an I/O error part-way through,
// a path that is not a directory, a listing cut off at the soft timeout — is
// an error that names the listing, never an empty directory: a caller that
// enumerates the node acts on what a listing leaves out as gone. Each
// listing reaches the OsClient on a ctx carrying the SH15 soft timeout.
func TestOsBaseReadDirAbsentOnlyForMissing(t *testing.T) {
	const path = "/sys/block/md127/md"
	for _, tc := range []struct {
		name  string
		names []string
		err   error
		// present and fails are what ListDir must answer.
		present bool
		fails   bool
	}{
		{name: "listed", names: []string{"array_state", "dev-dm-3"},
			present: true},
		{name: "empty", present: true},
		{name: "missing", err: &fs.PathError{
			Op: "open", Path: path, Err: syscall.ENOENT}},
		{name: "an I/O error part-way through", err: &fs.PathError{
			Op: "readdirent", Path: path, Err: syscall.EIO}, fails: true},
		{name: "not a directory", err: &fs.PathError{
			Op: "open", Path: path, Err: syscall.ENOTDIR}, fails: true},
		{name: "cut off at the soft timeout",
			err: context.DeadlineExceeded, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var listed []string
			oc := &common.FakeOsClient{
				ReadDirFn: func(
					ctx context.Context, got string,
				) ([]string, error) {
					listed = append(listed, got)
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) >
						common.CmdSoftTimeout*time.Second {
						t.Errorf("the listing of %s reached the OsClient "+
							"without the SH15 soft timeout", got)
					}
					return tc.names, tc.err
				},
			}
			names, present, err := NewCmd(oc).ListDir(
				context.Background(), path)
			if len(listed) != 1 || listed[0] != path {
				t.Fatalf("listings = %q, want %s once", listed, path)
			}
			if !tc.fails {
				if err != nil || present != tc.present ||
					fmt.Sprint(names) != fmt.Sprint(tc.names) {
					t.Fatalf("ListDir = (%q, %v, %v), want (%q, %v, nil)",
						names, present, err, tc.names, tc.present)
				}
				return
			}
			if err == nil {
				t.Fatalf("a listing that did not answer read as (%q, "+
					"present=%v): an absent or empty directory", names,
					present)
			}
			if present || names != nil {
				t.Errorf("a failed listing came back with (%q, present=%v)",
					names, present)
			}
			if !strings.Contains(err.Error(), "list "+path) {
				t.Errorf("error %q does not name the listing", err)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("error %q does not carry the listing's own %v",
					err, tc.err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("error %q reads as a missing directory", err)
			}
		})
	}
}
