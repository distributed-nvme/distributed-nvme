package agent

import "sync"

// LockSet is the node/object lock hierarchy (dnagent.md §2.6). Lock order is
// always node before object; object locks are created on first use and
// dropped only during teardown while the node write lock is held.
//
// Node write lock: the startup reconcile and the node-level syncup (SH10).
// Node read lock + the object lock: every object-scoped RPC and every
// background converge attempt (SH11). Node read lock alone: node-scoped
// reads (SH12). Object locks are never nested (SH13).
type LockSet struct {
	node sync.RWMutex

	mu   sync.Mutex
	objs map[string]*sync.Mutex
}

func NewLockSet() *LockSet {
	return &LockSet{objs: make(map[string]*sync.Mutex)}
}

// Node returns the node-level lock. Callers take it for writing around
// operations that may tear objects down, for reading around everything else.
func (l *LockSet) Node() *sync.RWMutex {
	return &l.node
}

// Obj returns the lock of one object, creating it on first use.
func (l *LockSet) Obj(key string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	lock, ok := l.objs[key]
	if !ok {
		lock = &sync.Mutex{}
		l.objs[key] = lock
	}
	return lock
}

// DropObj forgets an object's lock. Only legal during teardown, with the
// node write lock held — otherwise a concurrent holder would be dropped.
func (l *LockSet) DropObj(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.objs, key)
}
