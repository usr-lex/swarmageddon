package telemetry

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
	sentryzap "github.com/getsentry/sentry-go/zap"
	"go.uber.org/zap/zapcore"
)

// Since sentry-go 0.48 its logging integrations only send Sentry Logs, so
// events and breadcrumbs from log records are made here (report).

// LogHandler returns next with Sentry added, when reporting is on: records
// at Error and above become events, Info and Warn records breadcrumbs on the
// events that follow, and all of them Sentry Logs (sentryslog). next still
// gets every record unchanged, and decides which levels are logged. Put it
// inside redact.NewHandler, so Sentry gets the redacted record too.
func LogHandler(next slog.Handler) slog.Handler {
	if !Enabled() {
		return next
	}
	logs := sentryslog.Option{LogLevel: []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError, sentryslog.LevelFatal}}.
		NewSentryHandler(context.Background())
	return &logHandler{next: next, logs: logs}
}

type logHandler struct {
	next, logs slog.Handler
	// prefix holds the groups opened with WithGroup, as "a.b.".
	prefix string
	// attrs are those added with WithAttrs, flattened.
	attrs map[string]any
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	if r.Level < slog.LevelInfo {
		return err
	}
	_ = h.logs.Handle(ctx, r)
	data := maps.Clone(h.attrs)
	if data == nil {
		data = map[string]any{}
	}
	var cause error
	r.Attrs(func(a slog.Attr) bool {
		flatten(data, h.prefix, a, &cause)
		return true
	})
	var stack *sentry.Stacktrace
	if r.Level >= slog.LevelError {
		stack = callerStack()
	}
	report(ctx, slogLevel(r.Level), r.Message, "", cause, data, stack)
	return err
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next, c.logs = h.next.WithAttrs(as), h.logs.WithAttrs(as)
	c.attrs = maps.Clone(h.attrs)
	if c.attrs == nil {
		c.attrs = map[string]any{}
	}
	for _, a := range as {
		flatten(c.attrs, h.prefix, a, nil)
	}
	return &c
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next, c.logs = h.next.WithGroup(name), h.logs.WithGroup(name)
	c.prefix = h.prefix + name + "."
	return &c
}

// flatten adds a to data, groups as dotted keys. With cause set, the first
// error value goes there instead.
func flatten(data map[string]any, prefix string, a slog.Attr, cause *error) {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, g := range v.Group() {
			flatten(data, prefix, g, cause)
		}
		return
	case slog.KindString, slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool:
		if a.Key != "" {
			data[prefix+a.Key] = v.Any()
		}
		return
	}
	if err, ok := v.Any().(error); ok && cause != nil && *cause == nil {
		*cause = err
		return
	}
	if a.Key != "" {
		data[prefix+a.Key] = v.String()
	}
}

func slogLevel(l slog.Level) sentry.Level {
	switch {
	case l < slog.LevelWarn:
		return sentry.LevelInfo
	case l < slog.LevelError:
		return sentry.LevelWarning
	case l < sentryslog.LevelFatal:
		return sentry.LevelError
	}
	return sentry.LevelFatal
}

// callerStack is the stack of the code that logged: the frames outside
// log/slog and the handlers it called.
func callerStack() *sentry.Stacktrace {
	st := sentry.NewStacktrace()
	if st == nil {
		return nil
	}
	f := st.Frames // outermost first
	i := len(f)
	for i > 0 && f[i-1].Module != "log/slog" {
		i--
	}
	for i > 0 && f[i-1].Module == "log/slog" {
		i--
	}
	if i > 0 {
		st.Frames = f[:i]
	}
	return st
}

// ZapCore returns a zapcore.Core for the operator (controller-runtime logs
// through zap), to tee with its console core: entries at Error and above
// become events, Info and Warn breadcrumbs, and all of them Sentry Logs
// (sentryzap). It is enabled where enab is, from Info up. Without
// reporting, it is a no-op core.
func ZapCore(enab zapcore.LevelEnabler) zapcore.Core {
	if !Enabled() {
		return zapcore.NewNopCore()
	}
	return &zapCore{enab: enab, logs: sentryzap.NewSentryCore(context.Background(), sentryzap.Option{})}
}

type zapCore struct {
	enab   zapcore.LevelEnabler
	logs   zapcore.Core
	fields []zapcore.Field
}

func (c *zapCore) Enabled(l zapcore.Level) bool { return l >= zapcore.InfoLevel && c.enab.Enabled(l) }

func (c *zapCore) With(fs []zapcore.Field) zapcore.Core {
	return &zapCore{enab: c.enab, logs: c.logs.With(fs), fields: append(slices.Clip(c.fields), fs...)}
}

func (c *zapCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *zapCore) Write(e zapcore.Entry, fs []zapcore.Field) error {
	_ = c.logs.Write(e, fs)
	enc := zapcore.NewMapObjectEncoder()
	var cause error
	for _, f := range append(slices.Clip(c.fields), fs...) {
		if err, ok := f.Interface.(error); ok && f.Type == zapcore.ErrorType && cause == nil {
			cause = err
			continue
		}
		f.AddTo(enc)
	}
	level := sentry.LevelInfo
	switch {
	case e.Level >= zapcore.DPanicLevel:
		level = sentry.LevelFatal
	case e.Level == zapcore.ErrorLevel:
		level = sentry.LevelError
	case e.Level == zapcore.WarnLevel:
		level = sentry.LevelWarning
	}
	// No stack: the operator's errors are logged by controller-runtime's
	// reconcile loop, whose stack is the same for all of them; without one,
	// Sentry groups them by error instead.
	report(context.Background(), level, e.Message, e.LoggerName, cause, enc.Fields, nil)
	return nil
}

func (c *zapCore) Sync() error { return nil }

// report sends one log record to Sentry: an event at Error and above, a
// breadcrumb below. stack is where it was logged, if known.
func report(ctx context.Context, level sentry.Level, msg, logger string, cause error, data map[string]any, stack *sentry.Stacktrace) {
	h := hub(ctx)
	if level != sentry.LevelError && level != sentry.LevelFatal {
		h.AddBreadcrumb(&sentry.Breadcrumb{Category: "log", Level: level, Message: msg, Data: data}, nil)
		return
	}
	var ev *sentry.Event
	if cause != nil {
		ev = h.Client().EventFromException(cause, level)
		ev.Message = msg
		// The SDK adds the current stack, which is ours; use the caller's.
		if n := len(ev.Exception); n > 0 && sentry.ExtractStacktrace(cause) == nil {
			ev.Exception[n-1].Stacktrace = stack
		}
	} else {
		ev = h.Client().EventFromMessage(msg, level)
		if stack != nil {
			ev.Threads = []sentry.Thread{{Stacktrace: stack, Current: true}}
		}
	}
	ev.Logger = logger
	if len(data) > 0 {
		ev.Contexts = map[string]sentry.Context{"log": data}
	}
	h.CaptureEvent(ev)
}
