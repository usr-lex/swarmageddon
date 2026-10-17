package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/supervisor/tmux"
)

type countExits struct {
	*fakeEvents
	n atomic.Int32
}

func (c *countExits) Exited(error) { c.n.Add(1) }

// Restart gives a running CLI new variables (a rotated gateway key) by
// restarting it, without counting an exit.
func TestRestartWithNewKey(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "keys")
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"$ANTHROPIC_AUTH_TOKEN\" >> "+out+"\nexec sleep 30\n"), 0o755)
	sock := fmt.Sprintf("swarm-test-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", sock, "kill-server").Run() })
	ev := &countExits{fakeEvents: &fakeEvents{}}
	d := &Driver{Name: "athena", Binary: bin, Home: dir, WorkDir: dir, Events: ev,
		Env:     []string{"ANTHROPIC_AUTH_TOKEN=old-key"},
		Session: tmux.Session{Name: "agent", Socket: sock}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitKeys := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			b, _ := os.ReadFile(out)
			if got := strings.Fields(string(b)); strings.Join(got, ",") == want {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("keys %q, want %s", got, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitKeys("old-key")
	d.Restart(map[string]string{"ANTHROPIC_AUTH_TOKEN": "new-key"})
	waitKeys("old-key,new-key")
	if n := ev.n.Load(); n != 0 {
		t.Fatalf("%d exits reported", n)
	}
	cancel()
	<-done
}
