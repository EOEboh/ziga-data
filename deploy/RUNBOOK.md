# ziga-data — Production Deployment Runbook

This runbook takes a fresh Ubuntu 24 box (the existing Hetzner CPX22 that already
runs hookdrop) to a running **staging** deployment of ziga-data. Follow it
**top to bottom** — every step only depends on things created in earlier steps.

**Scope of this pass:** staging only. DNS for `zigadata.com` is **not** flipped;
the app is reached via an SSH tunnel (§f). Going live is §h; what is deliberately
left for later is listed in §i.

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
> *origin* cert: it is only trusted by Cloudflare's edge, so it is meaningful
> only after DNS is flipped (§h). It does no harm to install it now.

---

## d. Nginx server block

```bash
sudo cp deploy/nginx-ziga.conf /etc/nginx/sites-available/ziga.conf
sudo ln -s /etc/nginx/sites-available/ziga.conf /etc/nginx/sites-enabled/ziga.conf
sudo nginx -t
sudo systemctl reload nginx
```

> **Staging note:** until DNS is flipped, this block is inert — nothing resolves
> `app.zigadata.com` to this box yet. It is installed now so the config is
> versioned and ready. Daily staging access is via the SSH tunnel in §f, which
> bypasses Nginx entirely.

---

## e. Nightly backup + **mandatory restore test**

A `sqlite3 .backup` snapshot is gzipped and uploaded to **R2** via rclone nightly,
driven by a **systemd timer** — the same mechanism hookdrop uses on this box.

> **Separate buckets:** hookdrop backs up to `r2:hookdrop-backups/` and ziga to
> `r2:ziga-backups/`. They share the rclone config and nothing else — neither
> app's retention prune can touch the other's objects.

> **Which "deploy"?** The backup runs as the box's existing `deploy` user (home
> `/home/deploy`), because that account owns the rclone config holding the R2
> credentials. That is **not** `<DEPLOY_USER>`, the restricted CI account in §g.

**1. Give `deploy` read access to the database.** The app runs as `ziga` and
`/opt/ziga/ziga.db` is mode 600, so the job cannot read it out of the box. Add
`deploy` to the `ziga` group and open group-read:

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

**2. Confirm rclone and the `r2:` remote.** hookdrop already set both up; ziga
reuses them read-only and adds nothing:

```bash
rclone version                                   # confirm rclone is installed
sudo -u deploy rclone --config /home/deploy/.config/rclone/rclone.conf \
    listremotes                                  # expect: r2:
```

The credentials stay in that file. This repo references the config **path** only
and never its contents — do not copy it into `/opt/ziga` or into git.

**3. Install the script:**

```bash
sudo install -o deploy -g deploy -m 750 deploy/backup-ziga.sh /opt/ziga/backup-ziga.sh
```

**4. Install and enable the timer.** No editing required — the bucket, config
path, retention window, and database path are defaulted in the script's header
block; override any of them with `sudo systemctl edit ziga-backup.service` if
they ever change.

```bash
sudo install -o root -g root -m 644 deploy/ziga-backup.service /etc/systemd/system/ziga-backup.service
sudo install -o root -g root -m 644 deploy/ziga-backup.timer   /etc/systemd/system/ziga-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now ziga-backup.timer
systemctl list-timers ziga-backup.timer    # expect: a NEXT elapse ~02:30 UTC
```

Enable the **timer**, not the service — enabling the service directly would run
a backup at every boot instead of on schedule.

**5. Run it once manually and confirm the object landed.** Dry run first (it
still snapshots and gzips locally, so it proves everything short of the upload):

```bash
sudo -u deploy env BACKUP_DRY_RUN=1 /opt/ziga/backup-ziga.sh
echo "rc=$?"                               # expect: rc=0

sudo systemctl start ziga-backup.service   # the real thing, through systemd
sudo -u deploy rclone --config /home/deploy/.config/rclone/rclone.conf \
    ls r2:ziga-backups/                    # expect a ziga-YYYYMMDD-HHMMSS.db.gz
```

**6. RESTORE TEST — a backup is NOT done until a restore has been done.**
Download the object you just uploaded, decompress it, open it with sqlite3, and
count rows. If this fails, the backup is worthless — fix it before moving on.

```bash
cd /tmp
sudo -u deploy rclone --config /home/deploy/.config/rclone/rclone.conf \
    copy r2:ziga-backups/ziga-<STAMP>.db.gz /tmp/
gunzip -k /tmp/ziga-<STAMP>.db.gz          # -> /tmp/ziga-<STAMP>.db
sqlite3 /tmp/ziga-<STAMP>.db '.tables'
sqlite3 /tmp/ziga-<STAMP>.db 'SELECT count(*) FROM submissions;'   # any real table
rm -f /tmp/ziga-<STAMP>.db /tmp/ziga-<STAMP>.db.gz
```

A clean `.tables` listing and a plausible row count means the pipeline is sound.
Re-run this test after any change to the script, the bucket, or the rclone
config — it is the only thing that proves the backups are restorable.

**7. Check that it ran.** The timer logs to journald under `ziga-backup`:

```bash
systemctl list-timers ziga-backup.timer          # next + last elapse
systemctl status ziga-backup.service             # last run's result
journalctl -u ziga-backup.service --since '2 days ago'
```

A healthy run ends with `backup complete: ziga-<STAMP>.db.gz` and
`Result: success`. A failure shows `Result: exit-code` — the script preserves
the real exit status through its cleanup trap specifically so that a failed
upload or an unreadable config surfaces here instead of looking like a success.

> **Logs:** the app logs JSON to stdout, captured by journald — there is **no**
> log file and no logrotate config. journald rotates on its own; cap disk use if
> desired via `SystemMaxUse=` in `/etc/systemd/journald.conf`, or vacuum with
> `sudo journalctl --vacuum-time=30d`.

---

## f. Daily staging access (SSH tunnel)

Until DNS is flipped, reach the app by forwarding its local port over SSH:

```bash
ssh -L 8080:localhost:8090 <DEPLOY_USER>@<HOST>
# leave that open, then browse:  http://localhost:8080
```

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

- **DNS flip** — `app.zigadata.com` is not yet pointed at this box. Until it is,
  the app is reachable only through the SSH tunnel (§f). The switch-over is §h.
- **SMTP** — no provider configured; verification links are read from the
  journal (§a.1).
- **Nginx / TLS** — the server block in `deploy/nginx-ziga.conf` (upstream
  `127.0.0.1:8090`) and the Cloudflare origin cert are for the DNS flip (§h), not
  for staging.
- **Marketing site** — `zigadata.com` apex / marketing pages are a separate
  effort, unrelated to this app deployment.

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
| `/opt/ziga/ziga.db` | ziga 640* | SQLite database — the only persistent state; group-readable so the backup can snapshot it (§e) |
| `/opt/ziga/ziga.db-journal` | ziga | transient rollback journal (present only mid-write) |
| `/opt/ziga/backup-ziga.sh` | deploy 750 | nightly backup script, run by the timer as `deploy` |
| `/etc/systemd/system/ziga.service` | root 644 | systemd unit |
| `/etc/nginx/sites-available/ziga.conf` | root 644 | Nginx server block (+ symlink in sites-enabled) |
| `/etc/systemd/system/ziga-backup.service` | root 644 | one-shot backup job (triggered by the timer, not enabled itself) |
| `/etc/systemd/system/ziga-backup.timer` | root 644 | nightly backup schedule (02:30 UTC, `Persistent=true`) |
| `/etc/ssl/cloudflare/zigadata.pem` | root 644 | Cloudflare origin certificate |
| `/etc/ssl/cloudflare/zigadata.key` | root 600 | Cloudflare origin private key |
| `/etc/sudoers.d/ziga-deploy` | root 440 | scoped sudo for the CI deploy user |
| `/home/<DEPLOY_USER>/.ssh/authorized_keys` | deploy 600 | CI deploy public key |

Depended on but **not** placed by this setup: `/home/deploy/.config/rclone/rclone.conf`
(deploy 600) — hookdrop's pre-existing rclone/R2 credentials, which §e reuses. If it
moves, the nightly backup fails.

\* the app creates `ziga.db` on first boot with the process umask; it is written
only by the ziga user inside `/opt/ziga` (the sole `ReadWritePaths` in the unit).
§e widens it to 640 and adds `deploy` to the `ziga` group so the backup can read it.
