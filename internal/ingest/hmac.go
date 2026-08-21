package ingest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
)

// Signature scheme for the email ingestion webhook.
//
// The endpoint has no session and no CSRF token: the caller is a mail worker,
// not a browser. It authenticates by signing the request body with a secret
// shared with this server.
const (
	// SignatureHeader carries the scheme-prefixed signature.
	SignatureHeader = "X-Ziga-Signature"
	// TimestampHeader carries Unix seconds, and bounds replay.
	TimestampHeader = "X-Ziga-Timestamp"
	// SignatureScheme prefixes the signature so a future key or algorithm
	// rotation can ship alongside this one instead of needing a flag day.
	SignatureScheme = "v1"
	// MaxSkew is how far a request timestamp may sit from our clock in either
	// direction. Five minutes tolerates drift between the mail worker and this
	// host while keeping the replay window small. A replay inside the window
	// is harmless anyway: ingestion is idempotent on Message-ID and content
	// hash, so it produces no second lead.
	MaxSkew = 5 * time.Minute
)

// SigningString is the exact string that gets signed.
//
// The body is hashed rather than concatenated so the signed string is a fixed
// length regardless of payload size, and so the fixed-width hex digest leaves
// no way to shift bytes between the timestamp and the body and still produce
// the same string.
func SigningString(timestamp string, body []byte) string {
	sum := sha256.Sum256(body)
	return SignatureScheme + ":" + timestamp + ":" + hex.EncodeToString(sum[:])
}

// Sign produces the value for SignatureHeader.
func Sign(secret []byte, timestamp string, body []byte) string {
	return SignatureScheme + "=" + auth.SignHMAC(secret, SigningString(timestamp, body))
}

// VerifySignature reports whether the request is authentic and fresh.
//
// reason describes the failure for the server log only. It must never reach
// the response body: a caller that can distinguish "bad signature" from "stale
// timestamp" from "unknown address" learns more than it should.
func VerifySignature(secret []byte, tsHeader, sigHeader string, body []byte, now time.Time) (ok bool, reason string) {
	if len(secret) == 0 {
		return false, "no ingestion secret configured"
	}
	if tsHeader == "" || sigHeader == "" {
		return false, "missing signature headers"
	}

	secs, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil {
		return false, "malformed timestamp"
	}
	skew := now.Sub(time.Unix(secs, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxSkew {
		return false, fmt.Sprintf("timestamp outside the %s window (off by %s)", MaxSkew, skew.Round(time.Second))
	}

	scheme, got, found := strings.Cut(strings.TrimSpace(sigHeader), "=")
	if !found || scheme != SignatureScheme {
		return false, "unsupported signature scheme"
	}

	want := auth.SignHMAC(secret, SigningString(strings.TrimSpace(tsHeader), body))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return false, "signature mismatch"
	}
	return true, ""
}
