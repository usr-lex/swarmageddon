// Package egress is the rooms' egress proxy (docs/design.md §11.2): an HTTP
// proxy that handles CONNECT and plain HTTP requests, authenticates each
// room with Proxy-Authorization, and lets it reach only the hosts on its
// allowlist. It never terminates TLS; for CONNECT it peeks at the TLS
// ClientHello and requires its SNI to match the CONNECT host.
//
// The operator keeps the allowlists in ConfigMap egress-allowlist in the
// platform namespace: one data key per room, "<workspace>.<room>", holding an
// Entry as JSON. Each room's controller patches only its own key.
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ConfigMapName is the allowlist table in the platform namespace.
const ConfigMapName = "egress-allowlist"

// Port is the proxy's port.
const Port = 3128

// Network modes (api/v1alpha1.NetworkMode).
const (
	ModeNone    = "none"
	ModeLimited = "limited"
	ModeFull    = "full"
)

// Entry is one room's row.
type Entry struct {
	// Room is the room's address, <room>@<workspace>.
	Room string `json:"room"`
	// Mode is none, limited or full.
	Mode string `json:"mode"`
	// Auth is Hash of the room's credential (identity Secret key egress_auth).
	Auth string `json:"auth"`
	// Domains are the allowed hosts, package managers included: exact hosts
	// or *.suffix, with an optional :port (default 443 and 80).
	Domains []string `json:"domains,omitempty"`
	// PackageManagers records that the package managers' hosts are included.
	PackageManagers bool `json:"packageManagers,omitempty"`
}

// Name is the room's name.
func (e Entry) Name() string { n, _, _ := strings.Cut(e.Room, "@"); return n }

// Marshal encodes an entry for the ConfigMap.
func (e Entry) Marshal() string {
	b, _ := json.Marshal(e)
	return string(b)
}

// DataKey is a room's key in the ConfigMap.
func DataKey(workspace, room string) string { return workspace + "." + room }

// Hash is the hex SHA-256 of a credential.
func Hash(credential string) string {
	s := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(s[:])
}

// PackageManagers are the package registries' hosts the packageManagers
// switch adds (§11.2). Docker Hub is covered by the registry cache.
var PackageManagers = []string{
	// npm, Yarn and pnpm
	"registry.npmjs.org", "registry.yarnpkg.com", "repo.yarnpkg.com",
	// PyPI
	"pypi.org", "files.pythonhosted.org",
	// Go
	"proxy.golang.org", "sum.golang.org",
	// Rust
	"crates.io", "index.crates.io", "static.crates.io",
	// Ruby
	"rubygems.org", "index.rubygems.org",
	// Maven Central
	"repo.maven.apache.org", "repo1.maven.org",
	// Ubuntu
	"archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com",
	// GitHub release downloads (the github.com URLs redirect here)
	"objects.githubusercontent.com", "release-assets.githubusercontent.com",
}

// Pattern is a parsed allowlist entry.
type Pattern struct {
	// Host is the exact host, or the suffix (with its leading dot) when Wildcard.
	Host     string
	Wildcard bool
	// Port is 0 for the defaults, 443 and 80.
	Port int
}

// ParsePattern parses "host", "*.suffix", with an optional ":port".
func ParsePattern(s string) (Pattern, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	host, port := s, 0
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return Pattern{}, fmt.Errorf("bad port in %q", s)
		}
		host, port = h, n
	}
	host = strings.TrimSuffix(host, ".")
	var p Pattern
	if strings.HasPrefix(host, "*.") {
		p.Wildcard, host = true, host[1:]
	}
	if host == "" || host == "." || strings.ContainsAny(host, "*/ @") {
		return Pattern{}, fmt.Errorf("bad host in %q", s)
	}
	p.Host, p.Port = host, port
	return p, nil
}

// Matches reports whether host:port is allowed by p. A wildcard matches
// subdomains of its suffix, not the suffix itself.
func (p Pattern) Matches(host string, port int) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if p.Port == 0 {
		if port != 443 && port != 80 {
			return false
		}
	} else if port != p.Port {
		return false
	}
	if p.Wildcard {
		return strings.HasSuffix(host, p.Host) && len(host) > len(p.Host)
	}
	return host == p.Host
}

// ValidDomain reports whether s is a valid allowlist entry.
func ValidDomain(s string) bool {
	_, err := ParsePattern(s)
	return err == nil
}

// room is an entry with its patterns compiled.
type room struct {
	Entry
	patterns []Pattern
}

func (r *room) allows(host string, port int) bool {
	switch r.Mode {
	case ModeFull:
		return true
	case ModeLimited:
		for _, p := range r.patterns {
			if p.Matches(host, port) {
				return true
			}
		}
	}
	return false
}

// Table maps credentials to rooms.
type Table struct {
	byAuth map[string]*room
}

// Parse reads the ConfigMap's data. Bad rows and bad patterns are skipped
// and reported.
func Parse(data map[string]string) (*Table, []error) {
	t := &Table{byAuth: map[string]*room{}}
	var errs []error
	for k, v := range data {
		var e Entry
		if err := json.Unmarshal([]byte(v), &e); err != nil || e.Room == "" || e.Auth == "" {
			errs = append(errs, fmt.Errorf("%s %s: bad entry: %v", ConfigMapName, k, err))
			continue
		}
		r := &room{Entry: e}
		for _, d := range e.Domains {
			p, err := ParsePattern(d)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", ConfigMapName, k, err))
				continue
			}
			r.patterns = append(r.patterns, p)
		}
		t.byAuth[strings.ToLower(e.Auth)] = r
	}
	return t, errs
}

func (t *Table) lookup(credential string) *room {
	if t == nil || credential == "" {
		return nil
	}
	return t.byAuth[Hash(credential)]
}

// Len is the number of rooms.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byAuth)
}
