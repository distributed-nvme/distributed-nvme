package gateway

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Subsystems, namespaces — subsystems,
// namespaces and the CdcEntry writes that go with them (gateway.md, Subsystems
// and namespaces). All eight RPCs are pure etcd:
// nothing here reaches an agent, so every mutator is the plain
// resolve → token → precondition → mutate → bump shape of GW5/GW6 in one STM
// (GW8), and the one read-only RPC is a single Snapshot.
//
// A namespace is not a key of its own: it lives inside its Subsystem's value,
// which is why the three namespace updaters all end by writing the Subsystem
// key back. None of them touches the CdcEntry — an entry carries only the
// subsystem's NQN, its allowed hosts and the transports of the cntlrs the
// listing rule lists (architecture.md [D18]), and no namespace field appears
// in the discovery log.

// The op names the bump helper records, one per mutator so a log line names
// something greppable. ListSubsystems has none: it never bumps.
const (
	opCreateSubsystem          = "CreateSubsystem"
	opDeleteSubsystem          = "DeleteSubsystem"
	opUpdateSubsystemHosts     = "UpdateSubsystemHosts"
	opCreateNamespace          = "CreateNamespace"
	opDeleteNamespace          = "DeleteNamespace"
	opUpdateNamespaceDev       = "UpdateNamespaceDev"
	opUpdateNamespaceSuspended = "UpdateNamespaceSuspended"
)

// subsystemByNqn reads one subsystem of the resolved SP, which is what every
// RPC of this file but CreateSubsystem starts from. An absent key is
// NOT_FOUND (GW7's "named object absent"): unlike eachCdcEntry's walk, the NQN
// here comes from the request and not from nqn_list, so a miss is a user error
// and never a lost invariant.
func subsystemByNqn(
	stm etcdutil.STM,
	sc *spScope,
	nqn string,
) (*pb.Subsystem, error) {
	subsystem := &pb.Subsystem{}
	if !stm.Get(model.SubsystemKey(sc.Cid, sc.SpId(), nqn), subsystem) {
		return nil, errNotFound("subsystem %q not found", nqn)
	}
	return subsystem, nil
}

// namespaceByIdx locates one namespace of one subsystem, the opening the three
// namespace updaters share. The returned Namespace points into the returned
// Subsystem's ns_list, so a caller mutates it and then writes the subsystem
// back — the namespace has no key of its own (architecture.md, Subsystems,
// namespaces).
func namespaceByIdx(
	stm etcdutil.STM,
	sc *spScope,
	nqn string,
	nsIdx uint32,
) (*pb.Subsystem, *pb.Namespace, error) {
	subsystem, err := subsystemByNqn(stm, sc, nqn)
	if err != nil {
		return nil, nil, err
	}
	ns := findNs(subsystem, nsIdx)
	if ns == nil {
		return nil, nil, errNotFound(
			"subsystem %q has no namespace with ns_idx %d", nqn, nsIdx)
	}
	return subsystem, ns, nil
}

// newDevUuid mints the RFC 4122 version 4 uuid an empty dev_uuid defaults to
// (architecture.md, Subsystems, namespaces: CreateNamespace's Defaults), in
// the canonical dashed lower-case form — which is the only
// form validateDevIdentity accepts, so a generated identity and a supplied one
// are indistinguishable once stored.
//
// crypto/rand and never math/rand: the uuid ends up in the host-visible
// namespace identify data, where a value another cluster can reproduce would
// let two unrelated namespaces claim the same identity. The version nibble and
// the variant bits are stamped after the draw, as RFC 4122 section 4.4
// requires; a reader that only pattern-matches would still accept an unstamped
// value, but a host that parses the version would not.
func newDevUuid() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errAborted("dev_uuid generation failed: %v", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// newDevNguid mints the 16 random bytes an empty dev_nguid defaults to,
// rendered as the 32 lower-case hex characters validateDevIdentity accepts
// (architecture.md, Subsystems, namespaces: CreateNamespace's Defaults). An
// NGUID has no version or variant structure, so the whole
// value is random.
func newDevNguid() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errAborted("dev_nguid generation failed: %v", err)
	}
	return fmt.Sprintf("%x", raw[:]), nil
}

// sharingPoolNames are the names of the storage pools of the cluster but
// spName whose nqn_list holds nqn, read in one range of the cluster's pool
// records, with their values, outside any transaction, so that
// CreateSubsystem's transaction reads only those pools again
// (sharedNqnCheck) and its reads do not grow with the pool count, which the
// transaction budget bounds (architecture.md, Common validation). A pool
// that takes the NQN after the range is met by the cn agent's own refusal
// (cnagent.md CN16, A shared NQN).
func (s *Server) sharingPoolNames(
	ctx context.Context,
	clusterName string,
	spName string,
	nqn string,
) ([]string, error) {
	var cid uint64
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		resolved, _, err := resolveCluster(stm, clusterName)
		cid = resolved
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	prefix := model.SpConfPrefix(cid)
	kvs, _, err := s.cli.Range(ctx, prefix)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	var names []string
	for _, kv := range kvs {
		name := strings.TrimPrefix(kv.Key, prefix)
		if name == spName {
			continue
		}
		conf := &pb.SpConf{}
		if err := s.cli.Decode(ctx, kv, conf); err != nil {
			return nil, errAborted("%v", err)
		}
		if containsName(conf.GetNqnList(), nqn) {
			names = append(names, name)
		}
	}
	return names, nil
}

// sharedNqnCheck is CreateSubsystem's check of a shared NQN
// (architecture.md, Subsystems, namespaces): an NQN another pool of the
// cluster holds a subsystem of is refused FAILED_PRECONDITION, naming that
// pool, unless the request sets share_nqn, as the destination pool of a
// move does; with it, a controller node that holds a cntlr of both pools is
// refused, naming the node, as the kernel holds one subsystem per NQN, no
// cntlr of such a node changes it, and the pool whose cntlr did not build
// it is not served there (cnagent.md CN16, A shared NQN). others are the
// pools the range found holding the NQN (sharingPoolNames), at most one, as
// the caller refuses more; its record is read again in the caller's
// transaction, and a pool deleted since, or that let the NQN go, is skipped.
// A pool that takes the NQN after the range is not seen: two creates of one
// NQN in two pools at once can both commit, and the cn agent's rule is then
// what keeps either pool's volume from the other's hosts.
func sharedNqnCheck(
	stm etcdutil.STM,
	cid uint64,
	spName string,
	own []*pb.Cntlr,
	others []string,
	nqn string,
	share bool,
) error {
	ownCns := make(map[string]bool, len(own))
	for _, cntlr := range own {
		ownCns[cntlr.GetAddrPort()] = true
	}
	for _, name := range others {
		conf := &pb.SpConf{}
		if !stm.Get(model.SpConfKey(cid, name), conf) ||
			!containsName(conf.GetNqnList(), nqn) {
			continue
		}
		if !share {
			return errPrecondition("subsystem %q is a subsystem of storage "+
				"pool %q too; set share_nqn to make the pair of a move",
				nqn, name)
		}
		theirs, err := loadCntlrs(stm, cid, conf)
		if err != nil {
			return err
		}
		for _, cntlr := range theirs {
			if ownCns[cntlr.GetAddrPort()] {
				return errPrecondition("controller node %s holds a cntlr "+
					"of storage pool %q and of storage pool %q, which "+
					"would share subsystem %q, and on such a node no "+
					"cntlr changes it and one pool is not served; move "+
					"one of the cntlrs first",
					cntlr.GetAddrPort(), spName, name, nqn)
			}
		}
	}
	return nil
}

// CreateSubsystem is the CreateSubsystem of architecture.md, Subsystems,
// namespaces.
//
// The NQN needs no discovery-NQN rejection: ValidNqnPattern demands a ':'
// after the domain part, which the well-known discovery NQN does not have, so
// the pattern of architecture.md, Common validation, already refuses it
// (validate.go). Unlike a host NQN, it must also lie outside the dnv namespace
// (validateHostFacingNqn).
//
// It writes three keys — the Subsystem, the SpConf whose nqn_list gained the
// name and the counter advanced, and the CdcEntry dnv-cdc will serve the
// discovery log from — plus the one SpRev bump they share (architecture.md,
// Revision keys and the sync fan-out). The CdcEntry
// is written here and not left to dnv-cdc because the entry IS the desired
// state: it lists the transports of the cntlrs the listing rule lists
// (architecture.md [D18]) — the primary and every enabled standby with a zero
// err_epoch — and a disabled cntlr is deliberately not advertised, its
// namespaces being ANA inaccessible.
func (s *Server) CreateSubsystem(
	ctx context.Context,
	req *pb.CreateSubsystemRequest,
) (*pb.CreateSubsystemReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateHostFacingNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	if err := validateHosts(
		"allowed_hosts", req.GetAllowedHosts(),
	); err != nil {
		return nil, err
	}
	others, err := s.sharingPoolNames(ctx, req.GetClusterName(),
		req.GetSpName(), req.GetNqn())
	if err != nil {
		return nil, err
	}
	// At most two pools hold one NQN, the pair of a move, so the STM reads
	// one other pool and its cntlrs at most (TestCreateSubsystemTxnBudget).
	if len(others) > 1 {
		return nil, errPrecondition("subsystem %q is a subsystem of "+
			"storage pools %q and %q already, and at most two pools hold "+
			"one NQN", req.GetNqn(), others[0], others[1])
	}
	var ssId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		ssId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		conf := sc.Conf
		if len(conf.GetNqnList()) >= common.MaxSsCntPerSp {
			return errExhausted(
				"storage pool %q holds %d subsystems, the maximum is %d",
				req.GetSpName(), len(conf.GetNqnList()),
				common.MaxSsCntPerSp)
		}
		ssKey := model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn())
		if containsName(conf.GetNqnList(), req.GetNqn()) ||
			stm.Get(ssKey, &pb.Subsystem{}) {
			return errExists("subsystem %q already exists", req.GetNqn())
		}
		// Read before the first write, so a lost cntlr key refuses the RPC
		// rather than aborting it half-written: an error out of the closure
		// discards the whole transaction, but the ordering is what makes that
		// true by construction rather than by trusting EU4.
		cntlrs, err := loadCntlrs(stm, sc.Cid, conf)
		if err != nil {
			return err
		}
		if err := sharedNqnCheck(stm, sc.Cid, req.GetSpName(), cntlrs,
			others, req.GetNqn(), req.GetShareNqn()); err != nil {
			return err
		}
		minter := newSpIdMinter(conf)
		ssId = minter.mint()
		stm.Put(ssKey, &pb.Subsystem{
			SsId: ssId,
			// serial = %016x(ss_id) and model = common.NvmeModel [D2]: the
			// pair is what a host sees as the device identity, so it is
			// derived from the id that outlives every cntlr rather than from
			// a cntlr.
			Serial:       fmt.Sprintf(common.IdKeyFmt, ssId),
			Model:        common.NvmeModel,
			AllowedHosts: req.GetAllowedHosts(),
		})
		conf.NqnList = append(conf.NqnList, req.GetNqn())
		stm.Put(
			model.CdcEntryKey(sc.Cid, sc.Shard(), sc.SpId(), ssId),
			&pb.CdcEntry{
				Nqn:            req.GetNqn(),
				NvmeTrConfList: model.CdcTrConfList(cntlrs),
				AllowedHosts:   req.GetAllowedHosts(),
			})
		if err := minter.commit(conf, opCreateSubsystem); err != nil {
			return err
		}
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), conf)
		return bumpSp(stm, opCreateSubsystem, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateSubsystemReply{SsId: ssId}, nil
}

// DeleteSubsystem is the DeleteSubsystem of architecture.md, Subsystems,
// namespaces.
//
// Namespaces block the deletion, allowed hosts never do: a host entry is a
// permission, and removing the subsystem removes the permission with it, while
// a namespace is a device a host may still be using. The CdcEntry goes in the
// same transaction as the Subsystem — leaving it behind would keep dnv-cdc
// advertising a subsystem no cntlr serves, and hosts running nvme-stas would
// keep reconnecting to it.
func (s *Server) DeleteSubsystem(
	ctx context.Context,
	req *pb.DeleteSubsystemRequest,
) (*pb.DeleteSubsystemReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateExistingNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	var ssId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		ssId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		subsystem, err := subsystemByNqn(stm, sc, req.GetNqn())
		if err != nil {
			return err
		}
		if len(subsystem.GetNsList()) != 0 {
			return errPrecondition(
				"subsystem %q still holds %d namespaces",
				req.GetNqn(), len(subsystem.GetNsList()))
		}
		ssId = subsystem.GetSsId()
		stm.Del(model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()))
		stm.Del(model.CdcEntryKey(sc.Cid, sc.Shard(), sc.SpId(), ssId))
		sc.Conf.NqnList = removeName(sc.Conf.GetNqnList(), req.GetNqn())
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opDeleteSubsystem, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteSubsystemReply{SsId: ssId}, nil
}

// ListSubsystems is the ListSubsystems of architecture.md, Subsystems,
// namespaces.
//
// It is a Snapshot and not a page range (architecture.md, page_token; GW5):
// the reply is a whole map, bounded by MaxSsCntPerSp, and it must agree with
// the nqn_list it was read from — a client uses it to decide what to delete
// next. A listed NQN whose key is missing is therefore ABORTED and not a hole
// in the map: the SP has lost an invariant key, and answering with a partial
// map would hide it.
func (s *Server) ListSubsystems(
	ctx context.Context,
	req *pb.ListSubsystemsRequest,
) (*pb.ListSubsystemsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var nqnToSubsystem map[string]*pb.Subsystem
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		nqnToSubsystem = make(map[string]*pb.Subsystem)
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		for _, nqn := range sc.Conf.GetNqnList() {
			key := model.SubsystemKey(sc.Cid, sc.SpId(), nqn)
			subsystem := &pb.Subsystem{}
			if !stm.Get(key, subsystem) {
				return errAborted("subsystem key %q is missing", key)
			}
			nqnToSubsystem[nqn] = subsystem
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.ListSubsystemsReply{NqnToSubsystem: nqnToSubsystem}, nil
}

// UpdateSubsystemHosts is the UpdateSubsystemHosts of architecture.md,
// Subsystems, namespaces.
//
// The host list is stored twice on purpose and both copies move together: the
// Subsystem's copy is what every cntlr's nvmet allowed_hosts is built from,
// the CdcEntry's is what dnv-cdc filters its discovery log with. A CdcEntry
// whose key is missing is rebuilt (rebuildCdcEntry) and written with the new
// hosts, exactly as eachCdcEntry does for the cntlr mutators: skipping it
// would answer OK while the subsystem stayed out of dnv-cdc's discovery log
// for every host, the newly allowed ones included (dnv-cdc drops an entry
// whose key is deleted, cdc.md WV3). An entry that exists gets its
// nvme_tr_conf_list set by the listing rule as well (architecture.md
// [D18]): every writer of an entry lists by the one rule.
// The entry is read, and rebuilt if missing, before the first write, as
// CreateSubsystem reads its cntlrs: a lost cntlr key then refuses the RPC
// before anything is staged.
func (s *Server) UpdateSubsystemHosts(
	ctx context.Context,
	req *pb.UpdateSubsystemHostsRequest,
) (*pb.UpdateSubsystemHostsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	if err := validateHosts(
		"allowed_hosts", req.GetAllowedHosts(),
	); err != nil {
		return nil, err
	}
	var ssId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		ssId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		subsystem, err := subsystemByNqn(stm, sc, req.GetNqn())
		if err != nil {
			return err
		}
		ssId = subsystem.GetSsId()
		entryKey := model.CdcEntryKey(sc.Cid, sc.Shard(), sc.SpId(), ssId)
		entry := &pb.CdcEntry{}
		if !stm.Get(entryKey, entry) {
			entry, err = rebuildCdcEntry(stm, sc, req.GetNqn(), subsystem)
			if err != nil {
				return err
			}
		} else {
			// Every writer of an entry lists by the one rule ([D18]).
			cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
			if err != nil {
				return err
			}
			entry.NvmeTrConfList = model.CdcTrConfList(cntlrs)
		}
		subsystem.AllowedHosts = req.GetAllowedHosts()
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		entry.AllowedHosts = req.GetAllowedHosts()
		stm.Put(entryKey, entry)
		return bumpSp(stm, opUpdateSubsystemHosts, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateSubsystemHostsReply{SsId: ssId}, nil
}

// CreateNamespace is the CreateNamespace of architecture.md, Subsystems,
// namespaces.
//
// ns_idx is the NVMe NSID the host will see, so it is the user's to choose and
// only three things can be wrong with it: 0 and common.NvmeNsidAll, which
// NVMe reserves and nvmet makes no namespace of, and a value this subsystem
// already uses. All three are INVALID_ARGUMENT and not ALREADY_EXISTS — a
// namespace is not a key, it is a field of the subsystem, and
// architecture.md, Subsystems, namespaces, spells the code out.
//
// The td is resolved to its td_id here and the id is what is stored: renaming
// or recreating a thin device must never silently repoint a live namespace,
// and the id is never reused (architecture.md, Globals: id allocation + shard
// buckets).
//
// Both generated identities are minted INSIDE the closure (GW8): they escape
// the transaction into stored state, so a retried attempt must produce its own
// values rather than reuse the ones an aborted attempt drew.
func (s *Server) CreateNamespace(
	ctx context.Context,
	req *pb.CreateNamespaceRequest,
) (*pb.CreateNamespaceReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	if err := validateName("td_name", req.GetTdName()); err != nil {
		return nil, err
	}
	if err := validateDevIdentity(
		req.GetDevUuid(), req.GetDevNguid(),
	); err != nil {
		return nil, err
	}
	// NVMe reserves NSID 0 and common.NvmeNsidAll, which addresses every
	// namespace. The request alone shows it, so it is refused with the rest
	// of the validation, before any read (GW4): a full subsystem must not
	// answer it with its namespace ceiling.
	if req.GetNsIdx() == 0 {
		return nil, errInvalid("ns_idx must not be 0")
	}
	if req.GetNsIdx() == common.NvmeNsidAll {
		return nil, errInvalid("ns_idx must not be %#x, which names every "+
			"namespace", uint32(common.NvmeNsidAll))
	}
	var nsId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		nsId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		subsystem, err := subsystemByNqn(stm, sc, req.GetNqn())
		if err != nil {
			return err
		}
		if len(subsystem.GetNsList()) >= common.MaxNsCntPerSs {
			return errExhausted(
				"subsystem %q holds %d namespaces, the maximum is %d",
				req.GetNqn(), len(subsystem.GetNsList()),
				common.MaxNsCntPerSs)
		}
		if findNs(subsystem, req.GetNsIdx()) != nil {
			return errInvalid("ns_idx %d is already used by subsystem %q",
				req.GetNsIdx(), req.GetNqn())
		}
		xferName, found, err := nsTransferRef(
			stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		if found {
			return errNsInTransfer(req.GetNqn(), req.GetNsIdx(), xferName)
		}
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName())
		td := &pb.ThinDevice{}
		if !stm.Get(tdKey, td) {
			return errNotFound("thin device %q not found", req.GetTdName())
		}
		if xferName, found, err := tdTransferRef(
			stm, sc, td.GetTdId()); err != nil {
			return err
		} else if found {
			return errTdOfTransfer(req.GetTdName(), xferName)
		}
		devUuid := req.GetDevUuid()
		if devUuid == "" {
			if devUuid, err = newDevUuid(); err != nil {
				return err
			}
		}
		devNguid := req.GetDevNguid()
		if devNguid == "" {
			if devNguid, err = newDevNguid(); err != nil {
				return err
			}
		}
		minter := newSpIdMinter(sc.Conf)
		nsId = minter.mint()
		subsystem.NsList = append(subsystem.GetNsList(), &pb.Namespace{
			NsId:      nsId,
			NsIdx:     req.GetNsIdx(),
			TdId:      td.GetTdId(),
			DevUuid:   devUuid,
			DevNguid:  devNguid,
			Suspended: req.GetSuspended(),
		})
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		if err := minter.commit(sc.Conf, opCreateNamespace); err != nil {
			return err
		}
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opCreateNamespace, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateNamespaceReply{NsId: nsId}, nil
}

// DeleteNamespace is the DeleteNamespace of architecture.md, Subsystems,
// namespaces.
//
// Only the Subsystem key changes: the ns_id is not returned to any counter
// (per-SP ids are never reused, architecture.md, Globals: id allocation +
// shard buckets), so the SpConf is untouched and the SP's
// single revision bump is the whole fan-out. The removed ns_id is the reply,
// which is why the namespace is located before it is dropped. A live
// transfer's origin is refused (nsTransferRef).
func (s *Server) DeleteNamespace(
	ctx context.Context,
	req *pb.DeleteNamespaceRequest,
) (*pb.DeleteNamespaceReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateExistingNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	var nsId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		nsId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		subsystem, ns, err := namespaceByIdx(
			stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		xferName, found, err := nsTransferRef(
			stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		if found {
			return errNsInTransfer(req.GetNqn(), req.GetNsIdx(), xferName)
		}
		nsId = ns.GetNsId()
		kept := make([]*pb.Namespace, 0, len(subsystem.GetNsList()))
		for _, item := range subsystem.GetNsList() {
			if item.GetNsIdx() == req.GetNsIdx() {
				continue
			}
			kept = append(kept, item)
		}
		subsystem.NsList = kept
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		return bumpSp(stm, opDeleteNamespace, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteNamespaceReply{NsId: nsId}, nil
}

// UpdateNamespaceDev is the UpdateNamespaceDev of architecture.md, Subsystems,
// namespaces: it repoints a namespace at another thin device, which is how a
// snapshot is exposed in place of its origin.
//
// The switch is invisible to the host only at one size — every cntlr reloads
// the namespace's own dm-linear onto the new td's raid0 and the nvmet
// device_path never changes, so nvmet is not told of a new size and the host
// keeps the one it read — so a td of another size than the one the namespace
// points at is FAILED_PRECONDITION naming both: a smaller one would fail the
// host's IO past its end. Nothing here touches the subsystem's identity, its
// allowed hosts or the CdcEntry. A live transfer's origin is refused
// (nsTransferRef): the primary's xfer stack maps the td it points at where
// it serves the transfer.
//
// The namespace holds only its td's td_id, and no key names a td by id, so
// the td it points at is found by a walk of td_name_list. That walk is the
// PLAN, a read-only Snapshot, run in the candidate unit's loop as
// DeleteThinDevice's is (GW9): a deciding STM that walked the list would
// compare every td it read and pass EtcdMaxTxnOps toward MaxTdCntPerSp, and
// the namespace could then never be repointed. The deciding STM reads the one
// td the plan found, after checking that the namespace still points at it; a
// namespace repointed since is candidate changed, and the unit plans again.
// TestUpdateNamespaceDevTxnBudget holds the arithmetic.
func (s *Server) UpdateNamespaceDev(
	ctx context.Context,
	req *pb.UpdateNamespaceDevRequest,
) (*pb.UpdateNamespaceDevReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	if err := validateName("td_name", req.GetTdName()); err != nil {
		return nil, err
	}
	var nsId uint64
	err := candidateUnit(ctx, func() error {
		nsId = 0
		plan, err := planUpdateNamespaceDev(ctx, s.cli, req)
		if err != nil {
			return err
		}
		nsId, err = decideUpdateNamespaceDev(ctx, s.cli, req, plan)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateNamespaceDevReply{NsId: nsId}, nil
}

// nsDevPlan is what UpdateNamespaceDev's plan hands its deciding STM: the SP
// incarnation it read, and the td the namespace pointed at, by id and name.
type nsDevPlan struct {
	Cid       uint64
	SpId      uint64
	CurTdId   uint64
	CurTdName string
}

// planUpdateNamespaceDev is UpdateNamespaceDev's plan: one read-only Snapshot
// that opens with openSp, so a stale token is ABORTED before any other state
// check (GW6), locates the namespace, NOT_FOUND as in the deciding STM, and
// walks td_name_list for the td whose td_id the namespace holds (tdNameById).
// None is a lost invariant, ABORTED: DeleteThinDevice refuses a td a
// namespace points at.
func planUpdateNamespaceDev(
	ctx context.Context,
	cli *etcdutil.Client,
	req *pb.UpdateNamespaceDevRequest,
) (nsDevPlan, error) {
	var plan nsDevPlan
	err := cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		plan = nsDevPlan{}
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		_, ns, err := namespaceByIdx(stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		name, found, err := tdNameById(stm, sc, ns.GetTdId())
		if err != nil {
			return err
		}
		if !found {
			return errAborted("namespace %d of subsystem %s points at "+
				"td_id %d, which no thin device of the pool holds",
				req.GetNsIdx(), req.GetNqn(), ns.GetTdId())
		}
		plan = nsDevPlan{
			Cid:       sc.Cid,
			SpId:      sc.SpId(),
			CurTdId:   ns.GetTdId(),
			CurTdName: name,
		}
		return nil
	})
	if err != nil {
		return nsDevPlan{}, mapStmErr(err)
	}
	return plan, nil
}

// decideUpdateNamespaceDev is UpdateNamespaceDev's deciding STM. A plan read
// of another SP incarnation, or a namespace that no longer points at the td
// the plan found, is errCandidateChanged, which the candidate unit answers
// with a new plan (GW9); a request that carries a token meets openSp's GW6
// check first, since every repoint bumps SpRev. The td the plan found is read
// by its name, and must still hold the plan's td_id: td ids are never reused
// and a td a namespace points at is never deleted, so another one there is a
// lost invariant, ABORTED. Its size never changes, which is what lets the
// size rule read it once here.
func decideUpdateNamespaceDev(
	ctx context.Context,
	cli *etcdutil.Client,
	req *pb.UpdateNamespaceDevRequest,
	plan nsDevPlan,
) (uint64, error) {
	var nsId uint64
	err := cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		nsId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		if sc.Cid != plan.Cid || sc.SpId() != plan.SpId {
			return errCandidateChanged
		}
		subsystem, ns, err := namespaceByIdx(
			stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		xferName, found, err := nsTransferRef(
			stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		if found {
			return errNsInTransfer(req.GetNqn(), req.GetNsIdx(), xferName)
		}
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName())
		td := &pb.ThinDevice{}
		if !stm.Get(tdKey, td) {
			return errNotFound("thin device %q not found", req.GetTdName())
		}
		if xferName, found, err := tdTransferRef(
			stm, sc, td.GetTdId()); err != nil {
			return err
		} else if found {
			return errTdOfTransfer(req.GetTdName(), xferName)
		}
		if ns.GetTdId() != plan.CurTdId {
			return errCandidateChanged
		}
		curKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), plan.CurTdName)
		cur := &pb.ThinDevice{}
		if !stm.Get(curKey, cur) || cur.GetTdId() != plan.CurTdId {
			return errAborted("thin device key %q no longer holds td_id "+
				"%d, which namespace %d of subsystem %s points at",
				curKey, plan.CurTdId, req.GetNsIdx(), req.GetNqn())
		}
		if td.GetSize() != cur.GetSize() {
			return errPrecondition(
				"thin device %s is %d bytes and namespace %d of subsystem "+
					"%s is on %s, of %d bytes: a namespace moves only to a "+
					"thin device of its own size, since its hosts keep the "+
					"size they read",
				req.GetTdName(), td.GetSize(), req.GetNsIdx(),
				req.GetNqn(), plan.CurTdName, cur.GetSize())
		}
		nsId = ns.GetNsId()
		ns.TdId = td.GetTdId()
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		return bumpSp(stm, opUpdateNamespaceDev, sc)
	})
	if err != nil {
		return 0, err
	}
	return nsId, nil
}

// UpdateNamespaceSuspended is the UpdateNamespaceSuspended of architecture.md,
// Subsystems, namespaces: a suspended namespace has its `CnNsDevName`
// **parked** on the td's dm-error on every cntlr — live, never dm-suspended
// (architecture.md, Namespace suspend semantics; `cnagent.md` CN16 rule 1) —
// and moves to the inaccessible ANA group, which is what retires a namespace
// during the transfer/clone choreography of architecture.md, Transfer + clone
// = cross-SP live migration.
//
// It writes and bumps even when the stored flag already equals the requested
// one: this RPC is not among GW6's no-writes, and the choreography relies on
// the bump reaching the cntlrs.
func (s *Server) UpdateNamespaceSuspended(
	ctx context.Context,
	req *pb.UpdateNamespaceSuspendedRequest,
) (*pb.UpdateNamespaceSuspendedReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateNqn("nqn", req.GetNqn()); err != nil {
		return nil, err
	}
	var nsId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		nsId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		subsystem, ns, err := namespaceByIdx(
			stm, sc, req.GetNqn(), req.GetNsIdx())
		if err != nil {
			return err
		}
		nsId = ns.GetNsId()
		ns.Suspended = req.GetSuspended()
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		return bumpSp(stm, opUpdateNamespaceSuspended, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateNamespaceSuspendedReply{NsId: nsId}, nil
}
