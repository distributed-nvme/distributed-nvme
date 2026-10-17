package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// osBase is the shared plumbing of the OS wrappers
// (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`): one
// process-wide OsClient, and the soft timeout of
// architecture.md, Common validation, wrapped around every call (SH15;
// osclient.md, RunCommand, turns it into SIGTERM, then SIGKILL at the hard
// timeout).
type osBase struct {
	oc common.OsClient
}

func cmdCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		ctx, common.CmdSoftTimeout*time.Second)
}

// CmdCtx bounds one OS touch by the soft timeout
// (architecture.md, Common validation; SH15). The unexported
// cmdCtx stays; this is the same thing for role packages that call the
// OsClient directly (the thin_dump file read of thinbm.go, the dn
// disk-metadata block reads and writes of diskmeta.go).
func CmdCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return cmdCtx(ctx)
}

// Reported says whether the tool ran to completion and gave an answer.
// common.OsClient.RunCommand returns exitCode -1 together with a non-nil
// error when the process never reported — killed at the soft timeout, killed
// at the hard timeout, failed to start, ctx cancelled, semaphore refused —
// and exitCode > 0 when the tool ran and answered "no". Conflating the two
// would make a killed probe read as "the object does not exist": the removal
// is skipped, and nothing enumerates the object again.
//
// A killed command may still have completed in the kernel (the ioctl finishes
// regardless of the signal), so a caller that learns "did not answer" must
// re-probe rather than conclude anything.
func Reported(exitCode int, err error) bool {
	return err == nil || exitCode > 0
}

func reported(exitCode int, err error) bool {
	return Reported(exitCode, err)
}

// run executes one command under the soft timeout. It returns the raw
// results; callers decide whether a non-zero exit means "absent" or "failed".
func (b *osBase) run(
	ctx context.Context,
	name string,
	args ...string,
) (string, string, int, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	return b.oc.RunCommand(cctx, name, args, "")
}

// runProbe executes one command whose non-zero exit means "the object is not
// there". It splits the three outcomes the sweep depends on: (stdout, true,
// nil) the tool answered yes, ("", false, nil) the tool ran and answered no,
// and ("", false, err) the tool did not answer at all and the caller learned
// nothing.
func (b *osBase) runProbe(
	ctx context.Context,
	name string,
	args ...string,
) (string, bool, error) {
	stdout, stderr, exitCode, err := b.run(ctx, name, args...)
	if err != nil {
		if !reported(exitCode, err) {
			return "", false,
				cmdError(name, args, stdout, stderr, exitCode, err)
		}
		return "", false, nil
	}
	return stdout, true, nil
}

// runStdin executes one command with a stdin payload. dmsetup's --table is
// single-line only, so a multi-target table is fed through stdin instead
// (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`).
func (b *osBase) runStdin(
	ctx context.Context,
	stdin string,
	name string,
	args ...string,
) (string, string, int, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	return b.oc.RunCommand(cctx, name, args, stdin)
}

// runStdinOk is runOk with a stdin payload.
func (b *osBase) runStdinOk(
	ctx context.Context,
	stdin string,
	name string,
	args ...string,
) error {
	stdout, stderr, exitCode, err := b.runStdin(ctx, stdin, name, args...)
	if err != nil {
		return cmdError(name, args, stdout, stderr, exitCode, err)
	}
	return nil
}

// runOk executes one command and folds a non-zero exit into a CmdError whose
// text is the command output (DN19 error capture).
func (b *osBase) runOk(
	ctx context.Context,
	name string,
	args ...string,
) error {
	stdout, stderr, exitCode, err := b.run(ctx, name, args...)
	if err != nil {
		return cmdError(name, args, stdout, stderr, exitCode, err)
	}
	return nil
}

// Cmd is the exported form of osBase for role packages that must wrap a tool
// only *they* run — the cn role's mdadm wrapper (`md.go`), the CN base-state
// and clone-metadata tooling of `clonemeta.go` (tmpfs, `truncate`, `losetup`,
// `blkdiscard`) and the thin-provisioning-tools reader (`thinbm.go`), i.e. the
// file list of `cnagent.md`, Files. There is no LVM in that list, and none
// anywhere else in dnv ([D13], [D14]).
// By the split rule of `cnagent.md`, Scope and placement, those wrappers stay
// role code, but they still owe the
// SH15 soft-timeout discipline and the DN19 error capture, which is exactly
// what this type carries.
type Cmd struct {
	osBase
}

func NewCmd(oc common.OsClient) *Cmd {
	return &Cmd{osBase{oc: oc}}
}

// Run executes one command under the soft timeout and returns the raw results;
// the caller decides whether a non-zero exit means "absent" or "failed".
func (c *Cmd) Run(
	ctx context.Context,
	name string,
	args ...string,
) (string, string, int, error) {
	return c.run(ctx, name, args...)
}

// RunProbe is runProbe for role packages: a command whose non-zero exit means
// "absent", with "did not answer" kept apart from it.
func (c *Cmd) RunProbe(
	ctx context.Context,
	name string,
	args ...string,
) (string, bool, error) {
	return c.runProbe(ctx, name, args...)
}

// ListDir lists a sysfs directory for a role package, in the agent's own
// process (readDir); ok is false only when the directory does not exist, and
// an error means the listing did not answer. The nvmet configfs tree is never
// listed this way (SH18).
func (c *Cmd) ListDir(
	ctx context.Context,
	path string,
) ([]string, bool, error) {
	return c.readDir(ctx, path)
}

// ReadAttr reads a virtual-filesystem attribute for a role package under the
// "did not answer" rule: ok is false only for a genuine ENOENT.
func (c *Cmd) ReadAttr(
	ctx context.Context,
	path string,
) (string, bool, error) {
	return c.readAttrStrict(ctx, path)
}

// RunOk folds a non-zero exit into an error carrying the command output.
func (c *Cmd) RunOk(ctx context.Context, name string, args ...string) error {
	return c.runOk(ctx, name, args...)
}

// CmdError is the error of a command that failed (DN19 error capture): its
// text is the command line and the tool's own output, word for word, so the
// details of an ERROR row read what the tool said. It also carries the exit
// status and, for dmsetup, the errno the output names, so a caller branches
// on IsNoData, IsExists or IsBusy and never on the text.
type CmdError struct {
	// Name and Args are the command line.
	Name string
	Args []string
	// Output is the tool's trimmed stderr, else its trimmed stdout, else the
	// run's own error when the tool printed nothing. A whole-node dm listing
	// gives no stdout here: its stdout is the listing (dmListingAnswer).
	Output string
	// ExitCode is the tool's exit status, or -1 when it never reported
	// (Reported).
	ExitCode int
	// Errno is the errno a dmsetup refusal's output names (dmErrnoTexts). It
	// is 0 for any other tool, for an output that names none of them, and
	// for a run that never reported, which refused nothing.
	Errno syscall.Errno
}

// Error is the command line and the output: the text every caller logs and
// a failed row carries in its details.
func (e *CmdError) Error() string {
	return fmt.Sprintf("%s %s: %s",
		e.Name, strings.Join(e.Args, " "), e.Output)
}

// Unwrap gives the errno alone, so errors.Is with a syscall errno holds as
// well; the run's own error is not in the chain.
func (e *CmdError) Unwrap() error {
	if e.Errno == 0 {
		return nil
	}
	return e.Errno
}

// dmErrnoTexts is the one place that turns a tool's error words into an
// errno. A dmsetup refusal names, in strerror's words, the errno its ioctl
// returned, as in "device-mapper: message ioctl on <name>  failed: File
// exists": the C locale's words, the locale every command runs in
// (osclient.md, RunCommand). The match is a case-insensitive substring of the
// output, never of the command line, and the first row that matches names the
// errno. "already exists" reads as EEXIST too, for an output that words the
// refusal that way and not in strerror's words.
var dmErrnoTexts = []struct {
	text  string
	errno syscall.Errno
}{
	{"no data available", syscall.ENODATA},
	{"file exists", syscall.EEXIST},
	{"already exists", syscall.EEXIST},
	{"device or resource busy", syscall.EBUSY},
}

// dmErrno is the errno a dmsetup output names, 0 when it names none.
func dmErrno(output string) syscall.Errno {
	lower := strings.ToLower(output)
	for _, row := range dmErrnoTexts {
		if strings.Contains(lower, row.text) {
			return row.errno
		}
	}
	return 0
}

// cmdError builds the CmdError of a failed run. Only a dmsetup run that
// reported gets an errno: a run that was killed or never started refused
// nothing, whatever it printed first.
func cmdError(
	name string,
	args []string,
	stdout string,
	stderr string,
	exitCode int,
	err error,
) error {
	out := strings.TrimSpace(stderr)
	if out == "" {
		out = strings.TrimSpace(stdout)
	}
	if out == "" {
		out = err.Error()
	}
	cmdErr := &CmdError{
		Name:     name,
		Args:     append([]string(nil), args...),
		Output:   out,
		ExitCode: exitCode,
	}
	if name == "dmsetup" && reported(exitCode, err) {
		cmdErr.Errno = dmErrno(out)
	}
	return cmdErr
}

// IsNoData says whether err holds a dmsetup refusal with ENODATA: a thin
// table, or a create_snap, that names a thin id its pool does not hold.
func IsNoData(err error) bool {
	return cmdErrno(err) == syscall.ENODATA
}

// IsExists says whether err holds a dmsetup refusal with EEXIST: a
// create_thin or a create_snap of a thin id its pool already holds.
func IsExists(err error) bool {
	return cmdErrno(err) == syscall.EEXIST
}

// IsBusy says whether err holds a dmsetup refusal with EBUSY: a device that
// is still open, or a reserve_metadata_snap while the pool already holds a
// metadata snapshot.
func IsBusy(err error) bool {
	return cmdErrno(err) == syscall.EBUSY
}

// cmdErrno is the errno of the first CmdError in err's chain, 0 when the
// chain holds none.
func cmdErrno(err error) syscall.Errno {
	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) {
		return 0
	}
	return cmdErr.Errno
}

// readAttr reads a virtual-filesystem attribute and reports whether it
// exists. Read-back is whitespace-stripped: configfs normalizes and pads some
// attributes (SH17).
func (b *osBase) readAttr(
	ctx context.Context,
	path string,
) (string, bool, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	data, err := b.oc.ReadFile(cctx, path)
	if err != nil {
		return "", false, nil
	}
	return strings.TrimSpace(data), true, nil
}

// readAttrStrict is readAttr under the "did not answer" rule: only a genuine
// ENOENT is "absent", every other failure is an error. The enumerators of the
// sweep read through this one, because their caller's next move on an
// "absent" is a removal — an unreadable device_path must never make a
// subsystem look unowned. The probe-first writes keep readAttr, where a
// failed read just means "write the value".
func (b *osBase) readAttrStrict(
	ctx context.Context,
	path string,
) (string, bool, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	data, err := b.oc.ReadFile(cctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(data), true, nil
}

// writeAttr writes a virtual-filesystem attribute in place (SH18: WriteFile's
// atomic replace cannot work on configfs).
func (b *osBase) writeAttr(
	ctx context.Context,
	path string,
	value string,
) error {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	if err := b.oc.WriteFileDirect(cctx, path, value); err != nil {
		return fmt.Errorf("write %s = %q: %w", path, value, err)
	}
	return nil
}

// ensureAttr is the probe-first attribute write (SH16): read, compare
// whitespace-stripped, write only on a difference.
func (b *osBase) ensureAttr(
	ctx context.Context,
	path string,
	value string,
) (bool, error) {
	cur, ok, err := b.readAttr(ctx, path)
	if err != nil {
		return false, err
	}
	if ok && cur == strings.TrimSpace(value) {
		return false, nil
	}
	if err := b.writeAttr(ctx, path, value); err != nil {
		return false, err
	}
	return true, nil
}

// readDir lists a directory in the agent's own process (OsClient.ReadDir) on
// a ctx carrying the soft timeout (SH15). ok is false only when the directory
// does not exist; every other failure is an error naming the listing —
// "list <path>: <cause>" — and is never an absence: a listing that failed,
// read as an empty directory, would let a caller that enumerates the node
// skip every object in it.
func (b *osBase) readDir(
	ctx context.Context,
	path string,
) ([]string, bool, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	names, err := b.oc.ReadDir(cctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("list %s: %w", path, err)
	}
	return names, true, nil
}

// listDir returns the entries of a directory of the nvmet configfs tree by
// `ls`: that tree alone is listed by a child rather than in the agent's own
// process (SH18). ok is false when the directory does not exist. An `ls` that
// did NOT answer is an error, never "absent": RemoveSubsystem and
// RemovePortLink walk their children through this, and a killed listing must
// never read as an empty directory, or they would skip every object silently.
func (b *osBase) listDir(
	ctx context.Context,
	path string,
) ([]string, bool, error) {
	stdout, ok, err := b.runProbe(ctx, "ls", "-1", path)
	if err != nil || !ok {
		return nil, false, err
	}
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			out = append(out, name)
		}
	}
	return out, true, nil
}

// dirMtime reads a directory's modification time from the node in the
// agent's own process (OsClient.ReadMtime) on a ctx carrying the soft timeout
// (SH15), so an age can be judged from what the kernel says rather than from
// anything remembered (dnagent.md DN6). It reads the directory itself, never
// one of its entries, and is no listing (SH18). ok is false only when the
// directory does not exist; every other failure is an error naming the read
// — "mtime of <path>: <cause>" — and is never an absence, as listDir's split
// has it: a read that did not answer is not a directory that is gone.
func (b *osBase) dirMtime(
	ctx context.Context,
	path string,
) (time.Time, bool, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	mtime, err := b.oc.ReadMtime(cctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("mtime of %s: %w", path, err)
	}
	return mtime, true, nil
}

// dirExists says whether a directory of the nvmet configfs tree exists, by
// listDir's `ls` (SH18), with listDir's split: an `ls` that did not answer
// is an error.
func (b *osBase) dirExists(ctx context.Context, path string) (bool, error) {
	_, ok, err := b.listDir(ctx, path)
	return ok, err
}

func (b *osBase) mkdir(ctx context.Context, path string) error {
	return b.runOk(ctx, "mkdir", "-p", path)
}

func (b *osBase) rmdir(ctx context.Context, path string) error {
	return b.runOk(ctx, "rmdir", path)
}

func (b *osBase) symlink(ctx context.Context, target, link string) error {
	return b.runOk(ctx, "ln", "-s", target, link)
}

func (b *osBase) unlink(ctx context.Context, link string) error {
	return b.runOk(ctx, "rm", "-f", link)
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
