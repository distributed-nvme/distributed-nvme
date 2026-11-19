package cnagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// v010CnStore is the store a primary's cn agent of v0.1.0 held
// (pb/samples_test.go says how it was made).
const v010CnStore = "../../pb/testdata/v0.1.0/store/cn"

// TestCnAgentStartsOverTheV010Store starts today's cn agent over the store
// a v0.1.0 cn agent left (architecture.md, Upgrade in lockstep): its startup
// pass loads the cn file, every cntlr file and every clone bitmap chunk,
// removes none of them, and the cntlr conf gate admits every cntlr it loaded.
// Each file is read by v0.1.0's prefix of its kind, not by the store's
// names for them today.
func TestCnAgentStartsOverTheV010Store(t *testing.T) {
	entries, err := os.ReadDir(v010CnStore)
	if err != nil {
		t.Fatal(err)
	}
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	var (
		cn     *pb.SyncupCnRequest
		cntlrs []*pb.SyncupCntlrRequest
		chunks []*pb.PushCloneBitmapRequest
	)
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(v010CnStore, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		node.protos[common.DefaultLocalStorPrefix+"/"+e.Name()] = raw
		var msg proto.Message
		switch {
		case strings.HasPrefix(e.Name(), "cn-"):
			cn = &pb.SyncupCnRequest{}
			msg = cn
		case strings.HasPrefix(e.Name(), "cntlr-"):
			req := &pb.SyncupCntlrRequest{}
			cntlrs = append(cntlrs, req)
			msg = req
		case strings.HasPrefix(e.Name(), "clone-bm-"):
			req := &pb.PushCloneBitmapRequest{}
			chunks = append(chunks, req)
			msg = req
		default:
			t.Fatalf("%s: no store kind of a controller node", e.Name())
		}
		if err := proto.Unmarshal(raw, msg); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
	}
	if cn == nil || len(cntlrs) == 0 || len(chunks) == 0 {
		t.Fatalf("the cn sample lacks a cn file, a cntlr or a chunk")
	}

	srv := newCnServer(node)
	reconcileForTest(t, srv)

	cid, cnId := cn.GetClusterId(), cn.GetCnId()
	if srv.getCn(cnKey(cid, cnId)) == nil {
		t.Fatalf("the cn file did not load")
	}
	for _, req := range cntlrs {
		ptr := req.GetCntlrPointer()
		st := srv.getCntlr(cntlrKey(cid, cnId, ptr.GetSpId(), ptr.GetCntlrId()))
		if st == nil {
			t.Errorf("cntlr %d did not load", ptr.GetCntlrId())
			continue
		}
		if err := validateCntlrConf(st.loadReq()); err != nil {
			t.Errorf("the cntlr conf gate refuses cntlr %d: %v", ptr.GetCntlrId(), err)
		}
		for _, chunk := range chunks {
			if chunk.GetCntlrPointer().GetCntlrId() != ptr.GetCntlrId() {
				continue
			}
			key := agent.CloneChunkKey{SliceIdx: chunk.GetSrcSliceIdx(), BmIdx: chunk.GetBmIdx()}
			if _, ok := st.chunkSet(chunk.GetCloneId()).Get(key); !ok {
				t.Errorf("cntlr %d lacks clone %d's chunk %v", ptr.GetCntlrId(),
					chunk.GetCloneId(), key)
			}
		}
	}
	for _, e := range entries {
		if _, ok := node.protos[common.DefaultLocalStorPrefix+"/"+e.Name()]; !ok {
			t.Errorf("the startup pass removed %s", e.Name())
		}
	}
}
