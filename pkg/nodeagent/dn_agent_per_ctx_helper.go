package nodeagent

import (
	"context"

	"bytes"
	"os/exec"
	"strings"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)

type dnAgentPerCtxHelper struct {
	ctx context.Context
	logger *lib.Logger
}

func (pch *dnAgentPerCtxHelper) runOsCmd(
	name string,
	args []string,
	stdin string,
) (string, string, error) {
	pch.logger.Info("OsCmd name: [%v]", name)
	pch.logger.Info("OsCmd args: [%v]", args)
	pch.logger.Info("OsCmd stdin: [%v]")
	cmd := exec.CommandContext(pch.ctx, name, args)
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
	oc.logger.Info("OsCmd err: %v\n", err)
	oc.logger.Info("OsCmd stdout: %v\n", stdout)
	oc.logger.Info("OsCmd stderr: %v\n", stderr)
	return stdout, stderr, err
}

func (pch *dnAgentPerCtxHelper) getBlockDevSize(
	devPath string,
) (uint64, error) {
	name := "blockdev"
	args := []string{"--getsize64", devPath}
	stdout, _, err := pch.runOsCmd(name, args, "")
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseUint(stdout, 10, 64)
	return size, err
}

func (pch *dnAgentPerCtxHelper) close() {
}

func newDnAgentPerCtxHelper(ctx context.Context, reqId string) {
	logPrefix := fmt.Sprintf("dn_agent %s", reqId)
	return dnAgentPerCtxHelper{
		ctx: ctx,
		logger: &lib.Logger(logPrefix)
	}
}
