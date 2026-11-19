package csi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// What the node plugin reads of the node's kernel at its start (csi.md
// CSI13), as sysfs shows it from the pod: the files say what is loaded or
// built in, and nothing of what the kernel could load.
const (
	multipathParam = "/sys/module/nvme_core/parameters/multipath"
	nvmeCoreModule = "/sys/module/nvme_core"
	fabricsDevice  = "/sys/class/misc/nvme-fabrics/dev"
)

// checkHostKernel refuses a node on which no path the plugin makes can
// serve a mount through a failover, with one error that names every fact it
// read wrong and its fix: native NVMe multipath off, or not built, since
// without it a path's loss takes the device the mount uses; and no NVMe
// over Fabrics device, through which every connect goes and which no
// connect loads. The NVMe/TCP module is not read: the kernel loads it at
// the first connect, as the agents' check counts it (dnagent.md CM5). The
// check loads nothing.
func checkHostKernel(ctx context.Context, oc common.OsClient) error {
	var problems []string
	unread := func(p string, err error) {
		problems = append(problems, fmt.Sprintf("%s cannot be read: %v", p, err))
	}
	mp, err := oc.ReadFile(ctx, multipathParam)
	switch {
	case err == nil:
		if v := strings.TrimSpace(mp); v != "Y" {
			problems = append(problems, fmt.Sprintf("native NVMe multipath is "+
				"off (%s reads %q): boot with nvme_core.multipath=Y",
				multipathParam, v))
		}
	case !errors.Is(err, fs.ErrNotExist):
		unread(multipathParam, err)
	default:
		// nvme_core has the parameter only when native multipath is built
		// in; with nvme_core not loaded at all, the fabrics device below is
		// missing too, and its fix loads nvme_core.
		_, err := oc.ReadDir(ctx, nvmeCoreModule)
		switch {
		case err == nil:
			problems = append(problems, fmt.Sprintf("native NVMe multipath is "+
				"not built into this kernel (no %s): run a kernel built with "+
				"CONFIG_NVME_MULTIPATH", multipathParam))
		case !errors.Is(err, fs.ErrNotExist):
			unread(nvmeCoreModule, err)
		}
	}
	if _, err := oc.ReadFile(ctx, fabricsDevice); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("the NVMe over Fabrics "+
				"device is not there (no %s): run `modprobe nvme-tcp`, which "+
				"loads nvme-fabrics with it, and name nvme-tcp in a file under "+
				"/etc/modules-load.d so it loads at boot", fabricsDevice))
		} else {
			unread(fabricsDevice, err)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the node plugin cannot serve on this node: %s",
			strings.Join(problems, "; "))
	}
	return nil
}
