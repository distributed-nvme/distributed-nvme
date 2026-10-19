package dnagent

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The fake worker
// ---------------------------------------------------------------------------

// fakeWorker is the worker's half of the DiskNodeAgent surface, played by the
// test: it sends the syncups, the bitmap pushes and the check rounds a worker
// sends, as real gRPC calls over the in-memory connection dialAgent serves,
// through the server interceptors cmd/dnv-agent wires. It opens one check
// stream per object and keeps it open across rounds, as the worker does, so a
// case plays many syncups and rounds against one real agent over the fake OS
// client below it.
type fakeWorker struct {
	t      *testing.T
	client pb.DiskNodeAgentClient
	// sides holds the open CheckSide streams by side id; dn is the CheckDn
	// stream, nil until the first DN round.
	sides map[uint64]pb.DiskNodeAgent_CheckSideClient
	dn    pb.DiskNodeAgent_CheckDnClient
	// ctx bounds every call and stream of the worker; it ends with the test
	// or when the worker is closed for a restart.
	ctx    context.Context
	cancel context.CancelFunc
}

// newFakeWorker dials srv and returns a worker whose streams end with the
// test.
func newFakeWorker(t *testing.T, srv *DnAgentServer) *fakeWorker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &fakeWorker{
		t:      t,
		client: dialAgent(t, srv),
		sides:  make(map[uint64]pb.DiskNodeAgent_CheckSideClient),
		ctx:    ctx,
		cancel: cancel,
	}
}

// close ends the worker's streams, as a worker's connection to an agent that
// exits ends.
func (w *fakeWorker) close() {
	w.cancel()
}

// callCtx is one unary call's deadline, generous for a loaded machine.
func (w *fakeWorker) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(w.ctx, 20*time.Second)
}

func (w *fakeWorker) syncupDn(req *pb.SyncupDnRequest) *pb.SyncupDnReply {
	w.t.Helper()
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.SyncupDn(ctx, req)
	if err != nil {
		w.t.Fatalf("SyncupDn rev %d: %v", req.GetRevision(), err)
	}
	return reply
}

func (w *fakeWorker) syncupSide(
	req *pb.SyncupSideRequest,
) *pb.SyncupSideReply {
	w.t.Helper()
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.SyncupSide(ctx, req)
	if err != nil {
		w.t.Fatalf("SyncupSide %s rev %d: %v",
			sidePointerText(req.GetSidePointer()), req.GetRevision(), err)
	}
	return reply
}

func (w *fakeWorker) push(
	req *pb.PushMigrBitmapRequest,
) *pb.PushMigrBitmapReply {
	w.t.Helper()
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.PushMigrBitmap(ctx, req)
	if err != nil {
		w.t.Fatalf("PushMigrBitmap bm_idx %d: %v", req.GetBmIdx(), err)
	}
	return reply
}

// dnRound is one CheckDn round of the test DN at the given desired revision,
// on the worker's one DN stream.
func (w *fakeWorker) dnRound(revision uint64) *pb.CheckDnReply {
	w.t.Helper()
	if w.dn == nil {
		stream, err := w.client.CheckDn(w.ctx)
		if err != nil {
			w.t.Fatalf("CheckDn: %v", err)
		}
		w.dn = stream
	}
	if err := w.dn.Send(&pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: revision,
		ShowInfo: true,
	}); err != nil {
		w.t.Fatalf("CheckDn send: %v", err)
	}
	reply, err := w.dn.Recv()
	if err != nil {
		w.t.Fatalf("CheckDn recv: %v", err)
	}
	return reply
}

// sideRound is one CheckSide round of the side ptr names, on that side's own
// stream.
func (w *fakeWorker) sideRound(
	ptr *pb.SidePointer,
	revision uint64,
) *pb.CheckSideReply {
	w.t.Helper()
	return w.sideRoundTraced(ptr, revision, "")
}

// sideRoundTraced is sideRound under the round's own trace id, which the
// read the round may start runs under too (SH24).
func (w *fakeWorker) sideRoundTraced(
	ptr *pb.SidePointer,
	revision uint64,
	traceId string,
) *pb.CheckSideReply {
	w.t.Helper()
	reply, err := w.trySideRound(ptr, revision, traceId)
	if err != nil {
		w.t.Fatal(err)
	}
	return reply
}

// trySideRound is sideRoundTraced returning its failure instead of failing
// the test, so a goroutine other than the test's can play the round.
func (w *fakeWorker) trySideRound(
	ptr *pb.SidePointer,
	revision uint64,
	traceId string,
) (*pb.CheckSideReply, error) {
	stream := w.sides[ptr.GetSideId()]
	if stream == nil {
		var err error
		stream, err = w.client.CheckSide(w.ctx)
		if err != nil {
			return nil, fmt.Errorf("CheckSide: %w", err)
		}
		w.sides[ptr.GetSideId()] = stream
	}
	if err := stream.Send(&pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: ptr,
		Revision: revision, ShowInfo: true, TraceId: traceId,
	}); err != nil {
		return nil, fmt.Errorf("CheckSide send: %w", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("CheckSide recv: %w", err)
	}
	return reply, nil
}

// fanOutWidth is how many syncups of one node the full shapes keep in
// flight at once. The worker sends each side's syncups from a child of its
// own (dnv-worker.md, RW14), so the agent meets a node's syncups together;
// a bound keeps each call's wait for the agent's locks well inside its
// deadline under the race detector.
const fanOutWidth = 32

// fanOut runs call for every i below n from goroutines of its own, at most
// width at a time, and returns the first failure for the test's goroutine
// to report.
func fanOut(n, width int, call func(i int) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	slots := make(chan struct{}, width)
	for i := 0; i < n; i++ {
		slots <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := call(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return first
}

// trySyncupSide is syncupSide returning its failure, a reply with a code
// other than zero included, instead of failing the test.
func (w *fakeWorker) trySyncupSide(req *pb.SyncupSideRequest) error {
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.SyncupSide(ctx, req)
	if err != nil {
		return fmt.Errorf("SyncupSide %s rev %d: %w",
			sidePointerText(req.GetSidePointer()), req.GetRevision(), err)
	}
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		return fmt.Errorf("SyncupSide %s rev %d: code %d (%s)",
			sidePointerText(req.GetSidePointer()), req.GetRevision(), code,
			reply.GetAgentReply().GetDetails())
	}
	return nil
}

// notOkRows names every row of info, at any depth, whose status is not OK.
// A side or a DN the agent has converged clean reports nothing here.
func notOkRows(info proto.Message) []string {
	var out []string
	var walk func(prefix string, m protoreflect.Message)
	walk = func(prefix string, m protoreflect.Message) {
		if row, ok := m.Interface().(*pb.ResInfo); ok {
			if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				out = append(out, fmt.Sprintf("%s %v %q", prefix,
					row.GetStatus(), row.GetDetails()))
			}
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor,
			v protoreflect.Value) bool {
			name := prefix + "." + string(fd.Name())
			switch {
			case fd.IsMap() && fd.MapValue().Message() != nil:
				v.Map().Range(func(k protoreflect.MapKey,
					mv protoreflect.Value) bool {
					walk(fmt.Sprintf("%s[%v]", name, k.Interface()),
						mv.Message())
					return true
				})
			case fd.IsList() && fd.Message() != nil:
				for i := 0; i < v.List().Len(); i++ {
					walk(fmt.Sprintf("%s[%d]", name, i),
						v.List().Get(i).Message())
				}
			case fd.Message() != nil && !fd.IsMap() && !fd.IsList():
				walk(name, v.Message())
			}
			return true
		})
	}
	if info != nil {
		walk("", info.ProtoReflect())
	}
	return out
}

// assertCleanRound fails unless a round answered code zero at revision, from
// an observation, with every row it carries OK.
func assertCleanRound(
	t *testing.T,
	what string,
	reply interface {
		GetAgentReply() *pb.AgentReply
		GetRevision() uint64
		GetObsStamp() *pb.ObsStamp
	},
	revision uint64,
	info proto.Message,
) {
	t.Helper()
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Errorf("%s: code %d (%s), want 0", what, code,
			reply.GetAgentReply().GetDetails())
	}
	if got := reply.GetRevision(); got != revision {
		t.Errorf("%s: revision %d, want %d", what, got, revision)
	}
	if reply.GetObsStamp().GetObsTime() == nil {
		t.Errorf("%s: answered with no observation", what)
	}
	if rows := notOkRows(info); len(rows) > 0 {
		t.Errorf("%s: rows not OK: %v", what, rows)
	}
}

// ---------------------------------------------------------------------------
// The migration bitmap index bound (DN15)
// ---------------------------------------------------------------------------

// TestFakeWorkerPushIndexBound pushes a destination side the chunks of a
// migration at the bounds of its index, through the gRPC surface. The gateway
// appends no chunk past MaxMigrBmCnt (architecture.md, Migrations), so the
// first index and the last one a migration can hold are applied, and the
// index at the limit is refused by name before anything is persisted: it is
// in no applied set the next SyncupSide reports, so the worker never sees it
// as delivered (DN15).
func TestFakeWorkerPushIndexBound(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	w := newFakeWorker(t, srv)
	if reply := w.syncupDn(dnReq(1, testSide)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	syncupSideTwoPhase(t, srv, migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE))

	for _, bmIdx := range []uint32{0, common.MaxMigrBmCnt - 1} {
		reply := w.push(pushReq(bmIdx, testMigrId, []byte{0x05}))
		if code := reply.GetAgentReply().GetCode(); code != 0 {
			t.Errorf("bm_idx %d: code %d (%s), want it accepted", bmIdx,
				code, reply.GetAgentReply().GetDetails())
		}
	}

	node.Reset()
	over := w.push(pushReq(common.MaxMigrBmCnt, testMigrId, []byte{0x05}))
	if code := over.GetAgentReply().GetCode(); code !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("bm_idx %d: code %d (%s), want ReplyCodeUnknownObject",
			common.MaxMigrBmCnt, code, over.GetAgentReply().GetDetails())
	}
	details := over.GetAgentReply().GetDetails()
	for _, want := range []string{
		fmt.Sprintf("bm_idx %d", common.MaxMigrBmCnt),
		fmt.Sprintf("max_migr_bm_cnt %d", common.MaxMigrBmCnt),
	} {
		if !strings.Contains(details, want) {
			t.Errorf("refusal %q does not name %q", details, want)
		}
	}
	if muts := node.Mutations(); len(muts) > 0 {
		t.Errorf("the refused push changed the node: %v", muts)
	}
	overPath := nf.LocalMigrBmPath(testCluster, testDn, testSp, testMigrId,
		common.MaxMigrBmCnt)
	node.mu.Lock()
	_, stored := node.protos[overPath]
	node.mu.Unlock()
	if stored {
		t.Errorf("the refused chunk was persisted at %s", overPath)
	}

	reply := w.syncupSide(migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	got := reply.GetBmInfo().GetBmIdxList()
	want := []uint32{0, common.MaxMigrBmCnt - 1}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("applied set = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// The full disk node (MaxSideCntPerDn)
// ---------------------------------------------------------------------------

// fullSidePtr is side i of the full disk node: every side of its own pool,
// leg and side id, so no two share a name, an NQN or a record.
func fullSidePtr(i int) *pb.SidePointer {
	id := uint64(0x1000 + i)
	return &pb.SidePointer{SpId: id, LegId: id, SideId: id}
}

// fullDnReq is the full disk node's SyncupDn naming sideCnt sides.
func fullDnReq(revision uint64, sideCnt int) *pb.SyncupDnRequest {
	req := dnReq(revision)
	for i := 0; i < sideCnt; i++ {
		req.SidePointerList = append(req.SidePointerList, fullSidePtr(i))
	}
	return req
}

// fullSideReq is side i's request: one extent, zeroed in one batch, exported
// to the fixture's primary and standby CNs.
func fullSideReq(
	revision uint64,
	i int,
	provisioned bool,
) *pb.SyncupSideRequest {
	req := sideReq(revision, 0, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	req.SidePointer = fullSidePtr(i)
	req.SideConf.ExtCnt = 1
	req.SideConf.ZeroBytes = testExtentSize
	req.SideConf.Provisioned = provisioned
	return req
}

// joinLongReads is joinReads with a bound for the read of a full shape,
// which enumerates every export of a thousand sides.
func joinLongReads(t *testing.T, srv *DnAgentServer) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.reads.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("a read never ended")
	}
}

// sideZeroedById reports whether the record of the side ptr names has
// zeroed its whole length.
func sideZeroedById(t *testing.T, srv *DnAgentServer,
	ptr *pb.SidePointer) bool {
	t.Helper()
	rec, ok, err := srv.meta.LookupSide(context.Background(),
		ptr.GetSpId(), ptr.GetSideId())
	if err != nil {
		t.Fatalf("LookupSide: %v", err)
	}
	return ok && sideFullyZeroed(rec)
}

// TestFakeWorkerConvergesAFullDiskNode builds the most sides one disk node
// holds, MaxSideCntPerDn, and one more, through the worker's own calls: the
// DN's syncup naming every side, each side's provisioning syncup and, once
// its zeroing has finished, its exporting one, the side syncups sent
// concurrently as the worker's children send them, then a round on every
// object's stream. The count is the control plane's bound (architecture.md,
// Capacity index keys), not the agent's: the agent's own bounds are its
// extents and its volume table, so the side past the count converges like
// the others. At this count every side holds one extent run, and the table
// stays far inside its slot (DnTableSlotSize): runs, not sides, are what can
// fill it.
func TestFakeWorkerConvergesAFullDiskNode(t *testing.T) {
	if testing.Short() {
		t.Skip("full shape: skipped under -short")
	}
	const sideCnt = common.MaxSideCntPerDn + 1
	node := newFakeNode()
	node.devSize[testDisk] = common.DnDataOffset +
		uint64(sideCnt+1)*testExtentSize
	node.devNo[testDisk] = "253:0"
	node.dirs[agent.NvmetRoot] = true
	srv := startTestServer(t, node)
	w := newFakeWorker(t, srv)

	start := time.Now()
	if reply := w.syncupDn(fullDnReq(1, sideCnt)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	if err := fanOut(sideCnt, fanOutWidth, func(i int) error {
		return w.trySyncupSide(fullSideReq(1, i, false))
	}); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	deadline := time.Now().Add(time.Minute)
	for i := 0; i < sideCnt; i++ {
		for !sideZeroedById(t, srv, fullSidePtr(i)) {
			if time.Now().After(deadline) {
				t.Fatalf("side %d never finished zeroing", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := fanOut(sideCnt, fanOutWidth, func(i int) error {
		return w.trySyncupSide(fullSideReq(2, i, true))
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	t.Logf("%d sides converged in %v", sideCnt, time.Since(start))
	for i := 0; i < sideCnt; i++ {
		reply := w.sideRound(fullSidePtr(i), 2)
		assertCleanRound(t, fmt.Sprintf("side %d round", i), reply, 2,
			reply.GetSideInfo())
		if len(reply.GetSideInfo().GetCnIdToNvmeof()) != 2 {
			t.Errorf("side %d exports to %d CNs, want the primary and the "+
				"standby", i, len(reply.GetSideInfo().GetCnIdToNvmeof()))
		}
	}
	// The DN's own observation is from its syncup, before any side was
	// allocated; a round starts the read that counts the records.
	srv.roundReads = true
	w.dnRound(1)
	joinLongReads(t, srv)
	dn := w.dnRound(1)
	assertCleanRound(t, "dn round", dn, 1, dn.GetDnInfo())
	counts := dn.GetDnInfo().GetDiskCounts()
	if counts.GetSideRecCnt() != sideCnt || !counts.GetSideRecsMatch() ||
		counts.GetFreeExtCnt() != 1 {
		t.Errorf("disk counts = %v, want %d side records matching the "+
			"list and one extent free", counts, sideCnt)
	}
	srv.meta.mu.Lock()
	tableBytes := dnSlotProtoOff + proto.Size(srv.meta.table)
	srv.meta.mu.Unlock()
	t.Logf("volume table of %d sides: %d bytes of a %d-byte slot",
		sideCnt, tableBytes, common.DnTableSlotSize)
	if tableBytes > common.DnTableSlotSize {
		t.Errorf("the volume table is %d bytes, over its %d-byte slot",
			tableBytes, common.DnTableSlotSize)
	}

	t.Logf("full disk node case took %v", time.Since(start))
}

// ---------------------------------------------------------------------------
// The two size limits of the disk format ([D13])
// ---------------------------------------------------------------------------

// varintValue is the smallest value whose varint takes n bytes, 1 <= n <= 10.
func varintValue(n int) uint64 {
	return uint64(1) << (7 * (n - 1))
}

// fillerRecord is a side record whose encoding inside a DnDiskTable, its tag
// and length included, takes exactly size bytes: 2, or 4 to 46. It holds no
// extent run and its pool id is a power of 128, which no test pool is, so it
// changes no allocation and matches no lookup; only its bytes count.
func fillerRecord(t *testing.T, size int) *pb.DnDiskTable_SideRecord {
	t.Helper()
	// The body is up to four varint fields of 2 to 11 bytes each, tag
	// included; pick their widths greedily.
	body := size - 2
	var widths []int
	for left := body; left > 0; {
		w := min(11, left)
		if left-w == 1 {
			w--
		}
		if w < 2 || len(widths) == 4 {
			t.Fatalf("no filler record of %d bytes", size)
		}
		widths = append(widths, w)
		left -= w
	}
	rec := &pb.DnDiskTable_SideRecord{}
	setters := []func(uint64){
		func(v uint64) { rec.SpId = v },
		func(v uint64) { rec.SideId = v },
		func(v uint64) { rec.ZeroBytes = v },
		func(v uint64) { rec.ZeroedBytes = v },
	}
	for k, w := range widths {
		setters[k](varintValue(w - 1))
	}
	got := proto.Size(&pb.DnDiskTable{
		SideList: []*pb.DnDiskTable_SideRecord{rec}})
	if got != size {
		t.Fatalf("filler record: %d bytes, want %d", got, size)
	}
	return rec
}

// fillerTable is a volume table whose encoding takes exactly size bytes.
func fillerTable(t *testing.T, size int) *pb.DnDiskTable {
	t.Helper()
	const widest = 46
	n, rest := size/widest, size%widest
	table := &pb.DnDiskTable{}
	tail := []int{}
	switch {
	case rest == 0:
	case rest == 1 || rest == 3:
		n--
		tail = []int{widest - 1, rest + 1}
	default:
		tail = []int{rest}
	}
	wide := fillerRecord(t, widest)
	for i := 0; i < n; i++ {
		table.SideList = append(table.SideList,
			proto.Clone(wide).(*pb.DnDiskTable_SideRecord))
	}
	for _, s := range tail {
		table.SideList = append(table.SideList, fillerRecord(t, s))
	}
	if got := proto.Size(table); got != size {
		t.Fatalf("filler table: %d bytes, want %d", got, size)
	}
	return table
}

// TestVolumeTableFillsItsSlot holds the volume table at the size of its slot
// (DnTableSlotSize, architecture.md, Disk node). Side records of real calls
// cannot fill it in a test: at MaxSideCntPerDn sides of one extent run each
// the table is tens of KiB (TestFakeWorkerConvergesAFullDiskNode), and
// filling 16 MiB takes about two million extent runs, a disk of millions of
// extents fragmented by as many allocations. So the table is
// planted through the agent's own save, and the real allocation then lands
// exactly at the room the slot leaves after its envelope: written, and read
// back from the disk as the newest table. One byte more is refused before any
// write, which leaves the record out of the table in memory and on the disk:
// a new side's record, and the zeroing count that grows the record of a side
// already in the table, which so can never record its progress. A slot whose
// length field claims more than the room is no candidate at load, even with a
// checksum that matches, and the other slot wins.
func TestVolumeTableFillsItsSlot(t *testing.T) {
	meta, node := formatted(t)
	ctx := context.Background()
	room := common.DnTableSlotSize - dnSlotProtoOff
	added := proto.Size(&pb.DnDiskTable{
		SideList: []*pb.DnDiskTable_SideRecord{{
			SpId: testSp, SideId: testSide,
			RunList:   []*pb.DnDiskTable_ExtentRun{{Start: 0, Count: 1}},
			ZeroBytes: metaExtentSize,
		}},
	})
	filler := fillerTable(t, room-added)
	meta.mu.Lock()
	err := meta.save(ctx, filler)
	meta.mu.Unlock()
	if err != nil {
		t.Fatalf("planting the filler table: %v", err)
	}
	plantedSeq := meta.seq

	rec, err := meta.AllocSide(ctx, testSp, testSide, 1, metaExtentSize)
	if err != nil {
		t.Fatalf("the allocation that fills the slot exactly: %v", err)
	}
	if runs := rec.GetRunList(); len(runs) != 1 || runs[0].GetStart() != 0 {
		t.Fatalf("allocated %v, want extent 0, which the filler leaves free",
			runs)
	}
	reloaded := reopen(node)
	if err := reloaded.EnsureFormatted(ctx, testCluster, testDn,
		metaExtentSize, mayFormat); err != nil {
		t.Fatalf("reloading the full table: %v", err)
	}
	reloaded.mu.Lock()
	gotSize, gotSeq := proto.Size(reloaded.table), reloaded.seq
	reloaded.mu.Unlock()
	if gotSize != room || gotSeq != plantedSeq+1 {
		t.Fatalf("reloaded table = %d bytes at seq %d, want the full room "+
			"of %d bytes at seq %d", gotSize, gotSeq, room, plantedSeq+1)
	}
	if _, ok, err := reloaded.LookupSide(ctx, testSp, testSide); err != nil ||
		!ok {
		t.Fatalf("the side in the full table did not reload: %v", err)
	}

	slotText := fmt.Sprintf("over the %d slot size", common.DnTableSlotSize)
	node.Reset()
	if _, err := meta.AllocSide(
		ctx, testSp, testSide2, 1, metaExtentSize); err == nil ||
		!strings.Contains(err.Error(), slotText) {
		t.Errorf("an allocation past the slot = %v, want %q", err, slotText)
	}
	if err := meta.SetSideZeroed(
		ctx, testSp, testSide, 0, metaExtentSize); err == nil ||
		!strings.Contains(err.Error(), slotText) {
		t.Errorf("a zeroing step past the slot = %v, want %q", err, slotText)
	}
	assertNoWrite(t, node, "a save past the slot")
	if _, ok, _ := meta.LookupSide(ctx, testSp, testSide2); ok {
		t.Error("the refused side is in the table in memory")
	}
	if rec, _, _ := meta.LookupSide(ctx, testSp, testSide); rec.
		GetZeroedBytes() != 0 {
		t.Errorf("the refused zeroing step moved the count to %d",
			rec.GetZeroedBytes())
	}

	// A slot whose length field claims one byte more than the room is no
	// candidate at load, even with a checksum that matches its envelope and a
	// table of that length: slot B is forged that way one sequence number
	// above slot A, and slot A still loads. Slot A holds the newest table and
	// slot B the forge, because a body past slot A's room would run into slot
	// B's first block and break both slots. A forged slot B of exactly the
	// room loads first, so what refuses the longer one is the size bound and
	// nothing else of the forge.
	var resaved error
	meta.mu.Lock()
	if meta.newestSlot != 0 {
		resaved = meta.save(ctx, meta.table)
	}
	formatUuid, inRoomSeq, newest := meta.hdr.GetFormatUuid(), meta.seq,
		meta.newestSlot
	meta.mu.Unlock()
	if resaved != nil || newest != 0 {
		t.Fatalf("making slot A the newest: slot %d, %v", newest, resaved)
	}
	forgeSlotB := func(size int) {
		body, err := proto.Marshal(fillerTable(t, size))
		if err != nil {
			t.Fatalf("encoding a forged table of %d bytes: %v", size, err)
		}
		block := make([]byte, dnSlotRoundUp(uint64(dnSlotProtoOff+len(body))))
		copy(block[0:8], dnTableMagic)
		binary.LittleEndian.PutUint64(block[8:16], formatUuid)
		binary.LittleEndian.PutUint64(block[16:24], inRoomSeq+1)
		binary.LittleEndian.PutUint32(block[24:28], uint32(len(body)))
		binary.LittleEndian.PutUint32(block[dnSlotCrcOff:],
			dnSlotCrc(block[:dnSlotCrcOff], body))
		copy(block[dnSlotProtoOff:], body)
		node.corruptBlock(metaDisk, dnSlotOffsets[1], block)
	}
	load := func() (*DiskMeta, uint64) {
		again := reopen(node)
		if err := again.EnsureFormatted(ctx, testCluster, testDn,
			metaExtentSize, mayFormat); err != nil {
			t.Fatalf("loading past a forged slot B: %v", err)
		}
		again.mu.Lock()
		defer again.mu.Unlock()
		return again, again.seq
	}
	forgeSlotB(room)
	if _, seq := load(); seq != inRoomSeq+1 {
		t.Fatalf("a forged slot B of exactly the room: loaded seq %d, want "+
			"its %d", seq, inRoomSeq+1)
	}
	forgeSlotB(room + 1)
	loaded, seq := load()
	if seq != inRoomSeq {
		t.Errorf("loaded seq %d, want slot A's %d: the slot past its room "+
			"was a candidate", seq, inRoomSeq)
	}
	if _, ok, _ := loaded.LookupSide(ctx, testSp, testSide); !ok {
		t.Error("slot A's table, which holds the side, did not load")
	}
}

// TestDiskHeaderRecordBound holds the disk header's record to the room its
// 4 KiB block leaves between the length field and the checksum (DnHeaderSize,
// architecture.md, Disk node). The record is seven numbers, so the widest
// header this format writes is a few dozen bytes and never reaches the room:
// it formats and reads back with every field at its widest. The bound binds
// only a record that carries more than those seven fields, which the codec
// writes and reads at exactly the room and refuses one byte over, on the
// write and on the read.
func TestDiskHeaderRecordBound(t *testing.T) {
	ctx := context.Background()
	room := dnHeaderCrcOff - dnHeaderProtoOff
	const widest = math.MaxUint64
	wide := &pb.DnDiskHeader{
		ClusterId: widest, DnId: widest, ExtentSize: widest,
		FormatUuid: widest, DataOffset: widest, CloneMetaOffset: widest,
		CloneMetaSize: widest,
	}
	t.Logf("widest header record: %d of %d bytes", proto.Size(wide), room)
	if proto.Size(wide) >= room {
		t.Fatalf("the widest header record is %d bytes, not below the "+
			"%d-byte room", proto.Size(wide), room)
	}

	meta, node := newTestMeta(t)
	if err := meta.EnsureFormatted(ctx, widest, widest, widest,
		mayFormat); err != nil {
		t.Fatalf("formatting the widest identity: %v", err)
	}
	again := reopen(node)
	if err := again.EnsureFormatted(ctx, widest, widest, widest,
		mayFormat); err != nil {
		t.Fatalf("reading the widest identity back: %v", err)
	}
	if cluster, dn, ok := again.Identity(); !ok || cluster != widest ||
		dn != widest {
		t.Errorf("identity read back = %d/%d/%v, want the widest", cluster,
			dn, ok)
	}

	padded := func(size int) *pb.DnDiskHeader {
		hdr := proto.Clone(wide).(*pb.DnDiskHeader)
		for pad := size - proto.Size(wide); pad >= 0; pad-- {
			unknown := protowire.AppendBytes(
				protowire.AppendTag(nil, 1000, protowire.BytesType),
				make([]byte, pad))
			if proto.Size(wide)+len(unknown) == size {
				hdr.ProtoReflect().SetUnknown(unknown)
				return hdr
			}
		}
		t.Fatalf("no padded header of %d bytes", size)
		return nil
	}
	block, err := buildDnHeader(padded(room))
	if err != nil {
		t.Fatalf("a header record of exactly the room: %v", err)
	}
	hdr, ok, err := parseDnHeader(block)
	if err != nil || !ok || hdr.GetClusterId() != widest ||
		proto.Size(hdr) != room {
		t.Fatalf("the header of exactly the room read back as %v/%v/%v",
			hdr, ok, err)
	}
	if _, err := buildDnHeader(padded(room + 1)); err == nil {
		t.Error("a header record one byte over the room was built")
	}
	// The read side: a block whose length field claims one byte over.
	binary.LittleEndian.PutUint32(block[12:16], uint32(room+1))
	binary.LittleEndian.PutUint32(block[dnHeaderCrcOff:],
		crc32.ChecksumIEEE(block[:dnHeaderCrcOff]))
	if _, _, err := parseDnHeader(block); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Errorf("a header claiming one byte over the room = %v, want "+
			"refused as out of range", err)
	}
}

// ---------------------------------------------------------------------------
// Restarts, with the local files kept and with them lost
// ---------------------------------------------------------------------------

// testSide3 is a third side of the fixture pool, for the cases that add a
// side to a node that holds two.
const testSide3 = uint64(0x18)

// legPtr is the pointer of a side of the fixture pool on a leg of its own:
// the leg id is the side id's, so two sides of one disk node never share a
// leg, as no two sides of a leg share a disk node outside a migration.
func legPtr(sideId uint64) *pb.SidePointer {
	return &pb.SidePointer{SpId: testSp, LegId: sideId << 8, SideId: sideId}
}

// legDnReq is dnReq over legPtr pointers.
func legDnReq(revision uint64, sideIds ...uint64) *pb.SyncupDnRequest {
	req := dnReq(revision)
	for _, sideId := range sideIds {
		req.SidePointerList = append(req.SidePointerList, legPtr(sideId))
	}
	return req
}

// legSideReq is sideReq over a legPtr pointer, exported to the fixture's
// primary and standby CNs.
func legSideReq(revision uint64, sideId uint64) *pb.SyncupSideRequest {
	req := sideReq(revision, sideId, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	req.SidePointer = legPtr(sideId)
	return req
}

// provision plays the sp worker's provisioning flip for one side through the
// worker's surface: the request at provisioned = false, the wait for its
// zeroing, then the request as given.
func (w *fakeWorker) provision(srv *DnAgentServer, req *pb.SyncupSideRequest) {
	w.t.Helper()
	phase1 := proto.Clone(req).(*pb.SyncupSideRequest)
	phase1.SideConf.Provisioned = false
	if reply := w.syncupSide(phase1); reply.GetAgentReply().GetCode() != 0 {
		w.t.Fatalf("provisioning %s: %v",
			sidePointerText(req.GetSidePointer()), reply.GetAgentReply())
	}
	waitZeroed(w.t, srv, req.GetSidePointer().GetSideId())
	if reply := w.syncupSide(req); reply.GetAgentReply().GetCode() != 0 {
		w.t.Fatalf("exporting %s: %v",
			sidePointerText(req.GetSidePointer()), reply.GetAgentReply())
	}
}

// restartAgent plays an agent restart under the worker: the worker's streams
// end, the process exits and joins its background work, and a new process
// starts over the same disk and kernel objects, with its local store kept
// or lost. Its rounds start their reads, as in production (SH29). It returns
// the new server, a new worker dialed to it, and what the startup reconcile
// changed on the node.
func restartAgent(
	t *testing.T,
	w *fakeWorker,
	srv *DnAgentServer,
	node *fakeNode,
	keepFiles bool,
) (*DnAgentServer, *fakeWorker, []string) {
	t.Helper()
	w.close()
	stopTestServer(t, srv)
	node.mu.Lock()
	if !keepFiles {
		node.protos = map[string][]byte{}
	}
	node.mu.Unlock()
	node.Reset()
	restarted := startTestServer(t, node)
	muts := node.Mutations()
	restarted.roundReads = true
	return restarted, newFakeWorker(t, restarted), muts
}

// roundAll runs a round of the DN and of each side, and checks each is clean
// at its revision: an object the worker would send no syncup (dnv-worker.md
// RW4).
func (w *fakeWorker) roundAll(
	what string,
	dnRevision uint64,
	sideRevision uint64,
	sideIds ...uint64,
) {
	w.t.Helper()
	dn := w.dnRound(dnRevision)
	assertCleanRound(w.t, what+": dn round", dn, dnRevision, dn.GetDnInfo())
	for _, sideId := range sideIds {
		side := w.sideRound(legPtr(sideId), sideRevision)
		assertCleanRound(w.t, fmt.Sprintf("%s: side %#x round", what, sideId),
			side, sideRevision, side.GetSideInfo())
	}
}

// failoverSideReq is legSideReq after a failover: the fixture's standby CN
// is the primary, the old primary its standby.
func failoverSideReq(revision uint64, sideId uint64) *pb.SyncupSideRequest {
	req := legSideReq(revision, sideId)
	req.SideConf.PrimaryCnId = testCn1
	req.SideConf.StandbyIdList = []uint64{testCn0}
	return req
}

// TestFakeWorkerRestartsWithKeptFiles restarts the agent four times with its
// local store kept, between rounds of syncups that fail the sides' primary
// over, add a side and drop one. Each startup reconcile converges every held
// object from its stored request (DN2) and changes nothing on a node that
// already matches it (SH16): no device, export or zeroing is redone. It
// stores an observation of each object it converges (SH28), so the first
// round of every object after the restart answers clean at the revision it
// holds, and the worker, whose desired revision is the same, sends nothing
// (dnv-worker.md RW4). A side whose pointer left the DN's list before the
// restart stays unknown after it.
func TestFakeWorkerRestartsWithKeptFiles(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if reply := w.syncupDn(legDnReq(1, testSide, testSide2)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	for _, sideId := range []uint64{testSide, testSide2} {
		w.provision(srv, legSideReq(1, sideId))
	}
	w.roundAll("built", 1, 1, testSide, testSide2)

	restart := func(what string) {
		t.Helper()
		var muts []string
		srv, w, muts = restartAgent(t, w, srv, node, true)
		if len(muts) > 0 {
			t.Errorf("%s: the startup reconcile changed the node: %v",
				what, muts)
		}
	}

	restart("restart 1")
	w.roundAll("after restart 1", 1, 1, testSide, testSide2)

	for _, sideId := range []uint64{testSide, testSide2} {
		if reply := w.syncupSide(failoverSideReq(2, sideId)); reply.
			GetAgentReply().GetCode() != 0 {
			t.Fatalf("failover of side %#x: %v", sideId, reply.GetAgentReply())
		}
	}
	w.roundAll("failed over", 1, 2, testSide, testSide2)
	restart("restart 2")
	w.roundAll("after restart 2", 1, 2, testSide, testSide2)

	if reply := w.syncupDn(legDnReq(2, testSide, testSide2, testSide3)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn adding a side: %v", reply.GetAgentReply())
	}
	w.provision(srv, failoverSideReq(2, testSide3))
	w.roundAll("side added", 2, 2, testSide, testSide2, testSide3)
	restart("restart 3")
	w.roundAll("after restart 3", 2, 2, testSide, testSide2, testSide3)

	if reply := w.syncupDn(legDnReq(3, testSide2, testSide3)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn dropping a side: %v", reply.GetAgentReply())
	}
	dropped := nf.DnSideName(testCluster, testDn, testSp, testSide)
	if dmPresent(node, dropped) {
		t.Errorf("%s survived the SyncupDn that dropped its side", dropped)
	}
	w.roundAll("side dropped", 3, 2, testSide2, testSide3)
	restart("restart 4")
	w.roundAll("after restart 4", 3, 2, testSide2, testSide3)
	if gone := w.sideRound(legPtr(testSide), 2); gone.GetAgentReply().
		GetCode() != common.ReplyCodeUnknownObject ||
		gone.GetRevision() != 0 {
		t.Errorf("the dropped side's round = code %d rev %d, want unknown "+
			"at revision 0", gone.GetAgentReply().GetCode(),
			gone.GetRevision())
	}
	dn := w.dnRound(3)
	if got := dn.GetDnInfo().GetDiskCounts().GetSideRecCnt(); got != 2 {
		t.Errorf("side records after the last restart = %d, want 2", got)
	}
}

// TestFakeWorkerRestartsWithEmptyFiles restarts the agent twice over an empty
// local store, the disk and the kernel objects kept. The agent then holds
// nothing: every round answers the unknown-object code at revision zero
// until the object's syncup arrives (SH25), which is what brings the worker's
// syncups (dnv-worker.md RW4). The DN's syncup comes first; its sweep keeps
// every device of a side its list names, held or not (DN6), and a side's own
// syncup then finds its record on the disk, which is authoritative ([D13]),
// so it adopts what is there: nothing is zeroed again and nothing is
// rebuilt. A side the list no longer names when the second restart's DN
// syncup arrives is swept, devices and record alike.
func TestFakeWorkerRestartsWithEmptyFiles(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if reply := w.syncupDn(legDnReq(1, testSide, testSide2)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	for _, sideId := range []uint64{testSide, testSide2} {
		w.provision(srv, legSideReq(1, sideId))
	}
	w.roundAll("built", 1, 1, testSide, testSide2)

	unknownRounds := func(what string, sideIds ...uint64) {
		t.Helper()
		for _, sideId := range sideIds {
			for round := 0; round < 3; round++ {
				reply := w.sideRound(legPtr(sideId), 1)
				if reply.GetAgentReply().GetCode() !=
					common.ReplyCodeUnknownObject || reply.GetRevision() != 0 {
					t.Errorf("%s: side %#x round %d = code %d rev %d, want "+
						"unknown at revision 0", what, sideId, round,
						reply.GetAgentReply().GetCode(), reply.GetRevision())
				}
			}
		}
	}
	// What a side's own syncup over a lost store may change: its request is
	// saved again (SH5), and nothing on the node.
	adopt := func(what string, req *pb.SyncupSideRequest) {
		t.Helper()
		node.Reset()
		reply := w.syncupSide(req)
		if code := reply.GetAgentReply().GetCode(); code != 0 {
			t.Fatalf("%s: code %d (%s)", what, code,
				reply.GetAgentReply().GetDetails())
		}
		for _, call := range node.Mutations() {
			if !strings.HasPrefix(call, "writeproto ") {
				t.Errorf("%s changed the node: %s", what, call)
			}
		}
	}

	var muts []string
	srv, w, muts = restartAgent(t, w, srv, node, false)
	if len(muts) > 0 {
		t.Errorf("the startup reconcile over an empty store changed the "+
			"node: %v", muts)
	}
	if dn := w.dnRound(1); dn.GetAgentReply().GetCode() !=
		common.ReplyCodeUnknownObject || dn.GetRevision() != 0 {
		t.Errorf("dn round over an empty store = code %d rev %d, want "+
			"unknown at revision 0", dn.GetAgentReply().GetCode(),
			dn.GetRevision())
	}
	unknownRounds("before the dn syncup", testSide, testSide2)
	node.Reset()
	if reply := w.syncupDn(legDnReq(1, testSide, testSide2)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn over an empty store: %v", reply.GetAgentReply())
	}
	for _, call := range node.Mutations() {
		if !strings.HasPrefix(call, "writeproto ") {
			t.Errorf("the dn syncup over an empty store changed the node: %s",
				call)
		}
	}
	unknownRounds("after the dn syncup", testSide, testSide2)
	adopt("side 1 over an empty store", legSideReq(1, testSide))
	unknownRounds("before its own syncup", testSide2)
	adopt("side 2 over an empty store", legSideReq(1, testSide2))
	w.roundAll("adopted", 1, 1, testSide, testSide2)

	// The second restart: side 2 was deleted while the agent was down.
	srv, w, _ = restartAgent(t, w, srv, node, false)
	unknownRounds("after the second restart", testSide, testSide2)
	if reply := w.syncupDn(legDnReq(2, testSide)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupDn naming one side: %v", reply.GetAgentReply())
	}
	swept := nf.DnSideName(testCluster, testDn, testSp, testSide2)
	if dmPresent(node, swept) {
		t.Errorf("%s survived the sweep of a list that no longer names it",
			swept)
	}
	if !dmPresent(node, nf.DnSideName(testCluster, testDn, testSp,
		testSide)) {
		t.Error("the sweep took the listed side's device")
	}
	adopt("side 1 after the second restart", legSideReq(1, testSide))
	w.roundAll("one side left", 2, 1, testSide)
	unknownRounds("the swept side", testSide2)
	joinReads(t, srv)
	dn := w.dnRound(2)
	if got := dn.GetDnInfo().GetDiskCounts().GetSideRecCnt(); got != 1 {
		t.Errorf("side records after the sweep = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// A command that hangs, and a syncup that arrives during a converge
// ---------------------------------------------------------------------------

// sideSyncup is the outcome of one SyncupSide sent from its own goroutine.
type sideSyncup struct {
	reply *pb.SyncupSideReply
	err   error
}

// asyncSyncupSide sends one SyncupSide from its own goroutine; its outcome
// arrives on the returned channel, for the test's goroutine to judge.
func (w *fakeWorker) asyncSyncupSide(
	req *pb.SyncupSideRequest,
) <-chan sideSyncup {
	done := make(chan sideSyncup, 1)
	go func() {
		ctx, cancel := w.callCtx()
		defer cancel()
		reply, err := w.client.SyncupSide(ctx, req)
		if err != nil {
			err = fmt.Errorf("SyncupSide rev %d: %w", req.GetRevision(), err)
		}
		done <- sideSyncup{reply, err}
	}()
	return done
}

// awaitSide waits for the outcome of an asyncSyncupSide and fails the test
// unless the syncup replied.
func (w *fakeWorker) awaitSide(
	done <-chan sideSyncup,
	what string,
) *pb.SyncupSideReply {
	w.t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			w.t.Fatalf("%s: %v", what, r.err)
		}
		return r.reply
	case <-time.After(15 * time.Second):
		w.t.Fatalf("%s never replied", what)
	}
	return nil
}

// promptRound runs one side round and fails the test unless it answers
// within the bound a worker's round deadline would allow on a loaded
// machine.
func (w *fakeWorker) promptRound(
	ptr *pb.SidePointer,
	revision uint64,
	traceId string,
) *pb.CheckSideReply {
	w.t.Helper()
	type outcome struct {
		reply *pb.CheckSideReply
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		reply, err := w.trySideRound(ptr, revision, traceId)
		done <- outcome{reply, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			w.t.Fatalf("%s: %v", traceId, r.err)
		}
		return r.reply
	case <-time.After(2 * time.Second):
		w.t.Fatalf("%s: a CheckSide round did not return within 2s", traceId)
	}
	return nil
}

// TestFakeWorkerHungConnectAcrossRounds makes a plain side a migration
// destination whose connect to the source hangs until the soft timeout kills
// it (SH15), across many check rounds and past the pass's connect budget
// (DnMigrConnectPassBudget). Every round meanwhile answers at once from the
// side's observation before the syncup, with the converge's start, and
// starts no read of a side whose converge is in flight (SH28 to SH30). The
// hang spends the whole budget, so the pass tries no second connect (DN13),
// reports the target in error and leaves the destination to the DN8 retry,
// which connects once the source answers: the next rounds answer the retry's
// observation at the syncup's revision, with no further syncup.
func TestFakeWorkerHungConnectAcrossRounds(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	if reply := w.syncupDn(dnReq(1, testSide)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	w.provision(srv, sideReq(1, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE))
	srv.roundReads = true
	w.sideRound(sidePtr(testSide), 1)
	joinReads(t, srv)
	before := sideObs(t, srv).Time

	// The DN8 retry the hung pass registers comes soon after it, so the
	// case does not wait out the production interval.
	srv.migrRetryInterval = 200 * time.Millisecond
	release := softGate(t, node, "cmd nvme connect ")
	start := time.Now()
	done := w.asyncSyncupSide(migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	waitUntil(t, "the destination's connect", func() bool {
		return node.hasCall("cmd nvme connect ")
	})
	type played struct {
		traceId string
		reply   *pb.CheckSideReply
	}
	var rounds []played
	var reply *pb.SyncupSideReply
	for reply == nil {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			reply = r.reply
			continue
		case <-time.After(200 * time.Millisecond):
		}
		if time.Since(start) > 15*time.Second {
			t.Fatal("the syncup never returned past its killed connect")
		}
		traceId := fmt.Sprintf("hung-round-%d", len(rounds))
		rounds = append(rounds, played{traceId,
			w.promptRound(sidePtr(testSide), 2, traceId)})
	}
	if took := time.Since(start); took < common.DnMigrConnectPassBudget {
		t.Errorf("the syncup returned after %v, before the hang outlasted "+
			"the pass budget", took)
	}
	// The rounds answered before the converge stored its observation are
	// the ones the hang spanned.
	converged := reply.GetObsStamp().GetObsTime().AsTime()
	hung, pastBudget := 0, 0
	for _, r := range rounds {
		stamp := r.reply.GetObsStamp()
		if !stamp.GetAnswerTime().AsTime().Before(converged) {
			continue
		}
		hung++
		if r.reply.GetRevision() != 1 ||
			!stamp.GetObsTime().AsTime().Equal(before) ||
			stamp.GetConvergeStart() == nil {
			t.Errorf("%s during the hang = rev %d, stamp %v; want the "+
				"observation before the syncup, at revision 1, and the "+
				"converge's start", r.traceId, r.reply.GetRevision(), stamp)
			continue
		}
		if stamp.GetAnswerTime().AsTime().Sub(
			stamp.GetConvergeStart().AsTime()) >
			common.DnMigrConnectPassBudget {
			pastBudget++
		}
		if calls := node.Traced(r.traceId); len(calls) > 0 {
			t.Errorf("%s started a read during the converge: %v",
				r.traceId, calls)
		}
	}
	if hung < 3 || pastBudget == 0 {
		t.Errorf("%d rounds answered during the hang, %d of them past the "+
			"pass budget; want several, and one past it", hung, pastBudget)
	}
	if n := len(node.callsMatching("cmd nvme connect ")); n != 1 {
		t.Errorf("the pass made %d connects, want the one that hung: the "+
			"hang spent the budget", n)
	}
	target := reply.GetSideInfo().GetMigrDstInfo().GetTargetInfo()
	if code := reply.GetAgentReply().GetCode(); code != 0 ||
		target.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("syncup reply = code %d, target %v/%q; want code 0 and the "+
			"target in error", code, target.GetStatus(), target.GetDetails())
	}

	// The source answers again; the DN8 retry finishes the destination.
	release()
	deadline := time.Now().Add(15 * time.Second)
	for {
		round := w.sideRound(sidePtr(testSide), 2)
		dst := round.GetSideInfo().GetMigrDstInfo()
		if round.GetRevision() == 2 &&
			dst.GetTargetInfo().GetStatus() == pb.ResStatus_RES_STATUS_OK &&
			dst.GetDmCloneInfo().GetStatus() == pb.ResStatus_RES_STATUS_OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retry never finished the destination: %v", round)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFakeWorkerSyncupDuringAConverge sends a side's next syncup while the
// converge of the one before it hangs in a command. The newer syncup marks
// its converge as it arrives and then waits for the side's lock (SH30): the
// older converge runs to its end and replies first, at its own revision, and
// the newer one converges after it. Rounds meanwhile answer at once from the
// observation before both, with the start of the first converge, which the
// newer syncup's mark does not move (SH30), and start no read (SH29). The
// rounds after answer the newer converge's observation.
func TestFakeWorkerSyncupDuringAConverge(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	w := newFakeWorker(t, srv)
	if reply := w.syncupDn(dnReq(1, testSide)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	w.provision(srv, sideReq(1, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE))
	srv.roundReads = true
	w.sideRound(sidePtr(testSide), 1)
	joinReads(t, srv)
	before := sideObs(t, srv).Time

	// The failover's converge hangs in its first suspend, which no read
	// issues.
	node.blockCmd("cmd dmsetup suspend ")
	t.Cleanup(func() { node.releaseCmd("cmd dmsetup suspend ") })
	olderSent := time.Now()
	older := w.asyncSyncupSide(failoverReq(2))
	waitUntil(t, "the failover's converge to hang", func() bool {
		return node.hasCall("cmd dmsetup suspend ")
	})
	newerSent := time.Now()
	newer := w.asyncSyncupSide(sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	// The rounds come once the newer syncup has marked its converge: a read
	// one of them started before that mark would be the one the mark
	// cancels, which counts as running until it ends and so would keep
	// every later round from starting a read whatever the rule.
	time.Sleep(200 * time.Millisecond)
	var traces []string
	var firstStart *timestamppb.Timestamp
	for round := 0; round < 5; round++ {
		traceId := fmt.Sprintf("busy-round-%d", round)
		traces = append(traces, traceId)
		reply := w.promptRound(sidePtr(testSide), 3, traceId)
		if round == 0 {
			firstStart = reply.GetObsStamp().GetConvergeStart()
			if firstStart == nil {
				t.Fatal("a round during the converge reports no converge " +
					"start")
			}
			// The side's marks have stood without a break since the older
			// syncup arrived, so the start is that one's, not the newer's
			// (SH30).
			if got := firstStart.AsTime(); got.Before(olderSent) ||
				!got.Before(newerSent) {
				t.Errorf("converge start %v, want the older syncup's: "+
					"after it was sent at %v and before the newer one was "+
					"sent at %v", got.UTC(), olderSent.UTC(),
					newerSent.UTC())
			}
		}
		if reply.GetRevision() != 1 ||
			!reply.GetObsStamp().GetObsTime().AsTime().Equal(before) ||
			!proto.Equal(reply.GetObsStamp().GetConvergeStart(),
				firstStart) {
			t.Errorf("round %d = rev %d, stamp %v; want the observation "+
				"before both syncups and the first converge's start",
				round, reply.GetRevision(), reply.GetObsStamp())
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-newer:
		t.Fatal("the newer syncup replied while the older converge held " +
			"the side")
	default:
	}

	node.releaseCmd("cmd dmsetup suspend ")
	olderReply := w.awaitSide(older, "the older syncup")
	newerReply := w.awaitSide(newer, "the newer syncup")
	if olderReply.GetRevision() != 2 || olderReply.GetAgentReply().
		GetCode() != 0 {
		t.Errorf("older reply = code %d rev %d, want 0 at 2",
			olderReply.GetAgentReply().GetCode(), olderReply.GetRevision())
	}
	if newerReply.GetRevision() != 3 || newerReply.GetAgentReply().
		GetCode() != 0 {
		t.Errorf("newer reply = code %d rev %d, want 0 at 3",
			newerReply.GetAgentReply().GetCode(), newerReply.GetRevision())
	}
	if !newerReply.GetObsStamp().GetObsTime().AsTime().After(
		olderReply.GetObsStamp().GetObsTime().AsTime()) {
		t.Errorf("the newer converge's observation %v is not after the "+
			"older's %v", newerReply.GetObsStamp().GetObsTime().AsTime(),
			olderReply.GetObsStamp().GetObsTime().AsTime())
	}
	joinReads(t, srv)
	for _, traceId := range traces {
		if calls := node.Traced(traceId); len(calls) > 0 {
			t.Errorf("round %s started a read during the converges: %v",
				traceId, calls)
		}
	}
	after := w.sideRound(sidePtr(testSide), 3)
	assertCleanRound(t, "round after both", after, 3, after.GetSideInfo())
	if after.GetObsStamp().GetConvergeStart() != nil {
		t.Errorf("a converge start stands after both replied: %v",
			after.GetObsStamp())
	}
	// The newer request, the fail-back, is what the node serves.
	state, err := srv.nvmet.ProbeNamespace(context.Background(),
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0), sideNsid)
	if err != nil {
		t.Fatalf("ProbeNamespace: %v", err)
	}
	if state.AnaGrpId != common.AnaGrpIdOptimized {
		t.Errorf("cn %d ana_grpid = %d, want optimized after the fail-back",
			testCn0, state.AnaGrpId)
	}
}

// ---------------------------------------------------------------------------
// A sweep that meets stale listings
// ---------------------------------------------------------------------------

// leftoverNames is the sorted list of objects a leftover reply's details
// name, read as LeftoverReply writes them when it names cnt objects and no
// failure; nil if details is not such a reply.
func leftoverNames(details string, cnt int) []string {
	body, ok := strings.CutPrefix(details, fmt.Sprintf("leftover(%d): ", cnt))
	if !ok || strings.Contains(body, "; ") {
		return nil
	}
	names := strings.Split(body, ", ")
	slices.Sort(names)
	return names
}

// TestFakeWorkerSweepMeetsStaleListings drops a side while both of the
// sweep's listings are stale: `dmsetup ls` and the nvmet subsystems listing
// name this node's devices and exports of a side that are already gone, as
// another removal leaves them between a listing and the probes after it.
// The sweep removes what is really left over and verifies every removal by a
// probe (DN6), so a name that is gone is nothing to remove and no leftover:
// the DN's syncup and the kept side's syncup answer clean. A DN read over
// the stale listing names the stale dm devices as leftovers and nothing
// else, its verdict being the sweep with the removals left out (DN16): it
// passes over the stale exports, whose age read finds them gone (DN6). The
// SyncupDn that brings answers clean again, and once the listing is fresh,
// every round is clean.
func TestFakeWorkerSweepMeetsStaleListings(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	w := newFakeWorker(t, srv)
	if reply := w.syncupDn(legDnReq(1, testSide, testSide2)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	for _, sideId := range []uint64{testSide, testSide2} {
		w.provision(srv, legSideReq(1, sideId))
	}
	srv.roundReads = true
	w.roundAll("built", 1, 1, testSide, testSide2)
	joinReads(t, srv)

	// The ghosts are a third side's: its side device, its per-CN dm-linears
	// and dm-errors, and its exports, every name this node's.
	ghost := legPtr(testSide3)
	var dmGhosts, nqnGhosts []string
	dmGhosts = append(dmGhosts, nf.DnSideName(testCluster, testDn, testSp,
		ghost.GetSideId()))
	for _, cnId := range []uint64{testCn0, testCn1} {
		dmGhosts = append(dmGhosts,
			nf.DnLinearName(testCluster, testDn, testSp, ghost.GetSideId(),
				cnId),
			nf.DnErrorName(testCluster, testDn, testSp, ghost.GetSideId(),
				cnId))
		nqnGhosts = append(nqnGhosts, nf.SideToCnNqn(testCluster, testSp,
			ghost.GetLegId(), cnId))
	}
	// The read's leftover names the dm ghosts and no export: an export's age
	// read finds a ghost subsystem gone, so it is passed over (DN6).
	var wantLeft []string
	for _, name := range dmGhosts {
		wantLeft = append(wantLeft, agent.LeftoverKindDm+":"+name)
	}
	slices.Sort(wantLeft)
	// A re-sent SyncupDn may try to remove a ghost, which it then finds gone;
	// it removes nothing else.
	isGhostRemoval := func(call string) bool {
		fields := strings.Fields(call)
		return strings.HasPrefix(call, "cmd dmsetup remove ") &&
			slices.Contains(dmGhosts, fields[len(fields)-1])
	}
	node.mu.Lock()
	for i, name := range dmGhosts {
		node.devNo["/dev/mapper/"+name] = fmt.Sprintf("253:%d", 200+i)
	}
	node.lsGhosts = dmGhosts
	node.subsysGhosts = nqnGhosts
	node.mu.Unlock()

	dropped := nf.DnSideName(testCluster, testDn, testSp, testSide2)
	reply := w.syncupDn(legDnReq(2, testSide))
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Errorf("SyncupDn over stale listings: code %d (%s), want 0", code,
			reply.GetAgentReply().GetDetails())
	}
	if rows := notOkRows(reply.GetDnInfo()); len(rows) > 0 {
		t.Errorf("SyncupDn over stale listings: rows %v", rows)
	}
	if dmPresent(node, dropped) {
		t.Errorf("%s survived the sweep that dropped its side", dropped)
	}
	for _, cnId := range []uint64{testCn0, testCn1} {
		nqn := nf.SideToCnNqn(testCluster, testSp, legPtr(testSide2).
			GetLegId(), cnId)
		if subsysPresent(node, nqn) {
			t.Errorf("%s survived the sweep that dropped its side", nqn)
		}
	}
	kept := w.syncupSide(legSideReq(1, testSide))
	assertCleanRound(t, "the kept side's syncup over stale listings", kept,
		1, kept.GetSideInfo())

	// A read takes its verdict from the same listings with the removals
	// left out (DN16), so it names the stale devices as leftovers, and the
	// SyncupDn the worker re-sends for them (dnv-worker.md RW4) probes them
	// gone and answers clean. A side's read covers its own side only.
	for round := 0; round < 3; round++ {
		what := fmt.Sprintf("stale round %d", round)
		side := w.sideRound(legPtr(testSide), 1)
		assertCleanRound(t, what+": side round", side, 1, side.GetSideInfo())
		w.dnRound(2)
		joinReads(t, srv)
		dn := w.dnRound(2)
		if dn.GetAgentReply().GetCode() != common.ReplyCodeLeftover {
			t.Errorf("%s: dn round over a stale listing = code %d (%s), "+
				"want the read's leftover", what,
				dn.GetAgentReply().GetCode(), dn.GetAgentReply().GetDetails())
		}
		if got := leftoverNames(dn.GetAgentReply().GetDetails(),
			len(wantLeft)); !slices.Equal(got, wantLeft) {
			t.Errorf("%s: the read's leftover %q, want exactly the dm "+
				"ghosts %v", what, dn.GetAgentReply().GetDetails(), wantLeft)
		}
		node.Reset()
		resent := w.syncupDn(legDnReq(2, testSide))
		if code := resent.GetAgentReply().GetCode(); code != 0 {
			t.Errorf("%s: re-sent SyncupDn = code %d (%s), want 0", what,
				code, resent.GetAgentReply().GetDetails())
		}
		for _, call := range node.Mutations() {
			if strings.HasPrefix(call, "writeproto ") || isGhostRemoval(call) {
				continue
			}
			t.Errorf("%s: the re-sent SyncupDn changed the node: %s", what,
				call)
		}
		joinReads(t, srv)
	}

	node.mu.Lock()
	node.lsGhosts, node.subsysGhosts = nil, nil
	node.mu.Unlock()
	w.roundAll("fresh round", 2, 1, testSide)
	joinReads(t, srv)
	w.roundAll("fresh round after its read", 2, 1, testSide)
}
