//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// secretKey matches the Secret keys that hold generated secret values:
// tokens, passwords, API and master keys, room credentials.
var secretKey = regexp.MustCompile(`(?i)(token|password|secret|apikey|masterkey|privatekey|^key$|_key$|_auth$)`)

// secretValue is one value to look for, with where it came from (never the
// value itself in a message).
type secretValue struct {
	where, value string
	// exempt: pods allowed to print it. The operator prints the setup token
	// once by design (§3.3), and the welcome Job prints it for the person.
	exempt []string
}

// generatedSecrets reads every single-line secret value in the platform's
// Secrets, plus the setup token and the admin's password.
func (s *suite) generatedSecrets(t *testing.T) []secretValue {
	out := []secretValue{{where: "the setup token", value: s.token, exempt: []string{"swarmageddon-operator-", "welcome-"}}}
	if s.password != "" {
		out = append(out, secretValue{where: "the local admin's password", value: s.password})
	}
	for _, ns := range []string{v1.SystemNamespace, v1.PlatformNamespace, wsNS} {
		var l corev1.SecretList
		if err := s.kube.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		for _, sec := range l.Items {
			if sec.Name == "setup-token" || sec.Type == corev1.SecretTypeServiceAccountToken || strings.HasPrefix(sec.Name, "sh.helm.") {
				continue
			}
			for k, v := range sec.Data {
				val := strings.TrimSpace(string(v))
				if secretKey.MatchString(k) && !strings.HasSuffix(k, ".pub") && printable(val) && len(val) >= 12 {
					out = append(out, secretValue{where: fmt.Sprintf("Secret %s/%s key %s", ns, sec.Name, k), value: val})
				}
			}
		}
	}
	return out
}

func printable(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return v != ""
}

// noSecretsInLogs greps the logs of every pod in the system, platform and
// workspace namespaces (operator, control center, messaging, warden,
// egress, the services, rooms' supervisors) for every generated secret
// value (§11.4).
func (s *suite) noSecretsInLogs(t *testing.T) {
	secrets := s.generatedSecrets(t)
	t.Logf("looking for %d secret values", len(secrets))
	for _, ns := range []string{v1.SystemNamespace, v1.PlatformNamespace, wsNS} {
		var pods corev1.PodList
		if err := s.kube.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		for _, p := range pods.Items {
			logs, err := s.kc("-n", ns, "logs", p.Name, "--all-containers")
			if err != nil {
				t.Logf("%s/%s: no logs: %s", ns, p.Name, clip(logs))
			}
			if prev, err := s.kc("-n", ns, "logs", p.Name, "--all-containers", "--previous"); err == nil {
				logs += prev
			}
			for _, sv := range secrets {
				if n := strings.Count(logs, sv.value); n > 0 && !exempt(p.Name, sv.exempt) {
					t.Errorf("%s/%s's log contains %s (%d times)", ns, p.Name, sv.where, n)
				}
			}
		}
	}
}

func exempt(pod string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(pod, p) {
			return true
		}
	}
	return false
}
