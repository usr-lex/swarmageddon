// Package githubimport reads what a GitHub organization has, for the
// workspace wizard's import (ADR 0012): its repositories and the accounts,
// by prefix bot-<room>, whose history becomes rooms'. The token is used for
// these calls and passed on for the import; nothing here stores it.
package githubimport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Client calls GitHub's REST API.
type Client struct {
	// BaseURL defaults to https://api.github.com.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Repo is a repository of the organization.
type Repo struct {
	Name        string
	Description string
	Private     bool
	Archived    bool
	HasWiki     bool
	OpenIssues  int
}

// Bot is an account named <prefix><room>.
type Bot struct {
	Login string
	ID    int64
	// Room is the room name it maps to; empty when the rest isn't a valid
	// room name.
	Room string
}

// Discovery is what the organization has.
type Discovery struct {
	Repos []Repo
	Bots  []Bot
	// People counts the other accounts; their posts stay "originally by".
	People int
}

var roomRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// RoomName is the room for login: the part after prefix, lower case, with
// other characters as dashes; "" when that isn't a valid room name.
func RoomName(login, prefix string) string {
	if !strings.HasPrefix(strings.ToLower(login), strings.ToLower(prefix)) {
		return ""
	}
	n := strings.ToLower(login[len(prefix):])
	n = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, n)
	if !roomRE.MatchString(n) {
		return ""
	}
	return n
}

// Discover lists the organization's repositories, and its members' and
// contributors' accounts, sorting those named prefix* into Bots.
func (c *Client) Discover(ctx context.Context, org, prefix string) (*Discovery, error) {
	var repos []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
		Archived    bool   `json:"archived"`
		HasWiki     bool   `json:"has_wiki"`
		OpenIssues  int    `json:"open_issues_count"`
	}
	if err := c.list(ctx, "/orgs/"+url.PathEscape(org)+"/repos?type=all", &repos); err != nil {
		return nil, err
	}
	d := &Discovery{}
	type account struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	seen := map[int64]account{}
	var members []account
	if err := c.list(ctx, "/orgs/"+url.PathEscape(org)+"/members", &members); err != nil {
		return nil, err
	}
	for _, a := range members {
		seen[a.ID] = a
	}
	for _, r := range repos {
		d.Repos = append(d.Repos, Repo{Name: r.Name, Description: r.Description, Private: r.Private, Archived: r.Archived, HasWiki: r.HasWiki, OpenIssues: r.OpenIssues})
		var contributors []account
		if err := c.list(ctx, "/repos/"+url.PathEscape(org)+"/"+url.PathEscape(r.Name)+"/contributors", &contributors); err != nil {
			return nil, err
		}
		for _, a := range contributors {
			seen[a.ID] = a
		}
	}
	for _, a := range seen {
		switch {
		case strings.HasPrefix(strings.ToLower(a.Login), strings.ToLower(prefix)):
			d.Bots = append(d.Bots, Bot{Login: a.Login, ID: a.ID, Room: RoomName(a.Login, prefix)})
		case a.Type == "Bot" || strings.HasSuffix(a.Login, "[bot]"):
			// GitHub Apps (dependabot and the like): neither rooms nor people.
		default:
			d.People++
		}
	}
	slices.SortFunc(d.Bots, func(a, b Bot) int { return strings.Compare(a.Login, b.Login) })
	return d, nil
}

// list GETs path and follows Link: rel="next", appending every page to out.
func (c *Client) list(ctx context.Context, path string, out any) error {
	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := strings.TrimRight(base, "/") + path + sep + "per_page=100"
	var all []json.RawMessage
	for pages := 0; next != "" && pages < 50; pages++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusNoContent:
			// An empty repository has no contributors.
		case resp.StatusCode/100 != 2:
			return fmt.Errorf("GitHub %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
		default:
			var page []json.RawMessage
			if err := json.Unmarshal(body, &page); err != nil {
				return fmt.Errorf("GitHub %s: %w", path, err)
			}
			all = append(all, page...)
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// nextLink is the rel="next" URL of a Link header, or "".
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		u, rel, ok := strings.Cut(strings.TrimSpace(part), ";")
		if ok && strings.Contains(rel, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(u), "<>")
		}
	}
	return ""
}
