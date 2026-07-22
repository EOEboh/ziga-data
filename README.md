# Ziga Data

Paste unstructured lead info — text, a forwarded email, or a screenshot — and Ziga Data extracts it into structured fields (name, contact, source, need, date, notes), shows them in an editable review pane, and appends a row to **your own Google Sheet** when you confirm. Nothing is written until you confirm.

- **Shape**: one Go binary. The React frontend is built ahead of time and embedded via `go:embed`, so deploying is copying a single file — no Node, no runtime assets, no separate web server.
- **Tenancy**: multi-tenant. Every user has an account, connects their own Google account, and writes to their own spreadsheet. One user is one tenant; there are no shared workspaces yet.
- **Auth**: email + password with emailed verification, password reset, and Google sign-in. Sessions are cookie-based with CSRF on every unsafe method.
- **Destination**: the Google Sheets API called with **each user's own OAuth token** under the `drive.file` scope. Ziga Data can only see the spreadsheet a user created through the app or picked with the Google Picker — nothing else in their Drive.
- **LLM**: OpenAI `gpt-5.4-nano` via the Chat Completions API (text + vision, [structured outputs](https://platform.openai.com/docs/guides/structured-outputs) with `strict: true` guarantee schema-valid JSON). The client sits behind an interface (`internal/llm.Extractor`), so the provider/model can be swapped without touching the pipeline.
- **Frontend**: React 18 + TypeScript built with Vite (`web/`), styled with Tailwind CSS on shared design tokens. `BrowserRouter` for the auth and onboarding screens; a `useReducer` state machine for the review flow.
- **Storage**: a single SQLite file for accounts, encrypted OAuth tokens, per-user sheet links, dedup keys, pending reviews, failed writes, and history. Raw originals (full pasted text, uploaded images) are purged `RETENTION_DAYS` (default 14) after a submission is confirmed or discarded — extraction results and the short excerpt stay. The cleanup runs at boot and daily.

## Architecture

Every request that touches data is scoped to the signed-in user: the handler reads the user id from the session and passes it to the store, so queue, history, preview, confirm, and the image endpoint can only ever see one account's rows. `internal/httpapi/isolation_test.go` holds the suite that enforces this.

The write path is resolved per request by `Server.writerFor` (`internal/httpapi/sheets.go`), and which branch it takes depends entirely on whether Google OAuth is configured:

| Google OAuth configured | Writer | Used for |
|---|---|---|
| **Yes** (client id + secret + `TOKEN_ENCRYPTION_KEY`) | A per-user Sheets client built from that user's stored refresh token, targeting the spreadsheet they connected | **Production.** Each user's rows go to their own sheet |
| No | One process-wide writer: the service-account writer when `SHEET_ID` + `GOOGLE_APPLICATION_CREDENTIALS` are both set, otherwise an in-memory dry-run sheet | **Local dev and tests only** |

When OAuth is configured the process-wide writer is never consulted. There is no supported production configuration in which every tenant shares one spreadsheet.

Tokens are encrypted at rest with AES-256-GCM (`internal/secretbox`) before they reach SQLite, and refreshed access tokens are re-encrypted and written back as they rotate. Two connection states surface to the user rather than failing a write: a user who has not connected a sheet yet gets `409 Connect a destination sheet before confirming`, and a revoked or expired grant gets `409 Reconnect your Google account`, which also flags the connection so the destination picker prompts for a reconnect.

## How a submission flows

1. `POST /api/submit` (text and/or image, ≤5 MB png/jpeg/webp/gif)
2. Dedup check: SHA-256 of content + day bucket — an identical re-submit the same day returns the prior result, no second LLM call
3. The LLM extracts the fields under a strict JSON schema with **per-field confidence**; the system prompt treats submitted content as **data only** (prompt-injection defense), handles any input language, and reports low confidence instead of guessing on bad images
4. The extraction is stored as **pending** and rendered in the review pane: low-confidence fields get an amber state, missing required fields (`contact`, `need`) a red one — all editable inline
5. `POST /api/submissions/{id}/confirm` writes the (possibly edited) row to your sheet (3 attempts, exponential backoff). A terminal failure keeps the submission as `failed_write` with the edited data intact; the Retry button is the same confirm call
6. If multiple leads are detected in one submission, only the primary one is extracted and the review pane shows a banner

**Dedup semantics.** The dedup key is the SHA-256 of the submitted content plus a UTC day bucket, so identical content is blocked for the rest of the day — except after a discard. Discarding is a *soft delete*: the row is kept with status `discarded` (its original input is later purged, see retention), but its dedup hash is rewritten to a per-row tombstone, so discarding a submission immediately frees its content for genuine resubmission the same day. Discarded submissions never appear in the queue or history and can no longer be confirmed.

## Local setup

Requirements: Go 1.22+.

```sh
git clone https://github.com/EOEboh/ziga-data && cd ziga-data
cp .env.example .env   # fill in values
go run ./cmd/server
```

> The GitHub repository was renamed to `ziga-data`; clone URLs from before the rename redirect automatically. If you have a local database under the old default name, rename it to `ziga.db` or point `DB_PATH` at it.

The server loads `.env` from the working directory automatically; variables already exported in your shell take precedence over the file.

Open http://localhost:8080, create an account, and paste a lead.

Only `OPENAI_API_KEY` is required to boot. With Google OAuth left unconfigured the server runs in **dry-run mode**: sign-in is email + password only, and the destination becomes an in-memory sheet shared by the process, so the full submit → review → confirm → preview flow works without touching Google (rows are lost on restart). To exercise the failed-write UI, put the literal `[fail]` in any field before confirming.

Without `SMTP_HOST` the verification and password-reset emails are written to the server log instead of being sent — copy the link out of the log to verify a local account.

To develop against the real per-user Sheets path, configure Google OAuth as described in [Setting up Google OAuth](#setting-up-google-oauth) below.

<details>
<summary><strong>Dev/test only:</strong> the legacy shared service-account writer</summary>

Before the multi-tenant pass, the app wrote every row to one spreadsheet shared with a service account. That writer still exists so tests and the smoke script have a real Sheets target without an OAuth dance, and it is reachable **only when Google OAuth is unconfigured**.

To use it: enable the Google Sheets API in a Cloud project, create a service account and a JSON key, share your spreadsheet with the service account's email, then set `GOOGLE_APPLICATION_CREDENTIALS` and `SHEET_ID`.

**This is not a production configuration.** In production every user connects their own sheet through OAuth; if you set these variables on a deployed instance alongside a configured OAuth client, they are ignored.

</details>

### Frontend development

The UI is a Vite + React project in `web/` (sources in `web/src`). The production build output `web/dist` is committed and embedded into the binary via `go:embed`, so `go build` needs no Node — Node is only needed when changing frontend code:

```sh
npm --prefix web install   # once: react, vite, tailwind, typescript
npm --prefix web run dev   # dev server on :5173, proxies /api to :8080
make ui-check              # type-check (tsc strict)
make ui-build              # rebuild web/dist — commit the result
```

**Node version.** `.nvmrc` pins the version (22.11.0, the LTS line) and CI reads that same file, so there is one source of truth rather than a number buried in the workflow. Builds have so far proven reproducible across Node majors given the locked dependency tree — a dist built on 25 matches one built on 22 — so you do not have to switch versions to contribute. If you ever see CI's stale-dist guard reject a dist that looks correct, matching `.nvmrc` locally is the first thing to rule out. Any manager that reads `.nvmrc` works (`nvm use`, `fnm use`, `asdf install`).

Open http://localhost:5173/?mock=1 (or :8080/?mock=1 against the embedded build) to drive the UI against built-in fixtures (all confidence states, a failing confirm) with no backend calls.

### Environment variables

Grouped as in [`.env.example`](.env.example). Everything in the first four groups is live in production; the last group exists only for local dev and tests.

**Core**

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `OPENAI_API_KEY` | ✅ | — | OpenAI API key (server-side only, never sent to the browser) |
| `LLM_MODEL` | | `gpt-5.4-nano` | Any vision-capable OpenAI chat model |
| `PORT` | | `8080` | HTTP port |
| `DB_PATH` | | `./ziga.db` | SQLite file. The only persistent state |
| `SCHEMA_PATH` | | `config/schema.json` | Extraction schema + column mapping |
| `RATE_LIMIT_PER_MIN` | | `10` | Per-IP submissions per minute (burst 5). Login and password reset have their own stricter budgets |
| `RETENTION_DAYS` | | `14` | Days after confirm/discard before the raw original input (full text, image) is purged |

**Auth and sessions**

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `APP_BASE_URL` | in production | `http://localhost:8080` | Public origin. Builds the links in verification and reset emails, and decides whether session cookies get `Secure` (any `https://` origin) |
| `SESSION_SECRET` | in production | generated | HMAC key for CSRF tokens. If empty, an ephemeral one is generated at boot and sessions do not survive a restart |

**Google OAuth** (identity + per-user Sheets)

Leave the client id and secret empty to run without Google sign-in and without per-user Sheets — see dry-run mode above.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `GOOGLE_OAUTH_CLIENT_ID` | for the real write path | — | From a Google Cloud OAuth 2.0 Client (Web application) |
| `GOOGLE_OAUTH_CLIENT_SECRET` | for the real write path | — | Same client |
| `OAUTH_REDIRECT_URL` | | `$APP_BASE_URL/api/auth/google/callback` | Must match the redirect URI registered in the Cloud console exactly |
| `TOKEN_ENCRYPTION_KEY` | ✅ whenever OAuth is set | — | Base64 32-byte key encrypting OAuth tokens at rest (AES-256-GCM). **The server refuses to boot** if OAuth is configured and this is empty, so tokens can never be stored in plaintext. Generate with `head -c 32 /dev/urandom \| base64` |
| `GOOGLE_PICKER_API_KEY` | for the attach flow | — | Browser API key served to the frontend for the Google Picker (attach an existing sheet) |

**Sheet layout**

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `SHEET_TAB` | | `Leads` | Tab name used when the app creates a user's spreadsheet. Attached (Picker-selected) sheets use their own first tab instead |
| `HEADER_ROW` | | `1` | `1`/`true`: maintain a header row of the schema's column names — written when the app creates a sheet, and on the first append into an otherwise empty tab. `0`/`none`/`false`: no header row |

**Email** — verification and password reset. With `SMTP_HOST` empty, links are logged instead of sent.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `SMTP_HOST` | in production | — | Outbound SMTP host. Empty selects the dev mailer that logs links |
| `SMTP_PORT` | | `587` | |
| `SMTP_USERNAME` | | — | |
| `SMTP_PASSWORD` | | — | |
| `SMTP_FROM` | | `ziga@localhost` | From address on outbound mail |

**Dev/test only** — the legacy shared service-account writer. Ignored once `GOOGLE_OAUTH_CLIENT_ID` is set. See the collapsed section above.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `GOOGLE_APPLICATION_CREDENTIALS` | — | — | Path to a service-account JSON key |
| `SHEET_ID` | — | — | Spreadsheet ID of the one shared sheet |

## Setting up Google OAuth

This is what enables the real write path: users sign in with Google and rows go to their own spreadsheets.

1. In [Google Cloud Console](https://console.cloud.google.com), create (or pick) a project and **enable the Google Sheets API** and the **Google Drive API**.
2. Configure the **OAuth consent screen**. The app name must be `Ziga Data`, matching the marketing site and privacy policy a reviewer will check. Add these scopes and no others:

   | Scope | Why |
   |---|---|
   | `openid`, `.../auth/userinfo.email`, `.../auth/userinfo.profile` | Sign-in and identifying which account a lead belongs to |
   | `.../auth/drive.file` | Per-file access to the spreadsheet the user creates or picks |

   `drive.file` is deliberate: it grants access only to files the app created or the user explicitly selected, so Ziga Data never sees the rest of a user's Drive. Do **not** add the broad `spreadsheets` scope — `internal/oauth/oauth_test.go` asserts it is absent, because adding it would escalate the app into Google's restricted-scope review.

3. Create an **OAuth 2.0 Client ID** of type *Web application*. Set the authorized redirect URI to `$APP_BASE_URL/api/auth/google/callback` (for local dev, `http://localhost:8080/api/auth/google/callback`). It must match `OAUTH_REDIRECT_URL` exactly.
4. Create a **browser API key** for the Google Picker and set `GOOGLE_PICKER_API_KEY`. This is what powers the "attach an existing sheet" flow.
5. Generate the token-encryption key and set `TOKEN_ENCRYPTION_KEY`:

   ```sh
   head -c 32 /dev/urandom | base64
   ```

6. Set `GOOGLE_OAUTH_CLIENT_ID` and `GOOGLE_OAUTH_CLIENT_SECRET`, then restart. The boot log should show `google oauth enabled` with the four scopes.

### What a user then does

Sign in with Google, then either let Ziga Data **create** a spreadsheet (`POST /api/sheets/create` — makes a "Ziga Leads" sheet with a `SHEET_TAB` tab and writes the header row) or **attach** an existing one through the Google Picker (`POST /api/sheets/attach` — appends to that spreadsheet's first tab). With the default `HEADER_ROW=1`, a created sheet gets the `columns` from `config/schema.json` as row 1:

| date | name | contact | source | need | notes | flags |
|------|------|---------|--------|------|-------|-------|

From then on every confirmed row is appended to that user's sheet using their own token. If they later revoke access from their Google account, the next write returns a reconnect prompt rather than failing silently.

## API

Every unsafe method carries CSRF. Routes marked 🔒 require a session and operate only on the signed-in user's data.

**Auth**

| Endpoint | Description |
|---|---|
| `POST /api/auth/signup` | `{email, password}`. Creates the account and emails a verification link. Returns `201`; `409` if the email is taken |
| `GET /api/auth/verify?token=` | Consumes the emailed single-use token, marks the account verified, redirects into the app |
| `POST /api/auth/login` | `{email, password}`. Rate-limited separately from the API budget |
| `POST /api/auth/logout` | Clears the session |
| `POST /api/auth/password/forgot` | Emails a reset link. Rate-limited; response does not reveal whether the address exists |
| `POST /api/auth/password/reset` | `{token, password}` |
| `GET /api/me` | Current user, sheet connection state, and whether a reconnect is needed |

**Google**

| Endpoint | Description |
|---|---|
| `GET /api/auth/google/start` | Begins the OAuth flow with an anti-forgery state cookie |
| `GET /api/auth/google/callback` | Exchanges the code, stores encrypted tokens, starts the session. Links to an existing account only when that account's email is already verified; an unverified match is refused rather than silently linked |
| `POST /api/auth/google/disconnect` 🔒 | Drops the Google link and its stored tokens |
| `POST /api/sheets/create` 🔒 | Creates a "Ziga Leads" spreadsheet under the user's account and records it as their destination |
| `POST /api/sheets/attach` 🔒 | `{spreadsheet_id}` from the Picker. Records an existing spreadsheet, appending to its first tab |

**Submissions** (all 🔒)

| Endpoint | Description |
|---|---|
| `POST /api/submit` | multipart form: `text` and/or `image`. Extract-only — stores a `pending` submission and returns `{id, status, result, field_states, flags, input, created_at}`. Writes nothing to the sheet |
| `POST /api/submissions/{id}/confirm` | body `{"fields": {name: value, ...}}` with the reviewed values. The only path that appends a sheet row. Accepts `pending` and `failed_write` (retry = same call); `422` if a required field is still empty; `409` once written, if discarded, if no sheet is connected yet, or if the Google grant needs reconnecting |
| `POST /api/submissions/{id}/discard` | soft-delete a pending/failed submission: row retained as `discarded`, same-day dedup hash freed. Idempotent; discarded items leave the queue and history |
| `GET /api/submissions/{id}/image` | the original uploaded image |
| `GET /api/queue` | pending + failed submissions, newest 100, with `count` for the badge |
| `GET /api/preview` | last 3 data rows of the user's sheet. Degrades to an empty strip rather than erroring when no sheet is connected or the grant is broken |
| `GET /api/destination` | the user's connected destination, for the picker |
| `GET /api/history` | last 50 written submissions |

`GET /healthz` is the unauthenticated liveness probe.

`status` is `pending` / `written` / `failed_write` / `discarded`. `/api/submit` (LLM cost) and `/api/submissions/{id}/confirm` (Google Sheets quota) share one per-IP rate-limit budget (`RATE_LIMIT_PER_MIN`).

Every request is logged as structured JSON (content hash — never raw content —, confidence, missing fields, status, duration).

## Live smoke test

```sh
make smoke
```

Runs the full submit → confirm → preview flow against a running server (or starts one). It needs a real `OPENAI_API_KEY`. This exercises the **dev path**, not the per-user OAuth path: with `SHEET_ID` + `GOOGLE_APPLICATION_CREDENTIALS` set it appends a clearly marked test lead (`SMOKE TEST smoke-<timestamp> … Safe to delete.`) to that shared sheet and verifies it comes back through `/api/preview` — delete the row afterwards. Without sheet config it exercises the in-memory destination. A timestamp nonce defeats the same-day dedup, so it can be re-run freely.

Checking the real per-user write path means signing in with Google against a configured OAuth client and confirming a lead by hand; there is no automated end-to-end for it.

## Customizing the schema

`config/schema.json` defines the extracted fields, which are required (gating), and the sheet column order. The prompt, JSON schema, validation, and sheet writer are all driven from it — per-user custom schemas later only need a config per user, not code changes.

## Tests

```sh
go test ./...
```

Covers the per-field confidence matrix, date defaulting, JSON-schema shape, dedup/store behavior including the legacy-database migration, and the confirm/retry/discard handler paths. On the multi-tenant side: `internal/httpapi/auth_test.go` (signup, verification, login, reset), `internal/httpapi/oauth_test.go` (the callback and the account-linking rules), `internal/httpapi/sheets_test.go` (create and attach), `internal/oauth/oauth_test.go` (which asserts the broad `spreadsheets` scope is never requested), and `internal/httpapi/isolation_test.go`, which walks every user-scoped endpoint and fails if one account can reach another's data.

For a live end-to-end check, run the server and try: a normal lead, one containing "ignore previous instructions…", a two-lead message, and a non-English lead.

## Deployment

Production deployment to the Hetzner box is fully versioned under [`deploy/`](deploy/)
and driven by a copy-paste runbook — **[deploy/RUNBOOK.md](deploy/RUNBOOK.md)**.

The `deploy/` directory contains:

- **`ziga.service`** — hardened systemd unit (dedicated `ziga` user, resource
  caps, `ProtectSystem=strict`).
- **`nginx-ziga.conf`** — TLS-terminating reverse-proxy server block with an
  `/api/` rate limit and a 6 MB body ceiling (above the app's 5 MB image limit).
- **`backup-ziga.sh`** + **`backup-ziga.cron`** — nightly WAL/journal-safe SQLite
  backup to R2 via rclone, with a 30-day retention prune and a `BACKUP_DRY_RUN`
  mode. The runbook's backup step includes a **mandatory restore test**.

Pushes to `main` build and deploy automatically via
[`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) (atomic binary
swap, health-check, and automatic rollback on failure). The runbook covers the
one-time server setup, the Cloudflare origin cert, the restricted CI deploy user,
and the required GitHub secrets.

**Backups.** The SQLite file at `DB_PATH` is the only persistent state — it holds
the dedup keys, the review queue, and history. The Google Sheet only holds
confirmed rows, so it is not a substitute for backing up the database. See the
runbook's backup + restore-test section.

> **Staging note:** auth has landed, so the blocker the RUNBOOK describes is
> cleared. DNS for `app.zigadata.com` is still not flipped and access remains
> via the SSH tunnel; what is left before the flip is production configuration
> rather than code — a real OAuth client with its redirect URI, `SESSION_SECRET`
> and `TOKEN_ENCRYPTION_KEY` in `/opt/ziga/ziga.env`, and working SMTP so
> verification mail is delivered instead of logged. See RUNBOOK §h.

## TODO

Deliberately out of scope for now:

- Queue navigation (prev/next between queued items; today the review pane auto-advances FIFO)
- Multi-lead extraction (splitting one paste into several rows; today only the primary lead is extracted and a banner is shown)
- History depth (pagination/search beyond the last 50 written submissions)

Deferred from the multi-tenant auth pass:

- **Billing / subscriptions** — no plans or payment yet; every account is free.
- **Team accounts** — one user = one tenant; no shared workspaces or member roles.
- **Marketing site** — the app is the only surface; `zigadata.com` marketing pages are separate. (In flight on the `marketing-site` branch — update this line when that PR merges.)
- **Google app-verification submission** — the code targets the `drive.file`
  scope (not the broad `spreadsheets` scope) so verification stays light; the
  formal submission happens before a public launch.

## Changelog

- 2026-07 — renamed to **Ziga Data** (formerly sheetdrop)
