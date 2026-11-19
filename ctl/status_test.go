// The status read's table (dnvctl.md CT4, `cluster` — `ctl/cluster.go`):
// `cluster get-status` prints a table by default and the CT4 document with
// --json, both through the one checked write of a result.
package ctl

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// statusReplyOf is a status page of three rows: a cluster row, a report row
// with a status and a reason whose text holds a newline and a tab, and a
// pool row with no status and no age.
func statusReplyOf(token string) *pb.GetClusterStatusReply {
	errStatus := pb.ResStatus_RES_STATUS_ERROR
	return &pb.GetClusterStatusReply{
		ClusterName: "c0",
		ReadTime:    timestamppb.New(time.Unix(1700000000, 0)),
		SpCnt:       2,
		PageToken:   token,
		RowList: []*pb.StatusRow{
			{
				Kind:    pb.StatusKind_STATUS_KIND_WORKER,
				Object:  "worker role sp",
				Text:    "no worker of the role is registered",
				ObjName: "sp",
			},
			{
				SpName: "pool0",
				Kind:   pb.StatusKind_STATUS_KIND_ROW,
				Object: "slice 2 thin pool",
				Status: &errStatus,
				Reason: pb.ResReason_RES_REASON_POOL_NEEDS_CHECK,
				Text:   "line one\nline\ttwo",
				Age:    durationpb.New(90*time.Second + 400*time.Millisecond),
			},
			{
				SpName: "pool1",
				Kind:   pb.StatusKind_STATUS_KIND_LOST_RECORD,
				Object: "dnv sp_rev 00 1 2",
				Text:   "the pool's revision key is absent",
			},
		},
	}
}

// TestStatusTableGolden pins the table cell by cell, each column starting
// at one place in every line: the header, one line per row with "-" for a
// cluster row's pool and for an age that is unset,
// the kind with the status and the reason where the row has them, a text's
// newline and tab shown as " | ", the count of pools read and the next
// page's flag.
func TestStatusTableGolden(t *testing.T) {
	client := &recordingClient{
		want: "GetClusterStatus", reply: statusReplyOf("tok1"),
	}
	res := runCLI(t, client, globalArgv("cluster", "get-status")...)
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	wantLines := [][]string{
		{"POOL", "OBJECT", "STATUS", "AGE", "TEXT"},
		{"-", "worker role sp", "worker", "-",
			"no worker of the role is registered"},
		{"pool0", "slice 2 thin pool", "row/error/pool_needs_check",
			"1m30s", "line one | line | two"},
		{"pool1", "dnv sp_rev 00 1 2", "lost-record", "-",
			"the pool's revision key is absent"},
	}
	if len(lines) != len(wantLines)+2 {
		t.Fatalf("stdout has %d lines, want %d:\n%s",
			len(lines), len(wantLines)+2, res.stdout)
	}
	// The columns line up: each starts at one place in every line.
	starts := columnStarts(lines[0], wantLines[0])
	for idx, cells := range wantLines {
		for col, cell := range cells {
			at := starts[col]
			if !strings.HasPrefix(lines[idx][at:], cell) {
				t.Errorf("line %d column %d: %q, want %q at byte %d",
					idx, col, lines[idx], cell, at)
			}
		}
	}
	if lines[len(lines)-2] != "2 pools read" ||
		lines[len(lines)-1] != "next page: --page-token tok1" {
		t.Errorf("the last two lines %q, want the pools read and the "+
			"next page", lines[len(lines)-2:])
	}
}

// columnStarts is the byte at which each header cell starts.
func columnStarts(header string, cells []string) []int {
	var starts []int
	from := 0
	for _, cell := range cells {
		at := strings.Index(header[from:], cell) + from
		starts = append(starts, at)
		from = at + len(cell)
	}
	return starts
}

// TestStatusTableNothingWrong pins a page with no row: one line that says
// how many pools were read, one pool in the singular, and no next-page line
// on the last page.
func TestStatusTableNothingWrong(t *testing.T) {
	client := &recordingClient{
		want:  "GetClusterStatus",
		reply: &pb.GetClusterStatusReply{SpCnt: 3},
	}
	res := runCLI(t, client, globalArgv("cluster", "get-status")...)
	if res.code != 0 ||
		res.stdout != "nothing is wrong in the 3 pools read\n" {
		t.Errorf("exit %d stdout %q, want the one line", res.code, res.stdout)
	}
	client = &recordingClient{
		want:  "GetClusterStatus",
		reply: &pb.GetClusterStatusReply{SpCnt: 1},
	}
	res = runCLI(t, client, globalArgv("cluster", "get-status")...)
	if res.stdout != "nothing is wrong in the 1 pool read\n" {
		t.Errorf("stdout %q, want the one line in the singular", res.stdout)
	}
}

// TestStatusJsonPrintsTheDocument pins --json: the reply as the one CT4
// document, every field shown.
func TestStatusJsonPrintsTheDocument(t *testing.T) {
	client := &recordingClient{
		want: "GetClusterStatus", reply: statusReplyOf(""),
	}
	res := runCLI(t, client,
		globalArgv("cluster", "get-status", "--json")...)
	if res.code != 0 || strings.Count(res.stdout, "\n") != 1 {
		t.Fatalf("exit %d stdout %q, want one line", res.code, res.stdout)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v", err)
	}
	rows, _ := doc["row_list"].([]any)
	if len(rows) != 3 || doc["sp_cnt"] != float64(2) {
		t.Errorf("document %v: want three rows and sp_cnt 2", doc)
	}
}

// statusTableFields are the fields the table shows, and statusLeftOut the
// ones it leaves out, each with its reason; every field of StatusRow and of
// GetClusterStatusReply is in exactly one of the two (item 343: a table
// shows each field or says why not).
var (
	statusTableFields = map[string]bool{
		"StatusRow.sp_name": true, "StatusRow.kind": true,
		"StatusRow.object": true, "StatusRow.status": true,
		"StatusRow.reason": true, "StatusRow.text": true,
		"StatusRow.age":                    true,
		"GetClusterStatusReply.row_list":   true,
		"GetClusterStatusReply.page_token": true,
		"GetClusterStatusReply.sp_cnt":     true,
	}
	statusLeftOut = map[string]string{
		"StatusRow.cntlr_id":                 "the object names it",
		"StatusRow.slice_id":                 "the object names it",
		"StatusRow.grp_id":                   "the object names it",
		"StatusRow.leg_id":                   "the object names it",
		"StatusRow.side_id":                  "the object names it",
		"StatusRow.obj_name":                 "the object names it",
		"StatusRow.addr_port":                "the object names it",
		"GetClusterStatusReply.cluster_name": "the operator named it",
		"GetClusterStatusReply.read_time":    "the ages are taken from it",
	}
)

// TestStatusTableAccountsForEveryField walks both messages' descriptors:
// a field added to either must join the table or the list of the fields
// it leaves out. Each field the table shows is set to a value of its own,
// which the table must print.
func TestStatusTableAccountsForEveryField(t *testing.T) {
	for _, desc := range []protoreflect.MessageDescriptor{
		(&pb.StatusRow{}).ProtoReflect().Descriptor(),
		(&pb.GetClusterStatusReply{}).ProtoReflect().Descriptor(),
	} {
		fields := desc.Fields()
		for idx := 0; idx < fields.Len(); idx++ {
			name := string(desc.Name()) + "." + string(fields.Get(idx).Name())
			_, left := statusLeftOut[name]
			if statusTableFields[name] == left {
				t.Errorf("%s is shown %v and left out %v: want one",
					name, statusTableFields[name], left)
			}
		}
	}
	client := &recordingClient{
		want: "GetClusterStatus", reply: statusReplyOf("tok-shown"),
	}
	res := runCLI(t, client, globalArgv("cluster", "get-status")...)
	for _, shown := range []string{
		"pool0", "row/", "slice 2 thin pool", "/error", "/pool_needs_check",
		"line one", "1m30s", "next page: --page-token tok-shown",
		"2 pools read",
	} {
		if !strings.Contains(res.stdout, shown) {
			t.Errorf("the table holds no %q:\n%s", shown, res.stdout)
		}
	}
}

// TestStatusTableIsTheCheckedWrite pins that the table reaches stdout
// through the one checked write (CT5): a write that fails is exit 3.
func TestStatusTableIsTheCheckedWrite(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	client := &recordingClient{
		want: "GetClusterStatus", reply: statusReplyOf(""),
	}
	res := runCLIOnStdout(t, func(_ context.Context, _ string) (
		pb.GatewayClient, func() error, error,
	) {
		return client, func() error { return nil }, nil
	}, full, globalArgv("cluster", "get-status")...)
	if res.code != 3 || res.stdout != "" {
		t.Errorf("exit %d stdout %q, want 3 and nothing", res.code, res.stdout)
	}
}
