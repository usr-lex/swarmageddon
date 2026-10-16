//go:build canary

// Package canary runs the pinned agent CLIs against the mock model and checks
// the integration points the room supervisor relies on (ADR 0006, ADR 0007,
// design §14). Run with: go test -tags canary ./test/canary/...
//
// The CLIs are installed from npm into $SWARM_CANARY_CACHE (default
// ~/.cache/swarmageddon-canary). Managed settings and /etc/codex files are
// tested for real by running the CLI in a private mount namespace where a
// copy of /etc is mounted over /etc, which needs root and unshare(1).
package canary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/mockmodel"
	"github.com/usr-lex/swarmageddon/internal/supervisor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient/msgfake"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// Pinned versions (keep in step with the room image).
const (
	ClaudeCodeVersion = "2.1.301"
	OreVersion        = "1.161.0"
)

func cacheDir(t *testing.T) string {
	if d := os.Getenv("SWARM_CANARY_CACHE"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, "swarmageddon-canary")
}

// npmInstall installs pkg@version into its own prefix under the cache and
// returns the prefix.
func npmInstall(t *testing.T, pkg, version string) string {
	t.Helper()
	prefix := filepath.Join(cacheDir(t), strings.NewReplacer("/", "_", "@", "").Replace(pkg)+"-"+version)
	marker := filepath.Join(prefix, ".installed")
	if _, err := os.Stat(marker); err == nil {
		return prefix
	}
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("npm", "install", "--prefix", prefix, "--no-save", "--no-audit", "--no-fund", pkg+"@"+version)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm install %s@%s: %v\n%s", pkg, version, err, out)
	}
	os.WriteFile(marker, nil, 0o644)
	return prefix
}

// buildSwarm builds the swarm binary into a temp dir.
func buildSwarm(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	cmd := exec.Command("go", "build", "-o", dir, "./cmd/swarm")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build swarm: %v\n%s", err, out)
	}
	return filepath.Join(dir, "swarm")
}

// mock starts the mock model with a script on 127.0.0.1.
func mock(t *testing.T, script string) (*mockmodel.Server, string) {
	t.Helper()
	sc, err := mockmodel.ParseScript([]byte(script))
	if err != nil {
		t.Fatal(err)
	}
	m := mockmodel.New(sc)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv.URL
}

// room is a supervisor core with a fake messaging service and the room API
// on a Unix socket.
type room struct {
	core *supervisor.Core
	fake *msgfake.Server
	sock string
}

func newRoom(t *testing.T, ctx context.Context, cli string, a func(*supervisor.Core) agent.Agent) *room {
	t.Helper()
	fake := msgfake.New()
	fake.Self = "athena@acme"
	_, h := messagingv1connect.NewMessagingServiceHandler(fake)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	mc, _ := msgclient.New(srv.URL, msgclient.TLSFiles{})
	dedup, _ := msgclient.LoadDedup(filepath.Join(t.TempDir(), "seen"), 1000)
	core := &supervisor.Core{
		Room: &roomcfg.Room{Room: "athena", Workspace: "acme", CLI: cli, RoleTitle: "Canary"},
		CLI:  cli, Messaging: mc, Dedup: dedup, StatusDebounce: 100 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	core.Agent = a(core)
	sock := filepath.Join(shortTemp(t), "sv.sock")
	api := &supervisor.API{Core: core}
	go supervisor.ServeUnix(ctx, sock, -1, api.Handler())
	go core.Run(ctx)
	return &room{core: core, fake: fake, sock: sock}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// shortTemp is a temp dir with a short path (Unix socket paths are limited).
func shortTemp(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "cn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// waitFor polls cond (tests only) until it holds or the timeout ends.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// requestsWith returns mock requests for a marker turn and step.
func requestsWith(m *mockmodel.Server, turn string, step int) []mockmodel.Request {
	var out []mockmodel.Request
	for _, r := range m.Requests() {
		if r.Turn == turn && r.Step == step {
			out = append(out, r)
		}
	}
	return out
}

// etcOverlay prepares a copy of /etc with extra files, and a wrapper script
// that runs a binary in a private mount namespace with that copy as /etc.
// Skips the test if that is not possible.
func etcOverlay(t *testing.T, files map[string][]byte, binary string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for a private mount namespace")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("needs unshare")
	}
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	if out, err := exec.Command("cp", "-a", "/etc/.", etc+"/").CombinedOutput(); err != nil {
		os.MkdirAll(etc, 0o755)
		if out, err = exec.Command("cp", "-a", "/etc/.", etc+"/").CombinedOutput(); err != nil {
			t.Fatalf("copy /etc: %v %s", err, out)
		}
	}
	for name, b := range files {
		p := filepath.Join(etc, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := filepath.Join(dir, "wrapped-"+filepath.Base(binary))
	script := fmt.Sprintf("#!/bin/sh\nexec unshare -m --propagation private sh -c 'mount --bind %s /etc && exec \"$0\" \"$@\"' %s \"$@\"\n", etc, binary)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "--version").CombinedOutput(); err != nil {
		t.Skipf("mount namespace not available: %v %s", err, out)
	}
	return wrapper
}

func dump(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
