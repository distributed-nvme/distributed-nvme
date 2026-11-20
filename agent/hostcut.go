package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The facts a controller node's cut of an nvmet connection rests on
// (cnagent.md CN33), read the one way for the cn agent's start check
// (cnagent.md CN-CM2) and the node check's host cut row (dnagent.md CM5), so
// the two never disagree.
const (
	// ProcMountsPath is where the mounts are read from: debugfs must be
	// mounted at DebugfsDir.
	ProcMountsPath = "/proc/mounts"
	// DebugfsDir is the mount point the kernel NVMe target's debug
	// directory (NvmetDebugRoot) lives under.
	DebugfsDir = "/sys/kernel/debug"
	// LockdownPath is the kernel's lockdown file: a kernel under lockdown
	// refuses to open an nvmet connection's "state" file, so it cannot end a
	// connection. A missing file is read as no lockdown; the file is a
	// securityfs file, missing also where securityfs is not mounted, and a
	// lockdown there shows only as a refused cut (cnagent.md CN33).
	LockdownPath = "/sys/kernel/security/lockdown"
)

// HostCutCauses reads whether this node's kernel can end an nvmet connection
// of its NVMe target: nvmet loaded (NvmetRoot there), debugfs mounted at
// DebugfsDir, nvmet's debug directory there (NvmetDebugRoot), which needs
// the kernel option CONFIG_NVME_TARGET_DEBUGFS and a kernel of 6.11 or
// newer, and no lockdown in force. It answers nil when the kernel can, and
// otherwise every cause it found, in that order. A read that does not answer
// is a cause, never a pass. The debug directory is judged only once nvmet is
// loaded and debugfs mounted: before that its absence says nothing more.
func HostCutCauses(ctx context.Context, oc common.OsClient) []string {
	var causes []string
	b := osBase{oc: oc}
	loaded, err := b.dirExists(ctx, NvmetRoot)
	switch {
	case err != nil:
		causes = append(causes, "nvmet not read: "+err.Error())
	case !loaded:
		causes = append(causes, "no "+NvmetRoot+": nvmet is not loaded, "+
			"or configfs is not mounted")
	}
	mounted, err := debugfsMounted(ctx, b)
	switch {
	case err != nil:
		causes = append(causes, "mounts not read: "+err.Error())
	case !mounted:
		causes = append(causes, "debugfs is not mounted at "+DebugfsDir)
	}
	if loaded && mounted {
		there, err := b.dirExists(ctx, NvmetDebugRoot)
		switch {
		case err != nil:
			causes = append(causes, NvmetDebugRoot+" not read: "+err.Error())
		case !there:
			causes = append(causes, "nvmet has no debug directory "+
				NvmetDebugRoot+" (it needs CONFIG_NVME_TARGET_DEBUGFS and "+
				"a kernel of 6.11 or newer)")
		}
	}
	mode, err := lockdownMode(ctx, b)
	switch {
	case err != nil:
		causes = append(causes, "lockdown not read: "+err.Error())
	case mode != "" && mode != "none":
		causes = append(causes, "lockdown is "+mode)
	}
	return causes
}

// debugfsMounted reports whether /proc/mounts holds a debugfs mount at
// DebugfsDir.
func debugfsMounted(ctx context.Context, b osBase) (bool, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	mounts, err := b.oc.ReadFile(cctx, ProcMountsPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", ProcMountsPath, err)
	}
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == DebugfsDir &&
			fields[2] == "debugfs" {
			return true, nil
		}
	}
	return false, nil
}

// LockdownMode is the lockdown mode in force on this node, as the start
// check reads it (lockdownMode): "" with no lockdown file, "none" with no
// lockdown. The cn agent's cut reads it to tell a node that cannot end a
// stray connection from one whose connection has not gone yet (cnagent.md
// CN33).
func (n *Nvmet) LockdownMode(ctx context.Context) (string, error) {
	return lockdownMode(ctx, n.osBase)
}

// lockdownMode is the mode the lockdown file marks as in force, in
// brackets: "none" of "[none] integrity confidentiality". A node with no
// lockdown file answers "", no lockdown; a file that marks no mode answers
// its text, quoted, which is no "none" either.
func lockdownMode(ctx context.Context, b osBase) (string, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	text, err := b.oc.ReadFile(cctx, LockdownPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", LockdownPath, err)
	}
	for _, word := range strings.Fields(text) {
		if strings.HasPrefix(word, "[") && strings.HasSuffix(word, "]") {
			return strings.Trim(word, "[]"), nil
		}
	}
	return fmt.Sprintf("%q", strings.TrimSpace(text)), nil
}
