package source

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/getnabbed/agent/internal/netguard"
)

// A box made by libsodium 1.0.20 (libsodium-wrappers, crypto_box_seal), as
// the web app makes them. The agent must open it with its X25519 key.
const (
	vectorKey       = "4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	vectorPublic    = "5590e18b13045370235f1719ab8448c3c0a39a269bfc86ded297b1fa1bf4ac30"
	vectorSealed    = "SfAUr+XZ1fE66NrgLVDZ3AEMKBnJQ0yMKO/On2zDe0gN7llSLXN4GS2D6c2Q8l5BffeCejMBcvOPJVvVbNm9myowRoWv4U1bbw/Yowdf/z68KVygqwhzR1Sii69QcfXxLzNTj6QDIqZ8sKNb6y699HCEoIIElD3aEhI9gl04MqUSexPYpyXaXq+1VBO4uerUeTZQksei5SRDIdrq2eGmTQ6KZaVkUQnh/Ktrw31wGL4podYpBi37s1oy8nNpyB49DFv43VMuKz7oLKM/QpWdAmYm2MYlz6M="
	vectorPlaintext = `{"schema_version":1,"host":"news.newshosting.com","port":563,"tls":true,"insecure_skip_verify":false,"credentials":{"username":"vector-user","password":"vector-pass"}}`
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

	p, err := parseUsenet(plain, netguard.Policy{})
	if err != nil || p.Host != "news.newshosting.com" || p.Username != "vector-user" || p.Password != "vector-pass" || p.Connections != 8 {
		t.Fatalf("got provider %+v, err %v", p, err)
	}

	sealed[40] ^= 1
	if _, err := Open(key, sealed, nil); !errors.Is(err, ErrUnsealable) {
		t.Fatalf("got err = %v for a tampered box, want ErrUnsealable", err)
	}
}
