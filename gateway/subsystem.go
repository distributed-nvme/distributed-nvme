package gateway

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.8 — subsystems, namespaces and the CdcEntry
// writes that go with them (gateway.md §5.7). All eight RPCs are pure etcd:
// nothing here reaches an agent, so every mutator is the plain
// resolve → token → precondition → mutate → bump shape of GW5/GW6 in one STM
// (GW8), and the one read-only RPC is a single Snapshot.
//
// A namespace is not a key of its own: it lives inside its Subsystem's value,
// which is why the three namespace updaters all end by writing the Subsystem
// key back. None of them touches the CdcEntry — an entry carries only the
// subsystem's NQN, its allowed hosts and the transports of the enabled cntlrs
// (§8.8), and no namespace field appears in the discovery log.

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

// subsystemModel is the `model` string every Subsystem carries [D2]. It is a
// constant and not derived from anything per-SP on purpose: hosts identify one
// device by (serial, model), so the model must stay identical across cntlrs
// and across the whole cluster.
const subsystemModel = "dnv"

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
// back — the namespace has no key of its own (§8.8).
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
// (§8.8 Defaults), in the canonical dashed lower-case form — which is the only
// form validateDevIdentity accepts, so a generated identity and a supplied one
// are indistinguishable once stored.
//
// crypto/rand and never math/rand: the uuid ends up in the host-visible
// namespace identify data, where a value another cluster can reproduce would
// let two unrelated namespaces claim the same identity. The version nibble and
// the variant bits are stamped after the draw, as RFC 4122 §4.4 requires; a
// reader that only pattern-matches would still accept an unstamped value, but
// a host that parses the version would not.
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
// (§8.8 Defaults). An NGUID has no version or variant structure, so the whole
// value is random.
func newDevNguid() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errAborted("dev_nguid generation failed: %v", err)
	}
	return fmt.Sprintf("%x", raw[:]), nil
}

// CreateSubsystem is architecture.md §8.8's CreateSubsystem.
//
// The NQN needs no discovery-NQN rejection: ValidNqnPattern demands a ':'
// after the domain part, which the well-known discovery NQN does not have, so
// §7's pattern already refuses it (validate.go).
//
// It writes three keys — the Subsystem, the SpConf whose nqn_list gained the
// name and the counter advanced, and the CdcEntry dnv-cdc will serve the
// discovery log from — plus the one SpRev bump they share (§5.5). The CdcEntry
// is written here and not left to dnv-cdc because the entry IS the desired
// state: it lists the transports of every ENABLED cntlr's CN, and a disabled
// cntlr is deliberately not advertised, its namespaces being ANA inaccessible.
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
		minter := newSpIdMinter(conf)
		ssId = minter.mint()
		stm.Put(ssKey, &pb.Subsystem{
			SsId: ssId,
			// serial = %016x(ss_id) and model = "dnv" [D2]: the pair is what
			// a host sees as the device identity, so it is derived from the
			// id that outlives every cntlr rather than from a cntlr.
			Serial:       fmt.Sprintf(common.IdKeyFmt, ssId),
			Model:        subsystemModel,
			AllowedHosts: req.GetAllowedHosts(),
		})
		conf.NqnList = append(conf.NqnList, req.GetNqn())
		stm.Put(
			model.CdcEntryKey(sc.Cid, sc.Shard(), sc.SpId(), ssId),
			&pb.CdcEntry{
				Nqn:            req.GetNqn(),
				NvmeTrConfList: enabledCntlrTrConfs(cntlrs),
				AllowedHosts:   req.GetAllowedHosts(),
			})
		minter.commit(conf)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), conf)
		return bumpSp(stm, opCreateSubsystem, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateSubsystemReply{SsId: ssId}, nil
}

// DeleteSubsystem is architecture.md §8.8's DeleteSubsystem.
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
	if err := validateNqn("nqn", req.GetNqn()); err != nil {
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

// ListSubsystems is architecture.md §8.8's ListSubsystems.
//
// It is a Snapshot and not a page range (§5.7, GW5): the reply is a whole map,
// bounded by MaxSsCntPerSp, and it must agree with the nqn_list it was read
// from — a client uses it to decide what to delete next. A listed NQN whose
// key is missing is therefore ABORTED and not a hole in the map: the SP has
// lost an invariant key, and answering with a partial map would hide it.
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

// UpdateSubsystemHosts is architecture.md §8.8's UpdateSubsystemHosts.
//
// The host list is stored twice on purpose and both copies move together: the
// Subsystem's copy is what every cntlr's nvmet allowed_hosts is built from,
// the CdcEntry's is what dnv-cdc filters its discovery log with. A CdcEntry
// that is not there yet is skipped rather than invented, exactly as in
// eachCdcEntry — only CreateSubsystem knows the rest of an entry's fields.
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
		subsystem.AllowedHosts = req.GetAllowedHosts()
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		entryKey := model.CdcEntryKey(sc.Cid, sc.Shard(), sc.SpId(), ssId)
		entry := &pb.CdcEntry{}
		if stm.Get(entryKey, entry) {
			entry.AllowedHosts = req.GetAllowedHosts()
			stm.Put(entryKey, entry)
		}
		return bumpSp(stm, opUpdateSubsystemHosts, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateSubsystemHostsReply{SsId: ssId}, nil
}

// CreateNamespace is architecture.md §8.8's CreateNamespace.
//
// ns_idx is the NVMe NSID the host will see, so it is the user's to choose and
// only two things can be wrong with it: 0, which NVMe reserves, and a value
// this subsystem already uses. Both are INVALID_ARGUMENT and not
// ALREADY_EXISTS — a namespace is not a key, it is a field of the subsystem,
// and §8.8 spells the code out.
//
// The td is resolved to its td_id here and the id is what is stored: renaming
// or recreating a thin device must never silently repoint a live namespace,
// and the id is never reused (§5.4).
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
		if req.GetNsIdx() == 0 {
			return errInvalid("ns_idx must not be 0")
		}
		if findNs(subsystem, req.GetNsIdx()) != nil {
			return errInvalid("ns_idx %d is already used by subsystem %q",
				req.GetNsIdx(), req.GetNqn())
		}
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName())
		td := &pb.ThinDevice{}
		if !stm.Get(tdKey, td) {
			return errNotFound("thin device %q not found", req.GetTdName())
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
		minter.commit(sc.Conf)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opCreateNamespace, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateNamespaceReply{NsId: nsId}, nil
}

// DeleteNamespace is architecture.md §8.8's DeleteNamespace.
//
// Only the Subsystem key changes: the ns_id is not returned to any counter
// (per-SP ids are never reused, §5.4), so the SpConf is untouched and the SP's
// single revision bump is the whole fan-out. The removed ns_id is the reply,
// which is why the namespace is located before it is dropped.
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

// UpdateNamespaceDev is architecture.md §8.8's UpdateNamespaceDev: it repoints
// a namespace at another thin device, which is how a snapshot is exposed in
// place of its origin.
//
// The switch is invisible to the host — every cntlr reloads the namespace's
// own dm-linear onto the new td's raid0 and the nvmet device_path never
// changes — so nothing here needs to touch the subsystem's identity, its
// allowed hosts or the CdcEntry.
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
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName())
		td := &pb.ThinDevice{}
		if !stm.Get(tdKey, td) {
			return errNotFound("thin device %q not found", req.GetTdName())
		}
		nsId = ns.GetNsId()
		ns.TdId = td.GetTdId()
		stm.Put(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetNqn()), subsystem)
		return bumpSp(stm, opUpdateNamespaceDev, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateNamespaceDevReply{NsId: nsId}, nil
}

// UpdateNamespaceSuspended is architecture.md §8.8's
// UpdateNamespaceSuspended: suspended namespaces have their device suspended
// on every cntlr and move to the inaccessible ANA group, which is what retires
// a namespace during the transfer/clone choreography of §11.3.
//
// It writes and bumps even when the stored flag already equals the requested
// one: the §0 #17 idempotent no-write applies to UpdateCntlrEnabled and the
// two Update*Disabled RPCs only, and the choreography relies on the bump
// reaching the cntlrs.
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
