package oscmd

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type OsCommand struct {
}

func (oc *OsCommand) runOsCmd(
	pch *ctxhelper.PerCtxHelper,
	name string,
	args []string,
	stdin string,
) (string, string, error) {
	pch.Logger.Info("OsCommand name: [%v]", name)
	pch.Logger.Info("OsCommand args: [%v]", args)
	pch.Logger.Info("OsCommand stdin: [%v]")
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
	pch.Logger.Info("OsCommand err: %v\n", err)
	pch.Logger.Info("OsCommand stdout: %v\n", stdout)
	pch.Logger.Info("OsCommand stderr: %v\n", stderr)
	return stdout, stderr, err
}

func (oc *OsCommand) GetBlockDevSize(
	pch *ctxhelper.PerCtxHelper,
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

func (oc *OsCommand) CreateNvmetPort(
	pch *ctxhelper.PerCtxHelper,
	portNum uint32,
	trType string,
	adrFam string,
	trAddr string,
	trSvcId string,
	seqCh uint32,
) error {
	return nil
}

func NewOsCommand() *OsCommand {
	return &OsCommand{}
}
