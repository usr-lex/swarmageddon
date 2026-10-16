// Package roomtest has test doubles for the room and runnerpool controllers:
// a Kubernetes client (envtest when KUBEBUILDER_ASSETS is set, else the
// controller-runtime fake client), a CA, image names, and recording fakes of
// Forgejo and LiteLLM.
package roomtest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
)

// Scheme has the core types and ours.
func Scheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	return s
}

// Client returns a client to a real API server (envtest) when
// KUBEBUILDER_ASSETS is set, else a fake client. real tells which.
func Client(t *testing.T) (c client.Client, real bool) {
	t.Helper()
	s := Scheme()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return fake.NewClientBuilder().WithScheme(s).
			WithStatusSubresource(&v1.Room{}, &v1.RunnerPool{}).Build(), false
	}
	_, file, _, _ := runtime.Caller(0)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		Scheme:                s,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	return c, true
}

// PKI is a throwaway CA.
type PKI struct {
	mu     sync.Mutex
	Issued []string // CNs
	key    *ecdsa.PrivateKey
	cert   *x509.Certificate
	der    []byte
}

// NewPKI makes a CA.
func NewPKI() *PKI {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	c, _ := x509.ParseCertificate(der)
	return &PKI{key: k, cert: c, der: der}
}

// IssueClientCert issues a client certificate.
func (p *PKI) IssueClientCert(cn, uriSAN string, ttl time.Duration) ([]byte, []byte, error) {
	p.mu.Lock()
	p.Issued = append(p.Issued, cn)
	n := len(p.Issued)
	p.mu.Unlock()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(uriSAN)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(n + 1)), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(ttl), URIs: []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &k.PublicKey, p.key)
	if err != nil {
		return nil, nil, err
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), nil
}

// CABundle returns the CA certificate.
func (p *PKI) CABundle() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.der})
}

// Images are fixed image names.
type Images struct{ RoomImage, RunnerImage string }

func (i *Images) Room() string   { return i.RoomImage }
func (i *Images) Runner() string { return i.RunnerImage }

// Call is one recorded HTTP request.
type Call struct {
	Method, Path, Sudo string
	Body               map[string]any
}

// Recorder records calls to a fake server.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
	n     int
}

func (r *Recorder) record(req *http.Request, prefix string) (Call, int) {
	c := Call{Method: req.Method, Path: strings.TrimPrefix(req.URL.RequestURI(), prefix), Sudo: req.Header.Get("Sudo")}
	if b, _ := io.ReadAll(req.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &c.Body)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
	r.n++
	return c, r.n
}

// Calls returns the calls matching method and a path prefix.
func (r *Recorder) Calls(method, pathPrefix string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		if c.Method == method && strings.HasPrefix(c.Path, pathPrefix) {
			out = append(out, c)
		}
	}
	return out
}

// Forge is a fake Forgejo good enough for provisioning rooms and runners.
type Forge struct {
	Recorder
	Teams []forgejo.Team
	// Jobs answers GET .../actions/runners/jobs.
	Jobs []forgejo.RunJob
	URL  string
}

// NewForge starts a fake Forgejo with the given teams in every org.
func NewForge(t *testing.T, teams ...string) *Forge {
	f := &Forge{}
	for i, n := range teams {
		f.Teams = append(f.Teams, forgejo.Team{ID: int64(i + 1), Name: n})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, n := f.record(req, "/api/v1")
		p, _, _ := strings.Cut(c.Path, "?")
		w.Header().Set("Content-Type", "application/json")
		write := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }
		switch {
		case c.Method == "GET" && strings.HasPrefix(p, "/users/") && strings.Count(p, "/") == 2:
			write(404, map[string]string{"message": "not found"})
		case c.Method == "GET" && strings.HasSuffix(p, "/teams"):
			write(200, f.Teams)
		case c.Method == "GET" && strings.HasSuffix(p, "/actions/runners/jobs"):
			f.mu.Lock()
			jobs := f.Jobs
			f.mu.Unlock()
			write(200, jobs)
		case c.Method == "GET" && (strings.HasSuffix(p, "/keys") || strings.HasSuffix(p, "/gpg_keys") || strings.HasSuffix(p, "/tokens")):
			write(200, []any{})
		case c.Method == "GET" && p == "/user/gpg_key_token":
			_, _ = w.Write([]byte("token-to-sign"))
		case c.Method == "POST" && strings.HasSuffix(p, "/tokens"):
			write(201, forgejo.AccessToken{ID: int64(n), Name: fmt.Sprint(c.Body["name"]), SHA1: fmt.Sprintf("forgetoken-%d", n)})
		case c.Method == "POST" && strings.HasSuffix(p, "/actions/runners"):
			write(201, forgejo.RegisteredRunner{ID: int64(n), UUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", n), Token: fmt.Sprintf("runnertoken-%d", n)})
		case c.Method == "POST":
			write(201, map[string]any{})
		case c.Method == "DELETE":
			w.WriteHeader(204)
		default:
			write(200, map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// SetJobs sets the waiting jobs.
func (f *Forge) SetJobs(j []forgejo.RunJob) {
	f.mu.Lock()
	f.Jobs = j
	f.mu.Unlock()
}

// Client returns a client for the fake.
func (f *Forge) Client(context.Context) (*forgejo.Client, error) { return forgejo.New(f.URL, "admintoken"), nil }

// Gateway is a fake LiteLLM.
type Gateway struct {
	Recorder
	URL string
}

// NewGateway starts a fake LiteLLM.
func NewGateway(t *testing.T) *Gateway {
	g := &Gateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, n := g.record(req, "")
		w.Header().Set("Content-Type", "application/json")
		if c.Path == "/key/generate" {
			_ = json.NewEncoder(w).Encode(map[string]any{"key": fmt.Sprintf("sk-room-%d", n), "key_alias": c.Body["key_alias"]})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	t.Cleanup(srv.Close)
	g.URL = srv.URL
	return g
}

// Client returns a client for the fake.
func (g *Gateway) Client(context.Context) (*gateway.Client, error) { return gateway.New(g.URL, "sk-master"), nil }
