<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/png/lockup-dark-1600.png">
    <img src="brand/png/lockup-light-1600.png" alt="Ziga Data" width="260">
  </picture>
</h1>

Paste unstructured lead info — text, a forwarded email, or a screenshot — and Ziga Data extracts it into structured fields (name, contact, source, need, date, notes), shows them in an editable review pane, and writes it to **your own Google Sheet or Notion database** when you confirm. Nothing is written until you confirm.

- **Shape**: one Go binary. The React frontend is built ahead of time and embedded via `go:embed`, so deploying is copying a single file — no Node, no runtime assets, no separate web server.
- **Tenancy**: multi-tenant. Every user has an account, connects their own Google or Notion account, and writes to their own destination. One user is one tenant; there are no shared workspaces yet.
- **Auth**: email + password with emailed verification, password reset, and Google sign-in. Sessions are cookie-based with CSRF on every unsafe method.
- **Destination**: a pluggable interface (`internal/destination.Writer`) with two implementations, one active per user.
  - **Google Sheets** — called with each user's own OAuth token under the `drive.file` scope. Ziga Data can only see the spreadsheet a user created through the app or picked with the Google Picker — nothing else in their Drive.
  - **Notion** — called with each user's own workspace token from a public connection. Notion's consent screen has the user pick exactly which pages and databases to share; Ziga Data never requests whole-workspace access.
- **LLM**: OpenAI `gpt-5.4-nano` via the Chat Completions API (text + vision, [structured outputs](https://platform.openai.com/docs/guides/structured-outputs) with `strict: true` guarantee schema-valid JSON). The client sits behind an interface (`internal/llm.Extractor`), so the provider/model can be swapped without touching the pipeline.
- **Frontend**: React 18 + TypeScript built with Vite (`web/`), styled with Tailwind CSS on shared design tokens. `BrowserRouter` for the auth and onboarding screens; a `useReducer` state machine for the review flow.
- **Storage**: a single SQLite file for accounts, encrypted OAuth tokens, per-user destinations, dedup keys, pending reviews, failed writes, and history. Raw originals (full pasted text, uploaded images) are purged `RETENTION_DAYS` (default 14) after a submission is confirmed or discarded — extraction results and the short excerpt stay. The cleanup runs at boot and daily.

## Architecture

Every request that touches data is scoped to the signed-in user: the handler reads the user id from the session and passes it to the store, so queue, history, preview, confirm, and the image endpoint can only ever see one account's rows. `internal/httpapi/isolation_test.go` holds the suite that enforces this.

The write path is resolved per request by `Server.writerFor` (`internal/httpapi/sheets.go`), which is the one place that knows which implementation serves which destination type. Everything downstream — confirm, preview — works through `destination.Writer`:

| Provider configured | Writer | Used for |
|---|---|---|
| **Google** (client id + secret + `TOKEN_ENCRYPTION_KEY`) | A per-user Sheets client built from that user's stored refresh token, targeting the spreadsheet they connected | **Production** |
| **Notion** (client id + secret + redirect + `TOKEN_ENCRYPTION_KEY`) | A per-user Notion client targeting the data source they connected, with the field→property mapping they reviewed | **Production** |
| Neither | One process-wide writer: the service-account writer when `SHEET_ID` + `GOOGLE_APPLICATION_CREDENTIALS` are both set, otherwise an in-memory dry-run sheet | **Local dev and tests only** |

When either provider is configured the process-wide writer is never consulted. There is no supported production configuration in which every tenant shares one destination.

**One destination at a time.** A user writes to a Google Sheet *or* a Notion database, stored as a single row in `destinations` (type + a per-type JSON config + connect time + broken flag). Switching replaces it. Fan-out to both is not in this pass — see [TODO](#todo).

**The unit of exchange is a lead, not a row.** `destination.Lead` is ordered (field, value) pairs. A sheet writes the values in column order; Notion needs to know which field each value came from, because its properties are typed. A write returns which fields the destination could not accept, so a partial write is reported rather than silently lossy.

Tokens are encrypted at rest with AES-256-GCM (`internal/secretbox`) before they reach SQLite, and refreshed Google access tokens are re-encrypted and written back as they rotate. Notion tokens are long-lived and carry a refresh token only when the connection uses token rotation, so both refresh and expiry are optional there.

Two connection states surface to the user rather than failing a write: a user with no destination yet gets `409 Connect a destination before confirming`, and a revoked or expired grant gets a `409` reconnect prompt that also flags the destination so the picker and account menu ask for a reconnect. A **broken destination is still a configured one** — that user stays in the app with a reconnect prompt and can keep submitting and reviewing; only the write is blocked.

## How a submission flows

1. `POST /api/submit` (text and/or image, ≤5 MB png/jpeg/webp/gif)
2. Dedup check: SHA-256 of content + day bucket — an identical re-submit the same day returns the prior result, no second LLM call
3. The LLM extracts the fields under a strict JSON schema with **per-field confidence**; the system prompt treats submitted content as **data only** (prompt-injection defense), handles any input language, and reports low confidence instead of guessing on bad images
4. The extraction is stored as **pending** and rendered in the review pane: low-confidence fields get an amber state, missing required fields (`contact`, `need`) a red one — all editable inline
5. `POST /api/submissions/{id}/confirm` writes the (possibly edited) row to your sheet (3 attempts, exponential backoff). A terminal failure keeps the submission as `failed_write` with the edited data intact; the Retry button is the same confirm call
6. If multiple leads are detected in one submission, only the primary one is extracted and the review pane shows a banner

## How an emailed lead flows

Each user can turn on a private capture address (`lead-<16 random chars>@in.zigadata.com`). Mail sent or auto-forwarded there ends up in the same review queue, and **still waits for a human** — ingestion automates capture, never confirmation.

1. Cloudflare Email Routing matches the address and hands the message to the Worker in [`worker/email-ingest/`](worker/email-ingest/)
2. The Worker parses MIME and `POST`s JSON to `/api/ingest/email`, signed with HMAC-SHA256 over the request body. It never decides whether something is a lead — all judgement lives here, where it is testable
3. The **filter pipeline** runs before any model call, cheapest check first: blocked senders → forwarding-confirmation handshake → machine mail → structure → HTML→text → length. Anything filtered becomes a visible quarantine row, never a drop
4. Dedup by Message-ID *and* content hash, then the per-user daily cap. Dedup runs first so a mail system's retry never spends budget
5. **Origin resolution** decides whose lead it is. An auto-forward preserves the original `From:`; a manual forward does not, and the sender is parsed out of the body — for a thread, the *earliest* message, skipping the user's own
6. Extraction runs through the same `ingestLead` path as a paste, and the result lands as `pending`

**Never silently lost.** Every rejection is recorded and reachable: filtered mail in the quarantine view with the reason and a one-click rescue, and mail we could not deliver at all forwarded to a fallback mailbox by the Worker.

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

The Notion path is walkable there too:

| URL | State |
|---|---|
| `/?mock=1` | Google Sheet destination (the default) |
| `/onboarding-notion?mock=1` | The Notion connect screen; "Connect Notion" simulates returning from consent |
| `/?mock=1&notion=connected` | Already on a Notion destination |
| `/?mock=1&verify=1#/email` | Email capture setup, with a pending forwarding-confirmation code |
| `/?mock=1#/quarantine` | Filtered mail: rescue, dismiss, block sender |
| `/?mock=1&email=off` | Before opting in to email capture |
| `/?mock=1&notion=broken` | Notion access revoked — the reconnect prompts in the topbar, account menu, and confirm |

The fixture workspace deliberately includes a database with no `notes` property and an unwritable `status` property, so the dropped-fields notice and the excluded-property behavior are both reachable.

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
| `TOKEN_ENCRYPTION_KEY` | ✅ whenever any OAuth provider is set | — | Base64 32-byte key encrypting Google **and** Notion tokens at rest (AES-256-GCM). **The server refuses to boot** if a provider is configured and this is empty, so tokens can never be stored in plaintext. Generate with `head -c 32 /dev/urandom \| base64` |
| `GOOGLE_PICKER_API_KEY` | for the attach flow | — | Browser API key served to the frontend for the Google Picker (attach an existing sheet) |

**Notion OAuth** (per-user Notion database destination)

All three of the `NOTION_OAUTH_*` variables, or none. Partial configuration is a boot error naming the missing ones; with none set, Notion is not offered.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `NOTION_OAUTH_CLIENT_ID` | to offer Notion | — | From a Notion **public connection** |
| `NOTION_OAUTH_CLIENT_SECRET` | to offer Notion | — | Same connection |
| `NOTION_OAUTH_REDIRECT_URL` | to offer Notion | — | Must match a redirect URI registered on the connection exactly |
| `NOTION_VERSION` | | `2026-03-11` | The `Notion-Version` header sent on every request. See [Notion API version](#notion-api-version) before changing it |

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

**Email ingestion** — a private capture address per user. All-or-nothing: setting some but not all of the first four refuses to boot, because a domain with no shared secret would mount an ingestion endpoint with nothing to authenticate against, and a secret with no Cloudflare credentials would hand users an address no mail can reach. Leave them all unset and the feature is simply not offered.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `INBOUND_EMAIL_DOMAIN` | for ingestion | — | The subdomain capture addresses live on, e.g. `in.zigadata.com`. Enable Cloudflare Email Routing on **this subdomain specifically** — it does not inherit the apex's config, and it adds MX records to the subdomain only, so the apex's own mail is untouched |
| `INGEST_SHARED_SECRET` | for ingestion | — | Keys the HMAC on `POST /api/ingest/email`. Must match `ZIGA_INGEST_SECRET` in the Worker and be ≥32 chars — a short one is a boot error, not a warning |
| `CLOUDFLARE_API_TOKEN` | for ingestion | — | Scoped token with Zone → Email Routing Rules → Edit. Used to create one routing rule per address (there is no catch-all on a subdomain) |
| `CLOUDFLARE_ZONE_ID` | for ingestion | — | The zone the inbound domain belongs to |
| `INGEST_WORKER_NAME` | | `ziga-email-ingest` | The Worker mail is routed to |
| `INGEST_DAILY_CAP` | | `50` | Per-user extractions per day from email. The primary bound on the model bill |
| `INGEST_BURST` | | `10` | Short-window burst allowance per user and per address |
| `INGEST_MAX_ADDRESSES` | | `180` | How many capture addresses may exist at once. Kept under Cloudflare's 200-rules-per-domain cap so provisioning fails with our message rather than theirs |

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

## Setting up Notion

Optional. With the `NOTION_OAUTH_*` variables unset, Notion is simply not offered and every `/api/notion/*` route returns 404.

1. In the [Notion developer portal](https://app.notion.com/developers/connections), create a **public connection**. Notion renamed these from "integrations" — the OAuth credentials live on the **Configuration** tab. Only a *public* connection speaks OAuth; an *internal* connection uses a static token and cannot serve other people's workspaces.
2. Set the **installation scope to "Any workspace"**. This is fixed at creation and cannot be changed afterwards, and "selected workspaces only" would stop other people's workspaces from connecting at all.
3. Register the redirect URI on the connection — it must match `NOTION_OAUTH_REDIRECT_URL` exactly, including scheme and port.
4. Grant exactly these capabilities, which are what the code actually uses:

   | Capability | Needed for |
   |---|---|
   | **Read content** | `POST /search` (the granted databases and pages), `GET /databases/{id}` → data source, `GET /data_sources/{id}` → the property schema, `POST /data_sources/{id}/query` (preview strip) |
   | **Insert content** | `POST /pages` (writing a lead), `POST /databases` (auto-creating "Ziga Leads") |
   | **Update content** | `PATCH /data_sources/{id}` — only ever used to add a missing `select` option before retrying a write. Without it, a lead whose `source` value the property has not seen fails instead of being repaired |
   | **No user information** | The app never calls `/users`. Workspace identity comes from the token response, so there is no reason to ask for user data |

   Comment capabilities are not needed.
5. Set `NOTION_OAUTH_CLIENT_ID`, `NOTION_OAUTH_CLIENT_SECRET`, `NOTION_OAUTH_REDIRECT_URL`, and `TOKEN_ENCRYPTION_KEY`.

> **A public connection must be submitted for review before its OAuth flow goes live.** Notion populates the connection's Authorization URL only after submission, so plan for that lead time before a public launch — and see [Verifying against a real workspace](#verifying-against-a-real-workspace) for what can be tested before then.

**All three or none.** Setting some but not others is a **boot error** that names the missing ones — the same guard as `ZIGA_DEV_MODE` for Google, so a typo can never produce a running process that offers a "Connect Notion" button which then dies at the callback.

### What a Notion user then does

The user creates nothing in Notion — no connection, no API key. They only grant access to pages they already have.

1. Picks Notion as their destination in Ziga and clicks **Connect Notion**.
2. On Notion's screen: approves the capabilities, then uses the **page picker** to select which pages and databases to share. Notion only lists resources the user has *full access* to, so a page shared with them at comment- or read-level will not appear.
3. **What they pick determines their options in the next step:**
   - shared at least one **page** → Ziga can create a "Ziga Leads" database inside it
   - shared at least one **database** → they can use that database instead
   - shared only databases → the create option is unavailable, and Ziga says so rather than failing
   - shared nothing usable → Ziga tells them to reconnect and grant something
4. Back in Ziga: either lets it **create a "Ziga Leads" database** (the safe default — the app owns the schema, so every field has a home of the right type and nothing can be dropped), or **picks an existing database** and reviews the **field→property mapping** before saving.

Access is revocable from the user's side at any time, in Notion's own settings. Ziga detects the revocation on the next write and prompts a reconnect rather than failing silently.

### Verifying against a real workspace

A public connection must be submitted for review before Notion serves its OAuth consent screen, which gates the full end-to-end check. What can be verified before that:

| Check | Needs review? |
|---|---|
| Boot guards — partial config exits naming the missing vars; no config boots with Notion unoffered | no |
| The whole UI flow — connect, create/pick, mapping, dropped fields, reconnect — via `?mock=1` | no |
| Whether `api.notion.com/v1/oauth/authorize` accepts the client id — hit the consent URL and see whether it forwards to `app.notion.com/install-integration` (accepted) or rejects the client | no — one request |
| Token exchange, schema fetch, page create, select-option creation, revoked-access handling | **yes** |

The last row is the one that validates this build's assumptions about Notion's response shapes, since every test runs against fakes built from the documentation.

Checked on 2026-08-06 against a freshly created, **unsubmitted** connection: the authorize leg is already live. `GET /v1/oauth/authorize` returned `302` to `app.notion.com/install-integration` with `state` and `owner=user` preserved, and an unauthenticated visit lands on Notion's login page rather than an error — so a valid client id resolves before review. That does not prove the post-login consent screen renders for an unsubmitted connection, which is the next thing a real login settles.

### Why there is a mapping step

A Google Sheet is rows and columns: every schema field has a cell, always. A Notion database is pages with *typed* properties, and a user's existing database was not built for Ziga. So on connect Ziga fetches the database's schema and proposes a mapping — exact name match, then case-insensitive, then type affinity — and shows it for review rather than applying it silently.

- **Property names are case-sensitive.** `Name` and `name` are different properties; the exact casing round-trips untouched from the schema into the stored mapping.
- **Values are coerced to the target property's type.** The title property takes the lead name; an `email` property takes the contact only when it really is an email address (a phone number or `@handle` would be rejected by Notion); a `date` property takes the ISO date.
- **A field with no home is dropped, not fatal.** The page is created with everything that maps, and the response names what was dropped so the review pane can say so. Never a silent loss.
- **`select` options are created on demand.** If `source` carries a value the property has not seen, the write is retried once after adding the option to the schema.
- **`status` properties are never mapped.** The API cannot add options to them, so an unseen value would be unwritable.

The mapping is re-validated against the live schema when it is saved, so a database edited between the mapping screen and the save cannot produce a destination that fails on the first real lead.

### Notion API version

Notion pins behavior to a dated `Notion-Version` header sent on **every** request. It lives once in config (`NOTION_VERSION`, default `2026-03-11`) rather than at call sites.

This build targets the **data-source model** introduced in `2025-09-03`: a database is a parent of one or more data sources, the property schema lives on the data source, and pages are created with a `data_source_id` parent. Do not set this back to `2022-06-28` — Notion documents that version as failing outright on databases with more than one data source, which would break lead writes for a user who merely restructured their database.

Notion's rate limit is roughly **3 requests per second per connection install**, enforced client-side by a limiter keyed per workspace, with retry-on-429 honoring `Retry-After`.

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
| `GET /api/me` | Current user, destination state (`destination_configured` gates onboarding, `destination_connected` means writable right now), and which providers this server offers |

**Google**

| Endpoint | Description |
|---|---|
| `GET /api/auth/google/start` | Begins the OAuth flow with an anti-forgery state cookie |
| `GET /api/auth/google/callback` | Exchanges the code, stores encrypted tokens, starts the session. Links to an existing account only when that account's email is already verified; an unverified match is refused rather than silently linked |
| `POST /api/auth/google/disconnect` 🔒 | Drops the Google link and its stored tokens |
| `POST /api/sheets/create` 🔒 | Creates a "Ziga Leads" spreadsheet under the user's account and records it as their destination |
| `POST /api/sheets/attach` 🔒 | `{spreadsheet_id}` from the Picker. Records an existing spreadsheet, appending to its first tab |

**Notion** (all 🔒; all `404` when Notion is not configured)

| Endpoint | Description |
|---|---|
| `GET /api/notion/start` | Begins the OAuth flow with an anti-forgery state cookie. Connect-only, never a sign-in, so it requires an existing session |
| `GET /api/notion/callback` | Exchanges the code and stores the encrypted workspace token, then redirects into the database-choice step |
| `POST /api/notion/disconnect` | Drops the Notion link and its stored token; marks a Notion destination broken |
| `GET /api/notion/resources` | The databases and pages the user granted during consent, plus `can_create` (false when no page was shared, so there is nowhere to create a database) |
| `GET /api/notion/databases/{id}/mapping` | The proposed field→property mapping, every property offered as an alternative, and the fields this database has no home for |
| `POST /api/notion/databases/create` | `{parent_page_id}`. Creates a "Ziga Leads" database with a known-correct schema and records it as the destination |
| `POST /api/notion/destination` | `{database_id, mapping}`. Re-validates the mapping against the live schema, then records the database as the destination. `422` if the mapping no longer fits |

**Submissions** (all 🔒)

| Endpoint | Description |
|---|---|
| `POST /api/submit` | multipart form: `text` and/or `image`. Extract-only — stores a `pending` submission and returns `{id, status, result, field_states, flags, input, created_at}`. Writes nothing to the sheet |
| `POST /api/submissions/{id}/confirm` | body `{"fields": {name: value, ...}}` with the reviewed values. The only path that writes a lead. Returns `dropped_fields` when the destination had no home for some of them, and `url` when it links to the written row/page. Accepts `pending` and `failed_write` (retry = same call); `422` if a required field is still empty; `409` once written, if discarded, if no destination is connected yet, or if the grant needs reconnecting |
| `POST /api/submissions/{id}/discard` | soft-delete a pending/failed submission: row retained as `discarded`, same-day dedup hash freed. Idempotent; discarded items leave the queue and history |
| `GET /api/submissions/{id}/image` | the original uploaded image |
| `GET /api/queue` | pending + failed submissions, newest 100, with `count` for the badge |
| `GET /api/preview` | last 3 leads at the user's destination, in schema column order. Degrades to an empty strip rather than erroring when nothing is connected or the grant is broken |
| `GET /api/destination` | the user's active destination plus the alternatives, for the picker. An alternative is marked `unavailable` when this server has no connection configured for it |
| `GET /api/history` | last 50 written submissions |

**Email ingestion** (mounted only when ingestion is configured; otherwise these routes do not exist).

| Endpoint | Purpose |
|---|---|
| `POST /api/ingest/email` | The mail Worker's delivery. **No session and no CSRF** — see below |
| `GET /api/inbound` | The user's capture address, or `enabled:false` before opt-in |
| `POST /api/inbound/enable` | Provision the address and its routing rule |
| `POST /api/inbound/rotate` | Issue a new address; the old one keeps routing for a two-week grace period |
| `GET /api/quarantine` | Filtered mail. `?status=verification` narrows to pending forwarding handshakes |
| `POST /api/quarantine/{id}/rescue` | Re-extract a filtered message into the review queue |
| `POST /api/quarantine/{id}/dismiss` | Close it without extracting |
| `GET/POST /api/senders/blocked`, `/block`, `/unblock` | Per-user sender blocklist |
| `POST /api/queue/seen` | Resets the "captured while you were away" count |

`POST /api/ingest/email` is registered **bare on the mux — deliberately, and tested**. Its caller is a mail Worker, not a browser: there is no session to require and no cookie for a CSRF token to defend. It authenticates with an HMAC-SHA256 signature over the exact request body plus a timestamp header, constant-time compared, with a five-minute skew window.

Its rate limit is keyed by **inbound address and user id, never by IP**. All ingestion traffic arrives from one upstream, so a per-IP budget would be a single bucket shared by every tenant — and `clientIP` trusts `X-Forwarded-For`, which is *also a real email header*, so the Worker sends message headers only inside the JSON body. The address-keyed limit is applied before the database lookup, so probing for live addresses is throttled at the same rate as real mail.

Unknown, retired, unprovisioned and wrong-domain recipients all return a byte-identical `202` and write nothing, so none of them is distinguishable from outside.

`GET /healthz` is the unauthenticated liveness probe.

`status` is `pending` / `written` / `failed_write` / `discarded`. `/api/submit` (LLM cost) and `/api/submissions/{id}/confirm` (destination API quota) share one per-IP rate-limit budget (`RATE_LIMIT_PER_MIN`).

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

Covers the per-field confidence matrix, date defaulting, JSON-schema shape, dedup/store behavior including the legacy-database migrations, and the confirm/retry/discard handler paths. No test touches the network: Google and Notion are both `httptest` fakes.

On the multi-tenant side: `internal/httpapi/auth_test.go` (signup, verification, login, reset), `internal/httpapi/oauth_test.go` (the callback and the account-linking rules), `internal/httpapi/sheets_test.go` (create and attach), `internal/oauth/oauth_test.go` (which asserts the broad `spreadsheets` scope is never requested), and `internal/httpapi/isolation_test.go`, which walks every user-scoped endpoint and fails if one account can reach another's data.

The migration guarantees are load-bearing and tested as such:

- `TestLegacySheetUserWritesAfterMigration` seeds a database as the pre-generalization build left it (a `user_sheets` row, no `destinations` table), boots the server on it, signs in, and confirms a lead — asserting it lands on the *original* spreadsheet with no reconnect. Verified to fail when the backfill is removed.
- `TestMigrateOAuthAccountsRenamesGoogleSub` opens a database written when the column was `google_sub` and asserts existing links still resolve. Losing this would sign every Google user out.
- `TestBackfillIsIdempotentAndDoesNotClobber` — the backfill runs on every boot, so it must never resurrect a stale legacy row over a destination the user has since changed.

On the Notion side: `internal/notionauth` (the consent URL shape, that no scope is ever requested, and that the client secret travels in the HTTP Basic header rather than the body), `internal/notion` (mapping across property types, case-sensitivity, partial writes with reported dropped fields, select-option creation, `Notion-Version` on every request, 429 backoff, and 404-as-permission), and `internal/httpapi/notion_oauth_test.go` / `notion_setup_test.go` (the connect flow, encrypted-token round trip, the mapping and destination endpoints, the revoked-access reconnect path, and Notion destination isolation).

On the email ingestion side, the pipeline is a pure package (no store, no LLM client, no `net/http`), so a **fixture corpus** is most of its test surface. `internal/ingest/testdata/corpus/` holds 25 realistic messages as `.eml` / `.json` / `.want.json` triples — see the README there for the contract, and `go test ./internal/ingest/ -run TestCorpus -update` to regenerate the goldens after a deliberate change. It is this repo's first `testdata/`.

The corpus is a **cross-language contract**. Go never parses the `.eml` (a second MIME parser would have its own bugs); instead `worker/email-ingest/test/parse.test.ts` reads the same files and asserts the Worker's payload builder reproduces the committed `.json`. Likewise `internal/ingest/hmac_test.go` and `worker/email-ingest/test/sign.test.ts` pin the *same* HMAC known-answer vector: the two implementations otherwise drift silently, and the failure is total — every lead stops arriving with nothing but 401s to show for it.

The properties that are easy to get wrong, and are therefore asserted directly:

- `TestExtractorSingleCallSite` parses this package's AST and fails if anything other than `ingestLead` calls the extractor. The filter pipeline is a cost control, and a cost control with a second door is not a control. Verified to fail on a planted call site.
- `TestAutoForwardTrustsTheFromHeader` — Gmail auto-forward *preserves* the original `From:` and adds `X-Forwarded-*` headers containing the **user's own** addresses. Reading the identity out of those headers would file every auto-forwarded lead under the user's own mailbox, and the rows would look entirely normal.
- `TestVerificationOutranksTheMachineFilter` asserts its own premise (that the machine filter really would catch a confirmation from `forwarding-noreply@google.com`) before asserting the ordering, so it cannot quietly stop testing anything.
- `TestFakeConfirmationIsNeverSurfacedAsVerification` — detection requires the real sender, never body text, or we would render an attacker's code and link inside our own UI.
- `TestPastePathUnchanged` pins the pasted-submission prompt byte for byte, so email work cannot regress the flow every existing user relies on.
- `TestIngestionDataIsolation` drives a real signed delivery and asserts the captured lead belongs to exactly one tenant and is invisible to the other.

For a live end-to-end check, run the server and try: a normal lead, one containing "ignore previous instructions…", a two-lead message, and a non-English lead. The frontend equivalent for ingestion is `?mock=1` — see below.

## Deployment

Production deployment to the Hetzner box is fully versioned under [`deploy/`](deploy/)
and driven by a copy-paste runbook — **[deploy/RUNBOOK.md](deploy/RUNBOOK.md)**.

The `deploy/` directory contains:

- **`ziga.service`** — hardened systemd unit (dedicated `ziga` user, resource
  caps, `ProtectSystem=strict`).
- **`nginx-ziga.conf`** — TLS-terminating reverse-proxy server block with an
  `/api/` rate limit and a 6 MB body ceiling (above the app's 5 MB image limit).
- **`backup.sh`** + **`ziga-backup.service`**/**`ziga-backup.timer`** (+
  **`ziga-backup.logrotate`**) — nightly journal-safe SQLite backup to its own R2
  bucket via rclone, on a systemd timer, with a 30-day retention prune and a
  `BACKUP_DRY_RUN` mode. Deliberately mirrors the co-hosted hookdrop app's backup
  layout — same script name and location, object naming, rclone flags, and log
  file — so the two are debugged the same way. The runbook's backup step includes
  a **mandatory restore test**.

Pushes to `main` build and deploy automatically via
[`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) (atomic binary
swap, health-check, and automatic rollback on failure). The runbook covers the
one-time server setup, the Cloudflare origin cert, the restricted CI deploy user,
and the required GitHub secrets.

**Backups.** The SQLite file at `DB_PATH` is the only persistent state — it holds
the dedup keys, the review queue, and history. The Google Sheet only holds
confirmed rows, so it is not a substitute for backing up the database. See the
runbook's backup + restore-test section.

> **Deployment note:** DNS for `app.zigadata.com` **is** flipped — the host
> resolves through Cloudflare and serves the app over HTTPS, so the SSH tunnel
> is no longer the only access path. What remains is not code:
>
> - the three GitHub Actions secrets (`DEPLOY_HOST`, `DEPLOY_USER`,
>   `DEPLOY_SSH_KEY`) so the deploy job can reach the box — until they exist,
>   every push to `main` fails at the SSH step and the running binary stays
>   whatever was last installed by hand. See RUNBOOK §g.
> - production configuration in `/opt/ziga/ziga.env`: a real OAuth client with
>   its redirect URI, `SESSION_SECRET`, `TOKEN_ENCRYPTION_KEY`, the
>   `NOTION_OAUTH_*` trio, and SMTP so verification mail is delivered rather
>   than logged. See RUNBOOK §h.

## Local staging access

`app.zigadata.com` serves the deployed app directly. The SSH tunnel below stays
useful for reaching it past Cloudflare and Nginx when isolating a fault. One
command opens a verified one:

```sh
make tunnel        # opens it, holds it open — Ctrl+C to close
make tunnel-down   # closes a tunnel left running from somewhere else
```

`make tunnel` clears the port, opens the forward, and then **proves** it reaches
the server: it clears a local `go run ./cmd/server` off `:8080` (only a dev build
of this app — a process it doesn't recognize is reported and the run aborts
rather than being force-killed), closes a stale tunnel, opens
`ssh -N -L 8080:localhost:8090`, and polls `/healthz` until the server answers
`ok`. If the app is down or ssh cannot connect or forward, it says so and exits
non-zero instead of leaving you at a URL that silently serves something else.

**It must stay running in its terminal.** The tunnel lives for as long as that
`make tunnel` process does — Ctrl+C, closing the window, or sleeping the machine
ends the session, and `localhost:8080` stops resolving to staging until you run
it again. Use a dedicated terminal tab for it.

The local port must stay **8080**: `APP_BASE_URL` and the redirect URI registered
on the Google OAuth client both say `http://localhost:8080`, and Google matches
the port exactly, so any other local port fails sign-in with
`redirect_uri_mismatch`.

The server host and deploy user are read from `deploy/tunnel.env`, which is
**untracked** (the `*.env` gitignore rule covers it) so this public repo carries
no real IP or username. Create it once:

```sh
printf 'TUNNEL_USER=%s\nTUNNEL_HOST=%s\n' <deploy user> <host> > deploy/tunnel.env
```

`make tunnel` prints this hint if the file is missing. Everything is overridable
per invocation — `make tunnel TUNNEL_HOST=1.2.3.4`, and `LOCAL_PORT` /
`REMOTE_PORT` likewise. See [RUNBOOK §f](deploy/RUNBOOK.md) for the manual
equivalent and why the ports differ.

## TODO

Deliberately out of scope for now:

- Queue navigation (prev/next between queued items; today the review pane auto-advances FIFO)
- Multi-lead extraction (splitting one paste into several rows; today only the primary lead is extracted and a banner is shown)
- History depth (pagination/search beyond the last 50 written submissions)

Deliberately out of scope for the Notion destination pass:

- **Multi-destination fan-out** — writing one lead to both Sheets *and* Notion. A user has exactly one active destination; switching replaces it. The destination model is a single row per user, so fan-out means a schema change, not just a loop.
- **Airtable / HubSpot** — the writer interface now has two implementations and adding a third is additive, but none is planned yet.
- **Attachments** — the submitted image is never uploaded to the destination, on either provider.
- **Notion `status` properties** — they are excluded from mapping and rejected if chosen manually, because the API cannot add options to them, so a lead with an unseen value would be unwritable.
- **Multiple data sources per Notion database** — a database with more than one data source writes to the first. Choosing among them is not offered.

Deferred from the email ingestion pass:

- **Attachments and image extraction** — the Worker sends attachment metadata but no bytes, so a lead that exists only inside a PDF or a photo is quarantined as `no_text` rather than read. The payload has room for content when this lands.
- **Auto-write of high-confidence leads** — the automation ladder comes later, as an opt-in per-user setting. Today every ingested lead waits for a human exactly like a pasted one.
- **Webhook / form ingestion** and a **Chrome extension** — other capture channels; `ingestLead` is already the shared path they would use.
- **Multiple inbound addresses per user** — one active address, with rotation.
- **Localised forward markers** — German/French/Italian Outlook (`Von:`, `De:`, `Da:`) are not parsed. Such a message falls through with low confidence, which raises a review-pane flag rather than silently attributing the lead to the forwarder.
- **Queue pagination under an email burst** — `/api/queue` returns the newest 100, so a large capture day could push older pasted leads off the review queue. The daily cap bounds this for now.

Deferred from the multi-tenant auth pass:

- **Billing / subscriptions** — no plans or payment yet; every account is free.
- **Team accounts** — one user = one tenant; no shared workspaces or member roles.
- **Marketing site** — the app is the only surface; `zigadata.com` marketing pages are separate. (In flight on the `marketing-site` branch — update this line when that PR merges.)
- **Google app-verification submission** — the code targets the `drive.file`
  scope (not the broad `spreadsheets` scope) so verification stays light; the
  formal submission happens before a public launch.

## Changelog

- 2026-08 — **Email ingestion**: a private capture address per user, a Cloudflare Email Worker, an HMAC-authenticated ingestion endpoint, a filter pipeline that runs before any model call, forwarded-email sender attribution, and a quarantine view that makes "never silently lost" checkable
- 2026-08 — **Notion** joins Google Sheets as a lead destination; the per-user destination model is generalized behind a writer interface, with existing Sheets users migrated in place
- 2026-07 — renamed to **Ziga Data** (formerly sheetdrop)
