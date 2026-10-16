package room

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Keys of the identity Secret (docs/conventions.md).
const (
	KeySSHPrivate     = "id_ed25519"
	KeySSHPublic      = "id_ed25519.pub"
	KeyGPGSecret      = "gpg_secret.asc"
	KeyForgeToken     = "forge_token"
	KeyTLSCert        = "tls.crt"
	KeyTLSKey         = "tls.key"
	KeyCA             = "ca.crt"
	KeyGatewayKey     = "gateway_key"
	KeyVNCPassword    = "vnc_password"
	KeyForgeKnownHost = "forge_known_hosts"
)

// newSSHKey returns an OpenSSH private key and its authorized_keys line.
func newSSHKey(comment string) (priv, pub []byte, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return nil, nil, err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return nil, nil, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment + "\n"
	return pem.EncodeToMemory(block), []byte(line), nil
}

// randomString returns n random bytes, base32 without padding.
func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// certNeedsRenewal reports whether a PEM certificate is missing, unparsable
// or past 2/3 of its lifetime (§7.5).
func certNeedsRenewal(certPEM []byte, now time.Time) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return true
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return true
	}
	life := c.NotAfter.Sub(c.NotBefore)
	return now.After(c.NotBefore.Add(life * 2 / 3))
}

// HostKeyScanner returns known_hosts lines for an SSH server.
type HostKeyScanner func(ctx context.Context, host string, port int) ([]byte, error)

var errGotKey = errors.New("got host key")

// ScanHostKey connects to host:port, records the server's host key and hangs up.
func ScanHostKey(ctx context.Context, host string, port int) ([]byte, error) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	var line string
	cfg := &ssh.ClientConfig{
		User:    "git",
		Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			name := host
			if port != 22 {
				name = addr
			}
			line = knownhosts.Line([]string{name}, key)
			return errGotKey
		},
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if line == "" {
		return nil, fmt.Errorf("no host key from %s: %w", addr, err)
	}
	return []byte(line + "\n"), nil
}
