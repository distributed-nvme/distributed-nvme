// Rendering goldens (dnvctl.md CT4).
//
// CT4 promises ONE canonical JSON document per invocation on stdout: sorted
// keys, stable spacing, proto field names, proto3 defaults visible, uint64 as
// JSON strings. Every clause of that is a property of the four-step pipeline
// in emit — protojson.Marshal with UseProtoNames+EmitUnpopulated, re-parse
// through encoding/json into `any`, json.Marshal that, fmt.Println — and each
// step can silently undo the one before it. Re-parsing into the generated
// struct instead of into a fresh `any`, for instance, re-encodes through the
// struct's own `omitempty` tags and quietly drops both EmitUnpopulated and
// the uint64-as-string rendering while still printing plausible JSON.
//
// So the assertions here are BYTE-EXACT strings captured off the process's
// real stdout, not field lookups in a re-parsed document: a test that parses
// the output before checking it cannot see the difference between the
// pipeline and a plausible impostor.
package ctl

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// renderCase is one golden: what the gateway replied, and the exact line
// dnvctl must print for it.
type renderCase struct {
	name  string
	rpc   string
	argv  []string
	reply proto.Message
	want  string
}

var renderCases = []renderCase{
	{
		// The sweep golden for step 03 (integtest/dnvctl_test.sh): an EMPTY
		// canned reply, which is what pins EmitUnpopulated and the key order at
		// once. Every field is at its proto3 default and every one of them is
		// still visible — the uint64 as a STRING, the five message fields as
		// null.
		name:  "cluster get, empty reply",
		rpc:   "GetCluster",
		argv:  []string{"cluster", "get"},
		reply: &pb.GetClusterReply{},
		want: `{"cluster_conf":null,"cluster_id":"0","cluster_name":"",` +
			`"cn_global":null,"dn_global":null,"sp_global":null}` + "\n",
	},
	{
		// The id step's reply: the uint64 cluster id renders as a string
		// beside the uint32 pool count, which stays a number.
		name: "cluster advance-ids, the reply",
		rpc:  "AdvanceClusterIds",
		argv: []string{"cluster", "advance-ids"},
		reply: &pb.AdvanceClusterIdsReply{
			ClusterId: 18446744073709551615, SpCnt: 3,
		},
		want: `{"cluster_id":"18446744073709551615","sp_cnt":3}` + "\n",
	},
	{
		// The sweep golden for step 20: a revision on the wire is a uint64,
		// and protojson renders it as a quoted string so that a value above
		// 2^53 survives a JavaScript or jq consumer intact. This is the
		// number an operator copies back into --rev.
		name: "sp get, revision as a string",
		rpc:  "GetStoragePool",
		argv: []string{"sp", "get"},
		reply: &pb.GetStoragePoolReply{
			SpName: "sp0",
			SpRev:  &pb.SpRev{SpName: "sp0", Revision: 12},
		},
		want: `{"cntlr_list":[],"slice_list":[],"sp_conf":null,` +
			`"sp_name":"sp0","sp_rev":{"revision":"12","sp_name":"sp0"}}` +
			"\n",
	},
	{
		// A repeated field that IS populated, next to one that is not: the
		// empty one renders as [] rather than being elided.
		name: "sp list, empty and populated repeated fields",
		rpc:  "ListStoragePools",
		argv: []string{"sp", "list"},
		reply: &pb.ListStoragePoolsReply{
			SpName: []string{"sp0", "sp1"},
		},
		want: `{"page_token":"","sp_name":["sp0","sp1"]}` + "\n",
	},
	{
		// The `created` poll (architecture.md, Thin devices: the client's wait
		// primitive) is the reason EmitUnpopulated is on: the gateway writes
		// `created` false and only the sp-worker flips it, so the field has to
		// be visible while it is still false. The map also pins two more
		// renderings at once — uint32 stays a NUMBER while uint64 becomes a
		// string.
		name: "td list, created visible while false",
		rpc:  "ListThinDevices",
		argv: []string{"td", "list"},
		reply: &pb.ListThinDevicesReply{
			NameToTd: map[string]*pb.ThinDevice{
				"t0": {TdId: 7, DevId: 3, Size: 67108864},
			},
		},
		want: `{"name_to_td":{"t0":{"created":false,"dev_id":3,` +
			`"ori_id":0,"size":"67108864","td_id":"7","torn":false}}}` + "\n",
	},
	{
		name: "td list, created true",
		rpc:  "ListThinDevices",
		argv: []string{"td", "list"},
		reply: &pb.ListThinDevicesReply{
			NameToTd: map[string]*pb.ThinDevice{
				"t1": {TdId: 8, Created: true},
			},
		},
		want: `{"name_to_td":{"t1":{"created":true,"dev_id":0,"ori_id":0,` +
			`"size":"0","td_id":"8","torn":false}}}` + "\n",
	},
	{
		// The torn mark of a snapshot (architecture.md, Thin devices) renders
		// as `created` does: false on every thin device above, and true where
		// the record holds it, which it does from the created flip on.
		name: "td list, a torn snapshot",
		rpc:  "ListThinDevices",
		argv: []string{"td", "list"},
		reply: &pb.ListThinDevicesReply{
			NameToTd: map[string]*pb.ThinDevice{
				"s1": {TdId: 9, DevId: 4, OriId: 3, Created: true, Torn: true},
			},
		},
		want: `{"name_to_td":{"s1":{"created":true,"dev_id":4,"ori_id":3,` +
			`"size":"0","td_id":"9","torn":true}}}` + "\n",
	},
	{
		// A disk node's own counts (dnagent.md DN18) ride in dn_info beside
		// its rows, under the key names of `dn get`'s record: the uint64
		// counts as strings, and side_recs_match a bool that is visible while
		// false. The rows the reply leaves out render as null.
		name: "dn inspect, the disk node's counts",
		rpc:  "InspectDiskNode",
		argv: []string{"dn", "inspect", "--addr", "dn0:9520"},
		reply: &pb.InspectDiskNodeReply{
			AppliedRevision: 7,
			DnInfo: &pb.DnInfo{
				DiskCounts: &pb.DnInfo_DiskCounts{
					TotalExtCnt:   256,
					FreeExtCnt:    231,
					SideRecCnt:    1,
					FreeMetaUnits: 64,
				},
			},
		},
		want: `{"applied_revision":"7","dn_info":{"disk_counts":` +
			`{"free_ext_cnt":"231","free_meta_units":"64",` +
			`"side_rec_cnt":"1","side_recs_match":false,` +
			`"total_ext_cnt":"256"},"disk_info":null,"meta_info":null,` +
			`"port_info":null},"obs_age":null,"obs_stamp":null}` + "\n",
	},
	{
		// Counts the agent could not take are absent, never zeros: the
		// field renders as null.
		name: "dn inspect, no counts",
		rpc:  "InspectDiskNode",
		argv: []string{"dn", "inspect", "--addr", "dn0:9520"},
		reply: &pb.InspectDiskNodeReply{
			AppliedRevision: 7,
			DnInfo:          &pb.DnInfo{},
		},
		want: `{"applied_revision":"7","dn_info":{"disk_counts":null,` +
			`"disk_info":null,"meta_info":null,"port_info":null},` +
			`"obs_age":null,"obs_stamp":null}` + "\n",
	},
	{
		// The agent's stamp and the observation's age (architecture.md,
		// Live-state reporting): the times as RFC 3339 strings in UTC, the
		// age as protojson's seconds string, and a converge start the agent
		// left unset as null.
		name: "cntlr inspect, the observation's stamp and age",
		rpc:  "InspectCntlr",
		argv: []string{"cntlr", "inspect", "--id", "3"},
		reply: &pb.InspectCntlrReply{
			AppliedRevision: 9,
			ObsStamp: &pb.ObsStamp{
				ObsTime: timestamppb.New(
					time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)),
				ObsTraceId: "trace-1",
				AnswerTime: timestamppb.New(
					time.Date(2026, 1, 2, 3, 4, 6, 500506000, time.UTC)),
			},
			ObsAge: durationpb.New(1500500 * time.Microsecond),
		},
		want: `{"applied_revision":"9","cntlr_info":null,` +
			`"obs_age":"1.500500s","obs_stamp":{` +
			`"answer_time":"2026-01-02T03:04:06.500506Z",` +
			`"converge_start":null,` +
			`"obs_time":"2026-01-02T03:04:05.000006Z",` +
			`"obs_trace_id":"trace-1"}}` + "\n",
	},
	{
		// A uint64 MAP KEY is quoted too, and the re-marshal sorts the map's
		// keys the same way it sorts a message's fields.
		name: "sp find-names, uint64 map keys",
		rpc:  "FindStoragePoolNames",
		argv: []string{"sp", "find-names", "--ids", "2,1"},
		reply: &pb.FindStoragePoolNamesReply{
			SpIdToName: map[uint64]string{2: "sp2", 1: "sp1"},
		},
		want: `{"sp_id_to_name":{"1":"sp1","2":"sp2"}}` + "\n",
	},
	{
		// A repair as stored: its stage an enum rendered by name, a uint64
		// map key and value quoted, and the empty new_leg_list an empty list
		// rather than an absent key.
		name: "repair get, the record",
		rpc:  "GetRepair",
		argv: []string{"repair", "get", "--name", "r0"},
		reply: &pb.GetRepairReply{
			RepName: "r0",
			Repair: &pb.Repair{
				RepId:                  9,
				GrpId:                  2,
				RepStage:               pb.RepStage_REP_STAGE_STARTED,
				BadLegIdList:           []uint64{3},
				TryToAbort:             true,
				SliceId:                1,
				NewLegIdToRetiredEpoch: map[uint64]uint64{4: 5},
			},
		},
		want: `{"rep_name":"r0","repair":{"bad_leg_id_list":["3"],` +
			`"clone_name":"","grp_id":"2",` +
			`"new_leg_id_to_retired_epoch":{"4":"5"},"new_leg_list":[],` +
			`"rep_id":"9","rep_stage":"REP_STAGE_STARTED","slice_id":"1",` +
			`"try_to_abort":true}}` + "\n",
	},
	{
		// The finish of a worker's repair at the bound of retired legs
		// (architecture.md, Repairs, FinishRepair): the reply names the
		// group's oldest retired leg, which the finish gave back, beside the
		// bad leg it retired.
		name: "repair finish, a retired leg given back",
		rpc:  "FinishRepair",
		argv: []string{"repair", "finish", "--name", "r0"},
		reply: &pb.FinishRepairReply{
			RepId: 9, RetiredLegId: 3, GivenBackLegId: 5,
		},
		want: `{"given_back_leg_id":"5","rep_id":"9",` +
			`"retired_leg_id":"3"}` + "\n",
	},
	{
		// A finish that gave nothing back still shows the field, as zero.
		name: "repair finish, nothing given back",
		rpc:  "FinishRepair",
		argv: []string{"repair", "finish", "--name", "r0"},
		reply: &pb.FinishRepairReply{
			RepId: 9, RetiredLegId: 3,
		},
		want: `{"given_back_leg_id":"0","rep_id":"9",` +
			`"retired_leg_id":"3"}` + "\n",
	},
	{
		// A slice group's forced-start mark as stored (dnvctl.md, `group` —
		// `ctl/group.go`): the named leg and the revision as strings on the
		// group that carries one, and null on the group that carries none.
		name: "sp get, a slice group's forced-start mark",
		rpc:  "GetStoragePool",
		argv: []string{"sp", "get"},
		reply: &pb.GetStoragePoolReply{
			SpName: "sp0",
			SliceList: []*pb.Slice{{
				MetaGrpList: []*pb.Group{{GrpId: 1}},
				DataGrpList: []*pb.Group{{
					GrpId:       2,
					ForcedStart: &pb.ForcedStart{LegId: 5, Revision: 12},
				}},
			}},
		},
		want: `{"cntlr_list":[],"slice_list":[{"data_grp_list":[` +
			`{"data_blocks":"0","ext_cnt":"0",` +
			`"forced_start":{"leg_id":"5","revision":"12"},"grp_id":"2",` +
			`"leg_list":[],"meta_blocks":"0","rep_name":"",` +
			`"retired_leg_id_list":[]}],"meta_grp_list":[` +
			`{"data_blocks":"0","ext_cnt":"0","forced_start":null,` +
			`"grp_id":"1","leg_list":[],"meta_blocks":"0","rep_name":"",` +
			`"retired_leg_id_list":[]}],"slice_idx":0}],"sp_conf":null,` +
			`"sp_name":"sp0","sp_rev":null}` + "\n",
	},
	{
		// The forced start's reply: the group and the revision its mark
		// carries, which a report must reach before the sp worker clears the
		// mark (dnv-worker.md AR13), both uint64 strings.
		name: "group force-start, the reply",
		rpc:  "ForceStartGroup",
		argv: []string{"group", "force-start", "--grp", "2", "--leg", "5"},
		reply: &pb.ForceStartGroupReply{
			GrpId: 2, Revision: 12,
		},
		want: `{"grp_id":"2","revision":"12"}` + "\n",
	},
	{
		// An empty map renders as {} rather than null, and the reply's only
		// field being empty does not make the document empty.
		name:  "ss list, empty map",
		rpc:   "ListSubsystems",
		argv:  []string{"ss", "list"},
		reply: &pb.ListSubsystemsReply{},
		want:  `{"nqn_to_subsystem":{}}` + "\n",
	},
	{
		// CT4's one deviation: protojson would render `bitmap` as base64, so
		// the two bitmap reads print a hex map instead. This is the sweep
		// golden for step 34.
		name: "td get-bm, the hex map",
		rpc:  "GetThinDeviceBitmap",
		argv: []string{"td", "get-bm", "--name", "t0"},
		reply: &pb.GetThinDeviceBitmapReply{
			Bitmap: []byte{0xa5},
		},
		want: `{"bitmap_hex":"a5","byte_cnt":1}` + "\n",
	},
	{
		name: "td get-leg-bm, the hex map",
		rpc:  "GetLegBitmap",
		argv: []string{"td", "get-leg-bm", "--leg", "9"},
		reply: &pb.GetLegBitmapReply{
			Bitmap: []byte{0xa5},
		},
		want: `{"bitmap_hex":"a5","byte_cnt":1}` + "\n",
	},
	{
		// Lowercase, no 0x, and byte_cnt travels alongside so a length check
		// needs no arithmetic on the hex string.
		name: "td get-bm, multi-byte bitmap",
		rpc:  "GetThinDeviceBitmap",
		argv: []string{"td", "get-bm", "--name", "t0"},
		reply: &pb.GetThinDeviceBitmapReply{
			Bitmap: []byte{0xff, 0x00, 0x10, 0xde},
		},
		want: `{"bitmap_hex":"ff0010de","byte_cnt":4}` + "\n",
	},
	{
		// An empty bitmap is a legal answer (a window past the end of the
		// device), and it must render as the empty string rather than as
		// null or as base64's "".
		name:  "td get-bm, empty bitmap",
		rpc:   "GetThinDeviceBitmap",
		argv:  []string{"td", "get-bm", "--name", "t0"},
		reply: &pb.GetThinDeviceBitmapReply{},
		want:  `{"bitmap_hex":"","byte_cnt":0}` + "\n",
	},
}

// TestRenderGoldens compares stdout byte for byte.
func TestRenderGoldens(t *testing.T) {
	for _, tc := range renderCases {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingClient{want: tc.rpc, reply: tc.reply}
			res := runCLI(t, client, globalArgv(tc.argv...)...)
			if res.code != 0 {
				t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
			}
			if res.stdout != tc.want {
				t.Errorf("stdout\n got: %q\nwant: %q", res.stdout, tc.want)
			}
			// CT5: a successful invocation says nothing on stderr. Together
			// with "exactly one line on stdout" that is the whole of CT7's
			// stream contract.
			if res.stderr != "" {
				t.Errorf("stderr = %q, want empty", res.stderr)
			}
			if lines := strings.Count(res.stdout, "\n"); lines != 1 {
				t.Errorf("stdout has %d lines, want exactly 1", lines)
			}
		})
	}
}

// TestEmitIsCanonical drives emit directly with a reply whose fields are
// deliberately out of alphabetical order in the schema, to pin the property
// the re-parse step exists for: protojson emits fields in FIELD-NUMBER order
// and varies its spacing, and the re-marshal through encoding/json is what
// turns that into one canonical, sorted, space-free document.
func TestEmitIsCanonical(t *testing.T) {
	// GetDiskNodeReply's fields are addr_port(1), dn_conf(2), dn_rev(3):
	// already sorted. UpdateCntlrEnabledReply's are cntlr_id(1), enabled(2),
	// also sorted. GetStoragePoolReply is the interesting one — sp_name(1),
	// sp_conf(2) — where field-number order and alphabetical order disagree,
	// so a missing re-marshal would print sp_name first.
	stdout, stderr := captureOutput(t, func() {
		if err := emit(&pb.GetStoragePoolReply{
			SpName: "sp0",
			SpConf: &pb.SpConf{SpId: 3},
		}); err != nil {
			t.Errorf("emit: %v", err)
		}
	})
	if stderr != "" {
		t.Errorf("emit wrote %q to stderr, want nothing", stderr)
	}
	spConf := strings.Index(stdout, `"sp_conf"`)
	spName := strings.Index(stdout, `"sp_name"`)
	if spConf < 0 || spName < 0 {
		t.Fatalf("emit printed %q, expected both sp_conf and sp_name", stdout)
	}
	if spConf > spName {
		t.Errorf("emit printed sp_name before sp_conf (%q): the reply was "+
			"not re-marshalled through encoding/json", stdout)
	}
	if strings.Contains(stdout, ": ") || strings.Contains(stdout, ", ") {
		t.Errorf("emit printed protojson's variable spacing: %q", stdout)
	}
}

// TestEmitHexBitmapResult covers hexBitmapResult on its own, including the
// nil slice the two bitmap commands hand it when a reply carries no bitmap
// at all.
func TestEmitHexBitmapResult(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{nil, `{"bitmap_hex":"","byte_cnt":0}`},
		{[]byte{}, `{"bitmap_hex":"","byte_cnt":0}`},
		{[]byte{0xa5}, `{"bitmap_hex":"a5","byte_cnt":1}`},
		{[]byte{0x00, 0x0f}, `{"bitmap_hex":"000f","byte_cnt":2}`},
	}
	for _, tc := range cases {
		stdout, _ := captureOutput(t, func() {
			if err := emit(hexBitmapResult(tc.in)); err != nil {
				t.Errorf("emit: %v", err)
			}
		})
		if stdout != tc.want+"\n" {
			t.Errorf("emit(hexBitmapResult(%v)) = %q, want %q",
				tc.in, stdout, tc.want+"\n")
		}
	}
}
