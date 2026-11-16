package common

import (
	"strconv"
	"strings"
)

// DmTarget is one parsed line of a dm table or status: the target's start
// and length in sectors, its type, and the fields after the type.
type DmTarget struct {
	Start  uint64
	Length uint64
	Type   string
	Args   []string
}

// ParseDmLines parses `dmsetup table` and `dmsetup status` output, one
// DmTarget per dm line. A line with fewer than three fields, or whose first
// two fields are not unsigned numbers, is no dm line and is skipped, which is
// why the text a row puts ahead of a status line (agent.CloneStatusDetails)
// never counts as a target.
func ParseDmLines(out string) []DmTarget {
	var targets []DmTarget
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		start, err1 := strconv.ParseUint(fields[0], 10, 64)
		length, err2 := strconv.ParseUint(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		targets = append(targets, DmTarget{
			Start:  start,
			Length: length,
			Type:   fields[2],
			Args:   fields[3:],
		})
	}
	return targets
}

// CloneStatus is the parsed `dmsetup status` line of a dm-clone device: the
// region size in sectors, the hydration progress in regions, the count of
// regions being copied at the moment of the read, whether hydration is on
// (no no_hydration feature argument), the two hydration knobs among its core
// arguments, and the metadata mode the line ends with, one of the
// CloneMetadata modes, or "" when the line does not end with one.
type CloneStatus struct {
	RegionSectors   uint64
	HydratedRegions uint64
	TotalRegions    uint64
	// Hydrating is the count of regions whose copy is in flight, the
	// background copy's and the copies writes started; HydratingRead says
	// the count parsed. A count that does not parse leaves both zero, so a
	// caller that waits for no copy in flight (agent.ApplySkipRanges) reads
	// HydratingRead and never a zero alone.
	Hydrating        uint64
	HydratingRead    bool
	HydrationEnabled bool
	Threshold        uint32
	BatchSize        uint32
	MetadataMode     string
}

// The metadata modes the kernel prints at the end of a dm-clone's status
// line (CloneStatus.MetadataMode). dm-clone leaves read-write mode for
// read-only when a metadata operation fails, and for fail mode when it
// cannot read its metadata at all; in fail mode it prints "Fail" alone in
// place of the line (CloneFailed).
const (
	CloneMetadataReadWrite = "rw"
	CloneMetadataReadOnly  = "ro"
	CloneMetadataFail      = "Fail"
)

// CloneFailed reports whether raw holds the status of a dm-clone in fail
// mode: one dm line, a dm-clone's, whose one argument is CloneMetadataFail.
// ParseCloneStatus does not parse that line, since it carries no count.
func CloneFailed(raw string) bool {
	targets := ParseDmLines(raw)
	return len(targets) == 1 && targets[0].Type == "clone" &&
		len(targets[0].Args) == 1 &&
		targets[0].Args[0] == CloneMetadataFail
}

// ParseCloneStatus reads the dm-clone status line:
//
//	<meta block size> <used>/<total> <region size> <hydrated>/<total>
//	<hydrating> <#feature args> <feature args>* <#core args> <core args>*
//	<metadata mode>
//
// It is the one Go reader of that format: both agents, both agent drivers and
// the gateway's proof that a copy finished read the line here
// (architecture.md, Live-state reporting). raw must hold exactly one dm line
// (ParseDmLines), and it must be a dm-clone line with at least six arguments;
// anything else does not parse. The text a row puts ahead of the line is no
// dm line, so the whole details of a row parse. A count that does not parse,
// one past 64 bits included, reads as zero, so a caller that needs proof
// refuses a total of zero, and a feature or core list whose count does not
// fit the line is not read. The metadata mode is the argument right after the
// core list when it is the line's last; a line whose lists are not read, or
// that ends elsewhere, reads no mode.
//
// The shell suites cannot import it. hyd_fraction (integtest/e2e_test.sh)
// keeps its rule of one dm line, a dm-clone's with at least six arguments, by
// hand; the dn suite's readers in migr_read_through, migr_first_hydr and
// teardown_dead_source (integtest/dnagent_test.sh) read the hydration counts
// four fields past the first "clone" field of the details instead.
//
// The gateway reads the line out of an agent's reply, so no count on it may
// panic the reader: each list's count is compared with what is left of the
// line, never added to an index that a count near the top of int would wrap.
func ParseCloneStatus(raw string) (*CloneStatus, bool) {
	targets := ParseDmLines(raw)
	if len(targets) != 1 || targets[0].Type != "clone" {
		return nil, false
	}
	args := targets[0].Args
	if len(args) < 6 {
		return nil, false
	}
	st := &CloneStatus{HydrationEnabled: true}
	// A count is kept only when it parses: for one past 64 bits ParseUint
	// hands back the largest value beside its error, and a hydrated count
	// read as that would prove any total.
	if v, err := strconv.ParseUint(args[2], 10, 64); err == nil {
		st.RegionSectors = v
	}
	if hydrated, total, ok := strings.Cut(args[3], "/"); ok {
		if v, err := strconv.ParseUint(hydrated, 10, 64); err == nil {
			st.HydratedRegions = v
		}
		if v, err := strconv.ParseUint(total, 10, 64); err == nil {
			st.TotalRegions = v
		}
	}
	if v, err := strconv.ParseUint(args[4], 10, 64); err == nil {
		st.Hydrating = v
		st.HydratingRead = true
	}
	featureCnt, err := strconv.Atoi(args[5])
	if err != nil || featureCnt < 0 || featureCnt > len(args)-6 {
		return st, true
	}
	for _, feature := range args[6 : 6+featureCnt] {
		if feature == "no_hydration" {
			st.HydrationEnabled = false
		}
	}
	rest := args[6+featureCnt:]
	if len(rest) == 0 {
		return st, true
	}
	coreCnt, err := strconv.Atoi(rest[0])
	if err != nil || coreCnt < 0 || coreCnt > len(rest)-1 {
		return st, true
	}
	core := rest[1 : 1+coreCnt]
	if len(rest) == 2+coreCnt {
		st.MetadataMode = rest[1+coreCnt]
	}
	for i := 0; i+1 < len(core); i += 2 {
		value, convErr := strconv.ParseUint(core[i+1], 10, 32)
		if convErr != nil {
			continue
		}
		switch core[i] {
		case "hydration_threshold":
			st.Threshold = uint32(value)
		case "hydration_batch_size":
			st.BatchSize = uint32(value)
		}
	}
	return st, true
}

// HydrationComplete decides whether a dm-clone has finished copying, from the
// details of its row, which carry the raw `dmsetup status` line
// (architecture.md, Live-state reporting), read with ParseCloneStatus.
// Details that do not parse and a total of zero regions are "not complete":
// the gateway's DeleteClone and FinishMigration refuse, and the worker latches
// no clone (dnv-worker.md CLD13), unless the line PROVES the copy done, and an
// unreadable status is not proof (architecture.md, Clones). The zero rule is
// this function's own; ParseCloneStatus reads a count that does not parse, one
// past 64 bits included, as zero.
//
// The row is the one in the agent's latest observation, not a fresh read of
// the node (architecture.md, Live-state reporting). That is proof enough: a
// copy only advances, so an observation that lags the node shows less of it,
// and the refusal it gives is one the caller repeats; an agent that holds no
// observation of the object yet reports no row, and empty details refuse.
func HydrationComplete(details string) bool {
	status, ok := ParseCloneStatus(details)
	return ok && status.TotalRegions != 0 &&
		status.HydratedRegions >= status.TotalRegions
}

// ThinPoolStatus is what one thin pool's `dmsetup status` line says of its
// size and state: the metadata pair in dm-thin's fixed metadata blocks
// (ThinMetaBlockSize), the data pair in the pool's data_block_size, and
// whether the line carries needs_check.
type ThinPoolStatus struct {
	UsedMetaBlocks  uint64
	TotalMetaBlocks uint64
	UsedDataBlocks  uint64
	TotalDataBlocks uint64
	// NeedsCheck is the line's needs_check flag, which dm-thin keeps in the
	// pool's metadata: the pool is read-only on every node that opens it
	// until the flag is cleared, and dm-thin grows no metadata device at a
	// resume (maybe_resize_metadata_dev), whatever its size.
	NeedsCheck bool
}

// ThinPoolFault is the word a thin pool's `dmsetup status` line carries in
// place of its numbers (pool_status): "Fail" when dm-thin has failed the
// loaded pool, and "Error" when the status read could not read the pool's
// metadata. It is "" for any other line, a parsed one among them.
func ThinPoolFault(raw string) string {
	fields := strings.Fields(raw)
	for idx, field := range fields {
		if field != "thin-pool" {
			continue
		}
		if len(fields) == idx+2 &&
			(fields[idx+1] == "Fail" || fields[idx+1] == "Error") {
			return fields[idx+1]
		}
		return ""
	}
	return ""
}

// ParseThinPoolStatus parses one raw `dmsetup status` line of a thin pool.
// The fields after the `thin-pool` token are
//
//	<transaction_id> <used_meta>/<total_meta> <used_data>/<total_data> …
//
// so the token is located rather than a fixed column counted: the line may
// or may not be prefixed with the device name and the start/length pair.
// Of what follows the two ratios — the held metadata root, the mode word
// (rw, ro or out_of_data_space), the discard and no-space words,
// needs_check, the low watermark — only needs_check is read: dm-thin prints
// ro both for a pool out of metadata space and for a read-only one, and only
// the second carries needs_check, since dnv's table never asks for
// read-only. A line that carries `Fail` or `Error` in place of its numbers
// (ThinPoolFault) does not parse.
func ParseThinPoolStatus(raw string) (*ThinPoolStatus, bool) {
	fields := strings.Fields(raw)
	token := -1
	for idx, field := range fields {
		if field == "thin-pool" {
			token = idx
			break
		}
	}
	if token < 0 || len(fields) < token+4 {
		return nil, false
	}
	usedMeta, totalMeta, ok := parseStatusRatio(fields[token+2])
	if !ok {
		return nil, false
	}
	usedData, totalData, ok := parseStatusRatio(fields[token+3])
	if !ok {
		return nil, false
	}
	st := &ThinPoolStatus{
		UsedMetaBlocks:  usedMeta,
		TotalMetaBlocks: totalMeta,
		UsedDataBlocks:  usedData,
		TotalDataBlocks: totalData,
	}
	for _, field := range fields[token+4:] {
		if field == "needs_check" {
			st.NeedsCheck = true
		}
	}
	return st, true
}

// parseStatusRatio parses one `<used>/<total>` field of a status line.
func parseStatusRatio(field string) (uint64, uint64, bool) {
	used, total, found := strings.Cut(field, "/")
	if !found {
		return 0, 0, false
	}
	usedVal, err := strconv.ParseUint(used, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	totalVal, err := strconv.ParseUint(total, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return usedVal, totalVal, true
}

// ThinMetaBlocks is the total metadata blocks a thin pool reports once it
// has taken a metadata device of sectors 512-byte sectors, as dm-thin
// counts them at a resume (get_metadata_dev_size_in_blocks) and at its
// format: the device's size capped at ThinMetaMaxBlocks blocks of
// ThinMetaBlockSize.
func ThinMetaBlocks(sectors uint64) uint64 {
	return min(sectors/(ThinMetaBlockSize/512), ThinMetaMaxBlocks)
}
