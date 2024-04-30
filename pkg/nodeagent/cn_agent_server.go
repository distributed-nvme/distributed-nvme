package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/localdata"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/namefmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/oscmd"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

func encodeSpCntlrId(
	spId string,
	cntlrId string,
) string {
	return fmt.Sprintf("%s-%s", spId, cntlrId)
}

func decodeSpCntlrId(
	key string,
) (string, string, error) {
	items := strings.Split(key, "-")
	if len(items) != 2 {
		return "", "", fmt.Errorf("Invalid item len: %s %d", items, len(items))
	}
	return items[0], items[1], nil
}

type spCntlrRuntimeData struct {
	mu           sync.Mutex
	spCntlrLocal *localdata.SpCntlrLocal
	spCntlrConf  *pbnd.SpCntlrConf
}

func syncupCntlrNvmePort(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	nvmePortConf *pbnd.NvmePortConf,
) *pbnd.NvmePortInfo {
	if err := oc.NvmetPortCreate(
		pch,
		nvmePortConf.PortNum,
		nvmePortConf.NvmeListener.TrType,
		nvmePortConf.NvmeListener.AdrFam,
		nvmePortConf.NvmeListener.TrAddr,
		nvmePortConf.NvmeListener.TrSvcId,
		nvmePortConf.TrEq.SeqCh,
	); err != nil {
		return &pbnd.NvmePortInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
		}
	}
	return &pbnd.NvmePortInfo{
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: pch.Timestamp,
		},
	}
}

func syncupCntlrLd(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	localLegConf *pbnd.LocalLegConf,
	grpConf *pbnd.GrpConf,
	ldCnConf *pbnd.LdCnConf,
) *pbnd.LdCnInfo {
	nvmeArg := &oscmd.NvmeArg{
		Nqn:       nf.LdDnDmNqn(ldCnConf.DnId, spCntlrConf.SpId, ldCnConf.LdId),
		Transport: ldCnConf.NvmePortConf.NvmeListener.TrType,
		TrAddr:    ldCnConf.NvmePortConf.NvmeListener.TrAddr,
		TrSvcId:   ldCnConf.NvmePortConf.NvmeListener.TrSvcId,
		HostNqn:   nf.HostNqnCn(spCntlrConf.CnId),
	}
	if err := oc.NvmeConnectPath(pch, nvmeArg); err != nil {
		return &pbnd.LdCnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
		}
	}
	return &pbnd.LdCnInfo{
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: pch.Timestamp,
		},
	}
}

func syncupCntlrSingleRaid1(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	localLegConf *pbnd.LocalLegConf,
	grpConf *pbnd.GrpConf,
	raid1Name string,
	size uint64,
	meta0LdConf *pbnd.LdCnConf,
	data0LdConf *pbnd.LdCnConf,
	meta1LdConf *pbnd.LdCnConf,
	data1LdConf *pbnd.LdCnConf,
) (*pbnd.RedundancyInfo, error) {
	meta0Nqn := nf.LdDnDmNqn(
		meta0LdConf.DnId,
		spCntlrConf.SpId,
		meta0LdConf.LdId,
	)
	meta0Path := nf.NsPath(meta0Nqn, nf.LdDnDmNsNum())
	data0Nqn := nf.LdDnDmNqn(
		data0LdConf.DnId,
		spCntlrConf.SpId,
		data0LdConf.LdId,
	)
	data0Path := nf.NsPath(data0Nqn, nf.LdDnDmNsNum())
	meta1Nqn := nf.LdDnDmNqn(
		meta1LdConf.DnId,
		spCntlrConf.SpId,
		meta1LdConf.LdId,
	)
	meta1Path := nf.NsPath(meta1Nqn, nf.LdDnDmNsNum())
	data1Nqn := nf.LdDnDmNqn(
		data1LdConf.DnId,
		spCntlrConf.SpId,
		data1LdConf.LdId,
	)
	data1Path := nf.NsPath(data1Nqn, nf.LdDnDmNsNum())
	raid1Arg := &oscmd.DmRaid1Arg{
		Start:      0,
		Size:       size,
		Meta0:      meta0Path,
		Data0:      data0Path,
		Meta1:      meta1Path,
		Data1:      data1Path,
		RegionSize: activeCntlrConf.RedundancyConf.RegionSize,
		Nosync:     grpConf.NoSync,
		RebuildIdx: grpConf.RebuildIdx,
	}
	if len(grpConf.OmitIdxList) > 0 {
		switch grpConf.OmitIdxList[0] {
		case 0:
			raid1Arg.Meta0 = "-"
			raid1Arg.Data0 = "-"
		case 1:
			raid1Arg.Meta1 = "-"
			raid1Arg.Data1 = "-"
		default:
			pch.Logger.Fatal(
				"Invalid omit idx: %d",
				grpConf.OmitIdxList[0],
			)
		}
	}
	if err := oc.DmCreateRaid1(pch, raid1Name, raid1Arg); err != nil {
		return nil, err
	}
	dmRaidStatus, err := oc.DmGetRaidStatus(pch, raid1Name)
	if err != nil {
		return nil, err
	}
	return &pbnd.RedundancyInfo{
		HealthChars: dmRaidStatus.HealthChars,
		SyncCurr:    dmRaidStatus.SyncCurr,
		SyncAction:  dmRaidStatus.SyncAction,
		MismatchCnt: dmRaidStatus.MismatchCnt,
		DataOffset:  dmRaidStatus.DataOffset,
		JournalChar: uint32(dmRaidStatus.JournalChar),
	}, nil
}

func syncupCntlrRaid1(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	localLegConf *pbnd.LocalLegConf,
	grpConf *pbnd.GrpConf,
) (*pbnd.RedundancyInfo, *pbnd.RedundancyInfo, error) {
	ldCnt := len(grpConf.LdCnConfList)
	if grpConf.MetaSize > 0 {
		if ldCnt != 8 {
			pch.Logger.Fatal(
				"Has meta devs and ld cnt is incorrect: %d",
				ldCnt,
			)
		}

		metaRedunInfo, err := syncupCntlrSingleRaid1(
			pch,
			oc,
			nf,
			spCntlrConf,
			activeCntlrConf,
			localLegConf,
			grpConf,
			nf.GrpMetaDmName(
				spCntlrConf.CnId,
				spCntlrConf.SpId,
				grpConf.GrpId,
			),
			grpConf.MetaSize,
			grpConf.LdCnConfList[0],
			grpConf.LdCnConfList[1],
			grpConf.LdCnConfList[4],
			grpConf.LdCnConfList[5],
		)
		if err != nil {
			return nil, nil, err
		}

		dataRedunInfo, err := syncupCntlrSingleRaid1(
			pch,
			oc,
			nf,
			spCntlrConf,
			activeCntlrConf,
			localLegConf,
			grpConf,
			nf.GrpDataDmName(
				spCntlrConf.CnId,
				spCntlrConf.SpId,
				grpConf.GrpId,
			),
			grpConf.DataSize,
			grpConf.LdCnConfList[2],
			grpConf.LdCnConfList[3],
			grpConf.LdCnConfList[6],
			grpConf.LdCnConfList[7],
		)
		if err != nil {
			return nil, nil, err
		}

		return metaRedunInfo, dataRedunInfo, nil
	}

	if ldCnt != 4 {
		pch.Logger.Fatal(
			"No meta devs and ld cnt is incorrect: %d",
			ldCnt,
		)
	}
	dataRedunInfo, err := syncupCntlrSingleRaid1(
		pch,
		oc,
		nf,
		spCntlrConf,
		activeCntlrConf,
		localLegConf,
		grpConf,
		nf.GrpDataDmName(
			spCntlrConf.CnId,
			spCntlrConf.SpId,
			grpConf.GrpId,
		),
		grpConf.DataSize,
		grpConf.LdCnConfList[0],
		grpConf.LdCnConfList[1],
		grpConf.LdCnConfList[2],
		grpConf.LdCnConfList[3],
	)
	if err != nil {
		return nil, nil, err
	}
	return nil, dataRedunInfo, err
}

func syncupCntlrGrp(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	localLegConf *pbnd.LocalLegConf,
	grpConf *pbnd.GrpConf,
) *pbnd.GrpInfo {
	ldCnInfoList := make([]*pbnd.LdCnInfo, len(grpConf.LdCnConfList))
	for i, ldCnInfo := range grpConf.LdCnConfList {
		ldCnInfoList[i] = syncupCntlrLd(
			pch,
			oc,
			nf,
			spCntlrConf,
			activeCntlrConf,
			localLegConf,
			grpConf,
			ldCnInfo,
		)
	}
	var metaRedunInfo *pbnd.RedundancyInfo
	var dataRedunInfo *pbnd.RedundancyInfo
	var err error
	switch activeCntlrConf.RedundancyConf.RedunType {
	case constants.RedunTypeRaid1:
		metaRedunInfo, dataRedunInfo, err = syncupCntlrRaid1(
			pch,
			oc,
			nf,
			spCntlrConf,
			activeCntlrConf,
			localLegConf,
			grpConf,
		)
	default:
		pch.Logger.Fatal("Unknow RedunType: %d", constants.RedunTypeRaid1)
	}
	if err != nil {
		return &pbnd.GrpInfo{
			GrpId: grpConf.GrpId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			MetaRedunInfo: metaRedunInfo,
			DataRedunInfo: dataRedunInfo,
			LdCnInfoList:  ldCnInfoList,
		}
	}

	return &pbnd.GrpInfo{
		GrpId: grpConf.GrpId,
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: pch.Timestamp,
		},
		MetaRedunInfo: metaRedunInfo,
		DataRedunInfo: dataRedunInfo,
		LdCnInfoList:  ldCnInfoList,
	}
}

func syncupCntlrExportToRemote(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	localLegConf *pbnd.LocalLegConf,
) error {
	poolName := nf.LegPoolDmName(
		spCntlrConf.CnId,
		spCntlrConf.SpId,
		localLegConf.LegId,
	)
	poolPath := nf.DmNameToPath(poolName)
	cnt := uint64(len(activeCntlrConf.RemoteLegConfList) + 1)
	hostNqnMap := make(map[string]bool)
	for _, remoteLegConf := range activeCntlrConf.RemoteLegConfList {
		hostNqn := nf.HostNqnCn(remoteLegConf.CnId)
		hostNqnMap[hostNqn] = true
	}
	for _, ssConf := range spCntlrConf.SsConfList {
		nqn := nf.RemoteLegNqn(
			spCntlrConf.CnId,
			spCntlrConf.SpId,
			localLegConf.LegId,
		)
		nsMap := make(map[string]*oscmd.NvmetNsArg)
		for _, nsConf := range ssConf.NsConfList {
			thinName := nf.LegThinDmName(
				spCntlrConf.CnId,
				spCntlrConf.SpId,
				localLegConf.LegId,
				nsConf.DevId,
			)
			thinPath := nf.DmNameToPath(thinName)
			if nsConf.Size%cnt > 0 {
				pch.Logger.Fatal(
					"Size is not divisible by cnt: %d %d",
					nsConf.Size,
					cnt,
				)
			}
			size := nsConf.Size / cnt
			thinArg := &oscmd.DmThinArg{
				Start:    0,
				Size:     size,
				PoolPath: poolPath,
				DevId:    nsConf.DevId,
			}
			if err := oc.DmCreateThin(
				pch,
				thinName,
				thinArg,
			); err != nil {
				return err
			}
			linearArgs := make([]*oscmd.DmLinearArg, 1)
			linearArgs[0] = &oscmd.DmLinearArg{
				Start:   0,
				Size:    size,
				DevPath: thinPath,
				Offset:  0,
			}
			localName := nf.LegToLocalDmName(
				spCntlrConf.CnId,
				spCntlrConf.SpId,
				localLegConf.LegId,
				nsConf.DevId,
			)
			if err := oc.DmCreateLinear(
				pch,
				localName,
				linearArgs,
			); err != nil {
				return err
			}
			remoteName := nf.LegToRemoteDmName(
				spCntlrConf.CnId,
				spCntlrConf.SpId,
				localLegConf.LegId,
				nsConf.DevId,
			)
			if err := oc.DmCreateLinear(
				pch,
				remoteName,
				linearArgs,
			); err != nil {
				return err
			}
			remotePath := nf.DmNameToPath(remoteName)
			nsUuid := nf.NsUuid(nqn, nsConf.NsId)
			nsMap[nsConf.NsNum] = &oscmd.NvmetNsArg{
				NsNum:   nsConf.NsNum,
				DevPath: remotePath,
				Uuid:    nsUuid,
			}
		}
		if err := oc.NvmetSubsysCreate(
			pch,
			nqn,
			localLegConf.CntlidMin,
			localLegConf.CntlidMax,
			"0",
			hostNqnMap,
			nsMap,
		); err != nil {
			return err
		}
	}
	return nil
}

func syncupCntlrLocalLeg(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	localLegConf *pbnd.LocalLegConf,
) *pbnd.LocalLegInfo {
	grpInfoList := make(
		[]*pbnd.GrpInfo,
		len(localLegConf.GrpConfList),
	)
	metaLinearArgs := make([]*oscmd.DmLinearArg, 0)
	dataLinearArgs := make([]*oscmd.DmLinearArg, 0)
	metaStart := uint64(0)
	dataStart := uint64(0)
	for i, grpConf := range localLegConf.GrpConfList {
		grpInfoList[i] = syncupCntlrGrp(
			pch,
			oc,
			nf,
			spCntlrConf,
			activeCntlrConf,
			localLegConf,
			grpConf,
		)
		grpDataName := nf.GrpDataDmName(
			spCntlrConf.CnId,
			spCntlrConf.SpId,
			grpConf.GrpId,
		)
		dataArg := &oscmd.DmLinearArg{
			Start:   dataStart,
			Size:    grpConf.DataSize,
			DevPath: nf.DmNameToPath(grpDataName),
			Offset:  0,
		}
		dataLinearArgs = append(dataLinearArgs, dataArg)
		dataStart += grpConf.DataSize
		if grpConf.MetaSize > 0 {
			grpMetaName := nf.GrpMetaDmName(
				spCntlrConf.CnId,
				spCntlrConf.SpId,
				grpConf.GrpId,
			)
			metaArg := &oscmd.DmLinearArg{
				Start:   metaStart,
				Size:    grpConf.MetaSize,
				DevPath: nf.DmNameToPath(grpMetaName),
				Offset:  0,
			}
			metaLinearArgs = append(metaLinearArgs, metaArg)
			metaStart += grpConf.MetaSize
		}
	}
	dataSize := dataStart

	metaName := nf.LegMetaDmName(
		spCntlrConf.CnId,
		spCntlrConf.SpId,
		localLegConf.LegId,
	)
	if err := oc.DmCreateLinear(pch, metaName, metaLinearArgs); err != nil {
		return &pbnd.LocalLegInfo{
			LegId: localLegConf.LegId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			GrpInfoList: grpInfoList,
		}
	}
	dataName := nf.LegDataDmName(
		spCntlrConf.CnId,
		spCntlrConf.SpId,
		localLegConf.LegId,
	)
	if err := oc.DmCreateLinear(pch, dataName, metaLinearArgs); err != nil {
		return &pbnd.LocalLegInfo{
			LegId: localLegConf.LegId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			GrpInfoList: grpInfoList,
		}
	}

	poolName := nf.LegPoolDmName(
		spCntlrConf.CnId,
		spCntlrConf.SpId,
		localLegConf.LegId,
	)
	poolArg := &oscmd.DmPoolArg{
		Start:             0,
		Size:              dataSize,
		MetaDev:           nf.DmNameToPath(metaName),
		DataDev:           nf.DmNameToPath(dataName),
		DataBlockSize:     activeCntlrConf.ThinPoolConf.DataBlockSize,
		LowWaterMark:      activeCntlrConf.ThinPoolConf.LowWaterMark,
		ErrorIfNoSpace:    activeCntlrConf.ThinPoolConf.ErrorIfNoSpace,
		SkipBlockZeroing:  true,
		IgnoreDiscard:     false,
		NoDiscardPassdown: false,
		ReadOnly:          false,
	}
	if err := oc.DmCreatePool(
		pch,
		poolName,
		poolArg,
		localLegConf.Reload,
	); err != nil {
		return &pbnd.LocalLegInfo{
			LegId: localLegConf.LegId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			GrpInfoList: grpInfoList,
		}
	}

	if activeCntlrConf.CreatingSnapConf != nil {
		if activeCntlrConf.CreatingSnapConf.OriId == constants.Uint32Max {
			if err := oc.DmPoolMsgCreateThin(
				pch,
				poolName,
				activeCntlrConf.CreatingSnapConf.DevId,
			); err != nil {
				return &pbnd.LocalLegInfo{
					LegId: localLegConf.LegId,
					StatusInfo: &pbnd.StatusInfo{
						Code:      constants.StatusCodeInternalErr,
						Msg:       err.Error(),
						Timestamp: pch.Timestamp,
					},
					GrpInfoList: grpInfoList,
				}
			}
		} else {
			if err := oc.DmPoolMsgCreateSnap(
				pch,
				poolName,
				activeCntlrConf.CreatingSnapConf.DevId,
				activeCntlrConf.CreatingSnapConf.OriId,
			); err != nil {
				return &pbnd.LocalLegInfo{
					LegId: localLegConf.LegId,
					StatusInfo: &pbnd.StatusInfo{
						Code:      constants.StatusCodeInternalErr,
						Msg:       err.Error(),
						Timestamp: pch.Timestamp,
					},
					GrpInfoList: grpInfoList,
				}
			}
		}
	}

	if activeCntlrConf.DeletingSnapConf != nil {
		if err := oc.DmPoolMsgDelete(
			pch,
			poolName,
			activeCntlrConf.DeletingSnapConf.DevId,
		); err != nil {
			return &pbnd.LocalLegInfo{
				LegId: localLegConf.LegId,
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: pch.Timestamp,
				},
				GrpInfoList: grpInfoList,
			}
		}
	}

	if err := syncupCntlrExportToRemote(
		pch,
		oc,
		nf,
		spCntlrConf,
		activeCntlrConf,
		localLegConf,
	); err != nil {
		return &pbnd.LocalLegInfo{
			LegId: localLegConf.LegId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			GrpInfoList: grpInfoList,
		}
	}

	poolStatus, err := oc.DmGetPoolStatus(
		pch,
		poolName,
	)
	if err != nil {
		return &pbnd.LocalLegInfo{
			LegId: localLegConf.LegId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			GrpInfoList: grpInfoList,
		}
	}

	return &pbnd.LocalLegInfo{
		LegId: localLegConf.LegId,
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: pch.Timestamp,
		},
		ThinPoolInfo: &pbnd.ThinPoolInfo{
			TransactionId:        poolStatus.TransactionId,
			UsedMetaBlocks:       poolStatus.UsedMetaBlocks,
			TotalMetaBlocks:      poolStatus.TotalMetaBlocks,
			UsedDataBlocks:       poolStatus.UsedDataBlocks,
			HeldMetadataRoot:     poolStatus.HeldMetadataRoot,
			Mode:                 poolStatus.Mode,
			DiscardPassdown:      poolStatus.DiscardPassdown,
			ErrorOrQueue:         poolStatus.ErrorOrQueue,
			NeedsCheck:           poolStatus.NeedsCheck,
			MetadataLowWatermark: poolStatus.MetadataLowWatermark,
		},
		GrpInfoList: grpInfoList,
	}
}

func syncupCntlrRemoteLeg(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
	remoteLegConf *pbnd.RemoteLegConf,
) *pbnd.RemoteLegInfo {
	// FIXME: implement fence
	fenceId := ""

	nvmeArg := &oscmd.NvmeArg{
		Nqn: nf.RemoteLegNqn(
			remoteLegConf.CnId,
			spCntlrConf.SpId,
			remoteLegConf.LegId,
		),
		Transport: remoteLegConf.NvmeListener.TrType,
		TrAddr:    remoteLegConf.NvmeListener.TrAddr,
		TrSvcId:   remoteLegConf.NvmeListener.TrSvcId,
		HostNqn:   nf.HostNqnCn(spCntlrConf.CnId),
	}

	if err := oc.NvmeConnectPath(pch, nvmeArg); err != nil {
		return &pbnd.RemoteLegInfo{
			LegId: remoteLegConf.LegId,
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeInternalErr,
				Msg:       err.Error(),
				Timestamp: pch.Timestamp,
			},
			FenceId: fenceId,
		}
	}
	return &pbnd.RemoteLegInfo{
		LegId: remoteLegConf.LegId,
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: pch.Timestamp,
		},
		FenceId: fenceId,
	}
}

func syncupActiveCntlr(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	activeCntlrConf *pbnd.ActiveCntlrConf,
) *pbnd.ActiveCntlrInfo {
	localLegInfoList := make(
		[]*pbnd.LocalLegInfo,
		len(activeCntlrConf.LocalLegConfList),
	)
	for i, localLegConf := range activeCntlrConf.LocalLegConfList {
		localLegInfoList[i] = syncupCntlrLocalLeg(
			pch,
			oc,
			nf,
			spCntlrConf,
			activeCntlrConf,
			localLegConf,
		)
	}

	remoteLegInfoList := make(
		[]*pbnd.RemoteLegInfo,
		len(activeCntlrConf.RemoteLegConfList),
	)
	for i, remoteLegConf := range activeCntlrConf.RemoteLegConfList {
		remoteLegInfoList[i] = syncupCntlrRemoteLeg(
			pch,
			oc,
			nf, spCntlrConf,
			activeCntlrConf,
			remoteLegConf,
		)
	}
	// FIXME: implement MovingTask and ImportingTask
	return &pbnd.ActiveCntlrInfo{
		LocalLegInfoList:  localLegInfoList,
		RemoteLegInfoList: remoteLegInfoList,
	}
}

func syncupCntlrSs(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
	ssConf *pbnd.SsConf,
) *pbnd.SsInfo {
	return &pbnd.SsInfo{}
}

func syncupSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrConf *pbnd.SpCntlrConf,
) *pbnd.SpCntlrInfo {
	nvmePortInfo := syncupCntlrNvmePort(
		pch,
		oc,
		nf,
		spCntlrConf,
		spCntlrConf.NvmePortConf,
	)
	activeCntlrInfo := syncupActiveCntlr(
		pch,
		oc,
		nf,
		spCntlrConf,
		spCntlrConf.ActiveCntlrConf,
	)
	ssInfoList := make([]*pbnd.SsInfo, len(spCntlrConf.SsConfList))
	for i, ssConf := range spCntlrConf.SsConfList {
		ssInfoList[i] = syncupCntlrSs(
			pch,
			oc,
			nf,
			spCntlrConf,
			ssConf,
		)
	}
	return &pbnd.SpCntlrInfo{
		StatusInfo: &pbnd.StatusInfo{
			Code:      constants.StatusCodeSucceed,
			Msg:       constants.StatusMsgSucceed,
			Timestamp: pch.Timestamp,
		},
		NvmePortInfo:    nvmePortInfo,
		ActiveCntlrInfo: activeCntlrInfo,
		SsInfoList:      ssInfoList,
	}
}

func cleanupSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	oc *oscmd.OsCommand,
	nf *namefmt.NameFmt,
	spCntlrLocal *localdata.SpCntlrLocal,
) error {
	return nil
}

type cnAgentServer struct {
	pbnd.UnimplementedControllerNodeAgentServer
	mu         sync.Mutex
	oc         *oscmd.OsCommand
	nf         *namefmt.NameFmt
	local      *localdata.LocalClient
	bgInterval time.Duration
	cnLocal    *localdata.CnLocal
	spCntlrMap map[string]*spCntlrRuntimeData
}

func (cnAgent *cnAgentServer) SyncupCn(
	ctx context.Context,
	req *pbnd.SyncupCnRequest,
) (*pbnd.SyncupCnReply, error) {
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	pch := ctxhelper.GetPerCtxHelper(ctx)

	if cnAgent.cnLocal == nil {
		cnLocal, err := cnAgent.local.GetCnLocal(pch, req.CnConf.CnId)
		if err != nil {
			return &pbnd.SyncupCnReply{
				CnInfo: &pbnd.CnInfo{
					StatusInfo: &pbnd.StatusInfo{
						Code:      constants.StatusCodeInternalErr,
						Msg:       err.Error(),
						Timestamp: pch.Timestamp,
					},
				},
			}, nil
		}
		if cnLocal == nil {
			cnAgent.cnLocal = &localdata.CnLocal{
				CnId:           req.CnConf.CnId,
				Revision:       req.CnConf.Revision,
				LiveSpCntlrMap: make(map[string]bool),
				DeadSpCntlrMap: make(map[string]bool),
			}
		} else {
			cnAgent.cnLocal = cnLocal
		}
	}

	if req.CnConf.CnId != cnAgent.cnLocal.CnId {
		return &pbnd.SyncupCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("CnId: %s", cnAgent.cnLocal.CnId),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	if req.CnConf.Revision < cnAgent.cnLocal.Revision {
		return &pbnd.SyncupCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", cnAgent.cnLocal.Revision),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	keyInReq := make(map[string]bool)
	for _, spCntlr := range req.CnConf.SpCntlrIdList {
		key := encodeSpCntlrId(spCntlr.SpId, spCntlr.CntlrId)
		keyInReq[key] = true
	}

	for key := range cnAgent.cnLocal.LiveSpCntlrMap {
		_, ok := keyInReq[key]
		if !ok {
			delete(cnAgent.cnLocal.LiveSpCntlrMap, key)
			cnAgent.cnLocal.DeadSpCntlrMap[key] = true
		}
	}

	for key := range keyInReq {
		cnAgent.cnLocal.LiveSpCntlrMap[key] = true
	}

	keyToLoad := make([]string, 0)
	for key := range cnAgent.cnLocal.LiveSpCntlrMap {
		keyToLoad = append(keyToLoad, key)
	}
	for key := range cnAgent.cnLocal.DeadSpCntlrMap {
		keyToLoad = append(keyToLoad, key)
	}
	for _, key := range keyToLoad {
		var spCntlrData *spCntlrRuntimeData
		if spCntlrData, ok := cnAgent.spCntlrMap[key]; !ok {
			spId, cntlrId, err := decodeSpCntlrId(key)
			if err != nil {
				pch.Logger.Fatal("ecodeSpCntlrId err: %s %v", key, err)
			}
			spCntlrLocal, err := cnAgent.local.GetSpCntlrLocal(
				pch,
				cnAgent.cnLocal.CnId,
				spId,
				cntlrId,
			)
			if err != nil {
				pch.Logger.Fatal(
					"GetSpCntlrLocal err: %s %s %s %v",
					cnAgent.cnLocal.CnId,
					spId,
					cntlrId,
					err,
				)
			}
			spCntlrData = &spCntlrRuntimeData{
				spCntlrLocal: spCntlrLocal,
			}
			cnAgent.spCntlrMap[key] = spCntlrData
		}
		spCntlrData.mu.Lock()
		if _, ok := cnAgent.cnLocal.DeadSpCntlrMap[key]; ok {
			spCntlrData.spCntlrLocal.Revision = constants.RevisionDeleted
			if err := cnAgent.local.SetSpCntlrLocal(
				pch,
				spCntlrData.spCntlrLocal,
			); err != nil {
				spCntlrData.mu.Unlock()
				return &pbnd.SyncupCnReply{
					CnInfo: &pbnd.CnInfo{
						StatusInfo: &pbnd.StatusInfo{
							Code:      constants.StatusCodeInternalErr,
							Msg:       err.Error(),
							Timestamp: pch.Timestamp,
						},
					},
				}, nil
			}
		}
		spCntlrData.mu.Unlock()
	}

	if err := cnAgent.local.SetCnLocal(pch, cnAgent.cnLocal); err != nil {
		return &pbnd.SyncupCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	return &pbnd.SyncupCnReply{
		CnInfo: &pbnd.CnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: pch.Timestamp,
			},
		},
	}, nil
}

func (cnAgent *cnAgentServer) fetchDeadSpCntlr(
	pch *ctxhelper.PerCtxHelper,
) map[string]*spCntlrRuntimeData {
	keyToSpCntlr := make(map[string]*spCntlrRuntimeData)

	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	if cnAgent.cnLocal != nil {
		for key := range cnAgent.cnLocal.DeadSpCntlrMap {
			spCntlrData, ok := cnAgent.spCntlrMap[key]
			if !ok {
				pch.Logger.Fatal("Can not find key in spCntlrMap: %s", key)
			}
			keyToSpCntlr[key] = spCntlrData
		}
	}

	return keyToSpCntlr
}

func (cnAgent *cnAgentServer) updateDeadSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	deleted []string,
) {
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	if cnAgent.cnLocal != nil {
		for _, key := range deleted {
			delete(cnAgent.cnLocal.DeadSpCntlrMap, key)
			delete(cnAgent.spCntlrMap, key)
		}
	}

	if err := cnAgent.local.SetCnLocal(
		pch,
		cnAgent.cnLocal,
	); err != nil {
		pch.Logger.Fatal("SetCnLocal err: %v", err)
	}
}

func (cnAgent *cnAgentServer) cleanup(
	pch *ctxhelper.PerCtxHelper,
	keyToSpCntlr map[string]*spCntlrRuntimeData,
) []string {
	deleted := make([]string, 0)
	for key, spCntlrData := range keyToSpCntlr {
		spCntlrData.mu.Lock()
		err := cleanupSpCntlr(
			pch,
			cnAgent.oc,
			cnAgent.nf,
			spCntlrData.spCntlrLocal,
		)
		spCntlrData.mu.Unlock()
		if err != nil {
			pch.Logger.Error("cleanupSpCntlr err: %v", err)
			continue
		}
		deleted = append(deleted, key)
	}
	return deleted
}

func (cnAgent *cnAgentServer) background(
	parentCtx context.Context,
) {
	traceId := uuid.New().String()
	logPrefix := fmt.Sprintf("CnCleanUp|%s", traceId)
	logger := prefixlog.NewPrefixLogger(logPrefix)
	pch := ctxhelper.NewPerCtxHelper(parentCtx, logger, traceId)
	select {
	case <-pch.Ctx.Done():
		return
	case <-time.After(cnAgent.bgInterval):
		keyToSpCntlr := cnAgent.fetchDeadSpCntlr(pch)
		deleted := cnAgent.cleanup(pch, keyToSpCntlr)
		cnAgent.updateDeadSpCntlr(pch, deleted)
	}
}

func (cnAgent *cnAgentServer) CheckCn(
	ctx context.Context,
	req *pbnd.CheckCnRequest,
) (*pbnd.CheckCnReply, error) {
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()

	timestamp := time.Now().UnixMilli()

	if cnAgent.cnLocal == nil {
		return &pbnd.CheckCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if cnAgent.cnLocal.CnId != req.CnId {
		return &pbnd.CheckCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("CnId: %s", cnAgent.cnLocal.CnId),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if cnAgent.cnLocal.Revision != req.Revision {
		return &pbnd.CheckCnReply{
			CnInfo: &pbnd.CnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("Revision: %s", cnAgent.cnLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.CheckCnReply{
		CnInfo: &pbnd.CnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func (cnAgent *cnAgentServer) getSpCntlrData(
	cnId string,
	spId string,
	cntlrId string,
) *spCntlrRuntimeData {
	key := encodeSpCntlrId(spId, cntlrId)
	cnAgent.mu.Lock()
	defer cnAgent.mu.Unlock()
	if spCntlrData, ok := cnAgent.spCntlrMap[key]; ok {
		return spCntlrData
	}
	return nil
}

func (cnAgent *cnAgentServer) SyncupSpCntlr(
	ctx context.Context,
	req *pbnd.SyncupSpCntlrRequest,
) (*pbnd.SyncupSpCntlrReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	spCntlrData := cnAgent.getSpCntlrData(
		req.SpCntlrConf.CnId,
		req.SpCntlrConf.SpId,
		req.SpCntlrConf.CntlrId,
	)
	if spCntlrData == nil {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code: constants.StatusCodeNotFound,
					Msg: fmt.Sprintf(
						"Do not find spCntlrData: %s %s %s",
						req.SpCntlrConf.CnId,
						req.SpCntlrConf.SpId,
						req.SpCntlrConf.CntlrId,
					),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	spCntlrData.mu.Lock()
	defer spCntlrData.mu.Unlock()

	if spCntlrData.spCntlrLocal.Revision == constants.RevisionDeleted {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spCntlrData.spCntlrLocal.Revision),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrLocal.Revision > req.SpCntlrConf.Revision {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spCntlrData.spCntlrLocal.Revision),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrLocal.Revision == constants.RevisionUninit {
		spCntlrData.spCntlrLocal.PortNum = req.SpCntlrConf.NvmePortConf.PortNum
	} else {
		if spCntlrData.spCntlrLocal.PortNum != req.SpCntlrConf.NvmePortConf.PortNum {
			pch.Logger.Fatal(
				"SpCntlr PortNum mismatch: %s %s",
				spCntlrData.spCntlrLocal.PortNum,
				req.SpCntlrConf.NvmePortConf.PortNum,
			)
		}
	}
	spCntlrData.spCntlrLocal.Revision = req.SpCntlrConf.Revision

	if err := cnAgent.local.SetSpCntlrLocal(
		pch,
		spCntlrData.spCntlrLocal,
	); err != nil {
		return &pbnd.SyncupSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeInternalErr,
					Msg:       err.Error(),
					Timestamp: pch.Timestamp,
				},
			},
		}, nil
	}

	spCntlrInfo := syncupSpCntlr(
		pch,
		cnAgent.oc,
		cnAgent.nf,
		req.SpCntlrConf,
	)

	spCntlrData.spCntlrConf = req.SpCntlrConf

	return &pbnd.SyncupSpCntlrReply{
		SpCntlrInfo: spCntlrInfo,
	}, nil
}

func (cnAgent *cnAgentServer) CheckSpCntlr(
	ctx context.Context,
	req *pbnd.CheckSpCntlrRequest,
) (*pbnd.CheckSpCntlrReply, error) {
	timestamp := time.Now().UnixMilli()

	spCntlrData := cnAgent.getSpCntlrData(
		req.CnId,
		req.SpId,
		req.CntlrId,
	)
	if spCntlrData == nil {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	spCntlrData.mu.Lock()
	defer spCntlrData.mu.Unlock()

	if spCntlrData.spCntlrLocal.Revision == constants.RevisionDeleted {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeOldRevision,
					Msg:       fmt.Sprintf("Revision: %d", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrLocal.Revision != req.Revision {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeDataMismatch,
					Msg:       fmt.Sprintf("Revision: %s", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if spCntlrData.spCntlrConf == nil {
		return &pbnd.CheckSpCntlrReply{
			SpCntlrInfo: &pbnd.SpCntlrInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code:      constants.StatusCodeNoConf,
					Msg:       fmt.Sprintf("Revision: %s", spCntlrData.spCntlrLocal.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.CheckSpCntlrReply{
		SpCntlrInfo: &pbnd.SpCntlrInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeSucceed,
				Msg:       constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func newCnAgentServer(
	ctx context.Context,
	dataPath string,
	bgInterval time.Duration,
) *cnAgentServer {
	cnAgent := &cnAgentServer{
		oc: oscmd.NewOsCommand(),
		nf: namefmt.NewNameFmt(
			constants.DeviceMapperPrefixDefault,
			constants.NqnPrefixDefault,
		),
		local:      localdata.NewLocalClient(dataPath),
		cnLocal:    nil,
		bgInterval: bgInterval,
	}
	go cnAgent.background(ctx)
	return cnAgent
}
