package auth

import "testing"

func TestPasswordHashing(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("password must not be stored in plaintext")
	}
	if !CheckPassword(hash, "correct horse battery staple") {
		t.Fatal("correct password must verify")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("wrong password must not verify")
	}
	if CheckPassword("", "anything") {
		t.Fatal("empty hash (google-only account) must never verify")
	}
}

func TestRandomTokenUnique(t *testing.T) {
	a, err := RandomToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RandomToken()
	if a == b || a == "" {
		t.Fatalf("tokens must be random and non-empty: %q %q", a, b)
	}
	if HashToken(a) == a {
		t.Fatal("HashToken must not return the token itself")
	}
	if HashToken(a) != HashToken(a) {
		t.Fatal("HashToken must be deterministic")
	}
}

func TestCSRFToken(t *testing.T) {
	secret := []byte("test-secret")
	tok, err := NewCSRFToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidCSRFToken(secret, tok) {
		t.Fatal("freshly minted token must validate")
	}
	if ValidCSRFToken([]byte("other-secret"), tok) {
		t.Fatal("token must not validate under a different secret")
	}
	if ValidCSRFToken(secret, tok+"x") {
		t.Fatal("tampered token must not validate")
	}
	if ValidCSRFToken(secret, "no-dot-separator") {
		t.Fatal("malformed token must not validate")
	}
	if !EqualToken(tok, tok) || EqualToken(tok, "different") {
		t.Fatal("EqualToken comparison wrong")
	}
}
