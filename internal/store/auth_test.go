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
		UserID: u.ID, Provider: "google", ProviderSub: "google-123",
		AccessTokenEnc: []byte("enc-access"), RefreshTokenEnc: []byte("enc-refresh"),
		TokenExpiry: time.Now().Add(time.Hour).UTC().Truncate(time.Second), Scopes: "openid email",
	}
	if err := st.UpsertOAuthAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOAuthAccount(ctx, u.ID, "google")
	if err != nil || string(got.AccessTokenEnc) != "enc-access" || got.ProviderSub != "google-123" {
		t.Fatalf("get oauth: %+v err=%v", got, err)
	}
	if got.Broken() {
		t.Fatal("fresh account must not be broken")
	}

	bySub, err := st.GetOAuthAccountBySub(ctx, "google", "google-123")
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

// TestUpsertOAuthAccountReconnectSameSub covers reconnecting a provider the
// user has already linked, which is what every Notion reconnect does.
//
// oauth_accounts carries TWO unique indexes: PRIMARY KEY (user_id, provider)
// and a standalone UNIQUE on provider_sub. The upsert only names the first as
// its conflict target, but SQLite checks the provider_sub index first, so a
// re-link with the same subject aborted before the ON CONFLICT clause could
// apply:
//
//	UNIQUE constraint failed: oauth_accounts.provider_sub (2067)
//
// In production this meant the first Notion connect succeeded and every
// reconnect after it failed with notion_error=server.
func TestUpsertOAuthAccountReconnectSameSub(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "reconnect@example.com", "")
	uid := u.ID

	first := &OAuthAccount{
		UserID: uid, Provider: "notion", ProviderSub: "bot-abc",
		AccessTokenEnc: []byte("enc-v1"), Scopes: "notion:granted-resources",
	}
	if err := st.UpsertOAuthAccount(ctx, first); err != nil {
		t.Fatalf("first connect: %v", err)
	}

	// Same user, same provider, same bot id, a freshly issued token.
	again := &OAuthAccount{
		UserID: uid, Provider: "notion", ProviderSub: "bot-abc",
		AccessTokenEnc: []byte("enc-v2"), Scopes: "notion:granted-resources",
	}
	if err := st.UpsertOAuthAccount(ctx, again); err != nil {
		t.Fatalf("reconnect with same provider_sub: %v", err)
	}

	got, err := st.GetOAuthAccount(ctx, uid, "notion")
	if err != nil {
		t.Fatalf("GetOAuthAccount: %v", err)
	}
	if string(got.AccessTokenEnc) != "enc-v2" {
		t.Errorf("access token = %q, want the reconnect's %q", got.AccessTokenEnc, "enc-v2")
	}
}

// TestUpsertOAuthAccountTwoUsersSameNotionWorkspace covers two different Ziga
// users connecting the SAME Notion workspace. Notion issues one bot id per
// install, so both users legitimately present the same provider_sub.
//
// oauth_accounts declared provider_sub globally UNIQUE, which is the right
// identity rule for Google (that subject is how sign-in resolves a user) but
// wrong for Notion, which the code is explicit is "never a sign-in, only ever
// a destination". The second user's connect aborted with:
//
//	UNIQUE constraint failed: oauth_accounts.provider_sub (2067)
//
// surfacing as notion_error=server with nothing actionable for the user.
func TestUpsertOAuthAccountTwoUsersSameNotionWorkspace(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a, _ := st.CreateUser(ctx, "teammate-a@example.com", "")
	b, _ := st.CreateUser(ctx, "teammate-b@example.com", "")

	const sharedBot = "bot-shared-workspace"

	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: a.ID, Provider: "notion", ProviderSub: sharedBot,
		AccessTokenEnc: []byte("enc-a"),
	}); err != nil {
		t.Fatalf("first user connect: %v", err)
	}

	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: b.ID, Provider: "notion", ProviderSub: sharedBot,
		AccessTokenEnc: []byte("enc-b"),
	}); err != nil {
		t.Fatalf("second user connecting the same workspace: %v", err)
	}

	// Each user keeps their own token for the shared workspace.
	gotA, err := st.GetOAuthAccount(ctx, a.ID, "notion")
	if err != nil || string(gotA.AccessTokenEnc) != "enc-a" {
		t.Errorf("user A token = %q err=%v, want enc-a", gotA.AccessTokenEnc, err)
	}
	gotB, err := st.GetOAuthAccount(ctx, b.ID, "notion")
	if err != nil || string(gotB.AccessTokenEnc) != "enc-b" {
		t.Errorf("user B token = %q err=%v, want enc-b", gotB.AccessTokenEnc, err)
	}
}

// TestUpsertOAuthAccountGoogleSubStaysUnique guards the other half: a Google
// subject is a sign-in identity, so it must never map to two users.
func TestUpsertOAuthAccountGoogleSubStaysUnique(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a, _ := st.CreateUser(ctx, "g-a@example.com", "")
	b, _ := st.CreateUser(ctx, "g-b@example.com", "")

	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: a.ID, Provider: "google", ProviderSub: "google-shared",
		AccessTokenEnc: []byte("enc-a"),
	}); err != nil {
		t.Fatalf("first google link: %v", err)
	}
	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: b.ID, Provider: "google", ProviderSub: "google-shared",
		AccessTokenEnc: []byte("enc-b"),
	}); err == nil {
		t.Fatal("second user claimed the same Google subject; want a uniqueness error")
	}
}
