// Command dnagentctl is the gRPC driver of the dn-agent integration test
// (doc/dnagent_integtest.md §8). The agent serves plaintext gRPC without
// server reflection, so grpcurl cannot drive it; this binary speaks the
// generated DiskNodeAgent client instead and prints every reply as protojson
// (proto field names) on stdout for the test script to parse with jq.
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

type hexUintList []uint64

func (l *hexUintList) String() string {
	parts := make([]string, 0, len(*l))
	for _, value := range *l {
		parts = append(parts, fmt.Sprintf("%#x", value))
	}
	return strings.Join(parts, ",")
}

func (l *hexUintList) Set(s string) error {
	value, err := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return err
	}
	*l = append(*l, value)
	return nil
}

// sidePtrList collects repeated --side sp:leg:side flags into the declarative
// full list SyncupDn expects.
type sidePtrList []*pb.SidePointer

func (l *sidePtrList) String() string { return fmt.Sprintf("%d sides", len(*l)) }

func (l *sidePtrList) Set(s string) error {
	ptr, err := parseSidePointer(s)
	if err != nil {
		return err
	}
	*l = append(*l, ptr)
	return nil
}

func parseSidePointer(s string) (*pb.SidePointer, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("want sp:leg:side, got %q", s)
	}
	ids := make([]uint64, 3)
	for i, part := range parts {
		value, err := strconv.ParseUint(strings.TrimSpace(part), 0, 64)
		if err != nil {
			return nil, fmt.Errorf("%q in %q: %w", part, s, err)
		}
		ids[i] = value
	}
	return &pb.SidePointer{SpId: ids[0], LegId: ids[1], SideId: ids[2]}, nil
}

// parseTriple reads the "a:b:c" form of --migr-src / --migr-dst.
func parseTriple(s string) (uint64, uint64, uint64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("want three ':'-separated ids, got %q", s)
	}
	ids := make([]uint64, 3)
	for i, part := range parts {
		value, err := strconv.ParseUint(strings.TrimSpace(part), 0, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%q in %q: %w", part, s, err)
		}
		ids[i] = value
	}
	return ids[0], ids[1], ids[2], nil
}

var spLevels = map[string]pb.SpLevel{
	"readwrite":    pb.SpLevel_SP_LEVEL_READWRITE,
	"readonly":     pb.SpLevel_SP_LEVEL_READONLY,
	"no_clone":     pb.SpLevel_SP_LEVEL_NO_CLONE,
	"no_thinpool":  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	"no_redund":    pb.SpLevel_SP_LEVEL_NO_REDUND,
	"no_migration": pb.SpLevel_SP_LEVEL_NO_MIGRATION,
	"no_side":      pb.SpLevel_SP_LEVEL_NO_SIDE,
	"disable":      pb.SpLevel_SP_LEVEL_DISABLE,
}

func parseSpLevel(s string) (pb.SpLevel, error) {
	level, ok := spLevels[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("unknown --sp-level %q", s)
	}
	return level, nil
}

// ---------------------------------------------------------------------------
// Globals, dialing and output
// ---------------------------------------------------------------------------

type globals struct {
	addr       string
	cluster    hexUint
	dn         hexUint
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
	fs.Var(&g.dn, "dn", "disk node id")
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

func (g *globals) dial() (*grpc.ClientConn, pb.DiskNodeAgentClient, error) {
	if g.addr == "" {
		return nil, nil, fmt.Errorf("--addr is required")
	}
	conn, err := grpc.NewClient(g.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, pb.NewDiskNodeAgentClient(conn), nil
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

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dnagentctl: "+format+"\n", args...)
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
	{"get-dn-size", cmdGetDnSize},
	{"syncup-dn", cmdSyncupDn},
	{"syncup-side", cmdSyncupSide},
	{"push-migr-bm", cmdPushMigrBm},
	{"get-dn-info", cmdGetDnInfo},
	{"get-side-info", cmdGetSideInfo},
	{"check-dn", cmdCheckDn},
	{"check-side", cmdCheckSide},
	{"wait-hydrated", cmdWaitHydrated},
	{"ns-id", cmdNsId},
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
		"usage: dnagentctl <%s> [flags]\n", strings.Join(names, "|"))
	os.Exit(2)
}

func newFlagSet(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	g.bind(fs, true)
	return fs
}

// sidePointerFlags registers the --sp/--leg/--side triple of the object-scoped
// RPCs.
func sidePointerFlags(fs *flag.FlagSet) (*hexUint, *hexUint, *hexUint) {
	var sp, leg, side hexUint
	fs.Var(&sp, "sp", "storage pool id")
	fs.Var(&leg, "leg", "leg id")
	fs.Var(&side, "side", "side id")
	return &sp, &leg, &side
}

func sidePointerOf(sp, leg, side *hexUint) *pb.SidePointer {
	return &pb.SidePointer{
		SpId:   uint64(*sp),
		LegId:  uint64(*leg),
		SideId: uint64(*side),
	}
}

// cmdGetDnSize doubles as the agent liveness probe: GetDnSize takes no lock,
// so --wait retries it until the freshly started agent answers (§7 step 6).
func cmdGetDnSize(args []string) {
	var g globals
	fs := newFlagSet("get-dn-size", &g)
	wait := fs.Float64("wait", 0, "seconds to retry until the agent answers")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(time.Duration(*wait * float64(time.Second)))
	req := &pb.GetDnSizeRequest{
		ClusterId: uint64(g.cluster), DnId: uint64(g.dn)}
	for attempt := 1; ; attempt++ {
		ctx, cancel := g.rpcCtx()
		reply, err := client.GetDnSize(ctx, req)
		cancel()
		if err == nil {
			emit(reply)
			return
		}
		if time.Now().After(deadline) {
			die("GetDnSize failed after %d attempts: %v", attempt, err)
		}
		fmt.Fprintf(os.Stderr,
			"dnagentctl: waiting for %s (attempt %d): %v\n",
			g.addr, attempt, err)
		time.Sleep(500 * time.Millisecond)
	}
}

func cmdSyncupDn(args []string) {
	var g globals
	fs := newFlagSet("syncup-dn", &g)
	var revision, extentSize hexUint
	var sides sidePtrList
	fs.Var(&revision, "revision", "request revision")
	fs.Var(&extentSize, "extent-size", "DN VG physical extent size in bytes")
	fs.Var(&sides, "side",
		"sp:leg:side, repeatable — the full desired side list")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.SyncupDn(ctx, &pb.SyncupDnRequest{
		ClusterId:       uint64(g.cluster),
		DnId:            uint64(g.dn),
		Revision:        uint64(revision),
		SidePointerList: sides,
		ExtentSize:      uint64(extentSize),
	})
	if err != nil {
		die("SyncupDn failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdSyncupSide(args []string) {
	var g globals
	fs := newFlagSet("syncup-side", &g)
	sp, leg, side := sidePointerFlags(fs)
	var revision, extCnt, primaryCn hexUint
	var standbys hexUintList
	cntlidSlot := fs.Uint("cntlid-slot", 0, "cntlid slot (architecture.md §11.8)")
	spLevel := fs.String("sp-level", "readwrite", "sp level name")
	migrSrc := fs.String("migr-src", "",
		"migr_src_conf as migr:dstSide:dstDn")
	migrDst := fs.String("migr-dst", "",
		"migr_dst_conf as migr:srcSide:srcDn")
	srcTrAddr := fs.String("src-traddr", "", "migration source transport address")
	srcTrSvcId := fs.String("src-trsvcid", "", "migration source service id")
	blockSize := fs.Uint64("block-size", 0, "dm-clone region size in bytes")
	metaBlocks := fs.Uint64("meta-blocks", 0, "leading regions that always copy")
	hydrThreshold := fs.Uint("hydr-threshold", 0, "dm-clone hydration threshold")
	hydrBatch := fs.Uint("hydr-batch", 0, "dm-clone hydration batch size")
	bmCnt := fs.Uint("bm-cnt", 0, "number of bitmap chunks the CP will push")
	fs.Var(&revision, "revision", "request revision")
	fs.Var(&extCnt, "ext-cnt", "side size in DN VG extents")
	fs.Var(&primaryCn, "primary-cn", "primary CN id")
	fs.Var(&standbys, "standby-cn", "standby CN id, repeatable")
	fs.Parse(args)

	level, err := parseSpLevel(*spLevel)
	if err != nil {
		die("%v", err)
	}
	req := &pb.SyncupSideRequest{
		ClusterId:   uint64(g.cluster),
		DnId:        uint64(g.dn),
		SidePointer: sidePointerOf(sp, leg, side),
		Revision:    uint64(revision),
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        uint64(extCnt),
			CntlidSlot:    uint32(*cntlidSlot),
			PrimaryCnId:   uint64(primaryCn),
			StandbyIdList: standbys,
			SpLevel:       level,
		},
	}
	if *migrSrc != "" {
		migrId, dstSideId, dstDnId, err := parseTriple(*migrSrc)
		if err != nil {
			die("--migr-src: %v", err)
		}
		req.MigrSrcConf = &pb.SyncupSideRequest_MigrSrcConf{
			MigrId:    migrId,
			DstSideId: dstSideId,
			DstDnId:   dstDnId,
		}
	}
	if *migrDst != "" {
		migrId, srcSideId, srcDnId, err := parseTriple(*migrDst)
		if err != nil {
			die("--migr-dst: %v", err)
		}
		req.MigrDstConf = &pb.SyncupSideRequest_MigrDstConf{
			MigrId:    migrId,
			SrcSideId: srcSideId,
			SrcDnId:   srcDnId,
			// The whole harness runs nvme over tcp/ipv4 (§8).
			SrcNvmeTrConf: &pb.NvmeTrConf{
				TrType:  "tcp",
				AdrFam:  "ipv4",
				TrAddr:  *srcTrAddr,
				TrSvcId: *srcTrSvcId,
			},
			BlockSize:  *blockSize,
			MetaBlocks: *metaBlocks,
			DmCloneConf: &pb.DmCloneConf{
				HydrationThreshold: uint32(*hydrThreshold),
				HydrationBatchSize: uint32(*hydrBatch),
			},
			BmCnt: uint32(*bmCnt),
		}
	}

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.SyncupSide(ctx, req)
	if err != nil {
		die("SyncupSide failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdPushMigrBm(args []string) {
	var g globals
	fs := newFlagSet("push-migr-bm", &g)
	sp, leg, side := sidePointerFlags(fs)
	var revision, migr hexUint
	fs.Var(&revision, "revision", "the side's current revision (gates only)")
	fs.Var(&migr, "migr", "migration id")
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

	reply, err := client.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
		ClusterId:   uint64(g.cluster),
		DnId:        uint64(g.dn),
		SidePointer: sidePointerOf(sp, leg, side),
		Revision:    uint64(revision),
		MigrId:      uint64(migr),
		BmIdx:       uint32(*bmIdx),
		Bitmap:      bitmap,
	})
	if err != nil {
		die("PushMigrBitmap failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdGetDnInfo(args []string) {
	var g globals
	fs := newFlagSet("get-dn-info", &g)
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: uint64(g.cluster), DnId: uint64(g.dn)})
	if err != nil {
		die("GetDnInfo failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdGetSideInfo(args []string) {
	var g globals
	fs := newFlagSet("get-side-info", &g)
	sp, leg, side := sidePointerFlags(fs)
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()
	ctx, cancel := g.rpcCtx()
	defer cancel()

	reply, err := client.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId:   uint64(g.cluster),
		DnId:        uint64(g.dn),
		SidePointer: sidePointerOf(sp, leg, side),
	})
	if err != nil {
		die("GetSideInfo failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

// cmdCheckDn runs exactly one worker-initiated round on the bidi stream and
// closes it (SH24).
func cmdCheckDn(args []string) {
	var g globals
	fs := newFlagSet("check-dn", &g)
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

	stream, err := client.CheckDn(ctx)
	if err != nil {
		die("CheckDn failed: %v", err)
	}
	if err := stream.Send(&pb.CheckDnRequest{
		ClusterId: uint64(g.cluster),
		DnId:      uint64(g.dn),
		Revision:  uint64(revision),
		ShowInfo:  *showInfo,
	}); err != nil {
		die("CheckDn send failed: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		die("CheckDn recv failed: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		die("CheckDn close failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

func cmdCheckSide(args []string) {
	var g globals
	fs := newFlagSet("check-side", &g)
	sp, leg, side := sidePointerFlags(fs)
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

	stream, err := client.CheckSide(ctx)
	if err != nil {
		die("CheckSide failed: %v", err)
	}
	if err := stream.Send(&pb.CheckSideRequest{
		ClusterId:   uint64(g.cluster),
		DnId:        uint64(g.dn),
		SidePointer: sidePointerOf(sp, leg, side),
		Revision:    uint64(revision),
		ShowInfo:    *showInfo,
	}); err != nil {
		die("CheckSide send failed: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		die("CheckSide recv failed: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		die("CheckSide close failed: %v", err)
	}
	emit(reply)
	g.checkReply(reply.GetAgentReply())
}

// cmdWaitHydrated polls GetSideInfo until the destination dm-clone reports
// every region hydrated, parsing the raw `dmsetup status` line the agent puts
// in migr_dst_info.dm_clone_info.details (§9.5). --min-first asserts the
// bitmap jump: case C's first sample must already show >= 64/128 (§14).
func cmdWaitHydrated(args []string) {
	var g globals
	fs := flag.NewFlagSet("wait-hydrated", flag.ExitOnError)
	g.bind(fs, false)
	sp, leg, side := sidePointerFlags(fs)
	interval := fs.Float64("interval", 0.5, "seconds between samples")
	limit := fs.Float64("timeout", 120, "seconds to wait for full hydration")
	minFirst := fs.Uint64("min-first", 0,
		"the first parseable sample must show at least this many hydrated")
	fs.Parse(args)

	conn, client, err := g.dial()
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()

	req := &pb.GetSideInfoRequest{
		ClusterId:   uint64(g.cluster),
		DnId:        uint64(g.dn),
		SidePointer: sidePointerOf(sp, leg, side),
	}
	deadline := time.Now().Add(time.Duration(*limit * float64(time.Second)))
	var firstHydrated, firstTotal uint64
	haveFirst := false
	samples := 0
	for {
		ctx, cancel := g.rpcCtx()
		reply, err := client.GetSideInfo(ctx, req)
		cancel()
		if err != nil {
			die("GetSideInfo failed: %v", err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			die("agent_reply.code = %d (%s)",
				reply.GetAgentReply().GetCode(),
				reply.GetAgentReply().GetDetails())
		}
		raw := reply.GetSideInfo().GetMigrDstInfo().
			GetDmCloneInfo().GetDetails()
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
				"dnagentctl: hydrated %d/%d\n",
				status.HydratedRegions, status.TotalRegions)
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
		}
		if time.Now().After(deadline) {
			die("hydration did not finish within %gs (last status %q)",
				*limit, raw)
		}
		time.Sleep(time.Duration(*interval * float64(time.Second)))
	}
}

// cmdNsId prints the deterministic namespace identity of a leg, which is how
// the test finds the CN-side device node (/dev/disk/by-id/nvme-uuid.<uuid>).
func cmdNsId(args []string) {
	var g globals
	fs := newFlagSet("ns-id", &g)
	sp, leg, _ := sidePointerFlags(fs)
	fs.Parse(args)

	uuid, nguid := common.DnNsIdentity(
		uint64(g.cluster), uint64(*sp), uint64(*leg))
	out, err := json.Marshal(map[string]string{
		"uuid":   uuid,
		"nguid":  nguid,
		"serial": fmt.Sprintf(common.IdKeyFmt, uint64(*leg)),
		"by_id":  "/dev/disk/by-id/nvme-uuid." + uuid,
	})
	if err != nil {
		die("%v", err)
	}
	fmt.Println(string(out))
}
