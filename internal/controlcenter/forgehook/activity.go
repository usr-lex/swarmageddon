package forgehook

import (
	"fmt"
	"strings"
	"time"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// Activities turns a delivery into observatory events (docs/design.md §8.7):
// one per commit pushed (deduplicated by SHA, since a merge pushes the
// branch's commits again), PRs opened, merged or closed, reviews, issues
// opened or closed, comments, CI runs, and forge handoffs (review
// requested, assigned, mentioned) as edges.
func Activities(d Delivery, p *payload, now time.Time) []store.Activity {
	repo := p.repo()
	if repo == nil {
		return nil
	}
	actor := ""
	if p.Sender != nil {
		actor = strings.ToLower(p.Sender.Login)
	}
	base := func(kind string) store.Activity {
		a := store.Activity{TS: now, Workspace: d.Workspace, Actor: actor, Kind: kind, Repo: repo.FullName}
		if it := p.item(); it != nil {
			a.Number, a.Summary, a.RefURL = int(it.Number), it.Title, it.HTMLURL
		}
		return a
	}
	var out []store.Activity
	add := func(a store.Activity, dedup string) {
		a.Dedup = dedup
		out = append(out, a)
	}
	mentions := func(text string) {
		for _, m := range Mentions(text) {
			if m == actor {
				continue
			}
			a := base(store.KindMentioned)
			a.Target = m
			add(a, fmt.Sprintf("delivery/%s/mentioned/%s", d.ID, m))
		}
	}
	kind := d.EventType
	if kind == "" {
		kind = d.Event
	}
	it := p.item()
	switch {
	case d.Event == "push":
		for _, c := range p.Commits {
			who := strings.ToLower(c.Author.Username)
			if who == "" && p.Pusher != nil {
				who = strings.ToLower(p.Pusher.Login)
			}
			ts := c.Timestamp
			if ts.IsZero() {
				ts = now
			}
			first, _, _ := strings.Cut(c.Message, "\n")
			out = append(out, store.Activity{TS: ts, Workspace: d.Workspace, Actor: who, Kind: store.KindCommit, Repo: repo.FullName,
				Summary: first, RefURL: c.URL, Dedup: "commit/" + repo.FullName + "/" + c.ID})
		}
	case isCI(d) || strings.HasSuffix(kind, "run_success"):
		if p.Run == nil {
			break
		}
		k := store.KindCIFailed
		if strings.HasSuffix(kind, "_success") || strings.HasSuffix(d.Event, "_success") {
			k = store.KindCIPassed
		}
		a := store.Activity{TS: now, Workspace: d.Workspace, Kind: k, Repo: repo.FullName, Summary: p.Run.Title, RefURL: p.Run.HTMLURL}
		if pr := p.Run.pullRequest(); pr != nil {
			a.Actor, a.Number = strings.ToLower(pr.User.Login), int(pr.Number)
		} else if p.Run.TriggerUser != nil {
			a.Actor = strings.ToLower(p.Run.TriggerUser.Login)
		}
		add(a, fmt.Sprintf("ci/%s/%d/%s", repo.FullName, p.Run.ID, k))
	case (kind == "issue_assign" || kind == "pull_request_assign") && p.Action == "assigned" && it != nil:
		for _, as := range it.Assignees {
			a := base(store.KindAssigned)
			a.Target = strings.ToLower(as.Login)
			add(a, fmt.Sprintf("delivery/%s/assigned/%s", d.ID, a.Target))
		}
	case kind == "pull_request_review_request" && p.Action == "review_requested" && it != nil && p.RequestedReviewer != nil:
		a := base(store.KindReviewRequested)
		a.Target = strings.ToLower(p.RequestedReviewer.Login)
		add(a, "delivery/"+d.ID)
	case kind == "issues" && it != nil && (p.Action == "opened" || p.Action == "closed"):
		k := store.KindIssueOpened
		if p.Action == "closed" {
			k = store.KindIssueClosed
		}
		add(base(k), fmt.Sprintf("%s/%s/%d", k, repo.FullName, it.Number))
		if p.Action == "opened" {
			mentions(it.Body)
		}
	case kind == "pull_request" && it != nil && (p.Action == "opened" || p.Action == "closed"):
		k := store.KindPROpened
		a := base(k)
		if p.Action == "closed" {
			k = store.KindPRClosed
			if it.Merged {
				k = store.KindPRMerged
				if it.MergedBy != nil {
					a.Actor = strings.ToLower(it.MergedBy.Login)
				}
			}
			a.Kind = k
			a.Target = strings.ToLower(it.User.Login)
		}
		add(a, fmt.Sprintf("%s/%s/%d", k, repo.FullName, it.Number))
		if p.Action == "opened" {
			mentions(it.Body)
		}
	case (kind == "issue_comment" || kind == "pull_request_comment") && p.Action == "created" && it != nil && p.Comment != nil:
		a := base(store.KindComment)
		a.Target = strings.ToLower(it.User.Login)
		if a.Target == actor {
			a.Target = ""
		}
		add(a, "delivery/"+d.ID)
		mentions(p.Comment.Body)
	case strings.HasPrefix(kind, "pull_request_review_") && p.Review != nil && it != nil:
		k := store.KindReviewComment
		switch kind {
		case "pull_request_review_approved":
			k = store.KindReviewApproved
		case "pull_request_review_rejected":
			k = store.KindReviewChanges
		}
		a := base(k)
		a.Target = strings.ToLower(it.User.Login)
		add(a, "delivery/"+d.ID)
		mentions(p.Review.Content)
	}
	return out
}
