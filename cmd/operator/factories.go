package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
)

// Service URLs inside the cluster (docs/conventions.md).
var (
	forgejoURL = "http://forgejo." + v1alpha1.PlatformNamespace + ".svc:3000"
	gatewayURL = "http://litellm." + v1alpha1.PlatformNamespace + ".svc:4000" // admin API, past the warden
)

// serviceReady fails with deps.ErrNotReady until the Platform reports the
// condition true.
func serviceReady(ctx context.Context, c client.Client, condition string) error {
	var p v1alpha1.Platform
	if err := c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p); err != nil {
		return fmt.Errorf("platform: %v: %w", err, deps.ErrNotReady)
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, condition) {
		return fmt.Errorf("%s is not true: %w", condition, deps.ErrNotReady)
	}
	return nil
}

func secretValue(ctx context.Context, c client.Client, name, key string) (string, error) {
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: name}, &s); err != nil {
		return "", fmt.Errorf("secret %s: %v: %w", name, err, deps.ErrNotReady)
	}
	v := string(s.Data[key])
	if v == "" {
		return "", fmt.Errorf("secret %s has no %s yet: %w", name, key, deps.ErrNotReady)
	}
	return v, nil
}

// ForgeFactory returns admin clients for Forgejo once it is ready, using the
// token in Secret forgejo-admin.
func ForgeFactory(c client.Client) func(context.Context) (*forgejo.Client, error) {
	return func(ctx context.Context) (*forgejo.Client, error) {
		if err := serviceReady(ctx, c, v1alpha1.ConditionForgejoReady); err != nil {
			return nil, err
		}
		tok, err := secretValue(ctx, c, "forgejo-admin", "token")
		if err != nil {
			return nil, err
		}
		return forgejo.New(forgejoURL, tok), nil
	}
}

// GatewayFactory returns master-key clients for LiteLLM once it is ready.
func GatewayFactory(c client.Client) func(context.Context) (*gateway.Client, error) {
	return func(ctx context.Context) (*gateway.Client, error) {
		if err := serviceReady(ctx, c, v1alpha1.ConditionGatewayReady); err != nil {
			return nil, err
		}
		key, err := secretValue(ctx, c, "gateway-master", "masterKey")
		if err != nil {
			return nil, err
		}
		return gateway.New(gatewayURL, key), nil
	}
}

// PlatformTier is the tier forced in Platform spec, or the one preflight chose.
func PlatformTier(c client.Client) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var p v1alpha1.Platform
		if err := c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p); err != nil {
			return "", err
		}
		if t := p.Spec.IsolationTier; t != "" && t != v1alpha1.TierAuto {
			return string(t), nil
		}
		if p.Status.Preflight != nil && p.Status.Preflight.Tier != "" {
			return string(p.Status.Preflight.Tier), nil
		}
		return "", fmt.Errorf("preflight has not chosen a tier yet: %w", deps.ErrNotReady)
	}
}
