package gateway

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestInspectStoragePool pins InspectStoragePool (architecture.md, Storage
// pools): the records' half — each slice's data space, the thin devices'
// summed size, the footprint — read in one Snapshot, the primary's report
// asked once after it, about the primary cntlr and with no counters, its
// thin pool rows turned into sizes and use, a row that does not parse
// leaving its slice unread, and nothing written; then the refusals: an
// agent that fails, a pool with no primary, a pool that does not exist, and
// a stored bdev_conf its validator refuses, before any agent call.
func TestInspectStoragePool(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	conf := env.spConf()
	conf.TdNameList = []string{"td-a", "td-b"}
	env.putSpConf(conf)
	for idx, name := range conf.GetTdNameList() {
		mustPut(t, env.cli, model.ThinDeviceKey(env.cid, volSpId, name),
			&pb.ThinDevice{TdId: uint64(900 + idx), DevId: uint32(idx + 1),
				Size: volTdSize})
	}
	agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{
			volSliceId: {Status: pb.ResStatus_RES_STATUS_OK,
				Details: "0 33538048 thin-pool 3 300/4096 100/8189 - rw " +
					"discard_passdown queue_if_no_space - 1024"},
		},
	})
	now := time.Now()
	stamp := &pb.ObsStamp{
		ObsTime:    timestamppb.New(now.Add(-3 * time.Second)),
		AnswerTime: timestamppb.New(now),
	}
	agent.setStamp(stamp)
	inspect := func() (*pb.InspectStoragePoolReply, error) {
		return env.srv.InspectStoragePool(env.ctx,
			&pb.InspectStoragePoolRequest{
				ClusterName: env.cluster, SpName: volSpName,
			})
	}

	before := env.dump()
	reply, err := inspect()
	if err != nil {
		t.Fatalf("InspectStoragePool: %v", err)
	}
	env.wantNoWrite("InspectStoragePool", before)
	reqs := agent.cntlrInfoReqList()
	if len(reqs) != 1 ||
		reqs[0].GetCntlrPointer().GetCntlrId() != volCntlrA ||
		reqs[0].GetCntlrPointer().GetSpId() != volSpId ||
		reqs[0].GetCnId() != fsCnId || reqs[0].GetReadIoStats() {
		t.Fatalf("GetCntlrInfo requests %v, want one about the primary "+
			"cntlr %d on its node, with no counters", reqs, volCntlrA)
	}
	if reply.GetPrimaryCntlrId() != volCntlrA ||
		reply.GetAppliedRevision() != fakeAgentRevision ||
		!proto.Equal(reply.GetObsStamp(), stamp) ||
		reply.GetObsAge().AsDuration() != 3*time.Second {
		t.Errorf("reply %v, want the primary, the agent's revision, its "+
			"stamp and an age of 3s", reply)
	}
	dataBytes := uint64(8189) * volPoolBlock
	want := &pb.PoolSpace{
		SliceList: []*pb.SliceSpace{{
			SliceId: volSliceId, SliceIdx: 0, DataGrpCnt: 1,
			DataBytes:     dataBytes,
			Read:          true,
			PoolDataBytes: dataBytes,
			UsedDataBytes: 100 * volPoolBlock,
			PoolMetaBytes: 4096 * common.ThinMetaBlockSize,
			UsedMetaBytes: 300 * common.ThinMetaBlockSize,
		}},
		TdCnt:           2,
		VirtualBytes:    2 * volTdSize,
		DataBytes:       dataBytes,
		UsedDataBytes:   100 * volPoolBlock,
		OvercommitPct:   2 * volTdSize * 100 / dataBytes,
		FootprintExtCnt: volMetaExtCnt + volDataExtCnt,
	}
	hnodeWantProto(t, reply.GetSpace(), want, "the pool's space")

	// A failed pool's row does not parse: the slice is unread, and the
	// records' half is still whole.
	agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{
			volSliceId: {Status: pb.ResStatus_RES_STATUS_ERROR,
				Details: "0 33538048 thin-pool Fail"},
		},
	})
	reply, err = inspect()
	if err != nil {
		t.Fatalf("InspectStoragePool over a failed pool: %v", err)
	}
	if got := reply.GetSpace(); got.GetSliceList()[0].GetRead() ||
		got.GetUsedDataBytes() != 0 || got.GetDataBytes() != dataBytes ||
		got.GetVirtualBytes() != 2*volTdSize {
		t.Errorf("space over a failed pool %v, want the slice unread and "+
			"the records' half whole", got)
	}

	// An agent that fails is ABORTED (AG3).
	agent.setFail(status.Error(codes.Unavailable, "down"))
	_, err = inspect()
	wantCode(t, err, codes.Aborted, "an agent that fails")
	agent.setFail(nil)

	// A stored bdev_conf its validator refuses is ABORTED before any agent
	// call (GW11): the data space is blocks times the stored block size.
	calls := agent.callCount("GetCntlrInfo")
	sparse := proto.Clone(conf).(*pb.SpConf)
	sparse.GetBdevConf().GetDmPoolConf().DataBlockSize = 0
	env.putSpConf(sparse)
	_, err = inspect()
	wantCode(t, err, codes.Aborted, "a sparse stored bdev_conf")
	if got := status.Convert(err).Message(); got != scMsgDataBlockSize {
		t.Errorf("message %q, want %q", got, scMsgDataBlockSize)
	}
	if agent.callCount("GetCntlrInfo") != calls {
		t.Errorf("a refused read called the agent")
	}
	env.putSpConf(conf)

	// No primary: FAILED_PRECONDITION, as the bitmap read.
	mustPut(t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrA),
		&pb.Cntlr{AddrPort: agent.AddrPort, NvmeTrConf: volTrConf(agent.AddrPort)})
	_, err = inspect()
	wantCode(t, err, codes.FailedPrecondition, "a pool with no primary")

	_, err = env.srv.InspectStoragePool(env.ctx, &pb.InspectStoragePoolRequest{
		ClusterName: env.cluster, SpName: "no-such-pool",
	})
	wantCode(t, err, codes.NotFound, "a pool that does not exist")
	_, err = env.srv.InspectStoragePool(env.ctx, &pb.InspectStoragePoolRequest{
		ClusterName: env.cluster,
	})
	wantCode(t, err, codes.InvalidArgument, "no pool name")
}
