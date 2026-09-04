package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// osBase is the shared plumbing of the OS wrappers (dnagent.md §2.8): one
// process-wide OsClient, and the §7 soft timeout wrapped around every call
// (SH15; osclient.md §4.2 turns it into SIGTERM, then SIGKILL at the hard
// timeout).
type osBase struct {
	oc common.OsClient
}

func cmdCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		ctx, common.CmdSoftTimeout*time.Second)
}

// CmdCtx bounds one OS touch by the §7 soft timeout (SH15). The unexported
// cmdCtx stays; this is the same thing for role packages that call the
// OsClient directly (the cn sysfs walk of leg.go).
func CmdCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return cmdCtx(ctx)
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

// runStdin executes one command with a stdin payload. dmsetup's --table is
// single-line only, so a multi-target table is fed through stdin instead
// (architecture.md Appendix A).
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
	stdout, stderr, _, err := b.runStdin(ctx, stdin, name, args...)
	if err != nil {
		return cmdError(name, args, stdout, stderr, err)
	}
	return nil
}

// runOk executes one command and folds a non-zero exit into an error whose
// text is the command output (DN19 error capture).
func (b *osBase) runOk(
	ctx context.Context,
	name string,
	args ...string,
) error {
	stdout, stderr, _, err := b.run(ctx, name, args...)
	if err != nil {
		return cmdError(name, args, stdout, stderr, err)
	}
	return nil
}

// Cmd is the exported form of osBase for role packages that must wrap a tool
// only *they* run — the cn role's mdadm wrapper (`md.go`), the CN base-state
// and clone-metadata tooling of `clonemeta.go` (tmpfs, `truncate`, `losetup`,
// `blkdiscard`) and the thin-provisioning-tools reader (`thinbm.go`), i.e. the
// `cnagent.md` §4.1 file list. There is no LVM in that list, and none anywhere
// else in dnv: [D14] removed the clone VG, LVM's last user (update_01.md U3).
// By the §1 split rule those wrappers stay role code, but they still owe the
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

// RunOk folds a non-zero exit into an error carrying the command output.
func (c *Cmd) RunOk(ctx context.Context, name string, args ...string) error {
	return c.runOk(ctx, name, args...)
}

// RunStdinOk is RunOk with a stdin payload.
func (c *Cmd) RunStdinOk(
	ctx context.Context,
	stdin string,
	name string,
	args ...string,
) error {
	return c.runStdinOk(ctx, stdin, name, args...)
}

func cmdError(
	name string,
	args []string,
	stdout string,
	stderr string,
	err error,
) error {
	out := strings.TrimSpace(stderr)
	if out == "" {
		out = strings.TrimSpace(stdout)
	}
	if out == "" {
		out = err.Error()
	}
	return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), out)
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

// listDir returns the entries of a directory; ok is false when the directory
// does not exist.
func (b *osBase) listDir(
	ctx context.Context,
	path string,
) ([]string, bool, error) {
	stdout, _, _, err := b.run(ctx, "ls", "-1", path)
	if err != nil {
		return nil, false, nil
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
