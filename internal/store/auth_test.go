package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUserCreateAndLookup(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	u, err := st.CreateUser(ctx, "a@example.com", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.Verified() {
		t.Fatalf("new user should have id and be unverified: %+v", u)
	}

	// Duplicate email is rejected by the UNIQUE constraint.
	if _, err := st.CreateUser(ctx, "a@example.com", "hash2"); err == nil {
		t.Fatal("duplicate email must error")
	}

	byEmail, err := st.GetUserByEmail(ctx, "a@example.com")
	if err != nil || byEmail.ID != u.ID || byEmail.PasswordHash != "hash1" {
		t.Fatalf("get by email: %+v err=%v", byEmail, err)
	}
	if _, err := st.GetUserByEmail(ctx, "missing@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing email should be ErrNotFound, got %v", err)
	}

	// Verify + password change round-trip.
	if err := st.MarkEmailVerified(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPasswordHash(ctx, u.ID, "hash3"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetUser(ctx, u.ID)
	if !got.Verified() || got.PasswordHash != "hash3" {
		t.Fatalf("verify/password not applied: %+v", got)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "s@example.com", "h")

	if err := st.CreateSession(ctx, "sid-live", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sess, err := st.GetSession(ctx, "sid-live")
	if err != nil || sess.UserID != u.ID {
		t.Fatalf("get session: %+v err=%v", sess, err)
	}

	// Expired sessions read as absent.
	if err := st.CreateSession(ctx, "sid-old", u.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(ctx, "sid-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session should be ErrNotFound, got %v", err)
	}

	// Logout deletes.
	if err := st.DeleteSession(ctx, "sid-live"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(ctx, "sid-live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session should be ErrNotFound, got %v", err)
	}

	// Expiry sweep removes the stale one.
	n, err := st.DeleteExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired sweep = %d err=%v, want 1", n, err)
	}
}

func TestOAuthAccountRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "o@example.com", "")

	acct := &OAuthAccount{
		UserID: u.ID, Provider: "google", GoogleSub: "google-123",
		AccessTokenEnc: []byte("enc-access"), RefreshTokenEnc: []byte("enc-refresh"),
		TokenExpiry: time.Now().Add(time.Hour).UTC().Truncate(time.Second), Scopes: "openid email",
	}
	if err := st.UpsertOAuthAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOAuthAccount(ctx, u.ID, "google")
	if err != nil || string(got.AccessTokenEnc) != "enc-access" || got.GoogleSub != "google-123" {
		t.Fatalf("get oauth: %+v err=%v", got, err)
	}
	if got.Broken() {
		t.Fatal("fresh account must not be broken")
	}

	bySub, err := st.GetOAuthAccountBySub(ctx, "google-123")
	if err != nil || bySub.UserID != u.ID {
		t.Fatalf("get by sub: %+v err=%v", bySub, err)
	}

	// Refresh persists a new access token.
	if err := st.UpdateOAuthTokens(ctx, u.ID, "google", []byte("enc-access-2"), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetOAuthAccount(ctx, u.ID, "google")
	if string(got.AccessTokenEnc) != "enc-access-2" {
		t.Fatalf("refresh not persisted: %s", got.AccessTokenEnc)
	}

	// Broken then reconnect clears it.
	if err := st.MarkOAuthBroken(ctx, u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetOAuthAccount(ctx, u.ID, "google"); !got.Broken() {
		t.Fatal("account should be broken")
	}
	if err := st.UpsertOAuthAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetOAuthAccount(ctx, u.ID, "google"); got.Broken() {
		t.Fatal("reconnect must clear broken flag")
	}

	if err := st.DeleteOAuthAccount(ctx, u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOAuthAccount(ctx, u.ID, "google"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted account should be ErrNotFound, got %v", err)
	}
}

func TestUserSheetRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "sh@example.com", "")

	if _, err := st.GetUserSheet(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no sheet yet should be ErrNotFound, got %v", err)
	}
	if err := st.SetUserSheet(ctx, &UserSheet{UserID: u.ID, SpreadsheetID: "sheet-1", SheetTab: "Leads", CreatedByApp: true}); err != nil {
		t.Fatal(err)
	}
	sh, err := st.GetUserSheet(ctx, u.ID)
	if err != nil || sh.SpreadsheetID != "sheet-1" || !sh.CreatedByApp || sh.Broken() {
		t.Fatalf("get sheet: %+v err=%v", sh, err)
	}

	// Switching sheets replaces and clears broken.
	if err := st.MarkSheetBroken(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if sh, _ = st.GetUserSheet(ctx, u.ID); !sh.Broken() {
		t.Fatal("sheet should be broken")
	}
	if err := st.SetUserSheet(ctx, &UserSheet{UserID: u.ID, SpreadsheetID: "sheet-2", SheetTab: "Leads", CreatedByApp: false}); err != nil {
		t.Fatal(err)
	}
	sh, _ = st.GetUserSheet(ctx, u.ID)
	if sh.SpreadsheetID != "sheet-2" || sh.CreatedByApp || sh.Broken() {
		t.Fatalf("switch sheet not applied: %+v", sh)
	}
}

func TestAuthTokenSingleUse(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "t@example.com", "")

	if err := st.CreateAuthToken(ctx, u.ID, TokenVerifyEmail, "hash-live", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Wrong kind must not consume it.
	if _, err := st.ConsumeAuthToken(ctx, TokenPasswordReset, "hash-live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-kind consume should fail: %v", err)
	}
	uid, err := st.ConsumeAuthToken(ctx, TokenVerifyEmail, "hash-live")
	if err != nil || uid != u.ID {
		t.Fatalf("consume: uid=%d err=%v", uid, err)
	}
	// Second use fails (single-use).
	if _, err := st.ConsumeAuthToken(ctx, TokenVerifyEmail, "hash-live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume must fail: %v", err)
	}

	// Expired token can't be consumed.
	if err := st.CreateAuthToken(ctx, u.ID, TokenPasswordReset, "hash-exp", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeAuthToken(ctx, TokenPasswordReset, "hash-exp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired consume must fail: %v", err)
	}
}
