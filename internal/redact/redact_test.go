package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func logger(r *Registry) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(NewHandler(slog.NewJSONHandler(&buf, nil), r)), &buf
}

func TestHandler(t *testing.T) {
	r := &Registry{}
	log, buf := logger(r)
	log.Info("before sk-live-0123456789")
	if !strings.Contains(buf.String(), "sk-live-0123456789") {
		t.Fatalf("nothing registered yet: %s", buf)
	}
	r.Add("sk-live-0123456789\n", "short", "sk-live-0123456789-and-more")
	buf.Reset()
	log.With("key", "x=sk-live-0123456789").WithGroup("g").Info("got sk-live-0123456789-and-more",
		"url", "https://hooks.example/sk-live-0123456789",
		"err", errors.New("dial: sk-live-0123456789: refused"),
		"body", []byte("sk-live-0123456789"),
		slog.Group("inner", "v", "sk-live-0123456789"),
		"n", 42, "word", "short", "plain", "fine")
	out := buf.String()
	if strings.Contains(out, "sk-live") || strings.Contains(out, "-and-more") {
		t.Fatalf("secret in log: %s", out)
	}
	for _, want := range []string{`"msg":"got [redacted]"`, `"key":"x=[redacted]"`, `"url":"https://hooks.example/[redacted]"`,
		`"err":"dial: [redacted]: refused"`, `"inner":{"v":"[redacted]"}`, `"n":42`, `"word":"short"`, `"plain":"fine"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

func TestRegistryString(t *testing.T) {
	r := &Registry{}
	if r.String("abcdefghij") != "abcdefghij" {
		t.Fatal("empty registry changed text")
	}
	r.Add("abcdefgh", "abcdefghijkl")
	if got := r.String("x abcdefghijkl y abcdefgh"); got != "x [redacted] y [redacted]" {
		t.Fatalf("got %q", got)
	}
}

func TestConcurrent(t *testing.T) {
	r := &Registry{}
	log, _ := logger(r)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 50 {
				r.Add(strings.Repeat(string(rune('a'+i)), 8+j))
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				log.Info("aaaaaaaaaaaa", "v", "bbbbbbbbbbbbb")
			}
		}()
	}
	wg.Wait()
}

func BenchmarkHandle(b *testing.B) {
	r := &Registry{}
	for i := range 50 {
		r.Add(strings.Repeat(string(rune('a'+i%26)), 20+i))
	}
	log := slog.New(NewHandler(slog.NewJSONHandler(discard{}, nil), r))
	for b.Loop() {
		log.Info("reconciled room", "room", "athena@acme", "err", errors.New("connection refused"), "n", 3)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
