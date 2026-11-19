package agent

import (
	"context"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// HostNamePath is the file the kernel keeps the machine's host name in.
const HostNamePath = "/proc/sys/kernel/hostname"

// HostName reads the machine's host name for the size calls of both roles
// (dnagent.md SH34), trimmed of white space, through oc under the soft
// timeout. A read that fails gives "": the OsClient logs a read it ran, but
// not a wait for one of its slots that the soft timeout ended; the size call
// still answers, and the gateway refuses a create with no location over an
// empty name (architecture.md, Disk nodes).
func HostName(ctx context.Context, oc common.OsClient) string {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	data, err := oc.ReadFile(cctx, HostNamePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(data)
}
