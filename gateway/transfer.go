package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.10 / gateway.md §5.9: the source half of the
// §11.3 cross-SP live migration. A Transfer is a pure etcd record — it names
// an existing namespace of this SP and the hosts allowed to reach it through
// the xfer subsystem — so all four RPCs are the standard mutator/reader shape
// with no agent call anywhere (AG1: the cntlrs learn about the record from the
// SpRev bump and build the stack themselves).

// The op names the bump helper cites in its error messages; they are the RPC
// names so a log line names something greppable (gateway.md §8).
const (
	opCreateTransfer      = "CreateTransfer"
	opDeleteTransfer      = "DeleteTransfer"
	opUpdateTransferHosts = "UpdateTransferHosts"
)

// resolveTransfer reads one Transfer of the SP and returns its key alongside
// it, because every caller that finds the record also rewrites or deletes that
// exact key (GW5: an id-addressed or named object that is absent is
// NOT_FOUND).
//
// The record is looked up by key rather than through SpConf.xfer_name_list:
// the key is what the RPC is about to act on, so reading it is both the
// existence check and the load, and no second read can disagree with it.
func resolveTransfer(
	stm etcdutil.STM,
	sc *spScope,
	xferName string,
) (string, *pb.Transfer, error) {
	key := model.TransferKey(sc.Cid, sc.SpId(), xferName)
	xfer := &pb.Transfer{}
	if !stm.Get(key, xfer) {
		return "", nil, errNotFound("transfer %q not found", xferName)
	}
	return key, xfer, nil
}

// CreateTransfer is architecture.md §8.10's CreateTransfer: it retires an
// existing namespace of this SP behind an xfer subsystem so a destination SP's
// clone can read the bytes over NVMe-oF (§11.3).
//
// The origin is resolved — the Subsystem by ori_nqn and the Namespace by
// ori_ns_idx inside it — inside the STM and NOT_FOUND when either is absent:
// the record is a pointer into the SP's own subsystem table, and a transfer
// pointing at a namespace that does not exist would make every cntlr build a
// stack over nothing.
//
// allowed_hosts carries the destination cntlrs' CnHostNqns, which is why it is
// validated with the §7 host-NQN rules and stored verbatim; nothing here
// touches a CdcEntry, because the xfer subsystem is reached by the destination
// through the transport addresses its operator already knows and is never
// advertised as a dnv discovery subsystem (§5.9).
func (s *Server) CreateTransfer(
	ctx context.Context,
	req *pb.CreateTransferRequest,
) (*pb.CreateTransferReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("xfer_name", req.GetXferName()); err != nil {
		return nil, err
	}
	if err := validateNqn("ori_nqn", req.GetOriNqn()); err != nil {
		return nil, err
	}
	if err := validateHosts(
		"allowed_hosts", req.GetAllowedHosts(),
	); err != nil {
		return nil, err
	}
	var xferId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		// Reassigned on every attempt: the id is minted from the next_id this
		// attempt read, so a retry that reads a different next_id must not
		// reply the previous attempt's value (GW8).
		xferId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		conf := sc.Conf
		if len(conf.GetXferNameList()) >= common.MaxXferCntPerSp {
			return errExhausted(
				"transfer count %d has reached the per-storage-pool "+
					"maximum %d",
				len(conf.GetXferNameList()), common.MaxXferCntPerSp)
		}
		key := model.TransferKey(sc.Cid, sc.SpId(), req.GetXferName())
		// Both halves of the name's identity are checked: the list is what
		// bounds the SP and what DeleteTransfer maintains, the key is what
		// this RPC would overwrite. Only one of them can be stale — never
		// both, since they are always written in one STM — so refusing on
		// either is what keeps a create from clobbering a live record.
		if containsName(conf.GetXferNameList(), req.GetXferName()) ||
			stm.Get(key, &pb.Transfer{}) {
			return errExists("transfer %q already exists", req.GetXferName())
		}
		subsystem := &pb.Subsystem{}
		if !stm.Get(
			model.SubsystemKey(sc.Cid, sc.SpId(), req.GetOriNqn()),
			subsystem,
		) {
			return errNotFound("subsystem %q not found", req.GetOriNqn())
		}
		if findNs(subsystem, req.GetOriNsIdx()) == nil {
			return errNotFound("namespace %d not found in subsystem %q",
				req.GetOriNsIdx(), req.GetOriNqn())
		}
		minter := newSpIdMinter(conf)
		xferId = minter.mint()
		stm.Put(key, &pb.Transfer{
			XferId:       xferId,
			OriNqn:       req.GetOriNqn(),
			OriNsIdx:     req.GetOriNsIdx(),
			AllowedHosts: req.GetAllowedHosts(),
			AutoSuspend:  req.GetAutoSuspend(),
		})
		conf.XferNameList = append(conf.GetXferNameList(), req.GetXferName())
		minter.commit(conf)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), conf)
		return bumpSp(stm, opCreateTransfer, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateTransferReply{XferId: xferId}, nil
}

// DeleteTransfer is architecture.md §8.10's DeleteTransfer, whose `force` flag
// picks between the two ways a hand-over ends.
//
// force == false FINALIZES a completed copy: it additionally sets
// suspended = true on the origin namespace in the same STM, so the source
// stays retired once the Transfer record — the only other thing keeping the
// origin's dm device suspended and its ns ANA inaccessible — is gone. Doing it
// in this transaction and not in a follow-up UpdateNamespaceSuspended is what
// closes the window in which the next syncup would resume the source while the
// destination is already serving the same nguid.
//
// force == true ABORTS: the origin is left untouched with suspended still
// false, so the next syncup resumes its device and moves the ns back to the
// optimized group (§11.3's abort path).
//
// A missing origin subsystem or ns_idx on the finalize path is skipped, not an
// error [D-G]: the transfer is being deleted either way, and the RPC must not
// become unable to complete because the namespace it points at was removed
// first.
func (s *Server) DeleteTransfer(
	ctx context.Context,
	req *pb.DeleteTransferRequest,
) (*pb.DeleteTransferReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("xfer_name", req.GetXferName()); err != nil {
		return nil, err
	}
	var xferId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		xferId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		key, xfer, err := resolveTransfer(stm, sc, req.GetXferName())
		if err != nil {
			return err
		}
		xferId = xfer.GetXferId()
		if !req.GetForce() {
			ssKey := model.SubsystemKey(sc.Cid, sc.SpId(), xfer.GetOriNqn())
			subsystem := &pb.Subsystem{}
			if stm.Get(ssKey, subsystem) {
				ns := findNs(subsystem, xfer.GetOriNsIdx())
				// Rewritten only when the flag actually moves: an origin
				// that is already retired needs no write, so finalizing a
				// transfer twice leaves the subsystem key byte-identical.
				// (It does NOT keep the key out of the conflict window —
				// stm.Get above has already put it in this transaction's
				// read set.)
				if ns != nil && !ns.GetSuspended() {
					ns.Suspended = true
					stm.Put(ssKey, subsystem)
				}
			}
		}
		stm.Del(key)
		conf := sc.Conf
		conf.XferNameList = removeName(
			conf.GetXferNameList(), req.GetXferName())
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), conf)
		return bumpSp(stm, opDeleteTransfer, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteTransferReply{XferId: xferId}, nil
}

// GetTransfer is architecture.md §8.10's GetTransfer: one consistency read
// (GW5), so the SP the transfer is scoped to and the record itself come from
// the same store revision and a reply can never describe a transfer of an SP
// that was deleted between the two reads.
//
// It is a Snapshot and not a RunSTM because nothing is written: §5.8's
// one-transaction rule is about the read set being consistent, not about
// committing.
func (s *Server) GetTransfer(
	ctx context.Context,
	req *pb.GetTransferRequest,
) (*pb.GetTransferReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("xfer_name", req.GetXferName()); err != nil {
		return nil, err
	}
	var xfer *pb.Transfer
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		xfer = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		_, found, err := resolveTransfer(stm, sc, req.GetXferName())
		if err != nil {
			return err
		}
		xfer = found
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetTransferReply{Xfer: xfer}, nil
}

// UpdateTransferHosts is architecture.md §8.10's UpdateTransferHosts: it
// replaces the transfer's OWN allowed_hosts, which is what an operator calls
// after a destination cntlr moved to another CN and therefore presents a
// different CnHostNqn.
//
// No CdcEntry is touched — unlike UpdateSubsystemHosts (§8.8), which maintains
// one — because the xfer subsystem is not a discovery-advertised dnv subsystem
// (§5.9): it exists only for the destination SP's clone, which is told where
// to connect out of band.
//
// The list is rewritten and the SP bumped unconditionally: §0 #17's
// idempotent no-write applies to the three Update*Enabled/Disabled RPCs only,
// and a caller that resends the same list still wants the cntlrs to re-apply
// it (§5.5).
func (s *Server) UpdateTransferHosts(
	ctx context.Context,
	req *pb.UpdateTransferHostsRequest,
) (*pb.UpdateTransferHostsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("xfer_name", req.GetXferName()); err != nil {
		return nil, err
	}
	if err := validateHosts(
		"allowed_hosts", req.GetAllowedHosts(),
	); err != nil {
		return nil, err
	}
	var xferId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		xferId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		key, xfer, err := resolveTransfer(stm, sc, req.GetXferName())
		if err != nil {
			return err
		}
		xferId = xfer.GetXferId()
		xfer.AllowedHosts = req.GetAllowedHosts()
		stm.Put(key, xfer)
		return bumpSp(stm, opUpdateTransferHosts, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateTransferHostsReply{XferId: xferId}, nil
}
