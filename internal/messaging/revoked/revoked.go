// Package revoked is the messaging service's certificate deny list. The
// operator writes the serial numbers of removed rooms' (and rotated)
// client certificates into a ConfigMap in the platform namespace; messaging
// mounts it and refuses those certificates until they expire.
package revoked

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ConfigMapName is the deny list in the platform namespace. Each key is a
// serial number (Key), each value "<notAfter RFC 3339> <subject CN>".
const ConfigMapName = "messaging-revoked"

// Key is a certificate's key in the deny list: its serial in lowercase hex.
func Key(serial *big.Int) string { return strings.ToLower(serial.Text(16)) }

// Entry returns the deny-list key and value of a PEM certificate.
func Entry(certPEM []byte) (key, value string, err error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return "", "", errors.New("no PEM certificate")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "", "", err
	}
	return Key(c.SerialNumber), c.NotAfter.UTC().Format(time.RFC3339) + " " + c.Subject.CommonName, nil
}

// Expiry parses an entry's value; ok is false if it is malformed.
func Expiry(value string) (time.Time, bool) {
	ts, _, _ := strings.Cut(value, " ")
	t, err := time.Parse(time.RFC3339, ts)
	return t, err == nil
}

// Expired returns the keys of data whose certificates have expired, which
// no longer need listing (malformed entries too).
func Expired(data map[string]string, now time.Time) []string {
	var out []string
	for k, v := range data {
		if t, ok := Expiry(v); !ok || now.After(t) {
			out = append(out, k)
		}
	}
	return out
}

// List is a loaded deny list.
type List struct {
	mu    sync.RWMutex
	until map[string]time.Time
}

// Set replaces the list's entries (key -> value).
func (l *List) Set(data map[string]string) {
	m := make(map[string]time.Time, len(data))
	for k, v := range data {
		if t, ok := Expiry(v); ok {
			m[strings.ToLower(k)] = t
		}
	}
	l.mu.Lock()
	l.until = m
	l.mu.Unlock()
}

// Revoked reports whether c is on the list and not yet expired.
func (l *List) Revoked(c *x509.Certificate, now time.Time) bool {
	if l == nil || c == nil {
		return false
	}
	l.mu.RLock()
	t, ok := l.until[Key(c.SerialNumber)]
	l.mu.RUnlock()
	return ok && !now.After(t)
}

// Len is the number of entries.
func (l *List) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.until)
}

// LoadDir reads the ConfigMap as kubelet mounts it: one file per key. A
// missing directory is an empty list.
func LoadDir(dir string) (map[string]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue // kubelet's ..data and timestamped directories
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out[e.Name()] = strings.TrimSpace(string(b))
	}
	return out, nil
}
