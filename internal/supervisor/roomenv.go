package supervisor

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/supervisor/identity"
	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// The room's environment (design §6.6, §11.2): room.json's env, the secret
// env from the identity Secret (room.json secretEnv names, keys env_<NAME>), and in limited network
// mode the egress proxy settings. It is set for the CLI, written for the
// agent's shells, and the proxy part for nested containers.

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are set by the supervisor and can't be overridden by the room.
var reservedEnv = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "PATH": true,
	"ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_BASE_URL": true, "SWARM_GATEWAY_KEY": true,
	"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true,
	"https_proxy": true, "http_proxy": true, "no_proxy": true,
}

// RoomEnv returns the room's variables as sorted NAME=value entries, and the
// names it skipped.
func RoomEnv(room *roomcfg.Room, secretDir string) (env []string, skipped []string) {
	vars := map[string]string{}
	set := func(k, v string) {
		if !envNameRE.MatchString(k) || reservedEnv[k] {
			skipped = append(skipped, k)
			return
		}
		vars[k] = v
	}
	for k, v := range room.Env {
		set(k, v)
	}
	for _, name := range room.SecretEnv {
		b, err := os.ReadFile(filepath.Join(secretDir, roomcfg.SecretEnvKey(name)))
		if err != nil {
			skipped = append(skipped, name)
			continue
		}
		v := strings.TrimRight(string(b), "\n")
		redact.Add(v)
		set(name, v)
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env, skipped
}

// ProxyEnv returns HTTPS_PROXY, HTTP_PROXY and NO_PROXY (and lowercase
// forms) for limited mode. The proxy URL carries the room name as user and
// auth (the identity Secret's egress_auth) as password; an auth of the form
// user:password is used as is. Other modes, or no proxy URL, return nil.
func ProxyEnv(room *roomcfg.Room, auth string) ([]string, error) {
	n := room.Network
	if n.Mode != roomcfg.NetworkLimited || n.ProxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(n.ProxyURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("network.proxyURL %q is not a URL", n.ProxyURL)
	}
	auth = strings.TrimSpace(auth)
	if user, pass, ok := strings.Cut(auth, ":"); ok {
		u.User = url.UserPassword(user, pass)
	} else if auth != "" {
		u.User = url.UserPassword(room.Room, auth)
	}
	noProxy := strings.Join(room.NoProxyHosts(), ",")
	p := u.String()
	return []string{
		"HTTPS_PROXY=" + p, "HTTP_PROXY=" + p, "NO_PROXY=" + noProxy,
		"https_proxy=" + p, "http_proxy=" + p, "no_proxy=" + noProxy,
	}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Shell env files: the agent's variables in ~/.swarm/env (0600), sourced by
// login shells (/etc/profile.d) and interactive bash (/etc/bash.bashrc).
const (
	profileScript = "/etc/profile.d/swarm-env.sh"
	bashrc        = "/etc/bash.bashrc"
	envBlockBegin = "# >>> swarm environment >>>"
	envBlockEnd   = "# <<< swarm environment <<<"
	sourceLine    = `[ -r "$HOME/.swarm/env" ] && . "$HOME/.swarm/env"`
)

// WriteShellEnv writes env for the agent's shells. etc is "/" in a room
// (a temporary root in tests).
func WriteShellEnv(u *proc.User, env []string, etc string) error {
	var b strings.Builder
	b.WriteString("# Written by the room supervisor at start.\n")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
	}
	dir := filepath.Join(u.Home, ".swarm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, "env")
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		_ = os.Chown(dir, int(u.UID), int(u.GID))
		_ = os.Chown(file, int(u.UID), int(u.GID))
	}
	prof := filepath.Join(etc, profileScript)
	if err := os.MkdirAll(filepath.Dir(prof), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(prof, []byte(sourceLine+"\n"), 0o644); err != nil {
		return err
	}
	rc := filepath.Join(etc, bashrc)
	old, _ := os.ReadFile(rc)
	return os.WriteFile(rc, []byte(identity.ReplaceBlock(string(old), envBlockBegin, envBlockEnd, sourceLine+"\n")), 0o644)
}

// PodmanProxyConf is the containers.conf drop-in for nested containers.
const PodmanProxyConf = "/etc/containers/containers.conf.d/60-swarm-proxy.conf"

// WritePodmanProxy writes (or, without proxy settings, removes) the drop-in
// that gives nested containers the proxy settings.
func WritePodmanProxy(file string, proxy []string) error {
	if len(proxy) == 0 {
		err := os.Remove(file)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var q []string
	for _, kv := range proxy {
		q = append(q, fmt.Sprintf("%q", kv))
	}
	content := "# Written by the room supervisor: the egress proxy (limited network mode).\n[containers]\nhttp_proxy = false\nenv = [" + strings.Join(q, ", ") + "]\n"
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(content), 0o600)
}
