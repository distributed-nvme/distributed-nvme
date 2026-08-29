package common

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ---------------------------------------------------------------------------
// Trace id plumbing (log.md R5)
// ---------------------------------------------------------------------------

type contextKey string

const traceIdCtxKey contextKey = "trace_id"

// TraceIdLogKey is the JSON attribute name under which the trace id appears
// in every log record.
const TraceIdLogKey = "trace_id"

// TraceIdMetadataKey is the gRPC metadata key used to propagate the trace id
// between processes (see grpc.md).
const TraceIdMetadataKey = "trace_id"

// WithTraceId returns a ctx carrying the given trace id.
func WithTraceId(ctx context.Context, traceId string) context.Context {
	return context.WithValue(ctx, traceIdCtxKey, traceId)
}

// TraceIdFromCtx extracts the trace id from ctx, if present and non-empty.
func TraceIdFromCtx(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	traceId, ok := ctx.Value(traceIdCtxKey).(string)
	if !ok || traceId == "" {
		return "", false
	}
	return traceId, true
}

// NewTraceId mints a new trace id. Entry points create one (dnvctl per CLI
// invocation, dnv-worker per sync/health round, dnv-gateway for a request
// that arrived without one); the interceptors never invent one (grpc.md T4).
func NewTraceId() string {
	var b [8]byte
	// crypto/rand.Read never returns an error (it panics on failure since
	// Go 1.24), so the result is always a full 8 random bytes.
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Handler (log.md R2, R12)
// ---------------------------------------------------------------------------

// TraceIdHandler wraps another slog.Handler and adds the trace_id attribute
// from the context to every record.
type TraceIdHandler struct {
	slog.Handler
}

func (h *TraceIdHandler) Handle(ctx context.Context, r slog.Record) error {
	if traceId, ok := TraceIdFromCtx(ctx); ok {
		r.AddAttrs(slog.String(TraceIdLogKey, traceId))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs / WithGroup must re-wrap so that derived loggers
// (logger.With(...), logger.WithGroup(...)) keep trace-id injection (R12).
func (h *TraceIdHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceIdHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *TraceIdHandler) WithGroup(name string) slog.Handler {
	return &TraceIdHandler{Handler: h.Handler.WithGroup(name)}
}

// logLevel's zero value is slog.LevelInfo, the level used by dnv-gateway,
// dnv-worker, dnv-agent and dnv-cdc (log.md R6).
var logLevel = new(slog.LevelVar)

// SetLogLevel changes the process-wide log level. dnvctl calls
// SetLogLevel(slog.LevelWarn) as the first statement of main(); the daemons
// do not call it.
func SetLogLevel(l slog.Level) {
	logLevel.Set(l)
}

func init() {
	baseHandler := slog.NewJSONHandler(
		os.Stdout,
		&slog.HandlerOptions{Level: logLevel},
	)
	slog.SetDefault(slog.New(&TraceIdHandler{Handler: baseHandler}))
}

// ---------------------------------------------------------------------------
// Shared logging helpers (log.md R10, R11)
// ---------------------------------------------------------------------------

// TruncForLog prepares string file data for logging: valid UTF-8 is truncated
// to the first LogStrDataLimit characters; anything else is reduced to its
// size (log.md R11).
func TruncForLog(s string) string {
	if !utf8.ValidString(s) {
		return fmt.Sprintf("<binary %d bytes>", len(s))
	}
	runes := []rune(s)
	if len(runes) <= LogStrDataLimit {
		return s
	}
	return fmt.Sprintf(
		"%s...(%d chars total)",
		string(runes[:LogStrDataLimit]),
		len(runes),
	)
}

// PbToLogValue renders a protobuf message as a JSON-marshalable value for
// logging (use as slog.Any("data", PbToLogValue(msg))). Every bytes field, at
// any nesting depth, is replaced by the string "<N bytes>" so raw payloads
// never reach the logs (log.md R10). Enum values are rendered by name. Only
// populated fields appear.
func PbToLogValue(msg proto.Message) any {
	if msg == nil {
		return nil
	}
	refMsg := msg.ProtoReflect()
	if !refMsg.IsValid() {
		return nil
	}
	return pbMsgToAny(refMsg)
}

func pbMsgToAny(m protoreflect.Message) map[string]any {
	out := make(map[string]any)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		out[string(fd.Name())] = pbFieldToAny(fd, v)
		return true
	})
	return out
}

func pbFieldToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch {
	case fd.IsMap():
		mp := make(map[string]any)
		valFd := fd.MapValue()
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			// MapKey.String() renders non-string keys too.
			mp[k.String()] = pbScalarToAny(valFd, mv)
			return true
		})
		return mp
	case fd.IsList():
		lst := v.List()
		arr := make([]any, 0, lst.Len())
		for i := 0; i < lst.Len(); i++ {
			arr = append(arr, pbScalarToAny(fd, lst.Get(i)))
		}
		return arr
	default:
		return pbScalarToAny(fd, v)
	}
}

func pbScalarToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch fd.Kind() {
	case protoreflect.BytesKind:
		return fmt.Sprintf("<%d bytes>", len(v.Bytes()))
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return pbMsgToAny(v.Message())
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
			return string(ev.Name())
		}
		return int32(v.Enum())
	default:
		// Bool, ints, uints, floats, string: native Go value; the JSON
		// handler marshals uint64 exactly (no precision loss).
		return v.Interface()
	}
}
