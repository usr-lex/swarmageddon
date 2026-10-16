package pki

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Secret names and keys (docs/conventions.md).
const (
	CASecretName = "platform-ca"
	CACertKey    = "ca.crt"
	CAKeyKey     = "ca.key"
	TLSCertKey   = "tls.crt"
	TLSKeyKey    = "tls.key"
)

// Store loads the CA from Secret platform-ca, creating it on first use, and
// keeps it in memory. It is safe for concurrent use.
type Store struct {
	// Client creates the Secret. Reader reads it, uncached, so the CA can be
	// read before (or without) a cache on Secrets.
	Client    client.Client
	Reader    client.Reader
	Namespace string
	Now       func() time.Time

	mu sync.Mutex
	ca *CA
}

// CA returns the platform CA, creating its Secret if there is none.
func (s *Store) CA(ctx context.Context) (*CA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ca != nil {
		return s.ca, nil
	}
	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	key := types.NamespacedName{Namespace: s.Namespace, Name: CASecretName}
	for range 2 {
		var sec corev1.Secret
		err := reader.Get(ctx, key, &sec)
		if err == nil {
			ca, err := ParseCA(sec.Data[CACertKey], sec.Data[CAKeyKey])
			if err != nil {
				return nil, fmt.Errorf("secret %s: %w", key, err)
			}
			s.ca = ca
			return ca, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		ca, err := NewCA("swarmageddon platform CA", s.now())
		if err != nil {
			return nil, err
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: CASecretName, Namespace: s.Namespace,
				Labels: map[string]string{"swarmageddon.dev/managed": "true", "swarmageddon.dev/component": "pki"},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{CACertKey: ca.CertPEM, CAKeyKey: ca.KeyPEM},
		}
		err = s.Client.Create(ctx, &sec)
		if err == nil {
			s.ca = ca
			return ca, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Someone else created it first: read theirs.
	}
	return nil, fmt.Errorf("secret %s: could not load or create", key)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// EnsureTLSSecret makes sure Secret key holds a current certificate for req in
// tls.crt and tls.key, plus the CA in ca.crt. It only touches those three
// keys, so other data in the Secret stays. mutate, if not nil, runs on the
// Secret before it is written (to add labels or owner references). It
// reports whether it issued a new certificate.
func EnsureTLSSecret(ctx context.Context, c client.Client, ca *CA, key types.NamespacedName, req Request, now time.Time, mutate func(*corev1.Secret) error) (bool, error) {
	var sec corev1.Secret
	err := c.Get(ctx, key, &sec)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if !exists {
		sec = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Type: corev1.SecretTypeOpaque}
	}
	before := sec.DeepCopy()
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	issued := false
	if ca.NeedsRenewal(sec.Data[TLSCertKey], req, now) {
		leaf, err := ca.Issue(req, now)
		if err != nil {
			return false, err
		}
		sec.Data[TLSCertKey] = leaf.CertPEM
		sec.Data[TLSKeyKey] = leaf.KeyPEM
		issued = true
	}
	sec.Data[CACertKey] = ca.CertPEM
	if mutate != nil {
		if err := mutate(&sec); err != nil {
			return false, err
		}
	}
	if !exists {
		return issued, c.Create(ctx, &sec)
	}
	if !equality.Semantic.DeepEqual(before, &sec) {
		return issued, c.Update(ctx, &sec)
	}
	return false, nil
}
