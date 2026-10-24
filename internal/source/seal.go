package source

import (
	"bytes"
	"crypto/ecdh"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// Open opens a libsodium sealed box (crypto_box_seal: an ephemeral X25519
// key, XSalsa20-Poly1305, nonce = BLAKE2b(ephemeral_pk || recipient_pk)),
// which is what the web app produces. sealedTo, when set, must be the
// agent's X25519 public key, so a box sealed to a previous pairing gets a
// clear error instead of a generic failure.
func Open(key *ecdh.PrivateKey, sealed, sealedTo []byte) ([]byte, error) {
	pub := key.PublicKey().Bytes()

	if len(sealedTo) > 0 && !bytes.Equal(sealedTo, pub) {
		return nil, fmt.Errorf("%w: it was sealed to a different agent key; enter the source again in the web app", ErrUnsealable)
	}

	if len(sealed) < box.AnonymousOverhead {
		return nil, fmt.Errorf("%w: too short", ErrUnsealable)
	}

	var pk, sk [32]byte
	copy(pk[:], pub)
	copy(sk[:], key.Bytes())

	defer clear(sk[:])

	plain, ok := box.OpenAnonymous(nil, sealed, &pk, &sk)
	if !ok {
		return nil, fmt.Errorf("%w: authentication failed", ErrUnsealable)
	}

	return plain, nil
}

// Seal seals plain to pub the way the web app does. The agent never seals;
// it exists for tests and tooling.
func Seal(pub *ecdh.PublicKey, plain []byte) ([]byte, error) {
	var pk [32]byte
	copy(pk[:], pub.Bytes())

	return box.SealAnonymous(nil, plain, &pk, nil)
}
