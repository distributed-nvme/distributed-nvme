// Command workerctl is the etcd driver of the dnv-worker integration test
// (dnv-worker.md §14.8): "the etcd driver that plays the gateway". It formats
// the architecture.md §5.3 keys through model, marshals the pb messages, bumps
// the revision keys and reads keys back as protojson.
//
// It runs ON THE TEST SERVER, where etcd listens on localhost, and is invoked
// over ssh by integtest/worker_test.sh (§14.3, §14.10). It never dials an
// agent and never sleeps: it is the gateway's write path with explicit
// placement (§14.8, architecture.md §0 item 19). Every mutation is one
// etcdutil.RunSTM, so the state it leaves behind is exactly the state a real
// gateway STM would have produced — capacity keys per §5.6, revision keys
// bumped in place per §5.5, SpConf.next_id past every id the script assigned.
//
// Conventions the script relies on:
//
//   - Global flags may be given BEFORE the subcommand (the §14.10 `ctl`
//     wrapper does exactly that) or after it; the later occurrence wins.
//   - stdout carries exactly one JSON document per invocation — protojson for
//     a read, the ids and revisions it assigned for a mutation — except
//     list-keys (one key per line) and list-workers (one object per line).
//   - The log.md §5.3 records etcdutil emits ("etcd get", "etcd put", …) go to
//     STDERR, where common/log.go's init() handler puts every record, so that
//     they never interleave with the JSON the script pipes into jq.
//     They carry --trace-id, so a failing run still correlates every write of
//     the driver with the test stage that made it.
//   - protojson is emitted with EmitUnpopulated, so that the fields the §14.11
//     assertions read — err_epoch, provisioned, created, primary — are present
//     even when they hold their proto3 default. uint64 fields are JSON strings
//     (the proto3 JSON mapping), so jq compares them as "0", not 0.
//   - Ids accept decimal or 0x hex everywhere.
//
// Exit codes: 0 on success, 1 on any error (with a message on stderr), 2 on a
// usage error.
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
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
	// defaultEndpoints is the single-node etcd of §14.3: it listens on the
	// loopback of the test server, and workerctl runs there.
	defaultEndpoints = "127.0.0.1:12379"
	// pingKey is the key `ping` reads. It is deliberately a key nothing ever
	// writes: the probe succeeds on not-found (§14.7 step 3), so what it
	// proves is that etcd answers, not that anything is stored.
	pingKey = common.DnvPrefix + " ping"
	// The nvme_tr_conf fields every DnConf/CnConf this driver writes carries
	// (§14.8: "so the SyncupSide nvme_tr_conf fields are non-empty"). The
	// fake agents never open an NVMe-oF port, so the service id only has to be
	// deterministic and plausible: 4420 is the IANA nvme-tcp port and the one
	// the real agents use.
	nvmeTrType  = "tcp"
	nvmeAdrFam  = "ipv4"
	nvmeTrSvcId = "4420"
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
// §14.11 checks on provisioned, created and err_epoch need.
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

// idHex renders an id the way every key field does (architecture.md §5.1), so
// that the map members of a composite reply are addressable by the same
// spelling the keys use.
func idHex(id uint64) string {
	return fmt.Sprintf(common.IdKeyFmt, id)
}

// ---------------------------------------------------------------------------
// Flag value types
// ---------------------------------------------------------------------------

// hexUint is an id flag: decimal or 0x hex, parsed with base 0 (§14.8).
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
// common.ShardCodeFmt spelling (architecture.md §5.1), and §14.11 case E
// spreads DNs over "00", "55", "aa" and "ff", so the value is parsed as HEX —
// with or without a 0x prefix. Reading it as decimal would silently turn the
// "55" of the test plan into shard 0x37.
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
// on its own and neither may disturb the other (§14.8).
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

// ---------------------------------------------------------------------------
// Tuple parsers (§14.8). Every one of them rejects a malformed spec loudly, so
// that a mistyped test invocation fails instead of writing garbage into etcd.
// ---------------------------------------------------------------------------

// parseId parses one id field: decimal or 0x hex (§14.8).
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

// parseSidePointer reads the `--side sp:leg:side` form of put-dn (§14.8): one
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

// parseCntlrPointer reads the `--cntlr sp:cntlr` form of put-cn (§14.8).
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

// cntlrSpec is one `--cntlr id:cn_id:slot:primary` of put-sp: which cntlr id
// to use, which CN hosts it, its cntlid slot and whether it is the primary.
type cntlrSpec struct {
	cntlrId uint64
	cnId    uint64
	slot    uint32
	primary bool
}

func parseCntlrSpec(spec string) (cntlrSpec, error) {
	var out cntlrSpec
	parts, err := splitSpec(spec, 4, cntlrForm)
	if err != nil {
		return out, err
	}
	if out.cntlrId, err = parseIdField(parts[0], spec, cntlrForm); err != nil {
		return out, err
	}
	if out.cnId, err = parseIdField(parts[1], spec, cntlrForm); err != nil {
		return out, err
	}
	if out.slot, err = parseU32Field(parts[2], spec, cntlrForm); err != nil {
		return out, err
	}
	if out.primary, err = parseBoolField(parts[3], spec, cntlrForm); err != nil {
		return out, err
	}
	return out, nil
}

const sliceForm = "id:idx"

// sliceSpec is one `--slice id:idx` of put-sp.
type sliceSpec struct {
	sliceId  uint64
	sliceIdx uint32
}

func parseSliceSpec(spec string) (sliceSpec, error) {
	var out sliceSpec
	parts, err := splitSpec(spec, 2, sliceForm)
	if err != nil {
		return out, err
	}
	if out.sliceId, err = parseIdField(parts[0], spec, sliceForm); err != nil {
		return out, err
	}
	if out.sliceIdx, err = parseU32Field(parts[1], spec, sliceForm); err != nil {
		return out, err
	}
	return out, nil
}

const groupForm = "slice:grp:meta|data:ext_cnt:none|raid1"

// groupSpec is one `--group slice:grp:meta|data:ext_cnt:none|raid1` of put-sp.
// The redundancy kind is per group in the flag because that is what decides
// the group's §3.6 meta_blocks; every group of one SP must name the same kind,
// since SpConf.bdev_conf holds exactly one redund_conf.
type groupSpec struct {
	sliceId uint64
	grpId   uint64
	isMeta  bool
	extCnt  uint64
	raid1   bool
}

func parseGroupSpec(spec string) (groupSpec, error) {
	var out groupSpec
	parts, err := splitSpec(spec, 5, groupForm)
	if err != nil {
		return out, err
	}
	if out.sliceId, err = parseIdField(parts[0], spec, groupForm); err != nil {
		return out, err
	}
	if out.grpId, err = parseIdField(parts[1], spec, groupForm); err != nil {
		return out, err
	}
	switch strings.TrimSpace(parts[2]) {
	case "meta":
		out.isMeta = true
	case "data":
		out.isMeta = false
	default:
		return out, fmt.Errorf(
			"%q in %q: want meta or data", parts[2], spec,
		)
	}
	if out.extCnt, err = parseIdField(parts[3], spec, groupForm); err != nil {
		return out, err
	}
	if out.extCnt == 0 {
		return out, fmt.Errorf("%q: ext_cnt must not be zero", spec)
	}
	switch strings.TrimSpace(parts[4]) {
	case "raid1":
		out.raid1 = true
	case "none":
		out.raid1 = false
	default:
		return out, fmt.Errorf(
			"%q in %q: want none or raid1", parts[4], spec,
		)
	}
	return out, nil
}

const legForm = "grp:leg:idx"

// legSpec is one `--leg grp:leg:idx` of put-sp.
type legSpec struct {
	grpId  uint64
	legId  uint64
	legIdx uint32
}

func parseLegSpec(spec string) (legSpec, error) {
	var out legSpec
	parts, err := splitSpec(spec, 3, legForm)
	if err != nil {
		return out, err
	}
	if out.grpId, err = parseIdField(parts[0], spec, legForm); err != nil {
		return out, err
	}
	if out.legId, err = parseIdField(parts[1], spec, legForm); err != nil {
		return out, err
	}
	if out.legIdx, err = parseU32Field(parts[2], spec, legForm); err != nil {
		return out, err
	}
	return out, nil
}

const sideForm = "leg:side:dn_id:slot"

// sideSpec is one `--side leg:side:dn_id:slot` of put-sp.
type sideSpec struct {
	legId  uint64
	sideId uint64
	dnId   uint64
	slot   uint32
}

func parseSideSpec(spec string) (sideSpec, error) {
	var out sideSpec
	parts, err := splitSpec(spec, 4, sideForm)
	if err != nil {
		return out, err
	}
	if out.legId, err = parseIdField(parts[0], spec, sideForm); err != nil {
		return out, err
	}
	if out.sideId, err = parseIdField(parts[1], spec, sideForm); err != nil {
		return out, err
	}
	if out.dnId, err = parseIdField(parts[2], spec, sideForm); err != nil {
		return out, err
	}
	if out.slot, err = parseU32Field(parts[3], spec, sideForm); err != nil {
		return out, err
	}
	return out, nil
}

const migrForm = "name:id:src_side:dst_side"

// migrSpec is the `--migr name:id:src_side:dst_side` of put-migr.
type migrSpec struct {
	name    string
	migrId  uint64
	srcSide uint64
	dstSide uint64
}

func parseMigrSpec(spec string) (migrSpec, error) {
	var out migrSpec
	parts, err := splitSpec(spec, 4, migrForm)
	if err != nil {
		return out, err
	}
	out.name = strings.TrimSpace(parts[0])
	if out.name == "" {
		return out, fmt.Errorf("%q: the migration name is empty", spec)
	}
	if out.migrId, err = parseIdField(parts[1], spec, migrForm); err != nil {
		return out, err
	}
	if out.srcSide, err = parseIdField(parts[2], spec, migrForm); err != nil {
		return out, err
	}
	if out.dstSide, err = parseIdField(parts[3], spec, migrForm); err != nil {
		return out, err
	}
	return out, nil
}

const nsForm = "id:idx:td_id"

// nsSpec is one `--ns id:idx:td_id` of put-ss.
type nsSpec struct {
	nsId  uint64
	nsIdx uint32
	tdId  uint64
}

func parseNsSpec(spec string) (nsSpec, error) {
	var out nsSpec
	parts, err := splitSpec(spec, 3, nsForm)
	if err != nil {
		return out, err
	}
	if out.nsId, err = parseIdField(parts[0], spec, nsForm); err != nil {
		return out, err
	}
	if out.nsIdx, err = parseU32Field(parts[1], spec, nsForm); err != nil {
		return out, err
	}
	if out.tdId, err = parseIdField(parts[2], spec, nsForm); err != nil {
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

// parseSpLevel reads `--level`: either the number the §14.11 steps use
// (`set-level sp0 48`) or the enum name. Any other number is refused — an
// undefined level would be stored and then read back as a level no rule of §7
// knows.
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
// Keys (architecture.md §5.1, §5.3)
// ---------------------------------------------------------------------------

// keyPrefix joins key fields the §5.1 way and ends in the separating space, so
// that a prefix can never match a longer sibling field. Every full key this
// driver writes comes from model; model exports no bare scan prefix for the
// kinds `list-keys` walks, so this mirrors the one grammar rule rather than
// hand-spelling each prefix.
func keyPrefix(fields ...string) string {
	return strings.Join(fields, " ") + " "
}

// kindInfo describes one row of the §5.3 key table: the message a value of
// that kind decodes into (which is how `get` picks a type from a key's SECOND
// field) and whether {cluster_id} is the field right after the kind (which is
// how `list-keys` scopes a prefix to one cluster).
type kindInfo struct {
	newMsg        func() proto.Message
	clusterScoped bool
}

// kinds is the §5.3 key table.
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
// is what `get --key` needs (§5.3: every stored message has one key schema and
// the kind field names it).
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

// nvmeTrConfOf builds the transport record of a node from its gRPC endpoint.
// The fakes never open an NVMe-oF port, so only the shape matters: the worker
// copies these fields into every SyncupSide/SyncupCntlr, and §14.8 asks for
// them to be non-empty.
func nvmeTrConfOf(addrPort string) *pb.NvmeTrConf {
	host, port := addrPort, nvmeTrSvcId
	if idx := strings.LastIndex(addrPort, ":"); idx >= 0 {
		host, port = addrPort[:idx], addrPort[idx+1:]
	}
	// tr_svc_id is the node's own port, not the fixed nvmeTrSvcId: the fake
	// agents never open an NVMe-oF port, so the value is inert, and every node
	// of the §14.3 topology shares one host address. A constant here would make
	// every node's NvmeTrConf byte-identical, and a CdcEntry's
	// nvme_tr_conf_list would then be unattributable to a particular CN —
	// which is exactly what §14.11 case D step 3 has to assert ("CdcEntry of
	// ss0 lists cn 2 and cn 3, not cn 1").
	return &pb.NvmeTrConf{
		TrType:  nvmeTrType,
		AdrFam:  nvmeAdrFam,
		TrAddr:  host,
		TrSvcId: port,
	}
}

// nsIdentity derives a namespace's dev_uuid / dev_nguid from (cluster_id,
// sp_id, ns_id). The gateway generates a random v4 uuid and 16 random bytes
// (architecture.md §8.8 CreateNamespace); a test driver must instead be
// reproducible, so the same namespace of the same cluster always gets the same
// identity.
func nsIdentity(cid uint64, spId uint64, nsId uint64) (string, string) {
	var raw [16]byte
	high := fnv.New64a()
	fmt.Fprintf(high, "dnv-ns-uuid %016x %016x %016x", cid, spId, nsId)
	binary.BigEndian.PutUint64(raw[0:8], high.Sum64())
	low := fnv.New64a()
	fmt.Fprintf(low, "dnv-ns-nguid %016x %016x %016x", cid, spId, nsId)
	binary.BigEndian.PutUint64(raw[8:16], low.Sum64())
	// The RFC 4122 v4 shape: version 4, variant 10xx.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	nguid := hex.EncodeToString(raw[:])
	uuid := strings.Join([]string{
		nguid[0:8], nguid[8:12], nguid[12:16], nguid[16:20], nguid[20:32],
	}, "-")
	return uuid, nguid
}

// ---------------------------------------------------------------------------
// Globals (§14.8)
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
// `workerctl --cluster c put-dn …` (the §14.10 `ctl` wrapper) and
// `workerctl put-dn --cluster c …` are both accepted and the later occurrence
// wins.
func (g *globals) bind(fs *flag.FlagSet) {
	fs.StringVar(&g.endpoints, "endpoints", g.endpoints,
		"comma-separated etcd client endpoints")
	fs.StringVar(&g.cluster, "cluster", g.cluster,
		"cluster NAME; the cluster_id is derived from it and the stored "+
			"creation_epoch (architecture.md §5.2)")
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

// clusterId reads ClusterConf and derives the cluster_id from it, exactly as
// the gateway must (architecture.md §5.2, §14.8): cluster_id is not computable
// from a name alone, so every subcommand but put-cluster / ping / list-* goes
// through here first.
func (g *globals) clusterId(
	ctx context.Context,
	cli *etcdutil.Client,
) (uint64, *pb.ClusterConf) {
	if g.cluster == "" {
		die("--cluster is required")
	}
	cc := &pb.ClusterConf{}
	found, err := cli.Get(ctx, model.ClusterConfKey(g.cluster), cc)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("cluster %q does not exist", g.cluster)
	}
	return model.ClusterId(g.cluster, cc.GetCreationEpoch()), cc
}

// ---------------------------------------------------------------------------
// Shared lookups
// ---------------------------------------------------------------------------

// findNodeAddr resolves a dn_id / cn_id to the addr_port its record is keyed
// by (§14.8's note on id-addressed reads): DnConf and CnConf are keyed by
// endpoint, several subcommands take an id, so exactly one helper scans the
// cluster's conf prefix for the record that carries the id.
func findNodeAddr(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	kind string,
	id uint64,
) string {
	prefix := keyPrefix(common.DnvPrefix, kind, idHex(cid))
	kvs, _, err := cli.Range(ctx, prefix)
	if err != nil {
		die("%v", err)
	}
	found := ""
	for _, kv := range kvs {
		addrPort := strings.TrimPrefix(kv.Key, prefix)
		var stored uint64
		switch kind {
		case "dn_conf":
			dn := &pb.DnConf{}
			if err := cli.Decode(ctx, kv, dn); err != nil {
				die("%v", err)
			}
			stored = dn.GetDnId()
		case "cn_conf":
			cn := &pb.CnConf{}
			if err := cli.Decode(ctx, kv, cn); err != nil {
				die("%v", err)
			}
			stored = cn.GetCnId()
		default:
			die("findNodeAddr: unknown kind %q", kind)
		}
		if stored != id {
			continue
		}
		if found != "" {
			die("%s %#x is stored at both %q and %q",
				kind, id, found, addrPort)
		}
		found = addrPort
	}
	if found == "" {
		die("no %s with id %#x under prefix %q", kind, id, prefix)
	}
	return found
}

// spTarget is an SP resolved from a `--sp` value, which may be either the SP
// NAME (SpConf is name-keyed) or the sp_id (the §14.11 steps use both, e.g.
// `set-lwm sp0` and `set-cntlr --sp 1`).
type spTarget struct {
	name  string
	spId  uint64
	shard uint32
}

// resolveSp turns a `--sp` value into a target. A value that parses as a
// number is looked up through the {p} sp_id_to_name reverse key first; a value
// that does not, or an id with no reverse key, is taken as a name.
func resolveSp(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spec string,
) spTarget {
	if strings.TrimSpace(spec) == "" {
		die("--sp is required")
	}
	name := strings.TrimSpace(spec)
	if id, err := parseId(name); err == nil {
		reverse := &pb.SpName{}
		found, err := cli.Get(ctx, model.SpNameKey(cid, id), reverse)
		if err != nil {
			die("%v", err)
		}
		if found {
			name = reverse.GetSpName()
		}
	}
	conf := &pb.SpConf{}
	found, err := cli.Get(ctx, model.SpConfKey(cid, name), conf)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("no storage pool %q in cluster %s", spec, idHex(cid))
	}
	return spTarget{
		name:  name,
		spId:  conf.GetSpId(),
		shard: conf.GetShardCode(),
	}
}

// getSpConf re-reads an SP inside an STM and re-checks its identity, the way
// model does for the worker's ops (AR3): the SpConf is name-keyed, so an SP
// deleted and re-created under the same name between the resolve and this
// transaction is a different SP and none of the ids the caller carries mean
// anything in it.
func getSpConf(
	s etcdutil.STM,
	cid uint64,
	target spTarget,
) (*pb.SpConf, error) {
	conf := &pb.SpConf{}
	if !s.Get(model.SpConfKey(cid, target.name), conf) {
		return nil, fmt.Errorf("storage pool %q not found", target.name)
	}
	if conf.GetSpId() != target.spId {
		return nil, fmt.Errorf(
			"storage pool %q changed its id: %#x, want %#x",
			target.name, conf.GetSpId(), target.spId,
		)
	}
	return conf, nil
}

// bumpSpRev rewrites the SP's revision key with revision + 1 IN PLACE (§5.5):
// the key is id-based and therefore stable, so a watcher must see one put, not
// a delete followed by a put. Every SpConf sub-object mutation calls it
// exactly once. model has the same body, unexported.
func bumpSpRev(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) (uint64, error) {
	key := model.SpRevKey(conf.GetShardCode(), cid, conf.GetSpId())
	rev := &pb.SpRev{}
	if !s.Get(key, rev) {
		return 0, fmt.Errorf("sp rev key %q not found", key)
	}
	rev.Revision++
	s.Put(key, rev)
	return rev.Revision, nil
}

// bumpDnRev bumps one DN's revision key in place (§5.5).
func bumpDnRev(s etcdutil.STM, cid uint64, dn *pb.DnConf) (uint64, error) {
	key := model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId())
	rev := &pb.DnRev{}
	if !s.Get(key, rev) {
		return 0, fmt.Errorf("dn rev key %q not found", key)
	}
	rev.Revision++
	s.Put(key, rev)
	return rev.Revision, nil
}

// bumpCnRev bumps one CN's revision key in place (§5.5).
func bumpCnRev(s etcdutil.STM, cid uint64, cn *pb.CnConf) (uint64, error) {
	key := model.CnRevKey(cn.GetShardCode(), cid, cn.GetCnId())
	rev := &pb.CnRev{}
	if !s.Get(key, rev) {
		return 0, fmt.Errorf("cn rev key %q not found", key)
	}
	rev.Revision++
	s.Put(key, rev)
	return rev.Revision, nil
}

// cloneBdevConf copies a BdevConf for mutation. proto.Clone of a nil message
// returns a read-only zero, so a cluster written without a bdev_conf needs a
// fresh one.
func cloneBdevConf(src *pb.BdevConf) *pb.BdevConf {
	if src == nil {
		return &pb.BdevConf{}
	}
	return proto.Clone(src).(*pb.BdevConf)
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
	{"set-created", cmdSetCreated},
	{"set-provisioned", cmdSetProvisioned},
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
// get-rev / set-free off the argument list. It is accepted both before the
// flags (the §14.8 table's spelling) and after them, since Go's flag package
// stops at the first non-flag argument either way.
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

// ---------------------------------------------------------------------------
// ping
// ---------------------------------------------------------------------------

// cmdPing is the etcd liveness probe of §14.7 step 3: one Get of a key nothing
// writes, which succeeds on not-found.
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
// put-cluster
// ---------------------------------------------------------------------------

// cmdPutCluster implements CreateCluster (architecture.md §8.1, §5.2): it
// stamps creation_epoch = time.Now().UnixNano() like the gateway, derives the
// cluster_id from the name and that epoch, and writes the three zeroed globals
// alongside the ClusterConf (§5.4).
//
// It refuses to overwrite an existing cluster: creation_epoch is immutable, and
// rewriting it would orphan every other key of the cluster (§5.2).
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
		"bdev_conf.dm_pool_conf.data_block_size, the §3.6 block_size")
	chunkBlocks := fs.Uint64("chunk-blocks", common.DefaultChunkBlockCnt,
		"redund_md_raid1.bitmap_chunk_block_cnt, the §3.6 bitmap chunk")
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

	// The epoch is stamped once, outside the transaction: a retried attempt
	// must derive the same cluster_id.
	epoch := uint64(time.Now().UnixNano())
	cid := model.ClusterId(clusterName, epoch)
	cc := &pb.ClusterConf{
		CreationEpoch: epoch,
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{
				DataBlockSize:   *blockSize,
				LowWaterMarkPct: uint32(*lwm),
			},
			// put-sp inherits this bdev_conf and replaces the redund kind
			// with the one its --group flags name; the chunk count rides
			// along, which is how a case pins the §3.6 geometry.
			RedundConf: &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundMdRaid1{
					RedundMdRaid1: &pb.RedundMdRaid1{
						BitmapChunkBlockCnt: *chunkBlocks,
					},
				},
			},
		},
		DnBinConf: &pb.DnBinConf{ExtentSize: *extentSize},
		AllocConf: &pb.AllocConf{
			DnBatchSize: uint32(*dnBatch),
			CnBatchSize: uint32(*cnBatch),
		},
		HealthCheckConf: &pb.HealthCheckConf{
			DnInterval:    uint32(*dnInterval),
			CnInterval:    uint32(*cnInterval),
			SideInterval:  uint32(*sideInterval),
			CntlrInterval: uint32(*cntlrInterval),
		},
	}
	// workerctl plays the gateway for this suite, so it must store what
	// CreateCluster stores: a FULLY RESOLVED conf (§7). Every member left at
	// its flag's zero — the batch sizes, the four intervals, the bin shift
	// ladder, dm_raid0_conf.stripe_size — becomes concrete here, because the
	// worker now validates the stored conf and refuses to drive an object
	// whose geometry nobody chose. Writing the raw literal would make every
	// case in the suite fail at the first round.
	cc = model.ResolveClusterConf(cc)
	newGlobal := func() ([]uint32, uint64) {
		return make([]uint32, common.ShardBucketSize), 1
	}
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := model.ClusterConfKey(clusterName)
		if s.Get(key, &pb.ClusterConf{}) {
			return fmt.Errorf(
				"cluster %q already exists; creation_epoch is immutable "+
					"(architecture.md §5.2)", clusterName,
			)
		}
		s.Put(key, cc)
		bucket, nextId := newGlobal()
		s.Put(model.DnGlobalKey(cid), &pb.DnGlobal{
			NextId: nextId, ShardBucket: bucket,
		})
		bucket, nextId = newGlobal()
		s.Put(model.CnGlobalKey(cid), &pb.CnGlobal{
			NextId: nextId, ShardBucket: bucket,
		})
		bucket, nextId = newGlobal()
		s.Put(model.SpGlobalKey(cid), &pb.SpGlobal{
			NextId: nextId, ShardBucket: bucket,
		})
		return nil
	})
	if err != nil {
		die("put-cluster: %v", err)
	}
	emit(map[string]any{
		"cluster_name":       clusterName,
		"cluster_id":         idHex(cid),
		"cluster_id_0x":      fmt.Sprintf("%#016x", cid),
		"creation_epoch":     epoch,
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
// (§14.8, architecture.md §8.2): ONE STM writes DnConf, DnRev (revision =
// current + 1, or 1 when absent) and the DnCapacity key through
// model.MaintainDnCapacity, so the §5.6 presence rule holds the moment the
// transaction commits.
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

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	if *addr == "" {
		die("--addr is required")
	}
	if *totalExt == 0 {
		*totalExt = *freeExt
	}
	shardCode := uint32(shard)
	ptrList := make([]*pb.SidePointer, 0, len(sides))
	for _, spec := range sides {
		ptr, err := parseSidePointer(spec)
		if err != nil {
			die("--side: %v", err)
		}
		ptrList = append(ptrList, ptr)
	}

	ctx, done, cli := g.open()
	defer done()
	cid, cc := g.clusterId(ctx, cli)

	newDn := &pb.DnConf{
		DnId:        uint64(id),
		ShardCode:   shardCode,
		Disabled:    *disabled,
		ErrEpoch:    0,
		NvmeTrConf:  nvmeTrConfOf(*addr),
		Location:    *location,
		SidePtrList: ptrList,
		TotalExtCnt: *totalExt,
		FreeExtCnt:  *freeExt,
	}
	revision := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		confKey := model.DnConfKey(cid, *addr)
		old := &pb.DnConf{}
		var oldDn *pb.DnConf
		if s.Get(confKey, old) {
			oldDn = old
		}
		revKey := model.DnRevKey(shardCode, cid, uint64(id))
		rev := &pb.DnRev{}
		created := !s.Get(revKey, rev)
		revision = rev.GetRevision() + 1
		s.Put(confKey, newDn)
		model.MaintainDnCapacity(s, cid, *addr, cc, oldDn, newDn)
		s.Put(revKey, &pb.DnRev{AddrPort: *addr, Revision: revision})
		if created {
			// §5.4: creating a node consumes an id and one bucket slot, so
			// a worker reaction that allocates later never reuses this id.
			globalKey := model.DnGlobalKey(cid)
			global := &pb.DnGlobal{}
			if s.Get(globalKey, global) {
				if global.NextId <= uint64(id) {
					global.NextId = uint64(id) + 1
				}
				if int(shardCode) < len(global.ShardBucket) {
					global.ShardBucket[shardCode]++
				}
				s.Put(globalKey, global)
			}
		}
		return nil
	})
	if err != nil {
		die("put-dn: %v", err)
	}
	emit(map[string]any{
		"dn_id":        idHex(uint64(id)),
		"shard_code":   fmt.Sprintf(common.ShardCodeFmt, shardCode),
		"addr_port":    *addr,
		"revision":     revision,
		"free_ext_cnt": *freeExt,
		"allocatable":  model.DnAllocatable(newDn, cc.GetDnBinConf()),
	})
}

// cmdPutCn is cmdPutDn's mirror for a controller node (§8.3): CnConf, CnRev
// and the CnCapacity key through model.MaintainCnCapacity.
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

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	if *addr == "" {
		die("--addr is required")
	}
	if *totalExt == 0 {
		*totalExt = *freeExt
	}
	shardCode := uint32(shard)
	ptrList := make([]*pb.CntlrPointer, 0, len(cntlrs))
	for _, spec := range cntlrs {
		ptr, err := parseCntlrPointer(spec)
		if err != nil {
			die("--cntlr: %v", err)
		}
		ptrList = append(ptrList, ptr)
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)

	newCn := &pb.CnConf{
		CnId:         uint64(id),
		ShardCode:    shardCode,
		Disabled:     *disabled,
		ErrEpoch:     0,
		NvmeTrConf:   nvmeTrConfOf(*addr),
		Location:     *location,
		CntlrPtrList: ptrList,
		TotalExtCnt:  *totalExt,
		FreeExtCnt:   *freeExt,
	}
	revision := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		confKey := model.CnConfKey(cid, *addr)
		old := &pb.CnConf{}
		var oldCn *pb.CnConf
		if s.Get(confKey, old) {
			oldCn = old
		}
		revKey := model.CnRevKey(shardCode, cid, uint64(id))
		rev := &pb.CnRev{}
		created := !s.Get(revKey, rev)
		revision = rev.GetRevision() + 1
		s.Put(confKey, newCn)
		model.MaintainCnCapacity(s, cid, *addr, oldCn, newCn)
		s.Put(revKey, &pb.CnRev{AddrPort: *addr, Revision: revision})
		if created {
			globalKey := model.CnGlobalKey(cid)
			global := &pb.CnGlobal{}
			if s.Get(globalKey, global) {
				if global.NextId <= uint64(id) {
					global.NextId = uint64(id) + 1
				}
				if int(shardCode) < len(global.ShardBucket) {
					global.ShardBucket[shardCode]++
				}
				s.Put(globalKey, global)
			}
		}
		return nil
	})
	if err != nil {
		die("put-cn: %v", err)
	}
	emit(map[string]any{
		"cn_id":        idHex(uint64(id)),
		"shard_code":   fmt.Sprintf(common.ShardCodeFmt, shardCode),
		"addr_port":    *addr,
		"revision":     revision,
		"free_ext_cnt": *freeExt,
		"allocatable":  model.CnAllocatable(newCn),
	})
}

// ---------------------------------------------------------------------------
// bump-rev / move-dn / del-rev
// ---------------------------------------------------------------------------

// cmdBumpRev raises one revision key IN PLACE (§5.5): the key is id-based and
// therefore stable, so a watcher sees one put — never a delete followed by a
// put, which would read as "node removed, then a different node added".
func cmdBumpRev(g *globals, args []string) {
	fs := newFlagSet("bump-rev", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id / sp_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn", "sp")

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	shardCode := uint32(shard)

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)

	revision := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		switch kind {
		case "dn":
			key := model.DnRevKey(shardCode, cid, uint64(id))
			rev := &pb.DnRev{}
			if !s.Get(key, rev) {
				return fmt.Errorf("%q not found", key)
			}
			rev.Revision++
			revision = rev.Revision
			s.Put(key, rev)
		case "cn":
			key := model.CnRevKey(shardCode, cid, uint64(id))
			rev := &pb.CnRev{}
			if !s.Get(key, rev) {
				return fmt.Errorf("%q not found", key)
			}
			rev.Revision++
			revision = rev.Revision
			s.Put(key, rev)
		case "sp":
			key := model.SpRevKey(shardCode, cid, uint64(id))
			rev := &pb.SpRev{}
			if !s.Get(key, rev) {
				return fmt.Errorf("%q not found", key)
			}
			rev.Revision++
			revision = rev.Revision
			s.Put(key, rev)
		}
		return nil
	})
	if err != nil {
		die("bump-rev: %v", err)
	}
	emit(map[string]any{
		"kind":       kind,
		"id":         idHex(uint64(id)),
		"shard_code": fmt.Sprintf(common.ShardCodeFmt, shardCode),
		"revision":   revision,
	})
}

// cmdMoveDn is the §5.5 moved-node case: one STM rewrites DnRev.addr_port and
// bumps its revision, and moves DnConf and the DnCapacity key to the new
// endpoint. The rev key itself is rewritten, never deleted and re-created, so
// the watching worker sees a single put that says "the same DN now answers
// somewhere else".
//
// It rewrites nothing else: Sides already placed on the DN keep the old
// addr_port copy their Slice holds, because §5.5 has no rename path and no
// v001 RPC changes a node's endpoint. The suite only moves a DN that carries
// no side (§14.11 A2, before its SP exists).
func cmdMoveDn(g *globals, args []string) {
	fs := newFlagSet("move-dn", g)
	var id hexUint
	fs.Var(&id, "id", "dn_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	addr := fs.String("addr", "", "the NEW ip:port (required)")
	fs.Parse(args)

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	if *addr == "" {
		die("--addr is required")
	}
	shardCode := uint32(shard)

	ctx, done, cli := g.open()
	defer done()
	cid, cc := g.clusterId(ctx, cli)
	oldAddr := findNodeAddr(ctx, cli, cid, "dn_conf", uint64(id))
	if oldAddr == *addr {
		die("dn %#x is already at %q", uint64(id), *addr)
	}

	revision := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		oldKey := model.DnConfKey(cid, oldAddr)
		oldDn := &pb.DnConf{}
		if !s.Get(oldKey, oldDn) {
			return fmt.Errorf("%q not found", oldKey)
		}
		if oldDn.GetDnId() != uint64(id) {
			return fmt.Errorf(
				"%q holds dn %#x, want %#x",
				oldKey, oldDn.GetDnId(), uint64(id),
			)
		}
		newKey := model.DnConfKey(cid, *addr)
		if s.Get(newKey, &pb.DnConf{}) {
			return fmt.Errorf("%q already exists", newKey)
		}
		newDn := proto.Clone(oldDn).(*pb.DnConf)
		newDn.NvmeTrConf = nvmeTrConfOf(*addr)
		s.Del(oldKey)
		model.MaintainDnCapacity(s, cid, oldAddr, cc, oldDn, nil)
		s.Put(newKey, newDn)
		model.MaintainDnCapacity(s, cid, *addr, cc, nil, newDn)
		revKey := model.DnRevKey(shardCode, cid, uint64(id))
		rev := &pb.DnRev{}
		if !s.Get(revKey, rev) {
			return fmt.Errorf("%q not found", revKey)
		}
		rev.AddrPort = *addr
		rev.Revision++
		revision = rev.Revision
		s.Put(revKey, rev)
		return nil
	})
	if err != nil {
		die("move-dn: %v", err)
	}
	emit(map[string]any{
		"dn_id":         idHex(uint64(id)),
		"shard_code":    fmt.Sprintf(common.ShardCodeFmt, shardCode),
		"old_addr_port": oldAddr,
		"addr_port":     *addr,
		"revision":      revision,
	})
}

// cmdDelRev deletes one revision key and nothing else — the "the object is
// gone as far as the watching worker is concerned" case of §14.11 A5.
func cmdDelRev(g *globals, args []string) {
	fs := newFlagSet("del-rev", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id / sp_id")
	var shard shardFlag
	fs.Var(&shard, "shard", "shard code, hex (00, 55, aa, ff …)")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn", "sp")

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	shardCode := uint32(shard)

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)

	key := ""
	revision := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		switch kind {
		case "dn":
			key = model.DnRevKey(shardCode, cid, uint64(id))
			rev := &pb.DnRev{}
			if !s.Get(key, rev) {
				return fmt.Errorf("%q not found", key)
			}
			revision = rev.GetRevision()
		case "cn":
			key = model.CnRevKey(shardCode, cid, uint64(id))
			rev := &pb.CnRev{}
			if !s.Get(key, rev) {
				return fmt.Errorf("%q not found", key)
			}
			revision = rev.GetRevision()
		case "sp":
			key = model.SpRevKey(shardCode, cid, uint64(id))
			rev := &pb.SpRev{}
			if !s.Get(key, rev) {
				return fmt.Errorf("%q not found", key)
			}
			revision = rev.GetRevision()
		}
		s.Del(key)
		return nil
	})
	if err != nil {
		die("del-rev: %v", err)
	}
	emit(map[string]any{
		"kind":         kind,
		"id":           idHex(uint64(id)),
		"shard_code":   fmt.Sprintf(common.ShardCodeFmt, shardCode),
		"key":          key,
		"old_revision": revision,
	})
}

// ---------------------------------------------------------------------------
// put-sp
// ---------------------------------------------------------------------------

// spPlan is the validated tree the --cntlr/--slice/--group/--leg/--side flags
// describe. The flags name their parent by id, so the tree is built by joining
// on those ids; every dangling reference and every duplicate id is a hard
// error, because a mistyped placement would otherwise become an SP no worker
// can drive.
type spPlan struct {
	cntlrs []cntlrSpec
	slices []sliceSpec
	groups []groupSpec
	legs   []legSpec
	sides  []sideSpec
	// raid1 is the one redundancy kind every group named.
	raid1 bool
	// grpOfLeg and extOfLeg answer "which group owns this leg" and "how many
	// extents does a side of this leg cost".
	grpOfLeg map[uint64]uint64
	extOfLeg map[uint64]uint64
	// footprint is Σ ext_cnt over ALL groups — what one cntlr's CN reserves
	// for the SP (§8.4, §8.6).
	footprint uint64
	// nextId is one past the largest id the plan uses, so that a worker
	// reaction allocates fresh ids ABOVE the script's (§14.5).
	nextId uint64
}

// buildSpPlan parses and validates the placement flags of put-sp.
func buildSpPlan(
	cntlrSpecs, sliceSpecs, groupSpecs, legSpecs, sideSpecs []string,
	slots []uint32,
) (*spPlan, error) {
	plan := &spPlan{
		grpOfLeg: make(map[uint64]uint64),
		extOfLeg: make(map[uint64]uint64),
	}
	// Every sub-object id must be distinct WITHIN ITS KIND, and next_id ends
	// up past all of them (§5.4, §14.5). Uniqueness is not enforced ACROSS
	// kinds even though a real gateway draws all of them from the one
	// SpConf.next_id counter: §14.11 labels the objects of one SP per kind
	// (slice `1`, group `G1`, leg `L1`, side `S1`), nothing in model or
	// worker ever looks an id up without knowing its kind, and refusing the
	// numbering the cases are written in would buy nothing.
	ids := make(map[string]map[uint64]struct{})
	claim := func(id uint64, what string) error {
		if id == 0 {
			return fmt.Errorf("%s id 0 is never valid", what)
		}
		if ids[what] == nil {
			ids[what] = make(map[uint64]struct{})
		}
		if _, ok := ids[what][id]; ok {
			return fmt.Errorf("%s id %#x is used twice", what, id)
		}
		ids[what][id] = struct{}{}
		if plan.nextId <= id {
			plan.nextId = id + 1
		}
		return nil
	}
	slotSet := make(map[uint32]struct{}, len(slots))
	for _, slot := range slots {
		slotSet[slot] = struct{}{}
	}

	primaryCnt := 0
	usedSlots := make(map[uint32]uint64)
	usedCns := make(map[uint64]uint64)
	for _, spec := range cntlrSpecs {
		parsed, err := parseCntlrSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("--cntlr: %w", err)
		}
		if err := claim(parsed.cntlrId, "cntlr"); err != nil {
			return nil, err
		}
		if _, ok := slotSet[parsed.slot]; !ok {
			return nil, fmt.Errorf(
				"--cntlr %q: cntlid slot %d is not in --slots",
				spec, parsed.slot,
			)
		}
		if prev, ok := usedSlots[parsed.slot]; ok {
			return nil, fmt.Errorf(
				"--cntlr %q: cntlid slot %d already taken by cntlr %#x "+
					"(architecture.md §11.8 wants them distinct)",
				spec, parsed.slot, prev,
			)
		}
		usedSlots[parsed.slot] = parsed.cntlrId
		if prev, ok := usedCns[parsed.cnId]; ok {
			return nil, fmt.Errorf(
				"--cntlr %q: cn %#x already hosts a cntlr of this sp, "+
					"%#x (architecture.md §6.4)",
				spec, parsed.cnId, prev,
			)
		}
		usedCns[parsed.cnId] = parsed.cntlrId
		if parsed.primary {
			primaryCnt++
		}
		plan.cntlrs = append(plan.cntlrs, parsed)
	}
	if len(plan.cntlrs) == 0 {
		return nil, fmt.Errorf("at least one --cntlr is required")
	}
	if len(slots) < len(plan.cntlrs) {
		return nil, fmt.Errorf(
			"--slots has %d entries, fewer than the %d cntlrs",
			len(slots), len(plan.cntlrs),
		)
	}
	if primaryCnt != 1 {
		return nil, fmt.Errorf(
			"exactly one --cntlr must be primary, got %d", primaryCnt,
		)
	}

	sliceIdx := make(map[uint32]uint64)
	knownSlices := make(map[uint64]struct{})
	for _, spec := range sliceSpecs {
		parsed, err := parseSliceSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("--slice: %w", err)
		}
		if err := claim(parsed.sliceId, "slice"); err != nil {
			return nil, err
		}
		if prev, ok := sliceIdx[parsed.sliceIdx]; ok {
			return nil, fmt.Errorf(
				"--slice %q: slice_idx %d already used by slice %#x",
				spec, parsed.sliceIdx, prev,
			)
		}
		sliceIdx[parsed.sliceIdx] = parsed.sliceId
		knownSlices[parsed.sliceId] = struct{}{}
		plan.slices = append(plan.slices, parsed)
	}
	if len(plan.slices) == 0 {
		return nil, fmt.Errorf("at least one --slice is required")
	}

	knownGroups := make(map[uint64]groupSpec)
	for i, spec := range groupSpecs {
		parsed, err := parseGroupSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("--group: %w", err)
		}
		if err := claim(parsed.grpId, "group"); err != nil {
			return nil, err
		}
		if _, ok := knownSlices[parsed.sliceId]; !ok {
			return nil, fmt.Errorf(
				"--group %q: no --slice with id %#x", spec, parsed.sliceId,
			)
		}
		if i == 0 {
			plan.raid1 = parsed.raid1
		} else if parsed.raid1 != plan.raid1 {
			return nil, fmt.Errorf(
				"--group %q: every group of one sp must name the same "+
					"redundancy (SpConf.bdev_conf holds one redund_conf)",
				spec,
			)
		}
		knownGroups[parsed.grpId] = parsed
		plan.footprint += parsed.extCnt
		plan.groups = append(plan.groups, parsed)
	}
	if len(plan.groups) == 0 {
		return nil, fmt.Errorf("at least one --group is required")
	}

	legIdx := make(map[uint64]map[uint32]uint64)
	grpLegCnt := make(map[uint64]int)
	for _, spec := range legSpecs {
		parsed, err := parseLegSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("--leg: %w", err)
		}
		if err := claim(parsed.legId, "leg"); err != nil {
			return nil, err
		}
		grp, ok := knownGroups[parsed.grpId]
		if !ok {
			return nil, fmt.Errorf(
				"--leg %q: no --group with id %#x", spec, parsed.grpId,
			)
		}
		if legIdx[parsed.grpId] == nil {
			legIdx[parsed.grpId] = make(map[uint32]uint64)
		}
		if prev, ok := legIdx[parsed.grpId][parsed.legIdx]; ok {
			return nil, fmt.Errorf(
				"--leg %q: leg_idx %d already used by leg %#x in group %#x",
				spec, parsed.legIdx, prev, parsed.grpId,
			)
		}
		legIdx[parsed.grpId][parsed.legIdx] = parsed.legId
		grpLegCnt[parsed.grpId]++
		plan.grpOfLeg[parsed.legId] = parsed.grpId
		plan.extOfLeg[parsed.legId] = grp.extCnt
		plan.legs = append(plan.legs, parsed)
	}
	for _, grp := range plan.groups {
		if grpLegCnt[grp.grpId] == 0 {
			return nil, fmt.Errorf(
				"group %#x has no --leg", grp.grpId,
			)
		}
	}

	legSideCnt := make(map[uint64]int)
	grpDns := make(map[uint64]map[uint64]uint64)
	for _, spec := range sideSpecs {
		parsed, err := parseSideSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("--side: %w", err)
		}
		if err := claim(parsed.sideId, "side"); err != nil {
			return nil, err
		}
		grpId, ok := plan.grpOfLeg[parsed.legId]
		if !ok {
			return nil, fmt.Errorf(
				"--side %q: no --leg with id %#x", spec, parsed.legId,
			)
		}
		if _, ok := slotSet[parsed.slot]; !ok {
			return nil, fmt.Errorf(
				"--side %q: cntlid slot %d is not in --slots",
				spec, parsed.slot,
			)
		}
		if grpDns[grpId] == nil {
			grpDns[grpId] = make(map[uint64]uint64)
		}
		if prev, ok := grpDns[grpId][parsed.dnId]; ok {
			return nil, fmt.Errorf(
				"--side %q: dn %#x already carries side %#x of group %#x "+
					"(two legs of one group on one node have no redundancy)",
				spec, parsed.dnId, prev, grpId,
			)
		}
		grpDns[grpId][parsed.dnId] = parsed.sideId
		legSideCnt[parsed.legId]++
		plan.sides = append(plan.sides, parsed)
	}
	for _, leg := range plan.legs {
		if legSideCnt[leg.legId] == 0 {
			return nil, fmt.Errorf("leg %#x has no --side", leg.legId)
		}
	}
	return plan, nil
}

// cmdPutSp implements the CreateStoragePool STM with EXPLICIT PLACEMENT
// (dnv-worker.md §14.8, architecture.md §8.4). Everything §8.4 step 2 does is
// done here, with the §6.5 allocator replaced by the --cntlr/--side flags:
// SpConf (next_id past every id used, next_dev_id 1, bdev_conf inherited from
// ClusterConf, event_threshold, cntlid_slot_list, sp_level, deleting = false,
// every id list filled), SpName, one Cntlr per --cntlr, one Slice per --slice
// with its groups → legs → sides (provisioned = false, [D15]), the SpRev key
// with revision = 1, SpGlobal, and the DN/CN bookkeeping: every DN gains its
// side pointers, loses the group's ext_cnt, has its capacity key maintained
// (§5.6) and its DnRev bumped ONCE; every CN gains its cntlr pointer, loses
// the SP footprint, and likewise.
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
		"event_threshold as primary,cntlr,side,leg (0 = the §7 default)")
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
	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	shardCode := uint32(shard)
	slots, err := parseU32List(*slotSpec)
	if err != nil {
		die("--slots: %v", err)
	}
	if len(slots) == 0 {
		die("--slots must not be empty")
	}
	seenSlot := make(map[uint32]struct{}, len(slots))
	for _, slot := range slots {
		if slot >= common.CnCntlidSlotCnt {
			die("--slots: %d is >= CnCntlidSlotCnt (%d)",
				slot, common.CnCntlidSlotCnt)
		}
		if _, ok := seenSlot[slot]; ok {
			die("--slots: %d appears twice", slot)
		}
		seenSlot[slot] = struct{}{}
	}
	level, err := parseSpLevel(*levelSpec)
	if err != nil {
		die("--level: %v", err)
	}
	thresholdList, err := parseU32List(*thresholdSpec)
	if err != nil {
		die("--thresholds: %v", err)
	}
	if len(thresholdList) != 0 && len(thresholdList) != 4 {
		die("--thresholds wants four values, primary,cntlr,side,leg")
	}
	threshold := &pb.EventThreshold{}
	if len(thresholdList) == 4 {
		threshold = &pb.EventThreshold{
			PrimaryUnhealthy: thresholdList[0],
			CntlrUnhealthy:   thresholdList[1],
			SideUnhealthy:    thresholdList[2],
			LegUnhealthy:     thresholdList[3],
		}
	}
	plan, err := buildSpPlan(
		cntlrSpecs, sliceSpecs, groupSpecs, legSpecs, sideSpecs, slots,
	)
	if err != nil {
		die("put-sp: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cid, cc := g.clusterId(ctx, cli)

	// The SP's bdev_conf is the cluster's, with the redundancy the groups
	// name and the --lwm override (§8.4 "bdev_conf member-wise from
	// ClusterConf.bdev_conf then constants").
	bdev := cloneBdevConf(cc.GetBdevConf())
	if bdev.DmPoolConf == nil {
		bdev.DmPoolConf = &pb.DmPoolConf{}
	}
	if *lwm >= 0 {
		bdev.DmPoolConf.LowWaterMarkPct = uint32(*lwm)
	}
	chunkBlocks := bdev.GetRedundConf().GetRedundMdRaid1().
		GetBitmapChunkBlockCnt()
	if plan.raid1 {
		bdev.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: chunkBlocks,
				},
			},
		}
	} else {
		bdev.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
	}
	// The gateway's resolve-at-write, mirrored (§7): CreateStoragePool stores
	// the merge passed through model.ResolveBdevConf, so put-sp does too —
	// the cluster's bdev_conf is already concrete, but the SP's redund kind
	// was just rebuilt above and the merge's own constants rung still has to
	// fire.
	bdev = model.ResolveBdevConf(bdev)
	if err := model.ValidateClusterConf(cc); err != nil {
		die("put-sp: %v", err)
	}
	extentSize := cc.GetDnBinConf().GetExtentSize()

	// The §3.6 geometry of every group, computed by model.GroupBlocks — the
	// one implementation of the formula (MD6).
	type grpGeometry struct {
		metaBlocks uint64
		dataBlocks uint64
	}
	geometry := make(map[uint64]grpGeometry, len(plan.groups))
	grpReport := make([]any, 0, len(plan.groups))
	for _, grp := range plan.groups {
		metaBlocks, dataBlocks, err := model.GroupBlocks(
			grp.extCnt, extentSize, bdev,
		)
		if err != nil {
			die("put-sp: group %#x: %v", grp.grpId, err)
		}
		geometry[grp.grpId] = grpGeometry{metaBlocks, dataBlocks}
		kind := "data"
		if grp.isMeta {
			kind = "meta"
		}
		grpReport = append(grpReport, map[string]any{
			"grp_id":      idHex(grp.grpId),
			"slice_id":    idHex(grp.sliceId),
			"kind":        kind,
			"ext_cnt":     grp.extCnt,
			"meta_blocks": metaBlocks,
			"data_blocks": dataBlocks,
		})
	}

	// Resolve every named node to the endpoint its record is keyed by. The
	// STM re-reads each record and re-checks the id, so a node that moved
	// between the scan and the transaction fails the put instead of
	// corrupting the placement.
	dnAddr := make(map[uint64]string)
	dnOrder := make([]string, 0, len(plan.sides))
	dnCharge := make(map[string]uint64)
	dnPtrs := make(map[string][]*pb.SidePointer)
	dnIdOf := make(map[string]uint64)
	for _, side := range plan.sides {
		addr, ok := dnAddr[side.dnId]
		if !ok {
			addr = findNodeAddr(ctx, cli, cid, "dn_conf", side.dnId)
			dnAddr[side.dnId] = addr
			dnIdOf[addr] = side.dnId
			dnOrder = append(dnOrder, addr)
		}
		dnCharge[addr] += plan.extOfLeg[side.legId]
		dnPtrs[addr] = append(dnPtrs[addr], &pb.SidePointer{
			SpId:   uint64(id),
			LegId:  side.legId,
			SideId: side.sideId,
		})
	}
	cnAddr := make(map[uint64]string)
	cnOrder := make([]string, 0, len(plan.cntlrs))
	cnPtrs := make(map[string][]*pb.CntlrPointer)
	cnIdOf := make(map[string]uint64)
	for _, cntlr := range plan.cntlrs {
		addr, ok := cnAddr[cntlr.cnId]
		if !ok {
			addr = findNodeAddr(ctx, cli, cid, "cn_conf", cntlr.cnId)
			cnAddr[cntlr.cnId] = addr
			cnIdOf[addr] = cntlr.cnId
			cnOrder = append(cnOrder, addr)
		}
		cnPtrs[addr] = append(cnPtrs[addr], &pb.CntlrPointer{
			SpId:    uint64(id),
			CntlrId: cntlr.cntlrId,
		})
	}

	dnRevisions := make(map[string]uint64)
	cnRevisions := make(map[string]uint64)
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		confKey := model.SpConfKey(cid, *name)
		if s.Get(confKey, &pb.SpConf{}) {
			return fmt.Errorf("%q already exists", confKey)
		}
		nameKey := model.SpNameKey(cid, uint64(id))
		if s.Get(nameKey, &pb.SpName{}) {
			return fmt.Errorf("%q already exists", nameKey)
		}
		// --- the nodes, read once and used for both the copies and the
		// bookkeeping ---
		dns := make(map[string]*pb.DnConf, len(dnOrder))
		for _, addr := range dnOrder {
			dn := &pb.DnConf{}
			if !s.Get(model.DnConfKey(cid, addr), dn) {
				return fmt.Errorf("%q not found", model.DnConfKey(cid, addr))
			}
			if dn.GetDnId() != dnIdOf[addr] {
				return fmt.Errorf(
					"%q holds dn %#x, want %#x",
					addr, dn.GetDnId(), dnIdOf[addr],
				)
			}
			dns[addr] = dn
		}
		cns := make(map[string]*pb.CnConf, len(cnOrder))
		for _, addr := range cnOrder {
			cn := &pb.CnConf{}
			if !s.Get(model.CnConfKey(cid, addr), cn) {
				return fmt.Errorf("%q not found", model.CnConfKey(cid, addr))
			}
			if cn.GetCnId() != cnIdOf[addr] {
				return fmt.Errorf(
					"%q holds cn %#x, want %#x",
					addr, cn.GetCnId(), cnIdOf[addr],
				)
			}
			cns[addr] = cn
		}
		// --- the slices, built by joining the flags on their ids ---
		legs := make(map[uint64]*pb.Leg, len(plan.legs))
		groups := make(map[uint64]*pb.Group, len(plan.groups))
		slices := make(map[uint64]*pb.Slice, len(plan.slices))
		sliceIds := make([]uint64, 0, len(plan.slices))
		for _, spec := range plan.slices {
			slices[spec.sliceId] = &pb.Slice{SliceIdx: spec.sliceIdx}
			sliceIds = append(sliceIds, spec.sliceId)
		}
		for _, spec := range plan.groups {
			geo := geometry[spec.grpId]
			grp := &pb.Group{
				GrpId:      spec.grpId,
				ExtCnt:     spec.extCnt,
				MetaBlocks: geo.metaBlocks,
				DataBlocks: geo.dataBlocks,
			}
			groups[spec.grpId] = grp
			slice := slices[spec.sliceId]
			if spec.isMeta {
				slice.MetaGrpList = append(slice.MetaGrpList, grp)
			} else {
				slice.DataGrpList = append(slice.DataGrpList, grp)
			}
		}
		for _, spec := range plan.legs {
			leg := &pb.Leg{LegId: spec.legId, LegIdx: spec.legIdx}
			legs[spec.legId] = leg
			grp := groups[spec.grpId]
			grp.LegList = append(grp.LegList, leg)
		}
		for _, spec := range plan.sides {
			addr := dnAddr[spec.dnId]
			leg := legs[spec.legId]
			leg.SideList = append(leg.SideList, &pb.Side{
				SideId:      spec.sideId,
				AddrPort:    addr,
				CntlidSlot:  spec.slot,
				NvmeTrConf:  dns[addr].GetNvmeTrConf(),
				ErrEpoch:    0,
				Provisioned: false,
			})
		}
		for _, sliceId := range sliceIds {
			s.Put(model.SliceKey(cid, uint64(id), sliceId), slices[sliceId])
		}
		// --- the cntlrs ---
		cntlrIds := make([]uint64, 0, len(plan.cntlrs))
		for _, spec := range plan.cntlrs {
			addr := cnAddr[spec.cnId]
			s.Put(model.CntlrKey(cid, uint64(id), spec.cntlrId), &pb.Cntlr{
				AddrPort:   addr,
				NvmeTrConf: cns[addr].GetNvmeTrConf(),
				CntlidSlot: spec.slot,
				Primary:    spec.primary,
				Disabled:   false,
				ErrEpoch:   0,
			})
			cntlrIds = append(cntlrIds, spec.cntlrId)
		}
		// --- SpConf, SpName, SpRev ---
		s.Put(confKey, &pb.SpConf{
			SpId:           uint64(id),
			ShardCode:      shardCode,
			NextId:         plan.nextId,
			NextDevId:      1,
			BdevConf:       bdev,
			EventThreshold: threshold,
			CntlidSlotList: slots,
			SpLevel:        level,
			Deleting:       false,
			CntlrIdList:    cntlrIds,
			SliceIdList:    sliceIds,
		})
		s.Put(nameKey, &pb.SpName{SpName: *name})
		s.Put(model.SpRevKey(shardCode, cid, uint64(id)), &pb.SpRev{
			SpName:   *name,
			Revision: 1,
		})
		// --- the DN bookkeeping, once per DN (§8.4, §5.6, §5.5) ---
		for _, addr := range dnOrder {
			dn := dns[addr]
			charge := dnCharge[addr]
			if dn.GetFreeExtCnt() < charge {
				return fmt.Errorf(
					"dn %#x at %q has free_ext_cnt %d, needs %d",
					dn.GetDnId(), addr, dn.GetFreeExtCnt(), charge,
				)
			}
			newDn := proto.Clone(dn).(*pb.DnConf)
			newDn.SidePtrList = append(newDn.SidePtrList, dnPtrs[addr]...)
			newDn.FreeExtCnt -= charge
			s.Put(model.DnConfKey(cid, addr), newDn)
			model.MaintainDnCapacity(s, cid, addr, cc, dn, newDn)
			revision, err := bumpDnRev(s, cid, newDn)
			if err != nil {
				return err
			}
			dnRevisions[addr] = revision
		}
		// --- the CN bookkeeping, once per CN (§8.4, §8.6) ---
		for _, addr := range cnOrder {
			cn := cns[addr]
			if cn.GetFreeExtCnt() < plan.footprint {
				return fmt.Errorf(
					"cn %#x at %q has free_ext_cnt %d, needs the sp "+
						"footprint %d",
					cn.GetCnId(), addr, cn.GetFreeExtCnt(), plan.footprint,
				)
			}
			newCn := proto.Clone(cn).(*pb.CnConf)
			newCn.CntlrPtrList = append(newCn.CntlrPtrList, cnPtrs[addr]...)
			newCn.FreeExtCnt -= plan.footprint
			s.Put(model.CnConfKey(cid, addr), newCn)
			model.MaintainCnCapacity(s, cid, addr, cn, newCn)
			revision, err := bumpCnRev(s, cid, newCn)
			if err != nil {
				return err
			}
			cnRevisions[addr] = revision
		}
		// --- SpGlobal (§5.4) ---
		globalKey := model.SpGlobalKey(cid)
		global := &pb.SpGlobal{}
		if s.Get(globalKey, global) {
			if global.NextId <= uint64(id) {
				global.NextId = uint64(id) + 1
			}
			if int(shardCode) < len(global.ShardBucket) {
				global.ShardBucket[shardCode]++
			}
			s.Put(globalKey, global)
		}
		return nil
	})
	if err != nil {
		die("put-sp: %v", err)
	}

	cntlrIds := make([]string, 0, len(plan.cntlrs))
	for _, spec := range plan.cntlrs {
		cntlrIds = append(cntlrIds, idHex(spec.cntlrId))
	}
	sliceIds := make([]string, 0, len(plan.slices))
	for _, spec := range plan.slices {
		sliceIds = append(sliceIds, idHex(spec.sliceId))
	}
	legIds := make([]string, 0, len(plan.legs))
	for _, spec := range plan.legs {
		legIds = append(legIds, idHex(spec.legId))
	}
	sideIds := make([]string, 0, len(plan.sides))
	for _, spec := range plan.sides {
		sideIds = append(sideIds, idHex(spec.sideId))
	}
	emit(map[string]any{
		"sp_id":         idHex(uint64(id)),
		"sp_name":       *name,
		"shard_code":    fmt.Sprintf(common.ShardCodeFmt, shardCode),
		"next_id":       plan.nextId,
		"revision":      uint64(1),
		"footprint":     plan.footprint,
		"cntlr_id_list": cntlrIds,
		"slice_id_list": sliceIds,
		"leg_id_list":   legIds,
		"side_id_list":  sideIds,
		"groups":        grpReport,
		"dn_revisions":  dnRevisions,
		"cn_revisions":  cnRevisions,
	})
}

// ---------------------------------------------------------------------------
// SpConf sub-object writes
// ---------------------------------------------------------------------------

// advanceNextId keeps SpConf.next_id past every id the script assigned, so
// that a worker reaction allocates fresh ids ABOVE them (§14.5, §5.4).
func advanceNextId(conf *pb.SpConf, ids ...uint64) {
	for _, id := range ids {
		if conf.NextId <= id {
			conf.NextId = id + 1
		}
	}
}

// containsName reports whether a name list already holds name.
func containsName(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// cmdPutTd writes one ThinDevice and its SpConf.td_name_list entry
// (architecture.md §8.7 CreateThinDevice), then bumps SpRev once.
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
	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	assignedDev := uint32(0)
	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetTdNameList(), *name) {
			return fmt.Errorf("td %q already exists", *name)
		}
		key := model.ThinDeviceKey(cid, target.spId, *name)
		if s.Get(key, &pb.ThinDevice{}) {
			return fmt.Errorf("%q already exists", key)
		}
		assignedDev = uint32(devId)
		if assignedDev == 0 {
			assignedDev = conf.GetNextDevId()
		}
		if conf.NextDevId <= assignedDev {
			conf.NextDevId = assignedDev + 1
		}
		s.Put(key, &pb.ThinDevice{
			TdId:    uint64(id),
			DevId:   assignedDev,
			OriId:   0,
			Size:    *size,
			Created: false,
		})
		conf.TdNameList = append(conf.TdNameList, *name)
		advanceNextId(conf, uint64(id))
		s.Put(model.SpConfKey(cid, target.name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("put-td: %v", err)
	}
	emit(map[string]any{
		"sp_name": target.name,
		"td_name": *name,
		"td_id":   idHex(uint64(id)),
		"dev_id":  assignedDev,
		"sp_rev":  spRev,
	})
}

// cmdPutSs writes one Subsystem, its SpConf.nqn_list entry and the SP's
// CdcEntry (architecture.md §8.8 CreateSubsystem, §12): the discovery entry is
// keyed by the SP's shard code and lists every cntlr's transport, and it is
// what case D3 reads back after a cntlr replacement.
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
	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	namespaces := make([]nsSpec, 0, len(nsSpecs))
	seenIdx := make(map[uint32]uint64)
	for _, spec := range nsSpecs {
		parsed, err := parseNsSpec(spec)
		if err != nil {
			die("--ns: %v", err)
		}
		if parsed.nsIdx == 0 {
			die("--ns %q: ns_idx 0 is never valid", spec)
		}
		if prev, ok := seenIdx[parsed.nsIdx]; ok {
			die("--ns %q: ns_idx %d already used by ns %#x",
				spec, parsed.nsIdx, prev)
		}
		seenIdx[parsed.nsIdx] = parsed.nsId
		namespaces = append(namespaces, parsed)
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetNqnList(), *nqn) {
			return fmt.Errorf("subsystem %q already exists", *nqn)
		}
		key := model.SubsystemKey(cid, target.spId, *nqn)
		if s.Get(key, &pb.Subsystem{}) {
			return fmt.Errorf("%q already exists", key)
		}
		subsystem := &pb.Subsystem{
			SsId:   uint64(id),
			Serial: *serial,
			Model:  *model_,
		}
		advanceNextId(conf, uint64(id))
		for _, spec := range namespaces {
			uuid, nguid := nsIdentity(cid, target.spId, spec.nsId)
			subsystem.NsList = append(subsystem.NsList, &pb.Namespace{
				NsId:      spec.nsId,
				NsIdx:     spec.nsIdx,
				TdId:      spec.tdId,
				DevUuid:   uuid,
				DevNguid:  nguid,
				Suspended: false,
			})
			advanceNextId(conf, spec.nsId)
		}
		s.Put(key, subsystem)
		// The discovery entry advertises every cntlr of the SP (§12).
		trList := make([]*pb.NvmeTrConf, 0, len(conf.GetCntlrIdList()))
		for _, cntlrId := range conf.GetCntlrIdList() {
			cntlr := &pb.Cntlr{}
			if !s.Get(model.CntlrKey(cid, target.spId, cntlrId), cntlr) {
				return fmt.Errorf("cntlr %#x not found", cntlrId)
			}
			trList = append(trList, cntlr.GetNvmeTrConf())
		}
		s.Put(
			model.CdcEntryKey(
				cid, conf.GetShardCode(), target.spId, uint64(id),
			),
			&pb.CdcEntry{Nqn: *nqn, NvmeTrConfList: trList},
		)
		conf.NqnList = append(conf.NqnList, *nqn)
		s.Put(model.SpConfKey(cid, target.name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("put-ss: %v", err)
	}
	nsIds := make([]string, 0, len(namespaces))
	for _, spec := range namespaces {
		nsIds = append(nsIds, idHex(spec.nsId))
	}
	emit(map[string]any{
		"sp_name": target.name,
		"nqn":     *nqn,
		"ss_id":   idHex(uint64(id)),
		"ns_ids":  nsIds,
		"cdc_key": model.CdcEntryKey(cid, target.shard, target.spId, uint64(id)),
		"sp_rev":  spRev,
	})
}

// cmdPutClone writes one Clone and its SpConf.clone_name_list entry
// (architecture.md §8.9), then bumps SpRev once. --dst-td accepts either a td
// name or a td_id.
func cmdPutClone(g *globals, args []string) {
	fs := newFlagSet("put-clone", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "clone_name (required)")
	var id hexUint
	fs.Var(&id, "id", "clone_id (required)")
	dstTd := fs.String("dst-td", "", "destination td name or td_id (required)")
	srcSliceCnt := fs.Uint("src-slice-cnt", 0, "Clone.src_slice_cnt")
	srcNqn := fs.String("src-nqn", "", "Clone.src_nqn")
	srcNsIdx := fs.Uint("src-ns-idx", 0, "Clone.src_ns_idx")
	srcStripe := fs.Uint64("src-stripe-size", 0, "Clone.src_stripe_size")
	srcBlock := fs.Uint64("src-block-size", 0, "Clone.src_block_size")
	fs.Parse(args)

	if *name == "" {
		die("--name is required")
	}
	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	if *dstTd == "" {
		die("--dst-td is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	dstTdId := uint64(0)
	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetCloneNameList(), *name) {
			return fmt.Errorf("clone %q already exists", *name)
		}
		td := &pb.ThinDevice{}
		if s.Get(model.ThinDeviceKey(cid, target.spId, *dstTd), td) {
			dstTdId = td.GetTdId()
		} else {
			parsed, perr := parseId(*dstTd)
			if perr != nil {
				return fmt.Errorf("--dst-td %q: no such td", *dstTd)
			}
			dstTdId = parsed
		}
		key := model.CloneKey(cid, target.spId, *name)
		if s.Get(key, &pb.Clone{}) {
			return fmt.Errorf("%q already exists", key)
		}
		s.Put(key, &pb.Clone{
			CloneId:       uint64(id),
			SrcNqn:        *srcNqn,
			SrcNsIdx:      uint32(*srcNsIdx),
			SrcSliceCnt:   uint32(*srcSliceCnt),
			SrcStripeSize: *srcStripe,
			SrcBlockSize:  *srcBlock,
			DstTdId:       dstTdId,
			BmCnt:         0,
		})
		conf.CloneNameList = append(conf.CloneNameList, *name)
		advanceNextId(conf, uint64(id))
		s.Put(model.SpConfKey(cid, target.name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("put-clone: %v", err)
	}
	emit(map[string]any{
		"sp_name":    target.name,
		"clone_name": *name,
		"clone_id":   idHex(uint64(id)),
		"dst_td_id":  idHex(dstTdId),
		"sp_rev":     spRev,
	})
}

// cmdPutXfer writes one Transfer and its SpConf.xfer_name_list entry
// (architecture.md §8.10), then bumps SpRev once.
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
	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	if *oriNqn == "" {
		die("--ori-nqn is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetXferNameList(), *name) {
			return fmt.Errorf("transfer %q already exists", *name)
		}
		key := model.TransferKey(cid, target.spId, *name)
		if s.Get(key, &pb.Transfer{}) {
			return fmt.Errorf("%q already exists", key)
		}
		s.Put(key, &pb.Transfer{
			XferId:   uint64(id),
			OriNqn:   *oriNqn,
			OriNsIdx: uint32(*oriNsIdx),
		})
		conf.XferNameList = append(conf.XferNameList, *name)
		advanceNextId(conf, uint64(id))
		s.Put(model.SpConfKey(cid, target.name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("put-xfer: %v", err)
	}
	emit(map[string]any{
		"sp_name":   target.name,
		"xfer_name": *name,
		"xfer_id":   idHex(uint64(id)),
		"sp_rev":    spRev,
	})
}

// cmdPutMigr writes one Migration and its SpConf.migr_name_list entry
// (architecture.md §8.11) AND appends the destination Side to the SOURCE
// side's leg, which is what makes the leg carry two sides — the shape RW15
// keys migr_src_conf/migr_dst_conf off and the precondition AR8 reads as "a
// leg with two sides is left alone". The destination DN gets the full §8.4
// treatment: the side pointer, the group's ext_cnt off its budget, its
// capacity key maintained and its DnRev bumped once.
func cmdPutMigr(g *globals, args []string) {
	fs := newFlagSet("put-migr", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	migr := fs.String("migr", "", migrForm+" (required)")
	var dstDn hexUint
	fs.Var(&dstDn, "dst-dn", "dn_id hosting the destination side (required)")
	dstSlot := fs.Uint("dst-slot", 0, "the destination side's cntlid_slot")
	fs.Parse(args)

	spec, err := parseMigrSpec(*migr)
	if err != nil {
		die("--migr: %v", err)
	}
	if spec.migrId == 0 || spec.srcSide == 0 || spec.dstSide == 0 {
		die("--migr %q: ids must not be 0", *migr)
	}
	if uint64(dstDn) == 0 {
		die("--dst-dn is required and must not be 0")
	}

	ctx, done, cli := g.open()
	defer done()
	cid, cc := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)
	dstAddr := findNodeAddr(ctx, cli, cid, "dn_conf", uint64(dstDn))

	var legId, grpId, sliceId, extCnt, dnRev, spRev uint64
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		legId, grpId, sliceId, extCnt = 0, 0, 0, 0
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if containsName(conf.GetMigrNameList(), spec.name) {
			return fmt.Errorf("migration %q already exists", spec.name)
		}
		dn := &pb.DnConf{}
		if !s.Get(model.DnConfKey(cid, dstAddr), dn) {
			return fmt.Errorf("%q not found", model.DnConfKey(cid, dstAddr))
		}
		if dn.GetDnId() != uint64(dstDn) {
			return fmt.Errorf(
				"%q holds dn %#x, want %#x",
				dstAddr, dn.GetDnId(), uint64(dstDn),
			)
		}
		// Find the leg that carries the source side.
		var slice *pb.Slice
		var leg *pb.Leg
		for _, candidateId := range conf.GetSliceIdList() {
			candidate := &pb.Slice{}
			key := model.SliceKey(cid, target.spId, candidateId)
			if !s.Get(key, candidate) {
				return fmt.Errorf("%q not found", key)
			}
			grpLists := [][]*pb.Group{
				candidate.GetMetaGrpList(),
				candidate.GetDataGrpList(),
			}
			for _, grpList := range grpLists {
				for _, grp := range grpList {
					for _, candidateLeg := range grp.GetLegList() {
						for _, side := range candidateLeg.GetSideList() {
							if side.GetSideId() != spec.srcSide {
								continue
							}
							slice = candidate
							leg = candidateLeg
							legId = candidateLeg.GetLegId()
							grpId = grp.GetGrpId()
							sliceId = candidateId
							extCnt = grp.GetExtCnt()
						}
					}
				}
			}
		}
		if leg == nil {
			return fmt.Errorf(
				"no side %#x in storage pool %q", spec.srcSide, target.name,
			)
		}
		for _, side := range leg.GetSideList() {
			if side.GetSideId() == spec.dstSide {
				return fmt.Errorf(
					"leg %#x already carries side %#x", legId, spec.dstSide,
				)
			}
		}
		leg.SideList = append(leg.SideList, &pb.Side{
			SideId:      spec.dstSide,
			AddrPort:    dstAddr,
			CntlidSlot:  uint32(*dstSlot),
			NvmeTrConf:  dn.GetNvmeTrConf(),
			ErrEpoch:    0,
			Provisioned: false,
		})
		s.Put(model.SliceKey(cid, target.spId, sliceId), slice)
		s.Put(model.MigrationKey(cid, target.spId, spec.name), &pb.Migration{
			MigrId:    spec.migrId,
			SrcSideId: spec.srcSide,
			DstSideId: spec.dstSide,
			BmCnt:     0,
		})
		conf.MigrNameList = append(conf.MigrNameList, spec.name)
		advanceNextId(conf, spec.migrId, spec.dstSide)
		s.Put(model.SpConfKey(cid, target.name), conf)
		// The destination DN's bookkeeping (§8.4, §5.6, §5.5).
		if dn.GetFreeExtCnt() < extCnt {
			return fmt.Errorf(
				"dn %#x at %q has free_ext_cnt %d, needs %d",
				dn.GetDnId(), dstAddr, dn.GetFreeExtCnt(), extCnt,
			)
		}
		newDn := proto.Clone(dn).(*pb.DnConf)
		newDn.SidePtrList = append(newDn.SidePtrList, &pb.SidePointer{
			SpId:   target.spId,
			LegId:  legId,
			SideId: spec.dstSide,
		})
		newDn.FreeExtCnt -= extCnt
		s.Put(model.DnConfKey(cid, dstAddr), newDn)
		model.MaintainDnCapacity(s, cid, dstAddr, cc, dn, newDn)
		if dnRev, err = bumpDnRev(s, cid, newDn); err != nil {
			return err
		}
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("put-migr: %v", err)
	}
	emit(map[string]any{
		"sp_name":     target.name,
		"migr_name":   spec.name,
		"migr_id":     idHex(spec.migrId),
		"src_side_id": idHex(spec.srcSide),
		"dst_side_id": idHex(spec.dstSide),
		"leg_id":      idHex(legId),
		"grp_id":      idHex(grpId),
		"slice_id":    idHex(sliceId),
		"dn_id":       idHex(uint64(dstDn)),
		"addr_port":   dstAddr,
		"ext_cnt":     extCnt,
		"dn_revision": dnRev,
		"sp_rev":      spRev,
	})
}

// cmdPutBitmap writes one chunk of a clone's or a migration's bitmap and
// raises bm_cnt on the parent when the chunk index is new (architecture.md
// §8.9/§8.11, BM3), then bumps SpRev once. Rewriting an existing chunk leaves
// bm_cnt alone but still bumps SpRev — which is exactly the "grown chunk"
// trigger of §14.11 C5.
func cmdPutBitmap(g *globals, args []string) {
	fs := newFlagSet("put-bitmap", g)
	kind := fs.String("kind", "", "clone|migr (required)")
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	name := fs.String("name", "", "clone_name or migr_name (required)")
	bmIdx := fs.Uint("bm-idx", 0, "the chunk index")
	hexData := fs.String("hex", "", "the chunk payload as hex (required)")
	fs.Parse(args)

	if *kind != "clone" && *kind != "migr" {
		usageDie("--kind wants clone or migr, got %q", *kind)
	}
	if *name == "" {
		die("--name is required")
	}
	if *bmIdx >= 1<<8 {
		// The key field is common.BmIdxFmt, exactly two hex digits, so an
		// index of 256 or more could not be parsed back (model.ParseBmIdx).
		die("--bm-idx %d does not fit in the %q key field",
			*bmIdx, common.BmIdxFmt)
	}
	data, err := parseHexBitmap(*hexData)
	if err != nil {
		die("--hex: %v", err)
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	var bmCnt uint32
	var spRev uint64
	created := false
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if *kind == "clone" {
			parentKey := model.CloneKey(cid, target.spId, *name)
			parent := &pb.Clone{}
			if !s.Get(parentKey, parent) {
				return fmt.Errorf("%q not found", parentKey)
			}
			key := model.CloneBitmapKey(
				cid, target.spId, *name, uint32(*bmIdx),
			)
			created = !s.Get(key, &pb.CloneBitmap{})
			s.Put(key, &pb.CloneBitmap{Bitmap: data})
			if created {
				parent.BmCnt++
				s.Put(parentKey, parent)
			}
			bmCnt = parent.GetBmCnt()
		} else {
			parentKey := model.MigrationKey(cid, target.spId, *name)
			parent := &pb.Migration{}
			if !s.Get(parentKey, parent) {
				return fmt.Errorf("%q not found", parentKey)
			}
			key := model.MigrBitmapKey(
				cid, target.spId, *name, uint32(*bmIdx),
			)
			created = !s.Get(key, &pb.MigrBitmap{})
			s.Put(key, &pb.MigrBitmap{Bitmap: data})
			if created {
				parent.BmCnt++
				s.Put(parentKey, parent)
			}
			bmCnt = parent.GetBmCnt()
		}
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("put-bitmap: %v", err)
	}
	emit(map[string]any{
		"sp_name": target.name,
		"kind":    *kind,
		"name":    *name,
		"bm_idx":  uint32(*bmIdx),
		"bytes":   len(data),
		"created": created,
		"bm_cnt":  bmCnt,
		"sp_rev":  spRev,
	})
}

// ---------------------------------------------------------------------------
// set-*
// ---------------------------------------------------------------------------

// cmdSetCntlr rewrites one Cntlr's primary / disabled flags and bumps SpRev
// once. The two flags are independently settable and a tri-state, so that
// "not given" differs from "false" (§14.8) — case C7 flips the primary role
// with two calls and case D10 disables a standby without touching its role.
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

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	if !primary.set && !disabled.set {
		die("at least one of --primary / --disabled is required")
	}

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	var stored *pb.Cntlr
	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		key := model.CntlrKey(cid, target.spId, uint64(id))
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			return fmt.Errorf("%q not found", key)
		}
		if primary.set {
			cntlr.Primary = primary.value
		}
		if disabled.set {
			cntlr.Disabled = disabled.value
		}
		s.Put(key, cntlr)
		stored = cntlr
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("set-cntlr: %v", err)
	}
	emit(map[string]any{
		"sp_name":  target.name,
		"cntlr_id": idHex(uint64(id)),
		"cntlr":    pbToAny(stored),
		"sp_rev":   spRev,
	})
}

// cmdSetLevel writes SpConf.sp_level and bumps SpRev once (architecture.md
// §8.4 UpdateStoragePoolLevel): the level rides in both Syncup requests, and
// AR3 makes NO_THINPOOL and above suppress every reaction — which is what case
// D10 proves.
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
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	spRev := uint64(0)
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		conf.SpLevel = level
		s.Put(model.SpConfKey(cid, target.name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("set-level: %v", err)
	}
	emit(map[string]any{
		"sp_name":  target.name,
		"sp_level": level.String(),
		"level":    int32(level),
		"sp_rev":   spRev,
	})
}

// cmdSetLwm writes SpConf.bdev_conf.dm_pool_conf.low_water_mark_pct and bumps
// SpRev once: the lever case D5/D6 pulls to arm and disarm the §8.5 grow rule
// (a value above 100 means "never grow automatically").
func cmdSetLwm(g *globals, args []string) {
	fs := newFlagSet("set-lwm", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	pct := fs.Uint("pct", 0, "low_water_mark_pct")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	// The gateway's rule, mirrored (§7): 0 asks for the default and is
	// resolved before the write, because the worker refuses a stored 0. A
	// value above 100 is a MEANING — "never grow automatically" — and is
	// stored exactly as given.
	pctValue := uint32(*pct)
	if pctValue == 0 {
		pctValue = common.DefaultPoolLowWatermarkPct
	}

	spRev := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		if conf.BdevConf == nil {
			conf.BdevConf = &pb.BdevConf{}
		}
		if conf.BdevConf.DmPoolConf == nil {
			conf.BdevConf.DmPoolConf = &pb.DmPoolConf{}
		}
		conf.BdevConf.DmPoolConf.LowWaterMarkPct = pctValue
		s.Put(model.SpConfKey(cid, target.name), conf)
		spRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		die("set-lwm: %v", err)
	}
	emit(map[string]any{
		"sp_name":            target.name,
		"low_water_mark_pct": pctValue,
		"sp_rev":             spRev,
	})
}

// cmdSetFree rewrites a node's free_ext_cnt and maintains its capacity key in
// the same STM (§5.6) — and bumps NO revision: a budget change is an allocator
// input, not agent-visible desired state (§5.5). It is the script's lever for
// deterministic allocation (§14.5).
func cmdSetFree(g *globals, args []string) {
	fs := newFlagSet("set-free", g)
	kind, rest := splitKind(args)
	var id hexUint
	fs.Var(&id, "id", "dn_id / cn_id")
	freeExt := fs.Uint64("free-ext", 0, "the new free_ext_cnt")
	fs.Parse(rest)
	kind = kindArg(fs, kind, "dn", "cn")

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}

	ctx, done, cli := g.open()
	defer done()
	cid, cc := g.clusterId(ctx, cli)

	allocatable := false
	addr := ""
	if kind == "dn" {
		addr = findNodeAddr(ctx, cli, cid, "dn_conf", uint64(id))
		err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
			key := model.DnConfKey(cid, addr)
			dn := &pb.DnConf{}
			if !s.Get(key, dn) {
				return fmt.Errorf("%q not found", key)
			}
			newDn := proto.Clone(dn).(*pb.DnConf)
			newDn.FreeExtCnt = *freeExt
			s.Put(key, newDn)
			model.MaintainDnCapacity(s, cid, addr, cc, dn, newDn)
			allocatable = model.DnAllocatable(newDn, cc.GetDnBinConf())
			return nil
		})
		if err != nil {
			die("set-free: %v", err)
		}
	} else {
		addr = findNodeAddr(ctx, cli, cid, "cn_conf", uint64(id))
		err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
			key := model.CnConfKey(cid, addr)
			cn := &pb.CnConf{}
			if !s.Get(key, cn) {
				return fmt.Errorf("%q not found", key)
			}
			newCn := proto.Clone(cn).(*pb.CnConf)
			newCn.FreeExtCnt = *freeExt
			s.Put(key, newCn)
			model.MaintainCnCapacity(s, cid, addr, cn, newCn)
			allocatable = model.CnAllocatable(newCn)
			return nil
		})
		if err != nil {
			die("set-free: %v", err)
		}
	}
	emit(map[string]any{
		"kind":         kind,
		"id":           idHex(uint64(id)),
		"addr_port":    addr,
		"free_ext_cnt": *freeExt,
		"allocatable":  allocatable,
	})
}

// ---------------------------------------------------------------------------
// Playing the worker (gateway.md §2.4)
// ---------------------------------------------------------------------------
//
// These are the ONLY writes the gateway integration suite performs through
// workerctl (gateway.md §10.9). They exist because two gateway preconditions
// are gated on a flag only the sp-worker ever sets — CreateThinDevice's
// snapshot gate on ThinDevice.created (§8.7) and SwitchSpareLeg's gate on
// Side.provisioned (§9.4) — and that suite deliberately runs no dnv-worker.
// Both go through the very model op the worker calls, so the state they leave
// behind is exactly the state a converging worker would have produced,
// including the single SpRev bump the flip owes (which the suite's rev
// bookkeeping must count).

// cmdSetCreated flips ThinDevice.created through model.FlipCreated: the
// materialization the sp-worker performs once a cntlr has reported the td's
// thin volume RES_STATUS_OK in every slice (§10.3, ThinDeviceCreated.md U3).
//
// The td_id comes from the stored record rather than from a flag: FlipCreated
// takes a TdRef of name AND id precisely so a td deleted and re-created under
// the same name between the read and the transaction is skipped instead of
// silently flipped, and a caller that could pass a wrong id would defeat that.
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
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	key := model.ThinDeviceKey(cid, target.spId, *name)
	td := &pb.ThinDevice{}
	found, err := cli.Get(ctx, key, td)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("key %q not found", key)
	}

	flipped, err := model.FlipCreated(
		ctx, cli, cid, target.shard, target.spId,
		[]model.TdRef{{Name: *name, TdId: td.GetTdId()}},
	)
	if err != nil {
		die("set-created: %v", err)
	}
	emit(map[string]any{
		"td_name": *name,
		"td_id":   idHex(td.GetTdId()),
		"flipped": len(flipped) == 1,
		"sp_rev":  readSpRev(ctx, cli, cid, target),
	})
}

// cmdSetProvisioned flips one Side.provisioned through model.FlipProvisioned:
// the §9.4 completion the sp-worker records once the dn agent reports the side
// fully zeroed. The suite pulls it exactly where a gateway precondition
// demands it — SwitchSpareLeg refuses an unprovisioned spare (§8.12).
func cmdSetProvisioned(g *globals, args []string) {
	fs := newFlagSet("set-provisioned", g)
	sp := fs.String("sp", "", "sp name or sp_id (required)")
	var slice, leg, side hexUint
	fs.Var(&slice, "slice", "slice_id (required)")
	fs.Var(&leg, "leg", "leg_id (required)")
	fs.Var(&side, "side", "side_id (required)")
	fs.Parse(args)

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	ref := model.SideRef{
		SliceId: uint64(slice),
		LegId:   uint64(leg),
		SideId:  uint64(side),
	}
	flipped, err := model.FlipProvisioned(
		ctx, cli, cid, target.shard, target.spId, []model.SideRef{ref},
	)
	if err != nil {
		die("set-provisioned: %v", err)
	}
	emit(map[string]any{
		"slice_id": idHex(ref.SliceId),
		"leg_id":   idHex(ref.LegId),
		"side_id":  idHex(ref.SideId),
		"flipped":  len(flipped) == 1,
		"sp_rev":   readSpRev(ctx, cli, cid, target),
	})
}

// readSpRev reports the SP's revision after a flip, so the caller can refresh
// the optimistic-concurrency token the flip invalidated without a second
// round trip through get-rev.
func readSpRev(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	target spTarget,
) uint64 {
	rev := &pb.SpRev{}
	found, err := cli.Get(
		ctx, model.SpRevKey(target.shard, cid, target.spId), rev)
	if err != nil {
		die("%v", err)
	}
	if !found {
		die("sp_rev of %q not found", target.name)
	}
	return rev.GetRevision()
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// cmdGet reads any key and prints it as protojson, choosing the message type
// from the key's SECOND field — the §5.3 table's "message kind" column.
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
	cid, _ := g.clusterId(ctx, cli)

	endpoint := *addr
	if endpoint == "" {
		if uint64(id) == 0 {
			die("--id or --addr is required")
		}
		endpoint = findNodeAddr(ctx, cli, cid, "dn_conf", uint64(id))
	}
	dn := &pb.DnConf{}
	found, err := cli.Get(ctx, model.DnConfKey(cid, endpoint), dn)
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
	cid, _ := g.clusterId(ctx, cli)

	endpoint := *addr
	if endpoint == "" {
		if uint64(id) == 0 {
			die("--id or --addr is required")
		}
		endpoint = findNodeAddr(ctx, cli, cid, "cn_conf", uint64(id))
	}
	cn := &pb.CnConf{}
	found, err := cli.Get(ctx, model.CnConfKey(cid, endpoint), cn)
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

	if uint64(id) == 0 {
		die("--id is required and must not be 0")
	}
	shardCode := uint32(shard)

	ctx, done, cli := g.open()
	defer done()
	cid, _ := g.clusterId(ctx, cli)

	var key string
	var msg proto.Message
	switch kind {
	case "dn":
		key, msg = model.DnRevKey(shardCode, cid, uint64(id)), &pb.DnRev{}
	case "cn":
		key, msg = model.CnRevKey(shardCode, cid, uint64(id)), &pb.CnRev{}
	case "sp":
		key, msg = model.SpRevKey(shardCode, cid, uint64(id)), &pb.SpRev{}
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
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, spec)

	state, err := model.LoadSp(ctx, cli, cid, target.name)
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
	bmIdx := func(chunks map[string][]model.BmChunk) map[string]any {
		out := make(map[string]any, len(chunks))
		for name, list := range chunks {
			idxList := make([]uint32, 0, len(list))
			for _, chunk := range list {
				idxList = append(idxList, chunk.Idx)
			}
			out[name] = idxList
		}
		return out
	}
	emit(map[string]any{
		"sp_name":      target.name,
		"store_rev":    state.Rev,
		"sp_conf":      pbToAny(state.Conf),
		"cntlrs":       cntlrs,
		"slices":       slices,
		"tds":          tds,
		"subsystems":   subsystems,
		"clones":       clones,
		"xfers":        xfers,
		"migrs":        migrs,
		"clone_bm_idx": bmIdx(state.CloneBmIdx),
		"migr_bm_idx":  bmIdx(state.MigrBmIdx),
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
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	if uint64(id) != 0 {
		key := model.CntlrKey(cid, target.spId, uint64(id))
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
	state, err := model.LoadSp(ctx, cli, cid, target.name)
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
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	if uint64(id) != 0 {
		key := model.SliceKey(cid, target.spId, uint64(id))
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
	state, err := model.LoadSp(ctx, cli, cid, target.name)
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
	cid, _ := g.clusterId(ctx, cli)
	target := resolveSp(ctx, cli, cid, *sp)

	if *name != "" {
		key := model.ThinDeviceKey(cid, target.spId, *name)
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
	state, err := model.LoadSp(ctx, cli, cid, target.name)
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
// either a full key prefix ("dnv dn_conf …") or one §5.3 kind, which is then
// scoped to --cluster when the kind's key carries {cluster_id} right after it.
func cmdListKeys(g *globals, args []string) {
	fs := newFlagSet("list-keys", g)
	prefix := fs.String("prefix", "", "a full key prefix or a §5.3 key kind")
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
		// space of a "dnv " the caller may have quoted, and §14.13's
		// diagnostics dump asks for exactly `list-keys --prefix dnv`, so a
		// bare prefix must not be mistaken for a §5.3 kind name.
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
			cid, _ := g.clusterId(ctx, cli)
			scan = keyPrefix(common.DnvPrefix, spec, idHex(cid))
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
// of the key and the epoch out of the value (§5.3, VW2). It is how case E
// checks that a dead worker's key is gone (VW6).
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
