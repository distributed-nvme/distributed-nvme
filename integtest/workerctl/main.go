// Command workerctl is the etcd driver of the dnv-worker integration test
// (dnv-worker.md, Integration test plan, The driver): "the etcd driver that
// plays the gateway". It is a front over the planting library,
// integtest/plant, which holds every write it makes: this program parses its
// flags and their string specs into the library's typed specs, calls the
// library, and prints what the library wrote. It reads keys back as protojson
// on its own.
//
// Every subcommand that reaches etcd runs ON THE TEST SERVER, where etcd
// listens on localhost, and is invoked over ssh by two suites:
// integtest/worker_test.sh, whose fake gateway it is (dnv-worker.md,
// Integration test plan, The driver), and integtest/gateway_test.sh, which
// reads etcd back through it as ground truth and writes through exactly the
// five worker-role stand-ins — set-created, set-provisioned, start-cutover,
// drain-sp and drain-clone — of gateway.md, Integration test plan, The etcd
// verification.
// It never dials an agent and never sleeps: it is the gateway's write path
// with explicit placement (dnv-worker.md, Integration test plan, The driver).
// Every mutation is one etcdutil.RunSTM of the library, so the state it
// leaves behind is exactly the state a real gateway STM would have produced.
//
// `constants`, `geometry` and `meta-units` are the exceptions: they open no
// client and read no key, so a suite runs them on the DRIVER, on the binary it
// has just built and before it ships anything. They are how a shell suite
// reads a Go constant, the group geometry formula (architecture.md, Group
// on-leg layout: meta region, data region, health block), a side's length to
// zero (architecture.md, Side provisioning protocol) and the units a DN's
// metadata area gives a migration (architecture.md, Metadata units) instead
// of hand-copying any of them.
//
// Conventions the script relies on:
//
//   - Global flags may be given BEFORE the subcommand (worker_test.sh's `ctl`
//     wrapper does exactly that) or after it; the later occurrence wins.
//   - stdout carries exactly one JSON document per invocation — protojson for
//     a read, the ids and revisions it assigned for a mutation — except
//     list-keys (one key per line) and list-workers (one object per line).
//   - The etcd records etcdutil emits (log.md, etcd: "etcd get", "etcd put",
//     …) go to STDERR, where common/log.go's init() handler puts every
//     record, so that they never interleave with the JSON the script pipes
//     into jq.
//     They carry --trace-id, so a failing run still correlates every write of
//     the driver with the test stage that made it.
//   - protojson is emitted with EmitUnpopulated, so that the fields the suite's
//     assertions read — err_epoch, provisioned, created, primary — are present
//     even when they hold their proto3 default. uint64 fields are JSON strings
//     (the proto3 JSON mapping), so jq compares them as "0", not 0.
//   - Ids accept decimal or 0x hex everywhere, and every id flag that names
//     a new object is required: the library's "0 takes the next free id" is
//     for Go callers only.
//
// Exit codes: 0 on success, 1 on any error (with a message on stderr), 2 on a
// usage error.
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

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// defaultEndpoints is the suite's single-node etcd (dnv-worker.md,
	// Integration test plan, Topology): it listens on the loopback of the test
	// server, and workerctl runs there.
	defaultEndpoints = "127.0.0.1:12379"
	// pingKey is the key `ping` reads. It is deliberately a key nothing ever
	// writes: the probe succeeds on not-found, so what it proves is that etcd
	// answers, not that anything is stored.
	pingKey = common.DnvPrefix + " ping"
)

// ---------------------------------------------------------------------------
// Output and failure
// ---------------------------------------------------------------------------

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "workerctl: "+format+"\n", args...)
	os.Exit(1)
}

func usageDie(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "workerctl: "+format+"\n", args...)
	os.Exit(2)
}

// marshalOpts renders every message this driver prints. UseProtoNames keeps
// the JSON field names identical to the schema.proto spelling the assertions
// quote; EmitUnpopulated keeps a false/0/[] field visible, which is what the
// suite's checks on provisioned, created and err_epoch need.
var marshalOpts = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// pbToAny renders one message as a generic JSON value, so that composite
// replies (get-sp) can nest messages inside an object of their own.
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

// emit prints one JSON document. protojson deliberately varies its whitespace,
// so everything goes through encoding/json (sorted keys, stable spacing) to
// stay diffable.
func emit(value any) {
	out, err := json.Marshal(value)
	if err != nil {
		die("marshaling the reply failed: %v", err)
	}
	fmt.Println(string(out))
}

func emitPb(msg proto.Message) {
	emit(pbToAny(msg))
}

// idHex renders an id the way every key field does (architecture.md, Key
// grammar), so that the map members of a composite reply are addressable by
// the same spelling the keys use.
func idHex(id uint64) string {
	return fmt.Sprintf(common.IdKeyFmt, id)
}

// idHexList renders a list of ids with idHex.
func idHexList(ids []uint64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, idHex(id))
	}
	return out
}

// shardHex renders a shard code the way every key field does.
func shardHex(shard uint32) string {
	return fmt.Sprintf(common.ShardCodeFmt, shard)
}

// ---------------------------------------------------------------------------
// Flag value types
// ---------------------------------------------------------------------------

// hexUint is an id flag: decimal or 0x hex, parsed with base 0.
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
// common.ShardCodeFmt spelling (architecture.md, Key grammar), and
// worker_test.sh's case E spreads DNs over "00", "55", "aa" and "ff", so the
// value is parsed as HEX — with or without a 0x prefix. Reading it as decimal
// would silently turn the "55" of that case into shard 0x37.
type shardFlag uint32

func (c *shardFlag) String() string {
	return fmt.Sprintf(common.ShardCodeFmt, uint32(*c))
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
	*c = shardFlag(value)
	return nil
}

// stringList collects a repeatable flag in the order it was given.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// triBool is a bool flag that distinguishes "not given" from "given false",
// which is what `set-cntlr --primary=… --disabled=…` needs: each is settable
// on its own and neither may disturb the other.
type triBool struct {
	set   bool
	value bool
}

func (t *triBool) String() string {
	if !t.set {
		return "unset"
	}
	return strconv.FormatBool(t.value)
}

func (t *triBool) Set(s string) error {
	value, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("want a bool, got %q", s)
	}
	t.set = true
	t.value = value
	return nil
}

// IsBoolFlag lets `--disabled` stand for `--disabled=true`.
func (t *triBool) IsBoolFlag() bool { return true }

// ptr is the library's form of a triBool: nil when the flag was not given.
func (t *triBool) ptr() *bool {
	if !t.set {
		return nil
	}
	value := t.value
	return &value
}

// ---------------------------------------------------------------------------
// Tuple parsers. Every one of them rejects a malformed spec loudly, so
// that a mistyped test invocation fails instead of writing garbage into etcd.
// Each turns one string spec of a flag into the library's typed spec.
// ---------------------------------------------------------------------------

// parseId parses one id field: decimal or 0x hex.
func parseId(s string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an id: %w", s, err)
	}
	return value, nil
}

// parseIdField parses one ':'-separated id of a tuple and names the whole spec
// in the error, so that the message points at the flag the script typed.
func parseIdField(part string, spec string, form string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(part), 0, 64)
	if err != nil {
		return 0, fmt.Errorf(
			"%q in %q: want %s: %w", part, spec, form, err,
		)
	}
	return value, nil
}

// splitSpec splits a ':'-separated tuple and checks its field count.
func splitSpec(spec string, want int, form string) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	if len(parts) != want {
		return nil, fmt.Errorf("want %s, got %q", form, spec)
	}
	return parts, nil
}

// parseU32Field parses one ':'-separated field that must fit in 32 bits (an
// index, a slot, a percentage).
func parseU32Field(part string, spec string, form string) (uint32, error) {
	value, err := parseIdField(part, spec, form)
	if err != nil {
		return 0, err
	}
	if value > 0xffffffff {
		return 0, fmt.Errorf("%q in %q: does not fit in 32 bits", part, spec)
	}
	return uint32(value), nil
}

// parseBoolField parses one ':'-separated bool field.
func parseBoolField(part string, spec string, form string) (bool, error) {
	value, err := strconv.ParseBool(strings.TrimSpace(part))
	if err != nil {
		return false, fmt.Errorf(
			"%q in %q: want %s (a bool): %w", part, spec, form, err,
		)
	}
	return value, nil
}

const sidePtrForm = "sp:leg:side"

// parseSidePointer reads the `--side sp:leg:side` form of put-dn: one
// entry of a DnConf.side_ptr_list.
func parseSidePointer(spec string) (*pb.SidePointer, error) {
	parts, err := splitSpec(spec, 3, sidePtrForm)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 3)
	for i, part := range parts {
		ids[i], err = parseIdField(part, spec, sidePtrForm)
		if err != nil {
			return nil, err
		}
	}
	return &pb.SidePointer{SpId: ids[0], LegId: ids[1], SideId: ids[2]}, nil
}

const cntlrPtrForm = "sp:cntlr"

// parseCntlrPointer reads the `--cntlr sp:cntlr` form of put-cn.
func parseCntlrPointer(spec string) (*pb.CntlrPointer, error) {
	parts, err := splitSpec(spec, 2, cntlrPtrForm)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 2)
	for i, part := range parts {
		ids[i], err = parseIdField(part, spec, cntlrPtrForm)
		if err != nil {
			return nil, err
		}
	}
	return &pb.CntlrPointer{SpId: ids[0], CntlrId: ids[1]}, nil
}

const cntlrForm = "id:cn_id:slot:primary"

// parseCntlrSpec reads one `--cntlr id:cn_id:slot:primary` of put-sp: which
// cntlr id to use, which CN hosts it, its cntlid slot and whether it is the
// primary.
func parseCntlrSpec(spec string) (plant.CntlrSpec, error) {
	var out plant.CntlrSpec
	parts, err := splitSpec(spec, 4, cntlrForm)
	if err != nil {
		return out, err
	}
	if out.CntlrId, err = parseIdField(parts[0], spec, cntlrForm); err != nil {
		return out, err
	}
	if out.CnId, err = parseIdField(parts[1], spec, cntlrForm); err != nil {
		return out, err
	}
	if out.Slot, err = parseU32Field(parts[2], spec, cntlrForm); err != nil {
		return out, err
	}
	if out.Primary, err = parseBoolField(parts[3], spec, cntlrForm); err != nil {
		return out, err
	}
	return out, nil
}

const sliceForm = "id:idx"

// parseSliceSpec reads one `--slice id:idx` of put-sp.
func parseSliceSpec(spec string) (plant.SliceSpec, error) {
	var out plant.SliceSpec
	parts, err := splitSpec(spec, 2, sliceForm)
	if err != nil {
		return out, err
	}
	if out.SliceId, err = parseIdField(parts[0], spec, sliceForm); err != nil {
		return out, err
	}
	if out.SliceIdx, err = parseU32Field(parts[1], spec, sliceForm); err != nil {
		return out, err
	}
	return out, nil
}

const groupForm = "slice:grp:meta|data:ext_cnt:none|raid1"

// parseGroupSpec reads one `--group slice:grp:meta|data:ext_cnt:none|raid1` of
// put-sp.
func parseGroupSpec(spec string) (plant.GroupSpec, error) {
	var out plant.GroupSpec
	parts, err := splitSpec(spec, 5, groupForm)
	if err != nil {
		return out, err
	}
	if out.SliceId, err = parseIdField(parts[0], spec, groupForm); err != nil {
		return out, err
	}
	if out.GrpId, err = parseIdField(parts[1], spec, groupForm); err != nil {
		return out, err
	}
	switch strings.TrimSpace(parts[2]) {
	case "meta":
		out.Meta = true
	case "data":
		out.Meta = false
	default:
		return out, fmt.Errorf(
			"%q in %q: want meta or data", parts[2], spec,
		)
	}
	if out.ExtCnt, err = parseIdField(parts[3], spec, groupForm); err != nil {
		return out, err
	}
	if out.ExtCnt == 0 {
		return out, fmt.Errorf("%q: ext_cnt must not be zero", spec)
	}
	switch strings.TrimSpace(parts[4]) {
	case "raid1":
		out.Raid1 = true
	case "none":
		out.Raid1 = false
	default:
		return out, fmt.Errorf(
			"%q in %q: want none or raid1", parts[4], spec,
		)
	}
	return out, nil
}

const legForm = "grp:leg:idx"

// parseLegSpec reads one `--leg grp:leg:idx` of put-sp.
func parseLegSpec(spec string) (plant.LegSpec, error) {
	var out plant.LegSpec
	parts, err := splitSpec(spec, 3, legForm)
	if err != nil {
		return out, err
	}
	if out.GrpId, err = parseIdField(parts[0], spec, legForm); err != nil {
		return out, err
	}
	if out.LegId, err = parseIdField(parts[1], spec, legForm); err != nil {
		return out, err
	}
	if out.LegIdx, err = parseU32Field(parts[2], spec, legForm); err != nil {
		return out, err
	}
	return out, nil
}

const sideForm = "leg:side:dn_id:slot"

// parseSideSpec reads one `--side leg:side:dn_id:slot` of put-sp.
func parseSideSpec(spec string) (plant.SideSpec, error) {
	var out plant.SideSpec
	parts, err := splitSpec(spec, 4, sideForm)
	if err != nil {
		return out, err
	}
	if out.LegId, err = parseIdField(parts[0], spec, sideForm); err != nil {
		return out, err
	}
	if out.SideId, err = parseIdField(parts[1], spec, sideForm); err != nil {
		return out, err
	}
	if out.DnId, err = parseIdField(parts[2], spec, sideForm); err != nil {
		return out, err
	}
	if out.Slot, err = parseU32Field(parts[3], spec, sideForm); err != nil {
		return out, err
	}
	return out, nil
}

const cloneLegForm = "leg:idx"

// parseCloneLegSpec reads one `--leg leg:idx` of put-clone: one leg of the
// clone's clone meta group, which put-clone has no --group for, the clone
// owning exactly one. Its side is a `--side` of put-sp's form that names it
// (cloneLegsOf).
func parseCloneLegSpec(spec string) (plant.CloneLegSpec, error) {
	var out plant.CloneLegSpec
	parts, err := splitSpec(spec, 2, cloneLegForm)
	if err != nil {
		return out, err
	}
	if out.LegId, err = parseIdField(parts[0], spec, cloneLegForm); err != nil {
		return out, err
	}
	if out.LegIdx, err = parseU32Field(parts[1], spec, cloneLegForm); err != nil {
		return out, err
	}
	return out, nil
}

// cloneLegsOf joins put-clone's `--leg leg:idx` and `--side
// leg:side:dn_id:slot` flags into the library's legs, one side each, in the
// order the --leg flags come. A leg with no side or with two, a side whose
// leg no --leg names, and a leg or side id of 0 are refused: the library
// takes a 0 for "the next free id", which is for Go callers only.
func cloneLegsOf(legSpecs []string, sideSpecs []string) (
	[]plant.CloneLegSpec, error,
) {
	legs := make([]plant.CloneLegSpec, 0, len(legSpecs))
	at := make(map[uint64]int, len(legSpecs))
	for _, text := range legSpecs {
		leg, err := parseCloneLegSpec(text)
		if err != nil {
			return nil, fmt.Errorf("--leg: %w", err)
		}
		if leg.LegId == 0 {
			return nil, fmt.Errorf("--leg %q: leg id 0 is never valid", text)
		}
		if _, dup := at[leg.LegId]; dup {
			return nil, fmt.Errorf("--leg %q: leg %#x is named twice",
				text, leg.LegId)
		}
		at[leg.LegId] = len(legs)
		legs = append(legs, leg)
	}
	sided := make(map[uint64]bool, len(legs))
	for _, text := range sideSpecs {
		side, err := parseSideSpec(text)
		if err != nil {
			return nil, fmt.Errorf("--side: %w", err)
		}
		idx, ok := at[side.LegId]
		switch {
		case !ok:
			return nil, fmt.Errorf("--side %q: no --leg names leg %#x",
				text, side.LegId)
		case sided[side.LegId]:
			return nil, fmt.Errorf("--side %q: leg %#x already has a side "+
				"(a leg of a clone meta group has one)", text, side.LegId)
		case side.SideId == 0:
			return nil, fmt.Errorf("--side %q: side id 0 is never valid", text)
		}
		sided[side.LegId] = true
		legs[idx].SideId = side.SideId
		legs[idx].DnId = side.DnId
		legs[idx].Slot = side.Slot
	}
	for _, leg := range legs {
		if !sided[leg.LegId] {
			return nil, fmt.Errorf("leg %#x has no --side", leg.LegId)
		}
	}
	return legs, nil
}

const migrForm = "name:id:src_side:dst_side"

// parseMigrSpec reads the `--migr name:id:src_side:dst_side` of put-migr into
// the four members of the library's spec it names.
func parseMigrSpec(spec string) (plant.MigrSpec, error) {
	var out plant.MigrSpec
	parts, err := splitSpec(spec, 4, migrForm)
	if err != nil {
		return out, err
	}
	out.Name = strings.TrimSpace(parts[0])
	if out.Name == "" {
		return out, fmt.Errorf("%q: the migration name is empty", spec)
	}
	if out.Id, err = parseIdField(parts[1], spec, migrForm); err != nil {
		return out, err
	}
	if out.SrcSide, err = parseIdField(parts[2], spec, migrForm); err != nil {
		return out, err
	}
	if out.DstSide, err = parseIdField(parts[3], spec, migrForm); err != nil {
		return out, err
	}
	return out, nil
}

const nsForm = "id:idx:td_id"

// parseNsSpec reads one `--ns id:idx:td_id` of put-ss.
func parseNsSpec(spec string) (plant.NsSpec, error) {
	var out plant.NsSpec
	parts, err := splitSpec(spec, 3, nsForm)
	if err != nil {
		return out, err
	}
	if out.NsId, err = parseIdField(parts[0], spec, nsForm); err != nil {
		return out, err
	}
	if out.NsIdx, err = parseU32Field(parts[1], spec, nsForm); err != nil {
		return out, err
	}
	if out.TdId, err = parseIdField(parts[2], spec, nsForm); err != nil {
		return out, err
	}
	return out, nil
}

// parseU32List reads a `--slots 0,1,2` / `--thresholds p,c,s,l` list.
func parseU32List(spec string) ([]uint32, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]uint32, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseUint(strings.TrimSpace(part), 0, 32)
		if err != nil {
			return nil, fmt.Errorf("%q in %q: %w", part, spec, err)
		}
		out = append(out, uint32(value))
	}
	return out, nil
}

// parseHexBitmap reads the `--hex` payload of put-bitmap: an even number of
// hex digits, optionally 0x-prefixed. A bitmap chunk is opaque bytes to
// everything in dnv above the agent (BM3), so the test supplies it literally.
func parseHexBitmap(spec string) ([]byte, error) {
	trimmed := strings.TrimSpace(spec)
	trimmed = strings.TrimPrefix(trimmed, "0x")
	trimmed = strings.TrimPrefix(trimmed, "0X")
	if trimmed == "" {
		return nil, fmt.Errorf("the bitmap is empty")
	}
	if len(trimmed)%2 != 0 {
		return nil, fmt.Errorf(
			"%q has an odd number of hex digits", spec,
		)
	}
	data, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%q is not hex: %w", spec, err)
	}
	return data, nil
}

// parseSpLevel reads `--level`: either the number the suite's steps use
// (`set-level sp0 48`) or the enum name. Any other number is refused — an
// undefined level would be stored and then read back as a level no rule of
// architecture.md, Common validation, knows.
func parseSpLevel(spec string) (pb.SpLevel, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return 0, fmt.Errorf("--level is required")
	}
	if value, err := strconv.ParseInt(trimmed, 0, 32); err == nil {
		if _, ok := pb.SpLevel_name[int32(value)]; !ok {
			return 0, fmt.Errorf("%q is not a defined SpLevel", spec)
		}
		return pb.SpLevel(value), nil
	}
	name := strings.ToUpper(trimmed)
	if !strings.HasPrefix(name, "SP_LEVEL_") {
		name = "SP_LEVEL_" + name
	}
	value, ok := pb.SpLevel_value[name]
	if !ok {
		return 0, fmt.Errorf("%q is not a defined SpLevel", spec)
	}
	return pb.SpLevel(value), nil
}

// ---------------------------------------------------------------------------
// Keys (architecture.md, Key grammar and Key table)
// ---------------------------------------------------------------------------

// keyPrefix joins key fields per architecture.md, Key grammar, and ends in the
// separating space, so that a prefix can never match a longer sibling field.
// Every full key this driver reads comes from model; model exports no bare
// scan prefix for the kinds `list-keys` walks, so this mirrors the one grammar
// rule rather than hand-spelling each prefix.
func keyPrefix(fields ...string) string {
	return strings.Join(fields, " ") + " "
}

// kindInfo describes one row of the key table (architecture.md, Key table):
// the message a value of that kind decodes into (which is how `get` picks a
// type from a key's SECOND field) and whether {cluster_id} is the field right
// after the kind (which is how `list-keys` scopes a prefix to one cluster).
type kindInfo struct {
	newMsg        func() proto.Message
	clusterScoped bool
}

// kinds is the key table (architecture.md, Key table).
var kinds = map[string]kindInfo{
	"cluster_conf": {
		newMsg:        func() proto.Message { return &pb.ClusterConf{} },
		clusterScoped: false,
	},
	"dn_global": {
		newMsg:        func() proto.Message { return &pb.DnGlobal{} },
		clusterScoped: true,
	},
	"cn_global": {
		newMsg:        func() proto.Message { return &pb.CnGlobal{} },
		clusterScoped: true,
	},
	"sp_global": {
		newMsg:        func() proto.Message { return &pb.SpGlobal{} },
		clusterScoped: true,
	},
	// The three rev kinds carry {shard_code} before {cluster_id}, so their
	// prefix cannot be scoped to a cluster.
	"dn_rev": {
		newMsg:        func() proto.Message { return &pb.DnRev{} },
		clusterScoped: false,
	},
	"cn_rev": {
		newMsg:        func() proto.Message { return &pb.CnRev{} },
		clusterScoped: false,
	},
	"sp_rev": {
		newMsg:        func() proto.Message { return &pb.SpRev{} },
		clusterScoped: false,
	},
	"dn_conf": {
		newMsg:        func() proto.Message { return &pb.DnConf{} },
		clusterScoped: true,
	},
	"cn_conf": {
		newMsg:        func() proto.Message { return &pb.CnConf{} },
		clusterScoped: true,
	},
	"dn_capacity": {
		newMsg:        func() proto.Message { return &pb.DnCapacity{} },
		clusterScoped: true,
	},
	"cn_capacity": {
		newMsg:        func() proto.Message { return &pb.CnCapacity{} },
		clusterScoped: true,
	},
	"cdc": {
		newMsg:        func() proto.Message { return &pb.CdcEntry{} },
		clusterScoped: true,
	},
	"sp_conf": {
		newMsg:        func() proto.Message { return &pb.SpConf{} },
		clusterScoped: true,
	},
	"cntlr": {
		newMsg:        func() proto.Message { return &pb.Cntlr{} },
		clusterScoped: true,
	},
	"slice": {
		newMsg:        func() proto.Message { return &pb.Slice{} },
		clusterScoped: true,
	},
	"thin_device": {
		newMsg:        func() proto.Message { return &pb.ThinDevice{} },
		clusterScoped: true,
	},
	"subsystem": {
		newMsg:        func() proto.Message { return &pb.Subsystem{} },
		clusterScoped: true,
	},
	"clone": {
		newMsg:        func() proto.Message { return &pb.Clone{} },
		clusterScoped: true,
	},
	"clone_bitmap": {
		newMsg:        func() proto.Message { return &pb.CloneBitmap{} },
		clusterScoped: true,
	},
	"transfer": {
		newMsg:        func() proto.Message { return &pb.Transfer{} },
		clusterScoped: true,
	},
	"migration": {
		newMsg:        func() proto.Message { return &pb.Migration{} },
		clusterScoped: true,
	},
	"migration_bitmap": {
		newMsg:        func() proto.Message { return &pb.MigrBitmap{} },
		clusterScoped: true,
	},
	"sp_id_to_name": {
		newMsg:        func() proto.Message { return &pb.SpName{} },
		clusterScoped: true,
	},
	"worker": {
		newMsg:        func() proto.Message { return &pb.WorkerReg{} },
		clusterScoped: false,
	},
}

// messageForKey picks the message type of a key from its SECOND field, which
// is what `get --key` needs (architecture.md, Key table: every stored message
// has one key schema and the kind field names it).
func messageForKey(key string) (proto.Message, error) {
	fields := strings.Split(key, " ")
	if len(fields) < 3 || fields[0] != common.DnvPrefix {
		return nil, fmt.Errorf(
			"%q is not a dnv key (want %q as the first field)",
			key, common.DnvPrefix,
		)
	}
	info, ok := kinds[fields[1]]
	if !ok {
		return nil, fmt.Errorf("unknown key kind %q in %q", fields[1], key)
	}
	return info.newMsg(), nil
}

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

type globals struct {
	endpoints string
	cluster   string
	traceId   string
	timeout   float64
}

func newGlobals() globals {
	return globals{endpoints: defaultEndpoints, timeout: 10}
}

// bind registers the global flags. It is called twice — once on the top-level
// set, once on the subcommand's — with the current values as defaults, so that
// `workerctl --cluster c put-dn …` (worker_test.sh's `ctl` wrapper) and
// `workerctl put-dn --cluster c …` are both accepted and the later occurrence
// wins.
func (g *globals) bind(fs *flag.FlagSet) {
	fs.StringVar(&g.endpoints, "endpoints", g.endpoints,
		"comma-separated etcd client endpoints")
	fs.StringVar(&g.cluster, "cluster", g.cluster,
		"cluster NAME; the cluster_id is derived from it and the stored "+
			"creation_epoch (architecture.md, cluster_id derivation)")
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

// loadCluster reads the --cluster's ClusterConf through plant.LoadCluster,
// which derives the cluster_id from it exactly as the gateway must
// (architecture.md, cluster_id derivation): cluster_id is not computable from
// a name alone, so a subcommand that BUILDS a cluster-scoped key comes
// through here first. Seven subcommands never do: put-cluster (it writes the
// very ClusterConf this reads), get (it is handed a whole key and needs no
// id), list-workers (the worker registry is keyed by role and seed, not by
// cluster), ping, and the three etcd-free ones, constants, geometry and
// meta-units.
// list-keys is the single CONDITIONAL caller: it reads the ClusterConf when it
// was given a cluster-scoped kind name and a --cluster, and not when it was
// given a raw `dnv `-prefixed prefix. Every other subcommand reads it
// unconditionally.
func (g *globals) loadCluster(
	ctx context.Context,
	cli *etcdutil.Client,
) plant.Cluster {
	if g.cluster == "" {
		die("--cluster is required")
	}
	cl, err := plant.LoadCluster(ctx, cli, g.cluster)
	if err != nil {
		die("%v", err)
	}
	return cl
}

// resolveSp turns a `--sp` value, the SP NAME (SpConf is name-keyed) or the
// sp_id (the suite's steps use both, e.g. `set-lwm sp0` and
// `set-cntlr --sp 1`), into the library's reference (plant.ResolveSp).
func resolveSp(
	ctx context.Context,
	cli *etcdutil.Client,
	cl plant.Cluster,
	spec string,
) plant.SpRef {
	if strings.TrimSpace(spec) == "" {
		die("--sp is required")
	}
	target, err := plant.ResolveSp(ctx, cli, cl, spec)
	if err != nil {
		die("%v", err)
	}
	return target
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
	{"constants", cmdConstants},
	{"geometry", cmdGeometry},
	{"meta-units", cmdMetaUnits},
	{"put-cluster", cmdPutCluster},
	{"put-dn", cmdPutDn},
	{"put-cn", cmdPutCn},
	{"bump-rev", cmdBumpRev},
	{"move-dn", cmdMoveDn},
	{"del-rev", cmdDelRev},
	{"put-sp", cmdPutSp},
	{"put-td", cmdPutTd},
	{"put-ss", cmdPutSs},
	{"put-clone", cmdPutClone},
	{"put-xfer", cmdPutXfer},
	{"put-migr", cmdPutMigr},
	{"put-bitmap", cmdPutBitmap},
	{"set-cntlr", cmdSetCntlr},
	{"set-level", cmdSetLevel},
	{"set-lwm", cmdSetLwm},
	{"set-free", cmdSetFree},
	{"set-epoch", cmdSetEpoch},
	{"set-created", cmdSetCreated},
	{"set-provisioned", cmdSetProvisioned},
	{"start-cutover", cmdStartCutover},
	{"set-deleting", cmdSetDeleting},
	{"drain-sp", cmdDrainSp},
	{"set-clone-deleting", cmdSetCloneDeleting},
	{"drain-clone", cmdDrainClone},
	{"get", cmdGet},
	{"get-dn", cmdGetDn},
	{"get-cn", cmdGetCn},
	{"get-rev", cmdGetRev},
	{"get-sp", cmdGetSp},
	{"get-cntlr", cmdGetCntlr},
	{"get-slice", cmdGetSlice},
	{"get-td", cmdGetTd},
	{"list-keys", cmdListKeys},
	{"list-workers", cmdListWorkers},
}

func main() {
	// No logger setup: common's init() already puts the JSON records on
	// stderr, which is what keeps this driver's stdout the result channel —
	// one JSON document per invocation for most subcommands, one line per key
	// for list-keys — that the script pipes into jq or reads line by line.
	g := newGlobals()
	top := flag.NewFlagSet("workerctl", flag.ExitOnError)
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
		"usage: workerctl [global flags] <subcommand> [flags]\n"+
			"global flags: --endpoints --cluster --trace-id --timeout\n"+
			"subcommands: %s\n", strings.Join(names, " "))
}

func newFlagSet(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	g.bind(fs)
	return fs
}

// splitKind peels the `dn|cn|sp` positional argument of bump-rev / del-rev /
// get-rev / set-free, and the `cntlr|leg|side` one of set-epoch, off the
// argument list. It is accepted both before the flags (worker_test.sh's
// spelling) and after them, since Go's flag package stops at the first
// non-flag argument either way.
func splitKind(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// kindArg finishes what splitKind started, after the flags have been parsed.
func kindArg(fs *flag.FlagSet, kind string, want ...string) string {
	if kind == "" && fs.NArg() > 0 {
		kind = fs.Arg(0)
	}
	for _, candidate := range want {
		if kind == candidate {
			return kind
		}
	}
	usageDie("want one of %s, got %q", strings.Join(want, "|"), kind)
	return ""
}

// requireId dies unless an id flag was given and is not 0.
func requireId(id hexUint) uint64 {
	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	return uint64(id)
}

// ---------------------------------------------------------------------------
// ping
// ---------------------------------------------------------------------------

// cmdPing is the etcd liveness probe the suites' setup waits on: one Get of a
// key nothing writes, which succeeds on not-found.
func cmdPing(g *globals, args []string) {
	fs := newFlagSet("ping", g)
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()

	found, err := cli.Get(ctx, pingKey, &pb.ClusterConf{})
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
// constants, geometry, meta-units — the three subcommands that touch no etcd
// ---------------------------------------------------------------------------

// cmdConstants prints, as one JSON object, the common package's constants a
// shell suite reads instead of typing: the request size every etcd serving
// dnv is started with, transaction budgets and their factors, the slice
// ceiling, the health timers, a DN's metadata area, the strides of the id
// step and the cdc's pending cap and pre-Connect deadline. It exists because
// a shell suite cannot import common: every shell suite that starts an etcd
// has to launch it with --max-txn-ops=common.EtcdMaxTxnOps and
// --max-request-bytes=common.EtcdMaxRequestBytes (dnv-worker.md, Integration
// test plan, Topology), and reads the two numbers here instead of typing
// them.
//
// The keys are the Go IDENTIFIERS, not this driver's usual snake_case, so that
// one `grep EtcdMaxTxnOps` finds common/constants.go, this table and the shell
// that reads it. The table opens with the two etcd request caps,
// EtcdMaxTxnOps and EtcdMaxRequestBytes. Four more are the factors of the
// SPD13/CLD11 budgets a suite may want to size a case against, and
// MaxSliceCntPerSp is the slice ceiling — CreateClone's src_slice_cnt bound —
// that the gateway suite creates a clone at, which is why that suite must
// read it rather than type it. The next three are the timers a suite sizes
// its health and failover waits by: the default check interval a cluster
// created without one stores, the default primary threshold a pool created
// without one reads, and the demotion hold's wait (dnv-worker.md RW22). The
// next two are a DN's metadata area (architecture.md, Metadata units): the
// ceiling of a DN's used units and the unit. The gateway suite's ceiling case
// fills a DN's area to that ceiling with a migration it sizes in that unit.
// The next two, IdStepStride and ThinIdStepStride, are the strides of the id
// step (architecture.md, Clusters), which the gateway suite's smoke case
// checks every counter the step moves against. The last two are the cdc's
// pending cap and pre-Connect deadline (cdc.md NP1, NP10), which the cdc
// suite's flood case fills and waits by (cdc.md, Integration test plan).
//
// It dials nothing and needs no --cluster, so a suite runs it on the DRIVER
// against the binary it has just built, before any host is contacted.
func cmdConstants(g *globals, args []string) {
	fs := newFlagSet("constants", g)
	fs.Parse(args)

	emit(map[string]any{
		"EtcdMaxTxnOps":       common.EtcdMaxTxnOps,
		"EtcdMaxRequestBytes": common.EtcdMaxRequestBytes,

		"MaxDelGrpPerTxn":   common.MaxDelGrpPerTxn,
		"MaxAllocLegPerGrp": common.MaxAllocLegPerGrp,
		"MaxSpareLegPerGrp": common.MaxSpareLegPerGrp,
		"MaxDelBmPerTxn":    common.MaxDelBmPerTxn,
		"MaxSliceCntPerSp":  common.MaxSliceCntPerSp,

		"DefaultHealthCheckInterval": common.DefaultHealthCheckInterval,
		"DefaultPrimaryUnhealthy":    common.DefaultPrimaryUnhealthy,
		"DemotionHoldTimeout":        common.DemotionHoldTimeout,

		"DnCloneMetaUnitCnt": common.DnCloneMetaUnitCnt,
		"DnCloneMetaUnit":    common.DnCloneMetaUnit,

		"IdStepStride":     common.IdStepStride,
		"ThinIdStepStride": common.ThinIdStepStride,

		"CdcMaxPendingConnCnt":   common.CdcMaxPendingConnCnt,
		"DefaultCdcPendingTmoMs": common.DefaultCdcPendingTmoMs,
	})
}

// cmdGeometry prints the block geometry of ONE group (architecture.md, Group
// on-leg layout: meta region, data region, health block) — the meta_blocks
// and data_blocks a group of --ext-cnt extents gets — by calling
// model.GroupBlocks, the one implementation of that formula (MD6). The
// arithmetic is deliberately NOT repeated here: put-sp computes a group's
// geometry with the same call on a BdevConf assembled the same way, with the
// redund_conf of plant.RedundConf, so the two cannot disagree.
//
// It prints the length to zero (architecture.md, Side provisioning protocol)
// of a side of that group too, by calling model.SideZeroBytes, which the sp
// worker's side requests carry (dnv-worker.md RW15): meta_zero_bytes for a
// group of the slice's meta list, data_zero_bytes for one of its data list.
//
// The flag defaults are put-cluster's, because the conf this builds is the one
// put-cluster would store and put-sp would inherit: --block-size and
// --chunk-blocks are the cluster's bdev_conf, --extent-size its
// dn_bin_conf.extent_size, and --raid1 chooses the redund kind that put-sp's
// `--group …:raid1` would have chosen. low_water_mark_pct and stripe_size are
// not flags: ValidateBdevConf refuses them at zero, ResolveBdevConf fills both
// with their defaults, and neither enters the geometry.
//
// Like constants, it touches no etcd and runs on the driver.
func cmdGeometry(g *globals, args []string) {
	fs := newFlagSet("geometry", g)
	extentSize := fs.Uint64("extent-size", common.MinDnExtSize,
		"dn_bin_conf.extent_size in bytes")
	blockSize := fs.Uint64("block-size", common.DefaultDmPoolDataBlockSize,
		"bdev_conf.dm_pool_conf.data_block_size, the geometry's block_size")
	chunkBlocks := fs.Uint64("chunk-blocks", common.DefaultChunkBlockCnt,
		"redund_md_raid1.bitmap_chunk_block_cnt, the geometry's bitmap "+
			"chunk; ignored without --raid1")
	extCnt := fs.Uint64("ext-cnt", 0, "the group's ext_cnt (required)")
	raid1 := fs.Bool("raid1", false,
		"the group is md-raid1 (md superblock + bitmap + health block); "+
			"without it the group is redund_none and meta is the health "+
			"block alone")
	fs.Parse(args)

	if *extCnt == 0 {
		die("--ext-cnt is required and must not be 0")
	}
	bdev := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{DataBlockSize: *blockSize},
		RedundConf: plant.RedundConf(*raid1, *chunkBlocks),
	}
	bdev = model.ResolveBdevConf(bdev)
	metaBlocks, dataBlocks, err := model.GroupBlocks(*extCnt, *extentSize, bdev)
	if err != nil {
		die("geometry: %v", err)
	}
	grp := &pb.Group{
		ExtCnt:     *extCnt,
		MetaBlocks: metaBlocks,
		DataBlocks: dataBlocks,
	}
	metaZeroBytes, err := model.SideZeroBytes(
		grp, true, model.PoolBlockSize(bdev))
	if err != nil {
		die("geometry: %v", err)
	}
	dataZeroBytes, err := model.SideZeroBytes(
		grp, false, model.PoolBlockSize(bdev))
	if err != nil {
		die("geometry: %v", err)
	}
	emit(map[string]any{
		"ext_cnt":         *extCnt,
		"raid1":           *raid1,
		"meta_blocks":     metaBlocks,
		"data_blocks":     dataBlocks,
		"meta_zero_bytes": metaZeroBytes,
		"data_zero_bytes": dataZeroBytes,
	})
}

// cmdMetaUnits prints the units a DN's metadata area gives one copy
// (architecture.md, Metadata units) — what CreateMigration charges a
// destination of a side of --bytes, in a pool whose block size is
// --block-size — by calling common.CloneMetaUnits, the one implementation of
// the formula. A suite sizes its migrations by it and checks the units the
// gateway stored against it, rather than re-deriving either.
//
// --bytes is the moved group's ext_cnt × extent_size. --block-size is the
// pool's data_block_size and
// defaults to common.DefaultDmPoolDataBlockSize, as geometry's does. Zero is
// refused for both: no copy is empty and no stored pool has a zero block
// size, so a zero is a script that read nothing.
//
// Like constants, it touches no etcd and runs on the driver.
func cmdMetaUnits(g *globals, args []string) {
	fs := newFlagSet("meta-units", g)
	copyBytes := fs.Uint64("bytes", 0, "the bytes of the copy (required)")
	blockSize := fs.Uint64("block-size", common.DefaultDmPoolDataBlockSize,
		"bdev_conf.dm_pool_conf.data_block_size of the copy's pool")
	fs.Parse(args)

	units, err := metaUnitsOf(*copyBytes, *blockSize)
	if err != nil {
		die("meta-units: %v", err)
	}
	emit(map[string]any{
		"bytes":      *copyBytes,
		"block_size": *blockSize,
		"units":      units,
	})
}

// metaUnitsOf is meta-units' arithmetic and its two refusals, apart from the
// flags so that a test can see a refusal without the process exiting.
func metaUnitsOf(copyBytes uint64, blockSize uint64) (uint64, error) {
	if copyBytes == 0 {
		return 0, fmt.Errorf("--bytes is required and must not be 0")
	}
	if blockSize == 0 {
		return 0, fmt.Errorf("--block-size must not be 0")
	}
	return common.CloneMetaUnits(copyBytes, blockSize), nil
}

// ---------------------------------------------------------------------------
// put-cluster
// ---------------------------------------------------------------------------

// cmdPutCluster implements CreateCluster through plant.PutCluster
// (architecture.md, Clusters and cluster_id derivation). Its flags default to
// the extent size, block size, bitmap chunk and low-water mark a suite's
// cluster has unless the case says otherwise, and it prints them as given.
func cmdPutCluster(g *globals, args []string) {
	fs := newFlagSet("put-cluster", g)
	name := fs.String("name", "", "cluster name (defaults to --cluster)")
	dnInterval := fs.Uint("dn-interval", 0, "health_check_conf.dn_interval")
	cnInterval := fs.Uint("cn-interval", 0, "health_check_conf.cn_interval")
	sideInterval := fs.Uint("side-interval", 0,
		"health_check_conf.side_interval")
	cntlrInterval := fs.Uint("cntlr-interval", 0,
		"health_check_conf.cntlr_interval")
	extentSize := fs.Uint64("extent-size", common.MinDnExtSize,
		"dn_bin_conf.extent_size in bytes")
	lwm := fs.Uint("lwm", common.DefaultPoolLowWatermarkPct,
		"bdev_conf.dm_pool_conf.low_water_mark_pct")
	blockSize := fs.Uint64("block-size", common.DefaultDmPoolDataBlockSize,
		"bdev_conf.dm_pool_conf.data_block_size, the geometry's block_size")
	chunkBlocks := fs.Uint64("chunk-blocks", common.DefaultChunkBlockCnt,
		"redund_md_raid1.bitmap_chunk_block_cnt, the geometry's bitmap chunk")
	dnBatch := fs.Uint("dn-batch", 0, "alloc_conf.dn_batch_size")
	cnBatch := fs.Uint("cn-batch", 0, "alloc_conf.cn_batch_size")
	fs.Parse(args)

	clusterName := *name
	if clusterName == "" {
		clusterName = g.cluster
	}
	if clusterName == "" {
		die("--name (or --cluster) is required")
	}

	ctx, done, cli := g.open()
	defer done()

	cl, err := plant.PutCluster(ctx, cli, plant.ClusterSpec{
		Name:          clusterName,
		DnInterval:    uint32(*dnInterval),
		CnInterval:    uint32(*cnInterval),
		SideInterval:  uint32(*sideInterval),
		CntlrInterval: uint32(*cntlrInterval),
		ExtentSize:    *extentSize,
		Lwm:           uint32(*lwm),
		BlockSize:     *blockSize,
		ChunkBlocks:   *chunkBlocks,
		DnBatch:       uint32(*dnBatch),
		CnBatch:       uint32(*cnBatch),
	})
	if err != nil {
		die("put-cluster: %v", err)
	}
	emit(map[string]any{
		"cluster_name":       clusterName,
		"cluster_id":         idHex(cl.Id),
		"cluster_id_0x":      fmt.Sprintf("%#016x", cl.Id),
		"creation_epoch":     cl.Conf.GetCreationEpoch(),
		"extent_size":        *extentSize,
		"block_size":         *blockSize,
		"chunk_blocks":       *chunkBlocks,
		"low_water_mark_pct": uint32(*lwm),
	})
}

// ---------------------------------------------------------------------------
// put-dn / put-cn
// ---------------------------------------------------------------------------

// cmdPutDn implements CreateDiskNode with an explicit id, shard and budget
// through plant.PutDn (architecture.md, Disk nodes).
func cmdPutDn(g *globals, args []string) {
	fs := newFlagSet("put-dn", g)
	var id hexUint
	fs.Var(&id, "id", "dn_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	addr := fs.String("addr", "", "the dn agent's ip:port (required)")
	location := fs.String("location", "", "DnConf.location")
	totalExt := fs.Uint64("total-ext", 0,
		"total_ext_cnt (default: --free-ext, a node with nothing on it)")
	freeExt := fs.Uint64("free-ext", 0, "free_ext_cnt")
	disabled := fs.Bool("disabled", false, "DnConf.disabled")
	var sides stringList
	fs.Var(&sides, "side", "a side pointer, "+sidePtrForm+" (repeatable)")
	fs.Parse(args)

	spec := plant.DnSpec{
		Id:       requireId(id),
		Shard:    uint32(shard),
		Addr:     *addr,
		Location: *location,
		TotalExt: *totalExt,
		FreeExt:  *freeExt,
		Disabled: *disabled,
	}
	if spec.Addr == "" {
		die("--addr is required")
	}
	for _, text := range sides {
		ptr, err := parseSidePointer(text)
		if err != nil {
			die("--side: %v", err)
		}
		spec.Sides = append(spec.Sides, ptr)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	dn, err := plant.PutDn(ctx, cli, cl, spec)
	if err != nil {
		die("put-dn: %v", err)
	}
	emit(map[string]any{
		"dn_id":        idHex(dn.Conf.GetDnId()),
		"shard_code":   shardHex(dn.Conf.GetShardCode()),
		"addr_port":    dn.Addr,
		"revision":     dn.Revision,
		"free_ext_cnt": dn.Conf.GetFreeExtCnt(),
		"allocatable":  model.DnAllocatable(dn.Conf, cl.Conf.GetDnBinConf()),
	})
}

// cmdPutCn is cmdPutDn's mirror for a controller node through plant.PutCn
// (architecture.md, Controller nodes).
func cmdPutCn(g *globals, args []string) {
	fs := newFlagSet("put-cn", g)
	var id hexUint
	fs.Var(&id, "id", "cn_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	addr := fs.String("addr", "", "the cn agent's ip:port (required)")
	location := fs.String("location", "", "CnConf.location")
	totalExt := fs.Uint64("total-ext", 0,
		"total_ext_cnt (default: --free-ext, a node with nothing on it)")
	freeExt := fs.Uint64("free-ext", 0, "free_ext_cnt")
	disabled := fs.Bool("disabled", false, "CnConf.disabled")
	var cntlrs stringList
	fs.Var(&cntlrs, "cntlr", "a cntlr pointer, "+cntlrPtrForm+" (repeatable)")
	fs.Parse(args)

	spec := plant.CnSpec{
		Id:       requireId(id),
		Shard:    uint32(shard),
		Addr:     *addr,
		Location: *location,
		TotalExt: *totalExt,
		FreeExt:  *freeExt,
		Disabled: *disabled,
	}
	if spec.Addr == "" {
		die("--addr is required")
	}
	for _, text := range cntlrs {
		ptr, err := parseCntlrPointer(text)
		if err != nil {
			die("--cntlr: %v", err)
		}
		spec.Cntlrs = append(spec.Cntlrs, ptr)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	cn, err := plant.PutCn(ctx, cli, cl, spec)
	if err != nil {
		die("put-cn: %v", err)
	}
	emit(map[string]any{
		"cn_id":        idHex(cn.Conf.GetCnId()),
		"shard_code":   shardHex(cn.Conf.GetShardCode()),
		"addr_port":    cn.Addr,
		"revision":     cn.Revision,
		"free_ext_cnt": cn.Conf.GetFreeExtCnt(),
		"allocatable":  model.CnAllocatable(cn.Conf),
	})
}

// ---------------------------------------------------------------------------
// bump-rev / move-dn / del-rev
// ---------------------------------------------------------------------------

// cmdBumpRev raises one revision key in place through plant.BumpRev
// (architecture.md, Revision keys and the sync fan-out).
func cmdBumpRev(g *globals, args []string) {
	fs := newFlagSet("bump-rev", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id / sp_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn", "sp")
	objId := requireId(id)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	revision, err := plant.BumpRev(
		ctx, cli, cl, plant.RevKind(kind), uint32(shard), objId)
	if err != nil {
		die("bump-rev: %v", err)
	}
	emit(map[string]any{
		"kind":       kind,
		"id":         idHex(objId),
		"shard_code": shardHex(uint32(shard)),
		"revision":   revision,
	})
}

// cmdMoveDn is the moved-node case of architecture.md, Revision keys and the
// sync fan-out, through plant.MoveDn. The suite only moves a DN that carries
// no side (worker_test.sh case A step 2, before its SP exists).
func cmdMoveDn(g *globals, args []string) {
	fs := newFlagSet("move-dn", g)
	var id hexUint
	fs.Var(&id, "id", "dn_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	addr := fs.String("addr", "", "the NEW ip:port (required)")
	fs.Parse(args)

	dnId := requireId(id)
	if *addr == "" {
		die("--addr is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	oldAddr, revision, err := plant.MoveDn(
		ctx, cli, cl, dnId, uint32(shard), *addr)
	if err != nil {
		die("move-dn: %v", err)
	}
	emit(map[string]any{
		"dn_id":         idHex(dnId),
		"shard_code":    shardHex(uint32(shard)),
		"old_addr_port": oldAddr,
		"addr_port":     *addr,
		"revision":      revision,
	})
}

// cmdDelRev deletes one revision key and nothing else through plant.DelRev —
// the case of SW3 that worker_test.sh case A step 5 stages.
func cmdDelRev(g *globals, args []string) {
	fs := newFlagSet("del-rev", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id / sp_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn", "sp")
	objId := requireId(id)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	key, revision, err := plant.DelRev(
		ctx, cli, cl, plant.RevKind(kind), uint32(shard), objId)
	if err != nil {
		die("del-rev: %v", err)
	}
	emit(map[string]any{
		"kind":         kind,
		"id":           idHex(objId),
		"shard_code":   shardHex(uint32(shard)),
		"key":          key,
		"old_revision": revision,
	})
}

// ---------------------------------------------------------------------------
// put-sp
// ---------------------------------------------------------------------------

// cmdPutSp implements the CreateStoragePool STM with EXPLICIT PLACEMENT
// through plant.PutSp (dnv-worker.md, Integration test plan, The driver;
// architecture.md, Storage pools): the --cntlr/--slice/--group/--leg/--side
// flags are the five lists of plant.SpSpec, which name their parent by id.
func cmdPutSp(g *globals, args []string) {
	fs := newFlagSet("put-sp", g)
	name := fs.String("name", "", "sp_name (required)")
	var id hexUint
	fs.Var(&id, "id", "sp_id (required)")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	slotSpec := fs.String("slots", "0,1,2,3,4,5,6,7",
		"cntlid_slot_list, comma separated")
	levelSpec := fs.String("level", "0", "sp_level")
	thresholdSpec := fs.String("thresholds", "",
		"event_threshold as primary,cntlr,side,leg (0 = the default)")
	lwm := fs.Int("lwm", -1,
		"low_water_mark_pct (default: inherit from the cluster)")
	var cntlrSpecs, sliceSpecs, groupSpecs, legSpecs, sideSpecs stringList
	fs.Var(&cntlrSpecs, "cntlr", cntlrForm+" (repeatable)")
	fs.Var(&sliceSpecs, "slice", sliceForm+" (repeatable)")
	fs.Var(&groupSpecs, "group", groupForm+" (repeatable)")
	fs.Var(&legSpecs, "leg", legForm+" (repeatable)")
	fs.Var(&sideSpecs, "side", sideForm+" (repeatable)")
	fs.Parse(args)

	if *name == "" {
		die("--name is required")
	}
	spec := plant.SpSpec{
		Name:  *name,
		Id:    requireId(id),
		Shard: uint32(shard),
	}
	var err error
	if spec.Slots, err = parseU32List(*slotSpec); err != nil {
		die("--slots: %v", err)
	}
	if len(spec.Slots) == 0 {
		die("--slots must not be empty")
	}
	if spec.Level, err = parseSpLevel(*levelSpec); err != nil {
		die("--level: %v", err)
	}
	thresholdList, err := parseU32List(*thresholdSpec)
	if err != nil {
		die("--thresholds: %v", err)
	}
	if len(thresholdList) != 0 && len(thresholdList) != 4 {
		die("--thresholds wants four values, primary,cntlr,side,leg")
	}
	if len(thresholdList) == 4 {
		spec.Threshold = &pb.EventThreshold{
			PrimaryUnhealthy: thresholdList[0],
			CntlrUnhealthy:   thresholdList[1],
			SideUnhealthy:    thresholdList[2],
			LegUnhealthy:     thresholdList[3],
		}
	}
	if *lwm >= 0 {
		pct := uint32(*lwm)
		spec.Lwm = &pct
	}
	for _, text := range cntlrSpecs {
		parsed, err := parseCntlrSpec(text)
		if err != nil {
			die("put-sp: --cntlr: %v", err)
		}
		spec.Cntlrs = append(spec.Cntlrs, parsed)
	}
	for _, text := range sliceSpecs {
		parsed, err := parseSliceSpec(text)
		if err != nil {
			die("put-sp: --slice: %v", err)
		}
		spec.Slices = append(spec.Slices, parsed)
	}
	for _, text := range groupSpecs {
		parsed, err := parseGroupSpec(text)
		if err != nil {
			die("put-sp: --group: %v", err)
		}
		spec.Groups = append(spec.Groups, parsed)
	}
	for _, text := range legSpecs {
		parsed, err := parseLegSpec(text)
		if err != nil {
			die("put-sp: --leg: %v", err)
		}
		spec.Legs = append(spec.Legs, parsed)
	}
	for _, text := range sideSpecs {
		parsed, err := parseSideSpec(text)
		if err != nil {
			die("put-sp: --side: %v", err)
		}
		spec.Sides = append(spec.Sides, parsed)
	}
	if err := spec.Check(); err != nil {
		die("put-sp: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	sp, err := plant.PutSp(ctx, cli, cl, spec)
	if err != nil {
		die("put-sp: %v", err)
	}

	cntlrIds := make([]uint64, 0, len(spec.Cntlrs))
	for _, cntlr := range spec.Cntlrs {
		cntlrIds = append(cntlrIds, cntlr.CntlrId)
	}
	sliceIds := make([]uint64, 0, len(spec.Slices))
	for _, slice := range spec.Slices {
		sliceIds = append(sliceIds, slice.SliceId)
	}
	legIds := make([]uint64, 0, len(spec.Legs))
	for _, leg := range spec.Legs {
		legIds = append(legIds, leg.LegId)
	}
	sideIds := make([]uint64, 0, len(spec.Sides))
	for _, side := range spec.Sides {
		sideIds = append(sideIds, side.SideId)
	}
	grpReport := make([]any, 0, len(sp.Groups))
	for _, grp := range sp.Groups {
		kind := "data"
		if grp.Meta {
			kind = "meta"
		}
		grpReport = append(grpReport, map[string]any{
			"grp_id":      idHex(grp.GrpId),
			"slice_id":    idHex(grp.SliceId),
			"kind":        kind,
			"ext_cnt":     grp.ExtCnt,
			"meta_blocks": grp.MetaBlocks,
			"data_blocks": grp.DataBlocks,
		})
	}
	emit(map[string]any{
		"sp_id":         idHex(sp.Ref.Id),
		"sp_name":       sp.Ref.Name,
		"shard_code":    shardHex(sp.Ref.Shard),
		"next_id":       sp.NextId,
		"revision":      sp.Revision,
		"footprint":     sp.Footprint,
		"cntlr_id_list": idHexList(cntlrIds),
		"slice_id_list": idHexList(sliceIds),
		"leg_id_list":   idHexList(legIds),
		"side_id_list":  idHexList(sideIds),
		"groups":        grpReport,
		"dn_revisions":  sp.DnRevisions,
		"cn_revisions":  sp.CnRevisions,
	})
}

// ---------------------------------------------------------------------------
// SpConf sub-object writes
// ---------------------------------------------------------------------------

// cmdPutTd writes one ThinDevice through plant.PutTd (architecture.md, Thin
// devices: CreateThinDevice).
func cmdPutTd(g *globals, args []string) {
	fs := newFlagSet("put-td", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "td_name (required)")
	var id hexUint
	fs.Var(&id, "id", "td_id (required)")
	var devId hexUint
	fs.Var(&devId, "dev-id", "dev_id (default: SpConf.next_dev_id)")
	size := fs.Uint64("size", 0, "ThinDevice.size in bytes")
	fs.Parse(args)

	if *name == "" {
		die("--name is required")
	}
	spec := plant.TdSpec{
		Name:  *name,
		Id:    requireId(id),
		DevId: uint32(devId),
		Size:  *size,
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	td, err := plant.PutTd(ctx, cli, cl, target, spec)
	if err != nil {
		die("put-td: %v", err)
	}
	emit(map[string]any{
		"sp_name": target.Name,
		"td_name": td.Name,
		"td_id":   idHex(td.Id),
		"dev_id":  td.DevId,
		"sp_rev":  td.SpRev,
	})
}

// cmdPutSs writes one Subsystem and the SP's CdcEntry through plant.PutSs
// (architecture.md, Subsystems, namespaces: CreateSubsystem).
func cmdPutSs(g *globals, args []string) {
	fs := newFlagSet("put-ss", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	nqn := fs.String("nqn", "", "subsystem nqn (required)")
	var id hexUint
	fs.Var(&id, "id", "ss_id (required)")
	serial := fs.String("serial", "", "Subsystem.serial")
	model_ := fs.String("model", "", "Subsystem.model")
	var nsSpecs stringList
	fs.Var(&nsSpecs, "ns", "a namespace, "+nsForm+" (repeatable)")
	fs.Parse(args)

	if *nqn == "" {
		die("--nqn is required")
	}
	spec := plant.SsSpec{
		Nqn:    *nqn,
		Id:     requireId(id),
		Serial: *serial,
		Model:  *model_,
	}
	for _, text := range nsSpecs {
		parsed, err := parseNsSpec(text)
		if err != nil {
			die("--ns: %v", err)
		}
		spec.Namespaces = append(spec.Namespaces, parsed)
	}
	if err := spec.Check(); err != nil {
		die("put-ss: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	ss, err := plant.PutSs(ctx, cli, cl, target, spec)
	if err != nil {
		die("put-ss: %v", err)
	}
	emit(map[string]any{
		"sp_name": target.Name,
		"nqn":     ss.Nqn,
		"ss_id":   idHex(ss.Id),
		"ns_ids":  idHexList(ss.NsIds),
		"cdc_key": model.CdcEntryKey(cl.Id, target.Shard, target.Id, ss.Id),
		"sp_rev":  ss.SpRev,
	})
}

// cmdPutClone writes one Clone with its clone meta group and charges the
// group's extents to the DNs its --side flags name, through plant.PutClone
// (architecture.md, Clones). --dst-td accepts either a td name or a td_id. The
// group is required, as every clone CreateClone writes has one: --grp, and
// one --leg per leg with the --side that places it (cloneLegsOf). The legs
// are active ones; a spare a Go caller plants through the library.
func cmdPutClone(g *globals, args []string) {
	fs := newFlagSet("put-clone", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "clone_name (required)")
	var id, grpId hexUint
	fs.Var(&id, "id", "clone_id (required)")
	fs.Var(&grpId, "grp", "grp_id of the clone meta group (required)")
	dstTd := fs.String("dst-td", "", "destination td name or td_id (required)")
	srcSliceCnt := fs.Uint("src-slice-cnt", 0, "Clone.src_slice_cnt")
	srcNqn := fs.String("src-nqn", "", "Clone.src_nqn")
	srcNsIdx := fs.Uint("src-ns-idx", 0, "Clone.src_ns_idx")
	srcStripe := fs.Uint64("src-stripe-size", 0, "Clone.src_stripe_size")
	srcBlock := fs.Uint64("src-block-size", 0, "Clone.src_block_size")
	var legSpecs, sideSpecs stringList
	fs.Var(&legSpecs, "leg", cloneLegForm+
		" of the clone meta group (repeatable, at least one)")
	fs.Var(&sideSpecs, "side", sideForm+" of each --leg (one per leg)")
	fs.Parse(args)

	if *name == "" {
		die("--name is required")
	}
	cloneId := requireId(id)
	if *dstTd == "" {
		die("--dst-td is required")
	}
	if uint64(grpId) == 0 {
		die("--grp is required and must not be 0")
	}
	if len(legSpecs) == 0 {
		die("put-clone: at least one --leg is required")
	}
	legs, err := cloneLegsOf(legSpecs, sideSpecs)
	if err != nil {
		die("put-clone: %v", err)
	}
	spec := plant.CloneSpec{
		Name:          *name,
		Id:            cloneId,
		DstTd:         *dstTd,
		SrcSliceCnt:   uint32(*srcSliceCnt),
		SrcNqn:        *srcNqn,
		SrcNsIdx:      uint32(*srcNsIdx),
		SrcStripeSize: *srcStripe,
		SrcBlockSize:  *srcBlock,
		GrpId:         uint64(grpId),
		Legs:          legs,
	}
	if err := spec.Check(); err != nil {
		die("put-clone: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	clone, err := plant.PutClone(ctx, cli, cl, target, spec)
	if err != nil {
		die("put-clone: %v", err)
	}
	emit(map[string]any{
		"sp_name":      target.Name,
		"clone_name":   clone.Name,
		"clone_id":     idHex(clone.Id),
		"dst_td_id":    idHex(clone.DstTdId),
		"grp_id":       idHex(clone.GrpId),
		"leg_id_list":  idHexList(clone.LegIds),
		"side_id_list": idHexList(clone.SideIds),
		"meta_blocks":  clone.MetaBlocks,
		"data_blocks":  clone.DataBlocks,
		"dn_revisions": clone.DnRevisions,
		"sp_rev":       clone.SpRev,
	})
}

// cmdPutXfer writes one Transfer through plant.PutXfer (architecture.md,
// Transfers).
func cmdPutXfer(g *globals, args []string) {
	fs := newFlagSet("put-xfer", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "xfer_name (required)")
	var id hexUint
	fs.Var(&id, "id", "xfer_id (required)")
	oriNqn := fs.String("ori-nqn", "", "Transfer.ori_nqn (required)")
	oriNsIdx := fs.Uint("ori-ns-idx", 0, "Transfer.ori_ns_idx")
	fs.Parse(args)

	if *name == "" {
		die("--name is required")
	}
	xferId := requireId(id)
	if *oriNqn == "" {
		die("--ori-nqn is required")
	}
	spec := plant.XferSpec{
		Name:     *name,
		Id:       xferId,
		OriNqn:   *oriNqn,
		OriNsIdx: uint32(*oriNsIdx),
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	xfer, err := plant.PutXfer(ctx, cli, cl, target, spec)
	if err != nil {
		die("put-xfer: %v", err)
	}
	emit(map[string]any{
		"sp_name":   target.Name,
		"xfer_name": xfer.Name,
		"xfer_id":   idHex(xfer.Id),
		"sp_rev":    xfer.SpRev,
	})
}

// cmdPutMigr writes one Migration and its destination side through
// plant.PutMigr (architecture.md, Migrations; Metadata units).
func cmdPutMigr(g *globals, args []string) {
	fs := newFlagSet("put-migr", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	migr := fs.String("migr", "", migrForm+" (required)")
	var dstDn hexUint
	fs.Var(&dstDn, "dst-dn", "dn_id hosting the destination side (required)")
	dstSlot := fs.Uint("dst-slot", 0, "the destination side's cntlid_slot")
	pending := fs.Bool("cutover-pending", false,
		"write the record with cutover_pending set, as CreateMigration does")
	fs.Parse(args)

	spec, err := parseMigrSpec(*migr)
	if err != nil {
		die("--migr: %v", err)
	}
	if spec.Id == 0 || spec.SrcSide == 0 || spec.DstSide == 0 {
		die("--migr %q: ids must not be 0", *migr)
	}
	if uint64(dstDn) == 0 {
		die("--dst-dn is required and must not be 0")
	}
	spec.DstDn = uint64(dstDn)
	spec.DstSlot = uint32(*dstSlot)
	spec.CutoverPending = *pending

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	out, err := plant.PutMigr(ctx, cli, cl, target, spec)
	if err != nil {
		die("put-migr: %v", err)
	}
	emit(map[string]any{
		"sp_name":         target.Name,
		"migr_name":       out.Name,
		"migr_id":         idHex(out.Id),
		"src_side_id":     idHex(out.SrcSide),
		"dst_side_id":     idHex(out.DstSide),
		"leg_id":          idHex(out.LegId),
		"grp_id":          idHex(out.GrpId),
		"slice_id":        idHex(out.SliceId),
		"dn_id":           idHex(out.DstDn),
		"addr_port":       out.Addr,
		"ext_cnt":         out.ExtCnt,
		"meta_units":      out.MetaUnits,
		"cutover_pending": out.CutoverPending,
		"dn_revision":     out.DnRevision,
		"sp_rev":          out.SpRev,
	})
}

// cmdPutBitmap writes one chunk of a clone's or a migration's bitmap through
// plant.PutCloneBitmap or plant.PutMigrBitmap (architecture.md, Clones and
// Migrations; BM3).
func cmdPutBitmap(g *globals, args []string) {
	fs := newFlagSet("put-bitmap", g)
	kind := fs.String("kind", "", "clone|migr (required)")
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "clone_name or migr_name (required)")
	srcSliceIdx := fs.Uint("src-slice-idx", 0,
		"the source slice the chunk describes (--kind clone only)")
	bmIdx := fs.Uint("bm-idx", 0, "the chunk index")
	hexData := fs.String("hex", "", "the chunk payload as hex (required)")
	fs.Parse(args)

	if *kind != "clone" && *kind != "migr" {
		usageDie("--kind wants clone or migr, got %q", *kind)
	}
	if *kind == "migr" && *srcSliceIdx != 0 {
		// A migration's chunks address no slice at all, so a non-zero
		// --src-slice-idx is a mistake in the script rather than a chunk
		// this command could write.
		usageDie("--src-slice-idx %d is a clone address; --kind migr has none",
			*srcSliceIdx)
	}
	if *name == "" {
		die("--name is required")
	}
	// The flags are wider than the library's indexes, so their bound is
	// checked here, before the conversion could wrap: a chunk key field
	// holds two hex digits (model.ParseCloneBmKey, model.ParseBmIdx).
	if *srcSliceIdx >= 1<<8 {
		die("--src-slice-idx %d does not fit in the %q key field",
			*srcSliceIdx, common.BmIdxFmt)
	}
	if *bmIdx >= 1<<8 {
		die("--bm-idx %d does not fit in the %q key field",
			*bmIdx, common.BmIdxFmt)
	}
	data, err := parseHexBitmap(*hexData)
	if err != nil {
		die("--hex: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	var bitmap plant.Bitmap
	if *kind == "clone" {
		bitmap, err = plant.PutCloneBitmap(ctx, cli, cl, target,
			plant.CloneBitmapSpec{
				Clone:       *name,
				SrcSliceIdx: uint32(*srcSliceIdx),
				BmIdx:       uint32(*bmIdx),
				Data:        data,
			})
	} else {
		bitmap, err = plant.PutMigrBitmap(ctx, cli, cl, target,
			plant.MigrBitmapSpec{
				Migr:  *name,
				BmIdx: uint32(*bmIdx),
				Data:  data,
			})
	}
	if err != nil {
		die("put-bitmap: %v", err)
	}
	out := map[string]any{
		"sp_name": target.Name,
		"kind":    *kind,
		"name":    *name,
		// The pair a clone chunk was written at; 0 for a migration, whose
		// chunk addresses no slice (model.BmChunk).
		"src_slice_idx": uint32(*srcSliceIdx),
		"bm_idx":        uint32(*bmIdx),
		"bytes":         len(data),
		"created":       bitmap.Created,
		"sp_rev":        bitmap.SpRev,
	}
	// bm_cnt is Migration's only: a Clone record carries no chunk count.
	if *kind == "migr" {
		out["bm_cnt"] = bitmap.BmCnt
	}
	emit(out)
}

// ---------------------------------------------------------------------------
// set-*
// ---------------------------------------------------------------------------

// cmdSetCntlr rewrites one Cntlr's primary / disabled flags through
// plant.SetCntlr. The two flags are independently settable and a tri-state,
// so that "not given" differs from "false".
func cmdSetCntlr(g *globals, args []string) {
	fs := newFlagSet("set-cntlr", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	var id hexUint
	fs.Var(&id, "id", "cntlr_id (required)")
	primary := &triBool{}
	fs.Var(primary, "primary", "Cntlr.primary")
	disabled := &triBool{}
	fs.Var(disabled, "disabled", "Cntlr.disabled")
	fs.Parse(args)

	cntlrId := requireId(id)
	if !primary.set && !disabled.set {
		die("at least one of --primary / --disabled is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	stored, spRev, err := plant.SetCntlr(ctx, cli, cl, target, cntlrId,
		plant.CntlrFlags{Primary: primary.ptr(), Disabled: disabled.ptr()})
	if err != nil {
		die("set-cntlr: %v", err)
	}
	emit(map[string]any{
		"sp_name":  target.Name,
		"cntlr_id": idHex(cntlrId),
		"cntlr":    pbToAny(stored),
		"sp_rev":   spRev,
	})
}

// cmdSetLevel writes SpConf.sp_level through plant.SetLevel (architecture.md,
// Storage pools: UpdateStoragePoolLevel).
func cmdSetLevel(g *globals, args []string) {
	fs := newFlagSet("set-level", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	levelSpec := fs.String("level", "", "sp_level, a number or a name")
	fs.Parse(args)

	level, err := parseSpLevel(*levelSpec)
	if err != nil {
		die("--level: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	spRev, err := plant.SetLevel(ctx, cli, cl, target, level)
	if err != nil {
		die("set-level: %v", err)
	}
	emit(map[string]any{
		"sp_name":  target.Name,
		"sp_level": level.String(),
		"level":    int32(level),
		"sp_rev":   spRev,
	})
}

// cmdSetLwm writes the pool's low_water_mark_pct through plant.SetLwm: the
// lever case D5/D6 pulls to arm and disarm the AR6 grow rule.
func cmdSetLwm(g *globals, args []string) {
	fs := newFlagSet("set-lwm", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	pct := fs.Uint("pct", 0, "low_water_mark_pct")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	stored, spRev, err := plant.SetLwm(ctx, cli, cl, target, uint32(*pct))
	if err != nil {
		die("set-lwm: %v", err)
	}
	emit(map[string]any{
		"sp_name":            target.Name,
		"low_water_mark_pct": stored,
		"sp_rev":             spRev,
	})
}

// cmdSetFree rewrites a node's free_ext_cnt through plant.SetDnFree or
// plant.SetCnFree — the script's lever for deterministic allocation, which
// bumps no revision.
func cmdSetFree(g *globals, args []string) {
	fs := newFlagSet("set-free", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id")
	freeExt := fs.Uint64("free-ext", 0, "the new free_ext_cnt")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn")
	nodeId := requireId(id)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	allocatable := false
	addr := ""
	if kind == "dn" {
		var dn *pb.DnConf
		var err error
		addr, dn, err = plant.SetDnFree(ctx, cli, cl, nodeId, *freeExt)
		if err != nil {
			die("set-free: %v", err)
		}
		allocatable = model.DnAllocatable(dn, cl.Conf.GetDnBinConf())
	} else {
		var cn *pb.CnConf
		var err error
		addr, cn, err = plant.SetCnFree(ctx, cli, cl, nodeId, *freeExt)
		if err != nil {
			die("set-free: %v", err)
		}
		allocatable = model.CnAllocatable(cn)
	}
	emit(map[string]any{
		"kind":         kind,
		"id":           idHex(nodeId),
		"addr_port":    addr,
		"free_ext_cnt": *freeExt,
		"allocatable":  allocatable,
	})
}

// cmdSetEpoch plants one err_epoch, a Cntlr's or a Leg's or a Side's, through
// plant.SetCntlrEpoch, plant.SetLegEpoch or plant.SetSideEpoch, or, with
// --clone, a leg's or a side's of that clone's clone meta group through
// plant.SetCloneLegEpoch or plant.SetCloneSideEpoch: set --age seconds before
// now, or cleared with --clear. Now is the clock of the host it runs on,
// which in the suites is the test server, whose clock the workers read too.
// The library call is raw: it writes the epoch and nothing else
// (dnv-worker.md, Integration test plan, The driver).
func cmdSetEpoch(g *globals, args []string) {
	fs := newFlagSet("set-epoch", g)
	kind, rest := splitKind(args)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	var id hexUint
	fs.Var(&id, "id", "cntlr_id, leg_id or side_id (required)")
	var sliceId hexUint
	fs.Var(&sliceId, "slice", "slice_id of the leg or the side")
	cloneName := fs.String("clone", "",
		"clone_name whose clone meta group holds the leg or the side")
	age := fs.Uint64("age", 0, "seconds before now the err_epoch is set to")
	clearEpoch := fs.Bool("clear", false, "set the err_epoch to 0 instead")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "cntlr", "leg", "side")

	objId := requireId(id)
	if err := epochHolder(kind, uint64(sliceId), *cloneName); err != nil {
		die("set-epoch: %v", err)
	}
	ageGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "age" {
			ageGiven = true
		}
	})
	epoch, err := plantedEpoch(
		uint64(time.Now().Unix()), *age, ageGiven, *clearEpoch,
	)
	if err != nil {
		die("set-epoch: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	var old uint64
	switch {
	case kind == "cntlr":
		old, err = plant.SetCntlrEpoch(ctx, cli, cl, target, objId, epoch)
	case kind == "leg" && *cloneName != "":
		old, err = plant.SetCloneLegEpoch(
			ctx, cli, cl, target, *cloneName, objId, epoch)
	case kind == "leg":
		old, err = plant.SetLegEpoch(
			ctx, cli, cl, target, uint64(sliceId), objId, epoch)
	case *cloneName != "":
		old, err = plant.SetCloneSideEpoch(
			ctx, cli, cl, target, *cloneName, objId, epoch)
	default:
		old, err = plant.SetSideEpoch(
			ctx, cli, cl, target, uint64(sliceId), objId, epoch)
	}
	if err != nil {
		die("set-epoch: %v", err)
	}
	emit(map[string]any{
		"sp_name":       target.Name,
		"kind":          kind,
		"id":            idHex(objId),
		"slice_id":      idHex(uint64(sliceId)),
		"clone_name":    *cloneName,
		"err_epoch":     epoch,
		"old_err_epoch": old,
	})
}

// epochHolder checks that set-epoch names where its object lives: a cntlr
// by its id alone, a leg or a side by exactly one of --slice and --clone,
// the slice or the clone whose group holds it.
func epochHolder(kind string, sliceId uint64, cloneName string) error {
	if kind == "cntlr" {
		if sliceId != 0 || cloneName != "" {
			return fmt.Errorf("--slice and --clone are refused for a cntlr")
		}
		return nil
	}
	if (sliceId == 0) == (cloneName == "") {
		return fmt.Errorf("a %s takes exactly one of --slice and --clone",
			kind)
	}
	return nil
}

// plantedEpoch is the err_epoch set-epoch writes: age seconds before now, or
// 0 when clearEpoch asks for it. Exactly one of the two must be given. An
// err_epoch is a unix second and 0 reads as healthy, so an age that reaches
// back to the epoch itself is refused rather than written as a clear.
func plantedEpoch(
	now uint64, age uint64, ageGiven bool, clearEpoch bool,
) (uint64, error) {
	switch {
	case ageGiven && clearEpoch:
		return 0, fmt.Errorf("--age and --clear exclude each other")
	case clearEpoch:
		return 0, nil
	case !ageGiven:
		return 0, fmt.Errorf("one of --age and --clear is required")
	case age >= now:
		return 0, fmt.Errorf("--age %d reaches back to the unix epoch", age)
	}
	return now - age, nil
}

// ---------------------------------------------------------------------------
// Playing the worker (gateway.md, Integration test plan, The etcd verification)
// ---------------------------------------------------------------------------
//
// set-created, set-provisioned and start-cutover, with the drains below, are
// the ONLY writes the gateway integration suite performs through workerctl
// (gateway.md, Integration test plan, The etcd verification). Each is a
// library call that goes through the very model op the worker calls, except
// set-provisioned --clone, which goes through that op's clone-keyed twin
// (dnv-worker.md MD6).

// cmdSetCreated flips ThinDevice.created through plant.SetCreated
// (dnv-worker.md RW19).
func cmdSetCreated(g *globals, args []string) {
	fs := newFlagSet("set-created", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "td_name (required)")
	fs.Parse(args)

	if strings.TrimSpace(*name) == "" {
		usageDie("--name is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	tdId, flipped, spRev, err := plant.SetCreated(ctx, cli, cl, target, *name)
	if err != nil {
		die("set-created: %v", err)
	}
	emit(map[string]any{
		"td_name": *name,
		"td_id":   idHex(tdId),
		"flipped": flipped,
		"sp_rev":  spRev,
	})
}

// cmdSetProvisioned flips one Side.provisioned through plant.SetProvisioned
// (architecture.md, Side provisioning protocol), or, with --clone in place of
// --slice, one side of that clone's clone meta group through
// plant.SetCloneProvisioned, the clone-keyed twin of the flip (dnv-worker.md
// MD6).
func cmdSetProvisioned(g *globals, args []string) {
	fs := newFlagSet("set-provisioned", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	var slice, leg, side hexUint
	fs.Var(&slice, "slice", "slice_id (required without --clone)")
	cloneName := fs.String("clone", "",
		"clone_name whose clone meta group holds the side, in place of "+
			"--slice")
	fs.Var(&leg, "leg", "leg_id (required)")
	fs.Var(&side, "side", "side_id (required)")
	fs.Parse(args)

	if (uint64(slice) == 0) == (*cloneName == "") {
		usageDie("set-provisioned: exactly one of --slice and --clone is " +
			"required")
	}
	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	if *cloneName != "" {
		cloneId, flipped, spRev, err := plant.SetCloneProvisioned(
			ctx, cli, cl, target, *cloneName, uint64(leg), uint64(side))
		if err != nil {
			die("set-provisioned: %v", err)
		}
		emit(map[string]any{
			"clone_name": *cloneName,
			"clone_id":   idHex(cloneId),
			"leg_id":     idHex(uint64(leg)),
			"side_id":    idHex(uint64(side)),
			"flipped":    flipped,
			"sp_rev":     spRev,
		})
		return
	}
	ref := model.SideRef{
		SliceId: uint64(slice),
		LegId:   uint64(leg),
		SideId:  uint64(side),
	}
	flipped, spRev, err := plant.SetProvisioned(ctx, cli, cl, target, ref)
	if err != nil {
		die("set-provisioned: %v", err)
	}
	emit(map[string]any{
		"slice_id": idHex(ref.SliceId),
		"leg_id":   idHex(ref.LegId),
		"side_id":  idHex(ref.SideId),
		"flipped":  flipped,
		"sp_rev":   spRev,
	})
}

// cmdStartCutover clears one Migration.cutover_pending through
// plant.StartCutover (dnv-worker.md RW18): the cntlr the op checks is the
// pool's stored primary unless --cntlr names one.
func cmdStartCutover(g *globals, args []string) {
	fs := newFlagSet("start-cutover", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "migr_name (required)")
	var cntlr hexUint
	fs.Var(&cntlr, "cntlr",
		"cntlr_id the op checks is the primary (default: the stored primary)")
	fs.Parse(args)

	if strings.TrimSpace(*name) == "" {
		usageDie("--name is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	cutover, err := plant.StartCutover(
		ctx, cli, cl, target, *name, uint64(cntlr))
	if err != nil {
		die("start-cutover: %v", err)
	}
	ref := cutover.Ref
	emit(map[string]any{
		"migr_name":   ref.MigrName,
		"migr_id":     idHex(ref.MigrId),
		"slice_id":    idHex(ref.SliceId),
		"leg_id":      idHex(ref.LegId),
		"src_side_id": idHex(ref.SrcSideId),
		"dst_side_id": idHex(ref.DstSideId),
		"cntlr_id":    idHex(cutover.CntlrId),
		"sp_rev":      cutover.SpRev,
	})
}

// cmdSetDeleting latches an SP the way DeleteStoragePool does, through
// plant.SetDeleting: the worker suite runs no gateway, so this is how a case
// puts an SP into the state the sp coordinator's drain reacts to.
func cmdSetDeleting(g *globals, args []string) {
	fs := newFlagSet("set-deleting", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	latch, err := plant.SetDeleting(ctx, cli, cl, target)
	if err != nil {
		die("set-deleting: %v", err)
	}
	emit(map[string]any{
		"sp_name": target.Name,
		"sp_id":   idHex(target.Id),
		"latched": latch.Latched,
		"sp_rev":  latch.SpRev,
	})
}

// cmdDrainSp runs a latched SP's drain through plant.DrainSp. The worker
// suite drives exactly ONE batch through this, with --max-steps 1, to BUILD a
// partially drained SP; the gateway suite drains to the end.
func cmdDrainSp(g *globals, args []string) {
	fs := newFlagSet("drain-sp", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	// --max-steps is a real argument and not only a runaway guard: a case that
	// needs a PARTIALLY drained SP — to prove a restarted coordinator resumes
	// one (SPD8) — asks for exactly as many steps as it wants. Running out is
	// therefore reported in `sp_deleted`, never fatal; 4096 is above any shape
	// the ceilings of architecture.md, Cardinality limits, allow
	// (1 + MaxSliceCntPerSp x batches + 1).
	maxSteps := fs.Int("max-steps", 4096,
		"run at most this many drain steps")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	drain, err := plant.DrainSp(ctx, cli, cl, target, *maxSteps)
	if err != nil {
		die("drain-sp: %v", err)
	}
	emit(map[string]any{
		"sp_name":    target.Name,
		"sp_id":      idHex(target.Id),
		"steps":      drain.Steps,
		"cntlr_cnt":  drain.CntlrCnt,
		"grp_cnt":    drain.GrpCnt,
		"slice_cnt":  drain.SliceCnt,
		"sp_deleted": drain.Deleted,
	})
}

// cmdSetCloneDeleting latches one clone the way DeleteClone does, through
// plant.SetCloneDeleting (CLD4).
func cmdSetCloneDeleting(g *globals, args []string) {
	fs := newFlagSet("set-clone-deleting", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "clone_name (required)")
	fs.Parse(args)

	if strings.TrimSpace(*name) == "" {
		usageDie("--name is required")
	}
	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	latch, err := plant.SetCloneDeleting(ctx, cli, cl, target, *name)
	if err != nil {
		die("set-clone-deleting: %v", err)
	}
	emit(map[string]any{
		"sp_name":    target.Name,
		"clone_name": *name,
		"clone_id":   idHex(latch.Id),
		"latched":    latch.Latched,
		"sp_rev":     latch.SpRev,
	})
}

// cmdDrainClone runs a latched clone's drain through plant.DrainClone (CLD7).
// The worker suite drives exactly ONE batch through it, with --max-steps 1,
// to BUILD case G's restart fixture.
func cmdDrainClone(g *globals, args []string) {
	fs := newFlagSet("drain-clone", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "clone_name (required)")
	maxSteps := fs.Int("max-steps", 4096, "run at most this many steps")
	fs.Parse(args)

	if strings.TrimSpace(*name) == "" {
		usageDie("--name is required")
	}
	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	drain, err := plant.DrainClone(ctx, cli, cl, target, *name, *maxSteps)
	if err != nil {
		die("drain-clone: %v", err)
	}
	emit(map[string]any{
		"sp_name":    target.Name,
		"clone_name": *name,
		"steps":      drain.Steps,
		// chunk_cnt is the number of chunk KEYS this drain removed. The Clone
		// record carries no count of its own to confuse it with.
		"chunk_cnt":     drain.ChunkCnt,
		"clone_deleted": drain.Deleted,
		// grp_side_cnt is the sides of the clone meta group this drain's
		// final step gave back, 0 when it ran none.
		"grp_side_cnt": drain.GrpSideCnt,
	})
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// cmdGet reads any key and prints it as protojson, choosing the message type
// from the key's SECOND field, the kind that follows the prefix in the key
// column of architecture.md, Key table.
func cmdGet(g *globals, args []string) {
	fs := newFlagSet("get", g)
	key := fs.String("key", "", "the full key (required)")
	fs.Parse(args)

	if *key == "" {
		die("--key is required")
	}
	msg, err := messageForKey(*key)
	if err != nil {
		die("%v", err)
	}

	ctx, done, cli := g.open()
	defer done()

	found, err := cli.Get(ctx, *key, msg)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("key %q not found", *key)
	}
	emitPb(msg)
}

// cmdGetDn prints one DnConf, addressed by dn_id (or directly by --addr).
func cmdGetDn(g *globals, args []string) {
	fs := newFlagSet("get-dn", g)
	var id hexUint
	fs.Var(&id, "id", "dn_id")
	addr := fs.String("addr", "", "the dn's ip:port (skips the id lookup)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	endpoint := *addr
	if endpoint == "" {
		if uint64(id) == 0 {
			die("--id or --addr is required")
		}
		var err error
		if endpoint, err = plant.DnAddr(ctx, cli, cl, uint64(id)); err != nil {
			die("%v", err)
		}
	}
	dn := &pb.DnConf{}
	found, err := cli.Get(ctx, model.DnConfKey(cl.Id, endpoint), dn)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("no dn at %q", endpoint)
	}
	emitPb(dn)
}

// cmdGetCn prints one CnConf, addressed by cn_id (or directly by --addr).
func cmdGetCn(g *globals, args []string) {
	fs := newFlagSet("get-cn", g)
	var id hexUint
	fs.Var(&id, "id", "cn_id")
	addr := fs.String("addr", "", "the cn's ip:port (skips the id lookup)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	endpoint := *addr
	if endpoint == "" {
		if uint64(id) == 0 {
			die("--id or --addr is required")
		}
		var err error
		if endpoint, err = plant.CnAddr(ctx, cli, cl, uint64(id)); err != nil {
			die("%v", err)
		}
	}
	cn := &pb.CnConf{}
	found, err := cli.Get(ctx, model.CnConfKey(cl.Id, endpoint), cn)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("no cn at %q", endpoint)
	}
	emitPb(cn)
}

// cmdGetRev prints one revision key.
func cmdGetRev(g *globals, args []string) {
	fs := newFlagSet("get-rev", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id / sp_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn", "sp")
	objId := requireId(id)
	shardCode := uint32(shard)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)

	var key string
	var msg proto.Message
	switch kind {
	case "dn":
		key, msg = model.DnRevKey(shardCode, cl.Id, objId), &pb.DnRev{}
	case "cn":
		key, msg = model.CnRevKey(shardCode, cl.Id, objId), &pb.CnRev{}
	case "sp":
		key, msg = model.SpRevKey(shardCode, cl.Id, objId), &pb.SpRev{}
	}
	found, err := cli.Get(ctx, key, msg)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("key %q not found", key)
	}
	emitPb(msg)
}

// cmdGetSp prints an SP's whole desired state — SpConf plus every Cntlr, every
// Slice and every td — read at ONE store revision through model.LoadSp (MD3),
// as a single JSON object whose members jq can address. The subsystems,
// clones, transfers, migrations and bitmap indexes of the same snapshot come
// along, since LoadSp already read them.
func cmdGetSp(g *globals, args []string) {
	fs := newFlagSet("get-sp", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "alias of --sp")
	fs.Parse(args)

	spec := *sp
	if spec == "" {
		spec = *name
	}

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, spec)

	state, err := model.LoadSp(ctx, cli, cl.Id, target.Name)
	if err != nil {
		die("get-sp: %v", err)
	}
	cntlrs := make(map[string]any, len(state.Cntlrs))
	for cntlrId, cntlr := range state.Cntlrs {
		cntlrs[idHex(cntlrId)] = pbToAny(cntlr)
	}
	slices := make(map[string]any, len(state.Slices))
	for sliceId, slice := range state.Slices {
		slices[idHex(sliceId)] = pbToAny(slice)
	}
	tds := make(map[string]any, len(state.Tds))
	for i, td := range state.Tds {
		tds[state.TdNames[i]] = pbToAny(td)
	}
	subsystems := make(map[string]any, len(state.Subsystems))
	for nqn, subsystem := range state.Subsystems {
		subsystems[nqn] = pbToAny(subsystem)
	}
	clones := make(map[string]any, len(state.Clones))
	for cloneName, clone := range state.Clones {
		clones[cloneName] = pbToAny(clone)
	}
	xfers := make(map[string]any, len(state.Xfers))
	for xferName, xfer := range state.Xfers {
		xfers[xferName] = pbToAny(xfer)
	}
	migrs := make(map[string]any, len(state.Migrs))
	for migrName, migr := range state.Migrs {
		migrs[migrName] = pbToAny(migr)
	}
	// Each chunk prints as its ADDRESS, in the shape the fake agents write
	// into state.json so the two sides of an assertion spell a chunk
	// the same way: a clone chunk is the decimal "{src_slice_idx}:{bm_idx}"
	// pair it is addressed by, a migration chunk its plain append sequence
	// number. Both lists keep model.LoadSp's ascending order.
	bmIdx := func(
		chunks map[string][]model.BmChunk,
		pair bool,
	) map[string]any {
		out := make(map[string]any, len(chunks))
		for name, list := range chunks {
			idList := make([]any, 0, len(list))
			for _, chunk := range list {
				if pair {
					idList = append(idList, fmt.Sprintf(
						"%d:%d", chunk.SliceIdx, chunk.Idx))
					continue
				}
				idList = append(idList, chunk.Idx)
			}
			out[name] = idList
		}
		return out
	}
	emit(map[string]any{
		"sp_name":      target.Name,
		"store_rev":    state.Rev,
		"sp_conf":      pbToAny(state.Conf),
		"cntlrs":       cntlrs,
		"slices":       slices,
		"tds":          tds,
		"subsystems":   subsystems,
		"clones":       clones,
		"xfers":        xfers,
		"migrs":        migrs,
		"clone_bm_idx": bmIdx(state.CloneBmIdx, true),
		"migr_bm_idx":  bmIdx(state.MigrBmIdx, false),
		"missing":      state.Missing,
	})
}

// cmdGetCntlr prints one Cntlr, or — with no --id — every Cntlr of the SP as a
// map keyed by cntlr_id.
func cmdGetCntlr(g *globals, args []string) {
	fs := newFlagSet("get-cntlr", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	var id hexUint
	fs.Var(&id, "id", "cntlr_id (default: every cntlr of the sp)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	if uint64(id) != 0 {
		key := model.CntlrKey(cl.Id, target.Id, uint64(id))
		cntlr := &pb.Cntlr{}
		found, err := cli.Get(ctx, key, cntlr)
		if err != nil {
			die("%v", err)
		}
		if !found {
			die("key %q not found", key)
		}
		emitPb(cntlr)
		return
	}
	state, err := model.LoadSp(ctx, cli, cl.Id, target.Name)
	if err != nil {
		die("get-cntlr: %v", err)
	}
	out := make(map[string]any, len(state.Cntlrs))
	for cntlrId, cntlr := range state.Cntlrs {
		out[idHex(cntlrId)] = pbToAny(cntlr)
	}
	emit(out)
}

// cmdGetSlice prints one Slice, or — with no --id — every Slice of the SP as a
// map keyed by slice_id.
func cmdGetSlice(g *globals, args []string) {
	fs := newFlagSet("get-slice", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	var id hexUint
	fs.Var(&id, "id", "slice_id (default: every slice of the sp)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	if uint64(id) != 0 {
		key := model.SliceKey(cl.Id, target.Id, uint64(id))
		slice := &pb.Slice{}
		found, err := cli.Get(ctx, key, slice)
		if err != nil {
			die("%v", err)
		}
		if !found {
			die("key %q not found", key)
		}
		emitPb(slice)
		return
	}
	state, err := model.LoadSp(ctx, cli, cl.Id, target.Name)
	if err != nil {
		die("get-slice: %v", err)
	}
	out := make(map[string]any, len(state.Slices))
	for sliceId, slice := range state.Slices {
		out[idHex(sliceId)] = pbToAny(slice)
	}
	emit(out)
}

// cmdGetTd prints one ThinDevice, or — with no --name — every td of the SP as
// a map keyed by td_name.
func cmdGetTd(g *globals, args []string) {
	fs := newFlagSet("get-td", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "td_name (default: every td of the sp)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cl := g.loadCluster(ctx, cli)
	target := resolveSp(ctx, cli, cl, *sp)

	if *name != "" {
		key := model.ThinDeviceKey(cl.Id, target.Id, *name)
		td := &pb.ThinDevice{}
		found, err := cli.Get(ctx, key, td)
		if err != nil {
			die("%v", err)
		}
		if !found {
			die("key %q not found", key)
		}
		emitPb(td)
		return
	}
	state, err := model.LoadSp(ctx, cli, cl.Id, target.Name)
	if err != nil {
		die("get-td: %v", err)
	}
	out := make(map[string]any, len(state.Tds))
	for i, td := range state.Tds {
		out[state.TdNames[i]] = pbToAny(td)
	}
	emit(out)
}

// cmdListKeys prints the keys under a prefix, one per line. --prefix takes
// either a full key prefix ("dnv dn_conf …") or one kind of architecture.md,
// Key table, which is then scoped to --cluster when the kind's key carries
// {cluster_id} right after it.
func cmdListKeys(g *globals, args []string) {
	fs := newFlagSet("list-keys", g)
	prefix := fs.String("prefix", "", "a full key prefix or a key kind")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()

	spec := strings.TrimSpace(*prefix)
	scan := ""
	switch {
	case spec == "":
		scan = common.DnvPrefix + " "
	case spec == common.DnvPrefix:
		// The whole store. TrimSpace above has already eaten the trailing
		// space of a "dnv " the caller may have quoted, and worker_test.sh's
		// diagnostics dump asks for exactly `list-keys --prefix dnv`, so a
		// bare prefix must not be mistaken for a key kind name
		// (architecture.md, Key table).
		scan = common.DnvPrefix + " "
	case strings.HasPrefix(spec, common.DnvPrefix+" "):
		scan = spec
	default:
		info, ok := kinds[spec]
		if !ok {
			die("unknown key kind %q; pass a full %q-prefixed prefix "+
				"instead", spec, common.DnvPrefix)
		}
		if info.clusterScoped && g.cluster != "" {
			cl := g.loadCluster(ctx, cli)
			scan = keyPrefix(common.DnvPrefix, spec, idHex(cl.Id))
		} else {
			scan = keyPrefix(common.DnvPrefix, spec)
		}
	}
	keys, _, err := cli.RangeKeys(ctx, scan)
	if err != nil {
		die("%v", err)
	}
	for _, key := range keys {
		fmt.Println(key.Key)
	}
}

// cmdListWorkers prints one JSON object per worker registration — the seed out
// of the key and the epoch out of the value (architecture.md, Key table;
// VW2). It is how case E checks that a dead worker's key is gone (VW6).
func cmdListWorkers(g *globals, args []string) {
	fs := newFlagSet("list-workers", g)
	role := fs.String("role", "", "dn|cn|sp (default: all three)")
	fs.Parse(args)

	roles := []string{
		common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp,
	}
	if *role != "" {
		switch *role {
		case common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp:
		default:
			usageDie("--role wants dn|cn|sp, got %q", *role)
		}
		roles = []string{*role}
	}

	ctx, done, cli := g.open()
	defer done()

	for _, name := range roles {
		kvs, _, err := cli.Range(ctx, model.WorkerRegPrefix(name))
		if err != nil {
			die("%v", err)
		}
		for _, kv := range kvs {
			parsedRole, seed, ok := model.ParseWorkerRegKey(kv.Key)
			if !ok {
				die("malformed worker key %q", kv.Key)
			}
			reg := &pb.WorkerReg{}
			if err := cli.Decode(ctx, kv, reg); err != nil {
				die("%v", err)
			}
			emit(map[string]any{
				"role":  parsedRole,
				"seed":  seed,
				"epoch": reg.GetEpoch(),
			})
		}
	}
}
