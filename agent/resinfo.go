package agent

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ResTracker turns probe outcomes into pb.ResInfo values per
// architecture.md, Live-state reporting / dnagent.md SH14: epoch is the unix
// second of the last *status* change — a change of the details or of the
// reason alone does not bump it. The reason is RES_REASON_NONE unless the
// caller names one through ErrReason or SetReason.
//
// The agent emits MISSING/ERROR/OK/PROVISIONING, the cn agent also PENDING on a
// primary's leg rows (cnagent.md CN11), and never UNKNOWN (that one is
// worker-only).
//
// One tracker belongs to one synced object (a DN, a side, …). State is
// in-memory: after a restart the epochs restart at the reconcile time, which
// is what "last observed change" means for a freshly started agent.
//
// A pass that writes the object's rows — a converge or a background read —
// writes them through the tracker with its ctx bound (Bind) or through a view
// of its own (In). Once that ctx is done the pass is cut off: a row it makes
// then is returned, with the epoch the status rule gives it, but not
// recorded. Such a pass stores no observation (SH28, SH29): every command
// after the cut fails at once, so its rows tell of the cut and not of the
// node. Recorded, they would move the epoch of each row the cut turned to an
// error, the next pass would move it back, and every reply after would give
// that pass as the row's last status change, where the node changed nothing
// (architecture.md, Live-state reporting); each syncup that cancels a running
// read (SH30) would move them so. A row made with no pass bound — a unit test
// that calls a probe itself — is recorded.
type ResTracker struct {
	hist *resHistory
	// pass is the ctx of the converge or read that writes through this
	// tracker, nil when none is bound. On an object's own tracker a pass sets
	// it (Bind) and every write reads it under the one lock that serializes
	// every pass that writes the object's rows; on a view from In it never
	// changes.
	pass context.Context
}

// resHistory is the rows of one object as its tracker and every view of it
// (In) record them.
type resHistory struct {
	mu    sync.Mutex
	state map[string]*pb.ResInfo
	now   func() int64 // overridable in tests
}

func NewResTracker() *ResTracker {
	return &ResTracker{hist: &resHistory{
		state: make(map[string]*pb.ResInfo),
		now:   func() int64 { return time.Now().Unix() },
	}}
}

// Bind makes ctx the pass that writes through t until the func it returns
// runs, which puts back the pass bound before. The caller holds the lock that
// serializes every pass that writes the object's rows — a side's or a
// cntlr's object lock, or the node write lock — and holds it until that func
// has run, so the helpers its passes share write through the object's own
// tracker. An object whose passes can overlap has its passes write through In
// instead.
func (t *ResTracker) Bind(ctx context.Context) func() {
	prev := t.pass
	t.pass = ctx
	return func() { t.pass = prev }
}

// In is a view of t's history that the pass under ctx writes through, for an
// object whose passes can overlap: a DN's or a CN's background read holds
// only the node read lock, which the object's next read may share before this
// one has returned. Every pass of such an object writes through a view of its
// own, its converges too, so no write reads a pass that another may bind.
func (t *ResTracker) In(ctx context.Context) *ResTracker {
	return &ResTracker{hist: t.hist, pass: ctx}
}

// cut reports whether the pass that writes through t has been cut off.
func (t *ResTracker) cut() bool {
	return t.pass != nil && t.pass.Err() != nil
}

// Set records an outcome for the resource identified by key (a stable
// logical key, e.g. "dm_linear/{cn_id}") and returns the ResInfo to embed in
// the reply. resName is the user-visible name of the resource (a device
// name, an NQN, a path). The row's reason is RES_REASON_NONE. A pass that is
// cut off records nothing (ResTracker).
func (t *ResTracker) Set(
	key string,
	resName string,
	status pb.ResStatus,
	details string,
) *pb.ResInfo {
	return t.SetReason(key, resName, status, details,
		pb.ResReason_RES_REASON_NONE)
}

// SetReason is Set with a reason, for a row whose state a control-plane
// decision reads in place of its details (architecture.md, Live-state
// reporting): the dn agent's migration destination rows while they wait for
// the cutover (dnagent.md DN18). The epoch moves only when the status
// changes, whatever the details and the reason do, and the caller gets a
// copy of the whole stored row, every field of it, so no caller can reach
// the state. A pass that is cut off gets the row and records nothing
// (ResTracker).
func (t *ResTracker) SetReason(
	key string,
	resName string,
	status pb.ResStatus,
	details string,
	reason pb.ResReason,
) *pb.ResInfo {
	h := t.hist
	h.mu.Lock()
	defer h.mu.Unlock()
	prev, ok := h.state[key]
	epoch := uint64(h.now())
	if ok && prev.GetStatus() == status {
		epoch = prev.GetEpoch()
	}
	cur := &pb.ResInfo{
		ResName: resName,
		Status:  status,
		Details: details,
		Epoch:   epoch,
		Reason:  reason,
	}
	if t.cut() {
		return cur
	}
	h.state[key] = cur
	return proto.Clone(cur).(*pb.ResInfo)
}

// Ok / Missing / Err / Provisioning are the four outcomes every agent may
// report. The fifth, RES_STATUS_PENDING, has no helper: only the cn agent's
// primary leg report produces it, through Set (cnagent.md CN11).
func (t *ResTracker) Ok(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_OK, details)
}

func (t *ResTracker) Missing(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_MISSING, details)
}

func (t *ResTracker) Err(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_ERROR, details)
}

// ErrReason is Err with a reason, for a row whose cause a control-plane
// decision reads in place of its details (architecture.md, Live-state
// reporting); every helper but it and SetReason gives RES_REASON_NONE. A row
// that keeps its status keeps its epoch when only its reason changes.
func (t *ResTracker) ErrReason(
	key, resName, details string,
	reason pb.ResReason,
) *pb.ResInfo {
	return t.SetReason(
		key, resName, pb.ResStatus_RES_STATUS_ERROR, details, reason)
}

// Provisioning is the [D15] outcome (architecture.md, Live-state reporting):
// the resource is deliberately not created yet while a side it depends on
// provisions (architecture.md, Side provisioning protocol); or it exists and
// is being prepared — the dn's side device while it zeroes, and on the cn a
// deferred td's ns-devs and nvmet namespaces and a deferred transfer's
// device, subsystem and namespace, all backed by an error table (cnagent.md
// CN16, CN17); or, on the dn, it sits above a side device whose provisioning
// gate is closed and is reported unprobed, whether or not it exists
// (dnagent.md DN9, DN18); or it waits for a free run in its node's metadata
// area — on the cn a clone's metadata wrapper and dm-clone (cnagent.md CN18),
// on the dn a migration destination's side device and migration rows
// (dnagent.md DN13) — with the details "waiting for metadata room: " and the
// allocator's own text (architecture.md, Metadata units); or, on the dn, a
// migration's rows wait for its cutover, the src side's migr_src_info rows
// and the dst side's two migr_dst_info rows, which carry a reason as well
// (SetReason; dnagent.md DN12, DN13, DN18). It means healthy /
// not ready / no action needed, and — unlike ERROR — never feeds err_epoch
// (architecture.md, Live-state reporting, dn / cn roles, sp role and
// Automatic reactions).
func (t *ResTracker) Provisioning(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_PROVISIONING, details)
}

// FromErr is the common shape of DN19 error capture: err == nil ⇒ OK with
// details, otherwise ERROR carrying the command output / error text.
func (t *ResTracker) FromErr(
	key, resName, details string,
	err error,
) *pb.ResInfo {
	if err != nil {
		return t.Err(key, resName, err.Error())
	}
	return t.Ok(key, resName, details)
}

// Drop forgets a resource's history (used when a resource leaves the desired
// state, so a later re-creation reports a fresh epoch).
func (t *ResTracker) Drop(key string) {
	h := t.hist
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, key)
}

// Keep forgets every history whose key the desired state no longer names. It
// is Drop derived the same way a sweep derives a removal — from the wanted
// set, not from a diff against a remembered plan — so an object that left the
// desired state cannot leave its epoch behind to be read as a new object's on
// a later rebuild (SH14).
func (t *ResTracker) Keep(wanted map[string]struct{}) {
	h := t.hist
	h.mu.Lock()
	defer h.mu.Unlock()
	for key := range h.state {
		if _, ok := wanted[key]; !ok {
			delete(h.state, key)
		}
	}
}
