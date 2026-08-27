package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := NewBox(testKey(t))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	sealed, err := box.Seal("super-secret-api-key")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == "super-secret-api-key" {
		t.Fatal("Seal returned plaintext unchanged")
	}
	plain, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if plain != "super-secret-api-key" {
		t.Errorf("Open = %q, want original plaintext", plain)
	}
}

func TestSealDistinctNoncePerCall(t *testing.T) {
	box, err := NewBox(testKey(t))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	a, err := box.Seal("same-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := box.Seal("same-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if a == b {
		t.Error("two Seal calls on the same plaintext produced identical ciphertext -- nonce reuse")
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	sealBox, err := NewBox(testKey(t))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	sealed, err := sealBox.Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	openBox, err := NewBox(testKey(t))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	if _, err := openBox.Open(sealed); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Open with wrong key = %v, want ErrDecryptFailed", err)
	}
}

func TestOpenCorruptedValueFails(t *testing.T) {
	box, err := NewBox(testKey(t))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	if _, err := box.Open("not-a-valid-sealed-value"); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Open garbage = %v, want ErrDecryptFailed", err)
	}
	if _, err := box.Open("v1:not-base64!!!"); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Open invalid base64 = %v, want ErrDecryptFailed", err)
	}
}

func TestNewBoxEmptyKeyReturnsNilBox(t *testing.T) {
	box, err := NewBox("")
	if err != nil {
		t.Fatalf("NewBox(\"\") returned error %v, want nil error", err)
	}
	if box != nil {
		t.Fatal("NewBox(\"\") returned non-nil box, want nil (not configured)")
	}
}

func TestNewBoxInvalidKeyErrors(t *testing.T) {
	cases := []string{
		"not-base64!!!",
		base64.StdEncoding.EncodeToString([]byte("too-short")),
	}
	for _, k := range cases {
		if _, err := NewBox(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("NewBox(%q) = %v, want ErrInvalidKey", k, err)
		}
	}
}

func TestNilBoxDegradesGracefully(t *testing.T) {
	var box *Box
	if _, err := box.Seal("x"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil Box.Seal = %v, want ErrUnavailable", err)
	}
	if _, err := box.Open("v1:whatever"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil Box.Open = %v, want ErrUnavailable", err)
	}
}
