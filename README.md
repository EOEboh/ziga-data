# Ziga Data

Paste unstructured lead info — text, a forwarded email, or a screenshot — and Ziga Data extracts it into structured fields (name, contact, source, need, date, notes), shows them in an editable review pane, and appends a row to **your own Google Sheet** when you confirm. Nothing is written until you confirm.

- **Backend**: Go, no framework
- **Frontend**: server-served static files; TypeScript compiled to one plain-JS bundle (`web/app.js`, committed), plain CSS with custom properties. No framework, no SPA.
- **LLM**: OpenAI `gpt-5.4-nano` via the Chat Completions API (text + vision, [structured outputs](https://platform.openai.com/docs/guides/structured-outputs) with `strict: true` guarantee schema-valid JSON). The client sits behind an interface (`internal/llm.Extractor`), so the provider/model can be swapped without touching the pipeline.
- **Destination**: Google Sheets API with a service account — you share your sheet with the service account's email; no OAuth flow.
- **Storage**: a single SQLite file for dedup keys, pending reviews, failed writes, and history. Raw originals (full pasted text, uploaded images) are purged `RETENTION_DAYS` (default 14) after a submission is confirmed or discarded — extraction results and the short excerpt stay. The cleanup runs at boot and daily.

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
git clone https://github.com/EOEboh/ziga && cd ziga
cp .env.example .env   # fill in values
go run ./cmd/server
```

> The GitHub repository rename to `ziga` is done manually outside this codebase; until it lands, clone from the old URL (GitHub redirects after a rename). If you have a local database under the old default name, rename it to `ziga.db` or point `DB_PATH` at it.

The server loads `.env` from the working directory automatically; variables already exported in your shell take precedence over the file.

Open http://localhost:8080 and paste a lead.

Without `SHEET_ID` / `GOOGLE_APPLICATION_CREDENTIALS` the server runs in **dry-run mode**: the destination becomes an in-memory sheet, so the full submit → review → confirm → preview flow works locally (rows are lost on restart). To exercise the failed-write UI in dry-run mode, put the literal `[fail]` in any field before confirming.

### Frontend development

The UI sources live in `ui/*.ts` and compile to the committed `web/app.js` (embedded into the binary via `go:embed`, so `go build` needs no Node):

```sh
npm install     # once: esbuild + typescript
make ui-check   # type-check
make ui         # rebuild web/app.js — commit the result
```

Open http://localhost:8080/?mock=1 to drive the UI against built-in fixtures (all confidence states, a failing confirm) with no backend calls.

### Environment variables

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `OPENAI_API_KEY` | ✅ | — | OpenAI API key (server-side only, never sent to the browser) |
| `GOOGLE_APPLICATION_CREDENTIALS` | for sheet writes | — | Path to the service-account JSON key |
| `SHEET_ID` | for sheet writes | — | Spreadsheet ID from the sheet URL |
| `SHEET_TAB` | | `Leads` | Worksheet tab to append to |
| `LLM_MODEL` | | `gpt-5.4-nano` | Any vision-capable OpenAI chat model |
| `PORT` | | `8080` | HTTP port |
| `DB_PATH` | | `./ziga.db` | SQLite file |
| `SCHEMA_PATH` | | `config/schema.json` | Extraction schema + column mapping |
| `RATE_LIMIT_PER_MIN` | | `10` | Per-IP submissions per minute (burst 5) |
| `RETENTION_DAYS` | | `14` | Days after confirm/discard before the raw original input (full text, image) is purged |

## Pointing it at a real Google Sheet

1. In [Google Cloud Console](https://console.cloud.google.com), create (or pick) a project and **enable the Google Sheets API**.
2. Create a **service account** (IAM & Admin → Service Accounts). No project roles are needed.
3. Create a **JSON key** for it and save the file next to the binary; point `GOOGLE_APPLICATION_CREDENTIALS` at it.
4. Create your sheet with a tab named `Leads` and this header row (must match `columns` in `config/schema.json`):

   | date | name | contact | source | need | notes | flags |
   |------|------|---------|--------|------|-------|-------|

5. **Share the sheet** (editor access) with the service account's email (`...@<project>.iam.gserviceaccount.com`).
6. Set `SHEET_ID` to the ID from the sheet URL and restart.

The service account only ever touches sheets explicitly shared with it — the app requests the `spreadsheets` scope only, no Drive access.

## API

| Endpoint | Description |
|---|---|
| `POST /api/submit` | multipart form: `text` and/or `image`. Extract-only — stores a `pending` submission and returns `{id, status, result, field_states, flags, input, created_at}`. Writes nothing to the sheet |
| `POST /api/submissions/{id}/confirm` | body `{"fields": {name: value, ...}}` with the reviewed values. The only path that appends a sheet row. Accepts `pending` and `failed_write` (retry = same call); `409` once written, `422` if a required field is still empty |
| `POST /api/submissions/{id}/discard` | soft-delete a pending/failed submission: row retained as `discarded`, same-day dedup hash freed. Idempotent; discarded items leave the queue and history |
| `GET /api/submissions/{id}/image` | the original uploaded image |
| `GET /api/queue` | pending + failed submissions, newest 100, with `count` for the badge |
| `GET /api/preview` | last 3 data rows of the connected sheet (assumes row 1 is the header) |
| `GET /api/destination` | connected destination for the picker |
| `GET /api/history` | last 50 written submissions |
| `GET /healthz` | liveness |

`status` is `pending` / `written` / `failed_write` / `discarded`. `/api/submit` (LLM cost) and `/api/submissions/{id}/confirm` (Google Sheets quota) share one per-IP rate-limit budget (`RATE_LIMIT_PER_MIN`).

Every request is logged as structured JSON (content hash — never raw content —, confidence, missing fields, status, duration).

## Customizing the schema

`config/schema.json` defines the extracted fields, which are required (gating), and the sheet column order. The prompt, JSON schema, validation, and sheet writer are all driven from it — per-user custom schemas later only need a config per user, not code changes.

## Tests

```sh
go test ./...
```

Covers the per-field confidence matrix, date defaulting, JSON-schema shape, dedup/store behavior including the legacy-database migration, and the confirm/retry/discard handler paths. For a live end-to-end check, run the server and try: a normal lead, one containing "ignore previous instructions…", a two-lead message, and a non-English lead.

## Deploying to a VPS (Hetzner)

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ziga ./cmd/server
scp ziga config/schema.json service-account.json you@your-vps:/opt/ziga/
```

`/etc/systemd/system/ziga.service`:

```ini
[Unit]
Description=Ziga Data
After=network.target

[Service]
WorkingDirectory=/opt/ziga
ExecStart=/opt/ziga/ziga
Environment=OPENAI_API_KEY=sk-...
Environment=GOOGLE_APPLICATION_CREDENTIALS=/opt/ziga/service-account.json
Environment=SHEET_ID=...
Environment=SCHEMA_PATH=/opt/ziga/schema.json
Environment=DB_PATH=/opt/ziga/ziga.db
Restart=on-failure
User=ziga

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now ziga
```

Put it behind your existing nginx/caddy with TLS; the rate limiter reads `X-Forwarded-For`, so forwarding that header from the proxy is enough.

**Backups.** Back up the SQLite file at `DB_PATH` (default `./ziga.db`; the resolved absolute path is logged at boot as `sqlite store open`) — it holds the dedup keys, the review queue, and history. The Google Sheet only holds confirmed rows, so it is not a substitute for backing up the database.

## TODO

Deliberately out of scope for now:

- Queue navigation (prev/next between queued items; today the review pane auto-advances FIFO)
- Multi-lead extraction (splitting one paste into several rows; today only the primary lead is extracted and a banner is shown)
- History depth (pagination/search beyond the last 50 written submissions)

## Changelog

- 2026-07 — renamed to **Ziga Data** (formerly sheetdrop)
