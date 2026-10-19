package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The program's own tests: its command line and its two files. What the
// fake answers under each lever is the library's (integtest/agentfake);
// these tests play the levers through the behavior file and the stored
// state through the state file, over the program's own wiring.

// ---------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantMsg  string
		want     options
	}{
		{name: "no subcommand", args: nil, wantCode: 2, wantMsg: usageText},
		{name: "unknown subcommand", args: []string{"xn"}, wantCode: 2,
			wantMsg: usageText},
		{name: "flag before the subcommand",
			args: []string{"--dir", "d", "dn"}, wantCode: 2,
			wantMsg: usageText},
		{name: "unknown flag", args: []string{"dn", "--nope"}, wantCode: 2},
		{name: "help", args: []string{"cn", "-h"}, wantCode: 0},
		{name: "no address", args: []string{"dn", "--dir", "d"},
			wantCode: 1, wantMsg: "fakeagent: --grpc-address is required"},
		{name: "no dir", args: []string{"dn", "--grpc-address", "a:1"},
			wantCode: 1, wantMsg: "fakeagent: --dir is required"},
		{name: "dn with the default size",
			args: []string{"dn", "--grpc-address", "127.0.0.1:1",
				"--dir", "/w/dn0"},
			wantCode: -1,
			want: options{mode: "dn", kind: agentfake.Dn,
				addr: "127.0.0.1:1", dir: "/w/dn0",
				size: agentfake.DefaultNodeSize}},
		// The gateway suite starts its cn fakes at size zero, which the
		// fake replies as it is.
		{name: "cn at size zero",
			args: []string{"cn", "--grpc-address", "127.0.0.1:2",
				"--dir", "/w/cn0", "--size", "0"},
			wantCode: -1,
			want: options{mode: "cn", kind: agentfake.Cn,
				addr: "127.0.0.1:2", dir: "/w/cn0", size: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseArgs(tc.args, &output)
			if tc.wantCode < 0 {
				if err != nil {
					t.Fatalf("parseArgs(%q): %v", tc.args, err)
				}
				if got != tc.want {
					t.Fatalf("parseArgs(%q) = %+v, want %+v", tc.args, got,
						tc.want)
				}
				return
			}
			var exit *exitError
			if !errors.As(err, &exit) || exit.code != tc.wantCode ||
				exit.msg != tc.wantMsg {
				t.Fatalf("parseArgs(%q) = %v, want exit %d %q", tc.args, err,
					tc.wantCode, tc.wantMsg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Harness: the program's wiring, served over bufconn
// ---------------------------------------------------------------------------

// startProgram builds and serves one program of the kind over dir, with its
// behavior file poll running, and returns a client connection to it.
func startProgram(
	t *testing.T, kind agentfake.Kind, dir string, size uint64,
) (*program, *grpc.ClientConn) {
	t.Helper()
	mode := "dn"
	if kind == agentfake.Cn {
		mode = "cn"
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := newProgram(ctx, options{
		mode: mode, kind: kind, addr: "bufnet", dir: dir, size: size,
	})
	if err != nil {
		cancel()
		t.Fatalf("newProgram: %v", err)
	}
	lis := bufconn.Listen(1024 * 1024)
	go func() {
		if err := p.serve(ctx, lis); err != nil {
			t.Logf("fake agent server stopped: %v", err)
		}
	}()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		p.server.Stop()
		cancel()
	})
	return p, conn
}

// writeFile writes one file of the directory and stamps a strictly newer
// mtime, so the reload rules ("mtime or size changed" for behavior.json,
// "newer than the fake's own last write" for state.json) fire
// deterministically however coarse the filesystem's timestamps are.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

// returned waits for a call's outcome.
func returned(t *testing.T, what string, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// held fails when a call that must be held has returned.
func held(t *testing.T, what string, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned while held: %v", what, err)
	case <-time.After(2 * pollInterval):
	}
}

func syncupDn(
	t *testing.T, client pb.DiskNodeAgentClient, revision uint64,
	ptrList ...*pb.SidePointer,
) *pb.SyncupDnReply {
	t.Helper()
	reply, err := client.SyncupDn(context.Background(), &pb.SyncupDnRequest{
		ClusterId: 1, DnId: 1, Revision: revision,
		SidePointerList: ptrList, ExtentSize: 67108864,
	})
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	return reply
}

func dnInfo(t *testing.T, client pb.DiskNodeAgentClient) *pb.GetDnInfoReply {
	t.Helper()
	reply, err := client.GetDnInfo(context.Background(),
		&pb.GetDnInfoRequest{ClusterId: 1, DnId: 1})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	return reply
}

// dnRound runs one CheckDn round on a fresh stream.
func dnRound(t *testing.T, client pb.DiskNodeAgentClient) *pb.CheckDnReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	defer stream.CloseSend()
	if err := stream.Send(&pb.CheckDnRequest{
		ClusterId: 1, DnId: 1, Revision: 1, TraceId: "round",
	}); err != nil {
		t.Fatalf("CheckDn send: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("CheckDn recv: %v", err)
	}
	return reply
}

// ---------------------------------------------------------------------------
// behavior.json
// ---------------------------------------------------------------------------

// TestBehaviorFileReload pins the behavior file's reload: a changed file is
// seen by the next request, a malformed one is logged and IGNORED — the agent
// keeps answering with the last good levers instead of dying — and an absent
// one means `{}`.
func TestBehaviorFileReload(t *testing.T) {
	dir := t.TempDir()
	_, conn := startProgram(t, agentfake.Dn, dir, 4096)
	client := pb.NewDiskNodeAgentClient(conn)
	syncupDn(t, client, 1)

	writeFile(t, dir, behaviorFileName,
		`{"objects": {"dn": {"rows": {"disk_info": {"status": "ERROR",
		 "details": "io"}}}}}`)
	if got := dnInfo(t, client).GetDnInfo().GetDiskInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != "io" {
		t.Fatalf("disk_info = %v", got)
	}

	writeFile(t, dir, behaviorFileName, `{"objects": {"dn": {`)
	if got := dnInfo(t, client).GetDnInfo().GetDiskInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != "io" {
		t.Fatalf("disk_info after the malformed file = %v, want the "+
			"previous behaviour", got)
	}
	// A file the library refuses — when_primary on the dn — is malformed
	// the same way.
	writeFile(t, dir, behaviorFileName, `{"objects": {"dn": {"rows":
		{"disk_info": {"status": "OK", "when_primary": true}}}}}`)
	if got := dnInfo(t, client).GetDnInfo().GetDiskInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("disk_info after a refused file = %v, want the previous "+
			"behaviour", got)
	}

	if err := os.Remove(filepath.Join(dir, behaviorFileName)); err != nil {
		t.Fatalf("removing behavior.json: %v", err)
	}
	if got := dnInfo(t, client).GetDnInfo().GetDiskInfo().
		GetStatus(); got != pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("disk_info status after the removal = %v, want OK", got)
	}
}

// TestBehaviorFilePollReleasesAHold pins the poll of the behavior file: a
// Check round the hang lever holds, and a syncup hang_syncup holds, are
// answered once the script clears the lever, with no other request made to
// load the rewrite.
func TestBehaviorFilePollReleasesAHold(t *testing.T) {
	dir := t.TempDir()
	_, conn := startProgram(t, agentfake.Dn, dir, 4096)
	client := pb.NewDiskNodeAgentClient(conn)
	syncupDn(t, client, 1)
	writeFile(t, dir, behaviorFileName,
		`{"objects": {"dn": {"hang": true, "hang_syncup": true}}}`)

	round := make(chan error, 1)
	stream, err := client.CheckDn(context.Background())
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	defer stream.CloseSend()
	if err := stream.Send(&pb.CheckDnRequest{
		ClusterId: 1, DnId: 1, Revision: 1,
	}); err != nil {
		t.Fatalf("CheckDn send: %v", err)
	}
	go func() {
		_, err := stream.Recv()
		round <- err
	}()
	syncup := make(chan error, 1)
	go func() {
		_, err := client.SyncupDn(context.Background(),
			&pb.SyncupDnRequest{ClusterId: 1, DnId: 1, Revision: 2})
		syncup <- err
	}()
	held(t, "the round", round)
	held(t, "the syncup", syncup)

	writeFile(t, dir, behaviorFileName, `{}`)
	if err := returned(t, "the round", round); err != nil {
		t.Fatalf("the released round: %v", err)
	}
	if err := returned(t, "the syncup", syncup); err != nil {
		t.Fatalf("the released syncup: %v", err)
	}
}

// TestBehaviorFileNewLevers plays each lever the library adds to the
// program's through the behavior file, by the key the Go front's field
// carries, and sees it act on the wire.
func TestBehaviorFileNewLevers(t *testing.T) {
	ptr := &pb.SidePointer{SpId: 1, LegId: 3, SideId: 5}
	t.Run("obs_age_ms", func(t *testing.T) {
		dir := t.TempDir()
		_, conn := startProgram(t, agentfake.Dn, dir, 4096)
		client := pb.NewDiskNodeAgentClient(conn)
		saved := syncupDn(t, client, 1)
		writeFile(t, dir, behaviorFileName,
			`{"objects": {"dn": {"obs_age_ms": 3600000}}}`)
		stamp := dnRound(t, client).GetObsStamp()
		if !stamp.GetObsTime().AsTime().Equal(
			saved.GetObsStamp().GetObsTime().AsTime()) {
			t.Fatalf("a round under an hour's age: obs %v, want the "+
				"syncup's %v repeated", stamp.GetObsTime(),
				saved.GetObsStamp().GetObsTime())
		}
	})
	t.Run("obs_none", func(t *testing.T) {
		dir := t.TempDir()
		_, conn := startProgram(t, agentfake.Dn, dir, 4096)
		client := pb.NewDiskNodeAgentClient(conn)
		syncupDn(t, client, 1)
		writeFile(t, dir, behaviorFileName,
			`{"objects": {"dn": {"obs_none": true}}}`)
		round := dnRound(t, client)
		if stamp := round.GetObsStamp(); round.GetAgentReply().GetCode() !=
			0 || round.GetRevision() != 0 || round.GetDnInfo() != nil ||
			stamp.GetObsTime() != nil || stamp.GetAnswerTime() == nil {
			t.Fatalf("a round under obs_none = %v; want code 0, revision 0, "+
				"no info and an answer time alone", round)
		}
		if read := dnInfo(t, client); read.GetRevision() != 0 ||
			read.GetDnInfo() != nil || read.GetObsStamp().GetObsTime() != nil {
			t.Fatalf("an info read under obs_none = %v; want revision 0, no "+
				"info and no observation", read)
		}
	})
	t.Run("reason", func(t *testing.T) {
		dir := t.TempDir()
		_, conn := startProgram(t, agentfake.Dn, dir, 4096)
		client := pb.NewDiskNodeAgentClient(conn)
		syncupDn(t, client, 1)
		writeFile(t, dir, behaviorFileName, `{"objects": {"dn": {"rows":
		  {"meta_info": {"status": "ERROR", "reason": "THIN_ID_LOST"}}}}}`)
		if got := dnInfo(t, client).GetDnInfo().GetMetaInfo(); got.
			GetReason() != pb.ResReason_RES_REASON_THIN_ID_LOST {
			t.Fatalf("meta_info = %v, want the lever's reason", got)
		}
	})
	t.Run("syncup_revision", func(t *testing.T) {
		dir := t.TempDir()
		_, conn := startProgram(t, agentfake.Dn, dir, 4096)
		client := pb.NewDiskNodeAgentClient(conn)
		writeFile(t, dir, behaviorFileName,
			`{"objects": {"dn": {"syncup_revision": 9}}}`)
		if got := syncupDn(t, client, 2).GetRevision(); got != 9 {
			t.Fatalf("an accepted SyncupDn carries revision %d, want 9", got)
		}
		if got := dnRound(t, client).GetRevision(); got != 2 {
			t.Fatalf("a round reports revision %d, want the stored 2", got)
		}
	})
	t.Run("leftover_rounds", func(t *testing.T) {
		dir := t.TempDir()
		_, conn := startProgram(t, agentfake.Dn, dir, 4096)
		client := pb.NewDiskNodeAgentClient(conn)
		writeFile(t, dir, behaviorFileName,
			`{"objects": {"dn": {"leftover_rounds": 1}}}`)
		if code := syncupDn(t, client, 1).GetAgentReply().GetCode(); code !=
			common.ReplyCodeLeftover {
			t.Fatalf("a SyncupDn under a leftover: code %d", code)
		}
		for i, want := range []uint32{common.ReplyCodeLeftover, 0} {
			round := dnRound(t, client)
			if got := round.GetAgentReply().GetCode(); got != want ||
				round.GetRevision() != 1 {
				t.Fatalf("round %d: code %d revision %d, want %d and the "+
					"applied 1", i, got, round.GetRevision(), want)
			}
		}
	})
	t.Run("disk_counts", func(t *testing.T) {
		dir := t.TempDir()
		_, conn := startProgram(t, agentfake.Dn, dir, 4096)
		client := pb.NewDiskNodeAgentClient(conn)
		syncupDn(t, client, 1, ptr)
		writeFile(t, dir, behaviorFileName, `{"objects": {"dn":
		  {"disk_counts": {"total_ext_cnt": 64, "free_ext_cnt": 60,
		                   "free_meta_units": 3}}}}`)
		counts := dnInfo(t, client).GetDnInfo().GetDiskCounts()
		if counts.GetTotalExtCnt() != 64 || counts.GetFreeExtCnt() != 60 ||
			counts.GetFreeMetaUnits() != 3 || counts.GetSideRecCnt() != 1 ||
			!counts.GetSideRecsMatch() {
			t.Fatalf("disk counts = %v, want 64 60 3 and one matching "+
				"side record", counts)
		}
	})
}

// ---------------------------------------------------------------------------
// state.json and --size
// ---------------------------------------------------------------------------

// TestStateFileAcrossRestart pins the program's state file: a program
// started again over the same directory answers from the stored state — the
// stored revision, so the worker sends no syncup, and the side its node
// syncup listed — and a rewrite of the file while it runs is seen by the
// next request: a revision set by hand, and the store emptied as the suites'
// reset does.
func TestStateFileAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	first, conn := startProgram(t, agentfake.Dn, dir, 4096)
	client := pb.NewDiskNodeAgentClient(conn)
	ptr := &pb.SidePointer{SpId: 1, LegId: 3, SideId: 5}
	syncupDn(t, client, 7, ptr)
	first.server.Stop()

	_, conn = startProgram(t, agentfake.Dn, dir, 4096)
	client = pb.NewDiskNodeAgentClient(conn)
	if got := dnRound(t, client).GetRevision(); got != 7 {
		t.Fatalf("the restarted program's revision = %d, want the stored 7",
			got)
	}
	side, err := client.SyncupSide(context.Background(),
		&pb.SyncupSideRequest{ClusterId: 1, DnId: 1, SidePointer: ptr,
			Revision: 7, SideConf: &pb.SyncupSideRequest_SideConf{ExtCnt: 2}})
	if err != nil || side.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupSide after the restart = %v, %v; want the side "+
			"the stored SyncupDn listed known", side, err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, agentfake.StateFileName))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	var parsed map[string]map[string]map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("state.json is not readable JSON: %v", err)
	}
	dn := parsed["objects"][agentfake.DnKey]
	dn["revision"] = 9
	edited, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshaling the edit: %v", err)
	}
	writeFile(t, dir, agentfake.StateFileName, string(edited))
	if got := dnRound(t, client).GetRevision(); got != 9 {
		t.Fatalf("revision after the hand edit = %d, want 9", got)
	}

	writeFile(t, dir, agentfake.StateFileName, `{"objects":{}}`)
	if got := dnRound(t, client).GetRevision(); got != 0 {
		t.Fatalf("revision after the reset = %d, want 0", got)
	}
}

// TestSizeFlagAndFile pins the GetDnSize/GetCnSize reply: --size, zero
// included, unless the behavior file's "size" says otherwise; and the
// program serves only its subcommand's service.
func TestSizeFlagAndFile(t *testing.T) {
	dir := t.TempDir()
	_, conn := startProgram(t, agentfake.Cn, dir, 0)
	client := pb.NewControllerNodeAgentClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		common.TraceIdMetadataKey, "size")
	size := func() uint64 {
		t.Helper()
		reply, err := client.GetCnSize(ctx,
			&pb.GetCnSizeRequest{ClusterId: 1, CnId: 1})
		if err != nil {
			t.Fatalf("GetCnSize: %v", err)
		}
		return reply.GetSize()
	}
	if got := size(); got != 0 {
		t.Fatalf("GetCnSize = %d, want --size's 0", got)
	}
	writeFile(t, dir, behaviorFileName, `{"size": 8192}`)
	if got := size(); got != 8192 {
		t.Fatalf("GetCnSize = %d, want the behavior file's 8192", got)
	}
	if _, err := pb.NewDiskNodeAgentClient(conn).GetDnSize(ctx,
		&pb.GetDnSizeRequest{ClusterId: 1, DnId: 1}); status.Code(err) !=
		codes.Unimplemented {
		t.Fatalf("GetDnSize on a cn program: %v, want Unimplemented", err)
	}
}
