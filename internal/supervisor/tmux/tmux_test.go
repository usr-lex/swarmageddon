package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunWaitsForExit(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	s := &Session{Name: "t1", Socket: "swarm-test-" + filepath.Base(dir), Env: []string{"GREETING=it's here"}, EnvFile: filepath.Join(dir, "env")}
	defer s.tmux("kill-server").Run()
	out := filepath.Join(dir, "out")
	start := time.Now()
	err := s.Run(context.Background(), []string{"sh", "-c", `sleep 0.5; echo "$GREETING" > ` + out})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Error("returned before the command ended")
	}
	b, _ := os.ReadFile(out)
	if string(b) != "it's here\n" {
		t.Errorf("env not passed: %q", b)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, []string{"sleep", "60"}) }()
	time.Sleep(300 * time.Millisecond)
	if !s.Exists() {
		t.Fatal("session not running")
	}
	if _, err := s.Capture(); err != nil {
		t.Errorf("capture: %v", err)
	}
	if err := s.SendKeys("x"); err != nil {
		t.Errorf("send-keys: %v", err)
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped")
	}
	if s.Exists() {
		t.Error("session still exists")
	}
}

// A restarted command runs in the same session, so a person attached to it
// (the control center's `tmux attach -t agent`) stays attached instead of
// seeing the session end (OpenShift report, issue 1: Ctrl+C under Take
// control quit ore's TUI, and the restart replaced the session).
func TestRunKeepsTheSessionAcrossRestarts(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	s := &Session{Name: "t2", Socket: "swarm-test-" + filepath.Base(dir)}
	defer s.tmux("kill-server").Run()
	id := func() string {
		t.Helper()
		out, err := s.tmux("display-message", "-p", "-t", "="+s.Name+":", "#{session_id}").Output()
		if err != nil {
			t.Fatalf("session gone: %v", err)
		}
		return string(out)
	}
	if err := s.Run(context.Background(), []string{"true"}); err != nil {
		t.Fatal(err)
	}
	first := id()
	out := filepath.Join(dir, "second")
	if err := s.Run(context.Background(), []string{"sh", "-c", "echo ran > " + out}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "ran\n" {
		t.Fatalf("second command didn't run: %q", b)
	}
	if got := id(); got != first {
		t.Fatalf("session replaced: %s then %s", first, got)
	}
}
