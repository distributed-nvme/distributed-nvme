package model

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The discovery listing rule's own suite (architecture.md [D18]; model half):
// a cntlr's transport address is in every CdcEntry of its SP while the cntlr
// is enabled and either is the primary or has a zero err_epoch, and each of
// the three writers in this package — the cntlr health write, Failover and
// ReplaceCntlr — sets the whole list from the cntlrs its transaction reads.
//
// The op tests grow the MD6 fixture of ops_test.go: more cntlrs through
// gainCntlr, more subsystems through addSubsystem, and every CdcEntry they
// check is first written by putCdcEntry, which gives it the allowed_hosts
// wantCdc expects back. An entry that must not be put is proven so by its
// mod_revision, and is seeded with a list the rule would correct wherever a
// rewrite could otherwise go unseen. nqn_list holds the subsystems in the
// order a test adds them, and a subsystem the rewrite skips — no CdcEntry, or
// one that already holds the rule's list — is listed before one it must
// rewrite, so a skip that ended the walk of nqn_list would leave that one
// stale.

// cdcSs is one subsystem of these tests: its NQN and its ss_id, which address
// its Subsystem record and its CdcEntry.
type cdcSs struct {
	nqn  string
	ssId uint64
}

var (
	// cdcSs1 is the fixture's own subsystem; addSubsystem writes the others.
	cdcSs1 = cdcSs{nqn: opsNqn, ssId: opsSsId}
	cdcSs2 = cdcSs{nqn: opsNqn + "-2", ssId: opsSsId + 1}
	cdcSs3 = cdcSs{nqn: opsNqn + "-3", ssId: opsSsId + 2}
	cdcSs4 = cdcSs{nqn: opsNqn + "-4", ssId: opsSsId + 3}
	cdcSs5 = cdcSs{nqn: opsNqn + "-5", ssId: opsSsId + 4}
	// cdcSsLost is listed in nqn_list by the tests that need it and never
	// has a Subsystem record.
	cdcSsLost = cdcSs{nqn: opsNqn + "-lost", ssId: opsSsId + 9}
)

// cdcHosts is the allowed_hosts of every CdcEntry putCdcEntry writes; no
// rewrite by the listing rule may touch it.
var cdcHosts = []string{"nqn.2014-08.org.nvmexpress:uuid:cdc-host"}

const (
	// cdcLostCntlr is a cntlr_id the tests that need it list in
	// cntlr_id_list without a Cntlr record.
	cdcLostCntlr = uint64(299)
	// cdcOtherSpId and cdcOtherSsId are the SP that re-uses the fixture's
	// name and the ss_id of its subsystem.
	cdcOtherSpId = uint64(0x200)
	cdcOtherSsId = uint64(601)
)

// cdcKey is the CdcEntry key of one subsystem of the fixture SP.
func (e *opsEnv) cdcKey(ss cdcSs) string {
	return CdcEntryKey(e.cid, opsShard, opsSpId, ss.ssId)
}

// listNqn appends one NQN to the fixture SP's nqn_list and writes nothing
// else.
func (e *opsEnv) listNqn(ss cdcSs) {
	e.t.Helper()
	conf := e.spConf()
	conf.NqnList = append(conf.GetNqnList(), ss.nqn)
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
}

// addSubsystem writes one more Subsystem of the fixture SP and lists it in
// nqn_list, with no CdcEntry: putCdcEntry writes one where a test wants it.
func (e *opsEnv) addSubsystem(ss cdcSs) {
	e.t.Helper()
	mustPut(e.t, e.cli, SubsystemKey(e.cid, opsSpId, ss.nqn), &pb.Subsystem{
		SsId: ss.ssId, Serial: ss.nqn, Model: "m0",
	})
	e.listNqn(ss)
}

// putCdcEntry writes one subsystem's CdcEntry advertising the transports of
// addrs, in order.
func (e *opsEnv) putCdcEntry(ss cdcSs, addrs ...string) {
	e.t.Helper()
	list := make([]*pb.NvmeTrConf, 0, len(addrs))
	for _, addrPort := range addrs {
		list = append(list, opsTrConf(addrPort))
	}
	mustPut(e.t, e.cli, e.cdcKey(ss), &pb.CdcEntry{
		Nqn:            ss.nqn,
		NvmeTrConfList: list,
		AllowedHosts:   cdcHosts,
	})
}

// wantCdc asserts that one CdcEntry advertises exactly the transports of
// addrs, in order, and still carries the nqn and allowed_hosts putCdcEntry
// wrote: the rule rewrites nvme_tr_conf_list alone.
func (e *opsEnv) wantCdc(ss cdcSs, addrs ...string) {
	e.t.Helper()
	entry := &pb.CdcEntry{}
	e.get(e.cdcKey(ss), entry)
	got := entry.GetNvmeTrConfList()
	same := len(got) == len(addrs)
	for idx := 0; same && idx < len(got); idx++ {
		same = proto.Equal(got[idx], opsTrConf(addrs[idx]))
	}
	if !same {
		gotAddrs := make([]string, 0, len(got))
		for _, tr := range got {
			gotAddrs = append(gotAddrs, tr.GetTrAddr())
		}
		e.t.Errorf("cdc entry of %s: got %v, want %v", ss.nqn, gotAddrs, addrs)
	}
	if entry.GetNqn() != ss.nqn ||
		!slices.Equal(entry.GetAllowedHosts(), cdcHosts) {
		e.t.Errorf("cdc entry of %s: nqn %q allowed_hosts %v must be kept",
			ss.nqn, entry.GetNqn(), entry.GetAllowedHosts())
	}
}

// setCntlrDisabled sets one cntlr's disabled flag directly, bypassing the
// gateway's UpdateCntlrEnabled and its CdcEntry rewrite.
func (e *opsEnv) setCntlrDisabled(cntlrId uint64, disabled bool) {
	e.t.Helper()
	cntlr := e.cntlr(cntlrId)
	cntlr.Disabled = disabled
	mustPut(e.t, e.cli, CntlrKey(e.cid, opsSpId, cntlrId), cntlr)
}

// setCntlrHealth runs the cntlr health write of one cntlr of the fixture SP
// through the op, unlike setCntlrErr, and fails the test on an error.
func (e *opsEnv) setCntlrHealth(cntlrId uint64, epoch uint64, settle bool) {
	e.t.Helper()
	if err := SetCntlrErrEpoch(
		e.ctx, e.cli, e.cid, opsSpId, cntlrId, epoch, settle,
	); err != nil {
		e.t.Fatalf("SetCntlrErrEpoch(%d, %d, %v): %v",
			cntlrId, epoch, settle, err)
	}
}

// modRevs is the mod_revision of each key, in order.
func (e *opsEnv) modRevs(keys ...string) []int64 {
	e.t.Helper()
	revs := make([]int64, 0, len(keys))
	for _, key := range keys {
		revs = append(revs, e.modRev(key))
	}
	return revs
}

// ---------------------------------------------------------------------------
// The rule itself (no etcd needed)
// ---------------------------------------------------------------------------

// TestCdcListed is the listing rule as a truth table: a cntlr is listed while
// it is enabled and either is the primary or has a zero err_epoch. settling
// steers nothing here, so every row holds with it set too.
func TestCdcListed(t *testing.T) {
	for _, tc := range []struct {
		disabled bool
		primary  bool
		errEpoch uint64
		want     bool
	}{
		{disabled: false, primary: true, errEpoch: 0, want: true},
		{disabled: false, primary: true, errEpoch: 1000, want: true},
		{disabled: false, primary: false, errEpoch: 0, want: true},
		{disabled: false, primary: false, errEpoch: 1000, want: false},
		{disabled: true, primary: true, errEpoch: 0, want: false},
		{disabled: true, primary: true, errEpoch: 1000, want: false},
		{disabled: true, primary: false, errEpoch: 0, want: false},
		{disabled: true, primary: false, errEpoch: 1000, want: false},
	} {
		for _, settling := range []bool{false, true} {
			cntlr := &pb.Cntlr{
				AddrPort:   opsCnA,
				NvmeTrConf: opsTrConf(opsCnA),
				Disabled:   tc.disabled,
				Primary:    tc.primary,
				ErrEpoch:   tc.errEpoch,
				Settling:   settling,
			}
			if got := CdcListed(cntlr); got != tc.want {
				t.Errorf(
					"disabled %v primary %v err_epoch %d settling %v: "+
						"got %v, want %v",
					tc.disabled, tc.primary, tc.errEpoch, settling,
					got, tc.want,
				)
			}
		}
	}
}

// TestCdcTrConfList pins the list every CdcEntry carries: the nvme_tr_conf of
// each cntlr CdcListed lists, in the order the cntlrs are given — an order no
// sort by address reproduces here — and nothing of the others.
func TestCdcTrConfList(t *testing.T) {
	const cnE = "cn-e:9000"
	cntlrs := []*pb.Cntlr{
		// A healthy standby: listed.
		{AddrPort: opsCnC, NvmeTrConf: opsTrConf(opsCnC)},
		// A primary with an err_epoch: listed.
		{AddrPort: opsCnA, NvmeTrConf: opsTrConf(opsCnA),
			Primary: true, ErrEpoch: 1000},
		// An unhealthy standby: not listed.
		{AddrPort: opsCnD, NvmeTrConf: opsTrConf(opsCnD), ErrEpoch: 1000},
		// A disabled standby: not listed.
		{AddrPort: opsCnB, NvmeTrConf: opsTrConf(opsCnB), Disabled: true},
		// A healthy standby: listed.
		{AddrPort: cnE, NvmeTrConf: opsTrConf(cnE)},
	}
	check := func(what string, got []*pb.NvmeTrConf, want []string) {
		t.Helper()
		same := len(got) == len(want)
		for idx := 0; same && idx < len(got); idx++ {
			same = proto.Equal(got[idx], opsTrConf(want[idx]))
		}
		if !same {
			t.Errorf("%s: got %v, want the transports of %v", what, got, want)
		}
	}
	check("given order", CdcTrConfList(cntlrs),
		[]string{opsCnC, opsCnA, cnE})
	reversed := slices.Clone(cntlrs)
	slices.Reverse(reversed)
	check("reversed order", CdcTrConfList(reversed),
		[]string{cnE, opsCnA, opsCnC})
	if got := CdcTrConfList(cntlrs[2:4]); len(got) != 0 {
		t.Errorf("no listed cntlr: got %v, want an empty list", got)
	}
	if got := CdcTrConfList(nil); len(got) != 0 {
		t.Errorf("no cntlr: got %v, want an empty list", got)
	}
}

// TestTrConfListEqual pins the rewrite's "already the rule's list" test: equal
// members in the same order, so an entry holding the right transports out of
// order is still rewritten.
func TestTrConfListEqual(t *testing.T) {
	trA, trB := opsTrConf(opsCnA), opsTrConf(opsCnB)
	otherPort := opsTrConf(opsCnA)
	otherPort.TrSvcId = "4421"
	for _, tc := range []struct {
		name string
		a    []*pb.NvmeTrConf
		b    []*pb.NvmeTrConf
		want bool
	}{
		{"both empty", nil, []*pb.NvmeTrConf{}, true},
		{
			"equal members, same order",
			[]*pb.NvmeTrConf{trA, trB},
			[]*pb.NvmeTrConf{opsTrConf(opsCnA), opsTrConf(opsCnB)},
			true,
		},
		{
			"equal members, other order",
			[]*pb.NvmeTrConf{trA, trB},
			[]*pb.NvmeTrConf{trB, trA},
			false,
		},
		{
			"one member more",
			[]*pb.NvmeTrConf{trA},
			[]*pb.NvmeTrConf{trA, trB},
			false,
		},
		{
			"one field differs",
			[]*pb.NvmeTrConf{trA},
			[]*pb.NvmeTrConf{otherPort},
			false,
		},
	} {
		if got := trConfListEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if got := trConfListEqual(tc.b, tc.a); got != tc.want {
			t.Errorf("%s, swapped: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Failover
// ---------------------------------------------------------------------------

// TestFailoverRewritesEveryCdcEntry pins the rule in Failover's own
// transaction, on the threshold trigger: the old primary carries the
// err_epoch that fired it, so as a standby it leaves every CdcEntry; the new
// primary and the other healthy standby stay, in cntlr_id_list order; the
// disabled cntlr stays out. Each entry gets the whole list, whatever it
// holds; one that already holds it is not put; a subsystem with no CdcEntry
// is not given one.
func TestFailoverRewritesEveryCdcEntry(t *testing.T) {
	env := newOpsEnv(t)
	// cntlr_id_list [A, B, C, D]: C a healthy standby whose id is above B's,
	// so B stays the elected candidate, and D disabled.
	env.gainCntlr(opsCnC)
	cntlrD := env.gainCntlr(opsCnD)
	env.setCntlrDisabled(cntlrD, true)
	env.setCntlrErr(opsCntlrA, 1000)
	// In nqn_list order: cdcSs1 holds the rule's list before the failover;
	// cdcSs2 already the list the failover leaves; cdcSs3 has no CdcEntry;
	// cdcSs4 a stale list, with D in it, B missing and the order wrong;
	// cdcSs5 the members the failover leaves, out of order. The two the
	// rewrite skips come before the last two, which it must rewrite.
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB, opsCnC)
	env.addSubsystem(cdcSs2)
	env.putCdcEntry(cdcSs2, opsCnB, opsCnC)
	env.addSubsystem(cdcSs3)
	env.addSubsystem(cdcSs4)
	env.putCdcEntry(cdcSs4, opsCnD, opsCnC, opsCnA)
	env.addSubsystem(cdcSs5)
	env.putCdcEntry(cdcSs5, opsCnC, opsCnB)
	modBefore := env.modRev(env.cdcKey(cdcSs2))
	revBefore := env.spRev()
	if err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, 1000+common.DefaultPrimaryUnhealthy,
	); err != nil {
		t.Fatalf("Failover: %v", err)
	}
	if env.cntlr(opsCntlrA).GetPrimary() || !env.cntlr(opsCntlrB).GetPrimary() {
		t.Fatalf("the role must move from A to B")
	}
	for _, ss := range []cdcSs{cdcSs1, cdcSs2, cdcSs4, cdcSs5} {
		env.wantCdc(ss, opsCnB, opsCnC)
	}
	if got := env.modRev(env.cdcKey(cdcSs2)); got != modBefore {
		t.Errorf("an entry that already holds the rule's list must not be "+
			"put: mod_revision %d -> %d", modBefore, got)
	}
	if env.exists(env.cdcKey(cdcSs3)) {
		t.Errorf("a missing CdcEntry must stay missing")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)",
			got, revBefore+1)
	}
}

// TestFailoverDisabledPrimaryLeavesCdcEntries is the disabled trigger: the
// disable that fired it took the old primary out of the records already, and
// the failover moves no other cntlr in or out of the rule, so no CdcEntry is
// put while the role still moves and SpRev is still bumped.
func TestFailoverDisabledPrimaryLeavesCdcEntries(t *testing.T) {
	env := newOpsEnv(t)
	env.gainCntlr(opsCnC)
	env.setCntlrDisabled(opsCntlrA, true)
	env.putCdcEntry(cdcSs1, opsCnB, opsCnC)
	env.addSubsystem(cdcSs2)
	env.putCdcEntry(cdcSs2, opsCnB, opsCnC)
	keys := []string{env.cdcKey(cdcSs1), env.cdcKey(cdcSs2)}
	modsBefore := env.modRevs(keys...)
	revBefore := env.spRev()
	// The clock reads the fixture's own epoch: the disabled trigger waits for
	// nothing.
	if err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, 1000,
	); err != nil {
		t.Fatalf("Failover: %v", err)
	}
	if env.cntlr(opsCntlrA).GetPrimary() || !env.cntlr(opsCntlrB).GetPrimary() {
		t.Fatalf("the role must move from A to B")
	}
	env.wantCdc(cdcSs1, opsCnB, opsCnC)
	env.wantCdc(cdcSs2, opsCnB, opsCnC)
	if got := env.modRevs(keys...); !slices.Equal(got, modsBefore) {
		t.Errorf("no CdcEntry may be put: mod_revisions %v -> %v",
			modsBefore, got)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
}

// TestFailoverRefusesAMissingSubsystem: a subsystem nqn_list names whose
// record is missing has no CdcEntry key to form, so the whole op fails rather
// than leave its entry advertising a cntlr the rule does not list. It is
// listed after an entry the failover would rewrite, which must still not be
// put: an op never writes half of what it owes.
func TestFailoverRefusesAMissingSubsystem(t *testing.T) {
	env := newOpsEnv(t)
	env.setCntlrErr(opsCntlrA, 1000)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB)
	env.listNqn(cdcSsLost)
	keys := []string{
		env.cdcKey(cdcSs1),
		CntlrKey(env.cid, opsSpId, opsCntlrA),
		CntlrKey(env.cid, opsSpId, opsCntlrB),
	}
	modsBefore := env.modRevs(keys...)
	revBefore := env.spRev()
	err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, 1000+common.DefaultPrimaryUnhealthy,
	)
	precondition := wantPrecondition(t, err, opFailover)
	if precondition.Reason != "subsystem not found" {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, "subsystem not found")
	}
	if got := env.modRevs(keys...); !slices.Equal(got, modsBefore) {
		t.Errorf("an aborted op must put nothing: mod_revisions %v -> %v",
			modsBefore, got)
	}
	if !env.cntlr(opsCntlrA).GetPrimary() {
		t.Errorf("the role must not move")
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("an aborted op must not bump SpRev: got %d", got)
	}
}

// ---------------------------------------------------------------------------
// The cntlr health write
// ---------------------------------------------------------------------------

// TestSetCntlrErrEpochListsAStandbyByItsHealth pins the health write's share
// of the rule: an enabled standby whose err_epoch is set leaves every
// CdcEntry, and the clear lists it again in its cntlr_id_list place, not at
// the end; an entry that already holds the rule's list is not put; and
// neither write bumps SpRev, as no health write does.
func TestSetCntlrErrEpochListsAStandbyByItsHealth(t *testing.T) {
	env := newOpsEnv(t)
	env.gainCntlr(opsCnC)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB, opsCnC)
	// cdcSs2 already holds what the set leaves; cdcSs3, after it, must still
	// be rewritten.
	env.addSubsystem(cdcSs2)
	env.putCdcEntry(cdcSs2, opsCnA, opsCnC)
	env.addSubsystem(cdcSs3)
	env.putCdcEntry(cdcSs3, opsCnA, opsCnB, opsCnC)
	modBefore := env.modRev(env.cdcKey(cdcSs2))
	revBefore := env.spRev()
	env.setCntlrHealth(opsCntlrB, 1000, false)
	if got := env.cntlr(opsCntlrB).GetErrEpoch(); got != 1000 {
		t.Errorf("err_epoch: got %d, want 1000", got)
	}
	for _, ss := range []cdcSs{cdcSs1, cdcSs2, cdcSs3} {
		env.wantCdc(ss, opsCnA, opsCnC)
	}
	if got := env.modRev(env.cdcKey(cdcSs2)); got != modBefore {
		t.Errorf("an entry that already holds the rule's list must not be "+
			"put: mod_revision %d -> %d", modBefore, got)
	}
	env.setCntlrHealth(opsCntlrB, 0, false)
	for _, ss := range []cdcSs{cdcSs1, cdcSs2, cdcSs3} {
		env.wantCdc(ss, opsCnA, opsCnB, opsCnC)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d",
			got, revBefore)
	}
}

// TestSetCntlrErrEpochLeavesCdcEntries: a health write that moves its cntlr
// neither into nor out of the rule puts no CdcEntry. Every case seeds the
// entry with a list the rule would correct — reversed — so that a rewrite
// would show in it and in its mod_revision.
func TestSetCntlrErrEpochLeavesCdcEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(env *opsEnv)
		cntlrId uint64
		epoch   uint64
		settle  bool
		// wantEpoch is the cntlr's err_epoch after the write, and wantPut
		// whether the write put the cntlr at all.
		wantEpoch uint64
		wantPut   bool
	}{
		{
			name:      "primary set",
			setup:     func(env *opsEnv) {},
			cntlrId:   opsCntlrA,
			epoch:     1000,
			wantEpoch: 1000,
			wantPut:   true,
		},
		{
			name:      "primary cleared",
			setup:     func(env *opsEnv) { env.setCntlrErr(opsCntlrA, 1000) },
			cntlrId:   opsCntlrA,
			epoch:     0,
			wantEpoch: 0,
			wantPut:   true,
		},
		{
			name: "disabled standby set",
			setup: func(env *opsEnv) {
				env.setCntlrDisabled(opsCntlrB, true)
			},
			cntlrId:   opsCntlrB,
			epoch:     1000,
			wantEpoch: 1000,
			wantPut:   true,
		},
		{
			name: "disabled standby cleared",
			setup: func(env *opsEnv) {
				env.setCntlrDisabled(opsCntlrB, true)
				env.setCntlrErr(opsCntlrB, 1000)
			},
			cntlrId:   opsCntlrB,
			epoch:     0,
			wantEpoch: 0,
			wantPut:   true,
		},
		{
			name: "disabled primary set",
			setup: func(env *opsEnv) {
				env.setCntlrDisabled(opsCntlrA, true)
			},
			cntlrId:   opsCntlrA,
			epoch:     1000,
			wantEpoch: 1000,
			wantPut:   true,
		},
		{
			// The settle of a primary seen clean with its stack built (HL2).
			name: "settle only",
			setup: func(env *opsEnv) {
				env.setCntlrSettling(opsCntlrA, true)
			},
			cntlrId:   opsCntlrA,
			epoch:     0,
			settle:    true,
			wantEpoch: 0,
			wantPut:   true,
		},
		{
			name: "settle with the clear of a primary",
			setup: func(env *opsEnv) {
				env.setCntlrSettling(opsCntlrA, true)
				env.setCntlrErr(opsCntlrA, 1000)
			},
			cntlrId:   opsCntlrA,
			epoch:     0,
			settle:    true,
			wantEpoch: 0,
			wantPut:   true,
		},
		{
			// The threshold clock never restarts: nothing to write.
			name:      "later epoch on an unhealthy standby",
			setup:     func(env *opsEnv) { env.setCntlrErr(opsCntlrB, 1000) },
			cntlrId:   opsCntlrB,
			epoch:     2000,
			wantEpoch: 1000,
			wantPut:   false,
		},
		{
			name:      "clear of a healthy standby",
			setup:     func(env *opsEnv) {},
			cntlrId:   opsCntlrB,
			epoch:     0,
			wantEpoch: 0,
			wantPut:   false,
		},
		{
			// A settle with a nonzero epoch proves no role (HL2).
			name: "settle with an epoch",
			setup: func(env *opsEnv) {
				env.setCntlrSettling(opsCntlrA, true)
				env.setCntlrErr(opsCntlrA, 1000)
			},
			cntlrId:   opsCntlrA,
			epoch:     2000,
			settle:    true,
			wantEpoch: 1000,
			wantPut:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			tc.setup(env)
			env.putCdcEntry(cdcSs1, opsCnB, opsCnA)
			entryKey := env.cdcKey(cdcSs1)
			cntlrKey := CntlrKey(env.cid, opsSpId, tc.cntlrId)
			entryMod, cntlrMod := env.modRev(entryKey), env.modRev(cntlrKey)
			revBefore := env.spRev()
			env.setCntlrHealth(tc.cntlrId, tc.epoch, tc.settle)
			if got := env.cntlr(tc.cntlrId).GetErrEpoch(); got != tc.wantEpoch {
				t.Errorf("err_epoch: got %d, want %d", got, tc.wantEpoch)
			}
			if put := env.modRev(cntlrKey) != cntlrMod; put != tc.wantPut {
				t.Errorf("cntlr put: got %v, want %v", put, tc.wantPut)
			}
			env.wantCdc(cdcSs1, opsCnB, opsCnA)
			if got := env.modRev(entryKey); got != entryMod {
				t.Errorf("the CdcEntry must not be put: mod_revision "+
					"%d -> %d", entryMod, got)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("health must not bump SpRev: got %d, want %d",
					got, revBefore)
			}
		})
	}
}

// TestSetCntlrErrEpochWithoutItsSp: the health write reaches the CdcEntry
// records through sp_id -> sp_name -> SpConf (MD2), and an SP it cannot reach
// that way — its name record gone, its SpConf gone, or its name re-used by
// another SP — has no entry the write may rewrite. No op leaves a cntlr record
// in such an SP: the sp drain deletes every cntlr (DrainSpCntlrs) before its
// last step deletes the name record and the SpConf together
// (FinishSpDelete), and only that step frees the name. The write must still
// succeed on such a store: the cntlr is still written, no entry of either SP
// is put, and SpRev is not bumped.
func TestSetCntlrErrEpochWithoutItsSp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *opsEnv)
	}{
		{
			name: "sp name gone",
			setup: func(env *opsEnv) {
				env.delKey(SpNameKey(env.cid, opsSpId))
			},
		},
		{
			name: "sp conf gone",
			setup: func(env *opsEnv) {
				env.delKey(SpConfKey(env.cid, opsSpName))
			},
		},
		{
			// The fixture's name holds an SpConf of cdcOtherSpId, while
			// the fixture's sp_id -> sp_name record keeps naming it.
			name: "name re-used by another sp",
			setup: func(env *opsEnv) {
				conf := opsSpConf()
				conf.SpId = cdcOtherSpId
				mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putCdcEntry(cdcSs1, opsCnA, opsCnB)
			// The other SP's subsystem under the same NQN and its entry,
			// which a rewrite through the re-used name would reach and empty:
			// the other SP has no cntlr records.
			mustPut(t, env.cli,
				SubsystemKey(env.cid, cdcOtherSpId, opsNqn),
				&pb.Subsystem{SsId: cdcOtherSsId, Serial: "s1", Model: "m0"})
			otherKey := CdcEntryKey(
				env.cid, opsShard, cdcOtherSpId, cdcOtherSsId)
			mustPut(t, env.cli, otherKey, &pb.CdcEntry{
				Nqn:            opsNqn,
				NvmeTrConfList: []*pb.NvmeTrConf{opsTrConf(opsCnA)},
			})
			tc.setup(env)
			keys := []string{env.cdcKey(cdcSs1), otherKey}
			modsBefore := env.modRevs(keys...)
			revBefore := env.spRev()
			env.setCntlrHealth(opsCntlrB, 1000, false)
			if got := env.cntlr(opsCntlrB).GetErrEpoch(); got != 1000 {
				t.Errorf("err_epoch: got %d, want 1000", got)
			}
			env.wantCdc(cdcSs1, opsCnA, opsCnB)
			if got := env.modRevs(keys...); !slices.Equal(got, modsBefore) {
				t.Errorf("no CdcEntry may be put: mod_revisions %v -> %v",
					modsBefore, got)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("health must not bump SpRev: got %d, want %d",
					got, revBefore)
			}
		})
	}
}

// TestSetCntlrErrEpochSkipsALostCntlr: a cntlr_id_list member with no Cntlr
// record has no address to list, and the health write of another cntlr does
// not fail on it.
func TestSetCntlrErrEpochSkipsALostCntlr(t *testing.T) {
	env := newOpsEnv(t)
	conf := env.spConf()
	conf.CntlrIdList = []uint64{opsCntlrA, cdcLostCntlr, opsCntlrB}
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB)
	env.setCntlrHealth(opsCntlrB, 1000, false)
	env.wantCdc(cdcSs1, opsCnA)
	env.setCntlrHealth(opsCntlrB, 0, false)
	env.wantCdc(cdcSs1, opsCnA, opsCnB)
}

// TestSetCntlrErrEpochRefusesAMissingSubsystem: the health write that moves a
// cntlr in or out of the rule owes every CdcEntry its new list, so a
// subsystem nqn_list names whose record is missing fails it whole, the cntlr
// unwritten. A write that moves no cntlr reads no subsystem and is not
// stopped by one.
func TestSetCntlrErrEpochRefusesAMissingSubsystem(t *testing.T) {
	env := newOpsEnv(t)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB)
	env.listNqn(cdcSsLost)
	keys := []string{env.cdcKey(cdcSs1), CntlrKey(env.cid, opsSpId, opsCntlrB)}
	modsBefore := env.modRevs(keys...)
	err := SetCntlrErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrB, 1000, false,
	)
	precondition := wantPrecondition(t, err, opSetCntlrErrEpoch)
	if precondition.Reason != "subsystem not found" {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, "subsystem not found")
	}
	if got := env.modRevs(keys...); !slices.Equal(got, modsBefore) {
		t.Errorf("an aborted op must put nothing: mod_revisions %v -> %v",
			modsBefore, got)
	}
	// A primary stays listed whatever its err_epoch, so its health write
	// moves nothing and the missing subsystem does not stop it.
	env.setCntlrHealth(opsCntlrA, 1000, false)
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 1000 {
		t.Errorf("err_epoch: got %d, want 1000", got)
	}
}

// TestSetCntlrErrEpochAtASuppressedLevel: the rule holds at every sp_level.
// The health write finds its SP by sp_id, not through loadSpConfForOp and its
// AR3 refusals, so an SP an operator has taken charge of keeps discovery
// records that follow its cntlrs' health.
func TestSetCntlrErrEpochAtASuppressedLevel(t *testing.T) {
	env := newOpsEnv(t)
	env.setLevel(pb.SpLevel_SP_LEVEL_NO_THINPOOL)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB)
	env.setCntlrHealth(opsCntlrB, 1000, false)
	env.wantCdc(cdcSs1, opsCnA)
	env.setCntlrHealth(opsCntlrB, 0, false)
	env.wantCdc(cdcSs1, opsCnA, opsCnB)
}

// ---------------------------------------------------------------------------
// ReplaceCntlr
// ---------------------------------------------------------------------------

// TestReplaceCntlrCdcEntries pins the rule in ReplaceCntlr's transaction for a
// standby in the middle of cntlr_id_list: it leaves the records when its
// err_epoch is set, and its replacement — healthy and enabled — joins every
// CdcEntry at the end, its own place in cntlr_id_list, not the old one's; a
// subsystem with no CdcEntry is not given one.
func TestReplaceCntlrCdcEntries(t *testing.T) {
	env := newOpsEnv(t)
	env.putCn(opsCnD, 803, 3, opsCnFree)
	cntlrD := env.gainCntlr(opsCnD)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB, opsCnD)
	// cdcSs2 has no CdcEntry; cdcSs3, after it, must still be rewritten by
	// both writes below.
	env.addSubsystem(cdcSs2)
	env.addSubsystem(cdcSs3)
	env.putCdcEntry(cdcSs3, opsCnA, opsCnB, opsCnD)
	env.setCntlrHealth(opsCntlrB, 1000, false)
	env.wantCdc(cdcSs1, opsCnA, opsCnD)
	env.wantCdc(cdcSs3, opsCnA, opsCnD)
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, env.cnCand(opsCnC), env.otherCntlrAddrs(opsCntlrB), false,
		1000+common.DefaultCntlrUnhealthy,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	wantIds := []uint64{opsCntlrA, cntlrD, newId}
	if got := env.spConf().GetCntlrIdList(); !slices.Equal(got, wantIds) {
		t.Errorf("cntlr_id_list: got %v, want %v", got, wantIds)
	}
	env.wantCdc(cdcSs1, opsCnA, opsCnD, opsCnC)
	env.wantCdc(cdcSs3, opsCnA, opsCnD, opsCnC)
	if env.exists(env.cdcKey(cdcSs2)) {
		t.Errorf("a missing CdcEntry must stay missing")
	}
}

// TestReplaceCntlrRefusesAMissingSubsystem: ReplaceCntlr owes every CdcEntry
// its new list, as Failover does, so a subsystem nqn_list names whose record
// is missing fails it whole. It is listed after an entry the replacement
// would rewrite, and the aborted op writes nothing: the entry, the old cntlr,
// the SpConf and both CNs keep their mod_revisions, and SpRev is not bumped.
func TestReplaceCntlrRefusesAMissingSubsystem(t *testing.T) {
	env := newOpsEnv(t)
	env.setCntlrErr(opsCntlrB, 1000)
	env.putCdcEntry(cdcSs1, opsCnA, opsCnB)
	env.listNqn(cdcSsLost)
	keys := []string{
		env.cdcKey(cdcSs1),
		CntlrKey(env.cid, opsSpId, opsCntlrB),
		SpConfKey(env.cid, opsSpName),
		CnConfKey(env.cid, opsCnB),
		CnConfKey(env.cid, opsCnC),
	}
	modsBefore := env.modRevs(keys...)
	revBefore := env.spRev()
	_, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, env.cnCand(opsCnC), env.otherCntlrAddrs(opsCntlrB), false,
		1000+common.DefaultCntlrUnhealthy,
	)
	precondition := wantPrecondition(t, err, opReplaceCntlr)
	if precondition.Reason != "subsystem not found" {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, "subsystem not found")
	}
	if got := env.modRevs(keys...); !slices.Equal(got, modsBefore) {
		t.Errorf("an aborted op must put nothing: mod_revisions %v -> %v",
			modsBefore, got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("an aborted op must not bump SpRev: got %d", got)
	}
}

// TestReplaceCntlrSolePrimaryStaysListed: a primary is listed whatever its
// err_epoch, so the sole primary keeps its place in the records through its
// health write — which puts no entry — and its replacement, a primary too,
// takes that place.
func TestReplaceCntlrSolePrimaryStaysListed(t *testing.T) {
	env := newOpsEnv(t)
	conf := env.spConf()
	conf.CntlrIdList = []uint64{opsCntlrA}
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	env.putCdcEntry(cdcSs1, opsCnA)
	modBefore := env.modRev(env.cdcKey(cdcSs1))
	env.setCntlrHealth(opsCntlrA, 1000, false)
	env.wantCdc(cdcSs1, opsCnA)
	if got := env.modRev(env.cdcKey(cdcSs1)); got != modBefore {
		t.Errorf("the health write of a primary must not put the CdcEntry: "+
			"mod_revision %d -> %d", modBefore, got)
	}
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, env.cnCand(opsCnC), nil, true,
		1000+common.DefaultCntlrUnhealthy,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	if !env.cntlr(newId).GetPrimary() {
		t.Fatalf("the replacement of a sole primary must be primary")
	}
	env.wantCdc(cdcSs1, opsCnC)
}
