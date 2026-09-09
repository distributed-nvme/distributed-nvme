// Command gatewayctl is the gRPC driver of the dnv-gateway integration test
// (gateway.md §10). The gateway serves plaintext gRPC without server
// reflection, so grpcurl cannot drive it; this binary speaks the generated
// pb.GatewayClient instead and prints every reply as protojson (proto field
// names) on stdout for the test script to parse with jq.
//
// It runs ON THE TEST SERVER, where etcd, the three gateways and the seven
// fake agents all listen on the loopback, and is invoked over ssh by
// integtest/gateway_test.sh (§10.3, §10.8).
//
// Conventions the script relies on:
//
//   - Global flags may be given BEFORE the subcommand (the §10.8 `gw` wrapper
//     does exactly that) or after it; the later occurrence wins.
//   - stdout carries exactly one JSON document per invocation: the reply for
//     an expected success, `{"code":…,"message":…}` for an EXPECTED failure,
//     and for `race` one JSON array ordered by input index.
//   - The slog records go to STDERR, so they never interleave with the JSON
//     the script pipes into jq.
//   - protojson is emitted with UseProtoNames and EmitUnpopulated and then
//     re-encoded through encoding/json, so field names match schema.proto,
//     proto3 defaults stay visible, uint64 fields are JSON strings and the
//     whitespace is stable enough to diff.
//   - Ids accept decimal or 0x hex everywhere.
//   - `--expect <CODE>` (default OK) is the assertion: the process exits 0
//     when the call returned exactly that gRPC code and 1 otherwise, so the
//     caller's `set -e` catches a wrong success and a wrong failure alike.
//
// Exit codes: 0 when the outcome matched --expect, 1 when it did not (or on
// any other failure), 2 on a usage error.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// defaultGateway is gw0 of §10.3: the three gateway instances listen on
	// 29810..29812 of the test server's loopback, and gatewayctl runs there.
	defaultGateway = "127.0.0.1:29810"
	// defaultCluster is the suite's cluster name (§10.5). Every subcommand
	// puts it into the request's cluster_name, so the script never repeats it.
	defaultCluster = "itgw"
)

// ---------------------------------------------------------------------------
// Output and failure
// ---------------------------------------------------------------------------

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gatewayctl: "+format+"\n", args...)
	os.Exit(1)
}

func usageDie(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gatewayctl: "+format+"\n", args...)
	os.Exit(2)
}

// marshalOpts renders every reply this driver prints. UseProtoNames keeps the
// JSON field names identical to the schema.proto spelling the assertions
// quote; EmitUnpopulated keeps a false/0/[] field visible, which is what the
// §10.11 checks on `created`, `provisioned` and `primary` need.
var marshalOpts = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// pbToAny renders one message as a generic JSON value, so that a reply can be
// embedded in a composite document (the `race` array) without being escaped
// into a string.
func pbToAny(msg proto.Message) any {
	raw, err := marshalOpts.Marshal(msg)
	if err != nil {
		die("marshaling the reply failed: %v", err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		die("re-parsing the reply failed: %v", err)
	}
	return value
}

// emit prints one JSON document. protojson deliberately varies its
// whitespace, so everything goes through encoding/json (sorted keys, stable
// spacing) to stay diffable.
func emit(value any) {
	out, err := json.Marshal(value)
	if err != nil {
		die("marshaling the reply failed: %v", err)
	}
	fmt.Println(string(out))
}

// ---------------------------------------------------------------------------
// Flag value types (§10.8: ids accept decimal or 0x hex)
// ---------------------------------------------------------------------------

// hexUint is an id flag parsed with base 0, so 17 and 0x11 are the same value.
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

// stringList is a comma-separated list flag. It REPLACES rather than appends
// on every Set, so a value from the race job's params overrides a default the
// same way a repeated command-line flag would.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	*l = out
	return nil
}

// u32List is stringList for the cntlid slot lists, parsed with base 0.
type u32List []uint32

func (l *u32List) String() string {
	parts := make([]string, 0, len(*l))
	for _, value := range *l {
		parts = append(parts, strconv.FormatUint(uint64(value), 10))
	}
	return strings.Join(parts, ",")
}

func (l *u32List) Set(s string) error {
	var out []uint32
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		value, err := strconv.ParseUint(item, 0, 32)
		if err != nil {
			return err
		}
		out = append(out, uint32(value))
	}
	*l = out
	return nil
}

// idList is u32List for the uint64 id lists (find-sp-names --ids).
type idList []uint64

func (l *idList) String() string {
	parts := make([]string, 0, len(*l))
	for _, value := range *l {
		parts = append(parts, fmt.Sprintf("%#x", value))
	}
	return strings.Join(parts, ",")
}

func (l *idList) Set(s string) error {
	var out []uint64
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		value, err := strconv.ParseUint(item, 0, 64)
		if err != nil {
			return err
		}
		out = append(out, value)
	}
	*l = out
	return nil
}

// parseHexBitmap turns a --bm-hex value into bytes. The bitmaps this suite
// pushes are tiny and opaque (GW14), so hex is the whole encoding.
func parseHexBitmap(spec string) ([]byte, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(spec), "0x")
	if clean == "" {
		return nil, fmt.Errorf("empty bitmap")
	}
	return hex.DecodeString(clean)
}

// trConfFlags registers one NvmeTrConf as four flags under a common prefix and
// returns the accessor that builds the message. An entirely empty set yields
// nil, so a subcommand can tell "not given" from "given empty".
func trConfFlags(
	fs *flag.FlagSet,
	prefix string,
	defTrType string,
	defAdrFam string,
	defTrAddr string,
	defTrSvcId string,
) func() *pb.NvmeTrConf {
	trType := fs.String(prefix+"tr-type", defTrType, "nvme transport type")
	adrFam := fs.String(prefix+"adr-fam", defAdrFam, "nvme address family")
	trAddr := fs.String(prefix+"tr-addr", defTrAddr, "nvme transport address")
	svcId := fs.String(prefix+"tr-svc-id", defTrSvcId, "nvme service id")
	return func() *pb.NvmeTrConf {
		if *trType == "" && *adrFam == "" && *trAddr == "" && *svcId == "" {
			return nil
		}
		return &pb.NvmeTrConf{
			TrType:  *trType,
			AdrFam:  *adrFam,
			TrAddr:  *trAddr,
			TrSvcId: *svcId,
		}
	}
}

// spLevels maps the --level spellings the script uses onto the enum. Both the
// bare name and the full proto name are accepted, case-insensitively.
var spLevels = map[string]pb.SpLevel{
	"READWRITE":    pb.SpLevel_SP_LEVEL_READWRITE,
	"READONLY":     pb.SpLevel_SP_LEVEL_READONLY,
	"NO_CLONE":     pb.SpLevel_SP_LEVEL_NO_CLONE,
	"NO_THINPOOL":  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	"NO_REDUND":    pb.SpLevel_SP_LEVEL_NO_REDUND,
	"NO_MIGRATION": pb.SpLevel_SP_LEVEL_NO_MIGRATION,
	"NO_SIDE":      pb.SpLevel_SP_LEVEL_NO_SIDE,
	"DISABLE":      pb.SpLevel_SP_LEVEL_DISABLE,
}

// parseSpLevel accepts a level name, a full enum name or a raw number, so the
// script can also send a level the enum does not declare and watch the
// gateway refuse it (§10.14 step 1).
func parseSpLevel(spec string) (pb.SpLevel, error) {
	key := strings.ToUpper(strings.TrimSpace(spec))
	key = strings.TrimPrefix(key, "SP_LEVEL_")
	if level, ok := spLevels[key]; ok {
		return level, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(spec), 0, 32)
	if err != nil {
		return 0, fmt.Errorf("unknown sp_level %q", spec)
	}
	return pb.SpLevel(value), nil
}

// ---------------------------------------------------------------------------
// Globals (§10.8)
// ---------------------------------------------------------------------------

type globals struct {
	gateway string
	cluster string
	traceId string
	timeout float64
	expect  string
	// retryBudget is only read by `race`; it lives here so the flag can be
	// given before the subcommand like every other global.
	retryBudget int
}

func newGlobals() globals {
	return globals{
		gateway:     defaultGateway,
		cluster:     defaultCluster,
		timeout:     10,
		expect:      "OK",
		retryBudget: 20,
	}
}

// bind registers the global flags. It is called twice — once on the top-level
// set, once on the subcommand's — with the current values as defaults, so that
// `gatewayctl --gateway h:p create-cluster` and
// `gatewayctl create-cluster --gateway h:p` are both accepted and the later
// occurrence wins.
func (g *globals) bind(fs *flag.FlagSet) {
	fs.StringVar(&g.gateway, "gateway", g.gateway,
		"gateway gRPC endpoint host:port")
	fs.StringVar(&g.cluster, "cluster", g.cluster,
		"cluster_name put into every request")
	fs.StringVar(&g.traceId, "trace-id", g.traceId,
		"value of the trace_id gRPC metadata key")
	fs.Float64Var(&g.timeout, "timeout", g.timeout,
		"per-RPC timeout in seconds")
	fs.StringVar(&g.expect, "expect", g.expect,
		"the UPPER_SNAKE gRPC code the call must return")
	fs.IntVar(&g.retryBudget, "retry-budget", g.retryBudget,
		"race: how often a stale-token job re-fetches its token and retries")
}

// dial opens one connection to a gateway. The interceptors are deliberately
// NOT installed: this is a test driver, not a dnv component, and its own
// request/reply logging would only duplicate what the gateway already logs.
// The trace id travels as plain outgoing metadata instead (rpcCtx).
func dial(addr string) (*grpc.ClientConn, pb.GatewayClient, error) {
	if addr == "" {
		return nil, nil, fmt.Errorf("--gateway is required")
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, pb.NewGatewayClient(conn), nil
}

// rpcCtx carries the trace id as gRPC metadata so the driver's call, the
// gateway's handler, its etcd writes and any agent it reaches all share one id
// in the logs — the T3 chain the suite asserts in case S.
func (g *globals) rpcCtx() (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if g.traceId != "" {
		ctx = metadata.AppendToOutgoingContext(
			ctx, common.TraceIdMetadataKey, g.traceId)
	}
	return context.WithTimeout(
		ctx, time.Duration(g.timeout*float64(time.Second)))
}

// codeNames spells every gRPC status code the way --expect and the §10.13
// jq counts do: UPPER_SNAKE, the canonical proto enum spelling.
//
// codes.Code.String() renders CamelCase ("AlreadyExists"), so upper-casing it
// yields ALREADYEXISTS and no expectation on a multi-word code would ever
// match. The table is explicit for exactly that reason.
var codeNames = map[codes.Code]string{
	codes.OK:                 "OK",
	codes.Canceled:           "CANCELLED",
	codes.Unknown:            "UNKNOWN",
	codes.InvalidArgument:    "INVALID_ARGUMENT",
	codes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
	codes.NotFound:           "NOT_FOUND",
	codes.AlreadyExists:      "ALREADY_EXISTS",
	codes.PermissionDenied:   "PERMISSION_DENIED",
	codes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
	codes.FailedPrecondition: "FAILED_PRECONDITION",
	codes.Aborted:            "ABORTED",
	codes.OutOfRange:         "OUT_OF_RANGE",
	codes.Unimplemented:      "UNIMPLEMENTED",
	codes.Internal:           "INTERNAL",
	codes.Unavailable:        "UNAVAILABLE",
	codes.DataLoss:           "DATA_LOSS",
	codes.Unauthenticated:    "UNAUTHENTICATED",
}

// codeName renders a gRPC status code the way --expect spells it. An unknown
// numeric code falls back to its number, so a surprise is visible rather than
// silently equal to something else.
func codeName(code codes.Code) string {
	if name, ok := codeNames[code]; ok {
		return name
	}
	return fmt.Sprintf("CODE_%d", uint32(code))
}

// ---------------------------------------------------------------------------
// The subcommand contract
// ---------------------------------------------------------------------------

// job is one prepared RPC: the flags have been parsed and the request is
// whatever the closure builds from them. Splitting a subcommand into
// "register flags" and "run" is what lets `race` reuse the exact same request
// construction as the command line, with the flags fed from a JSON object
// instead of argv.
// The result is normally the reply message; a subcommand whose reply needs
// reshaping for the script — the two bitmap reads, which print hex rather
// than protojson's base64 — returns a plain map instead (resultToAny).
type job func(
	ctx context.Context,
	client pb.GatewayClient,
	g *globals,
) (any, error)

// resultToAny renders whatever a job returned as a JSON value.
func resultToAny(value any) any {
	if msg, ok := value.(proto.Message); ok {
		return pbToAny(msg)
	}
	return value
}

// command is one subcommand. setup registers the subcommand's own flags on fs
// and returns the job that reads them; it MUST NOT read a flag value itself,
// because Parse has not run yet when it is called.
type command struct {
	name  string
	setup func(fs *flag.FlagSet) job
}

// commands is the §10.8 table: one subcommand per RPC, kebab-cased, plus the
// two harness commands. The order is the order §10.8 lists them, so the usage
// line reads like the table.
var commands = []command{
	// cluster
	{"create-cluster", setupCreateCluster},
	{"delete-cluster", setupDeleteCluster},
	{"get-cluster", setupGetCluster},
	{"list-clusters", setupListClusters},
	// dn
	{"create-dn", setupCreateDn},
	{"delete-dn", setupDeleteDn},
	{"get-dn", setupGetDn},
	{"list-dns", setupListDns},
	{"set-dn-disabled", setupSetDnDisabled},
	{"inspect-dn", setupInspectDn},
	// cn
	{"create-cn", setupCreateCn},
	{"delete-cn", setupDeleteCn},
	{"get-cn", setupGetCn},
	{"list-cns", setupListCns},
	{"set-cn-disabled", setupSetCnDisabled},
	{"inspect-cn", setupInspectCn},
	// sp
	{"create-sp", setupCreateSp},
	{"delete-sp", setupDeleteSp},
	{"get-sp", setupGetSp},
	{"list-sps", setupListSps},
	{"set-cntlid-slots", setupSetCntlidSlots},
	{"set-sp-level", setupSetSpLevel},
	{"find-sp-names", setupFindSpNames},
	{"grow-slice", setupGrowSlice},
	// cntlr
	{"create-cntlr", setupCreateCntlr},
	{"delete-cntlr", setupDeleteCntlr},
	{"set-cntlr-enabled", setupSetCntlrEnabled},
	{"inspect-cntlr", setupInspectCntlr},
	{"inspect-side", setupInspectSide},
	// td
	{"create-td", setupCreateTd},
	{"delete-td", setupDeleteTd},
	{"list-tds", setupListTds},
	// ss / ns
	{"create-ss", setupCreateSs},
	{"delete-ss", setupDeleteSs},
	{"list-sss", setupListSss},
	{"set-ss-hosts", setupSetSsHosts},
	{"create-ns", setupCreateNs},
	{"delete-ns", setupDeleteNs},
	{"set-ns-dev", setupSetNsDev},
	{"set-ns-suspended", setupSetNsSuspended},
	// clone
	{"create-clone", setupCreateClone},
	{"delete-clone", setupDeleteClone},
	{"get-clone", setupGetClone},
	{"set-clone-tr", setupSetCloneTr},
	{"append-clone-bm", setupAppendCloneBm},
	// xfer
	{"create-xfer", setupCreateXfer},
	{"delete-xfer", setupDeleteXfer},
	{"get-xfer", setupGetXfer},
	{"set-xfer-hosts", setupSetXferHosts},
	// migr
	{"create-migr", setupCreateMigr},
	{"finish-migr", setupFinishMigr},
	{"cancel-migr", setupCancelMigr},
	{"get-migr", setupGetMigr},
	{"append-migr-bm", setupAppendMigrBm},
	// spare
	{"create-spare", setupCreateSpare},
	{"delete-spare", setupDeleteSpare},
	{"switch-spare", setupSwitchSpare},
	// bitmap
	{"get-td-bm", setupGetTdBm},
	{"get-leg-bm", setupGetLegBm},
	// harness
	{"ping", setupPing},
}

// lookup finds a subcommand by name.
func lookup(name string) (command, bool) {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

func main() {
	// The slog records belong on stderr: stdout is one JSON document per
	// invocation, which the script pipes into jq.
	slog.SetDefault(slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(os.Stderr, nil),
	}))
	g := newGlobals()
	top := flag.NewFlagSet("gatewayctl", flag.ExitOnError)
	g.bind(top)
	top.Usage = usage
	if err := top.Parse(os.Args[1:]); err != nil {
		usageDie("%v", err)
	}
	args := top.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	if args[0] == "race" {
		runRace(&g, args[1:])
		return
	}
	cmd, ok := lookup(args[0])
	if !ok {
		usageDie("unknown subcommand %q", args[0])
	}
	runOne(&g, cmd, args[1:])
}

func usage() {
	names := make([]string, 0, len(commands)+1)
	for _, cmd := range commands {
		names = append(names, cmd.name)
	}
	names = append(names, "race")
	fmt.Fprintf(os.Stderr,
		"usage: gatewayctl [global flags] <subcommand> [flags]\n"+
			"global flags: --gateway --cluster --trace-id --timeout "+
			"--expect --retry-budget\n"+
			"subcommands: %s\n", strings.Join(names, " "))
}

// runOne is the command-line path: parse, dial, call once, and turn the
// outcome into the exit code --expect asks for.
//
// An expected non-OK code prints `{"code":…,"message":…}` and exits 0, so the
// script asserts a refusal exactly the way it asserts a success — with `set -e`
// and one jq read — and a call that unexpectedly SUCCEEDS fails the stage just
// as loudly as one that unexpectedly fails.
func runOne(g *globals, cmd command, args []string) {
	fs := flag.NewFlagSet(cmd.name, flag.ExitOnError)
	g.bind(fs)
	run := cmd.setup(fs)
	if err := fs.Parse(args); err != nil {
		usageDie("%v", err)
	}
	conn, client, err := dial(g.gateway)
	if err != nil {
		die("%v", err)
	}
	defer conn.Close()

	ctx, cancel := g.rpcCtx()
	defer cancel()
	reply, err := run(ctx, client, g)
	got := codeName(status.Code(err))
	want := strings.ToUpper(strings.TrimSpace(g.expect))
	if err != nil {
		emit(map[string]any{
			"code":    got,
			"message": status.Convert(err).Message(),
		})
		if got != want {
			fmt.Fprintf(os.Stderr,
				"gatewayctl: %s returned %s, want %s: %v\n",
				cmd.name, got, want, err)
			os.Exit(1)
		}
		return
	}
	emit(resultToAny(reply))
	if want != codeName(codes.OK) {
		fmt.Fprintf(os.Stderr,
			"gatewayctl: %s succeeded, want %s\n", cmd.name, want)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// race — the barrier runner of §10.8
// ---------------------------------------------------------------------------

// raceJob is one line of `race`'s stdin.
type raceJob struct {
	Op         string         `json:"op"`
	Params     map[string]any `json:"params"`
	Gateway    string         `json:"gateway,omitempty"`
	RetryStale bool           `json:"retry_stale,omitempty"`
	Expect     string         `json:"expect,omitempty"`
}

// raceResult is one element of race's output array.
type raceResult struct {
	Idx     int    `json:"idx"`
	Op      string `json:"op"`
	Code    string `json:"code"`
	Reply   any    `json:"reply,omitempty"`
	Message string `json:"message,omitempty"`
	Tries   int    `json:"tries,omitempty"`
}

// paramArgs turns one job's params object into the argv the subcommand's flag
// set already understands, so a race job and a command line build the SAME
// request through the SAME code.
//
// A bool selects the `--flag` / `--flag=false` spellings the flag package
// gives a boolean flag; every other value becomes `--flag=<value>`, with an
// array joined by commas because every list flag of this driver is
// comma-separated. Keys are sorted so an argv is reproducible.
func paramArgs(params map[string]any) ([]string, error) {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys))
	for _, key := range keys {
		switch value := params[key].(type) {
		case bool:
			args = append(args, fmt.Sprintf("--%s=%t", key, value))
		case string:
			args = append(args, fmt.Sprintf("--%s=%s", key, value))
		case float64:
			// encoding/json decodes every number as float64; the ids and
			// sizes this driver takes are integers, so print them as such
			// rather than in scientific notation.
			args = append(args, fmt.Sprintf("--%s=%s", key,
				strconv.FormatFloat(value, 'f', -1, 64)))
		case []any:
			parts := make([]string, 0, len(value))
			for _, item := range value {
				switch item := item.(type) {
				case string:
					parts = append(parts, item)
				case float64:
					parts = append(parts,
						strconv.FormatFloat(item, 'f', -1, 64))
				default:
					return nil, fmt.Errorf(
						"param %q: unsupported list element %T", key, item)
				}
			}
			args = append(args, fmt.Sprintf("--%s=%s", key,
				strings.Join(parts, ",")))
		case nil:
			// An explicit null means "leave the flag at its default".
		default:
			return nil, fmt.Errorf("param %q: unsupported type %T", key, value)
		}
	}
	return args, nil
}

// prepared is one race job with its flags parsed, its connection open and its
// closure ready — everything that can be done before the barrier is done
// before the barrier, so the release is as close to simultaneous as one
// process can make it.
type prepared struct {
	idx     int
	spec    raceJob
	globals globals
	// fs is kept so the stale-token retry can rewrite --rev in place: the
	// closure captured pointers into this very set, so a Set is all the next
	// attempt needs.
	fs     *flag.FlagSet
	run    job
	client pb.GatewayClient
	conn   *grpc.ClientConn
}

// runRace reads jobs as JSON lines on stdin, prepares every one of them, then
// releases them all at once against a WaitGroup barrier (§10.8).
//
// --targets round-robins the jobs that carry no explicit gateway across the
// instances, which is what makes case A's waves cross all three gateways and
// case B's races contend across them.
//
// It exits 0 whenever every job EXECUTED, whatever code it got: the codes are
// the measurement, and the script counts them with jq and asserts the etcd
// outcome separately. A non-zero exit means the harness itself failed.
func runRace(g *globals, args []string) {
	fs := flag.NewFlagSet("race", flag.ExitOnError)
	g.bind(fs)
	var targets stringList
	fs.Var(&targets, "targets",
		"comma-separated gateways to round-robin jobs over")
	if err := fs.Parse(args); err != nil {
		usageDie("%v", err)
	}
	if len(targets) == 0 {
		targets = stringList{g.gateway}
	}

	var specs []raceJob
	dec := json.NewDecoder(os.Stdin)
	for {
		var spec raceJob
		if err := dec.Decode(&spec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			die("reading jobs: %v", err)
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		die("no jobs on stdin")
	}

	jobs := make([]*prepared, 0, len(specs))
	for idx, spec := range specs {
		cmd, ok := lookup(spec.Op)
		if !ok {
			die("job %d: unknown op %q", idx, spec.Op)
		}
		// Each job gets its own copy of the globals, so a per-job gateway or
		// expectation never leaks into its neighbours.
		jobGlobals := *g
		jobGlobals.gateway = spec.Gateway
		if jobGlobals.gateway == "" {
			jobGlobals.gateway = targets[idx%len(targets)]
		}
		if spec.Expect != "" {
			jobGlobals.expect = spec.Expect
		}
		jobFs := flag.NewFlagSet(spec.Op, flag.ContinueOnError)
		jobFs.SetOutput(os.Stderr)
		jobGlobals.bind(jobFs)
		run := cmd.setup(jobFs)
		argv, err := paramArgs(spec.Params)
		if err != nil {
			die("job %d (%s): %v", idx, spec.Op, err)
		}
		if err := jobFs.Parse(argv); err != nil {
			die("job %d (%s): %v", idx, spec.Op, err)
		}
		conn, client, err := dial(jobGlobals.gateway)
		if err != nil {
			die("job %d (%s): %v", idx, spec.Op, err)
		}
		jobs = append(jobs, &prepared{
			idx:     idx,
			spec:    spec,
			globals: jobGlobals,
			fs:      jobFs,
			run:     run,
			client:  client,
			conn:    conn,
		})
	}
	defer func() {
		for _, job := range jobs {
			job.conn.Close()
		}
	}()

	results := make([]raceResult, len(jobs))
	var release sync.WaitGroup
	var done sync.WaitGroup
	release.Add(1)
	for _, job := range jobs {
		done.Add(1)
		go func(job *prepared) {
			defer done.Done()
			release.Wait()
			results[job.idx] = job.execute()
		}(job)
	}
	// Everything above is preparation; this is the start gun.
	release.Done()
	done.Wait()

	out := make([]any, 0, len(results))
	for _, result := range results {
		out = append(out, result)
	}
	emit(out)
}

// execute runs one prepared job, applying the documented client retry protocol
// when the job asked for it (§10.8 `retry_stale`).
//
// A job that fails ABORTED "stale revision" re-fetches its SP's token through
// GetStoragePool, rewrites its own --rev flag and tries again, at most
// --retry-budget times. That is exactly what a real client does, so case B
// step 6 exercises the protocol end to end rather than asserting it in prose.
func (p *prepared) execute() raceResult {
	result := raceResult{Idx: p.idx, Op: p.spec.Op}
	budget := 1
	if p.spec.RetryStale {
		budget = p.globals.retryBudget + 1
	}
	for try := 1; try <= budget; try++ {
		ctx, cancel := p.globals.rpcCtx()
		reply, err := p.run(ctx, p.client, &p.globals)
		cancel()
		result.Tries = try
		if err == nil {
			result.Code = codeName(codes.OK)
			result.Reply = resultToAny(reply)
			return result
		}
		st := status.Convert(err)
		result.Code = codeName(st.Code())
		result.Message = st.Message()
		if try == budget || !isStaleRevision(st) {
			return result
		}
		if err := p.refreshToken(); err != nil {
			result.Message = fmt.Sprintf(
				"%s; refreshing the token failed: %v", st.Message(), err)
			return result
		}
	}
	return result
}

// isStaleRevision recognises the one refusal the retry protocol reacts to
// (GW6, §0 #7): ABORTED with the message "stale revision".
func isStaleRevision(st *status.Status) bool {
	return st.Code() == codes.Aborted &&
		strings.Contains(st.Message(), "stale revision")
}

// refreshToken re-reads the SP's current revision and rewrites the job's
// --rev flag with it. The flag set is the same one the job's closure captured
// pointers into, so the next attempt sends the new token without rebuilding
// anything.
func (p *prepared) refreshToken() error {
	spName, ok := p.spec.Params["sp"].(string)
	if !ok || spName == "" {
		return fmt.Errorf("job has no --sp to refresh a token for")
	}
	ctx, cancel := p.globals.rpcCtx()
	defer cancel()
	reply, err := p.client.GetStoragePool(ctx, &pb.GetStoragePoolRequest{
		ClusterName: p.globals.cluster,
		SpName:      spName,
	})
	if err != nil {
		return err
	}
	rev := reply.GetSpRev().GetRevision()
	if err := p.fs.Set("rev", strconv.FormatUint(rev, 10)); err != nil {
		return fmt.Errorf("rewriting --rev: %w", err)
	}
	return nil
}
