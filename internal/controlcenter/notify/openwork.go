package notify

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/internal/forgejo"
)

// DefaultOpenWorkTTL is how long ForgeOpenWork keeps a workspace's answer.
const DefaultOpenWorkTTL = 5 * time.Minute

// ForgeOpenWork finds open work in Forgejo (docs/design.md §10.3): open
// issues and pull requests assigned to a room's forge user (the room's
// name) in the workspace's organization. It reads with the read-only
// forge-reader token and keeps each workspace's answer for TTL.
type ForgeOpenWork struct {
	// Forge returns a client with the forge-reader token.
	Forge func(ctx context.Context) (*forgejo.Client, error)
	// Org returns a workspace's Forgejo organization.
	Org func(ctx context.Context, workspace string) (string, error)
	TTL time.Duration // 0: DefaultOpenWorkTTL
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]assignees
}

// assignees are the lower-case logins with open work, as of at.
type assignees struct {
	at    time.Time
	users map[string]bool
}

// HasOpenWork implements OpenWork.
func (f *ForgeOpenWork) HasOpenWork(ctx context.Context, workspace, room string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	ttl := f.TTL
	if ttl <= 0 {
		ttl = DefaultOpenWorkTTL
	}
	a, ok := f.cache[workspace]
	if !ok || now.Sub(a.at) >= ttl {
		users, err := f.fetch(ctx, workspace)
		if err != nil {
			return false, err
		}
		if f.cache == nil {
			f.cache = map[string]assignees{}
		}
		a = assignees{at: now, users: users}
		f.cache[workspace] = a
	}
	return a.users[strings.ToLower(room)], nil
}

func (f *ForgeOpenWork) fetch(ctx context.Context, workspace string) (map[string]bool, error) {
	org, err := f.Org(ctx, workspace)
	if err != nil {
		return nil, err
	}
	fc, err := f.Forge(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := fc.ListOrgRepos(ctx, org)
	if err != nil {
		return nil, err
	}
	users := map[string]bool{}
	for _, r := range repos {
		issues, err := fc.ListOpenIssues(ctx, org, r.Name)
		if forgejo.IsNotFound(err) {
			continue // issues and pull requests are off in this repository
		}
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			for _, u := range is.Assignees {
				users[strings.ToLower(u.Login)] = true
			}
		}
	}
	return users, nil
}
