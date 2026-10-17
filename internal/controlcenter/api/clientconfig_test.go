package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/telemetry"
)

// TestClientConfig: the web UI learns from /client-config.json, before
// sign-in, whether and how to report errors.
func TestClientConfig(t *testing.T) {
	e := newEnv(t)
	get := func() string {
		t.Helper()
		res, err := http.Get(e.ts.URL + "/client-config.json")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%d %s: %s", res.StatusCode, res.Header.Get("Content-Type"), b)
		}
		return string(b)
	}
	if got := get(); got != "{}\n" {
		t.Fatalf("off: %s", got)
	}
	e.srv.Cfg.Sentry = &telemetry.BrowserConfig{DSN: "https://pub@o1.ingest.example/2", Release: "swarmageddon@0.1.0", TracesSampleRate: 0.1}
	var body struct {
		Sentry telemetry.BrowserConfig `json:"sentry"`
	}
	if err := json.Unmarshal([]byte(get()), &body); err != nil || body.Sentry != *e.srv.Cfg.Sentry {
		t.Fatalf("on: %+v %v", body, err)
	}
}
