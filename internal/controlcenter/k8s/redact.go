package k8s

import (
	"context"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/internal/redact"
)

// RedactSecrets wraps c so that the values of every Secret read or written
// through it are added to the log redaction registry (docs/design.md §11.4):
// session keys, tokens, gateway and provider keys, client secrets, webhook
// URLs and the database URL's password.
func RedactSecrets(c client.Client) client.Client { return secretRedactor{c} }

type secretRedactor struct{ client.Client }

func (c secretRedactor) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if err == nil {
		registerSecret(obj)
	}
	return err
}

func (c secretRedactor) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := c.Client.List(ctx, list, opts...)
	if l, ok := list.(*corev1.SecretList); ok && err == nil {
		for i := range l.Items {
			registerSecret(&l.Items[i])
		}
	}
	return err
}

func (c secretRedactor) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	registerSecret(obj)
	return c.Client.Create(ctx, obj, opts...)
}

func (c secretRedactor) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	registerSecret(obj)
	return c.Client.Update(ctx, obj, opts...)
}

func (c secretRedactor) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	err := c.Client.Patch(ctx, obj, patch, opts...)
	registerSecret(obj)
	return err
}

// publicKeys are Secret keys whose values aren't secret, and would hide
// ordinary log text (names, addresses, certificates).
var publicKeys = map[string]bool{"username": true, "email": true, "host": true, "port": true, "from": true, "to": true,
	"publicKey": true, "ca.crt": true, "tls.crt": true, "database": true}

func registerSecret(obj client.Object) {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	add := func(k, v string) {
		if publicKeys[k] {
			return
		}
		// A URL's password is secret, and so is one whose path or query
		// may hold a token (webhooks); a bare service URL is not.
		if u, err := url.Parse(v); err == nil && u.Scheme != "" && u.Host != "" {
			if p, ok := u.User.Password(); ok {
				redact.Add(p)
			}
			if (u.Path == "" || u.Path == "/") && u.RawQuery == "" {
				return
			}
		}
		redact.Add(v)
	}
	for k, v := range s.Data {
		add(k, string(v))
	}
	for k, v := range s.StringData {
		add(k, v)
	}
}
