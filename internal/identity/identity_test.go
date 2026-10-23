package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func TestRoundTripSecrets(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := FromSecrets(id.SignSeed(), id.SealKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(id.SignPublic(), id2.SignPublic()) || !bytes.Equal(id.SealPublic(), id2.SealPublic()) {
		t.Fatal("public keys differ after round trip")
	}
	msg := []byte("hello")
	if !Verify(id2.SignPublic(), msg, id.Sign(msg)) {
		t.Fatal("signature from original does not verify with restored key")
	}
}

func TestFromSecretsRejectsBadLengths(t *testing.T) {
	if _, err := FromSecrets(make([]byte, 31), make([]byte, 32)); err == nil {
		t.Error("expected error for short seed")
	}
	if _, err := FromSecrets(make([]byte, 32), make([]byte, 31)); err == nil {
		t.Error("expected error for short x25519 key")
	}
}

// RFC 8037 appendix A.3: thumbprint of the example Ed25519 key.
func TestThumbprintRFC8037(t *testing.T) {
	pub, _ := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	const want = "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"
	if got := Thumbprint(ed25519.PublicKey(pub)); got != want {
		t.Fatalf("Thumbprint = %s, want %s", got, want)
	}
}

func TestSigningInputsAreUnambiguous(t *testing.T) {
	a := HelloSigningInput("ab", "c", 1)
	b := HelloSigningInput("a", "bc", 1)
	if bytes.Equal(a, b) {
		t.Fatal("hello signing input is ambiguous across field boundaries")
	}
	if bytes.Equal(HelloSigningInput("j", "i", 1), RefreshSigningInput("j", "i", 1, "")) {
		t.Fatal("hello and refresh inputs collide")
	}
}

func TestHelloSigningInputLayout(t *testing.T) {
	got := HelloSigningInput("j", "i", 258)
	want := []byte{
		0, 0, 0, 21, 'n', 'a', 'b', 'b', 'e', 'd', '-', 'a', 'g', 'e', 'n', 't', '-', 'h', 'e', 'l', 'l', 'o', '-', 'v', '1',
		0, 0, 0, 1, 'j',
		0, 0, 0, 1, 'i',
		0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 1, 2,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("layout mismatch:\n got %v\nwant %v", got, want)
	}
}
