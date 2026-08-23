# ziga-email-ingest

The Cloudflare Email Worker that captures inbound mail and hands it to Ziga.

It is deliberately thin: parse MIME, sign, POST. Every judgement about whether
a message is a lead — filtering, forwarded-sender attribution, deduplication,
rate limiting — lives in Ziga, where it is testable against a fixture corpus
and visible to the user when it goes wrong.

## Mail path

```
sender → lead-<random>@in.zigadata.com
       → Cloudflare Email Routing (per-address rule)
       → this Worker
       → HTTPS POST (HMAC-signed) → Ziga → filter → extract → review queue
```

**There is no catch-all.** Cloudflare supports catch-all only at a zone apex,
never on a subdomain, and capture addresses live on a subdomain precisely so
enabling mail routing does not rewrite the apex's MX records. Ziga therefore
creates one literal-match routing rule per address through the Cloudflare API
(`internal/cfroute`). Rules are capped at 200 per domain — see
`INGEST_MAX_ADDRESSES` in `.env.example`.

## Deploying

```sh
npm ci
npm test
npx wrangler login                           # interactive; opens a browser
npx wrangler deploy
npx wrangler secret put ZIGA_INGEST_SECRET   # must equal INGEST_SHARED_SECRET
npx wrangler secret put FALLBACK_ADDRESS     # a VERIFIED destination address
```

Secrets go in **after** the first deploy — there is nothing to attach them to
before that. They survive later redeploys, unlike `vars`, which are replaced
from `wrangler.jsonc` every time.

`ZIGA_INGEST_SECRET` is a **secret**, never a var, and never committed. If it
drifts from `INGEST_SHARED_SECRET` in `/opt/ziga/ziga.env`, every delivery 401s
and leads stop arriving with no other symptom — check the app log for 401s
first when triaging.

`FALLBACK_ADDRESS` is a **secret, not a var** — it is a real person's mailbox
and this repository is public, so a var would put it in git history forever. It
must be a **verified destination address** on the account: `forward()` throws
on an unverified one, which turns the safety net into a second failure.

Full setup, including enabling Email Routing on the subdomain, is in
`deploy/RUNBOOK.md` §k.

## Never losing a lead

Cloudflare does not retry the `email()` handler and has no queue behind it, so
an unhandled exception loses the message outright. The Worker is arranged so
that cannot happen:

| Situation | What happens | Why |
|---|---|---|
| Message over `MAX_RAW_BYTES` | Reported to Ziga as a `size_rejected` quarantine event, headers only | Usually a real lead with a large attachment. `setReject()` is a **permanent** SMTP error and would bounce it back to them. |
| MIME parse fails | Reported as `parse_failed`, headers only | A client we mishandle is still a real message. |
| Ziga returns 5xx or the network fails | 3 attempts, 250ms/1s/4s backoff with jitter | Transient. The lead is fine. |
| Ziga returns 4xx | No retry | A 401 will not fix itself; retrying triples the log noise and delays the fallback. |
| Retries exhausted | `forward()` to `FALLBACK_ADDRESS` with `X-Ziga-*` headers | Preserves the mail in a real inbox for manual replay. |
| No fallback configured | Loud `console.error` | Throwing would not preserve the mail either — Cloudflare does not retry — so it would only make the log less legible. |

## Tests

```sh
npm test
```

`test/sign.test.ts` asserts the **same known-answer HMAC vector** as
`internal/ingest/hmac_test.go`. Two implementations of one signing scheme drift
silently, and the failure is total, so both sides pin the same vector and fail
together on a deliberate change.

`test/parse.test.ts` reads the **shared corpus** at
`internal/ingest/testdata/corpus` and asserts this Worker's payload builder
reproduces each committed `.json` from the matching `.eml`. Go never parses the
`.eml` — a second MIME parser would have its own bugs — so this is what stops
the two sides drifting. See that directory's README for the triple contract.

Adding a fixture that the Worker cannot reproduce fails here rather than
quietly changing what gets filtered in production.

## Not in this version

Attachment bytes. `attachments` carries metadata only, because attachment and
image extraction are out of scope — but their presence is what distinguishes "a
lead with a logo attached" from "a scan and no text", which the Go filters act
on. The payload has room for content when that lands.
