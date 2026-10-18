// Package agentfake is the fake dn and cn agent of the integration tests
// (dnv-worker.md, Integration test plan, The fake agent). One Agent
// implements the generated DiskNodeAgent and ControllerNodeAgent services and
// answers as a real agent would: it applies the agents' revision gate per
// object and their ordering rule, refuses a bitmap push for a migration or a
// clone it does not know, derives every *Info row from the last request it
// accepted for the object, reports only changes on a Check stream, and stamps
// every answer with a new observation. Levers a test sets make it misbehave.
//
// Two fronts set the same levers, so one fake cannot drift from itself. The
// behavior file of the fakeagent program, whose format ParseBehavior parses
// and which the program re-reads whenever it changes, drives the shell
// suites; Go calls (SetBehavior, Update, Stop, Start) drive the Go tests,
// which serve a Fleet of agents in-process over in-memory connections keyed
// by addr_port.
//
// Objects are keyed as the behavior file keys them — "dn", "cn",
// "side <sp_id>:<leg_id>:<side_id>", "cntlr <sp_id>:<cntlr_id>", ids in plain
// decimal (DnKey, CnKey, SideKey, CntlrKey) — and every rule and every lever
// is per object.
//
// Every Check* and Get*Info reply, and every Syncup* reply that does not
// refuse its request, carries an observation stamp (dnagent.md SH28) beside
// its *Info, never inside it, so the change-only rule's fingerprint of the
// *Info stays put: the fake observes an object at every answer, so the
// observation's time is the answer's own, made later than the object's
// previous observation, and its trace id is the call's — the round's own for
// a Check* round. Every round is thus a fresh, new observation to the worker
// (dnv-worker.md HL7, RW23), unless a lever ages it or leaves it out. A round
// or an info read that refuses its object as unknown — one its parent does
// not list, or one a forced reply_code refuses so — has no observation: its
// stamp carries only the answer time. A Syncup* that saved the request also
// carries the save time, and its observation comes after it; one that
// refuses the request — a stale revision, an object its parent does not
// list, or a refusing code the reply_code lever forces — carries no stamp and
// no save time, as a live agent's refusal carries neither (architecture.md,
// Live-state reporting). A forced ReplyCodeLeftover is no refusal: it carries
// an observation, and no save time, since the fake saves nothing on a forced
// code. The times are not stored: a restarted fake stamps the time it
// answers, as an agent's startup reconcile stores a new observation.
//
// The stored state — each object's last accepted request and revision plus
// the bitmap chunks it received — lives in memory, and in the state file of
// Config.Dir when one is given, so that a restarted program replies like a
// restarted agent (store.go).
package agentfake

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// DefaultNodeSize is the fakeagent program's GetDnSize/GetCnSize reply
	// unless its --size or the behavior's Size says otherwise (the worker
	// never calls them).
	DefaultNodeSize = 1024 * 1024 * 1024 * 1024

	// DnKey and CnKey are the two singleton object keys; the side and cntlr
	// keys are built by SideKey and CntlrKey.
	DnKey = "dn"
	CnKey = "cn"

	sideObjPrefix  = "side "
	cntlrObjPrefix = "cntlr "

	// The details of the rows the fake derives from the last accepted
	// request rather than defaulting to OK, the texts the real agents write:
	// a migration destination's two rows while it waits for the cutover
	// (dnagent.md DN18), and a path row of a leg with two sides, live for a
	// provisioned side and held for one that is not (cnagent.md CN28). The
	// worker reads none of them: it reads the reason and the status.
	detailsAwaitingCutover  = "waiting for the cutover"
	detailsPathLive         = "live/optimized"
	detailsPathProvisioning = "provisioning"
)

// SideKey renders the "side <sp_id>:<leg_id>:<side_id>" object key
// ("side 1:3:5").
func SideKey(spId, legId, sideId uint64) string {
	return fmt.Sprintf("%s%d:%d:%d", sideObjPrefix, spId, legId, sideId)
}

// CntlrKey renders the "cntlr <sp_id>:<cntlr_id>" object key ("cntlr 1:1").
func CntlrKey(spId, cntlrId uint64) string {
	return fmt.Sprintf("%s%d:%d", cntlrObjPrefix, spId, cntlrId)
}

// sideObjKey is SideKey of a side pointer.
func sideObjKey(ptr *pb.SidePointer) string {
	return SideKey(ptr.GetSpId(), ptr.GetLegId(), ptr.GetSideId())
}

// cntlrObjKey is CntlrKey of a cntlr pointer.
func cntlrObjKey(ptr *pb.CntlrPointer) string {
	return CntlrKey(ptr.GetSpId(), ptr.GetCntlrId())
}

// rowKey renders the "<map_field>.<id>" row key of a map row.
func rowKey(field string, id uint64) string {
	return field + "." + strconv.FormatUint(id, 10)
}

// migrChunkKey renders the stored chunk key of one migration chunk: the
// bm_idx alone, in plain decimal, because a migration bitmap's chunks are
// numbered by their append sequence (architecture.md, Bitmap push protocol).
func migrChunkKey(bmIdx uint32) string {
	return strconv.FormatUint(uint64(bmIdx), 10)
}

// cloneChunkKey renders the stored chunk key of one clone chunk: the
// (src_slice_idx, bm_idx) pair a clone chunk is addressed by
// (architecture.md, Bitmap push protocol), decimal and colon-separated
// ("2:1"). The behavior file's chunk_id_list override spells a chunk the
// same way.
func cloneChunkKey(srcSliceIdx, bmIdx uint32) string {
	return fmt.Sprintf("%d:%d", srcSliceIdx, bmIdx)
}

// parseCloneChunkKey is cloneChunkKey's inverse. A key that is not an "s:b"
// pair — a migration chunk's plain index, or a hand edit's typo — addresses no
// clone chunk and is skipped by its caller.
func parseCloneChunkKey(key string) (uint32, uint32, bool) {
	sliceField, bmField, found := strings.Cut(key, ":")
	if !found {
		return 0, 0, false
	}
	srcSliceIdx, err := strconv.ParseUint(sliceField, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	bmIdx, err := strconv.ParseUint(bmField, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return uint32(srcSliceIdx), uint32(bmIdx), true
}

// sortChunkIdList orders a clone's applied set ascending by (src_slice_idx,
// bm_idx), the order the worker diffs and pushes in (architecture.md, Bitmap
// push protocol).
func sortChunkIdList(chunkIdList []*pb.BmChunkId) {
	slices.SortFunc(chunkIdList, func(a, b *pb.BmChunkId) int {
		if c := cmp.Compare(
			a.GetSrcSliceIdx(), b.GetSrcSliceIdx()); c != 0 {
			return c
		}
		return cmp.Compare(a.GetBmIdx(), b.GetBmIdx())
	})
}

// ---------------------------------------------------------------------------
// The agent
// ---------------------------------------------------------------------------

// Config is what an Agent is built with.
type Config struct {
	// Size is the GetDnSize/GetCnSize reply unless the behavior's Size says
	// otherwise. Zero is replied as zero: the gateway suite starts its cn
	// fakes at size zero.
	Size uint64
	// Dir, when set, holds the state file: the stored state is loaded from
	// it at New and at a Start that keeps it, written on every change, and
	// re-read whenever the file is newer than the agent's own last write, so
	// that a hand edit while the agent runs is seen by the next request.
	// Empty keeps the stored state in memory alone.
	Dir string
	// Refresh, when set, runs at the arrival of every request with no lock
	// of the agent held: the behavior file front reloads its file there, so
	// that a request sees a rewrite made before it.
	Refresh func(ctx context.Context)
}

// epochEntry tracks one row's last reported status so ResInfo.epoch only
// moves when the status actually changes (architecture.md, Live-state
// reporting).
type epochEntry struct {
	status pb.ResStatus
	epoch  uint64
}

// Agent implements both agent services; NewServer registers the ones a
// caller names. Every field behind mu is shared by the concurrent unary
// handlers and Check streams of the agent; every exported method is safe
// for concurrent use with them and with each other.
type Agent struct {
	pb.UnimplementedDiskNodeAgentServer
	pb.UnimplementedControllerNodeAgentServer

	cfg Config

	mu         sync.Mutex
	state      map[string]*objectState
	stateMtime time.Time
	beh        *Behavior
	epochs     map[string]map[string]*epochEntry
	// observed is each object's latest observation (observeLocked).
	observed map[string]*observation
	// heldRounds counts, per object, the Check rounds hang_rounds still
	// holds, and leftRounds the rounds leftover_rounds still answers with
	// the leftover code: each armed from the levers whenever they are
	// written (armLocked) and spent one per round.
	heldRounds map[string]uint32
	leftRounds map[string]uint32
	// changed is closed and replaced whenever a lever changes
	// (notifyLocked): every waiting lever waits on it, so a change wakes
	// it at once and no wait polls.
	changed chan struct{}
	// records and calls are what the agent received (Record, Calls).
	records map[string]*objectRecord
	calls   map[string]int
	// gen numbers the agent's lifetimes: Stop and Start each move it, so a
	// call that arrived in an earlier lifetime acts no more (liveLocked).
	gen uint64

	// serving is the in-process serving of a Fleet member (fleet.go),
	// guarded by smu, which is never taken under mu.
	smu     sync.Mutex
	serving *serving
}

// New builds an agent, loading its stored state from cfg.Dir when one is
// given.
func New(ctx context.Context, cfg Config) (*Agent, error) {
	a := &Agent{
		cfg:        cfg,
		state:      make(map[string]*objectState),
		beh:        &Behavior{},
		epochs:     make(map[string]map[string]*epochEntry),
		observed:   make(map[string]*observation),
		heldRounds: make(map[string]uint32),
		leftRounds: make(map[string]uint32),
		changed:    make(chan struct{}),
		records:    make(map[string]*objectRecord),
		calls:      make(map[string]int),
	}
	if err := a.openStore(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// notifyLocked wakes every waiting lever (waitForRound, holdUnary), each of
// which re-reads the levers.
func (a *Agent) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

// refresh runs Config.Refresh, the front's reload, with no lock held.
func (a *Agent) refresh(ctx context.Context) {
	if a.cfg.Refresh != nil {
		a.cfg.Refresh(ctx)
	}
}

// lifetime is the agent's lifetime now, the one a call that arrives now
// belongs to (liveLocked).
func (a *Agent) lifetime() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen
}

// liveLocked is the test a call passes, with the lock held, before it
// records, applies or answers anything: the call arrived in gen, the
// agent's lifetime then, and acts only while that lifetime lasts and its
// caller still waits. A call that a Stop ended — one the stop found in
// flight, or one whose handler ran only after it — ends unavailable, as a
// killed agent's calls end, having applied nothing; one whose caller gave up
// ends with the caller's error. The stop cancels every call it ends, so a
// call that reads the lifetime of a later Start fails the second test.
func (a *Agent) liveLocked(ctx context.Context, gen uint64) error {
	if a.gen != gen {
		return status.Error(codes.Unavailable, "agentfake: the agent stopped")
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

// callTraceId is the trace id of a unary call, the one a Syncup*'s or an info
// read's observation is stamped with: the server interceptor puts it into
// the ctx, and without the interceptor it is read from the incoming metadata
// (common.TraceIdMetadataKey), so a quiet server stamps it all the same.
func callTraceId(ctx context.Context) string {
	if traceId, ok := common.TraceIdFromCtx(ctx); ok {
		return traceId
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(common.TraceIdMetadataKey); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Records: what the agent received
// ---------------------------------------------------------------------------

// The RPC names Calls counts by, the method names of the two services.
const (
	MethodGetDnSize       = "GetDnSize"
	MethodSyncupDn        = "SyncupDn"
	MethodSyncupSide      = "SyncupSide"
	MethodPushMigrBitmap  = "PushMigrBitmap"
	MethodGetDnInfo       = "GetDnInfo"
	MethodGetSideInfo     = "GetSideInfo"
	MethodCheckDn         = "CheckDn"
	MethodCheckSide       = "CheckSide"
	MethodGetCnSize       = "GetCnSize"
	MethodSyncupCn        = "SyncupCn"
	MethodSyncupCntlr     = "SyncupCntlr"
	MethodPushCloneBitmap = "PushCloneBitmap"
	MethodGetCnInfo       = "GetCnInfo"
	MethodGetCntlrInfo    = "GetCntlrInfo"
	MethodGetThinDeviceBm = "GetThinDeviceBm"
	MethodGetLegBm        = "GetLegBm"
	MethodCheckCn         = "CheckCn"
	MethodCheckCntlr      = "CheckCntlr"
)

// Record is what an agent received about one object, each list in arrival
// order. A request is recorded as it arrives, before any lever holds it or
// any gate refuses it, so a held or a refused request is in it too; one that
// reaches the agent only after a Stop, or after its caller gave up, is not
// (liveLocked). The messages are the received ones and must not be
// modified.
type Record struct {
	// Syncups are the object's Syncup* requests.
	Syncups []proto.Message
	// Rounds are the object's Check* requests, one per round.
	Rounds []proto.Message
	// InfoReads are the object's Get*Info requests.
	InfoReads []proto.Message
	// Pushes are the object's Push*Bitmap requests.
	Pushes []proto.Message
	// Streams counts the Check* streams whose first round named the
	// object, and StreamEnds those of them whose handler has returned: the
	// worker's side of a stream it drops is invisible from here, so this is
	// how a test tells a stream that is gone from one that is idle.
	Streams    int
	StreamEnds int
}

// objectRecord is the stored form of one object's Record.
type objectRecord struct {
	syncups    []proto.Message
	rounds     []proto.Message
	infoReads  []proto.Message
	pushes     []proto.Message
	streams    int
	streamEnds int
}

// Record returns a copy of what the agent received about the object at key.
// Records survive Stop and Start: they are the test's evidence, not the
// agent's state.
func (a *Agent) Record(key string) Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.records[key]
	if rec == nil {
		return Record{}
	}
	return Record{
		Syncups:    slices.Clone(rec.syncups),
		Rounds:     slices.Clone(rec.rounds),
		InfoReads:  slices.Clone(rec.infoReads),
		Pushes:     slices.Clone(rec.pushes),
		Streams:    rec.streams,
		StreamEnds: rec.streamEnds,
	}
}

// Calls is how many calls of one RPC the agent received, by its method name
// (MethodSyncupDn, ...); for a Check* RPC, how many streams were opened.
func (a *Agent) Calls(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[method]
}

// recordLocked returns the object's record, creating it.
func (a *Agent) recordLocked(key string) *objectRecord {
	rec := a.records[key]
	if rec == nil {
		rec = &objectRecord{}
		a.records[key] = rec
	}
	return rec
}

// recvKind says which list of a Record a unary request joins.
type recvKind int

const (
	recvNone recvKind = iota
	recvSyncup
	recvInfoRead
	recvPush
)

// arrive is the arrival of one unary request: the front's reload, the call
// count and the request's record. It returns the agent's lifetime the
// request arrived in, which the handler passes to liveLocked before it acts,
// or the error the call ends with when it is not live.
func (a *Agent) arrive(
	ctx context.Context, method, key string, kind recvKind, req proto.Message,
) (uint64, error) {
	gen := a.lifetime()
	a.refresh(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return 0, err
	}
	a.calls[method]++
	switch kind {
	case recvSyncup:
		rec := a.recordLocked(key)
		rec.syncups = append(rec.syncups, req)
	case recvInfoRead:
		rec := a.recordLocked(key)
		rec.infoReads = append(rec.infoReads, req)
	case recvPush:
		rec := a.recordLocked(key)
		rec.pushes = append(rec.pushes, req)
	}
	return gen, nil
}

// streamTrack is one Check* stream's record keeping: the agent's lifetime
// it opened in, and the object its first round named, empty until then.
type streamTrack struct {
	gen uint64
	key string
}

// openStream counts one Check* stream opened, unless it is not live
// (liveLocked).
func (a *Agent) openStream(
	ctx context.Context, method string,
) (*streamTrack, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	track := &streamTrack{gen: a.gen}
	if err := a.liveLocked(ctx, track.gen); err != nil {
		return nil, err
	}
	a.calls[method]++
	return track, nil
}

// arriveRound is the arrival of one Check* round: the front's reload and the
// round's record, the stream counted for the object of its first round. A
// round of a stream that is not live is not recorded: its error ends the
// stream.
func (a *Agent) arriveRound(
	ctx context.Context, track *streamTrack, key string, req proto.Message,
) error {
	a.refresh(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, track.gen); err != nil {
		return err
	}
	rec := a.recordLocked(key)
	rec.rounds = append(rec.rounds, req)
	if track.key == "" {
		track.key = key
		rec.streams++
	}
	return nil
}

// endStream counts the end of a stream that a round named, a stream a Stop
// ended included.
func (a *Agent) endStream(track *streamTrack) {
	if track.key == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recordLocked(track.key).streamEnds++
}
