package gateway

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the handler tests of ListWorkers (gateway.md, Worker
// registry), the read of the worker registry (architecture.md, Worker
// registry), driven against the real etcd of etcdenv_test.go.
//
// The registry is cluster-independent, so neither a cluster name nor testCid
// keeps these tests apart from each other: the registry is one key space for
// the whole etcd. No other test of the package writes a WorkerReg key and none
// runs in parallel, so each test here empties the registry before it starts
// and again when it ends (hwkClearRegistry), and asserts the whole reply.

// hwkClearRegistry deletes every key under the registry prefix now and again
// when the test ends.
func hwkClearRegistry(t *testing.T, s *Server) {
	t.Helper()
	clearAll := func(report func(string, ...any)) {
		ctx := context.Background()
		keys, _, err := s.cli.RangeKeys(ctx, model.WorkerRegistryPrefix())
		if err != nil {
			report("RangeKeys %s: %v", model.WorkerRegistryPrefix(), err)
			return
		}
		for _, entry := range keys {
			if err := s.cli.Delete(ctx, entry.Key); err != nil {
				report("Delete %s: %v", entry.Key, err)
			}
		}
	}
	clearAll(t.Fatalf)
	t.Cleanup(func() { clearAll(t.Errorf) })
}

// hwkReply builds the reply ListWorkers must give: every role of
// common.WorkerRole* present, the ones regs names with its registrations, the
// others empty.
func hwkReply(regs map[string]map[string]uint64) *pb.ListWorkersReply {
	reply := &pb.ListWorkersReply{
		RoleToWorkers: map[string]*pb.RoleWorkers{},
	}
	for _, role := range []string{
		common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp,
	} {
		workers := &pb.RoleWorkers{SeedToReg: map[string]*pb.WorkerReg{}}
		for seed, epoch := range regs[role] {
			workers.SeedToReg[seed] = &pb.WorkerReg{Epoch: epoch}
		}
		reply.RoleToWorkers[role] = workers
	}
	return reply
}

// hwkList calls ListWorkers and asserts the whole reply.
func hwkList(
	t *testing.T, s *Server, want *pb.ListWorkersReply, label string,
) {
	t.Helper()
	got, err := s.ListWorkers(context.Background(), &pb.ListWorkersRequest{})
	wantCode(t, err, codes.OK, label)
	if !proto.Equal(got, want) {
		t.Errorf("%s:\n got %v\nwant %v", label, got, want)
	}
	// Every role is also looked up by name, so a role missing from the reply
	// names itself in the failure: an empty role must show.
	for role := range want.GetRoleToWorkers() {
		if _, ok := got.GetRoleToWorkers()[role]; !ok {
			t.Errorf("%s: the reply has no role %q", label, role)
		}
	}
}

// TestListWorkersListsEveryRoleAsStored pins the reply of an empty registry
// and of a registry with workers in two of the three roles: each registration
// under its role and seed with the epoch its writer stored, and every role
// present, the empty one included. No cluster exists for the call to resolve,
// and it needs none.
func TestListWorkersListsEveryRoleAsStored(t *testing.T) {
	s := newTestServer(t)
	hwkClearRegistry(t, s)

	hwkList(t, s, hwkReply(nil), "an empty registry")

	mustPut(t, s.cli, model.WorkerRegKey(common.WorkerRoleDn, "dn-seed-a"),
		&pb.WorkerReg{Epoch: 100})
	mustPut(t, s.cli, model.WorkerRegKey(common.WorkerRoleDn, "dn-seed-b"),
		&pb.WorkerReg{Epoch: 200})
	mustPut(t, s.cli, model.WorkerRegKey(common.WorkerRoleSp, "sp-seed-a"),
		&pb.WorkerReg{Epoch: 300})
	hwkList(t, s, hwkReply(map[string]map[string]uint64{
		common.WorkerRoleDn: {"dn-seed-a": 100, "dn-seed-b": 200},
		common.WorkerRoleSp: {"sp-seed-a": 300},
	}), "two roles registered, cn empty")
}

// TestListWorkersSkipsAMalformedKey pins the skip of a key under the registry
// prefix that model.ParseWorkerRegKey refuses, as the vote layer skips it: an
// unknown role, a field too many, an empty seed and a role with no seed. The
// well-formed key beside them is still listed.
func TestListWorkersSkipsAMalformedKey(t *testing.T) {
	s := newTestServer(t)
	hwkClearRegistry(t, s)

	prefix := model.WorkerRegistryPrefix()
	for _, key := range []string{
		prefix + "xx seed-x",
		prefix + common.WorkerRoleDn + " seed-y extra",
		prefix + common.WorkerRoleCn + " ",
		prefix + common.WorkerRoleSp,
	} {
		if _, _, ok := model.ParseWorkerRegKey(key); ok {
			t.Fatalf("ParseWorkerRegKey accepts %q: not a malformed key", key)
		}
		mustPut(t, s.cli, key, &pb.WorkerReg{Epoch: 9})
	}
	mustPut(t, s.cli, model.WorkerRegKey(common.WorkerRoleCn, "cn-seed-a"),
		&pb.WorkerReg{Epoch: 400})
	hwkList(t, s, hwkReply(map[string]map[string]uint64{
		common.WorkerRoleCn: {"cn-seed-a": 400},
	}), "malformed keys beside a good one")
}

// TestListWorkersListsAnUndecodableValueEmpty pins the registration whose
// value does not decode as WorkerReg: it is listed, under its role and seed,
// with an empty WorkerReg. The vote layer counts the key as present whatever
// its value holds, so the listing must show it too.
//
// The value is a well-formed epoch followed by a truncated tag: the decode
// fails after it has set the epoch, so a handler that kept the half-decoded
// message would list 77 where the reply must say 0.
func TestListWorkersListsAnUndecodableValueEmpty(t *testing.T) {
	s := newTestServer(t)
	hwkClearRegistry(t, s)

	bad := &pb.WorkerReg{Epoch: 77}
	bad.ProtoReflect().SetUnknown(protoreflect.RawFields{0x08})
	raw, err := proto.Marshal(bad)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := proto.Unmarshal(raw, &pb.WorkerReg{}); err == nil {
		t.Fatalf("the value %x decodes: not an undecodable value", raw)
	}
	mustPut(t, s.cli, model.WorkerRegKey(common.WorkerRoleSp, "sp-seed-bad"),
		bad)
	mustPut(t, s.cli, model.WorkerRegKey(common.WorkerRoleSp, "sp-seed-good"),
		&pb.WorkerReg{Epoch: 500})
	hwkList(t, s, hwkReply(map[string]map[string]uint64{
		common.WorkerRoleSp: {"sp-seed-bad": 0, "sp-seed-good": 500},
	}), "an undecodable value beside a good one")
}

// TestListWorkersRangeErrorIsAborted pins GW7's ABORTED for a failed range of
// the registry, the reply carrying the range's error. The context is cancelled
// before the call, so the range fails at once: a connection that breaks under
// the range would not do, because the etcd client retries a range until its
// deadline (etcdloss_test.go breaks a transaction instead).
func TestListWorkersRangeErrorIsAborted(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.ListWorkers(ctx, &pb.ListWorkersRequest{})
	wantCode(t, err, codes.Aborted, "ListWorkers under a cancelled context")
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "etcd range "+model.WorkerRegistryPrefix()) ||
		!strings.Contains(msg, context.Canceled.Error()) {
		t.Errorf("ListWorkers' message %q: not the failed range's error", msg)
	}
}
