package nodeagent

import (
	"bytes"
	"os/exec"
	"strings"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

type osCmd struct {
}

func (oc *osCmd) runOsCmd(
	pch *lib.PerCtxHelper,
	name string,
	args []string,
	stdin string,
) (string, string, error) {
	pch.Logger.Info("OsCmd name: [%v]", name)
	pch.Logger.Info("OsCmd args: [%v]", args)
	pch.Logger.Info("OsCmd stdin: [%v]")
	cmd := exec.CommandContext(pch.Ctx, name, args...)
	var stdoutBuilder strings.Builder
	var stderrBuilder strings.Builder
	cmd.Stdout = &stdoutBuilder
	cmd.Stderr = &stderrBuilder
	if stdin != "" {
		cmd.Stdin = bytes.NewBufferString(stdin)
	}
	err := cmd.Run()
	stdout := stdoutBuilder.String()
	stderr := stderrBuilder.String()
	pch.Logger.Info("OsCmd err: %v\n", err)
	pch.Logger.Info("OsCmd stdout: %v\n", stdout)
	pch.Logger.Info("OsCmd stderr: %v\n", stderr)
	return stdout, stderr, err
}

func (oc *osCmd) getBlockDevSize(
	pch *lib.PerCtxHelper,
	devPath string,
) (uint64, error) {
	name := "blockdev"
	args := []string{"--getsize64", devPath}
	stdout, _, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseUint(stdout, 10, 64)
	return size, err
}
