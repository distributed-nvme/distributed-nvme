package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Bitmap reads / gateway.md, Bitmap reads: the
// bitmap read a clone pages through before it copies anything.
//
// It is a read-only two-phase RPC (AG1): one `Snapshot` resolves everything
// the agent requests need at a single store revision, and the agent calls
// happen strictly outside it (architecture.md, STM discipline). It writes no
// key, so it takes no revision token and bumps nothing
// (architecture.md, Revision keys and the sync fan-out) — AG4's "re-run
// resolution in the deciding STM" has no second STM to apply to here, because
// there is nothing to decide: the reply is a measurement of the primary's
// live dm-thin metadata, not a change to desired state. A concurrent etcd
// change therefore cannot corrupt anything; it can only make the answer
// describe a slightly older world. The time of the read matters for
// another reason, which architecture.md, Clones and Bitmap reads, states: a
// bitmap read while the source still takes writes marks blocks written
// afterwards as skippable, and the reply carries no mark of when it was
// read. So the read is refused unless the primary reports every namespace
// on the volume parked (checkVolumeParked).
//
// It goes to the PRIMARY cntlr's CN. The primary is the one cntlr that builds
// the stack of architecture.md, Primary cntlr — the per-slice thin pools and
// the leg devices under them — so it is the only node that can walk the pool
// metadata the answer is computed from. A secondary holds none of it.

// The op names the agent-failure messages cite, so a log line names something
// greppable back to the RPC the operator called.
const (
	opGetThinDeviceBitmap = "GetThinDeviceBitmap"
)

// bitmapTarget is everything the phase-2 agent calls need, carried out of
// the resolving Snapshot: the ids that address the object on the CN plus the
// endpoint to dial.
type bitmapTarget struct {
	Cid      uint64
	SpId     uint64
	CntlrId  uint64
	CnId     uint64
	AddrPort string
}

// openBitmapTarget resolves the SP's primary cntlr and the CN it runs on
// (architecture.md, Bitmap reads; gateway.md, Bitmap reads).
//
// An SP with no primary is FAILED_PRECONDITION rather than NOT_FOUND: the SP
// and its cntlrs exist, but no node has been promoted yet (the sp-worker
// elects one, dnv-worker.md AR5), so the bitmap is unavailable *for
// now* — a caller that retries after promotion succeeds, which is what a
// precondition means.
//
// The CnConf is read for its `cn_id` only, exactly as InspectCntlr does
// (gateway.md, Cntlrs and inspects): the Cntlr stores the addr_port, and the
// agent request addresses the node by id. Its absence is an ABORTED
// (architecture.md, UNEXPECTED_ERROR → `ABORTED`) and not NOT_FOUND —
// DeleteControllerNode refuses a CN whose `cntlr_ptr_list` is non-empty, so a
// live cntlr pointing at a missing CnConf is a lost invariant key, not a
// user-visible "no such node".
func openBitmapTarget(
	stm etcdutil.STM,
	sc *spScope,
	spName string,
) (bitmapTarget, error) {
	cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
	if err != nil {
		return bitmapTarget{}, err
	}
	cntlrId, cntlr, ok := primaryCntlr(sc.Conf, cntlrs)
	if !ok {
		return bitmapTarget{}, errPrecondition(
			"storage pool %q has no primary cntlr", spName)
	}
	key := model.CnConfKey(sc.Cid, cntlr.GetAddrPort())
	cn := &pb.CnConf{}
	if !stm.Get(key, cn) {
		return bitmapTarget{}, errAborted("cn_conf key %q is missing", key)
	}
	return bitmapTarget{
		Cid:      sc.Cid,
		SpId:     sc.SpId(),
		CntlrId:  cntlrId,
		CnId:     cn.GetCnId(),
		AddrPort: cntlr.GetAddrPort(),
	}, nil
}

// GetThinDeviceBitmap is the GetThinDeviceBitmap of architecture.md, Bitmap
// reads: the mapping bitmap of one thin device's volume in one slice, which a
// clone pages through and feeds to AppendCloneBitmap on the destination.
//
// Phase 1 is a Snapshot and not a RunSTM because the RPC writes nothing: one
// store revision is enough to make the td, the primary and the CN it names
// mutually consistent (architecture.md, STM discipline; GW8).
//
// `slice_idx` is checked against the SP's own slice count and not against a
// constant: it is state-dependent, so it belongs inside the transaction and
// not in validate.go (GW4). It is checked after the td and the primary are
// resolved, so a caller naming a td that does not exist hears that first.
//
// `block_cnt == 0` means "to the end of the volume" and is passed through
// unchanged. A window whose start is not a multiple of 8 blocks, or whose
// count is not and that ends before the slice's part of the volume does, is
// INVALID_ARGUMENT, judged in the snapshot against the td's size and the
// pool's geometry, as the agent sizes the slice's part: such a reply cannot
// be placed at a byte of a chunk, or would put its pad bits in the middle of
// one (architecture.md, Bitmap reads).
//
// Before the read, no clone may fill the volume, every namespace of the
// pool on it must be suspended in the desired state (volumeNamespaces) and
// read parked in the primary's report (checkVolumeParked); a volume no
// namespace exports needs no report.
//
// The reply is the agent's bytes VERBATIM (GW14). The wire convention
// — bit k = 1 iff block start_block+k is unmapped, LSB-first within each byte
// (bit i at `bitmap[i/8] & (1 << (i%8))`), trailing pad bits 0 — is produced
// by the agent, which inverts thin-pool metadata's native "mapped = written"
// once at that boundary (architecture.md, raid0 bitmap math). The gateway
// never inspects or rewrites a bit, so it can never disagree with the agent
// about the convention, and the caller's pages stay concatenable while every
// page but the last covers a multiple of 8 blocks (architecture.md, Bitmap
// reads).
func (s *Server) GetThinDeviceBitmap(
	ctx context.Context,
	req *pb.GetThinDeviceBitmapRequest,
) (*pb.GetThinDeviceBitmapReply, error) {
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
	var target bitmapTarget
	var tdId uint64
	var onVolume []volumeNs
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		// Reassigned on entry so the closure stays a pure function of what
		// it reads (GW8), whatever a previous attempt left behind.
		target = bitmapTarget{}
		tdId = 0
		onVolume = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
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
		found, err := openBitmapTarget(stm, sc, req.GetSpName())
		if err != nil {
			return err
		}
		if int(req.GetSliceIdx()) >= spSliceCnt(sc.Conf) {
			return errInvalid(
				"slice_idx %d is not below the storage pool's slice count %d",
				req.GetSliceIdx(), spSliceCnt(sc.Conf))
		}
		if err := checkBitmapWindow(sc.Conf, td, req.GetStartBlock(),
			req.GetBlockCnt()); err != nil {
			return err
		}
		// A volume a clone fills changes under the read, whatever its
		// namespaces do: the dm-clone copies into it (architecture.md,
		// Clones).
		cloneName, filled, err := tdCloneRef(stm, sc, td.GetTdId())
		if err != nil {
			return err
		}
		if filled {
			return errPrecondition(
				"thin device %q is the destination of clone %q, which "+
					"still writes it", req.GetTdName(), cloneName)
		}
		nss, err := volumeNamespaces(stm, sc, td.GetTdId())
		if err != nil {
			return err
		}
		target = found
		tdId = td.GetTdId()
		onVolume = nss
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	if err := checkVolumeParked(ctx, target, req.GetTdName(),
		onVolume); err != nil {
		return nil, err
	}
	var bitmap []byte
	err = withCnAgent(ctx, target.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetThinDeviceBm(
				ctx,
				&pb.GetThinDeviceBmRequest{
					ClusterId:  target.Cid,
					CnId:       target.CnId,
					SpId:       target.SpId,
					CntlrId:    target.CntlrId,
					TdId:       tdId,
					SliceIdx:   req.GetSliceIdx(),
					StartBlock: req.GetStartBlock(),
					BlockCnt:   req.GetBlockCnt(),
				},
			)
			if err != nil {
				return err
			}
			bitmap = reply.GetBitmap()
			return nil
		})
	if err != nil {
		// AG3: a transport failure or a non-OK status from the agent is this
		// RPC's ABORTED — the gateway has no second source for the answer.
		return nil, errAborted("%s: controller node %q: %v",
			opGetThinDeviceBitmap, target.AddrPort, err)
	}
	return &pb.GetThinDeviceBitmapReply{Bitmap: bitmap}, nil
}

// checkBitmapWindow is the window rule of architecture.md, Bitmap reads: a
// read starts at a multiple of 8 blocks and not past the slice's part of the
// volume, its count fits in what is left of that part, and its count is a
// multiple of 8 unless it is zero, "to the end", or ends where that part
// does. The part has the agent's size, the td's size over the slice count
// and the pool's block size (cnagent.md CN26). The count is compared with
// what is left, never summed with the start: a sum past 2^64 wraps.
func checkBitmapWindow(
	conf *pb.SpConf,
	td *pb.ThinDevice,
	start uint64,
	cnt uint64,
) error {
	if start%8 != 0 {
		return errInvalid("start_block %d is not a multiple of 8 blocks",
			start)
	}
	sliceCnt := uint64(spSliceCnt(conf))
	blockSize := conf.GetBdevConf().GetDmPoolConf().GetDataBlockSize()
	if sliceCnt == 0 || blockSize == 0 {
		return errAborted("storage pool geometry: %d slices of %d-byte "+
			"blocks", sliceCnt, blockSize)
	}
	end := td.GetSize() / sliceCnt / blockSize
	if start > end {
		return errInvalid("start_block %d is past the slice's %d blocks",
			start, end)
	}
	if cnt > end-start {
		return errInvalid("block_cnt %d from %d is past the slice's %d "+
			"blocks", cnt, start, end)
	}
	if cnt == 0 || cnt%8 == 0 || cnt == end-start {
		return nil
	}
	return errInvalid("block_cnt %d is not a multiple of 8 blocks and the "+
		"window from %d ends before the slice's %d blocks", cnt, start, end)
}

// volumeNs is one namespace of the pool on a volume, by its id and its
// address for a refusal's message.
type volumeNs struct {
	nsId  uint64
	nqn   string
	nsIdx uint32
}

// volumeNamespaces lists every namespace of the pool whose td is tdId, and
// refuses FAILED_PRECONDITION one the desired state does not keep
// effectively suspended: its `suspended` set, or a transfer with
// `auto_suspend` naming it (cnagent.md CN16; the override of a clone with
// `auto_resume` cannot apply, as no clone fills the td). A report of a park
// that the desired state has since lifted is then never trusted. A listed
// key that is gone aborts the read (architecture.md, UNEXPECTED_ERROR →
// `ABORTED`), as a read that cannot prove which namespaces export the
// volume must not go on.
func volumeNamespaces(
	stm etcdutil.STM,
	sc *spScope,
	tdId uint64,
) ([]volumeNs, error) {
	type place struct {
		nqn   string
		nsIdx uint32
	}
	autoSuspended := make(map[place]bool)
	for _, name := range sc.Conf.GetXferNameList() {
		key := model.TransferKey(sc.Cid, sc.SpId(), name)
		xfer := &pb.Transfer{}
		if !stm.Get(key, xfer) {
			return nil, errAborted("transfer key %q is missing", key)
		}
		if xfer.GetAutoSuspend() {
			autoSuspended[place{xfer.GetOriNqn(), xfer.GetOriNsIdx()}] = true
		}
	}
	var out []volumeNs
	for _, nqn := range sc.Conf.GetNqnList() {
		key := model.SubsystemKey(sc.Cid, sc.SpId(), nqn)
		subsystem := &pb.Subsystem{}
		if !stm.Get(key, subsystem) {
			return nil, errAborted("subsystem key %q is missing", key)
		}
		for _, ns := range subsystem.GetNsList() {
			if ns.GetTdId() != tdId {
				continue
			}
			if !ns.GetSuspended() &&
				!autoSuspended[place{nqn, ns.GetNsIdx()}] {
				return nil, errPrecondition(
					"namespace %d of subsystem %q is on the volume and "+
						"not suspended, so a host can still write it",
					ns.GetNsIdx(), nqn)
			}
			out = append(out, volumeNs{
				nsId: ns.GetNsId(), nqn: nqn, nsIdx: ns.GetNsIdx(),
			})
		}
	}
	return out, nil
}

// checkVolumeParked is the between-the-phases agent call of
// GetThinDeviceBitmap (AG1): every namespace of the pool on the volume must
// read parked in the primary's report — its ns-dev row RES_STATUS_OK with
// RES_REASON_PARKED (cnagent.md CN28) — so no write through a namespace of
// the pool reaches the volume while its bitmap is read (architecture.md,
// Bitmap reads). With a transfer's origin the only namespace of its volume
// (architecture.md, Transfers), that is the transfer's park. GetCntlrInfo
// answers from the agent's latest observation, which can lag the node: a
// park that has not reached the report yet is refused, and the caller asks
// again; a park the desired state has since lifted was refused already in
// the snapshot (volumeNamespaces). A volume no namespace exports needs no
// call. A refusal, an agent that refuses the cntlr, and one that does not
// answer are FAILED_PRECONDITION, as for DeleteClone's proof: the read
// cannot be proven safe yet.
func checkVolumeParked(
	ctx context.Context,
	target bitmapTarget,
	tdName string,
	onVolume []volumeNs,
) error {
	if len(onVolume) == 0 {
		return nil
	}
	var rows map[uint64]*pb.ResInfo
	var code uint32
	callErr := withCnAgent(ctx, target.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: target.Cid,
				CnId:      target.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    target.SpId,
					CntlrId: target.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			rows = reply.GetCntlrInfo().GetNsIdToDmLinear()
			code = reply.GetAgentReply().GetCode()
			return nil
		})
	if callErr != nil {
		return errPrecondition(
			"thin device %q: its namespaces are not proven parked, "+
				"controller node %q did not answer: %v", tdName,
			target.AddrPort, callErr)
	}
	if code != 0 {
		return errPrecondition(
			"thin device %q: its namespaces are not proven parked, "+
				"controller node %q answered code %d for the primary cntlr",
			tdName, target.AddrPort, code)
	}
	for _, ns := range onVolume {
		row := rows[ns.nsId]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetReason() != pb.ResReason_RES_REASON_PARKED {
			return errPrecondition(
				"thin device %q: namespace %d of subsystem %q is not "+
					"parked on the primary, so a host can still write the "+
					"volume", tdName, ns.nsIdx, ns.nqn)
		}
	}
	return nil
}
