package agent

import (
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ResTracker turns probe outcomes into pb.ResInfo values per
// architecture.md §9.5 / dnagent.md SH14: epoch is the unix second of the
// last *status* change — a details-only change does not bump it. The agent
// emits MISSING/ERROR/OK/PROVISIONING and never UNKNOWN (that one is
// worker-only).
//
// One tracker belongs to one synced object (a DN, a side, …). State is
// in-memory: after a restart the epochs restart at the reconcile time, which
// is what "last observed change" means for a freshly started agent.
type ResTracker struct {
	mu    sync.Mutex
	state map[string]*pb.ResInfo
	now   func() int64 // overridable in tests
}

func NewResTracker() *ResTracker {
	return &ResTracker{
		state: make(map[string]*pb.ResInfo),
		now:   func() int64 { return time.Now().Unix() },
	}
}

// Set records an outcome for the resource identified by key (a stable
// logical key, e.g. "dm_linear/{cn_id}") and returns the ResInfo to embed in
// the reply. resName is the user-visible name of the resource (a device
// name, an NQN, a path).
func (t *ResTracker) Set(
	key string,
	resName string,
	status pb.ResStatus,
	details string,
) *pb.ResInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.state[key]
	epoch := uint64(t.now())
	if ok && prev.GetStatus() == status {
		epoch = prev.GetEpoch()
	}
	cur := &pb.ResInfo{
		ResName: resName,
		Status:  status,
		Details: details,
		Epoch:   epoch,
	}
	t.state[key] = cur
	return &pb.ResInfo{
		ResName: cur.ResName,
		Status:  cur.Status,
		Details: cur.Details,
		Epoch:   cur.Epoch,
	}
}

// Ok / Missing / Err / Provisioning are the four outcomes an agent may
// report.
func (t *ResTracker) Ok(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_OK, details)
}

func (t *ResTracker) Missing(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_MISSING, details)
}

func (t *ResTracker) Err(key, resName, details string) *pb.ResInfo {
	return t.Set(key, resName, pb.ResStatus_RES_STATUS_ERROR, details)
}

// Provisioning is the U4 outcome: the resource is deliberately not created
// yet, because the sides underneath it are still being zeroed (§9.4). It
// means healthy / not ready / no action needed, and — unlike ERROR — never
// feeds err_epoch (architecture.md §9.5, §10.2-§10.4, update_01.md U4).
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
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, key)
}
