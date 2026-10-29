package csi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// fakeNode is the node plugin's scripted node (csi.md, Integration test
// plan): a fake OS client over an in-memory sysfs, mount table, by-id
// directory and command table. nvme-cli's commands move controllers and
// links in the maps; the mount tools move the mount table.
type fakeNode struct {
	mu       sync.Mutex
	files    map[string]string     // ReadFile answers beyond sysfs and mountinfo
	dirs     map[string]bool       // directories mkdir made
	ctrls    map[string]ctrl       // controller name -> controller
	nextCtrl int                   // next controller number
	links    map[string]bool       // by-id links present (ReadDevNo answers)
	uuidOf   map[string]string     // subsystem NQN -> namespace uuid, for the link
	mounts   []mountEntry          // the mount table
	fsOf     map[string]string     // blkid's answer per device
	ptOf     map[string]string     // blkid's partition table per device
	blkidErr map[string]string     // blkid's complaint per device it cannot read
	logs     map[string][]logEntry // discovery log per cdc endpoint
	logFail  map[string]bool       // cdcs whose log read fails
	discFail map[string]bool       // cdcs a discovery connect fails to
	refuse   map[string]int        // endpoint -> connects still refused
	sizes    map[string]uint64     // blockdev --getsize64 per device
	mountErr bool                  // a mount of a device fails
	calls    []string              // every command, joined
	statLine string                // stat -f's answer
	// onCommand, when set, runs before every command, outside the lock,
	// so a test can act in the middle of an operation.
	onCommand func(name string, args []string)
}

const (
	fakeCdcA = "10.0.0.78:18030"
	fakeCdcB = "10.0.0.79:18030"
	fakeCn0  = "10.0.0.125:4300"
	fakeCn1  = "10.0.0.229:4300"
	fakeCn2  = "10.0.0.77:4300"
)

func newFakeNode() *fakeNode {
	f := &fakeNode{
		files: map[string]string{
			DefaultHostNqnPath: testNodeA + "\n",
			DefaultHostIdPath:  "1a788e0a-02bd-4ae9-9a4b-e4aad8b04c26\n",
		},
		dirs:     map[string]bool{},
		ctrls:    map[string]ctrl{},
		links:    map[string]bool{},
		uuidOf:   map[string]string{},
		fsOf:     map[string]string{},
		ptOf:     map[string]string{},
		blkidErr: map[string]string{},
		logs:     map[string][]logEntry{},
		logFail:  map[string]bool{},
		discFail: map[string]bool{},
		refuse:   map[string]int{},
		sizes:    map[string]uint64{},
		statLine: "4096 1000 500 400 100 50",
	}
	return f
}

// addCtrl plants a controller under the node's host NQN; a path to a
// subsystem with a known uuid also makes the device link appear, as a live
// optimized path does.
func (f *fakeNode) addCtrl(subNqn string, ep endpoint) string {
	return f.addCtrlAs(subNqn, ep, testNodeA)
}

// addCtrlAs plants a controller made under a given host NQN.
func (f *fakeNode) addCtrlAs(subNqn string, ep endpoint, hostNqn string) string {
	name := fmt.Sprintf("nvme%d", f.nextCtrl)
	f.nextCtrl++
	f.ctrls[name] = ctrl{name: name, subNqn: subNqn, trAddr: ep.trAddr,
		trSvcId: ep.trSvcId, state: "live", hostNqn: hostNqn}
	if uuid, ok := f.uuidOf[subNqn]; ok {
		f.links[devicePath(uuid)] = true
	}
	return name
}

// dropCtrl removes a controller; the last path of a subsystem takes the
// device link with it.
func (f *fakeNode) dropCtrl(name string) {
	c, ok := f.ctrls[name]
	if !ok {
		return
	}
	delete(f.ctrls, name)
	for _, other := range f.ctrls {
		if other.subNqn == c.subNqn {
			return
		}
	}
	if uuid, ok := f.uuidOf[c.subNqn]; ok {
		delete(f.links, devicePath(uuid))
	}
}

func (f *fakeNode) ctrlNames() []string {
	names := make([]string, 0, len(f.ctrls))
	for name := range f.ctrls {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// paths lists the endpoints the node holds a path to for a subsystem.
func (f *fakeNode) paths(subNqn string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.ctrls {
		if c.subNqn == subNqn {
			out = append(out, c.key())
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeNode) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeNode) mounted(target string) *mountEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.mounts {
		if f.mounts[i].target == target {
			return &f.mounts[i]
		}
	}
	return nil
}

func notExist(path string) error {
	return fmt.Errorf("%s: %w", path, fs.ErrNotExist)
}

// argument reads the value after a flag in an argument list.
func argument(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeNode) client() common.OsClient {
	return &common.FakeOsClient{
		RunCommandFn: f.runCommand,
		ReadFileFn: func(ctx context.Context, path string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if path == mountInfoPath {
				var b strings.Builder
				for i, m := range f.mounts {
					fmt.Fprintf(&b, "%d 1 0:1 / %s rw - %s %s rw\n", 100+i,
						m.target, m.fsType, m.source)
				}
				return b.String(), nil
			}
			if strings.HasPrefix(path, sysClassNvme+"/") {
				rest := strings.TrimPrefix(path, sysClassNvme+"/")
				name, attr, _ := strings.Cut(rest, "/")
				c, ok := f.ctrls[name]
				if !ok {
					return "", notExist(path)
				}
				switch attr {
				case "subsysnqn":
					return c.subNqn + "\n", nil
				case "address":
					return "traddr=" + c.trAddr + ",trsvcid=" + c.trSvcId +
						",src_addr=10.0.0.193\n", nil
				case "state":
					return c.state + "\n", nil
				case "hostnqn":
					if c.hostNqn != "" {
						return c.hostNqn + "\n", nil
					}
				}
				return "", notExist(path)
			}
			if data, ok := f.files[path]; ok {
				return data, nil
			}
			return "", notExist(path)
		},
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if path == sysClassNvme {
				return f.ctrlNames(), nil
			}
			if f.dirs[path] {
				return nil, nil
			}
			if _, ok := f.files[path]; ok {
				return nil, errors.New("not a directory")
			}
			return nil, notExist(path)
		},
		ReadDevNoFn: func(ctx context.Context, path string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.links[path] {
				return "259:1", nil
			}
			return "", notExist(path)
		},
		WriteFileFn: func(ctx context.Context, path string, data string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.dirs[path] {
				return errors.New("is a directory")
			}
			f.files[path] = data
			return nil
		},
	}
}

// runCommand is the command table.
func (f *fakeNode) runCommand(
	ctx context.Context, name string, args []string, stdin string,
) (string, string, int, error) {
	if f.onCommand != nil {
		f.onCommand(name, args)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	fail := func(code int, msg string) (string, string, int, error) {
		return "", msg, code, fmt.Errorf("exit %d", code)
	}
	switch name {
	case nvmeBin:
		switch args[0] {
		case "connect":
			ep := endpoint{trAddr: argument(args, "-a"), trSvcId: argument(args, "-s")}
			if argument(args, "--hostnqn") != testNodeA || argument(args, "--hostid") == "" {
				return fail(1, "the connect carries no node identity")
			}
			if f.refuse[ep.String()] > 0 {
				f.refuse[ep.String()]--
				return fail(1, "Failed to write to /dev/nvme-fabrics: Connection refused")
			}
			nqn := argument(args, "-n")
			for _, c := range f.ctrls {
				if c.subNqn == nqn && c.key() == ep.String() {
					return fail(1, "already connected")
				}
			}
			f.addCtrl(nqn, ep)
			return "", "", 0, nil
		case "discover":
			if dev := argument(args, "-d"); dev != "" {
				c, ok := f.ctrls[dev]
				if !ok {
					return fail(1, "no such device")
				}
				if f.logFail[c.key()] {
					return fail(1, "discovery failed")
				}
				var records []map[string]string
				for _, e := range f.logs[c.key()] {
					records = append(records, map[string]string{
						"trtype": "tcp", "adrfam": "ipv4", "subtype": "nvme subsystem",
						"trsvcid": e.ep.trSvcId, "traddr": e.ep.trAddr, "subnqn": e.subNqn,
					})
				}
				records = append(records, map[string]string{
					"trtype": "tcp", "subtype": "discovery subsystem referral",
					"trsvcid": "8009", "traddr": "10.0.0.1",
					"subnqn": common.NvmeDiscoveryNqn,
				})
				doc, _ := json.Marshal(map[string]any{"device": dev, "records": records})
				return string(doc), "", 0, nil
			}
			ep := endpoint{trAddr: argument(args, "-a"), trSvcId: argument(args, "-s")}
			if f.discFail[ep.String()] {
				return fail(1, "Failed to write to /dev/nvme-fabrics: Connection refused")
			}
			f.addCtrlAs(common.NvmeDiscoveryNqn, ep, argument(args, "--hostnqn"))
			return "", "", 0, nil
		case "disconnect":
			if dev := argument(args, "-d"); dev != "" {
				f.dropCtrl(dev)
				return "", "", 0, nil
			}
			nqn := argument(args, "-n")
			for _, name := range f.ctrlNames() {
				if f.ctrls[name].subNqn == nqn {
					f.dropCtrl(name)
				}
			}
			return "", "", 0, nil
		}
	case "blkid":
		dev := args[len(args)-1]
		if msg := f.blkidErr[dev]; msg != "" {
			return fail(2, msg)
		}
		var out string
		if fs := f.fsOf[dev]; fs != "" {
			out += "TYPE=" + fs + "\n"
		}
		if pt := f.ptOf[dev]; pt != "" {
			out += "PTTYPE=" + pt + "\n"
		}
		if out == "" {
			return fail(2, "")
		}
		return "DEVNAME=" + dev + "\n" + out, "", 0, nil
	case "mkfs.ext4", "mkfs.xfs":
		f.fsOf[args[len(args)-1]] = strings.TrimPrefix(name, "mkfs.")
		return "", "", 0, nil
	case "mkdir":
		f.dirs[args[len(args)-1]] = true
		return "", "", 0, nil
	case "mount":
		switch {
		case args[0] == "--bind":
			f.mounts = append(f.mounts, mountEntry{target: args[2], source: args[1], fsType: "bind"})
		case args[0] == "-o" && strings.HasPrefix(args[1], "remount"):
		case f.mountErr:
			return fail(32, "mount: wrong fs type, bad option")
		default:
			f.mounts = append(f.mounts, mountEntry{
				target: args[len(args)-1], source: args[len(args)-2],
				fsType: argument(args, "-t"),
			})
		}
		return "", "", 0, nil
	case "umount":
		for i, m := range f.mounts {
			if m.target == args[0] {
				f.mounts = append(f.mounts[:i], f.mounts[i+1:]...)
				return "", "", 0, nil
			}
		}
		return fail(32, "not mounted")
	case "rmdir":
		delete(f.dirs, args[0])
		return "", "", 0, nil
	case "rm":
		delete(f.files, args[len(args)-1])
		return "", "", 0, nil
	case "stat":
		return f.statLine + "\n", "", 0, nil
	case "blockdev":
		return fmt.Sprintf("%d\n", f.sizes[args[len(args)-1]]), "", 0, nil
	}
	return fail(127, "unknown command "+name)
}

// newTestNode builds a node plugin over a fake node whose cdc lists the
// test volume on two cntlrs.
func newTestNode(t *testing.T) (*node, *fakeNode) {
	t.Helper()
	f := newFakeNode()
	nqn := subsystemNqnOf(testVolume)
	uuid := namespaceUuidOf(testVolume)
	f.uuidOf[nqn] = uuid
	f.logs[fakeCdcA] = []logEntry{
		{subNqn: nqn, ep: mustEndpoint(fakeCn0)},
		{subNqn: nqn, ep: mustEndpoint(fakeCn1)},
	}
	f.sizes[devicePath(uuid)] = 10 << 30
	oc := f.client()
	id, err := readIdentity(t.Context(), oc, DefaultHostNqnPath, DefaultHostIdPath)
	if err != nil {
		t.Fatal(err)
	}
	host := &nvmeHost{oc: oc, id: id}
	n := &node{
		id:         id,
		host:       host,
		follow:     newFollower(host, time.Hour, nil),
		mnt:        &mounter{oc: oc},
		deviceWait: 3 * time.Second,
	}
	return n, f
}

func mustEndpoint(s string) endpoint {
	ep, err := parseEndpoint(s)
	if err != nil {
		panic(err)
	}
	return ep
}

func stageReq(cap *csi.VolumeCapability) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          fakeClusterOne + "/" + testVolume,
		StagingTargetPath: "/var/lib/kubelet/plugins/kubernetes.io/csi/staging/" + testVolume,
		VolumeCapability:  cap,
		PublishContext: map[string]string{
			ctxNqn:  subsystemNqnOf(testVolume),
			ctxUuid: namespaceUuidOf(testVolume),
			ctxCdc:  fakeCdcA,
		},
	}
}

// TestReadIdentity pins csi.md CSI13: both files present and well-formed,
// or no start.
func TestReadIdentity(t *testing.T) {
	ctx := t.Context()
	f := newFakeNode()
	id, err := readIdentity(ctx, f.client(), DefaultHostNqnPath, DefaultHostIdPath)
	if err != nil || id.hostNqn != testNodeA || id.hostId == "" {
		t.Fatalf("readIdentity = %+v, %v", id, err)
	}
	delete(f.files, DefaultHostIdPath)
	if _, err := readIdentity(ctx, f.client(), DefaultHostNqnPath, DefaultHostIdPath); err == nil ||
		!strings.Contains(err.Error(), DefaultHostIdPath) {
		t.Errorf("a missing host id file: %v", err)
	}
	f.files[DefaultHostIdPath] = "\n"
	if _, err := readIdentity(ctx, f.client(), DefaultHostNqnPath, DefaultHostIdPath); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty host id file: %v", err)
	}
	f.files[DefaultHostIdPath] = "x"
	f.files[DefaultHostNqnPath] = "not-an-nqn"
	if _, err := readIdentity(ctx, f.client(), DefaultHostNqnPath, DefaultHostIdPath); err == nil ||
		!strings.Contains(err.Error(), "host NQN") {
		t.Errorf("a host NQN file without an NQN: %v", err)
	}
}

// TestNodeStageMountsAFilesystemVolume pins csi.md CSI14 and CSI17: the
// discovery connection, a path to each listed cntlr, the wait for the
// link, the format of a blank device, the mount, and a repeat that does
// nothing twice.
func TestNodeStageMountsAFilesystemVolume(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	req := stageReq(mountCap(fsExt4, rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")
	nqn := subsystemNqnOf(testVolume)
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn0+" "+fakeCn1 {
		t.Errorf("paths %v, want both cntlrs", got)
	}
	if got := f.paths(common.NvmeDiscoveryNqn); strings.Join(got, " ") != fakeCdcA {
		t.Errorf("discovery connections %v, want the cdc", got)
	}
	if f.count("mkfs.ext4") != 1 {
		t.Errorf("mkfs.ext4 ran %d times, want 1", f.count("mkfs.ext4"))
	}
	m := f.mounted(req.GetStagingTargetPath())
	if m == nil || m.fsType != fsExt4 || m.source != devicePath(namespaceUuidOf(testVolume)) {
		t.Errorf("staging mount %+v", m)
	}
	connects := f.count(nvmeBin + " connect")
	_, err = n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage again")
	if f.count("mkfs.ext4") != 1 || f.count("mount") != 1 || f.count(nvmeBin+" connect") != connects {
		t.Errorf("the repeat did work twice: %v", f.calls)
	}
}

// TestNodeStageRetriesARefusedConnect pins the retry of csi.md CSI14: a
// cntlr whose port does not listen yet refuses, and the stage tries again
// within its wait.
func TestNodeStageRetriesARefusedConnect(t *testing.T) {
	n, f := newTestNode(t)
	f.refuse[fakeCn0] = 1
	f.refuse[fakeCn1] = 1
	_, err := n.NodeStageVolume(t.Context(), stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage with a refused first connect")
	if got := f.paths(subsystemNqnOf(testVolume)); len(got) != 2 {
		t.Errorf("paths %v, want both cntlrs after the retry", got)
	}
	if f.count("mkfs") != 0 || f.count("mount") != 0 {
		t.Errorf("a block volume was formatted or mounted: %v", f.calls)
	}
}

// TestNodeStageRefuses pins the stage's refusals.
func TestNodeStageRefuses(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	noId := stageReq(mountCap(fsExt4, rwo))
	noId.VolumeId = ""
	_, err := n.NodeStageVolume(ctx, noId)
	wantCode(t, err, codes.InvalidArgument, "no volume id")
	noPath := stageReq(mountCap(fsExt4, rwo))
	noPath.StagingTargetPath = ""
	_, err = n.NodeStageVolume(ctx, noPath)
	wantCode(t, err, codes.InvalidArgument, "no staging path")
	noCap := stageReq(nil)
	_, err = n.NodeStageVolume(ctx, noCap)
	wantCode(t, err, codes.InvalidArgument, "no capability")
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)))
	wantCode(t, err, codes.FailedPrecondition, "a multi-node mode")
	noCdc := stageReq(mountCap(fsExt4, rwo))
	noCdc.PublishContext = map[string]string{ctxNqn: "x", ctxUuid: "y"}
	_, err = n.NodeStageVolume(ctx, noCdc)
	wantCode(t, err, codes.InvalidArgument, "no cdc address")
	// The device carries another filesystem: never reformatted.
	f.fsOf[devicePath(namespaceUuidOf(testVolume))] = fsXfs
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.FailedPrecondition, "a device with another filesystem")
	if f.count("mkfs") != 0 {
		t.Error("a device with a filesystem was reformatted")
	}
	// blkid cannot open the device: no answer, so no format.
	f.blkidErr[devicePath(namespaceUuidOf(testVolume))] = "error: " +
		devicePath(namespaceUuidOf(testVolume)) + ": Input/output error"
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.Internal, "a device blkid cannot open")
	if f.count("mkfs") != 0 {
		t.Error("a device blkid could not open was formatted")
	}
	f.blkidErr = map[string]string{}
	// The device carries a partition table and no filesystem: not blank.
	f.fsOf[devicePath(namespaceUuidOf(testVolume))] = ""
	f.ptOf[devicePath(namespaceUuidOf(testVolume))] = "gpt"
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.FailedPrecondition, "a device with a partition table")
	if f.count("mkfs") != 0 {
		t.Error("a device with a partition table was formatted")
	}
	// No cdc lists the volume: the link never comes.
	f.logs[fakeCdcA] = nil
	other := stageReq(mountCap(fsExt4, rwo))
	other.VolumeId = fakeClusterOne + "/pvc-other"
	other.PublishContext[ctxNqn] = subsystemNqnOf("pvc-other")
	other.PublishContext[ctxUuid] = namespaceUuidOf("pvc-other")
	n.deviceWait = time.Second
	_, err = n.NodeStageVolume(ctx, other)
	wantCode(t, err, codes.Unavailable, "a volume no cdc lists")
}

// TestNodePublishAndUnpublish pins csi.md CSI17 for a filesystem and a
// block volume, read-only included, each repeated, and the unpublish.
func TestNodePublishAndUnpublish(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	fsReq := stageReq(mountCap(fsExt4, rwo))
	_, err := n.NodeStageVolume(ctx, fsReq)
	wantCode(t, err, codes.OK, "stage")
	target := "/var/lib/kubelet/pods/p1/volumes/kubernetes.io~csi/" + testVolume + "/mount"
	pub := &csi.NodePublishVolumeRequest{
		VolumeId:          fsReq.GetVolumeId(),
		StagingTargetPath: fsReq.GetStagingTargetPath(),
		TargetPath:        target,
		VolumeCapability:  fsReq.GetVolumeCapability(),
		PublishContext:    fsReq.GetPublishContext(),
		Readonly:          true,
	}
	_, err = n.NodePublishVolume(ctx, pub)
	wantCode(t, err, codes.OK, "publish read-only")
	if m := f.mounted(target); m == nil || m.source != fsReq.GetStagingTargetPath() {
		t.Errorf("target mount %+v", m)
	}
	if f.count("mount -o remount,bind,ro") != 1 {
		t.Errorf("the read-only remount ran %d times", f.count("mount -o remount,bind,ro"))
	}
	binds := f.count("mount --bind")
	_, err = n.NodePublishVolume(ctx, pub)
	wantCode(t, err, codes.OK, "publish again")
	if f.count("mount --bind") != binds {
		t.Error("the repeat mounted again")
	}
	_, err = n.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: pub.GetVolumeId(), TargetPath: target,
	})
	wantCode(t, err, codes.OK, "unpublish")
	if f.mounted(target) != nil || f.dirs[target] {
		t.Error("the target is still there after the unpublish")
	}
	_, err = n.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: pub.GetVolumeId(), TargetPath: target,
	})
	wantCode(t, err, codes.OK, "unpublish again")

	t.Run("block", func(t *testing.T) {
		n, f := newTestNode(t)
		blk := stageReq(blockCap(rwo))
		_, err := n.NodeStageVolume(ctx, blk)
		wantCode(t, err, codes.OK, "stage a block volume")
		target := "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/" + testVolume + "/p1"
		pub := &csi.NodePublishVolumeRequest{
			VolumeId:          blk.GetVolumeId(),
			StagingTargetPath: blk.GetStagingTargetPath(),
			TargetPath:        target,
			VolumeCapability:  blk.GetVolumeCapability(),
			PublishContext:    blk.GetPublishContext(),
		}
		_, err = n.NodePublishVolume(ctx, pub)
		wantCode(t, err, codes.OK, "publish a block volume")
		m := f.mounted(target)
		if m == nil || m.source != devicePath(namespaceUuidOf(testVolume)) {
			t.Errorf("block target mount %+v", m)
		}
		if _, ok := f.files[target]; !ok {
			t.Error("no file was made at the block target")
		}
		_, err = n.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
			VolumeId: pub.GetVolumeId(), TargetPath: target,
		})
		wantCode(t, err, codes.OK, "unpublish a block volume")
		if _, ok := f.files[target]; ok || f.mounted(target) != nil {
			t.Error("the block target is still there")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		_, err := n.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId: pub.GetVolumeId(), TargetPath: target,
			VolumeCapability: pub.GetVolumeCapability(),
		})
		wantCode(t, err, codes.FailedPrecondition, "publish without a staging path")
		_, err = n.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId: pub.GetVolumeId(), TargetPath: target,
			StagingTargetPath: pub.GetStagingTargetPath(),
		})
		wantCode(t, err, codes.InvalidArgument, "publish without a capability")
		_, err = n.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId: pub.GetVolumeId(), StagingTargetPath: pub.GetStagingTargetPath(),
			VolumeCapability: pub.GetVolumeCapability(),
		})
		wantCode(t, err, codes.InvalidArgument, "publish without a target")
	})
}

// TestNodeUnstage pins csi.md CSI16: the unmount, the drop of every path,
// the drop of the discovery connections once nothing is staged, and a
// repeat that is done.
func TestNodeUnstage(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	req := stageReq(mountCap(fsExt4, rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")
	unstage := &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	}
	_, err = n.NodeUnstageVolume(ctx, unstage)
	wantCode(t, err, codes.OK, "unstage")
	if f.mounted(req.GetStagingTargetPath()) != nil {
		t.Error("the staging path is still mounted")
	}
	if len(f.paths(subsystemNqnOf(testVolume))) != 0 {
		t.Error("the volume's paths are still there")
	}
	if len(f.paths(common.NvmeDiscoveryNqn)) != 0 {
		t.Error("the discovery connection outlived the last volume")
	}
	_, err = n.NodeUnstageVolume(ctx, unstage)
	wantCode(t, err, codes.OK, "unstage again")
	_, err = n.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: "not-an-id", StagingTargetPath: "/x",
	})
	wantCode(t, err, codes.NotFound, "unstage of an id of another form")
}

// TestFollowerFollowsTheCdc pins csi.md CSI15: a cntlr that appears in the
// log gains a path, a cntlr the log withdraws loses its path, a log that
// cannot be read holds every drop off, and the union of two cdcs counts.
func TestFollowerFollowsTheCdc(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	_, err := n.NodeStageVolume(ctx, stageReq(blockCap(rwo)))
	wantCode(t, err, codes.OK, "stage")

	// A failover replaced cn0 by cn2.
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{
		{subNqn: nqn, ep: mustEndpoint(fakeCn1)},
		{subNqn: nqn, ep: mustEndpoint(fakeCn2)},
	}
	f.mu.Unlock()
	if err := n.follow.sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn1+" "+fakeCn2 {
		t.Errorf("paths %v after the cdc moved, want cn1 and cn2", got)
	}

	// A cdc that does not answer holds the drops off.
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn2)}}
	f.logFail[fakeCdcA] = true
	f.mu.Unlock()
	_ = n.follow.sync(ctx)
	if got := f.paths(nqn); len(got) != 2 {
		t.Errorf("paths %v after an unreadable log, want both kept", got)
	}
	f.mu.Lock()
	f.logFail[fakeCdcA] = false
	f.mu.Unlock()
	if err := n.follow.sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn2 {
		t.Errorf("paths %v after the log shrank, want cn2 alone", got)
	}

	// A second cdc lists cn1 as well: the union keeps both.
	f.mu.Lock()
	f.logs[fakeCdcB] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn1)}}
	f.mu.Unlock()
	req := stageReq(blockCap(rwo))
	req.PublishContext[ctxCdc] = fakeCdcA + "," + fakeCdcB
	_, err = n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage with two cdcs")
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn1+" "+fakeCn2 {
		t.Errorf("paths %v with two cdcs, want cn1 and cn2", got)
	}
	if got := f.paths(common.NvmeDiscoveryNqn); len(got) != 2 {
		t.Errorf("discovery connections %v, want both cdcs", got)
	}
}

// TestFollowerRecovers pins the restart (csi.md CSI15): the staged
// subsystems are read back from sysfs, and the follower's discovery
// connections are those under the node's host NQN to the configured cdcs,
// which it knows from the start; a configured cdc that cannot be read holds
// the drops off as before a restart. Another tool's connections are neither
// used nor dropped.
func TestFollowerRecovers(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	const (
		foreignCdc = "10.0.0.9:8009"   // another storage's discovery service
		retiredCdc = "10.0.0.10:18030" // a cdc an attach named, since dropped from the configuration
	)
	foreignNqn := "nqn.2024-01.io.dnv-it:e2e:ss0"
	n.follow = newFollower(n.host, time.Hour,
		[]endpoint{mustEndpoint(fakeCdcA), mustEndpoint(fakeCdcB)})
	f.mu.Lock()
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcA))
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcB))
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(foreignCdc))
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(retiredCdc))
	f.addCtrlAs(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcA), testNodeB)
	f.addCtrl(nqn, mustEndpoint(fakeCn0))
	f.addCtrl(nqn, mustEndpoint(fakeCn1))
	f.addCtrl(foreignNqn, mustEndpoint(fakeCn1))
	f.logs[fakeCdcA] = []logEntry{
		{subNqn: nqn, ep: mustEndpoint(fakeCn0)},
		{subNqn: nqn, ep: mustEndpoint(fakeCn2)},
	}
	f.logs[fakeCdcB] = f.logs[fakeCdcA]
	f.logs[retiredCdc] = f.logs[fakeCdcA]
	f.logs[foreignCdc] = []logEntry{{subNqn: foreignNqn, ep: mustEndpoint(fakeCn1)}}
	f.logFail[fakeCdcB] = true
	f.mu.Unlock()
	if err := n.follow.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !n.follow.staged[nqn] || len(n.follow.staged) != 1 {
		t.Errorf("staged %v, want the driver's subsystem alone", n.follow.staged)
	}
	// The retired cdc's log lists the staged subsystem: it is followed;
	// the foreign one's lists none: it is not.
	if _, ok := n.follow.cdcs[retiredCdc]; !ok || len(n.follow.cdcs) != 3 {
		t.Errorf("cdcs %v, want the two configured and the retired one", n.follow.cdcs)
	}
	// The second cdc cannot be read: the new cntlr is connected, and cn1,
	// which the readable cdc no longer lists, keeps its path.
	_ = n.follow.sync(ctx)
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn0+" "+fakeCn1+" "+fakeCn2 {
		t.Errorf("paths %v while a configured cdc is unread, want all three kept", got)
	}
	f.mu.Lock()
	f.logFail[fakeCdcB] = false
	f.mu.Unlock()
	if err := n.follow.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn0+" "+fakeCn2 {
		t.Errorf("paths %v once both cdcs answer, want cn0 and cn2", got)
	}
	if got := f.paths(foreignNqn); len(got) != 1 {
		t.Errorf("the foreign subsystem's path was touched: %v", got)
	}
	// The last unstage drops the follower's discovery connections, the
	// retired cdc's with them, and leaves the other tool's and the other
	// host NQN's; the configured cdcs stay known.
	if err := n.follow.unstage(ctx, nqn); err != nil {
		t.Fatal(err)
	}
	if got := f.paths(common.NvmeDiscoveryNqn); strings.Join(got, " ") !=
		fakeCdcA+" "+foreignCdc {
		t.Errorf("discovery connections %v after the unstage, want the "+
			"foreign one and the other host's", got)
	}
	if len(n.follow.cdcs) != 2 {
		t.Errorf("cdcs %v after the unstage, want the configured two", n.follow.cdcs)
	}
}

// TestFollowerSplitRangesAcrossARestart pins csi.md CSI15 for cdcs that
// split the shard codes: after a restart while the cdc of one range cannot
// be read, a volume only that cdc lists keeps its paths, however the other
// range's cdc answers.
func TestFollowerSplitRangesAcrossARestart(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	one, two := subsystemNqnOf("pvc-one"), subsystemNqnOf("pvc-two")
	n.follow = newFollower(n.host, time.Hour,
		[]endpoint{mustEndpoint(fakeCdcA), mustEndpoint(fakeCdcB)})
	f.mu.Lock()
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcA))
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcB))
	f.addCtrl(one, mustEndpoint(fakeCn0))
	f.addCtrl(two, mustEndpoint(fakeCn1))
	f.addCtrl(two, mustEndpoint(fakeCn2))
	f.logs[fakeCdcA] = []logEntry{{subNqn: one, ep: mustEndpoint(fakeCn0)}}
	f.logs[fakeCdcB] = []logEntry{
		{subNqn: two, ep: mustEndpoint(fakeCn1)},
		{subNqn: two, ep: mustEndpoint(fakeCn2)},
	}
	f.logFail[fakeCdcB] = true
	f.mu.Unlock()
	if err := n.follow.recover(ctx); err != nil {
		t.Fatal(err)
	}
	_ = n.follow.sync(ctx)
	if got := f.paths(two); len(got) != 2 {
		t.Errorf("paths %v of the volume the unread cdc lists, want both kept", got)
	}
	if got := f.paths(one); len(got) != 1 {
		t.Errorf("paths %v of the volume the readable cdc lists", got)
	}
}

// TestFollowerHoldsDropsWithoutAnswer pins the rule that a sync hearing from
// no cdc drops nothing (csi.md CSI15): a restart that finds a volume's paths
// and no discovery connection, and a known cdc that cannot be reached, both
// leave every path in place.
func TestFollowerHoldsDropsWithoutAnswer(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	f.mu.Lock()
	f.addCtrl(nqn, mustEndpoint(fakeCn0))
	f.addCtrl(nqn, mustEndpoint(fakeCn1))
	f.mu.Unlock()
	if err := n.follow.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.follow.cdcs) != 0 {
		t.Fatalf("cdcs %v, want none", n.follow.cdcs)
	}
	_ = n.follow.sync(ctx)
	if got := f.paths(nqn); len(got) != 2 {
		t.Errorf("paths %v after a sync with no cdc, want both kept", got)
	}
	// The only cdc cannot be reached: still nothing heard.
	f.mu.Lock()
	f.discFail[fakeCdcA] = true
	f.logs[fakeCdcA] = nil
	f.mu.Unlock()
	n.follow.mu.Lock()
	n.follow.cdcs[fakeCdcA] = mustEndpoint(fakeCdcA)
	n.follow.mu.Unlock()
	if err := n.follow.sync(ctx); err == nil {
		t.Error("a sync that reached no cdc answered no error")
	}
	if got := f.paths(nqn); len(got) != 2 {
		t.Errorf("paths %v after a sync that reached no cdc, want both kept", got)
	}
}

// TestFollowerUnstageRacesStage pins the one-at-a-time rule of csi.md
// CSI15: the last unstage of one volume, with a stage of another volume
// arriving while it drops the discovery connection, leaves the new volume
// followed, with its paths and its discovery connection.
func TestFollowerUnstageRacesStage(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	const other = "pvc-other"
	otherNqn := subsystemNqnOf(other)
	f.mu.Lock()
	f.uuidOf[otherNqn] = namespaceUuidOf(other)
	f.logs[fakeCdcA] = append(f.logs[fakeCdcA],
		logEntry{subNqn: otherNqn, ep: mustEndpoint(fakeCn0)},
		logEntry{subNqn: otherNqn, ep: mustEndpoint(fakeCn1)})
	f.mu.Unlock()
	req := stageReq(blockCap(rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")

	otherReq := stageReq(blockCap(rwo))
	otherReq.VolumeId = fakeClusterOne + "/" + other
	otherReq.PublishContext[ctxNqn] = otherNqn
	otherReq.PublishContext[ctxUuid] = namespaceUuidOf(other)
	done := make(chan error, 1)
	var once sync.Once
	f.onCommand = func(name string, args []string) {
		if name != nvmeBin || args[0] != "disconnect" || argument(args, "-d") == "" {
			return
		}
		once.Do(func() {
			go func() {
				_, err := n.NodeStageVolume(ctx, otherReq)
				done <- err
			}()
			// The window the stage would have run in.
			time.Sleep(200 * time.Millisecond)
		})
	}
	_, err = n.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: req.GetVolumeId(), StagingTargetPath: req.GetStagingTargetPath(),
	})
	wantCode(t, err, codes.OK, "unstage")
	wantCode(t, <-done, codes.OK, "the stage that raced the unstage")
	if err := n.follow.sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.paths(otherNqn); len(got) != 2 {
		t.Errorf("paths %v of the new volume, want both cntlrs", got)
	}
	if got := f.paths(common.NvmeDiscoveryNqn); strings.Join(got, " ") != fakeCdcA {
		t.Errorf("discovery connections %v, want the cdc", got)
	}
	if _, ok := n.follow.cdcs[fakeCdcA]; !ok {
		t.Errorf("cdcs %v: the new volume is not followed", n.follow.cdcs)
	}
}

// TestFollowerOneCdcDown pins csi.md CSI15 for cdc twins: a cdc that cannot
// be reached leaves the connects to the other and holds the drops off until
// it answers again.
func TestFollowerOneCdcDown(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	f.mu.Lock()
	f.logs[fakeCdcB] = f.logs[fakeCdcA]
	f.discFail[fakeCdcB] = true
	f.mu.Unlock()
	req := stageReq(blockCap(rwo))
	req.PublishContext[ctxCdc] = fakeCdcA + "," + fakeCdcB
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage with one cdc down")
	if got := f.paths(nqn); len(got) != 2 {
		t.Errorf("paths %v with one cdc down, want both cntlrs", got)
	}
	// A failover the reachable cdc shows: cn1 is withdrawn, but the other
	// cdc has not answered, so the path stays.
	f.mu.Lock()
	f.logs[fakeCdcA] = []logEntry{{subNqn: nqn, ep: mustEndpoint(fakeCn0)}}
	f.mu.Unlock()
	_ = n.follow.sync(ctx)
	if got := f.paths(nqn); len(got) != 2 {
		t.Errorf("paths %v while a cdc is down, want both kept", got)
	}
	f.mu.Lock()
	f.discFail[fakeCdcB] = false
	f.logs[fakeCdcB] = f.logs[fakeCdcA]
	f.mu.Unlock()
	if err := n.follow.sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.paths(nqn); strings.Join(got, " ") != fakeCn0 {
		t.Errorf("paths %v once both cdcs answer, want cn0 alone", got)
	}
}

// TestNodeStageForgetsAFailedStage pins csi.md CSI16: a stage that fails
// for good with nothing mounted leaves no path and no discovery connection,
// since the kubelet never unstages it; a failure over a volume an earlier
// stage mounted keeps everything.
func TestNodeStageForgetsAFailedStage(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	nqn := subsystemNqnOf(testVolume)
	f.mountErr = true
	_, err := n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.Internal, "a stage whose mount fails")
	if len(n.follow.staged) != 0 || len(f.paths(nqn)) != 0 ||
		len(f.paths(common.NvmeDiscoveryNqn)) != 0 {
		t.Errorf("staged %v, paths %v, discovery %v after a failed stage",
			n.follow.staged, f.paths(nqn), f.paths(common.NvmeDiscoveryNqn))
	}
	f.mountErr = false
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsExt4, rwo)))
	wantCode(t, err, codes.OK, "the stage once the mount works")
	_, err = n.NodeStageVolume(ctx, stageReq(mountCap(fsXfs, rwo)))
	wantCode(t, err, codes.AlreadyExists, "a stage over another filesystem's mount")
	if !n.follow.staged[nqn] || len(f.paths(nqn)) != 2 {
		t.Errorf("staged %v, paths %v: the mounted volume lost its paths",
			n.follow.staged, f.paths(nqn))
	}
}

// TestNodeGetVolumeStats pins csi.md CSI17's report: a mounted filesystem's
// totals, a block volume's size alone, NOT_FOUND for a path with nothing
// mounted.
func TestNodeGetVolumeStats(t *testing.T) {
	n, f := newTestNode(t)
	ctx := t.Context()
	req := stageReq(mountCap(fsExt4, rwo))
	_, err := n.NodeStageVolume(ctx, req)
	wantCode(t, err, codes.OK, "stage")
	stats, err := n.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId: req.GetVolumeId(), VolumePath: req.GetStagingTargetPath(),
	})
	wantCode(t, err, codes.OK, "stats of a mounted volume")
	if len(stats.GetUsage()) != 2 || stats.GetUsage()[0].GetTotal() != 4096*1000 ||
		stats.GetUsage()[0].GetUsed() != 4096*500 ||
		stats.GetUsage()[0].GetAvailable() != 4096*400 ||
		stats.GetUsage()[1].GetTotal() != 100 {
		t.Errorf("usage %v", stats.GetUsage())
	}
	_, err = n.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId: req.GetVolumeId(), VolumePath: "/nowhere",
	})
	wantCode(t, err, codes.NotFound, "stats of a path with nothing mounted")

	// A block volume's target is a device node under a bind mount.
	dev := devicePath(namespaceUuidOf(testVolume))
	target := "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/" + testVolume + "/p1"
	f.mu.Lock()
	f.mounts = append(f.mounts, mountEntry{target: target, source: dev, fsType: "bind"})
	f.links[target] = true
	f.sizes[target] = 10 << 30
	f.mu.Unlock()
	stats, err = n.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId: req.GetVolumeId(), VolumePath: target,
	})
	wantCode(t, err, codes.OK, "stats of a block volume")
	if len(stats.GetUsage()) != 1 || stats.GetUsage()[0].GetTotal() != 10<<30 {
		t.Errorf("block usage %v", stats.GetUsage())
	}
}

// TestParsers pins the three readers of what the node answers: the sysfs
// address, nvme-cli's discovery log and the kernel's mount table.
func TestParsers(t *testing.T) {
	addr, svc := parseAddress("traddr=10.0.0.125,trsvcid=4300,src_addr=10.0.0.193")
	if addr != "10.0.0.125" || svc != "4300" {
		t.Errorf("parseAddress = %q, %q", addr, svc)
	}
	// An IPv6 cdc address comes in brackets and goes to nvme-cli bare; the
	// key of the controller sysfs shows for it is the same address.
	ep, err := parseEndpoint("[fd00::10]:8009")
	if err != nil || ep.trAddr != "fd00::10" || ep.trSvcId != "8009" {
		t.Errorf("parseEndpoint of an IPv6 address = %+v, %v", ep, err)
	}
	addr, svc = parseAddress("traddr=fd00::10,trsvcid=8009,src_addr=fd00::193")
	if c := (ctrl{trAddr: addr, trSvcId: svc}); c.key() != ep.String() ||
		ep.String() != "[fd00::10]:8009" {
		t.Errorf("the controller's key %q, the endpoint %q", c.key(), ep.String())
	}
	if _, err := parseEndpoint("fd00::10:8009"); err == nil {
		t.Error("an IPv6 address without brackets was read as ip:port")
	}
	entries, err := parseDiscoveryLog(`{"device":"nvme0","genctr":3,"records":[
		{"trtype":"tcp","adrfam":"ipv4","subtype":"nvme subsystem","treq":"not specified",
		 "portid":1,"trsvcid":"4300","subnqn":"nqn.2024-01.io.dnv.csi:pvc-1","traddr":"10.0.0.125"},
		{"trtype":"tcp","adrfam":"ipv4","subtype":"discovery subsystem referral",
		 "portid":2,"trsvcid":"8009","subnqn":"nqn.2014-08.org.nvmexpress.discovery","traddr":"10.0.0.79"},
		{"trtype":"rdma","adrfam":"ipv4","subtype":"nvme subsystem",
		 "portid":3,"trsvcid":"4420","subnqn":"nqn.2024-01.io.dnv.csi:pvc-2","traddr":"10.0.0.1"}]}`)
	if err != nil || len(entries) != 1 || entries[0].subNqn != "nqn.2024-01.io.dnv.csi:pvc-1" ||
		entries[0].ep.String() != "10.0.0.125:4300" {
		t.Errorf("parseDiscoveryLog = %v, %v", entries, err)
	}
	if entries, err := parseDiscoveryLog(""); err != nil || len(entries) != 0 {
		t.Errorf("an empty log = %v, %v", entries, err)
	}
	if _, err := parseDiscoveryLog("not json"); err == nil {
		t.Error("garbage was taken as a log")
	}
	mounts := parseMountInfo("36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue\n" +
		"40 35 0:36 / /var/lib/kubelet/pods/a\\040b/m rw - ext4 /dev/nvme0n1 rw\n" +
		"garbage line\n")
	if len(mounts) != 2 || mounts[0].target != "/mnt2" || mounts[0].fsType != "ext3" ||
		mounts[0].source != "/dev/root" || mounts[1].target != "/var/lib/kubelet/pods/a b/m" {
		t.Errorf("parseMountInfo = %+v", mounts)
	}
}

// TestRunNodeServesOverTheSocket runs the whole node plugin over a unix
// socket on the fake node: the identity answers without the controller
// capability, the node id is the host NQN, and the stop leaves a node with
// nothing staged without discovery connections.
func TestRunNodeServesOverTheSocket(t *testing.T) {
	f := newFakeNode()
	// A discovery connection of the plugin's, left by an earlier run, and
	// another tool's: the stop with nothing staged drops the first alone.
	const foreignCdc = "10.0.0.9:8009"
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(fakeCdcA))
	f.addCtrl(common.NvmeDiscoveryNqn, mustEndpoint(foreignCdc))
	dir, err := os.MkdirTemp("", "dnv-csi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	endpoint := filepath.Join(dir, "csi.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunNode(ctx, Config{Endpoint: endpoint, OsClient: f.client(),
			CdcAddresses: []string{fakeCdcA}})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(endpoint); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node plugin did not listen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	defer callCancel()
	info, err := csi.NewNodeClient(conn).NodeGetInfo(callCtx, &csi.NodeGetInfoRequest{})
	wantCode(t, err, codes.OK, "NodeGetInfo")
	if info.GetNodeId() != testNodeA {
		t.Errorf("node id %q, want the host NQN", info.GetNodeId())
	}
	// The one segment every node reports, the same everywhere.
	if seg := info.GetAccessibleTopology().GetSegments(); len(seg) != 1 ||
		seg[topologyKey] != topologyValue {
		t.Errorf("accessible topology %v, want the one segment", seg)
	}
	caps, err := csi.NewIdentityClient(conn).GetPluginCapabilities(callCtx,
		&csi.GetPluginCapabilitiesRequest{})
	wantCode(t, err, codes.OK, "GetPluginCapabilities")
	// A capability describes the driver, so the node plugin advertises
	// what the controller plugin does.
	wantPluginCapabilities(t, caps)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunNode returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunNode did not stop")
	}
	if got := f.paths(common.NvmeDiscoveryNqn); strings.Join(got, " ") != foreignCdc {
		t.Errorf("discovery connections %v after the stop, want the other tool's alone", got)
	}

	// Without the identity files the plugin refuses to start.
	delete(f.files, DefaultHostNqnPath)
	if err := RunNode(t.Context(), Config{Endpoint: endpoint, OsClient: f.client()}); err == nil {
		t.Error("RunNode started without the node's identity")
	}
}
