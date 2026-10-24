package source

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

// A box made by libsodium 1.0.20 (libsodium-wrappers, crypto_box_seal), as
// the web app makes them. The agent must open it with its X25519 key.
const (
	vectorKey       = "4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	vectorPublic    = "5590e18b13045370235f1719ab8448c3c0a39a269bfc86ded297b1fa1bf4ac30"
	vectorSealed    = "kgwYqpZHE7lPpSe7ZEoaS7BE3tONx2T6XP7hG8w7+mkerMB5p7ZDb1rlIjhIix0cfYl3mfyxE//R8k2FzD6o/Y1z5BTcZRlA7Yk32vxMPbXkUM/8MpDLHZxAL6AjckRJZpUZTNsih8ELntgOS/yGipzJw+Koml4Npd7FJJvpwuJfeAb/voB4Db4BDrdqNYRsjqLJc/VNR46Z/zw0PyKtWX056cz5D46ElZruKEaE"
	vectorPlaintext = `{"v":1,"host":"news.newshosting.com","port":563,"tls":true,"username":"vector-user","password":"vector-pass","connections":30}`
)

func TestOpensLibsodiumSealedBox(t *testing.T) {
	raw, _ := hex.DecodeString(vectorKey)

	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}

	if got := hex.EncodeToString(key.PublicKey().Bytes()); got != vectorPublic {
		t.Fatalf("got public key %s, want libsodium's %s", got, vectorPublic)
	}

	sealed, _ := base64.StdEncoding.DecodeString(vectorSealed)

	plain, err := Open(key, sealed, key.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}

	if string(plain) != vectorPlaintext {
		t.Fatalf("got plaintext %q, want %q", plain, vectorPlaintext)
	}

	p, err := parseNNTP(plain)
	if err != nil || p.Host != "news.newshosting.com" || p.Username != "vector-user" || p.Connections != 30 {
		t.Fatalf("got provider %+v, err %v", p, err)
	}

	sealed[40] ^= 1
	if _, err := Open(key, sealed, nil); !errors.Is(err, ErrUnsealable) {
		t.Fatalf("got err = %v for a tampered box, want ErrUnsealable", err)
	}
}
