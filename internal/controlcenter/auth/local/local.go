// Package local hashes and checks local account passwords with argon2id,
// stored as PHC strings: $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>.
package local

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Parameters for new hashes (RFC 9106's second recommended option, with more passes).
const (
	memoryKiB = 64 * 1024
	passes    = 3
	threads   = 2
	saltLen   = 16
	keyLen    = 32
	// MinPasswordLen is the shortest password accepted for a new account.
	MinPasswordLen = 12
)

var b64 = base64.RawStdEncoding

// ErrMismatch is returned when the password does not match.
var ErrMismatch = errors.New("password does not match")

// Hash returns the PHC string for password.
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, passes, memoryKiB, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memoryKiB, passes, threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify checks password against a PHC string in constant time.
func Verify(phc, password string) error {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("not an argon2id PHC string")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return errors.New("unsupported argon2 version")
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return fmt.Errorf("bad argon2 parameters: %w", err)
	}
	if m == 0 || m > 1<<22 || t == 0 || t > 32 || p == 0 {
		return errors.New("argon2 parameters out of range")
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("bad salt: %w", err)
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 {
		return errors.New("bad hash")
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// CheckNew validates a password for a new account.
func CheckNew(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("the password must have at least %d characters", MinPasswordLen)
	}
	if len(password) > 1024 {
		return errors.New("the password is too long")
	}
	return nil
}
