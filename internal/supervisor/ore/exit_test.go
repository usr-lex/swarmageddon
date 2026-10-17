package ore

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An app-server that exits while the driver waits for its socket (ore 1.161
// refusing its requirements) must end runOnce with that error, so the
// driver restarts it, rather than hang.
func TestRunOnceReturnsWhenAppServerExitsAtStart(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ore")
	script := "#!/bin/sh\necho 'Error: socket parent must be owned by the user or root' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Driver{
		Binary: bin, CodexHome: filepath.Join(dir, "home"), WorkDir: dir,
		Socket: filepath.Join(dir, "ore.sock"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.Session.EnvFile = filepath.Join(dir, "session", "env")
	d.ready = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- d.runOnce(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exited at start") {
			t.Fatalf("runOnce = %v, want an exited-at-start error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runOnce hung after the app-server exited")
	}
}
