package gateway

import (
	"bytes"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The gateway half of the sp drain (gateway.md §5.4, dnv-worker.md §11.6):
// DeleteStoragePool LATCHES and returns, and everything that used to be its
// transaction is now the worker's.
//
// The teardown assertions of §8.4 did not move with it — they are still the
// only proof that a delete returns every extent it charged — so the tests that
// own them run the drain here, through sptDrain, exactly as the sp coordinator
// would. That is the same stand-in the §14 suite makes with `wctl
// set-provisioned` and `wctl set-created`: a worker-role write driven from a
// gateway test, so that the gateway's end state can be asserted without a
// worker process.

// sptDrainSteps bounds sptDrain so a drain that cannot progress fails the test
// instead of spinning. The largest shape these fixtures build is 2 slices x 2
// groups, i.e. 1 (D1) + 2 (one batch per slice) + 1 (D3) = 4 steps;
// common.MaxDelGrpPerTxn is 20, so the bound is slack by an order of magnitude.
const sptDrainSteps = 64

// sptDrain runs the drain of one latched SP to completion.
//
// It is SPD8's derivation in a loop, with no pass and no timer: read the
// SpConf, take the first matching phase, commit it, repeat until the SpConf is
// gone. Every step is one of the three model ops the worker calls, so what this
// asserts about the END state is what the worker produces.
//
// A step that fails is fatal rather than retried: in a unit test there is no
// "the world heals later" (SPD6), and a drain that cannot make progress is
// exactly the bug these tests exist to catch.
func sptDrain(env *sptEnv, spName string) {
	env.t.Helper()
	for step := 0; step < sptDrainSteps; step++ {
		key := model.SpConfKey(env.cid, spName)
		if !env.exists(key) {
			// D3 committed: the SpConf, the name key, the rev key and the
			// bucket slot are all gone.
			return
		}
		conf := &pb.SpConf{}
		env.get(key, conf)
		if !conf.GetDeleting() {
			env.t.Fatalf("sptDrain: %q is not latched", spName)
		}
		shard, spId := conf.GetShardCode(), conf.GetSpId()
		var err error
		switch {
		case len(conf.GetCntlrIdList()) > 0:
			_, err = model.DrainSpCntlrs(
				env.ctx, env.cli, env.cid, shard, spId, spName)
		case len(conf.GetSliceIdList()) > 0:
			_, _, err = model.DrainSpSlice(
				env.ctx, env.cli, env.cid, shard, spId, spName,
				sptLowestId(conf.GetSliceIdList()), env.cc)
		default:
			err = model.FinishSpDelete(
				env.ctx, env.cli, env.cid, shard, spId, spName)
		}
		if err != nil {
			env.t.Fatalf("sptDrain %q step %d: %v", spName, step, err)
		}
	}
	env.t.Fatalf("sptDrain: %q did not finish in %d steps",
		spName, sptDrainSteps)
}

// sptLowestId is SPD10's target rule: the LOWEST listed slice id, so that two
// overlapping drivers choose the same one however the list is stored.
func sptLowestId(ids []uint64) uint64 {
	lowest := ids[0]
	for _, id := range ids[1:] {
		if id < lowest {
			lowest = id
		}
	}
	return lowest
}

// sptDelete latches one SP with the token the SP currently carries and returns
// the reply's sp_id.
func sptDelete(env *sptEnv, spName string, rev uint64) uint64 {
	env.t.Helper()
	reply, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      spName,
			SpRev:       &pb.SpRev{Revision: rev},
		})
	if err != nil {
		env.t.Fatalf("DeleteStoragePool %q: %v", spName, err)
	}
	return reply.GetSpId()
}

// sptChangedKeys is the set of keys whose stored bytes differ between two
// dumps, plus every key one dump has and the other does not.
func sptChangedKeys(before map[string][]byte, after map[string][]byte) []string {
	var changed []string
	for key, want := range before {
		got, ok := after[key]
		if !ok || !bytes.Equal(want, got) {
			changed = append(changed, key)
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			changed = append(changed, key)
		}
	}
	return changed
}

// TestDeleteStoragePoolLatchesOnly is §8.4's whole write set: SpConf with
// `deleting = true` and one SpRev bump, and NOTHING else.
//
// It is asserted as a key-set difference rather than as a list of things that
// are still there, because the failure this guards against is a leftover half
// of the one-shot teardown — a cntlr key deleted, a DN credited, a bucket slot
// released — and only a whole-cluster diff sees all of those at once.
func TestDeleteStoragePoolLatchesOnly(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	before := env.dump()

	if got := sptDelete(env, sptSpName, 1); got != spId {
		t.Errorf("sp_id: got %d, want %d", got, spId)
	}
	after := env.dump()

	want := map[string]bool{
		model.SpConfKey(env.cid, sptSpName):                true,
		model.SpRevKey(conf.GetShardCode(), env.cid, spId): true,
	}
	for _, key := range sptChangedKeys(before, after) {
		if !want[key] {
			t.Errorf("the latch wrote %q; it may write only SpConf and SpRev",
				key)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("the latch did not write %q", key)
	}
	if !env.spConf(sptSpName).GetDeleting() {
		t.Errorf("deleting must be true after the latch")
	}
	if got := env.spRev(conf.GetShardCode(), spId); got != 2 {
		t.Errorf("sp_rev: got %d, want 2 (exactly one bump)", got)
	}
	// Everything the drain will remove is still there, which is the property
	// `sp get` shows an operator as progress.
	for _, cntlrId := range conf.GetCntlrIdList() {
		env.cntlr(spId, cntlrId)
	}
	for _, sliceId := range conf.GetSliceIdList() {
		env.slice(spId, sliceId)
	}
}

// TestDeleteStoragePoolRepeatIsANoOp is SPD3, pinned in BOTH directions: the
// first delete MUST bump and the repeat MUST NOT.
//
// One direction alone passes a broken implementation. A latch that bumped on
// every call would pass an assertion that only checked the first bump, and one
// that never bumped at all would pass an assertion that only checked the
// repeat; the pair is what makes either failure visible. The whole-store diff
// on the repeat is the third witness — a no-op must write nothing anywhere,
// not merely leave the revision alone.
func TestDeleteStoragePoolRepeatIsANoOp(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptSmallSpec(sptSpName))
	shard := env.spConf(sptSpName).GetShardCode()

	sptDelete(env, sptSpName, 1)
	if got := env.spRev(shard, spId); got != 2 {
		t.Fatalf("the first delete must bump: sp_rev %d, want 2", got)
	}
	before := env.dump()
	// The token is the one the latch produced: a repeat delete is an ordinary
	// request that passes GW6 and then finds the flag already set.
	if got := sptDelete(env, sptSpName, 2); got != spId {
		t.Errorf("sp_id: got %d, want %d", got, spId)
	}
	if got := env.spRev(shard, spId); got != 2 {
		t.Errorf("a repeat delete bumped sp_rev to %d, want 2", got)
	}
	if changed := sptChangedKeys(before, env.dump()); len(changed) != 0 {
		t.Errorf("a repeat delete wrote %v", changed)
	}
	// A repeat delete with no token at all is the same no-op: GW6 is
	// presence-based, and absence opts out of the check, not out of SPD3.
	if _, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
		}); err != nil {
		t.Fatalf("token-less repeat delete: %v", err)
	}
	if got := env.spRev(shard, spId); got != 2 {
		t.Errorf("a token-less repeat bumped sp_rev to %d, want 2", got)
	}
	// A STALE token still ABORTs on a deleting SP: SPD3 puts GW6 ahead of the
	// `deleting` short-circuit, so a client that lost the race is told so
	// rather than being answered OK.
	_, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
		})
	sptWantCode(t, err, codes.Aborted)
	sptWantStale(t, err)
}

// TestDeleteStoragePoolDuringDrain pins the two consequences SPD12 and SPD4
// state for the record: the name is NOT reusable until D3 commits, and every
// other mutator keeps refusing while the drain runs — at every stage of it,
// not only right after the latch.
func TestDeleteStoragePoolDuringDrain(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	shard := conf.GetShardCode()
	sptDelete(env, sptSpName, 1)

	// One step at a time, asserting the refusals after each: the drain passes
	// through a zero-cntlr state and a shrinking slice list, and a gate keyed
	// on either of those instead of on `deleting` would open partway through.
	for step := 0; step < sptDrainSteps; step++ {
		if !env.exists(model.SpConfKey(env.cid, sptSpName)) {
			break
		}
		// CreateStoragePool under the same name: the surviving sp_conf key is
		// what refuses it, so name reuse resumes only at D3.
		_, err := env.srv.CreateStoragePool(
			env.ctx, sptDefaultSpec(sptSpName).req(env.name))
		sptWantCode(t, err, codes.AlreadyExists)
		rev := env.spRev(shard, spId)
		_, err = env.srv.UpdateStoragePoolLevel(
			env.ctx, &pb.UpdateStoragePoolLevelRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				SpRev:       &pb.SpRev{Revision: rev},
				SpLevel:     pb.SpLevel_SP_LEVEL_READONLY,
			})
		sptWantCode(t, err, codes.FailedPrecondition)
		_, err = env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: rev},
			SliceId:     conf.GetSliceIdList()[0],
			IsMeta:      true,
		})
		sptWantCode(t, err, codes.FailedPrecondition)
		if got := env.spRev(shard, spId); got != rev {
			t.Fatalf("a refusal bumped sp_rev to %d, want %d", got, rev)
		}
		sptDrainStep(env, sptSpName)
	}
	if env.exists(model.SpConfKey(env.cid, sptSpName)) {
		t.Fatalf("the drain did not finish in %d steps", sptDrainSteps)
	}
	// The name is reusable the instant D3 commits.
	if _, err := env.srv.CreateStoragePool(
		env.ctx, sptDefaultSpec(sptSpName).req(env.name),
	); err != nil {
		t.Fatalf("CreateStoragePool after the drain: %v", err)
	}
}

// sptDrainStep runs exactly ONE drain step, for the tests that assert
// something between two of them.
func sptDrainStep(env *sptEnv, spName string) {
	env.t.Helper()
	conf := &pb.SpConf{}
	env.get(model.SpConfKey(env.cid, spName), conf)
	shard, spId := conf.GetShardCode(), conf.GetSpId()
	var err error
	switch {
	case len(conf.GetCntlrIdList()) > 0:
		_, err = model.DrainSpCntlrs(
			env.ctx, env.cli, env.cid, shard, spId, spName)
	case len(conf.GetSliceIdList()) > 0:
		_, _, err = model.DrainSpSlice(
			env.ctx, env.cli, env.cid, shard, spId, spName,
			sptLowestId(conf.GetSliceIdList()), env.cc)
	default:
		err = model.FinishSpDelete(
			env.ctx, env.cli, env.cid, shard, spId, spName)
	}
	if err != nil {
		env.t.Fatalf("drain step on %q: %v", spName, err)
	}
}
