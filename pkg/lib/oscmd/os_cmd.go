package oscmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

const (
	nvmetPath    = "/sys/kernel/config/nvmet"
	waitInterval = 100 * time.Millisecond
	waitCnt      = 20
)

func pathExist(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func waitUntilExist(path string) error {
	for i := 0; i < waitCnt; i++ {
		exist, err := pathExist(path)
		if err != nil {
			return err
		}
		if exist {
			return nil
		}
		time.Sleep(waitInterval)
	}
	return fmt.Errorf("waitUntilExist timeout: %s", path)
}

func waitUntilNoExist(path string) error {
	for i := 0; i < waitCnt; i++ {
		exist, err := pathExist(path)
		if err != nil {
			return err
		}
		if !exist {
			return nil
		}
		time.Sleep(waitInterval)
	}
	return fmt.Errorf("waitUntilNoExist timeout: %s", path)
}

func createDir(path string) error {
	exists, err := pathExist(path)
	if err != nil {
		return err
	}
	if !exists {
		if err := os.Mkdir(path, 0755); err != nil {
			return err
		}
		if err := waitUntilExist(path); err != nil {
			return err
		}
	}
	return nil
}

func createLink(oldPath, newPath string) error {
	exists, err := pathExist(newPath)
	if err != nil {
		return err
	}
	if !exists {
		if err := os.Symlink(oldPath, newPath); err != nil {
			return err
		}
		if err := waitUntilExist(newPath); err != nil {
			return err
		}
	}
	return nil
}

func removeAny(path string) error {
	exists, err := pathExist(path)
	if err != nil {
		return err
	}
	if exists {
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := waitUntilNoExist(path); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path, data string) error {
	oldData, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	bData := []byte(data)
	if !bytes.Equal(bData, oldData) {
		err := os.WriteFile(path, bData, 0644)
		return err
	}
	return nil
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func nvmetPortPath(portNum uint32) string {
	return fmt.Sprintf("%s/ports/%s", nvmetPath, portNum)
}

func trTypePath(portNum uint32) string {
	return fmt.Sprintf("%s/addr_trtype", nvmetPortPath(portNum))
}

func adrFamPath(portNum uint32) string {
	return fmt.Sprintf("%s/addr_adrfam", nvmetPortPath(portNum))
}

func trAddrPath(portNum uint32) string {
	return fmt.Sprintf("%s/addr_traddr", nvmetPortPath(portNum))
}

func trSvcIdPath(portNum uint32) string {
	return fmt.Sprintf("%s/addr_trsvcid", nvmetPortPath(portNum))
}

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
	if err := createDir(nvmetPortPath(portNum)); err != nil {
		return err
	}

	if err := writeFile(trTypePath(portNum), trType); err != nil {
		return err
	}

	if err := writeFile(adrFamPath(portNum), trType); err != nil {
		return err
	}

	if err := writeFile(trAddrPath(portNum), trType); err != nil {
		return err
	}

	if err := writeFile(trSvcIdPath(portNum), trType); err != nil {
		return err
	}

	// FIXME: support addr_treq

	return nil
}

func NewOsCommand() *OsCommand {
	return &OsCommand{}
}
