package proc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
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
