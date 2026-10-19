package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/supervisor/advisor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/claude"
	"github.com/usr-lex/swarmageddon/internal/supervisor/display"
	"github.com/usr-lex/swarmageddon/internal/supervisor/engine"
	"github.com/usr-lex/swarmageddon/internal/supervisor/identity"
	"github.com/usr-lex/swarmageddon/internal/supervisor/janitor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/llm"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient"
	"github.com/usr-lex/swarmageddon/internal/supervisor/ore"
	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// personaFiles maps persona ConfigMap keys to where they go (docs/conventions.md).
var personaFiles = map[string]string{
	"room.json":                    roomcfg.RoomJSON,
	"role.md":                      roomcfg.RoleFile,
	"claude-managed-settings.json": filepath.Join(roomcfg.ClaudeEtc, "managed-settings.json"),
	"claude-managed-mcp.json":      filepath.Join(roomcfg.ClaudeEtc, "managed-mcp.json"),
	"ore-requirements.toml":        filepath.Join(roomcfg.CodexEtc, "requirements.toml"),
	"ore-config.toml":              filepath.Join(roomcfg.CodexEtc, "config.toml"),
	"ore-persona.rules":            filepath.Join(roomcfg.CodexEtc, "rules", "persona.rules"),
	"claude-agent-advisor.md":      filepath.Join(roomcfg.ClaudeEtc, ".claude", "agents", "advisor.md"),
}

// installPersona copies the mounted persona files into place, root-owned and
// read-only to the agent.
func installPersona(dir string) error {
	var errs []error
	for key, dst := range personaFiles {
		b, err := os.ReadFile(filepath.Join(dir, key))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = os.MkdirAll(filepath.Dir(dst), 0o755)
		}
		if err == nil {
			err = os.WriteFile(dst, b, 0o644)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("persona %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// Identity Secret keys the supervisor reads besides the identity files
// (docs/conventions.md).
const (
	KeyGateway    = "gateway_key"
	KeyAdvisor    = "advisor_key"
	KeyWarden     = "warden_key"
	KeyEgressAuth = "egress_auth"
)

func readSecret(name string) string {
	b, _ := os.ReadFile(filepath.Join(roomcfg.IdentityDir, name))
	v := strings.TrimSpace(string(b))
	redact.Add(v)
	return v
}

// Main runs a room: PID 1 under tini, as root.
func Main(ctx context.Context, log *slog.Logger) error {
	if err := installPersona(roomcfg.PersonaDir); err != nil {
		log.Warn("installing persona files", "err", err)
	}
	room, err := roomcfg.Load(roomcfg.RoomJSON)
	if err != nil {
		return err
	}
	log = log.With("room", room.AddressOrDefault())
	home := room.Home()
	user, err := identity.EnsureUser(room.Room, roomcfg.AgentUID, home, room.SudoEnabled())
	if err != nil {
		return fmt.Errorf("agent user: %w", err)
	}
	gitName, gitEmail := room.GitIdentity()
	if err := identity.Install(identity.Setup{
		User: user, SecretDir: roomcfg.IdentityDir, GitName: gitName, GitEmail: gitEmail,
		ForgeURL: room.ForgeURL, ForgeSSH: room.ForgeSSHHost, ForgeUser: room.ForgeAccount(),
	}); err != nil {
		log.Warn("identity setup incomplete", "err", err)
	}
	// /run/swarm holds our socket and ore's directory (the ore driver makes it).
	if err := os.MkdirAll("/run/swarm", 0o770); err == nil {
		_ = os.Chown("/run/swarm", 0, int(user.GID))
		_ = os.Chmod("/run/swarm", 0o770)
	}
	stateDir := filepath.Join(home, ".swarm")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.Chown(stateDir, int(user.UID), int(user.GID))

	ca := roomcfg.CABundle
	if _, err := os.Stat(ca); err != nil {
		ca = filepath.Join(roomcfg.IdentityDir, "ca.crt")
	}
	msg, err := msgclient.New(room.MessagingURL, msgclient.TLSFiles{
		Cert: filepath.Join(roomcfg.IdentityDir, "tls.crt"), Key: filepath.Join(roomcfg.IdentityDir, "tls.key"), CA: ca,
	})
	if err != nil {
		return fmt.Errorf("messaging client: %w", err)
	}
	roleText := ""
	if b, err := os.ReadFile(roomcfg.RoleFile); err == nil {
		roleText = string(b)
	}
	var peers *messagingv1.ListPeersResponse
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if res, err := msg.ListPeers(pctx, connect.NewRequest(&messagingv1.ListPeersRequest{})); err == nil {
		peers = res.Msg
	} else {
		log.Warn("team list unavailable; instructions without it", "err", err)
	}
	cancel()
	if err := writeInstructions(room, user, roleText, peers); err != nil {
		log.Warn("writing instructions", "err", err)
	}

	var wg sync.WaitGroup
	start := func(s proc.Spec) {
		if _, err := exec.LookPath(s.Path); err != nil {
			log.Warn("service not installed; skipped", "service", s.Name, "path", s.Path)
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); proc.Supervise(ctx, s, log) }()
	}
	roomEnv, skipped := RoomEnv(room, roomcfg.IdentityDir)
	if len(skipped) > 0 {
		log.Warn("room variables skipped (invalid or reserved names, or unreadable)", "names", skipped)
	}
	proxyEnv, err := ProxyEnv(room, readSecret(KeyEgressAuth))
	if err != nil {
		log.Warn("egress proxy not configured", "err", err)
	}
	roomEnv = append(roomEnv, proxyEnv...)
	if err := WriteShellEnv(user, roomEnv, "/"); err != nil {
		log.Warn("writing the shell environment", "err", err)
	}
	if err := WritePodmanProxy(PodmanProxyConf, proxyEnv); err != nil {
		log.Warn("writing the Podman proxy settings", "err", err)
	}
	runOnce(ctx, log, "/usr/local/libexec/podman-ulimits")
	eng := engine.Spec()
	eng.Env = append(eng.Env, proxyEnv...)
	start(eng)
	start(display.Xvfb(user, ""))
	start(display.Openbox(user))
	start(display.X11VNC(user))

	core := &Core{
		Room: room, CLI: room.CLI, Messaging: msg, Log: log, CoalesceBytes: room.Storm.CoalesceLimit(),
		Volumes: []Volume{{Name: "home", Path: home}, {Name: "engine", Path: roomcfg.EngineDir}},
	}
	if !room.Janitor.Disabled {
		j := &janitor.Janitor{Config: janitorConfig(room.Janitor), Log: log}
		wg.Add(1)
		go func() { defer wg.Done(); j.Run(ctx) }()
	}
	cloneRepos(ctx, room, user, log)
	work := room.WorkPath(home)
	if err := os.MkdirAll(work, 0o755); err == nil {
		_ = os.Chown(work, int(user.UID), int(user.GID))
	}
	dedup, err := msgclient.LoadDedup(filepath.Join(stateDir, "delivered"), 10000)
	if err != nil {
		log.Warn("dedup set unreadable; starting empty", "err", err)
		dedup, _ = msgclient.LoadDedup(filepath.Join(stateDir, "delivered.new"), 10000)
	}
	core.Dedup = dedup
	gatewayKey := readSecret(KeyGateway)
	if room.Direct() {
		gatewayKey = "" // the CLI uses its own provider key or login
	}
	core.Agent = newAgent(room, user, work, roleText, roomEnv, gatewayKey, core, log)
	core.Warden = NewWarden(room, core, readSecret(KeyWarden), roleText, work)
	if room.Advisor.Model != "" {
		core.Advisor = &advisor.Advisor{
			LLM:        &llm.Client{BaseURL: room.GatewayURL, Key: readSecret(KeyAdvisor), Model: room.Advisor.Model, Effort: room.Advisor.Effort},
			MaxPerHour: room.Advisor.MaxCallsPerHour, WorkDir: work, User: user, RoleText: roleText,
			Task: core.CurrentTask, Log: log.With("component", "advisor"),
		}
		if t, ok := core.Agent.(agent.Transcriber); ok {
			core.Advisor.Transcript = t.Transcript
		}
	}

	api := &API{Core: core}
	wg.Add(4)
	go func() {
		defer wg.Done()
		if !room.Direct() {
			watchGatewayKey(ctx, func() string { return readSecret(KeyGateway) }, gatewayKey, core.Agent, gatewayKeyEnv(room), GatewayKeyEvery, log)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ServeUnix(ctx, roomcfg.SupervisorSock, int(user.GID), api.Handler()); err != nil {
			log.Error("room API", "err", err)
		}
	}()
	go func() { defer wg.Done(); core.Run(ctx) }()
	go func() {
		defer wg.Done()
		if err := core.Agent.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("agent driver", "err", err)
		}
	}()
	log.Info("room started", "cli", room.CLI, "work", work)
	wg.Wait()
	return nil
}

// runOnce runs a root setup program if it is installed, logging failures.
func runOnce(ctx context.Context, log *slog.Logger, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(cctx, path).CombinedOutput(); err != nil {
		log.Warn("setup step failed", "path", path, "err", err, "out", strings.TrimSpace(string(out)))
	}
}

func janitorConfig(s roomcfg.JanitorSettings) janitor.Config {
	return janitor.Config{
		Interval:           time.Duration(s.IntervalMinutes) * time.Minute,
		PruneAfter:         time.Duration(s.PruneAfterHours) * time.Hour,
		ImagesAbovePercent: s.ImagesAbovePercent,
		WarnPercent:        s.WarnPercent,
		AggressivePercent:  s.AggressivePercent,
		EngineDir:          roomcfg.EngineDir,
		Env:                []string{"DOCKER_HOST=" + engine.DockerHost},
	}
}

// writeInstructions puts the generated instructions where the CLI reads
// them. Claude: the managed CLAUDE.md, which is the operator's rendered
// claude-CLAUDE.md (identity, tools, role) plus the team from ListPeers, or
// our own full text if the persona has none. ore: the generated block of
// ~/.ore/AGENTS.md.
func writeInstructions(room *roomcfg.Room, u *proc.User, roleText string, peers *messagingv1.ListPeersResponse) error {
	if room.CLI == roomcfg.CLIClaude {
		text := Instructions(room, roleText, peers)
		if persona, err := os.ReadFile(filepath.Join(roomcfg.PersonaDir, "claude-CLAUDE.md")); err == nil && len(strings.TrimSpace(string(persona))) > 0 {
			text = strings.TrimRight(string(persona), "\n") + "\n\n## Messages and channels\n\n" + ChannelGuide(room) + TeamSection(peers)
		}
		if err := os.MkdirAll(roomcfg.ClaudeEtc, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(roomcfg.ClaudeEtc, "CLAUDE.md"), []byte(text), 0o644)
	}
	text := Instructions(room, roleText, peers)
	dir := filepath.Join(u.Home, ".ore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chown(dir, int(u.UID), int(u.GID))
	file := filepath.Join(dir, "AGENTS.md")
	old, _ := os.ReadFile(file)
	out := identity.ReplaceBlock(string(old), OreBlockBegin, OreBlockEnd, text)
	if err := os.WriteFile(file, []byte(out), 0o644); err != nil {
		return err
	}
	return os.Chown(file, int(u.UID), int(u.GID))
}

// cloneRepos clones spec.repos missing from ~/work over SSH, as the agent.
func cloneRepos(ctx context.Context, room *roomcfg.Room, u *proc.User, log *slog.Logger) {
	if room.ForgeSSHHost == "" {
		return
	}
	workDir := filepath.Join(u.Home, "work")
	_ = os.MkdirAll(workDir, 0o755)
	_ = os.Chown(workDir, int(u.UID), int(u.GID))
	for _, repo := range room.Repos {
		repo = strings.TrimSuffix(repo, ".git")
		dest := filepath.Join(workDir, roomcfg.RepoName(repo))
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			continue
		}
		if entries, _ := os.ReadDir(dest); len(entries) > 0 {
			log.Warn("work folder exists and is not a clone; not cloning", "repo", repo, "dest", dest)
			continue
		}
		url := "ssh://git@" + room.ForgeSSHHost + "/" + repo + ".git"
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cmd := exec.CommandContext(cctx, "git", "clone", "--quiet", url, dest)
		cmd.Env = proc.BaseEnv(u)
		cmd.SysProcAttr = proc.SysProcAttr(u)
		cmd.SysProcAttr.Setpgid = false
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			log.Warn("clone failed", "repo", repo, "err", err, "out", strings.TrimSpace(string(out)))
			continue
		}
		log.Info("cloned", "repo", repo, "dest", dest)
	}
}

// gatewayKeyEnv is the variable that carries the gateway key to the CLI.
func gatewayKeyEnv(room *roomcfg.Room) string {
	if room.CLI == roomcfg.CLIOre {
		return ore.GatewayKeyEnv
	}
	return "ANTHROPIC_AUTH_TOKEN"
}

// newAgent builds the CLI driver for the room. roomEnv is RoomEnv plus the
// proxy settings; key is the gateway key.
func newAgent(room *roomcfg.Room, u *proc.User, work, roleText string, roomEnv []string, key string, ev agent.Events, log *slog.Logger) agent.Agent {
	roomEnv = slices.Clip(roomEnv)
	direct := room.Direct()
	if room.CLI == roomcfg.CLIOre {
		p := ore.Policy{GatewayURL: room.OreBaseURL(), WireAPI: room.OreWireAPI()}
		d := &ore.Driver{
			Home: u.Home, WorkDir: work, User: u, Socket: roomcfg.OreSock,
			Env:                   append(roomEnv, gatewayKeyEnv(room)+"="+key),
			Settings:              ore.UserSettings{Provider: p.Provider(), Model: room.Model, Effort: room.Effort},
			DeveloperInstructions: roleText, FirstTurn: room.FirstTurnPrompt(),
			Events: ev, Log: log,
		}
		if direct {
			// ore's own provider, with OPENAI_API_KEY (room secretEnv) or its login.
			d.Env, d.Settings.Provider, d.Settings.Direct = roomEnv, nil, true
		}
		return d
	}
	env := roomEnv
	switch {
	case direct:
		// Claude Code's own endpoint, with ANTHROPIC_API_KEY (room
		// secretEnv) or its own login (claude /login in the terminal).
	case room.GatewayURL != "":
		env = append(env, gatewayKeyEnv(room)+"="+key, "ANTHROPIC_BASE_URL="+room.GatewayURL)
	default:
		env = append(env, gatewayKeyEnv(room)+"="+key)
		if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
			env = append(env, "ANTHROPIC_BASE_URL="+base)
		}
	}
	var extra []string
	if room.Advisor.Native && room.Advisor.Model != "" {
		// ADR 0010: opt-in, unverified through the gateway.
		extra = []string{"--advisor", room.Advisor.Model}
		env = append(env, "CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL=1")
	}
	return &claude.Driver{
		Name: room.Room, ExtraArgs: extra, Model: room.Model, Effort: room.Effort, PermissionMode: room.PermissionMode,
		Home: u.Home, WorkDir: work, User: u, Env: env, FirstTurn: room.FirstTurnPrompt(),
		Events: ev, Log: log,
	}
}
