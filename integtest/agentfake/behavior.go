package agentfake

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The levers (dnv-worker.md, Integration test plan, The fake agent)
// ---------------------------------------------------------------------------

// Row overrides one row of an object's *Info. Every field is optional: a row
// may set only the status, only the details, only the reason, or any of them.
//
// Reason sets the row's ResReason as given. Without it, a row the fake
// derives with a reason keeps that reason only while the row still reads the
// derived status (resolveDerivedRow), as the real agent sets the reason on
// that status alone.
//
// WhenPrimary gates the override on the cntlr's role: it applies only while
// the cntlr's last accepted request carries cntlr.primary = true and
// cntlr.disabled = false (cnagent.md CN9), so a row
// planted on a standby bites the moment a failover promotes it and not
// before. A row the fake derives on a standby too — a namespace's, a
// subsystem's, a td's dm-error — would otherwise make the standby
// unhealthy, and ineligible, before the failover; one the fake derives on
// a primary alone, as the cn agent builds it (cntlrInfoLocked), bites only
// once the cntlr is primary either way, and so does a leg row, since a
// standby's leg row is logged, never recorded (dnv-worker.md HL2). Only a cntlr object has a role:
// validate refuses it anywhere else.
type Row struct {
	Status      *pb.ResStatus
	Details     *string
	Reason      *pb.ResReason
	WhenPrimary bool
}

// ChunkId is one clone chunk's address, the (src_slice_idx, bm_idx) pair
// (architecture.md, Bitmap push protocol).
type ChunkId struct {
	SrcSliceIdx uint32
	BmIdx       uint32
}

// RepairVerdict is the primary's verdict on one repair that a cntlr's reply
// carries in rep_id_to_repair_info (architecture.md, Repair): Started, the
// new leg holds a place in the group's array, or AbortConfirmed, the abort
// of a repair whose flag is set is confirmed; and Refusal, the text of the
// primary's refusal of the add of a new leg taken from a retired leg that
// holds newer data than the array (cnagent.md CN12), empty when it refuses
// nothing; Held, the new legs md holds a member on in any state
// (held_new_leg_id_list); and Waits, the text of why the primary holds the
// repair back (cnagent.md CN12, Why a repair waits). The fake reports each
// as the lever says, with no check that a real primary would give that
// combination.
type RepairVerdict struct {
	Started        bool
	AbortConfirmed bool
	Refusal        string
	Held           []uint64
	Waits          string
}

// GrpReading overrides a primary's reading of one group's running array in
// grp_id_to_grp_info (architecture.md, Repair): Whole, whether the array is
// whole, InSync, the leg ids md holds in sync, and Acted, the revision of
// the forced-start mark the primary acted on (cnagent.md CN28). A nil field
// keeps the default reading: whole, with every leg of the group's leg_list
// in sync, and the revision of the group's mark in the request, as a
// primary whose forced start acted on it at once.
type GrpReading struct {
	Whole  *bool
	InSync *[]uint64
	Acted  *uint64
	// MdDone overrides the md check request the reading names done.
	MdDone *uint64
}

// DiskCounts are the disk counts a dn reports (dnagent.md DN18), the lever
// of the count compare (dnv-worker.md RW13). They are reported only beside
// an OK meta row, as the dn agent sends them. SideRecCnt defaults to the
// number of side pointers of the last accepted SyncupDn and SideRecsMatch to
// true: the side records name exactly those sides.
type DiskCounts struct {
	TotalExtCnt   uint64
	FreeExtCnt    uint64
	FreeMetaUnits uint64
	SideRecCnt    *uint64
	SideRecsMatch *bool
}

// Object is the levers of one object. The zero value changes nothing: every
// row OK save the rows the fake derives, sides instantly zeroed, every
// answer at once and from a new observation.
type Object struct {
	// Status and Details apply to every row of the object, under the
	// default entry's and over the derived base; Rows overrides single rows
	// by "<row>" or "<map_field>.<id>" key, applied in the order a row's
	// keys are given.
	Status  *pb.ResStatus
	Details *string
	Rows    map[string]Row

	// ZeroBytes and ZeroedBytes override a side's provisioning counters:
	// zero_bytes defaults to the request's side_conf.zero_bytes and
	// zeroed_bytes to zero_bytes (instant provisioning).
	ZeroedBytes *uint64
	ZeroBytes   *uint64

	// ThinOk reports a cntlr's thin rows while it is primary, minus the
	// slices ThinMissingSlices names.
	ThinOk            bool
	ThinMissingSlices []uint64

	// BmIdxList and ChunkIdList force a side's migration and a cntlr's
	// clone applied sets in place of the ones derived from the received
	// chunks; a forced empty list is "nothing applied".
	BmIdxList   *[]uint32
	ChunkIdList *[]ChunkId

	// LegBm is what a cntlr's GetLegBm answers, for every leg: the bitmap
	// the worker reads for a migration (dnv-worker.md BM7). Unset, the
	// answer is an empty bitmap.
	LegBm []byte

	// Hang holds the object's Check rounds and its unary Get* calls while it
	// is set; HangRounds holds the next that many of its rounds, each until
	// its stream ends; HangSyncup holds its Syncup* while it is set, until
	// the caller's deadline; DelayRounds answers each round that late; and
	// DropStream closes its Check stream with an error.
	Hang        bool
	HangRounds  uint32
	HangSyncup  bool
	DelayRounds time.Duration
	DropStream  bool

	// ReplyCode forces agent_reply.code on the object's replies and
	// suppresses the apply of a Syncup* and a push, but where a gate or
	// another lever answers first: Starting answers a round, an info read
	// and a Syncup* ahead of it, though a push answers the forced code
	// first (gatePushLocked); a round or an info read of an object the
	// fake does not hold, and a Syncup* of a side or a cntlr its node's
	// last syncup does not list, answer unknown whatever the lever
	// (knownLocked, sideKnownLocked, cntlrKnownLocked); ObsNone answers
	// code zero, an ObsAge repeat the code of the observation it repeats,
	// and a round under ReadLeftover the code of the read the round before
	// it started, which carries the forced code (readAnswerLocked).
	ReplyCode uint32
	// Starting, when set, answers every round and info read of the object
	// with ReplyCodeStarting, no observation and Starting as the agent's
	// start time, refuses every Syncup* and push with the same code before
	// any other gate, a Syncup* with only its code and its stamp, and fails
	// a bitmap read with the gRPC status Unavailable, as an agent whose
	// startup pass still runs does (dnagent.md SH33). The start stays the
	// agent's once the lever is cleared.
	Starting time.Time
	// SyncupCode forces agent_reply.code on the object's Syncup* replies
	// alone and suppresses their apply; its rounds and info reads answer
	// from what the object last applied, as a real agent whose gate or
	// save refuses a request does (dnv-worker.md HL9).
	SyncupCode uint32

	// ObsAge answers each round and info read from an observation that
	// ended that long before the answer, never from one earlier than the
	// latest observation the fake gave of the object: while the age would
	// reach back past it, the answer repeats that observation whole — its
	// time, trace id, code, revision and rows — and spends no leftover
	// round, as an agent whose reads stall answers from its last one.
	// ObsNone answers them as an agent that holds the object and no
	// observation of it yet (dnagent.md SH28): code zero, revision zero, no
	// *Info and the answer time alone. Neither touches a Syncup* reply,
	// whose converge always makes a new observation.
	ObsAge  time.Duration
	ObsNone bool

	// SyncupRevision is the revision an accepted Syncup* reply carries in
	// place of the stored one; the fake stores the request as it would
	// without the lever. A real agent answers an accepted syncup with the
	// request's revision (architecture.md, Live-state reporting). What this
	// plays is a reply read against a plan newer than the request, one the
	// pool coordinator handed the child while the call was in flight
	// (dnv-worker.md, RW14): a cntlr's loop reads the reply against that
	// plan itself (dnv-worker.md, HL2), and the sides-first hold and the
	// demotion hold read the revision a side's or a demoted cntlr's loop
	// reports applied (dnv-worker.md, RW14, RW22). A dn or cn loop compares
	// only a round's revision with the desired one, so there it changes
	// nothing but the revision a dn loop keeps with a SyncupDn reply's
	// DnInfo, which holds the count compare off until a round's reply
	// carries a DnInfo again (dnv-worker.md, RW13). A value above the
	// request's plays nothing an agent produces: on a side or a cntlr it can
	// release the sides-first hold or the demotion hold early
	// (dnv-worker.md, RW14, RW22).
	SyncupRevision *uint64

	// LeftoverRounds is a leftover that clears: while it lasts, a Syncup*
	// is applied and saved as usual but answered with ReplyCodeLeftover, as
	// is every info read, and each Check round answered with it from a new
	// observation spends one of its rounds; once they are spent the object
	// answers code zero.
	LeftoverRounds uint32

	// ReadLeftover is a leftover that only the object's reads name, as a cn
	// agent's read names an absent array a converge would start, or a
	// forced start a converge would carry through, while the converge's own
	// reply names nothing (cnagent.md CN30). While it is set, the object's
	// Check rounds answer as an agent's rounds do (dnagent.md SH29, SH30):
	// each round answers from the object's latest observation and starts a
	// read, which ends at once, and the next round answers from that read;
	// an accepted Syncup* cancels the read the round before it started and
	// stores its converge's observation, which the next round answers from.
	// A read answers ReplyCodeLeftover while the leftover has re-sends left
	// and code zero once they are spent; each accepted Syncup* that applies
	// its request spends one, and its own reply answers code zero unless
	// another lever forces a code. A forced reply_code wins over it on the
	// reads too. The info reads are left as they are. ReadRows override
	// rows of a read that names the leftover, over every other lever, as a
	// read finds absent what the converge the re-send brings builds.
	ReadLeftover uint32
	ReadRows     map[string]Row

	// DiskCounts, on the dn alone, are the disk counts DnInfo carries.
	DiskCounts *DiskCounts

	// Repairs, on a cntlr alone, are the verdicts its replies carry, by the
	// rep_name of a repair its last accepted request names, keyed in the
	// reply by that repair's rep_id, while the request makes the cntlr the
	// primary; a repair with no entry gets no verdict. Grps, on a cntlr
	// alone, override its reading of a group by grp_id (GrpReading), which
	// the fake reports for every group of its request while it is the
	// primary.
	Repairs map[string]RepairVerdict
	Grps    map[uint64]GrpReading
}

// Behavior is the whole set of levers of one agent: the behavior file's
// content.
type Behavior struct {
	// Size is the GetDnSize/GetCnSize reply, zero for Config.Size.
	Size uint64
	// Default supplies a status and details under every object's own; every
	// other lever is refused on it (validateDefault), since each acts on
	// one object.
	Default *Object
	// Objects are the levers of each object, by object key.
	Objects map[string]*Object
}

// clone deep-copies one object's levers, so that a caller's later edit of
// the value it set cannot reach the agent and a waiter's copy cannot change
// under it.
func (ob *Object) clone() *Object {
	if ob == nil {
		return nil
	}
	out := *ob
	out.Status = clonePtr(ob.Status)
	out.Details = clonePtr(ob.Details)
	out.Rows = cloneRows(ob.Rows)
	out.ReadRows = cloneRows(ob.ReadRows)
	out.ZeroedBytes = clonePtr(ob.ZeroedBytes)
	out.ZeroBytes = clonePtr(ob.ZeroBytes)
	out.ThinMissingSlices = slices.Clone(ob.ThinMissingSlices)
	out.LegBm = slices.Clone(ob.LegBm)
	if ob.BmIdxList != nil {
		list := slices.Clone(*ob.BmIdxList)
		if list == nil {
			list = []uint32{}
		}
		out.BmIdxList = &list
	}
	if ob.ChunkIdList != nil {
		list := slices.Clone(*ob.ChunkIdList)
		if list == nil {
			list = []ChunkId{}
		}
		out.ChunkIdList = &list
	}
	out.SyncupRevision = clonePtr(ob.SyncupRevision)
	if ob.DiskCounts != nil {
		counts := *ob.DiskCounts
		counts.SideRecCnt = clonePtr(ob.DiskCounts.SideRecCnt)
		counts.SideRecsMatch = clonePtr(ob.DiskCounts.SideRecsMatch)
		out.DiskCounts = &counts
	}
	if ob.Repairs != nil {
		out.Repairs = make(map[string]RepairVerdict, len(ob.Repairs))
		for repName, verdict := range ob.Repairs {
			verdict.Held = slices.Clone(verdict.Held)
			out.Repairs[repName] = verdict
		}
	}
	if ob.Grps != nil {
		out.Grps = make(map[uint64]GrpReading, len(ob.Grps))
		for grpId, reading := range ob.Grps {
			reading.Whole = clonePtr(reading.Whole)
			reading.Acted = clonePtr(reading.Acted)
			if reading.InSync != nil {
				list := slices.Clone(*reading.InSync)
				if list == nil {
					list = []uint64{}
				}
				reading.InSync = &list
			}
			out.Grps[grpId] = reading
		}
	}
	return &out
}

// cloneRows deep-copies a map of row overrides.
func cloneRows(rows map[string]Row) map[string]Row {
	if rows == nil {
		return nil
	}
	out := make(map[string]Row, len(rows))
	for key, row := range rows {
		row.Status = clonePtr(row.Status)
		row.Details = clonePtr(row.Details)
		row.Reason = clonePtr(row.Reason)
		out[key] = row
	}
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// clone deep-copies a whole behavior.
func (b *Behavior) clone() *Behavior {
	out := &Behavior{
		Size:    b.Size,
		Default: b.Default.clone(),
		Objects: make(map[string]*Object, len(b.Objects)),
	}
	for key, ob := range b.Objects {
		if ob != nil {
			out.Objects[key] = ob.clone()
		}
	}
	return out
}

// validate refuses levers that could never apply where they are set — a
// misplaced lever would otherwise fail a test far from its cause: a row's
// when_primary anywhere but on a cntlr object, disk counts anywhere but on
// the dn, read rows with no read leftover — and a read leftover beside a
// lever that answers its rounds another way (obs_age_ms, obs_none,
// leftover_rounds).
func (ob *Object) validate(where, key string) error {
	if ob == nil {
		return nil
	}
	cntlr := strings.HasPrefix(key, cntlrObjPrefix)
	for _, rows := range []struct {
		name string
		rows map[string]Row
	}{{"rows", ob.Rows}, {"read_rows", ob.ReadRows}} {
		for _, rowName := range slices.Sorted(maps.Keys(rows.rows)) {
			if rows.rows[rowName].WhenPrimary && !cntlr {
				return fmt.Errorf(
					"%s: %s[%q]: when_primary on a non-cntlr object",
					where, rows.name, rowName)
			}
		}
	}
	if ob.ReadRows != nil && ob.ReadLeftover == 0 {
		return fmt.Errorf("%s: read_rows with no read_leftover", where)
	}
	if ob.ReadLeftover != 0 &&
		(ob.ObsAge != 0 || ob.ObsNone || ob.LeftoverRounds != 0) {
		return fmt.Errorf("%s: read_leftover beside obs_age_ms, obs_none "+
			"or leftover_rounds, which answer its rounds another way", where)
	}
	if ob.DiskCounts != nil && key != DnKey {
		return fmt.Errorf("%s: disk_counts on an object that is not %q",
			where, DnKey)
	}
	if ob.Repairs != nil && !cntlr {
		return fmt.Errorf("%s: repairs on a non-cntlr object", where)
	}
	if ob.Grps != nil && !cntlr {
		return fmt.Errorf("%s: grps on a non-cntlr object", where)
	}
	return nil
}

// defaultLevers are the fields of Object the default entry is read for
// (resolveDerivedRow): a status and details under every object's own.
var defaultLevers = map[string]bool{"Status": true, "Details": true}

// validateDefault refuses on the default entry every field of Object but the
// two it is read for: a walk over the fields, so that a lever added to
// Object later is refused there too, where a list of names would let it
// pass and do nothing.
func (ob *Object) validateDefault() error {
	if ob == nil {
		return nil
	}
	val := reflect.ValueOf(ob).Elem()
	for i := range val.NumField() {
		name := val.Type().Field(i).Name
		if !defaultLevers[name] && !val.Field(i).IsZero() {
			return fmt.Errorf("default: %s acts on one object, not on "+
				"every one; the default entry takes status and details "+
				"alone", name)
		}
	}
	return nil
}

// validate checks a whole behavior.
func (b *Behavior) validate() error {
	if err := b.Default.validateDefault(); err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(b.Objects)) {
		if err := b.Objects[key].validate(
			"objects["+key+"]", key); err != nil {
			return err
		}
	}
	return nil
}

// SetBehavior replaces every lever of the agent with b — nil clears them —
// and wakes every waiting lever. It re-arms the counted levers
// (HangRounds, LeftoverRounds, ReadLeftover) of every object from b, so a rewrite counts
// its rounds afresh and drops the rounds an earlier one had not spent, as a
// reload of the behavior file does.
func (a *Agent) SetBehavior(b *Behavior) error {
	if b == nil {
		b = &Behavior{}
	}
	if err := b.validate(); err != nil {
		return err
	}
	b = b.clone()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.beh = b
	clear(a.heldRounds)
	clear(a.leftRounds)
	clear(a.readLeft)
	clear(a.reads)
	for key, ob := range b.Objects {
		a.armLocked(key)
		if ob != nil && !ob.Starting.IsZero() {
			// As Update: the Starting lever's start becomes the agent's.
			a.started = ob.Starting
		}
	}
	a.notifyLocked()
	return nil
}

// Update edits one object's levers: fn gets a copy of them — zero ones for
// an object with none — and what it leaves is set, which wakes every
// waiting lever. A counted lever (HangRounds, LeftoverRounds, ReadLeftover)
// is re-armed only when fn changed its value, so an edit of another lever leaves the
// rounds a count has left as they are; to count the same number afresh, a
// test sets the lever to zero and back, or sets the whole behavior. fn runs
// with the agent's lock held and must not call the agent.
func (a *Agent) Update(key string, fn func(*Object)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.beh.Objects[key]
	ob := old.clone()
	if ob == nil {
		ob = &Object{}
	}
	fn(ob)
	if err := ob.validate("objects["+key+"]", key); err != nil {
		return err
	}
	if !ob.Starting.IsZero() {
		// One process, one start (dnagent.md SH33): the Starting lever's
		// start is the agent's, and stays so after the lever is cleared.
		a.started = ob.Starting
	}
	if a.beh.Objects == nil {
		a.beh.Objects = make(map[string]*Object)
	}
	a.beh.Objects[key] = ob.clone()
	var was Object
	if old != nil {
		was = *old
	}
	if ob.HangRounds != was.HangRounds {
		a.armHangRoundsLocked(key)
	}
	if ob.LeftoverRounds != was.LeftoverRounds {
		a.armLeftoverRoundsLocked(key)
	}
	if ob.ReadLeftover != was.ReadLeftover {
		a.armReadLeftoverLocked(key)
	}
	a.notifyLocked()
	return nil
}

// Behavior returns a copy of the agent's levers.
func (a *Agent) Behavior() *Behavior {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.beh.clone()
}

// armLocked arms one object's counted levers from its current levers.
func (a *Agent) armLocked(key string) {
	a.armHangRoundsLocked(key)
	a.armLeftoverRoundsLocked(key)
	a.armReadLeftoverLocked(key)
}

// armHangRoundsLocked arms one object's HangRounds count from its current
// levers.
func (a *Agent) armHangRoundsLocked(key string) {
	delete(a.heldRounds, key)
	if ob := a.beh.Objects[key]; ob != nil && ob.HangRounds != 0 {
		a.heldRounds[key] = ob.HangRounds
	}
}

// armLeftoverRoundsLocked arms one object's LeftoverRounds count from its
// current levers.
func (a *Agent) armLeftoverRoundsLocked(key string) {
	delete(a.leftRounds, key)
	if ob := a.beh.Objects[key]; ob != nil && ob.LeftoverRounds != 0 {
		a.leftRounds[key] = ob.LeftoverRounds
	}
}

// armReadLeftoverLocked arms one object's ReadLeftover count from its
// current levers and drops the read its rounds left running, so a re-armed
// leftover starts from the object's latest observation.
func (a *Agent) armReadLeftoverLocked(key string) {
	delete(a.readLeft, key)
	delete(a.reads, key)
	if ob := a.beh.Objects[key]; ob != nil && ob.ReadLeftover != 0 {
		a.readLeft[key] = ob.ReadLeftover
	}
}

// objLocked is the levers of one object, nil when it has none.
func (a *Agent) objLocked(key string) *Object {
	return a.beh.Objects[key]
}

// sizeLocked is the GetDnSize/GetCnSize reply.
func (a *Agent) sizeLocked() uint64 {
	if a.beh.Size != 0 {
		return a.beh.Size
	}
	return a.cfg.Size
}

// ---------------------------------------------------------------------------
// The behavior file's format (dnv-worker.md, Integration test plan, The fake
// agent)
// ---------------------------------------------------------------------------

// rowJSON is one row of the behavior file's "rows" map.
type rowJSON struct {
	Status      *string `json:"status,omitempty"`
	Details     *string `json:"details,omitempty"`
	Reason      *string `json:"reason,omitempty"`
	WhenPrimary bool    `json:"when_primary,omitempty"`
}

// repairVerdictJSON is one entry of the behavior file's "repairs" map.
type repairVerdictJSON struct {
	Started        bool     `json:"started,omitempty"`
	AbortConfirmed bool     `json:"abort_confirmed,omitempty"`
	Refusal        string   `json:"refusal,omitempty"`
	Held           []uint64 `json:"held,omitempty"`
	Waits          string   `json:"waits,omitempty"`
}

// grpReadingJSON is one entry of the behavior file's "grps" map.
type grpReadingJSON struct {
	Whole  *bool     `json:"whole,omitempty"`
	InSync *[]uint64 `json:"in_sync,omitempty"`
	Acted  *uint64   `json:"acted,omitempty"`
	MdDone *uint64   `json:"md_done,omitempty"`
}

// diskCountsJSON is the behavior file's disk_counts.
type diskCountsJSON struct {
	TotalExtCnt   uint64  `json:"total_ext_cnt"`
	FreeExtCnt    uint64  `json:"free_ext_cnt"`
	FreeMetaUnits uint64  `json:"free_meta_units"`
	SideRecCnt    *uint64 `json:"side_rec_cnt,omitempty"`
	SideRecsMatch *bool   `json:"side_recs_match,omitempty"`
}

// objectJSON is one entry of the behavior file's "objects" map, and the
// shape of its "default" entry.
type objectJSON struct {
	Status            *string             `json:"status,omitempty"`
	Details           *string             `json:"details,omitempty"`
	Rows              map[string]*rowJSON `json:"rows,omitempty"`
	ZeroedBytes       *uint64             `json:"zeroed_bytes,omitempty"`
	ZeroBytes         *uint64             `json:"zero_bytes,omitempty"`
	ThinOk            bool                `json:"thin_ok,omitempty"`
	ThinMissingSlices []uint64            `json:"thin_missing_slices,omitempty"`
	BmIdxList         *[]uint32           `json:"bm_idx_list,omitempty"`
	ChunkIdList       *[]string           `json:"chunk_id_list,omitempty"`
	LegBmHex          string              `json:"leg_bm_hex,omitempty"`
	Hang              bool                `json:"hang,omitempty"`
	DropStream        bool                `json:"drop_stream,omitempty"`
	ReplyCode         uint32              `json:"reply_code,omitempty"`
	SyncupCode        uint32              `json:"syncup_code,omitempty"`
	HangSyncup        bool                `json:"hang_syncup,omitempty"`
	HangRounds        uint32              `json:"hang_rounds,omitempty"`
	DelayRoundsMs     uint32              `json:"delay_rounds_ms,omitempty"`
	ObsAgeMs          uint32              `json:"obs_age_ms,omitempty"`
	ObsNone           bool                `json:"obs_none,omitempty"`
	SyncupRevision    *uint64             `json:"syncup_revision,omitempty"`
	LeftoverRounds    uint32              `json:"leftover_rounds,omitempty"`
	ReadLeftover      uint32              `json:"read_leftover,omitempty"`
	ReadRows          map[string]*rowJSON `json:"read_rows,omitempty"`
	DiskCounts        *diskCountsJSON     `json:"disk_counts,omitempty"`
	// Repairs is keyed by rep_name, Grps by grp_id in decimal or 0x hex.
	Repairs map[string]*repairVerdictJSON `json:"repairs,omitempty"`
	Grps    map[string]*grpReadingJSON    `json:"grps,omitempty"`
}

// behaviorJSON is the whole behavior file.
type behaviorJSON struct {
	Size    uint64                 `json:"size,omitempty"`
	Default *objectJSON            `json:"default,omitempty"`
	Objects map[string]*objectJSON `json:"objects,omitempty"`
}

var resStatusNames = map[string]pb.ResStatus{
	"UNKNOWN":      pb.ResStatus_RES_STATUS_UNKNOWN,
	"MISSING":      pb.ResStatus_RES_STATUS_MISSING,
	"ERROR":        pb.ResStatus_RES_STATUS_ERROR,
	"OK":           pb.ResStatus_RES_STATUS_OK,
	"PROVISIONING": pb.ResStatus_RES_STATUS_PROVISIONING,
	"PENDING":      pb.ResStatus_RES_STATUS_PENDING,
}

// parseResStatus accepts both the short spelling ("OK", "ERROR",
// "MISSING", "PROVISIONING", "PENDING", "UNKNOWN") and the full proto enum
// name ("RES_STATUS_ERROR"), case-insensitively.
func parseResStatus(name string) (pb.ResStatus, error) {
	key := strings.ToUpper(strings.TrimSpace(name))
	key = strings.TrimPrefix(key, "RES_STATUS_")
	resStatus, ok := resStatusNames[key]
	if !ok {
		return 0, fmt.Errorf("unknown status %q", name)
	}
	return resStatus, nil
}

// parseResReason accepts any ResReason by its short spelling ("NONE",
// "THIN_ID_LOST", "PARKED", ...) or its full proto enum name
// ("RES_REASON_THIN_ID_LOST"), case-insensitively.
func parseResReason(name string) (pb.ResReason, error) {
	key := strings.ToUpper(strings.TrimSpace(name))
	if !strings.HasPrefix(key, "RES_REASON_") {
		key = "RES_REASON_" + key
	}
	value, ok := pb.ResReason_value[key]
	if !ok {
		return 0, fmt.Errorf("unknown reason %q", name)
	}
	return pb.ResReason(value), nil
}

// convert parses one row's names.
func (r *rowJSON) convert() (Row, error) {
	row := Row{Details: r.Details, WhenPrimary: r.WhenPrimary}
	if r.Status != nil {
		resStatus, err := parseResStatus(*r.Status)
		if err != nil {
			return Row{}, err
		}
		row.Status = &resStatus
	}
	if r.Reason != nil {
		reason, err := parseResReason(*r.Reason)
		if err != nil {
			return Row{}, err
		}
		row.Reason = &reason
	}
	return row, nil
}

// convert parses every status, reason and "s:b" chunk address of one object
// entry. An unparseable one makes the whole file malformed, which the front
// logs and ignores — a silently misspelled status or chunk address would
// otherwise fail a test far from its cause.
func (oj *objectJSON) convert(where string) (*Object, error) {
	ob := &Object{
		Details:           oj.Details,
		ZeroedBytes:       oj.ZeroedBytes,
		ZeroBytes:         oj.ZeroBytes,
		ThinOk:            oj.ThinOk,
		ThinMissingSlices: oj.ThinMissingSlices,
		BmIdxList:         oj.BmIdxList,
		Hang:              oj.Hang,
		HangRounds:        oj.HangRounds,
		HangSyncup:        oj.HangSyncup,
		DelayRounds:       time.Duration(oj.DelayRoundsMs) * time.Millisecond,
		DropStream:        oj.DropStream,
		ReplyCode:         oj.ReplyCode,
		SyncupCode:        oj.SyncupCode,
		ObsAge:            time.Duration(oj.ObsAgeMs) * time.Millisecond,
		ObsNone:           oj.ObsNone,
		SyncupRevision:    oj.SyncupRevision,
		LeftoverRounds:    oj.LeftoverRounds,
		ReadLeftover:      oj.ReadLeftover,
	}
	if oj.Status != nil {
		resStatus, err := parseResStatus(*oj.Status)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		ob.Status = &resStatus
	}
	if oj.LegBmHex != "" {
		bitmap, err := hex.DecodeString(oj.LegBmHex)
		if err != nil {
			return nil, fmt.Errorf("%s: leg_bm_hex: %w", where, err)
		}
		ob.LegBm = bitmap
	}
	if oj.ChunkIdList != nil {
		list := make([]ChunkId, 0, len(*oj.ChunkIdList))
		for _, key := range *oj.ChunkIdList {
			srcSliceIdx, bmIdx, ok := parseCloneChunkKey(key)
			if !ok {
				return nil, fmt.Errorf(
					"%s: chunk_id_list: %q is not a \"<src_slice_idx>:"+
						"<bm_idx>\" pair", where, key)
			}
			list = append(list, ChunkId{SrcSliceIdx: srcSliceIdx, BmIdx: bmIdx})
		}
		ob.ChunkIdList = &list
	}
	var err error
	if ob.Rows, err = convertRows(where, "rows", oj.Rows); err != nil {
		return nil, err
	}
	if ob.ReadRows, err = convertRows(
		where, "read_rows", oj.ReadRows); err != nil {
		return nil, err
	}
	if dc := oj.DiskCounts; dc != nil {
		ob.DiskCounts = &DiskCounts{
			TotalExtCnt:   dc.TotalExtCnt,
			FreeExtCnt:    dc.FreeExtCnt,
			FreeMetaUnits: dc.FreeMetaUnits,
			SideRecCnt:    dc.SideRecCnt,
			SideRecsMatch: dc.SideRecsMatch,
		}
	}
	for repName, rj := range oj.Repairs {
		if rj == nil {
			continue
		}
		if ob.Repairs == nil {
			ob.Repairs = make(map[string]RepairVerdict)
		}
		ob.Repairs[repName] = RepairVerdict{
			Started:        rj.Started,
			AbortConfirmed: rj.AbortConfirmed,
			Refusal:        rj.Refusal,
			Held:           slices.Clone(rj.Held),
			Waits:          rj.Waits,
		}
	}
	for key, gj := range oj.Grps {
		if gj == nil {
			continue
		}
		grpId, err := strconv.ParseUint(strings.TrimSpace(key), 0, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: grps: %q is not a grp_id", where, key)
		}
		if ob.Grps == nil {
			ob.Grps = make(map[uint64]GrpReading)
		}
		ob.Grps[grpId] = GrpReading{
			Whole: gj.Whole, InSync: gj.InSync, Acted: gj.Acted,
			MdDone: gj.MdDone}
	}
	return ob, nil
}

// convertRows parses one map of row overrides, nil when it holds none.
func convertRows(
	where, name string, rows map[string]*rowJSON,
) (map[string]Row, error) {
	var out map[string]Row
	for key, rj := range rows {
		if rj == nil {
			continue
		}
		row, err := rj.convert()
		if err != nil {
			return nil, fmt.Errorf("%s: %s[%q]: %w", where, name, key, err)
		}
		if out == nil {
			out = make(map[string]Row)
		}
		out[key] = row
	}
	return out, nil
}

// ParseBehavior decodes a behavior file. Unknown fields are an error on
// purpose: a typo in a key ("reply-code") must be reported, not silently
// dropped, because the resulting behavior would look like a worker bug. So
// is every lever validate or validateDefault refuses.
func ParseBehavior(data []byte) (*Behavior, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	parsed := &behaviorJSON{}
	if err := dec.Decode(parsed); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the top-level object")
	}
	out := &Behavior{Size: parsed.Size}
	if parsed.Default != nil {
		ob, err := parsed.Default.convert("default")
		if err != nil {
			return nil, err
		}
		out.Default = ob
	}
	for key, oj := range parsed.Objects {
		if oj == nil {
			continue
		}
		ob, err := oj.convert("objects[" + key + "]")
		if err != nil {
			return nil, err
		}
		if out.Objects == nil {
			out.Objects = make(map[string]*Object)
		}
		out.Objects[key] = ob
	}
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}
