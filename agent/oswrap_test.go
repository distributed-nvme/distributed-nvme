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
// reserve retry), ENXIO for dmsetup's own line for a device that is not there
// (the absence Dm.Info reads, dnagent.md SH15). A caller that read the text
// instead would break with the next wording change, and one that read it
// from the whole error would match the command line too. Around those four,
// the cases that must name no errno: another errno, a driver dmsetup cannot
// reach, a run that never reported, a tool other than dmsetup.
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
			name: "a noflush status read, which builds its own error",
			run:  dmRefusal("status", dev, "Device or resource busy"),
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).StatusNoFlush(ctx, dev)
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
			// dmsetup's own line for a name no device has, which Info and
			// InfoDevNo read as absent (IsNoDevice) and any other read
			// carries as its error.
			name: "a table read of a device that is not there",
			run: osRun{
				stderr:   "Device does not exist.\nCommand failed.\n",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).Table(ctx, dev)
				return err
			},
			errno: syscall.ENXIO,
		},
		{
			// dmsetup exits 1 here too, and nothing says the device is
			// not there.
			name: "a table read that cannot reach the driver",
			run: osRun{
				stderr: "Failure to communicate with kernel " +
					"device-mapper driver.\nCommand failed.\n",
				exitCode: 1,
				err:      errors.New("exit status 1"),
			},
			call: func(oc common.OsClient) error {
				_, err := NewDm(oc).Table(ctx, dev)
				return err
			},
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
				{"IsNoDevice", IsNoDevice, syscall.ENXIO},
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
		errors.New("Device does not exist."),
	} {
		if IsNoData(err) || IsExists(err) || IsBusy(err) || IsNoDevice(err) {
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

// ---------------------------------------------------------------------------
// An mtime read in the agent's own process: absent only when missing
// ---------------------------------------------------------------------------

// TestSubsysMtimeAbsentOnlyForMissing pins the split of the age read DN6
// judges an export without namespace one by (osclient.md, ReadMtime): it
// reads the subsystem's own directory, in the agent's own process, and only
// a directory that does not exist is absent — the export is gone, and a
// sweep passes it over. Every other failure — an I/O error, a path that runs
// through a file, a read cut off at the soft timeout — is an error that names
// the read, never an absence: a sweep would pass a live export over as gone,
// and its verdict would come back clean. Each read reaches the OsClient on a
// ctx carrying the SH15 soft timeout.
func TestSubsysMtimeAbsentOnlyForMissing(t *testing.T) {
	const nqn = "nqn.2024-01.io.dnv:2:0000000000000001:0000000000000011:" +
		"0000000000000021:0000000000000005"
	path := NvmetRoot + "/subsystems/" + nqn
	stamp := time.Unix(1700000000, 123456789)
	for _, tc := range []struct {
		name  string
		mtime time.Time
		err   error
		// present and fails are what SubsysMtime must answer.
		present bool
		fails   bool
	}{
		{name: "read", mtime: stamp, present: true},
		{name: "missing", err: &fs.PathError{
			Op: "lstat", Path: path, Err: syscall.ENOENT}},
		{name: "an I/O error", err: &fs.PathError{
			Op: "lstat", Path: path, Err: syscall.EIO}, fails: true},
		{name: "through a file", err: &fs.PathError{
			Op: "lstat", Path: path, Err: syscall.ENOTDIR}, fails: true},
		{name: "cut off at the soft timeout",
			err: context.DeadlineExceeded, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var read []string
			oc := &common.FakeOsClient{
				ReadMtimeFn: func(
					ctx context.Context, got string,
				) (time.Time, error) {
					read = append(read, got)
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) >
						common.CmdSoftTimeout*time.Second {
						t.Errorf("the mtime read of %s reached the "+
							"OsClient without the SH15 soft timeout", got)
					}
					return tc.mtime, tc.err
				},
			}
			mtime, present, err := NewNvmet(oc).SubsysMtime(
				context.Background(), nqn)
			if len(read) != 1 || read[0] != path {
				t.Fatalf("mtime reads = %q, want %s once", read, path)
			}
			if !tc.fails {
				if err != nil || present != tc.present ||
					!mtime.Equal(tc.mtime) {
					t.Fatalf("SubsysMtime = (%v, %v, %v), want (%v, %v, "+
						"nil)", mtime, present, err, tc.mtime, tc.present)
				}
				return
			}
			if err == nil {
				t.Fatalf("a read that did not answer read as (%v, "+
					"present=%v)", mtime, present)
			}
			if present || !mtime.IsZero() {
				t.Errorf("a failed read came back with (%v, present=%v)",
					mtime, present)
			}
			if !strings.Contains(err.Error(), "mtime of "+path) {
				t.Errorf("error %q does not name the read", err)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("error %q does not carry the read's own %v",
					err, tc.err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("error %q reads as a missing directory", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The disk's size, read in the agent's own process
// ---------------------------------------------------------------------------

// TestDiskSizesReadsSysfsInProcess pins the read of a disk's two sizes
// (dnagent.md SH17): the number of the block device the path names, once,
// then the "size" sysfs keeps under that number, which counts 512-byte
// sectors, and its logical block size, all read in the agent's own process
// on a ctx carrying the SH15 soft timeout, with no command run. A whole
// disk's block size is in its own queue/; a partition, which its "partition"
// file marks, has none, and its disk's is read through "..", the path
// joined as given and never cleaned, since only the kernel's walk of the
// number's link makes ".." the disk's directory. Every failure is an error
// that names what failed: a device that is not there or is not a block
// device, a read that did not answer, a file that is not there, a value
// that is not a number. None of them reads as a size, and the two reads
// stand apart: one that fails keeps the other's answer.
func TestDiskSizesReadsSysfsInProcess(t *testing.T) {
	const dev = "/dev/disk/by-id/nvme-fake"
	const dir = "/sys/dev/block/259:3"
	const sizePath = dir + "/size"
	const partPath = dir + "/partition"
	const diskLbs = dir + "/queue/logical_block_size"
	const partLbs = dir + "/../queue/logical_block_size"
	enoent := func(path string) error {
		return &fs.PathError{Op: "open", Path: path, Err: syscall.ENOENT}
	}
	eio := func(path string) error {
		return &fs.PathError{Op: "read", Path: path, Err: syscall.EIO}
	}
	type file struct {
		text string
		err  error
	}
	// disk512 is a whole disk of 1 GiB at 512-byte blocks: no partition
	// file, its own queue/.
	disk512 := func() map[string]file {
		return map[string]file{
			sizePath: {text: "2097152\n"},
			partPath: {err: enoent(partPath)},
			diskLbs:  {text: "512\n"},
			partLbs:  {err: enoent(partLbs)},
		}
	}
	// part is a partition of 1 GiB whose disk has lbs-byte blocks: a
	// partition file, no queue/ of its own.
	part := func(lbs string) map[string]file {
		return map[string]file{
			sizePath: {text: "2097152\n"},
			partPath: {text: "1\n"},
			diskLbs:  {err: enoent(diskLbs)},
			partLbs:  {text: lbs},
		}
	}
	with := func(files map[string]file, path string, f file) map[string]file {
		files[path] = f
		return files
	}
	diskReads := []string{"read " + sizePath, "read " + partPath,
		"read " + diskLbs}
	partReads := []string{"read " + sizePath, "read " + partPath,
		"read " + partLbs}
	for _, tc := range []struct {
		name     string
		devNo    string
		devNoErr error
		files    map[string]file
		want     DiskSizes
		// wantReads are the reads after the number's.
		wantReads []string
		// wantErr are parts of the error's text; none when the read
		// answers whole.
		wantErr []string
	}{
		{name: "a disk", devNo: "259:3", files: disk512(),
			want:      DiskSizes{Bytes: 1 << 30, LogicalBlock: 512},
			wantReads: diskReads},
		{name: "a disk of 4 KiB blocks", devNo: "259:3",
			files:     with(disk512(), diskLbs, file{text: "4096\n"}),
			want:      DiskSizes{Bytes: 1 << 30, LogicalBlock: 4096},
			wantReads: diskReads},
		{name: "a partition, through its disk", devNo: "259:3",
			files:     part("512\n"),
			want:      DiskSizes{Bytes: 1 << 30, LogicalBlock: 512},
			wantReads: partReads},
		{name: "a partition of a disk of 4 KiB blocks", devNo: "259:3",
			files:     part("4096\n"),
			want:      DiskSizes{Bytes: 1 << 30, LogicalBlock: 4096},
			wantReads: partReads},
		{name: "no device there", devNoErr: &fs.PathError{
			Op: "stat", Path: dev, Err: syscall.ENOENT},
			wantErr: []string{"device number of " + dev}},
		{name: "not a block device", devNoErr: &fs.PathError{
			Op: "stat", Path: dev, Err: syscall.ENOTBLK},
			wantErr: []string{"device number of " + dev}},
		{name: "the number read cut off", devNoErr: context.DeadlineExceeded,
			wantErr: []string{"device number of " + dev}},
		{name: "no size file", devNo: "259:3",
			files:     with(disk512(), sizePath, file{err: enoent(sizePath)}),
			want:      DiskSizes{LogicalBlock: 512},
			wantReads: diskReads,
			wantErr:   []string{dev + " (259:3) is not there"}},
		{name: "the size read failed", devNo: "259:3",
			files:     with(disk512(), sizePath, file{err: eio(sizePath)}),
			want:      DiskSizes{LogicalBlock: 512},
			wantReads: diskReads,
			wantErr:   []string{"read " + sizePath}},
		{name: "a size that is not a number", devNo: "259:3",
			files:     with(disk512(), sizePath, file{text: "lots\n"}),
			want:      DiskSizes{LogicalBlock: 512},
			wantReads: diskReads,
			wantErr:   []string{sizePath + ` reads "lots"`}},
		{name: "the partition read failed", devNo: "259:3",
			files:     with(disk512(), partPath, file{err: eio(partPath)}),
			want:      DiskSizes{Bytes: 1 << 30},
			wantReads: diskReads[:2],
			wantErr:   []string{"read " + partPath}},
		{name: "no block size file", devNo: "259:3",
			files:     with(disk512(), diskLbs, file{err: enoent(diskLbs)}),
			want:      DiskSizes{Bytes: 1 << 30},
			wantReads: diskReads,
			wantErr:   []string{dev + " (259:3) has no " + diskLbs}},
		{name: "the block size read failed", devNo: "259:3",
			files:     with(part("512\n"), partLbs, file{err: eio(partLbs)}),
			want:      DiskSizes{Bytes: 1 << 30},
			wantReads: partReads,
			wantErr:   []string{"read " + partLbs}},
		{name: "a block size of 0", devNo: "259:3",
			files:     with(disk512(), diskLbs, file{text: "0\n"}),
			want:      DiskSizes{Bytes: 1 << 30},
			wantReads: diskReads,
			wantErr:   []string{diskLbs + ` reads "0"`}},
		{name: "both reads failed", devNo: "259:3",
			files: with(with(disk512(), sizePath, file{err: eio(sizePath)}),
				diskLbs, file{err: eio(diskLbs)}),
			wantReads: diskReads,
			wantErr:   []string{"read " + sizePath, "read " + diskLbs}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads []string
			bounded := func(ctx context.Context, what string) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) >
					common.CmdSoftTimeout*time.Second {
					t.Errorf("%s reached the OsClient without the SH15 "+
						"soft timeout", what)
				}
			}
			oc := &common.FakeOsClient{
				RunCommandFn: func(
					_ context.Context, name string, args []string, _ string,
				) (string, string, int, error) {
					t.Errorf("the size read ran %s %q", name, args)
					return "", "", 0, nil
				},
				ReadDevNoFn: func(
					ctx context.Context, path string,
				) (string, error) {
					reads = append(reads, "devno "+path)
					bounded(ctx, "the device-number read")
					return tc.devNo, tc.devNoErr
				},
				ReadFileFn: func(
					ctx context.Context, path string,
				) (string, error) {
					reads = append(reads, "read "+path)
					bounded(ctx, "the read of "+path)
					f, ok := tc.files[path]
					if !ok {
						t.Errorf("read %s, a path no kernel lays out", path)
						return "", enoent(path)
					}
					return f.text, f.err
				},
			}
			sizes, err := NewDm(oc).DiskSizes(context.Background(), dev)
			wantReads := append([]string{"devno " + dev}, tc.wantReads...)
			if strings.Join(reads, "; ") != strings.Join(wantReads, "; ") {
				t.Errorf("reads = %q, want %q", reads, wantReads)
			}
			if sizes != tc.want {
				t.Errorf("DiskSizes = %+v, want %+v", sizes, tc.want)
			}
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("DiskSizes: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a failed read gave no error, and %+v", sizes)
			}
			for _, part := range tc.wantErr {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not say %q", err, part)
				}
			}
			causes := []error{tc.devNoErr}
			for _, f := range tc.files {
				causes = append(causes, f.err)
			}
			for _, cause := range causes {
				if cause != nil && !errors.Is(err, cause) &&
					!errors.Is(cause, fs.ErrNotExist) {
					t.Errorf("error %q does not carry the read's own %v",
						err, cause)
				}
			}
		})
	}
}
