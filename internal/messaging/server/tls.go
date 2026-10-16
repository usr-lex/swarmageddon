package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/internal/messaging/revoked"
)

// TLSConfig returns a server config that requires and verifies client
// certificates against the CA bundle at caFile. The certificate, key and CA
// are re-read at most once a minute, so rotated Secrets take effect without
// a restart.
func TLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	return TLSConfigWithDenyList(certFile, keyFile, caFile, "")
}

// TLSConfigWithDenyList is TLSConfig that also refuses client certificates
// on the deny list mounted at revokedDir (the messaging-revoked ConfigMap,
// one file per serial), until they expire. It is re-read with the rest.
func TLSConfigWithDenyList(certFile, keyFile, caFile, revokedDir string) (*tls.Config, error) {
	r := &reloader{certFile: certFile, keyFile: keyFile, caFile: caFile, revokedDir: revokedDir, denied: &revoked.List{}}
	if err := r.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool := r.get()
			return &tls.Config{
				MinVersion:       tls.VersionTLS12,
				NextProtos:       []string{"h2", "http/1.1"},
				Certificates:     []tls.Certificate{*cert},
				ClientAuth:       tls.RequireAndVerifyClientCert,
				ClientCAs:        pool,
				VerifyConnection: r.verify,
			}, nil
		},
	}, nil
}

// ErrRevoked is returned for a client certificate on the deny list.
var ErrRevoked = errors.New("client certificate revoked")

type reloader struct {
	certFile, keyFile, caFile, revokedDir string
	denied                                *revoked.List

	mu     sync.Mutex
	cert   *tls.Certificate
	pool   *x509.CertPool
	loaded time.Time
}

func (r *reloader) load() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("serving certificate: %w", err)
	}
	ca, err := os.ReadFile(r.caFile)
	if err != nil {
		return fmt.Errorf("CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("CA bundle: no certificates in " + r.caFile)
	}
	if r.revokedDir != "" {
		data, err := revoked.LoadDir(r.revokedDir)
		if err != nil {
			return fmt.Errorf("deny list: %w", err)
		}
		r.denied.Set(data)
	}
	r.mu.Lock()
	r.cert, r.pool, r.loaded = &cert, pool, time.Now()
	r.mu.Unlock()
	return nil
}

func (r *reloader) get() (*tls.Certificate, *x509.CertPool) {
	r.mu.Lock()
	stale := time.Since(r.loaded) > time.Minute
	r.mu.Unlock()
	if stale {
		_ = r.load() // keep the old pair if the new one is unreadable
		r.mu.Lock()
		r.loaded = time.Now()
		r.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cert, r.pool
}

// verify refuses a verified client certificate that is on the deny list.
func (r *reloader) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) > 0 && r.denied.Revoked(cs.PeerCertificates[0], time.Now()) {
		return fmt.Errorf("%w: %s", ErrRevoked, cs.PeerCertificates[0].Subject.CommonName)
	}
	return nil
}
