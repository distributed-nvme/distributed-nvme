package lib

import (
	"bytes"
	"os/exec"
	"strings"
	"strconv"
)

type OsCmd struct {
	logger *Logger
}

func (oc *OsCmd) runCmd(args []string, stdin string) (string, string, error){
	oc.logger.Info("runCmd args: %v", args)
	oc.logger.Info("runCmd stdin: %v", stdin)
	baseCmd := args[0]
	cmdArgs := args[1:]
	cmd := exec.Command(baseCmd, cmdArgs...)
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
	oc.logger.Info("runCmd err: %v\n", err)
	oc.logger.Info("runCmd stdout: %v\n", stdout)
	oc.logger.Info("runCmd stderr: %v\n", stderr)
	return stdout, stderr, err
}

func (oc *OsCmd) GetBlockDevSize(devPath string) (uint64, error) {
	args := []string{"blockdev", "--getsize64", devPath}
	stdout, _, err := oc.runCmd(args, "")
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseUint(stdout, 10, 64)
	return size, err
}

func NewOsCmd(logger *Logger) *OsCmd {
	return &OsCmd{
		logger: logger,
	}
}
