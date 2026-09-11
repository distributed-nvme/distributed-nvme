package common

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
)

// OsClient defines high-level primitives for executing OS commands,
// performing disk file I/O, and handling Protobuf binary serialization with
// context support.
//
// All OS command execution and disk file I/O in the codebase goes through an
// OsClient; os/exec and os.ReadFile/os.WriteFile are never called directly
// outside this file (osclient.md §1). That is what makes the logging rules of
// log.md R8.1/R8.2 enforceable, and every consumer unit-testable through
// FakeOsClient.
//
// One carve-out is recorded (osclient.md §4.5.1): the CN11
// leg health prober calls the raw helpers WriteBlockAt / ReadBlockDirectAt
// directly, because its IO may block indefinitely by design — a pathless leg
// queues IO forever — and must never hold one of the LimitedOsClient's
// semaphore slots, nor run under a lock, while it does. It logs its own
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

	// WriteFile creates or overwrites a file at the specified path with
	// string data using default file permissions. The replacement is atomic:
	// a temp file in the same directory is written, fsynced and renamed onto
	// path, so readers never observe a partial file (architecture.md §9.1).
	WriteFile(ctx context.Context, path string, data string) (err error)

	// WriteFileDirect writes data straight into the file at path with a
	// plain open/truncate/write/close — no temp file, no rename. It exists
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

	// WriteBlock writes data at byte offset and fdatasyncs the file
	// descriptor before returning.
	WriteBlock(ctx context.Context, path string, offset uint64, data []byte) (err error)

	// ReadProto loads a raw Protobuf binary file from disk and deserializes
	// it into a target message struct (target must be a non-nil pointer,
	// e.g. &pb.SyncupDnRequest{}).
	ReadProto(ctx context.Context, path string, target proto.Message) (err error)

	// WriteProto serializes a Protobuf message into wire binary format and
	// writes it directly to disk, with the same atomic replace as WriteFile.
	WriteProto(ctx context.Context, path string, msg proto.Message) (err error)
}

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
	if stdinInput != "" {
		cmd.Stdin = strings.NewReader(stdinInput)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// architecture.md §7: SIGTERM at the (caller-set) soft timeout,
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

	attrs := []any{
		slog.String("cmd", name),
		slog.Any("args", args),
		slog.String("stdin", stdinInput),
		slog.String("stdout", stdout.String()),
		slog.String("stderr", stderr.String()),
		slog.Int("exit_code", exitCode),
	}
	slog.InfoContext(ctx, "os command", appendErr(attrs, err)...)

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

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(rawData)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os read file", appendErr(attrs, err)...)

	return data, err
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

	err := os.WriteFile(path, []byte(data), 0o644)

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(data)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os write file direct", appendErr(attrs, err)...)

	return err
}

// ReadBlock / WriteBlock are the raw-device metadata path of
// architecture.md [D13]: buffered pread/pwrite plus an fdatasync on the write
// side. No O_DIRECT — the regions they touch are never part of any dm table
// and the agent is their only writer, so page-cache aliasing cannot occur —
// and never a shell-out to dd, whose uutils build silently mishandles
// iflag=/oflag=direct (dnagent_integtest.md §4).
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

	attrs := []any{
		slog.String("path", path),
		slog.Uint64("offset", offset),
		slog.Uint64("length", length),
	}
	slog.InfoContext(ctx, "os read block", appendErr(attrs, err)...)

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

// WriteBlockAt writes data at byte offset and fdatasyncs before returning: a
// plain O_WRONLY open (never O_CREATE — the target is a block device, which
// always exists at full size), one pwrite, one Sync, close. The fd is opened
// and closed per call; nothing is ever cached.
//
// It is the raw helper behind OsClient.WriteBlock. It takes no context, does
// no logging and holds no semaphore slot, so a caller that uses it directly
// MUST emit its own log record. The only sanctioned direct caller is the CN11
// leg health prober (osclient.md §4.5.1): its IO may block
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

// ReadBlockDirectAt reads exactly length bytes at byte offset with O_DIRECT,
// so the read is served by the device and not by the page cache: the §3.6 leg
// health probe writes a block through a device and must read it back *from the
// device*, and a buffered read of a just-written block would be answered from
// cache and observe no IO at all.
//
// offset and length MUST be multiples of 4096; the check happens before the
// open, so a misaligned caller never touches the device at all. The pread
// lands in a 4096-aligned scratch buffer and is copied out. A short read is an
// error, never a silently truncated buffer.
//
// Like WriteBlockAt it takes no context, does no logging and holds no OsClient
// semaphore slot; the caller logs. It may block for as long as the device
// queues IO — a pathless nvme multipath leg queues forever — which is
// precisely why it lives outside the OsClient: a wedged probe
// must not consume one of the DefaultOsClientLimit slots the node's teardown
// commands need. The only sanctioned direct caller is the CN11 leg health
// prober, and it must never run under a lock (osclient.md §4.5.1,
// cnagent.md CN1/CN11).
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
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT, 0)
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

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(rawData)),
		slog.Any("data", PbToLogValue(target)),
	}
	slog.InfoContext(ctx, "os read proto", appendErr(attrs, err)...)

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

// atomicWrite implements the temp-file + fsync + rename protocol of
// architecture.md §9.1: readers never observe a partial file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
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
	return os.Rename(tmpPath, path)
}
