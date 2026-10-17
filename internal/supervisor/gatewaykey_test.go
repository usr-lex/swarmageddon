package supervisor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

type restartAgent struct {
	agent.Agent
	mu   sync.Mutex
	vars []map[string]string
}

func (r *restartAgent) Restart(vars map[string]string) {
	r.mu.Lock()
	r.vars = append(r.vars, vars)
	r.mu.Unlock()
}

func (r *restartAgent) calls() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string(nil), r.vars...)
}

func TestWatchGatewayKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway_key")
	write := func(s string) { os.WriteFile(file, []byte(s), 0o600) }
	read := func() string {
		b, _ := os.ReadFile(file)
		return strings.TrimSpace(string(b))
	}
	write("sk-old\n")
	a := &restartAgent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchGatewayKey(ctx, read, "sk-old", a, "ANTHROPIC_AUTH_TOKEN", 5*time.Millisecond, slog.New(slog.DiscardHandler))
	wait := func(n int) []map[string]string {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for len(a.calls()) < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(30 * time.Millisecond) // and no more than n
		return a.calls()
	}
	if got := wait(0); len(got) != 0 {
		t.Fatalf("restarted without a change: %v", got)
	}
	write("") // a Secret being swapped in may read empty for a moment
	if got := wait(0); len(got) != 0 {
		t.Fatalf("restarted on an empty key: %v", got)
	}
	write("sk-new\n")
	got := wait(1)
	if len(got) != 1 || got[0]["ANTHROPIC_AUTH_TOKEN"] != "sk-new" {
		t.Fatalf("restarts %v", got)
	}
}

func TestSetEnv(t *testing.T) {
	env := []string{"A=1", "KEY=old", "B=2", "KEY=dup"}
	got := agent.SetEnv(env, map[string]string{"KEY": "new", "C": "3"})
	if strings.Join(got, " ") != "A=1 KEY=new B=2 C=3" || env[1] != "KEY=old" {
		t.Fatalf("got %q (env %q)", got, env)
	}
}
