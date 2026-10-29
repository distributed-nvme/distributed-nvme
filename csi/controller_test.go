package csi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
)

const (
	testVolume = "pvc-0b7a4d2e-3c1f-4e5a-9b6d-7c8e9f0a1b2c"
	testNodeA  = "nqn.2014-08.org.nvmexpress:uuid:1a788e0a-02bd-4ae9-9a4b-e4aad8b04c26"
	testNodeB  = "nqn.2014-08.org.nvmexpress:uuid:0e3429e4-bcba-474d-ac18-16bd7de12c6e"
	gib        = int64(1) << 30
)

var testCdc = []string{"192.168.122.78:18030", "192.168.122.79:18030"}

// newTestController wires a controller to a fresh fake gateway.
func newTestController(t *testing.T) (*controller, *fakeGateway) {
	t.Helper()
	g := newFakeGateway()
	c := newController(g.serve(t), Config{
		CdcAddresses:   testCdc,
		GatewayTimeout: 2 * time.Second,
	})
	return c, g
}

func mountCap(fs string, mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fs},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func blockCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

var rwo = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER

func createReq(name string, size int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: size},
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(fsExt4, rwo)},
		Parameters: map[string]string{
			paramCluster: fakeClusterOne, paramRedundancy: redundancyRaid1,
			paramCntlrs: "2",
		},
	}
}

func wantCode(t *testing.T, err error, want codes.Code, label string) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("%s: code %s, want %s (err %v)", label, got, want, err)
	}
}

// TestCreateVolumeMakesTheFourObjects pins csi.md, The volume model: the
// pool named as the volume with one slice, the class's shape and a first
// data group of one eighth of the volume; the thin device named as the
// volume and sized to the request; the subsystem with no host; the one
// namespace with the derived uuid; and the reply's id and context.
func TestCreateVolumeMakesTheFourObjects(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, 10*gib))
	wantCode(t, err, codes.OK, "CreateVolume")
	vol := reply.GetVolume()
	if vol.GetVolumeId() != fakeClusterOne+"/"+testVolume {
		t.Errorf("volume id %q", vol.GetVolumeId())
	}
	if vol.GetCapacityBytes() != 10*gib {
		t.Errorf("capacity %d, want %d", vol.GetCapacityBytes(), 10*gib)
	}
	nqn := subsystemNqnOf(testVolume)
	uuid := namespaceUuidOf(testVolume)
	if vol.GetVolumeContext()[ctxNqn] != nqn ||
		vol.GetVolumeContext()[ctxUuid] != uuid {
		t.Errorf("volume context %v", vol.GetVolumeContext())
	}
	p := g.pool(fakeClusterOne, testVolume)
	if p == nil {
		t.Fatal("no pool named as the volume")
	}
	if p.req.GetSliceCnt() != 1 || p.req.GetCntlrCnt() != 2 ||
		p.req.GetBdevConf().GetRedundConf().GetRedundMdRaid1() == nil {
		t.Errorf("pool shape %v", p.req)
	}
	// dnv's default auto-grow mark, whatever the cluster stores.
	if got := p.req.GetBdevConf().GetDmPoolConf().GetLowWaterMarkPct(); got !=
		common.DefaultPoolLowWatermarkPct {
		t.Errorf("low-water mark %d, want dnv's default %d", got,
			common.DefaultPoolLowWatermarkPct)
	}
	// One eighth of 10 GiB is 1.25 GiB, which is 20 extents of 64 MiB.
	if p.req.GetInitExtCnt() != 20 {
		t.Errorf("init_ext_cnt %d, want 20", p.req.GetInitExtCnt())
	}
	td := p.tds[testVolume]
	if td == nil || td.GetSize() != uint64(10*gib) {
		t.Errorf("thin device %v", td)
	}
	ss := p.ss[nqn]
	if ss == nil || len(ss.GetAllowedHosts()) != 0 {
		t.Fatalf("subsystem %v", ss)
	}
	ns := namespaceOf(ss)
	if ns == nil || ns.GetTdId() != td.GetTdId() || ns.GetDevUuid() != uuid {
		t.Errorf("namespace %v", ns)
	}
	want := []string{"GetCluster", "CreateStoragePool", "CreateThinDevice",
		"CreateSubsystem", "CreateNamespace"}
	if strings.Join(g.calls, " ") != strings.Join(want, " ") {
		t.Errorf("calls %v, want %v", g.calls, want)
	}
}

// TestCreateVolumeIsRepeatable pins the idempotency the spec asks for: the
// same request answers the same reply and creates nothing twice; another
// size for the same name is ALREADY_EXISTS; a volume whose pool is still
// being deleted is ABORTED, and comes back once the drain removed it.
func TestCreateVolumeIsRepeatable(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	first, err := c.CreateVolume(ctx, createReq(testVolume, 10*gib))
	wantCode(t, err, codes.OK, "first create")
	second, err := c.CreateVolume(ctx, createReq(testVolume, 10*gib))
	wantCode(t, err, codes.OK, "second create")
	if first.GetVolume().GetVolumeId() != second.GetVolume().GetVolumeId() ||
		first.GetVolume().GetCapacityBytes() != second.GetVolume().GetCapacityBytes() {
		t.Errorf("replies differ: %v vs %v", first, second)
	}
	if n := g.methodCalls("CreateThinDevice"); n != 2 {
		t.Errorf("CreateThinDevice called %d times, want 2 (one refused)", n)
	}
	if len(g.pool(fakeClusterOne, testVolume).tds) != 1 {
		t.Error("a second thin device was created")
	}
	_, err = c.CreateVolume(ctx, createReq(testVolume, 20*gib))
	wantCode(t, err, codes.AlreadyExists, "another size")
	other := createReq(testVolume, 10*gib)
	other.Parameters[paramRedundancy] = redundancyNone
	_, err = c.CreateVolume(ctx, other)
	wantCode(t, err, codes.AlreadyExists, "another redundancy")
	// A namespace at index one that is not the volume's.
	ss := g.pool(fakeClusterOne, testVolume).ss[subsystemNqnOf(testVolume)]
	g.mu.Lock()
	namespaceOf(ss).DevUuid = "00000000-0000-0000-0000-000000000001"
	g.mu.Unlock()
	_, err = c.CreateVolume(ctx, createReq(testVolume, 10*gib))
	wantCode(t, err, codes.AlreadyExists, "a namespace that is not the volume's")
	g.mu.Lock()
	namespaceOf(ss).DevUuid = namespaceUuidOf(testVolume)
	g.mu.Unlock()

	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: first.GetVolume().GetVolumeId(),
	})
	wantCode(t, err, codes.OK, "delete")
	_, err = c.CreateVolume(ctx, createReq(testVolume, 10*gib))
	wantCode(t, err, codes.Aborted, "create while the pool is deleting")
	g.drain(fakeClusterOne, testVolume)
	_, err = c.CreateVolume(ctx, createReq(testVolume, 10*gib))
	wantCode(t, err, codes.OK, "create after the drain")
}

// TestCreateVolumeCompletesAHalfMadeVolume pins the repeat of csi.md CSI9:
// a create that stopped after any of its first three objects is repeated
// and makes only what is missing, with the reply a clean create gives.
func TestCreateVolumeCompletesAHalfMadeVolume(t *testing.T) {
	for _, step := range []string{"CreateThinDevice", "CreateSubsystem", "CreateNamespace"} {
		t.Run(step, func(t *testing.T) {
			c, g := newTestController(t)
			ctx := t.Context()
			g.mu.Lock()
			g.fail[step] = codes.Unavailable
			g.mu.Unlock()
			_, err := c.CreateVolume(ctx, createReq(testVolume, 10*gib))
			wantCode(t, err, codes.Unavailable, "a create that stops at "+step)
			g.mu.Lock()
			delete(g.fail, step)
			g.mu.Unlock()
			reply, err := c.CreateVolume(ctx, createReq(testVolume, 10*gib))
			wantCode(t, err, codes.OK, "the repeated create")
			if reply.GetVolume().GetVolumeId() != fakeClusterOne+"/"+testVolume ||
				reply.GetVolume().GetCapacityBytes() != 10*gib {
				t.Errorf("reply %v", reply)
			}
			p := g.pool(fakeClusterOne, testVolume)
			ss := p.ss[subsystemNqnOf(testVolume)]
			if len(p.tds) != 1 || len(p.ss) != 1 || ss == nil || len(ss.GetNsList()) != 1 {
				t.Fatalf("thin devices %d, subsystems %d, subsystem %v",
					len(p.tds), len(p.ss), ss)
			}
			if ns := namespaceOf(ss); ns.GetTdId() != p.tds[testVolume].GetTdId() ||
				ns.GetDevUuid() != namespaceUuidOf(testVolume) {
				t.Errorf("namespace %v", ns)
			}
		})
	}
}

// TestCreateVolumeRefuses pins every refusal of a create.
func TestCreateVolumeRefuses(t *testing.T) {
	c, _ := newTestController(t)
	ctx := t.Context()
	cases := []struct {
		name string
		mut  func(*csi.CreateVolumeRequest)
		code codes.Code
	}{
		{"no name", func(r *csi.CreateVolumeRequest) { r.Name = "" },
			codes.InvalidArgument},
		{"no capability", func(r *csi.CreateVolumeRequest) {
			r.VolumeCapabilities = nil
		}, codes.InvalidArgument},
		{"a multi-node mode", func(r *csi.CreateVolumeRequest) {
			r.VolumeCapabilities = []*csi.VolumeCapability{mountCap(fsExt4,
				csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}
		}, codes.InvalidArgument},
		{"a filesystem the driver does not format",
			func(r *csi.CreateVolumeRequest) {
				r.VolumeCapabilities = []*csi.VolumeCapability{mountCap("btrfs", rwo)}
			}, codes.InvalidArgument},
		{"an unknown parameter", func(r *csi.CreateVolumeRequest) {
			r.Parameters["pool"] = "sp0"
		}, codes.InvalidArgument},
		{"a bad redundancy", func(r *csi.CreateVolumeRequest) {
			r.Parameters[paramRedundancy] = "raid5"
		}, codes.InvalidArgument},
		{"a bad cntlr count", func(r *csi.CreateVolumeRequest) {
			r.Parameters[paramCntlrs] = "9"
		}, codes.InvalidArgument},
		{"a cluster that does not exist", func(r *csi.CreateVolumeRequest) {
			r.Parameters[paramCluster] = "nowhere"
		}, codes.InvalidArgument},
		{"a content source", func(r *csi.CreateVolumeRequest) {
			r.VolumeContentSource = &csi.VolumeContentSource{}
		}, codes.InvalidArgument},
		{"a range no unit fits", func(r *csi.CreateVolumeRequest) {
			r.CapacityRange = &csi.CapacityRange{RequiredBytes: 100, LimitBytes: 100}
		}, codes.OutOfRange},
		{"a negative range", func(r *csi.CreateVolumeRequest) {
			r.CapacityRange = &csi.CapacityRange{RequiredBytes: -1}
		}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := createReq(testVolume, gib)
			tc.mut(req)
			_, err := c.CreateVolume(ctx, req)
			wantCode(t, err, tc.code, tc.name)
		})
	}
}

// TestCreateVolumeSizes pins the rounding of csi.md, The volume model: up
// to the pool's unit from the required bytes, down to it from a limit
// alone, the driver's default when neither is given, and a block volume
// with the same rules.
func TestCreateVolumeSizes(t *testing.T) {
	c, _ := newTestController(t)
	ctx := t.Context()
	cases := []struct {
		name string
		rng  *csi.CapacityRange
		want int64
	}{
		{"one byte rounds up to the unit", &csi.CapacityRange{RequiredBytes: 1},
			int64(fakeStripe)},
		{"a limit alone rounds down", &csi.CapacityRange{LimitBytes: 200 << 10},
			192 << 10},
		{"no range is the default", nil, defaultVolumeSize},
		{"both equal and aligned",
			&csi.CapacityRange{RequiredBytes: gib, LimitBytes: gib}, gib},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := createReq(testVolume+string(rune('a'+i)), 0)
			req.CapacityRange = tc.rng
			req.VolumeCapabilities = []*csi.VolumeCapability{blockCap(rwo)}
			reply, err := c.CreateVolume(ctx, req)
			wantCode(t, err, codes.OK, tc.name)
			if got := reply.GetVolume().GetCapacityBytes(); got != tc.want {
				t.Errorf("capacity %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDeleteVolume pins csi.md, The volume model: a delete removes the
// namespace, the subsystem and the thin device, then marks the pool
// deleting; a pool that is absent, already deleting or named by an id of
// another form is done; only an empty id is refused.
func TestDeleteVolume(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id})
	wantCode(t, err, codes.OK, "delete")
	p := g.pool(fakeClusterOne, testVolume)
	if !p.deleting || len(p.tds) != 0 || len(p.ss) != 0 {
		t.Errorf("after the delete: deleting %v, %d thin devices, %d subsystems",
			p.deleting, len(p.tds), len(p.ss))
	}
	want := []string{"DeleteNamespace", "DeleteSubsystem", "DeleteThinDevice",
		"DeleteStoragePool"}
	got := g.calls[len(g.calls)-4:]
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("delete calls %v, want %v", got, want)
	}
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id})
	wantCode(t, err, codes.OK, "delete while deleting")
	g.drain(fakeClusterOne, testVolume)
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id})
	wantCode(t, err, codes.OK, "delete of an absent volume")
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "not-an-id"})
	wantCode(t, err, codes.OK, "delete of an id of another form")
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{})
	wantCode(t, err, codes.InvalidArgument, "delete with no id")
}

// TestPublishAndUnpublish pins csi.md, Attach and detach.
func TestPublishAndUnpublish(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	nqn := subsystemNqnOf(testVolume)
	publish := func(node string) (*csi.ControllerPublishVolumeResponse, error) {
		return c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId:         id,
			NodeId:           node,
			VolumeCapability: mountCap(fsExt4, rwo),
		})
	}
	hosts := func() []string {
		return g.pool(fakeClusterOne, testVolume).ss[nqn].GetAllowedHosts()
	}

	pub, err := publish(testNodeA)
	wantCode(t, err, codes.OK, "publish to A")
	if got := hosts(); len(got) != 1 || got[0] != testNodeA {
		t.Errorf("allowed hosts %v, want [%s]", got, testNodeA)
	}
	pc := pub.GetPublishContext()
	if pc[ctxNqn] != nqn || pc[ctxUuid] != namespaceUuidOf(testVolume) ||
		pc[ctxCdc] != strings.Join(testCdc, cdcSep) {
		t.Errorf("publish context %v", pc)
	}
	_, err = publish(testNodeA)
	wantCode(t, err, codes.OK, "publish to A again")
	_, err = publish(testNodeB)
	wantCode(t, err, codes.FailedPrecondition, "publish to B while A holds it")
	if !strings.Contains(err.Error(), testNodeA) {
		t.Errorf("the refusal does not name the holder: %v", err)
	}

	_, err = c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: id, NodeId: testNodeA,
	})
	wantCode(t, err, codes.OK, "unpublish A")
	if got := hosts(); len(got) != 0 {
		t.Errorf("allowed hosts %v, want none", got)
	}
	_, err = c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: id, NodeId: testNodeA,
	})
	wantCode(t, err, codes.OK, "unpublish A again")
	_, err = publish(testNodeB)
	wantCode(t, err, codes.OK, "publish to B after A let go")
	_, err = c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: id,
	})
	wantCode(t, err, codes.OK, "unpublish from every node")
	if got := hosts(); len(got) != 0 {
		t.Errorf("allowed hosts %v after an unpublish from every node", got)
	}

	t.Run("refusals", func(t *testing.T) {
		_, err := c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId: id, NodeId: "", VolumeCapability: mountCap(fsExt4, rwo),
		})
		wantCode(t, err, codes.InvalidArgument, "no node id")
		_, err = c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId: id, NodeId: testNodeA,
		})
		wantCode(t, err, codes.InvalidArgument, "no capability")
		_, err = c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId: id, NodeId: "some-fake-node", VolumeCapability: mountCap(fsExt4, rwo),
		})
		wantCode(t, err, codes.NotFound, "a node id that is no host NQN")
		_, err = c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId: fakeClusterOne + "/pvc-none", NodeId: testNodeA,
			VolumeCapability: mountCap(fsExt4, rwo),
		})
		wantCode(t, err, codes.NotFound, "a volume that does not exist")
		_, err = c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId: "some-vol-id", NodeId: testNodeA,
			VolumeCapability: mountCap(fsExt4, rwo),
		})
		wantCode(t, err, codes.NotFound, "an id of another form")
		_, err = c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
			VolumeId: fakeClusterOne + "/pvc-none", NodeId: testNodeA,
		})
		wantCode(t, err, codes.OK, "unpublish of a volume that does not exist")
		_, err = c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{})
		wantCode(t, err, codes.InvalidArgument, "unpublish with no id")
	})
}

// TestValidateVolumeCapabilities pins the confirmation and its refusals.
func TestValidateVolumeCapabilities(t *testing.T) {
	c, _ := newTestController(t)
	ctx := t.Context()
	reply, err := c.CreateVolume(ctx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "create")
	id := reply.GetVolume().GetVolumeId()
	ok, err := c.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           id,
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(fsXfs, rwo), blockCap(rwo)},
		VolumeContext:      reply.GetVolume().GetVolumeContext(),
	})
	wantCode(t, err, codes.OK, "validate")
	if ok.GetConfirmed() == nil || len(ok.GetConfirmed().GetVolumeCapabilities()) != 2 {
		t.Errorf("not confirmed: %v", ok)
	}
	no, err := c.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: id,
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(fsExt4,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY)},
	})
	wantCode(t, err, codes.OK, "validate a multi-node mode")
	if no.GetConfirmed() != nil || no.GetMessage() == "" {
		t.Errorf("a multi-node mode was confirmed: %v", no)
	}
	_, err = c.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           fakeClusterOne + "/pvc-none",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(fsExt4, rwo)},
	})
	wantCode(t, err, codes.NotFound, "a volume that does not exist")
	_, err = c.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: id,
	})
	wantCode(t, err, codes.InvalidArgument, "no capabilities")
}

// TestGetCapacity pins csi.md, Capacity, on the fixture: the controller
// side is the smaller, a disabled, an unhealthy or a full disk node drops
// out, the Kubernetes-reserved parameters are ignored, and the largest
// volume follows the legs-th largest disk node.
func TestGetCapacity(t *testing.T) {
	c, g := newTestController(t)
	ctx := t.Context()
	req := &csi.GetCapacityRequest{Parameters: map[string]string{
		paramCluster: fakeClusterOne, paramCntlrs: "2",
		"csi.storage.k8s.io/fstype": "ext4",
	}}
	reply, err := c.GetCapacity(ctx, req)
	wantCode(t, err, codes.OK, "capacity")
	// Disk side: 4 x 1000 x 64 MiB / 2 legs; controller side: 3 x 900 x
	// 64 MiB / 2 cntlrs, the smaller.
	wantAvail := int64(3 * fakeCnFree * fakeExtent / 2)
	if reply.GetAvailableCapacity() != wantAvail {
		t.Errorf("available %d, want %d", reply.GetAvailableCapacity(), wantAvail)
	}
	// The fourth-largest disk node holds 1000 extents and the
	// second-largest controller node 900, less the meta group's one; times
	// 8 either is above the available bytes, so the largest volume is the
	// available bytes.
	if reply.GetMaximumVolumeSize().GetValue() != wantAvail {
		t.Errorf("maximum volume %d, want %d",
			reply.GetMaximumVolumeSize().GetValue(), wantAvail)
	}

	g.mu.Lock()
	cl := g.clusters[fakeClusterOne]
	cl.dns["dn0:9000"].Disabled = true
	cl.dns["dn1:9000"].ErrEpoch = 7
	cl.dns["dn2:9000"].FreeExtCnt = 10
	for _, cn := range cl.cns {
		cn.FreeExtCnt = 10000
	}
	g.mu.Unlock()
	reply, err = c.GetCapacity(ctx, req)
	wantCode(t, err, codes.OK, "capacity with nodes dropped")
	// Two disk nodes are left, and a RAID1 pool takes four: nothing, with a
	// largest volume of zero that is set, not left out.
	if reply.GetAvailableCapacity() != 0 || reply.GetMaximumVolumeSize() == nil ||
		reply.GetMaximumVolumeSize().GetValue() != 0 {
		t.Errorf("RAID1 over two disk nodes answers %v, want zero and zero", reply)
	}

	none := &csi.GetCapacityRequest{Parameters: map[string]string{
		paramCluster: fakeClusterOne, paramRedundancy: redundancyNone, paramCntlrs: "1",
	}}
	reply, err = c.GetCapacity(ctx, none)
	wantCode(t, err, codes.OK, "capacity without redundancy")
	// A pool without redundancy takes two disk nodes, with 10 and 1000
	// free; its meta group may take the fuller, so the data group is sure
	// of 10 extents, times 8.
	if want := int64(1010 * fakeExtent); reply.GetAvailableCapacity() != want ||
		reply.GetMaximumVolumeSize().GetValue() != int64(10*fakeExtent*volumeSteps) {
		t.Errorf("without redundancy: available %d, maximum %d, want %d and %d",
			reply.GetAvailableCapacity(), reply.GetMaximumVolumeSize().GetValue(), want,
			10*fakeExtent*volumeSteps)
	}
	// The cluster's smallest bin is 16 extents: the disk node with 10 is
	// offered to no scan, and one is left.
	g.mu.Lock()
	cl.conf.DnBinConf.Bin0Shift = 4
	g.mu.Unlock()
	reply, err = c.GetCapacity(ctx, none)
	wantCode(t, err, codes.OK, "capacity under a bin floor")
	if reply.GetAvailableCapacity() != 0 || reply.GetMaximumVolumeSize().GetValue() != 0 {
		t.Errorf("one disk node above the floor answers %v, want nothing", reply)
	}
	_, err = c.GetCapacity(ctx, &csi.GetCapacityRequest{Parameters: map[string]string{
		"pool": "sp0",
	}})
	wantCode(t, err, codes.InvalidArgument, "an unknown parameter")
	// A cluster that does not exist has nothing free, and the answer is
	// not an error, as Kubernetes asks on a timer; nor is a query with no
	// parameters at all, which the spec allows.
	reply, err = c.GetCapacity(ctx, &csi.GetCapacityRequest{Parameters: map[string]string{
		paramCluster: "nowhere",
	}})
	wantCode(t, err, codes.OK, "a cluster that does not exist")
	if reply.GetAvailableCapacity() != 0 || reply.GetMaximumVolumeSize() == nil ||
		reply.GetMaximumVolumeSize().GetValue() != 0 {
		t.Errorf("a cluster that does not exist answers %v, want zero and zero", reply)
	}
	_, err = c.GetCapacity(ctx, &csi.GetCapacityRequest{})
	wantCode(t, err, codes.OK, "no parameters at all")
}

// TestCreateVolumeWithANameDnvDoesNotTake pins csi.md, The volume model: a
// name longer than dnv's names, which the spec lets a caller send, becomes
// a dnv name, the same on a repeat, and the volume id carries that name, so
// a delete finds the objects.
func TestCreateVolumeWithANameDnvDoesNotTake(t *testing.T) {
	c, _ := newTestController(t)
	ctx := t.Context()
	long := strings.Repeat("v", 128)
	first, err := c.CreateVolume(ctx, createReq(long, gib))
	wantCode(t, err, codes.OK, "a 128-byte name")
	cluster, name, err := parseVolumeId(first.GetVolume().GetVolumeId())
	if err != nil || cluster != fakeClusterOne || name != dnvNameOf(long) || name == long {
		t.Fatalf("volume id %q splits into %q, %q, %v",
			first.GetVolume().GetVolumeId(), cluster, name, err)
	}
	second, err := c.CreateVolume(ctx, createReq(long, gib))
	wantCode(t, err, codes.OK, "the same name again")
	if second.GetVolume().GetVolumeId() != first.GetVolume().GetVolumeId() {
		t.Errorf("a repeated create gave id %q, want %q",
			second.GetVolume().GetVolumeId(), first.GetVolume().GetVolumeId())
	}
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: first.GetVolume().GetVolumeId(),
	})
	wantCode(t, err, codes.OK, "a delete by the id")
}

// TestControllerGetCapabilities pins the three capabilities advertised.
func TestControllerGetCapabilities(t *testing.T) {
	c, _ := newTestController(t)
	reply, err := c.ControllerGetCapabilities(t.Context(),
		&csi.ControllerGetCapabilitiesRequest{})
	wantCode(t, err, codes.OK, "capabilities")
	got := map[csi.ControllerServiceCapability_RPC_Type]bool{}
	for _, cap := range reply.GetCapabilities() {
		got[cap.GetRpc().GetType()] = true
	}
	for _, want := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	} {
		if !got[want] {
			t.Errorf("capability %s is not advertised", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("%d capabilities advertised, want 3", len(got))
	}
}

// TestGatewayFailuresKeepTheirCode pins fromGateway: a gateway code passes
// through behind a message naming the call.
func TestGatewayFailuresKeepTheirCode(t *testing.T) {
	c, g := newTestController(t)
	g.fail["CreateStoragePool"] = codes.ResourceExhausted
	_, err := c.CreateVolume(t.Context(), createReq(testVolume, gib))
	wantCode(t, err, codes.ResourceExhausted, "an exhausted cluster")
	if !strings.Contains(err.Error(), "CreateStoragePool") {
		t.Errorf("the error does not name the call: %v", err)
	}
	if err := fromGateway("X", context.DeadlineExceeded); status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("a deadline is %v", err)
	}
	if err := fromGateway("X", context.Canceled); status.Code(err) != codes.Unavailable {
		t.Errorf("a plain error is %v", err)
	}
}
