package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
)

// OsClient defines high-level primitives for executing OS commands, listing
// directories, reading a path's modification time or a block device's
// number, dialling a TCP address, performing disk file I/O, and handling
// Protobuf binary serialization with context support.
//
// All OS command execution, detached child start (StartDetached), directory
// listing, modification-time read, device-number read, TCP connect made to
// test whether an address answers (DialTcp) and disk file I/O in production
// code goes
// through an OsClient; os/exec and os.ReadFile/os.WriteFile are never called, and no
// directory is read, no path is stat'ed and no such test connect is made,
// directly outside this file (osclient.md, Scope and placement). The
// connections of the gRPC and etcd clients carry dnv's own calls and are
// outside this rule. That is what makes the logging rules
// of log.md R8.1, R8.2, R8.5, R8.6, R8.7, R8.8, R8.9 and R8.10 enforceable, and every consumer
// unit-testable through FakeOsClient.
//
// One carve-out is recorded (osclient.md, Exported raw helpers and the
// probe-IO carve-out): the CN11
// leg health prober calls the raw helpers WriteBlockAt / ReadBlockDirectAt
// directly, because its IO may block indefinitely by design — it waits
// without limit while no path of the leg can serve and one stays live — and
// must never hold one of the LimitedOsClient's semaphore slots, nor run under
// a lock, while it does. It logs its own
// records ("probe write block" / "probe read block direct") and is faked
// through its own LegProbeIO interface, so both properties above survive.
type OsClient interface {

	// RunCommand executes an operating system binary with the given
	// command-line arguments and optional stdin data.
	//
	// Inputs:
	//   - ctx: Controls command cancellation or execution timeout
	//     (e.g., context.WithTimeout).
	//   - name: The executable binary name or full path
	//     (e.g., "git", "grep", "/bin/sh").
	//   - args: Command-line parameters passed to the executable.
	//   - stdinInput: String payload passed via standard input stream
	//     (pass empty string "" if none).
	//
	// Outputs:
	//   - stdout: Captured standard output stream as a string.
	//   - stderr: Captured standard error stream as a string.
	//   - exitCode: Process exit status code (0 for success, non-zero for
	//     error, -1 if execution failed to start).
	//   - err: Go error interface indicating non-zero exit code or execution
	//     failure.
	RunCommand(ctx context.Context, name string, args []string, stdinInput string) (stdout string, stderr string, exitCode int, err error)

	// ReadFile loads the entire contents of a file specified by path into
	// memory.
	//
	// Outputs:
	//   - data: String content of the file.
	//   - err: nil on success, non-nil error if context is canceled, file is
	//     missing, or I/O fails.
	ReadFile(ctx context.Context, path string) (data string, err error)

	// ReadDir lists the directory at path in the caller's own process: every
	// name in it but "." and "..", sorted in byte order (the order `ls` prints
	// in the C locale), untrimmed, names that start with a dot included. It
	// opens no entry. A missing directory is an error wrapping
	// fs.ErrNotExist. Any other failure — a path that is not a directory, a
	// read that fails part-way through — is an error, and no names come back
	// with it.
	ReadDir(ctx context.Context, path string) (names []string, err error)

	// ReadMtime reads the modification time of the path itself in the
	// caller's own process, and nothing else about it: it follows no
	// symbolic link, so a link answers its own time. A missing path is an
	// error wrapping fs.ErrNotExist. Any other failure is an error, and the
	// time that comes back with an error is the zero time.
	ReadMtime(ctx context.Context, path string) (mtime time.Time, err error)

	// ReadDevNo reads the number of the block device at path in the
	// caller's own process, as "major:minor", and nothing else about it: it
	// follows symbolic links, so a link answers the number of the device it
	// leads to. A missing path is an error wrapping fs.ErrNotExist. A path
	// that is not a block device is an error, and so is any other failure;
	// the number that comes back with an error is "".
	ReadDevNo(ctx context.Context, path string) (devNo string, err error)

	// WriteFile creates or overwrites a file at the specified path with
	// string data using default file permissions. The replacement is atomic:
	// a temp file in the same directory is written, fsynced and renamed onto
	// path, so readers never observe a partial file, and the directory is
	// fsynced after the rename, so a replacement that returned nil survives a
	// crash (architecture.md, Common agent rules, Local store). A failed
	// directory fsync fails the write, with the new file already in place at
	// path.
	WriteFile(ctx context.Context, path string, data string) (err error)

	// WriteFileDirect writes data straight into the existing file at path
	// with a plain open/truncate/write/close — no temp file, no rename, no
	// create: a missing file is an error wrapping fs.ErrNotExist. It exists
	// for kernel virtual filesystems (nvmet configfs, sysfs), where the
	// atomic-replace protocol of WriteFile is impossible: configfs forbids
	// creating arbitrary files, so a temp file + rename can never succeed
	// there. Use WriteFile for every regular-file write; use
	// WriteFileDirect only for virtual-filesystem attribute writes
	// (dnagent.md SH18).
	WriteFileDirect(ctx context.Context, path string, data string) (err error)

	// ReadBlock reads exactly length bytes at byte offset from a block
	// device (or regular file). A short read is an error. Buffered IO —
	// callers use it only for regions no dm table references.
	ReadBlock(ctx context.Context, path string, offset uint64, length uint64) (data []byte, err error)

	// ReadBlockDirect reads exactly length bytes at byte offset from a
	// block device with O_DIRECT in the caller's own process, so no page
	// cache answers it: the bytes are those on the device now. offset and
	// length must be multiples of 4096 and length must not be zero, checked
	// before the device is opened. A short read is an error. The read
	// cannot be cut off once it has started: it waits for as long as the
	// device holds the IO (osclient.md, ReadBlockDirect).
	ReadBlockDirect(ctx context.Context, path string, offset uint64, length uint64) (data []byte, err error)

	// WriteBlock writes data at byte offset and fsyncs the file
	// descriptor before returning.
	WriteBlock(ctx context.Context, path string, offset uint64, data []byte) (err error)

	// DiscardRanges discards byte ranges of the block device at path in the
	// caller's own process: one open, then one BLKDISCARD ioctl per range,
	// in order, with ctx checked before each. It returns how many ranges it
	// discarded and the first error, after which it discards no more. One
	// "os discard ranges" record covers the whole call.
	DiscardRanges(ctx context.Context, path string, ranges []DiscardRange) (done int, err error)

	// DialTcp opens a TCP connection to address ("host:port") in the
	// caller's own process, waiting at most timeout for the peer to answer,
	// and closes it dialHold after it opened, on its own, having returned at
	// once. An address that did not answer within timeout, while ctx was
	// still live, is an error wrapping ErrDialTimeout; a refusal, and every
	// other failure, is an error that does not wrap it. No byte is sent.
	DialTcp(ctx context.Context, address string, timeout time.Duration) (err error)

	// StartDetached starts the OS binary name with args as a child in a
	// process group of its own, its stdout and stderr both written to a new
	// file it creates in dir (the system's temporary directory when dir is
	// ""), and returns at once, never waiting for the child to end: a child
	// that waits in the kernel holds no slot of the limit and keeps no
	// caller waiting. The returned Detached says when the child ended and
	// how, and names the output file, which the caller reads with ReadFile
	// and removes. ctx's end sends SIGKILL to the child's whole group, which
	// takes effect only once a wait in the kernel ends (osclient.md,
	// StartDetached).
	StartDetached(ctx context.Context, name string, args []string, dir string) (child *Detached, err error)

	// ReadProto loads a raw Protobuf binary file from disk and deserializes
	// it into a target message struct (target must be a non-nil pointer,
	// e.g. &pb.SyncupDnRequest{}).
	ReadProto(ctx context.Context, path string, target proto.Message) (err error)

	// WriteProto serializes a Protobuf message into wire binary format and
	// writes it directly to disk, with the same atomic replace as WriteFile.
	WriteProto(ctx context.Context, path string, msg proto.Message) (err error)
}

// ErrCommandNotFound is what a RunCommand error wraps when no binary of the
// name was found by PATH (osclient.md, RunCommand): a tool that is not
// installed, told apart without reading the error's text.
var ErrCommandNotFound = exec.ErrNotFound

// commandLocale is the locale setting RunCommand adds to every command's
// environment (osclient.md, RunCommand).
const commandLocale = "LC_ALL=C"

// LimitedOsClient is the concrete OsClient. It caps the total number of
// in-flight operations (all methods combined) at the limit given to
// NewLimitedOsClient, and logs every operation per log.md / osclient.md.
type LimitedOsClient struct {
	sem *semaphore.Weighted
}

var _ OsClient = (*LimitedOsClient)(nil)

// NewLimitedOsClient creates a LimitedOsClient. limit <= 0 selects
// DefaultOsClientLimit.
func NewLimitedOsClient(limit int64) *LimitedOsClient {
	if limit <= 0 {
		limit = DefaultOsClientLimit
	}
	return &LimitedOsClient{sem: semaphore.NewWeighted(limit)}
}

func appendErr(attrs []any, err error) []any {
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	return attrs
}

// pathReadLevel is the level of a file, proto, modification-time or
// device-number read or a directory listing (log.md R9): ReadLevel, but a
// path found absent is an answer, not a failure, so it is Debug too; a raw
// block read takes ReadLevel alone. Callers decide by that answer (an absent
// file reads as "no such object"), and a dn's background read finds a whole
// disk's partition file absent on every round.
func pathReadLevel(err error) slog.Level {
	if errors.Is(err, fs.ErrNotExist) {
		return slog.LevelDebug
	}
	return ReadLevel(err)
}

// devNoLevel is the level of an "os read devno" record: pathReadLevel's,
// and a path that holds no block device is an answer too, so it is Debug:
// the CSI node plugin tells a block volume from a mounted filesystem by it on
// every stats call Kubernetes makes (NodeGetVolumeStats (csi/node.go)).
func devNoLevel(err error) slog.Level {
	if errors.Is(err, syscall.ENOTBLK) {
		return slog.LevelDebug
	}
	return pathReadLevel(err)
}

// commandLevel is the level of an "os command" record (log.md R9): Debug for
// a command of the read list (commandReadsOnly) that succeeded, Info for any
// other command and for a read that failed or answered "no".
func commandLevel(name string, args []string, err error) slog.Level {
	if err == nil && commandReadsOnly(name, args) {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// commandReadsOnly is the read list of log.md R9: the commands, named with
// the argument that makes them reads, that change nothing on the node. A
// command not named here logs at Info, so a new command cannot hide below
// the default level by being left out.
//   - dmsetup info, ls, table, status, deps, version and targets;
//   - mdadm whose first option is an examine, detail, query or version
//     option (the device-first form, mdadm <md> --add, has --add first);
//   - nvme list, list-subsys, id-ctrl, id-ns, show-hostnqn and version, and
//     nvme discover over a kept controller (-d or --device; a discover by
//     address makes a controller);
//   - blockdev whose every option is a --get one;
//   - lsblk, blkid, stat, findmnt and uname, which never write.
func commandReadsOnly(name string, args []string) bool {
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	switch filepath.Base(name) {
	case "dmsetup":
		switch first {
		case "info", "ls", "table", "status", "deps", "version", "targets":
			return true
		}
	case "mdadm":
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				continue
			}
			switch arg {
			case "--examine", "-E", "--detail", "-D", "--query", "-Q",
				"--version", "-V", "--examine-bitmap", "-X",
				"--examine-badblocks":
				return true
			}
			return false
		}
	case "nvme":
		switch first {
		case "list", "list-subsys", "id-ctrl", "id-ns", "show-hostnqn",
			"version":
			return true
		case "discover":
			for _, arg := range args {
				if arg == "-d" || arg == "--device" ||
					strings.HasPrefix(arg, "--device=") {
					return true
				}
			}
		}
	case "blockdev":
		gets := 0
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				continue
			}
			if !strings.HasPrefix(arg, "--get") {
				return false
			}
			gets++
		}
		return gets > 0
	case "lsblk", "blkid", "stat", "findmnt", "uname":
		return true
	}
	return false
}

func (c *LimitedOsClient) RunCommand(
	ctx context.Context,
	name string,
	args []string,
	stdinInput string,
) (string, string, int, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", "", -1, err
	}
	defer c.sem.Release(1)

	cmd := exec.CommandContext(ctx, name, args...)
	// osclient.md, RunCommand: the C locale, whatever the caller's, so a
	// tool's output reads in the words its callers parse. The last value of
	// a repeated key is the one the command gets.
	cmd.Env = append(os.Environ(), commandLocale)
	if stdinInput != "" {
		cmd.Stdin = strings.NewReader(stdinInput)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// osclient.md, RunCommand: SIGTERM at the (caller-set) soft timeout,
	// SIGKILL at the hard timeout.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = time.Duration(CmdHardTimeout-CmdSoftTimeout) * time.Second

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	if level := commandLevel(name, args, err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("cmd", name),
			slog.Any("args", args),
			slog.String("stdin", stdinInput),
			slog.String("stdout", stdout.String()),
			slog.String("stderr", stderr.String()),
			slog.Int("exit_code", exitCode),
		}
		slog.Log(ctx, level, "os command", appendErr(attrs, err)...)
	}

	return stdout.String(), stderr.String(), exitCode, err
}

func (c *LimitedOsClient) ReadFile(
	ctx context.Context,
	path string,
) (string, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return "", err
	}

	rawData, err := os.ReadFile(path)
	data := string(rawData)

	if level := pathReadLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("path", path),
			slog.Int("size", len(rawData)),
			slog.String("data", TruncForLog(data)),
		}
		slog.Log(ctx, level, "os read file", appendErr(attrs, err)...)
	}

	return data, err
}

func (c *LimitedOsClient) ReadDir(
	ctx context.Context,
	path string,
) ([]string, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	names, err := readDirNames(path)

	if level := pathReadLevel(err); LogEnabled(ctx, level) {
		// log.md R8: every name, never truncated, and an array even when
		// the listing returned none.
		logged := names
		if logged == nil {
			logged = []string{}
		}
		attrs := []any{
			slog.String("path", path),
			slog.Int("count", len(names)),
			slog.Any("names", logged),
		}
		slog.Log(ctx, level, "os read dir", appendErr(attrs, err)...)
	}

	return names, err
}

func (c *LimitedOsClient) ReadMtime(
	ctx context.Context,
	path string,
) (time.Time, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return time.Time{}, err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}

	// osclient.md, ReadMtime: the path itself, never what a link names.
	var mtime time.Time
	info, err := os.Lstat(path)
	if err == nil {
		mtime = info.ModTime()
	}

	if level := pathReadLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("path", path),
			slog.Time("mtime", mtime),
		}
		slog.Log(ctx, level, "os read mtime", appendErr(attrs, err)...)
	}

	return mtime, err
}

func (c *LimitedOsClient) ReadDevNo(
	ctx context.Context,
	path string,
) (string, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return "", err
	}

	devNo, err := readDevNo(path)

	if level := devNoLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("path", path),
			slog.String("devno", devNo),
		}
		slog.Log(ctx, level, "os read devno", appendErr(attrs, err)...)
	}

	return devNo, err
}

// readDevNo is the read behind ReadDevNo (osclient.md, ReadDevNo): the stat
// follows links, as the /dev/disk/by-* names of a disk are links, and a
// path that is not a block device fails with ENOTBLK, which is no "not
// exist".
func readDevNo(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeDevice == 0 ||
		info.Mode()&os.ModeCharDevice != 0 || !ok {
		return "", &os.PathError{Op: "stat", Path: path, Err: syscall.ENOTBLK}
	}
	return devNoString(uint64(st.Rdev)), nil
}

// devNoString renders a device number as the "major:minor" that sysfs's
// "dev" files and dm tables print, decoding Linux's dev_t as the C
// library's major and minor do: the major is bits 8 to 19 and 44 to 63, the
// minor bits 0 to 7 and 20 to 43.
func devNoString(dev uint64) string {
	major := (dev&0x00000000000fff00)>>8 | (dev&0xfffff00000000000)>>32
	minor := dev&0x00000000000000ff | (dev&0x00000ffffff00000)>>12
	return fmt.Sprintf("%d:%d", major, minor)
}

// dirNames is the read of every name behind readDirNames. It is package
// state so a test can make a read fail part-way; production never touches it.
var dirNames = func(f *os.File) ([]string, error) { return f.Readdirnames(-1) }

// readDirNames lists the directory at path (osclient.md, ReadDir). The open
// takes O_DIRECTORY, so a path that is not a directory fails there with
// ENOTDIR — a FIFO among them, whose plain open would wait for a writer —
// and a missing one with ENOENT, which keeps fs.ErrNotExist in the error's
// chain. The read takes the names alone: it opens and stats no entry. A read
// that fails part-way returns no names, because a part of a listing read as
// the whole would make a present entry read as absent.
func readDirNames(path string) ([]string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := dirNames(f)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func (c *LimitedOsClient) WriteFile(
	ctx context.Context,
	path string,
	data string,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	err := atomicWrite(path, []byte(data))

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(data)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os write file", appendErr(attrs, err)...)

	return err
}

func (c *LimitedOsClient) WriteFileDirect(
	ctx context.Context,
	path string,
	data string,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	err := writeExisting(path, []byte(data))

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(data)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os write file direct", appendErr(attrs, err)...)

	return err
}

// writeExisting writes data into the file at path, which must exist: it opens
// it write-only and truncated, never with O_CREATE (osclient.md, ReadFile /
// WriteFile / WriteFileDirect). configfs and sysfs have no create operation,
// so a create only ever worked on a path that left them for a real file
// system, where it made a file; a missing attribute is ENOENT. The write's
// error wins over the close's.
func writeExisting(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadBlock / WriteBlock are the raw-device metadata path of
// architecture.md [D13]: buffered pread/pwrite plus an fsync on the write
// side. No O_DIRECT — of the regions they touch only a clone-metadata slot is
// ever part of a dm table, and there the agent only ever WRITES, before the
// slot's wrapper dm-linear exists, so page-cache aliasing cannot occur
// (osclient.md, ReadBlock / WriteBlock) — and never a shell-out to dd, whose
// uutils build silently mishandles iflag=/oflag=direct (dnagent_integtest.md,
// Assumptions and preflight checks).
func (c *LimitedOsClient) ReadBlock(
	ctx context.Context,
	path string,
	offset uint64,
	length uint64,
) ([]byte, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := readBlockAt(path, offset, length)

	if level := ReadLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("path", path),
			slog.Uint64("offset", offset),
			slog.Uint64("length", length),
		}
		slog.Log(ctx, level, "os read block", appendErr(attrs, err)...)
	}

	return data, err
}

// ReadBlockDirect is the O_DIRECT twin of ReadBlock for metadata another
// writer puts on the device with its own IO, past any page cache: the md
// superblock area of a member, which the kernel writes with bios of its own,
// so a buffered read can answer an old copy (osclient.md, ReadBlockDirect).
// It runs the same body as the probe's ReadBlockDirectAt, but as an OsClient
// operation: under one semaphore slot and with its "os read block direct"
// record.
func (c *LimitedOsClient) ReadBlockDirect(
	ctx context.Context,
	path string,
	offset uint64,
	length uint64,
) ([]byte, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := ReadBlockDirectAt(path, offset, length)

	if level := ReadLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("path", path),
			slog.Uint64("offset", offset),
			slog.Uint64("length", length),
		}
		slog.Log(ctx, level, "os read block direct", appendErr(attrs, err)...)
	}

	return data, err
}

func readBlockAt(path string, offset uint64, length uint64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound-check before allocating. os.Stat reports 0 for a block device,
	// so the size comes from a seek to the end — which works for both block
	// devices and regular files. Without this a nonsense length (a corrupt
	// on-disk length field, say) would be a multi-gigabyte allocation before
	// the read could fail.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if offset+length > uint64(size) || offset+length < offset {
		return nil, fmt.Errorf(
			"short read: %s offset=%d length=%d size=%d",
			path, offset, length, size)
	}
	data := make([]byte, length)
	// A short read is still an error, never a silent truncation.
	if _, err := io.ReadFull(io.NewSectionReader(
		f, int64(offset), int64(length)), data); err != nil {
		return nil, err
	}
	return data, nil
}

// DiscardRange is one byte range of OsClient.DiscardRanges.
type DiscardRange struct {
	Offset uint64
	Length uint64
}

// discardLogRanges bounds the ranges an "os discard ranges" record lists:
// a fragmented clone source can bring hundreds of thousands, and the record
// carries their count whatever the bound.
const discardLogRanges = 64

// blkDiscardIoctl is BLKDISCARD, _IO(0x12, 119): discard the [offset,
// length) byte range its argument holds.
const blkDiscardIoctl = 0x1277

// discardOpenFlags open the device for DiscardRanges write-only and not
// exclusively, as `blkdiscard --force` does: the dm-clone a piece marks is
// held by the devices above it while it serves, and its caller pauses them
// instead (agent.ApplySkipRanges; osclient.md, DiscardRanges).
const discardOpenFlags = os.O_WRONLY

// discardArg is BLKDISCARD's argument for one range: its start, then its
// length, in bytes.
func discardArg(r DiscardRange) [2]uint64 {
	return [2]uint64{r.Offset, r.Length}
}

// blkDiscard sends one BLKDISCARD on an open device. A variable, so the
// package's tests can run DiscardRanges over a regular file.
var blkDiscard = func(f *os.File, r DiscardRange) error {
	arg := discardArg(r)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		blkDiscardIoctl, uintptr(unsafe.Pointer(&arg[0]))); errno != 0 {
		return fmt.Errorf("BLKDISCARD %s offset=%d length=%d: %w",
			f.Name(), r.Offset, r.Length, errno)
	}
	return nil
}

func (c *LimitedOsClient) DiscardRanges(
	ctx context.Context,
	path string,
	ranges []DiscardRange,
) (int, error) {
	// The wait for a slot is bounded by the soft timeout, so a caller whose
	// ctx has no deadline waits for one no longer than a command's caller
	// does (osclient.md, DiscardRanges).
	actx, cancel := context.WithTimeout(ctx, discardSoftTimeout)
	err := c.sem.Acquire(actx, 1)
	cancel()
	if err != nil {
		return 0, err
	}
	done, err := discardRanges(ctx, path, ranges)
	c.sem.Release(1)
	logged := ranges
	if len(logged) > discardLogRanges {
		logged = logged[:discardLogRanges]
	}
	pairs := make([][2]uint64, len(logged))
	for i, r := range logged {
		pairs[i] = discardArg(r)
	}
	attrs := []any{
		slog.String("path", path),
		slog.Int("range_cnt", len(ranges)),
		slog.Int("done", done),
		slog.Any("ranges", pairs),
	}
	slog.InfoContext(ctx, "os discard ranges", appendErr(attrs, err)...)
	return done, err
}

// discardSoftTimeout bounds DiscardRanges' wait for a semaphore slot, as
// CmdSoftTimeout bounds a command's caller. A variable, so the package's
// tests need not wait it out.
var discardSoftTimeout = CmdSoftTimeout * time.Second

// discardRanges is DiscardRanges without the slot and the record. It sends
// one BLKDISCARD per range, in order, ctx read before each, so a cancelled
// caller sends no further range; and it waits for each ioctl to return,
// however long the kernel takes, so that when it returns no discard it sent
// is still in the kernel: its caller (agent.ApplySkipRanges) resumes the
// writes to the dm-clone only then, and a discard that marked its regions
// after that resume could let a copy in flight land over a write.
func discardRanges(
	ctx context.Context,
	path string,
	ranges []DiscardRange,
) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, discardOpenFlags, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	for i, r := range ranges {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if err := blkDiscard(f, r); err != nil {
			return i, err
		}
	}
	return len(ranges), nil
}

// ErrDialTimeout is what a DialTcp error wraps when the address did not
// answer within the timeout: the one outcome its caller decides by
// (osclient.md, DialTcp), told apart without reading the error's text.
var ErrDialTimeout = errors.New("timed out")

// dialHold is how long DialTcp keeps a connection it opened before it closes
// it (osclient.md, DialTcp): nvmet-tcp logs an error for a connection that
// closes before its queue is set up, and one held this long closes in
// silence. A variable, so the package's tests can see the close.
var dialHold = 200 * time.Millisecond

func (c *LimitedOsClient) DialTcp(
	ctx context.Context,
	address string,
	timeout time.Duration,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		c.sem.Release(1)
		return err
	}
	start := time.Now()
	conn, err := dialTcp(ctx, address, timeout)
	took := time.Since(start)
	c.sem.Release(1)
	if err == nil {
		go holdThenClose(conn, dialHold)
	}
	if level := ReadLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("address", address),
			slog.Int64("timeout_ms", timeout.Milliseconds()),
			slog.Int64("duration_ms", took.Milliseconds()),
		}
		slog.Log(ctx, level, "os dial", appendErr(attrs, err)...)
	}
	return err
}

// dialTcp is DialTcp without the slot, the hold and the record. A dial whose
// own timeout ended it, with ctx still live, wraps ErrDialTimeout; one that
// ctx ended is ctx's, whatever the dialer says, so a caller that went away
// never marks an address dead.
func dialTcp(
	ctx context.Context,
	address string,
	timeout time.Duration,
) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", address, ctx.Err())
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil, fmt.Errorf("dial tcp %s: %w after %s", address,
			ErrDialTimeout, timeout)
	}
	return nil, err
}

// holdThenClose closes conn once hold has passed. It runs on its own, so no
// caller waits for the hold, and it holds no semaphore slot.
func holdThenClose(conn net.Conn, hold time.Duration) {
	time.Sleep(hold)
	conn.Close()
}

// Detached is a child StartDetached started: its process id, which is also
// its process group's, the file its stdout and stderr go to, and, once Done
// is closed, how it ended.
type Detached struct {
	Pid    int
	Output string

	done     chan struct{}
	exitCode int
	err      error
}

// NewDetached builds a Detached for a child pid writing to output, and the
// function that ends it with an exit code and an error, which closes Done:
// what StartDetached's child waiter calls, and what a test double gives a
// child it models.
func NewDetached(pid int, output string) (*Detached, func(int, error)) {
	d := &Detached{Pid: pid, Output: output, done: make(chan struct{})}
	var once sync.Once
	return d, func(exitCode int, err error) {
		once.Do(func() {
			d.exitCode, d.err = exitCode, err
			close(d.done)
		})
	}
}

// Done is closed once the child has ended.
func (d *Detached) Done() <-chan struct{} { return d.done }

// Exit is how the child ended, read once Done is closed, in RunCommand's
// terms: the exit code, -1 for a child a signal ended, and an error for any
// end but a zero exit.
func (d *Detached) Exit() (int, error) {
	<-d.done
	return d.exitCode, d.err
}

func (c *LimitedOsClient) StartDetached(
	ctx context.Context,
	name string,
	args []string,
	dir string,
) (*Detached, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	child, err := startDetached(ctx, name, args, dir)
	c.sem.Release(1)
	pid, output := 0, ""
	if child != nil {
		pid, output = child.Pid, child.Output
	}
	attrs := []any{
		slog.String("cmd", name),
		slog.Any("args", args),
		slog.Int("pid", pid),
		slog.String("output", output),
	}
	slog.InfoContext(ctx, "os start detached", appendErr(attrs, err)...)
	return child, err
}

// startDetached is StartDetached without the slot and the record. The child
// gets the C locale, as RunCommand's does, and no stdin. Its waiter runs on
// its own and is the only one that waits: a child stuck in the kernel keeps
// that goroutine, never the caller. ctx's end kills the child's group, since
// what it ran can have children of its own (a shell's).
func startDetached(
	ctx context.Context,
	name string,
	args []string,
	dir string,
) (*Detached, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := os.CreateTemp(dir, "dnv-detached-*.out")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), commandLocale)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err := cmd.Start(); err != nil {
		out.Close()
		os.Remove(out.Name())
		return nil, err
	}
	// The child holds its own copy of the file; the parent's is closed now,
	// so a stuck child keeps nothing of the caller's open.
	out.Close()
	child, end := NewDetached(cmd.Process.Pid, out.Name())
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
		}
		end(exitCode, err)
	}()
	return child, nil
}

func (c *LimitedOsClient) WriteBlock(
	ctx context.Context,
	path string,
	offset uint64,
	data []byte,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	err := WriteBlockAt(path, offset, data)

	attrs := []any{
		slog.String("path", path),
		slog.Uint64("offset", offset),
		slog.Int("length", len(data)),
	}
	slog.InfoContext(ctx, "os write block", appendErr(attrs, err)...)

	return err
}

// WriteBlockAt writes data at byte offset and fsyncs before returning: a
// plain O_WRONLY open (never O_CREATE — the target is a block device, which
// always exists at full size), one pwrite, one Sync, close. The fd is opened
// and closed per call; nothing is ever cached.
//
// It is the raw helper behind OsClient.WriteBlock. It takes no context, does
// no logging and holds no semaphore slot, so a caller that uses it directly
// MUST emit its own log record. The only sanctioned direct caller is the CN11
// leg health prober (osclient.md, Exported raw helpers and the probe-IO
// carve-out): its IO may block
// for as long as the device queues IO, and it must never occupy an OsClient
// slot — nor run under a lock — while it does (cnagent.md CN1/CN11).
func WriteBlockAt(path string, offset uint64, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, int64(offset)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// rawOpen is the open(2) behind ReadBlockDirectAt. It is package state so a
// test can swap the seam and read the flags of the descriptor it returns;
// production never touches it.
var rawOpen = syscall.Open

// ReadBlockDirectAt reads exactly length bytes at byte offset with O_DIRECT,
// so the read is served by the device and not by the page cache: the leg
// health probe (architecture.md, Group on-leg layout: meta region, data
// region, health block) writes a block through a device and must read it back
// *from the device*, and a buffered read of a just-written block would be
// answered from cache and observe no IO at all.
//
// offset and length MUST be multiples of 4096; the check happens before the
// open, so a misaligned caller never touches the device at all. The pread
// lands in a 4096-aligned scratch buffer and is copied out. A short read is an
// error, never a silently truncated buffer.
//
// Like WriteBlockAt it takes no context, does no logging and holds no OsClient
// semaphore slot; the caller logs. It may block for as long as the device
// queues IO — without limit while no path of an nvme multipath leg can serve
// and one stays live — which is precisely why it lives outside the OsClient: a
// wedged probe must not consume one of the DefaultOsClientLimit slots the
// node's teardown commands need. The only sanctioned direct caller is the CN11
// leg health prober, and it must never run under a lock (osclient.md, Exported
// raw helpers and the probe-IO carve-out; cnagent.md CN1/CN11).
//
// The descriptor is opened O_CLOEXEC, as os.OpenFile opens every one;
// syscall.Open does not add it. The read can block for as long as the device
// queues IO, and every command the agent forks meanwhile would otherwise
// inherit the descriptor: one that outlives the probe — an nvme disconnect
// stalled on its target, say — keeps the leg wrapper open, and the teardown's
// dmsetup remove fails EBUSY until that command exits.
func ReadBlockDirectAt(
	path string,
	offset uint64,
	length uint64,
) ([]byte, error) {
	const align = 4096
	if offset%align != 0 || length%align != 0 || length == 0 {
		return nil, fmt.Errorf(
			"read block direct: %s offset=%d length=%d not %d-aligned",
			path, offset, length, align)
	}
	fd, err := rawOpen(
		path, syscall.O_RDONLY|syscall.O_DIRECT|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)
	raw := make([]byte, length+align)
	shift := align - (uint64(uintptr(unsafe.Pointer(&raw[0]))) % align)
	if shift == align {
		shift = 0
	}
	buf := raw[shift : shift+length]
	n, err := syscall.Pread(fd, buf, int64(offset))
	if err != nil {
		return nil, &os.PathError{Op: "pread", Path: path, Err: err}
	}
	if uint64(n) != length {
		return nil, fmt.Errorf(
			"short read: %s offset=%d length=%d got=%d",
			path, offset, length, n)
	}
	data := make([]byte, length)
	copy(data, buf)
	return data, nil
}

func (c *LimitedOsClient) ReadProto(
	ctx context.Context,
	path string,
	target proto.Message,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	rawData, err := os.ReadFile(path)
	if err == nil {
		err = proto.Unmarshal(rawData, target)
	}

	if level := pathReadLevel(err); LogEnabled(ctx, level) {
		attrs := []any{
			slog.String("path", path),
			slog.Int("size", len(rawData)),
			slog.Any("data", PbToLogValue(target)),
		}
		slog.Log(ctx, level, "os read proto", appendErr(attrs, err)...)
	}

	return err
}

func (c *LimitedOsClient) WriteProto(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	rawData, err := proto.Marshal(msg)
	if err == nil {
		err = atomicWrite(path, rawData)
	}

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(rawData)),
		slog.Any("data", PbToLogValue(msg)),
	}
	slog.InfoContext(ctx, "os write proto", appendErr(attrs, err)...)

	return err
}

// syncDir fsyncs a directory, which is what makes a rename inside it
// durable. It is package state so a test can swap the seam; production never
// touches it.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// AtomicWriteTmpInfix is what atomicWrite puts between the name of the file it
// replaces and the random suffix of the temp file it writes beside it:
// `{name}.tmp-{random}`. Every failure path of atomicWrite removes that file,
// the removal's own error aside, so what leaves one behind is a removal that
// fails, a process that dies between creating it and renaming it, or a
// machine crash that undoes a rename or a removal not yet durable. The
// agents' local store never reads such a leftover: it deletes it at startup
// (dnagent.md SH6).
const AtomicWriteTmpInfix = ".tmp-"

// atomicWrite implements the temp-file + fsync + rename + directory-fsync
// protocol of architecture.md, Common agent rules, Local store: readers
// never observe a partial file, and
// a write that returned nil survives a crash. The rename is a change to the
// directory, so until the directory itself is fsynced a crash can undo it —
// bring the old file back or, for a first write, leave none — however durable
// the temp file's own fsync made the bytes. A failed directory fsync fails the
// write, with the new file already in place at path.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+AtomicWriteTmpInfix+"*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// No-op after a successful rename (ENOENT is ignored); cleans up on
	// every failure path.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncDir(dir)
}
