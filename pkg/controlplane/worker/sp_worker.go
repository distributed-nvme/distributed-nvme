package worker

import (
	"fmt"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/keyfmt"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

var (
	spFastRetryCodeMap = make(map[uint32]bool)
)

type spWorkerServer struct {
	pbcp.UnimplementedStoragePoolWorkerServer
	mu             sync.Mutex
	etcdCli        *clientv3.Client
	kf             *keyfmt.KeyFmt
	sm             *stmwrapper.StmWrapper
	initTrigger    chan struct{}
	idAndRevToConf map[string]map[int64]*pbcp.StoragePoolConf
}

func (spwkr *spWorkerServer) getName() string {
	return "sp"
}

func (spwkr *spWorkerServer) getEtcdCli() *clientv3.Client {
	return spwkr.etcdCli
}

func (spwkr *spWorkerServer) getMemberPrefix() string {
	return spwkr.kf.SpMemberPrefix()
}

func (spwkr *spWorkerServer) getResPrefix() string {
	return spwkr.kf.SpConfEntityPrefix()
}

func (spwkr *spWorkerServer) getInitTrigger() <-chan struct{} {
	return spwkr.initTrigger
}

func (spwkr *spWorkerServer) addResRev(
	spId string,
	resBody []byte,
	rev int64,
) ([]string, error) {
	spConf := &pbcp.StoragePoolConf{}
	if err := proto.Unmarshal(resBody, spConf); err != nil {
		return nil, err
	}
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if ok {
		if len(revToConf) > 1 {
			panic("More tahn 1 sp rev: " + spId)
		}
	} else {
		revToConf = make(map[int64]*pbcp.StoragePoolConf)
		spwkr.idAndRevToConf[spId] = revToConf
	}
	revToConf[rev] = spConf
	grpcTargetList := make([]string, 0)
	for _, legConf := range spConf.LegConfList {
		for _, grpConf := range legConf.GrpConfList {
			for _, ldConf := range grpConf.LdConfList {
				grpcTargetList = append(grpcTargetList, ldConf.DnGrpcTarget)
			}
		}
	}
	for _, cntlrConf := range spConf.CntlrConfList {
		grpcTargetList = append(grpcTargetList, cntlrConf.CnGrpcTarget)
	}
	return grpcTargetList, nil
}

func (spwkr *spWorkerServer) delResRev(
	spId string,
	rev int64,
) error {
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if !ok {
		panic("Unknown sp id: " + spId)
	}
	delete(revToConf, rev)
	if len(revToConf) == 0 {
		delete(spwkr.idAndRevToConf, spId)
	}
	return nil
}

type storagePoolAttr struct {
	legIdToConf      map[string]*pbcp.LegConf
	grpIdToConf      map[string]*pbcp.GrpConf
	cntlrIdToConf    map[string]*pbcp.CntlrConf
	ldIdToCnIdList   map[string][]string
	creatingSnapConf *pbnd.SnapConf
	deletingSnapConf *pbnd.SnapConf
	ssConfList       []*pbnd.SsConf
}

func generateSpAttr(spConf *pbcp.StoragePoolConf) *storagePoolAttr {
	legIdToConf := make(map[string]*pbcp.LegConf)
	grpIdToConf := make(map[string]*pbcp.GrpConf)
	ldIdToCnIdList := make(map[string][]string)

	cntlrIdToConf := make(map[string]*pbcp.CntlrConf)
	for _, cntlrConf := range spConf.CntlrConfList {
		cntlrIdToConf[cntlrConf.CntlrId] = cntlrConf
	}

	for _, legConf := range spConf.LegConfList {
		legIdToConf[legConf.LegId] = legConf
		for _, grpConf := range legConf.GrpConfList {
			grpIdToConf[grpConf.GrpId] = grpConf
			for _, ldConf := range grpConf.LdConfList {
				cnIdList := make([]string, 1)
				cntlrConf := cntlrIdToConf[legConf.AcCntlrId]
				cnIdList[0] = cntlrConf.CnId
				ldIdToCnIdList[ldConf.LdId] = cnIdList
			}
		}
	}

	var creatingSnapConf *pbnd.SnapConf
	if spConf.CreatingSnapConf != nil {
		creatingSnapConf = &pbnd.SnapConf{
			DevId: spConf.CreatingSnapConf.DevId,
			OriId: spConf.CreatingSnapConf.OriId,
		}
	}
	var deletingSnapConf *pbnd.SnapConf
	if spConf.DeletingSnapConf != nil {
		deletingSnapConf = &pbnd.SnapConf{
			DevId: spConf.CreatingSnapConf.DevId,
			OriId: spConf.CreatingSnapConf.OriId,
		}
	}

	ssConfList := make([]*pbnd.SsConf, len(spConf.SsConfList))
	for i, ssConf := range spConf.SsConfList {
		nsConfList := make([]*pbnd.NsConf, len(ssConf.NsConfList))
		for j, nsConf := range ssConf.NsConfList {
			nsConfList[j] = &pbnd.NsConf{
				NsId:  nsConf.NsId,
				NsNum: nsConf.NsNum,
				Size:  nsConf.Size,
				DevId: nsConf.DevId,
			}
		}
		hostConfList := make([]*pbnd.HostConf, len(ssConf.HostConfList))
		for j, hostConf := range ssConf.HostConfList {
			hostConfList[j] = &pbnd.HostConf{
				HostId:  hostConf.HostId,
				HostNqn: hostConf.HostNqn,
			}
		}
		ssConfList[i] = &pbnd.SsConf{
			SsId:         ssConf.SsId,
			NsConfList:   nsConfList,
			HostConfList: hostConfList,
		}
	}
	return &storagePoolAttr{
		legIdToConf:      legIdToConf,
		grpIdToConf:      grpIdToConf,
		cntlrIdToConf:    cntlrIdToConf,
		ldIdToCnIdList:   ldIdToCnIdList,
		creatingSnapConf: creatingSnapConf,
		deletingSnapConf: deletingSnapConf,
		ssConfList:       ssConfList,
	}
}

type spInfoBuilder struct {
	ssStatusInfoMap           map[string]*pbcp.StatusInfo
	oldSsStatusInfoMap        map[string]*pbcp.StatusInfo
	nsStatusInfoMap           map[string]*pbcp.StatusInfo
	oldNsStatusInfoMap        map[string]*pbcp.StatusInfo
	hostStatusInfoMap         map[string]*pbcp.StatusInfo
	oldHostStatusInfoMap      map[string]*pbcp.StatusInfo
	ldDnStatusInfoMap         map[string]*pbcp.StatusInfo
	oldLdDnStatusInfoMap      map[string]*pbcp.StatusInfo
	ldCnStatusInfoMap         map[string]*pbcp.StatusInfo
	oldLdCnStatusInfoMap      map[string]*pbcp.StatusInfo
	grpStatusInfoMap          map[string]*pbcp.StatusInfo
	oldGrpStatusInfoMap       map[string]*pbcp.StatusInfo
	grpMetaRedunInfoMap       map[string]*pbcp.RedundancyInfo
	oldGrpMetaRedunInfoMap    map[string]*pbcp.RedundancyInfo
	grpDataRedunInfoMap       map[string]*pbcp.RedundancyInfo
	oldGrpDataRedunInfoMap    map[string]*pbcp.RedundancyInfo
	remoteLegStatusInfoMap    map[string]*pbcp.StatusInfo
	oldRemoteLegStatusInfoMap map[string]*pbcp.StatusInfo
	legStatusInfoMap          map[string]*pbcp.StatusInfo
	oldLegStatusInfoMap       map[string]*pbcp.StatusInfo
	legThinPoolInfoMap        map[string]*pbcp.ThinPoolInfo
	oldLegThinPoolInfoMap     map[string]*pbcp.ThinPoolInfo
	cntlrStatusInfoMap        map[string]*pbcp.StatusInfo
	oldCntlrStatusInfoMap     map[string]*pbcp.StatusInfo
	allSucceeded              bool
}

func perCntlrKey(cntlrId, resId string) string {
	return fmt.Sprintf("%s-%s", cntlrId, resId)
}

func newSpInfoBuilder(
	spConf *pbcp.StoragePoolConf,
	oldSpInfo *pbcp.StoragePoolInfo,
	ldIdToInfo map[string]*pbnd.SpLdInfo,
	cntlrIdToInfo map[string]*pbnd.SpCntlrInfo,
	allSucceeded bool,
) *spInfoBuilder {
	ssStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldSsStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	nsStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldNsStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	hostStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldHostStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	ldDnStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldLdDnStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	ldCnStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldLdCnStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	grpStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldGrpStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	grpMetaRedunInfoMap := make(map[string]*pbcp.RedundancyInfo)
	oldGrpMetaRedunInfoMap := make(map[string]*pbcp.RedundancyInfo)
	grpDataRedunInfoMap := make(map[string]*pbcp.RedundancyInfo)
	oldGrpDataRedunInfoMap := make(map[string]*pbcp.RedundancyInfo)
	remoteLegStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldRemoteLegStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	legStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldLegStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	legThinPoolInfoMap := make(map[string]*pbcp.ThinPoolInfo)
	oldLegThinPoolInfoMap := make(map[string]*pbcp.ThinPoolInfo)
	cntlrStatusInfoMap := make(map[string]*pbcp.StatusInfo)
	oldCntlrStatusInfoMap := make(map[string]*pbcp.StatusInfo)

	for ldId, spLdInfo := range ldIdToInfo {
		if spLdInfo == nil {
			continue
		}
		ldDnStatusInfoMap[ldId] = &pbcp.StatusInfo{
			Code:      spLdInfo.StatusInfo.Code,
			Msg:       spLdInfo.StatusInfo.Msg,
			Timestamp: spLdInfo.StatusInfo.Timestamp,
		}
	}

	for cntlrId, spCntlrInfo := range cntlrIdToInfo {
		if spCntlrInfo == nil {
			continue
		}
		if spCntlrInfo.SsInfoList != nil {
			for _, ssInfo := range spCntlrInfo.SsInfoList {
				if ssInfo == nil {
					continue
				}
				for _, nsInfo := range ssInfo.NsInfoList {
					if nsInfo == nil {
						continue
					}
					key := perCntlrKey(cntlrId, nsInfo.NsId)
					nsStatusInfoMap[key] = &pbcp.StatusInfo{
						Code:      nsInfo.StatusInfo.Code,
						Msg:       nsInfo.StatusInfo.Msg,
						Timestamp: nsInfo.StatusInfo.Timestamp,
					}
				}
				for _, hostInfo := range ssInfo.HostInfoList {
					if hostInfo == nil {
						continue
					}
					key := perCntlrKey(cntlrId, hostInfo.HostId)
					hostStatusInfoMap[key] = &pbcp.StatusInfo{
						Code:      hostInfo.StatusInfo.Code,
						Msg:       hostInfo.StatusInfo.Msg,
						Timestamp: hostInfo.StatusInfo.Timestamp,
					}
				}
				key := perCntlrKey(cntlrId, ssInfo.SsId)
				ssStatusInfoMap[key] = &pbcp.StatusInfo{
					Code:      ssInfo.StatusInfo.Code,
					Msg:       ssInfo.StatusInfo.Msg,
					Timestamp: ssInfo.StatusInfo.Timestamp,
				}
			}
		}

		if spCntlrInfo.ActiveCntlrInfo != nil {
			for _, localLegInfo := range spCntlrInfo.ActiveCntlrInfo.LocalLegInfoList {
				if localLegInfo == nil {
					continue
				}
				for _, grpInfo := range localLegInfo.GrpInfoList {
					if grpInfo == nil {
						continue
					}
					for _, ldCnInfo := range grpInfo.LdCnInfoList {
						if ldCnInfo == nil {
							continue
						}
						ldCnStatusInfoMap[ldCnInfo.LdId] = &pbcp.StatusInfo{
							Code:      ldCnInfo.StatusInfo.Code,
							Msg:       ldCnInfo.StatusInfo.Msg,
							Timestamp: ldCnInfo.StatusInfo.Timestamp,
						}
					}
					grpStatusInfoMap[grpInfo.GrpId] = &pbcp.StatusInfo{
						Code:      grpInfo.StatusInfo.Code,
						Msg:       grpInfo.StatusInfo.Msg,
						Timestamp: grpInfo.StatusInfo.Timestamp,
					}
					if grpInfo.MetaRedunInfo != nil {
						grpMetaRedunInfoMap[grpInfo.GrpId] = &pbcp.RedundancyInfo{
							HealthChars: grpInfo.MetaRedunInfo.HealthChars,
							SyncCurr:    grpInfo.MetaRedunInfo.SyncCurr,
							SyncTotal:   grpInfo.MetaRedunInfo.SyncTotal,
							SyncAction:  grpInfo.MetaRedunInfo.SyncAction,
							MismatchCnt: grpInfo.MetaRedunInfo.MismatchCnt,
							DataOffset:  grpInfo.MetaRedunInfo.DataOffset,
							JournalChar: grpInfo.MetaRedunInfo.JournalChar,
						}
					}
					if grpInfo.DataRedunInfo != nil {
						grpDataRedunInfoMap[grpInfo.GrpId] = &pbcp.RedundancyInfo{
							HealthChars: grpInfo.DataRedunInfo.HealthChars,
							SyncCurr:    grpInfo.DataRedunInfo.SyncCurr,
							SyncTotal:   grpInfo.DataRedunInfo.SyncTotal,
							SyncAction:  grpInfo.DataRedunInfo.SyncAction,
							MismatchCnt: grpInfo.DataRedunInfo.MismatchCnt,
							DataOffset:  grpInfo.DataRedunInfo.DataOffset,
							JournalChar: grpInfo.DataRedunInfo.JournalChar,
						}
					}

				}
				legStatusInfoMap[localLegInfo.LegId] = &pbcp.StatusInfo{
					Code:      localLegInfo.StatusInfo.Code,
					Msg:       localLegInfo.StatusInfo.Msg,
					Timestamp: localLegInfo.StatusInfo.Timestamp,
				}
				if localLegInfo.ThinPoolInfo != nil {
					legThinPoolInfoMap[localLegInfo.LegId] = &pbcp.ThinPoolInfo{
						TransactionId:        localLegInfo.ThinPoolInfo.TransactionId,
						UsedMetaBlocks:       localLegInfo.ThinPoolInfo.UsedMetaBlocks,
						TotalMetaBlocks:      localLegInfo.ThinPoolInfo.TotalMetaBlocks,
						UsedDataBlocks:       localLegInfo.ThinPoolInfo.UsedDataBlocks,
						TotalDataBlocks:      localLegInfo.ThinPoolInfo.TotalDataBlocks,
						HeldMetadataRoot:     localLegInfo.ThinPoolInfo.HeldMetadataRoot,
						Mode:                 localLegInfo.ThinPoolInfo.Mode,
						DiscardPassdown:      localLegInfo.ThinPoolInfo.DiscardPassdown,
						ErrorOrQueue:         localLegInfo.ThinPoolInfo.ErrorOrQueue,
						NeedsCheck:           localLegInfo.ThinPoolInfo.NeedsCheck,
						MetadataLowWatermark: localLegInfo.ThinPoolInfo.MetadataLowWatermark,
					}
				}
			}

			for _, remoteLegInfo := range spCntlrInfo.ActiveCntlrInfo.RemoteLegInfoList {
				if remoteLegInfo == nil {
					continue
				}
				key := perCntlrKey(cntlrId, remoteLegInfo.LegId)
				remoteLegStatusInfoMap[key] = &pbcp.StatusInfo{
					Code:      remoteLegInfo.StatusInfo.Code,
					Msg:       remoteLegInfo.StatusInfo.Msg,
					Timestamp: remoteLegInfo.StatusInfo.Timestamp,
				}
			}
		}

		cntlrStatusInfoMap[cntlrId] = &pbcp.StatusInfo{
			Code:      spCntlrInfo.StatusInfo.Code,
			Msg:       spCntlrInfo.StatusInfo.Msg,
			Timestamp: spCntlrInfo.StatusInfo.Timestamp,
		}
	}

	for _, ssInfo := range oldSpInfo.SsInfoList {
		for _, ssPerCntlrInfo := range ssInfo.SsPerCntlrInfoList {
			key := perCntlrKey(ssPerCntlrInfo.CntlrId, ssInfo.SsId)
			oldSsStatusInfoMap[key] = ssPerCntlrInfo.StatusInfo
			for _, nsInfo := range ssPerCntlrInfo.NsInfoList {
				key := perCntlrKey(ssPerCntlrInfo.CntlrId, nsInfo.NsId)
				oldNsStatusInfoMap[key] = nsInfo.StatusInfo
			}
			for _, hostInfo := range ssPerCntlrInfo.HostInfoList {
				key := perCntlrKey(ssPerCntlrInfo.CntlrId, hostInfo.HostId)
				oldHostStatusInfoMap[key] = hostInfo.StatusInfo
			}
		}
	}

	for _, legInfo := range oldSpInfo.LegInfoList {
		for _, grpInfo := range legInfo.GrpInfoList {
			for _, ldInfo := range grpInfo.LdInfoList {
				oldLdDnStatusInfoMap[ldInfo.LdId] = ldInfo.DnStatusInfo
				oldLdCnStatusInfoMap[ldInfo.LdId] = ldInfo.CnStatusInfo
			}
			oldGrpStatusInfoMap[grpInfo.GrpId] = grpInfo.StatusInfo
			oldGrpMetaRedunInfoMap[grpInfo.GrpId] = grpInfo.MetaRedunInfo
			oldGrpDataRedunInfoMap[grpInfo.GrpId] = grpInfo.DataRedunInfo
		}
		oldLegStatusInfoMap[legInfo.LegId] = legInfo.StatusInfo
		oldLegThinPoolInfoMap[legInfo.LegId] = legInfo.ThinPoolInfo
		for _, remoteLegInfo := range legInfo.RemoteLegInfoList {
			key := perCntlrKey(remoteLegInfo.CntlrId, legInfo.LegId)
			oldRemoteLegStatusInfoMap[key] = remoteLegInfo.StatusInfo
		}
	}

	for _, cntlrInfo := range oldSpInfo.CntlrInfoList {
		oldCntlrStatusInfoMap[cntlrInfo.CntlrId] = cntlrInfo.StatusInfo
	}

	return &spInfoBuilder{
		ssStatusInfoMap:           ssStatusInfoMap,
		oldSsStatusInfoMap:        oldSsStatusInfoMap,
		nsStatusInfoMap:           nsStatusInfoMap,
		oldNsStatusInfoMap:        oldNsStatusInfoMap,
		ldDnStatusInfoMap:         ldDnStatusInfoMap,
		oldLdDnStatusInfoMap:      oldLdDnStatusInfoMap,
		ldCnStatusInfoMap:         ldCnStatusInfoMap,
		oldLdCnStatusInfoMap:      oldLdCnStatusInfoMap,
		grpStatusInfoMap:          grpStatusInfoMap,
		oldGrpStatusInfoMap:       oldGrpStatusInfoMap,
		grpMetaRedunInfoMap:       grpMetaRedunInfoMap,
		oldGrpMetaRedunInfoMap:    oldGrpMetaRedunInfoMap,
		grpDataRedunInfoMap:       grpDataRedunInfoMap,
		oldGrpDataRedunInfoMap:    oldGrpDataRedunInfoMap,
		remoteLegStatusInfoMap:    remoteLegStatusInfoMap,
		oldRemoteLegStatusInfoMap: oldRemoteLegStatusInfoMap,
		legStatusInfoMap:          legStatusInfoMap,
		oldLegStatusInfoMap:       oldLegStatusInfoMap,
		legThinPoolInfoMap:        legThinPoolInfoMap,
		oldLegThinPoolInfoMap:     oldLegThinPoolInfoMap,
		cntlrStatusInfoMap:        cntlrStatusInfoMap,
		oldCntlrStatusInfoMap:     oldCntlrStatusInfoMap,
		allSucceeded:              allSucceeded,
	}
}

func (builder *spInfoBuilder) getNsStatusInfo(
	cntlrId string,
	nsId string,
) *pbcp.StatusInfo {
	key := perCntlrKey(cntlrId, nsId)
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.nsStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldNsStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getHostStatusInfo(
	cntlrId string,
	hostId string,
) *pbcp.StatusInfo {
	key := perCntlrKey(cntlrId, hostId)
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.hostStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldHostStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getSsStatusInfo(
	cntlrId string,
	ssId string,
) *pbcp.StatusInfo {
	key := perCntlrKey(cntlrId, ssId)
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.ssStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldSsStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getLdDnStatusInfo(
	ldId string,
) *pbcp.StatusInfo {
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.ldDnStatusInfoMap[ldId]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldLdDnStatusInfoMap[ldId]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getLdCnStatusInfo(
	ldId string,
) *pbcp.StatusInfo {
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.ldCnStatusInfoMap[ldId]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldLdCnStatusInfoMap[ldId]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getGrpStatusInfo(
	grpId string,
) *pbcp.StatusInfo {
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.grpStatusInfoMap[grpId]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldGrpStatusInfoMap[grpId]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getGrpMetaRedunInfo(
	grpId string,
) *pbcp.RedundancyInfo {
	var redunInfo *pbcp.RedundancyInfo
	var ok bool
	redunInfo, ok = builder.grpMetaRedunInfoMap[grpId]
	if ok {
		return redunInfo
	}
	redunInfo, ok = builder.oldGrpMetaRedunInfoMap[grpId]
	if ok {
		return redunInfo
	}
	return nil
}

func (builder *spInfoBuilder) getGrpDataRedunInfo(
	grpId string,
) *pbcp.RedundancyInfo {
	var redunInfo *pbcp.RedundancyInfo
	var ok bool
	redunInfo, ok = builder.grpDataRedunInfoMap[grpId]
	if ok {
		return redunInfo
	}
	redunInfo, ok = builder.oldGrpDataRedunInfoMap[grpId]
	if ok {
		return redunInfo
	}
	return nil
}

func (builder *spInfoBuilder) getRemoteLegStatusInfo(
	cntlrId string,
	legId string,
) *pbcp.StatusInfo {
	key := perCntlrKey(cntlrId, legId)
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.remoteLegStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldRemoteLegStatusInfoMap[key]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getLegStatusInfo(
	legId string,
) *pbcp.StatusInfo {
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.legStatusInfoMap[legId]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldLegStatusInfoMap[legId]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getLegThinPoolInfo(
	legId string,
) *pbcp.ThinPoolInfo {
	var thinPoolInfo *pbcp.ThinPoolInfo
	var ok bool
	thinPoolInfo, ok = builder.legThinPoolInfoMap[legId]
	if ok {
		return thinPoolInfo
	}
	thinPoolInfo, ok = builder.oldLegThinPoolInfoMap[legId]
	if ok {
		return thinPoolInfo
	}
	return nil
}

func (builder *spInfoBuilder) getCntlrStatusInfo(
	cntlrId string,
) *pbcp.StatusInfo {
	var statusInfo *pbcp.StatusInfo
	var ok bool
	statusInfo, ok = builder.cntlrStatusInfoMap[cntlrId]
	if ok {
		return statusInfo
	}
	statusInfo, ok = builder.oldCntlrStatusInfoMap[cntlrId]
	if ok {
		return statusInfo
	}
	return nil
}

func (builder *spInfoBuilder) getSpStatusInfo() *pbcp.StatusInfo {
	if !builder.allSucceeded {
		return &pbcp.StatusInfo{
			Code:      constants.StatusCodeInternalErr,
			Msg:       "internal error",
			Timestamp: time.Now().UnixMilli(),
		}
	}
	return &pbcp.StatusInfo{
		Code:      constants.StatusCodeSucceed,
		Msg:       constants.StatusMsgSucceed,
		Timestamp: time.Now().UnixMilli(),
	}
}

func (spwkr *spWorkerServer) syncupSpLd(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	conn *grpc.ClientConn,
	ldConf *pbcp.LdConf,
	spAttr *storagePoolAttr,
) *pbnd.SpLdInfo {
	start := uint64(ldConf.ExtentSize) * uint64(ldConf.Start)
	length := uint64(ldConf.ExtentSize) * uint64(ldConf.Cnt)
	req := &pbnd.SyncupSpLdRequest{
		SpLdConf: &pbnd.SpLdConf{
			DnId:     ldConf.DnId,
			SpId:     spId,
			LdId:     ldConf.LdId,
			Revision: revision,
			Start:    start,
			Length:   length,
			CnIdList: spAttr.ldIdToCnIdList[ldConf.LdId],
			Inited:   ldConf.Inited,
		},
	}
	client := pbnd.NewDiskNodeAgentClient(conn)
	reply, err := client.SyncupSpLd(pch.Ctx, req)
	if err != nil {
		return &pbnd.SpLdInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeUnreachable,
				Msg:       err.Error(),
				Timestamp: time.Now().UnixMilli(),
			},
		}
	}
	return reply.SpLdInfo
}

func (spwkr *spWorkerServer) syncupSpCntlr(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	conn *grpc.ClientConn,
	spConf *pbcp.StoragePoolConf,
	cntlrConf *pbcp.CntlrConf,
	spAttr *storagePoolAttr,
) *pbnd.SpCntlrInfo {

	localLegConfList := make([]*pbnd.LocalLegConf, 0)
	remoteLegConfList := make([]*pbnd.RemoteLegConf, 0)
	for _, legConf := range spConf.LegConfList {
		if legConf.AcCntlrId == cntlrConf.CntlrId {
			grpConfList := make([]*pbnd.GrpConf, len(legConf.GrpConfList))
			for i, grpConf := range legConf.GrpConfList {
				ldCnConfList := make([]*pbnd.LdCnConf, len(grpConf.LdConfList))
				for j, ldConf := range grpConf.LdConfList {
					ldCnConfList[j] = &pbnd.LdCnConf{
						LdId: ldConf.LdId,
						DnId: ldConf.DnId,
						NvmeListener: &pbnd.NvmeListener{
							TrType:  ldConf.DnNvmeListener.TrType,
							AdrFam:  ldConf.DnNvmeListener.AdrFam,
							TrAddr:  ldConf.DnNvmeListener.TrAddr,
							TrSvcId: ldConf.DnNvmeListener.TrSvcId,
						},
						LdIdx:  ldConf.LdIdx,
						LdSize: uint64(ldConf.ExtentSize) * uint64(ldConf.Cnt),
					}
				}
				grpConfList[i] = &pbnd.GrpConf{
					GrpId:  grpConf.GrpId,
					GrpIdx: grpConf.GrpIdx,
					// FIXME: Get custom extent size
					MetaSize:     uint64(grpConf.MetaExtentCnt * (1 << constants.MetaExtentSizeShiftDefault)),
					DataSize:     uint64(grpConf.DataExtentCnt * (1 << constants.DataExtentSizeShiftDefault)),
					LdCnConfList: ldCnConfList,
					NoSync:       grpConf.NoSync,
					RebuildIdx:   grpConf.RebuildIdx,
					OmitIdxList:  grpConf.OmitIdxList,
				}
			}
			localLegConf := &pbnd.LocalLegConf{
				LegId:       legConf.LegId,
				LegIdx:      legConf.LegIdx,
				Reload:      legConf.Reload,
				GrpConfList: grpConfList,
			}
			localLegConfList = append(localLegConfList, localLegConf)
		} else {
			remoteCntlrConf, _ := spAttr.cntlrIdToConf[legConf.AcCntlrId]
			remoteLegConf := &pbnd.RemoteLegConf{
				LegId: legConf.LegId,
				CnId:  remoteCntlrConf.CnId,
				NvmeListener: &pbnd.NvmeListener{
					TrType:  remoteCntlrConf.NvmePortConf.NvmeListener.TrType,
					AdrFam:  remoteCntlrConf.NvmePortConf.NvmeListener.AdrFam,
					TrAddr:  remoteCntlrConf.NvmePortConf.NvmeListener.TrAddr,
					TrSvcId: remoteCntlrConf.NvmePortConf.NvmeListener.TrSvcId,
				},
			}
			remoteLegConfList = append(remoteLegConfList, remoteLegConf)
		}
	}
	// FIXME: implement moving task and importing task
	mtConfList := make([]*pbnd.MtConf, 0)
	itConfList := make([]*pbnd.ItConf, 0)
	req := &pbnd.SyncupSpCntlrRequest{
		SpCntlrConf: &pbnd.SpCntlrConf{
			CnId:     cntlrConf.CnId,
			SpId:     spId,
			CntlrId:  cntlrConf.CntlrId,
			Revision: revision,
			CntlrIdx: cntlrConf.CntlrIdx,
			NvmePortConf: &pbnd.NvmePortConf{
				PortNum: cntlrConf.NvmePortConf.PortNum,
				NvmeListener: &pbnd.NvmeListener{
					TrType:  cntlrConf.NvmePortConf.NvmeListener.TrType,
					AdrFam:  cntlrConf.NvmePortConf.NvmeListener.AdrFam,
					TrAddr:  cntlrConf.NvmePortConf.NvmeListener.TrAddr,
					TrSvcId: cntlrConf.NvmePortConf.NvmeListener.TrSvcId,
				},
				// FIXME: implement TrEq
				// TrEq: &pbnd.NvmeTReq{
				// 	SeqCh: cntlrConf.NvmePortConf.TrEq.SeqCh,
				// },
			},
			SsConfList: spAttr.ssConfList,
			ActiveCntlrConf: &pbnd.ActiveCntlrConf{
				StripeConf: &pbnd.StripeConf{
					ChunkSize: spConf.GeneralConf.StripeConf.ChunkSize,
				},
				ThinPoolConf: &pbnd.ThinPoolConf{
					DataBlockSize:  spConf.GeneralConf.ThinPoolConf.DataBlockSize,
					LowWaterMark:   spConf.GeneralConf.ThinPoolConf.LowWaterMark,
					ErrorIfNoSpace: spConf.GeneralConf.ThinPoolConf.ErrorIfNoSpace,
				},
				RedundancyConf: &pbnd.RedundancyConf{
					RedunType:       spConf.GeneralConf.RedundancyConf.RedunType,
					RegionSize:      spConf.GeneralConf.RedundancyConf.RegionSize,
					ChunkSize:       spConf.GeneralConf.RedundancyConf.ChunkSize,
					DaemonSleep:     spConf.GeneralConf.RedundancyConf.DaemonSleep,
					MinRecoveryRate: spConf.GeneralConf.RedundancyConf.MinRecoveryRate,
					MaxRecoveryRate: spConf.GeneralConf.RedundancyConf.MaxRecoveryRate,
					StripeCache:     spConf.GeneralConf.RedundancyConf.StripeCache,
					JournalMode:     spConf.GeneralConf.RedundancyConf.JournalMode,
				},
				CreatingSnapConf:  spAttr.creatingSnapConf,
				DeletingSnapConf:  spAttr.deletingSnapConf,
				LocalLegConfList:  localLegConfList,
				RemoteLegConfList: remoteLegConfList,
				MtConfList:        mtConfList,
				ItConfList:        itConfList,
			},
		},
	}
	client := pbnd.NewControllerNodeAgentClient(conn)
	reply, err := client.SyncupSpCntlr(pch.Ctx, req)
	if err != nil {
		return &pbnd.SpCntlrInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code:      constants.StatusCodeUnreachable,
				Msg:       err.Error(),
				Timestamp: time.Now().UnixMilli(),
			},
		}
	}
	return reply.SpCntlrInfo
}

func createSpInfo(
	revision int64,
	spConf *pbcp.StoragePoolConf,
	oldSpInfo *pbcp.StoragePoolInfo,
	ldIdToInfo map[string]*pbnd.SpLdInfo,
	cntlrIdToInfo map[string]*pbnd.SpCntlrInfo,
	allSucceeded bool,
	spAttr *storagePoolAttr,
) *pbcp.StoragePoolInfo {
	builder := newSpInfoBuilder(
		spConf,
		oldSpInfo,
		ldIdToInfo,
		cntlrIdToInfo,
		allSucceeded,
	)

	ssInfoList := make([]*pbcp.SsInfo, len(spConf.SsConfList))
	for i, ssConf := range spConf.SsConfList {
		// FIXME: use active cntlr only
		ssPerCntlrInfoList := make([]*pbcp.SsPerCntlrInfo, len(spConf.CntlrConfList))
		for j, cntlrConf := range spConf.CntlrConfList {
			nsInfoList := make([]*pbcp.NsInfo, len(ssConf.NsConfList))
			for k, nsConf := range ssConf.NsConfList {
				nsInfoList[k] = &pbcp.NsInfo{
					NsId: nsConf.NsId,
					StatusInfo: builder.getNsStatusInfo(
						cntlrConf.CntlrId,
						nsConf.NsId,
					),
				}
			}
			hostInfoList := make([]*pbcp.HostInfo, len(ssConf.HostConfList))
			for k, hostConf := range ssConf.HostConfList {
				hostInfoList[k] = &pbcp.HostInfo{
					HostId: hostConf.HostId,
					StatusInfo: builder.getHostStatusInfo(
						cntlrConf.CntlrId,
						hostConf.HostId,
					),
				}
			}
			ssPerCntlrInfoList[j] = &pbcp.SsPerCntlrInfo{
				CntlrId: cntlrConf.CntlrId,
				StatusInfo: builder.getSsStatusInfo(
					cntlrConf.CntlrId,
					ssConf.SsId,
				),
				NsInfoList:   nsInfoList,
				HostInfoList: hostInfoList,
			}
		}
		ssInfoList[i] = &pbcp.SsInfo{
			SsId:               ssConf.SsId,
			SsPerCntlrInfoList: ssPerCntlrInfoList,
		}
	}

	legInfoList := make([]*pbcp.LegInfo, len(spConf.LegConfList))
	for i, legConf := range spConf.LegConfList {
		grpInfoList := make([]*pbcp.GrpInfo, len(legConf.GrpConfList))
		for j, grpConf := range legConf.GrpConfList {
			ldInfoList := make([]*pbcp.LdInfo, len(grpConf.LdConfList))
			for k, ldConf := range grpConf.LdConfList {
				ldInfoList[k] = &pbcp.LdInfo{
					LdId:         ldConf.LdId,
					DnStatusInfo: builder.getLdDnStatusInfo(ldConf.LdId),
					CnStatusInfo: builder.getLdCnStatusInfo(ldConf.LdId),
				}
			}
			grpInfoList[j] = &pbcp.GrpInfo{
				GrpId:         grpConf.GrpId,
				StatusInfo:    builder.getGrpStatusInfo(grpConf.GrpId),
				MetaRedunInfo: builder.getGrpMetaRedunInfo(grpConf.GrpId),
				DataRedunInfo: builder.getGrpDataRedunInfo(grpConf.GrpId),
				LdInfoList:    ldInfoList,
			}
		}
		remoteLegInfoList := make([]*pbcp.RemoteLegInfo, 0)
		for _, cntlrConf := range spConf.CntlrConfList {
			if legConf.AcCntlrId == cntlrConf.CntlrId {
				continue
			}
			// FIXME: get fence info
			remoteLegInfo := &pbcp.RemoteLegInfo{
				CntlrId: cntlrConf.CntlrId,
				StatusInfo: builder.getRemoteLegStatusInfo(
					cntlrConf.CntlrId,
					legConf.LegId,
				),
			}
			remoteLegInfoList = append(remoteLegInfoList, remoteLegInfo)
		}
		legInfoList[i] = &pbcp.LegInfo{
			LegId:             legConf.LegId,
			StatusInfo:        builder.getLegStatusInfo(legConf.LegId),
			ThinPoolInfo:      builder.getLegThinPoolInfo(legConf.LegId),
			RemoteLegInfoList: remoteLegInfoList,
			GrpInfoList:       grpInfoList,
		}
	}

	cntlrInfoList := make([]*pbcp.CntlrInfo, len(spConf.CntlrConfList))
	for i, cntlrConf := range spConf.CntlrConfList {
		cntlrInfoList[i] = &pbcp.CntlrInfo{
			CntlrId:    cntlrConf.CntlrId,
			StatusInfo: builder.getCntlrStatusInfo(cntlrConf.CntlrId),
		}
	}

	// FIXME: set MtInfo and ItInfo
	return &pbcp.StoragePoolInfo{
		ConfRev:       revision,
		StatusInfo:    builder.getSpStatusInfo(),
		SsInfoList:    ssInfoList,
		LegInfoList:   legInfoList,
		CntlrInfoList: cntlrInfoList,
	}
}

func (spwkr *spWorkerServer) updateConfAndInfo(
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
	ldIdToInfo map[string]*pbnd.SpLdInfo,
	cntlrIdToInfo map[string]*pbnd.SpCntlrInfo,
	allSucceeded bool,
	updateConf bool,
	spAttr *storagePoolAttr,
) bool {
	spConfKey := spwkr.kf.SpConfEntityKey(spId)
	spInfoKey := spwkr.kf.SpInfoEntityKey(spId)
	oldSpConf := &pbcp.StoragePoolConf{}
	oldSpInfo := &pbcp.StoragePoolInfo{}
	apply := func(stm concurrency.STM) error {
		if updateConf {
			val := []byte(stm.Get(spConfKey))
			if len(val) <= 0 {
				pch.Logger.Warning("No spConf: %s", spConfKey)
				return nil
			}
			if err := proto.Unmarshal(val, oldSpConf); err != nil {
				pch.Logger.Fatal(
					"Get oldSpConf err: %s %v",
					spConfKey,
					err,
				)
			}
			rev := stm.Rev(spConfKey)
			if rev != revision {
				pch.Logger.Warning("Revision mismatch: %d %d", rev, revision)
				return nil
			}
			spConfVal, err := proto.Marshal(spConf)
			if err != nil {
				pch.Logger.Fatal("Marshal spConf err: %v %v", spConf, err)
			}
			spConfValStr := string(spConfVal)
			stm.Put(spConfKey, spConfValStr)
		}

		val := []byte(stm.Get(spInfoKey))
		if len(val) <= 0 {
			pch.Logger.Warning("NO oldSpInfo: %s", spInfoKey)
			return nil
		}
		if err := proto.Unmarshal(val, oldSpInfo); err != nil {
			pch.Logger.Fatal(
				"Get oldSpInfo err: %s %v",
				spInfoKey,
				err,
			)
		}
		if oldSpInfo.ConfRev > revision {
			pch.Logger.Warning(
				"Ignore old sp ConfRev: %d %d",
				oldSpInfo.ConfRev,
				revision,
			)
			return nil
		}
		spInfo := createSpInfo(
			revision,
			spConf,
			oldSpInfo,
			ldIdToInfo,
			cntlrIdToInfo,
			allSucceeded,
			spAttr,
		)
		spInfoVal, err := proto.Marshal(spInfo)
		if err != nil {
			pch.Logger.Fatal("Marshal spInfo err: %v %v", spInfo, err)
		}
		spInfoValStr := string(spInfoVal)
		stm.Put(spInfoKey, spInfoValStr)

		return nil
	}

	pch.Logger.Debug("spwkr.sm.RunStm")
	if err := spwkr.sm.RunStm(pch, apply); err != nil {
		pch.Logger.Error("Update sp err: %s %v", spId, err)
		return false
	}
	return true
}

func (spwkr *spWorkerServer) syncupAllLdAndCntlr(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
	spAttr *storagePoolAttr,
) bool {
	allSucceeded := true
	updateConf := false
	ldIdToInfo := make(map[string]*pbnd.SpLdInfo)
	for _, legConf := range spConf.LegConfList {
		for _, grpConf := range legConf.GrpConfList {
			for _, ldConf := range grpConf.LdConfList {
				conn, _ := targetToConn[ldConf.DnGrpcTarget]
				spLdInfo := spwkr.syncupSpLd(
					pch,
					spId,
					revision,
					conn,
					ldConf,
					spAttr,
				)
				if spLdInfo.StatusInfo.Code != constants.StatusCodeSucceed {
					pch.Logger.Debug("syncupSpLd failed")
					allSucceeded = false
				}
				if !ldConf.Inited &&
					spLdInfo.StatusInfo.Code == constants.StatusCodeSucceed {
					ldConf.Inited = true
					updateConf = true
				}
				ldIdToInfo[ldConf.LdId] = spLdInfo
			}
		}
	}

	cntlrIdToInfo := make(map[string]*pbnd.SpCntlrInfo)
	for _, cntlrConf := range spConf.CntlrConfList {
		conn, _ := targetToConn[cntlrConf.CnGrpcTarget]
		spCntlrInfo := spwkr.syncupSpCntlr(
			pch,
			spId,
			revision,
			conn,
			spConf,
			cntlrConf,
			spAttr,
		)
		if spCntlrInfo.StatusInfo.Code != constants.StatusCodeSucceed {
			pch.Logger.Debug("syncupSpCntlr failed")
			allSucceeded = false
		}
		if spCntlrInfo.ActiveCntlrInfo != nil {
			for _, localLegInfo := range spCntlrInfo.ActiveCntlrInfo.LocalLegInfoList {
				legConf := spAttr.legIdToConf[localLegInfo.LegId]
				if legConf.Reload &&
					localLegInfo.StatusInfo.Code == constants.StatusCodeSucceed {
					legConf.Reload = false
					updateConf = true
				}
				for _, grpInfo := range localLegInfo.GrpInfoList {
					grpConf := spAttr.grpIdToConf[grpInfo.GrpId]
					if grpConf.NoSync &&
						grpInfo.StatusInfo.Code == constants.StatusCodeSucceed {
						grpConf.NoSync = false
						updateConf = true
					}
				}
			}
		}
		cntlrIdToInfo[cntlrConf.CntlrId] = spCntlrInfo
	}

	if allSucceeded {
		if spConf.CreatingSnapConf != nil {
			spConf.CreatingSnapConf = nil
			updateConf = true
		}
		if spConf.DeletingSnapConf != nil {
			spConf.DeletingSnapConf = nil
			updateConf = true
		}
	}

	if succeed := spwkr.updateConfAndInfo(
		pch,
		spId,
		revision,
		spConf,
		ldIdToInfo,
		cntlrIdToInfo,
		allSucceeded,
		updateConf,
		spAttr,
	); !succeed {
		pch.Logger.Debug("updateConfAndInfo failed")
		allSucceeded = false
	}
	pch.Logger.Debug("allSucceeded: %v", allSucceeded)
	return allSucceeded
}

func (spwkr *spWorkerServer) syncupSp(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
	spConf *pbcp.StoragePoolConf,
	spAttr *storagePoolAttr,
) bool {
	interval := constants.SpRetryBase
	for {
		if allSucceeded := spwkr.syncupAllLdAndCntlr(
			targetToConn,
			pch,
			spId,
			revision,
			spConf,
			spAttr,
		); allSucceeded {
			return false
		}
		pch.Logger.Debug("Not allSucceeded")
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(interval):
			// FIXME: support fast retry
			interval = constants.SpRetryBase
		}
	}

}

func (spwkr *spWorkerServer) checkSp(
	targetToConn map[string]*grpc.ClientConn,
	pch *ctxhelper.PerCtxHelper,
	spId string,
	revision int64,
) bool {
	interval := constants.SpRetryBase
	for {
		select {
		case <-pch.Ctx.Done():
			return true
		case <-time.After(interval):
			interval = constants.SpRetryBase
		}
	}
}

func (spwkr *spWorkerServer) trackRes(
	spId string,
	pch *ctxhelper.PerCtxHelper,
	targetToConn map[string]*grpc.ClientConn,
) {
	revToConf, ok := spwkr.idAndRevToConf[spId]
	if !ok {
		pch.Logger.Fatal("Can not find spId: %s", spId)
	}
	if len(revToConf) != 1 {
		pch.Logger.Fatal("revToConf cnt error: %s %v", spId, revToConf)
	}
	var revision int64
	var spConf *pbcp.StoragePoolConf
	for key, value := range revToConf {
		revision = key
		spConf = value
		break
	}

	spAttr := generateSpAttr(spConf)
	for {
		// FIXME: implement sp error handling
		if exit := spwkr.syncupSp(
			targetToConn,
			pch,
			spId,
			revision,
			spConf,
			spAttr,
		); exit {
			return
		}
		if exit := spwkr.checkSp(
			targetToConn,
			pch,
			spId,
			revision,
		); exit {
			return
		}
	}
}

func newSpWorkerServer(
	etcdCli *clientv3.Client,
	prefix string,
) *spWorkerServer {
	return &spWorkerServer{
		etcdCli:        etcdCli,
		kf:             keyfmt.NewKeyFmt(prefix),
		sm:             stmwrapper.NewStmWrapper(etcdCli),
		initTrigger:    make(chan struct{}),
		idAndRevToConf: make(map[string]map[int64]*pbcp.StoragePoolConf),
	}
}
