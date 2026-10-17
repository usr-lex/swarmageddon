// Package redact keeps secrets out of logs (docs/design.md §11.4). Components
// Add every secret they load or mint to a Registry, and log through a
// Handler, which replaces those values in messages and attribute values
// with [redacted].
package redact

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Placeholder replaces a secret.
const Placeholder = "[redacted]"

// MinLen is the shortest value redacted; shorter ones would match ordinary
// text.
const MinLen = 8

// Registry is a set of secret values. The zero value is ready to use.
type Registry struct {
	mu   sync.Mutex
	vals map[string]bool
	repl atomic.Pointer[strings.Replacer] // nil while empty
}

// Default is the process-wide registry, used by NewHandler(h, nil).
var Default = &Registry{}

// Add registers secret values in Default.
func Add(vals ...string) { Default.Add(vals...) }

// Add registers secret values. Surrounding whitespace is trimmed, and values
// shorter than MinLen are ignored.
func (r *Registry) Add(vals ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if len(v) < MinLen || r.vals[v] {
			continue
		}
		if r.vals == nil {
			r.vals = map[string]bool{}
		}
		r.vals[v] = true
		changed = true
	}
	if !changed {
		return
	}
	// Longest first, so a secret containing another is replaced whole.
	keys := make([]string, 0, len(r.vals))
	for v := range r.vals {
		keys = append(keys, v)
	}
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, k, Placeholder)
	}
	r.repl.Store(strings.NewReplacer(pairs...))
}

// String returns s with every registered value replaced.
func (r *Registry) String(s string) string {
	if p := r.repl.Load(); p != nil && len(s) >= MinLen {
		return p.Replace(s)
	}
	return s
}

// Handler wraps a slog.Handler and redacts records before passing them on.
type Handler struct {
	h slog.Handler
	r *Registry
}

// NewHandler wraps h; r nil means Default.
func NewHandler(h slog.Handler, r *Registry) *Handler {
	if r == nil {
		r = Default
	}
	return &Handler{h: h, r: r}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.h.Enabled(ctx, l) }

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	if h.r.repl.Load() == nil {
		return h.h.Handle(ctx, rec)
	}
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.h.Handle(ctx, out)
}

// WithAttrs implements slog.Handler. The attributes are redacted with the
// values registered now.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.r.repl.Load() != nil {
		attrs = slices.Clone(attrs)
		for i, a := range attrs {
			attrs[i] = h.attr(a)
		}
	}
	return &Handler{h: h.h.WithAttrs(attrs), r: h.r}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{h: h.h.WithGroup(name), r: h.r}
}

func (h *Handler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(h.r.String(v.String()))
	case slog.KindGroup:
		g := slices.Clone(v.Group())
		for i, ga := range g {
			g[i] = h.attr(ga)
		}
		a.Value = slog.GroupValue(g...)
	case slog.KindAny:
		// Errors, Stringers, byte slices and anything else print as text;
		// only one that holds a secret is replaced, by its redacted text.
		var s string
		switch x := v.Any().(type) {
		case error:
			s = x.Error()
		case fmt.Stringer:
			s = x.String()
		case []byte:
			s = string(x)
		default:
			s = fmt.Sprint(x)
		}
		if r := h.r.String(s); r != s {
			a.Value = slog.StringValue(r)
		} else {
			a.Value = v
		}
	default:
		a.Value = v
	}
	return a
}
