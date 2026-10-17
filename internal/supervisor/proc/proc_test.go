package proc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestSuperviseRestarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuf
	exits := make(chan struct{}, 10)
	go Supervise(ctx, Spec{
		Name: "echo", Path: "/bin/sh", Args: []string{"-c", "echo hello"},
		Output: &out, MinBackoff: 10 * time.Millisecond,
		OnExit: func(error, time.Duration) { exits <- struct{}{} },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for range 3 {
		select {
		case <-exits:
		case <-time.After(5 * time.Second):
			t.Fatal("no restart")
		}
	}
	cancel()
	if !strings.Contains(out.String(), "[echo] hello\n") {
		t.Errorf("output %q", out.String())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := Spec{Name: "sleep", Path: "/bin/sleep", Args: []string{"60"}}
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, s.Command(ctx)) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped")
	}
}

// A child that exits but leaves a grandchild holding its output pipe must
// not hang Wait (ore's app-server did this, and the supervisor stalled).
func TestWaitReturnsWhenAGrandchildHoldsThePipe(t *testing.T) {
	s := Spec{Name: "t", Path: "/bin/sh", Args: []string{"-c", "sleep 60 & echo started; exit 3"}, Output: io.Discard}
	cmd := s.Command(context.Background())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Wait still blocked 20 s after the child exited")
	}
}
