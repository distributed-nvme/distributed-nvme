package common

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

// NewTraceId mints a new trace id for the entry points of grpc.md T4; the
// shared interceptors never mint one.
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

// logLevel's zero value is slog.LevelInfo, the default of --log-level on
// dnv-gateway, dnv-worker, dnv-agent, dnv-cdc and dnv-csi (log.md R6).
var logLevel = new(slog.LevelVar)

// SetLogLevel changes the process-wide log level. dnvctl calls
// SetLogLevel(slog.LevelWarn) as the first statement of main() and then sets
// its --log-level; each daemon sets its --log-level before its first record;
// the node check and the gather set Warn (log.md R6).
func SetLogLevel(l slog.Level) {
	logLevel.Set(l)
}

// LogLevel is the process-wide log level now.
func LogLevel() slog.Level {
	return logLevel.Level()
}

// The three words --log-level takes (log.md R6). There is no "error": a Warn
// record is one an operator must look at, and a level that hid it would also
// hide the "log level changed" record of WatchLogLevel.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
)

// ParseLogLevel reads a --log-level value, from a flag, the environment or a
// config file alike: one of the three words, in any case, spaces around it
// ignored. Anything else is refused with an error that names the words and
// the value as given.
func ParseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case LogLevelDebug:
		return slog.LevelDebug, nil
	case LogLevelInfo:
		return slog.LevelInfo, nil
	case LogLevelWarn:
		return slog.LevelWarn, nil
	}
	return 0, fmt.Errorf("--log-level must be %s, %s or %s, got %q",
		LogLevelDebug, LogLevelInfo, LogLevelWarn, raw)
}

// LogLevelName is the word of a level, as --log-level spells it.
func LogLevelName(l slog.Level) string {
	return strings.ToLower(l.String())
}

// LogEnabled says whether a record at level reaches the log. A logging point
// asks it before it builds its attributes, so a record the level drops costs
// no rendering (log.md R9).
func LogEnabled(ctx context.Context, level slog.Level) bool {
	return slog.Default().Enabled(ctx, level)
}

// ReadLevel is the level of a read's record (log.md R9): Debug when the read
// succeeded, Info when it failed, so a failure shows at the default level.
func ReadLevel(err error) slog.Level {
	if err == nil {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// MsgLogLevelChanged is the record of a log level change by signal (log.md
// R13).
const MsgLogLevelChanged = "log level changed"

// WatchLogLevel is a daemon's change of its log level while it runs (log.md
// R13): SIGUSR1 sets Debug and SIGUSR2 sets start, the level the daemon
// started with. Each signal logs MsgLogLevelChanged at Warn, a level every
// --log-level shows, under ctx's trace id. The returned stop ends the watch
// and waits for its goroutine; after it a SIGUSR1 or SIGUSR2 does nothing,
// as before the watch, since the Go runtime catches both and takes no action
// when no channel is notified.
func WatchLogLevel(ctx context.Context, start slog.Level) (stop func()) {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGUSR2)
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchLogLevel(wctx, start, sigCh)
	}()
	return func() {
		signal.Stop(sigCh)
		cancel()
		<-done
	}
}

// watchLogLevel is WatchLogLevel's loop over the signals sigCh delivers,
// until ctx ends. Both signals set a level outright rather than a step up or
// down, so the result never depends on a level the operator cannot see: two
// SIGUSR1 still mean Debug, and SIGUSR2 always goes back to start. Every
// signal is logged, a repeat too, so the operator can see that the signal
// reached the process.
func watchLogLevel(
	ctx context.Context,
	start slog.Level,
	sigCh <-chan os.Signal,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-sigCh:
			to := start
			if sig == syscall.SIGUSR1 {
				to = slog.LevelDebug
			}
			from := logLevel.Level()
			logLevel.Set(to)
			slog.WarnContext(ctx, MsgLogLevelChanged,
				slog.String("log_level", LogLevelName(to)),
				slog.String("from", LogLevelName(from)),
				slog.String("signal", sig.String()),
			)
		}
	}
}

// init installs the one handler chain every dnv binary logs through. The
// stream is STDERR, the kubernetes convention: klog defaults to stderr, and
// kubectl reserves stdout for the result a script pipes. Applied here it means
// stdout is the payload channel of every dnv binary — dnvctl's one result
// document, and nothing from a running daemon (cobra's own --help text is all
// five ever put there) — so a log record can never be mistaken for output.
// The handler holds logLevel (a *slog.LevelVar) rather than a fixed level, so
// SetLogLevel keeps biting after init.
func init() {
	baseHandler := slog.NewJSONHandler(
		os.Stderr,
		&slog.HandlerOptions{Level: logLevel},
	)
	slog.SetDefault(slog.New(&TraceIdHandler{Handler: baseHandler}))
}

// ---------------------------------------------------------------------------
// Shared logging helpers (log.md R10, R11)
// ---------------------------------------------------------------------------

// TruncForLog prepares string file data for logging: valid UTF-8 is truncated
// to the first LogStrDataLimit characters; anything else is reduced to its
// size (log.md R11). It counts the characters in place and copies only the
// ones it keeps, since a file read logs its whole data through it and the
// cn agent reads a thin_dump document of tens of MB that way (readThinDump,
// agent/cnagent/thinbm.go).
func TruncForLog(s string) string {
	if !utf8.ValidString(s) {
		return fmt.Sprintf("<binary %d bytes>", len(s))
	}
	total := utf8.RuneCountInString(s)
	if total <= LogStrDataLimit {
		return s
	}
	cut, kept := 0, 0
	for i := range s {
		if kept == LogStrDataLimit {
			cut = i
			break
		}
		kept++
	}
	return fmt.Sprintf("%s...(%d chars total)", s[:cut], total)
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
