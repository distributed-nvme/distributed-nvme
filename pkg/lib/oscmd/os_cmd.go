package oscmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

const (
	nvmetPath    = "/sys/kernel/config/nvmet"
	waitInterval = 100 * time.Millisecond
	waitCnt      = 20
	dmNotExist   = "Device does not exist"
	dmEmpty      = "No devices found"
)

func byteToSector(inp uint64) uint64 {
	return inp / 512
}

func sectorToByte(inp uint64) uint64 {
	return inp * 512
}

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

func nvmetPortPath(portNum string) string {
	return fmt.Sprintf("%s/ports/%s", nvmetPath, portNum)
}

func nvmetTrTypePath(portNum string) string {
	return fmt.Sprintf("%s/addr_trtype", nvmetPortPath(portNum))
}

func nvmetAdrFamPath(portNum string) string {
	return fmt.Sprintf("%s/addr_adrfam", nvmetPortPath(portNum))
}

func nvmetTrAddrPath(portNum string) string {
	return fmt.Sprintf("%s/addr_traddr", nvmetPortPath(portNum))
}

func nvmetTrSvcIdPath(portNum string) string {
	return fmt.Sprintf("%s/addr_trsvcid", nvmetPortPath(portNum))
}

func nvmetSubsysPath(nqn string) string {
	return fmt.Sprintf("%s/subsystems/%s", nvmetPath, nqn)
}

func nvmetCntlidMinPath(nqn string) string {
	return fmt.Sprintf("%s/attr_cntlid_min", nvmetSubsysPath(nqn))
}

func nvmetCntlidMaxPath(nqn string) string {
	return fmt.Sprintf("%s/attr_cntlid_max", nvmetSubsysPath(nqn))
}

func nvmetAllowAnyHostPath(nqn string) string {
	return fmt.Sprintf("%s/attr_allow_any_host", nvmetSubsysPath(nqn))
}

func nvmetSubsysHostsPath(nqn string) string {
	return fmt.Sprintf("%s/allowed_hosts", nvmetSubsysPath(nqn))
}

func nvmetHostPath(hostNqn string) string {
	return fmt.Sprintf("%s/hosts/%s", nvmetPath, hostNqn)
}

func nvmetHostInSubsysPath(nqn, hostNqn string) string {
	return fmt.Sprintf("%s/%s", nvmetSubsysHostsPath(nqn), hostNqn)
}

func nvmetSubsysInPortPath(nqn string, portNum string) string {
	return fmt.Sprintf("%s/subsystems/%s", nvmetPortPath(portNum), nqn)
}

func nvmetSubsysNsParentPath(nqn string) string {
	return fmt.Sprintf("%s/namespaces", nvmetSubsysPath(nqn))
}

func nvmetSubsysNsPath(nqn, nsNum string) string {
	return fmt.Sprintf("%s/%s", nvmetSubsysNsParentPath(nqn), nsNum)
}

func nvmetSubsysNsDevPath(nqn, nsNum string) string {
	return fmt.Sprintf("%s/device_path", nvmetSubsysNsPath(nqn, nsNum))
}

func nvmetSubsysNsNguidPath(nqn, nsNum string) string {
	return fmt.Sprintf("%s/device_nguid", nvmetSubsysNsPath(nqn, nsNum))
}

func nvmetSubsysNsUuidPath(nqn, nsNum string) string {
	return fmt.Sprintf("%s/device_uuid", nvmetSubsysNsPath(nqn, nsNum))
}

func nvmetSubsysNsEnablePath(nqn, nsNum string) string {
	return fmt.Sprintf("%s/enable", nvmetSubsysNsPath(nqn, nsNum))
}

func nvmetSubsysNsAnaGrpIdPath(nqn, nsNum string) string {
	return fmt.Sprintf("%s/ana_grpid", nvmetSubsysNsPath(nqn, nsNum))
}

type OsCommand struct {
	exePath string
}

func (oc *OsCommand) runOsCmd(
	pch *ctxhelper.PerCtxHelper,
	name string,
	args []string,
	stdin string,
) (string, string, error) {
	pch.Logger.Info("OsCommand name: [%v]", name)
	pch.Logger.Info("OsCommand args: [%v]", args)
	pch.Logger.Info("OsCommand stdin: [%v]", stdin)
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

func (oc *OsCommand) NvmetPortCreate(
	pch *ctxhelper.PerCtxHelper,
	portNum string,
	trType string,
	adrFam string,
	trAddr string,
	trSvcId string,
) error {
	if err := createDir(nvmetPortPath(portNum)); err != nil {
		return err
	}

	trTypePath := nvmetTrTypePath(portNum)
	currTrType, err := readFile(trTypePath)
	if err != nil {
		return err
	}
	if currTrType != trType {
		if err := writeFile(trTypePath, trType); err != nil {
			return err
		}
	}

	adrFamPath := nvmetAdrFamPath(portNum)
	currAdrFam, err := readFile(adrFamPath)
	if err != nil {
		return err
	}
	if currAdrFam != adrFam {
		if err := writeFile(adrFamPath, adrFam); err != nil {
			return err
		}
	}

	trAddrPath := nvmetTrAddrPath(portNum)
	currTrAddr, err := readFile(trAddrPath)
	if err != nil {
		return err
	}
	if currTrAddr != trAddr {
		if err := writeFile(trAddrPath, trAddr); err != nil {
			return err
		}
	}

	trSvcIdPath := nvmetTrSvcIdPath(portNum)
	currTrSvcId, err := readFile(trSvcIdPath)
	if err != nil {
		return err
	}
	if currTrSvcId != trSvcId {
		if err := writeFile(trSvcIdPath, trSvcId); err != nil {
			return err
		}
	}

	// FIXME: support addr_treq

	return nil
}

func (oc *OsCommand) NvmetPortDelete(
	pch *ctxhelper.PerCtxHelper,
	portNum string,
) error {
	if err := removeAny(nvmetPortPath(portNum)); err != nil {
		return err
	}

	return nil
}

type NvmetNsArg struct {
	NsNum   string
	DevPath string
	Uuid    string
}

func (oc *OsCommand) nvmetAddHostToSubsys(nqn, hostNqn string) error {
	path := nvmetHostPath(hostNqn)
	// Ignore error because multiple agents may create the same host at the same time.
	// We are ok if at least one of them succeeds
	createDir(path)
	if err := createLink(path, nvmetHostInSubsysPath(nqn, hostNqn)); err != nil {
		return err
	}
	return nil
}

func (oc *OsCommand) nvmetRemoveHostFromSubsys(nqn, hostNqn string) error {
	return removeAny(nvmetHostInSubsysPath(nqn, hostNqn))
}

func (oc *OsCommand) nvmetAddSubsysToPort(nqn string, portNum string) error {
	return createLink(nvmetSubsysPath(nqn), nvmetSubsysInPortPath(nqn, portNum))
}

func (oc *OsCommand) nvmetRemoveSubsysFromPort(nqn string, portNum string) error {
	return removeAny(nvmetSubsysInPortPath(nqn, portNum))
}

func (oc *OsCommand) nvmetSubsysNsCreate(nqn string, nsArg *NvmetNsArg) error {
	nsPath := nvmetSubsysNsPath(nqn, nsArg.NsNum)
	if err := createDir(nsPath); err != nil {
		return err
	}

	if err := writeFile(
		nvmetSubsysNsDevPath(nqn, nsArg.NsNum),
		nsArg.DevPath,
	); err != nil {
		return err
	}
	if err := writeFile(
		nvmetSubsysNsAnaGrpIdPath(nqn, nsArg.NsNum),
		"1",
	); err != nil {
		return err
	}
	if err := writeFile(
		nvmetSubsysNsNguidPath(nqn, nsArg.NsNum),
		nsArg.Uuid,
	); err != nil {
		return err
	}
	if err := writeFile(
		nvmetSubsysNsUuidPath(nqn, nsArg.NsNum),
		nsArg.Uuid,
	); err != nil {
		return err
	}
	if err := writeFile(
		nvmetSubsysNsEnablePath(nqn, nsArg.NsNum),
		"1",
	); err != nil {
		return err
	}

	return nil
}

func (oc *OsCommand) nvmetSubsysNsDelete(nqn string, nsNum string) error {
	return removeAny(nvmetSubsysNsPath(nqn, nsNum))
}

func (oc *OsCommand) NvmetSubsysCreate(
	pch *ctxhelper.PerCtxHelper,
	nqn string,
	cntlidMin uint32,
	cntlidMax uint32,
	portNum string,
	hostNqnMap map[string]bool,
	nsMap map[string]*NvmetNsArg,
) error {
	if err := createDir(nvmetSubsysPath(nqn)); err != nil {
		return err
	}
	cntlidMinStr := fmt.Sprintf("%d", cntlidMin)
	if err := writeFile(nvmetCntlidMinPath(nqn), cntlidMinStr); err != nil {
		return err
	}
	cntlidMaxStr := fmt.Sprintf("%d", cntlidMax)
	if err := writeFile(nvmetCntlidMaxPath(nqn), cntlidMaxStr); err != nil {
		return err
	}
	if err := writeFile(nvmetAllowAnyHostPath(nqn), "0"); err != nil {
		return err
	}

	hostEntries, err := os.ReadDir(nvmetSubsysHostsPath(nqn))
	if err != nil {
		return err
	}
	currHostNqnMap := make(map[string]bool)
	for _, hostEntry := range hostEntries {
		currHostNqnMap[hostEntry.Name()] = true
	}

	hostToBeCreated := make([]string, 0)
	hostToBeDeleted := make([]string, 0)
	for hostNqn := range hostNqnMap {
		if _, ok := currHostNqnMap[hostNqn]; !ok {
			hostToBeCreated = append(hostToBeCreated, hostNqn)
		}
	}
	for hostNqn := range currHostNqnMap {
		if _, ok := hostNqnMap[hostNqn]; !ok {
			hostToBeDeleted = append(hostToBeDeleted, hostNqn)
		}
	}

	for _, hostNqn := range hostToBeCreated {
		if err := oc.nvmetAddHostToSubsys(nqn, hostNqn); err != nil {
			return err
		}
	}
	if len(hostToBeDeleted) > 0 {
		if err := oc.nvmetRemoveSubsysFromPort(nqn, portNum); err != nil {
			return err
		}
		for _, hostNqn := range hostToBeDeleted {
			if err := oc.nvmetRemoveHostFromSubsys(nqn, hostNqn); err != nil {
				return err
			}
		}
	}
	if err := oc.nvmetAddSubsysToPort(nqn, portNum); err != nil {
		return err
	}

	nsEntries, err := os.ReadDir(nvmetSubsysNsParentPath(nqn))
	if err != nil {
		return err
	}
	currNsMap := make(map[string]bool)
	for _, nsEntity := range nsEntries {
		currNsMap[nsEntity.Name()] = true
	}
	nsToBeCreated := make([]*NvmetNsArg, 0)
	nsToBeDeleted := make([]string, 0)
	for nsNum := range currNsMap {
		nsArg, ok := nsMap[nsNum]
		if !ok {
			nsToBeDeleted = append(nsToBeDeleted, nsNum)
		} else {
			devPath, err := readFile(nvmetSubsysNsDevPath(nqn, nsNum))
			if err != nil {
				return err
			}
			if devPath != nsArg.DevPath {
				nsToBeDeleted = append(nsToBeDeleted, nsNum)
				nsToBeCreated = append(nsToBeCreated, nsArg)
			}
		}
	}
	for nsNum, nsArg := range nsMap {
		if _, ok := currNsMap[nsNum]; !ok {
			nsToBeCreated = append(nsToBeCreated, nsArg)
		}
	}

	for _, nsNum := range nsToBeDeleted {
		if err := oc.nvmetSubsysNsDelete(nqn, nsNum); err != nil {
			return err
		}
	}
	for _, nsArg := range nsToBeCreated {
		if err := oc.nvmetSubsysNsCreate(nqn, nsArg); err != nil {
			return err
		}
	}

	return nil
}

func (oc *OsCommand) NvmetSubsysDelete(
	pch *ctxhelper.PerCtxHelper,
	nqn string,
) error {
	hostEntries, err := os.ReadDir(nvmetSubsysHostsPath(nqn))
	if err != nil {
		return err
	}
	for _, hostEntry := range hostEntries {
		if err := oc.nvmetRemoveHostFromSubsys(nqn, hostEntry.Name()); err != nil {
			return err
		}
	}

	nsEntries, err := os.ReadDir(nvmetSubsysNsParentPath(nqn))
	if err != nil {
		return err
	}
	for _, nsEntity := range nsEntries {
		if err := oc.nvmetSubsysNsDelete(nqn, nsEntity.Name()); err != nil {
			return err
		}
	}

	return nil
}

func (oc *OsCommand) RemoveSubsysFromPort(
	pch *ctxhelper.PerCtxHelper,
	nqn string,
	portNum string,
) error {
	return oc.nvmetRemoveSubsysFromPort(nqn, portNum)
}

type DmLinearArg struct {
	Start   uint64
	Size    uint64
	DevPath string
	Offset  uint64
}

type DmRaid1Arg struct {
	Start      uint64
	Size       uint64
	Meta0      string
	Data0      string
	Meta1      string
	Data1      string
	RegionSize uint64
	Nosync     bool
	RebuildIdx uint32
}

func (oc *OsCommand) dmStatus(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) (string, error) {
	name := "dmsetup"
	args := []string{"status", dmName}
	stdout, stderr, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		if strings.Contains(stderr, dmNotExist) {
			return "", nil
		}
		return "", err
	}
	return stdout, nil
}

func (oc *OsCommand) dmTable(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) (string, error) {
	name := "dmsetup"
	args := []string{"table", dmName}
	stdout, stderr, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		if strings.Contains(stderr, "No such device or address") {
			return "", nil
		}
		return "", err
	}
	return stdout, nil
}

func (oc *OsCommand) dmCreate(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	table string,
) error {
	name := "dmsetup"
	args := []string{"create", dmName}
	if _, _, err := oc.runOsCmd(pch, name, args, table); err != nil {
		return err
	}
	return nil
}

func (oc *OsCommand) dmRemove(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) error {
	name := "dmsetup"
	args := []string{"remove", dmName}
	_, _, err := oc.runOsCmd(pch, name, args, "")
	return err
}

func (oc *OsCommand) DmSuspend(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) error {
	name := "dmsetup"
	args := []string{"suspend", dmName}
	_, _, err := oc.runOsCmd(pch, name, args, "")
	return err
}

func (oc *OsCommand) DmResume(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) error {
	name := "dmsetup"
	args := []string{"resume", dmName}
	_, _, err := oc.runOsCmd(pch, name, args, "")
	return err
}

func (oc *OsCommand) DmLoad(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	table string,
) error {
	name := "dmsetup"
	args := []string{"load", dmName}
	_, _, err := oc.runOsCmd(pch, name, args, table)
	return err
}

func (oc *OsCommand) dmReload(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	table string,
) error {
	if err := oc.DmSuspend(pch, dmName); err != nil {
		return err
	}
	if err := oc.DmLoad(pch, dmName, table); err != nil {
		return err
	}
	if err := oc.DmResume(pch, dmName); err != nil {
		return err
	}
	return nil
}

func (oc *OsCommand) DmLinearChange(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	linearArgs []*DmLinearArg,
) (bool, error) {
	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return false, err
	}
	if status == "" {
		return false, nil
	}
	lines := strings.Split(status, "\n")
	if len(lines) == len(linearArgs) && strings.Contains(status, "linear") {
		return false, nil
	}
	return true, nil
}

func (oc *OsCommand) DmCreateLinear(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	linearArgs []*DmLinearArg,
) error {
	if len(linearArgs) < 1 {
		pch.Logger.Fatal("Invalid linearArgs: %v", linearArgs)
	}

	var tableBuilder strings.Builder
	for _, linearArg := range linearArgs {
		oneLine := fmt.Sprintf(
			"%d %d linear %s %d\n",
			byteToSector(linearArg.Start),
			byteToSector(linearArg.Size),
			linearArg.DevPath,
			byteToSector(linearArg.Offset),
		)
		tableBuilder.WriteString(oneLine)
	}
	table := tableBuilder.String()

	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return err
	}

	if status == "" {
		// If not exist, create new
		return oc.dmCreate(pch, dmName, table)
	}

	lines := strings.Split(status, "\n")
	// len(lines) - 1 to remove the last empty line
	if (len(lines)-1) == len(linearArgs) && strings.Contains(status, "linear") {
		// If exist and same, nothing to do
		return nil
	}

	// If exist and not same, reload
	return oc.dmReload(pch, dmName, table)
}

type majorMinor struct {
	major uint32
	minor uint32
}

func (oc *OsCommand) dmRaidMajorMinor(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) ([]*majorMinor, error) {
	tableStr, err := oc.dmTable(pch, dmName)
	if err != nil {
		return nil, err
	}
	if tableStr == "" {
		return nil, nil
	}
	if !strings.Contains(tableStr, "raid raid") {
		return nil, fmt.Errorf("%s is not a dm raid device", dmName)
	}
	mmList := make([]*majorMinor, 0)
	items := strings.Split(tableStr, " ")
	for _, item := range items {
		if item == "-" {
			mm := &majorMinor{
				major: constants.DevMajorNone,
				minor: constants.DevMinorNone,
			}
			mmList = append(mmList, mm)
			continue
		}
		if !strings.Contains(item, ":") {
			continue
		}
		mmItems := strings.Split(item, ":")
		if len(mmItems) != 2 {
			return nil, fmt.Errorf("Invalid major minor number: %s", item)
		}
		major, err := strconv.ParseUint(mmItems[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("Invalid major: %s", mmItems[0])
		}
		minor, err := strconv.ParseUint(mmItems[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("Invalid minor: %s", mmItems[1])
		}
		mm := &majorMinor{
			major: uint32(major),
			minor: uint32(minor),
		}
		mmList = append(mmList, mm)
	}
	if len(mmList) != 4 {
		return nil, fmt.Errorf("Invalid mm cnt: %d", len(mmList))
	}
	return mmList, nil
}

type DmRaidStatus struct {
	Start       uint64
	Size        uint64
	RaidType    string
	DevCnt      uint32
	HealthChars []byte
	SyncCurr    uint64
	SyncTotal   uint64
	SyncAction  string
	MismatchCnt uint64
	DataOffset  uint64
	JournalChar byte
}

func (oc *OsCommand) DmGetRaidStatus(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) (*DmRaidStatus, error) {
	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return nil, err
	}
	if status == "" {
		return nil, nil
	}
	items := strings.Split(status, " ")
	if len(items) != 11 {
		return nil, fmt.Errorf("Raid status item cnt incorrect: %d", len(items))
	}
	start, err := strconv.ParseUint(items[0], 10, 64)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseUint(items[1], 10, 64)
	if err != nil {
		return nil, err
	}
	if items[2] != "raid" {
		return nil, fmt.Errorf("Not raid status: %s", items[2])
	}
	if !strings.HasPrefix(items[3], "raid") {
		return nil, fmt.Errorf("Invalid raid type: %s", items[3])
	}
	raidType := items[3]
	devCnt, err := strconv.ParseUint(items[4], 10, 32)
	if err != nil {
		return nil, err
	}
	if len(items[5]) != int(devCnt) {
		return nil, fmt.Errorf("Incorrect health chars cnt: %d", len(items[5]))
	}
	healthChars := make([]byte, devCnt)
	for i := 0; i < int(devCnt); i++ {
		ch := items[5][i]
		if ch != constants.RaidHealthAliveInSync &&
			ch != constants.RaidHealthAliveOutSync &&
			ch != constants.RaidHealthDead &&
			ch != constants.RaidHealthMiss {
			return nil, fmt.Errorf("Invalid health char: %c", ch)
		}
		healthChars[i] = ch
	}
	syncRatioItems := strings.Split(items[6], "/")
	if len(syncRatioItems) != 2 {
		return nil, fmt.Errorf("Invalid sync ratio cnt: %d", len(syncRatioItems))
	}
	syncCurr, err := strconv.ParseUint(syncRatioItems[0], 10, 64)
	if err != nil {
		return nil, err
	}
	syncTotal, err := strconv.ParseUint(syncRatioItems[1], 10, 64)
	if err != nil {
		return nil, err
	}
	syncAction := items[7]
	mismatchCnt, err := strconv.ParseUint(items[8], 10, 64)
	if err != nil {
		return nil, err
	}
	dataOffset, err := strconv.ParseUint(items[9], 10, 64)
	if err != nil {
		return nil, err
	}
	// Only first character is the journal char, the last one is '\n'
	if len(items[10]) != 2 {
		return nil, fmt.Errorf("Invalid journal char cnt: %d", len(items[10]))
	}
	journalChar := items[10][0]
	return &DmRaidStatus{
		Start:       sectorToByte(start),
		Size:        sectorToByte(size),
		RaidType:    raidType,
		DevCnt:      uint32(devCnt),
		HealthChars: healthChars,
		SyncCurr:    syncCurr,
		SyncTotal:   syncTotal,
		SyncAction:  syncAction,
		MismatchCnt: mismatchCnt,
		DataOffset:  dataOffset,
		JournalChar: journalChar,
	}, nil
}

func (oc *OsCommand) DmCreateRaid1(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	raid1Arg *DmRaid1Arg,
) error {
	paramCnt := 3
	noSync := ""
	if raid1Arg.Nosync {
		noSync = "nosync "
		paramCnt += 1
	}

	rebuild := ""
	if raid1Arg.RebuildIdx != constants.Uint32Max {
		rebuild = fmt.Sprintf("rebuild %d ", raid1Arg.RebuildIdx)
		paramCnt += 2
	}

	table := fmt.Sprintf(
		"%d %d raid raid1 %d 0 region_size %d %s%s2 %s %s %s %s",
		byteToSector(raid1Arg.Start),
		byteToSector(raid1Arg.Size),
		paramCnt,
		byteToSector(raid1Arg.RegionSize),
		noSync,
		rebuild,
		raid1Arg.Meta0,
		raid1Arg.Data0,
		raid1Arg.Meta1,
		raid1Arg.Data1,
	)

	mmList, err := oc.dmRaidMajorMinor(pch, dmName)
	if err != nil {
		return err
	}

	if mmList == nil {
		return oc.dmCreate(pch, dmName, table)
	}

	meta0Major, meta0Minor, err := oc.blkGetMajorMinor(pch, raid1Arg.Meta0)
	if err != nil {
		return err
	}
	data0Major, data0Minor, err := oc.blkGetMajorMinor(pch, raid1Arg.Data0)
	if err != nil {
		return err
	}
	meta1Major, meta1Minor, err := oc.blkGetMajorMinor(pch, raid1Arg.Meta1)
	if err != nil {
		return err
	}
	data1Major, data1Minor, err := oc.blkGetMajorMinor(pch, raid1Arg.Data1)
	if err != nil {
		return err
	}
	if meta0Major != mmList[0].major ||
		meta0Minor != mmList[0].minor ||
		data0Major != mmList[1].major ||
		data0Minor != mmList[1].minor ||
		meta1Major != mmList[2].major ||
		meta1Minor != mmList[2].minor ||
		data1Major != mmList[3].major ||
		data1Minor != mmList[3].minor {
		return oc.dmReload(pch, dmName, table)
	}

	return nil
}

type DmRaid0Arg struct {
	Start     uint64
	Size      uint64
	ChunkSize uint64
	DevList   []string
}

func (oc *OsCommand) DmCreateRaid0(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	raid0Arg *DmRaid0Arg,
) error {
	cnt := len(raid0Arg.DevList)
	var tableBuilder strings.Builder
	prefix := fmt.Sprintf(
		"%d %d raid raid0 1 %d %d",
		byteToSector(raid0Arg.Start),
		byteToSector(raid0Arg.Size),
		byteToSector(raid0Arg.ChunkSize),
		cnt,
	)
	tableBuilder.WriteString(prefix)
	for _, devPath := range raid0Arg.DevList {
		devStr := fmt.Sprintf(" - %s", devPath)
		tableBuilder.WriteString(devStr)
	}
	table := tableBuilder.String()

	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return err
	}

	if status == "" {
		return oc.dmCreate(pch, dmName, table)
	}

	return nil
}

type DmPoolArg struct {
	Start             uint64
	Size              uint64
	MetaDev           string
	DataDev           string
	DataBlockSize     uint64
	LowWaterMark      uint64
	SkipBlockZeroing  bool
	IgnoreDiscard     bool
	NoDiscardPassdown bool
	ReadOnly          bool
	ErrorIfNoSpace    bool
}

func genDmPoolTable(poolArg *DmPoolArg) string {
	paramCnt := 0
	skipBlockZeroing := ""
	if poolArg.SkipBlockZeroing {
		skipBlockZeroing = " skip_block_zeroing"
		paramCnt += 1
	}
	ignoreDiscard := ""
	if poolArg.IgnoreDiscard {
		ignoreDiscard = " ignore_discard"
		paramCnt += 1
	}
	noDiscardPassdown := ""
	if poolArg.NoDiscardPassdown {
		noDiscardPassdown = " no_discard_passdown"
		paramCnt += 1
	}
	readOnly := ""
	if poolArg.ReadOnly {
		readOnly = " read_only"
		paramCnt += 1
	}
	errorIfNoSpace := ""
	if poolArg.ErrorIfNoSpace {
		errorIfNoSpace = " error_if_no_space"
		paramCnt += 1
	}
	table := fmt.Sprintf(
		"%d %d thin-pool %s %s %d %d %d%s%s%s%s%s",
		byteToSector(poolArg.Start),
		byteToSector(poolArg.Size),
		poolArg.MetaDev,
		poolArg.DataDev,
		byteToSector(poolArg.DataBlockSize),
		poolArg.LowWaterMark,
		paramCnt,
		skipBlockZeroing,
		ignoreDiscard,
		noDiscardPassdown,
		readOnly,
		errorIfNoSpace,
	)
	return table
}

func (oc *OsCommand) DmCreatePool(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	poolArg *DmPoolArg,
	reload bool,
) error {
	table := genDmPoolTable(poolArg)
	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return err
	}
	if status == "" {
		return oc.dmCreate(pch, dmName, table)
	}
	if reload {
		return oc.dmReload(pch, dmName, table)
	}
	return nil
}

func (oc *OsCommand) DmPoolMsgCreateThin(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	devId uint32,
) error {
	msg := fmt.Sprintf("create_thin %d", devId)
	name := "dmsetup"
	args := []string{"message", dmName, "0", msg}
	_, stderr, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		if strings.Contains(stderr, "File exists") {
			return nil
		}
		return err
	}
	return nil
}

func (oc *OsCommand) DmPoolMsgCreateSnap(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	devId uint32,
	oriId uint32,
) error {
	msg := fmt.Sprintf("create_snap %d %d", devId, oriId)
	name := "dmsetup"
	args := []string{"message", dmName, "0", msg}
	_, stderr, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		if strings.Contains(stderr, "File exists") {
			return nil
		}
		return err
	}
	return nil
}

func (oc *OsCommand) DmPoolMsgDelete(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	devId uint32,
) error {
	msg := fmt.Sprintf("delete %d", devId)
	name := "dmsetup"
	args := []string{"message", dmName, "0", msg}
	_, stderr, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		if strings.Contains(stderr, "No data available") {
			return nil
		}
		return err
	}
	return nil
}

type DmPoolStatus struct {
	Start                uint64
	Size                 uint64
	TransactionId        uint64
	UsedMetaBlocks       uint64
	TotalMetaBlocks      uint64
	UsedDataBlocks       uint64
	TotalDataBlocks      uint64
	HeldMetadataRoot     int64
	Mode                 string
	DiscardPassdown      bool
	ErrorOrQueue         string
	NeedsCheck           bool
	MetadataLowWatermark uint64
}

func (oc *OsCommand) DmGetPoolStatus(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) (*DmPoolStatus, error) {
	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return nil, err
	}
	if status == "" {
		return nil, nil
	}
	items := strings.Split(status, " ")
	if len(items) != 12 {
		return nil, fmt.Errorf("Pool status item cnt incorrect: %d", len(items))
	}
	start, err := strconv.ParseUint(items[0], 10, 64)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseUint(items[1], 10, 64)
	if err != nil {
		return nil, err
	}
	if items[2] != "thin-pool" {
		return nil, fmt.Errorf("Not pool status: %d", items[2])
	}
	transactionId, err := strconv.ParseUint(items[3], 10, 64)
	if err != nil {
		return nil, err
	}
	metaBlocksItems := strings.Split(items[4], "/")
	if len(metaBlocksItems) != 2 {
		return nil, fmt.Errorf(
			"Invalid metaBlocksItems cnt: %d",
			len(metaBlocksItems),
		)
	}
	usedMetaBlocks, err := strconv.ParseUint(metaBlocksItems[0], 10, 64)
	if err != nil {
		return nil, err
	}
	totalMetaBlocks, err := strconv.ParseUint(metaBlocksItems[1], 10, 64)
	if err != nil {
		return nil, err
	}
	dataBlocksItems := strings.Split(items[5], "/")
	if len(dataBlocksItems) != 2 {
		return nil, fmt.Errorf(
			"Invalid dataBlocksItems cnt: %d",
			len(dataBlocksItems),
		)
	}
	usedDataBlocks, err := strconv.ParseUint(dataBlocksItems[0], 10, 64)
	if err != nil {
		return nil, err
	}
	totalDataBlocks, err := strconv.ParseUint(dataBlocksItems[1], 10, 64)
	if err != nil {
		return nil, err
	}
	heldMetadataRoot := int64(-1)
	if items[6] != "-" {
		root, err := strconv.ParseUint(items[6], 10, 64)
		if err != nil {
			return nil, err
		}
		heldMetadataRoot = int64(root)
	}
	mode := items[7]
	discardPassdown := true
	if items[8] == "no_discard_passdown" {
		discardPassdown = false
	}
	errorOrQueue := items[9]
	needsCheck := false
	if items[10] != "-" {
		needsCheck = true
	}
	metadataLowWatermark, err := strconv.ParseUint(items[11], 10, 64)
	if err != nil {
		return nil, err
	}
	return &DmPoolStatus{
		Start:                sectorToByte(start),
		Size:                 sectorToByte(size),
		TransactionId:        transactionId,
		UsedMetaBlocks:       usedMetaBlocks,
		TotalMetaBlocks:      totalMetaBlocks,
		UsedDataBlocks:       usedDataBlocks,
		TotalDataBlocks:      totalDataBlocks,
		HeldMetadataRoot:     heldMetadataRoot,
		Mode:                 mode,
		DiscardPassdown:      discardPassdown,
		ErrorOrQueue:         errorOrQueue,
		NeedsCheck:           needsCheck,
		MetadataLowWatermark: metadataLowWatermark,
	}, nil
}

type DmThinArg struct {
	Start    uint64
	Size     uint64
	PoolPath string
	DevId    uint32
}

func (oc *OsCommand) DmCreateThin(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
	thinArg *DmThinArg,
) error {
	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return err
	}
	if status == "" {
		table := fmt.Sprintf(
			"%d %d thin %s %d",
			thinArg.Start,
			thinArg.Size,
			thinArg.PoolPath,
			thinArg.DevId,
		)
		return oc.dmCreate(pch, dmName, table)
	}
	return nil
}

func (oc *OsCommand) DmRemove(
	pch *ctxhelper.PerCtxHelper,
	dmName string,
) error {
	status, err := oc.dmStatus(pch, dmName)
	if err != nil {
		return err
	}
	if status != "" {
		return oc.dmRemove(pch, dmName)
	}
	return nil
}

func (oc *OsCommand) DmGetAll(
	pch *ctxhelper.PerCtxHelper,
) (map[string]bool, error) {
	dmMap := make(map[string]bool)
	name := "dmsetup"
	args := []string{"status"}
	stdout, _, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		return nil, err
	}
	if strings.Contains(stdout, dmEmpty) {
		return nil, err
	}
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		items := strings.Split(line, ":")
		if len(items) < 1 {
			return nil, fmt.Errorf("Invalid dmstatus: %s", line)
		}
		name := items[0]
		dmMap[name] = true
	}
	return dmMap, nil
}

// FIXME: Support more nvme connect parameters
type NvmeArg struct {
	Nqn       string
	Transport string
	TrAddr    string
	TrSvcId   string
	HostNqn   string
}

type nvmePath struct {
	name      string
	transport string
	trAddr    string
	trSvcId   string
	status    string
}

type nvmeSubsys struct {
	nqn      string
	pathList []*nvmePath
}

func (oc *OsCommand) nvmeGetSubsys(
	pch *ctxhelper.PerCtxHelper,
	nqn string,
) (*nvmeSubsys, error) {
	name := filepath.Join(oc.exePath, "nvme")
	args := []string{"list-subsys"}
	stdout, _, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(stdout, "\n")
	var subsys *nvmeSubsys
	for _, line := range lines {
		if strings.HasPrefix(line, "nvme-subsys") {
			if subsys != nil {
				break
			}
			if strings.HasSuffix(line, nqn) {
				subsys = &nvmeSubsys{
					nqn:      nqn,
					pathList: make([]*nvmePath, 0),
				}
			}
		} else {
			if subsys != nil {
				if strings.HasPrefix(line, " +- nvme") {
					items := strings.Split(line, " ")
					name := items[2]
					transport := items[3]
					status := items[5]
					addrAndSvcId := strings.Split(items[4], ",")
					trAddr := strings.Split(addrAndSvcId[0], "=")[1]
					trSvcId := strings.Split(addrAndSvcId[1], "=")[1]
					nvPath := &nvmePath{
						name:      name,
						transport: transport,
						trAddr:    trAddr,
						trSvcId:   trSvcId,
						status:    status,
					}
					subsys.pathList = append(subsys.pathList, nvPath)
				}
			}
		}
	}
	return subsys, nil
}

func (oc *OsCommand) nvmeConnectPath(
	pch *ctxhelper.PerCtxHelper,
	nvmeArg *NvmeArg,
) error {
	name := filepath.Join(oc.exePath, "nvme")
	args := []string{
		"connect",
		"--nqn",
		nvmeArg.Nqn,
		"--transpor",
		nvmeArg.Transport,
		"--traddr",
		nvmeArg.TrAddr,
		"--trsvcid",
		nvmeArg.TrSvcId,
		"--hostnqn",
		nvmeArg.HostNqn,
	}
	if _, _, err := oc.runOsCmd(pch, name, args, ""); err != nil {
		return err
	}

	return nil
}

func (oc *OsCommand) nvmeDisconnectPath(
	pch *ctxhelper.PerCtxHelper,
	nvmeName string,
) error {
	name := filepath.Join(oc.exePath, "nvme")
	args := []string{
		"disconnect",
		"--device",
		nvmeName,
	}
	if _, _, err := oc.runOsCmd(pch, name, args, ""); err != nil {
		return err
	}
	return nil
}

func (oc *OsCommand) NvmeConnectPath(
	pch *ctxhelper.PerCtxHelper,
	nvmeArg *NvmeArg,
) error {
	subsys, err := oc.nvmeGetSubsys(pch, nvmeArg.Nqn)
	if err != nil {
		return err
	}
	if subsys != nil {
		for _, nvPath := range subsys.pathList {
			if nvPath.transport == nvmeArg.Transport &&
				nvPath.trAddr == nvmeArg.TrAddr &&
				nvPath.trSvcId == nvmeArg.TrSvcId {
				// The path exists, nothing to do
				return nil
			}
		}
	}
	if err := oc.nvmeConnectPath(pch, nvmeArg); err != nil {
		return err
	}
	return nil
}

func (oc *OsCommand) NvmeDisconnectPath(
	pch *ctxhelper.PerCtxHelper,
	nvmeArg *NvmeArg,
) error {
	subsys, err := oc.nvmeGetSubsys(pch, nvmeArg.Nqn)
	if err != nil {
		return err
	}
	if subsys != nil {
		for _, nvPath := range subsys.pathList {
			if nvPath.transport == nvmeArg.Transport &&
				nvPath.trAddr == nvmeArg.TrAddr &&
				nvPath.trSvcId == nvmeArg.TrSvcId {
				return oc.nvmeDisconnectPath(pch, nvPath.name)
			}
		}
	}
	return nil
}

func (oc *OsCommand) fileRealpath(
	pch *ctxhelper.PerCtxHelper,
	path string,
) (string, error) {
	name := "realpath"
	args := []string{path}
	stdout, _, err := oc.runOsCmd(pch, name, args, "")
	return stdout, err
}

func (oc *OsCommand) blkGetMajorMinor(
	pch *ctxhelper.PerCtxHelper,
	devPath string,
) (uint32, uint32, error) {
	if devPath == "-" {
		return constants.DevMajorNone, constants.DevMinorNone, nil
	}
	realPath, err := oc.fileRealpath(pch, devPath)
	if err != nil {
		return 0, 0, err
	}
	name := "stat"
	args := []string{
		"-c",
		"%t %T",
		realPath,
	}
	stdout, _, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		return 0, 0, err
	}
	items := strings.Split(stdout, " ")
	if len(items) != 2 {
		return 0, 0, fmt.Errorf("Invalid major/minor cnt: %d", len(items))
	}
	major, err := strconv.ParseUint(items[0], 16, 32)
	if err != nil {
		return 0, 0, err
	}
	minor, err := strconv.ParseUint(items[1], 16, 32)
	if err != nil {
		return 0, 0, err
	}
	return uint32(major), uint32(minor), nil
}

func (oc *OsCommand) BlkGetSize(
	pch *ctxhelper.PerCtxHelper,
	devPath string,
) (uint64, error) {
	name := "blockdev"
	args := []string{"--getsize64", devPath}
	stdout, _, err := oc.runOsCmd(pch, name, args, "")
	if err != nil {
		return 0, err
	}
	cmdOut := strings.TrimSuffix(stdout, "\n")
	size, err := strconv.ParseUint(cmdOut, 10, 64)
	return size, err
}

func (oc *OsCommand) BlkDiscard(
	pch *ctxhelper.PerCtxHelper,
	devPath string,
) error {
	name := "blkdiscard"
	args := []string{"--force", devPath}
	_, _, err := oc.runOsCmd(pch, name, args, "")
	return err
}

func NewOsCommand() *OsCommand {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return &OsCommand{
		exePath: filepath.Dir(exe),
	}
}
