package common

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The level of each OsClient record (log.md R9; osclient.md, Logging)
// ---------------------------------------------------------------------------

// levelOf is the "level" of the one record with msg, failing unless exactly
// one was logged since the capture's mark.
func levelOf(t *testing.T, capture *logCapture, msg string) string {
	t.Helper()
	level, _ := capture.onlyMsg(t, msg)["level"].(string)
	return level
}

// R9: a read that succeeds logs at DEBUG, and so does a file, proto,
// modification-time or device-number read or a directory listing that finds
// its path absent, and a device-number read that finds no block device at
// its path; any other failure logs at INFO with its error, a raw block read
// of an absent path among them; every write logs at INFO whatever its
// outcome.
func TestOsClientReadRecordsAreDebug(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	blockDev := aBlockDevice()
	// shut is a directory its owner may not search, so a stat of a path
	// under it fails with EACCES, a failure that is no answer.
	shut := filepath.Join(dir, "shut")
	if err := os.Mkdir(shut, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shut, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shut, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(shut, 0o700) })
	client := NewLimitedOsClient(0)
	for _, tc := range []struct {
		name, msg, want string
		op              func(t *testing.T) error
	}{
		{"file", "os read file", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadFile(ctx, file)
			return err
		}},
		{"absent file", "os read file", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadFile(ctx, missing)
			return err
		}},
		{"a directory read as a file", "os read file", "INFO", func(t *testing.T) error {
			_, err := client.ReadFile(ctx, dir)
			return err
		}},
		{"dir", "os read dir", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadDir(ctx, dir)
			return err
		}},
		{"absent dir", "os read dir", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadDir(ctx, missing)
			return err
		}},
		{"a file read as a dir", "os read dir", "INFO", func(t *testing.T) error {
			_, err := client.ReadDir(ctx, file)
			return err
		}},
		{"mtime", "os read mtime", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadMtime(ctx, file)
			return err
		}},
		{"absent mtime", "os read mtime", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadMtime(ctx, missing)
			return err
		}},
		{"devno", "os read devno", "DEBUG", func(t *testing.T) error {
			if blockDev == "" {
				t.Skip("no block device under /dev")
			}
			_, err := client.ReadDevNo(ctx, blockDev)
			return err
		}},
		{"absent devno", "os read devno", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadDevNo(ctx, missing)
			return err
		}},
		{"devno of a character device", "os read devno", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadDevNo(ctx, "/dev/null")
			return err
		}},
		{"devno of a directory", "os read devno", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadDevNo(ctx, dir)
			return err
		}},
		{"devno behind a directory it may not search", "os read devno", "INFO",
			func(t *testing.T) error {
				if os.Geteuid() == 0 {
					t.Skip("root searches every directory")
				}
				_, err := client.ReadDevNo(ctx, filepath.Join(shut, "f"))
				return err
			}},
		{"block", "os read block", "DEBUG", func(t *testing.T) error {
			_, err := client.ReadBlock(ctx, file, 0, 4)
			return err
		}},
		{"short block", "os read block", "INFO", func(t *testing.T) error {
			_, err := client.ReadBlock(ctx, file, 0, 4096)
			return err
		}},
		{"absent block device", "os read block", "INFO", func(t *testing.T) error {
			_, err := client.ReadBlock(ctx, missing, 0, 4)
			return err
		}},
		{"absent block device direct", "os read block direct", "INFO",
			func(t *testing.T) error {
				_, err := client.ReadBlockDirect(ctx, missing, 0, 4096)
				return err
			}},
		{"absent proto", "os read proto", "DEBUG", func(t *testing.T) error {
			return client.ReadProto(ctx, missing, &pb.NvmeTrConf{})
		}},
		{"a directory read as a proto", "os read proto", "INFO", func(t *testing.T) error {
			return client.ReadProto(ctx, dir, &pb.NvmeTrConf{})
		}},
		{"write file", "os write file", "INFO", func(t *testing.T) error {
			return client.WriteFile(ctx, filepath.Join(dir, "w"), "x")
		}},
		{"failed write file", "os write file", "INFO", func(t *testing.T) error {
			return client.WriteFile(ctx, filepath.Join(missing, "w"), "x")
		}},
		{"write proto", "os write proto", "INFO", func(t *testing.T) error {
			return client.WriteProto(ctx, filepath.Join(dir, "p"),
				&pb.NvmeTrConf{TrType: "tcp"})
		}},
		{"write block", "os write block", "INFO", func(t *testing.T) error {
			return client.WriteBlock(ctx, file, 0, []byte("ab"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureLogs(t)
			err := tc.op(t)
			if got := levelOf(t, capture, tc.msg); got != tc.want {
				t.Errorf("%s (err %v): level %s, want %s", tc.msg, err, got,
					tc.want)
			}
			rec := capture.onlyMsg(t, tc.msg)
			if (err != nil) != (rec["error"] != nil) {
				t.Errorf("err %v, but the record's error is %v", err,
					rec["error"])
			}
		})
	}
}

// aBlockDevice is the first block device /sys/class/block names that has a
// node under /dev, or "" when there is none.
func aBlockDevice() string {
	names, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return ""
	}
	for _, entry := range names {
		dev := filepath.Join("/dev", entry.Name())
		if _, err := os.Stat(dev); err == nil {
			return dev
		}
	}
	return ""
}

// R9: an exclusive open that was granted, or that found its path absent,
// is an answer at DEBUG, with "error" only for the absent path, which wraps
// fs.ErrNotExist; a call whose ctx ended opens nothing, logs nothing and
// gives its slot back.
func TestOsOpenExclusive(t *testing.T) {
	dir := t.TempDir()
	// O_EXCL without O_CREAT on a plain file is not checked by the kernel,
	// which takes it on a block device alone, so the open is granted.
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	client := NewLimitedOsClient(1)
	for _, tc := range []struct {
		name, path string
		absent     bool
	}{
		{"a granted open", file, false},
		{"an absent path", filepath.Join(dir, "missing"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureLogs(t)
			err := client.OpenExclusive(context.Background(), tc.path)
			if tc.absent != errors.Is(err, fs.ErrNotExist) || (!tc.absent && err != nil) {
				t.Fatalf("OpenExclusive(%s) = %v", tc.path, err)
			}
			if got := levelOf(t, capture, "os open exclusive"); got != "DEBUG" {
				t.Errorf("level %s, want DEBUG", got)
			}
			rec := capture.onlyMsg(t, "os open exclusive")
			if rec["path"] != tc.path || (rec["error"] != nil) != tc.absent {
				t.Errorf("record %v", rec)
			}
		})
	}
	capture := captureLogs(t)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.OpenExclusive(ended, file); !errors.Is(err, context.Canceled) {
		t.Errorf("OpenExclusive on an ended ctx = %v, want context.Canceled", err)
	}
	if got := len(capture.withMsg(t, "os open exclusive")); got != 0 {
		t.Errorf("%d records of an open that did not run", got)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := client.ReadFile(short, file); err != nil {
		t.Errorf("a call after the opens = %v: an open kept the only slot", err)
	}
}

// R9: a dial that connects is a read at DEBUG; a refused one is INFO.
func TestOsDialLevels(t *testing.T) {
	saved := dialHold
	dialHold = 0
	defer func() { dialHold = saved }()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	capture := captureLogs(t)
	client := NewLimitedOsClient(0)
	if err := client.DialTcp(context.Background(), addr, time.Second); err != nil {
		t.Fatalf("DialTcp: %v", err)
	}
	if got := levelOf(t, capture, "os dial"); got != "DEBUG" {
		t.Errorf("a dial that connected logs at %s, want DEBUG", got)
	}
	lis.Close()
	capture = captureLogs(t)
	if err := client.DialTcp(context.Background(), addr, time.Second); err == nil {
		t.Fatal("a dial to a closed port connected")
	}
	if got := levelOf(t, capture, "os dial"); got != "INFO" {
		t.Errorf("a refused dial logs at %s, want INFO", got)
	}
}

// R9: a command of the read list that succeeds logs at DEBUG; one that fails,
// and any command not on the list, at INFO.
func TestRunCommandLevel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	client := NewLimitedOsClient(0)
	for _, tc := range []struct {
		name, cmd, want string
		args            []string
	}{
		{"a read that answers", "stat", "DEBUG", []string{"--format", "%s", dir}},
		{"a read that fails", "stat", "INFO",
			[]string{"--format", "%s", filepath.Join(dir, "missing")}},
		{"a write", "mkdir", "INFO", []string{"-p", filepath.Join(dir, "d")}},
		{"a command not on the list", "true", "INFO", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureLogs(t)
			client.RunCommand(ctx, tc.cmd, tc.args, "")
			if got := levelOf(t, capture, "os command"); got != tc.want {
				t.Errorf("%s %v: level %s, want %s", tc.cmd, tc.args, got,
					tc.want)
			}
		})
	}
}

// R9: the read list of commandReadsOnly. Every form below the reads reads;
// every mutating form the dn and cn suites grep for (dnagent_test.sh and
// cnagent_test.sh, mutations) and every command the agents' fake nodes count
// as a mutation does not, nor does a read tool given a writing verb.
func TestCommandReadsOnly(t *testing.T) {
	reads := [][]string{
		{"dmsetup", "info", "--columns", "dnv-x"},
		{"dmsetup", "ls"},
		{"dmsetup", "table", "dnv-x"},
		{"dmsetup", "status", "--noflush", "dnv-x"},
		{"dmsetup", "deps", "dnv-x"},
		{"dmsetup", "version"},
		{"dmsetup", "targets"},
		{"/usr/sbin/dmsetup", "table", "dnv-x"},
		{"mdadm", "--examine", "--export", "--metadata=1.2", "/dev/x"},
		{"mdadm", "-E", "/dev/x"},
		{"mdadm", "--detail", "/dev/md/x"},
		{"mdadm", "-D", "/dev/md/x"},
		{"mdadm", "--query", "/dev/x"},
		{"mdadm", "--version"},
		{"mdadm", "--examine-bitmap", "/dev/x"},
		{"mdadm", "-X", "/dev/x"},
		{"mdadm", "--examine-badblocks", "/dev/x"},
		{"nvme", "list"},
		{"nvme", "list-subsys", "-o", "json"},
		{"nvme", "id-ctrl", "/dev/nvme0"},
		{"nvme", "id-ns", "/dev/nvme0n1"},
		{"nvme", "show-hostnqn"},
		{"nvme", "version"},
		{"nvme", "discover", "-d", "nvme3", "-o", "json"},
		{"nvme", "discover", "--device", "nvme3"},
		{"nvme", "discover", "--device=nvme3"},
		{"blockdev", "--getsize64", "/dev/x"},
		{"blockdev", "--getss", "--getsize64", "/dev/x"},
		{"lsblk", "--nodeps", "--noheadings", "--output", "MAJ:MIN", "/dev/x"},
		{"blkid", "-o", "value", "-s", "TYPE", "/dev/x"},
		{"stat", "-f", "-c", "%S %b", "/mnt"},
		{"stat", "--format", "%s", "/dev/x"},
		{"findmnt", "-n", "/mnt"},
		{"uname", "-r"},
	}
	for _, argv := range reads {
		if !commandReadsOnly(argv[0], argv[1:]) {
			t.Errorf("%v is not read-only", argv)
		}
	}
	writes := [][]string{
		// dnagent_test.sh, mutations
		{"blkdiscard", "--zeroout", "/dev/x"},
		{"dmsetup", "create", "dnv-x", "--table", "0 8 linear /dev/x 0"},
		{"dmsetup", "reload", "dnv-x"},
		{"dmsetup", "remove", "dnv-x"},
		{"dmsetup", "suspend", "dnv-x"},
		{"dmsetup", "resume", "dnv-x"},
		{"dmsetup", "clear", "dnv-x"},
		{"dmsetup", "message", "dnv-x", "0", "enable_hydration"},
		{"nvme", "connect", "--transport", "tcp"},
		{"nvme", "disconnect", "--nqn", "nqn.x"},
		{"mkdir", "-p", "/sys/kernel/config/nvmet/ports/1"},
		{"rmdir", "/sys/kernel/config/nvmet/ports/1"},
		{"ln", "-s", "/a", "/b"},
		{"rm", "-f", "/a"},
		// cnagent_test.sh, mutations
		{"mdadm", "--create", "/dev/md/x", "--metadata=1.2"},
		{"mdadm", "--assemble", "/dev/md/x", "--metadata=1.2"},
		{"mdadm", "/dev/md/x", "--add", "--failfast", "/dev/y"},
		{"mdadm", "/dev/md/x", "--fail", "/dev/y"},
		{"mdadm", "/dev/md/x", "--remove", "/dev/y"},
		{"mdadm", "--stop", "/dev/md/x"},
		{"mdadm", "--zero-superblock", "--metadata=1.2", "/dev/y"},
		{"mount", "/dev/x", "/mnt"},
		{"umount", "/mnt"},
		{"truncate", "-s", "1G", "/f"},
		{"losetup", "-f", "--show", "/f"},
		// the cn's own writes the fakes count
		{"thin_dump", "--metadata-snap", "-o", "/tmp/x", "/dev/y"},
		{"udevadm", "settle"},
		{"mkfs.xfs", "-q", "-f", "/dev/x"},
		{"mkfs.ext4", "-q", "-F", "/dev/x"},
		// a read tool given a writing verb, or none
		{"nvme", "discover", "-t", "tcp", "-a", "10.0.0.1", "-s", "8009"},
		{"nvme"},
		{"dmsetup"},
		{"blockdev", "--setro", "/dev/x"},
		{"blockdev", "--getsize64", "--setro", "/dev/x"},
		{"blockdev", "--flushbufs", "/dev/x"},
		{"hostname", "newname"},
	}
	for _, argv := range writes {
		if commandReadsOnly(argv[0], argv[1:]) {
			t.Errorf("%v reads as read-only", argv)
		}
	}
}

// R9: a record the level drops is not built: at Info a successful proto read
// allocates no more than the bare read and decode it does, so neither the
// record's attributes nor PbToLogValue's rendering of the message were made.
func TestDroppedReadRecordIsNotBuilt(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p")
	client := NewLimitedOsClient(0)
	msg := &pb.NvmeTrConf{TrType: "tcp", AdrFam: "ipv4", TrAddr: "10.0.0.1",
		TrSvcId: strings.Repeat("4", 64)}
	if err := client.WriteProto(ctx, path, msg); err != nil {
		t.Fatal(err)
	}
	bare := testing.AllocsPerRun(20, func() {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.Unmarshal(raw, &pb.NvmeTrConf{}); err != nil {
			t.Fatal(err)
		}
	})
	SetLogLevel(slog.LevelInfo)
	atInfo := testing.AllocsPerRun(20, func() {
		if err := client.ReadProto(ctx, path, &pb.NvmeTrConf{}); err != nil {
			t.Fatal(err)
		}
	})
	if atInfo > bare {
		t.Errorf("a proto read allocates %v times at Info, and the bare read "+
			"and decode %v: the dropped record was built", atInfo, bare)
	}
}
