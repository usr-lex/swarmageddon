package terminal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/redact"
)

func TestInputRecorder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	r := newInputRecorder(func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	for _, k := range []string{"l", "s", " -la", "\r", "vim", strings.Repeat("x", recordMax)} {
		r.write([]byte(k))
	}
	r.write([]byte("tail"))
	r.Close()
	r.write([]byte("late"))
	if strings.Join(got, "|") != "ls -la\r|vim"+strings.Repeat("x", recordMax)+"|tail" {
		t.Fatalf("chunks %q", got)
	}
}

func TestTerminalRecordsInput(t *testing.T) {
	redact.Add("ghp_terminal_redact")
	op := &auth.Principal{Role: auth.RoleOperator, Email: "owner@x", Handle: "owner"}
	for _, tc := range []struct {
		mode   string
		record bool
	}{{"shell", true}, {"view", true}, {"shell", false}} {
		audits := &auditLog{}
		h := &Handler{
			Principal:   func(http.ResponseWriter, *http.Request) *auth.Principal { return op },
			Owners:      func(context.Context, string) ([]string, error) { return []string{"owner@x"}, nil },
			Exec:        &echoExec{},
			Audit:       func(_ context.Context, ev store.AuditEvent) { audits.add(ev) },
			RecordInput: func(_ context.Context, ws string) bool { return tc.record && ws == "acme" },
		}
		srv := httptest.NewServer(http.HandlerFunc(h.ServeTerminal))
		t.Cleanup(srv.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, _, err := websocket.Dial(ctx, wsURL(srv, "/?workspace=acme&room=athena&mode="+tc.mode), nil)
		if err != nil {
			t.Fatal(err)
		}
		want := tc.record && tc.mode != "view"
		if want {
			typ, data, err := c.Read(ctx)
			if err != nil || typ != websocket.MessageText || !strings.Contains(string(data), `"recording"`) {
				t.Fatalf("recording notice %v %q %v", typ, data, err)
			}
		}
		c.Write(ctx, websocket.MessageBinary, []byte("export T=ghp_terminal_redact\r"))
		if _, data, err := c.Read(ctx); err != nil || !strings.HasPrefix(string(data), "EXPORT") {
			t.Fatalf("echo %q %v", data, err)
		}
		c.Write(ctx, websocket.MessageBinary, []byte("exit"))
		c.Read(ctx)
		c.CloseNow()
		cancel()

		n := 2
		if want {
			n = 4
		}
		evs := audits.wait(n)
		var inputs []string
		for _, ev := range evs {
			if ev.Action == "terminal.input" {
				var d map[string]string
				_ = json.Unmarshal([]byte(ev.Detail), &d)
				inputs = append(inputs, d["input"])
			}
		}
		if !want {
			if len(inputs) != 0 || strings.Contains(evs[0].Detail, "recording") {
				t.Fatalf("%s record=%v: recorded %+v", tc.mode, tc.record, evs)
			}
			continue
		}
		if !strings.Contains(evs[0].Detail, `"recording":true`) || strings.Join(inputs, "|") != "export T=[redacted]\r|exit" || evs[len(evs)-1].Action != "terminal.close" {
			t.Fatalf("audits %+v", evs)
		}
	}
}
