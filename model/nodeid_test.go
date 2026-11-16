package model

import (
	"fmt"
	"testing"
)

// TestNodeErrEpochOfAnotherId: a node health write whose id is not the
// record's — another node now at the address — refuses as for no record and
// writes nothing (dnv-worker.md HL1).
func TestNodeErrEpochOfAnotherId(t *testing.T) {
	env := newOpsEnv(t)
	store := env.storeRev()
	err := SetDnErrEpoch(env.ctx, env.cli, env.cid, opsDnA,
		env.dn(opsDnA).GetDnId()+1, 1000, env.cc)
	wantPrecondition(t, err, opSetDnErrEpoch)
	err = SetCnErrEpoch(env.ctx, env.cli, env.cid, opsCnA,
		env.cn(opsCnA).GetCnId()+1, 1000)
	wantPrecondition(t, err, opSetCnErrEpoch)
	if env.storeRev() != store {
		t.Fatalf("a write for another node's id wrote")
	}
}

// TestDrainRefusalsNameTheLostKey: the drain's first step refuses a pool
// whose cntlr or slice key is lost with a reason that names the lost id, so
// that the operator puts the right key back (dnv-worker.md SPD12).
func TestDrainRefusalsNameTheLostKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    func(env *opsEnv) string
		reason string
	}{
		{"a cntlr", func(env *opsEnv) string {
			return CntlrKey(env.cid, opsSpId, opsCntlrB)
		}, fmt.Sprintf("cntlr %d not found", opsCntlrB)},
		{"a slice", func(env *opsEnv) string {
			return SliceKey(env.cid, opsSpId, opsSliceId)
		}, fmt.Sprintf("slice %d not found", opsSliceId)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.setDeleting()
			env.delKey(tc.key(env))
			_, err := DrainSpCntlrs(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName)
			if pre := wantPrecondition(t, err, opDrainSpCntlrs); pre.Reason !=
				tc.reason {
				t.Fatalf("reason %q, want %q", pre.Reason, tc.reason)
			}
		})
	}
}
