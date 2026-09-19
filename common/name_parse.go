package common

import (
	"strconv"
	"strings"
)

// This file is the reverse of name_fmt.go. A sweep decides what to remove by
// enumerating what exists on the node and subtracting what the desired state
// wants, so it has to be able to look at a name the kernel hands back and say
// whether it is ours, of which kind, and of which sp. Parsing is strict on
// purpose: a name that does not decode exactly is "not a dnv name", never a
// half-decoded one, because the caller's next move is a removal.

// DmName is a dnv dm device name decoded into its fields. Which object each
// Ids position names depends on the kind (Appendix B of the teardown-by-sweep
// design); for every kind of both roles Ids[0] is the sp id.
type DmName struct {
	ClusterId uint64
	NodeId    uint64
	Kind      DmKind
	Ids       []uint64
}

// Role is the kind's role letter, DmRoleCn or DmRoleDn.
func (n DmName) Role() byte {
	return n.Kind[0]
}

// SpId is Ids[0], the sp id every dnv dm name carries first.
func (n DmName) SpId() uint64 {
	return n.Ids[0]
}

// ParseDmName decodes a name built by one of the Cn*Name/Dn*Name formatters:
// dnv-<cluster:016x>-<node:016x>-<kind>-<id:016x>… . It returns false for
// anything else — a foreign dm device, an md name, a kind this build does not
// know, a name whose id count does not match its kind — so an enumerating
// caller can separate "not ours" from "ours and unwanted" without guessing.
func ParseDmName(name string) (DmName, bool) {
	fields := strings.Split(name, "-")
	if len(fields) < 5 || fields[0] != DmPrefix {
		return DmName{}, false
	}
	clusterId, ok := parseHex64(fields[1])
	if !ok {
		return DmName{}, false
	}
	nodeId, ok := parseHex64(fields[2])
	if !ok {
		return DmName{}, false
	}
	kind := DmKind(fields[3])
	idCnt, known := dmKindIdCnt[kind]
	if !known || len(fields) != 4+idCnt {
		return DmName{}, false
	}
	ids := make([]uint64, 0, idCnt)
	for _, f := range fields[4:] {
		id, ok := parseHex64(f)
		if !ok {
			return DmName{}, false
		}
		ids = append(ids, id)
	}
	return DmName{
		ClusterId: clusterId,
		NodeId:    nodeId,
		Kind:      kind,
		Ids:       ids,
	}, true
}

// NqnParts is a dnv NVMe qualified name decoded into its fields. The Ids
// order is the kind's own (SideToCnNqn: cluster, sp, leg, cn; MigrSrcNqn:
// cluster, dn, sp, migr; XferNqn: cluster, sp, xfer) — unlike a dm name an
// NQN has no single "sp is first" rule, which is why the sweep's attribution
// tables name the position per kind.
type NqnParts struct {
	Kind NqnKind
	Ids  []uint64
}

// IsDnvNqn reports whether an NQN is in the dnv namespace at all, whether or
// not it decodes. The distinction matters to a sweep: a subsystem whose NQN
// carries the dnv prefix but decodes to nothing — an unknown kind, a wrong
// arity, a non-hex id — is "not ours, never touched", NOT the user-chosen
// host-facing NQN that ParseNqn's false otherwise means. Treating it as
// host-facing would let a malformed name get a subsystem deleted, because a
// host-facing subsystem with nothing attributable under it is swept as
// unowned.
func IsDnvNqn(nqn string) bool {
	return strings.HasPrefix(nqn, NqnPrefix+":")
}

// ParseNqn decodes a dnv-format NQN. A host-facing subsystem NQN is chosen by
// the user and carries nothing, so it does not start with the dnv prefix and
// parses false; that "false" is the signal the sweep attributes such a
// subsystem by its namespaces' backing device instead.
func ParseNqn(nqn string) (NqnParts, bool) {
	rest, found := strings.CutPrefix(nqn, NqnPrefix+":")
	if !found {
		return NqnParts{}, false
	}
	fields := strings.Split(rest, ":")
	if len(fields) < 2 || len(fields[0]) != 1 {
		return NqnParts{}, false
	}
	kindVal, err := strconv.ParseUint(fields[0], 16, 8)
	if err != nil {
		return NqnParts{}, false
	}
	kind := NqnKind(kindVal)
	idCnt, known := nqnKindIdCnt[kind]
	if !known || len(fields) != 1+idCnt {
		return NqnParts{}, false
	}
	ids := make([]uint64, 0, idCnt)
	for _, f := range fields[1:] {
		id, ok := parseHex64(f)
		if !ok {
			return NqnParts{}, false
		}
		ids = append(ids, id)
	}
	return NqnParts{Kind: kind, Ids: ids}, true
}

// parseHex64 accepts exactly the %016x the formatters write: 16 lower-case
// hex digits. Anything shorter, longer, signed or 0x-prefixed is rejected, so
// a name that merely resembles a dnv name cannot decode.
func parseHex64(field string) (uint64, bool) {
	if len(field) != 16 {
		return 0, false
	}
	for i := 0; i < len(field); i++ {
		c := field[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(field, 16, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
