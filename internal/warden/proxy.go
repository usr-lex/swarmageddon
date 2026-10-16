// Package warden is the gateway guard (ADR 0011, layer 4): a reverse proxy
// in front of LiteLLM that every room's model requests pass through. It
// forwards each request unchanged, streaming included, and scores a copy
// asynchronously: the rule table over the tool calls the transcript carries,
// and a periodic review by a small model. It refuses the requests of
// quarantined rooms, quarantines rooms on critical verdicts, and reports
// verdicts to messaging.
//
// The proxy is stateless across replicas: the key table comes from a
// ConfigMap, and per-room scoring state (seen items, recent calls, the
// rolling summary, rates) lives in memory. A restarted replica rebuilds it
// from the next request, which carries the whole transcript; it scores only
// that request's last few items, so old events are not reported again. The
// gateway Service uses ClientIP session affinity so a room's requests reach
// one replica and its state isn't split.
package warden

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
	"github.com/usr-lex/swarmageddon/internal/warden/transcript"
)

// DefaultMaxBody is how much of a request body is buffered for scoring.
// Larger requests are proxied without being scored.
const DefaultMaxBody = 8 << 20

// Proxy is the warden's HTTP handler.
type Proxy struct {
	// Upstream is LiteLLM's base URL.
	Upstream *url.URL
	// Scorer receives copies of rooms' requests; nil scores nothing.
	Scorer *Scorer
	// MaxBody overrides DefaultMaxBody.
	MaxBody int64
	Log     *slog.Logger
	Now     func() time.Time

	table atomic.Pointer[keytable.Table]
	rp    *httputil.ReverseProxy
	once  sync.Once

	mu sync.Mutex
	// held are rooms this replica quarantined, until the table says so too.
	held map[string]time.Time
}

// holdFor bounds how long a local quarantine waits for the table.
const holdFor = 10 * time.Minute

// SetTable replaces the key table.
func (p *Proxy) SetTable(t *keytable.Table) {
	p.table.Store(t)
	p.mu.Lock()
	defer p.mu.Unlock()
	for room := range p.held {
		if e := t.Room(room); e != nil && e.Quarantined {
			delete(p.held, room) // the table has caught up and is authoritative now
		}
	}
}

// Table returns the current key table.
func (p *Proxy) Table() *keytable.Table { return p.table.Load() }

// Hold refuses a room's requests at once, before the table reflects its
// quarantine.
func (p *Proxy) Hold(room string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = map[string]time.Time{}
	}
	p.held[room] = p.now()
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proxy) quarantined(e *keytable.Entry) bool {
	if e.Quarantined {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.held[e.Room]
	return ok && p.now().Sub(at) < holdFor
}

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Proxy) proxy() *httputil.ReverseProxy {
	p.once.Do(func() {
		p.rp = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(p.Upstream)
			},
			FlushInterval: -1, // stream server-sent events as they come
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				p.log().Warn("upstream error", "path", r.URL.Path, "err", err)
				writeError(w, r.URL.Path, http.StatusBadGateway, "api_error", "bad_gateway", "the model gateway is unavailable")
			},
		}
	})
	return p.rp
}

// APIKey returns the gateway key a request carries: a bearer token, or the
// x-api-key or api-key header.
func APIKey(r *http.Request) string {
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	return r.Header.Get("Api-Key")
}

// ServeHTTP proxies one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ref, known := p.Table().Lookup(APIKey(r))
	if known && p.quarantined(ref.Entry) {
		writeQuarantined(w, r.URL.Path, ref.Entry.Room)
		return
	}
	if known && ref.Kind == keytable.KindMain && p.Scorer != nil && r.Method == http.MethodPost &&
		transcript.APIOf(r.URL.Path) != "" && ref.Entry.Mode != "off" {
		if body, ok := p.buffer(r); ok {
			p.Scorer.Enqueue(Job{Entry: ref.Entry, Path: r.URL.Path, Body: body})
		}
	}
	p.proxy().ServeHTTP(w, r)
}

// buffer reads up to MaxBody bytes of r's body and puts them back. It
// reports false (and leaves the body intact) when the body is larger.
func (p *Proxy) buffer(r *http.Request) ([]byte, bool) {
	limit := p.MaxBody
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	if r.ContentLength > limit {
		return nil, false
	}
	orig := r.Body
	buf, err := io.ReadAll(io.LimitReader(orig, limit+1))
	if err != nil {
		// Hand what we have plus the rest upstream; it reports the error.
		r.Body = readCloser{io.MultiReader(bytes.NewReader(buf), orig), orig}
		return nil, false
	}
	if int64(len(buf)) > limit {
		r.Body = readCloser{io.MultiReader(bytes.NewReader(buf), orig), orig}
		return nil, false
	}
	_ = orig.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

type readCloser struct {
	io.Reader
	io.Closer
}

// QuarantineMessage is what a quarantined room's agent reads.
func QuarantineMessage(room string) string {
	return "room " + room + " is quarantined by the warden: its model requests are refused until a person releases it in the control center"
}

func writeQuarantined(w http.ResponseWriter, path, room string) {
	writeError(w, path, http.StatusForbidden, "permission_error", "room_quarantined", QuarantineMessage(room))
}

// writeError writes an error in the shape of the API the path belongs to:
// Anthropic's {"type":"error","error":{...}} for /v1/messages, OpenAI's
// {"error":{...}} otherwise.
func writeError(w http.ResponseWriter, path string, status int, typ, code, msg string) {
	var body any
	if transcript.APIOf(path) == transcript.Anthropic || strings.Contains(path, "/messages") {
		body = map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}}
	} else {
		body = map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code, "param": nil}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
