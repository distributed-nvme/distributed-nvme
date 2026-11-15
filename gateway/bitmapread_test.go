package gateway

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The source bitmap read of architecture.md, Bitmap reads: its window rule
// and the park every namespace on the volume must read in the primary's
// report (gateway.md, Bitmap reads).

// bmReadEnv is a volEnv whose primary cntlr runs on a fake agent, so the
// read's agent calls are observable, with the volume "src", td 900, of
// blocks per slice of the fixture's one slice: its size over the pool's
// block size.
func bmReadEnv(t *testing.T, blocks uint64) (*volEnv, *fakeAgent) {
	return bmReadEnvSlices(t, blocks, 1)
}

// bmReadEnvSlices is bmReadEnv over a pool of slices slices: the volume's
// size is blocks per slice times that many.
func bmReadEnvSlices(
	t *testing.T,
	blocks uint64,
	slices uint64,
) (*volEnv, *fakeAgent) {
	t.Helper()
	env := newVolEnv(t)
	addr := fakeAddrPort(t, "cn")
	agent := startFakeAgent(t, addr, 1<<40)
	env.putCn(addr, 802, 2)
	mustPut(t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrA), &pb.Cntlr{
		AddrPort:   addr,
		NvmeTrConf: volTrConf(addr),
		Primary:    true,
	})
	mustPut(t, env.cli, model.ThinDeviceKey(env.cid, volSpId, "src"),
		&pb.ThinDevice{
			TdId: 900, DevId: 7, Size: slices * blocks * volPoolBlock,
			Created: true,
		})
	conf := env.spConf()
	conf.TdNameList = append(conf.GetTdNameList(), "src")
	// Only the count of the list reaches the read's arithmetic.
	for i := uint64(1); i < slices; i++ {
		conf.SliceIdList = append(conf.GetSliceIdList(), volSliceId+i)
	}
	env.putSpConf(conf)
	return env, agent
}

// bmRead reads slice 0 of "src" over the window [start, start+cnt).
func bmRead(env *volEnv, start, cnt uint64) error {
	_, err := env.srv.GetThinDeviceBitmap(env.ctx,
		&pb.GetThinDeviceBitmapRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			TdName:      "src",
			SliceIdx:    0,
			StartBlock:  start,
			BlockCnt:    cnt,
		})
	return err
}

// TestThinDeviceBitmapWindow pins the window rule: a read starts at a
// multiple of 8 blocks and not past the slice's part of the volume, its count
// fits in what is left, and its count is a multiple of 8 unless it is zero,
// "to the end", or ends where that part ends — here 515 blocks per slice of
// a two-slice pool, so a count of 3 from 512 is the one short count allowed.
// A count that would wrap past 2^64 with its start is refused, not summed. A
// refused window is INVALID_ARGUMENT and reaches no agent.
func TestThinDeviceBitmapWindow(t *testing.T) {
	const blocks = 515
	for _, tc := range []struct {
		name       string
		start, cnt uint64
		want       codes.Code
	}{
		{"the whole slice", 0, 0, codes.OK},
		{"a multiple of 8", 0, 64, codes.OK},
		{"the rest from a multiple of 8", 64, 0, codes.OK},
		{"a short count that ends at the end", 512, 3, codes.OK},
		{"a start that is no multiple of 8", 4, 8, codes.InvalidArgument},
		{"a short count before the end", 0, 12, codes.InvalidArgument},
		{"a short count one block short of the end", 512, 2,
			codes.InvalidArgument},
		{"a count past the end", 512, 8, codes.InvalidArgument},
		{"a start past the end", 520, 0, codes.InvalidArgument},
		{"a count that wraps with its start", 8, ^uint64(0) - 7,
			codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, agent := bmReadEnvSlices(t, blocks, 2)
			bmWantCode(t, bmRead(env, tc.start, tc.cnt), tc.want)
			calls := agent.callCount("GetThinDeviceBm")
			if tc.want == codes.OK && calls != 1 ||
				tc.want != codes.OK && calls != 0 {
				t.Fatalf("%d GetThinDeviceBm calls for a read answered %v",
					calls, tc.want)
			}
		})
	}
}

// bmWantCode is volWantCode with OK for a nil error.
func bmWantCode(t *testing.T, err error, want codes.Code) string {
	t.Helper()
	if want == codes.OK {
		if err != nil {
			t.Fatalf("want OK, got %v", err)
		}
		return ""
	}
	return volWantCode(t, err, want)
}

// parkedRow is the primary's row of a parked ns-dev (cnagent.md CN28).
func parkedRow() *pb.ResInfo {
	return &pb.ResInfo{
		Status:  pb.ResStatus_RES_STATUS_OK,
		Details: "parked",
		Reason:  pb.ResReason_RES_REASON_PARKED,
	}
}

// TestThinDeviceBitmapWaitsForThePark pins the park rule: the read is
// refused FAILED_PRECONDITION, with no bitmap read, unless every namespace of
// the pool on the volume is suspended in the stored state — its own flag, or
// a transfer with auto_suspend naming it — and reads parked in the primary's
// report, its row OK with RES_REASON_PARKED, the reason and not the text
// deciding. A volume no namespace exports is read with no report asked for.
// A stored state that does not suspend a namespace is refused before any
// report, however parked the report says it is. An agent that refuses the
// cntlr, or does not answer, proves nothing.
func TestThinDeviceBitmapWaitsForThePark(t *testing.T) {
	ns := func(nsId uint64, idx uint32, tdId uint64) *pb.Namespace {
		return &pb.Namespace{NsId: nsId, NsIdx: idx, TdId: tdId,
			Suspended: true}
	}
	serving := func(nsId uint64, idx uint32, tdId uint64) *pb.Namespace {
		return &pb.Namespace{NsId: nsId, NsIdx: idx, TdId: tdId}
	}
	const nqn = "nqn.2024-01.io.dnv:bm"
	for _, tc := range []struct {
		name   string
		nsList []*pb.Namespace
		// xfer names (nqn, idx 1) with auto_suspend.
		xfer bool
		rows map[uint64]*pb.ResInfo
		// unknown makes the agent answer that it holds no such cntlr; fail
		// makes the call fail.
		unknown, fail bool
		want          codes.Code
		// wantInfo is whether the report is asked for at all.
		wantInfo bool
	}{
		{"no namespace on the volume", nil, false, nil, false, false,
			codes.OK, false},
		{"a namespace of another volume only",
			[]*pb.Namespace{serving(41, 1, 901)}, false, nil, false, false,
			codes.OK, false},
		{"its one namespace parked", []*pb.Namespace{ns(41, 1, 900)}, false,
			map[uint64]*pb.ResInfo{41: parkedRow()}, false, false, codes.OK,
			true},
		{"a transfer's auto-suspended origin, parked",
			[]*pb.Namespace{serving(41, 1, 900)}, true,
			map[uint64]*pb.ResInfo{41: parkedRow()}, false, false, codes.OK,
			true},
		{"a stored serving namespace the report still shows parked",
			[]*pb.Namespace{serving(41, 1, 900)}, false,
			map[uint64]*pb.ResInfo{41: parkedRow()}, false, false,
			codes.FailedPrecondition, false},
		{"its namespace serving", []*pb.Namespace{ns(41, 1, 900)}, false,
			map[uint64]*pb.ResInfo{41: {Status: pb.ResStatus_RES_STATUS_OK}},
			false, false, codes.FailedPrecondition, true},
		{"its namespace with no row", []*pb.Namespace{ns(41, 1, 900)},
			false, nil, false, false, codes.FailedPrecondition, true},
		{"parked by the text alone", []*pb.Namespace{ns(41, 1, 900)}, false,
			map[uint64]*pb.ResInfo{41: {
				Status: pb.ResStatus_RES_STATUS_OK, Details: "parked"}},
			false, false, codes.FailedPrecondition, true},
		{"a parked reason on a row not OK", []*pb.Namespace{ns(41, 1, 900)},
			false, map[uint64]*pb.ResInfo{41: {
				Status: pb.ResStatus_RES_STATUS_ERROR,
				Reason: pb.ResReason_RES_REASON_PARKED}},
			false, false, codes.FailedPrecondition, true},
		{"one of two namespaces parked",
			[]*pb.Namespace{ns(41, 1, 900), ns(42, 2, 900)}, false,
			map[uint64]*pb.ResInfo{41: parkedRow()}, false, false,
			codes.FailedPrecondition, true},
		{"both namespaces parked",
			[]*pb.Namespace{ns(41, 1, 900), ns(42, 2, 900)}, false,
			map[uint64]*pb.ResInfo{41: parkedRow(), 42: parkedRow()}, false,
			false, codes.OK, true},
		{"an agent that holds no such cntlr",
			[]*pb.Namespace{ns(41, 1, 900)}, false,
			map[uint64]*pb.ResInfo{41: parkedRow()}, true, false,
			codes.FailedPrecondition, true},
		{"an agent that does not answer", []*pb.Namespace{ns(41, 1, 900)},
			false, map[uint64]*pb.ResInfo{41: parkedRow()}, false, true,
			codes.FailedPrecondition, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, agent := bmReadEnv(t, 512)
			if tc.nsList != nil {
				env.putSubsystem(nqn, 600, tc.nsList)
			}
			if tc.xfer {
				mustPut(t, env.cli, model.TransferKey(env.cid, volSpId, "x0"),
					&pb.Transfer{XferId: 700, OriNqn: nqn, OriNsIdx: 1,
						AutoSuspend: true})
				conf := env.spConf()
				conf.XferNameList = append(conf.GetXferNameList(), "x0")
				env.putSpConf(conf)
			}
			if tc.rows != nil {
				agent.setInfo(nil, nil, nil,
					&pb.CntlrInfo{NsIdToDmLinear: tc.rows})
			}
			if tc.unknown {
				// The rows stay: the code alone must refuse.
				agent.setCntlrReply(&pb.AgentReply{
					Code: common.ReplyCodeUnknownObject})
			}
			if tc.fail {
				agent.setFail(errors.New("controller node down"))
			}
			msg := bmWantCode(t, bmRead(env, 0, 0), tc.want)
			if tc.want == codes.FailedPrecondition &&
				!strings.Contains(msg, "parked") &&
				!strings.Contains(msg, "not suspended") {
				t.Errorf("message %q names no park", msg)
			}
			if got := agent.callCount("GetCntlrInfo") > 0; got != tc.wantInfo {
				t.Errorf("report asked for: %v, want %v", got, tc.wantInfo)
			}
			reads := agent.callCount("GetThinDeviceBm")
			if tc.want == codes.OK && reads != 1 ||
				tc.want != codes.OK && reads != 0 {
				t.Errorf("%d bitmap reads for a read answered %v", reads,
					tc.want)
			}
		})
	}
}

// TestThinDeviceBitmapRefusesWhatTheStateCannotProve: a volume a clone fills
// is refused FAILED_PRECONDITION naming the clone, however its namespaces
// stand, as the dm-clone writes it; namespaces on the volume in two
// subsystems are all found; a listed subsystem whose key is gone is ABORTED.
// None reaches the bitmap read.
func TestThinDeviceBitmapRefusesWhatTheStateCannotProve(t *testing.T) {
	t.Run("a clone's destination", func(t *testing.T) {
		env, agent := bmReadEnv(t, 512)
		mustPut(t, env.cli, model.CloneKey(env.cid, volSpId, "c0"),
			&pb.Clone{CloneId: 701, DstTdId: 900})
		conf := env.spConf()
		conf.CloneNameList = append(conf.GetCloneNameList(), "c0")
		env.putSpConf(conf)
		msg := bmWantCode(t, bmRead(env, 0, 0), codes.FailedPrecondition)
		if !strings.Contains(msg, `clone "c0"`) {
			t.Errorf("message %q does not name the clone", msg)
		}
		if n := agent.callCount("GetThinDeviceBm"); n != 0 {
			t.Errorf("%d bitmap reads", n)
		}
	})
	t.Run("namespaces in two subsystems", func(t *testing.T) {
		env, agent := bmReadEnv(t, 512)
		env.putSubsystem("nqn.2024-01.io.dnv:a", 600, []*pb.Namespace{
			{NsId: 41, NsIdx: 1, TdId: 900, Suspended: true}})
		env.putSubsystem("nqn.2024-01.io.dnv:b", 601, []*pb.Namespace{
			{NsId: 42, NsIdx: 1, TdId: 900, Suspended: true}})
		agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
			NsIdToDmLinear: map[uint64]*pb.ResInfo{41: parkedRow()}})
		msg := bmWantCode(t, bmRead(env, 0, 0), codes.FailedPrecondition)
		if !strings.Contains(msg, "nqn.2024-01.io.dnv:b") {
			t.Errorf("message %q does not name the second subsystem", msg)
		}
		if n := agent.callCount("GetThinDeviceBm"); n != 0 {
			t.Errorf("%d bitmap reads", n)
		}
	})
	t.Run("a listed subsystem whose key is gone", func(t *testing.T) {
		env, agent := bmReadEnv(t, 512)
		conf := env.spConf()
		conf.NqnList = append(conf.GetNqnList(), "nqn.2024-01.io.dnv:gone")
		env.putSpConf(conf)
		bmWantCode(t, bmRead(env, 0, 0), codes.Aborted)
		if n := agent.callCount("GetThinDeviceBm"); n != 0 {
			t.Errorf("%d bitmap reads", n)
		}
	})
}
