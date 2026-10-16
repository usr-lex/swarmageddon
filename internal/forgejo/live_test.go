package forgejo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestLive runs the provisioning flow against a real Forgejo when
// FORGEJO_TEST_URL and FORGEJO_TEST_TOKEN (an admin token) are set.
func TestLive(t *testing.T) {
	base, tok := os.Getenv("FORGEJO_TEST_URL"), os.Getenv("FORGEJO_TEST_TOKEN")
	if base == "" || tok == "" {
		t.Skip("FORGEJO_TEST_URL and FORGEJO_TEST_TOKEN not set")
	}
	c := New(base, tok)
	ctx := context.Background()
	sfx := make([]byte, 3)
	_, _ = rand.Read(sfx)
	org, user := "t"+hex.EncodeToString(sfx), "agent"+hex.EncodeToString(sfx)
	t.Cleanup(func() {
		_ = c.DeleteRepo(ctx, org, "api")
		_ = c.DeleteOrg(ctx, org)
		_ = c.DeleteUser(ctx, user)
	})

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	email := user + "@agents." + org + ".invalid"
	u, err := c.EnsureAgentUser(ctx, user, email, "Agent", "Pw-"+hex.EncodeToString(sfx)+"-x9!")
	must(err)
	if !u.Active {
		t.Error("user not active")
	}
	_, err = c.EnsureAgentUser(ctx, user, email, "Agent", "ignored")
	must(err)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, _ := ssh.NewPublicKey(pub)
	must(c.EnsureUserKey(ctx, user, "swarm", string(ssh.MarshalAuthorizedKey(sp))))
	must(c.EnsureUserKey(ctx, user, "swarm", string(ssh.MarshalAuthorizedKey(sp))))
	ks, err := c.ListUserKeys(ctx, user)
	must(err)
	if len(ks) != 1 {
		t.Errorf("keys = %d, want 1", len(ks))
	}

	kp, err := NewGPGKey("Agent", email)
	must(err)
	sign := func(tok string) (string, error) { return SignToken(kp.ArmoredPrivate, tok) }
	must(c.EnsureGPGKey(ctx, user, kp.KeyID, kp.ArmoredPublic, sign))
	gks, err := c.ListGPGKeys(ctx, user)
	must(err)
	if len(gks) != 1 || !gks[0].Verified {
		t.Errorf("gpg keys = %+v, want one verified", gks)
	}
	must(c.EnsureGPGKey(ctx, user, kp.KeyID, kp.ArmoredPublic, sign))

	at, err := c.CreateUserToken(ctx, user, "swarm-1", AgentTokenScopes)
	must(err)
	if at.SHA1 == "" {
		t.Error("no token secret")
	}
	_, err = c.CreateUserToken(ctx, user, "swarm-2", AgentTokenScopes)
	must(err)
	_, err = c.CreateUserToken(ctx, user, "reader", ReaderTokenScopes) // scope names are accepted
	must(err)
	must(c.DeleteUserToken(ctx, user, "reader"))
	must(c.DeleteTokensExcept(ctx, user, "swarm-", "swarm-2"))
	ts, err := c.ListUserTokens(ctx, user)
	must(err)
	if len(ts) != 1 || ts[0].Name != "swarm-2" {
		t.Errorf("tokens = %+v", ts)
	}

	_, err = c.EnsureOrg(ctx, org, "Test")
	must(err)
	team, err := c.EnsureTeam(ctx, org, Team{Name: "implementers", IncludesAllRepositories: true})
	must(err)
	_, err = c.EnsureTeam(ctx, org, Team{Name: "implementers", IncludesAllRepositories: true})
	must(err)
	_, err = c.EnsureTeam(ctx, org, Team{Name: "mergers", IncludesAllRepositories: true})
	must(err)
	must(c.AddTeamMember(ctx, team.ID, user))
	must(c.AddTeamMember(ctx, team.ID, user))

	repo, err := c.EnsureOrgRepo(ctx, org, "api")
	must(err)
	must(c.EnsureBranchProtection(ctx, org, "api", DefaultBranchProtection(repo.DefaultBranch, 1, true, nil)))
	must(c.EnsureBranchProtection(ctx, org, "api", DefaultBranchProtection(repo.DefaultBranch, 2, true, nil)))
	must(c.EnsureTagProtection(ctx, org, "api", "v*", []string{"mergers"}))
	must(c.EnsureTagProtection(ctx, org, "api", "v*", []string{"mergers"}))

	p, err := c.GetRepoPermission(ctx, org, "api", user)
	must(err)
	if !p.CanRead() {
		t.Errorf("permission = %+v", p)
	}

	h, err := c.EnsureOrgHook(ctx, org, "http://example.invalid/hooks/forgejo/"+org, "s", ForgeNotificationEvents)
	must(err)
	_, err = c.EnsureOrgHook(ctx, org, "http://example.invalid/hooks/forgejo/"+org, "s2", ForgeNotificationEvents)
	must(err)
	hs, err := c.ListOrgHooks(ctx, org)
	must(err)
	if len(hs) != 1 {
		t.Errorf("hooks = %d", len(hs))
	}
	must(c.DeleteOrgHook(ctx, org, h.ID))

	rr, err := c.RegisterOrgRunner(ctx, org, "pool-test", "test", true)
	must(err)
	if rr.UUID == "" || rr.Token == "" {
		t.Errorf("runner = %+v", rr)
	}
	_, err = c.WaitingJobs(ctx, org, []string{"swarm"})
	must(err)
	must(c.DeleteOrgRunner(ctx, org, rr.ID))

	must(c.DisableUser(ctx, user))
}
