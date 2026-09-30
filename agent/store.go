package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// Local-store file-name kind prefixes (architecture.md §4.6). The dn role
// owns the first three, the cn role the last three (SH6).
const (
	StoreKindDn      = "dn-"
	StoreKindSide    = "side-"
	StoreKindMigrBm  = "migr-bm-"
	StoreKindCn      = "cn-"
	StoreKindCntlr   = "cntlr-"
	StoreKindCloneBm = "clone-bm-"
)

// Store is the agent's local store (SH4-SH7): per object, the last
// Syncup*Request saved for it, plus one file per received Push*Bitmap chunk,
// all under the --local-store prefix; Save says when each is saved. Writes go
// through OsClient.WriteProto (atomic replace); reads through ReadProto.
type Store struct {
	oc     common.OsClient
	prefix string
}

func NewStore(oc common.OsClient, localStorPrefix string) *Store {
	if localStorPrefix == "" {
		localStorPrefix = common.DefaultLocalStorPrefix
	}
	return &Store{oc: oc, prefix: localStorPrefix}
}

// Prefix is the directory the store lives in.
func (s *Store) Prefix() string {
	return s.prefix
}

// List enumerates the store and returns, per requested kind prefix, the full
// paths of the matching files in ascending name order (SH6). A failure to
// read the prefix is the one fatal startup condition of SH3.
//
// The committed file is the only truth. A name of a requested kind that
// carries common.AtomicWriteTmpInfix is the temp file of a Save that did not
// succeed — one whose process died before its rename, for instance; the
// constant's comment lists every way one is left behind — and List never
// returns it, whatever it holds: nothing tells a whole one from a
// half-written one, which can decode with its tail missing, and what it holds
// was never committed (a Save that fails is only logged), so the restarted
// agent reports only what was committed and the worker sends again whatever
// that lacks. It sorts right after the file it was meant to replace, so a
// reload that decoded it would let it win — and, left in place, win again at
// every later restart, long after later saves had overtaken it. List removes
// those leftovers before it returns, with Remove's bounded rm. A removal that
// fails is logged and does not fail the listing; the next startup's listing
// finds the files again.
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
	stdout, stderr, _, err := s.oc.RunCommand(
		cctx, "ls", []string{"-1", s.prefix}, "")
	if err != nil {
		return nil, fmt.Errorf(
			"local store %s unreadable: %w: %s", s.prefix, err, stderr)
	}
	out := make(map[string][]string, len(kinds))
	for _, kind := range kinds {
		out[kind] = nil
	}
	var leftovers []string
	for _, line := range strings.Split(stdout, "\n") {
		name := strings.TrimSpace(line)
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

// Load decodes one store file into msg.
func (s *Store) Load(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	return s.oc.ReadProto(ctx, path, msg)
}

// Save persists one store file. When to save is the caller's decision, but a
// save always comes after the gates: a request a gate rejected is never saved
// (SH5; for a chunk, DN15 and CN22). A parent request (SyncupDn/SyncupCn) is
// saved as soon as it has passed them, before its converge; an object request
// (SyncupSide/SyncupCntlr) when SH5 says; a bitmap chunk before it is applied
// (SH21).
func (s *Store) Save(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	return s.oc.WriteProto(ctx, path, msg)
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
