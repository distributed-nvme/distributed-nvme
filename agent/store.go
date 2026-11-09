package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Local-store file-name kind prefixes (architecture.md,
// Agent local-store paths). The dn role owns the dn, side, migration chunk
// and dn probe kinds, the cn role the cn, cntlr, clone chunk and cn probe
// kinds (SH6). No kind is a prefix of another, so a listing by kind never
// takes one kind's file for another's.
const (
	StoreKindDn      = "dn-"
	StoreKindSide    = "side-"
	StoreKindMigrBm  = "migr-bm-"
	StoreKindDnProbe = "probe-dn-"
	StoreKindCn      = "cn-"
	StoreKindCntlr   = "cntlr-"
	StoreKindCloneBm = "clone-bm-"
	StoreKindCnProbe = "probe-cn-"
)

// Store is the agent's local store (SH4-SH7): per object, the last
// Syncup*Request saved for it, plus one file per received Push*Bitmap chunk,
// all under the --local-store prefix; Save says when each is saved. Writes go
// through OsClient.WriteProto (atomic replace); reads through ReadProto. It
// keeps two states over every save, SaveFailing and Refusal, and while the
// second holds a node's read saves a probe file through Probe (DN18).
type Store struct {
	oc     common.OsClient
	prefix string
	// mu guards the two states below, so that a Save and a node read's
	// Probe, which can run at once, never tear them.
	mu sync.Mutex
	// failing is whether the latest Save failed (SaveFailing).
	failing bool
	// refused is the latest Save the disk refused, nil once a Save or a
	// Probe succeeds (Refusal); refusals counts every Save the disk
	// refused, so that a Probe that began before one never clears it.
	refused  *storeRefusal
	refusals uint64
}

// storeRefusal is a Save the disk refused: its error's text, and the size of
// what it tried to write, which a Probe writes again.
type storeRefusal struct {
	text string
	size int
}

func NewStore(oc common.OsClient, localStorPrefix string) *Store {
	if localStorPrefix == "" {
		localStorPrefix = common.DefaultLocalStorPrefix
	}
	return &Store{oc: oc, prefix: localStorPrefix}
}

// List enumerates the store with one listing of the prefix in the agent's own
// process (OsClient.ReadDir) and returns, per requested kind prefix, the full
// paths of the matching files in ascending name order (SH6). Any failure of
// that listing — a prefix that is missing or is not a directory, a listing
// that did not answer — is the one fatal startup condition of SH3.
//
// The committed file is the only truth. A name of a requested kind that
// carries common.AtomicWriteTmpInfix is the temp file of a Save or a Probe
// that did not succeed — one whose process died before its rename, for
// instance; the constant's comment lists every way one is left behind — and
// List never returns it, whatever it holds: nothing tells a whole one from a
// half-written one, which can decode with its tail missing, and what it holds
// was never committed (a request whose Save fails is refused, SH5, and a chunk
// whose Save fails is not applied, DN15 and CN22), so the restarted agent
// reports only what was committed and the worker sends again whatever that
// lacks. It sorts right after the file it was meant to replace, so a reload
// that decoded it would let it win — and, left in place, win again at every
// later restart, long after later saves had overtaken it. List removes those
// leftovers before it returns, with Remove's bounded rm. A removal that fails
// is logged and does not fail the listing; the next startup's listing finds
// the files again.
//
// Only the requested kinds are touched: a dn and a cn agent may share one
// prefix, and a temp file of the other role's kinds may be a write in flight.
// One of the requested kinds never is, and only because List belongs to the
// startup reload: it runs before this process saves anything, and two agents
// of one role must not share a prefix (dnagent.md CM2). Called beside a Save
// of a requested kind, it could delete that Save's temp file and fail it.
func (s *Store) List(
	ctx context.Context,
	kinds ...string,
) (map[string][]string, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	names, err := s.oc.ReadDir(cctx, s.prefix)
	if err != nil {
		return nil, fmt.Errorf("local store %s unreadable: %w", s.prefix, err)
	}
	out := make(map[string][]string, len(kinds))
	for _, kind := range kinds {
		out[kind] = nil
	}
	var leftovers []string
	for _, name := range names {
		if name == "" {
			continue
		}
		for _, kind := range kinds {
			if !strings.HasPrefix(name, kind) {
				continue
			}
			path := s.prefix + "/" + name
			if strings.Contains(name, common.AtomicWriteTmpInfix) {
				leftovers = append(leftovers, path)
			} else {
				out[kind] = append(out[kind], path)
			}
			break
		}
	}
	for _, kind := range kinds {
		sort.Strings(out[kind])
	}
	if len(leftovers) > 0 {
		sort.Strings(leftovers)
		for _, path := range leftovers {
			slog.WarnContext(ctx, "removing an interrupted local store write",
				slog.String("path", path))
		}
		if err := s.Remove(ctx, leftovers...); err != nil {
			slog.ErrorContext(ctx,
				"removing interrupted local store writes failed",
				slog.String("error", err.Error()))
		}
	}
	return out, nil
}

// Paths lists the committed files of one kind, sorted. Unlike List it
// removes nothing and runs beside any Save: an interrupted write's temp file
// is skipped and left for the next startup's List.
func (s *Store) Paths(ctx context.Context, kind string) ([]string, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	names, err := s.oc.ReadDir(cctx, s.prefix)
	if err != nil {
		return nil, fmt.Errorf("local store %s unreadable: %w", s.prefix, err)
	}
	var out []string
	for _, name := range names {
		if strings.HasPrefix(name, kind) &&
			!strings.Contains(name, common.AtomicWriteTmpInfix) {
			out = append(out, s.prefix+"/"+name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Load decodes one store file into msg, on a ctx carrying the soft timeout
// like every other call the store makes (SH4, SH15).
func (s *Store) Load(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	return s.oc.ReadProto(cctx, path, msg)
}

// Save persists one store file. When to save is the caller's decision, but a
// save always comes after the gates: a request a gate rejected is never saved
// (SH5; for a chunk, DN15 and CN22). A request, parent (SyncupDn/SyncupCn)
// or object (SyncupSide/SyncupCntlr), is saved as soon as it has passed
// them, before it becomes the desired state and before its converge (SH5); a
// bitmap chunk before it is applied (SH21).
//
// It runs on a ctx carrying the soft timeout (SH4, SH15). The write is
// in-process, so the bound ends only a wait for an OsClient slot. Every
// caller saves under its RPC's locks — the node write lock for a parent
// request, the node read lock and an object lock for an object request or a
// chunk — and without the bound a save that finds every slot held could
// wait for one until the RPC's deadline. A save the bound cuts off fails
// like any other and is logged by its caller. A request whose save failed is
// refused with common.ReplyCodeSaveFailed: it does not become the desired
// state and no converge runs, so the store never holds a request older than
// the last one the agent accepted (SH5). A save whose rename held but whose
// directory fsync failed leaves the refused request in the file, which a
// restart converges and the worker wants anyway. A chunk whose save failed
// is acked at once, and its RPC returns, releasing its locks. Every Save
// updates both states, as SaveFailing and Refusal say.
func (s *Store) Save(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	err := s.oc.WriteProto(cctx, path, msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = err != nil
	switch {
	case err == nil:
		s.refused = nil
	case !cutOff(err):
		s.refused = &storeRefusal{text: err.Error(), size: proto.Size(msg)}
		s.refusals++
	}
	return err
}

// cutOff reports whether a save's error is its ctx's: a cancelled call, or
// the soft timeout's bound on the wait for an OsClient slot. Such a save
// fails on a healthy disk too, so it says nothing of the disk.
func cutOff(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// SaveFailing reports whether the latest Save failed, of a request or of a
// chunk, whichever object it was for; the first Save that succeeds ends it.
// While it holds, a syncup that the gates admit as it arrives marks its own
// object alone and cancels no read (SH30): the worker re-sends every round a
// request the agent refused for a failed save, and each re-send would
// otherwise cancel that round's reads.
func (s *Store) SaveFailing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failing
}

// Refusal reports whether the disk refused a save, and the latest refused
// write's error text, a Save's or a Probe's: a Save that fails with an error
// of the write itself, such as no space left, a read-only file system or an IO
// error, sets it; the next Save that succeeds clears it, and so does a Probe
// that holds; a Save cut off by its ctx, cancelled or past the soft timeout,
// leaves it as it is, since a cut-off save happens on a healthy disk too. It
// is the node's store row (dnagent.md DN18, cnagent.md CN28): the worker marks
// a node whose store refuses writes unhealthy, so it gets no new side or cntlr
// (dnv-worker.md HL1).
func (s *Store) Refusal() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused == nil {
		return "", false
	}
	return s.refused.text, true
}

// ResKeyStore is the tracker key of a node's store row, in both roles.
const ResKeyStore = "store"

// MsgProbeRemoveFailed is the record of a startup whose removal of a probe
// file a probe left failed, in both roles; the next startup tries again.
const MsgProbeRemoveFailed = "removing the local store's probe file failed"

// Row is the node's store row (dnagent.md DN18, cnagent.md CN28), named after
// the store's directory, the --local-store flag: RES_STATUS_ERROR with the
// latest refused write's error while Refusal holds, RES_STATUS_OK otherwise,
// at startup included. The worker sets a node's health epoch from an error row
// and clears it on a clean round (dnv-worker.md HL1), so a node whose store
// refuses writes gets no new side or cntlr; no side or cntlr is marked by it.
func (s *Store) Row(t *ResTracker) *pb.ResInfo {
	if text, refused := s.Refusal(); refused {
		return t.Err(ResKeyStore, s.prefix, text)
	}
	return t.Ok(ResKeyStore, s.prefix, "")
}

// Probe writes the probe file at path, but only while Refusal holds; while it
// is clear it writes nothing and returns nil. A node's background read calls
// it every read (dnagent.md DN18, cnagent.md CN28): a chunk whose save failed
// is acked, and the worker pushes it again only after a later accepted syncup
// of its object (dnagent.md DN15, cnagent.md CN22), which a quiet node does
// not get, so without the probe the store row of a quiet node would read the
// refusal until the next save of anything. The probe makes the write the
// refused save made, a temp file, its fsync, the rename and the directory's
// fsync, under the same bound, of random bytes, at least as many as that save
// tried to write, so that no file system stores it in less room by compressing
// it or sharing its blocks. A probe that holds proves the disk took one such
// write and clears Refusal, unless a Save the disk refused came after it
// began, and then removes the file, so that it keeps none of the room it
// proved; a refused probe keeps Refusal and gives it the probe's error; a
// cut-off one changes nothing. It never touches SaveFailing, which waits for a
// Save of a request or a chunk to hold (SH30): a re-sent request whose save
// fails would otherwise cancel each round's reads, a probe that holds clearing
// the state between two of them.
func (s *Store) Probe(ctx context.Context, path string) error {
	s.mu.Lock()
	refused, refusals := s.refused, s.refusals
	s.mu.Unlock()
	if refused == nil {
		return nil
	}
	payload := make([]byte, refused.size)
	_, _ = rand.Read(payload)
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	err := s.oc.WriteProto(cctx, path, wrapperspb.Bytes(payload))
	s.mu.Lock()
	switch {
	case err == nil:
		if s.refusals == refusals {
			s.refused = nil
		}
	case !cutOff(err) && s.refused != nil:
		s.refused = &storeRefusal{text: err.Error(), size: s.refused.size}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.Remove(ctx, path)
}

// Remove deletes store files (rm -f: a file already gone is no error). No
// store file is kept as a record of what is left to remove: the sweep finds
// an object's resources by name (SH7), so an object's request and chunk files
// go the moment its pointer leaves its parent's list, before any of its
// resources is removed, and a crash in between is nothing worse than a
// startup sweep.
func (s *Store) Remove(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"-f"}, paths...)
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	_, stderr, _, err := s.oc.RunCommand(cctx, "rm", args, "")
	if err != nil {
		return fmt.Errorf("rm %v: %w: %s", paths, err, stderr)
	}
	return nil
}
