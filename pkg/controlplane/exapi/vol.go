package exapi

import (
	"context"
	"fmt"
	"strconv"

	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/mbrhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type raid1DnLd struct {
	dnId                       string
	dnConf                     *pbcp.DiskNodeConf
	thinMetaRaid1MetaLdId      string
	thinMetaRaid1MetaStart     uint32
	thinMetaRaid1MetaExtentCnt uint32
	thinMetaRaid1DataLdId      string
	thinMetaRaid1DataStart     uint32
	thinMetaRaid1DataExtentCnt uint32
	thinDataRaid1MetaLdId      string
	thinDataRaid1MetaStart     uint32
	thinDataRaid1MetaExtentCnt uint32
	thinDataRaid1DataLdId      string
	thinDataRaid1DataStart     uint32
	thinDataRaid1DataExtentCnt uint32
}

type generalCnCntlr struct {
	cnId    string
	cnConf  *pbcp.ControllerNodeConf
	cntlrId string
	portNum uint32
}

func (exApi *exApiServer) tryToCreateVol(
	pch *ctxhelper.PerCtxHelper,
	req *pbcp.CreateVolRequest,
	metaExtentSize uint32,
	dataExtentSize uint32,
	dataExtentCnt uint32,
	dnValueToId map[string]string,
	cnValueToId map[string]string,
) ([]string, []string, error) {
	pch.Logger.Debug("metaExtentSize=%v", metaExtentSize)
	pch.Logger.Debug("dataExtentSize=%v", dataExtentSize)
	pch.Logger.Debug("dataExtentCnt=%v", dataExtentCnt)
	pch.Logger.Debug("dnValueToId=%v", dnValueToId)
	pch.Logger.Debug("cnValueToId=%v", cnValueToId)
	legCnt := int(req.CntlrCnt * req.LegPerCntlr)
	dnCnt := legCnt * 2
	cnCnt := int(req.CntlrCnt)

	thinMetaRaid1MetaExtentCnt := uint32(1)
	thinMetaRaid1DataExtentCnt := thinMetaExtentCntCalc(
		metaExtentSize,
		dataExtentSize,
		dataExtentCnt,
		constants.ThinBlockSizeDefault,
	)
	thinDataRaid1MetaExtentCnt := uint32(1)
	thinDataRaid1DataExtentCnt := dataExtentCnt

	spGlobalKey := exApi.kf.SpGlobalEntityKey()
	spGlobal := &pbcp.SpGlobal{}

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.VolName)

	r1DnLdList := make([]*raid1DnLd, 0)
	invalidDnList := make([]string, 0)
	genCnCntlrList := make([]*generalCnCntlr, 0)
	invalidCnList := make([]string, 0)
	apply := func(stm concurrency.STM) error {
		spGlobalValOld := []byte(stm.Get(spGlobalKey))
		if len(spGlobalValOld) == 0 {
			pch.Logger.Error("No spGlobal: %s", spGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				spGlobalKey,
			}
		}
		if err := proto.Unmarshal(spGlobalValOld, spGlobal); err != nil {
			pch.Logger.Error(
				"spGlobal unmarshal err: %s %v",
				spGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spGlobal.GlobalCounter++
		counter := spGlobal.GlobalCounter
		minCnt := constants.Uint32Max
		idx := -1
		for i, cnt := range spGlobal.ShardBucket {
			if cnt < minCnt {
				minCnt = cnt
				idx = i
			}
		}
		if idx < 0 {
			panic("Do not find minimal cnt")
		}
		spGlobal.ShardBucket[idx] = spGlobal.ShardBucket[idx] + 1
		spGlobalVal, err := proto.Marshal(spGlobal)
		if err != nil {
			pch.Logger.Error(
				"spGlobal marshal err: %v %v",
				spGlobal,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spGlobalStr := string(spGlobalVal)
		stm.Put(spGlobalKey, spGlobalStr)

		spIdNum := (uint64(idx) << (constants.ShardMove)) | counter
		spId := fmt.Sprintf("%016x", spIdNum)
		spCounter := uint64(0)

		for _, dnId := range dnValueToId {
			if len(r1DnLdList) >= dnCnt {
				break
			}
			dnConfKey := exApi.kf.DnConfEntityKey(dnId)
			dnConfVal := []byte(stm.Get(dnConfKey))
			dnConf := &pbcp.DiskNodeConf{}
			if err := proto.Unmarshal(dnConfVal, dnConf); err != nil {
				pch.Logger.Error(
					"dnConf unmarshal err: %s %v",
					dnConfKey,
					err,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			if !dnConf.GeneralConf.Online {
				pch.Logger.Warning(
					"Skip not online dn: %s",
					dnId,
				)
				invalidDnList = append(invalidDnList, dnId)
				continue
			}

			dnInfoKey := exApi.kf.DnInfoEntityKey(dnId)
			dnInfoVal := []byte(stm.Get(dnInfoKey))
			dnInfo := &pbcp.DiskNodeInfo{}
			if err := proto.Unmarshal(dnInfoVal, dnInfo); err != nil {
				pch.Logger.Error(
					"dnInfo unmarshal err: %s %v",
					dnInfoKey,
					err,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}

			if dnInfo.StatusInfo.Code != constants.StatusCodeSucceed {
				pch.Logger.Warning(
					"Skip error dn: %s %v",
					dnId,
					dnInfo.StatusInfo,
				)
				invalidDnList = append(invalidDnList, dnId)
				continue
			}

			thinMetaRaid1MetaLdId := fmt.Sprintf("%016x", spCounter)
			spCounter++
			thinMetaRaid1MetaStart, thinMetaRaid1MetaExtentCnt, err := allocateLd(
				dnConf.GeneralConf.MetaExtentConf,
				thinMetaRaid1MetaExtentCnt,
				1<<constants.MetaExtentPerSetShiftDefault,
			)
			if err != nil {
				pch.Logger.Warning(
					"Allocate thin meta raid1 meta failed: %s %v",
					dnId,
					err,
				)
				invalidDnList = append(invalidDnList, dnId)
				continue
			}
			dnConf.SpLdIdList = append(
				dnConf.SpLdIdList,
				&pbcp.SpLdId{
					SpId: spId,
					LdId: thinMetaRaid1MetaLdId,
				},
			)

			thinMetaRaid1DataLdId := fmt.Sprintf("%016x", spCounter)
			spCounter++
			thinMetaRaid1DataStart, thinMetaRaid1DataExtentCnt, err := allocateLd(
				dnConf.GeneralConf.MetaExtentConf,
				thinMetaRaid1DataExtentCnt,
				1<<constants.MetaExtentPerSetShiftDefault,
			)
			if err != nil {
				pch.Logger.Warning(
					"Allocate thin meta radi1 data failed: %s %v",
					dnId,
					err,
				)
				invalidDnList = append(invalidDnList, dnId)
				continue
			}
			dnConf.SpLdIdList = append(
				dnConf.SpLdIdList,
				&pbcp.SpLdId{
					SpId: spId,
					LdId: thinMetaRaid1DataLdId,
				},
			)

			thinDataRaid1MetaLdId := fmt.Sprintf("%016x", spCounter)
			spCounter++
			thinDataRaid1MetaStart, thinDataRaid1MetaExtentCnt, err := allocateLd(
				dnConf.GeneralConf.MetaExtentConf,
				thinDataRaid1MetaExtentCnt,
				1<<constants.MetaExtentPerSetShiftDefault,
			)
			if err != nil {
				pch.Logger.Warning(
					"Allocate thin data raid1 meta failed: %s %v",
					dnId,
					err,
				)
				invalidDnList = append(invalidDnList, dnId)
				continue
			}
			dnConf.SpLdIdList = append(
				dnConf.SpLdIdList,
				&pbcp.SpLdId{
					SpId: spId,
					LdId: thinDataRaid1MetaLdId,
				},
			)

			thinDataRaid1DataLdId := fmt.Sprintf("%016x", spCounter)
			spCounter++
			thinDataRaid1DataStart, thinDataRaid1DataExtentCnt, err := allocateLd(
				dnConf.GeneralConf.MetaExtentConf,
				thinDataRaid1DataExtentCnt,
				1<<constants.DataExtentPerSetShiftDefault,
			)
			if err != nil {
				pch.Logger.Warning(
					"Allocate thin data raid1 data failed: %s %v",
					dnId,
					err,
				)
				invalidDnList = append(invalidDnList, dnId)
				continue
			}
			dnConf.SpLdIdList = append(
				dnConf.SpLdIdList,
				&pbcp.SpLdId{
					SpId: spId,
					LdId: thinDataRaid1DataLdId,
				},
			)

			r1DnLd := &raid1DnLd{
				dnId:                       dnId,
				dnConf:                     dnConf,
				thinMetaRaid1MetaLdId:      thinMetaRaid1MetaLdId,
				thinMetaRaid1MetaStart:     thinMetaRaid1MetaStart,
				thinMetaRaid1MetaExtentCnt: thinMetaRaid1MetaExtentCnt,
				thinMetaRaid1DataLdId:      thinMetaRaid1DataLdId,
				thinMetaRaid1DataStart:     thinMetaRaid1DataStart,
				thinMetaRaid1DataExtentCnt: thinMetaRaid1DataExtentCnt,
				thinDataRaid1MetaLdId:      thinDataRaid1MetaLdId,
				thinDataRaid1MetaStart:     thinDataRaid1MetaStart,
				thinDataRaid1MetaExtentCnt: thinDataRaid1MetaExtentCnt,
				thinDataRaid1DataLdId:      thinDataRaid1DataLdId,
				thinDataRaid1DataStart:     thinDataRaid1DataStart,
				thinDataRaid1DataExtentCnt: thinDataRaid1DataExtentCnt,
			}
			r1DnLdList = append(r1DnLdList, r1DnLd)

			dnConfValNew, err := proto.Marshal(dnConf)
			if err != nil {
				pch.Logger.Error("Marshal dnConf err: %v %v", dnConf, err)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			dnConfStrNew := string(dnConfValNew)
			stm.Put(dnConfKey, dnConfStrNew)
		}

		for _, cnId := range cnValueToId {
			if len(genCnCntlrList) >= cnCnt {
				break
			}
			cnConfKey := exApi.kf.CnConfEntityKey(cnId)
			cnConfVal := []byte(stm.Get(cnConfKey))
			cnConf := &pbcp.ControllerNodeConf{}
			if err := proto.Unmarshal(cnConfVal, cnConf); err != nil {
				pch.Logger.Error(
					"cnConf unmarshal err: %s %v",
					cnConfKey,
					err,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			if !cnConf.GeneralConf.Online {
				pch.Logger.Warning(
					"Skip not online cn: %s",
					cnId,
				)
				invalidCnList = append(invalidCnList, cnId)
				continue
			}

			cnInfoKey := exApi.kf.CnInfoEntityKey(cnId)
			cnInfoVal := []byte(stm.Get(cnInfoKey))
			cnInfo := &pbcp.ControllerNodeInfo{}
			if err := proto.Unmarshal(cnInfoVal, cnInfo); err != nil {
				pch.Logger.Error(
					"cnInfo unmarshal err: %s %v",
					cnInfoKey,
					err,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}

			if cnInfo.StatusInfo.Code != constants.StatusCodeSucceed {
				pch.Logger.Warning(
					"Skip error cn: %s %v",
					cnId,
					cnInfo.StatusInfo,
				)
				invalidCnList = append(invalidCnList, cnId)
				continue
			}
			cntlrId := fmt.Sprintf("%016x", spCounter)
			spCounter++

			portNum, err := getAndUpdateNextBit(cnConf.GeneralConf.PortNextBit)
			if err != nil {
				pch.Logger.Error(
					"get portNum err: %v",
					err,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			genCnCntlr := &generalCnCntlr{
				cnId:    cnId,
				cnConf:  cnConf,
				cntlrId: cntlrId,
				portNum: portNum,
			}
			genCnCntlrList = append(genCnCntlrList, genCnCntlr)

			cnConf.SpCntlrIdList = append(
				cnConf.SpCntlrIdList,
				&pbcp.SpCntlrId{
					SpId:    spId,
					CntlrId: cntlrId,
				},
			)

			cnConfValNew, err := proto.Marshal(cnConf)
			if err != nil {
				pch.Logger.Error("Marshal cnConf err: %v %v", cnConf, err)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			cnConfStrNew := string(cnConfValNew)
			stm.Put(cnConfKey, cnConfStrNew)
		}

		if len(r1DnLdList) < dnCnt || len(genCnCntlrList) < cnCnt {
			pch.Logger.Warning("No enough dn and/or cn")
			return &stmwrapper.StmError{
				constants.ReplyCodeNeedMore,
				"",
			}
		}

		snapConf := &pbcp.SnapConf{
			DevId:    0,
			OriId:    constants.Uint32Max,
			SnapName: "default",
		}
		snapConfList := make([]*pbcp.SnapConf, 1)
		snapConfList[0] = snapConf

		ssId := fmt.Sprintf("%016x", spCounter)
		spCounter++
		ssConfList := make([]*pbcp.SsConf, 1)
		ssConf := &pbcp.SsConf{
			SsId:         ssId,
			NsNextBit:    initNextBit(constants.NsBitSizeDefault),
			NsConfList:   make([]*pbcp.NsConf, 1),
			HostConfList: make([]*pbcp.HostConf, 0),
		}
		nsId := fmt.Sprintf("%016x", spCounter)
		spCounter++
		nsNum, err := getAndUpdateNextBit(ssConf.NsNextBit)
		if err != nil {
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		ssConf.NsConfList[0] = &pbcp.NsConf{
			NsId:   nsId,
			NsName: "default",
			NsNum:  string(nsNum),
			Size:   req.Size,
			DevId:  0,
		}
		ssInfoList := make([]*pbcp.SsInfo, 1)
		ssInfoList[0] = &pbcp.SsInfo{
			SsId:               ssId,
			SsPerCntlrInfoList: make([]*pbcp.SsPerCntlrInfo, req.CntlrCnt),
		}
		for i, genCnCntlr := range genCnCntlrList {
			ssInfoList[0].SsPerCntlrInfoList[i] = &pbcp.SsPerCntlrInfo{
				CntlrId: genCnCntlr.cntlrId,
				StatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				NsInfoList: make([]*pbcp.NsInfo, 1),
			}
			ssInfoList[0].SsPerCntlrInfoList[i].NsInfoList[0] = &pbcp.NsInfo{
				NsId: nsId,
				StatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
		}
		legConfList := make([]*pbcp.LegConf, legCnt)
		legInfoList := make([]*pbcp.LegInfo, legCnt)
		for i := 0; i < legCnt; i++ {
			cntlrIdx := uint32(i) % req.CntlrCnt
			r1DnLd0 := r1DnLdList[2*i]
			r1DnLd1 := r1DnLdList[2*i+1]
			legId := fmt.Sprintf("%016x", spCounter)
			spCounter++
			legConf := &pbcp.LegConf{
				LegId:       legId,
				LegIdx:      uint32(i),
				AcCntlrId:   genCnCntlrList[cntlrIdx].cntlrId,
				Reload:      false,
				GrpConfList: make([]*pbcp.GrpConf, 1),
			}
			grpId := fmt.Sprintf("%016x", spCounter)
			spCounter++
			legConf.GrpConfList[0] = &pbcp.GrpConf{
				GrpId:         grpId,
				GrpIdx:        uint32(0),
				MetaExtentCnt: uint32(thinMetaRaid1DataExtentCnt),
				DataExtentCnt: uint32(thinDataRaid1DataExtentCnt),
				LdConfList:    make([]*pbcp.LdConf, 8),
				NoSync:        true,
				RebuildIdx:    constants.Uint32Max,
				OmitIdxList:   make([]uint32, 0),
			}

			legConf.GrpConfList[0].LdConfList[0] = &pbcp.LdConf{
				LdId:           r1DnLd0.thinMetaRaid1MetaLdId,
				DnId:           r1DnLd0.dnId,
				DnGrpcTarget:   r1DnLd0.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd0.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd0.thinMetaRaid1MetaStart,
				Cnt:            r1DnLd0.thinMetaRaid1MetaExtentCnt,
				ExtentSize:     metaExtentSize,
				Inited:         false,
			}
			legConf.GrpConfList[0].LdConfList[1] = &pbcp.LdConf{
				LdId:           r1DnLd0.thinMetaRaid1DataLdId,
				DnId:           r1DnLd0.dnId,
				DnGrpcTarget:   r1DnLd0.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd0.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd0.thinMetaRaid1DataStart,
				Cnt:            r1DnLd0.thinMetaRaid1DataExtentCnt,
				ExtentSize:     metaExtentSize,
				Inited:         false,
			}
			legConf.GrpConfList[0].LdConfList[2] = &pbcp.LdConf{
				LdId:           r1DnLd0.thinDataRaid1MetaLdId,
				DnId:           r1DnLd0.dnId,
				DnGrpcTarget:   r1DnLd0.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd0.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd0.thinDataRaid1MetaStart,
				Cnt:            r1DnLd0.thinDataRaid1MetaExtentCnt,
				ExtentSize:     metaExtentSize,
				Inited:         false,
			}
			legConf.GrpConfList[0].LdConfList[3] = &pbcp.LdConf{
				LdId:           r1DnLd0.thinDataRaid1DataLdId,
				DnId:           r1DnLd0.dnId,
				DnGrpcTarget:   r1DnLd0.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd0.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd0.thinDataRaid1DataStart,
				Cnt:            r1DnLd0.thinDataRaid1DataExtentCnt,
				ExtentSize:     dataExtentSize,
				Inited:         false,
			}

			legConf.GrpConfList[0].LdConfList[4] = &pbcp.LdConf{
				LdId:           r1DnLd1.thinMetaRaid1MetaLdId,
				DnId:           r1DnLd1.dnId,
				DnGrpcTarget:   r1DnLd1.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd1.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd1.thinMetaRaid1MetaStart,
				Cnt:            r1DnLd1.thinMetaRaid1MetaExtentCnt,
				ExtentSize:     metaExtentSize,
				Inited:         false,
			}
			legConf.GrpConfList[0].LdConfList[5] = &pbcp.LdConf{
				LdId:           r1DnLd1.thinMetaRaid1DataLdId,
				DnId:           r1DnLd1.dnId,
				DnGrpcTarget:   r1DnLd1.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd1.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd1.thinMetaRaid1DataStart,
				Cnt:            r1DnLd1.thinMetaRaid1DataExtentCnt,
				ExtentSize:     metaExtentSize,
				Inited:         false,
			}
			legConf.GrpConfList[0].LdConfList[6] = &pbcp.LdConf{
				LdId:           r1DnLd1.thinDataRaid1MetaLdId,
				DnId:           r1DnLd1.dnId,
				DnGrpcTarget:   r1DnLd1.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd1.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd1.thinDataRaid1MetaStart,
				Cnt:            r1DnLd1.thinDataRaid1MetaExtentCnt,
				ExtentSize:     metaExtentSize,
				Inited:         false,
			}
			legConf.GrpConfList[0].LdConfList[7] = &pbcp.LdConf{
				LdId:           r1DnLd1.thinDataRaid1DataLdId,
				DnId:           r1DnLd1.dnId,
				DnGrpcTarget:   r1DnLd1.dnConf.GeneralConf.GrpcTarget,
				DnNvmeListener: r1DnLd1.dnConf.GeneralConf.NvmePortConf.NvmeListener,
				LdIdx:          0,
				Start:          r1DnLd1.thinDataRaid1DataStart,
				Cnt:            r1DnLd1.thinDataRaid1DataExtentCnt,
				ExtentSize:     dataExtentSize,
				Inited:         false,
			}
			legConfList[i] = legConf

			legInfo := &pbcp.LegInfo{
				LegId: legId,
				StatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				ThinPoolInfo:      &pbcp.ThinPoolInfo{},
				RemoteLegInfoList: make([]*pbcp.RemoteLegInfo, 0),
				GrpInfoList:       make([]*pbcp.GrpInfo, 1),
			}
			for j, genCnCntlr := range genCnCntlrList {
				if uint32(j) == cntlrIdx {
					continue
				}
				remoteLegInfo := &pbcp.RemoteLegInfo{
					CntlrId: genCnCntlr.cntlrId,
					StatusInfo: &pbcp.StatusInfo{
						Code:      constants.StatusCodeUninit,
						Msg:       "uninit",
						Timestamp: pch.Timestamp,
					},
					FenceInfo: &pbcp.FenceInfo{},
				}
				legInfo.RemoteLegInfoList = append(
					legInfo.RemoteLegInfoList,
					remoteLegInfo,
				)
			}
			legInfo.GrpInfoList[0] = &pbcp.GrpInfo{
				GrpId: grpId,
				StatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				MetaRedunInfo: &pbcp.RedundancyInfo{},
				DataRedunInfo: &pbcp.RedundancyInfo{},
				LdInfoList:    make([]*pbcp.LdInfo, 8),
			}
			legInfo.GrpInfoList[0].LdInfoList[0] = &pbcp.LdInfo{
				LdId: r1DnLd0.thinMetaRaid1MetaLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[1] = &pbcp.LdInfo{
				LdId: r1DnLd0.thinMetaRaid1DataLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[2] = &pbcp.LdInfo{
				LdId: r1DnLd0.thinDataRaid1MetaLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[3] = &pbcp.LdInfo{
				LdId: r1DnLd0.thinDataRaid1DataLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[4] = &pbcp.LdInfo{
				LdId: r1DnLd1.thinMetaRaid1MetaLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[5] = &pbcp.LdInfo{
				LdId: r1DnLd1.thinMetaRaid1DataLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[6] = &pbcp.LdInfo{
				LdId: r1DnLd1.thinDataRaid1MetaLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfo.GrpInfoList[0].LdInfoList[7] = &pbcp.LdInfo{
				LdId: r1DnLd1.thinDataRaid1DataLdId,
				DnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
				CnStatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
			legInfoList[i] = legInfo
		}

		cntlrConfList := make([]*pbcp.CntlrConf, req.CntlrCnt)
		cntlrInfoList := make([]*pbcp.CntlrInfo, req.CntlrCnt)
		for i := uint32(0); i < req.CntlrCnt; i++ {
			genCnCntlr := genCnCntlrList[i]
			cnListener := genCnCntlr.cnConf.GeneralConf.NvmePortConf.NvmeListener
			baseTrSvcId, err := strconv.ParseUint(cnListener.TrSvcId, 10, 32)
			if err != nil {
				pch.Logger.Error(
					"Can not convert trSvcId to num: %s",
					cnListener.TrSvcId,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			cntlrConfList[i] = &pbcp.CntlrConf{
				CntlrId:      genCnCntlr.cntlrId,
				CnId:         genCnCntlr.cnId,
				CnGrpcTarget: genCnCntlr.cnConf.GeneralConf.GrpcTarget,
				CntlrIdx:     uint32(i),
				NvmePortConf: &pbcp.NvmePortConf{
					PortNum: string(genCnCntlr.portNum),
					NvmeListener: &pbcp.NvmeListener{
						TrType:  cnListener.TrType,
						AdrFam:  cnListener.AdrFam,
						TrAddr:  cnListener.TrAddr,
						TrSvcId: string(uint32(baseTrSvcId) + genCnCntlr.portNum),
					},
				},
			}
			cntlrInfoList[i] = &pbcp.CntlrInfo{
				CntlrId: genCnCntlr.cntlrId,
				StatusInfo: &pbcp.StatusInfo{
					Code:      constants.StatusCodeUninit,
					Msg:       "uninit",
					Timestamp: pch.Timestamp,
				},
			}
		}

		spConf := &pbcp.StoragePoolConf{
			TagList: req.TagList,
			GeneralConf: &pbcp.SpGeneralConf{
				SpName:       req.VolName,
				SpCounter:    spCounter,
				DevIdCounter: 0,
				Qos:          0,
				StripeConf: &pbcp.StripeConf{
					ChunkSize: constants.Raid0ChunkSizeDefault,
				},
				ThinPoolConf: &pbcp.ThinPoolConf{
					DataBlockSize:  constants.ThinBlockSizeDefault,
					LowWaterMark:   constants.ThinLowWaterMarkDefault,
					ErrorIfNoSpace: constants.ThinErrorIfNoSpaceDefault,
				},
				RedundancyConf: &pbcp.RedundancyConf{
					RedunType:  constants.RedunTypeRaid1,
					RegionSize: constants.RaidDataRegionSizeDefault,
				},
				DnAllocateConf: &pbcp.AllocateConf{
					DistinguishKey: req.DnDistinguishKey,
				},
				CnAllocateConf: &pbcp.AllocateConf{
					DistinguishKey: req.CnDistinguishKey,
				},
			},
			CreatingSnapConf: snapConf,
			DeletingSnapConf: nil,
			SnapConfList:     snapConfList,
			SsConfList:       ssConfList,
			LegConfList:      legConfList,
			CntlrConfList:    cntlrConfList,
			MtConfList:       make([]*pbcp.MtConf, 0),
			ItConfList:       make([]*pbcp.ItConf, 0),
		}
		spConfKey := exApi.kf.SpConfEntityKey(spId)
		spConfVal, err := proto.Marshal(spConf)
		if err != nil {
			pch.Logger.Error("Marshal spConf err: %v %v", spConf, err)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spConfStr := string(spConfVal)
		stm.Put(spConfKey, spConfStr)

		spInfo := &pbcp.StoragePoolInfo{
			ConfRev: 0,
			StatusInfo: &pbcp.StatusInfo{
				Code:      constants.StatusCodeUninit,
				Msg:       "uninit",
				Timestamp: pch.Timestamp,
			},
			SsInfoList:    ssInfoList,
			LegInfoList:   legInfoList,
			CntlrInfoList: cntlrInfoList,
			MtInfoList:    make([]*pbcp.MtInfo, 0),
			ItInfoList:    make([]*pbcp.ItInfo, 0),
		}
		spInfoKey := exApi.kf.SpInfoEntityKey(spId)
		spInfoVal, err := proto.Marshal(spInfo)
		if err != nil {
			pch.Logger.Error("Marshal spInfo err: %v %v", spInfo, err)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spInfoStr := string(spInfoVal)
		stm.Put(spInfoKey, spInfoStr)

		if val := stm.Get(nameToIdKey); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  nameToIdKey,
			}
		}
		nameToId := &pbcp.NameToId{
			ResId: spId,
		}
		nameToIdVal, err := proto.Marshal(nameToId)
		if err != nil {
			pch.Logger.Error(
				"nameToId marshal err: %v %v",
				nameToId,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		nameToIdStr := string(nameToIdVal)
		stm.Put(nameToIdKey, nameToIdStr)

		return nil
	}

	err := exApi.sm.RunStm(pch, apply)
	return invalidDnList, invalidCnList, err
}

func (exApi *exApiServer) CreateVol(
	ctx context.Context,
	req *pbcp.CreateVolRequest,
) (*pbcp.CreateVolReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	pch.Logger.Debug("CreateVol: %v", req)

	session, err := concurrency.NewSession(exApi.etcdCli,
		concurrency.WithTTL(constants.AllocLockTTL))
	if err != nil {
		pch.Logger.Error("Create session err: %v", err)
		return &pbcp.CreateVolReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	defer session.Close()
	mutex := concurrency.NewMutex(session, exApi.kf.AllocLockPath())
	if err = mutex.Lock(ctx); err != nil {
		pch.Logger.Error("Lock mutex err: %v", err)
		return &pbcp.CreateVolReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	defer func() {
		if err := mutex.Unlock(ctx); err != nil {
			pch.Logger.Error("Unlock mutex err: %v", err)
		}
	}()

	legCnt := uint64(req.CntlrCnt * req.LegPerCntlr)
	dnCnt := legCnt * 2
	size := (req.Size + legCnt - 1) / legCnt
	metaExtentSize := uint32(1 << constants.MetaExtentSizeShiftDefault)
	dataExtentSize := uint32(1 << constants.DataExtentSizeShiftDefault)
	dataExtentCnt := uint32(divRoundUp(size, uint64(dataExtentSize)))

	dnExcludeIdList := make([]string, 0)
	cnExcludeIdList := make([]string, 0)

	dnDistinguishKey := req.DnDistinguishKey
	if len(dnDistinguishKey) == 0 {
		dnDistinguishKey = constants.DefaultTagKey
	}

	cnDistinguishKey := req.CnDistinguishKey
	if len(cnDistinguishKey) == 0 {
		cnDistinguishKey = constants.DefaultTagKey
	}

	for i := 0; i < constants.AllocateRetryCntDefault; i++ {
		pch.Logger.Debug("AllocateRetryCnt: %v", i)
		allocateDnReq := &pbcp.AllocateDnRequest{
			DistinguishKey: dnDistinguishKey,
			DnCnt:          uint32(dnCnt),
			DataExtentCnt:  dataExtentCnt,
			ExcludeIdList:  dnExcludeIdList,
		}
		dnwkrTargetList, err := mbrhelper.GetAllMembers(
			exApi.etcdCli,
			pch,
			exApi.kf.DnMemberPrefix(),
		)
		if err != nil {
			return &pbcp.CreateVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
		dnValueToId := make(map[string]string, 0)
		for _, grpcTarget := range dnwkrTargetList {
			pch.Logger.Debug("grpcTarget: %v", grpcTarget)
			conn, err := grpc.DialContext(
				ctx,
				grpcTarget,
				grpc.WithInsecure(),
				grpc.WithBlock(),
				grpc.WithTimeout(exApi.wkrTimeout),
				grpc.WithChainUnaryInterceptor(
					ctxhelper.UnaryClientPerCtxHelperInterceptor,
				),
			)
			if err != nil {
				pch.Logger.Warning("Dial err: %v", err)
				return &pbcp.CreateVolReply{
					ReplyInfo: &pbcp.ReplyInfo{
						ReplyCode: constants.ReplyCodeInternalErr,
						ReplyMsg:  err.Error(),
					},
				}, nil
			}
			defer conn.Close()

			c := pbcp.NewDiskNodeWorkerClient(conn)
			allocateDnReply, err := c.AllocateDn(ctx, allocateDnReq)
			if err != nil {
				pch.Logger.Warning("AllocateDn err: %v", err)
				return &pbcp.CreateVolReply{
					ReplyInfo: &pbcp.ReplyInfo{
						ReplyCode: constants.ReplyCodeInternalErr,
						ReplyMsg:  err.Error(),
					},
				}, nil
			}
			for _, item := range allocateDnReply.DnItemList {
				if _, ok := dnValueToId[item.DistinguishValue]; !ok {
					dnValueToId[item.DistinguishValue] = item.DnId
				}
			}
		}

		if len(dnValueToId) < int(dnCnt) {
			return &pbcp.CreateVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeNoCapacity,
					ReplyMsg: fmt.Sprintf(
						"required dn: %d, available dn: %d, %v",
						dnCnt,
						len(dnValueToId),
						dnValueToId,
					),
				},
			}, nil
		}

		allocateCnReq := &pbcp.AllocateCnRequest{
			DistinguishKey: cnDistinguishKey,
			CnCnt:          req.CntlrCnt,
			ExcludeIdList:  cnExcludeIdList,
		}
		cnwkrTargetList, err := mbrhelper.GetAllMembers(
			exApi.etcdCli,
			pch,
			exApi.kf.CnMemberPrefix(),
		)
		if err != nil {
			return &pbcp.CreateVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
		cnValueToId := make(map[string]string, 0)
		for _, grpcTarget := range cnwkrTargetList {
			conn, err := grpc.DialContext(
				ctx,
				grpcTarget,
				grpc.WithInsecure(),
				grpc.WithBlock(),
				grpc.WithTimeout(exApi.wkrTimeout),
				grpc.WithChainUnaryInterceptor(
					ctxhelper.UnaryClientPerCtxHelperInterceptor,
				),
			)
			if err != nil {
				return &pbcp.CreateVolReply{
					ReplyInfo: &pbcp.ReplyInfo{
						ReplyCode: constants.ReplyCodeInternalErr,
						ReplyMsg:  err.Error(),
					},
				}, nil
			}
			defer conn.Close()

			c := pbcp.NewControllerNodeWorkerClient(conn)
			allocateCnReply, err := c.AllocateCn(ctx, allocateCnReq)
			if err != nil {
				return &pbcp.CreateVolReply{
					ReplyInfo: &pbcp.ReplyInfo{
						ReplyCode: constants.ReplyCodeInternalErr,
						ReplyMsg:  err.Error(),
					},
				}, nil
			}
			for _, item := range allocateCnReply.CnItemList {
				if _, ok := cnValueToId[item.DistinguishValue]; !ok {
					cnValueToId[item.DistinguishValue] = item.CnId
				}
			}
		}

		if len(cnValueToId) < int(req.CntlrCnt) {
			return &pbcp.CreateVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeNoCapacity,
					ReplyMsg: fmt.Sprintf(
						"required cn: %d, available cn: %d, %v",
						req.CntlrCnt,
						len(cnValueToId),
						cnValueToId,
					),
				},
			}, nil
		}

		invalidDnList, invalidCnList, err := exApi.tryToCreateVol(
			pch,
			req,
			metaExtentSize,
			dataExtentSize,
			dataExtentCnt,
			dnValueToId,
			cnValueToId,
		)
		if err != nil {
			if serr, ok := err.(*stmwrapper.StmError); ok {
				if serr.Code == constants.ReplyCodeNeedMore {
					for _, dnId := range invalidDnList {
						dnExcludeIdList = append(dnExcludeIdList, dnId)
					}
					for _, cnId := range invalidCnList {
						cnExcludeIdList = append(cnExcludeIdList, cnId)
					}
					pch.Logger.Warning(
						"Retry with dnExcludeIdList and cnExcludeIdList: %v %v",
						dnExcludeIdList,
						cnExcludeIdList,
					)
					continue
				} else {
					return &pbcp.CreateVolReply{
						ReplyInfo: &pbcp.ReplyInfo{
							ReplyCode: serr.Code,
							ReplyMsg:  serr.Msg,
						},
					}, nil
				}
			} else {
				return &pbcp.CreateVolReply{
					ReplyInfo: &pbcp.ReplyInfo{
						ReplyCode: constants.ReplyCodeInternalErr,
						ReplyMsg:  err.Error(),
					},
				}, nil
			}
		} else {
			return &pbcp.CreateVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeSucceed,
					ReplyMsg:  constants.ReplyMsgSucceed,
				},
			}, nil
		}
	}

	return &pbcp.CreateVolReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeInternalErr,
			ReplyMsg:  "Exceed max allocate retry",
		},
	}, nil
}

func (exApi *exApiServer) DeleteVol(
	ctx context.Context,
	req *pbcp.DeleteVolRequest,
) (*pbcp.DeleteVolReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	session, err := concurrency.NewSession(exApi.etcdCli,
		concurrency.WithTTL(constants.AllocLockTTL))
	if err != nil {
		pch.Logger.Error("Create session err: %v", err)
		return &pbcp.DeleteVolReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	defer session.Close()
	mutex := concurrency.NewMutex(session, exApi.kf.AllocLockPath())
	if err = mutex.Lock(ctx); err != nil {
		pch.Logger.Error("Lock mutex err: %v", err)
		return &pbcp.DeleteVolReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	defer func() {
		if err := mutex.Unlock(ctx); err != nil {
			pch.Logger.Error("Unlock mutex err: %v", err)
		}
	}()

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.VolName)
	nameToId := &pbcp.NameToId{}
	spConf := &pbcp.StoragePoolConf{}
	spGlobalKey := exApi.kf.SpGlobalEntityKey()
	spGlobal := &pbcp.SpGlobal{}

	apply := func(stm concurrency.STM) error {
		nameToIdVal := []byte(stm.Get(nameToIdKey))
		if len(nameToIdVal) == 0 {
			pch.Logger.Error("No nameToID: %s", nameToIdKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				nameToIdKey,
			}
		}

		if err := proto.Unmarshal(nameToIdVal, nameToId); err != nil {
			pch.Logger.Error(
				"nameToId unmarshal err: %s %v",
				nameToIdKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		stm.Del(nameToIdKey)

		spId := nameToId.ResId
		spIdNum, err := strconv.ParseUint(spId, 16, 64)
		if err != nil {
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		idx := spIdNum >> constants.ShardMove

		spConfKey := exApi.kf.SpConfEntityKey(spId)
		spConfVal := []byte(stm.Get(spConfKey))
		if err := proto.Unmarshal(spConfVal, spConf); err != nil {
			pch.Logger.Error(
				"spConf unmarshal err: %s %v",
				spConfKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		for _, legConf := range spConf.LegConfList {
			for _, grpConf := range legConf.GrpConfList {
				for _, ldConf := range grpConf.LdConfList {
					dnId := ldConf.DnId
					ldId := ldConf.LdId
					dnConfKey := exApi.kf.DnConfEntityKey(dnId)
					dnConfVal := []byte(stm.Get(dnConfKey))
					dnConf := &pbcp.DiskNodeConf{}
					if err := proto.Unmarshal(dnConfVal, dnConf); err != nil {
						pch.Logger.Error(
							"dnConf unmarshal err: %s %v",
							dnConfKey,
							err,
						)
						return &stmwrapper.StmError{
							constants.ReplyCodeInternalErr,
							err.Error(),
						}
					}

					metaExtentSize := uint32(1 << constants.MetaExtentSizeShiftDefault)
					if ldConf.ExtentSize == metaExtentSize {
						freeLd(
							dnConf.GeneralConf.MetaExtentConf,
							ldConf.Start,
							ldConf.Cnt,
							1<<constants.MetaExtentPerSetShiftDefault,
						)
					} else {
						freeLd(
							dnConf.GeneralConf.DataExtentConf,
							ldConf.Start,
							ldConf.Cnt,
							1<<constants.DataExtentPerSetShiftDefault,
						)
					}

					lastIdx := len(dnConf.SpLdIdList) - 1
					for i, spLdId := range dnConf.SpLdIdList {
						if spLdId.SpId == spId && spLdId.LdId == ldId {
							dnConf.SpLdIdList[i] = dnConf.SpLdIdList[lastIdx]
							dnConf.SpLdIdList = dnConf.SpLdIdList[:lastIdx]
							break
						}
					}
					dnConfValNew, err := proto.Marshal(dnConf)
					if err != nil {
						pch.Logger.Error("Marshal dnConf err: %v %v", dnConf, err)
						return &stmwrapper.StmError{
							constants.ReplyCodeInternalErr,
							err.Error(),
						}
					}
					dnConfStrNew := string(dnConfValNew)
					stm.Put(dnConfKey, dnConfStrNew)
				}
			}
		}

		for _, cntlrConf := range spConf.CntlrConfList {
			cnId := cntlrConf.CnId
			cntlrId := cntlrConf.CntlrId
			cnConfKey := exApi.kf.CnConfEntityKey(cnId)
			cnConfVal := []byte(stm.Get(cnConfKey))
			cnConf := &pbcp.ControllerNodeConf{}
			if err := proto.Unmarshal(cnConfVal, cnConf); err != nil {
				pch.Logger.Error(
					"cnConf unmarshal err: %s %v",
					cnConfKey,
					err,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			lastIdx := len(cnConf.SpCntlrIdList) - 1
			for i, spCntlrId := range cnConf.SpCntlrIdList {
				if spCntlrId.SpId == spId && spCntlrId.CntlrId == cntlrId {
					cnConf.SpCntlrIdList[i] = cnConf.SpCntlrIdList[lastIdx]
					cnConf.SpCntlrIdList = cnConf.SpCntlrIdList[:lastIdx]
					break
				}
			}
			cnConfValNew, err := proto.Marshal(cnConf)
			if err != nil {
				pch.Logger.Error("Marshal cnConf err: %v %v", cnConf, err)
				return &stmwrapper.StmError{
					constants.ReplyCodeInternalErr,
					err.Error(),
				}
			}
			cnConfStrNew := string(cnConfValNew)
			stm.Put(cnConfKey, cnConfStrNew)
		}

		spInfoKey := exApi.kf.SpInfoEntityKey(spId)
		if len(stm.Get(spInfoKey)) > 0 {
			stm.Del(spInfoKey)
		}

		spGlobalOldVal := []byte(stm.Get(spGlobalKey))
		if len(spGlobalOldVal) == 0 {
			pch.Logger.Error("No spGlobal: %s", spGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				spGlobalKey,
			}
		}
		if err := proto.Unmarshal(spGlobalOldVal, spGlobal); err != nil {
			pch.Logger.Error(
				"spGlobal unmarshal err: %s %v",
				spGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		if spGlobal.ShardBucket[idx] == 0 {
			panic("ShardBucket underflow")
		}
		spGlobal.ShardBucket[idx] = spGlobal.ShardBucket[idx] - 1
		spGlobalVal, err := proto.Marshal(spGlobal)
		if err != nil {
			pch.Logger.Error(
				"spGlobal marshal err: %s %v",
				spGlobal,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spGlobalStr := string(spGlobalVal)
		stm.Put(spGlobalKey, spGlobalStr)

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.DeleteVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.DeleteVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.DeleteVolReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) GetVol(
	ctx context.Context,
	req *pbcp.GetVolRequest,
) (*pbcp.GetVolReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.VolName)
	nameToId := &pbcp.NameToId{}
	spConf := &pbcp.StoragePoolConf{}
	spInfo := &pbcp.StoragePoolInfo{}

	apply := func(stm concurrency.STM) error {
		nameToIdVal := []byte(stm.Get(nameToIdKey))
		if len(nameToIdVal) == 0 {
			pch.Logger.Error("No nameToID: %s", nameToIdKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				nameToIdKey,
			}
		}
		if err := proto.Unmarshal(nameToIdVal, nameToId); err != nil {
			pch.Logger.Error(
				"nameToId unmarshal err: %s %v",
				nameToIdKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		spId := nameToId.ResId

		spConfKey := exApi.kf.SpConfEntityKey(spId)
		spConfVal := []byte(stm.Get(spConfKey))
		if err := proto.Unmarshal(spConfVal, spConf); err != nil {
			pch.Logger.Error(
				"spConf unmarshal err: %s %v",
				spConfKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		spInfoKey := exApi.kf.SpInfoEntityKey(spId)
		spInfoVal := []byte(stm.Get(spInfoKey))
		if err := proto.Unmarshal(spInfoVal, spInfo); err != nil {
			pch.Logger.Error(
				"spInfo unmarshal err: %s %v",
				spInfoKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.GetVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.GetVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.GetVolReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) ExportVol(
	ctx context.Context,
	req *pbcp.ExportVolRequest,
) (*pbcp.ExportVolReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.VolName)
	nameToId := &pbcp.NameToId{}
	spConf := &pbcp.StoragePoolConf{}

	apply := func(stm concurrency.STM) error {
		nameToIdVal := []byte(stm.Get(nameToIdKey))
		if len(nameToIdVal) == 0 {
			pch.Logger.Error("No nameToID: %s", nameToIdKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				nameToIdKey,
			}
		}
		if err := proto.Unmarshal(nameToIdVal, nameToId); err != nil {
			pch.Logger.Error(
				"nameToId unmarshal err: %s %v",
				nameToIdKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		spId := nameToId.ResId

		spConfKey := exApi.kf.SpConfEntityKey(spId)
		spConfVal := []byte(stm.Get(spConfKey))
		if err := proto.Unmarshal(spConfVal, spConf); err != nil {
			pch.Logger.Error(
				"spConf unmarshal err: %s %v",
				spConfKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		ssConf := spConf.SsConfList[0]
		for _, hostConf := range ssConf.HostConfList {
			if hostConf.HostNqn == req.HostNqn {
				pch.Logger.Error(
					"duplicate host nqn: %v",
					hostConf,
				)
				return &stmwrapper.StmError{
					constants.ReplyCodeDupRes,
					hostConf.HostNqn,
				}
			}
		}
		hostId := fmt.Sprintf("%016", spConf.GeneralConf.SpCounter)
		spConf.GeneralConf.SpCounter++
		hostConf := &pbcp.HostConf{
			HostId:  hostId,
			HostNqn: req.HostNqn,
		}
		ssConf.HostConfList = append(ssConf.HostConfList, hostConf)
		spConfValNew, err := proto.Marshal(spConf)
		if err != nil {
			pch.Logger.Error("Marshal spConf err: %v %v", spConf, err)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spConfStrNew := string(spConfValNew)
		stm.Put(spConfKey, spConfStrNew)

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.ExportVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.ExportVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.ExportVolReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) UnexportVol(
	ctx context.Context,
	req *pbcp.UnexportVolRequest,
) (*pbcp.UnexportVolReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.VolName)
	nameToId := &pbcp.NameToId{}
	spConf := &pbcp.StoragePoolConf{}

	apply := func(stm concurrency.STM) error {
		nameToIdVal := []byte(stm.Get(nameToIdKey))
		if len(nameToIdVal) == 0 {
			pch.Logger.Error("No nameToID: %s", nameToIdKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				nameToIdKey,
			}
		}
		if err := proto.Unmarshal(nameToIdVal, nameToId); err != nil {
			pch.Logger.Error(
				"nameToId unmarshal err: %s %v",
				nameToIdKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		spId := nameToId.ResId

		spConfKey := exApi.kf.SpConfEntityKey(spId)
		spConfVal := []byte(stm.Get(spConfKey))
		if err := proto.Unmarshal(spConfVal, spConf); err != nil {
			pch.Logger.Error(
				"spConf unmarshal err: %s %v",
				spConfKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}

		ssConf := spConf.SsConfList[0]
		targetIdx := -1
		for i, hostConf := range ssConf.HostConfList {
			if hostConf.HostNqn == req.HostNqn {
				targetIdx = i
				break
			}
		}

		if targetIdx < 0 {
			pch.Logger.Error("Can not find host nqn: %s", req.HostNqn)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				req.HostNqn,
			}
		}

		lastIdx := len(ssConf.HostConfList) - 1
		ssConf.HostConfList[targetIdx] = ssConf.HostConfList[lastIdx]
		ssConf.HostConfList = ssConf.HostConfList[:lastIdx]

		spConfValNew, err := proto.Marshal(spConf)
		if err != nil {
			pch.Logger.Error("Marshal spConf err: %v %v", spConf, err)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		spConfStrNew := string(spConfValNew)
		stm.Put(spConfKey, spConfStrNew)

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.UnexportVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.UnexportVolReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.UnexportVolReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}
