// Package identity holds the agent's long-lived key material and the
// canonical signing inputs used for proof of possession.
//
// An agent has two keypairs, generated on first run and never sent anywhere:
//
//   - Ed25519 identity key. The cloud binds access tokens to its JWK
//     thumbprint (the token's cnf claim). The agent signs every stream Hello
//     and every refresh request with it.
//   - X25519 seal key. Browsers seal download-source credentials to its
//     public half; only the agent can open them.
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// Identity is the agent's key material.
type Identity struct {
	sign ed25519.PrivateKey
	seal *ecdh.PrivateKey
}

// Generate creates fresh keys from crypto/rand.
func Generate() (*Identity, error) {
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	seal, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate x25519 key: %w", err)
	}
	return &Identity{sign: sign, seal: seal}, nil
}

// FromSecrets rebuilds an Identity from persisted secrets: a 32-byte Ed25519
// seed and a 32-byte X25519 private key.
func FromSecrets(signSeed, sealKey []byte) (*Identity, error) {
	if len(signSeed) != ed25519.SeedSize {
		return nil, errors.New("identity: bad ed25519 seed length")
	}
	seal, err := ecdh.X25519().NewPrivateKey(sealKey)
	if err != nil {
		return nil, fmt.Errorf("identity: bad x25519 key: %w", err)
	}
	return &Identity{sign: ed25519.NewKeyFromSeed(signSeed), seal: seal}, nil
}

// SignSeed returns the Ed25519 seed for persistence. Treat as secret.
func (i *Identity) SignSeed() []byte { return i.sign.Seed() }

// SealKey returns the X25519 private key for persistence. Treat as secret.
func (i *Identity) SealKey() []byte { return i.seal.Bytes() }

// SignPublic returns the Ed25519 public key.
func (i *Identity) SignPublic() ed25519.PublicKey {
	return i.sign.Public().(ed25519.PublicKey)
}

// SealPublic returns the X25519 public key.
func (i *Identity) SealPublic() []byte { return i.seal.PublicKey().Bytes() }

// SealPrivate returns the X25519 private key for opening sealed boxes.
func (i *Identity) SealPrivate() *ecdh.PrivateKey { return i.seal }

// Sign signs msg with the identity key.
func (i *Identity) Sign(msg []byte) []byte { return ed25519.Sign(i.sign, msg) }

// Thumbprint returns the RFC 7638 JWK thumbprint of the Ed25519 public key,
// base64url without padding. This is the value the cloud puts in cnf.jkt.
func (i *Identity) Thumbprint() string { return Thumbprint(i.SignPublic()) }

// Thumbprint computes the RFC 7638 thumbprint of an Ed25519 public key.
// Required members of an OKP JWK, in lexicographic order: crv, kty, x.
func Thumbprint(pub ed25519.PublicKey) string {
	x := base64.RawURLEncoding.EncodeToString(pub)
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Verify reports whether sig is a valid signature of msg by pub.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, msg, sig)
}

// Domain-separation labels. Changing one is a protocol break.
const (
	helloLabel   = "nabbed-agent-hello-v1"
	refreshLabel = "nabbed-agent-refresh-v1"
)

// HelloSigningInput returns the bytes signed in Hello.signature.
//
// Every field is length-prefixed (see encode) so that no two distinct field
// tuples produce the same input. Plain concatenation, as sketched in design
// 0001, is ambiguous: jti "ab" + instance "c" equals jti "a" + instance "bc".
func HelloSigningInput(jti, instanceID string, signedAtMs int64) []byte {
	return encode(helloLabel, []byte(jti), []byte(instanceID), i64(signedAtMs))
}

// RefreshSigningInput returns the bytes signed on POST /api/device/refresh.
// apiHost binds the proof to the intended audience.
func RefreshSigningInput(apiHost, refreshToken string, timestampMs int64, nonce string) []byte {
	return encode(refreshLabel, []byte(apiHost), []byte(refreshToken), i64(timestampMs), []byte(nonce))
}

// encode writes label and fields as a sequence of uint32 big-endian length
// prefixes, each followed by that many bytes.
func encode(label string, fields ...[]byte) []byte {
	n := 4 + len(label)
	for _, f := range fields {
		n += 4 + len(f)
	}
	out := make([]byte, 0, n)
	out = binary.BigEndian.AppendUint32(out, uint32(len(label)))
	out = append(out, label...)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}

func i64(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }
