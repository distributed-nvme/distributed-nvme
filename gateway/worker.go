package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md, Worker registry: ListWorkers, the operator's read
// of the worker registry (architecture.md, Worker registry).
//
// The registry is the one part of the store that belongs to no cluster: a
// WorkerReg key carries a role and a seed and no cluster_id, so the handler
// takes no cluster_name and resolves no cluster (GW5). It writes nothing.

// listedWorkerRoles are the roles every ListWorkers reply carries, each even
// when it has no registration.
var listedWorkerRoles = []string{
	common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp,
}

// ListWorkers is the ListWorkers of architecture.md, Worker registry: every
// registration as stored, keyed by role and seed, each with the epoch its
// worker last wrote.
//
// One plain range of model.WorkerRegistryPrefix, no STM, as the paged lists
// read (architecture.md, page_token): a transaction cannot range, and the
// listing is a snapshot of the keys, not an invariant. A failed range is
// ABORTED, as pageNames answers one. The reply is not paged: the registry
// holds one key per worker and role.
//
// A key model.ParseWorkerRegKey refuses is skipped: the vote layer parses the
// registry with the same function and skips such a key too, since it names no
// role a worker drives. A value that does not decode as WorkerReg is listed
// with an empty WorkerReg — a fresh one, never the message the failed decode
// half filled. The vote layer never decodes a value its scan finds: a key
// present is a live registration whatever its value holds (dnv-worker.md
// VW3), so such a key keeps its worker in every observer's view and the
// listing shows it rather than hide it; Decode's "etcd get" record carries
// the error.
func (s *Server) ListWorkers(
	ctx context.Context,
	_ *pb.ListWorkersRequest,
) (*pb.ListWorkersReply, error) {
	kvs, _, err := s.cli.Range(ctx, model.WorkerRegistryPrefix())
	if err != nil {
		return nil, errAborted("%v", err)
	}
	roleToWorkers := make(map[string]*pb.RoleWorkers, len(listedWorkerRoles))
	for _, role := range listedWorkerRoles {
		roleToWorkers[role] = &pb.RoleWorkers{
			SeedToReg: map[string]*pb.WorkerReg{},
		}
	}
	for _, kv := range kvs {
		role, seed, ok := model.ParseWorkerRegKey(kv.Key)
		if !ok {
			continue
		}
		reg := &pb.WorkerReg{}
		if s.cli.Decode(ctx, kv, reg) != nil {
			reg = &pb.WorkerReg{}
		}
		roleToWorkers[role].SeedToReg[seed] = reg
	}
	return &pb.ListWorkersReply{RoleToWorkers: roleToWorkers}, nil
}
