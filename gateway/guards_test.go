package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

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
