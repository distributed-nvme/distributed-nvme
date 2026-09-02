package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Revision gate (SH8, SH9)
// ---------------------------------------------------------------------------

func TestGateRevision(t *testing.T) {
	if reply := GateRevision(0, 1); reply != nil {
		t.Errorf("a first request was rejected: %v", reply)
	}
	if reply := GateRevision(7, 7); reply != nil {
		t.Errorf("an equal revision was rejected: %v", reply)
	}
	if reply := GateRevision(7, 8); reply != nil {
		t.Errorf("a higher revision was rejected: %v", reply)
	}
	reply := GateRevision(7, 6)
	if reply.GetCode() != common.ReplyCodeStaleRevision {
		t.Fatalf("code = %d, want ReplyCodeStaleRevision", reply.GetCode())
	}
	if !strings.Contains(reply.GetDetails(), "6") ||
		!strings.Contains(reply.GetDetails(), "7") {
		t.Errorf("details %q names neither revision", reply.GetDetails())
	}
	if code := UnknownObjectReply("side %d", 3).GetCode(); code !=
		common.ReplyCodeUnknownObject {
		t.Errorf("code = %d, want ReplyCodeUnknownObject", code)
	}
	if OkReply().GetCode() != 0 {
		t.Error("OkReply is not code 0")
	}
}

// ---------------------------------------------------------------------------
// Lock hierarchy (SH10-SH13)
// ---------------------------------------------------------------------------

func TestLockSet(t *testing.T) {
	locks := NewLockSet()
	if locks.Obj("a") != locks.Obj("a") {
		t.Error("Obj returned two different locks for one key")
	}
	if locks.Obj("a") == locks.Obj("b") {
		t.Error("two keys share a lock")
	}
	first := locks.Obj("a")
	locks.DropObj("a")
	if locks.Obj("a") == first {
		t.Error("DropObj kept the old lock")
	}

	// Node read locks do not exclude each other; a writer does.
	locks.Node().RLock()
	second := make(chan struct{})
	go func() {
		locks.Node().RLock()
		locks.Node().RUnlock()
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("two node read locks excluded each other")
	}
	writer := make(chan struct{})
	go func() {
		locks.Node().Lock()
		locks.Node().Unlock()
		close(writer)
	}()
	select {
	case <-writer:
		t.Fatal("the node write lock ignored a live reader")
	case <-time.After(50 * time.Millisecond):
	}
	locks.Node().RUnlock()
	select {
	case <-writer:
	case <-time.After(time.Second):
		t.Fatal("the node write lock never acquired")
	}
}

func TestLockSetConcurrentObjCreation(t *testing.T) {
	locks := NewLockSet()
	var wg sync.WaitGroup
	got := make([]*sync.Mutex, 16)
	for i := range got {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got[idx] = locks.Obj("shared")
		}(i)
	}
	wg.Wait()
	for _, lock := range got {
		if lock != got[0] {
			t.Fatal("concurrent Obj calls created different locks")
		}
	}
}

// ---------------------------------------------------------------------------
// ResInfo tracking (SH14)
// ---------------------------------------------------------------------------

func TestResTrackerEpoch(t *testing.T) {
	tracker := NewResTracker()
	now := int64(100)
	tracker.now = func() int64 { return now }

	first := tracker.Ok("lv", "side-lv", "")
	if first.GetEpoch() != 100 {
		t.Fatalf("epoch = %d, want 100", first.GetEpoch())
	}
	// A details-only change does not bump the epoch.
	now = 200
	same := tracker.Ok("lv", "side-lv", "hydrated 3/10")
	if same.GetEpoch() != 100 {
		t.Errorf("a details change bumped the epoch to %d", same.GetEpoch())
	}
	if same.GetDetails() != "hydrated 3/10" {
		t.Errorf("details = %q", same.GetDetails())
	}
	// A status change does.
	now = 300
	changed := tracker.Err("lv", "side-lv", "not_trimmed")
	if changed.GetEpoch() != 300 {
		t.Errorf("epoch = %d, want 300", changed.GetEpoch())
	}
	if changed.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("status = %v", changed.GetStatus())
	}
	// Dropping forgets the history, so a re-creation reports a fresh epoch.
	now = 400
	tracker.Drop("lv")
	if got := tracker.Err("lv", "side-lv", "x").GetEpoch(); got != 400 {
		t.Errorf("epoch after Drop = %d, want 400", got)
	}
	// The returned message is a copy: mutating it cannot corrupt the state.
	info := tracker.Ok("k", "n", "d")
	info.Details = "mutated"
	if again := tracker.Ok("k", "n", "d"); again.GetDetails() != "d" {
		t.Errorf("tracker state was aliased: %q", again.GetDetails())
	}
	if got := tracker.Missing("m", "n", "").GetStatus(); got !=
		pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("status = %v, want MISSING", got)
	}
}

// ---------------------------------------------------------------------------
// Local store (SH4-SH7)
// ---------------------------------------------------------------------------

func TestStoreListFiltersByKind(t *testing.T) {
	names := []string{
		"dn-0000000000000001-0000000000000003",
		"side-0000000000000001-0000000000000003-11-16",
		"migr-bm-0000000000000001-0000000000000003-11-21-00",
		"cn-0000000000000001-0000000000000005",
		"unrelated",
	}
	oc := &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			if name != "ls" || args[1] != "/store" {
				t.Errorf("unexpected command %s %v", name, args)
			}
			return strings.Join(names, "\n") + "\n", "", 0, nil
		},
	}
	store := NewStore(oc, "/store")
	files, err := store.List(context.Background(),
		StoreKindDn, StoreKindSide, StoreKindMigrBm)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := files[StoreKindDn]; len(got) != 1 ||
		got[0] != "/store/"+names[0] {
		t.Errorf("dn files = %v", got)
	}
	if got := files[StoreKindSide]; len(got) != 1 {
		t.Errorf("side files = %v", got)
	}
	if got := files[StoreKindMigrBm]; len(got) != 1 {
		t.Errorf("migr-bm files = %v", got)
	}
	// cn-* and unrelated names are not the dn role's business.
	if _, ok := files[StoreKindCn]; ok {
		t.Error("an unrequested kind was returned")
	}
}

func TestStoreListUnreadablePrefixIsFatal(t *testing.T) {
	oc := &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			return "", "No such file or directory", 2,
				context.DeadlineExceeded
		},
	}
	_, err := NewStore(oc, "/nope").List(context.Background(), StoreKindDn)
	if err == nil {
		t.Fatal("an unreadable store prefix was not reported")
	}
	if !strings.Contains(err.Error(), "/nope") {
		t.Errorf("error %q does not name the prefix", err)
	}
}

// ---------------------------------------------------------------------------
// Bitmap chunk store and skip-range math (SH21-SH23, §11.4)
// ---------------------------------------------------------------------------

func TestBitmapBit(t *testing.T) {
	bitmap := []byte{0b0000_0101, 0b1000_0000}
	for _, want := range []struct {
		idx uint64
		set bool
	}{{0, true}, {1, false}, {2, true}, {7, false}, {15, true}, {99, false}} {
		if got := BitmapBit(bitmap, want.idx); got != want.set {
			t.Errorf("bit %d = %v, want %v", want.idx, got, want.set)
		}
	}
	if got := BitmapBitCount(bitmap); got != 16 {
		t.Errorf("bit count = %d, want 16", got)
	}
}

func TestChunkSetContiguousPrefix(t *testing.T) {
	chunks := NewChunkSet()
	chunks.Put(1, []byte{0x02})
	chunks.Put(3, []byte{0x08})
	// Chunk 0 is missing, so nothing is placeable yet (SH23) — but every
	// file present is still reported as applied (SH21).
	if got := chunks.ContiguousPrefix(); len(got) != 0 {
		t.Errorf("prefix = %v, want empty", got)
	}
	if got := chunks.Indexes(); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("indexes = %v", got)
	}
	chunks.Put(0, []byte{0x01})
	if got := chunks.ContiguousPrefix(); len(got) != 2 ||
		got[0] != 0x01 || got[1] != 0x02 {
		t.Errorf("prefix = %v, want [1 2]", got)
	}
	chunks.Put(2, []byte{0x04})
	if got := chunks.ContiguousPrefix(); len(got) != 4 {
		t.Errorf("prefix = %v, want all four chunks", got)
	}
	chunks.Delete(1)
	if got := chunks.ContiguousPrefix(); len(got) != 1 {
		t.Errorf("prefix after deleting chunk 1 = %v", got)
	}
	if _, ok := chunks.Get(2); !ok {
		t.Error("self-positioned access lost chunk 2")
	}
}

func TestSkipRanges(t *testing.T) {
	const region = uint64(1 << 20)
	// Bits 0 and 2 set, shifted past 3 meta blocks ⇒ regions 3 and 5.
	got := SkipRanges([]byte{0b0000_0101}, 3, 10, region)
	want := []SkipRange{
		{Offset: 3 * region, Length: region},
		{Offset: 5 * region, Length: region},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ranges = %v, want %v", got, want)
	}
	// Adjacent skippable regions coalesce into one blkdiscard.
	got = SkipRanges([]byte{0b0000_0111}, 0, 10, region)
	if len(got) != 1 || got[0].Offset != 0 || got[0].Length != 3*region {
		t.Fatalf("adjacent regions did not coalesce: %v", got)
	}
	// A bitmap longer than the device discards nothing beyond it.
	got = SkipRanges([]byte{0xff}, 0, 4, region)
	if len(got) != 1 || got[0].Length != 4*region {
		t.Fatalf("ranges = %v, want a single 4-region range", got)
	}
	// The meta region is never skippable, whatever the bitmap says.
	got = SkipRanges([]byte{0xff}, 3, 10, region)
	if len(got) != 1 || got[0].Offset != 3*region {
		t.Fatalf("ranges = %v, want the meta region excluded", got)
	}
	if SkipRanges([]byte{0xff}, 0, 10, 0) != nil {
		t.Error("a zero region size produced ranges")
	}
	if SkipRanges(nil, 0, 10, region) != nil {
		t.Error("an empty bitmap produced ranges")
	}
}

func TestApplySkipRanges(t *testing.T) {
	var got []string
	oc := &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			got = append(got, name+" "+strings.Join(args, " "))
			return "", "", 0, nil
		},
	}
	err := ApplySkipRanges(context.Background(), NewDm(oc), "/dev/mapper/x",
		[]SkipRange{{Offset: 0, Length: 4096}})
	if err != nil {
		t.Fatalf("ApplySkipRanges: %v", err)
	}
	if len(got) != 1 ||
		got[0] != "blkdiscard --offset 0 --length 4096 /dev/mapper/x" {
		t.Errorf("commands = %v", got)
	}
}

// ---------------------------------------------------------------------------
// dm table builders and status parsing
// ---------------------------------------------------------------------------

func TestTableBuilders(t *testing.T) {
	if got := ErrorTable(2048); got != "0 2048 error" {
		t.Errorf("error table = %q", got)
	}
	if got := LinearTable(2048, "253:3", 0); got != "0 2048 linear 253:3 0" {
		t.Errorf("linear table = %q", got)
	}
	got := CloneTable(2048, "253:1", "253:2", "259:0", 2048, true, 1, 2)
	want := "0 2048 clone 253:1 253:2 259:0 2048 1 no_hydration 4 " +
		"hydration_threshold 1 hydration_batch_size 2"
	if got != want {
		t.Errorf("clone table = %q, want %q", got, want)
	}
	if got := CloneTable(
		2048, "253:1", "253:2", "259:0", 2048, false, 0, 0); got !=
		"0 2048 clone 253:1 253:2 259:0 2048 0 0" {
		t.Errorf("bare clone table = %q", got)
	}
}

func TestParseDmLinesAndCloneStatus(t *testing.T) {
	targets := ParseDmLines("0 2048 linear 253:3 0\n")
	if len(targets) != 1 || targets[0].Type != "linear" ||
		targets[0].Length != 2048 || targets[0].Args[0] != "253:3" {
		t.Fatalf("parsed %v", targets)
	}
	if got := ParseDmLines("garbage\n"); len(got) != 0 {
		t.Errorf("garbage parsed as %v", got)
	}

	raw := "0 20480 clone 8 1/1024 2048 4/10 0 1 no_hydration 4 " +
		"hydration_threshold 3 hydration_batch_size 5"
	status, ok := ParseCloneStatus(raw)
	if !ok {
		t.Fatal("clone status did not parse")
	}
	if status.RegionSectors != 2048 || status.HydratedRegions != 4 ||
		status.TotalRegions != 10 {
		t.Errorf("status = %+v", status)
	}
	if status.HydrationEnabled {
		t.Error("no_hydration was not detected")
	}
	if status.Threshold != 3 || status.BatchSize != 5 {
		t.Errorf("knobs = %d/%d", status.Threshold, status.BatchSize)
	}
	enabled, ok := ParseCloneStatus(
		"0 20480 clone 8 1/1024 2048 4/10 0 0 4 " +
			"hydration_threshold 1 hydration_batch_size 1")
	if !ok || !enabled.HydrationEnabled {
		t.Errorf("hydration state = %+v", enabled)
	}
	if _, ok := ParseCloneStatus("0 2048 linear 253:3 0"); ok {
		t.Error("a linear status parsed as a clone")
	}
}

func TestAnaStateOf(t *testing.T) {
	for grpId, want := range map[int]string{
		common.AnaGrpIdOptimized:    AnaStateOptimized,
		common.AnaGrpIdNonOptimized: AnaStateNonOptimized,
		common.AnaGrpIdInaccessible: AnaStateInaccessible,
	} {
		if got := AnaStateOf(grpId); got != want {
			t.Errorf("group %d = %q, want %q", grpId, got, want)
		}
	}
	if AnaStateOf(99) != "" {
		t.Error("an unknown group id got a state")
	}
}

// ---------------------------------------------------------------------------
// nvme list-subsys parsing (SH20)
// ---------------------------------------------------------------------------

func TestListSubsysBothJsonShapes(t *testing.T) {
	const nqn = "nqn.2024-01.io.dnv:3:a:b:c"
	arrayForm := `[{"HostNQN":"h","Subsystems":[{"Name":"nvme-subsys0",
	  "NQN":"` + nqn + `","Paths":[{"Name":"nvme3","State":"live"}],
	  "Namespaces":[{"NameSpace":"nvme3n1","NSID":1}]}]}]`
	objectForm := `{"HostNQN":"h","Subsystems":[{"Name":"nvme-subsys0",
	  "NQN":"` + nqn + `","Paths":[{"Name":"nvme3","State":"connecting",
	  "Namespaces":[{"Name":"nvme3n1"}]}]}]}`

	for name, payload := range map[string]string{
		"array": arrayForm, "object": objectForm,
	} {
		oc := &common.FakeOsClient{
			RunCommandFn: func(
				ctx context.Context, cmd string, args []string, stdin string,
			) (string, string, int, error) {
				return payload, "", 0, nil
			},
		}
		state, err := NewNvmeHost(oc).ListSubsys(context.Background(), nqn)
		if err != nil {
			t.Fatalf("%s: ListSubsys: %v", name, err)
		}
		if !state.Found {
			t.Fatalf("%s: subsystem not found", name)
		}
		if state.DevicePath != "/dev/nvme3n1" {
			t.Errorf("%s: device = %q", name, state.DevicePath)
		}
		if name == "array" && !state.Live {
			t.Errorf("%s: a live path was not detected", name)
		}
		if name == "object" && state.Live {
			t.Errorf("%s: a connecting path counted as live", name)
		}
	}

	oc := &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, cmd string, args []string, stdin string,
		) (string, string, int, error) {
			return "[]", "", 0, nil
		},
	}
	state, err := NewNvmeHost(oc).ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if state.Found {
		t.Error("an empty listing reported the subsystem as present")
	}
}

// The dm `attr` column is four positions — dmsetup(8): "(L)ive, (I)nactive,
// (s)uspended, (r)ead-only, read-(w)rite". Reading suspended or read-only at
// the wrong offset silently reports every device as resumed and writeable,
// which would defeat the [D12] resume-convergence branch and the read-only
// reload branch alike. The strings below are real captures
// (dnagent_issue_00.md issues 1 and 2).
func TestDmInfoAttrPositions(t *testing.T) {
	for _, tc := range []struct {
		attr      string
		suspended bool
		readOnly  bool
	}{
		{"L--w", false, false}, // live, resumed, writeable
		{"L-sw", true, false},  // the migration source's suspended linear
		{"L--r", false, true},  // the read-only linear nvmet refused
		{"L-sr", true, true},
		{"L", false, false},   // truncated output must not panic
		{"L--", false, false}, // ditto
	} {
		oc := &common.FakeOsClient{
			RunCommandFn: func(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
				return tc.attr + "  \n", "", 0, nil
			},
		}
		dev, err := NewDm(oc).Info(context.Background(), "dnv-x")
		if err != nil {
			t.Fatalf("%q: Info: %v", tc.attr, err)
		}
		if dev.Suspended != tc.suspended || dev.ReadOnly != tc.readOnly {
			t.Errorf("%q -> suspended=%v readOnly=%v, want %v/%v",
				tc.attr, dev.Suspended, dev.ReadOnly,
				tc.suspended, tc.readOnly)
		}
	}
}
