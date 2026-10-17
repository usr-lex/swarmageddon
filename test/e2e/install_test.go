//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

func (s *suite) platform() (*v1.Platform, error) {
	var p v1.Platform
	err := s.kube.Get(context.Background(), types.NamespacedName{Name: v1.PlatformName}, &p)
	return &p, err
}

func (s *suite) setupSecret() (*corev1.Secret, error) {
	var sec corev1.Secret
	err := s.kube.Get(context.Background(), types.NamespacedName{Namespace: v1.SystemNamespace, Name: "setup-token"}, &sec)
	return &sec, err
}

// install checks what `make dev` installed, reads the setup token the way
// NOTES.txt and the welcome Job tell a person to, and connects to the
// control center with the CA pinned (design §3.1, §3.2).
func (s *suite) install(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"swarmageddon-operator", "control-center"} {
		s.eventually(t, 5*time.Minute, name+" available", func() error {
			var d appsv1.Deployment
			if err := s.kube.Get(ctx, types.NamespacedName{Namespace: v1.SystemNamespace, Name: name}, &d); err != nil {
				return err
			}
			if d.Status.AvailableReplicas < 1 {
				return fmt.Errorf("%d available", d.Status.AvailableReplicas)
			}
			return nil
		})
	}
	var p *v1.Platform
	s.eventually(t, 5*time.Minute, "Platform status.url and status.caFingerprint", func() error {
		var err error
		if p, err = s.platform(); err != nil {
			return err
		}
		if p.Status.URL == "" || p.Status.CAFingerprint == "" {
			return fmt.Errorf("url %q, caFingerprint %q", p.Status.URL, p.Status.CAFingerprint)
		}
		return nil
	})
	reset := p.Status.AdminClaimed
	if reset {
		// A second run on the same cluster: recover the way §3.3 documents,
		// `kubectl annotate platform default swarmageddon.dev/reset-setup-token=now`.
		t.Log("admin is already claimed (an earlier run); asking for a new setup token")
		patch := client.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[v1.AnnotationResetSetupToken] = "now"
		if err := s.kube.Patch(ctx, p, patch); err != nil {
			t.Fatal(err)
		}
	}
	// kubectl -n swarmageddon-system get secret setup-token -o jsonpath='{.data.token}' | base64 -d
	s.eventually(t, 3*time.Minute, "Secret setup-token", func() error {
		sec, err := s.setupSecret()
		if err != nil {
			return err
		}
		if p, err = s.platform(); err != nil {
			return err
		}
		if p.Status.AdminClaimed {
			return errors.New("status.adminClaimed is still true")
		}
		s.token = strings.TrimSpace(string(sec.Data["token"]))
		if s.token == "" {
			return errors.New("no key token")
		}
		return nil
	})
	if out, err := s.kc("-n", v1.SystemNamespace, "logs", "job/welcome"); err == nil {
		if m := regexp.MustCompile(`Setup token:\s+(\S+)`).FindStringSubmatch(out); m != nil && m[1] != s.token && !reset {
			t.Errorf("the welcome Job printed a different setup token than Secret setup-token holds")
		}
		if !strings.Contains(out, p.Status.CAFingerprint) {
			t.Errorf("the welcome Job doesn't print the CA fingerprint %s:\n%s", p.Status.CAFingerprint, clip(out))
		}
	} else {
		t.Logf("welcome Job log not available (it may have expired): %s", clip(out))
	}

	var bundle corev1.ConfigMap
	if err := s.kube.Get(ctx, types.NamespacedName{Namespace: v1.SystemNamespace, Name: "platform-ca-bundle"}, &bundle); err != nil {
		t.Fatal(err)
	}
	ca, err := pinCA([]byte(bundle.Data["ca.crt"]), p.Status.CAFingerprint)
	if err != nil {
		t.Fatal(err)
	}

	base := os.Getenv("E2E_URL")
	if base == "" {
		base = "https://localhost:8443/"
	}
	status := func(c *cc) func() error {
		return func() error {
			st, err := call(c.setup.GetSetupStatus, &ccv1.GetSetupStatusRequest{})
			if err != nil {
				return err
			}
			if st.GetCaFingerprint() != p.Status.CAFingerprint {
				return fmt.Errorf("GetSetupStatus shows CA fingerprint %q, the Platform %q", st.GetCaFingerprint(), p.Status.CAFingerprint)
			}
			return nil
		}
	}
	s.cc = newCC(base, ca)
	if s.lbErr = s.wait(2*time.Minute, status(s.cc)); s.lbErr != nil {
		pf, err := s.portForward()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s didn't answer (%v); using kubectl port-forward at %s", base, s.lbErr, pf)
		s.cc = newCC(pf, ca)
		s.eventually(t, 2*time.Minute, "GetSetupStatus through a port-forward", status(s.cc))
	}
	st, err := call(s.cc.setup.GetSetupStatus, &ccv1.GetSetupStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetAdminClaimed() || st.GetUrl() != p.Status.URL {
		t.Errorf("GetSetupStatus: admin_claimed %v, url %q; want false and %q", st.GetAdminClaimed(), st.GetUrl(), p.Status.URL)
	}
}

// ingress reports whether a person reaches the control center the ways
// make dev and the Platform status say (design §3.2).
func (s *suite) ingress(t *testing.T) {
	if s.lbErr != nil {
		t.Errorf("the control center isn't reachable with the pinned CA through the k3d load balancer: %v", s.lbErr)
	}
	p, err := s.platform()
	if err != nil {
		t.Fatal(err)
	}
	c := newCC(p.Status.URL, s.cc.ca)
	if err := s.wait(time.Minute, func() error {
		_, err := call(c.setup.GetSetupStatus, &ccv1.GetSetupStatusRequest{})
		return err
	}); err != nil {
		t.Errorf("status.url %s isn't reachable with the pinned CA: %v", p.Status.URL, err)
	}
}

// claimAdmin claims the installation with the setup token and creates the
// local admin, then signs in as that admin from a fresh browser (§3.3).
func (s *suite) claimAdmin(t *testing.T) {
	_, err := call(s.cc.setup.ClaimAdmin, &ccv1.ClaimAdminRequest{Token: "WRONG" + s.token, Username: admin, Password: "a wrong attempt first"})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("ClaimAdmin with a wrong token: %v, want PermissionDenied", err)
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	s.password = "e2e-" + hex.EncodeToString(b)
	res, err := call(s.cc.setup.ClaimAdmin, &ccv1.ClaimAdminRequest{Token: s.token, Username: admin, Password: s.password, Email: "admin@example.com"})
	if err != nil {
		t.Fatalf("ClaimAdmin: %v", err)
	}
	if res.GetUser().GetRole() != ccv1.Role_ROLE_ADMIN {
		t.Errorf("ClaimAdmin signed in %v, want an admin", res.GetUser())
	}
	st, err := call(s.cc.setup.GetSetupStatus, &ccv1.GetSetupStatusRequest{})
	if err != nil || !st.GetAdminClaimed() {
		t.Errorf("GetSetupStatus after the claim: %v, %v; want admin_claimed", st, err)
	}
	s.eventually(t, 2*time.Minute, "the operator deletes Secret setup-token", func() error {
		_, err := s.setupSecret()
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("still there (%v)", err)
	})
	if _, err := call(s.cc.setup.ClaimAdmin, &ccv1.ClaimAdminRequest{Token: s.token}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("ClaimAdmin again after the claim: %v, want FailedPrecondition", err)
	}

	// Sign in from a fresh browser with the new local account.
	c := newCC(s.cc.base, s.cc.ca)
	prov, err := call(c.auth.ListProviders, &ccv1.ListProvidersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !prov.GetAdminClaimed() || !slices.ContainsFunc(prov.GetProviders(), func(p *ccv1.Provider) bool {
		return p.GetKind() == ccv1.ProviderKind_PROVIDER_KIND_LOCAL
	}) {
		t.Errorf("ListProviders after the claim: %v; want admin claimed and local sign-in", prov)
	}
	if _, err := call(c.auth.LocalLogin, &ccv1.LocalLoginRequest{Username: admin, Password: s.password + "x"}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("LocalLogin with a wrong password: %v, want Unauthenticated", err)
	}
	if _, err := call(c.auth.LocalLogin, &ccv1.LocalLoginRequest{Username: admin, Password: s.password}); err != nil {
		t.Fatalf("LocalLogin: %v", err)
	}
	me, err := call(c.auth.GetMe, &ccv1.GetMeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if me.GetUser().GetRole() != ccv1.Role_ROLE_ADMIN || me.GetUser().GetHandle() != admin {
		t.Fatalf("GetMe: %v; want the admin %q", me.GetUser(), admin)
	}
	s.cc = c
	gp, err := call(s.cc.platform.GetPlatform, &ccv1.GetPlatformRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !gp.GetPlatform().GetStatus().GetAdminClaimed() {
		t.Error("GetPlatform: status.adminClaimed is false after the claim")
	}
}

// preflight reads the checks through the API, as the wizard's first step
// does, and accepts the privileged tier explicitly (k3d on a CI runner has
// no user namespaces for pods), then waits for preflight to run again.
func (s *suite) preflight(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), s.until(10*time.Minute))
	defer cancel()
	stream, err := s.cc.platform.WatchPlatform(ctx, connect.NewRequest(&ccv1.WatchPlatformRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var first *ccv1.Platform
	for stream.Receive() {
		if p := stream.Msg().GetPlatform(); p.GetStatus().GetPreflightDone() {
			first = p
			break
		}
	}
	_ = stream.Close()
	if first == nil {
		t.Fatalf("WatchPlatform: preflight not done: %v", stream.Err())
	}
	logChecks(t, "first preflight", first.GetStatus())

	if _, err := call(s.cc.platform.SetIsolation, &ccv1.SetIsolationRequest{Tier: "privileged"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("SetIsolation(privileged) without acceptance: %v, want InvalidArgument", err)
	}
	res, err := call(s.cc.platform.SetIsolation, &ccv1.SetIsolationRequest{Tier: "privileged", PrivilegedAccepted: true})
	if err != nil {
		t.Fatalf("SetIsolation(privileged, accepted): %v", err)
	}
	p := res.GetPlatform()
	if p.GetIsolationTier() != "privileged" || !p.GetPrivilegedAccepted() {
		t.Errorf("SetIsolation returned tier %q, accepted %v", p.GetIsolationTier(), p.GetPrivilegedAccepted())
	}
	want := p.GetStatus().GetPreflightRerunRequested()
	var st *ccv1.PlatformStatus
	s.eventually(t, 10*time.Minute, "preflight re-run with the privileged tier", func() error {
		gp, err := call(s.cc.platform.GetPlatform, &ccv1.GetPlatformRequest{})
		if err != nil {
			return err
		}
		st = gp.GetPlatform().GetStatus()
		if want != "" && st.GetPreflightRerun() != want {
			return fmt.Errorf("status.preflightRerun %q, requested %q", st.GetPreflightRerun(), want)
		}
		if !st.GetPreflightDone() || st.GetTier() != "privileged" {
			return fmt.Errorf("done %v, tier %q", st.GetPreflightDone(), st.GetTier())
		}
		return nil
	})
	logChecks(t, "preflight with the privileged tier", st)
	results := map[string]string{}
	for _, c := range st.GetPreflight() {
		results[c.GetName()] = c.GetResult()
	}
	for _, name := range []string{"kubernetesVersion", "defaultStorageClass", "networkPolicy", "privilegedAdmission", "clusterDNS"} {
		if r := results[name]; r != "pass" && r != "warn" {
			t.Errorf("preflight check %s: %q, want pass", name, r)
		}
	}
	if results["isolationTier"] == "fail" {
		t.Error("preflight check isolationTier fails with the privileged tier accepted")
	}
}

func logChecks(t *testing.T, what string, st *ccv1.PlatformStatus) {
	t.Helper()
	var b strings.Builder
	for _, c := range st.GetPreflight() {
		fmt.Fprintf(&b, "\n  %-24s %-5s %s", c.GetName(), c.GetResult(), c.GetMessage())
	}
	t.Logf("%s: tier %q, room estimate %d:%s", what, st.GetTier(), st.GetRoomEstimate(), b.String())
}

// serviceConditions are the platform services the wizard waits for (§3.4).
var serviceConditions = []string{"PostgresReady", "ForgejoReady", "MessagingReady", "GatewayReady", "EgressReady", "RegistryCacheReady"}

// services adds the mock model as a provider and approves the services, as
// the wizard's services step does, then waits for them through the API.
func (s *suite) services(t *testing.T) {
	models := []string{"mock-claude", "mock-ore", "mock-small"}
	res, err := call(s.cc.platform.UpdateServices, &ccv1.UpdateServicesRequest{Services: &ccv1.ServicesSettings{
		MessagingReplicas: 1, // one node
		Providers:         []*ccv1.ModelProviderSettings{{Name: "mock", Kind: "openaiCompatible", BaseUrl: mockLM, Models: models}},
		Approved:          true,
	}})
	if err != nil {
		t.Fatalf("UpdateServices: %v", err)
	}
	if !res.GetPlatform().GetServices().GetApproved() {
		t.Error("UpdateServices: services not approved in the returned Platform")
	}
	s.eventually(t, 25*time.Minute, "platform services ready", func() error {
		gp, err := call(s.cc.platform.GetPlatform, &ccv1.GetPlatformRequest{})
		if err != nil {
			return err
		}
		return notTrue(gp.GetPlatform().GetStatus().GetConditions(), serviceConditions...)
	})
	s.eventually(t, 5*time.Minute, "the control center connects to PostgreSQL", func() error {
		st, err := call(s.cc.setup.GetSetupStatus, &ccv1.GetSetupStatusRequest{})
		if err != nil {
			return err
		}
		if !st.GetDatabaseReady() {
			return errors.New("database_ready is false")
		}
		return nil
	})
	lm, err := call(s.cc.workspaces.ListModels, &ccv1.ListModelsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if !slices.Contains(lm.GetModels(), m) {
			t.Errorf("ListModels: %v, missing %s", lm.GetModels(), m)
		}
	}
	// The session started before PostgreSQL; it must carry over (§8.1).
	if me, err := call(s.cc.auth.GetMe, &ccv1.GetMeRequest{}); err != nil || me.GetUser().GetId() == "" {
		t.Errorf("GetMe once the database is up: %v, %v; want a stored user", me.GetUser(), err)
	}
}
