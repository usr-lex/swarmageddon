package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/getsentry/sentry-go"
	sentryhttpclient "github.com/getsentry/sentry-go/httpclient"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/usr-lex/swarmageddon/internal/redact"
)

// fakeTransport keeps what the SDK would send, and its JSON: everything
// that would reach Sentry.
type fakeTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
	raw    bytes.Buffer
}

func (f *fakeTransport) Configure(sentry.ClientOptions)        {}
func (f *fakeTransport) Flush(time.Duration) bool              { return true }
func (f *fakeTransport) FlushWithContext(context.Context) bool { return true }
func (f *fakeTransport) Close()                                {}

func (f *fakeTransport) SendEvent(e *sentry.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	f.raw.Write(b) // a log batch's records are its "items"
	f.raw.WriteByte('\n')
}

// waitFor waits until an event matching pred is sent: a server finishes
// its transaction after the response is written.
func (f *fakeTransport) waitFor(t *testing.T, pred func(*sentry.Event) bool) {
	t.Helper()
	for range 250 {
		f.mu.Lock()
		e := eventWith(f.events, pred)
		f.mu.Unlock()
		if e != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("event not sent")
}

func (f *fakeTransport) sent(t *testing.T) ([]*sentry.Event, string) {
	t.Helper()
	sentry.Flush(time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events, f.raw.String()
}

// capture turns reporting on with a fake transport.
func capture(t *testing.T, rate float64) *fakeTransport {
	t.Helper()
	tr := &fakeTransport{}
	if err := start("test", Config{DSN: "https://public@sentry.example.invalid/1", Environment: "test", TracesSampleRate: rate}, tr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enabled.Store(false) })
	return tr
}

func eventWith(evs []*sentry.Event, pred func(*sentry.Event) bool) *sentry.Event {
	for _, e := range evs {
		if pred(e) {
			return e
		}
	}
	return nil
}

// TestSecretsNeverReachTheTransport registers a secret and puts it in an
// error message, a log attribute, a breadcrumb, request headers, a query, a
// body, an outgoing request's headers and a Connect error, then checks that
// none of what the SDK sends contains it.
func TestSecretsNeverReachTheTransport(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	tr := capture(t, 1)
	// No redact.Handler: the SDK hooks alone must redact.
	log := slog.New(LogHandler(slog.NewJSONHandler(io.Discard, nil)))

	// A breadcrumb recorded before the secret is registered is redacted
	// when an event carries it. (Log records, like the console, are
	// redacted when they are logged.)
	sentry.AddBreadcrumb(&sentry.Breadcrumb{Message: "early breadcrumb " + secret})
	redact.Add(secret)

	log.Info("breadcrumb with "+secret, "detail", secret)
	log.Warn("log attribute", "token", secret, slog.Group("g", "inner", "x="+secret))
	log.Error("dial failed", "err", fmt.Errorf("dial with key %s: refused", secret), "room", "athena")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	out := &http.Client{Transport: sentryhttpclient.NewSentryRoundTripper(nil)}

	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/", func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL+"/v1/x?key="+secret, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-Other", secret)
		if res, err := out.Do(req); err == nil {
			res.Body.Close()
		}
		log.ErrorContext(r.Context(), "hook failed", "err", errors.New("bad hook "+secret))
		http.Error(w, "no", http.StatusInternalServerError)
	})
	mux.Handle("/test.v1.Svc/", connect.NewUnaryHandler("/test.v1.Svc/Fail",
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			return nil, connect.NewError(connect.CodeInternal, errors.New("database said "+secret))
		}, connect.WithInterceptors(Connect())))
	srv := httptest.NewServer(Handler(mux))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hooks/acme?token="+secret+"&q="+secret, strings.NewReader(`{"password":"`+secret+`"}`))
	for k, v := range map[string]string{
		"Authorization": "Bearer " + secret, "Proxy-Authorization": "Basic " + secret, "Cookie": "session=" + secret,
		"X-Swarm-On-Behalf-Of": "ada@acme", "X-Api-Key": secret, "X-Custom": "v=" + secret,
	} {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	c := connect.NewClient[emptypb.Empty, emptypb.Empty](http.DefaultClient, srv.URL+"/test.v1.Svc/Fail")
	if _, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("connect call: %v", err)
	}
	for _, name := range []string{"POST /hooks/acme", "POST /test.v1.Svc/Fail"} {
		tr.waitFor(t, func(e *sentry.Event) bool { return e.Transaction == name })
	}

	evs, raw := tr.sent(t)
	if strings.Contains(raw, secret) || strings.Contains(raw, "0123456789abcdef") {
		t.Fatalf("the secret reached the transport:\n%s", raw)
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Swarm-On-Behalf-Of", "ada@acme", "X-Api-Key", "password"} {
		if strings.Contains(raw, h) {
			t.Errorf("%s was sent", h)
		}
	}
	// And what should arrive did, redacted.
	for _, want := range []string{"early breadcrumb [redacted]", "dial with key [redacted]: refused", `"token":{"value":"[redacted]"`,
		`"g.inner":{"value":"x=[redacted]"`, `"X-Custom":"v=[redacted]"`, "bad hook [redacted]", "database said [redacted]", `"component":"test"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %s in\n%s", want, raw)
		}
	}

	hook := eventWith(evs, func(e *sentry.Event) bool { return e.Message == "hook failed" })
	if hook == nil || hook.Request == nil || hook.Request.Data != "" || hook.Request.Cookies != "" {
		t.Fatalf("hook event: %+v", hook)
	}
	if hook.Contexts["log"] != nil && hook.Contexts["log"]["err"] != nil {
		t.Error("the error is the exception, not also data")
	}
	rpc := eventWith(evs, func(e *sentry.Event) bool { return e.Tags["rpc.procedure"] == "/test.v1.Svc/Fail" })
	if rpc == nil || rpc.Tags["rpc.code"] != "internal" || rpc.Level != sentry.LevelError {
		t.Fatalf("connect event: %+v", rpc)
	}
	// Transactions: named by URL path (the procedure, not the mux's
	// pattern), with the outgoing request as a span.
	var names []string
	for _, e := range evs {
		if e.Type == "transaction" {
			names = append(names, e.Transaction)
		}
	}
	if !strings.Contains(strings.Join(names, ","), "POST /test.v1.Svc/Fail") || !strings.Contains(strings.Join(names, ","), "POST /hooks/acme") {
		t.Errorf("transactions %v", names)
	}
	if !strings.Contains(raw, `"op":"http.client"`) {
		t.Error("no span for the outgoing request")
	}
}

// TestSentryHTTPTransport runs Sentry's own transport (the one production
// uses) against a fake Sentry: an event and a log record arrive, redacted.
func TestSentryHTTPTransport(t *testing.T) {
	const secret = "sk-http-0123456789abcdef"
	var mu sync.Mutex
	var got bytes.Buffer
	sentrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got.Write(b)
		mu.Unlock()
	}))
	defer sentrySrv.Close()
	u, _ := url.Parse(sentrySrv.URL)
	if err := start("test", Config{DSN: "http://public@" + u.Host + "/1"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enabled.Store(false) })
	redact.Add(secret)
	log := slog.New(LogHandler(slog.NewJSONHandler(io.Discard, nil)))
	log.Error("over http", "err", errors.New("key "+secret), "detail", secret)
	if !sentry.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	mu.Lock()
	defer mu.Unlock()
	s := got.String()
	if strings.Contains(s, secret) || !strings.Contains(s, "key [redacted]") || !strings.Contains(s, `"body":"over http"`) {
		t.Fatalf("sent:\n%s", s)
	}
}

func TestLogEvents(t *testing.T) {
	tr := capture(t, 0)
	var buf bytes.Buffer
	log := slog.New(redact.NewHandler(LogHandler(slog.NewJSONHandler(&buf, nil)), nil))
	log.Debug("not logged, not sent")
	log.Info("first step", "n", 1)
	log.With("ws", "acme").WithGroup("req").Error("lost", "id", 7, "err", errors.New("boom"))
	log.Error("no error value", "why", "unknown")

	if strings.Count(buf.String(), "\n") != 3 {
		t.Fatalf("the console handler got:\n%s", buf.String())
	}
	evs, raw := tr.sent(t)
	lost := eventWith(evs, func(e *sentry.Event) bool { return e.Message == "lost" })
	if lost == nil || len(lost.Exception) != 1 || lost.Exception[0].Value != "boom" || lost.Level != sentry.LevelError {
		t.Fatalf("lost: %+v", lost)
	}
	if d := lost.Contexts["log"]; d["ws"] != "acme" || d["req.id"] != int64(7) {
		t.Errorf("data %v", d)
	}
	if b := lost.Breadcrumbs; len(b) == 0 || b[len(b)-1].Message != "first step" {
		t.Errorf("breadcrumbs %+v", b)
	}
	// The stack ends in this test, not in slog or the handlers.
	st := lost.Exception[0].Stacktrace
	if st == nil || !strings.HasSuffix(st.Frames[len(st.Frames)-1].Function, "TestLogEvents") {
		t.Fatalf("stack %+v", st)
	}
	msg := eventWith(evs, func(e *sentry.Event) bool { return e.Message == "no error value" })
	if msg == nil || len(msg.Threads) != 1 || len(msg.Exception) != 0 {
		t.Fatalf("message event %+v", msg)
	}
	if strings.Contains(raw, "not logged") {
		t.Error("a debug record was sent")
	}
	if !strings.Contains(raw, `"body":"first step"`) {
		t.Errorf("no Sentry Log for the info record:\n%s", raw)
	}
}

func TestZapCore(t *testing.T) {
	tr := capture(t, 0)
	var buf bytes.Buffer
	console := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&buf), zapcore.InfoLevel)
	log := zap.New(console, zap.WrapCore(func(c zapcore.Core) zapcore.Core { return zapcore.NewTee(c, ZapCore(c)) })).Named("controller")
	log.Debug("below the console's level")
	log.Info("reconciling", zap.String("room", "athena"))
	log.With(zap.String("controller", "room")).Error("Reconciler error", zap.Error(errors.New("forgejo is down")))

	evs, raw := tr.sent(t)
	e := eventWith(evs, func(e *sentry.Event) bool { return e.Message == "Reconciler error" })
	if e == nil || e.Logger != "controller" || len(e.Exception) != 1 || e.Exception[0].Value != "forgejo is down" || e.Exception[0].Stacktrace != nil {
		t.Fatalf("event %+v", e)
	}
	if e.Contexts["log"]["controller"] != "room" || len(e.Breadcrumbs) == 0 || e.Breadcrumbs[len(e.Breadcrumbs)-1].Message != "reconciling" {
		t.Errorf("context %v, breadcrumbs %+v", e.Contexts["log"], e.Breadcrumbs)
	}
	if strings.Contains(raw, "below the console") || !strings.Contains(raw, `"body":"reconciling"`) {
		t.Errorf("sent:\n%s", raw)
	}
	if !strings.Contains(buf.String(), "Reconciler error") {
		t.Error("the console core lost the entry")
	}
}

func TestHandlerKeepsProxying(t *testing.T) {
	tr := capture(t, 1)
	got := make(chan [2]bool, 1)
	srv := httptest.NewServer(Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hijack := w.(http.Hijacker)
		_, flush := w.(http.Flusher)
		got <- [2]bool{hijack, flush}
	})))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	res, err := c.Get("http://deb.example.invalid/pool/main/secret-path")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	tr.waitFor(t, func(e *sentry.Event) bool { return e.Type == "transaction" })
	if w := <-got; !w[0] || !w[1] {
		t.Fatalf("hijacker, flusher: %v", w)
	}
	evs, _ := tr.sent(t)
	tx := eventWith(evs, func(e *sentry.Event) bool { return e.Type == "transaction" })
	if tx == nil || tx.Transaction != "GET (proxy)" {
		t.Fatalf("transaction %+v", tx)
	}
}

func TestRecoverPanic(t *testing.T) {
	tr := capture(t, 0)
	func() {
		defer func() { RecoverPanic(context.Background(), recover()) }()
		panic("reconcile blew up")
	}()
	evs, _ := tr.sent(t)
	if e := eventWith(evs, func(e *sentry.Event) bool { return e.Message == "reconcile blew up" }); e == nil || e.Level != sentry.LevelFatal {
		t.Fatalf("events %+v", evs)
	}
}

func TestOff(t *testing.T) {
	enabled.Store(false)
	t.Setenv(EnvDSN, "")
	flush, err := Init("test")
	if err != nil || Enabled() {
		t.Fatal(err, Enabled())
	}
	flush()
	mux := http.NewServeMux()
	next := slog.NewJSONHandler(io.Discard, nil)
	if Handler(mux) != http.Handler(mux) || LogHandler(next) != slog.Handler(next) || Browser() != nil {
		t.Fatal("not a no-op without a DSN")
	}
	if _, ok := ZapCore(zapcore.InfoLevel).(*zapCore); ok {
		t.Fatal("zap core without a DSN")
	}
}

func TestFromEnv(t *testing.T) {
	for in, want := range map[string]float64{"": DefaultTracesSampleRate, "0": 0, "1": 1, "0.25": 0.25} {
		t.Setenv(EnvTracesSampleRate, in)
		if c, err := FromEnv(); err != nil || c.TracesSampleRate != want {
			t.Errorf("%q: %v %v", in, c.TracesSampleRate, err)
		}
	}
	for _, bad := range []string{"x", "-0.1", "1.5"} {
		t.Setenv(EnvTracesSampleRate, bad)
		if _, err := FromEnv(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBrowser(t *testing.T) {
	capture(t, 0.5)
	b := Browser()
	if b == nil || b.DSN != "https://public@sentry.example.invalid/1" || b.Environment != "test" || b.TracesSampleRate != 0.5 || !strings.HasPrefix(b.Release, "swarmageddon@") {
		t.Fatalf("%+v", b)
	}
}
