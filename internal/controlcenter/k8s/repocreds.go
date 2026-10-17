package k8s

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Repository credentials (docs/conventions.md) are the import and push
// mirror credentials people enter in the workspace wizard. Each is its own
// Secret in the workspace namespace, named by the repository's
// importSecretName or mirrorSecretName, with keys token and, optionally,
// username; only the operator reads them, and hands them to Forgejo.
//
// In a workspace namespace the control center may create Secrets but not
// read, change or delete them (create can't be limited by name), so a new
// credential always gets a new Secret with a fresh name.

// RepoCredentialComponent is the swarmageddon.dev/component label of
// repository credential Secrets.
const RepoCredentialComponent = "repo-credential"

// ErrWorkspaceNamespaceMissing means the operator hasn't created the
// workspace namespace yet.
var ErrWorkspaceNamespaceMissing = errors.New("the workspace's namespace is not there yet; apply again in a moment")

// RepoCredentialSecretName returns a fresh Secret name for a repository's
// credential: repo-<repo>-<kind>-<random>, kind "import" or "mirror".
func RepoCredentialSecretName(repo, kind string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(repo) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) > 200 {
		name = name[:200]
	}
	return "repo-" + name + "-" + kind + "-" + strings.ToLower(rand.Text()[:8])
}

// CreateRepoCredential creates a credential Secret in a workspace namespace,
// waiting up to wait for the namespace. An empty username is left out (the
// operator then uses oauth2). Creating a name that exists is an error: names
// come from RepoCredentialSecretName.
func (c *Client) CreateRepoCredential(ctx context.Context, workspace, name, repo, kind, username, token string, wait time.Duration) error {
	data := map[string][]byte{"token": []byte(token)}
	if username != "" {
		data["username"] = []byte(username)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: v1alpha1.WorkspaceNamespace(workspace),
			Labels: map[string]string{v1alpha1.LabelWorkspace: workspace, v1alpha1.LabelComponent: RepoCredentialComponent},
			Annotations: map[string]string{
				"swarmageddon.dev/repository": repo,
				"swarmageddon.dev/credential": kind,
			}},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	deadline := time.Now().Add(wait)
	for {
		err := c.C.Create(ctx, s.DeepCopy())
		// NotFound on create: the namespace isn't there yet.
		if !apierrors.IsNotFound(err) {
			return err
		}
		if time.Now().After(deadline) {
			return ErrWorkspaceNamespaceMissing
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
