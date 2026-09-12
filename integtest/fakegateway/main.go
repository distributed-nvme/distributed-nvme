// Command fakegateway is the fake dnv gateway of the dnvctl integration suite
// (doc/dnvctl.md §7.5). It serves all 59 methods of the generated `Gateway`
// service on a plaintext listener behind the real server interceptors of
// doc/grpc.md §4, so `fakegateway.log` carries one `grpc server
// request`/`reply` record per call with the caller's trace id — the suite's
// evidence of what dnvctl put on the wire (§7.7). The JSON log goes to stderr
// through common's default logger; the script redirects it into
// `fakegateway.log`.
//
// It mirrors integtest/fakeagent's behavior/state/log contract, with the one
// structural difference doc/dnvctl.md §0 #13 calls out: everything is keyed
// per *method*, not per object, because this fake models no cluster state. It
// answers every call with an empty reply unless behavior.json says otherwise,
// and the suite it serves tests dnvctl only (§7.1) — gateway semantics stay
// gateway_test.sh's job.
//
// Two files in --dir drive and record the fake:
//
//	behavior.json  what to answer. Re-read at the top of every request whose
//	               mtime or size changed, so the script flips behaviour
//	               without restarting the fake. Absent (or `{}`) means: every
//	               method succeeds with an empty reply. A malformed file is
//	               logged and ignored, keeping the previous behaviour, so a
//	               bad file fails a test on its assertion instead of killing
//	               the fake.
//	state.json     the per-method call count and last request, written
//	               (temp file + rename) on every call BEFORE any behaviour is
//	               applied, so a refused or hung call is recorded too — §7.5
//	               "Request recording (always first)". The requests are
//	               protojson without EmitUnpopulated, which is what makes the
//	               §4 token assertions possible: an absent `--rev` is an
//	               absent `*_rev` key, `--rev 0` is `"*_rev": {}`.
//
// Methods are keyed exactly as behavior.json and state.json key them: the
// bare RPC name of `service Gateway` ("ListClusters"), the last element of
// the interceptor's `method` attribute.
//
//	usage: fakegateway --grpc-address <ip:port> --dir <dir>
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
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// The two files of §7.3's $WORK/fgw directory, next to fakegateway.log.
	behaviorFileName = "behavior.json"
	stateFileName    = "state.json"

	// hangPollInterval is how often a hanging call re-reads behavior.json
	// (§7.5: "poll every 200 ms until the lever clears or the ctx dies").
	hangPollInterval = 200 * time.Millisecond
)

// ---------------------------------------------------------------------------
// The method type registry (§7.5)
// ---------------------------------------------------------------------------

// methodTypes is one method's request and reply message type. behavior.json's
// `reply` is strict protojson of the reply type, and a hand-edited
// `last_request` is parsed against the request type, so the fake needs both.
type methodTypes struct {
	request protoreflect.MessageType
	reply   protoreflect.MessageType
}

// gatewayMethodTypes maps every `service Gateway` method name to its types.
//
// It is derived from the service's own descriptor in this one place, so it
// cannot drift: a renamed reply message or a 60th RPC changes the descriptor
// and this map with it. Building it by hand, or by concatenating "Reply" onto
// the method name at 59 call sites, is exactly the drift this avoids.
//
// The compiler is NOT the backstop here — the embedded
// UnimplementedGatewayServer would silently satisfy a 60th method with an
// Unimplemented stub — so main_test.go is: it pins the map against
// pb.Gateway_ServiceDesc.Methods in both directions and drives all 59
// methods, which is what would fail the day the service grows.
var gatewayMethodTypes = newGatewayMethodTypes()

func newGatewayMethodTypes() map[string]methodTypes {
	// pb.Gateway_ServiceDesc.ServiceName is the service's full proto name
	// ("Gateway" — schema.proto declares no proto package), so it is also
	// its key in the global descriptor registry.
	name := protoreflect.FullName(pb.Gateway_ServiceDesc.ServiceName)
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err != nil {
		panic(fmt.Sprintf("service %s is not registered: %v", name, err))
	}
	service, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		panic(fmt.Sprintf("%s is a %T, not a service", name, desc))
	}
	methods := service.Methods()
	types := make(map[string]methodTypes, methods.Len())
	for i := 0; i < methods.Len(); i++ {
		method := methods.Get(i)
		request, err := protoregistry.GlobalTypes.FindMessageByName(
			method.Input().FullName())
		if err != nil {
			panic(fmt.Sprintf("%s request type: %v", method.Name(), err))
		}
		reply, err := protoregistry.GlobalTypes.FindMessageByName(
			method.Output().FullName())
		if err != nil {
			panic(fmt.Sprintf("%s reply type: %v", method.Name(), err))
		}
		types[string(method.Name())] = methodTypes{
			request: request,
			reply:   reply,
		}
	}
	return types
}

// newReply returns an empty reply message of one method — the canned default
// of §7.5 ("absent ⇒ the canned default, an empty reply message"), which
// dnvctl's EmitUnpopulated rendering then fills out client-side. An unknown
// method returns nil; serve turns that into an Internal status.
func newReply(method string) proto.Message {
	types, ok := gatewayMethodTypes[method]
	if !ok {
		return nil
	}
	return types.reply.New().Interface()
}

// newRequest is newReply's twin, used to parse a hand-edited last_request.
func newRequest(method string) proto.Message {
	types, ok := gatewayMethodTypes[method]
	if !ok {
		return nil
	}
	return types.request.New().Interface()
}

// ---------------------------------------------------------------------------
// gRPC status codes (§7.5: `code` is UPPER_SNAKE)
// ---------------------------------------------------------------------------

// codeNames spells every gRPC status code the way behavior.json's `code`
// writes it and dnvctl's §3.2 error line renders it: UPPER_SNAKE, the
// canonical proto enum spelling. This is integtest/gatewayctl's table, kept
// identical on purpose — a test author copying a code out of a dnvctl error
// line must be able to paste it into behavior.json.
//
// codes.Code.String() renders CamelCase ("AlreadyExists"), so upper-casing it
// would yield ALREADYEXISTS and no injection of a multi-word code would ever
// parse. The table is explicit for exactly that reason.
var codeNames = map[codes.Code]string{
	codes.OK:                 "OK",
	codes.Canceled:           "CANCELLED",
	codes.Unknown:            "UNKNOWN",
	codes.InvalidArgument:    "INVALID_ARGUMENT",
	codes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
	codes.NotFound:           "NOT_FOUND",
	codes.AlreadyExists:      "ALREADY_EXISTS",
	codes.PermissionDenied:   "PERMISSION_DENIED",
	codes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
	codes.FailedPrecondition: "FAILED_PRECONDITION",
	codes.Aborted:            "ABORTED",
	codes.OutOfRange:         "OUT_OF_RANGE",
	codes.Unimplemented:      "UNIMPLEMENTED",
	codes.Internal:           "INTERNAL",
	codes.Unavailable:        "UNAVAILABLE",
	codes.DataLoss:           "DATA_LOSS",
	codes.Unauthenticated:    "UNAUTHENTICATED",
}

// codeValues inverts codeNames so the file is the single source of both
// directions. "CANCELED" is added as an alias because that is how Go spells
// codes.Canceled; both spellings mean code 1, and neither is a typo worth
// rejecting.
var codeValues = newCodeValues()

func newCodeValues() map[string]codes.Code {
	values := make(map[string]codes.Code, len(codeNames)+1)
	for code, name := range codeNames {
		values[name] = code
	}
	values["CANCELED"] = codes.Canceled
	return values
}

// codeName renders a status code the way behavior.json writes it. An unknown
// numeric code falls back to its number, so a surprise is visible rather than
// silently equal to something else.
func codeName(code codes.Code) string {
	if name, ok := codeNames[code]; ok {
		return name
	}
	return fmt.Sprintf("CODE_%d", uint32(code))
}

// parseCode accepts the UPPER_SNAKE spelling of §7.5, case-insensitively and
// with surrounding space trimmed (the fakeagent parseResStatus recipe). An
// unknown spelling is an error, which makes the whole file malformed: a
// silently misspelled code would otherwise fail a test far from its cause.
func parseCode(name string) (codes.Code, error) {
	key := strings.ToUpper(strings.TrimSpace(name))
	code, ok := codeValues[key]
	if !ok {
		return codes.OK, fmt.Errorf("unknown code %q", name)
	}
	return code, nil
}

// ---------------------------------------------------------------------------
// behavior.json (§7.5)
// ---------------------------------------------------------------------------

// methodBehavior is one entry of behavior.json's "methods" map and also the
// shape of its "default" entry (which may not carry a `reply`, see validate).
//
// Every lever is a pointer so "absent" and "set to the zero value" stay
// distinguishable: that is what lets the §7.5 merge pick the most specific
// value KEY BY KEY, e.g. `methods.X.hang = false` switching off a
// `default.hang = true` while `default.code` still applies.
type methodBehavior struct {
	Code    *string         `json:"code,omitempty"`
	Message *string         `json:"message,omitempty"`
	Hang    *bool           `json:"hang,omitempty"`
	Reply   json.RawMessage `json:"reply,omitempty"`

	// code and reply are Code and Reply parsed once by validate, so a
	// request never re-parses them.
	code  codes.Code
	reply proto.Message
}

// behaviorFile is the whole file.
type behaviorFile struct {
	Default *methodBehavior            `json:"default,omitempty"`
	Methods map[string]*methodBehavior `json:"methods,omitempty"`
}

// validate parses one entry's code and reply. reply is the empty reply
// message of the method the entry belongs to, or nil for the "default" entry,
// which has no reply type of its own.
//
// An unparseable code or reply makes the whole file malformed, which the
// caller logs and ignores, keeping the previous behaviour (§7.5).
func (mb *methodBehavior) validate(where string, reply proto.Message) error {
	if mb == nil {
		return nil
	}
	if mb.Code != nil {
		code, err := parseCode(*mb.Code)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		mb.code = code
	}
	if len(mb.Reply) == 0 {
		return nil
	}
	if reply == nil {
		// A reply type is per method, so a "default" reply could not be
		// decoded, let alone applied to all 59 methods at once. Rejecting
		// it beats accepting a key that silently does nothing.
		return fmt.Errorf(
			"%s: \"reply\" belongs under \"methods\", not \"default\"", where)
	}
	// Strict on purpose (§7.5 "reply is strict protojson of the method's
	// reply type"): a reply field that does not exist is a test bug, and a
	// dropped field would show up as a puzzling assertion failure instead.
	if err := protojson.Unmarshal(mb.Reply, reply); err != nil {
		return fmt.Errorf("%s: reply: %w", where, err)
	}
	mb.reply = reply
	return nil
}

// parseBehavior decodes behavior.json. Unknown fields are an error on
// purpose: a typo in a key ("hang_it") must be reported, not silently
// dropped, because the resulting behaviour would look like a dnvctl bug.
// An unknown "methods" key is rejected for the same reason — a misspelled
// RPC name would inject nothing at all.
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
	if err := parsed.Default.validate("default", nil); err != nil {
		return nil, err
	}
	for method, mb := range parsed.Methods {
		where := fmt.Sprintf("methods[%q]", method)
		// newReply doubles as the existence check: an unknown RPC name has
		// no reply type, and it is rejected here even when the entry carries
		// no `reply` at all.
		reply := newReply(method)
		if reply == nil {
			return nil, fmt.Errorf("%s: unknown method", where)
		}
		if err := mb.validate(where, reply); err != nil {
			return nil, err
		}
	}
	return parsed, nil
}

// resolvedBehavior is what one method's levers come to after the §7.5 merge.
type resolvedBehavior struct {
	code    codes.Code
	message string
	hang    bool

	// reply is the injected reply, or nil for the canned empty one.
	reply proto.Message
}

// ---------------------------------------------------------------------------
// state.json (§7.5)
// ---------------------------------------------------------------------------

// methodState is one method's record: how many calls arrived and what the
// last one carried. LastRequest holds protojson so the file stays readable
// and jq-able — §7.7 reads it as `.methods["<Rpc>"].last_request`.
type methodState struct {
	Count       uint64          `json:"count"`
	LastRequest json.RawMessage `json:"last_request,omitempty"`
}

// stateFile is the whole file: {"methods": {"<RpcName>": {…}}}.
type stateFile struct {
	Methods map[string]*methodState `json:"methods"`
}

// ---------------------------------------------------------------------------
// The fake
// ---------------------------------------------------------------------------

// fakeGateway implements all 59 methods of service Gateway. Every field
// behind mu is shared by the concurrent handlers of one process: the suite
// drives one command at a time, but nothing about a gRPC server guarantees
// that, and a torn state.json would be an unreproducible test failure.
//
// UnimplementedGatewayServer is embedded because the generated interface
// demands it (pb/schema_grpc.pb.go), not because anything is left
// unimplemented — all 59 methods below shadow its stubs.
type fakeGateway struct {
	pb.UnimplementedGatewayServer

	dir string

	mu         sync.Mutex
	state      map[string]*methodState
	stateMtime time.Time
	beh        *behaviorFile
	behMtime   time.Time
	behSize    int64
	behPresent bool
}

func newFakeGateway(ctx context.Context, dir string) (*fakeGateway, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fake := &fakeGateway{
		dir:   dir,
		state: make(map[string]*methodState),
		beh:   &behaviorFile{},
	}
	// Load both files at start, so a restarted fake answers like the one it
	// replaced and keeps counting where it left off.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.refreshLocked(ctx)
	return fake, nil
}

// refreshLocked re-reads both files if they changed. It runs at the top of
// every call: behavior.json so the script can flip behaviour without a
// restart, state.json so an operator edit (resetting a count, say) is seen
// before the next count bump rather than after it.
func (g *fakeGateway) refreshLocked(ctx context.Context) {
	g.reloadBehaviorLocked(ctx)
	g.reloadStateLocked(ctx)
}

// reloadBehaviorLocked implements the §7.5 "reloaded at the top of every
// request when mtime *or size* changed" rule. Size is part of the guard
// because a same-second rewrite of a different length must still be noticed.
// A malformed file is logged once per mtime and ignored, keeping the previous
// behaviour.
func (g *fakeGateway) reloadBehaviorLocked(ctx context.Context) {
	path := filepath.Join(g.dir, behaviorFileName)
	info, err := os.Stat(path)
	if err != nil {
		if g.behPresent {
			slog.InfoContext(ctx, "behavior file gone",
				slog.String("path", path))
			g.beh = &behaviorFile{}
			g.behPresent = false
			g.behMtime = time.Time{}
			g.behSize = 0
		}
		return
	}
	if g.behPresent && info.ModTime().Equal(g.behMtime) &&
		info.Size() == g.behSize {
		return
	}
	// Stamped before the read, so a malformed file is reported once per
	// mtime instead of once per request.
	g.behPresent = true
	g.behMtime = info.ModTime()
	g.behSize = info.Size()
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
	g.beh = parsed
	slog.InfoContext(ctx, "behavior file loaded",
		slog.String("path", path),
		slog.Int("method_cnt", len(parsed.Methods)))
}

// reloadStateLocked re-reads state.json when it is newer than this process's
// own last write. Comparing against the fake's own write is what keeps a
// recorded call from being mistaken for an operator edit (and the operator's
// edit from being clobbered): the fake rewrites the file on every call, so
// without the guard every call would re-read what the previous one wrote.
func (g *fakeGateway) reloadStateLocked(ctx context.Context) {
	path := filepath.Join(g.dir, stateFileName)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if !info.ModTime().After(g.stateMtime) {
		return
	}
	g.stateMtime = info.ModTime()
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
	loaded := make(map[string]*methodState, len(parsed.Methods))
	for method, entry := range parsed.Methods {
		if entry == nil {
			continue
		}
		if len(entry.LastRequest) != 0 {
			// The parsed message is thrown away — the fake never reads a
			// request back — but parsing it turns a bad hand edit into one
			// clear record here instead of a confusing jq failure later.
			// Lenient (DiscardUnknown) because the file is meant to be
			// edited by hand; behavior.json, which is generated by the
			// script, is the strict one.
			msg := newRequest(method)
			if msg == nil {
				slog.ErrorContext(ctx, "state file unknown method",
					slog.String("method", method))
				continue
			}
			opts := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := opts.Unmarshal(entry.LastRequest, msg); err != nil {
				slog.ErrorContext(ctx, "state file request malformed",
					slog.String("method", method),
					slog.String("error", err.Error()))
				continue
			}
		}
		loaded[method] = entry
	}
	g.state = loaded
	slog.InfoContext(ctx, "state file loaded",
		slog.String("path", path),
		slog.Int("method_cnt", len(loaded)))
}

// saveStateLocked writes state.json through a temp file and a rename, so the
// script's concurrent reader never sees a partial file (§7.5). The resulting
// mtime is remembered as "our own last write" for reloadStateLocked. Every
// failure is logged and swallowed: a fake that cannot write its state still
// answers, and the missing record fails the assertion that needed it.
func (g *fakeGateway) saveStateLocked(ctx context.Context) {
	path := filepath.Join(g.dir, stateFileName)
	data, err := json.MarshalIndent(&stateFile{Methods: g.state}, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "state file marshaling failed",
			slog.String("error", err.Error()))
		return
	}
	data = append(data, '\n')
	// The temp file must share the directory: rename is only atomic within
	// one filesystem.
	tmp, err := os.CreateTemp(g.dir, stateFileName+".tmp*")
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
		g.stateMtime = info.ModTime()
	}
}

// recordLocked implements §7.5's "Request recording (always first)": the
// count and the request land before any behaviour is applied, so a call that
// is then refused or left hanging is still on the record. §7.12's error
// cases and §7.13's hang both depend on that.
//
// The request is protojson with UseProtoNames and WITHOUT EmitUnpopulated:
// snake_case keys, and an unset message stays an absent key. That is the
// whole point of the §4 token assertions — no `--rev` means no `sp_rev` key,
// `--rev 0` means `"sp_rev": {}`.
func (g *fakeGateway) recordLocked(
	ctx context.Context, method string, req proto.Message,
) {
	entry := g.state[method]
	if entry == nil {
		entry = &methodState{}
		g.state[method] = entry
	}
	entry.Count++
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		slog.ErrorContext(ctx, "request marshaling failed",
			slog.String("method", method),
			slog.String("error", err.Error()))
	} else {
		entry.LastRequest = raw
	}
	g.saveStateLocked(ctx)
}

// resolveLocked merges behavior.json down to one method's levers, most
// specific wins KEY BY KEY (§7.5): the built-in default, then "default",
// then "methods.<Rpc>". Each key is taken from the most specific entry that
// set it, so `default` can force a code while one method overrides only the
// message.
func (g *fakeGateway) resolveLocked(method string) resolvedBehavior {
	resolved := resolvedBehavior{code: codes.OK}
	messageSet := false
	apply := func(mb *methodBehavior) {
		if mb == nil {
			return
		}
		if mb.Code != nil {
			resolved.code = mb.code
		}
		if mb.Message != nil {
			resolved.message = *mb.Message
			messageSet = true
		}
		if mb.Hang != nil {
			resolved.hang = *mb.Hang
		}
		if mb.reply != nil {
			resolved.reply = mb.reply
		}
	}
	if g.beh != nil {
		apply(g.beh.Default)
		apply(g.beh.Methods[method])
	}
	if !messageSet {
		// §7.5: message defaults to "behavior.json <code>". Computed even
		// for OK so the error path has nothing left to decide.
		resolved.message = "behavior.json " + codeName(resolved.code)
	}
	return resolved
}

// waitForHang applies behavior.json's `hang` lever (§7.5). It is how §7.13
// case D step 2 manufactures a DEADLINE_EXCEEDED: a listening gateway that
// never answers, distinct from a closed port, which fails instantly.
//
// The lock is never held while waiting, so one hung call cannot wedge the
// rest of the process, and behavior.json is re-read on every poll, so the
// wait ends as soon as the script clears the lever — or as soon as the
// caller's context dies, which is what dnvctl's --timeout cancels.
// status.FromContextError maps that to codes.DeadlineExceeded resp.
// codes.Canceled, so the client sees a proper gRPC code, not a bare Unknown.
func (g *fakeGateway) waitForHang(ctx context.Context, method string) error {
	for {
		g.mu.Lock()
		g.refreshLocked(ctx)
		hang := g.resolveLocked(method).hang
		g.mu.Unlock()
		if !hang {
			return nil
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(hangPollInterval):
		}
	}
}

// call is the whole §7.5 per-call pipeline, in the order the section fixes:
// reload, record, hang, code, reply.
func (g *fakeGateway) call(
	ctx context.Context, method string, req proto.Message,
) (proto.Message, error) {
	g.mu.Lock()
	g.refreshLocked(ctx)
	g.recordLocked(ctx, method, req)
	g.mu.Unlock()

	if err := g.waitForHang(ctx, method); err != nil {
		return nil, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	// No refresh here: waitForHang always ran at least one poll, and that
	// poll's reload is the freshest read there is.
	resolved := g.resolveLocked(method)
	if resolved.code != codes.OK {
		return nil, status.Error(resolved.code, resolved.message)
	}
	if resolved.reply != nil {
		// Cloned, never handed out directly: the parse is cached until the
		// file changes, so two concurrent calls would otherwise share one
		// message with whatever the grpc codec and the §4 interceptor do
		// to it.
		return proto.Clone(resolved.reply), nil
	}
	return newReply(method), nil
}

// serve adapts call to one handler's concrete reply type. Go has no generic
// methods, so this is a free function; the type parameter is what the 59
// one-line handlers below spell out.
func serve[RepT proto.Message](
	g *fakeGateway, ctx context.Context, method string, req proto.Message,
) (RepT, error) {
	var zero RepT
	reply, err := g.call(ctx, method, req)
	if err != nil {
		return zero, err
	}
	typed, ok := reply.(RepT)
	if !ok {
		// Only reachable if a handler passed a method name the registry
		// does not know, or one belonging to a different reply type.
		// main_test.go drives all 59 methods to keep it unreachable.
		return zero, status.Errorf(codes.Internal,
			"%s: the registry produced %T, not the handler's reply type",
			method, reply)
	}
	return typed, nil
}

// ---------------------------------------------------------------------------
// The 59 Gateway methods (§7.5), in pb.Gateway_ServiceDesc order
// ---------------------------------------------------------------------------
//
// Every one of them is the same one-liner: the fake models no cluster state
// (§0 #13), so the method name is all that separates them.

func (g *fakeGateway) CreateCluster(
	ctx context.Context, req *pb.CreateClusterRequest,
) (*pb.CreateClusterReply, error) {
	return serve[*pb.CreateClusterReply](g, ctx, "CreateCluster", req)
}

func (g *fakeGateway) DeleteCluster(
	ctx context.Context, req *pb.DeleteClusterRequest,
) (*pb.DeleteClusterReply, error) {
	return serve[*pb.DeleteClusterReply](g, ctx, "DeleteCluster", req)
}

func (g *fakeGateway) GetCluster(
	ctx context.Context, req *pb.GetClusterRequest,
) (*pb.GetClusterReply, error) {
	return serve[*pb.GetClusterReply](g, ctx, "GetCluster", req)
}

func (g *fakeGateway) ListClusters(
	ctx context.Context, req *pb.ListClustersRequest,
) (*pb.ListClustersReply, error) {
	return serve[*pb.ListClustersReply](g, ctx, "ListClusters", req)
}

func (g *fakeGateway) CreateDiskNode(
	ctx context.Context, req *pb.CreateDiskNodeRequest,
) (*pb.CreateDiskNodeReply, error) {
	return serve[*pb.CreateDiskNodeReply](g, ctx, "CreateDiskNode", req)
}

func (g *fakeGateway) DeleteDiskNode(
	ctx context.Context, req *pb.DeleteDiskNodeRequest,
) (*pb.DeleteDiskNodeReply, error) {
	return serve[*pb.DeleteDiskNodeReply](g, ctx, "DeleteDiskNode", req)
}

func (g *fakeGateway) GetDiskNode(
	ctx context.Context, req *pb.GetDiskNodeRequest,
) (*pb.GetDiskNodeReply, error) {
	return serve[*pb.GetDiskNodeReply](g, ctx, "GetDiskNode", req)
}

func (g *fakeGateway) ListDiskNodes(
	ctx context.Context, req *pb.ListDiskNodesRequest,
) (*pb.ListDiskNodesReply, error) {
	return serve[*pb.ListDiskNodesReply](g, ctx, "ListDiskNodes", req)
}

func (g *fakeGateway) UpdateDiskNodeDisabled(
	ctx context.Context, req *pb.UpdateDiskNodeDisabledRequest,
) (*pb.UpdateDiskNodeDisabledReply, error) {
	return serve[*pb.UpdateDiskNodeDisabledReply](g, ctx, "UpdateDiskNodeDisabled", req)
}

func (g *fakeGateway) InspectDiskNode(
	ctx context.Context, req *pb.InspectDiskNodeRequest,
) (*pb.InspectDiskNodeReply, error) {
	return serve[*pb.InspectDiskNodeReply](g, ctx, "InspectDiskNode", req)
}

func (g *fakeGateway) CreateControllerNode(
	ctx context.Context, req *pb.CreateControllerNodeRequest,
) (*pb.CreateControllerNodeReply, error) {
	return serve[*pb.CreateControllerNodeReply](g, ctx, "CreateControllerNode", req)
}

func (g *fakeGateway) DeleteControllerNode(
	ctx context.Context, req *pb.DeleteControllerNodeRequest,
) (*pb.DeleteControllerNodeReply, error) {
	return serve[*pb.DeleteControllerNodeReply](g, ctx, "DeleteControllerNode", req)
}

func (g *fakeGateway) GetControllerNode(
	ctx context.Context, req *pb.GetControllerNodeRequest,
) (*pb.GetControllerNodeReply, error) {
	return serve[*pb.GetControllerNodeReply](g, ctx, "GetControllerNode", req)
}

func (g *fakeGateway) ListControllerNodes(
	ctx context.Context, req *pb.ListControllerNodesRequest,
) (*pb.ListControllerNodesReply, error) {
	return serve[*pb.ListControllerNodesReply](g, ctx, "ListControllerNodes", req)
}

func (g *fakeGateway) UpdateControllerNodeDisabled(
	ctx context.Context, req *pb.UpdateControllerNodeDisabledRequest,
) (*pb.UpdateControllerNodeDisabledReply, error) {
	return serve[*pb.UpdateControllerNodeDisabledReply](g, ctx, "UpdateControllerNodeDisabled", req)
}

func (g *fakeGateway) InspectControllerNode(
	ctx context.Context, req *pb.InspectControllerNodeRequest,
) (*pb.InspectControllerNodeReply, error) {
	return serve[*pb.InspectControllerNodeReply](g, ctx, "InspectControllerNode", req)
}

func (g *fakeGateway) CreateStoragePool(
	ctx context.Context, req *pb.CreateStoragePoolRequest,
) (*pb.CreateStoragePoolReply, error) {
	return serve[*pb.CreateStoragePoolReply](g, ctx, "CreateStoragePool", req)
}

func (g *fakeGateway) DeleteStoragePool(
	ctx context.Context, req *pb.DeleteStoragePoolRequest,
) (*pb.DeleteStoragePoolReply, error) {
	return serve[*pb.DeleteStoragePoolReply](g, ctx, "DeleteStoragePool", req)
}

func (g *fakeGateway) GetStoragePool(
	ctx context.Context, req *pb.GetStoragePoolRequest,
) (*pb.GetStoragePoolReply, error) {
	return serve[*pb.GetStoragePoolReply](g, ctx, "GetStoragePool", req)
}

func (g *fakeGateway) ListStoragePools(
	ctx context.Context, req *pb.ListStoragePoolsRequest,
) (*pb.ListStoragePoolsReply, error) {
	return serve[*pb.ListStoragePoolsReply](g, ctx, "ListStoragePools", req)
}

func (g *fakeGateway) UpdateStoragePoolCntlidSlotList(
	ctx context.Context, req *pb.UpdateStoragePoolCntlidSlotListRequest,
) (*pb.UpdateStoragePoolCntlidSlotListReply, error) {
	return serve[*pb.UpdateStoragePoolCntlidSlotListReply](g, ctx, "UpdateStoragePoolCntlidSlotList", req)
}

func (g *fakeGateway) UpdateStoragePoolLevel(
	ctx context.Context, req *pb.UpdateStoragePoolLevelRequest,
) (*pb.UpdateStoragePoolLevelReply, error) {
	return serve[*pb.UpdateStoragePoolLevelReply](g, ctx, "UpdateStoragePoolLevel", req)
}

func (g *fakeGateway) FindStoragePoolNames(
	ctx context.Context, req *pb.FindStoragePoolNamesRequest,
) (*pb.FindStoragePoolNamesReply, error) {
	return serve[*pb.FindStoragePoolNamesReply](g, ctx, "FindStoragePoolNames", req)
}

func (g *fakeGateway) GrowSlice(
	ctx context.Context, req *pb.GrowSliceRequest,
) (*pb.GrowSliceReply, error) {
	return serve[*pb.GrowSliceReply](g, ctx, "GrowSlice", req)
}

func (g *fakeGateway) CreateCntlr(
	ctx context.Context, req *pb.CreateCntlrRequest,
) (*pb.CreateCntlrReply, error) {
	return serve[*pb.CreateCntlrReply](g, ctx, "CreateCntlr", req)
}

func (g *fakeGateway) DeleteCntlr(
	ctx context.Context, req *pb.DeleteCntlrRequest,
) (*pb.DeleteCntlrReply, error) {
	return serve[*pb.DeleteCntlrReply](g, ctx, "DeleteCntlr", req)
}

func (g *fakeGateway) UpdateCntlrEnabled(
	ctx context.Context, req *pb.UpdateCntlrEnabledRequest,
) (*pb.UpdateCntlrEnabledReply, error) {
	return serve[*pb.UpdateCntlrEnabledReply](g, ctx, "UpdateCntlrEnabled", req)
}

func (g *fakeGateway) InspectCntlr(
	ctx context.Context, req *pb.InspectCntlrRequest,
) (*pb.InspectCntlrReply, error) {
	return serve[*pb.InspectCntlrReply](g, ctx, "InspectCntlr", req)
}

func (g *fakeGateway) InspectSide(
	ctx context.Context, req *pb.InspectSideRequest,
) (*pb.InspectSideReply, error) {
	return serve[*pb.InspectSideReply](g, ctx, "InspectSide", req)
}

func (g *fakeGateway) CreateThinDevice(
	ctx context.Context, req *pb.CreateThinDeviceRequest,
) (*pb.CreateThinDeviceReply, error) {
	return serve[*pb.CreateThinDeviceReply](g, ctx, "CreateThinDevice", req)
}

func (g *fakeGateway) DeleteThinDevice(
	ctx context.Context, req *pb.DeleteThinDeviceRequest,
) (*pb.DeleteThinDeviceReply, error) {
	return serve[*pb.DeleteThinDeviceReply](g, ctx, "DeleteThinDevice", req)
}

func (g *fakeGateway) ListThinDevices(
	ctx context.Context, req *pb.ListThinDevicesRequest,
) (*pb.ListThinDevicesReply, error) {
	return serve[*pb.ListThinDevicesReply](g, ctx, "ListThinDevices", req)
}

func (g *fakeGateway) CreateSubsystem(
	ctx context.Context, req *pb.CreateSubsystemRequest,
) (*pb.CreateSubsystemReply, error) {
	return serve[*pb.CreateSubsystemReply](g, ctx, "CreateSubsystem", req)
}

func (g *fakeGateway) DeleteSubsystem(
	ctx context.Context, req *pb.DeleteSubsystemRequest,
) (*pb.DeleteSubsystemReply, error) {
	return serve[*pb.DeleteSubsystemReply](g, ctx, "DeleteSubsystem", req)
}

func (g *fakeGateway) ListSubsystems(
	ctx context.Context, req *pb.ListSubsystemsRequest,
) (*pb.ListSubsystemsReply, error) {
	return serve[*pb.ListSubsystemsReply](g, ctx, "ListSubsystems", req)
}

func (g *fakeGateway) UpdateSubsystemHosts(
	ctx context.Context, req *pb.UpdateSubsystemHostsRequest,
) (*pb.UpdateSubsystemHostsReply, error) {
	return serve[*pb.UpdateSubsystemHostsReply](g, ctx, "UpdateSubsystemHosts", req)
}

func (g *fakeGateway) CreateNamespace(
	ctx context.Context, req *pb.CreateNamespaceRequest,
) (*pb.CreateNamespaceReply, error) {
	return serve[*pb.CreateNamespaceReply](g, ctx, "CreateNamespace", req)
}

func (g *fakeGateway) DeleteNamespace(
	ctx context.Context, req *pb.DeleteNamespaceRequest,
) (*pb.DeleteNamespaceReply, error) {
	return serve[*pb.DeleteNamespaceReply](g, ctx, "DeleteNamespace", req)
}

func (g *fakeGateway) UpdateNamespaceDev(
	ctx context.Context, req *pb.UpdateNamespaceDevRequest,
) (*pb.UpdateNamespaceDevReply, error) {
	return serve[*pb.UpdateNamespaceDevReply](g, ctx, "UpdateNamespaceDev", req)
}

func (g *fakeGateway) UpdateNamespaceSuspended(
	ctx context.Context, req *pb.UpdateNamespaceSuspendedRequest,
) (*pb.UpdateNamespaceSuspendedReply, error) {
	return serve[*pb.UpdateNamespaceSuspendedReply](g, ctx, "UpdateNamespaceSuspended", req)
}

func (g *fakeGateway) CreateClone(
	ctx context.Context, req *pb.CreateCloneRequest,
) (*pb.CreateCloneReply, error) {
	return serve[*pb.CreateCloneReply](g, ctx, "CreateClone", req)
}

func (g *fakeGateway) DeleteClone(
	ctx context.Context, req *pb.DeleteCloneRequest,
) (*pb.DeleteCloneReply, error) {
	return serve[*pb.DeleteCloneReply](g, ctx, "DeleteClone", req)
}

func (g *fakeGateway) GetClone(
	ctx context.Context, req *pb.GetCloneRequest,
) (*pb.GetCloneReply, error) {
	return serve[*pb.GetCloneReply](g, ctx, "GetClone", req)
}

func (g *fakeGateway) UpdateCloneTrConf(
	ctx context.Context, req *pb.UpdateCloneTrConfRequest,
) (*pb.UpdateCloneTrConfReply, error) {
	return serve[*pb.UpdateCloneTrConfReply](g, ctx, "UpdateCloneTrConf", req)
}

func (g *fakeGateway) AppendCloneBitmap(
	ctx context.Context, req *pb.AppendCloneBitmapRequest,
) (*pb.AppendCloneBitmapReply, error) {
	return serve[*pb.AppendCloneBitmapReply](g, ctx, "AppendCloneBitmap", req)
}

func (g *fakeGateway) CreateTransfer(
	ctx context.Context, req *pb.CreateTransferRequest,
) (*pb.CreateTransferReply, error) {
	return serve[*pb.CreateTransferReply](g, ctx, "CreateTransfer", req)
}

func (g *fakeGateway) DeleteTransfer(
	ctx context.Context, req *pb.DeleteTransferRequest,
) (*pb.DeleteTransferReply, error) {
	return serve[*pb.DeleteTransferReply](g, ctx, "DeleteTransfer", req)
}

func (g *fakeGateway) GetTransfer(
	ctx context.Context, req *pb.GetTransferRequest,
) (*pb.GetTransferReply, error) {
	return serve[*pb.GetTransferReply](g, ctx, "GetTransfer", req)
}

func (g *fakeGateway) UpdateTransferHosts(
	ctx context.Context, req *pb.UpdateTransferHostsRequest,
) (*pb.UpdateTransferHostsReply, error) {
	return serve[*pb.UpdateTransferHostsReply](g, ctx, "UpdateTransferHosts", req)
}

func (g *fakeGateway) CreateMigration(
	ctx context.Context, req *pb.CreateMigrationRequest,
) (*pb.CreateMigrationReply, error) {
	return serve[*pb.CreateMigrationReply](g, ctx, "CreateMigration", req)
}

func (g *fakeGateway) FinishMigration(
	ctx context.Context, req *pb.FinishMigrationRequest,
) (*pb.FinishMigrationReply, error) {
	return serve[*pb.FinishMigrationReply](g, ctx, "FinishMigration", req)
}

func (g *fakeGateway) CancelMigration(
	ctx context.Context, req *pb.CancelMigrationRequest,
) (*pb.CancelMigrationReply, error) {
	return serve[*pb.CancelMigrationReply](g, ctx, "CancelMigration", req)
}

func (g *fakeGateway) GetMigration(
	ctx context.Context, req *pb.GetMigrationRequest,
) (*pb.GetMigrationReply, error) {
	return serve[*pb.GetMigrationReply](g, ctx, "GetMigration", req)
}

func (g *fakeGateway) AppendMigrationBitmap(
	ctx context.Context, req *pb.AppendMigrationBitmapRequest,
) (*pb.AppendMigrationBitmapReply, error) {
	return serve[*pb.AppendMigrationBitmapReply](g, ctx, "AppendMigrationBitmap", req)
}

func (g *fakeGateway) CreateSpareLeg(
	ctx context.Context, req *pb.CreateSpareLegRequest,
) (*pb.CreateSpareLegReply, error) {
	return serve[*pb.CreateSpareLegReply](g, ctx, "CreateSpareLeg", req)
}

func (g *fakeGateway) DeleteSpareLeg(
	ctx context.Context, req *pb.DeleteSpareLegRequest,
) (*pb.DeleteSpareLegReply, error) {
	return serve[*pb.DeleteSpareLegReply](g, ctx, "DeleteSpareLeg", req)
}

func (g *fakeGateway) SwitchSpareLeg(
	ctx context.Context, req *pb.SwitchSpareLegRequest,
) (*pb.SwitchSpareLegReply, error) {
	return serve[*pb.SwitchSpareLegReply](g, ctx, "SwitchSpareLeg", req)
}

func (g *fakeGateway) GetThinDeviceBitmap(
	ctx context.Context, req *pb.GetThinDeviceBitmapRequest,
) (*pb.GetThinDeviceBitmapReply, error) {
	return serve[*pb.GetThinDeviceBitmapReply](g, ctx, "GetThinDeviceBitmap", req)
}

func (g *fakeGateway) GetLegBitmap(
	ctx context.Context, req *pb.GetLegBitmapRequest,
) (*pb.GetLegBitmapReply, error) {
	return serve[*pb.GetLegBitmapReply](g, ctx, "GetLegBitmap", req)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	fs := flag.NewFlagSet("fakegateway", flag.ExitOnError)
	fs.Usage = usage
	addr := fs.String("grpc-address", "",
		"listen address ip:port (required)")
	dir := fs.String("dir", "",
		"directory holding behavior.json and state.json (required)")
	fs.Parse(os.Args[1:])
	if *addr == "" {
		die("--grpc-address is required")
	}
	if *dir == "" {
		die("--dir is required")
	}

	// One trace id for the process's own records; per-call records carry the
	// caller's, extracted from the metadata by the §4 interceptor.
	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	fake, err := newFakeGateway(ctx, *dir)
	if err != nil {
		die("%v", err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		die("listening on %s failed: %v", *addr, err)
	}
	// The server interceptors are not optional: fakegateway.log is the
	// suite's only record of what dnvctl sent and under which trace id
	// (doc/grpc.md §4, dnvctl.md §7.5/§7.7). The stream twin is installed
	// even though every Gateway RPC is unary — the §4 chain is one rule, and
	// a fake that half-applies it is a fake that stops being evidence.
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterGatewayServer(server, fake)
	slog.InfoContext(ctx, "fakegateway started",
		slog.String("grpc_address", *addr),
		slog.String("dir", *dir),
	)
	if err := server.Serve(listener); err != nil {
		die("serving failed: %v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr,
		"usage: fakegateway --grpc-address <ip:port> --dir <dir>\n")
	os.Exit(2)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakegateway: "+format+"\n", args...)
	os.Exit(1)
}
