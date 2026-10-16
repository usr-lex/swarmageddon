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
