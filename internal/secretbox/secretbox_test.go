package secretbox

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}

	secret := []byte("ya29.a0-super-secret-refresh-token")
	ct, err := box.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, secret) {
		t.Fatal("ciphertext must not contain the plaintext")
	}
	// Two seals of the same plaintext differ (random nonce).
	ct2, _ := box.Seal(secret)
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonce reuse: two seals produced identical ciphertext")
	}

	got, err := box.Open(ct)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("open: got %q err=%v", got, err)
	}
}

func TestOpenRejectsTamperedOrWrongKey(t *testing.T) {
	k1, _ := GenerateKey()
	box, _ := New(k1)
	ct, _ := box.Seal([]byte("secret"))

	// Tampering breaks the GCM tag.
	ct[len(ct)-1] ^= 0xff
	if _, err := box.Open(ct); err == nil {
		t.Fatal("tampered ciphertext must fail to open")
	}

	// A different key can't open it.
	k2, _ := GenerateKey()
	other, _ := New(k2)
	ct2, _ := box.Seal([]byte("secret"))
	if _, err := other.Open(ct2); err == nil {
		t.Fatal("wrong key must fail to open")
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New("not-base64!!!"); err == nil {
		t.Fatal("non-base64 key must error")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := New(short); err == nil {
		t.Fatal("wrong-length key must error")
	}
}
