// Package wizard turns the workspace wizard's plan into resources
// (docs/design.md §8.3): a Workspace, its Rooms and a RunnerPool. It writes
// only custom resources. Re-running it on an existing workspace changes only
// what differs; fields the wizard doesn't show are kept as they are.
package wizard

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
)

// RepoCredential is a write-only credential a plan carries for one
// repository: kind "import" or "mirror", stored in Secret SecretName.
type RepoCredential struct {
	Repo, Kind, SecretName string
	Username, Token        string
}

// StageRepoCredentials gives each repository credential in the plan a fresh
// Secret name (k8s.RepoCredentialSecretName), set in the plan's
// import_secret_name or mirror_secret_name so Build points the spec at it,
// and returns the credentials to create. A credential is "token" or
// "username:token".
func StageRepoCredentials(p *ccv1.WorkspacePlan) []RepoCredential {
	var out []RepoCredential
	add := func(repo, kind, cred string) string {
		user, token, ok := strings.Cut(cred, ":")
		if !ok {
			user, token = "", cred
		}
		c := RepoCredential{Repo: repo, Kind: kind, SecretName: k8s.RepoCredentialSecretName(repo, kind), Username: user, Token: token}
		out = append(out, c)
		return c.SecretName
	}
	for _, r := range p.GetRepositories() {
		if r.GetImportCredential() != "" {
			r.ImportSecretName = add(r.GetName(), "import", r.GetImportCredential())
		}
		if r.GetMirrorToken() != "" {
			r.MirrorSecretName = add(r.GetName(), "mirror", r.GetMirrorToken())
		}
	}
	return out
}

// validateRepos checks repository names and credentials.
func validateRepos(p *ccv1.WorkspacePlan) []string {
	var probs []string
	for _, r := range p.GetRepositories() {
		n := r.GetName()
		if !repoRE.MatchString(n) {
			probs = append(probs, fmt.Sprintf("repository %q: the name is not valid", n))
		}
		if r.GetImportGithub() && !strings.HasPrefix(r.GetImportUrl(), "https://github.com/") {
			probs = append(probs, fmt.Sprintf("repository %q: a GitHub import needs a github.com import URL", n))
		}
		for _, c := range []struct{ what, cred, url string }{
			{"import credential", r.GetImportCredential(), r.GetImportUrl()},
			{"mirror token", r.GetMirrorToken(), r.GetPushMirror()},
		} {
			if c.cred == "" {
				continue
			}
			if c.url == "" {
				probs = append(probs, fmt.Sprintf("repository %q: a %s needs its URL", n, c.what))
			}
			if _, token, ok := strings.Cut(c.cred, ":"); ok && token == "" || strings.TrimSpace(c.cred) != c.cred {
				probs = append(probs, fmt.Sprintf("repository %q: the %s is not a token or username:token", n, c.what))
			}
		}
	}
	agents := map[string]bool{}
	for _, a := range p.GetAgents() {
		agents[a.GetName()] = true
	}
	for _, a := range p.GetGithubAccounts() {
		if !githubLoginRE.MatchString(a.GetLogin()) {
			probs = append(probs, fmt.Sprintf("GitHub account %q: not a GitHub login", a.GetLogin()))
		}
		if a.GetId() <= 0 {
			probs = append(probs, fmt.Sprintf("GitHub account %s: GitHub user ID is required", a.GetLogin()))
		}
		if !agents[a.GetRoom()] {
			probs = append(probs, fmt.Sprintf("GitHub account %s: no room %s in the plan", a.GetLogin(), a.GetRoom()))
		}
	}
	return probs
}

var githubLoginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

var repoRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Names is the default agent name pool: Greek gods, titans and spirits.
var Names = []string{
	"athena", "hermes", "metis", "apollo", "artemis", "hephaestus", "demeter", "hestia", "dionysus", "ares",
	"poseidon", "hera", "zeus", "aphrodite", "persephone", "hades", "eos", "helios", "selene", "iris",
	"nike", "themis", "mnemosyne", "prometheus", "epimetheus", "atlas", "rhea", "kronos", "hyperion", "theia",
	"tethys", "oceanus", "phoebe", "coeus", "crius", "iapetus", "leto", "asteria", "hecate", "pan",
	"nemesis", "tyche", "hebe", "eros", "morpheus", "hypnos", "thanatos", "nyx", "gaia", "uranus",
	"proteus", "triton", "nereus", "eirene", "dike", "eunomia", "clio", "thalia", "urania", "calliope",
	"erato", "euterpe", "melpomene", "terpsichore", "polyhymnia", "aether", "hemera", "harmonia", "boreas", "zephyrus",
}

// Suggest returns count names from the pool not in used, then numbered ones.
func Suggest(count int, used map[string]bool) []string {
	var out []string
	for _, n := range Names {
		if len(out) == count {
			return out
		}
		if !used[n] {
			out = append(out, n)
			used[n] = true
		}
	}
	for i := 2; len(out) < count; i++ {
		for _, n := range Names {
			c := fmt.Sprintf("%s-%d", n, i)
			if len(out) == count {
				break
			}
			if !used[c] {
				out = append(out, c)
				used[c] = true
			}
		}
	}
	return out
}

// Built-in role template names (the operator copies them into each workspace).
var BuiltinRoles = []string{"coordinator", "implementer", "reviewer", "ci-infra", "qa", "chronicler"}

// DefaultRoles is the default role mix for n agents: one coordinator, n
// implementers, one reviewer, plus CI/infra and QA past five agents.
func DefaultRoles(n int) []string {
	switch {
	case n <= 0:
		return nil
	case n == 1:
		return []string{"implementer"}
	case n == 2:
		return []string{"coordinator", "implementer"}
	}
	roles := []string{"coordinator"}
	extra := []string{"reviewer"}
	if n > 5 {
		extra = append(extra, "ci-infra", "qa")
	}
	for i := 0; i < n-1-len(extra); i++ {
		roles = append(roles, "implementer")
	}
	return append(roles, extra...)
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Validate returns problems that would refuse the plan. reserved names
// are used elsewhere on the platform (other workspaces' rooms).
func Validate(p *ccv1.WorkspacePlan, reserved map[string]bool) []string {
	var probs []string
	if !nameRE.MatchString(p.GetName()) {
		probs = append(probs, "the workspace name must be lower case letters, digits and dashes")
	}
	if p.GetOrg() == "" {
		probs = append(probs, "the Forgejo organization is required")
	} else if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(p.GetOrg()) {
		probs = append(probs, "the Forgejo organization name is not valid")
	}
	validAccess := func(a string) bool { return a == "" || a == "gateway" || a == "direct" }
	if !validAccess(p.GetModelAccess()) {
		probs = append(probs, fmt.Sprintf("model access %q: gateway or direct", p.GetModelAccess()))
	}
	for _, a := range p.GetAgents() {
		if !validAccess(a.GetModelAccess()) {
			probs = append(probs, fmt.Sprintf("agent %s: model access %q: gateway or direct", a.GetName(), a.GetModelAccess()))
		}
	}
	groups := map[string]bool{"": true}
	for _, g := range p.GetGroups() {
		if !nameRE.MatchString(g.GetName()) {
			probs = append(probs, fmt.Sprintf("group %q: lower case letters, digits and dashes", g.GetName()))
		}
		groups[g.GetName()] = true
	}
	seen := map[string]bool{}
	for _, a := range p.GetAgents() {
		n := a.GetName()
		switch {
		case !nameRE.MatchString(n):
			probs = append(probs, fmt.Sprintf("agent %q: names are lower case letters, digits and dashes", n))
		case seen[n]:
			probs = append(probs, fmt.Sprintf("agent %q appears twice", n))
		case reserved[n]:
			probs = append(probs, fmt.Sprintf("agent %q: the name is already used on this platform", n))
		case n == "forge":
			probs = append(probs, `agent "forge": the name is reserved for forge notices`)
		}
		seen[n] = true
		if a.GetCli() != "claude" && a.GetCli() != "ore" {
			probs = append(probs, fmt.Sprintf("agent %q: the CLI must be claude or ore", n))
		}
		if a.GetRole() == "" {
			probs = append(probs, fmt.Sprintf("agent %q: a role is required", n))
		}
		if !groups[a.GetGroup()] {
			probs = append(probs, fmt.Sprintf("agent %q: unknown group %q", n, a.GetGroup()))
		}
		for _, q := range []struct{ field, v string }{{"cpu", a.GetCpu()}, {"memory", a.GetMemory()}, {"home volume", a.GetHomeVolume()}, {"engine volume", a.GetEngineVolume()}} {
			if q.v != "" {
				if _, err := resource.ParseQuantity(q.v); err != nil {
					probs = append(probs, fmt.Sprintf("agent %q: %s %q is not a quantity", n, q.field, q.v))
				}
			}
		}
	}
	for _, l := range p.GetLinks() {
		if !groups[l.GetFrom()] || !groups[l.GetTo()] {
			probs = append(probs, fmt.Sprintf("link %s→%s names an unknown group", l.GetFrom(), l.GetTo()))
		}
		if !slices.Contains([]string{"open", "mediated", "none"}, l.GetMode()) {
			probs = append(probs, fmt.Sprintf("link %s→%s: mode must be open, mediated or none", l.GetFrom(), l.GetTo()))
		}
	}
	if d := p.GetApprovalTimeout(); d != "" {
		if _, err := time.ParseDuration(d); err != nil {
			probs = append(probs, "the approval timeout is not a duration (e.g. 24h)")
		}
	}
	for _, e := range p.GetForgeNotifications() {
		if !slices.Contains([]string{"assigned", "reviewRequested", "mentioned", "reviewSubmitted", "commented", "ciFailed"}, e) {
			probs = append(probs, fmt.Sprintf("unknown forge notification %q", e))
		}
	}
	probs = append(probs, validateChannels(p)...)
	probs = append(probs, validateRepos(p)...)
	return append(probs, validateLayers(p)...)
}

// Desired is what a plan asks for.
type Desired struct {
	Workspace *v1alpha1.Workspace
	Rooms     []*v1alpha1.Room
	Pool      *v1alpha1.RunnerPool // nil: none
	// RoleTemplates holds every template, with the plan's roomDefaults;
	// ones without a ResourceVersion don't exist yet.
	RoleTemplates []*v1alpha1.RoleTemplate
}

// Current is what exists.
type Current struct {
	Workspace     *v1alpha1.Workspace
	Rooms         []v1alpha1.Room
	Pool          *v1alpha1.RunnerPool
	RoleTemplates []v1alpha1.RoleTemplate
}

func qty(s string) *resource.Quantity {
	if s == "" {
		return nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil
	}
	return &q
}

func qstr(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}

// SecretName names the Secret holding a notification channel's settings.
func SecretName(ws, kind string) string { return "notify-" + ws + "-" + strings.ToLower(kind) }

// Build computes the desired resources, starting from the current ones so
// fields the wizard doesn't manage are kept.
func Build(p *ccv1.WorkspacePlan, cur Current) Desired {
	ws := &v1alpha1.Workspace{}
	if cur.Workspace != nil {
		ws = cur.Workspace.DeepCopy()
	}
	ws.Name = p.GetName()
	s := &ws.Spec
	s.DisplayName = p.GetDisplayName()
	s.Forge.Org = p.GetOrg()
	if p.GetRequiredApprovals() > 0 {
		s.Forge.RequiredApprovals = p.GetRequiredApprovals()
	}
	s.Groups = nil
	for _, g := range p.GetGroups() {
		s.Groups = append(s.Groups, v1alpha1.Group{Name: g.GetName(), Description: g.GetDescription()})
	}
	if p.GetDefaultLinks() != "" {
		s.Messaging.DefaultLinks = v1alpha1.DefaultLinks(p.GetDefaultLinks())
	}
	s.Messaging.Links = nil
	for _, l := range p.GetLinks() {
		s.Messaging.Links = append(s.Messaging.Links, v1alpha1.GroupLink{From: l.GetFrom(), To: l.GetTo(), Mode: v1alpha1.LinkMode(l.GetMode()),
			Senders: l.GetSenders(), Recipients: l.GetRecipients(), RequirePrefix: l.GetRequirePrefix()})
	}
	s.Models.Default = p.GetDefaultModel()
	s.Models.Access = v1alpha1.ModelAccess(p.GetModelAccess())
	s.Models.Budgets.PerRoomMonthlyUSD = p.GetBudgetPerRoomMonthlyUsd()
	s.Notifications.Channels = nil
	for _, c := range p.GetNotificationChannels() {
		ch := v1alpha1.NotificationChannel{Kind: v1alpha1.ChannelKind(c.GetKind()), Topic: c.GetTopic()}
		switch c.GetKind() {
		case "slack", "discord", "email":
			ch.SecretName = SecretName(p.GetName(), c.GetKind())
		}
		s.Notifications.Channels = append(s.Notifications.Channels, ch)
	}
	s.Notifications.Events = nil
	for _, e := range p.GetNotificationEvents() {
		s.Notifications.Events = append(s.Notifications.Events, v1alpha1.NotificationEvent(e))
	}
	s.Repositories = nil
	for _, r := range p.GetRepositories() {
		s.Repositories = append(s.Repositories, v1alpha1.RepositorySpec{Name: r.GetName(), ImportURL: r.GetImportUrl(), ImportSecretName: r.GetImportSecretName(),
			Group: r.GetGroup(), PushMirror: r.GetPushMirror(), MirrorSecretName: r.GetMirrorSecretName(), StatusChecks: r.GetStatusChecks(),
			ImportGitHub: r.GetImportGithub()})
	}
	s.GitHubAccounts = nil
	for _, a := range p.GetGithubAccounts() {
		s.GitHubAccounts = append(s.GitHubAccounts, v1alpha1.GitHubAccount{Login: a.GetLogin(), ID: a.GetId(), Room: a.GetRoom()})
	}
	s.Owners = p.GetOwners()
	if p.RoomDefaults != nil {
		s.RoomDefaults = SettingsFromPlan(p.GetRoomDefaults())
	}
	// The deprecated internet flag becomes roomDefaults.network; FromCluster
	// already folded an old workspace's internet: false into the plan's network.
	if s.RoomDefaults.Network == nil && !p.GetInternet() {
		s.RoomDefaults.Network = &v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkNone}
	}
	s.Internet = nil //nolint:staticcheck // cleared: roomDefaults.network now says the same
	s.SharedVolumes = volumesFromPlan(p)
	if d, err := time.ParseDuration(p.GetApprovalTimeout()); err == nil && p.GetApprovalTimeout() != "" {
		s.ApprovalTimeout = &metav1.Duration{Duration: d}
	} else {
		s.ApprovalTimeout = nil
	}
	if p.GetUpdateMode() != "" {
		s.UpdatePolicy = v1alpha1.UpdatePolicy{Mode: v1alpha1.UpdateMode(p.GetUpdateMode()), Window: p.GetUpdateWindow()}
	}
	s.ForgeNotifications = nil
	for _, e := range p.GetForgeNotifications() {
		s.ForgeNotifications = append(s.ForgeNotifications, v1alpha1.ForgeEvent(e))
	}
	s.RecordKeystrokes = p.GetRecordKeystrokes()
	applyChannels(p, s)

	existing := map[string]v1alpha1.Room{}
	for _, r := range cur.Rooms {
		existing[r.Name] = r
	}
	ns := v1alpha1.WorkspaceNamespace(p.GetName())
	var rooms []*v1alpha1.Room
	for _, a := range p.GetAgents() {
		r := &v1alpha1.Room{}
		if e, ok := existing[a.GetName()]; ok {
			r = e.DeepCopy()
		}
		r.Name, r.Namespace = a.GetName(), ns
		rs := &r.Spec
		rs.DisplayName = a.GetDisplayName()
		rs.Role = a.GetRole()
		rs.CLI = v1alpha1.CLI(a.GetCli())
		rs.Model = a.GetModel()
		rs.Effort = a.GetEffort()
		rs.Group = a.GetGroup()
		rs.Paused = a.GetPaused()
		rs.Resources.CPU = qty(a.GetCpu())
		rs.Resources.Memory = qty(a.GetMemory())
		rs.Volumes.Home = qty(a.GetHomeVolume())
		rs.Volumes.Engine = qty(a.GetEngineVolume())
		rs.Repos = a.GetRepos()
		rs.WorkDir = a.GetWorkDir()
		if a.Settings != nil {
			SetRoomSettings(rs, SettingsFromPlan(a.GetSettings()))
		}
		rs.Advisor = AdvisorFromPlan(a.GetAdvisor(), true)
		rs.WardenMode = v1alpha1.WardenMode(a.GetWardenMode())
		rs.ModelAccess = v1alpha1.ModelAccess(a.GetModelAccess())
		rooms = append(rooms, r)
	}
	var templates []*v1alpha1.RoleTemplate
	have := map[string]bool{}
	for _, t := range cur.RoleTemplates {
		t := t.DeepCopy()
		if rd, ok := p.GetRoleDefaults()[t.Name]; ok {
			t.Spec.RoomDefaults = SettingsFromPlan(rd)
		}
		if ad, ok := p.GetRoleAdvisors()[t.Name]; ok {
			t.Spec.Advisor = AdvisorFromPlan(ad, false)
		}
		have[t.Name] = true
		templates = append(templates, t)
	}
	names := slices.Collect(maps.Keys(p.GetRoleDefaults()))
	names = append(names, slices.Collect(maps.Keys(p.GetRoleAdvisors()))...)
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if !have[name] {
			t := &v1alpha1.RoleTemplate{}
			t.Name, t.Namespace = name, ns
			if rd, ok := p.GetRoleDefaults()[name]; ok {
				t.Spec.RoomDefaults = SettingsFromPlan(rd)
			}
			if ad, ok := p.GetRoleAdvisors()[name]; ok {
				t.Spec.Advisor = AdvisorFromPlan(ad, false)
			}
			templates = append(templates, t)
		}
	}
	var pool *v1alpha1.RunnerPool
	if len(p.GetRunnerLabels()) > 0 {
		pool = &v1alpha1.RunnerPool{}
		if cur.Pool != nil {
			pool = cur.Pool.DeepCopy()
		}
		pool.Name, pool.Namespace = "default", ns
		pool.Spec.Labels = p.GetRunnerLabels()
		pool.Spec.MaxConcurrent = p.GetRunnerMaxConcurrent()
	}
	return Desired{Workspace: ws, Rooms: rooms, Pool: pool, RoleTemplates: templates}
}

// FromCluster turns existing resources into a plan.
func FromCluster(cur Current) *ccv1.WorkspacePlan {
	w := cur.Workspace
	s := w.Spec
	p := &ccv1.WorkspacePlan{
		Name: w.Name, DisplayName: s.DisplayName, Org: s.Forge.Org, DefaultLinks: string(s.Messaging.DefaultLinks),
		Owners: s.Owners, DefaultModel: s.Models.Default, ModelAccess: string(s.Models.Access), BudgetPerRoomMonthlyUsd: s.Models.Budgets.PerRoomMonthlyUSD,
		UpdateMode: string(s.UpdatePolicy.Mode), UpdateWindow: s.UpdatePolicy.Window, RequiredApprovals: s.Forge.RequiredApprovals,
		Internet:     s.Internet == nil || *s.Internet, //nolint:staticcheck // still read for workspaces written before roomDefaults.network
		RoomDefaults: SettingsToPlan(WorkspaceDefaults(w)), SharedVolumes: volumesToPlan(s.SharedVolumes),
		RoleDefaults: map[string]*ccv1.RoomSettingsPlan{}, RoleAdvisors: map[string]*ccv1.AdvisorPlan{},
		RecordKeystrokes: s.RecordKeystrokes,
	}
	for _, t := range cur.RoleTemplates {
		if !isEmpty(t.Spec.RoomDefaults) {
			p.RoleDefaults[t.Name] = SettingsToPlan(t.Spec.RoomDefaults)
		}
		if t.Spec.Advisor != nil {
			p.RoleAdvisors[t.Name] = AdvisorToPlan(t.Spec.Advisor)
		}
	}
	if s.ApprovalTimeout != nil {
		p.ApprovalTimeout = s.ApprovalTimeout.Duration.String()
	}
	for _, g := range s.Groups {
		p.Groups = append(p.Groups, &ccv1.GroupPlan{Name: g.Name, Description: g.Description})
	}
	for _, l := range s.Messaging.Links {
		p.Links = append(p.Links, &ccv1.LinkPlan{From: l.From, To: l.To, Mode: string(l.Mode), Senders: l.Senders, Recipients: l.Recipients, RequirePrefix: l.RequirePrefix})
	}
	for _, c := range s.Notifications.Channels {
		p.NotificationChannels = append(p.NotificationChannels, &ccv1.NotificationChannelPlan{Kind: string(c.Kind), Topic: c.Topic, SecretName: c.SecretName})
	}
	for _, e := range s.Notifications.Events {
		p.NotificationEvents = append(p.NotificationEvents, string(e))
	}
	for _, e := range s.ForgeNotifications {
		p.ForgeNotifications = append(p.ForgeNotifications, string(e))
	}
	channelsToPlan(s, p)
	for _, r := range s.Repositories {
		p.Repositories = append(p.Repositories, &ccv1.RepositoryPlan{Name: r.Name, ImportUrl: r.ImportURL, ImportSecretName: r.ImportSecretName,
			Group: r.Group, PushMirror: r.PushMirror, MirrorSecretName: r.MirrorSecretName, StatusChecks: r.StatusChecks,
			ImportGithub: r.ImportGitHub})
	}
	for _, a := range s.GitHubAccounts {
		p.GithubAccounts = append(p.GithubAccounts, &ccv1.GitHubAccountPlan{Login: a.Login, Id: a.ID, Room: a.Room})
	}
	rooms := append([]v1alpha1.Room(nil), cur.Rooms...)
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Name < rooms[j].Name })
	for _, r := range rooms {
		rs := r.Spec
		p.Agents = append(p.Agents, &ccv1.AgentPlan{Name: r.Name, DisplayName: rs.DisplayName, Role: rs.Role, Cli: string(rs.CLI),
			Model: rs.Model, Effort: rs.Effort, Group: rs.Group, Cpu: qstr(rs.Resources.CPU), Memory: qstr(rs.Resources.Memory),
			HomeVolume: qstr(rs.Volumes.Home), EngineVolume: qstr(rs.Volumes.Engine), Repos: rs.Repos, WorkDir: rs.WorkDir, Paused: rs.Paused,
			Settings: SettingsToPlan(RoomSettings(rs)), Advisor: AdvisorToPlan(rs.Advisor), WardenMode: string(rs.WardenMode),
			ModelAccess: string(rs.ModelAccess)})
	}
	if cur.Pool != nil {
		p.RunnerLabels = cur.Pool.Spec.Labels
		p.RunnerMaxConcurrent = cur.Pool.Spec.MaxConcurrent
	}
	return p
}

func specJSON(v any) string {
	if v == nil || reflect.ValueOf(v).IsNil() {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// fieldDiff names the top-level spec fields that differ.
func fieldDiff(before, after string) []string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal([]byte(before), &a)
	_ = json.Unmarshal([]byte(after), &b)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if string(a[k]) != string(b[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func change(kind, name string, before, after any) *ccv1.Change {
	bj, aj := "", ""
	if before != nil && !reflect.ValueOf(before).IsNil() {
		bj = specJSON(before)
	}
	if after != nil && !reflect.ValueOf(after).IsNil() {
		aj = specJSON(after)
	}
	c := &ccv1.Change{Kind: kind, Name: name, Before: bj, After: aj}
	switch {
	case bj == "" && aj != "":
		c.Action = ccv1.ChangeAction_CHANGE_ACTION_CREATE
	case aj == "" && bj != "":
		c.Action = ccv1.ChangeAction_CHANGE_ACTION_DELETE
	case bj == aj:
		c.Action = ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED
	default:
		c.Action = ccv1.ChangeAction_CHANGE_ACTION_UPDATE
		c.Fields = fieldDiff(bj, aj)
	}
	return c
}

// Diff compares the current and desired resources. With prune, rooms not in
// the plan are deleted.
func Diff(cur Current, d Desired, prune bool) []*ccv1.Change {
	var out []*ccv1.Change
	var curWS *v1alpha1.WorkspaceSpec
	if cur.Workspace != nil {
		curWS = &cur.Workspace.Spec
	}
	out = append(out, change("Workspace", d.Workspace.Name, curWS, &d.Workspace.Spec))
	existing := map[string]*v1alpha1.RoomSpec{}
	for i := range cur.Rooms {
		existing[cur.Rooms[i].Name] = &cur.Rooms[i].Spec
	}
	planned := map[string]bool{}
	for _, r := range d.Rooms {
		planned[r.Name] = true
		out = append(out, change("Room", r.Name, existing[r.Name], &r.Spec))
	}
	if prune {
		for _, r := range cur.Rooms {
			if !planned[r.Name] {
				out = append(out, change("Room", r.Name, &r.Spec, (*v1alpha1.RoomSpec)(nil)))
			}
		}
	}
	var curPool, newPool *v1alpha1.RunnerPoolSpec
	if cur.Pool != nil {
		curPool = &cur.Pool.Spec
	}
	if d.Pool != nil {
		newPool = &d.Pool.Spec
	}
	curRT := map[string]*roleFields{}
	for i := range cur.RoleTemplates {
		curRT[cur.RoleTemplates[i].Name] = &roleFields{cur.RoleTemplates[i].Spec.RoomDefaults, cur.RoleTemplates[i].Spec.Advisor}
	}
	for _, t := range d.RoleTemplates {
		before := curRT[t.Name]
		if before == nil {
			before = &roleFields{}
		}
		after := &roleFields{t.Spec.RoomDefaults, t.Spec.Advisor}
		c := change("RoleTemplate", t.Name, before, after)
		if c.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			c.Fields = nil
			if specJSON(&before.RoomDefaults) != specJSON(&after.RoomDefaults) {
				c.Fields = append(c.Fields, "roomDefaults")
			}
			if specJSON(before.Advisor) != specJSON(after.Advisor) {
				c.Fields = append(c.Fields, "advisor")
			}
			out = append(out, c)
		}
	}
	if curPool != nil || newPool != nil {
		c := change("RunnerPool", "default", curPool, newPool)
		if newPool == nil {
			c.Action = ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED // the wizard never deletes a pool
			c.After = c.Before
		}
		out = append(out, c)
	}
	return out
}

// roleFields are the RoleTemplate fields the wizard manages.
type roleFields struct {
	RoomDefaults v1alpha1.RoomDefaults `json:"roomDefaults"`
	Advisor      *v1alpha1.AdvisorSpec `json:"advisor,omitempty"`
}

// LoadCurrent reads a workspace's resources; a missing workspace is a zero Current.
func LoadCurrent(ctx context.Context, c client.Client, name string) (Current, error) {
	var cur Current
	var ws v1alpha1.Workspace
	err := c.Get(ctx, types.NamespacedName{Name: name}, &ws)
	if apierrors.IsNotFound(err) {
		return cur, nil
	}
	if err != nil {
		return cur, err
	}
	cur.Workspace = &ws
	ns := v1alpha1.WorkspaceNamespace(name)
	var rooms v1alpha1.RoomList
	if err := c.List(ctx, &rooms, client.InNamespace(ns)); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
		return cur, err
	}
	cur.Rooms = rooms.Items
	var pool v1alpha1.RunnerPool
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "default"}, &pool); err == nil {
		cur.Pool = &pool
	}
	var rts v1alpha1.RoleTemplateList
	if err := c.List(ctx, &rts, client.InNamespace(ns)); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
		return cur, err
	}
	cur.RoleTemplates = rts.Items
	return cur, nil
}

// ErrNamespaceNotReady means the workspace exists but its namespace (or the
// control center's access to it) isn't there yet; apply again shortly.
var ErrNamespaceNotReady = fmt.Errorf("the workspace's namespace is not ready yet; apply again in a moment")

// Apply writes the changes. Rooms are created only once the workspace's
// namespace exists; it waits up to wait for that.
func Apply(ctx context.Context, c client.Client, cur Current, d Desired, changes []*ccv1.Change, wait time.Duration) error {
	byKey := map[string]*ccv1.Change{}
	for _, ch := range changes {
		byKey[ch.Kind+"/"+ch.Name] = ch
	}
	switch byKey["Workspace/"+d.Workspace.Name].GetAction() {
	case ccv1.ChangeAction_CHANGE_ACTION_CREATE:
		if err := c.Create(ctx, d.Workspace); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
	case ccv1.ChangeAction_CHANGE_ACTION_UPDATE:
		if err := c.Update(ctx, d.Workspace); err != nil {
			return fmt.Errorf("update workspace: %w", err)
		}
	}
	needNS := false
	for _, ch := range changes {
		if ch.Kind != "Workspace" && ch.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			needNS = true
		}
	}
	if needNS {
		if err := waitNamespace(ctx, c, d.Workspace.Name, wait); err != nil {
			return err
		}
	}
	for _, r := range d.Rooms {
		switch byKey["Room/"+r.Name].GetAction() {
		case ccv1.ChangeAction_CHANGE_ACTION_CREATE:
			if err := c.Create(ctx, r); err != nil {
				return fmt.Errorf("create room %s: %w", r.Name, err)
			}
		case ccv1.ChangeAction_CHANGE_ACTION_UPDATE:
			if err := c.Update(ctx, r); err != nil {
				return fmt.Errorf("update room %s: %w", r.Name, err)
			}
		}
	}
	for _, r := range cur.Rooms {
		if byKey["Room/"+r.Name].GetAction() == ccv1.ChangeAction_CHANGE_ACTION_DELETE {
			if err := c.Delete(ctx, &r); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete room %s: %w", r.Name, err)
			}
		}
	}
	for _, t := range d.RoleTemplates {
		if ch := byKey["RoleTemplate/"+t.Name]; ch != nil && ch.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			if err := setRoleFields(ctx, c, t, wait); err != nil {
				return err
			}
		}
	}
	if d.Pool != nil {
		switch byKey["RunnerPool/default"].GetAction() {
		case ccv1.ChangeAction_CHANGE_ACTION_CREATE:
			if err := c.Create(ctx, d.Pool); err != nil {
				return fmt.Errorf("create runner pool: %w", err)
			}
		case ccv1.ChangeAction_CHANGE_ACTION_UPDATE:
			if err := c.Update(ctx, d.Pool); err != nil {
				return fmt.Errorf("update runner pool: %w", err)
			}
		}
	}
	return nil
}

// setRoleFields writes a template's roomDefaults and advisor. A built-in
// template the operator hasn't created yet is waited for; the wizard never
// creates one, since the operator would then skip filling it in.
func setRoleFields(ctx context.Context, c client.Client, t *v1alpha1.RoleTemplate, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var cur v1alpha1.RoleTemplate
		err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, &cur)
		if err == nil {
			orig := cur.DeepCopy()
			cur.Spec.RoomDefaults = t.Spec.RoomDefaults
			cur.Spec.Advisor = t.Spec.Advisor
			if err := c.Patch(ctx, &cur, client.MergeFrom(orig)); err != nil {
				return fmt.Errorf("role template %s: %w", t.Name, err)
			}
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("role template %s: %w", t.Name, err)
		}
		if !slices.Contains(BuiltinRoles, t.Name) {
			return fmt.Errorf("role template %s does not exist", t.Name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("role template %s is not there yet; apply again in a moment", t.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func waitNamespace(ctx context.Context, c client.Client, ws string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var l v1alpha1.RoomList
		err := c.List(ctx, &l, client.InNamespace(v1alpha1.WorkspaceNamespace(ws)), client.Limit(1))
		var w v1alpha1.Workspace
		gerr := c.Get(ctx, types.NamespacedName{Name: ws}, &w)
		if err == nil && gerr == nil && w.Status.Namespace != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrNamespaceNotReady
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
