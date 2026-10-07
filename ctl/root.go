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
//     PARSE (exit 2), which for --timeout, a deadline rather than a request
//     value, includes NaN and ±Inf (timeoutOf), plus a --rev typed on a
//     command whose request has no token field to put it in (run, CT3);
//     every parsed value is sent as typed and the gateway's validation is
//     the only validator. Empty required fields, contradictory flags and
//     unknown enum numbers are all forwarded.
//   - CT9 — only the env-backed global flags (every one but --rev) have a
//     second carrier: they are bound into viper, so a DNVCTL_* environment
//     variable or a --config file supplies one just as the flag does. --rev
//     and every leaf flag are read off the parsed command line and nothing
//     else (CT9): a token, a force or a name is typed per command, and a
//     leaf value is parsed before the dial — by pflag, or by the readers
//     below and spParseLevel/spRedundConf for the flags pflag holds as
//     strings — so text that does not fit is exit 2, never a cast that reads
//     back as zero. The comma-split list flags below stay plain strings for
//     the replace-on-each-occurrence rule (dnvctl.md, Conventions), which
//     pflag's own slice flags do not follow (they append).
package ctl

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// envPrefix names the environment carrier of an env-backed global flag,
// DNVCTL_<FLAG_WITH_UNDERSCORES> (CT9). No other flag has one.
const envPrefix = "DNVCTL"

// defaultTimeout is the per-invocation deadline in seconds (dnvctl.md,
// Global flags, env, config). It is a literal, not
// common.DefaultGatewayAgentTimeout: the two coincide at 10 but mean
// different things, and dnvctl must not inherit a change to the gateway's
// own per-agent budget.
const defaultTimeout = 10.0

// job is one RPC invocation. It returns whatever should be rendered on
// stdout: a reply message for 57 of the 59 commands, and a plain
// map[string]any for the two bitmap reads (CT4).
type job func(ctx context.Context, client pb.GatewayClient) (any, error)

// ---------------------------------------------------------------------------
// Exit codes and error rendering (CT5)
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
// for the wire spelling (CT5).
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

// Execute runs the CLI and returns the process exit code (CT5):
//
//	0  the RPC returned OK; the CT4 document is on stdout, stderr is empty
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
// The root command (dnvctl.md, Global flags, env, config)
// ---------------------------------------------------------------------------

// NewRootCmd builds the whole command tree. It is exported so the unit tests
// can drive the real tree through argv rather than a stand-in (the argv →
// request tests of dnvctl.md, Conventions).
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
		// CT4 reserves. See group() for why this needs a RunE.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return needsSubcommand(cmd, args)
		},
		// The binding must happen after cobra has merged the root's
		// persistent flags into the invoked leaf's flag set, which
		// ParseFlags does before PersistentPreRunE runs — so cmd.Flags()
		// here holds the global flags bindViper looks up.
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

// addGlobalFlags declares the persistent flags, the global flags of
// dnvctl.md, Global flags, env, config. Only --cluster, --sp and --rev fill
// request fields; the rest steer the invocation itself.
func addGlobalFlags(root *cobra.Command) {
	flags := root.PersistentFlags()
	flags.String("gateway-address", "",
		"gateway ip:port to dial (required)")
	flags.String("cluster", "",
		"cluster_name of every request that has one")
	flags.String("sp", "",
		"sp_name of every request that has one")
	flags.String("rev", "",
		"revision token (base 0; command line only; omitted sends no "+
			"token message at all; a usage error on a command whose "+
			"request carries no token)")
	flags.Float64("timeout", defaultTimeout,
		"per-invocation deadline, seconds")
	flags.String("trace-id", "",
		"override the per-invocation trace id mint")
	flags.String("config", "",
		"optional viper config file for the env-backed global flags")
}

// envGlobals are the global flags that have an environment and a config
// carrier besides the flag: every persistent flag but --rev (CT9). A
// revision token is per object and per write, so an exported DNVCTL_REV
// would stamp one number on every later write that carries a token; it is
// read off the command line alone, like every leaf flag (see invoked).
var envGlobals = []string{
	"gateway-address", "cluster", "sp", "timeout", "trace-id", "config",
}

// bindViper wires flags, config file and environment together the way the
// four daemons do (CT9), for the env-backed global flags only: it binds each
// of them from the invoked command's flag set and binds nothing else, and no
// other value is ever read through viper.
func bindViper(cmd *cobra.Command) error {
	for _, name := range envGlobals {
		if err := viper.BindPFlag(name, cmd.Flags().Lookup(name)); err != nil {
			return err
		}
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
// gateway, the argv → request tests' recordingClient and the CT5 tests'
// stub client do not.
type dialFunc func(ctx context.Context, address string) (
	pb.GatewayClient, func() error, error)

// dial is the production dialFunc: the mandatory client block of grpc.md,
// Wiring. Both chains are installed even though all 59 RPCs are unary,
// because grpc.md, Wiring, says both chains on every dnv connection.
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

// run is every leaf command's RunE body: resolve the global flags, build
// the traced and deadlined context, dial, invoke, render.
//
// A returned *rpcFailure is exit 1; every other error is a usage error and
// exit 2. The ordering matters: everything that can fail to PARSE is done
// BEFORE the dial, so a usage error provably issues no RPC (CT5, CT8).
func run(cmd *cobra.Command, build func() (job, error)) error {
	address := strings.TrimSpace(viper.GetString("gateway-address"))
	if address == "" {
		return errors.New("missing required flag: --gateway-address")
	}
	budget, err := timeoutOf()
	if err != nil {
		return err
	}
	invoked = cmd.Flags()
	revRead = false
	defer func() { invoked = nil }()
	j, err := build()
	if err != nil {
		return err
	}
	// A typed --rev that build never read has no field to travel in: this
	// command's request carries no token (CT3). Sending the request without
	// it would drop, unseen, a gate the operator asked for, so it is a usage
	// error, refused before the dial like every other.
	if invoked.Changed("rev") && !revRead {
		return fmt.Errorf("invalid --rev: %q takes no revision token",
			cmd.CommandPath())
	}

	// The trace id: --trace-id when non-empty, else the T4 mint. The client
	// chain of grpc.md, Wiring, moves it into the outgoing trace_id metadata;
	// dnvctl never touches metadata itself (that shortcut belongs to the
	// integtest drivers, grpc.md, Drivers and fakes).
	traceId := strings.TrimSpace(viper.GetString("trace-id"))
	if traceId == "" {
		traceId = common.NewTraceId()
	}
	ctx := common.WithTraceId(context.Background(), traceId)
	ctx, cancel := context.WithTimeout(ctx, budget)
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

// leaf builds one RPC command. Every one of the 59 RPC leaves has this shape:
// a Use line, a one-line Short, no positional arguments, and a RunE that
// defers to run.
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
// help to STDOUT and exiting 0. That breaks CT5 (an unknown command must be
// exit 2 with empty stdout) and CT4 (stdout carries the result document and
// nothing else) at once, and it tells a script that mistyped a verb that it
// succeeded. Being Runnable puts ValidateArgs back in the path, where
// cobra.NoArgs produces the `unknown command` error a typo deserves.
//
// `--help` is unaffected: cobra handles the help flag before any of this, so
// the stock help (dnvctl.md, Conventions) still prints to stdout and exits 0.
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
// Result rendering (CT4)
// ---------------------------------------------------------------------------

// marshalOpts renders every reply. UseProtoNames keeps the JSON field names
// identical to schema.proto's spelling; EmitUnpopulated keeps a false/0/[]
// field visible, which is what the `created` poll
// (architecture.md, Thin devices: the client's wait primitive) needs from
// `td list`.
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

// hexBitmapResult is CT4's one deviation. protojson renders a bytes field as
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
// Reading values back (CT9)
// ---------------------------------------------------------------------------

// timeoutOf is --timeout, the one numeric env-backed global flag, as the
// budget run hands context.WithTimeout. Its environment and config carriers
// hand viper text, and viper's GetFloat64 casts text that is not a number to
// 0 — a deadline that has already passed — so the text is parsed here, and
// text that is not a number is refused as pflag refuses it as a flag
// argument: a usage error, before any dial. Go's float parser, which is
// pflag's too, also accepts NaN and ±Inf, which name no deadline, so those
// are refused the same way from every carrier. Any other value is kept as
// typed — zero or negative is a deadline already passed, and the call fails
// DEADLINE_EXCEEDED — but saturated at time.Duration's range, about 292
// years: out of range, Go leaves the float-to-integer conversion to the
// implementation, and amd64 answers math.MinInt64, an expired deadline for
// --timeout 1e10.
func timeoutOf() (time.Duration, error) {
	raw := strings.TrimSpace(viper.GetString("timeout"))
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --timeout: %w", err)
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf(
			"invalid --timeout: %q is not a finite number of seconds", raw)
	}
	switch nanos := seconds * float64(time.Second); {
	case nanos >= float64(math.MaxInt64):
		return math.MaxInt64, nil
	case nanos <= float64(math.MinInt64):
		return math.MinInt64, nil
	default:
		return time.Duration(nanos), nil
	}
}

// invoked is the flag set of the command being run: its own flags merged with
// the root's persistent ones, as cobra's ParseFlags left them. run points it
// at the leaf before calling build, and the leaf readers below (strOf and the
// rest) read it, so --rev and every leaf value come off the parsed command
// line and nothing else — never through viper, which is what keeps
// DNVCTL_<LEAF> and a config-file key of the same name out of every request
// (CT9). It is package state for the reason viper is (one invocation per
// process), and it spares the twelve group files' build closures a
// parameter.
var invoked *pflag.FlagSet

// revRead records that the command being run read its token: revToken sets
// it and run clears it before each build. The 34 token carriers read --rev
// in build, and the 25 commands whose request has no token field never do,
// so a typed --rev left unread is how run knows it has nowhere to go (CT3).
var revRead bool

// leafValue reads one flag of the invoked command through pflag's typed
// getter. pflag already parsed the value with the command line, so the only
// errors left are a name the command does not declare or a reader of the
// wrong type — bugs in this package, not operator errors, and the
// integration suite's sweep (dnvctl.md, Integration test plan) drives every
// leaf — so it panics rather than reading back as a zero.
func leafValue[T any](
	name string, get func(*pflag.FlagSet, string) (T, error),
) T {
	if invoked == nil {
		panic(fmt.Sprintf("dnvctl: --%s read outside run", name))
	}
	value, err := get(invoked, name)
	if err != nil {
		panic(fmt.Sprintf("dnvctl: reading --%s: %v", name, err))
	}
	return value
}

// strOf is one string value.
func strOf(name string) string {
	return leafValue(name, (*pflag.FlagSet).GetString)
}

// boolOf is one boolean value. A boolean that must be turned off is written
// --enabled=false; --enabled false would be parsed as a positional argument
// and rejected by cobra.NoArgs.
func boolOf(name string) bool {
	return leafValue(name, (*pflag.FlagSet).GetBool)
}

// u64Of is one plain uint64 (a size or a count), decimal.
func u64Of(name string) uint64 {
	return leafValue(name, (*pflag.FlagSet).GetUint64)
}

// u32Of is one plain uint32 (an index or a count), off a Uint32 flag, so a
// value above 2^32-1 is pflag's parse error rather than a narrowing here.
func u32Of(name string) uint32 {
	return leafValue(name, (*pflag.FlagSet).GetUint32)
}

// hexOf is an id value: Go base-0, so 17 and 0x11 are the same number. An
// unset flag is the empty string and reads as 0; a malformed one is a usage
// error (exit 2), which is all the checking dnvctl does on an id (CT8).
func hexOf(name string) (uint64, error) {
	raw := strings.TrimSpace(strOf(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s: %w", name, err)
	}
	return value, nil
}

// strListOf is a comma-split string list. It REPLACES on each occurrence
// rather than accumulating, and empty items are dropped, so `--hosts a,,b` is
// two hosts. An empty flag is an empty list, which several RPCs treat as a
// meaningful "clear it".
func strListOf(name string) []string {
	var out []string
	for _, item := range strings.Split(strOf(name), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// idListOf is a comma-split base-0 numeric list (--ids).
func idListOf(name string) ([]uint64, error) {
	var out []uint64
	for _, item := range strings.Split(strOf(name), ",") {
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
	for _, item := range strings.Split(strOf(name), ",") {
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
// non-empty one is a usage error (exit 2), per the note on
// `clone append-bm` in dnvctl.md, `clone` — `ctl/clone.go`.
func hexBytesOf(name string) ([]byte, error) {
	raw := strings.TrimSpace(strOf(name))
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
// The two scope global flags (dnvctl.md, Conventions)
// ---------------------------------------------------------------------------

// clusterOf fills `cluster_name`. It is the global --cluster for 55 of the 58
// requests that carry the field; only the `cluster` group's own three
// commands override it, through clusterNameOf. Like spOf it reads an
// env-backed global flag, so it reads viper, where bindViper put the flag's
// carriers (CT9).
func clusterOf() string { return viper.GetString("cluster") }

// clusterNameOf is the `cluster` group's rule: its own --name wins, and an
// empty --name falls back to the global --cluster. A `cluster` command is the
// only place the field is addressed by anything but the global flag.
func clusterNameOf() string {
	if name := strOf("name"); name != "" {
		return name
	}
	return clusterOf()
}

// spOf fills `sp_name` on the 41 requests that carry it, `sp create`
// included.
func spOf() string { return viper.GetString("sp") }

// ---------------------------------------------------------------------------
// Revision tokens (CT3)
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
// "Given" is pflag's Changed bit — the flag was typed on this command line —
// never a value from some other carrier: --rev has none (CT9), so an
// exported DNVCTL_REV cannot put a token on a write nobody typed it for. A
// typed --rev must parse, an empty one included (`--rev "$REV"` with REV
// unset is a usage error, not an ungated write). Calling it records the read
// (revRead): a typed --rev that no build reads is refused by run.
func revToken() (uint64, bool, error) {
	revRead = true
	raw := strings.TrimSpace(strOf("rev"))
	if !invoked.Changed("rev") {
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
// Shared flag helpers (dnvctl.md, Conventions)
// ---------------------------------------------------------------------------

// pageFlags adds the two flags every paged List* takes. A count of 0 asks for
// the server's default page size; dnvctl does not substitute one (CT8).
//
// --count is a Uint32 because `count` is uint32 on all four List* requests.
// Declaring it wider and narrowing on read would turn an out-of-range value
// into a silent wrap to 0 — i.e. into "use the server default", the opposite
// of what was asked — whereas pflag rejects it outright as the parse failure
// it is (exit 2, no RPC issued), the kind of rejection CT8 allows.
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
// conf — the GW11 "not given" convention: the handler stores the pair as it
// arrived, the sp-worker fills a migration's zeros in (dnv-worker.md RW15),
// and a clone's are never sent to the dm-clone target at all (CN18), leaving
// the target's own default in place.
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
