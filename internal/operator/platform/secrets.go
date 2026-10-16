package platform

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"fmt"

	webpush "github.com/SherClockHolmes/webpush-go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// randomBytes returns n random bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// NewSetupToken is 32 random bytes in unpadded base32 (design §3.3).
func NewSetupToken() string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes(32))
}

// ensureSecret creates Secret key with the data gen returns, if it doesn't
// exist. Existing Secrets keep their values, except keys gen adds that are
// missing (so a new key can be added in an upgrade). owner may be nil.
func (r *Reconciler) ensureSecret(ctx context.Context, owner client.Object, key types.NamespacedName, component string, gen func() (map[string][]byte, error)) (*corev1.Secret, error) {
	var sec corev1.Secret
	err := r.Get(ctx, key, &sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if apierrors.IsNotFound(err) {
		data, err := gen()
		if err != nil {
			return nil, err
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{
				v1alpha1.LabelManaged: "true", v1alpha1.LabelComponent: component,
			}},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		if owner != nil {
			if err := controllerutil.SetControllerReference(owner, &sec, r.Scheme); err != nil {
				return nil, err
			}
		}
		if err := r.Create(ctx, &sec); err != nil {
			return nil, fmt.Errorf("create secret %s: %w", key, err)
		}
		return &sec, nil
	}
	data, err := gen()
	if err != nil {
		return nil, err
	}
	changed := false
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range data {
		if _, ok := sec.Data[k]; !ok {
			sec.Data[k] = v
			changed = true
		}
	}
	if changed {
		if err := r.Update(ctx, &sec); err != nil {
			return nil, err
		}
	}
	return &sec, nil
}

func staticData(kv map[string]string) func() (map[string][]byte, error) {
	return func() (map[string][]byte, error) {
		out := map[string][]byte{}
		for k, v := range kv {
			out[k] = []byte(v)
		}
		return out, nil
	}
}

func vapidKeys() (map[string][]byte, error) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"privateKey": []byte(priv), "publicKey": []byte(pub)}, nil
}

// pgDatabase is one service's database.
type pgDatabase struct {
	// secret in the platform namespace, postgres-<service>
	secret string
	// role and database name
	name string
}

var pgDatabases = []pgDatabase{
	{secret: "postgres-forgejo", name: "forgejo"},
	{secret: "postgres-gateway", name: "litellm"},
	{secret: "postgres-control-center", name: "controlcenter"},
}

func pgURL(ns, user, password, db string) string {
	return fmt.Sprintf("postgres://%s:%s@postgres.%s.svc:5432/%s?sslmode=disable", user, password, ns, db)
}

// ensureDatabaseSecrets makes postgres-superuser, postgres-<service> and the
// control center's control-center-db.
func (r *Reconciler) ensureDatabaseSecrets(ctx context.Context, p *v1alpha1.Platform) error {
	ns := v1alpha1.PlatformNamespace
	if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: ns, Name: "postgres-superuser"}, "postgres",
		staticData(map[string]string{"username": "postgres", "password": randomHex(24)})); err != nil {
		return err
	}
	for _, d := range pgDatabases {
		pw := randomHex(24)
		sec, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: ns, Name: d.secret}, "postgres",
			staticData(map[string]string{"username": d.name, "password": pw, "database": d.name, "url": pgURL(ns, d.name, pw, d.name)}))
		if err != nil {
			return err
		}
		if d.name == "controlcenter" {
			url := string(sec.Data["url"])
			if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "control-center-db"}, "control-center",
				staticData(map[string]string{"url": url})); err != nil {
				return err
			}
		}
	}
	return nil
}
