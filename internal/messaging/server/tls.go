package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// TLSConfig returns a server config that requires and verifies client
// certificates against the CA bundle at caFile. The certificate, key and CA
// are re-read at most once a minute, so rotated Secrets take effect without
// a restart.
func TLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	r := &reloader{certFile: certFile, keyFile: keyFile, caFile: caFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool := r.get()
			return &tls.Config{
				MinVersion:   tls.VersionTLS12,
				NextProtos:   []string{"h2", "http/1.1"},
				Certificates: []tls.Certificate{*cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    pool,
			}, nil
		},
	}, nil
}

type reloader struct {
	certFile, keyFile, caFile string

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
