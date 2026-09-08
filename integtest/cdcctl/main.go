// Command cdcctl is the etcd driver of the dnv-cdc integration test (cdc.md
// §9.2, §9.6): "the etcd driver that plays gateway + worker for CdcEntry keys
// only". It formats the {p} cdc keys through model, marshals pb.CdcEntry and
// reads the prefix back as protojson.
//
// It runs ON SERVER 1, where etcd listens on the loopback (§9.3), and is
// invoked over ssh by integtest/cdc_test.sh (§9.2). It touches NOTHING but
// CdcEntry keys: dnv-cdc reads only those and never reads ClusterConf, so this
// suite fabricates its cluster ids (§0 #13, §9.5) and --cluster takes the id
// itself, not a name. No ClusterConf, global, revision or capacity key is ever
// read or written here — that whole pipeline is the worker suite's job
// (dnv-worker.md §14, whose case D already asserts CdcEntry contents). Apart
// from that, this driver follows workerctl's conventions exactly.
//
// Conventions the script relies on:
//
//   - Global flags may be given BEFORE the subcommand (a `ctl` wrapper does
//     exactly that) or after it; the later occurrence wins.
//   - stdout carries exactly one JSON document per invocation — except `list`,
//     which prints one "<key>\t<protojson>" line per entry (§9.6), and an
//     empty prefix therefore prints nothing at all.
//   - The log.md §5.3 records etcdutil emits ("etcd get", "etcd put", …) go to
//     STDERR, not to the process-wide stdout handler common/log.go installs,
//     so that they never interleave with the JSON the script pipes into jq.
//     They carry --trace-id, so a failing run still correlates every write of
//     the driver with the case that made it.
//   - protojson is emitted with EmitUnpopulated, so an entry with no
//     allowed_hosts — the §0 #5 "visible to everyone" entry, which §9.5 uses
//     for ssA — reads back as an explicit [] instead of vanishing from the
//     document.
//   - Ids accept decimal or 0x hex; --shard is always read as HEX (§9.5
//     spreads the entry set over 00, 07, 08, 3c, 81 and ff).
//
// Exit codes: 0 on success, 1 on any error (with a message on stderr), 2 on a
// usage error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// defaultEndpoints is the single-node etcd of §9.3. It is deliberately NOT
	// workerctl's 12379: §9.3 gives this suite 13379/13380 so that a stray
	// cdcctl can never write into a worker-suite store, and the two suites can
	// be installed on the same lab machine.
	defaultEndpoints = "127.0.0.1:13379"
	// pingKey is the key `ping` reads. It is deliberately a key nothing ever
	// writes: the probe succeeds on not-found, so what it proves is that etcd
	// answers, not that anything is stored.
	pingKey = common.DnvPrefix + " ping"
	// trConfForm is the --tr spelling. Its fields are COMMA-separated, unlike
	// workerctl's ':'-separated tuples, because tr_addr may be an IPv6
	// literal (adr_fam ipv6, §2.1) whose colons would be indistinguishable
	// from field separators.
	trConfForm = "tr_type,adr_fam,tr_addr,tr_svc_id"
)

// ---------------------------------------------------------------------------
// Output and failure
// ---------------------------------------------------------------------------

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cdcctl: "+format+"\n", args...)
	os.Exit(1)
}

func usageDie(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cdcctl: "+format+"\n", args...)
	os.Exit(2)
}

// marshalOpts renders every message this driver prints. UseProtoNames keeps
// the JSON field names identical to the schema.proto spelling the assertions
// quote (nvme_tr_conf_list, allowed_hosts); EmitUnpopulated keeps an empty
// repeated field visible, which is what an open entry (§0 #5) needs.
var marshalOpts = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// pbToAny renders one message as a generic JSON value, so that a reply can
// nest the message inside an object carrying its key.
func pbToAny(msg proto.Message) any {
	raw, err := marshalOpts.Marshal(msg)
	if err != nil {
		die("marshaling %T failed: %v", msg, err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		die("re-parsing %T failed: %v", msg, err)
	}
	return value
}

// mustJson renders one value the stable way. protojson deliberately varies its
// whitespace, so everything goes through encoding/json (sorted keys, stable
// spacing) to stay diffable and greppable.
func mustJson(value any) []byte {
	out, err := json.Marshal(value)
	if err != nil {
		die("marshaling the reply failed: %v", err)
	}
	return out
}

// emit prints one JSON document: the whole stdout of an invocation.
func emit(value any) {
	fmt.Println(string(mustJson(value)))
}

// idHex renders an id the way every key field does (architecture.md §5.1), so
// that a reply names ids in the spelling the keys use.
func idHex(id uint64) string {
	return fmt.Sprintf(common.IdKeyFmt, id)
}

// shardHex renders a shard code the way the key field does (§5.1).
func shardHex(shard uint32) string {
	return fmt.Sprintf(common.ShardCodeFmt, shard)
}

// ---------------------------------------------------------------------------
// Flag value types
// ---------------------------------------------------------------------------

// parseId parses one id field: decimal or 0x hex, parsed with base 0. §9.5
// writes the fabricated cluster ids as 0xcdc2 and the sp/ss ids as 0x1/0xa.
func parseId(s string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an id: %w", s, err)
	}
	return value, nil
}

// hexUint is an id flag: decimal or 0x hex.
type hexUint uint64

func (h *hexUint) String() string { return fmt.Sprintf("%#x", uint64(*h)) }

func (h *hexUint) Set(s string) error {
	value, err := parseId(s)
	if err != nil {
		return err
	}
	*h = hexUint(value)
	return nil
}

// shardFlag is a --shard flag. A shard code is ALWAYS written in its
// common.ShardCodeFmt spelling (architecture.md §5.1), so the value is parsed
// as HEX — with or without a 0x prefix. Reading "81" as decimal would place
// the §9.5 cross-cluster entry in shard 0x51, i.e. in the low half, where the
// wrong pair of instances would serve it.
//
// Unlike workerctl's, it remembers whether it was SET: "00" is a shard code
// the §9.5 table actually uses (ssA), so a missing --shard cannot be told
// from --shard 00 by value alone. A put that silently landed in shard 00 would
// still serve, and the mistake would surface much later as a `del` that finds
// nothing.
type shardFlag struct {
	set  bool
	code uint32
}

func (c *shardFlag) String() string {
	if c == nil || !c.set {
		return "unset"
	}
	return shardHex(c.code)
}

func (c *shardFlag) Set(s string) error {
	trimmed := strings.TrimSpace(s)
	trimmed = strings.TrimPrefix(trimmed, "0x")
	trimmed = strings.TrimPrefix(trimmed, "0X")
	value, err := strconv.ParseUint(trimmed, 16, 32)
	if err != nil {
		return fmt.Errorf("%q is not a hex shard code: %w", s, err)
	}
	if value >= common.ShardBucketSize {
		return fmt.Errorf(
			"shard code %q is out of range (00..%02x)",
			s, common.ShardBucketSize-1,
		)
	}
	c.set = true
	c.code = uint32(value)
	return nil
}

// stringList collects a repeatable flag in the order it was given. It is what
// --allowed uses: allowed_hosts is a set to DS4's filter, but the driver still
// stores exactly what the script listed, so that a `list` row can be compared
// literally against the case's own table.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return fmt.Errorf("an empty hostnqn is not a host")
	}
	*l = append(*l, trimmed)
	return nil
}

// trConfList collects the repeatable --tr flag IN ORDER: an element's position
// in nvme_tr_conf_list is the DS5 tr-conf index that decides which rendered
// log entry a change impacts, and §9.11 step 3 asserts ssD's two records by
// their trsvcids. The driver therefore never reorders, deduplicates or sorts
// them.
type trConfList []*pb.NvmeTrConf

func (l *trConfList) String() string {
	if l == nil {
		return ""
	}
	specs := make([]string, 0, len(*l))
	for _, conf := range *l {
		specs = append(specs, strings.Join([]string{
			conf.GetTrType(), conf.GetAdrFam(),
			conf.GetTrAddr(), conf.GetTrSvcId(),
		}, ","))
	}
	return strings.Join(specs, " ")
}

func (l *trConfList) Set(s string) error {
	conf, err := parseTrConf(s)
	if err != nil {
		return err
	}
	*l = append(*l, conf)
	return nil
}

// parseTrConf reads one `--tr tcp,ipv4,<ip2>,14420` (§9.5's port<n>).
//
// tr_type is NOT checked against common.DefaultCdcTrType: DS3 skips an element
// whose tr_type is not tcp and keeps serving the rest of the entry, so a case
// must be able to write exactly that. Only an EMPTY field is refused, since a
// field that is empty is always a mistyped flag and would otherwise reach the
// host as an unusable TRADDR or TRSVCID.
func parseTrConf(spec string) (*pb.NvmeTrConf, error) {
	parts := strings.Split(strings.TrimSpace(spec), ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("want %s, got %q", trConfForm, spec)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return nil, fmt.Errorf(
				"field %d of %q is empty: want %s", i+1, spec, trConfForm,
			)
		}
	}
	return &pb.NvmeTrConf{
		TrType:  parts[0],
		AdrFam:  parts[1],
		TrAddr:  parts[2],
		TrSvcId: parts[3],
	}, nil
}

// ---------------------------------------------------------------------------
// Globals (§9.6)
// ---------------------------------------------------------------------------

type globals struct {
	endpoints string
	cluster   hexUint
	traceId   string
	timeout   float64
}

func newGlobals() globals {
	return globals{endpoints: defaultEndpoints, timeout: 10}
}

// bind registers the global flags. It is called twice — once on the top-level
// set, once on the subcommand's — with the current values as defaults, so that
// `cdcctl --cluster 0xcdc2 put …` (what a `ctl` wrapper types) and
// `cdcctl put --cluster 0xcdc2 …` are both accepted and the later occurrence
// wins.
func (g *globals) bind(fs *flag.FlagSet) {
	fs.StringVar(&g.endpoints, "etcd", g.endpoints,
		"comma-separated etcd client endpoints (§9.6)")
	// workerctl spells the same flag --endpoints. Both suites are driven from
	// the same script style by the same hands, so the other spelling is
	// accepted rather than silently ignored as an unknown flag.
	fs.StringVar(&g.endpoints, "endpoints", g.endpoints,
		"alias for --etcd (workerctl's spelling)")
	fs.Var(&g.cluster, "cluster",
		"cluster id, decimal or 0x hex; it is FABRICATED (§0 #13, §9.5), "+
			"no ClusterConf is read")
	fs.StringVar(&g.traceId, "trace-id", g.traceId,
		"trace id stamped on every log record of this invocation")
	fs.Float64Var(&g.timeout, "timeout", g.timeout,
		"overall timeout in seconds")
}

// open dials etcd and returns the bounded, trace-carrying ctx every command
// runs under.
func (g *globals) open() (context.Context, func(), *etcdutil.Client) {
	ctx := context.Background()
	if g.traceId != "" {
		ctx = common.WithTraceId(ctx, g.traceId)
	}
	ctx, cancel := context.WithTimeout(
		ctx, time.Duration(g.timeout*float64(time.Second)),
	)
	cli, err := etcdutil.New(ctx, strings.Split(g.endpoints, ","), 0)
	if err != nil {
		cancel()
		die("%v", err)
	}
	return ctx, func() {
		cli.Close()
		cancel()
	}, cli
}

// clusterId returns the cluster id of the invocation. Unlike workerctl's, it
// reads nothing: the id is fabricated per case (§9.5 — S 0xcdc1 … H 0xcdc5,
// plus case id + 0x1000 for the cross-cluster entry) and no ClusterConf exists
// to derive it from (§0 #13). Zero stands for "not given": no case uses it,
// and a missing --cluster would otherwise write into a cluster id of 0, where
// dnv-cdc would happily serve the entry and only the later `del` would fail.
func (g *globals) clusterId() uint64 {
	if uint64(g.cluster) == 0 {
		die("--cluster is required (a nonzero cluster id, e.g. 0xcdc2)")
	}
	return uint64(g.cluster)
}

// entryKey builds the one key kind this driver knows (MD2, §2.2) out of the
// four addressing flags, refusing every value that is missing rather than
// defaulting it — see shardFlag on why a defaulted key field is invisible
// until a much later assertion fails.
func entryKey(g *globals, shard *shardFlag, spId, ssId hexUint) (
	string, uint64,
) {
	cid := g.clusterId()
	if !shard.set {
		die("--shard is required (2 hex digits, e.g. 00, 3c, ff)")
	}
	if uint64(spId) == 0 {
		die("--sp is required (a nonzero sp_id)")
	}
	if uint64(ssId) == 0 {
		die("--ss is required (a nonzero ss_id)")
	}
	return model.CdcEntryKey(
		cid, shard.code, uint64(spId), uint64(ssId),
	), cid
}

// ---------------------------------------------------------------------------
// Command plumbing
// ---------------------------------------------------------------------------

type command struct {
	name string
	run  func(g *globals, args []string)
}

var commands = []command{
	{"ping", cmdPing},
	{"put", cmdPut},
	{"del", cmdDel},
	{"wipe", cmdWipe},
	{"list", cmdList},
}

func main() {
	// The etcdutil records belong on stderr: stdout is one JSON document per
	// invocation, which the script pipes into jq.
	slog.SetDefault(slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(os.Stderr, nil),
	}))
	g := newGlobals()
	top := flag.NewFlagSet("cdcctl", flag.ExitOnError)
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
	for _, cmd := range commands {
		if cmd.name == args[0] {
			cmd.run(&g, args[1:])
			return
		}
	}
	usageDie("unknown subcommand %q", args[0])
}

func usage() {
	names := make([]string, 0, len(commands))
	for _, cmd := range commands {
		names = append(names, cmd.name)
	}
	fmt.Fprintf(os.Stderr,
		"usage: cdcctl [global flags] <subcommand> [flags]\n"+
			"global flags: --etcd --cluster --trace-id --timeout\n"+
			"subcommands: %s\n", strings.Join(names, " "))
}

func newFlagSet(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	g.bind(fs)
	return fs
}

// ---------------------------------------------------------------------------
// ping
// ---------------------------------------------------------------------------

// cmdPing is the etcd liveness probe of the §9.4 preflight: one Get of a key
// nothing writes, which succeeds on not-found.
func cmdPing(g *globals, args []string) {
	fs := newFlagSet("ping", g)
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()

	found, err := cli.Get(ctx, pingKey, &pb.CdcEntry{})
	if err != nil {
		die("ping: %v", err)
	}
	emit(map[string]any{
		"endpoints": g.endpoints,
		"key":       pingKey,
		"found":     found,
	})
}

// ---------------------------------------------------------------------------
// put / del
// ---------------------------------------------------------------------------

// cmdPut writes one CdcEntry at model.CdcEntryKey (§9.6) — what the gateway
// does at CreateSubsystem / UpdateSubsystemAllowedHosts and the worker at
// CreateCntlr / ReplaceCntlr (§1), reduced to the one key dnv-cdc reads.
//
// It is ONE plain Put, not a read-modify-write: the stored value is the whole
// message, and the rewrites of §9.12 steps 4-5 and §9.13 step 4 ("ssE now
// allows only H2", "port3 → port4") are expressed by re-putting the entry. A
// driver that merged with what is already stored could not express a REMOVAL
// at all, and case L's negative — h1 losing ssE — is exactly a removal.
func cmdPut(g *globals, args []string) {
	fs := newFlagSet("put", g)
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, 2 hex digits (00, 3c, ff …)")
	var spId hexUint
	fs.Var(&spId, "sp", "sp_id, the {sp_id} key field")
	var ssId hexUint
	fs.Var(&ssId, "ss", "ss_id, the {ss_id} key field")
	nqn := fs.String("nqn", "", "CdcEntry.nqn, the subsystem NQN (required)")
	var trConfs trConfList
	fs.Var(&trConfs, "tr",
		"one nvme_tr_conf, "+trConfForm+" (repeatable, order preserved)")
	var allowed stringList
	fs.Var(&allowed, "allowed",
		"one allowed hostnqn (repeatable; none at all means every host)")
	fs.Parse(args)

	key, cid := entryKey(g, &shard, spId, ssId)
	subNqn := strings.TrimSpace(*nqn)
	if subNqn == "" {
		die("--nqn is required")
	}
	if len(trConfs) == 0 {
		// An entry with no transport renders no discovery log entry at all
		// (DS3), so it would be served as silence — indistinguishable from a
		// put that never happened. That is never what a case means.
		die("at least one --tr is required (%s)", trConfForm)
	}

	entry := &pb.CdcEntry{
		Nqn:            subNqn,
		NvmeTrConfList: []*pb.NvmeTrConf(trConfs),
		AllowedHosts:   []string(allowed),
	}

	ctx, done, cli := g.open()
	defer done()

	if err := cli.Put(ctx, key, entry); err != nil {
		die("put: %v", err)
	}
	emit(map[string]any{
		"key":        key,
		"cluster_id": idHex(cid),
		"shard_code": shardHex(shard.code),
		"sp_id":      idHex(uint64(spId)),
		"ss_id":      idHex(uint64(ssId)),
		"cdc_entry":  pbToAny(entry),
	})
}

// cmdDel removes one CdcEntry (§9.6) — DeleteSubsystem as dnv-cdc sees it, the
// step that makes a host lose a device in §9.12 step 6 and §9.13 step 5.
//
// It reads the entry first, for two reasons: the reply then reports what was
// removed, and a key that is not there is refused. A del that names a key no
// case ever wrote is always a mistyped id here — the per-case reset is `wipe`
// (§9.9), and every del of §9.12/§9.13 names an entry the case itself put — so
// failing loudly, with the key in the message, is the only place that typo is
// ever visible; skipping it silently would surface minutes later as "the host
// still has the device".
func cmdDel(g *globals, args []string) {
	fs := newFlagSet("del", g)
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, 2 hex digits (00, 3c, ff …)")
	var spId hexUint
	fs.Var(&spId, "sp", "sp_id, the {sp_id} key field")
	var ssId hexUint
	fs.Var(&ssId, "ss", "ss_id, the {ss_id} key field")
	fs.Parse(args)

	key, cid := entryKey(g, &shard, spId, ssId)

	ctx, done, cli := g.open()
	defer done()

	old := &pb.CdcEntry{}
	found, err := cli.Get(ctx, key, old)
	if err != nil {
		die("del: %v", err)
	}
	if !found {
		die("key %q not found", key)
	}
	if err := cli.Delete(ctx, key); err != nil {
		die("del: %v", err)
	}
	emit(map[string]any{
		"key":        key,
		"cluster_id": idHex(cid),
		"shard_code": shardHex(shard.code),
		"sp_id":      idHex(uint64(spId)),
		"ss_id":      idHex(uint64(ssId)),
		"cdc_entry":  pbToAny(old),
	})
}

// ---------------------------------------------------------------------------
// wipe / list
// ---------------------------------------------------------------------------

// cmdWipe deletes every key under model.CdcEntryPrefix() — the per-case reset
// of §9.9 and the mass-teardown assertion of §9.13 step 6, where both hosts
// must converge to zero dnv-it subsystems.
//
// etcdutil exposes no range delete and layout.md §3 allows this driver no
// other etcd path, so the range is walked with RangeKeys (keys only — the
// values are about to be gone) and each key deleted on its own. The count is
// reported, so the script can assert what a reset actually removed.
//
// It deliberately ignores --cluster, even though the flag is global and a
// `ctl` wrapper may carry one: a reset must leave the prefix EMPTY. Narrowing
// it to one cluster would silently leave the second-cluster entry of §9.5
// (ssF, in case id + 0x1000) behind, and the next case would start with a
// stray entry that its own discover grid does not expect.
func cmdWipe(g *globals, args []string) {
	fs := newFlagSet("wipe", g)
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()

	prefix := model.CdcEntryPrefix()
	keys, _, err := cli.RangeKeys(ctx, prefix)
	if err != nil {
		die("wipe: %v", err)
	}
	for _, key := range keys {
		if err := cli.Delete(ctx, key.Key); err != nil {
			die("wipe: %v", err)
		}
	}
	emit(map[string]any{
		"prefix":  prefix,
		"deleted": len(keys),
	})
}

// cmdList prints every CdcEntry as "<key>\t<protojson>", one per line (§9.6) —
// the diagnostics dump of §9.16. Like wipe it walks the whole prefix and
// ignores --cluster: DS1 serves every cluster, so a dump that hid one would
// hide exactly the stray key it is there to find. An empty prefix prints
// nothing, which is what a post-wipe `wc -l` expects.
func cmdList(g *globals, args []string) {
	fs := newFlagSet("list", g)
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()

	kvs, _, err := cli.Range(ctx, model.CdcEntryPrefix())
	if err != nil {
		die("list: %v", err)
	}
	for _, kv := range kvs {
		entry := &pb.CdcEntry{}
		if err := cli.Decode(ctx, kv, entry); err != nil {
			die("list: %v", err)
		}
		fmt.Printf("%s\t%s\n", kv.Key, mustJson(pbToAny(entry)))
	}
}
