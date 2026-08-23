# ziga-data — Production Deployment Runbook

This runbook takes a fresh Ubuntu 24 box (the existing Hetzner CPX22 that already
runs hookdrop) to a running **staging** deployment of ziga-data. Follow it
**top to bottom** — every step only depends on things created in earlier steps.

**Scope of this pass:** the box is deployed and DNS for `app.zigadata.com`
**is** flipped, so the app is reachable over HTTPS; the SSH tunnel (§f) remains
as a debugging path that bypasses Nginx. §h records what the flip involved.
What is still outstanding is listed in §i — most importantly the CI deploy
secrets, without which pushes to `main` never reach the box.

All commands run as a sudo-capable admin user unless a step says otherwise.
Replace every `<PLACEHOLDER>` with a real value — this repo contains no real
secrets, IPs, or usernames.

Conventions used below:

| Placeholder | Meaning |
|-------------|---------|
| `<HOST>` | server hostname or IP |
| `<ADMIN>` | your sudo-capable admin login |
| `<DEPLOY_USER>` | restricted CI deploy user created in §g (e.g. `zigadeploy`) |
| `<PORT>` | app bind port on the server; this runbook uses `8090` to avoid hookdrop. The browser-facing port stays `8080` via the tunnel (§f) |

---

## a. System user, directory layout, and secrets

Create a dedicated, unprivileged, no-login system user and the `/opt/ziga` tree.

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ziga

sudo mkdir -p /opt/ziga/config
sudo chown -R ziga:ziga /opt/ziga
sudo chmod 750 /opt/ziga
```

**Install the schema file.** The binary reads `config/schema.json` from disk at
startup (relative to its working directory) — the binary alone will not boot
without it. Copy the repo's `config/schema.json` onto the box:

```bash
# from your workstation, in a clone of the repo:
scp config/schema.json <ADMIN>@<HOST>:/tmp/schema.json
# on the server:
sudo install -o ziga -g ziga -m 640 /tmp/schema.json /opt/ziga/config/schema.json && rm /tmp/schema.json
```

**Install `/opt/ziga/ziga.env`** (mode 600, owned by ziga). This is the complete
set of variables the app reads.

Do **not** hand-type it. `deploy/ziga.env` in your local clone is generated with
the real values already filled in (it is gitignored, so it exists only on your
workstation). Copy that file up:

```bash
# from your workstation, in a clone of the repo:
scp deploy/ziga.env <ADMIN>@<HOST>:/tmp/ziga.env
# on the server:
sudo install -o ziga -g ziga -m 600 /tmp/ziga.env /opt/ziga/ziga.env && rm /tmp/ziga.env
```

Two properties of that file matter and are easy to break if you edit it by hand:

- **The OAuth variables are named `GOOGLE_OAUTH_CLIENT_ID` /
  `GOOGLE_OAUTH_CLIENT_SECRET`**, not `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`.
  With the wrong names the app still boots and reports healthy, but Google
  sign-in, the Picker, and all per-user sheet writing are silently disabled.
- **systemd's `EnvironmentFile=` does not strip inline `#` comments.**
  `PORT=8090  # avoid hookdrop` sets the port to the entire string after the `=`
  and the service fails to bind. Keep every comment on its own line, and do not
  wrap values in quotes — systemd keeps the quotes literally.

The variables that are server-specific (already set correctly in the generated
file):

| Variable | Staging value | Why |
|---|---|---|
| `PORT` | `8090` | avoids a collision with hookdrop; the tunnel maps 8080 → 8090 |
| `DB_PATH` | `/opt/ziga/ziga.db` | the only writable path under `ProtectSystem=strict` |
| `SCHEMA_PATH` | `/opt/ziga/config/schema.json` | not embedded in the binary |
| `APP_BASE_URL` | `http://localhost:8080` | browser-facing origin through the tunnel |
| `OAUTH_REDIRECT_URL` | `http://localhost:8080/api/auth/google/callback` | must match the console exactly |
| `SMTP_*` | placeholders | see §a.1 — optional for this deploy |

> **There is no service-account JSON in this deployment.** Each user connects
> their own Google account and their own spreadsheet through OAuth, so
> `SHEET_ID` and `GOOGLE_APPLICATION_CREDENTIALS` are not set at all. If you see
> instructions to install `/opt/ziga/service-account.json`, they are from the
> pre-multi-tenant version of this runbook.

At this point `/opt/ziga` holds `config/schema.json` and `ziga.env`. The binary
comes next.

### a.1 Email is optional for this deploy

The `SMTP_*` lines ship **commented out**, and must stay that way until you have
real values. The log-instead-of-send fallback triggers only when `SMTP_HOST` is
genuinely empty — setting it to a literal `<SMTP_HOST>` placeholder counts as
"configured" and the app will try to reach a host by that name. Signup would
still return 201, but the verification link would be neither emailed nor logged,
leaving no way to verify the account short of reading the token out of SQLite.

With the lines commented out, the app logs verification and password-reset links
to the journal, so you can create and verify an account:

```bash
sudo journalctl -u ziga -f | grep -i 'email not sent'
```

Copy the link out of that log line and open it in the browser. When you do pick a
provider, note the mailer uses **STARTTLS on the submission port** (587). Port
465 (implicit TLS) is not supported.

---

### a.2 Notion is optional for this deploy

Notion is a second lead destination, offered as an alternative to Google Sheets.
The `NOTION_OAUTH_*` lines ship **commented out**. Unlike the SMTP block, this is
enforced rather than merely advised:

- **All three or none.** If any one of `NOTION_OAUTH_CLIENT_ID`,
  `NOTION_OAUTH_CLIENT_SECRET`, `NOTION_OAUTH_REDIRECT_URL` is set and another is
  not, the app **exits at boot** naming the missing ones. This is deliberate: a
  misnamed variable must never produce a running process that offers a "Connect
  Notion" button which then dies at the callback.
- **None set is fully supported.** Notion is simply not offered; the destination
  picker shows it as unavailable and every `/api/notion/*` route returns 404.
  This is the current state of the deploy.
- `TOKEN_ENCRYPTION_KEY` is **required** once Notion is configured. Notion access
  tokens are encrypted at rest with the same AES-256-GCM key as Google's.

To turn Notion on:

1. Create a **public connection** in the Notion developer portal at
   <https://app.notion.com/developers/connections>. Notion renamed these from
   "integrations"; the OAuth client id and secret live on the connection's
   **Configuration** tab. Only a *public* connection speaks OAuth — an
   *internal* connection uses a static API token and cannot serve other
   people's workspaces.

   Note that a public connection must be **submitted for review** before its
   OAuth authorization URL goes live, so allow lead time for that ahead of a
   public launch.
2. Register the redirect URI on the connection. It must match
   `NOTION_OAUTH_REDIRECT_URL` **exactly**, including scheme and port. For
   staging behind the SSH tunnel that is
   `http://localhost:8080/api/notion/callback`; in production it is
   `https://app.zigadata.com/api/notion/callback`.
3. Copy the OAuth client id and secret into `/opt/ziga/ziga.env`, uncommenting
   all three lines.
4. Restart and confirm the boot log:

```bash
sudo systemctl restart ziga
sudo journalctl -u ziga -n 30 | grep -i notion
# expect: "notion oauth enabled" with the notion_version and redirect
```

Users grant access **per resource**: on Notion's own consent screen they pick
exactly which pages and databases the connection may touch. The app never asks
for whole-workspace access — the same posture as `drive.file` on the Google side.

**About `NOTION_VERSION`.** Notion pins API behavior to a dated version header
sent on every request. The build default targets the current data-source model
(a database parents one or more data sources; the property schema lives on the
data source). Do not set this back to `2022-06-28`: Notion documents that
version as failing outright on databases with more than one data source, which
would break lead writes for a user who merely restructured their database.

---

## b. First manual deploy

**Build the Linux binary** on your workstation (pure-Go SQLite ⇒ no CGO needed;
`web/dist` is committed so no Node build is required here):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/ziga-linux-amd64 ./cmd/server
scp dist/ziga-linux-amd64 <ADMIN>@<HOST>:/tmp/ziga
```

On the server, place the binary and install the systemd unit:

```bash
sudo install -o ziga -g ziga -m 755 /tmp/ziga /opt/ziga/ziga && rm /tmp/ziga

# install the unit from the repo's deploy/ziga.service
sudo cp deploy/ziga.service /etc/systemd/system/ziga.service
sudo systemctl daemon-reload
sudo systemctl enable --now ziga
```

**Verify:**

```bash
systemctl status ziga --no-pager
journalctl -u ziga -n 50 --no-pager        # expect JSON logs
curl -fsS http://localhost:8090/healthz    # expect: ok
```

In the journal, the line that proves the multi-tenant configuration is live is:

```
{"level":"INFO","msg":"google oauth enabled","scopes":["openid", ... ,"drive.file"]}
```

If that line is **absent**, the process did not stay up. Unless `ZIGA_DEV_MODE`
is set, the app now refuses to boot when Google OAuth is not fully configured and
exits with a message naming the missing variable, e.g.:

```
{"level":"ERROR","msg":"config","err":"Google OAuth is required unless ZIGA_DEV_MODE=true; missing: GOOGLE_OAUTH_CLIENT_ID"}
```

So a misnamed or missing OAuth var is now a hard, loud boot failure
(`systemctl status ziga` shows the unit failed) rather than a "healthy" process
that silently drops every row. `ZIGA_DEV_MODE` must stay unset (or `false`) in
this deployment — it exists only for local no-Google development.

> **Not an error:** with OAuth configured you will **not** see the old
> `running in dry-run mode` warning — it now fires only when the in-memory
> fallback writer can actually be reached (dev mode with no OAuth). Its absence
> here is correct. Judge the deploy by the `google oauth enabled` line.

**Then verify the app end to end through the tunnel** (see §f for the tunnel
itself), because `/healthz` does not exercise any of the interesting paths:

```bash
# with the tunnel open, from your workstation:
curl -fsS http://localhost:8080/api/me | python3 -m json.tool
```

Expect `config.google_oauth: true` and non-empty `config.google_client_id` and
`config.google_picker_api_key`. Those two values are served to the browser at
runtime — the frontend does **not** bake them in at build time, so changing them
only requires editing `ziga.env` and restarting, never a rebuild.

---

## c. Cloudflare Origin certificate

The Nginx block (installed in §d) terminates TLS with a Cloudflare **Origin**
certificate. Create it now so §d has something to load.

1. Cloudflare dashboard → your `zigadata.com` zone → **SSL/TLS → Origin Server**
   → **Create Certificate**.
2. Leave "Generate private key and CSR with Cloudflare" selected; hostnames
   `zigadata.com` and `*.zigadata.com`; choose a validity (e.g. 15 years).
3. Copy the **Origin Certificate** (PEM) and the **Private Key**.
4. Place them on the server:

```bash
sudo mkdir -p /etc/ssl/cloudflare
sudo chmod 700 /etc/ssl/cloudflare
# paste the certificate:
sudo tee /etc/ssl/cloudflare/zigadata.pem >/dev/null   # then paste + Ctrl-D
# paste the private key:
sudo tee /etc/ssl/cloudflare/zigadata.key >/dev/null   # then paste + Ctrl-D
sudo chmod 600 /etc/ssl/cloudflare/zigadata.key
sudo chmod 644 /etc/ssl/cloudflare/zigadata.pem
```

> These paths match the placeholders in `deploy/nginx-ziga.conf`. The cert is an
> *origin* cert: it is only trusted by Cloudflare's edge, which is the live
> serving path now that DNS is flipped (§h).

---

## d. Nginx server block

```bash
sudo cp deploy/nginx-ziga.conf /etc/nginx/sites-available/ziga.conf
sudo ln -s /etc/nginx/sites-available/ziga.conf /etc/nginx/sites-enabled/ziga.conf
sudo nginx -t
sudo systemctl reload nginx
```

> **Note:** this block is live — `app.zigadata.com` resolves to this box
> through Cloudflare. The SSH tunnel in §f still works and bypasses Nginx
> entirely, which is the quickest way to tell an app fault from a proxy fault.

---

## e. Nightly backup + **mandatory restore test**

A `sqlite3 .backup` snapshot is gzipped and uploaded to **R2** via rclone nightly,
driven by a **systemd timer**. This mirrors hookdrop's setup on the same box
(`/opt/hookdrop/backup.sh` + `hookdrop-backup.service`/`.timer`) so that debugging
one teaches you the other — same script location and filename, same object-naming
style, same rclone flags, same `tail`-able log file.

Where ziga deliberately differs, and why — do not "fix" these:

| | hookdrop | ziga |
|---|---|---|
| Snapshot | `docker exec hookdrop sqlite3 …` | direct `sqlite3` (ziga is not containerised) |
| Bucket / token | `r2:hookdrop-backups/` | `r2ziga:ziga-backups/`, its own scoped token |
| Retention | 7 days | 30 days |
| Upload | raw `.db` (~15 MB/night) | gzipped `.db.gz` |
| Schedule | 03:00 | 02:30 (offset to avoid contending for the upload) |
| Script | `set -e`, no trap, no dry-run | `set -euo pipefail`, exit-preserving trap, `BACKUP_DRY_RUN` |
| Log rotation | none (log grows unbounded) | `/etc/logrotate.d/ziga-backup` |

> **Separate buckets, separate credentials:** hookdrop's R2 token is scoped to
> `hookdrop-backups` only. ziga gets its own bucket and its own scoped token, so
> neither app's prune can touch the other's objects and a leak of one credential
> cannot reach the other's backups.

> **Which "deploy"?** The backup runs as the box's existing `deploy` user (home
> `/home/deploy`), because that account owns the rclone config. That is **not**
> `<DEPLOY_USER>`, the restricted CI account in §g. Note that `deploy` does
> **not** have passwordless sudo here — every `sudo` below will prompt.

**1. Give `deploy` read access to the database.** The app runs as `ziga` and
`/opt/ziga` is not readable by `deploy` today (verify: `sudo -u deploy ls
/opt/ziga` currently fails). Add `deploy` to the `ziga` group and open
group-read:

```bash
sudo usermod -aG ziga deploy
sudo chmod 750 /opt/ziga
sudo chmod 640 /opt/ziga/ziga.db
sudo -u deploy test -r /opt/ziga/ziga.db && echo ok    # expect: ok
```

The group change only applies to new sessions, which is why the check runs
through a fresh `sudo -u`. Do not move the database or change its owner — the
app's systemd unit writes it as `ziga`.

> **If `sqlite3` later reports `attempt to write a readonly database`:** it hit a
> hot rollback journal and needed to recover it, which requires a writable file
> and directory. Widen to `sudo chmod 770 /opt/ziga` and
> `sudo chmod 660 /opt/ziga/ziga.db`.

**2. Create the bucket and a scoped token — in the Cloudflare dashboard.** This
cannot be done from the box: the existing `r2:` token is scoped to
`hookdrop-backups`, so it cannot list or create anything else (`rclone lsd r2:`
returns `403 AccessDenied`, and so does any bucket outside its scope).

1. R2 → **Create bucket** → name it `ziga-backups`, same location/class as
   `hookdrop-backups`.
2. R2 → **Manage R2 API Tokens** → **Create API token**:
   - Permission: **Object Read & Write**
   - Scope: **Apply to specific buckets only** → `ziga-backups`
3. Note the Access Key ID, Secret Access Key, and the S3 endpoint. Do **not**
   edit or re-scope hookdrop's existing token — leaving it untouched is the
   point.

**3. Add the `r2ziga:` remote.** It goes in the same config file as `r2:`, as the
`deploy` user:

```bash
sudo -u deploy rclone config
#   n) New remote
#   name> r2ziga
#   Storage> s3        →   provider> Cloudflare
#   access_key_id / secret_access_key / endpoint  →  from step 2
#   Edit advanced config> y  →  set  no_check_bucket> true   (matches [r2])
```

Verify — an empty listing that exits 0, **not** a 403:

```bash
sudo -u deploy rclone --config /home/deploy/.config/rclone/rclone.conf \
    lsf r2ziga:ziga-backups/ ; echo "rc=$?"      # expect: no output, rc=0
```

The credentials live only in that file. This repo references the config **path**
and never its contents — do not copy it into `/opt/ziga` or into git.

**4. Create the log file and install rotation.** The script appends to
`/var/log/ziga-backup.log` in addition to journald, the way hookdrop uses
`/opt/hookdrop/backup.log`:

```bash
sudo install -o deploy -g deploy -m 640 /dev/null /var/log/ziga-backup.log
sudo install -o root -g root -m 644 deploy/ziga-backup.logrotate /etc/logrotate.d/ziga-backup
sudo logrotate --debug /etc/logrotate.d/ziga-backup    # expect: no errors
```

**5. Install the script:**

```bash
sudo install -o deploy -g deploy -m 750 deploy/backup.sh /opt/ziga/backup.sh
```

**6. Install and enable the timer.** No editing required — the bucket, config
path, retention window, database path, and log file are all defaulted in the
script's header block; override any of them with `sudo systemctl edit
ziga-backup.service` and an `Environment=` line if they ever change.

```bash
sudo install -o root -g root -m 644 deploy/ziga-backup.service /etc/systemd/system/ziga-backup.service
sudo install -o root -g root -m 644 deploy/ziga-backup.timer   /etc/systemd/system/ziga-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now ziga-backup.timer
systemctl list-timers ziga-backup.timer    # expect: a NEXT elapse ~02:30 UTC
```

Enable the **timer**, not the service — enabling the service directly would run
a backup at every boot instead of on schedule.

**7. Run it once manually and confirm the object landed.** Dry run first (it
still snapshots and gzips locally, so it proves everything short of the upload):

```bash
sudo -u deploy env BACKUP_DRY_RUN=1 /opt/ziga/backup.sh
echo "rc=$?"                               # expect: rc=0

sudo systemctl start ziga-backup.service   # the real thing, through systemd
sudo -u deploy rclone --config /home/deploy/.config/rclone/rclone.conf \
    lsf r2ziga:ziga-backups/               # expect a ziga_YYYYMMDD_HHMMSS.db.gz
```

**8. RESTORE TEST — a backup is NOT done until a restore has been done.**
Download the object you just uploaded, decompress it, open it with sqlite3, and
count rows. If this fails, the backup is worthless — fix it before moving on.

```bash
cd /tmp
sudo -u deploy rclone --config /home/deploy/.config/rclone/rclone.conf \
    copy r2ziga:ziga-backups/ziga_<STAMP>.db.gz /tmp/
gunzip -k /tmp/ziga_<STAMP>.db.gz          # -> /tmp/ziga_<STAMP>.db
sqlite3 /tmp/ziga_<STAMP>.db '.tables'
sqlite3 /tmp/ziga_<STAMP>.db 'PRAGMA integrity_check;'             # expect: ok
sqlite3 /tmp/ziga_<STAMP>.db 'SELECT count(*) FROM submissions;'   # any real table
rm -f /tmp/ziga_<STAMP>.db /tmp/ziga_<STAMP>.db.gz
```

A clean `.tables` listing, `integrity_check` = `ok`, and a plausible row count
mean the pipeline is sound. Re-run this test after any change to the script, the
bucket, the token, or the rclone config — it is the only thing that proves the
backups are restorable.

**9. Check that it ran.** Two views, matching the two ways hookdrop is debugged:

```bash
systemctl list-timers ziga-backup.timer          # next + last elapse
systemctl status ziga-backup.service             # last run's result
journalctl -u ziga-backup.service --since '2 days ago'
tail -f /var/log/ziga-backup.log                 # as with hookdrop's backup.log
```

A healthy run ends with `backup complete: ziga_<STAMP>.db.gz` and
`Result: success`. A failure shows `Result: exit-code` — the script preserves the
real exit status through its cleanup trap specifically so that a failed upload or
an unreadable config surfaces here instead of looking like a success.

> **Logs:** the app itself logs JSON to stdout, captured by journald — there is
> **no** app log file. journald rotates on its own; cap disk use if desired via
> `SystemMaxUse=` in `/etc/systemd/journald.conf`, or vacuum with
> `sudo journalctl --vacuum-time=30d`. The backup's own
> `/var/log/ziga-backup.log` is rotated by the logrotate snippet from step 4.

---

## f. Daily staging access (SSH tunnel)

`app.zigadata.com` serves the app directly, but forwarding its local port over
SSH bypasses Cloudflare and Nginx, which isolates the app from the proxy layer
when debugging:

```bash
ssh -L 8080:localhost:8090 <DEPLOY_USER>@<HOST>
# leave that open, then browse:  http://localhost:8080
```

From a clone, `make tunnel` does the same thing with the port cleanup and a
`/healthz` check built in, so a tunnel that failed to bind cannot masquerade as a
working one — see the README's "Local staging access". It reads the host and user
from an untracked `deploy/tunnel.env`; the manual command above stays the
reference, since this runbook is placeholder-only by design.

Note the ports differ on purpose: the app binds **8090** on the server (avoiding
hookdrop), while the tunnel presents it on **8080** locally. The browser-facing
port must stay 8080 — `APP_BASE_URL` and the redirect URI registered on the
Google OAuth client both say `http://localhost:8080`, and Google matches the
redirect URI exactly, including the port. If you forward to a different local
port, Google sign-in fails with `redirect_uri_mismatch`.

`http://localhost` is exempt from Google's HTTPS requirement for redirect URIs,
which is what makes this tunnel workable before DNS and TLS exist.

Everything (UI + `/api/`) is served from the single app port. This path bypasses
Nginx and TLS entirely, which is expected for staging.

---

## g. Restricted CI deploy user (for the GitHub Actions workflow)

`.github/workflows/deploy.yml` deploys over SSH as a **non-root, restricted**
user whose only privilege is restarting the ziga unit. Create it once:

```bash
sudo useradd --system --create-home --shell /bin/bash <DEPLOY_USER>

# let the deploy user write the app binary into /opt/ziga:
sudo usermod -aG ziga <DEPLOY_USER>
sudo chmod 775 /opt/ziga          # group-writable so the deploy user can swap the binary

# install the deploy key (public half of the CI keypair, see below):
sudo -u <DEPLOY_USER> mkdir -p /home/<DEPLOY_USER>/.ssh
sudo -u <DEPLOY_USER> tee /home/<DEPLOY_USER>/.ssh/authorized_keys >/dev/null <<'EOF'
<PASTE_CI_DEPLOY_PUBLIC_KEY>
EOF
sudo -u <DEPLOY_USER> chmod 700 /home/<DEPLOY_USER>/.ssh
sudo -u <DEPLOY_USER> chmod 600 /home/<DEPLOY_USER>/.ssh/authorized_keys
```

**Scope sudo to exactly the restart** — nothing else, never root shell:

```bash
echo '<DEPLOY_USER> ALL=(root) NOPASSWD: /usr/bin/systemctl restart ziga' \
    | sudo tee /etc/sudoers.d/ziga-deploy
sudo chmod 440 /etc/sudoers.d/ziga-deploy
sudo visudo -c        # validate syntax
```

**Generate the CI keypair** (on your workstation; the private half becomes a
GitHub secret, the public half goes in `authorized_keys` above):

```bash
ssh-keygen -t ed25519 -f ziga_deploy_key -N '' -C 'ziga-ci-deploy'
# ziga_deploy_key.pub  -> paste into authorized_keys (above)
# ziga_deploy_key      -> paste into the DEPLOY_SSH_KEY secret (below)
```

**Set the three GitHub Actions secrets** (repo → Settings → Secrets and
variables → Actions):

| Secret | Value |
|--------|-------|
| `DEPLOY_HOST` | `<HOST>` |
| `DEPLOY_USER` | `<DEPLOY_USER>` |
| `DEPLOY_SSH_KEY` | contents of the `ziga_deploy_key` private key |

Once the secrets are set, pushes to `main` deploy automatically (see the workflow
and §g's rollback, which the workflow performs on a failed health check).

---

## h. Going live: the DNS flip

When `app.zigadata.com` is pointed at this box, the app moves off the localhost
tunnel and onto its public HTTPS origin. Edit `/opt/ziga/ziga.env` and change
exactly these two values:

| Variable | Staging (now) | Production (after flip) |
|----------|---------------|-------------------------|
| `APP_BASE_URL` | `http://localhost:8080` | `https://app.zigadata.com` |
| `OAUTH_REDIRECT_URL` | `http://localhost:8080/api/auth/google/callback` | `https://app.zigadata.com/api/auth/google/callback` |

Everything else in `ziga.env` (`PORT=8090`, DB path, keys) stays as-is. Then:

```bash
sudo systemctl restart ziga
```

Checklist:

- [ ] The Google OAuth client already has the **production redirect URI**
      `https://app.zigadata.com/api/auth/google/callback` **and** the **JS origin**
      `https://app.zigadata.com` registered. (Both are already added — verify, do
      not assume.) `OAUTH_REDIRECT_URL` must match the registered URI character for
      character, including scheme and no trailing slash.
- [ ] Nginx (§d) and the Cloudflare origin cert (§c) are installed and serving
      `app.zigadata.com` → `127.0.0.1:8090`.
- [ ] After the restart, cookies are set `Secure` automatically — the app derives
      that from the `https://` prefix of `APP_BASE_URL`, so no separate flag.
- [ ] Verification / reset **email links now point at the public origin**, so SMTP
      (§a.1) should be configured before or with the flip; otherwise links are only
      in the journal.
- [ ] The SSH tunnel (§f) is no longer the access path — browse `https://app.zigadata.com`.

Verify: `curl -fsS https://app.zigadata.com/api/me` returns `config.google_oauth:
true`, and a real Google sign-in completes without `redirect_uri_mismatch`.

---

## i. Deliberately NOT done in this pass

- **CI deploy secrets** — `DEPLOY_HOST`, `DEPLOY_USER`, and `DEPLOY_SSH_KEY`
  are **not set** on the repository, so the deploy job in
  `.github/workflows/deploy.yml` fails at the SSH step on every push to `main`
  (`can't connect without a private SSH key or password`). The binary on the
  box is therefore whatever was last installed by hand, not what `main`
  contains. Fixing this is §g.
- **SMTP** — no provider configured on the box; verification links are read
  from the journal (§a.1). Note the *domain* is now able to send (see the
  sending-vs-receiving note in `site/README.md`); what is missing is
  `SMTP_HOST`/`SMTP_USERNAME`/`SMTP_PASSWORD`/`SMTP_FROM` in
  `/opt/ziga/ziga.env`.
- **Nginx / TLS** — the server block in `deploy/nginx-ziga.conf` (upstream
  `127.0.0.1:8090`) and the Cloudflare origin cert are for the DNS flip (§h), not
  for staging.
- **Marketing site** — `zigadata.com` apex / marketing pages are a separate
  effort, unrelated to this app deployment.
- **Email ingestion** — the code ships, but nothing is configured on the box:
  `INBOUND_EMAIL_DOMAIN` is unset, so the ingestion endpoint is not mounted and
  the UI never offers a capture address. Turning it on is §k, and it also needs
  the `CLOUDFLARE_API_TOKEN` secret on the repository for the Worker's own
  deploy workflow.

---

## j. Server-state inventory (for drift audits)

Every file this setup places on the box, and why. Audit against this list to
detect drift.

| Path | Owner / mode | Purpose |
|------|--------------|---------|
| `/opt/ziga/ziga` | ziga 755 | the application binary (replaced on each deploy) |
| `/opt/ziga/ziga.prev` | ziga 755 | previous binary, kept for one-step rollback (§g / workflow) |
| `/opt/ziga/ziga.env` | ziga 600 | all runtime configuration (secrets) |
| `/opt/ziga/config/schema.json` | ziga 640 | extraction schema, read from disk at boot |
| `/opt/ziga/ziga.db` | ziga 640* | SQLite database — the only persistent state; group-readable so the backup can snapshot it (§e). Includes the email ingestion tables (`inbound_addresses`, `ingestion_events`, `blocked_senders`), so the nightly backup already covers them |
| `/opt/ziga/ziga.db-journal` | ziga | transient rollback journal (present only mid-write) |
| `/opt/ziga/backup.sh` | deploy 750 | nightly backup script (same name/position as `/opt/hookdrop/backup.sh`) |
| `/etc/systemd/system/ziga.service` | root 644 | systemd unit |
| `/etc/nginx/sites-available/ziga.conf` | root 644 | Nginx server block (+ symlink in sites-enabled) |
| `/etc/systemd/system/ziga-backup.service` | root 644 | one-shot backup job (triggered by the timer, not enabled itself) |
| `/etc/systemd/system/ziga-backup.timer` | root 644 | nightly backup schedule (02:30 UTC, `Persistent=true`) |
| `/var/log/ziga-backup.log` | deploy 640 | backup log, mirroring hookdrop's `/opt/hookdrop/backup.log` |
| `/etc/logrotate.d/ziga-backup` | root 644 | rotation for the above (hookdrop has no equivalent) |
| `/etc/ssl/cloudflare/zigadata.pem` | root 644 | Cloudflare origin certificate |
| `/etc/ssl/cloudflare/zigadata.key` | root 600 | Cloudflare origin private key |
| `/etc/sudoers.d/ziga-deploy` | root 440 | scoped sudo for the CI deploy user |
| `/home/<DEPLOY_USER>/.ssh/authorized_keys` | deploy 600 | CI deploy public key |

Depended on but **not** placed by this setup: `/home/deploy/.config/rclone/rclone.conf`
(deploy 600) — hookdrop's pre-existing rclone config. §e adds a second remote,
`[r2ziga]`, to it and leaves hookdrop's `[r2]` untouched. If the file moves, both
apps' backups fail.

Owned by hookdrop, listed only so a drift audit does not mistake them for ziga's:
`/opt/hookdrop/backup.sh`, `/etc/systemd/system/hookdrop-backup.{service,timer}`,
`/opt/hookdrop/backup.log`.

\* the app creates `ziga.db` on first boot with the process umask; it is written
only by the ziga user inside `/opt/ziga` (the sole `ReadWritePaths` in the unit).
§e widens it to 640 and adds `deploy` to the `ziga` group so the backup can read it.

## k. Email ingestion (optional)

Gives each user a private capture address on a subdomain. Skip this whole
section and the feature is simply not offered: with `INBOUND_EMAIL_DOMAIN`
unset, the ingestion endpoint is never mounted and the UI never shows it.

### k.1 The gotcha: there is no catch-all on a subdomain

Cloudflare supports a catch-all rule **only at a zone apex**. A subdomain gets
Email Routing enabled separately and supports explicit per-address rules only,
capped at **200 rules per domain**.

That is why capture addresses live on a subdomain at all: enabling Email
Routing rewrites MX records for whatever domain it is enabled on, and the apex
(`zigadata.com`) already receives real mail. Enabling it on `in.zigadata.com`
adds MX records **to the subdomain only** — the apex's existing `support@` rule
is untouched.

The consequence is that Ziga creates one routing rule per address through the
Cloudflare API, and rules are a capped resource. `INGEST_MAX_ADDRESSES`
(default 180) refuses provisioning before Cloudflare does, so an operator gets
a clear message and a log line instead of an opaque rejection at the moment
capacity runs out. Cloudflare raises the limit on request — raise both together.

### k.2 Enable Email Routing on the subdomain

In the Cloudflare dashboard, on the `zigadata.com` zone:

1. **Email → Email Routing → Settings**, find the subdomain list, and add
   `in.zigadata.com`. Cloudflare adds the MX records for that subdomain.
2. Wait for the records to propagate. Confirm from the box:

```sh
dig +short MX in.zigadata.com
# expect route1.mx.cloudflare.net. et al
dig +short MX zigadata.com
# expect UNCHANGED — whatever the apex used before
```

That second check is the one that matters. If the apex MX changed, stop and
revert: the marketing domain's mail is now going somewhere else.

3. Verify a **destination address** for the Worker's fallback (Email →
   Destination addresses). `forward()` throws on an unverified address, which
   would turn the safety net into a second failure.

   Reusing whatever `support@zigadata.com` already forwards to is the easy
   choice: it is verified by definition and it is a mailbox someone watches,
   which is the entire point of a fallback.

   It is set as a **secret**, not a var (see k.4) — it is a real person's
   mailbox and this repository is public, so a var in `wrangler.jsonc` would
   put it in git history permanently.

### k.3 Two Cloudflare API tokens — not one

They are easy to conflate and the failure is confusing, so create both
deliberately. **My Profile → API Tokens → Create Token → Custom token**.

**Token A — the app's, for routing rules.** Lives in `/opt/ziga/ziga.env` as
`CLOUDFLARE_API_TOKEN`. Ziga uses it to create one routing rule per capture
address.

- Permissions: `Zone` → `Email Routing Rules` → `Edit`
- Zone Resources: Include → Specific zone → `zigadata.com`

**Token B — CI's, for publishing the Worker.** Lives as the GitHub repository
secret `CLOUDFLARE_WORKERS_TOKEN`, and is what you export locally when running
`wrangler deploy` by hand.

- Permissions: `Account` → `Workers Scripts` → `Edit`
- Account Resources: Include → your account

Neither token can do the other's job. Token A cannot publish a Worker; Token B
cannot create a routing rule. Giving CI the routing token produces an
authentication error at `wrangler deploy` that reads like a bad token rather
than a wrong scope, which is the trap this section exists to prevent.

Also add `CLOUDFLARE_ACCOUNT_ID` as a repository secret (Cloudflare dashboard →
Workers & Pages → account id in the right sidebar). Wrangler can usually infer
it from a single-account token, but setting it removes the ambiguity.

Until `CLOUDFLARE_WORKERS_TOKEN` is set, the deploy job in
`.github/workflows/worker.yml` **skips rather than fails**, so merging this
without any of it configured does not turn `main` red.

### k.4 Deploy the Worker

First set `FALLBACK_ADDRESS` in `worker/email-ingest/wrangler.jsonc` to the
verified destination address from §k.2 and commit it — it ships empty, which
degrades to a loud log instead of preserving mail. Check `ZIGA_INGEST_URL`
matches the deployed app while you are in there.

```sh
cd worker/email-ingest
export CLOUDFLARE_API_TOKEN=<Token B from k.3>
npm ci
npm test                      # includes the cross-language corpus + HMAC vector
npx wrangler deploy
npx wrangler secret put ZIGA_INGEST_SECRET
```

Generate the shared secret once and use the same value in both places:

```sh
head -c 32 /dev/urandom | base64
```

### k.5 App configuration

Add to `/opt/ziga/ziga.env` (mode 600), then `sudo systemctl restart ziga`:

```sh
INBOUND_EMAIL_DOMAIN=in.zigadata.com
INGEST_SHARED_SECRET=<the same value given to wrangler secret>
CLOUDFLARE_API_TOKEN=<the scoped token from k.3>
CLOUDFLARE_ZONE_ID=<zone id from the Cloudflare dashboard overview>
# Optional, defaults shown:
# INGEST_WORKER_NAME=ziga-email-ingest
# INGEST_DAILY_CAP=50
# INGEST_BURST=10
# INGEST_MAX_ADDRESSES=180
```

These are **all-or-nothing**: partial configuration refuses to boot. A domain
with no shared secret would mount an ingestion endpoint with nothing to
authenticate against, and a secret with no Cloudflare credentials would hand
users an address no mail can ever reach. A secret shorter than 32 characters is
also a boot error, not a warning.

Confirm it came up:

```sh
sudo journalctl -u ziga -n 30 --no-pager | grep 'email ingestion enabled'
```

### k.6 Verify end to end

1. In the app, open the account menu → **Email capture** → *Turn on email
   capture*. Confirm the matching rule appears in Cloudflare (Email → Email
   Routing → Routing rules).
2. Send a plain lead email to the address. It should appear in the review queue
   within seconds, badged **Email**, with the sender and subject shown.
3. Set up a real Gmail auto-forward to it. The confirmation code should appear
   on the setup page within a few seconds — that is the step users cannot
   complete without us. Finish it, then forward a labelled lead and confirm it
   is attributed to the **original sender**, not to your own Gmail address.
4. Send a newsletter and an out-of-office. Both should land in **Filtered** with
   the right reason. Rescue one and confirm it enters the review queue.
5. Send the same message twice — exactly one lead.
6. `sudo systemctl stop ziga`, send a lead, confirm it lands in the fallback
   mailbox with `X-Ziga-*` headers, then `sudo systemctl start ziga`.

### k.7 Triage: "leads stopped arriving"

In this order — the first is by far the most likely:

```sh
# 1. Did the shared secret drift? This is the classic failure and its ONLY
#    symptom is 401s; nothing in the product looks broken.
sudo journalctl -u ziga --since '1 hour ago' | grep 'rejected unauthenticated'

# 2. What did the Worker see?
cd worker/email-ingest && npx wrangler tail

# 3. Did the user hit their daily cap? (Their mail is in quarantine, not lost.)
sudo journalctl -u ziga --since today | grep 'per-user cap reached'

# 4. Did addresses and routing rules drift apart?
sudo journalctl -u ziga -n 200 --no-pager | grep 'inbound:'
```

If the secret drifted, re-run `wrangler secret put ZIGA_INGEST_SECRET` with the
value from `ziga.env` — no lead is lost in the meantime, they are in the
fallback mailbox.

### k.8 Note on the capture addresses

They are stored in plaintext in `inbound_addresses.local_part`, because the UI
has to display them. An address is a capability: anyone who knows one can spend
that tenant's extraction budget. That is bounded by 80 bits of entropy, a
per-address rate limit applied before the database lookup, and the per-user
daily cap — but it is worth knowing that a database read yields live capture
addresses.
