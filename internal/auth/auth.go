// Package auth provides the primitives for email+password accounts and
// sessions: password hashing, opaque random tokens (sessions, email
// verification, password reset) and their storage hashes, and signed
// double-submit CSRF tokens. It holds no state and does no I/O.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is deliberately above bcrypt.DefaultCost (10) for password
// storage.
const bcryptCost = 12

// HashPassword returns a bcrypt hash of the plaintext password.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword reports whether plain matches the stored bcrypt hash. It is
// constant-time (bcrypt) and false for an empty hash (Google-only accounts).
func CheckPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// RandomToken returns a 32-byte cryptographically-random token, base64url
// (unpadded) encoded. Used for session cookies and single-use email links; the
// plaintext is given to the user, only its HashToken is stored.
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the at-rest form of a token: its hex SHA-256. Storing this
// (rather than the token itself) means a database read cannot reconstruct a
// live session cookie or a usable email link.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// --- CSRF: signed double-submit token ---

// NewCSRFToken mints a CSRF token: a random value plus an HMAC over it, keyed
// by the server secret. Placed in a non-HttpOnly cookie and echoed by the
// client in a header; ValidCSRFToken verifies the signature so an attacker who
// can only plant a cookie (e.g. from a sibling subdomain) cannot forge a pair.
func NewCSRFToken(secret []byte) (string, error) {
	r, err := RandomToken()
	if err != nil {
		return "", err
	}
	return r + "." + sign(secret, r), nil
}

// ValidCSRFToken reports whether token carries a valid signature for secret.
func ValidCSRFToken(secret []byte, token string) bool {
	r, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(sign(secret, r)))
}

// EqualToken is a constant-time string comparison, for matching the CSRF
// header against the cookie.
func EqualToken(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func sign(secret []byte, msg string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
