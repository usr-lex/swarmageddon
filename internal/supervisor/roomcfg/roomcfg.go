// Package roomcfg holds /etc/swarm/room.json, the room's non-secret settings
// rendered by the operator into the persona ConfigMap (docs/conventions.md,
// "Inside a room"), and the well-known paths inside a room.
//
// It has no dependencies beyond the standard library so the operator can
// import it to render room.json.
package roomcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// Paths inside a room (docs/conventions.md).
const (
	RoomJSON       = "/etc/swarm/room.json"
	RoleFile       = "/etc/swarm/role.md"
	PersonaDir     = "/etc/swarm/persona"
	IdentityDir    = "/run/swarm/identity"
	CABundle       = "/etc/swarm/ca/ca.crt"
	SupervisorSock = "/run/swarm/supervisor.sock"
	OreSock        = "/run/swarm/ore.sock"
	DockerSock     = "/var/run/docker.sock"
	EngineDir      = "/var/lib/containers"
	ClaudeEtc      = "/etc/claude-code"
	CodexEtc       = "/etc/codex"
	AgentUID       = 1000
)

// CLI values.
const (
	CLIClaude = "claude"
	CLIOre    = "ore"
)

// Room is the content of room.json.
type Room struct {
	Room        string `json:"room"`
	Workspace   string `json:"workspace"`
	Group       string `json:"group,omitempty"`
	RoleTitle   string `json:"roleTitle,omitempty"`
	RoleSummary string `json:"roleSummary,omitempty"`
	CLI         string `json:"cli"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	// ForgeURL is Forgejo's HTTP base URL, e.g. http://forgejo.swarmageddon-platform.svc:3000.
	ForgeURL string `json:"forgeURL,omitempty"`
	// ForgeSSH is Forgejo's SSH host[:port], e.g. forgejo-ssh.swarmageddon-platform.svc.
	ForgeSSH string `json:"forgeSSH,omitempty"`
	// ForgeUser defaults to the room name.
	ForgeUser    string `json:"forgeUser,omitempty"`
	MessagingURL string `json:"messagingURL,omitempty"`
	GatewayURL   string `json:"gatewayURL,omitempty"`
	// WorkDir is relative to the home directory, e.g. work/api.
	WorkDir string `json:"workDir,omitempty"`
	// Repos are owner/name, cloned into ~/work/<name>.
	Repos []string `json:"repos,omitempty"`
	// FirstTurn is the prompt the agent gets after every start.
	FirstTurn string `json:"firstTurn,omitempty"`

	// Optional settings; zero values mean the defaults.

	// PermissionMode is Claude Code's permission mode.
	PermissionMode string `json:"permissionMode,omitempty"`
	// Sudo gives the agent passwordless sudo; nil means true.
	Sudo *bool `json:"sudo,omitempty"`
	// GitName and GitEmail default to the capitalized room name and
	// <room>@agents.<workspace>.invalid.
	GitName  string `json:"gitName,omitempty"`
	GitEmail string `json:"gitEmail,omitempty"`
	// OreWireAPI is the gateway wire API for ore: responses (default), chat or anthropic.
	OreWireAPI string `json:"oreWireAPI,omitempty"`
	// Janitor thresholds (design §12); zero means the default.
	Janitor JanitorSettings `json:"janitor,omitempty"`
}

// JanitorSettings tunes the disk janitor.
type JanitorSettings struct {
	Disabled bool `json:"disabled,omitempty"`
	// IntervalMinutes between runs; default 60.
	IntervalMinutes int `json:"intervalMinutes,omitempty"`
	// PruneAfterHours: stopped containers and dangling images older than this; default 24.
	PruneAfterHours int `json:"pruneAfterHours,omitempty"`
	// ImagesAbovePercent of the engine volume also prunes unused tagged images; default 70.
	ImagesAbovePercent int `json:"imagesAbovePercent,omitempty"`
	// WarnPercent raises diskLow; default 80.
	WarnPercent int `json:"warnPercent,omitempty"`
	// AggressivePercent prunes everything unused; default 90.
	AggressivePercent int `json:"aggressivePercent,omitempty"`
}

// DefaultFirstTurn is the built-in first-turn prompt.
const DefaultFirstTurn = "Your room has just started. Check your messages and your open work " +
	"(issues and pull requests assigned to you, review requests) with `tea`, and act on what you find. " +
	"If there is nothing to do, stop and wait: messages from other agents and people will wake you."

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Load reads and validates room.json.
func Load(file string) (*Room, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var r Room
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &r, nil
}

// Validate checks the fields the supervisor relies on.
func (r *Room) Validate() error {
	var errs []error
	if !nameRE.MatchString(r.Room) {
		errs = append(errs, fmt.Errorf("room %q is not a valid name", r.Room))
	}
	if r.Workspace == "" {
		errs = append(errs, errors.New("workspace is empty"))
	}
	if r.CLI != CLIClaude && r.CLI != CLIOre {
		errs = append(errs, fmt.Errorf("cli %q is not claude or ore", r.CLI))
	}
	for _, repo := range r.Repos {
		if !repoRE.MatchString(strings.TrimSuffix(repo, ".git")) {
			errs = append(errs, fmt.Errorf("repo %q is not owner/name", repo))
		}
	}
	if path.IsAbs(r.WorkDir) || strings.Contains(r.WorkDir, "..") {
		errs = append(errs, fmt.Errorf("workDir %q must be relative to the home directory", r.WorkDir))
	}
	return errors.Join(errs...)
}

// Address is name@workspace.
func (r *Room) Address() string { return r.Room + "@" + r.Workspace }

// Home is the agent's home directory.
func (r *Room) Home() string { return "/home/" + r.Room }

// WorkPath is the absolute work folder: ~/<workDir>, or ~/work/<first repo>,
// or ~/work.
func (r *Room) WorkPath(home string) string {
	switch {
	case r.WorkDir != "":
		return path.Join(home, r.WorkDir)
	case len(r.Repos) > 0:
		return path.Join(home, "work", RepoName(r.Repos[0]))
	default:
		return path.Join(home, "work")
	}
}

// RepoName is the folder name of owner/name.
func RepoName(repo string) string {
	repo = strings.TrimSuffix(repo, ".git")
	return repo[strings.LastIndex(repo, "/")+1:]
}

// FirstTurnPrompt is the configured first turn or the default.
func (r *Room) FirstTurnPrompt() string {
	if strings.TrimSpace(r.FirstTurn) != "" {
		return r.FirstTurn
	}
	return DefaultFirstTurn
}

// SudoEnabled reports whether the agent gets sudo.
func (r *Room) SudoEnabled() bool { return r.Sudo == nil || *r.Sudo }

// GitIdentity returns the commit name and email.
func (r *Room) GitIdentity() (name, email string) {
	name, email = r.GitName, r.GitEmail
	if name == "" {
		name = strings.ToUpper(r.Room[:1]) + r.Room[1:]
	}
	if email == "" {
		email = r.Room + "@agents." + r.Workspace + ".invalid"
	}
	return name, email
}

// ForgeAccount is the forge user name.
func (r *Room) ForgeAccount() string {
	if r.ForgeUser != "" {
		return r.ForgeUser
	}
	return r.Room
}
