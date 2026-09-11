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

// This file is architecture.md §8.7 / gateway.md §5.6: the three thin-device
// RPCs. All three are pure etcd — a thin device has no allocator footprint and
// no agent call, because the gateway only ever writes the DESIRED row and the
// primary cntlr builds the dm-thin volumes on the SpRev fan-out these bumps
// start (§3.3, §10.3).
//
// The one subtlety the whole file is built around is `ThinDevice.created`. The
// gateway always writes it false and never reads it back except as a gate: it
// is set true exactly once by the sp-worker, when a cntlr has reported that
// td's thin volume RES_STATUS_OK in EVERY slice of the SP. Snapshot creation
// is gated on it because `create_snap` has a kernel-level dependency on the
// origin's id already being in each slice pool, and origin deletion is gated
// on it because retire runs before build (cnagent.md CN9), so an origin
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

// msgOriginNotCreated is §8.7's normative refusal for a snapshot whose origin
// has not materialized. It is a format constant rather than an inline string
// because the integration suite greps the sentence, and because it is the one
// error in this file that also promises the client a way forward.
const msgOriginNotCreated = "origin %s is not created yet; " +
	"wait for ListThinDevices to report created = true"

// msgSnapshotNotCreated is §8.7's normative detail for the delete guard that
// names the blocking snapshot(s).
const msgSnapshotNotCreated = "snapshot %s of %s is not created yet"

// CreateThinDevice is architecture.md §8.7's CreateThinDevice.
//
// Only two of its §7 checks are pure and therefore run here (GW4): the names,
// and `size == 0`, which is legal exactly when `ori_name` is set because a
// snapshot then inherits the origin's size ([D-H]). The rest of the size rule
// — a positive multiple of `slice_cnt × stripe_size`, which is what lets
// dm-striped take equal, chunk-aligned members — depends on the SP's stored
// geometry and on the origin, so it is state-dependent and runs inside the
// transaction (gateway.md §5.6).
//
// The `created == false` refusal is the one place in this file that must write
// literally nothing: §8.7 requires no key, no next_id / next_dev_id
// consumption and no SpRev bump, so the check sits before the minter is ever
// created and returns straight out of the closure, which leaves the whole
// transaction uncommitted (EU4). Consuming an id there would be visible
// forever — ids are never reused — for a request that failed.
//
// The RPC never blocks on materialization (§5.8 keeps every RPC short); a
// client that wants to snapshot polls ListThinDevices instead.
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
		// Both halves are checked: the list is what every walk of §8.7 uses
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
				// Checked after NOT_FOUND (§8.7) and before anything is
				// minted, so the request is a no-op the client can retry
				// unchanged once the origin materializes. A snapshot OF a
				// snapshot follows the same rule: only the IMMEDIATE origin
				// is gated.
				return errPrecondition(
					msgOriginNotCreated, req.GetOriName())
			}
		}
		// A nil origin reads as 0 here, which is exactly the "no origin"
		// sentinel of ori_id and the "inherit nothing" case of size — and
		// the pure check above has already refused size 0 without an origin.
		size := req.GetSize()
		if size == 0 {
			size = origin.GetSize()
		}
		sliceCnt := spSliceCnt(sc.Conf)
		stripe := spStripeSize(sc.Conf)
		unit := uint64(sliceCnt) * stripe
		if unit == 0 {
			// An SP always has at least one slice; a slice_id_list that is
			// empty is a lost invariant, not a user error (§5.9).
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
			// Always false: materialization is the sp-worker's write (§10.3,
			// ThinDeviceCreated.md U2/U3), and this RPC has no way to know whether every slice
			// pool already holds the id.
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

// DeleteThinDevice is architecture.md §8.7's DeleteThinDevice.
//
// Its three FAILED_PRECONDITION guards are all evaluated from reads made
// inside the deleting transaction, which is what makes them race-proof: a
// CreateNamespace, a CreateClone or a CreateThinDevice snapshotting this td
// that commits concurrently touches a key this transaction read, so one of the
// two aborts and the client retries against the state that actually won
// (§5.8/§5.9). Checking them from a pre-read would leave exactly the window
// the guards exist to close.
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
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		tdId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
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
		blockers, err := tdUncreatedSnapshots(
			stm, sc, req.GetTdName(), td.GetDevId())
		if err != nil {
			return err
		}
		if len(blockers) != 0 {
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
		return nil, mapStmErr(err)
	}
	return &pb.DeleteThinDeviceReply{TdId: tdId}, nil
}

// ListThinDevices is architecture.md §8.7's ListThinDevices: one Snapshot over
// the SpConf and every td it lists, so the whole map is read at ONE store
// revision (EU4) and no caller can see a td that was created after another one
// it also sees.
//
// This is the documented client wait primitive for `created` (§8.7): a client
// that wants to snapshot a td polls this RPC until the origin reads
// `created == true`, then calls CreateThinDevice, which is why the read must
// be consistent rather than a page of independent Gets. It is unpaged — the
// list is bounded by MaxTdCntPerSp — and takes no token, so it never bumps.
//
// A listed key that is missing is §5.9's ABORTED and never a short map: the
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

// tdNamespaceRef is §8.7's first delete guard: the first namespace of the SP
// whose td_id is tdId, as its subsystem NQN and ns_idx.
//
// The walk is nqn_list × ns_list, bounded by MaxSsCntPerSp × MaxNsCntPerSs =
// 16 embedded entries over at most 4 keys, which is why §8.7 calls it cheap
// enough to run in the transaction. Namespaces live inside their Subsystem
// value, so there is nothing finer to read.
//
// A listed subsystem whose key is gone is §5.9's ABORTED: the guard cannot be
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

// tdCloneRef is §8.7's second delete guard: the name of the first clone of the
// SP whose dst_td_id is tdId.
//
// A clone hydrates INTO its destination td, so deleting that td would leave a
// dm-clone copying into a device the pool no longer holds. The walk is
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

// tdUncreatedSnapshots is §8.7's third delete guard: one normative sentence
// per snapshot of devId that has not materialized yet, in td_name_list order.
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
// a guessing game. Reads are per key because an STM cannot range (§8.7 allows
// either shape); they are the ListThinDevices read set, bounded by
// MaxTdCntPerSp, and being inside the transaction is what makes a snapshot
// created concurrently conflict this delete.
func tdUncreatedSnapshots(
	stm etcdutil.STM,
	sc *spScope,
	tdName string,
	devId uint32,
) ([]string, error) {
	var blockers []string
	for _, name := range sc.Conf.GetTdNameList() {
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
			blockers = append(blockers,
				fmt.Sprintf(msgSnapshotNotCreated, name, tdName))
		}
	}
	return blockers, nil
}
