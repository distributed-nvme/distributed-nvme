package common

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
