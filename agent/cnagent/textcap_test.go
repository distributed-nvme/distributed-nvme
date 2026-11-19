package cnagent

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file pins the cn agent's half of the row text cap of architecture.md,
// Live-state reporting: the texts beside the rows, a group reading's refusal
// and a repair verdict's refusal and waits, are cut to common.AgentTextMax
// keeping their head and their end, as a row's text is; the forced start's
// dry run cuts its
// refusal as the real start's row does, so the two read the same; and the
// longest texts the agent composes from ids, md change counts and md
// identities fit the cap uncut at full widths.

// textcapWantCut asserts got is text cut to the cap: a head of text and its
// end around "...", within the cap.
func textcapWantCut(t *testing.T, what string, got string, text string) {
	t.Helper()
	head, tail, ok := strings.Cut(got, "...")
	if len(got) > common.AgentTextMax || !ok ||
		!strings.HasPrefix(text, head) || !strings.HasSuffix(text, tail) ||
		len(got) < common.AgentTextMax-6 {
		t.Errorf("%s is %q (%d bytes), want the text's head and end cut "+
			"to %d bytes around \"...\"", what, got, len(got),
			common.AgentTextMax)
	}
}

// TestGroupReadingCutsItsTexts pins the cut of the group reading's refusal
// and of the repair verdict's refusal and waits (reportGrpReading,
// repairVerdict): each over the cap is cut keeping its head and its end,
// which names the leg and the cause; a short one is kept whole.
func TestGroupReadingCutsItsTexts(t *testing.T) {
	gp := &grpPlan{grpId: 101, raid1: true}
	gp.legs = []*legPlan{{legId: 11, name: "dnv-leg-11", grp: gp}}
	gp.rep = &repPlan{repId: 7, rep: &pb.Repair{
		RepStage: pb.RepStage_REP_STAGE_CREATED,
	}}
	detail := &MdDetail{
		State:   "clean",
		Members: []MdMember{{DmName: "dnv-leg-11", State: "in_sync", Slot: "0"}},
	}
	long := func(what string) string {
		return strings.Repeat(what+" ", 60) + "leg 0xb: the cause"
	}
	refusals := grpRefusals{
		grp:   long("grp"),
		rep:   long("rep"),
		waits: long("waits"),
	}
	info := &pb.CntlrInfo{
		GrpIdToGrpInfo:    make(map[uint64]*pb.CntlrInfo_GrpInfo),
		RepIdToRepairInfo: make(map[uint64]*pb.CntlrInfo_RepairInfo),
	}
	reportGrpReading(info, gp, detail, refusals, 0, 0)
	textcapWantCut(t, "the group reading's refusal",
		info.GetGrpIdToGrpInfo()[101].GetRefusal(), refusals.grp)
	verdict := info.GetRepIdToRepairInfo()[7]
	textcapWantCut(t, "the repair verdict's refusal", verdict.GetRefusal(),
		refusals.rep)
	textcapWantCut(t, "the repair verdict's waits", verdict.GetWaits(),
		refusals.waits)
	short := grpRefusals{grp: "leg 0xb carries another array's superblock"}
	reportGrpReading(info, gp, detail, short, 0, 0)
	if got := info.GetGrpIdToGrpInfo()[101].GetRefusal(); got != short.grp {
		t.Errorf("a short refusal became %q", got)
	}
}

// TestForcedStartDryRunCutsItsRefusal pins that the dry run's refusal reads
// as the real start's row does (cnagent.md CN31): a refusal over the cap, as
// a failed read of the array can be, is cut by the same cut the row's
// writer makes.
func TestForcedStartDryRunCutsItsRefusal(t *testing.T) {
	long := errors.New("mdadm --detail /dev/md/x: " +
		strings.Repeat("output ", 80) + "the cause")
	s := &CnAgentServer{}
	gp := &grpPlan{grpId: 101, raid1: true}
	dry := s.forcedStartDryRun(context.Background(),
		&inspectLook{gp: gp, detailErr: long}, 11, false)
	textcapWantCut(t, "the dry run's refusal", dry.GetRefusal(), long.Error())
}

// TestComposedTextsFitTheCapUncut pins that the longest texts the cn agent
// composes, a forced start's refusals, a take-back's refusals and the
// assembly's refusal of two identities, fit common.AgentTextMax uncut with
// every id, md change count and list length at its widest and md's
// identities in their 35-character spelling: such a text is cut only when
// it carries a command's output.
func TestComposedTextsFitTheCapUncut(t *testing.T) {
	const id = uint64(math.MaxUint64)
	uuidA := "ffffffff:ffffffff:ffffffff:fffffff0"
	uuidB := "ffffffff:ffffffff:ffffffff:fffffff1"
	gp := &grpPlan{grpId: id, raid1: true, mark: &pb.ForcedStart{LegId: id}}
	named := &legPlan{legId: id, grp: gp}
	other := &legPlan{legId: id - 1, grp: gp}
	texts := map[string]string{
		"an older named leg": olderNamedLeg(gp, named,
			&mdSuperblock{Uuid: uuidA, Events: math.MaxUint64 - 1}, other,
			&mdSuperblock{Uuid: uuidA, Events: math.MaxUint64}),
		"a named leg with a bad-block list": badBlocksRefusal(gp, named,
			&mdSuperblock{Uuid: uuidA, BadBlocks: math.MaxInt}),
	}
	gp.rep = &repPlan{repId: id, rep: &pb.Repair{}}
	texts["a taken-back leg of another identity"] = takeBackRefusal(gp,
		named, &mdSuperblock{Uuid: uuidA}, other, &mdSuperblock{Uuid: uuidB})
	texts["a taken-back leg with newer data"] = takeBackRefusal(gp, named,
		&mdSuperblock{Uuid: uuidA, Events: math.MaxUint64}, other,
		&mdSuperblock{Uuid: uuidA, Events: math.MaxUint64 - 1})
	_, err := assemblyIdentity(gp, []assemblyCand{
		{lp: named, tier: assemblyTierLegList, sb: &mdSuperblock{Uuid: uuidA}},
		{lp: other, tier: assemblyTierLegList, sb: &mdSuperblock{Uuid: uuidB}},
	})
	if err == nil {
		t.Fatalf("the assembly took two identities")
	}
	texts["members of two identities"] = err.Error()
	for what, text := range texts {
		if text == "" {
			t.Errorf("%s: no text", what)
			continue
		}
		if len(text) > common.AgentTextMax {
			t.Errorf("%s is %d bytes at full widths, over the cap of %d, so "+
				"it would be cut: %q", what, len(text), common.AgentTextMax,
				text)
		}
	}
}
