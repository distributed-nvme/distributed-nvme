package cnagent

import (
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestMdadmCallsNameTheFormat pins cnagent.md CN12, The superblock format,
// over the md commands of a group's life: a create, a stop, an assembly, a
// forced start's zero, forced assembly and add, and a repair's fail, remove
// and add. Every create and assembly names the 1.2 format right after the md
// device, and every zero right after its mode word, so the members stay at
// the line's end; no add, fail, remove or stop names a format, mdadm
// refusing one in manage mode. Each kind of command is met at least once, so
// a flow that stops reaching one fails the test instead of passing it.
func TestMdadmCallsNameTheFormat(t *testing.T) {
	var calls []string
	keep := func(f *repFix) {
		calls = append(calls, f.node.callsMatching("cmd mdadm ")...)
		f.node.Reset()
	}
	standby := func(revision uint64) *pb.SyncupCntlrRequest {
		return cntlrReq(reqOpts{revision: revision, raid1: true,
			twoLegs: true})
	}

	f := newRepFix(t, allLegs...)
	keep(f)
	f.syncup(t, standby(3))
	keep(f)
	assertOk(t, f.syncup(t, noRepairReq(4)).GetGrpIdToMdRaid()[testDataGrp],
		"grp data after the assembly")
	keep(f)
	f.syncup(t, standby(5))
	keep(f)
	assertOk(t, f.syncup(t, withMark(noRepairReq(6), testDataLeg2)).
		GetGrpIdToMdRaid()[testDataGrp], "grp data after the forced start")
	keep(f)

	r := newRepFix(t, allLegs...)
	r.badLegDead()
	r.node.Reset()
	assertOk(t, r.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
		false)).GetGrpIdToMdRaid()[testDataGrp], "grp data after the repair")
	keep(r)

	seen := map[string]int{}
	for _, call := range calls {
		args := strings.Fields(strings.TrimPrefix(call, "cmd mdadm "))
		var kind string
		var at int
		switch {
		case args[0] == "--create":
			kind, at = "create", 2
		case args[0] == "--assemble" && slices.Contains(args, "--force"):
			kind, at = "forced assembly", 2
		case args[0] == "--assemble":
			kind, at = "assembly", 2
		case args[0] == "--zero-superblock":
			kind, at = "zero", 1
		case args[0] == "--stop":
			kind, at = "stop", -1
		case len(args) > 1 && slices.Contains(
			[]string{"--add", "--fail", "--remove"}, args[1]):
			kind, at = args[1], -1
		default:
			t.Fatalf("an mdadm command of no kind the test knows: %q", call)
		}
		seen[kind]++
		var formats []int
		for i, arg := range args {
			if strings.HasPrefix(arg, "--metadata") {
				formats = append(formats, i)
			}
		}
		switch {
		case at < 0 && len(formats) != 0:
			t.Fatalf("the %s %q names a superblock format", kind, call)
		case at >= 0 && (len(formats) != 1 || formats[0] != at ||
			args[at] != "--metadata=1.2"):
			t.Fatalf("the %s %q does not name --metadata=1.2 once, as "+
				"word %d", kind, call, at)
		case kind == "zero" && len(args) != 3:
			t.Fatalf("the zero %q is not the mode word, the format and "+
				"the member", call)
		}
	}
	for _, kind := range []string{"create", "assembly", "forced assembly",
		"zero", "stop", "--add", "--fail", "--remove"} {
		if seen[kind] == 0 {
			t.Fatalf("no %s among the md commands %q", kind, calls)
		}
	}
}
