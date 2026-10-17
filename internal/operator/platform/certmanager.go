package platform

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Secrets the control center's ingress serves (design §3.2).
const (
	// ControlCenterTLSSecret is the platform CA's serving certificate.
	ControlCenterTLSSecret = "control-center-tls"
	// PublicTLSSecret is cert-manager's certificate for spec.hostname, in
	// the certManager TLS mode.
	PublicTLSSecret = "control-center-public-tls"
	// certificateName is our cert-manager Certificate in the system namespace.
	certificateName = "control-center"
)

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// publicTLS returns the Secret the control center's ingress serves, and how
// soon to look again. In the certManager mode it keeps a cert-manager
// Certificate for spec.hostname from spec.tls.issuerRef and switches to its
// Secret once it is ready; until then, and in every other case, the ingress
// keeps the platform CA's certificate. Progress is on ControlCenterTLSReady.
// Without cert-manager's CRDs it says so there rather than failing.
func (r *Reconciler) publicTLS(ctx context.Context, p *v1alpha1.Platform) (string, time.Duration, error) {
	installed := r.hasKind(certificateGVK)
	if p.Spec.TLS.Mode != v1alpha1.TLSCertManager {
		if installed {
			cert := newCertificate()
			if err := r.Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
				return ControlCenterTLSSecret, 0, err
			}
		}
		return ControlCenterTLSSecret, 0, nil
	}
	const meanwhile = "; the control center keeps the platform CA's certificate meanwhile"
	notReady := func(reason, msg string) {
		setCond(p, v1alpha1.ConditionControlCenterTLS, false, reason, msg+meanwhile)
	}
	issuer := p.Spec.TLS.IssuerRef
	switch {
	case p.Spec.Hostname == "":
		notReady("NoHostname", "cert-manager needs spec.hostname")
		return ControlCenterTLSSecret, 0, nil
	case issuer == nil || issuer.Name == "":
		notReady("NoIssuer", "cert-manager needs spec.tls.issuerRef")
		return ControlCenterTLSSecret, 0, nil
	case !installed:
		notReady("CertManagerMissing", "tls.mode is certManager, but cert-manager is not installed (no cert-manager.io/v1 Certificate kind); "+
			"install it (https://cert-manager.io/docs/installation/) or set tls.mode to selfSigned")
		return ControlCenterTLSSecret, time.Minute, nil
	}
	kind := issuer.Kind
	if kind == "" {
		kind = "ClusterIssuer"
	}
	cert := newCertificate()
	cert.Object["spec"] = map[string]any{
		"secretName": PublicTLSSecret,
		"dnsNames":   []any{p.Spec.Hostname},
		"issuerRef":  map[string]any{"group": "cert-manager.io", "kind": kind, "name": issuer.Name},
	}
	cert.SetLabels(map[string]string{v1alpha1.LabelComponent: "control-center"})
	if err := r.applier().Apply(ctx, p, cert); err != nil {
		return ControlCenterTLSSecret, 0, err
	}
	cur := newCertificate()
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: certificateName}, cur); err != nil {
		return ControlCenterTLSSecret, 0, err
	}
	from := fmt.Sprintf("%s %s for %s", kind, issuer.Name, p.Spec.Hostname)
	ready, msg := certificateReady(cur)
	if !ready {
		if msg == "" {
			msg = "waiting for cert-manager"
		}
		notReady("Issuing", fmt.Sprintf("Certificate %s/%s from %s: %s", v1alpha1.SystemNamespace, certificateName, from, msg))
		return ControlCenterTLSSecret, 30 * time.Second, nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: PublicTLSSecret}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			notReady("Issuing", "Certificate is ready but Secret "+PublicTLSSecret+" is missing")
			return ControlCenterTLSSecret, 30 * time.Second, nil
		}
		return ControlCenterTLSSecret, 0, err
	}
	setCond(p, v1alpha1.ConditionControlCenterTLS, true, "CertManager", "serving Secret "+PublicTLSSecret+" from "+from)
	return PublicTLSSecret, 0, nil
}

func newCertificate() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(certificateGVK)
	u.SetNamespace(v1alpha1.SystemNamespace)
	u.SetName(certificateName)
	return u
}

// certificateReady reads a Certificate's Ready condition.
func certificateReady(u *unstructured.Unstructured) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		msg, _ := m["message"].(string)
		return m["status"] == string(metav1.ConditionTrue), msg
	}
	return false, ""
}

// useTLSSecret points the rendered Ingress objects' TLS at secret.
func useTLSSecret(objs []*unstructured.Unstructured, secret string) error {
	for _, o := range objs {
		if o.GetKind() != "Ingress" {
			continue
		}
		tls, _, err := unstructured.NestedSlice(o.Object, "spec", "tls")
		if err != nil {
			return err
		}
		for i := range tls {
			if m, ok := tls[i].(map[string]any); ok {
				m["secretName"] = secret
			}
		}
		if err := unstructured.SetNestedSlice(o.Object, tls, "spec", "tls"); err != nil {
			return err
		}
	}
	return nil
}
