// Command fakeagent is the fake dn/cn agent pair of the dnv-worker
// integration suite (doc/dnv-worker.md §14.9). One binary with a `dn` and a
// `cn` subcommand, serving the generated DiskNodeAgent resp.
// ControllerNodeAgent service on a plaintext listener behind the real server
// interceptors of doc/grpc.md §4, so `agent.log` carries one `grpc server
// request`/`reply`/`recv`/`send` record per message with the caller's trace
// id — the suite's evidence of what the worker sent and which worker sent it
// (RW10). The JSON log goes to stdout through common's default logger; the
// script redirects it into `agent.log`.
//
// Two files in --dir drive and record the fake:
//
//	behavior.json  what to report. Re-read on every request whose mtime
//	               changed, so the script flips behaviour without restarting
//	               the fake. Absent (or `{}`) means: every row OK and sides
//	               instantly zeroed. A malformed file is logged and ignored,
//	               keeping the previous behaviour, so a bad file fails a test
//	               on its assertion instead of killing the agent.
//	state.json     the last applied request and revision per object plus the
//	               received bitmap chunks (index + byte length). Written on
//	               every apply (temp file + rename) and loaded at start, so a
//	               killed and restarted fake replies like a restarted real
//	               agent — the last revision and a bm_idx_list derived from
//	               the recorded chunks (§14.11 case B step 5). The requests
//	               are protojson so the file stays human-editable: case A
//	               step 3 sets a DN's stored revision by hand while the
//	               process runs, and the fake picks the edit up on the next
//	               request because the file's mtime is newer than its own
//	               last write.
//
// Objects are keyed exactly as behavior.json keys them — "dn", "cn",
// "side <sp_id>:<leg_id>:<side_id>", "cntlr <sp_id>:<cntlr_id>", ids in plain
// decimal — and every rule of §14.9 (the revision gate, the ordering gate,
// the *Info shape, the change-only Check streams) is per object.
//
//	usage: fakeagent dn|cn --grpc-address <ip:port> --dir <dir> [--size N]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// The two files of §14.3's per-agent directory, next to agent.log.
	behaviorFileName = "behavior.json"
	stateFileName    = "state.json"

	// defaultNodeSize is what GetDnSize/GetCnSize reply unless --size or
	// behavior.json's "size" says otherwise (§14.9: "Get*Size replies a
	// configured size"; the worker never calls them).
	defaultNodeSize = 1024 * 1024 * 1024 * 1024

	// The object keys of §14.9. The side and cntlr forms are built by
	// sideObjKey/cntlrObjKey; these are the two singletons.
	dnObjKey = "dn"
	cnObjKey = "cn"

	sideObjPrefix  = "side "
	cntlrObjPrefix = "cntlr "
)

// sideObjKey renders the "side <sp_id>:<leg_id>:<side_id>" key of §14.9 —
// the same string behavior.json uses ("side 1:3:5").
func sideObjKey(ptr *pb.SidePointer) string {
	return fmt.Sprintf("%s%d:%d:%d", sideObjPrefix,
		ptr.GetSpId(), ptr.GetLegId(), ptr.GetSideId())
}

// cntlrObjKey renders the "cntlr <sp_id>:<cntlr_id>" key of §14.9
// ("cntlr 1:1").
func cntlrObjKey(ptr *pb.CntlrPointer) string {
	return fmt.Sprintf("%s%d:%d", cntlrObjPrefix,
		ptr.GetSpId(), ptr.GetCntlrId())
}

// rowKey renders the "<map_field>.<id>" behavior.json row key of a map row.
func rowKey(field string, id uint64) string {
	return field + "." + strconv.FormatUint(id, 10)
}

// ---------------------------------------------------------------------------
// behavior.json (§14.9)
// ---------------------------------------------------------------------------

// rowBehavior overrides one row of an object's *Info. Both fields are
// optional: a row may set only the status, only the details, or both.
type rowBehavior struct {
	Status  *string `json:"status,omitempty"`
	Details *string `json:"details,omitempty"`

	// status is Status parsed once by validate.
	status pb.ResStatus
}

// objectBehavior is one entry of behavior.json's "objects" map (and the
// shape of its "default" entry, of which only status/details are read).
type objectBehavior struct {
	Status            *string                 `json:"status,omitempty"`
	Details           *string                 `json:"details,omitempty"`
	Rows              map[string]*rowBehavior `json:"rows,omitempty"`
	ZeroedExtCnt      *uint64                 `json:"zeroed_ext_cnt,omitempty"`
	TotalExtCnt       *uint64                 `json:"total_ext_cnt,omitempty"`
	ThinOk            bool                    `json:"thin_ok,omitempty"`
	ThinMissingSlices []uint64                `json:"thin_missing_slices,omitempty"`
	BmIdxList         *[]uint32               `json:"bm_idx_list,omitempty"`
	Hang              bool                    `json:"hang,omitempty"`
	DropStream        bool                    `json:"drop_stream,omitempty"`
	ReplyCode         uint32                  `json:"reply_code,omitempty"`

	// status is Status parsed once by validate.
	status pb.ResStatus
}

// behaviorFile is the whole file. "size" is this fake's one addition to the
// §14.9 schema: the GetDnSize/GetCnSize reply, so it too can be changed
// without a restart.
type behaviorFile struct {
	Size    uint64                     `json:"size,omitempty"`
	Default *objectBehavior            `json:"default,omitempty"`
	Objects map[string]*objectBehavior `json:"objects,omitempty"`
}

var resStatusNames = map[string]pb.ResStatus{
	"UNKNOWN":      pb.ResStatus_RES_STATUS_UNKNOWN,
	"MISSING":      pb.ResStatus_RES_STATUS_MISSING,
	"ERROR":        pb.ResStatus_RES_STATUS_ERROR,
	"OK":           pb.ResStatus_RES_STATUS_OK,
	"PROVISIONING": pb.ResStatus_RES_STATUS_PROVISIONING,
}

// parseResStatus accepts both the short spelling of §14.9 ("OK", "ERROR",
// "MISSING", "PROVISIONING", "UNKNOWN") and the full proto enum name
// ("RES_STATUS_ERROR"), case-insensitively.
func parseResStatus(name string) (pb.ResStatus, error) {
	key := strings.ToUpper(strings.TrimSpace(name))
	key = strings.TrimPrefix(key, "RES_STATUS_")
	resStatus, ok := resStatusNames[key]
	if !ok {
		return 0, fmt.Errorf("unknown status %q", name)
	}
	return resStatus, nil
}

// validate parses every status string of one object entry. An unparseable
// status makes the whole file malformed, which the caller logs and ignores —
// a silently misspelled status would otherwise fail a test far from its
// cause.
func (ob *objectBehavior) validate(where string) error {
	if ob == nil {
		return nil
	}
	if ob.Status != nil {
		resStatus, err := parseResStatus(*ob.Status)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		ob.status = resStatus
	}
	for key, row := range ob.Rows {
		if row == nil || row.Status == nil {
			continue
		}
		resStatus, err := parseResStatus(*row.Status)
		if err != nil {
			return fmt.Errorf("%s: rows[%q]: %w", where, key, err)
		}
		row.status = resStatus
	}
	return nil
}

// parseBehavior decodes behavior.json. Unknown fields are an error on
// purpose: a typo in a key ("reply-code") must be reported, not silently
// dropped, because the resulting behaviour would look like a worker bug.
func parseBehavior(data []byte) (*behaviorFile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	parsed := &behaviorFile{}
	if err := dec.Decode(parsed); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the top-level object")
	}
	if err := parsed.Default.validate("default"); err != nil {
		return nil, err
	}
	for key, ob := range parsed.Objects {
		if err := ob.validate("objects[" + key + "]"); err != nil {
			return nil, err
		}
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// state.json (§14.9)
// ---------------------------------------------------------------------------

// objectState is one object's last applied request. Chunks maps a
// migration/clone id to bm_idx to the chunk's byte length, which is all the
// applied-set report of §9.6 needs.
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
// so a loaded state.json can be unmarshalled without a type tag.
func newRequestForKey(key string) proto.Message {
	switch {
	case key == dnObjKey:
		return &pb.SyncupDnRequest{}
	case key == cnObjKey:
		return &pb.SyncupCnRequest{}
	case strings.HasPrefix(key, sideObjPrefix):
		return &pb.SyncupSideRequest{}
	case strings.HasPrefix(key, cntlrObjPrefix):
		return &pb.SyncupCntlrRequest{}
	}
	return nil
}

// putChunk records one received Push*Bitmap chunk (§14.9: index + byte
// length).
func (o *objectState) putChunk(resId uint64, bmIdx uint32, size int) {
	if o.Chunks == nil {
		o.Chunks = make(map[string]map[string]uint64)
	}
	key := strconv.FormatUint(resId, 10)
	if o.Chunks[key] == nil {
		o.Chunks[key] = make(map[string]uint64)
	}
	o.Chunks[key][strconv.FormatUint(uint64(bmIdx), 10)] = uint64(size)
}

// bmIdxList derives the applied index set of one migration/clone from the
// recorded chunks (§9.6: the report is derived from the files present, so it
// survives a restart).
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

// ---------------------------------------------------------------------------
// The agent
// ---------------------------------------------------------------------------

// epochEntry tracks one row's last reported status so ResInfo.epoch only
// moves when the status actually changes (architecture.md §9.5).
type epochEntry struct {
	status pb.ResStatus
	epoch  uint64
}

// fakeAgent implements both agent services; main registers only the one its
// subcommand names. Every field behind mu is shared by the concurrent unary
// handlers and Check streams of one process.
type fakeAgent struct {
	pb.UnimplementedDiskNodeAgentServer
	pb.UnimplementedControllerNodeAgentServer

	dir  string
	size uint64

	mu         sync.Mutex
	state      map[string]*objectState
	stateMtime time.Time
	beh        *behaviorFile
	behMtime   time.Time
	behSize    int64
	behPresent bool
	epochs     map[string]map[string]*epochEntry
}

func newFakeAgent(ctx context.Context, dir string, size uint64) (*fakeAgent, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	agent := &fakeAgent{
		dir:    dir,
		size:   size,
		state:  make(map[string]*objectState),
		beh:    &behaviorFile{},
		epochs: make(map[string]map[string]*epochEntry),
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.refreshLocked(ctx)
	return agent, nil
}

// refreshLocked re-reads both files if they changed. It runs at the top of
// every request handler: behavior.json so the script can flip behaviour
// without a restart, state.json so a hand edit of a stored revision (§14.11
// case A step 3) is seen by the next round.
func (a *fakeAgent) refreshLocked(ctx context.Context) {
	a.reloadBehaviorLocked(ctx)
	a.reloadStateLocked(ctx)
}

// reloadBehaviorLocked implements the §14.9 "re-read on every request when
// its mtime changed" rule. A malformed file is logged once per mtime and
// ignored, keeping the previous behaviour.
func (a *fakeAgent) reloadBehaviorLocked(ctx context.Context) {
	path := filepath.Join(a.dir, behaviorFileName)
	info, err := os.Stat(path)
	if err != nil {
		if a.behPresent {
			slog.InfoContext(ctx, "behavior file gone",
				slog.String("path", path))
			a.beh = &behaviorFile{}
			a.behPresent = false
			a.behMtime = time.Time{}
			a.behSize = 0
		}
		return
	}
	if a.behPresent && info.ModTime().Equal(a.behMtime) &&
		info.Size() == a.behSize {
		return
	}
	a.behPresent = true
	a.behMtime = info.ModTime()
	a.behSize = info.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		slog.ErrorContext(ctx, "behavior file unreadable",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return
	}
	parsed, err := parseBehavior(data)
	if err != nil {
		slog.ErrorContext(ctx, "behavior file malformed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return
	}
	a.beh = parsed
	slog.InfoContext(ctx, "behavior file loaded",
		slog.String("path", path),
		slog.Int("object_cnt", len(parsed.Objects)))
}

// reloadStateLocked re-reads state.json when it is newer than this process's
// own last write — the hand-edit path of §14.11 case A step 3. Comparing
// against the fake's own write is what keeps an apply from being mistaken
// for an operator edit (and the operator's edit from being clobbered).
func (a *fakeAgent) reloadStateLocked(ctx context.Context) {
	path := filepath.Join(a.dir, stateFileName)
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

// saveStateLocked writes state.json through a temp file and a rename, so the
// script's concurrent reader never sees a partial file. The resulting mtime
// is remembered as "our own last write" for reloadStateLocked.
func (a *fakeAgent) saveStateLocked(ctx context.Context) {
	path := filepath.Join(a.dir, stateFileName)
	data, err := json.MarshalIndent(&stateFile{Objects: a.state}, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "state file marshaling failed",
			slog.String("error", err.Error()))
		return
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(a.dir, stateFileName+".tmp*")
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

// applyLocked stores one object's request and revision (§14.9: "revision >=
// stored => apply (store the request and the revision)").
func (a *fakeAgent) applyLocked(
	ctx context.Context, key string, revision uint64, req proto.Message,
) {
	obj := a.state[key]
	if obj == nil {
		obj = &objectState{}
		a.state[key] = obj
	}
	obj.Revision = revision
	obj.msg = proto.Clone(req)
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
// (architecture.md §9.1 "full sync").
func (a *fakeAgent) pruneLocked(prefix string, keep map[string]bool) {
	for key := range a.state {
		if strings.HasPrefix(key, prefix) && !keep[key] {
			delete(a.state, key)
			delete(a.epochs, key)
		}
	}
}

func (a *fakeAgent) revisionLocked(key string) uint64 {
	if obj := a.state[key]; obj != nil {
		return obj.Revision
	}
	return 0
}

func (a *fakeAgent) requestLocked(key string) proto.Message {
	if obj := a.state[key]; obj != nil {
		return obj.msg
	}
	return nil
}

func (a *fakeAgent) objBehaviorLocked(key string) *objectBehavior {
	if a.beh == nil {
		return nil
	}
	return a.beh.Objects[key]
}

func (a *fakeAgent) sizeLocked() uint64 {
	if a.beh != nil && a.beh.Size != 0 {
		return a.beh.Size
	}
	return a.size
}

// ---------------------------------------------------------------------------
// Row resolution (§14.9 "rows", architecture.md §9.5)
// ---------------------------------------------------------------------------

// resolveRow builds one ResInfo. The status and details come from
// behavior.json's "default" entry, then the object's own status/details, then
// its "rows" overrides, applied in the order the keys are given (the last key
// is the most specific and becomes res_name). epoch is the unix second of the
// last status change, so it only moves when the status actually moves.
func (a *fakeAgent) resolveRow(objKey string, keys ...string) *pb.ResInfo {
	resStatus := pb.ResStatus_RES_STATUS_OK
	details := ""
	if a.beh != nil && a.beh.Default != nil {
		if a.beh.Default.Status != nil {
			resStatus = a.beh.Default.status
		}
		if a.beh.Default.Details != nil {
			details = *a.beh.Default.Details
		}
	}
	if ob := a.objBehaviorLocked(objKey); ob != nil {
		if ob.Status != nil {
			resStatus = ob.status
		}
		if ob.Details != nil {
			details = *ob.Details
		}
		for _, key := range keys {
			row := ob.Rows[key]
			if row == nil {
				continue
			}
			if row.Status != nil {
				resStatus = row.status
			}
			if row.Details != nil {
				details = *row.Details
			}
		}
	}
	resName := keys[len(keys)-1]
	return &pb.ResInfo{
		ResName: resName,
		Status:  resStatus,
		Details: details,
		Epoch:   a.epochLocked(objKey, resName, resStatus),
	}
}

// epochLocked returns the unix seconds of resName's last status change.
func (a *fakeAgent) epochLocked(
	objKey, resName string, resStatus pb.ResStatus,
) uint64 {
	rows := a.epochs[objKey]
	if rows == nil {
		rows = make(map[string]*epochEntry)
		a.epochs[objKey] = rows
	}
	entry := rows[resName]
	if entry == nil || entry.status != resStatus {
		entry = &epochEntry{
			status: resStatus,
			epoch:  uint64(time.Now().Unix()),
		}
		rows[resName] = entry
	}
	return entry.epoch
}

// ---------------------------------------------------------------------------
// *Info derivation from the last applied request (§14.9)
// ---------------------------------------------------------------------------

// dnInfoLocked reports the three DnInfo rows; nil until a SyncupDn has been
// applied, so the worker's revision check re-issues the syncup.
func (a *fakeAgent) dnInfoLocked() *pb.DnInfo {
	if a.requestLocked(dnObjKey) == nil {
		return nil
	}
	return &pb.DnInfo{
		DiskInfo: a.resolveRow(dnObjKey, "disk_info"),
		MetaInfo: a.resolveRow(dnObjKey, "meta_info"),
		PortInfo: a.resolveRow(dnObjKey, "port_info"),
	}
}

// cnInfoLocked reports the four CnInfo rows (architecture.md §3.2: port,
// tmpfs, the sparse arena file and its loop device).
func (a *fakeAgent) cnInfoLocked() *pb.CnInfo {
	if a.requestLocked(cnObjKey) == nil {
		return nil
	}
	return &pb.CnInfo{
		PortInfo:    a.resolveRow(cnObjKey, "port_info"),
		TmpfsInfo:   a.resolveRow(cnObjKey, "tmpfs_info"),
		TmpFileInfo: a.resolveRow(cnObjKey, "tmp_file_info"),
		LoopDevInfo: a.resolveRow(cnObjKey, "loop_dev_info"),
	}
}

// sideInfoLocked derives SideInfo from the side's last applied request: one
// row per per-CN export stack (primary_cn_id when non-zero plus every
// standby_id_list entry), the migration roles only when the request carried
// their conf, and the §9.4 provisioning counters — total = ext_cnt and
// zeroed = total by default (instant provisioning), either overridable per
// object in behavior.json.
func (a *fakeAgent) sideInfoLocked(key string) *pb.SideInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupSideRequest)
	if req == nil {
		return nil
	}
	conf := req.GetSideConf()
	info := &pb.SideInfo{
		SideDevInfo:    a.resolveRow(key, "side_dev_info"),
		CnIdToDmError:  make(map[uint64]*pb.ResInfo),
		CnIdToDmLinear: make(map[uint64]*pb.ResInfo),
		CnIdToNvmeof:   make(map[uint64]*pb.ResInfo),
	}
	cnIdList := make([]uint64, 0, len(conf.GetStandbyIdList())+1)
	if conf.GetPrimaryCnId() != 0 {
		cnIdList = append(cnIdList, conf.GetPrimaryCnId())
	}
	cnIdList = append(cnIdList, conf.GetStandbyIdList()...)
	for _, cnId := range cnIdList {
		info.CnIdToDmError[cnId] = a.resolveRow(
			key, rowKey("cn_id_to_dm_error", cnId))
		info.CnIdToDmLinear[cnId] = a.resolveRow(
			key, rowKey("cn_id_to_dm_linear", cnId))
		info.CnIdToNvmeof[cnId] = a.resolveRow(
			key, rowKey("cn_id_to_nvmeof", cnId))
	}
	if req.GetMigrSrcConf() != nil {
		info.MigrSrcInfo = &pb.SideInfo_MigrSrcInfo{
			DmLinearInfo: a.resolveRow(key, "migr_src_info.dm_linear_info"),
			NvmeofInfo:   a.resolveRow(key, "migr_src_info.nvmeof_info"),
		}
	}
	if req.GetMigrDstConf() != nil {
		info.MigrDstInfo = &pb.SideInfo_MigrDstInfo{
			TargetInfo:  a.resolveRow(key, "migr_dst_info.target_info"),
			DmCloneInfo: a.resolveRow(key, "migr_dst_info.dm_clone_info"),
		}
	}
	total := conf.GetExtCnt()
	ob := a.objBehaviorLocked(key)
	if ob != nil && ob.TotalExtCnt != nil {
		total = *ob.TotalExtCnt
	}
	zeroed := total
	if ob != nil && ob.ZeroedExtCnt != nil {
		zeroed = *ob.ZeroedExtCnt
	}
	info.TotalExtCnt = total
	info.ZeroedExtCnt = zeroed
	return info
}

// sideBmInfoLocked reports the applied migration-bitmap indexes of the side's
// destination role (§9.6: BitmapInfo.res_id = migr_id, the set derived from
// the recorded chunks unless behavior.json overrides it).
func (a *fakeAgent) sideBmInfoLocked(key string) *pb.BitmapInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupSideRequest)
	if req == nil || req.GetMigrDstConf() == nil {
		return nil
	}
	migrId := req.GetMigrDstConf().GetMigrId()
	return &pb.BitmapInfo{
		ResId:     migrId,
		BmIdxList: a.bmIdxListLocked(key, migrId),
	}
}

// bmIdxListLocked derives one applied index set, honouring the behavior
// file's bm_idx_list override.
func (a *fakeAgent) bmIdxListLocked(key string, resId uint64) []uint32 {
	if ob := a.objBehaviorLocked(key); ob != nil && ob.BmIdxList != nil {
		return slices.Clone(*ob.BmIdxList)
	}
	return a.state[key].bmIdxList(resId)
}

// sliceIdListLocked returns the sorted slice ids of a cntlr's last request.
// id_to_slice is keyed by common.IdKeyFmt ("%016x").
func sliceIdList(req *pb.SyncupCntlrRequest) []uint64 {
	idList := make([]uint64, 0, len(req.GetIdToSlice()))
	for key := range req.GetIdToSlice() {
		sliceId, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			continue
		}
		idList = append(idList, sliceId)
	}
	slices.Sort(idList)
	return idList
}

// cntlrInfoLocked derives CntlrInfo from the cntlr's last applied request:
// one row per object it named — per slice, per group (meta and data), per leg
// AND per spare leg of every group, per td, per subsystem, per namespace, per
// clone and per xfer. td_id_to_thin_info is filled only when the request's
// cntlr.primary is true and behavior.json's thin_ok says so (architecture.md
// §10.3, cnagent CN14 "primary only"), minus any thin_missing_slices — the
// created-flip negative of §14.11 case S step 6.
func (a *fakeAgent) cntlrInfoLocked(key string) *pb.CntlrInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupCntlrRequest)
	if req == nil {
		return nil
	}
	info := &pb.CntlrInfo{
		SsIdToSubsystem:   make(map[uint64]*pb.ResInfo),
		NsIdToNamespace:   make(map[uint64]*pb.ResInfo),
		NsIdToDmLinear:    make(map[uint64]*pb.ResInfo),
		TdIdToRaid0:       make(map[uint64]*pb.ResInfo),
		TdIdToDmError:     make(map[uint64]*pb.ResInfo),
		TdIdToThinInfo:    make(map[uint64]*pb.CntlrInfo_ThinInfo),
		SliceIdToDmPool:   make(map[uint64]*pb.ResInfo),
		SliceIdToMeta:     make(map[uint64]*pb.ResInfo),
		SliceIdToData:     make(map[uint64]*pb.ResInfo),
		GrpIdToMdRaid:     make(map[uint64]*pb.ResInfo),
		LegIdToLeg:        make(map[uint64]*pb.ResInfo),
		XferIdToDmLinear:  make(map[uint64]*pb.ResInfo),
		XferIdToSubsystem: make(map[uint64]*pb.ResInfo),
		XferIdToNamespace: make(map[uint64]*pb.ResInfo),
		CloneIdToTarget:   make(map[uint64]*pb.ResInfo),
		CloneIdToDmClone:  make(map[uint64]*pb.ResInfo),
		CloneIdToMeta:     make(map[uint64]*pb.ResInfo),
	}
	sliceIdList := sliceIdList(req)
	for _, sliceId := range sliceIdList {
		info.SliceIdToDmPool[sliceId] = a.resolveRow(
			key, rowKey("slice_id_to_dm_pool", sliceId))
		info.SliceIdToMeta[sliceId] = a.resolveRow(
			key, rowKey("slice_id_to_meta", sliceId))
		info.SliceIdToData[sliceId] = a.resolveRow(
			key, rowKey("slice_id_to_data", sliceId))
	}
	for _, slice := range req.GetIdToSlice() {
		grpList := make([]*pb.Group, 0,
			len(slice.GetMetaGrpList())+len(slice.GetDataGrpList()))
		grpList = append(grpList, slice.GetMetaGrpList()...)
		grpList = append(grpList, slice.GetDataGrpList()...)
		for _, grp := range grpList {
			info.GrpIdToMdRaid[grp.GetGrpId()] = a.resolveRow(
				key, rowKey("grp_id_to_md_raid", grp.GetGrpId()))
			legList := make([]*pb.Leg, 0,
				len(grp.GetLegList())+len(grp.GetSpareLegList()))
			legList = append(legList, grp.GetLegList()...)
			legList = append(legList, grp.GetSpareLegList()...)
			for _, leg := range legList {
				info.LegIdToLeg[leg.GetLegId()] = a.resolveRow(
					key, rowKey("leg_id_to_leg", leg.GetLegId()))
			}
		}
	}
	thinOk := false
	var thinMissing map[uint64]bool
	if ob := a.objBehaviorLocked(key); ob != nil {
		thinOk = ob.ThinOk
		thinMissing = make(map[uint64]bool, len(ob.ThinMissingSlices))
		for _, sliceId := range ob.ThinMissingSlices {
			thinMissing[sliceId] = true
		}
	}
	for _, td := range req.GetTdList() {
		tdId := td.GetTdId()
		info.TdIdToRaid0[tdId] = a.resolveRow(
			key, rowKey("td_id_to_raid0", tdId))
		info.TdIdToDmError[tdId] = a.resolveRow(
			key, rowKey("td_id_to_dm_error", tdId))
		if !req.GetCntlr().GetPrimary() || !thinOk {
			continue
		}
		thinKey := rowKey("td_id_to_thin_info", tdId)
		thin := &pb.CntlrInfo_ThinInfo{
			SliceIdToDmThin: make(map[uint64]*pb.ResInfo),
		}
		for _, sliceId := range sliceIdList {
			if thinMissing[sliceId] {
				continue
			}
			thin.SliceIdToDmThin[sliceId] = a.resolveRow(key, thinKey,
				thinKey+"."+strconv.FormatUint(sliceId, 10))
		}
		info.TdIdToThinInfo[tdId] = thin
	}
	for _, subsystem := range req.GetNqnToSubsystem() {
		info.SsIdToSubsystem[subsystem.GetSsId()] = a.resolveRow(
			key, rowKey("ss_id_to_subsystem", subsystem.GetSsId()))
		for _, ns := range subsystem.GetNsList() {
			info.NsIdToNamespace[ns.GetNsId()] = a.resolveRow(
				key, rowKey("ns_id_to_namespace", ns.GetNsId()))
			info.NsIdToDmLinear[ns.GetNsId()] = a.resolveRow(
				key, rowKey("ns_id_to_dm_linear", ns.GetNsId()))
		}
	}
	for _, clone := range req.GetCloneList() {
		cloneId := clone.GetCloneId()
		info.CloneIdToTarget[cloneId] = a.resolveRow(
			key, rowKey("clone_id_to_target", cloneId))
		info.CloneIdToDmClone[cloneId] = a.resolveRow(
			key, rowKey("clone_id_to_dm_clone", cloneId))
		info.CloneIdToMeta[cloneId] = a.resolveRow(
			key, rowKey("clone_id_to_meta", cloneId))
	}
	for _, xfer := range req.GetXferList() {
		xferId := xfer.GetXferId()
		info.XferIdToDmLinear[xferId] = a.resolveRow(
			key, rowKey("xfer_id_to_dm_linear", xferId))
		info.XferIdToSubsystem[xferId] = a.resolveRow(
			key, rowKey("xfer_id_to_subsystem", xferId))
		info.XferIdToNamespace[xferId] = a.resolveRow(
			key, rowKey("xfer_id_to_namespace", xferId))
	}
	return info
}

// cntlrBmInfoListLocked reports one BitmapInfo per clone of the last request
// (§9.6: SyncupCntlrReply.bm_info_list, res_id = clone_id).
func (a *fakeAgent) cntlrBmInfoListLocked(key string) []*pb.BitmapInfo {
	req, _ := a.requestLocked(key).(*pb.SyncupCntlrRequest)
	if req == nil {
		return nil
	}
	bmInfoList := make([]*pb.BitmapInfo, 0, len(req.GetCloneList()))
	for _, clone := range req.GetCloneList() {
		bmInfoList = append(bmInfoList, &pb.BitmapInfo{
			ResId:     clone.GetCloneId(),
			BmIdxList: a.bmIdxListLocked(key, clone.GetCloneId()),
		})
	}
	if len(bmInfoList) == 0 {
		return nil
	}
	return bmInfoList
}

// ---------------------------------------------------------------------------
// The gates (§14.9)
// ---------------------------------------------------------------------------

func agentReply(code uint32, details string) *pb.AgentReply {
	return &pb.AgentReply{Code: code, Details: details}
}

// forcedCodeLocked applies behavior.json's reply_code, which forces
// agent_reply.code on every reply of an object. A forced non-zero code also
// suppresses the apply: an agent that answers "rejected" must not have
// stored the request, or §14.11 case C step 6's "clear ⇒ the push succeeds"
// would have nothing left to push.
func (a *fakeAgent) forcedCodeLocked(key string) uint32 {
	if ob := a.objBehaviorLocked(key); ob != nil {
		return ob.ReplyCode
	}
	return 0
}

// gateSyncupLocked is the §14.9 revision gate: a Syncup* with a revision
// lower than the stored one is ReplyCodeStaleRevision, anything else applies.
func (a *fakeAgent) gateSyncupLocked(
	key string, revision uint64,
) (uint32, string) {
	if code := a.forcedCodeLocked(key); code != 0 {
		return code, "behavior.json reply_code"
	}
	if obj := a.state[key]; obj != nil && revision < obj.Revision {
		return common.ReplyCodeStaleRevision, fmt.Sprintf(
			"stale revision %d < stored %d", revision, obj.Revision)
	}
	return 0, ""
}

// gatePushLocked is the §14.9 gate of the Push* RPCs: a lower revision is
// ReplyCodeStaleRevision, and a migr_id/clone_id absent from the object's
// last applied request is ReplyCodeUnknownObject.
func (a *fakeAgent) gatePushLocked(
	key string, revision uint64, resId uint64, known bool,
) (uint32, string) {
	if code := a.forcedCodeLocked(key); code != 0 {
		return code, "behavior.json reply_code"
	}
	if obj := a.state[key]; obj != nil && revision < obj.Revision {
		return common.ReplyCodeStaleRevision, fmt.Sprintf(
			"stale revision %d < stored %d", revision, obj.Revision)
	}
	if !known {
		return common.ReplyCodeUnknownObject, fmt.Sprintf(
			"id %d is not in %s's last request", resId, key)
	}
	return 0, ""
}

// sideKnownLocked implements the ordering rule of §14.9: a side pointer
// absent from the DN's last SyncupDn.side_pointer_list is unknown, so its
// SyncupSide/CheckSide/PushMigrBitmap is refused with
// ReplyCodeUnknownObject. The worker's independent dn and sp roles must
// survive exactly this.
func (a *fakeAgent) sideKnownLocked(ptr *pb.SidePointer) bool {
	req, _ := a.requestLocked(dnObjKey).(*pb.SyncupDnRequest)
	if req == nil {
		return false
	}
	want := sideObjKey(ptr)
	for _, known := range req.GetSidePointerList() {
		if sideObjKey(known) == want {
			return true
		}
	}
	return false
}

// cntlrKnownLocked is sideKnownLocked's cn twin: a cntlr absent from the CN's
// last SyncupCn.cntlr_pointer_list is unknown.
func (a *fakeAgent) cntlrKnownLocked(ptr *pb.CntlrPointer) bool {
	req, _ := a.requestLocked(cnObjKey).(*pb.SyncupCnRequest)
	if req == nil {
		return false
	}
	want := cntlrObjKey(ptr)
	for _, known := range req.GetCntlrPointerList() {
		if cntlrObjKey(known) == want {
			return true
		}
	}
	return false
}

// migrKnownLocked reports whether a migr_id appears in the side's last
// applied request, in either the source or the destination role.
func (a *fakeAgent) migrKnownLocked(key string, migrId uint64) bool {
	req, _ := a.requestLocked(key).(*pb.SyncupSideRequest)
	if req == nil {
		return false
	}
	if src := req.GetMigrSrcConf(); src != nil && src.GetMigrId() == migrId {
		return true
	}
	if dst := req.GetMigrDstConf(); dst != nil && dst.GetMigrId() == migrId {
		return true
	}
	return false
}

// cloneKnownLocked reports whether a clone_id appears in the cntlr's last
// applied request.
func (a *fakeAgent) cloneKnownLocked(key string, cloneId uint64) bool {
	req, _ := a.requestLocked(key).(*pb.SyncupCntlrRequest)
	if req == nil {
		return false
	}
	for _, clone := range req.GetCloneList() {
		if clone.GetCloneId() == cloneId {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The Check* streams (§14.9, architecture.md §9.7)
// ---------------------------------------------------------------------------

// checkResult is what one Check* round decides under the lock, before the
// handler sends its single reply.
type checkResult struct {
	code     uint32
	details  string
	revision uint64
}

// checkGateLocked resolves a Check* round for one object: the gate's code and
// the agent's last fully applied revision (architecture.md §9.7 — a reply
// whose revision differs from the request's is what makes the worker re-issue
// the object's Syncup*).
func (a *fakeAgent) checkGateLocked(key string, known bool) checkResult {
	if !known {
		return checkResult{
			code:    common.ReplyCodeUnknownObject,
			details: key + " is not in its parent's last pointer list",
		}
	}
	result := checkResult{revision: a.revisionLocked(key)}
	if code := a.forcedCodeLocked(key); code != 0 {
		result.code = code
		result.details = "behavior.json reply_code"
	}
	return result
}

// infoTracker implements the change-only delivery of architecture.md §9.7:
// show_info = true always sends the *Info, show_info = false sends it only
// when it changed since the previous reply ON THIS STREAM, and the first
// reply on a fresh stream always carries it.
type infoTracker struct {
	sent bool
	last []byte
}

func (t *infoTracker) include(showInfo bool, info proto.Message) bool {
	var fingerprint []byte
	if info != nil && info.ProtoReflect().IsValid() {
		fingerprint, _ = proto.MarshalOptions{Deterministic: true}.
			Marshal(info)
	}
	if !t.sent || showInfo || !bytes.Equal(fingerprint, t.last) {
		t.sent = true
		t.last = fingerprint
		return true
	}
	return false
}

// hangPollInterval is how often a hanging round re-reads behavior.json.
const hangPollInterval = 200 * time.Millisecond

// waitForRound applies the two stream levers of §14.9 between a Check*
// request and its reply, and reports whether the stream must be dropped.
//
// hang holds this object's round — and only this object's, the lock is never
// held while waiting — for as long as behavior.json says so, so the worker
// misses the reply and times the round out. It returns as soon as the
// stream's context is cancelled (the worker closes the stream on that
// timeout), so no goroutine is leaked per round, and it also returns once the
// script clears the lever, so a fake whose stream the worker happens to keep
// open cannot stay wedged.
func (a *fakeAgent) waitForRound(
	ctx context.Context, key string,
) (bool, error) {
	for {
		a.mu.Lock()
		a.refreshLocked(ctx)
		hang, drop := false, false
		if ob := a.objBehaviorLocked(key); ob != nil {
			hang, drop = ob.Hang, ob.DropStream
		}
		a.mu.Unlock()
		if drop {
			return true, nil
		}
		if !hang {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(hangPollInterval):
		}
	}
}

// dropStreamError is what drop_stream closes a Check* stream with.
func dropStreamError() error {
	return status.Error(codes.Unavailable, "behavior.json drop_stream")
}

// ---------------------------------------------------------------------------
// service DiskNodeAgent
// ---------------------------------------------------------------------------

// GetDnSize replies the configured size (§14.9; the worker never calls it).
func (a *fakeAgent) GetDnSize(
	ctx context.Context, req *pb.GetDnSizeRequest,
) (*pb.GetDnSizeReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)
	return &pb.GetDnSizeReply{Size: a.sizeLocked()}, nil
}

// SyncupDn implements the §14.9 revision gate for the "dn" object and the
// §9.1 full-sync rule: side pointers that disappear from the list are
// dropped, as the real agent deletes their local files.
func (a *fakeAgent) SyncupDn(
	ctx context.Context, req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	code, details := a.gateSyncupLocked(dnObjKey, req.GetRevision())
	if code == 0 {
		a.applyLocked(ctx, dnObjKey, req.GetRevision(), req)
		keep := make(map[string]bool, len(req.GetSidePointerList()))
		for _, ptr := range req.GetSidePointerList() {
			keep[sideObjKey(ptr)] = true
		}
		a.pruneLocked(sideObjPrefix, keep)
		a.saveStateLocked(ctx)
	}
	return &pb.SyncupDnReply{
		AgentReply: agentReply(code, details),
		Revision:   a.revisionLocked(dnObjKey),
		DnInfo:     a.dnInfoLocked(),
	}, nil
}

// SyncupSide implements the §14.9 ordering gate (a side pointer absent from
// the last SyncupDn is ReplyCodeUnknownObject) followed by the revision gate.
func (a *fakeAgent) SyncupSide(
	ctx context.Context, req *pb.SyncupSideRequest,
) (*pb.SyncupSideReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	key := sideObjKey(req.GetSidePointer())
	if !a.sideKnownLocked(req.GetSidePointer()) {
		return &pb.SyncupSideReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupDn.side_pointer_list"),
		}, nil
	}
	code, details := a.gateSyncupLocked(key, req.GetRevision())
	if code == 0 {
		a.applyLocked(ctx, key, req.GetRevision(), req)
		a.saveStateLocked(ctx)
	}
	return &pb.SyncupSideReply{
		AgentReply: agentReply(code, details),
		Revision:   a.revisionLocked(key),
		SideInfo:   a.sideInfoLocked(key),
		BmInfo:     a.sideBmInfoLocked(key),
	}, nil
}

// PushMigrBitmap records one migration-bitmap chunk behind the §14.9 push
// gate; the applied set derived from the recorded chunks is what the next
// SyncupSide reply reports as bm_info (§9.6).
func (a *fakeAgent) PushMigrBitmap(
	ctx context.Context, req *pb.PushMigrBitmapRequest,
) (*pb.PushMigrBitmapReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	key := sideObjKey(req.GetSidePointer())
	known := a.sideKnownLocked(req.GetSidePointer()) &&
		a.migrKnownLocked(key, req.GetMigrId())
	code, details := a.gatePushLocked(
		key, req.GetRevision(), req.GetMigrId(), known)
	if code == 0 {
		a.state[key].putChunk(
			req.GetMigrId(), req.GetBmIdx(), len(req.GetBitmap()))
		a.saveStateLocked(ctx)
	}
	return &pb.PushMigrBitmapReply{
		AgentReply: agentReply(code, details),
	}, nil
}

// GetDnInfo returns the same DnInfo the CheckDn stream reports (§14.9).
func (a *fakeAgent) GetDnInfo(
	ctx context.Context, req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)
	return &pb.GetDnInfoReply{
		AgentReply: agentReply(a.forcedCodeLocked(dnObjKey), ""),
		Revision:   a.revisionLocked(dnObjKey),
		DnInfo:     a.dnInfoLocked(),
	}, nil
}

// GetSideInfo returns the same SideInfo the CheckSide stream reports.
func (a *fakeAgent) GetSideInfo(
	ctx context.Context, req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	key := sideObjKey(req.GetSidePointer())
	if !a.sideKnownLocked(req.GetSidePointer()) {
		return &pb.GetSideInfoReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupDn.side_pointer_list"),
		}, nil
	}
	return &pb.GetSideInfoReply{
		AgentReply: agentReply(a.forcedCodeLocked(key), ""),
		Revision:   a.revisionLocked(key),
		SideInfo:   a.sideInfoLocked(key),
	}, nil
}

// CheckDn serves the DN health stream: one reply per request, never an
// unsolicited message, with the *Info delivered per the §9.7 change-only
// rule.
func (a *fakeAgent) CheckDn(
	stream grpc.BidiStreamingServer[pb.CheckDnRequest, pb.CheckDnReply],
) error {
	ctx := stream.Context()
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		drop, err := a.waitForRound(ctx, dnObjKey)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		result := a.checkGateLocked(dnObjKey, true)
		info := a.dnInfoLocked()
		a.mu.Unlock()

		reply := &pb.CheckDnReply{
			AgentReply: agentReply(result.code, result.details),
			Revision:   result.revision,
		}
		if tracker.include(req.GetShowInfo(), info) {
			reply.DnInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// CheckSide serves one side's health stream; a side pointer absent from the
// last SyncupDn is refused with ReplyCodeUnknownObject on every round
// (§14.9).
func (a *fakeAgent) CheckSide(
	stream grpc.BidiStreamingServer[pb.CheckSideRequest, pb.CheckSideReply],
) error {
	ctx := stream.Context()
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		key := sideObjKey(req.GetSidePointer())
		drop, err := a.waitForRound(ctx, key)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		result := a.checkGateLocked(
			key, a.sideKnownLocked(req.GetSidePointer()))
		var info *pb.SideInfo
		if result.code != common.ReplyCodeUnknownObject {
			info = a.sideInfoLocked(key)
		}
		a.mu.Unlock()

		reply := &pb.CheckSideReply{
			AgentReply: agentReply(result.code, result.details),
			Revision:   result.revision,
		}
		if tracker.include(req.GetShowInfo(), info) {
			reply.SideInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// service ControllerNodeAgent
// ---------------------------------------------------------------------------

// GetCnSize replies the configured size (§14.9; the worker never calls it).
func (a *fakeAgent) GetCnSize(
	ctx context.Context, req *pb.GetCnSizeRequest,
) (*pb.GetCnSizeReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)
	return &pb.GetCnSizeReply{Size: a.sizeLocked()}, nil
}

// SyncupCn implements the §14.9 revision gate for the "cn" object and drops
// the cntlrs that disappeared from the pointer list (§9.1).
func (a *fakeAgent) SyncupCn(
	ctx context.Context, req *pb.SyncupCnRequest,
) (*pb.SyncupCnReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	code, details := a.gateSyncupLocked(cnObjKey, req.GetRevision())
	if code == 0 {
		a.applyLocked(ctx, cnObjKey, req.GetRevision(), req)
		keep := make(map[string]bool, len(req.GetCntlrPointerList()))
		for _, ptr := range req.GetCntlrPointerList() {
			keep[cntlrObjKey(ptr)] = true
		}
		a.pruneLocked(cntlrObjPrefix, keep)
		a.saveStateLocked(ctx)
	}
	return &pb.SyncupCnReply{
		AgentReply: agentReply(code, details),
		Revision:   a.revisionLocked(cnObjKey),
		CnInfo:     a.cnInfoLocked(),
	}, nil
}

// SyncupCntlr implements the §14.9 ordering gate (a cntlr absent from the
// last SyncupCn is ReplyCodeUnknownObject) followed by the revision gate.
func (a *fakeAgent) SyncupCntlr(
	ctx context.Context, req *pb.SyncupCntlrRequest,
) (*pb.SyncupCntlrReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	key := cntlrObjKey(req.GetCntlrPointer())
	if !a.cntlrKnownLocked(req.GetCntlrPointer()) {
		return &pb.SyncupCntlrReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupCn.cntlr_pointer_list"),
		}, nil
	}
	code, details := a.gateSyncupLocked(key, req.GetRevision())
	if code == 0 {
		a.applyLocked(ctx, key, req.GetRevision(), req)
		a.saveStateLocked(ctx)
	}
	return &pb.SyncupCntlrReply{
		AgentReply: agentReply(code, details),
		Revision:   a.revisionLocked(key),
		CntlrInfo:  a.cntlrInfoLocked(key),
		BmInfoList: a.cntlrBmInfoListLocked(key),
	}, nil
}

// PushCloneBitmap records one clone-bitmap chunk behind the §14.9 push gate;
// the applied set is reported in the next SyncupCntlr reply's bm_info_list.
func (a *fakeAgent) PushCloneBitmap(
	ctx context.Context, req *pb.PushCloneBitmapRequest,
) (*pb.PushCloneBitmapReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	key := cntlrObjKey(req.GetCntlrPointer())
	known := a.cntlrKnownLocked(req.GetCntlrPointer()) &&
		a.cloneKnownLocked(key, req.GetCloneId())
	code, details := a.gatePushLocked(
		key, req.GetRevision(), req.GetCloneId(), known)
	if code == 0 {
		a.state[key].putChunk(
			req.GetCloneId(), req.GetBmIdx(), len(req.GetBitmap()))
		a.saveStateLocked(ctx)
	}
	return &pb.PushCloneBitmapReply{
		AgentReply: agentReply(code, details),
	}, nil
}

// GetCnInfo returns the same CnInfo the CheckCn stream reports (§14.9).
func (a *fakeAgent) GetCnInfo(
	ctx context.Context, req *pb.GetCnInfoRequest,
) (*pb.GetCnInfoReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)
	return &pb.GetCnInfoReply{
		AgentReply: agentReply(a.forcedCodeLocked(cnObjKey), ""),
		Revision:   a.revisionLocked(cnObjKey),
		CnInfo:     a.cnInfoLocked(),
	}, nil
}

// GetCntlrInfo returns the same CntlrInfo the CheckCntlr stream reports.
func (a *fakeAgent) GetCntlrInfo(
	ctx context.Context, req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked(ctx)

	key := cntlrObjKey(req.GetCntlrPointer())
	if !a.cntlrKnownLocked(req.GetCntlrPointer()) {
		return &pb.GetCntlrInfoReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupCn.cntlr_pointer_list"),
		}, nil
	}
	return &pb.GetCntlrInfoReply{
		AgentReply: agentReply(a.forcedCodeLocked(key), ""),
		Revision:   a.revisionLocked(key),
		CntlrInfo:  a.cntlrInfoLocked(key),
	}, nil
}

// GetThinDeviceBm replies an empty bitmap (§14.9; the worker never calls it).
func (a *fakeAgent) GetThinDeviceBm(
	ctx context.Context, req *pb.GetThinDeviceBmRequest,
) (*pb.GetThinDeviceBmReply, error) {
	return &pb.GetThinDeviceBmReply{}, nil
}

// GetLegBm replies an empty bitmap (§14.9; the worker never calls it).
func (a *fakeAgent) GetLegBm(
	ctx context.Context, req *pb.GetLegBmRequest,
) (*pb.GetLegBmReply, error) {
	return &pb.GetLegBmReply{}, nil
}

// CheckCn serves the CN health stream, one reply per request.
func (a *fakeAgent) CheckCn(
	stream grpc.BidiStreamingServer[pb.CheckCnRequest, pb.CheckCnReply],
) error {
	ctx := stream.Context()
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		drop, err := a.waitForRound(ctx, cnObjKey)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		result := a.checkGateLocked(cnObjKey, true)
		info := a.cnInfoLocked()
		a.mu.Unlock()

		reply := &pb.CheckCnReply{
			AgentReply: agentReply(result.code, result.details),
			Revision:   result.revision,
		}
		if tracker.include(req.GetShowInfo(), info) {
			reply.CnInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// CheckCntlr serves one cntlr's health stream; a cntlr absent from the last
// SyncupCn is refused with ReplyCodeUnknownObject on every round (§14.9).
func (a *fakeAgent) CheckCntlr(
	stream grpc.BidiStreamingServer[pb.CheckCntlrRequest, pb.CheckCntlrReply],
) error {
	ctx := stream.Context()
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		key := cntlrObjKey(req.GetCntlrPointer())
		drop, err := a.waitForRound(ctx, key)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		result := a.checkGateLocked(
			key, a.cntlrKnownLocked(req.GetCntlrPointer()))
		var info *pb.CntlrInfo
		if result.code != common.ReplyCodeUnknownObject {
			info = a.cntlrInfoLocked(key)
		}
		a.mu.Unlock()

		reply := &pb.CheckCntlrReply{
			AgentReply: agentReply(result.code, result.details),
			Revision:   result.revision,
		}
		if tracker.include(req.GetShowInfo(), info) {
			reply.CntlrInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	mode := os.Args[1]
	if mode != "dn" && mode != "cn" {
		usage()
	}
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	addr := fs.String("grpc-address", "",
		"listen address ip:port (required)")
	dir := fs.String("dir", "",
		"directory holding behavior.json and state.json (required)")
	size := fs.Uint64("size", defaultNodeSize,
		"the GetDnSize/GetCnSize reply, unless behavior.json sets \"size\"")
	fs.Parse(os.Args[2:])
	if *addr == "" {
		die("--grpc-address is required")
	}
	if *dir == "" {
		die("--dir is required")
	}

	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	agent, err := newFakeAgent(ctx, *dir, *size)
	if err != nil {
		die("%v", err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		die("listening on %s failed: %v", *addr, err)
	}
	// The server interceptors are not optional: agent.log is the suite's
	// only record of what the worker sent and which worker sent it
	// (doc/grpc.md §4, dnv-worker.md §14.9).
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	if mode == "dn" {
		pb.RegisterDiskNodeAgentServer(server, agent)
	} else {
		pb.RegisterControllerNodeAgentServer(server, agent)
	}
	slog.InfoContext(ctx, "fakeagent started",
		slog.String("mode", mode),
		slog.String("grpc_address", *addr),
		slog.String("dir", *dir),
	)
	if err := server.Serve(listener); err != nil {
		die("serving failed: %v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr,
		"usage: fakeagent dn|cn --grpc-address <ip:port> --dir <dir> [--size N]\n")
	os.Exit(2)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakeagent: "+format+"\n", args...)
	os.Exit(1)
}
