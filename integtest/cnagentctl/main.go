// Command cnagentctl is the gRPC driver of the cn-agent integration test
// (doc/cnagent_integtest.md §8). The agent serves plaintext gRPC without
// server reflection, so grpcurl cannot drive it; this binary speaks the
// generated ControllerNodeAgent client instead and prints every reply as
// protojson (proto field names) on stdout for the test script to parse with
// jq.
//
// It exits non-zero on a gRPC error or when agent_reply.code differs from
// --expect-code (default 0), so the caller's `set -e` catches both.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Flag value types. Every id flag accepts decimal or 0x hex (§8).
// ---------------------------------------------------------------------------

type hexUint uint64

func (h *hexUint) String() string { return fmt.Sprintf("%#x", uint64(*h)) }

func (h *hexUint) Set(s string) error {
	value, err := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return err
	}
	*h = hexUint(value)
	return nil
}

// cntlrPtrList collects repeated --cntlr sp:cntlr flags into the declarative
// full list SyncupCn expects.
type cntlrPtrList []*pb.CntlrPointer

func (l *cntlrPtrList) String() string { return fmt.Sprintf("%d cntlrs", len(*l)) }

func (l *cntlrPtrList) Set(s string) error {
	ptr, err := parseCntlrPointer(s)
	if err != nil {
		return err
	}
	*l = append(*l, ptr)
	return nil
}

func parseCntlrPointer(s string) (*pb.CntlrPointer, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("want sp:cntlr, got %q", s)
	}
	ids := make([]uint64, 2)
	for i, part := range parts {
		value, err := strconv.ParseUint(strings.TrimSpace(part), 0, 64)
		if err != nil {
			return nil, fmt.Errorf("%q in %q: %w", part, s, err)
		}
		ids[i] = value
	}
	return &pb.CntlrPointer{SpId: ids[0], CntlrId: ids[1]}, nil
}

// ---------------------------------------------------------------------------
// Globals, dialing and output
// ---------------------------------------------------------------------------

type globals struct {
	addr       string
	cluster    hexUint
	cn         hexUint
	traceId    string
	timeout    float64
	expectCode uint
}

// bind registers the §8 global flags on a subcommand's flag set, so they may
// be given in any order after the subcommand name. wait-hydrated binds with
// withTimeout = false and spends --timeout on its own polling budget.
func (g *globals) bind(fs *flag.FlagSet, withTimeout bool) {
	fs.StringVar(&g.addr, "addr", "", "agent gRPC endpoint ip:port (required)")
	g.cluster = 1
	fs.Var(&g.cluster, "cluster", "cluster id")
	fs.Var(&g.cn, "cn", "controller node id")
	fs.StringVar(&g.traceId, "trace-id", "",
		"value of the trace_id gRPC metadata key")
	g.timeout = 10
	if withTimeout {
		fs.Float64Var(&g.timeout, "timeout", 10,
			"per-RPC timeout in seconds")
	}
	fs.UintVar(&g.expectCode, "expect-code", 0,
		"the AgentReply.code the call must return")
}

func (g *globals) dial() (*grpc.ClientConn, pb.ControllerNodeAgentClient, error) {
	if g.addr == "" {
		return nil, nil, fmt.Errorf("--addr is required")
	}
	conn, err := grpc.NewClient(g.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, pb.NewControllerNodeAgentClient(conn), nil
}

// rpcCtx carries the trace id as gRPC metadata so the driver's call, both
// agents' handlers and every os command they run share one id in the logs.
func (g *globals) rpcCtx() (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if g.traceId != "" {
		ctx = metadata.AppendToOutgoingContext(
			ctx, common.TraceIdMetadataKey, g.traceId)
	}
	return context.WithTimeout(
		ctx, time.Duration(g.timeout*float64(time.Second)))
}

// emit prints a reply as one line of JSON. protojson deliberately varies its
// whitespace, so the result is re-marshaled through encoding/json (sorted
// keys, stable spacing) to stay diffable — case D compares snapshots.
func emit(msg proto.Message) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		die("marshaling the reply failed: %v", err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		die("re-parsing the reply failed: %v", err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		die("re-marshaling the reply failed: %v", err)
	}
	fmt.Println(string(out))
}

// emitBitmap prints a bitmap reply: the raw bytes as lowercase hex on the
// first line, the bit count on the second. The bitmap RPCs carry no
// AgentReply, and their protojson would render the bytes as base64 — the
// script asserts the hex string itself (§12, §13).
func emitBitmap(bitmap []byte) {
	fmt.Println(hex.EncodeToString(bitmap))
	fmt.Printf("bits=%d\n", len(bitmap)*8)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cnagentctl: "+format+"\n", args...)
	os.Exit(1)
}

// checkReply enforces the expected AgentReply.code; the reply itself has
// already been printed, so a failure leaves the details on stdout.
func (g *globals) checkReply(reply *pb.AgentReply) {
	if uint(reply.GetCode()) != g.expectCode {
		die("agent_reply.code = %d (%s), want %d",
			reply.GetCode(), reply.GetDetails(), g.expectCode)
	}
}

// ---------------------------------------------------------------------------
// Subcommands
// ---------------------------------------------------------------------------

type command struct {
	name string
	run  func(args []string)
}

var commands = []command{
	{"get-cn-size", cmdGetCnSize},
	{"syncup-cn", cmdSyncupCn},
	{"syncup-cntlr", cmdSyncupCntlr},
	{"push-clone-bm", cmdPushCloneBm},
	{"get-cn-info", cmdGetCnInfo},
	{"get-cntlr-info", cmdGetCntlrInfo},
	{"check-cn", cmdCheckCn},
	{"check-cntlr", cmdCheckCntlr},
	{"get-td-bm", cmdGetTdBm},
	{"get-leg-bm", cmdGetLegBm},
	{"wait-hydrated", cmdWaitHydrated},
	{"md-name", cmdMdName},
	{"host-id", cmdHostId},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	for _, cmd := range commands {
		if cmd.name == os.Args[1] {
			cmd.run(os.Args[2:])
			return
		}
	}
	usage()
}

func usage() {
	names := make([]string, 0, len(commands))
	for _, cmd := range commands {
		names = append(names, cmd.name)
	}
	fmt.Fprintf(os.Stderr,
		"usage: cnagentctl <%s> [flags]\n", strings.Join(names, "|"))
	os.Exit(2)
}

func newFlagSet(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	g.bind(fs, true)
	return fs
}

// cntlrPointerFlags registers the --sp/--cntlr pair of the object-scoped
// RPCs.
func cntlrPointerFlags(fs *flag.FlagSet) (*hexUint, *hexUint) {
	var sp, cntlr hexUint
	fs.Var(&sp, "sp", "storage pool id")
	fs.Var(&cntlr, "cntlr", "controller id")
	return &sp, &cntlr
}

func cntlrPointerOf(sp, cntlr *hexUint) *pb.CntlrPointer {
	return &pb.CntlrPointer{
		SpId:    uint64(*sp),
		CntlrId: uint64(*cntlr),
	}
}

// cmdGetCnSize doubles as the agent liveness probe: GetCnSize takes no lock,
// so --wait retries it until the freshly started agent answers (§7 step 7).
func cmdGetCnSize(args []string) {
	var g globals
	fs := newFlagSet("get-cn-size", &g)
	wait := fs.Float64("wait", 0, "seconds to retry until the agent answers")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(time.Duration(*wait * float64(time.Second)))
	req := &pb.GetCnSizeRequest{
		ClusterId: uint64(g.cluster), CnId: uint64(g.cn)}
	for attempt := 1; ; attempt++ {
		ctx, cancel := g.rpcCtx()
		reply, err := client.GetCnSize(ctx, req)
		cancel()
		if err == nil {
			emit(reply)
			return
		}
		if time.Now().After(deadline) {
			die("GetCnSize failed after %d attempts: %v", attempt, err)
		}
		fmt.Fprintf(os.Stderr,
			"cnagentctl: waiting for %s (attempt %d): %v\n",
			g.addr, attempt, err)
		time.Sleep(500 * time.Millisecond)
	}
}

// cmdSyncupCn sends the full desired cntlr pointer list every call — the RPC
// is declarative. qos_ratio is deliberately never set (deferred, cnagent.md
// CN6).
func cmdSyncupCn(args []string) {
	var g globals
	fs := newFlagSet("syncup-cn", &g)
	var revision hexUint
	var cntlrs cntlrPtrList
	fs.Var(&revision, "revision", "request revision")
	fs.Var(&cntlrs, "cntlr",
		"sp:cntlr, repeatable — the full desired cntlr list")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId:        uint64(g.cluster),
		CnId:             uint64(g.cn),
		Revision:         uint64(revision),
		CntlrPointerList: cntlrs,
	})
	if err != nil {
		die("SyncupCn failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

// cmdSyncupCntlr reads one complete SyncupCntlrRequest as protojson: the
// message is far too deep for flags, so the script generates it as a file and
// edits only the fields that change between steps (§8). The ids are
// cross-checked against the globals, which is what catches a request file
// aimed at the wrong node.
func cmdSyncupCntlr(args []string) {
	var g globals
	fs := newFlagSet("syncup-cntlr", &g)
	reqPath := fs.String("req", "",
		"path to the SyncupCntlrRequest as protojson (required)")
	fs.Parse(args)

	if *reqPath == "" {
		die("--req is required")
	}
	raw, err := os.ReadFile(*reqPath)
	if err != nil {
		die("--req: %v", err)
	}
	req := &pb.SyncupCntlrRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		die("--req %s: %v", *reqPath, err)
	}
	if req.GetClusterId() != uint64(g.cluster) {
		die("--req %s: cluster_id is %#x, want %#x",
			*reqPath, req.GetClusterId(), uint64(g.cluster))
	}
	if req.GetCnId() != uint64(g.cn) {
		die("--req %s: cn_id is %#x, want %#x",
			*reqPath, req.GetCnId(), uint64(g.cn))
	}

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.SyncupCntlr(ctx, req)
	if err != nil {
		die("SyncupCntlr failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdPushCloneBm(args []string) {
	var g globals
	fs := newFlagSet("push-clone-bm", &g)
	sp, cntlr := cntlrPointerFlags(fs)
	var revision, clone hexUint
	fs.Var(&revision, "revision", "the cntlr's current revision (gates only)")
	fs.Var(&clone, "clone", "clone id")
	bmIdx := fs.Uint("bm-idx", 0, "chunk index")
	bitmapHex := fs.String("bitmap-hex", "", "chunk bytes as hex")
	fs.Parse(args)

	bitmap, err := hex.DecodeString(strings.TrimSpace(*bitmapHex))
	if err != nil {
		die("--bitmap-hex: %v", err)
	}

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.PushCloneBitmap(ctx, &pb.PushCloneBitmapRequest{
		ClusterId:    uint64(g.cluster),
		CnId:         uint64(g.cn),
		CntlrPointer: cntlrPointerOf(sp, cntlr),
		Revision:     uint64(revision),
		CloneId:      uint64(clone),
		BmIdx:        uint32(*bmIdx),
		Bitmap:       bitmap,
	})
	if err != nil {
		die("PushCloneBitmap failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdGetCnInfo(args []string) {
	var g globals
	fs := newFlagSet("get-cn-info", &g)
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.GetCnInfo(ctx, &pb.GetCnInfoRequest{
		ClusterId: uint64(g.cluster), CnId: uint64(g.cn)})
	if err != nil {
		die("GetCnInfo failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdGetCntlrInfo(args []string) {
	var g globals
	fs := newFlagSet("get-cntlr-info", &g)
	sp, cntlr := cntlrPointerFlags(fs)
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
		ClusterId:    uint64(g.cluster),
		CnId:         uint64(g.cn),
		CntlrPointer: cntlrPointerOf(sp, cntlr),
	})
	if err != nil {
		die("GetCntlrInfo failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

// cmdCheckCn runs exactly one worker-initiated round on the bidi stream and
// closes it.
func cmdCheckCn(args []string) {
	var g globals
	fs := newFlagSet("check-cn", &g)
	var revision hexUint
	fs.Var(&revision, "revision", "the revision the worker believes is current")
	showInfo := fs.Bool("show-info", false, "ask for the info on every reply")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	stream, err := client.CheckCn(ctx)
	if err != nil {
		die("CheckCn failed: %v", err)
	}
	if err := stream.Send(&pb.CheckCnRequest{
		ClusterId: uint64(g.cluster),
		CnId:      uint64(g.cn),
		Revision:  uint64(revision),
		ShowInfo:  *showInfo,
	}); err != nil {
		die("CheckCn send failed: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		die("CheckCn recv failed: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		die("CheckCn close failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdCheckCntlr(args []string) {
	var g globals
	fs := newFlagSet("check-cntlr", &g)
	sp, cntlr := cntlrPointerFlags(fs)
	var revision hexUint
	fs.Var(&revision, "revision", "the revision the worker believes is current")
	showInfo := fs.Bool("show-info", false, "ask for the info on every reply")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	stream, err := client.CheckCntlr(ctx)
	if err != nil {
		die("CheckCntlr failed: %v", err)
	}
	if err := stream.Send(&pb.CheckCntlrRequest{
		ClusterId:    uint64(g.cluster),
		CnId:         uint64(g.cn),
		CntlrPointer: cntlrPointerOf(sp, cntlr),
		Revision:     uint64(revision),
		ShowInfo:     *showInfo,
	}); err != nil {
		die("CheckCntlr send failed: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		die("CheckCntlr recv failed: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		die("CheckCntlr close failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

// cmdGetTdBm reads the thin device's allocation bitmap. GetThinDeviceBmReply
// carries no AgentReply, so there is nothing to check — a refusal arrives as
// a gRPC error.
func cmdGetTdBm(args []string) {
	var g globals
	fs := newFlagSet("get-td-bm", &g)
	sp, cntlr := cntlrPointerFlags(fs)
	var td hexUint
	fs.Var(&td, "td", "thin device id")
	sliceIdx := fs.Uint("slice-idx", 0, "slice index")
	startBlock := fs.Uint64("start-block", 0, "first block of the window")
	blockCnt := fs.Uint64("block-cnt", 0, "window length in blocks, 0 = all")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.GetThinDeviceBm(ctx, &pb.GetThinDeviceBmRequest{
		ClusterId:  uint64(g.cluster),
		CnId:       uint64(g.cn),
		SpId:       uint64(*sp),
		CntlrId:    uint64(*cntlr),
		TdId:       uint64(td),
		SliceIdx:   uint32(*sliceIdx),
		StartBlock: *startBlock,
		BlockCnt:   *blockCnt,
	})
	if err != nil {
		die("GetThinDeviceBm failed: %v", err)
	}
	emitBitmap(reply.GetBitmap())
}

// cmdGetLegBm reads a leg's skip bitmap. GetLegBmReply carries no
// AgentReply either.
func cmdGetLegBm(args []string) {
	var g globals
	fs := newFlagSet("get-leg-bm", &g)
	sp, cntlr := cntlrPointerFlags(fs)
	var leg hexUint
	fs.Var(&leg, "leg", "leg id")
	startBlock := fs.Uint64("start-block", 0, "first block of the window")
	blockCnt := fs.Uint64("block-cnt", 0, "window length in blocks, 0 = all")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.GetLegBm(ctx, &pb.GetLegBmRequest{
		ClusterId:  uint64(g.cluster),
		CnId:       uint64(g.cn),
		SpId:       uint64(*sp),
		CntlrId:    uint64(*cntlr),
		LegId:      uint64(leg),
		StartBlock: *startBlock,
		BlockCnt:   *blockCnt,
	})
	if err != nil {
		die("GetLegBm failed: %v", err)
	}
	emitBitmap(reply.GetBitmap())
}

// cmdWaitHydrated polls GetCntlrInfo until the clone reports every region
// hydrated, parsing the raw `dmsetup status` line the agent puts in
// cntlr_info.clone_id_to_dm_clone[<clone>].details. --min-first asserts the
// bitmap jump: case C's first sample must already show >= 32/64 (§13 stage
// 5), and --sample-only stops right there, taking exactly one sample so the
// mid-hydration probe never waits for the copy to finish.
func cmdWaitHydrated(args []string) {
	var g globals
	fs := flag.NewFlagSet("wait-hydrated", flag.ExitOnError)
	g.bind(fs, false)
	sp, cntlr := cntlrPointerFlags(fs)
	var clone hexUint
	fs.Var(&clone, "clone", "clone id")
	interval := fs.Float64("interval", 0.5, "seconds between samples")
	limit := fs.Float64("timeout", 120, "seconds to wait for full hydration")
	minFirst := fs.Uint64("min-first", 0,
		"the first parseable sample must show at least this many hydrated")
	sampleOnly := fs.Bool("sample-only", false,
		"take exactly one sample, print hydrated/total and exit")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()

	req := &pb.GetCntlrInfoRequest{
		ClusterId:    uint64(g.cluster),
		CnId:         uint64(g.cn),
		CntlrPointer: cntlrPointerOf(sp, cntlr),
	}
	deadline := time.Now().Add(time.Duration(*limit * float64(time.Second)))
	var firstHydrated, firstTotal uint64
	haveFirst := false
	samples := 0
	for {
		ctx, cancel := g.rpcCtx()
		reply, err := client.GetCntlrInfo(ctx, req)
		cancel()
		if err != nil {
			die("GetCntlrInfo failed: %v", err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			die("agent_reply.code = %d (%s)",
				reply.GetAgentReply().GetCode(),
				reply.GetAgentReply().GetDetails())
		}
		raw := reply.GetCntlrInfo().
			GetCloneIdToDmClone()[uint64(clone)].GetDetails()
		status, ok := agent.ParseCloneStatus(raw)
		if ok {
			samples++
			if !haveFirst {
				haveFirst = true
				firstHydrated = status.HydratedRegions
				firstTotal = status.TotalRegions
				if *minFirst != 0 && firstHydrated < *minFirst {
					die("first hydration sample is %d/%d, want >= %d",
						firstHydrated, firstTotal, *minFirst)
				}
			}
			fmt.Fprintf(os.Stderr,
				"cnagentctl: hydrated %d/%d\n",
				status.HydratedRegions, status.TotalRegions)
			// The single sample of the §13 stage 5 probe: --min-first is
			// the assertion, the ratio is the observation the script logs
			// as the read-through window HIT or missed.
			if *sampleOnly {
				fmt.Printf("%d/%d\n",
					status.HydratedRegions, status.TotalRegions)
				return
			}
			if status.TotalRegions != 0 &&
				status.HydratedRegions >= status.TotalRegions {
				out, _ := json.Marshal(map[string]any{
					"hydrated":       status.HydratedRegions,
					"total":          status.TotalRegions,
					"first_hydrated": firstHydrated,
					"first_total":    firstTotal,
					"samples":        samples,
				})
				fmt.Println(string(out))
				return
			}
		} else if *sampleOnly {
			die("no dm-clone status for clone %#x (details %q)",
				uint64(clone), raw)
		}
		if time.Now().After(deadline) {
			die("hydration did not finish within %gs (last status %q)",
				*limit, raw)
		}
		time.Sleep(time.Duration(*interval * float64(time.Second)))
	}
}

// cmdMdName prints the md names of one raid1 group, which is how the test
// finds /dev/md/<dev_name>: the device name folds the cluster and cn ids
// through the package-private fnv getShortId, so it cannot be computed in
// bash (§5).
func cmdMdName(args []string) {
	var g globals
	fs := newFlagSet("md-name", &g)
	var sp hexUint
	fs.Var(&sp, "sp", "storage pool id")
	sliceIdx := fs.Uint("slice-idx", 0, "slice index")
	grpIdx := fs.Uint("grp-idx", 0, "group index")
	isMeta := fs.Bool("meta", false, "the group is a meta group")
	fs.Parse(args)

	nf := common.NewNameFmt("")
	devName := nf.CnMdDevName(uint64(g.cluster), uint64(g.cn), uint64(sp),
		uint32(*sliceIdx), uint32(*grpIdx), *isMeta)
	arrayName := nf.CnMdArrayName(uint64(sp),
		uint32(*sliceIdx), uint32(*grpIdx), *isMeta)
	out, err := json.Marshal(map[string]string{
		"dev_name":   devName,
		"array_name": arrayName,
		"dev_path":   "/dev/md/" + devName,
	})
	if err != nil {
		die("%v", err)
	}
	fmt.Println(string(out))
}

// cmdHostId prints the deterministic NVMe host id of a hostnqn. Every
// `nvme connect` the suites issue by hand — the emulated CN and host
// identities — must pass it, for the same reason the agent does: the kernel
// allows one hostnqn per hostid, and a VM that plays several identities
// would otherwise have them all collide on the node-wide /etc/nvme/hostid
// (common.NvmeHostId).
func cmdHostId(args []string) {
	var g globals
	fs := newFlagSet("host-id", &g)
	hostNqn := fs.String("hostnqn", "", "host nqn to derive the id from")
	fs.Parse(args)

	if *hostNqn == "" {
		die("--hostnqn is required")
	}
	fmt.Println(common.NvmeHostId(*hostNqn))
}
