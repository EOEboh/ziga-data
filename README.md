# SheetDrop

Paste unstructured lead info — text, a forwarded email, or a screenshot — and SheetDrop extracts it into structured fields (name, contact, source, need, date, notes) and appends a row to **your own Google Sheet**.

- **Backend**: Go, no framework
- **LLM**: OpenAI `gpt-5.4-nano` via the Chat Completions API (text + vision, [structured outputs](https://platform.openai.com/docs/guides/structured-outputs) with `strict: true` guarantee schema-valid JSON). The client sits behind an interface (`internal/llm.Extractor`), so the provider/model can be swapped without touching the pipeline.
- **Destination**: Google Sheets API with a service account — you share your sheet with the service account's email; no OAuth flow.
- **Storage**: a single SQLite file for dedup keys, the needs-review queue, and failed writes.

## How a submission flows

1. `POST /api/submit` (text and/or image, ≤5 MB png/jpeg/webp/gif)
2. Dedup check: SHA-256 of content + day bucket — an identical re-submit the same day returns the prior result, no second row, no second LLM call
3. The LLM extracts the fields under a strict JSON schema; the system prompt treats submitted content as **data only** (prompt-injection defense), handles any input language, and reports low confidence instead of guessing on bad images
4. Gate: `confidence == "low"` or a missing required field (`contact`, `need`) → saved to the **needs-review queue**, not written to the sheet
5. Otherwise the row is appended to your sheet (3 attempts, exponential backoff); a terminal failure lands in the **failed-writes queue** with a clear error
6. If multiple leads are detected in one submission, only the primary one is extracted and the row/response is flagged

## Local setup

Requirements: Go 1.22+.

```sh
git clone https://github.com/EOEboh/sheetdrop && cd sheetdrop
cp .env.example .env   # fill in values
go run ./cmd/server
```

The server loads `.env` from the working directory automatically; variables already exported in your shell take precedence over the file.

Open http://localhost:8080 and paste a lead.

Without `SHEET_ID` / `GOOGLE_APPLICATION_CREDENTIALS` the server runs in **dry-run mode**: extraction, gating, and dedup all work; the row is logged instead of written. Handy for testing the LLM path first.

### Environment variables

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `OPENAI_API_KEY` | ✅ | — | OpenAI API key (server-side only, never sent to the browser) |
| `GOOGLE_APPLICATION_CREDENTIALS` | for sheet writes | — | Path to the service-account JSON key |
| `SHEET_ID` | for sheet writes | — | Spreadsheet ID from the sheet URL |
| `SHEET_TAB` | | `Leads` | Worksheet tab to append to |
| `LLM_MODEL` | | `gpt-5.4-nano` | Any vision-capable OpenAI chat model |
| `PORT` | | `8080` | HTTP port |
| `DB_PATH` | | `./sheetdrop.db` | SQLite file |
| `SCHEMA_PATH` | | `config/schema.json` | Extraction schema + column mapping |
| `RATE_LIMIT_PER_MIN` | | `10` | Per-IP submissions per minute (burst 5) |

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
| `POST /api/submit` | multipart form: `text` and/or `image`. Returns `{status, duplicate, result, flags, message}` where `status` is `written` / `needs_review` / `failed_write` |
| `GET /api/review` | needs-review queue (newest 100) |
| `GET /api/failed` | failed-writes queue |
| `GET /healthz` | liveness |

Every request is logged as structured JSON (content hash — never raw content —, confidence, missing fields, status, duration).

## Customizing the schema

`config/schema.json` defines the extracted fields, which are required (gating), and the sheet column order. The prompt, JSON schema, validation, and sheet writer are all driven from it — per-user custom schemas later only need a config per user, not code changes.

## Tests

```sh
go test ./...
```

Covers the confidence-gating matrix, date defaulting, JSON-schema shape, and dedup/store behavior. For a live end-to-end check, run the server and try: a normal lead, one containing "ignore previous instructions…", a two-lead message, and a non-English lead.

## Deploying to a VPS (Hetzner)

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o sheetdrop ./cmd/server
scp sheetdrop config/schema.json service-account.json you@your-vps:/opt/sheetdrop/
```

`/etc/systemd/system/sheetdrop.service`:

```ini
[Unit]
Description=SheetDrop
After=network.target

[Service]
WorkingDirectory=/opt/sheetdrop
ExecStart=/opt/sheetdrop/sheetdrop
Environment=OPENAI_API_KEY=sk-...
Environment=GOOGLE_APPLICATION_CREDENTIALS=/opt/sheetdrop/service-account.json
Environment=SHEET_ID=...
Environment=SCHEMA_PATH=/opt/sheetdrop/schema.json
Environment=DB_PATH=/opt/sheetdrop/sheetdrop.db
Restart=on-failure
User=sheetdrop

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now sheetdrop
```

Put it behind your existing nginx/caddy with TLS; the rate limiter reads `X-Forwarded-For`, so forwarding that header from the proxy is enough.
