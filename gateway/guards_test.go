package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestCreateNamespaceRefusesASecondNamespaceOnATransfersVolume pins
// architecture.md, Transfers, The origin is its td's only namespace: a namespace onto
// the volume of a transfer's origin namespace would let host writes change
// the source in the middle of the clone's copy, so CreateNamespace refuses
// it with FAILED_PRECONDITION naming the transfer, and writes nothing. A
// namespace onto another volume is still accepted.
func TestCreateNamespaceRefusesASecondNamespaceOnATransfersVolume(
	t *testing.T,
) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{{
		NsId: 601, NsIdx: 1, TdId: 900,
	}})
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("other", 901, 8, 0, true)
	volCreateTransfer(env, "xfer-a")
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.CreateNamespace(env.ctx, &pb.CreateNamespaceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       &pb.SpRev{Revision: beforeRev},
		Nqn:         volNqn,
		NsIdx:       2,
		TdName:      "vol",
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "xfer-a") {
		t.Errorf("message %q must name the transfer", msg)
	}
	env.wantUntouched(before, beforeRev)
	if _, err := env.srv.CreateNamespace(env.ctx, &pb.CreateNamespaceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		Nqn:         volNqn,
		NsIdx:       2,
		TdName:      "other",
	}); err != nil {
		t.Fatalf("a namespace on another volume: %v", err)
	}
}

// TestCreateCloneRefusesAnUncreatedSnapshotOfItsDestination pins
// architecture.md, Clones, CreateClone: a snapshot of the destination not yet created
// would hold part of the copy, so CreateClone refuses it with
// FAILED_PRECONDITION naming the snapshot, and writes nothing.
func TestCreateCloneRefusesAnUncreatedSnapshotOfItsDestination(
	t *testing.T,
) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	env.putTd("snap", 901, 8, 7, false)
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.CreateClone(env.ctx,
		volCloneRequest(env, "clone-a", "dst"))
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "snap") {
		t.Errorf("message %q must name the snapshot", msg)
	}
	env.wantUntouched(before, beforeRev)
}

// TestCreateCloneSeesASnapshotMadeAfterItsPlan: the snapshot walk runs in
// the plan, so the deciding transaction holds the plan to the pool's
// revision; a snapshot of the destination made between the two moves the
// revision, the plan is made again, and its walk refuses.
func TestCreateCloneSeesASnapshotMadeAfterItsPlan(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	// The placement of TestCreateCloneReplansWhenAPickMoves, whose scan
	// reads dn-c after the plan.
	env.setDnBatchSize(1)
	env.setDnFree(volDnC, volDnCFree)
	env.setDnFree(volDnD, volDnCFree-1)
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     model.DnConfKey(env.cid, volDnC),
		actor: func() {
			env.putTd("snap", 901, 8, 7, false)
			rev := &pb.SpRev{}
			env.get(model.SpRevKey(volShard, env.cid, volSpId), rev)
			rev.Revision++
			mustPut(env.t, env.cli,
				model.SpRevKey(volShard, env.cid, volSpId), rev)
		},
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, volReadHookKey{}, true), 30*time.Second)
	defer cancel()
	req := volCloneRequest(env, "clone-a", "dst")
	req.SpRev = nil
	_, err := env.srv.CreateClone(ctx, req)
	slog.SetDefault(prev)
	if !hook.fired.Load() {
		t.Fatalf("the request never read dn-c: nothing was raced")
	}
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "snap") {
		t.Errorf("message %q must name the snapshot made after the plan",
			msg)
	}
	if env.exists(model.CloneKey(env.cid, volSpId, "clone-a"),
		&pb.Clone{}) {
		t.Fatal("the clone was created over the uncreated snapshot")
	}
}

// TestCreateCloneReplansAfterAPoolRecreate: the deciding transaction holds
// the plan to the pool's sp_id as well as to its revision (architecture.md,
// Clones), as DeleteThinDevice's does. Between the plan and the deciding
// transaction the pool is deleted and created again under the same name: a
// new sp_id, its SpRev at the plan's revision, the destination created and
// an uncreated snapshot of it that the plan's walk never read. The round is
// planned again, and its walk refuses naming the snapshot; the new pool
// takes no clone and its records do not move. A token does not help: it is
// the plan's revision, which the new pool also stands at, so both modes run.
// The new pool is written directly, the state a delete and a create inside
// one plan-to-transaction gap would leave, and only the keys the request
// reads are rewritten.
func TestCreateCloneReplansAfterAPoolRecreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token bool
	}{
		{name: "without a token"},
		{name: "with the token", token: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.putTd("dst", 900, 7, 0, true)
			// The placement of TestCreateCloneSeesASnapshotMadeAfterItsPlan,
			// whose scan reads dn-c after the plan.
			env.setDnBatchSize(1)
			env.setDnFree(volDnC, volDnCFree)
			env.setDnFree(volDnD, volDnCFree-1)
			planRev := env.spRev()
			newSpId := volSpId + 1
			recreated := volSpConf()
			recreated.SpId = newSpId
			recreated.NextId = volNextId + 2
			recreated.NextDevId = volNextDevId + 2
			recreated.TdNameList = []string{"dst", "snap"}
			prev := slog.Default()
			hook := &volReadHook{
				Handler: prev.Handler(),
				key:     model.DnConfKey(env.cid, volDnC),
				actor: func() {
					for _, key := range []string{
						model.ThinDeviceKey(env.cid, volSpId, "dst"),
						model.SpRevKey(volShard, env.cid, volSpId),
						model.SpNameKey(env.cid, volSpId),
						model.SpConfKey(env.cid, volSpName),
					} {
						if err := env.cli.Delete(env.ctx, key); err != nil {
							t.Errorf("Delete %s: %v", key, err)
						}
					}
					mustPut(t, env.cli, model.SpConfKey(env.cid, volSpName),
						recreated)
					mustPut(t, env.cli, model.SpNameKey(env.cid, newSpId),
						&pb.SpName{SpName: volSpName})
					mustPut(t, env.cli,
						model.SpRevKey(volShard, env.cid, newSpId),
						&pb.SpRev{SpName: volSpName, Revision: planRev})
					mustPut(t, env.cli,
						model.ThinDeviceKey(env.cid, newSpId, "dst"),
						&pb.ThinDevice{
							TdId: volNextId, DevId: 1, Size: volTdSize,
							Created: true,
						})
					mustPut(t, env.cli,
						model.ThinDeviceKey(env.cid, newSpId, "snap"),
						&pb.ThinDevice{
							TdId: volNextId + 1, DevId: 2, OriId: 1,
							Size: volTdSize,
						})
				},
			}
			slog.SetDefault(slog.New(hook))
			defer slog.SetDefault(prev)
			ctx, cancel := context.WithTimeout(
				context.WithValue(env.ctx, volReadHookKey{}, true),
				30*time.Second)
			defer cancel()
			req := volCloneRequest(env, "clone-a", "dst")
			if !tc.token {
				req.SpRev = nil
			}
			_, err := env.srv.CreateClone(ctx, req)
			slog.SetDefault(prev)
			if !hook.fired.Load() {
				t.Fatalf("the request never read dn-c: nothing was raced")
			}
			msg := volWantCode(t, err, codes.FailedPrecondition)
			if want := "snapshot snap of dst is not created yet"; msg != want {
				t.Errorf("message: got %q, want %q", msg, want)
			}
			if env.exists(model.CloneKey(env.cid, newSpId, "clone-a"),
				&pb.Clone{}) {
				t.Error("the clone was created in the new pool over its " +
					"uncreated snapshot")
			}
			conf := &pb.SpConf{}
			env.get(model.SpConfKey(env.cid, volSpName), conf)
			if !proto.Equal(conf, recreated) {
				t.Errorf("the new pool's sp_conf moved on a refusal:\n got "+
					"%v\nwant %v", conf, recreated)
			}
			rev := &pb.SpRev{}
			env.get(model.SpRevKey(volShard, env.cid, newSpId), rev)
			if rev.GetRevision() != planRev {
				t.Errorf("the new pool's sp_rev: got %d, want %d (a "+
					"refusal must not bump)", rev.GetRevision(), planRev)
			}
		})
	}
}

// TestTheOriginsVolumeTakesNoSecondNamespace pins the two other ways to put a
// second namespace on a transfer's origin volume (architecture.md,
// Transfers): UpdateNamespaceDev of another namespace onto that volume is
// refused naming the transfer, and CreateTransfer of an origin whose volume
// another namespace already carries is refused naming that namespace. Each
// writes nothing.
func TestTheOriginsVolumeTakesNoSecondNamespace(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{
		{NsId: 601, NsIdx: 1, TdId: 900},
		{NsId: 602, NsIdx: 2, TdId: 901},
	})
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("other", 901, 8, 0, true)
	volCreateTransfer(env, "xfer-a")
	before := env.spConf()
	beforeRev := env.spRev()
	_, err := env.srv.UpdateNamespaceDev(env.ctx,
		&pb.UpdateNamespaceDevRequest{
			ClusterName: env.cluster, SpName: volSpName,
			SpRev: &pb.SpRev{Revision: beforeRev},
			Nqn:   volNqn, NsIdx: 2, TdName: "vol",
		})
	if msg := volWantCode(t, err, codes.FailedPrecondition); !strings.Contains(
		msg, "xfer-a") {
		t.Errorf("UpdateNamespaceDev refusal %q must name the transfer", msg)
	}
	env.wantUntouched(before, beforeRev)

	env2 := newVolEnv(t)
	env2.putSubsystem(volNqn, 501, []*pb.Namespace{
		{NsId: 601, NsIdx: 1, TdId: 900},
	})
	env2.putSubsystem(volNqnB, 502, []*pb.Namespace{
		{NsId: 611, NsIdx: 3, TdId: 900},
	})
	env2.putTd("vol", 900, 7, 0, true)
	before = env2.spConf()
	beforeRev = env2.spRev()
	_, err = env2.srv.CreateTransfer(env2.ctx, &pb.CreateTransferRequest{
		ClusterName: env2.cluster, SpName: volSpName,
		SpRev:    &pb.SpRev{Revision: beforeRev},
		XferName: "xfer-a", OriNqn: volNqn, OriNsIdx: 1,
		AllowedHosts: []string{volHostA}, AutoSuspend: true,
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(msg, "namespace 3") ||
		!strings.Contains(msg, volNqnB) {
		t.Errorf("CreateTransfer refusal %q must name the other namespace",
			msg)
	}
	env2.wantUntouched(before, beforeRev)
}

// TestNoTransferOfAVolumeACloneFills pins architecture.md, Transfers and
// Clones: a volume a clone fills has holes below its dm-clone, so
// CreateTransfer refuses an origin on it with FAILED_PRECONDITION naming the
// clone, and CreateClone refuses a destination a transfer exports, naming the
// transfer; neither writes. The refusal lasts as long as the clone's record:
// once the record is gone, the transfer is accepted.
func TestNoTransferOfAVolumeACloneFills(t *testing.T) {
	t.Run("a transfer of a clone's destination", func(t *testing.T) {
		env := newVolEnv(t)
		env.putSubsystem(volNqn, 501, []*pb.Namespace{
			{NsId: 601, NsIdx: 1, TdId: 900, Suspended: true}})
		env.putTd("vol", 900, 7, 0, true)
		volCreateClone(env, "clone-a", "vol")
		before := env.spConf()
		beforeRev := env.spRev()
		_, err := env.srv.CreateTransfer(env.ctx, &pb.CreateTransferRequest{
			ClusterName: env.cluster, SpName: volSpName,
			SpRev:    &pb.SpRev{Revision: beforeRev},
			XferName: "xfer-a", OriNqn: volNqn, OriNsIdx: 1,
			AllowedHosts: []string{volHostA}, AutoSuspend: true,
		})
		msg := volWantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(msg, `clone "clone-a"`) {
			t.Errorf("CreateTransfer refusal %q must name the clone", msg)
		}
		env.wantUntouched(before, beforeRev)

		// A clone latched for deletion still fills the volume until the
		// drain removes its record (CLD9), so the refusal stands.
		if _, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
			ClusterName: env.cluster, SpName: volSpName,
			SpRev:     &pb.SpRev{Revision: env.spRev()},
			CloneName: "clone-a", Force: true,
		}); err != nil {
			t.Fatalf("DeleteClone: %v", err)
		}
		latched := &pb.Clone{}
		env.get(model.CloneKey(env.cid, volSpId, "clone-a"), latched)
		if !latched.GetDeleting() {
			t.Fatalf("clone-a not latched: %v", latched)
		}
		before, beforeRev = env.spConf(), env.spRev()
		_, err = env.srv.CreateTransfer(env.ctx, &pb.CreateTransferRequest{
			ClusterName: env.cluster, SpName: volSpName,
			SpRev:    &pb.SpRev{Revision: beforeRev},
			XferName: "xfer-a", OriNqn: volNqn, OriNsIdx: 1,
			AllowedHosts: []string{volHostA}, AutoSuspend: true,
		})
		msg = volWantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(msg, `clone "clone-a"`) {
			t.Errorf("CreateTransfer refusal %q must name the latched clone",
				msg)
		}
		env.wantUntouched(before, beforeRev)

		// The clone's record gone, as the drain's final step leaves it.
		if err := env.cli.Delete(env.ctx,
			model.CloneKey(env.cid, volSpId, "clone-a")); err != nil {
			t.Fatalf("delete the clone key: %v", err)
		}
		conf := env.spConf()
		conf.CloneNameList = nil
		env.putSpConf(conf)
		volCreateTransfer(env, "xfer-a")
	})
	t.Run("a clone into a transfer's volume", func(t *testing.T) {
		env := newVolEnv(t)
		env.putSubsystem(volNqn, 501, []*pb.Namespace{
			{NsId: 601, NsIdx: 1, TdId: 900}})
		env.putTd("vol", 900, 7, 0, true)
		volCreateTransfer(env, "xfer-a")
		before := env.spConf()
		beforeRev := env.spRev()
		_, err := env.srv.CreateClone(env.ctx,
			volCloneRequest(env, "clone-a", "vol"))
		msg := volWantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(msg, "xfer-a") {
			t.Errorf("CreateClone refusal %q must name the transfer", msg)
		}
		env.wantUntouched(before, beforeRev)
	})
}
