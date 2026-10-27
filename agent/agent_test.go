package agent

import (
	"context"
	"errors"
	"fmt"
	// Aliased: the fakeSysfs methods below bind `fs` to their receiver.
	iofs "io/fs"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Agent lifecycle (SH1-SH3)
// ---------------------------------------------------------------------------

// SH27 "Background tasks and process exit": the role server tracks its background
// goroutines in a WaitGroup and `agent.Serve` waits for them before returning,
// so **no orphan `blkdiscard` child ever outlives the agent**. `reconcile`
// already starts those goroutines (a dn Reconcile arms one zeroing loop per
// unprovisioned side, each forking a `blkdiscard --zeroout` child), so the
// guarantee has to hold on the *early* return paths too — a `net.Listen`
// failure (a stale unix socket, a duplicate instance, an interface that is not
// up yet at boot) and a `reconcile` failure both return after the children are
// already running. Cancelling `runCtx` alone does not kill them: the
// `exec.CommandContext` watchdog that turns cancellation into SIGTERM/SIGKILL
// (osclient.md, RunCommand; SH15) lives in *this* process and dies with it, so
// the child is reparented to init and keeps `/dev/mapper/{DnSideName}` open —
// EBUSY for the next incarnation's `dmsetup remove`.
//
// The fake background goroutine below is the shape that matters: rooted at the
// ctx handed to `reconcile`, reaped only by cancellation. So a Serve that skips
// the join fails on the closed-channel check, and a Serve that joins *before*
// cancelling deadlocks and fails on the deadline.
func TestServeJoinsBackgroundOnEveryReturnPath(t *testing.T) {
	for _, tc := range []struct {
		name         string
		address      string
		reconcileErr error
	}{
		// An unusable listen address: net.Listen fails after reconcile has
		// already started the zeroing goroutines.
		{"listener failure", "127.0.0.1:999999", nil},
		// reconcile itself fails after starting some of them — convergeSide
		// arms the goroutine before the loop's later error (DN9).
		{"reconcile failure", "127.0.0.1:0", errors.New("reconcile failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var background sync.WaitGroup
			joined := make(chan struct{})
			reconcile := func(runCtx context.Context) error {
				background.Add(1)
				go func() {
					defer background.Done()
					<-runCtx.Done()
				}()
				return tc.reconcileErr
			}
			done := make(chan error, 1)
			go func() {
				done <- Serve(
					context.Background(), "tcp", tc.address, reconcile,
					func(*grpc.Server) {},
					func() {
						background.Wait()
						close(joined)
					})
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Serve reported success on a failure path")
				}
			case <-time.After(10 * time.Second):
				// Waiting before cancelling can never finish: the background
				// goroutine only returns on cancellation.
				t.Fatal("Serve never returned; it joined before cancelling")
			}
			select {
			case <-joined:
			default:
				t.Fatal("Serve returned without joining the background, " +
					"orphaning its children")
			}
		})
	}
}

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

// TestLockSetObjCount pins the count the role packages' tests read around a
// refused call (SH11): it is the number of entries Obj created and DropObj
// has not forgotten, so a call that asks for an object's lock moves it and a
// call that never does leaves it where it was.
func TestLockSetObjCount(t *testing.T) {
	locks := NewLockSet()
	if got := locks.ObjCount(); got != 0 {
		t.Fatalf("a new set counts %d entries", got)
	}
	locks.Obj("a")
	if got := locks.ObjCount(); got != 1 {
		t.Errorf("one entry counts %d", got)
	}
	locks.Obj("a")
	if got := locks.ObjCount(); got != 1 {
		t.Errorf("asking for the same lock again counts %d", got)
	}
	locks.Obj("b")
	if got := locks.ObjCount(); got != 2 {
		t.Errorf("two entries count %d", got)
	}
	locks.DropObj("a")
	if got := locks.ObjCount(); got != 1 {
		t.Errorf("a dropped entry still counts: %d", got)
	}
	locks.DropObj("never-created")
	if got := locks.ObjCount(); got != 1 {
		t.Errorf("dropping an unknown key counts %d", got)
	}
}

// ---------------------------------------------------------------------------
// ResInfo tracking (SH14)
// ---------------------------------------------------------------------------

func TestResTrackerEpoch(t *testing.T) {
	tracker := NewResTracker()
	now := int64(100)
	tracker.hist.now = func() int64 { return now }

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
	changed := tracker.Err("lv", "side-lv", "zeroing 0/4 MiB")
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
	// PROVISIONING is the fourth outcome: healthy, not
	// ready, no action needed. It is an ordinary status change, so it moves
	// the epoch exactly like the other three — what makes it special is that
	// the *worker* never turns it into err_epoch (architecture.md,
	// Live-state reporting).
	now = 500
	provisioning := tracker.Provisioning("p", "side-dev", "zeroing 3/10 MiB")
	if provisioning.GetStatus() !=
		pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Errorf("status = %v, want PROVISIONING", provisioning.GetStatus())
	}
	if provisioning.GetEpoch() != 500 ||
		provisioning.GetDetails() != "zeroing 3/10 MiB" {
		t.Errorf("provisioning info = %+v", provisioning)
	}
	now = 600
	if got := tracker.Provisioning("p", "side-dev", "zeroing 7/10 MiB"); got.
		GetEpoch() != 500 {
		t.Errorf("a progress-only change bumped the epoch to %d",
			got.GetEpoch())
	}
}

// TestResTrackerReason pins the reason beside the epoch rule (architecture.md,
// Live-state reporting): only ErrReason, MissingReason and SetReason name
// one, every other helper gives RES_REASON_NONE, the tracker keeps the reason
// of the latest outcome, and the reason never moves the epoch — only a status
// change does. The returned row is a copy of the whole stored row, the reason
// included.
func TestResTrackerReason(t *testing.T) {
	const lost = pb.ResReason_RES_REASON_THIN_ID_LOST
	const none = pb.ResReason_RES_REASON_NONE
	tracker := NewResTracker()
	now := int64(100)
	tracker.hist.now = func() int64 { return now }

	plain := tracker.Err("thin", "dnv-thin", "dmsetup create: Invalid argument")
	if plain.GetReason() != none || plain.GetEpoch() != 100 {
		t.Fatalf("Err row = %v, want no reason at epoch 100", plain)
	}
	// The reason alone changes: the status stays ERROR, so the epoch stays.
	now = 200
	withReason := tracker.ErrReason("thin", "dnv-thin",
		"dmsetup create: No data available", lost)
	want := &pb.ResInfo{
		ResName: "dnv-thin",
		Status:  pb.ResStatus_RES_STATUS_ERROR,
		Details: "dmsetup create: No data available",
		Epoch:   100,
		Reason:  lost,
	}
	if !proto.Equal(withReason, want) {
		t.Fatalf("ErrReason row = %v, want %v", withReason, want)
	}
	// The tracker stores the whole row, the reason included.
	if stored := tracker.hist.state["thin"]; !proto.Equal(stored, want) {
		t.Errorf("stored row = %v, want %v", stored, want)
	}
	// The returned row is a copy: changing it reaches no stored field.
	withReason.Reason = none
	withReason.Details = "mutated"
	withReason.Epoch = 1
	now = 300
	if again := tracker.ErrReason("thin", "dnv-thin",
		"dmsetup create: No data available", lost); !proto.Equal(again, want) {
		t.Errorf("the stored row was aliased: %v, want %v", again, want)
	}
	// The tracker keeps the latest outcome's reason, not the first one's: a
	// plain Err clears it, still at the same epoch.
	now = 400
	cleared := tracker.Err("thin", "dnv-thin", "dmsetup create: failed")
	if cleared.GetReason() != none || cleared.GetEpoch() != 100 {
		t.Errorf("Err after ErrReason = %v, want no reason at epoch 100",
			cleared)
	}
	// And a reason that comes back keeps the epoch too.
	now = 500
	if back := tracker.ErrReason("thin", "dnv-thin", "x", lost); back.
		GetReason() != lost || back.GetEpoch() != 100 {
		t.Errorf("a reason set again = %v, want it at epoch 100", back)
	}
	// A status change moves the epoch, and the new status has no reason.
	now = 600
	healthy := tracker.Ok("thin", "dnv-thin", "")
	if healthy.GetReason() != none || healthy.GetEpoch() != 600 {
		t.Errorf("OK after ErrReason = %v, want no reason at epoch 600",
			healthy)
	}
	// Back to ERROR with a reason is a status change like any other.
	now = 700
	if again := tracker.ErrReason("thin", "dnv-thin", "y", lost); again.
		GetEpoch() != 700 || again.GetReason() != lost {
		t.Errorf("ERROR again = %v, want the reason at epoch 700", again)
	}
	// SetReason names one on any status, under the same epoch rule: a row
	// that gains it at its status keeps the epoch, and Set clears it.
	const cutover = pb.ResReason_RES_REASON_AWAITING_CUTOVER
	now = 800
	waiting := tracker.Provisioning("dst", "dnv-dst", "waiting")
	now = 900
	if got := tracker.SetReason("dst", "dnv-dst",
		pb.ResStatus_RES_STATUS_PROVISIONING, "waiting for the cutover",
		cutover); got.GetReason() != cutover ||
		got.GetEpoch() != waiting.GetEpoch() {
		t.Errorf("SetReason = %v, want the reason at epoch %d", got,
			waiting.GetEpoch())
	}
	now = 1000
	if got := tracker.Set("dst", "dnv-dst",
		pb.ResStatus_RES_STATUS_PROVISIONING, "waiting"); got.
		GetReason() != none || got.GetEpoch() != waiting.GetEpoch() {
		t.Errorf("Set after SetReason = %v, want no reason at epoch %d",
			got, waiting.GetEpoch())
	}
	// MissingReason names one on a MISSING row under the same rule: a row
	// that gains it while it reads MISSING keeps its epoch, another reason
	// keeps it too, Missing clears it, and the row goes back to the tracker
	// whole.
	const level = pb.ResReason_RES_REASON_SP_LEVEL
	const source = pb.ResReason_RES_REASON_SOURCE_FAULT
	now = 1100
	absent := tracker.Missing("lvl", "dnv-pool", "")
	now = 1200
	suppressed := tracker.MissingReason("lvl", "dnv-pool", "sp_level", level)
	wantSuppressed := &pb.ResInfo{
		ResName: "dnv-pool",
		Status:  pb.ResStatus_RES_STATUS_MISSING,
		Details: "sp_level",
		Epoch:   absent.GetEpoch(),
		Reason:  level,
	}
	if !proto.Equal(suppressed, wantSuppressed) {
		t.Errorf("MissingReason row = %v, want %v", suppressed, wantSuppressed)
	}
	if stored := tracker.hist.state["lvl"]; !proto.Equal(stored,
		wantSuppressed) {
		t.Errorf("stored MissingReason row = %v, want %v", stored,
			wantSuppressed)
	}
	now = 1300
	if got := tracker.MissingReason("lvl", "dnv-pool", "source not connected",
		source); got.GetReason() != source ||
		got.GetEpoch() != absent.GetEpoch() {
		t.Errorf("MissingReason with another reason = %v, want it at "+
			"epoch %d", got, absent.GetEpoch())
	}
	now = 1400
	if got := tracker.Missing("lvl", "dnv-pool", ""); got.GetReason() !=
		none || got.GetEpoch() != absent.GetEpoch() {
		t.Errorf("Missing after MissingReason = %v, want no reason at "+
			"epoch %d", got, absent.GetEpoch())
	}
	// From another status it is a status change like any other.
	now = 1500
	tracker.Ok("lvl", "dnv-pool", "")
	now = 1600
	if got := tracker.MissingReason("lvl", "dnv-pool", "sp_level",
		level); got.GetEpoch() != 1600 || got.GetReason() != level {
		t.Errorf("MISSING again = %v, want the reason at epoch 1600", got)
	}
	// The other helpers give no reason.
	for name, row := range map[string]*pb.ResInfo{
		"Ok":           tracker.Ok("a", "n", ""),
		"Missing":      tracker.Missing("b", "n", ""),
		"Provisioning": tracker.Provisioning("c", "n", ""),
		"FromErr":      tracker.FromErr("d", "n", "", errors.New("e")),
		"Set":          tracker.Set("e", "n", pb.ResStatus_RES_STATUS_PENDING, ""),
	} {
		if row.GetReason() != none {
			t.Errorf("%s gave the reason %v", name, row.GetReason())
		}
	}
}

// TestResTrackerCutPass pins the rule for a pass that is cut off (SH14): a
// row made through a tracker whose bound pass (Bind) or whose view's pass
// (In) is done is returned with the epoch the status rule gives it, and not
// recorded, so the next row of the resource keeps the epoch from before the
// cut. A row made with no pass bound, or while the pass runs, is recorded;
// the func Bind returns puts back the pass bound before, and a view leaves
// the tracker's own pass alone.
func TestResTrackerCutPass(t *testing.T) {
	tracker := NewResTracker()
	now := int64(100)
	tracker.hist.now = func() int64 { return now }
	tracker.Ok("dev", "dnv-dev", "")

	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	cut, cancelCut := context.WithCancel(context.Background())
	cancelCut()

	// The bound pass is cut: the error row is returned at a status change's
	// epoch, and the history keeps the OK row of epoch 100.
	now = 200
	unbind := tracker.Bind(cut)
	if got := tracker.Err("dev", "dnv-dev", "context canceled"); got.
		GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		got.GetEpoch() != 200 {
		t.Errorf("cut row = %v, want ERROR at epoch 200", got)
	}
	// One that keeps the status gets the stored epoch, still unrecorded.
	if got := tracker.Ok("dev", "dnv-dev", "after the cut"); got.
		GetEpoch() != 100 {
		t.Errorf("cut OK row = %v, want epoch 100", got)
	}
	// A new key is not recorded either.
	tracker.Missing("new", "dnv-new", "")
	// Nested: a live pass bound over the cut one records, and its unbind
	// puts the cut one back.
	unbindLive := tracker.Bind(live)
	tracker.Ok("other", "dnv-other", "")
	unbindLive()
	tracker.Err("other", "dnv-other", "context canceled")
	unbind()
	if tracker.pass != nil {
		t.Errorf("unbind left the pass %v bound", tracker.pass)
	}

	now = 300
	if got := tracker.Ok("dev", "dnv-dev", "d").GetEpoch(); got != 100 {
		t.Errorf("epoch after the cut pass = %d, want 100", got)
	}
	// The same status again: a MISSING row the cut pass had recorded would
	// keep its epoch 200.
	if got := tracker.Missing("new", "dnv-new", "").GetEpoch(); got != 300 {
		t.Errorf("a key the cut pass made reads epoch %d, want 300 (never "+
			"recorded)", got)
	}
	if got := tracker.Ok("other", "dnv-other", "").GetEpoch(); got != 200 {
		t.Errorf("the live pass's row reads epoch %d, want 200", got)
	}

	// A view: a cut view records nothing, a live one records, and neither
	// binds the tracker itself.
	now = 400
	if got := tracker.In(cut).Err("dev", "dnv-dev", "x"); got.
		GetEpoch() != 400 {
		t.Errorf("cut view row = %v, want epoch 400", got)
	}
	if tracker.pass != nil {
		t.Errorf("In bound the pass %v on the tracker", tracker.pass)
	}
	now = 450
	if got := tracker.Ok("dev", "dnv-dev", "").GetEpoch(); got != 100 {
		t.Errorf("epoch after a cut view = %d, want 100", got)
	}
	tracker.In(live).Missing("dev", "dnv-dev", "")
	now = 500
	if got := tracker.Missing("dev", "dnv-dev", "").GetEpoch(); got != 450 {
		t.Errorf("epoch after the live view = %d, want 450", got)
	}
	// Drop and Keep follow the desired state and act whatever the pass. Each
	// row below keeps its status, so a row they left would keep its epoch.
	tracker.In(cut).Drop("dev")
	now = 550
	if got := tracker.Missing("dev", "dnv-dev", "").GetEpoch(); got != 550 {
		t.Errorf("epoch after a cut view's Drop = %d, want 550 (the "+
			"dropped row had 450)", got)
	}
	tracker.In(cut).Keep(map[string]struct{}{"dev": {}})
	now = 600
	if got := tracker.Ok("other", "dnv-other", "").GetEpoch(); got != 600 {
		t.Errorf("epoch after a cut view's Keep = %d, want 600 (the "+
			"unwanted row had 200)", got)
	}
	if got := tracker.Missing("dev", "dnv-dev", "").GetEpoch(); got != 550 {
		t.Errorf("a wanted row after a cut view's Keep reads epoch %d, "+
			"want 550", got)
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
	var listed []string
	oc := &common.FakeOsClient{
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			listed = append(listed, path)
			return names, nil
		},
		// No leftover, so no rm either: the listing is the one call.
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			t.Errorf("unexpected command %s %v", name, args)
			return "", "", 127, errors.New("unexpected command")
		},
	}
	store := NewStore(oc, "/store")
	files, err := store.List(context.Background(),
		StoreKindDn, StoreKindSide, StoreKindMigrBm)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0] != "/store" {
		t.Fatalf("listings = %q, want the prefix once", listed)
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

// TestStoreListUnreadablePrefixIsFatal: every failure of the prefix's listing
// is SH3's fatal case, naming the prefix — a missing prefix included, which
// the agent never creates (SH1), and a prefix that is a plain file. None of
// them is an empty store: the startup reload would go on as though the agent
// had accepted nothing.
func TestStoreListUnreadablePrefixIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the listing is cut off at the soft timeout",
			context.DeadlineExceeded},
		{"the prefix is missing", &iofs.PathError{
			Op: "open", Path: "/nope", Err: syscall.ENOENT}},
		{"the prefix is a plain file", &iofs.PathError{
			Op: "open", Path: "/nope", Err: syscall.ENOTDIR}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var listed []string
			oc := &common.FakeOsClient{
				ReadDirFn: func(
					ctx context.Context, path string,
				) ([]string, error) {
					listed = append(listed, path)
					return nil, tc.err
				},
			}
			files, err := NewStore(oc, "/nope").List(
				context.Background(), StoreKindDn)
			if len(listed) != 1 || listed[0] != "/nope" {
				t.Fatalf("listings = %q, want the prefix once", listed)
			}
			if err == nil {
				t.Fatalf("an unreadable store prefix was not reported: %v",
					files)
			}
			if !strings.Contains(err.Error(), "/nope") {
				t.Errorf("error %q does not name the prefix", err)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("error %q does not carry the listing's own %v",
					err, tc.err)
			}
		})
	}
}

// TestStoreListRefusesAPlainFilePrefix runs the listing over a real file and
// the production OsClient: a --local-store that names a plain file is no
// store, and the listing of it fails with ENOTDIR, which is SH3's fatal case
// like any other failure of it, never an empty store.
func TestStoreListRefusesAPlainFilePrefix(t *testing.T) {
	prefix := t.TempDir() + "/store"
	if err := os.WriteFile(prefix, []byte("not a store"), 0o644); err != nil {
		t.Fatalf("planting %s: %v", prefix, err)
	}
	files, err := NewStore(common.NewLimitedOsClient(0), prefix).List(
		context.Background(), StoreKindDn, StoreKindSide)
	if err == nil {
		t.Fatalf("a plain-file prefix listed as a store: %v", files)
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("error %q is not the ENOTDIR of the listing", err)
	}
	if !strings.Contains(err.Error(), prefix) {
		t.Errorf("error %q does not name the prefix", err)
	}
}

// TestStoreCallsCarryTheSoftTimeout: every call the store makes — SH6's
// listing, SH7's `rm` and SH4's proto read and write — reaches the OsClient
// on a ctx carrying the soft timeout (architecture.md, Common validation;
// SH15). The caller owns that deadline and the LimitedOsClient adds none
// (osclient.md, RunCommand), so without the wrap a listing waiting for an
// OsClient slot would hold the startup reconcile, node lock and all, until
// one frees, a wedged `rm` the pass that drops an object, and a save waiting
// for a slot the node lock of the RPC it runs in.
func TestStoreCallsCarryTheSoftTimeout(t *testing.T) {
	var cmds, unbounded []string
	bounded := func(ctx context.Context, name string) {
		cmds = append(cmds, name)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > common.CmdSoftTimeout*time.Second {
			unbounded = append(unbounded, name)
		}
	}
	oc := &common.FakeOsClient{
		ReadProtoFn: func(
			ctx context.Context, path string, target proto.Message,
		) error {
			bounded(ctx, "readproto")
			return nil
		},
		WriteProtoFn: func(
			ctx context.Context, path string, msg proto.Message,
		) error {
			bounded(ctx, "writeproto")
			return nil
		},
		// An interrupted write, so the listing's own rm (SH6) is bounded
		// here too.
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			bounded(ctx, "readdir")
			return []string{"dn-1" + common.AtomicWriteTmpInfix + "7"}, nil
		},
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			bounded(ctx, name)
			return "", "", 0, nil
		},
	}
	store := NewStore(oc, "/store")
	if _, err := store.List(context.Background(), StoreKindDn); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := store.Remove(
		context.Background(), "/store/dn-1", "/store/side-2"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	req := &pb.SyncupDnRequest{}
	if err := store.Save(
		context.Background(), "/store/dn-1", req); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Load(
		context.Background(), "/store/dn-1", req); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(cmds, " "); got !=
		"readdir rm rm writeproto readproto" {
		t.Fatalf("calls = %q, want exactly one listing and its rm, "+
			"the Remove's rm, then the Save and the Load", got)
	}
	if len(unbounded) != 0 {
		t.Errorf("store calls without the SH15 soft timeout: %v", unbounded)
	}
}

// TestStoreListNeverReturnsAnInterruptedWrite runs the store over a real
// directory and the production OsClient. Save replaces a file by writing
// `{name}.tmp-{random}` beside it and renaming that over the name, and a
// failed write removes its own temp file, so what leaves one behind is a save
// that did not succeed — a process that died between the two, say. It sorts
// right after the file it was
// meant to replace, and a reload that decoded it would let it replace the
// committed request: the dn one here decodes to an OLDER request — what a
// leftover holds once later saves have renamed newer ones over the name — and
// the side one is half written and does not decode. The committed file is the
// only truth (SH6), so the listing returns neither, removes both, and the
// committed file still decodes to the request it held. A leftover of a kind
// the caller did not ask for stays: a dn and a cn agent may share one prefix,
// and the other role's temp file may be a write in flight.
func TestStoreListNeverReturnsAnInterruptedWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewStore(common.NewLimitedOsClient(0), dir)
	nf := common.NewNameFmt(dir)
	dnPath := nf.LocalDnPath(1, 3)
	sidePath := nf.LocalSidePath(1, 3, 0x11, 0x16)
	if err := store.Save(ctx, dnPath, &pb.SyncupDnRequest{
		ClusterId: 1, DnId: 3, Revision: 2}); err != nil {
		t.Fatalf("saving the dn request: %v", err)
	}
	if err := store.Save(ctx, sidePath, &pb.SyncupSideRequest{
		ClusterId: 1, DnId: 3, Revision: 2}); err != nil {
		t.Fatalf("saving the side request: %v", err)
	}
	older, err := proto.Marshal(&pb.SyncupDnRequest{
		ClusterId: 1, DnId: 3, Revision: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dnLeftover := dnPath + common.AtomicWriteTmpInfix + "1234567890"
	sideLeftover := sidePath + common.AtomicWriteTmpInfix + "987654321"
	cnLeftover := nf.LocalCnPath(1, 5) + common.AtomicWriteTmpInfix + "42"
	for path, data := range map[string][]byte{
		dnLeftover:   older,
		sideLeftover: {0xff},
		cnLeftover:   {0xff},
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("planting %s: %v", path, err)
		}
	}

	files, err := store.List(ctx, StoreKindDn, StoreKindSide, StoreKindMigrBm)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := files[StoreKindDn]; len(got) != 1 || got[0] != dnPath {
		t.Errorf("dn files = %q, want only the committed %s", got, dnPath)
	}
	if got := files[StoreKindSide]; len(got) != 1 || got[0] != sidePath {
		t.Errorf("side files = %q, want only the committed %s",
			got, sidePath)
	}
	for _, path := range []string{dnLeftover, sideLeftover} {
		if _, err := os.Stat(path); !errors.Is(err, iofs.ErrNotExist) {
			t.Errorf("%s survived the listing (stat: %v)", path, err)
		}
	}
	if _, err := os.Stat(cnLeftover); err != nil {
		t.Errorf("a listing of the dn kinds removed the cn kind's %s: %v",
			cnLeftover, err)
	}
	stored := &pb.SyncupDnRequest{}
	if err := store.Load(ctx, dnPath, stored); err != nil {
		t.Fatalf("loading the committed dn request: %v", err)
	}
	if got := stored.GetRevision(); got != 2 {
		t.Errorf("the committed dn request decodes at revision %d, want 2",
			got)
	}
}

// TestStoreListRetriesAFailedLeftoverRemoval: an interrupted write whose rm
// fails does not fail the listing — a file the reload never reads must not
// stop the reload — and is still not returned. Nothing remembers the failure:
// the next listing, the next startup's, finds the file again and issues the
// same rm again.
func TestStoreListRetriesAFailedLeftoverRemoval(t *testing.T) {
	committed := "dn-0000000000000001-0000000000000003"
	leftover := committed + common.AtomicWriteTmpInfix + "1234567890"
	var rms []string
	oc := &common.FakeOsClient{
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			return []string{committed, leftover}, nil
		},
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			if name == "rm" {
				rms = append(rms, strings.Join(args, " "))
				return "", "rm: cannot remove: Read-only file system", 1,
					errors.New("exit status 1")
			}
			t.Errorf("unexpected command %s %v", name, args)
			return "", "", 127, errors.New("unexpected command")
		},
	}
	store := NewStore(oc, "/store")
	for listing := 1; listing <= 2; listing++ {
		files, err := store.List(context.Background(), StoreKindDn)
		if err != nil {
			t.Fatalf("listing %d failed over a leftover it could not "+
				"remove: %v", listing, err)
		}
		if got := files[StoreKindDn]; len(got) != 1 ||
			got[0] != "/store/"+committed {
			t.Errorf("listing %d: dn files = %q, want only the committed "+
				"file", listing, got)
		}
		if len(rms) != listing {
			t.Fatalf("after listing %d: %d rm calls, want %d",
				listing, len(rms), listing)
		}
		if want := "-f /store/" + leftover; rms[listing-1] != want {
			t.Errorf("listing %d: rm %s, want rm %s",
				listing, rms[listing-1], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Bitmap chunk store and skip-range math (SH21-SH23; architecture.md,
// raid0 bitmap math)
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

// TestCloneChunkSetPairKeyed: clone chunks are addressed by the pair, so the
// same bm_idx on two source slices is two distinct chunks, and Ids() orders
// them ascending (SliceIdx, BmIdx) — the deterministic applied set of SH21.
func TestCloneChunkSetPairKeyed(t *testing.T) {
	chunks := NewCloneChunkSet()
	chunks.Put(CloneChunkKey{SliceIdx: 1, BmIdx: 0}, []byte{0x10})
	chunks.Put(CloneChunkKey{SliceIdx: 0, BmIdx: 2}, []byte{0x02})
	chunks.Put(CloneChunkKey{SliceIdx: 0, BmIdx: 0}, []byte{0x01})

	if got := chunks.Len(); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	// Same bm_idx, different slice: two chunks, neither shadowing the other.
	for _, want := range []struct {
		key  CloneChunkKey
		byte byte
	}{
		{CloneChunkKey{SliceIdx: 0, BmIdx: 0}, 0x01},
		{CloneChunkKey{SliceIdx: 1, BmIdx: 0}, 0x10},
		{CloneChunkKey{SliceIdx: 0, BmIdx: 2}, 0x02},
	} {
		got, ok := chunks.Get(want.key)
		if !ok || len(got) != 1 || got[0] != want.byte {
			t.Fatalf("chunk %v = %v/%v, want [%#x]", want.key, got, ok,
				want.byte)
		}
	}
	// An absent pair is absent — a gap is legal, not an error.
	if _, ok := chunks.Get(CloneChunkKey{SliceIdx: 0, BmIdx: 1}); ok {
		t.Fatal("a chunk that was never put reads as present")
	}

	want := []CloneChunkKey{{0, 0}, {0, 2}, {1, 0}}
	got := chunks.Ids()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}

	chunks.Delete(CloneChunkKey{SliceIdx: 0, BmIdx: 2})
	if _, ok := chunks.Get(CloneChunkKey{SliceIdx: 0, BmIdx: 2}); ok {
		t.Fatal("the deleted chunk survived")
	}
	if got := chunks.Ids(); len(got) != 2 ||
		got[0] != (CloneChunkKey{0, 0}) || got[1] != (CloneChunkKey{1, 0}) {
		t.Fatalf("ids after the delete = %v", got)
	}
	// Deleting a pair never touches the same bm_idx on another slice.
	if _, ok := chunks.Get(CloneChunkKey{SliceIdx: 1, BmIdx: 0}); !ok {
		t.Fatal("deleting (0,2) lost (1,0)")
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
	// Builder-level coverage of the single-feature rendering: the derived
	// `<#feature args>` count must still come out as 1. No dnv call site
	// passes this combination — both dm-clones pass noDiscardPassdown = true
	// (the case below).
	got := CloneTable(
		2048, "253:1", "253:2", "259:0", 2048, true, false, 1, 2)
	want := "0 2048 clone 253:1 253:2 259:0 2048 1 no_hydration 4 " +
		"hydration_threshold 1 hydration_batch_size 2"
	if got != want {
		t.Errorf("clone table = %q, want %q", got, want)
	}
	if got := CloneTable(
		2048, "253:1", "253:2", "259:0", 2048, false, false, 0, 0); got !=
		"0 2048 clone 253:1 253:2 259:0 2048 0 0" {
		t.Errorf("bare clone table = %q", got)
	}
	// The dnv form, used by both role packages:
	// `blkdiscard` must stay a metadata-only "mark hydrated" primitive, so
	// every dnv dm-clone disables discard passdown (cnagent.md CN18 step 3).
	if got := CloneTable(
		2048, "253:1", "253:2", "259:0", 2048, true, true, 1, 1); got !=
		"0 2048 clone 253:1 253:2 259:0 2048 2 no_hydration "+
			"no_discard_passdown 4 hydration_threshold 1 "+
			"hydration_batch_size 1" {
		t.Errorf("cn clone table = %q", got)
	}
}

// TestCloneStatusDetails pins the details text both agents give a dm-clone
// row whose pass read the status: the note first, then the raw status line
// on a line of its own, which a reader that scans the details line by line,
// as the gateway's hydration checks do, still finds whole.
func TestCloneStatusDetails(t *testing.T) {
	const raw = "0 20480 clone 8 1/1024 2048 10/10 0 1 no_hydration 0"
	const note = "hydration not enabled"
	for _, tc := range []struct {
		note, raw, want string
	}{
		{note, raw, note + "\n" + raw},
		{"", raw, raw},
		{note, "", note},
		{"", "", ""},
	} {
		if got := CloneStatusDetails(tc.note, tc.raw); got != tc.want {
			t.Errorf("CloneStatusDetails(%q, %q) = %q, want %q",
				tc.note, tc.raw, got, tc.want)
		}
	}
	details := CloneStatusDetails(note, raw)
	lines := strings.Split(details, "\n")
	status, ok := common.ParseCloneStatus(lines[len(lines)-1])
	if !ok || status.HydratedRegions != 10 || status.TotalRegions != 10 {
		t.Errorf("the status line did not survive the note: %+v", status)
	}
	// A reader that parses the whole details, note and all, still finds
	// the one dm-clone line in it.
	whole, ok := common.ParseCloneStatus(details)
	if !ok || whole.HydratedRegions != 10 || whole.TotalRegions != 10 ||
		whole.HydrationEnabled {
		t.Errorf("the whole details did not parse: %+v", whole)
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
// ListSubsys reads sysfs (SH20)
// ---------------------------------------------------------------------------

// fakeSysfs is a tiny read-only tree: ReadDir lists the direct children of a
// registered directory in byte order, as the production client does, and
// `ls -1 <dir>`, which the nvmet configfs tree alone is listed with (SH18),
// lists them in the order they were registered; ReadFile serves a registered
// file. It also pins the rule that every read and listing of the SH20 walk
// must arrive with an SH15 deadline on its ctx, so a stalled /sys/class/nvme*
// read can never hold a converge — and through the node lock, a whole node's
// RPC surface — open.
type fakeSysfs struct {
	dirs  map[string][]string
	files map[string]string
	// killLs makes a listing of a directory answer the way one the SH15
	// soft timeout cut off does: `ls -1 <dir>` exits -1 with a non-nil error,
	// as a killed child does, instead of the exit status a tool that ran and
	// looked gives, and ReadDir fails with context.DeadlineExceeded instead
	// of the not-exist error of a missing directory. No arrangement of the
	// tree's contents can express that outcome, and telling it apart from an
	// absence is exactly what the enumerators of a sweep turn on.
	killLs map[string]bool
	// readErr answers one path with an error that is NOT fs.ErrNotExist: an
	// attribute that could not be read at all, as opposed to one that is not
	// there. It must never reach a caller as an absence.
	readErr map[string]error
	// read is every path ReadFile served, and noDeadline every path read or
	// listed on a ctx carrying no deadline.
	read       []string
	noDeadline []string
	// readDirCalls is every path ReadDir is asked to list, so a test that
	// expects a listing to fail, or its directory to be missing, can check
	// that the listing reached this tree: an OsClient that never reached it
	// answers an empty listing (common.FakeOsClient).
	readDirCalls []string
}

func (fs *fakeSysfs) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, cmd string, args []string, stdin string,
		) (string, string, int, error) {
			if cmd != "ls" {
				return "", "", 127, fmt.Errorf("unexpected command %q", cmd)
			}
			path := args[len(args)-1]
			if fs.killLs[path] {
				return "", "signal: killed", -1, errors.New("signal: killed")
			}
			entries, ok := fs.dirs[path]
			if !ok {
				return "", "", 2, fmt.Errorf("no such directory")
			}
			return strings.Join(entries, "\n") + "\n", "", 0, nil
		},
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			fs.readDirCalls = append(fs.readDirCalls, path)
			if _, ok := ctx.Deadline(); !ok {
				fs.noDeadline = append(fs.noDeadline, path)
			}
			if fs.killLs[path] {
				return nil, context.DeadlineExceeded
			}
			entries, ok := fs.dirs[path]
			if !ok {
				// A missing directory wraps fs.ErrNotExist, as the
				// production client's does: the one thing a caller tells
				// "absent" from "did not answer" by.
				return nil, fmt.Errorf("no such directory: %s: %w", path,
					iofs.ErrNotExist)
			}
			names := append([]string(nil), entries...)
			sort.Strings(names)
			return names, nil
		},
		ReadFileFn: func(ctx context.Context, path string) (string, error) {
			fs.read = append(fs.read, path)
			if _, ok := ctx.Deadline(); !ok {
				fs.noDeadline = append(fs.noDeadline, path)
			}
			if err, ok := fs.readErr[path]; ok {
				return "", err
			}
			data, ok := fs.files[path]
			if !ok {
				// An absent file must wrap fs.ErrNotExist, because that is
				// the one thing agent.readAttrStrict tests: the production
				// LimitedOsClient hands back os.ReadFile's *fs.PathError, and
				// "absent" is distinguished from "the read did not answer"
				// by errors.Is(err, fs.ErrNotExist) alone. A bare
				// fmt.Errorf here would make every absent attribute look
				// like a stalled sysfs read.
				return "", fmt.Errorf("no such file: %s: %w", path,
					iofs.ErrNotExist)
			}
			return data, nil
		},
	}
}

// The namespace device, the per-path ana_state and the controller state all
// come from sysfs: `nvme list-subsys -o json` carries none of the three
// (nvme-cli 2.16 lists no namespaces at all, and no ANAState without a
// namespace device argument), which is why ListSubsys never runs it.
func TestListSubsysReadsSysfs(t *testing.T) {
	const nqn = "nqn.2024-01.io.dnv:3:a:b:c"
	fs := &fakeSysfs{
		dirs: map[string][]string{
			"/sys/class/nvme-subsystem":              {"nvme-subsys0"},
			"/sys/class/nvme-subsystem/nvme-subsys0": {"nvme3", "nvme3n1", "subsysnqn"},
			"/sys/class/nvme/nvme3":                  {"nvme3c3n1", "state", "address"},
		},
		files: map[string]string{
			"/sys/class/nvme-subsystem/nvme-subsys0/subsysnqn": nqn + "\n",
			"/sys/class/nvme/nvme3/state":                      "live\n",
			"/sys/class/nvme/nvme3/transport":                  "tcp\n",
			"/sys/class/nvme/nvme3/address":                    "traddr=10.0.0.1,trsvcid=4420\n",
			"/sys/class/nvme/nvme3/nvme3c3n1/ana_state":        "non-optimized\n",
		},
	}
	state, err := NewNvmeHost(fs.osClient()).ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if !state.Found {
		t.Fatal("subsystem not found")
	}
	if state.DevicePath != "/dev/nvme3n1" {
		t.Errorf("device = %q, want /dev/nvme3n1", state.DevicePath)
	}
	if !state.Live {
		t.Error("a live controller was not detected")
	}
	if len(state.Paths) != 1 {
		t.Fatalf("paths = %v", state.Paths)
	}
	path := state.Paths[0]
	if path.Name != "nvme3" || path.TrAddr != "10.0.0.1" ||
		path.TrSvcId != "4420" || path.Transport != "tcp" {
		t.Errorf("path transport identity = %+v", path)
	}
	if path.AnaState != "non-optimized" {
		t.Errorf("ana_state = %q", path.AnaState)
	}

	// A connecting controller is found but not live: the migration
	// destination must keep waiting rather than build a dm-clone on it.
	fs.files["/sys/class/nvme/nvme3/state"] = "connecting\n"
	state, err = NewNvmeHost(fs.osClient()).ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if !state.Found || state.Live {
		t.Errorf("connecting path: found=%v live=%v", state.Found, state.Live)
	}

	// No nvme subsystem at all is "not connected", never an error: the tree
	// does not exist until the host holds its first fabrics controller.
	empty := &fakeSysfs{dirs: map[string][]string{}, files: map[string]string{}}
	state, err = NewNvmeHost(empty.osClient()).ListSubsys(
		context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if state.Found {
		t.Error("an empty sysfs reported the subsystem as present")
	}
	if !slices.Contains(empty.readDirCalls, sysfsNvmeSubsysDir) {
		t.Errorf("the walk never listed %s: its absence was not read",
			sysfsNvmeSubsysDir)
	}

	// Every sysfs read and listing of the walk is SH15-bounded, exactly
	// like every command and every configfs attribute: readTrimmed bounds
	// each read and readDir each listing, and an unbounded one fails here.
	// The per-suffix and per-directory guards keep the assertion from going
	// vacuous if a later fixture stops exercising one of them.
	for _, suffix := range []string{
		"/subsysnqn", "/address", "/transport", "/state", "/ana_state"} {
		if !anySuffix(fs.read, suffix) {
			t.Fatalf("no sysfs read of %s: the deadline check is vacuous",
				suffix)
		}
	}
	for _, dir := range []string{
		"/sys/class/nvme-subsystem",
		"/sys/class/nvme-subsystem/nvme-subsys0",
		"/sys/class/nvme/nvme3",
	} {
		if !slices.Contains(fs.readDirCalls, dir) {
			t.Fatalf("no listing of %s: the deadline check is vacuous", dir)
		}
	}
	if len(fs.noDeadline) != 0 {
		t.Errorf("sysfs reads or listings without an SH15 deadline: %v",
			fs.noDeadline)
	}
}

// TestHasCtrlSkipsADeletedController pins HasCtrl over the walk: a subsystem
// keeps listing a controller whose device the kernel has deleted until the
// last reference to it drops, with nothing of it left to read (cnagent.md
// CN10). The walk lists it in Paths with only its name, and it is no
// controller; one whose state reads beside it is, connecting or not.
func TestHasCtrlSkipsADeletedController(t *testing.T) {
	const nqn = "nqn.2024-01.io.dnv:4:a:b:c"
	const subsysDir = "/sys/class/nvme-subsystem/nvme-subsys0"
	fs := &fakeSysfs{
		dirs: map[string][]string{
			"/sys/class/nvme-subsystem": {"nvme-subsys0"},
			subsysDir:                   {"nvme3", "subsysnqn"},
		},
		files: map[string]string{
			subsysDir + "/subsysnqn": nqn + "\n",
		},
	}
	host := NewNvmeHost(fs.osClient())
	state, err := host.ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if !state.Found || len(state.Paths) != 1 ||
		state.Paths[0] != (PathState{Name: "nvme3"}) {
		t.Fatalf("want the deleted controller listed by name alone, "+
			"got %+v", state)
	}
	if state.HasCtrl() {
		t.Error("a deleted controller counts as a controller")
	}

	fs.dirs[subsysDir] = []string{"nvme3", "nvme4", "subsysnqn"}
	fs.dirs["/sys/class/nvme/nvme4"] = []string{"address", "state"}
	fs.files["/sys/class/nvme/nvme4/state"] = "connecting\n"
	fs.files["/sys/class/nvme/nvme4/address"] =
		"traddr=10.0.0.1,trsvcid=4420\n"
	state, err = host.ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if !state.HasCtrl() {
		t.Errorf("a connecting controller beside a deleted one counts as "+
			"none: %+v", state.Paths)
	}

	var none *SubsysState
	if none.HasCtrl() || (&SubsysState{Found: true}).HasCtrl() {
		t.Error("a nil state, or an entry with no controller, has one")
	}
}

func anySuffix(paths []string, suffix string) bool {
	for _, path := range paths {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// The dm `attr` column is four positions — dmsetup(8): "(L)ive, (I)nactive,
// (s)uspended, (r)ead-only, read-(w)rite". Reading suspended or read-only at
// the wrong offset silently reports every device as resumed and writeable,
// which would defeat the [D12] resume-convergence branch and the read-only
// reload branch alike. The first four strings below are attr columns as
// `dmsetup info` prints them; the last two are truncated on purpose.
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
