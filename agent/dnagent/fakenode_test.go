package dnagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// fakeNode is an in-memory model of the slice of the OS the dn agent drives:
// the raw disk's [D13] metadata regions, device-mapper, the nvmet configfs
// tree, the nvme host and the local store. It backs common.FakeOsClient, records every call, and lets tests
// assert command sequences, probe-first idempotency and teardown order
// without root or real devices (dnagent.md §6).
type fakeNode struct {
	mu sync.Mutex

	calls []string

	// block devices
	devSize   map[string]uint64
	devNo     map[string]string
	nextMinor int

	// raw-device metadata regions ([D13]). A sparse overlay, never a flat
	// buffer: the data area alone is hundreds of MiB.
	blocks map[string][]fakeSegment

	// device-mapper
	dms map[string]*fakeDm

	// configfs / directories
	dirs  map[string]bool
	files map[string]string
	links map[string]string

	// local store (WriteProto/ReadProto)
	protos map[string][]byte

	// nvme host connections, keyed by subsystem nqn
	conns map[string]*fakeConn

	// failBlockWrite fails every WriteBlock at this offset (0 disables it),
	// so a test can build the crash windows of the [D13] save protocol.
	failBlockWrite uint64
	failBlockSet   bool

	// failCmd fails the first matching command with the given stderr;
	// failCmdAlways fails every matching command, for the persistent
	// failures a single converge pass is supposed to survive.
	failCmd       map[string]string
	failCmdAlways map[string]string
	// gate blocks a command until the channel is closed (lock tests).
	gate map[string]chan struct{}
}

// fakeSegment is one recorded WriteBlock, in write order: later segments
// overlay earlier ones.
type fakeSegment struct {
	off  uint64
	data []byte
}

type fakeDm struct {
	table     string
	readOnly  bool
	suspended bool
	// dm-clone bookkeeping
	noHydration bool
	threshold   uint32
	batchSize   uint32
	discards    []string
}

type fakeConn struct {
	device string
	state  string
}

func newFakeNode() *fakeNode {
	return &fakeNode{
		devSize:       make(map[string]uint64),
		devNo:         make(map[string]string),
		nextMinor:     1,
		blocks:        make(map[string][]fakeSegment),
		dms:           make(map[string]*fakeDm),
		dirs:          map[string]bool{common.DefaultLocalStorPrefix: true},
		files:         make(map[string]string),
		links:         make(map[string]string),
		protos:        make(map[string][]byte),
		conns:         make(map[string]*fakeConn),
		failCmd:       make(map[string]string),
		failCmdAlways: make(map[string]string),
		gate:          make(map[string]chan struct{}),
	}
}

func (f *fakeNode) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn:      f.runCommand,
		ReadFileFn:        f.readFile,
		WriteFileFn:       f.writeFile,
		WriteFileDirectFn: f.writeFileDirect,
		ReadBlockFn:       f.readBlock,
		WriteBlockFn:      f.writeBlock,
		ReadProtoFn:       f.readProto,
		WriteProtoFn:      f.writeProto,
	}
}

// ---------------------------------------------------------------------------
// Call recording
// ---------------------------------------------------------------------------

func (f *fakeNode) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// Calls returns every recorded call, in order.
func (f *fakeNode) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeNode) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// readOnlyPrefixes are the probes; everything else changes the system.
var readOnlyPrefixes = []string{
	"cmd ls ", "cmd lsblk ",
	"cmd dmsetup info", "cmd dmsetup table", "cmd dmsetup status",
	"cmd nvme list-subsys", "read ", "readproto ", "readblock ",
}

// Mutations returns only the calls that change the system.
func (f *fakeNode) Mutations() []string {
	var out []string
	for _, call := range f.Calls() {
		mutating := true
		for _, prefix := range readOnlyPrefixes {
			if strings.HasPrefix(call, prefix) {
				mutating = false
				break
			}
		}
		if mutating {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeNode) callsMatching(substr string) []string {
	var out []string
	for _, call := range f.Calls() {
		if strings.Contains(call, substr) {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeNode) hasCall(substr string) bool {
	return len(f.callsMatching(substr)) > 0
}

// indexOfCall returns the position of the first call containing substr, or -1.
func (f *fakeNode) indexOfCall(substr string) int {
	return f.indexOfCallFrom(substr, 0)
}

// indexOfCallFrom is indexOfCall starting at from — what makes assertOrder a
// subsequence match, so a sequence may name the same call shape twice (two
// volume-table slot writes around a blkdiscard, say).
func (f *fakeNode) indexOfCallFrom(substr string, from int) int {
	calls := f.Calls()
	for i := from; i < len(calls); i++ {
		if strings.Contains(calls[i], substr) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// OsClient surface
// ---------------------------------------------------------------------------

func (f *fakeNode) readFile(ctx context.Context, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("read %s", path)
	data, ok := f.files[path]
	if !ok {
		return "", fmt.Errorf("no such file: %s", path)
	}
	return data, nil
}

func (f *fakeNode) writeFile(
	ctx context.Context, path string, data string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("write %s=%s", path, data)
	f.files[path] = data
	return nil
}

func (f *fakeNode) writeFileDirect(
	ctx context.Context, path string, data string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writedirect %s=%s", path, data)
	if !f.dirs[parentDir(path)] {
		return fmt.Errorf("no such directory: %s", parentDir(path))
	}
	f.files[path] = data
	return nil
}

func (f *fakeNode) readProto(
	ctx context.Context, path string, target proto.Message,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("readproto %s", path)
	raw, ok := f.protos[path]
	if !ok {
		return fmt.Errorf("no such file: %s", path)
	}
	return proto.Unmarshal(raw, target)
}

func (f *fakeNode) writeProto(
	ctx context.Context, path string, msg proto.Message,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writeproto %s", path)
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	f.protos[path] = raw
	return nil
}

// readBlock / writeBlock model the raw --disk device the [D13] metadata lives
// on: a device always exists at its full size, an unwritten region reads as
// zeros, and a read that runs past the end is a short read (an error).
func (f *fakeNode) readBlock(
	ctx context.Context, path string, offset uint64, length uint64,
) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("readblock %s off=%d len=%d", path, offset, length)
	size, ok := f.devSize[path]
	if !ok {
		return nil, fmt.Errorf("no such device: %s", path)
	}
	if offset+length > size {
		return nil, fmt.Errorf("short read: %s off=%d len=%d size=%d",
			path, offset, length, size)
	}
	out := make([]byte, length)
	for _, seg := range f.blocks[path] {
		lo := max64(seg.off, offset)
		hi := min64(seg.off+uint64(len(seg.data)), offset+length)
		if lo >= hi {
			continue
		}
		copy(out[lo-offset:hi-offset], seg.data[lo-seg.off:hi-seg.off])
	}
	return out, nil
}

func (f *fakeNode) writeBlock(
	ctx context.Context, path string, offset uint64, data []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("writeblock %s off=%d len=%d", path, offset, len(data))
	if f.failBlockSet && offset == f.failBlockWrite {
		return fmt.Errorf("fake: write block at %d failed", offset)
	}
	size, ok := f.devSize[path]
	if !ok {
		return fmt.Errorf("no such device: %s", path)
	}
	if offset+uint64(len(data)) > size {
		return fmt.Errorf("short write: %s off=%d len=%d size=%d",
			path, offset, len(data), size)
	}
	f.blocks[path] = append(f.blocks[path], fakeSegment{
		off: offset, data: append([]byte(nil), data...),
	})
	return nil
}

// corruptBlock flips bytes behind the agent's back, so tests can build a torn
// slot or a damaged header.
func (f *fakeNode) corruptBlock(path string, offset uint64, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks[path] = append(f.blocks[path], fakeSegment{
		off: offset, data: append([]byte(nil), data...),
	})
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func parentDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "/"
	}
	return path[:idx]
}

// ---------------------------------------------------------------------------
// Command dispatch
// ---------------------------------------------------------------------------

func (f *fakeNode) runCommand(
	ctx context.Context,
	name string,
	args []string,
	stdin string,
) (string, string, int, error) {
	line := "cmd " + name + " " + strings.Join(args, " ")
	if stdin != "" {
		// A multi-line table travels through stdin (dmsetup's --table is
		// single-line only); fold it onto the call string so assertOrder can
		// match on the table the agent actually sent.
		line += " stdin=" + strings.Join(dmTargets(stdin), " | ")
	}

	f.mu.Lock()
	f.record("%s", line)
	var gate chan struct{}
	for key, ch := range f.gate {
		if strings.Contains(line, key) {
			gate = ch
			break
		}
	}
	for key, stderr := range f.failCmd {
		if strings.Contains(line, key) {
			delete(f.failCmd, key)
			f.mu.Unlock()
			return "", stderr, 1, fmt.Errorf("exit status 1")
		}
	}
	for key, stderr := range f.failCmdAlways {
		if strings.Contains(line, key) {
			f.mu.Unlock()
			return "", stderr, 1, fmt.Errorf("exit status 1")
		}
	}
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", "", -1, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	stdout, code := f.dispatch(name, args, stdin)
	if code != 0 {
		return stdout, "fake: " + name + " failed", code,
			fmt.Errorf("exit status %d", code)
	}
	return stdout, "", 0, nil
}

func (f *fakeNode) dispatch(
	name string, args []string, stdin string,
) (string, int) {
	switch name {
	case "ls":
		return f.cmdLs(args)
	case "rm":
		return f.cmdRm(args)
	case "mkdir":
		return f.cmdMkdir(args)
	case "rmdir":
		return f.cmdRmdir(args)
	case "ln":
		return f.cmdLn(args)
	case "lsblk":
		return f.cmdLsblk(args)
	case "blkdiscard":
		return f.cmdBlkdiscard(args)
	case "dmsetup":
		return f.cmdDmsetup(args, stdin)
	case "nvme":
		return f.cmdNvme(args)
	}
	return "", 127
}

func (f *fakeNode) children(path string) []string {
	seen := make(map[string]struct{})
	for _, set := range []map[string]bool{f.dirs} {
		for entry := range set {
			if child, ok := childOf(path, entry); ok {
				seen[child] = struct{}{}
			}
		}
	}
	for entry := range f.files {
		if child, ok := childOf(path, entry); ok {
			seen[child] = struct{}{}
		}
	}
	for entry := range f.links {
		if child, ok := childOf(path, entry); ok {
			seen[child] = struct{}{}
		}
	}
	if path == common.DefaultLocalStorPrefix {
		for entry := range f.protos {
			if child, ok := childOf(path, entry); ok {
				seen[child] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for child := range seen {
		out = append(out, child)
	}
	sort.Strings(out)
	return out
}

func childOf(dir, entry string) (string, bool) {
	prefix := dir + "/"
	if !strings.HasPrefix(entry, prefix) {
		return "", false
	}
	rest := entry[len(prefix):]
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func (f *fakeNode) cmdLs(args []string) (string, int) {
	path := args[len(args)-1]
	if !f.dirs[path] {
		return "", 2
	}
	return strings.Join(f.children(path), "\n") + "\n", 0
}

func (f *fakeNode) cmdRm(args []string) (string, int) {
	for _, path := range args {
		if strings.HasPrefix(path, "-") {
			continue
		}
		delete(f.files, path)
		delete(f.links, path)
		delete(f.protos, path)
	}
	return "", 0
}

func (f *fakeNode) cmdMkdir(args []string) (string, int) {
	path := args[len(args)-1]
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	cur := ""
	for _, part := range parts {
		cur += "/" + part
		if !f.dirs[cur] {
			f.dirs[cur] = true
			f.autoCreate(cur)
		}
	}
	return "", 0
}

// autoCreate mirrors configfs: creating an nvmet port or subsystem directory
// makes the kernel populate its standard subdirectories (a port's
// subsystems/ and ana_groups/1, a subsystem's namespaces/ and
// allowed_hosts/). Attribute files appear only once written, which is what
// makes a fresh node's probes read empty.
func (f *fakeNode) autoCreate(path string) {
	rest, ok := strings.CutPrefix(path, agent.NvmetRoot+"/")
	if !ok {
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 2 && parts[0] == "ports":
		for _, child := range []string{
			"subsystems", "referrals", "ana_groups", "ana_groups/1",
		} {
			f.dirs[path+"/"+child] = true
		}
	case len(parts) == 2 && parts[0] == "subsystems":
		for _, child := range []string{"namespaces", "allowed_hosts"} {
			f.dirs[path+"/"+child] = true
		}
	}
}

// cmdRmdir models configfs: removing a directory takes its whole subtree
// (attributes and auto-created children) with it.
func (f *fakeNode) cmdRmdir(args []string) (string, int) {
	path := args[len(args)-1]
	if !f.dirs[path] {
		return "", 1
	}
	prefix := path + "/"
	for _, set := range []map[string]bool{f.dirs} {
		for entry := range set {
			if strings.HasPrefix(entry, prefix) {
				delete(set, entry)
			}
		}
	}
	for entry := range f.files {
		if strings.HasPrefix(entry, prefix) {
			delete(f.files, entry)
		}
	}
	for entry := range f.links {
		if strings.HasPrefix(entry, prefix) {
			delete(f.links, entry)
		}
	}
	delete(f.dirs, path)
	return "", 0
}

func (f *fakeNode) cmdLn(args []string) (string, int) {
	target, link := args[len(args)-2], args[len(args)-1]
	if _, ok := f.links[link]; ok {
		return "", 1
	}
	f.links[link] = target
	return "", 0
}

func (f *fakeNode) cmdLsblk(args []string) (string, int) {
	path := args[len(args)-1]
	if contains(args, "SIZE") {
		size, ok := f.devSize[path]
		if !ok {
			return "", 32
		}
		return fmt.Sprintf("%d\n", size), 0
	}
	devNo, ok := f.devNo[path]
	if !ok {
		return "", 32
	}
	return devNo + "  \n", 0
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func (f *fakeNode) newDevNo() string {
	f.nextMinor++
	return fmt.Sprintf("253:%d", f.nextMinor)
}

func (f *fakeNode) cmdBlkdiscard(args []string) (string, int) {
	dev := args[len(args)-1]
	if !contains(args, "--force") {
		name := strings.TrimPrefix(dev, "/dev/mapper/")
		if dm, ok := f.dms[name]; ok {
			dm.discards = append(dm.discards,
				strings.Join(args[:len(args)-1], " "))
		}
	}
	return "", 0
}

func (f *fakeNode) cmdDmsetup(args []string, stdin string) (string, int) {
	if len(args) == 0 {
		return "", 3
	}
	switch args[0] {
	case "info":
		name := args[len(args)-1]
		dm, ok := f.dms[name]
		if !ok {
			return "", 1
		}
		// The real four-position attr column: live, inactive-table,
		// suspended, read-only/read-write — e.g. "L--w", "L-sw", "L--r".
		attr := []byte("L---")
		if dm.suspended {
			attr[2] = 's'
		}
		if dm.readOnly {
			attr[3] = 'r'
		} else {
			attr[3] = 'w'
		}
		return string(attr) + "\n", 0
	case "table":
		dm, ok := f.dms[args[1]]
		if !ok {
			return "", 1
		}
		return strings.Join(dmTargets(dm.table), "\n") + "\n", 0
	case "status":
		return f.dmStatus(args[1])
	case "create":
		return f.dmCreate(args, stdin)
	case "reload":
		return f.dmReload(args, stdin)
	case "suspend", "resume":
		dm, ok := f.dms[args[1]]
		if !ok {
			return "", 1
		}
		dm.suspended = args[0] == "suspend"
		return "", 0
	case "remove":
		name := args[1]
		if _, ok := f.dms[name]; !ok {
			return "", 1
		}
		delete(f.dms, name)
		path := "/dev/mapper/" + name
		delete(f.devNo, path)
		delete(f.devSize, path)
		return "", 0
	case "message":
		return f.dmMessage(args)
	}
	return "", 3
}

// parseDmCreateArgs accepts both table forms: the single-line --table option
// and the multi-line stdin form dmsetup reads when --table is absent
// (architecture.md Appendix A). The stdin table is normalized to one space-
// joined line per target, keeping the parsing below single-line.
func parseDmCreateArgs(args []string, stdin string) (string, string, bool) {
	name := args[1]
	table := ""
	readOnly := false
	hasTableFlag := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--table":
			i++
			table = args[i]
			hasTableFlag = true
		case "--readonly":
			readOnly = true
		}
	}
	if !hasTableFlag {
		table = strings.TrimRight(stdin, "\n")
	}
	return name, table, readOnly
}

// dmTargets splits a table into its target lines.
func dmTargets(table string) []string {
	var out []string
	for _, line := range strings.Split(table, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func (f *fakeNode) dmCreate(args []string, stdin string) (string, int) {
	name, table, readOnly := parseDmCreateArgs(args, stdin)
	if table == "" {
		return "", 3
	}
	if _, exists := f.dms[name]; exists {
		return "", 1
	}
	dm := &fakeDm{table: table, readOnly: readOnly}
	applyCloneTable(dm, table)
	f.dms[name] = dm
	path := "/dev/mapper/" + name
	f.devNo[path] = f.newDevNo()
	f.devSize[path] = tableSectors(table) * 512
	return "", 0
}

func (f *fakeNode) dmReload(args []string, stdin string) (string, int) {
	name, table, readOnly := parseDmCreateArgs(args, stdin)
	if table == "" {
		return "", 3
	}
	dm, ok := f.dms[name]
	if !ok {
		return "", 1
	}
	dm.table = table
	dm.readOnly = readOnly
	applyCloneTable(dm, table)
	f.devSize["/dev/mapper/"+name] = tableSectors(table) * 512
	return "", 0
}

// tableSectors is the device's size: the sum of its targets' lengths.
func tableSectors(table string) uint64 {
	var total uint64
	for _, line := range dmTargets(table) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sectors, _ := strconv.ParseUint(fields[1], 10, 64)
		total += sectors
	}
	return total
}

func applyCloneTable(dm *fakeDm, table string) {
	lines := dmTargets(table)
	if len(lines) != 1 {
		return
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 3 || fields[2] != "clone" {
		return
	}
	dm.noHydration = contains(fields, "no_hydration")
	for i := 0; i+1 < len(fields); i++ {
		value, err := strconv.ParseUint(fields[i+1], 10, 32)
		if err != nil {
			continue
		}
		switch fields[i] {
		case "hydration_threshold":
			dm.threshold = uint32(value)
		case "hydration_batch_size":
			dm.batchSize = uint32(value)
		}
	}
}

func (f *fakeNode) dmMessage(args []string) (string, int) {
	dm, ok := f.dms[args[1]]
	if !ok {
		return "", 1
	}
	message := strings.Join(args[3:], " ")
	switch {
	case message == "enable_hydration":
		dm.noHydration = false
	case message == "disable_hydration":
		dm.noHydration = true
	case strings.HasPrefix(message, "hydration_threshold "):
		value, _ := strconv.ParseUint(strings.Fields(message)[1], 10, 32)
		dm.threshold = uint32(value)
	case strings.HasPrefix(message, "hydration_batch_size "):
		value, _ := strconv.ParseUint(strings.Fields(message)[1], 10, 32)
		dm.batchSize = uint32(value)
	}
	return "", 0
}

func (f *fakeNode) dmStatus(name string) (string, int) {
	dm, ok := f.dms[name]
	if !ok {
		return "", 1
	}
	lines := dmTargets(dm.table)
	if len(lines) == 0 {
		return "", 1
	}
	if len(lines) > 1 {
		// A multi-segment side device: report each line back. dmsetup
		// status and table agree for a linear target.
		var sb strings.Builder
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 5 || fields[2] != "linear" {
				return "", 1
			}
			fmt.Fprintf(&sb, "%s %s linear %s %s\n",
				fields[0], fields[1], fields[3], fields[4])
		}
		return sb.String(), 0
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 3 {
		return "", 1
	}
	sectors := fields[1]
	switch fields[2] {
	case "error":
		return fmt.Sprintf("0 %s error\n", sectors), 0
	case "linear":
		return fmt.Sprintf("0 %s linear %s %s\n",
			sectors, fields[3], fields[4]), 0
	case "clone":
		regionSectors := fields[6]
		regions := tableSectors(dm.table)
		region, _ := strconv.ParseUint(regionSectors, 10, 64)
		if region > 0 {
			regions /= region
		}
		features := "0"
		if dm.noHydration {
			features = "1 no_hydration"
		}
		return fmt.Sprintf(
			"0 %s clone 8 1/1024 %s 0/%d 0 %s 4 hydration_threshold %d "+
				"hydration_batch_size %d\n",
			sectors, regionSectors, regions, features,
			dm.threshold, dm.batchSize), 0
	}
	return "", 1
}

func (f *fakeNode) cmdNvme(args []string) (string, int) {
	switch args[0] {
	case "connect":
		var nqn string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--nqn" {
				nqn = args[i+1]
			}
		}
		if nqn == "" {
			return "", 3
		}
		device := fmt.Sprintf("nvme%dn1", len(f.conns))
		f.conns[nqn] = &fakeConn{device: device, state: "live"}
		f.devNo["/dev/"+device] = f.newDevNo()
		return "", 0
	case "disconnect":
		var nqn string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--nqn" {
				nqn = args[i+1]
			}
		}
		conn, ok := f.conns[nqn]
		if !ok {
			return "", 1
		}
		delete(f.devNo, "/dev/"+conn.device)
		delete(f.conns, nqn)
		return "", 0
	case "list-subsys":
		return f.listSubsysJson(), 0
	}
	return "", 3
}

func (f *fakeNode) listSubsysJson() string {
	nqns := make([]string, 0, len(f.conns))
	for nqn := range f.conns {
		nqns = append(nqns, nqn)
	}
	sort.Strings(nqns)
	subsystems := make([]map[string]any, 0, len(nqns))
	for i, nqn := range nqns {
		conn := f.conns[nqn]
		subsystems = append(subsystems, map[string]any{
			"Name": fmt.Sprintf("nvme-subsys%d", i),
			"NQN":  nqn,
			"Paths": []map[string]any{
				{"Name": conn.device, "State": conn.state},
			},
			"Namespaces": []map[string]any{
				{"NameSpace": conn.device, "NSID": 1},
			},
		})
	}
	raw, _ := json.Marshal([]map[string]any{
		{"HostNQN": "fake", "Subsystems": subsystems},
	})
	return string(raw)
}
