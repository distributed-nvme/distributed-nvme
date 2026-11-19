package cnagent

import (
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The forced start's refusals that an override passes (cnagent.md CN12,
// CN31): the start meets every one of them before it refuses, its row names
// their kinds first, its record lists them whole, and the dry run lists
// each, passed or not.

// wantRefusals checks the dry run's list against kinds, in order, each
// entry's text holding the matching want and its passed flag as passed.
func wantRefusals(
	t *testing.T,
	label string,
	dry *pb.ForcedStartDryRun,
	passed bool,
	kinds []pb.ForcedStartOverride,
	wants []string,
) {
	t.Helper()
	list := dry.GetRefusalList()
	if len(list) != len(kinds) {
		t.Fatalf("%s: the dry run lists %v, want %d refusals %v", label,
			list, len(kinds), kinds)
	}
	for i, entry := range list {
		if entry.GetOverride() != kinds[i] ||
			!strings.Contains(entry.GetText(), wants[i]) ||
			entry.GetOverridden() != passed {
			t.Errorf("%s: refusal %d is %v, want %v holding %q, passed %v",
				label, i, entry, kinds[i], wants[i], passed)
		}
	}
}

// TestForcedStartNamesEveryRefusal pins CN12's refusals of a named leg that
// both carries a bad-block list and is older than the member the start
// would zero: with no force the start meets both before it refuses and
// zeroes nothing, its row names both kinds at its head and both refusals,
// one Warn record lists each whole, and the read names no forced start.
// The dry run lists the two, neither passed, and refuses in the row's own
// words; with the force it lists both as passed and names the member
// zeroed and the list dropped.
func TestForcedStartNamesEveryRefusal(t *testing.T) {
	f := downNoRepair(t)
	f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10, badBlocks: 3})
	f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
	f.node.forgetTraces()
	logs := captureLogs(t)
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	row := info.GetGrpIdToMdRaid()[testDataGrp]
	if !strings.HasPrefix(row.GetDetails(),
		"forced start refused (bad-blocks, older-leg): ") {
		t.Errorf("the row reads %q, want it to name both kinds first",
			row.GetDetails())
	}
	for _, want := range []string{
		"named leg 0x17 carries a bad-block list of 3 entries",
		"above the 10 of named leg 0x17",
	} {
		assertErrorDetails(t, row, want, "grp data")
	}
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
	recs := logs.msgRecords(t, msgForcedStartRefused)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1", len(recs), msgForcedStartRefused)
	}
	rec := recs[0]
	got, _ := rec["refusals"].([]any)
	if rec["level"] != "WARN" || rec["grp_id"] != float64(testDataGrp) ||
		rec["leg_id"] != float64(testDataLeg2) || len(got) != 2 {
		t.Fatalf("the record is %v, want group %#x, leg %#x and two "+
			"refusals", rec, testDataGrp, testDataLeg2)
	}
	for i, want := range []string{
		"bad-blocks: named leg 0x17 carries a bad-block list of 3 entries",
		"older-leg: leg 0x7 is a full member with md change count 50",
	} {
		if text, _ := got[i].(string); !strings.HasPrefix(text, want) {
			t.Errorf("the record's refusal %d is %q, want %q first", i,
				got[i], want)
		}
	}
	f.assertForcedStartUnnamed(t, 4, "the two refusals")

	f.node.Reset()
	kinds := []pb.ForcedStartOverride{
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_BAD_BLOCKS,
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_OLDER_LEG,
	}
	wants := []string{"a bad-block list of 3 entries", "change count 50"}
	dry := f.inspect(t, testDataLeg2, false).GetDryRun()
	if dry.GetRefusal() != row.GetDetails() ||
		len(dry.GetZeroLegIdList()) != 0 {
		t.Errorf("the dry run with no force: %v, want the row's refusal %q "+
			"and nothing zeroed", dry, row.GetDetails())
	}
	wantRefusals(t, "with no force", dry, false, kinds, wants)
	dry = f.inspect(t, testDataLeg2, true).GetDryRun()
	if dry.GetRefusal() != "" ||
		!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg}) ||
		dry.GetUpdate() != mdUpdateUuid+","+mdUpdateForceNoBbl {
		t.Errorf("the dry run with the force: %v, want leg 1 zeroed and "+
			"the list dropped", dry)
	}
	wantRefusals(t, "with the force", dry, true, kinds, wants)
	assertNoCall(t, f.node, "cmd mdadm")
}

// TestForcedStartListStopsAtAHardRefusal pins the end of the dry run's
// list (cnagent.md CN12, CN31): a refusal no override passes, here another
// member whose read does not answer, ends the checks, the start's and the
// dry run's refusal is its own, and the list holds what was met before it.
func TestForcedStartListStopsAtAHardRefusal(t *testing.T) {
	f := downNoRepair(t)
	f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 2})
	f.node.mu.Lock()
	f.node.failCmdAlways["readsb "+f.leg1] = ""
	f.node.mu.Unlock()
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"forced start: member leg 0x7", "grp data")
	f.node.forgetTraces()
	dry := f.inspect(t, testDataLeg2, false).GetDryRun()
	if !strings.HasPrefix(dry.GetRefusal(), "forced start: member leg 0x7") {
		t.Errorf("the dry run refuses %q, want the member's read", dry.GetRefusal())
	}
	wantRefusals(t, "a hard refusal", dry, false,
		[]pb.ForcedStartOverride{
			pb.ForcedStartOverride_FORCED_START_OVERRIDE_BAD_BLOCKS},
		[]string{"a bad-block list of 2 entries"})
}
