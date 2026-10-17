// Package proc runs child processes and restarts them with backoff.
package proc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// User is who a child runs as. A nil *User runs as the supervisor.
type User struct {
	UID, GID uint32
	Groups   []uint32
	Name     string
	Home     string
}

// Spec describes one supervised service.
type Spec struct {
	Name string
	Path string
	Args []string
	// Env is appended to a minimal base (PATH, HOME, USER, LANG).
	Env  []string
	Dir  string
	User *User
	// Output receives stdout and stderr, line-prefixed; nil means os.Stderr.
	Output io.Writer
	// MinBackoff and MaxBackoff bound restart delays (defaults 1 s and 60 s).
	MinBackoff, MaxBackoff time.Duration
	// OnExit is called after each exit, with how long the process ran.
	OnExit func(err error, ran time.Duration)
}

// Command builds an exec.Cmd for the spec (not started).
func (s *Spec) Command(ctx context.Context) *exec.Cmd {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = append(BaseEnv(s.User), s.Env...)
	out := s.Output
	if out == nil {
		out = os.Stderr
	}
	w := &prefixWriter{prefix: []byte("[" + s.Name + "] "), w: out}
	cmd.Stdout, cmd.Stderr = w, w
	// Output goes through a pipe, and Wait waits for every holder of it to
	// close it: a grandchild that outlives the child (ore's app-server
	// leaves some) would block Wait, and so the restart, forever.
	cmd.WaitDelay = waitDelay
	cmd.SysProcAttr = SysProcAttr(s.User)
	return cmd
}

// waitDelay is how long Wait waits for the output pipe after the child exits.
const waitDelay = 5 * time.Second

// SysProcAttr returns attributes that run a child as u in its own process group.
func SysProcAttr(u *User) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{Setpgid: true}
	if u != nil && u.UID != uint32(os.Getuid()) {
		a.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: u.Groups}
	}
	return a
}

// BaseEnv is the environment every child starts from.
func BaseEnv(u *User) []string {
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
		"LANG=" + envOr("LANG", "C.UTF-8"),
	}
	if u != nil {
		env = append(env, "HOME="+u.Home, "USER="+u.Name, "LOGNAME="+u.Name)
	} else {
		env = append(env, "HOME="+envOr("HOME", "/root"))
	}
	return env
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Supervise runs the spec until ctx ends, restarting it after every exit with
// exponential backoff (reset after a minute of uptime). On cancellation it
// sends SIGTERM to the process group, then SIGKILL after 10 s.
func Supervise(ctx context.Context, s Spec, log *slog.Logger) {
	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB == 0 {
		minB = time.Second
	}
	if maxB == 0 {
		maxB = time.Minute
	}
	backoff := minB
	for ctx.Err() == nil {
		start := time.Now()
		err := Run(ctx, s.Command(ctx))
		ran := time.Since(start)
		if s.OnExit != nil {
			s.OnExit(err, ran)
		}
		if ctx.Err() != nil {
			return
		}
		if ran > time.Minute {
			backoff = minB
		}
		log.Warn("service exited; restarting", "service", s.Name, "err", err, "ran", ran.Round(time.Second), "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxB)
	}
}

// Run starts cmd and waits for it; when ctx ends it stops the process group
// gracefully (SIGTERM, then SIGKILL after 10 s).
func Run(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		Stop(cmd, done, 10*time.Second)
		return ctx.Err()
	}
}

// Stop signals the process group of a started command and waits on done.
func Stop(cmd *exec.Cmd, done <-chan error, grace time.Duration) {
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
}

// prefixWriter prefixes each line with the service name.
type prefixWriter struct {
	mu     sync.Mutex
	prefix []byte
	w      io.Writer
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := append(append([]byte{}, p.prefix...), p.buf[:i+1]...)
		if _, err := p.w.Write(line); err != nil {
			return len(b), nil
		}
		p.buf = p.buf[i+1:]
	}
	if len(p.buf) > 64<<10 {
		_, _ = p.w.Write(append(append([]byte{}, p.prefix...), append(p.buf, '\n')...))
		p.buf = p.buf[:0]
	}
	return len(b), nil
}
