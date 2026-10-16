// Package tmux runs a command in a detached tmux session as the agent user
// and waits for it to exit, without polling.
package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
)

// Session is one tmux session running one command.
type Session struct {
	// Name of the session, e.g. "agent".
	Name string
	// Socket is tmux's -L socket name; empty uses the user's default socket,
	// which is what `tmux attach -t agent` finds.
	Socket string
	User   *proc.User
	// Env is set for the command (on top of proc.BaseEnv). It is written to
	// EnvFile (mode 0600, owned by User) so values don't show in ps.
	Env     []string
	EnvFile string
	Dir     string
	// Width and Height of the detached window (defaults 200x50).
	Width, Height int
}

func (s *Session) tmux(args ...string) *exec.Cmd {
	if s.Socket != "" {
		args = append([]string{"-L", s.Socket}, args...)
	}
	cmd := exec.Command("tmux", args...)
	cmd.Env = append(proc.BaseEnv(s.User), "TERM=xterm-256color")
	cmd.SysProcAttr = proc.SysProcAttr(s.User)
	cmd.SysProcAttr.Setpgid = false
	return cmd
}

// Exists reports whether the session is running.
func (s *Session) Exists() bool {
	return s.tmux("has-session", "-t", "="+s.Name).Run() == nil
}

// Run starts argv in the session and blocks until it exits or ctx ends (then
// the session is killed). The exit is signalled through `tmux wait-for`.
func (s *Session) Run(ctx context.Context, argv []string) error {
	if s.Exists() {
		_ = s.Kill()
	}
	channel := "exit-" + s.Name
	w, h := s.Width, s.Height
	if w == 0 {
		w, h = 200, 50
	}
	// The command runs under sh so we can signal tmux when it ends, even if
	// a person detached everything. exec env sets the variables without
	// leaking them into tmux's global environment.
	script := shellJoin(argv) + "; rc=$?; tmux " + socketFlag(s.Socket) + "wait-for -S " + channel + "; exit $rc"
	if len(s.Env) > 0 {
		if s.EnvFile == "" {
			return fmt.Errorf("tmux: Env needs EnvFile")
		}
		if err := s.writeEnv(); err != nil {
			return err
		}
		script = "set -a; . " + quote(s.EnvFile) + "; set +a; " + script
	}
	args := []string{"new-session", "-d", "-s", s.Name, "-x", fmt.Sprint(w), "-y", fmt.Sprint(h)}
	if s.Dir != "" {
		args = append(args, "-c", s.Dir)
	}
	args = append(args, "sh", "-c", script)
	if out, err := s.tmux(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, out)
	}
	// Keep the window around long enough to capture a crash message? No:
	// the supervisor restarts it, and logs say why.
	wait := s.tmux("wait-for", channel)
	errc := make(chan error, 1)
	if err := wait.Start(); err != nil {
		return err
	}
	go func() { errc <- wait.Wait() }()
	select {
	case <-errc:
		if s.Exists() {
			// wait-for returned for another reason (the server died
			// differently); treat as exit anyway.
			_ = s.Kill()
		}
		return nil
	case <-ctx.Done():
		_ = s.Kill()
		_ = wait.Process.Kill()
		<-errc
		return ctx.Err()
	}
}

func (s *Session) writeEnv() error {
	var b strings.Builder
	for _, kv := range s.Env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&b, "%s=%s\n", k, quote(v))
	}
	tmp := s.EnvFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if s.User != nil && os.Getuid() == 0 {
		if err := os.Chown(tmp, int(s.User.UID), int(s.User.GID)); err != nil {
			return err
		}
	}
	return os.Rename(tmp, s.EnvFile)
}

// Kill ends the session.
func (s *Session) Kill() error { return s.tmux("kill-session", "-t", "="+s.Name).Run() }

// Capture returns the visible pane text.
func (s *Session) Capture() (string, error) {
	out, err := s.tmux("capture-pane", "-p", "-t", "="+s.Name).Output()
	return string(out), err
}

// SendKeys types keys into the pane (used by tests).
func (s *Session) SendKeys(keys ...string) error {
	return s.tmux(append([]string{"send-keys", "-t", "=" + s.Name}, keys...)...).Run()
}

func socketFlag(sock string) string {
	if sock == "" {
		return ""
	}
	return "-L " + quote(sock) + " "
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = quote(a)
	}
	return strings.Join(q, " ")
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
