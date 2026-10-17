// Package tokenhash is the set of secret tokens the operator issued for a
// workspace's rooms, kept as SHA-256 digests, which the messaging service
// uses to refuse messages that carry one (design §11.4).
//
// The operator writes the set to ConfigMap messaging-token-hashes in the
// workspace namespace, data key sha256: the lowercase hex digests, sorted,
// one per line. The tokens themselves are never written.
package tokenhash

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

const (
	// ConfigMapName is the ConfigMap in each workspace namespace.
	ConfigMapName = "messaging-token-hashes"
	// DataKey holds the digests.
	DataKey = "sha256"
	// MinLen and MaxLen bound the candidates Contains hashes. Issued tokens
	// are 16 to 256 characters of [A-Za-z0-9_-] (forge tokens, gateway keys,
	// egress credentials).
	MinLen = 16
	MaxLen = 256
)

// Set is a set of token digests.
type Set map[[32]byte]struct{}

// Format returns the ConfigMap value for tokens: their digests, sorted and
// without duplicates, one per line.
func Format(tokens []string) string {
	var lines []string
	for _, t := range tokens {
		if t == "" {
			continue
		}
		h := sha256.Sum256([]byte(t))
		lines = append(lines, hex.EncodeToString(h[:]))
	}
	slices.Sort(lines)
	return strings.Join(slices.Compact(lines), "\n")
}

// Parse reads a ConfigMap value, skipping lines that aren't digests.
func Parse(s string) Set {
	set := Set{}
	for _, line := range strings.Split(s, "\n") {
		b, err := hex.DecodeString(strings.TrimSpace(line))
		if err != nil || len(b) != sha256.Size {
			continue
		}
		set[[32]byte(b)] = struct{}{}
	}
	return set
}

func tokenByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

// Contains reports whether body holds one of the tokens: a run of token
// characters, delimited by anything else (whitespace, punctuation), of
// plausible length, taken as is or without leading and trailing '-' and
// '_'. Only those runs are hashed, so a check costs at most one or two
// digests per word of the body.
func (s Set) Contains(body string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(body); {
		if !tokenByte(body[i]) {
			i++
			continue
		}
		j := i
		for j < len(body) && tokenByte(body[j]) {
			j++
		}
		run := body[i:j]
		if s.has(run) {
			return true
		}
		if t := strings.Trim(run, "-_"); t != run && s.has(t) {
			return true
		}
		i = j
	}
	return false
}

func (s Set) has(t string) bool {
	if len(t) < MinLen || len(t) > MaxLen {
		return false
	}
	_, ok := s[sha256.Sum256([]byte(t))]
	return ok
}
