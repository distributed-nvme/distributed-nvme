package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Thin devices / gateway.md, Thin devices: the
// three thin-device RPCs. All three are pure etcd — a thin device has no
// allocator footprint and no agent call, because the gateway only ever writes
// the DESIRED row and the primary cntlr builds the dm-thin volumes on the
// SpRev fan-out these bumps start (architecture.md, Primary cntlr;
// architecture.md, sp role).
//
// The one subtlety the whole file is built around is `ThinDevice.created`. The
// gateway always writes it false and never reads it back except as a gate: it
// is set true exactly once by the sp-worker, when a cntlr has reported that
// td's thin volume RES_STATUS_OK in EVERY slice of the SP. Snapshot creation
// is gated on it because `create_snap` has a kernel-level dependency on the
// origin's id already being in each slice pool, and origin deletion is gated
// on it because the sweep runs before the build (cnagent.md CN9), so an origin
// leaving td_list in the converge that would first materialize its snapshot
// would send `delete {ori dev_id}` before `create_snap` and lose the snapshot
// for good.

// The op names the model helpers put into their error messages and the bump
// helpers cite; they are the RPC names so a log line names something
// greppable. ListThinDevices has none: it never bumps.
const (
	opCreateThinDevice = "CreateThinDevice"
	opDeleteThinDevice = "DeleteThinDevice"
)

// msgOriginNotCreated is the normative refusal of architecture.md, Thin
// devices, for a snapshot whose origin has not materialized. It is a format
// constant rather than an inline string because the integration suite greps
// the sentence, and because it is the one error in this file that also
// promises the client a way forward.
const msgOriginNotCreated = "origin %s is not created yet; " +
	"wait for ListThinDevices to report created = true"

// msgSnapshotNotCreated is the normative detail of architecture.md, Thin
// devices, for the delete guard that names the blocking snapshot(s).
const msgSnapshotNotCreated = "snapshot %s of %s is not created yet"

// CreateThinDevice is the CreateThinDevice of architecture.md, Thin devices.
//
// Only two of its checks of architecture.md, Common validation, are pure and
// therefore run here (GW4): the names,
// and `size == 0`, which is legal exactly when `ori_name` is set because a
// snapshot then inherits the origin's size ([D-H]). The rest of the size rule
// — a positive multiple of `slice_cnt × stripe_size`, which is what lets
// dm-striped take equal, chunk-aligned members — depends on the SP's stored
// geometry and on the origin, so it is state-dependent and runs inside the
// transaction (gateway.md, Thin devices).
//
// The two snapshot refusals — an origin with `created == false`, and an origin
// that is the destination of a clone — are the places in this file that must
// write literally nothing: architecture.md, Thin devices, requires no key, no
// next_id / next_dev_id consumption and no SpRev bump, so both checks sit
// before the minter is ever created and return straight out of the closure,
// which leaves the whole transaction uncommitted (EU4). Consuming an id there
// would be visible forever — ids are never reused — for a request that failed.
//
// The RPC never blocks on materialization (architecture.md, STM discipline,
// keeps every RPC short); a client that wants to snapshot polls
// ListThinDevices instead.
func (s *Server) CreateThinDevice(
	ctx context.Context,
	req *pb.CreateThinDeviceRequest,
) (*pb.CreateThinDeviceReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("td_name", req.GetTdName()); err != nil {
		return nil, err
	}
	if err := validateOptionalName(
		"ori_name", req.GetOriName(),
	); err != nil {
		return nil, err
	}
	if req.GetSize() == 0 && req.GetOriName() == "" {
		// A fresh device has nothing to inherit a size from ([D-H]).
		return nil, errInvalid(
			"size must not be 0 unless ori_name is set")
	}
	var tdId uint64
	var devId uint32
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		// Reassigned on every attempt: the closure is re-run on conflict and
		// must be a pure function of what it reads (GW8).
		tdId = 0
		devId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		if len(sc.Conf.GetTdNameList()) >= common.MaxTdCntPerSp {
			return errExhausted(
				"storage pool %q holds %d thin devices, the maximum is %d",
				req.GetSpName(), len(sc.Conf.GetTdNameList()),
				common.MaxTdCntPerSp)
		}
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName())
		// Both halves are checked: the list is what every walk of
		// architecture.md, Thin devices, uses
		// and the key is what the row actually lives at, so a store where
		// they disagree still refuses rather than orphaning one of them.
		if containsName(sc.Conf.GetTdNameList(), req.GetTdName()) ||
			stm.Get(tdKey, &pb.ThinDevice{}) {
			return errExists("thin device %q already exists", req.GetTdName())
		}
		var origin *pb.ThinDevice
		if req.GetOriName() != "" {
			origin = &pb.ThinDevice{}
			oriKey := model.ThinDeviceKey(
				sc.Cid, sc.SpId(), req.GetOriName())
			if !stm.Get(oriKey, origin) {
				return errNotFound(
					"thin device %q not found", req.GetOriName())
			}
			if !origin.GetCreated() {
				// Checked after NOT_FOUND (architecture.md, Thin devices) and
				// before anything is minted, so the request is a no-op the
				// client can retry unchanged once the origin materializes. A
				// snapshot OF a snapshot follows the same rule: only the
				// IMMEDIATE origin is gated.
				return errPrecondition(
					msgOriginNotCreated, req.GetOriName())
			}
			// A clone hydrates INTO its destination, and while it does, the
			// destination's thin volumes hold only the regions hydrated so
			// far, dm-clone serving every other read from the source: a
			// create_snap of them would take a partial copy for a snapshot
			// (architecture.md, Thin devices). Hydration is known only to the
			// primary's CN and this RPC is pure etcd, so the refusal holds for
			// as long as the Clone key does — the delete guard's walk, drain
			// included — and, like the created gate, it returns before
			// anything is minted.
			cloneName, found, err := tdCloneRef(stm, sc, origin.GetTdId())
			if err != nil {
				return err
			}
			if found {
				return errPrecondition(
					"origin %s is the destination of clone %s",
					req.GetOriName(), cloneName)
			}
		}
		// A nil origin reads as 0 here, which is exactly the "no origin"
		// sentinel of ori_id and the "inherit nothing" case of size — and
		// the pure check above has already refused size 0 without an origin.
		size := req.GetSize()
		if size == 0 {
			size = origin.GetSize()
		}
		// The stripe is the SP's stored one (architecture.md, Common
		// validation). Checking the stored bdev_conf separately keeps the two
		// lost-invariant cases apart: a zero stripe and an empty slice_id_list
		// would otherwise both arrive at "has no slice", and only one of them
		// is about slices.
		if err := model.ValidateBdevConf(spBdevConf(sc.Conf)); err != nil {
			return errAborted("%v", err)
		}
		sliceCnt := spSliceCnt(sc.Conf)
		stripe := spStripeSize(sc.Conf)
		unit := uint64(sliceCnt) * stripe
		if unit == 0 {
			// An SP always has at least one slice; a slice_id_list that is
			// empty is a lost invariant, not a user error (architecture.md,
			// UNEXPECTED_ERROR → `ABORTED`).
			return errAborted(
				"storage pool %q has no slice", req.GetSpName())
		}
		// Positive as well as aligned: a snapshot inheriting the size of an
		// origin row that somehow carries 0 would otherwise slip past the
		// pure check, which can only see the request's own field.
		if size == 0 || size%unit != 0 {
			return errInvalid(
				"size %d must be a positive multiple of %d "+
					"(slice_cnt %d x stripe_size %d)",
				size, unit, sliceCnt, stripe)
		}
		minter := newSpIdMinter(sc.Conf)
		tdId = minter.mint()
		devId = nextDevId(sc.Conf)
		stm.Put(tdKey, &pb.ThinDevice{
			TdId:  tdId,
			DevId: devId,
			OriId: origin.GetDevId(),
			Size:  size,
			// Always false: materialization is the sp-worker's write
			// (architecture.md, sp role; architecture.md, Thin devices,
			// Materialization; dnv-worker.md RW19), and this RPC has no way to
			// know whether every slice pool already holds the id.
			Created: false,
		})
		sc.Conf.TdNameList = append(sc.Conf.GetTdNameList(), req.GetTdName())
		minter.commit(sc.Conf)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opCreateThinDevice, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateThinDeviceReply{TdId: tdId, DevId: devId}, nil
}

// DeleteThinDevice is the DeleteThinDevice of architecture.md, Thin devices: a
// plan, then a deciding STM that verifies it, run in the candidate unit's loop
// (GW9).
//
// Its three FAILED_PRECONDITION guards are decided inside the deleting
// transaction, which is what makes them race-proof: a CreateNamespace, a
// CreateClone or a CreateThinDevice snapshotting this td that commits
// concurrently touches a key this transaction read, so the later of the two to
// commit conflicts and etcdutil re-runs it against the state that actually
// won; a request that carries a token then fails GW6, ABORTED, and its client
// retries (architecture.md, STM discipline; architecture.md, UNEXPECTED_ERROR
// → `ABORTED`).
//
// The third guard's walk is the one read the transaction does not make. Finding
// the snapshots of this td means reading every td of the SP, and a transaction
// compares every key it read: toward the MaxTdCntPerSp ceiling that walk took
// the delete past EtcdMaxTxnOps and etcd refused it, so that no td of a full
// pool could be deleted at all. The walk is therefore the PLAN, a read-only
// Snapshot, and the deciding STM re-reads only the snapshots the plan found —
// after checking that the SP it resolved is the one the plan walked and that
// SpRev still carries the revision the plan was read at. Every write to a td
// bumps SpRev (architecture.md, Revision keys and the sync fan-out), so a
// snapshot created between the plan and the delete's
// commit either moves that revision before the STM reads it, and the round
// re-plans, or commits after that read and conflicts the transaction on the
// SpRev key, whose re-run re-plans the same way. That is the token-less path.
// A request that carries a token does not get that far: its token is the
// revision the plan read, so openSp's GW6 check refuses the moved revision
// first, ABORTED "stale revision". The SP's identity and its revision are what
// the plan is verified by; nothing is counted on the origin.
//
// Deleting the origin of snapshots is allowed once every snapshot of it is
// created — dm-thin snapshots stay valid without their origin — which is why
// the third guard tests `created == false` and not "is a snapshot".
func (s *Server) DeleteThinDevice(
	ctx context.Context,
	req *pb.DeleteThinDeviceRequest,
) (*pb.DeleteThinDeviceReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("td_name", req.GetTdName()); err != nil {
		return nil, err
	}
	var tdId uint64
	err := candidateUnit(ctx, func() error {
		tdId = 0
		plan, err := planDeleteThinDevice(ctx, s.cli, req)
		if err != nil {
			return err
		}
		tdDeletePlanned()
		tdId, err = decideDeleteThinDevice(ctx, s.cli, req, plan)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteThinDeviceReply{TdId: tdId}, nil
}

// tdDeletePlanned runs in every DeleteThinDevice round between a plan that
// succeeded and the deciding STM, and does nothing in production. It is the
// seam a test drives the other side of a race through: a write committed here
// lands in exactly the window the plan opens, one actor after the other rather
// than concurrently, and counting its calls counts the plans that went on to
// a deciding STM.
var tdDeletePlanned = func() {}

// tdDeletePlan is what DeleteThinDevice's plan hands its deciding STM.
type tdDeletePlan struct {
	// Cid and SpId identify the SP incarnation the plan walked: a recreated
	// SP's SpRev starts again at 1, so its revision alone cannot tell two
	// incarnations of one sp_name apart.
	Cid  uint64
	SpId uint64
	// SpRevision is SpRev.revision as the plan read it: the pool revision the
	// deciding STM verifies the plan against.
	SpRevision uint64
	// Snapshots names the uncreated snapshots of the target the plan found,
	// in td_name_list order.
	Snapshots []string
}

// planDeleteThinDevice is DeleteThinDevice's plan: one read-only Snapshot
// (architecture.md, STM discipline) that walks td_name_list for the target's
// uncreated snapshots.
//
// It opens with openSp, so a stale token is ABORTED before any other state
// check exactly as in the deciding STM (GW6), and it refuses what the revision
// it read already proves: NOT_FOUND for an absent td, and ABORTED for a listed
// td whose key is gone, since a delete that cannot prove the td has no
// uncreated snapshot must not proceed.
func planDeleteThinDevice(
	ctx context.Context,
	cli *etcdutil.Client,
	req *pb.DeleteThinDeviceRequest,
) (tdDeletePlan, error) {
	var plan tdDeletePlan
	err := cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		plan = tdDeletePlan{}
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		td := &pb.ThinDevice{}
		if !stm.Get(
			model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName()), td,
		) {
			return errNotFound(
				"thin device %q not found", req.GetTdName())
		}
		snapshots, err := tdUncreatedSnapshots(stm, sc,
			sc.Conf.GetTdNameList(), req.GetTdName(), td.GetDevId())
		if err != nil {
			return err
		}
		plan = tdDeletePlan{
			Cid:        sc.Cid,
			SpId:       sc.SpId(),
			SpRevision: sc.Rev.GetRevision(),
			Snapshots:  snapshots,
		}
		return nil
	})
	if err != nil {
		return tdDeletePlan{}, mapStmErr(err)
	}
	return plan, nil
}

// decideDeleteThinDevice is DeleteThinDevice's deciding STM: it verifies the
// plan against the SP's identity and revision, evaluates the three guards and
// deletes. A plan the pool has moved past is errCandidateChanged, which the
// candidate unit answers with a new plan (GW9) — unless the request carries a
// token the stored revision no longer matches: openSp's GW6 check runs first
// and refuses that ABORTED "stale revision".
//
// What it commits is bounded without the td count: the target td and one key
// per subsystem and per clone of the SP are its only reads beyond the fixed
// resolution and token keys, and the snapshots the plan named are re-read only
// on a path that refuses — with the SP and its revision unchanged each of them
// still blocks. gateway/txnbudget_test.go's TestDeleteThinDeviceBudget holds
// the arithmetic.
func decideDeleteThinDevice(
	ctx context.Context,
	cli *etcdutil.Client,
	req *pb.DeleteThinDeviceRequest,
	plan tdDeletePlan,
) (uint64, error) {
	var tdId uint64
	err := cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		tdId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		if sc.Cid != plan.Cid || sc.SpId() != plan.SpId ||
			sc.Rev.GetRevision() != plan.SpRevision {
			// The pool moved after the plan read it, possibly by a snapshot
			// of this very td that the plan's walk could not see, or the
			// name now resolves to a recreated SP whose SpRev happens to
			// stand at the plan's revision.
			return errCandidateChanged
		}
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName())
		td := &pb.ThinDevice{}
		if !stm.Get(tdKey, td) {
			return errNotFound(
				"thin device %q not found", req.GetTdName())
		}
		nqn, nsIdx, found, err := tdNamespaceRef(stm, sc, td.GetTdId())
		if err != nil {
			return err
		}
		if found {
			return errPrecondition(
				"thin device %s backs namespace %d of subsystem %s",
				req.GetTdName(), nsIdx, nqn)
		}
		cloneName, found, err := tdCloneRef(stm, sc, td.GetTdId())
		if err != nil {
			return err
		}
		if found {
			return errPrecondition(
				"thin device %s is the destination of clone %s",
				req.GetTdName(), cloneName)
		}
		snapshots, err := tdUncreatedSnapshots(stm, sc, plan.Snapshots,
			req.GetTdName(), td.GetDevId())
		if err != nil {
			return err
		}
		if len(snapshots) != 0 {
			blockers := make([]string, 0, len(snapshots))
			for _, name := range snapshots {
				blockers = append(blockers, fmt.Sprintf(
					msgSnapshotNotCreated, name, req.GetTdName()))
			}
			return errPrecondition("%s", strings.Join(blockers, "; "))
		}
		tdId = td.GetTdId()
		stm.Del(tdKey)
		sc.Conf.TdNameList = removeName(
			sc.Conf.GetTdNameList(), req.GetTdName())
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opDeleteThinDevice, sc)
	})
	if err != nil {
		return 0, err
	}
	return tdId, nil
}

// ListThinDevices is the ListThinDevices of architecture.md, Thin devices: one
// Snapshot over the SpConf and every td it lists, so the whole map is read at
// ONE store revision (EU4) and no caller can see a td that was created after
// another one it also sees.
//
// This is the documented client wait primitive for `created` (architecture.md,
// Thin devices): a client
// that wants to snapshot a td polls this RPC until the origin reads
// `created == true`, then calls CreateThinDevice, which is why the read must
// be consistent rather than a page of independent Gets. When a clone targets
// the origin, the client also waits until that clone is deleted and drained,
// GetClone answering NOT_FOUND, since CreateThinDevice refuses a clone's
// destination while the Clone key exists. This RPC is unpaged — the list is
// bounded by MaxTdCntPerSp — and takes no token, so it never bumps. Nor does
// it return one, while the flip it waits for bumps SpRev: a client whose
// CreateThinDevice carries a token reads it from GetStoragePool after its last
// poll, because a token read before the flip — or before a clone's latch and
// drain, which bump SpRev too — is stale.
//
// A listed key that is missing is an ABORTED (architecture.md,
// UNEXPECTED_ERROR → `ABORTED`) and never a short map: the
// wait primitive that silently omitted a td would read as "not created yet"
// for ever.
func (s *Server) ListThinDevices(
	ctx context.Context,
	req *pb.ListThinDevicesRequest,
) (*pb.ListThinDevicesReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var nameToTd map[string]*pb.ThinDevice
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		nameToTd = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		names := sc.Conf.GetTdNameList()
		listed := make(map[string]*pb.ThinDevice, len(names))
		for _, name := range names {
			key := model.ThinDeviceKey(sc.Cid, sc.SpId(), name)
			td := &pb.ThinDevice{}
			if !stm.Get(key, td) {
				return errAborted("thin device key %q is missing", key)
			}
			listed[name] = td
		}
		nameToTd = listed
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.ListThinDevicesReply{NameToTd: nameToTd}, nil
}

// tdNamespaceRef is the first delete guard of architecture.md, Thin devices:
// the first namespace of the SP whose td_id is tdId, as its subsystem NQN and
// ns_idx.
//
// The walk is nqn_list × ns_list, bounded by MaxSsCntPerSp × MaxNsCntPerSs =
// 16 embedded entries over at most 4 keys, which is why architecture.md, Thin
// devices, calls it cheap enough to run in the transaction. Namespaces live
// inside their Subsystem value, so there is nothing finer to read.
//
// A listed subsystem whose key is gone is an ABORTED (architecture.md,
// UNEXPECTED_ERROR → `ABORTED`): the guard cannot be
// evaluated, and a delete that cannot PROVE the td is unreferenced must not
// proceed — a namespace left pointing at a deleted td would park on the td's
// CnErrorName for ever.
func tdNamespaceRef(
	stm etcdutil.STM,
	sc *spScope,
	tdId uint64,
) (string, uint32, bool, error) {
	for _, nqn := range sc.Conf.GetNqnList() {
		key := model.SubsystemKey(sc.Cid, sc.SpId(), nqn)
		subsystem := &pb.Subsystem{}
		if !stm.Get(key, subsystem) {
			return "", 0, false,
				errAborted("subsystem key %q is missing", key)
		}
		for _, ns := range subsystem.GetNsList() {
			if ns.GetTdId() == tdId {
				return nqn, ns.GetNsIdx(), true, nil
			}
		}
	}
	return "", 0, false, nil
}

// tdCloneRef is the second delete guard of architecture.md, Thin devices, and
// the walk behind CreateThinDevice's refusal to snapshot a clone destination:
// the name of the first clone of the SP whose dst_td_id is tdId.
//
// A clone hydrates INTO its destination td, so deleting that td would leave a
// dm-clone copying into a device the pool no longer holds, and a snapshot of
// it would capture only the regions hydrated so far. The walk is
// clone_name_list, bounded by MaxCloneCntPerSp; a listed clone whose key is
// gone is ABORTED for the same reason as a missing subsystem.
func tdCloneRef(
	stm etcdutil.STM,
	sc *spScope,
	tdId uint64,
) (string, bool, error) {
	for _, name := range sc.Conf.GetCloneNameList() {
		key := model.CloneKey(sc.Cid, sc.SpId(), name)
		clone := &pb.Clone{}
		if !stm.Get(key, clone) {
			return "", false, errAborted("clone key %q is missing", key)
		}
		if clone.GetDstTdId() == tdId {
			return name, true, nil
		}
	}
	return "", false, nil
}

// tdUncreatedSnapshots is the third delete guard of architecture.md, Thin
// devices: the names, among names, of the snapshots of devId that have not
// materialized yet, in the order given.
//
// The match is on dev_id and not on td_id or name, which is what makes a
// same-name recreate safe: dev_ids are never reused, so a snapshot left over
// from an earlier td of this name resolves its ori_id to a dev_id nothing
// carries any more and can never block. A snapshot with `created == true` does
// not block either — dm-thin snapshots stay valid once taken, so only the
// window before `create_snap` has run in every slice is dangerous.
//
// Every blocker is reported rather than only the first: the operator's next
// step is to wait for all of them, and a one-at-a-time refusal would make that
// a guessing game. Reads are per key because an STM cannot range
// (architecture.md, Thin devices, allows the plan one range instead, but only
// at the store revision it read SpRev at). The plan passes the whole
// td_name_list — the ListThinDevices read set,
// bounded by MaxTdCntPerSp, which is why it is read in a Snapshot and never in
// the deciding transaction — and the deciding STM only the names the plan
// returned.
func tdUncreatedSnapshots(
	stm etcdutil.STM,
	sc *spScope,
	names []string,
	tdName string,
	devId uint32,
) ([]string, error) {
	var blockers []string
	for _, name := range names {
		if name == tdName {
			// The target cannot be its own origin, and its row is already in
			// the caller's hands.
			continue
		}
		key := model.ThinDeviceKey(sc.Cid, sc.SpId(), name)
		td := &pb.ThinDevice{}
		if !stm.Get(key, td) {
			return nil, errAborted("thin device key %q is missing", key)
		}
		if td.GetOriId() == devId && !td.GetCreated() {
			blockers = append(blockers, name)
		}
	}
	return blockers, nil
}
