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
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

func validDnReq(req *pbcp.CreateDnRequest) error {
	if err := validStringLength(req.GrpcTarget, "GrpcTarget"); err != nil {
		return err
	}
	if err := validStringLength(req.DevPath, "DevPath"); err != nil {
		return err
	}
	if err := validStringLength(req.TrType, "TrType"); err != nil {
		return err
	}
	if err := validStringLength(req.AdrFam, "AdrFam"); err != nil {
		return err
	}
	if err := validStringLength(req.TrAddr, "TrAddr"); err != nil {
		return err
	}
	if err := validStringLength(req.TrSvcId, "TrSvcId"); err != nil {
		return err
	}
	for _, tag := range req.TagList {
		if err := validStringLength(tag.Key, "tag Key "+tag.Key); err != nil {
			return err
		}
		if err := validStringLength(tag.Value, "tag Value "+tag.Value); err != nil {
			return err
		}
	}
	return nil
}

func (exApi *exApiServer) CreateDn(
	ctx context.Context,
	req *pbcp.CreateDnRequest,
) (*pbcp.CreateDnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	if err := validDnReq(req); err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInvalidArg,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	conn, err := grpc.DialContext(
		ctx,
		req.GrpcTarget,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(exApi.agentTimeout),
		grpc.WithChainUnaryInterceptor(
			ctxhelper.UnaryClientPerCtxHelperInterceptor,
		),
	)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	defer conn.Close()

	c := pbnd.NewDiskNodeAgentClient(conn)
	getDevSizeRequest := &pbnd.GetDevSizeRequest{
		DevPath: req.DevPath,
	}
	getDevSizeReply, err := c.GetDevSize(ctx, getDevSizeRequest)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeAgentErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	if getDevSizeReply.StatusInfo.Code != constants.StatusCodeSucceed {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeAgentErr,
				ReplyMsg: fmt.Sprintf(
					"%d %s",
					getDevSizeReply.StatusInfo.Code,
					getDevSizeReply.StatusInfo.Msg,
				),
			},
		}, nil
	}

	cluster, err := exApi.getCluster(pch)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	metaSize := getDevSizeReply.Size >> cluster.ExtentRatioShift
	dataSize := getDevSizeReply.Size - metaSize

	metaBitmap, metaBucket, metaExtentCnt := extentInitCalc(
		metaSize,
		cluster.MetaExtentSizeShift,
		cluster.MetaExtentPerSetShift,
	)

	dataBitmap, dataBucket, dataExtentCnt := extentInitCalc(
		dataSize,
		cluster.DataExtentSizeShift,
		cluster.DataExtentPerSetShift,
	)

	metaBaseAddr := uint64(0)
	dataBaseAddr := metaBaseAddr + uint64(metaExtentCnt)*(1<<cluster.MetaExtentSizeShift)

	tag := &pbcp.Tag{
		Key:   constants.DefaultTagKey,
		Value: req.GrpcTarget,
	}
	tagList := append(req.TagList, tag)

	dnConf := &pbcp.DiskNodeConf{
		TagList: tagList,
		GeneralConf: &pbcp.DnGeneralConf{
			GrpcTarget: req.GrpcTarget,
			Online:     req.Online,
			DevPath:    req.DevPath,
			NvmePortConf: &pbcp.NvmePortConf{
				PortNum: constants.DnInternalPortNum,
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
			DnCapacity: &pbcp.DnCapacity{
				LdCnt:                0,
				TotalQos:             0,
				FreeQos:              0,
				MetaMaxExtentSetSize: metaBucket[0],
				MetaTotalExtentCnt:   metaExtentCnt,
				DataMaxExtentSetSize: dataBucket[0],
				DataTotalExtentCnt:   dataExtentCnt,
			},
			MetaExtentConf: &pbcp.ExtentConf{
				BaseAddr:        metaBaseAddr,
				ExtentSetBucket: metaBucket,
				Bitmap:          metaBitmap,
			},
			DataExtentConf: &pbcp.ExtentConf{
				BaseAddr:        dataBaseAddr,
				ExtentSetBucket: dataBucket,
				Bitmap:          dataBitmap,
			},
		},
		SpLdIdList: make([]*pbcp.SpLdId, 0),
	}
	dnConfVal, err := proto.Marshal(dnConf)
	if err != nil {
		pch.Logger.Error("Marshal dnConf err: %v %v", dnConf, err)
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnConfStr := string(dnConfVal)

	dnInfo := &pbcp.DiskNodeInfo{
		ConfRev: 0,
		StatusInfo: &pbcp.StatusInfo{
			Code:      constants.StatusCodeUninit,
			Msg:       "uninit",
			Timestamp: pch.Timestamp,
		},
	}
	dnInfoVal, err := proto.Marshal(dnInfo)
	if err != nil {
		pch.Logger.Error("Marshal dnInfo err: %v %v", dnInfo, err)
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnInfoStr := string(dnInfoVal)

	dnGlobalKey := exApi.kf.DnGlobalEntityKey()
	dnGlobal := &pbcp.DnGlobal{}

	nameToIdKey := exApi.kf.NameToIdEntityKey(req.GrpcTarget)

	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(dnGlobalKey))
		if len(val) == 0 {
			pch.Logger.Error("No dnGlobal: %s", dnGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				dnGlobalKey,
			}
		}
		if err := proto.Unmarshal(val, dnGlobal); err != nil {
			pch.Logger.Error(
				"dnGlobal unmarshal err: %s %v",
				dnGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		dnGlobal.GlobalCounter++
		counter := dnGlobal.GlobalCounter
		minCnt := constants.Uint32Max
		idx := -1
		for i, cnt := range dnGlobal.ShardBucket {
			if cnt < minCnt {
				minCnt = cnt
				idx = i
			}
		}
		if idx < 0 {
			panic("Do not find minimal cnt")
		}
		dnGlobal.ShardBucket[idx] = dnGlobal.ShardBucket[idx] + 1
		dnGlobalVal, err := proto.Marshal(dnGlobal)
		if err != nil {
			pch.Logger.Error(
				"dnGlobal marshal err: %v %v",
				dnGlobal,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		dnGlobalStr := string(dnGlobalVal)
		stm.Put(dnGlobalKey, dnGlobalStr)

		dnIdNum := (uint64(idx) << (constants.ShardMove)) | counter
		dnId := fmt.Sprintf("%016x", dnIdNum)

		dnConfKey := exApi.kf.DnConfEntityKey(dnId)
		if val := stm.Get(dnConfKey); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  dnConfKey,
			}
		}
		stm.Put(dnConfKey, dnConfStr)
		dnInfoKey := exApi.kf.DnInfoEntityKey(dnId)
		if val := []byte(stm.Get(dnInfoKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  dnInfoKey,
			}
		}
		stm.Put(dnInfoKey, dnInfoStr)

		if val := stm.Get(nameToIdKey); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  nameToIdKey,
			}
		}
		nameToId := &pbcp.NameToId{
			ResId: dnId,
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
			return &pbcp.CreateDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.CreateDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.CreateDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) DeleteDn(
	ctx context.Context,
	req *pbcp.DeleteDnRequest,
) (*pbcp.DeleteDnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	dnId := req.DnId
	dnIdNum, err := strconv.ParseUint(dnId, 16, 64)
	if err != nil {
		return &pbcp.DeleteDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInvalidArg,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	idx := dnIdNum >> constants.ShardMove

	dnConfKey := exApi.kf.DnConfEntityKey(dnId)
	dnConf := &pbcp.DiskNodeConf{}

	dnInfoKey := exApi.kf.DnInfoEntityKey(dnId)

	dnGlobalKey := exApi.kf.DnGlobalEntityKey()
	dnGlobal := &pbcp.DnGlobal{}

	apply := func(stm concurrency.STM) error {
		dnConfVal := []byte(stm.Get(dnConfKey))
		if len(dnConfVal) == 0 {
			pch.Logger.Error("No dnConf: %s", dnConfKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeNotFound,
				dnConfKey,
			}
		}
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
		if len(dnConf.SpLdIdList) > 0 {
			return &stmwrapper.StmError{
				constants.ReplyCodeResBusy,
				fmt.Sprintf("%v", dnConf.SpLdIdList),
			}
		}
		stm.Del(dnConfKey)

		if len(stm.Get(dnInfoKey)) > 0 {
			stm.Del(dnInfoKey)
		}

		nameToIdKey := exApi.kf.NameToIdEntityKey(dnConf.GeneralConf.GrpcTarget)
		if len(stm.Get(nameToIdKey)) > 0 {
			stm.Del(nameToIdKey)
		}

		dnGlobalOldVal := []byte(stm.Get(dnGlobalKey))
		if len(dnGlobalOldVal) == 0 {
			pch.Logger.Error("No dnGlobal: %s", dnGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				dnGlobalKey,
			}
		}
		if err := proto.Unmarshal(dnGlobalOldVal, dnGlobal); err != nil {
			pch.Logger.Error(
				"dnGlobal unmarshal err: %s %v",
				dnGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		if dnGlobal.ShardBucket[idx] == 0 {
			panic("ShardBucket underflow")
		}
		dnGlobal.ShardBucket[idx] = dnGlobal.ShardBucket[idx] - 1
		dnGlobalVal, err := proto.Marshal(dnGlobal)
		if err != nil {
			pch.Logger.Error(
				"dnGlobal marshal err: %s %v",
				dnGlobal,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		dnGlobalStr := string(dnGlobalVal)
		stm.Put(dnGlobalKey, dnGlobalStr)

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.DeleteDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.DeleteDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.DeleteDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) GetDn(
	ctx context.Context,
	req *pbcp.GetDnRequest,
) (*pbcp.GetDnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	grpcTarget := ""
	dnId := ""

	switch x := req.Name.(type) {
	case *pbcp.GetDnRequest_GrpcTarget:
		grpcTarget = x.GrpcTarget
	case *pbcp.GetDnRequest_DnId:
		dnId = x.DnId
	}

	dnConf := &pbcp.DiskNodeConf{}
	dnInfo := &pbcp.DiskNodeInfo{}

	apply := func(stm concurrency.STM) error {
		if dnId == "" {
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
			dnId = nameToId.ResId
		}

		dnConfKey := exApi.kf.DnConfEntityKey(dnId)
		dnConfVal := []byte(stm.Get(dnConfKey))
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

		dnInfoKey := exApi.kf.DnInfoEntityKey(dnId)
		dnInfoVal := []byte(stm.Get(dnInfoKey))
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

		return nil
	}

	if err := exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.GetDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.GetDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.GetDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
		DnConf: dnConf,
		DnInfo: dnInfo,
	}, nil
}
