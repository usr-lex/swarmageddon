package forgejo

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// GPGKeyPair is the GPG key of an agent: an EdDSA (ed25519) primary key with no
// expiry, armored.
type GPGKeyPair struct {
	ArmoredPrivate string
	ArmoredPublic  string
	// KeyID is the 16-hex-digit long key ID, as Forgejo reports it.
	KeyID string
}

// NewGPGKey generates a signing key for name <email>.
func NewGPGKey(name, email string) (*GPGKeyPair, error) {
	e, err := openpgp.NewEntity(name, "", email, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		return nil, err
	}
	return armorEntity(e)
}

func armorEntity(e *openpgp.Entity) (*GPGKeyPair, error) {
	var priv, pub bytes.Buffer
	w, err := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		return nil, err
	}
	w.Close()
	w, err = armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := e.Serialize(w); err != nil {
		return nil, err
	}
	w.Close()
	return &GPGKeyPair{ArmoredPrivate: priv.String(), ArmoredPublic: pub.String(), KeyID: e.PrimaryKey.KeyIdString()}, nil
}

// ParseGPGKey reads an armored private key back.
func ParseGPGKey(armoredPrivate string) (*GPGKeyPair, *openpgp.Entity, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPrivate))
	if err != nil {
		return nil, nil, err
	}
	if len(el) != 1 || el[0].PrivateKey == nil {
		return nil, nil, fmt.Errorf("gpg: want one private key, got %d entities", len(el))
	}
	kp, err := armorEntity(el[0])
	return kp, el[0], err
}

// SignToken returns an armored detached signature of token, as Forgejo's GPG
// key verification expects.
func SignToken(armoredPrivate, token string) (string, error) {
	_, e, err := ParseGPGKey(armoredPrivate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, e, strings.NewReader(token), nil); err != nil {
		return "", err
	}
	return buf.String(), nil
}
