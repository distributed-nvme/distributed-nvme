package agentfake

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The fake against the real agents (dnv-worker.md, Integration test plan,
// The fake against the real agents)
// ---------------------------------------------------------------------------

// RealDiff is one way the fake may answer otherwise than a real agent in
// the comparison tests of agent/cnagent and agent/dnagent, which drive a
// real agent and the fake through the same syncups and rounds: a row only
// one of them reports, or a row both report with another status or reason,
// each named with the exact status and reason of each row that is there; or
// a code the real agent answers a syncup with where the fake answers zero.
// Each holds only in the request state its When names, and carries the rule
// by which a worker test run on the fake stays true despite it. Only the
// ResInfo rows, the codes and the revisions are compared: a field that is no
// row, such as a group reading's slots, is not.
type RealDiff struct {
	// Info is the message the difference is in: "CntlrInfo", "CnInfo",
	// "SideInfo" or "DnInfo" for a row, or a reply's message name for a
	// code.
	Info string
	// Rows are the rows' fields, the first part of each key (RowsOf); none
	// for a code.
	Rows []string
	// When are the states of the object's last accepted request in which
	// the entry holds, every one of them (Conds); none is every state.
	When []Cond
	// Real and Fake are the status and reason of the real agent's row and
	// of the fake's; nil is no row.
	Real, Fake *RowState
	// Code is the code the real agent answers where the fake answers zero.
	Code uint32
	// Rule is why the difference changes no verdict a worker test reads.
	Rule string
}

// RowState is a row's status and reason.
type RowState struct {
	Status pb.ResStatus
	Reason pb.ResReason
}

// Cond is one state of an object's request that an entry of RealDiffs may
// hold in.
type Cond string

const (
	// CondPrimary and CondStandby: the cntlr's request makes it the primary
	// (cntlr.primary set, cntlr.disabled not, cnagent.md CN9), or not.
	CondPrimary Cond = "primary"
	CondStandby Cond = "standby"
	// CondLevelHoldsClone: the cntlr's request carries a clone at a level
	// at or above SP_LEVEL_NO_CLONE (cnagent.md CN19).
	CondLevelHoldsClone Cond = "level holds a clone"
	// CondUnflipped: the side's request says it is not provisioned yet
	// (architecture.md, Side provisioning protocol).
	CondUnflipped Cond = "unflipped"
	// CondCutoverPending: the side's request makes it a migration source
	// whose cutover is pending.
	CondCutoverPending Cond = "cutover pending"
)

// Conds are the states an object's last accepted request is in.
type Conds map[Cond]bool

// CntlrConds are the states of a cntlr's request.
func CntlrConds(req *pb.SyncupCntlrRequest) Conds {
	conds := Conds{CondStandby: true}
	if req.GetCntlr().GetPrimary() && !req.GetCntlr().GetDisabled() {
		conds = Conds{CondPrimary: true}
	}
	if len(req.GetCloneList()) > 0 &&
		req.GetSpLevel() >= pb.SpLevel_SP_LEVEL_NO_CLONE {
		conds[CondLevelHoldsClone] = true
	}
	return conds
}

// SideConds are the states of a side's request.
func SideConds(req *pb.SyncupSideRequest) Conds {
	conds := Conds{}
	if !req.GetSideConf().GetProvisioned() {
		conds[CondUnflipped] = true
	}
	if req.GetMigrSrcConf().GetCutoverPending() {
		conds[CondCutoverPending] = true
	}
	return conds
}

var (
	stateOk           = &RowState{Status: pb.ResStatus_RES_STATUS_OK}
	stateProvisioning = &RowState{Status: pb.ResStatus_RES_STATUS_PROVISIONING}
)

// provisioningRule is the rule of a row the real agent reports
// PROVISIONING where the fake reports it OK.
const provisioningRule = "HL1 and HL2 of dnv-worker.md judge a " +
	"PROVISIONING row as an OK one: neither sets a health epoch " +
	"(architecture.md, Live-state reporting)"

// RealDiffs are the allowed differences. A comparison test fails on a
// difference no entry names, and on an entry of its role that it never
// meets, so the table holds what the fake does.
var RealDiffs = []RealDiff{
	{
		Info: "CntlrInfo", Rows: []string{"td_id_to_thin_info"},
		When: []Cond{CondPrimary}, Real: stateOk,
		Rule: "the fake reports a primary's thin rows only under its " +
			"thin_ok lever, and the worker flips a thin device created on " +
			"them alone (dnv-worker.md RW19), so every whole-worker case " +
			"whose thin devices must be created sets the lever; a case " +
			"without it plays a primary that reports no thin row, a shape " +
			"no real agent gives, which RW19 flips nothing on and HL2 and " +
			"its settle read as they read an OK row: it is no model of a " +
			"primary whose thin devices are not built, which the cn agent " +
			"reports PROVISIONING or MISSING with no reason",
	},
	{
		Info: "CntlrInfo",
		Rows: []string{"clone_id_to_target", "clone_id_to_dm_clone"},
		When: []Cond{CondPrimary, CondLevelHoldsClone},
		Real: &RowState{Status: pb.ResStatus_RES_STATUS_MISSING,
			Reason: pb.ResReason_RES_REASON_SP_LEVEL},
		Fake: stateOk,
		Rule: "at a level that holds a clone the cn agent reports its " +
			"two rows MISSING with the level's reason (cnagent.md CN19), " +
			"and the fake, which reads no level, reports them OK: HL2 of " +
			"dnv-worker.md judges both alike, neither an ERROR row nor a " +
			"hold of the settle (architecture.md, Live-state reporting)",
	},
	{
		Info: "CnInfo", Rows: []string{"store_info"}, Real: stateOk,
		Rule: "HL1 of dnv-worker.md reads a node's store row for an " +
			"ERROR alone and takes a missing one as no row, as of an " +
			"agent that predates it; the fake's store never fails, so " +
			"the node's verdict is the one an OK row gives",
	},
	{
		Info: "DnInfo", Rows: []string{"store_info"}, Real: stateOk,
		Rule: "as CnInfo's store row",
	},
	{
		Info: "SideInfo",
		Rows: []string{"cn_id_to_dm_error", "cn_id_to_dm_linear",
			"cn_id_to_nvmeof", "migr_dst_info"},
		When: []Cond{CondUnflipped}, Real: stateProvisioning, Fake: stateOk,
		Rule: "before the provisioned flip the dn agent reports a side's " +
			"rows PROVISIONING and the fake, which zeroes at once, OK; " +
			provisioningRule + ", and the flip reads the side's zeroing " +
			"counts, not its rows (dnv-worker.md RW18)",
	},
	{
		Info: "SideInfo", Rows: []string{"migr_src_info"},
		When: []Cond{CondCutoverPending}, Real: stateProvisioning,
		Fake: stateOk,
		Rule: "while the cutover is pending the dn agent reports a " +
			"migration source's rows PROVISIONING and the fake OK; " +
			provisioningRule + ", and the cutover reads the " +
			"destination's rows, not the source's (dnv-worker.md RW18)",
	},
	{
		Info: "SyncupCntlrReply", Code: 4,
		Rule: "the cn agent answers a converge that leaves a teardown in " +
			"flight with ReplyCodeLeftover, which the worker takes as " +
			"accepted and sends again (dnv-worker.md RW5); the fake names " +
			"a leftover only under its leftover_rounds and read_leftover " +
			"levers, by which the whole-worker cases of the re-send stage " +
			"one",
	},
}

// RowDiff is one row in which two infos differ: the text that says how, and
// the index in RealDiffs of the entry that allows it, -1 when none does.
type RowDiff struct {
	Text    string
	Allowed int
}

// stateOf is a row's status and reason, nil for no row.
func stateOf(row *pb.ResInfo) *RowState {
	if row == nil {
		return nil
	}
	return &RowState{Status: row.GetStatus(), Reason: row.GetReason()}
}

// sameState reports whether two row states, nil for no row, are equal.
func sameState(a, b *RowState) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// CompareRows compares the rows of the real agent's info and the fake's,
// both of the message infoName names, of an object whose last accepted
// request is in the states conds names, and returns every row they differ
// in, in key order. An entry of RealDiffs allows a row's difference only
// when it names the row's field, holds in every state its When names, and
// gives the exact status and reason of each row that is there.
func CompareRows(infoName string, conds Conds, real, fake any) []RowDiff {
	realRows, fakeRows := RowsOf(real), RowsOf(fake)
	keys := make([]string, 0, len(realRows)+len(fakeRows))
	for key := range realRows {
		keys = append(keys, key)
	}
	for key := range fakeRows {
		if _, ok := realRows[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var out []RowDiff
	for _, key := range keys {
		r, f := realRows[key], fakeRows[key]
		rs, fs := stateOf(r), stateOf(f)
		if sameState(rs, fs) {
			continue
		}
		var text string
		switch {
		case f == nil:
			text = fmt.Sprintf("%s %s: the real agent reports %s %s (%q), "+
				"the fake no row", infoName, key, r.GetStatus(),
				r.GetReason(), r.GetDetails())
		case r == nil:
			text = fmt.Sprintf("%s %s: the fake reports %s %s (%q), the "+
				"real agent no row", infoName, key, f.GetStatus(),
				f.GetReason(), f.GetDetails())
		default:
			text = fmt.Sprintf("%s %s: the real agent reports %s %s (%q), the "+
				"fake %s %s (%q)", infoName, key, r.GetStatus(), r.GetReason(),
				r.GetDetails(), f.GetStatus(), f.GetReason(), f.GetDetails())
		}
		field, _, _ := strings.Cut(key, ".")
		allowed := slices.IndexFunc(RealDiffs, func(d RealDiff) bool {
			if d.Info != infoName || !slices.Contains(d.Rows, field) ||
				!sameState(d.Real, rs) || !sameState(d.Fake, fs) {
				return false
			}
			for _, cond := range d.When {
				if !conds[cond] {
					return false
				}
			}
			return true
		})
		out = append(out, RowDiff{Text: text, Allowed: allowed})
	}
	return out
}

// CompareCode compares the code the real agent answered a syncup with,
// whose reply is of the message replyName, with the fake's; ok is false
// when the two are equal.
func CompareCode(replyName string, real, fake uint32) (RowDiff, bool) {
	if real == fake {
		return RowDiff{}, false
	}
	allowed := -1
	if fake == 0 {
		allowed = slices.IndexFunc(RealDiffs, func(d RealDiff) bool {
			return d.Info == replyName && len(d.Rows) == 0 && d.Code == real
		})
	}
	return RowDiff{
		Text: fmt.Sprintf("%s: the real agent answers code %d, the fake %d",
			replyName, real, fake),
		Allowed: allowed,
	}, true
}

// RowsOf is every ResInfo row of an *Info message at any depth, by its key:
// the field's name, and for a map entry the map's key after a dot, as in
// "grp_id_to_md_raid.6" or "td_id_to_thin_info.9.slice_id_to_dm_thin.2". A
// nil info has no row.
func RowsOf(info any) map[string]*pb.ResInfo {
	out := make(map[string]*pb.ResInfo)
	msg, ok := info.(proto.Message)
	if !ok || msg == nil {
		return out
	}
	if m := msg.ProtoReflect(); m.IsValid() {
		walkRows("", m, out)
	}
	return out
}

func walkRows(prefix string, m protoreflect.Message, out map[string]*pb.ResInfo) {
	if row, ok := m.Interface().(*pb.ResInfo); ok {
		out[prefix] = row
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(fd.Name())
		if prefix != "" {
			name = prefix + "." + name
		}
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				return true
			}
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				walkRows(name+"."+k.String(), mv.Message(), out)
				return true
			})
		case fd.IsList():
			if fd.Message() == nil {
				return true
			}
			for i := range v.List().Len() {
				walkRows(fmt.Sprintf("%s.%d", name, i), v.List().Get(i).Message(), out)
			}
		case fd.Message() != nil:
			walkRows(name, v.Message(), out)
		}
		return true
	})
}
