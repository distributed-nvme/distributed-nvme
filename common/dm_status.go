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
// region size in sectors, the hydration progress in regions, whether
// hydration is on (no no_hydration feature argument), the two hydration
// knobs among its core arguments, and the metadata mode the line ends with,
// one of the CloneMetadata modes, or "" when the line does not end with one.
type CloneStatus struct {
	RegionSectors    uint64
	HydratedRegions  uint64
	TotalRegions     uint64
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
