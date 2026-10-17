package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // client-go's spdy dialer still returns these types
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// ConditionForgejoOIDC reports people's sign-in to Forgejo through the
// platform's OIDC provider (design §9.6).
const ConditionForgejoOIDC = "ForgejoOIDCReady"

// annForgejoOIDC on the Forgejo pod is a hash of the auth source last
// written into it. A new pod (and so a new database, should it come to
// that) has none, and gets the source written again.
const annForgejoOIDC = "swarmageddon.dev/oidc-login"

const forgejoPod, forgejoContainer = "forgejo-0", "forgejo"

// Execer runs a command in a pod's container (pods/exec).
type Execer interface {
	Exec(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// KubeExec runs commands through the API server, over WebSocket, falling
// back to SPDY.
type KubeExec struct{ Config *rest.Config }

// Exec implements Execer.
func (k KubeExec) Exec(ctx context.Context, ns, pod, container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cs, err := kubernetes.NewForConfig(k.Config)
	if err != nil {
		return err
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdin: stdin != nil, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(k.Config, "GET", req.URL().String())
	if err != nil {
		return err
	}
	sp, err := remotecommand.NewSPDYExecutor(k.Config, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, sp, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

// forgejoOIDC lets people sign in to Forgejo with the first OIDC provider
// when services.forgejo.oidcLogin is set. Forgejo has no API for auth
// sources, so it runs `forgejo admin auth add-oauth` (or `update-oauth`
// when the source exists) in the Forgejo pod. The client secret goes in on
// stdin, so it never appears in the exec request. Failures are reported on
// ForgejoOIDCReady and retried; they don't hold up the platform. It returns
// how soon to look again.
func (r *Reconciler) forgejoOIDC(ctx context.Context, p *v1alpha1.Platform) time.Duration {
	if !p.Spec.Services.Forgejo.OIDCLogin {
		meta.RemoveStatusCondition(&p.Status.Conditions, ConditionForgejoOIDC)
		return 0
	}
	fail := func(reason, msg string, retry time.Duration) time.Duration {
		setCond(p, ConditionForgejoOIDC, false, reason, msg)
		return retry
	}
	switch {
	case len(p.Spec.Auth.OIDC) == 0:
		return fail("NoProvider", "services.forgejo.oidcLogin uses the first provider in auth.oidc, and there is none", 0)
	case p.Spec.Hostname == "":
		return fail("NoHostname", "people reach Forgejo at forge.<hostname>; set spec.hostname", 0)
	case !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionForgejoReady):
		return fail("ForgejoNotReady", "waiting for Forgejo", 0)
	case r.Exec == nil:
		return fail("NoExec", "the operator can't run commands in pods", 0)
	}
	prov := p.Spec.Auth.OIDC[0]
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: prov.ClientSecretRef.Name}, &sec); err != nil {
		return fail("NoClientSecret", fmt.Sprintf("Secret %s/%s: %v", v1alpha1.SystemNamespace, prov.ClientSecretRef.Name, err), time.Minute)
	}
	secret := string(sec.Data["clientSecret"])
	if secret == "" {
		return fail("NoClientSecret", fmt.Sprintf("Secret %s/%s has no key clientSecret", v1alpha1.SystemNamespace, prov.ClientSecretRef.Name), time.Minute)
	}
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: forgejoPod}, &pod); err != nil || !podReady(&pod) {
		return fail("ForgejoNotReady", "waiting for pod "+forgejoPod, 30*time.Second)
	}

	flags := oauthFlags(prov, adminGroup(p.Spec.Auth.RoleMappings))
	h := sha256.Sum256([]byte(strings.Join(flags, "\x00") + "\x00" + secret))
	sum := hex.EncodeToString(h[:8])
	callback := "https://forge." + p.Spec.Hostname + "/user/oauth2/" + prov.Name + "/callback"
	done := fmt.Sprintf("people sign in to Forgejo with %s; the identity provider must allow the redirect URI %s", prov.Name, callback)
	if pod.Annotations[annForgejoOIDC] == sum {
		setCond(p, ConditionForgejoOIDC, true, "Configured", done)
		return 0
	}

	var out, errOut bytes.Buffer
	if err := r.Exec.Exec(ctx, v1alpha1.PlatformNamespace, forgejoPod, forgejoContainer, []string{"forgejo", "admin", "auth", "list"}, nil, &out, &errOut); err != nil {
		return fail("ExecFailed", fmt.Sprintf("forgejo admin auth list: %v: %s", err, strings.TrimSpace(errOut.String())), time.Minute)
	}
	verb := []string{"add-oauth"}
	if id, ok := authSourceID(out.String(), prov.Name); ok {
		verb = []string{"update-oauth", "--id", strconv.FormatInt(id, 10)}
	}
	// sh reads the secret from stdin; "$@" are the flags.
	cmd := append([]string{"sh", "-c", `exec forgejo admin auth "$@" --secret "$(cat)"`, "sh"}, append(verb, flags...)...)
	errOut.Reset()
	if err := r.Exec.Exec(ctx, v1alpha1.PlatformNamespace, forgejoPod, forgejoContainer, cmd, strings.NewReader(secret), io.Discard, &errOut); err != nil {
		return fail("ExecFailed", fmt.Sprintf("forgejo admin auth %s: %v: %s", verb[0], err, strings.TrimSpace(errOut.String())), time.Minute)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{annForgejoOIDC: sum}}})
	if err := r.Patch(ctx, &pod, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fail("Error", "annotate "+forgejoPod+": "+err.Error(), time.Minute)
	}
	setCond(p, ConditionForgejoOIDC, true, "Configured", done)
	return 0
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// adminGroup is the first group mapped to the admin role; its members are
// Forgejo admins.
func adminGroup(ms []v1alpha1.RoleMapping) string {
	for _, m := range ms {
		if m.Role == v1alpha1.RoleAdmin {
			return m.Group
		}
	}
	return ""
}

// oauthFlags are add-oauth's and update-oauth's flags for an OIDC
// provider, without the secret. Flag names are Forgejo 17's
// (cmd/admin_auth_oauth.go).
func oauthFlags(o v1alpha1.OIDCProvider, admin string) []string {
	groups := o.GroupsClaim
	if groups == "" {
		groups = "groups"
	}
	flags := []string{
		"--name", o.Name,
		"--provider", "openidConnect",
		"--key", o.ClientID,
		"--auto-discover-url", strings.TrimRight(o.Issuer, "/") + "/.well-known/openid-configuration",
		"--group-claim-name", groups,
		"--admin-group", admin,
	}
	scopes := []string{"openid", "profile", "email"}
	for _, s := range o.Scopes {
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	for _, s := range scopes {
		flags = append(flags, "--scopes", s)
	}
	return flags
}

// authSourceID finds name in `forgejo admin auth list`: a header, then
// rows of ID, name, type and enabled, separated by tabs.
func authSourceID(list, name string) (int64, bool) {
	for i, line := range strings.Split(list, "\n") {
		if i == 0 {
			continue
		}
		cells := strings.FieldsFunc(line, func(r rune) bool { return r == '\t' })
		if len(cells) < 3 || strings.TrimSpace(cells[1]) != name || strings.TrimSpace(cells[2]) != "OAuth2" {
			continue
		}
		if id, err := strconv.ParseInt(strings.TrimSpace(cells[0]), 10, 64); err == nil {
			return id, true
		}
	}
	return 0, false
}
