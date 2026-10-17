//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// forgeAPI calls Forgejo's API from inside a room with that room's own token
// (what `tea api` does) and filters the JSON with jq.
func (s *suite) forgeAPI(room, path, jq string) (string, error) {
	out, err := s.inRoomAsRoot(room, fmt.Sprintf(`curl -sS -f -H "Authorization: token $(cat /run/swarm/identity/forge_token)" `+
		`"$(jq -r .forgeURL /etc/swarm/room.json | sed 's|/*$||')/api/v1/%s" | jq -r '%s'`, path, jq))
	return strings.TrimSpace(out), err
}

// cloned: every room has its spec.repos cloned into ~/work by its
// supervisor at start (§6.3).
func (s *suite) cloned(t *testing.T) {
	for _, r := range planned {
		if out, err := s.inRoom(r, "test -d ~/work/api/.git"); err != nil {
			logs, _ := s.kc("-n", wsNS, "logs", podOf(r), "-c", "room")
			var why []string
			for _, l := range strings.Split(logs, "\n") {
				if strings.Contains(l, "clone") {
					why = append(why, l)
				}
			}
			t.Errorf("%s: acme/api not cloned into ~/work/api (%s); supervisor: %s", r, clip(out), clip(strings.Join(why, "\n")))
		}
	}
}

// identityReadable: the agent user can read its own identity Secret, which
// the room mounts for it (design §6.2; conventions: mode 0440, group agent).
func (s *suite) identityReadable(t *testing.T) {
	for _, r := range planned {
		if out, err := s.inRoom(r, "for k in forge_token gateway_key id_ed25519; do cat /run/swarm/identity/$k > /dev/null || exit 1; done"); err != nil {
			t.Errorf("%s: the agent can't read its identity: %v: %s", r, err, clip(out))
		}
	}
}

// signedPush commits on a branch in athena's clone, signed with her GPG key,
// pushes it, and checks that main itself refuses pushes, force pushes and
// deletion (§9.2, §9.3). The branch carries a workflow for the CI step.
func (s *suite) signedPush(t *testing.T) {
	s.branch = "e2e-" + s.nonce
	m := s.marker("commit")
	if _, err := s.inRoom("athena", "test -d ~/work/api/.git"); err != nil {
		t.Log("athena has no clone of acme/api (see the cloned-at-start step); cloning it by hand")
		if out, err := s.inRoom("athena", `git clone -q "ssh://git@$(jq -r .forgeSSHHost /etc/swarm/room.json)/acme/api.git" ~/work/api`); err != nil {
			t.Fatalf("cloning it by hand: %v: %s", err, clip(out))
		}
	}
	script := fmt.Sprintf(`set -e
cd ~/work/api
git fetch -q origin
git switch -q -c %[1]s origin/main
mkdir -p .forgejo/workflows
printf 'name: e2e\non: [pull_request]\njobs:\n  check:\n    runs-on: %[2]s\n    steps:\n      - run: echo %[3]s\n' > .forgejo/workflows/e2e.yaml
echo %[3]s > %[3]s.txt
git add -A
git commit -q -m 'e2e: %[3]s'
echo "SIG=$(git log -1 --format=%%G?)"
echo "SHA=$(git rev-parse HEAD)"
git push -q origin %[1]s 2>&1`, s.branch, runner, m)
	out, err := s.inRoom("athena", script)
	if err != nil {
		t.Fatalf("commit and push a branch: %v: %s", err, clip(out))
	}
	if sig := field(out, "SIG"); sig != "G" {
		t.Errorf("git log %%G? = %q, want G (a good signature): %s", sig, clip(out))
	}
	s.sha = field(out, "SHA")
	got, err := s.forgeAPI("athena", "repos/acme/api/git/commits/"+s.sha, `(.commit.verification // .verification) | "\(.verified) \(.reason)"`)
	if err != nil || !strings.HasPrefix(got, "true") {
		t.Errorf("Forgejo doesn't show %s as verified: %v: %s", s.sha, err, got)
	}
	for _, push := range []string{"git push origin HEAD:main", "git push --force origin HEAD:main", "git push origin --delete main"} {
		out, err := s.inRoom("athena", "cd ~/work/api && "+push+" 2>&1")
		if err == nil {
			t.Errorf("%s on the protected main was accepted: %s", push, clip(out))
		} else if !strings.Contains(strings.ToLower(out), "protected") {
			t.Logf("%s was refused, though not naming the protection: %s", push, clip(out))
		}
	}
}

// pullRequest opens a pull request from athena's branch with tea.
func (s *suite) pullRequest(t *testing.T) {
	out, err := s.inRoom("athena", fmt.Sprintf("cd ~/work/api && tea pulls create --repo acme/api --head %s --base main --title 'e2e %s' --description 'from the e2e suite'",
		s.branch, s.marker("pr")))
	if err != nil {
		t.Fatalf("tea pulls create: %v: %s", err, clip(out))
	}
	n, err := s.forgeAPI("athena", "repos/acme/api/pulls?state=open&limit=50", fmt.Sprintf(`.[] | select(.head.ref == "%s") | .number`, s.branch))
	if s.pr, _ = strconv.Atoi(n); err != nil || s.pr == 0 {
		t.Fatalf("no open pull request from %s: %v: %s", s.branch, err, n)
	}
}

// ciRunner waits for the pull request's workflow to run on an ephemeral
// runner pod and pass (§9.4, RunnerPool).
func (s *suite) ciRunner(t *testing.T) {
	ctx := context.Background()
	var pod string
	err := s.wait(15*time.Minute, func() error {
		var pods corev1.PodList
		if err := s.kube.List(ctx, &pods, client.InNamespace(wsNS), client.MatchingLabels{v1.LabelComponent: "runner"}); err != nil {
			return err
		}
		if len(pods.Items) > 0 && pod == "" {
			pod = pods.Items[0].Name
			t.Logf("runner pod %s started", pod)
		}
		state, err := s.forgeAPI("athena", "repos/acme/api/commits/"+s.sha+"/status", ".state")
		if err != nil {
			return err
		}
		if state != "success" {
			return fmt.Errorf("commit status %q", state)
		}
		return nil
	})
	if err != nil {
		var pool v1.RunnerPool
		_ = s.kube.Get(ctx, client.ObjectKey{Namespace: wsNS, Name: "default"}, &pool)
		tasks, _ := s.forgeAPI("athena", "repos/acme/api/actions/tasks", `.workflow_runs[]? | "\(.name) \(.status)"`)
		t.Fatalf("the workflow on %s didn't pass: %v (runner pod seen: %q; pool status %+v; tasks: %s)", s.sha, err, pod, pool.Status, tasks)
	}
	if pod == "" {
		t.Fatal("the job passed but no runner pod was seen in " + wsNS)
	}
	s.eventually(t, 5*time.Minute, "the runner pod is gone after its one job", func() error {
		var p corev1.Pod
		if err := s.kube.Get(ctx, client.ObjectKey{Namespace: wsNS, Name: pod}, &p); client.IgnoreNotFound(err) != nil {
			return err
		} else if err == nil {
			return fmt.Errorf("%s is %s", pod, p.Status.Phase)
		}
		return nil
	})
}

// review approves the pull request as artemis, a reviewer.
func (s *suite) review(t *testing.T) {
	out, err := s.inRoom("artemis", fmt.Sprintf("tea pulls approve --repo acme/api %d 'LGTM %s'", s.pr, s.marker("review")))
	if err != nil {
		t.Fatalf("artemis: tea pulls approve: %v: %s", err, clip(out))
	}
	got, err := s.forgeAPI("artemis", fmt.Sprintf("repos/acme/api/pulls/%d/reviews", s.pr), `[.[] | select(.state == "APPROVED") | .user.login] | join(",")`)
	if err != nil || !strings.Contains(got, "artemis") {
		t.Errorf("no approval by artemis on #%d: %v: %s", s.pr, err, got)
	}
}

// forgeNotice: the review reaches the pull request's author as a message
// from forge@acme (§9.7).
func (s *suite) forgeNotice(t *testing.T) {
	s.delivered(t, "athena", s.marker("review"))
}

// merge: athena, an implementer outside the mergers team, may not merge;
// artemis, a reviewer in it, may (§9.3).
func (s *suite) merge(t *testing.T) {
	merge := fmt.Sprintf("tea pulls merge --repo acme/api --style merge %d", s.pr)
	out, err := s.inRoom("athena", merge)
	merged, _ := s.forgeAPI("athena", fmt.Sprintf("repos/acme/api/pulls/%d", s.pr), ".merged")
	if err == nil && merged == "true" {
		t.Fatalf("athena (not in mergers) merged #%d: %s", s.pr, clip(out))
	}
	out, err = s.inRoom("artemis", merge)
	if err != nil {
		t.Fatalf("artemis (mergers): %s: %v: %s", merge, err, clip(out))
	}
	s.eventually(t, time.Minute, "the pull request merged", func() error {
		got, err := s.forgeAPI("artemis", fmt.Sprintf("repos/acme/api/pulls/%d", s.pr), `"\(.merged) \(.merged_by.login)"`)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(got, "true artemis") {
			return errors.New("merged, merged_by: " + got)
		}
		return nil
	})
}
