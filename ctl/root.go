// Package ctl is the dnv operator CLI (dnvctl.md). It exposes exactly the 59
// RPCs of `service Gateway`, one command per RPC, grouped by noun:
//
//	dnvctl <group> <verb> [flags]
//
// This file owns everything the twelve group files share: the cobra root and
// its persistent flags, the viper binding (CT9), the dial block (CT2), the
// per-invocation trace id (CT2), the result-emit pipeline (CT4), the error
// rendering and the exit codes (CT5).
//
// Two rules shape the whole package:
//
//   - CT8 — no client-side validation. dnvctl rejects only what fails to
//     PARSE (exit 2); every parsed value is sent as typed and the gateway's
//     validation is the only validator. Empty required fields, contradictory
//     flags and unknown enum numbers are all forwarded.
//   - CT9 — every value is read back through viper, never off the flag, so a
//     DNVCTL_* environment variable or a --config file satisfies a value just
//     as a flag does. That is why the comma-split list flags below are plain
//     strings: viper's GetStringSlice does not split a single environment or
//     config string on commas (the four daemons' splitList rationale).
package ctl

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// envPrefix makes every flag settable as DNVCTL_<FLAG_WITH_UNDERSCORES>
// (CT9). Mechanically that covers every flag; only the §2.1 globals are
// documented for environment use.
const envPrefix = "DNVCTL"

// defaultTimeout is the per-invocation deadline in seconds (§2.1). It is a
// literal, not common.DefaultGatewayAgentTimeout: the two coincide at 10 but
// mean different things, and dnvctl must not inherit a change to the
// gateway's own per-agent budget.
const defaultTimeout = 10.0

// job is one RPC invocation. It returns whatever should be rendered on
// stdout: a reply message for 57 of the 59 commands, and a plain
// map[string]any for the two bitmap reads (§3.1).
type job func(ctx context.Context, client pb.GatewayClient) (any, error)

// ---------------------------------------------------------------------------
// Exit codes and error rendering (CT5, §3.2)
// ---------------------------------------------------------------------------

// rpcFailure is the exit-1 class: the RPC, or the connection carrying it,
// failed. Everything else that reaches Execute is a usage error and exits 2,
// which is what keeps "no RPC was issued" provable from the exit code alone.
type rpcFailure struct {
	err     error
	traceId string
}

func (e *rpcFailure) Error() string { return e.err.Error() }
func (e *rpcFailure) Unwrap() error { return e.err }

// codeNames spells every gRPC code the way the wire and the specs do —
// UPPER_SNAKE — because codes.Code.String() is CamelCase and operators grep
// for the wire spelling (§3.2).
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

// codeName is the UPPER_SNAKE spelling of a code, falling back to the numeric
// form for a code no version of gRPC has defined yet.
func codeName(c codes.Code) string {
	if name, ok := codeNames[c]; ok {
		return name
	}
	return fmt.Sprintf("CODE_%d", uint32(c))
}

// Execute runs the CLI and returns the process exit code (§3.2):
//
//	0  the RPC returned OK; the §3.1 document is on stdout, stderr is empty
//	1  the RPC or the connection failed; stdout is empty, stderr is one line
//	2  a usage error; stdout is empty, no RPC was issued
func Execute() int {
	root := NewRootCmd()
	err := root.Execute()
	if err == nil {
		return 0
	}
	var failure *rpcFailure
	if errors.As(err, &failure) {
		st, _ := status.FromError(failure.err)
		fmt.Fprintf(os.Stderr, "dnvctl: %s: %s (trace_id %s)\n",
			codeName(st.Code()), st.Message(), failure.traceId)
		return 1
	}
	fmt.Fprintf(os.Stderr, "dnvctl: %v\n", err)
	return 2
}

// ---------------------------------------------------------------------------
// The root command (§2.1)
// ---------------------------------------------------------------------------

// NewRootCmd builds the whole command tree. It is exported so the unit tests
// can drive the real tree through argv rather than a stand-in (CT-T2).
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dnvctl",
		Short: "dnv operator CLI",
		Long: "dnvctl issues the 59 RPCs of the dnv Gateway service, one " +
			"command per RPC. It prints one canonical JSON document per " +
			"invocation on stdout and never issues an RPC the operator did " +
			"not type.",
		// Usage text on an RPC failure would bury the one line that matters;
		// Execute prints every error itself, so cobra must print none.
		SilenceUsage:  true,
		SilenceErrors: true,
		// The root is a group like any other: `dnvctl` alone is a usage
		// error, not a success that happens to print help on the stdout
		// §3.1 reserves. See group() for why this needs a RunE.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return needsSubcommand(cmd, args)
		},
		// The binding must happen after cobra has merged the root's
		// persistent flags into the invoked leaf's flag set, which
		// ParseFlags does before PersistentPreRunE runs — so cmd.Flags()
		// here is "persistent + local", exactly what CT9 asks for.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return bindViper(cmd)
		},
	}
	addGlobalFlags(root)
	registerCluster(root)
	registerDn(root)
	registerCn(root)
	registerSp(root)
	registerCntlr(root)
	registerTd(root)
	registerSs(root)
	registerNs(root)
	registerClone(root)
	registerXfer(root)
	registerMigr(root)
	registerSpare(root)
	return root
}

// addGlobalFlags declares the §2.1 persistent flags. Only --cluster and --sp
// fill request fields; the rest steer the invocation itself.
func addGlobalFlags(root *cobra.Command) {
	flags := root.PersistentFlags()
	flags.String("gateway-address", "",
		"gateway ip:port to dial (required)")
	flags.String("cluster", "",
		"cluster_name of every request that has one")
	flags.String("sp", "",
		"sp_name of every request that has one")
	flags.String("rev", "",
		"revision token (base 0; omitted sends no token message at all)")
	flags.Float64("timeout", defaultTimeout,
		"per-invocation deadline, seconds")
	flags.String("trace-id", "",
		"override the per-invocation trace id mint")
	flags.String("config", "", "optional viper config file")
}

// bindViper wires flags, config file and environment together exactly as the
// four daemons do (CT9). It binds the INVOKED command's full flag set, so
// every leaf's local flags are bound alongside the root's persistent ones.
func bindViper(cmd *cobra.Command) error {
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		return err
	}
	viper.SetEnvPrefix(envPrefix)
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	if path := viper.GetString("config"); path != "" {
		viper.SetConfigFile(path)
		if err := viper.ReadInConfig(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Running one command (CT2, CT4, CT5)
// ---------------------------------------------------------------------------

// dialFunc is the seam the unit tests replace: production dials a real
// gateway, CT-T2's recordingClient and CT-T4's stub client do not.
type dialFunc func(ctx context.Context, address string) (
	pb.GatewayClient, func() error, error)

// dial is the production dialFunc: the grpc.md §4 mandatory client block.
// Both chains are installed even though all 59 RPCs are unary, because §4
// says both chains on every dnv connection.
func dial(_ context.Context, address string) (
	pb.GatewayClient, func() error, error,
) {
	conn, err := grpc.NewClient(
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
	)
	if err != nil {
		return nil, nil, err
	}
	return pb.NewGatewayClient(conn), conn.Close, nil
}

// dialer is package state so a test can swap the seam; production never
// touches it.
var dialer dialFunc = dial

// run is every leaf command's RunE body: resolve the globals, build the
// traced and deadlined context, dial, invoke, render.
//
// A returned *rpcFailure is exit 1; every other error is a usage error and
// exit 2. The ordering matters: everything that can fail to PARSE is done
// BEFORE the dial, so a usage error provably issues no RPC (CT-T4, §7.12 c5).
func run(cmd *cobra.Command, build func() (job, error)) error {
	address := strings.TrimSpace(viper.GetString("gateway-address"))
	if address == "" {
		return errors.New("missing required flag: --gateway-address")
	}
	j, err := build()
	if err != nil {
		return err
	}

	// The trace id: --trace-id when non-empty, else the T4 mint. The §4
	// client chain moves it into the outgoing trace_id metadata; dnvctl
	// never touches metadata itself (that shortcut belongs to the integtest
	// drivers, grpc.md §6).
	traceId := strings.TrimSpace(viper.GetString("trace-id"))
	if traceId == "" {
		traceId = common.NewTraceId()
	}
	ctx := common.WithTraceId(context.Background(), traceId)
	timeout := viper.GetFloat64("timeout")
	ctx, cancel := context.WithTimeout(
		ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()

	client, closeConn, err := dialer(ctx, address)
	if err != nil {
		return &rpcFailure{err: err, traceId: traceId}
	}
	if closeConn != nil {
		defer closeConn()
	}
	result, err := j(ctx, client)
	if err != nil {
		return &rpcFailure{err: err, traceId: traceId}
	}
	return emit(result)
}

// leaf builds one RPC command. Every one of the 59 has this shape: a Use
// line, a one-line Short, no positional arguments, and a RunE that defers to
// run.
func leaf(use, short string, build func() (job, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, build)
		},
	}
}

// group builds one noun group: a parent command that issues no RPC itself.
//
// The RunE is what makes the group a *usage error* rather than a help screen.
// cobra's (*Command).execute returns flag.ErrHelp for any command that is not
// Runnable BEFORE it calls ValidateArgs, and ExecuteC treats flag.ErrHelp as
// success — so a group without a RunE answers `dnvctl td lst` by printing its
// help to STDOUT and exiting 0. That breaks §3.2 (an unknown command must be
// exit 2 with empty stdout) and §3.1 (stdout carries the result document and
// nothing else) at once, and it tells a script that mistyped a verb that it
// succeeded. Being Runnable puts ValidateArgs back in the path, where
// cobra.NoArgs produces the `unknown command` error a typo deserves.
//
// `--help` is unaffected: cobra handles the help flag before any of this, so
// the stock help of §0 #3 still prints to stdout and exits 0.
func group(use, short string, leaves ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return needsSubcommand(cmd, args)
		},
	}
	cmd.AddCommand(leaves...)
	return cmd
}

// needsSubcommand is the error a non-leaf command answers with. Reached only
// when cobra found no subcommand to dispatch to: either a bare group, or —
// if ValidateArgs let it through — a verb that does not exist.
func needsSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown command %q for %q",
			args[0], cmd.CommandPath())
	}
	return fmt.Errorf("%q needs a subcommand; run %q to list them",
		cmd.CommandPath(), cmd.CommandPath()+" --help")
}

// ---------------------------------------------------------------------------
// Result rendering (CT4, §3.1)
// ---------------------------------------------------------------------------

// marshalOpts renders every reply. UseProtoNames keeps the JSON field names
// identical to schema.proto's spelling; EmitUnpopulated keeps a false/0/[]
// field visible, which is what the `created` poll of ThinDeviceCreated.md R13
// needs from `td list`.
var marshalOpts = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// emit prints one canonical JSON document on stdout. protojson deliberately
// varies its whitespace, so the reply is re-parsed and re-encoded through
// encoding/json: sorted keys, stable spacing, proto field names, proto3
// defaults visible, uint64 as JSON strings.
//
// This and the two bitmap reads are the only fmt.Print* in the package — the
// log.md R1 exemption for CLI results (CT4).
func emit(result any) error {
	value := result
	if msg, ok := result.(proto.Message); ok {
		raw, err := marshalOpts.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshaling the reply: %w", err)
		}
		// Into a FRESH any, never into `value`: `value` still holds the
		// concrete *pb.XxxReply pointer, and unmarshaling through that would
		// decode the protojson back into the generated struct — which
		// re-encodes through its own `json:",omitempty"` tags and silently
		// undoes both EmitUnpopulated and the uint64-as-string rendering.
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return fmt.Errorf("re-parsing the reply: %w", err)
		}
		value = decoded
	}
	out, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshaling the reply: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

// hexBitmapResult is §3.1's one deviation. protojson renders a bytes field as
// base64, which is neither what an operator wants to read nor what the suite
// asserts, so the two bitmap reads return this map instead of their reply
// message. byte_cnt travels with it so a length check needs no arithmetic on
// the hex string.
func hexBitmapResult(bitmap []byte) map[string]any {
	return map[string]any{
		"bitmap_hex": hex.EncodeToString(bitmap),
		"byte_cnt":   len(bitmap),
	}
}

// ---------------------------------------------------------------------------
// Reading values back through viper (CT9)
// ---------------------------------------------------------------------------

// strOf is one string value.
func strOf(name string) string { return viper.GetString(name) }

// boolOf is one boolean value. A boolean that must be turned off is written
// --enabled=false; --enabled false would be parsed as a positional argument
// and rejected by cobra.NoArgs.
func boolOf(name string) bool { return viper.GetBool(name) }

// u64Of is one plain uint64 (a size or a count), decimal.
func u64Of(name string) uint64 { return viper.GetUint64(name) }

// u32Of is one plain uint32 (an index).
func u32Of(name string) uint32 { return uint32(viper.GetUint64(name)) }

// hexOf is an id value: Go base-0, so 17 and 0x11 are the same number. An
// unset flag is the empty string and reads as 0; a malformed one is a usage
// error (exit 2), which is the whole of dnvctl's input checking (CT8).
func hexOf(name string) (uint64, error) {
	raw := strings.TrimSpace(viper.GetString(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s: %w", name, err)
	}
	return value, nil
}

// hex32Of is hexOf for a 32-bit field.
func hex32Of(name string) (uint32, error) {
	raw := strings.TrimSpace(viper.GetString(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s: %w", name, err)
	}
	return uint32(value), nil
}

// strListOf is a comma-split string list. It REPLACES on each occurrence
// rather than accumulating, and empty items are dropped, so `--hosts a,,b` is
// two hosts. An empty flag is an empty list, which several RPCs treat as a
// meaningful "clear it".
func strListOf(name string) []string {
	var out []string
	for _, item := range strings.Split(viper.GetString(name), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// idListOf is a comma-split base-0 numeric list (--ids).
func idListOf(name string) ([]uint64, error) {
	var out []uint64
	for _, item := range strings.Split(viper.GetString(name), ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		value, err := strconv.ParseUint(item, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid --%s: %w", name, err)
		}
		out = append(out, value)
	}
	return out, nil
}

// u32ListOf is a comma-split base-0 numeric list of 32-bit values (--slots).
func u32ListOf(name string) ([]uint32, error) {
	var out []uint32
	for _, item := range strings.Split(viper.GetString(name), ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		value, err := strconv.ParseUint(item, 0, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid --%s: %w", name, err)
		}
		out = append(out, uint32(value))
	}
	return out, nil
}

// hexBytesOf parses a lowercase-or-uppercase hex bitmap. An EMPTY value is
// not an error — it sends an empty bitmap on purpose — but a malformed
// non-empty one is a usage error (exit 2), per §5.9's note on
// `clone append-bm`.
func hexBytesOf(name string) ([]byte, error) {
	raw := strings.TrimSpace(viper.GetString(name))
	if raw == "" {
		return nil, nil
	}
	value, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid --%s: %w", name, err)
	}
	return value, nil
}

// ---------------------------------------------------------------------------
// The two scope globals (§5.0)
// ---------------------------------------------------------------------------

// clusterOf fills `cluster_name`. It is the global for 57 of the 58 requests
// that carry the field; only the `cluster` group's own commands override it,
// through clusterNameOf.
func clusterOf() string { return strOf("cluster") }

// clusterNameOf is the `cluster` group's rule: its own --name wins, and an
// empty --name falls back to the global --cluster. A `cluster` command is the
// only place the field is addressed by anything but the global.
func clusterNameOf() string {
	if name := strOf("name"); name != "" {
		return name
	}
	return clusterOf()
}

// spOf fills `sp_name` on the 41 requests that carry it, `sp create`
// included.
func spOf() string { return strOf("sp") }

// ---------------------------------------------------------------------------
// Revision tokens (CT3, §4)
// ---------------------------------------------------------------------------

// revToken reports the request's token revision and whether the operator
// asked for a token at all. Presence is what the gateway keys on (GW6 is
// presence-based), so dnvctl sends exactly what was typed:
//
//	--rev not given  => ok=false => the token field stays nil
//	--rev N          => ok=true  => the message is present with revision = N
//	--rev 0          => ok=true  => present with revision 0, which is the
//	                               deliberate always-stale probe: a stored
//	                               revision starts at 1 and only grows
//
// The flag is a string rather than a numeric type precisely so that "not
// given" survives the trip through viper — a numeric flag's zero value would
// be indistinguishable from an explicit 0, and those two mean opposite things.
func revToken() (uint64, bool, error) {
	raw := strings.TrimSpace(viper.GetString("rev"))
	if raw == "" {
		return 0, false, nil
	}
	value, err := strconv.ParseUint(raw, 0, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid --rev: %w", err)
	}
	return value, true, nil
}

// spRev is the SpRev token of the 30 SP-scoped mutators, or nil when none was
// typed. Only `revision` participates in the gateway's check, so the echoed
// sp_name is deliberately left unset.
func spRev() (*pb.SpRev, error) {
	value, ok, err := revToken()
	if err != nil || !ok {
		return nil, err
	}
	return &pb.SpRev{Revision: value}, nil
}

// dnRev is the DnRev token of DeleteDiskNode and UpdateDiskNodeDisabled.
func dnRev() (*pb.DnRev, error) {
	value, ok, err := revToken()
	if err != nil || !ok {
		return nil, err
	}
	return &pb.DnRev{Revision: value}, nil
}

// cnRev is the CnRev token of the two CN mirrors.
func cnRev() (*pb.CnRev, error) {
	value, ok, err := revToken()
	if err != nil || !ok {
		return nil, err
	}
	return &pb.CnRev{Revision: value}, nil
}

// ---------------------------------------------------------------------------
// Shared flag helpers (§5.0)
// ---------------------------------------------------------------------------

// pageFlags adds the two flags every paged List* takes. A count of 0 asks for
// the server's default page size; dnvctl does not substitute one (CT8).
//
// --count is a Uint32 because `count` is uint32 on all four List* requests.
// Declaring it wider and narrowing on read would turn an out-of-range value
// into a silent wrap to 0 — i.e. into "use the server default", the opposite
// of what was asked — whereas pflag rejects it outright as the parse failure
// it is (exit 2, no RPC issued), which is the one rejection CT8 allows.
func pageFlags(flags *pflag.FlagSet) {
	flags.Uint32("count", 0, "page size (0 = the server default)")
	flags.String("page-token", "", "page token from a previous reply")
}

// trConfFlags adds the four NvmeTrConf flags under one prefix, so a command
// with two transport configs can carry both without a name clash. The
// defaults are the lab-shaped tcp/ipv4/127.0.0.1/4420; all four EMPTY yields
// a nil conf, which is how a command asks for "not given".
func trConfFlags(flags *pflag.FlagSet, prefix string) {
	flags.String(prefix+"tr-type", "tcp", "NvmeTrConf.tr_type")
	flags.String(prefix+"adr-fam", "ipv4", "NvmeTrConf.adr_fam")
	flags.String(prefix+"tr-addr", "127.0.0.1", "NvmeTrConf.tr_addr")
	flags.String(prefix+"tr-svc-id", "4420", "NvmeTrConf.tr_svc_id")
}

// trConfOf builds the NvmeTrConf those four flags describe, or nil when every
// one of them is empty.
func trConfOf(prefix string) *pb.NvmeTrConf {
	trType := strOf(prefix + "tr-type")
	adrFam := strOf(prefix + "adr-fam")
	trAddr := strOf(prefix + "tr-addr")
	trSvcId := strOf(prefix + "tr-svc-id")
	if trType == "" && adrFam == "" && trAddr == "" && trSvcId == "" {
		return nil
	}
	return &pb.NvmeTrConf{
		TrType:  trType,
		AdrFam:  adrFam,
		TrAddr:  trAddr,
		TrSvcId: trSvcId,
	}
}

// selectorFlags adds a NodeSelector's two list flags under one prefix.
func selectorFlags(flags *pflag.FlagSet, prefix string) {
	flags.String(prefix+"-black", "",
		"comma-separated NodeSelector.black_list")
	flags.String(prefix+"-white", "",
		"comma-separated NodeSelector.white_list")
}

// selectorOf builds the NodeSelector, or nil when both lists are empty.
func selectorOf(prefix string) *pb.NodeSelector {
	black := strListOf(prefix + "-black")
	white := strListOf(prefix + "-white")
	if len(black) == 0 && len(white) == 0 {
		return nil
	}
	return &pb.NodeSelector{BlackList: black, WhiteList: white}
}

// dmCloneConfFlags adds the two dm-clone tuning flags. Both zero yields a nil
// conf — the GW11 "not given" convention, which lets the gateway apply its
// own defaults.
func dmCloneConfFlags(flags *pflag.FlagSet) {
	flags.Uint32("hyd-threshold", 0,
		"DmCloneConf.hydration_threshold (0 = not given)")
	flags.Uint32("hyd-batch", 0,
		"DmCloneConf.hydration_batch_size (0 = not given)")
}

// dmCloneConfOf builds the DmCloneConf, or nil when both values are zero.
func dmCloneConfOf() *pb.DmCloneConf {
	threshold := u32Of("hyd-threshold")
	batch := u32Of("hyd-batch")
	if threshold == 0 && batch == 0 {
		return nil
	}
	return &pb.DmCloneConf{
		HydrationThreshold: threshold,
		HydrationBatchSize: batch,
	}
}
