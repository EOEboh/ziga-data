# zigadata.com — marketing site

The static site served at the root domain. Two jobs: satisfy Google OAuth
verification (reviewers visit these pages by hand) and send visitors to
`app.zigadata.com/signup`.

Plain HTML and CSS. No framework, no build step, no dependencies, no
JavaScript, no third-party requests. Deploying is "upload this folder".

```
site/
  index.html      the one-pager
  privacy.html    served at /privacy
  terms.html      served at /terms
  styles.css      the only stylesheet
  robots.txt
  assets/
    favicon.svg
```

## Local preview

```sh
python3 -m http.server 4321 --directory site
```

Then open <http://localhost:4321>. Locally the legal pages are at
`/privacy.html` and `/terms.html`; on Cloudflare Pages the `.html` is dropped
automatically, which is why the links in the markup point at `/privacy` and
`/terms`. Check those on a Pages preview deployment, not locally.

## Deploy to Cloudflare Pages

One-time setup, using the Git integration so every push to `main` publishes:

1. Cloudflare dashboard → **Workers & Pages** → **Create** → **Pages** →
   **Connect to Git**, and pick the `EOEboh/ziga-data` repository.
2. Build settings:
   - Framework preset: **None**
   - Build command: **leave empty**
   - Build output directory: **`site`**
   - Production branch: **`main`**
3. Save and deploy. You get a `*.pages.dev` URL to check first.
4. **Custom domains** → add `zigadata.com`, and add `www.zigadata.com` if you
   want the redirect. Cloudflare creates the DNS records itself when the domain
   is already on your account.

`app.zigadata.com` is unrelated to this project. It points at the Hetzner box
that serves the Go binary and is not touched by any of the above.

Pushes that only change `site/` are excluded from `.github/workflows/deploy.yml`
via `paths-ignore`, so editing marketing copy does not run the Go test suite or
ship a new binary. Cloudflare Pages watches the repository independently and is
unaffected by that filter.

### Deploying by hand instead

```sh
npx wrangler pages deploy site --project-name=zigadata
```

## Before going live

- [ ] **Demo asset.** See below.
- [ ] **Privacy policy.** Review the marked block in `privacy.html`.
- [ ] **Terms.** Review the marked block in `terms.html`, and fill the
      `[GOVERNING LAW / JURISDICTION]` placeholder. It is the one thing in there
      that cannot be inferred.
- [ ] **Email.** `hello@zigadata.com` is printed on all three pages and is the
      contact address in both legal documents, so it has to actually receive
      mail before you submit for Google verification. Cloudflare → your domain →
      **Email** → **Email Routing** → forward it to a real inbox. It is free.
- [ ] **Favicon.** `assets/favicon.svg` is a placeholder mark in the brand green.
      Replace it with the same logo you uploaded to the Google OAuth consent
      screen so the branding matches.
- [ ] **OG image.** Optional. Drop a 1200x630 PNG at `assets/og.png` and
      uncomment the `og:image` tag in `index.html`.

## Where the demo asset goes

The hero currently renders a dashed placeholder box rather than an `<img>`
pointing at a file that does not exist, so the live site never shows a broken
image.

1. Put the real file in `site/assets/` (for example `demo.png` or `demo.mp4`).
2. In `index.html`, find the block marked `DEMO ASSET SLOT`. The comment above
   it holds ready-to-paste `<img>` and `<video>` replacements. Replace the whole
   `<div class="demo__frame">…</div>` with one of them.

Keep the `<figcaption>` as is. "From pasted text to a saved row in seconds." is
part of the approved copy.

An image works better than a clip here: it is smaller, it needs no controls, and
it is legible in the first paint. If you use a video, keep it short, muted, and
under about 2 MB.

## Google OAuth verification notes

The reviewer checks that the homepage accurately describes the app, that a
privacy policy is reachable from it, and that both live on the domain listed as
authorized in the Google Cloud console. Three things must stay true:

- The homepage says rows are written to a Google Sheet the user **creates or
  selects**. That matches the `drive.file` scope the app requests
  (`internal/oauth/oauth.go`), which is per file rather than whole-Drive.
- The footer link to `/privacy` is a real, visible anchor on the homepage.
- The wordmark reads **Ziga Data**, matching the OAuth consent screen name.

If the requested scopes ever change, update the "What we access in your Google
account" section of `privacy.html` in the same commit. A mismatch between the
consent screen and the policy is the usual reason verification bounces.

## House style for the copy

The page copy is final and in the owner's voice. If you edit it: no em dashes,
and none of "leverage", "seamless", "unlock", "robust", or "empower".

## Design tokens

`styles.css` mirrors the app's visual system (`web/src/styles.css`) so the brand
does not shift when a visitor crosses to `app.zigadata.com`. The colors, radii,
font stacks, and shadow are copied verbatim under the same names. If you change
a token in the app, change it here too.

Two deliberate differences:

- **Base font size is 16px**, not the app's 14px. The app is a dense tool; this
  is marketing prose.
- **The semantic amber and red tokens are absent.** In the app they mean
  extraction confidence states and must never be used decoratively, so the
  marketing site does not carry them at all.

Neither the site nor the app loads a webfont, so both render in the same
system-ui fallback. Do not add a Google Fonts link here: it would be a
third-party request on the page a privacy reviewer is reading.
