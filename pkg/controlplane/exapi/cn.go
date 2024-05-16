package exapi

import (
	"context"
	"fmt"
	"strconv"

	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

func (exApi *exApiServer) CreateCn(
	ctx context.Context,
	req *pbcp.CreateCnRequest,
) (*pbcp.CreateCnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	cnConf := &pbcp.ControllerNodeConf{
		TagList: req.TagList,
		GeneralConf: &pbcp.CnGeneralConf{
			GrpcTarget: req.GrpcTarget,
			Online:     req.Online,
			NvmePortConf: &pbcp.NvmePortConf{
				PortNum: constants.CnInternalPortNum,
				NvmeListener: &pbcp.NvmeListener{
					TrType:  req.TrType,
					AdrFam:  req.AdrFam,
					TrAddr:  req.TrAddr,
					TrSvcId: req.TrSvcId,
				},
				TrEq: &pbcp.NvmeTReq{
					SeqCh: 0,
				},
			},
			CnCapacity: &pbcp.CnCapacity{
				CntlrCnt: 0,
				TotalQos: 0,
				FreeQos:  0,
			},
			PortNextBit: initNextBit(constants.ExternalPortSize),
		},
		SpCntlrIdList: make([]*pbcp.SpCntlrId, 0),
	}
	cnConfVal, err := proto.Marshal(cnConf)
	if err != nil {
		pch.Logger.Error("Marshal cnConf err: %v %v", cnConf, err)
		return &pbcp.CreateCnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	cnConfStr := string(cnConfVal)

	cnInfo := &pbcp.ControllerNodeInfo{
		ConfRev: 0,
		StatusInfo: &pbcp.StatusInfo{
			Code:      constants.StatusCodeUninit,
			Msg:       "uninit",
			Timestamp: pch.Timestamp,
		},
	}
	cnInfoVal, err := proto.Marshal(cnInfo)
	if err != nil {
		pch.Logger.Error("Marshal cnInfo err: %v %v", cnInfo, err)
		return &pbcp.CreateCnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	cnInfoStr := string(cnInfoVal)

	cnGlobalKey := exApi.kf.CnGlobalEntityKey()
	cnGlobal := &pbcp.CnGlobal{}

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.GrpcTarget)

	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(cnGlobalKey))
		if len(val) == 0 {
			pch.Logger.Error("No cnGlobal: %s", cnGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				cnGlobalKey,
			}
		}
		if err := proto.Unmarshal(val, cnGlobal); err != nil {
			pch.Logger.Error(
				"cnGlobal unmarshal err: %s %v",
				cnGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		cnGlobal.GlobalCounter++
		counter := cnGlobal.GlobalCounter
		minCnt := constants.Uint32Max
		idx := -1
		for i, cnt := range cnGlobal.ShardBucket {
			if cnt < minCnt {
				minCnt = cnt
				idx = i
			}
		}
		if idx < 0 {
			panic("Do not find minimal cnt")
		}
		cnGlobal.ShardBucket[idx] = cnGlobal.ShardBucket[idx] + 1
		cnGlobalVal, err := proto.Marshal(cnGlobal)
		if err != nil {
			pch.Logger.Error(
				"cnGlobal marshal err: %v %v",
				cnGlobal,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		cnGlobalStr := string(cnGlobalVal)
		stm.Put(cnGlobalKey, cnGlobalStr)

		cnIdNum := (uint64(idx) << (constants.ShardMove)) | counter
		cnId := fmt.Sprintf("%016x", cnIdNum)

		cnConfKey := exApi.kf.CnConfEntityKey(cnId)
		if val := stm.Get(cnConfKey); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  cnConfKey,
			}
		}
		stm.Put(cnConfKey, cnConfStr)
		cnInfoKey := exApi.kf.CnInfoEntityKey(cnId)
		if val := []byte(stm.Get(cnInfoKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  cnInfoKey,
			}
		}
		stm.Put(cnInfoKey, cnInfoStr)

		if val := stm.Get(nameToIdKey); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  nameToIdKey,
			}
		}
		nameToId := &pbcp.NameToId{
			ResId: cnId,
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

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.CreateCnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.CreateCnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.CreateCnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) DeleteCn(
	ctx context.Context,
	req *pbcp.DeleteCnRequest,
) (*pbcp.DeleteCnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	cnId := req.CnId
	cnIdNum, err := strconv.ParseUint(cnId, 16, 64)
	if err != nil {
		return &pbcp.DeleteCnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInvalidArg,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	idx := cnIdNum >> constants.ShardMove

	cnConfKey := exApi.kf.CnConfEntityKey(cnId)
	cnConf := &pbcp.ControllerNodeConf{}

	cnInfoKey := exApi.kf.CnInfoEntityKey(cnId)

	cnGlobalKey := exApi.kf.CnGlobalEntityKey()
	cnGlobal := &pbcp.CnGlobal{}

	apply := func(stm concurrency.STM) error {
		cnConfVal := []byte(stm.Get(cnConfKey))
		if len(cnConfVal) == 0 {
			pch.Logger.Error("No cnConf: %s", cnConfKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				cnConfKey,
			}
		}
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
		if len(cnConf.SpCntlrIdList) > 0 {
			return &stmwrapper.StmError{
				constants.ReplyCodeResBusy,
				fmt.Sprintf("%v", cnConf.SpCntlrIdList),
			}
		}
		stm.Del(cnConfKey)

		if len(stm.Get(cnInfoKey)) > 0 {
			stm.Del(cnInfoKey)
		}

		nameToIdKey := exApi.kf.NameToIdEntityKey(cnConf.GeneralConf.GrpcTarget)
		if len(stm.Get(nameToIdKey)) > 0 {
			stm.Del(nameToIdKey)
		}

		cnGlobalOldVal := []byte(stm.Get(cnGlobalKey))
		if len(cnGlobalOldVal) == 0 {
			pch.Logger.Error("No cnGlobal: %s", cnGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				cnGlobalKey,
			}
		}
		if err := proto.Unmarshal(cnGlobalOldVal, cnGlobal); err != nil {
			pch.Logger.Error(
				"cnGlobal unmarshal err: %s %v",
				cnGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		if cnGlobal.ShardBucket[idx] == 0 {
			panic("ShardBucket underflow")
		}
		cnGlobal.ShardBucket[idx] = cnGlobal.ShardBucket[idx] - 1
		cnGlobalVal, err := proto.Marshal(cnGlobal)
		if err != nil {
			pch.Logger.Error(
				"cnGlobal marshal err: %s %v",
				cnGlobal,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		cnGlobalStr := string(cnGlobalVal)
		stm.Put(cnGlobalKey, cnGlobalStr)

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.DeleteCnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.DeleteCnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.DeleteCnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) GetCn(
	ctx context.Context,
	req *pbcp.GetCnRequest,
) (*pbcp.GetCnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	grpcTarget := ""
	cnId := ""

	switch x := req.Name.(type) {
	case *pbcp.GetCnRequest_GrpcTarget:
		grpcTarget = x.GrpcTarget
	case *pbcp.GetCnRequest_CnId:
		cnId = x.CnId
	}

	cnConf := &pbcp.ControllerNodeConf{}
	cnInfo := &pbcp.ControllerNodeInfo{}

	apply := func(stm concurrency.STM) error {
		if cnId == "" {
			nameToIdKey := exApi.kf.NameToIdEntityKey(grpcTarget)
			nameToIdVal := []byte(stm.Get(nameToIdKey))
			if len(nameToIdVal) == 0 {
				pch.Logger.Error("No nameToId: %s", nameToIdKey)
				return &stmwrapper.StmError{
					constants.ReplyCodeNotFound,
					nameToIdKey,
				}
			}
			nameToId := &pbcp.NameToId{}
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
			cnId = nameToId.ResId
		}

		cnConfKey := exApi.kf.CnConfEntityKey(cnId)
		cnConfVal := []byte(stm.Get(cnConfKey))
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

		cnInfoKey := exApi.kf.CnInfoEntityKey(cnId)
		cnInfoVal := []byte(stm.Get(cnInfoKey))
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

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.GetCnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.GetCnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.GetCnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
		CnConf: cnConf,
		CnInfo: cnInfo,
	}, nil
}
