package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/pki"
)

// On a fresh install the operator writes Secret control-center-tls after the
// pod starts: the control center waits for the files instead of exiting
// (OpenShift report 5.7, one crash-restart per install).
func TestWaitForServingCert(t *testing.T) {
	dir := t.TempDir()
	cr := &certReloader{cert: filepath.Join(dir, "tls.crt"), key: filepath.Join(dir, "tls.key")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitForCert(ctx, log, cr, 10*time.Millisecond); err == nil {
		t.Fatal("no files: want the context's error")
	}

	ca, err := pki.NewCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(pki.Request{CommonName: "cc", DNSNames: []string{"cc"}, Usage: pki.Serving}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(cr.key, leaf.KeyPEM, 0o600)
		_ = os.WriteFile(cr.cert, leaf.CertPEM, 0o600)
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := waitForCert(ctx2, log, cr, 10*time.Millisecond); err != nil {
		t.Fatalf("files written later: %v", err)
	}
}
