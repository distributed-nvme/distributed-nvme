package worker

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// mdCheckHarness is the reaction harness over reactFixture with the md
// check set to days in the cluster's conf.
func mdCheckHarness(t *testing.T, days uint32) *reactHarness {
	t.Helper()
	h := newReactHarness(t, reactFixture(t))
	cc := reactClusterConf()
	cc.MdCheckConf = &pb.MdCheckConf{IntervalDays: days}
	setCachedConf(h.deps, testCid, cc)
	return h
}

// mdEdits is the edits of the pass's one AR16 write, nil when it made none.
func mdEdits(t *testing.T, h *reactHarness) []model.MdCheckEdit {
	t.Helper()
	h.rops.mu.Lock()
	defer h.rops.mu.Unlock()
	switch len(h.rops.mdChecks) {
	case 0:
		return nil
	case 1:
		return h.rops.mdChecks[0]
	}
	t.Fatalf("the pass made %d md check writes, want one",
		len(h.rops.mdChecks))
	return nil
}

func editOf(edits []model.MdCheckEdit, grpId uint64) *model.MdCheckEdit {
	for i := range edits {
		if edits[i].Ref.GrpId == grpId {
			return &edits[i]
		}
	}
	return nil
}

// TestMdCheckStampsNewGroups pins AR16's first time: a group never
// scheduled is stamped one interval ahead spread by its id over [0.5, 1.5)
// intervals, every such group of the pool in the pass's one write, with no
// request; a pool of a cluster whose setting is zero, and a pool without
// md-raid1, get no write at all.
func TestMdCheckStampsNewGroups(t *testing.T) {
	const days = 30
	interval := uint64(days * 24 * 3600)
	h := mdCheckHarness(t, days)
	h.pass()
	edits := mdEdits(t, h)
	if len(edits) != 2 {
		t.Fatalf("edits %v, want the two groups stamped", edits)
	}
	for _, grpId := range []uint64{reactMetaGrp, reactDataGrp} {
		edit := editOf(edits, grpId)
		if edit == nil || edit.From.GetNextEpoch() != 0 ||
			edit.To.GetRequestEpoch() != 0 {
			t.Fatalf("group %d: edit %+v, want a stamp", grpId, edit)
		}
		next := edit.To.GetNextEpoch()
		if next < h.now()+interval/2 || next >= h.now()+interval*3/2 {
			t.Fatalf("group %d: next %d outside [now+%d, now+%d)", grpId,
				next, interval/2, interval*3/2)
		}
	}
	if recs := h.logs.withMsg(msgMdChecks); len(recs) != 1 {
		t.Fatalf("records %v, want one %q", recs, msgMdChecks)
	}

	for _, tc := range []struct {
		name  string
		days  uint32
		raid1 bool
	}{{"the check off", 0, true}, {"no md-raid1", days, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := mdCheckHarness(t, tc.days)
			if !tc.raid1 {
				h.state.Conf.BdevConf.RedundConf = nil
			}
			h.pass()
			if edits := mdEdits(t, h); edits != nil {
				t.Fatalf("edits %v, want none", edits)
			}
		})
	}
}

// TestMdCheckAsksForOneDueGroup pins AR16's request: of the due groups the
// one with the earliest time that a check suits is asked for, request_epoch
// now and its time kept; nothing is asked while a request stands; and a
// group a check does not suit — a repair, a leg with two sides, a mark, the
// operator's flag, a row not OK, a reading not whole, one leg — is passed
// over.
func TestMdCheckAsksForOneDueGroup(t *testing.T) {
	due := func(h *reactHarness, meta, data uint64) {
		h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: meta}
		h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: data}
	}
	t.Run("the earliest", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		due(h, h.ago(10), h.ago(20))
		h.pass()
		edits := mdEdits(t, h)
		edit := editOf(edits, reactDataGrp)
		if len(edits) != 1 || edit == nil ||
			edit.To.GetRequestEpoch() != h.now() ||
			edit.To.GetNextEpoch() != h.ago(20) {
			t.Fatalf("edits %+v, want the data group asked for now", edits)
		}
	})
	t.Run("a request stands", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		due(h, h.ago(10), h.ago(20))
		h.metaGrp().MdCheck.RequestEpoch = h.ago(5)
		h.pass()
		if edits := mdEdits(t, h); edits != nil {
			t.Fatalf("edits %+v while a request stands, want none", edits)
		}
	})
	for _, tc := range []struct {
		name  string
		block func(h *reactHarness)
	}{
		{"a repair", func(h *reactHarness) { h.dataGrp().RepName = "r" }},
		{"a leg with two sides", func(h *reactHarness) {
			leg := h.dataGrp().GetLegList()[0]
			leg.SideList = append(leg.SideList, &pb.Side{SideId: 9})
		}},
		{"a mark", func(h *reactHarness) {
			h.dataGrp().ForcedStart = &pb.ForcedStart{LegId: reactDataLegA}
		}},
		{"the operator's flag", func(h *reactHarness) {
			h.dataGrp().OperatorInCharge = true
		}},
		{"a row not OK", func(h *reactHarness) {
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		}},
		{"a reading not whole", func(h *reactHarness) {
			h.setGrpInfo(reactDataGrp, false, reactDataLegA)
		}},
		{"one leg", func(h *reactHarness) {
			h.dataGrp().LegList = h.dataGrp().LegList[:1]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := mdCheckHarness(t, 30)
			due(h, h.ago(10), h.ago(20))
			tc.block(h)
			h.pass()
			edits := mdEdits(t, h)
			edit := editOf(edits, reactMetaGrp)
			if len(edits) != 1 || edit == nil ||
				edit.To.GetRequestEpoch() != h.now() {
				t.Fatalf("edits %+v, want the meta group asked for", edits)
			}
		})
	}
}

// TestMdCheckEndsARequest pins AR16's end: a request the primary's reading
// names done, or one that has stood MdCheckRequestBound, is ended with the
// next time one interval ahead; in the same write another due group is
// asked for, as no request stands then; one that stands short of the bound
// and is not done is left.
func TestMdCheckEndsARequest(t *testing.T) {
	const days = 30
	interval := uint64(days * 24 * 3600)
	for _, tc := range []struct {
		name    string
		age     uint64
		done    bool
		wantEnd bool
	}{
		{"done", 60, true, true},
		{"past the bound", common.MdCheckRequestBound, false, true},
		{"short of the bound", common.MdCheckRequestBound - 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := mdCheckHarness(t, days)
			req := h.ago(tc.age)
			h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(tc.age + 1),
				RequestEpoch: req}
			h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(1)}
			h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
			if tc.done {
				h.w.cntlrs[reactCntlrA].driver.lastInfo.
					GrpIdToGrpInfo[reactDataGrp].MdCheckDoneEpoch = req
			}
			h.pass()
			edits := mdEdits(t, h)
			end := editOf(edits, reactDataGrp)
			if !tc.wantEnd {
				if edits != nil {
					t.Fatalf("edits %+v, want none", edits)
				}
				return
			}
			if end == nil || end.To.GetRequestEpoch() != 0 ||
				end.To.GetNextEpoch() != h.now()+interval {
				t.Fatalf("edits %+v, want the data group's request ended",
					edits)
			}
			ask := editOf(edits, reactMetaGrp)
			if ask == nil || ask.To.GetRequestEpoch() != h.now() {
				t.Fatalf("edits %+v, want the meta group asked for", edits)
			}
		})
	}
}

// TestMdCheckWithdrawsARequestOnAGroupThatStopsSuiting pins AR16's
// withdrawal: a group whose request stands and that gets a repair has the
// request taken away, its time kept, so it is asked for again once it
// suits.
func TestMdCheckWithdrawsARequestOnAGroupThatStopsSuiting(t *testing.T) {
	h := mdCheckHarness(t, 30)
	h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(1)}
	h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(100),
		RequestEpoch: h.ago(50)}
	h.dataGrp().RepName = "r"
	h.pass()
	edits := mdEdits(t, h)
	edit := editOf(edits, reactDataGrp)
	if edit == nil || edit.To.GetRequestEpoch() != 0 ||
		edit.To.GetNextEpoch() != h.ago(100) {
		t.Fatalf("edits %+v, want the data group's request withdrawn, its "+
			"time kept", edits)
	}
}

// TestMdCheckKeepsTheDoneTime pins that every AR16 edit but the end of a
// done request keeps the group's done_epoch, which AR8 reads: the end of a
// request past its bound, and the withdrawal of a request on a group that
// stops suiting a check.
func TestMdCheckKeepsTheDoneTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		grp  func(h *reactHarness)
	}{
		{"past the bound", func(h *reactHarness) {
			h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(1),
				RequestEpoch: h.ago(common.MdCheckRequestBound),
				DoneEpoch:    h.ago(7)}
		}},
		{"withdrawn", func(h *reactHarness) {
			h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(1),
				RequestEpoch: h.ago(50), DoneEpoch: h.ago(7)}
			h.dataGrp().RepName = "r"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := mdCheckHarness(t, 30)
			h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.now() + 1000}
			tc.grp(h)
			h.pass()
			edit := editOf(mdEdits(t, h), reactDataGrp)
			if edit == nil || edit.To.GetRequestEpoch() != 0 ||
				edit.To.GetDoneEpoch() != h.ago(7) {
				t.Fatalf("edit %+v, want the request ended, done_epoch kept",
					edit)
			}
		})
	}
}
