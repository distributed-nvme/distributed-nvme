package agentfake

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// StateFileName is the state file in Config.Dir: the last accepted request
// and revision per object plus the received bitmap chunks (address + byte
// length; a migration chunk is addressed by its bm_idx alone, a clone chunk
// by the (src_slice_idx, bm_idx) pair it sits at: architecture.md, Bitmap
// push protocol). It is written on every change (temp file + rename) and
// loaded at start, so a killed and restarted fake replies like a restarted
// real agent — the last revision and the applied set derived from the
// recorded chunks, a bm_idx_list for a migration and a chunk_id_list for a
// clone. The requests are protojson so the file stays human-editable: a
// script may set a stored revision by hand while the fake runs, and the
// fake picks the edit up on the next request because the file's mtime is
// newer than its own last write.
const StateFileName = "state.json"

// objectState is one object's last accepted request. Chunks maps a
// migration/clone id to a chunk key (migrChunkKey resp. cloneChunkKey) to the
// chunk's byte length, which is all the applied-set report needs
// (architecture.md, Bitmap push protocol).
type objectState struct {
	Revision uint64                       `json:"revision"`
	Request  json.RawMessage              `json:"request,omitempty"`
	Chunks   map[string]map[string]uint64 `json:"chunks,omitempty"`

	// msg is Request unmarshalled; rebuilt on every load and apply.
	msg proto.Message
}

type stateFile struct {
	Objects map[string]*objectState `json:"objects"`
}

// newRequestForKey returns the empty request message an object key stores,
// so a loaded state file can be unmarshalled without a type tag.
func newRequestForKey(key string) proto.Message {
	switch {
	case key == DnKey:
		return &pb.SyncupDnRequest{}
	case key == CnKey:
		return &pb.SyncupCnRequest{}
	case strings.HasPrefix(key, sideObjPrefix):
		return &pb.SyncupSideRequest{}
	case strings.HasPrefix(key, cntlrObjPrefix):
		return &pb.SyncupCntlrRequest{}
	}
	return nil
}

// putChunk records one received Push*Bitmap chunk (its address and byte
// length). chunkKey is the caller's rendered address — migrChunkKey for a
// migration chunk, cloneChunkKey for a clone one — so a chunk that arrives
// again at the same address overwrites its length, which is how a grown chunk
// ([D8], BM5) is recorded.
func (o *objectState) putChunk(resId uint64, chunkKey string, size int) {
	if o.Chunks == nil {
		o.Chunks = make(map[string]map[string]uint64)
	}
	key := strconv.FormatUint(resId, 10)
	if o.Chunks[key] == nil {
		o.Chunks[key] = make(map[string]uint64)
	}
	o.Chunks[key][chunkKey] = uint64(size)
}

// bmIdxList derives the applied index set of one MIGRATION from the recorded
// chunks (architecture.md, Bitmap push protocol: the report is derived from
// the files present, so it survives a restart). A clone's applied set is
// pair-addressed and derived by chunkIdList.
func (o *objectState) bmIdxList(resId uint64) []uint32 {
	if o == nil {
		return nil
	}
	chunks := o.Chunks[strconv.FormatUint(resId, 10)]
	idxList := make([]uint32, 0, len(chunks))
	for key := range chunks {
		idx, err := strconv.ParseUint(key, 10, 32)
		if err != nil {
			continue
		}
		idxList = append(idxList, uint32(idx))
	}
	slices.Sort(idxList)
	return idxList
}

// chunkIdList is bmIdxList's clone twin: the applied set of one CLONE, derived
// from the recorded chunks the same way and for the same reason. A clone chunk
// is addressed by the (src_slice_idx, bm_idx) pair, so two source slices
// sharing a bm_idx are two distinct entries; the result is ascending
// (src_slice_idx, bm_idx) and a key that is no pair is skipped.
func (o *objectState) chunkIdList(resId uint64) []*pb.BmChunkId {
	if o == nil {
		return nil
	}
	chunks := o.Chunks[strconv.FormatUint(resId, 10)]
	chunkIdList := make([]*pb.BmChunkId, 0, len(chunks))
	for key := range chunks {
		srcSliceIdx, bmIdx, ok := parseCloneChunkKey(key)
		if !ok {
			continue
		}
		chunkIdList = append(chunkIdList, &pb.BmChunkId{
			SrcSliceIdx: srcSliceIdx,
			BmIdx:       bmIdx,
		})
	}
	sortChunkIdList(chunkIdList)
	return chunkIdList
}

// openStore prepares Config.Dir and loads the state file in it, if any.
func (a *Agent) openStore(ctx context.Context) error {
	if a.cfg.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(a.cfg.Dir, 0o755); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadStateLocked(ctx)
	return nil
}

// reloadStateLocked re-reads the state file when it is newer than this
// agent's own last write — the hand-edit path. Comparing against the fake's
// own write is what keeps an apply from being mistaken for an operator edit
// (and the operator's edit from being clobbered). It runs at the top of every
// request's locked part, so the edit is seen by the next request.
func (a *Agent) reloadStateLocked(ctx context.Context) {
	if a.cfg.Dir == "" {
		return
	}
	path := filepath.Join(a.cfg.Dir, StateFileName)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if !info.ModTime().After(a.stateMtime) {
		return
	}
	a.stateMtime = info.ModTime()
	data, err := os.ReadFile(path)
	if err != nil {
		slog.ErrorContext(ctx, "state file unreadable",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return
	}
	parsed := &stateFile{}
	if err := json.Unmarshal(data, parsed); err != nil {
		slog.ErrorContext(ctx, "state file malformed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return
	}
	loaded := make(map[string]*objectState, len(parsed.Objects))
	for key, obj := range parsed.Objects {
		if obj == nil {
			continue
		}
		if len(obj.Request) != 0 {
			msg := newRequestForKey(key)
			if msg == nil {
				slog.ErrorContext(ctx, "state file unknown object",
					slog.String("object", key))
				continue
			}
			opts := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := opts.Unmarshal(obj.Request, msg); err != nil {
				slog.ErrorContext(ctx, "state file request malformed",
					slog.String("object", key),
					slog.String("error", err.Error()))
				continue
			}
			obj.msg = msg
		}
		loaded[key] = obj
	}
	a.state = loaded
	slog.InfoContext(ctx, "state file loaded",
		slog.String("path", path),
		slog.Int("object_cnt", len(loaded)))
}

// saveStateLocked writes the state file through a temp file and a rename, so
// the script's concurrent reader never sees a partial file. The resulting
// mtime is remembered as "our own last write" for reloadStateLocked. Without
// a Config.Dir the stored state lives in memory alone and nothing is
// written.
func (a *Agent) saveStateLocked(ctx context.Context) {
	if a.cfg.Dir == "" {
		return
	}
	path := filepath.Join(a.cfg.Dir, StateFileName)
	data, err := json.MarshalIndent(&stateFile{Objects: a.state}, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "state file marshaling failed",
			slog.String("error", err.Error()))
		return
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(a.cfg.Dir, StateFileName+".tmp*")
	if err != nil {
		slog.ErrorContext(ctx, "state file temp failed",
			slog.String("error", err.Error()))
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		slog.ErrorContext(ctx, "state file write failed",
			slog.String("error", err.Error()))
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		slog.ErrorContext(ctx, "state file close failed",
			slog.String("error", err.Error()))
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		slog.ErrorContext(ctx, "state file rename failed",
			slog.String("error", err.Error()))
		return
	}
	if info, err := os.Stat(path); err == nil {
		a.stateMtime = info.ModTime()
	}
}

// restartStoreLocked is the stored state at a restart (Start): kept, it is
// what the agent held — re-read from the state file when there is one, as a
// restarted program loads it — and dropped, it is empty, the state file
// included, as an agent started on empty local files.
func (a *Agent) restartStoreLocked(ctx context.Context, store Store) {
	if store == DropStore {
		a.state = make(map[string]*objectState)
		a.saveStateLocked(ctx)
		return
	}
	a.stateMtime = time.Time{}
	a.reloadStateLocked(ctx)
}

// applyLocked stores one object's request and revision (the revision gate:
// revision >= stored => apply, storing the request and the revision).
func (a *Agent) applyLocked(
	ctx context.Context, key string, revision uint64, req proto.Message,
) {
	obj := a.state[key]
	if obj == nil {
		obj = &objectState{}
		a.state[key] = obj
	}
	obj.Revision = revision
	obj.msg = proto.Clone(req)
	if a.cfg.Dir == "" {
		return
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		slog.ErrorContext(ctx, "request marshaling failed",
			slog.String("object", key),
			slog.String("error", err.Error()))
		return
	}
	obj.Request = raw
}

// pruneLocked drops the child objects that disappeared from their parent's
// pointer list, the way the real agent deletes their local files
// (the Full sync rule of architecture.md, Common agent rules).
func (a *Agent) pruneLocked(prefix string, keep map[string]bool) {
	for key := range a.state {
		if strings.HasPrefix(key, prefix) && !keep[key] {
			delete(a.state, key)
			delete(a.epochs, key)
			delete(a.observed, key)
		}
	}
}

func (a *Agent) revisionLocked(key string) uint64 {
	if obj := a.state[key]; obj != nil {
		return obj.Revision
	}
	return 0
}

func (a *Agent) requestLocked(key string) proto.Message {
	if obj := a.state[key]; obj != nil {
		return obj.msg
	}
	return nil
}
