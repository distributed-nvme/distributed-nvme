// The limits of a dnvctl command line (dnvctl.md CT8, CT5, CT4).
//
// dnvctl checks one limit itself: a number must parse at the width of the
// request field it fills, so text that does not fit the field is a usage
// error (exit 2, no RPC issued) and every value that fits is sent as typed.
// Two walks pin it: one over the flags declared as pflag numbers, which
// pflag parses with the command line, and one over the flags declared as
// strings that dnvctl parses as numbers itself — the ids, the id and slot
// lists, the level and the global --rev. Every other limit is the gateway's
// (architecture.md, Common validation), the bounds on a count or a size and
// the lengths of a name or an NQN alike, and dnvctl forwards the value
// without looking at it (CT8): an accepted value comes back as the CT4
// document and exit 0, a refused one as the CT5 failure line and exit 1.
//
// The gateway here is the recording client with an answer function, judging
// the request it received against the same constants of common. A refusal
// therefore depends on the value dnvctl really sent, which is what makes
// "sent as typed" and "rendered as the gateway's refusal" one assertion
// rather than two. The refusal texts are the stand-in's own: what is pinned
// is that dnvctl prints the gateway's code and message verbatim, not what the
// gateway's message says.
package ctl

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// limitsTraceId is the trace id every invocation here runs under, so a
// refusal's failure line can be compared whole.
const limitsTraceId = "it-limits"

// runLeaf runs one leaf, named by its `<group> <verb>` path, under the
// suite's global prefix and limitsTraceId.
func runLeaf(
	t *testing.T, client *recordingClient, path string, argv ...string,
) cliResult {
	t.Helper()
	full := append([]string{"--trace-id", limitsTraceId},
		strings.Fields(path)...)
	return runCLI(t, client, globalArgv(append(full, argv...)...)...)
}

// wantAccepted is CT5's exit-0 row for an RPC the gateway accepted: exactly
// one RPC issued, exactly one JSON document on stdout (CT4) and nothing on
// stderr. It returns the document, decoded.
func wantAccepted(
	t *testing.T, res cliResult, client *recordingClient,
) map[string]any {
	t.Helper()
	if res.code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", res.code, res.stderr)
	}
	if client.calls != 1 {
		t.Errorf("%d RPCs were issued, want 1", client.calls)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
	if !strings.HasSuffix(res.stdout, "\n") ||
		strings.Count(res.stdout, "\n") != 1 {
		t.Errorf("stdout is not one line ending in a newline: %d bytes",
			len(res.stdout))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v", err)
	}
	return doc
}

// wantRefused is CT5's exit-1 row for an RPC the gateway refused: exactly
// one RPC issued, stdout empty, and stderr the failure line carrying the
// gateway's code and message verbatim.
func wantRefused(
	t *testing.T, res cliResult, client *recordingClient,
	code, message string,
) {
	t.Helper()
	if res.code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", res.code, res.stderr)
	}
	if client.calls != 1 {
		t.Errorf("%d RPCs were issued, want 1", client.calls)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
	want := fmt.Sprintf("dnvctl: %s: %s (trace_id %s)\n",
		code, message, limitsTraceId)
	if res.stderr != want {
		t.Errorf("stderr\n got: %q\nwant: %q", res.stderr, want)
	}
}

// ---------------------------------------------------------------------------
// List paging at the count limit
// ---------------------------------------------------------------------------

// listRow is one of the four paged list leaves: the RPC it drives, the
// reply's name list, how a name of its kind is spelled, the request a count
// and a token make, and the reply a page makes.
type listRow struct {
	path    string
	rpc     string
	field   string
	name    func(i int) string
	request func(count uint32, token string) proto.Message
	reply   func(names []string, token string) proto.Message
}

var listRows = []listRow{
	{"cluster list", "ListClusters", "cluster_name",
		func(i int) string { return fmt.Sprintf("c%05d", i) },
		func(count uint32, token string) proto.Message {
			// The one request with no cluster_name.
			return &pb.ListClustersRequest{Count: count, PageToken: token}
		},
		func(names []string, token string) proto.Message {
			return &pb.ListClustersReply{ClusterName: names, PageToken: token}
		}},
	{"dn list", "ListDiskNodes", "addr_port",
		func(i int) string {
			return fmt.Sprintf("10.1.%d.%d:9520", i/256, i%256)
		},
		func(count uint32, token string) proto.Message {
			return &pb.ListDiskNodesRequest{
				ClusterName: itCluster, Count: count, PageToken: token}
		},
		func(names []string, token string) proto.Message {
			return &pb.ListDiskNodesReply{AddrPort: names, PageToken: token}
		}},
	{"cn list", "ListControllerNodes", "addr_port",
		func(i int) string {
			return fmt.Sprintf("10.2.%d.%d:9521", i/256, i%256)
		},
		func(count uint32, token string) proto.Message {
			return &pb.ListControllerNodesRequest{
				ClusterName: itCluster, Count: count, PageToken: token}
		},
		func(names []string, token string) proto.Message {
			return &pb.ListControllerNodesReply{
				AddrPort: names, PageToken: token}
		}},
	{"sp list", "ListStoragePools", "sp_name",
		func(i int) string { return fmt.Sprintf("sp%05d", i) },
		func(count uint32, token string) proto.Message {
			return &pb.ListStoragePoolsRequest{
				ClusterName: itCluster, Count: count, PageToken: token}
		},
		func(names []string, token string) proto.Message {
			return &pb.ListStoragePoolsReply{SpName: names, PageToken: token}
		}},
}

// countRefusal is the stand-in's refusal of a count above MaxListCnt.
func countRefusal(count uint64) string {
	return fmt.Sprintf("count %d exceeds the maximum %d",
		count, common.MaxListCnt)
}

// pageKeyOf is the key the stand-in pages by for one name. The gateway's
// token is the standard base64 of the last key it returned, opaque to
// dnvctl (architecture.md, page_token). This key's bytes put a "+" and a
// "/" at the head of its base64 and leave "=" padding at its tail, the
// characters a token rewritten on its way back (re-padded, unpadded or
// URL-safe) loses first; a name whose length would leave no padding gets
// one more byte.
func pageKeyOf(name string) string {
	key := "\xfb\xef\xff" + name
	if len(key)%3 == 0 {
		key += "/"
	}
	return key
}

// pagingGateway answers a paged list over names the way the gateway pages
// (architecture.md, page_token): a count of 0 is DefaultListCnt, a count
// above MaxListCnt is refused, a page starts after the key its token
// encodes, and a full page carries an opaque standard-base64 token, as the
// gateway's is, while a short page carries none. A token that does not
// decode to the key of one of the names is refused, so a token dnvctl
// changed on its way back fails the next page rather than restarting the
// list.
func pagingGateway(
	row listRow, names []string,
) func(proto.Message) (proto.Message, error) {
	return func(req proto.Message) (proto.Message, error) {
		fields := req.ProtoReflect()
		desc := fields.Descriptor().Fields()
		count := fields.Get(desc.ByName("count")).Uint()
		token := fields.Get(desc.ByName("page_token")).String()
		if count > common.MaxListCnt {
			return nil, status.Error(codes.InvalidArgument,
				countRefusal(count))
		}
		limit := int(count)
		if count == 0 {
			limit = common.DefaultListCnt
		}
		start := 0
		if token != "" {
			last, err := base64.StdEncoding.DecodeString(token)
			at := slices.IndexFunc(names, func(name string) bool {
				return pageKeyOf(name) == string(last)
			})
			if err != nil || at < 0 {
				return nil, status.Errorf(codes.InvalidArgument,
					"malformed page_token %q", token)
			}
			start = at + 1
		}
		page := slices.Clone(names[start:min(start+limit, len(names))])
		next := ""
		if len(page) == limit {
			next = base64.StdEncoding.EncodeToString(
				[]byte(pageKeyOf(page[len(page)-1])))
		}
		return row.reply(page, next), nil
	}
}

// pageOf reads a list document back: its names, in order, and its token.
func pageOf(
	t *testing.T, doc map[string]any, field string,
) ([]string, string) {
	t.Helper()
	list, ok := doc[field].([]any)
	if !ok {
		t.Fatalf("the document's %q is %T, want a list", field, doc[field])
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("the document's %q holds a %T", field, item)
		}
		names = append(names, name)
	}
	token, ok := doc["page_token"].(string)
	if !ok {
		t.Fatalf("the document's page_token is %T, want a string "+
			"(CT4 renders an empty one too)", doc["page_token"])
	}
	return names, token
}

// TestListPagingAtTheCountLimit walks each paged list through a full page at
// MaxListCnt and on to the end, then at a count of 0 and one over the
// limit. dnvctl substitutes no page size and bounds none (CT8): the count
// goes out as typed, the full page is rendered whole (CT4), its token goes
// back verbatim on the next call, and the count one over is the gateway's
// refusal, rendered per CT5. The list holds a short second page, so the walk
// ends on a page that carries no token.
func TestListPagingAtTheCountLimit(t *testing.T) {
	const tail = 3
	for _, row := range listRows {
		t.Run(row.path, func(t *testing.T) {
			names := make([]string, common.MaxListCnt+tail)
			for i := range names {
				names[i] = row.name(i)
			}
			list := func(argv ...string) (*recordingClient, cliResult) {
				client := &recordingClient{
					want:   row.rpc,
					answer: pagingGateway(row, names),
				}
				return client, runLeaf(t, client, row.path, argv...)
			}
			limit := strconv.Itoa(common.MaxListCnt)

			// A full page at the limit, every name rendered, and a token.
			client, res := list("--count", limit)
			doc := wantAccepted(t, res, client)
			wantRequest(t, client.req, row.request(common.MaxListCnt, ""))
			first, token := pageOf(t, doc, row.field)
			if !slices.Equal(first, names[:common.MaxListCnt]) {
				t.Errorf("the full page rendered %d names, want the "+
					"first %d in order", len(first), common.MaxListCnt)
			}
			if token == "" {
				t.Fatalf("the full page rendered no page_token")
			}
			// The stand-in's keys make every full-page token hold the
			// characters a rewrite would touch; without them the next
			// request proves nothing about them.
			if !strings.Contains(token, "+") ||
				!strings.Contains(token, "/") ||
				!strings.HasSuffix(token, "=") {
				t.Fatalf("page_token %q lacks a \"+\", a \"/\" or "+
					"\"=\" padding", token)
			}

			// The token goes back as it came; the rest of the list ends
			// the walk.
			client, res = list("--count", limit, "--page-token", token)
			doc = wantAccepted(t, res, client)
			wantRequest(t, client.req,
				row.request(common.MaxListCnt, token))
			rest, end := pageOf(t, doc, row.field)
			if !slices.Equal(rest, names[common.MaxListCnt:]) {
				t.Errorf("the last page rendered %v, want %v",
					rest, names[common.MaxListCnt:])
			}
			if end != "" {
				t.Errorf("the last page rendered page_token %q, want "+
					"empty", end)
			}

			// A count of 0 is sent as 0, and the page is the gateway's
			// default size.
			client, res = list("--count", "0")
			doc = wantAccepted(t, res, client)
			wantRequest(t, client.req, row.request(0, ""))
			if got, _ := pageOf(t, doc, row.field); len(got) !=
				common.DefaultListCnt {
				t.Errorf("--count 0 rendered %d names, want the "+
					"gateway's default page of %d",
					len(got), common.DefaultListCnt)
			}

			// One over the limit is sent as typed and refused by the
			// gateway, not by dnvctl.
			client, res = list("--count",
				strconv.Itoa(common.MaxListCnt+1))
			wantRefused(t, res, client, "INVALID_ARGUMENT",
				countRefusal(common.MaxListCnt+1))
			if client.req == nil {
				t.Fatalf("the refused list recorded no request")
			}
			wantRequest(t, client.req, row.request(common.MaxListCnt+1, ""))
		})
	}
}

// rpcOfCmd maps each `<group> <verb>` path to the RPC it drives.
func rpcOfCmd() map[string]string {
	out := map[string]string{}
	for rpc, path := range rpcToCmd {
		out[path] = rpc
	}
	return out
}

// ---------------------------------------------------------------------------
// Every count or size flag at zero, at its bounds and one past them
// ---------------------------------------------------------------------------

// zeroRule is what a zero means for one flag.
type zeroRule int

const (
	// zeroDefault: the zero is sent, and the gateway takes its default
	// (architecture.md, Common validation: a proto3 zero is "unset").
	zeroDefault zeroRule = iota
	// zeroRefused: the zero is sent, and the gateway refuses it.
	zeroRefused
	// zeroNotSent: the zero sends no carrier message at all, the "not
	// given" convention of dnvctl.md, Conventions.
	zeroNotSent
)

// boundRow is one count or size flag: the leaf, the flag, the dotted path of
// the request field it fills, the gateway's bounds on a non-zero value (a max
// of 0 is no maximum, so a min of 1 with no maximum is no bound at all), what
// a zero means, and how a value is typed on the command line (nil is the
// decimal number).
type boundRow struct {
	path     string
	flag     string
	field    string
	min, max uint64
	zero     zeroRule
	arg      func(v uint64) string
}

// hostArg types n allowed hosts. From two on, the last repeats the first:
// dnvctl sends a list as typed, duplicates included, and the gateway counts
// them (architecture.md, Common validation).
func hostArg(n uint64) string {
	hosts := make([]string, n)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("nqn.2025-01.io.dnv:host%d", i)
	}
	if n >= 2 {
		hosts[n-1] = hosts[0]
	}
	return strings.Join(hosts, ",")
}

// bitmapArg types an n-byte bitmap as hex.
func bitmapArg(n uint64) string { return strings.Repeat("a5", int(n)) }

// slotArg types n distinct cntlid slots, 0 to n-1.
func slotArg(n uint64) string {
	slots := make([]string, n)
	for i := range slots {
		slots[i] = strconv.Itoa(i)
	}
	return strings.Join(slots, ",")
}

var boundRows = []boundRow{
	{"cluster create", "extent-size", "dn_bin_conf.extent_size",
		common.MinDnExtSize, common.MaxDnExtSize, zeroNotSent, nil},
	{"sp create", "cntlr-cnt", "cntlr_cnt",
		common.MinCntlrCntPerSp, common.MaxCntlrCntPerSp, zeroDefault, nil},
	{"sp create", "slice-cnt", "slice_cnt",
		1, common.MaxSliceCntPerSp, zeroDefault, nil},
	// init_ext_cnt has no default and no maximum of its own.
	{"sp create", "init-ext-cnt", "init_ext_cnt", 1, 0, zeroRefused, nil},
	{"sp create", "bitmap-chunk-blocks",
		"bdev_conf.redund_conf.redund_md_raid1.bitmap_chunk_block_cnt",
		common.MinChunkBlockCnt, common.MaxChunkBlockCnt, zeroDefault, nil},
	{"sp create", "stripe-size", "bdev_conf.dm_raid0_conf.stripe_size",
		common.MinDmRaid0StripeSize, common.MaxDmRaid0StripeSize,
		zeroNotSent, nil},
	{"sp create", "block-size", "bdev_conf.dm_pool_conf.data_block_size",
		common.MinDmPoolDataBlockSize, common.MaxDmPoolDataBlockSize,
		zeroNotSent, nil},
	// A data grow must state a non-zero ext_cnt, whose value is not bounded.
	{"sp grow-slice", "ext", "ext_cnt", 1, 0, zeroRefused, nil},
	// Without --ori a thin device needs a size; no maximum.
	{"td create", "size", "size", 1, 0, zeroRefused, nil},
	// A bitmap window of block_cnt 0 is the whole slice or leg, a zero the
	// gateway passes on to the agent; no other value is bounded.
	{"td get-bm", "cnt", "block_cnt", 1, 0, zeroDefault, nil},
	{"td get-leg-bm", "cnt", "block_cnt", 1, 0, zeroDefault, nil},
	{"clone create", "src-slices", "src_slice_cnt",
		1, common.MaxSliceCntPerSp, zeroRefused, nil},
	{"clone create", "src-stripe", "src_stripe_size",
		common.MinDmRaid0StripeSize, common.MaxDmRaid0StripeSize,
		zeroRefused, nil},
	{"clone create", "src-block", "src_block_size",
		common.MinDmPoolDataBlockSize, common.MaxDmPoolDataBlockSize,
		zeroRefused, nil},
	{"clone create", "hyd-threshold", "dm_clone_conf.hydration_threshold",
		1, common.MaxCloneThreshold, zeroNotSent, nil},
	{"clone create", "hyd-batch", "dm_clone_conf.hydration_batch_size",
		1, common.MaxCloneBatchSize, zeroNotSent, nil},
	{"migr create", "hyd-threshold", "dm_clone_conf.hydration_threshold",
		1, common.MaxCloneThreshold, zeroNotSent, nil},
	{"migr create", "hyd-batch", "dm_clone_conf.hydration_batch_size",
		1, common.MaxCloneBatchSize, zeroNotSent, nil},
	// An empty host list is sent as one, and is the gateway's to judge.
	{"ss create", "hosts", "allowed_hosts",
		1, common.MaxHostCntPerSs, zeroDefault, hostArg},
	{"ss set-hosts", "hosts", "allowed_hosts",
		1, common.MaxHostCntPerSs, zeroDefault, hostArg},
	{"xfer create", "hosts", "allowed_hosts",
		1, common.MaxHostCntPerSs, zeroDefault, hostArg},
	{"xfer set-hosts", "hosts", "allowed_hosts",
		1, common.MaxHostCntPerSs, zeroDefault, hostArg},
	// An empty bitmap is sent on purpose (dnvctl.md, `clone` —
	// `ctl/clone.go`); a clone page holds at most one chunk, while a
	// migration chunk has no byte cap.
	{"clone append-bm", "bm-hex", "bitmap",
		1, common.CloneBmChunkBytes, zeroRefused, bitmapArg},
	{"migr append-bm", "bm-hex", "bitmap", 1, 0, zeroRefused, bitmapArg},
	// A slot list is judged by its values, distinct and each below
	// CnCntlidSlotCnt, so its length has no bound of its own beyond this:
	// the full new list must not be empty. sp create's empty list is the
	// gateway's default, and its length is judged against cntlr_cnt, a rule
	// between two fields that this table does not model.
	{"sp set-cntlid-slots", "slots", "cntlid_slot_list",
		1, 0, zeroRefused, slotArg},
}

// fieldAt follows a dotted path of proto field names from msg and reports
// what the last field holds: a number as itself, a list by its length and
// bytes by their count. present is false when a message on the way is unset,
// which is how a "not given" carrier shows.
func fieldAt(msg proto.Message, path string) (uint64, bool, error) {
	m := msg.ProtoReflect()
	names := strings.Split(path, ".")
	for i, name := range names {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return 0, false, fmt.Errorf("%s has no field %q",
				m.Descriptor().FullName(), name)
		}
		if i < len(names)-1 {
			if fd.Kind() != protoreflect.MessageKind || fd.IsList() {
				return 0, false, fmt.Errorf("%s is not a message field",
					fd.FullName())
			}
			if !m.Has(fd) {
				return 0, false, nil
			}
			m = m.Get(fd).Message()
			continue
		}
		value := m.Get(fd)
		switch {
		case fd.IsList():
			return uint64(value.List().Len()), true, nil
		case fd.Kind() == protoreflect.BytesKind:
			return uint64(len(value.Bytes())), true, nil
		case fd.Kind() == protoreflect.Uint32Kind ||
			fd.Kind() == protoreflect.Uint64Kind:
			return value.Uint(), true, nil
		}
		return 0, false, fmt.Errorf("%s is neither a number, a list nor "+
			"bytes", fd.FullName())
	}
	return 0, false, fmt.Errorf("empty field path")
}

// accepts reports whether the gateway takes v for this row.
func (row boundRow) accepts(v uint64) bool {
	if v == 0 {
		return row.zero != zeroRefused
	}
	return v >= row.min && (row.max == 0 || v <= row.max)
}

// refusal is the stand-in's refusal text for a value of this row.
func (row boundRow) refusal(v uint64) string {
	switch {
	case v == 0:
		return fmt.Sprintf("%s must not be 0", row.field)
	case row.max == 0:
		return fmt.Sprintf("%s %d is below %d", row.field, v, row.min)
	}
	return fmt.Sprintf("%s %d is outside [%d, %d]",
		row.field, v, row.min, row.max)
}

// boundGateway judges the one field a row names against the row's bounds,
// as the gateway's validation does, and accepts anything else. A carrier
// that was not sent leaves nothing to judge.
func boundGateway(row boundRow) func(proto.Message) (proto.Message, error) {
	return func(req proto.Message) (proto.Message, error) {
		v, present, err := fieldAt(req, row.field)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if present && !row.accepts(v) {
			return nil, status.Error(codes.InvalidArgument, row.refusal(v))
		}
		return nil, nil
	}
}

// values is the walk of one row: zero, one under the minimum when that is
// not zero, the minimum, and the maximum and one over it when there is one.
func (row boundRow) values() []uint64 {
	out := []uint64{0}
	if row.min > 1 {
		out = append(out, row.min-1)
	}
	out = append(out, row.min)
	if row.max != 0 {
		out = append(out, row.max, row.max+1)
	}
	return out
}

// TestCountAndSizeFlagsReachTheGateway drives every count or size flag of
// boundRows at zero, one under its minimum, at its minimum, at its maximum
// and one over, where those exist. dnvctl checks none of these bounds (CT8):
// every value goes out as typed, a zero as the row's zeroRule says, and the
// gateway's verdict comes back unchanged — the CT4 document with exit 0
// inside the bounds, the CT5 failure line with exit 1 outside them.
func TestCountAndSizeFlagsReachTheGateway(t *testing.T) {
	cmdToRpc := rpcOfCmd()
	for _, row := range boundRows {
		arg := row.arg
		if arg == nil {
			arg = func(v uint64) string { return strconv.FormatUint(v, 10) }
		}
		for _, v := range row.values() {
			name := fmt.Sprintf("%s --%s %d", row.path, row.flag, v)
			t.Run(name, func(t *testing.T) {
				client := &recordingClient{
					want:   cmdToRpc[row.path],
					answer: boundGateway(row),
				}
				res := runLeaf(t, client, row.path, "--"+row.flag, arg(v))
				if row.accepts(v) {
					wantAccepted(t, res, client)
				} else {
					wantRefused(t, res, client, "INVALID_ARGUMENT",
						row.refusal(v))
				}
				if client.req == nil {
					t.Fatalf("no request was recorded")
				}
				got, present, err := fieldAt(client.req, row.field)
				if err != nil {
					t.Fatalf("reading %s: %v", row.field, err)
				}
				switch {
				case v == 0 && row.zero == zeroNotSent:
					if present {
						t.Errorf("a zero sent the carrier of %s, want "+
							"none (not given)", row.field)
					}
				case !present:
					t.Errorf("%s was not sent, want %d", row.field, v)
				case got != v:
					t.Errorf("%s = %d, want %d as typed",
						row.field, got, v)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Every name and NQN at its length limit and one over
// ---------------------------------------------------------------------------

// lengthRow is one text flag the gateway bounds by length (architecture.md,
// Common validation): the leaf, the flag, the request field it fills (a
// string, or a list of strings that gets the one entry typed), the most
// bytes the gateway takes, and the text of n bytes the row types.
type lengthRow struct {
	path  string
	flag  string
	field string
	limit int
	text  func(n int) string
}

// nameOfLen is a name of n bytes drawn from ValidStrPattern.
func nameOfLen(n int) string { return strings.Repeat("a", n) }

// nqnOfLen is an NQN of n bytes that matches ValidNqnPattern outside the dnv
// namespace.
func nqnOfLen(n int) string {
	const head = "nqn.2014-08.org.example:"
	return head + strings.Repeat("x", n-len(head))
}

var lengthRows = []lengthRow{
	{"td create", "name", "td_name", common.MaxStrSize, nameOfLen},
	// The global --sp and --cluster fill names of the request too.
	{"td create", "sp", "sp_name", common.MaxStrSize, nameOfLen},
	{"td create", "cluster", "cluster_name", common.MaxStrSize, nameOfLen},
	{"ss create", "nqn", "nqn", common.MaxNqnLength, nqnOfLen},
	{"ss create", "hosts", "allowed_hosts", common.MaxNqnLength, nqnOfLen},
	{"xfer set-hosts", "hosts", "allowed_hosts",
		common.MaxNqnLength, nqnOfLen},
}

// textsAt reads a top-level string field of msg, or each entry of a list of
// strings.
func textsAt(msg proto.Message, field string) ([]string, error) {
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil || fd.Kind() != protoreflect.StringKind {
		return nil, fmt.Errorf("%s has no string field %q",
			m.Descriptor().FullName(), field)
	}
	if !fd.IsList() {
		return []string{m.Get(fd).String()}, nil
	}
	list := m.Get(fd).List()
	out := make([]string, list.Len())
	for i := range out {
		out[i] = list.Get(i).String()
	}
	return out, nil
}

// refusal is the stand-in's refusal text for a value of n bytes.
func (row lengthRow) refusal(n int) string {
	return fmt.Sprintf("%s is %d bytes, the maximum is %d",
		row.field, n, row.limit)
}

// lengthGateway refuses a text of the row's field longer than the row's
// limit, as the gateway's validation does, and accepts anything else.
func lengthGateway(row lengthRow) func(proto.Message) (proto.Message, error) {
	return func(req proto.Message) (proto.Message, error) {
		texts, err := textsAt(req, row.field)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		for _, text := range texts {
			if len(text) > row.limit {
				return nil, status.Error(codes.InvalidArgument,
					row.refusal(len(text)))
			}
		}
		return nil, nil
	}
}

// TestNamesAndNqnsReachTheGatewayAtTheirLength drives a name and an NQN flag
// of each kind at the gateway's length limit and one byte over. dnvctl
// checks no length (CT8): the text goes out as typed, and the gateway's
// verdict comes back unchanged, the CT4 document with exit 0 at the limit
// and the CT5 failure line with exit 1 one byte over.
func TestNamesAndNqnsReachTheGatewayAtTheirLength(t *testing.T) {
	cmdToRpc := rpcOfCmd()
	for _, row := range lengthRows {
		for _, n := range []int{row.limit, row.limit + 1} {
			name := fmt.Sprintf("%s --%s of %d bytes", row.path, row.flag, n)
			t.Run(name, func(t *testing.T) {
				text := row.text(n)
				if len(text) != n {
					t.Fatalf("the row typed %d bytes, want %d", len(text), n)
				}
				client := &recordingClient{
					want:   cmdToRpc[row.path],
					answer: lengthGateway(row),
				}
				res := runLeaf(t, client, row.path, "--"+row.flag, text)
				if n <= row.limit {
					wantAccepted(t, res, client)
				} else {
					wantRefused(t, res, client, "INVALID_ARGUMENT",
						row.refusal(n))
				}
				if client.req == nil {
					t.Fatalf("no request was recorded")
				}
				got, err := textsAt(client.req, row.field)
				if err != nil {
					t.Fatalf("reading %s: %v", row.field, err)
				}
				if !slices.Equal(got, []string{text}) {
					sizes := make([]int, len(got))
					for i, value := range got {
						sizes[i] = len(value)
					}
					t.Errorf("%s holds values of %v bytes, want the one "+
						"value typed, %d bytes", row.field, sizes, n)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// The one limit dnvctl checks: the width of the field
// ---------------------------------------------------------------------------

// numericFlag is one leaf flag declared as a pflag number, found by the walk.
type numericFlag struct {
	path string
	name string
	bits int
}

// flagBits is the width of each pflag number type the walk knows.
var flagBits = map[string]int{"uint32": 32, "uint64": 64}

// numericLeafFlags walks the command tree for every leaf flag pflag parses
// as a number. A leaf flag of a type neither walk covers fails it — the two
// number types are this walk's, a string is the text walk's and a bool holds
// no number — so a new kind of flag is noticed rather than skipped.
func numericLeafFlags(t *testing.T) []numericFlag {
	t.Helper()
	root := NewRootCmd()
	var out []numericFlag
	for _, path := range leafPaths(t, root) {
		findLeaf(t, root, path).Flags().VisitAll(func(f *pflag.Flag) {
			kind := f.Value.Type()
			if bits, ok := flagBits[kind]; ok {
				out = append(out, numericFlag{path, f.Name, bits})
				return
			}
			if kind != "string" && kind != "bool" {
				t.Errorf("%q declares --%s as %s, which no width walk "+
					"covers", path, f.Name, kind)
			}
		})
	}
	return out
}

// fieldsHolding lists the kind of every number field of msg, at any depth
// and in any list, whose value spells text in decimal. The requests hold
// numbers only as uint32, uint64 and enums.
func fieldsHolding(msg protoreflect.Message, text string) []protoreflect.Kind {
	var out []protoreflect.Kind
	msg.Range(func(fd protoreflect.FieldDescriptor,
		value protoreflect.Value) bool {
		one := func(v protoreflect.Value) {
			switch fd.Kind() {
			case protoreflect.MessageKind:
				out = append(out, fieldsHolding(v.Message(), text)...)
			case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
				if strconv.FormatUint(v.Uint(), 10) == text {
					out = append(out, fd.Kind())
				}
			case protoreflect.EnumKind:
				if strconv.FormatInt(int64(v.Enum()), 10) == text {
					out = append(out, fd.Kind())
				}
			}
		}
		switch {
		case fd.IsMap():
		case fd.IsList():
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				one(list.Get(i))
			}
		default:
			one(value)
		}
		return true
	})
	return out
}

// wantUnparsed is CT5's exit-2 row for a value that does not parse: no RPC
// issued, stdout empty, and stderr opening with the parser's refusal.
func wantUnparsed(
	t *testing.T, res cliResult, client *recordingClient,
	flag, value, prefix string,
) {
	t.Helper()
	if res.code != 2 {
		t.Errorf("--%s %s exited %d, want 2 (stderr %q)", flag, value,
			res.code, res.stderr)
	}
	if client.calls != 0 {
		t.Errorf("--%s %s issued %d RPCs, want 0", flag, value, client.calls)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
	if !strings.HasPrefix(res.stderr, prefix) {
		t.Errorf("stderr = %q, want the refusal %q…", res.stderr, prefix)
	}
}

// TestNumericFlagsParseAtTheirFieldWidth walks every leaf flag declared as a
// pflag number. The largest value of the flag's type is sent as typed, and
// exactly one field of the request holds it, a field of the flag's own
// width: a flag wider than its field would cut the value on the way, and a
// narrower one would refuse values the field carries, a client-side bound
// CT8 does not allow. The smallest, 0, is accepted too. One past either end
// does not parse: pflag's usage error, exit 2, no RPC issued (CT5).
func TestNumericFlagsParseAtTheirFieldWidth(t *testing.T) {
	cmdToRpc := rpcOfCmd()
	flags := numericLeafFlags(t)

	// The walk must reach every flag the bound table drives and the count of
	// every paged list. That keeps it from passing on an empty tree, and it
	// fails when one of those flags stops being a pflag number.
	for _, row := range boundRows {
		if row.arg == nil && flagBitsOf(flags, row.path, row.flag) == 0 {
			t.Errorf("the walk did not find %q --%s", row.path, row.flag)
		}
	}
	for _, row := range listRows {
		if flagBitsOf(flags, row.path, "count") == 0 {
			t.Errorf("the walk did not find %q --count", row.path)
		}
	}

	wantKind := map[int]protoreflect.Kind{
		32: protoreflect.Uint32Kind,
		64: protoreflect.Uint64Kind,
	}
	for _, flag := range flags {
		t.Run(flag.path+" --"+flag.name, func(t *testing.T) {
			top := uint64(math.MaxUint64) >> (64 - flag.bits)
			text := strconv.FormatUint(top, 10)
			client := &recordingClient{want: cmdToRpc[flag.path]}
			res := runLeaf(t, client, flag.path, "--"+flag.name, text)
			wantAccepted(t, res, client)
			kinds := fieldsHolding(client.req.ProtoReflect(), text)
			if len(kinds) != 1 || kinds[0] != wantKind[flag.bits] {
				t.Errorf("--%s %s reached fields of kinds %v, want one "+
					"%v field", flag.name, text, kinds,
					wantKind[flag.bits])
			}

			// A proto3 zero leaves no field set to look for, so only the
			// exit and the one RPC are pinned here; the bound table pins
			// where the zero of a count or a size goes.
			client = &recordingClient{want: cmdToRpc[flag.path]}
			res = runLeaf(t, client, flag.path, "--"+flag.name, "0")
			wantAccepted(t, res, client)

			over := new(big.Int).Add(
				new(big.Int).SetUint64(top), big.NewInt(1)).String()
			for _, past := range []string{over, "-1"} {
				client = &recordingClient{}
				res = runLeaf(t, client, flag.path, "--"+flag.name, past)
				wantUnparsed(t, res, client, flag.name, past,
					fmt.Sprintf("dnvctl: invalid argument %q for %q flag",
						past, "--"+flag.name))
			}
		})
	}
}

// flagBitsOf is the width the walk found for one flag, or 0.
func flagBitsOf(flags []numericFlag, path, name string) int {
	for _, flag := range flags {
		if flag.path == path && flag.name == name {
			return flag.bits
		}
	}
	return 0
}

// textNumber is one flag declared as a string that dnvctl parses as a number
// itself, found by the text walk: the leaf, the flag, and the kind of the
// request field the number reached.
type textNumber struct {
	path string
	name string
	kind protoreflect.Kind
}

// textProbe is the number the text walk types into every string flag. It
// fits a field of any width.
const textProbe = "201"

// textGlobals are the global flags that fill request fields
// (addGlobalFlags). The text walk types them on every leaf, beside the
// leaf's own string flags.
var textGlobals = []string{"cluster", "sp", "rev"}

// textNumberNames are the flags declared as strings that dnvctl parses as
// numbers: the ids (hexOf), --ids (idListOf), --slots (u32ListOf), --level
// (spParseLevel) and --rev (revToken). The text walk must find a leaf flag of
// one of these names on every leaf that declares it, and the global --rev on
// exactly the leaves whose request has a CT3 token field, which keeps the
// walk from passing on a probe that finds nothing.
var textNumberNames = []string{
	"id", "slice", "leg", "grp", "spare", "target", "src-side",
	"ids", "slots", "level", "rev",
}

// textNumberFlags types textProbe into every string flag of every leaf, and
// into each of textGlobals, and keeps each flag whose probe reaches a number
// field of the request: those are the numbers dnvctl parses itself. A probe
// that does not parse (a hex bitmap, a redundancy name, a --rev on a leaf
// with no token field) or that lands in a string field is a flag that holds
// no number. Any such flag found is walked, so a new one is covered without
// a table to edit.
func textNumberFlags(
	t *testing.T, cmdToRpc map[string]string,
) []textNumber {
	t.Helper()
	root := NewRootCmd()
	var out []textNumber
	for _, path := range leafPaths(t, root) {
		var names []string
		findLeaf(t, root, path).Flags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() == "string" {
				names = append(names, f.Name)
			}
		})
		leafNames := len(names)
		for i, name := range append(names, textGlobals...) {
			client := &recordingClient{want: cmdToRpc[path]}
			res := runLeaf(t, client, path, "--"+name, textProbe)
			var kinds []protoreflect.Kind
			if res.code == 0 && client.req != nil {
				kinds = fieldsHolding(client.req.ProtoReflect(), textProbe)
			}
			switch {
			case len(kinds) == 1:
				out = append(out, textNumber{path, name, kinds[0]})
			case len(kinds) > 1:
				t.Errorf("%q --%s %s reached %d number fields, want one",
					path, name, textProbe, len(kinds))
			case i < leafNames && slices.Contains(textNumberNames, name):
				t.Errorf("%q --%s %s reached no number field (exit %d, "+
					"stderr %q)", path, name, textProbe, res.code,
					res.stderr)
			}
		}
	}
	for _, name := range textNumberNames {
		if !slices.ContainsFunc(out, func(f textNumber) bool {
			return f.name == name
		}) {
			t.Errorf("the text walk did not find --%s", name)
		}
	}

	// --rev is global, so "every leaf that declares it" is every leaf whose
	// request carries a token (TestTokenCarriersMatchCT3 pins which).
	carriers := map[string]bool{}
	for _, row := range sweepRows {
		if revTokenField(row.want.ProtoReflect().Descriptor()) != nil {
			carriers[rpcToCmd[row.rpc]] = true
		}
	}
	found := map[string]bool{}
	for _, flag := range out {
		if flag.name == "rev" {
			found[flag.path] = true
			if !carriers[flag.path] {
				t.Errorf("the text walk found --rev on %q, whose request "+
					"carries no token", flag.path)
			}
		}
	}
	for path := range carriers {
		if !found[path] {
			t.Errorf("the text walk did not find --rev on %q, whose "+
				"request carries a token", path)
		}
	}
	return out
}

// fieldRange is the range of numbers a request field of this kind holds. An
// enum is an int32.
func fieldRange(t *testing.T, kind protoreflect.Kind) (lo, hi *big.Int) {
	t.Helper()
	switch kind {
	case protoreflect.Uint32Kind:
		return big.NewInt(0), new(big.Int).SetUint64(math.MaxUint32)
	case protoreflect.Uint64Kind:
		return big.NewInt(0), new(big.Int).SetUint64(math.MaxUint64)
	case protoreflect.EnumKind:
		return big.NewInt(math.MinInt32), big.NewInt(math.MaxInt32)
	}
	t.Fatalf("no range for a %v field", kind)
	return nil, nil
}

// TestTextNumbersParseAtTheirFieldWidth is the width walk of the numbers
// held as strings. For each one the text walk finds, the largest value its
// field holds, and the smallest when that is below zero, is sent as typed and
// reaches exactly that field. One past either end of the field does not
// parse: dnvctl's "invalid --<flag>" usage error, exit 2, no RPC issued (CT8,
// CT5). A reader wider than its field would cut such a value and send it,
// and a count cut that way to zero asks for the server's default page.
func TestTextNumbersParseAtTheirFieldWidth(t *testing.T) {
	cmdToRpc := rpcOfCmd()
	for _, flag := range textNumberFlags(t, cmdToRpc) {
		t.Run(flag.path+" --"+flag.name, func(t *testing.T) {
			lo, hi := fieldRange(t, flag.kind)
			one := big.NewInt(1)
			ends := []*big.Int{hi}
			if lo.Sign() < 0 {
				ends = append(ends, lo)
			}
			for _, end := range ends {
				text := end.String()
				client := &recordingClient{want: cmdToRpc[flag.path]}
				res := runLeaf(t, client, flag.path, "--"+flag.name, text)
				wantAccepted(t, res, client)
				kinds := fieldsHolding(client.req.ProtoReflect(), text)
				if len(kinds) != 1 || kinds[0] != flag.kind {
					t.Errorf("--%s %s reached fields of kinds %v, want "+
						"one %v field", flag.name, text, kinds, flag.kind)
				}
			}
			for _, past := range []*big.Int{
				new(big.Int).Add(hi, one), new(big.Int).Sub(lo, one),
			} {
				text := past.String()
				client := &recordingClient{}
				res := runLeaf(t, client, flag.path, "--"+flag.name, text)
				wantUnparsed(t, res, client, flag.name, text,
					"dnvctl: invalid --"+flag.name+":")
			}
		})
	}
}
