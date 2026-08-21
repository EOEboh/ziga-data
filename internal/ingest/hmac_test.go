package ingest

import (
	"strings"
	"testing"
	"time"
)

// The mail worker signs in TypeScript and this package verifies in Go. Two
// implementations of one scheme drift silently, and the failure mode is total:
// every lead stops arriving, with nothing but 401s to show for it.
//
// So the canonical form is pinned here as a known answer, and
// worker/email-ingest/test/sign.test.ts asserts the identical vector. If you
// change the scheme, both sides fail together and on purpose.
const (
	knownSecret    = "test-secret-at-least-32-chars-long!!"
	knownTimestamp = "1755000000"
	knownBody      = `{"v":1,"to":"lead-abc@in.example.com"}`

	// SHA-256 of knownBody, hex.
	knownBodyHash = "fa69fbeccb120cc4d8b09441801f3ff8f2e8ac276dd5d31e0abc21ed43ca930c"
	// HMAC-SHA256 of "v1:{knownTimestamp}:{knownBodyHash}" under knownSecret,
	// base64url without padding.
	knownVectorSignature = "_cUrPMjZK2McM5nn9MeWGfrjcOQjDo7Q6g5bkwC0D4g"
)

func TestSigningStringShape(t *testing.T) {
	got := SigningString(knownTimestamp, []byte(knownBody))

	// The digest is fixed-width, which is what stops bytes being shifted
	// between the timestamp and the body to produce a colliding string.
	const wantLen = len("v1:") + len(knownTimestamp) + 1 + 64
	if len(got) != wantLen {
		t.Fatalf("signing string length = %d, want %d: %q", len(got), wantLen, got)
	}
	if got[:3] != "v1:" {
		t.Errorf("signing string must carry the scheme prefix, got %q", got)
	}

	// The body is hashed, not embedded, so a large payload does not produce a
	// large signing string.
	big := make([]byte, 1<<20)
	if len(SigningString(knownTimestamp, big)) != wantLen {
		t.Error("signing string length must not depend on body size")
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	secret := []byte(knownSecret)
	body := []byte(knownBody)
	now := time.Unix(1755000000, 0)
	ts := knownTimestamp

	sig := Sign(secret, ts, body)
	if ok, reason := VerifySignature(secret, ts, sig, body, now); !ok {
		t.Fatalf("a freshly signed request must verify, got %q", reason)
	}
}

func TestVerifySignatureRejections(t *testing.T) {
	secret := []byte(knownSecret)
	body := []byte(knownBody)
	now := time.Unix(1755000000, 0)
	ts := knownTimestamp
	good := Sign(secret, ts, body)

	tampered := append([]byte{}, body...)
	tampered[len(tampered)-1] = '!'

	cases := []struct {
		name         string
		secret       []byte
		ts, sig      string
		body         []byte
		now          time.Time
		wantContains string
	}{
		{"tampered body", secret, ts, good, tampered, now, "mismatch"},
		{"tampered timestamp", secret, "1755000001", good, body, now, "mismatch"},
		{"wrong secret", []byte("another-secret-at-least-32-chars!!!!"), ts, good, body, now, "mismatch"},
		{"missing signature", secret, ts, "", body, now, "missing"},
		{"missing timestamp", secret, "", good, body, now, "missing"},
		{"malformed timestamp", secret, "not-a-number", good, body, now, "malformed"},
		{"unknown scheme", secret, ts, "v9=" + good[3:], body, now, "scheme"},
		{"unprefixed signature", secret, ts, good[3:], body, now, "scheme"},
		{"no secret configured", nil, ts, good, body, now, "no ingestion secret"},
		// Replay: a captured request stops working once it ages out. Inside
		// the window a replay is harmless anyway — ingestion is idempotent —
		// but the window must still close.
		{"stale timestamp", secret, ts, good, body, now.Add(MaxSkew + time.Second), "window"},
		// A future timestamp is equally suspect: it would otherwise extend the
		// replay window arbitrarily far forward.
		{"future timestamp", secret, ts, good, body, now.Add(-MaxSkew - time.Second), "window"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := VerifySignature(c.secret, c.ts, c.sig, c.body, c.now)
			if ok {
				t.Fatalf("want rejection, got accepted")
			}
			if !contains(reason, c.wantContains) {
				t.Errorf("reason = %q, want it to mention %q", reason, c.wantContains)
			}
		})
	}
}

func TestVerifyAcceptsClockDriftInsideTheWindow(t *testing.T) {
	secret := []byte(knownSecret)
	body := []byte(knownBody)
	signed := time.Unix(1755000000, 0)
	sig := Sign(secret, knownTimestamp, body)

	// Drift in either direction, just inside the window, must be tolerated:
	// the worker and this host keep separate clocks.
	for _, skew := range []time.Duration{MaxSkew - time.Second, -(MaxSkew - time.Second)} {
		if ok, reason := VerifySignature(secret, knownTimestamp, sig, body, signed.Add(skew)); !ok {
			t.Errorf("skew %s must be tolerated, got %q", skew, reason)
		}
	}
}

// TestKnownAnswerVector pins the exact signature for a fixed input. The
// TypeScript signer asserts the same value; if this changes, the worker stops
// being able to talk to us.
func TestKnownAnswerVector(t *testing.T) {
	got := Sign([]byte(knownSecret), knownTimestamp, []byte(knownBody))
	const want = "v1=" + knownVectorSignature
	if got != want {
		t.Fatalf("signature vector changed.\n got: %s\nwant: %s\n\nIf this was deliberate, update worker/email-ingest/test/sign.test.ts to match, or the worker will start getting 401s.", got, want)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestSigningStringPinsTheBodyHash guards the other half of the cross-language
// contract: the TypeScript side hashes the body itself, so the digest of a
// fixed body has to match too.
func TestSigningStringPinsTheBodyHash(t *testing.T) {
	want := "v1:" + knownTimestamp + ":" + knownBodyHash
	if got := SigningString(knownTimestamp, []byte(knownBody)); got != want {
		t.Fatalf("signing string changed.\n got: %s\nwant: %s", got, want)
	}
}
