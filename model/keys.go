// Package model is the architecture.md §5 etcd data model expressed as Go
// (dnv-worker.md §4): key formats and their parsers, the §5.2 cluster_id, the
// §5.6 capacity keys, the §6 allocator and the internal §8/§10 mutations. It
// is shared by the worker now and the gateway later, so that both build the
// same keys and enforce the same invariants.
//
// model imports common, pb and etcdutil only (layout.md §3). It never dials an
// agent, never sleeps, and logs nothing of its own beyond the etcdutil records
// its reads and writes produce.
package model

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The second field of every key: the message kind (architecture.md §5.3). They
// are literals of the on-disk format and never change.
const (
	kindClusterConf = "cluster_conf"
	kindDnGlobal    = "dn_global"
	kindCnGlobal    = "cn_global"
	kindSpGlobal    = "sp_global"
	kindDnRev       = "dn_rev"
	kindCnRev       = "cn_rev"
	kindSpRev       = "sp_rev"
	kindDnConf      = "dn_conf"
	kindCnConf      = "cn_conf"
	kindDnCapacity  = "dn_capacity"
	kindCnCapacity  = "cn_capacity"
	kindCdcEntry    = "cdc"
	kindSpConf      = "sp_conf"
	kindCntlr       = "cntlr"
	kindSlice       = "slice"
	kindThinDevice  = "thin_device"
	kindSubsystem   = "subsystem"
	kindClone       = "clone"
	kindCloneBitmap = "clone_bitmap"
	kindTransfer    = "transfer"
	kindMigration   = "migration"
	kindMigrBitmap  = "migration_bitmap"
	kindSpName      = "sp_id_to_name"
	kindWorkerReg   = "worker"
)

// keySep is the single space every key field is joined by (architecture.md
// §5.1). No key field may contain it: names match common.ValidStrPattern and
// every other field is hex.
const keySep = " "

// ---------------------------------------------------------------------------
// Field formatting (architecture.md §5.1)
// ---------------------------------------------------------------------------

// joinKey builds a key out of its fields (MD2): one space between two fields,
// none at either end.
func joinKey(fields ...string) string {
	return strings.Join(fields, keySep)
}

// prefixOf builds the scan/watch prefix of a key whose leading fields are
// given (MD2): a prefix always ENDS in one space, so that it can never match a
// longer sibling field.
func prefixOf(fields ...string) string {
	return joinKey(fields...) + keySep
}

// idField renders any id with common.IdKeyFmt — cluster, dn, cn, sp, cntlr,
// slice and subsystem ids alike (architecture.md §5.1).
func idField(id uint64) string {
	return fmt.Sprintf(common.IdKeyFmt, id)
}

// shardField renders a shard code with common.ShardCodeFmt.
func shardField(shard uint32) string {
	return fmt.Sprintf(common.ShardCodeFmt, shard)
}

// binField renders a DN bin index with common.BinIdxFmt (§6.2: 0…3).
func binField(binIdx uint32) string {
	return fmt.Sprintf(common.BinIdxFmt, binIdx)
}

// freeField renders a free extent count with common.FreeSpaceFmt, whose
// zero-padding makes lexical key order equal numeric order (§5.6).
func freeField(freeExt uint64) string {
	return fmt.Sprintf(common.FreeSpaceFmt, freeExt)
}

// bmIdxField renders a bitmap chunk index with common.BmIdxFmt.
func bmIdxField(bmIdx uint32) string {
	return fmt.Sprintf(common.BmIdxFmt, bmIdx)
}

// ---------------------------------------------------------------------------
// cluster_id (architecture.md §5.2)
// ---------------------------------------------------------------------------

// ClusterId derives a cluster's id from its name and its immutable
// ClusterConf.creation_epoch (architecture.md §5.2, MD2): fnv64a over the raw
// name bytes followed by the epoch as exactly 8 big-endian bytes — no
// separator, no text formatting.
//
// It is deliberately not computable from a request alone: every caller beyond
// CreateCluster/ListClusters first reads {p} cluster_conf {cluster_name} to
// learn the epoch (§5.8).
func ClusterId(clusterName string, creationEpoch uint64) uint64 {
	h := fnv.New64a()
	// hash.Hash.Write never returns an error.
	h.Write([]byte(clusterName))
	var epochBytes [8]byte
	binary.BigEndian.PutUint64(epochBytes[:], creationEpoch)
	h.Write(epochBytes[:])
	return h.Sum64()
}

// ---------------------------------------------------------------------------
// Keys (MD2, architecture.md §5.3)
// ---------------------------------------------------------------------------

// ClusterConfKey is the key of a ClusterConf — the only name-keyed message
// (MD2).
func ClusterConfKey(clusterName string) string {
	return joinKey(common.DnvPrefix, kindClusterConf, clusterName)
}

// ClusterConfPrefix is the prefix the ClusterConf cache ranges and watches
// (MD2, RW21).
func ClusterConfPrefix() string {
	return prefixOf(common.DnvPrefix, kindClusterConf)
}

// DnGlobalKey is the key of a cluster's DnGlobal (MD2).
func DnGlobalKey(cid uint64) string {
	return joinKey(common.DnvPrefix, kindDnGlobal, idField(cid))
}

// CnGlobalKey is the key of a cluster's CnGlobal (MD2).
func CnGlobalKey(cid uint64) string {
	return joinKey(common.DnvPrefix, kindCnGlobal, idField(cid))
}

// SpGlobalKey is the key of a cluster's SpGlobal (MD2).
func SpGlobalKey(cid uint64) string {
	return joinKey(common.DnvPrefix, kindSpGlobal, idField(cid))
}

// DnRevKey is the key of one DN's revision record (MD2).
func DnRevKey(shard uint32, cid uint64, dnId uint64) string {
	return joinKey(
		common.DnvPrefix, kindDnRev,
		shardField(shard), idField(cid), idField(dnId),
	)
}

// DnRevPrefix is the prefix a dn-role shard worker watches (MD2, SW3).
func DnRevPrefix(shard uint32) string {
	return prefixOf(common.DnvPrefix, kindDnRev, shardField(shard))
}

// CnRevKey is the key of one CN's revision record (MD2).
func CnRevKey(shard uint32, cid uint64, cnId uint64) string {
	return joinKey(
		common.DnvPrefix, kindCnRev,
		shardField(shard), idField(cid), idField(cnId),
	)
}

// CnRevPrefix is the prefix a cn-role shard worker watches (MD2, SW3).
func CnRevPrefix(shard uint32) string {
	return prefixOf(common.DnvPrefix, kindCnRev, shardField(shard))
}

// SpRevKey is the key of one SP's revision record (MD2).
func SpRevKey(shard uint32, cid uint64, spId uint64) string {
	return joinKey(
		common.DnvPrefix, kindSpRev,
		shardField(shard), idField(cid), idField(spId),
	)
}

// SpRevPrefix is the prefix an sp-role shard worker watches (MD2, SW3).
func SpRevPrefix(shard uint32) string {
	return prefixOf(common.DnvPrefix, kindSpRev, shardField(shard))
}

// DnConfKey is the key of one DN's authoritative record (MD2). A DN is
// addressed by the addr_port of its agent.
func DnConfKey(cid uint64, addrPort string) string {
	return joinKey(common.DnvPrefix, kindDnConf, idField(cid), addrPort)
}

// CnConfKey is the key of one CN's authoritative record (MD2).
func CnConfKey(cid uint64, addrPort string) string {
	return joinKey(common.DnvPrefix, kindCnConf, idField(cid), addrPort)
}

// DnCapacityKey is the allocation-index key of one DN (MD2, §5.6). It exists
// iff the DN is allocatable, and it embeds free_ext_cnt so that a descending
// range over one bin returns DNs largest-free first (§6.3).
func DnCapacityKey(
	cid uint64,
	binIdx uint32,
	freeExt uint64,
	addrPort string,
) string {
	return joinKey(
		common.DnvPrefix, kindDnCapacity,
		idField(cid), binField(binIdx), freeField(freeExt), addrPort,
	)
}

// DnCapacityPrefix is the prefix the §6.3 walk range-scans, one bin at a time
// (MD2).
func DnCapacityPrefix(cid uint64, binIdx uint32) string {
	return prefixOf(
		common.DnvPrefix, kindDnCapacity, idField(cid), binField(binIdx),
	)
}

// CnCapacityKey is the allocation-index key of one CN (MD2, §5.6). CNs have no
// bins (§6.4).
func CnCapacityKey(cid uint64, freeExt uint64, addrPort string) string {
	return joinKey(
		common.DnvPrefix, kindCnCapacity,
		idField(cid), freeField(freeExt), addrPort,
	)
}

// CnCapacityPrefix is the single prefix the §6.4 scan walks (MD2).
func CnCapacityPrefix(cid uint64) string {
	return prefixOf(common.DnvPrefix, kindCnCapacity, idField(cid))
}

// CdcEntryKey is the key of one subsystem's discovery entry (MD2). The shard
// code is the SP's, so that dnv-cdc can shard by key field (§12).
func CdcEntryKey(cid uint64, shard uint32, spId uint64, ssId uint64) string {
	return joinKey(
		common.DnvPrefix, kindCdcEntry,
		idField(cid), shardField(shard), idField(spId), idField(ssId),
	)
}

// CdcEntryPrefix is the whole discovery prefix dnv-cdc watches, filtering on
// the {shard_code} field of each key (MD2, architecture.md §12).
func CdcEntryPrefix() string {
	return prefixOf(common.DnvPrefix, kindCdcEntry)
}

// SpConfKey is the key of one SP's configuration (MD2).
func SpConfKey(cid uint64, spName string) string {
	return joinKey(common.DnvPrefix, kindSpConf, idField(cid), spName)
}

// SpNameKey is the key of the sp_id -> sp_name reverse lookup (MD2); it is
// still needed because SpRev can only be addressed with the SP's shard code
// ([D10]).
func SpNameKey(cid uint64, spId uint64) string {
	return joinKey(common.DnvPrefix, kindSpName, idField(cid), idField(spId))
}

// CntlrKey is the key of one cntlr of an SP (MD2).
func CntlrKey(cid uint64, spId uint64, cntlrId uint64) string {
	return joinKey(
		common.DnvPrefix, kindCntlr,
		idField(cid), idField(spId), idField(cntlrId),
	)
}

// SliceKey is the key of one slice of an SP (MD2); its groups, legs and sides
// are embedded in the value.
func SliceKey(cid uint64, spId uint64, sliceId uint64) string {
	return joinKey(
		common.DnvPrefix, kindSlice,
		idField(cid), idField(spId), idField(sliceId),
	)
}

// ThinDeviceKey is the key of one thin device of an SP (MD2).
func ThinDeviceKey(cid uint64, spId uint64, tdName string) string {
	return joinKey(
		common.DnvPrefix, kindThinDevice,
		idField(cid), idField(spId), tdName,
	)
}

// SubsystemKey is the key of one subsystem of an SP (MD2); its namespaces are
// embedded in the value.
func SubsystemKey(cid uint64, spId uint64, nqn string) string {
	return joinKey(
		common.DnvPrefix, kindSubsystem,
		idField(cid), idField(spId), nqn,
	)
}

// CloneKey is the key of one clone of an SP (MD2).
func CloneKey(cid uint64, spId uint64, cloneName string) string {
	return joinKey(
		common.DnvPrefix, kindClone,
		idField(cid), idField(spId), cloneName,
	)
}

// CloneBitmapKey is the key of one chunk of a clone's bitmap (MD2); bm_idx is
// the source slice_idx.
func CloneBitmapKey(
	cid uint64,
	spId uint64,
	cloneName string,
	bmIdx uint32,
) string {
	return joinKey(
		common.DnvPrefix, kindCloneBitmap,
		idField(cid), idField(spId), cloneName, bmIdxField(bmIdx),
	)
}

// CloneBitmapPrefix is the prefix MD3 scans keys-only to learn which chunks of
// a clone's bitmap exist.
func CloneBitmapPrefix(cid uint64, spId uint64, cloneName string) string {
	return prefixOf(
		common.DnvPrefix, kindCloneBitmap,
		idField(cid), idField(spId), cloneName,
	)
}

// TransferKey is the key of one transfer of an SP (MD2).
func TransferKey(cid uint64, spId uint64, xferName string) string {
	return joinKey(
		common.DnvPrefix, kindTransfer,
		idField(cid), idField(spId), xferName,
	)
}

// MigrationKey is the key of one migration of an SP (MD2).
func MigrationKey(cid uint64, spId uint64, migrName string) string {
	return joinKey(
		common.DnvPrefix, kindMigration,
		idField(cid), idField(spId), migrName,
	)
}

// MigrBitmapKey is the key of one chunk of a migration's bitmap (MD2); bm_idx
// is the append sequence 0….
func MigrBitmapKey(
	cid uint64,
	spId uint64,
	migrName string,
	bmIdx uint32,
) string {
	return joinKey(
		common.DnvPrefix, kindMigrBitmap,
		idField(cid), idField(spId), migrName, bmIdxField(bmIdx),
	)
}

// MigrBitmapPrefix is the prefix MD3 scans keys-only to learn which chunks of
// a migration's bitmap exist.
func MigrBitmapPrefix(cid uint64, spId uint64, migrName string) string {
	return prefixOf(
		common.DnvPrefix, kindMigrBitmap,
		idField(cid), idField(spId), migrName,
	)
}

// WorkerRegKey is the key of one worker incarnation's registration (MD2, VW2).
// It is cluster-independent: the vote layer is a property of the fleet, not of
// a cluster.
func WorkerRegKey(role string, seed string) string {
	return joinKey(common.DnvPrefix, kindWorkerReg, role, seed)
}

// WorkerRegPrefix is the prefix one role's vote layer ranges and watches (MD2,
// VW3).
func WorkerRegPrefix(role string) string {
	return prefixOf(common.DnvPrefix, kindWorkerReg, role)
}

// ---------------------------------------------------------------------------
// Parsers (MD2)
// ---------------------------------------------------------------------------
//
// Every parser reports ok = false for a malformed key — wrong field count, an
// unexpected literal, a field that is not exactly the hex the §5.1 format
// prescribes — and never panics: a worker decodes keys straight off a watch or
// a range, and SW2 logs and skips whatever it cannot parse.

// parseFields splits a key and checks its field count and its first two
// literal fields.
func parseFields(key string, kind string, want int) ([]string, bool) {
	fields := strings.Split(key, keySep)
	if len(fields) != want {
		return nil, false
	}
	if fields[0] != common.DnvPrefix || fields[1] != kind {
		return nil, false
	}
	return fields, true
}

// parseHex parses one hex field and rejects anything that does not render back
// to itself under format — which is what makes "4", "0X4", "004" and
// upper-case hex malformed rather than tolerated.
func parseHex(s string, format string) (uint64, bool) {
	value, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, false
	}
	if fmt.Sprintf(format, value) != s {
		return 0, false
	}
	return value, true
}

// parseId parses one common.IdKeyFmt field.
func parseId(s string) (uint64, bool) {
	return parseHex(s, common.IdKeyFmt)
}

// parseFree parses one common.FreeSpaceFmt field.
func parseFree(s string) (uint64, bool) {
	return parseHex(s, common.FreeSpaceFmt)
}

// parseShard parses one common.ShardCodeFmt field; a shard code is always
// below common.ShardBucketSize and therefore exactly two hex digits.
func parseShard(s string) (uint32, bool) {
	value, ok := parseHex(s, common.ShardCodeFmt)
	if !ok || value >= common.ShardBucketSize {
		return 0, false
	}
	return uint32(value), true
}

// parseBin parses one common.BinIdxFmt field; §6.2 has exactly four bins, so
// the field is always a single hex digit.
func parseBin(s string) (uint32, bool) {
	if len(s) != 1 {
		return 0, false
	}
	value, ok := parseHex(s, common.BinIdxFmt)
	if !ok {
		return 0, false
	}
	return uint32(value), true
}

// parseBmIdxField parses one common.BmIdxFmt field; chunk counts are bounded
// by common.MaxCloneBmCnt / common.MaxMigrBmCnt, so the field is always
// exactly two hex digits.
func parseBmIdxField(s string) (uint32, bool) {
	if len(s) != 2 {
		return 0, false
	}
	value, ok := parseHex(s, common.BmIdxFmt)
	if !ok {
		return 0, false
	}
	return uint32(value), true
}

// parseRevKey is the shared body of ParseDnRevKey / ParseCnRevKey /
// ParseSpRevKey: the three rev keys differ only in their kind field.
func parseRevKey(
	key string,
	kind string,
) (uint32, uint64, uint64, bool) {
	fields, ok := parseFields(key, kind, 5)
	if !ok {
		return 0, 0, 0, false
	}
	shard, ok := parseShard(fields[2])
	if !ok {
		return 0, 0, 0, false
	}
	cid, ok := parseId(fields[3])
	if !ok {
		return 0, 0, 0, false
	}
	id, ok := parseId(fields[4])
	if !ok {
		return 0, 0, 0, false
	}
	return shard, cid, id, true
}

// ParseDnRevKey decodes a DnRev key as seen on a watch (MD2). A DnRev event is
// self-sufficient: the key gives the cluster and the dn_id, the value the
// revision and the addr_port (§5.5).
func ParseDnRevKey(key string) (uint32, uint64, uint64, bool) {
	return parseRevKey(key, kindDnRev)
}

// ParseCnRevKey decodes a CnRev key as seen on a watch (MD2).
func ParseCnRevKey(key string) (uint32, uint64, uint64, bool) {
	return parseRevKey(key, kindCnRev)
}

// ParseSpRevKey decodes an SpRev key as seen on a watch (MD2).
func ParseSpRevKey(key string) (uint32, uint64, uint64, bool) {
	return parseRevKey(key, kindSpRev)
}

// ParseWorkerRegKey decodes a WorkerReg key as seen on the vote layer's watch
// (MD2, VW3). A role outside the three common.WorkerRole* values is malformed:
// nothing in dnv writes one, and the vote layer must not invent a role it
// cannot drive.
func ParseWorkerRegKey(key string) (string, string, bool) {
	fields, ok := parseFields(key, kindWorkerReg, 4)
	if !ok {
		return "", "", false
	}
	role := fields[2]
	switch role {
	case common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp:
	default:
		return "", "", false
	}
	seed := fields[3]
	if seed == "" {
		return "", "", false
	}
	return role, seed, true
}

// ParseBmIdx decodes the chunk index out of a CloneBitmap or MigrBitmap key
// (MD2). MD3 scans those two prefixes keys-only, so this is the only way the
// loader learns which chunks exist without reading a single bitmap.
func ParseBmIdx(key string) (uint32, bool) {
	fields := strings.Split(key, keySep)
	if len(fields) != 6 {
		return 0, false
	}
	if fields[0] != common.DnvPrefix {
		return 0, false
	}
	switch fields[1] {
	case kindCloneBitmap, kindMigrBitmap:
	default:
		return 0, false
	}
	if _, ok := parseId(fields[2]); !ok {
		return 0, false
	}
	if _, ok := parseId(fields[3]); !ok {
		return 0, false
	}
	if fields[4] == "" {
		return 0, false
	}
	return parseBmIdxField(fields[5])
}

// ParseDnCapacityKey turns one scanned dn_capacity key back into the candidate
// it describes (MD2, MD5): the §6.3 walk reads free_ext_cnt and addr_port out
// of the key itself and only decodes the value for the location, so a scan
// needs no extra point reads (§5.6). The cluster id is validated but not
// returned — the caller built the prefix from it.
func ParseDnCapacityKey(key string) (uint32, uint64, string, bool) {
	fields, ok := parseFields(key, kindDnCapacity, 6)
	if !ok {
		return 0, 0, "", false
	}
	if _, ok := parseId(fields[2]); !ok {
		return 0, 0, "", false
	}
	binIdx, ok := parseBin(fields[3])
	if !ok {
		return 0, 0, "", false
	}
	freeExt, ok := parseFree(fields[4])
	if !ok {
		return 0, 0, "", false
	}
	addrPort := fields[5]
	if addrPort == "" {
		return 0, 0, "", false
	}
	return binIdx, freeExt, addrPort, true
}

// ParseCnCapacityKey turns one scanned cn_capacity key back into the candidate
// it describes (MD2, MD5). CNs have no bins (§6.4).
func ParseCnCapacityKey(key string) (uint64, string, bool) {
	fields, ok := parseFields(key, kindCnCapacity, 5)
	if !ok {
		return 0, "", false
	}
	if _, ok := parseId(fields[2]); !ok {
		return 0, "", false
	}
	freeExt, ok := parseFree(fields[3])
	if !ok {
		return 0, "", false
	}
	addrPort := fields[4]
	if addrPort == "" {
		return 0, "", false
	}
	return freeExt, addrPort, true
}

// ParseCdcEntryKey decodes a CdcEntry key as seen on dnv-cdc's prefix watch
// (MD2, cdc.md §2.2). Its field order is the key's own and is deliberately
// NOT the rev keys' order: cluster_id comes BEFORE shard_code, so that the
// single CdcEntryPrefix watch spans every cluster (DS1) and dnv-cdc decides
// ownership from the shard code it finds inside each key (DS2, WV2). A
// malformed key is reported here and skipped by the caller, never guessed at.
func ParseCdcEntryKey(
	key string,
) (cid uint64, shard uint32, spId uint64, ssId uint64, ok bool) {
	fields, ok := parseFields(key, kindCdcEntry, 6)
	if !ok {
		return 0, 0, 0, 0, false
	}
	cid, ok = parseId(fields[2])
	if !ok {
		return 0, 0, 0, 0, false
	}
	shard, ok = parseShard(fields[3])
	if !ok {
		return 0, 0, 0, 0, false
	}
	spId, ok = parseId(fields[4])
	if !ok {
		return 0, 0, 0, 0, false
	}
	ssId, ok = parseId(fields[5])
	if !ok {
		return 0, 0, 0, 0, false
	}
	return cid, shard, spId, ssId, true
}
