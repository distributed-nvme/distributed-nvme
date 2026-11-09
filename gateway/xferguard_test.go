package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A namespace a live transfer names is the transfer's origin: every cntlr's
// xfer stack reads its thin device. So DeleteNamespace and UpdateNamespaceDev
// of it are refused, and CreateNamespace refuses a place a transfer still
// names; UpdateNamespaceSuspended is not, since a suspend parks only the
// origin's own ns-dev and leaves the thin device the xfer stack reads as it
// is; a namespace no transfer names, the same index in another subsystem
// included, is not touched by the guard, and once the transfer is gone the
// namespace can be deleted (architecture.md, Transfers).
func TestANamespaceATransferNamesIsKept(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{
		{NsId: 601, NsIdx: 1, TdId: 900},
		{NsId: 602, NsIdx: 2, TdId: 900},
		{NsId: 603, NsIdx: 3, TdId: 900},
	})
	env.putSubsystem(volNqnB, 502, []*pb.Namespace{
		{NsId: 611, NsIdx: 1, TdId: 900},
	})
	env.putTd("vol", 900, 7, 0, true)
	env.putTd("snap", 901, 8, 7, true)
	volCreateTransfer(env, "xfer-a")
	// A second transfer whose origin, namespace 2, is then removed behind
	// the gateway's back: the shape a record changed outside the gateway
	// leaves.
	if _, err := env.srv.CreateTransfer(env.ctx, &pb.CreateTransferRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		XferName: "xfer-b", OriNqn: volNqn, OriNsIdx: 2,
		AllowedHosts: []string{volHostA},
	}); err != nil {
		t.Fatalf("CreateTransfer xfer-b: %v", err)
	}
	subsystem := env.subsystem(volNqn)
	subsystem.NsList = []*pb.Namespace{
		findNs(subsystem, 1), findNs(subsystem, 3)}
	mustPut(t, env.cli, model.SubsystemKey(env.cid, volSpId, volNqn),
		subsystem)

	_, err := env.srv.DeleteNamespace(env.ctx, &pb.DeleteNamespaceRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		Nqn: volNqn, NsIdx: 1,
	})
	if msg := volWantCode(t, err, codes.FailedPrecondition); !strings.Contains(
		msg, "xfer-a") {
		t.Errorf("DeleteNamespace refusal %q does not name the transfer", msg)
	}
	_, err = env.srv.UpdateNamespaceDev(env.ctx, &pb.UpdateNamespaceDevRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		Nqn: volNqn, NsIdx: 1, TdName: "snap",
	})
	if msg := volWantCode(t, err, codes.FailedPrecondition); !strings.Contains(
		msg, "xfer-a") {
		t.Errorf("UpdateNamespaceDev refusal %q does not name the transfer",
			msg)
	}
	_, err = env.srv.CreateNamespace(env.ctx, &pb.CreateNamespaceRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		Nqn: volNqn, NsIdx: 2, TdName: "vol",
	})
	if msg := volWantCode(t, err, codes.FailedPrecondition); !strings.Contains(
		msg, "xfer-b") {
		t.Errorf("CreateNamespace refusal %q does not name the transfer", msg)
	}
	if ns := findNs(env.subsystem(volNqn), 1); ns == nil ||
		ns.GetTdId() != 900 {
		t.Fatalf("the origin namespace changed: %v", ns)
	}
	if findNs(env.subsystem(volNqn), 2) != nil {
		t.Fatalf("a namespace was created where xfer-b names its origin")
	}

	if _, err := env.srv.UpdateNamespaceSuspended(env.ctx,
		&pb.UpdateNamespaceSuspendedRequest{
			ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
			Nqn: volNqn, NsIdx: 1, Suspended: true,
		}); err != nil {
		t.Fatalf("UpdateNamespaceSuspended of an origin: %v", err)
	}
	if _, err := env.srv.UpdateNamespaceDev(env.ctx,
		&pb.UpdateNamespaceDevRequest{
			ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
			Nqn: volNqn, NsIdx: 3, TdName: "snap",
		}); err != nil {
		t.Fatalf("UpdateNamespaceDev of a namespace no transfer names: %v",
			err)
	}
	// The same index in another subsystem is no origin.
	if _, err := env.srv.UpdateNamespaceDev(env.ctx,
		&pb.UpdateNamespaceDevRequest{
			ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
			Nqn: volNqnB, NsIdx: 1, TdName: "snap",
		}); err != nil {
		t.Fatalf("UpdateNamespaceDev of another subsystem's index 1: %v", err)
	}
	if _, err := env.srv.DeleteNamespace(env.ctx, &pb.DeleteNamespaceRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		Nqn: volNqnB, NsIdx: 1,
	}); err != nil {
		t.Fatalf("DeleteNamespace of another subsystem's index 1: %v", err)
	}

	if _, err := env.srv.DeleteTransfer(env.ctx, &pb.DeleteTransferRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		XferName: "xfer-a", Force: true,
	}); err != nil {
		t.Fatalf("DeleteTransfer: %v", err)
	}
	if _, err := env.srv.DeleteNamespace(env.ctx, &pb.DeleteNamespaceRequest{
		ClusterName: env.cluster, SpName: volSpName, SpRev: env.token(),
		Nqn: volNqn, NsIdx: 1,
	}); err != nil {
		t.Fatalf("DeleteNamespace after the transfer is gone: %v", err)
	}
}

// The guard reads the transfers inside the deciding transaction: a transfer
// created while DeleteNamespace's STM runs rewrites the pool's transfer list,
// so that commit fails, and the retry sees the transfer and refuses.
func TestATransferCreatedDuringANamespaceDeleteIsSeen(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, []*pb.Namespace{
		{NsId: 601, NsIdx: 1, TdId: 900},
	})
	env.putTd("vol", 900, 7, 0, true)
	var actorErr error
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     model.SubsystemKey(env.cid, volSpId, volNqn),
		actor: func() {
			_, actorErr = env.srv.CreateTransfer(env.ctx,
				&pb.CreateTransferRequest{
					ClusterName: env.cluster, SpName: volSpName,
					XferName: "xfer-r", OriNqn: volNqn, OriNsIdx: 1,
					AllowedHosts: []string{volHostA},
				})
		},
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	ctx := context.WithValue(env.ctx, volReadHookKey{}, true)
	_, err := env.srv.DeleteNamespace(ctx, &pb.DeleteNamespaceRequest{
		ClusterName: env.cluster, SpName: volSpName,
		Nqn: volNqn, NsIdx: 1,
	})
	if !hook.fired.Load() {
		t.Fatalf("the hook never fired")
	}
	if actorErr != nil {
		t.Fatalf("CreateTransfer during the delete: %v", actorErr)
	}
	volWantCode(t, err, codes.FailedPrecondition)
}
